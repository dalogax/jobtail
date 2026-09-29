#!/usr/bin/env bash
# Stand-in for `claude` in the README demo (see demo.tape).
#
# Two modes, matching the two ways jobtail invokes the real binary:
#
#   claude -p ... --output-format stream-json    a scheduled run: stream a
#     canned transcript with small pauses, so the dashboard's live tail has
#     something to show.
#   claude --resume <session>                    a resumed session: print what
#     picking that conversation back up looks like.
#
# The streamed transcript deliberately carries the same event shapes a real run
# does — tool_use paired with a tool_result by id, a thinking block, and a
# result event with its metrics — because that is what the log pane renders. A
# transcript without tool_result blocks would leave every tool call with an
# empty body and nothing for the pane's folding to fold.
set -euo pipefail

session=4f1c9a2e-7b3d-4e8a-9c61-d2a8b0f5e913

# --resume: not a stream, just a plausible picked-up session.
for arg in "$@"; do
  case $arg in
  --resume | resume | --session)
    cat <<'TXT'
  Claude Code · Sonnet 5 · ~/code/shop
  resumed session 4f1c9a2e · 5 turns restored

❯ Check for outdated deps. Open a PR if safe to update.

● Opened PR #142 bumping express and zod. vitest 2.x needs a config
  migration, skipped.

❯ ▋
TXT
    exit 0
    ;;
  esac
done

emit() { printf '%s\n' "$1"; sleep "${2:-0.7}"; }

# tool_result content is JSON-encoded on one line; \n is what the pane splits on.
outdated='{\n  \"express\": { \"current\": \"4.19.2\", \"latest\": \"4.21.1\" },\n  \"zod\": { \"current\": \"3.22.4\", \"latest\": \"3.23.8\" },\n  \"vitest\": { \"current\": \"1.6.0\", \"latest\": \"2.1.2\" }\n}'
installed='added 3 packages, removed 1 package, changed 12 packages\n\nrun `npm audit` for details\n\n> shop@1.4.0 postinstall\n> node scripts/patch-deps.js\n\npatched 2 files\n\nfound 0 vulnerabilities'
tests='> shop@1.4.0 test\n> vitest run\n\n RUN  v1.6.0 /home/demo/code/shop\n\n ✓ src/cart.test.ts (14 tests) 241ms\n ✓ src/checkout.test.ts (9 tests) 188ms\n ✓ src/api/orders.test.ts (22 tests) 410ms\n ✓ src/api/products.test.ts (17 tests) 302ms\n\n Test Files  4 passed (4)\n      Tests  62 passed (62)\n   Duration  1.19s'

emit "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"$session\"}" 0.4
emit '{"type":"assistant","message":{"content":[{"type":"text","text":"Checking for outdated dependencies."}]}}'

emit '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"npm outdated --json"}}]}}' 0.9
emit "{\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"t1\",\"content\":\"$outdated\"}]}}" 0.5

emit '{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"express and zod are both patch/minor bumps inside the same major, so\nsemver says they are safe and the test suite will confirm it.\n\nvitest 1.6 -> 2.1 is a major. v2 renamed the workspace config key and\ndropped the deprecated environmentMatchGlobs, so this repo would need a\nconfig migration before it builds. That is not a safe unattended bump;\nreport it instead of attempting it.","signature":"demo"}]}}' 0.8

emit '{"type":"assistant","message":{"content":[{"type":"text","text":"3 packages are behind: express 4.19.2 → 4.21.1, zod 3.22.4 → 3.23.8, vitest 1.6.0 → 2.1.2 (major)."}]}}'

emit '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"npm install express@4.21.1 zod@3.23.8"}}]}}' 0.8
emit "{\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"t2\",\"content\":\"$installed\"}]}}" 0.5

emit '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"npm test"}}]}}' 1.0
emit "{\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"t3\",\"content\":\"$tests\"}]}}" 0.6

emit '{"type":"assistant","message":{"content":[{"type":"text","text":"Tests pass on the two minor bumps. Leaving vitest alone: v2 changes the config format."}]}}'

emit '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t4","name":"Bash","input":{"command":"gh pr create --title \"chore(deps): bump express, zod\""}}]}}' 0.8
emit '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t4","content":"https://github.com/demo/shop/pull/142"}]}}' 0.5

emit "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"$session\",\"num_turns\":5,\"duration_ms\":42180,\"total_cost_usd\":0.0914,\"result\":\"Opened PR #142 bumping express and zod. vitest 2.x needs a config migration, skipped.\"}" 0
