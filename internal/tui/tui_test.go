// Phase 3 (PRD §11): the Bubble Tea dashboard itself. This drives the real
// model — real SQLite store, real log files on disk, real runner.Execute
// subprocess for "run now" — exactly as `jobtail tui` wires it up, but
// synchronously: each tea.Cmd it returns is resolved immediately in this
// goroutine instead of through the async tea.Program runtime. That's a
// deliberate choice over teatest's terminal-byte-stream matching, which
// proved racy here (a fast-finishing "run now" and a periodic re-render can
// both write to the same drained buffer in ways that made substring checks
// intermittently miss real, already-rendered frames) — driving the model
// directly is deterministic and still exercises the exact same Update/View
// logic a real session runs.
package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jarvis0064/jobtail/internal/store"
)

func init() {
	refreshInterval = time.Millisecond // don't pay tea.Tick's real sleep in tests
}

func seedStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "jobtail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	if err := st.CreateJob(ctx, store.Job{
		ID: "greet", Kind: "cli", Cron: "0 0 * * *", Timezone: "local",
		Enabled: true, Cwd: dir, Command: "echo hi", MaxConcurrent: 1, Keep: 200,
	}); err != nil {
		t.Fatal(err)
	}

	runID := "seed-run-1"
	logPath := filepath.Join(logsDir, runID+".log")
	if err := os.WriteFile(logPath, []byte("hello-from-seeded-log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run, err := st.StartRun(ctx, "greet", runID, "manual", logPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, run.ID, "ok", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	return st, logsDir
}

// send applies one message and fully resolves whatever tea.Cmd chain it
// triggers (batches unwrapped, nested cmds followed) before returning.
func send(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	newM, cmd := m.Update(msg)
	m = newM.(model)
	return resolve(t, m, cmd)
}

func resolve(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	for _, msg := range runCmd(cmd) {
		m = send(t, m, msg)
	}
	return m
}

// runCmd executes cmd (and recursively any tea.Batch it produces),
// deliberately dropping tickMsg results so the model's periodic
// refresh-forever command never causes unbounded recursion in a test.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, runCmd(c)...)
		}
		return out
	}
	if _, ok := msg.(tickMsg); ok {
		return nil // tests trigger refreshes explicitly via send(t, m, tickMsg(...))
	}
	return []tea.Msg{msg}
}

func key(r rune) tea.KeyMsg            { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }
func special(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func TestTUINavigatesJobsToRunsToLog(t *testing.T) {
	st, logsDir := seedStore(t)
	m := newModel(st, logsDir)

	m = send(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = resolve(t, m, m.Init())

	if !strings.Contains(m.View(), "greet") {
		t.Fatalf("initial view missing job id 'greet':\n%s", m.View())
	}
	if !strings.Contains(m.View(), "● greet") {
		t.Fatalf("expected the enabled marker on greet:\n%s", m.View())
	}

	m = send(t, m, special(tea.KeyEnter)) // jobs -> runs
	if m.focus != focusRuns {
		t.Fatalf("want focus on runs pane, got %v", m.focus)
	}
	if !strings.Contains(m.View(), "manual") {
		t.Fatalf("runs pane missing the seeded run's trigger:\n%s", m.View())
	}

	m = send(t, m, special(tea.KeyEnter)) // runs -> log
	if m.focus != focusLog {
		t.Fatalf("want focus on log pane, got %v", m.focus)
	}
	if !strings.Contains(m.View(), "hello-from-seeded-log") {
		t.Fatalf("log pane missing the seeded run's content:\n%s", m.View())
	}

	m = send(t, m, special(tea.KeyEsc))
	m = send(t, m, special(tea.KeyEsc))
	if m.focus != focusJobs {
		t.Fatalf("want focus back on jobs pane, got %v", m.focus)
	}

	m = send(t, m, key('e')) // disable
	if !strings.Contains(m.View(), "○ greet") {
		t.Fatalf("expected the disabled marker after 'e':\n%s", m.View())
	}
	m = send(t, m, key('e')) // re-enable
	if !strings.Contains(m.View(), "● greet") {
		t.Fatalf("expected the enabled marker after 'e' again:\n%s", m.View())
	}
}

func TestTUIRunNowExecutesThroughRunner(t *testing.T) {
	st, logsDir := seedStore(t)
	m := newModel(st, logsDir)

	m = send(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = resolve(t, m, m.Init())

	before, err := st.ListRuns(context.Background(), "greet", 200)
	if err != nil {
		t.Fatal(err)
	}

	// "r" must run the job through the exact same runner.Execute/Finish
	// path `jobtail run` uses (PRD §9), not TUI-only logic — checked
	// against the store directly, not against rendered table cell text.
	// send() fully resolves the returned tea.Cmd chain synchronously, so by
	// the time it returns, runNowCmd has already completed and the
	// runFinishedMsg it produced has already updated statusMsg.
	m = send(t, m, key('r'))
	if !strings.Contains(m.statusMsg, "run finished for greet") {
		t.Fatalf("expected a run-finished status message, got %q", m.statusMsg)
	}

	after, err := st.ListRuns(context.Background(), "greet", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("want %d runs after 'r', got %d", len(before)+1, len(after))
	}
	if after[0].Status != "ok" || after[0].Trigger != "manual" {
		t.Fatalf("unexpected new run: %+v", after[0])
	}

	// The periodic refresh (simulated here rather than waited for in real
	// time) is what picks the finished run up into the runs/log panes.
	m = send(t, m, tickMsg(time.Now()))
	if len(m.runs) != len(after) {
		t.Fatalf("runs pane didn't pick up the new run: have %d, want %d", len(m.runs), len(after))
	}
}
