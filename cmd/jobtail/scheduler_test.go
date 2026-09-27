package main

import (
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The macOS backend is the reason these tests parse rather than grep. A
// LaunchAgent that is subtly wrong doesn't fail loudly: launchd loads it and
// the jobs simply never run, or run and get killed, with nothing on screen to
// say so. Since the backend has to be written and maintained from machines
// that aren't Macs, the checks below assert on the *parsed* plist — the same
// keys and values launchd itself will read — and additionally hand the file
// to `plutil` whenever the test happens to run somewhere that has it.

func testEnv() schedulerEnv {
	return schedulerEnv{
		home:    "/Users/dani",
		exe:     "/Users/dani/.local/bin/jobtail",
		dataDir: "/Users/dani/.local/share/jobtail",
		uid:     501,
		path:    "/opt/homebrew/bin:/usr/bin:/bin",
	}
}

// --- plist parsing -------------------------------------------------------

type plistNode struct {
	XMLName xml.Name
	Chars   string      `xml:",chardata"`
	Nodes   []plistNode `xml:",any"`
}

type plistFile struct {
	XMLName xml.Name    `xml:"plist"`
	Nodes   []plistNode `xml:",any"`
}

// parsePlist decodes an Apple XML plist into Go values: <string>/<integer>
// become strings, <true/>/<false/> booleans, <array> a slice, <dict> a map.
func parsePlist(t *testing.T, s string) map[string]any {
	t.Helper()
	var f plistFile
	if err := xml.Unmarshal([]byte(s), &f); err != nil {
		t.Fatalf("plist is not well-formed XML: %v\n%s", err, s)
	}
	if len(f.Nodes) != 1 || f.Nodes[0].XMLName.Local != "dict" {
		t.Fatalf("plist root should hold exactly one <dict>, got %d nodes", len(f.Nodes))
	}
	return dictOf(t, f.Nodes[0])
}

func dictOf(t *testing.T, n plistNode) map[string]any {
	t.Helper()
	out := map[string]any{}
	for i := 0; i < len(n.Nodes); i++ {
		if n.Nodes[i].XMLName.Local != "key" {
			t.Fatalf("expected <key> at position %d in <dict>, got <%s>", i, n.Nodes[i].XMLName.Local)
		}
		if i+1 >= len(n.Nodes) {
			t.Fatalf("<key>%s</key> has no value after it", n.Nodes[i].Chars)
		}
		out[n.Nodes[i].Chars] = valueOf(t, n.Nodes[i+1])
		i++
	}
	return out
}

func valueOf(t *testing.T, n plistNode) any {
	t.Helper()
	switch n.XMLName.Local {
	case "string", "integer":
		return n.Chars
	case "true":
		return true
	case "false":
		return false
	case "array":
		var out []any
		for _, c := range n.Nodes {
			out = append(out, valueOf(t, c))
		}
		return out
	case "dict":
		return dictOf(t, n)
	default:
		t.Fatalf("unexpected plist value type <%s>", n.XMLName.Local)
		return nil
	}
}

// --- launchd -------------------------------------------------------------

func TestLaunchdAgentIsWhereLaunchdLooksForIt(t *testing.T) {
	s, err := schedulerFor("darwin", testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.files) != 1 {
		t.Fatalf("expected exactly one plist, got %d files", len(s.files))
	}
	want := "/Users/dani/Library/LaunchAgents/" + launchdLabel + ".plist"
	if s.files[0].path != want {
		t.Errorf("plist path = %q, want %q", s.files[0].path, want)
	}
	// launchd requires the filename to match the Label inside.
	base := strings.TrimSuffix(filepath.Base(s.files[0].path), ".plist")
	got := parsePlist(t, s.files[0].content)
	if got["Label"] != base {
		t.Errorf("Label %q does not match filename %q — launchd will reject it", got["Label"], base)
	}
}

func TestLaunchdAgentRunsTickEveryMinute(t *testing.T) {
	s, _ := schedulerFor("darwin", testEnv())
	got := parsePlist(t, s.files[0].content)

	args, ok := got["ProgramArguments"].([]any)
	if !ok || len(args) != 2 {
		t.Fatalf("ProgramArguments = %#v, want [<exe> tick]", got["ProgramArguments"])
	}
	if args[0] != testEnv().exe || args[1] != "tick" {
		t.Errorf("ProgramArguments = %#v, want [%q tick]", args, testEnv().exe)
	}
	if got["StartInterval"] != "60" {
		t.Errorf("StartInterval = %v, want 60", got["StartInterval"])
	}
	if got["RunAtLoad"] != true {
		t.Errorf("RunAtLoad = %v, want true — otherwise nothing happens until the first interval elapses", got["RunAtLoad"])
	}
}

// TestLaunchdAgentAbandonsItsProcessGroup guards the macOS counterpart of a
// bug that already happened for real on Linux. tick spawns each due job's
// run-exec as a detached grandchild and exits immediately; launchd's default
// is to SIGKILL everything left in the job's process group at that moment, so
// without this key every run dies before it does anything — and the only
// symptom is jobs that quietly never produce output.
func TestLaunchdAgentAbandonsItsProcessGroup(t *testing.T) {
	s, _ := schedulerFor("darwin", testEnv())
	if got := parsePlist(t, s.files[0].content); got["AbandonProcessGroup"] != true {
		t.Errorf("AbandonProcessGroup = %v, want true: without it launchd kills every "+
			"run-exec the moment tick exits", got["AbandonProcessGroup"])
	}
}

// TestLaunchdAgentCarriesAUsablePath is the other silent killer: a LaunchAgent
// starts with PATH=/usr/bin:/bin:/usr/sbin:/sbin, so claude, opencode, codex
// and anything Homebrew installed are all unresolvable.
func TestLaunchdAgentCarriesAUsablePath(t *testing.T) {
	s, _ := schedulerFor("darwin", testEnv())
	got := parsePlist(t, s.files[0].content)

	envVars, ok := got["EnvironmentVariables"].(map[string]any)
	if !ok {
		t.Fatalf("EnvironmentVariables missing or not a dict: %#v", got["EnvironmentVariables"])
	}
	path, _ := envVars["PATH"].(string)
	if path == "" {
		t.Fatal("EnvironmentVariables has no PATH — agent jobs would not find their CLI")
	}
	if !strings.Contains(path, "/opt/homebrew/bin") {
		t.Errorf("PATH %q has no Homebrew directory", path)
	}
}

func TestSchedulerPathAddsTheUsualBinDirsWithoutDuplicating(t *testing.T) {
	home := "/Users/dani"
	got := schedulerPath("/usr/bin:/bin", home)
	for _, want := range []string{"/opt/homebrew/bin", "/usr/local/bin", home + "/.local/bin"} {
		if !strings.Contains(got, want) {
			t.Errorf("schedulerPath() = %q, missing %q", got, want)
		}
	}
	// The caller's own PATH must come first: if they put a specific
	// toolchain ahead of Homebrew, jobs should see the same one.
	if !strings.HasPrefix(got, "/usr/bin:/bin:") {
		t.Errorf("schedulerPath() = %q, should start with the PATH it was given", got)
	}
	// And nothing already present should be appended a second time.
	already := schedulerPath("/opt/homebrew/bin:/usr/bin", home)
	if n := strings.Count(already, "/opt/homebrew/bin"); n != 1 {
		t.Errorf("schedulerPath() = %q repeats /opt/homebrew/bin %d times", already, n)
	}
	// An empty PATH must still yield something usable rather than a
	// leading empty element, which the shell reads as the cwd.
	if e := schedulerPath("", home); strings.HasPrefix(e, ":") {
		t.Errorf("schedulerPath(\"\") = %q starts with an empty element", e)
	}
}

func TestLaunchdEnableUsesTheUsersGUIDomain(t *testing.T) {
	s, _ := schedulerFor("darwin", testEnv())
	var all []string
	for _, step := range s.enable {
		all = append(all, strings.Join(step.argv, " "))
	}
	joined := strings.Join(all, "\n")
	if !strings.Contains(joined, "launchctl bootstrap gui/501 ") {
		t.Errorf("no bootstrap into the user's gui domain:\n%s", joined)
	}
	// Re-running the command must replace a previously loaded copy, or a
	// moved binary or changed PATH would never take effect.
	if !strings.Contains(joined, "launchctl bootout gui/501/"+launchdLabel) {
		t.Errorf("enable never unloads a previous copy:\n%s", joined)
	}
	for _, step := range s.enable {
		if step.argv[1] == "bootout" && !step.ignoreFail {
			t.Error("bootout must tolerate failure: nothing is loaded on a first install")
		}
	}
}

func TestLaunchdPlistEscapesPaths(t *testing.T) {
	env := testEnv()
	// Mac home directories really can contain these, and an unescaped one
	// would produce a plist launchd silently refuses to load.
	env.exe = "/Users/a & b/<tools>/jobtail"
	env.home = "/Users/a & b"
	s, _ := schedulerFor("darwin", env)

	got := parsePlist(t, s.files[0].content) // fails loudly if escaping is wrong
	args := got["ProgramArguments"].([]any)
	if args[0] != env.exe {
		t.Errorf("exe survived escaping as %q, want %q", args[0], env.exe)
	}
}

// TestLaunchdPlistPassesPlutil uses Apple's own parser when the test happens
// to run on a Mac: a plist can be well-formed XML and still not be a valid
// plist. It's the counterpart of the `systemd-analyze verify` check the e2e
// suite runs against the systemd units, and it skips everywhere else.
func TestLaunchdPlistPassesPlutil(t *testing.T) {
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("plutil not available (not a Mac); the parsed-structure tests still cover this")
	}
	s, _ := schedulerFor("darwin", testEnv())
	f := filepath.Join(t.TempDir(), launchdLabel+".plist")
	if err := os.WriteFile(f, []byte(s.files[0].content), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-lint", f).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint rejected the agent: %v\n%s", err, out)
	}
}

