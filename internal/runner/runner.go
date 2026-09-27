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
	"strings"
	"time"

	"github.com/dalogax/jobtail/internal/execengine"
	"github.com/dalogax/jobtail/internal/store"
)

// Execute dispatches by job kind and runs to completion in-process.
// When the job defines a precheck gate, it runs first: exit 0 proceeds
// (with the gate's stdout appended to an agent job's prompt as pending-item
// context), exit 1 marks the run "skipped" without executing the job, and
// exit >=2 (or a gate that could not run / timed out) marks the run
// "failed". Either early outcome is written to the run's log so the
// dashboard always shows what the gate decided and why.
func Execute(ctx context.Context, st *store.Store, j store.Job, runID, logPath string) (execengine.Result, error) {
	// The execengine run paths open the log with O_APPEND so a precheck
	// section written here survives underneath the job's own output. That
	// makes establishing the file this function's job: without it, running
	// the same run id twice would append to the previous attempt rather
	// than replace it, which is what os.Create used to guarantee.
	if err := truncateLog(logPath); err != nil {
		return execengine.Result{}, err
	}

	if j.Precheck != "" {
		pc := execengine.RunPrecheck(ctx, j)
		if err := writePrecheckLog(logPath, pc); err != nil {
			return execengine.Result{}, err
		}
		switch {
		case pc.ExitCode == 1:
			return execengine.Result{Status: "skipped", ExitCode: 1, Duration: pc.Duration}, nil
		case pc.ExitCode != 0:
			return execengine.Result{Status: "failed", ExitCode: pc.ExitCode, Duration: pc.Duration}, pc.Err
		}
		// Gate passed: give the agent the pending items the gate found.
		j.Prompt = appendPrecheckContext(j.Prompt, pc.Output)
	}
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

// writePrecheckLog writes the gate's transcript to the run's log file —
// either as the whole log (skip/fail outcomes, where the gate is all there
// is to show) or as the leading section before the agent's own transcript
// appends to the same file.
func writePrecheckLog(logPath string, pc execengine.PrecheckResult) error {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[jobtail precheck] exit=%d\n", pc.ExitCode))
	if pc.Err != nil {
		b.WriteString("[jobtail precheck] error: " + pc.Err.Error() + "\n")
	}
	b.WriteString(pc.Output)
	if pc.Output != "" && !strings.HasSuffix(pc.Output, "\n") {
		b.WriteString("\n")
	}
	if pc.ExitCode == 0 {
		b.WriteString("[jobtail precheck] passed; starting job\n\n")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// truncateLog creates the run's log, or empties it if it somehow already
// exists, leaving it for the precheck and then the job to append to.
func truncateLog(logPath string) error {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// appendPrecheckContext appends the gate's captured output to an agent
// prompt, bounded like MessagePart previews: gates can emit large TSV
// backlog dumps, and the agent only needs what it must act on.
func appendPrecheckContext(prompt, output string) string {
	if strings.TrimSpace(output) == "" {
		return prompt
	}
	const maxCtx = 16 * 1024
	trimmed := output
	if len(trimmed) > maxCtx {
		trimmed = trimmed[:maxCtx] + "\n... [truncated by jobtail]"
	}
	return prompt + "\n\n--- PENDING ITEMS (from precheck) ---\n" + trimmed
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
	if err := st.FinishRun(ctx, runID, status, res.ExitCode, time.Now(), res.Duration.Milliseconds()); err != nil {
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
