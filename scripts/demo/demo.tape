# Records docs/demo.gif, the README's walkthrough.
#
#   ./scripts/demo/setup.sh          # builds jobtail, seeds /tmp/jobtail-demo
#   vhs scripts/demo/demo.tape       # needs vhs + ttyd + ffmpeg on PATH
#
# Everything runs against the throwaway data dir setup.sh creates, and the
# agent job runs scripts/demo/fake_claude.sh instead of the real `claude`.

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
Sleep 2s

# 4. A failing job is red at a glance. Drill into its log.
Down
Sleep 1s
Enter
Sleep 1s
Enter
Sleep 2.5s
Escape
Sleep 400ms
Escape
Sleep 800ms

# 5. Kick off the agent job and watch its transcript live-tail.
Down
Sleep 800ms
Type "r"
Sleep 600ms
Enter
Sleep 400ms
Enter
Sleep 8s

Type "q"
Sleep 1s
