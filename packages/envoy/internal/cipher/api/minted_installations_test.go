// packages/envoy/internal/cipher/api/minted_installations_test.go
// A profile served from a github-installations secret, over HTTP: installation tokens a minter
// outside the broker minted ahead of any request, which the broker hands over and never mints.
package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// mintedTokensValue is a github-installations value holding, for each of acme and ada, a token
// "ghs_stored_<account>" at contents and metadata read, which lapses life from now.
func mintedTokensValue(t *testing.T, life time.Duration) string {
	t.Helper()
	installations := map[string]any{}
	for i, account := range []string{"acme", "ada"} {
		installations[account] = map[string]any{
			"installation_id": 41 + i, "token": "ghs_stored_" + account,
			"expires_at":  time.Now().Add(life).UTC().Format(time.RFC3339),
			"permissions": map[string]string{"contents": "read", "metadata": "read"}, "repository_selection": "all",
		}
	}
	value, err := json.Marshal(map[string]any{"shapeVersion": 1, "installations": installations})
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

// newMintedServer is a test broker whose store holds APP_TOKENS, a github-installations secret
// of testApprover's at agent tier holding fifty-minute tokens of acme and ada, served as
// TOKENS_READ, a bare token of either at read, by the broker's own github-installations minter.
func newMintedServer(t *testing.T) *profileServer {
	t.Helper()
	file := policytest.ProfilesFile(`{"name": "TOKENS_READ", "source": "APP_TOKENS", "github": {"accounts": ["acme", "ada"], "permissions": {"contents": "read", "metadata": "read"}, "value": "token"}}`)
	s := &profileServer{}
	s.testServer = newProfiledTestServer(t, file, func(d *api.Deps) {
		d.Machine.Minters = map[string]requests.Minter{policy.TypeGitHubInstallations: secrets.GitHubInstallations{}.Mint}
	})
	s.Secrets.Put(policytest.TypeSecret("APP_TOKENS", testApprover, policy.TierAgent, policy.TypeGitHubInstallations, mintedTokensValue(t, 50*time.Minute)))
	if status, body := s.sessionReread(t, "TOKENS_READ"); status != http.StatusOK {
		t.Fatalf("reread TOKENS_READ = %d %s", status, body)
	}
	s.enrollmentID, s.key = s.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	return s
}

// TestATargetNamingRepositoriesOfAMintedInstallationsProfileIs400TargetRepositoriesRefused: POST
// /v1/requests answers a target naming a repository of a profile served from a
// github-installations secret 400 TARGET_REPOSITORIES_REFUSED, a code of its own, whose message
// tells the caller to name the account alone, and writes nothing. A target naming the account is
// granted at once, and POST /v1/grants/{id}/values hands over that account's stored token.
//
// The named revert: answer the refusal as the TARGET_REFUSED it wraps, and this fails on the code.
func TestATargetNamingRepositoriesOfAMintedInstallationsProfileIs400TargetRepositoriesRefused(t *testing.T) {
	s := newMintedServer(t)
	status, reply, body := s.askTargeted(t, "TOKENS_READ", "https://github.com/ada/proofs")
	if status != http.StatusBadRequest || reply.Code != "TARGET_REPOSITORIES_REFUSED" || !strings.Contains(reply.Error, "--target ada") {
		t.Fatalf("POST /v1/requests for TOKENS_READ --target ada/proofs = %d %s; want 400 TARGET_REPOSITORIES_REFUSED telling the caller --target ada", status, body)
	}
	if rows, records := s.pendingRows(t); rows != 0 || records != 0 {
		t.Fatalf("the refused target left %d request rows and %d records; want none", rows, records)
	}

	status, body = s.request(t, "TOKENS_READ", map[string]any{"request": signTargetRequest(t, s.key, s.URL, "TOKENS_READ", "https://github.com/ada")})
	created := decode[wireCreateRequestResponse](t, body)
	if status != http.StatusOK || created.GrantID == nil {
		t.Fatalf("POST /v1/requests for TOKENS_READ --target ada = %d %s; want an automatic grant", status, body)
	}
	status, body = s.values(t, *created.GrantID, nil)
	var released struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal(body, &released); err != nil || status != http.StatusOK || released.Values["TOKENS_READ"] != "ghs_stored_ada" {
		t.Fatalf("POST values = %d %s; want ada's stored token", status, body)
	}
}

// TestAReleaseOfAStoredTokenWithinTheMarginOfItsExpiryIs502MintFailed: a release whose stored
// token lapses within the 20-minute margin, because its minter has written no newer one, releases
// nothing and answers 502 MINT_FAILED naming the token's expiry and never the token, as every mint
// whose upstream gave no usable answer does.
func TestAReleaseOfAStoredTokenWithinTheMarginOfItsExpiryIs502MintFailed(t *testing.T) {
	s := newMintedServer(t)
	status, body := s.request(t, "TOKENS_READ", map[string]any{"request": signTargetRequest(t, s.key, s.URL, "TOKENS_READ", "https://github.com/ada")})
	created := decode[wireCreateRequestResponse](t, body)
	if status != http.StatusOK || created.GrantID == nil {
		t.Fatalf("POST /v1/requests for TOKENS_READ --target ada = %d %s; want an automatic grant", status, body)
	}
	s.Secrets.Put(policytest.TypeSecret("APP_TOKENS", testApprover, policy.TierAgent, policy.TypeGitHubInstallations, mintedTokensValue(t, 10*time.Minute)))
	status, body = s.values(t, *created.GrantID, nil)
	reply := decode[wireError](t, body)
	if status != http.StatusBadGateway || reply.Code != "MINT_FAILED" || !strings.Contains(reply.Error, "expires at") || strings.Contains(string(body), "ghs_stored_ada") {
		t.Fatalf("POST values over a token of ten minutes left = %d %s; want 502 MINT_FAILED naming its expiry and not the token", status, body)
	}
}
