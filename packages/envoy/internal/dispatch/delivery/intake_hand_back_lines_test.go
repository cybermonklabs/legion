package delivery

// A handed-back envelope is delivered every headWriteRedeliverAfter for as long as its fault
// lasts, up to the 72 hours of the bus, so a line written at each of its deliveries is a line
// every 10 seconds for each of up to intakeMaxAckPending envelopes. The hand-back's own line is
// written at WARN at the first delivery and once every handBackLineEvery after it, and at DEBUG at
// every other (logHandBack); every other line such a delivery writes keeps the same cadence, and
// the lines of a delivery that ends any other way keep their own levels (Intake.deliver,
// deliveryLines). At def907052c the read of the owed read's revision wrote a WARN line at every
// delivery while pull_request_reads_owed was locked: 22 WARN lines over 21 deliveries in 330 s
// (acceptance of cybermonklabs/legion#4209 at def907052c, D9).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Each case is a fault that hands one envelope back and makes each of its deliveries write a
// line before the hand-back decides, from another part of the path: the read of the owed read's
// revision while the table is locked (D9's own), the same while GitHub answers 404, so that both
// of the delivery's owes fail, and the GitHub client's line for a shared rate limit it cannot
// read. Since N2 of the review at def907052c a
// 404 whose owe fails writes no line of its own before the hand-back's, so the 404 case has the
// locked table's line to bound, without which it passed with the cadence taken out (N13 of the
// review at 3a21edb2c9); the line N2 moved, written once after its owe, is
// TestAPullRequestGitHubGivesNoAnswerForLogsOneLineOnceItsReadIsOwed's. The envelope is handed
// back at least 32 times, so the cadence's second WARN delivery, the 31st, is among them, and a
// delivery after it; then the fault ends and the envelope is acknowledged.
//
// The log is read at DEBUG and cut at each hand-back line, which a delivery writes last: the lines
// before it are that delivery's. So the envelope is published only once the intake's take-over of
// its durable has ended in the log, and only the lines after that are read (awaitIntakeStarted):
// the durable delivers as soon as it is created, before the take-over writes its own lines, and
// under load its `NATS consumer bound` line was read as delivery 2's (2026-11-17). A line above
// DEBUG in a delivery outside the cadence fails the test, whatever wrote it. Where the case's line
// is named, every delivery must have written it, at its own level at the first, so the test sees
// the line it bounds; and the delivery that is acknowledged must write its own line above DEBUG,
// so the test sees that a line is held to the cadence only where its delivery hands the envelope
// back.
func TestEveryLineOfAHandedBackDeliveryKeepsTheHandBacksCadence(t *testing.T) {
	const number = 4216
	for _, fault := range []struct {
		name string
		// line is what the line the fault makes each delivery write says, "" where it writes
		// none of its own; cause is what the hand-back line's error names; final is what a line
		// of the delivery that is acknowledged says.
		line, cause, final string
		// start makes the fault before the envelope is published, and answers the pool and the
		// GitHub client the intake runs on and what ends the fault.
		start func(t *testing.T, ctx context.Context, pool *store.Pool, fake *fakeGitHub, github *githubPulls) (*store.Pool, *githubapp.Client, func())
	}{
		{
			name: "the_owed_table_is_locked", line: "read the revision of the read a pull request is owed",
			cause: "status 500", final: "the read it is owed is recorded",
			start: func(t *testing.T, ctx context.Context, pool *store.Pool, fake *fakeGitHub, github *githubPulls) (*store.Pool, *githubapp.Client, func()) {
				github.mu.Lock()
				github.failing = true
				github.mu.Unlock()
				return poolWithLockTimeout(t, ctx, pool, 50*time.Millisecond), fake.newTestClient(), lockOwedReads(t, ctx, pool)
			},
		},
		{
			name: "github_has_no_such_pull_request_while_the_owed_table_is_locked", line: "read the revision of the read a pull request is owed",
			cause: "status 404", final: "the read is owed, and the reconcile reads it again",
			start: func(t *testing.T, ctx context.Context, pool *store.Pool, fake *fakeGitHub, github *githubPulls) (*store.Pool, *githubapp.Client, func()) {
				github.mu.Lock()
				delete(github.pulls, number)
				github.mu.Unlock()
				return poolWithLockTimeout(t, ctx, pool, 50*time.Millisecond), fake.newTestClient(), lockOwedReads(t, ctx, pool)
			},
		},
		{
			name: "the_shared_rate_limit_cannot_be_read", line: "the shared GitHub rate limit was not read",
			cause: "status 500", final: "the read it is owed is recorded",
			start: func(t *testing.T, ctx context.Context, pool *store.Pool, fake *fakeGitHub, github *githubPulls) (*store.Pool, *githubapp.Client, func()) {
				github.mu.Lock()
				github.failing = true
				github.mu.Unlock()
				return pool, fake.newTestClient(githubapp.WithLimits(unreadableLimits{})), refuseOwedReads(t, ctx, pool)
			},
		},
	} {
		t.Run(fault.name, func(t *testing.T) {
			fastPageRetries(t)
			logs := captureLogsAt(t, slog.LevelDebug)
			pool, ctx := deliveryTestPool(t)
			issue := seedHeadIssue(t, ctx, pool, number)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			head, branch, body := "4216421642164216421642164216421642164216", "feature/x", "Dispatch: "+issue
			github.set("acme/widgets", number, "open", head, branch, body, at)
			intakePool, client, lift := fault.start(t, ctx, pool, fake, github)

			natsClient := intakeTestClient(t)
			intake := newIntake(natsClient, intakePool, deliveryTestLeases(t, pool),
				client, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
			// This intake's own delay, so 32 hand-backs take a second, not five minutes.
			intake.redeliverAfter = 20 * time.Millisecond
			startIntake(t, intake)
			awaitBoundDurable(t, natsClient)
			started := awaitIntakeStarted(t, logs)
			deliveries := func() []map[string]any { return logs()[started:] }
			publishPullRequestEnvelope(t, natsClient, "acme", "widgets", number,
				prEnvelope("acme/widgets", number, "opened", head, branch, body, at))
			const handedBack = 32
			waitFor(t, 3*time.Minute, func() bool {
				return len(handBackSegments(deliveries())) >= handedBack
			}, func() string {
				return fmt.Sprintf("the envelope was handed back %d times in 3 minutes, want %d: %+v",
					len(handBackSegments(deliveries())), handedBack, deliveryConsumerInfo(t, natsClient))
			})
			lift()
			awaitAcknowledged(t, natsClient)

			records := deliveries()
			segments := handBackSegments(records)
			final := records[segments[len(segments)-1].end:]
			t.Logf("start_lines=%d hand-backs=%d lines=%d final_lines=%d", started, len(segments), len(records), len(final))
			for i, segment := range segments {
				due := segment.delivered%handBackLineEvery == 1
				if want := i + 1; segment.delivered != want {
					t.Fatalf("hand-back line %d names delivery %d, want %d: the deliveries are not read in order", want, segment.delivered, want)
				}
				if cause, _ := segment.handBack["error"].(string); i == 0 && !strings.Contains(cause, fault.cause) {
					t.Errorf("the first hand-back line's error is %q, want it to name %q: the case did not happen", cause, fault.cause)
				}
				if level := segment.handBack["level"]; (level == "WARN") != due {
					t.Errorf("the hand-back line of delivery %d is %v, want WARN only at deliveries 1, 31, 61 and so on", segment.delivered, level)
				}
				wrote := false
				for _, record := range segment.lines {
					message, _ := record["msg"].(string)
					if fault.line != "" && strings.Contains(message, fault.line) {
						wrote = true
						if i == 0 && record["level"] == "DEBUG" {
							t.Errorf("delivery 1 wrote %q at DEBUG, want its own level: the first delivery is the cadence's", message)
						}
					}
					if !due && record["level"] != "DEBUG" {
						t.Errorf("delivery %d of the handed-back envelope wrote a %v line outside the hand-back's cadence: %v", segment.delivered, record["level"], record)
					}
				}
				if fault.line != "" && !wrote {
					t.Errorf("delivery %d wrote no line saying %q, want one at each delivery: the test does not see the line it bounds", segment.delivered, fault.line)
				}
			}
			acknowledged := false
			for _, record := range final {
				if message, _ := record["msg"].(string); strings.Contains(message, fault.final) && record["level"] != "DEBUG" {
					acknowledged = true
				}
			}
			if !acknowledged {
				t.Errorf("the delivery that was acknowledged wrote no line above DEBUG saying %q, want its own level: a delivery that does not hand the envelope back keeps its lines' levels; it wrote %v", fault.final, final)
			}
		})
	}
}

// handBackSegment is one delivery of a handed-back envelope, as the log reads it: the delivery's
// count and hand-back line, the lines written before that line and after the delivery before it,
// and the index of the record after the hand-back line.
type handBackSegment struct {
	delivered int
	handBack  map[string]any
	lines     []map[string]any
	end       int
}

// handBackSegments cuts records at each hand-back line (logHandBack).
func handBackSegments(records []map[string]any) []handBackSegment {
	var segments []handBackSegment
	from := 0
	for i, record := range records {
		if message, _ := record["msg"].(string); strings.Contains(message, "handing the envelope back") {
			delivered, _ := record["deliveries"].(float64)
			segments = append(segments, handBackSegment{delivered: int(delivered), handBack: record, lines: records[from:i], end: i + 1})
			from = i + 1
		}
	}
	return segments
}

// awaitIntakeStarted waits until the intake's take-over of its durable has ended in the log, and
// answers how many records the log held then: the `NATS consumer bound` line, and after it the
// consumer lease's `leased pass ran` line, which duty.Exclusive writes once the take-over returns.
// awaitBoundDurable reads the server, where the durable delivers as soon as it is created, while
// the take-over records its hold's token and writes those lines after (heldDurable.finishTakeOver).
// So a test that judges a delivery's lines by their place in the log publishes only after this,
// and reads only the records after the count: no line of the intake's start is a delivery's.
func awaitIntakeStarted(t *testing.T, logs func() []map[string]any) int {
	t.Helper()
	lease := consumerLease(bus.DispatchDeliveryDurable)
	started := 0
	waitFor(t, 20*time.Second, func() bool {
		bound := false
		for i, record := range logs() {
			message, _ := record["msg"].(string)
			switch {
			case message == "dispatch delivery: NATS consumer bound" && record["name"] == bus.DispatchDeliveryDurable:
				bound = true
			case bound && message == "dispatch duty: leased pass ran" && record["lease"] == lease:
				started = i + 1
				return true
			}
		}
		return false
	}, func() string {
		return fmt.Sprintf("in 20 s the log holds no `NATS consumer bound` line of %s followed by the end of the %s pass", bus.DispatchDeliveryDurable, lease)
	})
	return started
}

// poolWithLockTimeout is a pool of pool's database whose sessions wait at most timeout for a lock,
// so a statement on a table another session holds locked fails in that time, as it fails in a
// second (owedRevisionReadTimeout) or five (owedReadWriteTimeout) on the intake's own pool.
func poolWithLockTimeout(t *testing.T, ctx context.Context, pool *store.Pool, timeout time.Duration) *store.Pool {
	t.Helper()
	config := pool.Config()
	params := map[string]string{"lock_timeout": fmt.Sprintf("%dms", timeout.Milliseconds())}
	for name, value := range config.ConnConfig.RuntimeParams {
		params[name] = value
	}
	config.ConnConfig.RuntimeParams = params
	opened, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a pool whose sessions wait %s for a lock: %v", timeout, err)
	}
	t.Cleanup(opened.Close)
	return store.NewPool(opened)
}

