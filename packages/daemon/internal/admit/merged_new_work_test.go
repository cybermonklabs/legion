package admit

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The tests of new work on a closed issue whose work merged (D17 of the done-by-checks plan, Task
// 13) at admission's doors: a request starts one next change of the issue where its work merged,
// waits for the merge read where its record shows no merge, and runs as today only where a person
// confirmed that its work never merged or it was parked and never closed. Each applies a Dispatch
// fact through the workflow's handler and admission's, as the daemon does.

const (
	// mnwRoot is the tree root of every case, and mnwChild its child, the issue of the child
	// placements and of the orphan.
	mnwRoot  = "LEGION-700"
	mnwChild = "LEGION-701"
	// mnwMember is another member of mnwRoot's tree, mid-phase where a case gives the tree member
	// work, and mnwLeaf a member of the tree mnwChild roots once it is a root of its own.
	mnwMember = "LEGION-702"
	mnwLeaf   = "LEGION-703"
	// mnwSeq is the Dispatch sequence every seeded record stands at; a request comes after it.
	mnwSeq = 5
)

// mnwMerged is the merge every merged case's record holds: pull request #11 into main.
var mnwMerged = record.Merge{Number: 11, Head: "head-11", SHA: "merge-11", Base: "main", Repo: "acme/widgets", MergedAt: fixedNow.Add(-24 * time.Hour)}

// mnwFrom is the branch origin each next change of mnwMerged carries (record.IssueBranch.From).
var mnwFrom = record.BranchFrom{Base: "main", MergedHead: "head-11"}

// mnwIssue is key's record standing at at with status, in tree under parent, at generation, on
// acme/widgets at main, handed to Legion, at Dispatch's sequence mnwSeq.
func mnwIssue(key, tree string, parent *string, at phase.Phase, status string, generation uint64) record.Issue {
	return record.Issue{Key: key, Tree: tree, Parent: parent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: key, Phase: at, Generation: generation, TreeGeneration: generation, Status: status, DispatchStatus: status,
		Rank: "B", HandedOver: true, LastDispatchSeq: mnwSeq}
}

// mnwSnapshot records mnwMerged as key's merge snapshot (ANSWERED).
func mnwSnapshot(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := record.NewStore().PutMergeSnapshot(context.Background(), tx, key, mnwMerged); err != nil {
			t.Fatalf("record the merge of %s: %v", key, err)
		}
	})
}

// mnwGate records key's spec approved at version 3, its gate opened.
func mnwGate(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	seedGate(t, pool, record.DesignGate{Issue: key, ArtifactID: "spec-" + key, LatestVersion: 3, ApprovedVersion: new(3), EverOpened: true})
}

// mnwGates is every gate row key holds, by step.
func mnwGates(t *testing.T, pool *pgxpool.Pool, key string) map[record.DesignStep]record.DesignGate {
	t.Helper()
	var gates map[record.DesignStep]record.DesignGate
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if gates, err = record.NewStore().GatesOf(context.Background(), tx, key); err != nil {
			t.Fatalf("read the gates of %s: %v", key, err)
		}
	})
	return gates
}

// mnwGateIdentity is what a gate row names of the design: its document and the version a person
// approved, for each step. The carry's copy writes a row of its own issue, so a placement that
// carries the gate compares these.
func mnwGateIdentity(gates map[record.DesignStep]record.DesignGate) map[record.DesignStep]string {
	identity := map[record.DesignStep]string{}
	for step, gate := range gates {
		approved := "none"
		if gate.ApprovedVersion != nil {
			approved = fmt.Sprint(*gate.ApprovedVersion)
		}
		identity[step] = gate.ArtifactID + "@" + approved
	}
	return identity
}

// mnwOutcome is the earlier production check a case's issue holds: none, an open one, or one that
// closed with an outcome.
type mnwOutcome struct {
	name    string
	none    bool
	outcome record.CheckOutcome
}

// mnwOutcomes are the five earlier production checks of D17's table: N, S, and K three times.
var mnwOutcomes = []mnwOutcome{{name: "no earlier check", none: true}, {name: "an open earlier check"},
	{name: "an earlier check recorded", outcome: record.CheckRecorded}, {name: "an earlier check not run", outcome: record.CheckNotRun},
	{name: "an earlier check that found a defect", outcome: record.CheckDefect}}

// seed records the earlier production check o of key in tree at generation.
func (o mnwOutcome) seed(t *testing.T, pool *pgxpool.Pool, key, tree string, generation uint64) {
	t.Helper()
	if o.none {
		return
	}
	opened := fixedNow.Add(-20 * time.Hour).Truncate(time.Microsecond)
	check := record.ProductionCheck{Issue: key, Project: testProject, Tree: tree, Repo: "acme/widgets", Number: mnwMerged.Number, MergeSHA: mnwMerged.SHA,
		OpenedAt: opened, BoundAt: opened.Add(48 * time.Hour), Role: claim.RoleTester, Looks: 1, Generation: generation}
	if o.outcome != "" {
		closed := opened.Add(time.Hour)
		check.ClosedAt, check.Outcome = &closed, o.outcome
	} else {
		check.LookOpen = true
	}
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutProductionCheck(context.Background(), tx, check); err != nil {
			t.Fatalf("record the production check of %s: %v", key, err)
		}
	})
}

// holds fails the test unless key's production check after its next change is what o leaves: none
// for none, the open check closed not_run (D6), and a closed one as it closed.
func (o mnwOutcome) holds(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	var check *record.ProductionCheck
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if check, err = record.NewStore().ProductionCheck(context.Background(), tx, key); err != nil {
			t.Fatalf("read the production check of %s: %v", key, err)
		}
	})
	switch {
	case o.none && check != nil:
		t.Fatalf("the production check of %s = %+v, want none", key, check)
	case o.none:
	case check == nil:
		t.Fatalf("the production check of %s is gone, want the earlier one", key)
	case o.outcome == "" && (check.Open() || check.Outcome != record.CheckNotRun):
		t.Fatalf("the earlier open check of %s = %+v, want it closed not_run", key, check)
	case o.outcome != "" && check.Outcome != o.outcome:
		t.Fatalf("the earlier check of %s = %+v, want it closed %s as it was", key, check, o.outcome)
	}
}

// mnwRequest is one request row (kind request) as these tests read it.
type mnwRequest struct {
	id        int64
	role      claim.Role
	carriedBy uint64
}

// mnwRequests is every request row of key, oldest first.
func mnwRequests(t *testing.T, pool *pgxpool.Pool, key string) []mnwRequest {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, role, coalesce(carried_by, 0) from defects
		where issue = $1 and kind = 'request' order by id`, key)
	if err != nil {
		t.Fatalf("read the requests of %s: %v", key, err)
	}
	defer rows.Close()
	var got []mnwRequest
	for rows.Next() {
		var row mnwRequest
		var role string
		var carried int64
		if err := rows.Scan(&row.id, &role, &carried); err != nil {
			t.Fatalf("scan a request of %s: %v", key, err)
		}
		row.role, row.carriedBy = claim.Role(role), uint64(carried)
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the requests of %s: %v", key, err)
	}
	return got
}

// mnwPending is key's standing merge_read row, and whether one stands; more than one fails.
func mnwPending(t *testing.T, pool *pgxpool.Pool, key string) (record.MergeRead, bool) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload from outbox where issue = $1 and kind = 'merge_read' order by id`, key)
	if err != nil {
		t.Fatalf("read the merge reads of %s: %v", key, err)
	}
	defer rows.Close()
	var reads []record.MergeRead
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var read record.MergeRead
		if err := json.Unmarshal(payload, &read); err != nil {
			t.Fatalf("decode a merge read of %s: %v", key, err)
		}
		reads = append(reads, read)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	switch len(reads) {
	case 0:
		return record.MergeRead{}, false
	case 1:
		return reads[0], true
	}
	t.Fatalf("%s holds %d merge_read rows, want at most one", key, len(reads))
	return record.MergeRead{}, false
}

