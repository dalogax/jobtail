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
)

// typeText sends s one rune at a time, the way a terminal delivers typing.
func typeText(t testing.TB, m model, s string) model {
	t.Helper()
	for _, r := range s {
		m = send(t, m, key(r))
	}
	return m
}

// gotoField moves the form's cursor onto the field labelled label.
func gotoField(t testing.TB, m model, k string) model {
	t.Helper()
	m = send(t, m, special(tea.KeyHome))
	for range len(m.form.fields) {
		if m.form.fields[m.form.cursor].key == k {
			return m
		}
		m = send(t, m, special(tea.KeyDown))
	}
	t.Fatalf("field %q not reachable; cursor on %q", k, m.form.fields[m.form.cursor].key)
	return m
}

// setField types v into field k, replacing what was there.
func setField(t testing.TB, m model, k, v string) model {
	t.Helper()
	m = gotoField(t, m, k)
	m = send(t, m, special(tea.KeyEnter)) // start typing
	f := m.form.fields[m.form.cursor]
	if f.kind == fieldMulti {
		f.area.SetValue("")
	} else {
		f.input.SetValue("")
	}
	m = typeText(t, m, v)
	return send(t, m, special(tea.KeyEnter)) // done
}

// TestNewJobFormCreatesAJob: n on the jobs pane opens the form, every field
// typed in lands in the store, and the form closes onto the new job.
func TestNewJobFormCreatesAJob(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 120, 30)
	dir := t.TempDir()

	m = send(t, m, key('n'))
	if m.form == nil {
		t.Fatal("n should open the job form")
	}
	if v := plain(m.View()); !strings.Contains(v, "New job") || !strings.Contains(v, "s save") {
		t.Fatalf("form not drawn:\n%s", v)
	}
	m = setField(t, m, fID, "backup-check")
	m = setField(t, m, fCron, "*/15 * * * *")
	m = setField(t, m, fCwd, dir)
	m = setField(t, m, fCommand, "./check.sh --all")
	m = setField(t, m, fTimeout, "90")
	m = setField(t, m, fNotify, "started,failed")
	m = setField(t, m, fKeep, "50")
	if !strings.Contains(plain(m.View()), "New job ●") {
		t.Fatalf("a changed form should carry the dirty mark:\n%s", plain(m.View()))
	}
	m = send(t, m, key('s'))
	if m.form != nil {
		t.Fatalf("form should close after a valid save; errors: %+v", formErrors(m.form))
	}

	j, err := st.GetJob(context.Background(), "backup-check")
	if err != nil {
		t.Fatal(err)
	}
	if j.Kind != "cli" || j.Cron != "*/15 * * * *" || j.Cwd != dir || j.Command != "./check.sh --all" ||
		j.TimeoutSeconds != 90 || j.Notify != "started,failed" || j.Keep != 50 || j.MaxConcurrent != 1 ||
		!j.Enabled || j.Timezone != "local" {
		t.Fatalf("stored job doesn't match the form: %+v", j)
	}
	if got := m.selectedJobID(); got != "backup-check" {
		t.Fatalf("cursor should land on the new job, is on %q", got)
	}
	// The runs pane follows the cursor: it must not keep showing the runs of
	// the job selected before the form opened under the new job's name.
	if m.runsJobID != "backup-check" || len(m.runs) != 0 || !strings.Contains(plain(m.View()), "No runs yet.") {
		t.Fatalf("runs pane still shows %q's runs:\n%s", m.runsJobID, plain(m.View()))
	}
	if !strings.Contains(plain(m.View()), "created job backup-check") {
		t.Fatalf("footer should confirm the save:\n%s", plain(m.View()))
	}
}

func formErrors(f *jobForm) map[string]string {
	out := map[string]string{}
	for _, fl := range f.fields {
		if fl.err != "" {
			out[fl.key] = fl.err
		}
	}
	return out
}

