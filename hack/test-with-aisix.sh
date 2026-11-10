#!/usr/bin/env bash
# Run the Go tests with the real aisix binary (AISIX_BIN) inside a Linux
# container, since the binary is built for the gateway image.
#
#   hack/test-with-aisix.sh [go test args]
set -euo pipefail
cd "$(dirname "$0")/.."

AISIX_IMAGE=${AISIX_IMAGE:-docker.io/api7/aisix:$(sed 's/^v//' third_party/aisix/VERSION)}
GO_IMAGE=${GO_IMAGE:-golang:1.26-trixie}

mkdir -p bin
docker run --rm --entrypoint cat "$AISIX_IMAGE" /usr/local/bin/aisix > bin/aisix-linux
chmod +x bin/aisix-linux

MODCACHE=$(go env GOMODCACHE 2>/dev/null || echo "$HOME/go/pkg/mod")
mkdir -p "$MODCACHE"
docker run --rm \
  -v "$PWD:/src" -w /src \
  -v "$MODCACHE:/go/pkg/mod" \
  -e AISIX_BIN=/src/bin/aisix-linux \
  -e GOFLAGS=-buildvcs=false \
  "$GO_IMAGE" go test "${@:-./...}"
