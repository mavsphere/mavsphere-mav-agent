# MavSphere Agent — Operator Setup Guide

> **Who this is for:** You have a vehicle running ArduPilot (rover, boat, drone, etc.), a Raspberry Pi 4B as your companion computer, and you want to connect it to MavSphere so remote users can view and control it.

---

## What you need before you start

- Raspberry Pi 4B running Ubuntu 22.04 or 24.04 (64-bit). Raspberry Pi OS (Bookworm, 64-bit) also works.
- A MavSphere operator account with Primary Pilot status and at least one MAV registered.
- Your MAV's ID and your account credentials from the MavSphere dashboard.
- A USB cable **or** GPIO wiring to connect the Pi to your flight controller.
- The Pi connected to your local network via Ethernet or Wi-Fi.

---

## Step 1 — Connect the flight controller to the Pi

The Pi communicates with your ArduPilot flight controller over a serial link. There are two common ways to wire this.

### Option A — USB (easiest, recommended for getting started)

Plug a USB cable from the flight controller's USB port into any USB port on the Pi. That's it.

When the Pi boots with the flight controller connected, check which port it appeared on:

```bash
ls /dev/ttyACM* /dev/ttyUSB* 2>/dev/null
```

You will typically see `/dev/ttyACM0` (Pixhawk, Cube, etc.) or `/dev/ttyUSB0` (FTDI-based boards). If you see nothing, try a different cable — many USB cables are charge-only and carry no data.

### Option B — GPIO / UART (for permanent installations)

Connect the flight controller's UART TX → Pi GPIO 15 (RXD), and FC UART RX → Pi GPIO 14 (TXD). Share a common ground. This uses `/dev/ttyAMA0` on the Pi.

You must enable the UART and disable the serial console first:

```bash
sudo raspi-config
```

Go to **Interface Options → Serial Port → disable login shell → enable hardware**. Reboot.

Then verify the port is present:

```bash
ls /dev/ttyAMA0
```

> **Baud rate:** Most ArduPilot boards default to 57600 baud on the telemetry UART. USB connections negotiate automatically.

---

## Step 2 — Run the one-line installer

On the Pi, run:

```bash
curl -fsSL https://mavsphere.com/downloads/setup_pi.sh | sudo bash
```

This script installs Docker, MAVProxy, and the MavSphere agent as systemd services that start on boot. It also creates a config file at `/etc/mavsphere/config.json` which you will edit in the next step.

After the script finishes, the agent web UI is available at:

```
http://<your-pi-ip>:8090/
```

Open that address in a browser on any device on the same network.

---

## Step 3 — Configure the agent

Open `http://<pi-ip>:8090/` in your browser. You will see the MAVSphere Agent Configuration panel.

Fill in the following fields:

**Basics**

| Field | What to enter |
|---|---|
| MAV ID | The numeric ID of your MAV from the MavSphere dashboard |
| Username | Your MavSphere account email |
| Password | Your MavSphere account password |

**Backend**

Leave the Backend URL and WebSocket URL as-is if you are connecting to the live MavSphere service. They default to `https://mavsphere.com` and `wss://mavsphere.com/api/ws/agent`.

**MAVLink Connection**

This tells the agent how to receive MAVLink data. The installer sets up MAVProxy as a bridge between your flight controller and the agent, so the correct setting depends on how you connected the FC in Step 1.

| Connection type | FC wiring | Agent setting |
|---|---|---|
| UDP via MAVProxy (default) | USB (`/dev/ttyACM0`) | `udp:127.0.0.1:14600` |
| UDP via MAVProxy (default) | GPIO UART (`/dev/ttyAMA0`) | `udp:127.0.0.1:14600` |
| Serial direct (no MAVProxy) | USB or UART | `serial:/dev/ttyACM0:57600` |

For most setups, leave the connection type as **UDP via MAVProxy** and the host as `127.0.0.1` port `14600`. This is what the installer configures by default.

**Vehicle type and control gates**

- **Allow Control** — set to `true` to allow remote users to control the vehicle.
- **Allow aircraft-like control** — leave `false` unless your vehicle is a multicopter or fixed-wing. For rovers and boats this should stay `false`.
- **Thrust mode** — for rovers and boats with reverse capability, use `THRUST_FWD_REV`. For forward-only (e.g. a boat with a single forward prop), use `THRUST_FWD_ONLY`.

Click **Save & Restart Agent** when done.

---

## Step 4 — Tell MAVProxy which serial port to use

