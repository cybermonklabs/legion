package store

import (
	"context"
	"regexp"
	"slices"
	"strconv"
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
// which leaves the table as main has it, and the resume's own anchor (20261111181500). The nine
// versions of main's above the claim-death charge, the drop of the waiting-for-slot notice
// (20261109060737), the retirement's write mark (20261109191929), the backward-move counts with
// the hook prompt (20261110225246), the unmarked start of accepting, retro or merging
// (20261110233602), the kind of the rule that wrote a release mark (20261111001612), the issue's
// movement time (20261111010031), the declined ask (20261111021610), the tree-stalled mark
// (20261111041503) and the claim's failure cause (20261111212544), are main's own and are applied
// as main's run applied them.
//
// The set is computed rather than written (thisBranchsMigrations): the versions this branch added
// at or below main's newest (thisBranchsMigrationsAtOrBelowMainsNewest), and every embedded version
// above it. A version is a timestamp rather than a place in a line, so the branch wrote some of its
// own before main recorded its newest, and main took three more only after it recorded its newest
// (cybermonklabs/legion#3575's experiment assignments, 20261111045636, and
// cybermonklabs/legion#3519's pod volume notices and pod end, 20261111090000 and 20261111091500),
// which a database main migrated has not recorded either. That list is closed: a migration written
// now takes a timestamp of now, far above main's newest. Main recorded nothing above its newest, so
// a database main migrated lacks every version above it, and a migration added since joins the set
// with no edit here.
//
// What the upgrade needs of a migration of the set is what this test holds: one that rewrites
// outbox_kind_check admits the kind of every row still queued when it runs — main's
// (mainsOutboxKinds), less the ones an earlier migration of the set deleted — or the Migrate below
// fails on that row (SQLSTATE 23514); and one that rewrites or deletes a row main's daemon writes
// is read back after the upgrade below, as the merge-lane rows (20261029120800) and the queued
// phase_ended rows (20261119003157) are.
func thisBranchsMigrations(t *testing.T, all []migrations.Migration) map[int]bool {
	t.Helper()
	embedded := map[int]bool{}
	for _, migration := range all {
		embedded[migration.Version] = true
	}
	ours := map[int]bool{}
	for _, version := range thisBranchsMigrationsAtOrBelowMainsNewest {
		if version > mainsNewestMigration || !embedded[version] {
			t.Fatalf("thisBranchsMigrationsAtOrBelowMainsNewest names %d, which is not an embedded version at or below main's newest, %d", version, mainsNewestMigration)
		}
		ours[version] = true
	}
	for _, migration := range all {
		if migration.Version > mainsNewestMigration {
			ours[migration.Version] = true
		}
	}
	return ours
}

// thisBranchsMigrationsAtOrBelowMainsNewest are the versions of thisBranchsMigrations that stand at
// or below mainsNewestMigration, which main had not recorded.
var thisBranchsMigrationsAtOrBelowMainsNewest = []int{
	20261029120000, 20261029120100, 20261029120200, 20261029120300, 20261029120400,
	20261029120600, 20261029120700, 20261029120800, 20261029120900, 20261029121000,
	20261029121100, 20261029121200, 20261102003600, 20261105081500, 20261105183200,
	20261107095842, 20261108152000, 20261108190000, 20261109130413, 20261111045636, 20261111090000,
	20261111091500, 20261111181500,
}

// mainsNewestMigration is the newest version main carried at the forward merge of 2026-11-12
// that brought it into dispatch://INFRA-714's branch: the claim's failure cause. Every embedded
// version above it is this branch's (thisBranchsMigrations).
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

// queueRowOfKind writes one outbox row of kind for LEGION-208, with an empty payload, and answers
// the write's error rather than failing the test: a caller requires either that the outbox's kind
// check takes the row or that it refuses it.
func queueRowOfKind(store *Store, kind string) error {
	_, err := store.pool.Exec(context.Background(), `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
	return err
}

// A daemon of this release is deployed over a database main migrated, and that database's outbox
// holds the rows main's daemon left: a park queues a claim-release row and retries it for up to an
// hour (internal/workflow's linger, internal/daemon's claim release), so one pending at the
// upgrade is an ordinary state, and so is a merge-lane row of a release that still merged. The
// upgrade applies every version that database has not recorded, every kind list of this branch
// among them, and a list that leaves a kind out is refused by the constraint over the row that
// carries it (SQLSTATE 23514), which fails Migrate and leaves the daemon unable to boot.
//
// The database this test upgrades is main's: every migration up to and including main's own
// 20261108000545 except the two this branch adds below it, which is what main has recorded and
// what it leaves unrecorded. The negative control is the fix's own absence: with `claim_release`
// out of either list, the Migrate below fails on the pending release row.
func TestADatabaseMainMigratedUpgradesWithEveryRowItHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	ours := thisBranchsMigrations(t, all)
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
	// Every version above main's newest is this branch's own: one the set left out is applied here
	// as if main had recorded it, which no deployment's database ever did, and the upgrade this
	// test is for would go untested at that version.
	for _, migration := range all {
		if migration.Version > mainsNewestMigration && !ours[migration.Version] {
			t.Fatalf("%s is above main's newest version %d and thisBranchsMigrations left it out, so this test upgrades a database no deployment has",
				migration.Name, mainsNewestMigration)
		}
	}

	// The database now stands where main's run left it in every way but its kind constraint, which
	// the embedded migration wrote as the merged list; main's own text goes back over it here.
	if _, err := store.pool.Exec(ctx, mainsKindCheck); err != nil {
		t.Fatalf("put main's own kind constraint back: %v", err)
	}

	for _, kind := range mainsOutboxKinds {
		if err := queueRowOfKind(store, kind); err != nil {
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
	if err := queueRowOfKind(store, "claim_release"); err != nil {
		t.Fatalf("queue a claim release after the upgrade: %v", err)
	}
	for _, retired := range []string{"merge_queue_publish", "auto_merge_disable", "ready_merge"} {
		if err := queueRowOfKind(store, retired); err == nil {
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
	var ended int
	if err := store.pool.QueryRow(ctx, `select count(*) from outbox where kind = 'phase_ended'`).Scan(&ended); err != nil {
		t.Fatalf("count the phase-ended rows the upgrade left: %v", err)
	}
	if ended != 0 {
		t.Fatalf("phase_ended rows after the upgrade = %d, want 20261119003157 to have deleted the one main's daemon queued", ended)
	}
}

// thisBranchsMigrationsMainLacks are the versions of this branch that main has not recorded: main
// recorded each step (cybermonklabs/legion#4400) and, after it, read a conflict twice and merged
// the base forward (cybermonklabs/legion#4464, 20261119003157), fenced each value of the copy of
// Dispatch's issues (cybermonklabs/legion#4598, 20261120055717) and marked a launch that had Oh My
// Pi resume a claim's session by its id (cybermonklabs/legion#4581, 20261120042902), and every
// other embedded version, the step record's three, 20261119003157, 20261120042902 and
// 20261120055717 among them, is one main's run applied. One rewrites claims_suspended_by_check,
// one deletes the controller notices of two kinds the daemon no longer writes, and 20261126110259
// rewrites outbox_kind_check over the rows main's daemon left, and dispatch://INFRA-3931's state
// events (20261126163320), which main has not recorded either, rewrite it last. The upgrade
// applies all four whatever main's newest version is: Store.apply asks each migration whether its
// own version is recorded.
var thisBranchsMigrationsMainLacks = map[int]bool{
	20261115060739: true, 20261115121838: true, 20261126110259: true, stateEventsVersion: true,
}

// A daemon of this release is also deployed over a database main migrated once main recorded each
// step and each plan rating, read a conflict twice and read a root's design-gate basis, and that
// database's outbox can hold a row of each of main's newest kinds: main's daemon queues a
// step_record at each start and end of a step, its roll forward (20261117064152) queues one for
// each run a rollback left open, it queues a plan_record for each retro that rates its plan, a
// conflict_confirm for the second read of each head a first read found conflicting, a
// merge_forward for each message of a merge forward, a design_changes row for each reconcile of a
// tree's news of its approved design (20261119203047), a phase_ended row for each parent the
// admission pass moves into awaiting_children in the middle of its worker's phase, and a
// pending_basis_seed and a reparent_seed for each read of a root's design-gate basis and of a
// re-parented root (20261122154643). The upgrade applies this branch's kind lists with the merge
// holds change (20261126110259) and then the state events (20261126163320) over those rows, and
// `ADD CONSTRAINT … CHECK` validates the rows the table holds, so a list that left any of the
// eight kinds out would fail Migrate (SQLSTATE 23514) and leave the daemon unable to boot. After
// the upgrade the outbox takes a merge_holds_changed row, the kind 20261126110259 adds: both lists
// stand above 20261122154643's, which does not name the kind, and the state events' list, which
// names it too, is the last word of the check. The negative controls are each of the eight kinds
// taken out of either list, where the Migrate below then fails on that pending row, and
// merge_holds_changed taken out of the last one, where the row queued after the upgrade is refused.
func TestADatabaseMainMigratedWithRowsOfItsNewestKindsUpgrades(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if thisBranchsMigrationsMainLacks[migration.Version] {
			continue
		}
		if _, err := store.apply(ctx, migration); err != nil {
			t.Fatalf("apply %s as main's own run did: %v", migration.Name, err)
		}
	}
	kinds := []string{"step_record", "plan_record", "conflict_confirm", "merge_forward", "design_changes",
		"phase_ended", "pending_basis_seed", "reparent_seed"}
	for _, kind := range kinds {
		if _, err := store.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind); err != nil {
			t.Fatalf("queue the %s row main's daemon writes: %v", kind, err)
		}
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("upgrade the database main migrated with a row of each of %v pending: %v", kinds, err)
	}
	for _, kind := range kinds {
		var pending int
		if err := store.pool.QueryRow(ctx, `select count(*) from outbox where kind = $1`, kind).Scan(&pending); err != nil {
			t.Fatalf("count the %s rows the upgrade left: %v", kind, err)
		}
		if pending != 1 {
			t.Fatalf("%s rows after the upgrade = %d, want the pending one kept", kind, pending)
		}
	}
	if _, err := store.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('merge_holds_changed', 'LEGION-208', '{}', 0, now(), '')`); err != nil {
		t.Fatalf("queue a merge_holds_changed row after the upgrade: %v", err)
	}
}

// releaseDatabase is the database one release left, which this branch's daemon upgrades.
type releaseDatabase struct {
	name string
	// newest is the newest version the release embeds, and lacks each version numbered below
	// it that its database never recorded.
	newest int
	lacks  []int
	// seed writes the rows the release left before the upgrade, and check reads them after it.
	seed, check func(t *testing.T, store *Store)
}

// upgradeFrom builds the database release left from the embedded migrations numbered up to its
// newest but those it lacks, has seed write the release's rows, runs Migrate over it, and has
// check read the rows after it. It holds every upgrade to the same results, so a caller states
// only its release: the upgrade applies what the database lacks and what is numbered after its
// newest, records every embedded version once, and leaves the newest embedded the store's version.
func upgradeFrom(t *testing.T, release releaseDatabase) {
	t.Helper()
	ctx := context.Background()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, version := range release.lacks {
		if !slices.ContainsFunc(all, func(migration migrations.Migration) bool { return migration.Version == version }) {
			t.Fatalf("the embedded set has no migration %d, so the database %s migrated cannot lack it", version, release.name)
		}
	}
	store := emptyStore(t)
	var migrated []migrations.Migration
	later, newest := 0, 0
	for _, migration := range all {
		switch {
		case slices.Contains(release.lacks, migration.Version):
		case migration.Version <= release.newest:
			migrated = append(migrated, migration)
		default:
			later++
		}
		newest = max(newest, migration.Version)
	}
	if _, err := store.migrate(ctx, migrated); err != nil {
		t.Fatalf("migrate the database as %s did: %v", release.name, err)
	}
	if version, err := store.SchemaVersion(ctx); err != nil || version != release.newest {
		t.Fatalf("the database %s migrated reports version %d (%v), want its newest, %d", release.name, version, err, release.newest)
	}
	release.seed(t, store)

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("upgrade the database %s migrated: %v", release.name, err)
	}
	if want := len(release.lacks) + later; applied != want {
		t.Errorf("the upgrade applied %d migrations, want %d: the %d below its newest it lacked, %v, and the %d numbered after it",
			applied, want, len(release.lacks), release.lacks, later)
	}
	for _, migration := range all {
		if recorded := timesRecorded(t, store, migration.Version); recorded != 1 {
			t.Errorf("after the upgrade schema_version has %d rows for %s, want 1", recorded, migration.Name)
		}
	}
	if version, err := store.SchemaVersion(ctx); err != nil || version != newest {
		t.Errorf("after the upgrade the store reports version %d (%v), want the newest embedded, %d", version, err, newest)
	}
	release.check(t, store)
}

