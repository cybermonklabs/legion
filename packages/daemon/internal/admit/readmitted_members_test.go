package admit

import (
	"context"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
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

// putClaimRow records role's claim of key in root's tree at generation in state, the daemon's own
// suspend standing for a suspended one (the durable record that the role has run), unless the
// claim already has a row. midPhaseChild seeds its phase role's claim through it.
func putClaimRow(t *testing.T, pool *pgxpool.Pool, root, key string, role claim.Role, generation int, state string) {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, key, role)
	if err != nil {
		t.Fatal(err)
	}
	suspendedBy := ""
	if state == "suspended" {
		suspendedBy = "daemon"
	}
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
			values ($1, $2, $3, $4, $5, $6, 'ses', '/sessions/ses.jsonl', $7, $6, 0, 0, 0, 0, $8) on conflict (token) do nothing`,
			string(token), project, root, key, string(role), generation, state, suspendedBy); err != nil {
			t.Fatalf("seed the %s of %s: %v", role, key, err)
		}
	})
}

// readmittedTree records the running tree runningTree seeds and moves its root to backlog and back
// to todo, as the controller's restart did on 2026-11-23. The backlog parks the root and gives its
// slot to LEGION-ACTIVE; the todo re-admits it at generation 2, where it waits in the line.
//
// With cleaned false the root comes back inside its linger, so its tree's lifecycle stands open
// under the workflow, as INFRA-3118's did on 2026-11-23. With cleaned true the tree's cleanup was
// reserved between the backlog and the todo, as after a linger that ended.
func readmittedTree(t *testing.T, pool *pgxpool.Pool, admission *Admission, cleaned bool) {
	t.Helper()
	runningTree(t, pool, cleaned)
	apply(t, pool, admission, "root-backlog", intake.DispatchIssue{Key: rmRoot, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "backlog", Title: "root", Rank: "B", HandedOver: handed}, admission.engine)
	if got := issue(t, pool, rmRoot); !got.Lingers() || got.Status != "backlog" {
		t.Fatalf("the parked %s = %#v, want it lingering in backlog", rmRoot, got)
	}
	if !holdsSlot(t, pool, rmActive) || holdsSlot(t, pool, rmRoot) {
		t.Fatalf("the slots after %s's backlog = %#v, want %s promoted onto the slot it gave back", rmRoot, slots(t, pool), rmActive)
	}
	if cleaned {
		inTx(t, pool, func(tx pgx.Tx) {
			if _, err := treelifecycle.ReserveCleanup(context.Background(), tx, "legion", rmRoot, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("reserve the cleanup of %s's tree: %v", rmRoot, err)
			}
		})
	}

	apply(t, pool, admission, "root-todo", intake.DispatchIssue{Key: rmRoot, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated",
		Status: "todo", Title: "root", Rank: "B", HandedOver: handed}, admission.engine)
	if got := issue(t, pool, rmRoot); got.Generation != 2 || got.Phase != phase.Admitted || got.Status != "todo" {
		t.Fatalf("the re-admitted %s = %#v, want generation 2 at admitted and todo: the fixture must leave the root waiting in the line", rmRoot, got)
	}
	if holdsSlot(t, pool, rmRoot) || !slices.Contains(waitingLine(t, pool, testProject, nil), rmRoot) {
		t.Fatalf("the re-admitted %s holds a slot or stands outside the line (%v), want it waiting there behind %s's slot",
			rmRoot, waitingLine(t, pool, testProject, nil), rmActive)
	}
}

// runningTree records the tree the cases move out and back as it runs before the move: its root
// holds the cap's one slot at admitted with its design gate opened, LEGION-ACTIVE waits in the
// line behind it, and its members stand mid-phase, one implementing, one merging, one in accepting
// whose own architect and implementer have run, and one a person reopened at done. Their claims
// are suspended by the daemon, as the suspends the tree's close queues leave them. With cleaned
// set the tree also holds a released child at admitted.
func runningTree(t *testing.T, pool *pgxpool.Pool, cleaned bool) {
	t.Helper()
	seedSlottedInPhase(t, pool, testProject, rmRoot, "B", phase.Admitted, func(root *record.Issue) {
		root.Title, root.DispatchStatus, root.LastDispatchSeq = "root", "in_progress", 1
	})
	openRootGate(t, pool)
	seedWaiting(t, pool, rmActive, "A")
	midPhaseChild(t, pool, rmRoot, rmImplementing, phase.Implementing, "in_progress")
	midPhaseChild(t, pool, rmRoot, rmMerging, phase.Merging, "in_progress")
	midPhaseChild(t, pool, rmRoot, rmAccepting, phase.Accepting, "needs_review")
	putClaimRow(t, pool, rmRoot, rmAccepting, claim.RoleImplementer, 1, "suspended")
	parent := rmRoot
	putIssue(t, pool, record.Issue{Key: rmReopened, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: rmReopened, Tree: rmRoot, Parent: &parent, Phase: phase.Done, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "B4", LastDispatchSeq: 1})
	if cleaned {
		putIssue(t, pool, record.Issue{Key: rmAdmitted, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
			Title: rmAdmitted, Tree: rmRoot, Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "B5", LastDispatchSeq: 1})
	}
}

// openRootGate records the root's spec approved at version 3 and its gate opened at every step.
func openRootGate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedGate(t, pool, record.DesignGate{Issue: rmRoot, ArtifactID: "artifact-300", LatestVersion: 3, ApprovedVersion: new(3), EverOpened: true})
}

// registerApprovedSpec starts the root's architect and applies its register_gate of the spec a
// person already approved, which opens the gate at once (the workflow's gateRegistered). The start
// is the operator's retry of the architect's claim that INFRA's controller made on 2026-11-23 while
// the roots waited in the line (the API's operator route, which the fact here does not pass
// through): the claim stands ready at the root's new generation when its architect registers.
func registerApprovedSpec(t *testing.T, pool *pgxpool.Pool, admission *Admission, id string) {
	t.Helper()
	putClaimRow(t, pool, rmRoot, rmRoot, claim.RoleArchitect, 2, "ready")
	if states := claimStates(t, pool); states[string(architectToken(t, rmRoot))] != "ready" {
		t.Fatalf("the claims = %v, want %s's architect started before it registers its spec", states, rmRoot)
	}
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

// memberStarts is every start row of issue that is no news-only start, oldest first (startsOf).
func memberStarts(t *testing.T, pool *pgxpool.Pool, issue string) []memberStart {
	t.Helper()
	var got []memberStart
	for _, start := range startsOf(t, pool, issue) {
		if !start.news {
			got = append(got, memberStart{role: start.role, withTask: start.task != "", needsSlot: start.needsSlot})
		}
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

// readmittedAdmission is an admission under a cap of cap whose log the test reads: a controller
// tick classifies every issue the line leaves behind and logs each one no held word is true of
// (admit's logUnclassified), the line INFRA's daemon wrote for two stranded members on 2026-11-23.
func readmittedAdmission(t *testing.T, cap int) (*Admission, *strings.Builder) {
	t.Helper()
	var lines strings.Builder
	return newAdmission(t, cap, slog.New(slog.NewTextHandler(&lines, nil))), &lines
}

// assertNoMemberUnclassified applies a controller tick, whose admission pass classifies each issue
// it leaves behind, and fails if it logged any of members as an issue no held word is true of.
func assertNoMemberUnclassified(t *testing.T, pool *pgxpool.Pool, admission *Admission, lines *strings.Builder, id string, members ...string) {
	t.Helper()
	applyFact(t, pool, admission, id, intake.ControllerTick{})
	for line := range strings.SplitSeq(lines.String(), "\n") {
		if !strings.Contains(line, "no held word is true of") {
			continue
		}
		for _, key := range members {
			if strings.Contains(line, "issue="+key+" ") {
				t.Fatalf("the admission pass logged the member %s as unclassified: %s", key, line)
			}
		}
	}
}

// assertMembersBack fails unless each member came back as the promotion of a re-admitted root
// brings it back (admit's startMidPhaseMembers and reenterStrandedChildren): the implementing
// member owes its phase start, waiting for a slot, and stands in the line or holds one; the
// merging member's merger is started with that phase's task; the accepting member's roles that
// ran are started with no task; the reopened member is re-entered at admitted on a new generation;
// and no member reads unclassified, in the state's held list or in the admission pass's log.
func assertMembersBack(t *testing.T, pool *pgxpool.Pool, admission *Admission, lines *strings.Builder) {
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
	assertNoMemberUnclassified(t, pool, admission, lines, "tick-members-back", rmMembers...)
}

// INFRA-5471, case 1: a re-admitted root whose architect registers the spec a person already
// approved before any promotion stops being a queued root at that registration, and its members
// come back then, in the registration's own fact and with no daemon restart. A later pass starts
// none of them again. On 2026-11-23 INFRA-3118's members waited from 18:30:41Z to 20:33:51Z.
func TestReadmittedMembersComeBackWhenTheGateOpensFirst(t *testing.T) {
	pool := migratedPool(t)
	admission, lines := readmittedAdmission(t, 1)
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
	assertMembersBack(t, pool, admission, lines)

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
	admission, lines := readmittedAdmission(t, 1)
	readmittedTree(t, pool, admission, false)

	promoteRoot(t, pool, admission)
	assertMembersBack(t, pool, admission, lines)

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
	admission, lines := readmittedAdmission(t, 1)
	readmittedTree(t, pool, admission, true)
	// The implementing member still owes the start of its phase from before the tree's close, as a
	// start a lease hid from finishFencedStarts through the close does: it stands for the start the
	// promotion would make (workflow.StartFor), and it must not put the member in the line first.
	oweStart(t, pool, rmRoot, rmImplementing, phase.Implementing)
	before := membersStarted(t, pool)

	registerApprovedSpec(t, pool, admission, "root-gate")
	line := waitingLine(t, pool, testProject, nil)
	if !slices.Contains(line, rmRoot) {
		t.Fatalf("the waiting line after the registration = %v, want %s still waiting in it for the promotion that opens its tree", line, rmRoot)
	}
	for _, key := range []string{rmImplementing, rmAdmitted} {
		if slices.Contains(line, key) {
			t.Fatalf("the waiting line after the registration = %v, want %s offered no place before its root's promotion", line, key)
		}
	}
	if got := membersStarted(t, pool); !reflect.DeepEqual(got, before) {
		t.Fatalf("the members after the registration = %#v, want them as the re-admission left them, %#v: nothing starts before the promotion", got, before)
	}
	if starts := memberStarts(t, pool, rmAdmitted); len(starts) != 0 {
		t.Fatalf("the starts of the released %s before its root's promotion = %+v, want none", rmAdmitted, starts)
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
	assertNoMemberUnclassified(t, pool, admission, lines, "tick-before-promotion", append(slices.Clone(rmMembers), rmAdmitted)...)
	if got := membersStarted(t, pool); !reflect.DeepEqual(got, before) {
		t.Fatalf("the members after a tick = %#v, want them as the re-admission left them, %#v", got, before)
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
	assertMembersBack(t, pool, admission, lines)
}

// INFRA-5471, case 4: a generation promoted before the change counts as brought back. A running
// tree whose root was promoted by the release before, its members at work and one reopened at done,
// starts nothing at the first pass after the change: the migration leaves such a root unmarked
// (store's TestTheMembersOwedMarkComesOnlyOnRootsWhoseReadmissionHasStillToRun), which is the
// record this test starts from.
func TestReadmittedMembersOfATreePromotedBeforeTheChangeStartNothing(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	seedSlottedInPhase(t, pool, testProject, rmRoot, "B", phase.Admitted, func(root *record.Issue) { root.Generation = 2 })
	openRootGate(t, pool)
	midPhaseChild(t, pool, rmRoot, rmImplementing, phase.Implementing, "in_progress")
	midPhaseChild(t, pool, rmRoot, rmMerging, phase.Merging, "in_progress")
	midPhaseChild(t, pool, rmRoot, rmAccepting, phase.Accepting, "needs_review")
	parent := rmRoot
	putIssue(t, pool, record.Issue{Key: rmReopened, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: rmReopened, Tree: rmRoot, Parent: &parent, Phase: phase.Done, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "B4", LastDispatchSeq: 1})
	if issue(t, pool, rmRoot).MembersOwed {
		t.Fatalf("the promoted %s owes its members their return, want the mark a promoted generation carries, none", rmRoot)
	}
	before := membersStarted(t, pool)

	applyFact(t, pool, admission, "first-pass", intake.ControllerTick{})
	if got := membersStarted(t, pool); !reflect.DeepEqual(got, before) {
		t.Fatalf("the members after the first pass = %#v, want them as they stood, %#v: its root's promotion before the change brought them back", got, before)
	}
}

// A re-admitted root whose label a person takes off while it waits in the line is no queued root,
// and its tree's lifecycle still stands open, but nothing hands that tree's work to the daemon: the
// opening of its gate brings no member back, as no promotion would, and the line offers the root no
// first phase of its own, which would take a slot and start its work before its members' return.
// The mark stands, and the members come back when the label does.
func TestReadmittedMembersOfARootWhoseLabelIsOffWaitForTheLabel(t *testing.T) {
	pool := migratedPool(t)
	admission, lines := readmittedAdmission(t, 1)
	readmittedTree(t, pool, admission, false)
	apply(t, pool, admission, "label-off", intake.DispatchIssue{Key: rmRoot, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated",
		Status: "todo", Title: "root", Rank: "B"}, admission.engine)
	if got := issue(t, pool, rmRoot); got.HandedOver || !got.MembersOwed || got.Lingers() {
		t.Fatalf("the root after its label came off = %#v, want it in todo with no label and its members still owed their return", got)
	}

	registerApprovedSpec(t, pool, admission, "root-gate")
	for _, key := range rmMembers {
		if starts := memberStarts(t, pool, key); len(starts) != 0 {
			t.Fatalf("the starts of %s while its root's label is off = %+v, want none", key, starts)
		}
	}
	if got := issue(t, pool, rmReopened); got.Phase != phase.Done {
		t.Fatalf("the reopened %s while its root's label is off = %#v, want it still at done", rmReopened, got)
	}
	if line := waitingLine(t, pool, testProject, nil); slices.Contains(line, rmRoot) {
		t.Fatalf("the waiting line while %s's label is off = %v, want no first phase offered to a root still owing its members their return", rmRoot, line)
	}

	apply(t, pool, admission, "label-on", intake.DispatchIssue{Key: rmRoot, ResolvedRepo: "acme/widgets", Seq: 5, Type: "issue.updated",
		Status: "todo", Title: "root", Rank: "B", HandedOver: handed}, admission.engine)
	assertMembersBack(t, pool, admission, lines)
}

// The return is the promotion's, and it waits for what the promotion waits for: a root the
// Dispatch consumer's pending Reconcile summary still holds may miss what Dispatch holds, so the
// pass that sees its gate open brings nothing back, and the pass after the consumer reaches that
// summary's position brings the members back.
func TestReadmittedMembersOfARootTheConsumerStillHoldsWaitForTheRelease(t *testing.T) {
	pool := migratedPool(t)
	admission, lines := readmittedAdmission(t, 1)
	readmittedTree(t, pool, admission, false)
	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: rmRoot, ResolvedRepo: "acme/widgets", Title: "root", Status: "todo", Rank: "B", HandedOver: handed, LastSeq: 4},
	}, 10, 2, false)
	if !admission.held(rmRoot) {
		t.Fatalf("the boot listing holds no %s, want it held until the consumer reaches the listing's position: the case's premise", rmRoot)
	}

	before := membersStarted(t, pool)
	registerApprovedSpec(t, pool, admission, "root-gate")
	if got := membersStarted(t, pool); !reflect.DeepEqual(got, before) {
		t.Fatalf("the members while the consumer holds their root = %#v, want them as the re-admission left them, %#v", got, before)
	}
	if got := issue(t, pool, rmRoot); !got.MembersOwed {
		t.Fatalf("the held %s = %#v, want its members still owed their return", rmRoot, got)
	}

	applyFact(t, pool, admission, "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 10})
	if admission.held(rmRoot) {
		t.Fatalf("%s is still held once the consumer reached the listing's position", rmRoot)
	}
	assertMembersBack(t, pool, admission, lines)
}

// A daemon of the release before, sharing the database, promotes a root this release's migration
// marked: its promotion brings the members back and never names the mark, so the mark outlives it,
// while the root holds the slot that promotion granted, or has given it back for a wait, or has
// moved on into accepting, which keeps no slot and clears the wait mark. Once the root's gate has
// opened, the first pass of this release clears that mark and brings nothing back a second time.
func TestReadmittedMembersOfARootAnEarlierReleasePromotedComeBackNoSecondTime(t *testing.T) {
	for _, row := range []struct {
		name string
		// movedOn is where the root stands after the earlier release's promotion: holding its
		// slot at admitted, given back for a wait at admitted, or in accepting with neither.
		movedOn func(t *testing.T, pool *pgxpool.Pool)
	}{
		{name: "holding its slot at admitted", movedOn: func(*testing.T, *pgxpool.Pool) {}},
		{name: "released for a wait at admitted", movedOn: func(t *testing.T, pool *pgxpool.Pool) {
			releaseForWait(t, pool, rmRoot)
		}},
		{name: "in accepting with no slot", movedOn: func(t *testing.T, pool *pgxpool.Pool) {
			if _, err := pool.Exec(context.Background(), `with released as (delete from slots where issue = $1)
				update issues set phase = 'accepting' where key = $1`, rmRoot); err != nil {
				t.Fatalf("move %s on into accepting: %v", rmRoot, err)
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			readmittedTree(t, pool, admission, false)
			earlierReleasePromotes(t, pool, admission)
			row.movedOn(t, pool)
			before := membersStarted(t, pool)
			if got := memberStarts(t, pool, rmMerging); len(got) != 1 {
				t.Fatalf("the starts of the merging %s after the earlier release's promotion = %+v, want its merger: the case's premise", rmMerging, got)
			}
			if got := issue(t, pool, rmRoot); !got.MembersOwed {
				t.Fatalf("the root the earlier release promoted = %#v, want it still marked: the case's premise", got)
			}

			openRootGate(t, pool)
			applyFact(t, pool, admission, "tick", intake.ControllerTick{})
			if got := membersStarted(t, pool); !reflect.DeepEqual(got, before) {
				t.Fatalf("the members after the gate opened = %#v, want them as the earlier release's promotion left them, %#v", got, before)
			}
			if got := issue(t, pool, rmRoot); got.MembersOwed {
				t.Fatalf("the root the earlier release promoted = %#v, want its stale mark cleared", got)
			}
		})
	}
}

// earlierReleasePromotes is a daemon of the release before promoting the waiting root, in a fact
// of its own: it grants the root a slot, moves it in progress and brings its members back
// (startMidPhaseMembers, reenterStrandedChildren) without naming the mark, which that release's
// records do not hold.
func earlierReleasePromotes(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	takeSlot(t, pool, rmRoot)
	root := issue(t, pool, rmRoot)
	root.Status, root.DispatchStatus = "in_progress", "in_progress"
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		ctx = admission.OpenFact(ctx)
		if _, err := tx.Exec(ctx, `update issues set status = 'in_progress', dispatch_status = 'in_progress' where key = $1`, rmRoot); err != nil {
			return err
		}
		issues, _, _, err := admission.reread(ctx, tx)
		if err != nil {
			return err
		}
		if err := admission.startMidPhaseMembers(ctx, tx, root, issues, fixedNow); err != nil {
			return err
		}
		if err := admission.reenterStrandedChildren(ctx, tx, root, issues); err != nil {
			return err
		}
		return admission.SettleFact(ctx, tx)
	}); err != nil {
		t.Fatalf("run the earlier release's promotion of %s: %v", rmRoot, err)
	}
}

// releaseForWait gives key's slot back for a wait, as releaseSlotsForWaits does: the row and the
// mark in one statement (record.Store.ReleaseSlotForWait).
func releaseForWait(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().ReleaseSlotForWait(context.Background(), tx, key, fixedNow); err != nil {
			t.Fatalf("release the slot of %s for a wait: %v", key, err)
		}
	})
}

// A root moved to backlog and back to todo while its project is paused comes back with its tree's
// slots and wait marks of the generation before still standing, since a paused pass releases
// nothing and the re-admission ends the linger that release reads. The re-admission frees them
// all, so after the resume the root's promotion opens its tree and brings its members back. Kept,
// the root's own slot would read as a promotion, and the pass would clear the mark as one an
// earlier release left and strand the members; a member's slot would fill the cap of one, so the
// root would never be promoted.
func TestReadmittedMembersOfARootMovedOutAndBackDuringAPauseComeBackAfterTheResume(t *testing.T) {
	for _, row := range []struct {
		name string
		// before is how the running tree stands when the pause comes.
		before func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
		// after is what follows the resume until the members come back.
		after func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
	}{
		{
			name: "the root holds its slot and a member's was given back for a wait",
			before: func(t *testing.T, pool *pgxpool.Pool, _ *Admission) {
				takeSlot(t, pool, rmImplementing)
				releaseForWait(t, pool, rmImplementing)
			},
			after: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				if holdsSlot(t, pool, rmRoot) || !slices.Contains(waitingLine(t, pool, testProject, nil), rmRoot) {
					t.Fatalf("after the resume %s holds a slot or stands outside the line (%v, slots %#v), want it waiting behind %s",
						rmRoot, waitingLine(t, pool, testProject, nil), slots(t, pool), rmActive)
				}
				registerApprovedSpec(t, pool, admission, "root-gate")
			},
		},
		{
			name: "a member holds the cap's one slot and the root's was given back for a wait",
			before: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				apply(t, pool, admission, "active-backlog", intake.DispatchIssue{Key: rmActive, ResolvedRepo: "acme/widgets", Seq: 2,
					Type: "issue.updated", Status: "backlog", Title: rmActive, Rank: "A", HandedOver: handed}, admission.engine)
				releaseForWait(t, pool, rmRoot)
				takeSlot(t, pool, rmImplementing)
			},
			after: func(t *testing.T, pool *pgxpool.Pool, _ *Admission) {
				if !holdsSlot(t, pool, rmRoot) {
					t.Fatalf("after the resume the slots = %#v, want %s promoted onto the cap's one slot", slots(t, pool), rmRoot)
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission, lines := readmittedAdmission(t, 1)
			runningTree(t, pool, false)
			row.before(t, pool, admission)
			pause := pauseTestProject(t, pool, fixedNow)
			apply(t, pool, admission, "root-backlog", intake.DispatchIssue{Key: rmRoot, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
				Status: "backlog", Title: "root", Rank: "B", HandedOver: handed}, admission.engine)
			apply(t, pool, admission, "root-todo", intake.DispatchIssue{Key: rmRoot, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated",
				Status: "todo", Title: "root", Rank: "B", HandedOver: handed}, admission.engine)
			if got := issue(t, pool, rmRoot); got.Generation != 2 || got.Phase != phase.Admitted || !got.MembersOwed {
				t.Fatalf("the root re-admitted during the pause = %#v, want generation 2 at admitted with its members owed their return", got)
			}
			for _, key := range append([]string{rmRoot}, rmMembers...) {
				if holdsSlot(t, pool, key) {
					t.Fatalf("the slots after the re-admission during the pause = %#v, want none held by %s: its tree's slots of the generation before are given back",
						slots(t, pool), key)
				}
				if got := issue(t, pool, key); got.ReleasedForWaitAt != nil {
					t.Fatalf("%s after its tree's re-admission during the pause = %#v, want no mark of a slot released for a wait", key, got)
				}
			}

			endTestPause(t, pool, pause, fixedNow)
			applyFact(t, pool, admission, "resumed", intake.ProjectResumed{})
			row.after(t, pool, admission)
			assertMembersBack(t, pool, admission, lines)
		})
	}
}
