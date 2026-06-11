package mavlink

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gomavlib/v3"
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/common"
)

type StatusMessage struct {
	Severity  uint8  `json:"severity"`
	Text      string `json:"text"`
	Timestamp int64  `json:"timestamp"`
}

type GPSInfo struct {
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
	Alt  float64 `json:"alt"`
	Sats int     `json:"sats"`

	HDOP float64 `json:"hdop"`
}

type BatteryInfo struct {
	Voltage   float64 `json:"voltage"`
	Remaining float64 `json:"remaining"`
}

type TelemetryData struct {
	Attitude struct {
		Roll  float64 `json:"roll"`
		Pitch float64 `json:"pitch"`
		Yaw   float64 `json:"yaw"`
	} `json:"attitude"`
	GPS             GPSInfo         `json:"gps"`
	Battery         BatteryInfo     `json:"battery"`
	Armed           bool            `json:"armed"`
	Mode            string          `json:"mode"`
	Heading         float64         `json:"heading"`
	GroundSpeed     float64         `json:"ground_speed"`
	VerticalSpeed   float64         `json:"verticalSpeed"`
	RCThrottle      uint16          `json:"rcThrottle"`
	Thrust          float64         `json:"thrust"`                 // 0.0–1.0 (actual/best estimate)
	ThrustSource    string          `json:"thrustSource,omitempty"` // "vfr_hud" | "servo_outputs" | "commanded" | "rc_pwm" | "unknown"
	CommandedThrust float64         `json:"commandedThrust,omitempty"`
	StatusMessages  []StatusMessage `json:"statusMessages"`
	Timestamp       int64           `json:"timestamp"`
	VehicleType     string          `json:"vehicleType,omitempty"`
	VehicleTypeRaw  uint8           `json:"vehicleTypeRaw,omitempty"`
}

type MavlinkConnection struct {
	Node         *gomavlib.Node
	mu           sync.RWMutex
	telemetry    TelemetryData
	sysID        byte
	compID       byte
	vehicleType  uint8 // MAV_TYPE_* from heartbeat
	vfrHUD       *common.MessageVfrHud
	servoOut     *common.MessageServoOutputRaw
	vx, vy       int16
	maxStatusLen int

	outSysID      byte // our own sysid (GCS/agent)
	vehicleSysID  byte // sysid of the autopilot we're tracking
	vehicleCompID byte
	vehicleLocked bool

	lastMsg      time.Time
	lastHB       time.Time
	firstHB      sync.Once
	endpointDesc string

	// Debug/log rate-limiting
	lastVfrLog     time.Time
	lastThrust     float64
	lastEligLog    time.Time
	lastCtrlLog    time.Time
	lastLoggedCmdT float64 // last logged ATTITUDE commanded thrust

	paramWait map[string]chan *common.MessageParamValue

	// ---- Mount/gimbal control state ----
	mountConfigured bool
	lastMountCfg    time.Time

	// ---- Slew-limited gimbal state (degrees) ----
	gimbalActive        bool
	gimbalTargetPanDeg  float64
	gimbalTargetTiltDeg float64
	gimbalCurPanDeg     float64
	gimbalCurTiltDeg    float64
	gimbalLastUpdate    time.Time
	gimbalLastCmd       time.Time
	gimbalLoopOnce      sync.Once

	// --- Rover servo mapping (for thrust inference) ---
	roverServoMapDone     atomic.Bool
	roverServoMapInFlight atomic.Bool
	roverThrottleIdx      atomic.Int32 // SERVO output channel index (1..16) for function 70, or 0 if not found
	roverThrottleLeftIdx  atomic.Int32 // function 73
	roverThrottleRightIdx atomic.Int32 // function 74

}

// -------------------------
// GIMBAL TUNING CONSTANTS
// -------------------------

const (
	// Background loop rate (how often we emit DO_MOUNT_CONTROL)
	GIMBAL_LOOP_HZ = 20

	// Slew limits (deg/sec). Increase for faster response, decrease for smoother.
	GIMBAL_PAN_RATE_DEG_PER_SEC  = 220.0
	GIMBAL_TILT_RATE_DEG_PER_SEC = 140.0

	// If UI stops sending updates for this long, auto-target center (0,0).
	GIMBAL_RECENTER_AFTER = 350 * time.Millisecond

	// Snap very near center to exactly 0 (reduces “never quite settles”)
	GIMBAL_CENTER_SNAP_DEG = 0.5

	// If we haven’t been updated in a long time, clamp dt to avoid a big jump
	GIMBAL_MAX_DT = 250 * time.Millisecond
)

