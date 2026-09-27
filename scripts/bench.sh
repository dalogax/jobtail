#!/usr/bin/env bash
# Footprint benchmark: the things `go test -bench` can't see — how big the
# shipped binary is, how long a process takes to start and exit, and how
# much memory it peaks at.
#
# Startup matters more here than it looks: the systemd timer runs
# `jobtail tick` once a minute, forever, so that process's start-to-exit
# cost is paid ~525,000 times a year on an idle install.
#
# Usage: scripts/bench.sh [label]
# Writes a one-block report to stdout; pass a label to tag the run.
set -euo pipefail

label="${1:-current}"
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

bin="$work/jobtail"
export JOBTAIL_DATA_DIR="$work/data"
# Never let the update check reach the network or its timing will swamp
# everything else being measured here.
export JOBTAIL_UPDATE_API="http://127.0.0.1:9"

echo "=== jobtail footprint: $label ==="

# --- binary size, with the exact flags release.yml ships -----------------
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=v0.0.0-bench" \
  -o "$bin" "$repo/cmd/jobtail"
size=$(stat -c %s "$bin")
printf 'binary            %s bytes (%.2f MiB)\n' "$size" "$(echo "$size" | awk '{print $1/1048576}')"

# --- seed a realistic store ---------------------------------------------
"$bin" add bench-job --kind cli --cron '*/5 * * * *' --cwd /tmp --cmd 'echo hi' >/dev/null
for i in $(seq 1 11); do
  "$bin" add "bench-job-$i" --kind cli --cron '*/5 * * * *' --cwd /tmp --cmd 'echo hi' >/dev/null
done
for _ in $(seq 1 30); do "$bin" run bench-job >/dev/null 2>&1 || true; done
db="$JOBTAIL_DATA_DIR/jobtail.db"
printf 'database          %s bytes, %s runs\n' "$(stat -c %s "$db")" \
  "$("$bin" runs bench-job --json | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')"

# --- wall clock + peak RSS, measured per invocation ----------------------
# python3 rather than /usr/bin/time: wait4 reports ru_maxrss directly and is
# present everywhere a Go toolchain is, which `time -v` is not (absent on
# macOS and on this box).
measure() {
  local n="$1"; shift
  python3 - "$n" "$@" <<'PY'
import os, sys, time
n = int(sys.argv[1]); argv = sys.argv[2:]
devnull = os.open(os.devnull, os.O_WRONLY)
wall, rss = [], 0
for _ in range(n):
    t0 = time.perf_counter()
    pid = os.fork()
    if pid == 0:
        os.dup2(devnull, 1); os.dup2(devnull, 2)
        os.execvp(argv[0], argv)
    _, _, ru = os.wait4(pid, 0)
    wall.append((time.perf_counter() - t0) * 1000)
    rss = max(rss, ru.ru_maxrss)
wall.sort()
print(f"{sum(wall)/len(wall):7.1f} ms mean  {wall[len(wall)//2]:7.1f} ms median  "
      f"{wall[-1]:7.1f} ms max   {rss/1024:6.1f} MiB peak RSS")
PY
}

printf 'jobtail tick      %s\n' "$(measure 20 "$bin" tick)"
printf 'jobtail list      %s\n' "$(measure 20 "$bin" list --json)"
printf 'jobtail runs      %s\n' "$(measure 20 "$bin" runs bench-job --json)"
printf 'jobtail --version %s\n' "$(measure 20 "$bin" --version)"

# --- the dashboard's steady state ---------------------------------------
# What an open, untouched TUI costs per second — the figure that dominates
# jobtail's real-world cost, since the dashboard is left open and the CLI is
# not. Measured against a big agent transcript as well as a small one, because
# the log pipeline used to be the expensive part and its cost scaled with the
# file.
seed_log() { # runs, bytes-per-log -> writes logs and run rows for bench-job
  python3 - "$JOBTAIL_DATA_DIR" "$1" "$2" <<'PY'
import os, sqlite3, sys, datetime, json
datadir, runs, size = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
logs = os.path.join(datadir, "logs"); os.makedirs(logs, exist_ok=True)
prose = "The dependency audit found three outdated packages and one advisory. " * 6
parts, n = ['{"type":"system","subtype":"init","session_id":"bench-0001"}'], 0
while n < size:
    for ev in ({"type": "assistant", "message": {"content": [{"type": "text", "text": prose}]}},
               {"type": "assistant", "message": {"content": [
                   {"type": "tool_use", "name": "Bash", "input": {"command": "go test ./..."}}]}}):
        s = json.dumps(ev); parts.append(s); n += len(s) + 1
body = "\n".join(parts) + "\n"
db = sqlite3.connect(os.path.join(datadir, "jobtail.db"))
now = datetime.datetime.now(datetime.timezone.utc)
for r in range(runs):
    rid = f"seeded-run-{r:05d}"
    p = os.path.join(logs, rid + ".log")
    with open(p, "w") as f: f.write(body)
    started = (now - datetime.timedelta(minutes=runs - r)).strftime("%Y-%m-%dT%H:%M:%S.%f000Z")
    db.execute("INSERT OR REPLACE INTO runs (id,job_id,trigger,status,started_at,finished_at,"
               "exit_code,log_path,duration_ms) VALUES (?,'bench-job','scheduled',?,?,?,0,?,2413)",
               (rid, "failed" if r % 7 == 0 else "ok", started, started, p))
db.commit(); db.close()
print(f"{runs} runs x {len(body)} B")
PY
}

tui="$(dirname "${BASH_SOURCE[0]}")/bench_tui.py"
printf 'seeded            %s\n' "$(seed_log 200 262144)"
printf 'tui 172x40        %s\n' "$(python3 "$tui" "$bin" "$JOBTAIL_DATA_DIR" 172 40)"
printf 'tui  80x24        %s\n' "$(python3 "$tui" "$bin" "$JOBTAIL_DATA_DIR" 80 24)"
printf 'seeded            %s\n' "$(seed_log 60 2097152)"
printf 'tui 172x40 big    %s\n' "$(python3 "$tui" "$bin" "$JOBTAIL_DATA_DIR" 172 40)"
