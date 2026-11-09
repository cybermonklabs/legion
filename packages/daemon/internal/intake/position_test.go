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
// DeliverNewPolicy: it will never receive those older messages, so its ack floor can never reach
// a target the listing read against them. DispatchPosition's idle, not the ack floor alone, is
// what tells Reconcile nothing more is ever coming for a record those messages already advanced
// past.
//
// The gap between the floor and the target is built here rather than assumed. Against the server
// this module links in (2.15.1), a fresh DeliverNew consumer is created AT the stream's last
// sequence — measured, AckFloor.Stream 2 against LastSeq 2 — where the server the fixture's
// container used to run (2.10.22) created it at 0, so the original construction, two messages
// and nothing else, no longer separates them. What still does is the production shape: the
// target is the STREAM's last sequence (consume.go, DispatchTarget reading info.State.LastSeq)
// while the Dispatch consumer filters two subjects (consume.go, FilterSubjects), so a message on
// any other subject of the stream advances the target and reaches the consumer never. One such
// message, published after the consumer exists, is the whole of the difference: floor 2 against
// target 3, and idle alone closes it.
func TestDispatchPositionIsIdleForARecreatedConsumerBehindPreExistingMessages(t *testing.T) {
	js, _ := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.comment.created", envelopeJSON(t, "dispatch-comment", "dispatch",
		`{"id":2,"issue_key":"CAPTURE-3","seq":9,"notify":true,"type":"comment.created","payload":{"body":"hi"}}`))

	consumers, err := OpenConsumers(context.Background(), js, migratedPool(t), spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	// The one the Dispatch consumer's filter never matches: it moves the stream's last sequence,
	// which is the target, and the floor stays where the creation put it.
	publish(t, js, legionWakeSubject, envelopeJSON(t, "wake-position", "envoy", `{"issue":"CAPTURE-3"}`))
	target, err := consumers.DispatchTarget(context.Background())
	if err != nil {
		t.Fatalf("DispatchTarget: %v", err)
	}
	if target < 3 {
		t.Fatalf("target = %d, want at least 3 (the two messages already published and the one the filter never matches)", target)
	}
	position, err := consumers.DispatchPosition(context.Background())
	if err != nil {
		t.Fatalf("DispatchPosition: %v", err)
	}
	if !position.Idle {
		t.Fatalf("idle = false for a freshly (re)created consumer with nothing it can ever deliver from before it existed, want true")
	}
	if position.AckFloorStream >= target {
		t.Fatalf("ack_floor = %d, target = %d; want ack_floor short of target so idle is what closes the gap, not the floor", position.AckFloorStream, target)
	}
	if !position.Reached(target) {
		t.Fatalf("Reached(%d) = false at ack_floor %d with idle %v, want true: idle is what answers a target the floor can never reach", target, position.AckFloorStream, position.Idle)
	}
}

// Reached is the whole of that judgement as a function of its two fields, which the test above
// reaches through a real stream and a real consumer: a floor at or past the target answers it
// whatever idle says, and an idle consumer answers it whatever the floor says, because a target
// its filter never matches is one no delivery will ever move the floor to.
func TestADispatchConsumerPositionIsReachedByItsFloorOrByBeingIdle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		position DispatchConsumerPosition
		target   int64
		want     bool
	}{
		{name: "the floor is past the target", position: DispatchConsumerPosition{AckFloorStream: 9}, target: 8, want: true},
		{name: "the floor is the target", position: DispatchConsumerPosition{AckFloorStream: 8}, target: 8, want: true},
		{name: "the floor is short of the target and the consumer is working", position: DispatchConsumerPosition{AckFloorStream: 7}, target: 8},
		{name: "the floor is short of the target and the consumer is idle", position: DispatchConsumerPosition{AckFloorStream: 7, Idle: true}, target: 8, want: true},
		{name: "nothing has been acked and the consumer is idle", position: DispatchConsumerPosition{Idle: true}, target: 8, want: true},
		{name: "nothing has been acked and the consumer is working", position: DispatchConsumerPosition{}, target: 8},
		{name: "a target of nothing is reached from a standing start", position: DispatchConsumerPosition{}, target: 0, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.position.Reached(tc.target); got != tc.want {
				t.Fatalf("DispatchConsumerPosition{AckFloorStream: %d, Idle: %v}.Reached(%d) = %v, want %v",
					tc.position.AckFloorStream, tc.position.Idle, tc.target, got, tc.want)
			}
		})
	}
}

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

// A consumer that has applied nothing — here the GitHub consumer, whose only message was acknowledged
// without a transaction, which records no position — is created at the next message when it is
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
	push := strings.ReplaceAll(string(capturedGitHubEnvelope(t, "push.json")), "refs/heads/legion/LEGION-208", "refs/heads/main")
	publish(t, js, "notifications.github.sjawhar.legion.push.branch.main", []byte(push))
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
// none of them, and the pass's read of each pull request heals those it would have acted on.
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
