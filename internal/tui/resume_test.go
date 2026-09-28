package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/dalogax/jobtail/internal/store"
)

// seedAgentRun adds an agent job with one finished run, optionally carrying
// a captured session id, and returns a model already focused on that run.
//
// Nothing here resolves the resume command itself: resume.Open shells out to
// the real herdr binary, so these tests inspect the tea.Cmd the model hands
// back rather than running it. That keeps the suite from opening terminal
// tabs on whoever runs it.
func seedAgentRun(t *testing.T, sessionID string) model {
	t.Helper()
	st, logsDir := seedStore(t)
	ctx := context.Background()

	dir := t.TempDir()
	if err := st.CreateJob(ctx, store.Job{
		ID: "agentjob", Kind: "agent", Cron: "0 0 * * *", Timezone: "local",
		Enabled: true, Cwd: dir, Prompt: "do a thing", MaxConcurrent: 1, Keep: 200,
	}); err != nil {
		t.Fatal(err)
	}

	runID := "agent-run-1"
	logPath := filepath.Join(logsDir, runID+".log")
	if err := os.WriteFile(logPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run, err := st.StartRun(ctx, "agentjob", runID, "manual", logPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != "" {
		if err := st.SetRunSessionID(ctx, run.ID, sessionID); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FinishRun(ctx, run.ID, "ok", 0, time.Now(), 5); err != nil {
		t.Fatal(err)
	}

	m := newModel(st, logsDir)
	m = send(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = resolve(t, m, m.Init())
	m.selectJobByID("agentjob")
	m = resolve(t, m, m.reloadRuns("agentjob"))
	m = send(t, m, special(tea.KeyEnter)) // jobs -> runs
	return m
}

// The whole point of the feature: "r" on an agent run that captured a
// session id hands that run off to an interactive session.
func TestResumeKeyOffersAnAgentRunWithASession(t *testing.T) {
	m := seedAgentRun(t, "sess-abc")

	if !m.canResumeSelection() {
		t.Fatal("an agent run with a captured session id should be resumable")
	}
	newM, cmd := m.Update(key('r'))
	m = newM.(model)
	if cmd == nil {
		t.Fatal(`pressing "r" on a resumable run produced no command`)
	}
	if !m.resuming["agent-run-1"] {
		t.Error("the run was not marked as resuming, so a second keypress would open a second tab")
	}
	if !strings.Contains(m.statusMsg, "resuming") {
		t.Errorf("statusMsg = %q, want it to say resuming", m.statusMsg)
	}
}

// A second "r" while the first tab is still opening must not open another.
func TestResumeKeyIsIdempotentWhileInFlight(t *testing.T) {
	m := seedAgentRun(t, "sess-abc")
	newM, _ := m.Update(key('r'))
	m = newM.(model)

	newM, cmd := m.Update(key('r'))
	if cmd != nil {
		t.Error("a second resume keypress started a second Herdr tab for the same run")
	}
	_ = newM
}

// The refusals have to be visible. Before this, the only way to discover a
// run wasn't resumable was that nothing happened.
func TestResumeKeyExplainsWhenItCannot(t *testing.T) {
	cases := []struct {
		name      string
		sessionID string
		wantMsg   string
	}{
		{"agent run whose session id never arrived", "", "no captured session id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := seedAgentRun(t, c.sessionID)
			if m.canResumeSelection() {
				t.Fatal("run should not be resumable")
			}
			newM, cmd := m.Update(key('r'))
			m = newM.(model)
			if cmd != nil {
				t.Error("an unresumable run still produced a resume command")
			}
			if !strings.Contains(m.statusMsg, c.wantMsg) {
				t.Errorf("statusMsg = %q, want it to contain %q", m.statusMsg, c.wantMsg)
			}
		})
	}
}

// A cli job's runs have no session to resume, and the seeded "greet" job is
// exactly that case.
func TestResumeKeyRefusesACliRun(t *testing.T) {
	st, logsDir := seedStore(t)
	m := newModel(st, logsDir)
	m = send(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = resolve(t, m, m.Init())
	m.selectJobByID("greet")
	m = resolve(t, m, m.reloadRuns("greet"))
	m = send(t, m, special(tea.KeyEnter)) // jobs -> runs

	if m.canResumeSelection() {
		t.Fatal("a cli run should never be resumable")
	}
	newM, cmd := m.Update(key('r'))
	m = newM.(model)
	if cmd != nil {
		t.Error("a cli run produced a resume command")
	}
	if !strings.Contains(m.statusMsg, "not agent") {
		t.Errorf("statusMsg = %q, want it to explain the run is not an agent run", m.statusMsg)
	}
}

// The help bar must not advertise a key that the pane will refuse — the same
// rule that made hintsFor pane-aware in the first place.
func TestHelpBarOnlyOffersResumeWhenItWouldWork(t *testing.T) {
	for _, focus := range []focusPane{focusRuns, focusLog} {
		withSession := renderHelpBar(200, focus, "", true)
		if !strings.Contains(withSession, "resume") {
			t.Errorf("focus %v: resumable selection but the bar offers no resume hint: %q", focus, withSession)
		}
		without := renderHelpBar(200, focus, "", false)
		if strings.Contains(without, "resume") {
			t.Errorf("focus %v: unresumable selection but the bar still offers resume: %q", focus, without)
		}
	}
	// The jobs pane's "r" is "run now", which is a different action and must
	// keep its own hint regardless.
	jobsBar := renderHelpBar(200, focusJobs, "", true)
	if strings.Contains(jobsBar, "resume") {
		t.Errorf("the jobs pane offered a resume hint: %q", jobsBar)
	}
	if !strings.Contains(jobsBar, "run now") {
		t.Errorf("the jobs pane lost its run-now hint: %q", jobsBar)
	}
}

// Adding a hint must not push "q quit" off a narrow terminal — the exact
// regression renderHelpBar's drop ordering exists to prevent, now with one
// more hint competing for the space.
func TestResumeHintNeverCrowdsOutQuit(t *testing.T) {
	for _, width := range []int{30, 40, 50, 60, 80, 100} {
		for _, focus := range []focusPane{focusJobs, focusRuns, focusLog} {
			bar := renderHelpBar(width, focus, "", true)
			if lipgloss.Width(bar) > width {
				t.Errorf("width %d, focus %v: bar is %d cols wide: %q",
					width, focus, lipgloss.Width(bar), bar)
			}
			if !strings.Contains(bar, "q") {
				t.Errorf("width %d, focus %v: no way to discover quit: %q", width, focus, bar)
			}
		}
	}
}

// Resuming is an action that can fail; a failure belongs in the status line,
// not in m.err, which blanks the dashboard for a broken store.
func TestResumeFailureIsReportedWithoutBreakingTheDashboard(t *testing.T) {
	m := seedAgentRun(t, "sess-abc")
	m = send(t, m, resumedMsg{runID: "agent-run-1", err: os.ErrNotExist})

	if m.err != nil {
		t.Errorf("a failed resume set m.err (%v), which blanks the dashboard", m.err)
	}
	if !strings.Contains(m.statusMsg, "resume failed") {
		t.Errorf("statusMsg = %q, want it to report the failure", m.statusMsg)
	}
	if m.resuming["agent-run-1"] {
		t.Error("the run stayed marked as resuming, so it could never be retried")
	}
}
