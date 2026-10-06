# Records docs/demo.gif, the README's walkthrough.
#
#   ./scripts/demo/setup.sh          # builds jobtail, seeds /tmp/jobtail-demo
#   vhs scripts/demo/demo.tape       # needs vhs + ttyd + ffmpeg on PATH
#
# The README's three stills come from screenshot.tape, screenshot-form.tape
# and screenshot-narrow.tape, run the same way.
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

# 1. Register a job the way an agent would: one CLI call.
Type `jobtail add deps-review --kind agent --cron "0 3 * * *" --cwd ~/code/shop \`
Enter
Type `  --prompt "Check for outdated deps. Open a PR if safe to update."`
Sleep 300ms
Enter
Sleep 1.2s

# 2. Open the dashboard.
Type "jobtail"
Sleep 300ms
Enter
Sleep 1.5s

# 3. Or by hand: n opens the job form over the dashboard. enter types into a
#    field, enter again leaves it, s saves.
Type "n"
Sleep 1.2s
Enter
Type "backups"
Enter
Sleep 300ms
Down
Down
Enter
Type "*/15 * * * *"
Enter
Sleep 300ms
Down
Enter
Ctrl+U
Type "~/scripts"
Enter
Sleep 300ms
Down
Enter
Type "./check_backups.sh"
Enter
Sleep 1.2s
Type "s"
Sleep 1.2s

# 4. Run it now: it goes green.
Type "r"
Sleep 1.5s

# 4b. Every key, including the ones a narrow footer has to leave out.
Type "?"
Sleep 2.5s
Escape
Sleep 500ms

# 5. A failing job is red at a glance. Read why, then clear its history with
#    c: it asks first, and says what it will delete.
Down
Sleep 600ms
Enter
Sleep 600ms
Enter
Sleep 1.8s
Escape
Sleep 400ms
Type "c"
Sleep 1.8s
Type "y"
Sleep 1.2s
Escape
Sleep 500ms

# 6. e edits a job in the same form, filled in: move the agent job to
#    weekday mornings.
Down
Sleep 500ms
Type "e"
Sleep 1.2s
Enter
Ctrl+U
Type "0 6 * * 1-5"
Enter
Sleep 1.5s
Type "s"
Sleep 1.2s

# 7. Kick off the agent job and watch its transcript live-tail.
Type "r"
Sleep 500ms
Enter
Sleep 300ms
Enter
Sleep 9s

# 8. The transcript is blocks, not a wall of text: step through the tool calls
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

# 9. Pick the session back up where the unattended run left it. `r` here does
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
