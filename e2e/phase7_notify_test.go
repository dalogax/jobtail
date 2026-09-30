// Per-job Herdr notifications (--notify).
package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotifySettingRoundTrips(t *testing.T) {
	e := newEnv(t)
	e.run("add", "n", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", t.TempDir(), "--cmd", "true")
	if out := e.run("show", "n"); !strings.Contains(out, "notify:          failed,timeout (default)") {
		t.Fatalf("show should report the default, got:\n%s", out)
	}
	e.run("edit", "n", "--notify", "failed,started")
	if out := e.run("show", "n"); !strings.Contains(out, "notify:          started,failed\n") {
		t.Fatalf("show should report the edited list, got:\n%s", out)
	}
	e.run("edit", "n", "--notify", "none")
	if out := e.run("show", "n"); !strings.Contains(out, "notify:          none\n") {
		t.Fatalf("show should report none, got:\n%s", out)
	}
	if out, err := e.runAllowFail("edit", "n", "--notify", "sucess"); err == nil || !strings.Contains(out, "unknown event") {
		t.Fatalf("a misspelled event should be rejected, got err=%v:\n%s", err, out)
	}
}

// skipped_overlap is recorded before a run executes at all, so it is the
// one event raised outside the runner; check it end to end.
func TestSkippedOverlapNotifies(t *testing.T) {
	e := newEnv(t)
	e.run("add", "slow", "--kind", "cli", "--cron", "0 0 * * *", "--cwd", t.TempDir(), "--cmd", "sleep 2",
		"--notify", "skipped_overlap")

	herdrDir := t.TempDir()
	calls := filepath.Join(herdrDir, "calls")
	if err := os.WriteFile(filepath.Join(herdrDir, "herdr"),
		[]byte("#!/bin/sh\necho \"$*\" >> "+calls+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	withHerdr := func(args ...string) *exec.Cmd {
		cmd := exec.Command(e.bin, args...)
		cmd.Env = append(os.Environ(),
			"JOBTAIL_DATA_DIR="+e.dataDir,
			"JOBTAIL_DISABLE_NOTIFY=",
			"PATH="+herdrDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		)
		return cmd
	}

	first := withHerdr("run", "slow")
	var buf bytes.Buffer
	first.Stdout, first.Stderr = &buf, &buf
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if out, err := withHerdr("run", "slow").CombinedOutput(); err != nil || !strings.Contains(string(out), "skipped:") {
		t.Fatalf("second run should be skipped, got err=%v:\n%s", err, out)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("first run: %v\n%s", err, buf.String())
	}

	data, _ := os.ReadFile(calls)
	got := strings.TrimSpace(string(data))
	if strings.Count(got, "\n") != 0 || !strings.Contains(got, "jobtail: slow skipped_overlap") {
		t.Fatalf("want exactly one skipped_overlap notification (ok isn't subscribed), got:\n%s", got)
	}
}
