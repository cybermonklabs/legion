package cipher

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/redirects/redirectstest"
)

// TestABrokerRedirectNamesNoRunOfTheDaemonsBearer: whatever answers at the broker's address
// answers the pod session's registration, which carries the daemon's own ServiceAccount token as
// its bearer, with a redirect whose Location quotes that bearer. One net/http cannot parse
// (unparsed) is net/http's error whatever the client's redirect policy; one that parses names an
// address on another host nothing listens at (elsewhere), which a client that follows redirects
// (a plain http.Client here) follows, so net/http's error names it. The registration's error
// names the call's failure by its class and holds no run of the bearer nor a word of the
// Location. The twin shows the stand-in's echo reaches net/http's own error. Captures
// cipher.redirect/<case>.
func TestABrokerRedirectNamesNoRunOfTheDaemonsBearer(t *testing.T) {
	const bearer = "pw8mr-not-a-secret-RDX-c41f"
	dead := redirectstest.Dead(t)
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, redirectstest.Echo(t, "Authorization", redirectstest.Unparsed)+"/v1/oauth/exchange/pod-sessions", nil)
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
	for _, c := range []struct {
		name     string
		location func(credential string) string
	}{
		{"unparsed", redirectstest.Unparsed},
		{"elsewhere", func(credential string) string {
			return redirectstest.To(dead, strings.TrimPrefix(credential, "Bearer "))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := &Client{URL: redirectstest.Echo(t, "Authorization", c.location), HTTP: &http.Client{}, TokenFile: tokenFile(t, bearer)}
			_, err := client.RegisterPodSession(context.Background(), "pod-uid-1", testPodToken, "ses_implementer", testThumbprint, 7)
			if err == nil {
				t.Fatal("the registration answered no error")
			}
			oauth2calltest.RequireNoPiece(t, "cipher.redirect/"+c.name, err.Error(), bearer)
			if strings.Contains(err.Error(), "moved") {
				t.Errorf("the registration's error names the Location: %q", err)
			}
		})
	}
}
