#!/bin/sh
# Unloader entrypoint — the inverse of build.sh.
#
# Placeholder pending PR #38 (spec.unmanage auto-vacate), which lands
# the controller-side Job that drives this script. The contract below
# matches what #38 expects; if the PR's final design diverges, this
# script is replaced at merge time without re-cutting the image.
#
# Contract:
#   - rmmod tenstorrent if loaded with refcnt=0.
#   - Remove any cached .ko under /var/cache/tt-kmd (mounted from the
#     host) so a future reinstall starts clean.
#   - Optionally remove the host's tt-smi binary if /host/usr/local/bin
#     is mounted and TT_REMOVE_TT_SMI=true.
#   - Exit 0 on success; non-zero leaves the controller in Failed and
#     the next reconcile retries.

set -eu

MODULE=tenstorrent

log() { printf '[unload] %s\n' "$*" >&2; }

loaded_version() {
    cat "/sys/module/${MODULE}/version" 2>/dev/null || true
}

refcnt() {
    cat "/sys/module/${MODULE}/refcnt" 2>/dev/null || echo 0
}

LOADED=$(loaded_version)
if [ -n "$LOADED" ]; then
    rc=$(refcnt)
    if [ "$rc" -gt 0 ]; then
        log "ERROR: ${MODULE} loaded (v${LOADED}) with refcnt=${rc}; refusing to rmmod"
        log "Drain workloads holding /dev/tenstorrent and retry."
        exit 1
    fi
    log "rmmod ${MODULE} (v${LOADED})"
    rmmod "${MODULE}"
else
    log "${MODULE} not loaded; skipping rmmod"
fi

if [ -d /var/cache/tt-kmd ]; then
    log "removing cached .ko files under /var/cache/tt-kmd"
    find /var/cache/tt-kmd -name "${MODULE}.ko" -delete
fi

if [ "${TT_REMOVE_TT_SMI:-false}" = "true" ] && [ -f /host/usr/local/bin/tt-smi ]; then
    log "removing host /usr/local/bin/tt-smi"
    rm -f /host/usr/local/bin/tt-smi
fi

log "unload complete"
