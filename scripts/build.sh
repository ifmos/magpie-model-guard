#!/bin/sh
# Cross-platform release build for the model-guard Go backend.
set -e
cd "$(dirname "$0")/.."
mkdir -p bin
for target in \
  darwin/amd64 darwin/arm64 \
  linux/amd64 linux/arm64 \
  windows/amd64; do
  GOOS=${target%/*}
  GOARCH=${target#*/}
  suffix=""
  [ "$GOOS" = "windows" ] && suffix=".exe"
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -trimpath -ldflags="-s -w" -o "bin/model-guard-${GOOS}-${GOARCH}${suffix}" ./cmd/model-guard
  echo "built bin/model-guard-${GOOS}-${GOARCH}${suffix}"
done
# current-platform convenience binary for plugin development
CGO_ENABLED=0 go build -trimpath -o bin/model-guard ./cmd/model-guard
echo "built bin/model-guard"
