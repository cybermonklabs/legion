// packages/envoy/cmd/cipher/machine_login_slow_broker_linux_test.go
//go:build linux

// The real `agent-secrets machine login` against a broker slow to answer the device
// authorization the helper sends for it. The command waits helper.BrokerCallWait (30 s) for the
// login op, above the helper's own 20 s bound (helper.DefaultBrokerCallBound), so the helper's
// answer reaches it rather than the command timing out first, as it did when it waited 10 s.
//
// Linux-only, for the real helper this runs against (internal/cipher/helper is //go:build
// linux). login is no peer op, so this needs no SO_PEERPIDFD.
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMachineLoginPrintsItsCodeAgainstASlowBroker drives the real `agent-secrets machine login`
// through a real agent-secrets-helper whose broker answers the device authorization after 12 s.
// The client waits helper.BrokerCallWait (30 s) for that call, above the helper's 20 s bound, so
// the helper's answer reaches it; the login then issues and the command prints its code and
// exits 0.
//
// The named revert: restore `machine login`'s own 10 s helper.Call wait (machine.go) with an
// overlay, and a 12 s broker times the client out before the helper answers — the command exits
// 1 with a transport timeout, the bug this client wait closes.
func TestMachineLoginPrintsItsCodeAgainstASlowBroker(t *testing.T) {
	// Not parallel: it signals the test process or changes its signal handling (main.serveRealHelper, main.serveRealHelperWith, (*Server).Serve, (*Server).handle, (*Server).unenrollSessionKey, (*Broker).Revoke, (*Broker).signingAuthority, (*SignInAuthority).Current, (*Client).Token, (*Client).tradeHeld, (*Client).trade, (*ExternalAccountIdentity).Token, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.serveRealHelper, main.serveRealHelperWith, (*Server).Serve, (*Server).handle, (*Server).unenrollSessionKey, (*Broker).Revoke, (*Broker).signingAuthority, (*SignInAuthority).Current, (*Client).Token, (*Client).tradeHeld, (*Client).trade, (*ExternalAccountIdentity).Token, (gcloudToken).Token, main.runGcloud, main.runRecorded, runningGcloud set at secret_gcp.go:516).
	const (
		brokerDelay = 12 * time.Second
		userCode    = "ABCD-EFGH"
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(brokerDelay):
		case <-r.Context().Done():
			return
		}
		writeJSON(w, map[string]any{"device_code": "device-1", "user_code": userCode, "verification_uri": "https://s", "expires_in": 900, "interval": 1})
	})
	mux.HandleFunc("POST /v1/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"access_token": "not-a-secret-access", "token_type": "DPoP", "expires_in": 900, "refresh_token": "not-a-secret-refresh",
			"credential_id": "cred-1", "credential_expires_at": time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)})
	})
	broker := httptest.NewServer(mux)
	defer broker.Close()

	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, broker.URL, "ada@example.com")

	started := time.Now()
	stdout, stderr, exit := runAgentSecrets(t, binary, broker.URL, t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock},
		"machine", "login")
	waited := time.Since(started)
	if exit != 0 || !strings.Contains(stdout, "machine login code: "+userCode) {
		t.Fatalf("machine login against a broker answering in %s = %d %q (stderr %q) after %s; want exit 0 and the broker's code",
			brokerDelay, exit, stdout, stderr, waited)
	}
	if waited < brokerDelay {
		t.Fatalf("machine login answered after %s, before the broker did: the row proves nothing", waited)
	}
	t.Logf("machine login read the code a broker answered in %s after %s", brokerDelay, waited)
}
