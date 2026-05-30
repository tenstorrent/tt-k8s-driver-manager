# Flasher image — runs as a per-node Job under the operator.
# Bundles tt-flash, tt-smi, and a small entrypoint that asserts readback.
#
# Pin the *minimum* tool versions here (matching tt_firmware/defaults in
# tenstorrent/tt-ansible): bumping these requires a new image tag.
FROM ubuntu:22.04

ARG TT_FLASH_VERSION=3.6.5
ARG TT_SMI_VERSION=3.0.38

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git python3 python3-pip python3-venv \
        pciutils kmod \
        ipmitool \
    && rm -rf /var/lib/apt/lists/*

# Install tt-flash and tt-smi into a single venv. This is the same approach
# tt-ansible uses on hosts (one venv per tool with a /usr/local/bin wrapper)
# collapsed to one venv inside a single-purpose container.
RUN python3 -m venv /opt/tt && \
    /opt/tt/bin/pip install --upgrade pip && \
    /opt/tt/bin/pip install "git+https://github.com/tenstorrent/tt-flash.git@v${TT_FLASH_VERSION}" && \
    /opt/tt/bin/pip install "git+https://github.com/tenstorrent/tt-smi.git@v${TT_SMI_VERSION}"

# Wrappers on PATH.
RUN printf '#!/bin/sh\nexec /opt/tt/bin/tt-flash "$@"\n' > /usr/local/bin/tt-flash && \
    printf '#!/bin/sh\nexec /opt/tt/bin/tt-smi "$@"\n'   > /usr/local/bin/tt-smi  && \
    chmod 0755 /usr/local/bin/tt-flash /usr/local/bin/tt-smi

COPY flash.sh /usr/local/bin/flash.sh
RUN chmod 0755 /usr/local/bin/flash.sh

ENTRYPOINT ["/usr/local/bin/flash.sh"]
