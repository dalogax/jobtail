package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dalogax/jobtail/internal/execengine"
	"github.com/dalogax/jobtail/internal/store"
)

func limitedJob(t *testing.T, id, command string, maxTimeSeconds int64) store.Job {
	return store.Job{
		ID: id, Kind: "cli", Cron: "0 0 * * *", Timezone: "local", Enabled: true,
		Cwd: t.TempDir(), Command: command, Keep: 200, MaxTimeSeconds: maxTimeSeconds,
	}
}

func waitGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !execengine.ProcessAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// A run past its max time must be killed — and so must anything it
// spawned, not just the shell. The grandchild here is exactly what used to
// survive: a background process holding on after its `sh -c` parent was
// signalled.
func TestMaxTimeKillsTheWholeProcessGroup(t *testing.T) {
	st := testStore(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	j := limitedJob(t, "slow", "sleep 30 & echo $! > "+pidFile+"; wait", 1)
	if err := st.CreateJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "run.log")
	if _, err := st.StartRun(context.Background(), j.ID, "r1", "manual", logPath, time.Now()); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	res, err := Execute(context.Background(), st, j, "r1", logPath)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("a 1s max time let the run go on for %s", took)
	}
	if res.Status != "timeout" {
		t.Fatalf("want status timeout, got %+v", res)
	}
	data, _ := os.ReadFile(pidFile)
	gpid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("grandchild pid file: %q", data)
	}
	if !waitGone(gpid, 7*time.Second) {
		t.Fatalf("grandchild %d outlived its run's max time", gpid)
	}
	log, _ := os.ReadFile(logPath)
	if !strings.Contains(string(log), "exceeded its max time of 1s") {
		t.Errorf("log should say why the run was killed, got: %s", log)
	}
}

// The limit covers the precheck too: a gate that hangs is still the run
// hanging, and is reported as the run's timeout rather than a gate failure.
func TestMaxTimeCoversThePrecheck(t *testing.T) {
	st := testStore(t)
	j := limitedJob(t, "gated", "echo never", 1)
	j.Precheck = "sleep 30"
	res, _ := Execute(context.Background(), st, j, "r1", filepath.Join(t.TempDir(), "run.log"))
	if res.Status != "timeout" {
		t.Fatalf("want timeout, got %+v", res)
	}
}

func TestJobsDefaultToThirtyMinutes(t *testing.T) {
	if got := (store.Job{}).MaxTime(); got != 30*time.Minute {
		t.Fatalf("default max time = %s, want 30m", got)
	}
}

// deadPID returns the pid of a process that has already exited and been
// reaped, standing in for a run-exec that crashed.
func deadPID(t *testing.T) int {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func runStatus(t *testing.T, st *store.Store, id string) string {
	r, err := st.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r.Status
}

// This is the bug the reaper exists for: the process executing a run dies
// without recording an outcome, the row says "running" forever, and every
// later run of the job is skipped as an overlap.
func TestReapFreesAJobWhoseRunnerDied(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	j := limitedJob(t, "orphaned", "true", 0)
	if err := st.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "r1.log")
	if _, err := st.StartRun(ctx, j.ID, "r1", "scheduled", logPath, time.Now()); err != nil {
		t.Fatal(err)
	}

	// The job's own process, orphaned and still going.
	orphan := exec.Command("sleep", "30")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = orphan.Wait() }()
	if err := st.MarkRunExecuting(ctx, "r1", deadPID(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunChildPID(ctx, "r1", orphan.Process.Pid); err != nil {
		t.Fatal(err)
	}

	if _, err := st.StartRun(ctx, j.ID, "r2", "manual", "/dev/null", time.Now()); err != store.ErrOverlap {
		t.Fatalf("precondition: the stuck run should block the next one, got %v", err)
	}

	reaped, err := ReapStale(ctx, st, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].RunID != "r1" || reaped[0].Status != "failed" {
		t.Fatalf("want r1 reaped as failed, got %+v", reaped)
	}
	if !waitGone(orphan.Process.Pid, 7*time.Second) {
		t.Fatal("the orphaned job process was not killed")
	}
	if _, err := st.StartRun(ctx, j.ID, "r3", "manual", "/dev/null", time.Now()); err != nil {
		t.Fatalf("after reaping, the job should run again: %v", err)
	}
	log, _ := os.ReadFile(logPath)
	if !strings.Contains(string(log), "[jobtail] reaped:") {
		t.Errorf("reaped run's log should say so, got: %q", log)
	}
}

// A run with a live executor is left alone until it is past its max time
// plus the grace period — then it is a timeout, whoever is executing it.
func TestReapOverdueRuns(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	j := limitedJob(t, "overdue", "true", 60)
	if err := st.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if _, err := st.StartRun(ctx, j.ID, "r1", "manual", filepath.Join(t.TempDir(), "r1.log"), began); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRunExecuting(ctx, "r1", os.Getpid(), began); err != nil {
		t.Fatal(err)
	}

	if reaped, _ := ReapStale(ctx, st, began.Add(time.Minute+ReapGrace-time.Second)); len(reaped) != 0 {
		t.Fatalf("reaped a run still within its max time + grace: %+v", reaped)
	}
	if got := runStatus(t, st, "r1"); got != "running" {
		t.Fatalf("status = %s, want running", got)
	}
	reaped, err := ReapStale(ctx, st, began.Add(time.Minute+ReapGrace+time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].Status != "timeout" {
		t.Fatalf("want one timeout, got %+v", reaped)
	}
	if got := runStatus(t, st, "r1"); got != "timeout" {
		t.Fatalf("status = %s, want timeout", got)
	}

	// The executor finishing late must not overwrite the reaper's verdict.
	if err := st.FinishRun(ctx, "r1", "ok", 0, time.Now(), 1); err != nil {
		t.Fatalf("late FinishRun: %v", err)
	}
	if got := runStatus(t, st, "r1"); got != "timeout" {
		t.Fatalf("late FinishRun overwrote the reaper: status = %s", got)
	}
}

// Rows left "running" by jobtail versions that recorded no executor at all
// must still be cleared once they are overdue — they are what is blocking
// jobs on existing installs.
func TestReapLegacyRunWithoutPIDs(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if err := st.CreateJob(ctx, limitedJob(t, "legacy", "true", 0)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if _, err := st.StartRun(ctx, "legacy", "r1", "scheduled", filepath.Join(t.TempDir(), "r1.log"), old); err != nil {
		t.Fatal(err)
	}
	reaped, err := ReapStale(ctx, st, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].Status != "timeout" {
		t.Fatalf("want the legacy run reaped as timeout, got %+v", reaped)
	}
	r, _ := st.GetRun(ctx, "r1")
	if !r.DurationMs.Valid {
		t.Errorf("reaped run should have a duration, got %+v", r.DurationMs)
	}
}
