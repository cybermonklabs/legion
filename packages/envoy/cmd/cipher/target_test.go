// packages/envoy/cmd/cipher/target_test.go
//
// The exec form's --target: the GitHub target that narrows the run's one profile, which the
// client checks and signs into the request object as its one detail's locations
// (record.TargetLocations). Every refusal exits 2 before the broker is asked anything.
package main

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// noSessionEnv clears every variable the client would forward as session_id, so each request
// body is the one a session outside a harness sends (TestRequestSignsARequestObject).
var noSessionEnv = []string{"OMP_SESSION_ID=", "ENVOY_SESSION_ID=", "CLAUDE_CODE_SESSION_ID=", "CODEX_THREAD_ID="}

// TestTargetIsSentAsLocations: --target is signed into the request object as the locations of
// its one detail, the account lowercased whatever case it was typed in, the locations sorted as
// the record sorts them and each once, so two runs of the same target sign the same locations and
// match the same grant. The request body carries the signed object and the session id alone, and
// the release carries no target, only the command it starts. A run with no target signs no
// locations.
func TestTargetIsSentAsLocations(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newCommandBroker(t, knowsEveryField)
	keyDir := newKeyDir(t)
	var hundredFlags, hundredLocations []string
	for i := 99; i >= 0; i-- {
		hundredFlags = append(hundredFlags, "--target", fmt.Sprintf("Acme-Org/repo-%03d", i))
		hundredLocations = append(hundredLocations, fmt.Sprintf("https://github.com/acme-org/repo-%03d", 99-i))
	}
	for _, tc := range []struct {
		name      string
		flags     []string
		locations []string
	}{
		{"the account form, lowercased", []string{"--target", "Acme-Org"}, []string{"https://github.com/acme-org"}},
		{
			"repository forms, the account lowercased, sorted and each once",
			[]string{"--target", "Acme-Org/widgets", "--target=acme-org/Proofs", "--target", "ACME-ORG/widgets"},
			[]string{"https://github.com/acme-org/Proofs", "https://github.com/acme-org/widgets"},
		},
		{"the account form twice", []string{"--target", "acme-org", "--target", "ACME-org"}, []string{"https://github.com/acme-org"}},
		{"a hundred repositories", hundredFlags, hundredLocations},
		{"no target", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := slices.Concat([]string{"APP"}, tc.flags, []string{"--", "sh", "-c", `echo "ran $APP"`})
			stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, noSessionEnv, args...)
			if exit != 0 || stdout != "ran APP-v1\n" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0 running with the value", exit, stdout, stderr)
			}
			_, bodies, objects := b.takeRequests()
			if len(bodies) != 1 || len(objects) != 1 {
				t.Fatalf("the broker read %d request bodies and verified %d request objects, want one of each", len(bodies), len(objects))
			}
			if got, want := keySet(bodies[0]), map[string]bool{"request": true, "session_id": true}; !mapsEqual(got, want) {
				t.Fatalf("the request body carried the fields %v, want %v", got, want)
			}
			if details := objects[0].Details; len(details) != 1 || details[0].Identifier != "APP" || !slices.Equal(details[0].Locations, tc.locations) {
				t.Fatalf("the signed authorization_details = %+v, want one detail for APP with the locations %q", details, tc.locations)
			}
			if releases, want := b.take(), []string{`{"command":["sh","-c","echo \"ran $APP\""]}`}; !reflect.DeepEqual(releases, want) {
				t.Fatalf("release bodies = %q, want %q, which carries no target", releases, want)
			}
		})
	}
}

// requireNothingAsked fails t unless the run exited 2 with stderr exactly line and nothing run,
// and the broker received no call at all.
func requireNothingAsked(t *testing.T, b *commandBroker, args []string, stdout, stderr string, exit int, line string) {
	t.Helper()
	if exit != 2 || stdout != "" || stderr != line {
		t.Fatalf("%q: exit %d, stdout %q, stderr %q; want exit 2, nothing run, and stderr %q", args, exit, stdout, stderr, line)
	}
	if calls, _, _ := b.takeRequests(); calls != 0 {
		t.Fatalf("%q: the broker received %d calls, want none", args, calls)
	}
}

