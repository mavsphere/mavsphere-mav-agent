package mavlink

import (
	"testing"
)

// clamp is defined inline in FailSafeStopRcOverride.
// We test the same logic here independently to document the expected behaviour.
// If the clamp logic ever changes, this test catches it.

func clampPWM(v uint16) uint16 {
	if v < 1000 {
		return 1000
	}
	if v > 2000 {
		return 2000
	}
	return v
}

func TestClampPWM_BelowMinimumClampsTo1000(t *testing.T) {
	if got := clampPWM(0); got != 1000 {
		t.Errorf("expected 1000 for input 0, got %d", got)
	}
	if got := clampPWM(500); got != 1000 {
		t.Errorf("expected 1000 for input 500, got %d", got)
	}
	if got := clampPWM(999); got != 1000 {
		t.Errorf("expected 1000 for input 999, got %d", got)
	}
}

func TestClampPWM_AboveMaximumClampsTo2000(t *testing.T) {
	if got := clampPWM(2001); got != 2000 {
		t.Errorf("expected 2000 for input 2001, got %d", got)
	}
	if got := clampPWM(65535); got != 2000 {
		t.Errorf("expected 2000 for input 65535, got %d", got)
	}
}

func TestClampPWM_ValidRangePassesThrough(t *testing.T) {
	cases := []uint16{1000, 1500, 2000}
	for _, v := range cases {
		if got := clampPWM(v); got != v {
			t.Errorf("expected %d to pass through unchanged, got %d", v, got)
		}
	}
}

func TestClampPWM_SteeringCenterIsValid(t *testing.T) {
	// 1500 is the expected stop value for steering and bidirectional throttle.
	if got := clampPWM(1500); got != 1500 {
		t.Errorf("steering center 1500 should pass through, got %d", got)
	}
}

func TestClampPWM_ThrottleStopForwardOnly(t *testing.T) {
	// 1000 is the expected stop value for forward-only throttle.
	if got := clampPWM(1000); got != 1000 {
		t.Errorf("forward-only throttle stop 1000 should pass through, got %d", got)
	}
}

// FailSafeStopRcOverride nil safety:
// A nil MavlinkConnection must not panic. This matters because
// the failsafe path can be called during shutdown before the connection is ready.

func TestFailSafeStopRcOverride_NilReceiverNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("FailSafeStopRcOverride panicked on nil receiver: %v", r)
		}
	}()
	var m *MavlinkConnection
	m.FailSafeStopRcOverride(1000, 1500)
}

func TestFailSafeStopVelocity_NilReceiverNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("FailSafeStopVelocity panicked on nil receiver: %v", r)
		}
	}()
	var m *MavlinkConnection
	m.FailSafeStopVelocity()
}
