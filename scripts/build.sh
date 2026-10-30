#!/usr/bin/env bash
# Build the CLIProxyAPI shared-library plugin.
#
#   scripts/build.sh                       # linux/amd64 .so (Docker; this machine has no Go)
#   scripts/build.sh --target darwin       # macOS .dylib
#   scripts/build.sh --target windows      # Windows .dll
#   scripts/build.sh -o dist/custom.so     # explicit output path
#   scripts/build.sh --go                  # native build when a Go 1.26 toolchain is on PATH
#
# Output lands in dist/. The default name follows CPA's discovery rule:
# <plugin-id>-v<version>.<ext>; the -v<version> suffix is what makes the host
# see a hot-swapable upgrade instead of the same file.
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="golang:1.26-bookworm"
TARGET="linux"
MODE="docker"
OUTPUT=""

usage() {
  sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target) TARGET="${2:-}"; shift 2 ;;
    --target=*) TARGET="${1#*=}"; shift ;;
    -o|--output) OUTPUT="${2:-}"; shift 2 ;;
    --go) MODE="native"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

case "$TARGET" in
  linux)  GOOS="linux";  EXT="so" ;;
  darwin|mac|macos|osx) GOOS="darwin"; EXT="dylib" ;;
  windows|win) GOOS="windows"; EXT="dll" ;;
  *) echo "unknown --target: $TARGET (linux|darwin|windows)" >&2; exit 2 ;;
esac

VERSION="$(sed -n 's/^[[:space:]]*Version *= *"\([^"]*\)".*$/\1/p' internal/quotaplugin/plugin.go | head -1)"
PLUGIN_ID="$(sed -n 's/^[[:space:]]*Name *= *"\([^"]*\)".*$/\1/p' internal/quotaplugin/plugin.go | head -1)"
if [[ -z "$VERSION" || -z "$PLUGIN_ID" ]]; then
  echo "could not read Name/Version from internal/quotaplugin/plugin.go" >&2
  exit 1
fi

OUTPUT="${OUTPUT:-dist/${PLUGIN_ID}-v${VERSION}.${EXT}}"
mkdir -p "$(dirname "$OUTPUT")"

echo "building ${OUTPUT} for ${GOOS} (${MODE})"
if [[ "$MODE" == "native" ]]; then
  CGO_ENABLED=1 GOOS="$GOOS" go build -buildmode=c-shared -o "$OUTPUT" ./cmd/plugin
else
  # MSYS_NO_PATHCONV keeps Git Bash on Windows from rewriting the /src mount.
  MSYS_NO_PATHCONV=1 docker run --rm \
    -v "$(pwd -W 2>/dev/null || pwd):/src" -w /src \
    -e GOOS="$GOOS" -e OUTPUT="$OUTPUT" \
    "$IMAGE" \
    sh -c 'apt-get update -qq && apt-get install -y --no-install-recommends -qq build-essential >/dev/null && \
           CGO_ENABLED=1 go build -buildmode=c-shared -o "$OUTPUT" ./cmd/plugin'
fi

# -buildmode=c-shared emits a C header next to the library; consumers of the
# JSON RPC surface do not need it.
rm -f "${OUTPUT%.*}.h"

echo
echo "built: ${OUTPUT}"
if command -v file >/dev/null 2>&1; then
  file "$OUTPUT" || true
fi