// mnwCount is the number of outbox rows of kind on key whose payload's kind is notice.
func mnwCount(t *testing.T, pool *pgxpool.Pool, key, kind, notice string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = $2 and payload->>'kind' = $3`,
		key, kind, notice).Scan(&count); err != nil {
		t.Fatalf("count %s's %s rows of %s: %v", key, kind, notice, err)
	}
	return count
}

// mnwBranches is every issue_branch row of key, oldest first.
func mnwBranches(t *testing.T, pool *pgxpool.Pool, key string) []record.IssueBranch {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload from outbox where issue = $1 and kind = 'issue_branch' order by id`, key)
	if err != nil {
		t.Fatalf("read the branch rows of %s: %v", key, err)
	}
	defer rows.Close()
	var got []record.IssueBranch
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var branch record.IssueBranch
		if err := json.Unmarshal(payload, &branch); err != nil {
			t.Fatalf("decode a branch row of %s: %v", key, err)
		}
		got = append(got, branch)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// mnwState is key's record, with the moment it last moved taken off, and the state of each claim of
// it, as one string: what a request must leave as it stands on another member.
func mnwState(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	got := maybeIssue(t, pool, key)
	if got == nil {
		return key + " unrecorded"
	}
	rows, err := pool.Query(context.Background(), `select token, state from claims where issue = $1 order by token`, key)
	if err != nil {
		t.Fatalf("read the claims of %s: %v", key, err)
	}
	defer rows.Close()
	var claims []string
	for rows.Next() {
		var token, state string
		if err := rows.Scan(&token, &state); err != nil {
			t.Fatal(err)
		}
		claims = append(claims, token+"="+state)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	parent := deref(got.Parent)
	got.Parent = nil
	return fmt.Sprintf("%+v under %q %v", withoutMovement(*got), parent, claims)
}

// mnwMove is key's move to status at seq under parent ("" for a root), as who made it: "person"
// is a Dispatch user's event, "operator" the operator's legion issue status (a route's own write
// through its grant), "controller" and "architect" a route's own write, "unknown" an event that
// names no actor, and "session" an event a session wrote. The move carries the label.
func mnwMove(key, parent, status string, seq int64, who string) intake.DispatchIssue {
	fact := intake.DispatchIssue{Key: key, Seq: seq, Type: "issue.updated", Status: status, Title: key, Parent: parent, Rank: "B",
		HandedOver: true, ResolvedRepo: "acme/widgets"}
	if status == "done" {
		fact.Type = "issue.closed"
	}
	switch who {
	case "person":
		fact.PersonActor = "a-person"
	case "operator":
		fact.DaemonActor, fact.StatusMove, fact.Requester, fact.PersonActor = true, true, record.RequesterPerson, "operator"
	case "controller":
		fact.DaemonActor, fact.StatusMove, fact.Requester = true, true, claim.RoleController
	case "architect":
		fact.DaemonActor, fact.StatusMove, fact.Requester = true, true, claim.RoleArchitect
	case "session":
		fact.ActorSession = "ses-elsewhere"
	}
	return fact
}

// mnwMoves applies each move in order, each under an event id of its own.
func mnwMoves(t *testing.T, pool *pgxpool.Pool, admission *Admission, moves ...intake.DispatchIssue) {
	t.Helper()
	for _, move := range moves {
		apply(t, pool, admission, fmt.Sprintf("mnw-%s-%d-%s", move.Key, move.Seq, move.Status), move, admission.engine)
	}
}

// mnwListed applies summary as the boot listing does once the consumer has caught up with it, so
// the listing's move is taken and not held behind the stream.
func mnwListed(t *testing.T, pool *pgxpool.Pool, admission *Admission, summary dispatch.IssueSummary) {
	t.Helper()
	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{summary}, summary.LastSeq, summary.LastSeq, true)
}

// mnwStarted fails the test unless key's one next change started at generation, waiting at
// admitted or promoted into its first phase, in tree under parent, carrying one request of role, with the branch
// origin of mnwMerged. It answers the record.
func mnwStarted(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, tree string, parent *string, role claim.Role) record.Issue {
	t.Helper()
	got := issue(t, pool, key)
	if got.Generation != generation || got.NextChange != generation || got.NextChanges != 1 {
		t.Fatalf("%s stands at generation %d in %s with status %s, change %d of %d; want its next change %d, the first",
			key, got.Generation, got.Phase, got.Status, got.NextChange, got.NextChanges, generation)
	}
	// The change waits at admitted, or a free slot has already promoted it into its first phase.
	if got.Phase == phase.Done || got.Phase == phase.Verifying {
		t.Fatalf("%s's next change %d stands in %s, want it at admitted or in its first phase", key, generation, got.Phase)
	}
	if got.Tree != tree || !sameParent(got.Parent, parent) {
		t.Fatalf("%s's next change runs in tree %s under %v, want tree %s under %v", key, got.Tree, deref(got.Parent), tree, deref(parent))
	}
	if from := record.IssueBranchOf(got).From; from == nil || *from != mnwFrom {
		t.Fatalf("%s's branch row would start from %+v, want %+v", key, from, mnwFrom)
	}
	requests := mnwRequests(t, pool, key)
	if len(requests) != 1 || requests[0].carriedBy != generation || requests[0].role != role {
		t.Fatalf("the requests of %s = %+v, want one of %s carried by generation %d", key, requests, role, generation)
	}
	if _, standing := mnwPending(t, pool, key); standing {
		t.Fatalf("%s holds a merge read, want none: its record holds its merge", key)
	}
	return got
}

// mnwWaits fails the test unless the request on key waits for its merge read: the record stands at
// generation at done with the status todo and no change, and one merge_read row of that generation
// stands, asked by requester.
func mnwWaits(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, requester claim.Role) record.MergeRead {
	t.Helper()
	got := issue(t, pool, key)
	if got.Generation != generation || got.Phase != phase.Done || got.Status != "todo" || got.NextChange != 0 {
		t.Fatalf("%s stands at generation %d in %s with status %s and change %d, want generation %d at done with todo and no change",
			key, got.Generation, got.Phase, got.Status, got.NextChange, generation)
	}
	read, standing := mnwPending(t, pool, key)
	if !standing || read.Generation != generation || read.Requester != requester {
		t.Fatalf("%s's merge read = %+v (standing %t), want one of generation %d asked by %s", key, read, standing, generation, requester)
	}
	if requests := mnwRequests(t, pool, key); len(requests) != 0 {
		t.Fatalf("the requests of %s = %+v, want none before its merge is read", key, requests)
	}
	return read
}

// mnwRerun fails the test unless key ran again as today, at generation: no next change, no request
// and no merge read.
func mnwRerun(t *testing.T, pool *pgxpool.Pool, key string, generation uint64) record.Issue {
	t.Helper()
	got := issue(t, pool, key)
	if got.Generation != generation || got.NextChange != 0 || got.Phase != phase.Admitted {
		t.Fatalf("%s stands at generation %d in %s with change %d, want today's rerun at generation %d at admitted and no next change",
			key, got.Generation, got.Phase, got.NextChange, generation)
	}
	if requests := mnwRequests(t, pool, key); len(requests) != 0 {
		t.Fatalf("the requests of %s = %+v, want none on a rerun", key, requests)
	}
	if _, standing := mnwPending(t, pool, key); standing {
		t.Fatalf("%s holds a merge read, want none on a rerun", key)
	}
	return got
}

// mnwPlacement is one placement of D17's table: what it seeds, the issue the request is on, the
// door it reaches, and where the next change runs.
type mnwPlacement struct {
	name string
	// subject is the issue the request is on, at generation.
	subject    string
	generation uint64
	// seed records the placement's issues, with other member work where members is set.
	seed func(t *testing.T, pool *pgxpool.Pool, members bool)
	// move is the request's todo.
	move intake.DispatchIssue
	// tree and parent are where the next change runs, and gateFrom the issue whose gate rows the
	// subject's identity compares with: its own, or the old root's a carry copies.
	tree, gateFrom string
	parent         *string
	// root is the tree root whose tree generation the request must leave as it stood, and others
	// the members whose records and claims it must leave.
	root   string
	others []string
	// ownTree says the subject becomes a tree of its own, whose run starts at its generation.
	ownTree bool
}

