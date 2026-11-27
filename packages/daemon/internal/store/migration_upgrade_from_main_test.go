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

// thisBranchsMigrationsMainLacks are the versions this head embeds that main at f9809362, this
// branch's newest merge of main, has not recorded, read off that commit as releaseDatabase says:
// dispatch://INFRA-4730's three (20261122154644, 20261122154645 and 20261126035722), every one of
// them below main's newest, 20261126163320, and the merge's union of the kind lists
// (20261127104032), above it. Every other embedded version is one main's run applied, #4065's two
// (20261115060739 and 20261115121838) and its merge holds kind (20261126110259),
// dispatch://INFRA-3931's state events (20261126163320), cybermonklabs/legion#3712's four
// (20261112012123, 20261112215432, 20261118073048 and 20261122212816) and #5204's parent of a plan
// task (20261124022547) among them. 20261122154645 rewrites outbox_kind_check over the rows main's
// daemon left, and 20261127104032 rewrites it last, as it does on every database this release
// upgrades. The upgrade applies all four whatever main's newest version is: Store.apply asks each
// migration whether its own version is recorded.
var thisBranchsMigrationsMainLacks = map[int]bool{
	holdsGateVersion: true, issueFactsVersion: true, claimsIssueVersion: true,
	kindsAfterTheStateEventsVersion: true,
}

// A daemon of this release is also deployed over the database main at f9809362 migrated, and that
// database's outbox can hold a row of each of main's newest kinds: main's daemon queues a
// step_record at each start and end of a step, its roll forward (20261117064152) queues one for
// each run a rollback left open, it queues a plan_record for each retro that rates its plan, a
// conflict_confirm for the second read of each head a first read found conflicting, a
// merge_forward for each message of a merge forward, a design_changes row for each reconcile of a
// tree's news of its approved design (20261119203047), a phase_ended row for each parent the
// admission pass moves into awaiting_children in the middle of its worker's phase, and a
// pending_basis_seed and a reparent_seed for each read of a root's design-gate basis and of a
// re-parented root (20261122154643). The upgrade applies 20261122154645's kind list and then the
// merge's union of the kind lists (20261127104032) over those rows, and `ADD CONSTRAINT … CHECK`
// validates the rows the table holds, so a list that left any of the eight kinds out would fail
// Migrate (SQLSTATE 23514) and leave the daemon unable to boot. The embedded copies of the last two
// lists main's run applied, 20261126110259's and the state events', are the merged ones, which
// name retention_end where main's do not; no row of that kind is queued before the upgrade, so
// the rows the upgrade meets are rows main's daemon writes. After the upgrade the outbox takes a
// merge_holds_changed row, the kind 20261126110259 adds, and a retention_end row, the kind
// 20261122154645 adds: 20261127104032's list, which names both, is the last word of the check.
// The upgrade moves the store's schema version from the one main's daemon announced,
// 20261126163320, to 20261127104032's, which the merge's first boot announces (that migration's
// file says why), so the database main migrated reports a version below it and the upgraded store
// reports it. The negative controls are each of the eight kinds taken out of either of the two
// lists, where the Migrate below then fails on that pending row; merge_holds_changed or
// retention_end taken out of the last one, where the row queued after the upgrade is refused; and
// 20261127104032 taken out of thisBranchsMigrationsMainLacks, where the database main migrated
// already reports the merge's schema version.
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
	if version, err := store.SchemaVersion(ctx); err != nil || version >= kindsAfterTheStateEventsVersion {
		t.Fatalf("the database main migrated reports schema version %d (%v), want one below the merge's %d, which main's run never applied", version, err, kindsAfterTheStateEventsVersion)
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
	if version, err := store.SchemaVersion(ctx); err != nil || version != kindsAfterTheStateEventsVersion {
		t.Fatalf("after the upgrade the store reports schema version %d (%v), want the merge's %d, which the upgrade applies last", version, err, kindsAfterTheStateEventsVersion)
	}
	for _, kind := range []string{"merge_holds_changed", "retention_end"} {
		if err := queueRowOfKind(store, kind); err != nil {
			t.Fatalf("queue a %s row after the upgrade: %v", kind, err)
		}
	}
}

