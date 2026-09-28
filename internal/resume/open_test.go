package resume

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dalogax/jobtail/internal/store"
)

// fakeHerdr puts a stub `herdr` at the front of PATH that records every
// invocation's argv to a file and answers `tab create` with the real shape
// herdr 0.9.0 returns. It returns the path of that log.
//
// This exists because the one bug these tests could not otherwise have
// caught was in the argv itself: `herdr tab create --json` had been passed
// since the feature was written, and herdr 0.9.0 answers "unknown option:
// --json" with exit 2 — so every resume failed before the agent CLI was
// ever reached, while every unit test still passed. Checking the parsing
// without checking the command is how that survived.
func fakeHerdr(t *testing.T, tabCreateOutput string) string {
	t.Helper()
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")

	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + argvLog + "\n" +
		"case \"$1 $2\" in\n" +
		"  'tab create')\n" +
		"    for a in \"$@\"; do\n" +
		// Reproduce the failure that actually happened, so a reintroduced
		// --json fails this test the same way it failed in real life.
		"      if [ \"$a\" = '--json' ]; then echo 'unknown option: --json' >&2; exit 2; fi\n" +
		"    done\n" +
		"    cat <<'JSON'\n" + tabCreateOutput + "\nJSON\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"

	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvLog
}

func readArgv(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fake herdr was never invoked: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// The exact reply herdr 0.9.0 gives, trimmed to the fields that matter.
const realTabCreateReply = `{"id":"cli:tab:create","result":{"root_pane":{"cwd":"/w","pane_id":"wE:pA","tab_id":"wE:t9"},"tab":{"tab_id":"wE:t9"},"type":"tab_created"}}`

func TestOpenDrivesHerdrWithArgumentsItAccepts(t *testing.T) {
	t.Setenv("JOBTAIL_CLAUDE_BIN", "")
	argvLog := fakeHerdr(t, realTabCreateReply)

	j := store.Job{ID: "j1", Kind: "agent", Cwd: "/w"}
	r := store.Run{ID: "r1", JobID: "j1", SessionID: sql.NullString{String: "sess-1", Valid: true}}

	paneID, err := Open(j, r)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if paneID != "wE:pA" {
		t.Errorf("paneID = %q, want wE:pA", paneID)
	}

	argv := readArgv(t, argvLog)
	if len(argv) != 2 {
		t.Fatalf("expected a tab create then a pane run, got: %v", argv)
	}
	if argv[0] != "tab create --cwd /w" {
		t.Errorf("tab create argv = %q; the job's cwd must be passed and nothing herdr rejects", argv[0])
	}
	// The pane the tab reported is the pane the agent must be started in,
	// and it must be started with this run's own session id.
	if argv[1] != "pane run wE:pA claude --resume sess-1" {
		t.Errorf("pane run argv = %q", argv[1])
	}
}

// A tab that opened but whose agent command failed must report the failure
// rather than claiming a successful resume.
func TestOpenReportsAPaneRunFailure(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = 'tab create' ]; then echo '" + realTabCreateReply + "'; exit 0; fi\n" +
		"exit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	j := store.Job{ID: "j1", Kind: "agent", Cwd: "/w"}
	r := store.Run{ID: "r1", JobID: "j1", SessionID: sql.NullString{String: "sess-1", Valid: true}}
	if _, err := Open(j, r); err == nil {
		t.Error("Open reported success even though the agent command could not be started")
	}
}

// Herdr not being installed is an ordinary situation, not a crash.
func TestOpenWithoutHerdrExplainsItself(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	j := store.Job{ID: "j1", Kind: "agent", Cwd: "/w"}
	r := store.Run{ID: "r1", JobID: "j1", SessionID: sql.NullString{String: "sess-1", Valid: true}}

	_, err := Open(j, r)
	if err == nil {
		t.Fatal("Open succeeded with no herdr on PATH")
	}
	if !strings.Contains(err.Error(), "herdr") {
		t.Errorf("error = %v, want it to name herdr as the missing piece", err)
	}
}
