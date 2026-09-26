// Command jobtail schedules and watches recurring jobs — plain shell
// commands or headless Claude Code agent turns — for a Herdr-based dev box.
// See PRD.md at the repo root for the full design.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/dalogax/jobtail/internal/selfupdate"
	"github.com/dalogax/jobtail/internal/store"
)

// version is set at release-build time via -ldflags "-X main.version=vX.Y.Z"
// (see .github/workflows/release.yml). A plain `go build` leaves it "dev",
// which selfupdate.CheckForUpdate and `jobtail upgrade` both treat as
// "nothing to compare against, always safe to install latest."
var version = "dev"

// currentVersion lets the e2e suite simulate "this build is vX.Y.Z" without
// needing a real ldflags build per test case.
func currentVersion() string {
	if v := os.Getenv("JOBTAIL_VERSION_OVERRIDE"); v != "" {
		return v
	}
	return version
}

// noUpdateCheckCommands are commands that must never print an update
// suggestion — it's always written to stderr (so it can never corrupt a
// command's --json stdout), but it would still be noise in a systemd
// journal (tick, run-exec), disrupt the TUI's alt-screen (tui), or step on
// output a shell eval's directly (completion) — and `upgrade` already
// reports version status itself.
var noUpdateCheckCommands = map[string]bool{
	"tick": true, "run-exec": true, "tui": true, "upgrade": true, "completion": true,
}

// app bundles everything a subcommand needs: the open store and resolved
// data-dir paths (PRD §12 decision 4: no config file, just JOBTAIL_DATA_DIR).
type app struct {
	st      *store.Store
	dataDir string
	logsDir string
}

func dataDir() string {
	if d := os.Getenv("JOBTAIL_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "share", "jobtail")
}

func openApp() (*app, error) {
	dd := dataDir()
	if err := os.MkdirAll(dd, 0o700); err != nil {
		return nil, err
	}
	logsDir := filepath.Join(dd, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(dd, "jobtail.db"))
	if err != nil {
		return nil, err
	}
	return &app{st: st, dataDir: dd, logsDir: logsDir}, nil
}

func (a *app) logPath(runID string) string {
	return filepath.Join(a.logsDir, runID+".log")
}

func main() {
	root := &cobra.Command{
		Use:           "jobtail",
		Short:         "Schedule and watch recurring cli/agent jobs for Herdr",
		Version:       currentVersion(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("jobtail {{.Version}}\n")

	root.AddCommand(
		newAddCmd(),
		newListCmd(),
		newShowCmd(),
		newRunsCmd(),
		newLogCmd(),
		newEnableCmd(true),
		newEnableCmd(false),
		newEditCmd(),
		newRmCmd(),
		newRunCmd(),
		newResumeCmd(),
		newTickCmd(),
		newRunExecCmd(),
		newGCCmd(),
		newTUICmd(),
		newInstallSystemdCmd(),
		newUpgradeCmd(),
	)

	ran, err := root.ExecuteC()
	if err != nil {
		fmt.Fprintln(os.Stderr, "jobtail:", err)
		maybeSuggestUpdate(ran)
		os.Exit(1)
	}
	maybeSuggestUpdate(ran)
}

func maybeSuggestUpdate(ran *cobra.Command) {
	if ran == nil || noUpdateCheckCommands[ran.Name()] {
		return
	}
	dd := dataDir()
	if err := os.MkdirAll(dd, 0o700); err != nil {
		return
	}
	if msg := selfupdate.CheckForUpdate(context.Background(), dd, currentVersion()); msg != "" {
		fmt.Fprintln(os.Stderr, "jobtail:", msg)
	}
}
