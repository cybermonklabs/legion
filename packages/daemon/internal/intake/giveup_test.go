package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// fakeDelivery is one delivery of a stored message as a durable consumer hands it to
// consumeMessage: a fake is the only way to choose JetStream's delivery count, to make one
// termination fail, or to read the record at the moment the message is terminated. It keeps what
// consumeMessage asked of JetStream, and every call it does not define fails the row by the call's
// name (unexpectedMessage).
type fakeDelivery struct {
	unexpectedMessage
	subject  string
	data     []byte
	headers  nats.Header
	metadata jetstream.MsgMetadata
	// termFails is how many terminations fail before one is taken.
	termFails int
	// onTerm runs at each termination, before it is answered.
	onTerm func()

	mu    sync.Mutex
	acks  int
	terms int
	naks  []time.Duration
}

var _ jetstream.Msg = (*fakeDelivery)(nil)

// newDelivery is the first delivery by consumer, to the row t runs, of the message stored at
// stored as stream sequence seq.
func newDelivery(t testing.TB, consumer, subject string, data []byte, seq uint64, stored time.Time) *fakeDelivery {
	return &fakeDelivery{unexpectedMessage: unexpectedMessage{t: t}, subject: subject, data: data, metadata: jetstream.MsgMetadata{
		Sequence: jetstream.SequencePair{Stream: seq, Consumer: seq}, NumDelivered: 1, Consumer: consumer,
		Stream: notificationStream, Timestamp: stored,
	}}
}

// deliveredAgain is the same message delivered for the n-th time to the same row, with nothing
// asked of it yet.
func (d *fakeDelivery) deliveredAgain(n uint64) *fakeDelivery {
	again := &fakeDelivery{unexpectedMessage: d.unexpectedMessage, subject: d.subject, data: d.data, headers: d.headers,
		metadata: d.metadata, onTerm: d.onTerm}
	again.metadata.NumDelivered = n
	again.metadata.Sequence.Consumer = d.metadata.Sequence.Consumer + n - d.metadata.NumDelivered
	return again
}

func (d *fakeDelivery) Metadata() (*jetstream.MsgMetadata, error) {
	metadata := d.metadata
	return &metadata, nil
}
func (d *fakeDelivery) Subject() string      { return d.subject }
func (d *fakeDelivery) Data() []byte         { return d.data }
func (d *fakeDelivery) Headers() nats.Header { return d.headers }
func (d *fakeDelivery) InProgress() error    { return nil }

func (d *fakeDelivery) Ack() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.acks++
	return nil
}

func (d *fakeDelivery) NakWithDelay(delay time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.naks = append(d.naks, delay)
	return nil
}

