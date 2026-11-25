package intake

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// sweepClock is the time a sweep test runs on: the wall clock, moved on by the test, so a pass,
// a write and the pass after it are each later than the one before by at least the step.
type sweepClock struct{ ahead atomic.Int64 }

func (c *sweepClock) now() time.Time { return time.Now().Add(time.Duration(c.ahead.Load())) }

// step moves the clock a second on.
func (c *sweepClock) step() { c.ahead.Add(int64(time.Second)) }

// failsCapture3 is a handler that fails every fact of CAPTURE-3 and applies every other one as one
// write of intake_test_writes.
var failsCapture3 = handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
	if issue, ok := fact.(DispatchIssue); ok && issue.Key == "CAPTURE-3" {
		return Result{}, errAlwaysFails
	}
	_, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ('applied')`)
	return Result{}, err
})

// waitOpenFailures waits for an open failure of each of consumer's messages at seqs.
func waitOpenFailures(t *testing.T, pool *pgxpool.Pool, consumer string, seqs ...uint64) {
	t.Helper()
	testwait.Eventually(t, "an open failure of each failing message", func() bool {
		for _, seq := range seqs {
			if f, ok := intakeRecordOf(t, pool, consumer, seq); !ok || f.GivenUp() {
				return false
			}
		}
		return true
	})
}

// waitAckFloor waits for consumer's ack floor to reach seq.
func waitAckFloor(t *testing.T, stream jetstream.Stream, consumer string, seq uint64) {
	t.Helper()
	testwait.Eventually(t, "the ack floor at the failing messages", func() bool {
		item, err := stream.Consumer(context.Background(), consumer)
		return err == nil && item.CachedInfo().AckFloor.Stream >= seq
	})
}

// A failing message whose durable consumer is made again past it is never delivered again, and the
// sweep gives it up: not at the first pass that finds it at or below the consumer's ack floor, at
// the next, with the reason that the consumer moved past it, and never one a delivery wrote since
// the pass before. Both ways a durable is made past a message: deleted and made again at the next
// message, and made again by OpenConsumers at a stored position later than the message
// (createConsumer). Run's own sweep then gives up the one left within a few of its passes.
func TestASweepGivesUpAFailingMessageItsConsumerWasMadeAgainPast(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		// deliverNew makes the durable again at the next message; otherwise OpenConsumers makes it
		// again at the position the fact applied after the failing messages stored.
		deliverNew bool
	}{
		{name: "made again at the next message", deliverNew: true},
		{name: "made again by OpenConsumers at a later stored position"},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := migratedPool(t)
			createWrites(t, pool)
			js, stream := testJetStream(t)
			clock := &sweepClock{}
			spec := consumerSpec(&lockedBuffer{})
			spec.Now = clock.now
			consumers, err := OpenConsumers(context.Background(), js, pool, spec)
			if err != nil {
				t.Fatalf("OpenConsumers: %v", err)
			}
			stop := runConsumers(t, consumers, failsCapture3)
			captured := capturedIssueUpdatedEnvelope(t)
			left := publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", envelopeWithID(t, captured, "dispatch-swept-1"))
			touched := publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", envelopeWithID(t, captured, "dispatch-swept-2"))
			applied := publish(t, js, "notifications.dispatch.issue.CAPTURE-4.issue.created", capturedIssueCreatedEnvelope(t))
			dispatch := dispatchConsumerName(spec.Project)
			waitOpenFailures(t, pool, dispatch, left.Sequence, touched.Sequence)
			testwait.Eventually(t, "the fact behind the failing messages applied", func() bool { return writeCount(t, pool) == 1 })
			stop()

			filters := consumerInfo(t, stream, dispatch).Config.FilterSubjects
			if err := stream.DeleteConsumer(context.Background(), dispatch); err != nil {
				t.Fatalf("delete the Dispatch durable: %v", err)
			}
			if row.deliverNew {
				if _, err := stream.CreateConsumer(context.Background(), jetstream.ConsumerConfig{Durable: dispatch, FilterSubjects: filters,
					AckPolicy: jetstream.AckExplicitPolicy, AckWait: spec.AckWait, DeliverPolicy: jetstream.DeliverNewPolicy}); err != nil {
					t.Fatalf("make the Dispatch durable again at the next message: %v", err)
				}
			} else if consumers, err = OpenConsumers(context.Background(), js, pool, spec); err != nil {
				t.Fatalf("OpenConsumers at the stored position: %v", err)
			}
			waitAckFloor(t, stream, dispatch, applied.Sequence-1)

			sweep := &failureSweep{consumers: consumers}
			clock.step()
			sweep.pass(context.Background())
			for _, seq := range []uint64{left.Sequence, touched.Sequence} {
				if f, _ := intakeRecordOf(t, pool, dispatch, seq); f.GivenUp() {
					t.Fatalf("the record of message %d = %+v after the first pass that found it below the floor, want it open", seq, f)
				}
			}
			clock.step()
			touch, _ := intakeRecordOf(t, pool, dispatch, touched.Sequence)
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				_, err := record.NewStore().NoteRedelivery(context.Background(), tx, touch.IntakeMessage, touch.Deliveries+1, clock.now())
				return err
			}); err != nil {
				t.Fatalf("note a delivery of message %d: %v", touched.Sequence, err)
			}
			clock.step()
			sweep.pass(context.Background())
			given, _ := intakeRecordOf(t, pool, dispatch, left.Sequence)
			if given.GiveUpReason != record.GiveUpConsumerMovedPast || given.LastError != record.ConsumerMovedPastError {
				t.Fatalf("the record of message %d = %+v after the second pass, want it given up because its consumer moved past it", left.Sequence, given)
			}
			if f, _ := intakeRecordOf(t, pool, dispatch, touched.Sequence); f.GivenUp() {
				t.Fatalf("the record of message %d = %+v, want it open: a delivery wrote it after the first pass", touched.Sequence, f)
			}

			// Run's own sweep, at a period a test can wait out.
			again, err := OpenConsumers(context.Background(), js, pool, spec)
			if err != nil {
				t.Fatalf("OpenConsumers: %v", err)
			}
			again.sweepEvery = 100 * time.Millisecond
			defer runConsumers(t, again, failsCapture3)()
			if f := waitGivenUp(t, pool, dispatch, touched.Sequence); f.GiveUpReason != record.GiveUpConsumerMovedPast {
				t.Fatalf("the record of message %d = %+v, want it given up by Run's sweep because its consumer moved past it", touched.Sequence, f)
			}
		})
	}
}

// A failing message the stream deletes while it waits to be delivered again is given up by the
// sweep, which finds it below the consumer's ack floor: with the retention margin at a nanosecond
// the early give-up does not take it first, and the consumers are stopped while the stream
// deletes it, so no delivery finds it gone.
func TestASweepGivesUpAFailingMessageTheStreamDeleted(t *testing.T) {
	// Not parallel: it stops the consumers between the first failure and the 500 ms nak delay, so its one delivery holds only inside a margin the load of tests beside it can eat.
	pool := migratedPool(t)
	js := testnats.JetStream(t)
	stream := testnats.CreateStream(t, js, jetstream.StreamConfig{Name: notificationStream, Subjects: []string{"notifications.>"}, MaxAge: 3 * time.Second})
	spec := consumerSpec(&lockedBuffer{})
	spec.NakDelay, spec.NakDelayCap, spec.RetentionMargin = 500*time.Millisecond, 500*time.Millisecond, time.Nanosecond
	alwaysFails := handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) { return Result{}, errAlwaysFails })
	stop := startConsume(t, js, spec, pool, alwaysFails)
	sent := publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	dispatch := dispatchConsumerName(spec.Project)
	waitOpenFailures(t, pool, dispatch, sent.Sequence)
	stop()

	testwait.Eventually(t, "the stream's deletion of the message", func() bool {
		_, err := stream.GetMsg(context.Background(), sent.Sequence)
		return errors.Is(err, jetstream.ErrMsgNotFound)
	})
	waitAckFloor(t, stream, dispatch, sent.Sequence)
	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	consumers.sweepEvery = 100 * time.Millisecond
	defer runConsumers(t, consumers, alwaysFails)()
	if given := waitGivenUp(t, pool, dispatch, sent.Sequence); given.GiveUpReason != record.GiveUpConsumerMovedPast || given.Deliveries != 1 {
		t.Fatalf("the record = %+v, want it given up at its one delivery because its consumer moved past it", given)
	}
}

// A message the consumer moved past is given up at the sweep's second pass at or below the floor,
// so within two of its periods and one floor read of the move: at the production period that is
// under the 5 minutes R1 of dispatch://INFRA-1575 allows.
func TestTheSweepGivesUpWithinFiveMinutesOfTheConsumerMovingPast(t *testing.T) {
	t.Parallel()
	if within := 2*failureSweepEvery + consumerLagBound; within >= 5*time.Minute {
		t.Fatalf("the sweep gives up a message the consumer moved past within %s, want under 5m", within)
	}
}

// A change of the GitHub consumer's filter while a message it took is naked delivers that message
// again, and the delivery, which no longer decodes to a fact of a repository GitHub intake hears,
// acknowledges it and deletes its open failure: nothing is left for the sweep.
func TestAFilterChangeDeliversAPendingMessageAgainAndLeavesNoOpenFailure(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	spec.NakDelay = time.Second
	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	defer runConsumers(t, consumers, handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) { return Result{}, errAlwaysFails }))()
	sent := publish(t, js, "notifications.github.sjawhar.legion.pr.42", capturedGitHubEnvelope(t, "pr-opened.json"))
	github := githubConsumerName(spec.Project)
	waitOpenFailures(t, pool, github, sent.Sequence)
	if err := consumers.SetRepositories(context.Background(), []ghrepo.Repository{ghrepo.MustParse("acme/widgets")}); err != nil {
		t.Fatalf("SetRepositories: %v", err)
	}
	assertAcknowledgedThrough(t, stream, github, sent)
	testwait.Eventually(t, "the open failure deleted", func() bool {
		_, found := intakeRecordOf(t, pool, github, sent.Sequence)
		return !found
	})
}
