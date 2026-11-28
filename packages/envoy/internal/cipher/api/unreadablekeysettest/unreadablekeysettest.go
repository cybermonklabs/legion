// Package unreadablekeysettest reads the secrets broker's own answers to a token check that
// reached no verdict because the key set of the token's issuer could not be read: one answer of
// each shape a client reads, captured from the broker's real HTTP server into
// internal/cipher/api/testdata/unreadable-key-set-answers.json by internal/cipher/api's
// TestTheUnreadableKeySetAnswersAreTheFixture, which fails in every other run where the server
// answers otherwise. A client's test replays the answer of its shape (Answer.Write) rather than a
// body written by hand, so it reads what the broker sends. It is an ordinary package rather than a
// _test.go file so the broker's test and every client's read one copy; Go cannot share test-only
// code across packages. It imports the standard library alone, so a test of any package, one
// inside a package the broker's own code imports included, imports it without a cycle.
package unreadablekeysettest

//ci:reads packages/envoy/internal/cipher/api/testdata/unreadable-key-set-answers.json Read joins ../testdata/unreadable-key-set-answers.json from this file's own path and reads it for every client's test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Answers is the file: what it holds and how it was made (_comment, which a reader skips), and
// the broker's answer of each shape a client reads.
type Answers struct {
	Comment string `json:"_comment"`
	// A JSON route's: POST /v1/oauth/introspect with the caller's own token, 503
	// {"code":"ISSUER_UNAVAILABLE","error":...}.
	JSONRoute Answer `json:"json_route"`
	// The token endpoint's: POST /v1/oauth/token, a pod's token exchange, 503 OAuth's error
	// object {"error":"temporarily_unavailable","error_description":...,"code":"ISSUER_UNAVAILABLE"}.
	TokenEndpoint Answer `json:"token_endpoint"`
	// A route of the NATS writer hold's: GET /v1/nats-hold with the operator's token, 503
	// {"error":"issuer_unavailable","message":...}.
	HoldRoute Answer `json:"hold_route"`
}

// Answer is one answer as a client reads it: the request that drew it, its status, its
// Retry-After and Content-Type headers, and its body. Read gives the body as the broker wrote it
// with the white space between its JSON tokens taken out (json.Compact), which takes out the
// newline the broker's encoder ends it with too.
type Answer struct {
	Request     string          `json:"request"`
	Status      int             `json:"status"`
	RetryAfter  string          `json:"retry_after"`
	ContentType string          `json:"content_type"`
	Body        json.RawMessage `json:"body"`
}

// Write answers w with a as the broker sent it: its Content-Type, its Retry-After when it has
// one, its status and its body. A test whose row is about another wait writes its copy that asks
// for that wait instead (Asking).
func (a Answer) Write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", a.ContentType)
	if a.RetryAfter != "" {
		w.Header().Set("Retry-After", a.RetryAfter)
	}
	w.WriteHeader(a.Status)
	_, _ = w.Write(a.Body)
}

// Asking is a with its Retry-After replaced by seconds, "" for none.
func (a Answer) Asking(seconds string) Answer {
	a.RetryAfter = seconds
	return a
}

// Captured is the answer a server sent to request, its status, header and body, as the file holds
// it: its status, its Retry-After and Content-Type headers, and its body with the white space
// between its JSON tokens taken out (json.Compact). A body that is not JSON fails t.
func Captured(t testing.TB, request string, status int, header http.Header, body []byte) Answer {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		t.Fatalf("%s answered %d with a body that is not JSON: %v: %s", request, status, err, body)
	}
	return Answer{
		Request: request, Status: status, RetryAfter: header.Get("Retry-After"),
		ContentType: header.Get("Content-Type"), Body: compact.Bytes(),
	}
}

// Read is the file, found from this file's own path rather than from the process's working
// directory, so the test of every package reads the one file wherever its binary runs. A member
// the file holds and Answers does not, or an answer with no JSON body, fails t.
func Read(t testing.TB) Answers {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("this test binary carries no path for unreadablekeysettest.go")
	}
	path := filepath.Join(filepath.Dir(self), "..", "testdata", "unreadable-key-set-answers.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the broker's answers through an unreadable key set: %v", err)
	}
	var answers Answers
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&answers); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	for name, answer := range map[string]*Answer{"json_route": &answers.JSONRoute, "token_endpoint": &answers.TokenEndpoint, "hold_route": &answers.HoldRoute} {
		var body bytes.Buffer
		if err := json.Compact(&body, answer.Body); err != nil {
			t.Fatalf("%s's %s holds no JSON body: %v", path, name, err)
		}
		answer.Body = body.Bytes()
	}
	return answers
}