func (d *fakeDelivery) Term() error {
	if d.onTerm != nil {
		d.onTerm()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.termFails > 0 {
		d.termFails--
		return errors.New("the test's NATS connection is closed")
	}
	d.terms++
	return nil
}

// asked is what consumeMessage asked of JetStream for d: how many acknowledgements and
// terminations it took, and the delay of each nak.
func (d *fakeDelivery) asked() (acks, terms int, naks []time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.acks, d.terms, append([]time.Duration(nil), d.naks...)
}

// consumeDelivery runs consumeMessage on d as consumeConsumer does: d's metadata parsed once and
// handed on with it.
func consumeDelivery(ctx context.Context, d *fakeDelivery, run consumerRun, handlers ...Handler) bool {
	metadata, _ := d.Metadata()
	return consumeMessage(ctx, d, metadata, run, handlers)
}

// productionRun is the Dispatch durable of project CAPTURE as the daemon runs it: the spec
// normalizedSpec gives the NakDelay the daemon's connect sets (1 s) and nothing else of the
// bound, on a stream that keeps a message for notificationStreamMaxAge, with now as its clock.
func productionRun(t *testing.T, pool store.TxPool, logs *lockedBuffer, now func() time.Time) consumerRun {
	t.Helper()
	spec, err := normalizedSpec(ConsumerSpec{Project: "CAPTURE", Repositories: capturedRepositories, NakDelay: time.Second,
		Logger: slog.New(slog.NewTextHandler(logs, nil)), Now: now})
	if err != nil {
		t.Fatalf("normalizedSpec: %v", err)
	}
	return consumerRun{spec: spec, repositories: spec.Repositories, pool: pool, maxAge: notificationStreamMaxAge, names: &issueNames{},
		remade: &remadeFailures{}, dropped: newDroppedSettlements()}
}

// notificationStreamMaxAge is how long production's notification stream keeps a message, its
// MaxAge: Envoy sets it to the stream's 72-hour duplicate window, streamDuplicateWindow in
// packages/envoy/internal/bus/stream.go, which this module cannot import. The daemon reads the
// stream's own MaxAge at OpenConsumers; the production schedule is held to this copy of it.
const notificationStreamMaxAge = 72 * time.Hour

// The delay before a failed message's next delivery doubles from NakDelay with each delivery and
// stops at NakDelayCap; no delivery count overflows it, and a count of 0, a delivery whose
// metadata did not parse, waits the cap.
func TestTheNakDelayDoublesFromNakDelayToItsCap(t *testing.T) {
	production, err := normalizedSpec(ConsumerSpec{Project: "CAPTURE", NakDelay: time.Second})
	if err != nil {
		t.Fatalf("normalizedSpec: %v", err)
	}
	if production.NakDelayCap != 5*time.Minute {
		t.Fatalf("the cap a spec that sets none gets = %s, want 5m", production.NakDelayCap)
	}
	short := ConsumerSpec{NakDelay: 20 * time.Millisecond, NakDelayCap: 160 * time.Millisecond}
	for _, row := range []struct {
		spec      ConsumerSpec
		delivered uint64
		want      time.Duration
	}{
		{production, 0, 5 * time.Minute},
		{production, 1, time.Second},
		{production, 2, 2 * time.Second},
		{production, 3, 4 * time.Second},
		{production, 4, 8 * time.Second},
		{production, 5, 16 * time.Second},
		{production, 6, 32 * time.Second},
		{production, 7, 64 * time.Second},
		{production, 8, 128 * time.Second},
		{production, 9, 256 * time.Second},
		{production, 10, 5 * time.Minute},
		{production, 11, 5 * time.Minute},
		{production, 12, 5 * time.Minute},
		{production, 35, 5 * time.Minute},
		{production, 40, 5 * time.Minute},
		{production, 10_000, 5 * time.Minute},
		{short, 0, 160 * time.Millisecond},
		{short, 1, 20 * time.Millisecond},
		{short, 2, 40 * time.Millisecond},
		{short, 3, 80 * time.Millisecond},
		{short, 4, 160 * time.Millisecond},
		{short, 5, 160 * time.Millisecond},
		{short, 12, 160 * time.Millisecond},
		{short, 35, 160 * time.Millisecond},
		{short, 40, 160 * time.Millisecond},
		{short, 10_000, 160 * time.Millisecond},
	} {
		if got := nakDelay(row.delivered, row.spec); got != row.want {
			t.Errorf("nakDelay(%d) at NakDelay %s and cap %s = %s, want %s", row.delivered, row.spec.NakDelay, row.spec.NakDelayCap, got, row.want)
		}
	}
}

// The bound, row by row: a decoded message is given up once JetStream has delivered it 21 times
// and its first failure is an hour old, decided after the failure; before its handler runs when
// the delivery before it did not end in a recorded failure; and at once when the stream would
// delete it before its next delivery, a delivery a daemon stop ended included, on a context the
// stop does not end. A stop alone gives nothing up by the count and the hour and writes no
// failure. The values are the production ones: count 21, hour 1 h, margin 1 min, the stream's
// 72 hours.
func TestTheGiveUpRule(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	consumer := dispatchConsumerName("CAPTURE")
	const subject = "notifications.dispatch.issue.CAPTURE-3.issue.updated"
	captured := capturedIssueUpdatedEnvelope(t)
	retained := notificationStreamMaxAge - 30*time.Second
	for i, row := range []struct {
		name      string
		delivered uint64
		// seeded is whether the deliveries before this one left an open failure: its first failure
		// firstAgo before now, and ended the last delivery that ended in a recorded failure.
		seeded   bool
		firstAgo time.Duration
		ended    int
		// storedAgo is how long before now the stream stored the message.
		storedAgo time.Duration
		// retryLater is the wait the handler's failure names, 0 for none.
		retryLater time.Duration
		// stop is a daemon stop ending the delivery: the consumer's context is done when the
		// handler returns.
		stop bool
		// succeeds is a handler that applies the fact; every other row's fails.
		succeeds bool

		handled bool
		nak     time.Duration
		reason  record.IntakeGiveUpReason
	}{
		{name: "a first failure", delivered: 1, handled: true, nak: time.Second},
		{name: "20 deliveries over two hours", delivered: 20, seeded: true, firstAgo: 2 * time.Hour, ended: 19, handled: true, nak: 5 * time.Minute},
		{name: "21 deliveries in 59 minutes", delivered: 21, seeded: true, firstAgo: 59 * time.Minute, ended: 20, handled: true, nak: 5 * time.Minute},
		{name: "21 deliveries in an hour", delivered: 21, seeded: true, firstAgo: time.Hour, ended: 20, handled: true, reason: record.GiveUpBound},
		{name: "30 deliveries in an hour", delivered: 30, seeded: true, firstAgo: time.Hour, ended: 29, handled: true, reason: record.GiveUpBound},
		{name: "a delivery that did not end past the bound", delivered: 22, seeded: true, firstAgo: 2 * time.Hour, ended: 20, reason: record.GiveUpDidNotEnd},
		{name: "a delivery that did not end inside the hour", delivered: 22, seeded: true, firstAgo: 30 * time.Minute, ended: 20, succeeds: true, handled: true},
		{name: "a delivery that did not end below the count", delivered: 3, seeded: true, firstAgo: 2 * time.Hour, ended: 1, succeeds: true, handled: true},
		{name: "a first failure the stream deletes before the next delivery", delivered: 1, storedAgo: retained, handled: true, reason: record.GiveUpRetention},
		{name: "a wait past the stream's age", delivered: 5, seeded: true, ended: 4, retryLater: 80 * time.Hour, handled: true, reason: record.GiveUpRetention},
		{name: "a stop at the bound", delivered: 21, seeded: true, firstAgo: time.Hour, ended: 20, stop: true, handled: true, nak: 5 * time.Minute},
		{name: "a stop the stream deletes before the next delivery", delivered: 1, storedAgo: retained, stop: true, handled: true, reason: record.GiveUpRetention},
	} {
		t.Run(row.name, func(t *testing.T) {
			seq := uint64(100 + i)
			stored := now.Add(-row.storedAgo)
			// Each row's fact is an event of its own, so the one that applies is no duplicate of
			// another row's.
			eventID := fmt.Sprintf("dispatch-rule-%d", i)
			data := envelopeWithID(t, captured, eventID)
			message := record.IntakeMessage{Project: "CAPTURE", Consumer: consumer, Subject: subject, StreamSeq: seq, StoredAt: stored,
				Payload: data, EventSource: "dispatch", EventID: eventID}
			if row.seeded {
				seedFailure(t, pool, message, row.ended, row.delivered-1, now.Add(-row.firstAgo))
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			handler := handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
				calls++
				if row.stop {
					cancel()
				}
				switch {
				case row.succeeds:
					return Result{}, nil
				case row.retryLater > 0:
					return Result{}, &RetryLater{After: row.retryLater, Err: errors.New("GitHub's rate limit")}
				}
				return Result{}, errors.New("the handler fails")
			})
			delivery := newDelivery(t, consumer, subject, data, seq, stored).deliveredAgain(row.delivered)
			consumeDelivery(ctx, delivery, productionRun(t, pool, &lockedBuffer{}, func() time.Time { return now }), handler)

			if handled := calls == 1; handled != row.handled {
				t.Errorf("the handler ran %d times, want it run: %t", calls, row.handled)
			}
			acks, terms, naks := delivery.asked()
			switch {
			case row.reason != "" && (terms != 1 || acks != 0 || len(naks) != 0):
				t.Errorf("asked %d acks, %d terms and naks %v of JetStream, want the message terminated", acks, terms, naks)
			case row.nak != 0 && (len(naks) != 1 || naks[0] != row.nak || acks != 0 || terms != 0):
				t.Errorf("asked %d acks, %d terms and naks %v of JetStream, want one nak of %s", acks, terms, naks, row.nak)
			case row.succeeds && (acks != 1 || terms != 0 || len(naks) != 0):
				t.Errorf("asked %d acks, %d terms and naks %v of JetStream, want the message acknowledged", acks, terms, naks)
			}
			got, found := intakeRecordOf(t, pool, consumer, seq)
			switch {
			case row.reason != "":
				if !found || !got.GivenUp() || got.GiveUpReason != row.reason || !got.GivenUpAt.Equal(now) {
					t.Fatalf("the record = %+v (found %t), want it given up at %s for %q", got, found, now, row.reason)
				}
			case row.succeeds:
				if found {
					t.Fatalf("the record = %+v, want the open failure deleted by the fact that applied", got)
				}
			case row.stop:
				if !found || got.GivenUp() || got.EndedDelivery != row.ended {
					t.Fatalf("the record = %+v (found %t), want the open failure as the deliveries before the stop left it, ended at %d", got, found, row.ended)
				}
			default:
				if !found || got.GivenUp() || got.Deliveries != int(row.delivered) || got.EndedDelivery != int(row.delivered) {
					t.Fatalf("the record = %+v (found %t), want an open failure ended at delivery %d", got, found, row.delivered)
				}
				if want := now.Add(-row.firstAgo); !got.FirstFailedAt.Equal(want) {
					t.Fatalf("the first failure = %s, want %s", got.FirstFailedAt, want)
				}
			}
		})
	}
}

