package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dalogax/jobtail/skills"
)

// skillAgent is one coding-agent CLI that can load the jobtail skill: where
// it looks for user-level skills, and how to tell it's on this machine.
type skillAgent struct {
	name string
	dir  string // the agent's user-level skills directory
	// present reports the agent as installed: its binary is on PATH, or
	// its config directory exists (a mise/asdf shim or an app-bundled CLI
	// isn't always on the PATH the installer runs with).
	present bool
}

// skillEnv is everything agent detection reads from the process, passed in
// so tests don't depend on the machine running them.
type skillEnv struct {
	home     string
	getenv   func(string) string
	lookPath func(string) (string, error)
}

func (e skillEnv) envOr(key, fallback string) string {
	if v := e.getenv(key); v != "" {
		return v
	}
	return fallback
}

// skillAgents lists the supported agents in a fixed order. The skill
// directories are the ones each CLI documents for user-level skills:
// claude reads $CLAUDE_CONFIG_DIR/skills, opencode reads
// $XDG_CONFIG_HOME/opencode/skills, codex reads $CODEX_HOME/skills.
func skillAgents(e skillEnv) []skillAgent {
	claudeHome := e.envOr("CLAUDE_CONFIG_DIR", filepath.Join(e.home, ".claude"))
	opencodeHome := filepath.Join(e.envOr("XDG_CONFIG_HOME", filepath.Join(e.home, ".config")), "opencode")
	codexHome := e.envOr("CODEX_HOME", filepath.Join(e.home, ".codex"))

	agent := func(name, home string) skillAgent {
		_, err := e.lookPath(name)
		return skillAgent{
			name:    name,
			dir:     filepath.Join(home, "skills", skills.Name),
			present: err == nil || isDir(home),
		}
	}
	return []skillAgent{
		agent("claude", claudeHome),
		agent("opencode", opencodeHome),
		agent("codex", codexHome),
	}
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// selectSkillAgents picks the agents to install for: the named ones, or
// every detected one when none are named. With onlyInstalled, it narrows
// that to agents that already have the skill (what `upgrade` refreshes).
func selectSkillAgents(all []skillAgent, names []string, onlyInstalled bool) ([]skillAgent, error) {
	var picked []skillAgent
	if len(names) == 0 {
		for _, a := range all {
			if a.present {
				picked = append(picked, a)
			}
		}
	} else {
		byName := map[string]skillAgent{}
		for _, a := range all {
			byName[a.name] = a
		}
		for _, n := range names {
			a, ok := byName[strings.ToLower(strings.TrimSpace(n))]
			if !ok {
				return nil, fmt.Errorf("unknown agent %q (supported: %s)", n, supportedAgentNames(all))
			}
			picked = append(picked, a)
		}
	}
	if onlyInstalled {
		kept := picked[:0]
		for _, a := range picked {
			if _, err := os.Lstat(filepath.Join(a.dir, "SKILL.md")); err == nil {
				kept = append(kept, a)
			}
		}
		picked = kept
	}
	return picked, nil
}

func supportedAgentNames(all []skillAgent) string {
	names := make([]string, len(all))
	for i, a := range all {
		names[i] = a.name
	}
	return strings.Join(names, ", ")
}

// installSkill writes the embedded skill into dir, replacing any previous
// copy's files. A dir that's a symlink is left alone: someone pointed it at
// their own checkout on purpose, and writing through it would edit that
// checkout instead.
func installSkill(dir string) (linked bool, err error) {
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return true, nil
	}
	sub, err := fs.Sub(skills.FS, skills.Name)
	if err != nil {
		return false, err
	}
	return false, fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func newInstallSkillCmd() *cobra.Command {
	var (
		agents        []string
		dryRun, print bool
		onlyInstalled bool
	)
	cmd := &cobra.Command{
		Use:   "install-skill",
		Short: "Install the jobtail skill for your coding agents (claude, opencode, codex)",
		Long: "Install the jobtail skill, which teaches a coding agent to install, schedule,\n" +
			"run and debug jobs with this CLI on your behalf, into each agent's\n" +
			"user-level skills directory.\n\n" +
			"With no --agent, installs for every supported agent found on this machine.\n" +
			"Re-running it updates the installed copies to this jobtail's version.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if print {
				data, err := fs.ReadFile(skills.FS, skills.Name+"/SKILL.md")
				if err != nil {
					return err
				}
				_, err = out.Write(data)
				return err
			}

			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			all := skillAgents(skillEnv{home: home, getenv: os.Getenv, lookPath: exec.LookPath})
			picked, err := selectSkillAgents(all, agents, onlyInstalled)
			if err != nil {
				return err
			}
			if len(picked) == 0 {
				if onlyInstalled {
					return nil // nothing installed yet: nothing to refresh
				}
				return fmt.Errorf("no supported coding agent found (supported: %s); "+
					"pass --agent to install anyway", supportedAgentNames(all))
			}

			for _, a := range picked {
				if dryRun {
					fmt.Fprintf(out, "%s\t%s\n", a.name, a.dir)
					continue
				}
				linked, err := installSkill(a.dir)
				if err != nil {
					return fmt.Errorf("%s: %w", a.name, err)
				}
				if linked {
					fmt.Fprintf(out, "%s: %s is a symlink, left as is\n", a.name, a.dir)
					continue
				}
				fmt.Fprintf(out, "installed jobtail skill for %s: %s\n", a.name, a.dir)
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&agents, "agent", nil,
		"agent(s) to install for: claude, opencode, codex (default: every one detected)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"list the agents and directories it would install to, one per line, without writing")
	cmd.Flags().BoolVar(&print, "print", false, "print the skill's SKILL.md to stdout instead")
	cmd.Flags().BoolVar(&onlyInstalled, "only-installed", false,
		"only refresh copies that are already installed (what `upgrade` runs)")
	return cmd
}
