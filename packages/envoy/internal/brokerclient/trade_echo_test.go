package brokerclient

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestATradeRefusalPrintsNoSubjectTokenItEchoes: an answer in the broker's place refuses the
// trade with a description that echoes the subject token the trade's form carried. The refusal,
// as Run hands it on, as redact.Error prints it, and as the *oauth2.RetrieveError it unwraps to
// prints, holds none of it: the trade holds the answer's words with every credential its request
// carried redacted (redact.Carried, RemoteText.WithoutTokens), while the code the renewal loop
// acts on is the one the answer gave (the red team's probe of #4368 at db7e4a0226). Captures
// brokerclient.trade-echo/run, brokerclient.trade-echo/redact-error and
// brokerclient.trade-echo/answer.
func TestATradeRefusalPrintsNoSubjectTokenItEchoes(t *testing.T) {
	broker := newStubBroker(t, func(_ int, form url.Values) (int, map[string]any) {
		return 400, map[string]any{"error": "invalid_grant", "error_description": "subject_token " + form.Get("subject_token") + " is not one this broker reads"}
	})
	client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t),
		Subject: &scriptedSource{token: redacttest.Token, tokenType: "urn:envoy:params:oauth:token-type:kubernetes-service-account", describe: "probe subject"}})
	if err != nil {
		t.Fatal(err)
	}
	runErr := client.Run(context.Background(), func(context.Context, *Token) error { return nil })
	if runErr == nil {
		t.Fatal("Run answered no error")
	}
	var refused *RefusedError
	if !errors.As(runErr, &refused) || !refused.HasCode("invalid_grant") {
		t.Errorf("Run's error is %v, want a refusal with the code the answer gave", runErr)
	}
	redacttest.RequireNone(t, "brokerclient.trade-echo/run", runErr.Error())
	redacttest.RequireNone(t, "brokerclient.trade-echo/redact-error", redact.Error(runErr, broker.URL()).Error())
	var answer *oauth2.RetrieveError
	if !errors.As(runErr, &answer) {
		t.Fatalf("Run's error %v unwraps to no *oauth2.RetrieveError", runErr)
	}
	redacttest.RequireNone(t, "brokerclient.trade-echo/answer", answer.Error())
}
