#!/usr/bin/env bash
# Stand-in for the real `claude` binary in agent-job e2e tests: emits a
# canned stream-json transcript deterministically, for free, instead of
# spending real API calls on every test run. Controlled by $FAKE_CLAUDE_MODE:
#   ok      (default) - session init + a result with is_error:false, exit 0
#   error   - session init + a result with is_error:true, exit 0
#   crash   - session init, then the process dies with exit 1, no result
#   noinit  - jumps straight to a result with no init/session_id line at all
set -euo pipefail

mode="${FAKE_CLAUDE_MODE:-ok}"
session_id="${FAKE_CLAUDE_SESSION_ID:-fake-session-0000}"

if [[ "$mode" != "noinit" ]]; then
  printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$session_id"
fi

printf '{"type":"assistant","message":{"content":[{"type":"text","text":"working on it"}]}}\n'

case "$mode" in
  ok)
    printf '{"type":"result","subtype":"success","is_error":false,"session_id":"%s","result":"done"}\n' "$session_id"
    ;;
  error)
    printf '{"type":"result","subtype":"error","is_error":true,"session_id":"%s","result":"could not finish"}\n' "$session_id"
    ;;
  noinit)
    printf '{"type":"result","subtype":"success","is_error":false,"result":"done"}\n'
    ;;
  crash)
    echo "fake_claude: simulated crash" >&2
    exit 1
    ;;
  *)
    echo "fake_claude: unknown FAKE_CLAUDE_MODE=$mode" >&2
    exit 2
    ;;
esac
