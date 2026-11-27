package policy_test

// A profile never widens what its source allows: a profile of a secret that carries a
// dispatch-project or a project-channels tag is served with that tag, so Evaluate, and every
// reader of a served Secret's project, holds it to the project as it holds the source.

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// projectProfiles is the profiles file of these rows: PRIV_READ and GONE_READ, each a profile of
// a stored secret of a project under a name of its own, behind the proxy as a stored value's
// profile must be; APP_READ, a profile of APP_KEY behind no proxy; and APP_PROXIED, a profile of
// the same App behind the proxy.
var projectProfiles = policytest.KindsFile(
	`{"name": "PRIV_READ", "source": "PRIV_KEY", "proxy": "example"}`,
	`{"name": "GONE_READ", "source": "GONE_KEY", "proxy": "example"}`,
	`{"name": "APP_READ", "source": "APP_KEY", "github": {"accounts": ["ada"], "permissions": {"metadata": "read"}}}`,
	`{"name": "APP_PROXIED", "source": "APP_KEY", "github": {"accounts": ["ada"], "permissions": {"metadata": "read"}}, "proxy": "github"}`,
)

// loadProfiled is the policy a broker holding a K3 client publishes over stored, with
// projectProfiles applied: Current applies the profiles at every publish, which a bare Load does
// not.
func loadProfiled(t *testing.T, stored ...secrets.LocalSecret) *policy.Set {
	t.Helper()
	loader := projectLoader(secrets.NewLocal(stored...))
	loader.Profiles = policytest.Profiles(t, projectProfiles, projectSvc)
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("policy.NewCurrent: %v", err)
	}
	return current.Get()
}

// TestAProfileOfAPrivateProjectSecretGoesOnlyToACredentialThatNamesIt: PRIV_READ, a profile of
// PRIV_KEY, which PRIV tags, answers each credential as PRIV_KEY itself answers it
// (TestAPrivateProjectSecretGoesOnlyToACredentialThatNamesIt): released to a credential that names
// PRIV, denied project-not-named to one that names another project or none, an error with no state
// for PRIV, and denied project-missing for a profile of a secret whose project Dispatch does not
// hold. Served without its source's project, the profile would go by owner and tier to every
// session, which is what the tag exists to stop.
func TestAProfileOfAPrivateProjectSecretGoesOnlyToACredentialThatNamesIt(t *testing.T) {
	set := loadProfiled(t,
		projectSecret("PRIV_KEY", policy.OwnerShared, policy.TierAgent, privProject),
		projectSecret("GONE_KEY", policy.OwnerShared, policy.TierAgent, goneProject),
	)
	if got := set.Secrets["PRIV_READ"].Project; got != privProject {
		t.Fatalf("PRIV_READ is served with project %q; want its source's %q", got, privProject)
	}
	priv := secrets.LocalARN(policytest.ID("PRIV_KEY"))
	for _, c := range []struct {
		name, secret string
		requester    policy.Requester
		want         policy.Decision
	}{
		{"a credential naming PRIV", "PRIV_READ", credential(carol, "", privProject), policy.Decision{Outcome: policy.Automatic, Source: priv}},
		{"a credential naming no project", "PRIV_READ", credential(carol, ""), policy.Decision{Outcome: policy.Deny, Reason: policy.ReasonProjectNotNamed, Source: priv}},
		{"a credential naming the public project", "PRIV_READ", credential(carol, "", pubProject), policy.Decision{Outcome: policy.Deny, Reason: policy.ReasonProjectNotNamed, Source: priv}},
		{"a credential naming the missing project", "GONE_READ", credential(carol, "", goneProject),
			policy.Decision{Outcome: policy.Deny, Reason: policy.ReasonProjectMissing, Source: secrets.LocalARN(policytest.ID("GONE_KEY"))}},
	} {
		got, err := set.Evaluate(c.secret, c.requester)
		if err != nil || got != c.want {
			t.Errorf("Evaluate(%s) for %s = %+v, %v; want %+v", c.secret, c.name, got, err, c.want)
		}
	}
	if _, err := set.Evaluate("PRIV_READ", policy.Requester{Operator: carol}); !errors.Is(err, policy.ErrNoProjectState) {
		t.Errorf("Evaluate(PRIV_READ) with no state for PRIV = %v; want ErrNoProjectState, as for PRIV_KEY", err)
	}
}

