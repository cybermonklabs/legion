// packages/envoy/internal/redact/brokeranswer_test.go
//
// Which OAuth error answer a printer of the broker's refusals takes as the broker's own words:
// never one that Class or Masked holds, which is another server's answer this package has already
// printed as its class. Tested through cipheraddr.Address's Error and Cause, which is how every
// broker client reaches redact.Error and redact.Cause. Every secret here is made up.

package redact_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/redact"
)

// answerSecret stands for a credential that a server other than the broker repeats in its answer,
// or that a caller's text carries. No printed text holds it.
const answerSecret = "hunter2pwANSWER"

// foreignAnswer is an OAuth refusal from a server other than the broker, whose description
// repeats answerSecret, as a server can write anything there.
func foreignAnswer() *oauth2.RetrieveError {
	return &oauth2.RetrieveError{
		Response:  &http.Response{StatusCode: http.StatusBadRequest},
		ErrorCode: "invalid_client", ErrorDescription: "echo of " + answerSecret,
	}
}

// TestAnAnswerUnderClassIsNotTheBrokersOwn: an OAuth answer that Class or Masked holds came from
// another server, so BrokerAnswer does not find it. Beside a clean broker address, Error and Cause
// print the broker's own answer whole, as trusted words that the allow-list does not check. An
// answer under Class must not buy that trust for the text around it: that text names the other
// server, by a URL that can carry a credential, so Error and Cause print the failure's class
// alone. The broker's own answer beside the same address still prints its code and description
// whole: the control.
func TestAnAnswerUnderClassIsNotTheBrokersOwn(t *testing.T) {
	addr := cipheraddr.New("https://broker.example")
	issuer := "https://" + "tokuser9f3" + ":" + answerSecret + "@issuer.example"
	for name, held := range map[string]error{
		"Class":  redact.Class(foreignAnswer()),
		"Masked": redact.Masked(foreignAnswer(), "the server answered HTTP 400"),
	} {
		t.Run(name, func(t *testing.T) {
			err := fmt.Errorf("renew the sign-in at %s: %w", issuer, held)
			if re, ok := redact.BrokerAnswer(err); ok {
				t.Errorf("BrokerAnswer found %q under %s, want no answer of the broker's", re, name)
			}
			for what, printed := range map[string]error{"Error": addr.Error(err), "Cause": addr.Cause(err)} {
				text := printed.Error()
				if strings.Contains(text, answerSecret) || strings.Contains(text, "tokuser9f3") {
					t.Errorf("%s = %q, which holds the credential", what, text)
				}
				// The answer is another server's, so the class names the server and not the broker.
				if strings.Contains(text, "the broker answered") || !strings.Contains(text, "the server answered HTTP 400") {
					t.Errorf("%s = %q, want the class %s already gave it, naming the server", what, text, name)
				}
			}
		})
	}
	t.Run("control: the broker's own answer", func(t *testing.T) {
		own := &oauth2.RetrieveError{
			Response:  &http.Response{StatusCode: http.StatusBadRequest},
			ErrorCode: "invalid_request", ErrorDescription: "LOGIN_PENDING: a login is already pending",
		}
		err := fmt.Errorf("device authorization: %w", own)
		if re, ok := redact.BrokerAnswer(err); !ok || re != own {
			t.Errorf("BrokerAnswer = %v, %t; want the broker's own answer", re, ok)
		}
		for what, printed := range map[string]error{"Error": addr.Error(err), "Cause": addr.Cause(err)} {
			if text := printed.Error(); !strings.Contains(text, "invalid_request") || !strings.Contains(text, "a login is already pending") {
				t.Errorf("%s = %q, want the broker's code and description whole", what, text)
			}
		}
	})
}
