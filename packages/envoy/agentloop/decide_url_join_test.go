package agentloop

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestADecisionGoesToThePathJoinedOntoTheBaseURL: the endpoint's base URL is the caller's, and it
// can carry a path, a query or a fragment. A decision is posted to the URL's path with the System
// One path joined onto it, the query kept and the fragment dropped: http://host/base?sig=... is
// posted to /base/v1/systemone?sig=... (query), and so is http://host/base/?sig=...
// (query-trailing-slash), where the path appended to the URL's text sat inside the query
// (/base?sig=.../v1/systemone, the endpoint asked /base with the query's value changed);
// http://host/base#... is posted to /base/v1/systemone (fragment), where the path sat inside the
// fragment. A URL with neither is posted where it was (no-query). The stand-in records the path and
// the query the decision reaches it with.
func TestADecisionGoesToThePathJoinedOntoTheBaseURL(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/base?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"query-trailing-slash", "/base/?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"fragment", "/base#pane", ""},
		{"no-query", "/base", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStandIn(t, reply{body: []byte(decideAnswer)})
			req := newDecideFixture(t, s)
			req.Model.Endpoint.BaseURL = s.server.URL + c.suffix
			result, err := Decide(context.Background(), req)
			got := s.requests()
			if len(got) != 1 {
				t.Fatalf("%d requests reached the endpoint, want 1; Decide = %+v, %v", len(got), result, err)
			}
			if got[0].path != "/base/v1/systemone" || got[0].query != c.query {
				t.Errorf("a base URL ending %q was posted to path %q, query %q; want path %q, query %q", c.suffix, got[0].path, got[0].query, "/base/v1/systemone", c.query)
			}
			if err != nil || result.Fault != "" {
				t.Errorf("Decide = %+v, %v; want the decision answered", result, err)
			}
		})
	}
}

// TestASystemOneBaseURLThatDoesNotParsePrintsNoneOfIt: a base URL url.Parse refuses (an invalid
// escape in its path), with redacttest's credentials in its user information and its query, fails
// the decision in fixed words, with none of the URL (unparsed); http.NewRequest's error, which the
// request's construction printed, quotes it whole (twin). Nothing reaches the wire. Captures
// agentloop.decide-url/unparsed.
func TestASystemOneBaseURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	t.Parallel()
	unparsed := "http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/base%zz?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodPost, unparsed+decidePath, nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		s := newStandIn(t)
		req := newDecideFixture(t, s)
		req.Model.Endpoint.BaseURL = unparsed
		_, err := Decide(context.Background(), req)
		if err == nil {
			t.Fatal("Decide made a decision at a base URL that does not parse")
		}
		redacttest.RequireNoPiece(t, "agentloop.decide-url/unparsed", err.Error())
		if want := "agentloop: the systemone endpoint's base URL does not parse"; err.Error() != want {
			t.Errorf("Decide = %q; want %q", err, want)
		}
		if n := len(s.requests()); n != 0 {
			t.Errorf("%d requests reached the wire, want none", n)
		}
	})
}
