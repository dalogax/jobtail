package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dalogax/jobtail/internal/store"
)

// The log pane's title is how you tell whose output you are looking at.
// A bare "Log" header over a viewport full of JSONL is indistinguishable
// between two jobs that ran a minute apart, so the title must name the
// job and the run's local start time alongside the run id.
func TestLogPaneTitleIdentifiesJobAndRun(t *testing.T) {
	st, logsDir := seedStore(t)
	m := newModel(st, logsDir)

	m = send(t, m, tea.WindowSizeMsg{Width: 172, Height: 40})
	m = resolve(t, m, m.Init())
	m = send(t, m, special(tea.KeyEnter)) // jobs -> runs
	m = send(t, m, special(tea.KeyEnter)) // runs -> log

	title := m.logPaneTitle()
	if !strings.Contains(title, "greet") {
		t.Fatalf("log title missing job name: %q", title)
	}
	if !strings.Contains(title, "seed-run-1") {
		t.Fatalf("log title missing run id: %q", title)
	}
	// Local start time appears in the same 01-02 15:04:05 shape the Runs
	// pane uses (modulo seconds), so the two panes corroborate each other.
	if !strings.Contains(title, "·") {
		t.Fatalf("log title missing the separator layout: %q", title)
	}
}

// Runs of a deleted job keep their title identifying: jobByID only decorates
// the display name when the job still exists, and the raw JobID is used
// otherwise — never a bare "Log".
func TestLogPaneTitleFallBackToJobIDAfterDelete(t *testing.T) {
	st, logsDir := seedStore(t)
	m := newModel(st, logsDir)

	// Simulate selecting a run whose job was deleted: the runs table still
	// lists the run (rm only removes the job), while m.jobs no longer has
	// an entry for it.
	m.jobs = []store.JobSummary{}
	m.runs = []store.Run{{
		ID:        "ghost-run",
		JobID:     "greet",
		Trigger:   "manual",
		Status:    "ok",
		StartedAt: time.Now().Add(-2 * time.Minute),
	}}
	m.runsTable.SetRows(runRows(m.runs, m.runsCols))
	m.runsTable.SetCursor(0)
	if m.jobByID("greet") != nil {
		t.Fatalf("precondition: greet should not be in m.jobs")
	}

	title := m.logPaneTitle()
	if !strings.Contains(title, "greet") {
		t.Fatalf("title lost the job id after deletion: %q", title)
	}
	if !strings.Contains(title, "ghost-run") {
		t.Fatalf("title lost the run id: %q", title)
	}
}

// No run selected (empty runs list) keeps the plain "Log" title rather than
// decorating it with empty separators.
func TestLogPaneTitlePlainWithoutSelection(t *testing.T) {
	st, logsDir := seedStore(t)
	m := newModel(st, logsDir)
	m.runs = nil
	m.runsTable.SetCursor(0)

	if got := m.logPaneTitle(); got != "Log" {
		t.Fatalf("logPaneTitle() with no selection = %q, want %q", got, "Log")
	}
}
