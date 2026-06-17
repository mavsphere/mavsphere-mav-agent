package stomp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-stomp/stomp"
	"github.com/mavsphere/mavsphere-agent-go/pkg/auth"
	"github.com/mavsphere/mavsphere-agent-go/pkg/config"
	"github.com/mavsphere/mavsphere-agent-go/pkg/device"
	"github.com/mavsphere/mavsphere-agent-go/pkg/mavlink"
	"github.com/mavsphere/mavsphere-agent-go/pkg/stomp/wsconn"
)

// ControlPolicy is a server-authoritative gate sent by the backend.
// The agent must treat this as a hard deny-list: local config may further
// restrict, but must never expand beyond what the backend permits.
type ControlPolicy struct {
	AllowControl         bool `json:"allowControl"`
	AllowGoto            bool `json:"allowGoto"`
	AllowRcOverride      bool `json:"allowRcOverride"`
	AllowAircraftControl bool `json:"allowAircraftControl"`
	ShareGpsCoords       bool `json:"shareGpsCoords"`
}

type Session struct {
	cfg         *config.AgentConfig
	mavlink     *mavlink.MavlinkConnection
	videoDevice string
	audioDevice string
	wsURL       string
	token       string

	// STOMP connection is guarded by a RW mutex to prevent races
	connMu sync.RWMutex
	conn   *stomp.Conn

	sendMu sync.Mutex // serialize all conn.Send calls

	mavID string

	// Channels exposed to the rest of the agent
	RoomIDChan    chan int64
	SdpAnswerChan chan string
	ReadyChan     chan struct{}
	SendSdpOffer  func(string)

	// Internals
	subCmd *stomp.Subscription // /topic/agent/{mavId}/commands
	subSdp *stomp.Subscription // /user/queue/janus/sdp

	// Control subscriptions (backend → agent)
	subAttitude *stomp.Subscription // /user/queue/agent/{mavId}/control/attitudecontrol
	subVelocity *stomp.Subscription // /user/queue/agent/{mavId}/control/velocitycontrol
	subRc       *stomp.Subscription // /user/queue/agent/{mavId}/control/rc
	subGoto     *stomp.Subscription // /user/queue/agent/{mavId}/control/goto
	subGimbal   *stomp.Subscription // /user/queue/agent/{mavId}/control/gimbal

	subPolicy *stomp.Subscription // /user/queue/agent/{mavId}/policy

	// Latency probe (viewer -> agent)
	subLatencyPing *stomp.Subscription // /user/queue/latency/ping

	policyMu   sync.RWMutex
	policySeen bool
	policy     ControlPolicy

	stopCh     chan struct{}
	lastRoomID int64

	// rate-limit noisy control logs
	lastCtrlLog time.Time

	// ---- Control failsafe (dead-man) ----
	ctrlMu       sync.Mutex
	lastCtrlAt   time.Time
	lastCtrlKind string

	// ---- GOTO persistence (map click) ----
	gotoActive   bool
	gotoLat      float64
	gotoLon      float64
	gotoAltM     float64
	gotoLastSent time.Time
	gotoStarted  time.Time
	failsafeOn   bool

	pubHbCount     uint64
	lastPubHbState uint32 // 0=false, 1=true

	// SetDegradedFn is an optional callback that the STOMP reconnect loop
	// calls to push auth-error state into the web UI health endpoint.
	// Pass nil to disable (degraded state will still appear in logs).
	// Signature: setDegraded(isDegraded bool, reason string)
	SetDegradedFn func(degraded bool, reason string)
}

func NewSession(
	wsURL string,
	token string,
	cfg *config.AgentConfig,
	mav *mavlink.MavlinkConnection,
	videoDev string,
	audioDev string,
) *Session {
	return &Session{
		wsURL:         wsURL,
		token:         token,
		cfg:           cfg,
		mavlink:       mav,
		videoDevice:   videoDev,
		audioDevice:   audioDev,
		mavID:         cfg.MavID,
		RoomIDChan:    make(chan int64, 1),
		SdpAnswerChan: make(chan string, 1),
		ReadyChan:     make(chan struct{}, 1),
		// Default privacy: share GPS coordinates unless explicitly disabled by config/policy.
		policy: ControlPolicy{ShareGpsCoords: true},
	}
}

// setDegraded calls SetDegradedFn if set; always a no-op if nil.
func (s *Session) setDegraded(degraded bool, reason string) {
	if s.SetDegradedFn != nil {
		s.SetDegradedFn(degraded, reason)
	}
}

// --- conn helpers (thread-safe) ---

func (s *Session) setConn(c *stomp.Conn) {
	s.connMu.Lock()
	s.conn = c
	s.connMu.Unlock()
}

func (s *Session) getConn() *stomp.Conn {
	s.connMu.RLock()
	c := s.conn
	s.connMu.RUnlock()
	return c
}

// IsConnected reports whether the session currently holds a live STOMP
// connection to the backend. Used by the web UI's /api/health endpoint so
// the "Paired" indicator reflects real connectivity rather than just the
// presence of a stored agent token.
func (s *Session) IsConnected() bool {
	return s.getConn() != nil
}

func (s *Session) setPolicy(p ControlPolicy) {
	s.policyMu.Lock()
	s.policy = p
	s.policySeen = true
	s.policyMu.Unlock()
}

func (s *Session) getPolicy() (ControlPolicy, bool) {
	s.policyMu.RLock()
	p := s.policy
	seen := s.policySeen
	s.policyMu.RUnlock()
	return p, seen
}

type policySnap struct {
	policySeen bool
	policy     ControlPolicy
}

func (s *Session) SendPublisherHeartbeat(mavID string, roomID int64, publishing bool) error {
	payload := map[string]any{
		"mavId":      mavID,
		"roomId":     roomID,
		"publishing": publishing,
		"ts":         time.Now().UnixMilli(),
	}

	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[stomp] ❌ PublisherHeartbeat marshal failed mavId=%s roomId=%d publishing=%v err=%v",
			mavID, roomID, publishing, err)
		return err
	}

	dest := fmt.Sprintf("/app/agent/%s/publisherHeartbeat", mavID)
	if err := s.send(dest, "application/json", b); err != nil {
		log.Printf("[stomp] ❌ PublisherHeartbeat send failed mavId=%s roomId=%d publishing=%v err=%v",
			mavID, roomID, publishing, err)
		return err
	}

	// Lightweight proof logging:
	cnt := atomic.AddUint64(&s.pubHbCount, 1)

	var state uint32
	if publishing {
		state = 1
	}
	prev := atomic.SwapUint32(&s.lastPubHbState, state)
	stateChanged := prev != state

	if stateChanged || (cnt%10 == 0) {
		log.Printf("[stomp] ✅ PublisherHeartbeat sent mavId=%s roomId=%d publishing=%v count=%d",
			mavID, roomID, publishing, cnt)
	}

	return nil
}

