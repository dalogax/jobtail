// Benchmarks for the dashboard's steady state.
//
// The dashboard is not a request/response program: it is a process a user
// leaves open in a tab for hours. Its cost is therefore not "how fast does
// it start" but "what does it burn per second while nobody is touching
// it" — BenchmarkRefreshTick measures exactly that, end to end, by feeding
// the model the same tickMsg the real tea.Tick feeds it and resolving every
// command that falls out (job query, run query, log read, transcript
// render, wrap) plus the frame render that Bubble Tea would do after each.
//
// The remaining benchmarks break that number down so a regression can be
// attributed instead of just noticed.
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dalogax/jobtail/internal/store"
)

// benchTranscript builds a claude stream-json log of roughly sizeKB
// kilobytes, shaped like a real agent run: an init event, then alternating
// assistant prose and tool calls, then a result. Agent logs are the big
// ones — a long unattended run writes hundreds of KB — and they are the
// input to the most expensive per-tick work.
func benchTranscript(tb testing.TB, sizeKB int) string {
	tb.Helper()
	var sb strings.Builder
	must := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			tb.Fatal(err)
		}
		return string(b)
	}
	sb.WriteString(`{"type":"system","subtype":"init","session_id":"bench-session-0001"}` + "\n")
	prose := strings.Repeat("The dependency audit found three outdated packages and one advisory. ", 6)
	for sb.Len() < sizeKB*1024 {
		sb.WriteString(must(map[string]any{
			"type": "assistant",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": prose},
			}},
		}) + "\n")
		sb.WriteString(must(map[string]any{
			"type": "assistant",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "tool_use", "name": "Bash",
					"input": map[string]any{"command": "go test ./...", "description": "run the suite"}},
			}},
		}) + "\n")
		sb.WriteString(must(map[string]any{
			"type": "user",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "text": "ok  \tgithub.com/dalogax/jobtail\t0.412s\nok\tmore\n"},
			}},
		}) + "\n")
	}
	sb.WriteString(`{"type":"result","subtype":"success","is_error":false,"result":"done"}` + "\n")
	return sb.String()
}

// benchModel seeds a store shaped like a well-used install — several jobs,
// the selected one at its retention ceiling of 200 runs — points the
// selected run at a real agent log on disk, and returns a model already
// laid out at the given size with everything loaded.
func benchModel(tb testing.TB, w, h, runs, logKB int) model {
	tb.Helper()
	dir := tb.TempDir()
	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		tb.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "jobtail.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })

	ctx := context.Background()
	for i := 0; i < 12; i++ {
		j := store.Job{
			ID: fmt.Sprintf("job-%02d", i), Kind: "cli", Cron: "*/5 * * * *",
			Timezone: "local", Enabled: true, Cwd: dir, Command: "echo hi",
			MaxConcurrent: 1, Keep: 200,
		}
		if i == 0 {
			j.Kind, j.Command, j.Prompt = "agent", "", "audit dependencies"
		}
		if err := st.CreateJob(ctx, j); err != nil {
			tb.Fatal(err)
		}
	}

	transcript := benchTranscript(tb, logKB)
	now := time.Now()
	for r := 0; r < runs; r++ {
		runID := fmt.Sprintf("job-00-run-%05d", r)
		logPath := filepath.Join(logsDir, runID+".log")
		if err := os.WriteFile(logPath, []byte(transcript), 0o644); err != nil {
			tb.Fatal(err)
		}
		if _, err := st.StartRun(ctx, "job-00", runID, "scheduled", logPath,
			now.Add(-time.Duration(runs-r)*time.Minute)); err != nil {
			tb.Fatal(err)
		}
		status := "ok"
		if r%7 == 0 {
			status = "failed"
		}
		if err := st.FinishRun(ctx, runID, status, 0, now, 2413); err != nil {
			tb.Fatal(err)
		}
	}

	return modelAt(tb, st, logsDir, w, h)
}

