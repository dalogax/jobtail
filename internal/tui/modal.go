package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/dalogax/jobtail/internal/store"
)

// overlay draws box centered over a dashboard frame, with the frame behind it
// faded to one quiet color: the dashboard stays visible, so it is clear what
// the modal is about, but nothing on it competes with the modal for the eye.
//
// lipgloss v1 has no layers, so this is done per line on rendered strings.
// Fading first is what makes the splice safe: a faded line is plain text in
// one style, and cutting it at a column cannot land inside some other
// escape sequence or leave a color open into the modal.
func overlay(bg, box string, width, height int) string {
	lines := strings.Split(bg, "\n")
	for len(lines) < height {
		lines = append(lines, "")
	}
	for i, l := range lines {
		plain := ansi.Strip(l)
		if w := ansi.StringWidth(plain); w < width {
			plain += strings.Repeat(" ", width-w)
		}
		lines[i] = plain
	}

	boxLines := strings.Split(box, "\n")
	bw := lipgloss.Width(box)
	x := max((width-bw)/2, 0)
	y := max((height-len(boxLines))/2, 0)

	fade := lipgloss.NewStyle().Foreground(fgFaint)
	for i, l := range lines {
		j := i - y
		if j < 0 || j >= len(boxLines) {
			lines[i] = fade.Render(l)
			continue
		}
		left := ansi.Truncate(l, x, "")
		right := ansi.TruncateLeft(l, x+bw, "")
		lines[i] = fade.Render(left) + boxLines[j] + fade.Render(right)
	}
	return strings.Join(lines[:height], "\n")
}

// cleanPrompt is the question c on the runs pane asks before deleting a job's
// run history. It names the job and the count, and says what is spared: a run
// still going is never deleted from under its runner.
type cleanPrompt struct {
	jobID             string
	finished, running int
}

func (m model) askClean() model {
	jobID := m.selectedJobID()
	if jobID == "" || m.runsJobID != jobID {
		return m
	}
	p := cleanPrompt{jobID: jobID}
	for _, r := range m.runs {
		if r.Status == "running" {
			p.running++
		} else {
			p.finished++
		}
	}
	if p.finished == 0 {
		m.statusMsg = "no finished runs to clean"
		return m
	}
	m.cleanAsk = &p
	return m
}

type cleanedMsg struct {
	jobID string
	n     int
	err   error
}

// cleanRuns deletes the rows first and the log files after, as `jobtail rm`
// does: a log left behind is litter, a row pointing at a deleted log is a run
// that looks broken.
func cleanRunsCmd(ctx context.Context, st *store.Store, jobID string) tea.Cmd {
	return func() tea.Msg {
		paths, err := st.DeleteFinishedRuns(ctx, jobID)
		if err != nil {
			return cleanedMsg{jobID: jobID, err: err}
		}
		for _, p := range paths {
			_ = os.Remove(p) // best effort: an already-missing log is fine
		}
		return cleanedMsg{jobID: jobID, n: len(paths)}
	}
}

func (p cleanPrompt) view(screenW int) string {
	w := min(56, screenW-4)
	if screenW < 40 {
		w = screenW
	}
	cw := w - boxChromeX
	runs := "runs"
	if p.finished == 1 {
		runs = "run"
	}
	lines := []string{
		"",
		fmt.Sprintf("Deletes %d finished %s and their logs.", p.finished, runs),
	}
	if p.running > 0 {
		lines = append(lines, helpStyle.Render(fmt.Sprintf("%d running kept.", p.running)))
	}
	lines = append(lines, helpStyle.Render("This can't be undone."), "",
		renderHints(cw, "", []helpHint{
			{key: "y", long: "delete", short: "delete", drop: 1},
			{key: "n", long: "cancel", short: "cancel", drop: 0, global: true},
		}))
	for i, l := range lines {
		lines[i] = lipgloss.NewStyle().Width(cw).MaxWidth(cw).Render(truncateToWidth(l, cw))
	}
	return renderPane("Clean runs of "+p.jobID+"?", "", statusError, true, strings.Join(lines, "\n"))
}

// editSelectedJob opens the form on the selected job, read fresh from the store
// rather than from the list: the list is up to a second old, and editing a
// stale copy would quietly undo whatever changed in between.
func (m model) editSelectedJob() model {
	j := m.selectedJob()
	if j == nil {
		return m
	}
	fresh, err := m.st.GetJob(m.ctx, j.ID)
	if err != nil {
		m.statusMsg = fmt.Sprintf("edit failed: %v", err)
		return m
	}
	m.form = newJobForm(&fresh, m.cwd)
	m.form.resize(m.width, m.height)
	return m
}

func (m model) newJob() model {
	m.form = newJobForm(nil, m.cwd)
	m.form.resize(m.width, m.height)
	return m
}

// saveForm writes the form back through the same store calls `jobtail add`
// and `jobtail edit` use. A failed save keeps the form open with everything
// typed into it.
func (m model) saveForm() model {
	f := m.form
	j, ok := f.validate()
	if !ok {
		return m
	}
	f.err = ""
	if f.editID == "" {
		if err := m.st.CreateJob(m.ctx, j); err != nil {
			if err == store.ErrAlreadyExists {
				f.field(fID).err = "a job with this id already exists"
				f.cursor = 0
				return m
			}
			f.err = err.Error()
			return m
		}
		m.statusMsg = "created job " + j.ID
	} else {
		p := store.JobPatch{
			Cron: &j.Cron, Timezone: &j.Timezone, Cwd: &j.Cwd,
			MaxConcurrent: &j.MaxConcurrent, TimeoutSeconds: &j.TimeoutSeconds, Keep: &j.Keep,
			Precheck: &j.Precheck, PrecheckTimeoutSeconds: &j.PrecheckTimeoutSeconds, Notify: &j.Notify,
		}
		if j.Kind == "cli" {
			p.Command = &j.Command
		} else {
			p.Prompt, p.Provider, p.Model, p.PermissionMode = &j.Prompt, &j.Provider, &j.Model, &j.PermissionMode
		}
		if err := m.st.EditJob(m.ctx, f.editID, p); err != nil {
			f.err = err.Error()
			return m
		}
		if j.Enabled != f.enabledWas {
			if err := m.st.SetEnabled(m.ctx, f.editID, j.Enabled); err != nil {
				f.err = err.Error()
				return m
			}
		}
		m.statusMsg = "saved job " + j.ID
	}
	m.form = nil
	m.selectAfterLoad = j.ID
	return m
}
