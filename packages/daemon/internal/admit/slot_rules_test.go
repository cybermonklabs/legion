package admit

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// slotTree is the running tree every row-rule case stands in: its root LEGION-1, its lifecycle
// open under the workflow, and its design gate registered. approved opens that gate and leaves
// the root in accepting, where a parent whose own phases are behind it waits. It writes no row
// for the root: a caller that wants the root as the daemon leaves it — holding the row it took
// at its admission — takes one with takeSlot.
func slotTree(t *testing.T, pool *pgxpool.Pool, approved bool) {
	t.Helper()
	stands, status := phase.Admitted, "in_progress"
	gate := record.DesignGate{Issue: "LEGION-1", ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1}
	if approved {
		stands, status = phase.Accepting, "needs_review"
		gate.ApprovedVersion = new(1)
		gate.ApprovedMark = record.EventMark{Seq: 4, At: fixedNow.Add(-time.Hour)}
	}
	putIssue(t, pool, record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: "LEGION-1", Tree: "LEGION-1", Phase: stands, Generation: 1, Status: status, DispatchStatus: status, Rank: "A", HandedOver: true})
	seedLifecycle(t, pool, "LEGION-1")
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(context.Background(), tx, gate); err != nil {
			t.Fatalf("seed gate: %v", err)
		}
	})
}

// slotMember records key in LEGION-1's tree at stands, with the Dispatch status a member in that
// phase shows, and a working claim of the phase's own role, so a pass that moved a claim or a
// status would be seen.
func slotMember(t *testing.T, pool *pgxpool.Pool, key string, stands phase.Phase, status string, ready *int) {
	t.Helper()
	parent := "LEGION-1"
	var pending *record.ReadyPending
	if ready != nil {
		pending = &record.ReadyPending{Step: record.DesignStepSpec, Version: *ready}
	}
	putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: key, Tree: "LEGION-1", Parent: &parent, Phase: stands, Generation: 1, Status: status, DispatchStatus: status, Rank: "B",
		ReadyPending: pending, LastDispatchSeq: 3})
	role := workflow.RoleFor(stands)
	if role == "" || role == claim.RoleArchitect {
		role = claim.RoleImplementer
	}
	token, err := claim.NewToken("legion", key, role)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', 'LEGION-1', $2, $3, 1, 'ses-worker', '/tmp/worker.jsonl', 'working', 0, 0, 0, 0)`, string(token), key, string(role)); err != nil {
			t.Fatalf("put claim: %v", err)
		}
	})
}

// takeSlot gives key one of the cap's slots, as a promotion would.
func takeSlot(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		held, err := records.Slots(context.Background(), tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: key, Index: len(held), AdmittedAt: fixedNow}); err != nil {
			t.Fatalf("put slot: %v", err)
		}
	})
}

// oweStart queues the start of key's worker as a transition does (workflow.StartOrOwe): the
// relaunch row, and the task row the outbox holds unclaimable until key holds a slot. It is what
// puts an issue whose work came back in the waiting line.
func oweStart(t *testing.T, pool *pgxpool.Pool, key string, stands phase.Phase) {
	t.Helper()
	role := workflow.RoleFor(stands)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), key, record.SuperviseRequest{Op: "start", Tree: "LEGION-1",
			Role: role, Generation: 1, Phase: stands, Task: "Continue " + key + ".", Assignment: "assignment-" + key}, fixedNow); err != nil {
			t.Fatalf("owe %s a start: %v", key, err)
		}
	})
}

// holdsSlot says whether key holds one of the project's admission slots.
func holdsSlot(t *testing.T, pool *pgxpool.Pool, key string) bool {
	t.Helper()
	for _, slot := range slots(t, pool) {
		if slot.Issue == key {
			return true
		}
	}
	return false
}

