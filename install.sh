#!/bin/sh
# Installs the latest jobtail release binary.
#
#   curl -fsSL https://raw.githubusercontent.com/dalogax/jobtail/main/install.sh | sh
#
# Override the install directory with INSTALL_DIR (default: ~/.local/bin).
# Set JOBTAIL_SKILL=yes or JOBTAIL_SKILL=no to answer the agent-skill prompt
# ahead of time (e.g. in CI, or when no terminal is attached).
set -eu

REPO="dalogax/jobtail"
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
    *) add_to_path ;;
  esac
  offer_skill "$INSTALL_DIR/jobtail"
  echo "jobtail: next, run: jobtail install-scheduler --enable" >&2
}

# offer_skill installs the jobtail skill for the coding agents on this
# machine (claude, opencode, codex), which teaches them to drive jobtail's
# CLI for you. It asks first. The script is usually piped into sh, so stdin
# is the script itself: the answer is read from /dev/tty instead, and with
# no terminal it only prints the command to run.
offer_skill() {
  bin="$1"
  "$bin" --help 2>/dev/null | grep -q "install-skill" || return  # a release older than the skill
  targets="$("$bin" install-skill --dry-run 2>/dev/null)" || targets=""
  if [ -z "$targets" ]; then
    echo "jobtail: no supported coding agent found (claude, opencode, codex); add one later with: jobtail install-skill" >&2
    return
  fi
  agents="$(printf '%s\n' "$targets" | cut -f1 | paste -sd, - | sed 's/,/, /g')"

  answer="${JOBTAIL_SKILL:-}"
  if [ -z "$answer" ]; then
    if ! (exec </dev/tty) 2>/dev/null; then
      echo "jobtail: to teach your agents (${agents}) to use jobtail, run: jobtail install-skill" >&2
      return
    fi
    printf 'jobtail: install the jobtail skill for your coding agents (%s)? [Y/n] ' "$agents" >&2
    read -r answer </dev/tty || answer=n
  fi

  case "$answer" in
    ""|[Yy]*) "$bin" install-skill >&2 ;;
    *) echo "jobtail: skipped; run it any time with: jobtail install-skill" >&2 ;;
  esac
}

# add_to_path appends INSTALL_DIR to the current shell's profile, once, so
# a fresh shell resolves `jobtail` with no path needed — not just a printed
# warning left for the user to act on themselves.
add_to_path() {
  marker="# added by the jobtail installer"
  case "${SHELL:-}" in
    */fish)
      profile="$HOME/.config/fish/config.fish"
      line="set -gx PATH $INSTALL_DIR \$PATH $marker"
      ;;
    */zsh)
      profile="$HOME/.zshrc"
      line="export PATH=\"$INSTALL_DIR:\$PATH\" $marker"
      ;;
    *)
      profile="$HOME/.bashrc"
      line="export PATH=\"$INSTALL_DIR:\$PATH\" $marker"
      ;;
  esac

  if [ -f "$profile" ] && grep -qF "$marker" "$profile" 2>/dev/null; then
    return
  fi
  mkdir -p "$(dirname "$profile")"
  printf '\n%s\n' "$line" >> "$profile"
  echo "jobtail: added ${INSTALL_DIR} to PATH in ${profile} (open a new shell, or run: export PATH=\"${INSTALL_DIR}:\$PATH\")" >&2
}

main
