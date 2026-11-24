package api

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAGoingActionGoesToTheRowJoinedOntoTheBaseURL: GoingCaller's BaseURL can carry a path and a
// query. An action goes to the URL's path with its row's path joined onto it and the URL's query
// kept: http://host/dispatch?sig=... files an issue at /dispatch/api/v1/issues?sig=... (query),
// where the path appended to the URL's text sat inside the query
// (/dispatch?sig=.../api/v1/issues, Dispatch asked /dispatch with the query's value changed). A
// URL with neither goes where it did (no-query). An issue's row has no placeholder and a follow's
// fills two; the client records each request it was asked to send.
func TestAGoingActionGoesToTheRowJoinedOntoTheBaseURL(t *testing.T) {
	t.Parallel()
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatalf("build the catalogue: %v", err)
	}
	const ask = "01234567-0000-4000-8000-000000000001"
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/dispatch?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"no-query", "/dispatch", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			doer := &countingDoer{}
			caller := GoingCaller{Catalogue: catalogue, BaseURL: "http://dispatch.invalid:8080" + c.suffix, Session: "sess-7", Doer: doer}
			if _, err := caller.Do(context.Background(), "dispatch_issue", map[string]any{"project": "CORE", "title": "A new issue"}); err != nil {
				t.Fatalf("file an issue: %v", err)
			}
			if _, err := caller.Do(context.Background(), "dispatch_follow", map[string]any{"ask": ask, "action": "follow"}); err != nil {
				t.Fatalf("follow an ask: %v", err)
			}
			want := []url.URL{
				{Host: "dispatch.invalid:8080", Path: "/dispatch/api/v1/issues", RawQuery: c.query},
				{Host: "dispatch.invalid:8080", Path: "/dispatch/api/v1/asks/" + ask + "/followers/sess-7", RawQuery: c.query},
			}
			if len(doer.requests) != len(want) {
				t.Fatalf("the caller sent %d requests, want %d", len(doer.requests), len(want))
			}
			for i, sent := range doer.requests {
				if sent.URL.Host != want[i].Host || sent.URL.Path != want[i].Path || sent.URL.RawQuery != want[i].RawQuery {
					t.Errorf("a base URL ending %q: request %d went to host %q, path %q, query %q; want host %q, path %q, query %q",
						c.suffix, i, sent.URL.Host, sent.URL.Path, sent.URL.RawQuery, want[i].Host, want[i].Path, want[i].RawQuery)
				}
			}
		})
	}
}

// TestAGoingBaseURLThatDoesNotParsePrintsNoneOfIt: a BaseURL url.Parse refuses (an invalid escape
// in its path), with redacttest's credentials in its user information and its query, fails the
// action in the caller's own words, the action and its row named and the failure's class, with
// none of the URL (unparsed); http.NewRequest's error, which Do returned as it came, quotes it
// whole (twin). A BaseURL that parses, whose joined URL, as url.URL.String writes it, url.Parse
// refuses (an IPv6 zone holding a byte that is not ASCII, which String escapes as %FD), is refused
// the same way (reparsed); its twin shows http.NewRequest's error for the joined URL holds the
// credentials. Nothing is sent. Captures api.going-base-url/<case>.
func TestAGoingBaseURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	t.Parallel()
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatalf("build the catalogue: %v", err)
	}
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	unparsed := "http://" + userinfo + "127.0.0.1:1/dispatch%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/dispatch?" + param + "=" + redacttest.Token
	const wantSaid = "dispatch_issue: build the POST /api/v1/issues request: not a valid URL"
	perform := func(t *testing.T, base string) string {
		t.Helper()
		doer := &countingDoer{}
		_, err := GoingCaller{Catalogue: catalogue, BaseURL: base, Doer: doer}.Do(context.Background(), "dispatch_issue", map[string]any{"project": "CORE", "title": "A new issue"})
		if err == nil {
			t.Fatal("Do performed an action at a base URL that does not parse")
		}
		if len(doer.requests) != 0 {
			t.Fatalf("Do sent %d request(s) for a base URL that does not parse", len(doer.requests))
		}
		return err.Error()
	}
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodPost, unparsed+"/api/v1/issues", nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := perform(t, unparsed)
		redacttest.RequireNoPiece(t, "api.going-base-url/unparsed", said)
		if said != wantSaid {
			t.Errorf("Do = %q; want %q", said, wantSaid)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the base URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodPost, parsed.JoinPath("api/v1/issues").String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		said := perform(t, reparsed)
		redacttest.RequireNoPiece(t, "api.going-base-url/reparsed", said)
		if said != wantSaid {
			t.Errorf("Do = %q; want %q", said, wantSaid)
		}
	})
}
