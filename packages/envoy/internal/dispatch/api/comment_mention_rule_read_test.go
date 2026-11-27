package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// A held send whose whole delivery cannot read the copy rule waits as a send behind an earlier
// event does, behind none: it keeps its place at the head of its held lane and the claim its try
// took, which the sender goes on writing. Two comments to s1 wait behind the issue's earlier
// events on one held lane, and the events are then published. The first's try reads the rule
// alone, which lets it go, and the read its whole delivery makes once it has taken the attempt's
// claim fails once (ruleReadFailer). The second must not reach s1 before the first, and the
// first's claim must stay the sender's: recorded as its row carries it, and written again by the
// next refresh. The send then goes, and s1 takes the two in order. A failed read that ended the
// send instead would give its claim up to the delivery sweep, a lease later, and let the second
// go first.
func TestASendWhoseRuleReadFailsKeepsItsPlaceAndItsClaim(t *testing.T) {
	t.Parallel()
	failer := &ruleReadFailer{}
	listener, state := newMentionListener(t)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		envoyURL: listener.URL, envoyTimeout: 30 * time.Second, outboxPublishes: true, tracer: failer,
	})
	served.mentions.wait()
	sender := newMentionSender(served.lifetime, time.Hour, served.sendCommentMention,
		served.refreshMentionClaims, served.rereadMentionClaims, served.releaseMentionClaims)
	timers := make(chan func(), 4)
	sender.mu.Lock()
	sender.after = func(_ time.Duration, f func()) { timers <- f }
	sender.mu.Unlock()
	served.mentions = sender

	// No outbox runs, so the issue's earlier events stay unsent: the first comment's send parks
	// behind them, its timer left to the test, and the second queues behind it on the held lane.
	issue := createInteractionIssue(t, handler, "TEST", "A failed read of the copy rule", "before")
	first := postMentionOfS1(t, handler, issue.Key, "the first")
	unpark := <-timers
	second := postMentionOfS1(t, handler, issue.Key, "the second")
	held := heldLane(laneKey{recipient: "session:s1"}, issue.Key)
	waitUntil(t, "both sends on the issue's held lane", func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return len(sender.lanes[held]) == 2
	})

	publishIssueEvents(t, database, issue.Key)
	failer.stage.Store(ruleReadArmed)
	unpark()
	var parked func()
	waitUntil(t, "the first send parked again, or a send reaching s1", func() bool {
		select {
		case parked = <-timers:
			return true
		default:
			return len(state.bodiesTo("s1")) > 0
		}
	})
	key := mentionKey{comment: first, target: "session:s1", attempt: 1}
	recorded, unsure := recordedClaim(sender, key)
	row := attemptClaimedAt(t, database, first, "session:s1")
	t.Logf("failed rule reads %d; s1 took %q; the first's claim: recorded %v (unsure %t), row %v",
		failer.failed.Load(), state.bodiesTo("s1"), recorded, unsure, row)
	if failer.failed.Load() != 1 {
		t.Fatalf("%d reads of the rule failed, want the one after the first's take", failer.failed.Load())
	}
	if bodies := state.bodiesTo("s1"); len(bodies) != 0 {
		t.Fatalf("s1 took %q while the first send's read of the rule had failed, want nothing: the second passed the first", bodies)
	}
	if recorded.IsZero() || unsure || !recorded.Equal(row) {
		t.Fatalf("the sender records the first's claim as %v (unsure %t) while its row carries %v, want the claim its try took, held",
			recorded, unsure, row)
	}
	sender.refreshHeldClaims(context.Background())
	again := attemptClaimedAt(t, database, first, "session:s1")
	if refreshed, _ := recordedClaim(sender, key); !again.After(row) || !refreshed.Equal(again) {
		t.Fatalf("after a refresh the first's row carries %v (was %v) and the sender records %v, want the claim written again and recorded",
			again, row, refreshed)
	}

	parked()
	settledCommentDeliveries(t, database, second)
	states := attemptStates(t, database, "comment_deliveries", "comment_id", first)
	bodies := state.bodiesTo("s1")
	t.Logf("the first's attempts %v; s1 took %q", states, bodies)
	if !slices.Equal(states, []string{"1:sent"}) || !slices.Equal(bodies, []string{"the first", "the second"}) {
		t.Fatalf("the first's attempts are %v and s1 took %q, want the first sent, before the second", states, bodies)
	}
}

