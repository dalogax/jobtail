package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// grid is the jobs and runs tables. It keeps bubbles/table's column and row
// types and its key bindings, but draws the rows itself, for two reasons the
// widget could not be configured around:
//
//   - bubbles/table v1.0.0 truncates every cell with go-runewidth, which
//     counts the bytes of an SGR escape as visible columns. A colored
//     "failed" measured 15 wide, so in any status column narrower than that
//     — every terminal up to about 90 columns — the color had to be dropped
//     to keep the text, while the shorter "ok" kept its green. The status
//     that most needed color was the one that lost it. grid measures and
//     cuts with x/ansi, so a cell's styling costs nothing.
//
//   - Its Selected style is one foreground color applied to the cursor row
//     whatever the focus, so the jobs pane and the runs pane each showed an
//     identical pink row — two cursors and no way to tell which one the keys
//     would move — and under NO_COLOR, where lipgloss emits no SGR at all,
//     neither showed anything. grid marks the cursor row with a glyph in its
//     gutter, which survives without color, and a background band that is
//     stronger in the focused pane than in the others.
type grid struct {
	cols    []table.Column
	rows    []table.Row
	cursor  int
	offset  int // first row shown
	height  int // data rows shown (the header is extra)
	width   int
	focused bool
	accent  lipgloss.Color
	keys    table.KeyMap
}

// gridGutter is the column at the left edge of every row that carries the
// cursor marker. It is part of the table's width (see setTableWidth).
const gridGutter = 1

const cursorGlyph = "❯"

func newGrid(cols []table.Column, accent lipgloss.Color, focused bool) grid {
	return grid{
		cols:    cols,
		accent:  accent,
		focused: focused,
		height:  1,
		keys:    table.DefaultKeyMap(),
	}
}

func (g grid) Columns() []table.Column { return g.cols }
func (g grid) Rows() []table.Row       { return g.rows }
func (g grid) Cursor() int             { return g.cursor }
func (g grid) Offset() int             { return g.offset }
func (g grid) Width() int              { return g.width }
func (g *grid) Focus()                 { g.focused = true }
func (g *grid) Blur()                  { g.focused = false }

func (g *grid) SetColumns(c []table.Column) { g.cols = c }
func (g *grid) SetWidth(w int)              { g.width = w }

// SetHeight takes the table's whole height, header included, the way
// bubbles/table's does.
func (g *grid) SetHeight(h int) {
	g.height = max(h-1, 1)
	g.clamp()
}

func (g *grid) SetRows(r []table.Row) {
	g.rows = r
	g.clamp()
}

func (g *grid) SetCursor(n int) {
	g.cursor = n
	g.clamp()
}

func (g *grid) MoveUp(n int)   { g.SetCursor(g.cursor - n) }
func (g *grid) MoveDown(n int) { g.SetCursor(g.cursor + n) }

// clamp keeps the cursor on a row and the window around the cursor,
// scrolling as little as possible.
func (g *grid) clamp() {
	g.cursor = min(max(g.cursor, 0), max(len(g.rows)-1, 0))
	if g.cursor < g.offset {
		g.offset = g.cursor
	}
	if g.cursor >= g.offset+g.height {
		g.offset = g.cursor - g.height + 1
	}
	g.offset = min(max(g.offset, 0), max(len(g.rows)-g.height, 0))
}

// Update handles the same keys bubbles/table does, and only while focused.
func (g grid) Update(msg tea.Msg) (grid, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok || !g.focused {
		return g, nil
	}
	km := g.keys
	switch {
	case keyIn(k, km.LineUp):
		g.MoveUp(1)
	case keyIn(k, km.LineDown):
		g.MoveDown(1)
	case keyIn(k, km.PageUp):
		g.MoveUp(g.height)
	case keyIn(k, km.PageDown):
		g.MoveDown(g.height)
	case keyIn(k, km.HalfPageUp):
		g.MoveUp(max(g.height/2, 1))
	case keyIn(k, km.HalfPageDown):
		g.MoveDown(max(g.height/2, 1))
	case keyIn(k, km.GotoTop):
		g.SetCursor(0)
	case keyIn(k, km.GotoBottom):
		g.SetCursor(len(g.rows) - 1)
	}
	return g, nil
}

func keyIn(k tea.KeyMsg, b interface{ Keys() []string }) bool {
	for _, s := range b.Keys() {
		if k.String() == s {
			return true
		}
	}
	return false
}

// View draws the header and the visible rows, padded to the full height so
// the box around it keeps its size.
func (g grid) View() string {
	lines := make([]string, 0, g.height+1)
	titles := make([]string, len(g.cols))
	for i, c := range g.cols {
		titles[i] = headerStyle.Render(c.Title)
	}
	lines = append(lines, g.line(titles, " ", lipgloss.Style{}))

	end := min(g.offset+g.height, len(g.rows))
	for r := g.offset; r < end; r++ {
		if r != g.cursor {
			lines = append(lines, g.line(g.rows[r], " ", lipgloss.Style{}))
			continue
		}
		bg, mark := selectionInactiveBG, lipgloss.NewStyle().Foreground(fgMuted)
		if g.focused {
			bg, mark = selectionBG, lipgloss.NewStyle().Foreground(g.accent).Bold(true)
		}
		lines = append(lines, g.line(g.rows[r], mark.Background(bg).Render(cursorGlyph),
			lipgloss.NewStyle().Background(bg)))
	}
	blank := strings.Repeat(" ", g.width)
	for len(lines) < g.height+1 {
		lines = append(lines, blank)
	}
	return strings.Join(lines, "\n")
}

// line lays out one row: the gutter, then each cell cut to its column with
// an ellipsis and padded by one space either side. band, when set, is a
// background for the whole row, re-applied after every reset inside a cell
// so a colored status doesn't punch a hole in it.
func (g grid) line(cells []string, gutter string, band lipgloss.Style) string {
	var b strings.Builder
	b.WriteString(gutter)
	for i, c := range g.cols {
		if c.Width <= 0 {
			continue
		}
		v := ""
		if i < len(cells) {
			v = cells[i]
		}
		v = ansi.Truncate(v, c.Width, "…")
		b.WriteString(" ")
		b.WriteString(v)
		b.WriteString(strings.Repeat(" ", c.Width-ansi.StringWidth(v)+1))
	}
	out := b.String()
	if pad := g.width - ansi.StringWidth(out); pad > 0 {
		out += strings.Repeat(" ", pad)
	}
	if band.GetBackground() == (lipgloss.NoColor{}) {
		return out
	}
	open, _, ok := strings.Cut(band.Render("\x00"), "\x00")
	if !ok || open == "" {
		return out // no color profile: the gutter glyph alone marks the row
	}
	return open + strings.ReplaceAll(out, "\x1b[0m", "\x1b[0m"+open) + "\x1b[0m"
}