// TestNewAgentJobForm: switching Kind swaps the command for the agent fields,
// and a multi-line prompt is kept as written.
func TestNewAgentJobForm(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 120, 30)
	dir := t.TempDir()

	m = send(t, m, key('n'))
	if strings.Contains(plain(m.View()), "Prompt") {
		t.Fatal("a cli job shouldn't show the agent fields")
	}
	m = gotoField(t, m, fKind)
	m = send(t, m, special(tea.KeyRight))
	if v := plain(m.View()); !strings.Contains(v, "Prompt") || strings.Contains(v, "Command") {
		t.Fatalf("agent kind should show Prompt and hide Command:\n%s", v)
	}
	m = setField(t, m, fID, "deps-review")
	m = setField(t, m, fCron, "0 3 * * *")
	m = setField(t, m, fCwd, dir)

	// Prompt: two lines, the second via alt+enter.
	m = gotoField(t, m, fPrompt)
	m = send(t, m, special(tea.KeyEnter))
	m = typeText(t, m, "Check deps.")
	m = send(t, m, tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	m = typeText(t, m, "Open a PR if safe.")
	m = send(t, m, special(tea.KeyEnter))

	m = gotoField(t, m, fProvider)
	m = send(t, m, special(tea.KeyRight)) // claude -> opencode
	m = send(t, m, special(tea.KeyRight)) // -> codex
	m = setField(t, m, fPermission, "read-only")
	m = send(t, m, key('s'))
	if m.form != nil {
		t.Fatalf("form should close; errors: %+v", formErrors(m.form))
	}
	j, err := st.GetJob(context.Background(), "deps-review")
	if err != nil {
		t.Fatal(err)
	}
	if j.Kind != "agent" || j.Prompt != "Check deps.\nOpen a PR if safe." || j.Provider != "codex" ||
		j.PermissionMode != "read-only" || j.Command != "" {
		t.Fatalf("stored agent job doesn't match the form: %+v", j)
	}
}

// TestEditJobFormPrefillsAndSaves: e opens the selected job with its values,
// the id and kind can't be changed, and saving writes only through EditJob.
func TestEditJobFormPrefillsAndSaves(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 120, 30)

	m = send(t, m, key('e'))
	if m.form == nil {
		t.Fatal("e should open the job form")
	}
	v := plain(m.View())
	for _, want := range []string{"Edit greet", "0 0 * * *", "echo hi", "fixed"} {
		if !strings.Contains(v, want) {
			t.Fatalf("edit form should show %q:\n%s", want, v)
		}
	}
	if f := m.form.fields[m.form.cursor]; f.key == fID || f.key == fKind {
		t.Fatalf("cursor starts on fixed field %q", f.key)
	}

	// esc while typing puts the field back as it was.
	m = gotoField(t, m, fCommand)
	m = send(t, m, special(tea.KeyEnter))
	m = typeText(t, m, " && rm -rf nothing")
	m = send(t, m, special(tea.KeyEsc))
	if got := m.form.field(fCommand).value(); got != "echo hi" {
		t.Fatalf("esc should undo the field, got %q", got)
	}

	m = setField(t, m, fCron, "30 6 * * 1-5")
	m = gotoField(t, m, fEnabled)
	m = send(t, m, key(' ')) // yes -> no
	m = send(t, m, key('s'))
	if m.form != nil {
		t.Fatalf("form should close; errors: %+v", formErrors(m.form))
	}
	j, _ := st.GetJob(context.Background(), "greet")
	if j.Cron != "30 6 * * 1-5" || j.Enabled || j.Command != "echo hi" {
		t.Fatalf("edit not saved as typed: %+v", j)
	}
	if !strings.Contains(jobLine(m.View(), "greet"), "disabled") {
		t.Fatalf("the dashboard should show the saved change:\n%s", plain(m.View()))
	}
}