func (s *Session) policySnapshot() policySnap {
	p, ok := s.getPolicy()
	return policySnap{policySeen: ok, policy: p}
}

func denyReason(kind string, eligibleVehicle bool, isAircraft bool, eff ControlPolicy) string {
	if !eligibleVehicle {
		return "mavlink-ineligible"
	}
	if !eff.AllowControl {
		if isAircraft && !eff.AllowAircraftControl {
			return "aircraft-control-not-allowed"
		}
		return "allowControl=false"
	}
	switch kind {
	case "RC_OVERRIDE":
		if !eff.AllowRcOverride {
			return "rc-override-not-allowed"
		}
	case "GOTO_GLOBAL":
		if !eff.AllowGoto {
			return "goto-not-allowed"
		}
	}
	return "unknown"
}

func policyEqual(a, b ControlPolicy) bool {
	return a.AllowControl == b.AllowControl &&
		a.AllowGoto == b.AllowGoto &&
		a.AllowRcOverride == b.AllowRcOverride &&
		a.AllowAircraftControl == b.AllowAircraftControl
}

// effectiveGate returns the effective allow flags after applying:
//
//	(1) local config (can only restrict)
//	(2) backend policy (can only restrict; authoritative)
func (s *Session) effectiveGate(isAircraft bool) ControlPolicy {
	// start with local config
	eff := ControlPolicy{
		AllowControl:         s.cfg.AllowControl,
		AllowGoto:            s.cfg.AllowGotoEffective(),
		AllowRcOverride:      s.cfg.AllowRcOverrideEffective(),
		AllowAircraftControl: s.cfg.AllowAircraftControl,
	}
	if p, ok := s.getPolicy(); ok {
		eff.AllowControl = eff.AllowControl && p.AllowControl
		eff.AllowGoto = eff.AllowGoto && p.AllowGoto
		eff.AllowRcOverride = eff.AllowRcOverride && p.AllowRcOverride
		eff.AllowAircraftControl = eff.AllowAircraftControl && p.AllowAircraftControl
	}
	// For aircraft-like vehicles, aircraft control must be explicitly allowed.
	if isAircraft {
		eff.AllowControl = eff.AllowControl && eff.AllowAircraftControl
	}
	return eff
}

// snapshotMavlink reads the three MAVLink fields once, with a hard timeout,
// so the heartbeat loop can never block indefinitely if MAVLink stalls.
func (s *Session) snapshotMavlink(timeout time.Duration) (eligible bool, mode string, armed bool) {
	if s.mavlink == nil {
		return false, "NO_MAVLINK", false
	}

	type snap struct {
		eligible bool
		mode     string
		armed    bool
	}
	ch := make(chan snap, 1)

	go func() {
		eligibleBase, mode, armed := s.mavlink.SnapshotControlState()
		isAircraft := s.mavlink.IsAircraftLike()
		eff := s.effectiveGate(isAircraft)
		eligible := eligibleBase && eff.AllowControl
		ch <- snap{eligible: eligible, mode: mode, armed: armed}
	}()

	select {
	case v := <-ch:
		return v.eligible, v.mode, v.armed
	case <-time.After(timeout):
		return false, "MAVLINK_TIMEOUT", false
	}
}

func (s *Session) send(dest, contentType string, body []byte) error {
	c := s.getConn()
	if c == nil {
		return fmt.Errorf("stomp connection is nil")
	}

	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	return c.Send(dest, contentType, body)
}

// --- queue helpers (last-write-wins) ---

func (s *Session) drainRoomIDChan() {
	for {
		select {
		case <-s.RoomIDChan:
		default:
			return
		}
	}
}

func (s *Session) enqueueLatestRoomID(roomID int64) {
	select {
	case s.RoomIDChan <- roomID:
		log.Printf("📤 roomId %d enqueued to RoomIDChan", roomID)
		return
	default:
	}
	s.drainRoomIDChan()
	select {
	case s.RoomIDChan <- roomID:
		log.Printf("📤 roomId %d enqueued to RoomIDChan (overwrote stale)", roomID)
	default:
		log.Printf("⚠️ RoomIDChan still full after drain; dropping roomId=%d", roomID)
	}
}

