// Package execengine runs one job's one execution: either a plain shell
// command ("cli" jobs) or a headless Claude Code turn ("agent" jobs).
package execengine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/dalogax/jobtail/internal/store"
)

// LogCapBytes caps how much of a run's output is captured to its log file
// (PRD §12 decision 9): a truncation marker is appended and the job keeps
// running — this bounds disk use, it is not a job-killer.
const LogCapBytes = 10 * 1024 * 1024

// ClaudeBin resolves the binary used for "agent" jobs. Overridable via
// JOBTAIL_CLAUDE_BIN so tests can point it at a stub that emits canned
// stream-json without spending real API calls.
func ClaudeBin() string {
	if b := os.Getenv("JOBTAIL_CLAUDE_BIN"); b != "" {
		return b
	}
	return "claude"
}

// Result is what a run produced, independent of job kind.
type Result struct {
	Status    string // "ok" | "failed" | "timeout"
	ExitCode  int
	SessionID string // agent jobs only
}

// RunCLI executes job.Command via `sh -c` in job.Cwd, capturing combined
// stdout+stderr to logPath (capped at LogCapBytes).
func RunCLI(ctx context.Context, j store.Job, logPath string) (Result, error) {
	ctx, cancel := withJobTimeout(ctx, j)
	defer cancel()

	f, err := os.Create(logPath)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	w := newCapWriter(f, LogCapBytes)

	cmd := exec.CommandContext(ctx, "sh", "-c", j.Command)
	cmd.Dir = j.Cwd
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.Cancel = terminateThenKill(cmd)

	err = cmd.Run()
	return classifyExit(ctx, cmd, err)
}

// RunAgent runs `claude -p <prompt> --output-format stream-json ...` in
// job.Cwd, streaming the transcript to logPath (capped at LogCapBytes) and
// extracting the session id from the init event as soon as it appears, via
// onSessionID (so a run that later fails is still resumable — PRD §12
// decision 6). The final `result` event's is_error field decides ok/failed.
func RunAgent(ctx context.Context, j store.Job, logPath string, onSessionID func(sessionID string)) (Result, error) {
	ctx, cancel := withJobTimeout(ctx, j)
	defer cancel()

	f, err := os.Create(logPath)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	w := newCapWriter(f, LogCapBytes)

	args := []string{
		"-p", j.Prompt,
		"--output-format", "stream-json",
		"--verbose", // claude 2.1.x refuses -p --output-format=stream-json without it
		"--add-dir", j.Cwd,
		"--no-session-persistence",
	}
	if j.PermissionMode != "" {
		args = append(args, "--permission-mode", j.PermissionMode)
	} else {
		args = append(args, "--permission-mode", "acceptEdits")
	}
	if j.Model != "" {
		args = append(args, "--model", j.Model)
	}

	cmd := exec.CommandContext(ctx, ClaudeBin(), args...)
	cmd.Dir = j.Cwd
	cmd.Stderr = w
	cmd.Cancel = terminateThenKill(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}

	sawError := false
	sawResult := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024) // stream-json lines can be long
	for scanner.Scan() {
		line := scanner.Bytes()
		_, _ = w.Write(line)
		_, _ = w.Write([]byte("\n"))

		var ev streamEvent
		if json.Unmarshal(line, &ev) != nil {
			continue // non-JSON or partial line: still logged above, just not parsed
		}
		if ev.Type == "system" && ev.Subtype == "init" && ev.SessionID != "" && onSessionID != nil {
			onSessionID(ev.SessionID)
		}
		if ev.Type == "result" {
			sawResult = true
			if ev.IsError != nil && *ev.IsError {
				sawError = true
			}
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()

	res, err := classifyExit(ctx, cmd, waitErr)
	if err != nil {
		return res, err
	}
	if res.Status == "ok" && (sawError || !sawResult || scanErr != nil) {
		// Process exited 0 but the agent itself reported failure, the
		// stream ended without ever producing a result event, or the
		// transcript couldn't be scanned cleanly.
		res.Status = "failed"
	}
	return res, nil
}

type streamEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	IsError   *bool  `json:"is_error"`
}

func withJobTimeout(ctx context.Context, j store.Job) (context.Context, context.CancelFunc) {
	if j.TimeoutSeconds <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(j.TimeoutSeconds)*time.Second)
}

// terminateThenKill gives a job's process group SIGTERM, then SIGKILL after
// a short grace period, instead of Go's default immediate SIGKILL on ctx
// cancellation.
func terminateThenKill(cmd *exec.Cmd) func() error {
	return func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		go func() {
			t := time.NewTimer(5 * time.Second)
			defer t.Stop()
			done := make(chan struct{})
			go func() { _, _ = cmd.Process.Wait(); close(done) }()
			select {
			case <-done:
			case <-t.C:
				_ = cmd.Process.Signal(syscall.SIGKILL)
			}
		}()
		return nil
	}
}

func classifyExit(ctx context.Context, cmd *exec.Cmd, runErr error) (Result, error) {
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Result{Status: "timeout", ExitCode: exitCode}, nil
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return Result{Status: "failed", ExitCode: exitCode}, runErr
	}
	if exitCode != 0 {
		return Result{Status: "failed", ExitCode: exitCode}, nil
	}
	return Result{Status: "ok", ExitCode: 0}, nil
}

// capWriter forwards up to `cap` bytes to the underlying writer, then
// silently drops the rest after appending one truncation marker. It always
// reports success for the writer it wraps (an exec.Cmd's Stdout/Stderr),
// since a capped log must never make the job itself fail or block.
type capWriter struct {
	w         *os.File
	cap       int64
	written   int64
	truncated bool
}

func newCapWriter(w *os.File, cap int64) *capWriter {
	return &capWriter{w: w, cap: cap}
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.truncated {
		return len(p), nil
	}
	remaining := c.cap - c.written
	if remaining <= 0 {
		c.truncated = true
		_, _ = c.w.WriteString("\n--- jobtail: log truncated at " + strconv.FormatInt(c.cap, 10) + " bytes, job kept running ---\n")
		return len(p), nil
	}
	toWrite := p
	if int64(len(p)) > remaining {
		toWrite = p[:remaining]
	}
	n, err := c.w.Write(toWrite)
	c.written += int64(n)
	if err != nil {
		return n, err
	}
	if int64(len(toWrite)) < int64(len(p)) {
		c.truncated = true
		_, _ = c.w.WriteString("\n--- jobtail: log truncated at " + strconv.FormatInt(c.cap, 10) + " bytes, job kept running ---\n")
	}
	return len(p), nil
}
