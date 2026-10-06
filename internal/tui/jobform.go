package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	bkey "github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/cellbuf"

	"github.com/dalogax/jobtail/internal/cronx"
	"github.com/dalogax/jobtail/internal/execengine"
	"github.com/dalogax/jobtail/internal/runner"
	"github.com/dalogax/jobtail/internal/store"
)

// The job form is the dashboard's version of `jobtail add` and `jobtail edit`:
// every flag those take, as one field each, in a modal over the dashboard.
//
// It has two modes, because the keys the form is driven by are printable. While
// moving between fields, s saves and q quits; while typing into one, every
// printable key is text, so the field is left with enter, tab or esc and the
// form can only be saved with ctrl+s. A form whose s typed an "s" into the
// prompt half the time would be worse than one with no s at all.

type fieldKind int

const (
	fieldText   fieldKind = iota // one line of text
	fieldNumber                  // a non-negative integer
	fieldMulti                   // text that may span lines: a command, a prompt
	fieldChoice                  // one of a fixed set, cycled in place
	fieldToggle                  // yes / no
)

// Field keys. A field is looked up by key rather than by position, since which
// fields are on screen depends on the job's kind.
const (
	fID              = "id"
	fKind            = "kind"
	fCron            = "cron"
	fCwd             = "cwd"
	fCommand         = "cmd"
	fPrompt          = "prompt"
	fProvider        = "provider"
	fModel           = "model"
	fPermission      = "permission-mode"
	fEnabled         = "enabled"
	fTimezone        = "timezone"
	fTimeout         = "timeout"
	fPrecheck        = "precheck"
	fPrecheckTimeout = "precheck-timeout"
	fNotify          = "notify"
	fMaxConcurrent   = "max-concurrent"
	fKeep            = "keep"
)

var (
	kindOptions     = []string{"cli", "agent"}
	providerOptions = []string{execengine.ProviderClaude, execengine.ProviderOpenCode, execengine.ProviderCodex}
	toggleOptions   = []string{"yes", "no"}
)

type formField struct {
	key, label, help string
	kind             fieldKind
	placeholder      string

	input textinput.Model // fieldText, fieldNumber
	area  textarea.Model  // fieldMulti
	text  string          // the value of a text field while it isn't being typed into

	options []string // fieldChoice, fieldToggle
	choice  int

	// fixed fields are shown but can't be changed: a job's id and kind, once
	// it exists, exactly as `jobtail edit` has no flag for either.
	fixed bool
	err   string
}

func (f *formField) value() string {
	switch f.kind {
	case fieldChoice, fieldToggle:
		return f.options[f.choice]
	}
	return f.text
}

func (f *formField) editable() bool {
	return !f.fixed && (f.kind == fieldText || f.kind == fieldNumber || f.kind == fieldMulti)
}

type jobForm struct {
	editID string // the job being edited; "" for a new one
	fields []*formField

	cursor  int  // index into fields; always a field that is shown
	editing bool // typing into fields[cursor]
	before  string
	offset  int // first content row shown, when the form is taller than the screen

	initial     map[string]string // values as the form opened, for the dirty mark
	providerWas string            // a stored "" means claude; saving untouched keeps it ""
	enabledWas  bool
	confirmQuit bool   // q on a changed form asks before throwing the changes away
	err         string // a save that failed for a reason no one field owns
	attempted   bool   // save was pressed once: errors now show for every field
}

