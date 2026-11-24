package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestANATSServerNameNotTheOneAskedForPrintsAsTheRuleReadsIt: the NATS monitor, or whatever
// answers in its place, names a server that is not the one the guard asks for, in words that quote
// a credential's URL. The guard's error names that server as the rule reads it (the name held as
// redact.RemoteText), and none of what the monitor's stand-in wrote. Captures
// loadtest.answer-words/nats-server-name.
func TestANATSServerNameNotTheOneAskedForPrintsAsTheRuleReadsIt(t *testing.T) {
	monitor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"server_name":"` + redacttest.Words + `"}`))
	}))
	t.Cleanup(monitor.Close)
	err := checkNATSServerName(context.Background(), monitor.Client(), monitor.URL, "loadtest-nats")
	if err == nil {
		t.Fatal("checkNATSServerName answered no error")
	}
	redacttest.RequireNone(t, "loadtest.answer-words/nats-server-name", err.Error())
	if want := `server_name is "` + redacttest.Shown + `", want "loadtest-nats"`; !strings.Contains(err.Error(), want) {
		t.Errorf("the guard's error is %q, want %q in it", err, want)
	}
}