// mnwClosedRoot records mnwRoot closed at generation 1 with its merge and its approved gate. With
// lingers set its linger runs and its tree's lifecycle stands open; without it the tree's cleanup
// was reserved after the linger ended. Its architect's claim stands suspended. With members set
// mnwMember stands mid-phase in implementing.
func mnwClosedRoot(t *testing.T, pool *pgxpool.Pool, lingers, members bool) {
	t.Helper()
	root := mnwIssue(mnwRoot, mnwRoot, nil, phase.Done, "done", 1)
	if lingers {
		root.LingerUntil = new(fixedNow.Add(30 * time.Minute))
	}
	putIssue(t, pool, root)
	mnwSnapshot(t, pool, mnwRoot)
	mnwGate(t, pool, mnwRoot)
	seedLifecycle(t, pool, mnwRoot)
	if !lingers {
		inTx(t, pool, func(tx pgx.Tx) {
			reserved, err := treelifecycle.ReserveCleanup(context.Background(), tx, "legion", mnwRoot, treelifecycle.AuthorityWorkflow)
			if err != nil {
				t.Fatalf("reserve the cleanup of %s's tree: %v", mnwRoot, err)
			}
			if err := treelifecycle.ConfirmCleanup(context.Background(), tx, "legion", mnwRoot, reserved.Epoch); err != nil {
				t.Fatalf("confirm the cleanup of %s's tree: %v", mnwRoot, err)
			}
		})
	}
	putClaimRow(t, pool, mnwRoot, mnwRoot, claim.RoleArchitect, 1, "suspended")
	if members {
		midPhaseChild(t, pool, mnwRoot, mnwMember, phase.Implementing, "in_progress")
	}
}

// mnwPlacements are the five placements of D17's table.
func mnwPlacements() []mnwPlacement {
	rootTodo := mnwMove(mnwRoot, "", "todo", mnwSeq+1, "person")
	childTodo := mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+1, "person")
	return []mnwPlacement{
		{
			name: "root, its linger still running", subject: mnwRoot, generation: 1, move: rootTodo,
			seed: func(t *testing.T, pool *pgxpool.Pool, members bool) { mnwClosedRoot(t, pool, true, members) },
			tree: mnwRoot, gateFrom: mnwRoot, root: mnwRoot, others: []string{mnwMember},
		},
		{
			name: "root, its runtime tree gone", subject: mnwRoot, generation: 1, move: rootTodo,
			seed: func(t *testing.T, pool *pgxpool.Pool, members bool) { mnwClosedRoot(t, pool, false, members) },
			tree: mnwRoot, gateFrom: mnwRoot, root: mnwRoot, others: []string{mnwMember},
		},
		{
			name: "child, its tree live", subject: mnwChild, generation: 1, move: childTodo,
			seed: func(t *testing.T, pool *pgxpool.Pool, members bool) {
				seedSlottedInPhase(t, pool, testProject, mnwRoot, "A", phase.Implementing, func(root *record.Issue) { root.LastDispatchSeq = mnwSeq })
				mnwGate(t, pool, mnwRoot)
				putIssue(t, pool, mnwIssue(mnwChild, mnwRoot, new(mnwRoot), phase.Done, "done", 1))
				mnwSnapshot(t, pool, mnwChild)
				mnwGate(t, pool, mnwChild)
				if members {
					midPhaseChild(t, pool, mnwRoot, mnwMember, phase.Implementing, "in_progress")
				}
			},
			tree: mnwRoot, gateFrom: mnwChild, parent: new(mnwRoot), root: mnwRoot, others: []string{mnwRoot, mnwMember},
		},
		{
			name: "labelled child, its root in backlog", subject: mnwChild, generation: 1, move: childTodo,
			seed: func(t *testing.T, pool *pgxpool.Pool, members bool) {
				root := mnwIssue(mnwRoot, mnwRoot, nil, phase.Done, "backlog", 1)
				root.LingerUntil = new(fixedNow.Add(30 * time.Minute))
				putIssue(t, pool, root)
				mnwGate(t, pool, mnwRoot)
				seedLifecycle(t, pool, mnwRoot)
				child := mnwIssue(mnwChild, mnwRoot, new(mnwRoot), phase.Done, "done", 1)
				child.HandedOver = false
				putIssue(t, pool, child)
				mnwSnapshot(t, pool, mnwChild)
				if members {
					midPhaseChild(t, pool, mnwRoot, mnwMember, phase.Implementing, "in_progress")
				}
			},
			tree: mnwChild, gateFrom: mnwRoot, parent: new(mnwRoot), root: mnwRoot, others: []string{mnwRoot, mnwMember}, ownTree: true,
		},
		{
			name: "child handed over, a root of its own", subject: mnwChild, generation: 2,
			move: mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+1, "person"),
			seed: func(t *testing.T, pool *pgxpool.Pool, members bool) {
				putIssue(t, pool, mnwIssue(mnwRoot, mnwRoot, nil, phase.Done, "done", 1))
				mnwGate(t, pool, mnwRoot)
				handed := mnwIssue(mnwChild, mnwChild, new(mnwRoot), phase.Done, "done", 2)
				handed.LingerUntil = new(fixedNow.Add(30 * time.Minute))
				putIssue(t, pool, handed)
				mnwSnapshot(t, pool, mnwChild)
				mnwGate(t, pool, mnwChild)
				seedLifecycle(t, pool, mnwChild)
				putClaimRow(t, pool, mnwChild, mnwChild, claim.RoleArchitect, 2, "suspended")
				if members {
					midPhaseChild(t, pool, mnwChild, mnwLeaf, phase.Implementing, "in_progress")
				}
			},
			tree: mnwChild, gateFrom: mnwChild, parent: new(mnwRoot), root: mnwChild, others: []string{mnwRoot, mnwLeaf},
		},
	}
}

