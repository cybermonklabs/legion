// Package githubtest holds the fake GitHub server Envoy's tests share. A test that drives
// code which reads GitHub stands one up: an httptest server answering the handful of paths that
// code asks for, closed when the test ends. What every such fake does the same way — the server
// itself, the record it keeps of each request, the bearer gate, the JSON content type and the
// close — is Server's; what the test proves stays in the test's own handler: its switch, its
// default arm, and the state it keeps and reads. Unexpected is the default arm of a fake for
// which any request it did not plan for is a failure.
package githubtest

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"
)

// InstallationToken is a stand-in for an installation token GitHub mints, in GitHub's shape:
// "ghs_" and 36 letters and digits, the same for the same seed, so a fake that mints a token and a
// test that names it again name one token. A stand-in of words, such as "installation-token",
// holds runs of GitHub's own words, which the rule cuts from any answer that repeats them, as the
// credential the request carried (redact.Tokens): "for installation ID 7" in GitHub's rate-limit
// message reads "for [redacted] ID 7" beside it. A token GitHub mints holds no such run.
func InstallationToken(seed string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	first := sha256.Sum256([]byte("installation token " + seed))
	second := sha256.Sum256(first[:])
	digest := append(first[:], second[:]...)
	token := []byte("ghs_")
	for _, b := range digest[:36] {
		token = append(token, alphabet[int(b)%len(alphabet)])
	}
	return string(token)
}

// Spec is what a fake GitHub server is made of beyond its own handler.
type Spec struct {
	// Token, when set, is the bearer every request must carry; one that does not gets 401.
	Token string
	// Record, when set, is called with each request before the handler, from the server's own
	// goroutine, so a recorder a test reads keeps a lock of its own.
	Record func(*http.Request)
	// Handle is the caller's own switch, its default arm included.
	Handle http.HandlerFunc
}

// Server starts a fake GitHub server and returns its URL; it is closed when the test ends.
func Server(t *testing.T, spec Spec) string {
	t.Helper()
	return Serve(t, spec).URL
}

// Serve is Server for a fake that needs the server itself rather than its URL — its client, its
// handler, or a close of its own beside the one Serve registers.
func Serve(t *testing.T, spec Spec) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
func Unexpected(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
}
