package cipher

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
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
	dead := redirectstest.Dead(t)
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, redirectstest.EchoBearer(t, redirectstest.Unparsed)+"/v1/requests", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		redirectstest.RequireEcho(t, request, token)
	})
	for _, c := range []struct {
		name     string
		location func(bearer string) string
	}{
		{"unparsed", redirectstest.Unparsed},
		{"elsewhere", func(bearer string) string { return redirectstest.To(dead, bearer) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(redirectstest.EchoBearer(t, c.location), tokenFile(t, token)).Pending(context.Background(), "sjawhar")
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
