package admission

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// outsideWaiter is one Take of an Outside run on a goroutine of its own, whose answer arrives on
// answered.
type outsideWaiter struct {
	cancel   context.CancelFunc
	answered chan error
}

// takeInBackground starts a Take of user on outside, on a context the test can end.
func takeInBackground(outside *Outside, user OutsideUser) *outsideWaiter {
	ctx, cancel := context.WithCancel(context.Background())
	w := &outsideWaiter{cancel: cancel, answered: make(chan error, 1)}
	go func() { w.answered <- outside.Take(ctx, user) }()
	return w
}

// takeEveryPlace takes all OutsideLimit places of outside, failing the test if any is not free.
func takeEveryPlace(t *testing.T, outside *Outside) {
	t.Helper()
	for place := range OutsideLimit {
		if err := outside.Take(context.Background(), HeartbeatCheck); err != nil {
			t.Fatalf("place %d of %d: %v, want it taken at once", place+1, OutsideLimit, err)
		}
	}
}

// awaitQueued returns once outside's queue holds queued.
func awaitQueued(t *testing.T, outside *Outside, queued int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for outside.Stats().Queued != queued {
		if time.Now().After(deadline) {
			t.Fatalf("%d waiting for a place, want %d", outside.Stats().Queued, queued)
		}
		time.Sleep(time.Millisecond)
	}
}

// outsideRefusalReason is the Reason err refuses for, "" for an error that is no *OutsideRefusal.
func outsideRefusalReason(err error) Reason {
	var refusal *OutsideRefusal
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}

// The work outside admission holds at most the request pool's connections admission leaves, 8 of
// the 48, and waits for one of them when all are held: a ninth asks, waits, and takes the place
// one gives back. Its refusals are counted by who asked and why. A Hold's place is given back once
// however often it is released, taken again by Retake once it has been given back and by no Retake
// while it is held, and carried on a context to the call that gives it back (WithPlace); a
// Limiter's places are its own, and a nil Outside limits nothing.
func TestTheWorkOutsideAdmissionHoldsTheShareAdmissionLeaves(t *testing.T) {
	if OutsideLimit != store.SharedPoolSize-MaxAdmittedUnits {
		t.Fatalf("OutsideLimit = %d, want the request pool's %d less admission's %d", OutsideLimit, store.SharedPoolSize, MaxAdmittedUnits)
	}
	if OutsideLimit != 8 || OutsideQueueLimit != 176 {
		t.Fatalf("OutsideLimit = %d and OutsideQueueLimit = %d, want 8 and 176", OutsideLimit, OutsideQueueLimit)
	}
	outside := New(Limits{OutsideWait: time.Minute}).Outside()
	takeEveryPlace(t, outside)
	ninth := takeInBackground(outside, DPoPDecision)
	awaitQueued(t, outside, 1)
	if got := outside.Stats(); got.InUse != OutsideLimit || got.Queued != 1 || got.MaxQueued != 1 {
		t.Fatalf("stats %+v, want %d in use and the ninth waiting", got, OutsideLimit)
	}
	outside.Release()
	if err := <-ninth.answered; err != nil {
		t.Fatalf("the ninth, given the place one gave back: %v, want it taken", err)
	}
	if got := outside.Stats(); got.InUse != OutsideLimit || got.Queued != 0 {
		t.Fatalf("stats %+v, want the place handed over: %d in use and none waiting", got, OutsideLimit)
	}
	if other := New(DefaultLimits()).Outside(); other.Take(context.Background(), DPoPDecision) != nil {
		t.Fatal("another Limiter's first place refused, want each Limiter's places its own")
	}

	outside.Release()
	place, err := outside.Hold(context.Background(), SocketAdmission)
	if err != nil {
		t.Fatalf("a Hold with a place free: %v", err)
	}
	if carried := PlaceFrom(WithPlace(context.Background(), place)); carried != place {
		t.Fatalf("the place a context carries is %p, want the Hold's %p", carried, place)
	}
	if PlaceFrom(context.Background()) != nil {
		t.Fatal("a context that carries no place answered one")
	}
	place.Release()
	place.Release()
	if got := outside.Stats().InUse; got != OutsideLimit-1 {
		t.Fatalf("%d places in use after a Hold's place was released twice, want %d: one place given back once", got, OutsideLimit-1)
	}
	if err := place.Retake(context.Background()); err != nil {
		t.Fatalf("Retake of the place given back, with a place free: %v", err)
	}
	if err := place.Retake(context.Background()); err != nil {
		t.Fatalf("Retake of a place held: %v", err)
	}
	if got := outside.Stats().InUse; got != OutsideLimit {
		t.Fatalf("%d places in use after Retake twice, want %d: the place taken again once", got, OutsideLimit)
	}
	place.Release()

	var none *Outside
	if err := none.Take(context.Background(), DPoPDecision); err != nil {
		t.Fatalf("a nil Outside refused a place (%v), want it to limit nothing", err)
	}
	none.Release()
	unlimited, err := none.Hold(context.Background(), SocketAdmission)
	if err != nil {
		t.Fatalf("a nil Outside refused a Hold (%v), want it to limit nothing", err)
	}
	unlimited.Release()
	if err := unlimited.Retake(context.Background()); err != nil {
		t.Fatalf("a nil Outside refused a Retake (%v), want it to limit nothing", err)
	}
	var nothing *Place
	nothing.Release()
	if err := nothing.Retake(context.Background()); err != nil {
		t.Fatalf("a nil Place refused a Retake (%v), want it to hold nothing and take nothing", err)
	}
}

