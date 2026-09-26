package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dalogax/jobtail/internal/tui"
)

func newTUICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open the jobs -> runs -> log dashboard (run this inside a Herdr tab)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return openDashboard()
		},
	}
}

// openDashboard is shared by `jobtail tui` and bare `jobtail` (no
// subcommand) — both open the same dashboard through the same path.
func openDashboard() error {
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.st.Close()
	if err := tui.Run(a.st, a.logsDir); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
