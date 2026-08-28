# hotplug-stub: builds and loads the tenstorrent-simple PCI stub against the
# host kernel, then exits. Runs as an initContainer on the vfio-manage
# DaemonSet.
#
# UBUNTU_VERSION must match the host OS and the toolchain must match the
# compiler the host kernel was built with — same constraint as the
# driver-build image, for the same reason (out-of-tree module against the
# host's kernel headers).
ARG UBUNTU_VERSION=22.04
FROM ubuntu:${UBUNTU_VERSION}
LABEL org.opencontainers.image.source="https://github.com/tenstorrent/tt-k8s-driver-manager"
LABEL org.opencontainers.image.description="hotplug-stub — builds + loads the tenstorrent-simple PCI stub that suppresses PCIe hotplug on Galaxy."
LABEL org.opencontainers.image.licenses="Apache-2.0"

# gcc-12 matches the Ubuntu 22.04 kernel toolchain; libelf/libssl are what
# objtool and module signing link against. Same set as driver-build minus
# the pieces only the tt-kmd build needs.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential gcc-12 libelf-dev libssl-dev kmod \
    && rm -rf /var/lib/apt/lists/*

COPY src/ /usr/local/src/hotplug-stub/
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
