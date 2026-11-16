package delivery

import (
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// A pull-request envelope whose head row could not be stored at all is handed back for
// redelivery. Nothing else repairs a row that was never stored: the reconcile's passes read the
// rows that exist (ListStalePullRequestHeads), so an acknowledged envelope loses that pull
// request's head for good, and with it every `before_merge` result measured against that head.
//
// The write that cannot be made is the one UpsertPullRequestHead reports as ErrHeadNotStored,
// which handlePullRequestEnvelope returns; here branch_commits is away, so the transaction that
// writes the head row and the merge's seed together rolls back and no head row lands.
//
// The test asks NATS, not the handler: the durable must deliver the message again. Measured by
// the consumer's delivery count rather than by its pending count, because a message is pending
// while its handler runs, and what is under test is what happens after the handler returns.
func TestAnEnvelopeWhoseHeadRowCouldNotBeStoredIsHandedBack(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedDeliveryProject(t, ctx, pool)
	withoutBranchCommits(t, ctx, pool)

	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool),
		newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer

	// A merge, so the seed is written beside the head row and the write is the one that fails.
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 9101, map[string]string{
		"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets",
		"number": "9101", "author": "octocat", "base_ref": "integration/x",
		"head_sha":         "1111111111111111111111111111111111111111",
		"merge_commit_sha": "2222222222222222222222222222222222222222",
	})

	var info *natsgo.ConsumerInfo
	waitFor(t, 45*time.Second, func() bool {
		info = deliveryConsumerInfo(t, natsClient)
		return info.Delivered.Consumer-before >= 2
	}, func() string {
		return fmt.Sprintf("the envelope was delivered %d time(s) and never handed back, though no head row was stored: %+v",
			info.Delivered.Consumer-before, info)
	})

	var stored int
	if err := pool.QueryRow(ctx, "select count(*) from pull_request_heads where number = 9101").Scan(&stored); err != nil {
		t.Fatalf("read the head rows: %v", err)
	}
	if stored != 0 {
		t.Fatalf("head rows stored while branch_commits is away = %d, want none", stored)
	}
}
