package docs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// backgroundWaitBound bounds one wait of these tests for the service's background writes: the
// writers they hold end within a second, and a wait that does not return by then has hung.
const backgroundWaitBound = time.Minute

// The wait answers, for a document with rows after its first, the moment its oldest such row
// leaves compactTail, as the compaction queue's own check reads it (checkCompaction), whether ygo
// holds the document's room or has closed it: no compaction under the tail rule can fold before.
func TestTheBackgroundWaitAnswersWhenTheCompactionCheckLetsADocumentFold(t *testing.T) {
	for _, resident := range []bool{true, false} {
		name := "a resident room"
		if !resident {
			name = "a room ygo closed"
		}
		t.Run(name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			writeRoomRows(t, service, artifactID)
			// Every row a minute old, short of the tail, and the first older than the rest, as
			// when the rows after it came later.
			ageUpdatesThrough(t, service.store, artifactID, everyVersion, time.Minute)
			ageUpdatesThrough(t, service.store, artifactID, storedRows(t, service.store, artifactID)[0].version, 90*time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), backgroundWaitBound)
			defer cancel()
			if !resident {
				if err := service.Evict(ctx, artifactID); err != nil {
					t.Fatalf("close the document's room: %v", err)
				}
				if service.srv.GetDoc(artifactID) != nil {
					t.Fatal("UNKNOWN: the document's room is still resident, so this run shows nothing")
				}
			}
			began := time.Now()
			quiet, err := service.AwaitBackgroundWrites(ctx, time.Now())
			took := time.Since(began)
			if err != nil {
				t.Fatalf("wait for the service's background writes: %v", err)
			}
			check, err := service.checkCompaction(ctx, artifactID)
			if err != nil {
				t.Fatalf("check the document's compaction: %v", err)
			}
			if check.later == 0 || check.aged {
				t.Fatalf("UNKNOWN: the check reads %+v, not rows after the first short of the tail, so this run shows nothing", check)
			}
			// The moment itself, as a time the rows carry: the oldest row after the first, plus the
			// tail. The wait reads it as its own clock's moment of asking plus what the check's
			// statement answered was left, so it can read it early by no more than the time
			// between its asking and that statement's start, which falls inside the wait, and late
			// by no more than the microsecond the database rounds a time to.
			var moment time.Time
			if err := service.store.Pool.QueryRow(ctx, `
				select min(created_at) + make_interval(secs => $2) from doc_updates
				where artifact_id = $1 and version <= `+headVersion+`
					and version > (select min(version) from doc_updates where artifact_id = $1)
			`, artifactID, compactTail.Seconds()).Scan(&moment); err != nil {
				t.Fatalf("read when the document's oldest row after its first leaves the tail: %v", err)
			}
			if quiet.At.Before(moment.Add(-took)) || quiet.At.After(moment.Add(time.Microsecond)) || !strings.Contains(quiet.Writer, artifactID) {
				t.Fatalf("the wait answers %s (%s), want the compaction of %s at %s, less at most the %s the wait took",
					quiet.At.Format(time.RFC3339Nano), quiet.Writer, artifactID, moment.Format(time.RFC3339Nano), took)
			}
		})
	}
}

// A document whose rows a compaction may fold already - its room's close, the queue's retry or a
// forced eviction may run one at any moment - the wait compacts itself, so none of them is left
// anything to fold until the next row comes of age, which is the moment it answers.
func TestTheBackgroundWaitCompactsADocumentACompactionMayFoldNow(t *testing.T) {
	service, artifactID := newTestService(t)
	foldPoint := writeRoomRows(t, service, artifactID)
	ageUpdatesThrough(t, service.store, artifactID, foldPoint, time.Hour)
	before := storedRows(t, service.store, artifactID)
	ctx, cancel := context.WithTimeout(context.Background(), backgroundWaitBound)
	defer cancel()
	if check, err := service.checkCompaction(ctx, artifactID); err != nil || !check.aged {
		t.Fatalf("UNKNOWN: the check reads %+v (%v), not rows a compaction may fold, so this run shows nothing", check, err)
	}
	quiet, err := service.AwaitBackgroundWrites(ctx, time.Now())
	if err != nil {
		t.Fatalf("wait for the service's background writes: %v", err)
	}
	if after := storedRows(t, service.store, artifactID); len(after) >= len(before) {
		t.Fatalf("the document stores %d rows after the wait, as many as the %d before it: the wait left the rows a compaction may fold", len(after), len(before))
	}
	if check, err := service.checkCompaction(ctx, artifactID); err != nil || check.aged {
		t.Fatalf("after the wait the check reads %+v (%v), want no row a compaction may fold", check, err)
	}
	if !quiet.Waited || !quiet.At.After(time.Now()) {
		t.Fatalf("the wait answers waited=%v at %s (%s), want it to have compacted and to answer the next row's coming of age",
			quiet.Waited, quiet.At.Format(time.RFC3339Nano), quiet.Writer)
	}
}