// newJobForm opens the form for j, or for a new job when j is nil. A new job
// starts in cwd, which is where the dashboard was opened — the directory the
// user is most likely to mean.
func newJobForm(j *store.Job, cwd string) *jobForm {
	f := &jobForm{}
	if j == nil {
		j = &store.Job{Kind: "cli", Enabled: true, Timezone: "local", Cwd: cwd, MaxConcurrent: 1, Keep: 200}
	} else {
		f.editID = j.ID
	}
	f.providerWas = j.Provider
	f.enabledWas = j.Enabled
	provider := j.Provider
	if provider == "" {
		provider = execengine.ProviderClaude
	}
	enabled := "yes"
	if !j.Enabled {
		enabled = "no"
	}
	num := func(n int64) string { return strconv.FormatInt(n, 10) }
	f.fields = []*formField{
		textField(fID, "ID", fieldText, j.ID, "required, e.g. backup-check",
			"Name shown in the dashboard. It can't be changed once the job exists."),
		choiceField(fKind, "Kind", fieldChoice, kindOptions, j.Kind,
			"cli runs a shell command; agent runs one headless turn of an agent CLI."),
		textField(fCron, "Cron", fieldText, j.Cron, "required, e.g. */15 * * * *",
			"5 fields: minute hour day-of-month month day-of-week. No seconds, no @daily."),
		textField(fCwd, "Working dir", fieldText, j.Cwd, "required",
			"The existing directory the job runs in. ~ is expanded."),
		textField(fCommand, "Command", fieldMulti, j.Command, "required for cli",
			"Shell command, run with sh -c in the working dir."),
		textField(fPrompt, "Prompt", fieldMulti, j.Prompt, "required for agent",
			"What the agent does each run: one turn, with no one there to answer questions."),
		choiceField(fProvider, "Provider", fieldChoice, providerOptions, provider,
			"Which agent CLI runs the prompt."),
		textField(fModel, "Model", fieldText, j.Model, "provider default",
			"Model alias, e.g. sonnet. Empty uses the provider's default."),
		textField(fPermission, "Permission mode", fieldText, j.PermissionMode, "",
			""), // placeholder and help depend on the provider: see permissionHint
		choiceField(fEnabled, "Enabled", fieldToggle, toggleOptions, enabled,
			"A disabled job keeps its history but never fires on its schedule."),
		textField(fTimezone, "Timezone", fieldText, j.Timezone, "local",
			"local, or an IANA name such as Europe/Madrid."),
		textField(fTimeout, "Timeout", fieldNumber, num(j.TimeoutSeconds), "0",
			"Seconds before a run is killed and recorded as timeout. 0 = the 30m default."),
		textField(fPrecheck, "Precheck", fieldMulti, j.Precheck, "none",
			"Shell gate before each run: exit 0 runs the job, 1 skips it, 2 or more fails it."),
		textField(fPrecheckTimeout, "Precheck timeout", fieldNumber, num(j.PrecheckTimeoutSeconds), "0",
			"Seconds before the precheck is killed. 0 = no limit."),
		textField(fNotify, "Notify", fieldText, j.Notify, "failed,timeout (default)",
			"Herdr notifications: "+strings.Join(runner.AllEvents, ",")+"; or all, none. Empty = the default."),
		textField(fMaxConcurrent, "Max concurrent", fieldNumber, strconv.Itoa(j.MaxConcurrent), "1",
			"Runs of this job allowed at once; one due beyond that is recorded as skipped_overlap."),
		textField(fKeep, "Keep", fieldNumber, strconv.Itoa(j.Keep), "200",
			"How many past runs to retain; older ones are pruned."),
	}
	if f.editID != "" {
		f.field(fID).fixed = true
		f.field(fKind).fixed = true
	}
	f.initial = f.values()
	f.cursor = f.step(-1, 1)
	return f
}

func textField(k, label string, kind fieldKind, v, placeholder, help string) *formField {
	f := &formField{key: k, label: label, kind: kind, text: v, placeholder: placeholder, help: help}
	if kind == fieldMulti {
		f.area = textarea.New()
		f.area.ShowLineNumbers = false
		f.area.Prompt = ""
		f.area.CharLimit = 0
		f.area.MaxHeight = 0
		// enter leaves the field, as it does every other text field; a new
		// line is the exception, so it gets the chord.
		f.area.KeyMap.InsertNewline = bkey.NewBinding(bkey.WithKeys("alt+enter", "ctrl+j"))
		st := lipgloss.NewStyle().Background(fieldBG)
		f.area.FocusedStyle.Base = lipgloss.NewStyle()
		f.area.FocusedStyle.CursorLine = st
		f.area.FocusedStyle.Text = st
		f.area.FocusedStyle.EndOfBuffer = st
		// A steady cursor: a blinking one is a timer message every half
		// second for as long as the field is open, for no information.
		f.area.Cursor.SetMode(cursor.CursorStatic)
	} else {
		f.input = textinput.New()
		f.input.Prompt = ""
		f.input.CharLimit = 0
		st := lipgloss.NewStyle().Background(fieldBG)
		f.input.TextStyle = st
		f.input.PlaceholderStyle = st.Foreground(fgFaint)
		f.input.Cursor.TextStyle = st
		f.input.Cursor.SetMode(cursor.CursorStatic)
	}
	return f
}

