package tui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A captured agent log is parsed into blocks before it is rendered, rather
// than straight into a string. Two things need that intermediate form: the
// log pane's cursor, which moves between foldable blocks rather than lines,
// and the hanging indents, which have to be re-applied to every line a block
// wraps onto and so can only be applied once the pane width is known.
//
// The parse is provider-shaped (claude/opencode/codex all frame their JSONL
// differently); everything after it is not.
type blockKind int

const (
	blockSession  blockKind = iota // session <id> started
	blockText                      // assistant prose
	blockThinking                  // extended-thinking content, folded by default
	blockTool                      // one tool call *and* its result, paired
	blockResult                    // the terminal ok/error line plus its metrics
	blockRaw                       // a line that didn't parse as JSON at all
)

// block is one renderable unit of a transcript.
//
// body holds the block's full content, split into logical lines but not yet
// wrapped or indented. For blockTool it is the tool's output, which is what
// folding hides: a Read of a 600-line file is one line of intent and 600 of
// noise, and the pane is unreadable if it shows the noise by default.
type block struct {
	kind  blockKind
	title string   // tool name, "Thinking", or the session id
	arg   string   // tool argument summary: the PRD.md in Read(PRD.md)
	body  []string // assistant text, thinking text, or tool output
	ok    bool     // blockResult: false renders as an error
	meta  string   // blockResult: "4 turns · 15.1s · $0.1894"
	isErr bool     // blockTool: the tool itself reported failure

	// toolUseID pairs a tool_use with the tool_result that answers it. Claude
	// emits them in separate events — the call on an assistant message, the
	// result on the *next* user message — so the parser has to stitch them
	// back together to render them as one block. Unused by opencode/codex,
	// which carry a tool's input and output on a single event.
	toolUseID string
}

// foldedBodyLines is how much of a block's body survives folding. Tool output
// keeps a few lines because the first ones are usually the informative ones
// (a file's head, a command's first output); thinking keeps none, since its
// value is in the whole chain or not at all. -1 means never folded.
func (b block) foldedBodyLines() int {
	switch b.kind {
	case blockTool:
		return 3
	case blockThinking:
		return 0
	default:
		return -1
	}
}

