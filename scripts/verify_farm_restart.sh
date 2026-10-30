#!/bin/bash
# Deterministic verification of `farm` crash-restart semantics.
#
# Requires `just cli` to have produced bin/onvif-simulator(.exe). Override
# EXE/TEMPLATE/OUT through the environment to point at other artifacts.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXE="${EXE:-$ROOT/bin/onvif-simulator.exe}"
TEMPLATE="${TEMPLATE:-$ROOT/farm-template.json}"
OUT="${OUT:-${TMPDIR:-/tmp}/farmtest}"
HTTP_PORT="${HTTP_PORT:-19080}"
RTSP_PORT="${RTSP_PORT:-19554}"

# kill_pid force-kills one pid: taskkill on Windows, kill elsewhere.
kill_pid() {
  local pid=$1
  if command -v taskkill >/dev/null 2>&1; then
    MSYS_NO_PATHCONV=1 taskkill /F /PID "$pid" 2>/dev/null || true
  else
    kill -9 "$pid" 2>/dev/null || true
  fi
}

# kill_tree force-kills a pid and its descendants.
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

"$EXE" farm -config "$TEMPLATE" -count 1 -http-base "$HTTP_PORT" -rtsp-base "$RTSP_PORT" -out "$OUT" -ready-timeout 15s -log-level info >"$PARENTLOG" 2>&1 &
FARMPID=$!

# Wait for manifest to appear (farm writes it after readiness poll).
for i in $(seq 1 60); do
  if [ -f "$OUT/manifest.json" ]; then break; fi
  sleep 0.5
done

echo "=== manifest after launch ==="
cat "$OUT/manifest.json"

PID1=$(grep -oP '"pid":\s*\K\d+' "$OUT/manifest.json" | head -1)
echo "child pid1 = $PID1"

check_port() {
  local p=$1
  if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
    exec 3>&-; exec 3<&-; echo "OPEN"
  else
    echo "CLOSED"
  fi
}
echo "port $HTTP_PORT before kill: $(check_port "$HTTP_PORT")"

# Force-kill the CHILD only; supervisor should respawn it.
# On Windows, MSYS_NO_PATHCONV=1 is REQUIRED: Git Bash otherwise rewrites /F
# and /PID as Windows paths (e.g. F:/), so taskkill never runs.
echo "--- kill child $PID1 ---"
kill_pid "$PID1"

sleep 3

echo "=== manifest after kill (expect new pid) ==="
cat "$OUT/manifest.json"
PID2=$(grep -oP '"pid":\s*\K\d+' "$OUT/manifest.json" | head -1)
echo "child pid2 = $PID2"
echo "port $HTTP_PORT after restart: $(check_port "$HTTP_PORT")"

echo "=== parent log (restart evidence) ==="
grep -i "restart\|exited" "$PARENTLOG" || echo "(no restart line found)"

echo "=== verdict ==="
if [ "$PID1" != "$PID2" ] && [ "$(check_port "$HTTP_PORT")" = "OPEN" ]; then
  echo "CRASH-RESTART: PASS (pid $PID1 -> $PID2, port reopened)"
else
  echo "CRASH-RESTART: FAIL (pid1=$PID1 pid2=$PID2)"
fi

# Cleanup the whole process tree.
kill_tree "$FARMPID"
echo "cleanup done"
