package oidc

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

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

// recordingArm is a WaitArm that arms a deadline an hour out and records each bound it was given
// with the deadline it armed.
type recordingArm struct {
	mu    sync.Mutex
	calls []armCall
}

type armCall struct {
	bound    time.Duration
	deadline time.Time
}

func (r *recordingArm) arm(ctx context.Context, bound time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(time.Hour)
	r.mu.Lock()
	r.calls = append(r.calls, armCall{bound: bound, deadline: deadline})
	r.mu.Unlock()
	return context.WithDeadline(ctx, deadline)
}

func (r *recordingArm) recorded() []armCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]armCall(nil), r.calls...)
}

// A code flow waits for its issuer under the product's bounds, discoveryAttemptTimeout and
// TokenTimeout, until it is given an arm (WithWaitArm). Given one, its own read of the discovery
// document (Discover) and a call through its own client each run within what the arm gives for
// that bound, and the client keeps no Timeout of its own.
func TestACodeFlowWaitsForItsIssuerUnderItsBoundsOrItsArm(t *testing.T) {
	product := NewCodeFlow("https://issuer.example", flowClientID, flowClientSecret, "")
	if product.discovery.armAttempt != nil {
		t.Fatal("a flow with no arm has its discovery read armed, want discoveryAttemptTimeout")
	}
	if got := product.http.Timeout; got != TokenTimeout {
		t.Fatalf("a flow with no arm bounds a token call by %s, want TokenTimeout (%s)", got, TokenTimeout)
	}

	var arms recordingArm
	flow := NewCodeFlow("https://issuer.example", flowClientID, flowClientSecret, "", WithWaitArm(arms.arm))
	if got := flow.http.Timeout; got != 0 {
		t.Fatalf("an armed flow's client has a Timeout of %s, want none: the arm bounds each call", got)
	}
	var readEnd time.Time
	var readBounded bool
	flow.discovery.read = func(ctx context.Context) (*codeFlowIssuer, error) {
		readEnd, readBounded = ctx.Deadline()
		return nil, errors.New("the read is not answered here")
	}
	if err := flow.Discover(context.Background()); !errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("Discover: err = %v, want ErrIssuerUnavailable", err)
	}
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer issuer.Close()
	response, err := flow.http.Get(issuer.URL + "/token")
	if err != nil {
		t.Fatalf("a call through the armed client: %v", err)
	}
	callEnd, callBounded := response.Request.Context().Deadline()
	response.Body.Close()

	calls := arms.recorded()
	if len(calls) != 2 || calls[0].bound != discoveryAttemptTimeout || calls[1].bound != TokenTimeout {
		t.Fatalf("the arm was given %+v, want discoveryAttemptTimeout for the read, then TokenTimeout for the call", calls)
	}
	if !readBounded || !readEnd.Equal(calls[0].deadline) {
		t.Errorf("the discovery read ends at %v (bounded %t), want the arm's %v", readEnd, readBounded, calls[0].deadline)
	}
	if !callBounded || !callEnd.Equal(calls[1].deadline) {
		t.Errorf("the token call ends at %v (bounded %t), want the arm's %v", callEnd, callBounded, calls[1].deadline)
	}
}

// A call through an armed flow's transport ends at what the arm gives when its issuer never
// answers, and a call its issuer answers reads the whole answer.
func TestACallThroughAnArmedTransportEndsAtTheArmsEnd(t *testing.T) {
	soon := func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, 100*time.Millisecond)
	}
	release := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer silent.Close()
	defer close(release)
	client := &http.Client{Transport: armedTransport{base: http.DefaultTransport, arm: soon, bound: TokenTimeout}}
	response, err := client.Get(silent.URL)
	if err == nil {
		response.Body.Close()
		t.Fatalf("a call to an issuer that never answers answered %s, want the arm's end", response.Status)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a call to an issuer that never answers: err = %v, want context.DeadlineExceeded", err)
	}

	later := func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, time.Hour)
	}
	answering := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "the whole answer")
	}))
	defer answering.Close()
	client = &http.Client{Transport: armedTransport{base: http.DefaultTransport, arm: later, bound: TokenTimeout}}
	response, err = client.Get(answering.URL)
	if err != nil {
		t.Fatalf("a call to an issuer that answers: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	if closeErr := response.Body.Close(); err != nil || closeErr != nil || string(body) != "the whole answer" {
		t.Fatalf("the answer read %q (read error %v, close error %v), want the whole answer", body, err, closeErr)
	}
}