// foldable reports whether a block has anything worth hiding, and so whether
// the log pane's cursor should stop on it. A tool call that returned two
// lines earns neither a fold marker nor a cursor stop.
func (b block) foldable() bool {
	n := b.foldedBodyLines()
	return n >= 0 && len(b.body) > n
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

// contentBlock mirrors the content blocks inside claude's stream-json
// assistant/user messages.
//
// Content is deliberately a RawMessage: a tool_result's content is a string
// for ordinary text output but an array of content blocks when the tool
// returns something structured. Reading it as `text` — which is the *text*
// block's field, not the tool_result's — is what made every tool result in
// the log pane render as an empty line, with the content sitting in the file
// the whole time.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`     // text
	Thinking  string          `json:"thinking"` // thinking
	Name      string          `json:"name"`     // tool_use
	ID        string          `json:"id"`       // tool_use
	Input     json.RawMessage `json:"input"`    // tool_use
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result
	IsError   *bool           `json:"is_error"`
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

	// The result event carries the numbers that make a scheduled run worth
	// looking at — how long it took, how many turns, what it cost. They were
	// being discarded in favour of a bare "ok".
	DurationMS   int64    `json:"duration_ms"`
	NumTurns     int      `json:"num_turns"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
}

// parseClaudeTranscript turns a captured stream-json log into blocks.
func parseClaudeTranscript(raw string) []block {
	var blocks []block
	var lastText string
	pending := map[string]int{} // tool_use_id -> index of the blockTool awaiting it

	eachJSONLine(raw, func(line string) {
		var ev transcriptLine
		if json.Unmarshal([]byte(line), &ev) != nil {
			appendRaw(&blocks, line)
			return
		}
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" && ev.SessionID != "" {
				blocks = append(blocks, block{kind: blockSession, title: ev.SessionID})
			}
		case "assistant", "user":
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					if strings.TrimSpace(c.Text) != "" {
						blocks = append(blocks, block{kind: blockText, body: splitLines(c.Text)})
						lastText = c.Text
					}
				case "thinking":
					if strings.TrimSpace(c.Thinking) != "" {
						blocks = append(blocks, block{
							kind:  blockThinking,
							title: "Thinking",
							body:  splitLines(c.Thinking),
						})
					}
				case "tool_use":
					blocks = append(blocks, block{
						kind:      blockTool,
						title:     c.Name,
						arg:       toolArgSummary(c.Name, c.Input),
						toolUseID: c.ID,
					})
					if c.ID != "" {
						pending[c.ID] = len(blocks) - 1
					}
				case "tool_result":
					text := toolResultText(c.Content)
					i, ok := pending[c.ToolUseID]
					if !ok {
						// A result with no call in hand: render it standalone
						// rather than dropping it on the floor.
						blocks = append(blocks, block{kind: blockTool, title: "result", body: splitLines(text)})
						continue
					}
					delete(pending, c.ToolUseID)
					blocks[i].body = splitLines(text)
					blocks[i].isErr = c.IsError != nil && *c.IsError
				}
			}
		case "result":
			b := block{
				kind: blockResult,
				ok:   ev.IsError == nil || !*ev.IsError,
				meta: resultMeta(ev),
			}
			// The result event's own text is usually a verbatim repeat of the
			// last assistant message; only keep it when it adds something.
			if ev.Result != "" && ev.Result != lastText {
				b.body = splitLines(ev.Result)
			}
			blocks = append(blocks, b)
		default:
			// system/hook/rate_limit_event/thinking_tokens/etc: internal
			// bookkeeping, not part of the readable transcript. Dropped
			// rather than dumped as raw JSON (found via an actual screenshot:
			// a rate_limit_event was leaking through as an unparsed blob).
		}
	})
	return blocks
}

// openCodePart mirrors the bits of opencode's `run --format json` events
// worth rendering, captured from the real CLI (v1.18.21): each line's
// top-level "type" is one of tool_use/text/error/step_start/step_finish; the
// nested "part" object restates a similar but not identical type.
type openCodePart struct {
	Type string `json:"type"`
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

// parseOpenCodeTranscript is opencode's counterpart to
// parseClaudeTranscript. opencode carries a tool's input and output on one
// event, so there is no pairing to do, and it has no terminal result event
// (see runOpenCodeAgent), so there is no closing blockResult to emit.
func parseOpenCodeTranscript(raw string) []block {
	var blocks []block
	eachJSONLine(raw, func(line string) {
		var ev openCodePart
		if json.Unmarshal([]byte(line), &ev) != nil {
			appendRaw(&blocks, line)
			return
		}
		switch ev.Type {
		case "text":
			if strings.TrimSpace(ev.Part.Text) != "" {
				blocks = append(blocks, block{kind: blockText, body: splitLines(ev.Part.Text)})
			}
		case "tool_use":
			blocks = append(blocks, block{
				kind:  blockTool,
				title: ev.Part.Tool,
				arg:   toolArgSummary(ev.Part.Tool, ev.Part.State.Input),
				body:  splitLines(ev.Part.State.Output),
			})
		case "error":
			blocks = append(blocks, block{
				kind: blockResult,
				ok:   false,
				meta: ev.Error.Name,
				body: splitLines(ev.Error.Data.Message),
			})
		default:
			// step_start/step_finish: turn bookkeeping, not rendered.
		}
	})
	return blocks
}

// codexPart mirrors the bits of codex's `exec --json` events worth
// rendering. Only the error path has been observed against the real CLI
// (0.148.0) on this box — see runCodexAgent — so item.completed's
// assistant-text shape is from codex's documented event model rather than
// verified here; unknown shapes fall back to the raw line.
type codexPart struct {
	Type string `json:"type"`
	Item struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Message string `json:"message"`
	} `json:"item"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

// parseCodexTranscript is codex's counterpart to parseClaudeTranscript.
func parseCodexTranscript(raw string) []block {
	var blocks []block
	eachJSONLine(raw, func(line string) {
		var ev codexPart
		if json.Unmarshal([]byte(line), &ev) != nil {
			appendRaw(&blocks, line)
			return
		}
		switch ev.Type {
		case "thread.started":
			// thread_id is captured as the run's session id, not rendered.
		case "item.completed":
			if ev.Item.Type == "error" {
				blocks = append(blocks, block{kind: blockResult, body: splitLines(ev.Item.Message)})
				return
			}
			if strings.TrimSpace(ev.Item.Text) != "" {
				blocks = append(blocks, block{kind: blockText, body: splitLines(ev.Item.Text)})
			}
		case "turn.failed":
			blocks = append(blocks, block{kind: blockResult, meta: "turn failed", body: splitLines(ev.Error.Message)})
		case "error":
			blocks = append(blocks, block{kind: blockResult, body: splitLines(ev.Message)})
		default:
			// turn.started/etc: bookkeeping, not rendered.
		}
	})
	return blocks
}

// appendRaw records a line that didn't parse, merging it into the preceding
// raw block if there is one.
//
// Merging matters: blocks are rendered one blank line apart, so a plain-text
// log arriving a line at a time would come out double-spaced — which is what a
// log that isn't JSON at all (a cli command's output misfiled as an agent's, a
// crash before the first event) looks like.
func appendRaw(blocks *[]block, line string) {
	if n := len(*blocks); n > 0 && (*blocks)[n-1].kind == blockRaw {
		(*blocks)[n-1].body = append((*blocks)[n-1].body, line)
		return
	}
	*blocks = append(*blocks, block{kind: blockRaw, body: []string{line}})
}

// eachJSONLine runs fn over every non-blank line of raw. It exists so the
// three parsers share the scanner buffer sizing: stream-json lines routinely
// exceed bufio's default 64 KB limit, and a transcript that silently stopped
// at the first long line would look like a truncated run.
func eachJSONLine(raw string, fn func(line string)) {
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fn(line)
	}
}

