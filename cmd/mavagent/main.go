package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mavsphere/mavsphere-agent-go/internal/webui"
	"github.com/mavsphere/mavsphere-agent-go/pkg/auth"
	cfg "github.com/mavsphere/mavsphere-agent-go/pkg/config"
	"github.com/mavsphere/mavsphere-agent-go/pkg/device"
	"github.com/mavsphere/mavsphere-agent-go/pkg/iceutil"
	"github.com/mavsphere/mavsphere-agent-go/pkg/mavlink"
	"github.com/mavsphere/mavsphere-agent-go/pkg/stomp"
	"github.com/mavsphere/mavsphere-agent-go/pkg/stream"
)

// agentState holds runtime state that feeds back into the web UI health response.
type agentState struct {
	degraded bool
	reasons  []string
}

func (s *agentState) setDegraded(reasons ...string) {
	s.degraded = true
	s.reasons = reasons
}

func main() {
	enableDiagnostics()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfgPath := "config.json"
	if _, err := cfg.LoadConfig(cfgPath); err != nil {
		log.Printf("❌ Failed to load config: %v", err)
		return
	}
	conf := cfg.GetConfig()

	log.Printf("[CFG] MavID=%s  AllowControl=%v  AgentGcsID=%d", conf.MavID, conf.AllowControl, conf.AgentGcsID)
	log.Printf("[CFG] BackendWS=%s  BackendHTTP=%s", conf.BackendWsURL, conf.BackendURL)
	log.Printf("[CFG] MAVLink=%s", conf.MavlinkConnection)
	log.Printf("[CFG] JanusURL=%s", conf.JanusURL)

	// ── Web UI — started before pairing so the pairing code is visible ────────
	ui := webui.Start("0.0.0.0:8090", cfgPath)

	// ── Pairing — must run before token acquisition ──────────────────────────
	// The web UI is already running so it can display the pairing code.
	if conf.AgentToken == "" {
		log.Printf("[pair] No agent token in config — entering pairing mode")
		if err := auth.RunPairingLoop(ctx, cfgPath); err != nil {
			log.Printf("[pair] Pairing failed or was cancelled: %v", err)
			return
		}
		// Reload after pairing wrote token + mavId.
		if _, err := cfg.LoadConfig(cfgPath); err != nil {
			log.Printf("reload config after pairing: %v", err)
			return
		}
		conf = cfg.GetConfig()
	}

	mavId := conf.MavID

	// Agent-level state — fed into /api/health so the web UI can show errors.
	state := &agentState{}

	// ——— Handles visible to restart hooks / supervisor ———
	var (
		mav           *mavlink.MavlinkConnection
		currentRoomID int64
		streamMgr     *stream.Manager
	)

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		log.Println("🛑 Received shutdown signal, stopping stream then exiting...")

		if streamMgr != nil {
			streamMgr.Stop()
			time.Sleep(500 * time.Millisecond) // small grace
		}
		cancel()
	}()

	// Web UI
	ui.SetPreRestart(func() {
		log.Println("🔁 Pre-restart: stopping stream + closing MAVLink (with timeout)")

		done := make(chan struct{})

		go func() {
			defer close(done)

			if streamMgr != nil && streamMgr.IsRunning() {
				log.Println("🎥 Stopping GStreamer stream before restart…")
				stopDone := make(chan struct{})
				go func() {
					defer close(stopDone)
					streamMgr.Stop()
				}()

				select {
				case <-stopDone:
					log.Println("✅ Stream stopped")
				case <-time.After(2 * time.Second):
					log.Println("⚠️ Stream stop timed out; proceeding with restart anyway")
				}
			}

			if mav != nil {
				log.Println("🛰️ Closing MAVLink connection before restart…")
				mav.Close()
			}
		}()

		// Cancel ctx to wind down background goroutines, but do not block restart on it.
		cancel()

		// Hard deadline: always exit even if cleanup hangs.
		select {
		case <-done:
			log.Println("✅ Pre-restart cleanup finished")
		case <-time.After(3 * time.Second):
			log.Println("⚠️ Pre-restart cleanup timed out; forcing exit")
		}

		log.Println("🔁 Exiting process to trigger container restart…")
		os.Exit(0)
	})

	// Auth
	token := getTokenWithRetry(ctx, state, cfgPath)
	if token == "" {
		log.Printf("❌ Could not obtain auth token (canceled?)")
		return
	}
	log.Println("🔐 Auth acquired")

	// Make ICE available to gstreamer_native.go before any pipeline starts
	ice := refreshICE(token)

	videoDev, _ := device.DetectVideoDevice()
	audioDev, err := device.DetectAudioDevice()

	if videoDev != "" {
		log.Printf("🎥 Video device (auto-detected): %s", videoDev)
	} else {
		log.Printf("⚠️ No video device detected")
	}

	if strings.TrimSpace(conf.VideoDevice) != "" {
		v := strings.ToLower(strings.TrimSpace(conf.VideoDevice))
		switch v {
		case "none", "off", "disabled", "false":
			videoDev = ""
			log.Printf("🎥 Video disabled via config (VideoDevice=%q)", conf.VideoDevice)
		default:
			videoDev = strings.TrimSpace(conf.VideoDevice)
			log.Printf("🎥 Video device overridden by config: %s", videoDev)
		}
	}

	if err != nil {
		log.Printf("ℹ️ Audio auto-detect failed: %v", err)
	}

	if strings.TrimSpace(conf.AudioDevice) != "" {
		v := strings.ToLower(strings.TrimSpace(conf.AudioDevice))
		switch v {
		case "none", "off", "disabled", "false":
			audioDev = ""
			log.Printf("🎙️ Audio disabled via config (AudioDevice=%q)", conf.AudioDevice)
		default:
			audioDev = strings.TrimSpace(conf.AudioDevice)
			log.Printf("🎙️ Using audio device from config: %s", audioDev)
		}
	} else if audioDev != "" {
		log.Printf("🎙️ Audio device auto-detected: %s", audioDev)
	} else {
		audioDev = ""
		log.Printf("🎙️ No audio device detected; running video-only")
	}

	// MAVLink (retry)
	ml, err := initMavlinkWithRetry(ctx, conf.MavlinkConnection, byte(conf.AgentGcsID))
	if err != nil {
		log.Printf("❌ MAVLink connection failed: %v", err)
		return
	}
	mav = ml

	ui.AttachCommander(mav)

	// Backend STOMP (self-reconnecting)
	session := stomp.NewSession(conf.BackendWsURL, token, conf, mav, videoDev, audioDev)
	// Wire degraded-state callback so the STOMP reconnect loop can push
	// rate-limit / bad-credentials errors into the /api/health response.
	session.SetDegradedFn = func(degraded bool, reason string) {
		if degraded {
			state.setDegraded(reason)
		} else {
			state.degraded = false
			state.reasons = nil
		}
	}

	// Attach health provider — wraps mav.Health() with agent-level degraded/reasons
	// state and live STOMP connectivity so the web UI's "Paired" indicator reflects
	// real connection status rather than just the presence of a stored agent token.
	ui.AttachMavlink(func() map[string]any {
		h := mav.Health()
		h["connected"] = session.IsConnected()
		h["degraded"] = state.degraded
		h["reasons"] = state.reasons
		return h
	}, mav.ProbeParam)

	go session.Start()
	waitWithTimeout(ctx, session.ReadyChan, 30*time.Second, "STOMP ready")

	streamMgr = stream.NewManager(conf, videoDev, audioDev, nil)
	log.Printf("✅ Stream manager initialised (JanusURL=%s)", conf.JanusURL)

	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if streamMgr != nil && streamMgr.IsRunning() && currentRoomID > 0 {
					session.SendPublisherHeartbeat(mavId, currentRoomID, true)
				}
			}
		}
	}()

	// Apply initial ICE config
	streamMgr.SetICE(ctx, ice)

	// Start TTL-based ICE refresh loop
	startICERefresher(ctx, token, ice, streamMgr)

	// -------- helpers to start/stop publisher --------

	stopPublisher := func() {
		if streamMgr != nil {
			streamMgr.Stop()
		}
		if session != nil && currentRoomID > 0 {
			_ = session.SendPublisherHeartbeat(mavId, currentRoomID, false)
		}
		currentRoomID = 0
	}

	startPublisherForRoom := func(roomID int64) error {
		if roomID <= 0 {
			return fmt.Errorf("invalid room ID %d", roomID)
		}
		if streamMgr == nil {
			return fmt.Errorf("stream manager not initialised")
		}

		log.Printf("🔌 Starting janusvrwebrtcsink pipeline for roomId=%d", roomID)

		if err := streamMgr.Start(ctx, roomID); err != nil {
			return fmt.Errorf("start stream: %w", err)
		}

		currentRoomID = roomID
		return nil
	}

	// -------- supervise START_VIDEO commands from backend --------
	defer stopPublisher()
	log.Println("🎛️  Video supervisor running; waiting for room IDs…")

	for {
		select {
		case <-ctx.Done():
			return

		case roomID := <-session.RoomIDChan:
			running := streamMgr != nil && streamMgr.IsRunning()

			log.Printf("📨 START_VIDEO for roomId=%d (current=%d running=%v)",
				roomID, currentRoomID, running)

			// roomId <= 0 is our "STOP_VIDEO" sentinel
			if roomID <= 0 {
				log.Printf("🛑 Stop requested via roomId=%d; stopping publisher", roomID)
				stopPublisher()
				continue
			}

			// Ignore duplicates unconditionally
			if running && currentRoomID == roomID {
				log.Printf("ℹ️ START_VIDEO ignored (already publishing roomId=%d)", roomID)
				continue
			}

			// Different room, or not running: start
			if err := startPublisherForRoom(roomID); err != nil {
				log.Printf("❌ Failed to start stream for room %d: %v", roomID, err)
				continue
			}

			currentRoomID = roomID
		}
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func enableDiagnostics() {
	if os.Getenv("AGENT_DIAG") == "" {
		return
	}

	setIfEmpty := func(k, v string) {
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}

	setIfEmpty("G_DEBUG", "gc-friendly,fatal-warnings")
	setIfEmpty("G_SLICE", "always-malloc")
	setIfEmpty("GST_DEBUG", "webrtc*:5,rtp*:5,dtls*:5,srtp*:5,libnice*:5")
	setIfEmpty("GST_DEBUG_NO_COLOR", "1")
	setIfEmpty("GST_DEBUG_FILE", "/tmp/gst_agent.log")

	if os.Getenv("GLIBC_TUNABLES") == "" {
		log.Printf("[diag] Tip: export GLIBC_TUNABLES=glibc.malloc.check=3 before starting the agent.")
	}
	if os.Getenv("MALLOC_PERTURB_") == "" {
		log.Printf("[diag] Tip: export MALLOC_PERTURB_=153 before starting the agent.")
	}

	log.Printf("[diag] GST_DEBUG=%q GST_DEBUG_FILE=%q", os.Getenv("GST_DEBUG"), os.Getenv("GST_DEBUG_FILE"))
}

func waitWithTimeout(ctx context.Context, ch <-chan struct{}, d time.Duration, name string) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
		log.Printf("✅ %s", name)
	case <-timer.C:
		log.Printf("⏳ %s still pending after %v (continuing anyway)", name, d)
	case <-ctx.Done():
	}
}

