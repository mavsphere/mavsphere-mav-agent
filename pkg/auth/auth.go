// Package auth handles backend authentication for the mav-agent.
//
// The agent authenticates using a pre-issued agent token (set via the
// pairing flow into the agentToken field in config.json) by calling
// POST /api/agent/token-login. No password is ever stored on the device.
//
// Error classification for 401 responses:
//
//	ErrTokenRevoked  — backend explicitly says token is invalid or revoked.
//	                   Caller should clear the token and enter pairing mode.
//	transient error  — 401 without explicit revocation body (e.g. clock skew).
//	                   Caller should retry with normal backoff, not clear token.
package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mavsphere/mavsphere-agent-go/pkg/config"
)

// ── Request/response types ────────────────────────────────────────────────────

type TokenLoginRequest struct {
	AgentToken string `json:"agentToken"`
	MavID      string `json:"mavId"`
}

type LoginResponse struct {
	Token string `json:"token"`
}

// rateLimitResponse mirrors the backend's LoginRateLimitResponse DTO.
type rateLimitResponse struct {
	Error             string `json:"error"`
	Message           string `json:"message"`
	RetryAfterSeconds int64  `json:"retryAfterSeconds"`
	BlockedBy         string `json:"blockedBy"` // "username" | "ip" | "both"
}

// ── Sentinel errors ───────────────────────────────────────────────────────────

// ErrTokenRevoked is returned when the backend explicitly rejects an agent token
// as invalid or revoked (401 with a body containing "invalid" or "revoked").
// The caller should clear the token from config and restart into pairing mode.
var ErrTokenRevoked = errors.New("agent token revoked or invalid")

// ErrRateLimited is returned when the server returns 429 (Too Many Requests).
// Use AsRateLimit to extract the wait duration and reason.
var ErrRateLimited = errors.New("rate limited")

// RateLimitError carries the parsed backend rate-limit details.
type RateLimitError struct {
	RetryAfter time.Duration
	BlockedBy  string // "username" | "ip" | "both"
	Message    string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited (blocked_by=%s, retry_after=%v): %s",
		e.BlockedBy, e.RetryAfter.Round(time.Second), e.Message)
}

func (e *RateLimitError) Is(target error) bool {
	return target == ErrRateLimited
}

// AsRateLimit extracts a *RateLimitError from err if one is present.
func AsRateLimit(err error) (*RateLimitError, bool) {
	var rle *RateLimitError
	if errors.As(err, &rle) {
		return rle, true
	}
	return nil, false
}

// ── Login ─────────────────────────────────────────────────────────────────────

// Login authenticates the agent against the backend using its agent token
// and returns a JWT for use in subsequent STOMP/WS connections.
func Login() (string, error) {
	cfg := config.GetConfig()
	return loginWithToken(cfg)
}

// loginWithToken authenticates using a pre-issued agent token.
// Calls POST /api/agent/token-login.
func loginWithToken(cfg *config.AgentConfig) (string, error) {
	url := fmt.Sprintf("%s/api/agent/token-login", cfg.BackendURL)

	reqBody := TokenLoginRequest{
		AgentToken: cfg.AgentToken,
		MavID:      cfg.MavID,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewBuffer(data))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through to parse JWT

	case http.StatusUnauthorized:
		// 401 — could be:
		//   a) token explicitly revoked/invalid  → ErrTokenRevoked (clear + re-pair)
		//   b) transient auth issue (clock skew) → transient error (retry with backoff)
		//
		// Distinguish by inspecting the response body. The backend returns a plain
		// string message like "Invalid or revoked agent token" on (a).
		bodyStr := strings.ToLower(string(body))
		if strings.Contains(bodyStr, "invalid") || strings.Contains(bodyStr, "revoked") {
			return "", ErrTokenRevoked
		}
		// Transient — do not clear token, let the caller retry with backoff.
		return "", fmt.Errorf("token login 401 (transient): %s", string(body))

	case http.StatusForbidden:
		// 403 — access denied (e.g. owner lost primary_pilot role). Treat as permanent.
		return "", ErrTokenRevoked

	case http.StatusTooManyRequests:
		return "", parseRateLimitBody(body, resp.Header.Get("Retry-After"))

	default:
		return "", fmt.Errorf("token login failed HTTP %d: %s", resp.StatusCode, string(body))
	}

	var loginResp LoginResponse
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&loginResp); err != nil {
		return "", err
	}
	if loginResp.Token == "" {
		return "", errors.New("token login returned empty JWT")
	}

	return loginResp.Token, nil
}

// parseRateLimitBody tries to extract a structured RateLimitError from the 429 body.
func parseRateLimitBody(body []byte, retryAfterHeader string) *RateLimitError {
	const fallbackWait = 15 * time.Minute

	var parsed rateLimitResponse
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.RetryAfterSeconds > 0 {
		return &RateLimitError{
			RetryAfter: time.Duration(parsed.RetryAfterSeconds) * time.Second,
			BlockedBy:  parsed.BlockedBy,
			Message:    parsed.Message,
		}
	}

	if retryAfterHeader != "" {
		var secs int64
		if _, err := fmt.Sscanf(retryAfterHeader, "%d", &secs); err == nil && secs > 0 {
			return &RateLimitError{
				RetryAfter: time.Duration(secs) * time.Second,
				BlockedBy:  "unknown",
				Message:    "rate limited",
			}
		}
	}

	return &RateLimitError{
		RetryAfter: fallbackWait,
		BlockedBy:  "unknown",
		Message:    "rate limited",
	}
}