// parseTranscript dispatches to the parser for the agent CLI that produced
// the log. A log that yielded nothing at all comes back as a single raw block
// holding it verbatim, so an unrecognized format shows its contents rather
// than an empty pane.
func parseTranscript(raw, provider string) []block {
	var blocks []block
	switch provider {
	case "opencode":
		blocks = parseOpenCodeTranscript(raw)
	case "codex":
		blocks = parseCodexTranscript(raw)
	default:
		blocks = parseClaudeTranscript(raw)
	}
	if len(blocks) == 0 && strings.TrimSpace(raw) != "" {
		return []block{{kind: blockRaw, body: splitLines(raw)}}
	}
	return blocks
}

// ---------------------------------------------------------------------------
// Field extraction
// ---------------------------------------------------------------------------

// toolResultText pulls readable text out of a tool_result's content, which is
// a bare string for ordinary output and an array of content blocks when the
// tool returns something structured (an image, or text plus metadata).
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var texts []string
		for _, p := range parts {
			switch {
			case p.Text != "":
				texts = append(texts, p.Text)
			case p.Type != "":
				texts = append(texts, "["+p.Type+"]")
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(raw)
}

// toolArgSummary reduces a tool call's input to the one thing worth putting on
// the head line, the way the agent CLIs' own UIs do: Read(PRD.md), not
// Read({"file_path":"/home/jarvis/workspace/jobtail/PRD.md"}). Unrecognized
// tools fall back to compact JSON, so a new or custom tool degrades to the
// old rendering rather than to nothing.
func toolArgSummary(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return compactJSON(input)
	}
	str := func(k string) string {
		s, _ := m[k].(string)
		return s
	}
	var pick string
	switch name {
	case "Read", "Write", "Edit", "NotebookEdit":
		pick = shortenPath(str("file_path"))
	case "Bash", "BashOutput":
		pick = firstLine(str("command"))
	case "Grep":
		pick = str("pattern")
		if p := str("path"); p != "" {
			pick += " in " + shortenPath(p)
		}
	case "Glob":
		pick = str("pattern")
	case "Task", "Agent":
		pick = str("description")
	case "WebFetch":
		pick = str("url")
	case "WebSearch":
		pick = str("query")
	case "Skill":
		pick = str("skill")
	}
	if strings.TrimSpace(pick) == "" {
		return compactJSON(input)
	}
	return truncateRunes(pick, 100)
}

// shortenPath keeps a path recognizable without spending the whole head line
// on it: the last two segments are what distinguish one file from another in
// practice, and the pane is often only 80 columns wide.
func shortenPath(p string) string {
	if p == "" {
		return ""
	}
	parts := strings.Split(strings.TrimSuffix(p, "/"), "/")
	if len(parts) <= 2 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}

// resultMeta formats the numbers the result event carries. Any that are absent
// are left out, so an older or slimmer event still produces a sensible line
// rather than "0 turns · 0ms · $0.0000".
func resultMeta(ev transcriptLine) string {
	var parts []string
	if ev.NumTurns > 0 {
		parts = append(parts, fmt.Sprintf("%d turns", ev.NumTurns))
	}
	if ev.DurationMS > 0 {
		parts = append(parts, humanDuration(time.Duration(ev.DurationMS)*time.Millisecond))
	}
	if ev.TotalCostUSD != nil && *ev.TotalCostUSD > 0 {
		parts = append(parts, "$"+strconv.FormatFloat(*ev.TotalCostUSD, 'f', 4, 64))
	}
	return strings.Join(parts, " · ")
}

// humanDuration keeps a duration short and comparable down a column: "2ms",
// "8.9s", "4m05s", "1h12m". time.Duration's own String gave "8.921s" next to
// "2ms" and "4m5.123s".
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	// json.Marshal escapes &, < and > as \u0026 etc. for safe embedding in
	// HTML, which turns every `a && b` a tool ran into `a \u0026\u0026 b`.
	// This is terminal output, so write them as-is.
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return string(raw)
	}
	return truncateRunes(strings.TrimSuffix(b.String(), "\n"), 120)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + "…"
	}
	return s
}

// truncateRunes cuts to a rune count, not a byte count: slicing a string
// holding an em dash or a box-drawing character mid-rune renders as U+FFFD.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// splitLines splits body text into lines, dropping the trailing empty line a
// final newline would otherwise produce.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}
