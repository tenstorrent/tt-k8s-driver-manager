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

# tt_smi_snapshot runs `tt-smi -s` capturing both streams + exit code.
# On success: $TT_SMI_OUT contains the JSON stdout; rc=0.
# On failure: dumps everything to the log so debugging from `kubectl logs`
# doesn't need a privileged exec session. tt-smi puts its real error message
# (e.g. "No Tenstorrent driver detected!") on stdout, not stderr, so logging
# only one stream loses the useful signal.
tt_smi_snapshot() {
  local label="$1"
  local err_file
  err_file=$(mktemp)
  if TT_SMI_OUT=$(tt-smi -s 2>"$err_file"); then
    rm -f "$err_file"
    return 0
  fi
  local rc=$?
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
# Capture current fw_bundle_versions from tt-smi -s. Failure is tolerated
# only if TT_FORCE=true (matches the `rescue:` block in tt-ansible's
# tt_firmware role).
log "pre-flash: tt-smi -s"
if tt_smi_snapshot "pre-flash"; then
  current=$(printf '%s' "$TT_SMI_OUT" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" ".join(v.get("firmwares",{}).get("fw_bundle_version","?") for v in d.get("device_info",[])))' || echo "?")
  log "pre-flash versions: $current"
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
else
  if [[ "${TT_FORCE:-false}" == "true" ]]; then
    log "WARN: pre-flash tt-smi failed but TT_FORCE=true; continuing"
  else
    log "ERROR: pre-flash tt-smi failed — aborting (set spec.force: true to bypass)"
    exit 1
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