// Each placement of D17's table, with and without other member work, under each earlier production
// check: the request raises the issue's own generation once, starts one next change carrying one
// request, and keeps the placement, the gate's identity, the approved version, the branch and its
// origin. Other members' work and the tree's run stand as they stood, and the owning architect is
// told (next-change). The earlier open check closes not_run, and a closed one stands as it closed.
// Seven more rows: a root and a child in verifying take one change each, a request read twice starts
// one, and so does an architect's rerun_child with its echo. The boot listing that reads a closed
// root's todo first starts its change through the listing's own door. A person moves a never-merged
// root and a never-merged child through backlog, then todo, and each runs as today.
func TestMergedNewWorkUsesOneContinuationFromEveryDoor(t *testing.T) {
	for _, place := range mnwPlacements() {
		for _, members := range []bool{false, true} {
			for _, outcome := range mnwOutcomes {
				name := fmt.Sprintf("%s, member work %t, %s", place.name, members, outcome.name)
				t.Run(name, func(t *testing.T) {
					pool := migratedPool(t)
					admission := quiet(t, 4)
					place.seed(t, pool, members)
					outcome.seed(t, pool, place.subject, issue(t, pool, place.subject).Tree, place.generation)
					gates := mnwGateIdentity(mnwGates(t, pool, place.gateFrom))
					treeRun := issue(t, pool, place.root).TreeRun()
					others := map[string]string{}
					for _, key := range place.others {
						others[key] = mnwState(t, pool, key)
					}

					mnwMoves(t, pool, admission, place.move)

					got := mnwStarted(t, pool, place.subject, place.generation+1, place.tree, place.parent, record.RequesterPerson)
					if after := mnwGateIdentity(mnwGates(t, pool, place.subject)); !reflect.DeepEqual(after, gates) {
						t.Fatalf("%s's gate after the request = %v, want the approved design %v", place.subject, after, gates)
					}
					if place.ownTree && got.TreeRun() != got.Generation {
						t.Fatalf("%s's tree run = %d, want its own generation %d: it roots a tree of its own", place.subject, got.TreeRun(), got.Generation)
					}
					if root := issue(t, pool, place.root); root.TreeRun() != treeRun {
						t.Fatalf("the tree run of %s after the request = %d, want %d", place.root, root.TreeRun(), treeRun)
					}
					if got.Lingers() {
						t.Fatalf("%s lingers after the request, want its linger ended", place.subject)
					}
					for key, before := range others {
						if after := mnwState(t, pool, key); after != before {
							t.Fatalf("%s after the request = %s, want it as it stood: %s", key, after, before)
						}
					}
					if told := mnwCount(t, pool, place.subject, "notice", "next-change"); told != 1 {
						t.Fatalf("next-change notices of %s = %d, want one", place.subject, told)
					}
					outcome.holds(t, pool, place.subject)
				})
			}
		}
	}

	t.Run("a root in verifying", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		root := mnwIssue(mnwRoot, mnwRoot, nil, phase.Verifying, "testing", 1)
		putIssue(t, pool, root)
		mnwSnapshot(t, pool, mnwRoot)
		mnwGate(t, pool, mnwRoot)
		seedLifecycle(t, pool, mnwRoot)
		mnwMoves(t, pool, admission, mnwMove(mnwRoot, "", "todo", mnwSeq+1, "controller"))
		mnwStarted(t, pool, mnwRoot, 2, mnwRoot, nil, claim.RoleController)
	})

	t.Run("a child in verifying", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		seedSlottedInPhase(t, pool, testProject, mnwRoot, "A", phase.Implementing, func(root *record.Issue) { root.LastDispatchSeq = mnwSeq })
		putIssue(t, pool, mnwIssue(mnwChild, mnwRoot, new(mnwRoot), phase.Verifying, "testing", 1))
		mnwSnapshot(t, pool, mnwChild)
		mnwGate(t, pool, mnwChild)
		mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+1, "controller"))
		mnwStarted(t, pool, mnwChild, 2, mnwRoot, new(mnwRoot), claim.RoleController)
	})

	t.Run("a request read twice starts one change", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		mnwClosedRoot(t, pool, true, false)
		move := mnwMove(mnwRoot, "", "todo", mnwSeq+1, "person")
		apply(t, pool, admission, "mnw-twice-1", move, admission.engine)
		apply(t, pool, admission, "mnw-twice-2", move, admission.engine)
		mnwListed(t, pool, admission, dispatch.IssueSummary{Key: mnwRoot, Title: mnwRoot, Status: "todo", Rank: "B", HandedOver: true,
			LastSeq: mnwSeq + 1, ResolvedRepo: "acme/widgets"})
		mnwStarted(t, pool, mnwRoot, 2, mnwRoot, nil, record.RequesterPerson)
	})

	// The listing's own door (applyListed): the first read of the root's todo is the boot listing's,
	// so no event's door has placed the root before it. The rows above read the listing only after
	// an event started the change, when the root no longer stands where a door takes it.
	t.Run("the boot listing alone shows a closed root's todo", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		mnwClosedRoot(t, pool, true, false)
		mnwListed(t, pool, admission, dispatch.IssueSummary{Key: mnwRoot, Title: mnwRoot, Status: "todo", Rank: "B", HandedOver: true,
			LastSeq: mnwSeq + 1, ResolvedRepo: "acme/widgets"})
		mnwStarted(t, pool, mnwRoot, 2, mnwRoot, nil, record.RequesterPerson)
	})

	t.Run("an architect's rerun_child with its echo starts one change", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		seedSlottedInPhase(t, pool, testProject, mnwRoot, "A", phase.Implementing, func(root *record.Issue) { root.LastDispatchSeq = mnwSeq })
		putIssue(t, pool, mnwIssue(mnwChild, mnwRoot, new(mnwRoot), phase.Done, "done", 1))
		mnwSnapshot(t, pool, mnwChild)
		mnwGate(t, pool, mnwChild)
		rerun := mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+1, "architect")
		echo := rerun
		echo.StatusMove, echo.Requester = false, ""
		mnwMoves(t, pool, admission, rerun)
		apply(t, pool, admission, "mnw-rerun-echo", echo, admission.engine)
		mnwStarted(t, pool, mnwChild, 2, mnwRoot, new(mnwRoot), claim.RoleArchitect)
	})

	t.Run("a person moves a never-merged root through backlog, then todo", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		putIssue(t, pool, mnwIssue(mnwRoot, mnwRoot, nil, phase.Done, "done", 1))
		mnwGate(t, pool, mnwRoot)
		seedLifecycle(t, pool, mnwRoot)
		mnwMoves(t, pool, admission, mnwMove(mnwRoot, "", "backlog", mnwSeq+1, "person"), mnwMove(mnwRoot, "", "todo", mnwSeq+2, "person"))
		got := mnwRerun(t, pool, mnwRoot, 2)
		if got.TreeRun() != 2 {
			t.Fatalf("the tree run of the rerun = %d, want 2: a whole-tree rerun raises it", got.TreeRun())
		}
		if gates := mnwGates(t, pool, mnwRoot); len(gates) != 0 {
			t.Fatalf("the gate after the rerun = %+v, want it cleared with the tree's generation", gates)
		}
	})

	t.Run("a person moves a never-merged child through backlog, then todo", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		seedSlottedInPhase(t, pool, testProject, mnwRoot, "A", phase.Implementing, func(root *record.Issue) { root.LastDispatchSeq = mnwSeq })
		putIssue(t, pool, mnwIssue(mnwChild, mnwRoot, new(mnwRoot), phase.Done, "done", 1))
		mnwGate(t, pool, mnwChild)
		mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "backlog", mnwSeq+1, "person"), mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+2, "person"))
		mnwRerun(t, pool, mnwChild, 2)
		if gates := mnwGates(t, pool, mnwChild); len(gates) != 0 {
			t.Fatalf("the child's gate after the rerun = %+v, want it cleared with its generation", gates)
		}
	})
}

// mnwClosedChild records a closed child of a running tree at generation 1 with its approved gate
// and no merge snapshot. With partial set it keeps a retained pull request row that never merged
// (PARTIAL); without it the record holds no row that bears on its merge (NONE).
func mnwClosedChild(t *testing.T, pool *pgxpool.Pool, partial bool) {
	t.Helper()
	seedSlottedInPhase(t, pool, testProject, mnwRoot, "A", phase.Implementing, func(root *record.Issue) { root.LastDispatchSeq = mnwSeq })
	putIssue(t, pool, mnwIssue(mnwChild, mnwRoot, new(mnwRoot), phase.Done, "done", 1))
	mnwGate(t, pool, mnwChild)
	if partial {
		inTx(t, pool, func(tx pgx.Tx) {
			if err := record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{Issue: mnwChild, Repo: "acme/widgets", Number: 12,
				Branch: claim.IssueBranch(mnwChild), Base: "main", HeadSHA: "head-12", State: record.PullRequestClosed, Failing: []string{}}); err != nil {
				t.Fatalf("record the closed pull request of %s: %v", mnwChild, err)
			}
		})
	}
}

// mnwClass is key's merge history class as the record derives it.
func mnwClass(t *testing.T, pool *pgxpool.Pool, key string) record.MergeHistoryClass {
	t.Helper()
	var class record.MergeHistoryClass
	inTx(t, pool, func(tx pgx.Tx) {
		row, err := record.NewStore().MergeHistoryOf(context.Background(), tx, key)
		if err != nil {
			t.Fatalf("read the merge history of %s: %v", key, err)
		}
		class = row.Class
	})
	return class
}

// mnwUnread is the merge read's report that its search of key's merge ended with no answering
// candidate (intake.MergeUnread), as the outbox's executor posts it, under an event id of its own
// for each attempt, so a second report is a second fact and not a redelivery of the first.
func mnwUnread(t *testing.T, pool *pgxpool.Pool, admission *Admission, key string, generation uint64, attempt int) {
	t.Helper()
	applyFact(t, pool, admission, fmt.Sprintf("mnw-unread-%s-%d", key, attempt), intake.MergeUnread{Issue: key, Generation: generation, Cause: "no candidate answered"})
}

