// Phase 2b (PRD §14): agent-kind execution against opencode and codex as
// alternatives to the default claude provider. Like phase2_agent_test.go,
// these tests never call the real binaries — JOBTAIL_OPENCODE_BIN/
// JOBTAIL_CODEX_BIN point at fixtures/fake_opencode.sh and
// fake_codex.sh, deterministic stand-ins for the real event shapes
// (verified against the real CLIs directly — see execengine's
// runOpenCodeAgent/runCodexAgent comments), so the suite costs nothing.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fakeBinPath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs("fixtures/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	return p
}

// runOpenCode is like env.run but adds the fake-opencode env vars an
// opencode-provider agent job needs.
func (e *env) runOpenCode(mode string, args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Env = append(os.Environ(),
		"JOBTAIL_DATA_DIR="+e.dataDir,
		"JOBTAIL_DISABLE_NOTIFY=1",
		"JOBTAIL_OPENCODE_BIN="+fakeBinPath(e.t, "fake_opencode.sh"),
		"FAKE_OPENCODE_MODE="+mode,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runCodex is like env.run but adds the fake-codex env vars a
// codex-provider agent job needs.
func (e *env) runCodex(mode string, args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Env = append(os.Environ(),
		"JOBTAIL_DATA_DIR="+e.dataDir,
		"JOBTAIL_DISABLE_NOTIFY=1",
		"JOBTAIL_CODEX_BIN="+fakeBinPath(e.t, "fake_codex.sh"),
		"FAKE_CODEX_MODE="+mode,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestOpenCodeAgentJobSuccessCapturesSessionID(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "oc-nightly", "--kind", "agent", "--provider", "opencode", "--cron", "0 3 * * *",
		"--cwd", work, "--prompt", "check for updates")

	out, err := e.runOpenCode("ok", "run", "oc-nightly")
	if err != nil {
		t.Fatalf("opencode agent run should succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, ": ok") {
		t.Fatalf("expected ok status in output: %s", out)
	}

	runs := mustUnmarshalRuns(t, e.run("runs", "oc-nightly", "--json"))
	if len(runs) != 1 || runs[0].Status != "ok" {
		t.Fatalf("want 1 ok run, got %+v", runs)
	}
	if !runs[0].SessionID.Valid || runs[0].SessionID.String == "" {
		t.Fatalf("session id was not captured: %+v", runs[0])
	}
}

func TestOpenCodeAgentJobErrorEventIsFailedDespiteZeroExit(t *testing.T) {
	// Real opencode confirmed to exit 0 even when it emits a top-level
	// {"type":"error"} event mid-stream — this is the exact case
	// runOpenCodeAgent's JSON scan exists to catch instead of trusting the
	// process exit code alone.
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "oc-flaky", "--kind", "agent", "--provider", "opencode", "--cron", "0 3 * * *",
		"--cwd", work, "--prompt", "do something risky")

	_, err := e.runOpenCode("error", "run", "oc-flaky")
	if err == nil {
		t.Fatal("expected jobtail to report failure even though the fake exits 0, mirroring real opencode's error-event-but-exit-0 behavior")
	}

	runs := mustUnmarshalRuns(t, e.run("runs", "oc-flaky", "--json"))
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("want 1 failed run, got %+v", runs)
	}
}

func TestOpenCodeAgentJobCrashIsFailed(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "oc-crasher", "--kind", "agent", "--provider", "opencode", "--cron", "0 3 * * *",
		"--cwd", work, "--prompt", "will crash")

	_, err := e.runOpenCode("crash", "run", "oc-crasher")
	if err == nil {
		t.Fatal("expected non-zero exit when opencode crashes")
	}

	runs := mustUnmarshalRuns(t, e.run("runs", "oc-crasher", "--json"))
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("want 1 failed run, got %+v", runs)
	}
}

func TestCodexAgentJobSuccessCapturesThreadIDAsSessionID(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "cx-nightly", "--kind", "agent", "--provider", "codex", "--cron", "0 3 * * *",
		"--cwd", work, "--prompt", "check for updates")

	out, err := e.runCodex("ok", "run", "cx-nightly")
	if err != nil {
		t.Fatalf("codex agent run should succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, ": ok") {
		t.Fatalf("expected ok status in output: %s", out)
	}

	runs := mustUnmarshalRuns(t, e.run("runs", "cx-nightly", "--json"))
	if len(runs) != 1 || runs[0].Status != "ok" {
		t.Fatalf("want 1 ok run, got %+v", runs)
	}
	if !runs[0].SessionID.Valid || runs[0].SessionID.String == "" {
		t.Fatalf("thread id was not captured as session id: %+v", runs[0])
	}
}

