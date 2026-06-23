# Consolidated tools image — single image, multiple invocation modes.
# Mirrors NVIDIA's k8s-driver-manager pattern: one image, branched by
# the first argv. Each Job/DaemonSet the controller spawns passes the
# role as the first command arg (`build`, `flash`, `unload`).
#
# Why one image: the per-role images shared a huge fraction of their
# base layers (Ubuntu + apt toolchain + tt-smi) and diverged only in a
# few packages (tt-flash, ipmitool) and the entrypoint script. A single
# image cuts CI build time, halves the chart's image pin matrix, and
# matches the precedent set by NVIDIA's gpu-operator.
#
# Controller image stays separate (slim Go binary, distroless base) —
# the privilege boundary lives between "tools" (privileged, on each
# node) and "controller" (non-root, in operator namespace).

# UBUNTU_VERSION selects both the base image and the tt-smi release-binary
# flavor (tt-smi-<v>-ubuntu-<UBUNTU_VERSION>). The binary is a PyInstaller
# build linked against the glibc of that Ubuntu release. For the `build`
# role this must match the host OS (aus2-dev2 hosts run Ubuntu 22.04 jammy
# — bumping the host OS requires bumping this). For the `flash` role the
# version only has to be glibc-compatible with the container itself.
ARG UBUNTU_VERSION=22.04
ARG TT_FLASH_VERSION=3.6.5
ARG TT_SMI_VERSION=5.2.0
# tt-kmd's udev rule (MODE=0666 + by-id symlinks) doesn't change between
# kmd releases — the only thing it encodes is the Wormhole/Blackhole PCI
# IDs. Pin to a recent tag; bump when new silicon family lands upstream.
ARG TT_KMD_UDEV_VERSION=2.9.0
FROM ubuntu:${UBUNTU_VERSION}
ARG UBUNTU_VERSION
ARG TT_FLASH_VERSION
ARG TT_SMI_VERSION
ARG TT_KMD_UDEV_VERSION
LABEL org.opencontainers.image.source="https://github.com/tenstorrent/tt-k8s-driver-manager"
LABEL org.opencontainers.image.description="tt-k8s-driver-manager tools image — multi-mode entrypoint (build / flash / unload)."
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL com.tenstorrent.tt-smi-version="${TT_SMI_VERSION}"
LABEL com.tenstorrent.tt-flash-version="${TT_FLASH_VERSION}"
LABEL com.tenstorrent.tt-kmd-udev-version="${TT_KMD_UDEV_VERSION}"

ENV DEBIAN_FRONTEND=noninteractive

# Base toolchain — kernel module build + general utilities used by all
# roles. Kept stable so this large layer caches across builds.
#
# build-essential: gcc + make for the OOT module build (build role)
# gcc-12:          must match the host kernel's compiler. Ubuntu 22.04 LTS
#                  kernels (jammy + HWE 6.x) are built with gcc-12, and the
#                  kernel headers' Makefile hardcodes the binary name. Bump
#                  alongside the base image when the host kernel toolchain
#                  changes.
# libelf-dev:      objtool (shipped in the host's linux-headers tree as a
#                  prebuilt binary) dynamically links libelf.so.1.
# libssl-dev:      kernel module signing pulls in libcrypto.
# kmod:            insmod / rmmod against the host kernel (build + unload)
# git:             clone tt-kmd at the requested tag (build role)
# psmisc:          fuser, for surfacing holders of /dev/tenstorrent
# curl:            fetches the tt-smi release binary below; build-role
#                  entrypoint patches node labels via the in-cluster API.
# pciutils:        lspci for flash-role hardware introspection
# ca-certificates: TLS for the curl + git clone steps
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        build-essential gcc-12 libelf-dev libssl-dev \
        kmod git ca-certificates psmisc curl pciutils \
 && rm -rf /var/lib/apt/lists/*

# Python + tt-flash deps. Separated from the base layer so future
# flash-only additions here don't bust the kernel-toolchain cache.
# python3 is still needed for tt-flash (no standalone release) and for
# parsing tt-smi JSON in the flash entrypoint.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        python3 python3-pip python3-venv \
        ipmitool sudo \
 && rm -rf /var/lib/apt/lists/* \
 && command -v ipmitool >/dev/null \
 && command -v sudo     >/dev/null

# tt-flash via pip into an isolated venv (no standalone release).
RUN python3 -m venv /opt/tt \
 && /opt/tt/bin/pip install --upgrade pip \
 && /opt/tt/bin/pip install "git+https://github.com/tenstorrent/tt-flash.git@v${TT_FLASH_VERSION}"

RUN printf '#!/bin/sh\nexec /opt/tt/bin/tt-flash "$@"\n' > /usr/local/bin/tt-flash \
 && chmod 0755 /usr/local/bin/tt-flash

# tt-smi as the standalone PyInstaller binary (since v5.1.0). Bundles
# libatomic / libstdc++ / etc., so the container doesn't need any of the
# UMD native deps that the old pip-install approach implicitly required.
RUN curl -fsSL -o /usr/local/bin/tt-smi \
        "https://github.com/tenstorrent/tt-smi/releases/download/v${TT_SMI_VERSION}/tt-smi-${TT_SMI_VERSION}-ubuntu-${UBUNTU_VERSION}" \
 && chmod 0755 /usr/local/bin/tt-smi
ENV TT_SMI_VERSION=${TT_SMI_VERSION}

# tt-kmd's udev rule. Bundled at image build (same pattern as tt-smi
# above) so the build-role entrypoint never needs to fetch or clone
# for it. Lands on the host at /etc/udev/rules.d/50-tenstorrent.rules.
RUN mkdir -p /usr/local/share/tt-k8s-driver-manager \
 && curl -fsSL -o /usr/local/share/tt-k8s-driver-manager/udev-50-tenstorrent.rules \
        "https://raw.githubusercontent.com/tenstorrent/tt-kmd/ttkmd-${TT_KMD_UDEV_VERSION}/udev-50-tenstorrent.rules" \
 && chmod 0644 /usr/local/share/tt-k8s-driver-manager/udev-50-tenstorrent.rules
ENV TT_KMD_UDEV_VERSION=${TT_KMD_UDEV_VERSION}

# Per-role scripts live alongside the dispatcher. tools-entrypoint.sh
# branches on $1 ∈ {build, flash, unload} and exec's the corresponding
# role script; anything else is exec'd verbatim (ad-hoc debugging).
COPY build.sh             /usr/local/bin/build.sh
COPY flash.sh             /usr/local/bin/flash.sh
COPY unload.sh            /usr/local/bin/unload.sh
COPY tools-entrypoint.sh  /usr/local/bin/tools-entrypoint.sh
RUN chmod 0755 /usr/local/bin/build.sh \
               /usr/local/bin/flash.sh \
               /usr/local/bin/unload.sh \
               /usr/local/bin/tools-entrypoint.sh

ENTRYPOINT ["/usr/local/bin/tools-entrypoint.sh"]