// A closed root and a closed child at generation 1, on the repository Dispatch resolves them to,
// with no rename configured and no row that bears on their merge: their class is NONE. A todo on
// each queues the merge read and starts nothing: no generation rises, nothing of the tree clears,
// and the gate stands. A read that ends with no answering candidate tells the controller
// (merge-unread), once. A closed root whose design gate never opened, lingering or not, waits the
// same way with a slot free: the waiting line offers no root a slot while its request waits for
// its merge read, so no promotion opens its tree at done.
func TestAnIssueWithNoEvidenceWaitsUnread(t *testing.T) {
	t.Run("a root", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		putIssue(t, pool, mnwIssue(mnwRoot, mnwRoot, nil, phase.Done, "done", 1))
		mnwGate(t, pool, mnwRoot)
		seedLifecycle(t, pool, mnwRoot)
		midPhaseChild(t, pool, mnwRoot, mnwMember, phase.Implementing, "in_progress")
		if class := mnwClass(t, pool, mnwRoot); class != record.MergeHistoryNone {
			t.Fatalf("the class of %s = %s, want NONE", mnwRoot, class)
		}
		gates, member := mnwGates(t, pool, mnwRoot), mnwState(t, pool, mnwMember)
		mnwMoves(t, pool, admission, mnwMove(mnwRoot, "", "todo", mnwSeq+1, "controller"))
		mnwWaits(t, pool, mnwRoot, 1, claim.RoleController)
		if after := mnwGates(t, pool, mnwRoot); !reflect.DeepEqual(after, gates) {
			t.Fatalf("the gate of %s while it waits = %+v, want %+v", mnwRoot, after, gates)
		}
		if after := mnwState(t, pool, mnwMember); after != member {
			t.Fatalf("%s while its root waits = %s, want %s", mnwMember, after, member)
		}
		mnwUnread(t, pool, admission, mnwRoot, 1, 1)
		if told := mnwCount(t, pool, mnwRoot, "controller_notice", "merge-unread"); told != 1 {
			t.Fatalf("merge-unread notices of %s = %d, want one", mnwRoot, told)
		}
	})

	for _, lingers := range []bool{true, false} {
		t.Run(fmt.Sprintf("a root whose gate never opened, lingering %t, with a slot free", lingers), func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 4)
			root := mnwIssue(mnwRoot, mnwRoot, nil, phase.Done, "done", 1)
			if lingers {
				root.LingerUntil = new(fixedNow.Add(30 * time.Minute))
			}
			putIssue(t, pool, root)
			mnwMoves(t, pool, admission, mnwMove(mnwRoot, "", "todo", mnwSeq+1, "person"))
			mnwWaits(t, pool, mnwRoot, 1, record.RequesterPerson)
			assertSlots(t, pool, nil)
			if starts := architectStarts(t, pool, mnwRoot); starts != 0 {
				t.Fatalf("architect starts of %s = %d, want none: nothing promotes a root whose request waits for its merge read", mnwRoot, starts)
			}
		})
	}

	t.Run("a child", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		mnwClosedChild(t, pool, false)
		if class := mnwClass(t, pool, mnwChild); class != record.MergeHistoryNone {
			t.Fatalf("the class of %s = %s, want NONE", mnwChild, class)
		}
		gates := mnwGates(t, pool, mnwChild)
		mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+1, "controller"))
		mnwWaits(t, pool, mnwChild, 1, claim.RoleController)
		if after := mnwGates(t, pool, mnwChild); !reflect.DeepEqual(after, gates) {
			t.Fatalf("the gate of %s while it waits = %+v, want %+v", mnwChild, after, gates)
		}
		if told := mnwCount(t, pool, mnwChild, "notice", "child-status"); told != 0 {
			t.Fatalf("child-status notices of %s = %d, want none: nothing re-entered it", mnwChild, told)
		}
		mnwUnread(t, pool, admission, mnwChild, 1, 1)
		mnwUnread(t, pool, admission, mnwChild, 1, 2)
		if told := mnwCount(t, pool, mnwChild, "controller_notice", "merge-unread"); told != 1 {
			t.Fatalf("merge-unread notices of %s = %d, want one", mnwChild, told)
		}
	})
}

// Only a person confirms that closed work with no proof of its merge (PARTIAL or NONE) never merged:
// a person's backlog in Dispatch, then a todo, runs the child again as today and takes the
// confirmation. The operator's, the controller's, an architect's, the boot listing's and an event of
// no actor cannot: their backlog, then todo, waits for the merge read, and the record keeps the
// issue closed meanwhile. The operator's grant proves no person, since under `controller: operator`
// the controller's agent holds the operator token too: a request through it is still a person's,
// and its backlog confirms nothing. A rank or label edit at backlog confirms nothing. A confirmation
// allows one rerun: the child closed again waits at its next request. A merge that comes before the
// todo wins, a merge snapshot or a merged pull request row: it starts one next change, and the
// confirmation is gone. The class never moves for a confirmation. A child the controller parked
// while it ran, and which never closed, runs as today.
func TestOnlyAPersonCanConfirmNeverMergedWork(t *testing.T) {
	for _, partial := range []bool{false, true} {
		class := map[bool]record.MergeHistoryClass{false: record.MergeHistoryNone, true: record.MergeHistoryPartial}[partial]
		for _, who := range []string{"person", "operator", "controller", "architect", "boot listing", "unknown"} {
			t.Run(fmt.Sprintf("%s, %s", class, who), func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 4)
				mnwClosedChild(t, pool, partial)
				if got := mnwClass(t, pool, mnwChild); got != class {
					t.Fatalf("the class of %s = %s, want %s", mnwChild, got, class)
				}
				switch who {
				case "boot listing":
					mnwListed(t, pool, admission, dispatch.IssueSummary{Key: mnwChild, Title: mnwChild, Status: "backlog", Parent: new(mnwRoot),
						Rank: "B", HandedOver: true, LastSeq: mnwSeq + 1, ResolvedRepo: "acme/widgets"})
					if got := issue(t, pool, mnwChild); got.Status != "done" {
						t.Fatalf("%s's record status after the listing's backlog = %s, want done: no person confirmed anything", mnwChild, got.Status)
					}
					mnwListed(t, pool, admission, dispatch.IssueSummary{Key: mnwChild, Title: mnwChild, Status: "todo", Parent: new(mnwRoot),
						Rank: "B", HandedOver: true, LastSeq: mnwSeq + 2, ResolvedRepo: "acme/widgets"})
				default:
					mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "backlog", mnwSeq+1, who))
					if got := mnwClass(t, pool, mnwChild); got != class {
						t.Fatalf("the class of %s after the backlog = %s, want %s: a confirmation is no evidence", mnwChild, got, class)
					}
					mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+2, who))
				}
				switch who {
				case "person":
					mnwRerun(t, pool, mnwChild, 2)
				default:
					requester := map[string]claim.Role{"controller": claim.RoleController, "architect": claim.RoleArchitect}[who]
					if requester == "" {
						requester = record.RequesterPerson
					}
					mnwWaits(t, pool, mnwChild, 1, requester)
				}
			})
		}
	}

	t.Run("a rank and a label edit at backlog confirm nothing", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		mnwClosedChild(t, pool, false)
		backlog := mnwMove(mnwChild, mnwRoot, "backlog", mnwSeq+1, "controller")
		rank := mnwMove(mnwChild, mnwRoot, "backlog", mnwSeq+2, "person")
		rank.Rank = "C"
		label := mnwMove(mnwChild, mnwRoot, "backlog", mnwSeq+3, "person")
		label.Rank, label.Labels = "C", []string{"legion", "ops"}
		mnwMoves(t, pool, admission, backlog, rank, label, mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+4, "controller"))
		mnwWaits(t, pool, mnwChild, 1, claim.RoleController)
	})

	// One rerun is held in two places, both kept: the request takes the confirmation
	// (record.Store.ConsumeNeverMergedConfirmation), and every clear of a generation resets it
	// (clearNeverMergedConfirmations). The take is the rule for a request; the reset also covers a
	// clear that no request of the issue made, as a re-admission of its tree. With either gone the
	// other still holds this case, so its control (flag-kept) takes out both.
	t.Run("a confirmation allows one rerun", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		mnwClosedChild(t, pool, false)
		mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "backlog", mnwSeq+1, "person"), mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+2, "person"))
		mnwRerun(t, pool, mnwChild, 2)
		mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "done", mnwSeq+3, "person"), mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+4, "controller"))
		mnwWaits(t, pool, mnwChild, 2, claim.RoleController)
	})

	t.Run("a merge before the todo beats a confirmation", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		mnwClosedChild(t, pool, false)
		mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "backlog", mnwSeq+1, "person"))
		mnwSnapshot(t, pool, mnwChild)
		mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+2, "person"))
		mnwStarted(t, pool, mnwChild, 2, mnwRoot, new(mnwRoot), record.RequesterPerson)
		var confirmed bool
		if err := pool.QueryRow(context.Background(), `select never_merged_confirmed from issues where key = $1`, mnwChild).Scan(&confirmed); err != nil {
			t.Fatal(err)
		}
		if confirmed {
			t.Fatalf("%s's confirmation stands after its merge started a next change, want it cleared", mnwChild)
		}
	})

	// A merge that is a merged pull request row: nothing writes a snapshot before the todo, so
	// nothing takes the confirmation before the record's decision. The snapshot case above is held
	// twice, since PutMergeSnapshot also takes the confirmation; this one is held only by the order of
	// that decision (RouteRequest reads the evidence before the confirmation).
	t.Run("a merged pull request row before the todo beats a confirmation", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		mnwClosedChild(t, pool, false)
		mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "backlog", mnwSeq+1, "person"))
		inTx(t, pool, func(tx pgx.Tx) {
			if err := record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{Issue: mnwChild, Repo: "acme/widgets",
				Number: mnwMerged.Number, Branch: claim.IssueBranch(mnwChild), Base: mnwMerged.Base, HeadSHA: mnwMerged.Head,
				MergeSHA: mnwMerged.SHA, State: record.PullRequestMerged, Failing: []string{}}); err != nil {
				t.Fatalf("record the merged pull request of %s: %v", mnwChild, err)
			}
		})
		if class := mnwClass(t, pool, mnwChild); class != record.MergeHistoryAnswered {
			t.Fatalf("the class of %s with its merged pull request row = %s, want ANSWERED", mnwChild, class)
		}
		mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+2, "person"))
		mnwStarted(t, pool, mnwChild, 2, mnwRoot, new(mnwRoot), record.RequesterPerson)
	})

	t.Run("a child parked while it ran, which never closed, runs as today", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		seedSlottedInPhase(t, pool, testProject, mnwRoot, "A", phase.Implementing, func(root *record.Issue) { root.LastDispatchSeq = mnwSeq })
		midPhaseChild(t, pool, mnwRoot, mnwChild, phase.Implementing, "in_progress")
		mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "backlog", mnwSeq+1, "controller"), mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+2, "controller"))
		mnwRerun(t, pool, mnwChild, 2)
	})
}

