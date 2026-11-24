package policy_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// channelsSecret is a namespace secret carrying the project-channels tag, owned by owner at tier.
func channelsSecret(name, owner, tier, value string) memory.Secret {
	s := policytest.Secret(name, owner, tier, "v")
	s.Tags[policy.TagProjectChannels] = value
	return s
}

// TestProjectChannelsTagIsLoaded: the tag's one value is slack, read into Secret.ProjectChannels;
// any other value, the empty tag and the word none included, refuses the secret
// project-channels-malformed, since a value the broker cannot read names no limit it could apply.
func TestProjectChannelsTagIsLoaded(t *testing.T) {
	store := memory.New(
		channelsSecret("SLACK_TOKEN", policy.OwnerShared, policy.TierAgent, policy.ProjectChannelsSlack),
		channelsSecret("UPPER_TOKEN", policy.OwnerShared, policy.TierAgent, "SLACK"),
		channelsSecret("NONE_TOKEN", policy.OwnerShared, policy.TierAgent, "none"),
		channelsSecret("EMPTY_TOKEN", policy.OwnerShared, policy.TierAgent, ""),
		channelsSecret("OTHER_TOKEN", policy.OwnerShared, policy.TierAgent, "discord"),
		policytest.Secret("PLAIN_TOKEN", policy.OwnerShared, policy.TierAgent, "v"),
	)
	set := loadProjects(t, store)
	if got := set.Secrets["SLACK_TOKEN"].ProjectChannels; got != policy.ProjectChannelsSlack {
		t.Errorf("SLACK_TOKEN's ProjectChannels = %q, want %q", got, policy.ProjectChannelsSlack)
	}
	if got := set.Secrets["PLAIN_TOKEN"].ProjectChannels; got != "" {
		t.Errorf("PLAIN_TOKEN's ProjectChannels = %q, want none", got)
	}
	for _, name := range []string{"UPPER_TOKEN", "NONE_TOKEN", "EMPTY_TOKEN", "OTHER_TOKEN"} {
		want := policy.Refusal{Reason: policy.ReasonProjectChannelsMalformed, Owner: policy.OwnerShared}
		if got := set.Refused[policytest.ID(name)]; got != want {
			t.Errorf("%s refused as %+v, want %+v", name, got, want)
		}
	}
}

// TestProjectChannelsTagMovesTheVersionOnlyWhenPresent pins the digest's bytes for the
// project-channels tag: a secret that carries it ends its line with "\tproject-channels=slack",
// after the project field when it carries that too, so a tag a console or an infra-applier adds
// moves the version, and the airlock proxy reads the values it holds again (proxy's
// bound.go). A secret without it writes the line it wrote before the tag existed, so no deployed
// version moves.
func TestProjectChannelsTagMovesTheVersionOnlyWhenPresent(t *testing.T) {
	const arn = "arn:aws:secretsmanager:local:000000000000:secret:example/agent-secrets/"
	digest := func(lines string) string {
		sum := sha256.Sum256([]byte(lines))
		return hex.EncodeToString(sum[:])
	}
	for name, c := range map[string]struct {
		secret         policy.Secret
		untagged, line string
	}{
		"a plain secret": {
			secret:   policy.Secret{Name: "SLACK_TOKEN", Owner: policy.OwnerShared, Tier: policy.TierAgent, Source: arn + "slack-token"},
			untagged: "SLACK_TOKEN\tshared\tagent\t" + arn + "slack-token\n",
			line:     "SLACK_TOKEN\tshared\tagent\t" + arn + "slack-token\t0s\t\tproject-channels=slack\n",
		},
		"a secret with a max-grant": {
			secret:   policy.Secret{Name: "SLACK_TOKEN", Owner: policy.OwnerShared, Tier: policy.TierAgent, Source: arn + "slack-token", MaxGrant: time.Hour},
			untagged: "SLACK_TOKEN\tshared\tagent\t" + arn + "slack-token\t1h0m0s\t\n",
			line:     "SLACK_TOKEN\tshared\tagent\t" + arn + "slack-token\t1h0m0s\t\tproject-channels=slack\n",
		},
		"a session-default secret of a project": {
			secret:   policy.Secret{Name: "SLACK_TOKEN", Owner: policy.OwnerShared, Tier: policy.TierAgent, Source: arn + "slack-token", SessionDefault: true, Project: privProject},
			untagged: "SLACK_TOKEN\tshared\tagent\t" + arn + "slack-token\t0s\t\tdefault-grant=session\tproject=PRIV\n",
			line:     "SLACK_TOKEN\tshared\tagent\t" + arn + "slack-token\t0s\t\tdefault-grant=session\tproject=PRIV\tproject-channels=slack\n",
		},
		// The cases with optional fields before the project fields, one in each of NewSet's two
		// forms a tagged secret takes: requesters, scopes and admin are written first, then
		// project, then project-channels.
		"a secret with requesters, scopes, admin, a project and a max-grant": {
			secret:   policy.Secret{Name: "SLACK_TOKEN", Owner: policy.OwnerShared, Tier: policy.TierHuman, Source: arn + "slack-token", Requesters: "carol@example.com", Scopes: "cloud-platform", Admin: true, MaxGrant: time.Hour, Project: privProject},
			untagged: "SLACK_TOKEN\tshared\thuman\t" + arn + "slack-token\t1h0m0s\t\trequesters=carol@example.com\tscopes=cloud-platform\tadmin=true\tproject=PRIV\n",
			line:     "SLACK_TOKEN\tshared\thuman\t" + arn + "slack-token\t1h0m0s\t\trequesters=carol@example.com\tscopes=cloud-platform\tadmin=true\tproject=PRIV\tproject-channels=slack\n",
		},
		"a session-default secret with requesters, scopes, admin, a project and a max-grant": {
			secret:   policy.Secret{Name: "SLACK_TOKEN", Owner: policy.OwnerShared, Tier: policy.TierHuman, Source: arn + "slack-token", SessionDefault: true, Requesters: "carol@example.com", Scopes: "cloud-platform", Admin: true, MaxGrant: time.Hour, Project: privProject},
			untagged: "SLACK_TOKEN\tshared\thuman\t" + arn + "slack-token\t1h0m0s\t\tdefault-grant=session\trequesters=carol@example.com\tscopes=cloud-platform\tadmin=true\tproject=PRIV\n",
			line:     "SLACK_TOKEN\tshared\thuman\t" + arn + "slack-token\t1h0m0s\t\tdefault-grant=session\trequesters=carol@example.com\tscopes=cloud-platform\tadmin=true\tproject=PRIV\tproject-channels=slack\n",
		},
	} {
		untagged := policy.NewSet(map[string]policy.Secret{c.secret.Name: c.secret})
		if want := digest(c.untagged); untagged.Version != want {
			t.Errorf("%s with no project-channels tag: Version = %s, want %s, the digest of %q", name, untagged.Version, want, c.untagged)
		}
		tagged := c.secret
		tagged.ProjectChannels = policy.ProjectChannelsSlack
		set := policy.NewSet(map[string]policy.Secret{tagged.Name: tagged})
		if want := digest(c.line); set.Version != want {
			t.Errorf("%s tagged project-channels=slack: Version = %s, want %s, the digest of %q", name, set.Version, want, c.line)
		}
		if set.Version == untagged.Version {
			t.Errorf("%s: tagging it project-channels=slack left the version at %s", name, set.Version)
		}
	}
}

