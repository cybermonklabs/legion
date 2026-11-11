// packages/envoy/cmd/agent-secrets/githubtoken_test.go
//
// The exec form's --github-token: what the child's NAME holds with the flag and without it, and
// the refusals that keep the flag from handing over a value a command cannot send to GitHub.
package main

import (
	"maps"
	"strings"
	"testing"
)

// githubTokenReleases is what the fake broker (narrow_test.go's narrowBroker) releases for each
// name these tests ask for: APP is a github-app secret of one installation, TWO_APP one of two,
// and PLAIN a stored password. Each github-app value is the object the broker really mints
// (internal/broker/secrets, githubMintedInstallations), so what a child reads back is what a
// caller reads back today.
var githubTokenReleases = map[string]string{
	"APP":     `{"installations":{"ada":{"installation_id":8,"token":"ghs_one_installation","expires_at":"2100-01-01T00:00:00Z"}}}`,
	"TWO_APP": `{"installations":{"acme":{"installation_id":7,"token":"ghs_acme","expires_at":"2100-01-01T00:00:00Z"},"ada":{"installation_id":8,"token":"ghs_ada","expires_at":"2100-01-01T00:00:00Z"}}}`,
	"PLAIN":   "topsecretvalue123",
}

// TestTheGitHubTokenFlagHandsOverTheTokenAlone: with --github-token the child's NAME holds that
// installation's token, the string a command puts in an Authorization header; without the flag it
// holds the JSON object of installations the release carries, exactly as it always has, which is
// what a caller sends to GitHub today and GitHub answers 401.
func TestTheGitHubTokenFlagHandsOverTheTokenAlone(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newNarrowBroker(t, githubTokenReleases)
	keyDir := newKeyDir(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"with the flag", []string{"APP", "--github-owner", "ada", "--github-token"}, "ghs_one_installation"},
		{"without the flag", []string{"APP", "--github-owner", "ada"}, githubTokenReleases["APP"]},
		{"with the flag and every narrowing", []string{"APP", "--github-owner", "Ada", "--github-repo", "proofs", "--github-read-only", "--github-token"}, "ghs_one_installation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(tc.args, "--", "sh", "-c", `printf %s "$APP"`)
			stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, args...)
			if exit != 0 || stdout != tc.want {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0 and the child's APP %q", exit, stdout, stderr, tc.want)
			}
		})
	}
}

// TestTheGitHubTokenFlagLeavesAPlainValueAsItIs: the flag says how a GitHub App token is handed
// over and says nothing about any other secret, so a grant that holds a password beside the
// github-app secret runs with that password as it is stored and the App's token alone.
func TestTheGitHubTokenFlagLeavesAPlainValueAsItIs(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newNarrowBroker(t, githubTokenReleases)
	keyDir := newKeyDir(t)
	stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil,
		"PLAIN", "APP", "--github-owner", "ada", "--github-token", "--", "sh", "-c", `printf '%s|%s' "$PLAIN" "$APP"`)
	if want := githubTokenReleases["PLAIN"] + "|ghs_one_installation"; exit != 0 || stdout != want {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0 and %q", exit, stdout, stderr, want)
	}
}

// TestTheGitHubTokenFlagWithoutAnOwnerExitsTwo: --github-token hands over one account's token, so
// with no --github-owner it is a usage error that names the flag, and nothing reaches the broker,
// not even the request a person would approve.
func TestTheGitHubTokenFlagWithoutAnOwnerExitsTwo(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newNarrowBroker(t, githubTokenReleases)
	keyDir := newKeyDir(t)
	stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil,
		"APP", "--github-token", "--", "sh", "-c", "echo ran-the-child")
	if exit != 2 || strings.Contains(stdout, "ran-the-child") || !strings.Contains(stderr, "--github-token") || !strings.Contains(stderr, "--github-owner") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 2 naming --github-token and --github-owner", exit, stdout, stderr)
	}
	if requests, releases := b.takeRequests(), len(b.take()); requests != 0 || releases != 0 {
		t.Fatalf("the broker was asked %d requests and %d releases, want none", requests, releases)
	}
}

