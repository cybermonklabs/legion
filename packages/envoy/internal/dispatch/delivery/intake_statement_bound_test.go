package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Every statement of the intake on pull_request_reads_owed or on its revision sequence waits on a
// lock no longer than the owe does (owedReadWriteTimeout), so a lock of that table, as a migration
// of it holds one, holds a delivery, and one of the intakeMaxAckPending slots its repository
// shares, for seconds and not for the message's whole deadline. The three tests below are the
// statements the reviews of cybermonklabs/legion#4209 found with no bound of their own: the settle
// of the read owed before the intake's read and the head write, whose trigger owes a read where two
// answers of one GitHub second differ (at ac562e9837, N1), and the read's place, a value of the
// table's sequence (at 5ea6b2ffb2, N1).

// The settle. #4299 is owed a read; the read of its event is held, the owed table is locked, and
// the read is released: the head is written, and the settle waits on the lock. The message's
// deadline is 20 s. The test times the settle's statement on the intake's pool and holds it to the
// bound and statementWaitMargin, as the head write's and the place's tests hold theirs, and the
// delivery to the bound and 5 s, far below that deadline.
func TestTheSettleOfAnOwedReadWaitsOnALockNoLongerThanTheOwe(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	shrinkOwedReadWriteTimeout(t)
	tuningFor(t).messageTimeout = 20 * time.Second
	const number = 4299
	const head, branch = "4299429942994299429942994299429942994299", "feature/settle"
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	github.set("acme/widgets", number, "open", head, branch, "", at)
	if err := unrenamed().owePullRequestRead(ctx, pool, "acme/widgets", number, owedFailure, ""); err != nil {
		t.Fatal(err)
	}
	reading, answer := make(chan struct{}), make(chan struct{})
	github.holdFirst = func() {
		close(reading)
		<-answer
	}
	settles := &statementWaitTracer{match: "delete from pull_request_reads_owed"}
	intake := newIntake(tuningFor(t), nil, storetest.Traced(t, pool, settles, nil), nil, fake.newTestClient(), nil, newSettingsCache(pool, nil, tuningFor(t).log), unrenamed())
	payload, err := json.Marshal(prEnvelope("acme/widgets", number, "synchronize", head, branch, "", at))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan time.Time, 1)
	go func() {
		intake.deliver(deliveryWithNoNATS(ctx), intakeMessage{
			msg:      &natsgo.Msg{Subject: "notifications.github.acme.widgets.pr." + strconv.Itoa(number)},
			envelope: &contracts.Envelope{Payload: string(payload)},
		})
		done <- time.Now()
	}()
	waitOn(t, reading, "the event's read of GitHub")
	release := lockOwedReads(t, ctx, pool)
	defer release()
	answered := time.Now()
	close(answer)
	var ended time.Time
	select {
	case ended = <-done:
	case <-time.After(time.Minute):
		t.Fatal("the delivery did not end in a minute")
	}
	took := ended.Sub(answered)
	release()
	heads := headRowCount(t, ctx, intake, "acme/widgets", number)
	settled := settles.first()
	t.Logf("delivery_after_answer=%s settle_waited=%s head_rows=%d message_timeout=%s owe_bound=%s", took.Round(time.Millisecond), settled.Round(time.Microsecond), heads, tuningFor(t).messageTimeout, owedReadWriteTimeout)
	if heads != 1 {
		t.Errorf("#%d has %d head rows after its delivery, want 1: the head was written before the settle", number, heads)
	}
	if settled == 0 {
		t.Fatalf("no settle of #%d's owed read was timed on the intake's pool: the case did not happen", number)
	}
	if settled > owedReadWriteTimeout+statementWaitMargin {
		t.Errorf("the settle of the owed read waited %s on the lock of pull_request_reads_owed, want no longer than the owe's bound (%s) and %s",
			settled.Round(time.Millisecond), owedReadWriteTimeout, statementWaitMargin)
	}
	if took > owedReadWriteTimeout+5*time.Second {
		t.Errorf("the delivery ended %s after GitHub answered, while pull_request_reads_owed was locked; want within the owe's bound (%s) and 5 s: the settle of the owed read waits on the message's deadline (%s)",
			took.Round(time.Millisecond), owedReadWriteTimeout, tuningFor(t).messageTimeout)
	}
}

