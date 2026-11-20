package intake

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// boundSpec is consumerSpec with the bound shortened: a message is given up at its delivery
// deliveries once its first failure is after old, and naked at delays doubling from 20 ms to a cap
// of 160 ms.
func boundSpec(logs *lockedBuffer, deliveries int, after time.Duration) ConsumerSpec {
	spec := consumerSpec(logs)
	spec.NakDelay, spec.NakDelayCap = 20*time.Millisecond, 160*time.Millisecond
	spec.GiveUpDeliveries, spec.GiveUpAfter = deliveries, after
	return spec
}

// errAlwaysFails is the error of a handler whose every application fails.
var errAlwaysFails = errors.New("the handler always fails")

// A decoded message whose handler fails each time comes back at delays that double up to the cap,
// and is given up only once JetStream has delivered it the count and its first failure is the hour
// old: never at an earlier delivery, and never before the hour, however many deliveries come
// first. It is then terminated and not delivered again. The nak lines and the give-up line carry
// the delivery count.
func TestAMessageWhoseHandlerAlwaysFailsIsGivenUpAfterItsCountAndItsHour(t *testing.T) {
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := boundSpec(logs, 6, time.Second)
	var mu sync.Mutex
	var calls []time.Time
	var givenUpWhileHandled bool
	var readErr error
	handler := handlerFunc(func(ctx context.Context, _ pgx.Tx, _ Fact) (Result, error) {
		records, err := readIntakeRecords(ctx, pool)
		given := slices.ContainsFunc(records, record.IntakeFailure.GivenUp)
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, time.Now())
		givenUpWhileHandled = givenUpWhileHandled || given
		readErr = errors.Join(readErr, err)
		return Result{}, errAlwaysFails
	})
	stop := startConsume(t, js, spec, pool, handler)
	defer stop()

	sent := publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	dispatch := dispatchConsumerName(spec.Project)
	given := waitGivenUp(t, pool, dispatch, sent.Sequence)
	assertAcknowledgedThrough(t, stream, dispatch, sent)
	time.Sleep(2 * spec.NakDelayCap)

	mu.Lock()
	defer mu.Unlock()
	if readErr != nil || givenUpWhileHandled {
		t.Fatalf("a give-up record was there while the handler ran: %t (%v), want none before the last failure", givenUpWhileHandled, readErr)
	}
	if given.GiveUpReason != record.GiveUpBound || given.Deliveries < spec.GiveUpDeliveries || given.Deliveries != len(calls) {
		t.Fatalf("the record = %+v after %d handler calls, want it given up at the bound, at a delivery of %d or more that the handler ran", given, len(calls), spec.GiveUpDeliveries)
	}
	if age := given.GivenUpAt.Sub(given.FirstFailedAt); age < spec.GiveUpAfter {
		t.Fatalf("given up %s after its first failure, want %s or more: the count alone gives nothing up", age, spec.GiveUpAfter)
	}
	for i := 1; i < len(calls); i++ {
		want := min(spec.NakDelay<<(i-1), spec.NakDelayCap)
		if gap := calls[i].Sub(calls[i-1]); gap < want {
			t.Errorf("delivery %d came %s after delivery %d, want %s or more", i+1, gap, i, want)
		}
	}
	for delivered := 1; delivered < len(calls); delivered++ {
		requireLogLine(t, logs, "level=WARN", "retryable intake processing failure", "delivered="+strconv.Itoa(delivered)+" ")
	}
	requireLogLine(t, logs, "level=ERROR", "intake: a message was given up", "delivered="+strconv.Itoa(len(calls))+" ", "reason=bound")
}

