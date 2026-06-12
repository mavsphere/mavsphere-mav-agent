# syntax=docker/dockerfile:1.6
#
# mavsphere-agent
#
# Builds on top of ghcr.io/mavsphere/layout-agent-base which already contains:
#   - GStreamer 1.26.x (built from source) in /opt/gst
#   - libnice (built from source) with GStreamer plugin
#   - gst-plugins-rs WebRTC + RTP Rust plugins
#
# This means the Go agent build is the only thing that runs in CI — no
# 30-minute GStreamer/Rust compile, no flaky cargo-c downloads.
#
# To rebuild the base (only needed when GStreamer/libnice/Rust plugin versions change):
#   cd mavsphere-layout-agent && make docker-buildx-base

ARG BASE_IMAGE=ghcr.io/mavsphere/layout-agent-base:latest

# ============================================================
# Stage 1: build agent (Go)
# ============================================================
FROM golang:1.23-bookworm AS agent-builder

WORKDIR /src

# These are provided by buildx automatically
ARG TARGETOS
ARG TARGETARCH

# 1) Copy module files first so `go mod download` is cached
COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

# 2) Copy only source directories (avoid COPY . .)
COPY cmd ./cmd
COPY pkg ./pkg
COPY internal ./internal

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" \
    -o /out/mavsphere-agent ./cmd/mavagent

# ============================================================
# Stage 2: runtime — extend the shared GStreamer base
# ============================================================
FROM ${BASE_IMAGE}
ARG DEBIAN_FRONTEND=noninteractive

# Install runtime deps not already in the base
RUN set -eux; \
  apt-get update; \
  apt-get install -y --no-install-recommends \
    libgudev-1.0-0 \
    libjpeg-turbo8 libpng16-16 libvpx9 libx264-164 libx265-199 \
    libopus0 libvorbis0a libvorbisenc2 libtheora0 libspeex1 libmp3lame0 \
    libasound2t64 v4l-utils alsa-utils \
  ; \
  rm -rf /var/lib/apt/lists/*

COPY --from=agent-builder /out/mavsphere-agent /usr/local/bin/mavsphere-agent

ENV AGENT_CONFIG=/config/config.json

# Runtime fail-fast: missing shared libs + required elements
RUN set -eux; \
  rm -f /root/.cache/gstreamer-1.0/registry.*.bin || true; \
  \
  echo "---- shared-lib dependency checks (must be clean) ----"; \
  nice_sos="$(find /opt/gst/lib/gstreamer-1.0 -maxdepth 1 -type f -name '*nice*.so' -print)"; \
  test -n "$nice_sos"; \
  for so in $nice_sos; do \
    echo "ldd $so"; \
    ldd "$so" | grep "not found" && exit 1 || true; \
  done; \
  \
  for so in /opt/gst/lib/gstreamer-1.0/libgstrswebrtc.so /opt/gst/lib/gstreamer-1.0/libgstrsrtp.so; do \
    test -f "$so"; \
    echo "ldd $so"; \
    ldd "$so" | grep "not found" && exit 1 || true; \
  done; \
  \
  echo "---- gst-inspect required elements ----"; \
  gst-inspect-1.0 nicesrc   >/dev/null; \
  gst-inspect-1.0 nicesink  >/dev/null; \
  gst-inspect-1.0 webrtcbin >/dev/null; \
  \
  echo "---- gst-inspect app-specific elements ----"; \
  gst-inspect-1.0 janusvrwebrtcsink >/dev/null; \
  gst-inspect-1.0 rtpgccbwe        >/dev/null

RUN mkdir -p /opt/mavsphere-agent /config

RUN cat > /opt/mavsphere-agent/default.config.json <<'EOF'
{
  "mavId": "1",
  "backendWsUrl": "wss://mavsphere.com/api/ws/agent",
  "backendUrl": "https://mavsphere.com",
  "janusUrl": "wss://mavsphere.com/janus",
  "username": "your@email.com",
  "password": "password",
  "mavlinkConnection": "udp:0.0.0.0:14600",
  "agentGcsId": 252,
  "allowControl": true,
  "videoWidth": 640,
  "videoHeight": 360,
  "videoFps": 30
}
EOF

RUN cat > /entrypoint.sh <<'EOF'
#!/bin/sh
set -e
CONFIG_PATH="${AGENT_CONFIG:-/config/config.json}"
if [ ! -f "$CONFIG_PATH" ]; then
  echo "[entrypoint] Initialising config at $CONFIG_PATH"
  mkdir -p "$(dirname "$CONFIG_PATH")"
  cp /opt/mavsphere-agent/default.config.json "$CONFIG_PATH"
fi

echo "[entrypoint] GStreamer version:"
gst-launch-1.0 --version || true

rm -f /root/.cache/gstreamer-1.0/registry.*.bin 2>/dev/null || true
gst-inspect-1.0 janusvrwebrtcsink >/dev/null 2>&1 || echo "[entrypoint] WARNING: janusvrwebrtcsink not found"
gst-inspect-1.0 rtpgccbwe        >/dev/null 2>&1 || echo "[entrypoint] WARNING: rtpgccbwe not found"
gst-inspect-1.0 nicesrc          >/dev/null 2>&1 || echo "[entrypoint] WARNING: nicesrc not found (libnice-gstreamer plugin missing)"
gst-inspect-1.0 nicesink         >/dev/null 2>&1 || echo "[entrypoint] WARNING: nicesink not found (libnice-gstreamer plugin missing)"
gst-inspect-1.0 webrtcbin        >/dev/null 2>&1 || echo "[entrypoint] WARNING: webrtcbin not found"

exec /usr/local/bin/mavsphere-agent "$@"
EOF
RUN chmod +x /entrypoint.sh

EXPOSE 8090
WORKDIR /config
ENTRYPOINT ["/entrypoint.sh"]
