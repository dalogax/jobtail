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
  permission_mode TEXT,                     -- kind='agent': --permission-mode (default 'acceptEdits')
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
  session_id    TEXT,                       -- kind='agent' only: claude session id, for --resume
  log_path      TEXT NOT NULL
);

CREATE INDEX idx_runs_job ON runs(job_id, started_at DESC);
```

`run_count` and `last_status` shown in the job list are `SELECT count(*)` / latest `runs` row — not duplicated columns, to keep them always correct.

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
                                   # attached to `claude --resume <session_id>`, seeded
                                   # from that run's session_id — for picking up a
                                   # failed/incomplete agent run by hand

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

Three-pane layout inside a Herdr tab (opened per §8 — either a plain `jobtail tui` in a manually-created tab, or via the optional plugin's keybinding), Bubble Tea (`charmbracelet/bubbletea` + `bubbles` table/viewport + `lipgloss`), pattern deliberately close to `lazygit`/`k9s` since that's already muscle memory here.

```
┌─ Jobs ──────────────────────┬─ Runs: nightly-deps ─────────┬─ Log: run 8f2a ─────────────┐
│ ● nightly-deps    agent  47 │ ✓ 2026-09-26 03:00   1m12s   │ {"type":"assistant",...}     │
│ ○ backup-check    cli   912 │ ✓ 2026-09-25 03:00   0m54s   │ {"type":"tool_use",...}      │
│ ● cert-renew      cli     6 │ ✗ 2026-09-24 03:00   0m08s   │ {"type":"result","is_error"..│
│                              │ ✓ 2026-09-23 03:00   1m30s   │                              │
└──────────────────────────────┴───────────────────────────────┴──────────────────────────────┘
  j/k move · enter drill in · esc back · e enable/disable · r run now · R resume (agent, failed run) · / filter · q close
```

- **Left pane (jobs)**: `●`/`○` = enabled/disabled, type badge, run count. Row color = last status (green ok / red failed / yellow running / grey never-run).
- **Middle pane (runs)**: for the selected job, newest first; status glyph, start time, duration, trigger badge (S/M for scheduled/manual).
- **Right pane (log)**: for the selected run. `cli` jobs render the raw log as a scrolling text viewport. `agent` jobs parse the `stream-json` lines and render them the way a transcript reads — assistant text, tool calls with their input, tool results collapsed by default (expandable), final result/cost line — not raw JSON by default (raw available via a `J` toggle for debugging).
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
