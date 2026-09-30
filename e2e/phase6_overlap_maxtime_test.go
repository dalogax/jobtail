// Overlap policy and max time: a job that never finishes must not be able
// to block the job forever, whether it hangs or the process executing it
// dies.
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestShowReportsOverlapAndMaxTimeDefaults(t *testing.T) {
	e := newEnv(t)
	e.run("add", "plain", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", t.TempDir(), "--cmd", "true")
	out := e.run("show", "plain")
	if !strings.Contains(out, "allow-overlap:   false") || !strings.Contains(out, "max-time:        30m0s (default)") {
		t.Fatalf("show should report the defaults, got:\n%s", out)
	}

	e.run("edit", "plain", "--allow-overlap", "--max-time", "2h")
	out = e.run("show", "plain")
	if !strings.Contains(out, "allow-overlap:   true") || !strings.Contains(out, "max-time:        2h0m0s\n") {
		t.Fatalf("show should reflect the edit, got:\n%s", out)
	}

	if _, err := e.runAllowFail("edit", "plain", "--max-time", "0s"); err == nil {
		t.Fatal("--max-time 0s should be refused: runs can't be unbounded")
	}
}

// The flags this replaces keep working, translated.
func TestDeprecatedFlagsStillWork(t *testing.T) {
	e := newEnv(t)
	e.run("add", "old", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", t.TempDir(), "--cmd", "true",
		"--max-concurrent", "2", "--timeout-seconds", "90")
	out := e.run("show", "old")
	if !strings.Contains(out, "allow-overlap:   true") || !strings.Contains(out, "max-time:        1m30s") {
		t.Fatalf("deprecated flags weren't translated, got:\n%s", out)
	}
}

func TestAllowOverlapRunsConcurrently(t *testing.T) {
	e := newEnv(t)
	e.run("add", "stack", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", t.TempDir(), "--cmd", "sleep 1",
		"--allow-overlap")

	cmd1, buf1 := e.start("run", "stack")
	time.Sleep(300 * time.Millisecond)
	cmd2, buf2 := e.start("run", "stack")
	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func() { defer wg.Done(); err1 = cmd1.Wait() }()
	go func() { defer wg.Done(); err2 = cmd2.Wait() }()
	wg.Wait()
	if err1 != nil || err2 != nil {
		t.Fatalf("both runs should succeed:\n%v\n%s\n%v\n%s", err1, buf1, err2, buf2)
	}
	for _, r := range mustUnmarshalRuns(t, e.run("runs", "stack", "--json")) {
		if r.Status != "ok" {
			t.Fatalf("want every run ok with --allow-overlap, got %+v", r)
		}
	}
}

func TestMaxTimeKillsARun(t *testing.T) {
	e := newEnv(t)
	e.run("add", "hang", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", t.TempDir(), "--cmd", "sleep 30",
		"--max-time", "1s")

	start := time.Now()
	out, err := e.runAllowFail("run", "hang")
	if err == nil {
		t.Fatalf("a timed-out run should exit non-zero, got:\n%s", out)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("a 1s max time took %s to stop the run", took)
	}
	if !strings.Contains(out, "exceeded its max time of 1s") {
		t.Fatalf("output should say why the run was killed, got:\n%s", out)
	}
	runs := mustUnmarshalRuns(t, e.run("runs", "hang", "--json"))
	if len(runs) != 1 || runs[0].Status != "timeout" {
		t.Fatalf("want one timeout run, got %+v", runs)
	}
}

// The failure this whole feature is for: the jobtail process executing a
// run is killed outright, the run's row says "running" forever, and every
// later run is skipped as an overlap. The next run has to reap it, kill
// what the dead process left behind, and go ahead.
func TestRunWhoseExecutorDiedDoesNotBlockTheJob(t *testing.T) {
	e := newEnv(t)
	marker := filepath.Join(t.TempDir(), "first-run-happened")
	// First run hangs; any later run finishes at once.
	e.run("add", "fragile", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", t.TempDir(),
		"--cmd", "if [ -f "+marker+" ]; then exit 0; fi; touch "+marker+"; sleep 30")

	first, _ := e.start("run", "fragile")
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs := mustUnmarshalRuns(t, e.run("runs", "fragile", "--json"))
		if len(runs) == 1 && runs[0].Status == "running" && fileExists(marker) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first run never got going: %+v", runs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = first.Process.Signal(syscall.SIGKILL)
	_ = first.Wait()

	out := e.run("run", "fragile")
	if !strings.Contains(out, "reaped fragile run") {
		t.Fatalf("second run should report reaping the first, got:\n%s", out)
	}
	runs := mustUnmarshalRuns(t, e.run("runs", "fragile", "--json"))
	statuses := map[string]int{}
	for _, r := range runs {
		statuses[r.Status]++
	}
	if statuses["failed"] != 1 || statuses["ok"] != 1 || len(runs) != 2 {
		t.Fatalf("want the dead run failed and the new one ok, got %+v", runs)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
