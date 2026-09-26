package tui

import (
	"strings"
	"testing"
)

// The opencode fixtures below are lightly trimmed from a real `opencode
// run --format json` invocation against opencode/big-pickle on 2026-09-26
// (see execengine's runOpenCodeAgent comment) — not hand-constructed, so
// the renderer is verified against actual field shapes, not a guess.
func TestRenderOpenCodeTranscriptShowsTextAndToolCalls(t *testing.T) {
	raw := `{"type":"step_start","timestamp":1,"sessionID":"ses_abc","part":{"type":"step-start"}}
{"type":"tool_use","timestamp":2,"sessionID":"ses_abc","part":{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":"/tmp/test.txt"},"output":"hello world"}}}
{"type":"text","timestamp":3,"sessionID":"ses_abc","part":{"type":"text","text":"I'll read the file first."}}
{"type":"step_finish","timestamp":4,"sessionID":"ses_abc","part":{"type":"step-finish","reason":"stop"}}
`
	got := renderOpenCodeTranscript(raw)
	for _, want := range []string{"I'll read the file first.", "read(", "filePath", "hello world"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered opencode transcript missing %q:\n%s", want, got)
		}
	}
	for _, notWant := range []string{`"sessionID"`, "step_start", "step_finish"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("rendered opencode transcript should not leak raw bookkeeping %q:\n%s", notWant, got)
		}
	}
}

func TestRenderOpenCodeTranscriptShowsError(t *testing.T) {
	raw := `{"type":"error","timestamp":1,"sessionID":"ses_abc","error":{"name":"UnknownError","data":{"message":"Unexpected server error. Check server logs for details.","ref":"err_768c802e"}}}
`
	got := renderOpenCodeTranscript(raw)
	if !strings.Contains(got, "UnknownError") || !strings.Contains(got, "Unexpected server error") {
		t.Fatalf("rendered opencode transcript should surface the error, got:\n%s", got)
	}
}

func TestRenderOpenCodeTranscriptFallsBackOnUnparseableInput(t *testing.T) {
	raw := "not json at all\n"
	if got := renderOpenCodeTranscript(raw); got != raw {
		t.Fatalf("expected verbatim fallback for unparseable input, got:\n%s", got)
	}
}

// The codex fixtures below mirror the real event shapes captured from
// `codex exec --json` against an unauthenticated CLI on 2026-09-26 (see
// runCodexAgent's comment: only this error path has been observed against
// the real binary, since this box has no stored codex credentials).
func TestRenderCodexTranscriptShowsTurnFailed(t *testing.T) {
	raw := `{"type":"thread.started","thread_id":"01a0df33-972a-7ec0-9a99-a7836ea659c6"}
{"type":"turn.started"}
{"type":"error","message":"Reconnecting... 1/5 (unexpected status 401 Unauthorized)"}
{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized: Missing bearer or basic authentication in header"}}
`
	got := renderCodexTranscript(raw)
	for _, want := range []string{"turn failed", "401 Unauthorized"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered codex transcript missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "thread_id") {
		t.Fatalf("rendered codex transcript should not leak the raw thread_id field (captured separately as session id):\n%s", got)
	}
}

func TestRenderCodexTranscriptShowsAgentMessage(t *testing.T) {
	// item.completed's assistant-text shape is inferred from codex's
	// documented event model, not independently verified against a
	// successful real run (no credentials on this box) — see
	// runCodexAgent's comment.
	raw := `{"type":"item.completed","item":{"type":"agent_message","text":"OK"}}
`
	got := renderCodexTranscript(raw)
	if !strings.Contains(got, "OK") {
		t.Fatalf("rendered codex transcript missing assistant text:\n%s", got)
	}
}

func TestRenderCodexTranscriptFallsBackOnUnparseableInput(t *testing.T) {
	raw := "not json at all\n"
	if got := renderCodexTranscript(raw); got != raw {
		t.Fatalf("expected verbatim fallback for unparseable input, got:\n%s", got)
	}
}
