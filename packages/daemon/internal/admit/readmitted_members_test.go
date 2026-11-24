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
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The tree of dispatch://INFRA-5471's cases: a root a person moved out of the workflow and back,
// and the members its re-admission leaves as they stood. LEGION-ACTIVE holds the one slot of the
// cap, so the re-admitted root waits in the line until that slot frees.
const (
	rmRoot         = "LEGION-300"
	rmImplementing = "LEGION-301"
	rmMerging      = "LEGION-302"
	rmAccepting    = "LEGION-303"
	rmReopened     = "LEGION-304"
	rmAdmitted     = "LEGION-305"
	rmActive       = "LEGION-ACTIVE"
)

// rmMembers is every member a re-admission leaves mid-phase or reopened, the issues the root's
// first run brings back.
var rmMembers = []string{rmImplementing, rmMerging, rmAccepting, rmReopened}

// suspendedClaim records role's claim of key in root's tree as a tree's close leaves it: suspended
// by the daemon's own suspend, the durable record that the role has run (midPhaseChild's insert).
func suspendedClaim(t *testing.T, pool *pgxpool.Pool, root, key string, role claim.Role) {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, key, role)
	if err != nil {
		t.Fatal(err)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
			values ($1, $2, $3, $4, $5, 1, 'ses', '/sessions/ses.jsonl', 'suspended', 1, 0, 0, 0, 0, 'daemon')`,
			string(token), project, root, key, string(role)); err != nil {
			t.Fatalf("seed the %s of %s: %v", role, key, err)
		}
	})
}

// readmittedTree records the tree as a person's move to backlog and back leaves it, and applies
// the root's todo, which re-admits it at generation 2 while LEGION-ACTIVE holds the cap's one slot,
// so the root waits in the line. The members stand where the tree's close left them: one
// implementing, one merging, one in accepting whose own architect and implementer have run, and
// one a person reopened at done. Their claims are suspended by the daemon.
//
// With cleaned false the root comes back inside its linger, so its tree's lifecycle stands open
// under the workflow, as INFRA-3118's did on 2026-11-23. With cleaned true its linger ended and
// the tree's cleanup was reserved before the todo, and the tree holds a released child at
// admitted beside the others.
func readmittedTree(t *testing.T, pool *pgxpool.Pool, admission *Admission, cleaned bool) {
	t.Helper()
	seedSlotted(t, pool, rmActive, "A")
	linger := fixedNow.Add(time.Hour)
	if cleaned {
		linger = fixedNow.Add(-time.Hour)
	}
	putIssue(t, pool, record.Issue{Key: rmRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: rmRoot,
		Project: testProject, Title: "root", Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "B",
		HandedOver: true, LingerUntil: &linger, LastDispatchSeq: 1})
	midPhaseChild(t, pool, rmRoot, rmImplementing, phase.Implementing, "in_progress")
	midPhaseChild(t, pool, rmRoot, rmMerging, phase.Merging, "in_progress")
	midPhaseChild(t, pool, rmRoot, rmAccepting, phase.Accepting, "needs_review")
	suspendedClaim(t, pool, rmRoot, rmAccepting, claim.RoleImplementer)
	parent := rmRoot
	putIssue(t, pool, record.Issue{Key: rmReopened, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: rmReopened, Tree: rmRoot, Parent: &parent, Phase: phase.Done, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "B4", LastDispatchSeq: 1})
	if cleaned {
		putIssue(t, pool, record.Issue{Key: rmAdmitted, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
			Title: rmAdmitted, Tree: rmRoot, Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "B5", LastDispatchSeq: 1})
	}
	inTx(t, pool, func(tx pgx.Tx) {
		seedLifecycleTx(t, tx, rmRoot)
		if cleaned {
			if _, err := treelifecycle.ReserveCleanup(context.Background(), tx, "legion", rmRoot, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("reserve the cleanup of %s's tree: %v", rmRoot, err)
			}
		}
	})

	apply(t, pool, admission, "root-todo", intake.DispatchIssue{Key: rmRoot, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: "root", Rank: "B", HandedOver: handed}, admission.engine)
	if got := issue(t, pool, rmRoot); got.Generation != 2 || got.Phase != phase.Admitted || got.Status != "todo" {
		t.Fatalf("the re-admitted %s = %#v, want generation 2 at admitted and todo: the fixture must leave the root waiting in the line", rmRoot, got)
	}
	if holdsSlot(t, pool, rmRoot) || !slices.Contains(waitingLine(t, pool, testProject, nil), rmRoot) {
		t.Fatalf("the re-admitted %s holds a slot or stands outside the line (%v), want it waiting there behind %s's slot",
			rmRoot, waitingLine(t, pool, testProject, nil), rmActive)
	}
}

// registerApprovedSpec is the root architect's register_gate of the spec a person already
// approved, which opens the gate at once (the workflow's gateRegistered).
func registerApprovedSpec(t *testing.T, pool *pgxpool.Pool, admission *Admission, id string) {
	t.Helper()
	applyFact(t, pool, admission, id, intake.GateRegistered{Issue: rmRoot, ArtifactID: "artifact-300", Version: 3, ApprovedVersion: 3,
		ApprovedMark: record.EventMark{Seq: 9, At: reviewedAt}})
}

// promoteRoot frees LEGION-ACTIVE's slot, which promotes the waiting root.
func promoteRoot(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	apply(t, pool, admission, "active-done", intake.DispatchIssue{Key: rmActive, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "done", Title: rmActive, Rank: "A"}, admission.engine)
	if !holdsSlot(t, pool, rmRoot) {
		t.Fatalf("the slots after %s's done = %#v, want %s promoted onto one", rmActive, slots(t, pool), rmRoot)
	}
}

// memberStart is one start row of an issue that is no news-only start: a news start brings no work
// (workflow's startsWork), and an opening of the gate may queue one for any holder.
type memberStart struct {
	role      claim.Role
	withTask  bool
	needsSlot bool
}

// memberStarts is every start row of issue that is no news-only start, oldest first.
func memberStarts(t *testing.T, pool *pgxpool.Pool, issue string) []memberStart {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select coalesce(payload->>'role', ''), coalesce(payload->>'task', '') <> '',
		coalesce(payload->>'needsSlot', '') = 'true'
		from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' and coalesce(payload->>'news', '') <> 'true'
		order by id`, issue)
	if err != nil {
		t.Fatalf("list the starts of %s: %v", issue, err)
	}
	defer rows.Close()
	var got []memberStart
	for rows.Next() {
		var start memberStart
		if err := rows.Scan(&start.role, &start.withTask, &start.needsSlot); err != nil {
			t.Fatalf("scan a start of %s: %v", issue, err)
		}
		got = append(got, start)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the starts of %s: %v", issue, err)
	}
	return got
}

