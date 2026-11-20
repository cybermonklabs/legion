// packages/envoy/internal/cipher/helper/oauth_broker_error_test.go
//go:build linux

package helper

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/cipheraddr"
)

// TestAnOAuthRefusalSettlesAsTheBrokersRefusalWhereverItsWordsPrint: oauthBrokerError makes a
// BrokerError of the broker's OAuth answer, its code and words, exactly where the redactor prints
// that answer's words whole, so a refusal settles as the known refusal it is. That holds alone, in a
// call, and under a caller's %w, whose own words the redactor drops but whose answer it still
// prints. Beside an address that carries a credential, and for an answer with an error_uri, it is
// still the broker's refusal: a BrokerError of its status alone, with no code and no word of the
// description, since those are the text the redactor withheld, so a 4xx still settles as a refusal
// and not as an unknown outcome.
func TestAnOAuthRefusalSettlesAsTheBrokersRefusalWhereverItsWordsPrint(t *testing.T) {
	const (
		clean = "http://127.0.0.1:1"
		dirty = "http://demouser7q:demopass9z-not-a-secret@127.0.0.1:1"
	)
	pending := func() *oauth2.RetrieveError {
		return &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest},
			ErrorCode: "invalid_request", ErrorDescription: "LOGIN_PENDING: a login is already pending"}
	}
	inCall := func(base string, cause error) error {
		return &url.Error{Op: "Post", URL: base + "/v1/oauth/device_authorization", Err: cause}
	}
	for _, row := range []struct {
		name string
		base string
		err  error
		// known is whether the answer settles as a BrokerError of LOGIN_PENDING; otherwise it
		// settles as a BrokerError of its status alone.
		known bool
	}{
		{"the answer, clean", clean, pending(), true},
		{"the answer in a call, clean", clean, inCall(clean, pending()), true},
		{"the answer under a caller's %w, clean", clean, fmt.Errorf("start the login: %w", pending()), true},
		{"the answer under a caller's %w in a call, clean", clean, fmt.Errorf("start the login: %w", inCall(clean, pending())), true},
		{"the answer beside a credential", dirty, pending(), false},
		{"the answer in a call beside a credential", dirty, inCall(dirty, pending()), false},
		{"an answer with an error_uri, clean", clean, &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest},
			ErrorCode: "invalid_request", ErrorDescription: "LOGIN_PENDING: a login is already pending", ErrorURI: "https://docs.example.test/login"}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := oauthBrokerError(row.err, cipheraddr.New(row.base))
			var be *BrokerError
			isBrokerError := errors.As(got, &be)
			switch {
			case row.known && (!isBrokerError || be.Status != http.StatusBadRequest || be.Code != "LOGIN_PENDING" || be.Message != "a login is already pending"):
				t.Errorf("oauthBrokerError = %v (%T); want the broker's 400 LOGIN_PENDING as a BrokerError", got, got)
			case !row.known && (!isBrokerError || be.Status != http.StatusBadRequest || be.Code != ""):
				t.Errorf("oauthBrokerError = %v (%T); want the broker's 400 as a BrokerError of its status alone, with no code", got, got)
			case !row.known && (strings.Contains(got.Error(), "LOGIN_PENDING") || strings.Contains(got.Error(), "a login is already pending") || !strings.Contains(got.Error(), "broker 400")):
				t.Errorf("oauthBrokerError = %q; want the broker's status and none of its words", got)
			}
			if strings.Contains(got.Error(), "demopass9z-not-a-secret") || strings.Contains(got.Error(), "start the login") {
				t.Errorf("oauthBrokerError = %q, which holds the address's credential or the caller's words", got)
			}
		})
	}
}