// TestJobFormValidatesBeforeSaving: a bad value keeps the form open, says
// what's wrong under the field, and writes nothing.
func TestJobFormValidatesBeforeSaving(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 120, 30)

	m = send(t, m, key('n'))
	m = setField(t, m, fID, "greet") // taken
	m = setField(t, m, fCron, "*/15 * * *")
	m = setField(t, m, fCwd, "/no/such/dir")
	m = setField(t, m, fCommand, "true")
	m = setField(t, m, fNotify, "sometimes")
	m = send(t, m, key('s'))
	if m.form == nil {
		t.Fatal("an invalid form must stay open")
	}
	v := plain(m.View())
	for _, want := range []string{"3 fields need attention", "expected exactly 5 fields", "no such directory", "unknown event"} {
		if !strings.Contains(v, want) {
			t.Fatalf("want %q on screen:\n%s", want, v)
		}
	}
	if got := m.form.fields[m.form.cursor].key; got != fCron {
		t.Fatalf("cursor should jump to the first invalid field, is on %q", got)
	}

	// Fix the three; the duplicate id is only caught by the store.
	m = setField(t, m, fCron, "*/15 * * * *")
	if strings.Contains(plain(m.View()), "expected exactly 5 fields") {
		t.Fatal("fixing a field should clear its error")
	}
	m = setField(t, m, fCwd, t.TempDir())
	m = setField(t, m, fNotify, "")
	m = send(t, m, key('s'))
	if m.form == nil || !strings.Contains(plain(m.View()), "already exists") {
		t.Fatalf("a duplicate id must be reported on the form:\n%s", plain(m.View()))
	}
	if j, _ := st.GetJob(context.Background(), "greet"); j.Cron != "0 0 * * *" {
		t.Fatalf("existing job was overwritten: %+v", j)
	}
}

// TestJobFormKeysWhileTyping: s and q are text while typing into a field;
// ctrl+s still saves from there.
func TestJobFormKeysWhileTyping(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 120, 30)

	m = send(t, m, key('e'))
	m = gotoField(t, m, fCommand)
	m = send(t, m, special(tea.KeyEnter))
	m = typeText(t, m, "; sq")
	if m.form == nil || !m.form.editing {
		t.Fatal("s and q while typing must not save or quit")
	}
	m = send(t, m, special(tea.KeyCtrlS))
	if m.form != nil {
		t.Fatalf("ctrl+s should save from inside a field; errors: %+v", formErrors(m.form))
	}
	if j, _ := st.GetJob(context.Background(), "greet"); j.Command != "echo hi; sq" {
		t.Fatalf("command = %q", j.Command)
	}
}

// TestJobFormQuitAsksOnlyWhenChanged: q on an untouched form closes it; on a
// changed one it asks, and n goes back to the form with nothing lost.
func TestJobFormQuitAsksOnlyWhenChanged(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 120, 30)

	m = send(t, m, key('e'))
	m = send(t, m, key('q'))
	if m.form != nil {
		t.Fatal("q on an unchanged form should close it")
	}

	m = send(t, m, key('e'))
	m = setField(t, m, fCron, "5 5 * * *")
	m = send(t, m, key('q'))
	if m.form == nil || !strings.Contains(plain(m.View()), "Discard 1 unsaved change?") {
		t.Fatalf("q on a changed form should ask:\n%s", plain(m.View()))
	}
	m = send(t, m, key('n'))
	if m.form == nil || m.form.field(fCron).value() != "5 5 * * *" {
		t.Fatal("n should go back to the form with the change kept")
	}
	m = send(t, m, key('q'))
	m = send(t, m, key('y'))
	if m.form != nil {
		t.Fatal("y should discard and close")
	}
	if j, _ := st.GetJob(context.Background(), "greet"); j.Cron != "0 0 * * *" {
		t.Fatalf("a discarded form must not save: %+v", j)
	}
	if m.focus != focusJobs || strings.Contains(m.View(), "Discard") {
		t.Fatal("the dashboard should be back as it was")
	}
}

// TestModalsFitEveryLayout: the modal and the faded dashboard around it are
// exactly the terminal's size at every layout, down to the narrowest.
func TestModalsFitEveryLayout(t *testing.T) {
	st, logsDir := seedStore(t)
	for _, size := range []struct{ w, h int }{{172, 40}, {120, 30}, {80, 24}, {60, 24}, {46, 20}, {30, 10}} {
		m := modelAt(t, st, logsDir, size.w, size.h)
		for name, open := range map[string]func(model) model{
			"new":   func(m model) model { return send(t, m, key('n')) },
			"edit":  func(m model) model { return send(t, m, key('e')) },
			"clean": func(m model) model { return send(t, send(t, m, key('l')), key('c')) },
		} {
			frame := open(m).View()
			lines := strings.Split(frame, "\n")
			if len(lines) != size.h {
				t.Errorf("%s at %dx%d: %d lines", name, size.w, size.h, len(lines))
			}
			for i, l := range lines {
				if w := lipgloss.Width(l); w != size.w {
					t.Errorf("%s at %dx%d: line %d is %d wide: %q", name, size.w, size.h, i, w, plain(l))
					break
				}
			}
			if name != "clean" && !strings.Contains(plain(frame), "s save") {
				t.Errorf("%s at %dx%d: the save key must stay visible:\n%s", name, size.w, size.h, plain(frame))
			}
		}
	}
}

