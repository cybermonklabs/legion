package main

import (
	"net/http"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/daemon"
)

// The daemon's clean stop of a dead role's Envoy session sends its registry read and its delete
// through a transport of its own, a clone of Go's shared http.DefaultTransport with each fixed
// limit removed (notify.stopTransport), so that only the stop's context,
// daemon.CleanSessionStopBudget from when it is sent, ends them. An init in any package of the
// daemon's binary can change that shared transport, and a test sees only the inits its own binary
// links: this package's test binary links every package `legion` does, its own files included, so
// the checks of the shared transport are here. It must be an *http.Transport, the type the stop's
// transport is cloned from (another type leaves the stop to Go's defaults and drops the proxy and
// TLS configuration the clone would keep); it must have no response-header timeout; and its
// TLS-handshake limit, when it has one, must be after the budget. The stop removes both limits from
// its clone, so these hold the shared transport for what else in the binary sends through it.
// Its connect limit is no field a test can read:
// TestAStalledConnectThroughGosSharedTransportLastsTheCleanStopsBudget stalls a connect.
func TestGosSharedTransportLeavesTheCleanStopToItsBudget(t *testing.T) {
	shared, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport, which the clean stop's transport is cloned from, is %T, want an *http.Transport", http.DefaultTransport)
	}
	if shared.ResponseHeaderTimeout != 0 {
		t.Fatalf("http.DefaultTransport, which the clean stop's transport is cloned from, has a response-header timeout of %s, want none",
			shared.ResponseHeaderTimeout)
	}
	if timeout := shared.TLSHandshakeTimeout; timeout != 0 && timeout <= daemon.CleanSessionStopBudget {
		t.Fatalf("http.DefaultTransport, which the clean stop's transport is cloned from, has a TLS handshake timeout of %s, not after the clean stop's budget, daemon.CleanSessionStopBudget (%s)",
			timeout, daemon.CleanSessionStopBudget)
	}
}
