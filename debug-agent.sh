#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   ./debug-agent.sh [path-to-binary] [extra args...]
#
# Examples:
#   ./debug-agent.sh
#   ./debug-agent.sh bin/mavsphere-agent-2025-12-06_153020
#   ./debug-agent.sh bin/mavsphere-agent --some-flag

# Default binary
BIN="bin/mavsphere-agent"

# If first arg looks like a file, treat it as the binary path
if [[ $# -gt 0 && -x "$1" ]]; then
  BIN="$1"
  shift
fi

TS="$(date -u +%Y-%m-%d_%H%M%S)"
LOG_FILE="agent.debug.${TS}.log"

# Core GStreamer / WebRTC debug categories for janusvrwebrtcsink
export GST_DEBUG="webrtc*:6,janus*:6,rtp*:5,dtls*:5,srtp*:5,libnice*:5"
export GST_DEBUG_NO_COLOR=1
export GST_DEBUG_FILE=""

echo "Running: ${BIN} $*"
echo "GST_DEBUG=${GST_DEBUG}"
echo "Logging to: ${LOG_FILE}"

# Run agent and tee stdout+stderr to log
"${BIN}" "$@" 2>&1 | tee "${LOG_FILE}"
