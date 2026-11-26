package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const gateToken = claim.Token("legion-legion-legion-9-tester")

// launchChecks is the store under the gate: it records each start's check and refuses as told.
type launchChecks struct {
	supervise.Store
	mu      sync.Mutex
	checked []uint64
	refuse  error
}

func (s *launchChecks) CheckLaunch(_ context.Context, c supervise.Claim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checked = append(s.checked, c.Generation)
	return s.refuse
}

// claimViews is the machine's View as the gate reads it, set by the test.
type claimViews struct {
	mu   sync.Mutex
	view supervise.ClaimView
}

func (v *claimViews) get(token claim.Token) (supervise.ClaimView, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.view, token == gateToken
}

func (v *claimViews) set(state supervise.ClaimState, generation uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.view.State, v.view.Generation = state, generation
}

func newTestGate(t *testing.T) (*answerGate, *launchChecks, *claimViews) {
	t.Helper()
	inner := &launchChecks{}
	views := &claimViews{view: supervise.ClaimView{Claim: supervise.Claim{Token: gateToken, Session: "ses_tester", State: supervise.StateSuspended, Generation: 3}}}
	gate := newAnswerGate(inner, views.get, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return gate, inner, views
}

// A claim started while an answer is being added to its conversation waits for the append to end
// before its check passes; from then on the gate refuses the loop the conversation, so the next
// answer appends nothing (it is forwarded), until the process that start began has ended.
func TestAStartDuringAnAppendWaitsForItAndThenHoldsTheConversation(t *testing.T) {
	t.Parallel()
	gate, inner, views := newTestGate(t)
	appending, release := make(chan struct{}), make(chan struct{})
	appended := make(chan struct{})
	go func() {
		ran, err := gate.Append(gateToken, "ses_tester", func() error {
			close(appending)
			<-release
			return nil
		})
		if !ran || err != nil {
			t.Errorf("the first append = %t, %v; want it written", ran, err)
		}
		close(appended)
	}()
	<-appending
	started := make(chan error, 1)
	go func() { started <- gate.CheckLaunch(context.Background(), views.view.Claim) }()
	select {
	case err := <-started:
		t.Fatalf("the start passed its check (%v) while the answer was being appended", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-appended
	if err := <-started; err != nil {
		t.Fatalf("the start's check after the append = %v", err)
	}
	if len(inner.checked) != 1 {
		t.Fatalf("store checks = %v; want the start checked once", inner.checked)
	}

	wrote := false
	ran, err := gate.Append(gateToken, "ses_tester", func() error { wrote = true; return nil })
	if ran || err != nil || wrote || !gate.Running(gateToken, "ses_tester") {
		t.Fatalf("an append after the start = %t, %v, wrote %t; want nothing appended and the claim running", ran, err, wrote)
	}

	views.set(supervise.StateLaunching, 4)
	if !gate.Running(gateToken, "ses_tester") {
		t.Fatal("the launched claim does not run its conversation")
	}
	views.set(supervise.StateSuspended, 4)
	if gate.Running(gateToken, "ses_tester") {
		t.Fatal("the claim still runs its conversation after the process the start began ended")
	}
	if ran, err := gate.Append(gateToken, "ses_tester", func() error { wrote = true; return nil }); !ran || err != nil || !wrote {
		t.Fatalf("an append once the claim stopped = %t, %v, wrote %t; want it written", ran, err, wrote)
	}
}

// releaseOnce is a channel a blocked write waits on and the func that closes it, safe to call more
// than once; the test's cleanup calls it too, so a failed run leaves no write blocked.
func releaseOnce(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return ch, release
}

// Two appends to one claim's conversation (the loop's worker and the hand-off consumer, or the
// workers of two sessions of one claim) take turns: the second writes only once the first has
// ended, and a start that came during the first passes its check only while neither writes. Once
// the first ends, either the second takes its turn and the start waits for it too, or the start
// passes first and the second, finding the claim started, writes nothing.
func TestTwoAppendsOnOneClaimTakeTurnsAndAStartWaitsForBoth(t *testing.T) {
	t.Parallel()
	gate, _, views := newTestGate(t)
	var writers atomic.Int32
	write := func(began chan<- struct{}, release <-chan struct{}) func() error {
		return func() error {
			if n := writers.Add(1); n > 1 {
				t.Errorf("%d appends write to the claim's conversation at once", n)
			}
			defer writers.Add(-1)
			close(began)
			<-release
			return nil
		}
	}
	type appended struct {
		ran bool
		err error
	}
	began1, began2 := make(chan struct{}), make(chan struct{})
	release1, end1 := releaseOnce(t)
	release2, end2 := releaseOnce(t)
	appended1, appended2 := make(chan appended, 1), make(chan appended, 1)
	go func() {
		ran, err := gate.Append(gateToken, "ses_tester", write(began1, release1))
		appended1 <- appended{ran, err}
	}()
	<-began1
	go func() {
		ran, err := gate.Append(gateToken, "ses_tester", write(began2, release2))
		appended2 <- appended{ran, err}
	}()
	select {
	case <-began2:
		t.Fatal("the second append began to write while the first still wrote")
	case <-time.After(100 * time.Millisecond):
	}

	// writing is the count of writes in progress as the start's check returned.
	type launched struct {
		err     error
		writing int32
	}
	started := make(chan launched, 1)
	go func() {
		err := gate.CheckLaunch(context.Background(), views.view.Claim)
		started <- launched{err, writers.Load()}
	}()
	select {
	case early := <-started:
		t.Fatalf("the start passed its check (%v) while the first append still wrote", early.err)
	case <-time.After(100 * time.Millisecond):
	}

	end1()
	if a := <-appended1; !a.ran || a.err != nil {
		t.Fatalf("the first append = %t, %v; want it written", a.ran, a.err)
	}
	var l launched
	select {
	case <-began2:
		select {
		case early := <-started:
			t.Fatalf("the start passed its check (%v) while the second append wrote", early.err)
		case <-time.After(100 * time.Millisecond):
		}
		end2()
		if a := <-appended2; !a.ran || a.err != nil {
			t.Fatalf("the second append = %t, %v; want it written after the first", a.ran, a.err)
		}
		l = <-started
	case l = <-started:
		if a := <-appended2; a.ran || a.err != nil {
			t.Fatalf("the second append after the start passed = %t, %v; want nothing written", a.ran, a.err)
		}
		select {
		case <-began2:
			t.Fatal("the second append wrote though the start passed its check before its turn")
		default:
		}
	}
	if l.err != nil {
		t.Fatalf("the start's check after the appends = %v", l.err)
	}
	if l.writing != 0 {
		t.Fatalf("the start's check passed while %d appends wrote", l.writing)
	}
}

// A start waits for an append at most the gate's bound, then starts anyway.
func TestAStartWaitsForAnAppendOnlyAsLongAsTheBound(t *testing.T) {
	t.Parallel()
	gate, _, views := newTestGate(t)
	gate.wait = 50 * time.Millisecond
	appending, release := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = gate.Append(gateToken, "ses_tester", func() error { close(appending); <-release; return nil })
	}()
	<-appending
	defer close(release)
	begun := time.Now()
	if err := gate.CheckLaunch(context.Background(), views.view.Claim); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(begun); waited < gate.wait {
		t.Fatalf("the start waited %s; want the bound %s", waited, gate.wait)
	}
	if !gate.Running(gateToken, "ses_tester") {
		t.Fatal("the start past the bound is not marked started")
	}
}

// A start the store underneath refuses starts nothing, so the conversation stays the loop's; a
// live claim runs its current conversation and not an older one a newer session replaced.
func TestARefusedStartAndAnOlderConversationLeaveTheLoopItsAppend(t *testing.T) {
	t.Parallel()
	gate, inner, views := newTestGate(t)
	inner.refuse = errors.New("the tree's cleanup is reserved")
	if err := gate.CheckLaunch(context.Background(), views.view.Claim); err == nil {
		t.Fatal("the refused start passed its check")
	}
	if gate.Running(gateToken, "ses_tester") {
		t.Fatal("a refused start marked the claim started")
	}
	views.set(supervise.StateWorking, 4)
	if !gate.Running(gateToken, "ses_tester") || gate.Running(gateToken, "ses_older") {
		t.Fatal("a working claim must run its current conversation and no older one")
	}
	if ran, err := gate.Append(gateToken, "ses_older", func() error { return nil }); !ran || err != nil {
		t.Fatalf("an append to an older conversation = %t, %v; want it written", ran, err)
	}
}

// The loop answers for every claim that runs nothing: one the daemon released (an idle release of
// its issue pod, or a pause, which suspends a claim the same way), one the workflow suspended (a
// parked child, a tree whose root left the workflow during its linger), one retired, and one
// closed (no machine holds it any more). The gate writes into each conversation, and says only of
// the released one that the claim is released (Released), whose agent the loop gives everything
// it does not answer through its wake. A start of the released claim that passes its check takes
// the conversation back: it runs again, and is released no more.
func TestTheGateHoldsAReleasedClaimsConversationAndNoOtherStoppedOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		state    supervise.ClaimState
		by       supervise.SuspendSource
		closed   bool
		released bool
	}{
		{name: "released", state: supervise.StateSuspended, by: supervise.SuspendedByReleased, released: true},
		{name: "suspended by the workflow", state: supervise.StateSuspended, by: supervise.SuspendedByDaemon},
		{name: "retired", state: supervise.StateRetired},
		{name: "closed", state: supervise.StateRetired, closed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			views := &claimViews{view: supervise.ClaimView{Claim: supervise.Claim{Token: gateToken, Session: "ses_tester", State: tc.state, SuspendedBy: tc.by, Generation: 3}}}
			gate := newAnswerGate(&launchChecks{}, func(token claim.Token) (supervise.ClaimView, bool) {
				view, ok := views.get(token)
				return view, ok && !tc.closed
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if gate.Running(gateToken, "ses_tester") || gate.Released(gateToken, "ses_tester") != tc.released || gate.Released(gateToken, "ses_older") {
				t.Fatalf("for the claim %s %s by %q: Running %t, Released %t, Released of an older session %t; want it not running and released %t, and no older session released",
					gateToken, tc.state, tc.by, gate.Running(gateToken, "ses_tester"), gate.Released(gateToken, "ses_tester"),
					gate.Released(gateToken, "ses_older"), tc.released)
			}
			wrote := false
			ran, err := gate.Append(gateToken, "ses_tester", func() error { wrote = true; return nil })
			if err != nil || !ran || !wrote {
				t.Fatalf("Append for the claim %s %s by %q = %t, %v, wrote %t; want it written", gateToken, tc.state, tc.by, ran, err, wrote)
			}
			if !tc.released {
				return
			}
			if err := gate.CheckLaunch(context.Background(), views.view.Claim); err != nil {
				t.Fatal(err)
			}
			if !gate.Running(gateToken, "ses_tester") || gate.Released(gateToken, "ses_tester") {
				t.Fatal("the released claim's start passed its check and the gate still says it is released and not running")
			}
			if ran, err := gate.Append(gateToken, "ses_tester", func() error { return nil }); ran || err != nil {
				t.Fatalf("an append after the released claim's start = %t, %v; want nothing written", ran, err)
			}
		})
	}
}

// The gate reads a claim's cyber-policy hold off the machine's view of it
// (supervise.Claim.HeldForCyberPolicy), which the answer loop calls no model for: a claim that
// failed on OpenAI's cyber-policy refusal, and one an operator retired after it failed so. A claim
// that failed for another provider error, and one the daemon released, are not held, and the loop
// answers for them.
func TestTheGateReadsACyberPolicyHoldOffTheClaimsView(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		cause supervise.FailureCause
		by    supervise.SuspendSource
		held  bool
	}{
		{name: "failed for cyber policy", state: supervise.StateFailed, cause: supervise.CauseCyberPolicy, held: true},
		{name: "retired after it failed for cyber policy", state: supervise.StateRetired, cause: supervise.CauseCyberPolicy, held: true},
		{name: "failed for another provider error", state: supervise.StateFailed, cause: supervise.CauseProviderError},
		{name: "released", state: supervise.StateSuspended, by: supervise.SuspendedByReleased},
	} {
		t.Run(tc.name, func(t *testing.T) {
			views := &claimViews{view: supervise.ClaimView{Claim: supervise.Claim{Token: gateToken, Session: "ses_tester", State: tc.state,
				FailureCause: tc.cause, SuspendedBy: tc.by, Generation: 3}}}
			gate := newAnswerGate(&launchChecks{}, views.get, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if got := gate.HeldForCyberPolicy(gateToken); got != tc.held {
				t.Fatalf("HeldForCyberPolicy of the claim %s (%q) = %t, want %t", tc.state, tc.cause, got, tc.held)
			}
		})
	}
}
