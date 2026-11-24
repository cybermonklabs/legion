package mcpbridge

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEndpointOnTheConfiguredOriginCarriesTheConfiguredCredential: the configured URL carries a
// credential in its user information, and the MCP server's message route takes a POST only with
// that credential as its Basic Authorization, answering any other 401. An endpoint on the
// configured URL's origin is posted to with the configured credential and the handshake completes,
// whether the server names it by a path (relative, as before), absolutely (absolute) or by a
// network-path reference (network-path). The rows of an endpoint on another origin, which takes
// none of it, are TestAnEndpointResolvesAgainstTheSSEURL/elsewhere and /elsewhere-absolute.
func TestAnEndpointOnTheConfiguredOriginCarriesTheConfiguredCredential(t *testing.T) {
	const session = "s3ss10n-7Qz4k"
	configured := redacttest.User + ":" + redacttest.Password
	for _, c := range []struct {
		name     string
		endpoint func(host string) string
	}{
		{"relative", func(string) string { return "/message?session_id=" + session }},
		{"absolute", func(host string) string { return "http://" + host + "/message?session_id=" + session }},
		{"network-path", func(host string) string { return "//" + host + "/message?session_id=" + session }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var posts kept
			address := standIn(t, c.endpoint, requiring(&posts))
			s := NewHTTPServer(ServerConfig{Name: "origin", Transport: "http", URL: withUser("http://" + address)}, func(string) {})
			err := s.Start()
			if err == nil {
				s.Stop()
			}
			lines := posts.lines()
			if len(lines) == 0 {
				t.Fatalf("the server's endpoint was sent no POST (Start = %v)", err)
			}
			for _, line := range lines {
				if want := "/message?session_id=" + session + " " + configured; line != want {
					t.Errorf("the server's endpoint was sent %q; want each POST to carry the configured credential: %q", line, want)
				}
			}
			if err != nil {
				t.Errorf("Start = %v; want the handshake to complete with the configured credential", err)
			}
		})
	}
}

// TestAnEndpointThatIsNoHTTPURLPrintsNoneOfIt: the MCP server's SSE endpoint event names an
// endpoint that resolves to no http or https URL with a host, with an access token in its query: an
// opaque URL of another scheme (opaque, x:message?...) or an http URL with no host (hostless,
// http:/message?...). net/http posts to neither, and the rule reads each as words, which it prints
// as they came, since neither holds a scheme and two slashes. Start fails in the bridge's own words
// and prints none of the endpoint. The twin shows the rule's reading of each endpoint holds the
// token, so each row can see it printed. Captures mcpbridge.endpoint-not-a-url/<case>.
func TestAnEndpointThatIsNoHTTPURLPrintsNoneOfIt(t *testing.T) {
	query := "?session_id=s3ss10n-7Qz4k&access" + "_token=" + redacttest.Token
	cases := []struct{ name, endpoint string }{
		{"opaque", "x:message" + query},
		{"hostless", "http:/message" + query},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			if shown := redact.Field(c.endpoint); !strings.Contains(shown, redacttest.Token) {
				t.Errorf("redact.Field(%q) = %q; want the rule to print the token of an endpoint it reads as words, so the %s row can see it", c.endpoint, shown, c.name)
			}
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewHTTPServer(ServerConfig{Name: "not-a-url", Transport: "http", URL: "http://" + standIn(t, func(string) string { return c.endpoint }, refusing)}, func(string) {})
			err := s.Start()
			if err == nil {
				s.Stop()
				t.Fatal("Start succeeded against an endpoint that is no http URL")
			}
			redacttest.RequireNoPiece(t, "mcpbridge.endpoint-not-a-url/"+c.name, err.Error(), redacttest.Token)
			if want := "read endpoint event: endpoint URL in SSE event is not an http or https URL with a host"; err.Error() != want {
				t.Errorf("Start = %q; want %q", err, want)
			}
		})
	}
}

// TestAnEndpointThatDoesNotParseAgainPrintsNoneOfIt: the MCP server's SSE endpoint event names an
// absolute endpoint that url.Parse takes and whose text, as url.URL.String writes it, url.Parse
// refuses (an IPv6 zone holding a byte that is not ASCII, as the configured URL of
// TestAnSSEURLThatDoesNotParseAgainPrintsNoneOfIt does), with redacttest's credentials in its user
// information and its query. The message POST fails on net/http's parse of that text, and Start's
// error names the endpoint as the rule reads it, which cannot read it, and the failure by its
// class, with none of the credentials (reparsed). The twin shows net/http's error for that POST
// quotes them. Captures mcpbridge.endpoint-reparsed.
func TestAnEndpointThatDoesNotParseAgainPrintsNoneOfIt(t *testing.T) {
	endpoint := "http://" + redacttest.User + ":" + redacttest.Password + "@[::%25\xfd]/message?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			t.Fatalf("url.Parse of the endpoint = %v; want it taken", err)
		}
		if _, err := http.Post(parsed.String(), "application/json", strings.NewReader(`{}`)); err == nil || len(redacttest.Held(err.Error())) == 0 {
			t.Errorf("net/http's error for the POST = %v; want it to quote the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		s := NewHTTPServer(ServerConfig{Name: "reparsed", Transport: "http", URL: "http://" + standIn(t, func(string) string { return endpoint }, refusing)}, func(string) {})
		err := s.Start()
		if err == nil {
			s.Stop()
			t.Fatal("Start succeeded against an endpoint that does not parse again")
		}
		redacttest.RequireNoPiece(t, "mcpbridge.endpoint-reparsed", err.Error())
		if want := "POST to " + redact.WordsNotReadable + ": " + redact.ClassNotAURL; !strings.Contains(err.Error(), want) {
			t.Errorf("Start = %q; want %q", err, want)
		}
	})
}

// requiring is a /message route that keeps each POST in posts, refuses one whose Basic
// Authorization is not redacttest's user and password with 401, and answers the rest as handshake
// does.
func requiring(posts *kept) func(say func(string), w http.ResponseWriter, r *http.Request) {
	answer := handshake(posts)
	return func(say func(string), w http.ResponseWriter, r *http.Request) {
		if user, password, _ := r.BasicAuth(); user != redacttest.User || password != redacttest.Password {
			posts.keep(r)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		answer(say, w, r)
	}
}
