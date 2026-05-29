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
modify_path=auto   # auto | yes | no
assume_yes=0

usage() {
  cat <<'EOF'
aistat installer

Usage:
  install.sh [--prefix=DIR] [--no-modify-path] [-y|--yes]

Options:
  --prefix=DIR         install directory (default: /usr/local/bin if writable,
                       else $HOME/.local/bin)
  --no-modify-path     don't offer to append a PATH export to your shell rc
  --modify-path        force the PATH export (skip the consent prompt)
  -y, --yes            assume "yes" to the consent prompt (same as --modify-path)

Environment:
  AISTAT_VERSION=vX.Y.Z   pin a specific release tag (default: latest)
EOF
}

for arg in "$@"; do
  case "$arg" in
    --prefix=?*)       PREFIX="${arg#--prefix=}"; prefix_explicit=1 ;;
    --prefix=)         echo "aistat-install: --prefix requires a value" >&2; exit 2 ;;
    --no-modify-path)  modify_path=no ;;
    --modify-path)     modify_path=yes ;;
    -y|--yes)          assume_yes=1 ;;
    -h|--help)         usage; exit 0 ;;
    *)                 echo "aistat-install: unknown argument: $arg" >&2; exit 2 ;;
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
fell_back=0
if [ "$prefix_explicit" -eq 1 ]; then
  mkdir -p "$PREFIX" || err "could not create --prefix directory: $PREFIX"
else
  if [ -w /usr/local/bin ] 2>/dev/null || { [ "$(id -u)" -eq 0 ] && [ -d /usr/local/bin ]; }; then
    PREFIX="/usr/local/bin"
  else
    : "${HOME:?HOME not set; re-run with --prefix=DIR}"
    PREFIX="$HOME/.local/bin"
    mkdir -p "$PREFIX"
    fell_back=1
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

# --- final summary: success line first, then any follow-up actions ---
installed_version=$("$dest" --version 2>/dev/null | head -n1 || echo "$ver")

if [ -t 1 ]; then
  bold=$(printf '\033[1m')
  reset=$(printf '\033[0m')
else
  bold=""
  reset=""
fi

echo ""
echo "${bold}✓ installed aistat $installed_version → $dest${reset}"

# Is PREFIX already on PATH?
on_path=0
case ":$PATH:" in
  *":$PREFIX:"*) on_path=1 ;;
esac

# Pick the rc file and the line we'd add for the current shell.
case "${SHELL:-}" in
  */zsh)
    rc="$HOME/.zshrc"
    add="export PATH=\"$PREFIX:\$PATH\""
    ;;
  */bash)
    # macOS bash login shells source ~/.bash_profile; Linux uses ~/.bashrc.
    if [ "$os" = "darwin" ]; then rc="$HOME/.bash_profile"; else rc="$HOME/.bashrc"; fi
    add="export PATH=\"$PREFIX:\$PATH\""
    ;;
  */fish)
    rc="$HOME/.config/fish/config.fish"
    add="fish_add_path $PREFIX"
    ;;
  *)
    rc="$HOME/.profile"
    add="export PATH=\"$PREFIX:\$PATH\""
    ;;
esac

sentinel="# Added by aistat-install (https://github.com/drogers0/aistat)"

# Was an earlier run's edit (sentinel) or any reference to PREFIX already written?
already_added=0
if [ -f "$rc" ] && { grep -Fq "$sentinel" "$rc" 2>/dev/null || grep -Fq "$PREFIX" "$rc" 2>/dev/null; }; then
  already_added=1
fi

if [ "$on_path" -eq 1 ]; then
  echo ""
  echo "  Try it: ${bold}aistat -h${reset}"
elif [ "$already_added" -eq 1 ]; then
  echo ""
  echo "  $PREFIX is already referenced in $rc but isn't active in this shell yet."
  echo "  Run: ${bold}. $rc${reset} (or open a new terminal), then: ${bold}aistat -h${reset}"
elif [ "$modify_path" = "no" ]; then
  echo ""
  echo "  $PREFIX is not on your PATH yet. To fix:"
  echo ""
  echo "    echo '$add' >> $rc && . $rc"
  echo ""
  echo "  Then verify: ${bold}aistat -h${reset}"
else
  # Offer to append the line. Ask via /dev/tty so the curl-pipe doesn't block us.
  echo ""
  echo "  $PREFIX is not on your PATH. The installer can append to ${bold}$rc${reset}:"
  echo ""
  echo "    $sentinel"
  echo "    $add"
  echo ""

  proceed=0
  if [ "$modify_path" = "yes" ] || [ "$assume_yes" -eq 1 ]; then
    proceed=1
  elif ( exec </dev/tty >/dev/tty ) 2>/dev/null; then
    # Controlling tty is actually openable (not just present as a file node).
    printf "  Proceed? [Y/n] " > /dev/tty
    read -r ans < /dev/tty || ans=""
    case "$ans" in
      ""|y|Y|yes|YES) proceed=1 ;;
      *) proceed=0 ;;
    esac
  else
    # Non-interactive (CI, no controlling tty): default to yes, matching rustup/bun.
    echo "  (non-interactive shell — proceeding by default; pass --no-modify-path to skip)"
    proceed=1
  fi

  if [ "$proceed" -eq 1 ]; then
    if [ ! -e "$rc" ]; then
      mkdir -p "$(dirname "$rc")" 2>/dev/null || true
      : > "$rc" 2>/dev/null || true
    fi
    if { printf "\n%s\n%s\n" "$sentinel" "$add" >> "$rc"; } 2>/dev/null; then
      echo ""
      echo "  ${bold}Added to $rc.${reset} Activate it now:"
      echo ""
      echo "    ${bold}. $rc${reset}"
      echo ""
      echo "  Then verify: ${bold}aistat -h${reset}"
    else
      echo ""
      echo "  ${bold}Could not write to $rc.${reset} Add it manually:"
      echo ""
      echo "    echo '$add' >> $rc && . $rc"
      echo ""
      echo "  Then verify: ${bold}aistat -h${reset}"
    fi
  else
    echo ""
    echo "  Skipped. To add it yourself later:"
    echo ""
    echo "    echo '$add' >> $rc && . $rc"
  fi
fi

echo ""
