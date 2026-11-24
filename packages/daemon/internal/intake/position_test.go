package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A Dispatch consumer recreated after messages already landed on the stream — deleted and
// reopened, or a first boot against a stream that already carries history — starts at
// DeliverNewPolicy: it will never receive those older messages. DispatchPosition's idle, not the
// ack floor, is what tells Reconcile nothing more is ever coming for a record those messages
// already advanced past.
//
// What the consumer's own ack floor reads there is the server's business and not the daemon's,
// and the two servers this repository runs answer differently for exactly this consumer: on
// nats-server 2.10.22, which `.github/ci-images.yaml` pins for CI, it stands at 0, behind the two
// messages the consumer can never deliver; on 2.15.1, which `skill://legion-worker/references/
// tools.md` tells a pod to run, it is initialised at the stream's last sequence, 2, level with the
// target. Both readings carry the same facts — nothing pending and nothing awaiting an ack — so a
// test that asserted the floor stood short of the target measured the server rather than this
// code, and was red on the documented pod route while green in CI (dispatch://INFRA-3027).
//
// So the precondition is stated as what both servers hold: the consumer has delivered none of the
// messages published before it and has none to deliver, which is the state idle is read from.
func TestDispatchPositionIsIdleForARecreatedConsumerBehindPreExistingMessages(t *testing.T) {
	js, stream := testJetStream(t)
	pool := migratedPool(t)
	spec := consumerSpec(&lockedBuffer{})
	// Each read of NATS this row makes gets a deadline of its own (rowRead): a minute, or what is
	// left of the test binary's deadline less a second when that is sooner (testwait.Bounded).
	// A read handed a context with no deadline gets nats.go's 5 s default instead
	// (wrapContextWithoutDeadline), which a loaded machine can outrun, and which would make the row
	// fail on a clock rather than on what it reads.
	read := rowRead(t)
	// A diagnostic for a failure whose cause is not yet known: one loaded run of this package
	// under the race detector failed the precondition below with "delivered 0, with 2 pending
	// and 0 awaiting an ack", which is what a durable that already existed before the two
	// publishes reads (a planted DeliverNew durable reproduces that line exactly), while
	// CreateStream had found the stream with no consumer moments before. The row's claim is
	// about a consumer OpenConsumers creates after the publishes, so any consumer here is
	// another's, and the failure names each by what it carries.
	requireNoConsumers(t, read, stream)
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.comment.created", envelopeJSON(t, "dispatch-comment", "dispatch",
		`{"id":2,"issue_key":"CAPTURE-3","seq":9,"notify":true,"type":"comment.created","payload":{"body":"hi"}}`))

	opening, cancelOpening := read()
	defer cancelOpening()
	consumers, err := OpenConsumers(opening, js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	targetRead, cancelTarget := read()
	defer cancelTarget()
	target, err := consumers.DispatchTarget(targetRead)
	if err != nil {
		t.Fatalf("DispatchTarget: %v", err)
	}
	if target < 2 {
		t.Fatalf("target = %d, want at least 2 (the two messages already published)", target)
	}
	// The precondition, read off the consumer itself: it was created at DeliverNew over the two
	// messages, so it has none pending and none awaiting an ack, and no delivery of its own will
	// ever carry them. A consumer that could still deliver one would make the idle below true for
	// another reason than the one this test is about.
	consumerRead, cancelConsumer := read()
	defer cancelConsumer()
	consumer, err := stream.Consumer(consumerRead, dispatchConsumerName(spec.Project))
	if err != nil {
		t.Fatalf("read the Dispatch consumer: %v", err)
	}
	infoRead, cancelInfo := read()
	defer cancelInfo()
	info, err := consumer.Info(infoRead)
	if err != nil {
		t.Fatalf("read the Dispatch consumer's info: %v", err)
	}
	if info.Config.DeliverPolicy != jetstream.DeliverNewPolicy {
		t.Fatalf("deliver policy = %v, want %v: this test is about a consumer that can never deliver what came before it",
			info.Config.DeliverPolicy, jetstream.DeliverNewPolicy)
	}
	if info.Delivered.Consumer != 0 || info.NumPending != 0 || info.NumAckPending != 0 {
		t.Fatalf("the consumer has delivered %d, with %d pending and %d awaiting an ack; want it to have delivered none of the two and to have none to deliver",
			info.Delivered.Consumer, info.NumPending, info.NumAckPending)
	}
	positionRead, cancelPosition := read()
	defer cancelPosition()
	position, err := consumers.DispatchPosition(positionRead)
	if err != nil {
		t.Fatalf("DispatchPosition: %v", err)
	}
	if !position.Idle {
		t.Fatalf("idle = false for a freshly (re)created consumer with nothing it can ever deliver from before it existed, want true: ack floor %d, target %d",
			position.AckFloorStream, target)
	}
}

