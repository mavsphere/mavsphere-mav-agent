package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type AgentConfig struct {
	// IDs and endpoints
	MavID        string `json:"mavId"`        // e.g. "1"
	BackendWsURL string `json:"backendWsUrl"` // e.g. ws://localhost:8080/api/ws/agent
	BackendURL   string `json:"backendUrl"`   // e.g. http://localhost:8080 (used by auth/REST)
	JanusURL     string `json:"janusUrl"`     // e.g. ws://localhost:8188/janus or wss://mavsphere.com/janus

	// Optional local override for ICE transport policy.
	// - nil: no override (use backend ICEConfig.ForceRelay)
	// - true: force relay
	// - false: force all
	ForceRelayOverride *bool `json:"forceRelay,omitempty"`

	// Auth (used by pkg/auth)
	Username string `json:"username"`
	Password string `json:"password"`

	// MAVLink transport
	MavlinkConnection string `json:"mavlinkConnection"` // serial:/dev/...:57600 | udpclient:host:port | tcpclient:host:port
	AgentGcsID        int    `json:"agentGcsId"`        // default 255

	// Control gating
	AllowControl bool `json:"allowControl"`

	// Privacy: when false, the agent will NOT send GPS coordinates (lat/lon)
	// in telemetry updates. Altitude is still sent.
	//
	// Default: true (send coordinates).
	IncludeGpsCoords *bool `json:"includeGpsCoords,omitempty"`

	// Fine-grained control feature gating. These are surfaced as capabilities
	// to the backend/UI and enforced by the agent.
	AllowGoto       *bool `json:"allowGoto,omitempty"`       // map click / go-to (GUIDED)
	AllowRcOverride *bool `json:"allowRcOverride,omitempty"` // RC_CHANNELS_OVERRIDE

	// Aircraft safety gate: if the connected vehicle is aircraft-like, control
	// is denied unless this is explicitly enabled.
	AllowAircraftControl bool `json:"allowAircraftControl,omitempty"`

	// Thrust profile for rover-like VELOCITY + RC mapping.
	// Values are expressed as *capability flags* so the UI/backend/agent stay aligned:
	// - "THRUST_FWD_REV" (default): forward+reverse (bidirectional) intent.
	// - "THRUST_FWD_ONLY": forward-only intent.
	//
	// Back-compat: legacy values "FWD_REV" and "FWD_ONLY" are accepted and normalized.
	ThrustMode string `json:"thrustMode,omitempty"`

	// Optional ALSA audio capture device override, e.g. "hw:1,0"
	AudioDevice string `json:"audioDevice,omitempty"`
	// Optional V4L2 video capture device override, e.g. "/dev/video17".
	// If empty, the agent auto-detects the first compatible capture device.
	// Use this when auto-detection picks the wrong device (e.g. a hardware
	// codec node instead of a real camera, or a loopback device).
	VideoDevice string `json:"videoDevice,omitempty"`
	VideoWidth  int    `json:"videoWidth"`
	VideoHeight int    `json:"videoHeight"`
	VideoFps    int    `json:"videoFps"`

	// Video encoder selection (optional; defaults preserve current behavior)
	// videoCodec may be "vp8", "h264", or "auto".
	// - "auto": try H.264 first (HW then SW), then fall back to VP8 as a last resort.
	VideoCodec  string `json:"videoCodec"`            // "vp8" (default) | "h264" | "auto"
	H264Encoder string `json:"h264Encoder"`           // "auto" | "v4l2h264enc" | "x264enc"
	H264Profile string `json:"h264Profile,omitempty"` // "baseline" (default) | "main" | "high"
	// Used pre encode
	H264BitrateBps int `json:"h264BitrateBps"` // e.g. 1500000 (4G), 3000000 (LAN)

	// Prefer MJPG (image/jpeg) capture when the camera supports it.
	// This is important for many USB cameras that only provide 1080p30/720p30 as MJPG,
	// while raw YUYV at those resolutions is limited to low FPS.
	PreferMJPG *bool `json:"preferMjpg,omitempty"`
	// Optional: explicit camera pixel format selection based on v4l2 capabilities.
	// "AUTO" (default): follow PreferMJPG behaviour and available modes.
	// "MJPG" | "YUYV" | "H264": force that camera output mode when available.
	VideoPixFmt string `json:"videoPixFmt,omitempty"`

	// General bit rates set on sink regardless of encoder
	WebRTCStartBitrateBps int `json:"webrtcStartBitrateBps"`
	WebRTCMaxBitrateBps   int `json:"webrtcMaxBitrateBps"`
	WebRTCMinBitrateBps   int `json:"webrtcMinBitrateBps"`

	// Safety / failsafe settings (dead-man switch)
	Failsafe FailsafeConfig `json:"failsafe"`
}

