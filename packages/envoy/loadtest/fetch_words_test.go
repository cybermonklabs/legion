package main

import (
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAFetchPrintsNoCredentialOfItsURL: a foreign -pprof may carry user information, which
// fetchTo's GET sends as Basic credentials, and whatever answers there answers with a redirect
// whose Location does not parse and quotes the password (userinfoEcho). fetchTo's log line holds
// neither the user name, which net/http's error keeps in the URL it names, nor a run of the
// password, and names the failure by its class (unparsed). A failure beside a URL the rule prints
// still prints its transport text (refused). The twin shows net/http's own error holds the
// password. Captures loadtest.fetch-words/unparsed.
func TestAFetchPrintsNoCredentialOfItsURL(t *testing.T) {
	base := userinfoEcho(t)
	address := base + "/debug/pprof/heap"
	fetch := func(t *testing.T, base string) string {
		t.Helper()
		var logged strings.Builder
		fetchTo(base, "/debug/pprof/heap", filepath.Join(t.TempDir(), "heap.pprof"), 10*time.Second, log.New(&logged, "", 0))
		return logged.String()
	}
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, address, nil)
		if err != nil {
			t.Fatal(err)
		}
		redirectstest.RequireEcho(t, request, redacttest.Password)
	})
	t.Run("unparsed", func(t *testing.T) {
		logged := fetch(t, base)
		redacttest.RequireNoPiece(t, "loadtest.fetch-words/unparsed", logged, redacttest.Password)
		if !strings.HasSuffix(logged, "/debug/pprof/heap: "+redact.ClassRequestFailed+"\n") {
			t.Errorf("fetchTo logged %q, want the failure by its class", logged)
		}
	})
	t.Run("refused", func(t *testing.T) {
		if logged := fetch(t, "http://"+redirectstest.Dead(t)); !strings.Contains(logged, "connect: connection refused") {
			t.Errorf("fetchTo logged %q, want the refused connection's own words", logged)
		}
	})
}
