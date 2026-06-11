# MAVLink Serial Connection — Raspberry Pi ↔ Flight Controller (AET-H743-Basic)

> **Note:** Replace `YOUR_USERNAME` throughout this document with your actual Pi username
> (commonly `pi`, `ubuntu`, or whatever you set during OS install).

## 1. Physical Wiring
| Pi Pin | Function | FC Pin | Notes |
|---------|-----------|--------|-------|
| Pin 8 (GPIO 14) | TXD0 | FC RX (R1) | Pi → FC data |
| Pin 10 (GPIO 15) | RXD0 | FC TX (T1) | FC → Pi data |
| Pin 6 (GND) | GND | FC GND | Common ground |
| *(no 5 V)* |  |  | Each powered separately |

## 2. Enable Raspberry Pi UART
Disable console and enable hardware serial:

```bash
sudo raspi-config
# Interface Options → Serial Port
#  • "Login shell over serial?" → NO
#  • "Enable serial port hardware?" → YES
sudo reboot
```

For Ubuntu on Pi (manual method):

```bash
echo 'enable_uart=1' | sudo tee -a /boot/firmware/config.txt
echo 'dtoverlay=disable-bt' | sudo tee -a /boot/firmware/config.txt
sudo sed -i 's/console=serial0,[0-9]\+ //g' /boot/firmware/cmdline.txt
sudo sed -i 's/console=ttyAMA0,[0-9]\+ //g' /boot/firmware/cmdline.txt
sudo reboot
```

Verify:
```bash
ls -l /dev/serial* /dev/ttyAMA* /dev/ttyS*
# → /dev/ttyAMA0 should exist
```

## 3. Flight Controller Configuration (ArduPilot)
```
SERIAL1_PROTOCOL = 2     # MAVLink2
SERIAL1_BAUD     = 57    # 57,600 baud  (or 115 for 115,200)
```

## 4. Verify Serial Link on Pi
```bash
sudo stty -F /dev/ttyAMA0 57600 raw -echo
sudo hexdump -C /dev/ttyAMA0 | head
```
If no data → check wiring, TX/RX swap, baud, or FC port mode.

## 5. MavSphere Agent Config

The recommended setup is to run MAVProxy as a hub (see Section 13), which owns the
serial port and fans MAVLink out over UDP. The agent then connects via UDP:

```yaml
mavlink:
  type: udp
  bind: 0.0.0.0:14600   # agent listens here; MAVProxy sends to 127.0.0.1:14600
```

If you want to connect the agent **directly** to serial (no MAVProxy, single consumer only):

```yaml
mavlink:
  type: serial
  device: /dev/ttyAMA0
  baud: 57600
```

> **Note:** Direct serial only works if nothing else (MAVProxy, GCS) needs the port at the
> same time. For most deployments the MAVProxy hub setup in Section 13 is preferred.

## 6. MAVProxy Bridge (Run on Pi)
```bash
mavproxy.py --master=/dev/ttyAMA0,57600 --out=udp:0.0.0.0:14550
```
Then connect Mission Planner or QGroundControl to `udp://<Pi_IP>:14550`.

## 7. systemd Autostart Service
Create `/etc/systemd/system/mavproxy.service`:

```ini
[Unit]
Description=MAVProxy bridge service
After=network.target

[Service]
ExecStart=/usr/local/bin/mavproxy.py --master=/dev/ttyAMA0,57600 --out=udp:0.0.0.0:14550
Restart=on-failure
User=YOUR_USERNAME
Environment=PYTHONUNBUFFERED=1

[Install]
WantedBy=multi-user.target
```

Enable and start it:
```bash
sudo systemctl daemon-reload
sudo systemctl enable mavproxy
sudo systemctl start mavproxy
```

## ✅ Checklist
- `/dev/ttyAMA0` present
- UART enabled, console disabled
- FC `SERIAL1_PROTOCOL=2`
- Ground shared
- MAVProxy/agent shows heartbeats


## 8. Installing & Using MAVProxy

### Install MAVProxy (Python 3)
On Raspberry Pi:
```bash
sudo apt update
sudo apt install python3-pip -y
sudo pip3 install MAVProxy
```
Verify:
```bash
mavproxy.py --version
```

### Run MAVProxy as serial-to-UDP bridge
```bash
mavproxy.py --master=/dev/ttyAMA0,57600 --out=udp:0.0.0.0:14550 --out=udp:0.0.0.0:14551
```
- `--master` connects to FC via serial.
- `--out` opens UDP ports for GCS (Mission Planner, QGroundControl).
Connect GCS to `udp://<Pi_IP>:14550`.

### Mission Planner connection
1. Find Pi IP: `hostname -I`
2. In Mission Planner → CONNECT → UDP → Host `<Pi_IP>` → Port `14550`
3. Connect and ARM/DISARM normally.

