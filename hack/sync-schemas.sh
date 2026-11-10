#!/usr/bin/env bash
# Vendor the strict resource schemas of one AISIX release.
#
#   hack/sync-schemas.sh v1.9.0
set -euo pipefail
cd "$(dirname "$0")/.."

TAG=${1:?usage: hack/sync-schemas.sh <aisix tag>}
DEST=third_party/aisix
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

curl -fsSL "https://codeload.github.com/api7/aisix/tar.gz/refs/tags/${TAG}" | tar -xz -C "$TMP"
SRC=$(find "$TMP" -maxdepth 3 -type d -path '*/schemas/resources' | head -1)
[ -n "$SRC" ] || { echo "no schemas/resources in api7/aisix@${TAG}" >&2; exit 1; }

rm -rf "$DEST/schemas"
mkdir -p "$DEST/schemas"
cp "$SRC"/*.schema.json "$DEST/schemas/"
echo "$TAG" > "$DEST/VERSION"
echo "vendored $(ls "$DEST/schemas" | wc -l | tr -d ' ') schemas from api7/aisix@${TAG}"
