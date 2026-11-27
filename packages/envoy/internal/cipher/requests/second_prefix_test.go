// second_prefix_test.go holds the one rule of whether a grant's frozen source counts as the source
// the policy serves its name from (sourceRule), with BROKER_SECRETS_SECOND_PREFIX set and with it
// empty, on Secret Manager resource names, and the two places a release reaches it: the re-check
// of a release and a reuse (stillAllowed), and the read itself (readSource).
package requests

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// The three Secret Manager prefixes of these rows, in one placeholder project: the one the broker
// is configured with, the second one a move leaves, and one that is neither.
const (
	movedConfigured = "projects/000000000000/secrets/cipher-"
	movedSecond     = "projects/000000000000/secrets/agent-secrets-"
	movedNeither    = "projects/000000000000/secrets/elsewhere-"
)

// TestSourceCountsThroughTheSecondPrefix holds the rule a frozen source is held to, on Secret
// Manager sources of the form projects/000000000000/secrets/<id>. It counts as the policy's
// source when the two are equal, with the second prefix set or not; and, with the second prefix
// set, a stored source under the second prefix with the same remainder, a minted source of the
// same type whose store source is under the second prefix with the same remainder, and a
// profile whose source secret moved so. It refuses a source that differs in the remainder, a
// minted source of another type, a source under a prefix that is neither of the two, a stored
// frozen source against a minted policy source and the reverse, a source under the configured
// prefix where the policy serves one under the second, a source under the second prefix with the
// setting empty, and a source that is the second prefix itself where the policy serves the
// configured prefix itself, which leaves no remainder after either.
//
// The policy rows load a real policy through the loader on Secret Manager resource names
// (secrets.ResourceNamed), and hold stillAllowed, the re-check of a release and of a reuse, and
// readSource, the release's read, to the same answers, so each goes through the rule.
//
// The named reverts: compare by the part after the prefix alone, and the type, neither-prefix
// and stored-against-minted rows count; replace the prefix in both directions, and the
// "configured against second" row counts; count with the setting empty, and its row counts;
// drop the guard against an empty remainder, and the row of the two bare prefixes counts; have
// readSource answer the frozen source, and the read rows read the second prefix.
func TestSourceCountsThroughTheSecondPrefix(t *testing.T) {
	moved := sourceRule{prefix: movedConfigured, second: movedSecond}
	unset := sourceRule{prefix: movedConfigured}
	app := func(store string) string { return policy.TypeSource(policy.TypeGitHubApp, store) }
	installations := func(store string) string { return policy.TypeSource(policy.TypeGitHubInstallations, store) }
	for _, row := range []struct {
		name           string
		rule           sourceRule
		frozen, served string
		counts         bool
	}{
		{"an equal stored source", moved, movedConfigured + "deel-api-key", movedConfigured + "deel-api-key", true},
		{"an equal stored source with the setting empty", unset, movedConfigured + "deel-api-key", movedConfigured + "deel-api-key", true},
		{"an equal minted source", moved, app(movedConfigured + "app-key"), app(movedConfigured + "app-key"), true},
		{"a stored source under the second prefix with the same remainder", moved, movedSecond + "deel-api-key", movedConfigured + "deel-api-key", true},
		{"a minted source of the same type under the second prefix", moved, app(movedSecond + "app-key"), app(movedConfigured + "app-key"), true},

		{"a stored source under the second prefix with another remainder", moved, movedSecond + "other-key", movedConfigured + "deel-api-key", false},
		{"a minted source under the second prefix with another remainder", moved, app(movedSecond + "other-key"), app(movedConfigured + "app-key"), false},
		{"a minted source of another type under the second prefix", moved, installations(movedSecond + "app-key"), app(movedConfigured + "app-key"), false},
		{"a minted source of another type under the configured prefix", moved, installations(movedConfigured + "app-key"), app(movedConfigured + "app-key"), false},
		{"a stored source under a prefix that is neither", moved, movedNeither + "deel-api-key", movedConfigured + "deel-api-key", false},
		{"a minted source under a prefix that is neither", moved, app(movedNeither + "app-key"), app(movedConfigured + "app-key"), false},
		{"a stored frozen source against a minted policy source", moved, movedSecond + "app-key", app(movedConfigured + "app-key"), false},
		{"a stored frozen source against a minted policy source, both under the configured prefix", moved, movedConfigured + "app-key", app(movedConfigured + "app-key"), false},
		{"a minted frozen source against a stored policy source", moved, app(movedSecond + "deel-api-key"), movedConfigured + "deel-api-key", false},
		{"a source under the configured prefix where the policy serves one under the second", moved, movedConfigured + "deel-api-key", movedSecond + "deel-api-key", false},
		{"a source under the second prefix where the policy serves one under neither", moved, movedSecond + "deel-api-key", movedNeither + "deel-api-key", false},
		{"a source under the second prefix with the setting empty", unset, movedSecond + "deel-api-key", movedConfigured + "deel-api-key", false},
		{"a minted source under the second prefix with the setting empty", unset, app(movedSecond + "app-key"), app(movedConfigured + "app-key"), false},
		{"a minted source that names no store source", moved, "mint:" + policy.TypeGitHubApp + ":", app(movedConfigured + "app-key"), false},
		{"a source that is the second prefix itself, where the policy serves the configured prefix itself", moved, movedSecond, movedConfigured, false},
		{"a minted source on the second prefix itself, where the policy mints from the configured prefix itself", moved, app(movedSecond), app(movedConfigured), false},
	} {
		if got := row.rule.counts(row.frozen, row.served); got != row.counts {
			t.Errorf("%s: counts(%q, %q) = %t, want %t", row.name, row.frozen, row.served, got, row.counts)
		}
	}

	// A real policy on Secret Manager resource names: a stored secret, and a github-app secret
	// served through a profile of another name, whose source is its source secret's.
	stored := policytest.Secret("DEEL_API_KEY", policy.OwnerShared, policy.TierAgent, "deel-value")
	stored.Name = movedConfigured + "deel-api-key"
	appKey := policytest.TypeSecret("APP_KEY", policy.OwnerShared, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
	appKey.Name = movedConfigured + "app-key"
	loader := policy.Loader{
		Store:  secrets.ResourceNamed(policytest.Store(secrets.NewLocal(stored, appKey))),
		Prefix: movedConfigured,
		Profiles: policytest.Profiles(t, policytest.ProfilesFile(
			`{"name": "APP_READ", "source": "APP_KEY", "github": {"accounts": ["acme"], "permissions": {"metadata": "read"}}}`)),
	}
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("policy.NewCurrent: %v", err)
	}
	set := current.Get()
	if got := set.Secrets["DEEL_API_KEY"].Source; got != movedConfigured+"deel-api-key" {
		t.Fatalf("DEEL_API_KEY is served from %q; want its Secret Manager resource name, %s", got, movedConfigured+"deel-api-key")
	}
	if got := set.Secrets["APP_READ"].Source; got != app(movedConfigured+"app-key") {
		t.Fatalf("APP_READ is served from %q; want its source secret's, %s", got, app(movedConfigured+"app-key"))
	}
	requester := policy.Requester{Operator: "ada@example.com"}
	for _, row := range []struct {
		name, frozen string
		rule         sourceRule
		live         bool
	}{
		{"DEEL_API_KEY", movedSecond + "deel-api-key", moved, true},
		{"DEEL_API_KEY", movedSecond + "deel-api-key", unset, false},
		{"DEEL_API_KEY", movedNeither + "deel-api-key", moved, false},
		{"APP_READ", app(movedSecond + "app-key"), moved, true},
		{"APP_READ", app(movedSecond + "app-key"), unset, false},
		{"APP_READ", installations(movedSecond + "app-key"), moved, false},
	} {
		err := stillAllowed(set, row.rule, row.name, row.frozen, policy.Automatic, "", nil, requester)
		switch {
		case row.live && err != nil:
			t.Errorf("stillAllowed(%s frozen at %s, second prefix %q) = %v; want nil, the grant goes on", row.name, row.frozen, row.rule.second, err)
		case !row.live && !errors.Is(err, errSourceMoved):
			t.Errorf("stillAllowed(%s frozen at %s, second prefix %q) = %v; want errSourceMoved, which ends the grant", row.name, row.frozen, row.rule.second, err)
		}

		m := &Machine{SecretsPrefix: row.rule.prefix, SecondPrefix: row.rule.second}
		secretType, source, minted, err := m.readSource(requestedSecret{name: row.name, source: row.frozen}, set.Secrets[row.name])
		wantType, wantSource, wantMinted, _ := policy.ParseTypeSource(set.Secrets[row.name].Source)
		switch {
		case row.live && (err != nil || secretType != wantType || source != wantSource || minted != wantMinted):
			t.Errorf("readSource(%s frozen at %s) = %q, %q, %t, %v; want the policy's own source %q, never the frozen one", row.name, row.frozen, secretType, source, minted, err, wantSource)
		case !row.live && (!errors.Is(err, errSourceMoved) || source != ""):
			t.Errorf("readSource(%s frozen at %s, second prefix %q) = %q, %v; want no source read and errSourceMoved", row.name, row.frozen, row.rule.second, source, err)
		}
	}
}
