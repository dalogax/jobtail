# Records docs/screenshot-narrow.png: the dashboard in a 46-column pane,
# where it falls back to one pane at a time.
#
#   vhs scripts/demo/screenshot-narrow.tape   # same setup as demo.tape

Output "/tmp/jobtail-screenshot-narrow.gif"

Set Shell "bash"
Set FontSize 15
Set Width 470
Set Height 400
Set Padding 8
Set Theme "Catppuccin Mocha"

Hide
Type `eval "$(./scripts/demo/setup.sh)" && cd ~ && clear`
Enter
Sleep 4s
Type `jobtail add backups --kind cli --cron "*/15 * * * *" --cwd ~/scripts --cmd ./check_backups.sh >/dev/null && jobtail run backups >/dev/null`
Enter
Type `jobtail add deps-review --kind agent --cron "0 3 * * *" --cwd ~/code/shop --prompt "Check for outdated deps. Open a PR if safe to update." >/dev/null && clear`
Enter
Sleep 1s
Type "jobtail"
Enter
Sleep 1.5s
Down
Sleep 500ms
Show
Sleep 500ms
Screenshot docs/screenshot-narrow.png
Sleep 200ms
