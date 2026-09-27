// Tests for what the refresh does and does not do.
//
// The dashboard now drops a refresh whose result would change nothing on
// screen, which is what makes an idle dashboard nearly free. The whole risk
// of that is the opposite case: a refresh that drops something it should
// have shown. Every test here is about that second case — a run appearing, a
// run finishing, a job being toggled from another process, a log being
// appended to, the window being resized — and the one test that asserts the
// skipping happens at all exists so the rest can't be satisfied by simply
// reloading everything again.
package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// tickMsgs returns the messages one refresh tick actually produces. The
// clock tick itself is dropped by runCmd, so an empty result means every
// reload command answered "nothing changed" and Bubble Tea would have done
// no update and rendered no frame.
func tickMsgs(t testing.TB, m model) []tea.Msg {
	t.Helper()
	_, cmd := m.Update(tickMsg(time.Now()))
	return runCmd(cmd)
}

func TestIdleRefreshProducesNoMessages(t *testing.T) {
	st, logsDir := seedStore(t)
	addJobs(t, st, "worker", 5)
	m := modelAt(t, st, logsDir, 172, 40)

	// Settle first: the initial load is a real change, so drain until quiet.
	for i := 0; i < 5; i++ {
		msgs := tickMsgs(t, m)
		if len(msgs) == 0 {
			break
		}
		for _, msg := range msgs {
			m = send(t, m, msg)
		}
	}

	if msgs := tickMsgs(t, m); len(msgs) != 0 {
		t.Errorf("a refresh with nothing changed produced %d message(s), so it still costs a "+
			"row rebuild, a re-layout and a frame render: %#v", len(msgs), msgs)
	}
}

func TestRefreshSeesANewRun(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 172, 40)
	before := len(m.runs)

	// A run started by another process — which is how every scheduled run
	// arrives: `jobtail tick` spawns `jobtail run-exec`, its own process on
	// its own connection.
	ctx := context.Background()
	runID := "arrived-later"
	logPath := filepath.Join(logsDir, runID+".log")
	if err := os.WriteFile(logPath, []byte("fresh output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartRun(ctx, "greet", runID, "scheduled", logPath, time.Now()); err != nil {
		t.Fatal(err)
	}

	for _, msg := range tickMsgs(t, m) {
		m = send(t, m, msg)
	}
	if len(m.runs) != before+1 {
		t.Fatalf("new run never reached the model: have %d runs, want %d", len(m.runs), before+1)
	}
	if !strings.Contains(plain(m.View()), "running") {
		t.Errorf("new run is in the model but not on screen:\n%s", plain(m.View()))
	}
}

func TestRefreshSeesARunFinish(t *testing.T) {
	st, logsDir := seedStore(t)
	ctx := context.Background()
	runID := "in-flight"
	logPath := filepath.Join(logsDir, runID+".log")
	if err := os.WriteFile(logPath, []byte("working\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartRun(ctx, "greet", runID, "manual", logPath, time.Now()); err != nil {
		t.Fatal(err)
	}
	m := modelAt(t, st, logsDir, 172, 40)
	if !strings.Contains(plain(m.View()), "running") {
		t.Fatalf("expected the in-flight run to show as running:\n%s", plain(m.View()))
	}

	// Only the status and the finish fields change — the run count doesn't
	// move, so nothing about the *shape* of the data signals this.
	if err := st.FinishRun(ctx, runID, "failed", 2, time.Now(), 1234); err != nil {
		t.Fatal(err)
	}
	for _, msg := range tickMsgs(t, m) {
		m = send(t, m, msg)
	}
	if !strings.Contains(plain(m.View()), "failed") {
		t.Errorf("run finished as failed but the pane still doesn't say so:\n%s", plain(m.View()))
	}
}

func TestRefreshSeesAJobToggledElsewhere(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 172, 40)
	if !strings.Contains(plain(m.View()), "● greet") {
		t.Fatalf("expected greet to start out enabled:\n%s", plain(m.View()))
	}

	// `jobtail disable greet` in another terminal.
	if err := st.SetEnabled(context.Background(), "greet", false); err != nil {
		t.Fatal(err)
	}
	for _, msg := range tickMsgs(t, m) {
		m = send(t, m, msg)
	}
	if !strings.Contains(plain(m.View()), "○ greet") {
		t.Errorf("job was disabled elsewhere but the pane still shows it enabled:\n%s", plain(m.View()))
	}
}

