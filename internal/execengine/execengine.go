// Package execengine runs one job's one execution: either a plain shell
// command ("cli" jobs) or a headless Claude Code turn ("agent" jobs).
package execengine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// childEnv is the environment for every command execengine spawns: the
// parent's own environment, plus MISE_QUIET=1. Found via a real run: on a
// mise-managed box, the resolved `claude`/`opencode`/`codex`/etc binary is
// actually a mise shim that prints "mise ~/.config/mise/config.toml
// tools: <tool>@<version>" to stderr before delegating — which then landed
// verbatim in a captured log and leaked into the rendered transcript
// (exactly the raw-noise PRD §9 exists to prevent). This only silences
// jobtail's own child processes, not the user's interactive shell or mise
// generally.
func childEnv() []string {
	return append(os.Environ(), "MISE_QUIET=1")
}

// Provider names for store.Job.Provider. Empty string also means
// ProviderClaude — see EffectiveProvider.
const (
	ProviderClaude   = "claude"
	ProviderOpenCode = "opencode"
	ProviderCodex    = "codex"
)

// EffectiveProvider treats an empty Job.Provider as ProviderClaude: agent
// jobs existed before multi-provider support, so their (unset) provider
// column must keep meaning what it always meant. Exported so callers
// outside this package (cmd_run.go's `resume`) can branch on it too,
// without duplicating the "" -> claude default.
func EffectiveProvider(p string) string {
	if p == "" {
		return ProviderClaude
	}
	return p
}

// ClaudeBin resolves the binary used for provider="claude" agent jobs.
// Overridable via JOBTAIL_CLAUDE_BIN so tests can point it at a stub that
// emits canned stream-json without spending real API calls.
func ClaudeBin() string {
	if b := os.Getenv("JOBTAIL_CLAUDE_BIN"); b != "" {
		return b
	}
	return "claude"
}

// OpenCodeBin resolves the binary used for provider="opencode" agent jobs.
// Overridable via JOBTAIL_OPENCODE_BIN, same reasoning as ClaudeBin.
func OpenCodeBin() string {
	if b := os.Getenv("JOBTAIL_OPENCODE_BIN"); b != "" {
		return b
	}
	return "opencode"
}

// CodexBin resolves the binary used for provider="codex" agent jobs.
// Overridable via JOBTAIL_CODEX_BIN, same reasoning as ClaudeBin.
func CodexBin() string {
	if b := os.Getenv("JOBTAIL_CODEX_BIN"); b != "" {
		return b
	}
	return "codex"
}

// Result is what a run produced, independent of job kind.
type Result struct {
	Status    string // "ok" | "failed" | "timeout"
	ExitCode  int
	SessionID string // agent jobs only
	// Duration is measured directly around the actual exec.Cmd run, not
	// derived from the run row's started_at/finished_at afterward: for a
	// scheduled run, started_at is deliberately the nominal cron slot (see
	// store's additiveMigrations comment), which can make a timestamp-diff
	// duration read as minutes when the real run took milliseconds.
	Duration time.Duration
}

// RunCLI executes job.Command via `sh -c` in job.Cwd, capturing combined
// stdout+stderr to logPath (capped at LogCapBytes).
func RunCLI(ctx context.Context, j store.Job, logPath string) (Result, error) {
	start := time.Now()
	ctx, cancel := withJobTimeout(ctx, j)
	defer cancel()

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	w := newCapWriter(f, LogCapBytes)

	cmd := exec.CommandContext(ctx, "sh", "-c", j.Command)
	cmd.Dir = j.Cwd
	cmd.Env = childEnv()
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.Cancel = terminateThenKill(cmd)

	err = cmd.Run()
	return classifyExit(ctx, cmd, err, start)
}

// PrecheckResult is the outcome of a job's optional precheck gate: rc 0
// means "proceed" (with Output available as pending-item context), rc 1
// means "nothing to do" (skip the job), rc >=2 or an execution error means
// the gate itself failed (fail the run).
type PrecheckResult struct {
	ExitCode int
	Output   string // combined stdout+stderr, capped at LogCapBytes
	Err      error  // non-nil if the gate could not be executed at all
	TimedOut bool
}

