package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/dalogax/jobtail/internal/store"
)

// seedAgentLog builds an agent job whose one run captured the given transcript,
// and returns a model focused on the log pane — the pane the fold keys act on.
func seedAgentLog(t *testing.T, transcript string) model {
	t.Helper()
	st, logsDir := seedStore(t)
	ctx := context.Background()

	dir := t.TempDir()
	if err := st.CreateJob(ctx, store.Job{
		ID: "transcriptjob", Kind: "agent", Cron: "0 0 * * *", Timezone: "local",
		Enabled: true, Cwd: dir, Prompt: "do a thing", MaxConcurrent: 1, Keep: 200,
	}); err != nil {
		t.Fatal(err)
	}

	runID := "transcript-run-1"
	logPath := filepath.Join(logsDir, runID+".log")
	if err := os.WriteFile(logPath, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	run, err := st.StartRun(ctx, "transcriptjob", runID, "manual", logPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunSessionID(ctx, run.ID, "sess-fold"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, run.ID, "ok", 0, time.Now(), 5); err != nil {
		t.Fatal(err)
	}

	m := newModel(st, logsDir)
	m = send(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = resolve(t, m, m.Init())
	m.selectJobByID("transcriptjob")
	m = resolve(t, m, m.reloadRuns("transcriptjob"))
	m = send(t, m, special(tea.KeyEnter)) // jobs -> runs
	m = send(t, m, special(tea.KeyEnter)) // runs -> log
	if m.focus != focusLog {
		t.Fatalf("focus = %v, want the log pane", m.focus)
	}
	return m
}

// foldTranscript is three tool calls, each with enough output to fold.
func foldTranscript() string {
	var sb strings.Builder
	for i := 1; i <= 3; i++ {
		var out strings.Builder
		for j := 1; j <= 20; j++ {
			fmt.Fprintf(&out, "call%d-line%d\\n", i, j)
		}
		fmt.Fprintf(&sb,
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"cmd%d"}}]}}`+"\n"+
				`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t%d","content":"%s"}]}}`+"\n",
			i, i, i, out.String())
	}
	sb.WriteString(`{"type":"result","subtype":"success","is_error":false,"num_turns":3,"duration_ms":1500}` + "\n")
	return sb.String()
}

func logPaneText(m model) string { return ansi.Strip(m.logVP.View()) }

// The cursor starts on the first foldable block, so the keys have something to
// act on the moment the pane opens.
func TestLogCursorStartsOnTheFirstFoldableBlock(t *testing.T) {
	m := seedAgentLog(t, foldTranscript())

	if len(m.logBlocks) == 0 {
		t.Fatal("the transcript was not parsed into blocks")
	}
	if len(m.logView.stops) != 3 {
		t.Fatalf("stops = %v, want the three tool calls", m.logView.stops)
	}
	if m.logCursor != m.logView.stops[0] {
		t.Fatalf("logCursor = %d, want the first stop %d", m.logCursor, m.logView.stops[0])
	}
}

// j and k step between foldable blocks and clamp at both ends rather than
// wrapping — wrapping past the end of a long transcript loses your place.
func TestLogCursorStepsAndClamps(t *testing.T) {
	m := seedAgentLog(t, foldTranscript())
	stops := m.logView.stops

	m = send(t, m, key('j'))
	if m.logCursor != stops[1] {
		t.Fatalf(`after "j" logCursor = %d, want %d`, m.logCursor, stops[1])
	}
	m = send(t, m, key('j'))
	m = send(t, m, key('j')) // one past the end
	if m.logCursor != stops[len(stops)-1] {
		t.Fatalf("cursor ran off the end: %d, want %d", m.logCursor, stops[len(stops)-1])
	}
	for range stops {
		m = send(t, m, key('k'))
	}
	m = send(t, m, key('k')) // one before the start
	if m.logCursor != stops[0] {
		t.Fatalf("cursor ran off the start: %d, want %d", m.logCursor, stops[0])
	}
}

