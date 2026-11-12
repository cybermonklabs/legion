package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// phaseEntryMigration is the version of the migration this upgrade runs
// (internal/store/migrations/20261111225552_phase_entry.up.sql), so the fixture below migrates
// through everything under it whatever lands beside it.
const phaseEntryMigration = 20261111225552

// upgradeProject is the Dispatch project of the tree the upgrade is read on, and upgradeRoot its
// root. Each shape is a member of that one tree, standing in testing, so the role every shape is
// read on is the tester (phase.RoleFor).
const (
	upgradeProject = "UPGRADE"
	upgradeRoot    = "UPGRADE-800"
)

// preEntryStore is a database carrying the schema of the release before the phase entry: every
// embedded migration under phaseEntryMigration, each applied as Store.Migrate applies one — the
// migration's SQL, the widening of schema_version for a timestamped version, and the version row
// — and that migration itself withheld, for the test's own Store.Migrate to run.
//
// It is built this way because the old shape is reachable no other way: the three assignment
// columns are created by the migrations under this one and dropped by this one, so a record as
// the release before it leaves it can only be written against the schema those migrations build.
// internal/testpg's Clone copies a template migrated to the head instead, where those columns are
// already gone.
func preEntryStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Database(t, "legion_phase_entry_upgrade"))
	if err != nil {
		t.Fatalf("open the database of the release before the phase entry: %v", err)
	}
	t.Cleanup(st.Close)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	widened := false
	for _, migration := range all {
		if migration.Version >= phaseEntryMigration {
			break
		}
		if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, migration.SQL); err != nil {
				return err
			}
			if migration.Version > math.MaxInt32 && !widened {
				if _, err := tx.Exec(ctx, "alter table schema_version alter column version type bigint"); err != nil {
					return err
				}
				widened = true
			}
			_, err := tx.Exec(ctx, "insert into schema_version (version) values ($1::bigint)", migration.Version)
			return err
		}); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
	}
	return st
}

// migrationsAtOrAbove is how many embedded migrations carry version or a version above it: what
// preEntryStore withheld, and so what the upgrade under test applies.
func migrationsAtOrAbove(t *testing.T, version int) int {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	withheld := 0
	for _, migration := range all {
		if migration.Version >= version {
			withheld++
		}
	}
	return withheld
}

// upgradeShape is one member of the tree as the release before the phase entry left it, and what
// the sweep after the migration must send its tester. assignment is the token that release minted
// onto the role's phases row; serving is what its claim had confirmed, which is that same token
// for a worker that took the work and another token for the worker that never was given it;
// delivery is the token a delivery in flight carries. owedTask is the task of the start row the
// issue still owes that role, empty where it owes none, and leftTask the task of a second start
// it owes under a token the phases row no longer names — the work of an entry the role has left,
// written before the other and so the older of the two — empty where it owes one start or none.
// owedDelivered has the claim record that owed row as one it has already queued a task from
// (supervise.Claim.DeliveredRows), which is what the release before this one left behind where it
// wrote a later transition's start over the row an issue owed in place: the id the claim knows,
// carrying a payload that never reached its worker.
// wantGiven and wantServed are the two numbers the migration's fill must leave on the claim,
// which is what decides everything below: a claim at or above its row's entry is holding that
// entry's work. wantTask is the task the sweep must send, empty where it must send nothing, and
// wantResume says the task is the line the daemon composes for a worker already in its phase
// (workflow.ResumePhaseTask), which the fixture can only build once the issue record stands.
type upgradeShape struct {
	key           string
	title         string
	assignment    string
	claimed       bool
	serving       string
	state         supervise.ClaimState
	suspendedBy   supervise.SuspendSource
	session       string
	delivery      string
	owedTask      string
	leftTask      string
	owedDelivered bool
	wantGiven     int64
	wantServed    int64
	wantResume    bool
	wantTask      string
}