func InitMavlink(endpointStr string, outSysID byte) (*MavlinkConnection, error) {
	var (
		ep       gomavlib.EndpointConf
		epDesc   string
		warnHint string
	)

	if strings.HasPrefix(endpointStr, "udp:") {
		addr := strings.TrimPrefix(endpointStr, "udp:")
		ep = gomavlib.EndpointUDPServer{Address: addr}
		epDesc = "UDP server (listen) at " + addr
		warnHint = "If using MAVProxy, run: --out=udp:HOST_OF_AGENT:PORT"
	} else if strings.HasPrefix(endpointStr, "udpclient:") {
		addr := strings.TrimPrefix(endpointStr, "udpclient:")
		ep = gomavlib.EndpointUDPClient{Address: addr}
		epDesc = "UDP client (dial) to " + addr
		warnHint = "This expects a UDP server on the other end."
	} else if strings.HasPrefix(endpointStr, "tcpclient:") {
		addr := strings.TrimPrefix(endpointStr, "tcpclient:")
		ep = gomavlib.EndpointTCPClient{Address: addr}
		epDesc = "TCP client (dial) to " + addr
		warnHint = "Ensure SITL/FC is listening there."
	} else if strings.HasPrefix(endpointStr, "serial:") {
		parts := strings.Split(strings.TrimPrefix(endpointStr, "serial:"), ":")
		if len(parts) < 1 {
			return nil, fmt.Errorf("invalid serial format (want serial:/dev/ttyX[:BAUD])")
		}
		device := parts[0]
		baud := 57600
		if len(parts) >= 2 {
			fmt.Sscanf(parts[1], "%d", &baud)
		}
		ep = gomavlib.EndpointSerial{Device: device, Baud: baud}
		epDesc = fmt.Sprintf("Serial %s @ %d", device, baud)
		warnHint = "Ensure permissions and baud rate are correct."
	} else {
		return nil, fmt.Errorf("unsupported endpoint type: %s", endpointStr)
	}

	log.Printf("[MAVLink] Initializing on %s", epDesc)

	node, err := gomavlib.NewNode(gomavlib.NodeConf{
		Endpoints:   []gomavlib.EndpointConf{ep},
		Dialect:     common.Dialect,
		OutVersion:  gomavlib.V2,
		OutSystemID: outSysID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create node (%s): %w", epDesc, err)
	}

	log.Printf("[MAVLink] Started on %s; waiting for heartbeat…", epDesc)
	if warnHint != "" {
		log.Printf("[MAVLink] Hint: %s", warnHint)
	}

	conn := &MavlinkConnection{
		Node:         node,
		outSysID:     outSysID,
		telemetry:    TelemetryData{},
		maxStatusLen: 20,
		endpointDesc: epDesc,
		paramWait:    make(map[string]chan *common.MessageParamValue),
	}

	// Warn if no heartbeat after 10s
	go func(desc string) {
		time.Sleep(10 * time.Second)
		conn.mu.Lock()
		seen := conn.sysID != 0
		conn.mu.Unlock()
		if !seen {
			log.Printf("⚠️  [MAVLink] No heartbeat received yet on %s. Troubleshoot:", desc)
			log.Printf("   - If using MAVProxy on SAME host: --out=udp:127.0.0.1:14551  (and agent on udp:127.0.0.1:14551)")
			log.Printf("   - If on LAN:  MAVProxy --out=udp:AGENT_IP:14551   and agent udp:0.0.0.0:14551")
			log.Printf("   - Check: ss -lunp | grep 14551   (agent should LISTEN)")
			log.Printf("   - Check packets: sudo tcpdump -ni any udp port 14551")
		}
	}(epDesc)

	go conn.readMessages()

	// Background gimbal loop (only does work when gimbalActive is true)
	conn.startGimbalLoop()

	return conn, nil
}

func (m *MavlinkConnection) readMessages() {
	for evt := range m.Node.Events() {
		if frm, ok := evt.(*gomavlib.EventFrame); ok {
			m.mu.Lock()
			m.sysID = frm.SystemID()
			m.compID = frm.ComponentID()
			m.lastMsg = time.Now()
			m.mu.Unlock()
			m.handleMessage(frm.Message())
		}
	}
}

func (m *MavlinkConnection) handleMessage(msg any) {
	// Keep lock hold-times extremely short. Never do I/O (logging, network, param probes)
	// while holding the mutex.
	var (
		logVfr          bool
		vfrThrottle     uint16
		vfrThrust       float64
		vfrClimb        float32
		logFirstHB      bool
		firstHBsys      byte
		firstHBcomp     byte
		firstHBmode     string
		firstHBarmed    bool
		triggerRoverMap bool
	)

	switch t := msg.(type) {
	case *common.MessageAttitude:
		m.mu.Lock()
		m.telemetry.Attitude.Roll = float64(t.Roll)
		m.telemetry.Attitude.Pitch = float64(t.Pitch)
		m.telemetry.Attitude.Yaw = float64(t.Yaw)
		m.mu.Unlock()

	case *common.MessageGlobalPositionInt:
		m.mu.Lock()
		m.telemetry.GPS.Lat = float64(t.Lat) / 1e7
		m.telemetry.GPS.Lon = float64(t.Lon) / 1e7
		// Prefer relative altitude when available (better when GPS coords are withheld and/or GPS fix is weak).
		// Fall back to MSL altitude.
		if t.RelativeAlt != 0 {
			m.telemetry.GPS.Alt = float64(t.RelativeAlt) / 1000.0
		} else {
			m.telemetry.GPS.Alt = float64(t.Alt) / 1000.0
		}
		m.telemetry.Heading = float64(t.Hdg) / 100.0
		m.vx = t.Vx
		m.vy = t.Vy
		m.mu.Unlock()

	case *common.MessageGpsRawInt:
		m.mu.Lock()
		m.telemetry.GPS.Sats = int(t.SatellitesVisible)
		m.telemetry.GPS.HDOP = float64(t.Eph) / 100.0
		m.mu.Unlock()

	case *common.MessageSysStatus:
		m.mu.Lock()
		m.telemetry.Battery.Voltage = float64(t.VoltageBattery) / 1000.0
		m.telemetry.Battery.Remaining = float64(t.BatteryRemaining)
		m.mu.Unlock()

	case *common.MessageHeartbeat:
		const MAV_MODE_FLAG_SAFETY_ARMED = 128

		// We'll compute these under lock, then log/trigger work after unlock.
		var (
			didLock        bool
			lockedSys      byte
			lockedComp     byte
			lockedTypeName string
			doFirstHBLog   bool
			firstSys       byte
			firstComp      byte
			firstMode      string
			firstArmed     bool
			doRoverMap     bool
		)

		m.mu.Lock()

		// Ignore our own sysid if it loops back
		if m.sysID == m.outSysID {
			m.mu.Unlock()
			return
		}

		// Lock onto the autopilot heartbeat once
		if !m.vehicleLocked {
			if t.Autopilot == common.MAV_AUTOPILOT_ARDUPILOTMEGA && t.Type != common.MAV_TYPE_GCS {
				m.vehicleSysID = m.sysID
				m.vehicleCompID = m.compID
				m.vehicleLocked = true

				didLock = true
				lockedSys = m.vehicleSysID
				lockedComp = m.vehicleCompID
				lockedTypeName = mavTypeName(uint8(t.Type))
			}
		}

		// If locked, ignore other systems' heartbeats
		if m.vehicleLocked && m.sysID != m.vehicleSysID {
			m.mu.Unlock()
			return
		}

		// Authoritative vehicle state from HEARTBEAT
		m.telemetry.Armed = (t.BaseMode & MAV_MODE_FLAG_SAFETY_ARMED) != 0
		m.vehicleType = uint8(t.Type)
		m.telemetry.Mode = mavModeName(t.CustomMode, m.vehicleType)
		m.telemetry.VehicleType = mavTypeName(m.vehicleType)
		m.telemetry.VehicleTypeRaw = m.vehicleType
		m.lastHB = time.Now()

		// First HB side-effects (log once + request intervals + rover mapping)
		m.firstHB.Do(func() {
			doFirstHBLog = true
			firstSys = m.sysID
			firstComp = m.compID
			firstMode = m.telemetry.Mode
			firstArmed = m.telemetry.Armed
			doRoverMap = (m.vehicleType == uint8(common.MAV_TYPE_GROUND_ROVER) ||
				m.vehicleType == uint8(common.MAV_TYPE_SURFACE_BOAT))
		})

		m.mu.Unlock()

		if didLock {
			log.Printf("🔒 [MAVLink] Locked vehicle heartbeat to sysid=%d compid=%d type=%s",
				lockedSys, lockedComp, lockedTypeName)
		}
		if doFirstHBLog {
			log.Printf("[MAVLink] ✅ Heartbeat seen (sys=%d comp=%d) Mode=%s Armed=%v",
				firstSys, firstComp, firstMode, firstArmed)
			go m.requestMessageIntervals()
			if doRoverMap {
				m.ensureRoverServoMapAsync()
			}
		}

	case *common.MessageRcChannels:
		m.mu.Lock()
		m.telemetry.RCThrottle = t.Chan3Raw
		m.mu.Unlock()

	case *common.MessageServoOutputRaw:
		m.mu.Lock()
		m.servoOut = t
		m.mu.Unlock()

	case *common.MessageVfrHud:
		m.mu.Lock()
		m.telemetry.VerticalSpeed = float64(t.Climb)
		// Preferred actual thrust if firmware populates it (0..100 → 0..1)
		m.telemetry.Thrust = float64(t.Throttle) / 100.0
		m.vfrHUD = t

		now := time.Now()
		if math.Abs(m.telemetry.Thrust-m.lastThrust) >= 0.02 || now.Sub(m.lastVfrLog) >= time.Second {
			logVfr = true
			vfrThrottle = t.Throttle
			vfrThrust = m.telemetry.Thrust
			vfrClimb = t.Climb
			m.lastThrust = m.telemetry.Thrust
			m.lastVfrLog = now
		}
		m.mu.Unlock()

	case *common.MessageStatustext:
		m.mu.Lock()
		m.appendStatus(uint8(t.Severity), t.Text)
		m.mu.Unlock()

	case *common.MessageParamValue:
		name := strings.TrimRight(t.ParamId, "\x00")
		up := strings.ToUpper(name)
		m.mu.RLock()
		ch, ok := m.paramWait[up]
		m.mu.RUnlock()
		if ok {
			select {
			case ch <- t:
			default:
			}
		}
	}

	// Logs and slow follow-ups happen after the update, without holding m.mu.
	if logVfr {
		log.Printf("[VFR_HUD] throttle=%3d -> thrust=%.2f climb=%.2f", vfrThrottle, vfrThrust, vfrClimb)
	}
	if logFirstHB {
		log.Printf("[MAVLink] ✅ Heartbeat seen (sys=%d comp=%d) Mode=%s Armed=%v", firstHBsys, firstHBcomp, firstHBmode, firstHBarmed)
		go m.requestMessageIntervals()
		if triggerRoverMap {
			m.ensureRoverServoMapAsync()
		}
	}
}

func (m *MavlinkConnection) appendStatus(sev uint8, text string) {
	msg := StatusMessage{
		Severity:  sev,
		Text:      text,
		Timestamp: time.Now().UnixMilli(),
	}
	m.telemetry.StatusMessages = append(m.telemetry.StatusMessages, msg)
	if len(m.telemetry.StatusMessages) > m.maxStatusLen {
		m.telemetry.StatusMessages = m.telemetry.StatusMessages[1:]
	}
}

func pwmToNorm(pwm uint16) float64 {
	if pwm <= 1000 {
		return 0
	}
	if pwm >= 2000 {
		return 1
	}
	return (float64(pwm) - 1000.0) / 1000.0
}

func (m *MavlinkConnection) inferThrustFromServos() (float64, bool) {
	if m.servoOut == nil {
		return 0, false
	}
	// Heuristic: first 4 servos are motors for copter frames
	vals := []uint16{
		m.servoOut.Servo1Raw, m.servoOut.Servo2Raw,
		m.servoOut.Servo3Raw, m.servoOut.Servo4Raw,
	}
	n := 0
	sum := 0.0
	for _, v := range vals {
		if v != 0 {
			sum += pwmToNorm(v)
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	avg := sum / float64(n)
	if avg < 0 {
		avg = 0
	}
	if avg > 1 {
		avg = 1
	}
	return avg, true
}

// Choose thrust value with an explicit source label.
func (m *MavlinkConnection) thrustWithSourceLocked() (float64, string) {
	isRoverLike := m.vehicleType == uint8(common.MAV_TYPE_GROUND_ROVER) ||
		m.vehicleType == uint8(common.MAV_TYPE_SURFACE_BOAT)

	// Rover: VFR_HUD throttle is often unhelpful; prefer servo mapping.
	if isRoverLike {
		if s, ok := m.inferRoverThrustFromServos(); ok {
			return s, "servo_outputs_rover"
		}
	}

	// 1) Prefer VFR_HUD if clearly populated (>1%)
	if m.telemetry.Thrust > 0.01 {
		return m.telemetry.Thrust, "vfr_hud"
	}

	// 2) Try original copter heuristic
	if s, ok := m.inferThrustFromServos(); ok {
		return s, "servo_outputs"
	}

	// 3) Fall back to last commanded
	if m.telemetry.CommandedThrust > 0 {
		return m.telemetry.CommandedThrust, "commanded"
	}

	// 4) As a last resort, map RC PWM to 0..1
	if m.telemetry.RCThrottle > 0 {
		t := (float64(m.telemetry.RCThrottle) - 1000.0) / 1000.0
		if t < 0 {
			t = 0
		}
		if t > 1 {
			t = 1
		}
		return t, "rc_pwm"
	}

	return 0, "none"
}

func clean(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return x
}

func (m *MavlinkConnection) GetTelemetryUpdate(_ string) TelemetryData {
	// Never block telemetry on param probing. If rover mapping is needed,
	// kick it off asynchronously before we take the main lock.
	m.ensureRoverServoMapAsync()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Ground speed
	speed := 0.0
	if m.vx != 0 || m.vy != 0 {
		speed = math.Sqrt(float64(m.vx*m.vx+m.vy*m.vy)) / 100.0
	} else if m.vfrHUD != nil {
		speed = float64(m.vfrHUD.Groundspeed)
	}
	m.telemetry.GroundSpeed = clean(speed)

	// Thrust + source
	th, src := m.thrustWithSourceLocked()
	m.telemetry.Thrust = clean(th)
	m.telemetry.ThrustSource = src

	// Other possibly-uninitialized floats
	m.telemetry.VerticalSpeed = clean(m.telemetry.VerticalSpeed)
	m.telemetry.Heading = clean(m.telemetry.Heading)
	m.telemetry.CommandedThrust = clean(m.telemetry.CommandedThrust)

	m.telemetry.Timestamp = time.Now().UnixMilli()
	return m.telemetry
}

// Ask the FC to regularly send VFR_HUD, RC_CHANNELS and SERVO_OUTPUT_RAW
// so we always have thrust and rcThrottle.
func (m *MavlinkConnection) requestMessageIntervals() {
	m.mu.Lock()
	sys := m.sysID
	comp := m.compID
	m.mu.Unlock()

	if sys == 0 {
		return // no target yet
	}

	const (
		msgIDVfrHud     = 74 // VFR_HUD
		msgIDRcChannels = 65 // RC_CHANNELS
		msgIDServoOut   = 36 // SERVO_OUTPUT_RAW
	)

	set := func(msgID uint32, hz float64) {
		if hz <= 0 {
			return
		}
		intervalUs := int64(1e6 / hz) // microseconds per MAV_CMD_SET_MESSAGE_INTERVAL
		_ = m.Node.WriteMessageAll(&common.MessageCommandLong{
			TargetSystem:    sys,
			TargetComponent: comp,
			Command:         common.MAV_CMD_SET_MESSAGE_INTERVAL,
			Param1:          float32(msgID),
			Param2:          float32(intervalUs),
		})
	}

	set(uint32(msgIDVfrHud), 10)
	set(uint32(msgIDRcChannels), 10)
	set(uint32(msgIDServoOut), 10)
}

// Ensure the mount is in MAVLink targeting mode before sending DO_MOUNT_CONTROL.
// Mission Planner typically does this; without it, DO_MOUNT_CONTROL often has no effect.
func (m *MavlinkConnection) ensureMountMavlinkTargeting() {
	// Refresh periodically in case FC resets mount mode.
	const refreshEvery = 10 * time.Second

	now := time.Now()

	m.mu.Lock()
	sys := m.sysID
	comp := m.compID
	need := !m.mountConfigured || now.Sub(m.lastMountCfg) > refreshEvery
	m.mu.Unlock()

	if sys == 0 || !need {
		return
	}

	// MAV_MOUNT_MODE_MAVLINK_TARGETING is 2 in MAVLink common.
	const mavlinkTargetingMode = 2.0

	_ = m.Node.WriteMessageAll(&common.MessageCommandLong{
		TargetSystem:    sys,
		TargetComponent: comp,
		Command:         common.MAV_CMD_DO_MOUNT_CONFIGURE,
		Param1:          float32(mavlinkTargetingMode), // mount mode
		Param2:          0,                             // stabilize roll
		Param3:          0,                             // stabilize pitch
		Param4:          0,                             // stabilize yaw
		Param5:          0,
		Param6:          0,
		Param7:          0,
	})

	m.mu.Lock()
	m.mountConfigured = true
	m.lastMountCfg = now
	m.mu.Unlock()

	log.Printf("[MAVLink] Mount configured for MAVLink targeting (DO_MOUNT_CONFIGURE mode=2)")
}

func slewDeg(cur, tgt, maxDegPerSec, dtSec float64) float64 {
	maxStep := maxDegPerSec * dtSec
	delta := tgt - cur
	if delta > maxStep {
		return cur + maxStep
	}
	if delta < -maxStep {
		return cur - maxStep
	}
	return tgt
}

func (m *MavlinkConnection) startGimbalLoop() {
	m.gimbalLoopOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Second / time.Duration(GIMBAL_LOOP_HZ))
			defer ticker.Stop()

			for range ticker.C {
				// If the node is closed, Events() will close and loop will stop naturally,
				// but we also guard against nil.
				if m == nil || m.Node == nil {
					return
				}
				m.driveGimbalStep()
			}
		}()
	})
}

