// Package tui is jobtail's Bubble Tea dashboard: jobs -> runs -> log,
// three panes, meant to run inside a Herdr tab (PRD §9). Bare command, no
// Herdr plugin manifest required (PRD §8) — `jobtail tui` is a plain
// terminal program.
package tui

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/dalogax/jobtail/internal/cronx"
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

var (
	borderStyle        = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).Padding(0, 1)
	focusedBorderStyle = borderStyle.BorderForeground(lipgloss.Color("62"))
	titleStyle         = lipgloss.NewStyle().Bold(true)
	helpStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	statusOK           = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	statusFailed       = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	statusRunning      = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	statusNever        = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

type model struct {
	ctx     context.Context
	st      *store.Store
	logsDir string

	focus focusPane
	jobs  []store.JobSummary
	runs  []store.Run

	jobsTable table.Model
	runsTable table.Model
	logVP     viewport.Model

	width, height int
	err           error
	statusMsg     string
	running       map[string]bool // job IDs with an in-flight "run now" from the TUI

	// jobsBoxWidth/runsBoxWidth/topBoxHeight are the true rendered
	// dimensions set by layout() — jobs and runs side by side on top
	// (split by jobsBoxWidth), log spanning the full width below (split
	// by topBoxHeight). Mouse hit-testing must read these, not re-derive
	// its own geometry: jobs and runs have different column counts, so
	// their real widths differ from each other and from a naive even
	// split (confirmed by measuring an actual rendered frame).
	jobsBoxWidth, runsBoxWidth, topBoxHeight int
}

