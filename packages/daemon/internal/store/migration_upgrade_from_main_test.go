package store

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"

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

// todaysMainsNewestMigration is main's newest migration at this branch's last merge of main
// (f27003af63): the update ask mark's cutover. It and the other migrations main numbered after
// this branch's claim_sessions (20261116144907) — the role-holders publication (20261116165926),
// the reviewer's reviewed commit (20261116234034), the step records' three (20261117000528,
// 20261117004117, 20261117064152) and the pull request statuses (20261117112334) — stand above
// it. A deployment of main applies them first, so this branch's daemon finds a database whose
// newest recorded version stands above its own new migration.
const todaysMainsNewestMigration = 20261117200337

// thisBranchsClaimSessions is this branch's one daemon migration main has not recorded.
const thisBranchsClaimSessions = 20261116144907

// A database today's main migrated has recorded every migration up to main's newest,
// 20261117200337, but claim_sessions, which is numbered below it. Store.Migrate applies every
// migration that database has not recorded, whatever version stands highest (apply asks each
// migration whether its own version is recorded), so the upgrade applies 20261116144907 below that
// newest: claim_sessions is created, and its backfill holds the session a claim of main's daemon
// recorded for that claim. So this branch's migration keeps its number.
//
// The database is built from the migrations numbered up to main's newest alone (review round 13),
// so a migration added after this test still finds the database today's main left, and the upgrade
// applies it too, after claim_sessions.
//
// The control (the pull request records the run): a loader that skips a migration numbered below
// the newest version recorded leaves claim_sessions unapplied, and the test fails naming it.
func TestADatabaseTodaysMainMigratedTakesTheClaimSessionsMigrationBelowItsNewest(t *testing.T) {
	t.Parallel()
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
		case migration.Version <= todaysMainsNewestMigration:
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
		t.Fatalf("migrate the database as today's main did: %v", err)
	}
	if version, err := store.SchemaVersion(ctx); err != nil || version != todaysMainsNewestMigration {
		t.Fatalf("the database today's main migrated reports version %d (%v), want main's newest, %d", version, err, todaysMainsNewestMigration)
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
		t.Errorf("the upgrade applied %d migrations, want %d: claim_sessions and the %d numbered after main's newest", applied, 1+later, later)
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

// migrationBaseEnv names the commit whose daemon migrated the database that
// TestADatabaseMainMigratedTakesThisBranchsMigrationsLast upgrades. In CI it is the commit the run's
// tree lands onto: ci-job-daemon-go.yaml's plain step sets it to what
// .github/scripts/edited-migrations-base.sh prints. Anywhere else it can name any commit, for
// example the commit of the release a production database runs.
const migrationBaseEnv = "LEGION_TEST_MIGRATION_BASE"

// A deployed database has recorded every migration of the release that migrated it, the later
// ones included. The next release's daemon applies each embedded migration that database has not
// recorded: Store.apply asks each migration whether its own version is recorded. So each migration
// this tree adds runs after every migration of the base, also after those that sort above it. The
// two tests above apply this tree's migrations in version order, before main's later ones. So
// neither sees a later migration of main's that takes a name that one of this tree's adds.
//
// This test writes no list of versions. It reads the base's own migration files with git and
// migrates an empty database with them, which gives the database the base's daemon left. Then
// Store.Migrate applies every embedded version the base does not hold, after all of the base's. The
// test fails when the upgrade fails, when it applies a number of migrations other than the ones
// this tree adds, or when one of them is not recorded once. Where the tree adds no migration, the
// upgrade applies none, and the log line says so.
//
// The base is the commit migrationBaseEnv names. Where it is unset, the base is the merge base of
// HEAD with origin/main. Under GITHUB_ACTIONS an unset migrationBaseEnv fails the test, because the
// merge base is not the base of a pull request's merge commit. Anywhere else, a checkout with no
// origin/main skips the test and says why. A base that git cannot read fails the test everywhere.
//
// A pass does not show that an upgrade is safe. This test does not catch:
//   - a conflict that raises no error, such as `create ... if not exists` over an object of
//     another definition, or `add column if not exists` over a column of another type;
//   - wrong data: the database holds no rows, so no row and no backfill is checked;
//   - a partial state, such as a database that stopped between two migrations;
//   - the way back, a rollback to the base after the upgrade;
//   - Envoy's migrations, which internal/pgmigrate of packages/envoy applies;
//   - a database that daemons of two releases share at the same time;
//   - an upgrade from a commit other than the base: a production database of an older release
//     applies more migrations, in another order.
//
// The control (the pull request records the run): a base that holds a migration which adds
// issue_releases.turn_start with `if not exists`, and a tree that adds a migration which sorts
// below it and adds that column. The two tests above pass, and this one fails with SQLSTATE 42701.
func TestADatabaseMainMigratedTakesThisBranchsMigrationsLast(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base, source := deployedBase(t)
	commit, deployed := baseMigrations(t, base)
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	recorded := map[int]bool{}
	baseNewest := 0
	for _, migration := range deployed {
		recorded[migration.Version] = true
		baseNewest = max(baseNewest, migration.Version)
	}
	var last []int
	below, newest := 0, baseNewest
	for _, migration := range all {
		newest = max(newest, migration.Version)
		if recorded[migration.Version] {
			continue
		}
		last = append(last, migration.Version)
		if migration.Version < baseNewest {
			below++
		}
	}
	t.Logf("the base is %s (%s). It holds %d migrations, the newest %d. This tree adds %d migrations %v, and %d of them sort below %d.",
		commit, source, len(deployed), baseNewest, len(last), last, below, baseNewest)

	if _, err := store.migrate(ctx, deployed); err != nil {
		t.Fatalf("migrate the database as the daemon of %s did, with its own %d migrations: %v", commit, len(deployed), err)
	}
	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("upgrade the database the daemon of %s migrated, which applies this tree's migrations %v after all %d of the base's: %v",
			commit, last, len(deployed), err)
	}
	if applied != len(last) {
		t.Errorf("the upgrade applied %d migrations, want the %d this tree adds over %s", applied, len(last), commit)
	}
	for _, version := range last {
		var count int
		if err := store.pool.QueryRow(ctx, `select count(*) from schema_version where version = $1::bigint`, version).Scan(&count); err != nil {
			t.Fatalf("read whether the upgrade recorded %d: %v", version, err)
		}
		if count != 1 {
			t.Errorf("the upgrade recorded %d %d times, want once", version, count)
		}
	}
	if got, err := store.SchemaVersion(ctx); err != nil || got != newest {
		t.Errorf("after the upgrade the store reports version %d (%v), want the newest of the base and this tree, %d", got, err, newest)
	}
}

