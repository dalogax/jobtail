package execengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dalogax/jobtail/internal/store"
)

// TestChildProcessesGetMiseQuiet is the regression test for a real bug
// found while verifying opencode support against the actual CLI on a
// mise-managed box: the resolved binary was a mise shim that printed "mise
// ~/.config/mise/config.toml tools: opencode@1.18.21" to stderr before
// delegating, and that line landed verbatim in the captured log (and, from
// there, straight into the rendered transcript — exactly the raw-noise
// PRD §9 exists to prevent). MISE_QUIET=1 (confirmed directly against the
// real `mise`/`opencode` on this box) silences it; this test just locks in
// that every command execengine spawns actually gets it, without needing
// mise installed to run the test itself.
func TestChildProcessesGetMiseQuiet(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")
	j := store.Job{Kind: "cli", Cwd: dir, Command: `echo "MISE_QUIET=$MISE_QUIET"`}

	res, err := RunCLI(context.Background(), j, logPath)
	if err != nil {
		t.Fatalf("RunCLI: %v", err)
	}
	if res.Status != "ok" {
		t.Fatalf("want status ok, got %+v", res)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "MISE_QUIET=1") {
		t.Fatalf("child process did not see MISE_QUIET=1, log: %s", data)
	}
}
