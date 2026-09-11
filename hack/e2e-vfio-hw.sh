#!/usr/bin/env bash
# End-to-end hardware test for vfio-manage. Runs on a Tenstorrent runner VM
# (card on tt-kmd, ephemeral — destroyed after the job, so a failed restore
# can't strand anything durable).
#
# Phases:
#   1. Discover the TT device and dump its identity while on tt-kmd —
#      including subsystem IDs, to answer whether n150/n300 differ in config
#      space (if they do, telemetry-based identity is unnecessary).
#   2. Run vfio-manage: assert the device lands on vfio-pci and the metrics
#      endpoint reports devices_bound and device_info with the real
#      board_type.
#   3. Restart vfio-manage: with the device on vfio-pci (telemetry
#      unreadable) and no persisted state, the board type must come from
#      the PCI subsystem ID alone.
#   4. SIGTERM with --restore-on-exit: assert the device returns to tt-kmd
#      and driver_override is cleared.
set -euo pipefail

VFIO_MANAGE_BIN=${VFIO_MANAGE_BIN:-./bin/vfio-manage}
METRICS_PORT=${METRICS_PORT:-9402}
WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

log() { printf '\n== %s ==\n' "$*"; }
fail() { echo "::error::$*"; exit 1; }

# --- Phase 1: discovery + identity while on tt-kmd -------------------------

log "PCI discovery"
lspci -nn -d 1e52: || true
BDF_SHORT=$(lspci -n -d 1e52: | head -1 | awk '{print $1}')
[ -n "$BDF_SHORT" ] || fail "no Tenstorrent device on this runner"
BDF="0000:${BDF_SHORT}"
SYS="/sys/bus/pci/devices/${BDF}"
DEVICE_ID=$(cat "$SYS/device" | sed 's/^0x//')

log "identity readable on tt-kmd (BDF=$BDF device=$DEVICE_ID)"
CUR_DRIVER=$(basename "$(readlink "$SYS/driver")")
[ "$CUR_DRIVER" = "tenstorrent" ] || fail "device starts on '$CUR_DRIVER', expected tt-kmd"

# tt-kmd puts the telemetry attrs on its class device under an intermediate
# "tenstorrent" dir: <pci dev>/tenstorrent/tenstorrent!<N>/tt_card_type
# (verified on the n150 runner, kmd 2.8.0). The attrs may lag ARC init;
# poll briefly before concluding they're absent.
CARD_TYPE=""
for _ in $(seq 1 12); do
  ATTR=$(ls -d "$SYS"/tenstorrent/tenstorrent!*/tt_card_type 2>/dev/null | head -1 || true)
  if [ -n "$ATTR" ]; then
    CARD_TYPE=$(tr -d '[:space:]' < "$ATTR") && [ -n "$CARD_TYPE" ] && break
  fi
  sleep 5
