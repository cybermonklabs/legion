package cipherclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/dpopproof/dpopprooftest"
)

// placeholderBroker is a broker's token endpoint that records each trade's form and whether it
// carried a DPoP proof, and answers each with answer.
func placeholderBroker(t *testing.T, status int, answer map[string]any) (*httptest.Server, *[]url.Values, *[]string) {
	t.Helper()
	var forms []url.Values
	var proofs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TokenPath || r.ParseForm() != nil {
			http.Error(w, "not the token endpoint", http.StatusNotFound)
			return
		}
		forms = append(forms, r.PostForm)
		proofs = append(proofs, r.Header.Get("DPoP"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(server.Close)
	return server, &forms, &proofs
}

// placeholderClient is a client of broker trading the run's token held in a file.
func placeholderClient(t *testing.T, broker string) *Client {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "run-token")
	if err := os.WriteFile(path, []byte("the-runs-oidc-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{BrokerURL: cipheraddr.New(broker), Key: key, Subject: NewWorkflowRunTokenFile(path), NoLauncherCredential: true})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// tradeProofRefusal is why proof is not a DPoP proof that a broker's token endpoint at tokenURL
// takes for a trade signed with key, or "" when it is one: dpopprooftest.TradeRefusal's checks,
// and the key the proof carries and is signed by is key.
func tradeProofRefusal(proof, tokenURL string, key *ecdsa.PublicKey) string {
	why, signer := dpopprooftest.TradeRefusal(proof, tokenURL)
	if why == "" && (signer == nil || !signer.Equal(key)) {
		return "it is signed by a key other than the client's"
	}
	return why
}

// c8Answer is the token endpoint's answer to a result entry's run asking for the placeholder.
func c8Answer(held string) map[string]any {
	return map[string]any{
		"access_token": held, "issued_token_type": oauthwire.TokenTypeProxyPlaceholder, "token_type": "N_A", "expires_in": 900,
		oauthwire.ExtraGrantID: "result:00000000-0000-4000-8000-000000000001",
		oauthwire.ExtraProxy: oauthwire.AirlockRelease{ProxyURL: "http://127.0.0.1:13381", CACertificate: "PEM", ProxyPassword: held,
			Names: map[string]oauthwire.AirlockName{oauthwire.AirlockResultsName: {Value: held, Slots: map[string]string{"value": held}, Hosts: []string{"dispatch.example.test"}}}},
	}
}

// TestAirlockPlaceholderAsksTheTokenEndpointForTheRunsPlaceholder: the run's trade asks for the
// airlock placeholder, declines the launcher credential and carries a DPoP proof of the client's
// key that the token endpoint takes (signed by that key, for a POST to the endpoint, just now);
// the grant it reads back is the placeholder, its grant, its end and the release. A refusal is a
// RefusedError, and an answer that is no placeholder - an access token - is refused.
func TestAirlockPlaceholderAsksTheTokenEndpointForTheRunsPlaceholder(t *testing.T) {
	const held = "agent-secrets-placeholder-result-QqRrSsTtUuVvWwXxYyZz0123456789ab"
	broker, forms, proofs := placeholderBroker(t, http.StatusOK, c8Answer(held))
	asked := time.Now()
	client := placeholderClient(t, broker.URL)
	grant, err := client.AirlockPlaceholder(t.Context())
	if err != nil {
		t.Fatalf("AirlockPlaceholder: %v", err)
	}
	form := (*forms)[0]
	if len(*forms) != 1 || form.Get("grant_type") != GrantTypeTokenExchange || form.Get("client_id") != oauthwire.ExchangeClientID ||
		form.Get("subject_token") != "the-runs-oidc-token" || form.Get("subject_token_type") != oauthwire.SubjectTokenTypeGitHub ||
		form.Get("requested_token_type") != oauthwire.TokenTypeProxyPlaceholder ||
		form.Get(oauthwire.FormLauncherCredential) != oauthwire.FormLauncherCredentialDeclined {
		t.Fatalf("the trade sent %v; want the run's token exchanged for the placeholder type and the credential declined", *forms)
	}
	if why := tradeProofRefusal((*proofs)[0], broker.URL+TokenPath, &client.Key().PublicKey); why != "" {
		t.Fatalf("the trade's DPoP proof %q is not one the token endpoint takes: %s", (*proofs)[0], why)
	}
	if grant.Placeholder != held || grant.GrantID != "result:00000000-0000-4000-8000-000000000001" ||
		grant.ExpiresAt.Before(asked.Add(899*time.Second)) || grant.Release.ProxyPassword != held ||
		grant.Release.Names[oauthwire.AirlockResultsName].Hosts[0] != "dispatch.example.test" {
		t.Fatalf("the grant read = %+v; want the placeholder, its grant, an end 900 s on and the release", grant)
	}

	refusing, _, _ := placeholderBroker(t, http.StatusBadRequest, map[string]any{"error": "invalid_request", "error_description": "no result entry lists this run"})
	var refused *RefusedError
	if _, err := placeholderClient(t, refusing.URL).AirlockPlaceholder(t.Context()); !errors.As(err, &refused) || refused.Code != "invalid_request" {
		t.Fatalf("AirlockPlaceholder against a refusal = %v; want a RefusedError naming invalid_request", err)
	}

	token := c8Answer(held)
	token["issued_token_type"], token["token_type"] = "urn:ietf:params:oauth:token-type:access_token", "DPoP"
	minting, _, _ := placeholderBroker(t, http.StatusOK, token)
	if grant, err := placeholderClient(t, minting.URL).AirlockPlaceholder(t.Context()); err == nil {
		t.Fatalf("AirlockPlaceholder against an access token's answer = %+v; want it refused as no placeholder", grant)
	}
}
