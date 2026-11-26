package api

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// warnClock is the fake a held-WARN row's sender runs on: its parks fire, and its held sends age,
// only as the test moves it.
func warnClock() *clocktest.Fake {
	return clocktest.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
}

// Every send that waits on a held lane past the warn threshold is reported, once: the one that
// waited for an earlier event of its issue and each send of that issue to the same recipient
// queued behind it - three sends of A-1, three WARN lines, one naming each comment, and no
// second line for any of them however many times the lane parks again. The sender runs on a fake
// the test moves to each park's deadline in turn: three parks put the sends past the threshold,
// and five more park the lane again past it.
func TestEverySendHeldPastTheThresholdIsReportedOnce(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	lifetime, stop := context.WithCancel(context.Background())
	sender := newMentionSender(lifetime, stagedAhead, nil,
		func(_ context.Context, item mentionSend) (mentionSend, bool) { return item, true },
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) { return queued, nil },
		rereadNone,
		func(context.Context, []mentionClaim) error { return nil },
	)
	fake := warnClock()
	sender.mu.Lock()
	sender.heldWarnAfter = 200 * time.Millisecond
	sender.clock = fake
	sender.mu.Unlock()
	t.Cleanup(func() { endMentionSender(t, stop, sender) })
	for _, id := range []string{"a-first", "a-second", "a-third"} {
		sender.enqueue(context.Background(), mentionSend{
			comment: model.Comment{ID: id, IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: fake.Now(),
		})
	}
	// parkAgain moves the fake to the lane's pending park and waits for the park its try makes:
	// each park logs its WARNs before it arms its timer.
	parkAgain := func(park int) {
		t.Helper()
		fake.Set(fake.AwaitArmed(t, fmt.Sprintf("park %d of A-1's held lane", park), 1)[0])
	}
	const marker = `msg="dispatch: a comment mention's send waits behind an earlier event of its owner"`
	// Parks of 50, 100 and 200 ms: the fourth park, at 350 ms, is past the threshold.
	for park := 1; park <= 3; park++ {
		parkAgain(park)
	}
	fake.AwaitArmed(t, "park 4 of A-1's held lane", 1)
	if got := strings.Count(logs.String(), marker); got != 3 {
		t.Fatalf("%d WARN lines once the lane parked past the threshold, want three", got)
	}
	for park := 4; park <= 8; park++ {
		parkAgain(park)
	}
	fake.AwaitArmed(t, "park 9 of A-1's held lane", 1)
	logged := logs.String()
	t.Logf("WARN lines: %d", strings.Count(logged, marker))
	if got := strings.Count(logged, marker); got != 3 {
		t.Fatalf("%d WARN lines for three held sends, want three, one each", got)
	}
	for _, id := range []string{"a-first", "a-second", "a-third"} {
		if strings.Count(logged, "comment="+id+" ") != 1 {
			t.Fatalf("the WARN lines name %s other than once:\n%s", id, logged)
		}
	}
}

