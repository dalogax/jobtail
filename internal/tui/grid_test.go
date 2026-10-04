package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/dalogax/jobtail/internal/store"
)

func testGrid(n int) grid {
	g := newGrid([]table.Column{{Title: "ID", Width: 8}, {Title: "Status", Width: 6}}, accentJobs, true)
	rows := make([]table.Row, n)
	for i := range rows {
		rows[i] = table.Row{fmt.Sprintf("job-%02d", i), "ok"}
	}
	g.SetRows(rows)
	g.SetWidth(columnsWidth(g.Columns()) + gridGutter)
	return g
}

// Under NO_COLOR lipgloss emits no SGR at all — not even bold or reverse —
// so a selection drawn only with styling disappears. The cursor glyph is
// what still says which row the keys act on.
func TestCursorRowIsMarkedWithoutColor(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	g := testGrid(3)
	g.SetHeight(4)
	g.SetCursor(1)
	lines := strings.Split(g.View(), "\n")
	for i, l := range lines[1:] {
		marked := strings.HasPrefix(l, cursorGlyph)
		if marked != (i == 1) {
			t.Errorf("row %d marked=%v, want only row 1 marked:\n%s", i, marked, g.View())
		}
	}
}

// The jobs and runs panes each keep a selected row, but only the focused
// one is where the keys go. bubbles/table drew both the same pink, so the
// screen showed two identical cursors.
func TestOnlyTheFocusedGridHasTheLiveSelection(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	g := testGrid(2)
	g.SetHeight(3)
	focused := strings.Split(g.View(), "\n")[1]
	g.Blur()
	blurred := strings.Split(g.View(), "\n")[1]
	if focused == blurred {
		t.Fatal("the cursor row looks the same focused and unfocused")
	}
	live := lipgloss.NewStyle().Background(selectionBG).Render("x")
	live = live[:strings.Index(live, "x")]
	if !strings.Contains(focused, live) || strings.Contains(blurred, live) {
		t.Errorf("only the focused grid should use the live selection fill:\nfocused %q\nblurred %q", focused, blurred)
	}
}

// A colored cell inside the selected row must not end the row's background
// band: the cell's reset is followed by the band again.
func TestSelectionBandSurvivesAColoredCell(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	g := testGrid(1)
	g.SetRows([]table.Row{{"job-00", statusCell("failed", 6)}})
	g.SetHeight(2)
	row := strings.Split(g.View(), "\n")[1]
	band := lipgloss.NewStyle().Background(selectionBG).Render("x")
	band = band[:strings.Index(band, "x")]
	if !strings.Contains(row, "\x1b[0m"+band) {
		t.Errorf("the band is not re-opened after the status cell's reset: %q", row)
	}
}

func TestGridScrollsToKeepTheCursorVisible(t *testing.T) {
	g := testGrid(10)
	g.SetHeight(4) // header + 3 rows
	for range 5 {
		g, _ = g.Update(special(tea.KeyDown))
	}
	if g.Cursor() != 5 || g.Offset() != 3 {
		t.Fatalf("cursor %d offset %d, want 5 and 3", g.Cursor(), g.Offset())
	}
	if !strings.Contains(g.View(), "job-05") || strings.Contains(g.View(), "job-02") {
		t.Errorf("the window should hold rows 3-5:\n%s", g.View())
	}
	g, _ = g.Update(key('g'))
	if g.Cursor() != 0 || g.Offset() != 0 {
		t.Errorf("g should go to the top, got cursor %d offset %d", g.Cursor(), g.Offset())
	}
	g, _ = g.Update(key('G'))
	if g.Cursor() != 9 || g.Offset() != 7 {
		t.Errorf("G should go to the bottom, got cursor %d offset %d", g.Cursor(), g.Offset())
	}
	g.Blur()
	g, _ = g.Update(key('g'))
	if g.Cursor() != 9 {
		t.Error("an unfocused grid must ignore keys")
	}
}