// getTokenWithRetry attempts login with appropriate backoff strategy per error type:
//   - ErrTokenRevoked: backend explicitly rejected the token. Clear it and restart
//     into pairing mode rather than retrying.
//   - ErrRateLimited (429): back off for the duration specified by the backend, then retry.
//   - Transient errors (network, 5xx): exponential backoff 2s → 60s.
func getTokenWithRetry(ctx context.Context, state *agentState, cfgPath string) string {
	backoff := 2 * time.Second
	const maxBackoff = 60 * time.Second
	rand.Seed(time.Now().UnixNano()) //nolint:staticcheck

	for {
		token, err := auth.Login()
		if err == nil && token != "" {
			state.degraded = false
			state.reasons = nil
			return token
		}

		if errors.Is(err, auth.ErrTokenRevoked) {
			// Backend explicitly rejected the token as invalid or revoked.
			// Clear agentToken so the agent enters pairing mode on restart.
			log.Printf("[auth] agent token revoked — clearing token and restarting into pairing mode")
			conf := cfg.GetConfig()
			conf.AgentToken = ""
			if saveErr := cfg.SaveConfig(cfgPath, conf); saveErr != nil {
				log.Printf("[auth] failed to clear token from config: %v", saveErr)
			}
			os.Exit(0)
		}

		if errors.Is(err, auth.ErrRateLimited) {
			wait := 15 * time.Minute // conservative default if no structured response
			blockedBy := "unknown"
			if rle, ok := auth.AsRateLimit(err); ok {
				wait = rle.RetryAfter
				blockedBy = rle.BlockedBy
			}
			retryAt := time.Now().Add(wait)
			msg := fmt.Sprintf(
				"login rate limited (blocked_by=%s) — too many failed attempts; retry at %s (in ~%s)",
				blockedBy,
				retryAt.Format("15:04:05"),
				wait.Round(time.Second),
			)
			state.setDegraded(msg)
			log.Printf("⏳ %s", msg)
			select {
			case <-time.After(wait):
				state.degraded = false
				state.reasons = nil
			case <-ctx.Done():
				return ""
			}
			continue
		}

		if err != nil {
			log.Printf("❌ Login failed: %v", err)
		} else {
			log.Printf("❌ Login failed: empty token")
		}

		wait := backoff + time.Duration(rand.Int63n(int64(backoff/2)))
		if wait > maxBackoff {
			wait = maxBackoff
		}
		log.Printf("⏳ Retrying login in %v...", wait)

		select {
		case <-time.After(wait):
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		case <-ctx.Done():
			log.Println("🛑 Login retry canceled")
			return ""
		}
	}
}