// seedFailure writes the open failure the deliveries before this one left of message: its first
// failure at first, the last delivery that ended in a recorded failure ended, and delivered the
// count the last of them saw.
func seedFailure(t *testing.T, pool *pgxpool.Pool, message record.IntakeMessage, ended int, delivered uint64, first time.Time) {
	t.Helper()
	ctx := context.Background()
	records := record.NewStore()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := records.NoteFailure(ctx, tx, message, ended, "an earlier failure", first); err != nil {
			return err
		}
		if int(delivered) > ended {
			_, err := records.NoteRedelivery(ctx, tx, message, int(delivered), first.Add(time.Minute))
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the open failure of message %d: %v", message.StreamSeq, err)
	}
}

// envelopeWithID is the Envoy envelope data with its event id and dedupe key both id, which the
// fact it decodes to is applied under (envoyEnvelope.factID): the same event made another one.
func envelopeWithID(t *testing.T, data []byte, id string) []byte {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("read the envelope: %v", err)
	}
	envelope["event_id"], envelope["dedupe_key"] = id, id
	out, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("write the envelope: %v", err)
	}
	return out
}

// boundRun is productionRun with the bound at its least: the first failed delivery reaches the
// count, and no hour is waited. A test sets the zero hour after normalizedSpec, whose zero is the
// production hour.
func boundRun(t *testing.T, pool store.TxPool, logs *lockedBuffer, now func() time.Time) consumerRun {
	t.Helper()
	run := productionRun(t, pool, logs, now)
	run.spec.GiveUpDeliveries, run.spec.GiveUpAfter = 1, 0
	return run
}

