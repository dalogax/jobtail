# Records docs/screenshot.png, the README's log-pane still: an agent
# transcript folded into blocks, beside the jobs and runs it belongs to.
#
#   vhs scripts/demo/screenshot.tape   # same setup and tools as demo.tape

Output "/tmp/jobtail-screenshot.gif"

Set Shell "bash"
Set FontSize 15
Set Width 1400
Set Height 820
Set Padding 16
Set Theme "Catppuccin Mocha"

Hide
Type `eval "$(./scripts/demo/setup.sh)" && cd ~ && clear`
Enter
Sleep 4s
Type `jobtail add backups --kind cli --cron "*/15 * * * *" --cwd ~/scripts --cmd ./check_backups.sh >/dev/null && jobtail run backups >/dev/null`
Enter
Type `jobtail add deps-review --kind agent --cron "0 3 * * *" --cwd ~/code/shop --prompt "Check for outdated deps. Open a PR if safe to update." >/dev/null && jobtail run deps-review >/dev/null && clear`
Enter
Sleep 12s
Type "jobtail"
Enter
Sleep 1.5s
# deps-review -> its run -> the log, then step the cursor onto a tool call.
Down
Down
Enter
Enter
Sleep 1s
Type "k"
Type "k"
Type "k"
Sleep 1s
Show
Sleep 500ms
Screenshot docs/screenshot.png
Sleep 200ms
