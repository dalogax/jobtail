package main

import "testing"

// resumeCommand's real per-provider syntax was verified against each
// binary's own --help before writing this (see cmd_run.go's comment);
// this test just locks that mapping in place.
func TestResumeCommandPerProvider(t *testing.T) {
	// Isolate from any JOBTAIL_*_BIN already set in the ambient environment
	// (this test runs in-process, not via a subprocess with a fresh env
	// like the e2e suite's tests do).
	t.Setenv("JOBTAIL_CLAUDE_BIN", "")
	t.Setenv("JOBTAIL_OPENCODE_BIN", "")
	t.Setenv("JOBTAIL_CODEX_BIN", "")

	cases := []struct {
		provider string
		want     string
	}{
		{"", "claude --resume sess-1"},
		{"claude", "claude --resume sess-1"},
		{"opencode", "opencode --session sess-1"},
		{"codex", "codex resume sess-1"},
	}
	for _, c := range cases {
		if got := resumeCommand(c.provider, "sess-1"); got != c.want {
			t.Errorf("resumeCommand(%q, ...) = %q, want %q", c.provider, got, c.want)
		}
	}
}
