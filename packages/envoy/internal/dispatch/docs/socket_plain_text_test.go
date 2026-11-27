package docs

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// answer is one response a test server gave, as its peer reads it: the status, the two headers a
// plain-text answer carries, and the body.
type answer struct {
	status             int
	contentType, sniff string
	body               []byte
}

// served is what a server whose handler writes through write answers a GET, read over a real
// connection.
func served(t *testing.T, write func(http.ResponseWriter)) answer {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { write(w) }))
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{response.StatusCode, response.Header.Get("Content-Type"), response.Header.Get("X-Content-Type-Options"), body}
}

// revocable writes through a revocableSocketWriter over w whose admission stands, as ygo writes
// a document socket's answer before its upgrade.
func revocable(write func(http.ResponseWriter)) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		write(&revocableSocketWriter{ResponseWriter: w, ctx: context.Background()})
	}
}

// A document socket's answer before its upgrade is plain text, never read as HTML. ygo answers
// through http.Error (its 401s, 404 "invalid room name", 503 "room full" and the rest), and those
// answers reach the peer byte for byte as they would with no revocableSocketWriter in the way:
// status, Content-Type, X-Content-Type-Options and body. A write that no WriteHeader went before,
// whose bytes net/http would sniff as HTML, is sent as text/plain with nosniff.
//
// Control: forward Write's bytes without setting the two headers, as it did before, and the
// second row fails: the sniffed answer is text/html with no nosniff.
func TestASocketsAnswerBeforeItsUpgradeIsPlainText(t *testing.T) {
	for _, refusal := range []struct {
		text   string
		status int
	}{
		{"unauthorized", http.StatusUnauthorized},
		{"invalid room name", http.StatusNotFound},
		{"room full", http.StatusServiceUnavailable},
	} {
		write := func(w http.ResponseWriter) { http.Error(w, refusal.text, refusal.status) }
		direct, through := served(t, write), served(t, revocable(write))
		if through.status != direct.status || through.contentType != direct.contentType || through.sniff != direct.sniff || !bytes.Equal(through.body, direct.body) {
			t.Errorf("http.Error(%q, %d) through the socket's writer answered %+v, want %+v as with none", refusal.text, refusal.status, through, direct)
		}
		if through.contentType != "text/plain; charset=utf-8" || through.sniff != "nosniff" || string(through.body) != refusal.text+"\n" {
			t.Errorf("http.Error(%q, %d) through the socket's writer answered %+v, want http.Error's plain text", refusal.text, refusal.status, through)
		}
	}

	markup := []byte("<html><script>alert(1)</script></html>")
	write := func(w http.ResponseWriter) { _, _ = w.Write(markup) }
	if direct := served(t, write); direct.contentType != "text/html; charset=utf-8" {
		t.Fatalf("the bare write was answered as %q; want net/http to sniff it as HTML, the case this row guards", direct.contentType)
	}
	through := served(t, revocable(write))
	if through.status != http.StatusOK || through.contentType != "text/plain; charset=utf-8" || through.sniff != "nosniff" || !bytes.Equal(through.body, markup) {
		t.Errorf("a write with no WriteHeader before it, through the socket's writer, answered %+v; want 200 text/plain with nosniff and the bytes written", through)
	}
}
