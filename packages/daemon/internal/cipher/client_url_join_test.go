package cipher

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestABrokerCallGoesToTheRouteJoinedOntoTheBrokerURL: the broker URL a Client is given can carry
// a path and a query. Each call goes to the URL's path with its route joined onto it, the URL's
// query kept: http://host/broker?sig=... is asked /broker/v1/enrollments?sig=... for an enrollment
// and /broker/v1/enrollments/enr-1?sig=... for its revocation (query), and so is
// http://host/broker/?sig=... (query-trailing-slash), where the route appended to the URL's text
// sat inside the query (/broker?sig=.../v1/enrollments) and the broker was asked /broker. A URL with
// no query is asked as it was (no-query). Each call's launcher proof names the URL the call asked
// without its query and its user information (its htu), which is what the broker verifies a proof
// against; a URL with user information (user-information) names none of it there. A stand-in
// broker records the path, the query and the proof's htu of each call.
func TestABrokerCallGoesToTheRouteJoinedOntoTheBrokerURL(t *testing.T) {
	type call struct {
		method, path, query, htu string
	}
	for _, c := range []struct {
		name, suffix, query string
		user                bool
	}{
		{"query", "/broker?sig=s1g-7Qz4k", "sig=s1g-7Qz4k", false},
		{"query-trailing-slash", "/broker/?sig=s1g-7Qz4k", "sig=s1g-7Qz4k", false},
		{"no-query", "/broker", "", false},
		{"user-information", "/broker", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls := make(chan call, 2)
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, payload, err := decodeCompactJWSRaw(r.Header.Get("Proof"))
				if err != nil {
					t.Errorf("%s %s carried no launcher proof that decodes: %v", r.Method, r.URL.Path, err)
				}
				htu, _ := payload["htu"].(string)
				calls <- call{r.Method, r.URL.Path, r.URL.RawQuery, htu}
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"enrollment_id":"enr-1","lease_expires_at":"2026-11-25T00:00:00Z"}`)
			}))
			defer broker.Close()
			base := broker.URL + c.suffix
			if c.user {
				withUser, err := url.Parse(base)
				if err != nil {
					t.Fatal(err)
				}
				withUser.User = url.UserPassword("broker-user", "broker-word")
				base = withUser.String()
			}
			client := &Client{URL: base, HTTP: broker.Client()}
			withCredential(t, client, "cred-1")
			ask := func(name string, want call, err error) {
				t.Helper()
				select {
				case got := <-calls:
					if got != want {
						t.Errorf("%s at a broker URL ending %q asked %+v; want %+v", name, c.suffix, got, want)
					}
				default:
					t.Fatalf("%s reached no broker; it answered %v", name, err)
				}
				if err != nil {
					t.Errorf("%s = %v; want the broker's answer taken", name, err)
				}
			}
			_, err := client.Enroll(context.Background(), podEnrollment)
			ask("Enroll", call{http.MethodPost, "/broker/v1/enrollments", c.query, broker.URL + "/broker/v1/enrollments"}, err)
			err = client.Revoke(context.Background(), "enr-1")
			ask("Revoke", call{http.MethodDelete, "/broker/v1/enrollments/enr-1", c.query, broker.URL + "/broker/v1/enrollments/enr-1"}, err)
		})
	}
}

// TestABrokerURLThatDoesNotParsePrintsNoneOfIt: a broker URL url.Parse refuses (an invalid escape
// in its path), with made-up credentials in its user information and its query, fails the call in
// the client's own words, which hold none of the URL (unparsed); http.NewRequest's error for that
// URL quotes it whole (twin). A URL url.Parse takes whose joined URL, as url.URL.String writes it,
// url.Parse refuses (an IPv6 zone holding a byte that is not ASCII) fails through
// oauth2call.Unanswered, which names a URL it cannot read by no part of it (reparsed); its twin
// shows http.NewRequest's error for the joined URL holds the credentials. Captures
// cipher.broker-url/<case>.
func TestABrokerURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	const (
		user     = "not-a-secret-user"
		password = "not-a-secret-password"
	)
	unparsed := "http://" + user + ":" + password + "@127.0.0.1:1/broker%zz?sig=" + oauth2calltest.Token
	reparsed := "http://" + user + ":" + password + "@[::%25\xfd]/broker?sig=" + oauth2calltest.Token
	holds := func(t *testing.T, text string) {
		t.Helper()
		for _, credential := range []string{user, password, oauth2calltest.Token} {
			if !strings.Contains(text, credential) {
				t.Fatalf("%q holds no %q, so the row cannot see a leak", text, credential)
			}
		}
	}
	enroll := func(t *testing.T, brokerURL string) string {
		t.Helper()
		client := &Client{URL: brokerURL, HTTP: &http.Client{}}
		withCredential(t, client, "cred-1")
		_, err := client.Enroll(context.Background(), podEnrollment)
		if err == nil {
			t.Fatal("Enroll took a broker URL that does not parse")
		}
		return err.Error()
	}
	t.Run("twin", func(t *testing.T) {
		_, err := http.NewRequest(http.MethodPost, unparsed+"/v1/enrollments", nil)
		if err == nil {
			t.Fatalf("http.NewRequest took %q; the row needs a URL that does not parse", unparsed)
		}
		holds(t, err.Error())
	})
	t.Run("unparsed", func(t *testing.T) {
		said := enroll(t, unparsed)
		oauth2calltest.RequireNoPiece(t, "cipher.broker-url/unparsed", said, oauth2calltest.Token, user, password)
		if want := "broker POST /v1/enrollments: the broker URL is not an absolute URL with a host (it is not printed)"; said != want {
			t.Errorf("Enroll's error is %q, want %q", said, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused %q (%v); the row needs a broker URL it takes", reparsed, err)
		}
		_, err = http.NewRequest(http.MethodPost, parsed.JoinPath("/v1/enrollments").String(), nil)
		if err == nil {
			t.Fatal("http.NewRequest took the joined URL; the row needs one it refuses")
		}
		holds(t, err.Error())
	})
	t.Run("reparsed", func(t *testing.T) {
		oauth2calltest.RequireNoPiece(t, "cipher.broker-url/reparsed", enroll(t, reparsed), oauth2calltest.Token, user, password)
	})
}