// A send whose wait passes the threshold while no park can see it is reported as its wait ends.
// A-1's first send waits behind event 42 for s1 and parks, moving A-1's second send behind it; its
// retry starts before the threshold and is in flight past it, then goes, and the second goes after
// it. Neither parks again, yet each waited past the threshold: each logs one WARN naming its own
// comment, s1 and event 42. The sender runs on a fake: the test fires the park and moves the fake
// past the threshold while the retry is in flight.
func TestASendWhoseWaitPassesTheThresholdInFlightIsReportedAsItGoes(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	lifetime, stop := context.WithCancel(context.Background())
	retrying, release := make(chan struct{}), make(chan struct{})
	var tries atomic.Int32
	sender := newMentionSender(lifetime, stagedAhead, nil,
		func(_ context.Context, item mentionSend) (mentionSend, bool) {
			if item.comment.ID == "a-first" {
				if tries.Add(1) == 1 {
					item.heldFor, item.heldBy = "s1", model.MentionCopyHolder{EventID: 42}
					return item, true
				}
				close(retrying)
				<-release
			}
			return mentionSend{}, false
		},
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) { return queued, nil },
		rereadNone,
		func(context.Context, []mentionClaim) error { return nil },
	)
	fake := warnClock()
	sender.mu.Lock()
	sender.heldWarnAfter = 200 * time.Millisecond
	sender.clock = fake
	sender.mu.Unlock()
	t.Cleanup(func() { endMentionSender(t, stop, sender) })
	// a-first's retry parks on release until the row lets it go, and the sender's wait above
	// returns only once it has. Cleanups run last first, so this one, registered after that wait,
	// lets the retry go before it: a row that fails between the retry's start and the release
	// below fails by name rather than hang in sender.wait.
	letRetryGo := sync.OnceFunc(func() { close(release) })
	t.Cleanup(letRetryGo)
	for _, id := range []string{"a-first", "a-second"} {
		sender.enqueue(context.Background(), mentionSend{
			comment: model.Comment{ID: id, IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: fake.Now(),
		})
	}
	// The park's 50 ms is under the threshold; the retry is then in flight 300 ms more.
	fake.Set(fake.AwaitArmed(t, "a-first's park behind event 42", 1)[0])
	testwait.Receive(t, "a-first's retry in flight", retrying)
	fake.Advance(300 * time.Millisecond)
	const marker = `msg="dispatch: a comment mention's send waits behind an earlier event of its owner"`
	// A WARN is logged only by a park or as a send leaves its lane (park, reportLate), and neither
	// can run while the lane's one try is held in flight.
	if got := strings.Count(logs.String(), marker); got != 0 {
		t.Fatalf("%d WARN lines before either send's wait ended, want none: no park saw the threshold pass", got)
	}
	letRetryGo()
	awaitA1SendsGone(t, sender)
	// A send leaves held (claimedDuring) before its goroutine logs its late WARN (reportLate):
	// join that goroutine before reading the log.
	stop()
	testwait.Returns(t, "the mention sender's lanes, their late WARNs logged, and its claim refresh to return once its lifetime ended", sender.wait)
	logged := logs.String()
	t.Logf("WARN lines: %d", strings.Count(logged, marker))
	for _, id := range []string{"a-first", "a-second"} {
		lines := 0
		for _, line := range strings.Split(logged, "\n") {
			if strings.Contains(line, marker) && strings.Contains(line, "comment="+id+" ") {
				lines++
				if !strings.Contains(line, "session=s1 ") || !strings.Contains(line, "event_id=42 ") {
					t.Fatalf("the WARN line of %s does not name s1 and event 42: %s", id, line)
				}
			}
		}
		if lines != 1 {
			t.Fatalf("%d WARN lines name %s, which waited past the threshold, want one:\n%s", lines, id, logged)
		}
	}
}