func (m *MavlinkConnection) driveGimbalStep() {
	// Fast path: if we never used gimbal, do nothing.
	m.mu.Lock()
	active := m.gimbalActive
	sys := m.sysID
	comp := m.compID
	targetPan := m.gimbalTargetPanDeg
	targetTilt := m.gimbalTargetTiltDeg
	curPan := m.gimbalCurPanDeg
	curTilt := m.gimbalCurTiltDeg
	lastUpd := m.gimbalLastUpdate
	lastCmd := m.gimbalLastCmd
	m.mu.Unlock()

	if !active || sys == 0 {
		return
	}

	// If UI has gone quiet, auto-center.
	now := time.Now()
	if !lastCmd.IsZero() && now.Sub(lastCmd) > GIMBAL_RECENTER_AFTER {
		targetPan = 0
		targetTilt = 0
	}

	// dt
	dt := 1.0 / float64(GIMBAL_LOOP_HZ)
	if !lastUpd.IsZero() {
		d := now.Sub(lastUpd)
		if d < 0 {
			d = 0
		}
		if d > GIMBAL_MAX_DT {
			d = GIMBAL_MAX_DT
		}
		dt = d.Seconds()
	}

	// Slew
	curPan = slewDeg(curPan, targetPan, GIMBAL_PAN_RATE_DEG_PER_SEC, dt)
	curTilt = slewDeg(curTilt, targetTilt, GIMBAL_TILT_RATE_DEG_PER_SEC, dt)

	// Snap near center (helps settle)
	if math.Abs(curPan) < GIMBAL_CENTER_SNAP_DEG {
		curPan = 0
	}
	if math.Abs(curTilt) < GIMBAL_CENTER_SNAP_DEG {
		curTilt = 0
	}

	// Save state
	m.mu.Lock()
	m.gimbalCurPanDeg = curPan
	m.gimbalCurTiltDeg = curTilt
	m.gimbalLastUpdate = now

	// If we’re fully settled at center and UI is quiet, we can deactivate to stop sending.
	quiet := !m.gimbalLastCmd.IsZero() && now.Sub(m.gimbalLastCmd) > GIMBAL_RECENTER_AFTER
	if quiet && curPan == 0 && curTilt == 0 {
		m.gimbalActive = false
	}
	m.mu.Unlock()

	// Ensure mount mode
	m.ensureMountMavlinkTargeting()

	// Emit DO_MOUNT_CONTROL with MAVLink targeting mode
	const mavlinkTargetingMode = 2.0
	_ = m.Node.WriteMessageAll(&common.MessageCommandLong{
		TargetSystem:    sys,
		TargetComponent: comp,
		Command:         common.MAV_CMD_DO_MOUNT_CONTROL,
		Param1:          float32(curTilt), // pitch
		Param2:          0,                // roll
		Param3:          float32(curPan),  // yaw
		Param4:          0,
		Param5:          0,
		Param6:          0,
		Param7:          float32(mavlinkTargetingMode),
	})
}