// releaseDatabase is the database one release left, which this branch's daemon upgrades. Its
// newest and its lacks are read off the release's commit, not reasoned out, and the next merge
// reads its own the same way: from the workspace root, with c the commit,
//
//	m=packages/daemon/internal/store/migrations
//	held=$(git ls-tree --name-only "$c" "$m/" | grep -oE '[0-9]{14}' | sort)
//	echo "$held" | tail -1
//	comm -23 <(ls "$m"/*.up.sql | grep -oE '[0-9]{14}' | sort) <(echo "$held") |
//		awk -v newest="$(echo "$held" | tail -1)" '$1 <= newest'
//
// prints newest and then lacks: every version this head embeds, numbered at or below newest, that
// the commit does not list.
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

// todaysMainsNewestMigration is the pinned baseline this test upgrades from: the newest migration
// of main at 9aff682b90, the parent of #4220 (46795520eb), which brought claim_sessions
// (20261116144907) to main: the update ask mark's cutover (20261117200337). Main has numbered
// newer migrations since, and this test does not follow them. It and the other migrations main
// held at that commit numbered after claim_sessions — the role-holders publication
// (20261116165926), the reviewer's reviewed commit (20261116234034), the step records' three
// (20261117000528, 20261117004117, 20261117064152) and the pull request statuses (20261117112334) —
// stand above claim_sessions. A deployment of main applies them first, so this branch's daemon
// finds a database whose newest recorded version stands above claim_sessions.
const todaysMainsNewestMigration = 20261117200337

