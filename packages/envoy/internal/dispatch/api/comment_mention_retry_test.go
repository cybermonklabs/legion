package api

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// retryRig is a mention sender whose sends to one recipient, session:waits, wait for an earlier
// event (the send answers its item to take up again, as sendCommentMention does for a
// *mentionHeldForOrder) until the test opens it, and whose every other send takes a moment and
// records itself. Its timers are the sender's own (after, which starts a time.AfterFunc), counted:
// pending is how many retries' timers have been started and not run, which the sender starts only
// once no goroutine serves the waiting lane (serve), and fired how many have run. scheduled has a
// value once a retry's timer has started since it was last read (awaitRetry).
type retryRig struct {
	sender    *mentionSender
	open      atomic.Bool
	pending   atomic.Int32
	fired     atomic.Int64
	scheduled chan struct{}
	mu        sync.Mutex
	tries     []time.Time
	sent      []string
	active    int
	most      int
	written   map[mentionKey]int
	gaveUp    []mentionClaim
}

func newRetryRig(t *testing.T, capacity int) *retryRig {
	t.Helper()
	lifetime, stop := context.WithCancel(context.Background())
	rig := &retryRig{written: map[mentionKey]int{}, scheduled: make(chan struct{}, 1)}
	rig.sender = newMentionSender(lifetime, 100*time.Millisecond,
		func(_ context.Context, item mentionSend) (mentionSend, bool) {
			if item.mention.Target == "session:waits" && !rig.open.Load() {
				rig.mu.Lock()
				rig.tries = append(rig.tries, time.Now())
				rig.mu.Unlock()
				return item, true
			}
			rig.mu.Lock()
			rig.active++
			rig.most = max(rig.most, rig.active)
			rig.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			rig.mu.Lock()
			rig.active--
			rig.sent = append(rig.sent, item.comment.ID)
			rig.mu.Unlock()
			return mentionSend{}, false
		},
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			for _, claim := range queued {
				rig.written[claim.key]++
			}
			return queued, nil
		},
		rereadNone,
		func(_ context.Context, held []mentionClaim) error {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.gaveUp = append(rig.gaveUp, held...)
			return nil
		},
	)
	rig.sender.mu.Lock()
	rig.sender.capacity = capacity
	own := rig.sender.after
	rig.sender.after = func(delay time.Duration, f func()) {
		rig.pending.Add(1)
		own(delay, func() {
			rig.pending.Add(-1)
			rig.fired.Add(1)
			f()
		})
		select {
		case rig.scheduled <- struct{}{}:
		default:
		}
	}
	rig.sender.mu.Unlock()
	t.Cleanup(func() {
		stop()
		rig.sender.wait()
	})
	return rig
}

func (r *retryRig) enqueue(id, target string) mentionSend {
	item := mentionSend{
		comment: model.Comment{ID: id}, mention: ResolvedMention{Target: target}, attempt: 1, claimed: time.Now(),
	}
	r.sender.enqueue(context.Background(), item)
	return item
}

// awaitRetry waits for the sender's next scheduled retry - which, in a synctest bubble, moves the bubble's
// clock on to the timers that make the next tries - and then for every goroutine of the bubble to
// block. A lane that stops trying never waits for a retry again, and the bubble never deadlocks while the
// sender's claim refresh ticks in it, so the wait is bounded on the bubble's clock (retryLiveness),
// which costs no real time: past it the test fails at once, naming what the sender holds.
func (r *retryRig) awaitRetry(t *testing.T) {
	t.Helper()
	select {
	case <-r.scheduled:
	case <-time.After(retryLiveness()):
		serving, waiting, onLane, queued := r.state()
		t.Fatalf("no retry scheduled within %v of the bubble's clock: %d goroutines serve lanes, the held lane waits for its retry %v with %d sends on it, %d sends queued; a lane that stops trying never waits for a retry again",
			retryLiveness(), serving, waiting, onLane, queued)
	}
	synctest.Wait()
}

// retryLiveness is how long awaitRetry waits on the bubble's clock for the next scheduled retry. It is
// liveness alone, never the bound under test, so it grows with the sender's own waits
// (mentionOrderRetryFirst, mentionOrderRetryMax): a schedule of longer waits reads the rule less,
// which the cost requirement allows, and must not fail here.
func retryLiveness() time.Duration {
	return time.Minute + 2*(mentionOrderRetryFirst+mentionOrderRetryMax)
}