// A message whose handling never ends — a handler that holds it until the daemon stops, as one
// that kills the daemon would — counts each delivery, and once the delivery before one did not
// end and the bound is reached, it is given up before its handler runs again: the handler would
// end the daemon again. A give-up whose commit fails naks the message and still runs no handler.
func TestAMessageWhoseHandlingNeverEndsIsGivenUpWithoutRunningItAgain(t *testing.T) {
	for _, row := range []struct {
		name string
		// refuseCommit refuses the commit of the first give-up the delivery after the third tries.
		refuseCommit bool
	}{
		{name: "the give-up commits"},
		{name: "the give-up's commit fails", refuseCommit: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			raw := migratedPool(t)
			pool := &failingRecordPool{Pool: raw}
			js, stream := testJetStream(t)
			spec := boundSpec(&lockedBuffer{}, 3, time.Nanosecond)
			var calls atomic.Int32
			entered := make(chan struct{}, 1)
			handler := handlerFunc(func(ctx context.Context, _ pgx.Tx, _ Fact) (Result, error) {
				calls.Add(1)
				entered <- struct{}{}
				<-ctx.Done()
				return Result{}, ctx.Err()
			})
			run := func() func() {
				consumers, err := OpenConsumers(context.Background(), js, pool, spec)
				if err != nil {
					t.Fatalf("OpenConsumers: %v", err)
				}
				return runConsumers(t, consumers, handler)
			}
			// The durable is made before the message is stored, so it delivers it.
			stop := run()
			sent := publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
			for delivery := 1; delivery <= 3; delivery++ {
				if delivery > 1 {
					stop = run()
				}
				waitEntered(t, entered, "delivery "+strconv.Itoa(delivery))
				stop()
			}
			pool.refuseCommit.Store(row.refuseCommit)
			stop = run()
			defer stop()
			dispatch := dispatchConsumerName(spec.Project)
			if row.refuseCommit {
				testwait.Eventually(t, "the give-up's commit refused", func() bool { return pool.commitsRefused.Load() >= 1 })
				if f, _ := intakeRecordOf(t, raw, dispatch, sent.Sequence); f.GivenUp() {
					t.Fatalf("the record = %+v after its commit was refused, want no give-up", f)
				}
				pool.refuseCommit.Store(false)
			}
			given := waitGivenUp(t, raw, dispatch, sent.Sequence)
			assertAcknowledgedThrough(t, stream, dispatch, sent)
			if got := calls.Load(); got != 3 {
				t.Fatalf("the handler ran %d times, want the 3 deliveries that did not end and no more", got)
			}
			if given.GiveUpReason != record.GiveUpDidNotEnd || !strings.Contains(given.LastError, "did not end: the daemon stopped or crashed while it ran") {
				t.Fatalf("the record = %+v, want it given up because a delivery did not end", given)
			}
		})
	}
}

// A message the stream would delete before its next delivery is given up at once, at its first
// failure, with the reason retention; so is one whose first delivery a daemon stop ended, on a
// context the stop does not end.
func TestAMessageTheStreamWouldDeleteBeforeItsNextDeliveryIsGivenUpAtOnce(t *testing.T) {
	for _, row := range []struct {
		name string
		stop bool
	}{
		{name: "a failed delivery"},
		{name: "a delivery a stop ended", stop: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := migratedPool(t)
			js := testnats.JetStream(t)
			stream := testnats.CreateStream(t, js, jetstream.StreamConfig{Name: notificationStream, Subjects: []string{"notifications.>"}, MaxAge: 3 * time.Second})
			spec := consumerSpec(&lockedBuffer{})
			spec.NakDelay, spec.RetentionMargin = 2*time.Second, time.Second
			var calls atomic.Int32
			entered := make(chan struct{}, 1)
			handler := handlerFunc(func(ctx context.Context, _ pgx.Tx, _ Fact) (Result, error) {
				calls.Add(1)
				if !row.stop {
					return Result{}, errAlwaysFails
				}
				entered <- struct{}{}
				<-ctx.Done()
				return Result{}, ctx.Err()
			})
			stop := startConsume(t, js, spec, pool, handler)
			defer stop()
			sent := publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
			if row.stop {
				waitEntered(t, entered, "the first delivery")
				stop()
			}
			dispatch := dispatchConsumerName(spec.Project)
			given := waitGivenUp(t, pool, dispatch, sent.Sequence)
			if given.GiveUpReason != record.GiveUpRetention || given.Deliveries != 1 || calls.Load() != 1 {
				t.Fatalf("the record = %+v after %d handler calls, want it given up for retention at its first delivery", given, calls.Load())
			}
			if !row.stop {
				assertAcknowledgedThrough(t, stream, dispatch, sent)
			}
		})
	}
}

