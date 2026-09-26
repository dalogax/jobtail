package tui

import (
	"strings"
	"testing"
)

func TestRenderTranscriptHidesRawJSON(t *testing.T) {
	raw := `{"type":"system","subtype":"init","session_id":"sess-1"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello there, checking the repo now."}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1","result":"done"}
`
	got := renderTranscript(raw)

	for _, want := range []string{"Hello there, checking the repo now.", "Bash", "result: ok"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered transcript missing %q:\n%s", want, got)
		}
	}
	for _, notWant := range []string{`"type":"assistant"`, `"session_id"`} {
		if strings.Contains(got, notWant) {
			t.Fatalf("rendered transcript should not show raw JSON field %q:\n%s", notWant, got)
		}
	}
}

func TestRenderTranscriptFallsBackOnUnparseableInput(t *testing.T) {
	raw := "not json at all\njust plain text\n"
	got := renderTranscript(raw)
	if got != raw {
		t.Fatalf("expected verbatim fallback for unparseable input, got:\n%s", got)
	}
}

func TestRenderLogPassesThroughCLIJobsVerbatim(t *testing.T) {
	raw := "line one\nline two\n"
	got := renderLog(raw, "cli")
	if got != raw {
		t.Fatalf("cli logs should render verbatim, got:\n%s", got)
	}
}
