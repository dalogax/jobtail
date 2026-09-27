package main

import (
	"encoding/xml"
	"fmt"
	"path/filepath"
	"strings"
)

// jobtail has no daemon of its own: something outside it runs `jobtail tick`
// once a minute, and tick spawns whatever is due (PRD §12 decision 10). That
// decision is platform-neutral; the thing that implements it is not. Linux
// has systemd --user, macOS has launchd, and they disagree about file format,
// location, activation command, and — the part that actually breaks jobs —
// what happens to the processes tick leaves behind.
//
// Both are described here as plain data so one command can install either,
// and so the macOS backend is testable from a Linux box (and vice versa)
// rather than only on the platform it targets.

// schedulerFile is one file a backend needs on disk.
type schedulerFile struct {
	path    string
	content string
}

// schedulerStep is one command `--enable` runs. Some are expected to fail
// harmlessly — unloading a service that was never loaded, for instance.
type schedulerStep struct {
	argv       []string
	ignoreFail bool
}

// scheduler is one platform's answer to "run this every minute".
type scheduler struct {
	name   string          // for messages: "systemd --user", "launchd"
	unit   string          // the unit/label the user will see in their own tooling
	files  []schedulerFile //
	enable []schedulerStep // what --enable runs
	manual []string        // what to run by hand instead of --enable
	status string          // how to check it afterwards
	// enableHint is appended to the error when --enable fails, for the
	// failure modes that aren't really failures.
	enableHint string
}

// schedulerEnv is everything a backend needs from the running process,
// passed in rather than read from the environment so tests can build either
// backend from anywhere.
type schedulerEnv struct {
	home    string
	exe     string // absolute path to this jobtail binary
	dataDir string
	uid     int
	path    string // PATH to give the scheduled process; see launchdScheduler
	// dataDirEnv is JOBTAIL_DATA_DIR if the installing shell had it set,
	// empty otherwise. A scheduled tick inherits nothing from that shell,
	// so without passing it along the timer would quietly operate on the
	// default database while the TUI showed the custom one — jobs listed
	// and never firing, with nothing to suggest why.
	dataDirEnv string
}

// scheduledEnvironment is the environment the scheduled `jobtail tick` needs,
// as ordered pairs so the rendered files are stable.
func (e schedulerEnv) scheduledEnvironment() [][2]string {
	out := [][2]string{{"PATH", e.path}}
	if e.dataDirEnv != "" {
		out = append(out, [2]string{"JOBTAIL_DATA_DIR", e.dataDirEnv})
	}
	return out
}

func schedulerFor(goos string, env schedulerEnv) (scheduler, error) {
	switch goos {
	case "linux":
		return systemdScheduler(env), nil
	case "darwin":
		return launchdScheduler(env), nil
	default:
		return scheduler{}, fmt.Errorf(
			"no scheduler backend for %s — jobtail needs something to run `%s tick` once a "+
				"minute; a cron entry does the job:\n  * * * * * %s tick",
			goos, env.exe, env.exe)
	}
}

// --- systemd (Linux) -----------------------------------------------------

const systemdUnitName = "jobtail-tick.timer"

// serviceUnit and timerUnit are systemd --user units (PRD §12 decision 8):
// fixed 1-minute resolution, matching cron's own granularity, no configurable
// tick interval.
const serviceUnit = `[Unit]
Description=jobtail: check for due jobs

[Service]
Type=oneshot
ExecStart=%s tick
%s# tick spawns each due job's run-exec as a detached grandchild and returns
# immediately — run-exec is meant to keep running long after this oneshot
# unit itself exits. Without this, systemd's default KillMode
# (control-group) sends every remaining process in this unit's cgroup a
# kill signal the moment tick's own exit deactivates the unit, silently
# killing every just-spawned run-exec before it can do anything. Confirmed
# by an actual stuck job on a real box: KillMode=process, not just a
# careful goroutine/Release() dance in the Go code, is what's needed.
KillMode=process
`

const timerUnit = `[Unit]
Description=jobtail: run jobtail-tick.service every minute

[Timer]
OnCalendar=minutely
Persistent=true

[Install]
WantedBy=timers.target
`

func systemdScheduler(env schedulerEnv) scheduler {
	dir := filepath.Join(env.home, ".config", "systemd", "user")
	return scheduler{
		name: "systemd --user",
		unit: systemdUnitName,
		files: []schedulerFile{
			{filepath.Join(dir, "jobtail-tick.service"), fmt.Sprintf(serviceUnit, env.exe, systemdEnvironment(env))},
			{filepath.Join(dir, systemdUnitName), timerUnit},
		},
		enable: []schedulerStep{
			{argv: []string{"systemctl", "--user", "daemon-reload"}},
			{argv: []string{"systemctl", "--user", "enable", "--now", systemdUnitName}},
		},
		manual: []string{
			"systemctl --user daemon-reload",
			"systemctl --user enable --now " + systemdUnitName,
		},
		status: "systemctl --user list-timers " + systemdUnitName,
	}
}

// --- launchd (macOS) -----------------------------------------------------

// launchdLabel doubles as the plist's filename, which launchd requires to
// match the Label inside it.
const launchdLabel = "com.github.dalogax.jobtail.tick"