// The upgrade to the phase entry, on the record the release before it leaves and nothing else: the
// three assignment columns filled, no entry column anywhere, and the migration this release adds
// still to run. What runs over it is the daemon's own entry sweep (intake.EntrySweep,
// workflowRuntime.tickEntrySweep) — no operator call, no completion, no Dispatch event and no
// write of any other kind — so what each shape is sent is the migration's fill read by
// workflow.OwedEntry and nothing else.
//
// The ten shapes are the states that record can be in at the moment the daemon stops, and each
// answers the one question this change exists to answer: is this role holding the work its phases
// row names?
//
//  1. A claim that confirmed its phase's assignment took that work: the fill reads the token on
//     both sides and gives it given_entry 1 and served_entry 1, so it is at its row's entry and
//     is sent nothing.
//  2. A claim holding a delivery of that assignment was given that work, whatever its worker has
//     confirmed: the fill gives it given_entry 1 from the delivery, so it is sent nothing, and the
//     delivery it holds is still worked besides (supervise.Delivery.StillWorked).
//  3. A claim whose worker confirmed an earlier token is the shape six trees stopped in on
//     2026-11-10: the phase was entered again, the row that carried the new task was written over
//     the row the claim had already taken a task from, and nothing reached the worker. The fill
//     matches neither statement, so the claim stands at given_entry 0 under a row at entry 1 and
//     the sweep sends that entry's work. Its completion is accepted besides, where the release
//     before this one refused it for an assignment it was never given.
//  4. A claim an operator suspended is sent nothing whatever entry it stands on: `legion claims
//     stop` stopped that worker, and only the operator's own resume starts it again
//     (supervise.Claim.RuleHolds, supervise.SuspendedByOperator).
//  5. A claim the idle release suspended (supervise.SuspendedByReleased) that was never given its
//     entry is behind like any other: no wake is armed for work it was never given, so the sweep
//     sends that entry's work, and the start it writes is the one the outbox resumes the released
//     pod with (supervise.Claim.RuleMayResume, the StateSuspended arm of the outbox's start).
//  6. A role the daemon holds no claim row of was given nothing at all, and is sent that entry's
//     work.
//  7. A role whose start row was still owed at the upgrade — a supervise start marked needsSlot,
//     the row a stopped tree waits on — is sent nothing, and the row it already owes is what
//     starts it: the fill stamps entry 1 into that row's own payload, so the sweep reads the
//     entry as already carried and writes no second start of the same work (workflow.OwedEntry).
//     That row then runs here, through the daemon's own outbox executor, and the delivery it
//     makes records entry 1 as given, after which a further sweep still sends nothing. Without
//     the stamp the row would deliver with no entry, the claim would stand at 0 under a row at
//     entry 1, and that sweep would hand the work a second time to a worker already doing it.
//     The three roles the sweep does send are sent the line the daemon composes for a worker
//     already in its phase (workflow.ResumePhaseTask), since the release before this one recorded
//     no task of the entry it was handed.
//  8. A claim that confirmed its phase's assignment and whose issue still owes a start carrying
//     that same token is the retry of a phase the role never left: the release before this one
//     wrote such a start — its own restart, and the slot repair that went through it
//     (ResumeSlottedWorker) — and decided it by its outbox row, having no entry to decide it by.
//     The fill stamps that row with entry 1 and the resend mark, so the claim it reaches, which
//     the fill put at given_entry 1, takes the row's own task instead of declining it as work it
//     was already given (supervise.Machine.queue). That row runs here as the seventh shape's
//     does, and what it hands the claim is read off the claim. The sweep sends this role
//     nothing: it holds the entry its phases row names.
//  9. A role whose issue owes two starts — an older one under a token the phases row no longer
//     names, which a lane held when the daemon stopped, and the one the current token wrote
//     beside it — takes its entry's work from the row of the current token. Both the task the
//     fill writes on the phases row and the number it stamps into a payload are matched on that
//     token, so the two read one row, where the task taken by age alone would be the work of an
//     entry the role has left under a row carrying the work it is on. The older row stands as it
//     stood, at no entry and with no mark.
//  10. A claim never given its entry whose issue owes the very row the claim has already queued a
//     task from is the third shape with that row still standing: the release before this one
//     wrote the new task over the row in place, keeping its id, so the claim recognises the row
//     and the task on it reached nobody. The fill stamps that row with entry 1 and no resend
//     mark, which is why the mark is stamped on the rows of a given claim alone: read by the
//     entry, 1 above the claim's 0, the row gives its task and the stopped tree moves; read by
//     its id, as a marked row is, it would be declined for the delivery of a task nobody ever
//     got.
//
// The sweep then runs again on its period and writes nothing more: the start each role holds
// carries the entry, so every later pass reads that entry as already carried (workflow.OwedEntry).
//
// The second half is the way back the migration's header names. Its eleven recovery statements
// run — the three that add the assignment columns back, the one that strips this release's own
// payload keys, the six that drop the entry columns and the one that deletes this migration's
// version row — and then the record is read as the release before this one reads it: its own
// claim read, its phases read and the pending delivery its claim read joins, as literal SQL taken
// from that release, each answering with the assignment columns at the empty string the
// recovery's default leaves on every row; and every supervise payload left in the outbox decoded
// as that release decodes one, strictly, against a copy of its own request shape. That is the
// whole of what a test without the old binary can show: it cannot show that daemon running, so
// what the recovery costs that daemon — a fence that compares nothing and mints a fresh token at
// each role's next start — is not asserted here.
//
// The last seven of those statements are the re-arm, and what they leave is read as the
// roll-forward reads it: none of the six entry columns standing, and no row of this migration's
// version in schema_version, every other version row where it was.
//
// The third half is that roll-forward. A role whose task the older daemon lost while it ran — a
// start finished without acting, which leaves no row behind and a claim that was never given the
// work — is written into the recovered record with that release's own SQL, Migrate runs a second
// time, and the daemon's ordinary sweep sends that role the work of the entry the second fill
// derives from the token that daemon minted (upgradeRollForwardSweepsTheLostTask). Zeroed instead
// of re-armed, as this way back ran before, that role would be swept nothing: the migration would
// not run again, its entry and its claim would both read 0, and a role level at zero is owed
// nothing — which is all workflow.OwedEntry answers and all admit's owedEntryRoles selects.
func TestTheUpgradeToThePhaseEntrySendsEachRoleBehindItTheWorkItsEntryNames(t *testing.T) {
	ctx := context.Background()
	st := preEntryStore(t)
	pool := st.Pool()
	log := quietLogger()
	records := record.NewStore()

	shapes := []upgradeShape{
		{key: "UPGRADE-801", title: "the claim that confirmed its assignment", assignment: "a-801",
			claimed: true, serving: "a-801", state: supervise.StateWorking, session: "ses-801",
			wantGiven: 1, wantServed: 1},
		{key: "UPGRADE-802", title: "the claim holding a delivery of it", assignment: "a-802",
			claimed: true, state: supervise.StateWorking, session: "ses-802", delivery: "a-802",
			wantGiven: 1},
		{key: "UPGRADE-803", title: "the claim never given it", assignment: "a-803-entered-again",
			claimed: true, serving: "a-803-handed-out", state: supervise.StateReady, session: "ses-803", wantResume: true},
		{key: "UPGRADE-804", title: "the claim an operator suspended", assignment: "a-804",
			claimed: true, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByOperator, session: "ses-804"},
		{key: "UPGRADE-805", title: "the claim an idle release suspended", assignment: "a-805",
			claimed: true, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByReleased, session: "ses-805", wantResume: true},
		{key: "UPGRADE-806", title: "the role with no claim row", assignment: "a-806", wantResume: true},
		{key: "UPGRADE-807", title: "the role whose start was still owed", assignment: "a-807",
			claimed: true, state: supervise.StateReady, session: "ses-807",
			owedTask: "Test the change. Issue: UPGRADE-807. Phase: testing. Handoff: test version 1."},
		{key: "UPGRADE-808", title: "the claim that confirmed it, owed the retry of its own phase", assignment: "a-808",
			claimed: true, serving: "a-808", state: supervise.StateReady, session: "ses-808",
			owedTask:  "Test the change again. Issue: UPGRADE-808. Phase: testing. Retry: the tester's claim failed.",
			wantGiven: 1, wantServed: 1},
		{key: "UPGRADE-809", title: "the role owed a start of an entry it has left", assignment: "a-809",
			claimed: true, state: supervise.StateReady, session: "ses-809",
			leftTask: "Test the change. Issue: UPGRADE-809. Phase: testing. Handoff: test version 1.",
			owedTask: "Test the change. Issue: UPGRADE-809. Phase: testing. Handoff: test version 2."},
		{key: "UPGRADE-810", title: "the claim never given it, owed the row it had queued a task from", assignment: "a-810-entered-again",
			claimed: true, serving: "a-810-handed-out", state: supervise.StateReady, session: "ses-810",
			owedTask:      "Test the change. Issue: UPGRADE-810. Phase: testing. Handoff: test version 2.",
			owedDelivered: true},
	}

	project, err := claim.ProjectToken(upgradeProject)
	if err != nil {
		t.Fatal(err)
	}
	root := record.Issue{Key: upgradeRoot, Tree: upgradeRoot, Project: upgradeProject, Title: "the root",
		Phase: phase.Accepting, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review",
		Rank: upgradeRoot, HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	issues := map[string]record.Issue{}
	tokens := map[string]claim.Token{}
	for _, shape := range shapes {
		parent := root.Key
		issues[shape.key] = record.Issue{Key: shape.key, Tree: root.Key, Parent: &parent, Project: upgradeProject,
			Title: shape.title, Phase: phase.Testing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: shape.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
		token, err := claim.NewToken(project, shape.key, claim.RoleTester)
		if err != nil {
			t.Fatal(err)
		}
		tokens[shape.key] = token
	}

	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		if err := openTreeTx(ctx, tx, upgradeProject, root.Key); err != nil {
			return err
		}
		for index, shape := range shapes {
			if err := records.PutIssue(ctx, tx, issues[shape.key]); err != nil {
				return err
			}
			if err := records.PutSlot(ctx, tx, record.Slot{Issue: shape.key, Index: index, AdmittedAt: time.Now().UTC()}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record the tree as the release before the phase entry left it: %v", err)
	}

	// The three assignment columns, written as that release wrote them. Every statement here is
	// literal SQL: this release's own writers name the entry columns the migration has still to
	// add, and a record written through them would not be the record the upgrade finds.
	owedRows, leftRows := map[string]int64{}, map[string]int64{}
	for _, shape := range shapes {
		if _, err := pool.Exec(ctx, `insert into phases (issue, role, claim, handoff_commit, rounds, verdict, assignment)
			values ($1, 'tester', $2, '', 0, '', $3)`, shape.key, string(tokens[shape.key]), shape.assignment); err != nil {
			t.Fatalf("record the phases row of %s: %v", shape.key, err)
		}
		if shape.claimed {
			if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
				state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, capability_hash,
				serving_generation, serving_assignment, suspended_by)
				values ($1, $2, $3, $4, 'tester', 1, $5, '', $6, 0, 0, 0, 0, $7, 1, $8, $9)`,
				string(tokens[shape.key]), project, root.Key, shape.key, shape.session, string(shape.state),
				[]byte("capability-"+shape.key), shape.serving, string(shape.suspendedBy)); err != nil {
				t.Fatalf("record the claim of %s: %v", shape.key, err)
			}
		}
		if shape.delivery != "" {
			if _, err := pool.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, phase,
				generation, queued_at, marked_by, interrupted, assignment)
				values ($1, $2, $3, 'testing', 1, now(), '', false, $4)`,
				string(tokens[shape.key]), "outbox:"+shape.key, "Test the change. Issue: "+shape.key+". Phase: testing.",
				shape.delivery); err != nil {
				t.Fatalf("record the delivery in flight on %s: %v", shape.key, err)
			}
		}
		// The start rows the issue still owes that role, with the payload that release wrote: the
		// assignment on the row beside the task, which is the shape the migration's fill reads the
		// entry's work out of. A start of an entry the role has left is written first, so it is the
		// older of the two and the one a fill reading by age alone would take the task from.
		owes := func(task, assignment string) int64 {
			t.Helper()
			payload, err := json.Marshal(map[string]any{"op": "start", "tree": root.Key, "role": "tester",
				"phase": "testing", "generation": 1, "assignment": assignment, "needsSlot": true, "task": task})
			if err != nil {
				t.Fatal(err)
			}
			var row int64
			if err := pool.QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
				values ('supervise', $1, $2, 0, now(), '') returning id`, shape.key, string(payload)).Scan(&row); err != nil {
				t.Fatalf("record a start %s still owes its tester: %v", shape.key, err)
			}
			return row
		}
		if shape.leftTask != "" {
			leftRows[shape.key] = owes(shape.leftTask, shape.assignment+"-left")
		}
		if shape.owedTask != "" {
			owedRows[shape.key] = owes(shape.owedTask, shape.assignment)
		}
		if shape.owedDelivered {
			// The row the claim has already queued a task from: that release wrote this start
			// over the one the issue owed in place, so the id the claim recorded is the id the
			// new task now stands under and the claim would recognise it
			// (supervise.Claim.DeliveredRows).
			if _, err := pool.Exec(ctx, "update claims set delivered_rows = array[$1::bigint] where token = $2",
				owedRows[shape.key], string(tokens[shape.key])); err != nil {
				t.Fatalf("record the row %s's tester already queued a task from: %v", shape.key, err)
			}
		}
	}

	// The record is the old one: the assignment stands on every phases row, and the entry the
	// daemon of this release writes has nowhere to go.
	var assigned int
	if err := pool.QueryRow(ctx, "select count(*) from phases where assignment <> ''").Scan(&assigned); err != nil {
		t.Fatalf("read the phases rows the release before this one left: %v", err)
	}
	if assigned != len(shapes) {
		t.Fatalf("phases rows carrying an assignment = %d, want %d: the record this upgrade is read on is the old one", assigned, len(shapes))
	}
	if _, err := pool.Exec(ctx, "update phases set entry = 1 where issue = $1", shapes[0].key); err == nil {
		t.Fatal("raising a phases row's entry before the migration was taken, want the column to be unknown: the fixture is not the old shape")
	}

	applied, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("run the upgrade: %v", err)
	}
	// The fixture applied every migration under the phase entry itself, so what this run applies
	// is the phase entry and whatever has landed above it since. The number is counted from the
	// embedded set rather than written here, so a migration added later is not read as the
	// fixture having withheld one of its own.
	if want := migrationsAtOrAbove(t, phaseEntryMigration); applied != want {
		t.Fatalf("the upgrade applied %d migrations, want the %d at or above the phase entry: everything under it is the fixture's own", applied, want)
	}

	// What the fill left, which is what every send below is decided on: a role that release had
	// handed work is at entry 1, the work of that entry is the task of the start row the issue
	// still owes it and nothing where it owes none, and each claim carries what it was given and
	// what its worker took.
	for _, shape := range shapes {
		var entry int64
		var entryTask string
		if err := pool.QueryRow(ctx, "select entry, entry_task from phases where issue = $1 and role = 'tester'",
			shape.key).Scan(&entry, &entryTask); err != nil {
			t.Fatalf("read the phases row of %s after the upgrade: %v", shape.key, err)
		}
		if entry != 1 || entryTask != shape.owedTask {
			t.Fatalf("the phases row of %s (%s) = entry %d carrying %q, want entry 1 carrying %q: a role that release handed work is at entry 1, with the task of the start it still owes",
				shape.key, shape.title, entry, entryTask, shape.owedTask)
		}
		// The start row the issue still owes that role is the start of entry 1, and carries that
		// number: the delivery it makes is what records the entry as given to the claim, and one
		// carrying no entry would leave the claim at 0 under this row at entry 1. It carries the
		// resend mark where the fill counts its claim as given that entry, and only there: marked,
		// the row is decided by its own id, so the claim takes the task instead of declining it as
		// work it already has (supervise.Machine.queue); unmarked, a row whose claim is behind is
		// decided by the entry, which passes the task on its own.
		if owed, owes := owedRows[shape.key]; owes {
			carried, resend := stampOf(t, pool, owed)
			if carried != 1 || resend != (shape.wantGiven > 0) {
				t.Fatalf("the start row %d %s (%s) still owes its tester carries entry %d marked a resend %v, want entry 1 marked %v: the row the issue owes is the start of entry 1, and the mark is what has a claim already at that entry take its task",
					owed, shape.key, shape.title, carried, resend, shape.wantGiven > 0)
			}
		}
		// A start of an entry the role has left stands as it stood: every statement of the fill
		// matches the token the phases row names, and this row carries another.
		if left, has := leftRows[shape.key]; has {
			carried, resend := stampOf(t, pool, left)
			if carried != 0 || resend {
				t.Fatalf("the start row %d %s (%s) owes under a token it has left carries entry %d marked a resend %v, want no entry and no mark: the fill reads the token its phases row names",
					left, shape.key, shape.title, carried, resend)
			}
		}
		if !shape.claimed {
			continue
		}
		var given, served int64
		if err := pool.QueryRow(ctx, "select given_entry, served_entry from claims where token = $1",
			string(tokens[shape.key])).Scan(&given, &served); err != nil {
			t.Fatalf("read the claim of %s after the upgrade: %v", shape.key, err)
		}
		if given != shape.wantGiven || served != shape.wantServed {
			t.Fatalf("the claim of %s (%s) was given entry %d and served entry %d, want %d and %d",
				shape.key, shape.title, given, served, shape.wantGiven, shape.wantServed)
		}
	}

	// The daemon of this release, running nothing but its entry sweep over that record.
	engine := workflow.New(records, workflow.Config{Project: upgradeProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(records, engine, len(shapes), upgradeProject, log)
	w := &workflowRuntime{pool: pool, records: record.NewStore(), admission: admission,
		handlers: []intake.Handler{engine, admission}, dispatchProject: upgradeProject, bootID: "test-boot",
		log: log, entrySweep: 10 * time.Millisecond}
	swept := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select count(*) from processed_events
			where event_id like 'entry-sweep:UPGRADE:test-boot:%'`).Scan(&n); err != nil {
			t.Fatalf("read the sweeps applied: %v", err)
		}
		return n
	}

	// The sweep's own fact is what this waits on rather than the rows it writes: the sweep that
	// applied it wrote in the transaction that recorded it, so what the record holds afterwards
	// is every row that sweep means to write. A shape the sweep sent nothing then fails below
	// under its own name. Three more sweeps are driven by hand with the ticker stopped, because
	// every role that pass sent work now carries that entry and the gate applies no further fact
	// on its own period: they are what reads a later sweep as adding nothing.
	sweeping, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.tickEntrySweep(sweeping); close(done) }()
	testwait.Eventually(t, "the entry sweep's fact applied", func() bool { return swept() >= 1 })
	stop()
	<-done
	sweeps := swept()
	for i := range 3 {
		w.applyEntrySweep(ctx, time.Now().Add(time.Duration(i+1)*time.Second))
	}

	for i, shape := range shapes {
		if shape.wantResume {
			shapes[i].wantTask = workflow.ResumePhaseTask(issues[shape.key])
			continue
		}
		shapes[i].wantTask = shape.owedTask
	}
	for _, shape := range shapes {
		starts := sweptStarts(t, pool, shape.key)
		if left, has := leftRows[shape.key]; has {
			// The start of an entry the role has left is neither the sweep's work nor the role's:
			// it took no entry above, and what the role is sent is read off the rows that carry
			// one.
			starts = slices.DeleteFunc(starts, func(start sweptStart) bool { return start.id == left })
		}
		if shape.wantTask == "" {
			if len(starts) != 0 {
				t.Fatalf("the sweep sent %s (%s) %+v, want nothing: the claim given entry %d under a row at entry 1 is owed no second task",
					shape.key, shape.title, starts, shape.wantGiven)
			}
			continue
		}
		if len(starts) != 1 {
			t.Fatalf("the sweep sent %s (%s) %+v, want the one start carrying the work of entry 1", shape.key, shape.title, starts)
		}
		want := sweptStart{id: starts[0].id, role: claim.RoleTester, task: shape.wantTask, entry: 1}
		if starts[0] != want {
			t.Fatalf("the start the sweep sent %s (%s) = %+v, want %+v: the upgrade put that role at entry 1 with that work",
				shape.key, shape.title, starts[0], want)
		}
		// A shape whose issue already owed a start is started by that very row, and the sweep adds
		// none: the fill stamped the row with entry 1, so every pass reads the entry as carried
		// (workflow.OwedEntry). A sweep that did write one would have written over the owed row
		// (record.Store.EnqueueStart), leaving the same single row under a new id, which is why
		// the row is named here and not merely counted.
		if owed, owes := owedRows[shape.key]; owes && starts[0].id != owed {
			t.Fatalf("the start %s holds is row %d, not the row %d its issue already owed: the row owed is the start of entry 1, so the sweep writes no second one",
				shape.key, starts[0].id, owed)
		}
	}
	if n := swept(); n != sweeps {
		t.Fatalf("sweeps applied = %d, want the %d the ticker applied: a later sweep must be read as writing nothing more, and one owed nothing takes no fact", n, sweeps)
	}

	// The two suspensions the sweep told apart, read back as the rules that decide them read them:
	// the operator's holds every start the daemon queues, and the idle release's is resumed by the
	// start the sweep wrote (the StateSuspended arm of the outbox's start case).
	held := claimsByToken(t, st)
	if c := held[tokens["UPGRADE-804"]]; !c.RuleHolds() {
		t.Fatalf("the claim of UPGRADE-804 = state %q suspended by %q, want the operator's suspension: it is why the sweep sends it nothing",
			c.State, c.SuspendedBy)
	}
	if c := held[tokens["UPGRADE-805"]]; !c.Released() || !c.RuleMayResume() {
		t.Fatalf("the claim of UPGRADE-805 = state %q suspended by %q, want the idle release's suspension: the start the sweep wrote resumes its pod",
			c.State, c.SuspendedBy)
	}

	// The start UPGRADE-807 still owed runs, as the outbox runs it, and the entry its payload
	// carries becomes the entry its claim was given. The sweeps that follow are driven by hand
	// with the ticker stopped, as the ones above are, and they send that role nothing, where a
	// delivery made with no entry would have left the claim at 0 under a row at entry 1 and had
	// them hand the same work to a worker already doing it. What those passes do for the roles
	// still behind is the block above's to say; what is read here is the one role whose start ran.
	upgradeOwedStartGivesItsEntry(t, st, issues["UPGRADE-807"], held[tokens["UPGRADE-807"]], owedRows["UPGRADE-807"], false)
	for i := range 2 {
		w.applyEntrySweep(ctx, time.Now().Add(time.Duration(i+5)*time.Second))
	}
	if starts := sweptStarts(t, pool, "UPGRADE-807"); len(starts) != 1 || starts[0].id != owedRows["UPGRADE-807"] {
		t.Fatalf("the start rows of UPGRADE-807 once the row it owed had run = %+v, want that one row %d: its claim was given that entry, so the sweep sends no second task",
			starts, owedRows["UPGRADE-807"])
	}

	// The retry UPGRADE-808 still owed runs the same way, on a claim the fill put at the very
	// entry that row carries. The mark the fill stamped beside the number is what has that claim
	// take the row's own task: read by the entry alone the task is work the claim was already
	// given, so the row would queue nothing, the worker would be relaunched with no prompt, and
	// the issue would hold its slot with nobody at work on it and nothing saying so.
	upgradeOwedStartGivesItsEntry(t, st, issues["UPGRADE-808"], held[tokens["UPGRADE-808"]], owedRows["UPGRADE-808"], true)

	// The row UPGRADE-810 owes runs on the claim that never got its task, and gives it: the row
	// carries entry 1 and no mark, so it is decided by the entry the claim stands below and not
	// by its own id, which that claim already holds among the rows it has queued a task from.
	// This is what keeps the mark to the rows of a claim the fill counts as given: stamped here
	// too, the row would be declined for the delivery of a task nobody ever got, and the tree
	// would stand stopped through its upgrade.
	upgradeOwedStartGivesItsEntry(t, st, issues["UPGRADE-810"], held[tokens["UPGRADE-810"]], owedRows["UPGRADE-810"], false)

	upgradeCompletionAccepted(t, st, engine, issues["UPGRADE-803"], held[tokens["UPGRADE-803"]])
	upgradeRecoveryReadsAnswer(t, pool, tokens["UPGRADE-802"], "UPGRADE-802")
	upgradeRollForwardSweepsTheLostTask(t, st, w, root)
}