// lockOwedReads holds pull_request_reads_owed in ACCESS EXCLUSIVE mode on a connection of its
// own, as a migration of the table holds it, until the function it answers ends the hold.
func lockOwedReads(t *testing.T, ctx context.Context, pool *store.Pool) (release func()) {
	t.Helper()
	return holdOwedReads(t, ctx, pool, "lock table pull_request_reads_owed in access exclusive mode")
}

// holdOwedReads runs statement, a migration's statement on pull_request_reads_owed or its
// revision sequence, in a transaction of a connection of its own, and holds the locks it takes
// until the function it answers rolls the transaction back.
func holdOwedReads(t *testing.T, ctx context.Context, pool *store.Pool, statement string) (release func()) {
	t.Helper()
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect to hold %q: %v", statement, err)
	}
	tx, err := conn.Begin(ctx)
	if err == nil {
		_, err = tx.Exec(ctx, statement)
	}
	if err != nil {
		t.Fatalf("hold %q: %v", statement, err)
	}
	release = sync.OnceFunc(func() {
		ended := context.WithoutCancel(ctx)
		if err := tx.Rollback(ended); err != nil {
			t.Errorf("end the hold of %q: %v", statement, err)
		}
		if err := conn.Close(ended); err != nil {
			t.Errorf("close the connection that held %q: %v", statement, err)
		}
	})
	t.Cleanup(release)
	return release
}

// unreadableLimits is the shared rate limits of a database that answers no read of them: the
// client logs that it could not read the limit and sends the request (githubapp.Client.send).
type unreadableLimits struct{}

func (unreadableLimits) Wait(context.Context, string) (time.Duration, time.Time, error) {
	return 0, time.Time{}, errors.New("the test answers no read of github_rate_limits")
}

func (unreadableLimits) Record(context.Context, string, time.Duration) error { return nil }

func (unreadableLimits) Strike(context.Context, string, time.Duration) (ratelimit.Limit, error) {
	return ratelimit.Limit{}, nil
}
