package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/cellbuf"
)

// gutterWidth is the two columns reserved at the far left for the cursor
// marker. It is reserved on *every* line, cursor or not, so that moving the
// cursor never reflows the transcript sideways.
const gutterWidth = 2

// minWrapWidth keeps a deeply indented block readable in a pane too narrow for
// its own prefix: below this the text overflows rather than being wrapped to
// two or three columns, which produces one word per line and hundreds of them.
const minWrapWidth = 20

// plainWidth is the width the unstyled render wraps to. Wide enough that
// nothing realistic wraps, so the plain rendering stays a faithful dump.
const plainWidth = 1 << 20

// renderOpts is how a transcript is drawn.
//
// plain is the non-terminal rendering: no ANSI, no cursor gutter, every block
// expanded. It is what a test or a piped dump wants, and keeping it a mode of
// the real renderer — rather than a second implementation — is what stops the
// two from drifting.
type renderOpts struct {
	width    int
	cursor   int // block index under the cursor; -1 for none
	expanded map[int]bool
	plain    bool
}

// tsStyles is the transcript palette, resolved once per render so the plain
// mode can neutralize every style at one place instead of at each use.
//
// The bullets and the ⎿ gutter deliberately echo what the agent CLIs
// themselves draw, so a transcript read here and the same session resumed
// with "r" look like the same conversation rather than two unrelated
// renderings of it.
type tsStyles struct {
	textBullet, toolBullet, toolName, toolArg lipgloss.Style
	output, thinking, dim, cursorMark         lipgloss.Style
	errBullet, inlineBold, inlineCode         lipgloss.Style
	inlineItalic                              lipgloss.Style
	resultOK, resultError                     lipgloss.Style
}

func stylesFor(plain bool) tsStyles {
	if plain {
		var n lipgloss.Style // no attributes: Render returns its input unchanged
		return tsStyles{n, n, n, n, n, n, n, n, n, n, n, n, n, n}
	}
	return tsStyles{
		textBullet:   lipgloss.NewStyle().Foreground(accentLog),
		toolBullet:   lipgloss.NewStyle().Foreground(accentJobs),
		toolName:     lipgloss.NewStyle().Bold(true),
		toolArg:      lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		output:       lipgloss.NewStyle().Foreground(lipgloss.Color("250")),
		thinking:     lipgloss.NewStyle().Foreground(lipgloss.Color("140")).Italic(true),
		dim:          lipgloss.NewStyle().Foreground(lipgloss.Color("244")),
		cursorMark:   lipgloss.NewStyle().Foreground(accentLog).Bold(true),
		errBullet:    lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		inlineBold:   lipgloss.NewStyle().Bold(true),
		inlineCode:   lipgloss.NewStyle().Foreground(lipgloss.Color("180")),
		inlineItalic: lipgloss.NewStyle().Italic(true),
		resultOK:     lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true),
		resultError:  lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true),
	}
}

// transcriptView is a rendered transcript plus the index the log pane needs to
// scroll to a block. Positions are line offsets into content, which is what
// viewport.SetYOffset takes.
type transcriptView struct {
	content string
	lines   []string // content, unjoined — what markCursor patches in place
	top     []int    // top[i] = first line of block i
	bottom  []int    // bottom[i] = last line of block i, inclusive
	stops   []int    // block indices the cursor may land on, in order
}

// markCursor moves the cursor marker from one block to another by rewriting the
// two head lines that change, rather than re-rendering the transcript.
//
// Stepping with j/k is the one thing a reader does repeatedly, and a full
// re-render costs time proportional to the whole log — which run logs are capped
// at 10 MB of, not at the ~160 KB a real run produces. Patching two lines makes
// the cost of a keypress independent of how much is expanded below it.
func (v *transcriptView) markCursor(from, to int, st tsStyles) {
	mark := st.cursorMark.Render("▸") + " "
	blank := strings.Repeat(" ", gutterWidth)
	set := func(i int, gutter string) {
		if i < 0 || i >= len(v.top) {
			return
		}
		ln := v.top[i]
		v.lines[ln] = gutter + stripGutter(v.lines[ln], mark, blank)
	}
	set(from, blank)
	set(to, mark)
	v.content = strings.Join(v.lines, "\n")
}

// stripGutter removes whichever gutter a line carries — at most one of them.
// Trying both in turn would eat a second prefix off any line whose own content
// happens to start with spaces.
func stripGutter(s, mark, blank string) string {
	if r, ok := strings.CutPrefix(s, mark); ok {
		return r
	}
	if r, ok := strings.CutPrefix(s, blank); ok {
		return r
	}
	return s
}

