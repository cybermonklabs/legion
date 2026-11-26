// packages/envoy/cmd/cipher/proxy_trust_grant_reuse_test.go
//
// agent-secrets reuses a live grant across the commands of a session, so two runs of one grant
// can have different trust settings: the second command's CURL_CA_BUNDLE can name a private
// authority the first one's did not, or a pipe the second run alone can read. Each run's bundle
// must hold the trust its own command's settings name. When a second run of the grant finds the
// first's finished directory, prepareAirlock answers that directory, and so the bundle the first
// command's settings built; the second command's private authority is then in no file its clients
// read, and a direct call to that authority's host fails verification (measured through the CLI
// with dev-airlock.sh's stack: the second run's bundle held 149 certificates and not the private
// root, and curl, git and urllib refused the private server).
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
)

// TestPrepareAirlockSecondRunOfAGrantGetsTheTrustItsOwnSettingsName: a second run of one grant
// whose CURL_CA_BUNDLE names a private authority the first run's settings did not gets a bundle
// that holds that authority, not the first run's bundle.
func TestPrepareAirlockSecondRunOfAGrantGetsTheTrustItsOwnSettingsName(t *testing.T) {
	authority, err := proxyrulestest.NewAuthority("test private authority of the second command")
	if err != nil {
		t.Fatal(err)
	}
	private := strings.TrimSpace(string(authority.CertificatePEM()))
	privateFile := filepath.Join(t.TempDir(), "private.pem")
	if err := os.WriteFile(privateFile, []byte(private+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeDir := t.TempDir()
	expires := time.Now().Add(time.Hour)
	// The release's certificate is the test fixture's, which no store trusts, so both runs write a
	// bundle whatever the machine holds.
	first, err := prepareAirlock([]string{"XDG_RUNTIME_DIR=" + runtimeDir}, testRelease("AIRLOCK_ME", nil, nil), "grant-one", &expires)
	if err != nil {
		t.Fatalf("the first run: %v", err)
	}
	if data, err := os.ReadFile(first.bundle); err != nil || strings.Contains(string(data), private) {
		t.Fatalf("the first run's bundle (read error %v) already holds the private authority; the test needs a first command that names none", err)
	}
	second, err := prepareAirlock([]string{"XDG_RUNTIME_DIR=" + runtimeDir, "CURL_CA_BUNDLE=" + privateFile}, testRelease("AIRLOCK_ME", nil, nil), "grant-one", &expires)
	if err != nil {
		t.Fatalf("the second run of the same grant: %v", err)
	}
	data, err := os.ReadFile(second.bundle)
	if err != nil {
		t.Fatalf("read the second run's bundle %s: %v", second.bundle, err)
	}
	if !strings.Contains(string(data), private) {
		t.Fatalf("the second run's bundle %s lacks the private authority its CURL_CA_BUNDLE names; it is the first run's bundle (first %s, second %s)", second.bundle, first.bundle, second.bundle)
	}
}
