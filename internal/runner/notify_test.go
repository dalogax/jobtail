package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dalogax/jobtail/internal/store"
)

// fakeHerdr puts a herdr on PATH that records each call's arguments, one
// line per notification, and returns a function reading them back.
func fakeHerdr(t *testing.T) func() []string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + calls + "\n"
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("JOBTAIL_DISABLE_NOTIFY", "")
	return func() []string {
		data, _ := os.ReadFile(calls)
		s := strings.TrimSpace(string(data))
		if s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}
}

func TestParseNotify(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"default":                "",
		"none":                   "none",
		"all":                    "started,ok,failed,timeout,skipped,skipped_overlap",
		"failed, started":        "started,failed", // canonical order, spaces ignored
		"timeout,timeout":        "timeout",
		"skipped_overlap,failed": "failed,skipped_overlap",
	} {
		got, err := ParseNotify(in)
		if err != nil || got != want {
			t.Errorf("ParseNotify(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"fail", "started,bogus", ","} {
		if _, err := ParseNotify(bad); err == nil {
			t.Errorf("ParseNotify(%q) should fail", bad)
		}
	}
}

// Execute + Finish for a job, returning the notifications raised.
func runAndNotify(t *testing.T, j store.Job) []string {
	t.Helper()
	calls := fakeHerdr(t)
	st := testStore(t)
	ctx := context.Background()
	if err := st.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "run.log")
	if _, err := st.StartRun(ctx, j.ID, "r1", "manual", logPath, time.Now()); err != nil {
		t.Fatal(err)
	}
	res, err := Execute(ctx, st, j, "r1", logPath)
	_ = Finish(ctx, st, j, "r1", res, err)
	return calls()
}

func notifyJob(t *testing.T, command, precheck, notify string) store.Job {
	return store.Job{
		ID: "n", Kind: "cli", Cron: "0 0 * * *", Timezone: "local", Enabled: true, Cwd: t.TempDir(),
		Command: command, Precheck: precheck, Notify: notify, MaxConcurrent: 1, Keep: 200,
	}
}

// The case that motivated this: know when a gated job actually found work.
func TestStartedFiresOnlyWhenThePrecheckPasses(t *testing.T) {
	got := runAndNotify(t, notifyJob(t, "true", "echo 2 new issues", "started"))
	if len(got) != 1 || !strings.Contains(got[0], "jobtail: n started") || !strings.Contains(got[0], "precheck passed") {
		t.Fatalf("want one started notification mentioning the precheck, got %q", got)
	}

	got = runAndNotify(t, notifyJob(t, "true", "exit 1", "started"))
	if len(got) != 0 {
		t.Fatalf("a skipped run must not notify started, got %q", got)
	}
}

func TestSkippedIsItsOwnEvent(t *testing.T) {
	got := runAndNotify(t, notifyJob(t, "true", "exit 1", "skipped"))
	if len(got) != 1 || !strings.Contains(got[0], "jobtail: n skipped") || !strings.Contains(got[0], "--sound none") {
		t.Fatalf("want one quiet skipped notification, got %q", got)
	}
}

// With no setting, a job behaves as before: failures only.
func TestDefaultNotifiesFailuresOnly(t *testing.T) {
	if got := runAndNotify(t, notifyJob(t, "true", "", "")); len(got) != 0 {
		t.Fatalf("a successful run notified by default: %q", got)
	}
	got := runAndNotify(t, notifyJob(t, "exit 3", "", ""))
	if len(got) != 1 || !strings.Contains(got[0], "jobtail: n failed") || !strings.Contains(got[0], "exit 3") ||
		!strings.Contains(got[0], "--sound request") {
		t.Fatalf("want one failed notification with the exit code, got %q", got)
	}
}

func TestNoneSilencesEvenFailures(t *testing.T) {
	if got := runAndNotify(t, notifyJob(t, "exit 3", "", "none")); len(got) != 0 {
		t.Fatalf("notify=none still notified: %q", got)
	}
}

func TestAllNotifiesStartAndFinish(t *testing.T) {
	got := runAndNotify(t, notifyJob(t, "true", "", "all"))
	if len(got) != 2 || !strings.Contains(got[0], "n started") || !strings.Contains(got[1], "n ok") ||
		!strings.Contains(got[1], "--sound done") {
		t.Fatalf("want started then ok, got %q", got)
	}
}
