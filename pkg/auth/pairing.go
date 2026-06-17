// Package auth — pairing.go
//
// Implements the agent side of the pairing flow for mav-agent.
// See layout-agent pairing.go for full documentation; this is the same
// logic with mavId instead of layoutId in the config write path.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/mavsphere/mavsphere-agent-go/pkg/config"
)

const (
	pairPollInterval = 3 * time.Second
	pairCodeChars    = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	pairCodeLen      = 6
)

type pairResult struct {
	AgentToken   string `json:"agentToken"`
	ResourceType string `json:"resourceType"`
	ResourceID   int64  `json:"resourceId"`
}

var (
	pairMu   sync.RWMutex
	pairCode string
)

// GetPairingCode returns the current pairing code, or "" if not in pairing mode.
func GetPairingCode() string {
	pairMu.RLock()
	defer pairMu.RUnlock()
	return pairCode
}

func setPairingCode(code string) {
	pairMu.Lock()
	pairCode = code
	pairMu.Unlock()
}

func generateCode() string {
	b := make([]byte, pairCodeLen)
	for i := range b {
		b[i] = pairCodeChars[rand.Intn(len(pairCodeChars))]
	}
	return string(b)
}

// RunPairingLoop generates a pairing code, polls the backend until the operator
// confirms, writes the result into config.json, and then returns.
func RunPairingLoop(ctx context.Context, cfgPath string) error {
	cfg := config.GetConfig()
	if cfg == nil {
		return fmt.Errorf("config not loaded")
	}

	code := generateCode()
	setPairingCode(code)
	defer setPairingCode("")

	pollURL := fmt.Sprintf("%s/api/agent/pair/%s", cfg.BackendURL, code)

	log.Printf("════════════════════════════════════════════════════════")
	log.Printf(" AGENT PAIRING REQUIRED")
	log.Printf(" ")
	log.Printf(" Go to the MavSphere UI → Manage MAVs → Agent Tokens")
	log.Printf(" and enter this pairing code:")
	log.Printf(" ")
	log.Printf("         %s", code)
	log.Printf(" ")
	log.Printf(" Or open the agent web UI (http://<this-device>:8090)")
	log.Printf(" for the same code with a QR-friendly display.")
	log.Printf(" ")
	log.Printf(" Waiting for operator to confirm pairing…")
	log.Printf("════════════════════════════════════════════════════════")

	client := &http.Client{Timeout: 10 * time.Second}
	ticker := time.NewTicker(pairPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			result, err := pollOnce(client, pollURL)
			if err != nil {
				log.Printf("[pair] poll error: %v", err)
				continue
			}
			if result == nil {
				continue
			}

			log.Printf("[pair] pairing confirmed! type=%s id=%d", result.ResourceType, result.ResourceID)

			cfg := config.GetConfig()
			cfg.AgentToken = result.AgentToken
			cfg.MavID = fmt.Sprintf("%d", result.ResourceID)

			// Use the unvalidated save path here: pairing must never be blocked by an
			// unrelated, already-on-disk setting (e.g. a stale h264BitrateBps or video
			// resolution) failing SaveConfig's strict validation. The only fields this
			// flow needs to persist reliably are the identity/auth ones set above.
			if err := config.SaveConfigUnvalidated(cfgPath, cfg); err != nil {
				return fmt.Errorf("pair: failed to save config: %w", err)
			}

			log.Printf("[pair] config saved — agent is now paired to mavId=%s", cfg.MavID)
			return nil
		}
	}
}

func pollOnce(client *http.Client, url string) (*pairResult, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}
		var result pairResult
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("parse pair result: %w", err)
		}
		return &result, nil
	default:
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}
}