func choiceField(k, label string, kind fieldKind, options []string, v, help string) *formField {
	f := &formField{key: k, label: label, kind: kind, options: options, help: help}
	for i, o := range options {
		if o == v {
			f.choice = i
		}
	}
	return f
}

func (f *jobForm) field(k string) *formField {
	for _, fl := range f.fields {
		if fl.key == k {
			return fl
		}
	}
	return nil
}

func (f *jobForm) values() map[string]string {
	out := make(map[string]string, len(f.fields))
	for _, fl := range f.fields {
		out[fl.key] = fl.value()
	}
	return out
}

// changed counts the fields that differ from what the form opened with. A
// field the current kind hides still counts: its value would be lost too.
func (f *jobForm) changed() int {
	n := 0
	now := f.current()
	for k, v := range f.initial {
		if now[k] != v {
			n++
		}
	}
	return n
}

// current is values() with the field being typed into read from its input.
func (f *jobForm) current() map[string]string {
	v := f.values()
	if f.editing {
		fl := f.fields[f.cursor]
		v[fl.key] = fl.liveValue()
	}
	return v
}

func (fl *formField) liveValue() string {
	if fl.kind == fieldMulti {
		return fl.area.Value()
	}
	return fl.input.Value()
}

// shown says whether a field belongs to the job's current kind. Hidden fields
// keep their value — switching kind and back loses nothing — but are neither
// validated nor saved.
func (f *jobForm) shown(fl *formField) bool {
	agent := f.field(fKind).value() == "agent"
	switch fl.key {
	case fCommand:
		return !agent
	case fPrompt, fProvider, fModel, fPermission:
		return agent
	}
	return true
}

// step finds the next field from i in direction dir that is shown and can be
// focused, or i itself when there is none.
func (f *jobForm) step(i, dir int) int {
	for j := i + dir; j >= 0 && j < len(f.fields); j += dir {
		if fl := f.fields[j]; f.shown(fl) && !fl.fixed {
			return j
		}
	}
	if i < 0 {
		return 0
	}
	return i
}

// shownIndex is the cursor's position among the focusable fields, for the
// "7/16" in the modal's bottom border.
func (f *jobForm) shownIndex() (pos, total int) {
	for i, fl := range f.fields {
		if !f.shown(fl) || fl.fixed {
			continue
		}
		total++
		if i == f.cursor {
			pos = total
		}
	}
	return pos, total
}

// formResult is what a key did to the form, for the dashboard to act on.
type formResult int

const (
	formStay formResult = iota
	formSave
	formClose
)

func (f *jobForm) update(msg tea.KeyMsg) (formResult, tea.Cmd) {
	if f.confirmQuit {
		switch msg.String() {
		case "y":
			return formClose, nil
		case "s", "ctrl+s":
			f.confirmQuit = false
			return formSave, nil
		case "n", "esc", "q":
			f.confirmQuit = false
		}
		return formStay, nil
	}
	if f.editing {
		return f.updateEditing(msg)
	}
	fl := f.fields[f.cursor]
	switch msg.String() {
	case "s", "ctrl+s":
		return formSave, nil
	case "q", "esc":
		if f.changed() > 0 {
			f.confirmQuit = true
			return formStay, nil
		}
		return formClose, nil
	case "up", "k", "shift+tab":
		f.cursor = f.step(f.cursor, -1)
	case "down", "j", "tab":
		f.cursor = f.step(f.cursor, 1)
	case "home", "g":
		f.cursor = f.step(-1, 1)
	case "end", "G":
		f.cursor = f.step(len(f.fields), -1)
	case "left", "h":
		f.cycle(fl, -1)
	case "right", "l", " ":
		f.cycle(fl, 1)
	case "enter", "i":
		if fl.kind == fieldChoice || fl.kind == fieldToggle {
			f.cycle(fl, 1)
			return formStay, nil
		}
		return formStay, f.startEditing()
	}
	return formStay, nil
}

func (f *jobForm) cycle(fl *formField, dir int) {
	if fl.kind != fieldChoice && fl.kind != fieldToggle {
		return
	}
	n := len(fl.options)
	fl.choice = (fl.choice + dir + n) % n
	if fl.err != "" || f.attempted {
		f.check(fl.key)
	}
}