// A look for the service's background writes reads the registers a writer stands in before the
// counts it keeps while it runs, and takes a second look before the wait goes on, so a writer that
// passes from one to the other while a look reads is seen: one that starts and enters a count once
// the registers are read, and one that enters a register once they are read.
func TestTheBackgroundWaitSeesAWriterThatMovesBetweenItsReads(t *testing.T) {
	for _, test := range []struct {
		name  string
		start func(service *Service, room string) (end func())
	}{
		{"a settlement callback that drops its timer", func(service *Service, _ string) func() {
			timerID := service.nextSettleTimer.Add(1)
			service.registerSettleTimer(timerID, "between-reads", time.Now())
			service.settleCallbacks.Add(1)
			service.unregisterSettleTimer(timerID)
			return func() { service.settleCallbacks.Add(-1) }
		}},
		{"a forced eviction an append starts", func(service *Service, _ string) func() {
			service.evicting.Add(1)
			return func() { service.evicting.Add(-1) }
		}},
		{"a room update a socket leaves unstored", func(service *Service, room string) func() {
			state := service.lockState(room)
			state.pendingUpdates++
			service.unlockState(room, state)
			return func() {
				state := service.lockState(room)
				state.pendingUpdates--
				service.countsFellLocked(room, state)
				service.unlockState(room, state)
			}
		}},
		{"parked bytes a drain leaves for a store", func(service *Service, room string) func() {
			state := service.lockState(room)
			state.parked = append(state.parked, parkedBytes{update: []byte{0}})
			service.unlockState(room, state)
			return func() {
				state := service.lockState(room)
				state.parked = nil
				service.unlockState(room, state)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			const runs = 300 * time.Millisecond
			var once sync.Once
			ended := make(chan time.Time, 1)
			service.betweenBackgroundReads = func() {
				once.Do(func() {
					end := test.start(service, artifactID)
					time.AfterFunc(runs, func() {
						end()
						ended <- time.Now()
					})
				})
			}
			ctx, cancel := context.WithTimeout(context.Background(), backgroundWaitBound)
			defer cancel()
			quiet, err := service.AwaitBackgroundWrites(ctx, time.Now().Add(time.Second))
			returned := time.Now()
			if err != nil {
				t.Fatalf("wait for the service's background writes: %v", err)
			}
			select {
			case at := <-ended:
				if returned.Before(at) || !quiet.Waited {
					t.Fatalf("the wait returned at %s (waited=%v), before the writer ended at %s", returned.Format(time.RFC3339Nano), quiet.Waited, at.Format(time.RFC3339Nano))
				}
			default:
				t.Fatalf("the wait returned (waited=%v) while the writer that moved between its reads still ran", quiet.Waited)
			}
		})
	}
}

// One look reads the registers a writer stands in while it is owed before the counts it keeps
// while it runs (runningBackgroundWrite), so a writer that leaves a register for a count while the
// look reads is in what the look reads after: here a settlement whose timer fires between the
// look's reads, dropping its timer as its callback counts itself. The look alone must name it: a
// wait's second look would hide a look that read the other way round.
func TestALookForBackgroundWritesReadsTheRegistersBeforeTheCounts(t *testing.T) {
	service, _ := newTestService(t)
	timerID := service.nextSettleTimer.Add(1)
	service.registerSettleTimer(timerID, "between-reads", time.Now())
	var moved atomic.Bool
	service.betweenBackgroundReads = func() {
		if moved.CompareAndSwap(false, true) {
			service.settleCallbacks.Add(1)
			service.unregisterSettleTimer(timerID)
		}
	}
	t.Cleanup(func() {
		if moved.Load() {
			service.settleCallbacks.Add(-1)
		} else {
			service.unregisterSettleTimer(timerID)
		}
	})
	if writer := service.runningBackgroundWrite(time.Now().Add(time.Second)); writer == "" {
		t.Fatal("a look found no background write while a settlement left its timer for its callback between the look's reads")
	}
}

// A document whose aged rows do not fold into a state that reads back as them keeps them as
// stored (compactUnfoldable), so they stay aged and a compaction at a later fold point may fold
// them at any moment: the wait fails at once, naming the document and why, rather than compact it
// again on every pass until its bound.
func TestTheBackgroundWaitFailsNamingADocumentWhoseAgedRowsDoNotFold(t *testing.T) {
	service, _ := newTestService(t)
	versioned := service.persistence.(*PgVersioned)
	shrinkPendingItems(t, service, versioned)
	artifactID := createDocument(t, service.store, "unfoldable")
	rows, _ := unfoldableHistory()
	for index, row := range rows {
		if _, err := versioned.AppendUpdate(context.Background(), artifactID, row); err != nil {
			t.Fatalf("append update %d: %v", index+1, err)
		}
	}
	ageUpdatesThrough(t, service.store, artifactID, everyVersion, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), backgroundWaitBound)
	defer cancel()
	if check, err := service.checkCompaction(ctx, artifactID); err != nil || !check.aged {
		t.Fatalf("UNKNOWN: the check reads %+v (%v), not rows a compaction may fold, so this run shows nothing", check, err)
	}
	_, err := service.AwaitBackgroundWrites(ctx, time.Now())
	if err == nil || !strings.Contains(err.Error(), "document "+artifactID) || !strings.Contains(err.Error(), "do not fold") {
		t.Fatalf("the wait beside a document whose aged rows do not fold answered %v, want it to fail naming the document and why", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("the wait failed only at its bound (%v), not at once", err)
	}
}

// A document socket writes on its own goroutines - its peer's updates and their parked bytes, its
// last browser's settlement and the retirement of the room that settlement loaded - so the wait
// waits for it to end, teardown included. A wait whose bound ends while the socket is open fails
// naming it wherever the bound falls: here inside the look after one that found the socket, which
// the test holds there until the wait has returned, as a lock of the service held past the bound
// would (within). Every step waits for the event it checks, and backgroundWaitBound only fails a
// wait that hangs.
func TestTheBackgroundWaitWaitsOutADocumentSocket(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	connection, err := dialDocument(httpServer.URL, artifactID)
	if err != nil {
		t.Fatalf("open the document's socket: %v", err)
	}
	// The hook runs in each look that gets past the rooms and the timers, before it reads the
	// counts the socket is one of; it is set once, and each phase reads what it needs of it.
	var looks atomic.Int32
	var bounded atomic.Bool
	ended, unended := context.WithCancel(context.Background())
	defer unended()
	returned := make(chan struct{})
	service.betweenBackgroundReads = func() {
		n := looks.Add(1)
		if bounded.Load() && n == 2 {
			// The look before this one found the socket; the bound ends inside this one.
			unended()
			<-returned
		}
	}

	bounded.Store(true)
	_, err = service.AwaitBackgroundWrites(ended, time.Now().Add(time.Second))
	close(returned)
	bounded.Store(false)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "a document socket") {
		t.Fatalf("a wait whose bound ended inside a look after the open socket was found answered %v, want it to fail naming the socket", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), backgroundWaitBound)
	defer cancel()
	from := looks.Load()
	result := make(chan error, 1)
	go func() {
		_, err := service.AwaitBackgroundWrites(ctx, time.Now().Add(time.Second))
		result <- err
	}()
	// A wait that saw no socket ends after two looks that found nothing; one that sees it looks on.
	for looks.Load() < from+3 {
		select {
		case err := <-result:
			t.Fatalf("the wait beside an open document socket returned (%v) after %d looks: it did not wait for the socket", err, looks.Load()-from)
		case <-ctx.Done():
			t.Fatalf("the wait beside an open document socket took fewer than 3 looks within %s", backgroundWaitBound)
		case <-time.After(backgroundPoll):
		}
	}
	leaveDocument(connection)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("wait for the service's background writes once the socket closed: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("the wait did not return within %s of the socket's close", backgroundWaitBound)
	}
	if served := service.serving.Load(); served != 0 {
		t.Fatalf("the wait returned while %d document sockets were still served", served)
	}
}

