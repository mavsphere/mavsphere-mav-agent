// internal/webui/webui.go
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mavsphere/mavsphere-agent-go/pkg/auth"
	"github.com/mavsphere/mavsphere-agent-go/pkg/config"
	"github.com/mavsphere/mavsphere-agent-go/pkg/device"
)

//go:embed ui/*
var uiFS embed.FS

type HealthProvider func() map[string]any
type ParamProbe func(name string, timeout time.Duration) (float64, bool, error)

// CommandProvider exposes testing helpers on the MAVLink connection.
type CommandProvider interface {
	ArmDisarm(arm bool, force bool) error
	SetMode(modeName string) error
}

type Server struct {
	addr    string
	cfgPath string
	srv     *http.Server

	mu         sync.Mutex
	health     HealthProvider
	probeParam ParamProbe
	commander  CommandProvider
	preRestart func() // optional graceful cleanup before exec (stop video, stomp, etc.)
}

// AttachMavlink lets main wire the health/probe providers (called by /api/health & /api/probe/param).
func (s *Server) AttachMavlink(health HealthProvider, probe ParamProbe) {
	s.mu.Lock()
	s.health = health
	s.probeParam = probe
	s.mu.Unlock()
}

// AttachCommander wires the arm/disarm and mode-set helpers for the test toolbar.
func (s *Server) AttachCommander(c CommandProvider) {
	s.mu.Lock()
	s.commander = c
	s.mu.Unlock()
}

// SetPreRestart registers an optional cleanup hook that will be called
// just before the process exits to trigger a container restart.
func (s *Server) SetPreRestart(fn func()) {
	s.mu.Lock()
	s.preRestart = fn
	s.mu.Unlock()
}

// gstElementMatrix returns a small availability map for key GStreamer elements.
// It uses gst-inspect-1.0 and returns true/false per element name.
// This is used by the UI to show which codec paths are available in the runtime.
func gstElementMatrix() map[string]bool {
	elems := []string{
		"v4l2h264enc",
		"x264enc",
		"vp8enc",
		"jpegdec",
		"h264parse",
		"webrtcbin",
		"nicesrc",
		"nicesink",
	}

	out := make(map[string]bool, len(elems))
	for _, e := range elems {
		out[e] = device.HasGstElement(e)
	}
	return out
}

