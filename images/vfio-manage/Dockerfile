# vfio-manage: privileged DaemonSet that binds Tenstorrent PCI devices to
# vfio-pci. Built from the repo root so it shares the controller's module.
#
# Unlike the controller this cannot run on distroless: EnsureVFIOPCILoaded
# shells out to modprobe(8) when vfio-pci isn't loaded yet, so the runtime
# needs kmod and the host's /lib/modules bind-mounted.
FROM golang:1.26 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/      cmd/
COPY internal/ internal/
COPY api/      api/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -ldflags="-s -w" -o vfio-manage ./cmd/vfio-manage

FROM debian:bookworm-slim
LABEL org.opencontainers.image.source="https://github.com/tenstorrent/tt-k8s-driver-manager"
LABEL org.opencontainers.image.description="vfio-manage — binds Tenstorrent PCI devices to vfio-pci for VM passthrough."
LABEL org.opencontainers.image.licenses="Apache-2.0"
RUN apt-get update \
    && apt-get install -y --no-install-recommends kmod \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /workspace/vfio-manage /usr/local/bin/vfio-manage
ENTRYPOINT ["/usr/local/bin/vfio-manage"]