// RunPrecheck executes j.Precheck via `sh -c` in j.Cwd, bounded by
// precheckTimeout when > 0, and returns its exit code and captured output.
// The output is intended both for the run log (so the dashboard shows what
// the gate saw) and as prompt context for agent jobs on a pass.
func RunPrecheck(ctx context.Context, j store.Job) PrecheckResult {
	if j.Precheck == "" {
		return PrecheckResult{ExitCode: 0}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if j.PrecheckTimeoutSeconds > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(j.PrecheckTimeoutSeconds)*time.Second)
		defer cancel()
	}

	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "sh", "-c", j.Precheck)
	cmd.Dir = j.Cwd
	cmd.Env = childEnv()
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	// The gate is advisory context, not a program under our control: a
	// failing gate should yield "precheck failed", not a Go panic.
	cmd.Cancel = func() error { return context.Cause(ctx) }

	err := cmd.Run()
	out := buf.String()
	if len(out) > LogCapBytes {
		out = out[:LogCapBytes] + "\n... [jobtail: precheck output truncated]"
	}

	if ctx.Err() != nil && (errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		return PrecheckResult{ExitCode: 2, Output: out, Err: fmt.Errorf("precheck timed out after %ds", j.PrecheckTimeoutSeconds), TimedOut: true}
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return PrecheckResult{ExitCode: exitErr.ExitCode(), Output: out}
		}
		return PrecheckResult{ExitCode: 2, Output: out, Err: err}
	}
	return PrecheckResult{ExitCode: 0, Output: out}
}

// RunAgent runs one headless agent-CLI turn in job.Cwd, dispatching on
// job.Provider to the right binary/protocol (PRD §14: multi-provider
// support). Every provider streams its transcript to logPath (capped at
// LogCapBytes) and, as soon as a session/thread id appears in the stream,
// reports it via onSessionID — so a run that later fails is still
// resumable (PRD §12 decision 6), regardless of provider.
func RunAgent(ctx context.Context, j store.Job, logPath string, onSessionID func(sessionID string)) (Result, error) {
	switch EffectiveProvider(j.Provider) {
	case ProviderOpenCode:
		return runOpenCodeAgent(ctx, j, logPath, onSessionID)
	case ProviderCodex:
		return runCodexAgent(ctx, j, logPath, onSessionID)
	default:
		return runClaudeAgent(ctx, j, logPath, onSessionID)
	}
}

