#!/bin/sh
# Installs the latest jobtail release binary.
#
#   curl -fsSL https://raw.githubusercontent.com/jarvis0064/jobtail/main/install.sh | sh
#
# Override the install directory with INSTALL_DIR (default: ~/.local/bin).
set -eu

REPO="jarvis0064/jobtail"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

os() {
  case "$(uname -s)" in
    Linux) echo linux ;;
    Darwin) echo darwin ;;
    *) echo "jobtail: unsupported OS $(uname -s)" >&2; exit 1 ;;
  esac
}

arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    arm64|aarch64) echo arm64 ;;
    *) echo "jobtail: unsupported architecture $(uname -m)" >&2; exit 1 ;;
  esac
}

main() {
  asset="jobtail-$(os)-$(arch)"
  url="https://github.com/${REPO}/releases/latest/download/${asset}"

  mkdir -p "$INSTALL_DIR"
  tmp="$(mktemp)"
  trap 'rm -f "$tmp"' EXIT

  echo "jobtail: downloading ${asset}..." >&2
  if ! curl -fsSL "$url" -o "$tmp"; then
    echo "jobtail: could not download $url" >&2
    echo "jobtail: check https://github.com/${REPO}/releases for available builds" >&2
    exit 1
  fi

  chmod +x "$tmp"
  mv "$tmp" "$INSTALL_DIR/jobtail"
  trap - EXIT

  echo "jobtail: installed to ${INSTALL_DIR}/jobtail" >&2
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) echo "jobtail: ${INSTALL_DIR} is not on your PATH — add it to your shell profile" >&2 ;;
  esac
  echo "jobtail: next, run: jobtail install-systemd --enable" >&2
}

main