func launchdScheduler(env schedulerEnv) scheduler {
	plistPath := filepath.Join(env.home, "Library", "LaunchAgents", launchdLabel+".plist")
	// launchd gives an agent no journal of its own, so unlike the systemd
	// backend there is nowhere for tick's output to go by default. It only
	// ever prints when it actually fires something, so a single file costs
	// almost nothing and is the only way to answer "did the scheduler run?"
	logPath := filepath.Join(env.dataDir, "scheduler.log")
	domain := fmt.Sprintf("gui/%d", env.uid)
	service := domain + "/" + launchdLabel

	return scheduler{
		name:  "launchd",
		unit:  launchdLabel,
		files: []schedulerFile{{plistPath, launchdPlist(env.exe, logPath, env.scheduledEnvironment())}},
		enable: []schedulerStep{
			// Unload any previous copy first so re-running this command
			// picks up a new binary path or PATH. Fails when nothing is
			// loaded yet, which is the normal first-install case.
			{argv: []string{"launchctl", "bootout", service}, ignoreFail: true},
			// Undo a previous `launchctl disable`, which otherwise makes
			// bootstrap succeed while the agent stays inert.
			{argv: []string{"launchctl", "enable", service}, ignoreFail: true},
			{argv: []string{"launchctl", "bootstrap", domain, plistPath}},
		},
		manual: []string{
			"launchctl bootstrap " + domain + " " + plistPath,
		},
		status: "launchctl print " + service,
		// bootstrap into gui/<uid> needs a live login session, so it fails
		// when jobtail is installed over SSH with nobody logged in at the
		// screen. That isn't fatal: launchd loads everything in
		// ~/Library/LaunchAgents into that domain at login anyway, and the
		// plist is already written by this point.
		enableHint: "the agent file is written; if this failed because there is no " +
			"logged-in session (installing over SSH, say), it will load by itself at the " +
			"next login, or run the command above from a terminal on the Mac itself",
	}
}

// launchdPlist renders the LaunchAgent. The two keys worth knowing about are
// AbandonProcessGroup and EnvironmentVariables; both are explained in the
// comments the plist itself carries, since that is where someone debugging a
// Mac will be looking.
func launchdPlist(exe, logPath string, environment [][2]string) string {
	var envVars strings.Builder
	for _, kv := range environment {
		fmt.Fprintf(&envVars, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n",
			xmlEscape(kv[0]), xmlEscape(kv[1]))
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchdLabel + `</string>

	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(exe) + `</string>
		<string>tick</string>
	</array>

	<!-- Every minute, matching cron's own granularity (PRD §12 decision 8). -->
	<key>StartInterval</key>
	<integer>60</integer>

	<key>RunAtLoad</key>
	<true/>

	<!-- tick spawns each due job's run-exec as a detached grandchild and
	     returns immediately; run-exec is meant to outlive it by minutes or
	     hours. Without AbandonProcessGroup, launchd kills every process
	     left in the job's process group the moment tick exits, so every
	     run would die before it could do anything. This is the exact
	     counterpart of KillMode=process in the systemd unit, where that
	     same failure was already seen for real. -->
	<key>AbandonProcessGroup</key>
	<true/>

	<!-- A LaunchAgent inherits almost nothing: its PATH is
	     /usr/bin:/bin:/usr/sbin:/sbin, which has neither Homebrew nor
	     ~/.local/bin on it. Agent jobs shell out to claude/opencode/codex
	     and cli jobs to whatever they like, none of which would resolve.
	     This is the PATH as it stood when "jobtail install-scheduler" ran —
	     re-run that command after installing tools somewhere new. -->
	<key>EnvironmentVariables</key>
	<dict>
` + envVars.String() + `	</dict>

	<!-- launchd agents have no journal; without this tick's output is lost. -->
	<key>StandardOutPath</key>
	<string>` + xmlEscape(logPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(logPath) + `</string>
</dict>
</plist>
`
}

// schedulerPath is the PATH to hand the scheduled process: the one in effect
// now, plus the usual places a Mac keeps user-installed binaries, in case
// jobtail is being installed from a shell that doesn't have them either.
// Only the launchd backend uses it — systemd user services inherit the user
// manager's environment, and changing that is a decision for whoever set the
// machine up, not for jobtail.
func schedulerPath(current, home string) string {
	if current == "" {
		current = "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	have := map[string]bool{}
	out := strings.Split(current, ":")
	for _, p := range out {
		have[p] = true
	}
	for _, p := range []string{
		"/opt/homebrew/bin",                  // Homebrew on Apple Silicon
		"/usr/local/bin",                     // Homebrew on Intel
		filepath.Join(home, ".local", "bin"), // where jobtail itself installs
	} {
		if !have[p] {
			out = append(out, p)
		}
	}
	return strings.Join(out, ":")
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// systemdEnvironment renders Environment= lines for the service unit. Unlike
// launchd, a systemd --user service inherits the user manager's environment,
// which on a normal desktop session already has the PATH the user expects —
// so PATH is deliberately left alone here rather than pinned to whatever
// shell happened to run the install. JOBTAIL_DATA_DIR is different: it is
// jobtail's own setting, it is not in anyone's manager environment, and
// getting it wrong points the timer at a different database than the
// dashboard.
func systemdEnvironment(env schedulerEnv) string {
	var b strings.Builder
	for _, kv := range env.scheduledEnvironment() {
		if kv[0] == "PATH" {
			continue
		}
		fmt.Fprintf(&b, "Environment=%s=%s\n", kv[0], kv[1])
	}
	return b.String()
}