// A give-up record holds the whole message: its project, consumer, subject, stream sequence,
// stored time, header block and payload bytes, its event id, the delivery count, the first
// failure and the last error. A NUL and a byte that is no UTF-8, in the payload and in a header
// value, are kept as they came.
func TestAGiveUpRecordHoldsTheWholeMessage(t *testing.T) {
	for _, row := range []struct {
		name    string
		data    []byte
		header  nats.Header
		headers string
		// decodes is a message the handler fails, given up at its second delivery.
		decodes bool
	}{
		{name: "a decoded failing message with a header", data: capturedIssueUpdatedEnvelope(t), decodes: true,
			header: nats.Header{"Trace": {"one", "two"}}, headers: "NATS/1.0\r\nTrace: one\r\nTrace: two\r\n\r\n"},
		{name: "an undecodable message holding a NUL and an invalid byte", data: []byte("{\"a\x00b\xff"),
			header: nats.Header{"Trace": {"a\x00b\xffc"}}, headers: "NATS/1.0\r\nTrace: a\x00b\xffc\r\n\r\n"},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := migratedPool(t)
			js, stream := testJetStream(t)
			spec := boundSpec(&lockedBuffer{}, 2, time.Nanosecond)
			stop := startConsume(t, js, spec, pool, handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
				return Result{}, errAlwaysFails
			}))
			defer stop()
			const subject = "notifications.dispatch.issue.CAPTURE-3.issue.updated"
			sent, err := js.PublishMsg(context.Background(), &nats.Msg{Subject: subject, Data: row.data, Header: row.header})
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
			stored, err := stream.GetMsg(context.Background(), sent.Sequence)
			if err != nil {
				t.Fatalf("read the stored message: %v", err)
			}
			dispatch := dispatchConsumerName(spec.Project)
			given := waitGivenUp(t, pool, dispatch, sent.Sequence)
			if given.Project != "CAPTURE" || given.Consumer != dispatch || given.Subject != subject || given.StreamSeq != sent.Sequence ||
				!given.StoredAt.Equal(stored.Time.Truncate(time.Microsecond)) {
				t.Errorf("the record's key = %s %s %s %d %s, want CAPTURE %s %s %d %s", given.Project, given.Consumer, given.Subject, given.StreamSeq,
					given.StoredAt, dispatch, subject, sent.Sequence, stored.Time.Truncate(time.Microsecond))
			}
			if !bytes.Equal(given.Payload, row.data) || string(given.Headers) != row.headers {
				t.Errorf("the record holds payload %q and headers %q, want %q and %q byte for byte", given.Payload, given.Headers, row.data, row.headers)
			}
			if given.FirstFailedAt.IsZero() || given.FirstFailedAt.After(given.GivenUpAt) {
				t.Errorf("the record's first failure = %s, given up at %s, want a first failure at or before the give-up", given.FirstFailedAt, given.GivenUpAt)
			}
			if row.decodes {
				if given.EventSource != "dispatch" || given.EventID != "dispatch-15" || given.Deliveries != 2 || given.LastError != errAlwaysFails.Error() {
					t.Errorf("the record = %+v, want the event dispatch-15 given up at its second delivery with the handler's error", given)
				}
			} else if given.EventID != "" || given.Deliveries != 1 || !strings.Contains(given.LastError, "decode Envoy envelope") {
				t.Errorf("the record = %+v, want no event id, its first delivery and the decode error", given)
			}
		})
	}
}

// waitEntered waits for a handler to send on entered that delivery what reached it.
func waitEntered(t *testing.T, entered <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		t.Fatalf("%s never reached the handler", what)
	}
}

