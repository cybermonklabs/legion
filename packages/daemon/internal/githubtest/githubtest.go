// Package githubtest holds the fake GitHub server the daemon's tests share. A test that drives
// code which reads GitHub stands one up: an httptest server answering the handful of paths that
// code asks for, closed when the test ends. What every such fake does the same way — the server
// itself, the gate on the credential of the code under test, the record it keeps of each request,
// the bearer gate, the JSON content type and the close — is Server's; what the test proves stays
// in the test's own handler: its switch, its default arm, and the state it keeps and reads.
// Unexpected is the default arm of a fake for which any request it did not plan for is a failure.
package githubtest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Spec is what a fake GitHub server is made of beyond its own handler.
type Spec struct {
	// Credential, when set, reports whether a request carries a credential the code under test
	// presents to GitHub. Any other request is another process's: the loopback port the kernel
	// gives a fake may be one a process of the machine still sends to. It is answered 404 and
	// logged before Record, Token and Handle see it, so it fails nothing and is recorded as
	// nothing, and a fake's power to fail the test stays with the code under test's own requests.
	Credential func(*http.Request) bool
	// Token, when set, is the bearer every request must carry; one that does not gets 401.
	Token string
	// Record, when set, is called with each request before the handler, from the server's own
	// goroutine, so a recorder a test reads keeps a lock of its own.
	Record func(*http.Request)
	// Handle is the caller's own switch, its default arm included.
	Handle http.HandlerFunc
}

// Server starts a fake GitHub server and returns its URL; it is closed when the test ends. t is a
// testing.TB, so a test may run a fake under a testing.TB of its own that keeps what it reports.
func Server(t testing.TB, spec Spec) string {
	t.Helper()
	return Serve(t, spec).URL
}

// Serve is Server for a fake that needs the server itself rather than its URL — its client, its
// handler, or a close of its own beside the one Serve registers.
func Serve(t testing.TB, spec Spec) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if spec.Credential != nil && !spec.Credential(r) {
			t.Logf("a GitHub stand-in answered 404 to %.16q %.200q, which carries no credential of the code under test's", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if spec.Record != nil {
			spec.Record(r)
		}
		if spec.Token != "" && r.Header.Get("Authorization") != "Bearer "+spec.Token {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		spec.Handle(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// Unexpected answers a request the fake did not expect: it fails the test naming the method and
// path, and answers 404.
func Unexpected(t testing.TB, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
}
