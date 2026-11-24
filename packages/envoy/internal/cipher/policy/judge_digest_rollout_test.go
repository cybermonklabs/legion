package policy_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// TestAProfileKeepsItsDigestWhenItsProxyStringMovesToItsSourcesTag: a profile served behind the
// kind its proxy string names keeps its digest through the rollout that moves the kind to
// its source's proxy tag: tag the key with the same kind, drop the string from the file, roll. Each
// request records the digest of the profile it was decided under (request_secrets.airlock_digest),
// and checkRelease ends a grant whose digest moved (machine_read.go, profileChanged), so a digest
// that moved with the string would end every live grant of the profile at the deploy, as no change
// of where its credential may go calls for. The judged rows hold it with the source's judge tag in
// the digest too. The control row moves the kind instead, and the digest moves with it.
func TestAProfileKeepsItsDigestWhenItsProxyStringMovesToItsSourcesTag(t *testing.T) {
	served := func(t *testing.T, tag, judge, profile string) string {
		t.Helper()
		source := policytest.Secret("AIRLOCK_JUDGE_PROBE", policy.OwnerShared, policy.TierHuman, "v")
		if tag != "" {
			source = policytest.Proxied(source, tag)
		}
		if judge != "" {
			source.Tags[policy.TagJudge] = judge
			source.Tags[policy.TagGuidance] = "grant a reason that holds k14-grant"
		}
		set := policytest.ProfiledCurrent(t, awstest.NewLocal(source), policytest.KindsFile(profile)).Get()
		secret, ok := set.Secrets["AIRLOCK_JUDGE_PROBE"]
		if !ok {
			t.Fatalf("AIRLOCK_JUDGE_PROBE was not served: %+v", set.Refused)
		}
		if secret.Proxy != "github" && tag != "slack" {
			t.Fatalf("AIRLOCK_JUDGE_PROBE is served behind %q; want github", secret.Proxy)
		}
		return secret.ProfileDigest
	}
	const (
		withString = `{"name": "AIRLOCK_JUDGE_PROBE", "proxy": "github"}`
		bare       = `{"name": "AIRLOCK_JUDGE_PROBE"}`
	)
	for _, judge := range []string{"", "900"} {
		t.Run("judge "+judge, func(t *testing.T) {
			before := served(t, "", judge, withString)
			if after := served(t, "github", judge, bare); after != before {
				t.Errorf("the profile is served with digest %s once its string is its source's tag, want %s, the digest it had behind its string", after, before)
			}
			if both := served(t, "github", judge, withString); both != before {
				t.Errorf("the profile is served with digest %s while its string and its source's tag name one kind, want %s", both, before)
			}
		})
	}
	t.Run("the control: a source tagged another kind moves the digest", func(t *testing.T) {
		if moved := served(t, "slack", "", bare); moved == served(t, "", "", withString) {
			t.Error("a profile behind slack is served with the digest it had behind github")
		}
	})
}
