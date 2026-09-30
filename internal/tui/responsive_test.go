// Tests for the dashboard's responsive behavior: what it shows, and what it
// refuses to show, as the terminal gets smaller.
//
// These exist because the failures they cover were all invisible to the
// existing tests — the TUI kept "working" (no crash, no overflow, correct
// box arithmetic) while rendering panes made entirely of ellipses, a help
// bar cut off mid-word, and tables whose first row couldn't be clicked.
// Asserting on the rendered frame is the only way to catch that class.
package tui

import (
	"context"
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

// plain strips ANSI so assertions read the text a user would see.
func plain(s string) string { return ansiRE.ReplaceAllString(s, "") }

func modelAt(t testing.TB, st *store.Store, logsDir string, w, h int) model {
	t.Helper()
	m := newModel(st, logsDir)
	m = send(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	return resolve(t, m, m.Init())
}

// addJobs seeds n extra jobs with the given id prefix.
func addJobs(t testing.TB, st *store.Store, prefix string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if err := st.CreateJob(ctx, store.Job{
			ID: fmt.Sprintf("%s-%02d", prefix, i), Kind: "cli", Cron: "*/5 * * * *",
			Timezone: "local", Enabled: true, Cwd: t.TempDir(), Command: "echo hi",
			MaxConcurrent: 1, Keep: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNoPaneRendersAsPureEllipses is the regression test for the failure
// that prompted this work: below about 100 columns every column got a
// proportional share of a width that couldn't support them, so the jobs
// pane rendered "● ha-fr…  c…  …  09…  ok" — five columns of nothing. A
// column that can't show its content should leave, not starve its
// neighbors.
func TestNoPaneRendersAsPureEllipses(t *testing.T) {
	st, logsDir := seedStore(t)
	addJobs(t, st, "worker", 4)

	for _, size := range []struct{ w, h int }{
		{172, 40}, {120, 30}, {100, 30}, {80, 24}, {60, 20}, {50, 24}, {40, 20}, {30, 20},
	} {
		m := modelAt(t, st, logsDir, size.w, size.h)
		for i, line := range strings.Split(plain(m.View()), "\n") {
			// A line whose content is mostly ellipsis characters means the
			// columns were squeezed past the point of conveying anything.
			body := strings.Trim(line, "│╭╮╰╯─ ")
			if body == "" {
				continue
			}
			if dots := strings.Count(body, "…"); dots >= 3 {
				t.Errorf("at %dx%d line %d is mostly ellipses (%d of them), i.e. unreadable: %q",
					size.w, size.h, i, dots, line)
			}
		}
	}
}

// TestJobIDStaysReadable checks the thing the jobs pane exists to tell you.
// Truncating a name to "● ha-fr…" makes two jobs indistinguishable; the ID
// column is the one that must keep its width as others are dropped.
func TestJobIDStaysReadable(t *testing.T) {
	st, logsDir := seedStore(t)
	for _, size := range []struct{ w, h int }{{80, 24}, {60, 20}, {40, 20}, {30, 20}} {
		m := modelAt(t, st, logsDir, size.w, size.h)
		if !strings.Contains(plain(m.View()), "greet") {
			t.Errorf("at %dx%d the job id 'greet' is not shown in full:\n%s",
				size.w, size.h, plain(m.View()))
		}
	}
}

// TestHelpBarAlwaysShowsHowToQuit covers a real dead end: Bubble Tea crops
// each rendered line to the terminal width, so on an 80-column terminal the
// fixed help bar lost "q quit" entirely and nothing on screen said how to
// get out.
func TestHelpBarAlwaysShowsHowToQuit(t *testing.T) {
	st, logsDir := seedStore(t)
	for _, size := range []struct{ w, h int }{
		{172, 40}, {100, 30}, {80, 24}, {60, 20}, {40, 20}, {30, 20}, {24, 12}, {20, 10},
	} {
		m := modelAt(t, st, logsDir, size.w, size.h)
		lines := strings.Split(m.View(), "\n")
		bar := lines[len(lines)-1]
		if !strings.Contains(plain(bar), "quit") {
			t.Errorf("at %dx%d the help bar doesn't say how to quit: %q", size.w, size.h, plain(bar))
		}
		if w := lipgloss.Width(bar); w > size.w {
			t.Errorf("at %dx%d the help bar is %d columns wide and would be cropped: %q",
				size.w, size.h, w, plain(bar))
		}
	}
}

// TestHelpBarOnlyAdvertisesKeysThatWork: "e" and "r" act on the jobs table
// only (handleKey ignores them elsewhere), so offering them while reading a
// log was a promise the UI didn't keep.
func TestHelpBarOnlyAdvertisesKeysThatWork(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 172, 40)

	bar := func(m model) string {
		lines := strings.Split(m.View(), "\n")
		return plain(lines[len(lines)-1])
	}
	if got := bar(m); !strings.Contains(got, "run now") || !strings.Contains(got, "enable/disable") {
		t.Fatalf("jobs pane should offer r/e, got %q", got)
	}
	m = send(t, m, key('l')) // -> runs
	m = send(t, m, key('l')) // -> log
	if m.focus != focusLog {
		t.Fatalf("expected log focus, got %v", m.focus)
	}
	got := bar(m)
	for _, unusable := range []string{"run now", "enable/disable"} {
		if strings.Contains(got, unusable) {
			t.Errorf("log pane advertises %q, which does nothing there: %q", unusable, got)
		}
	}
	if !strings.Contains(got, "back") {
		t.Errorf("log pane should offer the key that gets you out of it, got %q", got)
	}
}

// TestNarrowLayoutShowsOnlyTheFocusedPane: on a phone-sized terminal three
// panes can't all be legible, so the dashboard follows its own drill-down
// and draws one level at a time.
func TestNarrowLayoutShowsOnlyTheFocusedPane(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 40, 20)
	if m.mode != layoutFocused {
		t.Fatalf("expected the single-pane layout at 40x20, got mode %v", m.mode)
	}

	view := plain(m.View())
	if !strings.Contains(view, "Jobs") {
		t.Fatalf("jobs pane should be on screen:\n%s", view)
	}
	if strings.Contains(view, "Log") {
		t.Errorf("only the focused pane should be drawn, but the log pane is too:\n%s", view)
	}

	m = send(t, m, key('l'))
	m = send(t, m, key('l'))
	view = plain(m.View())
	if !strings.Contains(view, "Log") {
		t.Fatalf("after drilling in twice the log pane should be on screen:\n%s", view)
	}
	if strings.Contains(view, "Jobs") {
		t.Errorf("the jobs pane should no longer be drawn:\n%s", view)
	}
	// The back-arrow is the only affordance saying esc goes up a level.
	if !strings.Contains(view, "◂") {
		t.Errorf("a drilled-in pane should show the back affordance in its title:\n%s", view)
	}
}

// TestListPanesShowHowManyRowsExist: with 21 jobs and room for 6, nothing
// in the old UI hinted the other 15 were there.
func TestListPanesShowHowManyRowsExist(t *testing.T) {
	st, logsDir := seedStore(t)
	addJobs(t, st, "filler", 20)

	m := modelAt(t, st, logsDir, 80, 24)
	view := plain(m.View())
	if !strings.Contains(view, "1/21") {
		t.Errorf("the jobs pane should say which row of how many is selected:\n%s", view)
	}
}

// TestPanesSizeToContentNotToAFixedFraction: the old layout gave the top
// row a flat 40% whatever it held, so a 21-job list showed six entries
// while an empty log pane sat below it.
func TestPanesSizeToContentNotToAFixedFraction(t *testing.T) {
	st, logsDir := seedStore(t) // 1 job
	few := modelAt(t, st, logsDir, 172, 40)

	addJobs(t, st, "filler", 20)
	many := modelAt(t, st, logsDir, 172, 40)

	if many.topBoxHeight <= few.topBoxHeight {
		t.Errorf("a 21-job list should claim more rows than a 1-job list, got %d vs %d",
			many.topBoxHeight, few.topBoxHeight)
	}
	if few.logVP.Height <= many.logVP.Height {
		t.Errorf("with fewer jobs the log should get the freed rows, got %d vs %d",
			few.logVP.Height, many.logVP.Height)
	}
}

// TestEmptyDatabaseTellsYouWhatToDo: an empty database is the first thing a
// new user sees, and it used to be three blank boxes.
func TestEmptyDatabaseTellsYouWhatToDo(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/jobtail.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	for _, size := range []struct{ w, h int }{{172, 40}, {100, 30}, {80, 24}, {40, 20}} {
		m := modelAt(t, st, dir, size.w, size.h)
		view := plain(m.View())
		if !strings.Contains(view, "No jobs yet") {
			t.Errorf("at %dx%d an empty database should say so:\n%s", size.w, size.h, view)
		}
		if !strings.Contains(view, "jobtail add") {
			t.Errorf("at %dx%d it should name the command that fixes it:\n%s", size.w, size.h, view)
		}
	}
}

// TestStatusColorNeverCorruptsTheCell is the guard on colorCell. bubbles/
// table truncates with a non-ANSI-aware width, so a colored cell in a
// column too narrow for its escape bytes gets cut mid-sequence: the text is
// mangled and the lost reset bleeds the color across the rest of the row.
func TestStatusColorNeverCorruptsTheCell(t *testing.T) {
	st, logsDir := seedStore(t)
	ctx := context.Background()
	// A failed run gives the jobs pane a status worth coloring.
	runID := "fail-run"
	if _, err := st.StartRun(ctx, "greet", runID, "manual", logsDir+"/"+runID+".log", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, runID, "failed", 1, time.Now(), 10); err != nil {
		t.Fatal(err)
	}

	for _, size := range []struct{ w, h int }{{172, 40}, {120, 30}, {80, 24}, {60, 20}, {40, 20}, {30, 20}} {
		m := modelAt(t, st, logsDir, size.w, size.h)
		view := m.View()
		// A truncated escape leaves a bare "\x1b[" or an SGR introducer with
		// no final byte — either means a cell was cut mid-sequence.
		for _, frag := range []string{"\x1b[…", "\x1b[3…", "\x1b[38…"} {
			if strings.Contains(view, frag) {
				t.Errorf("at %dx%d a status cell was truncated mid-escape (%q): color would bleed",
					size.w, size.h, frag)
			}
		}
		// Whatever the width, the status text itself must survive.
		if !strings.Contains(plain(view), "fail") {
			t.Errorf("at %dx%d the failed status is not shown at all:\n%s", size.w, size.h, plain(view))
		}
	}
}

// TestStatusIsColoredWhenThereIsRoom: the styles existed but nothing ever
// called statusStyle, so the dashboard's single most important signal —
// did it fail? — rendered in the same plain text as everything else.
func TestStatusIsColoredWhenThereIsRoom(t *testing.T) {
	// Under `go test` there's no TTY, so lipgloss picks the Ascii profile
	// and renders every style as plain text — which would make this test
	// pass vacuously. Force a real profile so the escapes actually appear.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	cols := jobsColumns(120)
	w := columnWidth(cols, "Status")
	if w == 0 {
		t.Fatal("a 120-column pane should keep the Status column")
	}
	got := colorCell("failed", statusFailed, w)
	if !strings.Contains(got, "\x1b[") {
		t.Errorf("at status width %d the cell should carry color, got %q", w, got)
	}
	// And at a width that can't hold the escapes, it must fall back to
	// plain text rather than emitting something the table will corrupt.
	if got := colorCell("failed", statusFailed, 6); got != "failed" {
		t.Errorf("a narrow status column should render plain text, got %q", got)
	}
}

// TestExtraColumnsAppearOnlyWhenThereIsRoom covers the other half of the
// brief: use spare width for more information, not more padding.
func TestExtraColumnsAppearOnlyWhenThereIsRoom(t *testing.T) {
	narrow := jobsColumns(40)
	if columnWidth(narrow, "Cron") != 0 {
		t.Errorf("a 40-column pane has no room for the Cron column: %v", narrow)
	}
	wide := jobsColumns(110)
	if columnWidth(wide, "Cron") == 0 {
		t.Errorf("a 110-column pane should show the Cron column: %v", wide)
	}
	if columnWidth(runsColumns(110), "Exit") == 0 {
		t.Errorf("a wide runs pane should show the Exit column: %v", runsColumns(110))
	}
}

// TestColumnsNeverExceedTheirPane: fitColumns hands widths to a table that
// then renders inside a fixed box, so the sum has to be exact. An earlier
// per-column minimum broke this and pushed the panes past the right edge.
func TestColumnsNeverExceedTheirPane(t *testing.T) {
	for w := 6; w <= 200; w++ {
		for _, tc := range []struct {
			name string
			cols []table.Column
		}{
			{"jobs", jobsColumns(w)},
			{"runs", runsColumns(w)},
		} {
			if got := columnsWidth(tc.cols); got > w {
				t.Fatalf("%s columns total %d at pane content width %d", tc.name, got, w)
			}
			for _, c := range tc.cols {
				if c.Width < 1 {
					t.Fatalf("%s column %q got width %d at pane content width %d",
						tc.name, c.Title, c.Width, w)
				}
			}
		}
	}
}

// TestEveryPaneTitleSurvivesNarrowTerminals: embedTitle used to bail out
// when the title didn't fit, leaving a blank top border — at 50 columns the
// runs pane had no title, so nothing said whose runs were listed.
func TestEveryPaneTitleSurvivesNarrowTerminals(t *testing.T) {
	st, logsDir := seedStore(t)
	for _, size := range []struct{ w, h int }{{172, 40}, {100, 30}, {80, 24}, {60, 20}, {50, 24}, {40, 20}, {30, 20}} {
		m := modelAt(t, st, logsDir, size.w, size.h)
		first := plain(strings.Split(m.View(), "\n")[0])
		if !strings.Contains(first, "Jobs") {
			t.Errorf("at %dx%d the first pane's title is missing from its border: %q",
				size.w, size.h, first)
		}
	}
}

// TestResizingAPopulatedDashboardNeverPanics is the regression test for a
// crash that took the whole program down mid-session: shrinking the terminal
// far enough to drop a table column panicked inside bubbles/table with
// "index out of range [5] with length 5", and Bubble Tea's recover printed a
// stack trace over the user's terminal and exited.
//
// Every earlier size test built a *fresh* model per size, so none of them
// ever resized a dashboard that already had rows in it — which is the only
// way to reach the bad state, where the table holds rows built for the old
// column set while the new one is being installed.
func TestResizingAPopulatedDashboardNeverPanics(t *testing.T) {
	st, logsDir := seedStore(t)
	addJobs(t, st, "worker", 6)

	sizes := []struct{ w, h int }{
		{172, 40}, {60, 20}, // wide -> narrow: drops columns
		{60, 20}, {172, 40}, // narrow -> wide: adds them back
		{120, 30}, {46, 22}, {200, 50}, {30, 16}, {80, 24}, {24, 14},
		{172, 40}, {100, 30}, {99, 29}, {56, 20}, {55, 19},
	}

	// One model, resized repeatedly — the point is the transitions.
	m := modelAt(t, st, logsDir, sizes[0].w, sizes[0].h)
	m = send(t, m, special(tea.KeyEnter)) // into runs, so both tables carry rows
	for _, s := range sizes {
		m = send(t, m, tea.WindowSizeMsg{Width: s.w, Height: s.h})
		frame := m.View() // renders both tables against the new columns
		if frame == "" {
			t.Fatalf("empty frame at %dx%d", s.w, s.h)
		}
		// Rows and columns must agree after every resize, or the next
		// render is the one that panics.
		for _, tbl := range []struct {
			name string
			cols []table.Column
			rows []table.Row
		}{
			{"jobs", m.jobsCols, m.jobsTable.Rows()},
			{"runs", m.runsCols, m.runsTable.Rows()},
		} {
			for i, row := range tbl.rows {
				if len(row) != len(tbl.cols) {
					t.Fatalf("at %dx%d the %s table has %d columns but row %d has %d cells",
						s.w, s.h, tbl.name, len(tbl.cols), i, len(row))
				}
			}
		}
	}
}

// Resizing must not silently move the selection, which is what naively
// clearing the rows to swap columns would do.
func TestResizingKeepsTheSelectedJob(t *testing.T) {
	st, logsDir := seedStore(t)
	addJobs(t, st, "worker", 6)

	m := modelAt(t, st, logsDir, 172, 40)
	m = send(t, m, special(tea.KeyDown))
	m = send(t, m, special(tea.KeyDown))
	want := m.selectedJobID()
	if want == "" {
		t.Fatal("expected a selected job to start with")
	}

	for _, s := range []struct{ w, h int }{{60, 20}, {172, 40}, {46, 22}, {120, 30}} {
		m = send(t, m, tea.WindowSizeMsg{Width: s.w, Height: s.h})
		if got := m.selectedJobID(); got != want {
			t.Errorf("resizing to %dx%d moved the selection from %q to %q", s.w, s.h, want, got)
		}
	}
}