func (m *MavlinkConnection) ApplyControl(ctrlType string, data map[string]interface{}) {
	if m == nil || m.Node == nil {
		return
	}
	// Snapshot target IDs once to avoid data races.
	m.mu.RLock()
	sys := m.sysID
	comp := m.compID
	modeSnap := m.telemetry.Mode
	armedSnap := m.telemetry.Armed
	m.mu.RUnlock()
	if sys == 0 {
		return
	}

	// Rate-limit control logging (1s window). For ATTITUDE, also require thrust delta >= 0.05.
	now := time.Now()
	shouldLog := now.Sub(m.lastCtrlLog) >= time.Second

	switch ctrlType {
	// inside func (m *MavlinkConnection) ApplyControl(...)
	case "RC_OVERRIDE":
		throttle, _ := data["throttle"].(float64)
		steering, _ := data["steering"].(float64)

		// Throttle: accept 0..1 or 1000..2000
		if throttle <= 2.0 {
			throttle = 1000.0 + throttle*1000.0
		}
		if throttle < 1000 {
			throttle = 1000
		}
		if throttle > 2000 {
			throttle = 2000
		}

		// Steering: accept -1..+1 (center 1500) or 1000..2000
		var steerPWM float64
		if steering <= 2.0 && steering >= -2.0 {
			// normalized: -1 → 1000, 0 → 1500, +1 → 2000
			steerPWM = 1500.0 + (steering * 500.0)
		} else {
			steerPWM = steering
		}
		if steerPWM < 1000 {
			steerPWM = 1000
		}
		if steerPWM > 2000 {
			steerPWM = 2000
		}

		msg := &common.MessageRcChannelsOverride{
			TargetSystem:    sys,
			TargetComponent: comp,
			Chan1Raw:        uint16(steerPWM),
			Chan3Raw:        uint16(throttle),
		}
		m.Node.WriteMessageAll(msg)

		if shouldLog {
			log.Printf("[MAVLink] ApplyControl RC_OVERRIDE steer=%d throttle=%d (sysID=%d compID=%d mode=%s armed=%v)",
				uint16(steerPWM), uint16(throttle), sys, comp, modeSnap, armedSnap)
			m.lastCtrlLog = now
		}

	case "VELOCITY":
		x, _ := data["vx"].(float64)
		y, _ := data["vy"].(float64)
		z, _ := data["vz"].(float64)
		yawRate, _ := data["yawRate"].(float64)

		// optional absolute yaw (radians, NED) - used for flying vehicles only
		yawAbs, yawProvided := data["yaw"].(float64)

		// Store cmdThrust for HUD comparison (UI -> agent)
		if v, ok := data["cmdThrust"].(float64); ok {
			if v < 0 {
				v = 0
			}
			if v > 1 {
				v = 1
			}
			m.mu.Lock()
			m.telemetry.CommandedThrust = v
			m.mu.Unlock()
			if shouldLog {
				log.Printf("[MAVLink] VELOCITY cmdThrust=%.2f", v)
			}
		}

		const (
			IGNORE_PX      uint16 = 1 << 0
			IGNORE_PY      uint16 = 1 << 1
			IGNORE_PZ      uint16 = 1 << 2
			IGNORE_VX      uint16 = 1 << 3
			IGNORE_VY      uint16 = 1 << 4
			IGNORE_VZ      uint16 = 1 << 5
			IGNORE_AX      uint16 = 1 << 6
			IGNORE_AY      uint16 = 1 << 7
			IGNORE_AZ      uint16 = 1 << 8
			IGNORE_YAW     uint16 = 1 << 10
			IGNORE_YAWRATE uint16 = 1 << 11
		)

		isRoverLike := m.vehicleType == uint8(common.MAV_TYPE_GROUND_ROVER) ||
			m.vehicleType == uint8(common.MAV_TYPE_SURFACE_BOAT)

		var (
			frame    common.MAV_FRAME
			typeMask uint16
		)

		if isRoverLike {
			// ArduRover: drive like an RC car using body-frame velocity + yaw rate.
			// Use BODY_OFFSET_NED for "vx forward/back" semantics.
			frame = common.MAV_FRAME_BODY_OFFSET_NED

			// Known-good ArduRover mask for "Vel + YawRate" is 1511 (0x05E7).
			// This ignores position, acceleration, yaw (absolute), and VZ.
			typeMask = 1511

			// Rover should not be given absolute yaw setpoints here.
			yawProvided = false
			yawAbs = 0

			// Ensure we don't accidentally command vertical velocity.
			z = 0
		} else {
			// Default behavior for flying vehicles: LOCAL_NED and your yaw/yawRate logic
			frame = common.MAV_FRAME_LOCAL_NED

			// Start by ignoring position + acceleration (we are doing velocity control)
			typeMask = IGNORE_PX | IGNORE_PY | IGNORE_PZ | IGNORE_AX | IGNORE_AY | IGNORE_AZ

			// If you are not intentionally controlling VZ, force it ignored and set to 0.
			// (Leaving VZ enabled while sending 0 can still be OK for copter/plane,
			// but ignoring it makes the intent explicit.)
			typeMask |= IGNORE_VZ
			z = 0

			// Yaw handling:
			if yawProvided {
				// Control absolute yaw; ignore yawRate but DO NOT ignore yaw.
				typeMask |= IGNORE_YAWRATE
			} else {
				// Control yawRate only; ignore yaw angle.
				typeMask |= IGNORE_YAW
				if math.Abs(yawRate) < 1e-4 {
					typeMask |= IGNORE_YAWRATE
				}
			}
		}

		msg := &common.MessageSetPositionTargetLocalNed{
			TargetSystem:    sys,
			TargetComponent: comp,
			CoordinateFrame: frame,
			TypeMask:        common.POSITION_TARGET_TYPEMASK(typeMask),
			Vx:              float32(x),
			Vy:              float32(y),
			Vz:              float32(z),
			YawRate:         float32(yawRate),
		}
		if yawProvided {
			msg.Yaw = float32(yawAbs)
		}

		_ = m.Node.WriteMessageAll(msg)

		if shouldLog {
			log.Printf("[MAVLink] ApplyControl VELOCITY frame=%v vx=%.2f vy=%.2f vz=%.2f yawRate=%.2f yawProvided=%v (targetSysID=%d targetCompID=%d mode=%s armed=%v typeMask=%d)",
				frame, x, y, z, yawRate, yawProvided, sys, comp, modeSnap, armedSnap, typeMask)
			m.lastCtrlLog = now
		}

	case "ATTITUDE":
		roll, _ := data["roll"].(float64)
		pitch, _ := data["pitch"].(float64)
		yaw, _ := data["yaw"].(float64)
		thrust, _ := data["thrust"].(float64)

		toRad := func(a float64) float64 {
			if a > 6.283185307 || a < -6.283185307 {
				return a * (math.Pi / 180.0)
			}
			return a
		}
		roll = toRad(roll)
		pitch = toRad(pitch)
		yaw = toRad(yaw)

		if thrust < 0 {
			thrust = 0
		}
		if thrust > 1 {
			thrust = 1
		}

		cr := math.Cos(roll * 0.5)
		sr := math.Sin(roll * 0.5)
		cp := math.Cos(pitch * 0.5)
		sp := math.Sin(pitch * 0.5)
		cy := math.Cos(yaw * 0.5)
		sy := math.Sin(yaw * 0.5)

		w := cr*cp*cy + sr*sp*sy
		x := sr*cp*cy - cr*sp*sy
		yq := cr*sp*cy + sr*cp*sy
		z := cr*cp*sy - sr*sp*cy

		n := math.Sqrt(w*w + x*x + yq*yq + z*z)
		if n == 0 {
			w, x, yq, z = 1, 0, 0, 0
		} else {
			w, x, yq, z = w/n, x/n, yq/n, z/n
		}

		const typeMaskIgnoreRates = 0b00000111
		msg := &common.MessageSetAttitudeTarget{
			TargetSystem:    sys,
			TargetComponent: comp,
			TypeMask:        typeMaskIgnoreRates,
			Q:               [4]float32{float32(w), float32(x), float32(yq), float32(z)},
			Thrust:          float32(thrust),
			BodyRollRate:    0,
			BodyPitchRate:   0,
			BodyYawRate:     0,
		}
		m.Node.WriteMessageAll(msg)

		// Echo commanded thrust for UI comparison/fallback
		m.mu.Lock()
		m.telemetry.CommandedThrust = thrust
		m.mu.Unlock()

		// Log only if 1s passed OR thrust changed ≥ 0.05 compared to last logged command
		if shouldLog || math.Abs(thrust-m.lastLoggedCmdT) >= 0.05 {
			log.Printf("[MAVLink] ApplyControl ATTITUDE roll=%.2f pitch=%.2f yaw=%.2f thrust=%.2f (sysID=%d compID=%d mode=%s armed=%v)",
				roll, pitch, yaw, thrust, sys, comp, modeSnap, armedSnap)
			m.lastCtrlLog = now
			m.lastLoggedCmdT = thrust
		}

	case "POSITION":
		x, _ := data["x"].(float64)
		y, _ := data["y"].(float64)
		z, _ := data["z"].(float64)

		msg := &common.MessageSetPositionTargetLocalNed{
			TargetSystem:    sys,
			TargetComponent: comp,
			CoordinateFrame: common.MAV_FRAME_LOCAL_NED,
			TypeMask:        common.POSITION_TARGET_TYPEMASK(0b0000111111111000),
			X:               float32(x),
			Y:               float32(y),
			Z:               float32(z),
		}
		m.Node.WriteMessageAll(msg)

		if shouldLog {
			log.Printf("[MAVLink] ApplyControl POSITION x=%.2f y=%.2f z=%.2f (sysID=%d compID=%d mode=%s armed=%v)",
				x, y, z, sys, comp, modeSnap, armedSnap)
			m.lastCtrlLog = now
		}

	case "GOTO_GLOBAL":
		lat, _ := data["lat"].(float64)
		lon, _ := data["lon"].(float64)
		alt, _ := data["alt"].(float64)
		groundspeed, _ := data["groundspeed"].(float64)

		// Use GLOBAL_INT so the UI can click on a map and send WGS84 coordinates.
		// For rovers/boats, altitude is ignored by the vehicle but still required by the message;
		// the UI will usually send 0.

		const (
			IGNORE_VX      uint16 = 1 << 3
			IGNORE_VY      uint16 = 1 << 4
			IGNORE_VZ      uint16 = 1 << 5
			IGNORE_AX      uint16 = 1 << 6
			IGNORE_AY      uint16 = 1 << 7
			IGNORE_AZ      uint16 = 1 << 8
			IGNORE_YAW     uint16 = 1 << 10
			IGNORE_YAWRATE uint16 = 1 << 11
		)

		typeMask := IGNORE_VX | IGNORE_VY | IGNORE_VZ | IGNORE_AX | IGNORE_AY | IGNORE_AZ | IGNORE_YAW | IGNORE_YAWRATE

		// If groundspeed is supplied, include it as a vx setpoint in BODY frame does not apply here.
		// For simplicity we leave velocities ignored; speed control can be handled via other modes.
		_ = groundspeed

		msg := &common.MessageSetPositionTargetGlobalInt{
			TargetSystem:    sys,
			TargetComponent: comp,
			CoordinateFrame: common.MAV_FRAME_GLOBAL_RELATIVE_ALT_INT,
			TypeMask:        common.POSITION_TARGET_TYPEMASK(typeMask),
			LatInt:          int32(math.Round(lat * 1e7)),
			LonInt:          int32(math.Round(lon * 1e7)),
			Alt:             float32(alt),
			Vx:              0,
			Vy:              0,
			Vz:              0,
			Afx:             0,
			Afy:             0,
			Afz:             0,
			Yaw:             0,
			YawRate:         0,
		}
		m.Node.WriteMessageAll(msg)

		if shouldLog {
			log.Printf("[MAVLink] ApplyControl GOTO_GLOBAL lat=%.7f lon=%.7f alt=%.1f (sysID=%d compID=%d mode=%s armed=%v)",
				lat, lon, alt, sys, comp, modeSnap, armedSnap)
			m.lastCtrlLog = now
		}

	case "GIMBAL":
		// Accept normalized -1..1 pan/tilt or absolute degrees.
		pan, _ := data["pan"].(float64)
		tilt, _ := data["tilt"].(float64)

		// If values look normalized, map to degrees.
		panDeg := pan
		tiltDeg := tilt
		if math.Abs(pan) <= 2.0 {
			panDeg = pan * 90.0
		}
		if math.Abs(tilt) <= 2.0 {
			tiltDeg = tilt * 45.0
		}

		// Clamp reasonable ranges
		if panDeg < -180 {
			panDeg = -180
		}
		if panDeg > 180 {
			panDeg = 180
		}
		if tiltDeg < -90 {
			tiltDeg = -90
		}
		if tiltDeg > 90 {
			tiltDeg = 90
		}

		// Just set the target; the background loop slews + emits DO_MOUNT_CONTROL.
		m.mu.Lock()
		m.gimbalActive = true
		m.gimbalTargetPanDeg = panDeg
		m.gimbalTargetTiltDeg = tiltDeg
		m.gimbalLastCmd = time.Now()
		m.mu.Unlock()

		if shouldLog {
			log.Printf("[MAVLink] ApplyControl GIMBAL target pan=%.1f° tilt=%.1f° (sysID=%d compID=%d)", panDeg, tiltDeg, sys, comp)
			m.lastCtrlLog = now
		}
	}
}

