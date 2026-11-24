package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// googleListenerAccount is the Google service account the Envoy listener on a VM calls the
	// broker's API as, listed in BROKER_API_ACCOUNTS as listener=google:<account>;
	// googleVMAccount is a fleet BROKER_VM_SERVICES lists, whose machines' tokens carry the same
	// audience.
	googleListenerAccount = "example-listener@example-project.iam.gserviceaccount.com"
	googleVMAccount       = "example-fleet@example-project.iam.gserviceaccount.com"
)

// TestAGoogleAPICallerReachesItsRoutesThroughTheBinary runs the REAL broker binary with a
// Google account listed as the listener caller and a stand-in for Google's issuer: the caller's
// own Google identity token reads a token back (200 on introspection) and is refused a relay
// route 403 ACCOUNT_LACKS_POWER, as the Kubernetes listener caller is in
// TestAListedAccountReachesOnlyItsOwnRoutes; a token of the same account for another audience is
// 401 ACCOUNT_TOKEN_INVALID. No other binary test drives a Google API caller.
// TestAGoogleAPICallerNotListedIsRefusedThroughTheBinary is its control.
func TestAGoogleAPICallerReachesItsRoutesThroughTheBinary(t *testing.T) {
	google := oidctest.New(t)
	googleKey := google.PublishKey(t, "google-key")
	live := newPlainBroker(startGoogleCallerBroker(t, google, "listener=google:"+googleListenerAccount).Addr)
	token := googleCallerToken(t, google, googleKey, machineAudience)

	status, body := live.callAs(t, http.MethodPost, introspection, token, "application/x-www-form-urlencoded", "token=a-token-nobody-issued")
	if status != http.StatusOK || body["active"] != false {
		t.Fatalf("introspection as the Google listener caller = %d %v, want 200 answering a token nobody issued inactive", status, body)
	}
	status, body = live.callAs(t, http.MethodGet, "/v1/pending?approver="+powersApprover, token, "", "")
	if status != http.StatusForbidden || body["code"] != lacksPower {
		t.Fatalf("GET /v1/pending as the Google listener caller = %d %v, want 403 %s: the listener relays no decision", status, body, lacksPower)
	}
	status, body = live.callAs(t, http.MethodPost, introspection, googleCallerToken(t, google, googleKey, "https://elsewhere.example"),
		"application/x-www-form-urlencoded", "token=a-token-nobody-issued")
	if status != http.StatusUnauthorized || body["code"] != "ACCOUNT_TOKEN_INVALID" {
		t.Fatalf("introspection with the caller's token for another audience = %d %v, want 401 ACCOUNT_TOKEN_INVALID", status, body)
	}
}

// TestAGoogleAPICallerNotListedIsRefusedThroughTheBinary is the control of the test above: the
// same caller's token where BROKER_API_ACCOUNTS lists another Google account is 403
// ACCOUNT_NOT_LISTED.
func TestAGoogleAPICallerNotListedIsRefusedThroughTheBinary(t *testing.T) {
	google := oidctest.New(t)
	googleKey := google.PublishKey(t, "google-key")
	live := newPlainBroker(startGoogleCallerBroker(t, google, "listener=google:example-other@example-project.iam.gserviceaccount.com").Addr)
	status, body := live.callAs(t, http.MethodPost, introspection, googleCallerToken(t, google, googleKey, machineAudience),
		"application/x-www-form-urlencoded", "token=a-token-nobody-issued")
	if status != http.StatusForbidden || body["code"] != "ACCOUNT_NOT_LISTED" {
		t.Fatalf("introspection as a Google account BROKER_API_ACCOUNTS does not list = %d %v, want 403 ACCOUNT_NOT_LISTED", status, body)
	}
}

// startGoogleCallerBroker starts the binary on a fresh schema with Google's issuer at google,
// the machine audience, one VM fleet and apiAccounts as BROKER_API_ACCOUNTS.
func startGoogleCallerBroker(t *testing.T, google *oidctest.Issuer, apiAccounts string) *brokerProcess {
	t.Helper()
	memorySecretsFile := filepath.Join(t.TempDir(), "memory-secrets.json")
	if err := os.WriteFile(memorySecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_SECRETS_STORE=memory",
		"BROKER_MEMORY_SECRETS_FILE=" + memorySecretsFile,
		"BROKER_GOOGLE_OIDC_ISSUER=" + google.URL(),
		"BROKER_GOOGLE_OIDC_AUDIENCE=" + machineAudience,
		"BROKER_VM_SERVICES=" + pinnedFleetService + "=" + googleVMAccount,
		"BROKER_API_ACCOUNTS=" + apiAccounts,
	}})
}

// googleCallerToken is the listener's own Google identity token for audience, verified, minted
// a few seconds ago and living under the hour a caller's token may.
func googleCallerToken(t *testing.T, google *oidctest.Issuer, key *oidctest.Key, audience string) string {
	t.Helper()
	claims := google.Claims("1234567890", audience)
	claims["email"], claims["email_verified"] = googleListenerAccount, true
	claims["iat"], claims["exp"] = time.Now().Add(-5*time.Second).Unix(), time.Now().Add(50*time.Minute).Unix()
	return google.Mint(t, key, claims)
}