// TestTargetWithTwoNamesIsRefused: a target narrows one profile, so a run of two names that gives
// one exits 2, naming the rule and the names, and the broker is asked nothing.
func TestTargetWithTwoNamesIsRefused(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newCommandBroker(t, knowsEveryField)
	keyDir := newKeyDir(t)
	for _, target := range []string{"acme-org", "acme-org/widgets"} {
		args := []string{"APP", "OTHER", "--target", target, "--", "sh", "-c", "echo ran-the-child"}
		stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, args...)
		requireNothingAsked(t, b, args, stdout, stderr, exit,
			"agent-secrets: --target narrows a run of one secret name, and this run names 2: APP, OTHER\n")
	}
}

// TestTargetsOfTwoAccountsAreRefused: every target of one run names one account, compared once
// each is lowercased, so targets naming two exit 2, naming both, and the broker is asked nothing.
func TestTargetsOfTwoAccountsAreRefused(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newCommandBroker(t, knowsEveryField)
	keyDir := newKeyDir(t)
	const line = "agent-secrets: --target names two accounts, acme-org and example-org: all targets of one run name one account\n"
	for _, targets := range [][]string{
		{"acme-org", "Example-Org"},
		{"Acme-Org/widgets", "example-org/widgets"},
		{"acme-org", "example-org/widgets"},
	} {
		args := []string{"APP", "--target", targets[0], "--target", targets[1], "--", "sh", "-c", "echo ran-the-child"}
		stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, args...)
		requireNothingAsked(t, b, args, stdout, stderr, exit, line)
	}
}

// TestTargetRulesAreCheckedBeforeAnyRequest: a target is ACCOUNT, a GitHub login once lowercased,
// or ACCOUNT/REPOSITORY, a name GitHub can hold; the account form stands alone; and a run names
// at most 100 targets once each is folded. Each refusal exits 2 naming the rule it breaks, and
// the broker is asked nothing. An empty value, the target an unset variable gives, is refused
// rather than read as no target, which would ask for the whole profile.
func TestTargetRulesAreCheckedBeforeAnyRequest(t *testing.T) {
	binary := buildAgentSecrets(t)
	b := newCommandBroker(t, knowsEveryField)
	keyDir := newKeyDir(t)
	// A value a location cannot hold is not printed (valueNotPrinted): it may be a pasted credential.
	const account = "agent-secrets: --target names no GitHub login as its account: 1 to 39 letters, digits, '-' and '_', the first a letter or a digit " + valueNotPrinted + "\n"
	const repository = "agent-secrets: --target names no repository name GitHub can hold: 1 to 100 letters, digits, '.', '-' and '_', neither . nor .. " + valueNotPrinted + "\n"
	var tooMany []string
	for i := range 101 {
		tooMany = append(tooMany, "--target", fmt.Sprintf("acme-org/repo-%03d", i))
	}
	longAccount, longRepository := strings.Repeat("a", 40), strings.Repeat("r", 101)
	for _, tc := range []struct {
		name  string
		flags []string
		line  string
	}{
		{"the account form beside a repository form", []string{"--target", "acme-org", "--target", "Acme-Org/widgets"},
			"agent-secrets: --target mixes the account form acme-org with repository forms of it: give ACCOUNT alone, or ACCOUNT/REPOSITORY for each repository\n"},
		{"more than 100 targets", tooMany, "agent-secrets: --target names 101 targets, more than 100\n"},
		{"an empty target", []string{"--target", ""}, account},
		{"an account with a space", []string{"--target", "Acme Org"}, account},
		{"an account opening with a dash", []string{"--target", "-acme"}, account},
		{"an account with a dot", []string{"--target", "acme.org"}, account},
		{"an account of 40 characters", []string{"--target", longAccount}, account},
		{"an account no ASCII lowercasing spells", []string{"--target", "\u212Aelvin"}, account},
		{"an empty repository", []string{"--target", "acme-org/"}, repository},
		{"the repository .", []string{"--target", "acme-org/."}, repository},
		{"the repository ..", []string{"--target", "acme-org/.."}, repository},
		{"a repository with a space", []string{"--target", "acme-org/a b"}, repository},
		{"a repository with a slash", []string{"--target", "acme-org/widgets/extra"}, repository},
		{"a repository of 101 characters", []string{"--target", "acme-org/" + longRepository}, repository},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := slices.Concat([]string{"APP"}, tc.flags, []string{"--", "sh", "-c", "echo ran-the-child"})
			stdout, stderr, exit := runAgentSecrets(t, binary, b.srv.URL, keyDir, nil, args...)
			requireNothingAsked(t, b, args, stdout, stderr, exit, tc.line)
		})
	}
}