// claimsByToken is every claim the store holds, read with this release's own claim read
// (store.Store.Claims, claimSelect).
func claimsByToken(t *testing.T, st *store.Store) map[claim.Token]supervise.Claim {
	t.Helper()
	claims, err := st.Claims(context.Background())
	if err != nil {
		t.Fatalf("read the claims the upgrade left: %v", err)
	}
	held := make(map[claim.Token]supervise.Claim, len(claims))
	for _, c := range claims {
		held[c.Token] = c
	}
	return held
}

// stampOf is what the fill stamped into a supervise start's payload: the entry the row carries
// and whether it is marked a resend of that entry (record.SuperviseRequest.Entry and
// ResendEntry), both read off the row as it stands.
func stampOf(t *testing.T, pool *pgxpool.Pool, row int64) (int64, bool) {
	t.Helper()
	var entry int64
	var resend bool
	if err := pool.QueryRow(context.Background(), `select coalesce((payload->>'entry')::bigint, 0),
		coalesce((payload->>'resendEntry')::boolean, false) from outbox where id = $1`, row).Scan(&entry, &resend); err != nil {
		t.Fatalf("read the start row %d after the upgrade: %v", row, err)
	}
	return entry, resend
}

// upgradeOwedStartGivesItsEntry runs the start row the issue still owed at the upgrade, on the
// claim the upgrade left, and reads the entry the record then holds that claim as given.
//
// The row runs through the daemon's own executor (the outbox's supervise case), so what carries
// the entry from the row's payload to the claim is the daemon's own path and not the test's: the
// start delivers its task with the entry the payload names, and the write that stores the
// delivery stores the given entry with it (supervise's queue, store's putClaimAndDelivery). The
// supervisor is built on the store under test rather than the outbox tests' fake one, since the
// number this reads back is the one the sweep above weighs each role against.
//
// resend is whether the row must carry the mark that has the claim take its task though it was
// given that entry (record.SuperviseRequest.ResendEntry): the fill stamps it on a row whose claim
// it has counted as holding the entry, where the entry test alone would queue nothing and the
// start would relaunch the worker with no prompt. A row whose claim is behind the entry carries
// no mark and is decided by the entry.
//
// The claim is ready with a session, so the row spawns nothing and resumes nothing: it has only
// its task to give (the runtime and connections are fakes, as every start case in this package
// uses).
func upgradeOwedStartGivesItsEntry(t *testing.T, st *store.Store, issue record.Issue, held supervise.Claim, owed int64, resend bool) {
	t.Helper()
	ctx := context.Background()
	pool := st.Pool()
	var payload []byte
	if err := pool.QueryRow(ctx, "select payload from outbox where id = $1", owed).Scan(&payload); err != nil {
		t.Fatalf("read the start row %d %s still owes its tester: %v", owed, issue.Key, err)
	}
	row := record.OutboxRow{ID: owed, Kind: record.OutboxKindSupervise, Issue: issue.Key,
		Payload: json.RawMessage(payload), NextAt: time.Now()}
	decoded, err := record.DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("decode the start row %d %s still owes its tester: %v", owed, issue.Key, err)
	}
	request, ok := decoded.(record.SuperviseRequest)
	if !ok || request.Entry != 1 || request.ResendEntry != resend {
		t.Fatalf("the start row %d %s still owes its tester = %+v, want a supervise start of entry 1 marked a resend %v: the fill stamps the entry the row's delivery records, and the mark where its claim already holds that entry",
			owed, issue.Key, decoded, resend)
	}

	project, err := claim.ProjectToken(upgradeProject)
	if err != nil {
		t.Fatal(err)
	}
	sup := newSupervisor(ctx, st, project, t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: fake.NewRuntime(), Conns: fake.NewConns(), Store: st, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: 24,
	}
	t.Cleanup(sup.stop)
	machine, _, err := sup.Create(ctx, held, "")
	if err != nil {
		t.Fatalf("supervise the claim the upgrade left on %s: %v", issue.Key, err)
	}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: upgradeProject, records: record.NewStore(),
		supervisor: sup, trees: st, tokens: outboxTokens{}, project: project, stateDir: t.TempDir(),
		now: time.Now}
	if err := runner.execute(ctx, row); err != nil {
		t.Fatalf("run the start row %d %s still owed its tester: %v", owed, issue.Key, err)
	}
	machine.Wait()

	if pending := machine.Claim().Pending; pending == nil || pending.Task != request.Task || pending.Entry != 1 {
		t.Fatalf("the delivery the owed start made = %+v, want its own task at entry 1: the row carries the entry to the claim", pending)
	}
	var given int64
	if err := pool.QueryRow(ctx, "select given_entry from claims where token = $1", string(held.Token)).Scan(&given); err != nil {
		t.Fatalf("read the entry the record holds %s's tester as given: %v", issue.Key, err)
	}
	if given != 1 {
		t.Fatalf("the claim of %s was given entry %d once the start it owed ran, want 1: a claim at its role's entry is holding that entry's work, and the sweep sends it nothing",
			issue.Key, given)
	}
}

