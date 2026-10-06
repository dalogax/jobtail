// Package tui is jobtail's Bubble Tea dashboard: jobs -> runs -> log,
// three panes, a plain terminal program that runs in any tab or pane —
// including a Herdr tab (PRD §9), with no plugin manifest required.
package tui

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/cellbuf"

	"github.com/dalogax/jobtail/internal/cronx"
	"github.com/dalogax/jobtail/internal/execengine"
	"github.com/dalogax/jobtail/internal/resume"
	"github.com/dalogax/jobtail/internal/runner"
	"github.com/dalogax/jobtail/internal/store"
)

// focusPane is which of the three panes has keyboard focus.
type focusPane int

const (
	focusJobs focusPane = iota
	focusRuns
	focusLog
)

// refreshInterval matches PRD §9: "poll SQLite every ~1s for list panes;
// this is a personal single-writer box, no need for push/subscribe." A var,
// not a const, so tests can shrink it and avoid paying a real 1s sleep per
// resolved tick — see internal/tui/tui_test.go's init().
var refreshInterval = time.Second

// layoutMode is how the three panes are arranged for the terminal we
// actually got. The dashboard's interaction model is already a drill-down
// (jobs -> runs -> log, via enter/l forward and esc/h back), so the narrow
// layout doesn't invent a new mental model — it just stops drawing the
// levels you aren't looking at.
type layoutMode int

const (
	layoutWide    layoutMode = iota // jobs | runs side by side, log full-width below
	layoutStacked                   // all three full-width, stacked vertically
	layoutFocused                   // one pane at a time, filling the screen
)

// Layout breakpoints, chosen from what the panes actually need rather than
// round numbers: the jobs table's five columns need ~38 columns of content
// before they stop being ellipses, plus 2 padding per column and 4 of box
// chrome — about 52. Side-by-side therefore needs ~104 to hold two honest
// panes, so below wideMinWidth the top row stacks instead of splitting.
// stackedMin* is where three boxes (2 lines of chrome each) still leave
// enough rows and columns to be worth drawing at all.
const (
	wideMinWidth     = 100
	wideMinHeight    = 18
	stackedMinWidth  = 56
	stackedMinHeight = 20
)

func modeFor(width, height int) layoutMode {
	switch {
	case width >= wideMinWidth && height >= wideMinHeight:
		return layoutWide
	case width >= stackedMinWidth && height >= stackedMinHeight:
		return layoutStacked
	default:
		return layoutFocused
	}
}

// colSpec describes one table column's appetite for space.
//
// min is the narrowest width at which the column still reads as itself (a
// timestamp needs 11 for "09-27 13:30"; anything less is a lie). max is the
// widest width it can actually use — a timestamp column gains nothing from
// being 30 wide, it just pushes everything else around — and 0 means
// unbounded, which marks the column that absorbs whatever is left over.
// grow is its relative share of the leftover, and drop is the priority
// order: the highest drop value is discarded first when the pane is too
// narrow to fit them all.
type colSpec struct {
	title string
	min   int
	max   int
	grow  int
	drop  int
}

// perCellPadding is what grid adds to every column on top of its declared
// width — one column of padding on each side. See columnsWidth.
const perCellPadding = 2

// fitColumns decides which columns survive at the given content width and
// how wide each one gets. Columns are dropped whole, least-important first,
// rather than squeezed down to an illegible character or two.
//
// The old behavior split the available width proportionally across all five
// columns no matter how little there was, which at ordinary terminal sizes
// produced a pane made entirely of ellipses — at 80 columns the jobs table
// rendered "● ha-fr…  c…  …  09…  ok", where even the *headers* ("Ki…",
// "…") had been truncated past recognition and the Runs column had
// collapsed to a single "…" occupying real estate while conveying nothing.
// Three columns you can read beat five you can't, so when the width isn't
// there a column leaves rather than starving the ones that remain.
//
// The column with drop == 0 is the pane's identity (which job? which run?)
// and is never dropped; if even it can't have its min, it takes whatever
// width exists, down to 1.
func fitColumns(width int, specs []colSpec) []table.Column {
	keep := make([]bool, len(specs))
	for i := range keep {
		keep[i] = true
	}

	cost := func() int {
		total := 0
		for i, s := range specs {
			if keep[i] {
				total += s.min + perCellPadding
			}
		}
		return total
	}

	for cost() > width {
		worst, worstDrop := -1, 0
		for i, s := range specs {
			if keep[i] && s.drop > worstDrop {
				worst, worstDrop = i, s.drop
			}
		}
		if worst < 0 {
			break // only the never-drop column is left
		}
		keep[worst] = false
	}

	// Hand out whatever is left beyond the mins, by grow weight. A column
	// that hits its max stops taking and its surplus goes back into the
	// pot for the others, so a fixed-width column (a timestamp, a kind)
	// never inflates into dead space while the ID column — the one that
	// can always use more — is still truncating.
	widths := make([]int, len(specs))
	capped := make([]bool, len(specs))
	for i, s := range specs {
		widths[i] = s.min
		capped[i] = !keep[i] || s.grow == 0 || (s.max > 0 && s.min >= s.max)
	}

	leftover := width - cost()
	for leftover > 0 {
		totalGrow := 0
		for i, s := range specs {
			if keep[i] && !capped[i] {
				totalGrow += s.grow
			}
		}
		if totalGrow == 0 {
			break
		}
		handed := 0
		for i, s := range specs {
			if !keep[i] || capped[i] {
				continue
			}
			extra := leftover * s.grow / totalGrow
			if extra < 1 {
				continue
			}
			if s.max > 0 && widths[i]+extra > s.max {
				extra = s.max - widths[i]
				capped[i] = true
			}
			widths[i] += extra
			handed += extra
		}
		if handed == 0 {
			// Integer division can't split the last few columns; give the
			// remainder to the first uncapped grower and stop.
			for i := range specs {
				if keep[i] && !capped[i] {
					give := leftover
					if specs[i].max > 0 && widths[i]+give > specs[i].max {
						give = specs[i].max - widths[i]
					}
					widths[i] += give
					leftover -= give
					break
				}
			}
			break
		}
		leftover -= handed
	}

	cols := make([]table.Column, 0, len(specs))
	for i, s := range specs {
		if !keep[i] {
			continue
		}
		w := widths[i]
		if w < 1 {
			w = 1
		}
		cols = append(cols, table.Column{Title: s.title, Width: w})
	}

	// Every column has hit its max but the pane is wider still: park the
	// slack on the last column. It renders as trailing space just inside
	// the right border — a margin — rather than being distributed back
	// into columns that have no use for it. Without this the box would
	// come out narrower than the width it was given, leaving a ragged
	// right edge against the full-width log pane below it.
	if n := len(cols); n > 0 {
		if short := width - columnsWidth(cols); short > 0 {
			cols[n-1].Width += short
		}
	}

	// Degenerate case: not even the primary column's min fits. Clamp it to
	// what the terminal actually has instead of declaring a width that
	// overflows the box (which is what made the panes spill past the right
	// edge at 30 columns).
	if len(cols) == 1 && cols[0].Width+perCellPadding > width {
		cols[0].Width = width - perCellPadding
		if cols[0].Width < 1 {
			cols[0].Width = 1
		}
	}
	return cols
}

type model struct {
	ctx     context.Context
	st      *store.Store
	logsDir string

	focus       focusPane
	jobs        []store.JobSummary
	runs        []store.Run
	runEntries  []runEntry
	runExpanded map[string]bool
	selection   textSelection
	// runsJobID is whose runs are in runs — needed because a selection
	// change and the load that answers it are a round trip apart, so runs
	// can briefly belong to the previously selected job.
	runsJobID string

	jobsTable grid
	runsTable grid
	logVP     viewport.Model

	// What the log pane holds, and what it was built from. logShown is the
	// file identity a refresh checks against before reading anything at all.
	//
	// The two content fields are exclusive: a cli job's log is plain text held
	// in logText and wrapped at render time, while an agent's is parsed into
	// logBlocks once and re-rendered from there. Keeping the parsed blocks
	// rather than a finished string is what lets a resize, a cursor move and a
	// fold all re-render without touching the disk or the parser again.
	logShown     logShown
	logText      string  // cli jobs
	logBlocks    []block // agent jobs
	logWrapWidth int

	// The log pane's own cursor: a block index (not a line), which moves
	// between the foldable blocks of logBlocks. -1 when the log has nothing
	// foldable in it, in which case the pane has no cursor at all and j/k go
	// back to being ordinary scroll keys.
	logView     transcriptView
	logCursor   int
	logExpanded map[int]bool

	width, height int
	err           error
	statusMsg     string
	showHelp      bool            // the ? key list is open
	running       map[string]bool // job IDs with an in-flight "run now" from the TUI
	resuming      map[string]bool // run IDs with a Herdr tab already opening

	// The modals. At most one is open; while it is, it owns every key.
	form     *jobForm     // n / e on the jobs pane
	cleanAsk *cleanPrompt // c on the runs pane

	// selectAfterLoad is a job to put the cursor on once the next job list
	// arrives: the one just created or saved, so the form closes onto it.
	selectAfterLoad string
	cwd             string // where the dashboard was opened; a new job starts there

	// Geometry, all set by layout() and read by both View() and mouse
	// hit-testing — the two must never re-derive it independently, which
	// is exactly what once put the clickable pane boundaries somewhere
	// other than the drawn ones (confirmed by measuring a real frame).
	//
	// mode selects which of these apply. In layoutWide, jobs and runs sit
	// side by side split at jobsBoxWidth with the log below topBoxHeight.
	// In layoutStacked all three are full width and jobsBoxHeight /
	// runsBoxHeight give the horizontal cuts. In layoutFocused only the
	// focused pane is drawn and it owns the whole screen.
	mode                                     layoutMode
	jobsBoxWidth, runsBoxWidth, topBoxHeight int
	jobsBoxHeight, runsBoxHeight             int

	// The columns each table currently renders. Rows are built from these
	// (see jobRows): which columns survived the width, and how wide each
	// landed, decides both how many cells a row needs and whether the
	// status cell has room to carry color.
	jobsCols, runsCols []table.Column

	// laidOut is the input set the geometry above was computed from, so an
	// unchanged screen isn't re-measured (see layoutKey).
	laidOut layoutKey
}

// layoutKey is every input layout() reads. Reload messages arrive far more
// often than the screen actually changes shape, and each blind re-layout
// pushed a fresh column set and width into both tables — two
// table.UpdateViewport passes apiece — plus a full row rebuild, all to
// arrive at the same numbers. Comparing the inputs first makes an
// unchanged layout free.
type layoutKey struct {
	width, height      int
	focus              focusPane
	jobCount, runCount int
}

func (m model) layoutKey() layoutKey {
	return layoutKey{m.width, m.height, m.focus, len(m.jobs), m.runRowCount()}
}

