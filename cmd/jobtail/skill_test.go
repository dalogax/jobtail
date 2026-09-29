package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dalogax/jobtail/skills"
)

func fakeSkillEnv(home string, env map[string]string, onPath ...string) skillEnv {
	return skillEnv{
		home:   home,
		getenv: func(k string) string { return env[k] },
		lookPath: func(name string) (string, error) {
			for _, p := range onPath {
				if p == name {
					return "/usr/bin/" + name, nil
				}
			}
			return "", errors.New("not found")
		},
	}
}

func agentByName(t *testing.T, all []skillAgent, name string) skillAgent {
	t.Helper()
	for _, a := range all {
		if a.name == name {
			return a
		}
	}
	t.Fatalf("no agent %q", name)
	return skillAgent{}
}

func TestSkillAgentDirsFollowEachCLIsConfigHome(t *testing.T) {
	home := t.TempDir()
	all := skillAgents(fakeSkillEnv(home, nil))
	for name, want := range map[string]string{
		"claude":   filepath.Join(home, ".claude", "skills", "jobtail"),
		"opencode": filepath.Join(home, ".config", "opencode", "skills", "jobtail"),
		"codex":    filepath.Join(home, ".codex", "skills", "jobtail"),
	} {
		if got := agentByName(t, all, name).dir; got != want {
			t.Errorf("%s: dir = %s, want %s", name, got, want)
		}
	}

	all = skillAgents(fakeSkillEnv(home, map[string]string{
		"CLAUDE_CONFIG_DIR": "/c", "XDG_CONFIG_HOME": "/x", "CODEX_HOME": "/k",
	}))
	for name, want := range map[string]string{
		"claude": "/c/skills/jobtail", "opencode": "/x/opencode/skills/jobtail", "codex": "/k/skills/jobtail",
	} {
		if got := agentByName(t, all, name).dir; got != want {
			t.Errorf("%s with env override: dir = %s, want %s", name, got, want)
		}
	}
}

func TestSkillAgentDetectedByBinaryOrConfigDir(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	all := skillAgents(fakeSkillEnv(home, nil, "claude"))
	for name, want := range map[string]bool{"claude": true, "opencode": false, "codex": true} {
		if got := agentByName(t, all, name).present; got != want {
			t.Errorf("%s: present = %v, want %v", name, got, want)
		}
	}
}

func TestSelectSkillAgents(t *testing.T) {
	home := t.TempDir()
	all := skillAgents(fakeSkillEnv(home, nil, "claude", "codex"))

	picked, err := selectSkillAgents(all, nil, false)
	if err != nil || len(picked) != 2 || picked[0].name != "claude" || picked[1].name != "codex" {
		t.Fatalf("default selection = %v, %v; want the detected claude and codex", picked, err)
	}

	// Naming an agent installs for it even when it isn't detected.
	picked, err = selectSkillAgents(all, []string{"OpenCode"}, false)
	if err != nil || len(picked) != 1 || picked[0].name != "opencode" {
		t.Fatalf("--agent opencode = %v, %v", picked, err)
	}

	if _, err := selectSkillAgents(all, []string{"cursor"}, false); err == nil ||
		!strings.Contains(err.Error(), "claude, opencode, codex") {
		t.Fatalf("unknown agent should list the supported ones, got %v", err)
	}

	// --only-installed keeps only agents that already have the skill.
	if _, err := installSkill(agentByName(t, all, "codex").dir); err != nil {
		t.Fatal(err)
	}
	picked, err = selectSkillAgents(all, nil, true)
	if err != nil || len(picked) != 1 || picked[0].name != "codex" {
		t.Fatalf("--only-installed = %v, %v; want just codex", picked, err)
	}
}

func TestInstallSkillWritesEmbeddedCopyAndReplacesOldOne(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills", "jobtail")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if linked, err := installSkill(dir); err != nil || linked {
		t.Fatalf("installSkill = %v, %v", linked, err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := skills.FS.ReadFile("jobtail/SKILL.md")
	if string(got) != string(want) {
		t.Fatal("installed SKILL.md doesn't match the embedded one")
	}
	if !strings.HasPrefix(string(got), "---\nname: jobtail\n") {
		t.Fatalf("SKILL.md should start with frontmatter naming the skill, got %.40q", got)
	}
}

func TestInstallSkillLeavesSymlinkedDirAlone(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "SKILL.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "jobtail")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}

	if linked, err := installSkill(link); err != nil || !linked {
		t.Fatalf("installSkill on a symlink = %v, %v; want linked=true", linked, err)
	}
	if got, _ := os.ReadFile(filepath.Join(checkout, "SKILL.md")); string(got) != "mine" {
		t.Fatal("installSkill wrote through a symlink into someone's checkout")
	}
}
