#!/bin/sh
# Switchboard installer. Usage:
#
#   curl -fsSL https://raw.githubusercontent.com/switchboard-code/switchboard/main/install.sh | bash
#
# Downloads the latest release (or SB_VERSION=vX.Y.Z for a specific one),
# verifies it against the release's checksums, and installs sb into
# SB_INSTALL_DIR or ~/.local/bin. No sudo: if you want /usr/local/bin, set
# SB_INSTALL_DIR and take responsibility for it.
set -eu

REPO="switchboard-code/switchboard"
INSTALL_DIR="${SB_INSTALL_DIR:-$HOME/.local/bin}"

die() { echo "install: $*" >&2; exit 1; }

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$OS" in
  darwin|linux) ;;
  *) die "unsupported OS $OS; on Windows, download from https://github.com/$REPO/releases" ;;
esac
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) die "unsupported architecture $ARCH" ;;
esac

command -v curl >/dev/null || die "curl is required"
if command -v sha256sum >/dev/null; then
  sha256() { sha256sum "$@" | cut -d' ' -f1; }
elif command -v shasum >/dev/null; then
  sha256() { shasum -a 256 "$@" | cut -d' ' -f1; }
else
  die "sha256sum or shasum is required; the checksum is not optional"
fi

TAG="${SB_VERSION:-}"
if [ -z "$TAG" ]; then
  TAG=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$TAG" ] || die "could not determine the latest release"
fi

VERSION="${TAG#v}"
ASSET="sb_${VERSION}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$TAG"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "installing switchboard $TAG ($OS/$ARCH)"
curl -fsSL "$BASE/$ASSET" -o "$TMP/$ASSET" || die "no build for $OS/$ARCH at $TAG"
curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt" || die "release has no checksums.txt; refusing to install unverified bits"

WANT=$(grep " $ASSET\$" "$TMP/checksums.txt" | cut -d' ' -f1)
[ -n "$WANT" ] || die "checksums.txt has no entry for $ASSET"
GOT=$(sha256 "$TMP/$ASSET")
[ "$GOT" = "$WANT" ] || die "checksum mismatch; nothing was installed"

tar -xzf "$TMP/$ASSET" -C "$TMP" || die "could not unpack $ASSET"
mkdir -p "$INSTALL_DIR"
mv "$TMP/sb" "$INSTALL_DIR/sb"
chmod 755 "$INSTALL_DIR/sb"

echo "installed to $INSTALL_DIR/sb"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "note: $INSTALL_DIR is not on your PATH" ;;
esac