// failingHandler is a handler whose every application fails, counting them in calls.
func failingHandler(calls *int) Handler {
	return handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
		*calls++
		return Result{}, errAlwaysFails
	})
}

// registerCaptureController records CAPTURE's controller as registered, as `legion controller
// start` and its session's registration leave the row ControllerRegistered reads.
func registerCaptureController(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	token, err := claim.ProjectToken("CAPTURE")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `insert into controllers (project, capability_hash, generation, session, secret_hash, registered_at)
		values ($1, $2, 1, 'ses-controller', $3, now())`, token, []byte("capability"), []byte("secret")); err != nil {
		t.Fatalf("register CAPTURE's controller: %v", err)
	}
}

// giveUpNotices is how many intake-gave-up notices the outbox holds and the count the highest
// of them carries.
func giveUpNotices(t *testing.T, pool *pgxpool.Pool) (notices, count int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `select count(*), coalesce(max((payload->>'count')::int), 0) from outbox
		where kind = $1 and payload->>'kind' = $2`, string(record.OutboxKindControllerNotice), string(record.IntakeGaveUpNotice)).
		Scan(&notices, &count); err != nil {
		t.Fatalf("read the intake-gave-up notices: %v", err)
	}
	return notices, count
}

// A RetryLater wait longer than the doubled nak delay takes its place, whole, with no cap; a
// shorter one leaves the doubled delay. The nak line names the delay and the delivery count.
func TestALongerRetryLaterWaitReplacesTheDoubledDelay(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	consumer := dispatchConsumerName("CAPTURE")
	captured := capturedIssueUpdatedEnvelope(t)
	for i, row := range []struct {
		name      string
		delivered uint64
		wait      time.Duration
		want      time.Duration
	}{
		{name: "a wait shorter than the doubled delay", delivered: 3, wait: time.Millisecond, want: 4 * time.Second},
		{name: "a wait longer than the doubled delay", delivered: 2, wait: 10 * time.Second, want: 10 * time.Second},
		{name: "a wait above the cap, kept whole", delivered: 12, wait: time.Hour, want: time.Hour},
	} {
		t.Run(row.name, func(t *testing.T) {
			logs := &lockedBuffer{}
			data := envelopeWithID(t, captured, fmt.Sprintf("dispatch-wait-%d", i))
			delivery := newDelivery(t, consumer, "notifications.dispatch.issue.CAPTURE-3.issue.updated", data, uint64(200+i), now).deliveredAgain(row.delivered)
			consumeDelivery(context.Background(), delivery, productionRun(t, pool, logs, func() time.Time { return now }),
				handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
					return Result{}, &RetryLater{After: row.wait, Err: errors.New("GitHub's rate limit")}
				}))
			if acks, terms, naks := delivery.asked(); acks != 0 || terms != 0 || !slices.Equal(naks, []time.Duration{row.want}) {
				t.Fatalf("asked %d acks, %d terms and naks %v of JetStream, want one nak of %s", acks, terms, naks, row.want)
			}
			requireLogLine(t, logs, "level=WARN", "retryable intake processing failure", "retry_in="+row.want.String(),
				"delivered="+strconv.FormatUint(row.delivered, 10)+" ")
		})
	}
}

