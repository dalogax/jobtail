#!/usr/bin/env bash
# Stand-in for the real `codex` binary in agent-job e2e tests: emits a
# canned `exec --json` event stream deterministically. The "ok" shape
# below (thread.started/turn.started/item.completed) is inferred from
# codex's documented event model, not independently verified against a
# successful real run (see execengine's runCodexAgent comment: this box
# has no stored codex credentials). The "error"/"crash" shapes below ARE
# verified against the real binary's actual 401-unauthorized output.
# Controlled by $FAKE_CODEX_MODE:
#   ok        (default) - thread.started + item.completed, exit 0
#   error     - turn.failed after thread.started, exit 1 (real codex exits
#               non-zero on failure, unlike claude/opencode)
#   crash     - the process dies with exit 1, no thread_id ever emitted
#   nosession - item.completed with no thread.started line at all, exit 0
set -uo pipefail

mode="${FAKE_CODEX_MODE:-ok}"
thread_id="${FAKE_CODEX_THREAD_ID:-01a0fake-0000-0000-0000-000000000000}"

case "$mode" in
  ok)
    printf '{"type":"thread.started","thread_id":"%s"}\n' "$thread_id"
    printf '{"type":"turn.started"}\n'
    printf '{"type":"item.completed","item":{"type":"agent_message","text":"OK"}}\n'
    exit 0
    ;;
  error)
    printf '{"type":"thread.started","thread_id":"%s"}\n' "$thread_id"
    printf '{"type":"turn.started"}\n'
    printf '{"type":"error","message":"simulated failure"}\n'
    printf '{"type":"turn.failed","error":{"message":"simulated failure"}}\n'
    exit 1
    ;;
  crash)
    echo "fake_codex: simulated crash" >&2
    exit 1
    ;;
  nosession)
    printf '{"type":"item.completed","item":{"type":"agent_message","text":"OK"}}\n'
    exit 0
    ;;
  *)
    echo "fake_codex: unknown FAKE_CODEX_MODE=$mode" >&2
    exit 2
    ;;
esac
