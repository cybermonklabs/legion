package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestJoinSocketURLDialsEachSchemeAsItsWebsocketsAndRefusesTheRest: an http URL's socket is ws://
// and an https URL's wss://, each with the path joined onto the URL's path and its query after the
// URL's own; a URL of another scheme, and one that does not parse, are refused in fixed words
// that quote none of it, the credentials of its user information and query among it.
func TestJoinSocketURLDialsEachSchemeAsItsWebsocketsAndRefusesTheRest(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:18777":                "ws://127.0.0.1:18777/ws/doc/a?schema_version=1",
		"https://dispatch.example/prefix?key=v": "wss://dispatch.example/prefix/ws/doc/a?key=v&schema_version=1",
		"HTTPS://dispatch.example/prefix/":      "wss://dispatch.example/prefix/ws/doc/a?schema_version=1",
	} {
		if got, err := joinSocketURL(in, "/ws/doc/a?schema_version=1"); err != nil || got != want {
			t.Errorf("joinSocketURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	credentials := redacttest.User + ":" + redacttest.Password + "@"
	query := "?access" + "_token=" + redacttest.Token
	for in, want := range map[string]string{
		"ftp://" + credentials + "dispatch.example/" + query: "the Dispatch URL is neither http:// nor https:// (it is not printed)",
		"http://" + credentials + "127.0.0.1:1/%zz" + query:  "the Dispatch URL is not an absolute URL with a host (it is not printed)",
		"/dispatch" + query: "the Dispatch URL is not an absolute URL with a host (it is not printed)",
	} {
		got, err := joinSocketURL(in, "/ws/live")
		if err == nil {
			t.Errorf("joinSocketURL took %q as %q", in, got)
			continue
		}
		if err.Error() != want || len(redacttest.Held(err.Error())) != 0 {
			t.Errorf("joinSocketURL refused a URL with %q; want %q, with none of the URL", err, want)
		}
	}
}
