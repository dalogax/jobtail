package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dalogax/jobtail/internal/cronx"
	"github.com/dalogax/jobtail/internal/execengine"
	"github.com/dalogax/jobtail/internal/runner"
	"github.com/dalogax/jobtail/internal/store"
)

// validProviders are the recognized values for --provider (kind=agent
// only). "" is also valid and means execengine.ProviderClaude.
var validProviders = map[string]bool{
	"":                          true,
	execengine.ProviderClaude:   true,
	execengine.ProviderOpenCode: true,
	execengine.ProviderCodex:    true,
}

// providerLabel shows the effective provider for a job whose Provider
// field may be "" (meaning claude, execengine.effectiveProvider's default).
func providerLabel(provider string) string {
	if provider == "" {
		return execengine.ProviderClaude + " (default)"
	}
	return provider
}

func validateProvider(kind, provider string) error {
	if !validProviders[provider] {
		return fmt.Errorf("--provider must be one of %q, %q, %q (or omitted for claude), got %q",
			execengine.ProviderClaude, execengine.ProviderOpenCode, execengine.ProviderCodex, provider)
	}
	if kind != "agent" && provider != "" {
		return fmt.Errorf("--provider only applies to --kind agent")
	}
	return nil
}

// runDuration is how long a run took, or "-" if it's still running (no
// finished_at yet) — the plain-text `show`/`runs` output only ever showed
// started_at, leaving duration visible nowhere outside --json or the TUI's
// own "Dur" column.
func runDuration(r store.Run) string {
	if r.DurationMs.Valid {
		return time.Duration(r.DurationMs.Int64 * int64(time.Millisecond)).Round(time.Millisecond).String()
	}
	// Pre-migration rows have no duration_ms; fall back to the old (less
	// accurate for scheduled runs — see additiveMigrations) calculation
	// rather than showing nothing for historical data.
	if !r.FinishedAt.Valid {
		return "-"
	}
	return r.FinishedAt.Time.Sub(r.StartedAt).Round(time.Second).String()
}