// nats-server answers a publish once it has stored the message, and counts the message into each
// consumer's pending only afterwards, on a goroutine of the stream's own (DispatchPosition). A
// CONSUMER.INFO read in between counts a Dispatch consumer with a message waiting as having nothing
// pending and nothing awaiting an ack; read idle, it let the daemon's boot reconcile decide a key
// that waiting message would still change (internal/daemon's
// TestReconcileHoldsAKeyTheBootListingShowsOutOfTheWorkflowStatusWindow, 1 of 20 race runs). The
// stand-in answers that reading every time (lagStandIn's uncounted), so the gap the server leaves
// for a moment is open for the whole row:
//   - a GitHub message, which the Dispatch consumer's filter never matches, leaves it idle;
//   - a Dispatch message stored after the consumer's delivered sequence makes it not idle;
//   - with the stored-message get refused at once, or never answered until its bound passes (as
//     NATS answers a user without the grant, and as a loaded server can), the poller's reading
//     (DispatchPosition) keeps the counts' idle and a reconcile's (ReconcilePosition) reads not
//     idle, each saying so at Warn.
func TestDispatchPositionIsNotIdleWhileTheStoreHoldsAMessageTheConsumerHasNotCounted(t *testing.T) {
	ctx := context.Background()
	served, _ := testJetStream(t)
	standIn := &lagStandIn{JetStream: served}
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	consumers, err := OpenConsumers(ctx, standIn, migratedPool(t), spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	standIn.uncounted = true
	reading := func(read func(context.Context) (DispatchConsumerPosition, error)) DispatchConsumerPosition {
		t.Helper()
		bounded, cancel := rowRead(t)()
		defer cancel()
		position, err := read(bounded)
		if err != nil {
			t.Fatalf("reading the Dispatch consumer's position: %v", err)
		}
		return position
	}
	position := func() DispatchConsumerPosition { return reading(consumers.DispatchPosition) }

	publish(t, served, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))
	if got := position(); !got.Idle {
		t.Fatalf("position with only a GitHub message stored = %+v, want idle: the Dispatch consumer's filter never matches it", got)
	}

	publish(t, served, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	if got := position(); got.Idle {
		t.Fatalf("position with a Dispatch message stored and not yet counted = %+v, want not idle", got)
	}

	never := func(ctx context.Context) (*jetstream.RawStreamMsg, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	for _, failing := range []struct {
		name   string
		getMsg func(context.Context) (*jetstream.RawStreamMsg, error)
		err    string
	}{
		{"refused", func(context.Context) (*jetstream.RawStreamMsg, error) {
			return nil, errors.New("nats: permissions violation")
		}, "nats: permissions violation"},
		{"never answered", never, context.DeadlineExceeded.Error()},
	} {
		standIn.getMsg = failing.getMsg
		logs.Reset()
		if got := position(); !got.Idle {
			t.Fatalf("the poller's position with the stored-message get %s = %+v, want the counts' idle", failing.name, got)
		}
		if got := reading(consumers.ReconcilePosition); got.Idle {
			t.Fatalf("a reconcile's position with the stored-message get %s = %+v, want not idle", failing.name, got)
		}
		for _, wants := range [][]string{{"its idle is its own count alone"}, {"a reconcile reads the consumer as not idle"}} {
			if line := logLine(logs.String(), append(wants, "level=WARN", "on a loaded NATS server or because the daemon's NATS user lacks $JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS", failing.err)...); line == "" {
				t.Fatalf("logs with the stored-message get %s = %q, want a WARN line saying %q, naming both causes and the error", failing.name, logs.String(), wants[0])
			}
		}
	}
}

// rowRead returns what gives each read of a row its context: one bounded by a minute, or by what
// is left of the test binary's deadline less a second when that is sooner (testwait.Bounded),
// read when the read starts.
func rowRead(t *testing.T) func() (context.Context, context.CancelFunc) {
	return func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), testwait.Bounded(t, time.Minute, time.Second))
	}
}