// TestAProjectChannelsSecretOutsideTheAirlockIsDeniedToAPrivateProject: such a secret served
// behind no kind is denied, project-channels-need-airlock, to a credential that names a private
// project, because a release outside the airlock hands the session the real token and the session
// could then post anywhere. The same secret behind the airlock - served as a profile of its own
// name behind the kind its proxy string names, or by its own name under its proxy tag - and the
// same secret asked for by a credential that names only a public project, follow owner and tier
// as before.
func TestAProjectChannelsSecretOutsideTheAirlockIsDeniedToAPrivateProject(t *testing.T) {
	store := memory.New(channelsSecret("SLACK_TOKEN", policy.OwnerShared, policy.TierAgent, policy.ProjectChannelsSlack))
	loader := projectLoader(store)
	bare, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loader.Profiles = policytest.Profiles(t, policytest.KindsFile(`{"name": "SLACK_TOKEN", "proxy": "slack"}`), projectSvc)
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("policy.NewCurrent with a proxy string: %v", err)
	}
	behind := current.Get()
	tagged := projectLoader(memory.New(policytest.Proxied(channelsSecret("SLACK_TOKEN", policy.OwnerShared, policy.TierAgent, policy.ProjectChannelsSlack), "slack")))
	tagged.Profiles = policytest.Profiles(t, policytest.KindsFile(), projectSvc)
	underTag, err := tagged.Load(context.Background())
	if err != nil {
		t.Fatalf("Load of the tagged secret: %v", err)
	}
	source := policytest.Source("SLACK_TOKEN")
	for _, c := range []struct {
		name      string
		set       *policy.Set
		requester policy.Requester
		want      policy.Decision
	}{
		{"outside the airlock, a credential naming the private project", bare, credential(carol, "", privProject),
			policy.Decision{Outcome: policy.Deny, Reason: policy.ReasonProjectChannelsNeedAirlock, Source: source}},
		{"outside the airlock, a credential naming the public project", bare, credential(carol, "", pubProject),
			policy.Decision{Outcome: policy.Automatic, Source: source}},
		{"outside the airlock, a credential naming no project", bare, credential(carol, ""),
			policy.Decision{Outcome: policy.Automatic, Source: source}},
		{"behind the airlock by a proxy string, a credential naming the private project", behind, credential(carol, "", privProject),
			policy.Decision{Outcome: policy.Automatic, Source: source}},
		{"behind the airlock by its proxy tag, a credential naming the private project", underTag, credential(carol, "", privProject),
			policy.Decision{Outcome: policy.Automatic, Source: source}},
	} {
		got, err := c.set.Evaluate("SLACK_TOKEN", c.requester)
		if err != nil || got != c.want {
			t.Errorf("Evaluate(SLACK_TOKEN) %s = %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
}

// TestListsPrivateReadsTheCredentialsOwnProjects: a requester is limited by the projects its
// credential names, read before the visibility filter, and not by the projects of the secret it
// asks for. So a session naming a private project is limited while asking for a secret carrying no
// dispatch-project tag at all, and stays limited once its person can no longer see that project,
// which is exactly when the limit must not fall away.
func TestListsPrivateReadsTheCredentialsOwnProjects(t *testing.T) {
	for _, c := range []struct {
		name      string
		requester policy.Requester
		want      bool
	}{
		{"names the private project, and may see it", policy.Requester{Named: []string{privProject}, Projects: []string{privProject}, ProjectStates: states}, true},
		{"names the private project, and may no longer see it", policy.Requester{Named: []string{privProject}, ProjectStates: states}, true},
		{"names the public project alone", policy.Requester{Named: []string{pubProject}, Projects: []string{pubProject}, ProjectStates: states}, false},
		{"names a project Dispatch does not hold", policy.Requester{Named: []string{goneProject}, ProjectStates: states}, false},
		{"names no project", policy.Requester{ProjectStates: states}, false},
	} {
		if got := c.requester.ListsPrivate(); got != c.want {
			t.Errorf("a requester that %s: ListsPrivate() = %v, want %v", c.name, got, c.want)
		}
	}
}