// renderFPS caps Bubble Tea's renderer. Its default is 60, which costs a
// timer wakeup and a flush check 60 times a second whether or not there is a
// new frame to write — measured against a real pty, that idle ticker alone
// was 6 ms of CPU per second, most of what an otherwise-idle dashboard
// burned. Halving it halves that.
//
// 30 is chosen rather than something lower because this is the one knob here
// that trades against feel: the renderer is also what puts a keystroke on
// screen, so the framerate is the worst-case input latency. 30 FPS is 33 ms,
// imperceptible and still smooth under held-key scrolling; 15 FPS (67 ms)
// starts to be noticeable. Nothing the dashboard displays changes faster
// than the 1 Hz refresh, so no content update can tell 30 from 60.
const renderFPS = 30

// Run opens the dashboard. It blocks until the user quits.
func Run(st *store.Store, logsDir string) error {
	m := newModel(st, logsDir)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithFPS(renderFPS))
	_, err := p.Run()
	return err
}

// defaultPaneWidth is used only until the first real tea.WindowSizeMsg
// arrives and layout() recomputes columns to fit the actual terminal.
const defaultPaneWidth = 40

func newModel(st *store.Store, logsDir string) model {
	jobCols, runCols := jobsColumns(defaultPaneWidth), runsColumns(defaultPaneWidth)
	jt := newGrid(jobCols, accentJobs, true)
	rt := newGrid(runCols, accentRuns, false)
	vp := viewport.New(20, 10)
	cwd, _ := os.Getwd()

	return model{
		cwd:       cwd,
		ctx:       context.Background(),
		st:        st,
		logsDir:   logsDir,
		focus:     focusJobs,
		jobsTable: jt,
		runsTable: rt,
		logVP:     vp,
		jobsCols:  jobCols,
		runsCols:  runCols,
		running:   map[string]bool{},
		resuming:  map[string]bool{},
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.reloadJobs(), tick())
}

type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Refreshes that change nothing must cost nothing.
//
// Every refresh below is a tea.Cmd, and Bubble Tea's event loop drops a
// command whose Msg is nil before it reaches anything (tea.go: `if msg ==
// nil { continue }`) — no Update, no row rebuild, no re-layout, and, the
// part that actually dominated, no frame render. So each command is handed
// what its pane is already showing and answers "nothing new" with nil.
//
// This is what an idle dashboard almost always is. Jobs change when someone
// edits one; runs change when one starts or finishes, which for a */5
// schedule is twice every five minutes; a finished run's log never changes
// at all. Yet the refresh ran the full pipeline — two queries, a file read,
// a JSON parse of the whole transcript, a re-wrap, four frame renders —
// once a second regardless, and then Bubble Tea's renderer discarded the
// result because the frame was byte-identical to the one already on screen.
// Measured against a real 12-job store with a 256 KB agent transcript
// selected, that was 32 ms of CPU per second and 0 bytes of terminal output.

type jobsLoadedMsg struct {
	jobs []store.JobSummary
	err  error
}

// reloadJobs compares against m.jobs, which is safe even before the first
// load: an empty database yields a nil slice equal to the nil m.jobs starts
// as, and delivering that message would be a genuine no-op (there is no
// cursor to place and layout() has already run from the window-size
// message).
func (m model) reloadJobs() tea.Cmd { return reloadJobsCmd(m.ctx, m.st, m.jobs) }

func reloadJobsCmd(ctx context.Context, st *store.Store, shown []store.JobSummary) tea.Cmd {
	return func() tea.Msg {
		jobs, err := st.ListJobs(ctx)
		if err == nil && slices.Equal(jobs, shown) {
			return nil
		}
		return jobsLoadedMsg{jobs: jobs, err: err}
	}
}

type runsLoadedMsg struct {
	jobID string
	runs  []store.Run
	err   error
}

// reloadRuns only lets the command drop a refresh when the runs on screen
// belong to the job being reloaded. Without that check, loading a job whose
// run list happens to be empty would compare equal to an empty m.runs and
// leave the *previous* job's runs on screen.
func (m model) reloadRuns(jobID string) tea.Cmd {
	return reloadRunsCmd(m.ctx, m.st, jobID, m.runs, jobID == m.runsJobID)
}

func reloadRunsCmd(ctx context.Context, st *store.Store, jobID string, shown []store.Run, shownIsThisJob bool) tea.Cmd {
	return func() tea.Msg {
		runs, err := st.ListRuns(ctx, jobID, -1) // all retained history, including folded skips
		if err == nil && shownIsThisJob && slices.Equal(runs, shown) {
			return nil
		}
		return runsLoadedMsg{jobID: jobID, runs: runs, err: err}
	}
}

type logLoadedMsg struct {
	runID   string
	text    string
	size    int64
	modTime time.Time
	err     error
}

// logShown identifies the exact bytes the log pane is displaying, so a
// refresh can decide from a stat alone whether to re-read the file — and
// with it re-parse a transcript and re-wrap the result, together the most
// expensive thing a tick could trigger. Run logs are append-only
// (execengine's capWriter only ever appends), so size and mtime settle it.
type logShown struct {
	runID   string
	size    int64
	modTime time.Time
	missing bool // the file wasn't there when we last looked
}

func (m model) reloadLog(runID, path string) tea.Cmd {
	return reloadLogCmd(runID, path, m.logShown)
}

func reloadLogCmd(runID, path string, shown logShown) tea.Cmd {
	return func() tea.Msg {
		fi, err := os.Stat(path)
		if err != nil {
			if shown.runID == runID && shown.missing {
				return nil // already showing "(no log yet)" for this run
			}
			return logLoadedMsg{runID: runID, err: err}
		}
		if shown.runID == runID && !shown.missing &&
			shown.size == fi.Size() && shown.modTime.Equal(fi.ModTime()) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return logLoadedMsg{runID: runID, err: err}
		}
		// Record the length actually read rather than the stat's size: a
		// live run may have appended between the two, and recording the
		// smaller number means the next tick sees a mismatch and re-reads
		// instead of treating a partial read as complete.
		return logLoadedMsg{runID: runID, text: string(data),
			size: int64(len(data)), modTime: fi.ModTime()}
	}
}

type runFinishedMsg struct {
	jobID string
}

// runNowCmd executes a job in the background (so the UI stays responsive)
// through the exact same runner.Execute/Finish path the CLI uses.
func runNowCmd(ctx context.Context, st *store.Store, j store.Job, logsDir string) tea.Cmd {
	return func() tea.Msg {
		runID := newRunID()
		logPath := logsDir + "/" + runID + ".log"
		_, _ = runner.ReapStale(ctx, st, time.Now()) // a stuck run must not block "run now"
		run, err := st.StartRun(ctx, j.ID, runID, "manual", logPath, time.Now())
		if err != nil && err != store.ErrOverlap {
			return runFinishedMsg{jobID: j.ID}
		}
		if run.Status == "skipped_overlap" {
			runner.NotifyOverlap(j)
			return runFinishedMsg{jobID: j.ID}
		}
		res, runErr := runner.Execute(ctx, st, j, runID, logPath)
		_ = runner.Finish(ctx, st, j, runID, res, runErr)
		return runFinishedMsg{jobID: j.ID}
	}
}

// resumedMsg reports the outcome of handing a run to an interactive
// session. Opening a Herdr tab shells out twice and so must not happen on
// the Update goroutine — a slow or wedged herdr would freeze the dashboard
// rather than the resume.
type resumedMsg struct {
	runID  string
	paneID string
	err    error
}

func resumeCmd(j store.Job, r store.Run) tea.Cmd {
	return func() tea.Msg {
		paneID, err := resume.Open(j, r)
		return resumedMsg{runID: r.ID, paneID: paneID, err: err}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.selection = textSelection{}
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		if m.form != nil {
			m.form.resize(m.width, m.height)
		}
		return m, nil

	case tickMsg:
		// A running run's Dur counts up. The reload below skips an unchanged
		// run list, so redraw those rows from what is already in memory —
		// only while something is running, so an idle dashboard stays idle.
		if slices.ContainsFunc(m.runs, func(r store.Run) bool { return r.Status == "running" }) {
			m.runsTable.SetRows(m.groupedRunRows(m.runsCols))
		}
		cmds := []tea.Cmd{tick(), m.reloadJobs()}
		if jobID := m.selectedJobID(); jobID != "" {
			cmds = append(cmds, m.reloadRuns(jobID))
		}
		// Offered unconditionally, not just for a run marked "running": the
		// command's own stat decides, and a stat is cheap enough that it
		// beats guessing from a status that may itself be a tick stale.
		if runID, path := m.selectedRun(); runID != "" {
			cmds = append(cmds, m.reloadLog(runID, path))
		}
		return m, tea.Batch(cmds...)

	case jobsLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		prevID := m.selectedJobID()
		target := prevID
		if m.selectAfterLoad != "" {
			target, m.selectAfterLoad = m.selectAfterLoad, ""
		}
		m.jobs = msg.jobs
		m.jobsTable.SetRows(jobRows(msg.jobs, m.jobsCols))
		m.layout() // pane heights follow the row count (see topRowHeight)
		if target != "" {
			m.selectJobByID(target)
		} else if len(msg.jobs) > 0 {
			m.jobsTable.SetCursor(0)
		}
		if jobID := m.selectedJobID(); jobID != "" && jobID != prevID {
			return m, m.reloadRuns(jobID)
		}
		return m, nil

	case runsLoadedMsg:
		if msg.jobID != m.selectedJobID() {
			return m, nil // stale response from a since-changed selection
		}
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		prevRunID, _ := m.selectedRun()
		prevEntry := m.selectedRunEntry()
		if m.runsJobID != msg.jobID {
			m.runExpanded = nil
		}
		m.runs, m.runsJobID = msg.runs, msg.jobID
		m.rebuildRunEntries()
		m.layout()
		if prevRunID != "" {
			m.selectRunByID(prevRunID)
			if prevEntry.group {
				for i, e := range m.runEntries {
					if e.group && e.key == prevEntry.key {
						m.runsTable.SetCursor(i)
						break
					}
				}
			}
		} else if len(msg.runs) > 0 {
			m.runsTable.SetCursor(0)
		}
		if runID, path := m.selectedRun(); runID != "" {
			return m, m.reloadLog(runID, path)
		}
		// No run to show, so no log: without this the pane went on showing
		// the previous job's output under a title that no longer named it.
		m.clearLog()
		return m, nil

	case logLoadedMsg:
		if id, _ := m.selectedRun(); id != msg.runID {
			return m, nil
		}
		if msg.err != nil {
			m.logShown = logShown{runID: msg.runID, missing: true}
			m.logText, m.logBlocks = "", nil
			m.logVP.SetContent("(no log yet)")
			return m, nil
		}
		m.logShown = logShown{runID: msg.runID, size: msg.size, modTime: msg.modTime}
		m.setLogContent(msg.text)
		return m, nil

	case runFinishedMsg:
		m.statusMsg = fmt.Sprintf("run finished for %s", msg.jobID)
		delete(m.running, msg.jobID)
		return m, m.reloadJobs()

	case resumedMsg:
		delete(m.resuming, msg.runID)
		if msg.err != nil {
			// Shown in the help bar, not set on m.err: m.err is for the
			// dashboard being unable to do its job, and a resume that
			// didn't open is a failed action, not a broken dashboard.
			m.statusMsg = fmt.Sprintf("resume failed: %v", msg.err)
			return m, nil
		}
		m.statusMsg = fmt.Sprintf("resumed in pane %s", msg.paneID)
		return m, nil
	case cleanedMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("clean failed: %v", msg.err)
			return m, nil
		}
		runs := "runs"
		if msg.n == 1 {
			runs = "run"
		}
		m.statusMsg = fmt.Sprintf("cleaned %d %s of %s", msg.n, runs, msg.jobID)
		// Forget what the runs and log panes were showing: the runs are gone,
		// and the reload must not try to put the cursor back on one of them.
		m.runs, m.runsJobID = nil, ""
		m.rebuildRunEntries()
		m.runsTable.SetCursor(0)
		m.clearLog()
		cmds := []tea.Cmd{m.reloadJobs()}
		if jobID := m.selectedJobID(); jobID != "" {
			cmds = append(cmds, m.reloadRuns(jobID))
		}
		return m, tea.Batch(cmds...)

	case copiedMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("copy failed: %v", msg.err)
		} else {
			m.statusMsg = "selection copied"
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return m, nil
}

