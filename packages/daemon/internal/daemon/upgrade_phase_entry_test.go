package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
// through everything but it whatever lands beside it.
const phaseEntryMigration = 20261111225552

// upgradeProject is the Dispatch project of the tree the upgrade is read on, and upgradeRoot its
// root. Each shape is a member of that one tree, standing in testing, so the role every shape is
// read on is the tester (phase.RoleFor).
const (
	upgradeProject = "UPGRADE"
	upgradeRoot    = "UPGRADE-800"
)

// preEntryStore is a database carrying the schema of the release before the phase entry: every
// embedded migration but phaseEntryMigration, each applied as Store.Migrate applies one — the
// migration's SQL, the widening of schema_version for a timestamped version, and the version row
// — and that migration itself withheld, for the test's own Store.Migrate to run. The migrations
// above it are the fixture's too, because the release before this one records them: the one
// standing there, 20261112180548_claim_leak_check_cause, is the contract 87 release's own, so a
// database that release migrated holds its row and its claims.leak_check_cause before the phase
// entry runs, and Store.Migrate, which decides each migration on its own row, then applies the
// phase entry alone. That column is one no statement of the phase entry reads or writes.
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
		if migration.Version == phaseEntryMigration {
			continue
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
// derives from the token that daemon minted (upgradeRollForwardSweepsTheLostTask). A way back
// that zeroed the six columns instead of re-arming this migration would have that role swept
// nothing: the migration would not run again, its entry and its claim would both read 0, and a
// role level at zero is owed nothing — which is all workflow.OwedEntry answers and all admit's
// owedEntryRoles selects.
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
	if applied != 1 {
		t.Fatalf("the upgrade applied %d migrations, want the one phase entry migration: every other migration is the fixture's own", applied)
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
		Settings: supervise.FixedSettings(
			supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
			supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond}),
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
// The release before this one refused that completion 409, under the code it named after its
// minted token: its fence compared the token on the role's phases row with the one the claim
// served, both were set, and they differed, because the task that minted the row's token never
// reached the worker. The upgrade leaves no such pair. The claim serves entry 0 — the fill wrote
// it no served entry, and nothing has delivered the work the sweep just sent — and the entry
// fence compares only when both sides name one (api/handoff.go), so the turn in flight completes
// instead of being refused for work the role was never given.
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
		Settings: api.NewLiveSettings(api.HeldSettings{Steps: config.DesignGateRootIssues.Steps()}), Supervisor: sup, Project: project, DispatchProject: upgradeProject,
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
