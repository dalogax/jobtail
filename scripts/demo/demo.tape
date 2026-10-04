# Records docs/demo.gif, the README's walkthrough.
#
#   ./scripts/demo/setup.sh          # builds jobtail, seeds /tmp/jobtail-demo
#   vhs scripts/demo/demo.tape       # needs vhs + ttyd + ffmpeg on PATH
#
# The README's two stills come from screenshot.tape and screenshot-narrow.tape,
# run the same way.
#
# Everything runs against the throwaway data dir setup.sh creates. Two binaries
# are stood in for: the agent job runs scripts/demo/fake_claude.sh instead of
# the real `claude`, and `herdr` is scripts/demo/fake_herdr.sh, which must
# shadow any real one — see that file for why, and for the single staged thing
# in this demo (a resumed session prints here rather than in a tab that a
# one-terminal recorder cannot film).

Output docs/demo.gif

Set Shell "bash"
Set FontSize 15
Set Width 1400
Set Height 820
Set Padding 16
Set Theme "Catppuccin Mocha"
Set TypingSpeed 18ms
Set PlaybackSpeed 1.6
Set Framerate 24

Hide
Type `eval "$(./scripts/demo/setup.sh)" && cd ~ && PS1='\[\e[38;5;212m\]❯\[\e[0m\] ' && clear`
Enter
Sleep 4s
Show

# 1. Register jobs: a shell command, and a headless agent run.
Type `jobtail add backups --kind cli --cron "*/15 * * * *" --cwd ~/scripts --cmd ./check_backups.sh`
Sleep 300ms
Enter
Sleep 1s
Type `jobtail add deps-review --kind agent --cron "0 3 * * *" --cwd ~/code/shop \`
Enter
Type `  --prompt "Check for outdated deps. Open a PR if safe to update."`
Sleep 300ms
Enter
Sleep 1.5s

# 2. Open the dashboard.
Type "jobtail"
Sleep 300ms
Enter
Sleep 2s

# 3. Run the backups job now: it goes green.
Type "r"
Sleep 1.8s

# 3b. Every key, including the ones a narrow footer has to leave out.
Type "?"
Sleep 2.5s
Escape
Sleep 500ms

# 4. A failing job is red at a glance. Drill into its log and back out.
Down
Sleep 800ms
Enter
Sleep 800ms
Enter
Sleep 2s
Escape
Sleep 300ms
Escape
Sleep 600ms

# 5. Kick off the agent job and watch its transcript live-tail.
Down
Sleep 700ms
Type "r"
Sleep 500ms
Enter
Sleep 300ms
Enter
Sleep 9s

# 6. The transcript is blocks, not a wall of text: step through the tool calls
#    with j/k, open one with enter, open everything with o.
Type "k"
Sleep 1.5s
Enter
Sleep 2.5s
Enter
Sleep 800ms
Type "j"
Sleep 1s
Type "j"
Sleep 1.2s
Type "o"
Sleep 2.5s
Type "o"
Sleep 1.2s

# 7. Pick the session back up where the unattended run left it. `r` here does
#    the same thing; run from the shell so the handoff is visible in one frame.
Type "q"
Sleep 800ms
# Hide stops new frames being recorded; it does not stop the typing reaching
# the terminal. Clear while hidden, or the id lookup stays on screen.
Hide
Type `last=$(jobtail runs deps-review --json | jq -r '.[0].ID')`
Enter
Sleep 400ms
Type "clear"
Enter
Sleep 400ms
Show
Type "jobtail resume $last"
Sleep 300ms
Enter
Sleep 4s
