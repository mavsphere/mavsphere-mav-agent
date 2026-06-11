#!/usr/bin/env bash
set -euo pipefail

# Simple installer for:
# - Docker CE + Compose plugin
# - MAVProxy
# - systemd units: mavproxy.service, mavsphere-agent.service
# - docker-compose.yml for mavsphere agent

# -----------------------------
# Tunables
# -----------------------------
AGENT_ROOT="/opt/mavsphere"
AGENT_DATA="/etc/mavsphere"
AGENT_USER="root"   # or "ubuntu" / "con-pi" if you want non-root + docker group

MAVPROXY_MASTER="/dev/ttyAMA0,57600"
MAVPROXY_OUT_GCS="udp:0.0.0.0:14550"
MAVPROXY_OUT_AGENT="udp:127.0.0.1:14600"
MAVPROXY_OUT_SPARE="udp:127.0.0.1:14601"

AGENT_UI_PORT=8484

# Runtime image for the agent (Docker Hub)
AGENT_IMAGE="mavsphere/agent:latest"

# -----------------------------
# Helpers
# -----------------------------
need_cmd() {
  command -v "$1" >/dev/null 2>&1
}

echo "[0/6] Pre-flight checks..."
if [ "$(id -u)" -ne 0 ]; then
  echo "This script must run as root (sudo ./setup_pi.sh)"
  exit 1
fi

echo "[1/6] Installing base packages (safe to re-run)..."
apt-get update
apt-get install -y \
  ca-certificates \
  curl \
  gnupg \
  lsb-release \
  python3-pip

echo "[2/6] Installing / checking Docker CE + Compose plugin..."
if ! need_cmd docker; then
  install -m 0755 -d /etc/apt/keyrings
  if [ ! -f /etc/apt/keyrings/docker.gpg ]; then
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
      | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
    chmod a+r /etc/apt/keyrings/docker.gpg
  fi

  codename="$(. /etc/os-release && echo "$VERSION_CODENAME")"

  cat >/etc/apt/sources.list.d/docker.list <<EOF
deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/ubuntu $codename stable
EOF

  apt-get update
  apt-get install -y \
    docker-ce \
    docker-ce-cli \
    containerd.io \
    docker-buildx-plugin \
    docker-compose-plugin

  systemctl enable --now docker
else
  echo "Docker already installed, ensuring service is running..."
  systemctl enable --now docker || true
fi

echo "[3/6] Installing MAVProxy (safe to re-run)..."
if ! need_cmd mavproxy.py; then
  pip3 install MAVProxy
  # Ensure it's on PATH for systemd service
  if [ ! -f /usr/local/bin/mavproxy.py ]; then
    MAVPROXY_BIN="$(python3 -m site --user-base)/bin/mavproxy.py"
    if [ -f "$MAVPROXY_BIN" ]; then
      ln -sf "$MAVPROXY_BIN" /usr/local/bin/mavproxy.py
    fi
  fi
else
  echo "MAVProxy already installed, skipping pip install."
fi

echo "[4/6] Creating directories and default config (idempotent)..."
mkdir -p "$AGENT_ROOT"
mkdir -p "$AGENT_DATA"

# Default agent config.json (only create if missing)
if [ ! -f "$AGENT_DATA/config.json" ]; then
  cat > "$AGENT_DATA/config.json" <<EOF
{
  "mavId": "1",
  "backendWsUrl": "wss://mavsphere.com/api/ws/agent",
  "backendUrl": "https://mavsphere.com",
  "janusUrl": "wss://mavsphere.com/janus",
  "username": "",
  "password": "",
  "mavlinkConnection": "udp:127.0.0.1:14600",
  "agentGcsId": 255,
  "allowControl": false
}
EOF
  echo "Wrote default $AGENT_DATA/config.json"
else
  echo "$AGENT_DATA/config.json already exists, leaving as-is."
fi

