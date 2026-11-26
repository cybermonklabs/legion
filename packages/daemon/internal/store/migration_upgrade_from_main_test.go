package store

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// thisBranchsMigrations are the versions missing from the historical main fixture at
// mainsNewestMigration. They include later main releases as well as this branch's migrations.
// Store.apply asks each migration whether its own version is recorded, so an
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
// which a database main migrated has not recorded either. The answer loop's three tables
// (20261106214000, 20261106214001 and 20261106214002) stand below main's newest too: their branch
// wrote them before main recorded its newest, and a database main migrated has not recorded them
// either. That list is closed: a migration written now takes a timestamp of now, far above main's
// newest. Main recorded nothing above its newest, so a database main migrated lacks every version
// above it, and a migration added since joins the set with no edit here.
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
	20261106214000, 20261106214001, 20261106214002,
	20261107095842, 20261108152000, 20261108190000, 20261109130413, 20261111045636, 20261111090000,
	20261111091500, 20261111181500,
}

// mainsNewestMigration is the newest version main carried at the forward merge of 2026-11-12
// that brought it into dispatch://INFRA-714's branch: the claim's failure cause. Every embedded
// version above it is this branch's (thisBranchsMigrations). The fixtures of the two later tests
// start from later, pinned mains (todaysMainsNewestMigration and
// answerMigrationsFixtureNewestMigration).
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
// upgrade applies every version that database has not recorded, every kind list of this branch among
// them, and a list that leaves a kind out is refused by the constraint over the row that carries it
// (SQLSTATE 23514), which fails Migrate and leaves the daemon unable to boot.
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
// rewrites outbox_kind_check over the rows main's daemon left. The upgrade applies all three
// whatever main's newest version is: Store.apply asks each migration whether its own version is
// recorded.
var thisBranchsMigrationsMainLacks = map[int]bool{
	20261115060739: true, 20261115121838: true, 20261126110259: true,
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
// re-parented root (20261122154643). The upgrade applies this branch's kind list with the merge
// holds change (20261126110259) over those rows, and `ADD CONSTRAINT … CHECK` validates the rows
// the table holds, so a list that left any of the eight kinds out would fail Migrate (SQLSTATE
// 23514) and leave the daemon unable to boot. After the upgrade the outbox takes a
// merge_holds_changed row, the kind 20261126110259 adds: its list stands above 20261122154643's,
// which does not name the kind, and is the last word of the check. The negative controls are each
// of the eight kinds taken out of 20261126110259's list, where the Migrate below then fails on
// that pending row, and merge_holds_changed taken out of it, where the row queued after the
// upgrade is refused.
func TestADatabaseMainMigratedWithRowsOfItsNewestKindsUpgrades(t *testing.T) {
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

// thisBranchsClaimSessions is the migration that this historical fixture excludes.
const thisBranchsClaimSessions = 20261116144907

// A database main migrated to the pinned baseline, 20261120055717, has recorded every migration
// up to it but claim_sessions, which is numbered below it, and the answer loop's three tables
// (answerLoopTables), which stand below it too. Store.Migrate applies every
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
		lacks:  append([]int{thisBranchsClaimSessions}, answerLoopTables...),
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

// At each merge of main since 77eac01e35d0 each side has recorded a migration the other has not,
// and this branch's daemon meets a database that either side left. One that main at 4a8d82bfd090,
// this head's merge of main before 4ad19fe8459c, migrated holds main's applied sequence
// (20261121170714) and the plan of an issue (20261117034837), which main took after 77eac01e35d0,
// and none of INFRA-3320's three versions, two of which stand below its newest. One that this
// branch's head before its merge of 77eac01e35d0 (10e98a85ef53) migrated holds the three and lacks
// both of main's, which stand below its newest, 20261122154643. One that this branch's head before
// its merge of a7fae897a408 (03ac783258da), the merge that brought it the plan of an issue,
// migrated holds main's applied sequence too and lacks the plan of an issue alone. The merge of
// 4ad19fe8459c brought this branch main's plan of an experiment's draw (20261123165139), which
// stands above every version of this branch's: one that main at 4ad19fe8459c migrated holds it and
// lacks INFRA-3320's three, every one of them below its newest, and one that this branch's head
// before that merge (dc73ab2f7f06) migrated holds every other version this head embeds below
// 20261123165139 and lacks that one, above its newest. The merge of 8517faba31ac brought
// dispatch://INFRA-5463's clear of the start failure of a claim whose agent is up (20261123212127),
// which stood above every other version this head then embedded: one that main at 8517faba31ac
// migrated holds it and lacks INFRA-3320's three, and one that this branch's head before that merge
// (8af2e555670f) migrated lacks it, above its newest. Each of the databases of the earlier merges
// lacks it too, above their newest. The merge of
// 1cd586b462b4 brought main's release that keeps no saved workspace (20261123050533), which stands
// above INFRA-3320's newest and below the plan of an experiment's draw: no database of an earlier
// merge holds it, so the two of main at 4ad19fe8459c and 8517faba31ac and the one of this branch
// at 8af2e555670f lack it below their newest, and the others above it. The merge of 1af8002b42b5
// brought main's waiting line's issue priority (20261124060500) and #5349's cause times
// (20261124073512), and the merge of c227d0466191 brought dispatch://INFRA-3149's release teardown
// (20261124143026), which stands above them: the three stand above every other version this head
// embeds, and no database of an earlier merge holds any of them, so each database here lacks all
// three, above its newest. Store.Migrate asks each embedded migration whether its own version is
// recorded (apply) and never compares it with the newest, so each upgrade applies the versions that
// database lacks, below its newest and above it alike, and leaves every embedded version recorded
// once. None of these databases holds the answer loop's three tables (answerLoopTables), which
// dispatch://INFRA-204's branch wrote below every newest here and brought to this test at its
// merge of 26d8c86dd614, so each lacks them too, below its newest.
//
// Main's database holds an outbox row of each kind its own kind check admits, read off that check:
// the basis seeds' list rewrites the check, which validates every row the table holds, so a kind
// main's daemon can queue that the list leaves out fails the upgrade. After it the outbox takes the
// two basis seeds as well. This branch's database at 10e98a85ef53 holds a row of the copy of
// Dispatch's issues as the release before 20261121170714 writes one, and the upgrade gives it
// applied_seq at 0. Its database at 03ac783258da holds a phases row as the release before
// 20261117034837 writes one, and the upgrade leaves the row as it was, running no step
// (migration_20261117034837_test.go). Its database at dc73ab2f7f06 holds an experiment's draw as
// the release before 20261123165139 writes one, and the upgrade leaves the row of no group, its
// plan the empty word (migration_20261123165139_test.go). Its database at 8af2e555670f holds a
// ready claim and a failed one, each with a start failure, as the release before 20261123212127
// writes them, and the upgrade empties the ready claim's and keeps the failed one's
// (migration_20261123212127_test.go).
//
// Each database is built from the migrations numbered up to its own newest alone, so a migration
// added after this test still finds the database that release left, and the upgrade applies it
// too.
func TestTheDatabasesOfMainAndOfThisBranchAtTheMergeEachTakeWhatTheyLack(t *testing.T) {
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
			lacks:  append([]int{gateBasisVersion, awaitingChildrenVersion}, answerLoopTables...),
			seed:   queueMainsKinds,
			check:  queueBasisSeeds,
		},
		{
			name:   "main at 4ad19fe8459c",
			newest: experimentAssignmentPlanVersion,
			lacks:  append([]int{gateBasisVersion, awaitingChildrenVersion, basisSeedsVersion, keepsNoWorkspaceVersion}, answerLoopTables...),
			seed:   queueMainsKinds,
			check:  queueBasisSeeds,
		},
		{
			name:   "main at 8517faba31ac",
			newest: startFailureAfterReadyVersion,
			lacks:  append([]int{gateBasisVersion, awaitingChildrenVersion, basisSeedsVersion, keepsNoWorkspaceVersion}, answerLoopTables...),
			seed:   queueMainsKinds,
			check:  queueBasisSeeds,
		},
		{
			name:   "this branch at 10e98a85ef53",
			newest: basisSeedsVersion,
			lacks:  append([]int{issuePlansVersion, mainsAppliedSequence}, answerLoopTables...),
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
			name:   "this branch at 03ac783258da",
			newest: basisSeedsVersion,
			lacks:  append([]int{issuePlansVersion}, answerLoopTables...),
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
			lacks:  answerLoopTables,
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
			lacks:  append([]int{keepsNoWorkspaceVersion}, answerLoopTables...),
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
	} {
		t.Run(release.name, func(t *testing.T) { upgradeFrom(t, release) })
	}
}

// answerMigrationsFixtureNewestMigration is the pinned newest version of the answer-migration
// fixture: cybermonklabs/legion#5505's claim held-for, the newest migration on main at
// 26d8c86dd614, this branch's merge of main before a2ac5fd7aceb. It is a pin, not live main's
// newest: main numbers later migrations above it (the merge holds change, 20261126110259, among
// them), and the test counts each such embedded migration as pending rather than seeding it, as it
// counts this branch's 20261126170315. The seed includes issue
// priority, claim sessions, the experiment assignment plan, the claim cause times, #5372's release
// teardown, #4392's issue pod disks, #5385's production check failed looks, #5575's claims tree
// and dispatch://INFRA-3320's three: the gate basis, the phase awaiting_children and the kind list
// with the two basis seeds (20261122154643).
const answerMigrationsFixtureNewestMigration = 20261125223008

// currentMainsNewestMigration is main's newest migration at this branch's merge of main
// a2ac5fd7aceb: the kind list with the merge holds change (20261126110259), which stands above
// every answer-loop migration and below this branch's 20261126170315.
const currentMainsNewestMigration = 20261126110259

// answerLoopMigrations are the answer loop's six migrations, every one of them numbered below
// answerMigrationsFixtureNewestMigration, and answerLoopTables the first three of them, its three
// tables, which stand below the newest of every older fixture of this file too: no database main
// migrated records any of them.
var (
	answerLoopMigrations = []int{20261106214000, 20261106214001, 20261106214002, 20261124021727, 20261124072849, 20261124114223}
	answerLoopTables     = answerLoopMigrations[:3:3]
)

// A database migrated to the pinned fixture records every embedded migration up to
// answerMigrationsFixtureNewestMigration but the six answer-loop ones: the three tables, thread
// replies, call tools and released-agent messages. All six stand below the pin, so the upgrade
// applies each once though the database already records a higher version. Every embedded migration
// numbered above the pin is pending as well, so the upgrade applies it after the six, and the
// store's highest version becomes the newest embedded one: the pin itself when nothing newer is
// embedded. The seed is a pin, not current main: a database current main migrated holds main's
// migrations above the pin already, where this test applies them after the answer migrations.
func TestAPinnedMainFixtureAppliesTheAnswerMigrationsAndThenThoseAboveThePin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	answers := map[int]bool{}
	for _, version := range answerLoopMigrations {
		answers[version] = true
	}
	var seed []migrations.Migration
	found, later, newest := map[int]bool{}, 0, 0
	for _, migration := range all {
		switch {
		case answers[migration.Version]:
			found[migration.Version] = true
		case migration.Version <= answerMigrationsFixtureNewestMigration:
			seed = append(seed, migration)
		default:
			later++
		}
		newest = max(newest, migration.Version)
	}
	for version := range answers {
		if !found[version] {
			t.Fatalf("the embedded set has no answer migration %d, so this test cannot hold that the upgrade applies it", version)
		}
	}
	if _, err := store.migrate(ctx, seed); err != nil {
		t.Fatalf("migrate the database to the pinned fixture: %v", err)
	}
	if got, err := store.SchemaVersion(ctx); err != nil || got != answerMigrationsFixtureNewestMigration {
		t.Fatalf("the pinned fixture reports version %d (%v), want its pin, %d", got, err, answerMigrationsFixtureNewestMigration)
	}
	if applied, err := store.Migrate(ctx); err != nil || applied != len(answers)+later {
		t.Fatalf("the upgrade from the pinned fixture applied %d (%v), want %d: the %d answer migrations and the %d numbered after the pin",
			applied, err, len(answers)+later, len(answers), later)
	}
	for version := range answers {
		var count int
		if err := store.pool.QueryRow(ctx, `select count(*) from schema_version where version = $1::bigint`, version).Scan(&count); err != nil || count != 1 {
			t.Errorf("migration %d recorded %d times after the upgrade (%v), want once", version, count, err)
		}
	}
	if got, err := store.SchemaVersion(ctx); err != nil || got != newest {
		t.Errorf("after the upgrade the store reports version %d (%v), want the newest embedded, %d", got, err, newest)
	}
}