// Start maintains a durable STOMP connection with heartbeats, telemetry, control subscriptions, and resubscription.
func (s *Session) Start() {
	// Throttle token refresh attempts so a rapid reconnect storm cannot burn
	// through the backend's per-username login rate-limit window (5 failures / 15 min).
	// We only attempt a re-login at most once every tokenRefreshCooldown period.
	// If we get a 429 we honour the backend's Retry-After and extend the cooldown.
	const tokenRefreshCooldown = 2 * time.Minute
	var lastTokenRefresh time.Time
	var rateLimitedUntil time.Time

	for {
		log.Println("[STOMP] Connecting to backend WebSocket...")
		conn, err := wsconn.DialStompOverWebSocket(s.wsURL, s.token, s.mavID)
		if err != nil {
			var hse *wsconn.HandshakeError
			if errors.As(err, &hse) {
				if hse.Status == http.StatusUnauthorized || hse.Status == http.StatusForbidden {
					now := time.Now()

					// If we are still inside a rate-limit window, don't even try.
					if now.Before(rateLimitedUntil) {
						remaining := rateLimitedUntil.Sub(now).Round(time.Second)
						log.Printf("⏳ STOMP handshake %d — skipping token refresh (rate-limited for another %v)",
							hse.Status, remaining)
						time.Sleep(5 * time.Second)
						continue
					}

					// Throttle: don't attempt re-login more than once per cooldown window.
					if now.Sub(lastTokenRefresh) < tokenRefreshCooldown {
						nextAllowed := lastTokenRefresh.Add(tokenRefreshCooldown)
						log.Printf("⏳ STOMP handshake %d — token refresh throttled (next allowed at %s)",
							hse.Status, nextAllowed.Format("15:04:05"))
						time.Sleep(5 * time.Second)
						continue
					}

					log.Printf("❌ STOMP handshake %d — refreshing token…", hse.Status)
					lastTokenRefresh = now

					tok, terr := auth.Login()
					if terr != nil {
						if errors.Is(terr, auth.ErrTokenRevoked) {
							log.Printf("❌ Auth re-login failed: token revoked — restarting into pairing mode")
							s.setDegraded(true, "agent token revoked — restarting to re-pair")
							// Don't duplicate the clear-token-and-save logic here; just exit
							// and let the next startup's auth.Login() call hit the same
							// ErrTokenRevoked path in main.go, which clears the token and
							// restarts into pairing.
							os.Exit(0)
						}
						if errors.Is(terr, auth.ErrRateLimited) {
							wait := 15 * time.Minute
							blockedBy := "unknown"
							if rle, ok := auth.AsRateLimit(terr); ok {
								wait = rle.RetryAfter
								blockedBy = rle.BlockedBy
							}
							rateLimitedUntil = time.Now().Add(wait)
							log.Printf("⏳ Auth re-login rate limited — pausing token refresh for %v (until %s)",
								wait.Round(time.Second), rateLimitedUntil.Format("15:04:05"))
							msg := fmt.Sprintf(
								"login rate limited (blocked_by=%s) — retry at %s (in ~%s)",
								blockedBy, rateLimitedUntil.Format("15:04:05"), wait.Round(time.Second),
							)
							s.setDegraded(true, msg)
						} else {
							log.Printf("❌ Auth re-login failed: %v", terr)
						}
					} else if tok != "" {
						s.token = tok
						log.Printf("🔐 Acquired new token")
						s.setDegraded(false, "")
					}
				} else if hse.Status != 0 {
					log.Printf("❌ STOMP handshake error (status=%d): %v", hse.Status, hse.Err)
				} else {
					log.Printf("❌ STOMP handshake error: %v", hse.Err)
				}
			} else {
				log.Printf("❌ STOMP connection error: %v", err)
			}
			time.Sleep(5 * time.Second)
			continue
		}

		s.setConn(conn)
		s.stopCh = make(chan struct{})
		// Connection established — clear any previous auth-error degraded state.
		s.setDegraded(false, "")

		// Reset command state on each fresh connection
		s.lastRoomID = 0
		s.drainRoomIDChan()

		// ---- Subscriptions: commands (video) ----
		cmdDest := fmt.Sprintf("/topic/agent/%s/commands", s.mavID)
		subCmd, err := conn.Subscribe(cmdDest, stomp.AckAuto)
		if err != nil {
			log.Printf("❌ Subscription error (%s): %v", cmdDest, err)
			_ = conn.Disconnect()
			time.Sleep(5 * time.Second)
			continue
		}
		s.subCmd = subCmd
		log.Printf("🛰️ Subscribed to %s (waiting for commands)...", cmdDest)

		// ---- Subscriptions: Janus SDP answers ----
		subSdp, err := conn.Subscribe("/user/queue/janus/sdp", stomp.AckAuto)
		if err != nil {
			log.Printf("❌ Failed to subscribe to SDP answer: %v", err)
			_ = conn.Disconnect()
			time.Sleep(5 * time.Second)
			continue
		}
		s.subSdp = subSdp
		log.Println("🛰️ Subscribed to /user/queue/janus/sdp (waiting for ANSWERs)")

		// ---- Subscriptions: CONTROL (backend → agent) ----
		ctrlBase := fmt.Sprintf("/user/queue/agent/%s/control", s.mavID)
		if s.subAttitude, err = conn.Subscribe(ctrlBase+"/attitudecontrol", stomp.AckAuto); err != nil {
			log.Printf("❌ Subscribe failed: %s: %v", ctrlBase+"/attitudecontrol", err)
			_ = conn.Disconnect()
			time.Sleep(5 * time.Second)
			continue
		}
		if s.subVelocity, err = conn.Subscribe(ctrlBase+"/velocitycontrol", stomp.AckAuto); err != nil {
			log.Printf("❌ Subscribe failed: %s: %v", ctrlBase+"/velocitycontrol", err)
			_ = conn.Disconnect()
			time.Sleep(5 * time.Second)
			continue
		}
		if s.subRc, err = conn.Subscribe(ctrlBase+"/rc", stomp.AckAuto); err != nil {
			log.Printf("❌ Subscribe failed: %s: %v", ctrlBase+"/rc", err)
			_ = conn.Disconnect()
			time.Sleep(5 * time.Second)
			continue
		}
		if s.subGoto, err = conn.Subscribe(ctrlBase+"/goto", stomp.AckAuto); err != nil {
			log.Printf("❌ Subscribe failed: %s: %v", ctrlBase+"/goto", err)
			_ = conn.Disconnect()
			time.Sleep(5 * time.Second)
			continue
		}
		if s.subGimbal, err = conn.Subscribe(ctrlBase+"/gimbal", stomp.AckAuto); err != nil {
			log.Printf("❌ Subscribe failed: %s: %v", ctrlBase+"/gimbal", err)
			_ = conn.Disconnect()
			time.Sleep(5 * time.Second)
			continue
		}

		// ---- Subscription: server control policy (backend → agent) ----
		policyDest := fmt.Sprintf("/user/queue/agent/%s/policy", s.mavID)
		if s.subPolicy, err = conn.Subscribe(policyDest, stomp.AckAuto); err != nil {
			log.Printf("❌ Subscribe failed: %s: %v", policyDest, err)
			_ = conn.Disconnect()
			time.Sleep(5 * time.Second)
			continue
		}
		log.Printf("🛰️ Subscribed to control queues under %s", ctrlBase)

		// ---- Subscription: Latency pings (backend → agent) ----
		if s.subLatencyPing, err = conn.Subscribe("/user/queue/latency/ping", stomp.AckAuto); err != nil {
			log.Printf("❌ Subscribe failed: %s: %v", "/user/queue/latency/ping", err)
			_ = conn.Disconnect()
			time.Sleep(5 * time.Second)
			continue
		}
		log.Printf("🛰️ Subscribed to /user/queue/latency/ping (latency probe)")

		// ---- Loops ----
		go s.sendHeartbeatLoop()
		go s.handleCommands()
		go s.handleSdpAnswers()
		go s.handleControlLoop()
		go s.handlePolicyLoop()
		go s.handleLatencyPings()
		go s.controlFailsafeLoop()
		go s.controlStatusLoop()
		go s.telemetryLoop()

		// Signal readiness after subscriptions exist
		select {
		case s.ReadyChan <- struct{}{}:
		default:
		}
		log.Println("✅ STOMP subscriptions confirmed")

		// Park here until the connection dies; handlers will exit on stopCh/close
		err = s.blockUntilConnDies()
		log.Printf("⚠️ STOMP connection ended: %v", err)

		// Cleanup & reconnect
		s.safeCloseStop()
		s.unsubscribeQuiet(s.subCmd)
		s.unsubscribeQuiet(s.subSdp)
		s.unsubscribeQuiet(s.subAttitude)
		s.unsubscribeQuiet(s.subVelocity)
		s.unsubscribeQuiet(s.subRc)
		s.unsubscribeQuiet(s.subGoto)
		s.unsubscribeQuiet(s.subGimbal)
		s.unsubscribeQuiet(s.subPolicy)
		s.unsubscribeQuiet(s.subLatencyPing)

		s.subCmd, s.subSdp = nil, nil
		s.subAttitude, s.subVelocity, s.subRc, s.subGoto, s.subGimbal, s.subPolicy = nil, nil, nil, nil, nil, nil
		s.subLatencyPing = nil

		if c := s.getConn(); c != nil {
			_ = c.Disconnect()
		}
		s.setConn(nil)

		time.Sleep(2 * time.Second)
	}
}

