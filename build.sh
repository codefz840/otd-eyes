#!/bin/sh
# Build single-file binaries (no runtime dependencies) into dist/.
set -e
cd "$(dirname "$0")"
mkdir -p dist
export CGO_ENABLED=0
GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/tablet-eyes-linux-amd64 .
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/tablet-eyes-windows-amd64.exe .
ls -lh dist