// A message is terminated only once its give-up record has committed: the termination of each
// kind of give-up finds the record committed, read through a connection of its own.
func TestTheTermFollowsTheCommittedRecord(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	consumer := dispatchConsumerName("CAPTURE")
	captured := capturedIssueUpdatedEnvelope(t)
	for i, row := range []struct {
		name string
		data []byte
		// production runs at the production bound, which a first failure does not reach.
		production bool
		storedAgo  time.Duration
		// stopFirst ends the first delivery with a daemon stop, and the second is the one decided.
		stopFirst bool
		reason    record.IntakeGiveUpReason
	}{
		{name: "a give-up at the bound", reason: record.GiveUpBound},
		{name: "a give-up before the handler, after a delivery that did not end", stopFirst: true, reason: record.GiveUpDidNotEnd},
		{name: "a give-up before the stream deletes the message", production: true, storedAgo: notificationStreamMaxAge - 30*time.Second,
			reason: record.GiveUpRetention},
		{name: "a message that does not decode", data: []byte(`{`), reason: record.GiveUpUndecodable},
	} {
		t.Run(row.name, func(t *testing.T) {
			seq := uint64(300 + i)
			data := row.data
			if data == nil {
				data = envelopeWithID(t, captured, fmt.Sprintf("dispatch-term-%d", i))
			}
			clock := func() time.Time { return now }
			run := boundRun(t, pool, &lockedBuffer{}, clock)
			if row.production {
				run = productionRun(t, pool, &lockedBuffer{}, clock)
			}
			first := newDelivery(t, consumer, "notifications.dispatch.issue.CAPTURE-3.issue.updated", data, seq, now.Add(-row.storedAgo))
			delivery := first
			if row.stopFirst {
				ctx, cancel := context.WithCancel(context.Background())
				consumeDelivery(ctx, first, run, handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
					cancel()
					return Result{}, errors.New("the daemon stopped")
				}))
				delivery = first.deliveredAgain(2)
			}
			var committed bool
			var readErr error
			delivery.onTerm = func() { committed, readErr = committedGiveUp(context.Background(), pool, consumer, seq) }
			calls := 0
			consumeDelivery(context.Background(), delivery, run, failingHandler(&calls))
			if _, terms, _ := delivery.asked(); terms != 1 || readErr != nil || !committed {
				t.Fatalf("%d terminations, each finding a committed give-up: %t (%v); want one, after the commit", terms, committed, readErr)
			}
			if got, _ := intakeRecordOf(t, pool, consumer, seq); got.GiveUpReason != row.reason {
				t.Fatalf("the record = %+v, want it given up for %q", got, row.reason)
			}
		})
	}
}

