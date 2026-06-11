package stomp

import (
	"testing"

	"github.com/mavsphere/mavsphere-agent-go/pkg/config"
)

// helpers

func sessionWithConfig(cfg *config.AgentConfig) *Session {
	return &Session{cfg: cfg}
}

func sessionWithConfigAndPolicy(cfg *config.AgentConfig, policy ControlPolicy) *Session {
	s := sessionWithConfig(cfg)
	s.setPolicy(policy)
	return s
}

// --- denyReason ---

func TestDenyReason_IneligibleVehicle(t *testing.T) {
	eff := ControlPolicy{AllowControl: true}
	got := denyReason("RC_OVERRIDE", false, false, eff)
	if got != "mavlink-ineligible" {
		t.Errorf("expected mavlink-ineligible, got %q", got)
	}
}

func TestDenyReason_AircraftBlockedByDefault(t *testing.T) {
	// AllowControl=true but AllowAircraftControl=false, vehicle is aircraft
	eff := ControlPolicy{AllowControl: false, AllowAircraftControl: false}
	got := denyReason("RC_OVERRIDE", true, true, eff)
	if got != "aircraft-control-not-allowed" {
		t.Errorf("expected aircraft-control-not-allowed, got %q", got)
	}
}

func TestDenyReason_AllowControlFalse(t *testing.T) {
	eff := ControlPolicy{AllowControl: false}
	got := denyReason("RC_OVERRIDE", true, false, eff)
	if got != "allowControl=false" {
		t.Errorf("expected allowControl=false, got %q", got)
	}
}

func TestDenyReason_RcOverrideNotAllowed(t *testing.T) {
	eff := ControlPolicy{AllowControl: true, AllowRcOverride: false}
	got := denyReason("RC_OVERRIDE", true, false, eff)
	if got != "rc-override-not-allowed" {
		t.Errorf("expected rc-override-not-allowed, got %q", got)
	}
}

func TestDenyReason_GotoNotAllowed(t *testing.T) {
	eff := ControlPolicy{AllowControl: true, AllowGoto: false}
	got := denyReason("GOTO_GLOBAL", true, false, eff)
	if got != "goto-not-allowed" {
		t.Errorf("expected goto-not-allowed, got %q", got)
	}
}

// --- effectiveGate: backend policy can only restrict, never expand ---

func TestEffectiveGate_BackendCannotExpandLocalRestriction(t *testing.T) {
	// Local config denies control. Backend says allow. Result must still deny.
	cfg := &config.AgentConfig{
		AllowControl:         false,
		AllowAircraftControl: false,
	}
	cfg.AllowGoto = boolPtr(false)
	cfg.AllowRcOverride = boolPtr(false)

	backendPolicy := ControlPolicy{
		AllowControl:         true,
		AllowGoto:            true,
		AllowRcOverride:      true,
		AllowAircraftControl: true,
	}

	s := sessionWithConfigAndPolicy(cfg, backendPolicy)
	eff := s.effectiveGate(false)

	if eff.AllowControl {
		t.Error("backend must not expand AllowControl beyond local config")
	}
	if eff.AllowGoto {
		t.Error("backend must not expand AllowGoto beyond local config")
	}
	if eff.AllowRcOverride {
		t.Error("backend must not expand AllowRcOverride beyond local config")
	}
}

func TestEffectiveGate_BackendCanRestrictLocalAllow(t *testing.T) {
	// Local config allows everything. Backend denies. Result must deny.
	cfg := &config.AgentConfig{
		AllowControl:         true,
		AllowAircraftControl: true,
	}
	cfg.AllowGoto = boolPtr(true)
	cfg.AllowRcOverride = boolPtr(true)

	backendPolicy := ControlPolicy{
		AllowControl:         false,
		AllowGoto:            false,
		AllowRcOverride:      false,
		AllowAircraftControl: false,
	}

	s := sessionWithConfigAndPolicy(cfg, backendPolicy)
	eff := s.effectiveGate(false)

	if eff.AllowControl {
		t.Error("backend restriction on AllowControl must be honoured")
	}
	if eff.AllowGoto {
		t.Error("backend restriction on AllowGoto must be honoured")
	}
	if eff.AllowRcOverride {
		t.Error("backend restriction on AllowRcOverride must be honoured")
	}
}

func TestEffectiveGate_AircraftBlockedWhenNotExplicitlyAllowed(t *testing.T) {
	// Both local config and backend allow general control,
	// but AllowAircraftControl is false — aircraft must be blocked.
	cfg := &config.AgentConfig{
		AllowControl:         true,
		AllowAircraftControl: false,
	}
	cfg.AllowGoto = boolPtr(true)
	cfg.AllowRcOverride = boolPtr(true)

	backendPolicy := ControlPolicy{
		AllowControl:         true,
		AllowAircraftControl: false,
	}

	s := sessionWithConfigAndPolicy(cfg, backendPolicy)
	eff := s.effectiveGate(true /* isAircraft */)

	if eff.AllowControl {
		t.Error("aircraft must be blocked when AllowAircraftControl=false")
	}
}

func TestEffectiveGate_NoPolicySeen_LocalConfigOnly(t *testing.T) {
	// No backend policy received yet — local config should be the gate.
	cfg := &config.AgentConfig{
		AllowControl: true,
	}
	cfg.AllowGoto = boolPtr(true)
	cfg.AllowRcOverride = boolPtr(false)

	s := sessionWithConfig(cfg) // no setPolicy called
	eff := s.effectiveGate(false)

	if !eff.AllowControl {
		t.Error("AllowControl should be true from local config when no backend policy seen")
	}
	if eff.AllowRcOverride {
		t.Error("AllowRcOverride should be false from local config")
	}
}

// --- policyEqual ---

func TestPolicyEqual_SamePoliciesAreEqual(t *testing.T) {
	a := ControlPolicy{AllowControl: true, AllowGoto: true, AllowRcOverride: false, AllowAircraftControl: false}
	b := ControlPolicy{AllowControl: true, AllowGoto: true, AllowRcOverride: false, AllowAircraftControl: false}
	if !policyEqual(a, b) {
		t.Error("identical policies should be equal")
	}
}

func TestPolicyEqual_DifferentPoliciesAreNotEqual(t *testing.T) {
	a := ControlPolicy{AllowControl: true}
	b := ControlPolicy{AllowControl: false}
	if policyEqual(a, b) {
		t.Error("different policies should not be equal")
	}
}