// upgradeCompletionAccepted posts the completion of the third shape's turn through the daemon's own
// route, on the claim exactly as the upgrade left it, and reads the answer.
//
// The release before this one refused that completion 409 HANDOFF_STALE_ASSIGNMENT: its fence
// compared the token on the role's phases row with the one the claim served, both were set, and
// they differed, because the task that minted the row's token never reached the worker. The
// upgrade leaves no such pair. The claim serves entry 0 — the fill wrote it no served entry, and
// nothing has delivered the work the sweep just sent — and the entry fence compares only when both
// sides name one (api/handoff.go), so the turn in flight completes instead of being refused for
// work the role was never given.
//
// The supervisor here is the outbox tests' own (newOutboxSupervisor): its store is a fake, so the
// machine the route reads holds the claim the upgrade left and no write of this call goes back
// into the record under test.
func upgradeCompletionAccepted(t *testing.T, st *store.Store, engine *workflow.Engine, issue record.Issue, c supervise.Claim) {
	t.Helper()
	ctx := context.Background()
	if c.ServingEntry() != 0 {
		t.Fatalf("the claim of %s serves entry %d after the upgrade, want 0: the fill gives a claim that was never given its entry none",
			issue.Key, c.ServingEntry())
	}
	project, err := claim.ProjectToken(upgradeProject)
	if err != nil {
		t.Fatal(err)
	}
	sup, _ := newOutboxSupervisor(t, project, t.TempDir())
	if _, _, err := sup.Create(ctx, c, ""); err != nil {
		t.Fatalf("supervise the claim the upgrade left on %s: %v", issue.Key, err)
	}
	grants := credential.New(nil)
	grant, err := grants.Mint(c)
	if err != nil {
		t.Fatalf("mint the grant %s's tester completes with: %v", issue.Key, err)
	}
	handoffs := &fleetHandoffs{handoffs: map[string]dispatch.Handoff{}}
	handler := api.NewServer("127.0.0.1", 0, api.Options{
		Steps: config.DesignGateRootIssues.Steps(), Supervisor: sup, Project: project, DispatchProject: upgradeProject,
		Pool: st.Pool(), Handlers: []intake.Handler{engine}, Record: record.NewStore(), Grants: grants,
		Dispatch: handoffs, Log: quietLogger(),
	}).Handler
	body, err := json.Marshal(api.HandoffCompleteRequest{GrantID: grant.ID, Summary: "the tests pass", Verdict: "pass",
		HandoffID: handoffs.write(issue.Key, "test", c.Session), HandoffVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/legion/v1/handoff/complete", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("the completion of %s's testing turn = %d %s, want 200: the upgrade left no assignment for it to be refused against",
			issue.Key, recorder.Code, strings.TrimSpace(recorder.Body.String()))
	}
}

