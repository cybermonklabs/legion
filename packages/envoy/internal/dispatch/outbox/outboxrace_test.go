package outbox

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The private-delivery hold beside the retry timer (Outbox.serve, Outbox.nextRetry): a holder's
// cycle asks PrivateHubsReady once for each batch's read of its scan and once for the fallback
// pass's claim, and its retry timer's read comes after them. The func signals nothing when it
// turns true, so an event or a copy a read held is read again only by a scan something else
// starts, and the timer the hold arms is that scan. The rows below make the func turn true between
// a read that held and the timer's read, every run, by answering by its call count, and run the
// outbox (Run) with nothing else to wake it: the rows are written before it runs and on a broker
// it does not hear, its NATS connection never changes, and its LISTEN connection never comes up
// (quietListen). Each row also reads that PRIV-1 was sent nothing by then, which is what shows the
// held ask was the one the row meant: a read answered true at that point would have sent it.

// quietListen is a LISTEN dial that never connects: it waits for the outbox's end. A row that
// counts the outbox's asks of PrivateHubsReady cycle by cycle runs with it, so the catch-up wake a
// LISTEN connection gives when it comes up (Outbox.listen) starts no scan the row did not count.
func quietListen(ctx context.Context) (*pgx.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// firstWait is what a holder had done when it first waited (afterServeWait): the waits it armed its
// retry timer for (afterRetryArmed), how often it had asked PrivateHubsReady, and how many
// envelopes it had sent to a topic naming the row's private owner.
type firstWait struct {
	armed []time.Duration
	asked int64
	sent  int
}

// runToFirstWait runs New(deps) until the test ends and returns what it had done when serve first
// waited: the hold's first scan, its fallback pass and the timer's read are behind it by then.
// asked is the count deps.PrivateHubsReady keeps, and private names the topics of the row's
// private owner.
func runToFirstWait(t *testing.T, deps Deps, asked *atomic.Int64, publisher *recordingPublisher, private string) firstWait {
	t.Helper()
	outbox := New(deps)
	var mu sync.Mutex
	var armed []time.Duration
	outbox.afterRetryArmed = func(wait time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		armed = append(armed, wait)
	}
	first := make(chan firstWait, 1)
	var once sync.Once
	outbox.afterServeWait = func() {
		once.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			first <- firstWait{armed: slices.Clone(armed), asked: asked.Load(), sent: len(topicsNaming(publisher, private))}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { outbox.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case got := <-first:
		return got
	case <-time.After(outboxHangGuard):
		t.Fatalf("the outbox did not wait within %v of its start", outboxHangGuard)
		return firstWait{}
	}
}

// TestAnEventAScanHeldPublishesWhenTheHubsTurnProvenBeforeTheTimerReads: PRIV-1's event is due
// when the outbox starts, and PrivateHubsReady answers false to its first ask alone - the hold's
// first scan reads once, and that read holds the event - and true from its second on, the pass's
// claim and every ask after it. The outbox waits having armed its retry timer for the hold's
// re-poll, and the scan that timer starts publishes the event: nothing else wakes the outbox.
//
// Control: nextRetry deciding by its own ask alone (the hold's arm only while the func answers
// false to it) reads nextRetrySQL, which counts no event never tried, so the outbox waits with no
// timer armed, and the row fails naming the empty wait.
func TestAnEventAScanHeldPublishesWhenTheHubsTurnProvenBeforeTheTimerReads(t *testing.T) {
	database := storetest.Open(t)
	seedHoldProjects(t, database, 1)
	private := appendHoldMessage(t, database, events.NewBroker(), "PRIV-1")

	var asked atomic.Int64
	publisher := &recordingPublisher{}
	deps := Deps{
		Store: database, Publisher: publisher, Broker: events.NewBroker(), Listen: quietListen,
		PrivateHubsReady: func() bool { return asked.Add(1) > 1 },
		privateHoldWait:  100 * time.Millisecond,
	}
	first := runToFirstWait(t, deps, &asked, publisher, ".PRIV-1.")
	if first.asked < 2 || first.sent != 0 {
		t.Fatalf("by its first wait the outbox asked PrivateHubsReady %d times and sent PRIV-1's topic %d envelopes, want 2 at least and none: the scan's one read, held, then the claim's, proven",
			first.asked, first.sent)
	}
	if len(first.armed) != 1 || first.armed[0] > deps.privateHoldWait {
		t.Fatalf("the outbox first waited with its retry timer armed for %v, want one wait of %v at most: the scan's read held PRIV-1's event and the hubs turned proven before the timer's read, so only that timer looks at the event again",
			first.armed, deps.privateHoldWait)
	}
	waitFor(t, 10*time.Second, "PRIV-1's event published by the scan the timer starts", func() bool {
		return publishedAt(t, database, private.ID) != nil
	})
	if sent := topicsNaming(publisher, ".PRIV-1."); len(sent) != 1 {
		t.Fatalf("PRIV-1's event was sent to %v, want its one topic", sent)
	}
}

// TestACopyAPassHeldPublishesWhenTheHubsTurnProvenBeforeTheTimerReads: PRIV-1's comment has a
// fallback copy held for s-priv when the outbox starts, and PrivateHubsReady answers false to its
// second ask alone - the fallback pass's claim, after the hold's first scan read nothing - and true
// to the scan's read before it and every ask after it. The outbox waits having armed its retry
// timer for the hold's re-poll, and the pass after the scan that timer starts publishes the copy.
//
// Control: nextRetry deciding by its own ask alone reads nextRetrySQL, which counts no copy never
// claimed, so the outbox waits with no timer armed, and the row fails naming the empty wait.
func TestACopyAPassHeldPublishesWhenTheHubsTurnProvenBeforeTheTimerReads(t *testing.T) {
	database := storetest.Open(t)
	seedHoldProjects(t, database, 1)
	private, _ := seedHoldFallbackCopies(t, database, events.NewBroker())

	var asked atomic.Int64
	publisher := &recordingPublisher{}
	deps := Deps{
		Store: database, Publisher: publisher, Broker: events.NewBroker(), Listen: quietListen,
		PrivateHubsReady: func() bool { return asked.Add(1) != 2 },
		privateHoldWait:  100 * time.Millisecond,
	}
	first := runToFirstWait(t, deps, &asked, publisher, "s-priv")
	if first.asked < 2 || first.sent != 0 {
		t.Fatalf("by its first wait the outbox asked PrivateHubsReady %d times and sent s-priv %d envelopes, want 2 at least and none: the scan's one read, proven, then the claim's, held",
			first.asked, first.sent)
	}
	if len(first.armed) != 1 || first.armed[0] > deps.privateHoldWait {
		t.Fatalf("the outbox first waited with its retry timer armed for %v, want one wait of %v at most: the pass's claim held PRIV-1's copy and the hubs turned proven before the timer's read, so only that timer looks at the copy again",
			first.armed, deps.privateHoldWait)
	}
	waitFor(t, 10*time.Second, "PRIV-1's copy published by the pass after the scan the timer starts", func() bool {
		row := readHeldCopy(t, database, private, "notifications.agent.s-priv")
		return row.settled && row.published
	})
	if sent := topicsNaming(publisher, "s-priv"); len(sent) != 1 {
		t.Fatalf("PRIV-1's copy was sent to %v, want s-priv's subject once", sent)
	}
}

// TestTheTimerLooksAgainOnceAfterAHeldRead: a read with PrivateHubsReady answering false leaves
// PRIV-1's event out, and the func answers true before the timer reads. nextRetry answers the
// hold's re-poll, privateHoldRecheck at the latest, rather than PUB-1's back-off an hour out; its
// next read, with no read held since, answers the hour again: what a read held arms the timer
// once, and a private event waiting does not keep it re-polling.
//
// Control: nextRetry deciding by its own ask alone answers the hour at its first read, and the row
// fails naming it; one that keeps what a read held past the read that took it answers the re-poll
// at its second, and the row fails naming that.
func TestTheTimerLooksAgainOnceAfterAHeldRead(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedHoldProjects(t, database, 1)
	backedOff := appendHoldMessage(t, database, broker, "PUB-1")
	if _, err := database.Pool.Exec(ctx, `update events set next_attempt_at = now() + interval '1 hour', attempt_count = 1 where id = $1`, backedOff.ID); err != nil {
		t.Fatalf("back PUB-1's event off an hour: %v", err)
	}
	appendHoldMessage(t, database, broker, "PRIV-1")

	var proven atomic.Bool
	outbox := New(Deps{Store: database, Publisher: &recordingPublisher{}, PrivateHubsReady: proven.Load})
	pending, _, _, err := readBatch(ctx, outbox.deps)
	if err != nil {
		t.Fatalf("read the batch while the hubs are unproven: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("the read while the hubs are unproven read %d events, want none: PRIV-1's held, PUB-1's not due", len(pending))
	}
	proven.Store(true)
	if wait, ok := outbox.nextRetry(ctx, false); !ok || wait > privateHoldRecheck {
		t.Errorf("after a read the hold kept PRIV-1's event out of, the next retry is due in %v (%v), want %v at the latest, the hold's re-poll, though the func answers true by now",
			wait, ok, privateHoldRecheck)
	}
	if wait, ok := outbox.nextRetry(ctx, false); !ok || wait < 50*time.Minute {
		t.Errorf("with no read held since the timer's last read, the next retry is due in %v (%v), want PUB-1's back-off, about an hour away", wait, ok)
	}
}

// TestANilPrivateHubsReadyLeavesTheTimersReadAsBefore (R9): with no PrivateHubsReady, as
// cmd/dispatch runs until a hub can prove it, the outbox (Run) publishes PRIV-1's event beside
// PUB-1's, every read binds the hold off, and every read of its retry timer is nextRetrySQL with
// no argument, never nextRetryHeldSQL.
//
// Control: nextRetry reading nextRetryHeldSQL whatever the hold fails the row naming it.
func TestANilPrivateHubsReadyLeavesTheTimersReadAsBefore(t *testing.T) {
	database := storetest.Open(t)
	seedHoldProjects(t, database, 1)
	broker := events.NewBroker()
	private := appendHoldMessage(t, database, broker, "PRIV-1")
	public := appendHoldMessage(t, database, broker, "PUB-1")

	tracer := &statementTracer{}
	timerReads := func() []tracedStatement { return tracer.matching("select (extract(epoch from least(") }
	stop := runDeps(t, Deps{Store: tracedStore(t, database, tracer), Publisher: &recordingPublisher{}, Broker: events.NewBroker(), Listen: quietListen})
	waitFor(t, 10*time.Second, "both events published and the retry timer read", func() bool {
		return publishedAt(t, database, private.ID) != nil && publishedAt(t, database, public.ID) != nil && len(timerReads()) > 0
	})
	stop()

	for _, statement := range timerReads() {
		if statement.sql != nextRetrySQL || len(statement.args) != 0 {
			t.Fatalf("with no PrivateHubsReady the retry timer read %.120q with %v, want nextRetrySQL with no argument", statement.sql, statement.args)
		}
	}
	held := tracer.matching("held.visibility = 'private'")
	if len(held) == 0 {
		t.Fatal("with no PrivateHubsReady no statement named the hold, want the batches' reads and the passes' claims")
	}
	for _, statement := range held {
		if on := statement.args[len(statement.args)-1]; on != false {
			t.Fatalf("with no PrivateHubsReady a statement bound the hold %v, want false: %.80q", on, statement.sql)
		}
	}
}
