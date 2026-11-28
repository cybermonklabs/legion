// packages/envoy/cmd/cipher/removed_github_flags_test.go
//
// The exec form's four GitHub flags of an earlier release: each exits 2, sends nothing, and names
// what replaces it.
package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestRemovedGitHubFlagsExit2NamingTheirReplacement: each of the four GitHub flags of an earlier
// release, in every spelling the flag parser takes, before the names or after them, exits 2 with
// the one line that names what replaces it, and the broker is asked nothing. The same word after
// -- is an argument of the command, which runs with it.
func TestRemovedGitHubFlagsExit2NamingTheirReplacement(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestTheRunFormsHelpSaysHowToReadAGitHubProfilesToken runs the command the exec form's help gives
// for reading one account's token out of a GitHub profile's JSON object of installations, as the
// help prints it, with a stand-in gh that prints the GH_TOKEN it was handed. For the login the
// object holds, gh gets that account's token. For a login the object does not hold, here the same
// login in another letter case, the command exits 1 naming the login and gh never runs: a read
// with jq -r alone handed gh the token "null", which GitHub answers 401. The help also says never
// to echo the variable, which prints a live token.
func TestTheRunFormsHelpSaysHowToReadAGitHubProfilesToken(t *testing.T) {
	t.Parallel()
	summary := lookupCommand("").summary
	if !strings.Contains(summary, "Never echo $NAME") {
		t.Errorf("the run form's help does not say never to echo $NAME:\n%s", summary)
	}
	// The command is the help's indented lines, the only ones in the summary.
	var lines []string
	for _, line := range strings.Split(summary, "\n") {
		if rest, indented := strings.CutPrefix(line, "  "); indented {
			lines = append(lines, rest)
		}
	}
	if len(lines) == 0 || !strings.HasSuffix(lines[len(lines)-1], " gh ...'") {
		t.Fatalf("the run form's help gives no command that ends in gh ...; its indented lines: %q", lines)
	}
	command := strings.Join(lines, "\n")
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatalf("the help's command runs jq, which is not on PATH: %v", err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho \"gh ran with GH_TOKEN=$GH_TOKEN\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	const release = `{"installations":{"acme":{"installation_id":7,"token":"ghs-fake7","expires_at":"2026-11-28T17:00:00Z"}}}`
	for _, tc := range []struct {
		account, stdout, stderr string
		exit                    int
	}{
		{account: "acme", stdout: "gh ran with GH_TOKEN=ghs-fake7\n", exit: 0},
		{account: "Acme", stderr: "no token for Acme\n", exit: 1},
	} {
		run := strings.Replace(strings.ReplaceAll(command, "ACCOUNT", tc.account), " gh ...'", " gh api /repos/acme/proofs'", 1)
		cmd := exec.Command("sh", "-c", run)
		cmd.Env = append(os.Environ(), "NAME="+release, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		exit := 0
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("run the help's command for %s: %v", tc.account, err)
			}
			exit = exitErr.ExitCode()
		}
		if exit != tc.exit || stdout.String() != tc.stdout || stderr.String() != tc.stderr {
			t.Errorf("the help's command for %s: exit %d, stdout %q, stderr %q; want exit %d, stdout %q, stderr %q\nthe command:\n%s",
				tc.account, exit, stdout.String(), stderr.String(), tc.exit, tc.stdout, tc.stderr, run)
		}
	}
}
