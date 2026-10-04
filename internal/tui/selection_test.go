package tui

import (
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestSelectionTextAndHighlight(t *testing.T) {
	for _, tc := range []struct {
		name, frame, want string
		start, end        screenPoint
	}{
		{"forward", "hello world", "hello", screenPoint{0, 0}, screenPoint{4, 0}},
		{"reverse", "hello world", "hello", screenPoint{4, 0}, screenPoint{0, 0}},
		{"multiline", "abc   \ndef   \nghi", "bc\ndef\ngh", screenPoint{1, 0}, screenPoint{1, 2}},
		{"reverse multiline", "abc   \ndef   \nghi", "bc\ndef\ngh", screenPoint{1, 2}, screenPoint{1, 0}},
		{"wide and combining", "a界e\u0301🙂z", "界e\u0301🙂", screenPoint{1, 0}, screenPoint{5, 0}},
		{"ANSI", "\x1b[31mhello\x1b[0m world", "hello", screenPoint{0, 0}, screenPoint{4, 0}},
		{"blank lines", "a\n\nb", "a\n\nb", screenPoint{0, 0}, screenPoint{0, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := textSelection{active: true, frame: tc.frame, start: tc.start, end: tc.end}
			if got := s.text(); got != tc.want {
				t.Fatalf("copied %q, want %q", got, tc.want)
			}
			if got := ansi.Strip(s.render()); got != ansi.Strip(tc.frame) {
				t.Fatalf("highlight changed displayed text: %q", got)
			}
			if !strings.Contains(s.render(), "\x1b[30;47m") {
				t.Fatal("selection not highlighted")
			}
		})
	}
}

func TestDashboardDragCopiesWithoutNavigating(t *testing.T) {
	previous := clipboardWrite
	t.Cleanup(func() { clipboardWrite = previous })
	var copied string
	clipboardWrite = func(text string) error { copied = text; return nil }
	for _, size := range [][2]int{{172, 40}, {80, 30}, {46, 20}} {
		st, logsDir := seedStore(t)
		m := modelAt(t, st, logsDir, size[0], size[1])
		for _, pane := range []focusPane{focusJobs, focusRuns, focusLog} {
			m.setFocus(pane)
			m.layout()
			frame := ansi.Strip(m.View())
			target := "greet"
			if pane == focusLog {
				target = "hello-from-seeded-log"
			}
			y, x := -1, -1
			for lineNo, line := range strings.Split(frame, "\n") {
				// Data rows only, not titles; search the pane in question.
				if lineNo == 0 || (pane == focusLog && lineNo <= m.topBoxHeight && m.mode != layoutFocused) {
					continue
				}
				if byteX := strings.Index(line, target); byteX >= 0 {
					col := ansi.StringWidth(line[:byteX])
					if got, _ := m.paneAt(col, lineNo); got == pane {
						x, y = col, lineNo
						break
					}
				}
			}
			// Runs rows have no job name, so select the status text instead.
			if pane == focusRuns {
				target = "ok"
				y = 2
				x = 6 // border, pad, cursor gutter, pad, "✓ "
				if m.mode == layoutWide {
					x += m.jobsBoxWidth
				}
				if m.mode == layoutStacked {
					y += m.jobsBoxHeight
				}
			}
			if x < 0 || y < 0 {
				t.Fatalf("target %q not found in %dx%d pane %v", target, size[0], size[1], pane)
			}
			jobID, runID := m.selectedJobID(), m.selectedRunRow().ID
			m = send(t, m, tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
			m = send(t, m, tea.MouseMsg{X: x + len(target) - 1, Y: y, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
			if !strings.Contains(m.View(), "\x1b[30;47m") {
				t.Fatal("drag must highlight")
			}
			m = send(t, m, tea.MouseMsg{X: x + len(target) - 1, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
			if copied != target {
				t.Fatalf("copied %q, want %q", copied, target)
			}
			if m.selectedJobID() != jobID || m.selectedRunRow().ID != runID || m.focus != pane || m.selection.active {
				t.Fatal("drag should copy without changing focus or selection")
			}
			if m.statusMsg != "selection copied" {
				t.Fatal("copy confirmation missing")
			}
		}
	}
}

func TestSelectionFreezesFrameAndCancelsOnResize(t *testing.T) {
	st, logsDir := seedStore(t)
	m := modelAt(t, st, logsDir, 172, 40)
	m = send(t, m, tea.MouseMsg{X: 3, Y: 2, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	frame := m.View()
	m.statusMsg = "changed underneath"
	if m.View() != frame {
		t.Fatal("refresh should not move text during selection")
	}
	m = send(t, m, tea.WindowSizeMsg{Width: 80, Height: 30})
	if m.selection.active {
		t.Fatal("resize must cancel stale selection coordinates")
	}
	m = send(t, m, copiedMsg{err: errors.New("clipboard unavailable")})
	if !strings.Contains(m.statusMsg, "copy failed") {
		t.Fatal("copy failure should be reported")
	}
}

func TestClipboardUsesNativeCommand(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "copied")
	name := "pbcopy"
	if runtime.GOOS != "darwin" {
		name = "wl-copy"
		t.Setenv("WAYLAND_DISPLAY", "test")
	}
	t.Setenv("PATH", dir)
	t.Setenv("JOBTAIL_TEST_CLIPBOARD", capture)
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n/bin/cat >\"$JOBTAIL_TEST_CLIPBOARD\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	text := "selected log\n界🙂\n"
	if err := writeClipboard(text); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(capture)
	if err != nil || string(got) != text {
		t.Fatalf("native clipboard got %q, err %v", got, err)
	}
}

func TestClipboardFallsBackToOSC52(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no native clipboard utility
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	previous := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = previous; writer.Close() })
	text := "selected text"
	if err := writeClipboard(text); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	os.Stdout = previous
	got, err := io.ReadAll(reader)
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
	if err != nil || string(got) != want {
		t.Fatalf("OSC 52 got %q, want %q, err %v", got, want, err)
	}
}