// renderBlocks draws a parsed transcript, with one block optionally marked as
// the cursor and a set of blocks expanded.
//
// It does its own wrapping rather than leaving it to wrapForViewport, because
// the indents are hanging: a tool's output sits under a ⎿ gutter, and a line
// that wraps has to stay under it. cellbuf.Wrap has no notion of a
// continuation prefix, so the only way to keep the shape is to wrap each
// block's text to the width left over after its own prefix, then re-apply that
// prefix per resulting line.
func renderBlocks(blocks []block, o renderOpts) transcriptView {
	v := transcriptView{
		top:    make([]int, len(blocks)),
		bottom: make([]int, len(blocks)),
	}
	width := o.width
	if o.plain {
		width = plainWidth
	} else if width <= 0 {
		width = 80
	}
	st := stylesFor(o.plain)

	var out []string
	for i, b := range blocks {
		if len(out) > 0 {
			out = append(out, "")
		}
		v.top[i] = len(out)
		out = append(out, renderBlock(b, width, st, i == o.cursor && !o.plain, o.plain || o.expanded[i])...)
		v.bottom[i] = len(out) - 1
		if b.foldable() {
			v.stops = append(v.stops, i)
		}
	}
	v.lines = out
	v.content = strings.Join(out, "\n")
	return v
}

func renderBlock(b block, width int, st tsStyles, isCursor, isExpanded bool) []string {
	// Two gutters, not one: the cursor marker belongs on the block's first line
	// only. Using the marked gutter as the continuation prefix too drew a ▸ down
	// the whole left edge of the selected block, which read as a change bar
	// rather than a cursor.
	gut, cont := strings.Repeat(" ", gutterWidth), strings.Repeat(" ", gutterWidth)
	if isCursor {
		gut = st.cursorMark.Render("▸") + " "
	}
	if width == plainWidth {
		gut, cont = "", "" // plain mode: no room reserved for a cursor that can't exist
	}

	switch b.kind {
	case blockSession:
		return wrapPrefixed(st.dim.Render("✻ session "+b.title), width, gut, cont+"  ")

	case blockText:
		// Assistant prose is the payload: never folded, and the only place
		// inline markdown is applied, since it's the only place the agent
		// writes markdown on purpose.
		var lines []string
		for i, l := range b.body {
			head := gut + st.textBullet.Render("● ")
			if i > 0 {
				head = cont + "  "
			}
			lines = append(lines, wrapPrefixed(inlineMarkdown(l, st), width, head, cont+"  ")...)
		}
		return lines

	case blockThinking:
		head := fmt.Sprintf("✻ Thinking (%s)", plural(len(b.body), "line"))
		lines := wrapPrefixed(st.thinking.Render(head), width, gut, cont+"  ")
		if !isExpanded {
			return lines
		}
		for _, l := range b.body {
			lines = append(lines, wrapPrefixed(st.thinking.Render(l), width, cont+"  ", cont+"  ")...)
		}
		return lines

	case blockTool:
		bullet := st.toolBullet.Render("● ")
		if b.isErr {
			bullet = st.errBullet.Render("● ")
		}
		head := bullet + st.toolName.Render(b.title)
		if b.arg != "" {
			head += st.toolArg.Render("(" + b.arg + ")")
		}
		lines := wrapPrefixed(head, width, gut, cont+"  ")
		return append(lines, renderToolBody(b, width, st, cont, isExpanded)...)

	case blockResult:
		status, style := "ok", st.resultOK
		if !b.ok {
			status, style = "error", st.resultError
		}
		line := st.dim.Render("── result: ") + style.Render(status)
		if b.meta != "" {
			line += st.dim.Render(" · " + b.meta)
		}
		lines := wrapPrefixed(line, width, gut, cont+"   ")
		for _, l := range b.body {
			lines = append(lines, wrapPrefixed(inlineMarkdown(l, st), width, cont+"   ", cont+"   ")...)
		}
		return lines

	default: // blockRaw
		var lines []string
		for i, l := range b.body {
			head := gut
			if i > 0 {
				head = cont
			}
			lines = append(lines, wrapPrefixed(l, width, head, cont)...)
		}
		return lines
	}
}

// renderToolBody draws a tool's output under its ⎿ gutter, folded to the first
// few lines unless expanded, with a marker saying how much is hidden. The
// marker is what makes folding discoverable: output that just stops looks like
// output that ended.
func renderToolBody(b block, width int, st tsStyles, cont string, isExpanded bool) []string {
	if len(b.body) == 0 {
		return nil
	}
	show, hidden := b.body, 0
	if !isExpanded {
		if n := b.foldedBodyLines(); n >= 0 && len(b.body) > n {
			show, hidden = b.body[:n], len(b.body)-n
		}
	}

	var lines []string
	indent := cont + "    "
	for i, l := range show {
		head := cont + "  " + st.dim.Render("⎿ ")
		if i > 0 {
			head = indent
		}
		lines = append(lines, wrapPrefixed(st.output.Render(l), width, head, indent)...)
	}
	if hidden > 0 {
		lines = append(lines, indent+st.dim.Render(fmt.Sprintf("… +%s", plural(hidden, "line"))))
	}
	return lines
}

