package main

import "testing"

// This file holds the unit test of gitIsolated (git_commit.go), apart from git_isolated_test.go,
// which names nothing the change adds and so compiles and runs at the base too.

// gitIsolated answers for each value as git 2.47.3 reads it, measured with `git config --get` of a
// key that only the file GIT_CONFIG_SYSTEM names sets, under GIT_CONFIG_NOSYSTEM=<value>: true
// where git skips that file, and false where it reads it or dies on the value (`bad boolean
// environment value`), which asks for no isolation. GIT_CONFIG_GLOBAL asks for it only as
// /dev/null exactly.
func TestGitIsolatedReadsEachValueAsGitDoes(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, value string
		want        bool
	}{
		{"GIT_CONFIG_NOSYSTEM", "0x10", true},
		{"GIT_CONFIG_NOSYSTEM", "0X1", true},
		{"GIT_CONFIG_NOSYSTEM", "0b1", true},
		{"GIT_CONFIG_NOSYSTEM", "0b2", false},
		{"GIT_CONFIG_NOSYSTEM", "010", true},
		{"GIT_CONFIG_NOSYSTEM", "08", false},
		{"GIT_CONFIG_NOSYSTEM", "1k", true},
		{"GIT_CONFIG_NOSYSTEM", "1G", true},
		{"GIT_CONFIG_NOSYSTEM", "2g", false},
		{"GIT_CONFIG_NOSYSTEM", "2047m", true},
		{"GIT_CONFIG_NOSYSTEM", "2048m", false},
		{"GIT_CONFIG_NOSYSTEM", "1x", false},
		{"GIT_CONFIG_NOSYSTEM", "2147483647", true},
		{"GIT_CONFIG_NOSYSTEM", "2147483648", false},
		{"GIT_CONFIG_NOSYSTEM", "-1", true},
		{"GIT_CONFIG_NOSYSTEM", "+1", true},
		{"GIT_CONFIG_NOSYSTEM", "-2147483648", false},
		{"GIT_CONFIG_NOSYSTEM", "2097152k", false},
		{"GIT_CONFIG_NOSYSTEM", " 1", true},
		{"GIT_CONFIG_NOSYSTEM", "\t1", true},
		{"GIT_CONFIG_NOSYSTEM", "1 ", false},
		{"GIT_CONFIG_NOSYSTEM", "true", true},
		{"GIT_CONFIG_NOSYSTEM", "TRUE", true},
		{"GIT_CONFIG_NOSYSTEM", "yes", true},
		{"GIT_CONFIG_NOSYSTEM", "On", true},
		{"GIT_CONFIG_NOSYSTEM", "false", false},
		{"GIT_CONFIG_NOSYSTEM", "No", false},
		{"GIT_CONFIG_NOSYSTEM", "off", false},
		{"GIT_CONFIG_NOSYSTEM", "", false},
		{"GIT_CONFIG_NOSYSTEM", "0", false},
		{"GIT_CONFIG_NOSYSTEM", "0x0", false},
		{"GIT_CONFIG_NOSYSTEM", "maybe", false},
		{"GIT_CONFIG_GLOBAL", "/dev/null", true},
		{"GIT_CONFIG_GLOBAL", "/dev/null2", false},
	} {
		if got := gitIsolated(map[string]string{row.name: row.value}); got != row.want {
			t.Errorf("gitIsolated(%s=%q) = %v, want %v", row.name, row.value, got, row.want)
		}
	}
}
