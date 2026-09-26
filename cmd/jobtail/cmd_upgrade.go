package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/jarvis0064/jobtail/internal/selfupdate"
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

			if checkOnly {
				fmt.Fprintf(cmd.OutOrStdout(), "a newer jobtail is available: %s -> %s\n", displayVersion(cur), rel.TagName)
				return nil
			}

			asset, ok := selfupdate.CurrentPlatformAsset(rel)
			if !ok {
				return fmt.Errorf("release %s has no asset for %s/%s (expected %s)",
					rel.TagName, runtime.GOOS, runtime.GOARCH, selfupdate.AssetName(runtime.GOOS, runtime.GOARCH))
			}

			execPath, err := os.Executable()
			if err != nil {
				return err
			}
			if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
				execPath = resolved
			}

			fmt.Fprintf(cmd.OutOrStdout(), "installing jobtail %s (%s)...\n", rel.TagName, asset.Name)
			if err := selfupdate.Install(ctx, asset, execPath); err != nil {
				return fmt.Errorf("install: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "upgraded %s -> %s\n", displayVersion(cur), rel.TagName)
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