// layout target: jobs and runs side by side on top, log spanning the full
// width underneath — the log is where the actual content is (command
// output, agent transcripts), so it gets the width and gets it below
// rather than squeezed into a third column.
// boxChrome is what one pane's frame costs horizontally and vertically:
// Border(RoundedBorder) contributes 2 columns (left+right) and 2 lines
// (top+bottom), and Padding(0, 1) contributes 2 more columns. The title
// costs no line of its own — it's spliced into the top border rule
// (embedTitle) — so vertically the chrome is just the 2 border lines.
const (
	boxChromeX = 4
	boxChromeY = 2
)

// setTableWidth sizes one table to a target *box* width: it fits the
// columns to the content width left over after chrome, then sets the
// table's own viewport to exactly what those columns sum to.
//
// The two must agree. Handing SetWidth anything other than columnsWidth of
// the columns just installed is what once silently clipped the last column
// away entirely (the Status header rendered but every row's value was
// gone), and what made the two top panes sum to 190 columns on a 172-column
// terminal — each step looking correct in isolation while the total
// overflowed. Returns the true rendered box width.
// Both setters re-render the table through UpdateViewport, so neither is
// called with a value the table already has.
//
// Columns and rows are swapped together, through an empty row set, because
// bubbles/table — what these tables were before grid — indexed its column
// slice by the row's cell position (renderRow: `for i := range m.rows[r] {
// m.cols[i] ... }`) and *both* setters re-rendered immediately. A table left holding six-cell rows against
// a five-column set therefore panics inside the setter itself, before
// anything gets the chance to rebuild the rows.
//
// That is not hypothetical: shrinking a terminal far enough to drop a
// column did it every time — "index out of range [5] with length 5", which
// Bubble Tea catches, prints with a stack trace, and exits on, so the whole
// dashboard vanishes mid-session. Building rows from the column set
// (cellsFor) made them consistent at any one size; it did not make the
// transition between two sizes safe.
func setTableWidth(t *grid, boxWidth int, cols func(int) []table.Column,
	rows func([]table.Column) []table.Row) (int, []table.Column) {
	content := boxWidth - boxChromeX - gridGutter
	if content < 1 {
		content = 1
	}
	c := cols(content)
	if !slices.Equal(t.Columns(), c) {
		cursor := t.Cursor()
		t.SetRows(nil) // no rows, so the column swap has nothing to mis-index
		t.SetColumns(c)
		t.SetRows(rows(c))
		t.SetCursor(cursor) // SetRows(nil) drops the selection; put it back
	}
	w := columnsWidth(c) + gridGutter
	if t.Width() != w {
		t.SetWidth(w)
	}
	return w + boxChromeX, c
}

func (m *model) layout() {
	if m.width == 0 {
		return
	}
	if key := m.layoutKey(); key == m.laidOut {
		return
	}
	m.laidOut = m.layoutKey()
	m.mode = modeFor(m.width, m.height)

	// One line at the bottom belongs to the help bar in every layout.
	avail := m.height - 1
	if avail < 6 {
		avail = 6
	}

	var jobCols, runCols []table.Column
	switch m.mode {
	case layoutWide:
		// Size the runs pane to what its columns can actually use, then
		// give every remaining column to the jobs pane. Runs content is
		// fixed-width (a status, a timestamp, a duration) and gains
		// nothing past that, while job IDs are open-ended — an even split
		// spent half the terminal padding timestamps while truncating the
		// names next to them.
		runsW := naturalBoxWidth(runsSpecs)
		if half := m.width / 2; runsW > half {
			runsW = half
		}
		m.runsBoxWidth, runCols = setTableWidth(&m.runsTable, runsW, runsColumns, m.runRowsFor)
		m.jobsBoxWidth, jobCols = setTableWidth(&m.jobsTable, m.width-m.runsBoxWidth, jobsColumns, m.jobRowsFor)
		m.logVP.Width = m.width - boxChromeX

		topBoxOuter := m.topRowHeight(avail)
		m.topBoxHeight = topBoxOuter
		m.jobsBoxHeight, m.runsBoxHeight = topBoxOuter, topBoxOuter
		setBoxHeight(&m.jobsTable, topBoxOuter)
		setBoxHeight(&m.runsTable, topBoxOuter)
		m.logVP.Height = viewportHeight(avail - topBoxOuter)

	case layoutStacked:
		m.jobsBoxWidth, jobCols = setTableWidth(&m.jobsTable, m.width, jobsColumns, m.jobRowsFor)
		m.runsBoxWidth, runCols = setTableWidth(&m.runsTable, m.width, runsColumns, m.runRowsFor)
		m.logVP.Width = m.width - boxChromeX

		jobsH, runsH, logH := m.stackedHeights(avail)
		m.jobsBoxHeight, m.runsBoxHeight = jobsH, runsH
		m.topBoxHeight = jobsH + runsH
		setBoxHeight(&m.jobsTable, jobsH)
		setBoxHeight(&m.runsTable, runsH)
		m.logVP.Height = viewportHeight(logH)

	case layoutFocused:
		m.jobsBoxWidth, jobCols = setTableWidth(&m.jobsTable, m.width, jobsColumns, m.jobRowsFor)
		m.runsBoxWidth, runCols = setTableWidth(&m.runsTable, m.width, runsColumns, m.runRowsFor)
		m.logVP.Width = m.width - boxChromeX

		// The one visible pane owns every row that isn't the help bar.
		m.jobsBoxHeight, m.runsBoxHeight, m.topBoxHeight = avail, avail, avail
		setBoxHeight(&m.jobsTable, avail)
		setBoxHeight(&m.runsTable, avail)
		m.logVP.Height = viewportHeight(avail)
	}

	// Rows were rebuilt against these columns inside setTableWidth, which
	// has to do it there rather than here: the two cannot be out of step
	// even momentarily.
	m.jobsCols, m.runsCols = jobCols, runCols

	// The log is wrapped to the pane width, so a resize has to re-wrap it.
	// It used to come right only because every tick re-read and re-wrapped
	// the whole log anyway; now that a tick leaves an unchanged log alone,
	// the resize has to say so — and says it immediately rather than on
	// whichever later refresh happened to rebuild the pane.
	if m.logVP.Width != m.logWrapWidth && (m.logText != "" || m.logBlocks != nil) {
		m.rebuildLogView()
	}
}

// clearLog empties the log pane, for when there is no run to show.
func (m *model) clearLog() {
	m.logShown = logShown{}
	m.logText, m.logBlocks, m.logView = "", nil, transcriptView{}
	m.logCursor = -1
	m.logVP.SetContent("")
}

// setLogContent installs a freshly read log: parsed into blocks for an agent
// run, kept as plain text for a cli one.
//
// The fold state is reset here rather than carried over, because block indices
// only mean anything within one parse — a re-read of a *growing* log yields
// more blocks, and holding on to "block 7 is expanded" across that would
// expand whichever block happened to land at 7 next time.
func (m *model) setLogContent(raw string) {
	m.logText, m.logBlocks = "", nil
	m.logExpanded = map[int]bool{}
	m.logCursor = -1

	if m.selectedJobKind() == "agent" {
		m.logBlocks = parseTranscript(raw, execengine.EffectiveProvider(m.selectedJobProvider()))
		m.logCursor = firstFoldable(m.logBlocks)
	} else {
		m.logText = raw
	}
	m.rebuildLogView()
}

// rebuildLogView re-renders the log pane to its current width, cursor and fold
// state, holding the view at the bottom if it was already there (live-tailing a
// running job).
func (m *model) rebuildLogView() {
	atBottom := m.logVP.AtBottom()
	if m.logBlocks == nil {
		m.logView = transcriptView{}
		m.logVP.SetContent(wrapForViewport(m.logText, m.logVP.Width))
	} else {
		m.logView = renderBlocks(m.logBlocks, renderOpts{
			width:    m.logVP.Width,
			cursor:   m.logCursor,
			expanded: m.logExpanded,
		})
		m.logVP.SetContent(m.logView.content)
	}
	m.logWrapWidth = m.logVP.Width
	if atBottom {
		m.logVP.GotoBottom()
	}
}

// firstFoldable is where the log pane's cursor starts: the first block that has
// anything hidden. -1 when there is none.
func firstFoldable(blocks []block) int {
	for i, b := range blocks {
		if b.foldable() {
			return i
		}
	}
	return -1
}

// scrollToCursor brings the cursor's block into view, doing nothing when it is
// already fully visible so that ordinary scrolling isn't yanked back.
func (m *model) scrollToCursor() {
	if m.logCursor < 0 || m.logCursor >= len(m.logView.top) || m.logVP.Height <= 0 {
		return
	}
	top, bottom := m.logView.top[m.logCursor], m.logView.bottom[m.logCursor]
	off := m.logVP.YOffset
	switch {
	case top < off:
		off = top
	case bottom > off+m.logVP.Height-1:
		off = bottom - m.logVP.Height + 1
		// A block taller than the pane — an expanded 600-line tool result —
		// would otherwise scroll to its tail and hide the call it belongs to.
		// Its head is the part that says what you're looking at.
		if off > top {
			off = top
		}
	default:
		return
	}
	m.logVP.SetYOffset(off)
}

// moveLogCursor steps the cursor to the next/previous foldable block. With
// nothing foldable in the log there is no cursor, and j/k stay plain scroll
// keys instead of becoming dead ones.
func (m model) moveLogCursor(delta int, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	stops := m.logView.stops
	if len(stops) == 0 {
		var cmd tea.Cmd
		m.logVP, cmd = m.logVP.Update(msg)
		return m, cmd
	}
	i := slices.Index(stops, m.logCursor)
	if i < 0 {
		i = 0
	} else {
		i += delta
	}
	i = min(max(i, 0), len(stops)-1)
	prev := m.logCursor
	m.logCursor = stops[i]
	if m.logCursor != prev {
		// Only the marker moved, so patch the two lines that changed instead of
		// re-rendering every block below them.
		m.logView.markCursor(prev, m.logCursor, stylesFor(false))
		m.logVP.SetContent(m.logView.content)
	}
	m.scrollToCursor()
	return m, nil
}