// runClaudeAgent runs `claude -p <prompt> --output-format stream-json ...`
// in job.Cwd. The final `result` event's is_error field decides ok/failed.
func runClaudeAgent(ctx context.Context, j store.Job, logPath string, onSessionID func(sessionID string)) (Result, error) {
	start := time.Now()
	ctx, cancel := withJobTimeout(ctx, j)
	defer cancel()

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
	cmd.Env = childEnv()
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

	res, err := classifyExit(ctx, cmd, waitErr, start)
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

// runOpenCodeAgent runs `opencode run <prompt> --format json ...` in
// job.Cwd. Verified against the real opencode CLI (v1.18.21): every JSONL
// event — including the final "text"/"step_finish" — carries the session
// id in a top-level camelCase "sessionID" field (unlike claude's
// snake_case "session_id" and no separate init event), so the first event
// with a non-empty sessionID is what's reported via onSessionID.
//
// The process exit code alone can't be trusted: a run against an invalid
// model produced a top-level {"type":"error",...} event and *still* exited
// 0 (confirmed by direct test). Failure is therefore decided by scanning
// for that event type, the same defensive pattern as runClaudeAgent's
// is_error check.
func runOpenCodeAgent(ctx context.Context, j store.Job, logPath string, onSessionID func(sessionID string)) (Result, error) {
	start := time.Now()
	ctx, cancel := withJobTimeout(ctx, j)
	defer cancel()

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	w := newCapWriter(f, LogCapBytes)

	args := []string{"run", j.Prompt, "--format", "json"}
	if j.Model != "" {
		args = append(args, "-m", j.Model)
	}

	cmd := exec.CommandContext(ctx, OpenCodeBin(), args...)
	cmd.Dir = j.Cwd
	cmd.Env = childEnv()
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
	gotSessionID := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		_, _ = w.Write(line)
		_, _ = w.Write([]byte("\n"))

		var ev openCodeEvent
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if !gotSessionID && ev.SessionID != "" && onSessionID != nil {
			onSessionID(ev.SessionID)
			gotSessionID = true
		}
		if ev.Type == "error" {
			sawError = true
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()

	res, err := classifyExit(ctx, cmd, waitErr, start)
	if err != nil {
		return res, err
	}
	if res.Status == "ok" && (sawError || scanErr != nil) {
		res.Status = "failed"
	}
	return res, nil
}

type openCodeEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
}

// runCodexAgent runs `codex exec --json ...` in job.Cwd. Verified against
// the real codex CLI (0.148.0) on its error path only (this box has no
// stored codex credentials, so a successful turn's item.completed shape
// for assistant text has not been observed first-hand) — the events
// actually seen are "thread.started" (carries thread_id, codex's
// equivalent of a session id), "turn.started", "turn.failed", and
// top-level "error". Unlike opencode, a failed codex turn reliably exits
// non-zero (confirmed directly), so classifyExit's own exit-code check is
// the primary signal; the "turn.failed"/"error" scan below is the same
// defensive belt-and-suspenders as the other two providers, in case a
// future codex version ever exits 0 on an internal error the way opencode
// does today.
//
// job.PermissionMode doubles as codex's --sandbox policy here (its values
// mean something different per provider — see cmd_job.go's flag help):
// "read-only" | "workspace-write" | "danger-full-access". Empty defaults
// to "workspace-write", the closest codex equivalent of claude's
// acceptEdits default (PRD §12 decision: never bypassPermissions/
// danger-full-access unless a job explicitly opts in).
func runCodexAgent(ctx context.Context, j store.Job, logPath string, onSessionID func(sessionID string)) (Result, error) {
	start := time.Now()
	ctx, cancel := withJobTimeout(ctx, j)
	defer cancel()

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	w := newCapWriter(f, LogCapBytes)

	sandbox := j.PermissionMode
	if sandbox == "" {
		sandbox = "workspace-write"
	}
	args := []string{"exec", "--json", "--sandbox", sandbox, "--skip-git-repo-check"}
	if j.Model != "" {
		args = append(args, "-m", j.Model)
	}
	args = append(args, j.Prompt)

	cmd := exec.CommandContext(ctx, CodexBin(), args...)
	cmd.Dir = j.Cwd
	cmd.Env = childEnv()
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
	gotThreadID := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		_, _ = w.Write(line)
		_, _ = w.Write([]byte("\n"))

		var ev codexEvent
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if !gotThreadID && ev.ThreadID != "" && onSessionID != nil {
			onSessionID(ev.ThreadID)
			gotThreadID = true
		}
		if ev.Type == "turn.failed" || ev.Type == "error" {
			sawError = true
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()

	res, err := classifyExit(ctx, cmd, waitErr, start)
	if err != nil {
		return res, err
	}
	if res.Status == "ok" && (sawError || scanErr != nil) {
		res.Status = "failed"
	}
	return res, nil
}

type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
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

func classifyExit(ctx context.Context, cmd *exec.Cmd, runErr error, start time.Time) (Result, error) {
	dur := time.Since(start)
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Result{Status: "timeout", ExitCode: exitCode, Duration: dur}, nil
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return Result{Status: "failed", ExitCode: exitCode, Duration: dur}, runErr
	}
	if exitCode != 0 {
		return Result{Status: "failed", ExitCode: exitCode, Duration: dur}, nil
	}
	return Result{Status: "ok", ExitCode: 0, Duration: dur}, nil
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