// enter expands the block under the cursor and collapses it again.
func TestEnterTogglesTheBlockUnderTheCursor(t *testing.T) {
	m := seedAgentLog(t, foldTranscript())

	if got := logPaneText(m); !strings.Contains(got, "… +17 lines") {
		t.Fatalf("the first tool call should open folded:\n%s", got)
	}
	m = send(t, m, special(tea.KeyEnter))
	if !m.logExpanded[m.logCursor] {
		t.Fatal("enter did not expand the block under the cursor")
	}
	if got := logPaneText(m); !strings.Contains(got, "call1-line5") {
		t.Fatalf("expanding did not reveal the hidden output:\n%s", got)
	}
	m = send(t, m, special(tea.KeyEnter))
	if m.logExpanded[m.logCursor] {
		t.Fatal("a second enter did not collapse the block again")
	}
}

// enter must stay the fold key in the log pane and not fall through to the
// pane-navigation meaning it has everywhere else.
func TestEnterInTheLogPaneDoesNotChangeFocus(t *testing.T) {
	m := seedAgentLog(t, foldTranscript())
	m = send(t, m, special(tea.KeyEnter))
	if m.focus != focusLog {
		t.Fatalf("focus = %v after enter, want to stay on the log pane", m.focus)
	}
}

// o is the whole-run view: expand everything, then collapse everything.
func TestExpandAllTogglesEveryBlock(t *testing.T) {
	m := seedAgentLog(t, foldTranscript())

	m = send(t, m, key('o'))
	for _, i := range m.logView.stops {
		if !m.logExpanded[i] {
			t.Fatalf("block %d stayed folded after \"o\"", i)
		}
	}
	if got := logPaneText(m); strings.Contains(got, "… +") {
		t.Errorf("a fold marker survived expand-all:\n%s", got)
	}

	m = send(t, m, key('o'))
	for _, i := range m.logView.stops {
		if m.logExpanded[i] {
			t.Fatalf("block %d stayed expanded after a second \"o\"", i)
		}
	}
}

// Expanding a block below the fold has to bring it into view, or the keypress
// looks like it did nothing.
func TestExpandingScrollsTheBlockIntoView(t *testing.T) {
	m := seedAgentLog(t, foldTranscript())

	// Walk to the last tool call, which starts out below the visible window.
	for range m.logView.stops {
		m = send(t, m, key('j'))
	}
	last := m.logCursor
	top, bottom := m.logView.top[last], m.logView.bottom[last]
	off, h := m.logVP.YOffset, m.logVP.Height
	if top < off || bottom > off+h-1 {
		t.Fatalf("cursor block spans [%d,%d] but the window shows [%d,%d]", top, bottom, off, off+h-1)
	}
	if !strings.Contains(logPaneText(m), "cmd3") {
		t.Errorf("the cursor's block is not on screen:\n%s", logPaneText(m))
	}
}

// A block taller than the pane must be scrolled to its head, not its tail: the
// head is the line that says which call the output belongs to.
func TestATallExpandedBlockScrollsToItsHead(t *testing.T) {
	m := seedAgentLog(t, foldTranscript())
	m.logVP.Height = 10
	m = send(t, m, special(tea.KeyEnter)) // expand the first call: 20 lines into a 10-line pane

	got, want := m.logVP.YOffset, m.logView.top[m.logCursor]
	if got != want {
		t.Fatalf("YOffset = %d, want the block's first line %d", got, want)
	}
	if !strings.Contains(logPaneText(m), "cmd1") {
		t.Errorf("the tool call's head line scrolled out of view:\n%s", logPaneText(m))
	}
}

