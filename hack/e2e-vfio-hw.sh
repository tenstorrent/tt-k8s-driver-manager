#!/usr/bin/env bash
# End-to-end hardware test for vfio-manage. Runs on a Tenstorrent runner VM
# (card on tt-kmd, ephemeral — destroyed after the job, so a failed restore
# can't strand anything durable).
#
# Phases:
#   1. Discover the TT device and dump its identity while on tt-kmd —
#      including subsystem IDs, to answer whether n150/n300 differ in config
#      space (if they do, telemetry-based identity is unnecessary).
#   2. Run vfio-manage: assert the device lands on vfio-pci, the metrics
#      endpoint reports devices_bound and device_info with the real
#      board_type, and the state file holds the identity.
#   3. Restart vfio-manage: assert the identity is recovered from the state
#      file alone (the device is on vfio-pci now, telemetry unreadable).
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

# tt_card_type may lag ARC init; poll briefly before concluding it's absent.
CARD_TYPE=""
for _ in $(seq 1 12); do
  CARD_TYPE=$(cat "$SYS/tt_card_type" 2>/dev/null | tr -d '[:space:]') && [ -n "$CARD_TYPE" ] && break
  sleep 5
done
SERIAL=$(cat "$SYS/tt_serial" 2>/dev/null | tr -d '[:space:]' || true)
echo "tt_card_type=${CARD_TYPE:-<unreadable>} tt_serial=${SERIAL:-<unreadable>}"
[ -n "$CARD_TYPE" ] || fail "tt_card_type unreadable while on tt-kmd — identity mechanism assumption broken (kmd $(cat /sys/module/tenstorrent/version 2>/dev/null))"

# The open question from the PR: does config space already distinguish SKUs?
log "config-space identity (subsystem IDs) — decides if telemetry caching is even needed"
echo "subsystem_vendor=$(cat "$SYS/subsystem_vendor") subsystem_device=$(cat "$SYS/subsystem_device") board_type=$CARD_TYPE"
echo "::notice title=SKU identity::board_type=$CARD_TYPE subsystem_device=$(cat "$SYS/subsystem_device") (compare across runner SKUs)"

# --- vfio prerequisites ------------------------------------------------------

log "IOMMU / vfio setup"
if [ -z "$(ls -A /sys/kernel/iommu_groups 2>/dev/null)" ]; then
  echo "no IOMMU groups — enabling unsafe noiommu mode (test VM)"
  sudo modprobe vfio enable_unsafe_noiommu_mode=1
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

STATE="$WORKDIR/identity.json"
run_daemon() {
  sudo "$VFIO_MANAGE_BIN" \
    --config "$WORKDIR/config.yaml" \
    --bind-interval 5s \
    --state-file "$STATE" \
    --metrics-addr ":${METRICS_PORT}" \
    "$@" >"$WORKDIR/daemon.log" 2>&1 &
  DAEMON_PID=$!
}

wait_driver() { # wait_driver <driver> <timeout_s>
  for _ in $(seq 1 "$2"); do
    [ "$(basename "$(readlink "$SYS/driver" 2>/dev/null)" )" = "$1" ] && return 0
    sleep 1
  done
  return 1
}

metrics() { curl -sf "http://localhost:${METRICS_PORT}/metrics"; }

# --- Phase 2: bind + identity + metrics -------------------------------------

log "phase 2: vfio-manage binds the device"
run_daemon
wait_driver vfio-pci 30 || { cat "$WORKDIR/daemon.log"; fail "device never bound to vfio-pci"; }
echo "device on vfio-pci"

sleep 2
M=$(metrics)
echo "$M" | grep 'tt_vfio_devices_bound_total{resource="tenstorrent.com/e2e-test"} 1' \
  || { echo "$M" | grep tt_vfio || true; fail "devices_bound metric wrong"; }
echo "$M" | grep "tt_vfio_device_info" | grep "board_type=\"${CARD_TYPE}\"" \
  || { echo "$M" | grep tt_vfio_device_info || true; fail "device_info missing board_type=${CARD_TYPE}"; }
echo "metrics OK: bound=1, board_type=${CARD_TYPE}"

sudo test -s "$STATE" || fail "state file not written"
sudo grep -q "\"boardType\": \"${CARD_TYPE}\"" "$STATE" || { sudo cat "$STATE"; fail "state file lacks board type"; }
echo "state file OK"

sudo kill "$DAEMON_PID"; wait "$DAEMON_PID" 2>/dev/null || true

# --- Phase 3: restart recovers identity from state alone ---------------------

log "phase 3: restart — device already on vfio-pci, identity must come from the state file"
run_daemon
sleep 8
M=$(metrics)
echo "$M" | grep "tt_vfio_device_info" | grep "board_type=\"${CARD_TYPE}\"" \
  || { echo "$M" | grep tt_vfio_device_info || true; fail "identity not recovered from state file after restart"; }
echo "identity recovered from state file"
sudo kill "$DAEMON_PID"; wait "$DAEMON_PID" 2>/dev/null || true

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
sudo kill -TERM "$DAEMON_PID"
wait "$DAEMON_PID" 2>/dev/null || true
wait_driver tenstorrent 30 || { cat "$WORKDIR/daemon.log"; fail "restore-on-exit did not return the device to tt-kmd"; }
OVERRIDE=$(cat "$SYS/driver_override" | tr -d '[:space:]')
[ -z "$OVERRIDE" ] || [ "$OVERRIDE" = "(null)" ] || fail "driver_override still '$OVERRIDE' after restore"
echo "restore-on-exit OK: device on tt-kmd, override cleared"

ls -l /dev/tenstorrent/ || fail "/dev/tenstorrent gone after restore"
log "ALL PHASES PASSED (board_type=${CARD_TYPE})"
