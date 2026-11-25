package store

import (
	"context"
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
// start from later mains (claimSessionsFixtureNewestMigration and todaysMainsNewestMigration).
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
	var ended int
	if err := store.pool.QueryRow(ctx, `select count(*) from outbox where kind = 'phase_ended'`).Scan(&ended); err != nil {
		t.Fatalf("count the phase-ended rows the upgrade left: %v", err)
	}
	if ended != 0 {
		t.Fatalf("phase_ended rows after the upgrade = %d, want 20261119003157 to have deleted the one main's daemon queued", ended)
	}
}

// claimSessionsFixtureNewestMigration is the newest migration on main when this regression
// fixture was recorded (f27003af63), the update ask mark's cutover.
const claimSessionsFixtureNewestMigration = 20261117200337

// thisBranchsClaimSessions is the migration that this historical fixture excludes.
const thisBranchsClaimSessions = 20261116144907

// A loader that skips versions below the highest recorded version leaves claim_sessions
// unapplied. Seed a claim before that backfill and read its holder after it.
// The answer-loop tables were not yet present at this historical base either.
func TestADatabaseTodaysMainMigratedTakesTheClaimSessionsMigrationBelowItsNewest(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	var mains []migrations.Migration
	ours, later, newest := false, 0, 0
	for _, migration := range all {
		switch {
		case migration.Version == thisBranchsClaimSessions:
			ours = true
		case migration.Version == 20261106214000, migration.Version == 20261106214001, migration.Version == 20261106214002:
			later++
		case migration.Version <= claimSessionsFixtureNewestMigration:
			mains = append(mains, migration)
		default:
			later++
		}
		newest = max(newest, migration.Version)
	}
	if !ours {
		t.Fatalf("the embedded set has no migration %d, so this test upgrades nothing", thisBranchsClaimSessions)
	}
	if _, err := store.migrate(ctx, mains); err != nil {
		t.Fatalf("migrate the database at the update ask cutover: %v", err)
	}
	if version, err := store.SchemaVersion(ctx); err != nil || version != claimSessionsFixtureNewestMigration {
		t.Fatalf("the database reports version %d (%v), want the update ask cutover, %d", version, err, claimSessionsFixtureNewestMigration)
	}
	const token, session = "legion-legion-legion-901-implementer", "ses_main"
	if _, err := store.pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', 'LEGION-901', 'LEGION-901', 'implementer', 1, $2, '/state/sessions/2026-11-17T00-00-00-000Z_ses_main.jsonl',
			'idle', 0, 0, 0, 0)`, token, session); err != nil {
		t.Fatalf("seed a claim main's daemon recorded: %v", err)
	}

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("upgrade the database today's main migrated: %v", err)
	}
	if applied != 1+later {
		t.Errorf("the upgrade applied %d migrations, want %d: claim_sessions and %d other missing migrations", applied, 1+later, later)
	}
	var recorded int
	if err := store.pool.QueryRow(ctx, `select count(*) from schema_version where version = $1::bigint`, thisBranchsClaimSessions).Scan(&recorded); err != nil {
		t.Fatalf("read whether the upgrade recorded %d: %v", thisBranchsClaimSessions, err)
	}
	if recorded != 1 {
		t.Errorf("the upgrade recorded claim_sessions (%d) %d times, want once", thisBranchsClaimSessions, recorded)
	}
	var holder string
	if err := store.pool.QueryRow(ctx, `select claim from claim_sessions where project = 'legion' and kind = 'session' and name = $1`,
		session).Scan(&holder); err != nil {
		t.Fatalf("read the hold of %s after the upgrade: %v", session, err)
	}
	if holder != token {
		t.Errorf("%s is held for %q after the upgrade, want %s, the claim that recorded it", session, holder, token)
	}
	if version, err := store.SchemaVersion(ctx); err != nil || version != newest {
		t.Errorf("after the upgrade the store reports version %d (%v), want the newest embedded, %d", version, err, newest)
	}
}

// todaysMainsNewestMigration is #5385's production check failed looks, the newest migration on
// main at d41b74eb. Main already includes issue priority, claim sessions, the experiment
// assignment plan, the claim cause times, #5372's release teardown and #4392's issue pod disks.
const todaysMainsNewestMigration = 20261125000600

// A database today's main migrated records every embedded migration but the six answer-loop
// ones: the three tables, thread replies, call tools and released-agent messages. All six stand
// below main's newest, so the upgrade applies each once though the database already records a
// higher version, and the store's highest version stays main's newest.
func TestADatabaseCurrentMainMigratedAppliesAnswerMigrationsBelowItsNewest(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	missing := map[int]bool{
		20261106214000: true, 20261106214001: true, 20261106214002: true,
		20261124021727: true, 20261124072849: true, 20261124114223: true,
	}
	var base []migrations.Migration
	for _, migration := range all {
		if migration.Version > todaysMainsNewestMigration {
			t.Fatalf("%s stands above main's %d: classify it before building the current-main fixture", migration.Name, todaysMainsNewestMigration)
		}
		if !missing[migration.Version] {
			base = append(base, migration)
		}
	}
	if _, err := store.migrate(ctx, base); err != nil {
		t.Fatalf("migrate as current main did: %v", err)
	}
	if got, err := store.SchemaVersion(ctx); err != nil || got != todaysMainsNewestMigration {
		t.Fatalf("current-main schema version = %d (%v), want %d", got, err, todaysMainsNewestMigration)
	}
	if applied, err := store.Migrate(ctx); err != nil || applied != len(missing) {
		t.Fatalf("upgrade from current main applied %d (%v), want %d", applied, err, len(missing))
	}
	for version := range missing {
		var count int
		if err := store.pool.QueryRow(ctx, `select count(*) from schema_version where version = $1::bigint`, version).Scan(&count); err != nil || count != 1 {
			t.Errorf("migration %d recorded %d times after the upgrade (%v), want once", version, count, err)
		}
	}
	if got, err := store.SchemaVersion(ctx); err != nil || got != todaysMainsNewestMigration {
		t.Errorf("upgraded schema version = %d (%v), want main's newest, %d, unchanged", got, err, todaysMainsNewestMigration)
	}
}
