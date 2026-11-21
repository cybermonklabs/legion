package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestARegistrationRefusalPrintsTheListenersWordsAsTheRuleReadsThem: the listener, or whatever
// answers in its place, refuses a registration with 409 and words that quote a credential's URL.
// register's error names the session, the status and the listener's words as the rule reads them
// (redact.Answer.Words): the URL with no user information and no query value. Captures
// refillrig.register-refused.
func TestARegistrationRefusalPrintsTheListenersWordsAsTheRuleReadsThem(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/interests/subscribe" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, "the registration is refused: %s", redacttest.Words)
	}))
	defer listener.Close()
	err := runRegister([]string{"--url", listener.URL, "--sets", "words=1", "--concurrency", "1"})
	if err == nil {
		t.Fatal("register succeeded against a listener that refuses every registration")
	}
	redacttest.RequireNone(t, "refillrig.register-refused", err.Error())
	if want := "subscribe " + sessionID("words", 0) + ": 409 the registration is refused: " + redacttest.Shown; !strings.Contains(err.Error(), want) {
		t.Errorf("register = %q; want %q, the listener's words as the rule reads them", err, want)
	}
}
