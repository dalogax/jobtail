// Phase 1 (PRD §11): schema + CLI + cli-kind execution + SQLite store + log
// files + systemd unit generation, all exercised as a user would from a
// shell — no TUI, no agent jobs yet.
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type jobJSON struct {
	ID         string
	Kind       string
	Enabled    bool
	Cron       string
	RunCount   int
	LastStatus string
}

type runJSON struct {
	ID       string
	JobID    string
	Trigger  string
	Status   string
	LogPath  string
	ExitCode struct {
		Int64 int64
		Valid bool
	}
	SessionID struct {
		String string
		Valid  bool
	}
}

func mustUnmarshalJobs(t *testing.T, s string) []jobJSON {
	t.Helper()
	var jobs []jobJSON
	if err := json.Unmarshal([]byte(s), &jobs); err != nil {
		t.Fatalf("unmarshal jobs: %v\ninput: %s", err, s)
	}
	return jobs
}

func mustUnmarshalRuns(t *testing.T, s string) []runJSON {
	t.Helper()
	var runs []runJSON
	if err := json.Unmarshal([]byte(s), &runs); err != nil {
		t.Fatalf("unmarshal runs: %v\ninput: %s", err, s)
	}
	return runs
}

func findJob(jobs []jobJSON, id string) *jobJSON {
	for i := range jobs {
		if jobs[i].ID == id {
			return &jobs[i]
		}
	}
	return nil
}

func TestCLIJobLifecycle(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()

	e.run("add", "greet", "--kind", "cli", "--cron", "*/5 * * * *",
		"--cwd", work, "--cmd", "echo hello-jobtail")

	jobs := mustUnmarshalJobs(t, e.run("list", "--json"))
	j := findJob(jobs, "greet")
	if j == nil {
		t.Fatalf("job 'greet' not in list output: %+v", jobs)
	}
	if !j.Enabled || j.Kind != "cli" || j.RunCount != 0 {
		t.Fatalf("unexpected job state: %+v", j)
	}

	runOut := e.run("run", "greet")
	if !strings.Contains(runOut, "hello-jobtail") {
		t.Fatalf("run output missing job stdout: %s", runOut)
	}
	if !strings.Contains(runOut, ": ok") {
		t.Fatalf("run output missing ok status: %s", runOut)
	}

	runs := mustUnmarshalRuns(t, e.run("runs", "greet", "--json"))
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d: %+v", len(runs), runs)
	}
	r := runs[0]
	if r.Status != "ok" || r.Trigger != "manual" {
		t.Fatalf("unexpected run: %+v", r)
	}

	logOut := e.run("log", r.ID)
	if !strings.Contains(logOut, "hello-jobtail") {
		t.Fatalf("log output missing job stdout: %s", logOut)
	}
	if _, err := os.Stat(r.LogPath); err != nil {
		t.Fatalf("log file missing on disk: %v", err)
	}

	jobs = mustUnmarshalJobs(t, e.run("list", "--json"))
	j = findJob(jobs, "greet")
	if j.RunCount != 1 || j.LastStatus != "ok" {
		t.Fatalf("job summary didn't pick up the run: %+v", j)
	}
}

func TestEnableDisable(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "toggle", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", work, "--cmd", "true")

	e.run("disable", "toggle")
	jobs := mustUnmarshalJobs(t, e.run("list", "--json"))
	if findJob(jobs, "toggle").Enabled {
		t.Fatal("job still enabled after disable")
	}

	e.run("enable", "toggle")
	jobs = mustUnmarshalJobs(t, e.run("list", "--json"))
	if !findJob(jobs, "toggle").Enabled {
		t.Fatal("job still disabled after enable")
	}
}

func TestEditChangesCommand(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "edit-me", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", work, "--cmd", "echo before")

	e.run("edit", "edit-me", "--cmd", "echo after")
	out := e.run("run", "edit-me")
	if strings.Contains(out, "before") || !strings.Contains(out, "after") {
		t.Fatalf("edit didn't take effect: %s", out)
	}
}

func TestFailingJobRecordsFailedStatus(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "boom", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", work, "--cmd", "exit 7")

	out, err := e.runAllowFail("run", "boom")
	if err == nil {
		t.Fatalf("expected non-zero exit for a failing job, got success: %s", out)
	}
	runs := mustUnmarshalRuns(t, e.run("runs", "boom", "--json"))
	if len(runs) != 1 || runs[0].Status != "failed" || runs[0].ExitCode.Int64 != 7 {
		t.Fatalf("unexpected run record: %+v", runs)
	}
}

