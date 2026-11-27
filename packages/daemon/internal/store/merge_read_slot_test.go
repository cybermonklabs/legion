package store

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The merge read's slot is one for the whole database. Two readers, each a store of its own on one
// database as two projects' daemons are, never hold it at once: a reader that finds it held is
// answered false at once, and while each sends its requests only under the slot, at most one
// request to the far side stands. A holder whose context is cancelled while its request stands
// gives the slot back as its connection closes, and another reader takes it only then.
func TestAMergeReadHasOneDatabaseWideSlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	first := emptyStore(t)
	second, err := Open(ctx, first.Pool().Config().ConnString())
	if err != nil {
		t.Fatalf("open the second reader's store: %v", err)
	}
	t.Cleanup(second.Close)
	readers := []*Pool{first.Pool(), second.Pool()}

	held, ok, err := readers[0].TakeMergeReadSlot(ctx)
	if err != nil || !ok {
		t.Fatalf("the first reader's take of the free slot = %t, %v; want it taken", ok, err)
	}
	if other, ok, err := readers[1].TakeMergeReadSlot(ctx); err != nil || ok {
		if ok {
			other.Release(ctx)
		}
		t.Fatalf("the second reader's take while the first holds the slot = %t, %v; want false at once", ok, err)
	}
	held.Release(ctx)

	var standing, most, served atomic.Int32
	far := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := standing.Add(1)
		for {
			seen := most.Load()
			if now <= seen || most.CompareAndSwap(seen, now) {
				break
			}
		}
		select {
		case <-time.After(2 * time.Millisecond):
		case <-r.Context().Done():
		}
		standing.Add(-1)
		served.Add(1)
	}))
	defer far.Close()
	const each = 15
	var readersDone sync.WaitGroup
	failures := make(chan error, len(readers)*each)
	for _, reader := range readers {
		readersDone.Add(1)
		go func() {
			defer readersDone.Done()
			for range each {
				slot, err := takeWhenFree(ctx, reader)
				if err != nil {
					failures <- err
					return
				}
				response, err := http.Get(far.URL)
				if err == nil {
					err = response.Body.Close()
				}
				slot.Release(ctx)
				if err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	readersDone.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("a reader's request: %v", err)
	}
	if most.Load() != 1 || served.Load() != 2*each {
		t.Fatalf("the far side served %d requests with at most %d standing at once, want %d with one standing at a time", served.Load(), most.Load(), 2*each)
	}

	cancelled, cancel := context.WithCancel(ctx)
	holder, ok, err := readers[0].TakeMergeReadSlot(cancelled)
	if err != nil || !ok {
		t.Fatalf("the first reader's take before its cancellation = %t, %v; want it taken", ok, err)
	}
	request, err := http.NewRequestWithContext(cancelled, http.MethodGet, far.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
		sent <- err
	}()
	cancel()
	if err := testwait.Receive(t, "the cancelled request to end", sent); !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled holder's request = %v, want it ended by its cancellation", err)
	}
	if other, ok, err := readers[1].TakeMergeReadSlot(ctx); err != nil || ok {
		if ok {
			other.Release(ctx)
		}
		t.Fatalf("the second reader's take before the cancelled holder gave the slot back = %t, %v; want false", ok, err)
	}
	holder.Release(cancelled)
	var after *MergeReadSlot
	testwait.Eventually(t, "the second reader to take the slot the cancelled holder's closed connection gave back", func() bool {
		slot, ok, err := readers[1].TakeMergeReadSlot(ctx)
		if err != nil {
			t.Fatalf("the second reader's take after the cancellation: %v", err)
		}
		after = slot
		return ok
	})
	after.Release(ctx)
}

// takeWhenFree takes reader's merge read slot, trying again each millisecond while another reader
// holds it, for at most ten seconds.
func takeWhenFree(ctx context.Context, reader *Pool) (*MergeReadSlot, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		slot, ok, err := reader.TakeMergeReadSlot(ctx)
		if err != nil || ok {
			return slot, err
		}
		select {
		case <-bounded.Done():
			return nil, errors.New("the slot stayed held for ten seconds")
		case <-time.After(time.Millisecond):
		}
	}
}
