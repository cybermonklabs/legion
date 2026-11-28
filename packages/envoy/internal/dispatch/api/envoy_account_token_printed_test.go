package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
)

// A token pasted into ENVOY_ACCOUNT_TOKEN_FILE in place of the file's path prints in neither of
// the two places a failure to present Dispatch's identity reaches: the 503 ENVOY_UNAVAILABLE
// answer a listener read carries the error in, and Dispatch's log line of the call the listener
// never took. Each names the setting and why the file cannot be read. The listener is never
// asked, since no token could be presented. The list is read straight from the listener, so this
// asks the handler body with no database behind it.
func TestAPastedAccountTokenPrintsInNeitherTheAnswerNorTheLog(t *testing.T) {
	t.Parallel()
	const pasted = "madeUpEnvoyAccountToken0123456789abcdefXYZ"
	var asked atomic.Int32
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(listener.Close)
	var logs bytes.Buffer
	s := &server{deps: Deps{Envoy: envoy.New(listener.URL, envoy.WithAccountTokenFile(pasted),
		envoy.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))}}

	response := httptest.NewRecorder()
	s.writeSubscribers(response, httptest.NewRequest(http.MethodGet, "/api/v1/issues/TEST-1/subscribers", nil), issueSubscriberBase("TEST-1"))
	body := response.Body.String()
	const why = "ENVOY_ACCOUNT_TOKEN_FILE names a file that cannot be read: it does not exist"
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"ENVOY_UNAVAILABLE"`) || !strings.Contains(body, why) {
		t.Fatalf("a subscriber list on a pasted token = %d %s, want 503 ENVOY_UNAVAILABLE saying %q", response.Code, body, why)
	}
	if !strings.Contains(logs.String(), why) {
		t.Fatalf("the log holds no line saying %q:\n%s", why, logs.String())
	}
	for _, printed := range []struct{ where, text string }{{"the 503 answer", body}, {"the log", logs.String()}} {
		for i := 0; i+10 <= len(pasted); i++ {
			if strings.Contains(printed.text, pasted[i:i+10]) {
				t.Fatalf("%s holds %q of the pasted token:\n%s", printed.where, pasted[i:i+10], printed.text)
			}
		}
	}
	if got := asked.Load(); got != 0 {
		t.Fatalf("the listener was asked %d times, want none: no token could be presented", got)
	}
}
