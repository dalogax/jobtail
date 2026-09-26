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

func TestRenderTranscriptSuppressesDuplicateResultText(t *testing.T) {
	raw := `{"type":"system","subtype":"init","session_id":"sess-1"}
{"type":"assistant","message":{"content":[{"type":"text","text":"All done here."}]}}
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1","result":"All done here."}
`
	got := renderTranscript(raw)
	if strings.Count(got, "All done here.") != 1 {
		t.Fatalf("result text identical to the last assistant message should not be printed twice:\n%s", got)
	}
	if !strings.Contains(got, "result: ok") {
		t.Fatalf("result status line should still be present:\n%s", got)
	}
}

func TestRenderTranscriptDropsUnrecognizedEventTypes(t *testing.T) {
	// rate_limit_event is real, observed output from a live agent run
	// (found via an actual screenshot: it was leaking through as a raw
	// JSON blob before the `default` case stopped dumping unrecognized
	// event types verbatim).
	raw := `{"type":"system","subtype":"init","session_id":"sess-1"}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Working on it."}]}}
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1","result":"Working on it."}
`
	got := renderTranscript(raw)
	if strings.Contains(got, "rate_limit_event") {
		t.Fatalf("unrecognized event types should be dropped, not shown as raw JSON:\n%s", got)
	}
	if !strings.Contains(got, "Working on it.") {
		t.Fatalf("the actual transcript content should still render:\n%s", got)
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