// toggleLogFold expands or collapses the block under the cursor.
func (m model) toggleLogFold() (tea.Model, tea.Cmd) {
	if m.logCursor < 0 {
		return m, nil
	}
	if m.logExpanded == nil {
		m.logExpanded = map[int]bool{}
	}
	m.logExpanded[m.logCursor] = !m.logExpanded[m.logCursor]
	m.rebuildLogView()
	m.scrollToCursor()
	return m, nil
}

// toggleLogExpandAll expands every foldable block, or collapses them all once
// none are left folded — one key for "show me everything" and back again,
// which is the common case when skimming a whole run rather than one step.
func (m model) toggleLogExpandAll() (tea.Model, tea.Cmd) {
	stops := m.logView.stops
	if len(stops) == 0 {
		return m, nil
	}
	expand := false
	for _, i := range stops {
		if !m.logExpanded[i] {
			expand = true
			break
		}
	}
	if m.logExpanded == nil {
		m.logExpanded = map[int]bool{}
	}
	for _, i := range stops {
		m.logExpanded[i] = expand
	}
	m.rebuildLogView()
	m.scrollToCursor()
	return m, nil
}

// topRowHeight sizes the wide layout's jobs/runs row to the content it
// actually holds instead of a blind fraction of the screen. The old fixed
// 40% split was wrong in both directions at once: with 21 jobs it showed 6
// of them (and gave no hint the other 15 existed) while the log pane sat
// empty below, and with 2 jobs it left a band of blank rows above a log
// that had been squeezed into the remainder.
func (m model) topRowHeight(avail int) int {
	// Sized to the *jobs* list, not to whichever of the two is longer: run
	// history is unbounded (a job firing every 10 minutes has hundreds of
	// runs), so letting it drive the height would peg the top row at its
	// cap forever — which is just the old fixed split by another name.
	// Jobs are few and you want to see all of them; runs scroll, and the
	// count in the pane title says how many there are.
	rows := len(m.jobs)
	// Unless you're actually reading run history, in which case that pane
	// is what the rows are for.
	if m.focus == focusRuns && m.runRowCount() > rows {
		rows = m.runRowCount()
	}
	if rows < 6 {
		rows = 6 // don't collapse to a sliver when there are few jobs
	}
	// +1 for the table's own header row, plus the box's border lines.
	want := rows + 1 + boxChromeY

	max := avail * 3 / 5
	min := 3 + boxChromeY // header + 3 data rows is the floor worth drawing
	if max < min {
		max = min
	}
	if want > max {
		want = max
	}
	if want < min {
		want = min
	}
	// Always leave the log something to occupy.
	if avail-want < 4 {
		want = avail - 4
	}
	if want < min {
		want = min
	}
	return want
}

// stackedHeights divides the screen among three full-width panes. Each pane
// gets what its content needs, capped so no pane starves the others, and
// the focused pane gets first claim on whatever is left over — on a screen
// this small, the pane you're looking at is the one worth the rows.
func (m model) stackedHeights(avail int) (jobs, runs, log int) {
	const minBox = 1 + boxChromeY + 1 // header + one data row, framed

	need := func(n int) int {
		if n < 1 {
			n = 1
		}
		return n + 1 + boxChromeY
	}
	jobs, runs = need(len(m.jobs)), need(m.runRowCount())
	log = minBox + 1

	cap := avail / 3
	if cap < minBox {
		cap = minBox
	}
	if jobs > cap {
		jobs = cap
	}
	if runs > cap {
		runs = cap
	}

	// Hand the remainder to whichever pane has focus; the log is the
	// natural home for anything still left after that, since it's the
	// pane whose content is unbounded.
	spare := avail - jobs - runs - log
	if spare > 0 {
		switch m.focus {
		case focusJobs:
			take := spare
			if want := need(len(m.jobs)) - jobs; take > want {
				take = want
			}
			if take < 0 {
				take = 0
			}
			jobs += take
			spare -= take
		case focusRuns:
			take := spare
			if want := need(m.runRowCount()) - runs; take > want {
				take = want
			}
			if take < 0 {
				take = 0
			}
			runs += take
			spare -= take
		}
		log += spare
	}

	// Too little room for all three at their floor: shrink the log first,
	// then the runs pane, so the jobs list (the entry point) survives.
	for jobs+runs+log > avail {
		switch {
		case log > minBox:
			log--
		case runs > minBox:
			runs--
		case jobs > minBox:
			jobs--
		default:
			return jobs, runs, avail - jobs - runs
		}
	}
	return jobs, runs, log
}

func setBoxHeight(t *grid, boxHeight int) {
	h := boxHeight - boxChromeY
	if h < 2 { // header + at least one data row
		h = 2
	}
	t.SetHeight(h)
}

func viewportHeight(boxHeight int) int {
	h := boxHeight - boxChromeY
	if h < 1 {
		h = 1
	}
	return h
}

// columnsWidth sums what SetColumns just laid out, so the table's viewport
// (which SetWidth controls) never clips a cell short of its own declared
// width — a mismatch there is what silently truncated column content
// mid-cell before this was fixed. +2 per column: each cell is padded one
// column on each side (perCellPadding), which is real rendered width the raw Column.Width doesn't include; missing
// it here under-sized the viewport by 2*len(cols) and silently clipped the
// last column entirely (found by actually screenshotting the TUI: the
// Status column's header showed but every row's value was gone).
func columnsWidth(cols []table.Column) int {
	w := 0
	for _, c := range cols {
		w += c.Width + 2
	}
	return w
}

// jobsColumns fits the jobs table to an available content width, keeping
// the most informative columns as the pane narrows. Drop order runs from
// the incidental to the essential: Runs (a count you can also get from the
// runs pane) goes first, then Kind (usually obvious from the job's name),
// then Next; ID and Status are the two things the dashboard exists to
// answer ("which job, and is it broken?"), so Status survives down to the
// last drop and ID never leaves at all.
func jobsColumns(width int) []table.Column {
	return fitColumns(width, jobsSpecs)
}

// jobsSpecs is the jobs table's full column set, richest first in
// importance. Cron is the "if there's room" column — genuinely useful, but
// the next fire time it produces is more useful still, so it only appears
// once everything else is comfortable.
var jobsSpecs = []colSpec{
	{title: "ID", min: 10, max: 0, grow: 6, drop: 0},
	{title: "Kind", min: 5, max: 5, grow: 0, drop: 3},
	{title: "Runs", min: 4, max: 5, grow: 0, drop: 4},
	{title: "Cron", min: 12, max: 16, grow: 0, drop: 5},
	{title: "Next", min: 11, max: 11, grow: 0, drop: 2},
	{title: "Status", min: 9, max: statusMaxWidth, grow: 1, drop: 1},
}

// runsColumns fits the runs table the same way: Status is the column the
// pane exists for and never leaves, Trigger (scheduled vs manual) is the
// first to go, then Dur, then Started.
func runsColumns(width int) []table.Column {
	return fitColumns(width, runsSpecs)
}

// runsSpecs is the runs table's column set. Trigger's min is 9, not 7,
// because "scheduled" is 9 characters and a Trigger column that renders
// "schedu…" is worse than no Trigger column at all — the whole point of
// dropping columns is to stop showing stumps.
var runsSpecs = []colSpec{
	{title: "Status", min: 9, max: statusMaxWidth, grow: 1, drop: 0},
	{title: "Trigger", min: 9, max: 9, grow: 0, drop: 3},
	{title: "Started", min: 11, max: 11, grow: 0, drop: 1},
	{title: "Dur", min: 9, max: 10, grow: 1, drop: 2},
	{title: "Exit", min: 4, max: 5, grow: 0, drop: 4},
}

// naturalBoxWidth is the box width at which every column in specs can have
// its max — beyond this the table gains nothing from extra columns.
func naturalBoxWidth(specs []colSpec) int {
	w := boxChromeX + gridGutter
	for _, s := range specs {
		want := s.max
		if want == 0 {
			want = s.min
		}
		w += want + perCellPadding
	}
	return w
}

// columnWidth returns the rendered width of the named column, or 0 if the
// current layout dropped it.
func columnWidth(cols []table.Column, title string) int {
	for _, c := range cols {
		if c.Title == title {
			return c.Width
		}
	}
	return 0
}

// setFocus moves focus between panes, keeping each table's own focus flag
// in step with it.
//
// That second part is a bug fix, not bookkeeping: table.Update returns
// immediately when its table isn't focused, and only the jobs table was ever
// focused (table.New(WithFocused(false)) for runs, and nothing ever called
// Focus). So ↑/↓ in the runs pane did nothing whatsoever — while the help bar
// advertised "↑↓ move" — and the pane was navigable by mouse alone, because
// the wheel and click paths call MoveUp/MoveDown/SetCursor directly and never
// go through table.Update. Found by a benchmark-adjacent test that tried to
// move the runs cursor with the keyboard and couldn't. Focus also picks
// which selection fill a table draws, so only one cursor reads as live.
func (m *model) setFocus(p focusPane) {
	m.focus = p
	m.jobsTable.Blur()
	m.runsTable.Blur()
	switch p {
	case focusJobs:
		m.jobsTable.Focus()
	case focusRuns:
		m.runsTable.Focus()
	}
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.selection = textSelection{}
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if m.form != nil {
		return m.handleFormKey(msg)
	}
	if m.cleanAsk != nil {
		switch msg.String() {
		case "y":
			jobID := m.cleanAsk.jobID
			m.cleanAsk = nil
			return m, cleanRunsCmd(m.ctx, m.st, jobID)
		case "n", "esc", "q":
			m.cleanAsk = nil
		}
		return m, nil
	}
	if m.showHelp {
		// Any key closes the key list; the quit keys still quit.
		if s := msg.String(); s == "q" || s == "ctrl+c" {
			return m, tea.Quit
		}
		m.showHelp = false
		return m, nil
	}
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.showHelp = true
		return m, nil
	case "esc", "h", "left":
		if m.focus > focusJobs {
			m.setFocus(m.focus - 1)
			m.layout() // focus steers the space split (see stackedHeights)
		}
		return m, nil
	case "enter":
		if m.focus == focusRuns && m.selectedRunEntry().group {
			return m.toggleRunGroup()
		}
		// In the log pane there is no deeper pane to open, so enter is the
		// fold toggle for the block under the cursor.
		if m.focus == focusLog {
			return m.toggleLogFold()
		}
		m.setFocus(m.focus + 1)
		m.layout()
		return m, nil
	case "l", "right":
		if m.focus < focusLog {
			m.setFocus(m.focus + 1)
			m.layout()
		}
		return m, nil
	case "n":
		if m.focus == focusJobs {
			return m.newJob(), nil
		}
	case "e":
		if m.focus == focusJobs {
			return m.editSelectedJob(), nil
		}
	case " ":
		if m.focus == focusJobs {
			return m.toggleEnabled()
		}
	case "c":
		if m.focus == focusRuns {
			return m.askClean(), nil
		}
	case "r":
		if m.focus == focusJobs {
			return m.runSelectedNow()
		}
		// In the runs and log panes the selection is a run, not a job, so
		// "r" means the run-shaped version of "run it": pick this agent
		// session back up interactively.
		return m.resumeSelectedRun()
	}

	var cmd tea.Cmd
	switch m.focus {
	case focusJobs:
		prevID := m.selectedJobID()
		m.jobsTable, cmd = m.jobsTable.Update(msg)
		if newID := m.selectedJobID(); newID != prevID && newID != "" {
			return m, tea.Batch(cmd, m.reloadRuns(newID))
		}
	case focusRuns:
		prevID, _ := m.selectedRun()
		m.runsTable, cmd = m.runsTable.Update(msg)
		if newID, path := m.selectedRun(); newID != prevID && newID != "" {
			return m, tea.Batch(cmd, m.reloadLog(newID, path))
		}
	case focusLog:
		// j/k walk the transcript's foldable blocks; ↑/↓ stay line scrolling,
		// so both ways of moving through a log remain available.
		switch msg.String() {
		case "j":
			return m.moveLogCursor(1, msg)
		case "k":
			return m.moveLogCursor(-1, msg)
		case "o":
			return m.toggleLogExpandAll()
		}
		m.logVP, cmd = m.logVP.Update(msg)
	}
	return m, cmd
}

