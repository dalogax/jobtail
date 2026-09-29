package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// plainLines renders blocks for a pane of the given width and strips the ANSI,
// so a test can assert on shape and indentation without matching escapes.
func plainLines(blocks []block, width, cursor int, expanded map[int]bool) []string {
	v := renderBlocks(blocks, renderOpts{width: width, cursor: cursor, expanded: expanded})
	return strings.Split(ansi.Strip(v.content), "\n")
}

// toolResult builds the two events claude actually emits for one tool call: the
// call on an assistant message, the result on the following user message.
func toolResult(name, input, toolUseID, content string) string {
	return fmt.Sprintf(
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`+"\n"+
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}}`+"\n",
		toolUseID, name, input, toolUseID, content)
}

// The bug this whole rendering path was built around: a tool_result's text
// lives in "content", not "text", so reading it as "text" rendered every tool
// result in the pane as an empty line while the output sat in the log file.
func TestToolResultContentIsRendered(t *testing.T) {
	raw := toolResult("Bash", `{"command":"ls"}`, "toolu_1", "file-a.txt\nfile-b.txt")
	blocks := parseClaudeTranscript(raw)

	if len(blocks) != 1 {
		t.Fatalf("a tool call and its result should be one block, got %d: %+v", len(blocks), blocks)
	}
	if got, want := blocks[0].body, []string{"file-a.txt", "file-b.txt"}; !equalStrings(got, want) {
		t.Fatalf("tool output body = %q, want %q", got, want)
	}
	if out := renderPlain(blocks); !strings.Contains(out, "file-a.txt") {
		t.Fatalf("tool output missing from the rendered transcript:\n%s", out)
	}
}

// A tool that returns structured content sends an array of blocks rather than a
// string, and decoding it as a string outright loses the whole result.
func TestToolResultAcceptsBlockArrayContent(t *testing.T) {
	raw := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/tmp/x"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"line one"},{"type":"image"}]}]}}
`
	blocks := parseClaudeTranscript(raw)
	if len(blocks) != 1 {
		t.Fatalf("want one paired block, got %d", len(blocks))
	}
	if got, want := blocks[0].body, []string{"line one", "[image]"}; !equalStrings(got, want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// A tool result whose call was never seen must still be shown rather than
// silently dropped — a truncated log can begin mid-call.
func TestOrphanToolResultIsStillRendered(t *testing.T) {
	raw := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"gone","content":"orphaned output"}]}}` + "\n"
	if out := renderPlain(parseClaudeTranscript(raw)); !strings.Contains(out, "orphaned output") {
		t.Fatalf("an unpaired tool result was dropped:\n%s", out)
	}
}

// Folding is the feature: long tool output is hidden until asked for, and the
// marker has to say how much, or output that stops looks like output that ended.
func TestToolOutputFoldsWithACountAndExpands(t *testing.T) {
	var body []string
	for i := 1; i <= 40; i++ {
		body = append(body, fmt.Sprintf("out %d", i))
	}
	blocks := []block{{kind: blockTool, title: "Bash", arg: "ls", body: body}}

	if !blocks[0].foldable() {
		t.Fatal("40 lines of output should be foldable")
	}

	folded := strings.Join(plainLines(blocks, 80, -1, nil), "\n")
	if !strings.Contains(folded, "out 3") {
		t.Errorf("the first lines should survive folding:\n%s", folded)
	}
	if strings.Contains(folded, "out 4") {
		t.Errorf("folded output should stop after 3 lines:\n%s", folded)
	}
	if !strings.Contains(folded, "… +37 lines") {
		t.Errorf("folded output must say how much is hidden:\n%s", folded)
	}

	expanded := strings.Join(plainLines(blocks, 80, -1, map[int]bool{0: true}), "\n")
	if !strings.Contains(expanded, "out 40") {
		t.Errorf("expanding should reveal the whole body:\n%s", expanded)
	}
	if strings.Contains(expanded, "… +") {
		t.Errorf("an expanded block should carry no fold marker:\n%s", expanded)
	}
}

