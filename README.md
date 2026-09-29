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

<p align="center"><img src="docs/demo.gif" alt="jobtail demo: adding a cli job and an agent job, opening the dashboard, running a job, drilling into a failed run's log, and live-tailing an agent run's transcript" width="900"></p>

<p align="center"><sub>Add a shell job and an agent job → open the dashboard → run one now → drill into a failed run's log → watch an agent run stream in live. (Sped up; recorded from <a href="scripts/demo/demo.tape"><code>scripts/demo/demo.tape</code></a>.)</sub></p>

## What it is

`jobtail` is a small scheduler and dashboard for two kinds of recurring job:

- **`cli`** — a plain shell command (a health check, a backup script, a cleanup task).
- **`agent`** — a headless turn from [Claude Code](https://claude.com/claude-code), [opencode](https://opencode.ai), or [Codex](https://github.com/openai/codex): a prompt, a working directory, a model, and `--provider` to pick which CLI runs it (default `claude`). The full transcript is captured and rendered as a readable log, not raw JSON — the parser is provider-aware, so this works the same regardless of which agent CLI a job uses.

A per-user timer — `systemd --user` on Linux, a LaunchAgent on macOS — checks for due jobs once a minute; each due job runs as its own process and writes its own log file. Everything — job definitions, run history, status — lives in one SQLite database. There's no daemon of jobtail's own to keep alive, and nothing tying it to any particular terminal, multiplexer, or session.

## You talk to your agent; your agent drives jobtail

The `jobtail` CLI isn't really meant for you to type. It's built to be driven by a coding agent like Claude Code, opencode or Codex. What you use is the dashboard.

You ask your agent in plain words:

> every night at 3, check ~/code/shop for outdated dependencies and open a PR if it's safe to bump them

The agent then does the rest on your behalf:
- turns that into a `jobtail add` with the right cron, working directory and prompt
- sets a timeout and a precheck where they make sense
- test-runs the job once
- tells you when it's scheduled

Later, "why did the backup check fail last night?" has it read the run's log for you. When you want to look for yourself, run `jobtail` and the dashboard opens.

What makes this work is the **jobtail skill** ([`skills/jobtail/SKILL.md`](skills/jobtail/SKILL.md)). It teaches an agent:
- how to install jobtail and check that its timer is actually running
- how to write prompts that work unattended
- how to gate agent runs behind a cheap precheck
- how to read runs as JSON
- what not to do, like hand-writing crontabs or `rm`-ing a job's history

The installer offers to add the skill for every supported agent it finds. You can also add it, or update it, at any time:

```sh
jobtail install-skill                   # every agent it finds: claude, opencode, codex
jobtail install-skill --agent claude    # or just the ones you name
```

The skill is embedded in the binary, so the version installed always matches the jobtail it came from. `jobtail upgrade` refreshes every copy that's already installed.

If you do want to use the CLI by hand, nothing stops you:

```sh
jobtail add backup-check --kind cli --cron "*/15 * * * *" \
  --cwd ~/scripts --cmd "./check_backups.sh"

jobtail add nightly-review --kind agent --cron "0 3 * * *" \
  --cwd ~/code/myproject --timeout-seconds 1800 \
  --prompt "Check for outdated dependencies. Open a PR if it's safe to update; otherwise report why not."

jobtail          # opens the dashboard
```

## Screenshot

<p align="center"><img src="docs/screenshot.png" alt="jobtail dashboard: jobs and runs side by side on top, log spanning the full width below" width="900"></p>

Jobs and runs side by side on top; the log — raw output for `cli` jobs, a rendered transcript for `agent` jobs — spans the full width below. Click or use the keyboard to move between them; live-tails while a run is in progress. Failed runs are red, so "is anything broken?" is answered at a glance.

### It fits the terminal you actually have

The dashboard adapts to the window instead of assuming a wide one. There are three layouts, picked from the terminal's size:

| Terminal | Layout |
| --- | --- |
| ≥ 100 cols | jobs and runs side by side, log full width below |
| 56–99 cols | all three panes full width, stacked |
| < 56 cols, or short | one pane at a time — the drill-down it already was |

<p align="center"><img src="docs/screenshot-narrow.png" alt="jobtail on a 46-column terminal: a single full-width jobs pane with readable job names and statuses" width="320"></p>

Columns are dropped whole rather than squeezed into ellipses, worst-first, so what's left stays readable — job names and statuses survive to the very last; run counts and cron expressions are the first to go. Spare width goes the other way: on a wide terminal the jobs table gains a `Cron` column and the runs table an `Exit` one. Panes are sized to their contents too, so a long job list gets the rows it needs instead of a fixed fraction of the screen.

On a phone-sized terminal (SSH from a handset, a narrow split) the panes stop competing: you get one, full width, and move through jobs → runs → log with `enter` and `esc` exactly as before.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/dalogax/jobtail/main/install.sh | sh
```

Downloads the right release binary for your OS/arch (Linux and macOS, amd64/arm64), installs it to `~/.local/bin`, and adds that to your `PATH` if it isn't there already. No Go toolchain, no package manager required.

If it finds claude, opencode or codex on your machine, it asks whether to install the [jobtail skill](#you-talk-to-your-agent-your-agent-drives-jobtail) for them. With no terminal attached (CI, a provisioning script), it doesn't ask; it prints the command to run instead. Set `JOBTAIL_SKILL=yes` or `JOBTAIL_SKILL=no` to answer ahead of time.

Then wire up the scheduler (or let your agent do it: the skill checks this first):

```sh
jobtail install-scheduler --enable
```

That writes the per-user timer your platform uses — `systemd --user` units on Linux, a LaunchAgent on macOS — and starts it. jobtail has no daemon of its own: the timer just runs `jobtail tick` once a minute, and `tick` starts whatever is due.

<details>
<summary>On macOS, two details worth knowing</summary>

A LaunchAgent starts with almost no environment: its `PATH` is just `/usr/bin:/bin:/usr/sbin:/sbin`, which has neither Homebrew nor `~/.local/bin` on it — so `claude`, `opencode`, `codex` and most of what a `cli` job shells out to would simply not resolve. `install-scheduler` therefore records your current `PATH` (plus the usual Homebrew locations) in the agent. **Re-run it after installing a tool somewhere new**, and after moving the `jobtail` binary. Same goes for `JOBTAIL_DATA_DIR`, if you set one.

Scheduled jobs run without a UI, so anything touching Desktop, Documents, or Downloads can hit a macOS privacy prompt that nothing is there to answer, and the job just fails. If that happens, grant Full Disk Access to the `jobtail` binary in System Settings → Privacy & Security.

To check on it: `launchctl print gui/$(id -u)/com.github.dalogax.jobtail.tick`. The timer's own output goes to `~/.local/share/jobtail/scheduler.log`.

</details>

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
- **Session capture + resume.** Every agent run's session (or, for codex, thread) id is captured as soon as it starts, even if the run later fails. Select a run in the dashboard and press **`r`** — or use `jobtail resume <run-id>` — to reopen that exact session interactively in the CLI that produced it (`claude --resume`, `opencode --session`, or `codex resume`), so you can pick up where an unattended run got stuck instead of starting over. Reading a run's log and deciding to continue it by hand is one keypress.

  Resuming means the agent CLI keeps its own session transcript on disk (for claude, under `~/.claude/projects/`), so scheduled agent runs will accumulate there and show up in that tool's own session picker. `jobtail gc` prunes jobtail's runs and logs, not another tool's session store.
- **A sane default permission mode.** Unattended `claude`/`codex` runs default to `acceptEdits`/`workspace-write` respectively — auto-accepts file edits, never `bypassPermissions`/`danger-full-access` unless a job explicitly opts in via `--permission-mode` (its meaning is provider-specific — see `jobtail add --help`). (Avoid `--permission-mode plan` for scheduled `claude` jobs: it expects an interactive approval that headless mode can never provide, and the run will just hang.)

This isn't tied to any particular terminal or workflow — it works the same whether you're driving it from a plain SSH session, tmux, or nothing open at all (the timer doesn't need a terminal to fire).

### Only run the agent when there's something to do

An agent turn costs real time and real tokens, so a job can carry a `--precheck`: a shell command run before it that decides whether the job runs at all.

```sh
jobtail add triage-backlog --kind agent --cron "0 * * * *" \
  --cwd ~/code/myproject \
  --precheck './list-open-tickets.sh' \
  --precheck-timeout-seconds 30 \
  --prompt "Triage each ticket below: reproduce it, and either fix it or explain why not."
```

| The precheck exits | What happens |
|---|---|
| `0` | The job runs, and the precheck's output is handed to the agent as `PENDING ITEMS` context appended to the prompt |
| `1` | The run is marked **skipped** — the agent never starts |
| anything else | The run is marked **failed** |

So the cheap, deterministic half ("is there anything to do, and what?") stays an ordinary shell command you can test on its own, and the expensive half only runs when the answer is yes — on exactly the items the gate found.

The precheck's output is always written to the run's log, whatever it decided, so a skipped run still shows you what it saw. `--precheck-timeout-seconds` bounds the gate itself, separately from `--timeout-seconds` for the job.

## Herdr integration (optional)

[Herdr](https://herdr.dev) is a terminal workspace manager built around AI coding agent panes. jobtail isn't a Herdr plugin and doesn't need one — `jobtail` is just a command that runs fine in any tab or pane, Herdr's included. If you do use Herdr, two things light up automatically:

- A failed run triggers a `herdr notification`, if `herdr` is on `PATH` — a desktop toast even if you're not looking at the dashboard.
- Resuming a run — `r` in the dashboard, or `jobtail resume` — opens the session in a new Herdr tab (`herdr tab create` + `herdr pane run`) instead of just printing instructions.

Neither requires any setup — jobtail detects `herdr` on `PATH` at the moment it'd be useful and silently skips both if it's not there.

## CLI reference

```
jobtail add <id>          Register a new scheduled job
jobtail list               List all jobs
jobtail show <id>          Show one job's detail and recent runs
jobtail runs <id>          List run history for one job
jobtail log <run-id>       Dump one run's log to stdout
jobtail run <id>           Manually trigger a job now and wait for it to finish
jobtail resume <run-id>    Reopen an agent run's session interactively (also `r` in the dashboard)
jobtail enable/disable <id>  Pause or resume a job without deleting it
jobtail edit <id>           Change one or more fields of an existing job
jobtail rm <id>             Delete a job and its run history
jobtail gc                 Prune old runs (and their logs) past retention
jobtail install-scheduler  Install the per-user timer that runs due jobs
jobtail upgrade            Check for and install a newer release
jobtail install-skill      Install the jobtail skill for your coding agents
jobtail                    Open the dashboard (same as `jobtail tui`)
```

`list`, `show` and `runs` take `--json` for scripting. `jobtail <command> --help` for the full flag list.

## How it works, and why

The full design — every decision, the alternatives considered, and why they were rejected — is written up in [`PRD.md`](PRD.md). Short version: single Go binary, no config file, SQLite in WAL mode, a per-user OS timer instead of a bundled scheduler daemon, and a Bubble Tea TUI that's a plain terminal program rather than anything requiring a specific host.

## License

MIT — see [`LICENSE`](LICENSE).
