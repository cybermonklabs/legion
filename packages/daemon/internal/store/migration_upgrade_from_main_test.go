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
// which leaves the table as main has it, the resume's own anchor (20261111181500), and the pull
// request's read stamp (20261112070503), which dispatch://INFRA-3027 adds on top of this branch
// and which a database the main before this branch migrated lacks as surely as this branch's own,
// so the upgrade applies it after them. The nine versions of main's above the claim-death
// charge, the drop of the waiting-for-slot notice (20261109060737), the retirement's write mark
// (20261109191929), the backward-move counts with the hook prompt (20261110225246), the
// unmarked start of accepting, retro or merging (20261110233602), the kind of the rule that wrote a
// release mark (20261111001612), the issue's movement time (20261111010031), the declined ask
// (20261111021610), the tree-stalled mark (20261111041503) and the claim's failure cause
// (20261111212544), are
// main's own and are applied as main's run applied them. A version of this branch's can stand
// below main's newest or above it, since a migration's version is a timestamp rather than a place
// in a line. Above it stand the phase entry's own (20261111225552, dispatch://INFRA-2800), which
// this branch wrote after main's claim-failure cause, and each other version above it that the
// next paragraph names. An upgrade of a database main migrated applies all of this list in
// version order, and the test below holds the list against the embedded set.
//
// A branch migration this list forgets would be applied as one of main's, which is a database no
// deployment ever has, so the test below holds the list against the embedded set: every version
// above main's newest is this branch's and has to be named here. That holds for a change built on
// this one too: INFRA-3036's 20261111214351, the mark of a slot released for a wait, stands above
// main's newest and below the phase entry's, and a database main migrated has not recorded it
// either, so the upgrade applies it with the rest. dispatch://INFRA-3219's claim leak-check cause
// (20261112180548), which came after, is above main's newest too, and above the phase entry's as
// well, so a database main migrated takes it in the same upgrade, after the phase entry, and it is
// named here with the rest, as is dispatch://INFRA-3027's pull request read stamp (20261112070503)
// for the same reason. So are dispatch://INFRA-2981's kind list with the update ask
// (20261113082214) and its drop of the claim's two update answer marks (20261113110610),
// dispatch://INFRA-3276's record of what a relaunch ended (20261113082415), and
// dispatch://INFRA-3274's kind list with the merge-queue read and removal (20261114031356), which
// stand above all of them and are applied after them, in version order: the kind list with the
// update ask, the record of what a relaunch ended, the drop, then the merge-queue kind list.
// dispatch://INFRA-175's design news (20261119203047) stands above those four and is named here
// the same way. It is the last word of outbox_kind_check but one: the upgrade applies it last of
// the kind lists but 20261123134547's, which restates its list with fix_job_read, after
// 20261029120800 has deleted main's merge-lane rows, and it names every other kind
// main's daemon writes (mainsOutboxKinds), with update_ask, the two merge-queue kinds,
// role_holders_publish, step_record, plan_record, conflict_confirm, merge_forward and
// design_changes besides, which main's daemon never writes.
// INFRA-3036's 20261111214351 and INFRA-3276's 20261113082415 each add one column, to issues and
// to claims, and leave the check as it stands.
// And so is dispatch://INFRA-3146's `phases.entry_moved_back` (20261113090512), which stands
// between INFRA-2981's two and adds one column beside the phase entry's, true on every row it
// finds and false by default after, and so needs no row of its own here
// (migration_20261113090512_test.go reads both). Store.Migrate applies every embedded migration a
// database has not recorded, so a database that took INFRA-2981's two before this one takes it
// in its next upgrade all the same.
// dispatch://INFRA-2614's architect stall marks (20261113181500), its architect entry upgrade
// (20261113182000), its claim handshake (20261114020537) and its phase entry's mark that a task is
// sent again as written (20261114061900) are named here too: an upgrade of a database main
// migrated takes each of them in version order with the rest, the upgrade after the phase entry
// whose columns it fills, and all four below the design news, and none of them touches
// outbox_kind_check.
// dispatch://INFRA-1997's commit statuses of the checked head (20261117112334) are named here too:
// that version stands above every other of this list but dispatch://INFRA-4109's 20261117200337,
// dispatch://INFRA-4374's 20261119003157, dispatch://INFRA-175's design news (20261119203047),
// dispatch://INFRA-3178's 20261120012547, dispatch://INFRA-4108's 20261120042902,
// dispatch://INFRA-4729's 20261120055717, the check states' 20261121065041, the phase verdict's
// removal (20261121074614), the copy's applied sequence, 20261121170714, the tester's return step,
// 20261123134347, the fix beside the other steps of a task, 20261123134547, the plan of an
// experiment's draw (20261123165139) and dispatch://INFRA-5463's 20261123212127, so an upgrade of a
// database main migrated applies it just before those thirteen. It adds
// pull_requests.statuses at its default, the empty list, and it touches no outbox_kind_check.
// The reviewer's reviewed commit (20261116234034, `phases.reviewed_head`) is named here for the
// same reason: it adds one column at its default, which every row it finds reads as no commit
// (migration_20261116234034_test.go), it stands below the design news too, and it touches no
// outbox_kind_check.
// cybermonklabs/legion#3575's experiment assignments (20261111045636, dispatch://INFRA-2566) are
// named here as well, though that version stands below main's newest: the migration was written
// after main recorded its newest, so a database main migrated has not recorded it, and the loop
// below, which holds this list only to the versions above main's newest, would otherwise apply it
// as one of main's own. It creates experiment_assignments and fills it from the issues' memory
// columns, which main's own 20261104073148 and 20261104153554 added, and it touches no
// outbox_kind_check.
// cybermonklabs/legion#3519's cache trimmer's two, the pod volume notices (20261111090000) and the
// claim's record of how its pod ended (20261111091500), are named here for the same reason: both
// stand below main's newest, but main took them after it recorded its newest, so a database main
// migrated has not recorded them. The first creates pod_volume_notices, the second adds
// claims.pod_end at its default, and neither touches outbox_kind_check.
// dispatch://INFRA-3759's project pause (20261115194357) adds the pause's three tables, two
// columns of pod_resumes and the outbox's held_by_pause, stands below the design news, and
// rewrites no kind list, so it needs no row of its own here (migration_20261115194357_test.go
// reads it).
// t-e-54's claim sessions (20261116144907) are named here too: it creates claim_sessions and fills
// it from each claim's session and the session it last abandoned (claims.abandoned_session, which
// this branch's 20261029121200 adds), it stands below the design news, and it touches no
// outbox_kind_check, so the design news's list stands over it.
// The kind list with the role-holders publication (20261116165926) stands above all of these but
// the reviewer's reviewed commit, the step records' three, the plan record's kind list
// (20261117023932), the plan of an issue (20261117034837), dispatch://INFRA-1997's
// 20261117112334, dispatch://INFRA-4109's cutover of the update ask's mark (20261117200337),
// dispatch://INFRA-4374's 20261119003157 and the design news: it names every kind the merge-queue
// list names and its own.
// The kind list with the step record (20261117000528) is named here too: it stands above the
// role-holders publication's and the reviewer's reviewed commit, and it restates
// 20261116165926's kinds with step_record. So is the kind list with the plan record
// (20261117023932), which stands above the step record's and restates its kinds with
// plan_record. The count of starts of each step (20261117004117) creates tables and a function of
// its own, and its roll forward (20261117064152) changes nothing on a database no rollback
// touched; neither touches outbox_kind_check, nor do the reviewed commit and the commit statuses,
// which each add a column at its default, nor the plan of an issue (20261117034837), which
// creates the three tables of the plan and adds `phases.step_task` and `phases.step_place`, each
// at a default that every row it finds reads as no plan (migration_20261117034837_test.go).
// The plan of an experiment's draw (20261123165139) adds `experiment_assignments.plan` at its
// default, which every row it finds reads as no group (migration_20261123165139_test.go), stands
// above every migration named here but dispatch://INFRA-5463's 20261123212127, and touches no
// outbox_kind_check.
// dispatch://INFRA-4109's cutover of the update ask's mark (20261117200337) is applied after all
// of these and touches no outbox_kind_check: it clears a claim's ask mark where that claim's ask
// is still queued.
// dispatch://INFRA-4374's kind list with the conflict's second read and the merge forward
// (20261119003157) stands above every other version of this list but the design news,
// dispatch://INFRA-3178's 20261120012547, dispatch://INFRA-4108's 20261120042902,
// dispatch://INFRA-4729's 20261120055717, the check states' 20261121065041, the phase verdict's
// removal (20261121074614), the copy's applied sequence, 20261121170714, the tester's return step,
// 20261123134347, the fix beside the other steps of a task, 20261123134547, the plan of an
// experiment's draw (20261123165139) and dispatch://INFRA-5463's 20261123212127: it names
// every kind the plan-record list names and its own two, phase_ended among them. The same
// migration deletes the queued phase_ended rows, the seeded one below among them, and adds
// pull_requests.conflict_read and pull_requests.merge_forward, both null on every row it finds.
// The design news's list, applied after it and after every other version of this list but
// 20261120012547, 20261120042902, 20261120055717, 20261121065041, 20261121074614, 20261121170714,
// 20261123134347, 20261123134547, 20261123165139 and 20261123212127, is the last of them but
// 20261123134547 to rewrite outbox_kind_check: it names those kinds and design_changes.
// dispatch://INFRA-3178's set-back sequence (20261120012547) is applied after all of these, the
// design news among them, and before dispatch://INFRA-4108's 20261120042902,
// dispatch://INFRA-4729's 20261120055717, 20261121065041, 20261121074614, 20261121170714,
// 20261123134347, 20261123134547, 20261123165139 and 20261123212127. It runs no statement, so it
// changes nothing on a database main migrated, and it touches no outbox_kind_check.
// dispatch://INFRA-4108's mark of a launch that had Oh My Pi resume a claim's session by its id
// (20261120042902) stands above every other version of this list, the design news among them, but
// dispatch://INFRA-4729's 20261120055717, 20261121065041, 20261121074614, 20261121170714,
// 20261123134347, 20261123134547, 20261123165139 and 20261123212127; it adds
// claims.agent_resume_generation at its default, 0, on every row it finds, and touches no
// outbox_kind_check, so it needs no row of its own here.
// dispatch://INFRA-4729's sequence of each value of the copy of Dispatch's issues (20261120055717)
// is applied after all of these but seven, the design news among them, and touches no
// outbox_kind_check: it adds columns to dispatch_issues at their defaults, an index on its parent,
// and a refill mark for each project the copy holds rows of.
// The check states the daemon keeps of Dispatch's check.state_changed (20261121065041) are applied
// after all of these but six, and touch no outbox_kind_check: the migration creates
// check_states, check_state_sets and check_state_serials, empty, and adds issues.check_states_seq
// at 0.
// The phase verdict's removal (20261121074614) is applied right after it, before 20261121170714,
// 20261123134347, 20261123134547, 20261123165139 and 20261123212127, and touches no
// outbox_kind_check: it gives phases.verdict, kept for the release before, the empty string for a
// default, adds phases.states_serial at 0, and takes the key verdict out of each queued notice.
// The applied sequence of the copy of Dispatch's issues (20261121170714) is applied after all of
// these but four and touches no outbox_kind_check: it adds dispatch_issues.applied_seq at its
// default, 0, on every row it finds.
// The step a tester's completion names for its task to return to (20261123134347) is applied
// after all of these but three, 20261123134547, 20261123165139 and 20261123212127, and touches no
// outbox_kind_check: it adds phases.return_to at its default, 0, on every row it finds.
// The fix beside the other steps of a task (20261123134547) is applied after all of these but
// two, 20261123165139 and 20261123212127, neither of which touches outbox_kind_check. It adds
// plan_tasks.fixes_ci and columns of step_starts and of pending_task_deliveries at their defaults,
// creates task_fixes and ci_failures, empty, and is the last word of outbox_kind_check: its list is
// the design news's with fix_job_read added, so it names every kind main's daemon writes that the
// upgrade keeps.
// dispatch://INFRA-5463's clear of the start failure of a claim whose agent is up
// (20261123212127) is applied after all of these and touches no outbox_kind_check: it empties
// claims.last_start_failure on every claim stored ready, working or idle, a column this list's
// 20261029121000 adds and the upgrade applies before it, so it needs no row of its own here
// (migration_20261123212127_test.go reads it).
var thisBranchsMigrations = []int{
	20261029120000, 20261029120100, 20261029120200, 20261029120300, 20261029120400,
	20261029120600, 20261029120700, 20261029120800, 20261029120900, 20261029121000,
	20261029121100, 20261029121200, 20261102003600, 20261105081500, 20261105183200,
	20261107095842, 20261108152000, 20261108190000, 20261109130413, 20261111045636, 20261111090000,
	20261111091500, 20261111181500,
	20261111214351, 20261111225552, 20261112070503, 20261112180548, 20261113082214, 20261113082415,
	20261113090512, 20261113110610, 20261113181500, 20261113182000, 20261114020537, 20261114031356,
	20261114061900, 20261115194357, 20261116144907, 20261116165926, 20261116234034, 20261117000528,
	20261117004117, 20261117023932, 20261117034837, 20261117064152, 20261117112334, 20261117200337,
	20261119003157, 20261119203047, 20261120012547, 20261120042902, 20261120055717, 20261121065041,
	20261121074614, 20261121170714, 20261123134347, 20261123134547, 20261123165139, 20261123212127,
}

