#!/bin/bash

# Build script for NautilusLB with dynamic versioning
# Usage: ./build/scripts/build.sh [output-binary-name]

set -e

# Default output binary name
OUTPUT_BINARY=${1:-nautiluslb}

# Get git information
GIT_TAG=$(git describe --tags --exact-match 2>/dev/null || echo "")
GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(date -u '+%Y-%m-%d_%H:%M:%S_UTC')

# Determine version
if [ ! -z "$GIT_TAG" ]; then
    VERSION="$GIT_TAG"
else
    # If no tag, use branch name + commit
    GIT_BRANCH=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown")
    VERSION="${GIT_BRANCH}-${GIT_COMMIT}"
fi

echo "Building NautilusLB..."
echo "Version: $VERSION"
echo "Git Tag: $GIT_TAG"
echo "Git Commit: $GIT_COMMIT"
echo "Build Time: $BUILD_TIME"
echo "Output: $OUTPUT_BINARY"
echo ""

# Build with ldflags to inject version information
cd app
go build -ldflags "\
    -X 'github.com/cloudresty/nautiluslb/version.Version=$VERSION' \
    -X 'github.com/cloudresty/nautiluslb/version.GitCommit=$GIT_COMMIT' \
    -X 'github.com/cloudresty/nautiluslb/version.BuildTime=$BUILD_TIME' \
    -X 'github.com/cloudresty/nautiluslb/version.GitTag=$GIT_TAG'" \
    -o "../$OUTPUT_BINARY" .

echo "Build complete: $OUTPUT_BINARY"
