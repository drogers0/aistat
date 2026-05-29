#!/bin/sh
# aistat installer — downloads the latest release tarball for your OS/arch,
# verifies its sha256 against the published checksums.txt, and installs the
# `aistat` binary into PREFIX (default: /usr/local/bin, falling back to
# $HOME/.local/bin if /usr/local/bin is not writable).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/drogers0/aistat/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/drogers0/aistat/main/install.sh | sh -s -- --prefix=$HOME/bin
#   AISTAT_VERSION=v2.1.0 curl -fsSL https://raw.githubusercontent.com/drogers0/aistat/main/install.sh | sh

set -eu

REPO="drogers0/aistat"
PREFIX=""
prefix_explicit=0

usage() {
  cat <<'EOF'
aistat installer

Usage:
  install.sh [--prefix=DIR]

Environment:
  AISTAT_VERSION=vX.Y.Z   pin a specific release tag (default: latest)
EOF
}

for arg in "$@"; do
  case "$arg" in
    --prefix=?*) PREFIX="${arg#--prefix=}"; prefix_explicit=1 ;;
    --prefix=)   echo "aistat-install: --prefix requires a value" >&2; exit 2 ;;
    -h|--help)   usage; exit 0 ;;
    *)           echo "aistat-install: unknown argument: $arg" >&2; exit 2 ;;
  esac
done

err() { echo "aistat-install: $*" >&2; exit 1; }

# --- detect OS / arch ---
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *) err "unsupported OS: $os (aistat ships binaries for darwin and linux only)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) err "unsupported architecture: $arch (aistat ships binaries for amd64 and arm64 only)" ;;
esac

# --- pick downloader (with bounded timeout so a hung connection fails fast) ---
if command -v curl >/dev/null 2>&1; then
  fetch()        { curl -fsSL --max-time 60 "$1" -o "$2"; }
  fetch_stdout() { curl -fsSL --max-time 60 "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch()        { wget --timeout=60 -qO "$2" "$1"; }
  fetch_stdout() { wget --timeout=60 -qO- "$1"; }
else
  err "need curl or wget on PATH"
fi

# --- pick sha256 tool ---
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  err "need sha256sum or shasum on PATH"
fi

# --- resolve version ---
version="${AISTAT_VERSION:-}"
if [ -z "$version" ]; then
  version=$(fetch_stdout "https://api.github.com/repos/$REPO/releases/latest" \
    | awk -F'"' '/"tag_name":/ {print $4; exit}') || true
  if [ -z "$version" ]; then
    err "could not resolve latest release tag from GitHub API (rate-limited?). Pin a version with AISTAT_VERSION=vX.Y.Z and retry."
  fi
fi
tag="$version"
case "$tag" in v*) ;; *) tag="v$tag" ;; esac
ver="${tag#v}"

# --- compute URLs ---
archive="aistat_${ver}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"
archive_url="$base/$archive"
checksums_url="$base/checksums.txt"

echo "aistat-install: downloading aistat $tag for $os/$arch"

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t aistat-install) || err "mktemp failed"
trap 'rm -rf "$tmp"' EXIT
trap 'rm -rf "$tmp"; exit 130' INT
trap 'rm -rf "$tmp"; exit 143' TERM

fetch "$archive_url"   "$tmp/$archive"       || err "download failed: $archive_url"
fetch "$checksums_url" "$tmp/checksums.txt"  || err "download failed: $checksums_url"

# --- verify checksum ---
expected=$(awk -v f="$archive" '$2 == f {print $1; exit}' "$tmp/checksums.txt")
[ -n "$expected" ] || err "no checksum entry for $archive in checksums.txt"
actual=$(sha256 "$tmp/$archive")
[ "$expected" = "$actual" ] || err "checksum mismatch for $archive (expected $expected, got $actual)"

# --- extract only the binary (sidesteps any LICENSE/README extras or path-traversal entries) ---
tar -xzf "$tmp/$archive" -C "$tmp" aistat || err "extracting aistat from $archive failed"
chmod +x "$tmp/aistat"

# --- pick / validate prefix ---
if [ "$prefix_explicit" -eq 1 ]; then
  mkdir -p "$PREFIX" || err "could not create --prefix directory: $PREFIX"
else
  if [ -w /usr/local/bin ] 2>/dev/null || { [ "$(id -u)" -eq 0 ] && [ -d /usr/local/bin ]; }; then
    PREFIX="/usr/local/bin"
  else
    : "${HOME:?HOME not set; re-run with --prefix=DIR}"
    PREFIX="$HOME/.local/bin"
    mkdir -p "$PREFIX"
    echo "aistat-install: /usr/local/bin not writable; installing to $PREFIX"
  fi
fi

# --- install ---
dest="$PREFIX/aistat"
if mv "$tmp/aistat" "$dest" 2>/dev/null; then
  :
elif command -v sudo >/dev/null 2>&1; then
  echo "aistat-install: installing to $dest (requires sudo)"
  sudo mv "$tmp/aistat" "$dest" || err "failed to install to $dest"
else
  err "cannot write to $dest (no sudo available); re-run with --prefix=DIR"
fi

# --- strip macOS quarantine attr so first run doesn't hit a Gatekeeper dialog ---
if [ "$os" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
  xattr -d com.apple.quarantine "$dest" 2>/dev/null || true
fi

# --- PATH heads-up after the install destination is final ---
case ":$PATH:" in
  *":$PREFIX:"*) ;;
  *) echo "aistat-install: note — $PREFIX is not on your PATH; add it to your shell rc." ;;
esac

installed_version=$("$dest" --version 2>/dev/null | head -n1 || echo "$ver")
echo "aistat-install: installed aistat $installed_version to $dest"
