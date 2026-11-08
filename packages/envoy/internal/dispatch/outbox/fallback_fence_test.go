package outbox

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// deadlinePassingOnTopic is the publisher hold a fallback pass runs under and the NATS it sends
// through: the window writes the copy while the deadline is still an hour ahead, and the deadline
// passes as that copy goes out, so the pass comes back from the answer to a hold whose margin ran
// out during the very wait it opened - what a renewal landing late leaves behind in production
// (publisherHoldOptions). holdPassingDuringAcknowledgement is the same hold for a batch. Only a
// send to topic passes it, so the scan that seeds the fallback row sends under a live deadline.
type deadlinePassingOnTopic struct {
	recordingPublisher
	topic  string
	passed atomic.Bool
}

func (p *deadlinePassingOnTopic) Deadline() time.Time {
	if p.passed.Load() {
		return time.Now().Add(-time.Second)
	}
	return time.Now().Add(time.Hour)
}

func (p *deadlinePassingOnTopic) WindowUntil(deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

func (p *deadlinePassingOnTopic) PublishAsync(item contracts.Envelope) <-chan error {
	answer := p.recordingPublisher.PublishAsync(item)
	if item.Topic == p.topic {
		p.passed.Store(true)
	}
	return answer
}

// TestAFallbackPassRecordsACopyWhoseAnswerOutlastedTheHoldsDeadline: the publisher hold's deadline
// governs a pass's sends and not its record, as it governs a batch's
// (TestRunCountsEveryDeniedJetStreamDestinationOfOneEvent, recordBatch). A copy the window let
// through went out under the hold, so what it came to is the hold's to record however long NATS
// took to answer: the row is settled published, and the next holder's pass publishes nothing of
// it. Left unrecorded, as the pass's own clock check left it, the row would sit claimed for
// fallbackClaim and the next holder's pass would send the copy a second time.
func TestAFallbackPassRecordsACopyWhoseAnswerOutlastedTheHoldsDeadline(t *testing.T) {
	const sessionTopic = "notifications.agent.s1"
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	publisher := &deadlinePassingOnTopic{topic: sessionTopic}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
	commentID := seedHeldFallbackCopy(ctx, t, deps)

	publishFallbackCopies(ctx, deps, publisher, holdingProcess(t, database))

	if !publisher.passed.Load() {
		t.Fatalf("the pass sent nothing to %s, so the hold's deadline never passed during an answer and the test holds nothing; the pass logged:\n%s",
			sessionTopic, logged.String())
	}
	// The scan that seeded the row published the issue topic; the copy is the pass's one send.
	copies := topicsOf(publisher.all())[1:]
	if len(copies) != 1 || copies[0] != sessionTopic {
		t.Fatalf("the pass published %#v, want s1's copy alone: the window took it before the deadline passed; the pass logged:\n%s",
			copies, logged.String())
	}
	row := readHeldCopy(t, database, commentID, sessionTopic)
	if row.attempts != 0 || !row.settled || !row.published {
		t.Fatalf("the held copy has attempts=%d settled=%v published=%v, want it settled published with no attempt counted: "+
			"its send went out under the hold, so the answer is the hold's to record whenever it came; the pass logged:\n%s",
			row.attempts, row.settled, row.published, logged.String())
	}
	if logged := logged.String(); strings.Contains(logged, "was not recorded") {
		t.Fatalf("the pass left itself unrecorded, so the next holder sends the copy again:\n%s", logged)
	}
}

// TestAFallbackPassWhoseHoldAnotherProcessTookRecordsNothing: the record is fenced on the process
// the pass runs in (recordFallbackCopies), so a pass whose publisher hold is no longer this
// process's - another process took it over while the copy was on the wire, or it lapsed with
// nobody taking it - writes nothing at all: neither the copy it published nor a release of the
// rows it did not decide. Every row it claimed keeps that claim and falls due again when the
// claim lapses, for the pass of whoever holds the hold by then, which publishes the copy again
// under the key this pass published it under; the host drops that repeat. A record written
// beside the process that took the hold is what the guard refuses: that process's own pass
// decides these rows, and two writers of one row settle it twice over.
func TestAFallbackPassWhoseHoldAnotherProcessTookRecordsNothing(t *testing.T) {
	const sessionTopic = "notifications.agent.s1"
	for _, tc := range []struct {
		name string
		// lose takes the publisher hold from the pass while its copy is on the wire.
		lose func(t *testing.T, database *store.Store)
	}{
		{
			name: "another process takes the hold over",
			lose: func(t *testing.T, database *store.Store) { holdRow(t, database, uuid.New()) },
		},
		{
			name: "the hold lapses with nobody taking it",
			lose: func(t *testing.T, database *store.Store) {
				t.Helper()
				if _, err := database.Pool.Exec(context.Background(),
					`update duty_holds set expires_at = now() - interval '1 second' where name = $1`, publisherHold); err != nil {
					t.Errorf("let the publisher hold lapse: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged lockedBuffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

			ctx := store.WithTransactionTracking(context.Background())
			database := storetest.Open(t)
			publisher := &takeoverPublisher{topic: sessionTopic}
			deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
			commentID := seedHeldFallbackCopy(ctx, t, deps)
			var lost atomic.Bool
			publisher.takeOver = func() {
				lost.Store(true)
				tc.lose(t, database)
			}

			before := time.Now()
			publishFallbackCopies(ctx, deps, unfenced{}, holdingProcess(t, database))
			after := time.Now()

			if !lost.Load() {
				t.Fatalf("the pass published no copy, so the hold was never taken from it and the test holds nothing; the pass logged:\n%s",
					logged.String())
			}
			row := readHeldCopy(t, database, commentID, sessionTopic)
			if row.attempts != 0 || row.settled || row.published || row.due == nil {
				t.Fatalf("the held copy has attempts=%d settled=%v published=%v next_attempt_at=%v, want it as the claim "+
					"left it: unsettled, unpublished, no attempt counted, for the pass of whoever holds the hold now; the pass logged:\n%s",
					row.attempts, row.settled, row.published, row.due, logged.String())
			}
			if row.due.Before(before.Add(fallbackClaim)) || row.due.After(after.Add(fallbackClaim)) {
				t.Fatalf("the held copy is due at %v, want the claim the pass took, fallbackClaim = %v out: a row the "+
					"record passed over is claimable again when that claim lapses, not stranded and not released early",
					*row.due, fallbackClaim)
			}
		})
	}
}

// TestAFallbackPassWhoseHoldAnotherProcessTookBeforeItsClaimRecordsNothing: the record is fenced
// on the process the pass runs in, and not on whoever the claim found holding, which answers only
// that somebody is publishing the outbox (recordFallbackCopies). A process whose hold another
// took before its pass claimed - its renewal never landed, while its own clock is still short of
// the deadline it last read - writes nothing: the rows it claimed are the new holder's pass's to
// decide, and a settle or a release written beside that holder makes two writers of one row.
func TestAFallbackPassWhoseHoldAnotherProcessTookBeforeItsClaimRecordsNothing(t *testing.T) {
	const sessionTopic = "notifications.agent.s1"
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
	commentID := seedHeldFallbackCopy(ctx, t, deps)
	stale := holdingProcess(t, database)
	// Another process takes the hold before this one claims a row of its own.
	holdRow(t, database, uuid.New())

	before := time.Now()
	publishFallbackCopies(ctx, deps, unfenced{}, stale)
	after := time.Now()

	// The scan that seeded the row published the issue topic; the copy is the pass's one send.
	if copies := topicsOf(publisher.all())[1:]; len(copies) != 1 || copies[0] != sessionTopic {
		t.Fatalf("the pass published %#v, want s1's copy alone: the pass whose record the guard must refuse is one "+
			"that claimed and sent; the pass logged:\n%s", copies, logged.String())
	}
	row := readHeldCopy(t, database, commentID, sessionTopic)
	if row.attempts != 0 || row.settled || row.published || row.due == nil {
		t.Fatalf("the held copy has attempts=%d settled=%v published=%v next_attempt_at=%v, want it as the claim "+
			"left it: the hold was another process's before this pass claimed, so what the pass came to is that "+
			"process's own pass to decide; the pass logged:\n%s",
			row.attempts, row.settled, row.published, row.due, logged.String())
	}
	if row.due.Before(before.Add(fallbackClaim)) || row.due.After(after.Add(fallbackClaim)) {
		t.Fatalf("the held copy is due at %v, want the claim the pass took, fallbackClaim = %v out: a row the "+
			"record passed over is claimable again when that claim lapses, not stranded and not released early",
			*row.due, fallbackClaim)
	}
}

// resumedPastTheDeadline is a publisher hold whose deadline has passed: the process it names was
// stopped - a collection that ran long, a frozen machine, a SIGSTOP - and resumed past the margin
// its last renewal bought it (publisherHoldOptions).
type resumedPastTheDeadline struct{}

func (resumedPastTheDeadline) Deadline() time.Time { return time.Now().Add(-time.Second) }

// TestAFallbackPassResumedPastItsHoldsDeadlineClaimsNothing: a pass claims nothing once its
// hold's deadline has passed, as a scan starts no batch past it (Outbox.scan). The process may
// be one the hold has left by then - the deadline is the margin a renewal must land inside - so
// a claim of its own would keep up to batchSize rows out of the live holder's pass for
// fallbackClaim, and what it decided it would write beside that holder: a row it settles with
// nothing sent needs no send at all, so the window's refusal, which answers the sends
// (bus.ErrWindowDeadline), holds nothing of it back.
func TestAFallbackPassResumedPastItsHoldsDeadlineClaimsNothing(t *testing.T) {
	const sessionTopic = "notifications.agent.s1"
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
	commentID := seedHeldFallbackCopy(ctx, t, deps)
	// The resend reached s1 after all, so a pass that claimed this row would settle it with
	// nothing sent: a write of its own that no send stands behind.
	if _, err := database.Pool.Exec(ctx,
		`update comment_deliveries set state = 'sent' where comment_id = $1`, commentID); err != nil {
		t.Fatalf("settle the resend of the mention: %v", err)
	}
	held := readHeldCopy(t, database, commentID, sessionTopic)

	full, failed := publishFallbackCopies(ctx, deps, resumedPastTheDeadline{}, holdingProcess(t, database))

	if full || failed {
		t.Fatalf("the pass reports full=%v failed=%v, want neither: it claimed no row and no statement of it failed",
			full, failed)
	}
	if copies := topicsOf(publisher.all())[1:]; len(copies) != 0 {
		t.Fatalf("the pass published %#v past its hold's deadline, want nothing sent: the window refuses every one "+
			"of them anyway; the pass logged:\n%s", copies, logged.String())
	}
	row := readHeldCopy(t, database, commentID, sessionTopic)
	switch {
	case row.attempts != held.attempts || row.settled || row.published:
		t.Fatalf("the held copy has attempts=%d settled=%v published=%v, want it as the scan left it, attempts=%d "+
			"and undecided: a pass past its hold's deadline decides no row; the pass logged:\n%s",
			row.attempts, row.settled, row.published, held.attempts, logged.String())
	case (row.due == nil) != (held.due == nil), row.due != nil && !row.due.Equal(*held.due):
		t.Fatalf("the held copy is due at %v, want the %v the scan left it: a pass past its hold's deadline claims "+
			"no row, so the row is the live holder's pass's now and not held out of it for fallbackClaim = %v",
			row.due, held.due, fallbackClaim)
	}
	// The row is still the hold's own pass's to decide, and that pass settles it.
	publishFallbackCopies(ctx, deps, unfenced{}, holdingProcess(t, database))
	if settled := readHeldCopy(t, database, commentID, sessionTopic); !settled.settled || settled.published {
		t.Fatalf("the holder's own pass left the held copy settled=%v published=%v, want it settled with nothing "+
			"published: the row the pass past its deadline left alone is this one's to decide; the passes logged:\n%s",
			settled.settled, settled.published, logged.String())
	}
}

// TestAFallbackPassWhoseHoldLapsedBeforeItsClaimRecordsNothingBesideItsNextHolder: a hold that
// lapsed leaves the pass no freer to write than one another process took, since what the record
// is guarded on is the process the pass runs in (recordFallbackCopies), and that process holds
// nothing. A lapsed hold is a hold about to be taken - whoever polls for it next takes it - so a
// pass that claimed under none and wrote what it came to writes beside the process that took the
// hold while its copy was on the wire, whose own pass these rows are now.
func TestAFallbackPassWhoseHoldLapsedBeforeItsClaimRecordsNothingBesideItsNextHolder(t *testing.T) {
	const sessionTopic = "notifications.agent.s1"
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	publisher := &takeoverPublisher{topic: sessionTopic}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
	commentID := seedHeldFallbackCopy(ctx, t, deps)
	stale := holdingProcess(t, database)
	if _, err := database.Pool.Exec(ctx,
		`update duty_holds set expires_at = now() - interval '1 second' where name = $1`, publisherHold); err != nil {
		t.Fatalf("let the publisher hold lapse before the pass claims: %v", err)
	}
	var took atomic.Bool
	publisher.takeOver = func() {
		took.Store(true)
		holdRow(t, database, uuid.New())
	}

	before := time.Now()
	publishFallbackCopies(ctx, deps, unfenced{}, stale)
	after := time.Now()

	if !took.Load() {
		t.Fatalf("the pass published no copy, so nobody took the lapsed hold while it sent and the test holds "+
			"nothing; the pass logged:\n%s", logged.String())
	}
	row := readHeldCopy(t, database, commentID, sessionTopic)
	if row.attempts != 0 || row.settled || row.published || row.due == nil {
		t.Fatalf("the held copy has attempts=%d settled=%v published=%v next_attempt_at=%v, want it as the claim "+
			"left it: the pass held nothing when it claimed, so what it came to is the pass's of whoever holds the "+
			"hold now; the pass logged:\n%s",
			row.attempts, row.settled, row.published, row.due, logged.String())
	}
	if row.due.Before(before.Add(fallbackClaim)) || row.due.After(after.Add(fallbackClaim)) {
		t.Fatalf("the held copy is due at %v, want the claim the pass took, fallbackClaim = %v out: a row the "+
			"record passed over is claimable again when that claim lapses, not stranded and not released early",
			*row.due, fallbackClaim)
	}
}
