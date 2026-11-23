// packages/envoy/cmd/cipher/removed_github_flags_test.go
//
// The exec form's four GitHub flags of an earlier release: each exits 2, sends nothing, and names
// what replaces it.
package main

import (
	"slices"
	"strings"
	"testing"
)

// TestRemovedGitHubFlagsExit2NamingTheirReplacement: each of the four GitHub flags of an earlier
// release, in every spelling the flag parser takes, before the names or after them, exits 2 with
// the one line that names what replaces it, and the broker is asked nothing. The same word after
// -- is an argument of the command, which runs with it.
func TestRemovedGitHubFlagsExit2NamingTheirReplacement(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newCommandBroker(t, knowsEveryField)
	keyDir := newKeyDir(t)
	for flag, message := range map[string]string{
		"github-owner":     "--github-owner was removed: narrow the profile with --target ACCOUNT",
		"github-repo":      "--github-repo was removed: narrow the profile with --target ACCOUNT/REPOSITORY",
		"github-read-only": "--github-read-only was removed: name a read profile, such as GITHUB_READ, and narrow it with --target ACCOUNT",
		"github-token":     `--github-token was removed: a profile with "value": "token" puts one token in the variable`,
	} {
		for _, spelling := range [][]string{
			{"--" + flag},
			{"--" + flag, "acme-org"},
			{"--" + flag + "=acme-org"},
			{"-" + flag, "acme-org"},
			{"-" + flag + "=true"},
		} {
			command := []string{"--", "sh", "-c", "echo ran-the-child"}
			for _, args := range [][]string{
				slices.Concat([]string{"APP"}, spelling, command),
				slices.Concat(spelling, []string{"APP"}, command),
			} {
				stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, args...)
				requireNothingAsked(t, b, args, stdout, stderr, exit, "agent-secrets: "+message+"\n")
			}
		}
	}

	stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, "APP", "--", "sh", "-c", `echo "$1"`, "sh", "--github-owner")
	if exit != 0 || stdout != "--github-owner\n" {
		t.Fatalf("a command whose own argument is --github-owner: exit %d, stdout %q, stderr %q; want exit 0 and the argument echoed", exit, stdout, stderr)
	}
}

// TestTheRunFormsHelpNamesTargetAndNoRemovedFlag: the exec form's -h shows --target in its
// synopsis and its flags, and names none of the four removed GitHub flags.
func TestTheRunFormsHelpNamesTargetAndNoRemovedFlag(t *testing.T) {
	binary := buildAgentSecrets(t)
	stdout, stderr, exit := runAgentSecrets(t, binary, "http://127.0.0.1:1", t.TempDir(), nil, "NAME", "--help", "--", "true")
	if exit != 0 || !strings.Contains(stderr, "[--target ACCOUNT[/REPOSITORY]]...") || !strings.Contains(stderr, "  -target ") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0 and --target in the synopsis and the flags", exit, stdout, stderr)
	}
	for _, removed := range []string{"github-owner", "github-repo", "github-read-only", "github-token"} {
		if strings.Contains(stderr, removed) {
			t.Fatalf("the help names %s: %q", removed, stderr)
		}
	}
}
