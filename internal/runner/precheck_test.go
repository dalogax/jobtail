package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dalogax/jobtail/internal/store"
)

func testStore(t testing.TB) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func agentJob(precheck string) store.Job {
	return store.Job{
		ID: "j1", Kind: "agent", Cron: "* * * * *", Timezone: "local", Enabled: true,
		Cwd: "/tmp", Prompt: "Do the analysis.",
		Provider: "claude", Precheck: precheck,
	}
}

func TestPrecheckSkip(t *testing.T) {
	st := testStore(t)
	j := agentJob("echo pending-stuff; exit 1")
	logPath := filepath.Join(t.TempDir(), "run.log")

	res, err := Execute(context.Background(), st, j, "run-skip", logPath)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != "skipped" || res.ExitCode != 1 {
		t.Fatalf("want skipped/1, got %s/%d", res.Status, res.ExitCode)
	}
	log, _ := os.ReadFile(logPath)
	for _, want := range []string{"exit=1", "pending-stuff"} {
		if !strings.Contains(string(log), want) {
			t.Fatalf("log missing %q: %s", want, log)
		}
	}
}

func TestPrecheckFailure(t *testing.T) {
	st := testStore(t)
	j := agentJob("echo boom >&2; exit 5")
	logPath := filepath.Join(t.TempDir(), "run.log")

	res, err := Execute(context.Background(), st, j, "run-fail", logPath)
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}
	if res.Status != "failed" || res.ExitCode != 5 {
		t.Fatalf("want failed/5, got %s/%d", res.Status, res.ExitCode)
	}
	log, _ := os.ReadFile(logPath)
	if !strings.Contains(string(log), "boom") || !strings.Contains(string(log), "exit=5") {
		t.Fatalf("log missing gate output: %s", log)
	}
}

func TestPrecheckPassInjectsContext(t *testing.T) {
	st := testStore(t)
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")

	// Stub agent CLI that records the prompt it was handed (-p <prompt>).
	bin := filepath.Join(dir, "fake-claude")
	stub := "#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = -p ] && [ $# -gt 1 ]; then printf '%s' \"$2\" > " + promptFile + "; fi; shift; done\nexit 0\n"
	if err := os.WriteFile(bin, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("JOBTAIL_CLAUDE_BIN", bin)

	logPath := filepath.Join(dir, "run.log")
	j := agentJob("echo MI-123\tTiming outage")
	res, err := Execute(context.Background(), st, j, "run-pass", logPath)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
// The stub agent exits 0 without emitting stream-json events, so the
// runner classifies the run as failed — that is expected and irrelevant
// here; this test only asserts the precheck context reached the prompt
// and the log records the pass.
	if res.Status == "skipped" || res.ExitCode == 1 {
		t.Fatalf("gate passed but run was skipped: %+v", res)
	}

	got, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("stub never received a prompt: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "Do the analysis.") {
		t.Fatalf("prompt lost original text: %s", s)
	}
	if !strings.Contains(s, "PENDING ITEMS") || !strings.Contains(s, "MI-123") {
		t.Fatalf("prompt missing precheck context: %s", s)
	}

	log, _ := os.ReadFile(logPath)
	if !strings.Contains(string(log), "[jobtail precheck] passed") {
		t.Fatalf("log missing pass header: %s", log)
	}
}
