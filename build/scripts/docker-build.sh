#!/bin/bash

# Docker build script for NautilusLB with dynamic versioning
# Usage: ./build/scripts/docker-build.sh [image-tag]

set -e

# Default image tag
IMAGE_TAG=${1:-nautiluslb:latest}

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

echo "Building NautilusLB Docker image..."
echo "Image Tag: $IMAGE_TAG"
echo "Version: $VERSION"
echo "Git Tag: $GIT_TAG"
echo "Git Commit: $GIT_COMMIT" 
echo "Build Time: $BUILD_TIME"
echo ""

# Build Docker image with build args
docker build \
    --build-arg VERSION="$VERSION" \
    --build-arg GIT_COMMIT="$GIT_COMMIT" \
    --build-arg BUILD_TIME="$BUILD_TIME" \
    --build-arg GIT_TAG="$GIT_TAG" \
    -f build/Dockerfile \
    -t "$IMAGE_TAG" \
    .

echo "Docker build complete: $IMAGE_TAG"
