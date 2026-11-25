package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// isolatedMessage is the message of each commit these tests make: it carries another session's
// Omp-Session line, so the session's own line is one the message lacks.
const isolatedMessage = "isolated\n\nOmp-Session: other-session"

// isolatedCommit makes the commit `commit --allow-empty -m <isolatedMessage>` in a new repository,
// through `legion git --real <the compiled git>` when wrapped and with that git alone otherwise.
// The environment is trailerEnv's with a session and a role whose lines legion git would add, the
// fixed author and committer dates, and set. It answers HEAD's id and message.
func isolatedCommit(t *testing.T, wrapped bool, set ...string) (id, message string) {
	t.Helper()
	git := fixturegit.CompiledGit(t)
	repo := trailerRepo(t, git)
	env := trailerEnv(t, trailerPath(git), append([]string{
		"DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=isolated-session", "LEGION_ROLE=implementer",
		"GIT_AUTHOR_DATE=1700000000 +0000", "GIT_COMMITTER_DATE=1700000000 +0000",
	}, set...)...)
	args := []string{"commit", "--allow-empty", "-m", isolatedMessage}
	if wrapped {
		if code, stderr := trailerCommit(t, trailerLegion(t), git, repo, env, "", args...); code != 0 {
			t.Fatalf("legion git --real %s -- %q = %d: %s", git, args, code, stderr)
		}
	} else if code, _, stderr := trailerRun(t, repo, env, "", git, args...); code != 0 {
		t.Fatalf("git %q = %d: %s", args, code, stderr)
	}
	code, stdout, stderr := trailerRun(t, repo, trailerEnv(t, trailerPath(git)), "", git, "rev-parse", "HEAD")
	if code != 0 {
		t.Fatalf("git rev-parse HEAD: %s", stderr)
	}
	return strings.TrimSuffix(stdout, "\n"), trailerMessage(t, git, repo)
}

// assertGitAlonesCommit fails unless the commit through legion git under set is the one git alone
// makes under set: the same id, so the same message.
func assertGitAlonesCommit(t *testing.T, set ...string) {
	t.Helper()
	aloneID, alone := isolatedCommit(t, false, set...)
	wrappedID, wrapped := isolatedCommit(t, true, set...)
	if wrappedID != aloneID {
		t.Fatalf("under %q the commit through legion git is %s with message %q, want git alone's %s with %q", set, wrappedID, wrapped, aloneID, alone)
	}
}

// GIT_CONFIG_NOSYSTEM=1 asks git for an isolated configuration, as a hermetic test does: legion git
// adds no trailer, so the commit is git alone's, its id too.
func TestACommitUnderAnIsolatedConfigurationGetsNoTrailer(t *testing.T) {
	assertGitAlonesCommit(t, "GIT_CONFIG_NOSYSTEM=1")
}

// GIT_CONFIG_GLOBAL=/dev/null, with GIT_CONFIG_NOSYSTEM unset, asks git for an isolated
// configuration too: the commit is git alone's.
func TestACommitUnderGitConfigGlobalDevNullGetsNoTrailer(t *testing.T) {
	assertGitAlonesCommit(t, "GIT_CONFIG_GLOBAL=/dev/null")
}

// GIT_CONFIG_NOSYSTEM=false is a value git reads as false, so it asks for no isolated
// configuration: the commit gets both lines after the message's own.
func TestACommitUnderGitConfigNoSystemFalseKeepsItsTrailers(t *testing.T) {
	_, got := isolatedCommit(t, true, "GIT_CONFIG_NOSYSTEM=false")
	if want := isolatedMessage + "\nOmp-Session: isolated-session\nLegion-Role: implementer\n"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

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