// A click on the first visible row of a scrolled table is that row, not
// row 0. bubbles/table had no offset getter, so clicks on a scrolled list
// used to land on the wrong job.
func TestClickInAScrolledJobsTableHitsTheVisibleRow(t *testing.T) {
	st, logsDir := seedStore(t)
	addJobs(t, st, "worker", 20)
	m := modelAt(t, st, logsDir, 100, 24)
	for range 15 {
		m = send(t, m, special(tea.KeyDown))
	}
	off := m.jobsTable.Offset()
	if off == 0 {
		t.Fatal("precondition: the jobs table should have scrolled")
	}
	m = send(t, m, tea.MouseMsg{X: 4, Y: 2, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = send(t, m, tea.MouseMsg{X: 4, Y: 2, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	if got := m.jobsTable.Cursor(); got != off {
		t.Errorf("clicked the first visible row (%d) but selected %d", off, got)
	}
}

func TestHelpOverlayOpensAndClosesAndQuitStillQuits(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 80, 24)
	m = send(t, m, key('?'))
	view := plain(m.View())
	for _, want := range []string{"Keys", "run now", "step through tool calls", "select & copy"} {
		if !strings.Contains(view, want) {
			t.Errorf("help should list %q:\n%s", want, view)
		}
	}
	if lines := strings.Split(view, "\n"); len(lines) > 24 {
		t.Errorf("help is %d lines on a 24-line terminal", len(lines))
	}
	m = send(t, m, special(tea.KeyEsc))
	if m.showHelp {
		t.Fatal("esc should close help")
	}
	m = send(t, m, key('?'))
	if _, cmd := m.Update(key('q')); cmd == nil {
		t.Fatal("q should still quit from the help screen")
	}
}

// In the log pane the fold keys are what the pane is for; they used to be
// the first hints dropped, so a 60-column bar kept "drag copy" and lost
// "enter expand".
func TestLogFooterKeepsTheFoldKeysOverGenericOnes(t *testing.T) {
	bar := plain(renderHints(60, "", append(hintsFor(focusLog, true, true),
		helpHint{key: "drag", long: "select & copy", short: "copy", drop: 9})))
	for _, want := range []string{"enter", "jk", "esc", "quit", "help"} {
		if !strings.Contains(bar, want) {
			t.Errorf("60-column log footer lost %q: %q", want, bar)
		}
	}
	if strings.Contains(bar, "drag") {
		t.Errorf("drag should drop before the fold keys: %q", bar)
	}
	if !strings.HasSuffix(strings.TrimRight(bar, " "), "q quit") {
		t.Errorf("global keys belong at the right edge: %q", bar)
	}
}

func TestExitCodeShownOnlyWhenNonZero(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 172, 40)
	runsRow := plain(strings.Split(m.runsTable.View(), "\n")[1])
	if strings.Contains(runsRow, " 0 ") || strings.HasSuffix(strings.TrimSpace(runsRow), "0") {
		t.Errorf("an ok run should not repeat its 0 exit code: %q", runsRow)
	}
}

func TestRunningRunShowsElapsedTime(t *testing.T) {
	r := store.Run{Status: "running", StartedAt: time.Now().Add(-12 * time.Second)}
	r.DurationMs.Valid = true // what StartRun leaves behind
	if got := runDuration(r); got != "12s" {
		t.Errorf("runDuration of a run 12s in = %q, want 12s", got)
	}
}

// The run list doesn't change while a run is in progress, so the reload
// that would rebuild its rows is skipped; the tick redraws them instead, or
// the elapsed time would freeze at whatever it said when the run started.
func TestRunningDurationAdvancesOnTick(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 172, 40)
	m.runs[0].Status = "running"
	m.runs[0].StartedAt = time.Now().Add(-5 * time.Second)
	m.runsTable.SetRows(m.groupedRunRows(m.runsCols))
	before := plain(m.runsTable.View())
	m.runs[0].StartedAt = time.Now().Add(-9 * time.Second)
	next, _ := m.Update(tickMsg(time.Now())) // the tick alone, not the reloads it schedules
	after := plain(next.(model).runsTable.View())
	if !strings.Contains(before, "5s") || !strings.Contains(after, "9s") {
		t.Errorf("elapsed time did not advance on tick:\nbefore %s\nafter  %s", before, after)
	}
}