// --- systemd -------------------------------------------------------------

func TestSystemdBackendIsUnchanged(t *testing.T) {
	env := testEnv()
	env.home = "/home/jarvis"
	env.exe = "/home/jarvis/.local/bin/jobtail"
	s, err := schedulerFor("linux", env)
	if err != nil {
		t.Fatal(err)
	}

	byPath := map[string]string{}
	for _, f := range s.files {
		byPath[f.path] = f.content
	}
	svc := "/home/jarvis/.config/systemd/user/jobtail-tick.service"
	timer := "/home/jarvis/.config/systemd/user/jobtail-tick.timer"
	if _, ok := byPath[svc]; !ok {
		t.Fatalf("no service unit; wrote %v", byPath)
	}
	if !strings.Contains(byPath[timer], "OnCalendar=minutely") {
		t.Errorf("timer unit missing OnCalendar=minutely:\n%s", byPath[timer])
	}
	// Same hazard as AbandonProcessGroup on macOS, and this one was
	// observed live: jobs stuck "running" forever with no log file.
	if !strings.Contains(byPath[svc], "KillMode=process") {
		t.Errorf("service unit missing KillMode=process:\n%s", byPath[svc])
	}
	if !strings.Contains(byPath[svc], env.exe+" tick") {
		t.Errorf("service unit doesn't run this binary:\n%s", byPath[svc])
	}
}

