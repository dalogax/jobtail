package resume

import (
	"database/sql"
	"testing"

	"github.com/dalogax/jobtail/internal/store"
)

// Command's real per-provider syntax was verified against each binary's own
// --help before writing this (see Command's comment); this test just locks
// that mapping in place.
func TestCommandPerProvider(t *testing.T) {
	// Isolate from any JOBTAIL_*_BIN already set in the ambient environment
	// (this test runs in-process, not via a subprocess with a fresh env
	// like the e2e suite's tests do).
	t.Setenv("JOBTAIL_CLAUDE_BIN", "")
	t.Setenv("JOBTAIL_OPENCODE_BIN", "")
	t.Setenv("JOBTAIL_CODEX_BIN", "")

	cases := []struct {
		provider string
		want     string
	}{
		{"", "claude --resume sess-1"},
		{"claude", "claude --resume sess-1"},
		{"opencode", "opencode --session sess-1"},
		{"codex", "codex resume sess-1"},
	}
	for _, c := range cases {
		if got := Command(c.provider, "sess-1"); got != c.want {
			t.Errorf("Command(%q, ...) = %q, want %q", c.provider, got, c.want)
		}
	}
}

func agentJob() store.Job { return store.Job{ID: "j1", Kind: "agent"} }

func runWithSession(id string) store.Run {
	return store.Run{ID: "r1", JobID: "j1", SessionID: sql.NullString{String: id, Valid: id != ""}}
}

func TestPossible(t *testing.T) {
	cases := []struct {
		name string
		job  store.Job
		run  store.Run
		want bool
	}{
		{"agent run with a session id", agentJob(), runWithSession("sess-1"), true},
		{"agent run before the session id arrived", agentJob(), runWithSession(""), false},
		{"cli run", store.Job{ID: "j1", Kind: "cli"}, runWithSession("sess-1"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Possible(c.job, c.run); got != c.want {
				t.Errorf("Possible() = %v, want %v", got, c.want)
			}
			// Reason and Possible must agree, or the dashboard would offer a
			// key it then refuses, or refuse one it offered.
			if gotReason := Reason(c.job, c.run) == ""; gotReason != c.want {
				t.Errorf("Reason()==\"\" is %v but Possible() is %v", gotReason, c.want)
			}
		})
	}
}

// The two Herdr response shapes Open has to cope with, plus the failure that
// matters: a well-formed reply carrying no pane id at all, which must be an
// error rather than an empty pane id handed to `herdr pane run`.
func TestExtractPaneID(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"root_pane", `{"result":{"root_pane":{"pane_id":"wE:p9"}}}`, "wE:p9", false},
		{"pane", `{"result":{"pane":{"pane_id":"wE:p9"}}}`, "wE:p9", false},
		{"root_pane wins when both are present", `{"result":{"root_pane":{"pane_id":"a"},"pane":{"pane_id":"b"}}}`, "a", false},
		{"no pane id", `{"result":{}}`, "", true},
		{"not json", `nope`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := extractPaneID([]byte(c.in))
			if (err != nil) != c.wantErr {
				t.Fatalf("extractPaneID(%s) error = %v, wantErr %v", c.in, err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("extractPaneID(%s) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Open must refuse before it shells out to herdr, so an unresumable run
// can't leave an empty tab lying around.
func TestOpenRefusesUnresumableRunsWithoutTouchingHerdr(t *testing.T) {
	if _, err := Open(store.Job{ID: "j1", Kind: "cli"}, runWithSession("sess-1")); err == nil {
		t.Error("Open on a cli run returned no error")
	}
	if _, err := Open(agentJob(), runWithSession("")); err == nil {
		t.Error("Open on a run with no session id returned no error")
	}
}