// --- Helpers ---

func (s *Session) unsubscribeQuiet(sub *stomp.Subscription) {
	if sub != nil {
		_ = sub.Unsubscribe()
	}
}

func (s *Session) safeCloseStop() {
	if s == nil || s.stopCh == nil {
		return
	}
	select {
	case <-s.stopCh:
	default:
		// On any reconnect/disconnect, immediately neutralize/stop.
		// This prevents "stuck" last-command behavior when the control path drops.
		s.applyControlFailsafe("stomp disconnect/reconnect")
		close(s.stopCh)
	}
}

// controlFailsafeLoop implements a "dead-man" switch for control.
// If control messages stop arriving for a short period, the agent actively sends
// a neutral/stop command. While stale, it re-issues stop periodically.
func (s *Session) controlFailsafeLoop() {
	// Reasonable defaults; overridden by config.
	checkEvery := 100 * time.Millisecond
	staleAfter := time.Duration(s.cfg.Failsafe.ControlTimeoutMs) * time.Millisecond
	reissueEvery := time.Duration(s.cfg.Failsafe.ReissueStopMs) * time.Millisecond

	if staleAfter <= 0 {
		staleAfter = 450 * time.Millisecond
	}
	if reissueEvery <= 0 {
		reissueEvery = 500 * time.Millisecond
	}

	tick := time.NewTicker(checkEvery)
	defer tick.Stop()

	var lastReissue time.Time

	for {
		select {
		case <-s.stopCh:
			// If we're stopping the session, ensure we stop the vehicle once.
			s.applyControlFailsafe("stopCh closed")
			return
		case <-tick.C:
			if s.mavlink == nil {
				continue
			}

			eligibleVehicle := s.mavlink.IsControlAllowed()
			if !(s.cfg.AllowControl && eligibleVehicle) {
				continue
			}

			s.ctrlMu.Lock()
			lastAt := s.lastCtrlAt
			alreadyOn := s.failsafeOn
			gotoActive := s.gotoActive
			gotoLat := s.gotoLat
			gotoLon := s.gotoLon
			gotoAltM := s.gotoAltM
			gotoLastSent := s.gotoLastSent
			gotoStarted := s.gotoStarted
			s.ctrlMu.Unlock()

			// If a map "GOTO" is active, keep the target alive and do not trigger the dead-man failsafe.
			if gotoActive {
				// Re-issue at ~1Hz (ArduPilot will stop if it does not see a fresh target).
				if time.Since(gotoLastSent) >= 1*time.Second {
					target := map[string]any{"lat": gotoLat, "lon": gotoLon, "alt": gotoAltM}
					s.mavlink.ApplyControl("GOTO_GLOBAL", target)
					now := time.Now()
					s.ctrlMu.Lock()
					s.gotoLastSent = now
					s.lastCtrlAt = now
					s.lastCtrlKind = "GOTO_GLOBAL"
					s.ctrlMu.Unlock()
				}

				// Best-effort arrival detection using current GPS (if available).
				tel := s.mavlink.GetTelemetryUpdate("vehicle")

				if (tel.GPS.Lat != 0 || tel.GPS.Lon != 0) && tel.GPS.Sats > 0 {
					d := haversineMeters(tel.GPS.Lat, tel.GPS.Lon, gotoLat, gotoLon)
					if d >= 0 && d <= 2.0 {
						log.Printf("✅ [CONTROL] GOTO reached (%.1fm) after %s",
							d, time.Since(gotoStarted).Truncate(time.Second))
						s.ctrlMu.Lock()
						s.gotoActive = false
						s.ctrlMu.Unlock()
					}
				}

				// Never engage failsafe while GOTO is active.
				continue
			}
			if lastAt.IsZero() {
				// Don't fail-safe until we've seen at least one control message.
				continue
			}

			age := time.Since(lastAt)
			if age < staleAfter {
				continue
			}

			// stale: send stop immediately, then re-issue periodically
			if !alreadyOn || time.Since(lastReissue) >= reissueEvery {
				s.applyControlFailsafe(fmt.Sprintf("stale control age=%v", age))
				lastReissue = time.Now()

				s.ctrlMu.Lock()
				s.failsafeOn = true
				s.ctrlMu.Unlock()
			}
		}
	}
}

