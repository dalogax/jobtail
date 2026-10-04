package tui

import "github.com/charmbracelet/lipgloss"

// The dashboard's palette, named by role rather than by hue: every style in
// the package is built from these, so a color means one thing everywhere it
// appears and changing one is a one-line edit. The names follow the semantic
// token set of the tui-design skill (fg.muted, border.focus, status.error…).
//
// Status colors are the terminal's own ANSI 1–3 so they follow the user's
// theme; the greys are fixed 256-color steps, picked so that
// faint < muted < default reads as a ladder on a dark background and the
// two selection fills stay distinguishable from each other and from the
// base.
var (
	// Text.
	fgMuted = lipgloss.Color("245") // labels, headers, key descriptions
	fgFaint = lipgloss.Color("242") // inert rows: disabled jobs, skipped runs
	fgKey   = lipgloss.Color("255") // a key in a hint

	// Transcript text: tool output a step below body text, and the two
	// inline roles an agent's prose carries.
	fgOutput   = lipgloss.Color("250")
	fgThinking = lipgloss.Color("140")
	fgCode     = lipgloss.Color("180")

	// Borders. Each pane has its own accent (btop-style); unfocused panes
	// all share the one quiet border.
	borderDefault = lipgloss.Color("240")
	accentJobs    = lipgloss.Color("39")  // blue
	accentRuns    = lipgloss.Color("135") // violet
	accentLog     = lipgloss.Color("42")  // green

	// Selection: the cursor row of the focused pane, and the remembered row
	// of the others — one step quieter, so only one cursor reads as live.
	selectionBG         = lipgloss.Color("237")
	selectionInactiveBG = lipgloss.Color("235")

	// Status.
	statusSuccess = lipgloss.Color("2")
	statusError   = lipgloss.Color("1")
	statusWarning = lipgloss.Color("3")
)

var (
	roundedPane   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	helpStyle     = lipgloss.NewStyle().Foreground(fgMuted)
	helpKeyStyle  = lipgloss.NewStyle().Foreground(fgKey).Bold(true)
	headerStyle   = lipgloss.NewStyle().Foreground(fgMuted)
	faintStyle    = lipgloss.NewStyle().Foreground(fgFaint)
	statusOK      = lipgloss.NewStyle().Foreground(statusSuccess)
	statusFailed  = lipgloss.NewStyle().Foreground(statusError)
	statusRunning = lipgloss.NewStyle().Foreground(statusWarning)
	statusNever   = faintStyle
	emptyStyle    = lipgloss.NewStyle().Foreground(fgMuted)
	emptyCmdStyle = lipgloss.NewStyle().Foreground(fgOutput)
)
