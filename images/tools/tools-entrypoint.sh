#!/bin/sh
# Multi-mode dispatcher for the consolidated tools image.
#
# Usage: tools-entrypoint.sh <role> [args...]
#
# Roles:
#   build   — compile + insmod tt-kmd (per-node DaemonSet pod).
#             Driven by TenstorrentDriverPolicy.
#   flash   — tt-flash + bundle (per-node firmware Job).
#             Driven by TenstorrentFirmwarePolicy.
#   unload  — rmmod tt-kmd + remove .ko files (per-node Job for
#             spec.unmanage auto-vacate; see sibling PR #38).
#
# Anything else is exec'd verbatim — keeps the image usable for ad-hoc
# debugging Jobs (`kubectl run --image=tools -- sh -c 'tt-smi -s'`).
set -eu

ROLE=${1:-}
case "$ROLE" in
    build)
        shift
        exec /usr/local/bin/build.sh "$@"
        ;;
    flash)
        shift
        exec /usr/local/bin/flash.sh "$@"
        ;;
    unload)
        shift
        exec /usr/local/bin/unload.sh "$@"
        ;;
    "")
        echo "tools-entrypoint: no role given; pass one of: build, flash, unload, or a verbatim command" >&2
        exit 2
        ;;
    *)
        # Verbatim passthrough. Lets `kubectl debug --image=...` and the
        # debug-flasher hack/ manifest keep working without re-templating
        # the command.
        exec "$@"
        ;;
esac
