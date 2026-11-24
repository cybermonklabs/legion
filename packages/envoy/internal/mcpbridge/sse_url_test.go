package mcpbridge

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestTheSSEURLKeepsTheConfiguredQuery: the configured URL names a path and carries a query (a
// key, say), and the bridge connects to that path's /sse with the query kept and completes the
// handshake: http://host/mcp?key=... connects to /mcp/sse?key=... (query), and so does
// http://host/mcp/?key=... (query-trailing-slash), where /sse appended to the URL's text sat inside
// the query (/mcp?key=.../sse, the server's /mcp asked with the key's value changed). A URL with no
// query connects to /mcp/sse, as it did (no-query). The endpoint event names an absolute URL, so
// the rows read the SSE URL alone.
func TestTheSSEURLKeepsTheConfiguredQuery(t *testing.T) {
	const key = "k3y-7Qz4k"
	for _, c := range []struct{ name, configured, want string }{
		{"query", "/mcp?key=" + key, "/mcp/sse?key=" + key},
		{"query-trailing-slash", "/mcp/?key=" + key, "/mcp/sse?key=" + key},
		{"no-query", "/mcp", "/mcp/sse"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var connects, posts kept
			endpoint := func(host string) string { return "http://" + host + "/mcp/message" }
			address := standInAt(t, "/mcp", &connects, endpoint, handshake(&posts))
			s := NewHTTPServer(ServerConfig{Name: "query", Transport: "http", URL: "http://" + address + c.configured}, func(string) {})
			err := s.Start()
			if err == nil {
				s.Stop()
			}
			if lines := connects.lines(); len(lines) != 1 || !strings.HasPrefix(lines[0], c.want+" ") {
				t.Errorf("with %s the server was asked %q; want one connect to %s", c.configured, lines, c.want)
			}
			if err != nil {
				t.Fatalf("Start with %s = %v; want the handshake to complete", c.configured, err)
			}
			if len(posts.lines()) == 0 {
				t.Error("the server's endpoint was sent no POST")
			}
		})
	}
}

// TestAConfiguredURLThatDoesNotParsePrintsNoneOfIt: a configured URL that url.Parse refuses (an
// invalid escape), with redacttest's credentials in its user information and its query, fails
// Start in the bridge's own words, with none of the URL (unparsed): url.Parse's error, which the
// request's construction printed, quotes it whole. LoadConfig refuses such a URL before any Start
// (ServerConfig.validate); a server built from a ServerConfig directly does not. The twin shows
// url.Parse's error holds the credentials. Captures mcpbridge.sse-url/unparsed.
func TestAConfiguredURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	unparsed := unparsedEndpoint("127.0.0.1:1")
	t.Run("twin", func(t *testing.T) {
		if _, err := url.Parse(unparsed); err == nil || len(redacttest.Held(err.Error())) == 0 {
			t.Errorf("url.Parse(%q) = %v; want an error that quotes the credentials, so the row can see them", unparsed, err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		err := NewHTTPServer(ServerConfig{Name: "unparsed", Transport: "http", URL: unparsed}, func(string) {}).Start()
		if err == nil {
			t.Fatal("Start succeeded with a configured URL that does not parse")
		}
		redacttest.RequireNoPiece(t, "mcpbridge.sse-url/unparsed", err.Error())
		if want := "create SSE request: the configured URL does not parse"; err.Error() != want {
			t.Errorf("Start = %q; want %q", err, want)
		}
	})
}

// TestAnSSEURLThatDoesNotParseAgainPrintsNoneOfIt: a configured URL that url.Parse takes and that
// ServerConfig.validate passes, whose text, as url.URL.String writes it, url.Parse refuses: an
// IPv6 zone holding a byte that is not ASCII, which String escapes as %FD and a second parse
// refuses (a fuzzer found it). It carries redacttest's credentials in its user information and its
// query. Start fails in the bridge's own words, with none of the URL (reparsed): http.NewRequest's
// error, which the request's construction printed, quotes the SSE URL whole. The twin shows the
// configured URL passes validate and that http.NewRequest's error for its SSE URL holds the
// credentials. Captures mcpbridge.sse-url/reparsed.
func TestAnSSEURLThatDoesNotParseAgainPrintsNoneOfIt(t *testing.T) {
	configured := "http://" + redacttest.User + ":" + redacttest.Password + "@[::%25\xfd]/mcp?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		cfg := httpServerConfig(configured)
		if err := cfg.validate(); err != nil {
			t.Errorf("validate of the configured URL = %v; want it passed, so a loaded configuration can hold it", err)
		}
		parsed, err := url.Parse(configured)
		if err != nil {
			t.Fatalf("url.Parse of the configured URL = %v; want it taken", err)
		}
		if _, err := http.NewRequest(http.MethodGet, parsed.JoinPath("sse").String(), nil); err == nil || len(redacttest.Held(err.Error())) == 0 {
			t.Errorf("http.NewRequest of the SSE URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		err := NewHTTPServer(ServerConfig{Name: "reparsed", Transport: "http", URL: configured}, func(string) {}).Start()
		if err == nil {
			t.Fatal("Start succeeded with an SSE URL that does not parse")
		}
		redacttest.RequireNoPiece(t, "mcpbridge.sse-url/reparsed", err.Error())
		if want := "create SSE request: the SSE URL does not parse"; err.Error() != want {
			t.Errorf("Start = %q; want %q", err, want)
		}
	})
}
