package admit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// architectUpgradeVersion is the version of the migration these tests run
// (internal/store/migrations/20261113182000_architect_entry_upgrade.up.sql), and
// architectUpgradeGeneration the run every root of this file stands at, which the marks the fill
// writes are keyed by.
const (
	architectUpgradeVersion    = 20261113182000
	architectUpgradeGeneration = 1
)

// applyArchitectUpgrade runs that migration's own SQL over the records the pool holds. The pool
// carries every migration already, so the fill ran once over an empty record: each case seeds the
// shape the release before this one leaves and runs the fill over it, which is what the upgrade
// does, and what the case then reads is the migration's own writing rather than a fixture's copy
// of it.
func applyArchitectUpgrade(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if migration.Version != architectUpgradeVersion {
			continue
		}
		if _, err := pool.Exec(context.Background(), migration.SQL); err != nil {
			t.Fatalf("run the fill of %s: %v", migration.Name, err)
		}
		return
	}
	t.Fatalf("no embedded migration of version %d: these tests run its own fill", architectUpgradeVersion)
}

// upgradedRoot is a root as the release before this one left it: at admitted, in the workflow,
// holding one of the cap's slots admitted now, with an architect claim in state and no architect
// phases row at all, since that release raised no entry for an architect and wrote it no row.
func upgradedRoot(key string, state supervise.ClaimState) architectWakeFixture {
	f := architectWakeRoot(key)
	f.generation = architectUpgradeGeneration
	f.state = state
	f.entry, f.given, f.task = 0, 0, false
	return f
}

// sweepAt applies one entry sweep with the pass reading the clock at at, which is how these cases
// cover the gate arm's ninety minutes without waiting them out: the bound is read from the
// admission's own now (wakeStalledArchitect).
func sweepAt(t *testing.T, pool *pgxpool.Pool, admission *Admission, eventID string, at time.Time) {
	t.Helper()
	admission.now = func() time.Time { return at }
	applySweep(t, pool, admission, eventID)
}

// upgradeClaims is every claim the record holds, as the daemon reads them for the state
// (store.Store.Claims, projection.Project's claims).
func upgradeClaims(t *testing.T, pool *pgxpool.Pool) []supervise.Claim {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the record store: %v", err)
	}
	defer st.Close()
	claims, err := st.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	return claims
}

