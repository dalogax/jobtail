// Package tui is jobtail's Bubble Tea dashboard: jobs -> runs -> log,
// three panes, meant to run inside a Herdr tab (PRD §9).
package tui

import (
	"fmt"

	"github.com/jarvis0064/jobtail/internal/store"
)

// Run is filled in during Phase 3 of the build plan (PRD §11); Phases 1-2
// (CLI + execution engine) don't depend on it.
func Run(st *store.Store, logsDir string) error {
	return fmt.Errorf("not implemented yet (Phase 3)")
}