// todaysMainsNewestMigration is the pinned baseline this test upgrades from: main's newest
// migration at this branch's merge of main f21107b285c7, the sequence of each value of the copy of
// Dispatch's issues. Main has numbered newer migrations since, and this test does not follow them.
// It and the other migrations main numbered after this branch's claim_sessions (20261116144907) —
// the role-holders publication (20261116165926), the reviewer's reviewed commit (20261116234034),
// the step records' three (20261117000528, 20261117004117, 20261117064152), the plan record's kind
// list (20261117023932), the pull request statuses (20261117112334), the update ask mark's cutover
// (20261117200337), the conflict's second read and the merge forward (20261119003157) and the mark
// of a launch that had Oh My Pi resume a session by its id (20261120042902) — stand above it.
// A deployment of main applies them first, so this branch's daemon finds a database whose newest
// recorded version stands above its own new migration.
const todaysMainsNewestMigration = 20261120055717

// thisBranchsClaimSessions is this branch's one daemon migration main has not recorded.
const thisBranchsClaimSessions = 20261116144907

// A database main migrated to the pinned baseline, 20261120055717, has recorded every migration
// up to it but claim_sessions, which is numbered below it. Store.Migrate applies every
// migration that database has not recorded, whatever version stands highest (apply asks each
// migration whether its own version is recorded), so the upgrade applies 20261116144907 below that
// baseline: claim_sessions is created, and its backfill holds the session a claim of main's daemon
// recorded for that claim. So this branch's migration keeps its number.
//
// The database is built from the migrations numbered up to the pinned baseline alone (review round
// 13), so a migration added after this test still finds the database that baseline leaves, and the
// upgrade applies it too, after claim_sessions.
//
// The control (the pull request records the run): a loader that skips a migration numbered below
// the newest version recorded leaves claim_sessions unapplied, and the test fails naming it.
func TestADatabaseTodaysMainMigratedTakesTheClaimSessionsMigrationBelowItsNewest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const token, session = "legion-legion-legion-901-implementer", "ses_main"
	upgradeFrom(t, releaseDatabase{
		name:   "main at the pinned baseline",
		newest: todaysMainsNewestMigration,
		lacks:  []int{thisBranchsClaimSessions},
		seed: func(t *testing.T, store *Store) {
			if _, err := store.pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
				state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
				values ($1, 'legion', 'LEGION-901', 'LEGION-901', 'implementer', 1, $2, '/state/sessions/2026-11-17T00-00-00-000Z_ses_main.jsonl',
					'idle', 0, 0, 0, 0)`, token, session); err != nil {
				t.Fatalf("seed a claim main's daemon recorded: %v", err)
			}
		},
		check: func(t *testing.T, store *Store) {
			var holder string
			if err := store.pool.QueryRow(ctx, `select claim from claim_sessions where project = 'legion' and kind = 'session' and name = $1`,
				session).Scan(&holder); err != nil {
				t.Fatalf("read the hold of %s after the upgrade: %v", session, err)
			}
			if holder != token {
				t.Errorf("%s is held for %q after the upgrade, want %s, the claim that recorded it", session, holder, token)
			}
		},
	})
}

// mainsAppliedSequence is main's newest migration at each of this branch's merges of main from
// 77eac01e35d0 to 4a8d82bfd090: the applied sequence of the copy of Dispatch's issues.
// dispatch://INFRA-3320's gate basis (20261114064234) and phase awaiting_children (20261114232555)
// stand below it, and its kind list with the two basis seeds (20261122154643), this branch's
// newest, above it.
const mainsAppliedSequence = 20261121170714

// issueReleaseCommittedVersion and podReleaseGiveUpsVersion are #4194's two migrations, the mark
// that a release's teardown is committed and the record of a release given up on, which main took
// below this branch's newest at its merge of dc18a1fe.
const (
	issueReleaseCommittedVersion = 20261120210000
	podReleaseGiveUpsVersion     = 20261126001514
)

// At each merge of main since 77eac01e35d0 each side has recorded a migration the other has not,
// and this branch's daemon meets a database that either side left. One that main at 4a8d82bfd090,
// this head's merge of main before 4ad19fe8459c, migrated holds main's applied sequence
// (20261121170714) and the plan of an issue (20261117034837), which main took after 77eac01e35d0,
// and none of INFRA-3320's three versions, two of which stand below its newest. One that this
// branch's head before its merge of 77eac01e35d0 (10e98a85ef53) migrated holds the three and lacks
// both of main's, which stand below its newest, 20261122154643, and so does one that
// dispatch://INFRA-4730's head before its merge of main (910fabb572) migrated, whose newest is its
// own 20261122154645. One that this branch's head before its merge of a7fae897a408
// (03ac783258da), the merge that brought it the plan of an issue, migrated holds main's applied
// sequence too and lacks the plan of an issue alone. The merge of 4ad19fe8459c brought this branch
// main's plan of an experiment's draw (20261123165139), which stands above every version of this
// branch's: one that main at 4ad19fe8459c migrated holds it and lacks INFRA-3320's three and
// dispatch://INFRA-4730's two (20261122154644 and 20261122154645), every one of them below its
// newest, and one that this branch's head before that merge (dc73ab2f7f06) migrated holds every
// version this head embeds up to its newest, 20261122154643, and lacks every version above it.
// The merge of 8517faba31ac brought dispatch://INFRA-5463's clear of the start failure of a claim
// whose agent is up (20261123212127), which stands above every other version this head embeds:
// one that main at 8517faba31ac migrated holds it and lacks INFRA-3320's three and INFRA-4730's
// two, and one that this branch's head before that merge (8af2e555670f) migrated lacks that one,
// above its newest, and INFRA-4730's two, below it, and holds every other version this head
// embeds. Each of the databases of the earlier merges lacks it too, above their newest.
// dispatch://INFRA-1575's intake failures (20261120144241), which this branch takes with main
// 843a9f45a3a0, stand below the newest of every one of these databases, and none of them recorded
// it: main took it after 8517faba31ac, and this branch only at this merge. So each lacks it too.
// The merge of 4e851eb15d0b brought dispatch://INFRA-5471's mark of a re-admission that still owes
// its tree's members their return (20261123224717), which stands above every other version this
// head embeds: one that main at 4e851eb15d0b migrated holds it and the intake failures and lacks
// INFRA-3320's three and INFRA-4730's two, every one of them below its newest, and one that this
// branch's head before that merge (9ed30ada122a) migrated lacks that one alone, above its newest.
// Each of the databases of the earlier merges lacks it too, above their newest.
// The merge of main 1cd586b462b4, which this branch takes through dispatch://INFRA-4728, brought
// main's release that keeps no saved workspace (20261123050533), which stands above INFRA-4730's
// two and below the plan of an experiment's draw: no database of an earlier merge holds it, so
// the ones of main at 4ad19fe8459c, 8517faba31ac and 4e851eb15d0b and the ones of this branch at
// 8af2e555670f and 9ed30ada122a lack it below their newest, and the others above it.
// The merges of main fd2f03fb and bcc5d7e6 brought main's waiting line's issue priority
// (20261124060500) and #5349's cause times (20261124073512), which stand above every other version
// this head embeds but 20261124143026: the database of main with Dispatch priority holds the first
// and lacks the second, above its newest, and every other database here lacks both, above its
// newest.
// The merge of main c227d046 brought dispatch://INFRA-3149's release teardown (20261124143026),
// which stands above every other version this head embeds: every database here lacks it, above
// its newest.
// The merge of main a2ac5fd7 brought #4065's merge holds kind (20261126110259), which stands above
// every other version this head embeds: one that main at a2ac5fd7 migrated holds it and lacks
// INFRA-4730's three (20261122154644, 20261122154645 and 20261126035722), every one of them below
// its newest, and can hold a merge_holds_changed row, which 20261122154645's list, applied over it
// as the last word on that database, names. Its upgrade takes both merge_holds_changed and
// retention_end after it; with merge_holds_changed out of 20261122154645's list, the Migrate fails
// on the pending row instead.
// The merge of main ca20a512 brought #5487's claim session run (20261125021636) and #5488's P0
// bound (20261125181828), both below 20261126110259: one that main at a2ac5fd7 migrated lacks
// both, below its newest, and one that main at ca20a512 migrated holds them, an issue's resolved
// priority among their columns, and lacks INFRA-4730's three as main at a2ac5fd7 does. Neither
// adds an outbox kind, so 20261122154645's list stands as the last word on that database too.
// The merge of main dc18a1fe brought #4194's committed release (20261120210000) and give-ups
// (20261126001514), both below 20261126110259, and dispatch://INFRA-3931's state events
// (20261126163320), which stands above every other version this head embeds. One that main at
// dc18a1fe migrated holds all three and lacks INFRA-4730's three, below its newest, and can hold a
// row of each state kind, which 20261122154645's list, applied over it as the last word on that
// database, names: with a state kind out of that list, the Migrate fails on its pending row.
// One that this branch's head before that merge (40b2ad0e) migrated holds INFRA-4730's three and
// lacks main's three, two below its newest and the state events above it, and can hold a
// retention_end row, which the state events' list, applied over it as the last word there, names:
// with retention_end out of that list, the Migrate fails on that row. Every database of an earlier
// merge lacks the state events too, above its newest.
// Store.Migrate asks each embedded migration whether its own version is recorded (apply) and
// never compares it with the newest, so each upgrade applies the versions that database lacks,
// below its newest and above it alike, and leaves every embedded version recorded once.
//
// Main's database holds an outbox row of each kind its own kind check admits, read off that check:
// the basis seeds' list rewrites the check, which validates every row the table holds, so a kind
// main's daemon can queue that the list leaves out fails the upgrade. After it the outbox takes the
// two basis seeds as well. This branch's database at 10e98a85ef53 and dispatch://INFRA-4730's at
// 910fabb572 each hold a row of the copy of Dispatch's issues as the release before
// 20261121170714 writes one, and the upgrade gives it applied_seq at 0. This branch's database at
// 03ac783258da holds a phases row as the release before 20261117034837 writes one, and the upgrade
// leaves the row as it was, running no step (migration_20261117034837_test.go). Its database at
// dc73ab2f7f06 holds an experiment's draw as the release before 20261123165139 writes one, and the
// upgrade leaves the row of no group, its plan the empty word (migration_20261123165139_test.go).
// Its database at 8af2e555670f holds a ready claim and a failed one, each with a start failure, as
// the release before 20261123212127 writes them, and the upgrade empties the ready claim's and
// keeps the failed one's (migration_20261123212127_test.go). Its database at 9ed30ada122a holds an
// issue as this branch's daemon writes one, holding its own gate at its seen sequence, and the
// upgrade keeps both and gives it members_owed at its default, false, a column this release
// writes nowhere and reads only in its first boot's conversion and in `legion members-back`.
//
// Each database is built from the migrations numbered up to its own newest alone, so a migration
// added after this test still finds the database that release left, and the upgrade applies it
// too.
func TestTheDatabasesOfMainAndOfThisBranchAtTheMergeEachTakeWhatTheyLack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// queueMainsKinds writes a row of each kind main's kind check admits, and queueBasisSeeds a row
	// of each basis seed after the upgrade: the seed and the check of a database main migrated.
	queueMainsKinds := func(t *testing.T, store *Store) {
		var check string
		if err := store.pool.QueryRow(ctx, `select pg_get_constraintdef(oid) from pg_constraint
			where conrelid = 'outbox'::regclass and conname = 'outbox_kind_check'`).Scan(&check); err != nil {
			t.Fatalf("read the kind check main's database stands on: %v", err)
		}
		kinds := regexp.MustCompile(`'([a-z_]+)'::`).FindAllStringSubmatch(check, -1)
		if len(kinds) == 0 {
			t.Fatalf("the kind check main's database stands on names no kind: %s", check)
		}
		for _, kind := range kinds {
			if err := queueRowOfKind(store, kind[1]); err != nil {
				t.Fatalf("queue the %s row main's kind check admits: %v", kind[1], err)
			}
		}
		t.Logf("main's database holds a row of each of the %d kinds its kind check admits", len(kinds))
	}
	queueBasisSeeds := func(t *testing.T, store *Store) {
		for _, kind := range []string{"pending_basis_seed", "reparent_seed"} {
			if err := queueRowOfKind(store, kind); err != nil {
				t.Errorf("queue a %s row after the upgrade: %v", kind, err)
			}
		}
	}
	for _, release := range []releaseDatabase{
		{
			name:   "main at 4a8d82bfd090",
			newest: mainsAppliedSequence,
			lacks:  []int{gateBasisVersion, awaitingChildrenVersion, intakeFailuresVersion},
			seed:   queueMainsKinds,
			check:  queueBasisSeeds,
		},
		{
			name:   "main at 4ad19fe8459c",
			newest: experimentAssignmentPlanVersion,
			lacks:  []int{gateBasisVersion, awaitingChildrenVersion, intakeFailuresVersion, basisSeedsVersion, holdsGateVersion, issueFactsVersion, keepsNoWorkspaceVersion},
			seed:   queueMainsKinds,
			check:  queueBasisSeeds,
		},
		{
			name:   "main at 8517faba31ac",
			newest: startFailureAfterReadyVersion,
			lacks:  []int{gateBasisVersion, awaitingChildrenVersion, intakeFailuresVersion, basisSeedsVersion, holdsGateVersion, issueFactsVersion, keepsNoWorkspaceVersion},
			seed:   queueMainsKinds,
			check:  queueBasisSeeds,
		},
		{
			name:   "this branch at 10e98a85ef53",
			newest: basisSeedsVersion,
			lacks:  []int{issuePlansVersion, intakeFailuresVersion, mainsAppliedSequence},
			seed: func(t *testing.T, store *Store) {
				// The release before 20261121170714 names these columns and no other (record's
				// PutDispatchIssue).
				if _, err := store.pool.Exec(ctx, `insert into dispatch_issues (key, project, dispatch_seq, status, status_seq,
					parent, parent_seq, blockers, blockers_seq, labels, labels_seq, rank, rank_seq)
					values ('ACME-1', 'ACME', 5, 'todo', 5, null, 5, '{}', 5, '{legion}', 5, 'a0', 5)`); err != nil {
					t.Fatalf("write a row of the copy of Dispatch's issues as this branch's daemon did: %v", err)
				}
			},
			check: func(t *testing.T, store *Store) {
				const applied = `select row(key, dispatch_seq, applied_seq)::text from dispatch_issues order by key`
				if got, want := rowsAsText(t, store, applied), []string{"(ACME-1,5,0)"}; !slices.Equal(got, want) {
					t.Errorf("the copy after the upgrade (key, dispatch_seq, applied_seq) = %v, want %v: the row it held, at 0", got, want)
				}
			},
		},
		{
			name:   "INFRA-4730 at 910fabb572",
			newest: issueFactsVersion,
			lacks:  []int{issuePlansVersion, intakeFailuresVersion, mainsAppliedSequence},
			seed: func(t *testing.T, store *Store) {
				// That release names the same columns as the one above, and no other (record's
				// PutDispatchIssue).
				if _, err := store.pool.Exec(ctx, `insert into dispatch_issues (key, project, dispatch_seq, status, status_seq,
					parent, parent_seq, blockers, blockers_seq, labels, labels_seq, rank, rank_seq)
					values ('ACME-1', 'ACME', 5, 'todo', 5, null, 5, '{}', 5, '{legion}', 5, 'a0', 5)`); err != nil {
					t.Fatalf("write a row of the copy of Dispatch's issues as INFRA-4730's daemon did: %v", err)
				}
			},
			check: func(t *testing.T, store *Store) {
				const applied = `select row(key, dispatch_seq, applied_seq)::text from dispatch_issues order by key`
				if got, want := rowsAsText(t, store, applied), []string{"(ACME-1,5,0)"}; !slices.Equal(got, want) {
					t.Errorf("the copy after the upgrade (key, dispatch_seq, applied_seq) = %v, want %v: the row it held, at 0", got, want)
				}
			},
		},
		{
			name:   "this branch at 03ac783258da",
			newest: basisSeedsVersion,
			lacks:  []int{issuePlansVersion, intakeFailuresVersion},
			seed: func(t *testing.T, store *Store) {
				// The release before 20261117034837 names no step of a phases row.
				for _, seed := range []string{
					`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
						values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'reviewing', 1, 'needs_review', 'A', 1)`,
					`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_task)
						values ('LEGION-208', 'implementer', 'legion-LEGION-208-implementer', 'abc123', 2, '', 3, 'Continue root. Issue: LEGION-208. Phase: implementing.')`,
				} {
					if _, err := store.pool.Exec(ctx, seed); err != nil {
						t.Fatalf("write a phases row as this branch's daemon did before the plan of an issue: %v", err)
					}
				}
			},
			check: func(t *testing.T, store *Store) {
				const steps = `select row(issue, role, handoff_commit, entry, step_task, step_place)::text from phases order by issue, role`
				if got, want := rowsAsText(t, store, steps), []string{`(LEGION-208,implementer,abc123,3,"",0)`}; !slices.Equal(got, want) {
					t.Errorf("the phases rows after the upgrade (issue, role, handoff_commit, entry, step_task, step_place) = %v, want %v: the row it held, running no step", got, want)
				}
			},
		},
		{
			name:   "this branch at dc73ab2f7f06",
			newest: basisSeedsVersion,
			lacks:  []int{intakeFailuresVersion},
			seed: func(t *testing.T, store *Store) {
				// The release before 20261123165139 names these seven columns of a draw and no other
				// (record's PutAssignments).
				for _, seed := range []string{
					`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
						values ('LEGION-501', 'LEGION-501', 'LEGION', 'root', 'admitted', 1, 'in_progress', 'A', 1)`,
					`insert into experiment_assignments (issue_key, experiment, unit, variant, mode, drawn_at, carried)
						values ('LEGION-501', 'planning', 'issue', 'b', 'random', now(), false)`,
				} {
					if _, err := store.pool.Exec(ctx, seed); err != nil {
						t.Fatalf("write a draw as this branch's daemon did before the plan of a draw: %v", err)
					}
				}
			},
			check: func(t *testing.T, store *Store) {
				const drawn = `select row(issue_key, experiment, variant, plan)::text from experiment_assignments order by issue_key`
				if got, want := rowsAsText(t, store, drawn), []string{`(LEGION-501,planning,b,"")`}; !slices.Equal(got, want) {
					t.Errorf("the draws after the upgrade (issue_key, experiment, variant, plan) = %v, want %v: the row it held, of no group", got, want)
				}
			},
		},
		{
			name:   "this branch at 8af2e555670f",
			newest: experimentAssignmentPlanVersion,
			lacks:  []int{intakeFailuresVersion, holdsGateVersion, issueFactsVersion, keepsNoWorkspaceVersion},
			seed: func(t *testing.T, store *Store) {
				// The release before 20261123212127 keeps a claim's start failure through every later
				// ready (record's claims columns as that release writes them).
				for i, state := range []string{"ready", "failed"} {
					if _, err := store.pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
						launch_failures, prompt_failures, prompt_retires, uncertain_streak, last_start_failure)
						values ($1, 'legion', $2, $2, 'implementer', 1, '', '', $3, 0, 0, 0, 0, 'launch: the pod never became ready')`,
						"legion-legion-legion-"+strconv.Itoa(600+i)+"-implementer", "LEGION-"+strconv.Itoa(600+i), state); err != nil {
						t.Fatalf("write a %s claim as this branch's daemon did before the start failure clear: %v", state, err)
					}
				}
			},
			check: func(t *testing.T, store *Store) {
				const kept = `select row(state, last_start_failure <> '')::text from claims order by issue`
				if got, want := rowsAsText(t, store, kept), []string{"(ready,f)", "(failed,t)"}; !slices.Equal(got, want) {
					t.Errorf("the claims after the upgrade (state, holds a start failure) = %v, want %v: the ready claim's emptied, the failed claim's kept", got, want)
				}
			},
		},
		{
			name:   "main at 4e851eb15d0b",
			newest: membersOwedVersion,
			lacks:  []int{gateBasisVersion, awaitingChildrenVersion, basisSeedsVersion, holdsGateVersion, issueFactsVersion, keepsNoWorkspaceVersion},
			seed:   queueMainsKinds,
			check:  queueBasisSeeds,
		},
		{
			name:   "main with Dispatch priority",
			newest: issuePriorityVersion,
			lacks:  []int{gateBasisVersion, awaitingChildrenVersion, basisSeedsVersion, holdsGateVersion, issueFactsVersion},
			seed: func(t *testing.T, store *Store) {
				queueMainsKinds(t, store)
				if _, err := store.pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, priority)
					values ('LEGION-702', 'LEGION-702', 'LEGION', 'urgent root', 'admitted', 1, 'todo', 'A', 7, 0)`); err != nil {
					t.Fatalf("write an issue at P0 before the chain upgrade: %v", err)
				}
			},
			check: func(t *testing.T, store *Store) {
				queueBasisSeeds(t, store)
				const priority = `select priority::text from issues where key = 'LEGION-702'`
				if got := rowsAsText(t, store, priority); !slices.Equal(got, []string{"0"}) {
					t.Errorf("priority after the chain upgrade = %v, want Dispatch's P0", got)
				}
			},
		},
		{
			name:   "this branch at 9ed30ada122a",
			newest: startFailureAfterReadyVersion,
			lacks:  []int{keepsNoWorkspaceVersion},
			seed: func(t *testing.T, store *Store) {
				// That release writes an issue's own facts on its record (record's PutIssue): the mark
				// that it holds its own gate and the newest Dispatch sequence its consumer saw.
				if _, err := store.pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank,
					last_dispatch_seq, holds_gate, seen_seq)
					values ('LEGION-701', 'LEGION-701', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 7, true, 7)`); err != nil {
					t.Fatalf("write an issue as this branch's daemon did before the members' mark: %v", err)
				}
			},
			check: func(t *testing.T, store *Store) {
				const facts = `select row(key, holds_gate, seen_seq, members_owed, priority)::text from issues order by key`
				if got, want := rowsAsText(t, store, facts), []string{"(LEGION-701,t,7,f,)"}; !slices.Equal(got, want) {
					t.Errorf("the issues after upgrade = %v, want %v: own facts kept, no members owed, and null priority", got, want)
				}
			},
		},
		{
			name:   "main at a2ac5fd7",
			newest: mergeHoldsChangedVersion,
			lacks:  []int{holdsGateVersion, issueFactsVersion, claimsIssueVersion, sessionRunVersion, p0StartVersion},
			seed:   queueMainsKinds,
			check: func(t *testing.T, store *Store) {
				for _, kind := range []string{"merge_holds_changed", "retention_end"} {
					if err := queueRowOfKind(store, kind); err != nil {
						t.Errorf("queue a %s row after the upgrade: %v", kind, err)
					}
				}
			},
		},
		{
			name:   "main at ca20a512",
			newest: mergeHoldsChangedVersion,
			lacks:  []int{holdsGateVersion, issueFactsVersion, claimsIssueVersion},
			seed: func(t *testing.T, store *Store) {
				queueMainsKinds(t, store)
				if _, err := store.pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
					resolved_known, resolved_priority, priority_from)
					values ('LEGION-703', 'LEGION-703', 'LEGION', 'inherited P0', 'admitted', 1, 'todo', 'A', 7, true, 0, 'OTHER-7')`); err != nil {
					t.Fatalf("write an issue at an inherited P0 as main's daemon did: %v", err)
				}
			},
			check: func(t *testing.T, store *Store) {
				for _, kind := range []string{"merge_holds_changed", "retention_end"} {
					if err := queueRowOfKind(store, kind); err != nil {
						t.Errorf("queue a %s row after the upgrade: %v", kind, err)
					}
				}
				const resolved = `select row(resolved_known, resolved_priority, priority_from)::text from issues where key = 'LEGION-703'`
				if got, want := rowsAsText(t, store, resolved), []string{"(t,0,OTHER-7)"}; !slices.Equal(got, want) {
					t.Errorf("the resolved priority after the upgrade = %v, want %v, as main's daemon wrote it", got, want)
				}
			},
		},
		{
			name:   "main at dc18a1fe",
			newest: stateEventsVersion,
			lacks:  []int{holdsGateVersion, issueFactsVersion, claimsIssueVersion},
			seed:   queueMainsKinds,
			check: func(t *testing.T, store *Store) {
				var sequenced int
				if err := store.pool.QueryRow(ctx, `select count(*) from outbox
					where kind in ('state_change', 'state_admission', 'state_boot') and state_seq is not null`).Scan(&sequenced); err != nil || sequenced != 3 {
					t.Errorf("state rows after the upgrade = %d (%v), want the one of each state kind main's daemon left, kept with its sequence", sequenced, err)
				}
				for _, kind := range []string{"state_change", "state_admission", "state_boot", "merge_holds_changed", "retention_end"} {
					if err := queueRowOfKind(store, kind); err != nil {
						t.Errorf("queue a %s row after the upgrade: %v", kind, err)
					}
				}
			},
		},
		{
			name:   "this branch at 40b2ad0e",
			newest: mergeHoldsChangedVersion,
			lacks:  []int{issueReleaseCommittedVersion, podReleaseGiveUpsVersion},
			seed:   queueMainsKinds,
			check: func(t *testing.T, store *Store) {
				var retained int
				if err := store.pool.QueryRow(ctx, `select count(*) from outbox where kind = 'retention_end'`).Scan(&retained); err != nil || retained != 1 {
					t.Errorf("retention_end rows after the upgrade = %d (%v), want the one this branch's daemon left, kept", retained, err)
				}
				for _, kind := range []string{"state_change", "state_admission", "state_boot", "retention_end"} {
					if err := queueRowOfKind(store, kind); err != nil {
						t.Errorf("queue a %s row after the upgrade: %v", kind, err)
					}
				}
			},
		},
	} {
		t.Run(release.name, func(t *testing.T) { upgradeFrom(t, release) })
	}
}
