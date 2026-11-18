package api

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// Every send that waits on a held lane past the warn threshold is reported, once: the one that
// waited for an earlier event of its issue and each send of that issue to the same recipient
// queued behind it - three sends of A-1, three WARN lines, one naming each comment, and no
// second line for any of them however many times the lane parks again.
func TestEverySendHeldPastTheThresholdIsReportedOnce(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	lifetime, stop := context.WithCancel(context.Background())
	sender := newMentionSender(lifetime, time.Hour,
		func(_ context.Context, item mentionSend) (mentionSend, bool) { return item, true },
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) { return queued, nil },
		rereadNone,
		func(context.Context, []mentionClaim) error { return nil },
	)
	sender.mu.Lock()
	sender.heldWarnAfter = 200 * time.Millisecond
	sender.mu.Unlock()
	t.Cleanup(func() {
		stop()
		sender.wait()
	})
	for _, id := range []string{"a-first", "a-second", "a-third"} {
		sender.enqueue(context.Background(), mentionSend{
			comment: model.Comment{ID: id, IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: time.Now(),
		})
	}
	const marker = `msg="dispatch: a comment mention's send waits behind an earlier event of its owner"`
	waitUntil(t, 5*time.Second, "a WARN line for each held send", func() bool {
		return strings.Count(logs.String(), marker) >= 3
	})
	// A second past the threshold the lane has parked again several times.
	time.Sleep(time.Second)
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
// comment, s1 and event 42.
func TestASendWhoseWaitPassesTheThresholdInFlightIsReportedAsItGoes(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	lifetime, stop := context.WithCancel(context.Background())
	retrying, release := make(chan struct{}), make(chan struct{})
	var tries atomic.Int32
	sender := newMentionSender(lifetime, time.Hour,
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
	sender.mu.Lock()
	sender.heldWarnAfter = 200 * time.Millisecond
	sender.mu.Unlock()
	t.Cleanup(func() {
		stop()
		sender.wait()
	})
	for _, id := range []string{"a-first", "a-second"} {
		sender.enqueue(context.Background(), mentionSend{
			comment: model.Comment{ID: id, IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: time.Now(),
		})
	}
	<-retrying
	time.Sleep(300 * time.Millisecond)
	const marker = `msg="dispatch: a comment mention's send waits behind an earlier event of its owner"`
	if got := strings.Count(logs.String(), marker); got != 0 {
		t.Fatalf("%d WARN lines before either send's wait ended, want none: no park saw the threshold pass", got)
	}
	close(release)
	waitUntil(t, 5*time.Second, "A-1's two sends gone", func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return len(sender.held) == 0
	})
	// A send leaves held (claimedDuring) before its goroutine logs its late WARN (reportLate):
	// join that goroutine before reading the log.
	stop()
	sender.wait()
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
	sender := &mentionSender{heldWarnAfter: time.Millisecond, waits: map[laneKey]heldWait{}, mu: new(sync.Mutex)}
	item := mentionSend{
		comment: model.Comment{ID: "late", IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
		heldFor: "s1", heldBy: model.MentionCopyHolder{EventID: 42}, heldSince: time.Now().Add(-time.Minute),
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
// of its sends at once. A-1's first send waits three times, A-1's second waits behind it; each send
// takes 5 ms, so two goroutines on the lane would have both in flight together.
func TestAReparkedHeldLaneHasOneGoroutine(t *testing.T) {
	lifetime, stop := context.WithCancel(context.Background())
	var tries, active, most atomic.Int32
	var mu sync.Mutex
	var sent []string
	sender := newMentionSender(lifetime, time.Hour,
		func(_ context.Context, item mentionSend) (mentionSend, bool) {
			now := active.Add(1)
			defer active.Add(-1)
			for seen := most.Load(); now > seen && !most.CompareAndSwap(seen, now); seen = most.Load() {
			}
			time.Sleep(5 * time.Millisecond)
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
	t.Cleanup(func() {
		stop()
		sender.wait()
	})
	for _, id := range []string{"a-first", "a-second"} {
		sender.enqueue(context.Background(), mentionSend{
			comment: model.Comment{ID: id, IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: time.Now(),
		})
	}
	waitUntil(t, 5*time.Second, "A-1's two sends gone", func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return len(sender.held) == 0
	})
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
// read (parkRig.state, countParks) where a count of goroutines would lag the park.
func TestAParkedLanesTimerStartsOnceNoGoroutineServesIt(t *testing.T) {
	lifetime, stop := context.WithCancel(context.Background())
	var tries atomic.Int32
	sender := newMentionSender(lifetime, time.Hour,
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
	sender.mu.Lock()
	sender.after = func(delay time.Duration, f func()) {
		sender.mu.Lock()
		serving := sender.serving
		sender.mu.Unlock()
		mu.Lock()
		servingAtStart = append(servingAtStart, serving)
		mu.Unlock()
		// The timer runs once no goroutine serves any lane, so the next park's start reads the
		// serve loop's own order and not how fast a goroutine left a lane it no longer serves.
		go func() {
			for {
				sender.mu.Lock()
				idle := sender.serving == 0
				sender.mu.Unlock()
				if idle {
					break
				}
				time.Sleep(time.Millisecond)
			}
			time.AfterFunc(delay, f)
		}()
	}
	sender.mu.Unlock()
	t.Cleanup(func() {
		stop()
		sender.wait()
	})
	for _, id := range []string{"a-first", "a-second"} {
		sender.enqueue(context.Background(), mentionSend{
			comment: model.Comment{ID: id, IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
			attempt: 1, claimed: time.Now(),
		})
	}
	waitUntil(t, 5*time.Second, "A-1's two sends gone", func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return len(sender.held) == 0
	})
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
			sender := newMentionSender(lifetime, time.Hour,
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
			t.Cleanup(func() {
				stop()
				sender.wait()
			})
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

			parked := false
			select {
			case <-tried:
			case <-timers:
				parked = true
			case <-time.After(10 * time.Second):
				t.Fatal("the send neither went nor parked")
			}
			if parked != tc.readFail {
				t.Fatalf("the send parked: %t, want %t", parked, tc.readFail)
			}
			count, _ := sender.heldSamples(time.Now())
			var held int64
			for _, sample := range count {
				held += sample.Value
			}
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

// A send parked only because its unknown claim could not be read waited behind no event, so when
// it goes, past the warn threshold, it logs no WARN that it waited behind an earlier event: such a
// WARN would name session "" and event 0, which does not exist. TestTheMentionSenderClaimModel
// finds it as a held send whose late WARN names event 0. The control row's read answers at once.
//
// A second send of the owner that joins the lane that park made is no held copy either while the
// lane waits behind no event (queueLocked stamps it only behind one): when both then go, past the
// threshold, neither logs a WARN. When the head's try then waits behind event 42, for more than a
// second, that park stamps the send that joined unstamped (park), so each WARN as they go names
// event 42 and a wait of at least a second and no longer than the test has run: never the age of
// a send stamped at no time, nor a wait measured backwards.
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
			began := time.Now()
			logs := &storetest.LockedLog{}
			logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
			lifetime, stop := context.WithCancel(context.Background())
			tried := make(chan mentionKey, 16)
			var fails, headTries atomic.Int32
			if tc.readFail {
				fails.Store(1)
			}
			sender := newMentionSender(lifetime, time.Hour,
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
			timers := make(chan func(), 4)
			sender.mu.Lock()
			sender.heldWarnAfter = 30 * time.Millisecond
			sender.after = func(_ time.Duration, f func()) { timers <- f }
			// No lane goroutine may take the send before its claim is unknown.
			sender.serving = mentionSendLanes
			sender.mu.Unlock()
			t.Cleanup(func() {
				stop()
				sender.wait()
			})
			item := mentionSend{
				comment: model.Comment{ID: "a-first", IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
				attempt: 1, claimed: time.Now(),
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
			// Each park's timer is fired once the wait is past the threshold; the run ends when the
			// sender holds no send. The deadline is five seconds of room beside the 1.2 s the head's
			// wait behind event 42 sleeps.
			for deadline := time.After(6200 * time.Millisecond); ; {
				sender.mu.Lock()
				done := len(sender.held) == 0
				sender.mu.Unlock()
				if done {
					break
				}
				select {
				case fire := <-timers:
					if tc.second && !joined {
						// The lane the failed read parked holds a-first and no wait: a-second joins it.
						joined = true
						sender.enqueue(context.Background(), mentionSend{
							comment: model.Comment{ID: "a-second", IssueKey: new("A-1")}, mention: ResolvedMention{Target: "session:s1"},
							attempt: 1, claimed: time.Now(),
						})
					}
					// The outage lasts past the threshold, then the read answers. The wait behind
					// event 42, once the head's try has met it, lasts more than a second, so a wait
					// read backwards is a negative one at slog's rounding.
					wait := 60 * time.Millisecond
					if headTries.Load() > 0 {
						wait = 1200 * time.Millisecond
					}
					time.Sleep(wait)
					fire()
				case <-tried:
				case <-time.After(10 * time.Millisecond):
				case <-deadline:
					t.Fatal("the sends neither went nor parked")
				}
			}
			if tc.second && !joined {
				t.Fatal("the failed read parked no lane for a-second to join")
			}
			// reportLate runs after a send leaves held, on the goroutine that served it: join that
			// goroutine before reading the log.
			stop()
			sender.wait()
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
			// Each send waited behind event 42 at least the second and more the test slept, and
			// no longer than this row has run, however slow the box: a send stamped at no time
			// would report the age of the zero time instead. Each field is read as the line's own
			// (horizontest.Field), so a quoted value that holds "event_id=42" or "held_for=0s"
			// cannot stand in for it.
			ran := time.Since(began).Round(time.Second) + time.Second
			second := false
			for _, line := range lines {
				event, _ := horizontest.Field(line, "event_id")
				value, found := horizontest.Field(line, "held_for")
				waited, err := time.ParseDuration(value)
				if event != "42" || !found || err != nil || waited < time.Second || waited > ran {
					t.Fatalf("a WARN of a send that waited behind event 42 for between 1s and %v names another wait: %s", ran, line)
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
