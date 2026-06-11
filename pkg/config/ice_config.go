package config

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mavsphere/mavsphere-agent-go/pkg/iceutil"
)

type iceAPIResponse struct {
	TTL        int    `json:"ttl"`
	Realm      string `json:"realm"`
	ForceRelay bool   `json:"forceRelay"`
	ICEServers []struct {
		Urls           any    `json:"urls"`
		Username       string `json:"username"`
		Credential     string `json:"credential"`
		CredentialType string `json:"credentialType"`
	} `json:"iceServers"`
}

var lastTTLSeconds int64

var lastTurnURLs atomic.Value // stores []string

func GetLastTurnURLs() []string {
	if v := lastTurnURLs.Load(); v != nil {
		if s, ok := v.([]string); ok {
			// return a copy to avoid accidental mutation
			out := make([]string, len(s))
			copy(out, s)
			return out
		}
	}
	return nil
}

func GetLastIceTTLSeconds() int {
	return int(atomic.LoadInt64(&lastTTLSeconds))
}

func NextIceRefreshDelay(margin time.Duration) time.Duration {
	ttl := time.Duration(GetLastIceTTLSeconds()) * time.Second
	if ttl <= 0 {
		return 0
	}
	d := ttl - margin
	if d < 2*time.Minute {
		d = 2 * time.Minute
	}
	return d
}

func BuildGStreamerICEFromBackendOrConfig(
	token string,
	refresh func() (string, error),
) (stunURL, turnURL, user, pass string, forceRelay bool) {
	cfg := GetConfig()
	if cfg == nil {
		atomic.StoreInt64(&lastTTLSeconds, 0)
		return "stun://stun.l.google.com:19302", "", "", "", false
	}

	su, tu, u, p, ok := fetchFromBackend(cfg, token, refresh)
	if ok {
		// Safe log
		log.Printf("[ICE] Backend OK | TTL=%ds | STUN=%q | TURN=%q | user=%s",
			GetLastIceTTLSeconds(),
			su,
			iceutil.PrettyTurnVariant(tu),
			maskTurnUsername(u),
		)

		// TEMPORARY DEBUG: print exact creds when ICE_DEBUG=1
		if os.Getenv("ICE_DEBUG") == "1" {
			log.Printf("[ICE][DEBUG] RAW TURN username=%q password=%q", u, p)
		}

		// Do NOT rewrite username. Use exactly what backend provided.
		if looksLikeBadUser(u) {
			log.Printf("[ICE][WARN] TURN username looks numeric-only after colon (%s). Ensure backend builds username from the authenticated user, not mavId.",
				maskTurnUsername(u),
			)
		}
		return su, tu, u, p, false
	}

	log.Printf("ICE: backend unavailable; using STUN-only fallback")
	atomic.StoreInt64(&lastTTLSeconds, 0)
	return "stun://stun.l.google.com:19302", "", "", "", false
}

func fetchFromBackend(cfg *AgentConfig, token string, refresh func() (string, error)) (stunURL, turnURL, user, pass string, ok bool) {
	base := strings.TrimSpace(cfg.BackendURL)
	if base == "" {
		return "", "", "", "", false
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", "", "", "", false
	}

	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/ice"
	q := u.Query()

	// You may keep mavId for other backend logic; with the new backend code it won't affect TURN username.
	if strings.TrimSpace(cfg.MavID) != "" {
		q.Set("mavId", cfg.MavID)
	}
	u.RawQuery = q.Encode()

	doReq := func(bearer string) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Cache-Control", "no-store, no-cache, max-age=0, must-revalidate")
		req.Header.Set("Pragma", "no-cache")
		if strings.TrimSpace(bearer) != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		client := &http.Client{Timeout: 6 * time.Second}
		return client.Do(req)
	}

	resp, err := doReq(token)
	if err != nil {
		log.Printf("ICE: backend fetch failed: %v", err)
		return "", "", "", "", false
	}
	defer resp.Body.Close()

	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && refresh != nil {
		_ = resp.Body.Close()
		newTok, rerr := refresh()
		if rerr != nil {
			log.Printf("ICE: token refresh failed: %v", rerr)
			return "", "", "", "", false
		}
		resp2, err2 := doReq(newTok)
		if err2 != nil {
			log.Printf("ICE: backend fetch (after refresh) failed: %v", err2)
			return "", "", "", "", false
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			log.Printf("ICE: backend responded %d (after refresh)", resp2.StatusCode)
			return "", "", "", "", false
		}
		var api iceAPIResponse
		if err := json.NewDecoder(resp2.Body).Decode(&api); err != nil {
			log.Printf("ICE: decode failed (after refresh): %v", err)
			return "", "", "", "", false
		}
		atomic.StoreInt64(&lastTTLSeconds, int64(api.TTL))
		su, tu, uu, pp, err := selectForGStreamer(api)
		if err != nil {
			log.Printf("ICE: selection failed (after refresh): %v", err)
			return "", "", "", "", false
		}
		return su, tu, uu, pp, true
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("ICE: backend responded %d", resp.StatusCode)
		return "", "", "", "", false
	}
	var api iceAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&api); err != nil {
		log.Printf("ICE: decode failed: %v", err)
		return "", "", "", "", false
	}
	atomic.StoreInt64(&lastTTLSeconds, int64(api.TTL))

	su, tu, uu, pp, err := selectForGStreamer(api)
	if err != nil {
		log.Printf("ICE: selection failed: %v", err)
		return "", "", "", "", false
	}
	return su, tu, uu, pp, true
}