// paneAt maps a terminal (X, Y) to the pane drawn there, and to that pane's
// own top edge so a row index can be computed relative to it. It reads the
// geometry layout() recorded rather than re-deriving any of it.
func (m model) paneAt(x, y int) (pane focusPane, paneTop int) {
	switch m.mode {
	case layoutFocused:
		// Only the focused pane is on screen; every click lands in it.
		return m.focus, 0

	case layoutStacked:
		switch {
		case y < m.jobsBoxHeight:
			return focusJobs, 0
		case y < m.jobsBoxHeight+m.runsBoxHeight:
			return focusRuns, m.jobsBoxHeight
		default:
			return focusLog, m.jobsBoxHeight + m.runsBoxHeight
		}

	default: // layoutWide
		if y >= m.topBoxHeight {
			return focusLog, m.topBoxHeight
		}
		if x < m.jobsBoxWidth {
			return focusJobs, 0
		}
		return focusRuns, 0
	}
}

// handleMouse maps a click/wheel event to a pane and, for a left-click, to
// a row within that pane's table.
func (m model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.form != nil || m.cleanAsk != nil {
		return m, nil // a modal is answered with keys; the dashboard behind it is inert
	}
	if m.showHelp {
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			m.showHelp = false
		}
		return m, nil
	}
	return m.selectWithMouse(msg)
}

func (m model) clickAt(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	pane, paneTop := m.paneAt(msg.X, msg.Y)

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		return m.scroll(pane, -1)
	case tea.MouseButtonWheelDown:
		return m.scroll(pane, 1)
	}

	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	if m.focus != pane {
		m.setFocus(pane)
		m.layout()
	}

	row, ok := rowAtY(msg.Y - paneTop)
	if !ok {
		return m, nil
	}
	switch pane {
	case focusJobs:
		row += m.jobsTable.Offset()
		if row >= len(m.jobs) {
			return m, nil
		}
		prevID := m.selectedJobID()
		m.jobsTable.SetCursor(row)
		if newID := m.selectedJobID(); newID != prevID && newID != "" {
			return m, m.reloadRuns(newID)
		}
	case focusRuns:
		row += m.runsTable.Offset()
		if row >= m.runRowCount() {
			return m, nil
		}
		prevID, _ := m.selectedRun()
		m.runsTable.SetCursor(row)
		if newID, path := m.selectedRun(); newID != prevID && newID != "" {
			return m, m.reloadLog(newID, path)
		}
	}
	return m, nil
}

// scroll handles a wheel event over a pane: moves the table cursor (jobs,
// runs) or scrolls the log viewport, and focuses whichever pane the wheel
// was over — matching ordinary "scroll wherever the mouse is" behavior.
func (m model) scroll(pane focusPane, dir int) (tea.Model, tea.Cmd) {
	if m.focus != pane {
		m.setFocus(pane)
		m.layout()
	}
	switch pane {
	case focusJobs:
		if dir < 0 {
			m.jobsTable.MoveUp(1)
		} else {
			m.jobsTable.MoveDown(1)
		}
		if newID := m.selectedJobID(); newID != "" {
			return m, m.reloadRuns(newID)
		}
	case focusRuns:
		if dir < 0 {
			m.runsTable.MoveUp(1)
		} else {
			m.runsTable.MoveDown(1)
		}
		if newID, path := m.selectedRun(); newID != "" {
			return m, m.reloadLog(newID, path)
		}
	case focusLog:
		if dir < 0 {
			m.logVP.LineUp(1)
		} else {
			m.logVP.LineDown(1)
		}
	}
	return m, nil
}

// rowAtY maps a y offset *within a pane* to that pane's body-row index.
// Two rows of chrome sit above the first data row: the box's top border
// and the table's own header.
//
// This was 3 while the pane title occupied a content row of its own. Moving
// the title into the border rule removed that row but left the constant
// behind, so every click resolved one row too high and the first row in
// each table couldn't be clicked at all (y=2 mapped to -1, which this
// function rejects). Caught by rendering a real frame and counting: with
// the title in the border, row 0 is the border, row 1 the header, row 2 the
// first job.
//
// The caller adds the table's scroll offset: a click on the first visible
// row of a scrolled table is not row 0.
func rowAtY(y int) (int, bool) {
	const chrome = 2
	row := y - chrome
	if row < 0 {
		return 0, false
	}
	return row, true
}

func (m model) toggleEnabled() (tea.Model, tea.Cmd) {
	j := m.selectedJob()
	if j == nil {
		return m, nil
	}
	_ = m.st.SetEnabled(m.ctx, j.ID, !j.Enabled)
	return m, m.reloadJobs()
}

func (m model) runSelectedNow() (tea.Model, tea.Cmd) {
	j := m.selectedJob()
	if j == nil || m.running[j.ID] {
		return m, nil
	}
	m.running[j.ID] = true
	m.statusMsg = fmt.Sprintf("running %s...", j.ID)
	return m, runNowCmd(m.ctx, m.st, *j, m.logsDir)
}

func (m model) selectedJob() *store.Job {
	idx := m.jobsTable.Cursor()
	if idx < 0 || idx >= len(m.jobs) {
		return nil
	}
	return &m.jobs[idx].Job
}

func (m model) selectedJobID() string {
	if j := m.selectedJob(); j != nil {
		return j.ID
	}
	return ""
}

func (m model) selectedJobKind() string {
	if j := m.selectedJob(); j != nil {
		return j.Kind
	}
	return ""
}

func (m model) selectedJobProvider() string {
	if j := m.selectedJob(); j != nil {
		return j.Provider
	}
	return ""
}

func (m *model) selectJobByID(id string) {
	for i, j := range m.jobs {
		if j.ID == id {
			m.jobsTable.SetCursor(i)
			return
		}
	}
}

// selectedRunRow is the whole selected run, where selectedRun returns only
// the two fields the log pane needs. Resuming needs the session id too.
func (m model) selectedRunRow() *store.Run {
	idx := m.selectedRunEntry().first
	if idx < 0 || idx >= len(m.runs) {
		return nil
	}
	return &m.runs[idx]
}

// jobForRun finds the job a run belongs to by the run's own JobID rather
// than trusting the jobs cursor. The two disagree for a moment every time
// the selection moves: the runs on screen still belong to the previously
// selected job until the load answering the change arrives (see runsJobID).
func (m model) jobForRun(r *store.Run) *store.Job {
	if r == nil {
		return nil
	}
	for i := range m.jobs {
		if m.jobs[i].Job.ID == r.JobID {
			return &m.jobs[i].Job
		}
	}
	return nil
}

// canResumeSelection decides whether to advertise "r resume" at all, so the
// key is only offered where it does something (see hintsFor).
func (m model) canResumeSelection() bool {
	r := m.selectedRunRow()
	j := m.jobForRun(r)
	return j != nil && resume.Possible(*j, *r)
}

func (m model) resumeSelectedRun() (tea.Model, tea.Cmd) {
	r := m.selectedRunRow()
	if r == nil {
		return m, nil
	}
	j := m.jobForRun(r)
	if j == nil {
		return m, nil
	}
	if why := resume.Reason(*j, *r); why != "" {
		m.statusMsg = "cannot resume: " + why
		return m, nil
	}
	if m.resuming[r.ID] {
		return m, nil // already opening a tab for this run
	}
	m.resuming[r.ID] = true
	m.statusMsg = "resuming..."
	return m, resumeCmd(*j, *r)
}

func (m model) selectedRun() (id string, logPath string) {
	if r := m.selectedRunRow(); r != nil {
		return r.ID, r.LogPath
	}
	return "", ""
}

func (m model) isSelectedRunRunning() bool {
	if r := m.selectedRunRow(); r != nil {
		return r.Status == "running"
	}
	return false
}

func (m *model) selectRunByID(id string) {
	for i := range m.runRowCount() {
		e := m.runEntryAt(i)
		for j := e.first; j < e.end; j++ {
			if m.runs[j].ID == id {
				// Prefer the individual row when this group is expanded.
				if e.group && m.runExpanded[e.key] {
					continue
				}
				m.runsTable.SetCursor(i)
				return
			}
		}
	}
}

func newRunID() string {
	return time.Now().UTC().Format("20060102T150405.000000000")
}

// jobRows builds the jobs table's rows to match the columns the current
// layout actually kept.
//
// A row must have exactly one cell per column: the table walks the row's
// cells and the column set by the same position, so building rows from the
// column set makes the two impossible to get out of step.
//
// A disabled job is drawn faint from end to end, with "disabled" where its
// next fire time would be — the word carries it where color can't. (Each
// row used to lead with ● or ○ for this, but with every job enabled that
// was the same glyph on every row, and it spent two columns of the job's
// name to say nothing.)
func jobRows(jobs []store.JobSummary, cols []table.Column) []table.Row {
	rows := make([]table.Row, 0, len(jobs))
	for _, j := range jobs {
		status := j.LastStatus
		if status == "" {
			status = "never run"
		}
		rows = append(rows, cellsFor(cols, func(title string, width int) string {
			var v string
			switch title {
			case "ID":
				v = j.ID
			case "Kind":
				v = j.Kind
			case "Runs":
				v = fmt.Sprint(j.RunCount)
			case "Cron":
				v = j.Cron
			case "Next":
				v = nextRunLabel(j)
			case "Status":
				if !j.Enabled {
					return faintStyle.Render(statusLabel(status, width))
				}
				return statusCell(status, width)
			}
			if !j.Enabled {
				return faintStyle.Render(v)
			}
			return v
		}))
	}
	return rows
}

// cellsFor builds one row by asking for each surviving column's value in
// the order the columns are rendered.
func cellsFor(cols []table.Column, value func(title string, width int) string) table.Row {
	row := make(table.Row, 0, len(cols))
	for _, c := range cols {
		row = append(row, value(c.Title, c.Width))
	}
	return row
}

func nextRunLabel(j store.JobSummary) string {
	if !j.Enabled {
		return "disabled"
	}
	lastFire := j.CreatedAt
	if j.LastRunAt.Valid {
		lastFire = j.LastRunAt.Time
	}
	next, err := cronx.Next(j.Cron, j.Timezone, lastFire)
	if err != nil {
		return "?"
	}
	return next.Local().Format("01-02 15:04")
}