// Without positive evidence of its merge, no repository fact decides that a closed issue's work
// never merged: an issue whose GitHub spelling differs from the repository its record and its merge
// history name, one whose record names another repository than Dispatch resolves it to, and one
// whose names match with no rename configured each wait for the merge read. So do a retained pull
// request that never merged (PARTIAL), and an issue whose one cleared clock a rename gave to another
// issue (NONE). Each read that ends with no answering candidate names the issue to the controller
// (merge-unread).
func TestAMergeDecisionRejectsPossibleRenameLoss(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string
		// github is GitHub's spelling of the record's repository where it differs from recorded. The
		// record allows only a difference of case (PutIssue), and the merge history is read under the
		// record's spelling, so GitHub's spelling is not the repository the history names.
		github   string
		resolved string
		seed     func(t *testing.T, pool *pgxpool.Pool)
		class    record.MergeHistoryClass
	}{
		{name: "GitHub's spelling differs from the history's repository", recorded: "acme/widgets", github: "Acme/Widgets", resolved: "acme/widgets", class: record.MergeHistoryNone},
		{name: "a repository other than Dispatch resolves it to", recorded: "acme/tools", resolved: "acme/widgets", class: record.MergeHistoryNone},
		{name: "matching names with no rename configured", recorded: "acme/widgets", resolved: "acme/widgets", class: record.MergeHistoryNone},
		{name: "a retained pull request that never merged", recorded: "acme/widgets", resolved: "acme/widgets", class: record.MergeHistoryPartial,
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				inTx(t, pool, func(tx pgx.Tx) {
					if err := record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{Issue: mnwChild, Repo: "acme/widgets", Number: 12,
						Branch: claim.IssueBranch(mnwChild), Base: "main", HeadSHA: "head-12", State: record.PullRequestClosed, Failing: []string{}}); err != nil {
						t.Fatal(err)
					}
				})
			}},
		{name: "a cleared clock a rename gave to another issue", recorded: "acme/widgets", resolved: "acme/widgets", class: record.MergeHistoryNone,
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				if _, err := pool.Exec(context.Background(), `insert into cleared_pull_requests (repo, number, issue, head_updated_at, cleared_at)
					values ('acme/widgets', 42, $1, now(), now())`, mnwMember); err != nil {
					t.Fatal(err)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 4)
			seedSlottedInPhase(t, pool, testProject, mnwRoot, "A", phase.Implementing, func(root *record.Issue) { root.LastDispatchSeq = mnwSeq })
			child := mnwIssue(mnwChild, mnwRoot, new(mnwRoot), phase.Done, "done", 1)
			child.Repo, child.GitHubRepo = tc.recorded, tc.recorded
			if tc.github != "" {
				child.GitHubRepo = tc.github
			}
			putIssue(t, pool, child)
			mnwGate(t, pool, mnwChild)
			if tc.seed != nil {
				tc.seed(t, pool)
			}
			if class := mnwClass(t, pool, mnwChild); class != tc.class {
				t.Fatalf("the class of %s = %s, want %s", mnwChild, class, tc.class)
			}
			move := mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+1, "controller")
			move.ResolvedRepo = tc.resolved
			mnwMoves(t, pool, admission, move)
			mnwWaits(t, pool, mnwChild, 1, claim.RoleController)
			mnwUnread(t, pool, admission, mnwChild, 1, 1)
			if told := mnwCount(t, pool, mnwChild, "controller_notice", "merge-unread"); told != 1 {
				t.Fatalf("merge-unread notices of %s = %d, want one", mnwChild, told)
			}
		})
	}
}

// A lingering root whose tree holds an unfinished member and whose architect stands mid-task: the
// member's queued idle release, its suspension and its design-news credit, the architect's phase
// entry and its pending task, and a gate request in flight. A request starts the root's next change
// in place of today's re-admission, and each stays valid under its own fence: the tree's run does
// not rise, the architect's entry stands, and no start supersedes its task.
func TestARootContinuationKeepsMemberWorkAndArchitectTask(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)
	mnwClosedRoot(t, pool, true, true)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: mnwRoot, Role: claim.RoleArchitect,
			Claim: architectToken(t, mnwRoot), Entry: 2, EntryTask: "Work the tree."}); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := pool.Exec(context.Background(), `insert into issue_releases
		(project, issue, generation, first_row, boundary_row, last_row, started_row, role, suspended, marked_at)
		values ('legion', $1, 1, 5, 6, 10, 0, 'implementer', '{}', now())`, mnwMember); err != nil {
		t.Fatalf("mark the release of %s: %v", mnwMember, err)
	}
	before := issue(t, pool, mnwRoot)
	member := mnwState(t, pool, mnwMember)

	mnwMoves(t, pool, admission, mnwMove(mnwRoot, "", "todo", mnwSeq+1, "person"))

	root := mnwStarted(t, pool, mnwRoot, 2, mnwRoot, nil, record.RequesterPerson)
	if root.TreeRun() != before.TreeRun() {
		t.Fatalf("the tree run after the request = %d, want %d", root.TreeRun(), before.TreeRun())
	}
	if after := mnwState(t, pool, mnwMember); after != member {
		t.Fatalf("%s after the request = %s, want %s", mnwMember, after, member)
	}
	var marked int
	if err := pool.QueryRow(context.Background(), `select count(*) from issue_releases where issue = $1 and generation = 1`, mnwMember).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	if marked != 1 {
		t.Fatalf("the idle release marks of %s = %d, want its one", mnwMember, marked)
	}
	var row record.PhaseRow
	inTx(t, pool, func(tx pgx.Tx) {
		rows, err := record.NewStore().Phases(context.Background(), tx, mnwRoot)
		if err != nil {
			t.Fatalf("read the phases rows of %s: %v", mnwRoot, err)
		}
		row = record.PhaseRowOf(rows, claim.RoleArchitect)
	})
	// The promotion resumes the standing run and raises no entry: the row still names entry 2.
	if row.Entry != 2 || row.EntryTask == "" {
		t.Fatalf("the architect's row after the request = %+v, want its entry 2 kept", row)
	}
	// No start supersedes the pending task: each architect start that carries a task is a send of
	// entry 2 itself. A send derives its text from the record at the send (SendArchitectStart), so
	// the entry it names is compared, not its text.
	entries, err := pool.Query(context.Background(), `select coalesce((payload->>'entry')::bigint, 0) from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = $2
		and coalesce(payload->>'task', '') <> '' order by id`, mnwRoot, string(claim.RoleArchitect))
	if err != nil {
		t.Fatalf("read the architect starts of %s: %v", mnwRoot, err)
	}
	var sent []int64
	for entries.Next() {
		var entry int64
		if err := entries.Scan(&entry); err != nil {
			t.Fatalf("scan an architect start of %s: %v", mnwRoot, err)
		}
		sent = append(sent, entry)
	}
	if err := entries.Err(); err != nil {
		t.Fatalf("read the architect starts of %s: %v", mnwRoot, err)
	}
	entries.Close()
	// The architect's claim stands suspended, so the promotion resumes it on its standing entry.
	if len(sent) == 0 {
		t.Fatalf("no architect start of %s carries a task after the request, want its suspended architect resumed on entry %d", mnwRoot, row.Entry)
	}
	for _, entry := range sent {
		if entry != row.Entry {
			t.Fatalf("the architect starts of %s that carry a task name entries %v, want each entry %d: a start of another entry would supersede the pending task",
				mnwRoot, sent, row.Entry)
		}
	}
	applyFact(t, pool, admission, "mnw-gate-request", intake.GateRequestFound{Issue: mnwRoot, Generation: root.TreeRun(), ArtifactID: "spec-" + mnwRoot,
		RequestAsk: "ask-700", Since: 2})
	var mirrored *record.RootAsk
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		mirrored, err = record.NewStore().RootAsk(context.Background(), tx, mnwRoot, "ask-700")
		if err != nil {
			t.Fatal(err)
		}
	})
	if mirrored == nil {
		t.Fatalf("the gate request's fact at the tree's run %d mirrored nothing, want it taken", root.TreeRun())
	}
}

