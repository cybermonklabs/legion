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
		{"200 cut off mid-body", func(i *oidctest.Issuer) { i.CutToken(http.StatusOK) }, true},
		{"400 cut off mid-body", func(i *oidctest.Issuer) { i.CutToken(http.StatusBadRequest) }, true},
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

// A 200 whose body breaks off before it arrives whole is an issuer that could not answer, however
// it breaks off: the connection closed, the connection reset, or the issuer sending nothing more
// until the token request's own timeout ends the read. The caller is still there in every row, so
// none is the caller gone.
func TestCodeFlowExchangeReadsABodyThatBreaksOffAsAnOutage(t *testing.T) {
	for _, c := range []struct {
		name string
		cut  func(issuer *oidctest.Issuer)
	}{
		{"closed after 10 bytes", func(i *oidctest.Issuer) { i.CutToken(http.StatusOK) }},
		{"reset after 10 bytes", func(i *oidctest.Issuer) { i.ResetToken(http.StatusOK) }},
		{"stalled after 10 bytes past the request's timeout", func(i *oidctest.Issuer) { i.StallToken(http.StatusOK) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			issuer, flow := newCodeFlowForDomain(t, flowDomain)
			if err := flow.Discover(context.Background()); err != nil {
				t.Fatalf("Discover: %v", err)
			}
			c.cut(issuer)
			client := *flow.http
			client.Timeout = 300 * time.Millisecond
			flow.http = &client
			_, err := flow.Exchange(context.Background(), flowRedirect, "a-code", "a-nonce")
			if !errors.Is(err, ErrIssuerUnavailable) || errors.Is(err, context.Canceled) {
				t.Fatalf("Exchange against a 200 %s: err = %v, want ErrIssuerUnavailable and not context.Canceled", c.name, err)
			}
		})
	}
}

// A caller that leaves while the token endpoint's body is still arriving - the browser left the
// callback mid-answer - is the caller gone, not the issuer down: the exchange fails with the
// caller's context error, not ErrIssuerUnavailable, so the broker writes no issuer_unreachable and
// logs no ERROR for it. x/oauth2 reports the body read that failed without wrapping its error, so
// only the read failure the recorder keeps names the caller's context. The token request's own
// timeout is far longer than the caller's here, so only the caller ends the read.
func TestCodeFlowExchangeReadsACallerWhoLeavesMidBodyAsGone(t *testing.T) {
	for _, c := range []struct {
		name  string
		leave func() (context.Context, context.CancelFunc)
		want  error
	}{
		{"its deadline passes", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 300*time.Millisecond)
		}, context.DeadlineExceeded},
		{"it is cancelled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(300*time.Millisecond, cancel)
			return ctx, cancel
		}, context.Canceled},
	} {
		t.Run(c.name, func(t *testing.T) {
			issuer, flow := newCodeFlowForDomain(t, flowDomain)
			if err := flow.Discover(context.Background()); err != nil {
				t.Fatalf("Discover: %v", err)
			}
			issuer.StallToken(http.StatusOK)
			client := *flow.http
			client.Timeout = 30 * time.Second
			flow.http = &client
			ctx, cancel := c.leave()
			defer cancel()
			_, err := flow.Exchange(ctx, flowRedirect, "a-code", "a-nonce")
			if !errors.Is(err, c.want) || errors.Is(err, ErrIssuerUnavailable) {
				t.Fatalf("Exchange whose caller's context ends mid-body (%s): err = %v, want %v and not ErrIssuerUnavailable", c.name, err, c.want)
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