// state is what the sender holds now: how many goroutines serve lanes, whether session:waits's
// held lane waits for its retry - its timer started and not run - how many sends wait on it, and how many
// claims it writes again, one for each send it holds (mentionSender.held). The rig's comments
// name no owner, so they share one held lane (heldLane). The four are read as of one moment: a
// read that a timer's run crossed is read again, since the run starts a goroutine on the lane
// (retryLane) after it counts itself fired.
func (r *retryRig) state() (serving int, waiting bool, onLane, queued int) {
	held := heldLane(laneKey{recipient: "session:waits"}, "")
	for {
		fired, pending := r.fired.Load(), r.pending.Load()
		r.sender.mu.Lock()
		serving, onLane, queued = r.sender.serving, len(r.sender.lanes[held]), len(r.sender.held)
		r.sender.mu.Unlock()
		if r.fired.Load() == fired && r.pending.Load() == pending {
			return serving, pending > 0, onLane, queued
		}
	}
}

// countRetries has sender count each retry whose timer it starts - which it does once no goroutine
// serves the waiting lane (serve) - and start that timer as the sender does, unless hold, which
// leaves every waiting lane waiting for the rest of the test. It answers the count. Call it before
// the first send is queued.
func countRetries(sender *mentionSender, hold bool) *atomic.Int32 {
	retries := &atomic.Int32{}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	sender.after = func(delay time.Duration, f func()) {
		retries.Add(1)
		if !hold {
			time.AfterFunc(delay, f)
		}
	}
	return retries
}

// serveStacks counts the goroutines whose stack is in the sender's lane loop.
func serveStacks() int {
	buffer := make([]byte, 1<<22)
	stacks := string(buffer[:runtime.Stack(buffer, true)])
	return strings.Count(stacks, "(*mentionSender).serve(")
}

// retryReadEvery and retryFirstWait are the pace the cost requirement allows a waiting lane,
// written here rather than read from mentionOrderRetryMax and mentionOrderRetryFirst, so that a
// change to the sender's schedule fails these tests rather than moving their bound with it. Once
// its waits reach their longest value, a waiting lane reads the order rule at most once each 2 s:
// N waiting lanes and M waiting fallback rows read it at most N + M times each 2 s. Its first wait
// is 50 ms, and each retry of the same send doubles it up to that bound. A longer wait only reads
// the rule less, so no wait is bounded above.
const (
	retryReadEvery = 2 * time.Second
	retryFirstWait = 50 * time.Millisecond
)

// pacedTries is how many tries the pacing tests read of a waiting send: its first, and one after
// each of nine waits, the last three of which come after the ramp, at retryReadEvery or more.
const pacedTries = 10

// retryWait is the shortest wait the cost requirement allows before a waiting send's next try,
// the retries'th time it waited: retryFirstWait doubled for each retry before it, at most
// retryReadEvery.
func retryWait(retries int) time.Duration {
	wait := retryFirstWait
	for n := 1; n < retries && wait < retryReadEvery; n++ {
		wait *= 2
	}
	return min(wait, retryReadEvery)
}

// shortWaits reads the times a waiting send was tried at: the waits between them, and each wait
// shorter than retryWait allows, described.
func shortWaits(tries []time.Time) (waits []time.Duration, short []string) {
	for index := 1; index < len(tries); index++ {
		wait := tries[index].Sub(tries[index-1])
		waits = append(waits, wait)
		if want := retryWait(index); wait < want {
			short = append(short, fmt.Sprintf("wait %d is %v, want at least %v", index, wait, want))
		}
	}
	return waits, short
}

