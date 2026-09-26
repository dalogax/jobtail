<p align="center">
  <img src="docs/logo.png" alt="jobtail" width="600">
</p>

<p align="center">
  Schedule and watch recurring jobs — plain shell commands <em>or</em> headless AI coding-agent runs —<br>
  from a three-pane terminal dashboard. One static binary, one SQLite file, no config to hand-edit.
</p>

<p align="center">
  <a href="https://github.com/dalogax/jobtail/releases/latest"><img src="https://img.shields.io/github/v/release/dalogax/jobtail" alt="latest release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="MIT license"></a>
</p>

---

## What it is

`jobtail` is a small scheduler and dashboard for two kinds of recurring job:

- **`cli`** — a plain shell command (a health check, a backup script, a cleanup task).
- **`agent`** — a headless turn from [Claude Code](https://claude.com/claude-code), [opencode](https://opencode.ai), or [Codex](https://github.com/openai/codex): a prompt, a working directory, a model, and `--provider` to pick which CLI runs it (default `claude`). The full transcript is captured and rendered as a readable log, not raw JSON — the parser is provider-aware, so this works the same regardless of which agent CLI a job uses.

A `systemd --user` timer checks for due jobs once a minute; each due job runs as its own process and writes its own log file. Everything — job definitions, run history, status — lives in one SQLite database. There's no daemon of jobtail's own to keep alive, and nothing tying it to any particular terminal, multiplexer, or session.

```
jobtail add backup-check --kind cli --cron "*/15 * * * *" \
  --cwd ~/scripts --cmd "./check_backups.sh"

jobtail add nightly-review --kind agent --cron "0 3 * * *" \
  --cwd ~/code/myproject \
  --prompt "Check for outdated dependencies. Open a PR if it's safe to update; otherwise report why not."

jobtail          # opens the dashboard
```

## Screenshot

<p align="center"><img src="docs/screenshot.png" alt="jobtail dashboard: jobs and runs side by side on top, log spanning the full width below" width="900"></p>

Jobs and runs side by side on top; the log — raw output for `cli` jobs, a rendered transcript for `agent` jobs — spans the full width below. Click or use the keyboard to move between them; live-tails while a run is in progress.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/dalogax/jobtail/main/install.sh | sh
```

Downloads the right release binary for your OS/arch (Linux and macOS, amd64/arm64), installs it to `~/.local/bin`, and adds that to your `PATH` if it isn't there already. No Go toolchain, no package manager required.

Then wire up the scheduler:

```sh
jobtail install-systemd --enable
```

`jobtail upgrade` checks for and installs newer releases later; every other command also prints a one-line heads-up on stderr when one's available, so you don't have to remember to check.

## Using it with AI coding agents

`--kind agent` jobs run a headless turn from whichever CLI `--provider` names — `claude` (default), `opencode`, or `codex` — the same non-interactive mode each of those tools exposes on its own, just on a schedule instead of triggered by hand:

```sh
jobtail add nightly-review --kind agent --provider opencode --cron "0 3 * * *" \
  --cwd ~/code/myproject --model opencode/big-pickle \
  --prompt "Check for outdated dependencies. Open a PR if it's safe to update; otherwise report why not."
```

Nothing agent-specific needs a special environment: give it a prompt and a working directory, same as a `cli` job needs a command and a working directory.

A few things exist specifically because the job is an agent, not a shell command:

- **Readable transcripts, not raw JSON.** The log pane parses each provider's own event stream and shows assistant text, tool calls (with their input), and the final result — not a wall of `{"type":"assistant",...}`.
- **Session capture + resume.** Every agent run's session (or, for codex, thread) id is captured as soon as it starts, even if the run later fails. `jobtail resume <run-id>` hands a failed run to an interactive session in the same CLI that produced it (`claude --resume`, `opencode --session`, or `codex resume`), so you can pick up exactly where an unattended run got stuck instead of starting over.
- **A sane default permission mode.** Unattended `claude`/`codex` runs default to `acceptEdits`/`workspace-write` respectively — auto-accepts file edits, never `bypassPermissions`/`danger-full-access` unless a job explicitly opts in via `--permission-mode` (its meaning is provider-specific — see `jobtail add --help`). (Avoid `--permission-mode plan` for scheduled `claude` jobs: it expects an interactive approval that headless mode can never provide, and the run will just hang.)

This isn't tied to any particular terminal or workflow — it works the same whether you're driving it from a plain SSH session, tmux, or nothing open at all (the systemd timer doesn't need a terminal to fire).

## Herdr integration (optional)

[Herdr](https://herdr.dev) is a terminal workspace manager built around AI coding agent panes. jobtail isn't a Herdr plugin and doesn't need one — `jobtail` is just a command that runs fine in any tab or pane, Herdr's included. If you do use Herdr, two things light up automatically:

- A failed run triggers a `herdr notification`, if `herdr` is on `PATH` — a desktop toast even if you're not looking at the dashboard.
- `jobtail resume` opens the resumed session in a new Herdr tab (`herdr tab create` + `herdr pane run`) instead of just printing instructions.

Neither requires any setup — jobtail detects `herdr` on `PATH` at the moment it'd be useful and silently skips both if it's not there.

## CLI reference

```
jobtail add <id>          Register a new scheduled job
jobtail list               List all jobs
jobtail show <id>          Show one job's detail and recent runs
jobtail runs <id>          List run history for one job
jobtail log <run-id>       Dump one run's log to stdout
jobtail run <id>           Manually trigger a job now and wait for it to finish
jobtail resume <run-id>    Hand a failed agent run to an interactive session
jobtail enable/disable <id>  Pause or resume a job without deleting it
jobtail edit <id>           Change one or more fields of an existing job
jobtail rm <id>             Delete a job and its run history
jobtail gc                 Prune old runs (and their logs) past retention
jobtail upgrade            Check for and install a newer release
jobtail                    Open the dashboard (same as `jobtail tui`)
```

Every command supports `--json` for scripting. `jobtail <command> --help` for the full flag list.

## How it works, and why

The full design — every decision, the alternatives considered, and why they were rejected — is written up in [`PRD.md`](PRD.md). Short version: single Go binary, no config file, SQLite in WAL mode, a systemd user timer instead of a bundled scheduler daemon, and a Bubble Tea TUI that's a plain terminal program rather than anything requiring a specific host.

## License

MIT — see [`LICENSE`](LICENSE).