// Run opens the dashboard. It blocks until the user quits.
func Run(st *store.Store, logsDir string) error {
	m := newModel(st, logsDir)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// defaultPaneWidth is used only until the first real tea.WindowSizeMsg
// arrives and layout() recomputes columns to fit the actual terminal.
const defaultPaneWidth = 40

func newModel(st *store.Store, logsDir string) model {
	jt := table.New(table.WithColumns(jobsColumns(defaultPaneWidth)), table.WithFocused(true))
	rt := table.New(table.WithColumns(runsColumns(defaultPaneWidth)), table.WithFocused(false))
	vp := viewport.New(20, 10)

	return model{
		ctx:       context.Background(),
		st:        st,
		logsDir:   logsDir,
		focus:     focusJobs,
		jobsTable: jt,
		runsTable: rt,
		logVP:     vp,
		running:   map[string]bool{},
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(reloadJobsCmd(m.ctx, m.st), tick())
}

type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type jobsLoadedMsg struct {
	jobs []store.JobSummary
	err  error
}

func reloadJobsCmd(ctx context.Context, st *store.Store) tea.Cmd {
	return func() tea.Msg {
		jobs, err := st.ListJobs(ctx)
		return jobsLoadedMsg{jobs: jobs, err: err}
	}
}

type runsLoadedMsg struct {
	jobID string
	runs  []store.Run
	err   error
}

func reloadRunsCmd(ctx context.Context, st *store.Store, jobID string) tea.Cmd {
	return func() tea.Msg {
		runs, err := st.ListRuns(ctx, jobID, 200)
		return runsLoadedMsg{jobID: jobID, runs: runs, err: err}
	}
}

type logLoadedMsg struct {
	runID string
	text  string
	err   error
}

func reloadLogCmd(runID, path string) tea.Cmd {
	return func() tea.Msg {
		data, err := os.ReadFile(path)
		return logLoadedMsg{runID: runID, text: string(data), err: err}
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
		run, err := st.StartRun(ctx, j.ID, runID, "manual", logPath, time.Now())
		if err != nil && err != store.ErrOverlap {
			return runFinishedMsg{jobID: j.ID}
		}
		if run.Status == "skipped_overlap" {
			return runFinishedMsg{jobID: j.ID}
		}
		res, runErr := runner.Execute(ctx, st, j, runID, logPath)
		_ = runner.Finish(ctx, st, j, runID, res, runErr)
		return runFinishedMsg{jobID: j.ID}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tickMsg:
		cmds := []tea.Cmd{tick(), reloadJobsCmd(m.ctx, m.st)}
		if jobID := m.selectedJobID(); jobID != "" {
			cmds = append(cmds, reloadRunsCmd(m.ctx, m.st, jobID))
		}
		if runID, path := m.selectedRun(); runID != "" && m.isSelectedRunRunning() {
			cmds = append(cmds, reloadLogCmd(runID, path))
		}
		return m, tea.Batch(cmds...)

	case jobsLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		prevID := m.selectedJobID()
		m.jobs = msg.jobs
		m.jobsTable.SetRows(jobRows(msg.jobs))
		if prevID != "" {
			m.selectJobByID(prevID)
		} else if len(msg.jobs) > 0 {
			m.jobsTable.SetCursor(0)
		}
		if jobID := m.selectedJobID(); jobID != "" && jobID != prevID {
			return m, reloadRunsCmd(m.ctx, m.st, jobID)
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
		m.runs = msg.runs
		m.runsTable.SetRows(runRows(msg.runs))
		if prevRunID != "" {
			m.selectRunByID(prevRunID)
		} else if len(msg.runs) > 0 {
			m.runsTable.SetCursor(0)
		}
		if runID, path := m.selectedRun(); runID != "" {
			return m, reloadLogCmd(runID, path)
		}
		return m, nil

	case logLoadedMsg:
		if id, _ := m.selectedRun(); id != msg.runID {
			return m, nil
		}
		if msg.err != nil {
			m.logVP.SetContent("(no log yet)")
			return m, nil
		}
		atBottom := m.logVP.AtBottom()
		m.logVP.SetContent(renderLog(msg.text, m.selectedJobKind()))
		if atBottom {
			m.logVP.GotoBottom()
		}
		return m, nil

	case runFinishedMsg:
		m.statusMsg = fmt.Sprintf("run finished for %s", msg.jobID)
		delete(m.running, msg.jobID)
		return m, reloadJobsCmd(m.ctx, m.st)

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
func (m *model) layout() {
	if m.width == 0 {
		return
	}
	half := m.width / 2

	// boxChrome: each pane's Border(NormalBorder) contributes 2 columns
	// (left+right border) and its Padding(0,1) contributes 2 more
	// (left+right padding) — 4 total, on top of the table's own content
	// width, which itself is *already* wider than the raw column widths
	// by 2 chars per column (bubbles/table's own per-cell Padding(0,1)).
	// jobsColumns/runsColumns are handed a target that backs *both* of
	// those overheads out (different per table, since they have a
	// different column count), so the box that comes out the other end
	// of SetColumns -> columnsWidth -> SetWidth actually lands on `half`
	// instead of quietly ending up wider — mixing these up is exactly
	// what caused a real, measured overflow (jobs+runs summed to 190
	// columns on a 172-wide terminal) despite each step looking right on
	// its own.
	const boxChrome = 4
	const jobsCols, runsCols = 5, 4

	jobCols := jobsColumns(half - boxChrome - 2*jobsCols)
	m.jobsTable.SetColumns(jobCols)
	jobsContentWidth := columnsWidth(jobCols)
	m.jobsTable.SetWidth(jobsContentWidth)
	m.jobsBoxWidth = jobsContentWidth + boxChrome

	runCols := runsColumns(half - boxChrome - 2*runsCols)
	m.runsTable.SetColumns(runCols)
	runsContentWidth := columnsWidth(runCols)
	m.runsTable.SetWidth(runsContentWidth)
	m.runsBoxWidth = runsContentWidth + boxChrome

	m.logVP.Width = m.width - boxChrome

	// Vertical split: reserve 1 line for the help bar below everything,
	// give the top row (jobs/runs — usually a short list) ~40% of what's
	// left, and the log the rest.
	totalH := m.height - 1
	if totalH < 10 {
		totalH = 10
	}
	topBoxOuter := totalH * 2 / 5
	if topBoxOuter < 6 {
		topBoxOuter = 6
	}
	logBoxOuter := totalH - topBoxOuter
	if logBoxOuter < 5 {
		logBoxOuter = 5
	}
	m.topBoxHeight = topBoxOuter

	// Each box's own border(2)+title(1) = 3 lines of chrome above the
	// table/viewport content proper.
	const titleAndBorderChrome = 3
	tableHeight := topBoxOuter - titleAndBorderChrome
	if tableHeight < 3 {
		tableHeight = 3
	}
	m.jobsTable.SetHeight(tableHeight)
	m.runsTable.SetHeight(tableHeight)

	logViewportHeight := logBoxOuter - titleAndBorderChrome
	if logViewportHeight < 3 {
		logViewportHeight = 3
	}
	m.logVP.Height = logViewportHeight
}

// columnsWidth sums what SetColumns just laid out, so the table's viewport
// (which SetWidth controls) never clips a cell short of its own declared
// width — a mismatch there is what silently truncated column content
// mid-cell before this was fixed. +2 per column: bubbles/table's default
// Cell/Header style is Padding(0, 1) — one padding column on each side —
// which is real rendered width the raw Column.Width doesn't include; missing
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

// jobsColumns splits an available pane width across the jobs table's
// columns: fixed budgets for Kind/Runs/Status, the rest split between ID
// and Next.
// proportionalWidths splits `width` across len(shares) columns by relative
// weight, guaranteeing the columns always sum to exactly `width` (down to
// 1 char each) — never more. A fixed-minimum floor per column here (an
// earlier version had one: "remaining < 20 -> 20") is exactly what broke
// narrow terminals: the declared column widths stayed wider than the
// actual available pane once the terminal got narrow enough (a phone-sized
// aspect ratio, e.g.), so the box didn't shrink with the terminal at all,
// while the log pane (no such floor) shrank correctly — that mismatch is
// what got reported and reproduced.
func proportionalWidths(width int, shares ...int) []int {
	if width < len(shares) {
		width = len(shares) // 1 char minimum per column
	}
	total := 0
	for _, s := range shares {
		total += s
	}
	cols := make([]int, len(shares))
	sum := 0
	for i, s := range shares {
		cols[i] = width * s / total
		if cols[i] < 1 {
			cols[i] = 1
		}
		sum += cols[i]
	}
	// Give integer-division leftover (or claw back an over-allocation from
	// the 1-char-minimum clamp above) to the first, widest column.
	cols[0] += width - sum
	if cols[0] < 1 {
		cols[0] = 1
	}
	return cols
}

func jobsColumns(width int) []table.Column {
	w := proportionalWidths(width, 6, 2, 1, 3, 2) // ID, Kind, Runs, Next, Status
	return []table.Column{
		{Title: "ID", Width: w[0]},
		{Title: "Kind", Width: w[1]},
		{Title: "Runs", Width: w[2]},
		{Title: "Next", Width: w[3]},
		{Title: "Status", Width: w[4]},
	}
}

// runsColumns splits an available pane width across the runs table's
// columns proportionally (see proportionalWidths).
func runsColumns(width int) []table.Column {
	w := proportionalWidths(width, 3, 3, 5, 2) // Status, Trigger, Started, Dur
	return []table.Column{
		{Title: "Status", Width: w[0]},
		{Title: "Trigger", Width: w[1]},
		{Title: "Started", Width: w[2]},
		{Title: "Dur", Width: w[3]},
	}
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "esc":
		if m.focus > focusJobs {
			m.focus--
		}
		return m, nil
	case "enter", "l", "right":
		if m.focus < focusLog {
			m.focus++
		}
		return m, nil
	case "h", "left":
		if m.focus > focusJobs {
			m.focus--
		}
		return m, nil
	case "e":
		if m.focus == focusJobs {
			return m.toggleEnabled()
		}
	case "r":
		if m.focus == focusJobs {
			return m.runSelectedNow()
		}
	}

	var cmd tea.Cmd
	switch m.focus {
	case focusJobs:
		prevID := m.selectedJobID()
		m.jobsTable, cmd = m.jobsTable.Update(msg)
		if newID := m.selectedJobID(); newID != prevID && newID != "" {
			return m, tea.Batch(cmd, reloadRunsCmd(m.ctx, m.st, newID))
		}
	case focusRuns:
		prevID, _ := m.selectedRun()
		m.runsTable, cmd = m.runsTable.Update(msg)
		if newID, path := m.selectedRun(); newID != prevID && newID != "" {
			return m, tea.Batch(cmd, reloadLogCmd(newID, path))
		}
	case focusLog:
		m.logVP, cmd = m.logVP.Update(msg)
	}
	return m, cmd
}

// handleMouse maps a click/wheel event's terminal (X, Y) to a pane —
// jobs/runs/log occupy the left/middle/right third of the screen — and, for
// a left-click, to a row within that pane's table.
func (m model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	pane := focusLog
	if msg.Y < m.topBoxHeight {
		if msg.X < m.jobsBoxWidth {
			pane = focusJobs
		} else {
			pane = focusRuns
		}
	}

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		return m.scroll(pane, -1)
	case tea.MouseButtonWheelDown:
		return m.scroll(pane, 1)
	}

	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	m.focus = pane

	row, ok := rowAtY(msg.Y)
	if !ok {
		return m, nil
	}
	switch pane {
	case focusJobs:
		if row >= len(m.jobs) {
			return m, nil
		}
		prevID := m.selectedJobID()
		m.jobsTable.SetCursor(row)
		if newID := m.selectedJobID(); newID != prevID && newID != "" {
			return m, reloadRunsCmd(m.ctx, m.st, newID)
		}
	case focusRuns:
		if row >= len(m.runs) {
			return m, nil
		}
		prevID, _ := m.selectedRun()
		m.runsTable.SetCursor(row)
		if newID, path := m.selectedRun(); newID != prevID && newID != "" {
			return m, reloadLogCmd(newID, path)
		}
	}
	return m, nil
}

// scroll handles a wheel event over a pane: moves the table cursor (jobs,
// runs) or scrolls the log viewport, and focuses whichever pane the wheel
// was over — matching ordinary "scroll wherever the mouse is" behavior.
func (m model) scroll(pane focusPane, dir int) (tea.Model, tea.Cmd) {
	m.focus = pane
	switch pane {
	case focusJobs:
		if dir < 0 {
			m.jobsTable.MoveUp(1)
		} else {
			m.jobsTable.MoveDown(1)
		}
		if newID := m.selectedJobID(); newID != "" {
			return m, reloadRunsCmd(m.ctx, m.st, newID)
		}
	case focusRuns:
		if dir < 0 {
			m.runsTable.MoveUp(1)
		} else {
			m.runsTable.MoveDown(1)
		}
		if newID, path := m.selectedRun(); newID != "" {
			return m, reloadLogCmd(newID, path)
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

// rowAtY maps a terminal row to a table body-row index: one border-top +
// one title line + one table header = 3 rows of chrome above the first
// data row in every pane. Doesn't compensate for a table scrolled past the
// first screenful (bubbles/table exposes no scroll-offset getter) — a
// non-issue in practice at this tool's job/run-history scale.
func rowAtY(y int) (int, bool) {
	const chrome = 3
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
	return m, reloadJobsCmd(m.ctx, m.st)
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

func (m *model) selectJobByID(id string) {
	for i, j := range m.jobs {
		if j.ID == id {
			m.jobsTable.SetCursor(i)
			return
		}
	}
}

func (m model) selectedRun() (id string, logPath string) {
	idx := m.runsTable.Cursor()
	if idx < 0 || idx >= len(m.runs) {
		return "", ""
	}
	return m.runs[idx].ID, m.runs[idx].LogPath
}

func (m model) isSelectedRunRunning() bool {
	idx := m.runsTable.Cursor()
	if idx < 0 || idx >= len(m.runs) {
		return false
	}
	return m.runs[idx].Status == "running"
}

func (m *model) selectRunByID(id string) {
	for i, r := range m.runs {
		if r.ID == id {
			m.runsTable.SetCursor(i)
			return
		}
	}
}

func newRunID() string {
	return time.Now().UTC().Format("20060102T150405.000000000")
}

func jobRows(jobs []store.JobSummary) []table.Row {
	rows := make([]table.Row, 0, len(jobs))
	for _, j := range jobs {
		status := j.LastStatus
		if status == "" {
			status = "never run"
		}
		mark := "○"
		if j.Enabled {
			mark = "●"
		}
		rows = append(rows, table.Row{mark + " " + j.ID, j.Kind, fmt.Sprint(j.RunCount), nextRunLabel(j), status})
	}
	return rows
}

func nextRunLabel(j store.JobSummary) string {
	if !j.Enabled {
		return "-"
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

func runRows(runs []store.Run) []table.Row {
	rows := make([]table.Row, 0, len(runs))
	for _, r := range runs {
		dur := "-"
		if r.FinishedAt.Valid {
			dur = r.FinishedAt.Time.Sub(r.StartedAt).Round(time.Second).String()
		}
		rows = append(rows, table.Row{r.Status, r.Trigger, r.StartedAt.Local().Format("01-02 15:04"), dur})
	}
	return rows
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
		return statusNever
	}
}

func (m model) View() string {
	if m.width == 0 {
		return "loading..."
	}

	jobsBox := paneStyle(m.focus == focusJobs).Render(
		titleStyle.Render("Jobs") + "\n" + m.jobsTable.View())
	runsBox := paneStyle(m.focus == focusRuns).Render(
		titleStyle.Render("Runs: "+m.selectedJobID()) + "\n" + m.runsTable.View())
	runID, _ := m.selectedRun()
	logBox := paneStyle(m.focus == focusLog).Render(
		titleStyle.Render("Log: "+runID) + "\n" + m.logVP.View())

	top := lipgloss.JoinHorizontal(lipgloss.Top, jobsBox, runsBox)
	body := lipgloss.JoinVertical(lipgloss.Left, top, logBox)
	help := helpStyle.Render("h/l or arrows/enter/esc: move · e: enable/disable · r: run now · q: quit  " + m.statusMsg)
	return body + "\n" + help
}

func paneStyle(focused bool) lipgloss.Style {
	if focused {
		return focusedBorderStyle
	}
	return borderStyle
}

// renderLog renders a run's captured output for the log pane: agent
// transcripts (stream-json) get a lightly parsed, readable rendering;
// cli logs are shown as-is (PRD §9).
func renderLog(raw, jobKind string) string {
	if jobKind != "agent" {
		return raw
	}
	return renderTranscript(raw)
}