// deployedBase is the commit whose daemon migrated the database the test upgrades, and what named
// it (TestADatabaseMainMigratedTakesThisBranchsMigrationsLast says how it is chosen).
func deployedBase(t *testing.T) (base, source string) {
	t.Helper()
	if base := os.Getenv(migrationBaseEnv); base != "" {
		return base, migrationBaseEnv
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatalf("%s is unset on GitHub Actions: ci-job-daemon-go.yaml's plain step sets it to the commit .github/scripts/edited-migrations-base.sh prints",
			migrationBaseEnv)
	}
	out, err := runGit("", "merge-base", "HEAD", "origin/main")
	if err != nil {
		t.Skipf("no base to upgrade from: %s is unset, and %v. Set %s to a commit whose daemon migrated a database.",
			migrationBaseEnv, err, migrationBaseEnv)
	}
	return strings.TrimSpace(string(out)), "the merge base of HEAD with origin/main"
}

// baseMigrations reads the migrations of this package's migrations directory at base, with git,
// and loads them as Store.Migrate loads the embedded set. It returns base's full commit id beside
// them. A base that git cannot read, or one that holds no migration, fails the test: a set read
// from nothing is no database any daemon left.
func baseMigrations(t *testing.T, base string) (string, []migrations.Migration) {
	t.Helper()
	out, err := runGit("", "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		t.Fatalf("read the base %q: %v", base, err)
	}
	commit := strings.TrimSpace(string(out))
	out, err = runGit("", "rev-parse", "--verify", commit+":./migrations")
	if err != nil {
		t.Fatalf("find this package's migrations directory at %s: %v", commit, err)
	}
	tree := strings.TrimSpace(string(out))
	out, err = runGit("", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("find the root of this checkout: %v", err)
	}
	// git archive limits the archive to the directory it runs in, so it runs at the root.
	archive, err := runGit(strings.TrimSpace(string(out)), "archive", "--format=tar", tree)
	if err != nil {
		t.Fatalf("read the migrations of %s: %v", commit, err)
	}
	files := fstest.MapFS{}
	entries := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read the archive of the migrations of %s: %v", commit, err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(entries)
		if err != nil {
			t.Fatalf("read %s of %s: %v", header.Name, commit, err)
		}
		files[header.Name] = &fstest.MapFile{Data: data}
	}
	set, err := migrations.Load(files)
	if err != nil {
		t.Fatalf("load the migrations of %s: %v", commit, err)
	}
	if len(set) == 0 {
		t.Fatalf("%s holds no migration in this package's migrations directory, so no daemon of it migrated a database", commit)
	}
	return commit, set
}

// runGit runs git with args in dir, or in the test's own directory where dir is "", and returns
// its standard output. Its error carries git's standard error.
func runGit(dir string, args ...string) ([]byte, error) {
	command := exec.Command("git", args...)
	command.Dir = dir
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