### MAVProxy CLI commands
| Command | Description |
|----------|--------------|
| `mode GUIDED` | Change flight mode |
| `arm throttle` | Arm motors |
| `disarm` | Disarm motors |
| `status` | Show vehicle state |
| `param show` | List parameters |
| `param set NAME VALUE` | Set parameter |
| `wp list` | List waypoints |
| `wp load file.txt` | Load waypoints |
| `set moddebug 4` | Verbose debug |

Example:
```bash
mavproxy.py --master=/dev/ttyAMA0,57600
# Inside MAVProxy prompt
mode GUIDED
arm throttle
```

### Auto-start with systemd
(See section 7: `/etc/systemd/system/mavproxy.service`)

```bash
sudo systemctl daemon-reload
sudo systemctl enable mavproxy
sudo systemctl start mavproxy
sudo systemctl status mavproxy
```


## 9. Official ArduPilot MAVProxy Installation (Modernized for Ubuntu/Pi OS 2025)

Based on [ArduPilot MAVProxy Documentation](https://ardupilot.org/mavproxy/docs/getting_started/download_and_installation.html)

### 1. Install prerequisites
```bash
sudo apt update
sudo apt install -y python3 python3-pip python3-dev python3-opencv python3-pygame \
  python3-lxml python3-yaml python3-pil python3-serial python3-future \
  python3-matplotlib python3-pyparsing python3-wxgtk4.0 python3-tk \
  python3-pipx python3-venv git
```

### 2. Install MAVProxy safely (PEP 668 compliant)
Modern Ubuntu and Raspberry Pi OS mark system Python as "externally managed."
Use **pipx** instead of `sudo pip3 install MAVProxy`:

```bash
pipx ensurepath
export PATH="$HOME/.local/bin:$PATH"   # add immediately for this session
pipx install MAVProxy
```
Confirm:
```bash
mavproxy.py --version
```
If you see `MAVProxy v1.x.x`, installation succeeded.

### 3. Connect to flight controller
**Direct session:**
```bash
mavproxy.py --master=/dev/ttyAMA0,57600
# At MAVProxy prompt:
mode GUIDED
arm throttle
```
**Bridge for Mission Planner:**
```bash
mavproxy.py --master=/dev/ttyAMA0,57600 --out=udp:0.0.0.0:14550
```
Connect Mission Planner (desktop) to `udp://<Pi_IP>:14550`.

### 4. Auto-start (systemd)
Use the same service file from Section 7, but ensure the path points to pipx install:

```ini
ExecStart=/home/YOUR_USERNAME/.local/bin/mavproxy.py --master=/dev/ttyAMA0,57600 --out=udp:0.0.0.0:14550
```

### 5. Permissions
Ensure serial access:
```bash
sudo usermod -aG dialout YOUR_USERNAME
sudo reboot
```

### ✅ Summary
- Use `pipx` (safe, user‑local) rather than `sudo pip3`.
- Confirm `/dev/ttyAMA0` works.
- Mission Planner connects via UDP 14550.
- `systemctl enable mavproxy` keeps bridge running on boot.


## 10. Package Availability and pipx Installation Fix (Ubuntu/Pi OS 2025)

Some Ubuntu and Raspberry Pi OS releases do not include `python3-future` or `python3-pipx` as APT packages.
Use the following corrected installation steps:

### 1. Install prerequisites safely
```bash
sudo apt update
sudo apt install -y pipx python3-venv python3-dev git \
  python3-opencv python3-pygame python3-lxml python3-yaml \
  python3-pil python3-serial python3-matplotlib python3-pyparsing \
  python3-wxgtk4.0 python3-tk || true
```
*(`|| true` allows the command to continue if any optional package is missing.)*

### 2. Enable pipx in PATH
```bash
pipx ensurepath
export PATH="$HOME/.local/bin:$PATH"
```
If `pipx` isn't found after install, open a new terminal or re‑source your shell profile.

### 3. Install MAVProxy (PEP 668 compliant)
```bash
pipx install MAVProxy
mavproxy.py --version
```

### 4. Run MAVProxy bridge
```bash
mavproxy.py --master=/dev/ttyAMA0,57600 --out=udp:0.0.0.0:14550
```

### ✅ Summary
- Use `sudo apt install pipx` (not `python3-pipx`)
- Skip unavailable optional packages such as `python3-future`
- After pipx install, ensure `$HOME/.local/bin` is in your PATH
- MAVProxy runs normally via `mavproxy.py`


## 11. Troubleshooting: `ModuleNotFoundError: No module named 'future'` (pipx + Python 3.13)

On some Ubuntu/Raspberry Pi OS builds with Python 3.13, the `future` dependency may not auto‑install inside the pipx venv.

### Fix (pipx):
```bash
# See the MAVProxy venv name
pipx list

# Inject the missing dependency into the MAVProxy venv
pipx inject MAVProxy future

# Verify MAVProxy launches
mavproxy.py --version
```

If other pure‑Python deps are reported missing, you can inject similarly, e.g.:
```bash
pipx inject MAVProxy pyserial lxml
```

> Tip: `pipx runpip MAVProxy list` shows what's installed inside the venv.
> If you still see issues, try `pipx upgrade MAVProxy` after injecting.


## 12. Final Troubleshooting Notes (setuptools + ModemManager)

### Missing `pkg_resources`
If you see:
```
ModuleNotFoundError: No module named 'pkg_resources'
```
Install `setuptools` inside the MAVProxy pipx venv:
```bash
pipx inject MAVProxy setuptools
mavproxy.py --version
```
This restores normal startup by providing `pkg_resources` used by MAVProxy.

### ModemManager Warning
If MAVProxy warns:
```
WARNING: You should uninstall ModemManager as it conflicts with APM and Pixhawk
```
Disable or remove it (it can seize serial ports used by the flight controller):
```bash
sudo systemctl disable --now ModemManager || true
sudo apt purge -y modemmanager || true
```

### ✅ After these fixes
Run MAVProxy normally:
```bash
mavproxy.py --master=/dev/ttyAMA0,57600 --out=udp:0.0.0.0:14550
```
Connect your Ground Control Station (Mission Planner or QGroundControl) to `udp://<Pi_IP>:14550`.


## 13. Auto‑start & Auto‑retry (Power Loss Safe)

Goal: after power loss the Pi boots, **MAVProxy starts first** (owns `/dev/ttyAMA0`), retries until the FC is present, and your **Agent** starts after MAVProxy and receives MAVLink on UDP.

### A) MAVProxy systemd service (hub)
Create `/etc/systemd/system/mavproxy.service`:
```ini
[Unit]
Description=MAVProxy hub (serial -> UDP fanout)
After=network-online.target
Wants=network-online.target

[Service]
# Run as your normal user so it can read ~/.mavproxy
User=YOUR_USERNAME
WorkingDirectory=/home/YOUR_USERNAME

# MAVProxy owns the UART and fans out to:
#  - UDP 14550 for GCS (Mission Planner, QGC)
#  - UDP 14600 for the local agent (agent should BIND to 14600)
ExecStart=/home/YOUR_USERNAME/.local/bin/mavproxy.py \
  --master=/dev/ttyAMA0,57600 \
  --out=udp:0.0.0.0:14550 \
  --out=udp:127.0.0.1:14600

# Robust auto-restart forever (even if FC not present yet)
Restart=always
RestartSec=3
StartLimitIntervalSec=0

# Environment + nicer logging
Environment=PYTHONUNBUFFERED=1

# Keep stdout/stderr in journal
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

> **Why no `Requires=dev-ttyAMA0.device`?** We *want* MAVProxy to start even if the FC
> isn't present; it will fail and systemd will retry until it can open the port.

Enable it:
```bash
sudo systemctl daemon-reload
sudo systemctl enable --now mavproxy
sudo systemctl status mavproxy
```

### B) Agent systemd service (after MAVProxy)
Update/create `/etc/systemd/system/mavsphere-agent.service`:
```ini
[Unit]
Description=MavSphere MAV Agent
After=network-online.target mavproxy.service
Wants=network-online.target mavproxy.service

[Service]
User=YOUR_USERNAME
WorkingDirectory=/home/YOUR_USERNAME/mavsphere-mav-agent
ExecStart=/home/YOUR_USERNAME/mavsphere-mav-agent/bin/mavsphere-agent
Restart=on-failure
RestartSec=3
Environment=PYTHONUNBUFFERED=1

[Install]
WantedBy=multi-user.target
```

Agent **config.json** (excerpt):
```json
{
  "mavlinkConnection": "udp:0.0.0.0:14600"
}
```

Reload + enable:
```bash
sudo systemctl daemon-reload
sudo systemctl enable --now mavsphere-agent
sudo systemctl status mavsphere-agent
```

### C) Mission Planner / GCS
Connect to the Pi's IP on **UDP 14550** (MAVProxy fan‑out).

### D) Health & Logs
```bash
# Who owns the UART?
sudo fuser -v /dev/ttyAMA0

# MAVProxy logs
journalctl -u mavproxy -e

# Agent logs
journalctl -u mavsphere-agent -e
```

### E) Optional: firewall
```bash
sudo ufw allow 14550/udp
```

This setup survives power loss: systemd keeps **retrying** MAVProxy until the serial device
and FC are up, and your Agent will already be listening on UDP 14600 when MAVProxy starts
delivering packets.