// A lazy verifier reads its issuer's discovery document under discoveryAttemptTimeout until it is
// given an arm (WithWaitArm); given one, its own read (Discover) runs within what the arm gives for
// that bound.
func TestALazyVerifiersReadRunsUnderItsBoundOrItsArm(t *testing.T) {
	if NewLazyVerifier("https://issuer.example", "dispatch").discovery.armAttempt != nil {
		t.Fatal("a lazy verifier with no arm has its discovery read armed, want discoveryAttemptTimeout")
	}
	var arms recordingArm
	verifier := NewLazyVerifier("https://issuer.example", "dispatch", WithWaitArm(arms.arm))
	var readEnd time.Time
	var readBounded bool
	verifier.discovery.read = func(ctx context.Context) (*Verifier, error) {
		readEnd, readBounded = ctx.Deadline()
		return nil, errors.New("the read is not answered here")
	}
	if err := verifier.Discover(context.Background()); !errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("Discover: err = %v, want ErrIssuerUnavailable", err)
	}
	calls := arms.recorded()
	if len(calls) != 1 || calls[0].bound != discoveryAttemptTimeout || !readBounded || !readEnd.Equal(calls[0].deadline) {
		t.Fatalf("the arm was given %+v and the read ends at %v (bounded %t), want one call with discoveryAttemptTimeout and the read at its deadline", calls, readEnd, readBounded)
	}
}

// A lazy verifier given an arm (WithWaitArm) reads its key set within what the arm gives for
// Keys.FetchTimeout, and its own Verify proves it: the arm is given discoveryAttemptTimeout for the
// discovery read, then DefaultKeysFetchTimeout for the key-set read, and the verification passes.
// Keys no verifier armed bound a read by FetchTimeout from its start.
func TestALazyVerifiersKeyReadRunsUnderItsBoundOrItsArm(t *testing.T) {
	unarmed := &Keys{FetchTimeout: DefaultKeysFetchTimeout}
	before := time.Now()
	ctx, cancel := unarmed.fetchContext(context.Background())
	after := time.Now()
	deadline, bounded := ctx.Deadline()
	cancel()
	if !bounded || deadline.Before(before.Add(DefaultKeysFetchTimeout)) || deadline.After(after.Add(DefaultKeysFetchTimeout)) {
		t.Fatalf("keys no verifier armed: a read's deadline %v (bounded %t), want FetchTimeout (%s) from its start", deadline, bounded, DefaultKeysFetchTimeout)
	}

	issuer := oidctest.New(t)
	signing := issuer.PublishKey(t, "signing-key")
	var arms recordingArm
	verifier := NewLazyVerifier(issuer.URL(), testAudience, WithWaitArm(arms.arm))
	claims, err := verifier.Verify(context.Background(), issuer.Mint(t, signing, issuer.Claims(testSubject, testAudience)))
	if err != nil || claims.Subject != testSubject {
		t.Fatalf("Verify: %+v, %v; want %s", claims, err, testSubject)
	}
	calls := arms.recorded()
	if len(calls) != 2 || calls[0].bound != discoveryAttemptTimeout || calls[1].bound != DefaultKeysFetchTimeout {
		t.Fatalf("the arm was given %+v, want discoveryAttemptTimeout for the discovery read, then DefaultKeysFetchTimeout for the key-set read", calls)
	}

	// Keys a caller builds itself (NewKeys) take the same option.
	var keyArms recordingArm
	keys, err := NewKeys(context.Background(), issuer.URL(), WithWaitArm(keyArms.arm))
	if err != nil {
		t.Fatalf("NewKeys: %v", err)
	}
	ctx, cancel = keys.fetchContext(context.Background())
	deadline, bounded = ctx.Deadline()
	cancel()
	if calls := keyArms.recorded(); len(calls) != 1 || calls[0].bound != DefaultKeysFetchTimeout || !bounded || !deadline.Equal(calls[0].deadline) {
		t.Fatalf("keys NewKeys armed: the arm was given %+v and the read ends at %v (bounded %t), want one call with DefaultKeysFetchTimeout and the read at its deadline", calls, deadline, bounded)
	}
}

