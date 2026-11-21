package dashboard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The two commits of the rows below: this release's and the release before it's.
const gateOwnCommit, gateEarlierCommit = "2222222222222222222222222222222222222222", "1111111111111111111111111111111111111111"

// sharedDist is one web dist, which two releases can both ship byte for byte.
func sharedDist(t *testing.T) string {
	t.Helper()
	return writeDist(t, map[string][]byte{
		"index.html":         page("/assets/index-g1.js", "g1"),
		"assets/index-g1.js": []byte("console.log('g1')"),
	})
}

// dashboardRow reads the one dashboard_builds row of the database: its commit and how long ago
// its last start was.
func dashboardRow(t *testing.T, database *store.Store) (commit string, age time.Duration) {
	t.Helper()
	var rows int
	var seconds float64
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) over (), commit, extract(epoch from now() - last_started_at)
		from dashboard_builds`).Scan(&rows, &commit, &seconds); err != nil || rows != 1 {
		t.Fatalf("read dashboard_builds: %d rows (%v), want the one row both commits published", rows, err)
	}
	return commit, time.Duration(seconds * float64(time.Second))
}

// partitioned reads whether the database's embeddings is partitioned by project.
func partitioned(t *testing.T, database *store.Store) bool {
	t.Helper()
	var is bool
	if err := database.Pool.QueryRow(context.Background(),
		`select relkind = 'p' from pg_class where oid = 'embeddings'::regclass`).Scan(&is); err != nil {
		t.Fatalf("read whether embeddings is partitioned: %v", err)
	}
	return is
}

// A release whose web dist is byte for byte the release before it's does not hold its own
// embeddings partition swap. The earlier release published the dist two hours ago, so it is no
// build in the fleet; this release then publishes the same dist, which renews the one row and
// leaves the earlier commit on it (Publish: on conflict (build) do update set last_started_at). A
// gate that read that row's commit would take this release's boot for the earlier release's and
// wait for an hour after its last boot, which a fleet that boots a process at least hourly never
// reaches. The gate reads each publish under its own commit (dashboard_publishes), so the fill
// swaps. The row is checked to be in that shape first: one row, naming the earlier commit,
// renewed just now.
func TestAReleaseThatShipsTheLastReleasesWebFilesDoesNotHoldItsOwnSwap(t *testing.T) {
	ctx := context.Background()
	database := storetest.OpenUnpartitioned(t)
	dist := sharedDist(t)
	if _, err := Publish(ctx, database.Pool, dist, gateEarlierCommit); err != nil {
		t.Fatalf("the earlier release's publish: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		with build as (update dashboard_builds set first_started_at = now() - interval '2 hours', last_started_at = now() - interval '2 hours')
		update dashboard_publishes set published_at = now() - interval '2 hours' where commit = $1`, gateEarlierCommit); err != nil {
		t.Fatalf("date the earlier release's publish two hours ago: %v", err)
	}
	if _, err := Publish(ctx, database.Pool, dist, gateOwnCommit); err != nil {
		t.Fatalf("this release's publish of the same dist: %v", err)
	}
	if commit, age := dashboardRow(t, database); commit != gateEarlierCommit || age > time.Minute {
		t.Fatalf("dashboard_builds holds commit %s renewed %s ago, want the earlier commit renewed just now: this row proves nothing of a shared dist", commit, age)
	}

	if _, err := database.FillEmbeddingProjectKeys(ctx, gateOwnCommit); err != nil {
		t.Fatalf("fill by this release after its own publish of a shared dist = %v, want the swap: no build of the earlier commit has published for two hours", err)
	}
	if !partitioned(t, database) {
		t.Error("embeddings is not partitioned after the fill")
	}
}

// The reverse of the row above: an earlier build whose web dist is the one this release
// published first renews a row that names this release's commit, and still holds the swap, since
// its publish is recorded under its own commit. A gate that read the row's commit would see this
// release alone and swap with the earlier build's coverage pass about to run.
func TestAnEarlierBuildThatShipsThisReleasesWebFilesHoldsTheSwap(t *testing.T) {
	ctx := context.Background()
	database := storetest.OpenUnpartitioned(t)
	dist := sharedDist(t)
	if _, err := Publish(ctx, database.Pool, dist, gateOwnCommit); err != nil {
		t.Fatalf("this release's publish: %v", err)
	}
	if _, err := Publish(ctx, database.Pool, dist, gateEarlierCommit); err != nil {
		t.Fatalf("the earlier build's publish of the same dist: %v", err)
	}
	if commit, age := dashboardRow(t, database); commit != gateOwnCommit || age > time.Minute {
		t.Fatalf("dashboard_builds holds commit %s renewed %s ago, want this release's commit renewed just now: this row proves nothing of a shared dist", commit, age)
	}

	_, err := database.FillEmbeddingProjectKeys(ctx, gateOwnCommit)
	if !errors.Is(err, store.ErrEmbeddingsSwapWaits) || !strings.Contains(err.Error(), gateEarlierCommit) {
		t.Fatalf("fill beside an earlier build that just published a shared dist = %v, want ErrEmbeddingsSwapWaits naming %s", err, gateEarlierCommit)
	}
	if partitioned(t, database) {
		t.Error("embeddings is partitioned beside an earlier build that just published")
	}
}
