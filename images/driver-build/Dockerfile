# UBUNTU_VERSION selects both the base image and the tt-smi release-binary
# flavor (tt-smi-<v>-ubuntu-<UBUNTU_VERSION>). The binary is a PyInstaller
# build linked against the glibc of that Ubuntu release, so it must match
# the host OS — aus2-dev2 hosts run Ubuntu 22.04 (jammy). Bumping the host
# OS requires bumping this too.
ARG UBUNTU_VERSION=22.04
ARG TT_SMI_VERSION=5.2.0
FROM ubuntu:${UBUNTU_VERSION}
ARG UBUNTU_VERSION
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

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