func selectForGStreamer(api iceAPIResponse) (stunURL, turnURL, user, pass string, err error) {
	var stun string

	// Collect TURN URLs in response order
	var turnURLs []string
	var turnUser, turnPass string

	// For dedupe while preserving order
	seenTurn := map[string]struct{}{}

	// Helper: normalize stun/turn url string into parseable scheme:// form
	normalize := func(raw string) string {
		s := strings.TrimSpace(raw)
		if s == "" {
			return ""
		}
		l := strings.ToLower(s)
		if strings.HasPrefix(l, "turn:") && !strings.Contains(s, "://") {
			return "turn://" + strings.TrimPrefix(s, "turn:")
		}
		if strings.HasPrefix(l, "turns:") && !strings.Contains(s, "://") {
			return "turns://" + strings.TrimPrefix(s, "turns:")
		}
		if strings.HasPrefix(l, "stun:") && !strings.Contains(s, "://") {
			// backend might return stun:host:port; gstreamer wants stun://
			return "stun://" + strings.TrimPrefix(s, "stun:")
		}
		return s
	}

	// Parse urls field that can be string or array
	extractURLs := func(v any) []string {
		var urls []string
		switch vv := v.(type) {
		case string:
			if strings.TrimSpace(vv) != "" {
				urls = append(urls, vv)
			}
		case []any:
			for _, it := range vv {
				if str, ok := it.(string); ok && strings.TrimSpace(str) != "" {
					urls = append(urls, str)
				}
			}
		case []string:
			for _, str := range vv {
				if strings.TrimSpace(str) != "" {
					urls = append(urls, str)
				}
			}
		}
		return urls
	}

	// 1) Collect STUN + TURN urls in order
	for _, s := range api.ICEServers {
		urls := extractURLs(s.Urls)

		// capture creds from the TURN server entry (the one that includes username/credential)
		if s.Username != "" && s.Credential != "" && turnUser == "" {
			turnUser = s.Username
			turnPass = s.Credential
		}

		for _, raw := range urls {
			n := normalize(raw)
			if n == "" {
				continue
			}
			ln := strings.ToLower(n)

			if strings.HasPrefix(ln, "stun://") && stun == "" {
				stun = n
				continue
			}

			if strings.HasPrefix(ln, "turn://") || strings.HasPrefix(ln, "turns://") {
				if _, ok := seenTurn[n]; ok {
					continue
				}
				seenTurn[n] = struct{}{}
				turnURLs = append(turnURLs, n)
			}
		}
	}

	if stun == "" {
		stun = "stun://stun.l.google.com:19302"
	}

	// Store full list for janusvr_sink.go logging/selection
	lastTurnURLs.Store(turnURLs)

	if os.Getenv("ICE_DEBUG") == "1" {
		log.Printf("[ICE][DEBUG] backend forceRelay=%v ttl=%d stun=%q turnURLs=%v user=%q",
			api.ForceRelay, api.TTL, stun, turnURLs, maskTurnUsername(turnUser))
	}

	if len(turnURLs) == 0 {
		log.Printf("ICE: backend provided no TURN; STUN=%q", stun)
		return stun, "", "", "", nil
	}

	// 2) Choose best TURN (prefer UDP always; do NOT auto-prefer turns/tcp)
	choose := func(pred func(string) bool) (string, bool) {
		for _, u := range turnURLs {
			if pred(strings.ToLower(u)) {
				return u, true
			}
		}
		return "", false
	}

	// Prefer: turn udp 3478
	if u, ok := choose(func(lu string) bool {
		return strings.HasPrefix(lu, "turn://") && strings.Contains(lu, ":3478") && strings.Contains(lu, "transport=udp")
	}); ok {
		turnURL = u
	} else if u, ok := choose(func(lu string) bool {
		// then: turn udp 443 (if you ever add it)
		return strings.HasPrefix(lu, "turn://") && strings.Contains(lu, ":443") && strings.Contains(lu, "transport=udp")
	}); ok {
		turnURL = u
	} else if u, ok := choose(func(lu string) bool {
		// then: turns tcp 443
		return strings.HasPrefix(lu, "turns://") && strings.Contains(lu, ":443") && strings.Contains(lu, "transport=tcp")
	}); ok {
		turnURL = u
	} else {
		// fallback: first
		turnURL = turnURLs[0]
	}

	log.Printf("ICE: backend TURN selected: %s user=%s",
		iceutil.PrettyTurnVariant(turnURL),
		maskTurnUsername(turnUser),
	)

	return stun, turnURL, turnUser, turnPass, nil
}

func maskTurnUsername(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return "(empty)"
	}

	// Support both separators: '.' (new) and ':' (old)
	sep := "."
	parts := strings.SplitN(u, sep, 2)
	if len(parts) != 2 {
		sep = ":"
		parts = strings.SplitN(u, sep, 2)
	}
	if len(parts) != 2 {
		if len(u) <= 4 {
			return "***"
		}
		return u[:2] + "***" + u[len(u)-2:]
	}

	exp, who := parts[0], parts[1]
	if who == "" {
		return exp + sep + "(empty)"
	}
	runes := []rune(who)
	head := string(runes[0])
	return fmt.Sprintf("%s%s%s***", exp, sep, head)
}

func looksLikeBadUser(u string) bool {
	u = strings.TrimSpace(u)
	if u == "" {
		return false
	}
	parts := strings.SplitN(u, ".", 2)
	if len(parts) != 2 {
		parts = strings.SplitN(u, ":", 2)
	}
	if len(parts) != 2 {
		return false
	}
	suf := parts[1]
	if suf == "" {
		return true
	}
	for _, r := range suf {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
