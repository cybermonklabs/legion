// Package approvetimingtest times, in tests, how long a machine-login approve takes from the moment
// Dispatch's handler is handed it to the moment the broker a test runs receives its relay. It is
// test support, imported by the tests of internal/dispatch/api and cmd/dispatch alone. Once a test
// package calls Enable (each does from an init in a _test.go file when Env is set, since Dispatch
// reads the process environment only through its settings table and a test file is where a test's
// own variable is read), each relay the broker receives prints one line, "APPROVE-RELAYED-IN
// <nanoseconds> ns", to standard error; the greatest of them under load is Dispatch's half of the
// measured in-flight time internal/cipher/machine.ApprovalInFlightWindow's comment gives as context
// beside its bound. Until then both wrappers answer the handler they are given, and nothing is
// timed.
package approvetimingtest

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Env is the variable that, set to anything, makes a test package call Enable.
const Env = "ENVOY_TIME_APPROVALS"

// enabled is whether Enable was called; the wrappers read it when a test builds its handlers.
var enabled atomic.Bool

// Enable turns the timing on for the handlers built after it.
func Enable() { enabled.Store(true) }

// starts is, by the URL of the broker a test runs, when the approve now in flight to it was handed
// to Dispatch's handler.
var starts sync.Map

// Approves is handler noting, once Enable was called, when each approve of a machine login reaches
// it, for the broker at brokerURL to time its relay against (Broker).
func Approves(brokerURL string, handler http.Handler) http.Handler {
	if !enabled.Load() {
		return handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/credential-requests/") && strings.HasSuffix(r.URL.Path, "/approve") {
			starts.Store(brokerURL, time.Now())
		}
		handler.ServeHTTP(w, r)
	})
}

// Broker is a broker's handler printing, once Enable was called, how long after Dispatch's handler
// was handed each approve the broker received its relay; url answers the broker's own URL when the
// relay arrives, since a test's broker learns its URL only once its server starts.
func Broker(url func() string, handler http.Handler) http.Handler {
	if !enabled.Load() {
		return handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/credential-requests/") && strings.HasSuffix(r.URL.Path, "/approve") {
			if started, ok := starts.LoadAndDelete(url()); ok {
				fmt.Fprintf(os.Stderr, "APPROVE-RELAYED-IN %d ns\n", time.Since(started.(time.Time)).Nanoseconds())
			}
		}
		handler.ServeHTTP(w, r)
	})
}
