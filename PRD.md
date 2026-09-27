# jobtail — a scheduled job & agent-run dashboard for Herdr

Status: decisions finalized — see §12
Owner: Dani
Target host: this machine (Omarchy/Arch, Herdr 0.9.x, Claude Code 2.1.x, systemd user services available)

## 1. Problem

Herdr already herds *interactive* agent panes (Claude, Codex, etc.) across workspaces/tabs. There's no way to:

- schedule a recurring job (either a plain shell command or a headless agent task),
- see it in one place — enabled/disabled, how many times it ran, when it runs next, what type it is, whether the last run succeeded,
- drill from "the job" → "its run history" → "the actual log of one run" (raw CLI output, or the agent's transcript),
- do all of the above with a single CLI command to register a new job, from inside a terminal, without leaving the keyboard.

This is for a one-person home server. It needs to be reliable across reboots, cheap to run, and to *feel* like it belongs next to the rest of Herdr rather than being a separate app to alt-tab into.

## 2. Research summary (what already exists)

| Option | What it is | Verdict |
|---|---|---|
| **Cronicle**, **Dkron**, **JobRunr**, **Rundeck** | Mature self-hosted job schedulers, web UI, run history, retries. | Built for fleets/servers-as-workers, not for "run a coding agent headless in a repo." Would mean running + maintaining a whole extra web service for one box. No Herdr integration. Overkill. |
| **systemd timers + tui-cron-style viewers** | `systemd` is already the reliable heartbeat on this machine. | Great as the *tick* mechanism, not a UI or a data model for runs/agents. |
| **Orca ADE** (`onorca.dev`) | A full "agentic dev environment" — terminals, previews, mobile control, and a first-class **Automations** feature: schedule a coding agent (Claude Code, Codex, Hermes, …) on a cron/RRULE trigger, with a run-history table (outcome, last-run time, filters) and a CLI (`orca automations create/list/show/run/runs/remove`). | This is the closest thing to "exactly what you're describing," but it's a *replacement* for Herdr's whole workspace model, not an add-on to it. Adopting it means giving up Herdr's pane/workspace/worktree workflow you already use daily (3 active workspaces, per-project tabs, remote machines). Its CLI verb shape (`create/list/show/run/runs/remove`) is worth stealing wholesale, though. |
| **herdr-sched** (`husniadil/herdr-sched`, community Herdr plugin) | A real, working Herdr plugin: a Go daemon (`hsched`) with cron jobs *and* webhook/file-watcher triggers, that fires actions (`task`/`mail`/`dispatch`/`shell`) into sibling plugins, plus a single read-only dashboard popup pane showing next-fire times and a tail of the event trail. | Closest existing Herdr-native plugin. But its data/run model doesn't match what you asked for: runs aren't stored as durable rows (they're an append-only trail capped at the newest 1000 events *per entity*, so history rots), there's no execution-count field, no per-job drill-down, and the dashboard is one flat popup, not a browsable list→list→log view. Its action vocabulary is generic glue (shell/mail/dispatch), not "run this as a headless Claude agent and keep its transcript." Good proof that a compiled-binary Herdr plugin with a tick loop is a sound pattern; not good enough to use as-is. |
| **Herdr's own plugin system** | Plugins (`herdr-plugin.toml`) can ship `[[actions]]` (invokable commands), `[[events]]` (hooks on Herdr events), and `[[panes]]` — and a pane with `placement = "tab"` opens as **a normal Herdr tab**, running any argv command (Bash, Node, Go binary, whatever). Explicitly *not* supported in plugin v1: native non-terminal UI or runtime action registration — so a plugin pane must be a terminal program (a TUI), not a custom widget-based sidebar. | This is exactly the mechanism to use: a TUI shipped as a Herdr plugin pane, opened as a tab. |
| **Claude Code CLI itself** | `claude -p "<prompt>" --output-format stream-json` runs one-shot headless, with `--permission-mode`, `--model`, `--add-dir`, `--max-budget-usd`, session persistence/resume. `claude --bg` / `claude agents --json` gives native background-agent dispatch + listing. | This *is* the "agent" job runner — no need to shell out to the Claude Agent SDK or reimplement anything; `claude -p` headless is the primitive. |

**Conclusion: build it**, as a standalone Go binary that opens in an ordinary Herdr tab — not a formal Herdr plugin (see §8 for why that's a later, optional layer, not the foundation). Nothing on the market is both (a) Herdr-compatible and (b) models "job → runs → log" the way you want. The pieces to reuse are: systemd (scheduling heartbeat), Herdr's ability to run any command in a tab (hosting, no plugin manifest required), `claude -p --output-format stream-json` (agent execution), and Orca's CLI verb naming (UX shape) — assembled new, because no existing project combines them.

## 3. Name

**`jobtail`** — job, plus tailing its log/run: the two things this tool actually does (schedule the job, then watch it). Landed on this after the obvious herding puns turned out to be a crowded namespace — `corral`, `drover`, and `paddock` are all already taken by unrelated tools or (worse) other Herdr plugins doing something else entirely; `jobtail` had no collision in the CLI/agent-tooling space and `jobtail.sh` was free. One Go module, one binary: `jobtail`, with subcommands for everything (`add/list/show/runs/log/enable/disable/edit/rm/run/resume/tick/run-exec/gc/tui`). Single build, single systemd `ExecStart` path, no separate scripts to keep in sync.

## 4. Goals / non-goals

**Goals (v1)**
- Register a job with one CLI command: type (`cli` or `agent`), schedule (cron), and what to run.
- See all jobs in a list: name, type, enabled, run count, next run, last status.
- Drill into a job → list of its runs (start time, duration, trigger: scheduled/manual, status).
- Drill into a run → the log: raw stdout/stderr for `cli` jobs, rendered transcript for `agent` jobs.
- Runs happen even when nobody has Herdr open (systemd timer, not "only while the TUI is open").
- Enable/disable, edit, delete, and manually re-run a job from the CLI (TUI is view + light control, not the only way in).
- A failed run surfaces as a Herdr desktop notification.

**Non-goals (v1)**
- No webhook/file-watcher triggers (that's `herdr-sched`'s territory; can steal the pattern later — see §11 Phase 4).
- No distributed/multi-host execution as a first-class feature. Cut, not deferred: a `cli` job that needs another machine just runs `ssh mars '...'` as its command — that already covers "check the TrueNAS backup status," no `--host` field or `herdr machine` integration needed.
- No retries/backoff. A missed or failed run is just a row with `status=failed`; re-running is a manual/explicit action (`jobtail run`, or `jobtail resume` for agent jobs — see §7, §10).
- No web UI. TUI + CLI only.
- No general workflow DAGs (no job-depends-on-job). One job = one schedule = one command or one agent prompt.
- No config file. All per-job settings live in SQLite via CLI flags; only fixed paths (data dir, log dir) have compiled-in defaults, overridable with `JOBTAIL_DATA_DIR` if ever needed — consistent with "schedule a job with one CLI command," not a YAML file to hand-edit.

## 5. Architecture

No long-running daemon of our own. Two moving parts:

```
                 ┌───────────────────────────┐
 systemd timer   │  jobtail tick             │   every 60s, checks due jobs,
 (OnCalendar=    │  (short-lived process)    │   claims + spawns runs, exits
  *-*-* *:*:00)  └─────────────┬─────────────┘
                                │ spawns (setsid, detached)
                                ▼
                  ┌───────────────────────────┐
                  │  jobtail run-exec <id>    │   does the actual work,
                  │  (one per run)            │   writes to SQLite + log file,
                  └───────────────────────────┘   notifies on failure

 Herdr tab (opened on demand, no plugin manifest required — see §8)
                  ┌───────────────────────────┐
                  │  jobtail tui              │   reads the same SQLite file,
                  │  (Bubble Tea, 3-pane)     │   read-mostly, polls every ~1s
                  └───────────────────────────┘
```

- **Storage**: single SQLite file, `~/.local/share/jobtail/jobtail.db`, opened with `PRAGMA journal_mode=WAL` and `PRAGMA busy_timeout=5000` on every connection — readers (TUI) never block writers and vice versa, and concurrent writers (`tick`, multiple `run-exec`s finishing close together, `gc`) queue and retry instead of erroring. See §12 decision 11 for why this is enough without a dedicated writer process.
- **Logs**: one file per run under `~/.local/share/jobtail/logs/<run-id>.log` (cli jobs: raw combined stdout+stderr) or `<run-id>.jsonl` (agent jobs: the `stream-json` transcript, one JSON event per line — same shape Claude Code itself uses, so it's easy to pretty-render).
- **No server/socket of our own.** The TUI just reads the DB + tails the log file for the currently-open run. This avoids the exact trap `herdr-sched` calls out — "Herdr has no shutdown hook" — since we never start something that has to be stopped; `jobtail tick` and `jobtail run-exec` are ordinary processes that exit on their own, and the TUI is just a pane that closes when its tab closes.
- **The TUI does not need to be a Herdr plugin to live in a Herdr tab.** Herdr already runs arbitrary commands in panes/tabs (open a tab, run `jobtail tui`). A `herdr-plugin.toml` is optional packaging on top — see §8 — not a requirement for "a tab with the dashboard in it."
- **Locking**: `jobtail tick` takes an exclusive `BEGIN IMMEDIATE` SQLite transaction per due job before spawning it, so a slow tick and a manual `jobtail run` never double-fire the same job. Default `max_concurrent=1` per job (configurable per job) — if a run is still active when its next scheduled time arrives, the tick skips it and logs a `skipped_overlap` row.

## 6. Data model

```sql
CREATE TABLE jobs (
  id            TEXT PRIMARY KEY,           -- short slug, e.g. "nightly-deps"
  kind          TEXT NOT NULL,              -- 'cli' | 'agent'
  cron          TEXT NOT NULL,              -- standard 5-field cron, evaluated in local tz
  timezone      TEXT NOT NULL DEFAULT 'local',
  enabled       INTEGER NOT NULL DEFAULT 1,
  cwd           TEXT NOT NULL,
  command       TEXT,                       -- kind='cli': shell command
  prompt        TEXT,                       -- kind='agent': the task prompt
  model         TEXT,                       -- kind='agent': --model (optional)
  provider      TEXT,                       -- kind='agent': 'claude' (default/NULL) | 'opencode' | 'codex'
  permission_mode TEXT,                     -- kind='agent': meaning is provider-specific, see §14
  max_concurrent INTEGER NOT NULL DEFAULT 1,
  timeout_seconds INTEGER,                  -- optional hard kill
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE TABLE runs (
  id            TEXT PRIMARY KEY,           -- uuid
  job_id        TEXT NOT NULL REFERENCES jobs(id),
  trigger       TEXT NOT NULL,              -- 'scheduled' | 'manual' | 'resume'
  status        TEXT NOT NULL,              -- 'running' | 'ok' | 'failed' | 'timeout' | 'skipped_overlap'
  started_at    TEXT NOT NULL,
  finished_at   TEXT,
  exit_code     INTEGER,
  session_id    TEXT,                       -- kind='agent' only: session/thread id, for `resume` (§14)
  log_path      TEXT NOT NULL,
  duration_ms   INTEGER                     -- measured directly around the exec.Cmd run; see note below
);

CREATE INDEX idx_runs_job ON runs(job_id, started_at DESC);
```

`run_count` and `last_status` shown in the job list are `SELECT count(*)` / latest `runs` row — not duplicated columns, to keep them always correct.

`duration_ms` deliberately isn't `finished_at - started_at`: for a scheduled run, `started_at` is the nominal cron slot (§12 decision — needed so catch-up after a gap advances one slot at a time instead of silently skipping backlogged ones), not the moment `run-exec` actually began. If ticks fall behind, that gap alone can make a run that took 30ms read as a minute long. `duration_ms` is measured directly around the actual `exec.Cmd` run instead, so it never conflates scheduling delay with real execution time. Added via an additive migration (`ALTER TABLE runs ADD COLUMN`, guarded by a `PRAGMA table_info` check) rather than bumping the whole schema, since this was found and fixed against a real, already-populated database, not before first use.

Log retention: default keep last 200 run rows + log files per job (`jobtail gc`, also run opportunistically at the end of `jobtail tick`); configurable per job (`--keep N`). This is deliberately *not* the herdr-sched approach (windowed to newest 1000 *events*, which quietly discards history) — `jobtail` keeps full per-run rows up to the retention count, so "how many times has this run, ever" (lifetime counter, separate from retained rows) stays accurate even after old logs are pruned.

## 7. CLI reference

```sh
# create
jobtail add nightly-deps \
  --kind agent \
  --cron "0 3 * * *" \
  --cwd ~/workspace/vault \
  --prompt "Check for outdated dependencies across the repo. If an update is safe, open a PR; otherwise report why not." \
  --model sonnet \
  --permission-mode acceptEdits

jobtail add backup-check \
  --kind cli \
  --cron "*/15 * * * *" \
  --cwd ~/workspace/vault \
  --cmd "scripts/check_backups.sh"

# inspect
jobtail list                       # table: id, kind, enabled, runs, next, last status
jobtail show nightly-deps          # full job detail + last 5 runs
jobtail runs nightly-deps          # full run history for one job
jobtail log <run-id>               # dump one run's log to stdout (pipeable, less-friendly)

# control
jobtail enable|disable nightly-deps
jobtail edit nightly-deps --cron "0 4 * * *"
jobtail rm nightly-deps
jobtail run nightly-deps           # manual trigger, trigger='manual', fresh session
jobtail resume <run-id>            # agent runs only: opens an interactive Herdr tab
                                   # attached to that run's provider (claude --resume /
                                   # opencode --session / codex resume), seeded from
                                   # the run's captured session id — see §14

# plumbing
jobtail tick                       # called by the systemd timer; not for interactive use
jobtail gc                         # prune old runs/logs past retention
jobtail tui                        # the Herdr plugin pane entrypoint
                                    # (bare `jobtail`, no subcommand, does the same thing)
```

All commands support `--json` for scripting, matching the convention Herdr itself uses.

## 8. Herdr integration

**Default (v1): no plugin manifest at all.** `jobtail tui` is a plain terminal program. Open a tab in Herdr, run `jobtail tui`, done — that's the whole integration. Nothing to install, nothing to keep in sync with Herdr's plugin-loader version compatibility, nothing that needs `herdr plugin link`/`install` to work. This is a personal single-host tool; a bare binary you can also just run over plain SSH/tmux if Herdr is ever down is strictly more robust than depending on the plugin system for the core feature to exist.

The one thing a bare command can't give you is a keybinding that jumps straight to the dashboard from anywhere without first creating/finding a tab. That's the *only* reason to add the manifest, and it's purely additive — same binary, no redesign:

```toml
id = "dani.jobtail"
name = "Jobtail"
version = "0.1.0"
min_herdr_version = "0.9.0"
description = "Scheduled CLI + agent job dashboard"
platforms = ["linux"]

[[panes]]
id = "dashboard"
title = "Jobs"
placement = "tab"
command = ["jobtail", "tui"]

[[actions]]
id = "run-now"
title = "Jobtail: run job now"
contexts = ["workspace"]
command = ["jobtail", "run", "--picker"]
```

If added: for local dev, `herdr plugin link ~/workspace/jobtail`; a keybinding opens the dashboard tab (`[[keys.command]]` in `config.toml`, same pattern `herdr-sched` uses), e.g. `prefix+shift+j`. Treat this as a Phase 4 nice-to-have (§11), decided after living with the plain-tab version for a while, not an up-front commitment.

- On any run finishing with `status='failed'` or `'timeout'`, `jobtail run-exec` shells out to `herdr notification show "jobtail: <job-id> failed" --sound request` — this is a plain CLI call at the end of the run and needs no plugin registration either way.
- Deliberately *not* using Herdr's `[[events]]` hooks or the socket API for v1 — nothing about scheduling needs to react to Herdr events, and the socket API has no auth beyond filesystem permissions, so keeping jobtail's own data path independent of it is simpler and doesn't add a dependency on the server being up.

## 9. TUI spec

### Framework choice: Bubble Tea, not OpenTUI

OpenCode itself moved off Go/Bubble Tea to **OpenTUI** — a Zig-compiled rendering core with TS bindings (`@opentui/core`, `@opentui/react`, `@opentui/solid`), flexbox layout, MIT-licensed, standalone on npm. It's a legitimately strong platform, and worth naming as the alternative:

| | Bubble Tea (Go) | OpenTUI (Zig core + TS) |
|---|---|---|
| Ships the widgets jobtail needs | Yes — `bubbles` has table/viewport/list/textinput off the shelf | Documented components are selects/inputs/scroll boxes; a sortable job table or log viewer is more assembly, less off-the-shelf |
| Deployment | `go build` → one static binary, no runtime dependency | Needs Bun present at run time (or a `bun build --compile` packaging step) |
| Fit for this UI | Elm architecture (model/update/view) matches a static 3-pane drilldown well | Built for high-frequency streaming/chat-like UIs (its actual production use case in OpenCode); more power than a job dashboard needs |
| Ecosystem maturity as a general toolkit | Mature, huge existing user base beyond its origin project | Young outside OpenCode itself, though OpenCode's scale is real proof it's solid |

Decision: **Bubble Tea**, because jobtail's UI is closer to `lazygit`/`k9s` (static-ish structured panes, not a live-rendered chat stream) and a single dependency-free binary matters for something a systemd timer and a Herdr tab both need to launch reliably. OpenTUI would be the right call if this were being built in TS/Bun already or needed OpenTUI's rendering headroom — revisit if either becomes true.

Jobs and runs side by side on top, log spanning the full width underneath — not three columns side by side — on a terminal wide enough for it; see §15 for the stacked and single-pane layouts smaller terminals get. Chosen over an even three-way split after actually using it: the log is where the real content lives (command output, agent transcripts), so it gets the width and gets it below the (usually short) job/run lists rather than squeezed into a third column. Herdr tab (opened per §8 — either a plain `jobtail tui` in a manually-created tab, or via the optional plugin's keybinding), Bubble Tea (`charmbracelet/bubbletea` + `bubbles` table/viewport + `lipgloss`), pattern deliberately close to `lazygit`/`k9s` since that's already muscle memory here.

```
┌─ Jobs ────────────────────────────────┬─ Runs: nightly-deps ──────────────────┐
│ ● nightly-deps    agent  47  ...  ✓   │ ✓ 2026-09-26 03:00   1m12s            │
│ ○ backup-check    cli   912  ...  ✓   │ ✓ 2026-09-25 03:00   0m54s            │
│ ● cert-renew      cli     6  ...  ✗   │ ✗ 2026-09-24 03:00   0m08s            │
└────────────────────────────────────────┴────────────────────────────────────────┘
┌─ Log: run 8f2a ──────────────────────────────────────────────────────────────────┐
│ session abc123 started                                                           │
│ > Bash({"command":"..."})                                                        │
│ ...                                                                              │
└────────────────────────────────────────────────────────────────────────────────┘
  h/l or arrows/enter/esc: move · e enable/disable · r run now · q quit
```

- **Jobs pane** (top-left): `●`/`○` = enabled/disabled, type badge, run count. Status color = last status (green ok / red failed / yellow running / grey never-run) — applied to the status cell, within the width constraint documented in §15.
- **Runs pane** (top-right): for the selected job, newest first; status glyph, start time, duration, trigger badge (S/M for scheduled/manual).
- **Log pane** (bottom, full width): for the selected run. `cli` jobs render the raw log as a scrolling text viewport. `agent` jobs parse the `stream-json` lines and render them the way a transcript reads — assistant text, tool calls with their input, tool results collapsed by default (expandable), final result/cost line — not raw JSON by default (raw available via a `J` toggle for debugging).
- Live tail: if the selected run's `status='running'`, the log pane tails the file (`fsnotify`/poll) instead of a static read.
- `e`/`r` act on the job/run under the cursor immediately via the same code path as the CLI (no separate "TUI-only" logic to keep in sync).
- Refresh: poll SQLite every ~1s for list panes; this is a personal single-writer box, no need for push/subscribe.
- Mouse: click a job/run row to select it (and switch focus to that pane); click anywhere in a pane to focus it; wheel scrolls whichever pane the cursor is over (job/run cursor moves a row per notch, the log viewport scrolls a line per notch). Keyboard remains the primary/complete interface — mouse is additive, not required.

## 10. Execution engine

**`cli` jobs**: `exec.Command("sh", "-c", job.command)` with `Dir: job.cwd`, stdout+stderr both piped to the run's log file, `context.WithTimeout` if `timeout_seconds` set. Exit code stored verbatim; non-zero → `status='failed'`.

**`agent` jobs**: build and run:

```sh
claude -p "<prompt>" \
  --output-format stream-json \
  --add-dir <cwd> \
  --permission-mode <permission_mode> \
  --model <model> \
  --no-session-persistence
```

from `job.cwd`, streaming stdout line-by-line straight into the run's `.jsonl` log. The first `system`/`init` event in the stream carries the session id — captured into `runs.session_id` as soon as it arrives, so a run that later fails is still resumable via `jobtail resume`. Exit code + the final `result` event's `is_error` field together decide `ok` vs `failed`. `--permission-mode acceptEdits` is the sane default for unattended runs (auto-accepts file edits, still not `bypassPermissions`); `--dangerously-skip-permissions`/`bypassPermissions` is deliberately never the default and only settable explicitly per job for cases that need it (e.g. a fully sandboxed maintenance job). `jobtail resume <run-id>` itself doesn't run headless — it does `herdr tab create` + `pane run <pane> "claude --resume <session_id>"`, handing the failed session to you interactively rather than trying to make unattended retries smart.

Both kinds: `SIGTERM` then `SIGKILL` after a grace period on timeout; run row gets `status='timeout'`. Log capture is capped at 10MB per run (a truncation marker line is appended and the process is left running — capping the *captured* log, not killing a noisy-but-otherwise-fine job) so one runaway `cli` job can't fill the disk.

Cron parsing uses `github.com/robfig/cron/v3`'s parser package only (just schedule math — `Next(t)` — not its scheduler/goroutine, since `jobtail tick` drives its own timing via systemd). Schedules are evaluated in the system's local timezone by default; `jobs.timezone` can override per job with an IANA name (e.g. `UTC`, `Europe/Madrid`) for the rare job that needs to ignore DST shifts.

## 11. Build plan

- **Phase 1 (MVP, this repo)**: schema + CLI (`add/list/show/runs/log/enable/disable/edit/rm/run/tick/gc`), systemd user unit + timer, `cli`-kind execution, SQLite store, log files. No TUI yet — `jobtail log <run-id> | less` is enough to validate the execution engine end to end.
- **Phase 2**: `agent`-kind execution against `claude -p`, transcript-aware log rendering logic (can be a shared Go package used by both `jobtail log --pretty` and the TUI later).
- **Phase 3**: the Bubble Tea TUI (`jobtail tui`), run as a plain command in a Herdr tab — no manifest. `herdr notification show` on failure is added here too (it's just a CLI call, not plugin plumbing).
- **Phase 4 (optional, only if wanted later)**: `herdr-plugin.toml` + keybinding (§8) once the plain-tab workflow has been lived with; webhook/file-watcher triggers (steal the pattern from `herdr-sched` rather than depending on it); `--host` field to run a job on a saved `herdr machine` over SSH.

## 12. Decisions

Everything that was open is now decided. Listed so each can be challenged individually rather than re-litigating the whole PRD.

| # | Decision | Rationale |
|---|---|---|
| 1 | **Bare command, not a Herdr plugin, for v1.** `jobtail tui` runs in a manually-opened Herdr tab. `herdr-plugin.toml` is Phase 4, purely for a keybinding, added only if actually missed. | The plugin manifest buys a shortcut, not the feature. Fewer moving parts, no dependency on Herdr's plugin loader for the tool to exist at all — it works the same over plain SSH if Herdr is down. |
| 2 | **Bubble Tea (Go), not OpenTUI (Zig/TS).** | `bubbles` ships the table/viewport/list widgets jobtail needs directly; compiles to one dependency-free binary, which matters for something both a systemd timer and a Herdr tab must launch unattended. OpenTUI is the better tool for a high-frequency streaming UI, which this isn't. |
| 3 | **Single Go binary**, all subcommands (`add … tick, run-exec, gc, tui`) in one module. | One build, one systemd `ExecStart`, nothing to keep in version-sync with itself. |
| 4 | **No config file.** Per-job settings live in SQLite via CLI flags only; fixed paths get compiled-in defaults, override via `JOBTAIL_DATA_DIR` env var if ever needed. | Matches "schedule with one CLI command" — a YAML file to hand-edit is exactly the friction being avoided. |
| 5 | **Cron evaluated in local timezone by default**, `jobs.timezone` overridable per job (IANA name). | This is a personal server in one place; local time is the natural mental model. `herdr-sched`'s UTC-only stance is right for a shared/multi-timezone fleet, not this. |
| 6 | **`runs.session_id` captured for every agent run**, plus `jobtail resume <run-id>` (hands a failed run to an interactive `claude --resume` in a new Herdr tab). | One column, added now instead of as a later migration. Unattended retries aren't smart enough to trust; handing a failed run to you interactively is the right level of automation. |
| 7 | **Multi-host is cut, not deferred.** No `--host` field, no `herdr machine` integration. A `cli` job that needs another machine just runs `ssh mars '...'` as its command. | Covers the one plausible case (checking TrueNAS/`mars`) with zero new mechanism. Add real multi-host support only if a concrete job actually needs jobtail itself (not just its command) to run elsewhere. |
| 8 | **Tick resolution fixed at 1 minute**, no configurable tick interval. Systemd user timer, `OnCalendar=minutely`, `Type=oneshot` service, enabled with `systemctl --user enable --now jobtail-tick.timer`. | Matches cron's native granularity exactly; a configurable tick (like `herdr-sched`'s `tick_seconds`) is a knob with no real use here. User-level systemd (not system-level) because jobtail only ever needs to act as your own user. |
| 9 | **Per-run captured log capped at 10MB**, truncation marker appended, job keeps running. | Caps disk risk from a runaway `cli` job without being a job killer — it's a log cap, not a job timeout (that's the separate `timeout_seconds` field). |
| 10 | **Cron math via `robfig/cron/v3`'s parser only** (not its scheduler). | `jobtail tick`'s own timing comes from systemd; only `Next(t)` schedule math is needed from the library. |
| 11 | **Concurrent writers handled by WAL + `PRAGMA busy_timeout=5000` on every connection, not by adding a writer daemon.** Every process (`tick`, `run-exec`, `gc`, `tui`) opens the DB directly; writes stay single-statement (claim at start, one `UPDATE` at finish — never a transaction held open for a run's duration). DB file must stay on local disk, not the TrueNAS/NFS mount. | SQLite serializes writers even in WAL mode, but `busy_timeout` makes that a silent retry, not an error — and at jobtail's job volume (tens of jobs, writes only at run-start/run-finish) the lock queue drains in sub-millisecond time, nowhere near the timeout. A dedicated single-writer daemon would solve the same problem but reintroduces the "long-running process with no shutdown hook" issue §5 deliberately avoided, for a contention level that doesn't exist at this scale. |
| 12 | **Public repo, MIT license.** No secrets in the code; standard `curl \| sh` installers need unauthenticated asset downloads, which a private repo blocks. | Distribution (below) depends on it — GitHub's `/releases/latest/download/<asset>` shorthand and the API's `/releases/latest` only work without a token on a public repo. |

## 13. Distribution & upgrades

- **Install**: `curl -fsSL https://raw.githubusercontent.com/dalogax/jobtail/main/install.sh | sh` — detects OS/arch, downloads the matching release binary from `github.com/dalogax/jobtail/releases/latest/download/jobtail-<os>-<arch>` (GitHub's own no-auth shorthand for "latest release's asset"), installs to `~/.local/bin/jobtail` (`INSTALL_DIR` overridable).
- **Releases**: `.github/workflows/release.yml` triggers on `v*` tag push — runs the full test suite as a gate, cross-compiles `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` (`CGO_ENABLED=0`; `modernc.org/sqlite` is pure Go, so there's no cross-C-toolchain problem to solve), and publishes them as a GitHub Release with `-ldflags "-X main.version=<tag>"` baked in.
- **Version**: `jobtail --version`. A plain `go build` (no ldflags) leaves it `"dev"` — treated everywhere as "nothing to compare against," never triggering an upgrade suggestion, and always safe to overwrite with `jobtail upgrade`.
- **`jobtail upgrade`**: checks `GET /repos/dalogax/jobtail/releases/latest`, compares tags with `golang.org/x/mod/semver`, and — unless already current — downloads the asset for the running `GOOS`/`GOARCH` and atomically replaces its own executable (write to a temp file in the same directory, then `rename` over the original; safe on Linux even for the binary currently executing). `--check` reports without installing; `--force` reinstalls even if already latest.
- **Background suggestion**: every command except `tick`, `run-exec`, `tui`, `upgrade`, and `completion` does a best-effort post-run check and prints `jobtail: a newer jobtail is available: vX -> vY (run \`jobtail upgrade\`)` to **stderr** (never stdout, so `--json` output stays parseable) if one is due. The check itself is cached on disk for 24h so it costs one HTTP request a day, not one per invocation, and a network failure or private/unreachable API is silently swallowed — this can never block or fail an ordinary command.

## 14. Multi-provider agent jobs

`--kind agent` jobs originally only meant "a headless `claude -p` turn." `--provider` (empty/omitted = `claude`, or `opencode`/`codex`) picks a different agent CLI for the same job shape (prompt, cwd, model) — one `execengine.RunAgent` dispatches to a `runClaudeAgent`/`runOpenCodeAgent`/`runCodexAgent` per provider, each with its own arg-building, JSON-stream parsing, and transcript renderer (`internal/tui/transcript.go`'s `renderTranscript`/`renderOpenCodeTranscript`/`renderCodexTranscript`).

Every provider's invocation and JSON event shape below was checked directly against the real CLI on this box before being coded, not guessed from docs — with one caveat noted for codex.

| Provider | Invocation | Session/thread id | Failure signal |
|---|---|---|---|
| `claude` | `claude -p <prompt> --output-format stream-json --verbose --add-dir <cwd> --no-session-persistence --permission-mode <acceptEdits\|...> [--model]` | `session_id` on the `type:"system",subtype:"init"` event | `is_error:true` on the `type:"result"` event, or no result event at all |
| `opencode` | `opencode run <prompt> --format json [-m <model>]` | top-level camelCase `sessionID`, present on every event from the first line (no distinct init event) | a top-level `type:"error"` event — **process exit code is 0 even then**, confirmed directly by running an invalid-model request; exit-code alone is not trustworthy for this provider |
| `codex` | `codex exec --json --sandbox <workspace-write\|read-only\|danger-full-access> --skip-git-repo-check [-m <model>] <prompt>` | `thread_id` on the `type:"thread.started"` event | `type:"turn.failed"` or top-level `type:"error"`; confirmed the process reliably exits non-zero on failure (unlike opencode) |

`opencode` vs. "opencode 2": these are the same CLI/JSON protocol (`opencode run --format json`), not two things to support separately — opencode 2 is a newer major version with a reworked *server* API (relevant to programmatic HTTP/SDK use), but its headless `run` command and JSONL event shape are unchanged from v1 as of the version installed here (checked against `opencode run --help` and opencode's own GitHub issues referencing `--format json` on both the 1.18.x and `dev`/v2 branches). One `opencode` provider, shelling out to whatever `opencode` is on `PATH`, covers both — exactly like the `claude` provider already does for whatever Claude Code version is installed.

**codex's success path is unverified on this box.** This box has no stored codex credentials (`codex login status` → "Not logged in"), so only codex's *error* path — a real 401 against `api.openai.com`, producing `thread.started`/`turn.started`/`error`/`turn.failed` — has been observed against the real binary. The `item.completed` shape used for a successful turn's assistant text is inferred from codex's documented event model. `TestRealCodexAcceptsOurAgentInvocation` in the e2e suite exists specifically so whoever next has working codex credentials can flip it on (`JOBTAIL_REAL_CODEX_TEST=1`) and get a real answer, the same opt-in-real-binary pattern already used for `TestRealClaudeAcceptsOurAgentInvocation`.

`permission_mode` is reused rather than adding a second provider-specific column: for `claude` it's `acceptEdits`(default)/`bypassPermissions`/`plan`; for `codex` it *is* the `--sandbox` policy (default `workspace-write`, the closest codex equivalent of claude's default — never `danger-full-access` unless a job opts in, mirroring decision 6's "never bypassPermissions unless explicit"); `opencode` doesn't consume it at all (its own `--auto` flag was deliberately left unused — a real run with no such flag still executed a `bash` tool call with no permission prompt or hang, confirmed directly, so the unattended-hang risk that motivates claude's acceptEdits default doesn't appear to apply to opencode's `run` command).

`resume`'s interactive command is provider-aware too (`cmd_run.go`'s `resumeCommand`), each verified against that binary's own `--help`: `claude --resume <id>`, `opencode --session <id>` (opens the interactive TUI on that session — its `run --session` counterpart is non-interactive), `codex resume <id>` (the top-level, interactive `resume` — distinct from the non-interactive `codex exec resume`).

**A real, incidental bug found while verifying this**: on this mise-managed box, the resolved `claude`/`opencode`/`codex` binaries are mise shims that print `mise ~/.config/mise/config.toml tools: <tool>@<version>` to stderr before delegating — which was landing verbatim in captured logs and leaking into rendered transcripts (exactly the raw-noise §9 exists to prevent). Fixed by setting `MISE_QUIET=1` on every command execengine spawns (`childEnv()`), for all job kinds, not just agent jobs — scoped to jobtail's own child processes only, never the user's interactive shell or mise generally.

## 15. Responsive layout

The TUI was built and tuned at one terminal size (a wide one) and quietly fell apart below it. A pass rendering the real dashboard through a pty at a dozen real sizes, against the real database and against 21-job/long-id/empty fixtures, found that it stopped conveying anything well before "narrow":

- At **80×24** — an ordinary terminal, not an edge case — the jobs pane read `● ha-fr…  c…  …  09…  ok`. Every column got a proportional share of a width that couldn't support any of them, so even the *headers* were truncated (`Ki…`, and a `Runs` column collapsed to a bare `…`, occupying space while conveying nothing). At 45 columns and below the panes were ellipses end to end.
- The **help bar was cropped by Bubble Tea**, silently: at 80 columns `q quit` was gone entirely, so nothing on screen said how to exit; at 60 it cut mid-word (`e enable/dis`).
- The top row took a **flat 40% of the height whatever it held** — showing 6 of 21 jobs, with no indication the other 15 existed, above a log pane that was empty.
- An **empty database** rendered as three blank boxes: the first thing a new user sees, with no hint of what to do next.
- Pane **titles vanished** rather than shortening when the border couldn't hold them, so at 50 columns the runs pane had a blank top rule and nothing named the job whose runs were listed.
- `statusStyle` and the four status colors had been written but **never called** — the dashboard's single most important signal, *did it fail*, rendered in the same plain text as everything else.
- `rowAtY` still assumed the pane title occupied a content row after the btop restyle moved it into the border, so **every mouse click resolved one row too high** and the first row of each table could not be clicked at all.

### Design

**Three layouts, chosen by terminal size** (`modeFor`). The dashboard's interaction model is already a drill-down — jobs → runs → log, `enter`/`l` forward and `esc`/`h` back — so the narrow layout doesn't invent a second mental model, it just stops drawing the levels you aren't looking at.

| Mode | When | Arrangement |
|---|---|---|
| `layoutWide` | ≥ 100 cols and ≥ 18 rows | jobs \| runs side by side, log full width below |
| `layoutStacked` | ≥ 56 cols and ≥ 20 rows | all three full width, stacked |
| `layoutFocused` | anything smaller | the focused pane only, filling the screen |

The 100-column breakpoint is derived, not round: the jobs table's columns need ~38 columns of content to stop being stumps, plus 2 padding per column and 4 of box chrome — about 52 per pane, so two honest panes side by side need ~104.

**Columns are dropped whole, not squeezed** (`fitColumns`). Each `colSpec` carries a `min` (the narrowest width at which it still reads as itself — a timestamp needs 11 for `09-27 13:30`; less is a lie), a `max` (past which it just pushes its neighbors around; `0` = unbounded, marking the column that absorbs the leftover), a `grow` weight, and a `drop` priority. When the width isn't there the worst-priority column leaves rather than starving the rest. Job ID and run status are the two things the panes exist to answer and are the last to give ground; run counts and cron expressions are the first to go. Three columns you can read beat five you can't.

**Spare width buys information, not padding.** The same mechanism runs in reverse: a wide jobs table gains a `Cron` column, a wide runs table an `Exit` one. A column that hits its `max` returns its surplus to the pot, so fixed-width content (a timestamp, a kind) never inflates into dead space while the ID column is still truncating. In the wide layout the runs pane is sized to what its columns can actually use and the jobs pane takes everything else — an even split spent half the terminal padding timestamps while truncating the names beside them.

**Panes are sized to their contents** (`topRowHeight`, `stackedHeights`), capped so none starves the others, with the focused pane getting first claim on the remainder. The jobs list drives the top row's height rather than the runs list, because run history is unbounded — letting it decide would peg the row at its cap permanently, which is the old fixed split by another name.

**Status color, within a measured constraint.** `bubbles/table` v1.0.0 truncates cells with `go-runewidth`, which is not ANSI-aware: it measures `"\x1b[31mfailed\x1b[0m"` as 13 columns rather than the 6 a terminal shows. Measured against the real widget rather than assumed — below that 13 it cuts mid-escape, mangling the text (`failed` → `f…`) *and* swallowing the reset so the color bleeds across the row; at ≥ 13 the string passes through untouched and the row's visible width still comes out exactly right. So `colorCell` colors a status only when the column can pay for the escape bytes too, and falls back to plain text otherwise. Narrow terminals lose the color, never the text. This is why a status column's `max` is `statusColorWidth` (15 + 9) rather than the width of its longest word.

**Everything else that was cropping now fits itself**: the help bar shortens its labels then drops hints worst-first (quitting never drops) and is tailored to the focused pane, since `e` and `r` only ever acted on the jobs table and advertising them while reading a log was a promise the UI didn't keep; pane titles shorten instead of disappearing; list panes carry a `3/21` position so hidden rows announce themselves, and the log pane a scroll percentage; an empty database names the command that fixes it, choosing a shorter phrasing rather than being clipped when the box is small.

Regression coverage lives in `internal/tui/responsive_test.go` and asserts on the **rendered frame** across a dozen sizes — no pane may render as mostly ellipses, no rendered line may exceed the terminal width, the help bar must always say how to quit, the job id must survive, every title must appear. The previous tests all passed throughout the broken period because they checked box arithmetic and absence of crashes; nothing looked at what was actually on screen.

## 16. Performance and footprint

Benchmarked before optimizing, then again after; both sets of numbers came from the real binary, a real SQLite file and — for the dashboard — a real pty, on an i5-1145G7.

### What the tool actually costs

The CLI is not where jobtail spends its life. Every subcommand starts, does one thing and exits in about 3 ms:

| Invocation | Wall | Peak RSS |
|---|---|---|
| `jobtail --version` (no database) | 2.1 ms | 13.8 MiB |
| `jobtail tick` (the once-a-minute timer) | 3.3 ms | 17.7 MiB |
| `jobtail list --json` | 3.3 ms | 18.5 MiB |

`store.Open` accounts for 0.23 ms of that, most of the rest being Go runtime start and cobra building its command tree. Nothing here was worth touching, and `tick` in particular is already lean: one `EnabledJobs` query, one indexed `LastRun` per job, and a subprocess only for jobs that are actually due.

The dashboard is the opposite: a process people leave open in a tab for hours, whose cost is paid whether or not anyone is looking. That is where all of it was.

### The dashboard was burning CPU to produce identical frames

An open, untouched dashboard cost **31.5 ms of CPU per second** against a 12-job store with a 256 KB agent transcript selected — and **105 ms/s**, a tenth of a core, when that transcript was 2 MB. Over the same interval it wrote **0 bytes to the terminal**, because every frame it computed was byte-identical to the one already on screen and Bubble Tea's renderer discarded it.

The refresh ran the whole pipeline once a second regardless of whether anything had changed: two queries, a full file read, a JSON parse of the entire transcript, a re-wrap, a row rebuild, a re-layout, four frame renders. Yet jobs only change when someone edits one, runs only when one starts or finishes, and a finished run's log never changes at all.

Worse, the guard that was supposed to limit the log work didn't. The tick only asked for a log reload when the selected run was still running — but the *runs* reload, which happened every tick, unconditionally asked for one too, so the expensive path ran every second for every run, finished or not.

### Refreshes that change nothing now cost nothing

The lever is that Bubble Tea's event loop drops a command whose `Msg` is nil before anything happens (`tea.go`: `if msg == nil { continue }`) — no `Update`, no row rebuild, no re-layout, and no frame render. So each reload command is handed what its pane is already showing and answers "nothing new" with nil:

- **Jobs and runs** compare the query result against the loaded slice with `slices.Equal`. `store.JobSummary` and `store.Run` are strictly comparable, and every field comes from the same parse path, so equal content is exactly equal structs. Comparing the whole struct rather than the displayed fields keeps behaviour identical: `selectedJob` hands `Cwd`/`Command`/`Prompt` to "run now", so a job edited from another terminal must still take effect.
- **The log** is decided by a `stat` alone, against the run id, size and mtime of the bytes on display. Run logs are append-only, so those settle it, and an unchanged file is never read, never re-parsed and never re-wrapped.
- **`layout()`** compares a `layoutKey` of everything it reads (size, focus, row counts) and returns immediately when the screen hasn't changed shape. Reload messages arrive far more often than the geometry moves, and each blind re-layout pushed fresh columns and widths into both tables — an `UpdateViewport` apiece — plus a full row rebuild, to arrive at the same numbers.

Two more fixes, both of them removing redundant work rather than trading anything away:

- **`wrapForViewport` no longer pads.** It called `lipgloss.Style.Width().Render()`, whose wrapping is exactly `cellbuf.Wrap` — but whose remaining work padded *every* line out to the full pane width, 4.3 MB of allocation per call on a 256 KB transcript. `viewport.View` already pads what it shows to its own width, so all of that padding was producing lines nobody would ever see. Calling `cellbuf.Wrap` directly keeps the wrapping and drops the rest.
- **`cronx.loadLocation` memoizes `time.LoadLocation`,** which caches nothing but `UTC` and `Local` and re-reads the zoneinfo file on every call. A job on an IANA timezone cost 5.3 µs per `Next` against 0.93 µs for a local one, all of the difference being that read — and `Next` is called for every enabled job every tick, and for every job each time the jobs table is rebuilt.

Because the log is only touched when it changes, the dashboard's cost also stopped scaling with log size:

| Dashboard, idle | Before | After | |
|---|---|---|---|
| 172×40, 200 runs, 256 KB log | 31.5 ms/s | **6.2 ms/s** | 5.1× |
| 172×40, 60 runs, **2 MB log** | 105.2 ms/s | **5.5 ms/s** | **19×** |
| 80×24, 200 runs, 256 KB log | 26.7 ms/s | **6.0 ms/s** | 4.5× |
| Peak RSS, 2 MB log | 43.4 MiB | **33.1 MiB** | −24% |

At the benchmark level, one refresh tick went from 17.1 ms / 8.6 MB / 49,262 allocs to 2.5 ms / 0.6 MB / 6,145 allocs, and `layout()` from 1.35 ms / 10,546 allocs to 242 ns / 0 allocs.

### Renderer framerate: 60 → 30 FPS

An *empty* database still cost 8 ms/s, all of it Bubble Tea's renderer waking 60 times a second to check whether there was a new frame to write. At 30 FPS that is 4.8 ms/s, at 15 FPS 3.1 ms/s.

30 is the deliberate stopping point. This is the one knob here that trades against feel rather than removing waste — the renderer is also what puts a keystroke on screen, so the framerate *is* the worst-case input latency. 33 ms is imperceptible and stays smooth under held-key scrolling; 15 FPS (67 ms) starts to be noticeable. Nothing the dashboard displays changes faster than the 1 Hz refresh, so no content update can tell 30 from 60.

### Binary size: 14 MiB, and why it stays there

Measured by building each dependency layer on its own:

| Layer | Cumulative | Marginal |
|---|---|---|
| Go runtime floor (`func main` printing one line) | 1.16 MiB | — |
| + Bubble Tea, bubbles, lipgloss, cobra | 3.00 MiB | 1.84 MiB |
| + `modernc.org/sqlite` | 6.70 MiB | **3.70 MiB** |
| + `net/http` over TLS | 10.25 MiB | **3.55 MiB** |
| jobtail (its own code, `encoding/json`, the rest) | 13.95 MiB | 3.70 MiB |

The two biggest items are both features, not accidents. `modernc.org/sqlite` is a SQLite transliterated into Go, and it is what makes the cgo-free static binary cross-compile to four platforms from one runner with no toolchain (PRD §12) — the cgo alternative is smaller and gives all of that up. The HTTPS stack is `jobtail upgrade` and the update-available notice. Neither is worth trading for megabytes, so the only size change here is `-trimpath` in the release build: about 53 KB, and the same source now produces the same bytes.

For reference, the 32 MiB `crypto/internal/fips140/drbg.memory` symbol that dominates a naive `go tool nm` listing is BSS — address space, not file, and not resident until touched.

### One correctness bug the benchmarking found

`bubbles/table`'s `Update` returns immediately unless the table is focused. Only the jobs table ever was — the runs table was built with `WithFocused(false)` and nothing called `Focus` when the pane changed — so **↑/↓ in the runs pane did nothing at all**, while the help bar advertised "↑↓ move". The mouse hid it completely, because the wheel and click paths call `MoveUp`/`MoveDown`/`SetCursor` directly and never go through `table.Update`. `setFocus` now keeps the two in step; focus doesn't affect how a selected row is drawn, so no frame changed.

### A rewrite that was measured and rejected

`ListJobs` runs three correlated subqueries per job, which looks like the textbook case for a single grouped `LEFT JOIN` over runs. That rewrite was written, verified to return identical results, benchmarked, and thrown away: it is **twice as slow** (1.73 ms vs 0.86 ms at 50 jobs × 200 runs). `idx_runs_job(job_id, started_at DESC)` turns each subquery into an index seek or a covered range scan, while `GROUP BY` must scan every run row and materialize a temporary b-tree to join back.

### Keeping it honest

- `internal/tui/bench_test.go` — `BenchmarkRefreshTick` is the headline: one second of an idle dashboard, end to end, with the pieces (`View`, `layout`, transcript render, wrap, row build) broken out so a regression can be attributed rather than merely noticed.
- `internal/store/bench_test.go` — the two per-refresh queries and the per-invocation `Open`, across dataset shapes from a fresh install to one at its retention ceiling.
- `internal/cronx/cronx_bench_test.go` — keeps the local/IANA gap closed.
- `internal/tui/refresh_test.go` — the safety net for all of the above. A refresh that skips work must never skip a *change*: a run appearing, a run finishing (which moves no row counts), a job toggled from another process, a log being appended to, a resize needing a re-wrap, two runs whose logs are the same size, a failing query still surfacing its error. One test asserts the skipping happens at all, so the others can't be satisfied by simply reloading everything again.
- `scripts/bench.sh` — binary size, per-invocation wall time and peak RSS, and the dashboard's steady state via `scripts/bench_tui.py`, which drives the real binary in a real pty. CPU there comes from `wait4` rusage in microseconds, with startup cancelled by differencing two run lengths; `/proc/<pid>/stat` counts in 10 ms ticks (too coarse) and `/proc/<pid>/schedstat` reads near-zero unless `sched_schedstats` is on.

## 17. macOS support

jobtail advertised macOS from the start — `install.sh` resolves `darwin/amd64` and `darwin/arm64`, and the release workflow has always built both — but nothing on a Mac ever actually ran on a schedule. This section records what was and wasn't wrong, because most of the plausible suspects turned out to be fine.

### What was already fine

- **The binary.** Every package cross-compiles and vets clean for `darwin/amd64` and `darwin/arm64`. The only platform-specific code is `syscall.SIGTERM`/`SIGKILL` (present on Darwin) and `runtime.GOOS` in the self-updater, which is already doing the right thing. No `/proc`, no cgroups, no Linux-only syscalls.
- **Code signing.** Apple Silicon refuses to run an unsigned binary at all — it is `SIGKILL`ed on exec, which presents exactly as "it doesn't work on macOS". Checked directly against the published `v0.1.16` asset rather than assumed: the Mach-O carries an `LC_CODE_SIGNATURE` load command with a valid `CS_SuperBlob` (`0xfade0cc0`). Go's linker ad-hoc signs `darwin/arm64` even when cross-compiling from Linux, so the existing release pipeline is already correct here.
- **`jobtail upgrade`.** `selfupdate.Install` downloads to a temp file in the target directory and `os.Rename`s over the running binary, which works the same on macOS as on Linux. Downloads made by `curl` carry no `com.apple.quarantine` attribute, so Gatekeeper never enters the picture.

### What was actually wrong

**There was no scheduler.** `install-systemd` wrote `~/.config/systemd/user/*` and shelled out to `systemctl`, which does not exist on macOS. So a Mac user could add jobs, see them in the dashboard, and run them by hand — and nothing would ever fire on its own. That is the whole of "jobtail doesn't work on macOS".

The command is now `install-scheduler` (with `install-systemd` kept as an alias, since it is what every README up to now said), and it installs whatever the platform schedules with. Backends are described as data in `cmd/jobtail/scheduler.go` rather than selected by build tag, so the macOS backend can be built and asserted on from a Linux machine — which matters when the maintainer's machine and CI are both Linux.

### The two launchd details that decide whether jobs run

Neither is discoverable from a failing run; both produce silence rather than an error.

- **`AbandonProcessGroup`.** `tick` spawns each due job's `run-exec` as a detached grandchild and exits immediately. launchd's default is to `SIGKILL` everything left in the job's process group the moment the job exits, so without this key every run dies before it does anything. This is precisely the same failure `KillMode=process` exists to prevent in the systemd unit, where it was already hit for real on a live box — the same bug, a second time, in a different vocabulary.
- **`EnvironmentVariables` → `PATH`.** A LaunchAgent starts with `PATH=/usr/bin:/bin:/usr/sbin:/sbin`. Homebrew (`/opt/homebrew/bin`, `/usr/local/bin`) and `~/.local/bin` are not on it, so `claude`, `opencode`, `codex` and most of what a `cli` job shells out to would not resolve — every agent job failing with "executable file not found". The agent therefore records the `PATH` in effect when `install-scheduler` ran, unioned with the usual Homebrew locations. The cost is that it is a snapshot: installing a tool somewhere new means re-running the command, which the plist says in its own comments.

Systemd deliberately does *not* get the same `PATH` treatment — a `systemd --user` service inherits the user manager's environment, which on a normal desktop session is the one the user expects, and pinning it to whatever shell ran the install would be a regression.

`JOBTAIL_DATA_DIR` is passed to both backends when it is set, which fixes a latent bug on Linux too: a scheduled tick inherits nothing from the shell that installed it, so a custom data directory meant the dashboard and the timer were reading different databases — jobs sitting there listed and never firing, with nothing to say why.

Two smaller things the platform forces: `install-scheduler` now creates the data directory before loading the agent, because launchd refuses to start a job whose `StandardOutPath` it cannot open; and the agent redirects `tick`'s output to `<data dir>/scheduler.log`, since launchd gives an agent no journal and otherwise there is no way to answer "did the scheduler run?". `tick` only prints when it actually fires something, so the file stays small.

### Testing something you can't run

The backend targets a platform that isn't available here, so the tests compensate by being structural rather than textual. `cmd/jobtail/scheduler_test.go` parses the generated plist into the same key/value tree launchd will read and asserts on *that* — a substring check would happily pass on a plist with mismatched keys and values, or one whose `Label` no longer matches its filename (which launchd rejects outright). On a machine that has `plutil`, the plist is additionally linted by Apple's own parser, mirroring the `systemd-analyze verify` check the e2e suite already runs against the systemd units.

What that does **not** cover, and what needs a real Mac to confirm: that `launchctl bootstrap` accepts the agent in the `gui/<uid>` domain, that the 60-second interval fires, and that a spawned `run-exec` genuinely survives `tick` exiting. The first two are conventional; the third is the one with a known counterpart failure on Linux, and is worth watching on the first install.

## 18. Releasing

Every merge to `main` publishes a release. Before this, releases were cut by hand — tag, push, watch — which worked while there were a handful of them and meant that whatever sat on `main` unreleased was invisible: PR #3's fix was merged and then simply didn't ship, because tagging is a separate act that is easy not to perform.

### How the version is decided

There is no version file to bump, and nothing to forget: the next version is derived from the last tag.

| Merge commit says | Result |
|---|---|
| (anything) | patch — `v0.1.17` → `v0.1.18` |
| `[minor]` | `v0.1.17` → `v0.2.0` |
| `[major]` | `v0.1.17` → `v1.0.0` |
| `[skip release]` | no release at all |

Markers are read from the **first line only** — for a squash merge, the PR title. Scanning the whole message means any commit that merely *discusses* a marker trips it, which is not hypothetical: the commit that introduced this workflow described all three in its body, and the first live run skipped itself on the `[skip release]` in its own prose. Had the skip not fired first, the same scan would have read `[major]` out of that sentence and cut a v1.0.0. A marker has to be a deliberate, visible act, not a substring.

`workflow_dispatch` takes an explicit `patch`/`minor`/`major` and wins over any marker; pushing a tag by hand still releases exactly that version, so the manual path is intact.

Conventional-commit parsing was the obvious alternative and was rejected: this repo's history is written in prose sentences ("Make the dashboard fit any terminal…"), and adopting `feat:`/`fix:` prefixes would mean changing how every commit is written to serve the tooling rather than the reader.

### One workflow, not two

The tempting shape — a workflow that pushes a tag, and the existing tag-triggered workflow that builds it — does not work: GitHub deliberately does not fire workflows for events pushed with the default `GITHUB_TOKEN`. The tag would land and nothing would happen. Working around it needs a personal access token stored as a secret. Doing the whole job in one workflow avoids both the trap and the credential, and `gh release create` creates the tag itself as a side effect of publishing.

That same rule is also what stops the obvious loop: the tag this workflow creates cannot re-trigger the workflow.

Releases run under `concurrency: release` so two merges landing close together queue instead of racing to claim the same version number.

### What gates a release

`ci.yml` is new, and its absence was the real gap: until now *nothing* ran on a pull request. The only workflow triggered on tags, so the first time CI saw any code was after it had been merged and tagged. That was survivable when a human decided each release; it is not when merging publishes one. CI now runs gofmt, `go vet`, `go vet` for `GOOS=darwin` (the launchd backend is maintained from Linux machines — see §17 — so nothing else would notice it breaking), the test suite, and a build of all four released platforms.

The release workflow re-runs the tests rather than trusting CI's earlier pass, so the tag can only ever point at code that tested green at that commit.

It also checks the shipped `darwin/arm64` binary carries a code signature, by reading the Mach-O for an `LC_CODE_SIGNATURE` command with a valid `CS_SuperBlob`. Go's linker ad-hoc signs that target even when cross-compiling from Linux, but if it ever stopped, the symptom on Apple Silicon is the binary being `SIGKILL`ed on launch — indistinguishable from jobtail crashing, and impossible to spot from a Linux CI runner. The check was verified to fail on an unsigned binary, not merely to pass on a signed one.

### The cost of this

Every merge bumps the version, and jobtail tells users about new versions: each release means an update-available notice and, for anyone who takes it, a 14 MB download. For a merge that changes nothing they would run — a typo, a note in this document — that is pure noise, which is what `[skip release]` is for. Using it is a judgement call per merge rather than a rule, and if the noise becomes a problem the honest fix is to skip releases for merges that touch no Go code, not to go back to tagging by hand.