// membersStarted is each member's work starts and the record of the reopened member, which a later
// pass must leave as they stand.
func membersStarted(t *testing.T, pool *pgxpool.Pool) map[string]any {
	t.Helper()
	got := map[string]any{}
	for _, key := range rmMembers {
		got[key] = memberStarts(t, pool, key)
	}
	reopened := issue(t, pool, rmReopened)
	got["reopened"] = []any{reopened.Phase, reopened.Generation}
	return got
}

// assertMembersBack fails unless each member came back as the promotion of a re-admitted root
// brings it back (admit's startMidPhaseMembers and reenterStrandedChildren): the implementing
// member owes its phase start, waiting for a slot, and stands in the line or holds one; the
// merging member's merger is started with that phase's task; the accepting member's roles that
// ran are started with no task; the reopened member is re-entered at admitted on a new generation;
// and no member reads unclassified.
func assertMembersBack(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	if got, want := memberStarts(t, pool, rmImplementing), []memberStart{{role: claim.RoleImplementer, withTask: true, needsSlot: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the starts of the implementing %s = %+v, want %+v: its phase start, waiting for a slot", rmImplementing, got, want)
	}
	if !slices.Contains(waitingLine(t, pool, testProject, nil), rmImplementing) && !holdsSlot(t, pool, rmImplementing) {
		t.Fatalf("the implementing %s stands neither in the line (%v) nor on a slot (%#v)", rmImplementing, waitingLine(t, pool, testProject, nil), slots(t, pool))
	}
	if got, want := memberStarts(t, pool, rmMerging), []memberStart{{role: claim.RoleMerger, withTask: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the starts of the merging %s = %+v, want %+v: its merger, with merging's task", rmMerging, got, want)
	}
	if got, want := memberStarts(t, pool, rmAccepting), []memberStart{{role: claim.RoleArchitect}, {role: claim.RoleImplementer}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the starts of the accepting %s = %+v, want %+v: each role of it that ran, with no task", rmAccepting, got, want)
	}
	if got := issue(t, pool, rmReopened); got.Tree != rmRoot || got.Phase != phase.Admitted || got.Generation < 2 {
		t.Fatalf("the reopened %s = %#v, want it re-entered at admitted on a new generation in %s's tree", rmReopened, got, rmRoot)
	}
	held := heldReasons(t, pool, admission)
	for _, key := range rmMembers {
		if held[key] == record.HeldUnclassified {
			t.Fatalf("admission.held gives %s %q: %#v", key, record.HeldUnclassified, held)
		}
	}
}

// INFRA-5471, case 1: a re-admitted root whose architect registers the spec a person already
// approved before any promotion stops being a queued root at that registration, and its members
// come back then, in the registration's own fact and with no daemon restart. A later pass starts
// none of them again. On 2026-11-23 INFRA-3118's members waited from 18:30:41Z to 20:33:51Z.
func TestReadmittedMembersComeBackWhenTheGateOpensFirst(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	readmittedTree(t, pool, admission, false)
	for _, key := range rmMembers {
		if starts := memberStarts(t, pool, key); len(starts) != 0 {
			t.Fatalf("the starts of %s while its root waits in the line = %+v, want none", key, starts)
		}
	}

	registerApprovedSpec(t, pool, admission, "root-gate")
	if holdsSlot(t, pool, rmRoot) {
		t.Fatalf("the slots = %#v, want %s still on none: the registration promotes nothing", slots(t, pool), rmRoot)
	}
	assertMembersBack(t, pool, admission)

	before := membersStarted(t, pool)
	applyFact(t, pool, admission, "tick-1", intake.ControllerTick{})
	applyFact(t, pool, admission, "tick-2", intake.ControllerTick{})
	if got := membersStarted(t, pool); !reflect.DeepEqual(got, before) {
		t.Fatalf("the members after two more passes = %#v, want them as the registration left them, %#v: once in a generation", got, before)
	}
}

// INFRA-5471, case 2, the control: the same tree promoted before its gate opens. The promotion
// brings the members back as it always has, and the registration after it starts none of them
// again. It passes before and after the change.
func TestReadmittedMembersComeBackOnceWhenThePromotionComesFirst(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	readmittedTree(t, pool, admission, false)

	promoteRoot(t, pool, admission)
	assertMembersBack(t, pool, admission)

	before := membersStarted(t, pool)
	registerApprovedSpec(t, pool, admission, "root-gate")
	applyFact(t, pool, admission, "tick", intake.ControllerTick{})
	if got := membersStarted(t, pool); !reflect.DeepEqual(got, before) {
		t.Fatalf("the members after the registration = %#v, want them as the promotion left them, %#v: once in a generation", got, before)
	}
}

// INFRA-5471, case 3: a root re-admitted after its tree's linger ended and the cleanup was
// reserved has no open tree to run its members in. The registration of its approved spec leaves
// it a queued root, so it keeps its place in the line; nothing of the tree starts, the released
// child at admitted is offered no phase, and no member reads unclassified or at_work while it
// waits. Once the cleanup has confirmed, the promotion opens the tree's next epoch and brings the
// members back.
func TestReadmittedMembersOfACleanedTreeWaitForThePromotion(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	readmittedTree(t, pool, admission, true)

	registerApprovedSpec(t, pool, admission, "root-gate")
	line := waitingLine(t, pool, testProject, nil)
	if !slices.Contains(line, rmRoot) {
		t.Fatalf("the waiting line after the registration = %v, want %s still waiting in it for the promotion that opens its tree", line, rmRoot)
	}
	if slices.Contains(line, rmAdmitted) {
		t.Fatalf("the waiting line after the registration = %v, want the released %s offered no phase before its root's promotion", line, rmAdmitted)
	}
	for _, key := range append(slices.Clone(rmMembers), rmAdmitted) {
		if starts := memberStarts(t, pool, key); len(starts) != 0 {
			t.Fatalf("the starts of %s before its root's promotion = %+v, want none", key, starts)
		}
	}
	if got := issue(t, pool, rmReopened); got.Phase != phase.Done {
		t.Fatalf("the reopened %s before its root's promotion = %#v, want it still at done", rmReopened, got)
	}
	// Each member stands in admission.held with tree_closed, whose sentence names the root's
	// promotion as what ends the hold, and never with at_work or unclassified, which would say its
	// workers run or that no rule names its wait.
	held := heldReasons(t, pool, admission)
	for _, key := range []string{rmImplementing, rmMerging, rmAccepting, rmAdmitted} {
		if word := held[key]; word != record.HeldTreeClosed {
			t.Fatalf("admission.held gives %s %q before its root's promotion, want %q: %#v", key, word, record.HeldTreeClosed, held)
		}
	}
	if word, listed := held[rmReopened]; listed && (word == record.HeldUnclassified || word == record.HeldAtWork) {
		t.Fatalf("admission.held gives %s %q before its root's promotion: %#v", rmReopened, word, held)
	}

	inTx(t, pool, func(tx pgx.Tx) {
		lifecycle, found, err := treelifecycle.Read(context.Background(), tx, "legion", rmRoot)
		if err != nil || !found {
			t.Fatalf("read the lifecycle of %s's tree: found %t, %v", rmRoot, found, err)
		}
		if err := treelifecycle.ConfirmCleanup(context.Background(), tx, "legion", rmRoot, lifecycle.Epoch); err != nil {
			t.Fatalf("confirm the cleanup of %s's tree: %v", rmRoot, err)
		}
	})
	promoteRoot(t, pool, admission)
	assertMembersBack(t, pool, admission)
}

// INFRA-5471, case 4: a generation promoted before the change counts as brought back. A running
// tree whose root was promoted by the release before, its members at work, starts nothing at the
// first pass after the change.
func TestReadmittedMembersOfATreePromotedBeforeTheChangeStartNothing(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	seedSlottedInPhase(t, pool, testProject, rmRoot, "B", phase.Admitted, func(root *record.Issue) { root.Generation = 2 })
	seedGate(t, pool, record.DesignGate{Issue: rmRoot, ArtifactID: "artifact-300", LatestVersion: 3, ApprovedVersion: new(3), EverOpened: true})
	midPhaseChild(t, pool, rmRoot, rmImplementing, phase.Implementing, "in_progress")
	midPhaseChild(t, pool, rmRoot, rmMerging, phase.Merging, "in_progress")
	midPhaseChild(t, pool, rmRoot, rmAccepting, phase.Accepting, "needs_review")

	applyFact(t, pool, admission, "first-pass", intake.ControllerTick{})
	for _, key := range []string{rmImplementing, rmMerging, rmAccepting} {
		if starts := memberStarts(t, pool, key); len(starts) != 0 {
			t.Fatalf("the starts of %s after the first pass = %+v, want none: its root's promotion before the change brought it back", key, starts)
		}
	}
}