// -------------------------
// SAFETY / FAILSAFE HELPERS
// -------------------------

// FailSafeStopRcOverride actively sends a neutral RC override (dead-man).
// For most rovers/boats, steering center is 1500. Throttle stop depends on whether
// reverse is supported:
// - bidirectional throttle: 1500 (center)
// - forward-only: 1000 (minimum)
func (m *MavlinkConnection) FailSafeStopRcOverride(throttleStopPwm, steeringStopPwm uint16) {
	if m == nil || m.Node == nil {
		return
	}
	m.mu.RLock()
	sys := m.sysID
	comp := m.compID
	m.mu.RUnlock()
	if sys == 0 {
		return
	}
	// Defensive clamp
	clamp := func(v uint16) uint16 {
		if v < 1000 {
			return 1000
		}
		if v > 2000 {
			return 2000
		}
		return v
	}
	throttleStopPwm = clamp(throttleStopPwm)
	steeringStopPwm = clamp(steeringStopPwm)

	_ = m.Node.WriteMessageAll(&common.MessageRcChannelsOverride{
		TargetSystem:    sys,
		TargetComponent: comp,
		Chan1Raw:        steeringStopPwm,
		Chan3Raw:        throttleStopPwm,
	})
}

// FailSafeStopVelocity sends a SET_POSITION_TARGET_LOCAL_NED that commands zero velocity
// and zero yaw-rate. This is safe for rovers and copters in guided control modes.
func (m *MavlinkConnection) FailSafeStopVelocity() {
	if m == nil || m.Node == nil {
		return
	}
	m.mu.RLock()
	sys := m.sysID
	comp := m.compID
	vt := m.vehicleType
	m.mu.RUnlock()
	if sys == 0 {
		return
	}
	const (
		IGNORE_PX      uint16 = 1 << 0
		IGNORE_PY      uint16 = 1 << 1
		IGNORE_PZ      uint16 = 1 << 2
		IGNORE_VX      uint16 = 1 << 3
		IGNORE_VY      uint16 = 1 << 4
		IGNORE_VZ      uint16 = 1 << 5
		IGNORE_AX      uint16 = 1 << 6
		IGNORE_AY      uint16 = 1 << 7
		IGNORE_AZ      uint16 = 1 << 8
		IGNORE_YAW     uint16 = 1 << 10
		IGNORE_YAWRATE uint16 = 1 << 11
	)

	isRoverLike := vt == uint8(common.MAV_TYPE_GROUND_ROVER) ||
		vt == uint8(common.MAV_TYPE_SURFACE_BOAT)

	frame := common.MAV_FRAME_LOCAL_NED
	typeMask := uint16(IGNORE_PX | IGNORE_PY | IGNORE_PZ | IGNORE_AX | IGNORE_AY | IGNORE_AZ | IGNORE_YAW)

	if isRoverLike {
		frame = common.MAV_FRAME_BODY_OFFSET_NED
		typeMask = 1511 // ArduRover known-good vel+yawRate (vx/vy/yawRate enabled)
	} else {
		typeMask |= IGNORE_VZ
	}

	_ = m.Node.WriteMessageAll(&common.MessageSetPositionTargetLocalNed{
		TargetSystem:    sys,
		TargetComponent: comp,
		CoordinateFrame: frame,
		TypeMask:        common.POSITION_TARGET_TYPEMASK(typeMask),
		Vx:              0,
		Vy:              0,
		Vz:              0,
		YawRate:         0,
	})
}

