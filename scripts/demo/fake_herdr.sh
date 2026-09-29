#!/usr/bin/env bash
# Stand-in for `herdr` in the README demo (see demo.tape).
#
# Two reasons it exists rather than letting the real binary through. It must not
# reach a real Herdr: recording on a machine that has one would create actual
# tabs in the viewer's session, invisible to the recording and confusing to
# whoever is using it. And a resume opened in another tab cannot be filmed by a
# single-terminal recorder, so `pane run` executes the command right here — the
# GIF shows the session being picked up where a real Herdr would show it in a
# new tab. That substitution is the one staged thing in the demo.
set -euo pipefail

case "${1:-}${2:+ $2}" in
"tab create")
  # The shape resume.extractPaneID parses.
  printf '{"id":"cli:tab:create","result":{"root_pane":{"pane_id":"w1:p2","cwd":"%s"},"type":"tab_created"}}\n' "$PWD"
  ;;
"pane run")
  # herdr pane run <PANE_ID> <COMMAND>...: drop the subcommand and pane id, run
  # the rest here instead of in a tab nothing can film.
  #
  # Straight to the tty, because resume.Open dispatches this with Run() and no
  # Stdout — it is a fire-and-forget API call to a real Herdr, so anything the
  # command prints would go to /dev/null and the recording would show only
  # jobtail's "resumed in pane …" line.
  #
  # Backgrounded so this returns at once, the way the real socket call does:
  # otherwise resume.Open blocks until the session prints and jobtail's own
  # "resumed in pane …" line lands *after* it, reading backwards.
  shift 3
  if [ -w /dev/tty ]; then
    (
      sleep 0.6
      sh -c "$*" >/dev/tty 2>&1
    ) &
    exit 0
  fi
  exec sh -c "$*"
  ;;
notification*)
  : # a failed run toasts the desktop; silent in a recording
  ;;
*)
  : # anything else jobtail probes for: succeed quietly
  ;;
esac