// requireNoConsumers fails t when stream holds a consumer, naming each by its name, when the
// server created it, its config and what it has delivered, so a consumer some other actor made
// says who it is. The listing takes its context from read, as the row's other reads do, and names
// that context's bound when it fails.
func requireNoConsumers(t *testing.T, read func() (context.Context, context.CancelFunc), stream jetstream.Stream) {
	t.Helper()
	ctx, cancel := read()
	defer cancel()
	deadline, _ := ctx.Deadline()
	bound := time.Until(deadline).Round(time.Second)
	var held []string
	listed := stream.ListConsumers(ctx)
	for info := range listed.Info() {
		config, err := json.Marshal(info.Config)
		if err != nil {
			config = []byte(fmt.Sprintf("%+v", info.Config))
		}
		held = append(held, fmt.Sprintf("consumer %s, created %s, delivered %d (stream sequence %d), %d pending, config %s",
			info.Name, info.Created.Format(time.RFC3339Nano), info.Delivered.Consumer, info.Delivered.Stream, info.NumPending, config))
	}
	if err := listed.Err(); err != nil {
		t.Fatalf("list the stream's consumers before the row's publishes, within %s: %v", bound, err)
	}
	if len(held) != 0 {
		t.Fatalf("the stream holds %d consumers before the row's publishes, want none: %s", len(held), strings.Join(held, "; "))
	}
}