// setClaimState records state on the claim of root's architect, as a launch that failed and an
// operator's `legion claims stop` each leave it.
func setClaimState(t *testing.T, pool *pgxpool.Pool, root string, state supervise.ClaimState) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update claims set state = $2 where token = $1`,
		string(architectToken(t, root)), string(state)); err != nil {
		t.Fatalf("record the architect claim of %s as %s: %v", root, state, err)
	}
}

// The first passes after the upgrade send no architect its first task and wake the project's
// controller for no root. Three roots stand as the release before this one left them — a tree
// whose work is under way under a phase worker, a root at admitted with no gate rows whose
// architect is working, and a root at admitted whose architect holds nothing — and the fill
// settles all three at entry 1 on the row and on the claim, which is a claim owed nothing
// (workflow.OwedEntry). Without it each of them would be read as owed an entry it was never
// given, and an architect in the middle of writing its tree's spec would be told to take the tree
// up again.
//
// The passes cover ninety minutes of simulated time, which is the gate arm's own bound
// (architectGateWake): the arm reads a root standing at admitted with no design-gate step
// registered, which is what two of these three roots are, and the mark the fill wrote is what
// keeps that reading from waking anybody.
func TestTheArchitectUpgradeSendsNoTaskAndRaisesNoWakeOnTheFirstPasses(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 3)
	const working, writing, holding = "LEGION-830", "LEGION-831", "LEGION-832"

	// The tree whose work is under way: a root a hand-over carried in, standing in the phase that
	// came with it, with its implementer at the entry the phase entry migration settled it on and
	// holding the task of that entry. A carried root is one the sweep's architect reach serves,
	// so what keeps its architect quiet is the fill and not the reach.
	underWay := upgradedRoot(working, supervise.StateIdle)
	underWay.stands, underWay.carried = phase.Implementing, true
	seedArchitectWakeRoot(t, pool, underWay)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: working,
			Role: claim.RoleImplementer, Claim: "legion-" + working + "-implementer", Entry: seededEntry,
			EntryTask: seededEntryTask(working), EntryRaisedAt: fixedNow}); err != nil {
			t.Fatalf("record the implementer's phases row of %s: %v", working, err)
		}
	})
	worker := putWorkerClaim(t, pool, working, working, claim.RoleImplementer, architectUpgradeGeneration,
		supervise.StateWorking, "")
	markGivenEntry(t, pool, worker, seededEntry)
	putWorkerTask(t, pool, worker, architectUpgradeGeneration, phase.Implementing, true)

	seedArchitectWakeRoot(t, pool, upgradedRoot(writing, supervise.StateWorking))
	seedArchitectWakeRoot(t, pool, upgradedRoot(holding, supervise.StateIdle))

	applyArchitectUpgrade(t, pool)

	for _, pass := range []struct {
		id string
		at time.Time
	}{
		{id: "upgrade-1", at: fixedNow},
		{id: "upgrade-2", at: fixedNow.Add(30 * time.Minute)},
		{id: "upgrade-3", at: fixedNow.Add(insideTheGate)},
		{id: "upgrade-4", at: fixedNow.Add(pastTheGate)},
		{id: "upgrade-5", at: fixedNow.Add(pastTheGate + time.Hour)},
	} {
		sweepAt(t, pool, admission, pass.id, pass.at)
	}

	for _, root := range []string{working, writing, holding} {
		if starts := convergeStarts(t, pool, root); len(starts) != 0 {
			t.Fatalf("the starts written for %s = %+v, want none: the fill settled every role of it at the entry its row names", root, starts)
		}
		if reasons := architectWakes(t, pool, root); len(reasons) != 0 {
			t.Fatalf("the architect wakes of %s = %q, want none: the release wakes the controller for no root it does not repair", root, reasons)
		}
		marks := architectWakeMarks(t, pool, root, architectUpgradeGeneration)
		if len(marks) != 2 || marks[0] != string(record.ArchitectSlowBeforeGate) || marks[1] != string(record.ArchitectTookNothing) {
			t.Fatalf("the arms %s is marked told for = %v, want the first and the third the fill wrote", root, marks)
		}
		row := architectRow(t, pool, root)
		if row.Entry != 1 || row.EntryTask != "" || !row.EntryRaisedAt.IsZero() {
			t.Fatalf("the architect row of %s = entry %d, task %q, raised at %s; want entry 1 with no task and no moment",
				root, row.Entry, row.EntryTask, row.EntryRaisedAt)
		}
	}
	// The quiet is the fill's and not a fixture the pass never looked at: the sweep's architect
	// reach serves each of these roots, and the two at admitted are exactly what the gate arm
	// reads.
	for _, root := range []string{working, writing, holding} {
		inTx(t, pool, func(tx pgx.Tx) {
			reached, err := workflow.ArchitectAtWork(context.Background(), tx, record.NewStore(), issue(t, pool, root))
			if err != nil || !reached {
				t.Fatalf("the architect reach over %s = %t, %v, want the root inside it", root, reached, err)
			}
		})
	}
}

// A claim the fill settled that then fails, and one an operator retires, each wake the project's
// controller once. The fill marks the first and the third arm and leaves the second unmarked,
// because the second reports a claim that is failed or retired: the record states that plainly,
// no send of the daemon's cures it, and the reason the fill stands on does not cover it.
func TestTheArchitectUpgradeStillWakesForAClaimThatFailsOrIsRetired(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		why   string
	}{
		{name: "a claim that failed", state: supervise.StateFailed,
			why: "a failed architect claim of a running tree is the claim workflow.Stranded has always named"},
		{name: "a claim an operator retired", state: supervise.StateRetired,
			why: "`legion claims stop` stopped that architect, and nothing the daemon sends starts it again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			const root = "LEGION-833"
			seedArchitectWakeRoot(t, pool, upgradedRoot(root, supervise.StateIdle))
			applyArchitectUpgrade(t, pool)
			setClaimState(t, pool, root, tc.state)

			for _, pass := range []string{"failed-1", "failed-2", "failed-3"} {
				applySweep(t, pool, admission, pass)
			}

			assertArchitectWakes(t, pool, root, []record.ArchitectStallArm{record.ArchitectClaimUnreachable}, tc.why)
			marks := architectWakeMarks(t, pool, root, architectUpgradeGeneration)
			if len(marks) != 3 {
				t.Fatalf("the arms %s is marked told for = %v, want the fill's two and this arm: %s", root, marks, tc.why)
			}
		})
	}
}

// The state of a root the fill settled carries its two entries and neither time. That is how a
// reader tells such a root from one this release served: the fill writes the numbers and no
// delivery, so the claim holds no task of its entry and the handshake has nothing to show
// (api.Issue.EntryQueuedAt, EntryConfirmedAt).
func TestTheArchitectUpgradeLeavesTheStateCarryingTwoEntriesAndNoTimes(t *testing.T) {
	pool := migratedPool(t)
	const root = "LEGION-834"
	seedArchitectWakeRoot(t, pool, upgradedRoot(root, supervise.StateIdle))
	applyArchitectUpgrade(t, pool)

	var state api.State
	claims := upgradeClaims(t, pool)
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		state, err = projection.Project(context.Background(), tx, record.NewStore(), testProject, nil, claims, nil, nil, fixedNow)
		if err != nil {
			t.Fatalf("project the state: %v", err)
		}
	})
	view, served := state.Issues[root]
	if !served {
		t.Fatalf("the state carries no view of %s", root)
	}
	if view.PhaseEntry != 1 || view.GivenEntry != 1 {
		t.Fatalf("the state of %s reads phaseEntry %d, givenEntry %d; want 1 and 1: the fill settled the row and the claim together",
			root, view.PhaseEntry, view.GivenEntry)
	}
	if view.EntryQueuedAt != nil || view.EntryConfirmedAt != nil {
		t.Fatalf("the state of %s reads the entry queued %v and confirmed %v, want neither: the fill writes no delivery, and the two "+
			"numbers with no times beside them are what tell a root of the older release from one this release served",
			root, view.EntryQueuedAt, view.EntryConfirmedAt)
	}
}

// A root admitted after the upgrade is owed its first task as any root of this release is, and
// so is a root the older daemon admitted in the window the upgrade leaves: after the migration
// ran and before this daemon served. The fill settles the roots standing when it runs and
// nothing else.
//
// The root this release admits is handed its entry by the promotion itself. The root of the
// window holds an architect claim with no phases row, because the older daemon's promotion
// raised no entry and the fill never saw it: the first pass raises entry 1 with the derivation's
// own task and sends it (workflow.RaiseArchitectEntryAndStart), and a second pass raises no
// second entry and writes no second row. No mark of the fill stands in the way of either root.
func TestARootAdmittedAfterTheArchitectUpgradeIsOwedItsTask(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 3, nil)
	const settled, fresh, window = "LEGION-835", "LEGION-836", "LEGION-838"
	seedArchitectWakeRoot(t, pool, upgradedRoot(settled, supervise.StateIdle))
	applyArchitectUpgrade(t, pool)
	// The root the older daemon admitted after the fill ran: its architect claim stands at this
	// generation and its architect holds no phases row.
	seedArchitectWakeRoot(t, pool, upgradedRoot(window, supervise.StateIdle))

	apply(t, pool, admission, "admitted-after", intake.DispatchIssue{Key: fresh, ResolvedRepo: "acme/widgets", Seq: 1,
		Type: "issue.created", Status: "todo", Title: "new", Rank: "B", HandedOver: true}, admission.engine)

	start := assertArchitectEntry(t, pool, fresh, 1)
	if start.Task == "" {
		t.Fatalf("the architect start of %s carries no task, want the first act the derivation composes", fresh)
	}
	if marks := architectWakeMarks(t, pool, fresh, 1); len(marks) != 0 {
		t.Fatalf("%s is marked told for %v, want no arm: the fill marked the roots standing when it ran and no other", fresh, marks)
	}
	if starts := convergeStarts(t, pool, settled); len(starts) != 0 {
		t.Fatalf("the starts written for the settled %s = %+v, want none", settled, starts)
	}

	opened := assertArchitectEntry(t, pool, window, 1)
	if want := architectStart(t, pool, admission, window, ""); opened.Task != want {
		t.Fatalf("the task the first pass sent %s = %q, want the derivation's %q", window, opened.Task, want)
	}
	if row := architectRow(t, pool, window); row.EntryTask != opened.Task {
		t.Fatalf("the architect row of %s carries %q while its start carries %q, want the one text on both", window, row.EntryTask, opened.Task)
	}
	if marks := architectWakeMarks(t, pool, window, architectUpgradeGeneration); len(marks) != 0 {
		t.Fatalf("%s is marked told for %v, want no arm: the fill ran before that root was admitted", window, marks)
	}

	applySweep(t, pool, admission, "window-again")

	if again := assertArchitectEntry(t, pool, window, 1); again != opened {
		t.Fatalf("the architect start of %s after a second pass = %+v, want the %+v the first wrote", window, again, opened)
	}
}

// A root the fill settled and a person then hands back is admitted again, and that re-admission
// raises its architect's entry from the 1 the fill wrote to 2 and carries this run's own work on
// the start. The run it is admitted at is a run of its own, so its bounds start again: the marks
// the fill wrote are of the run before it, and the gate arm wakes the controller once the new run
// has stood its own ninety minutes.
func TestTheArchitectUpgradeReadmissionRaisesTheEntryAgainAndStartsItsOwnBounds(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	const root = "LEGION-837"
	// The root as the park left it, with the architect claim of the run that was parked: the
	// shape parkedRoot records, under a key a claim token can be minted from, since the sweep
	// reads no run of an issue whose key yields none.
	until := fixedNow.Add(72 * time.Hour)
	putIssue(t, pool, record.Issue{Key: root, Tree: root, Project: testProject, Title: "root", Phase: phase.Done,
		Generation: 1, Status: "backlog", Rank: "A", HandedOver: true, LingerUntil: &until,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
	putWorkerClaim(t, pool, root, root, claim.RoleArchitect, 1, supervise.StateIdle, "")
	applyArchitectUpgrade(t, pool)
	if row := architectRow(t, pool, root); row.Entry != 1 {
		t.Fatalf("the architect row of the parked %s = entry %d, want the 1 the fill wrote", root, row.Entry)
	}

	apply(t, pool, admission, "readmitted", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 5,
		Type: "issue.updated", Status: "todo", Title: "root", Rank: "A", HandedOver: true}, admission.engine)

	start := assertArchitectEntry(t, pool, root, 2)
	if start.Task == "" {
		t.Fatalf("the re-admission's architect start of %s carries no task, want this run's own work", root)
	}
	readmitted := issue(t, pool, root)
	if readmitted.Generation != 2 {
		t.Fatalf("the re-admitted %s stands at generation %d, want 2: the marks of the run before say nothing of this one",
			root, readmitted.Generation)
	}

	sweepAt(t, pool, admission, "readmitted-inside", fixedNow.Add(insideTheGate))
	if reasons := architectWakes(t, pool, root); len(reasons) != 0 {
		t.Fatalf("the architect wakes of the re-admitted %s = %q, want none inside its own bound", root, reasons)
	}
	sweepAt(t, pool, admission, "readmitted-past", fixedNow.Add(pastTheGate))

	assertArchitectWakes(t, pool, root, []record.ArchitectStallArm{record.ArchitectSlowBeforeGate},
		"the run after the re-admission has stood its own ninety minutes at admitted with no design gate registered")
	if marks := architectWakeMarks(t, pool, root, 2); len(marks) != 1 || marks[0] != string(record.ArchitectSlowBeforeGate) {
		t.Fatalf("the arms the re-admitted %s is marked told for = %v, want the gate arm once for its own run", root, marks)
	}
	if marks := architectWakeMarks(t, pool, root, 1); len(marks) != 2 {
		t.Fatalf("the arms the run before is marked told for = %v, want the fill's two, left where they are", marks)
	}
	if starts := architectStartsOf(t, pool, root); len(starts) != 1 {
		t.Fatalf("%s holds %d architect starts after its passes, want the one its re-admission wrote: a start already on its way is "+
			"not written again (workflow.ArchitectStartPending)", root, len(starts))
	}
}

// embeddedMigration is the text of migration version as the binary ships it (migrations.All).
func embeddedMigration(t *testing.T, version int) string {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if migration.Version == version {
			return migration.SQL
		}
	}
	t.Fatalf("no embedded migration of version %d", version)
	return ""
}

// wayBackHeredoc and wayBackTerminator open and close a way back in a migration's header: a psql
// heredoc whose lines are the one transaction an operator runs, begin and commit included. They
// are the headers' own lines — the phase entry's (20261111225552_phase_entry.up.sql), and the one
// this migration's header and 20261113181500_architect_stalled_notices.up.sql's both carry — and
// internal/daemon's upgrade_phase_entry_way_back_test.go reads the phase entry's the same way.
const (
	wayBackHeredoc    = "psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'"
	wayBackTerminator = "SQL"
)

// headerWayBack is the way back the header of migration version carries, as the one transaction
// an operator runs: the lines between the heredoc's opener and its terminator, with the comment
// marker and the opener's indent taken off each. The header is the source of that transaction,
// so it is read from the embedded file rather than copied here, and it is sent in one exec, as
// psql sends it.
func headerWayBack(t *testing.T, version int) string {
	t.Helper()
	var lines []string
	indent, open := "", false
	for _, line := range strings.Split(embeddedMigration(t, version), "\n") {
		comment, isComment := strings.CutPrefix(line, "--")
		if !isComment {
			break
		}
		if !open {
			if strings.TrimSpace(comment) == wayBackHeredoc {
				indent, open = comment[:len(comment)-len(strings.TrimLeft(comment, " "))], true
			}
			continue
		}
		text := strings.TrimPrefix(comment, indent)
		if strings.TrimSpace(comment) == "" {
			text = ""
		}
		if text == wayBackTerminator {
			recovery := strings.Join(lines, "\n") + "\n"
			if !strings.HasPrefix(strings.TrimSpace(recovery), "begin;") || !strings.HasSuffix(strings.TrimSpace(recovery), "commit;") {
				t.Fatalf("the way back of migration %d is no transaction from begin to commit: %s", version, recovery)
			}
			return recovery
		}
		lines = append(lines, text)
	}
	t.Fatalf("the header of migration %d carries no %q block closed by %q", version, wayBackHeredoc, wayBackTerminator)
	return ""
}

// phaseEntryVersion is the version of the phase entry's own migration
// (20261111225552_phase_entry.up.sql), whose way back drops the entry columns and re-arms it.
// stalledNoticesVersion is the version of the marks' table
// (20261113181500_architect_stalled_notices.up.sql), whose way back is this migration's.
// olderReleaseVersion is the newest migration of the release before the phase entry,
// 20261112180548_claim_leak_check_cause, which is the version the store reports once both ways
// back have run.
const (
	phaseEntryVersion     = 20261111225552
	stalledNoticesVersion = 20261113181500
	olderReleaseVersion   = 20261112180548
)

// A rollback and the roll-forward after it leave every root architect settled, as the first
// upgrade did. The ways back run in the order docs/kubernetes.md gives an operator, each as the
// one transaction its header carries (headerWayBack): this migration's first, which
// 20261113181500's header carries as well, and the phase entry's after it. This migration's block
// deletes its own version row and 20261113181500's, drops the marks' table and the architect
// wakes still unsent, so the roll-forward makes the table again and runs this fill again after
// the phase entry's. Then the first pass prompts no architect and wakes the controller for no
// arm, and each root is marked again at the run it stands at.
//
// One architect-stalled wake stands unsent before the ways back, as a rollback can find one: the
// release before this one refuses its kind and would retry it for ever, so the block drops it and
// none stands after the two blocks. The store between the blocks and the roll-forward is that
// release's: it reports that release's own version and holds no marks' table.
//
// The record is the one the older release leaves after the rollback, written with that release's
// own statements: two roots at admitted with no design gate registered, each holding an architect
// claim and no architect phases row, since that release raised no entry for an architect. One of
// them the older daemon re-admitted inside the window, so it stands at generation 2, and the
// marks the first run wrote at generation 1 went with the table. The pass reads the clock past
// the gate arm's own bound, so a mark missing for either root would wake the controller.
//
// The control is the same record with only the phase entry's way back run. Migrate then runs the
// phase entry's fill and not this one, each architect reads entry 0 beside its claim, and the same
// pass hands every one of them a first task at once, which is the defect this migration's way
// back exists to prevent. The unsent wake still stands there, since only this migration's block
// drops it.
func TestTheArchitectUpgradeRunsAgainAfterTheWayBackAndARollForward(t *testing.T) {
	const settled, readmitted = "LEGION-850", "LEGION-851"
	wayBack := headerWayBack(t, architectUpgradeVersion)
	if stalled := headerWayBack(t, stalledNoticesVersion); stalled != wayBack {
		t.Fatalf("the headers of migrations %d and %d carry two ways back, want the one block both are reversed by:\n%s\nagainst\n%s",
			stalledNoticesVersion, architectUpgradeVersion, stalled, wayBack)
	}
	for _, tc := range []struct {
		name string
		// rearmed is whether this migration's own way back runs before the phase entry's.
		rearmed bool
	}{
		{name: "both ways back run", rearmed: true},
		{name: "the phase entry's way back alone", rearmed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := migratedPool(t)
			// The wake this release wrote and had not sent when the daemon stopped.
			inTx(t, pool, func(tx pgx.Tx) {
				row, err := record.NewOutboxRow(settled, record.ControllerNotice{Kind: record.ArchitectStalledNotice,
					Role: claim.RoleArchitect, Reason: record.ArchitectTookNothing.Words() + ": unsent when the daemon stopped"}, fixedNow)
				if err != nil {
					t.Fatalf("build the unsent architect wake of %s: %v", settled, err)
				}
				if err := record.NewStore().Enqueue(ctx, tx, row); err != nil {
					t.Fatalf("queue the unsent architect wake of %s: %v", settled, err)
				}
			})
			if tc.rearmed {
				if _, err := pool.Exec(ctx, wayBack, pgx.QueryExecModeSimpleProtocol); err != nil {
					t.Fatalf("run this migration's way back as its one transaction: %v", err)
				}
			}
			if _, err := pool.Exec(ctx, headerWayBack(t, phaseEntryVersion), pgx.QueryExecModeSimpleProtocol); err != nil {
				t.Fatalf("run the phase entry's way back as its one transaction: %v", err)
			}

			st, err := store.Open(ctx, pool.Config().ConnString())
			if err != nil {
				t.Fatalf("open the record store: %v", err)
			}
			defer st.Close()
			var unsent int
			if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = $1 and payload->>'kind' = $2`,
				string(record.OutboxKindControllerNotice), string(record.ArchitectStalledNotice)).Scan(&unsent); err != nil {
				t.Fatalf("count the unsent architect wakes after the way back: %v", err)
			}
			if !tc.rearmed {
				if unsent != 1 {
					t.Fatalf("the phase entry's way back alone left %d unsent architect wakes, want the 1 only this migration's block drops", unsent)
				}
			} else {
				if unsent != 0 {
					t.Fatalf("the two ways back left %d unsent architect wakes, want none: the older release refuses their kind", unsent)
				}
				version, err := st.SchemaVersion(ctx)
				if err != nil {
					t.Fatalf("read the version the store reports after the way back: %v", err)
				}
				var marksTable bool
				if err := pool.QueryRow(ctx, `select to_regclass('architect_stalled_notices') is not null`).Scan(&marksTable); err != nil {
					t.Fatalf("look for the marks' table after the way back: %v", err)
				}
				if version != olderReleaseVersion || marksTable {
					t.Fatalf("after the two ways back the store reports version %d and the marks' table stands: %v; want %d and no such "+
						"table, the older release's store", version, marksTable, olderReleaseVersion)
				}
			}

			// The record the older release leaves: its issues, slots and lifecycles are this
			// release's too, and its claims carry no entry, which the way back has dropped.
			seedArchitectWakeRoot(t, pool, upgradedRoot(settled, supervise.StateIdle))
			again := upgradedRoot(readmitted, supervise.StateWorking)
			again.generation = 2
			seedArchitectWakeRoot(t, pool, again)

			applied, err := st.Migrate(ctx)
			if err != nil {
				t.Fatalf("roll forward onto the record the older release left: %v", err)
			}
			want := 1
			if tc.rearmed {
				want = 3
			}
			if applied != want {
				t.Fatalf("the roll-forward applied %d migrations, want %d: each way back re-arms its own migrations and no other", applied, want)
			}

			admission := quiet(t, 3)
			sweepAt(t, pool, admission, "roll-forward", fixedNow.Add(pastTheGate))

			for _, root := range []struct {
				key        string
				generation uint64
			}{{settled, 1}, {readmitted, 2}} {
				starts := convergeStarts(t, pool, root.key)
				if !tc.rearmed {
					if len(starts) == 0 {
						t.Fatalf("the pass wrote no start for %s with only the phase entry's way back run, want the first task this "+
							"migration's way back exists to prevent: the control proves nothing", root.key)
					}
					continue
				}
				if len(starts) != 0 {
					t.Fatalf("the starts written for %s after the roll-forward = %+v, want none: its architect is settled again", root.key, starts)
				}
				if reasons := architectWakes(t, pool, root.key); len(reasons) != 0 {
					t.Fatalf("the architect wakes of %s after the roll-forward = %q, want none of any arm", root.key, reasons)
				}
				marks := architectWakeMarks(t, pool, root.key, root.generation)
				if len(marks) != 2 || marks[0] != string(record.ArchitectSlowBeforeGate) || marks[1] != string(record.ArchitectTookNothing) {
					t.Fatalf("the arms %s is marked told for at generation %d = %v, want the first and the third the second run wrote",
						root.key, root.generation, marks)
				}
				if row := architectRow(t, pool, root.key); row.Entry != 1 {
					t.Fatalf("the architect row of %s after the roll-forward = entry %d, want the 1 the second run wrote", root.key, row.Entry)
				}
				var given, served int64
				if err := pool.QueryRow(ctx, `select given_entry, served_entry from claims where token = $1`,
					string(architectToken(t, root.key))).Scan(&given, &served); err != nil {
					t.Fatalf("read the architect claim of %s: %v", root.key, err)
				}
				if given != 1 || served != 1 {
					t.Fatalf("the architect claim of %s after the roll-forward = given %d, served %d, want 1 and 1", root.key, given, served)
				}
			}
		})
	}
}
