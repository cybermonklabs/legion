package judge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestADispatchReadGoesToTheRouteJoinedOntoTheDispatchURL: the Dispatch URL is a setting
// (BROKER_JUDGE_DISPATCH_URL), and it can carry a path, a query or a fragment. Each read goes to
// the URL's path with its route joined onto it, the query kept and the fragment dropped:
// http://host/dispatch?sig=... reads /dispatch/api/v1/issues/<key>?sig=... (query), and so does
// http://host/dispatch/?sig=... (query-trailing-slash), where the route appended to the URL's text
// sat inside the query (/dispatch?sig=.../api/v1/issues/<key>, Dispatch asked /dispatch with the
// query's value changed); http://host/dispatch#... reads /dispatch/api/v1/issues/<key>
// (fragment), where the route sat inside the fragment. A URL with neither reads where it did
// (no-query). Issue reads the issue and its spec, Assignee the issue, and Ask the ask; a stand-in
// Dispatch records the path and the query each read reaches it with.
func TestADispatchReadGoesToTheRouteJoinedOntoTheDispatchURL(t *testing.T) {
	issue := "/dispatch/api/v1/issues/" + testIssue
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/dispatch?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"query-trailing-slash", "/dispatch/?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"fragment", "/dispatch#pane", ""},
		{"no-query", "/dispatch", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			stand := newJoinStandIn(t)
			dispatch := NewDispatch(stand.server.URL+c.suffix, ciphertoken.NewAccountToken("BROKER_JUDGE_DISPATCH_TOKEN_FILE", tokenFile(t, "projected-token")), stand.server.Client())
			if _, err := dispatch.Issue(t.Context(), testIssue); err != nil {
				t.Errorf("Issue: %v", err)
			}
			if _, err := dispatch.Assignee(t.Context(), testIssue); err != nil {
				t.Errorf("Assignee: %v", err)
			}
			if _, err := dispatch.Ask(t.Context(), testAsk); err != nil {
				t.Errorf("Ask: %v", err)
			}
			want := []url.URL{
				{Path: issue, RawQuery: c.query},
				{Path: issue + "/artifacts/spec/text", RawQuery: c.query},
				{Path: issue, RawQuery: c.query},
				{Path: "/dispatch/api/v1/asks/" + testAsk, RawQuery: c.query},
			}
			got := stand.got()
			if len(got) != len(want) {
				t.Fatalf("a Dispatch URL ending %q was asked %v; want %v", c.suffix, got, want)
			}
			for i := range want {
				if got[i].url.Path != want[i].Path || got[i].url.RawQuery != want[i].RawQuery {
					t.Errorf("a Dispatch URL ending %q: read %d went to path %q, query %q; want path %q, query %q", c.suffix, i, got[i].url.Path, got[i].url.RawQuery, want[i].Path, want[i].RawQuery)
				}
				if got[i].authorization != "Bearer projected-token" {
					t.Errorf("a Dispatch URL ending %q: read %d presented %q; want the token its account reads", c.suffix, i, got[i].authorization)
				}
			}
		})
	}
}

// TestADispatchURLThatDoesNotParsePrintsNoneOfIt: a Dispatch URL url.Parse refuses (an invalid
// escape in its path), with redacttest's credentials in its user information and its query, fails
// the read in the judge's own words, with none of the URL (unparsed); http.NewRequest's error,
// which the read's construction printed, quotes it whole (twin). A Dispatch URL that parses, and
// that config.Load's form check passes, whose joined URL, as url.URL.String writes it, url.Parse
// refuses (an IPv6 zone holding a byte that is not ASCII, which String escapes as %FD) is refused
// the same way (reparsed); its twin shows http.NewRequest's error for the joined URL holds the
// credentials. Captures judge.dispatch-url/<case>.
func TestADispatchURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	unparsed := "http://" + userinfo + "127.0.0.1:1/dispatch%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/dispatch?" + param + "=" + redacttest.Token
	read := func(t *testing.T, base string) string {
		t.Helper()
		_, err := NewDispatch(base, ciphertoken.NewAccountToken("BROKER_JUDGE_DISPATCH_TOKEN_FILE", tokenFile(t, "projected-token")), nil).Issue(t.Context(), testIssue)
		if err == nil {
			t.Fatal("Issue read an issue at a Dispatch URL that does not parse")
		}
		return err.Error()
	}
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodGet, unparsed+"/api/v1/issues/"+testIssue, nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := read(t, unparsed)
		redacttest.RequireNoPiece(t, "judge.dispatch-url/unparsed", said)
		if want := "the judge's Dispatch read could not be built: the Dispatch URL does not parse"; said != want {
			t.Errorf("Issue = %q; want %q", said, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the Dispatch URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodGet, parsed.JoinPath("api/v1/issues", testIssue).String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		said := read(t, reparsed)
		redacttest.RequireNoPiece(t, "judge.dispatch-url/reparsed", said)
		if want := "the judge's Dispatch read could not be built: the URL it joined does not parse"; said != want {
			t.Errorf("Issue = %q; want %q", said, want)
		}
	})
}

// joinStandIn answers the issue read, the spec read and the ask read wherever under its address
// they come, and records the URL and the Authorization header of every request it got.
type joinStandIn struct {
	server *httptest.Server
	mu     sync.Mutex
	asked  []askedRead
}

// askedRead is one read the stand-in got: its URL and what it presented.
type askedRead struct {
	url           url.URL
	authorization string
}

func newJoinStandIn(t *testing.T) *joinStandIn {
	t.Helper()
	s := &joinStandIn{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.asked = append(s.asked, askedRead{*r.URL, r.Header.Get("Authorization")})
		s.mu.Unlock()
		body := any(map[string]any{"key": testIssue, "title": "The broker's model judge", "status": "doing", "assignee": testApprover})
		switch {
		case strings.HasSuffix(r.URL.Path, "/artifacts/spec/text"):
			body = map[string]any{"markdown": testSpec}
		case strings.Contains(r.URL.Path, "/api/v1/asks/"):
			body = answeredAsk()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *joinStandIn) got() []askedRead {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]askedRead(nil), s.asked...)
}