// upgradeRecoveryReadsAnswer is the way back the migration's header names, as far as a test
// without the old binary can take it: the seven recovery statements run with the daemon stopped,
// and then the record is read as the release before this one reads it, and as the roll-forward
// after that rollback reads it.
//
// The three reads are that release's own, as literal SQL at commit e90c078559: its claim read
// (internal/store/claims.go, claimSelect), which joins the pending delivery and is the one read
// that names both dropped columns of the claim side; its phases read (internal/record/store.go,
// Postgres.Phases); and the delivery read its retirement makes (RetireDelivery). Each must answer
// a row, and the columns the recovery brought back must read as the empty string their default
// leaves, which is the whole reason the header can say the older daemon's fence compares nothing
// and refuses none of the completions in flight.
//
// The fourth statement is the strip of this release's own payload keys, and what it leaves is
// read the same way: every supervise payload still in the outbox is decoded as that release
// decodes one — strictly, with DisallowUnknownFields, which is the decoder
// record.DecodeOutboxPayload builds — against preEntrySuperviseRequest, that release's own
// request shape at the same commit. A key it does not know refuses the row, and its
// record.Postgres.RoleRuns decodes every supervise row of the issues an admission pass reads, so
// one such row would retry for ever and fail every pass of its issue besides.
//
// The last seven statements are the re-arm: the six entry columns dropped and this migration's
// version row deleted. Neither is owed to the older daemon — it reads none of those columns, and
// its own embedded set holds no migration of this version — and both are owed to the
// roll-forward, which has to run this file again. What this reads back is what makes that run
// possible: none of the six columns standing on the three tables the migration adds them to, and
// no row of 20261111225552 in schema_version with every other version row left where it was,
// since store.apply decides each migration on its own row. Put back to zero instead, as this way
// back ran before, the migration would never run again, and a role whose task that daemon lost
// inside the rollback window would read 0 against 0: level, owed nothing and swept nothing. What
// the re-armed migration makes of the token that daemon kept current is the next helper's
// (upgradeRollForwardSweepsTheLostTask).
//
// What this cannot show is that daemon running: no binary of it is here to boot against the
// recovered schema, so that it then mints a fresh token at each role's next start is not
// asserted, and neither is the decode this copies — what the copy shows is that the payloads the
// strip leaves carry no key outside that release's shape.
func upgradeRecoveryReadsAnswer(t *testing.T, pool *pgxpool.Pool, token claim.Token, issue string) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		"alter table phases add column assignment text not null default '';",
		"alter table claims add column serving_assignment text not null default '';",
		"alter table pending_task_deliveries add column assignment text not null default '';",
		`update outbox set payload = payload - 'entry' - 'resendEntry'
			where kind = 'supervise' and (payload ? 'entry' or payload ? 'resendEntry');`,
		"alter table phases drop column entry;",
		"alter table phases drop column entry_task;",
		"alter table phases drop column entry_raised_at;",
		"alter table claims drop column given_entry;",
		"alter table claims drop column served_entry;",
		"alter table pending_task_deliveries drop column entry;",
		"delete from schema_version where version = 20261111225552;",
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("run the recovery statement %q: %v", statement, err)
		}
	}

	const claimSelect = `select c.token, c.project, c.tree, c.issue, c.role, c.generation, c.tree_epoch, c.session,
	c.session_file, c.locator, c.state, c.failure, c.launch_failures, c.deaths, c.prompt_failures, c.prompt_retires,
	c.boot_token_hash, c.capability_hash, c.uncertain_streak, c.last_start_row, c.serving_generation,
	c.serving_assignment, c.serving_check_look, c.serving_check_opened_at,
	c.agent_secrets_enrollment, c.agent_secrets_enrollment_incarnation, c.suspended_by,
	c.plugin_contract, c.plugin_relaunch_for, c.delivered_rows,
	c.update_round, c.update_reasons, c.update_opened_at, c.update_tasked_at, c.update_signalled_at,
	c.update_head, c.update_told_at, c.update_answered_by_turn_end, c.update_reasked_for_push_check,
	c.update_left_round, c.provider_error_run, c.death_charged,
	c.step_at, c.turn_started_at, c.step_tool, c.step_call_id, c.step_args_bytes,
	c.step_args_hash, c.step_started_at, c.step_ended_at,
	d.delivery_id, d.task, d.phase, d.generation, d.assignment, d.check_look, d.check_opened_at,
	d.queued_at, d.delivered_at, d.marked_by, d.confirmed_at, d.interrupted, d.out_of_memory,
	d.send_failures, d.last_send_failure_at, d.last_send_error,
	d.kept_decisions, d.provider_error_retries, d.provider_error_note
	from claims c left join pending_task_deliveries d on d.claim_token = c.token`
	const phasesSelect = `select issue, role, claim, handoff_commit, handoff_record, handoff_version, rounds, conflict_rounds, backward_moves, cap_told, verdict, summary, last_handoff, decision, completed_at, merge_note, assignment from phases
		where issue = $1 order by role`
	const deliverySelect = "select delivery_id from pending_task_deliveries where claim_token = $1"

	for _, read := range []struct {
		what  string
		query string
		arg   any
		// empty names the columns the recovery brought back, which every row of the recovered
		// schema carries at the default's empty string.
		empty []string
	}{
		{what: "the claim read", query: claimSelect + " where c.token = $1", arg: string(token),
			empty: []string{"serving_assignment", "assignment"}},
		{what: "the phases read", query: phasesSelect, arg: issue, empty: []string{"assignment"}},
		{what: "the pending delivery read", query: deliverySelect, arg: string(token)},
	} {
		rows, err := pool.Query(ctx, read.query, read.arg)
		if err != nil {
			t.Fatalf("%s of the release before this one, after the recovery: %v", read.what, err)
		}
		// The field names are taken before the rows are read: pgx hands them out on the open
		// Rows, and the collect below closes it.
		fields := append([]pgconn.FieldDescription(nil), rows.FieldDescriptions()...)
		values, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([]any, error) { return row.Values() })
		if err != nil {
			t.Fatalf("%s of the release before this one, after the recovery: %v", read.what, err)
		}
		if len(values) != 1 {
			t.Fatalf("%s of the release before this one answered %d rows, want 1: the recovery is what lets that daemon read this record at all",
				read.what, len(values))
		}
		for _, name := range read.empty {
			held, ok := columnOf(fields, values[0], name)
			if !ok {
				t.Fatalf("%s names no column %s: the recovery statements are what bring it back", read.what, name)
			}
			if held != "" {
				t.Fatalf("%s reads %s = %q, want the empty string: the recovery adds the column with that default on every row, which is why the older daemon's fence compares nothing",
					read.what, name, held)
			}
		}
	}

	// Every supervise payload the strip left, decoded as the release before this one decodes one.
	// A row it refuses is a row that retries for ever on that release, and an admission pass of
	// its issue that fails with it, so each must decode and the outbox must hold rows to decode:
	// the starts the sweep wrote and the start UPGRADE-807 owed, which ran but was never finished.
	rows, err := pool.Query(ctx, "select id, payload from outbox where kind = 'supervise' order by id")
	if err != nil {
		t.Fatalf("read the supervise rows the recovery left: %v", err)
	}
	payloads, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct {
		id      int64
		payload []byte
	}, error) {
		var held struct {
			id      int64
			payload []byte
		}
		err := row.Scan(&held.id, &held.payload)
		return held, err
	})
	if err != nil {
		t.Fatalf("read the supervise rows the recovery left: %v", err)
	}
	if len(payloads) == 0 {
		t.Fatal("the record holds no supervise row after the recovery, want the starts of the upgrade's own shapes: a decode of nothing shows nothing")
	}
	for _, held := range payloads {
		decoder := json.NewDecoder(bytes.NewReader(held.payload))
		decoder.DisallowUnknownFields()
		var request preEntrySuperviseRequest
		if err := decoder.Decode(&request); err != nil {
			t.Fatalf("the release before this one decodes outbox row %d, %s, as %v; want it decoded: a payload it refuses is a row that retries for ever, and every admission pass of its issue fails with it (record.Postgres.RoleRuns)",
				held.id, held.payload, err)
		}
	}

	// What the re-arm leaves, which is the whole of what lets the roll-forward run this file
	// again: none of the six columns the migration's first statements add, since `add column` on a
	// column that already stands fails, and no version row of its own.
	for _, gone := range []struct{ table, column string }{
		{table: "phases", column: "entry"},
		{table: "phases", column: "entry_task"},
		{table: "phases", column: "entry_raised_at"},
		{table: "claims", column: "given_entry"},
		{table: "claims", column: "served_entry"},
		{table: "pending_task_deliveries", column: "entry"},
	} {
		var standing bool
		if err := pool.QueryRow(ctx, `select exists (select 1 from information_schema.columns
			where table_schema = current_schema() and table_name = $1 and column_name = $2)`,
			gone.table, gone.column).Scan(&standing); err != nil {
			t.Fatalf("read whether %s.%s stands after the recovery: %v", gone.table, gone.column, err)
		}
		if standing {
			t.Fatalf("the recovery left %s.%s standing, want it dropped: the roll-forward runs this migration again from its first statement, which adds that column",
				gone.table, gone.column)
		}
	}
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	var armed, recorded int
	if err := pool.QueryRow(ctx, "select count(*) from schema_version where version = $1::bigint", phaseEntryMigration).Scan(&armed); err != nil {
		t.Fatalf("read whether the phase entry migration is still recorded: %v", err)
	}
	if armed != 0 {
		t.Fatalf("schema_version holds %d rows of the phase entry migration after the recovery, want none: its row is what store.apply reads to decide the migration is taken, and the roll-forward has to run it again", armed)
	}
	if err := pool.QueryRow(ctx, "select count(*) from schema_version").Scan(&recorded); err != nil {
		t.Fatalf("count the versions recorded after the recovery: %v", err)
	}
	if recorded != len(all)-1 {
		t.Fatalf("schema_version holds %d rows after the recovery, want the %d every other migration recorded: the delete names one version, and store.apply decides each migration on its own row",
			recorded, len(all)-1)
	}
}