echo "[4b/6] Ensuring agent image is present..."
if ! docker image inspect "$AGENT_IMAGE" >/dev/null 2>&1; then
  echo "Pulling $AGENT_IMAGE ..."
  docker pull "$AGENT_IMAGE"
else
  echo "Image $AGENT_IMAGE already present, skipping pull."
fi

echo "[5/6] Writing docker-compose.yml for mavsphere-agent (only if missing)..."
COMPOSE_FILE="$AGENT_ROOT/docker-compose.yml"

if [ ! -f "$COMPOSE_FILE" ]; then
  cat > "$COMPOSE_FILE" <<EOF
services:
  mavsphere-agent:
    image: $AGENT_IMAGE
    container_name: mavsphere-agent
    restart: always

    # Use host network so UDP 127.0.0.1:14600 from MAVProxy reaches container
    network_mode: host

    # Full device access (multiple cameras, audio, etc.)
    privileged: true

    # Bind-mount /dev so host devices are visible
    volumes:
      - /dev:/dev
      - $AGENT_DATA:/etc/mavsphere

    # Agent UI runs on 0.0.0.0:$AGENT_UI_PORT inside container
    # With host network, this is reachable at http://<pi-ip>:$AGENT_UI_PORT
    # ports: not needed when network_mode: host
    #   - "${AGENT_UI_PORT}:${AGENT_UI_PORT}"
EOF
  echo "Wrote $COMPOSE_FILE"
else
  echo "$COMPOSE_FILE already exists, leaving as-is."
  echo "If you change AGENT_IMAGE or settings, edit it manually."
fi

echo "[6/6] Creating / updating systemd services (skip if present)..."

MAVPROXY_UNIT="/etc/systemd/system/mavproxy.service"
AGENT_UNIT="/etc/systemd/system/mavsphere-agent.service"

echo " - mavproxy.service"
if [ ! -f "$MAVPROXY_UNIT" ]; then
  cat > "$MAVPROXY_UNIT" <<EOF
[Unit]
Description=MAVProxy forwarding for FC
After=network.target
Wants=network-online.target

[Service]
Type=simple
User=$AGENT_USER
ExecStart=/usr/local/bin/mavproxy.py \\
  --master=$MAVPROXY_MASTER \\
  --out=$MAVPROXY_OUT_GCS \\
  --out=$MAVPROXY_OUT_AGENT \\
  --out=$MAVPROXY_OUT_SPARE
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
  echo "Created $MAVPROXY_UNIT"
else
  echo "$MAVPROXY_UNIT already exists, leaving as-is."
fi

echo " - mavsphere-agent.service"
if [ ! -f "$AGENT_UNIT" ]; then
  cat > "$AGENT_UNIT" <<EOF
[Unit]
Description=MAVSphere Agent (docker compose)
After=docker.service mavproxy.service
Requires=docker.service
Wants=mavproxy.service

[Service]
Type=oneshot
User=$AGENT_USER
WorkingDirectory=$AGENT_ROOT
ExecStart=/usr/bin/docker compose up -d
ExecStop=/usr/bin/docker compose down
RemainAfterExit=yes
TimeoutStartSec=0

[Install]
WantedBy=multi-user.target
EOF
  echo "Created $AGENT_UNIT"
else
  echo "$AGENT_UNIT" already exists, leaving as-is."
fi

echo "Reloading systemd and enabling services (idempotent)..."
systemctl daemon-reload
systemctl enable mavproxy.service
systemctl enable mavsphere-agent.service

echo "Starting services..."
systemctl restart mavproxy.service
systemctl restart mavsphere-agent.service

echo
echo "Done."
echo "Check MAVProxy:   systemctl status mavproxy.service"
echo "Check Agent:      systemctl status mavsphere-agent.service"
echo "Agent UI:         http://<pi-ip>:$AGENT_UI_PORT/"
echo
echo "If MAVLink wiring differs (not $MAVPROXY_MASTER), edit:"
echo "  $MAVPROXY_UNIT"
echo "then run: systemctl daemon-reload && systemctl restart mavproxy.service"