func TestCodexAgentJobTurnFailedIsFailed(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "cx-flaky", "--kind", "agent", "--provider", "codex", "--cron", "0 3 * * *",
		"--cwd", work, "--prompt", "do something risky")

	_, err := e.runCodex("error", "run", "cx-flaky")
	if err == nil {
		t.Fatal("expected non-zero exit on a turn.failed event, matching real codex's confirmed exit-1 behavior")
	}

	runs := mustUnmarshalRuns(t, e.run("runs", "cx-flaky", "--json"))
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("want 1 failed run, got %+v", runs)
	}
	if !runs[0].SessionID.Valid || runs[0].SessionID.String == "" {
		t.Fatalf("thread id should still be captured on a failed turn, so resume has something to attach to: %+v", runs[0])
	}
}

func TestAddRejectsProviderOnCLIJob(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	out, err := e.runAllowFail("add", "bad", "--kind", "cli", "--provider", "opencode", "--cron", "0 0 * * *",
		"--cwd", work, "--cmd", "true")
	if err == nil {
		t.Fatalf("expected --provider on a cli job to be rejected, output: %s", out)
	}
	if !strings.Contains(out, "--provider only applies to --kind agent") {
		t.Fatalf("expected a clear reason, got: %s", out)
	}
}

func TestAddRejectsUnknownProvider(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	out, err := e.runAllowFail("add", "bad2", "--kind", "agent", "--provider", "chatgpt", "--cron", "0 0 * * *",
		"--cwd", work, "--prompt", "hi")
	if err == nil {
		t.Fatalf("expected an unknown --provider value to be rejected, output: %s", out)
	}
	if !strings.Contains(out, "--provider must be one of") {
		t.Fatalf("expected a clear reason, got: %s", out)
	}
}

func TestShowDisplaysProvider(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "oc-show", "--kind", "agent", "--provider", "opencode", "--cron", "0 0 * * *",
		"--cwd", work, "--prompt", "hi")
	out := e.run("show", "oc-show")
	if !strings.Contains(out, "provider:        opencode") {
		t.Fatalf("expected show to display the provider, got: %s", out)
	}
}

func TestShowDisplaysDefaultProviderLabel(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "claude-show", "--kind", "agent", "--cron", "0 0 * * *",
		"--cwd", work, "--prompt", "hi")
	out := e.run("show", "claude-show")
	if !strings.Contains(out, "provider:        claude (default)") {
		t.Fatalf("expected show to label the unset provider as claude (default), got: %s", out)
	}
}

// TestRealOpenCodeAcceptsOurAgentInvocation is opencode's counterpart to
// phase2_agent_test.go's TestRealClaudeAcceptsOurAgentInvocation: it calls
// the real opencode binary end-to-end (with a real free model), confirmed
// working directly against this box. Skipped by default since it still
// depends on opencode being installed and a model being configured;
// opt in with JOBTAIL_REAL_OPENCODE_TEST=1.
func TestRealOpenCodeAcceptsOurAgentInvocation(t *testing.T) {
	if os.Getenv("JOBTAIL_REAL_OPENCODE_TEST") == "" {
		t.Skip("set JOBTAIL_REAL_OPENCODE_TEST=1 to run this against the real opencode binary")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode not on PATH")
	}
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "real-oc-check", "--kind", "agent", "--provider", "opencode", "--cron", "0 0 * * *",
		"--cwd", work, "--prompt", "Reply with exactly the word OK and nothing else.", "--model", "opencode/big-pickle")
	out, err := e.runAllowFail("run", "real-oc-check")
	if err != nil {
		t.Fatalf("real opencode invocation was rejected: %v\n%s", err, out)
	}
}

// TestRealCodexAcceptsOurAgentInvocation is codex's counterpart. Unlike
// the opencode version above, this has never passed on this box (no
// stored codex credentials — see runCodexAgent's comment) — it exists so
// that whoever next has working codex credentials can flip it on and get
// a real answer instead of relying on the inferred success-path schema.
// Opt in with JOBTAIL_REAL_CODEX_TEST=1.
func TestRealCodexAcceptsOurAgentInvocation(t *testing.T) {
	if os.Getenv("JOBTAIL_REAL_CODEX_TEST") == "" {
		t.Skip("set JOBTAIL_REAL_CODEX_TEST=1 to run this against the real codex binary (requires codex login)")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not on PATH")
	}
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "real-cx-check", "--kind", "agent", "--provider", "codex", "--cron", "0 0 * * *",
		"--cwd", work, "--prompt", "Reply with exactly the word OK and nothing else.")
	out, err := e.runAllowFail("run", "real-cx-check")
	if err != nil {
		t.Fatalf("real codex invocation was rejected: %v\n%s", err, out)
	}
}