// A look for the service's background writes takes the service's locks, and a holder that stalls
// keeps one past any bound: the wait fails at its own bound, naming the look, rather than waiting
// for the lock. The test holds the lock until the wait has returned, so any return is one the lock
// did not hold, and backgroundWaitBound only fails a wait that waited on the lock.
func TestTheBackgroundWaitFailsAtItsBoundWhenALockOutlastsIt(t *testing.T) {
	for _, test := range []struct {
		name string
		hold func(service *Service, room string) (release func())
	}{
		{"the settlement timers' lock", func(service *Service, _ string) func() {
			service.timerMu.Lock()
			return service.timerMu.Unlock
		}},
		{"a room's lock", func(service *Service, room string) func() {
			state := service.lockState(room)
			return func() { service.unlockState(room, state) }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, "held")
			// Its appends landed, so a look reaches the lock rather than stopping at them.
			waitForLandedAppends(t, service, artifactID)
			release := sync.OnceFunc(test.hold(service, artifactID))
			defer release()
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				_, err := service.AwaitBackgroundWrites(ctx, time.Now().Add(time.Second))
				result <- err
			}()
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "look for the document service's background writes") {
					t.Fatalf("the wait with %s held answered %v, want its bound's end, naming the look", test.name, err)
				}
			case <-time.After(backgroundWaitBound):
				t.Fatalf("the wait with %s held did not return within %s of its bound's end: it waited on the lock", test.name, backgroundWaitBound)
			}
		})
	}
}
