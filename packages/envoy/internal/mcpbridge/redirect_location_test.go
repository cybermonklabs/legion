package mcpbridge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAnSSEConnectRedirectPrintsNoLocationOfIt: the configured URL carries a credential in its user
// information, which net/http sends as the SSE connect's Basic Authorization, and whatever answers
// at the server's address answers the connect with a redirect whose Location quotes that password.
// One net/http cannot parse (unparsed) is net/http's error whatever the client's redirect policy,
// since Go parses a Location before it asks CheckRedirect; one that parses names an address on
// another host nothing listens at (elsewhere), which a following client asks, naming the Location
// in its error. Start's error holds no run of the password, no word of the Location and none of the
// URL's user information; the stand-in a Location on another host names is asked nothing
// (followed). A URL with no credential still names itself and the transport's own failure
// (control), and a redirect on the server's own origin is followed (same-origin). The twin shows
// the stand-in's echo reaches net/http's own error. Captures mcpbridge.sse-redirect/<case>.
func TestAnSSEConnectRedirectPrintsNoLocationOfIt(t *testing.T) {
	dead := redirectstest.Dead(t)
	echo := func(location func(password string) string) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, password, _ := r.BasicAuth()
			w.Header().Set("Location", location(password))
			w.WriteHeader(http.StatusFound)
		}))
		t.Cleanup(server.Close)
		return strings.Replace(server.URL, "://", "://"+redacttest.User+":"+redacttest.Password+"@", 1)
	}
	start := func(url string) error {
		s := NewHTTPServer(ServerConfig{Name: "redirect", Transport: "http", URL: url}, func(string) {})
		err := s.Start()
		if err == nil {
			s.Stop()
		}
		return err
	}
	t.Run("twin", func(t *testing.T) {
		response, err := http.Get(echo(redirectstest.Unparsed) + "/sse")
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("net/http answered the unparsed Location with no error: the rows cannot see one")
		}
		if !strings.Contains(err.Error(), redacttest.Password) {
			t.Fatalf("net/http's own error holds no echo of the password, so the rows cannot see a leak: %q", err)
		}
	})
	for _, c := range []struct {
		name     string
		location func(password string) string
	}{
		{"unparsed", redirectstest.Unparsed},
		{"elsewhere", func(password string) string { return redirectstest.To(dead, password) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := start(echo(c.location))
			if err == nil || !strings.Contains(err.Error(), "SSE connect to ") {
				t.Fatalf("Start = %v; want the SSE connect refused", err)
			}
			redacttest.RequireNoPiece(t, "mcpbridge.sse-redirect/"+c.name, err.Error(), redacttest.Password)
			if strings.Contains(err.Error(), "moved") {
				t.Errorf("Start's error names the Location: %q", err)
			}
		})
	}
	t.Run("followed", func(t *testing.T) {
		target, asked := redirectstest.Target(t)
		if err := start(echo(func(password string) string { return redirectstest.To(target, password) })); err == nil {
			t.Fatal("Start succeeded against a server that answers a redirect")
		}
		if n := asked(); n != 0 {
			t.Errorf("the stand-in the Location names was asked %d times; want none", n)
		}
	})
	t.Run("control", func(t *testing.T) {
		err := start("http://" + dead)
		if want := "SSE connect to http://" + dead + "/sse: dial tcp " + dead + ": connect: connection refused"; err == nil || err.Error() != want {
			t.Errorf("Start = %v; want the URL and the transport's own failure: %q", err, want)
		}
	})
	t.Run("same-origin", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/sse" {
				http.Redirect(w, r, "/sse/", http.StatusFound)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(server.Close)
		if err := start(server.URL); err == nil || !strings.HasSuffix(err.Error(), "/sse: status 503") {
			t.Errorf("Start = %v; want the redirect on the server's own origin followed to its 503", err)
		}
	})
}
