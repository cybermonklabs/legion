package wake

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestANoticeGoesToThePathJoinedOntoTheEnvoyURL: BROKER_ENVOY_URL is the deployment's, and it can
// carry a path, a query or a fragment. A notice is posted to that URL's path with
// /v1/messages/send joined onto it, the query kept and the fragment dropped:
// http://host/envoy?sig=... is posted to /envoy/v1/messages/send?sig=... (query), and so is
// http://host/envoy/?sig=... (query-trailing-slash), where the path appended to the URL's text sat
// inside the query (/envoy?sig=.../v1/messages/send, the listener asked /envoy with the query's
// value changed); http://host/envoy#pane is posted to /envoy/v1/messages/send with no fragment,
// which no request sends (fragment). A URL with neither is posted where it was (no-query). The
// stand-in records the path and the query the notice reaches it with.
func TestANoticeGoesToThePathJoinedOntoTheEnvoyURL(t *testing.T) {
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/envoy?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"query-trailing-slash", "/envoy/?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"fragment", "/envoy#pane", ""},
		{"no-query", "/envoy", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var asked []url.URL
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked = append(asked, *r.URL)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			NewEnvoy(srv.URL+c.suffix, staticAccount("tok"), srv.Client()).
				Notify(context.Background(), "sess-123", "req-456", "approved")
			if len(asked) != 1 {
				t.Fatalf("%d notices reached the listener, want 1", len(asked))
			}
			if want := "/envoy" + sendPath; asked[0].Path != want || asked[0].RawQuery != c.query {
				t.Errorf("a URL ending %q was posted to path %q, query %q; want path %q, query %q",
					c.suffix, asked[0].Path, asked[0].RawQuery, want, c.query)
			}
		})
	}
}

// TestAnEnvoyURLThatDoesNotParsePrintsNoneOfIt: a BROKER_ENVOY_URL with redacttest's credentials
// in its user information and its query that url.Parse refuses (an invalid escape in its path) is
// logged in this package's own words, with none of the URL (unparsed); one url.Parse takes whose
// joined URL url.URL.String writes as text url.Parse refuses again (an IPv6 zone escape) is logged
// in its own words too (reparsed). The twins show net/http's own error for each quotes the
// credentials, so the rows can see one printed. Nothing reaches the wire. Captures
// wake.envoy-url/<case>.
func TestAnEnvoyURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	param := "access" + "_token"
	unparsed := "http://" + userinfo + "127.0.0.1:1/envoy%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/envoy?" + param + "=" + redacttest.Token
	notice := func(t *testing.T, base string) string {
		t.Helper()
		var records []slog.Record
		logtest.SetDefault(t, capturingHandler{records: &records})
		sent := false
		NewEnvoy(base, &countingAccount{token: "tok", sent: &sent}, &http.Client{}).
			Notify(context.Background(), "sess-123", "req-456", "approved")
		if sent {
			t.Error("the account token was read for a notice that could not be built")
		}
		if len(records) != 1 {
			t.Fatalf("%d lines were logged, want the one naming the URL it could not use", len(records))
		}
		said := records[0].Message
		records[0].Attrs(func(a slog.Attr) bool {
			said += " " + a.Key + "=" + a.Value.String()
			return true
		})
		return said
	}
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodPost, unparsed+sendPath, nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := notice(t, unparsed)
		redacttest.RequireNoPiece(t, "wake.envoy-url/unparsed", said)
		if want := "wake envoy: build request session=sess-123 request=req-456 error=BROKER_ENVOY_URL is not an absolute URL with a host"; said != want {
			t.Errorf("the line logged = %q; want %q", said, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the Envoy URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodPost, parsed.JoinPath(sendPath).String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		said := notice(t, reparsed)
		redacttest.RequireNoPiece(t, "wake.envoy-url/reparsed", said)
		if want := "wake envoy: build request session=sess-123 request=req-456 error=the URL joined from BROKER_ENVOY_URL does not parse"; said != want {
			t.Errorf("the line logged = %q; want %q", said, want)
		}
	})
}

// countingAccount is an account that records whether its token was ever read.
type countingAccount struct {
	token string
	sent  *bool
}

func (a *countingAccount) Read() (string, error) {
	*a.sent = true
	return a.token, nil
}
