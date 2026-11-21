package oidc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// A code exchange's answer decides by its status alone (oauthrefusal.Outage). A 5xx, a 408 or a 429,
// whatever error its body names, and no answer at all are an issuer that could not answer, and
// match ErrIssuerUnavailable in both modes, so a caller answers the person as it answers an issuer
// it cannot reach. Every other answer is the token endpoint answering, and fails the exchange as
// that answer: a 4xx with or without an OAuth error, a 2xx that names an error or holds no token,
// and a 3xx.
func TestCodeFlowExchangeReadsOnlyAnOutageAsTheIssuerUnavailable(t *testing.T) {
	for _, c := range []struct {
		name        string
		fail        func(issuer *oidctest.Issuer)
		unavailable bool
	}{
		{"400 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusBadRequest, "invalid_grant") }, false},
		{"401 invalid_client", func(i *oidctest.Issuer) { i.FailToken(http.StatusUnauthorized, "invalid_client") }, false},
		{"403 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusForbidden, "invalid_grant") }, false},
		{"403 unauthorized_client", func(i *oidctest.Issuer) { i.FailToken(http.StatusForbidden, "unauthorized_client") }, false},
		{"400 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusBadRequest, "") }, false},
		{"401 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusUnauthorized, "") }, false},
		{"403 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusForbidden, "") }, false},
		{"404 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusNotFound, "") }, false},
		{"200 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusOK, "invalid_grant") }, false},
		{"200 holding no token", func(i *oidctest.Issuer) { i.FailToken(http.StatusOK, "") }, false},
		{"302 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusFound, "") }, false},
		{"408 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusRequestTimeout, "invalid_grant") }, true},
		{"503 naming no OAuth error", func(i *oidctest.Issuer) { i.FailToken(http.StatusServiceUnavailable, "") }, true},
		{"503 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusServiceUnavailable, "invalid_grant") }, true},
		{"500 invalid_client", func(i *oidctest.Issuer) { i.FailToken(http.StatusInternalServerError, "invalid_client") }, true},
		{"429 invalid_grant", func(i *oidctest.Issuer) { i.FailToken(http.StatusTooManyRequests, "invalid_grant") }, true},
		{"no answer", func(i *oidctest.Issuer) { i.DropToken() }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			issuer, flow := newCodeFlowForDomain(t, flowDomain)
			c.fail(issuer)
			_, err := flow.Exchange(context.Background(), flowRedirect, "a-code", "a-nonce")
			if err == nil || errors.Is(err, ErrIssuerUnavailable) != c.unavailable {
				t.Fatalf("Exchange against a token endpoint answering %s: err = %v, want an error matching ErrIssuerUnavailable: %t", c.name, err, c.unavailable)
			}
			_, err = flow.OnlineExchange(context.Background(), flowRedirect, "a-code", onlineSignIn("a-nonce"))
			if err == nil || errors.Is(err, ErrIssuerUnavailable) != c.unavailable {
				t.Fatalf("OnlineExchange against a token endpoint answering %s: err = %v, want an error matching ErrIssuerUnavailable: %t", c.name, err, c.unavailable)
			}
		})
	}
}

// A code exchange whose own context ends before the token endpoint answers - the browser left the
// callback - fails with that context's error, not ErrIssuerUnavailable: the issuer was not shown
// unable to answer, so a caller writes no outage for it. The flow has discovered its issuer, so the
// context ends only the token request.
func TestCodeFlowExchangeTheCallerLeftIsNoIssuerOutage(t *testing.T) {
	issuer, flow := newCodeFlowForDomain(t, flowDomain)
	if err := flow.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := flow.Exchange(ctx, flowRedirect, "a-code", "a-nonce")
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("Exchange whose context ended: err = %v, want context.Canceled and not ErrIssuerUnavailable", err)
	}
	_, err = flow.OnlineExchange(ctx, flowRedirect, "a-code", onlineSignIn("a-nonce"))
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("OnlineExchange whose context ended: err = %v, want context.Canceled and not ErrIssuerUnavailable", err)
	}
	if got := issuer.TokenRequests(); len(got) != 0 {
		t.Fatalf("the token endpoint received %q, want nothing: the context had ended", got)
	}
}

// leaveAfterAnswer is a transport that ends its caller's context once the token endpoint's answer,
// or the failure to get one, is in hand: a browser that leaves the callback just as the issuer
// answers. It reads the answer's body first, so the exchange still reads the answer whole.
type leaveAfterAnswer struct {
	base  http.RoundTripper
	leave context.CancelFunc
}

func (l leaveAfterAnswer) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := l.base.RoundTrip(request)
	if response != nil {
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			l.leave()
			return nil, readErr
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
	}
	l.leave()
	return response, err
}

// A code exchange reads the caller as gone only when its failure's cause is the caller's own
// context ending before any answer came. An issuer that answered an outage, or failed with no
// answer, just as the caller left is still an issuer that could not answer, and so is one that
// outlasted the token request's own timeout, whose error also matches context.DeadlineExceeded
// though the caller is still there.
func TestCodeFlowExchangeReadsTheCallerGoneOnlyFromItsOwnContext(t *testing.T) {
	for _, c := range []struct {
		name string
		fail func(issuer *oidctest.Issuer)
		// leave ends the caller's context once the answer is in hand; otherwise the flow's token
		// request times out first.
		leave bool
	}{
		{"a 503 answered as the caller leaves", func(i *oidctest.Issuer) { i.FailToken(http.StatusServiceUnavailable, "invalid_grant") }, true},
		{"no answer as the caller leaves", func(i *oidctest.Issuer) { i.DropToken() }, true},
		{"an answer past the token request's timeout", func(i *oidctest.Issuer) { i.DelayToken(5 * time.Second) }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			issuer, flow := newCodeFlowForDomain(t, flowDomain)
			if err := flow.Discover(context.Background()); err != nil {
				t.Fatalf("Discover: %v", err)
			}
			c.fail(issuer)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := *flow.http
			if c.leave {
				client.Transport = leaveAfterAnswer{base: http.DefaultTransport, leave: cancel}
			} else {
				client.Timeout = 200 * time.Millisecond
			}
			flow.http = &client
			_, err := flow.Exchange(ctx, flowRedirect, "a-code", "a-nonce")
			if !errors.Is(err, ErrIssuerUnavailable) || errors.Is(err, context.Canceled) {
				t.Fatalf("Exchange against %s: err = %v, want ErrIssuerUnavailable and not context.Canceled", c.name, err)
			}
		})
	}
}