// Output short enough to show whole earns neither a marker nor a cursor stop:
// a cursor that stops on nothing-to-expand is a cursor that wastes keypresses.
func TestShortToolOutputIsNotFoldable(t *testing.T) {
	b := block{kind: blockTool, title: "Bash", body: []string{"one", "two"}}
	if b.foldable() {
		t.Fatal("two lines of output should not be foldable")
	}
	v := renderBlocks([]block{b}, renderOpts{width: 80, cursor: -1})
	if len(v.stops) != 0 {
		t.Fatalf("stops = %v, want none", v.stops)
	}
}

// Thinking is folded to its head line by default. The real logs captured on
// this box carry thinking blocks with empty content (redacted at capture, only
// a signature present), so this path is covered by a fixture rather than by
// replaying one of them.
func TestThinkingFoldsToItsHeadLine(t *testing.T) {
	raw := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"step one\nstep two\nstep three","signature":"sig"}]}}` + "\n"
	blocks := parseClaudeTranscript(raw)
	if len(blocks) != 1 || blocks[0].kind != blockThinking {
		t.Fatalf("want one thinking block, got %+v", blocks)
	}

	folded := strings.Join(plainLines(blocks, 80, -1, nil), "\n")
	if !strings.Contains(folded, "Thinking (3 lines)") {
		t.Errorf("folded thinking should summarize its size:\n%s", folded)
	}
	if strings.Contains(folded, "step one") {
		t.Errorf("folded thinking should hide its content:\n%s", folded)
	}

	expanded := strings.Join(plainLines(blocks, 80, -1, map[int]bool{0: true}), "\n")
	for _, want := range []string{"step one", "step two", "step three"} {
		if !strings.Contains(expanded, want) {
			t.Errorf("expanded thinking missing %q:\n%s", want, expanded)
		}
	}
}

// An empty thinking block carries nothing to read, so it must not become a
// "Thinking (0 lines)" row — which is exactly what every real log here holds.
func TestEmptyThinkingIsDropped(t *testing.T) {
	raw := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"","signature":"sig"}]}}` + "\n"
	if blocks := parseClaudeTranscript(raw); len(blocks) != 0 {
		t.Fatalf("an empty thinking block should be dropped, got %+v", blocks)
	}
}

// The cursor marks one block, and only on its first line: using the marked
// gutter for continuation lines too drew a bar down the block's whole left edge.
func TestCursorMarksOnlyTheHeadLine(t *testing.T) {
	blocks := []block{{kind: blockTool, title: "Bash", arg: "ls", body: []string{"a", "b", "c", "d", "e"}}}
	lines := plainLines(blocks, 80, 0, nil)

	if !strings.HasPrefix(lines[0], "▸ ") {
		t.Fatalf("the cursor block's head line should carry the marker, got %q", lines[0])
	}
	for i, l := range lines[1:] {
		if strings.Contains(l, "▸") {
			t.Fatalf("line %d of the cursor block also carries the marker: %q", i+1, l)
		}
	}
}

// Every line reserves the cursor's two columns whether or not it is the cursor,
// so moving the cursor never shifts the transcript sideways.
func TestCursorDoesNotReflowTheTranscript(t *testing.T) {
	blocks := []block{
		{kind: blockTool, title: "Bash", arg: "ls", body: []string{"a", "b", "c", "d"}},
		{kind: blockText, body: []string{"some prose"}},
	}
	withCursor := plainLines(blocks, 80, 0, nil)
	without := plainLines(blocks, 80, -1, nil)

	if len(withCursor) != len(without) {
		t.Fatalf("line counts differ with and without a cursor: %d vs %d", len(withCursor), len(without))
	}
	// Only the marker itself may differ, and only on the block's head line:
	// swapping "▸" back for the space it replaced must reproduce the plain
	// rendering exactly, byte for byte.
	for i := range withCursor {
		if strings.Replace(withCursor[i], "▸", " ", 1) != without[i] {
			t.Fatalf("line %d shifted: %q vs %q", i, withCursor[i], without[i])
		}
	}
}

