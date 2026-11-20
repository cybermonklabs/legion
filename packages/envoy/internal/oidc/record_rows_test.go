package oidc

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// recordStub answers every request in place of an issuer: status, a status line of reason(r)
// and a body of body(r).
type recordStub struct {
	status       int
	reason, body func(*http.Request) string
}

func (s recordStub) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: s.status, Status: strconv.Itoa(s.status) + " " + s.reason(r),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"text/plain"}},
		Body:   io.NopCloser(strings.NewReader(s.body(r))), Request: r,
	}, nil
}

// afterRoundTrip is base, then after, once base has answered and before the answer goes back up.
type afterRoundTrip struct {
	base  http.RoundTripper
	after func()
}

func (a afterRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := a.base.RoundTrip(r)
	if a.after != nil {
		a.after()
	}
	return resp, err
}

// bearerContext is ctx carrying the composition production code builds for a library over a
// bearer (sessionproxy.impersonationClient): a RefusalClient, oauth2's transport setting the
// bearer, and a RefusalClient over stub beneath it; after runs once that RefusalClient has the
// answer and before it goes back up.
func bearerContext(stub http.RoundTripper, bearer string, after func()) context.Context {
	recorded := redact.RefusalClient(&http.Client{Transport: stub}).Transport
	beneath := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: afterRoundTrip{base: recorded, after: after}})
	client := redact.RefusalClient(oauth2.NewClient(beneath, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: bearer, TokenType: "Bearer"})))
	return context.WithValue(context.Background(), oauth2.HTTPClient, client)
}

// TestADiscoveryRefusalPrintsOnlyItsOwnRequestsCredentials: go-oidc's discovery error, which
// quotes the answer's status line and body (`<status>: <body>`), through NewKeys:
//
//   - interleaved: a gateway's page that shares its first 64 bytes with every refusal and echoes
//     the bearer; while the first discovery's answer is on its way back, a second discovery with
//     another bearer is refused by the same page, and a third request is refused "Forbidden". The
//     first error prints none of its bearer: its record came with it (the review of #4368 at
//     fd07192703, finding 2, which a match on the error's text broke);
//   - status-line-empty-body: an answer with no body whose status line echoes the bearer
//     (the red team's reason-phrase probe).
//
// The designated assertion is oidc.record/<case>.
func TestADiscoveryRefusalPrintsOnlyItsOwnRequestsCredentials(t *testing.T) {
	const second = "second-bearer-RDX-5c1d"
	page := func(r *http.Request) string {
		return "<!DOCTYPE html><html><head><title>403 Forbidden</title></head><body><p>The request carried " +
			r.Header.Get("Authorization") + "</p></body></html>"
	}
	forbidden := func(*http.Request) string { return "Forbidden" }
	t.Run("interleaved", func(t *testing.T) {
		var secondErr error
		later := func() {
			_, secondErr = NewKeys(bearerContext(recordStub{http.StatusForbidden, forbidden, page}, second, nil), "https://issuer.example")
			short := redact.RefusalClient(&http.Client{Transport: recordStub{http.StatusForbidden, forbidden, forbidden}})
			if resp, err := short.Get("https://other.example/"); err == nil {
				_ = resp.Body.Close()
			}
		}
		ctx := bearerContext(recordStub{http.StatusForbidden, forbidden, page}, redacttest.Token, later)
		_, err := NewKeys(ctx, "https://issuer.example")
		if err == nil || secondErr == nil {
			t.Fatalf("NewKeys answered %v and %v, want two refusals", err, secondErr)
		}
		if strings.Contains(secondErr.Error(), second) {
			t.Errorf("the second discovery's error prints its own bearer: %q", secondErr)
		}
		redacttest.RequireNone(t, "oidc.record/interleaved", err.Error())
	})
	t.Run("status-line-empty-body", func(t *testing.T) {
		echo := func(r *http.Request) string { return "Gateway Echo " + r.Header.Get("Authorization") }
		none := func(*http.Request) string { return "" }
		_, err := NewKeys(bearerContext(recordStub{http.StatusForbidden, echo, none}, redacttest.Token, nil), "https://issuer.example")
		if err == nil {
			t.Fatal("NewKeys answered no error")
		}
		redacttest.RequireNone(t, "oidc.record/status-line-empty-body", err.Error())
	})
}