// FailSafeStopAttitude sends a SET_ATTITUDE_TARGET with identity quaternion and zero thrust.
// thrust should be 0..1.
func (m *MavlinkConnection) FailSafeStopAttitude(thrust float64) {
	if m == nil || m.Node == nil {
		return
	}
	m.mu.RLock()
	sys := m.sysID
	comp := m.compID
	m.mu.RUnlock()
	if sys == 0 {
		return
	}
	if thrust < 0 {
		thrust = 0
	}
	if thrust > 1 {
		thrust = 1
	}

	const typeMaskIgnoreRates = 0b00000111
	_ = m.Node.WriteMessageAll(&common.MessageSetAttitudeTarget{
		TargetSystem:    sys,
		TargetComponent: comp,
		TypeMask:        typeMaskIgnoreRates,
		Q:               [4]float32{1, 0, 0, 0},
		Thrust:          float32(thrust),
		BodyRollRate:    0,
		BodyPitchRate:   0,
		BodyYawRate:     0,
	})
}

// ---- Control eligibility & accessors ----

func (m *MavlinkConnection) IsControlAllowed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	mode := m.telemetry.Mode
	isRover := m.vehicleType == uint8(common.MAV_TYPE_GROUND_ROVER) ||
		m.vehicleType == uint8(common.MAV_TYPE_SURFACE_BOAT)

	allowedMode := (mode == "GUIDED") || (mode == "STEERING") || (isRover && mode == "MANUAL")
	return allowedMode && m.telemetry.Armed
}

// SnapshotControlState returns (eligible, mode, armed) in a single, race-free read.
// Prefer this over calling IsControlAllowed/GetMode/GetArmed separately.
func (m *MavlinkConnection) SnapshotControlState() (bool, string, bool) {
	if m == nil {
		return false, "NO_MAVLINK", false
	}
	m.mu.RLock()
	mode := m.telemetry.Mode
	armed := m.telemetry.Armed
	vt := m.vehicleType
	m.mu.RUnlock()

	isRover := vt == uint8(common.MAV_TYPE_GROUND_ROVER) || vt == uint8(common.MAV_TYPE_SURFACE_BOAT)
	allowedMode := (mode == "GUIDED") || (mode == "STEERING") || (isRover && mode == "MANUAL")
	return allowedMode && armed, mode, armed
}

// IsAircraftLike returns true for vehicle types that should be treated as "aircraft".
// This is used for policy gating (e.g., require allowAircraftControl) regardless of ArduPilot mode.
func (m *MavlinkConnection) IsAircraftLike() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	vt := m.vehicleType
	m.mu.RUnlock()

	// Be deliberately conservative: explicitly treat known ground/surface vehicles
	// as NOT aircraft-like; treat everything else as aircraft-like.
	// This avoids relying on enum names that may not exist in the dialect package.
	switch vt {
	case uint8(common.MAV_TYPE_GROUND_ROVER),
		uint8(common.MAV_TYPE_SURFACE_BOAT):
		return false
	default:
		return true
	}
}

func (m *MavlinkConnection) GetMode() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.telemetry.Mode
}