// FailsafeConfig implements a control-path "dead-man". If the agent stops receiving
// control messages for ControlTimeoutMs, it will actively send a neutral/stop command.
// While stale, it can re-issue the stop every ReissueStopMs to keep asserting safety.
//
// Notes:
//   - RC override stop values depend on whether the vehicle supports reverse (center=1500)
//     or is forward-only (minimum=1000).
//   - For velocity/attitude, the agent will zero velocities/rates and set thrust to 0.
type FailsafeConfig struct {
	ControlTimeoutMs int                      `json:"controlTimeoutMs"`
	ReissueStopMs    int                      `json:"reissueStopMs"`
	RcOverride       RcOverrideFailsafeConfig `json:"rcOverride"`
}

type RcOverrideFailsafeConfig struct {
	// PWM values (1000..2000)
	ThrottleStopPwm int `json:"throttleStopPwm"`
	SteeringStopPwm int `json:"steeringStopPwm"`
}

// Effective getters (nil-safe defaults)

// AllowGotoEffective returns whether map-click GOTO (GUIDED) is enabled.
// Default: true.
func (c *AgentConfig) AllowGotoEffective() bool {
	if c == nil || c.AllowGoto == nil {
		return true
	}
	return *c.AllowGoto
}

// AllowRcOverrideEffective returns whether RC_CHANNELS_OVERRIDE is enabled.
// Default: false (must be explicitly opted in for safety).
func (c *AgentConfig) AllowRcOverrideEffective() bool {
	if c == nil || c.AllowRcOverride == nil {
		return false
	}
	return *c.AllowRcOverride
}

var (
	mu     sync.RWMutex
	global *AgentConfig
)

func LoadConfig(path string) (*AgentConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg AgentConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&cfg)

	mu.Lock()
	global = &cfg
	mu.Unlock()
	return &cfg, nil
}