// TestCleanRunsAsksThenDeletesFinishedRuns: c on the runs pane asks first,
// n leaves everything, y deletes the finished runs and their logs and keeps a
// run still in flight.
func TestCleanRunsAsksThenDeletesFinishedRuns(t *testing.T) {
	st, logsDir := seedStore(t)
	ctx := context.Background()
	live := filepath.Join(logsDir, "live.log")
	if err := os.WriteFile(live, []byte("still going\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartRun(ctx, "greet", "live", "manual", live, time.Now()); err != nil {
		t.Fatal(err)
	}
	m := modelAt(t, st, logsDir, 120, 30)

	m = send(t, m, key('c'))
	if m.cleanAsk != nil {
		t.Fatal("c on the jobs pane must not clean anything")
	}
	m = send(t, m, key('l'))
	m = send(t, m, key('c'))
	v := plain(m.View())
	if !strings.Contains(v, "Clean runs of greet?") || !strings.Contains(v, "Deletes 1 finished run") ||
		!strings.Contains(v, "1 running kept") {
		t.Fatalf("clean should ask, naming the job and the counts:\n%s", v)
	}
	m = send(t, m, key('n'))
	if runs, _ := st.ListRuns(ctx, "greet", -1); len(runs) != 2 {
		t.Fatalf("n must delete nothing, %d runs left", len(runs))
	}

	m = send(t, m, key('c'))
	m = send(t, m, key('y'))
	runs, _ := st.ListRuns(ctx, "greet", -1)
	if len(runs) != 1 || runs[0].ID != "live" {
		t.Fatalf("want only the running run left, got %+v", runs)
	}
	if _, err := os.Stat(filepath.Join(logsDir, "seed-run-1.log")); !os.IsNotExist(err) {
		t.Fatalf("the finished run's log should be gone: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the running run's log must stay: %v", err)
	}
	v = plain(m.View())
	if !strings.Contains(v, "cleaned 1 run of greet") || strings.Contains(v, "hello-from-seeded-log") {
		t.Fatalf("dashboard should show the result and drop the deleted log:\n%s", v)
	}
	if m.runRowCount() != 1 {
		t.Fatalf("runs pane should hold the one run left, has %d", m.runRowCount())
	}
}

func TestLimitLabel(t *testing.T) {
	for in, want := range map[time.Duration]string{
		30 * time.Minute: "30m", 90 * time.Minute: "1h30m", 40 * time.Second: "40s",
		time.Hour: "1h", 61 * time.Second: "1m1s", 0: "0s",
	} {
		if got := limitLabel(in); got != want {
			t.Errorf("limitLabel(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestJobWithoutRunsShowsNoLog: moving to a job that has never run must not
// leave the previous job's log on screen.
func TestJobWithoutRunsShowsNoLog(t *testing.T) {
	st, logsDir := seedStore(t)
	addJobs(t, st, "zz-fresh", 1)
	m := modelAt(t, st, logsDir, 120, 30)
	if !strings.Contains(plain(m.View()), "hello-from-seeded-log") {
		t.Fatal("precondition: greet's log should be showing")
	}
	m = send(t, m, special(tea.KeyDown))
	if strings.HasPrefix(m.selectedJobID(), "greet") {
		t.Fatal("cursor didn't move")
	}
	if v := plain(m.View()); strings.Contains(v, "hello-from-seeded-log") {
		t.Fatalf("a job with no runs still shows the previous job's log:\n%s", v)
	}
}
