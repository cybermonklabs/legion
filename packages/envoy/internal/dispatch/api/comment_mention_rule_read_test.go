package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	waitUntil(t, 10*time.Second, "both sends on the issue's held lane", func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		return len(sender.lanes[held]) == 2
	})

	publishIssueEvents(t, database, issue.Key)
	failer.stage.Store(ruleReadArmed)
	unpark()
	var parked func()
	waitUntil(t, 10*time.Second, "the first send parked again, or a send reaching s1", func() bool {
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

// ruleReadFailer is a tracer that, once armed, fails the first read of the copy rule that follows
// the take of an attempt's claim (claimedCommentDelivery.take): the read a held send's whole
// delivery makes once it holds the claim (deliverResolvedCommentMention). It fails that read
// before it is sent, as a statement timeout or a dropped connection would, by handing pgx a context
// that is already done; every other statement passes.
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

func (f *ruleReadFailer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.Contains(data.SQL, "update comment_deliveries set claimed_at = now()"):
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
