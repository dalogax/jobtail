package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/dalogax/jobtail/internal/store"
)

// Entries index the original history; folding never removes a run or its log.
// The oldest run identifies a streak so its expansion survives new skips.
type runEntry struct {
	first, end int
	group      bool
	key        string
}

func groupRuns(runs []store.Run, expanded map[string]bool) []runEntry {
	entries := make([]runEntry, 0, len(runs))
	for i := 0; i < len(runs); {
		end := i + 1
		if runs[i].Status == "skipped" {
			for end < len(runs) && runs[end].Status == "skipped" {
				end++
			}
		}
		e := runEntry{first: i, end: end, group: end-i > 1, key: runs[end-1].ID}
		entries = append(entries, e)
		if e.group && expanded[e.key] {
			for j := i; j < end; j++ {
				entries = append(entries, runEntry{first: j, end: j + 1})
			}
		}
		i = end
	}
	return entries
}

func (m model) runRowCount() int {
	if m.runEntries == nil {
		return len(m.runs) // models seeded directly by tests
	}
	return len(m.runEntries)
}

func (m model) runEntryAt(row int) runEntry {
	if row < 0 || row >= m.runRowCount() {
		return runEntry{first: -1}
	}
	if m.runEntries == nil {
		return runEntry{first: row, end: row + 1}
	}
	return m.runEntries[row]
}

func (m model) selectedRunEntry() runEntry { return m.runEntryAt(m.runsTable.Cursor()) }

func (m *model) rebuildRunEntries() {
	m.runEntries = groupRuns(m.runs, m.runExpanded)
	m.runsTable.SetRows(m.groupedRunRows(m.runsCols))
}

func (m model) groupedRunRows(cols []table.Column) []table.Row {
	rows := make([]table.Row, 0, m.runRowCount())
	for i := range m.runRowCount() {
		e := m.runEntryAt(i)
		row := runRows(m.runs[e.first:e.first+1], cols)[0]
		if e.group {
			for j, c := range cols {
				switch c.Title {
				case "Status":
					mark := "+"
					if m.runExpanded[e.key] {
						mark = "-"
					}
					label := fmt.Sprintf("%s skipped ×%d", mark, e.end-e.first)
					if lipgloss.Width(label) > c.Width {
						label = fmt.Sprintf("%s ×%d", mark, e.end-e.first)
					}
					row[j] = statusNever.Render(label)
				case "Dur":
					row[j] = humanDuration(m.runs[e.first].StartedAt.Sub(m.runs[e.end-1].StartedAt))
				case "Exit":
					row[j] = ""
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func (m model) toggleRunGroup() (tea.Model, tea.Cmd) {
	e := m.selectedRunEntry()
	if !e.group {
		return m, nil
	}
	if m.runExpanded == nil {
		m.runExpanded = map[string]bool{}
	}
	m.runExpanded[e.key] = !m.runExpanded[e.key]
	m.rebuildRunEntries()
	m.layout()
	return m, nil
}
