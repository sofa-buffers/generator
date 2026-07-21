#!/bin/sh
# install.sh — install the sofabgen SofaBuffers code generator.
#
# Quick start:
#   curl -fsSL https://raw.githubusercontent.com/sofa-buffers/generator/main/install.sh | sh
#   wget -qO- https://raw.githubusercontent.com/sofa-buffers/generator/main/install.sh | sh
#
# The script detects your OS/architecture, downloads the matching binary from the
# GitHub release, verifies its SHA-256 checksum, and installs it.
#
# Environment overrides:
#   SOFABGEN_VERSION      release tag to install, e.g. v0.19.4 (default: latest)
#   SOFABGEN_INSTALL_DIR  target directory for the binary
#                         (default: /usr/local/bin, falling back to $HOME/.local/bin)
#
# POSIX sh — no bash-isms; runs under sh, dash, bash, and busybox ash.

set -eu

REPO="sofa-buffers/generator"
BINARY="sofabgen"

# --- pretty output -----------------------------------------------------------
if [ -t 2 ]; then
	B="$(printf '\033[1m')"; R="$(printf '\033[0m')"
	GREEN="$(printf '\033[32m')"; YELLOW="$(printf '\033[33m')"; REDC="$(printf '\033[31m')"
else
	B=""; R=""; GREEN=""; YELLOW=""; REDC=""
fi
info()  { printf '%s==>%s %s\n' "$GREEN" "$R" "$*" >&2; }
warn()  { printf '%swarning:%s %s\n' "$YELLOW" "$R" "$*" >&2; }
error() { printf '%serror:%s %s\n' "$REDC" "$R" "$*" >&2; exit 1; }

# --- prerequisites -----------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
	dl() { curl -fSL --proto '=https' --tlsv1.2 -o "$1" "$2"; }
elif command -v wget >/dev/null 2>&1; then
	dl() { wget -qO "$1" "$2"; }
else
	error "need either curl or wget on PATH"
fi

# --- detect platform ---------------------------------------------------------
os="$(uname -s)"
case "$os" in
	Linux)                      os="linux" ;;
	Darwin)                     os="darwin" ;;
	MINGW*|MSYS*|CYGWIN*|Windows_NT) os="windows" ;;
	*) error "unsupported operating system: $os" ;;
esac

arch="$(uname -m)"
case "$arch" in
	x86_64|amd64)   arch="amd64" ;;
	i386|i686|x86)  arch="386" ;;
	aarch64|arm64)  arch="arm64" ;;
	armv7l|armv6l|arm) arch="arm" ;;
	*) error "unsupported architecture: $arch" ;;
esac

# Guard against combinations we do not ship a binary for (see .github/workflows/release.yml).
case "$os-$arch" in
	linux-amd64|linux-386|linux-arm64|linux-arm) ;;
	darwin-amd64|darwin-arm64) ;;
	windows-amd64|windows-386|windows-arm64) ;;
	*) error "no prebuilt sofabgen binary for $os/$arch — build from source with 'go install github.com/$REPO/cmd/sofabgen@latest'" ;;
esac

ext=""
[ "$os" = "windows" ] && ext=".exe"
asset="${BINARY}-${os}-${arch}${ext}"

# --- resolve download URL ----------------------------------------------------
if [ -n "${SOFABGEN_VERSION:-}" ]; then
	base="https://github.com/$REPO/releases/download/$SOFABGEN_VERSION"
	ver_label="$SOFABGEN_VERSION"
else
	# GitHub redirects /releases/latest/download/<asset> to the newest release asset.
	base="https://github.com/$REPO/releases/latest/download"
	ver_label="latest"
fi

# --- download ----------------------------------------------------------------
tmp="$(mktemp -d 2>/dev/null || mktemp -d -t sofabgen)"
trap 'rm -rf "$tmp"' EXIT INT TERM

info "downloading $asset ($ver_label) for $os/$arch"
dl "$tmp/$asset"        "$base/$asset"        || error "download failed: $base/$asset"
dl "$tmp/$asset.sha256" "$base/$asset.sha256" || error "download failed: $base/$asset.sha256"

# --- verify checksum ---------------------------------------------------------
if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "$tmp/$asset" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
	actual="$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')"
else
	actual=""
	warn "no sha256sum/shasum found — skipping checksum verification"
fi
expected="$(awk '{print $1}' "$tmp/$asset.sha256")"
if [ -n "$actual" ]; then
	if [ "$actual" != "$expected" ]; then
		error "checksum mismatch for $asset
  expected: $expected
  actual:   $actual"
	fi
	info "checksum verified"
fi

chmod +x "$tmp/$asset"

# --- choose install dir ------------------------------------------------------
dir="${SOFABGEN_INSTALL_DIR:-}"
use_sudo=""
if [ -z "$dir" ]; then
	if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		dir="/usr/local/bin"
	elif [ -d /usr/local/bin ] && command -v sudo >/dev/null 2>&1; then
		dir="/usr/local/bin"; use_sudo="sudo"
	else
		dir="$HOME/.local/bin"
	fi
fi
target="$dir/${BINARY}${ext}"

# --- install -----------------------------------------------------------------
if [ -n "$use_sudo" ]; then
	info "installing to $target (using sudo)"
	sudo mkdir -p "$dir"
	sudo mv "$tmp/$asset" "$target"
else
	mkdir -p "$dir" 2>/dev/null || error "cannot create $dir — set SOFABGEN_INSTALL_DIR to a writable path"
	if ! mv "$tmp/$asset" "$target" 2>/dev/null; then
		error "cannot write to $dir — set SOFABGEN_INSTALL_DIR to a writable path (e.g. \$HOME/.local/bin)"
	fi
fi

info "installed ${B}${BINARY}${R} to ${B}${target}${R}"

# --- post-install hints ------------------------------------------------------
case ":${PATH}:" in
	*":$dir:"*) ;;
	*) warn "$dir is not on your PATH — add this to your shell profile:
  export PATH=\"$dir:\$PATH\"" ;;
esac

# Report the installed version when the binary can run on this host.
if [ "$os" != "windows" ] || [ -n "${MSYSTEM:-}${WINDIR:-}" ]; then
	if v="$("$target" --version 2>/dev/null)"; then
		info "sofabgen $v ready — run '${B}sofabgen --help${R}' to get started"
	fi
fi
