package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An issuer with a user part (DISPATCH_OIDC_ISSUER, DISPATCH_SIGNIN_ISSUER) whose discovery
// succeeds: a stub answers each discovery as that issuer, naming the user part in its document's
// issuer, as go-oidc requires, so every refusal after a discovery is reached. They are the key
// source's refusal of a document with no jwks_uri, the code flow's of one with no endpoints and of
// an unreadable revocation_endpoint, the client check's three (a token endpoint that cannot be
// reached, invalid_client, another OAuth error), the verifier's refusal of a token another issuer
// issued, and the refusal of a wait for a discovery that does not end. Each names the issuer with no
// part of its user part; the control, the same issuer with no user part, names the stub's host and
// port in each. The user and the password are made for this test.
func TestEachRefusalAfterADiscoveryNamesNoUserPartOfTheIssuer(t *testing.T) {
	const (
		user     = "made-up-user"
		password = "not-a-secret-3e9b52"
	)
	// The issuer a request was asked of: the first segment of its path names the case, and the
	// user part, when there is one, comes as the basic credentials Go's client sends for it.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		variant, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		base := "http://" + r.Host + "/" + variant
		w.Header().Set("Content-Type", "application/json")
		switch rest {
		case ".well-known/openid-configuration":
			issuer := base
			if name, secret, ok := r.BasicAuth(); ok {
				issuer = "http://" + name + ":" + secret + "@" + r.Host + "/" + variant
			}
			document := map[string]any{
				"issuer":                 issuer,
				"authorization_endpoint": base + "/authorize",
				"token_endpoint":         base + "/token",
				"jwks_uri":               base + "/jwks",
			}
			switch variant {
			case "no-jwks":
				delete(document, "jwks_uri")
			case "no-endpoints":
				delete(document, "authorization_endpoint")
				delete(document, "token_endpoint")
			case "unreadable-revocation":
				document["revocation_endpoint"] = 5
			case "token-unreachable":
				document["token_endpoint"] = "http://127.0.0.1:1/token"
			}
			_ = json.NewEncoder(w).Encode(document)
		case "token":
			if variant == "invalid-client" {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unsupported_grant_type"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stub.Close)
	host := strings.TrimPrefix(stub.URL, "http://")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	segment := func(text string) string { return base64.RawURLEncoding.EncodeToString([]byte(text)) }
	otherIssuers := segment(`{"alg":"RS256","kid":"made-up-key"}`) + "." +
		segment(`{"iss":"https://other-issuer.example","aud":"dispatch","sub":"made-up-subject","exp":4102444800}`) + "." +
		segment("made-up-signature")

	for _, userPart := range []string{user + ":" + password + "@", ""} {
		issuer := func(variant string) string { return "http://" + userPart + host + "/" + variant }
		ctx := context.Background()
		texts := map[string]string{}
		refusal := func(name, want string, err error) {
			t.Helper()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("user part %q, %s: err = %v, want a refusal that says %q", userPart, name, err, want)
			}
			texts[name] = err.Error()
		}
		_, err := NewKeys(ctx, issuer("no-jwks"))
		refusal("the key source's document with no jwks_uri", "names no jwks_uri", err)
		_, err = discoverCodeFlow(ctx, issuer("no-endpoints"), "made-up-client", "not-a-secret-client")
		refusal("the code flow's document with no endpoints", "names no authorization_endpoint", err)
		_, err = discoverCodeFlow(ctx, issuer("unreadable-revocation"), "made-up-client", "not-a-secret-client")
		refusal("the code flow's unreadable revocation_endpoint", "revocation_endpoint", err)
		check, err := CheckClient(ctx, issuer("token-unreachable"), "made-up-client", "not-a-secret-client")
		refusal("the client check's unreachable token endpoint", "check the client at", err)
		if check != ClientCheckIssuerUnreachable {
			t.Errorf("the client check's unreachable token endpoint: %v, want %v", check, ClientCheckIssuerUnreachable)
		}
		check, err = CheckClient(ctx, issuer("invalid-client"), "made-up-client", "not-a-secret-client")
		refusal("the client check's invalid_client", "refused the client", err)
		if check != ClientCheckRejected {
			t.Errorf("the client check's invalid_client: %v, want %v", check, ClientCheckRejected)
		}
		check, err = CheckClient(ctx, issuer("other-error"), "made-up-client", "not-a-secret-client")
		refusal("the client check's other OAuth error", "unsupported_grant_type", err)
		if check != ClientCheckIssuerUnreachable {
			t.Errorf("the client check's other OAuth error: %v, want %v", check, ClientCheckIssuerUnreachable)
		}
		verifier, err := New(ctx, issuer("verifier"), "dispatch")
		if err != nil {
			t.Fatalf("user part %q: New: %v", userPart, err)
		}
		_, err = verifier.Verify(ctx, otherIssuers)
		refusal("the verifier's token of another issuer", "other-issuer.example", err)
		if !errors.Is(err, ErrIssuer) {
			t.Errorf("the verifier's token of another issuer: %v, want %v", err, ErrIssuer)
		}
		waiting := newDiscovery(issuer("waiting"), func(ctx context.Context) (int, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return 0, errors.New("made-up failure")
		})
		ended, cancel := context.WithCancel(ctx)
		cancel()
		_, err = waiting.discover(ended)
		refusal("a wait for a discovery that does not end", "waiting for issuer", err)

		for name, text := range texts {
			if strings.Contains(text, password) || strings.Contains(text, user) {
				t.Errorf("user part %q, %s: %q holds the user part", userPart, name, text)
			}
			if userPart == "" && !strings.Contains(text, host) {
				t.Errorf("the control with no user part, %s: %q names no issuer", name, text)
			}
		}
	}
}