// A send that leaves a held lane past the warn threshold with no WARN logged for it names, in the
// WARN it logs as it goes, what its lane waits behind (reportLate) - or, when the lane has no wait
// recorded, what the send itself last waited behind: here s1 and event 42, where a read of the
// lane alone would log an empty session and event 0.
func TestALateReportWithNoWaitOnItsLaneNamesTheSendsOwnWait(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	fake := warnClock()
	sender := &mentionSender{heldWarnAfter: time.Millisecond, clock: fake, waits: map[laneKey]heldWait{}, mu: new(sync.Mutex)}
	item := mentionSend{
		comment: model.Comment{ID: "late", IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
		heldFor: "s1", heldBy: model.MentionCopyHolder{EventID: 42}, heldSince: fake.Now().Add(-time.Minute),
	}
	sender.reportLate(heldLane(item.lane(), item.owner()), item)
	logged := logs.String()
	t.Logf("logged: %s", logged)
	if strings.Count(logged, "comment=late ") != 1 || !strings.Contains(logged, "session=s1 ") || !strings.Contains(logged, "event_id=42 ") {
		t.Fatalf("the late WARN does not name the send's own wait, s1 and event 42, once:\n%s", logged)
	}
}

// A held lane whose head waits again is parked by the goroutine that served it, which gives it up
// before the lane's timer starts: the timer, run here the moment it is started (after), starts
// one goroutine on the lane only once the first has let it go, and no two goroutines ever send two
// of its sends at once. A-1's first send waits three times, A-1's second waits behind it. Each send
// is held in flight until the test has read that it is the only one: a second goroutine on the lane
// would be in flight beside it, and one goroutine more serving a lane.
func TestAReparkedHeldLaneHasOneGoroutine(t *testing.T) {
	t.Parallel()
	lifetime, stop := context.WithCancel(context.Background())
	var tries, active, most atomic.Int32
	var mu sync.Mutex
	var sent []string
	entered, proceed, abort := make(chan string, 8), make(chan struct{}), make(chan struct{})
	sender := newMentionSender(lifetime, stagedAhead, nil,
		func(_ context.Context, item mentionSend) (mentionSend, bool) {
			now := active.Add(1)
			defer active.Add(-1)
			for seen := most.Load(); now > seen && !most.CompareAndSwap(seen, now); seen = most.Load() {
			}
			entered <- item.comment.ID
			select {
			case <-proceed:
			case <-abort:
			}
			if item.comment.ID == "a-first" && tries.Add(1) <= 3 {
				item.heldFor, item.heldBy = "s1", model.MentionCopyHolder{EventID: 7}
				return item, true
			}
			mu.Lock()
			defer mu.Unlock()
			sent = append(sent, item.comment.ID)
			return mentionSend{}, false
		},
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) { return queued, nil },
		rereadNone,
		func(context.Context, []mentionClaim) error { return nil },
	)
	sender.mu.Lock()
	sender.after = func(_ time.Duration, f func()) { f() }
	sender.mu.Unlock()
	t.Cleanup(func() { endMentionSender(t, stop, sender) })
	// Runs before the sender's wait: a row that failed while a send was held lets it go.
	t.Cleanup(func() { close(abort) })
	for _, id := range []string{"a-first", "a-second"} {
		sender.enqueue(context.Background(), mentionSend{
			comment: model.Comment{ID: id, IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: time.Now(),
		})
	}
	// a-first's three waits, its send, then a-second's.
	for index := range 5 {
		id := testwait.Receive(t, fmt.Sprintf("send %d of A-1 in flight", index+1), entered)
		// The goroutine that parked the lane leaves the lane it served (next) a moment after the
		// one the timer started has taken the held lane: the count settles at one, and a second
		// goroutine on the held lane would be a second send in flight, which ends the wait.
		testwait.Until(t, fmt.Sprintf("one goroutine serving lanes while %s is in flight", id), func() (string, bool) {
			if in := active.Load(); in > 1 {
				t.Fatalf("%d sends of one lane are in flight at once while %s is held, want one: two goroutines serve the held lane", in, id)
			}
			sender.mu.Lock()
			serving := sender.serving
			sender.mu.Unlock()
			return fmt.Sprintf("%d goroutines serving", serving), serving == 1
		})
		proceed <- struct{}{}
	}
	awaitA1SendsGone(t, sender)
	mu.Lock()
	defer mu.Unlock()
	t.Logf("sent %v; at most %d sends in flight at once", sent, most.Load())
	if most.Load() != 1 {
		t.Fatalf("%d sends of one lane were in flight at once, want one: two goroutines served the held lane", most.Load())
	}
	if !slices.Equal(sent, []string{"a-first", "a-second"}) {
		t.Fatalf("sent %v, want A-1's first, then its second, each once", sent)
	}
}

// A park's timer starts only once no goroutine serves the parked lane. A-1's first send to s1
// waits three times and its second waits behind it: the first park makes A-1's held lane beside
// s1's lane, whose goroutine may still be on s1's lane; the next two are the held lane's own head
// waiting again, and the goroutine on that lane gives it up (next) before its timer starts, so at
// each of their starts no goroutine serves any lane, the held lane being the only one with a
// send. That is why a parked lane's pending timer says no goroutine is on it, which the park tests
// read (parkRig.state, countParks) where a count of goroutines would lag the park. Each timer is
// handed to the test, which runs it once no goroutine serves any lane, so the next park's start
// reads the serve loop's own order and not how fast a goroutine left a lane it no longer serves.
func TestAParkedLanesTimerStartsOnceNoGoroutineServesIt(t *testing.T) {
	t.Parallel()
	lifetime, stop := context.WithCancel(context.Background())
	var tries atomic.Int32
	sender := newMentionSender(lifetime, stagedAhead, nil,
		func(_ context.Context, item mentionSend) (mentionSend, bool) {
			if item.comment.ID == "a-first" && tries.Add(1) <= 3 {
				item.heldFor, item.heldBy = "s1", model.MentionCopyHolder{EventID: 7}
				return item, true
			}
			return mentionSend{}, false
		},
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) { return queued, nil },
		rereadNone,
		func(context.Context, []mentionClaim) error { return nil },
	)
	var mu sync.Mutex
	var servingAtStart []int
	timers := make(chan func(), 4)
	sender.mu.Lock()
	sender.after = func(_ time.Duration, f func()) {
		sender.mu.Lock()
		serving := sender.serving
		sender.mu.Unlock()
		mu.Lock()
		servingAtStart = append(servingAtStart, serving)
		mu.Unlock()
		timers <- f
	}
	sender.mu.Unlock()
	t.Cleanup(func() { endMentionSender(t, stop, sender) })
	for _, id := range []string{"a-first", "a-second"} {
		sender.enqueue(context.Background(), mentionSend{
			comment: model.Comment{ID: id, IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: time.Now(),
		})
	}
	for park := 1; park <= 3; park++ {
		fire := testwait.Receive(t, fmt.Sprintf("park %d's timer started", park), timers)
		testwait.Eventually(t, fmt.Sprintf("no goroutine serving a lane before park %d's timer runs", park), func() bool {
			sender.mu.Lock()
			defer sender.mu.Unlock()
			return sender.serving == 0
		})
		fire()
	}
	awaitA1SendsGone(t, sender)
	mu.Lock()
	defer mu.Unlock()
	t.Logf("goroutines serving lanes as each park's timer started: %v", servingAtStart)
	if len(servingAtStart) != 3 {
		t.Fatalf("%d park timers started, want three, one per wait of A-1's first send", len(servingAtStart))
	}
	if servingAtStart[1] != 0 || servingAtStart[2] != 0 {
		t.Fatalf("goroutines serving lanes as the held lane's own parks started their timers: %v, want none for the second and third: the lane's goroutine starts its timer before it gives the lane up", servingAtStart)
	}
}

// A send whose claim turned unknown - a refresh's answer was lost - and whose claim read then fails
// makes no try and parks as a send that waited does, but it waited behind no earlier event: it is
// no held copy, so the held gauges count nothing of it and no WARN names an earlier event of its
// owner, which would be event 0. The control row's read answers, and the send is tried.
func TestASendParkedForAFailedClaimReadIsNoHeldCopy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readFail bool
	}{
		{name: "control: the claim read answers"},
		{name: "the claim read fails", readFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &storetest.LockedLog{}
			logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
			lifetime, stop := context.WithCancel(context.Background())
			tried := make(chan mentionKey, 4)
			sender := newMentionSender(lifetime, stagedAhead, nil,
				func(_ context.Context, item mentionSend) (mentionSend, bool) {
					tried <- item.key()
					return mentionSend{}, false
				},
				func(context.Context, []mentionClaim) ([]mentionClaim, error) { return nil, errLostAfterCommit },
				func(_ context.Context, named []mentionClaim) ([]mentionClaim, error) {
					if tc.readFail {
						return nil, errDatabaseUnreachable
					}
					return named, nil
				},
				func(context.Context, []mentionClaim) error { return nil },
			)
			timers := make(chan func(), 4)
			sender.mu.Lock()
			sender.heldWarnAfter = 0
			sender.after = func(_ time.Duration, f func()) { timers <- f }
			// Every goroutine the sender may start counts as busy, so the send waits ready on its
			// lane until its claim is unknown.
			sender.serving = mentionSendLanes
			sender.mu.Unlock()
			t.Cleanup(func() { endMentionSender(t, stop, sender) })
			item := mentionSend{
				comment: model.Comment{ID: "a-first", IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
				attempt: 1, claimed: time.Now(),
			}
			sender.enqueue(context.Background(), item)
			sender.refreshHeldClaims(context.Background())
			sender.mu.Lock()
			sender.serving = 0
			sender.mu.Unlock()
			sender.unpark(item.lane())

			// A try that finds neither reads the send's lane and the goroutines serving lanes.
			var parked bool
			testwait.Until(t, "the send to go or park", func() (string, bool) {
				select {
				case <-tried:
					return "tried", true
				case <-timers:
					parked = true
					return "parked", true
				default:
				}
				sender.mu.Lock()
				defer sender.mu.Unlock()
				return fmt.Sprintf("no try and no park's timer; %d sends wait on the send's lane, and %d goroutines serve lanes",
					len(sender.lanes[item.lane()]), sender.serving), false
			})
			if parked != tc.readFail {
				t.Fatalf("the send parked: %t, want %t", parked, tc.readFail)
			}
			held, _ := sender.heldSamples(sender.sendClock().Now())
			const marker = `msg="dispatch: a comment mention's send waits behind an earlier event of its owner"`
			logged := logs.String()
			warns := strings.Count(logged, marker)
			t.Logf("parked %t: the held gauge reads %d, and %d WARN lines name an earlier event", parked, held, warns)
			if held != 0 || warns != 0 {
				t.Fatalf("a send no earlier event holds counts %d in the held gauge, with %d WARN lines naming an earlier event it does not wait behind:\n%s",
					held, warns, logged)
			}
		})
	}
}