// BenchmarkRefreshTick is the headline number: one second of an open,
// untouched dashboard. Everything it measures happens whether or not the
// user is looking, so this is the cost of simply having jobtail on screen.
func BenchmarkRefreshTick(b *testing.B) {
	for _, shape := range []struct {
		name        string
		w, h        int
		runs, logKB int
	}{
		{"wide/200runs/256KB-log", 172, 40, 200, 256},
		{"narrow/200runs/256KB-log", 80, 24, 200, 256},
		{"wide/20runs/8KB-log", 172, 40, 20, 8},
	} {
		b.Run(shape.name, func(b *testing.B) {
			m := benchModel(b, shape.w, shape.h, shape.runs, shape.logKB)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m = send(b, m, tickMsg(time.Now()))
				_ = m.View() // Bubble Tea renders a frame after every Update
			}
		})
	}
}

// BenchmarkView is one frame render with nothing changed — the work
// repeated for every keystroke and every mouse move (mouse cell motion is
// enabled, so moving the pointer across the pane is a stream of Updates).
func BenchmarkView(b *testing.B) {
	for _, size := range []struct {
		name string
		w, h int
	}{
		{"172x40", 172, 40}, {"80x24", 80, 24}, {"46x22", 46, 22},
	} {
		b.Run(size.name, func(b *testing.B) {
			m := benchModel(b, size.w, size.h, 200, 64)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = m.View()
			}
		})
	}
}

// BenchmarkLayout is the geometry pass: breakpoint selection, column
// fitting, pane heights. It runs on every window resize and, because pane
// heights follow the row count, on every data reload too.
func BenchmarkLayout(b *testing.B) {
	m := benchModel(b, 172, 40, 200, 8)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.layout()
	}
}

// BenchmarkRenderTranscript parses and renders an agent log. This is the
// single most expensive thing a tick can trigger.
func BenchmarkRenderTranscript(b *testing.B) {
	for _, kb := range []int{8, 256, 1024} {
		b.Run(fmt.Sprintf("%dKB", kb), func(b *testing.B) {
			raw := benchTranscript(b, kb)
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = renderTranscript(raw)
			}
		})
	}
}

// BenchmarkParseTranscript and BenchmarkRenderBlocks split the two halves
// apart, because only one of them is on the interactive path. A log is parsed
// once when it is read; it is *rendered* again on every cursor step, fold and
// resize, so BenchmarkRenderBlocks is what governs how a keypress feels in a
// long transcript.
func BenchmarkParseTranscript(b *testing.B) {
	for _, kb := range []int{8, 256, 1024} {
		b.Run(fmt.Sprintf("%dKB", kb), func(b *testing.B) {
			raw := benchTranscript(b, kb)
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = parseClaudeTranscript(raw)
			}
		})
	}
}

func BenchmarkRenderBlocks(b *testing.B) {
	for _, kb := range []int{8, 256, 1024} {
		b.Run(fmt.Sprintf("%dKB", kb), func(b *testing.B) {
			blocks := parseClaudeTranscript(benchTranscript(b, kb))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = renderBlocks(blocks, renderOpts{width: 168, cursor: 0})
			}
		})
	}
}

// BenchmarkWrapForViewport is the other half of preparing a log for
// display: word-wrapping the rendered transcript to the pane width.
func BenchmarkWrapForViewport(b *testing.B) {
	rendered := renderTranscript(benchTranscript(b, 256))
	b.SetBytes(int64(len(rendered)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wrapForViewport(rendered, 168)
	}
}

// BenchmarkJobRows is the per-reload row build for the jobs pane.
func BenchmarkJobRows(b *testing.B) {
	st, _ := seedStore(b)
	addJobs(b, st, "worker", 30)
	jobs, err := st.ListJobs(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	cols := jobsColumns(120)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = jobRows(jobs, cols)
	}
}

var _ tea.Msg = tickMsg(time.Time{})
