package api

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// parkRig is a mention sender whose sends to one recipient, session:parked, wait for an earlier
// event (the send answers its item to take up again, as sendCommentMention does for a
// *mentionHeldForOrder) until the test opens it, and whose every other send takes a moment and
// records itself. Its timers are the sender's own (time.AfterFunc), counted: pending is how many
// parks' timers have been started and not run, which the sender starts only once no goroutine
// serves the parked lane (serve), and fired how many have run.
type parkRig struct {
	sender  *mentionSender
	open    atomic.Bool
	pending atomic.Int32
	fired   atomic.Int64
	mu      sync.Mutex
	tries   []time.Time
	sent    []string
	active  int
	most    int
	written map[mentionKey]int
	gaveUp  []mentionClaim
}

func newParkRig(t *testing.T, capacity int) *parkRig {
	t.Helper()
	lifetime, stop := context.WithCancel(context.Background())
	rig := &parkRig{written: map[mentionKey]int{}}
	rig.sender = newMentionSender(lifetime, 100*time.Millisecond,
		func(_ context.Context, item mentionSend) (mentionSend, bool) {
			if item.mention.Target == "session:parked" && !rig.open.Load() {
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
	rig.sender.after = func(delay time.Duration, f func()) {
		rig.pending.Add(1)
		time.AfterFunc(delay, func() {
			rig.pending.Add(-1)
			rig.fired.Add(1)
			f()
		})
	}
	rig.sender.mu.Unlock()
	t.Cleanup(func() {
		stop()
		rig.sender.wait()
	})
	return rig
}

func (r *parkRig) enqueue(id, target string) mentionSend {
	item := mentionSend{
		comment: model.Comment{ID: id}, mention: ResolvedMention{Target: target}, attempt: 1, claimed: time.Now(),
	}
	r.sender.enqueue(context.Background(), item)
	return item
}

// state is what the sender holds now: how many goroutines serve lanes, whether session:parked's
// held lane is parked - its timer started and not run - how many sends wait on it, and how many
// claims it writes again, one for each send it holds (mentionSender.held). The rig's comments
// name no owner, so they share one held lane (heldLane). The four are read as of one moment: a
// read that a timer's run crossed is read again, since the run starts a goroutine on the lane
// (unpark) after it counts itself fired.
func (r *parkRig) state() (serving int, parked bool, onLane, queued int) {
	held := heldLane(laneKey{recipient: "session:parked"}, "")
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

// countParks has sender count each park whose timer it starts - which it does once no goroutine
// serves the parked lane (serve) - and start that timer as the sender does, unless hold, which
// leaves every parked lane parked for the rest of the test. It answers the count. Call it before
// the first send is queued.
func countParks(sender *mentionSender, hold bool) *atomic.Int32 {
	parks := &atomic.Int32{}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	sender.after = func(delay time.Duration, f func()) {
		parks.Add(1)
		if !hold {
			time.AfterFunc(delay, f)
		}
	}
	return parks
}

// serveStacks counts the goroutines whose stack is in the sender's lane loop.
func serveStacks() int {
	buffer := make([]byte, 1<<22)
	stacks := string(buffer[:runtime.Stack(buffer, true)])
	return strings.Count(stacks, "(*mentionSender).serve(")
}

// A send that waits for an earlier event of its comment's owner parks its lane: no goroutine is
// on that lane, so no goroutine holds a connection for it, while it waits, however long - here
// six seconds, past the point where its tries come at mentionOrderRetryMax - and its tries come
// after waits that double from mentionOrderRetryFirst to that cap. The lane keeps its order:
// the send behind it waits behind it. The parked sends still count among the
// maxQueuedMentionSends the queue holds: with a capacity of two, the two parked sends fill it and
// a third recipient's send is refused and its claim given back, whenever it comes. Every other
// recipient's sends go meanwhile, at most mentionSendLanes at once, and the parked sends' claims
// are written again as every queued send's are. Once the earlier event is sent, the lane sends
// its two in order.
func TestAParkedLaneHoldsNoGoroutineAndKeepsTheLanesBounds(t *testing.T) {
	rig := newParkRig(t, 2)
	first := rig.enqueue("parked-first", "session:parked")
	second := rig.enqueue("parked-second", "session:parked")
	waitUntil(t, 5*time.Second, "the parked lane with no goroutine on it", func() bool {
		serving, parked, onLane, _ := rig.state()
		return parked && serving == 0 && onLane == 2
	})
	if stacks := serveStacks(); stacks != 0 {
		t.Fatalf("%d goroutines are in the sender's lane loop while its one lane is parked, want none", stacks)
	}

	// The two parked sends fill a queue of two, however long they have waited: another
	// recipient's send is refused and its claim given back.
	refused := rig.enqueue("one-too-many", "session:other-z")
	rig.mu.Lock()
	gaveUp := append([]mentionClaim(nil), rig.gaveUp...)
	rig.mu.Unlock()
	if len(gaveUp) != 1 || gaveUp[0].key != refused.key() {
		t.Fatalf("the queue gave back %v with its two places held by the parked sends, want the one send past its capacity", gaveUp)
	}

	// Ten other recipients' sends go while the lane is parked, the queue holding twelve.
	rig.sender.mu.Lock()
	rig.sender.capacity = 12
	rig.sender.mu.Unlock()
	for index := range 10 {
		rig.enqueue("other-"+string(rune('a'+index)), "session:other-"+string(rune('a'+index)))
	}
	waitUntil(t, 10*time.Second, "the other recipients' sends", func() bool {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		return len(rig.sent) == 10
	})

	// Six seconds parked, sampled every 10 ms between the lane's tries.
	parkedSince := time.Now()
	for time.Since(parkedSince) < 6*time.Second {
		serving, parked, onLane, queued := rig.state()
		if parked && (serving != 0 || onLane != 2 || queued != 2) {
			t.Fatalf("parked lane: %d goroutines serving, %d sends on it, %d queued; want 0, 2 and 2", serving, onLane, queued)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stacks := serveStacks(); stacks != 0 {
		t.Fatalf("%d goroutines are in the sender's lane loop after six seconds parked, want none", stacks)
	}

	rig.mu.Lock()
	tries := append([]time.Time(nil), rig.tries...)
	most, sent, gaveUp := rig.most, append([]string(nil), rig.sent...), append([]mentionClaim(nil), rig.gaveUp...)
	writtenFirst, writtenSecond := rig.written[first.key()], rig.written[second.key()]
	rig.mu.Unlock()
	var gaps []time.Duration
	for index := 1; index < len(tries); index++ {
		gaps = append(gaps, tries[index].Sub(tries[index-1]).Round(10*time.Millisecond))
	}
	t.Logf("the parked send's tries: %d, the waits between them %v; the most sends at once %d; queued sends written again: first %d times, second %d times",
		len(tries), gaps, most, writtenFirst, writtenSecond)
	// A wait shorter than its mentionOrderRetry would read the rule more often than the cost bound
	// says; a longer one only reads it less, so it is bounded loosely, for a loaded machine's late
	// timers.
	for index, gap := range gaps {
		want := mentionOrderRetry(index + 1)
		if gap < want-10*time.Millisecond || gap > want+2*time.Second {
			t.Errorf("wait %d before the parked send's next try is %v, want about %v (mentionOrderRetry)", index+1, gap, want)
		}
	}
	if len(gaps) == 0 || gaps[len(gaps)-1] < mentionOrderRetryMax-10*time.Millisecond {
		t.Errorf("the waits %v never reached mentionOrderRetryMax (%v)", gaps, mentionOrderRetryMax)
	}
	if most > mentionSendLanes {
		t.Errorf("%d sends ran at once, want at most mentionSendLanes (%d)", most, mentionSendLanes)
	}
	if len(sent) != 10 {
		t.Errorf("sent %v while the lane was parked, want the ten other recipients' sends", sent)
	}
	if len(gaveUp) != 1 || gaveUp[0].key != refused.key() {
		t.Errorf("the queue gave back %v, want only the one send refused while the parked sends filled it", gaveUp)
	}
	if writtenFirst == 0 || writtenSecond == 0 {
		t.Errorf("the parked sends' claims were written again %d and %d times, want both kept live", writtenFirst, writtenSecond)
	}

	rig.open.Store(true)
	waitUntil(t, 5*time.Second, "the parked lane's two sends", func() bool {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		return len(rig.sent) == 12
	})
	rig.mu.Lock()
	order := append([]string(nil), rig.sent[10:]...)
	rig.mu.Unlock()
	if order[0] != "parked-first" || order[1] != "parked-second" {
		t.Errorf("the lane sent %v once open, want parked-first and then parked-second", order)
	}
}

// triesWithin is how many tries a send that waits behind an earlier event makes within elapsed of
// its first: the first, and one each time a park's wait (mentionOrderRetry) runs out.
func triesWithin(elapsed time.Duration) int {
	tries, waited := 1, time.Duration(0)
	for parks := 1; ; parks++ {
		waited += mentionOrderRetry(parks)
		if waited > elapsed {
			return tries
		}
		tries++
	}
}

// The read load of many parked lanes: each parked lane tries once per mentionOrderRetry, and a try
// of a held send reads the rule alone (TestAHeldMentionSendIsCountedAndReportedOnce counts that on
// the database), so N lanes held behind a stuck topic family read the rule at most N times each
// mentionOrderRetryMax once their waits have ramped up - here 200 lanes over eight seconds, every
// try counted, against the bound triesWithin sets for each lane.
func TestManyParkedLanesTryAtMostOncePerRetryMax(t *testing.T) {
	const lanes = 200
	const window = 8 * time.Second
	rig := newParkRig(t, maxQueuedMentionSends)
	tries := map[string]int{}
	var triesMu sync.Mutex
	rig.sender.send = func(_ context.Context, item mentionSend) (mentionSend, bool) {
		triesMu.Lock()
		tries[item.mention.Target]++
		triesMu.Unlock()
		return item, true
	}
	started := time.Now()
	for index := range lanes {
		rig.enqueue(fmt.Sprintf("held-%d", index), fmt.Sprintf("session:held-%d", index))
	}
	time.Sleep(window)
	elapsed := time.Since(started)
	serving, _, _, queued := rig.state()
	triesMu.Lock()
	total, most, laneCount := 0, 0, len(tries)
	for _, count := range tries {
		total += count
		most = max(most, count)
	}
	// Released before the goroutine count is read below: a try takes this lock.
	triesMu.Unlock()
	bound := triesWithin(elapsed)
	t.Logf("%d lanes parked for %v: %d tries (%.1f a second, %.2f a lane a second at the end's 1 per %v), at most %d on one lane (bound %d); %d goroutines serving, %d sends queued",
		lanes, elapsed.Round(time.Millisecond), total, float64(total)/elapsed.Seconds(),
		float64(total)/elapsed.Seconds()/lanes, mentionOrderRetryMax, most, bound, serving, queued)
	if laneCount != lanes {
		t.Fatalf("%d lanes were tried, want all %d", laneCount, lanes)
	}
	if most > bound {
		t.Fatalf("a lane was tried %d times in %v, want at most %d (mentionOrderRetry)", most, elapsed, bound)
	}
	if queued != lanes {
		t.Fatalf("%d sends are queued, want the %d parked ones still among them", queued, lanes)
	}
	// No goroutine serves a parked lane: one serves it only for the moment of a try, so the count
	// reaches 0 between tries. A park that held its goroutine (slept its wait out on it) keeps one
	// on every lane it serves, and the count never reaches 0.
	idle := false
	for deadline := time.Now().Add(time.Second); !idle && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		serving, _, _, _ = rig.state()
		idle = serving == 0
	}
	if !idle {
		t.Fatalf("%d goroutines serve the %d parked lanes throughout a second, want none between tries", serving, lanes)
	}
}
