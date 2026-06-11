# mavsphere-mav-agent

The **MavSphere MAV Agent** connects an ArduPilot/MAVLink vehicle (drone, rover, or boat) to the MavSphere cloud platform, enabling live first-person remote control over WebRTC.

It runs on a companion computer (Raspberry Pi or similar) alongside the vehicle and handles:
- MAVLink telemetry forwarding
- WebRTC video streaming via Janus SFU
- STOMP control channel to the MavSphere backend
- Local web UI for configuration and status

> **Safety notice:** This agent is part of the wider MavSphere platform. Acceptance of platform agreements is handled by the backend and UI. The local operator remains responsible for lawful operation, supervision, fail-safe behaviour, local override, mode switching, and any shutdown process. Do not rely on the agent or platform as the only safety layer or emergency-stop system.

---

## Prerequisites

- Go 1.22+ (for building from source)
- Docker and Docker Compose (for running via the provided scripts)
- GStreamer (for video streaming):

```bash
sudo apt install -y \
  gstreamer1.0-tools \
  gstreamer1.0-plugins-base \
  gstreamer1.0-plugins-good \
  gstreamer1.0-plugins-bad \
  gstreamer1.0-plugins-ugly \
  gstreamer1.0-libav \
  gstreamer1.0-rtsp \
  gstreamer1.0-x \
  gstreamer1.0-gl \
  gstreamer1.0-nice
```

---

## Quick start (Raspberry Pi)

The easiest way to get started is the setup script:

```bash
curl -fsSL https://mavsphere.com/downloads/setup_pi.sh -o setup_pi.sh
chmod +x setup_pi.sh
sudo ./setup_pi.sh
```

This installs Docker, MAVProxy, creates the config file, and sets up systemd services that start automatically on boot.

After setup, the agent web UI is available at:

```
http://<pi-ip>:8484/
```

---

## Configuration

Copy the example config and edit it:

```bash
cp config.json.example config.json
```

### Required fields

| Field | Description |
|---|---|
| `mavId` | Your MAV ID — visible in the MavSphere UI |
| `backendUrl` | MavSphere backend URL, e.g. `https://mavsphere.com` |
| `backendWsUrl` | WebSocket URL, e.g. `wss://mavsphere.com/api/ws/agent` |
| `janusUrl` | Janus WebSocket URL, e.g. `wss://mavsphere.com/janus` |
| `username` | Your MavSphere login email |
| `password` | Your MavSphere password |
| `mavlinkConnection` | MAVLink connection string, e.g. `udp:0.0.0.0:14551` |

See `config.json.example` for the full reference including camera and ICE configuration.

---

## Building from source

### Local binary

```bash
go mod tidy
make build-release        # optimised binary → bin/
make build-debug          # debug-friendly binary → bin/
```

### Cross-compile

```bash
make build-linux-amd64
make build-linux-arm64
```

### Docker image (multi-arch)

```bash
# Build and push to GHCR (amd64 + arm64)
make docker-push

# Local build only (single arch, no push)
make docker-build
```

---

## Testing without hardware (SITL)

The agent works with ArduPilot SITL for development and testing without a real vehicle.

### 1. Run ArduPilot SITL

**Rover:**
```bash
./build/sitl/bin/ardurover \
  -S --model=rover --speedup=1 \
  --defaults=Tools/autotest/default_params/rover.parm \
  --home=51.259797,-1.082559,0,270 -I0
```

**Copter:**
```bash
./build/sitl/bin/arducopter \
  -S --model=+ --speedup=1 \
  --defaults=Tools/autotest/default_params/copter.parm \
  --home=51.259797,-1.082559,0,270 -I0
```

**Plane:**
```bash
./build/sitl/bin/arduplane \
  --model plane --speedup 1 \
  --defaults Tools/autotest/models/plane.parm \
  --home=51.259797,-1.082559,0,270 -I0
```

### 2. Run MAVProxy

```bash
mavproxy.py --master=tcp:127.0.0.1:5760 \
  --out=udp:127.0.0.1:14550 \
  --out=udp:127.0.0.1:14551
```

### 3. Virtual camera (simulated video feed)

For testing without a real camera, use `v4l2loopback`:

```bash
# Install
sudo apt install v4l2loopback-dkms linux-headers-$(uname -r) ffmpeg

# Load virtual device
sudo modprobe v4l2loopback devices=1 video_nr=42 \
  card_label="MavSphere SimCam" exclusive_caps=1

# Feed with an MP4 file
ffmpeg -re -stream_loop -1 -i ~/footage.mp4 \
  -vf scale=640:480 -vcodec mjpeg -q:v 5 -f v4l2 /dev/video42
```

Then set `"videoDevice": "/dev/video42"` in your config.

See [Docs/NETWORKING.md](./Docs/NETWORKING.md) for ICE/TURN configuration and networking details.

---

## Latency testing

Simulate network conditions on the agent's WiFi interface:

```bash
# Add 2s delay
sudo tc qdisc add dev wlan0 root netem delay 2000ms

# Simulate mobile network (1.2s delay, jitter, 2% loss)
sudo tc qdisc add dev wlan0 root netem delay 1200ms 200ms loss 2%

# Remove
sudo tc qdisc del dev wlan0 root
```

---

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) for the contribution workflow, CLA requirements, and code style guide.

This repository requires a signed CLA before pull requests can be merged. Agent contributions may be used in the commercial MAV product under the terms described in the CLA.

The MAV agent is MIT licensed. See [LICENSE.txt](./LICENSE.txt).
