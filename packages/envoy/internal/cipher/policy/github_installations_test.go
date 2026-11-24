package policy_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// tokensSourceName is a github-installations secret of the accounts acme and ada
// (policytest.FixtureGitHubOwners): installation tokens a minter outside the broker writes.
const tokensSourceName = "APP_TOKENS"

// tokensSource is tokensSourceName holding a value the decoder reads.
func tokensSource() memory.Secret {
	return policytest.TypeSecret(tokensSourceName, appOwner, policy.TierAgent, policy.TypeGitHubInstallations, policytest.TypeValue(policy.TypeGitHubInstallations))
}

// TestAGitHubInstallationsSourceIsServedOnlyThroughProfilesOfItsAccounts: a github-installations
// secret is served as a github-app secret is, only through profiles with a github block on the
// accounts its github-owners tag names, and never by its own name: a profile of an account the
// tag does not name is refused, and so is one with no github block. A profile may name
// repositories, which a release holds to the repositories the stored token reaches
// (secrets.GitHubInstallations), since the load reads no token's repositories.
//
// The named revert: leave the type out of the GitHub token types (policy.ServesGitHubTokens),
// and the github block rows are refused as a github block on a source of another type.
func TestAGitHubInstallationsSourceIsServedOnlyThroughProfilesOfItsAccounts(t *testing.T) {
	const tokensRead = `{"name": "TOKENS_READ", "source": "APP_TOKENS", "github": {"accounts": ["acme", "ada"], "permissions": {"contents": "read", "metadata": "read"}, "value": "token"}}`
	cur, _ := profiled(t, []memory.Secret{tokensSource()},
		tokensRead,
		`{"name": "OUTSIDE_READ", "source": "APP_TOKENS", "github": {"accounts": ["example-org"], "permissions": {"metadata": "read"}}}`,
		`{"name": "REPOS_READ", "source": "APP_TOKENS", "github": {"accounts": ["ada"], "repositories": ["ada/proofs"], "permissions": {"metadata": "read"}}}`,
		`{"name": "BARE_TOKENS", "source": "APP_TOKENS", `+apiProxy+`}`,
	)
	set := cur.Get()
	for _, name := range []string{"TOKENS_READ", "REPOS_READ"} {
		if served := servedProfile(t, set, name); served.Type() != policy.TypeGitHubInstallations {
			t.Errorf("%s = type %q; want github-installations", name, served.Type())
		}
	}
	if accounts := set.Secrets["TOKENS_READ"].GitHubOwners; accounts != "acme ada" {
		t.Errorf("TOKENS_READ's accounts = %q; want acme and ada", accounts)
	}
	for _, row := range []struct{ name, detail string }{
		{"OUTSIDE_READ", "example-org"},
		{"BARE_TOKENS", "no github block on a github-installations source"},
	} {
		if refusal := refusedProfile(t, set, row.name, policy.ReasonProfileBlockMismatch); !strings.Contains(refusal.Detail, row.detail) {
			t.Errorf("%s's refusal = %+v; want its detail to name %q", row.name, refusal, row.detail)
		}
	}
	if _, err := set.Evaluate(tokensSourceName, policy.Requester{Operator: appOwner}); !errors.Is(err, policy.ErrServedOnlyAsProfiles) {
		t.Errorf("Evaluate(%s) = %v; want ErrServedOnlyAsProfiles, the source served through its profiles alone", tokensSourceName, err)
	}
}

// TestAGitHubInstallationsSecretIsRefusedWithNoProfileNoOwnersOrAValueTheDecoderRefuses: a
// github-installations secret no profile names is refused by its own id with a reason of its own
// (ReasonGitHubInstallationsNoProfile), since every GitHub token the broker hands over is a
// profile's; one with no github-owners tag is refused github-owners-missing; and one whose value is
// no installation tokens the decoder reads is refused value-malformed, so no profile of it serves.
func TestAGitHubInstallationsSecretIsRefusedWithNoProfileNoOwnersOrAValueTheDecoderRefuses(t *testing.T) {
	id := policytest.ID(tokensSourceName)
	t.Run("no profile", func(t *testing.T) {
		cur, _ := profiled(t, []memory.Secret{tokensSource(), appSource()}, readProfile)
		if refusal := cur.Get().Refused[id]; refusal.Reason != policy.ReasonGitHubInstallationsNoProfile {
			t.Fatalf("Refused[%s] = %+v; want %s", id, refusal, policy.ReasonGitHubInstallationsNoProfile)
		}
	})
	t.Run("no github-owners tag", func(t *testing.T) {
		source := tokensSource()
		delete(source.Tags, policy.TagGitHubOwners)
		cur, _ := profiled(t, []memory.Secret{source}, `{"name": "TOKENS_READ", "source": "APP_TOKENS", `+acmeRead+`}`)
		if refusal := cur.Get().Refused[id]; refusal.Reason != policy.ReasonGitHubOwnersMissing {
			t.Fatalf("Refused[%s] = %+v; want %s", id, refusal, policy.ReasonGitHubOwnersMissing)
		}
	})
	t.Run("a value the decoder refuses", func(t *testing.T) {
		source := policytest.TypeSecret(tokensSourceName, appOwner, policy.TierAgent, policy.TypeGitHubInstallations, `{"shapeVersion": 1, "installations": {}}`)
		cur, _ := profiled(t, []memory.Secret{source}, `{"name": "TOKENS_READ", "source": "APP_TOKENS", `+acmeRead+`}`)
		set := cur.Get()
		if refusal := set.Refused[id]; refusal.Reason != policy.ReasonValueMalformed {
			t.Fatalf("Refused[%s] = %+v; want %s", id, refusal, policy.ReasonValueMalformed)
		}
		refusedProfile(t, set, "TOKENS_READ", policy.ReasonProfileSourceMissing)
	})
}

