package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/dalogax/jobtail/internal/store"
)

func history(statuses ...string) []store.Run {
	runs := make([]store.Run, len(statuses))
	for i, status := range statuses {
		runs[i] = store.Run{ID: fmt.Sprintf("run-%d", i), JobID: "greet", Status: status,
			Trigger: "scheduled", LogPath: "missing", StartedAt: time.Now().Add(-time.Duration(i) * time.Minute)}
	}
	return runs
}

func TestSkipGroupsOnlyFoldConsecutivePrecheckSkips(t *testing.T) {
	runs := history("skipped", "skipped", "ok", "skipped", "skipped", "skipped_overlap", "skipped", "failed")
	entries := groupRuns(runs, nil)
	if len(entries) != 6 {
		t.Fatalf("got %d entries, want 6: %+v", len(entries), entries)
	}
	for i, count := range []int{2, 1, 2, 1, 1, 1} {
		if got := entries[i].end - entries[i].first; got != count {
			t.Fatalf("entry %d: %d runs, want %d", i, got, count)
		}
	}
	if entries[3].group || entries[4].group {
		t.Fatal("overlap skips and isolated precheck skips must stay individual")
	}
}

func TestSkipGroupExpandCollapseAndSelectionMapping(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 172, 40)
	m = send(t, m, runsLoadedMsg{jobID: "greet", runs: history("skipped", "skipped", "skipped", "ok", "failed")})
	m.setFocus(focusRuns)
	m.layout()
	if m.runRowCount() != 3 || !strings.Contains(ansi.Strip(m.runsTable.View()), "skipped ×3") {
		t.Fatalf("missing collapsed skip stack: %s", m.runsTable.View())
	}
	if !strings.Contains(m.logPaneTitle(), "3 skips") {
		t.Fatalf("group log should identify the stack and latest log: %s", m.logPaneTitle())
	}
	m = send(t, m, special(tea.KeyEnter))
	if m.focus != focusRuns || m.runRowCount() != 6 {
		t.Fatalf("enter should expand the group, got focus %v, %d rows", m.focus, m.runRowCount())
	}
	for i := 1; i <= 3; i++ {
		m.runsTable.SetCursor(i)
		if r := m.selectedRunRow(); r == nil || r.ID != fmt.Sprintf("run-%d", i-1) {
			t.Fatalf("expanded row %d selected wrong run: %+v", i, r)
		}
	}
	m.runsTable.SetCursor(0)
	m = send(t, m, special(tea.KeyEnter))
	if m.runRowCount() != 3 {
		t.Fatal("enter on header should collapse the group")
	}
	m.selectRunByID("run-2")
	if m.runsTable.Cursor() != 0 {
		t.Fatal("hidden run should map to its collapsed group")
	}
	m.selectRunByID("run-3")
	if m.runsTable.Cursor() != 1 || m.isSelectedRunRunning() {
		t.Fatal("real run after group must remain selectable")
	}
}

func TestSkipExpansionSurvivesRefreshAndResetsForAnotherJob(t *testing.T) {
	st, logsDir := seedStore(t)
	addSecondJob(t, st)
	m := modelAt(t, st, logsDir, 172, 40)
	m.selectJobByID("greet")
	runs := history("skipped", "skipped", "ok")
	m = send(t, m, runsLoadedMsg{jobID: "greet", runs: runs})
	m.runsTable.SetCursor(0)
	m.setFocus(focusRuns)
	m = send(t, m, special(tea.KeyEnter))
	newSkip := runs[0]
	newSkip.ID = "new-skip"
	newSkip.StartedAt = newSkip.StartedAt.Add(time.Minute)
	m = send(t, m, runsLoadedMsg{jobID: "greet", runs: append([]store.Run{newSkip}, runs...)})
	if m.runRowCount() != 5 || !m.selectedRunEntry().group {
		t.Fatal("expanded group and group header selection must survive a new skip")
	}
	m.selectJobByID("abbey")
	m = send(t, m, runsLoadedMsg{jobID: "abbey", runs: nil})
	if len(m.runExpanded) != 0 || m.runRowCount() != 0 || m.selectedRunRow() != nil {
		t.Fatal("another job must not inherit the prior job's groups")
	}
}

func TestRunReloadIncludesHistoryBeyondTwoHundredSkips(t *testing.T) {
	st, _ := seedStore(t)
	ctx := newModel(st, "").ctx
	for i := 0; i < 205; i++ {
		id := fmt.Sprintf("skip-%d", i)
		now := time.Now().Add(time.Duration(i+1) * time.Minute)
		if _, err := st.StartRun(ctx, "greet", id, "scheduled", "", now); err != nil {
			t.Fatal(err)
		}
		if err := st.FinishRun(ctx, id, "skipped", 1, now, 0); err != nil {
			t.Fatal(err)
		}
	}
	msg := reloadRunsCmd(ctx, st, "greet", nil, false)().(runsLoadedMsg)
	if msg.err != nil || len(msg.runs) != 206 {
		t.Fatalf("older actual run should be loaded: %d runs, err %v", len(msg.runs), msg.err)
	}
	entries := groupRuns(msg.runs, nil)
	if len(entries) != 2 || msg.runs[entries[1].first].Status != "ok" {
		t.Fatal("older actual run should be visible immediately after skip stack")
	}
}
