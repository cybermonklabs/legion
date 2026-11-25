package delivery

// The intake in a deployment that has never saved delivery settings. A pull request's head row is
// what a `before_merge` check's results are compared with and what a check CI proves is read at
// (api/results_ci.go, NewestPullRequestHead), and no part of the delivery timeline, so the intake
// binds its durable and writes that row whatever the settings say; only the delivery timeline's
// own work waits for the settings. Its first bind starts at the stream's next message, so a
// deployment that turns the intake on does not replay the stream's whole retained history.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// unconfiguredBindWithin bounds the wait for an intake with no settings to bind its durable: a
// bind takes a hold and a consumer lease and creates the durable, which is seconds, not this.
const unconfiguredBindWithin = 30 * time.Second

// openedForKey is the `pull_request` envelope GitHub's `opened` makes for a pull request of
// deliveryRepo whose branch is legion/<key> and whose body names the issue on a `Dispatch:` line,
// at head sha: the shape a Legion implementer opens.
func openedForKey(number int, key, sha string) map[string]string {
	return map[string]string{
		"kind": "pr", "action": "opened", "merged": "false", "repo": deliveryRepo,
		"number": fmt.Sprint(number), "title": "feat: a widget",
		"body": "Dispatch: " + key, "author": "legion-implementer[bot]",
		"url":      fmt.Sprintf("https://github.com/%s/pull/%d", deliveryRepo, number),
		"head_ref": "legion/" + key, "head_repo": deliveryRepo, "head_sha": sha, "base_ref": "main",
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}
}

// awaitUnconfiguredBind waits, on unconfiguredBindWithin, for the delivery durable to be push-bound
// to a deliver subject other than notOn ("" for any), and answers what the server reports of it.
func awaitUnconfiguredBind(t *testing.T, natsClient *bus.Client, notOn string) *natsgo.ConsumerInfo {
	t.Helper()
	var info *natsgo.ConsumerInfo
	var err error
	waitFor(t, unconfiguredBindWithin, func() bool {
		info, err = natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.PushBound && info.Config.DeliverSubject != notOn
	}, func() string {
		return fmt.Sprintf("the intake of a deployment with no delivery settings did not bind its durable within %s: %+v, %v",
			unconfiguredBindWithin, info, err)
	})
	return info
}

// headRows is how many pull_request_heads rows deliveryRepo's pull request number has.
func headRows(t *testing.T, ctx context.Context, pool *store.Pool, number int) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(ctx, `select count(*) from pull_request_heads where repo = $1 and number = $2`,
		deliveryRepo, number).Scan(&rows); err != nil {
		t.Fatalf("count the head rows of %s#%d: %v", deliveryRepo, number, err)
	}
	return rows
}

// The reported case: a deployment with no delivery_settings row, and a pull request opened on a
// repository Dispatch serves, its branch legion/<KEY> and its body `Dispatch: <KEY>`. The intake
// binds its durable, writes the head row attributed to KEY, and the read `dispatch results` makes
// (NewestPullRequestHead) finds that head, so a check CI proves is derived at it rather than read
// as no_head. The branch names an issue held to the pull request's repository, so no commit
// message is read and GitHub is asked nothing. A workflow run in the same deployment is the
// delivery timeline's alone, so it is acknowledged and fetches nothing, and no delivery_settings
// row appears.
func TestAnIntakeWithNoDeliverySettingsRecordsAPullRequestsHead(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	key := seedHeadIssue(t, ctx, pool, 58)
	settings := loadedSettingsCache(t, ctx, pool)
	if held := settings.Load(); held != nil {
		t.Fatalf("the settings cache holds %+v in a database with no delivery_settings row, want nothing", held)
	}
	fake := newFakeGitHub(t)
	natsClient := intakeTestClient(t)
	intake := newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), settings, unrenamed())
	startIntake(t, intake)
	awaitUnconfiguredBind(t, natsClient, "")

	const number = 5801
	sha := "5801580158015801580158015801580158015801"
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", number, openedForKey(number, key, sha))
	waitFor(t, 20*time.Second, func() bool { return headRows(t, ctx, pool, number) == 1 }, func() string {
		return fmt.Sprintf("%s#%d has %d head rows 20s after its opened envelope, want 1\n%s",
			deliveryRepo, number, headRows(t, ctx, pool, number), intakeStateReport(t, natsClient))
	})
	head := readHead(t, ctx, pool, deliveryRepo, number)
	if head.IssueKey == nil || *head.IssueKey != key || head.State != "open" || head.HeadSHA != sha {
		t.Fatalf("the head row of %s#%d = %+v, want open at %s and attributed to %s", deliveryRepo, number, head, sha, key)
	}
	newest, found, err := NewestPullRequestHead(ctx, pool, key, HeadAnswers{})
	if err != nil || !found || newest.Number != number || newest.HeadSHA != sha {
		t.Fatalf("NewestPullRequestHead(%s) = %+v, found %t, %v; want %s#%d at %s", key, newest, found, err, deliveryRepo, number, sha)
	}

	publishWorkflowEnvelope(t, natsClient, "acme", "widgets", ".github/workflows/deploy.yml", 9101)
	awaitAcknowledged(t, natsClient)
	if runs := countRuns(t, ctx, pool); runs != 0 {
		t.Fatalf("delivery_runs rows = %d after a workflow run with no delivery settings, want 0", runs)
	}
	if mints := fake.tokenMints.Load(); mints != 0 {
		t.Fatalf("minted %d installation tokens, want none: the branch attributes the pull request and a workflow run fetches nothing without settings", mints)
	}
	if _, err := GetSettings(ctx, pool); !errors.Is(err, ErrNoSettings) {
		t.Fatalf("GetSettings after the envelopes = %v, want ErrNoSettings: the intake writes no delivery_settings row", err)
	}
}

