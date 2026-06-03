#!/usr/bin/env bash
# Flasher entrypoint for tt-operator.
#
# Contract — set by the operator's Job template (see internal/controller/job.go):
#   TT_FW_BUNDLE_PATH   Local path the initContainer wrote the bundle to.
#   TT_FW_VERSION       Version we asked tt-flash to write (e.g. 19.8.0).
#   TT_FW_READBACK      Version tt-smi should report after flash (e.g. 19.8.0.0).
#   TT_FLASH_ARGS       Extra args to tt-flash (e.g. "--force").
#   TT_FORCE            "true" if --force is in flight; lets us continue past a
#                       failed pre-flash readback (matches tt-ansible's rescue).
#   MOCK                "true" → no hardware; simulate a flash. For kind dev.
#   MOCK_FAIL           "true" → simulated failure path (for testing UpgradeFailed).
#
# Exit codes:
#   0   Flash succeeded (or skipped because version already matched and not force).
#   non-zero  Anything that should mark the node Failed.

set -euo pipefail

log() { printf '[flasher] %s\n' "$*" >&2; }

# is_galaxy returns 0 if any Tenstorrent device on this host is a Galaxy UBB
# variant, 1 otherwise. Reads PCI subsystem IDs from /sys (kernel-populated
# via PCI core, no tt-kmd driver state needed — works on wedged cards as
# long as PCIe enumeration succeeded at boot).
#
# IDs match tt-kmd/enumerate.c:
#   0x0035 = Wormhole Galaxy UBB
#   0x0047 = Blackhole Galaxy UBB
is_galaxy() {
  shopt -s nullglob
  for d in /sys/bus/pci/devices/*; do
    [[ "$(cat "$d/vendor" 2>/dev/null)" == "0x1e52" ]] || continue
    case "$(cat "$d/subsystem_device" 2>/dev/null)" in
      0x0035|0x0047) shopt -u nullglob; return 0 ;;
    esac
  done
  shopt -u nullglob
  return 1
}

# heal_cards attempts a single hardware reset cycle when the pre-flash
# tt-smi readback fails. Galaxy hosts need an IPMI tray power-cycle
# (tt-smi -glx_reset); PCIe-attached single-card hosts only need
# USER_RESET via the kernel driver (tt-smi -r).
heal_cards() {
  if is_galaxy; then
    log "heal: Galaxy host (PCI subsystem 0x0035/0x0047) → tt-smi -glx_reset"
    tt-smi -glx_reset || log "heal: tt-smi -glx_reset exited non-zero"
  else
    log "heal: PCIe-only host → tt-smi -r"
    tt-smi -r || log "heal: tt-smi -r exited non-zero"
  fi
  # Give PCIe / the driver a moment to re-enumerate.
  sleep 5
}

# tt_smi_snapshot runs `tt-smi -s` capturing both streams + exit code.
# On success: $TT_SMI_OUT contains the JSON stdout; rc=0.
# On failure: dumps everything to the log so debugging from `kubectl logs`
# doesn't need a privileged exec session. tt-smi puts its real error message
# (e.g. "No Tenstorrent driver detected!") on stdout, not stderr, so logging
# only one stream loses the useful signal.
tt_smi_snapshot() {
  local label="$1"
  local err_file rc
  err_file=$(mktemp)
  # Capture tt-smi's exit code immediately. Don't put the command inside the
  # `if` test — after a failed `if` with no matched branch, $? is 0 (bash
  # spec), so `local rc=$?` would mask real failures.
  TT_SMI_OUT=$(tt-smi -s 2>"$err_file"); rc=$?
  if [[ $rc -eq 0 ]]; then
    rm -f "$err_file"
    return 0
  fi
  local err
  err=$(cat "$err_file" 2>/dev/null || true)
  rm -f "$err_file"
  log "ERROR: $label tt-smi -s exited $rc"
  if [[ -n "$TT_SMI_OUT" ]]; then
    log "tt-smi stdout (first 50 lines):"
    printf '%s\n' "$TT_SMI_OUT" | head -50 | sed 's/^/    /' >&2
  else
    log "tt-smi stdout: (empty)"
  fi
  if [[ -n "$err" ]]; then
    log "tt-smi stderr (first 50 lines):"
    printf '%s\n' "$err" | head -50 | sed 's/^/    /' >&2
  else
    log "tt-smi stderr: (empty)"
  fi
  return $rc
}

if [[ "${MOCK:-false}" == "true" ]]; then
  log "MOCK=true; simulating flash of $TT_FW_VERSION (readback=$TT_FW_READBACK)"
  sleep "${MOCK_DELAY_SECONDS:-3}"
  if [[ "${MOCK_FAIL:-false}" == "true" ]]; then
    log "MOCK_FAIL=true; exiting non-zero"
    exit 1
  fi
  log "MOCK flash succeeded"
  exit 0
fi

# --- 1. Pre-flash readback ------------------------------------------------
# Capture current fw_bundle_versions from tt-smi -s. If the readback fails
# (cards wedged / ENODEV / driver detached), run one heal cycle and try
# once more before giving up. TT_FORCE retained as a final escape hatch.
log "pre-flash: tt-smi -s"
if ! tt_smi_snapshot "pre-flash"; then
  log "pre-flash readback failed — attempting one heal cycle"
  heal_cards
  if ! tt_smi_snapshot "pre-flash-after-heal"; then
    if [[ "${TT_FORCE:-false}" == "true" ]]; then
      log "WARN: tt-smi still failing after heal but TT_FORCE=true; continuing blindly"
    else
      log "ERROR: tt-smi still failing after heal — aborting (set spec.force: true to bypass)"
      exit 1
    fi
  fi
fi

# At this point TT_SMI_OUT is set if either readback (initial or post-heal)
# succeeded. If both failed and we're only here because of TT_FORCE,
# TT_SMI_OUT may be empty — guard the parse.
current=""
if [[ -n "${TT_SMI_OUT:-}" ]]; then
  current=$(printf '%s' "$TT_SMI_OUT" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" ".join(v.get("firmwares",{}).get("fw_bundle_version","?") for v in d.get("device_info",[])))' || echo "?")
  log "pre-flash versions: $current"
fi

# Skip flash if every card already reports the desired readback and not force.
if [[ "${TT_FORCE:-false}" != "true" ]] && [[ -n "$current" ]] && [[ "$current" != "?" ]]; then
  mismatch=0
  for v in $current; do
    [[ "$v" == "$TT_FW_READBACK" ]] || mismatch=1
  done
  if [[ $mismatch -eq 0 ]]; then
    log "all devices already at $TT_FW_READBACK; skipping flash"
    exit 0
  fi
fi

# --- 2. Flash --------------------------------------------------------------
log "flash: tt-flash --no-color flash --fw-tar $TT_FW_BUNDLE_PATH ${TT_FLASH_ARGS:-}"
flash_log=$(mktemp)
# shellcheck disable=SC2086
if ! tt-flash --no-color flash --fw-tar "$TT_FW_BUNDLE_PATH" ${TT_FLASH_ARGS:-} 2>&1 | tee "$flash_log"; then
  log "ERROR: tt-flash exited non-zero"
  exit 1
fi

# Matches the explicit failed_when in tt-ansible's tt_firmware role: a known
# bad-result string can appear with exit code 0.
if grep -q "Config space reset not completed for device" "$flash_log"; then
  log "ERROR: tt-flash reported 'Config space reset not completed for device' (exit was 0 but device is bad)"
  exit 1
fi

# --- 3. Post-flash readback assertion --------------------------------------
log "post-flash: tt-smi -s"
if ! tt_smi_snapshot "post-flash"; then
  log "ERROR: post-flash tt-smi failed; cannot verify readback"
  exit 1
fi
readback=$(printf '%s' "$TT_SMI_OUT" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" ".join(v.get("firmwares",{}).get("fw_bundle_version","?") for v in d.get("device_info",[])))')
log "post-flash versions: $readback"

mismatched=()
for v in $readback; do
  if [[ "$v" != "$TT_FW_READBACK" ]]; then
    mismatched+=("$v")
  fi
done
if [[ ${#mismatched[@]} -gt 0 ]]; then
  log "ERROR: readback mismatch — wanted $TT_FW_READBACK, got: ${mismatched[*]}"
  exit 1
fi

log "flash succeeded: all devices at $TT_FW_READBACK"
exit 0
