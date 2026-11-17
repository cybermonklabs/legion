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

// Every statement of the intake on pull_request_reads_owed waits on a lock no longer than the
// owe does (owedReadWriteTimeout), so a lock of that table, as a migration of it holds one, holds a
// delivery, and one of the intakeMaxAckPending slots its repository shares, for seconds and not
// for the message's whole deadline. The two tests below are the two statements the review of
// cybermonklabs/legion#4209 at ac562e9837 found with no bound of their own (N1): the settle of the
// read owed before the intake's read, and the head write, whose trigger owes a read where two
// answers of one GitHub second differ.

// The settle. #4299 is owed a read; the read of its event is held, the owed table is locked, and
// the read is released: the head is written, and the settle waits on the lock. The message's
// deadline is 20 s.
func TestTheSettleOfAnOwedReadWaitsOnALockNoLongerThanTheOwe(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	messageTimeout := intakeMessageTimeout
	intakeMessageTimeout = 20 * time.Second
	t.Cleanup(func() { intakeMessageTimeout = messageTimeout })
	const number = 4299
	const head, branch = "4299429942994299429942994299429942994299", "feature/settle"
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	github.set("acme/widgets", number, "open", head, branch, "", at)
	if err := owePullRequestRead(ctx, pool, "acme/widgets", number, owedFailure); err != nil {
		t.Fatal(err)
	}
	reading, answer := make(chan struct{}), make(chan struct{})
	github.holdFirst = func() {
		close(reading)
		<-answer
	}
	intake := newIntake(nil, pool, nil, fake.newTestClient(), nil, newSettingsCache(pool, nil))
	payload, err := json.Marshal(prEnvelope("acme/widgets", number, "synchronize", head, branch, "", at))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan time.Time, 1)
	go func() {
		intake.deliver(&intakeDelivery{handling: ctx}, intakeMessage{
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
	t.Logf("delivery_after_answer=%s head_rows=%d message_timeout=%s owe_bound=%s", took.Round(time.Millisecond), heads, intakeMessageTimeout, owedReadWriteTimeout)
	if heads != 1 {
		t.Errorf("#%d has %d head rows after its delivery, want 1: the head was written before the settle", number, heads)
	}
	if took > owedReadWriteTimeout+5*time.Second {
		t.Errorf("the delivery ended %s after GitHub answered, while pull_request_reads_owed was locked; want within the owe's bound (%s) and 5 s: the settle of the owed read waits on the message's deadline (%s)",
			took.Round(time.Millisecond), owedReadWriteTimeout, intakeMessageTimeout)
	}
}

// The head write. #4298's row holds head A of second S, read by an earlier read; GitHub holds B
// in the same second, so the write of B owes a read in its own statement (the trigger
// pull_request_heads_older_read), and that owe waits on a lock of the owed table. The write must
// give up within the owe's bound, the delivery hands the envelope back, and once the lock ends
// the redelivery stores B. The control holds no lock: B is stored at the first delivery. The
// message's deadline is ten minutes, so a write that waits on it writes no hand-back line in the
// minute the test waits. The test times the write's statement on the intake's pool and holds it
// to the bound and headWriteWaitMargin.
func TestTheHeadWriteOfTheIntakeWaitsOnALockNoLongerThanTheOwe(t *testing.T) {
	for _, locked := range []bool{false, true} {
		t.Run(map[bool]string{false: "owed_table_free", true: "owed_table_locked"}[locked], func(t *testing.T) {
			fastPageRetries(t)
			logs := captureLogs(t)
			pool, ctx := deliveryTestPool(t)
			messageTimeout := intakeMessageTimeout
			intakeMessageTimeout = 10 * time.Minute
			t.Cleanup(func() { intakeMessageTimeout = messageTimeout })
			const number = 4298
			older, newer := fakeSHA("a", number), fakeSHA("b", number)
			second := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			github.set("acme/widgets", number, "open", older, "feature/x", "", second)
			client := fake.newTestClient()
			if _, err := newIntake(nil, pool, nil, client, nil, nil).handlePullRequestEnvelope(ctx, nil,
				prEnvelope("acme/widgets", number, "opened", older, "feature/x", "", second)); err != nil {
				t.Fatalf("store head A: %v", err)
			}
			github.set("acme/widgets", number, "open", newer, "feature/x", "", second)
			release := func() {}
			if locked {
				release = lockOwedReads(t, ctx, pool)
			}
			natsClient := intakeTestClient(t)
			writes := &headWriteWaitTracer{}
			intake := newIntake(natsClient, storetest.Traced(t, pool, writes, nil), deliveryTestLeases(t, pool), client, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
			intake.redeliverAfter = time.Second
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
				if waited > owedReadWriteTimeout+headWriteWaitMargin {
					t.Errorf("the head write waited %s on the lock of pull_request_reads_owed, want no longer than the owe's bound (%s) and %s",
						waited.Round(time.Millisecond), owedReadWriteTimeout, headWriteWaitMargin)
				}
				if !errors.Is(errors.New(cause), ErrHeadNotStored) && !strings.Contains(cause, ErrHeadNotStored.Error()) {
					t.Errorf("the hand-back line's error %q, want the head not stored", cause)
				}
				release()
			}
			awaitAcknowledged(t, natsClient)
			got := readHead(t, ctx, pool, "acme/widgets", number)
			t.Logf("locked=%v head=%.12s owed=%d took=%s", locked, got.HeadSHA, owedCount(t, ctx, pool, "acme/widgets", number), time.Since(published).Round(time.Millisecond))
			if got.HeadSHA != newer {
				t.Errorf("#%d holds %.12s once its envelope is acknowledged, want GitHub's B, %.12s", number, got.HeadSHA, newer)
			}
		})
	}
}

// headWriteWaitMargin is how far past owedReadWriteTimeout the head write's statement may end: a
// statement whose context ran out ends once PostgreSQL has acted on its cancel. On this machine, in
// 20 runs at a load of 230 to 404 on 288 CPUs, 10 of them under -race, the statement ended at most
// 1.4 ms past its bound; the margin is for a scheduler that stalls a loaded machine's process, and
// it is far below the 15 s a write held to three times the bound waits.
const headWriteWaitMargin = time.Second

// headWriteWaitTracer times the head write's upsert statement on the pool it traces, from the
// moment pgx sends it to its answer.
type headWriteWaitTracer struct {
	mu    sync.Mutex
	waits []time.Duration
}

type headWriteStartKey struct{}

func (h *headWriteWaitTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "insert into pull_request_heads") {
		return context.WithValue(ctx, headWriteStartKey{}, time.Now())
	}
	return ctx
}

func (h *headWriteWaitTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	began, ok := ctx.Value(headWriteStartKey{}).(time.Time)
	if !ok {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.waits = append(h.waits, time.Since(began))
}

// first is how long the first head write waited, 0 where none was made.
func (h *headWriteWaitTracer) first() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.waits) == 0 {
		return 0
	}
	return h.waits[0]
}