// resolveCwd turns whatever --cwd the caller typed (relative or absolute)
// into an absolute path, resolved against the CLI's own working directory
// at the moment of the call, and confirms it actually exists.
//
// This has to happen at add/edit time, not execution time: a job's
// run-exec can be invoked from very different working directories
// depending on the trigger — an interactive `jobtail run` from whatever
// directory the user happened to be in, the systemd timer's `tick`
// (whatever cwd systemd gives a user unit, not necessarily $HOME), or the
// TUI's "run now". A relative --cwd would resolve differently in each of
// those, silently pointing at the wrong directory or failing outright.
func resolveCwd(cwd string) (string, error) {
	if cwd == "" {
		return "", fmt.Errorf("--cwd is required")
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("--cwd: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("--cwd %q: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--cwd %q is not a directory", abs)
	}
	return abs, nil
}

func newAddCmd() *cobra.Command {
	var kind, cronExpr, cwd, command, prompt, model, provider, permMode, timezone, precheck, notify string
	var maxConcurrent, keep int
	var timeoutSeconds, precheckTimeoutSeconds int64

	cmd := &cobra.Command{
		Use:   "add <id>",
		Short: "Register a new scheduled job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if kind != "cli" && kind != "agent" {
				return fmt.Errorf("--kind must be \"cli\" or \"agent\", got %q", kind)
			}
			if cronExpr == "" {
				return fmt.Errorf("--cron is required")
			}
			if err := cronx.Validate(cronExpr); err != nil {
				return fmt.Errorf("invalid --cron: %w", err)
			}
			resolvedCwd, err := resolveCwd(cwd)
			if err != nil {
				return err
			}
			cwd = resolvedCwd
			if kind == "cli" && command == "" {
				return fmt.Errorf("--cmd is required for --kind cli")
			}
			if kind == "agent" && prompt == "" {
				return fmt.Errorf("--prompt is required for --kind agent")
			}
			if err := validateProvider(kind, provider); err != nil {
				return err
			}
			if timezone == "" {
				timezone = "local"
			}
			if maxConcurrent <= 0 {
				maxConcurrent = 1
			}
			if timeoutSeconds < 0 {
				return fmt.Errorf("--timeout-seconds can't be negative")
			}
			notify, err = runner.ParseNotify(notify)
			if err != nil {
				return err
			}
			if keep <= 0 {
				keep = 200
			}

			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.st.Close()

			j := store.Job{
				ID: id, Kind: kind, Cron: cronExpr, Timezone: timezone, Enabled: true,
				Cwd: cwd, Command: command, Prompt: prompt, Model: model, Provider: provider, PermissionMode: permMode,
				MaxConcurrent: maxConcurrent, TimeoutSeconds: timeoutSeconds, Keep: keep,
				Precheck: precheck, PrecheckTimeoutSeconds: precheckTimeoutSeconds, Notify: notify,
			}
			if err := a.st.CreateJob(context.Background(), j); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added job %q (%s)\n", id, kind)
			return nil
		},
	}

	cmd.Flags().StringVar(&kind, "kind", "", `job kind: "cli" or "agent"`)
	cmd.Flags().StringVar(&cronExpr, "cron", "", "5-field cron expression")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory the job runs in")
	cmd.Flags().StringVar(&command, "cmd", "", "shell command (kind=cli)")
	cmd.Flags().StringVar(&prompt, "prompt", "", "agent task prompt (kind=agent)")
	cmd.Flags().StringVar(&model, "model", "", "model alias, e.g. sonnet (kind=agent)")
	cmd.Flags().StringVar(&provider, "provider", "", `agent CLI: "claude" (default), "opencode", or "codex" (kind=agent)`)
	cmd.Flags().StringVar(&permMode, "permission-mode", "", "permission mode: claude values are acceptEdits (default)/bypassPermissions/plan; "+
		"codex values are a --sandbox policy, read-only/workspace-write (default)/danger-full-access; unused by opencode (kind=agent)")
	cmd.Flags().StringVar(&timezone, "timezone", "local", `cron timezone: "local" or an IANA name`)
	cmd.Flags().IntVar(&maxConcurrent, "max-concurrent", 1, "max simultaneous runs of this job")
	cmd.Flags().Int64Var(&timeoutSeconds, "timeout-seconds", 0, "kill a run after N seconds and record it as timeout (0 = the default, 1800 = 30m)")
	cmd.Flags().IntVar(&keep, "keep", 200, "how many past runs to retain")
	cmd.Flags().StringVar(&precheck, "precheck", "", "shell gate run before execution: exit 0 runs the job (stdout becomes PENDING ITEMS context for agent prompts), exit 1 marks the run skipped, exit >=2 fails it (agent jobs mainly)")
	cmd.Flags().Int64Var(&precheckTimeoutSeconds, "precheck-timeout-seconds", 0, "hard kill the precheck after N seconds (0 = no timeout)")
	cmd.Flags().StringVar(&notify, "notify", "", notifyHelp)
	return cmd
}

// notifyHelp documents --notify for both add and edit.
var notifyHelp = "run events that raise a Herdr notification, comma-separated: " +
	strings.Join(runner.AllEvents, ", ") + " (started = the job began, after its precheck passed); " +
	"or all, none, default (" + strings.Join(runner.DefaultEvents, ",") + ")"

// timeoutLabel is a job's effective timeout for display, marking the default.
func timeoutLabel(j store.Job) string {
	if j.TimeoutSeconds <= 0 {
		return j.Timeout().String() + " (default)"
	}
	return j.Timeout().String()
}

func newListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.st.Close()

			jobs, err := a.st.ListJobs(context.Background())
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(jobs)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tENABLED\tRUNS\tNEXT\tLAST STATUS")
			for _, j := range jobs {
				next := "-"
				if j.Enabled {
					lastFire := j.CreatedAt
					if j.LastRunAt.Valid {
						lastFire = j.LastRunAt.Time
					}
					if n, err := cronx.Next(j.Cron, j.Timezone, lastFire); err == nil {
						next = n.Local().Format("2006-01-02 15:04")
					}
				}
				status := j.LastStatus
				if status == "" {
					status = "never run"
				}
				fmt.Fprintf(tw, "%s\t%s\t%v\t%d\t%s\t%s\n", j.ID, j.Kind, j.Enabled, j.RunCount, next, status)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func newShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one job's detail and recent runs",
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
			runs, err := a.st.ListRuns(ctx, args[0], 5)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"job": j, "recent_runs": runs})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "id:              %s\n", j.ID)
			fmt.Fprintf(out, "kind:            %s\n", j.Kind)
			fmt.Fprintf(out, "cron:            %s (%s)\n", j.Cron, j.Timezone)
			fmt.Fprintf(out, "enabled:         %v\n", j.Enabled)
			fmt.Fprintf(out, "cwd:             %s\n", j.Cwd)
			if j.Kind == "cli" {
				fmt.Fprintf(out, "cmd:             %s\n", j.Command)
			} else {
				fmt.Fprintf(out, "prompt:          %s\n", j.Prompt)
				fmt.Fprintf(out, "provider:        %s\n", providerLabel(j.Provider))
				fmt.Fprintf(out, "model:           %s\n", j.Model)
				fmt.Fprintf(out, "permission-mode: %s\n", j.PermissionMode)
			}
			if j.Precheck != "" {
				fmt.Fprintf(out, "precheck:        %s\n", j.Precheck)
				fmt.Fprintf(out, "precheck-timeout: %ds\n", j.PrecheckTimeoutSeconds)
			}
			fmt.Fprintf(out, "max-concurrent:  %d\n", j.MaxConcurrent)
			fmt.Fprintf(out, "timeout:         %s\n", timeoutLabel(j))
			fmt.Fprintf(out, "notify:          %s\n", runner.NotifyLabel(j.Notify))
			fmt.Fprintf(out, "keep:            %d\n", j.Keep)
			fmt.Fprintln(out, "recent runs:")
			for _, r := range runs {
				fmt.Fprintf(out, "  %s  %-8s %-16s %s  %s\n", r.ID, r.Status, r.Trigger, r.StartedAt.Local().Format(time.RFC3339), runDuration(r))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func newRunsCmd() *cobra.Command {
	var asJSON bool
	var limit int
	cmd := &cobra.Command{
		Use:   "runs <id>",
		Short: "List run history for one job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.st.Close()

			runs, err := a.st.ListRuns(context.Background(), args[0], limit)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(runs)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "RUN ID\tSTATUS\tTRIGGER\tSTARTED\tDURATION\tEXIT")
			for _, r := range runs {
				exit := "-"
				if r.ExitCode.Valid {
					exit = fmt.Sprint(r.ExitCode.Int64)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Status, r.Trigger, r.StartedAt.Local().Format(time.RFC3339), runDuration(r), exit)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.Flags().IntVar(&limit, "limit", 200, "max runs to show")
	return cmd
}

func newLogCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "log <run-id>",
		Short: "Dump one run's log to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.st.Close()

			r, err := a.st.GetRun(context.Background(), args[0])
			if err != nil {
				return err
			}
			data, err := os.ReadFile(r.LogPath)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
}

func newEnableCmd(enable bool) *cobra.Command {
	use, short := "enable <id>", "Enable a job"
	if !enable {
		use, short = "disable <id>", "Disable a job (pause without deleting)"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.st.Close()
			return a.st.SetEnabled(context.Background(), args[0], enable)
		},
	}
}

