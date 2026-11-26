// packages/envoy/cmd/cipher/relogin_refused_linux_test.go
//go:build linux

package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
)

// TestADeniedReLoginSaysDeniedWhenTheOldCredentialIsRefusedMeanwhile: an approved machine login,
// then a re-login that is still waiting for approval when the broker refuses the first login's
// credential, then the operator denies the re-login. The person who ran `machine login` must be
// told it was denied: the refusal is about the earlier credential, and this login's own outcome is
// the denial. Only its message is at stake (it exits 1 either way); "expired" would send them
// looking for a timeout that never happened.
func TestADeniedReLoginSaysDeniedWhenTheOldCredentialIsRefusedMeanwhile(t *testing.T) {
	// Not parallel: it signals the test process or changes its signal handling (main.serveRealHelper, main.serveRealHelperWith, (*Server).Serve, (*Server).handle, (*Server).unenrollSessionKey, (*Broker).Revoke, (*Broker).signingAuthority, (*SignInAuthority).Current, (*Client).Token, (*Client).tradeHeld, (*Client).trade, (*ExternalAccountIdentity).Token, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.serveRealHelper, main.serveRealHelperWith, (*Server).Serve, (*Server).handle, (*Server).unenrollSessionKey, (*Broker).Revoke, (*Broker).signingAuthority, (*SignInAuthority).Current, (*Client).Token, (*Client).tradeHeld, (*Client).trade, (*ExternalAccountIdentity).Token, (gcloudToken).Token, main.runGcloud, main.runRecorded, runningGcloud set at secret_gcp.go:516).
	rig := ciphertest.NewRig(t)
	binary := buildAgentSecrets(t)
	srv, sock := serveRealHelper(t, rig.URL, rig.Operator)

	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("approved machine login (code %s): exit %d, stderr %q; want 0", code, exit, stderr)
	}
	code, exit, stderr := machineLoginThen(t, rig, binary, sock, "deny", func() { refuseTheHeldCredential(t, rig, srv, sock) })
	if exit != 1 || stderr != "agent-secrets machine login: denied\n" {
		t.Fatalf("re-login denied after the old credential was refused (code %s): exit %d, stderr %q; want 1, %q",
			code, exit, stderr, "agent-secrets machine login: denied\n")
	}
}