func (f *jobForm) startEditing() tea.Cmd {
	fl := f.fields[f.cursor]
	if !fl.editable() {
		return nil
	}
	f.editing = true
	f.before = fl.text
	if fl.kind == fieldMulti {
		fl.area.SetValue(fl.text)
		return fl.area.Focus()
	}
	fl.input.SetValue(fl.text)
	fl.input.CursorEnd()
	return fl.input.Focus()
}

// stopEditing leaves the field, keeping what was typed unless revert, and
// validates it: on blur, never per keystroke, so nobody is told off for a cron
// expression they're halfway through writing.
func (f *jobForm) stopEditing(revert bool) {
	fl := f.fields[f.cursor]
	if revert {
		fl.text = f.before
	} else {
		fl.text = fl.liveValue()
	}
	fl.input.Blur()
	fl.area.Blur()
	f.editing = false
	if !revert || fl.err != "" {
		f.check(fl.key)
	}
}

func (f *jobForm) updateEditing(msg tea.KeyMsg) (formResult, tea.Cmd) {
	switch msg.String() {
	case "ctrl+s":
		f.stopEditing(false)
		return formSave, nil
	case "esc":
		f.stopEditing(true)
		return formStay, nil
	case "enter":
		f.stopEditing(false)
		return formStay, nil
	case "tab", "down":
		if f.fields[f.cursor].kind == fieldMulti && msg.String() == "down" {
			break // moves between the lines of the text
		}
		f.stopEditing(false)
		f.cursor = f.step(f.cursor, 1)
		return formStay, nil
	case "shift+tab", "up":
		if f.fields[f.cursor].kind == fieldMulti && msg.String() == "up" {
			break
		}
		f.stopEditing(false)
		f.cursor = f.step(f.cursor, -1)
		return formStay, nil
	}
	fl := f.fields[f.cursor]
	if fl.kind == fieldNumber && msg.Type == tea.KeyRunes {
		for _, r := range msg.Runes {
			if r < '0' || r > '9' {
				return formStay, nil // a number field takes digits only
			}
		}
	}
	var cmd tea.Cmd
	if fl.kind == fieldMulti {
		fl.area, cmd = fl.area.Update(msg)
	} else {
		fl.input, cmd = fl.input.Update(msg)
	}
	// Once a field shows an error, re-check it as it changes, so the error
	// goes away the moment the value is right.
	if fl.err != "" {
		fl.text = fl.liveValue()
		f.check(fl.key)
	}
	return formStay, cmd
}

// check re-validates one field and records its error.
func (f *jobForm) check(k string) {
	_, errs := f.build()
	if fl := f.field(k); fl != nil {
		fl.err = errs[k]
	}
}

// validate checks every shown field, focusing the first one that's wrong.
func (f *jobForm) validate() (store.Job, bool) {
	f.attempted = true
	j, errs := f.build()
	first := -1
	for i, fl := range f.fields {
		fl.err = errs[fl.key]
		if fl.err != "" && first < 0 {
			first = i
		}
	}
	if first >= 0 {
		f.cursor = first
		return j, false
	}
	return j, true
}

