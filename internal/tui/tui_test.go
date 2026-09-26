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
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dalogax/jobtail/internal/store"
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

	m = send(t, m, tea.WindowSizeMsg{Width: 172, Height: 40})
	m = resolve(t, m, m.Init())

	if !strings.Contains(m.View(), "greet") {
		t.Fatalf("initial view missing job id 'greet':\n%s", m.View())
	}
	if !strings.Contains(m.View(), "● greet") {
		t.Fatalf("expected the enabled marker on greet:\n%s", m.View())
	}
	// The jobs pane's last-status cell is its rightmost column — exactly
	// what a viewport-width miscalculation clips first without an obvious
	// symptom elsewhere (found only by actually screenshotting the TUI).
	// Assert on it explicitly so that class of regression fails here.
	if !strings.Contains(m.View(), "ok") {
		t.Fatalf("jobs pane missing the last-status column value ('ok'):\n%s", m.View())
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

// addSecondJob gives seedStore's single "greet" job company, alphabetically
// first, so mouse tests have >1 row to click between.
func addSecondJob(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.CreateJob(context.Background(), store.Job{
		ID: "abbey", Kind: "cli", Cron: "0 0 * * *", Timezone: "local",
		Enabled: true, Cwd: t.TempDir(), Command: "true", MaxConcurrent: 1, Keep: 200,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMouseClickSelectsRowAndSwitchesPane(t *testing.T) {
	st, logsDir := seedStore(t)
	addSecondJob(t, st)
	m := newModel(st, logsDir)
	m = send(t, m, tea.WindowSizeMsg{Width: 172, Height: 40})
	m = resolve(t, m, m.Init())

	if got := m.selectedJobID(); got != "abbey" {
		t.Fatalf("expected 'abbey' (alphabetically first) selected by default, got %q", got)
	}

	// Row chrome is 3 lines (border + title + header); Y=4 is the second
	// data row, which is "greet" once jobs are sorted alphabetically.
	m = send(t, m, tea.MouseMsg{X: 5, Y: 4, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if got := m.selectedJobID(); got != "greet" {
		t.Fatalf("click should have selected 'greet' (2nd row), got %q", got)
	}
	if m.focus != focusJobs {
		t.Fatalf("clicking in the jobs pane's x-range should focus it, got %v", m.focus)
	}

	// Use the model's own computed box widths, not an independently
	// guessed third of the screen — jobs and runs have different column
	// counts and so different real widths (this is exactly the bug that
	// broke real mouse clicks: the boundary math and the render math
	// disagreed with each other).
	m = send(t, m, tea.MouseMsg{X: m.jobsBoxWidth + 5, Y: 4, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.focus != focusRuns {
		t.Fatalf("clicking in the runs pane's x-range should focus it, got %v", m.focus)
	}

	// Log spans the full width below jobs/runs now (not a third column to
	// the right of them) — a click there needs Y past topBoxHeight, X is
	// irrelevant.
	m = send(t, m, tea.MouseMsg{X: 5, Y: m.topBoxHeight + 2, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.focus != focusLog {
		t.Fatalf("clicking below the top row should focus the log pane, got %v", m.focus)
	}
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

// TestMouseBoundariesMatchActualRenderedBorders is the regression test for
// the real bug this session found live: handleMouse computed pane
// boundaries from an even width/3 split, while the renderer gave jobs and
// runs their own widths based on column count (different for each table).
// The two disagreed, so clicks landing in the visual gap between them
// resolved to the wrong pane — invisible to every other test here because
// none of them cross-checked hit-testing math against the rendered
// output. This parses m.View() itself and asserts the border/title
// positions really do sit at jobsBoxWidth (horizontally, top row) and
// topBoxHeight (vertically, where the log box begins).
func TestMouseBoundariesMatchActualRenderedBorders(t *testing.T) {
	st, logsDir := seedStore(t)
	m := newModel(st, logsDir)
	m = send(t, m, tea.WindowSizeMsg{Width: 172, Height: 40})
	m = resolve(t, m, m.Init())

	lines := strings.Split(m.View(), "\n")
	if len(lines) < 2 {
		t.Fatalf("view has too few lines: %d", len(lines))
	}
	titleRow := []rune(ansiRE.ReplaceAllString(lines[1], "")) // "Jobs ... Runs: ..." (log is a separate row now)

	borderCols := []int{}
	for i, r := range titleRow {
		if r == '│' {
			borderCols = append(borderCols, i)
		}
	}
	// Expect 4 on the top row now that log isn't beside it: jobs' left
	// edge, the adjacent pair where jobs' right border meets runs' left
	// border, and runs' own right edge.
	if len(borderCols) != 4 {
		t.Fatalf("expected 4 vertical border characters on the top row (left edge + adjacent pair + right edge), found %d: %q",
			len(borderCols), string(titleRow))
	}
	if d := abs(borderCols[1] - m.jobsBoxWidth); d > 1 {
		t.Fatalf("jobs|runs border rendered at column %d, but jobsBoxWidth=%d (mouse clicks there would hit the wrong pane)",
			borderCols[1], m.jobsBoxWidth)
	}

	// Row topBoxHeight is the log box's own top border; topBoxHeight+1 is
	// its title line (same one-row offset the top boxes have at line 1).
	if m.topBoxHeight+1 >= len(lines) {
		t.Fatalf("topBoxHeight=%d is past the end of the rendered view (%d lines)", m.topBoxHeight, len(lines))
	}
	logBorderRow := ansiRE.ReplaceAllString(lines[m.topBoxHeight], "")
	if !strings.ContainsAny(logBorderRow, "┌┬┐─") {
		t.Fatalf("expected the log pane's top border at row topBoxHeight=%d, got: %q", m.topBoxHeight, logBorderRow)
	}
	logTitleRow := ansiRE.ReplaceAllString(lines[m.topBoxHeight+1], "")
	if !strings.Contains(logTitleRow, "Log") {
		t.Fatalf("expected the log pane's title at row topBoxHeight+1=%d, got: %q", m.topBoxHeight+1, logTitleRow)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func TestMouseClickOutOfRangeIsIgnoredNotACrash(t *testing.T) {
	st, logsDir := seedStore(t)
	m := newModel(st, logsDir)
	m = send(t, m, tea.WindowSizeMsg{Width: 172, Height: 40})
	m = resolve(t, m, m.Init())

	before := m.selectedJobID()
	// Far below any real row (only 1 seeded job).
	m = send(t, m, tea.MouseMsg{X: 5, Y: 30, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if got := m.selectedJobID(); got != before {
		t.Fatalf("an out-of-range click should not change the selection, got %q want %q", got, before)
	}
}

// TestNarrowTerminalDoesNotOverflow is the regression test for a real
// reported bug: on a narrow ("phone aspect ratio") terminal, the log pane
// correctly shrank to match, but the jobs/runs row on top didn't — because
// jobsColumns/runsColumns each had a hardcoded floor ("remaining < 20 ->
// 20", "startedW < 12 -> 12") that kept their declared total width wider
// than whatever the terminal actually had, regardless of how narrow it
// got. Checked down to 40 columns, a realistic narrow-phone-terminal
// width; below roughly 35 there's a genuine structural floor (5+4
// distinctly-labeled columns each need at least 1 char plus their own
// padding and box chrome), not a bug to chase — that's narrower than any
// real terminal app is likely to run at.
func TestNarrowTerminalDoesNotOverflow(t *testing.T) {
	st, logsDir := seedStore(t)
	for _, width := range []int{172, 90, 60, 45, 40} {
		m := newModel(st, logsDir)
		m = send(t, m, tea.WindowSizeMsg{Width: width, Height: 90})
		m = resolve(t, m, m.Init())

		if got := m.jobsBoxWidth + m.runsBoxWidth; got > width {
			t.Fatalf("at terminal width %d: jobs+runs boxes total %d, wider than the terminal itself", width, got)
		}
		if m.logVP.Width+4 > width {
			t.Fatalf("at terminal width %d: log box width %d, wider than the terminal itself", width, m.logVP.Width+4)
		}
	}
}

func TestMouseWheelMovesJobSelection(t *testing.T) {
	st, logsDir := seedStore(t)
	addSecondJob(t, st)
	m := newModel(st, logsDir)
	m = send(t, m, tea.WindowSizeMsg{Width: 172, Height: 40})
	m = resolve(t, m, m.Init())

	if got := m.selectedJobID(); got != "abbey" {
		t.Fatalf("expected 'abbey' selected by default, got %q", got)
	}
	m = send(t, m, tea.MouseMsg{X: 5, Y: 10, Button: tea.MouseButtonWheelDown})
	if got := m.selectedJobID(); got != "greet" {
		t.Fatalf("wheel-down should move selection to 'greet', got %q", got)
	}
}

func TestTUIRunNowExecutesThroughRunner(t *testing.T) {
	st, logsDir := seedStore(t)
	m := newModel(st, logsDir)

	m = send(t, m, tea.WindowSizeMsg{Width: 172, Height: 40})
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
