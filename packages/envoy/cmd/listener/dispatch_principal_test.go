package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logging"
)

// apiAuth hands every handler the principal the request's identity token proved: a projected
// service-account token's subject, a Google identity token's account. No other credential proves
// one, so a request on the shared token, on a broker token, or on none carries none. The send
// route reads it to take a role mark from Dispatch's own principal alone.
func TestAPIAuthCarriesTheVerifiedPrincipalOfAnIdentityTokenAndOfNoOtherCredential(t *testing.T) {
	t.Parallel()
	cluster := newClusterIssuer(t)
	google := newGoogleStandIn(t, listenerGoogleAccount)
	var principal string
	var reached bool
	recording := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, reached = verifiedPrincipal(r), true
		w.WriteHeader(http.StatusNoContent)
	})
	closed := apiAuth(listenerAPIAuth{token: "expected-token", verifier: cluster.verifier, google: google.identities, logger: logging.NewWithWriter("test-machine", &bytes.Buffer{})}, recording)
	open := apiAuth(listenerAPIAuth{logger: logging.NewWithWriter("test-machine", &bytes.Buffer{})}, recording)
	for _, tc := range []struct {
		name      string
		handler   http.Handler
		authorize string
		want      string
	}{
		{"a projected service-account token proves its subject", closed, "Bearer " + cluster.token(t, listenerListedSubject), listenerListedSubject},
		{"a Google identity token proves its account, in lower case", closed, "Bearer " + google.token(t, strings.ToUpper(listenerGoogleAccount), true, listenerGoogleAudience), listenerGoogleAccount},
		{"the shared token proves nobody", closed, "Bearer expected-token", ""},
		{"no credential proves nobody", open, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			principal, reached = "unset", false
			recorder := httptest.NewRecorder()
			tc.handler.ServeHTTP(recorder, bearerRequest("/v1/sessions", tc.authorize))
			if !reached || recorder.Code != http.StatusNoContent || principal != tc.want {
				t.Fatalf("status %d, reached %t, principal %q; want 204 carrying %q", recorder.Code, reached, principal, tc.want)
			}
		})
	}
}

// ENVOY_DISPATCH_PRINCIPAL names Dispatch's own principal, read at start beside the bearers' own
// settings: a projected service-account subject, or a Google service account's address. A value
// of neither shape refuses the start naming it, and so does one no credential this listener takes
// could ever prove: a subject with no projected-token verifier, or one ENVOY_OIDC_ACCOUNTS leaves
// out, and an address with no Google pair, or one ENVOY_OIDC_GOOGLE_ACCOUNTS does not list. Unset
// names nobody, and then no send may carry a role mark.
func TestTheDispatchPrincipalIsReadAtStartAndOneNoCredentialCouldProveRefusesIt(t *testing.T) {
	t.Parallel()
	const issuer = "https://container.googleapis.com/v1/projects/example/locations/us-central1/clusters/example"
	const subject = "system:serviceaccount:dispatch:example-dispatch"
	for _, tc := range []struct {
		name      string
		value     string
		issuer    string
		accounts  []string
		google    []string
		want      string
		wantError string
	}{
		{name: "unset names nobody", issuer: issuer},
		{name: "a listed subject", value: " " + subject + " ", issuer: issuer, accounts: []string{subject, listenerListedSubject}, want: subject},
		{name: "a subject beside a verifier that admits every subject", value: subject, issuer: issuer, want: subject},
		{name: "a Google address, compared in lower case", value: strings.ToUpper(listenerGoogleAccount), google: []string{listenerGoogleAccount}, want: listenerGoogleAccount},
		{name: "a subject with no verifier", value: subject, wantError: "ENVOY_OIDC_ISSUER"},
		{name: "a subject the account list leaves out", value: subject, issuer: issuer, accounts: []string{listenerListedSubject}, wantError: "ENVOY_OIDC_ACCOUNTS"},
		{name: "an address with no Google pair", value: listenerGoogleAccount, issuer: issuer, wantError: "ENVOY_OIDC_GOOGLE_AUDIENCE"},
		{name: "an address the Google list leaves out", value: listenerGoogleOtherEmail, google: []string{listenerGoogleAccount}, wantError: "ENVOY_OIDC_GOOGLE_ACCOUNTS"},
		{name: "a bare name", value: "dispatch", issuer: issuer, wantError: `"dispatch"`},
		{name: "a subject with no name", value: "system:serviceaccount:dispatch:", issuer: issuer, wantError: `"system:serviceaccount:dispatch:"`},
		{name: "a subject with a third part", value: "system:serviceaccount:dispatch:a:b", issuer: issuer, wantError: `"system:serviceaccount:dispatch:a:b"`},
		{name: "two principals", value: subject + " " + listenerListedSubject, issuer: issuer, wantError: "ENVOY_DISPATCH_PRINCIPAL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveListenerDispatchPrincipal(func(key string) string {
				if key == "ENVOY_DISPATCH_PRINCIPAL" {
					return tc.value
				}
				return ""
			}, tc.issuer, tc.accounts, tc.google)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || !strings.Contains(err.Error(), "ENVOY_DISPATCH_PRINCIPAL") {
					t.Fatalf("ENVOY_DISPATCH_PRINCIPAL=%q: %q, %v; want a refusal naming the setting and %s", tc.value, got, err, tc.wantError)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ENVOY_DISPATCH_PRINCIPAL=%q: %q, %v; want %q", tc.value, got, err, tc.want)
			}
		})
	}
}
