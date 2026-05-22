# Base image must match the host OS so the bundled tt-smi venv copies
# cleanly. aus2-dev2 hosts run Ubuntu 22.04 (jammy); python3.10's venv
# built here is the same one the host can execute. Bumping the host OS
# requires bumping this base too.
ARG UBUNTU_VERSION=22.04
ARG TT_SMI_VERSION=3.0.38
FROM ubuntu:${UBUNTU_VERSION}
ARG TT_SMI_VERSION
LABEL org.opencontainers.image.source="https://github.com/tenstorrent/tt-k8s-driver-manager"
LABEL org.opencontainers.image.description="tt-k8s-driver-manager builder pod — clones + builds tt-kmd, insmods, copies tt-smi to host."
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL com.tenstorrent.tt-smi-version="${TT_SMI_VERSION}"

# build-essential: gcc + make for the OOT module build
# gcc-12:          must match the host kernel's compiler. Ubuntu 22.04 LTS
#                  kernels (jammy + HWE 6.x) are built with gcc-12, and the
#                  kernel headers' Makefile hardcodes the binary name. Bump
#                  alongside the base image when the host kernel toolchain
#                  changes.
# libelf-dev:      objtool (shipped in the host's linux-headers tree as a
#                  prebuilt binary) dynamically links libelf.so.1.
# libssl-dev:      kernel module signing pulls in libcrypto.
# python3-venv:    needed to build the tt-smi venv that we ship to the host.
# kmod:            insmod / rmmod against the host kernel
# git:             clone tt-kmd at the requested tag
# psmisc:          fuser, for surfacing holders of /dev/tenstorrent
# curl:            entrypoint patches node labels via the in-cluster API.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        build-essential gcc-12 libelf-dev libssl-dev python3-venv \
        kmod git ca-certificates psmisc curl rsync \
 && rm -rf /var/lib/apt/lists/*

# tt-smi pinned via ARG TT_SMI_VERSION. We build a venv with --copies so
# /opt/tt/bin/python3 is a real binary (not a symlink into the container's
# /usr/bin/python3), making the whole tree portable to the host. The
# entrypoint copies /opt/tt → host /opt/tt and drops a shim at host
# /usr/local/bin/tt-smi.
RUN python3 -m venv --copies /opt/tt \
 && /opt/tt/bin/pip install --no-cache-dir --upgrade pip \
 && /opt/tt/bin/pip install --no-cache-dir \
        "git+https://github.com/tenstorrent/tt-smi.git@v${TT_SMI_VERSION}"
ENV TT_SMI_VERSION=${TT_SMI_VERSION}

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
