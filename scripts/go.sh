#!/bin/sh
# Run the pinned Go toolchain, downloading it on first use (no sudo, no system
# install). Every machine that builds dynamoat, including the build agents and
# the gate, uses this instead of whatever "go" is on the PATH.
#
#   scripts/go.sh version
#   scripts/go.sh -C server test ./...
set -eu

GO_VERSION=1.27.3
ROOT="${DYNAMOAT_GO_CACHE:-$HOME/.cache/dynamoat}"
GOROOT_DIR="$ROOT/go$GO_VERSION"

if [ ! -x "$GOROOT_DIR/bin/go" ]; then
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case $(uname -m) in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "go.sh: unsupported machine $(uname -m)" >&2; exit 1 ;;
  esac
  # Checksums from https://go.dev/dl/?mode=json; update them with GO_VERSION.
  case "$os-$arch" in
    darwin-amd64) want=b6f8e931e4ab497fcde6da8ae4499a58f9d258e55c8a80824a516eae5950a750 ;;
    darwin-arm64) want=dd3908cf20959d798c1e06ebe5b72213f0505ce01373b40ce3110975a6cd99c2 ;;
    linux-amd64)  want=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40 ;;
    linux-arm64)  want=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49 ;;
    *) echo "go.sh: unsupported platform $os-$arch" >&2; exit 1 ;;
  esac
  file="go$GO_VERSION.$os-$arch.tar.gz"
  mkdir -p "$ROOT"
  tmp=$(mktemp -d "$ROOT/.dl.XXXXXX")
  trap 'rm -rf "$tmp"' EXIT
  echo "go.sh: downloading $file" >&2
  curl -fsSL "https://go.dev/dl/$file" -o "$tmp/$file"
  if command -v sha256sum >/dev/null; then got=$(sha256sum "$tmp/$file" | cut -d' ' -f1)
  else got=$(shasum -a 256 "$tmp/$file" | cut -d' ' -f1); fi
  if [ -z "$want" ] || [ "$want" != "$got" ]; then
    echo "go.sh: checksum mismatch for $file (want '$want', got '$got')" >&2; exit 1
  fi
  tar -xzf "$tmp/$file" -C "$tmp"
  rm -rf "$GOROOT_DIR"
  mv "$tmp/go" "$GOROOT_DIR"
fi

GOTOOLCHAIN=local exec "$GOROOT_DIR/bin/go" "$@"