// Work waiting for a place is given one in the order it asked: twelve wait behind the eight that
// hold every place, each asking once the one before it is queued, and each place given back goes
// to the one that has waited longest, whatever its user. The queue's longest length is kept.
func TestWorkOutsideAdmissionIsGivenItsPlaceInTheOrderItAsked(t *testing.T) {
	const queued = 12
	outside := New(Limits{OutsideWait: time.Minute}).Outside()
	takeEveryPlace(t, outside)
	users := []OutsideUser{HeartbeatCheck, SocketAdmission, DPoPDecision}
	granted := make(chan int, queued)
	waiters := make([]*outsideWaiter, queued)
	for i := range queued {
		waiters[i] = takeInBackground(outside, users[i%len(users)])
		awaitQueued(t, outside, i+1)
		go func() {
			if err := <-waiters[i].answered; err == nil {
				granted <- i
			}
		}()
	}
	t.Cleanup(func() {
		for _, w := range waiters {
			w.cancel()
		}
	})
	if got := outside.Stats().MaxQueued; got != queued {
		t.Fatalf("the queue's longest length = %d, want %d", got, queued)
	}
	for want := range queued {
		outside.Release()
		select {
		case got := <-granted:
			if got != want {
				t.Fatalf("the place given back after %d went to waiter %d, want waiter %d: the one that has waited longest", want, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the place given back went to no waiter; %d still waiting", outside.Stats().Queued)
		}
	}
	if got := outside.Stats(); got.InUse != OutsideLimit || got.Queued != 0 || got.MaxQueued != queued {
		t.Fatalf("stats %+v, want every place held, none waiting and the longest queue %d", got, queued)
	}
}

// The queue for a place holds OutsideQueueLimit, ReadBurstFactor times the places, the cap that
// bounds its memory: with eight holding and 176 waiting, the next asks and is refused at once,
// counted as a full queue under its user, and work whose context ends while it waits leaves the
// queue, making room for the next.
func TestWorkOutsideAdmissionPastItsQueueIsRefusedAtOnce(t *testing.T) {
	outside := New(Limits{OutsideWait: time.Minute}).Outside()
	takeEveryPlace(t, outside)
	waiters := make([]*outsideWaiter, OutsideQueueLimit)
	for i := range waiters {
		waiters[i] = takeInBackground(outside, HeartbeatCheck)
		awaitQueued(t, outside, i+1)
	}
	t.Cleanup(func() {
		for _, w := range waiters {
			w.cancel()
		}
	})
	past := takeInBackground(outside, SocketAdmission)
	defer past.cancel()
	select {
	case err := <-past.answered:
		if outsideRefusalReason(err) != QueueFull {
			t.Fatalf("work past a full queue: %v, want refused for a full queue", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("work past a full queue of %d was still waiting after 2 s, want it refused at once", OutsideQueueLimit)
	}
	if got := outside.Stats().Refused[SocketAdmission]; got != (OutsideRefusals{QueueFull: 1}) {
		t.Fatalf("the socket admission's refusals %+v, want one for a full queue", got)
	}

	waiters[3].cancel()
	if err := <-waiters[3].answered; !errors.Is(err, context.Canceled) {
		t.Fatalf("a waiter whose context ended: %v, want its context's error", err)
	}
	awaitQueued(t, outside, OutsideQueueLimit-1)
	next := takeInBackground(outside, DPoPDecision)
	defer next.cancel()
	awaitQueued(t, outside, OutsideQueueLimit)
	if got := outside.Stats().Refused[HeartbeatCheck]; got != (OutsideRefusals{}) {
		t.Fatalf("the heartbeat checks' refusals %+v, want none: a waiter that left was not refused", got)
	}
}

// Work waits for a place for at most its wait (Limits.OutsideWait, a second on every server),
// counted from when it asked: work no place came free for in that time is refused, counted as a
// wait exceeded under its user, and leaves the queue; work a place comes free for within it takes
// that place.
func TestWorkOutsideAdmissionIsRefusedOnceItsWaitRunsOut(t *testing.T) {
	const (
		wait  = 300 * time.Millisecond
		slack = 250 * time.Millisecond
	)
	if limits := New(DefaultLimits()); limits.Outside().wait != DefaultReadWait {
		t.Fatalf("a server's wait for a place = %v, want DefaultReadWait, %v", limits.Outside().wait, DefaultReadWait)
	}
	outside := New(Limits{OutsideWait: wait}).Outside()
	takeEveryPlace(t, outside)
	asked := time.Now()
	waiting := takeInBackground(outside, HeartbeatCheck)
	defer waiting.cancel()
	select {
	case err := <-waiting.answered:
		waited := time.Since(asked)
		if outsideRefusalReason(err) != WaitExceeded {
			t.Fatalf("work no place came free for: %v, want refused for its wait", err)
		}
		if waited < wait || waited > wait+slack {
			t.Fatalf("work refused after %v, want after its %v wait and within %v of it", waited, wait, slack)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("work no place came free for was still waiting after 5 s, want it refused at its %v wait", wait)
	}
	if got := outside.Stats(); got.Queued != 0 || got.Refused[HeartbeatCheck] != (OutsideRefusals{WaitExceeded: 1}) {
		t.Fatalf("stats %+v, want none waiting and one heartbeat check refused for its wait", got)
	}

	served := takeInBackground(outside, SocketAdmission)
	defer served.cancel()
	awaitQueued(t, outside, 1)
	time.Sleep(wait / 2)
	outside.Release()
	if err := <-served.answered; err != nil {
		t.Fatalf("work a place came free for within its wait: %v, want it taken", err)
	}
}
