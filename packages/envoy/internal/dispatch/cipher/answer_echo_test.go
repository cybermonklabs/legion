package cipher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestARelayedRefusalCarriesNoTokenItsAnswerEchoes: an answer in the broker's place refuses
// Dispatch's call with a body that echoes the account token the call carried. The *Error Dispatch
// relays to the browser holds none of it, in its body or its text, and still names the status and
// the code: the client relays a refusal with every credential the call carried redacted
// (redact.Carried). Captures dispatch-agentsecrets.answer-echo.
func TestARelayedRefusalCarriesNoTokenItsAnswerEchoes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"code":"UNAUTHORIZED","error":"the bearer %s is not one this broker holds"}`, r.Header.Get("Authorization"))
	}))
	defer server.Close()
	_, err := New(server.URL, tokenFile(t, redacttest.Token)).Pending(context.Background(), "sjawhar")
	var refused *Error
	if !errors.As(err, &refused) {
		t.Fatalf("Pending = %v, want the broker's refusal", err)
	}
	redacttest.RequireNone(t, "dispatch-agentsecrets.answer-echo", fmt.Sprintf("%s | %v | %s", refused.Body, err, refused.Error()))
	if refused.Status != http.StatusUnauthorized || refused.Code != "UNAUTHORIZED" {
		t.Errorf("the refusal is %d %q, want 401 UNAUTHORIZED", refused.Status, refused.Code)
	}
}