// TestALoadOfAValueOfAnotherShapeVersionNamesTheVersionItReadAndTheOnesItKnows: the load reads
// each type secret's value before anybody asks for it, so a github-installations value whose
// shapeVersion is not 1 is refused there, value-malformed, and the refusal's detail is the
// decoder's words: the version it read and the versions it knows. A profile of it is refused
// profile-source-missing with the same words, so the session that asks for the profile is told why.
//
// The named revert: drop the decoder's words from the value refusal (withhold), and neither detail
// names the version.
func TestALoadOfAValueOfAnotherShapeVersionNamesTheVersionItReadAndTheOnesItKnows(t *testing.T) {
	value := strings.Replace(policytest.GitHubInstallationsValue(), `"shapeVersion":1`, `"shapeVersion":2`, 1)
	if !strings.Contains(value, `"shapeVersion":2`) {
		t.Fatalf("the fixture value names no shapeVersion 1 to replace: %d bytes", len(value))
	}
	source := policytest.TypeSecret(tokensSourceName, appOwner, policy.TierAgent, policy.TypeGitHubInstallations, value)
	cur, _ := profiled(t, []memory.Secret{source}, `{"name": "TOKENS_READ", "source": "APP_TOKENS", `+acmeRead+`}`)
	set := cur.Get()
	refusal := set.Refused[policytest.ID(tokensSourceName)]
	profile := refusedProfile(t, set, "TOKENS_READ", policy.ReasonProfileSourceMissing)
	for _, detail := range []string{refusal.Detail, profile.Detail} {
		if refusal.Reason != policy.ReasonValueMalformed || !strings.Contains(detail, "shapeVersion 2") || !strings.Contains(detail, "[1]") || strings.Contains(detail, "fixture-token") {
			t.Errorf("the source's refusal %+v, and the profile's detail %q; want value-malformed naming shapeVersion 2 and [1], and no token", refusal, detail)
		}
	}
}

// TestAProfileTakesAStoredSecretsNameOnlyWhereThatSecretIsSupersededByItsSource: a profile named
// for a stored secret other than its source is refused profile-name-taken, unless that stored
// secret carries superseded-by whose value is exactly the profile's own source. Then the profile
// is served from its source under that name, and the stored secret is not served by its name. The
// stored secret here is a github-app key, the profile the github-installations tokens a minter
// mints with that key: the cutover from the one to the other under one name. The tag is honoured
// while the key is refused too, its value unreadable or its version gone, so retiring the key
// leaves the profile served.
//
// The named reverts: drop the superseded-by arm, and the exact value is refused profile-name-taken;
// read the tag of a served key alone, and the two retired keys' rows are refused.
func TestAProfileTakesAStoredSecretsNameOnlyWhereThatSecretIsSupersededByItsSource(t *testing.T) {
	const takeover = `{"name": "LEGION_IMPLEMENT_APP", "source": "APP_TOKENS", "github": {"accounts": ["acme"], "permissions": {"contents": "read", "metadata": "read"}}}`
	for _, row := range []struct {
		name       string
		annotation *string
		value      *string // the key's value; nil for one the decoder reads
		served     bool
	}{
		{"superseded-by the profile's own source", new("APP_TOKENS"), nil, true},
		{"superseded-by another name", new("OTHER_TOKENS"), nil, false},
		{"superseded-by in another case", new("app_tokens"), nil, false},
		{"no superseded-by", nil, nil, false},
		{"superseded-by the source, of a key whose value is unreadable", new("APP_TOKENS"), new("{}"), true},
		{"superseded-by the source, of a key with no current value", new("APP_TOKENS"), new(""), true},
		{"superseded-by another name, of a key with no current value", new("OTHER_TOKENS"), new(""), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			key := appSource() // LEGION_IMPLEMENT_APP, a github-app key
			if row.annotation != nil {
				key.Tags[policy.TagSupersededBy] = *row.annotation
			}
			if row.value != nil {
				key.Value = *row.value
			}
			cur, _ := profiled(t, []memory.Secret{key, tokensSource()}, takeover)
			set := cur.Get()
			if !row.served {
				refusedProfile(t, set, appSourceName, policy.ReasonProfileNameTaken)
				return
			}
			served := servedProfile(t, set, appSourceName)
			if served.Type() != policy.TypeGitHubInstallations || served.Profile.Source != tokensSourceName {
				t.Fatalf("%s = %+v; want the profile of APP_TOKENS, a github-installations secret", appSourceName, served)
			}
			if refusal, refused := set.Refused[policytest.ID(appSourceName)]; refused && (row.value == nil || refusal.Reason == policy.ReasonProfileNameTaken) {
				t.Errorf("the superseded stored secret is refused %+v; want it out of the way, its name the profile's", refusal)
			}
		})
	}
}
