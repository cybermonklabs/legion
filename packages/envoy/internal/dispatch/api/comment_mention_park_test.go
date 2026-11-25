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
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// parkRig is a mention sender whose sends to one recipient, session:parked, wait for an earlier
// event (the send answers its item to take up again, as sendCommentMention does for a
// *mentionHeldForOrder) until the test opens it, and whose every other send waits at gate, which
// the test releases, and records itself. The sender runs on clock, a fake that moves only when the
// test moves it, so a parked lane tries again only once the test moves the fake to its park's
// deadline. Its timers are the sender's own (an AfterFunc on that fake), counted: pending is how
// many parks' timers have been started and not run, which the sender starts only once no goroutine
// serves the parked lane (serve), and fired how many have run. Its claim refreshes tick every
// mentionClaimRefresh on refreshes, a fake of their own (newMentionSender), so a refresh comes only
// where the test moves refreshes to it, and clock holds the parks alone.
type parkRig struct {
	sender    *mentionSender
	clock     *clocktest.Fake
	refreshes *clocktest.Fake
	open      atomic.Bool
	pending   atomic.Int32
	fired     atomic.Int64
	// release closes gate, at which every send to a recipient other than session:parked waits.
	release func()
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
	gate := make(chan struct{})
	rig := &parkRig{written: map[mentionKey]int{}, clock: clocktest.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		refreshes: clocktest.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		release:   sync.OnceFunc(func() { close(gate) })}
	rig.sender = newMentionSender(lifetime, mentionClaimRefresh, rig.refreshes,
		func(_ context.Context, item mentionSend) (mentionSend, bool) {
			if item.mention.Target == "session:parked" && !rig.open.Load() {
				rig.mu.Lock()
				rig.tries = append(rig.tries, rig.clock.Now())
				rig.mu.Unlock()
				return item, true
			}
			rig.mu.Lock()
			rig.active++
			rig.most = max(rig.most, rig.active)
			rig.mu.Unlock()
			<-gate
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
	rig.sender.clock = rig.clock
	rig.sender.after = func(delay time.Duration, f func()) {
		rig.pending.Add(1)
		rig.clock.AfterFunc(delay, func() {
			rig.pending.Add(-1)
			rig.fired.Add(1)
			f()
		})
	}
	rig.sender.mu.Unlock()
	t.Cleanup(func() {
		rig.release()
		stop()
		testwait.Returns(t, "the park rig's mention sender's lanes and claim refresh to return once its gate opened and its lifetime ended", rig.sender.wait)
	})
	return rig
}

func (r *parkRig) enqueue(id, target string) mentionSend {
	item := mentionSend{
		comment: model.Comment{ID: id}, mention: ResolvedMention{Target: target}, attempt: 1, claimed: r.clock.Now(),
	}
	r.sender.enqueue(context.Background(), item)
	return item
}

// atGate is how many of the rig's sends to other recipients wait at its gate now.
func (r *parkRig) atGate() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
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
// serves the parked lane (serve) - and start that timer as the sender does, on its clock, unless
// hold, which leaves every parked lane parked for the rest of the test. It answers the count. Call
// it before the first send is queued.
func countParks(sender *mentionSender, hold bool) *atomic.Int32 {
	parks := &atomic.Int32{}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	sender.after = func(delay time.Duration, f func()) {
		parks.Add(1)
		if !hold {
			sender.sendClock().AfterFunc(delay, f)
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

// parkedReadEvery and parkedFirstWait are the pace the cost requirement allows a parked lane,
// written here rather than read from mentionOrderRetryMax and mentionOrderRetryFirst, so that a
// change to the sender's schedule fails these tests rather than moving their bound with it. Once
// its waits reach their longest value, a parked lane reads the order rule at most once each 2 s:
// N parked lanes and M waiting fallback rows read it at most N + M times each 2 s. Its first wait
// is 50 ms, and each park of the same send doubles it up to that bound. A longer wait only reads
// the rule less, so no wait is bounded above.
const (
	parkedReadEvery = 2 * time.Second
	parkedFirstWait = 50 * time.Millisecond
)

// parkedWait is the shortest wait the cost requirement allows before a parked send's next try,
// the parks'th time it waited: parkedFirstWait doubled for each park before it, at most
// parkedReadEvery.
func parkedWait(parks int) time.Duration {
	wait := parkedFirstWait
	for park := 1; park < parks && wait < parkedReadEvery; park++ {
		wait *= 2
	}
	return min(wait, parkedReadEvery)
}

// shortWaits reads the times a parked send was tried at: the waits between them, and each wait
// shorter than parkedWait allows, described.
func shortWaits(tries []time.Time) (waits []time.Duration, short []string) {
	for index := 1; index < len(tries); index++ {
		wait := tries[index].Sub(tries[index-1])
		waits = append(waits, wait)
		if want := parkedWait(index); wait < want {
			short = append(short, fmt.Sprintf("wait %d is %v, want at least %v", index, wait, want))
		}
	}
	return waits, short
}

// A send that waits for an earlier event of its comment's owner parks its lane: no goroutine is
// on that lane, so no goroutine holds a connection for it, while it waits, however long - here
// eight tries, past the point where its tries come at mentionOrderRetryMax - and its tries come
// after waits that double from mentionOrderRetryFirst to that cap, each its park's span exactly on
// the fake, which the test moves to each park's deadline in turn, and each at least parkedWait:
// 50 ms doubling to 2 s. The lane keeps its order: the send behind it waits behind it. The parked
// sends still count among the maxQueuedMentionSends the queue holds: with a capacity of two, the
// two parked sends fill it and a third recipient's send is refused and its claim given back,
// whenever it comes. Every other recipient's sends go meanwhile, mentionSendLanes at once and no
// more, held at the rig's gate until the test has seen the sender run that many and keep the rest
// waiting for a goroutine, and the parked sends' claims are written again as every queued send's
// are. Once the earlier event is sent, the lane sends its two in order.
func TestAParkedLaneHoldsNoGoroutineAndKeepsTheLanesBounds(t *testing.T) {
	// Not parallel: serveStacks reads every goroutine of the process (runtime.Stack, all), so a
	// lane loop of another test's sender counts as this lane's (envoy-go-race-api job 137408608768).
	rig := newParkRig(t, 2)
	first := rig.enqueue("parked-first", "session:parked")
	second := rig.enqueue("parked-second", "session:parked")
	testwait.Eventually(t, "the parked lane with no goroutine on it", func() bool {
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

	// Ten other recipients' sends go while the lane is parked, the queue holding twelve. Held at
	// the gate, mentionSendLanes of them are in flight together and the other lanes wait for a
	// goroutine: the sender's own count of the goroutines serving lanes is the bound, and it starts
	// none past it while every one is busy.
	rig.sender.mu.Lock()
	rig.sender.capacity = 12
	rig.sender.mu.Unlock()
	for index := range 10 {
		rig.enqueue("other-"+string(rune('a'+index)), "session:other-"+string(rune('a'+index)))
	}
	testwait.Until(t, fmt.Sprintf("%d of the other recipients' sends in flight, the rest waiting for a goroutine", mentionSendLanes), func() (string, bool) {
		rig.sender.mu.Lock()
		serving, ready := rig.sender.serving, len(rig.sender.ready)
		rig.sender.mu.Unlock()
		inFlight := rig.atGate()
		return fmt.Sprintf("%d at the gate, %d goroutines serving, %d lanes waiting for one", inFlight, serving, ready),
			inFlight == mentionSendLanes && serving == mentionSendLanes && ready == 10-mentionSendLanes
	})
	rig.release()
	// A send is in the rig's list before its goroutine lets it go (claimedDuring) and leaves its
	// lane (next), so the ten are done only once the sender holds the two parked sends alone and
	// no goroutine serves a lane.
	testwait.Eventually(t, "the other recipients' sends", func() bool {
		rig.mu.Lock()
		sent := len(rig.sent)
		rig.mu.Unlock()
		serving, _, _, queued := rig.state()
		return sent == 10 && queued == 2 && serving == 0
	})
	// The sender writes the claims it holds again on its own ticker (keepClaims), whose tick the row
	// brings by moving refreshes to it; the ticker arms the next tick once the refresh has returned,
	// so the next tick armed says the refresh is done, and the claims it wrote are read then.
	refresh := rig.refreshes.AwaitArmed(t, "the sender's first claim refresh", 1)[0]
	rig.refreshes.Set(refresh)
	rig.refreshes.AwaitArmedAt(t, "the sender's claim refresh after the one the row ran", refresh.Add(mentionClaimRefresh))
	rig.mu.Lock()
	firstWritten, secondWritten := rig.written[first.key()], rig.written[second.key()]
	rig.mu.Unlock()
	if firstWritten != 1 || secondWritten != 1 {
		t.Fatalf("the claim refresh wrote the parked sends' claims %d and %d times, want once each: a refresh writes again every claim the sender holds",
			firstWritten, secondWritten)
	}

	// Eight more tries of the parked lane, each as the fake reaches its park's deadline; before
	// each the lane is parked with no goroutine on it and both sends on it.
	const tries = 8
	for try := 1; try <= tries; try++ {
		deadline := rig.clock.AwaitArmed(t, fmt.Sprintf("park %d of the lane", try), 1)[0]
		if serving, parked, onLane, queued := rig.state(); !parked || serving != 0 || onLane != 2 || queued != 2 {
			t.Fatalf("parked lane after park %d: parked %v, %d goroutines serving, %d sends on it, %d queued; want parked, 0, 2 and 2",
				try, parked, serving, onLane, queued)
		}
		if stacks := serveStacks(); stacks != 0 {
			t.Fatalf("%d goroutines are in the sender's lane loop after park %d, want none", stacks, try)
		}
		rig.clock.Set(deadline)
		testwait.Eventually(t, fmt.Sprintf("try %d of the parked lane, and its park", try+1), func() bool {
			rig.mu.Lock()
			tried := len(rig.tries)
			rig.mu.Unlock()
			_, parked, _, _ := rig.state()
			return tried == try+1 && parked
		})
	}

	rig.mu.Lock()
	tried := append([]time.Time(nil), rig.tries...)
	most, sent, gaveUp := rig.most, append([]string(nil), rig.sent...), append([]mentionClaim(nil), rig.gaveUp...)
	rig.mu.Unlock()
	var gaps []time.Duration
	for index := 1; index < len(tried); index++ {
		gaps = append(gaps, tried[index].Sub(tried[index-1]))
	}
	t.Logf("the parked send's tries: %d, the waits between them %v; the most sends at once %d", len(tried), gaps, most)
	// The fake moves only to each park's deadline, so each wait is its park's span exactly: a
	// shorter one would read the rule more often than the cost bound says.
	for index, gap := range gaps {
		if want := mentionOrderRetry(index + 1); gap != want {
			t.Errorf("wait %d before the parked send's next try is %v, want %v (mentionOrderRetry)", index+1, gap, want)
		}
	}
	if len(gaps) == 0 || gaps[len(gaps)-1] != mentionOrderRetryMax {
		t.Errorf("the waits %v never reached mentionOrderRetryMax (%v)", gaps, mentionOrderRetryMax)
	}
	if _, short := shortWaits(tried); len(short) != 0 {
		t.Errorf("the parked send's waits are shorter than the cost requirement allows (parkedWait): %s", strings.Join(short, "; "))
	}
	if most != mentionSendLanes {
		t.Errorf("%d sends ran at once, want mentionSendLanes (%d) and no more", most, mentionSendLanes)
	}
	if len(sent) != 10 {
		t.Errorf("sent %v while the lane was parked, want the ten other recipients' sends", sent)
	}
	if len(gaveUp) != 1 || gaveUp[0].key != refused.key() {
		t.Errorf("the queue gave back %v, want only the one send refused while the parked sends filled it", gaveUp)
	}

	rig.open.Store(true)
	rig.clock.Set(rig.clock.AwaitArmed(t, "the lane's park once its earlier event is sent", 1)[0])
	testwait.Eventually(t, "the parked lane's two sends", func() bool {
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

// The read load of many parked lanes: each parked lane tries once per mentionOrderRetry, each try
// at least parkedWait after the one before, and a try of a held send reads the rule once
// (TestAHeldMentionSendIsCountedAndReportedOnce counts that on the database), so N lanes held
// behind a stuck topic family read the rule at most N times each parkedReadEvery once their waits
// have ramped up - here 200 lanes over eight seconds of the fake, which the test moves to the next
// park's deadline only once every lane whose park ended has been tried again and every lane has
// parked again, its timer armed on the fake: each lane is tried exactly as often as triesWithin
// says, every wait between two tries of one lane is read against parkedWait, and once the lanes
// have parked no goroutine serves any. A park that held its goroutine (slept its wait out on it)
// would keep one on every lane it serves, and the count would never reach 0.
func TestManyParkedLanesTryAtMostOncePerRetryMax(t *testing.T) {
	t.Parallel()
	const lanes = 200
	const window = 8 * time.Second
	rig := newParkRig(t, maxQueuedMentionSends)
	tries := map[string][]time.Time{}
	var triesMu sync.Mutex
	rig.sender.send = func(_ context.Context, item mentionSend) (mentionSend, bool) {
		triesMu.Lock()
		tries[item.mention.Target] = append(tries[item.mention.Target], rig.clock.Now())
		triesMu.Unlock()
		return item, true
	}
	started := rig.clock.Now()
	for index := range lanes {
		rig.enqueue(fmt.Sprintf("held-%d", index), fmt.Sprintf("session:held-%d", index))
	}
	triesSoFar := func() int {
		triesMu.Lock()
		defer triesMu.Unlock()
		total := 0
		for _, times := range tries {
			total += len(times)
		}
		return total
	}
	// allParked waits until the lanes have made tried tries in all and every lane has parked again,
	// its timer armed on the fake past at: no goroutine serves any, every park is counted
	// (pending), and the fake holds one timer per lane, none due by at. The rig counts a park
	// before the sender's AfterFunc arms its timer, and a lane whose goroutine gives it up has left
	// the goroutines serving before that (serve, next), so pending and serving alone can read every
	// lane parked while one park's timer is still to be armed; a move made then arms that timer from
	// the moved fake, and its lane is tried a park later than its waits say. Every timer on the fake
	// is a lane's park: the sender reads the fake for its parks alone (newParkRig), and its claim
	// refreshes tick on a fake of their own that this row never moves (parkRig.refreshes).
	allParked := func(what string, tried int, at time.Time) {
		t.Helper()
		testwait.Until(t, what, func() (string, bool) {
			serving, _, _, queued := rig.state()
			pending := rig.pending.Load()
			armed := rig.clock.Armed()
			total := triesSoFar()
			return fmt.Sprintf("%d tries of %d, %d parks pending, %d timers armed, %d goroutines serving, %d sends queued",
					total, tried, pending, len(armed), serving, queued),
				total == tried && pending == lanes && serving == 0 && len(armed) == lanes && armed[0].After(at)
		})
	}
	allParked("every lane parked after its first try, with no goroutine serving any", lanes, started)
	for {
		armed := rig.clock.Armed()
		next := armed[0]
		if next.Sub(started) > window {
			break
		}
		// Each lane whose park ends by next is tried once more as the fake reaches it.
		due := 0
		for _, at := range armed {
			if !at.After(next) {
				due++
			}
		}
		tried := triesSoFar() + due
		rig.clock.Set(next)
		allParked(fmt.Sprintf("every lane parked again once the fake reached %v, with no goroutine serving any", next.Sub(started)), tried, next)
	}
	elapsed := rig.clock.Now().Sub(started)
	serving, _, _, queued := rig.state()
	triesMu.Lock()
	defer triesMu.Unlock()
	total, most, laneCount := 0, 0, len(tries)
	var short []string
	for lane, times := range tries {
		total += len(times)
		most = max(most, len(times))
		_, laneShort := shortWaits(times)
		for _, wait := range laneShort {
			short = append(short, lane+": "+wait)
		}
	}
	slices.Sort(short)
	bound := triesWithin(elapsed)
	t.Logf("%d lanes parked for %v of the fake: %d tries (%.2f a lane a second at the end's 1 per %v), at most %d on one lane (bound %d); %d goroutines serving, %d sends queued; %d waits shorter than parkedWait",
		lanes, elapsed, total, float64(total)/elapsed.Seconds()/lanes, mentionOrderRetryMax, most, bound, serving, queued, len(short))
	if laneCount != lanes {
		t.Fatalf("%d lanes were tried, want all %d", laneCount, lanes)
	}
	if len(short) != 0 {
		t.Fatalf("%d waits between two tries of one lane are shorter than the cost requirement allows (parkedWait), the first %s",
			len(short), strings.Join(short[:min(5, len(short))], "; "))
	}
	for target, times := range tries {
		if len(times) != bound {
			t.Fatalf("lane %s was tried %d times in %v, want %d (triesWithin, mentionOrderRetry)", target, len(times), elapsed, bound)
		}
	}
	if queued != lanes {
		t.Fatalf("%d sends are queued, want the %d parked ones still among them", queued, lanes)
	}
}
