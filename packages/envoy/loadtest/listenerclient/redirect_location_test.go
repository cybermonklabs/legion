package listenerclient

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAListenerRedirectPrintsNoLocationOfIt: whatever answers at the listener's address answers a
// call, which carries the stack's bearer, with a redirect whose Location quotes that bearer. One
// net/http cannot parse (unparsed) is net/http's error whatever the client's redirect policy,
// since Go parses a Location before it asks CheckRedirect; one that parses names an address on
// another host nothing listens at (elsewhere), which the client here, one that follows redirects,
// asks, naming the Location in its error. The error of the session-list read (sessions) and of a
// publish (publish) holds no run of the bearer and no word of the Location. The twin shows the
// stand-in's echo reaches net/http's own error. Captures listenerclient.redirect/<call>-<case>.
func TestAListenerRedirectPrintsNoLocationOfIt(t *testing.T) {
	const bearer = "not-a-secret-listener-RDX-0b6d"
	dead := redirectstest.Dead(t)
	echo := func(location func(bearer string) string) string {
		return redirectstest.Echo(t, "Authorization", func(authorization string) string {
			return location(strings.TrimPrefix(authorization, "Bearer "))
		})
	}
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, echo(redirectstest.Unparsed)+SessionsRoute, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+bearer)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("net/http answered the unparsed Location with no error: the rows cannot see one")
		}
		if !strings.Contains(err.Error(), bearer) {
			t.Fatalf("net/http's own error holds no echo of the bearer, so the rows cannot see a leak: %q", err)
		}
	})
	calls := []struct {
		name string
		call func(*Client) error
	}{
		{"sessions", func(c *Client) error {
			_, _, err := c.Sessions(context.Background())
			return err
		}},
		{"publish", func(c *Client) error {
			return c.Publish(context.Background(), listenerwire.PublishRequest{Topic: "notifications.team.a"})
		}},
	}
	for _, call := range calls {
		for _, c := range []struct {
			name     string
			location func(bearer string) string
		}{
			{"unparsed", redirectstest.Unparsed},
			{"elsewhere", func(bearer string) string { return redirectstest.To(dead, bearer) }},
		} {
			t.Run(call.name+"-"+c.name, func(t *testing.T) {
				err := call.call(&Client{URL: echo(c.location), Token: bearer, HTTP: &http.Client{}})
				if err == nil {
					t.Fatal("the call answered no error")
				}
				redacttest.RequireNoPiece(t, "listenerclient.redirect/"+call.name+"-"+c.name, err.Error(), bearer)
				if strings.Contains(err.Error(), "moved") {
					t.Errorf("the call's error names the Location: %q", err)
				}
			})
		}
	}
}
