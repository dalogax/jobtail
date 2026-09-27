package execengine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dalogax/jobtail/internal/store"
)

func gateJob(precheck string, timeoutSeconds int64) store.Job {
	return store.Job{
		ID: "gate", Kind: "agent", Cwd: "/tmp", Prompt: "do the thing",
		Precheck: precheck, PrecheckTimeoutSeconds: timeoutSeconds,
	}
}

// TestPrecheckTimeoutActuallyStopsTheGate is the regression test for a gate
// that reported a timeout it had not enforced. exec.CommandContext kills the
// process when the context is done, but only through the Cancel hook it
// installs — replacing that hook with one that merely reports the cause
// removes the kill, so the deadline fired, the result said TimedOut, and
// cmd.Run kept blocking until the gate finished by itself. Measured before
// the fix: a `sleep 10` gate with a 1s timeout took 10.003s and still
// claimed "precheck timed out after 1s".
//
// The assertion is on elapsed time, because the reported outcome was
// already correct while the behaviour was not.
func TestPrecheckTimeoutActuallyStopsTheGate(t *testing.T) {
	j := gateJob("sleep 30; echo should-never-print", 1)

	start := time.Now()
	res := RunPrecheck(context.Background(), j)
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("a 1s timeout let the gate run for %s: the deadline is not killing the process",
			elapsed.Round(time.Millisecond))
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false after a timeout; got %+v", res)
	}
	if res.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2 (gate failure) on timeout", res.ExitCode)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "timed out") {
		t.Errorf("Err = %v, want something saying it timed out", res.Err)
	}
	if strings.Contains(res.Output, "should-never-print") {
		t.Error("the gate ran to completion despite the timeout")
	}
}

// A gate with no timeout must still be allowed to finish normally — the fix
// above must not have turned every gate into a one-shot kill.
func TestPrecheckWithoutTimeoutRunsToCompletion(t *testing.T) {
	res := RunPrecheck(context.Background(), gateJob("sleep 0.2; echo done", 0))
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d (%v), want 0", res.ExitCode, res.Err)
	}
	if !strings.Contains(res.Output, "done") {
		t.Errorf("Output = %q, want it to contain the gate's output", res.Output)
	}
	if res.TimedOut {
		t.Error("TimedOut set on a gate that had no timeout")
	}
}

// A gate that finishes inside its timeout must not be reported as timing out.
func TestPrecheckInsideItsTimeoutIsNotATimeout(t *testing.T) {
	res := RunPrecheck(context.Background(), gateJob("echo quick", 30))
	if res.TimedOut || res.ExitCode != 0 {
		t.Fatalf("want a clean pass, got %+v", res)
	}
}

// TestPrecheckReportsItsOwnDuration covers the placeholder that used to sit
// here: a helper that took the result and returned zero regardless, so every
// skipped or gate-failed run showed 0ms in the runs table.
func TestPrecheckReportsItsOwnDuration(t *testing.T) {
	res := RunPrecheck(context.Background(), gateJob("sleep 0.25", 0))
	if res.Duration < 200*time.Millisecond {
		t.Errorf("Duration = %s, want roughly the 250ms the gate actually took", res.Duration)
	}
}

func TestPrecheckExitCodesAreReportedVerbatim(t *testing.T) {
	for _, code := range []int{1, 2, 7} {
		res := RunPrecheck(context.Background(), gateJob("exit "+string(rune('0'+code)), 0))
		if res.ExitCode != code {
			t.Errorf("gate exiting %d reported ExitCode %d", code, res.ExitCode)
		}
		if res.Err != nil {
			t.Errorf("a gate that exited %d is not an execution error, got Err=%v", code, res.Err)
		}
	}
}

// A gate whose working directory doesn't exist can't run at all, which is a
// gate failure rather than a job failure — and must not be mistaken for a
// clean exit 0.
func TestPrecheckThatCannotRunIsAGateFailure(t *testing.T) {
	j := gateJob("echo hi", 0)
	j.Cwd = "/nonexistent-directory-for-jobtail-test"
	res := RunPrecheck(context.Background(), j)
	if res.ExitCode == 0 {
		t.Fatalf("want a non-zero gate failure, got %+v", res)
	}
	if res.Err == nil {
		t.Error("Err should say the gate could not be executed")
	}
}

func TestNoPrecheckIsAPass(t *testing.T) {
	res := RunPrecheck(context.Background(), gateJob("", 0))
	if res.ExitCode != 0 || res.Output != "" || res.Err != nil {
		t.Errorf("a job with no gate should pass trivially, got %+v", res)
	}
}
