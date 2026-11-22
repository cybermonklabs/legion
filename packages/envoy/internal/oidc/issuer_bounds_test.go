package oidc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// armBoundEnds makes flow wait for its issuer until the bound end of the product's own bounds
// (testwait.BoundEndAt of the test's deadline, read when each read or call starts): of
// discoveryAttemptTimeout for each read of the discovery document, and of TokenTimeout for each
// call to the token or revocation endpoint. Those bounds are for an issuer that accepts a call and
// never answers. The test's own issuer answers in the machine's time, which on a loaded machine can
// be longer, and a test of how a call fails then reads the bound's timeout as the issuer's answer.
// Under -timeout 0, which sets no deadline, only the issuer's answer ends a read or a call.
func armBoundEnds(t *testing.T, flow *CodeFlow) {
	t.Helper()
	flow.http.Timeout = 0
	deadline, bounded := t.Deadline()
	if !bounded {
		flow.discovery.armAttempt = func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithCancel(ctx)
		}
		return
	}
	flow.discovery.armAttempt = func(ctx context.Context) (context.Context, context.CancelFunc) {
		return context.WithDeadline(ctx, testwait.BoundEndAt(deadline, time.Now(), discoveryAttemptTimeout))
	}
	base := flow.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	flow.http.Transport = endingAt{base: base, deadline: deadline, bound: TokenTimeout}
}

// endingAt is a transport whose every request ends at the bound end of bound at deadline
// (testwait.BoundEndAt, read when the request starts), where a client's Timeout is one span from
// each request's start whatever the test has left.
type endingAt struct {
	base     http.RoundTripper
	deadline time.Time
	bound    time.Duration
}

func (e endingAt) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithDeadline(request.Context(), testwait.BoundEndAt(e.deadline, time.Now(), e.bound))
	response, err := e.base.RoundTrip(request.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	response.Body = cancelOnClose{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

// cancelOnClose is a response body that releases its request's deadline when it is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// A read of the discovery document runs under discoveryAttemptTimeout from its start, or under the
// bound a test armed in its place (armAttempt). The read's own deadline is read against the clock
// on both sides of the call, so the row holds on any machine.
func TestADiscoveryReadRunsUnderItsAttemptBound(t *testing.T) {
	read := func(deadline *time.Time, bounded *bool) func(ctx context.Context) (int, error) {
		return func(ctx context.Context) (int, error) {
			*deadline, *bounded = ctx.Deadline()
			return 0, errors.New("the read is not answered here")
		}
	}

	var deadline time.Time
	var bounded bool
	product := newDiscovery("https://issuer.example", read(&deadline, &bounded))
	before := time.Now()
	if _, err := product.discover(context.Background()); !errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("discover: err = %v, want ErrIssuerUnavailable", err)
	}
	after := time.Now()
	if !bounded || deadline.Before(before.Add(discoveryAttemptTimeout)) || deadline.After(after.Add(discoveryAttemptTimeout)) {
		t.Fatalf("the product's read: deadline %v (bounded %t), want discoveryAttemptTimeout (%s) from the call, between %v and %v", deadline, bounded, discoveryAttemptTimeout, before.Add(discoveryAttemptTimeout), after.Add(discoveryAttemptTimeout))
	}

	armed := time.Now().Add(time.Hour)
	tests := newDiscovery("https://issuer.example", read(&deadline, &bounded))
	tests.armAttempt = func(ctx context.Context) (context.Context, context.CancelFunc) {
		return context.WithDeadline(ctx, armed)
	}
	if _, err := tests.discover(context.Background()); !errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("discover: err = %v, want ErrIssuerUnavailable", err)
	}
	if !bounded || !deadline.Equal(armed) {
		t.Fatalf("a read a test armed: deadline %v (bounded %t), want the armed %v", deadline, bounded, armed)
	}
}

// capturedDeadline is a transport that answers no request and keeps the deadline of the last one.
type capturedDeadline struct {
	deadline *time.Time
	bounded  *bool
}

func (c capturedDeadline) RoundTrip(request *http.Request) (*http.Response, error) {
	*c.deadline, *c.bounded = request.Context().Deadline()
	return nil, errors.New("the call is not answered here")
}

