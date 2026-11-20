package events

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A caller that refused the observation in flight shares the one after it however late it runs
// again: one that runs again only once that next observation has ended takes its answer, which
// started after the call, rather than run one more. A caller that ran again only to find none in
// flight and start its own would make every caller run one on a machine loaded enough that each
// runs again after the one before it has finished. Scripted: the first call's observation is
// held; the second and third calls arrive while it is in flight, so both refuse it; the third is
// held where it parks until the second has its answer, which it has only once the observation
// after the held one has ended. Three calls, two observations: fewer means a caller settled for
// the observation in flight when it called, more means a caller did not share the next.
func TestHorizonACallerThatRunsAgainLateSharesTheObservationAfterTheOneItRefused(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	counting := &countingHorizonDB{HorizonDB: database.Pool, entered: make(chan struct{}), release: make(chan struct{})}
	horizon := NewHorizon(counting)
	parked := make(chan struct{}, 4)
	late := make(chan struct{})
	// A failure below must not leave the held observation or the held caller blocked for ever.
	release, runLate := sync.OnceFunc(func() { close(counting.release) }), sync.OnceFunc(func() { close(late) })
	defer release()
	defer runLate()
	var parks atomic.Int32
	horizon.afterObservationParked = func() {
		n := parks.Add(1)
		parked <- struct{}{}
		if n == 2 {
			<-late
		}
	}
	call := func() <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := horizon.Safe(ctx)
			done <- err
		}()
		return done
	}
	await := func(what string, ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatalf("%s: %v", what, ctx.Err())
		}
	}
	answer := func(which string, done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the %s Safe call: %v", which, err)
			}
		case <-ctx.Done():
			t.Fatalf("the %s Safe call has no answer: %v", which, ctx.Err())
		}
	}

	first := call()
	await("the first call's observation never reached the database", counting.entered)
	second := call()
	await("the second call never parked on the observation in flight", parked)
	third := call()
	await("the third call never parked on the observation in flight", parked)
	release()
	answer("first", first)
	answer("second", second)
	runLate()
	answer("third", third)
	if observations := counting.batches.Load(); observations != 2 {
		t.Fatalf("three Safe calls, two made while the first one's observation was in flight, ran %d observations, want 2: the first one's and the one after it, which the two share", observations)
	}
}

// The call boundary and its reservation of the observation after one in flight are one locked
// operation. A caller suspended after that operation cannot forget its reservation: after another
// caller has run it, the suspended caller takes that observation's answer instead of seeing no
// observation in flight and starting one more. Scripted: the first call holds observation 1; the
// second reserves observation 2 and stops at the test hook immediately after that reservation;
// the third also takes that reservation and runs observation 2; then the second resumes. Three
// calls run two observations. If the second stops between its boundary and the reservation, it
// runs observation 3 when it resumes, the race this test refuses.
func TestHorizonCallerSuspendedAfterTheBoundarySharesItsReservedObservation(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	counting := &countingHorizonDB{HorizonDB: database.Pool, entered: make(chan struct{}), release: make(chan struct{})}
	horizon := NewHorizon(counting)
	boundary := make(chan struct{}, 1)
	resume := make(chan struct{})
	// A failure below must not leave the held observation or the held caller blocked for ever.
	release, unpause := sync.OnceFunc(func() { close(counting.release) }), sync.OnceFunc(func() { close(resume) })
	defer release()
	defer unpause()
	var boundaries atomic.Int32
	horizon.afterObservationBoundary = func() {
		if boundaries.Add(1) == 2 {
			boundary <- struct{}{}
			<-resume
		}
	}
	call := func() <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := horizon.Safe(ctx)
			done <- err
		}()
		return done
	}
	await := func(what string, ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatalf("%s: %v", what, ctx.Err())
		}
	}
	answer := func(which string, done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the %s Safe call: %v", which, err)
			}
		case <-ctx.Done():
			t.Fatalf("the %s Safe call has no answer: %v", which, ctx.Err())
		}
	}

	first := call()
	await("the first call's observation never reached the database", counting.entered)
	second := call()
	await("the second call never reserved the observation after the one in flight", boundary)
	third := call()
	release()
	answer("first", first)
	answer("third", third)
	unpause()
	answer("second", second)
	if observations := counting.batches.Load(); observations != 2 {
		t.Fatalf("three Safe calls, one suspended after reserving the observation after the one in flight, ran %d observations, want 2", observations)
	}
}