// A stranded merged child, closed in a tree that is not live and set todo without the label, enters
// once whichever comes first: its own todo and then its root's, its root's and then its own, or a
// boot listing that alone shows its todo. It takes one next change carrying one request, never a
// re-entry.
func TestStrandedMergedChildrenUseTheContinuation(t *testing.T) {
	seed := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		mnwClosedRoot(t, pool, false, false)
		child := mnwIssue(mnwChild, mnwRoot, new(mnwRoot), phase.Done, "done", 1)
		child.HandedOver = false
		putIssue(t, pool, child)
		mnwSnapshot(t, pool, mnwChild)
	}
	childTodo := mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+1, "person")
	childTodo.HandedOver = false
	rootTodo := mnwMove(mnwRoot, "", "todo", mnwSeq+1, "person")
	entered := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		mnwStarted(t, pool, mnwChild, 2, mnwRoot, new(mnwRoot), record.RequesterPerson)
		// One child-status stands: the start of the change's first phase (workflow.ChildStartedNotice),
		// which every child that takes a slot is told with. A re-entry adds its own beside it.
		if told := mnwCount(t, pool, mnwChild, "notice", "child-status"); told != 1 {
			t.Fatalf("child-status notices of %s = %d, want one, the start of its change's first phase: a re-entry adds another", mnwChild, told)
		}
	}
	t.Run("the child's todo, then the root's", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		seed(t, pool)
		mnwMoves(t, pool, admission, childTodo)
		if got := issue(t, pool, mnwChild); got.Generation != 1 || got.Status != "todo" {
			t.Fatalf("%s after its todo in a tree that is not live = generation %d with %s, want it recorded todo at generation 1", mnwChild, got.Generation, got.Status)
		}
		mnwMoves(t, pool, admission, rootTodo)
		tick(t, pool, admission, "mnw-stranded-pass")
		entered(t, pool)
	})
	t.Run("the root's todo, then the child's", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		seed(t, pool)
		mnwMoves(t, pool, admission, rootTodo, childTodo)
		tick(t, pool, admission, "mnw-stranded-pass")
		entered(t, pool)
	})
	t.Run("the boot listing alone shows the child's todo", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		seed(t, pool)
		mnwListed(t, pool, admission, dispatch.IssueSummary{Key: mnwChild, Title: mnwChild, Status: "todo", Parent: new(mnwRoot), Rank: "B",
			LastSeq: mnwSeq + 1, ResolvedRepo: "acme/widgets"})
		mnwMoves(t, pool, admission, rootTodo)
		tick(t, pool, admission, "mnw-stranded-pass")
		entered(t, pool)
	})
}

// Every next change's branch row carries where it starts from: the ordinary root's at its
// promotion, the orphan's at its promotion as a root of its own, the handed root's, and the
// stranded child's at its first phase. A member that stands on its own next change when its tree
// comes back carries it too, in the promotion's member loop.
func TestEveryMergedContinuationSetsItsBranchOrigin(t *testing.T) {
	// branched holds every issue_branch row of key's current generation to the merge's origin: a
	// row of the change's generation with no origin asks for the branch from the default branch,
	// whatever row follows it, so the last row alone proves nothing.
	branched := func(t *testing.T, pool *pgxpool.Pool, key string) {
		t.Helper()
		rows := mnwBranches(t, pool, key)
		got := issue(t, pool, key)
		var current []record.IssueBranch
		for _, row := range rows {
			if row.Generation == got.Generation {
				current = append(current, row)
			}
		}
		if len(current) == 0 {
			t.Fatalf("%s holds the branch rows %+v and none of generation %d, want its next change's", key, rows, got.Generation)
		}
		for _, row := range current {
			if row.From == nil || *row.From != mnwFrom {
				t.Fatalf("%s's branch rows = %+v, want each of generation %d from %+v", key, rows, got.Generation, mnwFrom)
			}
		}
	}
	t.Run("the ordinary root", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		mnwClosedRoot(t, pool, false, false)
		mnwMoves(t, pool, admission, mnwMove(mnwRoot, "", "todo", mnwSeq+1, "person"))
		tick(t, pool, admission, "mnw-branch-pass")
		branched(t, pool, mnwRoot)
		if task := mnwArchitectTask(t, pool, mnwRoot); !strings.Contains(task, workflow.RequestInstructionLabel) {
			t.Fatalf("the promoted architect's start task = %q, want the request's instruction", task)
		}
	})
	t.Run("the orphan", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		mnwPlacements()[3].seed(t, pool, false)
		mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+1, "person"))
		tick(t, pool, admission, "mnw-branch-pass")
		branched(t, pool, mnwChild)
	})
	t.Run("the handed root", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		mnwPlacements()[4].seed(t, pool, false)
		mnwMoves(t, pool, admission, mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+1, "person"))
		tick(t, pool, admission, "mnw-branch-pass")
		branched(t, pool, mnwChild)
	})
	t.Run("the stranded child", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		mnwClosedRoot(t, pool, false, false)
		child := mnwIssue(mnwChild, mnwRoot, new(mnwRoot), phase.Done, "done", 1)
		child.HandedOver = false
		putIssue(t, pool, child)
		mnwSnapshot(t, pool, mnwChild)
		todo := mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+1, "person")
		todo.HandedOver = false
		mnwMoves(t, pool, admission, todo, mnwMove(mnwRoot, "", "todo", mnwSeq+1, "person"))
		tick(t, pool, admission, "mnw-branch-pass")
		tick(t, pool, admission, "mnw-branch-pass-2")
		branched(t, pool, mnwChild)
	})
	t.Run("a member on its own next change in the promotion's member loop", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		mnwClosedRoot(t, pool, false, false)
		member := mnwIssue(mnwMember, mnwRoot, new(mnwRoot), phase.Implementing, "in_progress", 2)
		member.NextChange, member.NextChanges = 2, 1
		putIssue(t, pool, member)
		mnwSnapshot(t, pool, mnwMember)
		mnwMoves(t, pool, admission, mnwMove(mnwRoot, "", "todo", mnwSeq+1, "person"))
		tick(t, pool, admission, "mnw-branch-pass")
		branched(t, pool, mnwMember)
	})
}

// mnwArchitectTask is the task of the last start of key's architect that carries one, "" for none.
func mnwArchitectTask(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	task := ""
	for _, start := range startsOf(t, pool, key) {
		if start.role == claim.RoleArchitect && start.task != "" {
			task = start.task
		}
	}
	return task
}

