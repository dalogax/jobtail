// Package resume hands a finished agent run back to the agent CLI that
// produced it, as an interactive session in a new Herdr tab (PRD §14).
//
// It lives in its own package because two callers need it and must not
// drift: `jobtail resume <run-id>` on the command line, and the dashboard's
// "r" key, which is where you actually are when you decide a run is worth
// picking up by hand.
package resume

import (
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/dalogax/jobtail/internal/execengine"
	"github.com/dalogax/jobtail/internal/store"
)

// Command builds the interactive shell command that hands a captured
// session/thread id back to the agent CLI that produced it. Each provider's
// interactive resume syntax is verified against that binary's own --help:
//   - claude:   `claude --resume <session>`
//   - opencode: `opencode --session <session>` (opens the interactive TUI
//     on that session — its `run --session` counterpart is non-interactive)
//   - codex:    `codex resume <session>` (the top-level, interactive
//     `resume` command — distinct from `codex exec resume`, which is also
//     non-interactive)
func Command(provider, sessionID string) string {
	switch execengine.EffectiveProvider(provider) {
	case execengine.ProviderOpenCode:
		return fmt.Sprintf("%s --session %s", execengine.OpenCodeBin(), sessionID)
	case execengine.ProviderCodex:
		return fmt.Sprintf("%s resume %s", execengine.CodexBin(), sessionID)
	default:
		return fmt.Sprintf("%s --resume %s", execengine.ClaudeBin(), sessionID)
	}
}

// Possible reports whether a run is even a candidate for resuming: agent
// runs only, and only once a session id was captured. It's a pure check on
// data already in hand — no subprocesses, no PATH lookups — so the dashboard
// can call it while deciding whether to advertise the key at all, rather
// than offering a key that then explains itself with an error.
//
// It deliberately says nothing about whether the session is still on disk.
// Nothing in the database can know that: the id is recorded while the agent
// is running, and the transcript it names could be pruned by the provider
// afterwards. Open surfaces that as the agent CLI's own message.
func Possible(j store.Job, r store.Run) bool {
	return j.Kind == "agent" && r.SessionID.Valid && r.SessionID.String != ""
}

// Reason explains why a run can't be resumed, for a caller that needs to
// tell someone. It returns "" when Possible would return true.
//
// The wording is load-bearing — the e2e suite matches on "not agent" and
// "no captured session id" to check the refusals stay legible — so keep
// those phrases if this is ever reworded.
func Reason(j store.Job, r store.Run) string {
	switch {
	case j.Kind != "agent":
		return fmt.Sprintf("job %q is kind=%s, not agent", j.ID, j.Kind)
	case !r.SessionID.Valid || r.SessionID.String == "":
		return "no captured session id (the job may have failed before the agent CLI started)"
	default:
		return ""
	}
}

// Open starts an interactive session on this run's captured session id, in
// a new Herdr tab rooted at the job's working directory, and returns the id
// of the pane it started in.
//
// The agent CLI is launched *inside* the new pane rather than as a child of
// jobtail: an interactive session needs a terminal of its own, and the
// dashboard is already using this one.
func Open(j store.Job, r store.Run) (paneID string, err error) {
	if why := Reason(j, r); why != "" {
		return "", fmt.Errorf("run %s: %s", r.ID, why)
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		return "", fmt.Errorf("herdr not found on PATH: %w", err)
	}
	// No --json: herdr's socket-API commands already answer in JSON, and
	// 0.9.0 rejects the flag outright ("unknown option: --json", exit 2), so
	// passing it failed every resume before the agent CLI was ever reached.
	// Asking for the default is the portable option — an older herdr that
	// wanted --json would still have printed JSON without it.
	out, err := exec.Command("herdr", "tab", "create", "--cwd", j.Cwd).Output()
	if err != nil {
		return "", fmt.Errorf("herdr tab create: %w", err)
	}
	paneID, err = extractPaneID(out)
	if err != nil {
		return "", err
	}
	if err := exec.Command("herdr", "pane", "run", paneID, Command(j.Provider, r.SessionID.String)).Run(); err != nil {
		return "", fmt.Errorf("herdr pane run: %w", err)
	}
	return paneID, nil
}

// extractPaneID pulls the new pane's ID out of `herdr tab create --json`'s
// response shape: {"result": {"root_pane": {"pane_id": "..."}}} (or, on
// older/newer Herdr builds, {"result": {"pane": {"pane_id": "..."}}}).
func extractPaneID(rawJSON []byte) (string, error) {
	var resp struct {
		Result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
			Pane struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rawJSON, &resp); err != nil {
		return "", fmt.Errorf("parse herdr tab create output: %w", err)
	}
	if resp.Result.RootPane.PaneID != "" {
		return resp.Result.RootPane.PaneID, nil
	}
	if resp.Result.Pane.PaneID != "" {
		return resp.Result.Pane.PaneID, nil
	}
	return "", fmt.Errorf("herdr tab create output had no pane id: %s", rawJSON)
}
