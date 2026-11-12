package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
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
// read on is the tester (workflow.RoleFor).
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
// issue still owes that role, empty where it owes none. wantGiven and wantServed are the two
// numbers the migration's fill must leave on the claim, which is what decides everything below:
// a claim at or above its row's entry is holding that entry's work. wantTask is the task the
// sweep must send, empty where it must send nothing, and wantResume says the task is the line the
// daemon composes for a worker already in its phase (workflow.ResumePhaseTask), which the fixture
// can only build once the issue record stands.
type upgradeShape struct {
	key         string
	title       string
	assignment  string
	claimed     bool
	serving     string
	state       supervise.ClaimState
	suspendedBy supervise.SuspendSource
	session     string
	delivery    string
	owedTask    string
	wantGiven   int64
	wantServed  int64
	wantResume  bool
	wantTask    string
}

// The upgrade to the phase entry, on the record the release before it leaves and nothing else: the
// three assignment columns filled, no entry column anywhere, and the migration this release adds
// still to run. What runs over it is the daemon's own entry sweep (intake.EntrySweep,
// workflowRuntime.tickEntrySweep) — no operator call, no completion, no Dispatch event and no
// write of any other kind — so what each shape is sent is the migration's fill read by
// workflow.OwedEntry and nothing else.
//
// The seven shapes are the states that record can be in at the moment the daemon stops, and each
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
//     the row a stopped tree waits on — is sent that row's own task, which the migration took from
//     the row's payload into phases.entry_task. The other four are sent the line the daemon
//     composes for a worker already in its phase (workflow.ResumePhaseTask), since the release
//     before this one recorded no task of the entry it was handed.
//
// The sweep then runs again on its period and writes nothing more: the start it wrote carries the
// entry, so every later pass reads that entry as already carried (workflow.OwedEntry).
//
// The second half is the way back the migration's header names. The three recovery statements run,
// and then the record is read as the release before this one reads it — its own claim read, its
// phases read and the pending delivery its claim read joins, as literal SQL taken from that
// release — and each answers, with the assignment columns at the empty string the recovery's
// default leaves on every row. That is the whole of what a test without the old binary can show:
// it cannot show that daemon running, so what the recovery costs that daemon — a fence that
// compares nothing and mints a fresh token at each role's next start — is not asserted here.
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
	owedRows := map[string]int64{}
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
		if shape.owedTask == "" {
			continue
		}
		// The start row the issue still owes that role, with the payload that release wrote: the
		// assignment on the row beside the task, which is the shape the migration's fill reads the
		// entry's work out of.
		payload, err := json.Marshal(map[string]any{"op": "start", "tree": root.Key, "role": "tester",
			"phase": "testing", "generation": 1, "assignment": shape.assignment, "needsSlot": true, "task": shape.owedTask})
		if err != nil {
			t.Fatal(err)
		}
		var owed int64
		if err := pool.QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('supervise', $1, $2, 0, now(), '') returning id`, shape.key, string(payload)).Scan(&owed); err != nil {
			t.Fatalf("record the start %s still owes its tester: %v", shape.key, err)
		}
		owedRows[shape.key] = owed
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

	// The sweep's own period is what this waits on rather than the rows it writes: three sweeps
	// have run, each writing in the transaction that recorded it, so what the record holds
	// afterwards is every row the sweep means to write and no row a later one adds. A shape the
	// sweep sent nothing then fails below under its own name.
	sweeping, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.tickEntrySweep(sweeping); close(done) }()
	testwait.Eventually(t, "three entry sweeps applied", func() bool { return swept() >= 3 })
	stop()
	<-done

	for i, shape := range shapes {
		if shape.wantResume {
			shapes[i].wantTask = workflow.ResumePhaseTask(issues[shape.key])
			continue
		}
		shapes[i].wantTask = shape.owedTask
	}
	for _, shape := range shapes {
		starts := sweptStarts(t, pool, shape.key)
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
		if owed, ok := owedRows[shape.key]; ok && starts[0].id == owed {
			t.Fatalf("the start the sweep sent %s is row %d, the row the issue already owed: a start of an entry is a row of its own (record.Store.EnqueueStart)",
				shape.key, owed)
		}
	}
	if n := swept(); n < 3 {
		t.Fatalf("sweeps applied = %d, want at least 3: a later sweep must be read as writing nothing more", n)
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

	upgradeCompletionAccepted(t, st, engine, issues["UPGRADE-803"], held[tokens["UPGRADE-803"]])
	upgradeRecoveryReadsAnswer(t, pool, tokens["UPGRADE-802"], "UPGRADE-802")
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
// without the old binary can take it: the three recovery statements run with the daemon stopped,
// and then the record is read as the release before this one reads it.
//
// The three reads are that release's own, as literal SQL at commit e90c078559: its claim read
// (internal/store/claims.go, claimSelect), which joins the pending delivery and is the one read
// that names both dropped columns of the claim side; its phases read (internal/record/store.go,
// Postgres.Phases); and the delivery read its retirement makes (RetireDelivery). Each must answer
// a row, and the columns the recovery brought back must read as the empty string their default
// leaves, which is the whole reason the header can say the older daemon's fence compares nothing
// and refuses none of the completions in flight.
//
// What this cannot show is that daemon running: no binary of it is here to boot against the
// recovered schema, so that it then mints a fresh token at each role's next start is not asserted.
func upgradeRecoveryReadsAnswer(t *testing.T, pool *pgxpool.Pool, token claim.Token, issue string) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		"alter table phases add column assignment text not null default '';",
		"alter table claims add column serving_assignment text not null default '';",
		"alter table pending_task_deliveries add column assignment text not null default '';",
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