// build turns the fields into a job, applying the same rules and defaults as
// `jobtail add`, and returns an error message per field that breaks one.
func (f *jobForm) build() (store.Job, map[string]string) {
	errs := map[string]string{}
	v := func(k string) string { return strings.TrimSpace(f.field(k).value()) }
	num := func(k string) int64 {
		s := v(k)
		if s == "" {
			return 0
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			errs[k] = "must be a whole number, 0 or more"
		}
		return n
	}

	j := store.Job{
		ID:       v(fID),
		Kind:     v(fKind),
		Cron:     v(fCron),
		Timezone: v(fTimezone),
		Enabled:  v(fEnabled) == "yes",
	}
	switch {
	case j.ID == "":
		errs[fID] = "required"
	case strings.ContainsAny(j.ID, " \t\n/"):
		errs[fID] = "no spaces or slashes"
	}
	if j.Timezone == "" {
		j.Timezone = "local"
	}
	switch {
	case j.Cron == "":
		errs[fCron] = "required"
	case cronx.Validate(j.Cron) != nil:
		errs[fCron] = cronx.Validate(j.Cron).Error()
	}
	if _, err := cronx.Next("* * * * *", j.Timezone, time.Now()); err != nil {
		errs[fTimezone] = "unknown timezone"
	}

	cwd, err := resolveDir(v(fCwd))
	if err != nil {
		errs[fCwd] = err.Error()
	}
	j.Cwd = cwd

	if j.Kind == "cli" {
		j.Command = f.field(fCommand).value()
		if strings.TrimSpace(j.Command) == "" {
			errs[fCommand] = "required for a cli job"
		}
	} else {
		j.Prompt = f.field(fPrompt).value()
		if strings.TrimSpace(j.Prompt) == "" {
			errs[fPrompt] = "required for an agent job"
		}
		j.Provider = v(fProvider)
		if j.Provider == execengine.ProviderClaude && f.providerWas == "" {
			j.Provider = "" // still the default, as it was stored
		}
		j.Model = v(fModel)
		j.PermissionMode = v(fPermission)
	}

	j.Precheck = f.field(fPrecheck).value()
	if strings.TrimSpace(j.Precheck) == "" {
		j.Precheck = ""
	}
	j.TimeoutSeconds = num(fTimeout)
	j.PrecheckTimeoutSeconds = num(fPrecheckTimeout)
	j.MaxConcurrent = int(num(fMaxConcurrent))
	if j.MaxConcurrent <= 0 {
		j.MaxConcurrent = 1
	}
	j.Keep = int(num(fKeep))
	if j.Keep <= 0 {
		j.Keep = 200
	}
	if n, err := runner.ParseNotify(v(fNotify)); err != nil {
		errs[fNotify] = strings.TrimPrefix(err.Error(), "--notify: ")
	} else {
		j.Notify = n
	}

	// Only the fields on screen can be wrong: a hidden one can't be fixed.
	for k := range errs {
		if fl := f.field(k); fl != nil && !f.shown(fl) {
			delete(errs, k)
		}
	}
	return j, errs
}

// resolveDir is the form's `--cwd`: an existing directory, made absolute. The
// form has no shell in front of it, so it expands ~ itself.
func resolveDir(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("required")
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, dir[1:])
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return abs, errors.New("no such directory")
	case err != nil:
		return abs, err
	case !info.IsDir():
		return abs, errors.New("not a directory")
	}
	return abs, nil
}

// permissionHint is the placeholder and help for the permission mode, which
// means something different to each agent CLI.
func permissionHint(provider string) (placeholder, help string) {
	switch provider {
	case execengine.ProviderCodex:
		return "workspace-write (default)", "codex --sandbox: read-only, workspace-write or danger-full-access."
	case execengine.ProviderOpenCode:
		return "unused by opencode", "opencode has no permission mode; this is ignored."
	}
	return "acceptEdits (default)", "claude: acceptEdits or bypassPermissions. Never plan: it waits for an approval forever."
}

// aside is the faint note after a field's value: what a cron expression
// means in practice, and the unit of a number.
func (f *jobForm) aside(fl *formField) string {
	switch fl.key {
	case fCron:
		tz := strings.TrimSpace(f.field(fTimezone).value())
		if next, err := cronx.Next(strings.TrimSpace(fl.text), tz, time.Now()); err == nil && fl.text != "" {
			return "next " + next.Local().Format("01-02 15:04")
		}
	case fTimeout:
		if n, _ := strconv.Atoi(fl.text); n > 0 {
			return "s · " + limitLabel(time.Duration(n)*time.Second)
		}
		return "s · 30m default"
	case fPrecheckTimeout:
		return "s"
	case fKeep:
		return "runs"
	}
	return ""
}

// limitLabel is a time limit in its fewest words: "30m", "1h30m", "45s".
// Not humanDuration, whose "30m00s" reads as a measurement.
func limitLabel(d time.Duration) string {
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	out := ""
	if h > 0 {
		out += fmt.Sprintf("%dh", h)
	}
	if m > 0 {
		out += fmt.Sprintf("%dm", m)
	}
	if sec > 0 || out == "" {
		out += fmt.Sprintf("%ds", sec)
	}
	return out
}

// formGeometry is the modal's outer size on a width×height screen: wide enough
// for a prompt to be readable, never the whole of a wide terminal.
func formGeometry(width, height int) (w, h int) {
	w = min(80, width-4)
	if width < 50 {
		w = width
	}
	return w, height
}

// resize fits the text widgets to the value column of a modal this wide.
func (f *jobForm) resize(screenW, screenH int) {
	w, _ := formGeometry(screenW, screenH)
	_, vw := formColumns(w - boxChromeX)
	for _, fl := range f.fields {
		switch fl.kind {
		case fieldMulti:
			fl.area.SetWidth(max(vw-1, 1))
			fl.area.SetHeight(4)
		case fieldText, fieldNumber:
			fl.input.Width = max(vw-2, 1)
		}
	}
}