func Start(addr, cfgPath string) *Server {
	s := &Server{addr: addr, cfgPath: cfgPath}

	mux := http.NewServeMux()

	// Serve UI at "/" and keep "/ui/*" for assets.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/ui/") {
			http.StripPrefix("/ui/", http.FileServer(http.FS(uiFS))).ServeHTTP(w, r)
			return
		}
		f, err := uiFS.Open("ui/index.html")
		if err != nil {
			http.Error(w, "index not found", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.Copy(w, f)
	})
	mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(uiFS))))

	// Config API (GET current config, PUT new config; optional restart on PUT ?restart=1)
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cfg := config.GetConfig()
			if cfg == nil {
				http.Error(w, "config not loaded", http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, cfg)

		case http.MethodPut:
			var incoming config.AgentConfig
			if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
				http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
				return
			}
			// Normalize/trim
			incoming.MavlinkConnection = strings.TrimSpace(incoming.MavlinkConnection)
			incoming.BackendWsURL = strings.TrimSpace(incoming.BackendWsURL)
			incoming.BackendURL = strings.TrimSpace(incoming.BackendURL)
			incoming.MavID = strings.TrimSpace(incoming.MavID)
			incoming.JanusURL = strings.TrimSpace(incoming.JanusURL)
			incoming.ThrustMode = strings.TrimSpace(incoming.ThrustMode)

			// Validate/normalize Janus URL to ws(s)://host[:port]/janus
			if j, err := normalizeJanusURL(incoming.JanusURL, incoming.BackendURL); err != nil {
				http.Error(w, "janusUrl invalid: "+err.Error(), http.StatusBadRequest)
				return
			} else {
				incoming.JanusURL = j
			}

			if err := config.SaveConfig(s.cfgPath, &incoming); err != nil {
				// The config package performs static validation (e.g. fixed resolution allow-lists).
				// The streaming pipeline already snaps to camera-supported modes at runtime, so for
				// video width/height we allow saving even if the static validator rejects it.
				if strings.Contains(strings.ToLower(err.Error()), "unsupported video resolution") {
					if err2 := saveConfigRelaxed(s.cfgPath, &incoming); err2 != nil {
						http.Error(w, "save failed: "+err.Error()+" (relaxed save also failed: "+err2.Error()+")", http.StatusBadRequest)
						return
					}
				} else {
					http.Error(w, "save failed: "+err.Error(), http.StatusBadRequest)
					return
				}
			}
			config.Update(&incoming)

			// Decide whether to restart now
			restart := r.URL.Query().Get("restart")
			if restart == "1" || strings.EqualFold(restart, "true") {
				writeJSON(w, map[string]any{"ok": true, "restarting": true})
				s.restartSelf(300 * time.Millisecond)
				return
			}
			writeJSON(w, map[string]any{"ok": true})

		default:
			w.Header().Set("Allow", "GET, PUT")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Video device capabilities (for building valid UI presets)
	mux.HandleFunc("/api/video/caps", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		videoDev, _ := device.DetectVideoDevice()
		caps, err := device.GetVideoCaps(videoDev)
		if err != nil {
			// Still return something useful for the UI.
			writeJSON(w, map[string]any{
				"ok":     false,
				"device": videoDev,
				"error":  err.Error(),
				"gst":    gstElementMatrix(),
			})
			return
		}
		writeJSON(w, map[string]any{
			"ok":   true,
			"caps": caps,
			"gst":  gstElementMatrix(),
		})
	})

	// Explicit restart endpoint
	mux.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "restarting": true})
		s.restartSelf(300 * time.Millisecond)
	})

	// Health snapshot (MAVLink + agent state)
	// ── /api/pair-code ───────────────────────────────────────────────────────
	// Returns the current pairing code if the agent is in pairing mode.
	mux.HandleFunc("/api/pair-code", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		code := auth.GetPairingCode()
		writeJSON(w, map[string]any{"pairingCode": code})
	})

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.mu.Lock()
		h := s.health
		s.mu.Unlock()
		if h == nil {
			writeJSON(w, map[string]any{"attached": false})
			return
		}
		writeJSON(w, h())
	})

	// Control-path probe (PARAM_REQUEST_READ; defaults to SYSID_MYGCS)
	mux.HandleFunc("/api/probe/param", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "SYSID_MYGCS"
		}
		s.mu.Lock()
		probe := s.probeParam
		s.mu.Unlock()
		if probe == nil {
			writeJSON(w, map[string]any{"ok": false, "error": "probe not available"})
			return
		}

		start := time.Now()
		val, ok, err := probe(name, 1500*time.Millisecond)
		elapsed := time.Since(start).Milliseconds()
		if err != nil {
			writeJSON(w, map[string]any{
				"ok": false, "param": name, "elapsedMs": elapsed, "error": err.Error(),
			})
			return
		}
		writeJSON(w, map[string]any{
			"ok": ok, "param": name, "value": val, "elapsedMs": elapsed,
		})
	})

	// ── Test toolbar: arm / disarm ──────────────────────────────────────────
	// POST /api/mavcmd/arm   body: {"arm":true/false,"force":false}
	mux.HandleFunc("/api/mavcmd/arm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.mu.Lock()
		cmd := s.commander
		s.mu.Unlock()
		if cmd == nil {
			writeJSON(w, map[string]any{"ok": false, "error": "MAVLink not connected yet"})
			return
		}
		var body struct {
			Arm   bool `json:"arm"`
			Force bool `json:"force"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if err := cmd.ArmDisarm(body.Arm, body.Force); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	// ── Test toolbar: set flight mode ────────────────────────────────────────
	// POST /api/mavcmd/mode  body: {"mode":"MANUAL"}
	mux.HandleFunc("/api/mavcmd/mode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.mu.Lock()
		cmd := s.commander
		s.mu.Unlock()
		if cmd == nil {
			writeJSON(w, map[string]any{"ok": false, "error": "MAVLink not connected yet"})
			return
		}
		var body struct {
			Mode string `json:"mode"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Mode == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "mode is required"})
			return
		}
		if err := cmd.SetMode(body.Mode); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "mode": body.Mode})
	})

	s.srv = &http.Server{
		Addr:    addr,
		Handler: logRequests(mux),
	}

	go func() {
		log.Printf("[UI] config UI listening on http://%s/", addr)
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[UI] server error: %v", err)
		}
	}()

	return s
}

// normalizeJanusURL coerces user input to ws(s)://host[:port]/janus
func normalizeJanusURL(input string, backendURL string) (string, error) {
	in := strings.TrimSpace(input)
	if in == "" {
		return "", fmt.Errorf("empty")
	}

	defaultScheme := "ws"
	if u, err := url.Parse(backendURL); err == nil && strings.EqualFold(u.Scheme, "https") {
		defaultScheme = "wss"
	}

	if !strings.Contains(in, "://") {
		in = defaultScheme + "://" + in
	}

	u, err := url.Parse(in)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("missing host")
	}

	switch strings.ToLower(u.Scheme) {
	case "ws", "wss":
		// ok
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}

	if u.Path == "" || u.Path == "/" {
		u.Path = "/janus"
	} else if !strings.HasSuffix(u.Path, "/janus") {
		p := strings.TrimRight(u.Path, "/")
		u.Path = p + "/janus"
	}

	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// restartSelf schedules graceful cleanup then exits.
// Docker's restart: unless-stopped brings the container back up reading
// the updated config from disk. Simpler and more reliable than exec/spawn.
func (s *Server) restartSelf(delay time.Duration) {
	go func() {
		time.Sleep(delay)

		// 1) App-specific cleanup hook
		func() {
			s.mu.Lock()
			fn := s.preRestart
			s.mu.Unlock()
			if fn != nil {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[UI] pre-restart hook panicked: %v", r)
					}
				}()
				fn()
			}
		}()

		// 2) Gracefully stop HTTP server
		if s.srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
			_ = s.srv.Shutdown(ctx)
			cancel()
		}

		log.Printf("[UI] restarting — exiting process (Docker will restart container)")
		os.Exit(0)
	}()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			log.Printf("[UI] %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		} else {
			log.Printf("[UI] %s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// Stop gracefully shuts down the HTTP server.
func (s *Server) Stop() error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

// saveConfigRelaxed persists the config JSON without running the config package validation.
// This is used as a fallback when the static validator rejects a resolution that is in fact
// supported by the camera (we discover support dynamically via /api/video/caps and also
// snap to supported modes at runtime).
func saveConfigRelaxed(path string, conf *config.AgentConfig) error {
	if conf == nil {
		return fmt.Errorf("nil config")
	}
	b, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