// A termination that fails leaves a committed give-up: the message comes back, and its next
// delivery is terminated with no handling, no second record and no second count in the
// controller's notice. A consumer made again at an older stored time delivers it once more with a
// count of 1: it fails, and is terminated with its record left as it was.
func TestAMessageWhoseTermFailedComesBackAndIsTerminatedWithNoSecondRecord(t *testing.T) {
	pool := migratedPool(t)
	registerCaptureController(t, pool)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	consumer := dispatchConsumerName("CAPTURE")
	run := boundRun(t, pool, &lockedBuffer{}, func() time.Time { return now })
	calls := 0
	handler := failingHandler(&calls)
	first := newDelivery(t, consumer, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t), 400, now.Add(-time.Minute))
	first.termFails = 1
	if consumeDelivery(context.Background(), first, run, handler) {
		t.Fatal("a delivery whose termination failed was reported handled")
	}
	records := intakeRecords(t, pool)
	if len(records) != 1 || !records[0].GivenUp() {
		t.Fatalf("intake_failures after the failed termination = %+v, want the one give-up", records)
	}
	given := records[0]
	if notices, count := giveUpNotices(t, pool); notices != 1 || count != 1 {
		t.Fatalf("the outbox holds %d intake-gave-up notices counting %d, want one counting 1", notices, count)
	}

	for _, delivery := range []struct {
		name  string
		again *fakeDelivery
		calls int
	}{
		{name: "the next delivery", again: first.deliveredAgain(2), calls: 1},
		{name: "a consumer made again at an older stored time", again: first.deliveredAgain(1), calls: 2},
	} {
		consumeDelivery(context.Background(), delivery.again, run, handler)
		if acks, terms, naks := delivery.again.asked(); acks != 0 || terms != 1 || len(naks) != 0 || calls != delivery.calls {
			t.Fatalf("%s: asked %d acks, %d terms and naks %v after %d handler calls, want it terminated after %d", delivery.name, acks, terms, naks, calls, delivery.calls)
		}
		if records := intakeRecords(t, pool); len(records) != 1 || !reflect.DeepEqual(records[0], given) {
			t.Fatalf("%s: intake_failures = %+v, want the one give-up as it was: %+v", delivery.name, records, given)
		}
		if notices, count := giveUpNotices(t, pool); notices != 1 || count != 1 {
			t.Fatalf("%s: the outbox holds %d intake-gave-up notices counting %d, want one counting 1", delivery.name, notices, count)
		}
	}
}

// While PostgreSQL refuses the record, a failing message is naked at the delay its count earns and
// never terminated, a message that does not decode as well; once the record can be written, a
// later delivery records it and terminates it.
func TestWhileTheRecordCannotBeWrittenTheMessageIsNakedAndRecordedLater(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	consumer := dispatchConsumerName("CAPTURE")
	for _, row := range []struct {
		name string
		data []byte
	}{
		{name: "a decoded message whose handler fails", data: capturedIssueUpdatedEnvelope(t)},
		{name: "a message that does not decode", data: []byte(`{`)},
	} {
		t.Run(row.name, func(t *testing.T) {
			raw := migratedPool(t)
			pool := &failingRecordPool{TxPool: raw}
			pool.refuse.Store(true)
			run := boundRun(t, pool, &lockedBuffer{}, func() time.Time { return now })
			calls := 0
			first := newDelivery(t, consumer, "notifications.dispatch.issue.CAPTURE-3.issue.updated", row.data, 500, now.Add(-time.Minute))
			for delivered := uint64(1); delivered <= 2; delivered++ {
				delivery := first.deliveredAgain(delivered)
				consumeDelivery(context.Background(), delivery, run, failingHandler(&calls))
				if acks, terms, naks := delivery.asked(); acks != 0 || terms != 0 || !slices.Equal(naks, []time.Duration{nakDelay(delivered, run.spec)}) {
					t.Fatalf("delivery %d while the record is refused: asked %d acks, %d terms and naks %v, want one nak of %s",
						delivered, acks, terms, naks, nakDelay(delivered, run.spec))
				}
			}
			if pool.refused.Load() == 0 {
				t.Fatal("no statement on intake_failures was refused")
			}
			if records := intakeRecords(t, raw); len(records) != 0 {
				t.Fatalf("intake_failures while the record is refused = %+v, want none", records)
			}

			pool.refuse.Store(false)
			later := first.deliveredAgain(3)
			var committed bool
			var readErr error
			later.onTerm = func() { committed, readErr = committedGiveUp(context.Background(), raw, consumer, 500) }
			consumeDelivery(context.Background(), later, run, failingHandler(&calls))
			if _, terms, _ := later.asked(); terms != 1 || readErr != nil || !committed {
				t.Fatalf("the delivery after the refusal ended: %d terminations, finding a committed give-up: %t (%v); want it recorded, then terminated", terms, committed, readErr)
			}
		})
	}
}