// formColumns splits the modal's content width into the label column (with
// its two-cell cursor gutter) and the value column.
func formColumns(cw int) (lw, vw int) {
	lw = 18
	if cw < 50 {
		lw = 12
	}
	return lw, max(cw-2-lw, 4)
}

// view renders the modal at the size formGeometry gives it.
func (f *jobForm) view(screenW, screenH int) string {
	w, maxH := formGeometry(screenW, screenH)
	cw := w - boxChromeX
	lw, vw := formColumns(cw)
	pad := func(s string) string { return lipgloss.NewStyle().Width(cw).MaxWidth(cw).Render(s) }

	// Body rows, remembering where the cursor's field sits so a form taller
	// than the screen can scroll to it.
	var body []string
	curTop, curBottom := 0, 0
	nErr := 0
	for i, fl := range f.fields {
		if !f.shown(fl) {
			continue
		}
		if fl.err != "" {
			nErr++
		}
		focused := i == f.cursor
		if focused {
			curTop = len(body)
		}
		marker, label := "  ", helpStyle.Render(truncateLabel(fl.label, lw))
		if focused {
			st := lipgloss.NewStyle().Foreground(accentJobs).Bold(true)
			marker, label = st.Render("❯ "), st.Render(truncateLabel(fl.label, lw))
		}
		label += strings.Repeat(" ", max(lw-lipgloss.Width(truncateLabel(fl.label, lw)), 0))
		for k, line := range f.valueLines(fl, focused, vw) {
			if k == 0 {
				body = append(body, pad(marker+label+line))
			} else {
				body = append(body, pad(strings.Repeat(" ", 2+lw)+line))
			}
		}
		if fl.err != "" {
			body = append(body, pad(strings.Repeat(" ", 2+lw)+
				statusFailed.Render(truncateToWidth("✗ "+fl.err, vw))))
		}
		if focused {
			curBottom = len(body)
		}
	}

	var head []string
	if nErr > 0 {
		word := "fields need"
		if nErr == 1 {
			word = "field needs"
		}
		head = append(head, pad(statusFailed.Bold(true).Render(fmt.Sprintf("✗ %d %s attention", nErr, word))))
	}
	if f.err != "" {
		head = append(head, pad(statusFailed.Render(truncateToWidth("✗ "+f.err, cw))))
	}

	foot := []string{""}
	foot = append(foot, f.footer(cw)...)

	// Fit to the screen: the body scrolls, the head and the foot stay.
	room := maxH - boxChromeY - len(head) - len(foot)
	if room < 1 {
		room = 1
	}
	if room > len(body) {
		room = len(body)
	}
	if curTop < f.offset {
		f.offset = curTop
	}
	if curBottom > f.offset+room {
		f.offset = curBottom - room
	}
	f.offset = min(max(f.offset, 0), max(len(body)-room, 0))
	shownBody := body[f.offset : f.offset+room]

	lines := append(append(head, shownBody...), foot...)
	title := "New job"
	if f.editID != "" {
		title = "Edit " + f.editID
	}
	if f.changed() > 0 {
		title += " ●"
	}
	pos, total := f.shownIndex()
	return renderPane(title, fmt.Sprintf("%d/%d", pos, total), accentJobs, true, strings.Join(lines, "\n"))
}

func truncateLabel(s string, w int) string {
	if lipgloss.Width(s) <= w-1 {
		return s
	}
	return string([]rune(s)[:max(w-2, 1)]) + "…"
}

