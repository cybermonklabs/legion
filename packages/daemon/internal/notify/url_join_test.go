package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestAListenerCallGoesToTheRouteJoinedOntoTheListenerURL: the Envoy URL can carry a path and a
// query. Each of the publisher's four calls goes to the URL's path with its route joined onto it,
// the URL's query kept ahead of the route's own: http://host/envoy?sig=... is asked
// /envoy/v1/messages/publish?sig=... for a publish, /envoy/v1/registry/<session>?sig=...&include=...
// for the registration read, /envoy/v1/roles/<role>?sig=... for the role lookup and
// /envoy/v1/sessions/<session>?sig=...&if_not_registered_after=... for the delete (query), and so
// is http://host/envoy/?sig=... (query-trailing-slash), where the route appended to the URL's text
// sat inside the query (/envoy?sig=.../v1/messages/publish) and the listener was asked /envoy. A
// URL with no query is asked as it was (no-query). A stand-in listener records the path and the
// query each call reaches it with, and answers each so that the call succeeds wherever it lands.
func TestAListenerCallGoesToTheRouteJoinedOntoTheListenerURL(t *testing.T) {
	const notRegisteredAfter = 1700000000000
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/envoy?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"query-trailing-slash", "/envoy/?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"no-query", "/envoy", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := make(chan url.URL, 4)
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked <- *r.URL
				switch r.Method {
				case http.MethodPost:
					w.WriteHeader(http.StatusOK)
				case http.MethodGet:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, `{"reason":"unclaimed"}`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer listener.Close()
			publisher := New(listener.URL+c.suffix, "envoy-token")
			// with is the query a call reaches the listener with: the URL's own, then the route's.
			with := func(own string) string { return strings.TrimPrefix(c.query+"&"+own, "&") }
			ask := func(call, path, query string, err error) {
				t.Helper()
				select {
				case got := <-asked:
					if got.Path != path || got.RawQuery != query {
						t.Errorf("%s at a listener URL ending %q asked path %q, query %q; want path %q, query %q", call, c.suffix, got.Path, got.RawQuery, path, query)
					}
				default:
					t.Fatalf("%s reached no listener; it answered %v", call, err)
				}
				if err != nil {
					t.Errorf("%s = %v; want the listener's answer taken", call, err)
				}
			}
			ctx := context.Background()
			_, err := publisher.Publish(ctx, "notifications.legion.legion.controller", "a notice", map[string]string{}, "dedupe-1")
			ask("Publish", "/envoy/v1/messages/publish", c.query, err)
			_, err = publisher.ReadSessionRegistration(ctx, "ses_tester")
			ask("ReadSessionRegistration", "/envoy/v1/registry/ses_tester", with("include=unresponsive&view=registration"), err)
			_, _, err = publisher.RoleHolder(ctx, "architect")
			ask("RoleHolder", "/envoy/v1/roles/architect", c.query, err)
			_, err = publisher.DeleteSession(ctx, "ses_tester", DeleteConditions{NotRegisteredAfter: time.UnixMilli(notRegisteredAfter)})
			ask("DeleteSession", "/envoy/v1/sessions/ses_tester", with("if_not_registered_after=1700000000000"), err)
		})
	}
}

// TestAListenerURLThatDoesNotParsePrintsNoneOfIt: an Envoy URL url.Parse refuses (an invalid
// escape in its path), with made-up credentials in its user information and its query, fails the
// publish in the publisher's own words, which hold none of the URL (unparsed); http.NewRequest's
// error for that URL quotes it whole (twin). A URL url.Parse takes whose joined URL, as
// url.URL.String writes it, url.Parse refuses (an IPv6 zone holding a byte that is not ASCII)
// fails through oauth2call.Unanswered, which names a URL it cannot read by no part of it
// (reparsed); its twin shows http.NewRequest's error for the joined URL holds the credentials.
// Captures notify.envoy-url/<case>.
func TestAListenerURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	const (
		user     = "not-a-secret-user"
		password = "not-a-secret-password"
		topic    = "notifications.legion.legion.controller"
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
	publish := func(t *testing.T, envoyURL string) string {
		t.Helper()
		_, err := New(envoyURL, "envoy-token").Publish(context.Background(), topic, "a notice", map[string]string{}, "dedupe-1")
		if err == nil {
			t.Fatal("Publish took a listener URL that does not parse")
		}
		return err.Error()
	}
	t.Run("twin", func(t *testing.T) {
		_, err := http.NewRequest(http.MethodPost, unparsed+"/v1/messages/publish", nil)
		if err == nil {
			t.Fatalf("http.NewRequest took %q; the row needs a URL that does not parse", unparsed)
		}
		holds(t, err.Error())
	})
	t.Run("unparsed", func(t *testing.T) {
		said := publish(t, unparsed)
		oauth2calltest.RequireNoPiece(t, "notify.envoy-url/unparsed", said, oauth2calltest.Token, user, password)
		if want := "build notice publish request for " + topic + ": the Envoy listener's URL is not an absolute URL with a host (it is not printed)"; said != want {
			t.Errorf("Publish's error is %q, want %q", said, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused %q (%v); the row needs a listener URL it takes", reparsed, err)
		}
		_, err = http.NewRequest(http.MethodPost, parsed.JoinPath("/v1/messages/publish").String(), nil)
		if err == nil {
			t.Fatal("http.NewRequest took the joined URL; the row needs one it refuses")
		}
		holds(t, err.Error())
	})
	t.Run("reparsed", func(t *testing.T) {
		oauth2calltest.RequireNoPiece(t, "notify.envoy-url/reparsed", publish(t, reparsed), oauth2calltest.Token, user, password)
	})
}