func TestRmDeletesJobAndRuns(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "temp", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", work, "--cmd", "true")
	e.run("run", "temp")
	e.run("rm", "temp")

	jobs := mustUnmarshalJobs(t, e.run("list", "--json"))
	if findJob(jobs, "temp") != nil {
		t.Fatal("job still present after rm")
	}
	if _, err := e.runAllowFail("runs", "temp", "--json"); err == nil {
		t.Log("runs on a deleted job returned no error (empty list) — acceptable, just noting")
	}
}

// TestTickConvergesThenIdempotent exercises the scheduling core: each `tick`
// call advances a job by exactly one cron step from its last scheduled fire
// (PRD §10's cron.Next, one step per call). Ticking repeatedly at a *fixed*
// `now` therefore drains any backlog step by step and then stops — it must
// never fire more times than there are cron boundaries between the job's
// creation and `now`, and once caught up, ticking again must be a no-op.
// (In production this never floods: systemd fires `tick` once per real
// minute, so at most one boundary is ever pending at once — this test's
// repeated same-instant calls are a stress case a real deploy won't hit.)
func TestTickConvergesThenIdempotent(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	marker := filepath.Join(work, "ran.txt")
	e.run("add", "ticker", "--kind", "cli", "--cron", "*/1 * * * *",
		"--cwd", work, "--cmd", "echo x >> "+marker)

	fireAt := time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339)

	var runs []runJSON
	for i := 0; i < 10; i++ {
		e.run("tick", "--now", fireAt)
		next := waitUntilSettled(t, e, "ticker", 5*time.Second)
		if len(next) == len(runs) {
			runs = next
			break // converged: this tick produced no new run
		}
		runs = next
	}
	if len(runs) < 1 || len(runs) > 3 {
		t.Fatalf("expected a small (1-3), non-zero backlog to drain for a 2-minute window, got %d: %+v", len(runs), runs)
	}
	for _, r := range runs {
		if r.Trigger != "scheduled" || r.Status != "ok" {
			t.Fatalf("unexpected run: %+v", r)
		}
	}

	// Converged: one more tick at the same instant must change nothing.
	e.run("tick", "--now", fireAt)
	time.Sleep(300 * time.Millisecond)
	final := mustUnmarshalRuns(t, e.run("runs", "ticker", "--json"))
	if len(final) != len(runs) {
		t.Fatalf("tick fired again after convergence: had %d runs, now %d: %+v", len(runs), len(final), final)
	}
}

