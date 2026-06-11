#!/bin/sh
# Builder entrypoint. Three modes, decided per-startup:
#
#   1. HOST-MANAGED — host has its own DKMS/apt-installed tt-kmd (e.g.
#      tt-ansible's tt_kmd role). Builder stands down: doesn't touch
#      module, doesn't build, sets node label install-mode=host so the
#      operator surface honestly reports who owns the install. Mirrors
#      RBLN's rebellions.ai/npu.deploy.driver=pre-installed pattern.
#
#   2. CONTAINER-MANAGED, version matches — module already loaded at
#      the expected version (from cache + insmod on a previous pod
#      lifecycle). Builder idles, sets install-mode=container.
#
#   3. CONTAINER-MANAGED, mismatch — rmmod (refcnt=0 required), build
#      from cache or clone+make, insmod, idle.
set -eu

MODULE=tenstorrent
KVER=$(uname -r)
EXPECTED="${TT_KMD_VERSION:?TT_KMD_VERSION env var required}"
CACHE_DIR="/var/cache/tt-kmd/${KVER}/${EXPECTED}"
KO_PATH="${CACHE_DIR}/${MODULE}.ko"

API="https://kubernetes.default.svc"
TOKEN_PATH="/var/run/secrets/kubernetes.io/serviceaccount/token"
CA_PATH="/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

loaded_version() {
    cat "/sys/module/${MODULE}/version" 2>/dev/null || true
}

refcnt() {
    cat "/sys/module/${MODULE}/refcnt" 2>/dev/null || echo 0
}

# label_node sets a single label on $NODE_NAME via the Kubernetes API.
# Falls back to no-op (with a log warning) if the SA token isn't mounted
# or curl/curl returns non-2xx — kmd-version label sync still works via
# the controller, so a node-patch failure here just means the
# install-mode label is missing; not fatal.
label_node() {
    KEY=$1
    VAL=$2
    if [ -z "${NODE_NAME:-}" ] || [ ! -r "$TOKEN_PATH" ]; then
        echo "WARN: cannot label node ($KEY=$VAL): NODE_NAME or SA token missing"
        return 0
    fi
    PATCH="{\"metadata\":{\"labels\":{\"${KEY}\":\"${VAL}\"}}}"
    RESP=$(curl --silent --show-error --max-time 10 \
        --cacert "$CA_PATH" \
        --header "Authorization: Bearer $(cat $TOKEN_PATH)" \
        --header "Content-Type: application/strategic-merge-patch+json" \
        --request PATCH \
        --data "$PATCH" \
        "$API/api/v1/nodes/$NODE_NAME" -o /tmp/patch-resp -w '%{http_code}' 2>&1) || true
    if [ "$RESP" != "200" ]; then
        echo "WARN: node label patch returned HTTP $RESP for $KEY=$VAL"
        head -c 400 /tmp/patch-resp 2>/dev/null | sed 's/^/  /'
    fi
}

# host_install_detected returns true (0) when there's evidence of a
# host-side install of tt-kmd. Three signals, any one is enough:
#   - /var/lib/dkms/tenstorrent — DKMS tracks the module
#   - /usr/src/tenstorrent-<v>/dkms.conf — DKMS source registered
#   - tt-smi binary on host PATH at /usr/local/bin or /usr/bin
# Strong-signal-first to keep false positives low; tt-smi alone is
# present in many places (workloads bundle it), so it's a weaker
# heuristic and only used as a tiebreaker if the first two don't fire.
host_install_detected() {
    if [ -d "/var/lib/dkms/tenstorrent" ]; then
        echo "host-install signal: /var/lib/dkms/tenstorrent exists"
        return 0
    fi
    for d in /usr/src/tenstorrent-*; do
        if [ -f "$d/dkms.conf" ]; then
            echo "host-install signal: $d/dkms.conf exists"
            return 0
        fi
    done
    return 1
}

# install_tt_smi copies the self-contained tt-smi binary to the host.
# Skip in host-managed mode — the host already has its own tt-smi from
# tt-ansible / apt and we shouldn't overwrite it.
install_tt_smi() {
    [ -n "${TT_SMI_VERSION:-}" ] || return 0
    if [ ! -d /host/usr/local/bin ]; then
        echo "WARN: /host/usr/local/bin not mounted; skipping tt-smi install"
        return 0
    fi
    # rename(2) for atomic swap: concurrent callers see the old binary
    # or the new one, never a partial file.
    cp /usr/local/bin/tt-smi /host/usr/local/bin/.tt-smi.new
    chmod 0755 /host/usr/local/bin/.tt-smi.new
    mv /host/usr/local/bin/.tt-smi.new /host/usr/local/bin/tt-smi
    # Builders before tt-smi 5.x delivered a venv at /opt/tt behind a
    # shim; the binary above replaces the shim, so drop the orphan.
    rm -rf /host/opt/tt
    label_node "tt-smi.driver.tenstorrent.com/version" "${TT_SMI_VERSION}"
    echo "tt-smi ${TT_SMI_VERSION} installed at host:/usr/local/bin/tt-smi"
}

