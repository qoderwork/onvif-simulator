#!/bin/bash
# Verify the Windows Job Object orphan guarantee: hard-kill the farm parent
# (as Task Manager / kill -9 would) and confirm NO child survives.
# Requires bin/onvif-simulator(.exe) from `just cli`; override EXE/TEMPLATE/OUT
# through the environment. Windows-only: it asserts on the job object, which
# has no Unix counterpart (there Pdeathsig + checkFarmParent cover orphans).
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXE="${EXE:-$ROOT/bin/onvif-simulator.exe}"
TEMPLATE="${TEMPLATE:-$ROOT/farm-template.json}"
OUT="${OUT:-${TMPDIR:-/tmp}/farmjob}"
HTTP_BASE=19880
RTSP_BASE=20354

rm -rf "$OUT"; mkdir -p "$OUT"

check_port() {
  local p=$1
  if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then exec 3>&-; exec 3<&-; echo OPEN; else echo CLOSED; fi
}

"$EXE" farm -config "$TEMPLATE" -count 2 -http-base "$HTTP_BASE" -rtsp-base "$RTSP_BASE" -out "$OUT" -ready-timeout 15s >"$OUT/parent.log" 2>&1 &
MSYSPID=$!
for i in $(seq 1 80); do [ -f "$OUT/manifest.json" ] && break; sleep 0.5; done

FPPID=$(grep -oP '"parent_pid":\s*\K\d+' "$OUT/manifest.json" | head -1)
echo "farm parent_pid (REAL windows pid from manifest) = $FPPID"
echo "ports before hard-kill: http0=$(check_port $HTTP_BASE) http1=$(check_port $((HTTP_BASE+1))) rtsp0=$(check_port $RTSP_BASE)"

# HARD kill the parent only - exactly what made children orphans before.
echo "--- taskkill /F /PID $FPPID (simulates Task Manager / kill -9) ---"
MSYS_NO_PATHCONV=1 taskkill /F /PID "$FPPID"; echo "taskkill exit=$?"
sleep 3

kill -0 "$MSYSPID" 2>/dev/null && echo "parent alive: yes" || echo "parent alive: no"
echo "ports AFTER hard-kill: http0=$(check_port $HTTP_BASE) http1=$(check_port $((HTTP_BASE+1))) rtsp0=$(check_port $RTSP_BASE) rtsp1=$(check_port $((RTSP_BASE+1)))"

echo "=== verdict ==="
P0=$(check_port $HTTP_BASE); P1=$(check_port $((HTTP_BASE+1)))
if [ "$P0" = "CLOSED" ] && [ "$P1" = "CLOSED" ]; then
  echo "JOB OBJECT ORPHAN PROTECTION: PASS (no orphans after hard kill of parent)"
else
  echo "JOB OBJECT: FAIL - children survived (ports $P0 / $P1)"
  # cleanup
  for pid in $(grep -oP '"pid":\s*\K\d+' "$OUT/manifest.json"); do MSYS_NO_PATHCONV=1 taskkill /F /PID "$pid" 2>/dev/null; done
fi