// The hanging indent is the whole reason this renderer wraps its own text: a
// tool result sits under a ⎿ gutter, and a line long enough to wrap has to stay
// under it rather than starting back at column zero.
func TestWrappedToolOutputKeepsItsIndent(t *testing.T) {
	long := strings.Repeat("word ", 60)
	blocks := []block{{kind: blockTool, title: "Read", arg: "x.txt", body: []string{long, "second", "third", "fourth"}}}
	lines := plainLines(blocks, 60, -1, map[int]bool{0: true})

	// lines[0] is the tool head; lines[1] opens the body with "⎿ ".
	indent := strings.Index(lines[1], "⎿")
	if indent < 0 {
		t.Fatalf("no ⎿ gutter on the first body line: %q", lines[1])
	}
	body := lines[2:]
	if len(body) < 2 {
		t.Fatalf("expected the long line to wrap, got %d body lines", len(body))
	}
	for i, l := range body {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if got := len(l) - len(strings.TrimLeft(l, " ")); got != indent+2 {
			t.Errorf("body line %d indented %d columns, want %d: %q", i, got, indent+2, l)
		}
	}
}

// No rendered line may exceed the pane, or the viewport crops it and the text
// is unrecoverable at any scroll position.
func TestNoRenderedLineExceedsTheWidth(t *testing.T) {
	blocks := []block{
		{kind: blockSession, title: "sess-0123456789abcdef"},
		{kind: blockTool, title: "Bash", arg: strings.Repeat("x", 90), body: []string{strings.Repeat("y ", 80)}},
		{kind: blockText, body: []string{strings.Repeat("prose ", 50)}},
		{kind: blockResult, ok: true, meta: "4 turns · 15.1s · $0.1894"},
	}
	for _, width := range []int{40, 60, 80, 120} {
		for _, l := range plainLines(blocks, width, 1, map[int]bool{1: true}) {
			if w := lipgloss.Width(l); w > width {
				t.Errorf("width %d: line is %d cols: %q", width, w, l)
			}
		}
	}
}

// The result event's metrics are the most useful line in a scheduled run's log;
// they were being thrown away in favour of a bare "ok".
func TestResultLineCarriesItsMetrics(t *testing.T) {
	// The cost is the value a real captured run carried, float noise included.
	raw := `{"type":"result","subtype":"success","is_error":false,"num_turns":4,"duration_ms":15126,"total_cost_usd":0.18935580000000002,"result":"all done"}` + "\n"
	out := renderPlain(parseClaudeTranscript(raw))
	for _, want := range []string{"ok", "4 turns", "15.1s", "$0.1894"} {
		if !strings.Contains(out, want) {
			t.Errorf("result line missing %q:\n%s", want, out)
		}
	}
}

// A slimmer or older result event must not produce "0 turns · 0ms · $0.0000".
func TestResultLineOmitsAbsentMetrics(t *testing.T) {
	raw := `{"type":"result","subtype":"success","is_error":false,"result":"done"}` + "\n"
	out := renderPlain(parseClaudeTranscript(raw))
	for _, notWant := range []string{"0 turns", "$0.0000", "0ms"} {
		if strings.Contains(out, notWant) {
			t.Errorf("result line invented a zero metric %q:\n%s", notWant, out)
		}
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("the status itself should still be there:\n%s", out)
	}
}

func TestResultLineReportsErrors(t *testing.T) {
	raw := `{"type":"result","subtype":"error","is_error":true,"num_turns":1}` + "\n"
	if out := renderPlain(parseClaudeTranscript(raw)); !strings.Contains(out, "error") {
		t.Fatalf("a failed result should render as an error:\n%s", out)
	}
}