// A database current main migrated (currentMainsNewestMigration) can hold a queued pending-basis
// seed, a re-parent seed and a merge-holds message: the boot queues a pending-basis seed for every
// step that waits on an ancestor's approval request, a read that fails keeps its row on its
// backoff, and a merge_holds_changed row waits until the merger's session holds its role. This
// tree's upgrade applies the answer loop's six migrations below that newest, and 20261124114223
// among them writes the kind check again before 20261126170315 does: `ADD CONSTRAINT … CHECK`
// validates every row the table holds, so a list there that left any of the three kinds out would
// fail the upgrade (SQLSTATE 23514), and the daemon would fail its boot on every restart until
// someone deleted the rows. The upgrade keeps the three rows, and the outbox takes an
// agent_message row after it. The negative controls are 20261124114223's list without the two
// seeds, as it stood before this branch's merge of 26d8c86dd614, and without merge_holds_changed,
// as it stood before its merge of a2ac5fd7aceb: the upgrade then fails on the first seeded row of
// a kind the list leaves out.
func TestADatabaseCurrentMainMigratedKeepsItsQueuedBasisSeedsThroughTheUpgrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	queued := []string{"pending_basis_seed", "reparent_seed", "merge_holds_changed"}
	upgradeFrom(t, releaseDatabase{
		name:   "main at a2ac5fd7aceb",
		newest: currentMainsNewestMigration,
		lacks:  answerLoopMigrations,
		seed: func(t *testing.T, store *Store) {
			for _, kind := range queued {
				if err := queueRowOfKind(store, kind); err != nil {
					t.Fatalf("queue the %s row current main's daemon writes: %v", kind, err)
				}
			}
		},
		check: func(t *testing.T, store *Store) {
			for _, kind := range queued {
				var rows int
				if err := store.pool.QueryRow(ctx, `select count(*) from outbox where kind = $1`, kind).Scan(&rows); err != nil {
					t.Fatalf("count the %s rows the upgrade left: %v", kind, err)
				}
				if rows != 1 {
					t.Errorf("%s rows after the upgrade = %d, want the queued one kept", kind, rows)
				}
			}
			if err := queueRowOfKind(store, "agent_message"); err != nil {
				t.Errorf("queue an agent_message row after the upgrade: %v", err)
			}
		},
	})
}
