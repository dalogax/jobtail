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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/dalogax/jobtail/internal/store"
)

func init() {
	refreshInterval = time.Millisecond // don't pay tea.Tick's real sleep in tests
}

func seedStore(t testing.TB) (*store.Store, string) {
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
	if err := st.FinishRun(ctx, run.ID, "ok", 0, time.Now(), 250); err != nil {
		t.Fatal(err)
	}
	return st, logsDir
}

// send applies one message and fully resolves whatever tea.Cmd chain it
// triggers (batches unwrapped, nested cmds followed) before returning.
func send(t testing.TB, m model, msg tea.Msg) model {
	t.Helper()
	newM, cmd := m.Update(msg)
	m = newM.(model)
	return resolve(t, m, cmd)
}

func resolve(t testing.TB, m model, cmd tea.Cmd) model {
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

// TestLongLogLineIsWrappedNotCropped is the regression test for a real bug
// found by actually reading a long agent transcript in the TUI: a single
// long paragraph (no internal newlines — a common shape for prose a model
// writes) was silently missing its back half, at every scroll position.
// bubbles/viewport only splits on literal '\n' and horizontally *crops*
// (ansi.Cut) anything wider than its own width rather than wrapping it —
// scrolling can't recover content lost within one logical line. Fixed by
// word-wrapping to the viewport's width before SetContent.
func TestLongLogLineIsWrappedNotCropped(t *testing.T) {
	st, logsDir := seedStore(t)
	ctx := context.Background()
	if err := st.CreateJob(ctx, store.Job{
		ID: "longlog", Kind: "agent", Cron: "0 0 * * *", Timezone: "local",
		Enabled: true, Cwd: t.TempDir(), MaxConcurrent: 1, Keep: 200,
	}); err != nil {
		t.Fatal(err)
	}
	// One long unbroken line, deliberately much wider than any reasonable
	// terminal, ending in a distinctive tail to search for.
	longLine := strings.Repeat("word ", 60) + "FINDME-TAIL-MARKER"
	runID := "long-run-1"
	logPath := filepath.Join(logsDir, runID+".log")
	transcript := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + longLine + `"}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	run, err := st.StartRun(ctx, "longlog", runID, "manual", logPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, run.ID, "ok", 0, time.Now(), 5); err != nil {
		t.Fatal(err)
	}

	m := newModel(st, logsDir)
	m = send(t, m, tea.WindowSizeMsg{Width: 100, Height: 40}) // narrower than longLine
	m = resolve(t, m, m.Init())
	m.selectJobByID("longlog") // seedStore's "greet" sorts first alphabetically otherwise
	m = resolve(t, m, m.reloadRuns("longlog"))
	m = send(t, m, special(tea.KeyEnter)) // jobs -> runs
	m = send(t, m, special(tea.KeyEnter)) // runs -> log

	// The tail must be reachable by scrolling — proving the line actually
	// wrapped into multiple viewport lines instead of being cropped once.
	m.logVP.GotoBottom()
	if !strings.Contains(m.logVP.View(), "FINDME-TAIL-MARKER") {
		t.Fatalf("long line's tail was lost — cropped instead of wrapped:\n%s", m.logVP.View())
	}
}

// TestArrowKeysMoveTheCursorInEveryListPane is the regression test for a bug
// the help bar was advertising the whole time: bubbles/table's Update returns
// immediately unless the table is focused, only the jobs table ever was, and
// nothing called Focus when the pane changed — so ↑/↓ in the runs pane did
// nothing at all. The mouse hid it, because the wheel and click paths call
// MoveUp/MoveDown/SetCursor directly instead of going through table.Update.
func TestArrowKeysMoveTheCursorInEveryListPane(t *testing.T) {
	st, logsDir := seedStore(t)
	addJobs(t, st, "worker", 3)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		runID := fmt.Sprintf("extra-run-%d", i)
		logPath := filepath.Join(logsDir, runID+".log")
		if err := os.WriteFile(logPath, []byte(runID+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run, err := st.StartRun(ctx, "greet", runID, "scheduled", logPath,
			time.Now().Add(time.Duration(i+1)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.FinishRun(ctx, run.ID, "ok", 0, time.Now(), 10); err != nil {
			t.Fatal(err)
		}
	}

	m := modelAt(t, st, logsDir, 172, 40)
	if got := m.jobsTable.Cursor(); got != 0 {
		t.Fatalf("jobs cursor should start at 0, got %d", got)
	}
	m = send(t, m, special(tea.KeyDown))
	if got := m.jobsTable.Cursor(); got != 1 {
		t.Errorf("down in the jobs pane left the cursor at %d, want 1", got)
	}
	m = send(t, m, special(tea.KeyUp))
	if got := m.jobsTable.Cursor(); got != 0 {
		t.Errorf("up in the jobs pane left the cursor at %d, want 0", got)
	}

	m = send(t, m, special(tea.KeyEnter)) // jobs -> runs
	m = send(t, m, special(tea.KeyDown))
	if got := m.runsTable.Cursor(); got != 1 {
		t.Errorf("down in the runs pane left the cursor at %d, want 1", got)
	}
	m = send(t, m, special(tea.KeyUp))
	if got := m.runsTable.Cursor(); got != 0 {
		t.Errorf("up in the runs pane left the cursor at %d, want 0", got)
	}

	// Going back up a level must hand the keys back to the jobs table.
	m = send(t, m, special(tea.KeyEsc))
	m = send(t, m, special(tea.KeyDown))
	if got := m.jobsTable.Cursor(); got != 1 {
		t.Errorf("after esc, down in the jobs pane left the cursor at %d, want 1", got)
	}
}

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
	// The log title must identify what we are looking at: job name, local
	// start time, run id. A bare "Log" loses the job↔run association the
	// moment more than one job exists.
	if !strings.Contains(m.View(), "Log greet · ") {
		t.Fatalf("log title missing job name:\n%s", m.View())
	}
	if !strings.Contains(m.View(), "seed-run-1") {
		t.Fatalf("log title missing run id:\n%s", m.View())
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

	// Row chrome is 2 lines — the box's top border, then the table header —
	// because the pane title lives inside the border rule rather than on a
	// content row of its own. So Y=2 is the first data row and Y=3 the
	// second, which is "greet" once jobs are sorted alphabetically.
	m = send(t, m, tea.MouseMsg{X: 5, Y: 3, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if got := m.selectedJobID(); got != "greet" {
		t.Fatalf("click should have selected 'greet' (2nd row), got %q", got)
	}

	// The first data row must be clickable at all: while rowAtY still
	// assumed a title row, Y=2 mapped to index -1 and was discarded, so
	// the top entry in every table simply could not be selected by mouse.
	m = send(t, m, tea.MouseMsg{X: 5, Y: 2, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if got := m.selectedJobID(); got != "abbey" {
		t.Fatalf("clicking the first data row should select 'abbey', got %q", got)
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
	m = send(t, m, tea.MouseMsg{X: 5, Y: m.topBoxHeight + 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
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
	// Titles are embedded in each box's own top border line (row 0), not a
	// separate content row, so row 1 (an interior content row) still shows
	// the same left/mid/right vertical border characters a title row used
	// to — pane borders run down every row of the box, title or not.
	contentRow := []rune(ansiRE.ReplaceAllString(lines[1], ""))

	borderCols := []int{}
	for i, r := range contentRow {
		if r == '│' {
			borderCols = append(borderCols, i)
		}
	}
	// Expect 4 on the top row now that log isn't beside it: jobs' left
	// edge, the adjacent pair where jobs' right border meets runs' left
	// border, and runs' own right edge.
	if len(borderCols) != 4 {
		t.Fatalf("expected 4 vertical border characters on the top row (left edge + adjacent pair + right edge), found %d: %q",
			len(borderCols), string(contentRow))
	}
	if d := abs(borderCols[1] - m.jobsBoxWidth); d > 1 {
		t.Fatalf("jobs|runs border rendered at column %d, but jobsBoxWidth=%d (mouse clicks there would hit the wrong pane)",
			borderCols[1], m.jobsBoxWidth)
	}

	// Row topBoxHeight is the log box's own top border, with its title
	// embedded directly in that same rule (renderPane/embedTitle) — no
	// separate title row beneath it anymore.
	if m.topBoxHeight >= len(lines) {
		t.Fatalf("topBoxHeight=%d is past the end of the rendered view (%d lines)", m.topBoxHeight, len(lines))
	}
	logBorderRow := ansiRE.ReplaceAllString(lines[m.topBoxHeight], "")
	if !strings.ContainsAny(logBorderRow, "╭╮╰╯─") {
		t.Fatalf("expected the log pane's rounded top border at row topBoxHeight=%d, got: %q", m.topBoxHeight, logBorderRow)
	}
	if !strings.Contains(logBorderRow, "Log") {
		t.Fatalf("expected the log pane's title embedded in its top border at row topBoxHeight=%d, got: %q", m.topBoxHeight, logBorderRow)
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
	for _, size := range []struct{ w, h int }{
		{172, 40}, {120, 40}, {100, 30}, {90, 40}, {80, 24},
		{70, 20}, {60, 20}, {50, 24}, {45, 24}, {40, 20}, {30, 20}, {24, 12},
	} {
		m := newModel(st, logsDir)
		m = send(t, m, tea.WindowSizeMsg{Width: size.w, Height: size.h})
		m = resolve(t, m, m.Init())

		// No single box may be wider than the terminal, in any layout.
		for _, box := range []struct {
			name  string
			width int
		}{
			{"jobs", m.jobsBoxWidth},
			{"runs", m.runsBoxWidth},
			{"log", m.logVP.Width + 4},
		} {
			if box.width > size.w {
				t.Fatalf("at %dx%d: %s box is %d wide, wider than the terminal",
					size.w, size.h, box.name, box.width)
			}
		}
		// Side by side only in the wide layout; the others stack, where
		// each box legitimately spans the full width on its own row.
		if m.mode == layoutWide {
			if got := m.jobsBoxWidth + m.runsBoxWidth; got > size.w {
				t.Fatalf("at %dx%d: jobs+runs share a row but total %d, wider than the terminal",
					size.w, size.h, got)
			}
		}

		// The rendered frame itself must also fit, which is the thing the
		// user actually sees — box arithmetic can be right while the
		// render still spills (that mismatch is the bug this guards).
		for i, line := range strings.Split(m.View(), "\n") {
			if w := lipgloss.Width(line); w > size.w {
				t.Fatalf("at %dx%d: rendered line %d is %d columns wide, past the right edge: %q",
					size.w, size.h, i, w, ansiRE.ReplaceAllString(line, ""))
			}
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
	// Y must land inside the jobs pane, which is now sized to its content
	// (two jobs here) rather than a fixed fraction of the screen — a wheel
	// event further down belongs to the log pane and scrolls that instead.
	m = send(t, m, tea.MouseMsg{X: 5, Y: 2, Button: tea.MouseButtonWheelDown})
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