// runRows builds the runs table's rows. Exit is shown only when it says
// something: a 0 beside every "✓ ok" was the same fact twice, so the column
// now holds just the codes worth reading.
func runRows(runs []store.Run, cols []table.Column) []table.Row {
	rows := make([]table.Row, 0, len(runs))
	for _, r := range runs {
		rows = append(rows, cellsFor(cols, func(title string, width int) string {
			switch title {
			case "Status":
				return statusCell(r.Status, width)
			case "Trigger":
				return r.Trigger
			case "Started":
				return r.StartedAt.Local().Format("01-02 15:04")
			case "Dur":
				return runDuration(r)
			case "Exit":
				if !r.ExitCode.Valid || r.ExitCode.Int64 == 0 {
					return ""
				}
				return statusFailed.Render(fmt.Sprint(r.ExitCode.Int64))
			}
			return ""
		}))
	}
	return rows
}

// runDuration mirrors cmd_job.go's helper of the same name (measured
// duration_ms when present; falls back to the timestamp difference only
// for runs recorded before that field existed — see store's
// additiveMigrations comment for why the two aren't interchangeable).
// A run still going shows how long it has been going, so a stuck one is
// visible as a number that keeps climbing rather than a dash.
func runDuration(r store.Run) string {
	// Checked first: a run in progress already carries a duration_ms of 0.
	switch {
	case r.Status == "running":
		// Whole seconds: the column ticks once a second, and "0ms" or "3.0s"
		// would claim a precision a live counter doesn't have.
		d := time.Since(r.StartedAt)
		if d < time.Minute {
			return fmt.Sprintf("%ds", int(d.Seconds()))
		}
		return humanDuration(d.Truncate(time.Second))
	case r.DurationMs.Valid:
		return humanDuration(time.Duration(r.DurationMs.Int64) * time.Millisecond)
	case r.FinishedAt.Valid:
		return humanDuration(r.FinishedAt.Time.Sub(r.StartedAt))
	}
	return "-"
}

// statusMaxWidth is the widest status cell: a folded stack of skips,
// "+ skipped ×9".
const statusMaxWidth = 12

// statusGlyph is the shape half of a status: every status is a glyph, a word
// and a color, so it still reads with any one of the three missing — the
// word in a column too narrow for it, the color under NO_COLOR or in a
// faint disabled row. Glyphs are from the single-width set that every
// terminal font draws at one cell.
func statusGlyph(s string) string {
	switch s {
	case "ok":
		return "✓"
	case "failed", "timeout":
		return "✗"
	case "running":
		return "●"
	default: // never run, skipped, skipped_overlap: nothing ran
		return "·"
	}
}

// statusCell is a status as the tables show it: glyph and word, colored.
func statusCell(s string, width int) string {
	return statusStyle(s).Render(statusLabel(s, width))
}

// statusLabel is the glyph and the word, the word shortened to fit a narrow
// column rather than letting the table chop it into a stump ("fa…" is no
// shorter to read than "fail" and considerably less clear), and dropped
// altogether when even that won't fit: the glyph alone still says it.
func statusLabel(s string, width int) string {
	g := statusGlyph(s)
	word := s
	switch s {
	case "skipped_overlap":
		word = "overlap"
	}
	if width <= 0 || lipgloss.Width(g+" "+word) <= width {
		return g + " " + word
	}
	short := map[string]string{
		"failed": "fail", "timeout": "time", "running": "run", "never run": "never",
		"skipped": "skip", "skipped_overlap": "skip",
	}[s]
	if short != "" && lipgloss.Width(g+" "+short) <= width {
		return g + " " + short
	}
	return g
}

func statusStyle(s string) lipgloss.Style {
	switch s {
	case "ok":
		return statusOK
	case "failed", "timeout":
		return statusFailed
	case "running":
		return statusRunning
	default:
		// Includes "skipped" and "skipped_overlap": nothing ran, so the
		// row should read as inert rather than as a result.
		return statusNever
	}
}

func (m model) View() string {
	if m.selection.active {
		return m.selection.render()
	}
	if m.width > 0 && (m.width < minWidth || m.height < minHeight) {
		return tooSmallView(m.width, m.height)
	}
	if m.showHelp && m.width > 0 {
		return m.helpView()
	}
	if m.width > 0 && m.form != nil {
		return overlay(m.dashboardView(), m.form.view(m.width, m.height), m.width, m.height)
	}
	if m.width > 0 && m.cleanAsk != nil {
		return overlay(m.dashboardView(), m.cleanAsk.view(m.width), m.width, m.height)
	}
	return m.dashboardView()
}

// handleFormKey hands a key to the open form and acts on what it asks for.
func (m model) handleFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	res, cmd := m.form.update(msg)
	switch res {
	case formClose:
		m.form = nil
	case formSave:
		m = m.saveForm()
		if m.form == nil {
			return m, m.reloadJobs()
		}
	}
	return m, cmd
}

// minWidth and minHeight are the smallest terminal the single-pane layout
// can draw without clipping: a framed table with its header, three rows,
// and an ID and a status column that still read as themselves. Below them
// the frame used to be cut off at the top — at 20x5 the title and the
// column headers were simply gone — with nothing saying why.
const (
	minWidth  = 28
	minHeight = 7
)

// tooSmallView replaces the dashboard when the terminal is under the
// minimum: what is short, by how much, and how to leave. Everything is said
// in words as well as color.
func tooSmallView(w, h int) string {
	dim := func(label string, have, need int) string {
		mark, st := "✓", statusOK
		if have < need {
			mark, st = "✗", statusFailed
		}
		return st.Render(fmt.Sprintf("%s %s %d", mark, label, have)) +
			helpStyle.Render(fmt.Sprintf(", needs %d", need))
	}
	lines := []string{
		helpKeyStyle.Render("Terminal too small"),
		dim("width", w, minWidth),
		dim("height", h, minHeight),
		helpKeyStyle.Render("q") + helpStyle.Render(" quit"),
	}
	if len(lines) > h {
		lines = lines[1:min(len(lines), 1+h)] // the numbers beat the heading
	}
	for i, l := range lines {
		lines[i] = truncateToWidth(l, w)
	}
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, strings.Join(lines, "\n"))
}

func (m model) dashboardView() string {
	if m.width == 0 {
		return "loading..."
	}

	body := ""
	switch m.mode {
	case layoutFocused:
		// One level of the drill-down at a time. The back-arrow in the
		// title is the affordance that says esc/h will take you up.
		switch m.focus {
		case focusJobs:
			body = m.jobsPane(m.jobsBoxHeight)
		case focusRuns:
			body = m.runsPane(m.runsBoxHeight)
		default:
			body = m.logPane(m.topBoxHeight)
		}

	case layoutStacked:
		body = lipgloss.JoinVertical(lipgloss.Left,
			m.jobsPane(m.jobsBoxHeight),
			m.runsPane(m.runsBoxHeight),
			m.logPane(m.height-1-m.jobsBoxHeight-m.runsBoxHeight),
		)

	default: // layoutWide
		top := lipgloss.JoinHorizontal(lipgloss.Top,
			m.jobsPane(m.topBoxHeight), m.runsPane(m.topBoxHeight))
		body = lipgloss.JoinVertical(lipgloss.Left, top,
			m.logPane(m.height-1-m.topBoxHeight))
	}

	return body + "\n" + renderHints(m.width, m.footerStatus(), m.hints())
}

// footerStatus is the message shown between the key hints: a dashboard
// error if there is one — a reload that failed used to be stored and never
// shown, leaving a stale screen that looked live — otherwise the last
// action's outcome.
func (m model) footerStatus() string {
	if m.err != nil {
		return statusFailed.Render("✗ " + m.err.Error())
	}
	if m.statusMsg == "" {
		return ""
	}
	return helpStyle.Render(m.statusMsg)
}

// hints is the footer for the current pane and selection.
func (m model) hints() []helpHint {
	hints := hintsFor(m.focus, m.canResumeSelection(), len(m.logView.stops) > 0)
	if m.focus == focusRuns && m.selectedRunEntry().group {
		hints = []helpHint{
			{key: "↑↓", long: "move", short: "move", drop: 6},
			{key: "enter", long: "fold skips", short: "fold", drop: 2},
			{key: "→", long: "latest log", short: "log", drop: 4},
			{key: "c", long: "clean runs", short: "clean", drop: 7},
			{key: "esc", long: "back", short: "back", drop: 3},
		}
		hints = append(hints, globalHints...)
	}
	return append(hints, helpHint{key: "drag", long: "select & copy", short: "copy", drop: 9})
}

func (m model) jobsPane(boxHeight int) string {
	content := m.jobsTable.View()
	if len(m.jobs) == 0 {
		content = m.emptyBody(
			styleMessage(noJobsMessage(m.jobsBoxWidth-boxChromeX, boxHeight-boxChromeY)),
			m.jobsBoxWidth, boxHeight)
	}
	return renderPane(m.paneTitle("Jobs"), position(m.jobsTable.Cursor(), len(m.jobs)),
		accentJobs, m.focus == focusJobs, content)
}

func (m model) runsPane(boxHeight int) string {
	title := "Runs"
	if id := m.selectedJobID(); id != "" {
		title = "Runs · " + id
	}
	content := m.runsTable.View()
	if len(m.runs) == 0 {
		msg := []string{"No runs yet.", "", "Press r on a job to run it now."}
		if m.selectedJobID() == "" {
			msg = []string{"No job selected."}
		}
		if !fits(msg, m.runsBoxWidth-boxChromeX, boxHeight-boxChromeY) {
			msg = []string{"No runs yet."}
		}
		content = m.emptyBody(styleMessage(msg), m.runsBoxWidth, boxHeight)
	}
	return renderPane(m.paneTitle(title), position(m.runsTable.Cursor(), m.runRowCount()),
		accentRuns, m.focus == focusRuns, content)
}

func (m model) logPane(boxHeight int) string {
	// The run id goes first when the border is short of room: it is the
	// least readable part, and keeping it used to push everything after it —
	// including the scroll position — off the end of the rule.
	head, id := m.logTitleParts()
	title := m.paneTitle(head)
	if id != "" {
		if full := m.paneTitle(head + " · " + id); titleFits(full, m.width) {
			title = full
		}
	}
	// The tables say where you are via "3/21"; the log needs its own
	// version of that, or a long transcript gives no clue that there's
	// more above or below the visible slice. Only shown when it's
	// actually scrollable.
	pos := ""
	if m.logVP.TotalLineCount() > m.logVP.Height {
		pos = fmt.Sprintf("%d%%", int(m.logVP.ScrollPercent()*100))
	}
	return renderPane(title, pos, accentLog, m.focus == focusLog, m.logVP.View())
}

// position is a list pane's "where am I" label, drawn in its bottom border:
// "Jobs 3/21" is the difference between a list that happens to show six
// rows and a list that has fifteen more you can't see.
func position(cursor, total int) string {
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%d/%d", cursor+1, total)
}

// logPaneTitle identifies whose output the log pane shows. Run IDs alone
// ("20260928T101532.123456789") don't say which job produced them, and
// visually scanning several similar timestamps is how you end up reading
// the wrong run. The title therefore carries the job name and the run's
// local start time, mirroring the Runs pane columns, so the pane answers
// "what am I looking at" on its own.
func (m model) logPaneTitle() string {
	head, id := m.logTitleParts()
	if id == "" {
		return head
	}
	return head + " · " + id
}

