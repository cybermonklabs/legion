package main

import "testing"

func TestHarnessNameReadsClaudeCodesNpmBinaryAsClaude(t *testing.T) {
	for comm, want := range map[string]string{
		"claude\n":     "claude",
		"claude.exe\n": "claude",
		"codex\n":      "codex",
		"node\n":       "node",
	} {
		if got := harnessName(comm); got != want {
			t.Errorf("harnessName(%q) = %q, want %q", comm, got, want)
		}
	}
}
