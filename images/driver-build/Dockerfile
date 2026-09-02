# SPDX-License-Identifier: Apache-2.0
# SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

# UBUNTU_VERSION selects both the base image and the tt-smi release-binary
# flavor (tt-smi-<v>-ubuntu-<UBUNTU_VERSION>). The binary is a PyInstaller
# build linked against the glibc of that Ubuntu release, so it must match
# the host OS. Target hosts run Ubuntu 22.04 (jammy); bumping the host
# OS requires bumping this too.
ARG UBUNTU_VERSION=22.04
ARG TT_SMI_VERSION=5.2.0
# tt-kmd's udev rule (MODE=0666 + by-id symlinks) doesn't change between
# kmd releases — the only thing it encodes is the Wormhole/Blackhole PCI
# IDs. Pin to a recent tag; bump when new silicon family lands upstream.
ARG TT_KMD_UDEV_VERSION=2.9.0
FROM ubuntu:${UBUNTU_VERSION}
ARG UBUNTU_VERSION
ARG TT_SMI_VERSION
ARG TT_KMD_UDEV_VERSION
LABEL org.opencontainers.image.source="https://github.com/tenstorrent/tt-k8s-driver-manager"
LABEL org.opencontainers.image.description="tt-k8s-driver-manager builder pod — clones + builds tt-kmd, insmods, copies tt-smi to host."
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL com.tenstorrent.tt-smi-version="${TT_SMI_VERSION}"
LABEL com.tenstorrent.tt-kmd-udev-version="${TT_KMD_UDEV_VERSION}"

# build-essential: gcc + make for the OOT module build
# gcc-12:          must match the host kernel's compiler. Ubuntu 22.04 LTS
#                  kernels (jammy + HWE 6.x) are built with gcc-12, and the
#                  kernel headers' Makefile hardcodes the binary name. Bump
#                  alongside the base image when the host kernel toolchain
#                  changes.
# libelf-dev:      objtool (shipped in the host's linux-headers tree as a
#                  prebuilt binary) dynamically links libelf.so.1.
# libssl-dev:      kernel module signing pulls in libcrypto.
# kmod:            insmod / rmmod against the host kernel
# git:             clone tt-kmd at the requested tag
# psmisc:          fuser, for surfacing holders of /dev/tenstorrent
# curl:            fetches the tt-smi release binary below; entrypoint
#                  patches node labels via the in-cluster API.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        build-essential gcc-12 libelf-dev libssl-dev \
        kmod git ca-certificates psmisc curl \
 && rm -rf /var/lib/apt/lists/*

# tt-smi ships a self-contained single-file binary per Ubuntu release
# (PyInstaller, since v5.1.0) — no Python needed on the host. The
# entrypoint copies it to host /usr/local/bin/tt-smi.
RUN curl -fsSL -o /usr/local/bin/tt-smi \
        "https://github.com/tenstorrent/tt-smi/releases/download/v${TT_SMI_VERSION}/tt-smi-${TT_SMI_VERSION}-ubuntu-${UBUNTU_VERSION}" \
 && chmod 0755 /usr/local/bin/tt-smi
ENV TT_SMI_VERSION=${TT_SMI_VERSION}

# tt-kmd's udev rule. Bundled at image build (same pattern as tt-smi
# above) so the entrypoint never needs to fetch or clone for it. Lands
# on the host at /etc/udev/rules.d/50-tenstorrent.rules.
RUN mkdir -p /usr/local/share/tt-k8s-driver-manager \
 && curl -fsSL -o /usr/local/share/tt-k8s-driver-manager/udev-50-tenstorrent.rules \
        "https://raw.githubusercontent.com/tenstorrent/tt-kmd/ttkmd-${TT_KMD_UDEV_VERSION}/udev-50-tenstorrent.rules" \
 && chmod 0644 /usr/local/share/tt-k8s-driver-manager/udev-50-tenstorrent.rules
ENV TT_KMD_UDEV_VERSION=${TT_KMD_UDEV_VERSION}

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
