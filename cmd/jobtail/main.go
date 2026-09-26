// Command jobtail schedules and watches recurring jobs — plain shell
// commands or headless Claude Code agent turns — for a Herdr-based dev box.
// See PRD.md at the repo root for the full design.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jarvis0064/jobtail/internal/store"
)

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
		SilenceUsage:  true,
		SilenceErrors: true,
	}

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
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "jobtail:", err)
		os.Exit(1)
	}
}
