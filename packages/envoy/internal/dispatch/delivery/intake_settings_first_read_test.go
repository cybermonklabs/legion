package delivery

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// In a deployment with delivery settings, the intake binds its durable only once the settings
// cache's first read has answered (Intake.Run, SettingsCache.Read): before it the cache holds nil,
// and a merge routed by nil writes no delivery row. A first intake makes the durable and stops, so
// a merge published later waits in the stream for the next holder. The test then holds every read
// of the projects' delivery settings (project_delivery_settings, ListProjectSettings) behind a
// lock, starts a settings cache and an intake on it, and publishes the merge while the cache's
// first read waits. Once the lock is released, the read answers, the intake binds, and the merge's
// delivery row is written. An intake that bound before the read would have handled the merge by no
// rules and acknowledged it, so no row would ever be written.
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

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds project_delivery_settings: %v", err)
	}
	release := sync.OnceFunc(func() { _ = holder.Rollback(context.Background()) })
	t.Cleanup(release)
	if _, err := holder.Exec(ctx, `lock table project_delivery_settings in access exclusive mode`); err != nil {
		t.Fatalf("lock project_delivery_settings: %v", err)
	}

	cache := newSettingsCache(pool, SettingsWakes{}, tuningFor(t).log)
	runSettingsCache(t, cache)
	startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), cache, unrenamed()))
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 42, map[string]string{
		"kind": "pr", "action": "closed", "repo": "acme/widgets", "number": "42",
		"title": "feat: a widget", "author": "octocat", "url": "https://github.com/acme/widgets/pull/42",
		"merged": "true", "merge_commit_sha": "abc123", "updated_at": "2024-01-01T02:00:00Z",
	})
	waiting := func() int {
		var waiters int
		if err := pool.QueryRow(ctx, `
			select count(*) from pg_locks l join pg_class c on c.oid = l.relation
			where c.relname = 'project_delivery_settings' and not l.granted
		`).Scan(&waiters); err != nil {
			t.Fatalf("read the waiters on project_delivery_settings: %v", err)
		}
		return waiters
	}
	waitFor(t, 20*time.Second, func() bool { return waiting() > 0 }, func() string {
		return "control: the settings cache's first read never waited on the lock on project_delivery_settings"
	})
	// Long enough for an intake that did not wait for the read to bind and handle the merge.
	time.Sleep(3 * durableBindInterval)
	if cache.Load() != nil {
		t.Fatalf("control: the settings cache holds %+v while its first read waits on the lock", cache.Load())
	}

	release()
	count := func() int {
		var rows int
		if err := pool.QueryRow(ctx, `select count(*) from delivery_pull_requests where repo = 'acme/widgets' and number = 42`).Scan(&rows); err != nil {
			t.Fatalf("count the delivery rows of acme/widgets#42: %v", err)
		}
		return rows
	}
	waitFor(t, 30*time.Second, func() bool { return count() == 1 }, func() string {
		return fmt.Sprintf("acme/widgets#42, merged while the settings cache's first read waited, has %d delivery rows 30s after the read could answer, want 1: the intake handled it by no settings\n%s",
			count(), intakeStateReport(t, natsClient))
	})
}
