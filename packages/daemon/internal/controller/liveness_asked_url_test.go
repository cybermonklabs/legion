package controller

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestALivenessLineNamesTheLookupWithNoCredentialTheEnvoyURLCarries: envoy_url may carry the
// listener's credential as its user information (config's endpointURL refuses a query but not
// that), and the lookup joined onto it holds it. Each line a probe logs names the lookup as the URL
// rule names a URL a request asked (oauth2call.AskedURL: scheme, host and path), never as joined,
// so none prints a run of the URL's user, password or token: the listener unreachable, a 404 the
// probe could not read to its end, a holder that is only unresponsive, a role with no live holder,
// a refusal, and an answer the probe cannot read. The twin shows the lookup as joined holds every
// marker. Captures controller.asked-url/<case>.
func TestALivenessLineNamesTheLookupWithNoCredentialTheEnvoyURLCarries(t *testing.T) {
	const (
		user     = "not-a-secret-user"
		password = "not-a-secret-password"
	)
	envoyURL := func(host string) string {
		return "http://" + user + ":" + password + "@" + host + "/envoy?sig=" + oauth2calltest.Token
	}
	hostOf := func(base string) string { return strings.TrimPrefix(base, "http://") }
	t.Run("twin", func(t *testing.T) {
		var log bytes.Buffer
		lookup := newTestProber(envoyURL("127.0.0.1:1"), "envoy-bearer", &log).lookup
		for _, marker := range []string{user, password, oauth2calltest.Token} {
			if !strings.Contains(lookup, marker) {
				t.Errorf("the lookup as joined, %q, holds no %q, so the rows cannot see it printed", lookup, marker)
			}
		}
	})
	serving := func(status int, body string) func(t *testing.T) string {
		return func(t *testing.T) string {
			return hostOf((&listener{status: status, body: body}).serve(t))
		}
	}
	for _, c := range []struct {
		name string
		host func(t *testing.T) string
		want Liveness
	}{
		{"unreachable", func(t *testing.T) string { return redirectstest.Dead(t) }, Unknown},
		{"cut-404", func(t *testing.T) string { return hostOf(cutAnswer(t, http.StatusNotFound, lapsed("unresponsive"))) }, Unknown},
		{"unresponsive", serving(http.StatusNotFound, lapsed("unresponsive")), Unknown},
		{"gone", serving(http.StatusNotFound, lapsed("gone")), Gone},
		{"refused", serving(http.StatusForbidden, `{"error":"forbidden"}`), Unknown},
		{"unreadable", serving(http.StatusOK, "not json"), Unknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			var log bytes.Buffer
			host := c.host(t)
			if got := newTestProber(envoyURL(host), "envoy-bearer", &log).Probe(context.Background(), "ses_ctl"); got != c.want {
				t.Errorf("Probe = %q, want %q; it logged %s", got, c.want, log.String())
			}
			logged := log.String()
			if logged == "" {
				t.Fatal("the probe logged nothing; want the line that names the lookup")
			}
			oauth2calltest.RequireNoPiece(t, "controller.asked-url/"+c.name, logged, user, password, oauth2calltest.Token)
			if want := "url=http://" + host + "/envoy/v1/roles/"; !strings.Contains(logged, want) {
				t.Errorf("the probe logged %s; want the lookup named %q…", logged, want)
			}
		})
	}
}
