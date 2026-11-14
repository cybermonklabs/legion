package store

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// thisBranchsMigrations are the versions this branch adds (dispatch://INFRA-714), which main has
// not recorded; main carries every other version of the embedded set and nothing of its own beyond
// mainsNewestMigration. Store.apply asks each migration whether its own version is recorded, so an
// upgrade of a database main migrated applies exactly these, in version order, over main's rows —
// the three that rewrite outbox_kind_check (20261105081500, 20261107095842 and 20261108152000)
// after main's own 20261108000545, which added the claim-release kind, and after main's
// 20261110225246, which added the hook prompt's — so each of the three names that kind, since a
// database main migrated can carry a hook-prompt row the constraint would otherwise refuse — and
// after both the head-arrival column and the drop that takes it away again (20261109130413),
// which leaves the table as main has it, and the resume's own anchor (20261111181500), which is
// this branch's newest. The nine versions of main's above the claim-death
// charge, the drop of the waiting-for-slot notice (20261109060737), the retirement's write mark
// (20261109191929), the backward-move counts with the hook prompt (20261110225246), the
// unmarked start of a phase past its review (20261110233602), the kind of the rule that wrote a
// release mark (20261111001612), the issue's movement time (20261111010031), the declined ask
// (20261111021610), the tree-stalled mark (20261111041503) and the claim's failure cause
// (20261111212544), are
// main's own and are applied as main's run applied them. Every version of this branch's but five
// stands below main's newest: a migration's version is a timestamp rather than a place in a line,
// and the five above it are the phase entry's own (20261111225552, dispatch://INFRA-2800), which
// this branch wrote after main's claim-failure cause, and the four the next paragraph names.
//
// A branch migration this list forgets would be applied as one of main's, which is a database no
// deployment ever has, so the test below holds the list against the embedded set: every version
// above main's newest is this branch's and has to be named here. dispatch://INFRA-3219's claim
// leak-check cause (20261112180548), which came after, is above main's newest too, and above the
// phase entry's as well, so a database main migrated takes it in the same upgrade, after the
// phase entry, and it is named here with the rest. So are dispatch://INFRA-2981's kind list with
// the update ask (20261113082214) and its drop of the claim's two update answer marks
// (20261113110610), dispatch://INFRA-3276's record of what a relaunch ended (20261113082415), and
// dispatch://INFRA-3274's kind list with the merge-queue read and removal (20261114031356), which
// stand above all of them and are applied last, in version order: the kind list with the update
// ask, the record of what a relaunch ended, the drop, then the merge-queue kind list. The last of
// them is the last word of outbox_kind_check: the upgrade applies it after 20261029120800 has
// deleted main's merge-lane rows, and it names every other kind main's daemon writes
// (mainsOutboxKinds), with update_ask and the two merge-queue kinds besides, which main's daemon
// never writes.
var thisBranchsMigrations = []int{
	20261029120000, 20261029120100, 20261029120200, 20261029120300, 20261029120400,
	20261029120600, 20261029120700, 20261029120800, 20261029120900, 20261029121000,
	20261029121100, 20261029121200, 20261102003600, 20261105081500, 20261105183200,
	20261107095842, 20261108152000, 20261108190000, 20261109130413, 20261111181500,
	20261111225552, 20261112180548, 20261113082214, 20261113082415, 20261113110610, 20261114031356,
}

// mainsNewestMigration is the newest version main carries, the cause a claim records for its
// failure. Every embedded version above it is named in thisBranchsMigrations: main's newest came
// in above dispatch://INFRA-714's own at the forward merge of 2026-11-12, and six versions stand
// above it, the phase entry's 20261111225552, dispatch://INFRA-3219's 20261112180548,
// dispatch://INFRA-2981's 20261113082214 and 20261113110610, dispatch://INFRA-3276's
// 20261113082415 and dispatch://INFRA-3274's 20261114031356, which is why the loop below holds the
// list rather than counting it, so that a version added above any of them is named here rather
// than read as main's.
const mainsNewestMigration = 20261111212544

// mainsKindCheck is the constraint main's own 20261110235320 wrote, which is what a database main
// migrated carries. The embedded copy of that migration is the merged one — it names the kinds the
// merged daemon writes, `role_message` among them and the merge lane's three out, since a branch
// database with a pending role message would otherwise fail the upgrade on it — so main's own text
// is written here and put back over the database this test upgrades. Without it the seeds below
// could not be written at all, and the upgrade's one hard case, a merge-lane row of a release that
// still merged, would go untested.
const mainsKindCheck = `alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable', 'ready_merge', 'check_wake', 'check_look', 'check_bound',
  'phase_ended', 'issue_bundle', 'issue_release', 'gated_steps_publish', 'claim_release',
  'hook_prompt', 'design_gates_publish'))`

// mainsOutboxKinds is every kind main's daemon writes, which is the list main's own
// 20261110235320 admits. A database main migrated can carry a row of each at the upgrade.
var mainsOutboxKinds = []string{
	"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
	"linger_close", "workspace_remove", "merge_queue_publish", "issue_branch", "issue_suspend",
	"auto_merge_disable", "ready_merge", "check_wake", "check_look", "check_bound",
	"phase_ended", "issue_bundle", "issue_release", "gated_steps_publish", "claim_release",
	"hook_prompt", "design_gates_publish",
}

// A daemon of this release is deployed over a database main migrated, and that database's outbox
// holds the rows main's daemon left: a park queues a claim-release row and retries it for up to an
// hour (internal/workflow's linger, internal/daemon's claim release), so one pending at the
// upgrade is an ordinary state, and so is a merge-lane row of a release that still merged. The
// upgrade applies every version that database has not recorded, every kind list of this branch among
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
	// Every version above main's newest is this branch's own, so the list above has to name it:
	// one it forgets is applied here as if main had recorded it, which no deployment's database
	// ever did, and the upgrade this test is for would go untested at that version.
	for _, migration := range all {
		if migration.Version > mainsNewestMigration && !ours[migration.Version] {
			t.Fatalf("%s is above main's newest version %d and thisBranchsMigrations does not name it: add it, or this test upgrades a database no deployment has",
				migration.Name, mainsNewestMigration)
		}
	}

	// The database now stands where main's run left it in every way but its kind constraint, which
	// the embedded migration wrote as the merged list; main's own text goes back over it here.
	if _, err := store.pool.Exec(ctx, mainsKindCheck); err != nil {
		t.Fatalf("put main's own kind constraint back: %v", err)
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
