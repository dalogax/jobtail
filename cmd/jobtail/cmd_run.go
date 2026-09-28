package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/dalogax/jobtail/internal/cronx"
	"github.com/dalogax/jobtail/internal/resume"
	"github.com/dalogax/jobtail/internal/runner"
	"github.com/dalogax/jobtail/internal/store"
)

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

			res, runErr := runner.Execute(ctx, a.st, j, runID, logPath)
			if err := runner.Finish(ctx, a.st, j, runID, res, runErr); err != nil {
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
		Short: "Reopen an agent run's session interactively (in a new Herdr tab, if herdr is on PATH)",
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
			paneID, err := resume.Open(j, r)
			if err != nil {
				return err
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
			res, runErr := runner.Execute(ctx, a.st, j, r.ID, r.LogPath)
			return runner.Finish(ctx, a.st, j, r.ID, res, runErr)
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
