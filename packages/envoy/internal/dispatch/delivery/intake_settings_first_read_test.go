package delivery

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// In a deployment with delivery settings, the intake binds its durable only once the settings
// cache's first read has answered (Intake.Run, SettingsCache.Read): before it the cache holds nil,
// and a merge routed by nil writes no delivery row. A first intake makes the durable and stops, so
// a merge published later waits in the stream for the next holder. The test then holds every read
// of delivery_settings behind a lock, starts a settings cache and an intake on it, and publishes
// the merge while the cache's first read waits. Once the lock is released, the read answers, the
// intake binds, and the merge's delivery row is written. An intake that bound before the read
// would have handled the merge by no settings and acknowledged it, so no row would ever be written.
//
// What would make the intake bind is its first try for the durable (heldDurable.attempt), which its
// run loop makes only once Run's wait for the read has ended. So the row waits for Run to begin that
// wait (the tuning's awaitingSettings hook) with the read still held behind the lock, and reads that
// nothing has bound or been handled; and it reads, at the intake's first try (the attempted hook),
// that the cache's first read had answered by then.
func TestAnIntakeBindsOnlyOnceTheSettingsCachesFirstReadHasAnswered(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/42", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 42, "title": "feat: a widget", "html_url": "https://github.com/acme/widgets/pull/42",
			"user": map[string]any{"login": "octocat"}, "labels": []any{map[string]any{"name": "non-task"}},
			"created_at": "2024-01-01T00:00:00Z", "merged_at": "2024-01-01T02:00:00Z",
			"merge_commit_sha": "abc123", "additions": 10, "deletions": 2, "body": "",
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/42/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			mustEncode(t, w, []any{})
			return
		}
		mustEncode(t, w, []any{
			map[string]any{"commit": map[string]any{"message": "feat: a widget", "author": map[string]any{"date": "2024-01-01T00:00:00Z"}}},
		})
	})
	natsClient := intakeTestClient(t)

	stopFirst := runIntake(t, newIntake(tuningFor(t), intakeTestClient(t), pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	awaitBoundDurable(t, natsClient)
	stopFirst()
	// The durable the first intake made, which any take-over of the second would delete and create
	// again on a deliver subject of its own.
	first := deliveryConsumerInfo(t, natsClient)

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds delivery_settings: %v", err)
	}
	release := sync.OnceFunc(func() { _ = holder.Rollback(context.Background()) })
	t.Cleanup(release)
	if _, err := holder.Exec(ctx, `lock table delivery_settings in access exclusive mode`); err != nil {
		t.Fatalf("lock delivery_settings: %v", err)
	}

	cache := newSettingsCache(pool, nil, tuningFor(t))
	runSettingsCache(t, cache)
	// Set once the first intake has stopped, so they read the second alone.
	tuning := tuningFor(t)
	awaiting := make(chan struct{}, 1)
	awaited := func() {
		select {
		case awaiting <- struct{}{}:
		default:
		}
	}
	tuning.awaitingSettings.Store(&awaited)
	var firstTry sync.Once
	tried := make(chan bool, 1)
	attempted := func() {
		firstTry.Do(func() {
			select {
			case <-cache.Read():
				tried <- true
			default:
				tried <- false
			}
		})
	}
	tuning.attempted.Store(&attempted)
	startIntake(t, newIntake(tuning, natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), cache, unrenamed()))
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 42, map[string]string{
		"kind": "pr", "action": "closed", "repo": "acme/widgets", "number": "42",
		"title": "feat: a widget", "author": "octocat", "url": "https://github.com/acme/widgets/pull/42",
		"merged": "true", "merge_commit_sha": "abc123", "updated_at": "2024-01-01T02:00:00Z",
	})
	// The cache's first read queues behind the transaction that holds the lock, counted through that
	// transaction's own connection (pg_blocking_pids), so no other database's waiter counts.
	storetest.WaitForLockWaiters(t, holder, 1)
	testwait.Receive(t, "the intake's Run to begin its wait for the settings cache's first read", awaiting)
	if cache.Load() != nil {
		t.Fatalf("control: the settings cache holds %+v while its first read waits on the lock", cache.Load())
	}
	count := func() int {
		var rows int
		if err := pool.QueryRow(ctx, `select count(*) from delivery_pull_requests where repo = 'acme/widgets' and number = 42`).Scan(&rows); err != nil {
			t.Fatalf("count the delivery rows of acme/widgets#42: %v", err)
		}
		return rows
	}
	if info := deliveryConsumerInfo(t, natsClient); !info.Created.Equal(first.Created) || info.Config.DeliverSubject != first.Config.DeliverSubject ||
		info.Delivered.Consumer != first.Delivered.Consumer || count() != 0 {
		t.Fatalf("the intake took the durable over (created %s deliver subject %s, was %s %s), had %d message(s) delivered or wrote %d delivery rows while the settings cache's first read waited on the lock",
			info.Created, info.Config.DeliverSubject, first.Created, first.Config.DeliverSubject, info.Delivered.Consumer-first.Delivered.Consumer, count())
	}

	release()
	waitFor(t, func() bool { return count() == 1 }, func() string {
		return fmt.Sprintf("acme/widgets#42, merged while the settings cache's first read waited, has %d delivery rows after the read could answer, want 1: the intake handled it by no settings\n%s",
			count(), intakeStateReport(t, natsClient))
	})
	if !testwait.Receive(t, "the intake's first try for the durable", tried) {
		t.Fatal("the intake tried for the durable before the settings cache's first read had answered")
	}
}
