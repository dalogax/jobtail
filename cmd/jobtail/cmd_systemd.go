package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

// serviceUnit and timerUnit are systemd --user units (PRD §12 decision 8):
// fixed 1-minute resolution, matching cron's own granularity, no configurable
// tick interval.
const serviceUnit = `[Unit]
Description=jobtail: check for due jobs

[Service]
Type=oneshot
ExecStart=%s tick
# tick spawns each due job's run-exec as a detached grandchild and returns
# immediately — run-exec is meant to keep running long after this oneshot
# unit itself exits. Without this, systemd's default KillMode
# (control-group) sends every remaining process in this unit's cgroup a
# kill signal the moment tick's own exit deactivates the unit, silently
# killing every just-spawned run-exec before it can do anything. Confirmed
# by an actual stuck job on a real box: KillMode=process, not just a
# careful goroutine/Release() dance in the Go code, is what's needed.
KillMode=process
`

const timerUnit = `[Unit]
Description=jobtail: run jobtail-tick.service every minute

[Timer]
OnCalendar=minutely
Persistent=true

[Install]
WantedBy=timers.target
`

func newInstallSystemdCmd() *cobra.Command {
	var enable bool
	cmd := &cobra.Command{
		Use:   "install-systemd",
		Short: "Write and (optionally) enable the jobtail-tick systemd --user timer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			exe, err = filepath.Abs(exe)
			if err != nil {
				return err
			}

			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			unitDir := filepath.Join(home, ".config", "systemd", "user")
			if err := os.MkdirAll(unitDir, 0o755); err != nil {
				return err
			}

			svcPath := filepath.Join(unitDir, "jobtail-tick.service")
			if err := os.WriteFile(svcPath, []byte(fmt.Sprintf(serviceUnit, exe)), 0o644); err != nil {
				return err
			}
			timerPath := filepath.Join(unitDir, "jobtail-tick.timer")
			if err := os.WriteFile(timerPath, []byte(timerUnit), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\nwrote %s\n", svcPath, timerPath)

			if !enable {
				fmt.Fprintln(cmd.OutOrStdout(), "run with --enable, or manually:\n  systemctl --user daemon-reload\n  systemctl --user enable --now jobtail-tick.timer")
				return nil
			}
			for _, args := range [][]string{
				{"--user", "daemon-reload"},
				{"--user", "enable", "--now", "jobtail-tick.timer"},
			} {
				if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
					return fmt.Errorf("systemctl %v: %w: %s", args, err, out)
				}
			}
			fmt.Fprintln(cmd.OutOrStdout(), "enabled jobtail-tick.timer")
			return nil
		},
	}
	cmd.Flags().BoolVar(&enable, "enable", false, "also run systemctl --user enable --now")
	return cmd
}
