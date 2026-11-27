package ciphertoken_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// unparsedEcho serves, until t ends, a stand-in that answers every request 302 Found with a
// Location net/http cannot parse (redirectstest.Unparsed), quoting the Authorization the request
// carried and the probe's words (redacttest.Words).
func unparsedEcho(t *testing.T) string {
	t.Helper()
	return redirectstest.Echo(t, "Authorization", func(authorization string) string {
		return redirectstest.Unparsed(authorization + " " + redacttest.Words)
	})
}

// TestARedirectOfAMintPrintsNoLocationOfIt: whatever answers in IAM Service Account Credentials'
// place answers the mint, which carries the pod's own access token, with a Location that quotes
// it. The mint's error holds no run of that token, none of the probe's words and no word of the
// Location. Captures brokertoken.mint-redirect.
func TestARedirectOfAMintPrintsNoLocationOfIt(t *testing.T) {
	ciphertokentest.NewMetadataStand(t)
	account := ciphertoken.NewGoogleAccount(gatewayAudience)
	account.ServiceAccount = judgeAccount
	account.IAMCredentialsURL = unparsedEcho(t)
	_, err := account.Read()
	if err == nil {
		t.Fatal("the redirected mint answered no error")
	}
	redacttest.RequireNoPiece(t, "brokertoken.mint-redirect", err.Error(), ciphertokentest.MetadataAccessBearer)
	if strings.Contains(err.Error(), "moved") {
		t.Errorf("the mint's error names the Location: %q", err)
	}
}

// TestARedirectOfAnIntrospectionPrintsNoLocationOfIt: whatever answers at the broker's address
// answers Dispatch's introspection, which carries Dispatch's own account token, with a Location
// that quotes it. The verification is the unreachable outcome, and its error, which Dispatch
// logs, holds no run of that token, none of the probe's words and no word of the Location.
// Captures brokertoken.introspect-redirect.
func TestARedirectOfAnIntrospectionPrintsNoLocationOfIt(t *testing.T) {
	const account = "rk4wn-not-a-secret-RDX-6c1d"
	file := filepath.Join(t.TempDir(), "account")
	if err := os.WriteFile(file, []byte(account), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := unparsedEcho(t)
	rig := newRig(t, func(o *ciphertoken.Options) {
		o.BrokerURL = broker
		o.Account = ciphertoken.NewAccountToken("DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE", file)
	})
	_, err := rig.verifier.Verify(t.Context(), newSigner(t).at(resourcePath, "a-token-asked-about-at-a-redirect"))
	if !errors.Is(err, ciphertoken.ErrBrokerUnreachable) {
		t.Fatalf("a redirected introspection answered %v, want ErrBrokerUnreachable", err)
	}
	redacttest.RequireNoPiece(t, "brokertoken.introspect-redirect", err.Error(), account)
	if strings.Contains(err.Error(), "moved") {
		t.Errorf("the verification's error names the Location: %q", err)
	}
}