// A GitHub consumer that is found missing — deleted, or lost with the NATS server's state — while
// events were published is created at the last message it applied, so every event published while
// it was missing is applied, and none twice: the one at its position, already applied, is
// delivered again and dropped as a duplicate. Both ways a consumer is created do it: the boot's
// OpenConsumers, and a SetRepositories mid-run, as a recording on a repository not heard yet makes.
func TestAMissingGitHubConsumerStartsAtItsLastAppliedPosition(t *testing.T) {
	for _, reopen := range []struct {
		name string
		open func(t *testing.T, js jetstream.JetStream, pool *pgxpool.Pool, spec ConsumerSpec, missing func()) func()
	}{
		{"OpenConsumers", func(t *testing.T, js jetstream.JetStream, pool *pgxpool.Pool, spec ConsumerSpec, missing func()) func() {
			missing()
			return startConsume(t, js, spec, pool, factMarks())
		}},
		{"SetRepositories", func(t *testing.T, js jetstream.JetStream, pool *pgxpool.Pool, spec ConsumerSpec, missing func()) func() {
			consumers, err := OpenConsumers(context.Background(), js, pool, spec)
			if err != nil {
				t.Fatalf("OpenConsumers: %v", err)
			}
			missing()
			if err := consumers.SetRepositories(context.Background(), capturedRepositories); err != nil {
				t.Fatalf("SetRepositories: %v", err)
			}
			return runConsumers(t, consumers, factMarks())
		}},
	} {
		t.Run(reopen.name, func(t *testing.T) {
			pool := migratedPool(t)
			createWrites(t, pool)
			js, stream := testJetStream(t)
			logs := &lockedBuffer{}
			spec := consumerSpec(logs)
			github := githubConsumerName(spec.Project)

			stop := startConsume(t, js, spec, pool, factMarks())
			publish(t, js, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))
			waitDelivered(t, stream, github)
			testwait.Eventually(t, "the first event applied", func() bool { return slices.Equal(writeHandlers(t, pool), []string{"pr-1"}) })
			stop()

			defer reopen.open(t, js, pool, spec, func() {
				if err := stream.DeleteConsumer(context.Background(), github); err != nil {
					t.Fatalf("delete the GitHub consumer: %v", err)
				}
				for number := 2; number <= 4; number++ {
					publish(t, js, fmt.Sprintf("notifications.github.sjawhar.legion.pr.%d", number), pullRequestOpenedEnvelope(t, number))
				}
			})()
			waitDelivered(t, stream, github)
			want := []string{"pr-1", "pr-2", "pr-3", "pr-4"}
			testwait.Eventually(t, "the events published while the consumer was missing applied", func() bool {
				return len(writeHandlers(t, pool)) >= len(want)
			})
			if got := writeHandlers(t, pool); !slices.Equal(got, want) {
				t.Fatalf("applied %q, want each event applied once, %q", got, want)
			}
			if line := logLine(logs.String(), "level=INFO", "created at the last message it applied", "consumer="+github); line == "" {
				t.Errorf("no INFO line names the consumer created at its position; logs:\n%s", logs)
			}
			if line := logLine(logs.String(), "level=ERROR", "consumer="+github); line != "" {
				t.Errorf("a consumer whose position the stream still holds logged an error: %s", line)
			}
			info, err := stream.Info(context.Background())
			if err != nil {
				t.Fatalf("stream info: %v", err)
			}
			if seq := storedStreamSeq(t, pool, github); seq != int64(info.State.LastSeq) {
				t.Errorf("stored stream_seq = %d, want the last applied message's, %d", seq, info.State.LastSeq)
			}
		})
	}
}

// Each read of the notification stream's info goes through a handle of its own, never through the
// one handle the consumers share. This test reads at once: DispatchTarget on two goroutines; on a
// third, SetRepositories after each delete of the GitHub consumer, which creates it again at its
// stored position and so reads the stream's info (createConsumer); and on a fourth, for as long as
// the third runs, Lag, while a Dispatch issue event waits unacknowledged on the Dispatch consumer,
// so that every Lag reads that event's time (oldestWaiting) with a GetMsg on the shared handle,
// which reads the handle's cached info. nats.go's Info writes the cached info of the handle it is
// called on under no lock, so the race detector fails this test on a read of the stream's info on
// the shared handle: createConsumer's beside Lag's GetMsg, and DispatchTarget's beside the other
// goroutine's and Lag's. CI's daemon-go job runs this package under -race (ci_job_reads.py's
// DAEMON_RACE_PACKAGES).
func TestEachReadOfTheStreamsInfoBesideALagReadGoesThroughAHandleOfItsOwn(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	github, dispatch := githubConsumerName(spec.Project), dispatchConsumerName(spec.Project)
	pushed := publish(t, js, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))
	// A position stored for the GitHub consumer, so each creation of it reads the stream's info.
	if _, err := pool.Exec(ctx, `insert into consumer_positions (consumer, stream_seq, stream_time) values ($1, $2, $3)`,
		github, int64(pushed.Sequence), time.Now()); err != nil {
		t.Fatalf("store the GitHub consumer's position: %v", err)
	}
	consumers, err := OpenConsumers(ctx, js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	// Published after OpenConsumers created the Dispatch consumer at the next message, and handled
	// by nothing, since nothing runs the consumers: it waits on the Dispatch consumer throughout.
	last := publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	waiting, err := stream.GetMsg(ctx, last.Sequence)
	if err != nil {
		t.Fatalf("read the waiting Dispatch event back: %v", err)
	}

	const reads, creations = 50, 10
	recreated := make(chan struct{})
	lags := 0
	var group errgroup.Group
	for range 2 {
		group.Go(func() error {
			for range reads {
				target, err := consumers.DispatchTarget(ctx)
				if err != nil {
					return fmt.Errorf("DispatchTarget: %w", err)
				}
				if target != int64(last.Sequence) {
					return fmt.Errorf("DispatchTarget = %d, want the stream's last sequence, %d", target, last.Sequence)
				}
			}
			return nil
		})
	}
	group.Go(func() error {
		defer close(recreated)
		for range creations {
			if err := stream.DeleteConsumer(ctx, github); err != nil {
				return fmt.Errorf("delete the GitHub consumer: %w", err)
			}
			if err := consumers.SetRepositories(ctx, capturedRepositories); err != nil {
				return fmt.Errorf("SetRepositories: %w", err)
			}
		}
		return nil
	})
	group.Go(func() error {
		for {
			// The control: Lag read the waiting event's time, so it took oldestWaiting's GetMsg on
			// the shared handle.
			if got := consumers.Lag(ctx)[0]; got.Name != dispatch || got.Waiting != 1 || !got.OldestAt.Equal(waiting.Time) || got.Unavailable != "" {
				return fmt.Errorf("Lag's first consumer = %+v, want %s with the one event waiting, the oldest stored at %s, and nothing unavailable",
					got, dispatch, waiting.Time)
			}
			lags++
			select {
			case <-recreated:
				return nil
			default:
			}
		}
	})
	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}
	// The control: OpenConsumers and every SetRepositories created the GitHub consumer at its
	// stored position, the path that reads the stream's info.
	if created := strings.Count(logs.String(), "created at the last message it applied"); created != creations+1 {
		t.Fatalf("the GitHub consumer was created at its stored position %d times, want %d: once by OpenConsumers and once by each SetRepositories; logs:\n%s",
			created, creations+1, logs)
	}
	t.Logf("Lag read the waiting event's time %d times while the GitHub consumer was created %d times", lags, creations)
}