func (s *Session) applyControlFailsafe(reason string) {
	if s.mavlink == nil {
		return
	}

	s.ctrlMu.Lock()
	kind := s.lastCtrlKind
	s.ctrlMu.Unlock()

	// Avoid spamming logs when we're re-issuing stop repeatedly.
	if time.Since(s.lastCtrlLog) >= 500*time.Millisecond {
		log.Printf("🛑 CONTROL FAILSAFE: %s (lastKind=%s)", reason, kind)
		s.lastCtrlLog = time.Now()
	}

	switch kind {
	case "RC_OVERRIDE":
		th := s.cfg.Failsafe.RcOverride.ThrottleStopPwm
		st := s.cfg.Failsafe.RcOverride.SteeringStopPwm
		s.mavlink.FailSafeStopRcOverride(uint16(th), uint16(st))
	case "VELOCITY":
		s.mavlink.FailSafeStopVelocity()
	case "ATTITUDE":
		// For subs/boats: thrust=0 is safest.
		s.mavlink.FailSafeStopAttitude(0)
	case "GOTO_GLOBAL", "POSITION":
		// Position setpoints can "stick" too; safest is to issue a zero-velocity stop.
		s.mavlink.FailSafeStopVelocity()
	default:
		// Conservative fallback
		th := s.cfg.Failsafe.RcOverride.ThrottleStopPwm
		st := s.cfg.Failsafe.RcOverride.SteeringStopPwm
		s.mavlink.FailSafeStopRcOverride(uint16(th), uint16(st))
	}
}

// CommandMsg is a tolerant parser for backend commands.
type CommandMsg struct {
	Type   string `json:"type,omitempty"`   // e.g., "START_VIDEO"
	Action string `json:"action,omitempty"` // sometimes people use "action"
	RoomID int64  `json:"roomId,omitempty"`
}

// handleCommands drains /topic/agent/{mavId}/commands.
func (s *Session) handleCommands() {
	for {
		select {
		case <-s.stopCh:
			return
		default:
			msg, ok := <-s.subCmd.C
			if !ok {
				log.Println("❌ Commands subscription closed")
				return
			}
			if msg.Err != nil {
				if strings.Contains(msg.Err.Error(), "read timeout") {
					continue
				}
				log.Printf("❌ Commands message error: %v", msg.Err)
				s.requestReconnect("commands subscription error")
				return
			}

			body := string(msg.Body)
			if len(body) == 0 {
				log.Printf("⚠️ Empty commands message; ignoring")
				continue
			}
			log.Printf("📨 Commands payload: %s", body)

			var cmd CommandMsg
			if err := json.Unmarshal(msg.Body, &cmd); err != nil {
				log.Printf("⚠️ Failed to parse commands JSON: %v; payload=%s", err, body)
				continue
			}

			verb := cmd.Type
			if verb == "" {
				verb = cmd.Action
			}
			if verb == "" && cmd.RoomID > 0 {
				verb = "START_VIDEO"
			}

			switch strings.ToUpper(verb) {
			case "START_VIDEO":
				if cmd.RoomID <= 0 {
					log.Printf("⚠️ START_VIDEO missing roomId; payload=%s", body)
					continue
				}
				if s.lastRoomID != cmd.RoomID {
					log.Printf("✅ START_VIDEO received with roomId=%d", cmd.RoomID)
				} else {
					log.Printf("ℹ️ START_VIDEO received again for roomId=%d; forwarding (idempotent)", cmd.RoomID)
				}
				s.lastRoomID = cmd.RoomID
				s.enqueueLatestRoomID(cmd.RoomID)

			case "STOP_VIDEO":
				log.Printf("🛑 STOP_VIDEO received")
				// Sentinel roomId=0 -> "stop publisher"
				s.enqueueLatestRoomID(0)

			default:
				if verb == "" {
					log.Printf("ℹ️ Commands message with no type/action; ignoring")
				} else {
					log.Printf("ℹ️ Unhandled command: %s (payload=%s)", verb, body)
				}
			}
		}
	}
}

// handleSdpAnswers drains /user/queue/janus/sdp and forwards answers.
func (s *Session) handleSdpAnswers() {
	for {
		select {
		case <-s.stopCh:
			return
		default:
			msg, ok := <-s.subSdp.C
			if !ok {
				log.Println("❌ SDP subscription closed")
				return
			}
			if msg.Err != nil {
				if strings.Contains(msg.Err.Error(), "read timeout") {
					continue
				}
				log.Printf("❌ SDP message error: %v", msg.Err)
				s.requestReconnect("sdp subscription error")
				return
			}

			log.Println("📥 Received SDP ANSWER from backend")

			// (A) Dump the raw STOMP body exactly as received (likely JSON)
			dumpSDPToFile("janus-answer-raw", msg.Body)

			// The backend message is JSON. Extract the actual SDP string.
			var payload struct {
				MavID string `json:"mavId,omitempty"`
				SDP   string `json:"sdp,omitempty"`
				Type  string `json:"type,omitempty"`
			}

			if err := json.Unmarshal(msg.Body, &payload); err != nil {
				log.Printf("⚠️ Failed to parse SDP ANSWER JSON: %v; body=%s", err, string(msg.Body))
				continue
			}

			sdp := payload.SDP
			if strings.TrimSpace(sdp) == "" {
				log.Printf("⚠️ SDP ANSWER JSON had empty sdp field; body=%s", string(msg.Body))
				continue
			}

			// (B) Dump the extracted SDP (this is what you must feed to GStreamer)
			dumpSDPToFile("janus-answer-sdp", []byte(sdp))
			logSDPSummary(sdp)

			select {
			case s.SdpAnswerChan <- sdp:
				log.Println("📤 SDP ANSWER enqueued to SdpAnswerChan")
			default:
				log.Println("⚠️ SdpAnswerChan is full; dropping ANSWER")
			}
		}
	}
}

