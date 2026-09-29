#!/usr/bin/env bash
# Stand-in for `claude` in the README demo: streams a canned stream-json
# transcript with small pauses, so the dashboard's live tail has something
# to show. Arguments are ignored.
set -euo pipefail

emit() { printf '%s\n' "$1"; sleep "${2:-0.7}"; }

emit '{"type":"system","subtype":"init","session_id":"4f1c9a2e-7b3d-4e8a-9c61-d2a8b0f5e913"}' 0.4
emit '{"type":"assistant","message":{"content":[{"type":"text","text":"Checking for outdated dependencies."}]}}'
emit '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"npm outdated --json"}}]}}' 1.0
emit '{"type":"assistant","message":{"content":[{"type":"text","text":"3 packages are behind: express 4.19.2 → 4.21.1, zod 3.22.4 → 3.23.8, vitest 1.6.0 → 2.1.2 (major)."}]}}'
emit '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"npm install express@4.21.1 zod@3.23.8"}}]}}' 0.8
emit '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"npm test"}}]}}' 1.0
emit '{"type":"assistant","message":{"content":[{"type":"text","text":"Tests pass on the two minor bumps. Leaving vitest alone: v2 changes the config format."}]}}'
emit '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"gh pr create --title \"chore(deps): bump express, zod\""}}]}}' 0.8
emit '{"type":"result","subtype":"success","is_error":false,"session_id":"4f1c9a2e-7b3d-4e8a-9c61-d2a8b0f5e913","result":"Opened PR #142 bumping express and zod. vitest 2.x needs a config migration, skipped."}' 0
