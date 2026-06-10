# Flasher image — runs as a per-node Job under the operator.
# Bundles tt-flash and a small entrypoint that asserts readback.
# tt-smi is NOT bundled: the driver-build DS installs the standalone
# PyInstaller binary at host /usr/local/bin/tt-smi (so its libatomic
# and other native deps are self-contained), and the operator
# bind-mounts that file into this container at the same path.
#
# Pin the *minimum* tool versions here (matching tt_firmware/defaults in
# tenstorrent/tt-ansible): bumping these requires a new image tag.
FROM ubuntu:22.04

ARG TT_FLASH_VERSION=3.6.5

ENV DEBIAN_FRONTEND=noninteractive

# Base toolchain. Kept stable so the large layer caches across builds.
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

# tt-flash venv. tt-smi comes from the host bind-mount.
RUN python3 -m venv /opt/tt && \
    /opt/tt/bin/pip install --upgrade pip && \
    /opt/tt/bin/pip install "git+https://github.com/tenstorrent/tt-flash.git@v${TT_FLASH_VERSION}"

RUN printf '#!/bin/sh\nexec /opt/tt/bin/tt-flash "$@"\n' > /usr/local/bin/tt-flash && \
    chmod 0755 /usr/local/bin/tt-flash

COPY flash.sh /usr/local/bin/flash.sh
RUN chmod 0755 /usr/local/bin/flash.sh

ENTRYPOINT ["/usr/local/bin/flash.sh"]
