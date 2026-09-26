package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/jarvis0064/jobtail/internal/cronx"
	"github.com/jarvis0064/jobtail/internal/execengine"
	"github.com/jarvis0064/jobtail/internal/store"
)

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

// executeRun dispatches by job kind and runs to completion in-process.
func executeRun(ctx context.Context, a *app, j store.Job, runID string, logPath string) (execengine.Result, error) {
	switch j.Kind {
	case "cli":
		return execengine.RunCLI(ctx, j, logPath)
	case "agent":
		return execengine.RunAgent(ctx, j, logPath, func(sessionID string) {
			_ = a.st.SetRunSessionID(ctx, runID, sessionID)
		})
	default:
		return execengine.Result{}, fmt.Errorf("unknown job kind %q", j.Kind)
	}
}

// finishRun records the outcome and, on failure, tries a Herdr desktop
// notification (best-effort: silently skipped if herdr isn't on PATH, e.g.
// under test or outside a Herdr session).
func finishRun(ctx context.Context, a *app, j store.Job, runID string, res execengine.Result, runErr error) error {
	status := res.Status
	if status == "" {
		status = "failed"
	}
	if err := a.st.FinishRun(ctx, runID, status, res.ExitCode, time.Now()); err != nil {
		return err
	}
	if status == "failed" || status == "timeout" {
		notifyFailure(j.ID, status)
	}
	return runErr
}

func notifyFailure(jobID, status string) {
	if os.Getenv("JOBTAIL_DISABLE_NOTIFY") != "" {
		return // set by the e2e suite so intentionally-failing test jobs don't spam real Herdr toasts
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		return
	}
	_ = exec.Command("herdr", "notification", "show",
		fmt.Sprintf("jobtail: %s %s", jobID, status), "--sound", "request").Run()
}

func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run <id>",
		Short: "Manually trigger a job now and wait for it to finish",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.st.Close()

			ctx := context.Background()
			j, err := a.st.GetJob(ctx, args[0])
			if err != nil {
				return err
			}

			runID := uuid.NewString()
			logPath := a.logPath(runID)
			run, err := a.st.StartRun(ctx, j.ID, runID, "manual", logPath, time.Now())
			if errors.Is(err, store.ErrOverlap) {
				fmt.Fprintf(cmd.OutOrStdout(), "skipped: %s already has a run in progress (run %s)\n", j.ID, run.ID)
				return nil
			}
			if err != nil {
				return err
			}

			res, runErr := executeRun(ctx, a, j, runID, logPath)
			if err := finishRun(ctx, a, j, runID, res, runErr); err != nil {
				fmt.Fprintln(cmd.OutOrStdout(), err)
			}

			if data, readErr := os.ReadFile(logPath); readErr == nil {
				cmd.OutOrStdout().Write(data)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nrun %s: %s (exit %d)\n", runID, res.Status, res.ExitCode)
			if res.Status != "ok" {
				return fmt.Errorf("run %s: %s", runID, res.Status)
			}
			return nil
		},
	}
}

func newResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <run-id>",
		Short: "Hand a failed agent run to an interactive Herdr tab via claude --resume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.st.Close()

			ctx := context.Background()
			r, err := a.st.GetRun(ctx, args[0])
			if err != nil {
				return err
			}
			j, err := a.st.GetJob(ctx, r.JobID)
			if err != nil {
				return err
			}
			if j.Kind != "agent" {
				return fmt.Errorf("run %s belongs to job %q, which is kind=%s, not agent", r.ID, j.ID, j.Kind)
			}
			if !r.SessionID.Valid || r.SessionID.String == "" {
				return fmt.Errorf("run %s has no captured session id (job may have failed before Claude Code started)", r.ID)
			}

			if _, err := exec.LookPath("herdr"); err != nil {
				return fmt.Errorf("herdr not found on PATH: %w", err)
			}
			out, err := exec.Command("herdr", "tab", "create", "--json", "--cwd", j.Cwd).Output()
			if err != nil {
				return fmt.Errorf("herdr tab create: %w", err)
			}
			paneID, err := extractPaneID(out)
			if err != nil {
				return err
			}
			claudeCmd := fmt.Sprintf("%s --resume %s", execengine.ClaudeBin(), r.SessionID.String)
			if err := exec.Command("herdr", "pane", "run", paneID, claudeCmd).Run(); err != nil {
				return fmt.Errorf("herdr pane run: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "resumed run %s (session %s) in pane %s\n", r.ID, r.SessionID.String, paneID)
			return nil
		},
	}
}

func newTickCmd() *cobra.Command {
	var nowOverride string
	cmd := &cobra.Command{
		Use:    "tick",
		Short:  "Check for due jobs and spawn their runs (invoked by the systemd timer)",
		Args:   cobra.NoArgs,
		Hidden: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			now := time.Now()
			if nowOverride != "" {
				t, err := time.Parse(time.RFC3339, nowOverride)
				if err != nil {
					return fmt.Errorf("--now: %w", err)
				}
				now = t
			}

			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.st.Close()

			ctx := context.Background()
			jobs, err := a.st.EnabledJobs(ctx)
			if err != nil {
				return err
			}

			exe, err := os.Executable()
			if err != nil {
				return err
			}

			for _, j := range jobs {
				lastFire := j.CreatedAt
				if last, err := a.st.LastRun(ctx, j.ID, "scheduled"); err == nil {
					lastFire = last.StartedAt
				} else if !errors.Is(err, store.ErrNotFound) {
					return err
				}

				due, at, err := cronx.Due(j.Cron, j.Timezone, lastFire, now)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "jobtail: job %s: %v\n", j.ID, err)
					continue
				}
				if !due {
					continue
				}

				runID := uuid.NewString()
				logPath := a.logPath(runID)
				run, err := a.st.StartRun(ctx, j.ID, runID, "scheduled", logPath, at)
				if err != nil && !errors.Is(err, store.ErrOverlap) {
					return err
				}
				if run.Status == "skipped_overlap" {
					fmt.Fprintf(cmd.OutOrStdout(), "skipped %s: already running\n", j.ID)
					continue
				}

				spawnCmd := exec.Command(exe, "run-exec", runID)
				spawnCmd.Stdout, spawnCmd.Stderr = nil, nil
				if err := spawnCmd.Start(); err != nil {
					return fmt.Errorf("spawn run-exec for %s: %w", j.ID, err)
				}
				_ = spawnCmd.Process.Release() // detach: tick must return quickly, run-exec outlives it
				fmt.Fprintf(cmd.OutOrStdout(), "fired %s -> run %s\n", j.ID, runID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&nowOverride, "now", "", "override the current time (RFC3339); mainly for testing")
	return cmd
}

func newRunExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "run-exec <run-id>",
		Short:  "Execute one already-created run to completion (internal; spawned by tick)",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.st.Close()

			ctx := context.Background()
			r, err := a.st.GetRun(ctx, args[0])
			if err != nil {
				return err
			}
			j, err := a.st.GetJob(ctx, r.JobID)
			if err != nil {
				return err
			}
			res, runErr := executeRun(ctx, a, j, r.ID, r.LogPath)
			return finishRun(ctx, a, j, r.ID, res, runErr)
		},
	}
}

func newGCCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "gc",
		Short: "Prune runs (and their log files) past each job's retention count",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.st.Close()

			ids, err := a.st.GC(context.Background())
			if err != nil {
				return err
			}
			for _, id := range ids {
				_ = os.Remove(a.logPath(id))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pruned %d run(s)\n", len(ids))
			return nil
		},
	}
}