func applyDefaults(cfg *AgentConfig) {
	if cfg.AgentGcsID == 0 {
		cfg.AgentGcsID = 255
	}
	if strings.TrimSpace(cfg.MavID) == "" {
		cfg.MavID = "1"
	}
	if strings.TrimSpace(cfg.BackendWsURL) == "" {
		cfg.BackendWsURL = "ws://localhost:8080/api/ws/agent"
	}
	if strings.TrimSpace(cfg.BackendURL) == "" {
		cfg.BackendURL = "http://localhost:8080"
	}
	if strings.TrimSpace(cfg.MavlinkConnection) == "" {
		cfg.MavlinkConnection = "udpclient:127.0.0.1:14551"
	}

	// Privacy defaults
	if cfg.IncludeGpsCoords == nil {
		v := true
		cfg.IncludeGpsCoords = &v
	}

	// Fine-grained control feature defaults
	if cfg.AllowGoto == nil {
		v := true
		cfg.AllowGoto = &v
	}
	// RC override is opt-in: operators must explicitly set allowRcOverride=true
	// in their config. This is a safety default — RC_CHANNELS_OVERRIDE bypasses
	// the vehicle's own RC input and should not be active unless the operator
	// has considered the implications for their specific setup.
	if cfg.AllowRcOverride == nil {
		v := false
		cfg.AllowRcOverride = &v
	}
	// Aircraft control is opt-in (default false).
	// (If you want to allow aircraft control, set allowAircraftControl=true)
	// NOTE: do not auto-enable based on allowControl.
	// This field is treated as a higher-safety gate.

	// Thrust mode default + normalization
	// Persisted values (new): THRUST_FWD_REV | THRUST_FWD_ONLY
	// Accepted legacy values: FWD_REV | FWD_ONLY
	if strings.TrimSpace(cfg.ThrustMode) == "" {
		cfg.ThrustMode = "THRUST_FWD_REV"
	}
	{
		m := strings.ToUpper(strings.TrimSpace(cfg.ThrustMode))
		switch m {
		case "THRUST_FWD_REV", "THRUST_FWD_ONLY":
			cfg.ThrustMode = m
		case "FWD_REV":
			cfg.ThrustMode = "THRUST_FWD_REV"
		case "FWD_ONLY":
			cfg.ThrustMode = "THRUST_FWD_ONLY"
		default:
			cfg.ThrustMode = "THRUST_FWD_REV"
		}
	}

	// Video encoding defaults
	if strings.TrimSpace(cfg.VideoCodec) == "" {
		cfg.VideoCodec = "vp8"
	}
	if strings.TrimSpace(cfg.H264Encoder) == "" {
		cfg.H264Encoder = "auto"
	}

	if strings.TrimSpace(cfg.H264Profile) == "" {
		// Safer default for broad decoder compatibility.
		cfg.H264Profile = "baseline"
	}

	// Prefer MJPG by default: if the camera doesn't support it, the agent will fall back.
	// This enables high-res USB camera modes (1080p30/720p30) that are commonly MJPG-only.
	// If you need to preserve legacy behavior, set preferMjpg=false in config.
	// Note: defaulting to true is safe because the pipeline attempts will fall back quickly.
	if cfg.PreferMJPG == nil {
		v := true
		cfg.PreferMJPG = &v
	}

	if cfg.H264BitrateBps <= 0 {
		if strings.EqualFold(cfg.VideoCodec, "h264") || strings.EqualFold(cfg.VideoCodec, "auto") {
			cfg.H264BitrateBps = 1_500_000
		} else {
			cfg.H264BitrateBps = 2_000_000
		}
	}

	if cfg.WebRTCMaxBitrateBps <= 0 {
		// Use existing bitrate as a "target/max" default
		cfg.WebRTCMaxBitrateBps = cfg.H264BitrateBps
		if cfg.WebRTCMaxBitrateBps <= 0 {
			cfg.WebRTCMaxBitrateBps = 1_500_000
		}
	}
	if cfg.WebRTCStartBitrateBps <= 0 {
		cfg.WebRTCStartBitrateBps = 800_000
		if cfg.WebRTCStartBitrateBps > cfg.WebRTCMaxBitrateBps {
			cfg.WebRTCStartBitrateBps = cfg.WebRTCMaxBitrateBps
		}
	}
	if cfg.WebRTCMinBitrateBps <= 0 {
		cfg.WebRTCMinBitrateBps = 150_000
		if cfg.WebRTCMinBitrateBps > cfg.WebRTCStartBitrateBps {
			cfg.WebRTCMinBitrateBps = cfg.WebRTCStartBitrateBps
		}
	}

	// ---- Safety defaults (dead-man) ----
	if cfg.Failsafe.ControlTimeoutMs <= 0 {
		cfg.Failsafe.ControlTimeoutMs = 450
	}
	if cfg.Failsafe.ReissueStopMs <= 0 {
		cfg.Failsafe.ReissueStopMs = 500
	}
	// RC override defaults: assume bidirectional throttle (reverse supported)
	if cfg.Failsafe.RcOverride.ThrottleStopPwm == 0 {
		cfg.Failsafe.RcOverride.ThrottleStopPwm = 1500
	}
	if cfg.Failsafe.RcOverride.SteeringStopPwm == 0 {
		cfg.Failsafe.RcOverride.SteeringStopPwm = 1500
	}

	// Janus URL defaulting:
	// 1) explicit in config wins
	// 2) env JANUS_URL next (optional)
	// 3) derive from BackendURL host with /janus
	// 4) final fallback: local dev Janus
	if strings.TrimSpace(cfg.JanusURL) == "" {
		if v := strings.TrimSpace(os.Getenv("JANUS_URL")); v != "" {
			cfg.JanusURL = v
		} else if j := deriveJanusFromBackend(cfg.BackendURL); j != "" {
			cfg.JanusURL = j
		} else {
			cfg.JanusURL = "ws://localhost:8188/janus"
		}
	}
}