// claimStates is the state of every claim in the store, by token.
func claimStates(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select token, state from claims`)
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	defer rows.Close()
	states := map[string]string{}
	for rows.Next() {
		var token, state string
		if err := rows.Scan(&token, &state); err != nil {
			t.Fatalf("scan claim: %v", err)
		}
		states[token] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate claims: %v", err)
	}
	return states
}

// dispatchStatuses is the status of every row of the Dispatch mirror, by key.
func dispatchStatuses(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select key, status from dispatch_issues`)
	if err != nil {
		t.Fatalf("list the mirror: %v", err)
	}
	defer rows.Close()
	statuses := map[string]string{}
	for rows.Next() {
		var key, status string
		if err := rows.Scan(&key, &status); err != nil {
			t.Fatalf("scan mirror row: %v", err)
		}
		statuses[key] = status
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the mirror: %v", err)
	}
	return statuses
}

// issuePhases is the phase of every issue in the store, by key.
func issuePhases(t *testing.T, pool *pgxpool.Pool) map[string]phase.Phase {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select key, phase from issues`)
	if err != nil {
		t.Fatalf("list issues: %v", err)
	}
	defer rows.Close()
	phases := map[string]phase.Phase{}
	for rows.Next() {
		var key string
		var stands phase.Phase
		if err := rows.Scan(&key, &stands); err != nil {
			t.Fatalf("scan issue: %v", err)
		}
		phases[key] = stands
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate issues: %v", err)
	}
	return phases
}

// assertRowHolders compares the keys holding one of the project's rows, in key order, with want.
func assertRowHolders(t *testing.T, pool *pgxpool.Pool, want ...string) {
	t.Helper()
	var got []string
	for _, slot := range slots(t, pool) {
		got = append(got, slot.Issue)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the issues holding a row = %#v, want %#v", got, want)
	}
}

// A1. One row rule per phase, read by admission passes over trees that run.
//
// The daemon creates a slots row for an issue when it starts it, and deletes that row in four
// cases only: the issue's own phase is done; Dispatch shows it in backlog, icebox, triage or
// done; it belongs to a tree whose root closed or a tree the daemon no longer runs; or its
// phase is awaiting_merge. No other phase deletes a row, so an issue keeps its place against
// the cap while it waits — in awaiting_person, in accepting, in held, in merging while a closed
// design gate holds its merge request, and as a root standing at admitted whose design gate has
// opened.
//
// The first reading gives one started issue each of the phases that keep a row and reads the
// rows one admission pass leaves. The one exception the rule keeps is read beside them: a
// started issue in awaiting_merge, whose row goes on that same pass. The second reading makes
// the three deletions that are left — a phase of done while Dispatch still shows the issue
// in_progress, a Dispatch status of backlog while the phase is still in the workflow, and a
// tree whose root closed — and reads that those three rows go and no others. A child the daemon
// has not started holds no row in either reading.
//
// A row is accounting and nothing else: the pass suspends no claim, moves no phase and writes
// no Dispatch status, which the three unchanged reads around each pass pin.
func TestSlotPerPhase(t *testing.T) {
	pool := migratedPool(t)
	// A cap far above the rows seeded here, so the cap holds nothing back: each issue below
	// keeps or loses its row because the rule says so, and the two issues that start nothing
	// are held where they stand by a reason of their own.
	admission := quiet(t, 16)

	// Tree 1: its root at admitted with its design gate opened, one member in each phase that
	// keeps a row, and one in awaiting_merge.
	slotTree(t, pool, true)
	root := issue(t, pool, "LEGION-1")
	root.Phase, root.Status, root.DispatchStatus = phase.Admitted, "in_progress", "in_progress"
	putIssue(t, pool, root)
	takeSlot(t, pool, "LEGION-1")
	// The root's own first phase could start on this very pass — its gate is open and it holds
	// its row, so no cap can hold it back — which would move it out of the phase this case is
	// about. An open Dispatch child holds it at admitted instead (record.LineFacts.ChildWaits),
	// and the state names it under admission.blocked, read below beside its row.
	seedMirror(t, pool, "LEGION-1", "in_progress")
	seedMirrorChild(t, pool, "LEGION-99", "backlog", "LEGION-1")

	for _, member := range []struct {
		key    string
		stands phase.Phase
		status string
		ready  *int
	}{
		{key: "LEGION-2", stands: phase.Planning, status: "in_progress"},
		{key: "LEGION-3", stands: phase.Implementing, status: "in_progress"},
		{key: "LEGION-4", stands: phase.Testing, status: "testing"},
		{key: "LEGION-5", stands: phase.Reviewing, status: "needs_review"},
		{key: "LEGION-6", stands: phase.Accepting, status: "needs_review"},
		{key: "LEGION-7", stands: phase.Retro, status: "retro"},
		// A merger that has built its merge request and waits for a closed design gate to
		// release it (record.ReadyPending).
		{key: "LEGION-8", stands: phase.Merging, status: "retro", ready: new(2)},
		{key: "LEGION-9", stands: phase.Held, status: "in_progress"},
		// The one exception: this row goes on the pass that keeps every other row here.
		{key: "LEGION-10", stands: phase.AwaitingMerge, status: "retro"},
	} {
		slotMember(t, pool, member.key, member.stands, member.status, member.ready)
		takeSlot(t, pool, member.key)
	}

	// Tree 2: a root at work of its own, whose close ends the whole tree in the second reading.
	putIssue(t, pool, record.Issue{Key: "LEGION-20", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: "LEGION-20", Tree: "LEGION-20", Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: "C", HandedOver: true})
	seedLifecycle(t, pool, "LEGION-20")
	takeSlot(t, pool, "LEGION-20")

	// Tree 3: a root waiting on a person, which keeps its row, and a child of it the daemon has
	// never started. The root's wait is what holds that child where it stands, so its first
	// phase does not start and it takes no row.
	putIssue(t, pool, record.Issue{Key: "LEGION-30", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: "LEGION-30", Tree: "LEGION-30", Phase: phase.AwaitingPerson, Generation: 1,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: "D", HandedOver: true,
		Awaiting: &record.Awaiting{Ask: "ask-30", Since: record.EventMark{Seq: 9}}})
	seedLifecycle(t, pool, "LEGION-30")
	takeSlot(t, pool, "LEGION-30")
	unstarted := "LEGION-30"
	putIssue(t, pool, record.Issue{Key: "LEGION-31", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: "LEGION-31", Tree: "LEGION-30", Parent: &unstarted, Phase: phase.Admitted, Generation: 1,
		Status: "todo", DispatchStatus: "todo", Rank: "D1", LastDispatchSeq: 2})

	// pass runs one admission pass and reads, around it, the three things a row never moves.
	pass := func(stage, eventID string) {
		t.Helper()
		phases, claims, statuses := issuePhases(t, pool), claimStates(t, pool), dispatchStatuses(t, pool)

		applyFact(t, pool, admission, eventID, intake.ControllerTick{})

		for key, stood := range issuePhases(t, pool) {
			if stood != phases[key] {
				t.Fatalf("%s moved from %s to %s at %s, want the row rule to move no phase", key, phases[key], stood, stage)
			}
		}
		for token, state := range claimStates(t, pool) {
			if state != claims[token] {
				t.Fatalf("claim %s = %s at %s, was %s; want the row rule to suspend nothing", token, state, stage, claims[token])
			}
		}
		for key, status := range dispatchStatuses(t, pool) {
			if status != statuses[key] {
				t.Fatalf("the mirror row of %s = %s at %s, was %s; want the row rule to write no Dispatch status",
					key, status, stage, statuses[key])
			}
		}
	}

	pass("the first reading", "rows-per-phase")

	assertRowHolders(t, pool, "LEGION-1", "LEGION-2", "LEGION-3", "LEGION-4", "LEGION-5", "LEGION-6", "LEGION-7",
		"LEGION-8", "LEGION-9", "LEGION-20", "LEGION-30")
	if want := []record.BlockedIssue{{Issue: "LEGION-1", BlockedBy: []string{"LEGION-99"}}}; !reflect.DeepEqual(blockedNow(t, pool, admission), want) {
		t.Fatalf("blocked = %#v, want %#v: the root keeps its row at admitted because an open Dispatch child holds it there",
			blockedNow(t, pool, admission), want)
	}

	// The three deletions that are left, made together and read by one pass.
	retro := issue(t, pool, "LEGION-7")
	retro.Phase = phase.Done
	putIssue(t, pool, retro)
	parked := issue(t, pool, "LEGION-4")
	parked.Status, parked.DispatchStatus = "backlog", "backlog"
	putIssue(t, pool, parked)
	closed := issue(t, pool, "LEGION-20")
	lingers := fixedNow.Add(time.Hour)
	closed.Phase, closed.Status, closed.DispatchStatus, closed.LingerUntil = phase.Done, "done", "done", &lingers
	putIssue(t, pool, closed)

	pass("the second reading", "rows-after-the-three-deletions")

	assertRowHolders(t, pool, "LEGION-1", "LEGION-2", "LEGION-3", "LEGION-5", "LEGION-6", "LEGION-8", "LEGION-9", "LEGION-30")
}

// A2. The cap counts issues, so one tree holds one row for each issue the daemon has started:
// its root's own, taken at its admission and kept through accepting while its children work,
// and one more for each child as its first phase starts.
//
// The root's row is what then lets its own work begin under a cap the tree has filled: an issue
// holding a row starts its next phase whatever admission_cap says, so the root's implementing
// starts on the row it has held all along and the tree still holds four.
func TestATreeHoldsOneRowForEachIssueTheDaemonHasStarted(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)
	slotTree(t, pool, true)
	root := issue(t, pool, "LEGION-1")
	root.Phase, root.Status = phase.Accepting, "needs_review"
	putIssue(t, pool, root)
	takeSlot(t, pool, "LEGION-1")
	children := []string{"LEGION-2", "LEGION-3", "LEGION-4"}
	for i, child := range children {
		parent := "LEGION-1"
		putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
			Title: child, Tree: "LEGION-1", Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo",
			Rank: string(rune('B' + i)), LastDispatchSeq: 2})
	}

	applyFact(t, pool, admission, "released", intake.ControllerTick{})

	for _, child := range children {
		if got := issue(t, pool, child); got.Phase != phase.Planning {
			t.Fatalf("%s = %#v, want it planning on a row of its own", child, got)
		}
	}
	assertRowHolders(t, pool, "LEGION-1", "LEGION-2", "LEGION-3", "LEGION-4")
	rootRow := slots(t, pool)[0]
	if rootRow.Issue != "LEGION-1" {
		t.Fatalf("the first row = %#v, want the root's, taken at its admission before its children were started", rootRow)
	}

	// The root's own first phase begins with the cap full. It holds a row already, so it takes
	// no second one and the cap holds its start back for nothing.
	root = issue(t, pool, "LEGION-1")
	root.Phase, root.Status = phase.Implementing, "in_progress"
	putIssue(t, pool, root)
	oweStart(t, pool, "LEGION-1", phase.Implementing)

	applyFact(t, pool, admission, "root-implementing", intake.ControllerTick{})

	assertRowHolders(t, pool, "LEGION-1", "LEGION-2", "LEGION-3", "LEGION-4")
	if got := slots(t, pool)[0]; got != rootRow {
		t.Fatalf("the root's row = %#v, want the same row it held in accepting, %#v", got, rootRow)
	}
	delivered := false
	for _, start := range startsOf(t, pool, "LEGION-1") {
		if start.task != "" && start.claimable {
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("the root's starts = %#v, want its implementer's task claimable on the row the root already held",
			startsOf(t, pool, "LEGION-1"))
	}
}
