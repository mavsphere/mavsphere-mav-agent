package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config*.json")
	if err != nil {
		t.Fatalf("could not create temp config: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("could not write temp config: %v", err)
	}
	f.Close()
	return f.Name()
}

// --- defaults ---

func TestLoadConfig_AircraftControlDefaultsFalse(t *testing.T) {
	// AllowAircraftControl must default to false even when allowControl=true.
	// This is the most safety-critical default in the config.
	path := writeConfig(t, `{
		"mavId": "1",
		"backendWsUrl": "wss://mavsphere.com/api/ws/agent",
		"backendUrl": "https://mavsphere.com",
		"janusUrl": "wss://mavsphere.com/janus",
		"username": "test@example.com",
		"password": "test",
		"mavlinkConnection": "udp:0.0.0.0:14600",
		"allowControl": true
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.AllowAircraftControl {
		t.Error("AllowAircraftControl must default to false — aircraft control must be explicitly opted in")
	}
}

func TestLoadConfig_RcOverrideDefaultsFalse(t *testing.T) {
	// AllowRcOverride must default to false (opt-in for safety).
	path := writeConfig(t, `{
		"mavId": "1",
		"backendWsUrl": "wss://mavsphere.com/api/ws/agent",
		"backendUrl": "https://mavsphere.com",
		"janusUrl": "wss://mavsphere.com/janus",
		"username": "test@example.com",
		"password": "test",
		"mavlinkConnection": "udp:0.0.0.0:14600",
		"allowControl": true
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.AllowRcOverrideEffective() {
		t.Error("AllowRcOverride must default to false — RC override must be explicitly opted in")
	}
}

func TestLoadConfig_AgentGcsIDDefault(t *testing.T) {
	path := writeConfig(t, `{
		"mavId": "1",
		"backendWsUrl": "wss://mavsphere.com/api/ws/agent",
		"backendUrl": "https://mavsphere.com",
		"janusUrl": "wss://mavsphere.com/janus",
		"username": "test@example.com",
		"password": "test"
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.AgentGcsID != 255 {
		t.Errorf("AgentGcsID default should be 255, got %d", cfg.AgentGcsID)
	}
}

func TestLoadConfig_MavIDDefault(t *testing.T) {
	path := writeConfig(t, `{
		"backendWsUrl": "wss://mavsphere.com/api/ws/agent",
		"backendUrl": "https://mavsphere.com",
		"janusUrl": "wss://mavsphere.com/janus",
		"username": "test@example.com",
		"password": "test"
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.MavID != "1" {
		t.Errorf("MavID default should be \"1\", got %q", cfg.MavID)
	}
}

func TestLoadConfig_ExplicitValuesNotOverriddenByDefaults(t *testing.T) {
	path := writeConfig(t, `{
		"mavId": "7",
		"backendWsUrl": "wss://mavsphere.com/api/ws/agent",
		"backendUrl": "https://mavsphere.com",
		"janusUrl": "wss://mavsphere.com/janus",
		"username": "test@example.com",
		"password": "test",
		"agentGcsId": 252,
		"allowControl": true,
		"allowRcOverride": true,
		"allowAircraftControl": true
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.MavID != "7" {
		t.Errorf("explicit mavId should not be overridden, got %q", cfg.MavID)
	}
	if cfg.AgentGcsID != 252 {
		t.Errorf("explicit agentGcsId should not be overridden, got %d", cfg.AgentGcsID)
	}
	if !cfg.AllowRcOverrideEffective() {
		t.Error("explicit allowRcOverride=true should be respected")
	}
	if !cfg.AllowAircraftControl {
		t.Error("explicit allowAircraftControl=true should be respected")
	}
}

func TestLoadConfig_InvalidJSON(t *testing.T) {
	path := writeConfig(t, `this is not json`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

// --- AllowGotoEffective / AllowRcOverrideEffective nil safety ---

func TestAllowGotoEffective_NilConfigReturnsTrue(t *testing.T) {
	var cfg *AgentConfig
	if !cfg.AllowGotoEffective() {
		t.Error("nil config AllowGotoEffective() should return true (safe default)")
	}
}

func TestAllowRcOverrideEffective_NilConfigReturnsFalse(t *testing.T) {
	var cfg *AgentConfig
	if cfg.AllowRcOverrideEffective() {
		t.Error("nil config AllowRcOverrideEffective() should return false (safe default)")
	}
}
