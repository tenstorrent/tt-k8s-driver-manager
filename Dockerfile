# SPDX-License-Identifier: Apache-2.0
# SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

FROM golang:1.26 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/     cmd/
COPY api/     api/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -ldflags="-s -w" -o manager ./cmd/manager

FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.source="https://github.com/tenstorrent/tt-k8s-driver-manager"
LABEL org.opencontainers.image.description="tt-k8s-driver-manager controller — reconciles TenstorrentDriverPolicy + TenstorrentFirmwarePolicy CRs."
LABEL org.opencontainers.image.licenses="Apache-2.0"
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