// TestAGitHubAppReleaseOfTwoInstallationsRunsNothing: --github-token hands over one installation's
// token, and a release of two holds no one token, so the exec form exits 1 naming the secret and
// the flag, and runs nothing. The refusal names a remedy the caller can take, a narrowing that
// leaves one installation, and no part of either token.
func TestAGitHubAppReleaseOfTwoInstallationsRunsNothing(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newNarrowBroker(t, githubTokenReleases)
	keyDir := newKeyDir(t)
	stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil,
		"TWO_APP", "--github-owner", "ada", "--github-token", "--", "sh", "-c", "echo ran-the-child")
	if exit != 1 || strings.Contains(stdout, "ran-the-child") || !strings.Contains(stderr, "TWO_APP") ||
		!strings.Contains(stderr, "--github-token") || !strings.Contains(stderr, "nothing was run") ||
		!strings.Contains(stderr, "a narrowing that leaves one installation") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 naming TWO_APP, --github-token and a remedy, and nothing run", exit, stdout, stderr)
	}
	if strings.Contains(stderr, "ghs_") {
		t.Fatalf("stderr %q names a token; a refusal names the secret and the accounts alone", stderr)
	}
}

// TestAGrantWithNoGitHubAppReleaseRunsNothing: a grant whose every released value is a plain
// secret gives --github-token nothing to hand over, which would make the flag a silent no-op and
// send a password to GitHub, so the exec form exits 1 naming the names it read and the flag.
func TestAGrantWithNoGitHubAppReleaseRunsNothing(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newNarrowBroker(t, githubTokenReleases)
	keyDir := newKeyDir(t)
	stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil,
		"PLAIN", "--github-owner", "ada", "--github-token", "--", "sh", "-c", "echo ran-the-child")
	if exit != 1 || strings.Contains(stdout, "ran-the-child") || !strings.Contains(stderr, "PLAIN") ||
		!strings.Contains(stderr, "--github-token") || !strings.Contains(stderr, "nothing was run") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit 1 naming PLAIN and --github-token, and nothing run", exit, stdout, stderr)
	}
	if strings.Contains(stderr, githubTokenReleases["PLAIN"]) {
		t.Fatalf("stderr %q holds the value of PLAIN; a refusal names the secret alone", stderr)
	}
}