// A consumer that has applied nothing — here the GitHub consumer, whose only message was a push to
// a tag, which is no fact of either kind and is acknowledged without a transaction, recording no
// position — is created at the next message when it is
// found missing, as a consumer created for the first time is: what was published before it is
// not replayed.
func TestAMissingConsumerWithNoStoredPositionStartsAtTheNextMessage(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	github := githubConsumerName(spec.Project)

	stop := startConsume(t, js, spec, pool, factMarks())
	tag := strings.ReplaceAll(string(capturedGitHubEnvelope(t, "push.json")), "refs/heads/legion/LEGION-208", "refs/tags/v1.0.0")
	publish(t, js, "notifications.github.sjawhar.legion.push.tag.v1.0.0", []byte(tag))
	waitDelivered(t, stream, github)
	stop()
	var positions int
	if err := pool.QueryRow(context.Background(), `select count(*) from consumer_positions`).Scan(&positions); err != nil {
		t.Fatalf("count consumer positions: %v", err)
	}
	if positions != 0 {
		t.Fatalf("%d positions stored for a message acknowledged without a transaction, want none", positions)
	}

	if err := stream.DeleteConsumer(context.Background(), github); err != nil {
		t.Fatalf("delete the GitHub consumer: %v", err)
	}
	publish(t, js, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))
	defer startConsume(t, js, spec, pool, factMarks())()
	if pending := consumerInfo(t, stream, github).NumPending; pending != 0 {
		t.Fatalf("the recreated consumer has %d messages pending, want none of what was published before it", pending)
	}
	publish(t, js, "notifications.github.sjawhar.legion.pr.2", pullRequestOpenedEnvelope(t, 2))
	testwait.Eventually(t, "the next event applied", func() bool { return len(writeHandlers(t, pool)) >= 1 })
	waitDelivered(t, stream, github)
	if got := writeHandlers(t, pool); !slices.Equal(got, []string{"pr-2"}) {
		t.Fatalf("applied %q, want only the event published after the consumer was created", got)
	}
	if line := logLine(logs.String(), "created at the last message it applied"); line != "" {
		t.Errorf("a consumer with no position logged a start at one: %s", line)
	}
}