// Tool calls are labelled the way the agent CLIs label them: the one argument
// that says what the call was about, not the whole JSON input.
func TestToolArgSummaryPicksTheUsefulArgument(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"Read", `{"file_path":"/home/j/workspace/jobtail/PRD.md","offset":469}`, ".../jobtail/PRD.md"},
		{"Bash", `{"command":"go test ./...","description":"run tests"}`, "go test ./..."},
		{"Grep", `{"pattern":"TODO","path":"/a/b/c"}`, "TODO in .../b/c"},
		{"Glob", `{"pattern":"**/*.go"}`, "**/*.go"},
		{"WebFetch", `{"url":"https://example.com/x"}`, "https://example.com/x"},
	} {
		if got := toolArgSummary(tc.name, []byte(tc.input)); got != tc.want {
			t.Errorf("%s: summary = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// An unrecognized tool must degrade to the old compact-JSON rendering rather
// than to a blank label.
func TestToolArgSummaryFallsBackToCompactJSON(t *testing.T) {
	got := toolArgSummary("SomeNewTool", []byte(`{"alpha":1,"beta":"two"}`))
	if !strings.Contains(got, "alpha") {
		t.Fatalf("unknown tool lost its input: %q", got)
	}
}

// Bash commands keep their shell operators literally: json.Marshal escapes
// <, > and & by default, which turned a command into \u0026\u0026 on screen.
func TestToolArgSummaryKeepsShellOperators(t *testing.T) {
	got := toolArgSummary("Bash", []byte(`{"command":"npm install \u0026\u0026 npm test \u003e out.log"}`))
	if got != "npm install && npm test > out.log" {
		t.Fatalf("summary = %q, want the operators unescaped", got)
	}
}

// The same rule on the fallback path, which is the one compactJSON is actually
// on. A recognized tool like Bash reads its argument straight out of the decoded
// input and never re-encodes it, so it keeps passing even with the escaping bug
// back; only an unrecognized tool goes through compactJSON.
func TestCompactJSONKeepsShellOperators(t *testing.T) {
	raw := []byte(`{"command":"npm install \u0026\u0026 npm test \u003e out.log"}`)
	if got := compactJSON(raw); !strings.Contains(got, "&& npm test > out.log") {
		t.Errorf("compactJSON re-escaped the operators: %q", got)
	}
	if got := toolArgSummary("SomeUnknownTool", raw); !strings.Contains(got, "&&") {
		t.Errorf("the unknown-tool fallback re-escaped the operators: %q", got)
	}
}

func TestInlineMarkdown(t *testing.T) {
	st := stylesFor(false)
	for _, tc := range []struct{ in, want string }{
		{"**bold** text", "bold text"},
		{"a `code` span", "a code span"},
		{"an *italic* word", "an italic word"},
		{"## A heading", "A heading"},
		{"#hashtag stays", "#hashtag stays"},
		// The flanking rule: arithmetic must not become emphasis.
		{"2 * 3 * 4", "2 * 3 * 4"},
		// An unmatched delimiter is literal text.
		{"a lone * asterisk", "a lone * asterisk"},
		{"unclosed **bold", "unclosed **bold"},
	} {
		if got := ansi.Strip(inlineMarkdown(tc.in, st)); got != tc.want {
			t.Errorf("inlineMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Styling must actually be emitted — a silently unstyled pane was the starting
// point for all of this.
//
// The color profile has to be forced: lipgloss detects that `go test`'s output
// is not a terminal and renders every style as plain text, so without this the
// assertion would pass against a renderer that emits nothing at all.
func TestStyledModeEmitsEscapesAndPlainModeDoesNot(t *testing.T) {
	restore := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(restore) })

	if got := inlineMarkdown("**bold**", stylesFor(false)); got == ansi.Strip(got) {
		t.Errorf("bold produced no escape sequences: %q", got)
	}
	if got := inlineMarkdown("**bold**", stylesFor(true)); got != "bold" {
		t.Errorf("plain mode emitted styling: %q", got)
	}

	blocks := []block{{kind: blockTool, title: "Bash", arg: "ls", body: []string{"a", "b", "c", "d"}}}
	styled := renderBlocks(blocks, renderOpts{width: 80, cursor: 0}).content
	if styled == ansi.Strip(styled) {
		t.Errorf("the styled pane rendering carries no escapes:\n%q", styled)
	}
	if plain := renderPlain(blocks); plain != ansi.Strip(plain) {
		t.Errorf("the plain rendering carries escapes:\n%q", plain)
	}
}

// Consecutive unparseable lines belong to one block: blocks render a blank line
// apart, so a plain-text log arriving line by line would come out double-spaced.
func TestConsecutiveRawLinesStayOneBlock(t *testing.T) {
	blocks := parseClaudeTranscript("not json\nstill not json\nnor this\n")
	if len(blocks) != 1 {
		t.Fatalf("want one raw block, got %d: %+v", len(blocks), blocks)
	}
	if got := renderPlain(blocks); got != "not json\nstill not json\nnor this\n" {
		t.Fatalf("raw lines were reflowed: %q", got)
	}
}

// Block positions are what the pane scrolls by, so they have to point at the
// lines the block actually occupies.
func TestBlockPositionsMatchTheRenderedLines(t *testing.T) {
	blocks := []block{
		{kind: blockText, body: []string{"first"}},
		{kind: blockTool, title: "Bash", arg: "ls", body: []string{"a", "b", "c", "d", "e"}},
		{kind: blockResult, ok: true},
	}
	v := renderBlocks(blocks, renderOpts{width: 80, cursor: -1})
	lines := strings.Split(ansi.Strip(v.content), "\n")

	if len(v.lines) != len(lines) {
		t.Fatalf("v.lines holds %d lines, content has %d", len(v.lines), len(lines))
	}
	if !strings.Contains(lines[v.top[0]], "first") {
		t.Errorf("block 0 top points at %q", lines[v.top[0]])
	}
	if !strings.Contains(lines[v.top[1]], "Bash") {
		t.Errorf("block 1 top points at %q", lines[v.top[1]])
	}
	if !strings.Contains(lines[v.top[2]], "ok") {
		t.Errorf("block 2 top points at %q", lines[v.top[2]])
	}
	for i := range blocks {
		if v.bottom[i] < v.top[i] || v.bottom[i] >= len(lines) {
			t.Errorf("block %d spans [%d,%d] of %d lines", i, v.top[i], v.bottom[i], len(lines))
		}
	}
}

// markCursor is an optimization: patching two lines must land on exactly the
// content a full re-render would have produced, or the fast path drifts from
// the slow one and the pane shows a marker in the wrong place.
func TestMarkCursorMatchesAFullRerender(t *testing.T) {
	blocks := []block{
		{kind: blockSession, title: "sess-1"},
		{kind: blockTool, title: "Bash", arg: "ls", body: []string{"a", "b", "c", "d"}},
		{kind: blockText, body: []string{"some prose"}},
		{kind: blockThinking, title: "Thinking", body: []string{"one", "two"}},
		{kind: blockResult, ok: true, meta: "1 turns"},
	}
	st := stylesFor(false)
	v := renderBlocks(blocks, renderOpts{width: 80, cursor: -1})
	if len(v.stops) < 2 {
		t.Fatalf("stops = %v, want at least two to step between", v.stops)
	}

	from := -1
	for _, to := range append(v.stops, -1) {
		v.markCursor(from, to, st)
		want := renderBlocks(blocks, renderOpts{width: 80, cursor: to}).content
		if v.content != want {
			t.Fatalf("cursor %d -> %d:\n got %q\nwant %q", from, to, v.content, want)
		}
		from = to
	}
}

// A raw line whose own text begins with spaces must keep them: stripping two
// prefixes instead of one would eat the content's indentation every time the
// cursor passed by.
func TestMarkCursorKeepsIndentedRawContent(t *testing.T) {
	blocks := []block{
		{kind: blockRaw, body: []string{"    already indented"}},
		{kind: blockTool, title: "Bash", arg: "ls", body: []string{"a", "b", "c", "d"}},
	}
	st := stylesFor(false)
	v := renderBlocks(blocks, renderOpts{width: 80, cursor: -1})

	for i := 0; i < 3; i++ {
		v.markCursor(-1, 0, st)
		v.markCursor(0, -1, st)
	}
	if got := ansi.Strip(v.lines[v.top[0]]); got != "      already indented" {
		t.Fatalf("raw line lost its own indent after cursor passes: %q", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
