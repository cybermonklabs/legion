package brokerclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestARenewedSignInsErrorPrintsNoneOfTheIssuersWords: an issuer that refuses a sign-in's refresh
// with an error code quoting a URL that carries a credential, and not invalid_grant, leaves the
// sign-in as it was; Refresh's error, which wraps x/oauth2's answer with %w, prints none of the
// credential.
func TestARenewedSignInsErrorPrintsNoneOfTheIssuersWords(t *testing.T) {
	const clientID, clientKey = "example-device-client", "example-device-client-key"
	issuer := oidctest.New(t)
	issuer.EnableDeviceFlow(issuer.PublishKey(t, "google-key"), clientID, clientKey)
	issuer.FailToken(http.StatusBadRequest, redacttest.Words)
	flow := &DeviceFlow{Issuer: issuer.URL(), ClientID: clientID, ClientSecret: clientKey}
	_, err := flow.Refresh(context.Background(), "a-refresh-token")
	if err == nil || errors.Is(err, ErrSignInEnded) {
		t.Fatalf("Refresh = %v, want the issuer's refusal, not an ended sign-in", err)
	}
	redacttest.RequireNone(t, "brokerclient.device-refresh/error", err.Error())
	redacttest.RequireNone(t, "brokerclient.device-refresh/plus-v", fmt.Sprintf("%+v", err))
}