// The first bind of a deployment that never had the durable starts at the stream's next message,
// as an explicit start sequence: a pull request opened before the bind, which the stream still
// holds, gets no head row from it. A take-over of that durable before it has acknowledged anything
// replays none of it either, and a pull request opened after the take-over gets its row.
//
// The explicit start and the take-over's floor of OptStartSeq (takeOverConfig) are the rule
// docs/solutions/envoy/a-fresh-jetstream-consumer-reports-ack-floor-0-so-create-it-at-a-start-sequence-INFRA-594.md
// records from nats-server v2.10.29, which reports a fresh consumer's ack floor as 0, so a take-over
// at the floor plus one would hand the new holder the stream from its first message. This test
// proves no replay on the server it runs, and does not isolate that floor: on v2.15.1 the server
// reports the floor as the stream's last sequence, and a mutant without the floor of OptStartSeq,
// or without the explicit start, passes here too. The rule stays as the guard for a server version
// where the trap exists, and it is the one bus.SessionConsumers follows for the same shape.
func TestAFirstBindStartsAtTheStreamsNextMessageAndATakeOverKeepsThatStart(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	key := seedHeadIssue(t, ctx, pool, 59)
	fake := newFakeGitHub(t)
	publisher := intakeTestClient(t)

	const before, after = 5901, 5902
	stored := publishPullRequestEnvelope(t, publisher, "acme", "widgets", before, openedForKey(before, key, "5901590159015901590159015901590159015901"))

	firstClient := intakeTestClient(t)
	first := newIntake(tuningFor(t), firstClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	firstCtx, stopFirst := context.WithCancel(t.Context())
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		first.Run(firstCtx)
	}()
	t.Cleanup(func() {
		stopFirst()
		<-firstDone
	})
	bound := awaitUnconfiguredBind(t, publisher, "")
	if bound.Config.DeliverPolicy != natsgo.DeliverByStartSequencePolicy || bound.Config.OptStartSeq != stored+1 {
		t.Fatalf("the first bind created the durable with deliver policy %v at start %d, want an explicit start at %d, the message after the one the stream held",
			bound.Config.DeliverPolicy, bound.Config.OptStartSeq, stored+1)
	}
	stopFirst()
	<-firstDone

	second := newIntake(tuningFor(t), intakeTestClient(t), pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	startIntake(t, second)
	awaitUnconfiguredBind(t, publisher, bound.Config.DeliverSubject)
	publishPullRequestEnvelope(t, publisher, "acme", "widgets", after, openedForKey(after, key, "5902590259025902590259025902590259025902"))
	awaitAcknowledged(t, publisher)
	if rows := headRows(t, ctx, pool, after); rows != 1 {
		t.Fatalf("%s#%d, opened after the take-over, has %d head rows, want 1", deliveryRepo, after, rows)
	}
	if rows := headRows(t, ctx, pool, before); rows != 0 {
		t.Fatalf("%s#%d, opened before the first bind, has %d head rows, want 0: the take-over replayed the stream from before the durable's start", deliveryRepo, before, rows)
	}
}
