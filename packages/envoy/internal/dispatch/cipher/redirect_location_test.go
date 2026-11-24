package cipher

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABrokerRedirectPrintsNoLocationOfIt: whatever answers at the broker's address answers
// Dispatch's call, which carries Dispatch's own account token, with a redirect whose Location
// quotes that token. One net/http cannot parse (unparsed) is net/http's error whatever the
// client's redirect policy, since Go parses a Location before it asks CheckRedirect; one that
// parses names an address on another host nothing listens at (elsewhere), which a following
// client would ask, naming the Location in its error. The call's error, which Dispatch logs,
// holds no run of the token and no word of the Location. The twin shows the stand-in's echo
// reaches net/http's own error. Captures dispatch-agentsecrets.redirect/<case>.
func TestABrokerRedirectPrintsNoLocationOfIt(t *testing.T) {
	const token = "jw5qa-not-a-secret-RDX-0b7e"
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := closed.Addr().String()
	_ = closed.Close()
	echo := func(location func(bearer string) string) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", location(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
			w.WriteHeader(http.StatusFound)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	unparsed := func(bearer string) string { return "/moved%zz" + bearer }
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, echo(unparsed)+"/v1/requests", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("net/http answered the unparsed Location with no error: the rows cannot see one")
		}
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("net/http's own error holds no echo of the token, so the rows cannot see a leak: %q", err)
		}
	})
	for _, c := range []struct {
		name     string
		location func(bearer string) string
	}{
		{"unparsed", unparsed},
		{"elsewhere", func(bearer string) string { return "http://" + dead + "/moved?access_token=" + bearer }},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(echo(c.location), tokenFile(t, token)).Pending(context.Background(), "sjawhar")
			if err == nil {
				t.Fatal("the call answered no error")
			}
			redacttest.RequireNoPiece(t, "dispatch-agentsecrets.redirect/"+c.name, err.Error(), token)
			if strings.Contains(err.Error(), "moved") {
				t.Errorf("the call's error names the Location: %q", err)
			}
		})
	}
}