// A send that waits for an earlier event of its comment's owner leaves its lane waiting for its retry: no goroutine is
// on that lane, so no goroutine holds a connection for it, while it waits, however long - here
// pacedTries tries, past the point where its waits reach their longest value - and each wait
// before its next try is at least retryWait: 50 ms doubling to 2 s, then 2 s or more each. The
// lane keeps its order: the send behind it waits behind it. The waiting sends still count among
// the maxQueuedMentionSends the queue holds: with a capacity of two, the two waiting sends fill it
// and a third recipient's send is refused and its claim given back, whenever it comes. Every other
// recipient's sends go meanwhile, at most mentionSendLanes at once, and the waiting sends' claims
// are written again as every queued send's are. Once the earlier event is sent, the lane sends
// its two in order.
//
// The test runs in a synctest bubble, so its clock is the bubble's: each wait is read as the
// sender scheduled it, whatever the machine's load, and the test waits for pacedTries tries
// however long the schedule makes them take; a lane that stops trying fails at once (awaitRetry).
// The lane is read after each retry, once every goroutine of the bubble is blocked (awaitRetry),
// which is when a goroutine that slept a retry's wait out on the lane would still be on it.
func TestALaneWaitingForItsRetryHoldsNoGoroutineAndKeepsTheLanesBounds(t *testing.T) {
	// Not parallel: serveStacks reads every goroutine of the process (runtime.Stack, all), so a
	// lane loop of another test's sender counts as this lane's (envoy-go-race-api job 137408608768).
	synctest.Test(t, func(t *testing.T) {
		rig := newRetryRig(t, 2)
		first := rig.enqueue("waits-first", "session:waits")
		second := rig.enqueue("waits-second", "session:waits")
		synctest.Wait()
		if serving, waiting, onLane, _ := rig.state(); !waiting || serving != 0 || onLane != 2 {
			t.Fatalf("once every goroutine is blocked the lane waits for its retry %v, with %d goroutines serving and %d sends on it; want it waiting, 0 and 2",
				waiting, serving, onLane)
		}
		if stacks := serveStacks(); stacks != 0 {
			t.Fatalf("%d goroutines are in the sender's lane loop while its one lane waits for its retry, want none", stacks)
		}

		// The two waiting sends fill a queue of two, however long they have waited: another
		// recipient's send is refused and its claim given back.
		refused := rig.enqueue("one-too-many", "session:other-z")
		rig.mu.Lock()
		gaveUp := append([]mentionClaim(nil), rig.gaveUp...)
		rig.mu.Unlock()
		if len(gaveUp) != 1 || gaveUp[0].key != refused.key() {
			t.Fatalf("the queue gave back %v with its two places held by the waiting sends, want the one send past its capacity", gaveUp)
		}

		// Ten other recipients' sends go while the lane waits for its retry, the queue holding twelve.
		rig.sender.mu.Lock()
		rig.sender.capacity = 12
		rig.sender.mu.Unlock()
		for index := range 10 {
			rig.enqueue("other-"+string(rune('a'+index)), "session:other-"+string(rune('a'+index)))
		}
		// A send is in the rig's list before its goroutine lets it go (claimedDuring) and leaves
		// its lane (next), so the ten are done only once the sender holds the two waiting sends
		// alone and no goroutine serves a lane. The wait is bounded on the bubble's clock
		// (retryLiveness), as awaitRetry's is, which costs no real time. waitUntil's bound is the
		// test binary's deadline, a time on the machine's clock, which the bubble's clock, starting
		// in 2000, never reaches, and the sender's claim refresh keeps the bubble from deadlocking,
		// so a send that never goes would hold the row until go test's alarm.
		othersDone := func() bool {
			rig.mu.Lock()
			sent := len(rig.sent)
			rig.mu.Unlock()
			serving, _, _, queued := rig.state()
			return sent == 10 && queued == 2 && serving == 0
		}
		if !testwait.Holds(retryLiveness(), testwait.Poll, othersDone) {
			rig.mu.Lock()
			sent := append([]string(nil), rig.sent...)
			rig.mu.Unlock()
			serving, _, _, queued := rig.state()
			t.Fatalf("the other recipients' sends were not done within %v of the bubble's clock: sent %v, %d sends queued (want the 2 waiting), %d goroutines serving (want none)",
				retryLiveness(), sent, queued, serving)
		}

		// The waiting send's tries until it has made pacedTries, the lane read after each retry.
		for {
			rig.mu.Lock()
			tried := len(rig.tries)
			rig.mu.Unlock()
			if tried >= pacedTries {
				break
			}
			rig.awaitRetry(t)
			serving, waiting, onLane, queued := rig.state()
			if !waiting || serving != 0 || onLane != 2 || queued != 2 {
				t.Fatalf("once every goroutine is blocked the lane waits for its retry %v, with %d goroutines serving, %d sends on it and %d queued; want it waiting, 0, 2 and 2",
					waiting, serving, onLane, queued)
			}
		}
		if stacks := serveStacks(); stacks != 0 {
			t.Fatalf("%d goroutines are in the sender's lane loop after %d tries of the waiting send, want none", stacks, pacedTries)
		}

		rig.mu.Lock()
		tries := append([]time.Time(nil), rig.tries...)
		most, sent, gaveUp := rig.most, append([]string(nil), rig.sent...), append([]mentionClaim(nil), rig.gaveUp...)
		writtenFirst, writtenSecond := rig.written[first.key()], rig.written[second.key()]
		rig.mu.Unlock()
		waits, short := shortWaits(tries)
		t.Logf("the waiting send's tries: %d, the waits between them %v; the most sends at once %d; queued sends written again: first %d times, second %d times",
			len(tries), waits, most, writtenFirst, writtenSecond)
		if len(short) != 0 {
			t.Errorf("the waiting send's waits are shorter than the cost requirement allows (retryWait): %s", strings.Join(short, "; "))
		}
		if most > mentionSendLanes {
			t.Errorf("%d sends ran at once, want at most mentionSendLanes (%d)", most, mentionSendLanes)
		}
		if len(sent) != 10 {
			t.Errorf("sent %v while the lane waited for its retry, want the ten other recipients' sends", sent)
		}
		if len(gaveUp) != 1 || gaveUp[0].key != refused.key() {
			t.Errorf("the queue gave back %v, want only the one send refused while the waiting sends filled it", gaveUp)
		}
		if writtenFirst == 0 || writtenSecond == 0 {
			t.Errorf("the waiting sends' claims were written again %d and %d times, want both kept live", writtenFirst, writtenSecond)
		}

		// Once open, the lane's next try sends both, whenever its timer runs.
		rig.open.Store(true)
		for {
			synctest.Wait()
			rig.mu.Lock()
			sent := len(rig.sent)
			rig.mu.Unlock()
			if sent == 12 {
				break
			}
			if serving, waiting, _, _ := rig.state(); !waiting && serving == 0 {
				t.Fatalf("the lane sent %d of its two once open and neither waits for its retry nor is served, so it never will", sent-10)
			}
			time.Sleep(time.Second)
		}
		rig.mu.Lock()
		order := append([]string(nil), rig.sent[10:]...)
		rig.mu.Unlock()
		if order[0] != "waits-first" || order[1] != "waits-second" {
			t.Errorf("the lane sent %v once open, want waits-first and then waits-second", order)
		}
	})
}

