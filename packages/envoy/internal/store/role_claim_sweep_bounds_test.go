// The deregistration's fallback (releaseAllRoleClaimsByScan) is one scan of the whole role
// bucket, and the request waiting for it has a budget. Two mechanisms hold that scan to the
// budget, and the two are independent:
//
//   - the caller's bound. The listing runs under bus.Bounded, which returns at the request's end
//     while the listing's goroutine runs on, so the fallback answers its caller rather than the
//     bucket.
//   - the scan's own stop. roleClaimsUntil's visit function returns ctx.Err() before it records
//     a claim, which ends the scan (kvwatch.ScanExistingKeys) and has it stop and drain its
//     watcher, so a listing the request walked away from costs the bucket nothing more.
//
// Each is invisible to the other's test, which is why there are two cases here rather than one. A
// scan that is delivered nothing never runs its visit function, so its own stop cannot fire and
// only the caller's bound can answer that deregistration; a scan that is being delivered entries
// reaches its visit function, and its caller's bound answers while the listing's goroutine runs
// on behind the answer, so only the scan's own stop ends the reading of the bucket. Before these
// two cases nothing in the tree held either bound: the review that asked for them deleted the
// ctx.Err() check and ran the package green, then called the listing with no bus.Bounded and ran
// it green again.
//
// Each request's deadline is the case's input, so it is made on a fake clock of the case's own
// (clocktest.Fake.WithTimeout), which the case moves past it at the moment the case needs: once
// the listing waits on its watch, or once it has been handed the entries the case means it to read
// before its request ends. The verdict reads the deadline's error, as the route's own does.

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testwait"
)

const (
	// stalledListingDeadline is the request's budget in the stalled-listing case, on the case's
	// fake clock, which the case moves past once the listing has made its watch. It stands in for
	// the route's own.
	stalledListingDeadline = 200 * time.Millisecond

	// walkedAwayClaims is how many claims the bucket holds in the walked-away case. The fake watch
	// hands the listing an entry only once the case has let it (pacedWatchKV), so a scan that read
	// the bucket to its end is told apart from one that stopped by a count rather than by a clock.
	walkedAwayClaims = 200
	// walkedAwayHanded is how many entries the case lets the listing be handed before its request
	// ends: a few claims, and far short of the bucket.
	walkedAwayHanded = 15
	// walkedAwayDeadline is the request the deregistration is given, which the case moves past
	// once the listing has been handed walkedAwayHanded entries.
	walkedAwayDeadline = 150 * time.Millisecond
	// walkedAwayCeiling is the most entries a stopped listing may have been handed. The request
	// above ends after walkedAwayHanded of them, so this leaves an order of magnitude, and it is
	// half the bucket, so a listing that read the bucket to its end cannot pass under it.
	walkedAwayCeiling = walkedAwayClaims / 2
	// walkedAwayDrift is how many entries the fake may forward after the scan has stopped: the
	// one that can already be in flight on its way to the drain when the stop lands, and one
	// more for the reader of these numbers.
	walkedAwayDrift = 2
)

// watchRoleBucketThrough installs watch as the watch side of reg's role bucket handle, leaving
// the legacy side as it is, and puts the previous handle back when the test ends. It is
// countRoleBucketCalls' sibling for the other side of that handle: a listing of the bucket's keys
// is a watch of it (kvwatch.ScanExistingKeys), not a Get, so a case about what a listing does
// fakes this side and reads no counter on the other. The swap is made under the mutex that guards
// the handle (Registry.kvMu), which every reader of it takes (Registry.roles), for the reason
// swapRoleBucketHandle's is.
func watchRoleBucketThrough(t *testing.T, reg *Registry, watch jetstream.KeyValue) {
	t.Helper()
	reg.kvMu.Lock()
	previous := reg.roleKV
	reg.roleKV = bus.NewKeyValue(previous.Legacy(), watch, previous.Streams())
	reg.kvMu.Unlock()
	t.Cleanup(func() {
		reg.kvMu.Lock()
		defer reg.kvMu.Unlock()
		reg.roleKV = previous
	})
}