// A held send whose reads of the copy rule keep failing waits behind no event, and each of its
// later tries reads the rule alone, once, with no claim and no write: so N such lanes read the
// rule at most N times each mentionOrderRetryMax, as N lanes held behind an event do. That holds
// whether the send waited behind an earlier event before its reads began to fail, or its very
// first read failed, so that it never waited behind one. Each row lets the first comment's send to
// s1 park, with the second comment's queued behind it on the issue's held lane, makes the next four
// tries with every read of the rule failing (ruleReadsFail), then lets the reads answer. Through
// the four, each try reads the rule once and makes no statement on comment_deliveries, s1 takes
// nothing, and the sender records the first's claim as its row carries it. Once a read answers,
// s1 takes the first and then the second. The control is the code before, under which each try
// after a failed read makes the whole delivery: it takes and writes the attempt's claim and
// records its resolution.
//
// A send whose first read failed waited behind no event, so it is no held copy: with the warn
// threshold shortened to a millisecond, the held gauges count neither send at any point and no
// WARN names an earlier event, as it waits or as it goes. The row whose send waited behind the
// issue's events first is the control in the same run: the gauges count its two sends, and the
// WARN of each is logged as it goes. Parking the failed first read as a wait behind an event (in
// serve, park's waited without `&& !again.unread`) makes the second row fail on the gauges.
func TestASendWhoseRuleReadsKeepFailingReadsTheRuleAloneUntilOneAnswers(t *testing.T) {
	// Not parallel: it swaps the default logger (logtest.SetDefault).
	const warning = `msg="dispatch: a comment mention's send waits behind an earlier event of its owner"`
	for _, row := range []struct {
		name string
		// firstReadFails is that the first send's very first read of the rule fails, so it never
		// waits behind an earlier event; otherwise it waits behind the issue's unsent events, and
		// its reads fail only once those are sent.
		firstReadFails bool
	}{
		{name: "it waited behind an earlier event before its reads failed"},
		{name: "its first read of the rule failed", firstReadFails: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			logs := &storetest.LockedLog{}
			logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
			failer := &ruleReadsFail{}
			listener, state := newMentionListener(t)
			handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
				envoyURL: listener.URL, envoyTimeout: 30 * time.Second, outboxPublishes: true, tracer: failer,
			})
			served.mentions.wait()
			sender := newMentionSender(served.lifetime, time.Hour, served.sendCommentMention,
				served.refreshMentionClaims, served.rereadMentionClaims, served.releaseMentionClaims)
			timers := make(chan func(), 4)
			sender.mu.Lock()
			sender.after = func(_ time.Duration, f func()) { timers <- f }
			sender.heldWarnAfter = time.Millisecond
			sender.mu.Unlock()
			served.mentions = sender
			// heldCopies is what the held gauges must read while the first send waits: its two
			// sends when it waited behind the issue's events, none when its only wait is a read.
			heldCopies := int64(2)
			if row.firstReadFails {
				heldCopies = 0
			}
			gauged := func(when string) {
				t.Helper()
				if count, _ := sender.heldSamples(time.Now()); count != heldCopies {
					t.Fatalf("%s the held gauge reads %d, want %d", when, count, heldCopies)
				}
			}

			// No outbox runs, so the issue's events stay unsent until the test publishes them.
			issue := createInteractionIssue(t, handler, "TEST", "Reads of the copy rule that keep failing", "before")
			failer.failing.Store(row.firstReadFails)
			first := postMentionOfS1(t, handler, issue.Key, "the first")
			unpark := <-timers
			second := postMentionOfS1(t, handler, issue.Key, "the second")
			held := heldLane(laneKey{recipient: "session:s1"}, issue.Key)
			waitUntil(t, "both sends on the issue's held lane", func() bool {
				sender.mu.Lock()
				defer sender.mu.Unlock()
				return len(sender.lanes[held]) == 2
			})
			gauged("once the first send parked,")
			publishIssueEvents(t, database, issue.Key)
			failer.failing.Store(true)
			key := mentionKey{comment: first, target: "session:s1", attempt: 1}
			claimed := attemptClaimedAt(t, database, first, "session:s1")

			for try := 1; try <= 4; try++ {
				failer.take()
				unpark()
				waitUntil(t, fmt.Sprintf("try %d: the first send parked again, or a send reaching s1", try), func() bool {
					select {
					case unpark = <-timers:
						return true
					default:
						return len(state.bodiesTo("s1")) > 0
					}
				})
				reads, statements, writes := failer.take()
				t.Logf("try %d with the rule's reads failing: %d reads of the rule, %d statements on comment_deliveries (%d writes); s1 took %q",
					try, reads, statements, writes, state.bodiesTo("s1"))
				if reads != 1 || statements != 0 {
					t.Fatalf("try %d read the rule %d times and made %d statements on comment_deliveries, %d of them writes; "+
						"want one read of the rule alone and no statement on comment_deliveries", try, reads, statements, writes)
				}
				if bodies := state.bodiesTo("s1"); len(bodies) != 0 {
					t.Fatalf("try %d: s1 took %q while the first send's reads of the rule failed, want nothing", try, bodies)
				}
			}
			recorded, unsure := recordedClaim(sender, key)
			if now := attemptClaimedAt(t, database, first, "session:s1"); !now.Equal(claimed) || unsure || !recorded.Equal(now) {
				t.Fatalf("after four failed reads the first's row carries %v (was %v) and the sender records %v (unsure %t), "+
					"want the claim unmoved and recorded", now, claimed, recorded, unsure)
			}
			gauged("after four failed reads")

			failer.failing.Store(false)
			unpark()
			settledCommentDeliveries(t, database, second)
			// A send's WARN as it goes (reportLate) comes after its attempt settles: the lanes are
			// done with once no goroutine serves one.
			waitUntil(t, "no goroutine serving a lane", func() bool {
				sender.mu.Lock()
				defer sender.mu.Unlock()
				return sender.serving == 0
			})
			states := attemptStates(t, database, "comment_deliveries", "comment_id", first)
			bodies := state.bodiesTo("s1")
			warned := strings.Count(logs.String(), warning)
			t.Logf("once a read answers: the first's attempts %v; s1 took %q; %d held WARN lines", states, bodies, warned)
			if !slices.Equal(states, []string{"1:sent"}) || !slices.Equal(bodies, []string{"the first", "the second"}) {
				t.Fatalf("the first's attempts are %v and s1 took %q, want the first sent, before the second", states, bodies)
			}
			if want := int(heldCopies); warned != want {
				t.Fatalf("%d held WARN lines, want %d:\n%s", warned, want, logs.String())
			}
		})
	}
}

