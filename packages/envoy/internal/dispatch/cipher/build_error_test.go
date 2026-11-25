package cipher

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/configaddr"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestARequestTheClientCannotBuildNamesNoCredentialTheConfiguredURLCarries: a broker address
// carrying the probe's user information, query value and fragment value, with an escape net/url
// cannot read, makes no request: net/http's error quotes the URL it could not parse, credential and
// all. The call's error names the call and the failure's class (the address's Cause), and none of
// the credentials. Captures dispatch-agentsecrets.build.
func TestARequestTheClientCannotBuildNamesNoCredentialTheConfiguredURLCarries(t *testing.T) {
	unbuildable := "http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/%zz?access" + "_token=" +
		redacttest.Token + "#access" + "_token=" + redacttest.Token
	_, err := New(configaddr.New("DISPATCH_AGENT_SECRETS_URL", unbuildable), tokenFile(t, "account-token")).Pending(context.Background(), "sjawhar")
	if err == nil {
		t.Fatal("a call to an address net/url cannot parse answered no error")
	}
	redacttest.RequireNone(t, "dispatch-agentsecrets.build", err.Error())
	if !strings.Contains(err.Error(), "build GET /v1/pending") || !strings.Contains(err.Error(), "not a valid URL") {
		t.Errorf("the call's error = %q, want it to name the call and the failure's class", err)
	}
}