// mainsNewestMigration is the newest version main carries, the cause a claim records for its
// failure. Every embedded version above it is named in thisBranchsMigrations: main's newest came in
// above dispatch://INFRA-714's own at the forward merge of 2026-11-12, and above it stand
// INFRA-3036's 20261111214351, the phase entry's 20261111225552, dispatch://INFRA-3027's
// 20261112070503, dispatch://INFRA-3219's 20261112180548, dispatch://INFRA-2981's 20261113082214
// and 20261113110610, dispatch://INFRA-3276's 20261113082415, dispatch://INFRA-3146's
// 20261113090512, dispatch://INFRA-3274's 20261114031356, dispatch://INFRA-2614's
// 20261113181500, 20261113182000, 20261114020537 and 20261114061900, dispatch://INFRA-3759's
// 20261115194357, t-e-54's 20261116144907, the role-holders publication's 20261116165926, the
// reviewer's reviewed commit, 20261116234034, the step records' three: the kind list with the
// step record (20261117000528), the count of starts of each step (20261117004117) and its roll
// forward (20261117064152), the kind list with the plan record (20261117023932), the plan of an
// issue's 20261117034837, dispatch://INFRA-1997's 20261117112334, dispatch://INFRA-4109's
// 20261117200337, dispatch://INFRA-4374's 20261119003157, dispatch://INFRA-175's 20261119203047,
// dispatch://INFRA-3178's 20261120012547, dispatch://INFRA-4108's 20261120042902,
// dispatch://INFRA-4729's 20261120055717, the check states' 20261121065041, the phase verdict's
// removal, 20261121074614, the copy's applied sequence, 20261121170714, the tester's return step,
// 20261123134347, the fix beside the other steps of a task, 20261123134547, the plan of an
// experiment's draw (20261123165139) and dispatch://INFRA-5463's 20261123212127. That is why the
// loop below holds the list rather than counting it, so that a version added above any of them is
// named here rather than read as main's.
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