func initMavlinkWithRetry(ctx context.Context, endpoint string, outSysID byte) (*mavlink.MavlinkConnection, error) {
	backoff := 2 * time.Second
	const maxBackoff = 15 * time.Second

	for {
		conn, err := mavlink.InitMavlink(endpoint, outSysID)
		if err == nil {
			return conn, nil
		}

		log.Printf("❌ MAVLink init failed: %v", err)
		log.Printf("⏳ Retrying in %v...", backoff)

		select {
		case <-time.After(backoff):
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		case <-ctx.Done():
			return nil, fmt.Errorf("context canceled while initializing MAVLink: %w", ctx.Err())
		}
	}
}

// refreshICE fetches ICE from backend/config and returns a stream.ICEConfig.
func refreshICE(token string) stream.ICEConfig {
	stun, turn, user, pass, forceRelay := cfg.BuildGStreamerICEFromBackendOrConfig(token, nil)
	ttl := cfg.GetLastIceTTLSeconds()

	ice := stream.ICEConfig{
		StunURL:    stun,
		TurnURL:    turn,
		TurnURLs:   cfg.GetLastTurnURLs(),
		Username:   user,
		Password:   pass,
		ForceRelay: forceRelay,
		TTLSeconds: ttl,
	}

	log.Printf("[ICE] Backend provided STUN=%q TURN=%s turnURLs=%v user=%q forceRelay=%v ttl=%ds (applying to janusvrwebrtcsink locally)",
		ice.StunURL,
		iceutil.PrettyTurnVariant(ice.TurnURL),
		ice.TurnURLs,
		ice.Username,
		ice.ForceRelay,
		ice.TTLSeconds,
	)

	return ice
}