func (m *MavlinkConnection) GetArmed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.telemetry.Armed
}

func (m *MavlinkConnection) EndpointDesc() string { return m.endpointDesc }

// ---------- UI: Health + Control Path Probe ----------

func (m *MavlinkConnection) Health() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := !m.lastHB.IsZero()
	hbAge := int64(0)
	if seen {
		hbAge = time.Since(m.lastHB).Milliseconds()
	}
	msgAge := int64(0)
	if !m.lastMsg.IsZero() {
		msgAge = time.Since(m.lastMsg).Milliseconds()
	}

	return map[string]any{
		"attached":       true,
		"endpoint":       m.endpointDesc,
		"heartbeatSeen":  seen,
		"heartbeatAgeMs": hbAge,
		"lastMsgAgeMs":   msgAge,
		"mode":           m.telemetry.Mode,
		"armed":          m.telemetry.Armed,
		"sysid":          m.sysID,
		"compid":         m.compID,
	}
}

func (m *MavlinkConnection) ProbeParam(name string, timeout time.Duration) (float64, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "SYSID_MYGCS"
	}
	up := strings.ToUpper(name)

	ch := make(chan *common.MessageParamValue, 1)

	m.mu.Lock()
	sys := m.sysID
	comp := m.compID
	if sys == 0 {
		m.mu.Unlock()
		return 0, false, fmt.Errorf("no heartbeat yet (sysid unknown)")
	}
	if m.paramWait == nil {
		m.paramWait = make(map[string]chan *common.MessageParamValue)
	}
	m.paramWait[up] = ch
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.paramWait, up)
		m.mu.Unlock()
	}()

	req := &common.MessageParamRequestRead{
		TargetSystem:    sys,
		TargetComponent: comp,
		ParamId:         up,
		ParamIndex:      -1,
	}
	if err := m.Node.WriteMessageAll(req); err != nil {
		return 0, false, fmt.Errorf("write param_request_read: %w", err)
	}

	select {
	case pv := <-ch:
		return float64(pv.ParamValue), true, nil
	case <-time.After(timeout):
		return 0, false, fmt.Errorf("timeout waiting for %s", up)
	}
}

func packParamID16(s string) [16]uint8 {
	var out [16]uint8
	s = strings.TrimSpace(s)
	if len(s) > 16 {
		s = s[:16]
	}
	copy(out[:], []byte(s))
	return out
}

func trimParamID(id string) string {
	return strings.TrimRight(id, "\x00")
}

// ---------- Mode decoding per vehicle ----------

// Map MAV_TYPE_* to a stable string for the UI.
func mavTypeName(t uint8) string {
	switch t {
	case uint8(common.MAV_TYPE_QUADROTOR):
		return "QUADROTOR"
	case uint8(common.MAV_TYPE_HEXAROTOR):
		return "HEXAROTOR"
	case uint8(common.MAV_TYPE_OCTOROTOR):
		return "OCTOROTOR"
	case uint8(common.MAV_TYPE_TRICOPTER):
		return "TRICOPTER"
	case uint8(common.MAV_TYPE_HELICOPTER):
		return "HELICOPTER"
	case uint8(common.MAV_TYPE_FIXED_WING):
		return "FIXED_WING"
	case uint8(common.MAV_TYPE_GROUND_ROVER):
		return "GROUND_ROVER"
	case uint8(common.MAV_TYPE_SURFACE_BOAT):
		return "SURFACE_BOAT"
	case uint8(common.MAV_TYPE_SUBMARINE):
		return "SUBMARINE"
	// add others you care about later
	default:
		return fmt.Sprintf("UNKNOWN_TYPE(%d)", t)
	}
}

func mavModeName(mode uint32, vehicleType uint8) string {
	switch vehicleType {
	// Copter family (quad/hexa/octo/tri/heli)
	case uint8(common.MAV_TYPE_QUADROTOR),
		uint8(common.MAV_TYPE_HELICOPTER),
		uint8(common.MAV_TYPE_HEXAROTOR),
		uint8(common.MAV_TYPE_OCTOROTOR),
		uint8(common.MAV_TYPE_TRICOPTER):
		return copterModeName(mode)

	// Rover / Boat
	case uint8(common.MAV_TYPE_GROUND_ROVER),
		uint8(common.MAV_TYPE_SURFACE_BOAT):
		return roverModeName(mode)

	// Plane
	case uint8(common.MAV_TYPE_FIXED_WING):
		return planeModeName(mode)
	}

	// Unknown type: try copter mapping (most common); else UNKNOWN
	if n := copterModeName(mode); !strings.HasPrefix(n, "UNKNOWN(") {
		return n
	}
	return fmt.Sprintf("UNKNOWN(%d)", mode)
}

func copterModeName(mode uint32) string {
	switch mode {
	case 0:
		return "STABILIZE"
	case 1:
		return "ACRO"
	case 2:
		return "ALT_HOLD"
	case 3:
		return "AUTO"
	case 4:
		return "GUIDED"
	case 5:
		return "LOITER"
	case 6:
		return "RTL"
	case 7:
		return "CIRCLE"
	case 9:
		return "LAND"
	case 11:
		return "DRIFT"
	case 13:
		return "SPORT"
	case 16:
		return "POSHOLD"
	case 17:
		return "BRAKE"
	case 18:
		return "THROW"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", mode)
	}
}

func roverModeName(mode uint32) string {
	switch mode {
	case 0:
		return "MANUAL"
	case 1:
		return "ACRO"
	case 2:
		return "LEARNING"
	case 3:
		return "STEERING"
	case 4:
		return "HOLD"
	case 10:
		return "AUTO"
	case 11:
		return "RTL"
	case 15:
		return "GUIDED"
	case 16:
		return "INITIALIZING"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", mode)
	}
}

func planeModeName(mode uint32) string {
	switch mode {
	case 0:
		return "MANUAL"
	case 1:
		return "CIRCLE"
	case 2:
		return "STABILIZE"
	case 3:
		return "TRAINING"
	case 4:
		return "ACRO"
	case 5:
		return "FBWA"
	case 6:
		return "FBWB"
	case 10:
		return "AUTO"
	case 11:
		return "RTL"
	case 12:
		return "LOITER"
	case 15:
		return "GUIDED"
	case 16:
		return "INITIALIZING"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", mode)
	}
}

// ---------- UI payload + teardown ----------

func (m *MavlinkConnection) BuildTelemetryPayload(mavID int64) map[string]any {
	t := m.GetTelemetryUpdate("")
	log.Printf("[TX] mode=%s armed=%v thrust=%.2f src=%s cmd=%.2f rc=%d vs=%.2f gs=%.2f",
		t.Mode, t.Armed, t.Thrust, t.ThrustSource, t.CommandedThrust, t.RCThrottle, t.VerticalSpeed, t.GroundSpeed)

	att := t.Attitude
	att.Roll = -att.Roll
	att.Pitch = -att.Pitch

	return map[string]any{
		"type":            "TELEMETRY_UPDATE",
		"mavId":           mavID,
		"mode":            t.Mode,
		"armed":           t.Armed,
		"battery":         t.Battery,
		"gps":             t.GPS,
		"attitude":        att,
		"altitude":        t.GPS.Alt,
		"speed":           t.GroundSpeed,
		"verticalSpeed":   t.VerticalSpeed,
		"heading":         t.Heading,
		"rcThrottle":      t.RCThrottle,
		"thrust":          t.Thrust,
		"thrustSource":    t.ThrustSource,
		"commandedThrust": t.CommandedThrust,
		"statusMessages":  t.StatusMessages,
		"timestamp":       t.Timestamp,
		"vehicleType":     t.VehicleType,
		"vehicleTypeRaw":  t.VehicleTypeRaw,
	}
}