// A cli job's log has no blocks, so the pane has no cursor and j/k must keep
// working as plain scroll keys rather than becoming dead ones.
func TestCliLogHasNoCursorAndStillScrolls(t *testing.T) {
	st, logsDir := seedStore(t)
	ctx := context.Background()

	var long strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&long, "output line %d\n", i)
	}
	runID := "cli-long-1"
	logPath := filepath.Join(logsDir, runID+".log")
	if err := os.WriteFile(logPath, []byte(long.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	run, err := st.StartRun(ctx, "greet", runID, "manual", logPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, run.ID, "ok", 0, time.Now(), 5); err != nil {
		t.Fatal(err)
	}

	m := newModel(st, logsDir)
	m = send(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = resolve(t, m, m.Init())
	m.selectJobByID("greet")
	m = resolve(t, m, m.reloadRuns("greet"))
	m.selectRunByID(runID)
	m = resolve(t, m, m.reloadLog(runID, logPath))
	m = send(t, m, special(tea.KeyEnter))
	m = send(t, m, special(tea.KeyEnter))

	if m.logBlocks != nil {
		t.Fatal("a cli log should not be parsed into transcript blocks")
	}
	if m.logCursor != -1 {
		t.Fatalf("logCursor = %d, want -1 for a log with nothing foldable", m.logCursor)
	}
	// The pane opens scrolled to the bottom (it live-tails a running job), so
	// go to the top first — at the bottom there is nothing for "j" to prove.
	m.logVP.GotoTop()
	before := m.logVP.YOffset
	m = send(t, m, key('j'))
	if m.logVP.YOffset == before {
		t.Error(`"j" did nothing: with no cursor it must fall back to scrolling`)
	}
}

// The help bar must not advertise fold keys for a log that has nothing folded —
// the same rule that keeps "r resume" off a cli run.
func TestHelpBarOffersFoldKeysOnlyWhenSomethingFolds(t *testing.T) {
	withFolds := renderHelpBar(200, focusLog, "", false, true)
	for _, want := range []string{"expand", "step"} {
		if !strings.Contains(withFolds, want) {
			t.Errorf("a foldable log's bar is missing %q: %q", want, withFolds)
		}
	}
	without := renderHelpBar(200, focusLog, "", false, false)
	for _, notWant := range []string{"expand", "step"} {
		if strings.Contains(without, notWant) {
			t.Errorf("a log with nothing foldable still offers %q: %q", notWant, without)
		}
	}
}

// A log that grew is reparsed, and fold state must not survive that: block
// indices only mean anything within one parse, so keeping "block 7 is expanded"
// would expand whichever block landed at 7 this time.
func TestFoldStateResetsWhenTheLogGrows(t *testing.T) {
	m := seedAgentLog(t, foldTranscript())
	m = send(t, m, key('o')) // expand everything
	if len(m.logExpanded) == 0 {
		t.Fatal("expand-all recorded nothing")
	}

	runID, path := m.selectedRun()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"assistant","message":{"content":[{"type":"text","text":"and one more thing"}]}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	m = resolve(t, m, m.reloadLog(runID, path))
	if !strings.Contains(logPaneText(m), "and one more thing") {
		t.Fatal("the appended event was not picked up, so nothing was reparsed")
	}
	for i, on := range m.logExpanded {
		if on {
			t.Fatalf("block %d stayed expanded across a reparse", i)
		}
	}
}

// The other half of that rule: an *unchanged* log must keep its folds. The
// dashboard re-stats the log every second, and collapsing what you had just
// opened once a second would make the fold keys unusable.
func TestFoldStateSurvivesATickOnAnUnchangedLog(t *testing.T) {
	m := seedAgentLog(t, foldTranscript())
	m = send(t, m, special(tea.KeyEnter))
	opened := m.logCursor
	if !m.logExpanded[opened] {
		t.Fatal("enter did not expand anything to begin with")
	}

	runID, path := m.selectedRun()
	m = resolve(t, m, m.reloadLog(runID, path))

	if !m.logExpanded[opened] {
		t.Error("a re-stat of an unchanged log collapsed the open block")
	}
}
