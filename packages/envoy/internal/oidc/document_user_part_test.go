package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The key set's URL and the revocation endpoint are the ones the issuer's own discovery document
// names, and an issuer configured with a user part passes discovery only when its document names
// the same issuer, user part and all; an issuer that builds its endpoints from its own issuer then
// repeats the user part in them. A stub does so, and drops the connection of each request to the
// key set and the revocation endpoint, so the key read and the revocation fail at the transport,
// whose error quotes the request's URL. Each names no part of the user part. The control is the
// same issuer with no user part, whose errors are net/http's own, naming the stub's host. The user
// and the password are made for this test.
func TestTheEndpointsAnIssuersDocumentNamesAreNamedWithNoUserPart(t *testing.T) {
	const (
		user     = "made-up-user-2e7d"
		password = "not-a-secret-9b1c40"
	)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuer := "http://" + r.Host
		if name, secret, ok := r.BasicAuth(); ok {
			issuer = "http://" + name + ":" + secret + "@" + r.Host
		}
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 issuer,
				"authorization_endpoint": issuer + "/authorize",
				"token_endpoint":         issuer + "/token",
				"jwks_uri":               issuer + "/jwks",
				"revocation_endpoint":    issuer + "/revoke",
			})
		case "/jwks", "/revoke":
			// The connection ends with no answer: a transport error, which quotes the request's URL.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stub.Close)
	host := strings.TrimPrefix(stub.URL, "http://")
	for _, userPart := range []string{user + ":" + password + "@", ""} {
		issuer := "http://" + userPart + host
		ctx := context.Background()
		keys, err := NewKeys(ctx, issuer)
		if err != nil {
			t.Fatalf("user part %q: NewKeys: %v", userPart, err)
		}
		_, keyErr := keys.Key(ctx, "made-up-key", "RS256")
		revokeErr := NewCodeFlow(issuer, "made-up-client", "not-a-secret-client", "").Revoke(ctx, "made-up-refresh-token")
		for name, err := range map[string]error{"the key set's read": keyErr, "the revocation": revokeErr} {
			if err == nil {
				t.Fatalf("user part %q, %s: no error, want the dropped connection's", userPart, name)
			}
			if strings.Contains(err.Error(), user) || strings.Contains(err.Error(), password) {
				t.Errorf("user part %q, %s: %q holds the user part", userPart, name, err)
			}
			if userPart == "" && !strings.Contains(err.Error(), `"http://`+host+`/`) {
				t.Errorf("the control, %s: %q, want net/http's own error naming the stub's URL", name, err)
			}
		}
	}
}