func startICERefresher(
	ctx context.Context,
	token string,
	initial stream.ICEConfig,
	streamMgr *stream.Manager,
) {
	go func() {
		cur := initial

		for {
			ttl := cur.TTLSeconds
			if ttl <= 0 {
				ttl = 900
			}

			refreshIn := time.Duration(ttl) * time.Second * 8 / 10
			minBeforeExpiry := time.Duration(ttl-60) * time.Second
			if minBeforeExpiry < 60*time.Second {
				minBeforeExpiry = 60 * time.Second
			}
			if refreshIn > minBeforeExpiry {
				refreshIn = minBeforeExpiry
			}
			if refreshIn < 60*time.Second {
				refreshIn = 60 * time.Second
			}

			log.Printf("[ICE] Next ICE refresh scheduled in %v (ttl=%ds)", refreshIn, ttl)

			select {
			case <-time.After(refreshIn):
			case <-ctx.Done():
				return
			}

			next := refreshICE(token)

			changed :=
				next.StunURL != cur.StunURL ||
					next.TurnURL != cur.TurnURL ||
					next.Username != cur.Username ||
					next.Password != cur.Password ||
					next.ForceRelay != cur.ForceRelay

			if changed {
				log.Printf("[ICE] ICE credentials changed; applying and restarting stream if running")
				streamMgr.SetICE(ctx, next)
			} else {
				log.Printf("[ICE] ICE refreshed; no change detected")
			}

			cur = next
		}
	}()
}
