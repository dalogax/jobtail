---
name: jobtail
description: Schedule, run, and debug recurring jobs on the user's machine with the `jobtail` CLI — plain shell commands, or unattended headless runs of claude/opencode/codex on a cron schedule. Use this skill whenever the user wants something to happen on a schedule or repeatedly ("every morning", "nightly", "every 15 minutes", "each Monday", "keep an eye on X", "have an agent do Y every day"), asks what their scheduled jobs are doing, why one failed, or wants to change, pause, or remove one — even if they never say "jobtail" or "cron". Prefer it over writing crontab entries, systemd timers, or launchd plists by hand. Also covers installing jobtail if it isn't there yet.
---

# jobtail

jobtail is a scheduler for recurring jobs on the user's own machine. There are two kinds of job:

- **`cli`**: a shell command. Use this for health checks, backups, cleanup, and scripts.
- **`agent`**: one headless turn of a coding-agent CLI (`claude` by default, or `opencode`/`codex`) with a prompt, run in a working directory. Use this when the work needs judgement: triaging, reviewing, fixing, summarising.

**The CLI is for you, not the user.** The CLI exists so an agent can manage jobs on the user's behalf. The user sees everything in the dashboard, which they open by running `jobtail` with no arguments. It shows jobs, run history and logs, live. Do the CLI work yourself, then point the user at the dashboard. Don't hand them a list of commands to type.

All state lives in one SQLite database. A per-user OS timer runs `jobtail tick` once a minute, and each run that's due gets its own process and log file.

## 1. Make sure it's installed and actually firing

```sh
command -v jobtail || curl -fsSL https://raw.githubusercontent.com/dalogax/jobtail/main/install.sh | sh
```

The installer puts the binary in `~/.local/bin`. If that directory isn't on this shell's `PATH` yet, use `~/.local/bin/jobtail` for the rest of the session.

Jobs only run on schedule if the timer is installed. Check it, and install it if it's missing:

- Linux: `systemctl --user is-active jobtail-tick.timer`
- macOS: `launchctl print gui/$(id -u)/com.github.dalogax.jobtail.tick`
- To install on either: `jobtail install-scheduler --enable`

The timer records the `PATH` that was in effect when `install-scheduler` ran, and scheduled jobs get that `PATH`. So if a job needs a tool that was installed after that point (a new CLI, or `claude` itself), re-run `jobtail install-scheduler --enable`. Otherwise the job fails with "command not found" even though the same command works in your shell.

## 2. Adding a job

```sh
jobtail add <id> --kind cli   --cron "<5-field cron>" --cwd <abs dir> --cmd "<shell command>"
jobtail add <id> --kind agent --cron "<5-field cron>" --cwd <abs dir> --prompt "<task>" \
  [--provider claude|opencode|codex] [--model <alias>] [--permission-mode <mode>]
```

Optional flags for either kind: `--timeout-seconds N`, `--precheck "<cmd>"`, `--precheck-timeout-seconds N`, `--timezone <IANA name>` (the default is `local`), `--keep N` (runs to retain, default 200), `--max-concurrent N` (default 1).

Rules the CLI enforces, which you'd otherwise find out one error at a time:

- `--cwd` is required and must be an existing directory. Pass an absolute path.
- `--cron` takes exactly 5 fields: minute, hour, day-of-month, month, day-of-week. It has no seconds field and no `@daily` shortcuts.
- An agent job requires `--prompt`, and a cli job requires `--cmd`.
- An id that already exists is rejected with `already exists`. To change an existing job, use `jobtail edit`.

Guidelines:

- **Ids**: use short kebab-case names that say what the job does, e.g. `backup-check`, `pr-triage`, `deps-review`. The id is what the user sees in the dashboard.
- **Confirm the schedule in words.** Tell the user something like "weekdays at 09:00 local time". If the timing is ambiguous ("in the morning"), pick a sensible time and say which one you picked.
- **Check the command first.** For a cli job, make sure the command works from `--cwd` before you add the job. The scheduled run starts from a bare environment (the timer's `PATH`, not your shell's aliases or functions), so prefer a script with a shebang over a long one-liner.

### Writing the prompt for an agent job

No human is watching when the job runs, and nobody can answer a question. Write the prompt so one turn can finish the task:

- **Say what "done" looks like, and what to do when there's nothing to do.** For example, "If all dependencies are current, say so in one line and stop."
- **Put the side effects in the prompt.** Say explicitly whether the job should open a PR, commit to a branch, write a file, or only report. The transcript is the only record the user reads afterwards.
- **Include the context a fresh session needs**, such as repo conventions, which branch to use, and who the output is for. The run doesn't share this conversation's memory.
- **Permissions:** by default, `claude` runs with `acceptEdits` and `codex` with `workspace-write`. Only use `bypassPermissions` or `danger-full-access` if the user explicitly asks for it. **Never use `--permission-mode plan`** for a scheduled claude job: plan mode waits for an approval that can never come, and the run hangs until it times out.
- **Check that the task fits within those permissions.** The default modes let the agent edit files, but they don't let it run arbitrary shell commands:
  - With claude's `acceptEdits`, any Bash call that isn't pre-approved is denied, because nobody is there to approve it. That includes `git push`, `gh pr create` and test runners. If the task needs commands like these, allow exactly those in the project's `.claude/settings.json`. For example: `{"permissions": {"allow": ["Bash(git push:*)", "Bash(gh pr create:*)", "Bash(npm test:*)"]}}`. Tell the user you did it.
  - codex's `workspace-write` has no network access by default, so pushes and API calls fail there too.

  If you skip this step, the job "works" in the sense that it runs every time, but it never gets to the part the user asked for.