func newEditCmd() *cobra.Command {
	var cronExpr, cwd, command, prompt, model, provider, permMode, timezone, precheck, notify string
	var maxConcurrent, keep int
	var timeoutSeconds, precheckTimeoutSeconds int64
	cmd := &cobra.Command{
		Use:   "edit <id>",
		Short: "Change one or more fields of an existing job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cronExpr != "" {
				if err := cronx.Validate(cronExpr); err != nil {
					return fmt.Errorf("invalid --cron: %w", err)
				}
			}
			if cmd.Flags().Changed("cwd") {
				resolved, err := resolveCwd(cwd)
				if err != nil {
					return err
				}
				cwd = resolved
			}
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.st.Close()

			if cmd.Flags().Changed("provider") {
				existing, err := a.st.GetJob(context.Background(), args[0])
				if err != nil {
					return err
				}
				if err := validateProvider(existing.Kind, provider); err != nil {
					return err
				}
			}

			p := store.JobPatch{}
			setStr(&p.Cron, cmd, "cron", cronExpr)
			setStr(&p.Timezone, cmd, "timezone", timezone)
			setStr(&p.Cwd, cmd, "cwd", cwd)
			setStr(&p.Command, cmd, "cmd", command)
			setStr(&p.Prompt, cmd, "prompt", prompt)
			setStr(&p.Model, cmd, "model", model)
			setStr(&p.Provider, cmd, "provider", provider)
			setStr(&p.PermissionMode, cmd, "permission-mode", permMode)
			if cmd.Flags().Changed("max-concurrent") {
				p.MaxConcurrent = &maxConcurrent
			}
			if cmd.Flags().Changed("timeout-seconds") {
				if timeoutSeconds < 0 {
					return fmt.Errorf("--timeout-seconds can't be negative")
				}
				p.TimeoutSeconds = &timeoutSeconds
			}
			if cmd.Flags().Changed("keep") {
				p.Keep = &keep
			}
			setStr(&p.Precheck, cmd, "precheck", precheck)
			if cmd.Flags().Changed("precheck-timeout-seconds") {
				p.PrecheckTimeoutSeconds = &precheckTimeoutSeconds
			}
			if cmd.Flags().Changed("notify") {
				n, err := runner.ParseNotify(notify)
				if err != nil {
					return err
				}
				p.Notify = &n
			}
			return a.st.EditJob(context.Background(), args[0], p)
		},
	}
	cmd.Flags().StringVar(&cronExpr, "cron", "", "5-field cron expression")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory")
	cmd.Flags().StringVar(&command, "cmd", "", "shell command (kind=cli)")
	cmd.Flags().StringVar(&prompt, "prompt", "", "agent task prompt (kind=agent)")
	cmd.Flags().StringVar(&model, "model", "", "model alias (kind=agent)")
	cmd.Flags().StringVar(&provider, "provider", "", `agent CLI: "claude", "opencode", or "codex" (kind=agent)`)
	cmd.Flags().StringVar(&permMode, "permission-mode", "", "permission mode (kind=agent; meaning is provider-specific, see `add --help`)")
	cmd.Flags().StringVar(&timezone, "timezone", "", `cron timezone`)
	cmd.Flags().IntVar(&maxConcurrent, "max-concurrent", 0, "max simultaneous runs")
	cmd.Flags().Int64Var(&timeoutSeconds, "timeout-seconds", 0, "kill a run after N seconds (0 = the default, 1800 = 30m)")
	cmd.Flags().IntVar(&keep, "keep", 0, "how many past runs to retain")
	cmd.Flags().StringVar(&precheck, "precheck", "", "shell gate run before execution (empty string clears it)")
	cmd.Flags().Int64Var(&precheckTimeoutSeconds, "precheck-timeout-seconds", 0, "hard kill the precheck after N seconds")
	cmd.Flags().StringVar(&notify, "notify", "", notifyHelp)
	return cmd
}

func setStr(dst **string, cmd *cobra.Command, flag, val string) {
	if cmd.Flags().Changed(flag) {
		*dst = &val
	}
}

func newRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Delete a job and its run history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.st.Close()
			logPaths, err := a.st.DeleteJob(context.Background(), args[0])
			if err != nil {
				return err
			}
			for _, p := range logPaths {
				_ = os.Remove(p) // best-effort: a missing/already-gone file isn't an error here
			}
			return nil
		},
	}
}
