# Records docs/screenshot-form.png, the README's still of the job form: the
# agent job open with e, the cursor on its prompt. Run like demo.tape:
#
#   ./scripts/demo/setup.sh && vhs scripts/demo/screenshot-form.tape

Output "/tmp/jobtail-screenshot-form.gif"

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
Type `jobtail add deps-review --kind agent --cron "0 3 * * *" --cwd ~/code/shop --timeout-seconds 1800 --notify started,failed,timeout --prompt "Check for outdated deps. Open a PR if safe to update." >/dev/null && clear`
Enter
Sleep 1s
Type "jobtail"
Enter
Sleep 1.5s
Down
Down
Type "e"
Sleep 500ms
Down
Down
Sleep 500ms
Show
Sleep 500ms
Screenshot docs/screenshot-form.png
Sleep 200ms