// upgradeRollForwardSweepsTheLostTask is the roll-forward that follows the way back: the newer
// image comes back, Migrate runs this migration a second time because the way back deleted its
// version row, and the daemon's ordinary sweep runs over what the second fill left.
//
// The role it is read on is one whose task the older daemon lost while it ran, which is the
// window the re-arm exists for. The shape written here is a start finished without acting: that
// daemon handed the role its phase's work and minted the token onto the phases row, and the row
// that carried the task ended without delivering it, leaving no start row behind and a claim that
// was never given the work. That is the one of the two shapes this test can write faithfully —
// the other, a later transition's start written over the row an issue already owes, leaves a row
// the fill stamps, and the stamped row is what starts the role, so the sweep is not what sends
// it (the first half's seventh and tenth shapes read that path). It is written in that release's
// own SQL, a phases row carrying an assignment and a claim serving none, because this release's
// writers name the entry columns the way back has dropped.
//
// What the second run reads is the record that daemon kept current: the token on the phases row.
// The fill numbers the role entry 1 and finds no start row to take a task from, so the record
// holds no task of that entry and the sweep sends the line the daemon composes for a worker
// already in its phase (workflow.ResumePhaseTask, through Engine.SendOwedEntry), at entry 1.
//
// The control is this same half with the version row left standing: Migrate then applies nothing,
// the role's entry and its claim both read 0, and the sweep sends it nothing at all, because a
// role level at zero is owed nothing (workflow.OwedEntry) and admit's owedEntryRoles selects by
// that same answer. That is why the way back deletes the row rather than zeroing the columns.
//
// The ten shapes of the first half are left level by the same run: the recovery brought their
// assignment columns back empty on every row, so every statement of the second fill passes over
// them and the sweep writes no start for any of them, which is what the count of every other
// issue's start rows reads.
func upgradeRollForwardSweepsTheLostTask(t *testing.T, st *store.Store, w *workflowRuntime, root record.Issue) {
	t.Helper()
	ctx := context.Background()
	pool := st.Pool()
	const key = "UPGRADE-811"
	project, err := claim.ProjectToken(upgradeProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, key, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	parent := root.Key
	issue := record.Issue{Key: key, Tree: root.Key, Parent: &parent, Project: upgradeProject,
		Title: "the role whose start finished without acting while the older image ran", Phase: phase.Testing,
		Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: key,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	records := record.NewStore()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: 10, AdmittedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("record the issue the older daemon admitted after the rollback: %v", err)
	}
	// The phases row and the claim as that release writes them: the token it minted at the
	// transition stands on the row, and the claim serves none, because the start that would have
	// given it the work finished without acting. No outbox row stands beside them for the same
	// reason, so the second fill finds the role a task nowhere.
	if _, err := pool.Exec(ctx, `insert into phases (issue, role, claim, handoff_commit, rounds, verdict, assignment)
		values ($1, 'tester', $2, '', 0, '', $3)`, key, string(token), "a-811-minted-after-the-rollback"); err != nil {
		t.Fatalf("record the phases row the older daemon left on %s: %v", key, err)
	}
	if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, capability_hash,
		serving_generation, serving_assignment, suspended_by)
		values ($1, $2, $3, $4, 'tester', 1, 'ses-811', '', $5, 0, 0, 0, 0, $6, 1, '', '')`,
		string(token), project, root.Key, key, string(supervise.StateReady), []byte("capability-"+key)); err != nil {
		t.Fatalf("record the claim the older daemon left on %s: %v", key, err)
	}

	applied, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("roll forward onto the record the older daemon left: %v", err)
	}
	if applied != 1 {
		t.Fatalf("the roll-forward applied %d migrations, want the phase entry migration run a second time: the way back deletes its version row, and store.apply decides each migration on its own row",
			applied)
	}
	var entry int64
	var entryTask string
	if err := pool.QueryRow(ctx, "select entry, entry_task from phases where issue = $1 and role = 'tester'",
		key).Scan(&entry, &entryTask); err != nil {
		t.Fatalf("read the phases row of %s after the roll-forward: %v", key, err)
	}
	if entry != 1 || entryTask != "" {
		t.Fatalf("the phases row of %s = entry %d carrying %q, want entry 1 carrying no task: the second fill reads the token that daemon minted, and the role owes no start to take a task from",
			key, entry, entryTask)
	}
	var given, served int64
	if err := pool.QueryRow(ctx, "select given_entry, served_entry from claims where token = $1",
		string(token)).Scan(&given, &served); err != nil {
		t.Fatalf("read the claim of %s after the roll-forward: %v", key, err)
	}
	if given != 0 || served != 0 {
		t.Fatalf("the claim of %s was given entry %d and served entry %d, want 0 and 0: it serves no token, so the fill counts it as holding none of that entry's work",
			key, given, served)
	}

	elsewhere := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = 'supervise'
			and payload->>'op' = 'start' and issue <> $1`, key).Scan(&n); err != nil {
			t.Fatalf("count the start rows of every other issue: %v", err)
		}
		return n
	}
	before := elsewhere()
	w.applyEntrySweep(ctx, time.Now().Add(time.Hour))
	starts := sweptStarts(t, pool, key)
	if len(starts) != 1 {
		t.Fatalf("the sweep after the roll-forward sent %s %+v, want the one start of entry 1: a role whose task that daemon lost is what the re-arm exists to send again",
			key, starts)
	}
	want := sweptStart{id: starts[0].id, role: claim.RoleTester, task: workflow.ResumePhaseTask(issue), entry: 1}
	if starts[0] != want {
		t.Fatalf("the start the sweep sent %s = %+v, want %+v: the record holds no task of that entry, so what is sent is the line the daemon composes for a worker already in its phase",
			key, starts[0], want)
	}
	if after := elsewhere(); after != before {
		t.Fatalf("the sweep after the roll-forward wrote %d start rows elsewhere, want none beyond the %d standing: the recovery brought the assignment columns back empty, so the second fill passes over every shape of the first half and each stays level",
			after-before, before)
	}
}

