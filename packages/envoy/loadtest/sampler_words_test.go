package main

import (
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestASamplerReadPrintsNoCredentialOfItsURL: a foreign -pprof may carry user information, which
// the sampler's goroutine read (getOK) sends as Basic credentials, and whatever answers there
// answers with a redirect whose Location does not parse and quotes the password (userinfoEcho).
// The sampler's log line of the failed read (readErrors.add) holds neither the user name, which
// net/http's error keeps in the URL it names, nor a run of the password, and names the failure by
// its class. The twin shows net/http's own error holds the password. Captures
// loadtest.sampler-words/pprof.
func TestASamplerReadPrintsNoCredentialOfItsURL(t *testing.T) {
	pprof := userinfoEcho(t)
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, pprof+"/debug/pprof/goroutine?debug=1", nil)
		if err != nil {
			t.Fatal(err)
		}
		redirectstest.RequireEcho(t, request, redacttest.Password)
	})
	t.Run("pprof", func(t *testing.T) {
		_, _, err := goroutineCounts(pprof)
		if err == nil {
			t.Fatal("goroutineCounts read a profile from a server that answers a redirect")
		}
		var logged strings.Builder
		errs := readErrors{name: "sampler: pprof", lg: log.New(&logged, "", 0)}
		errs.add(err)
		redacttest.RequireNoPiece(t, "loadtest.sampler-words/pprof", logged.String(), redacttest.Password)
		if want := "sampler: pprof: " + redact.ClassRequestFailed + "\n"; logged.String() != want {
			t.Errorf("the sampler logged %q, want %q", logged.String(), want)
		}
	})
}