// ruleReadFailer is a tracer that, once armed, fails the first read of the copy rule that follows
// the take of an attempt's claim (claimedCommentDelivery.take): the read a held send's whole
// delivery makes once it holds the claim (deliverResolvedCommentMention). It fails that read
// before it is sent, as a statement timeout or a dropped connection would, by handing pgx a context
// that is already done; every other statement passes.
//
// It watches batches as well as statements sent alone (pgx.BatchTracer, which pgx uses only when
// the connection's tracer is one, as storetest.Traced installs this one): pgx hands a statement
// sent in a batch to a batch tracer and never to TraceQueryStart, and the take goes in a batch
// beside what it states of its attempt in the store (graphstore.QueueAttempted). The failpoint arms
// on the take whichever way it is sent, so a take sent in a batch still arms it and the read after
// it is still the one that fails. The read of the rule goes alone (copyrule.Holder), so only
// TraceQueryStart fails it.
type ruleReadFailer struct {
	stage  atomic.Int32
	failed atomic.Int32
}

const (
	ruleReadOff int32 = iota
	ruleReadArmed
	ruleReadAfterATake
	ruleReadFailed
)

var errRuleReadFails = errors.New("the read of the copy rule fails")

// ruleReadTake is the text of the take's statement (claimedCommentDelivery.take), which arms the
// failpoint.
const ruleReadTake = "update comment_deliveries set claimed_at = now()"

