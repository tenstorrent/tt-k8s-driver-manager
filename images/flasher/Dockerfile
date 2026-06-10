# Flasher image — runs as a per-node Job under the operator.
# Bundles tt-flash + the standalone tt-smi release binary + a small
# entrypoint that asserts readback.
#
# Pin the *minimum* tool versions here (matching tt_firmware/defaults in
# tenstorrent/tt-ansible): bumping these requires a new image tag.

# UBUNTU_VERSION selects both the base image and the tt-smi release-binary
# flavor (tt-smi-<v>-ubuntu-<UBUNTU_VERSION>). The binary is PyInstaller
# build linked against that Ubuntu release's glibc; the version here just
# has to be glibc-compatible with the container, not with the host.
ARG UBUNTU_VERSION=22.04
ARG TT_FLASH_VERSION=3.6.5
ARG TT_SMI_VERSION=5.2.0
FROM ubuntu:${UBUNTU_VERSION}
ARG UBUNTU_VERSION
ARG TT_FLASH_VERSION
ARG TT_SMI_VERSION

ENV DEBIAN_FRONTEND=noninteractive

# Base toolchain. Kept stable so the large layer caches across builds.
# (Python kept for tt-flash; tt-smi no longer needs it — see below.)
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git python3 python3-pip python3-venv \
        pciutils kmod \
    && rm -rf /var/lib/apt/lists/*

# Flasher-specific tools, isolated so future additions here don't bust the
# base toolchain layer. Also: the GHA buildx cache held a stale combined
# layer that silently dropped these — a separate RUN gives it a fresh
# cache key, and the verify step below catches any future regression.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ipmitool sudo \
    && rm -rf /var/lib/apt/lists/* \
    && command -v ipmitool >/dev/null \
    && command -v sudo     >/dev/null

# tt-flash via pip (no standalone release).
RUN python3 -m venv /opt/tt && \
    /opt/tt/bin/pip install --upgrade pip && \
    /opt/tt/bin/pip install "git+https://github.com/tenstorrent/tt-flash.git@v${TT_FLASH_VERSION}"

RUN printf '#!/bin/sh\nexec /opt/tt/bin/tt-flash "$@"\n' > /usr/local/bin/tt-flash && \
    chmod 0755 /usr/local/bin/tt-flash

# tt-smi as the standalone PyInstaller binary (since v5.1.0). Bundles
# libatomic / libstdc++ / etc., so the container doesn't need any of the
# UMD native deps that the old pip-install approach implicitly required.
# Same approach the driver-build image uses.
RUN curl -fsSL -o /usr/local/bin/tt-smi \
        "https://github.com/tenstorrent/tt-smi/releases/download/v${TT_SMI_VERSION}/tt-smi-${TT_SMI_VERSION}-ubuntu-${UBUNTU_VERSION}" \
 && chmod 0755 /usr/local/bin/tt-smi

COPY flash.sh /usr/local/bin/flash.sh
RUN chmod 0755 /usr/local/bin/flash.sh

ENTRYPOINT ["/usr/local/bin/flash.sh"]
