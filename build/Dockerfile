#
# Builder Stage
#

# Golang image
FROM    golang:1.24-bookworm AS builder

RUN     set -x && \
            DEBIAN_FRONTEND=noninteractive \
            apt-get update && \
            apt-get install -y \
                ca-certificates

# Set the Current Working Directory inside the container
WORKDIR /nautiluslb

ENV     CGO_ENABLED=0

# Copy all required files
COPY    app/ .
COPY    build/scripts/ ../scripts/

# Install git for version information
RUN     set -x && \
            DEBIAN_FRONTEND=noninteractive \
            apt-get update && \
            apt-get install -y \
                git

# Go Dependencies
RUN     go clean -modcache
RUN     go mod tidy -compat=1.24

# Build the Go app with version information
# Note: In Docker context, git information might not be available
# so we'll use build args for version information
ARG     VERSION=dev
ARG     GIT_COMMIT=unknown
ARG     BUILD_TIME=unknown
ARG     GIT_TAG=dev

RUN     go build -ldflags "\
            -X 'github.com/cloudresty/nautiluslb/version.Version=${VERSION}' \
            -X 'github.com/cloudresty/nautiluslb/version.GitCommit=${GIT_COMMIT}' \
            -X 'github.com/cloudresty/nautiluslb/version.BuildTime=${BUILD_TIME}' \
            -X 'github.com/cloudresty/nautiluslb/version.GitTag=${GIT_TAG}'" \
            -o nautiluslb .

#
# Final Image
#

# Start fresh from a smaller image
FROM    debian:bookworm-slim

# Copy the output from builder stage
COPY    --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY    --from=builder /nautiluslb/nautiluslb /nautiluslb/nautiluslb

# Set the Current Working Directory inside the container
WORKDIR /nautiluslb

EXPOSE  80 443

# Execute the application when the container starts
CMD     ["/nautiluslb/nautiluslb"]