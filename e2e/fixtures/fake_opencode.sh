#!/usr/bin/env bash
# Stand-in for the real `opencode` binary in agent-job e2e tests: emits a
# canned `run --format json` event stream deterministically, for free,
# instead of spending real API calls on every test run. Shapes below are
# lifted from a real invocation (see execengine's runOpenCodeAgent
# comment). Controlled by $FAKE_OPENCODE_MODE:
#   ok        (default) - text + step_finish, sessionID present, exit 0
#   error     - a mid-stream {"type":"error"} event, exit 0 anyway (this is
#               the real, confirmed opencode behavior runOpenCodeAgent's
#               JSON-scan exists specifically to catch)
#   crash     - the process dies with exit 1, no sessionID ever emitted
#   nosession - text + step_finish, exit 0, but no sessionID field at all
set -euo pipefail

mode="${FAKE_OPENCODE_MODE:-ok}"
session_id="${FAKE_OPENCODE_SESSION_ID:-ses_fake0000}"

case "$mode" in
  ok)
    printf '{"type":"step_start","sessionID":"%s","part":{"type":"step-start"}}\n' "$session_id"
    printf '{"type":"text","sessionID":"%s","part":{"type":"text","text":"working on it"}}\n' "$session_id"
    printf '{"type":"step_finish","sessionID":"%s","part":{"type":"step-finish","reason":"stop"}}\n' "$session_id"
    ;;
  error)
    printf '{"type":"step_start","sessionID":"%s","part":{"type":"step-start"}}\n' "$session_id"
    printf '{"type":"error","sessionID":"%s","error":{"name":"UnknownError","data":{"message":"simulated failure"}}}\n' "$session_id"
    ;;
  crash)
    echo "fake_opencode: simulated crash" >&2
    exit 1
    ;;
  nosession)
    printf '{"type":"text","part":{"type":"text","text":"working on it"}}\n'
    printf '{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}\n'
    ;;
  *)
    echo "fake_opencode: unknown FAKE_OPENCODE_MODE=$mode" >&2
    exit 2
    ;;
esac
