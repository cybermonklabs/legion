// second_prefix_test.go holds the one rule of whether a grant's frozen source counts as the source
// the policy serves its name from (sourceRule), with BROKER_SECRETS_SECOND_PREFIX set and with it
// empty, on Secret Manager resource names, and the two places a release reaches it: the re-check
// of a release and a reuse (stillAllowed), and the read itself (readSource).
package requests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// resourceNamed is a store whose listings' Source is the secret's full name (Listing.ID), as a
// Secret Manager listing answers (gcp.Store): the source a Secret Manager secret is read by is its
// resource name, projects/<number>/secrets/<id>. Over the memory store, whose own sources are
// memory:<name>, it gives these rows the sources a GCP deployment freezes, and Read takes that
// name, which the memory store answers by name.
type resourceNamed struct{ secretstore.Store }

func (r resourceNamed) List(ctx context.Context, prefix string, opts secretstore.ReadOptions) ([]secretstore.Listing, error) {
	listings, err := r.Store.List(ctx, prefix, opts)
	for i := range listings {
		listings[i].Source = listings[i].ID
	}
	return listings, err
}

func (r resourceNamed) Describe(ctx context.Context, id string, opts secretstore.ReadOptions) (secretstore.Listing, error) {
	listing, err := r.Store.Describe(ctx, id, opts)
	if err == nil {
		listing.Source = listing.ID
	}
	return listing, err
}

// The three Secret Manager prefixes of these rows, in one placeholder project: the one the broker
// is configured with, the second one a move leaves, and one that is neither.
const (
	movedConfigured = "projects/000000000000/secrets/cipher-"
	movedSecond     = "projects/000000000000/secrets/agent-secrets-"
	movedNeither    = "projects/000000000000/secrets/elsewhere-"
)

// TestSourceCountsThroughTheSecondPrefix holds the two places a release reaches the rule a frozen
// source is held to (sourceRule), on Secret Manager sources of the form
// projects/000000000000/secrets/<id>: a stored secret, and a profile whose source secret moved,
// under the second prefix, with the setting empty, and under a prefix that is neither. The rule's
// own rows, on Secret Manager sources alone, are TestSourceRuleOnSecretManagerSources'.
//
// The policy rows load a real policy through the loader on Secret Manager resource names (the
// memory store through resourceNamed), and hold stillAllowed, the re-check of a release and of a
// reuse, and readSource, the release's read, to the same answers, so each goes through the rule.
//
// The named reverts: count with the setting empty, and its rows count; have readSource answer
// the frozen source, and the read rows read the second prefix.
func TestSourceCountsThroughTheSecondPrefix(t *testing.T) {
	moved := gcpRule(t, movedConfigured, movedSecond)
	unset := gcpRule(t, movedConfigured, "")
	app := func(store string) string { return policy.TypeSource(policy.TypeGitHubApp, store) }
	installations := func(store string) string { return policy.TypeSource(policy.TypeGitHubInstallations, store) }
	// A real policy on Secret Manager resource names: a stored secret, and a github-app secret
	// served through a profile of another name, whose source is its source secret's.
	stored := policytest.Secret("DEEL_API_KEY", policy.OwnerShared, policy.TierAgent, "deel-value")
	stored.Name = movedConfigured + "deel-api-key"
	appKey := policytest.TypeSecret("APP_KEY", policy.OwnerShared, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
	appKey.Name = movedConfigured + "app-key"
	loader := policy.Loader{
		Store:  resourceNamed{memory.New(stored, appKey)},
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

		m := &Machine{SourcePrefix: row.rule.prefix, SecondSourcePrefix: row.rule.second}
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