// logTitleParts splits the log title into the part that must survive a
// narrow border and the run id, which may be dropped.
func (m model) logTitleParts() (head, id string) {
	run := m.selectedRunWithMeta()
	if run == nil {
		return "Log", ""
	}
	name := run.JobID
	if job := m.jobByID(run.JobID); job != nil {
		name = job.ID
	}
	if e := m.selectedRunEntry(); e.group {
		return fmt.Sprintf("Log · %s · %d skips %s–%s", name, e.end-e.first,
			m.runs[e.end-1].StartedAt.Local().Format("01-02 15:04"),
			run.StartedAt.Local().Format("01-02 15:04")), "latest: " + run.ID
	}
	return fmt.Sprintf("Log · %s · %s", name, run.StartedAt.Local().Format("01-02 15:04:05")), run.ID
}

func (m model) selectedRunWithMeta() *store.Run {
	id, _ := m.selectedRun()
	return m.runByID(id)
}

func (m model) runByID(id string) *store.Run {
	for i := range m.runs {
		if m.runs[i].ID == id {
			return &m.runs[i]
		}
	}
	return nil
}

func (m model) jobByID(id string) *store.JobSummary {
	for i := range m.jobs {
		if m.jobs[i].ID == id {
			return &m.jobs[i]
		}
	}
	return nil
}

// paneTitle labels a pane. In the single-pane layout the title also carries
// a back-arrow, since the pane you came from is no longer on screen to imply
// it.
func (m model) paneTitle(label string) string {
	if m.mode == layoutFocused && m.focus > focusJobs {
		label = "◂ " + label
	}
	return label
}

// titleFits reports whether embedTitle can show title whole in a box of the
// given outer width: a corner and a rule rune either side, and the label's
// own two spaces.
func titleFits(title string, boxWidth int) bool {
	return lipgloss.Width(title) <= boxWidth-5
}

// emptyBody fits an empty-state message to the space the pane's table would
// have filled, so the box keeps exactly the size the layout gave it.
//
// Both clamps matter. Without MaxHeight an over-long message makes the box
// taller than its neighbor, so the two top panes stop lining up and the
// pane boundaries the mouse code was told about no longer match what's
// drawn. Without MaxWidth a long line wraps and costs a row it wasn't
// budgeted.
func (m model) emptyBody(lines []styledLine, boxWidth, boxHeight int) string {
	h := boxHeight - boxChromeY
	if h < 1 {
		h = 1
	}
	w := boxWidth - boxChromeX
	if w < 1 {
		w = 1
	}
	// Style each line on its own. Rendering a whole multi-line block
	// through one lipgloss style pads every line out to the block's widest
	// — including blank ones — so anything appended afterwards starts in
	// the middle of that padding instead of at the left margin.
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l.text == "" {
			out = append(out, "")
			continue
		}
		out = append(out, l.style.Render(l.text))
	}
	return lipgloss.NewStyle().Width(w).Height(h).
		MaxWidth(w).MaxHeight(h).
		Render(strings.Join(out, "\n"))
}

type styledLine struct {
	text  string
	style lipgloss.Style
}

// noJobsMessage is the first thing anyone sees after installing jobtail,
// when the database is still empty — previously three blank boxes with no
// hint of what to do next. It names the command to type, and falls back to
// a shorter form rather than wrapping when the pane is narrow.
// It is sized against the box's height as well as its width: clipping a
// message that ends with the one actionable line ("then press r") to make
// it fit would throw away the part worth reading, so a box too short for
// the full version gets the compact one whole.
func noJobsMessage(width, height int) []string {
	candidates := [][]string{{
		"No jobs yet. Press n to create one,",
		"or from the shell:",
		"  jobtail add my-check \\",
		"    --kind cli \\",
		"    --cron \"*/15 * * * *\" \\",
		"    --cwd ~ --cmd \"./check.sh\"",
		"",
		"Then press r here to run it now.",
	}, {
		"No jobs yet.",
		"",
		"Press n to create one,",
		"then r to run it.",
	}, {
		"No jobs yet.",
		"Press n to add one.",
	}, {
		"No jobs yet.",
	}}
	for _, c := range candidates {
		if fits(c, width, height) {
			return c
		}
	}
	return candidates[len(candidates)-1]
}

func fits(lines []string, width, height int) bool {
	if len(lines) > height {
		return false
	}
	for _, l := range lines {
		if lipgloss.Width(l) > width {
			return false
		}
	}
	return true
}

// styleMessage marks up an empty-state message: anything indented is a
// command to type, so it gets the brighter of the two muted tones.
func styleMessage(lines []string) []styledLine {
	out := make([]styledLine, 0, len(lines))
	for _, l := range lines {
		style := emptyStyle
		if strings.HasPrefix(l, "  ") {
			style = emptyCmdStyle
		}
		out = append(out, styledLine{text: l, style: style})
	}
	return out
}

// renderPane draws one pane's box with a rounded, per-pane-accented border
// (quiet when unfocused, full accent + bold title when focused — btop
// itself has no focus concept, so this is jobtail's own adaptation to keep
// the existing focus affordance while adopting btop's per-panel color
// identity), its title embedded in the top border rule rather than as a
// separate content row (see embedTitle), and pos — the pane's scroll or
// cursor position, if any — at the right end of the bottom rule, where it
// stays out of the way of the title however long that gets.
func renderPane(title, pos string, accent lipgloss.Color, focused bool, content string) string {
	borderColor, labelStyle := borderDefault, lipgloss.NewStyle().Foreground(fgMuted)
	if focused {
		borderColor, labelStyle = accent, lipgloss.NewStyle().Foreground(accent).Bold(true)
	}
	box := roundedPane.BorderForeground(borderColor).Render(content)
	box = embedTitle(box, title, labelStyle)
	if pos != "" {
		box = embedPosition(box, pos, lipgloss.NewStyle().Foreground(fgMuted))
	}
	return box
}

// borderLineRE matches a top border line lipgloss rendered with a single
// BorderForeground color: one opening SGR sequence, the border rune run,
// one closing reset. Verified directly against lipgloss v1.1.0's actual
// output (never per-character escapes) before relying on this — see the
// embedTitle comment.
var borderLineRE = regexp.MustCompile(`^(\x1b\[[0-9;]*m)(.*)(\x1b\[0m)$`)

// embedTitle splices a title into a rendered box's already-drawn top
// border line — btop's signature look, a title inline in the border rule
// itself rather than a separate content row above it — since lipgloss has
// no built-in support for this.
//
// This is safe specifically *because* BorderForeground wraps the whole
// border line in one escape/reset pair, never per-rune (confirmed via a
// real pty capture of lipgloss's actual bytes, not assumed): the runes
// between that opening escape and the closing reset are plain border
// characters with no embedded codes, so slicing them by rune index can't
// corrupt anything. The label's own style ends with its own reset, which
// clears ALL active SGR state, not just its own — so the opening border
// color has to be re-emitted after the label, not assumed to still be
// active, or the rest of the border would render in the default color.
//
// If jobtail ever runs with color disabled, the border line carries no
// escape codes at all and borderLineRE simply won't match; the whole line
// is then treated as plain body text, so the title still gets spliced in,
// just uncolored.
func embedTitle(box, title string, labelStyle lipgloss.Style) string {
	lines := strings.SplitN(box, "\n", 2)
	top := lines[0]

	prefix, body, suffix := "", top, ""
	if m := borderLineRE.FindStringSubmatch(top); m != nil {
		prefix, body, suffix = m[1], m[2], m[3]
	}

	runes := []rune(body)
	const start = 1
	// Shorten the title to what the border can hold rather than dropping
	// it: bailing out left panes with a blank top rule on narrow terminals
	// — at 50 columns the runs pane had no title at all, so nothing said
	// which job's runs were on screen. Reserve one border rune on each
	// side plus the label's own two spaces.
	if avail := len(runes) - start - 3; avail < len([]rune(title)) {
		if avail < 2 {
			return box // genuinely no room for even a stub
		}
		title = string([]rune(title)[:avail-1]) + "…"
	}
	label := " " + title + " "
	labelRunes := []rune(label)
	end := start + len(labelRunes)
	if end >= len(runes) {
		return box
	}

	newTop := prefix + string(runes[:start]) + labelStyle.Render(label) + prefix + string(runes[end:]) + suffix
	if len(lines) == 1 {
		return newTop
	}
	return newTop + "\n" + lines[1]
}

// embedPosition splices label into the right end of a box's bottom border,
// the same way embedTitle does the top one. It leaves the box alone when
// the rule is too short to hold it.
func embedPosition(box, label string, style lipgloss.Style) string {
	i := strings.LastIndex(box, "\n")
	if i < 0 {
		return box
	}
	bottom := box[i+1:]
	prefix, body, suffix := "", bottom, ""
	if m := borderLineRE.FindStringSubmatch(bottom); m != nil {
		prefix, body, suffix = m[1], m[2], m[3]
	}
	runes := []rune(body)
	l := []rune(" " + label + " ")
	end := len(runes) - 2 // keep the corner and one rule rune after it
	start := end - len(l)
	if start < 2 {
		return box
	}
	return box[:i+1] + prefix + string(runes[:start]) + style.Render(string(l)) +
		prefix + string(runes[end:]) + suffix
}

// helpHint is one key hint, with a shorter label to fall back on and a drop
// priority (highest goes first when the bar won't fit). Global hints — keys
// that do the same thing everywhere — sit at the right end of the bar, apart
// from the ones for the pane in hand.
type helpHint struct {
	key, long, short string
	drop             int
	global           bool
}

// globalHints close every footer. Quitting — the one thing a user must
// always be able to discover — never drops, and help goes next to last,
// since it lists everything the narrower bars had to leave out.
var globalHints = []helpHint{
	{key: "?", long: "help", short: "help", drop: 1, global: true},
	{key: "q", long: "quit", short: "quit", drop: 0, global: true},
}