// TestOnlyAGitHubAppReleaseIsReadAsOne: githubTokensOf reads the object the broker mints and
// nothing else. A document of another shape — another field beside installations, an entry short
// of a field, a field of another type, an account that is no GitHub login — is no release, so its
// value is handed over as it is rather than read for a token it does not hold.
func TestOnlyAGitHubAppReleaseIsReadAsOne(t *testing.T) {
	const entry = `"ada":{"installation_id":8,"token":"ghs_one_installation","expires_at":"2100-01-01T00:00:00Z"}`
	for _, tc := range []struct {
		name      string
		value     string
		tokens    map[string]string
		isRelease bool
	}{
		{"one installation", githubTokenReleases["APP"], map[string]string{"ada": "ghs_one_installation"}, true},
		{"two installations", githubTokenReleases["TWO_APP"], map[string]string{"acme": "ghs_acme", "ada": "ghs_ada"}, true},
		{"no installation at all", `{"installations":{}}`, map[string]string{}, true},
		{"a stored password", githubTokenReleases["PLAIN"], nil, false},
		{"a token on its own", "ghs_one_installation", nil, false},
		{"an empty value", "", nil, false},
		{"another field beside installations", `{"installations":{` + entry + `},"expires_at":"2100-01-01T00:00:00Z"}`, nil, false},
		{"no installations field", `{"tokens":{` + entry + `}}`, nil, false},
		{"a null installations field", `{"installations":null}`, nil, false},
		{"an array of releases", `[{"installations":{` + entry + `}}]`, nil, false},
		{"an entry that is no object", `{"installations":{"ada":"ghs_one_installation"}}`, nil, false},
		{"an entry with no token", `{"installations":{"ada":{"installation_id":8,"expires_at":"2100-01-01T00:00:00Z"}}}`, nil, false},
		{"an entry with an empty token", `{"installations":{"ada":{"installation_id":8,"token":"","expires_at":"2100-01-01T00:00:00Z"}}}`, nil, false},
		{"an entry with no expiry", `{"installations":{"ada":{"installation_id":8,"token":"ghs_one_installation"}}}`, nil, false},
		{"an entry with no installation id", `{"installations":{"ada":{"token":"ghs_one_installation","expires_at":"2100-01-01T00:00:00Z"}}}`, nil, false},
		{"an installation id that is no number", `{"installations":{"ada":{"installation_id":"8","token":"ghs_one_installation","expires_at":"2100-01-01T00:00:00Z"}}}`, nil, false},
		{"an account that is no login", `{"installations":{"ada/proofs":{"installation_id":8,"token":"ghs_one_installation","expires_at":"2100-01-01T00:00:00Z"}}}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens, ok := githubTokensOf(tc.value)
			if ok != tc.isRelease {
				t.Fatalf("githubTokensOf read it as a release = %v, want %v", ok, tc.isRelease)
			}
			if ok && !maps.Equal(tokens, tc.tokens) {
				t.Fatalf("githubTokensOf tokens = %v, want %v", tokens, tc.tokens)
			}
		})
	}
}

// TestGitHubTokensAloneNamesNoToken: the two refusals name the secret, or the names the grant
// released, with the flag and the accounts, and no part of any token. A grant that holds one
// release beside a password leaves that password as it is. The refusal of a release of two names
// a remedy the caller can take: a narrowing that leaves one installation, or the same run
// without the flag. It never asks for --github-owner again, which the caller gave already and
// which cannot make two installations one, since the mint compares the owner without regard to
// case.
func TestGitHubTokensAloneNamesNoToken(t *testing.T) {
	values := map[string]string{"APP": githubTokenReleases["APP"], "PLAIN": githubTokenReleases["PLAIN"]}
	if err := githubTokensAlone(values); err != nil {
		t.Fatalf("githubTokensAlone of one release beside a password = %v, want it handed over", err)
	}
	want := map[string]string{"APP": "ghs_one_installation", "PLAIN": githubTokenReleases["PLAIN"]}
	if !maps.Equal(values, want) {
		t.Fatalf("values = %v, want %v", values, want)
	}

	err := githubTokensAlone(map[string]string{"TWO_APP": githubTokenReleases["TWO_APP"]})
	if err == nil || !strings.Contains(err.Error(), "TWO_APP") || !strings.Contains(err.Error(), "--github-token") ||
		!strings.Contains(err.Error(), "acme, ada") || strings.Contains(err.Error(), "ghs_") ||
		!strings.Contains(err.Error(), "a narrowing that leaves one installation") ||
		strings.Contains(err.Error(), "name the account with --github-owner") {
		t.Fatalf("githubTokensAlone of two installations = %v, want a refusal naming TWO_APP, --github-token, both accounts and a remedy the caller can take, and no token", err)
	}

	err = githubTokensAlone(map[string]string{"PLAIN": githubTokenReleases["PLAIN"], "OTHER": "another-password"})
	if err == nil || !strings.Contains(err.Error(), "OTHER, PLAIN") || !strings.Contains(err.Error(), "--github-token") ||
		strings.Contains(err.Error(), githubTokenReleases["PLAIN"]) {
		t.Fatalf("githubTokensAlone of no release = %v, want a refusal naming both names and --github-token, and no value", err)
	}
}
