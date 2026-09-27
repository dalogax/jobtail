#!/bin/bash
# End-to-end check that jobtail actually schedules on macOS.
#
# The launchd backend is written and tested from Linux (see PRD §17), so the
# parts that need a real Mac — launchctl accepting the agent, the interval
# firing, and a spawned run surviving tick's exit — are checked here instead.
#
#   scripts/verify_macos.sh [path-to-jobtail]
#
# It adds one temporary job named _jobtail-selftest, waits for the scheduler
# to fire it, then removes it again. Your other jobs are untouched.
set -uo pipefail

bin="${1:-$(command -v jobtail || true)}"
label="com.github.dalogax.jobtail.tick"
job="_jobtail-selftest"
domain="gui/$(id -u)"
fail=0

say()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[32mok\033[0m    %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; fail=1; }
note() { printf '        %s\n' "$*"; }

cleanup() {
  if [ -n "${bin:-}" ] && [ -x "$bin" ]; then
    "$bin" rm "$job" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

[ "$(uname -s)" = "Darwin" ] || { echo "this script is for macOS; uname says $(uname -s)" >&2; exit 2; }
[ -n "$bin" ] && [ -x "$bin" ] || { echo "jobtail not found — pass its path as the first argument" >&2; exit 2; }

say "1. the binary runs at all"
# On Apple Silicon an unsigned or damaged binary is SIGKILLed on exec, which
# looks identical to the program crashing. Separate the two now.
if version="$("$bin" --version 2>&1)"; then
  ok "$version"
else
  status=$?
  bad "jobtail could not start (exit $status): $version"
  [ $status -eq 137 ] && note "killed on exec — the binary is unsigned or damaged; re-download it"
  exit 1
fi
if codesign -dv "$bin" >/dev/null 2>&1; then
  ok "code signature present"
else
  note "no code signature reported by codesign (fine on Intel; on Apple Silicon it would not have run)"
fi

say "2. install the scheduler"
if out="$("$bin" install-scheduler --enable 2>&1)"; then
  ok "install-scheduler --enable succeeded"
  note "$(echo "$out" | tr '\n' ' ')"
else
  bad "install-scheduler failed:"
  note "$out"
  exit 1
fi

say "3. launchd accepted the agent"
if print_out="$(launchctl print "$domain/$label" 2>&1)"; then
  ok "loaded in $domain"
  state=$(echo "$print_out" | awk -F' = ' '/^[[:space:]]*state = /{print $2; exit}')
  [ -n "$state" ] && note "state = $state"
else
  bad "launchctl does not know about $label"
  note "$print_out"
  exit 1
fi

say "4. a scheduled job actually fires"
marker="$(mktemp -t jobtail-selftest)"
rm -f "$marker"
"$bin" rm "$job" >/dev/null 2>&1 || true
if ! out="$("$bin" add "$job" --kind cli --cron '* * * * *' --cwd /tmp \
      --cmd "/bin/date >> '$marker'; echo selftest-ran" 2>&1)"; then
  bad "could not create the test job: $out"
  exit 1
fi
ok "added $job (every minute)"

note "waiting up to 150s for the scheduler to pick it up..."
fired=""
for _ in $(seq 1 30); do
  sleep 5
  if [ -s "$marker" ]; then fired=yes; break; fi
  printf '.'
done
printf '\n'

if [ -n "$fired" ]; then
  ok "the job ran on its own — launchd fired tick, and the run survived tick exiting"
  note "$(head -1 "$marker")"
else
  bad "nothing fired within 150 seconds"
  note "the agent is loaded, so tick is being started but its runs aren't landing."
  note "look at:  $bin runs $job"
  note "          tail -20 '${JOBTAIL_DATA_DIR:-$HOME/.local/share/jobtail}/scheduler.log'"
  note "if runs appear but never finish, AbandonProcessGroup isn't taking effect."
fi
rm -f "$marker"

say "5. what the run recorded"
"$bin" runs "$job" 2>&1 | head -5

if [ "$fail" -eq 0 ]; then
  say "all good — scheduling works on this Mac"
else
  say "something is wrong; see the FAIL lines above"
fi
exit "$fail"
