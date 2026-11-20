package brokerclient

import (
	"fmt"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).

type refusedErrorTwin struct {
	status            int
	code, description string
	Identity          string
	answer            *oauth2.RetrieveError
}

func (e *refusedErrorTwin) Error() string {
	return fmt.Sprintf("the secrets broker refused %s: HTTP %d %s: %s", e.Identity, e.status, e.code, e.description)
}

type refusedErrorExported struct {
	Status            int
	Code, Description string
	Identity          string
}

type tokenAnswerTwin struct {
	AccessToken         string `json:"access_token"`
	TokenType           string `json:"token_type"`
	IssuedTokenType     string `json:"issued_token_type"`
	ExpiresIn           string `json:"expires_in"`
	RefreshToken        string `json:"refresh_token"`
	CredentialID        string `json:"credential_id"`
	CredentialExpiresAt string `json:"credential_expires_at"`
	Error               string `json:"error"`
	ErrorDescription    string `json:"error_description"`
}

func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	code, words := redact.NewRemoteText(redacttest.Code), redact.NewRemoteText(redacttest.Words)
	for _, h := range []redacttest.Holder{
		{
			Name:     "brokerclient.RefusedError",
			Kind:     "error",
			Boxed:    newRefusedError(refusedIdentity(projectedStub()), 400, code, words),
			Same:     &refusedErrorTwin{400, redacttest.Code, redacttest.Words, refusedIdentity(projectedStub()), redact.NewRetrieveError(400, code, words)},
			Exported: &refusedErrorExported{400, redacttest.Code, redacttest.Words, refusedIdentity(projectedStub())},
		},
		{
			Name: "brokerclient.tokenAnswer",
			Kind: "fields",
			Boxed: tokenAnswer{
				TokenType: code, ExpiresIn: redact.RemoteNumber{RemoteText: words},
				CredentialExpiresAt: words, Error: code, ErrorDescription: words,
			},
			Same: tokenAnswerTwin{
				TokenType: redacttest.Code, ExpiresIn: redacttest.Words,
				CredentialExpiresAt: redacttest.Words, Error: redacttest.Code, ErrorDescription: redacttest.Words,
			},
			Exported: tokenAnswerTwin{
				TokenType: redacttest.Code, ExpiresIn: redacttest.Words,
				CredentialExpiresAt: redacttest.Words, Error: redacttest.Code, ErrorDescription: redacttest.Words,
			},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