// The head write. #4298's row holds head A of second S, read by an earlier read; GitHub holds B
// in the same second, so the write of B owes a read in its own statement (the trigger
// pull_request_heads_older_read), and that owe waits on a lock of the owed table. The write must
// give up within the owe's bound, the delivery hands the envelope back, and once the lock ends
// the redelivery stores B. The control holds no lock: B is stored at the first delivery. The
// message's deadline is ten minutes, so a write that waits on it writes no hand-back line in the
// minute the test waits. The test times the write's statement on the intake's pool and holds it
// to the bound and statementWaitMargin.
func TestTheHeadWriteOfTheIntakeWaitsOnALockNoLongerThanTheOwe(t *testing.T) {
	for _, locked := range []bool{false, true} {
		t.Run(map[bool]string{false: "owed_table_free", true: "owed_table_locked"}[locked], func(t *testing.T) {
			fastPageRetries(t)
			logs := captureLogs(t)
			pool, ctx := deliveryTestPool(t)
			shrinkOwedReadWriteTimeout(t)
			tuningFor(t).messageTimeout = 10 * time.Minute
			const number = 4298
			older, newer := fakeSHA("a", number), fakeSHA("b", number)
			second := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			github.set("acme/widgets", number, "open", older, "feature/x", "", second)
			client := fake.newTestClient()
			if _, err := newIntake(tuningFor(t), nil, pool, nil, client, nil, nil, unrenamed()).handlePullRequestEnvelope(ctx, nil,
				prEnvelope("acme/widgets", number, "opened", older, "feature/x", "", second)); err != nil {
				t.Fatalf("store head A: %v", err)
			}
			github.set("acme/widgets", number, "open", newer, "feature/x", "", second)
			release := func() {}
			if locked {
				release = lockOwedReads(t, ctx, pool)
			}
			natsClient := intakeTestClient(t)
			writes := &statementWaitTracer{match: "insert into pull_request_heads"}
			tuningFor(t).redeliverAfter = testRedeliverAfter
			intake := newIntake(tuningFor(t), natsClient, storetest.Traced(t, pool, writes, nil), deliveryTestLeases(t, pool), client, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
			startIntake(t, intake)
			awaitBoundDurable(t, natsClient)
			published := time.Now()
			publishPullRequestEnvelope(t, natsClient, "acme", "widgets", number,
				prEnvelope("acme/widgets", number, "synchronize", newer, "feature/x", "", second))
			if locked {
				var line map[string]any
				waitFor(t, time.Minute, func() bool {
					for _, record := range logs() {
						if message, _ := record["msg"].(string); record["level"] == "WARN" && strings.Contains(message, "handing the envelope back") {
							line = record
							return true
						}
					}
					return false
				}, func() string {
					return fmt.Sprintf("no hand-back line in the minute after the event of #%d, while pull_request_reads_owed is locked: the head write waits on the message's deadline (%s)", number, intakeMessageTimeout)
				})
				took := time.Since(published)
				cause, _ := line["error"].(string)
				waited := writes.first()
				t.Logf("hand_back_after=%s head_write_waited=%s error=%q", took.Round(time.Millisecond), waited.Round(time.Microsecond), cause)
				if waited == 0 {
					t.Fatalf("no head write of #%d was timed on the intake's pool: the case did not happen", number)
				}
				if waited > owedReadWriteTimeout+statementWaitMargin {
					t.Errorf("the head write waited %s on the lock of pull_request_reads_owed, want no longer than the owe's bound (%s) and %s",
						waited.Round(time.Millisecond), owedReadWriteTimeout, statementWaitMargin)
				}
				if !errors.Is(errors.New(cause), ErrHeadNotStored) && !strings.Contains(cause, ErrHeadNotStored.Error()) {
					t.Errorf("the hand-back line's error %q, want the head not stored", cause)
				}
				release()
			}
			awaitAcknowledged(t, natsClient)
			got := readHead(t, unrenamed(), ctx, pool, "acme/widgets", number)
			t.Logf("locked=%v head=%.12s owed=%d took=%s", locked, got.HeadSHA, owedCount(t, ctx, pool, "acme/widgets", number), time.Since(published).Round(time.Millisecond))
			if got.HeadSHA != newer {
				t.Errorf("#%d holds %.12s once its envelope is acknowledged, want GitHub's B, %.12s", number, got.HeadSHA, newer)
			}
		})
	}
}

// The read's place. Each read of GitHub takes its place in the order of reads with one statement
// on the sequence of the owed table's revisions (startRead), and a migration that alters that
// sequence, or sets the table logged or unlogged, holds the sequence's lock until it commits.
// #4297's event is delivered while another session holds that lock: the place's statement must
// give up within the owe's bound, the read of GitHub does not start, and the delivery hands the
// envelope back once the owe, whose revision default waits on the same lock, has given up within
// its own; once the lock ends, the next delivery stores the head. The control holds no lock: the
// head is stored at the first delivery. The message's deadline is ten minutes, so a place that
// waits on it holds the delivery past the minute the test waits. The test times the place's
// statement on the intake's pool and holds it to the bound and statementWaitMargin, and the
// delivery to the bounds of the statements it waits on: the read of the owed revision (a lock of
// the table holds it), the place and the owe.
func TestTheReadsPlaceAtTheIntakeWaitsOnALockNoLongerThanTheOwe(t *testing.T) {
	for _, hold := range []struct{ name, statement string }{
		{"owed_sequence_free", ""},
		{"owed_sequence_altered", "alter sequence pull_request_reads_owed_revision_seq cache 1"},
		{"owed_table_set_unlogged", "alter table pull_request_reads_owed set unlogged"},
	} {
		t.Run(hold.name, func(t *testing.T) {
			fastPageRetries(t)
			logs := captureLogs(t)
			pool, ctx := deliveryTestPool(t)
			shrinkOwedReadWriteTimeout(t)
			tuningFor(t).messageTimeout = 10 * time.Minute
			const number = 4297
			head, branch := fakeSHA("c", number), "feature/place"
			at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			github.set("acme/widgets", number, "open", head, branch, "", at)
			places := &statementWaitTracer{match: "nextval('pull_request_reads_owed_revision_seq')"}
			intake := newIntake(tuningFor(t), nil, storetest.Traced(t, pool, places, nil), nil, fake.newTestClient(), nil, newSettingsCache(pool, nil, tuningFor(t).log), unrenamed())
			payload, err := json.Marshal(prEnvelope("acme/widgets", number, "synchronize", head, branch, "", at))
			if err != nil {
				t.Fatal(err)
			}
			deliver := func() <-chan time.Duration {
				done := make(chan time.Duration, 1)
				began := time.Now()
				go func() {
					intake.deliver(deliveryWithNoNATS(ctx), intakeMessage{
						msg:      &natsgo.Msg{Subject: "notifications.github.acme.widgets.pr." + strconv.Itoa(number)},
						envelope: &contracts.Envelope{Payload: string(payload)},
					})
					done <- time.Since(began)
				}()
				return done
			}
			ended := func(done <-chan time.Duration) time.Duration {
				t.Helper()
				select {
				case took := <-done:
					return took
				case <-time.After(time.Minute):
					t.Fatalf("the delivery of #%d did not end in a minute", number)
					return 0
				}
			}
			if hold.statement == "" {
				took := ended(deliver())
				heads := headRowCount(t, ctx, intake, "acme/widgets", number)
				t.Logf("delivery_took=%s place_waited=%s github_reads=%d head_rows=%d", took.Round(time.Millisecond), places.first().Round(time.Microsecond), github.reads.Load(), heads)
				if places.first() == 0 {
					t.Fatalf("no read's place of #%d was timed on the intake's pool: the case did not happen", number)
				}
				if heads != 1 {
					t.Errorf("#%d has %d head rows after its delivery with no lock held, want 1", number, heads)
				}
				return
			}
			release := holdOwedReads(t, ctx, pool, hold.statement)
			done := deliver()
			var took time.Duration
			ranOut := false
			select {
			case took = <-done:
			case <-time.After(time.Minute):
				ranOut = true
				t.Errorf("the delivery of #%d did not end in a minute while another session held %q, want within the bounds of the read's place and the owe (%s each): the place waits on the message's deadline (%s)",
					number, hold.statement, owedReadWriteTimeout, tuningFor(t).messageTimeout)
			}
			// Counted before the hold ends: a delivery still under the hold has made every read of
			// GitHub it makes under it, and one that ends after the hold could make another.
			readsDuringHold := github.reads.Load()
			release()
			if ranOut {
				took = ended(done)
			}
			waited := places.first()
			var cause string
			for _, record := range logs() {
				if message, _ := record["msg"].(string); record["level"] == "WARN" && strings.Contains(message, "handing the envelope back") {
					cause, _ = record["error"].(string)
				}
			}
			redelivered := ended(deliver())
			heads := headRowCount(t, ctx, intake, "acme/widgets", number)
			t.Logf("delivery_took=%s place_waited=%s github_reads_during_hold=%d hand_back_error=%q redelivery_took=%s head_rows=%d",
				took.Round(time.Millisecond), waited.Round(time.Microsecond), readsDuringHold, cause, redelivered.Round(time.Millisecond), heads)
			if waited == 0 {
				t.Fatalf("no read's place of #%d was timed on the intake's pool: the case did not happen", number)
			}
			if waited > owedReadWriteTimeout+statementWaitMargin {
				t.Errorf("the read's place waited %s on the lock %q holds, want no longer than the owe's bound (%s) and %s",
					waited.Round(time.Millisecond), hold.statement, owedReadWriteTimeout, statementWaitMargin)
			}
			if took > owedRevisionReadTimeout+2*owedReadWriteTimeout+5*time.Second {
				t.Errorf("the delivery took %s while another session held %q, want within the bounds of the owed revision's read (%s), the read's place and the owe (%s each), and 5 s",
					took.Round(time.Millisecond), hold.statement, owedRevisionReadTimeout, owedReadWriteTimeout)
			}
			if readsDuringHold != 0 {
				t.Errorf("the delivery read GitHub %d times while its place could not be taken, want none: the read does not start", readsDuringHold)
			}
			if !strings.Contains(cause, "take the place of a read of GitHub") {
				t.Errorf("the hand-back line's error %q, want the read's place that was not taken", cause)
			}
			if heads != 1 {
				t.Errorf("#%d has %d head rows after the lock ended and the envelope was delivered again, want 1", number, heads)
			}
		})
	}
}

// statementWaitMargin is how far past owedReadWriteTimeout a statement of the intake may end: a
// statement whose context ran out ends once PostgreSQL has acted on its cancel. On this machine, in
// 20 runs at a load of 230 to 404 on 288 CPUs, 10 of them under -race, the head write ended at most
// 1.4 ms past its bound; the margin is for a scheduler that stalls a loaded machine's process, and
// it is far below the 15 s a write held to three times the bound waits.
const statementWaitMargin = time.Second

// statementWaitTracer times each statement whose SQL holds match on the pool it traces, from the
// moment pgx sends it to its answer.
type statementWaitTracer struct {
	match string
	mu    sync.Mutex
	waits []time.Duration
}

type statementStartKey struct{}

func (h *statementWaitTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, h.match) {
		return context.WithValue(ctx, statementStartKey{}, time.Now())
	}
	return ctx
}

func (h *statementWaitTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	began, ok := ctx.Value(statementStartKey{}).(time.Time)
	if !ok {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.waits = append(h.waits, time.Since(began))
}

// first is how long the first such statement waited, 0 where none was made.
func (h *statementWaitTracer) first() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.waits) == 0 {
		return 0
	}
	return h.waits[0]
}