// --- everything else -----------------------------------------------------

func TestUnsupportedPlatformSaysWhatToDoInstead(t *testing.T) {
	_, err := schedulerFor("windows", testEnv())
	if err == nil {
		t.Fatal("expected an error for an unsupported platform")
	}
	// Leaving someone with "unsupported" and nothing else is the failure
	// mode worth avoiding: tick is a plain command, and cron can run it.
	if !strings.Contains(err.Error(), "tick") || !strings.Contains(err.Error(), "* * * * *") {
		t.Errorf("error should show the cron line that works instead, got: %v", err)
	}
}

func TestBothBackendsBuildOnAnyHost(t *testing.T) {
	// The point of describing backends as data: the Mac one is
	// constructible from Linux and vice versa, so neither can rot
	// unnoticed just because CI runs on the other.
	for _, goos := range []string{"linux", "darwin"} {
		s, err := schedulerFor(goos, testEnv())
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		if len(s.files) == 0 || s.unit == "" || len(s.enable) == 0 || s.status == "" {
			t.Errorf("%s: incomplete backend: %+v", goos, s)
		}
		for _, f := range s.files {
			if !filepath.IsAbs(f.path) {
				t.Errorf("%s: %q is not an absolute path", goos, f.path)
			}
			if strings.TrimSpace(f.content) == "" {
				t.Errorf("%s: %q would be written empty", goos, f.path)
			}
		}
	}
	t.Logf("host is %s/%s; both backends built here", runtime.GOOS, runtime.GOARCH)
}

// TestCustomDataDirReachesTheScheduledTick covers a failure that looks like
// jobtail being broken rather than misconfigured: with JOBTAIL_DATA_DIR set
// in a shell, the dashboard shows one database while a scheduled tick — which
// inherits nothing from that shell — operates on the default one. Jobs sit
// there listed and never fire, and nothing says why.
func TestCustomDataDirReachesTheScheduledTick(t *testing.T) {
	env := testEnv()
	env.dataDirEnv = "/Volumes/work/jobtail-data"

	mac, _ := schedulerFor("darwin", env)
	got := parsePlist(t, mac.files[0].content)
	vars, ok := got["EnvironmentVariables"].(map[string]any)
	if !ok || vars["JOBTAIL_DATA_DIR"] != env.dataDirEnv {
		t.Errorf("launchd agent did not carry JOBTAIL_DATA_DIR: %#v", got["EnvironmentVariables"])
	}

	lin, _ := schedulerFor("linux", env)
	var svc string
	for _, f := range lin.files {
		if strings.HasSuffix(f.path, ".service") {
			svc = f.content
		}
	}
	if !strings.Contains(svc, "Environment=JOBTAIL_DATA_DIR="+env.dataDirEnv) {
		t.Errorf("systemd unit did not carry JOBTAIL_DATA_DIR:\n%s", svc)
	}
}

// TestUnsetDataDirAddsNothing keeps the common case byte-identical to what
// jobtail has always written, so upgrading doesn't rewrite a working unit.
func TestUnsetDataDirAddsNothing(t *testing.T) {
	env := testEnv() // dataDirEnv empty
	lin, _ := schedulerFor("linux", env)
	for _, f := range lin.files {
		if strings.Contains(f.content, "Environment=") {
			t.Errorf("no JOBTAIL_DATA_DIR was set, but %s has an Environment= line:\n%s", f.path, f.content)
		}
	}
	mac, _ := schedulerFor("darwin", env)
	got := parsePlist(t, mac.files[0].content)
	vars := got["EnvironmentVariables"].(map[string]any)
	if _, present := vars["JOBTAIL_DATA_DIR"]; present {
		t.Error("JOBTAIL_DATA_DIR was not set but appears in the agent anyway")
	}
	if len(vars) != 1 {
		t.Errorf("expected only PATH in EnvironmentVariables, got %#v", vars)
	}
}