// A Dispatch event given up moves the Dispatch consumer's floor to its sequence, so admission's
// hold, which waits for the floor, no longer waits for it, and the issue event behind it applies
// once.
func TestAGivenUpMessageMovesTheFloorAndTheMessagesBehindItApply(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, _ := testJetStream(t)
	spec := boundSpec(&lockedBuffer{}, 2, time.Nanosecond)
	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	stop := runConsumers(t, consumers, handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
		if issue, ok := fact.(DispatchIssue); ok && issue.Key == "CAPTURE-3" {
			return Result{}, errAlwaysFails
		}
		_, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ('behind')`)
		return Result{}, err
	}))
	defer stop()

	failing := publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	behind := publish(t, js, "notifications.dispatch.issue.CAPTURE-4.issue.created", capturedIssueCreatedEnvelope(t))
	testwait.Eventually(t, "the Dispatch floor past the given-up event and the one behind it", func() bool {
		position, err := consumers.DispatchPosition(context.Background())
		return err == nil && position.AckFloorStream >= int64(behind.Sequence)
	})
	if given := waitGivenUp(t, pool, dispatchConsumerName(spec.Project), failing.Sequence); given.GiveUpReason != record.GiveUpBound {
		t.Fatalf("the failing event's record = %+v, want it given up at the bound", given)
	}
	if got := writeCount(t, pool); got != 1 {
		t.Fatalf("the event behind the given-up one applied %d times, want once", got)
	}
}

// The count and the first failure are the record's and JetStream's, not the consumers' memory: a
// restart of the consumers on the same stream and pool keeps the first failure's time, and the
// give-up at the bound counts the deliveries before the restart.
func TestTheCountAndTheFirstFailureSurviveARestartOfTheConsumers(t *testing.T) {
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	spec := boundSpec(&lockedBuffer{}, 4, time.Nanosecond)
	spec.NakDelay, spec.NakDelayCap = 200*time.Millisecond, 200*time.Millisecond
	var calls atomic.Int32
	handler := handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
		calls.Add(1)
		return Result{}, errAlwaysFails
	})
	stop := startConsume(t, js, spec, pool, handler)
	sent := publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	dispatch := dispatchConsumerName(spec.Project)
	var before record.IntakeFailure
	testwait.Eventually(t, "two failed deliveries recorded", func() bool {
		f, ok := intakeRecordOf(t, pool, dispatch, sent.Sequence)
		before = f
		return ok && f.EndedDelivery >= 2
	})
	stop()
	if handled := calls.Load(); handled >= int32(spec.GiveUpDeliveries) {
		t.Fatalf("the handler ran %d times before the restart, want fewer than the bound's %d", handled, spec.GiveUpDeliveries)
	}

	defer startConsume(t, js, spec, pool, handler)()
	given := waitGivenUp(t, pool, dispatch, sent.Sequence)
	assertAcknowledgedThrough(t, stream, dispatch, sent)
	if !given.FirstFailedAt.Equal(before.FirstFailedAt) {
		t.Fatalf("the first failure after the restart = %s, want %s from before it", given.FirstFailedAt, before.FirstFailedAt)
	}
	if handled := calls.Load(); given.GiveUpReason != record.GiveUpBound || given.Deliveries != spec.GiveUpDeliveries || handled != int32(spec.GiveUpDeliveries) {
		t.Fatalf("the record = %+v after %d handler calls, want it given up at the bound's delivery %d, counting those before the restart",
			given, handled, spec.GiveUpDeliveries)
	}
}

// A decoded message that fails below the bound writes its open failure at that first failure,
// holding the whole message and the time, and applies at a later delivery, whose transaction
// deletes the open failure: no record is left.
func TestAFactThatFailsBelowTheBoundAppliesAndLeavesNoRecord(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	data := capturedIssueUpdatedEnvelope(t)
	var calls atomic.Int32
	var mu sync.Mutex
	var open []record.IntakeFailure
	var readErr error
	stop := startConsume(t, js, spec, pool, handlerFunc(func(ctx context.Context, tx pgx.Tx, _ Fact) (Result, error) {
		if calls.Add(1) == 1 {
			return Result{}, errAlwaysFails
		}
		committed, err := readIntakeRecords(ctx, pool)
		mu.Lock()
		open, readErr = committed, err
		mu.Unlock()
		_, err = tx.Exec(ctx, `insert into intake_test_writes (handler) values ('applied')`)
		return Result{}, err
	}))
	defer stop()
	sent := publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", data)
	dispatch := dispatchConsumerName(spec.Project)
	assertAcknowledgedThrough(t, stream, dispatch, sent)
	testwait.Eventually(t, "the fact applied once", func() bool { return writeCount(t, pool) == 1 })

	mu.Lock()
	defer mu.Unlock()
	if readErr != nil || len(open) != 1 {
		t.Fatalf("the open failures the second delivery found = %+v (%v), want the one the first failure wrote", open, readErr)
	}
	f := open[0]
	if f.GivenUp() || f.Consumer != dispatch || f.StreamSeq != sent.Sequence || !bytes.Equal(f.Payload, data) || f.EventID != "dispatch-15" ||
		f.EndedDelivery != 1 || f.LastError != errAlwaysFails.Error() || f.FirstFailedAt.IsZero() {
		t.Fatalf("the open failure = %+v, want the whole message, its first failure's time and error, open", f)
	}
	if records := intakeRecords(t, pool); len(records) != 0 {
		t.Fatalf("intake_failures after the fact applied = %+v, want none", records)
	}
}

// Another project's issue event that does not decode is no fact of this daemon's, decided from
// its subject before its envelope is read, in either form of its name: it is acknowledged, and no
// record is made of it, as every daemon hears every project's issue events.
func TestAnotherProjectsUndecodableIssueEventMakesNoRecord(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	defer startConsume(t, js, spec, pool, writeHandler("applied", nil))()
	publish(t, js, "notifications.dispatch.issue.LEGION-7.issue.updated", []byte(`{`))
	publish(t, js, "notifications.dispatch.issue.LEGION.LEGION-7.issue.updated", []byte(`{`))
	last := publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	assertAcknowledgedThrough(t, stream, dispatchConsumerName(spec.Project), last)
	if records := intakeRecords(t, pool); len(records) != 0 {
		t.Fatalf("intake_failures = %+v, want no record of another project's events", records)
	}
	if strings.Contains(logs.String(), "intake: a message was given up") {
		t.Fatalf("another project's events were given up:\n%s", logs)
	}
}

// A fact that applies deletes its open failure inside its own transaction, beside its
// processed_events row, so the delete commits with the fact even when nothing runs after the
// commit: the handler, which runs after the position is recorded, finds the row gone in the
// transaction while it still stands committed. That holds for a message delivered again by its
// consumer, and for one a consumer made again at an older stored position delivers once more,
// with a delivery count of 1.
func TestAnAppliedFactDeletesItsOpenFailureInItsOwnTransaction(t *testing.T) {
	for _, row := range []struct {
		name string
		// remade makes the durable again, at the position of a fact applied before the failing
		// message, before the failing message applies.
		remade bool
	}{
		{name: "a fact that applies at its third delivery"},
		{name: "a fact a consumer made again delivers once more", remade: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := migratedPool(t)
			js, stream := testJetStream(t)
			spec := consumerSpec(&lockedBuffer{})
			dispatch := dispatchConsumerName(spec.Project)
			var failing atomic.Bool
			failing.Store(true)
			var calls atomic.Int32
			type seen struct{ inTransaction, committed, processed int }
			var mu sync.Mutex
			var saw *seen
			var readErr error
			handler := handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
				if issue, ok := fact.(DispatchIssue); !ok || issue.Key != "CAPTURE-3" {
					return Result{}, nil
				}
				if n := calls.Add(1); failing.Load() && (row.remade || n < 3) {
					return Result{}, errAlwaysFails
				}
				var got seen
				err := errors.Join(
					tx.QueryRow(ctx, `select count(*) from intake_failures where consumer = $1 and given_up_at is null`, dispatch).Scan(&got.inTransaction),
					tx.QueryRow(ctx, `select count(*) from processed_events where event_id = 'dispatch-15'`).Scan(&got.processed),
					pool.QueryRow(ctx, `select count(*) from intake_failures where consumer = $1 and given_up_at is null`, dispatch).Scan(&got.committed))
				mu.Lock()
				saw, readErr = &got, err
				mu.Unlock()
				return Result{}, nil
			})
			consumers, err := OpenConsumers(context.Background(), js, pool, spec)
			if err != nil {
				t.Fatalf("OpenConsumers: %v", err)
			}
			stop := runConsumers(t, consumers, handler)
			defer func() { stop() }()
			var sent *jetstream.PubAck
			if row.remade {
				applied := publish(t, js, "notifications.dispatch.issue.CAPTURE-4.issue.created", capturedIssueCreatedEnvelope(t))
				assertAcknowledgedThrough(t, stream, dispatch, applied)
				sent = publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
				testwait.Eventually(t, "the open failure of the failing message", func() bool {
					_, found := intakeRecordOf(t, pool, dispatch, sent.Sequence)
					return found
				})
				stop()
				if err := stream.DeleteConsumer(context.Background(), dispatch); err != nil {
					t.Fatalf("delete the Dispatch durable: %v", err)
				}
				failing.Store(false)
				if consumers, err = OpenConsumers(context.Background(), js, pool, spec); err != nil {
					t.Fatalf("OpenConsumers at the stored position: %v", err)
				}
				stop = runConsumers(t, consumers, handler)
			} else {
				sent = publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
			}
			assertAcknowledgedThrough(t, stream, dispatch, sent)
			mu.Lock()
			defer mu.Unlock()
			if readErr != nil || saw == nil || saw.inTransaction != 0 || saw.committed != 1 || saw.processed != 1 {
				t.Fatalf("the applying handler saw %+v (%v), want no open failure in its transaction beside its processed_events row, and the one committed before it", saw, readErr)
			}
			if _, found := intakeRecordOf(t, pool, dispatch, sent.Sequence); found {
				t.Fatal("the open failure outlived the fact that applied")
			}
		})
	}
}