// The read load of many waiting lanes: each waiting lane's tries come at least retryWait apart, and
// a try of a held send reads the rule once (TestAHeldMentionSendIsCountedAndReportedOnce counts
// that on the database), so N lanes held behind a stuck topic family read the rule at most N
// times each retryReadEvery once their waits have ramped up - here 200 lanes, each tried at least
// pacedTries times, every wait between two tries of one lane read. It runs in a synctest bubble,
// as the test above does: each wait is the one the sender scheduled, the test waits for the tries
// however long the schedule makes them take, and after each retry, with every goroutine of the
// bubble blocked, no goroutine serves a lane. A retry that held its goroutine (slept its wait out
// on it) would keep one on every lane it serves. The wait for the tries is bounded on the bubble's
// clock by progress, as awaitRetry's is: it fails by name once the fewest tries of any lane has
// not grown for retryLiveness, which costs no real time. One lane that keeps waiting for its retry
// keeps awaitRetry answering, so a lane that is never tried, or stops being tried, would otherwise
// hold the row until go test's alarm.
func TestManyLanesWaitingForTheirRetryTryAtMostOncePerRetryMax(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const lanes = 200
		rig := newRetryRig(t, maxQueuedMentionSends)
		tries := map[string][]time.Time{}
		var triesMu sync.Mutex
		rig.sender.send = func(_ context.Context, item mentionSend) (mentionSend, bool) {
			triesMu.Lock()
			tries[item.mention.Target] = append(tries[item.mention.Target], time.Now())
			triesMu.Unlock()
			return item, true
		}
		started := time.Now()
		for index := range lanes {
			rig.enqueue(fmt.Sprintf("held-%d", index), fmt.Sprintf("session:held-%d", index))
		}
		progressed, lastFewest := time.Now(), -1
		for {
			triesMu.Lock()
			fewest, tried := pacedTries, len(tries)
			if tried < lanes {
				fewest = 0
			}
			for _, times := range tries {
				fewest = min(fewest, len(times))
			}
			triesMu.Unlock()
			if fewest >= pacedTries {
				break
			}
			if fewest > lastFewest {
				progressed, lastFewest = time.Now(), fewest
			} else if stalled := time.Since(progressed); stalled > retryLiveness() {
				serving, _, _, queued := rig.state()
				t.Fatalf("the fewest tries of any lane stayed %d for %v of the bubble's clock: %d of the %d lanes tried at all, %d goroutines serving, %d sends queued; a lane never tried, or no longer tried, never reaches %d tries",
					fewest, stalled, tried, lanes, serving, queued, pacedTries)
			}
			rig.awaitRetry(t)
			if serving, _, _, queued := rig.state(); serving != 0 || queued != lanes {
				t.Fatalf("once every goroutine is blocked %d goroutines serve the waiting lanes and %d sends are queued; want none serving and the %d waiting sends queued",
					serving, queued, lanes)
			}
		}
		elapsed := time.Since(started)
		triesMu.Lock()
		total, read := 0, 0
		var short []string
		for lane, times := range tries {
			total += len(times)
			waits, laneShort := shortWaits(times)
			read += len(waits)
			for _, wait := range laneShort {
				short = append(short, lane+": "+wait)
			}
		}
		triesMu.Unlock()
		slices.Sort(short)
		t.Logf("%d lanes waited for their retries for %v of the bubble's clock: %d tries, %d waits between two tries of one lane read, %d of them shorter than retryWait",
			lanes, elapsed, total, read, len(short))
		if len(short) != 0 {
			t.Fatalf("%d waits between two tries of one lane are shorter than the cost requirement allows (retryWait), the first %s",
				len(short), strings.Join(short[:min(5, len(short))], "; "))
		}
	})
}
