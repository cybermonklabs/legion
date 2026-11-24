package controller

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestALivenessLookupGoesToTheRouteJoinedOntoTheEnvoyURL: the Envoy URL can carry a path and a
// query. The controller role lookup goes to the URL's path with the role route joined onto it, the
// URL's query kept: http://host/envoy?sig=... is asked /envoy/v1/roles/legion-demo-controller?sig=...
// (query), and so is http://host/envoy/?sig=... (query-trailing-slash), where the route appended to
// the URL's text sat inside the query (/envoy?sig=.../v1/roles/legion-demo-controller) and the
// listener was asked /envoy. A URL with no query is asked as it was (no-query). A stand-in listener
// records the path and the query the lookup reaches it with, and names the session the holder.
func TestALivenessLookupGoesToTheRouteJoinedOntoTheEnvoyURL(t *testing.T) {
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/envoy?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"query-trailing-slash", "/envoy/?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"no-query", "/envoy", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := make(chan url.URL, 1)
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked <- *r.URL
				_, _ = io.WriteString(w, holder("ses_ctl", probeNow))
			}))
			defer listener.Close()
			var logged bytes.Buffer
			got := newTestProber(listener.URL+c.suffix, "envoy-token", &logged).Probe(context.Background(), "ses_ctl")
			select {
			case lookup := <-asked:
				if want := "/envoy/v1/roles/legion-demo-controller"; lookup.Path != want || lookup.RawQuery != c.query {
					t.Errorf("an Envoy URL ending %q was asked path %q, query %q; want path %q, query %q", c.suffix, lookup.Path, lookup.RawQuery, want, c.query)
				}
			default:
				t.Fatalf("the lookup reached no listener; the probe logged %s", logged.String())
			}
			if got != Live {
				t.Errorf("Probe = %s, want %s; it logged %s", got, Live, logged.String())
			}
		})
	}
}

// TestAnEnvoyURLThatDoesNotParseIsLoggedByNoneOfIt: an Envoy URL url.Parse refuses (an invalid
// escape in its path), with made-up credentials in its user information and its query, leaves the
// probe no lookup: each probe is Unknown and logs fixed words, which hold none of the URL
// (unparsed), where it logged the lookup's text whole. A URL url.Parse takes whose joined URL, as
// url.URL.String writes it, url.Parse refuses (an IPv6 zone holding a byte that is not ASCII) is
// logged only through oauth2call.Unanswered, which names a URL it cannot read by no part of it
// (reparsed); the twin shows http.NewRequest's error for that joined URL holds the credentials.
// Captures controller.envoy-url/<case>.
func TestAnEnvoyURLThatDoesNotParseIsLoggedByNoneOfIt(t *testing.T) {
	const (
		user     = "not-a-secret-user"
		password = "not-a-secret-password"
	)
	unparsed := "http://" + user + ":" + password + "@127.0.0.1:1/envoy%zz?sig=" + oauth2calltest.Token
	reparsed := "http://" + user + ":" + password + "@[::%25\xfd]/envoy?sig=" + oauth2calltest.Token
	holds := func(t *testing.T, text string) {
		t.Helper()
		for _, credential := range []string{user, password, oauth2calltest.Token} {
			if !strings.Contains(text, credential) {
				t.Fatalf("%q holds no %q, so the row cannot see a leak", text, credential)
			}
		}
	}
	probe := func(t *testing.T, envoyURL string) string {
		t.Helper()
		var logged bytes.Buffer
		if got := newTestProber(envoyURL, "envoy-token", &logged).Probe(context.Background(), "ses_ctl"); got != Unknown {
			t.Fatalf("Probe = %s, want %s", got, Unknown)
		}
		return logged.String()
	}
	t.Run("twin", func(t *testing.T) {
		_, err := http.NewRequest(http.MethodGet, unparsed+"/v1/roles/legion-demo-controller", nil)
		if err == nil {
			t.Fatalf("http.NewRequest took %q; the row needs a URL that does not parse", unparsed)
		}
		holds(t, err.Error())
	})
	t.Run("unparsed", func(t *testing.T) {
		logged := probe(t, unparsed)
		oauth2calltest.RequireNoPiece(t, "controller.envoy-url/unparsed", logged, oauth2calltest.Token, user, password)
		if want := "the Envoy URL is not an absolute URL with a host (it is not printed)"; !strings.Contains(logged, want) {
			t.Errorf("the probe logged %s; want %q", logged, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused %q (%v); the row needs an Envoy URL it takes", reparsed, err)
		}
		_, err = http.NewRequest(http.MethodGet, parsed.JoinPath("/v1/roles/legion-demo-controller").String(), nil)
		if err == nil {
			t.Fatal("http.NewRequest took the joined URL; the row needs one it refuses")
		}
		holds(t, err.Error())
	})
	t.Run("reparsed", func(t *testing.T) {
		oauth2calltest.RequireNoPiece(t, "controller.envoy-url/reparsed", probe(t, reparsed), oauth2calltest.Token, user, password)
	})
}
