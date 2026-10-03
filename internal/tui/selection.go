package tui

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type screenPoint struct{ x, y int }

// Capture the frame before focusing a pane or moving a row. Refreshes may
// continue underneath, but cannot move the text out from under a drag.
type textSelection struct {
	active     bool
	dragged    bool
	start, end screenPoint
	frame      string
}

func (s textSelection) bounds(y, width int) (int, int) {
	a, b := s.start, s.end
	if a.y > b.y || (a.y == b.y && a.x > b.x) {
		a, b = b, a
	}
	if y < a.y || y > b.y {
		return 0, 0
	}
	left, right := 0, width
	if y == a.y {
		left = a.x
	}
	if y == b.y {
		right = b.x + 1
	}
	return min(left, width), min(right, width)
}

func (s textSelection) text() string {
	var selected []string
	for y, line := range strings.Split(ansi.Strip(s.frame), "\n") {
		left, right := s.bounds(y, ansi.StringWidth(line))
		if right > left {
			selected = append(selected, strings.TrimRight(ansi.Cut(line, left, right), " "))
		} else if y > min(s.start.y, s.end.y) && y < max(s.start.y, s.end.y) {
			selected = append(selected, "")
		}
	}
	return strings.Join(selected, "\n")
}

func (s textSelection) render() string {
	lines := strings.Split(s.frame, "\n")
	for y, line := range lines {
		left, right := s.bounds(y, ansi.StringWidth(line))
		if right <= left || s.start == s.end {
			continue
		}
		// Use explicit escape codes so selection remains visible even when
		// lipgloss's output profile was initialized without a terminal.
		middle := ansi.Strip(ansi.Cut(line, left, right))
		lines[y] = ansi.Cut(line, 0, left) + "\x1b[30;47m" + middle + "\x1b[0m" +
			ansi.Cut(line, right, lipgloss.Width(line))
	}
	return strings.Join(lines, "\n")
}

func (m model) selectWithMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	p := screenPoint{max(0, min(msg.X, max(0, m.width-1))), max(0, min(msg.Y, max(0, m.height-1)))}
	if msg.Action == tea.MouseActionRelease && m.selection.active {
		m.selection.end = p
		s := m.selection
		m.selection = textSelection{}
		if s.dragged || s.start != s.end {
			if s.start == s.end {
				return m, nil
			}
			if text := s.text(); strings.TrimSpace(text) != "" {
				return m, copyTextCmd(text)
			}
			return m, nil
		}
		return m.clickAt(tea.MouseMsg{X: p.x, Y: p.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	}
	if msg.Button == tea.MouseButtonLeft {
		switch msg.Action {
		case tea.MouseActionPress:
			m.selection = textSelection{active: true, start: p, end: p, frame: m.dashboardView()}
			return m, nil
		case tea.MouseActionMotion:
			if m.selection.active {
				m.selection.end = p
				m.selection.dragged = m.selection.dragged || p != m.selection.start
			}
			return m, nil
		}
	}
	if m.selection.active {
		return m, nil
	}
	return m.clickAt(msg)
}

type copiedMsg struct{ err error }

var clipboardWrite = writeClipboard

func copyTextCmd(text string) tea.Cmd {
	return func() tea.Msg { return copiedMsg{err: clipboardWrite(text)} }
}

// Prefer the local system clipboard. OSC 52 also works over SSH and in
// terminals without a clipboard utility (subject to terminal permission).
func writeClipboard(text string) error {
	var candidates [][]string
	if runtime.GOOS == "darwin" {
		candidates = [][]string{{"pbcopy"}}
	} else {
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			candidates = append(candidates, []string{"wl-copy"})
		}
		if os.Getenv("DISPLAY") != "" {
			candidates = append(candidates, []string{"xclip", "-selection", "clipboard"}, []string{"xsel", "--clipboard", "--input"})
		}
	}
	for _, args := range candidates {
		path, err := exec.LookPath(args[0])
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, path, args[1:]...)
		cmd.Stdin = strings.NewReader(text)
		err = cmd.Run()
		cancel()
		if err == nil {
			return nil
		}
	}
	_, err := fmt.Fprint(os.Stdout, "\x1b]52;c;"+base64.StdEncoding.EncodeToString([]byte(text))+"\a")
	return err
}
