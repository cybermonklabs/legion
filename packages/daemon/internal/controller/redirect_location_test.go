package controller

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/redirects/redirectstest"
)

// TestALivenessRedirectIsUnknownAndItsLogNamesNoRunOfTheBearer: whatever answers at the Envoy
// listener's address answers the controller role lookup, which carries the listener's bearer,
// with a redirect whose Location quotes that bearer. One net/http cannot parse (unparsed) is
// net/http's error whatever the client's redirect policy, which the probe logs; one that parses,
// to a stand-in that answers (followed), was followed. Each is Unknown, its log holds no run of
// the bearer nor a word of the Location, and the stand-in a Location names is asked nothing: the
// probe follows no redirect, so a 3xx is the listener's refusal. The twin shows the stand-in's
// echo reaches net/http's own error. Captures controller.redirect/<case>.
func TestALivenessRedirectIsUnknownAndItsLogNamesNoRunOfTheBearer(t *testing.T) {
	const bearer = "zq7wk-not-a-secret-RDX-77ab"
	target, asked := redirectstest.Target(t)
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, redirectstest.Echo(t, "Authorization", redirectstest.Unparsed)+"/v1/roles/legion-demo-controller", nil)
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
		{"followed", func(credential string) string {
			return redirectstest.To(target, strings.TrimPrefix(credential, "Bearer "))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			if got := newTestProber(redirectstest.Echo(t, "Authorization", c.location), bearer, &logged).Probe(context.Background(), "ses_ctl"); got != Unknown {
				t.Fatalf("Probe = %s, want %s", got, Unknown)
			}
			oauth2calltest.RequireNoPiece(t, "controller.redirect/"+c.name, logged.String(), bearer)
			if strings.Contains(logged.String(), "moved") {
				t.Errorf("the probe's log names the Location: %s", logged.String())
			}
			if n := asked(); n != 0 {
				t.Errorf("the redirect's target was asked %d times, want none", n)
			}
		})
	}
}
