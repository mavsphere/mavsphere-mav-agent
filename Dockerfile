# syntax=docker/dockerfile:1.6

ARG GST_VERSION=1.26.9
ARG MESON_VERSION=1.4.2
ARG GST_PLUGINS_RS_REF=0.14.4
ARG LIBNICE_VERSION=0.1.23

# ============================================================
# Stage 1: build GStreamer (from source) into /opt/gst
#         then build libnice (from source) WITH its GStreamer plugin
#         so we get nicesrc/nicesink
# ============================================================
FROM ubuntu:24.04 AS media-builder
ARG DEBIAN_FRONTEND=noninteractive
ARG GST_VERSION
ARG MESON_VERSION
ARG LIBNICE_VERSION

RUN set -eux; \
  apt-get update; \
  apt-get install -y --no-install-recommends \
    ca-certificates curl xz-utils \
    build-essential pkg-config ninja-build \
    python3 python3-pip python3-setuptools \
    bison flex gettext git \
    libglib2.0-dev liborc-0.4-dev \
    libssl-dev libffi-dev libmount-dev libpcre2-dev zlib1g-dev \
    libasound2-dev libjpeg-turbo8-dev \
    libopus-dev libvpx-dev libx264-dev libx265-dev \
    libvorbis-dev libtheora-dev libspeex-dev libmp3lame-dev \
    libsrtp2-dev libdrm-dev libunwind-dev libv4l-dev \
    libsoup-3.0-dev \
    # build-time libnice headers (libnice itself will be built from source below)
    libnice-dev \
  ; \
  rm -rf /var/lib/apt/lists/*

# Meson >= 1.4 required by GStreamer 1.26.x (Ubuntu 24.04 ships Meson 1.3.x)
RUN set -eux; \
  curl -fsSL -o /tmp/meson.tar.gz "https://github.com/mesonbuild/meson/releases/download/${MESON_VERSION}/meson-${MESON_VERSION}.tar.gz"; \
  python3 -m pip install --break-system-packages /tmp/meson.tar.gz; \
  meson --version

WORKDIR /build

# ---- Build gstreamer + plugins into /opt/gst ----
RUN set -eux; \
  export PREFIX=/opt/gst; \
  export PATH="$PREFIX/bin:$PATH"; \
  export LD_LIBRARY_PATH="$PREFIX/lib:${LD_LIBRARY_PATH:-}"; \
  # include multiarch pkg-config dirs (harmless if empty; helps some buildx cases)
  export PKG_CONFIG_PATH="$PREFIX/lib/pkgconfig:$PREFIX/lib/aarch64-linux-gnu/pkgconfig:$PREFIX/lib/x86_64-linux-gnu/pkgconfig:$PREFIX/share/pkgconfig:${PKG_CONFIG_PATH:-}"; \
  \
  for mod in gstreamer gst-plugins-base gst-plugins-good gst-plugins-bad gst-plugins-ugly; do \
    echo "===== BUILDING $mod ====="; \
    curl -fsSL -o "${mod}.tar.xz" "https://gstreamer.freedesktop.org/src/${mod}/${mod}-${GST_VERSION}.tar.xz"; \
    tar -xf "${mod}.tar.xz"; \
    cd "${mod}-${GST_VERSION}"; \
    \
    echo "PKG_CONFIG_PATH=$PKG_CONFIG_PATH"; \
    pkg-config --version; \
    (pkg-config --modversion gstreamer-1.0 && pkg-config --variable=pluginsdir gstreamer-1.0) || true; \
    \
    if [ "$mod" = "gst-plugins-bad" ]; then \
      meson setup build \
        --prefix="$PREFIX" \
        --libdir=lib \
        -Ddefault_library=shared \
        -Dtests=disabled \
        -Dwebrtc=enabled; \
      echo "---- meson configure (grep webrtc/dtls/srtp) ----"; \
      meson configure build | egrep -i '(webrtc|dtls|srtp)' || true; \
    elif [ "$mod" = "gst-plugins-ugly" ]; then \
      meson setup build \
        --prefix="$PREFIX" \
        --libdir=lib \
        -Ddefault_library=shared \
        -Dtests=disabled \
        -Dgpl=enabled \
        -Dx264=enabled; \
      echo "---- meson configure (grep gpl/x264) ----"; \
      meson configure build | egrep -i '(gpl|x264)' || true; \
    else \
      meson setup build \
        --prefix="$PREFIX" \
        --libdir=lib \
        -Ddefault_library=shared \
        -Dtests=disabled; \
    fi; \
    \
    ninja -C build; \
    ninja -C build install; \
    \
    if [ "$mod" = "gstreamer" ]; then \
      echo "---- verifying gstreamer-1.0.pc ----"; \
      find "$PREFIX" -name 'gstreamer-1.0.pc' -o -name 'gstreamer-plugins-base-1.0.pc' || true; \
      pkg-config --print-errors --modversion gstreamer-1.0; \
      pkg-config --variable=pluginsdir gstreamer-1.0; \
    fi; \
    \
    cd /build; \
  done

# ---- Build libnice from source WITH GStreamer plugin enabled ----
# This is what provides nicesrc/nicesink.
RUN set -eux; \
  export PREFIX=/opt/gst; \
  export PATH="$PREFIX/bin:$PATH"; \
  export LD_LIBRARY_PATH="$PREFIX/lib:${LD_LIBRARY_PATH:-}"; \
  export PKG_CONFIG_PATH="$PREFIX/lib/pkgconfig:$PREFIX/lib/aarch64-linux-gnu/pkgconfig:$PREFIX/lib/x86_64-linux-gnu/pkgconfig:$PREFIX/share/pkgconfig:${PKG_CONFIG_PATH:-}"; \
  \
  curl -fsSL -o "libnice.tar.gz" "https://nice.freedesktop.org/releases/libnice-${LIBNICE_VERSION}.tar.gz"; \
  tar -xzf libnice.tar.gz; \
  cd "libnice-${LIBNICE_VERSION}"; \
  meson setup build \
    --prefix="$PREFIX" \
    --libdir=lib \
    -Dtests=disabled \
    -Dgstreamer=enabled; \
  meson configure build | egrep -i '(gstreamer|gst)' || true; \
  ninja -C build; \
  ninja -C build install

# ---- Fail-fast (filename-agnostic) for "nice" plugin + deps ----
RUN set -eux; \
  export PATH="/opt/gst/bin:$PATH"; \
  export LD_LIBRARY_PATH="/opt/gst/lib:${LD_LIBRARY_PATH:-}"; \
  export GST_PLUGIN_SYSTEM_PATH_1_0=/opt/gst/lib/gstreamer-1.0; \
  rm -f /root/.cache/gstreamer-1.0/registry.*.bin || true; \
  \
  echo "---- installed plugins (grep nice/webrtc) ----"; \
  ls -l /opt/gst/lib/gstreamer-1.0/ | egrep -i 'nice|webrtc' || true; \
  \
  echo "---- required elements ----"; \
  gst-inspect-1.0 webrtcbin >/dev/null; \
  gst-inspect-1.0 nicesrc  >/dev/null; \
  gst-inspect-1.0 nicesink >/dev/null; \
  gst-inspect-1.0 h264parse >/dev/null; \
  gst-inspect-1.0 vp8enc >/dev/null; \
  gst-inspect-1.0 jpegdec >/dev/null; \
  gst-inspect-1.0 x264enc >/dev/null; \
  \
  echo "---- locate 'nice' plugin shared objects ----"; \
  nice_sos="$(find /opt/gst/lib/gstreamer-1.0 -maxdepth 1 -type f -name '*nice*.so' -print)"; \
  test -n "$nice_sos"; \
  echo "$nice_sos"; \
  \
  echo "---- assert 'nice' plugin shared objects have no missing deps ----"; \
  for so in $nice_sos; do \
    echo "ldd $so"; \
    ldd "$so" | (! grep -q "not found"); \
  done

# ============================================================
# Stage 2: build Rust GStreamer plugins (gst-plugins-rs) against /opt/gst
#
# NOTE: apt-get runs BEFORE the large COPY --from=media-builder to avoid
# a known buildkit/QEMU bug where a big overlay COPY corrupts GPG
# signature verification on arm64 emulated builds.
# ============================================================
FROM ubuntu:24.04 AS rsplugins-builder
ARG DEBIAN_FRONTEND=noninteractive
ARG GST_PLUGINS_RS_REF

# Install system packages first (before COPY) to sidestep QEMU/overlay GPG bug
RUN set -eux; \
  apt-get update; \
  apt-get install -y --no-install-recommends \
    ca-certificates git curl \
    build-essential pkg-config \
    libssl-dev libglib2.0-dev liborc-0.4-dev libunwind-dev \
  ; \
  rm -rf /var/lib/apt/lists/*

RUN set -eux; \
  curl -fsSL https://sh.rustup.rs | sh -s -- -y; \
  . "$HOME/.cargo/env"; \
  cargo install --locked cargo-c

# Now bring in /opt/gst (after apt is done)
COPY --from=media-builder /opt/gst /opt/gst

WORKDIR /build
ENV PKG_CONFIG_PATH=/opt/gst/lib/pkgconfig:/opt/gst/lib/aarch64-linux-gnu/pkgconfig:/opt/gst/lib/x86_64-linux-gnu/pkgconfig:/opt/gst/share/pkgconfig:/usr/lib/pkgconfig:/usr/share/pkgconfig
ENV PATH=/opt/gst/bin:$PATH
ENV LD_LIBRARY_PATH=/opt/gst/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}

RUN set -eux; \
  git clone https://gitlab.freedesktop.org/gstreamer/gst-plugins-rs.git; \
  cd gst-plugins-rs; \
  git fetch --tags; \
  (git checkout "refs/tags/${GST_PLUGINS_RS_REF}" || git checkout "refs/tags/v${GST_PLUGINS_RS_REF}"); \
  . "$HOME/.cargo/env"; \
  cargo cbuild -p gst-plugin-webrtc --release; \
  cargo cbuild -p gst-plugin-rtp --release; \
  mkdir -p /out-plugins; \
  cp "$(find /build/gst-plugins-rs/target -type f -name 'libgstrswebrtc.so' | head -n1)" /out-plugins/libgstrswebrtc.so; \
  cp "$(find /build/gst-plugins-rs/target -type f -name 'libgstrsrtp.so'   | head -n1)" /out-plugins/libgstrsrtp.so; \
  ls -lh /out-plugins

# ============================================================
# Stage 3: build agent (Go) - optimized for caching
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
# Stage 4: runtime
# ============================================================
FROM ubuntu:24.04
ARG DEBIAN_FRONTEND=noninteractive

RUN set -eux; \
  apt-get update; \
  apt-get install -y --no-install-recommends \
    ca-certificates \
    libglib2.0-0 liborc-0.4-0 libssl3 libgcc-s1 libstdc++6 zlib1g \
    libgudev-1.0-0 libunwind8 libdrm2 libsrtp2-1 \
    libjpeg-turbo8 libpng16-16 libvpx9 libx264-164 libx265-199 \
    libopus0 libvorbis0a libvorbisenc2 libtheora0 libspeex1 libmp3lame0 \
    libasound2t64 v4l-utils alsa-utils \
    # keep this (libnice runtime); libnice is also installed into /opt/gst from builder
    libnice10 \
  ; \
  rm -rf /var/lib/apt/lists/*

COPY --from=media-builder /opt/gst /opt/gst
COPY --from=rsplugins-builder /out-plugins/libgstrswebrtc.so /opt/gst/lib/gstreamer-1.0/
COPY --from=rsplugins-builder /out-plugins/libgstrsrtp.so   /opt/gst/lib/gstreamer-1.0/
COPY --from=agent-builder /out/mavsphere-agent /usr/local/bin/mavsphere-agent

ENV PATH=/opt/gst/bin:$PATH
ENV LD_LIBRARY_PATH=/opt/gst/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}
ENV GST_PLUGIN_SYSTEM_PATH_1_0=/opt/gst/lib/gstreamer-1.0
ENV GST_PLUGIN_SCANNER=/opt/gst/libexec/gstreamer-1.0/gst-plugin-scanner
ENV GST_PLUGIN_PATH=/opt/gst/lib/gstreamer-1.0${GST_PLUGIN_PATH:+:$GST_PLUGIN_PATH}
ENV AGENT_CONFIG=/config/config.json

# Runtime fail-fast (filename-agnostic): missing shared libs + required elements
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
