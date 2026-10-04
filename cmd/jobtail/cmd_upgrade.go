package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/dalogax/jobtail/internal/selfupdate"
)

func newUpgradeCmd() *cobra.Command {
	var checkOnly, force bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Check for and install a newer jobtail release",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			cur := currentVersion()

			rel, err := selfupdate.LatestRelease(ctx)
			if err != nil {
				return fmt.Errorf("check latest release: %w", err)
			}

			upToDate := semver.IsValid(cur) && semver.Compare(rel.TagName, cur) <= 0
			if upToDate && !force {
				fmt.Fprintf(cmd.OutOrStdout(), "jobtail %s is up to date\n", cur)
				return nil
			}

			execPath, err := executablePath()
			if err != nil {
				return err
			}
			manager, managed := selfupdate.ManagedBy(execPath)

			if checkOnly {
				fmt.Fprintf(cmd.OutOrStdout(), "a newer jobtail is available: %s -> %s\n", displayVersion(cur), rel.TagName)
				if managed {
					fmt.Fprintf(cmd.OutOrStdout(), "upgrade it with %s\n", manager)
				}
				return nil
			}
			if managed {
				return fmt.Errorf("%s belongs to a package manager, not to jobtail; upgrade it with %s", execPath, manager)
			}

			asset, ok := selfupdate.CurrentPlatformAsset(rel)
			if !ok {
				return fmt.Errorf("release %s has no asset for %s/%s (expected %s)",
					rel.TagName, runtime.GOOS, runtime.GOARCH, selfupdate.AssetName(runtime.GOOS, runtime.GOARCH))
			}

			fmt.Fprintf(cmd.OutOrStdout(), "installing jobtail %s (%s)...\n", rel.TagName, asset.Name)
			if err := selfupdate.Install(ctx, asset, execPath); err != nil {
				return fmt.Errorf("install: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "upgraded %s -> %s\n", displayVersion(cur), rel.TagName)

			// The skill is embedded in the binary, so installed copies are
			// now one version behind. Only the new binary has the new copy,
			// so it does the refresh. Failing here doesn't undo the upgrade.
			refresh := exec.Command(execPath, "install-skill", "--only-installed")
			refresh.Stdout, refresh.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
			if err := refresh.Run(); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "jobtail: could not refresh the agent skill (%v); run: jobtail install-skill\n", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "only report whether a newer release exists; don't install it")
	cmd.Flags().BoolVar(&force, "force", false, "reinstall even if already on the latest version")
	return cmd
}

func displayVersion(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

// executablePath is the running binary with symlinks resolved, so a
// ~/.local/bin link into a package manager's tree is seen for what it is.
func executablePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p, nil
}