// watchMadeKV hands out the watches of the handle it wraps, closing made once the first one has
// been made: the moment a scan of the bucket has its watcher and waits on what it delivers.
type watchMadeKV struct {
	jetstream.KeyValue

	made chan struct{}
	once *sync.Once
}

func (k watchMadeKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(ctx, keys, opts...)
	if err == nil {
		k.once.Do(func() { close(k.made) })
	}
	return watcher, err
}

// A deregistration whose role claim view cannot be served falls back to a scan of the whole role
// bucket, and that scan has an idle bound of its own measured in seconds (kvwatch.ScanIdleTimeout,
// 10 s). The request waiting for it has a budget measured in hundreds of milliseconds, and the
// fallback must answer within the request's: an HTTP handler that waits out the scan's instead is
// the 503 this issue was filed on, and the answer the caller is still owed is the one the route
// reports.
//
// The watch here delivers no entry at all, which is what makes this case about bus.Bounded and
// nothing else: the scan sits inside drainScan with an empty channel, so roleClaimsUntil's visit
// function never runs and the scan's own stop cannot fire. The only thing in the tree that can
// answer this deregistration before the scan's 10 s idle bound is the bound its caller holds. Run
// the listing as r.roleClaimsUntil(ctx) with no bus.Bounded around it and Remove answers after
// that 10 s, with the scan's timeout rather than the request's deadline, and only once the scan
// has ended, which stops its watch. So the case moves its fake clock past the request's deadline
// once the listing has made its watch, and the answer must come with that watch still running.
func TestTheDeregistrationFallbackAnswersItsRequestWhileItsListingIsStillStalled(t *testing.T) {
	t.Parallel()
	const session = "ses_stalled_listing"
	held := []string{"stalled-held-a", "stalled-held-b"}
	reg, _, _ := smallRoleBucketRegistry(t, session, held)
	// after 0, ended false: the watcher forwards nothing and never ends, the stalled scan
	// earlyEndKV documents. made closes once the listing has made its watch, and stopped once
	// its scan, ending, stops it (stopSignalledKV).
	made, stopped := make(chan struct{}), make(chan struct{})
	watchRoleBucketThrough(t, reg, stopSignalledKV{
		KeyValue: watchMadeKV{KeyValue: earlyEndKV{KeyValue: reg.roles().WatchHandle()}, made: made, once: &sync.Once{}},
		stopped:  stopped,
		once:     &sync.Once{},
	})

	clock := clocktest.New(registryClockStart)
	ctx, cancel := clock.WithTimeout(testwait.Context(t), stalledListingDeadline)
	defer cancel()
	answered := make(chan error, 1)
	go func() {
		_, err := reg.Remove(ctx, session, nil, releasedByItsProcess)
		answered <- err
	}()
	testwait.Receive(t, "the deregistration's listing to make its watch of the bucket", made)
	clock.Advance(stalledListingDeadline)
	err := testwait.Receive(t, "the deregistration to answer once its request's deadline passed", answered)
	// The listing's watch is stopped only as its scan ends, so a watch still running when the
	// answer came is a listing the answer did not wait for.
	select {
	case <-stopped:
		t.Fatalf("the deregistration answered %v only once its listing's scan had ended and stopped its watch, want it answered while the listing still waits: the scan delivers nothing until its own %s idle bound, so an answer that late is the fallback waiting for the listing rather than for its request",
			err, kvwatch.ScanIdleTimeout)
	default:
	}
	t.Logf("the deregistration answered %v, with the stalled scan's own idle bound at %s", err, kvwatch.ScanIdleTimeout)

	// The request's own deadline, not the scan's timeout: a fallback that waited for the listing
	// would answer kvwatch.ErrScanTimedOut instead, and ten seconds later.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Remove(%s) over a stalled listing answered %v, want the request's own deadline", session, err)
	}
	// The wording is what the route reports to the caller, so it names what was being done and
	// for which session; a bare context error says neither.
	want := fmt.Sprintf("list the role bucket's keys to release %s's claims:", session)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Remove(%s) answered %q, want it to say %q and carry the deadline", session, err, want)
	}
	// The deregistration failed, so nothing of it landed: the interest record is still there for
	// the caller's retry, as it is after any other failed sweep.
	if _, err := reg.Get(context.Background(), session); err != nil {
		t.Fatalf("read %s's interest record after the refused deregistration: %v, want the record left for the retry", session, err)
	}
}