// handleControlLoop listens on all control queues and applies control to MAVLink.
func (s *Session) handleControlLoop() {
	type ctrlPair struct {
		name string
		sub  *stomp.Subscription
		kind string // "ATTITUDE" | "VELOCITY" | "RC_OVERRIDE" | "GOTO_GLOBAL" | "GIMBAL"
	}

	streams := []ctrlPair{
		{name: "attitudecontrol", sub: s.subAttitude, kind: "ATTITUDE"},
		{name: "velocitycontrol", sub: s.subVelocity, kind: "VELOCITY"},
		{name: "rc", sub: s.subRc, kind: "RC_OVERRIDE"},
		{name: "goto", sub: s.subGoto, kind: "GOTO_GLOBAL"},
		{name: "gimbal", sub: s.subGimbal, kind: "GIMBAL"},
	}

	for _, cp := range streams {
		cp := cp

		// Defensive: if a subscription is missing, skip launching its goroutine.
		if cp.sub == nil {
			log.Printf("⚠️ Control stream not subscribed (nil): %s", cp.name)
			continue
		}

		go func() {
			for {
				select {
				case <-s.stopCh:
					return

				case msg, ok := <-cp.sub.C:
					if !ok {
						log.Printf("❌ Control subscription closed: %s", cp.name)
						return
					}
					if msg.Err != nil {
						// go-stomp uses read timeouts internally; ignore those.
						if strings.Contains(msg.Err.Error(), "read timeout") {
							continue
						}
						log.Printf("❌ Control msg error on %s: %v", cp.name, msg.Err)
						s.requestReconnect("control subscription error: " + cp.name)
						return
					}

					var data map[string]any
					if err := json.Unmarshal(msg.Body, &data); err != nil {
						log.Printf("⚠️ Bad JSON on %s: %v; body=%s", cp.name, err, string(msg.Body))
						continue
					}

					eligibleVehicle := false
					modeSnap := "NO_MAVLINK"
					armedSnap := false
					isAircraft := false
					if s.mavlink != nil {
						eligibleVehicle, modeSnap, armedSnap = s.mavlink.SnapshotControlState()
						isAircraft = s.mavlink.IsAircraftLike()
					}

					eff := s.effectiveGate(isAircraft)
					ps := s.policySnapshot()
					eligible := eligibleVehicle && eff.AllowControl

					// Fine-grained feature gates
					switch cp.kind {
					case "RC_OVERRIDE":
						eligible = eligible && eff.AllowRcOverride
					case "GOTO_GLOBAL":
						eligible = eligible && eff.AllowGoto
					}

					// rate-limit logs to ≤ 2/s
					now := time.Now()
					if now.Sub(s.lastCtrlLog) >= 500*time.Millisecond {
						if eligible {
							log.Printf("🕹️ Control %s: %+v", cp.kind, data)
						} else {
							reason := denyReason(cp.kind, eligibleVehicle, isAircraft, eff)
							log.Printf(
								"🚫 Control ignored kind=%s reason=%s (mode=%s armed=%v aircraft=%v policySeen=%v policy=%+v eff=%+v)",
								cp.kind, reason, modeSnap, armedSnap, isAircraft, ps.policySeen, ps.policy, eff,
							)
						}
						s.lastCtrlLog = now
					}

					if !eligible || s.mavlink == nil {
						continue
					}

					// Update last-control time for watchdog + manage GOTO state.
					s.ctrlMu.Lock()
					s.lastCtrlAt = now
					s.lastCtrlKind = cp.kind
					s.failsafeOn = false

					if cp.kind == "GOTO_GLOBAL" {
						lat, _ := toFloat64(data["lat"])
						lon, _ := toFloat64(data["lon"])
						alt, _ := toFloat64(data["alt"])

						s.gotoActive = true
						s.gotoLat = lat
						s.gotoLon = lon
						s.gotoAltM = alt
						s.gotoStarted = now
						s.gotoLastSent = time.Time{} // will be set right after initial send
					} else if s.gotoActive {
						// Any manual/streamed control cancels an active GOTO immediately.
						s.gotoActive = false
						log.Printf("🛑 [CONTROL] Cancelled active GOTO due to %s", cp.kind)
					}
					s.ctrlMu.Unlock()

					// Apply the control to MAVLink.
					s.mavlink.ApplyControl(cp.kind, data)

					// Mark goto as sent (so keepalive loop uses correct cadence).
					if cp.kind == "GOTO_GLOBAL" {
						s.ctrlMu.Lock()
						s.gotoLastSent = now
						s.ctrlMu.Unlock()
					}
				}
			}
		}()
	}
}

// handlePolicyLoop receives the server-authoritative control policy and stores it.
// Destination: /user/queue/agent/{mavId}/policy
// Payload example:
// {"allowControl":true,"allowGoto":false,"allowRcOverride":false,"allowAircraftControl":false}
func (s *Session) handlePolicyLoop() {
	if s.subPolicy == nil {
		return
	}
	for {
		select {
		case <-s.stopCh:
			return
		default:
			msg, ok := <-s.subPolicy.C
			if !ok {
				log.Printf("❌ Policy subscription closed")
				return
			}
			if msg.Err != nil {
				if strings.Contains(msg.Err.Error(), "read timeout") {
					continue
				}
				log.Printf("❌ Policy msg error: %v", msg.Err)
				s.requestReconnect("policy subscription error")
				return
			}

			// Backward-compatible defaults: if backend doesn't send a field, keep safe defaults.
			p := ControlPolicy{
				AllowControl:         true,
				AllowGoto:            true,
				AllowRcOverride:      true,
				AllowAircraftControl: true,
				ShareGpsCoords:       true,
			}
			if err := json.Unmarshal(msg.Body, &p); err != nil {
				log.Printf("⚠️ Bad policy JSON: %v; body=%s", err, string(msg.Body))
				continue
			}

			prev, prevSeen := s.getPolicy()
			s.setPolicy(p)

			isAircraft := s.mavlink != nil && s.mavlink.IsAircraftLike()
			eff := s.effectiveGate(isAircraft)

			changed := !prevSeen || !policyEqual(prev, p)
			if changed {
				log.Printf("🛡️ Policy updated from backend: %+v (prevSeen=%v prev=%+v)", p, prevSeen, prev)

				// Explicit logs when backend policy is more restrictive than local config.
				if s.cfg.AllowControl && !p.AllowControl {
					log.Printf("🚫 Backend policy overrides agent config: AllowControl disabled")
				}
				if s.cfg.AllowGotoEffective() && !p.AllowGoto {
					log.Printf("🚫 Backend policy overrides agent config: AllowGoto disabled")
				}
				if s.cfg.AllowRcOverrideEffective() && !p.AllowRcOverride {
					log.Printf("🚫 Backend policy overrides agent config: AllowRcOverride disabled")
				}
				if s.cfg.AllowAircraftControl && !p.AllowAircraftControl {
					log.Printf("🚫 Backend policy overrides agent config: AllowAircraftControl disabled")
				}

				// Show what the agent will actually enforce after config+policy+aircraft rule.
				log.Printf("✅ Effective gate (aircraft=%v): AllowControl=%v AllowGoto=%v AllowRcOverride=%v AllowAircraftControl=%v",
					isAircraft, eff.AllowControl, eff.AllowGoto, eff.AllowRcOverride, eff.AllowAircraftControl)
			}
		}
	}
}