// hintsFor returns the hints that actually do something in the pane you're
// in, in display order; drop order is separate. Each pane's own primary
// action outlasts the generic ones: in the log, the fold keys used to be
// the first to go, so at 60 columns the bar still said "drag copy" but no
// longer said how to open a tool call.
//
// The bar used to be a fixed list, which advertised keys that the pane
// silently ignored: "e enable/disable" and "r run now" only act on the jobs
// table (handleKey guards both), so showing them while reading a log was
// simply wrong. It also said "esc back" on the jobs pane, where there is
// nothing to go back to, and "enter open" on the log pane, where there is
// nothing deeper to open.
// canResume follows the same rule: "r resume" only acts on an agent run
// that captured a session id, so a cli job's runs — or an agent run that
// died before its session id arrived — must not advertise it.
func hintsFor(focus focusPane, canResume, canFold bool) []helpHint {
	resumeHint := helpHint{key: "r", long: "resume session", short: "resume", drop: 5}
	var hints []helpHint
	switch focus {
	case focusRuns:
		hints = []helpHint{
			{key: "↑↓", long: "move", short: "move", drop: 6},
			{key: "enter", long: "open log", short: "log", drop: 2},
		}
		if canResume {
			hints = append(hints, resumeHint)
		}
		hints = append(hints,
			helpHint{key: "c", long: "clean runs", short: "clean", drop: 7},
			helpHint{key: "esc", long: "back", short: "back", drop: 3})
	case focusLog:
		hints = []helpHint{
			{key: "↑↓", long: "scroll", short: "scroll", drop: 8},
		}
		// The fold keys are only advertised when the log on screen actually has
		// something folded in it: a cli job's output and a short transcript
		// have no blocks to step through, and the same rule that keeps "r" off
		// a cli run keeps these off too.
		if canFold {
			hints = append(hints,
				helpHint{key: "jk", long: "step", short: "step", drop: 4},
				helpHint{key: "enter", long: "expand", short: "expand", drop: 2},
				helpHint{key: "o", long: "expand all", short: "all", drop: 7},
			)
		}
		if canResume {
			hints = append(hints, resumeHint)
		}
		hints = append(hints, helpHint{key: "esc", long: "back", short: "back", drop: 3})
	default:
		hints = []helpHint{
			{key: "↑↓", long: "move", short: "move", drop: 6},
			{key: "enter", long: "runs", short: "runs", drop: 2},
			{key: "r", long: "run now", short: "run", drop: 3},
			{key: "n", long: "new", short: "new", drop: 4},
			{key: "e", long: "edit", short: "edit", drop: 5},
			{key: "space", long: "on/off", short: "on/off", drop: 7},
		}
	}
	return append(hints, globalHints...)
}

// renderHelpBar is the bottom key-hint bar, styled like btop's footer: each
// key highlighted, its description dimmed.
//
// It fits itself to the terminal instead of letting Bubble Tea crop it.
// Cropping silently ate the end of the bar on any terminal under ~88
// columns — at 80 the "q quit" hint was gone entirely (so nothing on screen
// said how to exit), and at 60 it cut mid-word to "e enable/dis". Hints now
// shorten, then drop whole, worst-priority first.
func renderHelpBar(width int, focus focusPane, statusMsg string, canResume, canFold bool) string {
	if statusMsg != "" {
		statusMsg = helpStyle.Render(statusMsg)
	}
	return renderHints(width, statusMsg, hintsFor(focus, canResume, canFold))
}

// minorHintDrop is the drop priority from which a hint gives way to a status
// message: the reminders, not the pane's actions.
const minorHintDrop = 7

// renderHints lays the pane's hints out from the left and the global ones
// against the right edge, with status (already styled) between them when
// there is room for it; the key hints matter more than the message.
func renderHints(width int, status string, hints []helpHint) string {
	key := func(h helpHint, short bool) string {
		label := h.long
		if short {
			label = h.short
		}
		return helpKeyStyle.Render(h.key) + helpStyle.Render(" "+label)
	}
	sep := helpStyle.Render("  ·  ")
	sepShort := helpStyle.Render(" · ")

	build := func(keep []bool, short bool, s string) (left, right string) {
		var l, r []string
		for i, h := range hints {
			if !keep[i] {
				continue
			}
			if h.global {
				r = append(r, key(h, short))
			} else {
				l = append(l, key(h, short))
			}
		}
		return strings.Join(l, s), strings.Join(r, s)
	}
	layout := func(left, right string) (string, bool) {
		wl, wr := lipgloss.Width(left), lipgloss.Width(right)
		gap := 0
		if wl > 0 && wr > 0 {
			gap = 3
		}
		if wl+gap+wr > width {
			return "", false
		}
		if status != "" {
			lead := "   "
			if wl == 0 {
				lead = ""
			}
			if wl+len(lead)+lipgloss.Width(status)+gap+wr <= width {
				left += lead + status
				wl = lipgloss.Width(left)
			}
		}
		return left + strings.Repeat(" ", width-wl-wr) + right, true
	}

	keep := make([]bool, len(hints))
	for i := range keep {
		keep[i] = true
	}
	// An action's outcome ("created job x", "cleaned 12 runs") outranks the
	// hints that are only reminders — copy by drag, the on/off toggle — so
	// those step aside for it before it would be the one left out.
	if status != "" {
		trial := slices.Clone(keep)
		for {
			l, r := build(trial, false, sep)
			if wl, wr := lipgloss.Width(l), lipgloss.Width(r); wl+3+lipgloss.Width(status)+3+wr <= width {
				if bar, ok := layout(l, r); ok {
					return bar
				}
			}
			worst, worstDrop := -1, minorHintDrop-1
			for i, h := range hints {
				if trial[i] && h.drop > worstDrop {
					worst, worstDrop = i, h.drop
				}
			}
			if worst < 0 {
				break
			}
			trial[worst] = false
		}
	}
	// Try progressively smaller renderings, most generous first.
	for {
		for _, attempt := range []struct {
			short bool
			s     string
		}{{false, sep}, {true, sep}, {true, sepShort}} {
			if bar, ok := layout(build(keep, attempt.short, attempt.s)); ok {
				return bar
			}
		}
		worst, worstDrop := -1, 0
		for i, h := range hints {
			if keep[i] && h.drop > worstDrop {
				worst, worstDrop = i, h.drop
			}
		}
		if worst < 0 {
			// Only the never-drop hint is left and it still doesn't fit;
			// a terminal this narrow gets whatever of it will show.
			l, r := build(keep, true, sepShort)
			return truncateToWidth(l+r, width)
		}
		keep[worst] = false
	}
}

// helpView is the full key list, opened with ?. The footer has to drop
// hints to fit a narrow terminal; this is where the dropped ones still are.
// Two columns when the terminal has the width, one when it doesn't.
func (m model) helpView() string {
	type entry struct{ keys, what string }
	type section struct {
		title  string
		accent lipgloss.Color
		rows   []entry
	}
	jobs := section{"Jobs", accentJobs, []entry{
		{"↑↓ jk", "move"}, {"enter l", "open runs"}, {"r", "run now"},
		{"n", "new job"}, {"e", "edit job"}, {"space", "enable / disable"},
	}}
	runs := section{"Runs", accentRuns, []entry{
		{"↑↓ jk", "move"}, {"enter l", "open log"}, {"enter", "fold a stack of skips"},
		{"r", "resume agent session"}, {"c", "clean finished runs"}, {"esc h", "back to jobs"},
	}}
	logs := section{"Log", accentLog, []entry{
		{"↑↓", "scroll"}, {"pgup pgdn", "page"}, {"j k", "step through tool calls"},
		{"enter", "expand / collapse step"}, {"o", "expand / collapse all"},
		{"r", "resume agent session"}, {"esc h", "back to runs"},
	}}
	mouse := section{"Mouse", fgMuted, []entry{
		{"click", "focus pane, pick row"}, {"wheel", "move / scroll"}, {"drag", "select & copy text"},
	}}
	anywhere := section{"Anywhere", fgMuted, []entry{{"?", "this help"}, {"q ctrl+c", "quit"}}}
	form := section{"Job form", accentJobs, []entry{
		{"↑↓ tab", "move between fields"}, {"enter", "type into a field"},
		{"←→ space", "choose an option"}, {"esc", "undo the field"},
		{"s", "save (ctrl+s typing)"}, {"q", "quit the form"},
	}}

	const keyW, whatW = 9, 23
	avail := m.height - 1
	gaps := true
	render := func(ss ...section) string {
		var lines []string
		for i, s := range ss {
			if i > 0 && gaps {
				lines = append(lines, "")
			}
			lines = append(lines, lipgloss.NewStyle().Foreground(s.accent).Bold(true).Render(s.title))
			for _, e := range s.rows {
				lines = append(lines, helpKeyStyle.Render(fmt.Sprintf("%-*s", keyW, e.keys))+" "+e.what)
			}
		}
		return lipgloss.NewStyle().Width(keyW + 1 + whatW).Render(strings.Join(lines, "\n"))
	}

	var body string
	if m.width >= 2*(keyW+1+whatW)+4+boxChromeX {
		body = lipgloss.JoinHorizontal(lipgloss.Top,
			render(jobs, runs, anywhere), "    ", render(logs, form, mouse))
	} else {
		// One column is tall: lose the blank lines between sections before
		// losing whole sections off the bottom.
		body = render(jobs, runs, logs, form, anywhere, mouse)
		if lipgloss.Height(body) > avail-boxChromeY {
			gaps = false
			body = render(jobs, runs, logs, form, anywhere, mouse)
		}
	}
	body = lipgloss.NewStyle().MaxWidth(m.width - boxChromeX).MaxHeight(avail - boxChromeY).Render(body)
	box := renderPane("Keys", "", accentJobs, true, body)
	footer := renderHints(m.width, "", []helpHint{
		{key: "any key", long: "close", short: "close", drop: 1},
		{key: "q", long: "quit", short: "quit", drop: 0, global: true},
	})
	return lipgloss.Place(m.width, avail, lipgloss.Center, lipgloss.Center, box) + "\n" + footer
}

// renderLog is the plain-text rendering of a run's captured output: agent
// transcripts get the parsed, readable form (which parser depends on which
// agent CLI produced the log — PRD §14); cli logs are shown as-is (PRD §9).
//
// The dashboard no longer goes through here — it keeps the parsed blocks so it
// can fold and move a cursor through them (setLogContent) — but this is still
// the whole-log view, unstyled and fully expanded, for anything that just wants
// the text.
func renderLog(raw, jobKind, provider string) string {
	if jobKind != "agent" {
		return raw
	}
	return renderPlain(parseTranscript(raw, execengine.EffectiveProvider(provider)))
}

// truncateToWidth cuts a styled string to a visible column count, keeping
// the ANSI escapes intact (lipgloss.Width is ANSI-aware; a plain slice is
// not). Only used as the last resort for a terminal too narrow even for a
// single key hint.
func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}

// wrapForViewport word-wraps text to width before handing it to
// viewport.SetContent. bubbles/viewport only ever splits on literal '\n'
// and, for any line wider than its own width, horizontally *crops* the
// rest with ansi.Cut rather than wrapping it — found by actually reading a
// real, longer agent transcript in the TUI: a single long paragraph (no
// internal newlines) was silently missing its back half, with no scroll
// position that could recover it, since the loss happened within one
// logical line rather than across lines.
// cellbuf.Wrap is exactly the call lipgloss.Style.Width makes internally
// (style.go: `str = cellbuf.Wrap(str, wrapAt, "")`), so the wrapping is
// unchanged — what's dropped is everything Render does afterwards, above all
// padding every line out to the full width. That padding was both redundant
// and the most expensive thing in the refresh: viewport.View already pads
// what it shows to its own width, so the wrap was padding thousands of lines
// to produce the few dozen visible ones. On a 256 KB transcript at 168
// columns it was allocating 4.3 MB per call.
func wrapForViewport(content string, width int) string {
	if width <= 0 {
		return content
	}
	return cellbuf.Wrap(content, width, "")
}

// jobRowsFor and runRowsFor build each table's rows against a given column
// set. They exist as methods so setTableWidth can rebuild rows at the exact
// moment it swaps columns, without knowing where the data lives.
func (m *model) jobRowsFor(cols []table.Column) []table.Row { return jobRows(m.jobs, cols) }
func (m *model) runRowsFor(cols []table.Column) []table.Row { return m.groupedRunRows(cols) }