// A code flow waits for its issuer under the product's bounds, discoveryAttemptTimeout and
// TokenTimeout, until a test moves each to its bound end (armBoundEnds). The row reads the
// deadline the flow's own read of its discovery document (Discover) and a call through the flow's
// own client carry, and wants each at testwait.BoundEndAt of the test's deadline and its bound.
func TestACodeFlowWaitsForItsIssuerUnderTheProductsBoundsUntilATestMovesThem(t *testing.T) {
	flow := NewCodeFlow("https://issuer.example", flowClientID, flowClientSecret, "")
	if flow.discovery.armAttempt != nil {
		t.Fatal("a new flow's discovery read has a bound armed, want discoveryAttemptTimeout")
	}
	if got := flow.http.Timeout; got != TokenTimeout {
		t.Fatalf("a new flow's token call bound is %s, want TokenTimeout (%s)", got, TokenTimeout)
	}

	var readEnd, callEnd time.Time
	var readBounded, callBounded bool
	flow.discovery.read = func(ctx context.Context) (*codeFlowIssuer, error) {
		readEnd, readBounded = ctx.Deadline()
		return nil, errors.New("the read is not answered here")
	}
	flow.http.Transport = capturedDeadline{deadline: &callEnd, bounded: &callBounded}
	armBoundEnds(t, flow)
	if got := flow.http.Timeout; got != 0 {
		t.Fatalf("after armBoundEnds the token client's Timeout is %s, want none: the bound end bounds each call", got)
	}
	before := time.Now()
	if err := flow.Discover(context.Background()); !errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("Discover: err = %v, want ErrIssuerUnavailable", err)
	}
	if response, err := flow.http.Get("http://issuer.invalid/token"); err == nil {
		response.Body.Close()
		t.Fatal("a call through the captured transport answered")
	}
	after := time.Now()

	testDeadline, testBounded := t.Deadline()
	if !testBounded {
		if readBounded || callBounded {
			t.Fatalf("under -timeout 0: the discovery read ends at %v (bounded %t) and the token call at %v (bounded %t), want no end", readEnd, readBounded, callEnd, callBounded)
		}
		return
	}
	for _, carried := range []struct {
		what    string
		end     time.Time
		bounded bool
		bound   time.Duration
	}{
		{"the discovery read", readEnd, readBounded, discoveryAttemptTimeout},
		{"the token call", callEnd, callBounded, TokenTimeout},
	} {
		lo, hi := testwait.BoundEndAt(testDeadline, before, carried.bound), testwait.BoundEndAt(testDeadline, after, carried.bound)
		if !carried.bounded || carried.end.Before(lo) || carried.end.After(hi) {
			t.Errorf("%s ends at %v (bounded %t), want the bound end of %s at the test's deadline %v, between %v and %v", carried.what, carried.end, carried.bounded, carried.bound, testDeadline, lo, hi)
		}
	}
}

// A call through the transport armBoundEnds installs ends at the transport's end when its
// issuer never answers, and a call its issuer answers reads the whole answer.
func TestACallThroughTheTestsTransportEndsAtItsEnd(t *testing.T) {
	release := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer silent.Close()
	defer close(release)
	client := &http.Client{Transport: endingAt{base: http.DefaultTransport, deadline: time.Now().Add(100 * time.Millisecond), bound: TokenTimeout}}
	response, err := client.Get(silent.URL)
	if err == nil {
		response.Body.Close()
		t.Fatalf("a call to an issuer that never answers answered %s, want the transport's end", response.Status)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a call to an issuer that never answers: err = %v, want context.DeadlineExceeded", err)
	}

	answering := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "the whole answer")
	}))
	defer answering.Close()
	client = &http.Client{Transport: endingAt{base: http.DefaultTransport, deadline: time.Now().Add(time.Hour), bound: TokenTimeout}}
	response, err = client.Get(answering.URL)
	if err != nil {
		t.Fatalf("a call to an issuer that answers: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	if closeErr := response.Body.Close(); err != nil || closeErr != nil || string(body) != "the whole answer" {
		t.Fatalf("the answer read %q (read error %v, close error %v), want the whole answer", body, err, closeErr)
	}
}