// telemetryLoop periodically publishes telemetry to /app/agent/{mavId}/telemetry.
func (s *Session) telemetryLoop() {
	ticker := time.NewTicker(200 * time.Millisecond) // ~5 Hz
	defer ticker.Stop()

	type Battery struct {
		Voltage   float64 `json:"voltage"`
		Remaining int     `json:"remaining"`
	}
	type Attitude struct {
		Pitch float64 `json:"pitch"`
		Roll  float64 `json:"roll"`
		Yaw   float64 `json:"yaw"`
	}
	type GPS struct {
		Lat  *float64 `json:"lat,omitempty"`
		Lon  *float64 `json:"lon,omitempty"`
		Alt  float64  `json:"alt"`
		Sats *int     `json:"sats,omitempty"`
		HDOP *float64 `json:"hdop,omitempty"`
	}
	type StatusMessage struct {
		Severity  int    `json:"severity"`
		Text      string `json:"text"`
		Timestamp int64  `json:"timestamp"`
	}
	type TelemetryOut struct {
		Type            string          `json:"type"`
		MavID           int64           `json:"mavId"`
		Mode            string          `json:"mode"`
		Armed           bool            `json:"armed"`
		Battery         Battery         `json:"battery"`
		Altitude        float64         `json:"altitude"`
		Attitude        Attitude        `json:"attitude"`
		GPS             GPS             `json:"gps"`
		Speed           float64         `json:"speed"`
		VerticalSpeed   float64         `json:"verticalSpeed"`
		Heading         float64         `json:"heading"`
		RCThrottle      int             `json:"rcThrottle"`
		Thrust          float64         `json:"thrust"`
		ThrustSource    string          `json:"thrustSource,omitempty"`
		CommandedThrust float64         `json:"commandedThrust,omitempty"`
		Status          []StatusMessage `json:"statusMessages"`
		Timestamp       int64           `json:"timestamp"`
	}

	mavID := int64(0)
	if id, err := strconv.ParseInt(s.mavID, 10, 64); err == nil {
		mavID = id
	}

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			c := s.getConn()
			if c == nil || s.mavlink == nil {
				continue
			}
			td := s.mavlink.GetTelemetryUpdate(s.mavID)

			// Effective privacy gate: local config may further restrict, but must never expand beyond backend policy.
			shareCoords := true
			if s.cfg != nil && s.cfg.IncludeGpsCoords != nil {
				shareCoords = *s.cfg.IncludeGpsCoords
			}
			{
				s.policyMu.RLock()
				p := s.policy
				seen := s.policySeen
				s.policyMu.RUnlock()
				// If we've seen a policy, treat it as authoritative. If not seen yet, default is share.
				if seen {
					shareCoords = shareCoords && p.ShareGpsCoords
				}
			}

			var sats *int
			if td.GPS.Sats != 0 {
				v := td.GPS.Sats
				sats = &v
			}
			var hdop *float64
			if td.GPS.HDOP != 0 {
				v := td.GPS.HDOP
				hdop = &v
			}

			var latPtr, lonPtr *float64
			if shareCoords {
				lat := td.GPS.Lat
				lon := td.GPS.Lon
				latPtr = &lat
				lonPtr = &lon
			}

			out := TelemetryOut{
				Type:  "TELEMETRY_UPDATE",
				MavID: mavID,
				Mode:  td.Mode,
				Armed: td.Armed,
				Battery: Battery{
					Voltage:   td.Battery.Voltage,
					Remaining: int(math.Round(td.Battery.Remaining)),
				},
				Altitude:      td.GPS.Alt,
				Attitude:      Attitude{Pitch: td.Attitude.Pitch, Roll: td.Attitude.Roll, Yaw: td.Attitude.Yaw},
				GPS:           GPS{Lat: latPtr, Lon: lonPtr, Alt: td.GPS.Alt, Sats: sats, HDOP: hdop},
				Speed:         td.GroundSpeed,
				VerticalSpeed: td.VerticalSpeed,
				Heading:       td.Heading,
				RCThrottle:    int(td.RCThrottle),
				Thrust:        td.Thrust,
				ThrustSource:  td.ThrustSource,
				CommandedThrust: func() float64 {
					// Preserve zero if not set; UI shows both actual and commanded.
					return td.CommandedThrust
				}(),
				Status: func() []StatusMessage {
					if len(td.StatusMessages) == 0 {
						return nil
					}
					out := make([]StatusMessage, 0, len(td.StatusMessages))
					for _, m := range td.StatusMessages {
						out = append(out, StatusMessage{Severity: int(m.Severity), Text: m.Text, Timestamp: m.Timestamp})
					}
					return out
				}(),
				Timestamp: td.Timestamp,
			}

			b, err := json.Marshal(out)
			if err != nil {
				log.Printf("❌ Failed to encode telemetry: %v", err)
				continue
			}

			dest := fmt.Sprintf("/app/agent/%s/telemetry", s.mavID)
			if err := s.send(dest, "application/json", b); err != nil {
				log.Printf("❌ Telemetry send failed: %v", err)
				s.requestReconnect("telemetry send failed")
			}
		}
	}
}

func (s *Session) blockUntilConnDies() error {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return fmt.Errorf("stopped")
		case <-t.C:
		}
	}
}

// controlStatusLoop publishes agent-side control-link health to the UI via the backend.
// The UI can use this to show "control link lost" and prompt the user to reconnect.
func (s *Session) controlStatusLoop() {
	// Publish fairly frequently; UI will mark link lost if updates stop.
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-tick.C:
			c := s.getConn()
			if c == nil {
				continue
			}

			// last control message receipt age (ms)
			var lastMsAgo *int64
			var failsafe bool
			s.ctrlMu.Lock()
			if !s.lastCtrlAt.IsZero() {
				v := time.Since(s.lastCtrlAt).Milliseconds()
				lastMsAgo = &v
			}
			failsafe = s.failsafeOn
			s.ctrlMu.Unlock()

			// mavlink heartbeat age (ms)
			mavlinkSeen := false
			var hbAgeMs *int64
			if s.mavlink != nil {
				health := s.mavlink.Health()
				if v, ok := health["heartbeatSeen"].(bool); ok && v {
					mavlinkSeen = true
				}
				if v, ok := health["heartbeatAgeMs"].(int64); ok {
					hbAgeMs = &v
				} else if v2, ok := health["heartbeatAgeMs"].(float64); ok {
					vv := int64(v2)
					hbAgeMs = &vv
				}
			}

			payload := map[string]any{
				"mavId":                 s.mavID,
				"ts":                    time.Now().UnixMilli(),
				"lastControlSeenMsAgo":  lastMsAgo,
				"failsafeActive":        failsafe,
				"stompConnected":        true,
				"mavlinkHeartbeatSeen":  mavlinkSeen,
				"mavlinkHeartbeatAgeMs": hbAgeMs,
			}

			data, err := json.Marshal(payload)
			if err != nil {
				continue
			}

			dest := fmt.Sprintf("/app/agent/%s/controlStatus", s.mavID)
			if err := s.send(dest, "application/json", data); err != nil {
				s.requestReconnect("controlStatus send failed")
				return
			}

		}
	}
}