The installer creates a MAVProxy systemd service. By default it reads from `/dev/ttyAMA0` at 57600 baud (GPIO UART). If your flight controller is on USB, update the service:

Edit `/etc/systemd/system/mavproxy.service` and change the `--master=` line to match your port:

```
# USB connection:
--master=/dev/ttyACM0,57600

# GPIO UART connection:
--master=/dev/ttyAMA0,57600
```

Then reload and restart:

```bash
sudo systemctl daemon-reload
sudo systemctl restart mavproxy
```

You can check MAVProxy is running and reading data:

```bash
sudo systemctl status mavproxy
sudo journalctl -u mavproxy -f
```

You should see heartbeat messages appearing every second if the FC is connected and powered.

---

## Step 5 — Verify the connection

Back in the agent web UI at `http://<pi-ip>:8090/`:

1. Check the **Status** panel at the top. You should see the **Mode** and **Armed** fields update — this means MAVLink data is flowing from the FC through MAVProxy to the agent.
2. Click **Run control-path check**. This sends a MAVLink parameter request to the FC and reports whether the control path is working end-to-end.
3. Check the **Heartbeat** and **Last msg** fields — these should update every few seconds.

If the status shows no data, check:
- MAVProxy is running: `sudo systemctl status mavproxy`
- The correct port is configured in the MAVProxy service
- The FC is powered and connected

---

## Step 6 — Set ArduPilot parameters

A few ArduPilot parameters are important for MavSphere to work correctly.

Connect to the vehicle with MAVProxy or QGroundControl (QGC is available at `http://<pi-ip>:14550` once MAVProxy is running) and set:

```
param set SYSID_MYGCS 252
```

This tells ArduPilot to accept GCS commands from the MavSphere agent (system ID 252 by default). Without this, the vehicle will not respond to guided commands.

**For rovers — recommended failsafe settings:**

```
param set RC_OVERRIDE_TIME 0.5
param set FS_THR_ENABLE 2
param set FS_THR_VALUE 910
param set FS_GCS_ENABLE 1
param write
```

These cause the vehicle to stop safely if communication is lost. `RC_OVERRIDE_TIME 0.5` means RC override commands time out after 0.5 seconds if no new command arrives — important so the vehicle does not carry on at speed if the network drops.

---

## Step 7 — Check the vehicle appears online in MavSphere

Log into MavSphere and open **Manage MAVs**. Your vehicle should show as **ACTIVE** (green) once the agent is connected and sending heartbeats. If it shows as OFFLINE, check the agent status panel and the backend connection fields in the config.

---

## Video streaming

The agent streams video via WebRTC through the Janus media server. The installer configures Janus automatically.

By default the agent uses H.264 hardware encoding on the Pi (`v4l2h264enc`) at 960×540, 30fps, 1.5 Mbps. These settings work well on a 4G connection. You can adjust them in the agent web UI under **Video**.

The camera must be a V4L2-compatible USB webcam or the Pi Camera Module (using the legacy camera stack or `libcamera-v4l2` bridge). To check what cameras are detected:

```bash
v4l2-ctl --list-devices
```

---

## Checking service health

```bash
# Agent container
sudo docker ps

# Agent logs
sudo docker logs mavsphere-agent -f

# MAVProxy
sudo systemctl status mavproxy
sudo journalctl -u mavproxy -f

# Restart everything
sudo systemctl restart mavproxy
sudo systemctl restart mavsphere-agent
```

---

## Ground station access (QGroundControl / Mission Planner)

While the agent is running, MAVProxy also forwards telemetry to port 14550. You can connect QGroundControl on another machine on the same network:

- Open QGC → **Application Settings → Comm Links → Add**
- Type: UDP, Port: 14550, Target host: `<pi-ip>`

This lets you monitor the vehicle alongside MavSphere without interfering with the agent.

---

## Networking

The agent needs outbound internet access to reach MavSphere. It connects over WebSocket (port 443 / WSS) and WebRTC (UDP, negotiated via ICE). No inbound ports need to be opened on your router.

For networking setup specific to the Pi (Wi-Fi priority, mobile hotspot preference, etc.) see [NETWORKING.md](Docs/NETWORKING.md).

---

## Safety reminder

The MavSphere agent relays control messages only. It is not a safety system or emergency stop. The Primary Pilot must remain present, aware of the vehicle's state, and able to intervene at any time. Local override — switching the vehicle to MANUAL or HOLD mode — takes effect immediately regardless of any remote command in flight.

See the [MavSphere operator safety model](https://mavsphere.com/operators/safety) for full guidance.
