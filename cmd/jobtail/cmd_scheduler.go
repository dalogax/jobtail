package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

func newInstallSchedulerCmd() *cobra.Command {
	var enable bool
	cmd := &cobra.Command{
		Use: "install-scheduler",
		// The command was install-systemd until macOS support needed a
		// second backend. Keeping the old name as an alias means existing
		// docs, notes and muscle memory keep working.
		Aliases: []string{"install-systemd"},
		Short:   "Install the per-user timer that runs due jobs once a minute",
		Long: "Install the per-user timer that runs due jobs once a minute.\n\n" +
			"jobtail has no daemon of its own: something outside it runs `jobtail tick`\n" +
			"every minute and tick starts whatever is due. This command writes that\n" +
			"timer for the platform you're on — systemd --user units on Linux, a\n" +
			"LaunchAgent on macOS.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			if cmd.CalledAs() == "install-systemd" {
				fmt.Fprintln(errOut, "jobtail: note: install-systemd is now install-scheduler "+
					"(it installs whatever this platform schedules with)")
			}

			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.Abs(exe); err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}

			// The launchd agent redirects tick's output into the data
			// directory, and launchd refuses to start a job whose
			// StandardOutPath it cannot open — so on a fresh Mac, where
			// nothing has run yet, the directory has to exist before the
			// agent is loaded rather than when the first command happens
			// to create it.
			if err := os.MkdirAll(dataDir(), 0o700); err != nil {
				return err
			}

			s, err := schedulerFor(runtime.GOOS, schedulerEnv{
				home:       home,
				exe:        exe,
				dataDir:    dataDir(),
				uid:        os.Getuid(),
				path:       schedulerPath(os.Getenv("PATH"), home),
				dataDirEnv: os.Getenv("JOBTAIL_DATA_DIR"),
			})
			if err != nil {
				return err
			}

			for _, f := range s.files {
				if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(f.path, []byte(f.content), 0o644); err != nil {
					return err
				}
				fmt.Fprintf(out, "wrote %s\n", f.path)
			}

			if !enable {
				fmt.Fprintf(out, "run again with --enable, or by hand:\n")
				for _, line := range s.manual {
					fmt.Fprintf(out, "  %s\n", line)
				}
				return nil
			}

			for _, step := range s.enable {
				combined, err := exec.Command(step.argv[0], step.argv[1:]...).CombinedOutput()
				if err != nil && !step.ignoreFail {
					failed := fmt.Errorf("%s: %w: %s", strings.Join(step.argv, " "), err,
						strings.TrimSpace(string(combined)))
					if s.enableHint != "" {
						return fmt.Errorf("%w\n  %s", failed, s.enableHint)
					}
					return failed
				}
			}
			fmt.Fprintf(out, "enabled %s via %s\n", s.unit, s.name)
			fmt.Fprintf(out, "check it with: %s\n", s.status)
			return nil
		},
	}
	cmd.Flags().BoolVar(&enable, "enable", false,
		"also load and start the timer, not just write its files")
	return cmd
}