func (s *Session) sendHeartbeatLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			c := s.getConn()
			if c == nil {
				continue
			}

			isAircraft := s.mavlink != nil && s.mavlink.IsAircraftLike()
			eff := s.effectiveGate(isAircraft)

			eligibleVehicle, mode, armed := s.snapshotMavlink(150 * time.Millisecond)
			enabled := eligibleVehicle && eff.AllowControl

			// Pass a policy-filtered view of cfg into capability detection.
			cfgView := *s.cfg

			// eff.AllowControl is already a bool field on AgentConfig
			cfgView.AllowControl = eff.AllowControl

			// These are now *bool fields, so assign pointers
			cfgView.AllowGoto = boolPtr(eff.AllowGoto)
			cfgView.AllowRcOverride = boolPtr(eff.AllowRcOverride)

			// This is still a bool in AgentConfig
			cfgView.AllowAircraftControl = eff.AllowAircraftControl

			caps := device.DetectCapabilities(&cfgView, s.videoDevice, s.audioDevice, enabled, isAircraft)
			snap := s.policySnapshot()

			log.Printf(
				"❤️ HB | mode=%s armed=%v eligibleVehicle=%v enabled=%v aircraft=%v policySeen=%v policy=%+v eff=%+v caps=%v",
				mode, armed, eligibleVehicle, enabled, isAircraft, snap.policySeen, snap.policy, eff, caps,
			)

			payload := map[string]any{
				"mavId":        s.mavID,
				"capabilities": caps,
				"timestamp":    time.Now().UnixMilli(),
			}

			data, err := json.Marshal(payload)
			if err != nil {
				log.Printf("❌ Failed to encode heartbeat: %v", err)
				continue
			}

			dest := fmt.Sprintf("/app/agent/%s/heartbeat", s.mavID)
			if err := s.send(dest, "application/json", data); err != nil {
				log.Printf("❌ Heartbeat send failed: %v", err)
				s.requestReconnect("heartbeat send failed")
			}
		}
	}
}

func (s *Session) SendSDP(sdp string) error {
	dest := "/app/agent/janus/sdp"
	body := map[string]string{"mavId": s.mavID, "sdp": sdp}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal SDP: %w", err)
	}
	log.Printf("📤 Sending SDP to %s (len=%d)", dest, len(sdp))
	return s.send(dest, "application/json", data)
}

func (s *Session) WaitUntilReady() {
	<-s.ReadyChan
}

func (s *Session) WaitForRoomID() int64 {
	return <-s.RoomIDChan
}

func (s *Session) requestReconnect(reason string) {
	log.Printf("↩️  Requesting STOMP reconnect: %s", reason)
	s.safeCloseStop()
}

func dumpSDPToFile(prefix string, data []byte) {
	dir := "/tmp/mavsphere-sdp"
	_ = os.MkdirAll(dir, 0755)

	ts := time.Now().Format("20060102-150405.000")
	sum := sha256.Sum256(data)

	name := prefix + "-" + ts + "-" + hex.EncodeToString(sum[:8]) + ".txt"
	path := filepath.Join(dir, name)

	_ = os.WriteFile(path, data, 0644)
	log.Printf("[SDP][dump] wrote %s (%d bytes, sha256=%x)", path, len(data), sum)
}

func logSDPSummary(sdp string) {
	log.Printf(
		"[SDP][summary] len=%d has_audio=%v has_video=%v has_ice_ufrag=%v has_ice_pwd=%v",
		len(sdp),
		strings.Contains(sdp, "m=audio"),
		strings.Contains(sdp, "m=video"),
		strings.Contains(sdp, "a=ice-ufrag:"),
		strings.Contains(sdp, "a=ice-pwd:"),
	)

	const preview = 200
	if len(sdp) > preview*2 {
		log.Printf("[SDP][preview]\n%s\n...\n%s", sdp[:preview], sdp[len(sdp)-preview:])
	} else {
		log.Printf("[SDP][preview]\n%s", sdp)
	}
}

func toFloat64(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000.0 // meters
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	φ1 := toRad(lat1)
	φ2 := toRad(lat2)
	Δφ := toRad(lat2 - lat1)
	Δλ := toRad(lon2 - lon1)
	a := math.Sin(Δφ/2)*math.Sin(Δφ/2) + math.Cos(φ1)*math.Cos(φ2)*math.Sin(Δλ/2)*math.Sin(Δλ/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return R * c
}

func boolPtr(v bool) *bool { return &v }

// handleLatencyPings listens for viewer latency probe pings and immediately echos them back as pongs.
// This gives the UI a robust "control responsiveness" RTT measurement without touching media pipelines.
func (s *Session) handleLatencyPings() {
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		sub := s.subLatencyPing
		if sub == nil {
			time.Sleep(250 * time.Millisecond)
			continue
		}

		select {
		case <-s.stopCh:
			return

		case msg, ok := <-sub.C:
			if !ok {
				log.Printf("❌ Latency ping subscription closed")
				s.requestReconnect("latency ping subscription closed")
				return
			}
			if msg.Err != nil {
				if strings.Contains(msg.Err.Error(), "read timeout") {
					continue
				}
				log.Printf("❌ Latency ping msg error: %v", msg.Err)
				s.requestReconnect("latency ping subscription error")
				return
			}

			if len(msg.Body) == 0 {
				continue
			}

			dest := fmt.Sprintf("/app/mav/%s/latency/pong", s.mavID)
			if err := s.send(dest, "application/json", msg.Body); err != nil {
				// If send fails, likely connection is dead; trigger reconnect.
				s.requestReconnect("latency pong send failed")
				return
			}
		}
	}
}