// A caller's wait on a lazy verifier's issuer that the caller bounds (WaitWithin, the listener's
// identityBudget) is that bound from now, or what the verifier's arm gives for it.
func TestALazyVerifiersCallerWaitsWithinItsBoundOrTheArm(t *testing.T) {
	const bound = 4 * time.Second
	before := time.Now()
	ctx, cancel := NewLazyVerifier("https://issuer.example", testAudience).WaitWithin(context.Background(), bound)
	after := time.Now()
	deadline, bounded := ctx.Deadline()
	cancel()
	if !bounded || deadline.Before(before.Add(bound)) || deadline.After(after.Add(bound)) {
		t.Fatalf("an unarmed verifier's wait ends at %v (bounded %t), want %s from the call", deadline, bounded, bound)
	}

	for name, armed := range map[string]func(WaitArm) *LazyVerifier{
		"a lazy verifier": func(arm WaitArm) *LazyVerifier {
			return NewLazyVerifier("https://issuer.example", testAudience, WithWaitArm(arm))
		},
		"a verifier discovered already": func(arm WaitArm) *LazyVerifier {
			return DiscoveredVerifier(NewWithKeys("https://issuer.example", testAudience, NewKeysAt("https://issuer.example/keys")), WithWaitArm(arm))
		},
	} {
		var arms recordingArm
		ctx, cancel = armed(arms.arm).WaitWithin(context.Background(), bound)
		deadline, bounded = ctx.Deadline()
		cancel()
		calls := arms.recorded()
		if len(calls) != 1 || calls[0].bound != bound || !bounded || !deadline.Equal(calls[0].deadline) {
			t.Fatalf("%s armed: the arm was given %+v and the wait ends at %v (bounded %t), want one call with %s and the wait at its deadline", name, calls, deadline, bounded, bound)
		}
	}
}

// A client check waits for its issuer under TokenTimeout until it is given an arm (WithWaitArm).
// Given one, its discovery read and its token request each run within what the arm gives for
// TokenTimeout, through a client that keeps no Timeout of its own.
func TestAClientCheckWaitsForItsIssuerUnderTokenTimeoutOrItsArm(t *testing.T) {
	if got := checkHTTPClient(nil).Timeout; got != TokenTimeout {
		t.Fatalf("a check with no arm bounds its requests by %s, want TokenTimeout (%s)", got, TokenTimeout)
	}
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), checkClientID, checkClientSecret)
	var arms recordingArm
	if got := checkHTTPClient([]Option{WithWaitArm(arms.arm)}).Timeout; got != 0 {
		t.Fatalf("an armed check's client has a Timeout of %s, want none: the arm bounds each request", got)
	}
	if check, err := CheckClient(context.Background(), issuer.URL(), checkClientID, checkClientSecret, WithWaitArm(arms.arm)); check != ClientCheckReady || err != nil {
		t.Fatalf("an armed CheckClient = (%q, %v), want ready", check, err)
	}
	calls := arms.recorded()
	if len(calls) != 2 || calls[0].bound != TokenTimeout || calls[1].bound != TokenTimeout {
		t.Fatalf("the arm was given %+v, want TokenTimeout twice: the discovery read, then the token request", calls)
	}
}