func (f *ruleReadFailer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.Contains(data.SQL, ruleReadTake):
		f.stage.CompareAndSwap(ruleReadArmed, ruleReadAfterATake)
	case strings.Contains(data.SQL, "k.owed_keys &&") && f.stage.CompareAndSwap(ruleReadAfterATake, ruleReadFailed):
		f.failed.Add(1)
		done, cancel := context.WithCancelCause(ctx)
		cancel(errRuleReadFails)
		return done
	}
	return ctx
}

func (f *ruleReadFailer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TraceBatchStart arms the failpoint on a take queued in the batch, as TraceQueryStart arms it on
// a take sent alone, before any statement of the batch is sent.
func (f *ruleReadFailer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if data.Batch == nil {
		return ctx
	}
	for _, queued := range data.Batch.QueuedQueries {
		if strings.Contains(queued.SQL, ruleReadTake) {
			f.stage.CompareAndSwap(ruleReadArmed, ruleReadAfterATake)
		}
	}
	return ctx
}

func (f *ruleReadFailer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (f *ruleReadFailer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// ruleReadsFail is a tracer that fails every read of the copy rule while failing is set, as
// ruleReadFailer fails one, and counts the reads of the rule and the statements on
// comment_deliveries the server sends, and the writes among those, since its last take.
//
// It counts the statements of a batch as well as those sent alone (pgx.BatchTracer), for the
// reason ruleReadFailer watches batches: pgx hands a batched statement to a batch tracer alone, and
// a claim or a release of an attempt goes in a batch beside what it states of the attempt in the
// store (graphstore.QueueAttempted). A try that sent a statement on comment_deliveries in a batch
// is counted as one that sent it alone, so it cannot pass as a try that made none. The read of the
// rule goes alone (copyrule.Holder), so only TraceQueryStart fails it.
type ruleReadsFail struct {
	failing                   atomic.Bool
	mu                        sync.Mutex
	reads, statements, writes int
}

// count counts one statement the server sends, alone or in a batch, and answers whether it is a
// read of the copy rule.
func (f *ruleReadsFail) count(statement string) bool {
	sql := strings.ToLower(strings.Join(strings.Fields(statement), " "))
	rule := strings.Contains(sql, "k.owed_keys &&")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case rule:
		f.reads++
	case strings.Contains(sql, "comment_deliveries"):
		f.statements++
		for _, write := range []string{"update comment_deliveries", "insert into comment_deliveries", "delete from comment_deliveries"} {
			if strings.Contains(sql, write) {
				f.writes++
				break
			}
		}
	}
	return rule
}

func (f *ruleReadsFail) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if f.count(data.SQL) && f.failing.Load() {
		done, cancel := context.WithCancelCause(ctx)
		cancel(errRuleReadFails)
		return done
	}
	return ctx
}

func (f *ruleReadsFail) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TraceBatchStart counts each statement queued in the batch, as TraceQueryStart counts one sent
// alone, before any of them is sent.
func (f *ruleReadsFail) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if data.Batch == nil {
		return ctx
	}
	for _, queued := range data.Batch.QueuedQueries {
		f.count(queued.SQL)
	}
	return ctx
}

func (f *ruleReadsFail) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (f *ruleReadsFail) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// take answers the counts since the last take and starts them again.
func (f *ruleReadsFail) take() (reads, statements, writes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reads, statements, writes = f.reads, f.statements, f.writes
	f.reads, f.statements, f.writes = 0, 0, 0
	return reads, statements, writes
}