// pacedWatchKV hands out a watcher over the real bucket that forwards an entry only for each one
// the case has let it forward (hand), and counts what it has forwarded, so a case reads how much
// of the bucket a listing was handed and decides when it is handed more. It is earlyEndKV's
// opposite number: that one stalls a scan, so the scan's visit function never runs, and this one
// feeds a scan, so its visit function runs on every entry it is handed and a scan that stops has
// to stop itself.
//
// The forwarding stops the moment the watcher is stopped, which is what a scan that ends does
// (kvwatch.drainScan's done: Stop, then a drain of what is left), and the stop is read before each
// step of an entry's forwarding, so only the entry already on its way when the stop lands can be
// forwarded after it. Entries nats.go has already buffered are then read off the real watcher
// without forwarding them - leaving them unread would park nats.go's delivery goroutine for good
// on a full buffer - and ended closes once the forwarding has returned, after which the count
// stands still: a count that climbed until then is a listing that went on reading the bucket.
type pacedWatchKV struct {
	jetstream.KeyValue

	// handed holds one token for each entry the case has let the watch forward.
	handed chan struct{}
	ended  chan struct{}
	ending sync.Once

	mu        sync.Mutex
	forwarded int
}

// newPacedWatchKV is a pacedWatchKV over kv that the case may let forward the whole walked-away
// bucket, its end marker included.
func newPacedWatchKV(kv jetstream.KeyValue) *pacedWatchKV {
	return &pacedWatchKV{KeyValue: kv, handed: make(chan struct{}, walkedAwayClaims+1), ended: make(chan struct{})}
}

// hand lets the watch forward n more entries.
func (k *pacedWatchKV) hand(n int) {
	for range n {
		k.handed <- struct{}{}
	}
}

func (k *pacedWatchKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(ctx, keys, opts...)
	if err != nil {
		return nil, err
	}
	paced := &pacedWatcher{KeyWatcher: watcher, updates: make(chan jetstream.KeyValueEntry), stopped: make(chan struct{})}
	go func() {
		defer k.ending.Do(func() { close(k.ended) })
		defer close(paced.updates)
		// What the real watcher still holds once the forwarding returns is read off it unforwarded.
		defer bus.DrainWatcher(watcher)
		for {
			var entry jetstream.KeyValueEntry
			select {
			case got, open := <-watcher.Updates():
				if !open {
					return
				}
				entry = got
			case <-paced.stopped:
				return
			}
			if paced.halted() {
				return
			}
			select {
			case <-k.handed:
			case <-paced.stopped:
				return
			}
			if paced.halted() {
				return
			}
			select {
			case paced.updates <- entry:
				k.mu.Lock()
				k.forwarded++
				k.mu.Unlock()
			case <-paced.stopped:
				return
			}
		}
	}()
	return paced, nil
}

// count is how many entries the fake has handed its caller.
func (k *pacedWatchKV) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.forwarded
}

// awaitHanded waits through the shared wait (testwait.Until) until the fake has handed its caller
// n entries.
func (k *pacedWatchKV) awaitHanded(t *testing.T, n int) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the listing to be handed %d entries", n), func() (string, bool) {
		handed := k.count()
		return fmt.Sprintf("handed %d", handed), handed == n
	})
}

type pacedWatcher struct {
	jetstream.KeyWatcher

	updates chan jetstream.KeyValueEntry
	stop    sync.Once
	stopped chan struct{}
}

func (w *pacedWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }

func (w *pacedWatcher) Stop() error {
	w.stop.Do(func() { close(w.stopped) })
	return w.KeyWatcher.Stop()
}

