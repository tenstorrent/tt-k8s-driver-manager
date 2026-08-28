#!/bin/sh
# Builds and loads tenstorrent-simple, the PCI stub that suppresses PCIe
# hotplug on Galaxy hosts (see src/tenstorrent_simple.c for why).
#
# Runs as an initContainer on the vfio-manage DaemonSet: vfio-manage needs
# the driver registered before its first bind pass, and an initContainer
# gives us that ordering for free. Exits 0 once the driver is registered.
#
# The .ko is cached per kernel version under $CACHE_ROOT, mirroring the
# driver-build builder, so a pod restart or a second node with the same
# kernel skips the compile.
set -eu

MODULE=tenstorrent_simple
DRIVER=tenstorrent-simple
KVER=$(uname -r)
CACHE_ROOT="${CACHE_ROOT:-/var/cache/tt-hotplug-stub}"
CACHE_DIR="${CACHE_ROOT}/${KVER}"
KO_PATH="${CACHE_DIR}/${DRIVER}.ko"
SRC_DIR=/usr/local/src/hotplug-stub

if [ -d "/sys/bus/pci/drivers/${DRIVER}" ]; then
    echo "${DRIVER} already registered on kernel ${KVER}; nothing to do"
    exit 0
fi

if [ ! -f "/lib/modules/${KVER}/build/Makefile" ]; then
    echo "ERROR: host has no kernel build tree at /lib/modules/${KVER}/build" >&2
    echo "Install linux-headers-${KVER} on the host." >&2
    exit 1
fi

if [ ! -f "${KO_PATH}" ]; then
    echo "cache miss for ${KVER}; building ${DRIVER}"
    BUILD=$(mktemp -d)
    cp "${SRC_DIR}/tenstorrent_simple.c" "${SRC_DIR}/Makefile" "${BUILD}/"
    make -j"$(nproc)" -C "/lib/modules/${KVER}/build" M="${BUILD}" modules
    mkdir -p "${CACHE_DIR}"
    cp "${BUILD}/${DRIVER}.ko" "${KO_PATH}"
    rm -rf "${BUILD}"
    echo "built ${KO_PATH}"
else
    echo "cache hit at ${KO_PATH}"
fi

echo "loading ${KO_PATH}"
insmod "${KO_PATH}"

# insmod succeeding isn't proof the driver registered — pci_register_driver
# could still have failed. The bind path only works if the driver directory
# is there, so check the thing vfio-manage actually depends on.
if [ ! -d "/sys/bus/pci/drivers/${DRIVER}" ]; then
    echo "ERROR: ${MODULE} loaded but /sys/bus/pci/drivers/${DRIVER} is missing" >&2
    exit 1
fi

echo "${DRIVER} registered on kernel ${KVER}"
