package appauth

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// releaseProof is a stand-in of the Proof header a release request of `agent-secrets` carries: a
// compact JWS the session's helper signs for that one request, base64url in three parts.
var releaseProof = strings.Join([]string{
	base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","typ":"dpop+jwt"}`)),
	base64.RawURLEncoding.EncodeToString([]byte(`{"htm":"POST","htu":"/v1/grants"}`)),
	base64.RawURLEncoding.EncodeToString([]byte("a stand-in signature of the release")),
}, ".")

// TestEachBrokerPhraseTheDaemonDecidesOnSurvivesTheCLIsRule: mintFromCommand decides on the
// stderr of `agent-secrets`, which printed the broker's refusal as the rule reads it with the
// release request's credentials redacted (its Proof; a GitHub token among them for good measure).
// Each phrase it decides on (brokerNoInstallation, brokerNarrowingRefused) is held in the
// refusal the broker writes, run through that rule, and must still be there, or the decision
// would read every such refusal as a plain failure. The control is a credential that shares a run
// of eight characters with each phrase: the rule then cuts the phrase, so the check can fail.
func TestEachBrokerPhraseTheDaemonDecidesOnSurvivesTheCLIsRule(t *testing.T) {
	carried := []string{releaseProof, githubtest.InstallationToken("release")}
	for _, tc := range []struct {
		phrase, refusal, sharing string
	}{
		// packages/envoy/internal/cipher/secrets/githubapp.go, GitHubApp.Mint.
		{brokerNoInstallation, "release github-app secret LEGION_IMPLEMENT_APP: github app 123456 has no installation on acme-org (refused)", "eyJinstallationQ"},
		// packages/envoy/internal/cipher/secrets/minterr.go, ErrMintNarrowingRefused, as
		// requests/narrowing_check.go words it.
		{brokerNarrowingRefused, "the narrowing cannot be served: github app 123456: GitHub answered HTTP 422 to the narrowed token for acme-org: the installation does not hold the permissions asked for; the caller fixes the narrowing", "eyJnarrowingQ"},
	} {
		t.Run(tc.phrase, func(t *testing.T) {
			printed := oauth2calltest.Said(tc.refusal).WithoutTokens(carried...).String()
			if !strings.Contains(printed, tc.phrase) {
				t.Errorf("the CLI's rule prints %q, which no longer holds %q: mintFromCommand would not read the refusal", printed, tc.phrase)
			}
			cut := oauth2calltest.Said(tc.refusal).WithoutTokens(append(carried, tc.sharing)...).String()
			if strings.Contains(cut, tc.phrase) {
				t.Fatalf("control: a credential sharing a run of %q still leaves %q, so this check cannot fail", tc.phrase, cut)
			}
		})
	}
}