// halted reports whether the watcher has been stopped.
func (w *pacedWatcher) halted() bool {
	select {
	case <-w.stopped:
		return true
	default:
		return false
	}
}

// A listing whose request has ended must stop reading the bucket, not finish the scan for a
// caller that is gone. The scan has a context of its own (kvwatch.ScanExistingKeys builds one),
// so nothing outside it ends it: roleClaimsUntil's visit function returns ctx.Err() at the first
// key it is handed once the request is over, which ends the scan and has it stop and drain its
// watcher. Left running, it holds an ordered consumer and appends to a map nobody reads until
// the bucket's last key arrives or its 10 s idle bound elapses - against a bucket already too far
// behind to serve the claim view, which is the only reason this listing runs at all - while the
// client's retry starts a second scan of the whole thing.
//
// The watch here hands the scan the bucket one claim at a time, each only once the case has let
// it (pacedWatchKV): walkedAwayHanded claims while the request runs, and, once the case has moved
// the request past its deadline on the case's fake clock, the rest of the bucket. So the scan's
// visit function runs on every entry it is handed, and a scan that stops itself is handed at
// most two more than walkedAwayHanded, the entry it refuses and the one already on its way to the
// drain when it stops, where one that does not is handed the whole bucket.
//
// The two subtests hold the two halves of that, and each is driven the only way that can hold
// its half. Called directly, roleClaimsUntil answers its own caller, so the answer is the half a
// direct call holds - the request's error and no claims - and the listing is over by the time
// that call returns: nothing is left reading the bucket, so there is nothing after the answer to
// check. Through a deregistration, which is the production shape, the listing runs inside
// bus.Bounded, which answers its caller at the request's end and leaves the listing's goroutine
// running behind that answer: there the answer says nothing about the bucket, and what the fake
// is handed after it is the whole question the scan's own stop exists for. That is read once the
// fake's forwarding has ended, which it does only as the listing's scan ends: stopped by the
// scan's own stop, or at the bucket's end for a listing that read it all.
//
// Delete the ctx.Err() check in roleClaimsUntil and each subtest fails on its own half. The
// direct call reads the bucket to its end and answers all walkedAwayClaims claims and no error,
// so its error, its claims and its ceiling all fail. The deregistration still answers at its
// deadline, because bus.Bounded answers it, and its listing goes on being handed the bucket
// behind that answer, so what fails there is the check after the answer.
func TestARoleListingTheRequestWalkedAwayFromStopsReadingTheBucket(t *testing.T) {
	t.Parallel()
	const session = "ses_walked_away"
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg, rawInterests := coldRegistry(t, conn)
	roles, holderOf := otherSessionsClaims("walked-away", walkedAwayClaims)
	putRoleClaimsInParallel(t, reg.roles().Legacy(), roles, holderOf, reg.now)
	putInterest(t, rawInterests, Interest{SessionID: session, MachineID: "m1", Topics: []string{"notifications.chat"}}, reg.now)
	// Through the bucket's own watch the listing is complete, which is what makes the counts
	// below a reading of how much of the bucket the stopped listing was handed rather than of
	// how much of it there is.
	seeded, err := reg.roleClaimsUntil(context.Background())
	if err != nil {
		t.Fatalf("list the role bucket's keys before the case's own watch is installed: %v", err)
	}
	if len(seeded) != walkedAwayClaims {
		t.Fatalf("the fixture's bucket holds %d claims, want %d", len(seeded), walkedAwayClaims)
	}
	if err := reg.RoleViewErr(); err == nil {
		t.Fatal("the registry's role claim view can be served, so the deregistration below would never reach the listing")
	}

	// Sequential: both subtests list the parent's registry's role bucket, each through a watch of
	// its own (watchRoleBucketThrough), which the other's would replace.
	t.Run("the listing answers its caller the request's error and no claims", func(t *testing.T) {
		watch := newPacedWatchKV(reg.roles().WatchHandle())
		watchRoleBucketThrough(t, reg, watch)

		clock := clocktest.New(registryClockStart)
		ctx, cancel := clock.WithTimeout(testwait.Context(t), walkedAwayDeadline)
		defer cancel()
		type listing struct {
			claims []scannedRoleClaim
			err    error
		}
		listed := make(chan listing, 1)
		go func() {
			claims, err := reg.roleClaimsUntil(ctx)
			listed <- listing{claims: claims, err: err}
		}()
		watch.hand(walkedAwayHanded)
		watch.awaitHanded(t, walkedAwayHanded)
		clock.Advance(walkedAwayDeadline)
		// The rest of the bucket, its end marker included: only a listing that reads past its
		// request is handed it.
		watch.hand(walkedAwayClaims + 1 - walkedAwayHanded)
		got := testwait.Receive(t, "the listing whose request ended to answer", listed)
		handed := watch.count()
		t.Logf("the listing answered %v, having been handed %d of the bucket's %d claims", got.err, handed, walkedAwayClaims)

		// A listing short of the bucket is not a reading of it, so it is an error and no
		// claims, and the error is the request's: a caller cannot tell a partial list from a
		// complete one.
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("a listing whose request ended answered %d claims and %v, want no claims and the request's deadline", len(got.claims), got.err)
		}
		if got.claims != nil {
			t.Fatalf("the refused listing returned %d claims, want none", len(got.claims))
		}
		if handed > walkedAwayCeiling {
			t.Fatalf("the stopped listing had been handed %d of the bucket's %d claims by the time it answered, want no more than %d: a listing that was handed most of the bucket after a request that ended %d claims in is one that read past its request",
				handed, walkedAwayClaims, walkedAwayCeiling, walkedAwayHanded)
		}
	})

	// Sequential: the parent's registry and bucket, as above.
	t.Run("the deregistration's listing stops reading the bucket behind its answer", func(t *testing.T) {
		watch := newPacedWatchKV(reg.roles().WatchHandle())
		watchRoleBucketThrough(t, reg, watch)

		clock := clocktest.New(registryClockStart)
		ctx, cancel := clock.WithTimeout(testwait.Context(t), walkedAwayDeadline)
		defer cancel()
		removed := make(chan error, 1)
		go func() {
			_, err := reg.Remove(ctx, session, nil, releasedByItsProcess)
			removed <- err
		}()
		watch.hand(walkedAwayHanded)
		watch.awaitHanded(t, walkedAwayHanded)
		clock.Advance(walkedAwayDeadline)
		err := testwait.Receive(t, "the deregistration to answer once its request's deadline passed", removed)
		answered := watch.count()
		t.Logf("the deregistration answered %v, its listing having been handed %d of the bucket's %d claims", err, answered, walkedAwayClaims)

		// The fallback answers its request rather than the bucket, in the wording the route
		// reports. That bound is the stalled-listing case's subject, and it is held here only
		// so far as the rest of this case needs it: a deregistration that answered something
		// else never reached the listing this one is about.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Remove(%s) over a listing short of the bucket answered %v, want the request's own deadline", session, err)
		}
		want := fmt.Sprintf("list the role bucket's keys to release %s's claims:", session)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Remove(%s) answered %q, want it to say %q and carry the deadline", session, err, want)
		}

		// The check the answer itself cannot make, and the reason this half runs through the
		// deregistration: the listing's goroutine outlives the answer, so it is let have the
		// rest of the bucket, and the bucket is read again once the fake's forwarding has ended,
		// which it does only as the listing's scan ends. A listing still reading it would by
		// then have been handed every claim.
		watch.hand(walkedAwayClaims + 1 - walkedAwayHanded)
		testwait.Receive(t, "the listing's watch to end, stopped by its scan or at the bucket's end", watch.ended)
		after := watch.count()
		t.Logf("once the listing's watch had ended, the bucket had been read for %d claims, up from %d", after, answered)
		if after > answered+walkedAwayDrift {
			t.Fatalf("the bucket was read for %d more claims after the deregistration answered (%d, up from %d), want no more than %d: the listing went on scanning the bucket for a caller that has its answer",
				after-answered, after, answered, walkedAwayDrift)
		}
	})
}
