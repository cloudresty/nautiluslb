# syntax=docker/dockerfile:1

#
# Builder Stage
#
# The build context is the repository root; /.dockerignore keeps it to app/
# minus local secrets (kubeconfig, config.yaml).
#

# golang:1.27.1-trixie
FROM    --platform=$BUILDPLATFORM golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734 AS builder

ARG     TARGETOS
ARG     TARGETARCH
ARG     VERSION=dev
ARG     COMMIT=unknown
ARG     BUILD_DATE=unknown

WORKDIR /src

# Dependencies first, so source edits do not invalidate this layer.
COPY    app/go.mod app/go.sum ./
RUN     --mount=type=cache,target=/go/pkg/mod \
        go mod download && go mod verify

# Build a static binary.
COPY    app/ ./
RUN     --mount=type=cache,target=/go/pkg/mod \
        --mount=type=cache,target=/root/.cache/go-build \
        CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
        go build -trimpath \
        -ldflags="-s -w \
        -X github.com/cloudresty/nautiluslb/internal/version.Version=${VERSION} \
        -X github.com/cloudresty/nautiluslb/internal/version.Commit=${COMMIT} \
        -X github.com/cloudresty/nautiluslb/internal/version.BuildDate=${BUILD_DATE}" \
        -o /out/nautiluslb ./cmd/nautiluslb

#
# Final Image
#
# Runtime contract (unchanged from earlier releases):
#   - binary:      /nautiluslb/nautiluslb
#   - working dir: /nautiluslb  (config.yaml is read from here, so mount it at
#                                /nautiluslb/config.yaml)
#
# Runs as the distroless "nonroot" user (UID/GID 65532). Mounted files such as
# a kubeconfig must be readable by that UID.
#

# gcr.io/distroless/static-debian13:nonroot
FROM    gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

COPY    --from=builder /out/nautiluslb /nautiluslb/nautiluslb

WORKDIR /nautiluslb

USER    65532:65532

EXPOSE  80 443

ENTRYPOINT ["/nautiluslb/nautiluslb"]
