// Package runner is the one code path that actually executes a run and
// records its outcome — shared by `jobtail run`/`run-exec` and the TUI's
// "run now"/"e" actions, so both act through the same logic (PRD §9: "act
// on the job/run under the cursor immediately via the same code path as the
// CLI — no separate TUI-only logic to keep in sync").
package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/dalogax/jobtail/internal/execengine"
	"github.com/dalogax/jobtail/internal/store"
)

// Execute dispatches by job kind and runs to completion in-process.
func Execute(ctx context.Context, st *store.Store, j store.Job, runID, logPath string) (execengine.Result, error) {
	switch j.Kind {
	case "cli":
		return execengine.RunCLI(ctx, j, logPath)
	case "agent":
		return execengine.RunAgent(ctx, j, logPath, func(sessionID string) {
			_ = st.SetRunSessionID(ctx, runID, sessionID)
		})
	default:
		return execengine.Result{}, fmt.Errorf("unknown job kind %q", j.Kind)
	}
}

// Finish records the outcome and, on failure, tries a Herdr desktop
// notification (best-effort: silently skipped if herdr isn't on PATH, or if
// JOBTAIL_DISABLE_NOTIFY is set — used by the e2e suite so intentionally
// failing test jobs don't spam real Herdr toasts on the dev box).
func Finish(ctx context.Context, st *store.Store, j store.Job, runID string, res execengine.Result, runErr error) error {
	status := res.Status
	if status == "" {
		status = "failed"
	}
	if err := st.FinishRun(ctx, runID, status, res.ExitCode, time.Now()); err != nil {
		return err
	}
	if status == "failed" || status == "timeout" {
		notifyFailure(j.ID, status)
	}
	return runErr
}

func notifyFailure(jobID, status string) {
	if os.Getenv("JOBTAIL_DISABLE_NOTIFY") != "" {
		return
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		return
	}
	_ = exec.Command("herdr", "notification", "show",
		fmt.Sprintf("jobtail: %s %s", jobID, status), "--sound", "request").Run()
}