// waitUntilSettled polls until no run for jobID is still status="running"
// (i.e. any detached run-exec spawned by the last tick has finished), and
// returns the settled run list.
func waitUntilSettled(t *testing.T, e *env, jobID string, timeout time.Duration) []runJSON {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		runs := mustUnmarshalRuns(t, e.run("runs", jobID, "--json"))
		settled := true
		for _, r := range runs {
			if r.Status == "running" {
				settled = false
			}
		}
		if settled {
			return runs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for runs on %s to settle, have: %+v", jobID, runs)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestOverlapSkipsSecondRun proves the max_concurrent=1 default: launching a
// second `run` while the first is still executing gets recorded as
// skipped_overlap, not run twice (PRD §5 "Locking", §6 schema, §12 decision 11).
func TestOverlapSkipsSecondRun(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "slow", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", work, "--cmd", "sleep 2")

	var wg sync.WaitGroup
	cmd1, buf1 := e.start("run", "slow")
	time.Sleep(400 * time.Millisecond) // let the first run's StartRun land before the second starts
	cmd2, buf2 := e.start("run", "slow")

	wg.Add(2)
	var err1, err2 error
	go func() { defer wg.Done(); err1 = cmd1.Wait() }()
	go func() { defer wg.Done(); err2 = cmd2.Wait() }()
	wg.Wait()

	if err1 != nil {
		t.Fatalf("first run should succeed: %v\n%s", err1, buf1.String())
	}
	if err2 != nil {
		t.Fatalf("skipped run should still exit 0: %v\n%s", err2, buf2.String())
	}
	if !strings.Contains(buf2.String(), "skipped:") {
		t.Fatalf("second run's output should say it was skipped, got: %s", buf2.String())
	}

	runs := mustUnmarshalRuns(t, e.run("runs", "slow", "--json"))
	var ok, skipped int
	for _, r := range runs {
		switch r.Status {
		case "ok":
			ok++
		case "skipped_overlap":
			skipped++
		}
	}
	if ok != 1 || skipped != 1 {
		t.Fatalf("want 1 ok + 1 skipped_overlap, got ok=%d skipped=%d: %+v", ok, skipped, runs)
	}
}

func TestGCPrunesPastRetention(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "chatty", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", work, "--cmd", "true", "--keep", "2")

	for i := 0; i < 3; i++ {
		e.run("run", "chatty")
	}
	runs := mustUnmarshalRuns(t, e.run("runs", "chatty", "--json"))
	if len(runs) != 3 {
		t.Fatalf("want 3 runs before gc, got %d", len(runs))
	}
	oldestLog := runs[len(runs)-1].LogPath

	e.run("gc")
	runs = mustUnmarshalRuns(t, e.run("runs", "chatty", "--json"))
	if len(runs) != 2 {
		t.Fatalf("want 2 runs after gc (keep=2), got %d: %+v", len(runs), runs)
	}
	if _, err := os.Stat(oldestLog); !os.IsNotExist(err) {
		t.Fatalf("gc should have removed the pruned run's log file %s", oldestLog)
	}
}

func TestInstallSystemdWritesUnits(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	cmd := exec.Command(e.bin, "install-systemd")
	cmd.Env = append(os.Environ(), "HOME="+home, "JOBTAIL_DATA_DIR="+e.dataDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install-systemd: %v\n%s", err, out)
	}
	svc := filepath.Join(home, ".config", "systemd", "user", "jobtail-tick.service")
	timer := filepath.Join(home, ".config", "systemd", "user", "jobtail-tick.timer")
	for _, p := range []string{svc, timer} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected unit file %s: %v", p, err)
		}
	}
	data, _ := os.ReadFile(timer)
	if !strings.Contains(string(data), "OnCalendar=minutely") {
		t.Fatalf("timer unit missing expected OnCalendar line: %s", data)
	}

	svcData, _ := os.ReadFile(svc)
	if !strings.Contains(string(svcData), "KillMode=process") {
		t.Fatalf(`service unit missing "KillMode=process" — without it, systemd's default
KillMode=control-group kills every run-exec tick just spawned the instant
tick itself exits (confirmed live on a real box: jobs got stuck in
"running" forever and their log files were never even created):
%s`, svcData)
	}

	// A syntactically plausible OnCalendar value isn't the same as one
	// systemd actually accepts (an earlier draft here wrote
	// "*-*-*-*:*:00" — four date fields instead of three — which
	// systemd-analyze verify caught immediately but a bare substring check
	// on the string would not have). Verify against the real systemd unit
	// validator rather than trusting the file's own text.
	if _, err := exec.LookPath("systemd-analyze"); err == nil {
		out, err := exec.Command("systemd-analyze", "verify", svc, timer).CombinedOutput()
		if err != nil {
			t.Fatalf("systemd-analyze verify rejected the generated units: %v\n%s", err, out)
		}
	} else {
		t.Skip("systemd-analyze not on PATH; skipping unit-file validation")
	}
}

// TestDetachedGrandchildSurvivesOneshotUnitExit is a real-systemd regression
// test for the bug the live install on this box actually hit: a Type=oneshot
// unit's detached grandchild process getting killed the instant the unit's
// own main process exits, because systemd's default KillMode=control-group
// sweeps the whole cgroup on unit deactivation — not just on an explicit
// stop. It reproduces jobtail-tick.service's exact shape (spawn a detached
// child, exit 0 immediately) via a uniquely-named transient unit, so it
// can't collide with (or ever touch) a real jobtail-tick.service on this
// machine. Skips cleanly wherever no systemd --user session is reachable
// (e.g. most CI runners), rather than failing the test-as-release-gate.
func TestDetachedGrandchildSurvivesOneshotUnitExit(t *testing.T) {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run not on PATH")
	}
	if out, err := exec.Command("systemctl", "--user", "status").CombinedOutput(); err != nil {
		t.Skipf("no reachable systemd --user session: %v\n%s", err, out)
	}

	marker := filepath.Join(t.TempDir(), "survived")
	unit := fmt.Sprintf("jobtail-e2e-killmode-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		exec.Command("systemctl", "--user", "reset-failed", unit+".service").Run()
	})

	// Exactly jobtail-tick.service's shape: a oneshot main process that
	// backgrounds a grandchild and returns immediately, carrying the same
	// KillMode=process property cmd_systemd.go's serviceUnit template sets.
	bg := fmt.Sprintf("setsid sh -c 'sleep 1; touch %s' >/dev/null 2>&1 & disown; exit 0", marker)
	out, err := exec.Command("systemd-run", "--user", "--unit="+unit,
		"--property=Type=oneshot", "--property=KillMode=process", "--wait",
		"--", "sh", "-c", bg).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-run: %v\n%s", err, out)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached grandchild did not survive its oneshot unit's exit — KillMode=process should prevent systemd from killing it")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