done
SERIAL=$(cat "$SYS"/tenstorrent/tenstorrent!*/tt_serial 2>/dev/null | tr -d '[:space:]' || true)
echo "tt_card_type=${CARD_TYPE:-<unreadable>} tt_serial=${SERIAL:-<unreadable>}"
if [ -z "$CARD_TYPE" ]; then
  echo "== sysfs layout diagnostics =="
  echo "-- $SYS:"; ls -la "$SYS" || true
  echo "-- /sys/class dirs mentioning tenstorrent:"
  find /sys/class -maxdepth 2 -iname '*tenstorrent*' 2>/dev/null || true
  echo "-- class device attrs:"
  for d in /sys/class/tenstorrent/*; do
    [ -e "$d" ] || continue
    echo "$d -> $(readlink -f "$d")"; ls "$d" || true
  done
  echo "-- kmd version: $(cat /sys/module/tenstorrent/version 2>/dev/null)"
  echo "-- dmesg tail:"; sudo dmesg | grep -i "tenstorrent\|telemetry" | tail -20 || true
  fail "tt_card_type unreadable while on tt-kmd — identity mechanism assumption broken"
fi

# The open question from the PR: does config space already distinguish SKUs?
log "config-space identity (subsystem IDs) — decides if telemetry caching is even needed"
echo "subsystem_vendor=$(cat "$SYS/subsystem_vendor") subsystem_device=$(cat "$SYS/subsystem_device") board_type=$CARD_TYPE"
echo "::notice title=SKU identity::board_type=$CARD_TYPE subsystem_device=$(cat "$SYS/subsystem_device") (compare across runner SKUs)"

# --- vfio prerequisites ------------------------------------------------------

log "IOMMU / vfio setup"
if [ -z "$(ls -A /sys/kernel/iommu_groups 2>/dev/null)" ]; then
  echo "no IOMMU groups — enabling unsafe noiommu mode (test VM)"
  # modprobe params are a no-op if vfio is already loaded; set it via sysfs.
  sudo modprobe vfio || true
  echo Y | sudo tee /sys/module/vfio/parameters/enable_unsafe_noiommu_mode >/dev/null
  cat /sys/module/vfio/parameters/enable_unsafe_noiommu_mode
fi
sudo modprobe vfio-pci || true

# Anything holding /dev/tenstorrent would block the unbind.
sudo fuser -k /dev/tenstorrent/* 2>/dev/null || true

cat > "$WORKDIR/config.yaml" <<EOF
devices:
  - resourceName: tenstorrent.com/e2e-test
    vendorId: "1e52"
    deviceId: ["${DEVICE_ID}"]
EOF

run_daemon() {
  sudo "$VFIO_MANAGE_BIN" \
    --config "$WORKDIR/config.yaml" \
    --bind-interval 5s \
    --metrics-addr ":${METRICS_PORT}" \
    "$@" >"$WORKDIR/daemon.log" 2>&1 &
  DAEMON_PID=$!
}

# Signal the daemon binary directly (not the sudo wrapper — waiting on it
# hung a run for 20 minutes) and poll for exit with a bound.
stop_daemon() { # stop_daemon [signal]
  sudo pkill "-${1:-TERM}" -x vfio-manage 2>/dev/null || true
  for _ in $(seq 1 20); do
    pgrep -x vfio-manage >/dev/null || return 0
    sleep 1
  done
  echo "daemon did not exit after ${1:-TERM}; killing"
  cat "$WORKDIR/daemon.log"
  sudo pkill -KILL -x vfio-manage 2>/dev/null || true
  sleep 1
  return 1
}

wait_driver() { # wait_driver <driver> <timeout_s>
  for _ in $(seq 1 "$2"); do
    [ "$(basename "$(readlink "$SYS/driver" 2>/dev/null)" )" = "$1" ] && return 0
    sleep 1
  done
  return 1
}

# The TT runners route through an HTTP proxy; make sure localhost calls
# never do (curl exit 22 = the proxy answering with an HTTP error).
metrics() { curl -sf --noproxy '*' "http://localhost:${METRICS_PORT}/metrics"; }

# --- Phase 2: bind + identity + metrics -------------------------------------

log "phase 2: vfio-manage binds the device"
run_daemon
wait_driver vfio-pci 30 || { cat "$WORKDIR/daemon.log"; fail "device never bound to vfio-pci"; }
echo "device on vfio-pci"

sleep 2
M=$(metrics) || { cat "$WORKDIR/daemon.log"; fail "metrics endpoint unreachable on :${METRICS_PORT}"; }
echo "$M" | grep 'tt_vfio_devices_bound_total{resource="tenstorrent.com/e2e-test"} 1' \
  || { echo "$M" | grep tt_vfio || true; fail "devices_bound metric wrong"; }
echo "$M" | grep "tt_vfio_device_info" | grep "board_type=\"${CARD_TYPE}\"" \
  || { echo "$M" | grep tt_vfio_device_info || true; fail "device_info missing board_type=${CARD_TYPE}"; }
echo "metrics OK: bound=1, board_type=${CARD_TYPE}"

stop_daemon || fail "phase-2 daemon hung on TERM"

# --- Phase 3: restart — board type from config space alone --------------------

log "phase 3: fresh daemon, device already on vfio-pci — subsystem ID must identify it"
run_daemon
sleep 8
M=$(metrics) || { cat "$WORKDIR/daemon.log"; fail "metrics endpoint unreachable on :${METRICS_PORT}"; }
# No state is persisted anywhere and telemetry is unreadable on vfio-pci, so
# the board type must come purely from config space (subsystem_device).
echo "$M" | grep "tt_vfio_device_info" | grep "board_type=\"${CARD_TYPE}\"" \
  || { echo "$M" | grep tt_vfio_device_info || true; cat "$WORKDIR/daemon.log"; fail "subsystem-ID identity failed after restart"; }
echo "restart OK: board_type=${CARD_TYPE} via subsystem ID, no persisted state"
stop_daemon || fail "phase-3 daemon hung on TERM"

# --- Phase 4: restore-on-exit hands the device back --------------------------

log "phase 4: restore-on-exit returns the device to tt-kmd"
# The restore needs a daemon that saw the device on tt-kmd (originalDrivers
# is in-memory). Hand the device back manually first, exactly as an operator
# would: clear the override (newline write), unbind, re-probe.
printf '\n' | sudo tee "$SYS/driver_override" >/dev/null
echo "$BDF" | sudo tee "$SYS/driver/unbind" >/dev/null
echo "$BDF" | sudo tee /sys/bus/pci/drivers_probe >/dev/null
wait_driver tenstorrent 30 || fail "manual restore to tt-kmd failed (drivers_probe)"
echo "device back on tt-kmd; running the restore-on-exit cycle:"

run_daemon --restore-on-exit
wait_driver vfio-pci 30 || { cat "$WORKDIR/daemon.log"; fail "bind failed in restore cycle"; }
stop_daemon TERM || fail "restore daemon hung on TERM"
wait_driver tenstorrent 30 || { cat "$WORKDIR/daemon.log"; fail "restore-on-exit did not return the device to tt-kmd"; }
OVERRIDE=$(cat "$SYS/driver_override" | tr -d '[:space:]')
[ -z "$OVERRIDE" ] || [ "$OVERRIDE" = "(null)" ] || fail "driver_override still '$OVERRIDE' after restore"
echo "restore-on-exit OK: device on tt-kmd, override cleared"

ls -l /dev/tenstorrent/ || fail "/dev/tenstorrent gone after restore"
log "ALL PHASES PASSED (board_type=${CARD_TYPE})"
