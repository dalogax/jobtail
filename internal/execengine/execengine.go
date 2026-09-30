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
	ownProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	notifyStart(ctx, cmd)
	err = cmd.Wait()
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
	// Duration is measured around the gate itself, so a run the gate
	// skipped or failed still reports how long it actually took rather
	// than a placeholder zero.
	Duration time.Duration
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
	// Cancel has to actually stop the gate. It was previously overridden
	// with a hook that only reported the context's cause, which *replaces*
	// the kill CommandContext installs rather than adding to it — so the
	// deadline fired, the result said TimedOut, and cmd.Run went on
	// blocking until the gate finished by itself: a `sleep 10` gate with a
	// 1s timeout took 10s and still claimed to have timed out.
	// ownProcessGroup is what the job run paths already use — SIGTERM to the
	// gate's whole process group, then SIGKILL if that is ignored.
	ownProcessGroup(cmd)
	// And WaitDelay bounds the tail: output is captured through a pipe
	// here (a buffer, not a file as the job paths use), and Wait blocks
	// until every writer closes it — including any grandchild the gate
	// left behind, which killing the shell does not reap.
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	err := cmd.Start()
	if err == nil {
		notifyStart(ctx, cmd)
		err = cmd.Wait()
	}
	elapsed := time.Since(start)

	out := buf.String()
	if len(out) > LogCapBytes {
		out = out[:LogCapBytes] + "\n... [jobtail: precheck output truncated]"
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return PrecheckResult{ExitCode: 2, Output: out, TimedOut: true, Duration: elapsed,
			Err: fmt.Errorf("precheck timed out after %ds", j.PrecheckTimeoutSeconds)}
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return PrecheckResult{ExitCode: exitErr.ExitCode(), Output: out, Duration: elapsed}
		}
		return PrecheckResult{ExitCode: 2, Output: out, Err: err, Duration: elapsed}
	}
	return PrecheckResult{ExitCode: 0, Output: out, Duration: elapsed}
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

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	w := newCapWriter(f, LogCapBytes)

	// Deliberately *not* --no-session-persistence. That flag was here from
	// the first commit and quietly made `jobtail resume` a no-op for claude
	// jobs: claude's own --help says sessions run under it "will not be
	// saved to disk and cannot be resumed". The session id still arrives on
	// the init event and was still captured into runs.session_id, so
	// everything looked right in the database while the session it named had
	// already been discarded — `claude --resume <that id>` answers "No
	// conversation found with session ID". Persisting is what makes the
	// captured id mean something (PRD §14, §20).
	args := []string{
		"-p", j.Prompt,
		"--output-format", "stream-json",
		"--verbose", // claude 2.1.x refuses -p --output-format=stream-json without it
		"--add-dir", j.Cwd,
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
	ownProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	notifyStart(ctx, cmd)

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
	ownProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	notifyStart(ctx, cmd)

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
	ownProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	notifyStart(ctx, cmd)

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

// startHookKey carries the callback WithStartHook installs.
type startHookKey struct{}

// WithStartHook returns a context under which every process execengine
// starts — the precheck gate as well as the job — is reported to fn by pid
// as soon as it is running. The runner records it so that a run can still
// be killed by a different jobtail process if the one executing it dies or
// wedges (runner.ReapStale). A context value rather than a parameter because
// it cuts across every entry point here, precheck included.
func WithStartHook(ctx context.Context, fn func(pid int)) context.Context {
	return context.WithValue(ctx, startHookKey{}, fn)
}

func notifyStart(ctx context.Context, cmd *exec.Cmd) {
	if fn, ok := ctx.Value(startHookKey{}).(func(pid int)); ok && fn != nil && cmd.Process != nil {
		fn(cmd.Process.Pid)
	}
}

// killGrace is how long a process group gets between SIGTERM and SIGKILL.
const killGrace = 5 * time.Second

// ownProcessGroup starts cmd as the leader of a new process group and makes
// ctx cancellation (the job's max time, or an interrupted `jobtail run`)
// stop the whole group: SIGTERM, then SIGKILL after killGrace if anything
// is still there. Signalling only the direct child, as this used to, left
// behind whatever it had spawned — `sh -c` running a pipeline, an agent
// CLI's own tool subprocesses — still holding the job's resources after
// the run had been recorded as over.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		pgid := cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		go func() {
			time.Sleep(killGrace)
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}()
		return nil
	}
}

// KillProcessGroup stops a process group from outside the process that
// started it: SIGTERM, up to killGrace for it to exit, then SIGKILL. It
// reports whether the group existed at all. Used by the reaper on runs whose
// own executor is gone or has overrun, where there is no exec.Cmd to cancel.
func KillProcessGroup(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		return false
	}
	deadline := time.Now().Add(killGrace)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pgid, 0) != nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	return true
}

// ProcessAlive reports whether pid names a live process. EPERM counts as
// alive: the process exists, it just isn't ours to signal.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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
