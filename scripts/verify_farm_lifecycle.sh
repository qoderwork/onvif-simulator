#!/bin/bash
# Verify `farm` full lifecycle: N-instance launch, both ports open, then a
# graceful SIGTERM tears down all children.
#
# Requires `just cli` to have produced bin/onvif-simulator(.exe). Override
# EXE/TEMPLATE/OUT through the environment to point at other artifacts.
#
# NOTE on Windows/MinGW: MSYS `$!` reports a phantom PID that native
# `taskkill` cannot resolve. Use `kill -TERM <msys-pid>` for graceful stop
# (MSYS maps it to the real process), or read real PIDs from manifest.json
# for `taskkill /F /PID <manifest-pid>`.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXE="${EXE:-$ROOT/bin/onvif-simulator.exe}"
TEMPLATE="${TEMPLATE:-$ROOT/farm-template.json}"
OUT="${OUT:-${TMPDIR:-/tmp}/farmlife}"
HTTP_BASE="${HTTP_BASE:-19280}"
RTSP_BASE="${RTSP_BASE:-19754}"

# kill_tree force-kills a pid and its descendants: taskkill on Windows, a
# process-group kill elsewhere (farm puts each child in its own group).
kill_tree() {
  local pid=$1
  if command -v taskkill >/dev/null 2>&1; then
    MSYS_NO_PATHCONV=1 taskkill /F /T /PID "$pid" 2>/dev/null || true
  else
    kill -9 -- "-$pid" 2>/dev/null || kill -9 "$pid" 2>/dev/null || true
  fi
}

rm -rf "$OUT"
mkdir -p "$OUT"
PARENTLOG="$OUT/farm-parent.log"

"$EXE" farm -config "$TEMPLATE" -count 2 -http-base "$HTTP_BASE" -rtsp-base "$RTSP_BASE" -out "$OUT" -ready-timeout 15s -log-level info >"$PARENTLOG" 2>&1 &
FARMPID=$!

for i in $(seq 1 60); do
  if [ -f "$OUT/manifest.json" ]; then break; fi
  sleep 0.5
done

check_port() {
  local p=$1
  if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
    exec 3>&-; exec 3<&-; echo "OPEN"
  else
    echo "CLOSED"
  fi
}

echo "instance 0 http $(check_port "$HTTP_BASE")  rtsp $(check_port "$RTSP_BASE")"
echo "instance 1 http $(check_port $((HTTP_BASE+1)))  rtsp $(check_port $((RTSP_BASE+1)))"

# Graceful shutdown: MSYS `kill -TERM` maps its PID view to the real process
# (native taskkill cannot, and SIGINT is ignored by a background Go process).
echo "--- sending SIGTERM to farm parent ($FARMPID) ---"
MSYS_NO_PATHCONV=1 kill -TERM "$FARMPID" 2>/dev/null || MSYS_NO_PATHCONV=1 kill -INT "$FARMPID"
sleep 2

if kill -0 "$FARMPID" 2>/dev/null; then
  echo "parent still alive after SIGTERM (unexpected)"
else
  echo "parent exited after SIGTERM"
fi

echo "after shutdown, instance 0 http $(check_port "$HTTP_BASE")  rtsp $(check_port "$RTSP_BASE")"
echo "after shutdown, instance 1 http $(check_port $((HTTP_BASE+1)))  rtsp $(check_port $((RTSP_BASE+1)))"

echo "=== verdict ==="
P0=$(check_port "$HTTP_BASE"); P1=$(check_port $((HTTP_BASE+1)))
if [ "$P0" = "CLOSED" ] && [ "$P1" = "CLOSED" ]; then
  echo "LIFECYCLE: PASS (both children reaped on graceful shutdown)"
else
  echo "LIFECYCLE: FAIL (ports $P0 / $P1)"
  # Forced cleanup using real PIDs from the manifest.
  for pid in $(grep -oP '"pid":\s*\K\d+' "$OUT/manifest.json"); do
    kill_tree "$pid"
  done
  kill_tree "$FARMPID"
fi