func (m *MavlinkConnection) Close() {
	if m == nil || m.Node == nil {
		return
	}
	m.Node.Close()
}

func (m *MavlinkConnection) servoRawByIndex(idx int) uint16 {
	if m.servoOut == nil {
		return 0
	}
	switch idx {
	case 1:
		return m.servoOut.Servo1Raw
	case 2:
		return m.servoOut.Servo2Raw
	case 3:
		return m.servoOut.Servo3Raw
	case 4:
		return m.servoOut.Servo4Raw
	case 5:
		return m.servoOut.Servo5Raw
	case 6:
		return m.servoOut.Servo6Raw
	case 7:
		return m.servoOut.Servo7Raw
	case 8:
		return m.servoOut.Servo8Raw
	case 9:
		return m.servoOut.Servo9Raw
	case 10:
		return m.servoOut.Servo10Raw
	case 11:
		return m.servoOut.Servo11Raw
	case 12:
		return m.servoOut.Servo12Raw
	case 13:
		return m.servoOut.Servo13Raw
	case 14:
		return m.servoOut.Servo14Raw
	case 15:
		return m.servoOut.Servo15Raw
	case 16:
		return m.servoOut.Servo16Raw
	default:
		return 0
	}
}

// Rover uses a "split" mapping: 1000=full reverse, 1500=stop, 2000=full forward.
// Return 0..1 so 0.5 means stop (matches your UI center-split).
func pwmToSplit01(pwm uint16) float64 {
	if pwm < 900 || pwm > 2100 {
		return 0.5
	}
	v := (float64(pwm) - 1000.0) / 1000.0 // 1000..2000 -> 0..1
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return v
}

// ensureRoverServoMapAsync probes SERVOx_FUNCTION parameters once (best-effort)
// and caches the mapping used for rover thrust inference.
//
// CRITICAL: this must never run while holding m.mu, because ProbeParam takes m.mu.
func (m *MavlinkConnection) ensureRoverServoMapAsync() {
	if m == nil {
		return
	}
	// Avoid redundant goroutines.
	if m.roverServoMapDone.Load() || m.roverServoMapInFlight.Load() {
		return
	}

	// Only relevant for rover-like vehicles, and only after we know target sys/comp.
	m.mu.RLock()
	sys := m.sysID
	comp := m.compID
	vt := m.vehicleType
	m.mu.RUnlock()

	if sys == 0 || comp == 0 {
		return
	}
	if !(vt == uint8(common.MAV_TYPE_GROUND_ROVER) || vt == uint8(common.MAV_TYPE_SURFACE_BOAT)) {
		return
	}

	if !m.roverServoMapInFlight.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer m.roverServoMapInFlight.Store(false)

		var throttle, left, right int32
		for i := 1; i <= 16; i++ {
			name := fmt.Sprintf("SERVO%d_FUNCTION", i)
			val, ok, err := m.ProbeParam(name, 400*time.Millisecond)
			if err != nil || !ok {
				continue
			}
			fn := int(val + 0.5) // param values are floats; round
			switch fn {
			case 70: // Throttle
				throttle = int32(i)
			case 73: // Throttle Left
				left = int32(i)
			case 74: // Throttle Right
				right = int32(i)
			}
		}

		m.roverThrottleIdx.Store(throttle)
		m.roverThrottleLeftIdx.Store(left)
		m.roverThrottleRightIdx.Store(right)
		m.roverServoMapDone.Store(true)

		log.Printf("[MAVLink] Rover servo map: throttle=%d left=%d right=%d", throttle, left, right)
	}()
}

func (m *MavlinkConnection) inferRoverThrustFromServos() (float64, bool) {
	if m.servoOut == nil {
		return 0, false
	}

	leftIdx := int(m.roverThrottleLeftIdx.Load())
	rightIdx := int(m.roverThrottleRightIdx.Load())
	thrIdx := int(m.roverThrottleIdx.Load())

	// Prefer skid-steer outputs if present (left/right)
	if leftIdx > 0 && rightIdx > 0 {
		l := m.servoRawByIndex(leftIdx)
		r := m.servoRawByIndex(rightIdx)
		if l == 0 || r == 0 {
			return 0, false
		}
		avg := 0.5 * (pwmToSplit01(l) + pwmToSplit01(r))
		return avg, true
	}

	// Otherwise single throttle output
	if thrIdx > 0 {
		t := m.servoRawByIndex(thrIdx)
		if t == 0 {
			return 0, false
		}
		return pwmToSplit01(t), true
	}

	return 0, false
}

// ──────────────────────────────────────────────────────────────────────────────
// Testing helpers – arm/disarm and mode-set commands.
// These are intentionally kept simple; they send a single COMMAND_LONG and
// return immediately without waiting for COMMAND_ACK.
// ──────────────────────────────────────────────────────────────────────────────

// ArmDisarm sends MAV_CMD_COMPONENT_ARM_DISARM (400).
// arm=true → arm, arm=false → disarm.
// force=true adds the magic param2 value (21196) that bypasses pre-arm checks.
func (m *MavlinkConnection) ArmDisarm(arm bool, force bool) error {
	m.mu.RLock()
	sys := m.sysID
	comp := m.compID
	m.mu.RUnlock()
	if sys == 0 {
		return fmt.Errorf("no MAVLink target yet (no heartbeat received)")
	}

	var p1 float32 // 1 = arm, 0 = disarm
	if arm {
		p1 = 1
	}
	var p2 float32 // 21196 = bypass pre-arm checks
	if force {
		p2 = 21196
	}

	log.Printf("[MAVLink] ArmDisarm arm=%v force=%v → sys=%d comp=%d", arm, force, sys, comp)
	return m.Node.WriteMessageAll(&common.MessageCommandLong{
		TargetSystem:    sys,
		TargetComponent: comp,
		Command:         common.MAV_CMD_COMPONENT_ARM_DISARM,
		Confirmation:    0,
		Param1:          p1,
		Param2:          p2,
	})
}

// roverModeNumber returns the ArduPilot Rover custom_mode number for a named mode.
// Returns -1 if the name is not recognised.
func roverModeNumber(name string) int {
	switch strings.ToUpper(name) {
	case "MANUAL":
		return 0
	case "ACRO":
		return 1
	case "LEARNING":
		return 2
	case "STEERING":
		return 3
	case "HOLD":
		return 4
	case "AUTO":
		return 10
	case "RTL":
		return 11
	case "GUIDED":
		return 15
	}
	return -1
}

// SetMode sends MAV_CMD_DO_SET_MODE for a named ArduPilot Rover mode.
// The base_mode is MAV_MODE_FLAG_CUSTOM_MODE_ENABLED.
func (m *MavlinkConnection) SetMode(modeName string) error {
	m.mu.RLock()
	sys := m.sysID
	comp := m.compID
	m.mu.RUnlock()
	if sys == 0 {
		return fmt.Errorf("no MAVLink target yet (no heartbeat received)")
	}

	customMode := roverModeNumber(modeName)
	if customMode < 0 {
		return fmt.Errorf("unknown mode %q", modeName)
	}

	const MAV_MODE_FLAG_CUSTOM_MODE_ENABLED = 1
	log.Printf("[MAVLink] SetMode %q (customMode=%d) → sys=%d comp=%d", modeName, customMode, sys, comp)
	return m.Node.WriteMessageAll(&common.MessageCommandLong{
		TargetSystem:    sys,
		TargetComponent: comp,
		Command:         common.MAV_CMD_DO_SET_MODE,
		Confirmation:    0,
		Param1:          MAV_MODE_FLAG_CUSTOM_MODE_ENABLED,
		Param2:          float32(customMode),
	})
}
