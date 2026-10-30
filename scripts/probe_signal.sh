#!/bin/bash
# One-off diagnostic (kept for reference): how can a background `farm` be
# stopped? On Windows only `taskkill /F /T` is reliable programmatic stop —
# external SIGTERM/SIGINT reach a background Go process only via console
# events. Requires bin/onvif-simulator(.exe) from `just cli`; override EXE/
# TEMPLATE/OUT through the environment.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXE="${EXE:-$ROOT/bin/onvif-simulator.exe}"
TEMPLATE="${TEMPLATE:-$ROOT/farm-template.json}"
OUT="${OUT:-${TMPDIR:-/tmp}/farmprobe}"
HTTP_BASE=19480
RTSP_BASE=19954

# kill_tree force-kills a pid and its descendants.
kill_tree() {
  local pid=$1
  if command -v taskkill >/dev/null 2>&1; then
    MSYS_NO_PATHCONV=1 taskkill /F /T /PID "$pid" 2>/dev/null || true
  else
    kill -9 -- "-$pid" 2>/dev/null || kill -9 "$pid" 2>/dev/null || true
  fi
}

rm -rf "$OUT"; mkdir -p "$OUT"

check_port() {
  local p=$1
  if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then exec 3>&-; exec 3<&-; echo "OPEN"; else echo "CLOSED"; fi
}

run_farm() {
  rm -rf "$OUT"; mkdir -p "$OUT"
  "$EXE" farm -config "$TEMPLATE" -count 2 -http-base "$HTTP_BASE" -rtsp-base "$RTSP_BASE" -out "$OUT" -ready-timeout 15s >"$OUT/parent.log" 2>&1 &
  echo $!
  for i in $(seq 1 60); do [ -f "$OUT/manifest.json" ] && break; sleep 0.5; done
}

alive() { kill -0 "$1" 2>/dev/null && echo yes || echo no; }

# --- Test A: external SIGTERM ---
FARMPID=$(run_farm)
echo "[A] external SIGTERM  -> farm pid $FARMPID"
MSYS_NO_PATHCONV=1 kill -TERM "$FARMPID" 2>/dev/null
sleep 2
echo "    parent alive after TERM: $(alive "$FARMPID")"
echo "    ports after TERM: http0=$(check_port $HTTP_BASE) http1=$(check_port $((HTTP_BASE+1)))"
# cleanup for next test
kill_tree "$FARMPID"
sleep 1

# --- Test B: external SIGINT ---
FARMPID=$(run_farm)
echo "[B] external SIGINT   -> farm pid $FARMPID"
MSYS_NO_PATHCONV=1 kill -INT "$FARMPID" 2>/dev/null
sleep 2
echo "    parent alive after INT: $(alive "$FARMPID")"
echo "    ports after INT: http0=$(check_port $HTTP_BASE) http1=$(check_port $((HTTP_BASE+1)))"
kill_tree "$FARMPID"
sleep 1

# --- Test C: forced tree kill (reliable programmatic stop) ---
FARMPID=$(run_farm)
echo "[C] taskkill /F /T    -> farm pid $FARMPID"
echo "    ports before: http0=$(check_port $HTTP_BASE) http1=$(check_port $((HTTP_BASE+1)))"
MSYS_NO_PATHCONV=1 taskkill /F /T /PID "$FARMPID" 2>/dev/null
sleep 2
echo "    parent alive after tree-kill: $(alive "$FARMPID")"
echo "    ports after tree-kill: http0=$(check_port $HTTP_BASE) http1=$(check_port $((HTTP_BASE+1)))"

echo "=== summary ==="
echo "External SIGTERM/SIGINT reach a background Go process on Windows only via console events;"
echo "reliable programmatic stop is taskkill /F /T (force tree kill)."