- **Always set `--timeout-seconds` on agent jobs.** 1800 is a reasonable default. Without one, a stuck run holds its concurrency slot, and every later run is recorded as `skipped_overlap`.

### Prechecks: only run the agent when there's work

An agent run costs time and tokens. If the job only has work sometimes (new issues, failing CI, unread tickets), add a cheap shell gate with `--precheck`. The precheck's exit code decides what happens:

| Precheck exits | What happens |
|---|---|
| `0` | The job runs. The precheck's stdout is appended to the prompt under `--- PENDING ITEMS (from precheck) ---` |
| `1` | The run is recorded as `skipped`, and the agent never starts |
| `2` or higher | The run is recorded as `failed` |

For example: `--precheck 'items=$(gh issue list --label bug --state open --json number,title); [ "$items" != "[]" ] || exit 1; echo "$items"'`. Write the prompt so it works on the pending items it's given. Set `--precheck-timeout-seconds` too.

### Test it before you walk away

Run the new job once, right away, instead of waiting for the schedule:

```sh
jobtail run <id>     # runs in the foreground and prints the output; exit status matches the run
```

If it fails, fix it with `jobtail edit` and run it again. Test the job on the user's data as it is. Don't touch, add, or "freshen" files in their directories to prove that the job picks up changes. Finish by telling the user what's scheduled and when, the result of the test run, and that they can watch it in the dashboard by running `jobtail`.

For an agent job, a test run starts a real agent turn with real side effects. That's normally what the user wants to see. But if the task is destructive or expensive (mass edits, deploys), ask the user before triggering it.

## 3. Inspecting and debugging

Only three commands support `--json`: `list`, `show` and `runs`. Use it with them so you're not parsing tables.

```sh
jobtail list --json            # every job, plus RunCount, LastStatus, LastRunAt
jobtail show <id> --json       # {"job": {...}, "recent_runs": [...]}
jobtail runs <id> --json --limit 10
jobtail log <run-id>           # the full log; agent logs are raw stream-json
```

Things to know when reading the output:

- **Run `Status` values:** `running`, `ok`, `failed`, `timeout`, `skipped` (the precheck said there was nothing to do), and `skipped_overlap` (the previous run was still going).
- **Nullable fields** are objects rather than plain values. `ExitCode` is `{"Int64": 1, "Valid": true}`, and `FinishedAt`, `DurationMs` and `SessionID` use the same shape. Check `Valid` before you use the value.
- **Newest first:** `recent_runs` and `runs` list the newest run first.
- **Agent logs** are the provider's JSON event stream. Read them for what the agent said and did. The final `{"type":"result",...}` line says whether it succeeded.

To diagnose "why did X fail?", run `show <id> --json`, take the newest failed run, and read its `log`. For agent runs, the usual causes are:

- the timer's `PATH` is missing the agent CLI (re-run `install-scheduler --enable`)
- the prompt asked a question nobody could answer
- permissions blocked an edit
- the run hit the timeout

**Resuming an agent run:** `jobtail resume <run-id>` reopens that agent run's session interactively (`claude --resume`, `opencode --session`, or `codex resume`). It needs a terminal, so tell the user to run it themselves, or to press `r` on the run in the dashboard. Don't run it from your tool shell.

## 4. Changing and removing jobs

```sh
jobtail edit <id> [any add flag]     # changes only the fields you pass; --precheck "" clears the precheck
jobtail disable <id> / enable <id>   # pause/resume, keeps history
jobtail rm <id>                      # deletes the job AND its whole run history
jobtail gc                           # prunes runs beyond each job's --keep
```

A job's `--kind` can't be changed after it's created.

Deleting a job with `rm` loses its run history for good. Unless the user clearly asked for deletion, prefer `disable`, and confirm with the user before running `rm` on a job you didn't create in this conversation.

## Things not to do

- **Don't write crontab entries, systemd units or launchd plists for recurring work** when jobtail is available. Jobs created that way don't appear in the dashboard, and their runs aren't logged anywhere the user will look.
- **Don't edit the SQLite database directly**, or touch `~/.local/share/jobtail/` (or `$JOBTAIL_DATA_DIR`). Everything you need is available through the CLI.
- **Don't open the dashboard (`jobtail` or `jobtail tui`) from your own shell.** It's an interactive full-screen program, and it will hang your tool call.
