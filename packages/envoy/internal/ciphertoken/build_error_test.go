package ciphertoken_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/configaddr"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnIntrospectionTheVerifierCannotBuildNamesNoCredentialTheBrokerURLCarries: a broker address
// carrying the probe's user information, query value and fragment value, with an escape net/url
// cannot read, makes no introspection: net/http's error quotes the URL it could not parse,
// credential and all. The verification is the unreachable outcome, and its error, which Dispatch
// and the listener log, names the failure's class (the address's Cause) and none of the
// credentials. Captures brokertoken.introspect-build.
func TestAnIntrospectionTheVerifierCannotBuildNamesNoCredentialTheBrokerURLCarries(t *testing.T) {
	unbuildable := "http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/%zz?access" + "_token=" +
		redacttest.Token + "#access" + "_token=" + redacttest.Token
	rig := newRig(t, func(o *ciphertoken.Options) {
		o.BrokerURL = configaddr.New("DISPATCH_AGENT_SECRETS_URL", unbuildable)
	})
	_, err := rig.verifier.Verify(t.Context(), newSigner(t).at(resourcePath, "a-token-no-introspection-can-carry"))
	if !errors.Is(err, ciphertoken.ErrBrokerUnreachable) {
		t.Fatalf("an introspection that could not be built answered %v, want ErrBrokerUnreachable", err)
	}
	redacttest.RequireNone(t, "brokertoken.introspect-build", err.Error())
	if !strings.Contains(err.Error(), "build the introspection request") || !strings.Contains(err.Error(), "not a valid URL") {
		t.Errorf("the verification's error = %q, want it to name the build and the failure's class", err)
	}
}