// A database main at 9aff682b90 migrated, to the pinned baseline, has recorded every migration this
// head embeds numbered up to it but ten, which releaseDatabase's command prints for that commit:
// claim_sessions (20261116144907), which #4220 brought next; cybermonklabs/legion#3712's
// repository correction (20261112012123) and held task's resume (20261112215432), which main took
// only at da1b3f8258; #4065's two (20261115060739 and 20261115121838), which main took at
// a2ac5fd7; the gate basis (20261114064234), the phase awaiting_children (20261114232555), the
// project pause (20261115194357), the plan record's kind list (20261117023932) and the plan of an
// issue (20261117034837). Store.Migrate applies every migration that database has not recorded,
// whatever version stands highest (apply asks each migration whether its own version is
// recorded), so the upgrade applies 20261116144907 below that baseline: claim_sessions is
// created, and its backfill holds the session a claim of main's daemon recorded for that claim.
// So claim_sessions keeps its number.
//
// The database is built from the migrations numbered up to the pinned baseline alone, but those it
// lacks (review round 13), so a migration added after this test still finds the database that
// baseline leaves, and the upgrade applies it too, after claim_sessions.
//
// The controls: a loader that skips a migration numbered below the newest version recorded leaves
// claim_sessions unapplied, and the test fails naming it (the pull request records the run); and
// claim_sessions taken out of the lacks builds a database that holds it before main's claim is
// written, whose backfill then holds nothing, and the test fails on the session it does not hold.
func TestADatabaseTodaysMainMigratedTakesTheClaimSessionsMigrationBelowItsNewest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const token, session = "legion-legion-legion-901-implementer", "ses_main"
	upgradeFrom(t, releaseDatabase{
		name:   "main at 9aff682b90",
		newest: todaysMainsNewestMigration,
		lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, gateBasisVersion, awaitingChildrenVersion,
			suspendedByControllerVersion, dropControllerWakesVersion, projectPauseVersion, claimSessionsVersion,
			planRecordKindVersion, issuePlansVersion},
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

// suspendedByControllerVersion is #4065's controller source of a claim's suspension, which main
// took at a2ac5fd7 beside the drop of the controller wakes for nothing (dropControllerWakesVersion),
// both below the newest of every database of an earlier merge.
const suspendedByControllerVersion = 20261115060739

// readmittedApprovalArtifactVersion is the document a kept approval names, one of
// cybermonklabs/legion#3712's four, which main took at da1b3f8258 below the newest of most
// databases here; repositoryCorrectionVersion, deliveryResumeVersion and phaseEntryAwaitingVersion
// are the other three.
const readmittedApprovalArtifactVersion = 20261122212816

// At each merge of main since 77eac01e35d0 each side has recorded a migration the other has not,
// and this branch's daemon meets a database that either side left. Each case below states its
// database's newest and lacks as releaseDatabase's command prints them for its commit, in version
// order. The paragraphs that follow say what each merge brought and which database holds it; a
// database lacks, besides what one paragraph names, what a later one says it lacks.
// One that main at 4a8d82bfd090, this head's merge of main before 4ad19fe8459c, migrated holds
// main's applied sequence (20261121170714) and the plan of an issue (20261117034837), which main
// took after 77eac01e35d0, and none of INFRA-3320's three versions, two of which stand below its
// newest. One that this branch's head before its merge of 77eac01e35d0 (10e98a85ef53) migrated
// holds the three and lacks both of main's, which stand below its newest, 20261122154643, and so
// does one that dispatch://INFRA-4730's head before its merge of main (910fabb572) migrated, whose
// newest is its own 20261122154645. One that this branch's head before its merge of a7fae897a408
// (03ac783258da), the merge that brought it the plan of an issue, migrated holds main's applied
// sequence too and lacks the plan of an issue. The merge of 4ad19fe8459c brought this branch
// main's plan of an experiment's draw (20261123165139), which stood above every version of this
// branch's then: one that main at 4ad19fe8459c migrated holds it and lacks INFRA-3320's three and
// dispatch://INFRA-4730's two (20261122154644 and 20261122154645), every one of them below its
// newest, and one that this branch's head before that merge (dc73ab2f7f06) migrated holds the
// plan of an issue and main's applied sequence, below its newest, 20261122154643, and lacks every
// version above it.
// The merge of 8517faba31ac brought dispatch://INFRA-5463's clear of the start failure of a claim
// whose agent is up (20261123212127), which stood above every other version this branch embedded
// then: one that main at 8517faba31ac migrated holds it and lacks INFRA-3320's three and
// INFRA-4730's two, and one that this branch's head before that merge (8af2e555670f) migrated
// lacks that one, above its newest, and INFRA-4730's two, below it. Each of the databases of the
// earlier merges lacks it too, above their newest.
// dispatch://INFRA-1575's intake failures (20261120144241), which this branch takes with main
// 843a9f45a3a0, stand below the newest of every one of these databases, and none of them recorded
// it: main took it after 8517faba31ac, and this branch only at this merge. So each lacks it too.
// The merge of 4e851eb15d0b brought dispatch://INFRA-5471's mark of a re-admission that still owes
// its tree's members their return (20261123224717), which stood above every other version this
// branch embedded then: one that main at 4e851eb15d0b migrated holds it and the intake failures and
// lacks INFRA-3320's three and INFRA-4730's two, every one of them below its newest, and one that
// this branch's head before that merge (9ed30ada122a) migrated lacks that one, above its newest,
// and holds INFRA-4730's two. Each of the databases of the earlier merges lacks it too, above their
// newest.
// The merge of main 1cd586b462b4, which this branch takes through dispatch://INFRA-4728, brought
// main's release that keeps no saved workspace (20261123050533), which stands above INFRA-4730's
// two and below the plan of an experiment's draw: no database of an earlier merge holds it, so
// the ones of main at 4ad19fe8459c, 8517faba31ac and 4e851eb15d0b and the ones of this branch at
// 8af2e555670f and 9ed30ada122a lack it below their newest, and the others above it.
// The merges of main fd2f03fb and bcc5d7e6 brought main's waiting line's issue priority
// (20261124060500) and #5349's cause times (20261124073512), which stand above every version an
// earlier merge brought: the database of main with Dispatch priority, main at fd2f03fb, holds the
// first and lacks the second, above its newest, and every database of an earlier merge lacks
// both, above its newest.
// The merge of main c227d046 brought dispatch://INFRA-3149's release teardown (20261124143026),
// which stands above every version an earlier merge brought: every database of an earlier merge
// lacks it, above its newest.
// The merge of main f69d69d2f71 brought #5310's earlier givers of a plan's version
// (20261124033010), which stands below the issue priority: the database of main with Dispatch
// priority lacks it, below its newest, and every other database of an earlier merge lacks it,
// above its newest.
// The merge of main a2ac5fd7 brought #4065's merge holds kind (20261126110259), which stands above
// every version an earlier merge brought, and #4065's controller source of a claim's suspension
// (20261115060739) and its drop of the controller wakes for nothing (20261115121838), which stand
// below the newest of every database of an earlier merge, none of which recorded them: each of
// those lacks the two. One that main at a2ac5fd7 migrated holds all three and lacks INFRA-4730's
// three (20261122154644, 20261122154645 and 20261126035722), every one of them below its newest,
// and can hold a merge_holds_changed row, which 20261122154645's list, applied over it, names. Its
// upgrade takes both merge_holds_changed and retention_end after it; with merge_holds_changed out
// of 20261122154645's list, the Migrate fails on the pending row instead.
// The merge of main ca20a512 brought #5487's claim session run (20261125021636) and #5488's P0
// bound (20261125181828), both below 20261126110259: one that main at a2ac5fd7 migrated lacks
// both, below its newest, and one that main at ca20a512 migrated holds them, an issue's resolved
// priority among their columns, and lacks INFRA-4730's three as main at a2ac5fd7 does. Neither
// adds an outbox kind, so the lists the upgrade applies over that database are the ones it applies
// over main's at a2ac5fd7.
// The merge of main dc18a1fe brought #4194's committed release (20261120210000) and give-ups
// (20261126001514), both below 20261126110259, and dispatch://INFRA-3931's state events
// (20261126163320), above it. One that main at dc18a1fe migrated holds all three and lacks
// INFRA-4730's three, below its newest, and can hold a row of each state kind, which
// 20261122154645's list, applied over it, names: with a state kind out of that list, the Migrate
// fails on its pending row.
// One that this branch's head before that merge (40b2ad0e) migrated holds INFRA-4730's three and
// lacks main's three, two below its newest and the state events above it, and can hold a
// retention_end row, which the state events' list, applied over it, names: with retention_end out
// of that list, the Migrate fails on that row. Every database of an earlier merge lacks the three
// too: the committed release below its newest, the give-ups below the newest of main's at
// a2ac5fd7 and at ca20a512 and above every other's, and the state events above its newest.
// That merge wrote the union of the kind lists (20261127104032), which stands above every other
// version this head embeds: every database here but this branch's at f4a07e03, which holds it as
// its newest, lacks it, above its newest, so each of those upgrades applies it last. Its list,
// which names the same kinds as 20261122154645's and the state events', is the last word of the
// check on each database here.
// The merge of main 45f6e9d3 brought cybermonklabs/legion#3712's four, which main took at
// da1b3f8258: an issue's repository correction (20261112012123), the resume a held task carries
// (20261112215432), the wait on a resume's phase entry (20261118073048) and the document a kept
// approval names (20261122212816). None adds an outbox kind. No database of an earlier merge
// recorded any of them, and every one of them stands below the newest of each of those databases
// but 20261122212816, which stands above the newest of main's at 4a8d82bfd090 and of this
// branch's at 10e98a85ef53, 910fabb572, 03ac783258da and dc73ab2f7f06: each of those databases
// lacks the four, the ones below its newest among its lacks. One that main at 45f6e9d3 migrated
// holds them, an issue whose repository was corrected and an approval kept with its document
// among its rows, and lacks INFRA-4730's three, below its newest, 20261126163320, as main's at
// dc18a1fe does. One that this branch's head before that merge (f4a07e03) migrated lacks the four,
// every one of them below its newest, 20261127104032, and can hold a phase entry a resume raised
// marked to be sent as written (entry_verbatim), which 20261118073048 gives the empty wait.
// The merge of main f9809362 brought #5204's parent of a plan task (20261124022547), a column it
// adds to 20261117034837's plan_tasks, which stands below the issue priority (20261124060500).
// It adds no outbox kind. No database of an earlier merge recorded it: the one of main with
// Dispatch priority, main's at a2ac5fd7, ca20a512, dc18a1fe and 45f6e9d3 and this branch's at
// 40b2ad0e and f4a07e03 lack it below their newest, and every other one above its newest. One that
// main at f9809362 migrated holds it, a plan task whose block names its parent among its rows, and
// lacks INFRA-4730's three, below its newest, 20261126163320, as main's at 45f6e9d3 does.
// Store.Migrate asks each embedded migration whether its own version is recorded (apply) and
// never compares it with the newest, so each upgrade applies the versions that database lacks,
// below its newest and above it alike, and leaves every embedded version recorded once.
//
// Main's database holds an outbox row of each kind its own kind check admits (queueMainsKinds):
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
// Main's database at 45f6e9d3 holds an issue whose repository a correction moved, with the
// repository it left and the moment, and the approval its re-admission kept with the document
// approved, as #3712's daemon writes them (record's CorrectIssueRepository and
// KeepReadmittedApproval), and the upgrade keeps all three and gives the issue, a root, its own
// gate (20261122154644). This branch's database at f4a07e03 holds a root architect's entry that a
// resume raised, marked to be sent as written as the release before 20261118073048 marks it, and
// the upgrade gives it the empty wait (migration_20261118073048_test.go). Main's database at
// f9809362 holds a plan task whose block names its parent, as #5204's daemon writes it (record's
// PutPlan), and the upgrade keeps the parent (migration_20261124022547_test.go).
//
// Each database is built from the migrations numbered up to its own newest alone, but those it
// lacks, so a migration added after this test still finds the database that release left, and the
// upgrade applies it too.
func TestTheDatabasesOfMainAndOfThisBranchAtTheMergeEachTakeWhatTheyLack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// queueKinds writes a row of each kind the kind check the database stands on admits but those
	// of skipped: the rows the daemon that migrated it could leave.
	queueKinds := func(t *testing.T, store *Store, skipped ...string) {
		var check string
		if err := store.pool.QueryRow(ctx, `select pg_get_constraintdef(oid) from pg_constraint
			where conrelid = 'outbox'::regclass and conname = 'outbox_kind_check'`).Scan(&check); err != nil {
			t.Fatalf("read the kind check the database stands on: %v", err)
		}
		kinds := regexp.MustCompile(`'([a-z_]+)'::`).FindAllStringSubmatch(check, -1)
		if len(kinds) == 0 {
			t.Fatalf("the kind check the database stands on names no kind: %s", check)
		}
		queued := 0
		for _, kind := range kinds {
			if slices.Contains(skipped, kind[1]) {
				continue
			}
			if err := queueRowOfKind(store, kind[1]); err != nil {
				t.Fatalf("queue the %s row the database's kind check admits: %v", kind[1], err)
			}
			queued++
		}
		t.Logf("the database holds a row of each of the %d kinds its kind check admits but %v", queued, skipped)
	}
	// queueMainsKinds is queueKinds for a database main migrated: a row of each kind main's own
	// kind check at its commit admits. A database of main at a2ac5fd7 or later stands on the
	// embedded copy of 20261126110259 or 20261126163320, this branch's merged list, which names
	// retention_end, and main's own list at each of those commits does not (git show
	// <commit>:packages/daemon/internal/store/migrations/<file>, as mainsKindCheck is read), so no
	// row of it is queued. Every older database of main stands on 20261119203047's list, which is
	// main's own text. queueBasisSeeds writes a row of each basis seed after the upgrade: the seed
	// and the check of a database main migrated.
	queueMainsKinds := func(t *testing.T, store *Store) { queueKinds(t, store, "retention_end") }
	queueBasisSeeds := func(t *testing.T, store *Store) {
		for _, kind := range []string{"pending_basis_seed", "reparent_seed"} {
			if err := queueRowOfKind(store, kind); err != nil {
				t.Errorf("queue a %s row after the upgrade: %v", kind, err)
			}
		}
	}
	// keepsMainsStateRows is the check of a database main migrated once it wrote the state events:
	// the row of each state kind queueMainsKinds left is kept with its sequence, and the outbox
	// takes a row of each state kind, of merge_holds_changed and of retention_end after the upgrade.
	keepsMainsStateRows := func(t *testing.T, store *Store) {
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
	}
	for _, release := range []releaseDatabase{
		{
			name:   "main at 4a8d82bfd090",
			newest: mainsAppliedSequence,
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, gateBasisVersion, awaitingChildrenVersion,
				suspendedByControllerVersion, dropControllerWakesVersion, phaseEntryAwaitingVersion, intakeFailuresVersion,
				issueReleaseCommittedVersion},
			seed:  queueMainsKinds,
			check: queueBasisSeeds,
		},
		{
			name:   "main at 4ad19fe8459c",
			newest: experimentAssignmentPlanVersion,
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, gateBasisVersion, awaitingChildrenVersion,
				suspendedByControllerVersion, dropControllerWakesVersion, phaseEntryAwaitingVersion, intakeFailuresVersion,
				issueReleaseCommittedVersion, basisSeedsVersion, holdsGateVersion, issueFactsVersion,
				readmittedApprovalArtifactVersion, keepsNoWorkspaceVersion},
			seed:  queueMainsKinds,
			check: queueBasisSeeds,
		},
		{
			name:   "main at 8517faba31ac",
			newest: startFailureAfterReadyVersion,
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, gateBasisVersion, awaitingChildrenVersion,
				suspendedByControllerVersion, dropControllerWakesVersion, phaseEntryAwaitingVersion, intakeFailuresVersion,
				issueReleaseCommittedVersion, basisSeedsVersion, holdsGateVersion, issueFactsVersion,
				readmittedApprovalArtifactVersion, keepsNoWorkspaceVersion},
			seed:  queueMainsKinds,
			check: queueBasisSeeds,
		},
		{
			name:   "this branch at 10e98a85ef53",
			newest: basisSeedsVersion,
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, suspendedByControllerVersion,
				dropControllerWakesVersion, issuePlansVersion, phaseEntryAwaitingVersion, intakeFailuresVersion,
				issueReleaseCommittedVersion, mainsAppliedSequence},
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
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, suspendedByControllerVersion,
				dropControllerWakesVersion, issuePlansVersion, phaseEntryAwaitingVersion, intakeFailuresVersion,
				issueReleaseCommittedVersion, mainsAppliedSequence},
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
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, suspendedByControllerVersion,
				dropControllerWakesVersion, issuePlansVersion, phaseEntryAwaitingVersion, intakeFailuresVersion,
				issueReleaseCommittedVersion},
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
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, suspendedByControllerVersion,
				dropControllerWakesVersion, phaseEntryAwaitingVersion, intakeFailuresVersion, issueReleaseCommittedVersion},
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
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, suspendedByControllerVersion,
				dropControllerWakesVersion, phaseEntryAwaitingVersion, intakeFailuresVersion, issueReleaseCommittedVersion,
				holdsGateVersion, issueFactsVersion, readmittedApprovalArtifactVersion, keepsNoWorkspaceVersion},
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
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, gateBasisVersion, awaitingChildrenVersion,
				suspendedByControllerVersion, dropControllerWakesVersion, phaseEntryAwaitingVersion,
				issueReleaseCommittedVersion, basisSeedsVersion, holdsGateVersion, issueFactsVersion,
				readmittedApprovalArtifactVersion, keepsNoWorkspaceVersion},
			seed:  queueMainsKinds,
			check: queueBasisSeeds,
		},
		{
			name:   "main with Dispatch priority",
			newest: issuePriorityVersion,
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, gateBasisVersion, awaitingChildrenVersion,
				suspendedByControllerVersion, dropControllerWakesVersion, phaseEntryAwaitingVersion,
				issueReleaseCommittedVersion, basisSeedsVersion, holdsGateVersion, issueFactsVersion,
				readmittedApprovalArtifactVersion, planTasksParentRefVersion, planEarlierGiversVersion},
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
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, suspendedByControllerVersion,
				dropControllerWakesVersion, phaseEntryAwaitingVersion, issueReleaseCommittedVersion,
				readmittedApprovalArtifactVersion, keepsNoWorkspaceVersion},
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
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, phaseEntryAwaitingVersion,
				issueReleaseCommittedVersion, holdsGateVersion, issueFactsVersion, readmittedApprovalArtifactVersion,
				planTasksParentRefVersion, sessionRunVersion, p0StartVersion, podReleaseGiveUpsVersion, claimsIssueVersion},
			seed: queueMainsKinds,
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
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, phaseEntryAwaitingVersion,
				issueReleaseCommittedVersion, holdsGateVersion, issueFactsVersion, readmittedApprovalArtifactVersion,
				planTasksParentRefVersion, podReleaseGiveUpsVersion, claimsIssueVersion},
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
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, phaseEntryAwaitingVersion,
				holdsGateVersion, issueFactsVersion, readmittedApprovalArtifactVersion, planTasksParentRefVersion, claimsIssueVersion},
			seed:  queueMainsKinds,
			check: keepsMainsStateRows,
		},
		{
			name:   "this branch at 40b2ad0e",
			newest: mergeHoldsChangedVersion,
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, phaseEntryAwaitingVersion,
				issueReleaseCommittedVersion, readmittedApprovalArtifactVersion, planTasksParentRefVersion, podReleaseGiveUpsVersion},
			// This branch's daemon queues every kind of its own list, retention_end among them.
			seed: func(t *testing.T, store *Store) { queueKinds(t, store) },
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
		{
			name:   "main at 45f6e9d3",
			newest: stateEventsVersion,
			lacks:  []int{holdsGateVersion, issueFactsVersion, planTasksParentRefVersion, claimsIssueVersion},
			seed: func(t *testing.T, store *Store) {
				queueMainsKinds(t, store)
				// #3712's daemon writes a correction with the repository the issue left and its
				// moment (record's CorrectIssueRepository), and keeps a re-admission's approval with
				// the document approved (record's KeepReadmittedApproval).
				for _, seed := range []string{
					`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
						repo, default_branch, github_repo, repo_before, repo_corrected_at)
						values ('LEGION-705', 'LEGION-705', 'LEGION', 'root', 'planning', 2, 'in_progress', 'A', 7,
							'acme/gadgets', 'main', 'Acme/Gadgets', 'acme/widgets', now())`,
					`insert into readmitted_approvals (issue, approved_version, latest_version, artifact)
						values ('LEGION-705', 2, 3, 'art-705')`,
				} {
					if _, err := store.pool.Exec(ctx, seed); err != nil {
						t.Fatalf("write a corrected issue and its kept approval as main's daemon at 45f6e9d3 did: %v", err)
					}
				}
			},
			check: func(t *testing.T, store *Store) {
				keepsMainsStateRows(t, store)
				const kept = `select row(i.repo, i.repo_before, i.repo_corrected_at is not null, i.holds_gate, a.artifact)::text
					from issues i join readmitted_approvals a on a.issue = i.key order by i.key`
				if got, want := rowsAsText(t, store, kept), []string{"(acme/gadgets,acme/widgets,t,t,art-705)"}; !slices.Equal(got, want) {
					t.Errorf("the issue after the upgrade (repo, repo_before, corrected, holds_gate, kept artifact) = %v, want %v: the correction and the document kept, and the root holding its own gate", got, want)
				}
			},
		},
		{
			name:   "this branch at f4a07e03",
			newest: kindsAfterTheStateEventsVersion,
			lacks: []int{repositoryCorrectionVersion, deliveryResumeVersion, phaseEntryAwaitingVersion, readmittedApprovalArtifactVersion,
				planTasksParentRefVersion},
			seed: func(t *testing.T, store *Store) {
				// The release before 20261118073048 marks the entry a resume raises to be sent as
				// written (record's PutPhase, phases.entry_verbatim).
				for _, seed := range []string{
					`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
						values ('LEGION-704', 'LEGION-704', 'LEGION', 'root', 'planning', 1, 'in_progress', 'A', 7)`,
					`insert into phases (issue, role, claim, handoff_commit, rounds, verdict, entry, entry_task, entry_verbatim)
						values ('LEGION-704', 'architect', '', '', 0, '', 2, 'A person approved the spec of LEGION-704.', true)`,
				} {
					if _, err := store.pool.Exec(ctx, seed); err != nil {
						t.Fatalf("write a resume's entry as this branch's daemon did before the wait on it: %v", err)
					}
				}
			},
			check: func(t *testing.T, store *Store) {
				const entries = `select row(issue, role, entry, entry_awaiting)::text from phases order by issue, role`
				if got, want := rowsAsText(t, store, entries), []string{"(LEGION-704,architect,2,{})"}; !slices.Equal(got, want) {
					t.Errorf("the phases rows after the upgrade (issue, role, entry, entry_awaiting) = %v, want %v: the resume's entry, holding the empty wait", got, want)
				}
			},
		},
		{
			name:   "main at f9809362",
			newest: stateEventsVersion,
			lacks:  []int{holdsGateVersion, issueFactsVersion, claimsIssueVersion},
			seed: func(t *testing.T, store *Store) {
				queueMainsKinds(t, store)
				// #5204's daemon writes the parent a task's block names on the task's row (record's
				// PutPlan, plan_tasks.parent_ref).
				for _, seed := range []string{
					`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
						values ('LEGION-706', 1, 1, 'owner', 'artifact-706', 'legion-legion-legion-706-architect', now())`,
					`insert into plan_tasks (issue, generation, task, plan_version, block_text, state, parent_ref)
						values ('LEGION-706', 1, 'a', 1, 'Build the widget.', 'waiting', 'dispatch://LEGION-706/spec#b-r1')`,
				} {
					if _, err := store.pool.Exec(ctx, seed); err != nil {
						t.Fatalf("write a plan task that names its parent as main's daemon at f9809362 did: %v", err)
					}
				}
			},
			check: func(t *testing.T, store *Store) {
				keepsMainsStateRows(t, store)
				const parents = `select task || '=' || parent_ref from plan_tasks where issue = 'LEGION-706' order by task`
				if got, want := rowsAsText(t, store, parents), []string{"a=dispatch://LEGION-706/spec#b-r1"}; !slices.Equal(got, want) {
					t.Errorf("the plan tasks after the upgrade (task=parent_ref) = %v, want %v: the parent its block names, kept", got, want)
				}
			},
		},
	} {
		t.Run(release.name, func(t *testing.T) { upgradeFrom(t, release) })
	}
}
