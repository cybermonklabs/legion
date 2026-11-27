package requests

import (
	"context"
	"maps"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// A grant ends for good when a release finds its secret's type changed or its lifetime over the
// current max-grant (TestATypeChangedAwayAndBackLeavesTheGrantEnded and
// TestAMaxGrantLoweredBelowAGrantAndRaisedBackLeavesItEnded pin that, with a release between the
// change and its revert). A change reverted before any release finds it leaves the grant as it
// was: the grant was approved for that secret, type and source, and on its next release it mints
// a token of that same type from that same frozen source, never the stored value. To end a grant,
// revoke it. The tests below pin the revert with no release between.

// TestATypeChangedAwayAndBackWithNoReleaseBetweenLeavesTheGrantAsItWas: a type retag reverted
// before any release leaves the grant live, and its next release goes through the minter of its
// original type, returning that minter's token and never the stored value.
func TestATypeChangedAwayAndBackWithNoReleaseBetweenLeavesTheGrantAsItWas(t *testing.T) {
	for name, away := range map[string]string{"removed": "", "changed": policy.TypeGoogleLogin} {
		t.Run(name, func(t *testing.T) {
			machine, enrollmentID, key, _ := newAppFixture(t, "QUIET_TYPE_KEY")
			secret := requestTypeSecret("QUIET_TYPE_KEY", policy.TypeGitHubApp, policy.TierAgent, "")
			retag(t, machine, secret)
			var githubCalls, googleCalls int
			var minted []string
			machine.Minters = map[string]Minter{
				policy.TypeGitHubApp: func(_ context.Context, req secrets.MintRequest) (secrets.Minted, error) {
					githubCalls++
					minted = append(minted, req.Value)
					return secrets.Minted{Value: "github-installation-token"}, nil
				},
				policy.TypeGoogleLogin: func(context.Context, secrets.MintRequest) (secrets.Minted, error) {
					googleCalls++
					return secrets.Minted{Value: "google-access-token"}, nil
				},
			}
			grant, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read App key", "QUIET_TYPE_KEY"), "")
			if err != nil || grant.GrantID == nil {
				t.Fatalf("Create = %+v, %v", grant, err)
			}
			if release, err := machine.Values(t.Context(), *grant.GrantID, enrollmentID, ReleaseCall{}); err != nil || release.Values["QUIET_TYPE_KEY"] != "github-installation-token" {
				t.Fatalf("Values before the retag = %v, %v; want the GitHub App minter's token", release.Values, err)
			}
			moved := secret
			moved.Tags = maps.Clone(secret.Tags)
			if away == "" {
				delete(moved.Tags, policy.TagType)
			} else {
				moved.Tags[policy.TagType] = away
				// The value the away type's decoder reads, so the policy serves the secret as
				// that type while it is away, rather than refusing it for its value.
				moved.Value = policytest.TypeValue(away)
			}
			retag(t, machine, moved)
			retag(t, machine, secret)
			release, err := machine.Values(t.Context(), *grant.GrantID, enrollmentID, ReleaseCall{})
			if err != nil || release.Values["QUIET_TYPE_KEY"] != "github-installation-token" {
				t.Fatalf("Values once the type is back, with no release while it was away = %v, %v; want the GitHub App minter's token: the grant is as it was", release.Values, err)
			}
			if release.Values["QUIET_TYPE_KEY"] == secret.Value {
				t.Fatal("the release returned the stored value")
			}
			if githubCalls != 2 || googleCalls != 0 {
				t.Fatalf("minter calls: github-app %d, google-login %d; want 2 and 0: the grant mints its original type", githubCalls, googleCalls)
			}
			for _, value := range minted {
				if value != secret.Value {
					t.Fatal("the GitHub App minter was handed another value than the secret's stored value, read from the grant's frozen source")
				}
			}
			if by, audits := endedForGood(t, machine, *grant.GrantID); by != "" || audits != 0 {
				t.Fatalf("grant revoked_by %q with %d grant.revoked rows; want it live", by, audits)
			}
		})
	}
}

// TestAMaxGrantLoweredAndRaisedWithNoReleaseBetweenLeavesTheGrantAsItWas: a max-grant lowered
// below a live grant's lifetime and raised back before any release leaves that grant live, and its
// next release mints its type's token, never the stored value.
func TestAMaxGrantLoweredAndRaisedWithNoReleaseBetweenLeavesTheGrantAsItWas(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	var calls int
	machine.Minters = map[string]Minter{
		policy.TypeGoogleLogin: func(_ context.Context, req secrets.MintRequest) (secrets.Minted, error) {
			calls++
			if req.Value != policytest.TypeValue(policy.TypeGoogleLogin) {
				t.Error("the Google login minter was handed another value than the secret's stored value")
			}
			return secrets.Minted{Value: "google-access-token"}, nil
		},
	}
	retag(t, machine, requestTypeSecret("QUIET_BOUNDED_LOGIN", policy.TypeGoogleLogin, policy.TierAgent, "1h"))
	long, err := machine.Create(t.Context(), enrollmentID, signRequestFor(t, machine, key, 60*60, "QUIET_BOUNDED_LOGIN"), "")
	if err != nil || long.GrantID == nil {
		t.Fatalf("Create(1h) = %+v, %v", long, err)
	}
	retag(t, machine, requestTypeSecret("QUIET_BOUNDED_LOGIN", policy.TypeGoogleLogin, policy.TierAgent, "30m"))
	retag(t, machine, requestTypeSecret("QUIET_BOUNDED_LOGIN", policy.TypeGoogleLogin, policy.TierAgent, "1h"))
	release, err := machine.Values(t.Context(), *long.GrantID, enrollmentID, ReleaseCall{})
	if err != nil || release.Values["QUIET_BOUNDED_LOGIN"] != "google-access-token" {
		t.Fatalf("Values(1h) once max-grant is 1h again, with no release while it was 30m = %v, %v; want the Google login minter's token: the grant is as it was", release.Values, err)
	}
	if calls != 1 {
		t.Fatalf("minter calls = %d; want 1", calls)
	}
	if by, audits := endedForGood(t, machine, *long.GrantID); by != "" || audits != 0 {
		t.Fatalf("grant revoked_by %q with %d grant.revoked rows; want it live", by, audits)
	}
}
