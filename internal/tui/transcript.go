package tui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strings"
)

// contentBlock mirrors the bits of Claude Code's stream-json content blocks
// that are worth rendering: text, and tool use/result. Unrecognized shapes
// fall back to the raw line rather than erroring.
type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`  // tool_use
	Input json.RawMessage `json:"input"` // tool_use
}

type transcriptLine struct {
	Type    string `json:"type"` // system | assistant | user | result
	Subtype string `json:"subtype"`
	Message struct {
		Content []contentBlock `json:"content"`
	} `json:"message"`
	SessionID string `json:"session_id"`
	IsError   *bool  `json:"is_error"`
	Result    string `json:"result"`
}

// renderTranscript turns a captured stream-json log into a readable
// transcript: assistant text, one-line tool call summaries, and a final
// result line — not raw JSON (PRD §9).
func renderTranscript(raw string) string {
	var out strings.Builder
	var lastText string
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev transcriptLine
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			out.WriteString(line + "\n")
			continue
		}
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" {
				fmt.Fprintf(&out, "* session %s started\n", ev.SessionID)
			}
		case "assistant", "user":
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					if strings.TrimSpace(c.Text) != "" {
						fmt.Fprintf(&out, "%s\n\n", c.Text)
						lastText = c.Text
					}
				case "tool_use":
					fmt.Fprintf(&out, "> %s(%s)\n", c.Name, compactJSON(c.Input))
				case "tool_result":
					fmt.Fprintf(&out, "< %s\n", firstLine(c.Text))
				}
			}
		case "result":
			status := "ok"
			if ev.IsError != nil && *ev.IsError {
				status = "error"
			}
			fmt.Fprintf(&out, "--- result: %s ---\n", status)
			// The result event's own text is often just a repeat of the
			// last assistant message already printed above; only show it
			// again when it actually adds something.
			if ev.Result != "" && ev.Result != lastText {
				fmt.Fprintf(&out, "%s\n", ev.Result)
			}
		default:
			// system/hook/rate_limit_event/thinking_tokens/etc: internal
			// bookkeeping events, not part of the readable transcript.
			// Dropped, not dumped as raw JSON (found via an actual
			// screenshot: a rate_limit_event line was leaking straight
			// through as an unparsed JSON blob).
		}
	}
	if out.Len() == 0 {
		return raw // nothing parsed as JSON at all: show it verbatim rather than an empty pane
	}
	return out.String()
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	s := string(b)
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + "..."
	}
	return s
}

// openCodePart mirrors the bits of opencode's `run --format json` events
// worth rendering, captured directly from the real CLI (v1.18.21): each
// line's top-level "type" is one of tool_use/text/error/step_start/
// step_finish; the nested "part" object restates a similar but not
// identical type (tool/text/step-start/step-finish, hyphenated).
type openCodePart struct {
	Type string `json:"type"` // tool_use | text | error | step_start | step_finish
	Part struct {
		Tool  string `json:"tool"`
		Text  string `json:"text"`
		State struct {
			Input  json.RawMessage `json:"input"`
			Output string          `json:"output"`
		} `json:"state"`
	} `json:"part"`
	Error struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// renderOpenCodeTranscript turns a captured opencode `run --format json`
// log into the same kind of readable transcript renderTranscript produces
// for claude: assistant text, one-line tool call summaries. opencode has
// no distinct terminal "result" event (see runOpenCodeAgent's comment), so
// there's no closing "--- result ---" line to render here.
func renderOpenCodeTranscript(raw string) string {
	var out strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev openCodePart
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			out.WriteString(line + "\n")
			continue
		}
		switch ev.Type {
		case "text":
			if strings.TrimSpace(ev.Part.Text) != "" {
				fmt.Fprintf(&out, "%s\n\n", ev.Part.Text)
			}
		case "tool_use":
			fmt.Fprintf(&out, "> %s(%s)\n", ev.Part.Tool, compactJSON(ev.Part.State.Input))
			if ev.Part.State.Output != "" {
				fmt.Fprintf(&out, "< %s\n", firstLine(ev.Part.State.Output))
			}
		case "error":
			fmt.Fprintf(&out, "--- error: %s ---\n%s\n", ev.Error.Name, ev.Error.Data.Message)
		default:
			// step_start/step_finish: internal turn bookkeeping, not part
			// of the readable transcript.
		}
	}
	if out.Len() == 0 {
		return raw
	}
	return out.String()
}

// codexPart mirrors the bits of codex's `exec --json` events worth
// rendering. Only the error path has been observed against the real CLI
// (0.148.0) on this box — see runCodexAgent's comment — so
// item.completed's assistant-text shape below is inferred from codex's
// documented JSONL event model, not independently verified here; unknown
// shapes fall back to the raw line rather than erroring, same as
// renderTranscript.
type codexPart struct {
	Type string `json:"type"` // thread.started | turn.started | item.completed | turn.failed | error
	Item struct {
		Type    string `json:"type"` // e.g. "agent_message", "error"
		Text    string `json:"text"`
		Message string `json:"message"`
	} `json:"item"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"` // top-level "error" events
}

// renderCodexTranscript is codex's counterpart to renderTranscript.
func renderCodexTranscript(raw string) string {
	var out strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev codexPart
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			out.WriteString(line + "\n")
			continue
		}
		switch ev.Type {
		case "thread.started":
			// thread_id isn't rendered here — it's captured separately as
			// the run's session id (see runCodexAgent).
		case "item.completed":
			switch ev.Item.Type {
			case "error":
				fmt.Fprintf(&out, "--- error ---\n%s\n", ev.Item.Message)
			default:
				if strings.TrimSpace(ev.Item.Text) != "" {
					fmt.Fprintf(&out, "%s\n\n", ev.Item.Text)
				}
			}
		case "turn.failed":
			fmt.Fprintf(&out, "--- turn failed ---\n%s\n", ev.Error.Message)
		case "error":
			fmt.Fprintf(&out, "--- error ---\n%s\n", ev.Message)
		default:
			// turn.started/etc: internal bookkeeping, not rendered.
		}
	}
	if out.Len() == 0 {
		return raw
	}
	return out.String()
}
