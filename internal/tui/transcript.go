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
			fmt.Fprintf(&out, "--- result: %s ---\n%s\n", status, ev.Result)
		default:
			out.WriteString(line + "\n")
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