// heldWaitBehindEvent42 is how long TestASendParkedForAFailedClaimReadLogsNoHeldWarnAsItGoes moves
// its fake on, once A-1's head has waited behind event 42, before it fires that park: more than a
// second, so a wait read backwards is a negative one at slog's rounding.
const heldWaitBehindEvent42 = 1200 * time.Millisecond

// parkOrDone is what the next step of TestASendParkedForAFailedClaimReadLogsNoHeldWarnAsItGoes
// found: a park's timer to fire, or the sender holding no send.
type parkOrDone struct {
	fire func()
	done bool
}

// A send parked only because its unknown claim could not be read waited behind no event, so when
// it goes, past the warn threshold, it logs no WARN that it waited behind an earlier event: such a
// WARN would name session "" and event 0, which does not exist. TestTheMentionSenderClaimModel
// finds it as a held send whose late WARN names event 0. The control row's read answers at once.
//
// A second send of the owner that joins the lane that park made is no held copy either while the
// lane waits behind no event (queueLocked stamps it only behind one): when both then go, past the
// threshold, neither logs a WARN. When the head's try then waits behind event 42, that park stamps
// the send that joined unstamped (park), so each WARN as they go names event 42 and the wait the
// test moved the sender's fake on since that park, heldWaitBehindEvent42 at slog's rounding: never
// the age of a send stamped at no time, nor a wait measured backwards.
func TestASendParkedForAFailedClaimReadLogsNoHeldWarnAsItGoes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readFail bool
		// second joins a-second to the lane the failed read parked, before the read answers.
		second bool
		// headWaits makes a-first's first try wait behind event 42 for s1.
		headWaits bool
	}{
		{name: "control: the claim read answers"},
		{name: "the claim read fails once, past the threshold", readFail: true},
		{name: "a second send joins the lane the failed read parked, and both go past the threshold", readFail: true, second: true},
		{name: "a second send joins the lane the failed read parked, and the head then waits behind event 42", readFail: true, second: true, headWaits: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &storetest.LockedLog{}
			logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
			lifetime, stop := context.WithCancel(context.Background())
			tried := make(chan mentionKey, 16)
			var fails, headTries atomic.Int32
			if tc.readFail {
				fails.Store(1)
			}
			sender := newMentionSender(lifetime, stagedAhead, nil,
				func(_ context.Context, item mentionSend) (mentionSend, bool) {
					tried <- item.key()
					if tc.headWaits && item.comment.ID == "a-first" && headTries.Add(1) == 1 {
						item.heldFor, item.heldBy = "s1", model.MentionCopyHolder{EventID: 42}
						return item, true
					}
					return mentionSend{}, false
				},
				func(context.Context, []mentionClaim) ([]mentionClaim, error) { return nil, errLostAfterCommit },
				func(_ context.Context, named []mentionClaim) ([]mentionClaim, error) {
					if fails.Add(-1) >= 0 {
						return nil, errDatabaseUnreachable
					}
					return named, nil
				},
				func(context.Context, []mentionClaim) error { return nil },
			)
			fake := warnClock()
			timers := make(chan func(), 4)
			sender.mu.Lock()
			sender.heldWarnAfter = 30 * time.Millisecond
			sender.clock = fake
			sender.after = func(_ time.Duration, f func()) { timers <- f }
			// No lane goroutine may take the send before its claim is unknown.
			sender.serving = mentionSendLanes
			sender.mu.Unlock()
			t.Cleanup(func() { endMentionSender(t, stop, sender) })
			item := mentionSend{
				comment: model.Comment{ID: "a-first", IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
				attempt: 1, claimed: fake.Now(),
			}
			sender.enqueue(context.Background(), item)
			// A refresh lost its answer naming this claim, which is therefore unknown
			// (refreshHeldClaims); set here so that the refresh's own re-read spends no failure.
			sender.mu.Lock()
			state := sender.held[item.key()]
			state.unsure, state.lost = true, state.lost+1
			sender.held[item.key()] = state
			sender.serving = 0
			sender.mu.Unlock()
			sender.unpark(item.lane())
			joined := false
			// Each park's timer is fired once the fake is past the threshold; the run ends when the
			// sender holds no send. A try that finds neither reads the sends the sender holds.
			for {
				var next parkOrDone
				testwait.Until(t, "a park's timer, or the sender holding no send", func() (string, bool) {
					for len(tried) > 0 {
						<-tried
					}
					select {
					case fire := <-timers:
						next = parkOrDone{fire: fire}
						return "a park's timer", true
					default:
					}
					sender.mu.Lock()
					defer sender.mu.Unlock()
					if len(sender.held) == 0 {
						next = parkOrDone{done: true}
						return "the sender holds no send", true
					}
					keys := make([]string, 0, len(sender.held))
					for key := range sender.held {
						keys = append(keys, fmt.Sprintf("%+v", key))
					}
					slices.Sort(keys)
					return fmt.Sprintf("no park's timer; the sender holds %d sends: %v", len(sender.held), keys), false
				})
				if next.done {
					break
				}
				if tc.second && !joined {
					// The lane the failed read parked holds a-first and no wait: a-second joins it.
					joined = true
					sender.enqueue(context.Background(), mentionSend{
						comment: model.Comment{ID: "a-second", IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
						attempt: 1, claimed: fake.Now(),
					})
				}
				// The outage lasts past the threshold, then the read answers. The wait behind
				// event 42, once the head's try has met it, is heldWaitBehindEvent42.
				wait := 60 * time.Millisecond
				if headTries.Load() > 0 {
					wait = heldWaitBehindEvent42
				}
				fake.Advance(wait)
				next.fire()
			}
			if tc.second && !joined {
				t.Fatal("the failed read parked no lane for a-second to join")
			}
			// reportLate runs after a send leaves held, on the goroutine that served it: join that
			// goroutine before reading the log.
			stop()
			testwait.Returns(t, "the mention sender's lanes, their late WARNs logged, and its claim refresh to return once its lifetime ended", sender.wait)
			const marker = `msg="dispatch: a comment mention's send waits behind an earlier event of its owner"`
			var lines []string
			for l := range strings.SplitSeq(logs.String(), "\n") {
				if strings.Contains(l, marker) {
					lines = append(lines, l)
				}
			}
			t.Logf("held WARN lines: %q", lines)
			if !tc.headWaits {
				if len(lines) != 0 {
					t.Fatalf("a send that waited behind no event logged the held WARN as it went: %s", lines[0])
				}
				return
			}
			// Each send waited behind event 42 exactly the span the test moved the fake on since
			// the park that stamped it: a send stamped at no time would report the age of the zero
			// time instead. Each field is read as the line's own (horizontest.Field), so a quoted
			// value that holds "event_id=42" or "held_for=0s" cannot stand in for it.
			want := heldWaitBehindEvent42.Round(time.Second)
			second := false
			for _, line := range lines {
				event, _ := horizontest.Field(line, "event_id")
				value, found := horizontest.Field(line, "held_for")
				waited, err := time.ParseDuration(value)
				if event != "42" || !found || err != nil || waited != want {
					t.Fatalf("a WARN of a send that waited behind event 42 for %v names another wait: %s", want, line)
				}
				comment, _ := horizontest.Field(line, "comment")
				second = second || comment == "a-second"
			}
			if !second {
				t.Fatalf("a-second, which waited behind event 42 past the threshold, logged no WARN: %q", lines)
			}
		})
	}
}

// awaitA1SendsGone waits through the shared wait until sender holds no send (mentionSender.held):
// both of A-1's sends have left it.
func awaitA1SendsGone(t *testing.T, sender *mentionSender) {
	t.Helper()
	testwait.Eventually(t, "A-1's two sends gone", func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return len(sender.held) == 0
	})
}
