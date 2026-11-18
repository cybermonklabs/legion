package intake

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

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
// nats-server 2.10.22, the published nats:2.10 image (`.github/ci-images.yaml`) the daemon's tests
// ran in a container before they ran their server in the test process, it stands at 0, behind the
// two messages the consumer can never deliver; on 2.15.1, which the hub runs and internal/testnats
// runs in the test process for this test, it is initialised at the stream's last sequence, 2,
// level with the target. Both readings carry the same facts — nothing pending and nothing awaiting
// an ack — so a test that asserted the floor stood short of the target measured the server rather
// than this code, and was red on a pod running 2.15.1 while green in CI on 2.10.22
// (dispatch://INFRA-3027).
//
// So the precondition is stated as what both servers hold: the consumer has delivered none of the
// messages published before it and has none to deliver, which is the state idle is read from.
func TestDispatchPositionIsIdleForARecreatedConsumerBehindPreExistingMessages(t *testing.T) {
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.comment.created", envelopeJSON(t, "dispatch-comment", "dispatch",
		`{"id":2,"issue_key":"CAPTURE-3","seq":9,"notify":true,"type":"comment.created","payload":{"body":"hi"}}`))

	consumers, err := OpenConsumers(context.Background(), js, migratedPool(t), spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	target, err := consumers.DispatchTarget(context.Background())
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
	consumer, err := stream.Consumer(context.Background(), dispatchConsumerName(spec.Project))
	if err != nil {
		t.Fatalf("read the Dispatch consumer: %v", err)
	}
	info, err := consumer.Info(context.Background())
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
	position, err := consumers.DispatchPosition(context.Background())
	if err != nil {
		t.Fatalf("DispatchPosition: %v", err)
	}
	if !position.Idle {
		t.Fatalf("idle = false for a freshly (re)created consumer with nothing it can ever deliver from before it existed, want true: ack floor %d, target %d",
			position.AckFloorStream, target)
	}
}

// A Dispatch consumer whose info reads nothing pending is idle only once the stream's own store
// holds no message of its filters after the last one it delivered. nats-server answers a publish
// before it counts the message pending on a consumer — processJetStreamMsgWithBatch (2.15.1)
// queues that signal after the answer, and signalConsumersLoop counts it — so for a moment the
// stream's last sequence, the boot's target, counts a message the consumer's info does not. An
// idle believed then had the boot's reconcile hold nothing behind a key the listing showed behind:
// the daemon's TestReconcileHoldsAKeyTheBootListingShowsOutOfTheWorkflowStatusWindow, red in one of
// five whole-package runs of internal/daemon under 32 busy loops with -race (dispatch://INFRA-3149).
// The stream here answers what the server answers in that moment, which no test of a real server
// can hold still.
//
// The second case is the consumer created over messages it will never be given, as nats-server
// 2.10 reports one — its ack floor at 0, its last delivered at the stream's end — which is idle,
// because the store is read from past the last delivered and not from past the ack floor.
//
// The negative control is the store check removed from DispatchPosition (the info's idle taken as
// it reads): the first case then reads idle, and its position reaches a target the consumer still
// has a message to give for.
func TestDispatchPositionIsIdleOnlyWhenTheStoreHoldsNothingAfterTheLastDelivered(t *testing.T) {
	filters := jetstream.ConsumerConfig{FilterSubjects: []string{"notifications.dispatch.issue.>", mappingSubject("CAPTURE")}}
	for _, tc := range []struct {
		name   string
		info   jetstream.ConsumerInfo
		stored []uint64
		target int64
		idle   bool
	}{
		{name: "a message stored a moment ago that the info does not count yet",
			info:   jetstream.ConsumerInfo{Config: filters, AckFloor: jetstream.SequenceInfo{Stream: 4}, Delivered: jetstream.SequenceInfo{Stream: 4}},
			stored: []uint64{3, 4, 5}, target: 5, idle: false},
		{name: "a consumer created past messages it will never be given",
			info:   jetstream.ConsumerInfo{Config: filters, AckFloor: jetstream.SequenceInfo{Stream: 0}, Delivered: jetstream.SequenceInfo{Stream: 2}},
			stored: []uint64{1, 2}, target: 2, idle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consumers := &Consumers{spec: ConsumerSpec{Project: "CAPTURE"}, stream: laggingStream{info: tc.info, stored: tc.stored}}
			position, err := consumers.DispatchPosition(context.Background())
			if err != nil {
				t.Fatalf("DispatchPosition: %v", err)
			}
			if position.Idle != tc.idle {
				t.Fatalf("idle = %v with the store holding %v past an ack floor of %d and a last delivered of %d, want %v",
					position.Idle, tc.stored, tc.info.AckFloor.Stream, tc.info.Delivered.Stream, tc.idle)
			}
			if got := position.Reached(tc.target); got != tc.idle {
				t.Fatalf("Reached(%d) = %v at %+v, want %v", tc.target, got, position, tc.idle)
			}
		})
	}
}

