package store

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// thisBranchsMigrations are the versions this branch adds (dispatch://INFRA-714), which main has
// not recorded; main carries every other version of the embedded set and nothing of its own beyond
// them. Store.apply asks each migration whether its own version is recorded, so an upgrade of a
// database main migrated applies exactly these, in version order, over main's rows — the two that
// rewrite outbox_kind_check (20261105081500 and 20261107095842) after main's own 20261108000545,
// which added the claim-release kind.
var thisBranchsMigrations = []int{
	20261029120000, 20261029120100, 20261029120200, 20261029120300, 20261029120400,
	20261029120600, 20261029120700, 20261029120800, 20261029120900, 20261029121000,
	20261029121100, 20261029121200, 20261102003600, 20261105081500, 20261105183200,
	20261107095842, 20261108152000,
}

// mainsOutboxKinds is every kind main's daemon writes, which is the list main's own
// 20261108000545 admits. A database main migrated can carry a row of each at the upgrade.
var mainsOutboxKinds = []string{
	"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
	"linger_close", "workspace_remove", "merge_queue_publish", "issue_branch", "issue_suspend",
	"auto_merge_disable", "ready_merge", "check_wake", "check_look", "check_bound",
	"phase_ended", "issue_bundle", "issue_release", "gated_steps_publish", "claim_release",
}

// A daemon of this release is deployed over a database main migrated, and that database's outbox
// holds the rows main's daemon left: a park queues a claim-release row and retries it for up to an
// hour (internal/workflow's linger, internal/daemon's claim release), so one pending at the
// upgrade is an ordinary state, and so is a merge-lane row of a release that still merged. The
// upgrade applies every version that database has not recorded, this branch's two kind lists among
// them, and a list that leaves a kind out is refused by the constraint over the row that carries it
// (SQLSTATE 23514), which fails Migrate and leaves the daemon unable to boot.
//
// The database this test upgrades is main's: every migration up to and including main's own
// 20261108000545 except the two this branch adds below it, which is what main has recorded and
// what it leaves unrecorded. The negative control is the fix's own absence: with `claim_release`
// out of either list, the Migrate below fails on the pending release row.
func TestADatabaseMainMigratedUpgradesWithEveryRowItHolds(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	ours := map[int]bool{}
	for _, version := range thisBranchsMigrations {
		ours[version] = true
	}
	applied := 0
	for _, migration := range all {
		if ours[migration.Version] {
			continue
		}
		if _, err := store.apply(ctx, migration); err != nil {
			t.Fatalf("apply %s as main's own run did: %v", migration.Name, err)
		}
		applied++
	}
	if applied == 0 {
		t.Fatalf("main's run applied no migration, so this database is not main's")
	}

	queue := func(kind string) error {
		_, err := store.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	for _, kind := range mainsOutboxKinds {
		if err := queue(kind); err != nil {
			t.Fatalf("queue the %s row main's daemon writes: %v", kind, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("upgrade the database main migrated: %v", err)
	}

	var pending int
	if err := store.pool.QueryRow(ctx, `select count(*) from outbox where kind = 'claim_release'`).Scan(&pending); err != nil {
		t.Fatalf("count the claim-release rows the upgrade left: %v", err)
	}
	if pending != 1 {
		t.Fatalf("claim-release rows after the upgrade = %d, want the pending one kept", pending)
	}
	if err := queue("claim_release"); err != nil {
		t.Fatalf("queue a claim release after the upgrade: %v", err)
	}
	for _, retired := range []string{"merge_queue_publish", "auto_merge_disable", "ready_merge"} {
		if err := queue(retired); err == nil {
			t.Fatalf("the upgraded constraint took a %s row, want it refused: this release writes no merge-lane row", retired)
		}
	}
	var merged int
	if err := store.pool.QueryRow(ctx, `select count(*) from outbox where kind in ('merge_queue_publish', 'auto_merge_disable', 'ready_merge')`).Scan(&merged); err != nil {
		t.Fatalf("count the merge-lane rows the upgrade left: %v", err)
	}
	if merged != 0 {
		t.Fatalf("merge-lane rows after the upgrade = %d, want 20261029120800 to have deleted every one", merged)
	}
}