// Build ws(s)://<backend-host>/janus from BackendURL
func deriveJanusFromBackend(backend string) string {
	if backend == "" {
		return ""
	}
	u, err := url.Parse(backend)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := "ws"
	if u.Scheme == "https" {
		scheme = "wss"
	}
	host := u.Host // includes port if present
	return fmt.Sprintf("%s://%s/janus", scheme, host)
}

func SaveConfig(path string, cfg *AgentConfig) error {
	if cfg == nil {
		return errors.New("nil cfg")
	}
	applyDefaults(cfg)

	// Normalize (store consistent values)
	cfg.VideoCodec = strings.ToLower(strings.TrimSpace(cfg.VideoCodec))
	cfg.H264Encoder = strings.ToLower(strings.TrimSpace(cfg.H264Encoder))
	cfg.H264Profile = strings.ToLower(strings.TrimSpace(cfg.H264Profile))
	switch cfg.H264Profile {
	case "baseline", "constrained-baseline", "main", "high":
		// ok
	default:
		return fmt.Errorf("invalid h264Profile %q (allowed: baseline|constrained-baseline|main|high)", cfg.H264Profile)
	}

	if cfg.AgentGcsID < 1 || cfg.AgentGcsID > 255 {
		return fmt.Errorf("agentGcsId must be 1..255")
	}
	if strings.TrimSpace(cfg.MavlinkConnection) == "" {
		return fmt.Errorf("mavlinkConnection is required")
	}

	// --- Thrust mode validation ---
	// (applyDefaults already normalizes/backs-compat)
	switch strings.ToUpper(strings.TrimSpace(cfg.ThrustMode)) {
	case "THRUST_FWD_REV", "THRUST_FWD_ONLY":
		// ok
	default:
		return fmt.Errorf("thrustMode must be 'THRUST_FWD_REV' or 'THRUST_FWD_ONLY'")
	}

	// --- Video config validation ---
	switch cfg.VideoCodec {
	case "vp8", "h264", "auto":
		// ok
	default:
		return fmt.Errorf("videoCodec must be 'vp8', 'h264', or 'auto'")
	}

	switch cfg.H264Encoder {
	case "auto", "v4l2h264enc", "x264enc":
		// ok
	default:
		return fmt.Errorf("h264Encoder must be 'auto', 'v4l2h264enc', or 'x264enc'")
	}

	// Reasonable bounds; protects against typos like 1500 instead of 1500000
	if cfg.H264BitrateBps < 100_000 || cfg.H264BitrateBps > 20_000_000 {
		return fmt.Errorf("h264BitrateBps out of range (expected 100000..20000000)")
	}

	// Match your UI preset list (add/remove to stay in sync with index.html)
	allowedRes := map[[2]int]bool{
		{320, 180}:   true,
		{640, 360}:   true,
		{854, 480}:   true,
		{960, 540}:   true,
		{1280, 720}:  true,
		{1920, 1080}: true,
	}
	if cfg.VideoWidth > 0 || cfg.VideoHeight > 0 {
		if cfg.VideoWidth <= 0 || cfg.VideoHeight <= 0 {
			return fmt.Errorf("videoWidth and videoHeight must both be set")
		}
		if !allowedRes[[2]int{cfg.VideoWidth, cfg.VideoHeight}] {
			return fmt.Errorf("unsupported video resolution %dx%d", cfg.VideoWidth, cfg.VideoHeight)
		}
	}

	if cfg.VideoFps > 0 {
		switch cfg.VideoFps {
		case 5, 10, 15, 20, 24, 25, 30:
			// ok (25 optional; keep if you want PAL-friendly)
		default:
			return fmt.Errorf("unsupported videoFps %d", cfg.VideoFps)
		}
	}

	// Raspberry Pi v4l2h264enc safety envelope: keep configs that negotiate cleanly.
	if (strings.EqualFold(cfg.VideoCodec, "h264") || strings.EqualFold(cfg.VideoCodec, "auto")) &&
		(strings.EqualFold(cfg.H264Encoder, "auto") || strings.EqualFold(cfg.H264Encoder, "v4l2h264enc")) {

		if cfg.VideoWidth == 1920 && cfg.VideoHeight == 1080 && cfg.VideoFps > 30 {
			return fmt.Errorf("Pi HW H.264 supports up to 1080p30; reduce FPS to 30")
		}
		if cfg.VideoWidth > 1920 || cfg.VideoHeight > 1080 {
			return fmt.Errorf("Pi HW H.264 supports up to 1920x1080; reduce resolution")
		}
	}

	// --- WebRTC bitrate envelope (applies to all codecs via janusvrwebrtcsink) ---
	// Reasonable bounds; protects against typos like 1500 instead of 1500000.
	if cfg.WebRTCMinBitrateBps < 50_000 || cfg.WebRTCMinBitrateBps > 20_000_000 {
		return fmt.Errorf("webrtcMinBitrateBps out of range (expected 50000..20000000)")
	}
	if cfg.WebRTCStartBitrateBps < 50_000 || cfg.WebRTCStartBitrateBps > 20_000_000 {
		return fmt.Errorf("webrtcStartBitrateBps out of range (expected 50000..20000000)")
	}
	if cfg.WebRTCMaxBitrateBps < 50_000 || cfg.WebRTCMaxBitrateBps > 20_000_000 {
		return fmt.Errorf("webrtcMaxBitrateBps out of range (expected 50000..20000000)")
	}

	// Must be ordered; avoids strange behavior in congestion control.
	if cfg.WebRTCMinBitrateBps > cfg.WebRTCStartBitrateBps {
		return fmt.Errorf("webrtcMinBitrateBps must be <= webrtcStartBitrateBps")
	}
	if cfg.WebRTCStartBitrateBps > cfg.WebRTCMaxBitrateBps {
		return fmt.Errorf("webrtcStartBitrateBps must be <= webrtcMaxBitrateBps")
	}

	// --- Safety / failsafe validation ---
	if cfg.Failsafe.ControlTimeoutMs < 100 || cfg.Failsafe.ControlTimeoutMs > 5000 {
		return fmt.Errorf("failsafe.controlTimeoutMs out of range (expected 100..5000)")
	}
	if cfg.Failsafe.ReissueStopMs < 100 || cfg.Failsafe.ReissueStopMs > 5000 {
		return fmt.Errorf("failsafe.reissueStopMs out of range (expected 100..5000)")
	}
	// RC override stop PWM
	if cfg.Failsafe.RcOverride.ThrottleStopPwm < 1000 || cfg.Failsafe.RcOverride.ThrottleStopPwm > 2000 {
		return fmt.Errorf("failsafe.rcOverride.throttleStopPwm out of range (expected 1000..2000)")
	}
	if cfg.Failsafe.RcOverride.SteeringStopPwm < 1000 || cfg.Failsafe.RcOverride.SteeringStopPwm > 2000 {
		return fmt.Errorf("failsafe.rcOverride.steeringStopPwm out of range (expected 1000..2000)")
	}

	tmp := path + ".tmp"
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mk parent: %w", err)
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}

	mu.Lock()
	global = cfg
	mu.Unlock()
	return nil
}

func GetConfig() *AgentConfig {
	mu.RLock()
	defer mu.RUnlock()
	return global
}

func Update(cfg *AgentConfig) {
	mu.Lock()
	global = cfg
	mu.Unlock()
}
