package requests

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestAProfileChangedRefusalNamesTheSourcesTagsItsFingerprintHolds: a profile behind the proxy
// carries its source key's judge seconds in its fingerprint, and a profile its source's proxy tag
// puts behind a kind carries that kind, so a grant of it that its new tags still admit (a
// 300-second grant once the judge tag goes from 900 to 600, or the same grant once the proxy tag
// names another kind) ends as a profile changed. The person changed a tag, not the file, so where
// a tag can be what moved, the refusal names it beside the entry: the proxy tag where the source's
// tag, not the profile's own proxy string, puts it behind its kind, and the judge tag where the
// source carries one now or the judge decided the grant, so it carried one then. A stored secret
// served by its own name behind its own proxy tag names that tag. A person's grant of a profile
// behind its own proxy string whose source carries no judge tag, a profile behind no kind, and a
// secret served by its own name behind no kind read as before.
func TestAProfileChangedRefusalNamesTheSourcesTagsItsFingerprintHolds(t *testing.T) {
	t.Parallel()
	judgedSource := policytest.Judged(policytest.Secret("RELEASE_PAT", fixtureOperator, policy.TierHuman, "v"), 600, "")
	plainSource := policytest.Secret("PLAIN_PAT", fixtureOperator, policy.TierHuman, "v")
	taggedSource := policytest.Proxied(policytest.Secret("TAGGED_PAT", fixtureOperator, policy.TierHuman, "v"), "github")
	taggedJudgedSource := policytest.Proxied(policytest.Judged(policytest.Secret("TAGGED_JUDGED_PAT", fixtureOperator, policy.TierHuman, "v"), 600, ""), "github")
	stored := policytest.Secret("PLAIN_KEY", fixtureOperator, policy.TierHuman, "v")
	taggedStored := policytest.Proxied(policytest.Secret("TAGGED_KEY", fixtureOperator, policy.TierHuman, "v"), "example")
	app := policytest.TypeSecret("APP_KEY", fixtureOperator, policy.TierHuman, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
	set := policytest.ProfiledCurrent(t, secrets.NewLocal(judgedSource, plainSource, taggedSource, taggedJudgedSource, stored, taggedStored, app), policytest.KindsFile(
		`{"name": "RELEASE_READ", "source": "RELEASE_PAT", "proxy": "github"}`,
		`{"name": "PLAIN_READ", "source": "PLAIN_PAT", "proxy": "github"}`,
		`{"name": "TAGGED_READ", "source": "TAGGED_PAT"}`,
		`{"name": "TAGGED_JUDGED_READ", "source": "TAGGED_JUDGED_PAT"}`,
		`{"name": "APP_READ", "source": "APP_KEY", "github": {"accounts": ["acme"], "permissions": {"metadata": "read"}}}`,
	)).Get()
	const changed = "'s profile changed since this grant was decided"
	const entry = changed + ": its entry in the profiles file, or the "
	judge := record.JudgeActor("the-model")
	for _, c := range []struct {
		row, name, decidedBy, want string
	}{
		{"behind its own proxy string, a source tagged judge now, a person's grant", "RELEASE_READ", fixtureOperator, "RELEASE_READ" + entry + "judge tag of its source key RELEASE_PAT"},
		{"behind its own proxy string, a source with no judge tag now, the judge's grant", "PLAIN_READ", judge, "PLAIN_READ" + entry + "judge tag of its source key PLAIN_PAT"},
		{"behind its own proxy string, a source with no judge tag now, a person's grant", "PLAIN_READ", fixtureOperator, "PLAIN_READ" + changed},
		{"behind its source's proxy tag, a person's grant", "TAGGED_READ", fixtureOperator, "TAGGED_READ" + entry + "proxy tag of its source key TAGGED_PAT"},
		{"behind its source's proxy tag, a source tagged judge now, a person's grant", "TAGGED_JUDGED_READ", fixtureOperator, "TAGGED_JUDGED_READ" + entry + "proxy or judge tag of its source key TAGGED_JUDGED_PAT"},
		{"behind no kind, the judge's grant", "APP_READ", judge, "APP_READ" + changed},
		{"a secret served by its own name behind its own proxy tag", "TAGGED_KEY", judge, "TAGGED_KEY" + changed + ": its entry in the profiles file, or its own proxy tag"},
		{"a secret served by its own name behind no kind, the judge's grant", "PLAIN_KEY", judge, "PLAIN_KEY" + changed},
	} {
		if _, served := set.Secrets[c.name]; !served {
			t.Fatalf("%s was not served: %+v", c.name, set.Refused)
		}
		err := profileChanged(set, c.name, c.decidedBy)
		if !errors.Is(err, ErrGrantNotLive) || !strings.HasSuffix(err.Error(), c.want) {
			t.Errorf("%s: profileChanged(%s) = %v; want an ErrGrantNotLive ending %q", c.row, c.name, err, c.want)
		}
	}
}
