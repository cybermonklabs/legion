package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/listenerwire"
)

// TestAnUnjudgedCredentialsWordsNameNoWait: each 503 the listener answers a credential it could
// not judge carries Retry-After: 1, and its words name no wait of their own, so they cannot say
// another one than the header: an identity token whose issuer could not be read, a broker token
// the broker gave no answer for, and a broker token whose spent-proof check did not run. A
// credential that was judged and refused is 401 with no Retry-After.
func TestAnUnjudgedCredentialsWordsNameNoWait(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/messages/send", nil)
	logger := logging.New("test")
	for _, row := range []struct {
		name   string
		answer func(http.ResponseWriter)
		status int
		words  string
		retry  string
	}{
		{"an identity token whose issuer could not be read", func(w http.ResponseWriter) {
			refuseIdentityToken(w, fmt.Errorf("%w: the test's key set answered 503", oidc.ErrIssuerUnavailable))
		}, http.StatusServiceUnavailable, "this identity token could not be checked; try again after Retry-After", "1"},
		{"a broker token the broker gave no answer for", func(w http.ResponseWriter) {
			refuseBrokerToken(w, logger, request, nil, fmt.Errorf("%w: the test's broker is away", ciphertoken.ErrBrokerUnreachable))
		}, http.StatusServiceUnavailable, "the secrets broker cannot be reached; try again after Retry-After", "1"},
		{"a broker token whose spent-proof check did not run", func(w http.ResponseWriter) {
			refuseBrokerToken(w, logger, request, nil, errors.New("the test's spent-proof store did not answer"))
		}, http.StatusServiceUnavailable, "this access token could not be checked; try again after Retry-After", "1"},
		{"an identity token that was refused", func(w http.ResponseWriter) {
			refuseIdentityToken(w, oidc.ErrSignature)
		}, http.StatusUnauthorized, "unauthorized", ""},
		{"a broker token the broker holds as not live", func(w http.ResponseWriter) {
			refuseBrokerToken(w, logger, request, nil, ciphertoken.ErrNotActive)
		}, http.StatusUnauthorized, "unauthorized", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			row.answer(recorder)
			var body listenerwire.APIError
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s: %v", recorder.Body.String(), err)
			}
			if recorder.Code != row.status || body.Error != row.words {
				t.Fatalf("answered %d %q, want %d %q", recorder.Code, body.Error, row.status, row.words)
			}
			if strings.Contains(body.Error, "minute") {
				t.Fatalf("the words %q name a wait of their own beside Retry-After %q", body.Error, recorder.Header().Get("Retry-After"))
			}
			if got := recorder.Header().Get("Retry-After"); got != row.retry {
				t.Fatalf("Retry-After = %q, want %q", got, row.retry)
			}
		})
	}
}