// A closed root's linger hands over its member that a person set back to todo at done, and a
// person sets the root itself todo: the hand-over's event, its direct consumption and the root's
// request, in each order, end in one placement of the member, one next change of it, and no
// adoption back into the old tree. A consumption that names another linger's generation moves
// nothing.
func TestHandoverAndReopenHaveOneWinner(t *testing.T) {
	seed := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		mnwClosedRoot(t, pool, true, false)
		child := mnwIssue(mnwChild, mnwRoot, new(mnwRoot), phase.Done, "todo", 1)
		child.HandedOver = false
		putIssue(t, pool, child)
		mnwSnapshot(t, pool, mnwChild)
		inTx(t, pool, func(tx pgx.Tx) {
			if err := record.NewStore().PutHandover(context.Background(), tx, record.Handover{Issue: mnwChild, Tree: mnwRoot, Generation: 1}); err != nil {
				t.Fatal(err)
			}
		})
	}
	event := mnwMove(mnwChild, mnwRoot, "todo", mnwSeq+1, "")
	event.DaemonActor = true
	consume := intake.Handover{Issue: mnwChild, Tree: mnwRoot, Generation: 1}
	rootTodo := mnwMove(mnwRoot, "", "todo", mnwSeq+1, "person")
	once := func(t *testing.T, pool *pgxpool.Pool, tree string) {
		t.Helper()
		got := issue(t, pool, mnwChild)
		if got.Tree != tree || got.NextChanges != 1 || len(mnwRequests(t, pool, mnwChild)) != 1 {
			t.Fatalf("%s stands in tree %s with %d changes and requests %+v, want tree %s with one change and one request",
				mnwChild, got.Tree, got.NextChanges, mnwRequests(t, pool, mnwChild), tree)
		}
	}
	t.Run("the hand-over's event, then its consumption, then the root's request", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		seed(t, pool)
		apply(t, pool, admission, "mnw-handover-event", event, admission.engine)
		applyFact(t, pool, admission, "mnw-handover-consume", consume)
		mnwMoves(t, pool, admission, rootTodo)
		tick(t, pool, admission, "mnw-handover-pass")
		once(t, pool, mnwChild)
	})
	t.Run("the consumption, then the hand-over's event, then the root's request", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		seed(t, pool)
		applyFact(t, pool, admission, "mnw-handover-consume", consume)
		apply(t, pool, admission, "mnw-handover-event", event, admission.engine)
		mnwMoves(t, pool, admission, rootTodo)
		tick(t, pool, admission, "mnw-handover-pass")
		once(t, pool, mnwChild)
	})
	t.Run("the root's request, then the hand-over's event and its consumption", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		seed(t, pool)
		mnwMoves(t, pool, admission, rootTodo)
		apply(t, pool, admission, "mnw-handover-event", event, admission.engine)
		applyFact(t, pool, admission, "mnw-handover-consume", consume)
		tick(t, pool, admission, "mnw-handover-pass")
		once(t, pool, mnwRoot)
	})

	// A consumption of another linger's generation is held in two places, both kept: the hand-over
	// row's own generation (consumeHandover) and the root's linger at that generation
	// (record.Issue.LingersAt). The row's generation is the fact's own identity; the linger is what
	// the consumption acts on, and it also refuses a row of the right generation once the root no
	// longer lingers. With either gone the other still holds this case, so its control
	// (handover-fence) takes out both.
	t.Run("a consumption of another linger's generation moves nothing", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		seed(t, pool)
		before := mnwState(t, pool, mnwChild)
		applyFact(t, pool, admission, "mnw-handover-other-linger", intake.Handover{Issue: mnwChild, Tree: mnwRoot, Generation: 2})
		if after := mnwState(t, pool, mnwChild); after != before {
			t.Fatalf("%s after a consumption of linger 2 = %s, want it as it stood: %s", mnwChild, after, before)
		}
		if requests := mnwRequests(t, pool, mnwChild); len(requests) != 0 {
			t.Fatalf("the requests of %s after a consumption of linger 2 = %+v, want none", mnwChild, requests)
		}
	})
}

// Today's two paths stand where D17 keeps them: an unfinished handed run is carried in place at the
// hand-over, with no next change, and a person-confirmed never-merged root runs its whole tree
// again with the tree's run raised.
func TestANonMergedRerunAndAnUnfinishedCarryRunAsToday(t *testing.T) {
	t.Run("an unfinished handed run is carried", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		mnwClosedRoot(t, pool, true, false)
		midPhaseChild(t, pool, mnwRoot, mnwChild, phase.Implementing, "in_progress")
		mnwSnapshot(t, pool, mnwChild)
		inTx(t, pool, func(tx pgx.Tx) {
			if err := record.NewStore().PutHandover(context.Background(), tx, record.Handover{Issue: mnwChild, Tree: mnwRoot, Generation: 1}); err != nil {
				t.Fatal(err)
			}
		})
		event := mnwMove(mnwChild, mnwRoot, "todo", 2, "")
		event.DaemonActor = true
		apply(t, pool, admission, "mnw-carry-event", event, admission.engine)
		got := issue(t, pool, mnwChild)
		if got.Tree != mnwChild || got.Phase != phase.Implementing || got.Generation != 2 || got.NextChange != 0 {
			t.Fatalf("%s after its hand-over = tree %s in %s at generation %d with change %d, want its own tree carrying implementing at 2 and no change",
				mnwChild, got.Tree, got.Phase, got.Generation, got.NextChange)
		}
		if requests := mnwRequests(t, pool, mnwChild); len(requests) != 0 {
			t.Fatalf("the requests of the carried %s = %+v, want none", mnwChild, requests)
		}
	})
	t.Run("a person-confirmed never-merged root reruns its whole tree", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 4)
		putIssue(t, pool, mnwIssue(mnwRoot, mnwRoot, nil, phase.Done, "done", 1))
		mnwGate(t, pool, mnwRoot)
		seedLifecycle(t, pool, mnwRoot)
		mnwMoves(t, pool, admission, mnwMove(mnwRoot, "", "backlog", mnwSeq+1, "person"), mnwMove(mnwRoot, "", "todo", mnwSeq+2, "person"))
		got := mnwRerun(t, pool, mnwRoot, 2)
		if got.TreeRun() != 2 {
			t.Fatalf("the tree run of the rerun = %d, want it raised to 2", got.TreeRun())
		}
	})
}

// A closed root whose linger still runs takes a request: its next change ends the linger, and the
// linger's close at the old generation tears nothing of the resumed tree down, neither now nor in a
// later linger of the same tree run. The later linger's own close still acts.
func TestAnEarlierLingerCannotCloseAReopenedTree(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)
	mnwClosedRoot(t, pool, true, true)
	mnwMoves(t, pool, admission, mnwMove(mnwRoot, "", "todo", mnwSeq+1, "person"))
	root := mnwStarted(t, pool, mnwRoot, 2, mnwRoot, nil, record.RequesterPerson)
	member := mnwState(t, pool, mnwMember)
	closes := func() int {
		var count int
		if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'tree_close'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	applyFact(t, pool, admission, "mnw-old-linger", intake.LingerExpired{Issue: mnwRoot, Generation: 1})
	if got := closes(); got != 0 {
		t.Fatalf("tree_close rows after the old linger's close = %d, want none", got)
	}
	if after := mnwState(t, pool, mnwMember); after != member {
		t.Fatalf("%s after the old linger's close = %s, want %s", mnwMember, after, member)
	}

	mnwMoves(t, pool, admission, mnwMove(mnwRoot, "", "backlog", mnwSeq+2, "person"))
	if again := issue(t, pool, mnwRoot); !again.Lingers() || again.TreeRun() != root.TreeRun() {
		t.Fatalf("%s after its backlog = lingering %t at tree run %d, want a later linger of tree run %d", mnwRoot, again.Lingers(), again.TreeRun(), root.TreeRun())
	}
	applyFact(t, pool, admission, "mnw-old-linger-again", intake.LingerExpired{Issue: mnwRoot, Generation: 1})
	if got := closes(); got != 0 {
		t.Fatalf("tree_close rows after the old linger's close in the later linger = %d, want none", got)
	}
	applyFact(t, pool, admission, "mnw-later-linger", intake.LingerExpired{Issue: mnwRoot, Generation: 2})
	if got := closes(); got == 0 {
		t.Fatalf("tree_close rows after the later linger's own close = 0, want its close")
	}
}
