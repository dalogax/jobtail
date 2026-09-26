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
}

// Run opens the dashboard. It blocks until the user quits.
func Run(st *store.Store, logsDir string) error {
	m := newModel(st, logsDir)
	p := tea.NewProgram(m, tea.WithAltScreen())
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
	}
	return m, nil
}

func (m *model) layout() {
	if m.width == 0 {
		return
	}
	third := m.width / 3
	paneWidth := third - 4
	if paneWidth < 10 {
		paneWidth = 10
	}

	jobCols := jobsColumns(paneWidth)
	m.jobsTable.SetColumns(jobCols)
	m.jobsTable.SetWidth(columnsWidth(jobCols))

	runCols := runsColumns(paneWidth)
	m.runsTable.SetColumns(runCols)
	m.runsTable.SetWidth(columnsWidth(runCols))

	m.logVP.Width = m.width - 2*third - 4
	h := m.height - 6
	if h < 3 {
		h = 3
	}
	m.jobsTable.SetHeight(h)
	m.runsTable.SetHeight(h)
	m.logVP.Height = h
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
func jobsColumns(width int) []table.Column {
	const kindW, runsW, statusW = 5, 4, 9
	remaining := width - kindW - runsW - statusW
	if remaining < 20 {
		remaining = 20
	}
	idW := remaining * 6 / 10
	nextW := remaining - idW
	return []table.Column{
		{Title: "ID", Width: idW},
		{Title: "Kind", Width: kindW},
		{Title: "Runs", Width: runsW},
		{Title: "Next", Width: nextW},
		{Title: "Status", Width: statusW},
	}
}

// runsColumns splits an available pane width across the runs table's
// columns: fixed budgets for Status/Trigger/Dur, the rest to Started.
func runsColumns(width int) []table.Column {
	const statusW, triggerW, durW = 8, 9, 6
	startedW := width - statusW - triggerW - durW
	if startedW < 12 {
		startedW = 12
	}
	return []table.Column{
		{Title: "Status", Width: statusW},
		{Title: "Trigger", Width: triggerW},
		{Title: "Started", Width: startedW},
		{Title: "Dur", Width: durW},
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

	row := lipgloss.JoinHorizontal(lipgloss.Top, jobsBox, runsBox, logBox)
	help := helpStyle.Render("h/l or arrows/enter/esc: move · e: enable/disable · r: run now · q: quit  " + m.statusMsg)
	return row + "\n" + help
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