// laggingStream is a notification stream whose Dispatch consumer's info reads nothing pending and
// nothing awaiting an ack, while its store holds the messages of the consumer's filters at the
// stream sequences stored, in order. It answers the two calls DispatchPosition makes and no other.
type laggingStream struct {
	jetstream.Stream
	info   jetstream.ConsumerInfo
	stored []uint64
}

func (s laggingStream) Consumer(context.Context, string) (jetstream.Consumer, error) {
	return laggingConsumer{info: &s.info}, nil
}

// GetMsg is STREAM.MSG.GET by next subject: the first stored message at or after seq.
func (s laggingStream) GetMsg(_ context.Context, seq uint64, _ ...jetstream.GetMsgOpt) (*jetstream.RawStreamMsg, error) {
	for _, stored := range s.stored {
		if stored >= seq {
			return &jetstream.RawStreamMsg{Sequence: stored}, nil
		}
	}
	return nil, jetstream.ErrMsgNotFound
}

type laggingConsumer struct {
	jetstream.Consumer
	info *jetstream.ConsumerInfo
}

func (c laggingConsumer) CachedInfo() *jetstream.ConsumerInfo { return c.info }

// A GitHub consumer that is found missing — deleted, or lost with the NATS server's state — while
// events were published is created at the last message it applied, so every event published while
// it was missing is applied, and none twice: the one at its position, already applied, is
// delivered again and dropped as a duplicate. Both ways a consumer is created do it: the boot's
// OpenConsumers, and a mapping refresh's SetRepositories mid-run.
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
			assertAcknowledgedThrough(t, stream, github, publish(t, js, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1)))
			testwait.Eventually(t, "the first event applied", func() bool { return slices.Equal(writeHandlers(t, pool), []string{"pr-1"}) })
			stop()

			var last *jetstream.PubAck
			defer reopen.open(t, js, pool, spec, func() {
				if err := stream.DeleteConsumer(context.Background(), github); err != nil {
					t.Fatalf("delete the GitHub consumer: %v", err)
				}
				for number := 2; number <= 4; number++ {
					last = publish(t, js, fmt.Sprintf("notifications.github.sjawhar.legion.pr.%d", number), pullRequestOpenedEnvelope(t, number))
				}
			})()
			assertAcknowledgedThrough(t, stream, github, last)
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
	assertAcknowledgedThrough(t, stream, github, publish(t, js, "notifications.github.sjawhar.legion.push.tag.v1.0.0", []byte(tag)))
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
	next := publish(t, js, "notifications.github.sjawhar.legion.pr.2", pullRequestOpenedEnvelope(t, 2))
	testwait.Eventually(t, "the next event applied", func() bool { return len(writeHandlers(t, pool)) >= 1 })
	assertAcknowledgedThrough(t, stream, github, next)
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
	first := publish(t, js, "notifications.github.sjawhar.legion.pr.1", pullRequestOpenedEnvelope(t, 1))

	defer startConsume(t, js, spec, pool, factMarks())()
	assertAcknowledgedThrough(t, stream, github, first)
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