// valueLines draws one field's value column: an input while it's being typed
// into, otherwise the value (or its placeholder) on the field fill.
func (f *jobForm) valueLines(fl *formField, focused bool, vw int) []string {
	fill := lipgloss.NewStyle().Background(fieldBG).Width(vw).MaxWidth(vw)
	if fl.fixed {
		return []string{helpStyle.Render(truncateToWidth(fl.value(), vw)) + faintStyle.Render("  fixed")}
	}
	switch fl.kind {
	case fieldChoice, fieldToggle:
		var parts []string
		for i, o := range fl.options {
			if i == fl.choice {
				st := lipgloss.NewStyle().Foreground(fgKey).Bold(true)
				if focused {
					st = st.Foreground(accentJobs)
				}
				parts = append(parts, st.Render("● "+o))
			} else {
				parts = append(parts, faintStyle.Render("○ "+o))
			}
		}
		return []string{truncateToWidth(strings.Join(parts, "  "), vw)}
	}

	if f.editing && focused {
		if fl.kind == fieldMulti {
			var out []string
			for _, l := range strings.Split(fl.area.View(), "\n") {
				out = append(out, fill.Render(" "+l))
			}
			return out
		}
		return []string{fill.Render(" " + fl.input.View())}
	}

	placeholder := fl.placeholder
	if fl.key == fPermission {
		placeholder, _ = permissionHint(f.field(fProvider).value())
	}
	text := fl.text
	more := ""
	if lines := strings.Split(text, "\n"); len(lines) > 1 {
		text, more = lines[0], fmt.Sprintf("+%d lines", len(lines)-1)
	}
	st := lipgloss.NewStyle().Background(fieldBG)
	if text == "" && more == "" {
		if lipgloss.Width(placeholder) > vw-2 {
			placeholder = truncateToWidth(placeholder, vw-3) + "…"
		}
		return []string{fill.Render(st.Foreground(fgFaint).Render(" " + placeholder))}
	}
	aside := f.aside(fl)
	if more != "" {
		aside = more
	}
	val := " " + text
	if aside != "" && lipgloss.Width(val)+2+lipgloss.Width(aside) < vw {
		return []string{fill.Render(st.Render(val) + st.Render("  ") + st.Foreground(fgFaint).Render(aside))}
	}
	if lipgloss.Width(val) > vw-1 {
		val = truncateToWidth(val, vw-2) + "…"
	}
	return []string{fill.Render(st.Render(val))}
}

// footer is the field's help and the keys for the mode the form is in.
func (f *jobForm) footer(cw int) []string {
	if f.confirmQuit {
		n := f.changed()
		word := "changes"
		if n == 1 {
			word = "change"
		}
		return []string{
			lipgloss.NewStyle().Foreground(statusWarning).Bold(true).
				Render(truncateToWidth(fmt.Sprintf("▲ Discard %d unsaved %s?", n, word), cw)),
			renderHints(cw, "", []helpHint{
				{key: "y", long: "discard", short: "discard", drop: 2},
				{key: "s", long: "save", short: "save", drop: 3},
				{key: "n", long: "keep editing", short: "keep", drop: 1, global: true},
			}),
		}
	}
	fl := f.fields[f.cursor]
	help := fl.help
	if fl.key == fPermission {
		_, help = permissionHint(f.field(fProvider).value())
	}
	var hints []helpHint
	switch {
	case f.editing:
		hints = []helpHint{
			{key: "enter", long: "done", short: "done", drop: 3},
			{key: "tab", long: "next", short: "next", drop: 5},
			{key: "esc", long: "undo", short: "undo", drop: 4},
		}
		if fl.kind == fieldMulti {
			hints = append(hints, helpHint{key: "alt+enter", long: "new line", short: "line", drop: 6})
		}
		hints = append(hints, helpHint{key: "ctrl+s", long: "save", short: "save", drop: 1, global: true})
	default:
		hints = []helpHint{{key: "↑↓", long: "field", short: "field", drop: 5}}
		if fl.kind == fieldChoice || fl.kind == fieldToggle {
			hints = append(hints, helpHint{key: "←→", long: "choose", short: "pick", drop: 3})
		} else {
			hints = append(hints, helpHint{key: "enter", long: "edit", short: "edit", drop: 3})
		}
		hints = append(hints,
			helpHint{key: "s", long: "save", short: "save", drop: 1, global: true},
			helpHint{key: "q", long: "quit", short: "quit", drop: 0, global: true},
		)
	}
	return append(helpLines(help, cw), renderHints(cw, "", hints))
}

// helpLines wraps a field's help to at most two lines, ending in an ellipsis
// only when even two aren't enough.
func helpLines(help string, cw int) []string {
	lines := strings.Split(cellbuf.Wrap(help, cw, ""), "\n")
	if len(lines) > 2 {
		lines = lines[:2]
		lines[1] = truncateToWidth(lines[1], cw-1) + "…"
	}
	for i, l := range lines {
		lines[i] = helpStyle.Render(truncateToWidth(strings.TrimRight(l, " "), cw))
	}
	return lines
}
