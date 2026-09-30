// Timeouts and stuck runs: a run that never finishes must not be able to
// block its job forever, whether it hangs or the process executing it dies.
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestShowReportsTheDefaultTimeout(t *testing.T) {
	e := newEnv(t)
	e.run("add", "plain", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", t.TempDir(), "--cmd", "true")
	if out := e.run("show", "plain"); !strings.Contains(out, "timeout:         30m0s (default)") {
		t.Fatalf("show should report the default timeout, got:\n%s", out)
	}
	e.run("edit", "plain", "--timeout-seconds", "7200")
	if out := e.run("show", "plain"); !strings.Contains(out, "timeout:         2h0m0s\n") {
		t.Fatalf("show should reflect the edit, got:\n%s", out)
	}
}

func TestTimeoutKillsARun(t *testing.T) {
	e := newEnv(t)
	e.run("add", "hang", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", t.TempDir(), "--cmd", "sleep 30",
		"--timeout-seconds", "1")

	start := time.Now()
	out, err := e.runAllowFail("run", "hang")
	if err == nil {
		t.Fatalf("a timed-out run should exit non-zero, got:\n%s", out)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("a 1s timeout took %s to stop the run", took)
	}
	if !strings.Contains(out, "exceeded its timeout of 1s") {
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