// A give-up whose commit fails takes back the failure its transaction recorded, so the failure is
// recorded again, alone, before the nak: the open failure ends at that delivery with its error,
// and the next delivery runs its handler, rather than giving the message up as a delivery that
// did not end, with a stop or a crash for its cause that never happened.
func TestAGiveUpWhoseCommitFailsStillRecordsTheDeliverysFailure(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	consumer := dispatchConsumerName("CAPTURE")
	raw := migratedPool(t)
	pool := &failingRecordPool{TxPool: raw}
	run := boundRun(t, pool, &lockedBuffer{}, func() time.Time { return now })
	calls := 0
	first := newDelivery(t, consumer, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t), 800, now.Add(-time.Minute))
	pool.refuseNextCommit.Store(true)
	consumeDelivery(context.Background(), first, run, failingHandler(&calls))
	if acks, terms, naks := first.asked(); acks != 0 || terms != 0 || !slices.Equal(naks, []time.Duration{nakDelay(1, run.spec)}) {
		t.Fatalf("the delivery whose give-up was refused: asked %d acks, %d terms and naks %v, want one nak of %s", acks, terms, naks, nakDelay(1, run.spec))
	}
	if refused := pool.commitsRefused.Load(); refused != 1 {
		t.Fatalf("%d commits were refused, want the give-up's alone", refused)
	}
	open, found := intakeRecordOf(t, raw, consumer, 800)
	if !found || open.GivenUp() || open.Deliveries != 1 || open.EndedDelivery != 1 || open.LastError != errAlwaysFails.Error() {
		t.Fatalf("the record after the refused give-up = %+v (found %t), want an open failure ended at delivery 1 with the handler's error", open, found)
	}

	again := first.deliveredAgain(2)
	consumeDelivery(context.Background(), again, run, failingHandler(&calls))
	if calls != 2 {
		t.Fatalf("the handler ran %d times, want it run at the next delivery too", calls)
	}
	given, _ := intakeRecordOf(t, raw, consumer, 800)
	if _, terms, _ := again.asked(); terms != 1 || given.GiveUpReason != record.GiveUpBound || given.LastError != errAlwaysFails.Error() {
		t.Fatalf("the next delivery made %d terminations and left %+v, want the message given up at the bound with the handler's error", terms, given)
	}
}

// A give-up is logged once, at error, with the delivery count; a lost termination's next delivery
// logs none. An error longer than the record keeps is in the log whole, in storetext.LogWhole's
// parts, while the give-up line carries it as the record does.
func TestAGiveUpIsLoggedOnceAtError(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	logs := &lockedBuffer{}
	run := boundRun(t, pool, logs, func() time.Time { return now })
	long := strings.Repeat("the database refused the write; ", 100)
	first := newDelivery(t, dispatchConsumerName("CAPTURE"), "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t), 600, now.Add(-time.Minute))
	first.termFails = 1
	handler := handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) { return Result{}, errors.New(long) })
	consumeDelivery(context.Background(), first, run, handler)
	consumeDelivery(context.Background(), first.deliveredAgain(2), run, handler)

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	var giveUps, wholes []string
	for _, line := range lines {
		switch {
		case strings.Contains(line, "intake: a message was given up"):
			giveUps = append(giveUps, line)
		case strings.Contains(line, "error in full"):
			wholes = append(wholes, line)
		}
	}
	if len(giveUps) != 1 || !strings.Contains(giveUps[0], "level=ERROR") || !strings.Contains(giveUps[0], "delivered=1 ") ||
		!strings.Contains(giveUps[0], "…") || strings.Contains(giveUps[0], long) {
		t.Fatalf("give-up lines = %q, want one at ERROR at delivery 1 with the error as the record cuts it", giveUps)
	}
	if len(wholes) != 1 || !strings.Contains(wholes[0], long) || !strings.Contains(wholes[0], "part=1 parts=1") {
		t.Fatalf("lines of the whole error = %q, want one part holding all %d characters", wholes, len(long))
	}
}