// TestAProfileOfAProjectChannelsSecretIsLimitedAsItsSource: APP_KEY carries project-channels, so
// a credential naming a private project gets a profile of it only behind the airlock, as it gets
// the source: APP_READ, with no proxy block, is denied project-channels-need-airlock, APP_PROXIED is
// released, and a credential naming only the public project gets either. The proxy reads the
// limit off the served profile's ProjectChannels (requests' AirlockValues), so the profile carries
// it.
func TestAProfileOfAProjectChannelsSecretIsLimitedAsItsSource(t *testing.T) {
	app := policytest.TypeSecret("APP_KEY", policy.OwnerShared, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
	app.Tags[policy.TagProjectChannels] = policy.ProjectChannelsSlack
	set := loadProfiled(t, app)
	for _, name := range []string{"APP_READ", "APP_PROXIED"} {
		if got := set.Secrets[name].ProjectChannels; got != policy.ProjectChannelsSlack {
			t.Errorf("%s is served with project-channels %q; want its source's %q", name, got, policy.ProjectChannelsSlack)
		}
	}
	for _, c := range []struct {
		name, secret string
		requester    policy.Requester
		outcome      string
		reason       string
	}{
		{"a credential naming PRIV, with no proxy block", "APP_READ", credential(carol, "", privProject), policy.Deny, policy.ReasonProjectChannelsNeedAirlock},
		{"a credential naming PRIV, behind the proxy", "APP_PROXIED", credential(carol, "", privProject), policy.Automatic, ""},
		{"a credential naming the public project, with no proxy block", "APP_READ", credential(carol, "", pubProject), policy.Automatic, ""},
	} {
		got, err := set.Evaluate(c.secret, c.requester)
		if err != nil || got.Outcome != c.outcome || got.Reason != c.reason {
			t.Errorf("Evaluate(%s) for %s = %+v, %v; want %s %q", c.secret, c.name, got, err, c.outcome, c.reason)
		}
	}
}

// TestTaggingAProfilesSourceMovesTheVersion: a source served only through profiles of other names
// writes no line of its own into the version, so its project tags reach the version through its
// profiles alone. Tagging PRIV_KEY with a project, or APP_KEY with project-channels, must move the
// version as tagging a secret served by its own name does, so the airlock proxy reads again the
// values it holds (proxy's bound.go) and each live grant is evaluated again.
func TestTaggingAProfilesSourceMovesTheVersion(t *testing.T) {
	app := func(channels bool) secrets.LocalSecret {
		s := policytest.TypeSecret("APP_KEY", policy.OwnerShared, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
		if channels {
			s.Tags[policy.TagProjectChannels] = policy.ProjectChannelsSlack
		}
		return s
	}
	gone := projectSecret("GONE_KEY", policy.OwnerShared, policy.TierAgent, goneProject)
	plain := loadProfiled(t, policytest.Secret("PRIV_KEY", policy.OwnerShared, policy.TierAgent, "v"), gone, app(false))
	tagged := loadProfiled(t, projectSecret("PRIV_KEY", policy.OwnerShared, policy.TierAgent, privProject), gone, app(false))
	limited := loadProfiled(t, policytest.Secret("PRIV_KEY", policy.OwnerShared, policy.TierAgent, "v"), gone, app(true))
	if _, served := plain.Secrets["PRIV_KEY"]; served {
		t.Fatalf("PRIV_KEY is served by its own name; want it served only through PRIV_READ, so this row reads the profile's line")
	}
	if plain.Version == tagged.Version {
		t.Errorf("tagging PRIV_KEY, served only as PRIV_READ, with %s left the version at %s", privProject, plain.Version)
	}
	if plain.Version == limited.Version {
		t.Errorf("tagging APP_KEY, served only as its profiles, project-channels=slack left the version at %s", plain.Version)
	}
}
