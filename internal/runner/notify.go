package runner

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Run events a job can be notified about (store.Job.Notify). Each matches
// the run status of the same name, except EventStarted, which fires when
// the job itself begins executing — after its precheck passed, for a job
// that has one, so it means "the gate found work and the agent is on it".
const (
	EventStarted        = "started"
	EventOK             = "ok"
	EventFailed         = "failed"
	EventTimeout        = "timeout"
	EventSkipped        = "skipped"
	EventSkippedOverlap = "skipped_overlap"
)

// AllEvents is every event, in the order they are listed back to the user.
var AllEvents = []string{EventStarted, EventOK, EventFailed, EventTimeout, EventSkipped, EventSkippedOverlap}

// DefaultEvents is what a job with no notify setting gets: what jobtail
// always notified about before this was configurable.
var DefaultEvents = []string{EventFailed, EventTimeout}

// ParseNotify validates a --notify value and returns what to store:
// "" for the default ("" or "default"), "none", or a comma-separated list
// of events in AllEvents order ("all" expands to every event).
func ParseNotify(v string) (string, error) {
	v = strings.TrimSpace(v)
	switch v {
	case "", "default":
		return "", nil
	case "none":
		return "none", nil
	case "all":
		return strings.Join(AllEvents, ","), nil
	}
	want := map[string]bool{}
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !isEvent(e) {
			return "", fmt.Errorf("--notify: unknown event %q (want a comma-separated list of %s, or all, none, default)",
				e, strings.Join(AllEvents, ", "))
		}
		want[e] = true
	}
	var out []string
	for _, e := range AllEvents {
		if want[e] {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return "", fmt.Errorf("--notify: no events given (use none to turn notifications off)")
	}
	return strings.Join(out, ","), nil
}

func isEvent(e string) bool {
	for _, a := range AllEvents {
		if a == e {
			return true
		}
	}
	return false
}

// NotifyEvents returns the events a stored notify setting subscribes to.
func NotifyEvents(setting string) []string {
	switch setting {
	case "", "default":
		return DefaultEvents
	case "none":
		return nil
	case "all":
		return AllEvents
	}
	return strings.Split(setting, ",")
}

// NotifyLabel is a notify setting for display.
func NotifyLabel(setting string) string {
	switch setting {
	case "":
		return strings.Join(DefaultEvents, ",") + " (default)"
	case "none":
		return "none"
	}
	return setting
}

func wants(setting, event string) bool {
	for _, e := range NotifyEvents(setting) {
		if e == event {
			return true
		}
	}
	return false
}

// Notify raises a Herdr notification for event if the job's notify setting
// includes it. Best-effort: silently skipped if herdr isn't on PATH, or if
// JOBTAIL_DISABLE_NOTIFY is set — used by the e2e suite so intentionally
// failing test jobs don't spam real Herdr toasts on the dev box.
func Notify(jobID, setting, event, body string) {
	if !wants(setting, event) || os.Getenv("JOBTAIL_DISABLE_NOTIFY") != "" {
		return
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		return
	}
	args := []string{"notification", "show", fmt.Sprintf("jobtail: %s %s", jobID, event), "--sound", eventSound(event)}
	if body != "" {
		args = append(args, "--body", body)
	}
	_ = exec.Command("herdr", args...).Run()
}

// eventSound keeps the attention-grabbing sound for what needs attention.
func eventSound(event string) string {
	switch event {
	case EventFailed, EventTimeout:
		return "request"
	case EventOK:
		return "done"
	default:
		return "none"
	}
}
