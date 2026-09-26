// Phase 2 (PRD §11): agent-kind execution against `claude -p
// --output-format stream-json`. These tests never call the real `claude`
// binary — JOBTAIL_CLAUDE_BIN points at fixtures/fake_claude.sh, a
// deterministic stand-in (PRD §10's ClaudeBin() override exists for exactly
// this), so the suite costs nothing and never depends on network/API state.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fakeClaudePath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("fixtures/fake_claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	return p
}

// runAgent is like env.run but adds the fake-claude env vars an agent job
// needs.
func (e *env) runAgent(mode string, args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Env = append(os.Environ(),
		"JOBTAIL_DATA_DIR="+e.dataDir,
		"JOBTAIL_DISABLE_NOTIFY=1",
		"JOBTAIL_CLAUDE_BIN="+fakeClaudePath(e.t),
		"FAKE_CLAUDE_MODE="+mode,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestAgentJobSuccessCapturesSessionID(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "nightly", "--kind", "agent", "--cron", "0 3 * * *",
		"--cwd", work, "--prompt", "check for updates")

	out, err := e.runAgent("ok", "run", "nightly")
	if err != nil {
		t.Fatalf("agent run should succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, ": ok") {
		t.Fatalf("expected ok status in output: %s", out)
	}

	runs := mustUnmarshalRuns(t, e.run("runs", "nightly", "--json"))
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %+v", runs)
	}
	r := runs[0]
	if r.Status != "ok" {
		t.Fatalf("want status ok, got %+v", r)
	}
	if !r.SessionID.Valid || r.SessionID.String == "" {
		t.Fatalf("session id was not captured: %+v", r)
	}

	logData, err := os.ReadFile(r.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), `"type":"result"`) {
		t.Fatalf("agent transcript log missing result event: %s", logData)
	}
}

func TestAgentJobReportedErrorIsFailed(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "flaky", "--kind", "agent", "--cron", "0 3 * * *",
		"--cwd", work, "--prompt", "do something risky")

	_, err := e.runAgent("error", "run", "flaky")
	if err == nil {
		t.Fatal("expected non-zero exit when the agent itself reports is_error:true")
	}

	runs := mustUnmarshalRuns(t, e.run("runs", "flaky", "--json"))
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("want 1 failed run, got %+v", runs)
	}
	// A run that failed mid-turn but still got past init should keep its
	// session id, so `resume` has something to attach to (PRD §12 decision 6).
	if !runs[0].SessionID.Valid || runs[0].SessionID.String == "" {
		t.Fatalf("session id should still be captured on a reported error: %+v", runs[0])
	}
}

func TestAgentJobCrashBeforeResultIsFailed(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "crasher", "--kind", "agent", "--cron", "0 3 * * *",
		"--cwd", work, "--prompt", "will crash")

	_, err := e.runAgent("crash", "run", "crasher")
	if err == nil {
		t.Fatal("expected non-zero exit when claude crashes")
	}

	runs := mustUnmarshalRuns(t, e.run("runs", "crasher", "--json"))
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("want 1 failed run, got %+v", runs)
	}
}

func TestAgentJobNoSessionIDStillOKButNotResumable(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "noinit", "--kind", "agent", "--cron", "0 3 * * *",
		"--cwd", work, "--prompt", "no init event")

	// noinit still ends with a result event, so it should be ok, but has no
	// session id — resume should refuse it, not crash.
	_, err := e.runAgent("noinit", "run", "noinit")
	if err != nil {
		t.Fatalf("noinit run should still succeed: %v", err)
	}
	runs := mustUnmarshalRuns(t, e.run("runs", "noinit", "--json"))
	if len(runs) != 1 || runs[0].Status != "ok" {
		t.Fatalf("want 1 ok run, got %+v", runs)
	}
	if runs[0].SessionID.Valid && runs[0].SessionID.String != "" {
		t.Fatalf("expected no session id to be captured, got %+v", runs[0])
	}

	out, err := e.runAllowFail("resume", runs[0].ID)
	if err == nil {
		t.Fatalf("resume should refuse a run with no captured session id, output: %s", out)
	}
	if !strings.Contains(out, "no captured session id") {
		t.Fatalf("expected a clear reason from resume, got: %s", out)
	}
}

func TestResumeRejectsCLIJob(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	e.run("add", "plain", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", work, "--cmd", "true")
	e.run("run", "plain")
	runs := mustUnmarshalRuns(t, e.run("runs", "plain", "--json"))

	out, err := e.runAllowFail("resume", runs[0].ID)
	if err == nil {
		t.Fatalf("resume should reject a cli-kind run, output: %s", out)
	}
	if !strings.Contains(out, "not agent") {
		t.Fatalf("expected a clear reason, got: %s", out)
	}
}