// The production values give a failing message up before the stream deletes it: with the
// daemon's NakDelay of 1 s and the defaults normalizedSpec gives the rest of the bound, the 20
// delays before the 21st delivery are 3,811 s, so the count and the hour meet there; and 21
// deliveries each held for the longest handler limit, the delays and the hour stay under the
// stream's 72 hours less the margin.
func TestTheProductionScheduleGivesUpBeforeTheStreamDeletesTheMessage(t *testing.T) {
	spec, err := normalizedSpec(ConsumerSpec{Project: "CAPTURE", NakDelay: time.Second})
	if err != nil {
		t.Fatalf("normalizedSpec: %v", err)
	}
	if spec.GiveUpDeliveries != 21 || spec.GiveUpAfter != time.Hour || spec.NakDelayCap != 5*time.Minute || spec.RetentionMargin != time.Minute {
		t.Fatalf("the defaults = count %d, hour %s, cap %s, margin %s; want 21, 1h, 5m and 1m", spec.GiveUpDeliveries, spec.GiveUpAfter, spec.NakDelayCap, spec.RetentionMargin)
	}
	var delays time.Duration
	for delivered := uint64(1); delivered < uint64(spec.GiveUpDeliveries); delivered++ {
		delays += nakDelay(delivered, spec)
	}
	if delays != 3811*time.Second {
		t.Fatalf("the delays before delivery %d add up to %s, want 3811s", spec.GiveUpDeliveries, delays)
	}
	if delays < spec.GiveUpAfter {
		t.Fatalf("delivery %d comes %s after the first, before the hour: the count and the hour do not meet there", spec.GiveUpDeliveries, delays)
	}
	if longest := time.Duration(spec.GiveUpDeliveries)*HandlerLimitCeiling + delays + spec.GiveUpAfter; longest >= notificationStreamMaxAge-spec.RetentionMargin {
		t.Fatalf("the longest a failing message waits for its give-up is %s, want under the stream's %s less %s", longest, notificationStreamMaxAge, spec.RetentionMargin)
	}
}

// Another project's issue event is no fact, decided from its subject before the envelope is read,
// so an envelope that is not JSON is no error of this daemon's in either form of the name. The
// daemon's own event that does not decode is an error, and so is a name in neither form, which no
// project's key reads (errIssueNameInNeitherForm).
func TestAnotherProjectsIssueEventIsNoFactBeforeItsPayloadIsRead(t *testing.T) {
	captured := capturedIssueUpdatedEnvelope(t)
	for _, row := range []struct {
		subject string
		data    []byte
		// fails is a decode that answers an error; neither, one that names a name in neither form.
		fails, neither bool
	}{
		{subject: "notifications.dispatch.issue.LEGION-7.issue.updated", data: []byte(`{`)},
		{subject: "notifications.dispatch.issue.LEGION.LEGION-7.issue.updated", data: []byte(`{`)},
		{subject: "notifications.dispatch.issue.CAPTURE-7.issue.updated", data: []byte(`{`), fails: true},
		{subject: "notifications.dispatch.issue.CAPTURE.issue.updated", data: []byte(`{`), fails: true},
		{subject: "notifications.dispatch.issue.CAPTURE.issue.updated", data: captured, fails: true, neither: true},
	} {
		decoded, err := decodeMessage(row.subject, "CAPTURE", capturedRepositories, row.data)
		switch {
		case !row.fails && (err != nil || decoded.Fact != nil):
			t.Errorf("decode %s of %q = %#v, %v; want no fact and no error", row.subject, row.data, decoded.Fact, err)
		case row.fails && err == nil:
			t.Errorf("decode %s of %q = %#v; want an error", row.subject, row.data, decoded.Fact)
		case row.neither && !errors.Is(err, errIssueNameInNeitherForm):
			t.Errorf("decode %s = %v; want a name in neither form", row.subject, err)
		}
	}
}
