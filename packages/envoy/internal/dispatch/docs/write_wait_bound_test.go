package docs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// callerRequestSince is a caller's request context that began at some point in the past, which is
// what the bound is measured from (WithCallerRequest, BoundWriteWait). A test uses it rather than
// waiting out WriteWaitBound in real time: the rule under test is the arithmetic - a request is
// refused once it has spent the bound waiting, wherever it spent it - and a request that began
// the bound ago has none of it left.
func callerRequestSince(parent context.Context, ago time.Duration) context.Context {
	return context.WithValue(parent, callerRequestKey{}, time.Now().Add(-ago))
}

// A caller's write that has spent the server's whole-write bound is refused with nothing of it
// applied, wherever it spent it: the pool has room here and the write waits for nothing, so what
// refuses it is the bound on the work. It is the other half of the wait bound's promise - the
// first answers a write the pod has no room for, this one a write the pod admitted and cannot
// finish, which is what left two writes of 320 answered by nothing while fourteen forks of a
// mebibyte shared the pod's two processors (dispatch://INFRA-179, WriteRequestBound).
func TestACallersWriteIsRefusedOnceItHasSpentTheServersWholeWriteBound(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	storeHistory(t, service, artifactID, grantTestState(t, 16<<10))
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	before := storedWriteOf(t, service, artifactID)

	request, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := cancelWrite(callerRequestSince(request, WriteRequestBound), service, artifactID, grantWriteHeading, "Written", nil)
	if err == nil {
		t.Fatal("the write committed after it had spent the server's whole-write bound")
	}
	if request.Err() != nil {
		t.Fatalf("the caller's request ended too (%v): the refusal must be the server's own", request.Err())
	}
	requireHeading(t, service, artifactID, true)
	if after := storedWriteOf(t, service, artifactID); after != before {
		t.Fatalf("the refused write left %+v, want the %+v it found", after, before)
	}
}

// A caller's write that cannot get a slot of the pod's load concurrency within the server's own
// bound is refused, by the server and not by its caller running out of time, and leaves the
// document exactly as it found it. This is the half of the contract the server had none of: a
// write with no bound of its own waited for the client to give up, and the client's retry then
// arrived beside a write still holding its place in the queue (dispatch://INFRA-179).
func TestACallersWriteIsRefusedOnceItHasSpentTheServersWaitBound(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	storeHistory(t, service, artifactID, grantTestState(t, 16<<10))
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	_, free := heldLoadSlots(t)
	defer free()
	before := storedWriteOf(t, service, artifactID)

	// The request's own context is alive throughout: nothing of the caller's ends this wait.
	request, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := cancelWrite(callerRequestSince(request, WriteWaitBound), service, artifactID, grantWriteHeading, "Written", nil)
	if err == nil {
		t.Fatal("the write committed while the pod's only load slot was held")
	}
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("the refusal is %v, want ErrServiceUnavailable, which the API answers 503", err)
	}
	if request.Err() != nil {
		t.Fatalf("the caller's request ended too (%v): the refusal must be the server's own", request.Err())
	}
	requireHeading(t, service, artifactID, true)
	if after := storedWriteOf(t, service, artifactID); after != before {
		t.Fatalf("the refused write left %+v, want the %+v it found", after, before)
	}

	// The slot is free again: the caller's retry applies the edit, exactly once.
	free()
	if err := cancelWrite(WithCallerRequest(request), service, artifactID, grantWriteHeading, "Written", nil); err != nil {
		t.Fatalf("the caller's retry: %v", err)
	}
	requireHeading(t, service, artifactID, false)
	once := storedWrite{updates: before.updates + 1, versions: before.versions + 1}
	if after := storedWriteOf(t, service, artifactID); after != once {
		t.Fatalf("the retry left %+v, want %+v: the edit applied exactly once", after, once)
	}
}

// A write of the server's own waits for its slot however long it takes, because nothing answers a
// 503 for it and nothing resends it: the settlement publish, the repair and the backfill each run
// on a context of the server's, and a continuation a handler detached from its request keeps the
// request's mark but loses its cancellation (context.WithoutCancel) and is the server's work from
// then on. Both are held here with the bound a caller's write would already have been refused at
// behind them.
func TestAWriteOfTheServersOwnIsNotRefusedByTheWaitBound(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	storeHistory(t, service, artifactID, grantTestState(t, 16<<10))
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}

	waitsThroughAHeldSlot := func(t *testing.T, writer context.Context, find, with string) {
		t.Helper()
		slots, free := heldLoadSlots(t)
		defer free()
		waiting := make(chan struct{})
		var waitOnce sync.Once
		slots.waiting = func() { waitOnce.Do(func() { close(waiting) }) }
		answered := make(chan error, 1)
		go func() { answered <- cancelWrite(writer, service, artifactID, find, with, nil) }()
		select {
		case <-waiting:
		case err := <-answered:
			t.Fatalf("the write finished (%v) while the only slot was held", err)
		case <-time.After(30 * time.Second):
			t.Fatal("the write did not wait for a slot in 30 s")
		}
		select {
		case err := <-answered:
			t.Fatalf("the write was answered (%v) while it waited for a slot: it has no caller to refuse", err)
		case <-time.After(2 * time.Second):
		}
		free()
		select {
		case err := <-answered:
			if err != nil {
				t.Fatalf("the write, once a slot was free: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the write did not finish in 30 s once a slot was free")
		}
	}

	t.Run("the server's own context", func(t *testing.T) {
		waitsThroughAHeldSlot(t, context.Background(), grantWriteHeading, "Written")
	})
	t.Run("a continuation detached from its request", func(t *testing.T) {
		detached := context.WithoutCancel(callerRequestSince(context.Background(), WriteWaitBound))
		waitsThroughAHeldSlot(t, detached, "Written", "Rewritten")
	})
}
