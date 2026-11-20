//go:build linux

package helper

import (
	"net/http"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestASplitRefusalPrintsNoCredentialItsWordsQuote: the broker's device authorization endpoint
// answers invalid_request with a description whose first ": " falls inside a URL that carries a
// credential. The rule cannot read the whole description as clean, so neither half is read on its
// own: the helper's error for that answer, both as the boundary hands it on (redact.OAuth2) and as
// x/oauth2 answers it, prints none of the credential. A description the rule reads whole is still
// split at its ": " into the broker's code and message (the "<CODE>: <message>" form the split is
// for). The designated assertion is helper.split-refusal/<row>.
func TestASplitRefusalPrintsNoCredentialItsWordsQuote(t *testing.T) {
	addr := brokeraddr.New("https://broker.example")
	for name, description := range map[string]string{
		"user-info": "see https://" + redacttest.User + ": " + redacttest.Password + "@issuer.example/p",
		"query":     "https://issuer.example/cb?token=: " + redacttest.Token,
	} {
		answer := func() *oauth2.RetrieveError {
			return &oauth2.RetrieveError{
				Response:         &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request"},
				ErrorCode:        "invalid_request",
				ErrorDescription: description,
			}
		}
		_, crossed := redact.OAuth2(struct{}{}, answer())
		for path, err := range map[string]error{"boundary": crossed, "answer": answer()} {
			redacttest.RequireNone(t, "helper.split-refusal/"+name+"-"+path, oauthBrokerError(err, addr).Error())
		}
	}
	code, message := redact.NewOAuthRefusal("invalid_request", "LOGIN_PENDING: a login is already pending")
	if !code.Equal("LOGIN_PENDING") || !message.Equal("a login is already pending") {
		t.Errorf("the broker's own refusal split as %v and %v, want LOGIN_PENDING and its message", code, message)
	}
}
