package main

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// TestALocalRunsPortZeroProxyURLIsTheProxysBoundAddress: a broker on the development store, the
// memory store, started with BROKER_AIRLOCK_PROXY_URL at port 0 hands a result run the address its
// proxy bound as the proxy's URL, never the port-0 URL it was given, as a port-0 BROKER_PUBLIC_URL
// is read as the address the API bound. A local run binds every port as the kernel picks it, so
// the URL it is started with cannot name the proxy's port.
//
// The named revert: make the rewrite in main.go look for anything but a development store, and the
// run is handed http://127.0.0.1:0.
func TestALocalRunsPortZeroProxyURLIsTheProxysBoundAddress(t *testing.T) {
	e := newResultsEnvironment(t, "BROKER_AIRLOCK_PROXY_URL=http://127.0.0.1:0")
	b := e.start(t)
	status, body := postTokenAt(t, b.Addr, "http://"+b.Addr, askedForThePlaceholder(runForm(mintRunToken(t, e.github, e.githubKey, resultsWorkflow, resultsBranch, "1"), "")))
	airlock, _ := body[oauthwire.ExtraProxy].(map[string]any)
	if status != http.StatusOK || airlock == nil {
		t.Fatalf("the run's placeholder exchange = %d %v; want 200 with the airlock's settings", status, body)
	}
	if got, want := airlock["proxy_url"], "http://"+b.proxyAddr; got != want {
		t.Fatalf("the run is handed the proxy's URL %v; want %s, the address the proxy bound", got, want)
	}
}