// wrapPrefixed wraps already-styled text to the room left over after its
// prefix, then puts head on the first resulting line and cont on the rest.
//
// The text is styled before wrapping on purpose: cellbuf.Wrap is ANSI-aware
// and carries style state across the break, whereas styling afterwards would
// have to re-open the style on every line and would nest a reset inside the
// caller's own styling.
func wrapPrefixed(text string, width int, head, cont string) []string {
	// The room left over is set by the *widest* of the two prefixes, not by the
	// first one. A tool head sits at the gutter while its wrapped continuation
	// lines are indented two further, so sizing to the head alone overflowed the
	// pane by exactly that indent — and the viewport crops an overlong line
	// rather than wrapping it, so the tail was unreachable at any scroll offset.
	inner := width - max(lipgloss.Width(head), lipgloss.Width(cont))
	if inner < minWrapWidth {
		inner = minWrapWidth
	}
	parts := strings.Split(cellbuf.Wrap(text, inner, ""), "\n")
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		prefix := cont
		if i == 0 {
			prefix = head
		}
		out = append(out, prefix+p)
	}
	return out
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// inlineMarkdown styles the forms agents actually emit in prose: ATX headings,
// **bold**, *italic* and `code`. Everything else is left exactly as written.
//
// Deliberately not a markdown renderer. A full one (glamour) brings its own
// theme, its own palette and its own wrapping, all three of which would fight
// the pane — the wrapping in particular has to stay under this file's control
// because of the hanging indents.
//
// Order matters: ** is consumed before * so that bold isn't read as two
// italics with an empty span between them.
func inlineMarkdown(s string, st tsStyles) string {
	if h := headingText(s); h != "" {
		return st.inlineBold.Render(h)
	}
	s = styleDelimited(s, "`", st.inlineCode)
	s = styleDelimited(s, "**", st.inlineBold)
	return styleDelimited(s, "*", st.inlineItalic)
}

// headingText returns the text of an ATX heading line ("## Summary" ->
// "Summary"), or "" if the line isn't one. The hashes carry no information once
// the text is bold, and in a narrow pane they cost characters that the heading
// itself needs.
func headingText(s string) string {
	t := strings.TrimLeft(s, "#")
	if len(t) == len(s) || len(s)-len(t) > 6 {
		return ""
	}
	if !strings.HasPrefix(t, " ") {
		return "" // "#hashtag", not a heading
	}
	return strings.TrimSpace(t)
}

// styleDelimited styles every span of s fenced by a matching pair of delim,
// dropping the delimiters. An unmatched opener is left alone, so a lone
// asterisk or backtick renders as itself.
//
// The span must not begin or end on a space — markdown's flanking rule. Without
// it, "2 * 3 * 4" reads as an italic " 3 " and prose loses its asterisks to
// arithmetic.
func styleDelimited(s, delim string, style lipgloss.Style) string {
	var out strings.Builder
	for {
		start := strings.Index(s, delim)
		if start < 0 {
			out.WriteString(s)
			return out.String()
		}
		rest := s[start+len(delim):]
		end := strings.Index(rest, delim)
		if end < 0 {
			out.WriteString(s)
			return out.String()
		}
		inner := rest[:end]
		if inner == "" || strings.HasPrefix(inner, " ") || strings.HasSuffix(inner, " ") {
			// Not emphasis: emit the opener as literal text and carry on from
			// just after it, so the closer stays available as a new opener.
			out.WriteString(s[:start+len(delim)])
			s = rest
			continue
		}
		out.WriteString(s[:start])
		out.WriteString(style.Render(inner))
		s = rest[end+len(delim):]
	}
}

// ---------------------------------------------------------------------------
// Plain-text entry points
// ---------------------------------------------------------------------------

// renderPlain is the unstyled, fully expanded rendering of a whole log — the
// shape the transcript had before the pane grew a cursor, and still the right
// answer for anything that isn't a terminal.
//
// It is newline-terminated, like the file it came from: this is text to print
// or pipe, not a block to lay out, and a dump whose last line has no newline
// runs into whatever is printed next.
func renderPlain(blocks []block) string {
	s := renderBlocks(blocks, renderOpts{cursor: -1, plain: true}).content
	if s == "" {
		return ""
	}
	return s + "\n"
}

func renderTranscript(raw string) string { return renderPlain(parseClaudeTranscript(raw)) }

func renderOpenCodeTranscript(raw string) string { return renderPlain(parseOpenCodeTranscript(raw)) }

func renderCodexTranscript(raw string) string { return renderPlain(parseCodexTranscript(raw)) }