// TestRefreshSeesAGrowingLog is the live-tail case: the log pane decides
// whether to re-read from a stat, so an append has to be picked up and a
// file that didn't move has to be left alone.
func TestRefreshSeesAGrowingLog(t *testing.T) {
	st, logsDir := seedStore(t)
	ctx := context.Background()
	runID := "tailing"
	logPath := filepath.Join(logsDir, runID+".log")
	if err := os.WriteFile(logPath, []byte("first chunk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartRun(ctx, "greet", runID, "manual", logPath, time.Now()); err != nil {
		t.Fatal(err)
	}
	m := modelAt(t, st, logsDir, 172, 40)
	m = send(t, m, special(tea.KeyEnter)) // jobs -> runs
	m = send(t, m, special(tea.KeyEnter)) // runs -> log
	if !strings.Contains(m.logVP.View(), "first chunk") {
		t.Fatalf("log pane didn't load the initial content:\n%s", m.logVP.View())
	}

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("second chunk\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	for _, msg := range tickMsgs(t, m) {
		m = send(t, m, msg)
	}
	if !strings.Contains(m.logVP.View(), "second chunk") {
		t.Errorf("log grew but the pane never re-read it:\n%s", m.logVP.View())
	}

	// And now that it has caught up, a tick must not read it again.
	for _, msg := range tickMsgs(t, m) {
		if _, ok := msg.(logLoadedMsg); ok {
			t.Error("log was re-read even though the file hasn't changed since")
		}
	}
}

// TestResizeRewrapsTheLog guards a bug the old code only avoided by
// accident: the log is wrapped to the pane width, and it used to be re-wrapped
// solely because every tick happened to rebuild it. Now that an unchanged log
// is left alone, the resize itself has to re-wrap.
func TestResizeRewrapsTheLog(t *testing.T) {
	st, logsDir := seedStore(t)
	ctx := context.Background()
	runID := "wide-lines"
	logPath := filepath.Join(logsDir, runID+".log")
	long := strings.Repeat("alpha bravo charlie delta echo foxtrot ", 12)
	if err := os.WriteFile(logPath, []byte(long+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run, err := st.StartRun(ctx, "greet", runID, "manual", logPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, run.ID, "ok", 0, time.Now(), 10); err != nil {
		t.Fatal(err)
	}

	m := modelAt(t, st, logsDir, 160, 40)
	m = send(t, m, special(tea.KeyEnter))
	m = send(t, m, special(tea.KeyEnter))

	m = send(t, m, tea.WindowSizeMsg{Width: 70, Height: 30})
	for _, line := range strings.Split(plain(m.logVP.View()), "\n") {
		if len(line) > m.logVP.Width {
			t.Fatalf("after shrinking to 70 columns a log line is still %d wide (viewport is %d): %q",
				len(line), m.logVP.Width, line)
		}
	}
	// Re-wrapping must not lose anything either.
	m.logVP.GotoBottom()
	if !strings.Contains(plain(m.logVP.View()), "foxtrot") {
		t.Errorf("re-wrapped log lost its content:\n%s", plain(m.logVP.View()))
	}
}

// TestSelectingAnotherRunLoadsItsLog covers the case the stat cache could
// break most quietly: two runs whose logs are the same size. Keying the
// cache on the file's identity as well as its size is what makes this work.
func TestSelectingAnotherRunLoadsItsLog(t *testing.T) {
	st, logsDir := seedStore(t)
	ctx := context.Background()
	for i, body := range []string{"AAAA output\n", "BBBB output\n"} {
		runID := []string{"same-size-1", "same-size-2"}[i]
		logPath := filepath.Join(logsDir, runID+".log")
		if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		run, err := st.StartRun(ctx, "greet", runID, "manual", logPath,
			time.Now().Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.FinishRun(ctx, run.ID, "ok", 0, time.Now(), 10); err != nil {
			t.Fatal(err)
		}
	}

	m := modelAt(t, st, logsDir, 172, 40)
	m = send(t, m, special(tea.KeyEnter)) // jobs -> runs, newest first
	if got := m.logVP.View(); !strings.Contains(got, "BBBB") {
		t.Fatalf("expected the newest run's log first, got:\n%s", got)
	}
	m = send(t, m, special(tea.KeyDown)) // the older run, whose log is the same size
	if got := m.logVP.View(); !strings.Contains(got, "AAAA") {
		t.Errorf("selecting another run of identical log size kept the old content:\n%s", got)
	}
}

func TestStoreErrorStillSurfaces(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 172, 40)

	// A closed store makes every query fail; the refresh must report that
	// rather than treat "no result" as "nothing changed".
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	msgs := tickMsgs(t, m)
	if len(msgs) == 0 {
		t.Fatal("queries are failing but the refresh reported nothing at all")
	}
	for _, msg := range msgs {
		m = send(t, m, msg)
	}
	if m.err == nil {
		t.Error("query failure never reached the model")
	}
}