// preEntrySuperviseRequest is record.SuperviseRequest as the release before this one declares it,
// at commit e90c078559: every field of this release's shape but the two it adds, Entry and
// ResendEntry. It is a copy because that release's own type is gone from the tree, and the
// recovery above is judged on exactly what that release would make of the payloads left behind.
type preEntrySuperviseRequest struct {
	Op            record.SuperviseOp    `json:"op"`
	Tree          string                `json:"tree"`
	Role          claim.Role            `json:"role"`
	Task          string                `json:"task,omitempty"`
	Kept          *record.KeptDecisions `json:"kept,omitempty"`
	Generation    uint64                `json:"generation"`
	Phase         phase.Phase           `json:"phase,omitempty"`
	Assignment    string                `json:"assignment,omitempty"`
	ResumeTask    bool                  `json:"resumeTask,omitempty"`
	RuleResume    bool                  `json:"ruleResume,omitempty"`
	NeedsSlot     bool                  `json:"needsSlot,omitempty"`
	Relaunch      bool                  `json:"relaunch,omitempty"`
	Linger        uint64                `json:"linger,omitempty"`
	Reason        string                `json:"reason,omitempty"`
	CheckLook     int                   `json:"checkLook,omitempty"`
	CheckOpenedAt time.Time             `json:"checkOpenedAt,omitzero"`
}

// columnOf is the value of the named column of one read row, as a string, and false where the read
// names no such column.
func columnOf(fields []pgconn.FieldDescription, values []any, name string) (string, bool) {
	for i, field := range fields {
		if field.Name != name || i >= len(values) {
			continue
		}
		held, ok := values[i].(string)
		if !ok {
			return fmt.Sprintf("%v", values[i]), true
		}
		return held, true
	}
	return "", false
}