// A position older than the stream's first message — the consumer was missing longer than the
// stream keeps messages (72 hours), the stream was recreated, or the project was quiet, since only
// an applied fact moves the position — starts the consumer at the stream's first message, and a
// WARN line names the gap: the events published between may have been missed, the daemon applied
// none of them, and the daemon's own read of each issue's pull request heals those it would have
// acted on.
func TestAStoredPositionOlderThanTheStreamWarnsOfEventsThatMayHaveBeenMissed(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	github := githubConsumerName(spec.Project)
	if _, err := pool.Exec(context.Background(), `insert into consumer_positions (consumer, stream_seq, stream_time) values ($1, 9000, $2)`,
		github, time.Now().Add(-100*time.Hour)); err != nil {
		t.Fatalf("store the old position: %v", err)
	}
	publish(t, js, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))

	defer startConsume(t, js, spec, pool, factMarks())()
	waitDelivered(t, stream, github)
	testwait.Eventually(t, "the stream's first event applied", func() bool { return slices.Equal(writeHandlers(t, pool), []string{"pr-1"}) })
	if line := logLine(logs.String(), "level=WARN", "consumer="+github, "older than the stream's first message", "may have been missed"); line == "" {
		t.Fatalf("no WARN line names the events that may have been missed before the stream's first message; logs:\n%s", logs)
	}
}

// factMarks is a handler that writes each fact it applies as a mark of its own: a pull request's
// opened as pr-<number>.
func factMarks() Handler {
	return handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
		var mark string
		switch fact := fact.(type) {
		case PullRequestOpened:
			mark = fmt.Sprintf("pr-%d", fact.Number)
		default:
			return Result{}, fmt.Errorf("unexpected fact %T", fact)
		}
		_, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ($1)`, mark)
		return Result{}, err
	})
}

// pullRequestOpenedEnvelope is the opened of sjawhar/legion#number on LEGION-208's branch, an event
// of its own.
func pullRequestOpenedEnvelope(t *testing.T, number int) []byte {
	t.Helper()
	return envelopeJSON(t, fmt.Sprintf("pr-opened-%d", number), "github", fmt.Sprintf(`{"action":"opened","author":"author","base_ref":"main",`+
		`"body":"Dispatch: LEGION-208","head_ref":"legion/LEGION-208","head_sha":"head-%d","kind":"pr","merged":"false","number":"%d",`+
		`"repo":"sjawhar/legion","title":"a pull request","updated_at":"2026-09-23T00:00:00Z","url":"https://github.com/sjawhar/legion/pull/%d"}`,
		number, number, number))
}

// waitDelivered waits until consumer has delivered and had acknowledged every message it has.
func waitDelivered(t *testing.T, stream jetstream.Stream, consumer string) {
	t.Helper()
	testwait.Eventually(t, consumer+" has every message delivered and acknowledged", func() bool {
		item, err := stream.Consumer(context.Background(), consumer)
		if err != nil {
			return false
		}
		info, err := item.Info(context.Background())
		return err == nil && info.NumPending == 0 && info.NumAckPending == 0
	})
}

func storedStreamSeq(t *testing.T, pool *pgxpool.Pool, consumer string) int64 {
	t.Helper()
	var seq int64
	if err := pool.QueryRow(context.Background(), `select stream_seq from consumer_positions where consumer = $1`, consumer).Scan(&seq); err != nil {
		t.Fatalf("read the stored position of %s: %v", consumer, err)
	}
	return seq
}

// logLine is the first line of logs holding every one of parts, or "" when none does.
func logLine(logs string, parts ...string) string {
	for line := range strings.SplitSeq(logs, "\n") {
		if !slices.ContainsFunc(parts, func(part string) bool { return !strings.Contains(line, part) }) {
			return line
		}
	}
	return ""
}