# --- 1. Host-managed mode? --------------------------------------------
if host_install_detected; then
    echo "host-managed mode detected — standing down, not touching the module"
    label_node "driver.tenstorrent.com/install-mode" "host"
    LOADED=$(loaded_version)
    if [ -n "$LOADED" ]; then
        echo "tt-kmd ${LOADED} loaded by host; idling"
    else
        echo "no tt-kmd loaded yet; the host is expected to load it (kmd-version label will follow)"
    fi
    # Skip tt-smi install — host is expected to manage it too.
    exec sleep infinity
fi

# --- 2. Container-managed: match? -------------------------------------
LOADED=$(loaded_version)

if [ "$LOADED" = "$EXPECTED" ]; then
    echo "tt-kmd ${LOADED} matches TT_KMD_VERSION on kernel ${KVER}; idling"
    label_node "driver.tenstorrent.com/install-mode" "container"
    install_tt_smi
    exec sleep infinity
fi

# --- 3. Container-managed: build + load -------------------------------
if [ -n "$LOADED" ]; then
    if [ "$(refcnt)" -gt 0 ]; then
        echo "tt-kmd ${LOADED} loaded with refcnt $(refcnt); holders: $(fuser /dev/tenstorrent/* 2>&1 || true)" >&2
        if [ "${TT_FORCE_UNLOAD:-false}" = "true" ]; then
            # Opt-in escape hatch: SIGKILL every process holding /dev/tenstorrent
            # so rmmod can proceed. Lossy — in-flight workloads on this node die.
            echo "TT_FORCE_UNLOAD=true; SIGKILL'ing device holders via fuser -k" >&2
            fuser -k /dev/tenstorrent/* 2>&1 || true
            # Kernel needs a moment to drop the refs after the killed processes' fds close.
            for _ in 1 2 3 4 5 6 7 8 9 10; do
                [ "$(refcnt)" -eq 0 ] && break
                sleep 1
            done
            if [ "$(refcnt)" -gt 0 ]; then
                echo "ERROR: refcnt still $(refcnt) after fuser -k; giving up" >&2
                exit 1
            fi
        else
            echo "ERROR: refcnt > 0 and forceUnload is false; cannot reinstall ${EXPECTED}" >&2
            echo "Drain workloads holding /dev/tenstorrent (or set spec.forceUnload: true) and let the next reconcile retry." >&2
            exit 1
        fi
    fi
    echo "tt-kmd ${LOADED} loaded (refcnt=0); unloading to install ${EXPECTED}"
    rmmod "${MODULE}"
fi

if [ ! -f "/lib/modules/${KVER}/build/Makefile" ]; then
    echo "ERROR: host has no kernel build tree at /lib/modules/${KVER}/build" >&2
    echo "Install linux-headers-${KVER} on the host." >&2
    exit 1
fi

if [ ! -f "${KO_PATH}" ]; then
    echo "cache miss for ${KVER}/${EXPECTED}; cloning tt-kmd + building"
    SRC=$(mktemp -d)
    git clone --depth 1 --branch "ttkmd-${EXPECTED}" \
        https://github.com/tenstorrent/tt-kmd.git "${SRC}"
    make -j"$(nproc)" -C "/lib/modules/${KVER}/build" M="${SRC}" modules
    mkdir -p "${CACHE_DIR}"
    cp "${SRC}/${MODULE}.ko" "${KO_PATH}"
    rm -rf "${SRC}"
    echo "built ${KO_PATH}"
else
    echo "cache hit at ${KO_PATH}"
fi

echo "loading ${KO_PATH}"
insmod "${KO_PATH}"

LOADED=$(loaded_version)
if [ "$LOADED" != "$EXPECTED" ]; then
    echo "ERROR: post-insmod /sys/module reports '${LOADED}', expected '${EXPECTED}'" >&2
    exit 1
fi

label_node "driver.tenstorrent.com/install-mode" "container"
install_tt_smi
echo "tt-kmd ${LOADED} loaded on kernel ${KVER}; idling"
exec sleep infinity
