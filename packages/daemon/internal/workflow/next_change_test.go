package workflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The tests of the next change (D7 of the done-by-checks plan): a reason after the
// merge starts one next change of the same issue, a new generation at admitted with its gate
// kept; the moment it starts; its steps; the join of a later reason; the carry of a return; its
// end with no pull request; the count; and the merger's completion in verifying. Each drives the
// waitRig of close_wait_test.go.

// changeGeneration is the generation the first next change of the rig's issues runs at.
const changeGeneration = waitGeneration + 1

// failing is check id failing at merge, its deciding result result-<id>.
func failing(id, merge string) dispatch.CheckBlock {
	return check(id, merge, dispatch.PassStandsNo, dispatch.PassStandsReasonFail)
}

// failingAt is check id failing at merge with deciding result result.
func failingAt(id, merge, result string) dispatch.CheckBlock {
	block := failing(id, merge)
	block.PassStands.Results = []string{result}
	return block
}

// readAt is the outbox's read of issue's checks queued at generation coming back, begun a minute
// after the clock stood.
func (r *waitRig) readAt(issue string, generation uint64, answer record.CheckReadAnswer) {
	r.t.Helper()
	r.now = r.now.Add(time.Minute)
	r.fact(intake.CloseChecksRead{Issue: issue, Generation: generation, ReadAt: r.now, Answer: &answer})
}

// defectSeen is one row of defects as these tests read it.
type defectSeen struct {
	id         int64
	generation uint64
	kind       record.DefectKind
	check      string
	result     string
	carriedBy  uint64
	merged     bool
	withdrawn  bool
}

// defectRows is every defect of issue, oldest first, whatever its state.
func (r *waitRig) defectRows(issue string) []defectSeen {
	r.t.Helper()
	rows, err := r.pool.Query(r.t.Context(), `select id, generation, kind, check_block, result_id, coalesce(carried_by, 0),
		merged_at is not null, withdrawn_at is not null from defects where issue = $1 order by id`, issue)
	if err != nil {
		r.t.Fatalf("read the defects of %s: %v", issue, err)
	}
	defer rows.Close()
	var seen []defectSeen
	for rows.Next() {
		var row defectSeen
		var generation, carried int64
		var kind string
		if err := rows.Scan(&row.id, &generation, &kind, &row.check, &row.result, &carried, &row.merged, &row.withdrawn); err != nil {
			r.t.Fatalf("scan a defect of %s: %v", issue, err)
		}
		row.generation, row.carriedBy, row.kind = uint64(generation), uint64(carried), record.DefectKind(kind)
		seen = append(seen, row)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the defects of %s: %v", issue, err)
	}
	return seen
}

// rootArchitect is the claim of the architect of the rig's tree, which owns every issue of it.
func (r *waitRig) rootArchitect() claim.Token {
	r.t.Helper()
	return claim.Token(treeRoleToken(r.t, waitRoot, claim.RoleArchitect))
}

// withdraw is the owning architect's withdraw_defect of defect id of issue.
func (r *waitRig) withdraw(issue string, id int64, reason string) intake.Result {
	r.t.Helper()
	result := r.fact(intake.DefectWithdrawal{Issue: issue, Defect: id, Reason: reason, By: r.rootArchitect()})
	if result.Refusal != nil {
		r.t.Fatalf("the withdrawal of defect %d of %s = %+v, want it taken", id, issue, result.Refusal)
	}
	return result
}

// controllerNotices is the number of controller notices of kind queued on issue.
func (r *waitRig) controllerNotices(issue, kind string) int {
	r.t.Helper()
	var count int
	if err := r.pool.QueryRow(r.t.Context(), `select count(*) from outbox where kind = 'controller_notice' and issue = $1 and payload->>'kind' = $2`,
		issue, kind).Scan(&count); err != nil {
		r.t.Fatalf("count %s's %s controller notices: %v", issue, kind, err)
	}
	return count
}

// branchRows is issue's issue_branch rows, oldest first, with their ids.
func (r *waitRig) branchRows(issue string) ([]int64, []record.IssueBranch) {
	r.t.Helper()
	var ids []int64
	var rows []record.IssueBranch
	for _, row := range issueOutboxOf(r.t, r.pool, issue, string(record.OutboxKindIssueBranch)) {
		var branch record.IssueBranch
		if err := json.Unmarshal(row.payload, &branch); err != nil {
			r.t.Fatalf("decode the branch row %d of %s: %v", row.id, issue, err)
		}
		ids, rows = append(ids, row.id), append(rows, branch)
	}
	return ids, rows
}

// workerStart is issue's first start of a phase worker at generation, with its outbox id: a start
// that carries a task and names a phase.
func (r *waitRig) workerStart(issue string, generation uint64) (int64, record.SuperviseRequest, bool) {
	r.t.Helper()
	for _, row := range issueOutboxOf(r.t, r.pool, issue, "supervise") {
		start := decodeSupervise(r.t, row)
		if start.Op == "start" && start.Task != "" && start.Phase != "" && start.Generation == generation && start.CheckLook == 0 {
			return row.id, start, true
		}
	}
	return 0, record.SuperviseRequest{}, false
}

// architectStarts is the number of issue's start rows of the architect.
func (r *waitRig) architectStarts(issue string) int {
	r.t.Helper()
	count := 0
	for _, start := range superviseRowsOf(r.t, r.pool, issue, "start") {
		if start.Role == claim.RoleArchitect {
			count++
		}
	}
	return count
}

// started fails the test unless issue's next change stands at generation, at admitted with the
// record's status todo, open, and count changes started.
func (r *waitRig) started(issue string, generation uint64, count int) record.Issue {
	r.t.Helper()
	got := r.issue(issue)
	if got.Generation != generation || got.Phase != phase.Admitted || got.Status != "todo" || got.NextChange != generation || got.NextChanges != count {
		r.t.Fatalf("%s stands at generation %d in %s with status %s, next change %d of %d started; want the next change %d at admitted with todo, change %d",
			issue, got.Generation, got.Phase, got.Status, got.NextChange, got.NextChanges, generation, count)
	}
	return got
}

// noChange fails the test unless issue still stands at generation with no change started.
func (r *waitRig) noChange(issue string, generation uint64) {
	r.t.Helper()
	if got := r.issue(issue); got.Generation != generation || got.NextChange != 0 || got.NextChanges != 0 {
		r.t.Fatalf("%s stands at generation %d with next change %d of %d started, want generation %d and no next change",
			issue, got.Generation, got.NextChange, got.NextChanges, generation)
	}
}

// changeMerges stands issue's open change in merging on pull request number at head, its merger
// done, and merges it at merge.
func (r *waitRig) changeMerges(key string, number int, merge, head string) {
	r.t.Helper()
	issue := r.issue(key)
	issue.Phase, issue.Status, issue.DispatchStatus = phase.Merging, "retro", "retro"
	seedIssue(r.t, r.pool, issue)
	seedPhase(r.t, r.pool, record.PhaseRow{Issue: key, Role: claim.RoleMerger, Claim: "change-merger", HandoffCommit: head})
	seedPR(r.t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: waitRepo, Number: number, Branch: claim.IssueBranch(key),
		Base: "main", HeadSHA: head, Generation: int64(issue.Generation), Failing: []string{}})
	r.fact(intake.PullRequestMerged{Repo: waitRepo, Number: number, MergeSHA: merge, HeadSHA: head, MergedAt: r.now})
}

// standIn stands issue's record in at with status, everything else as it was.
func (r *waitRig) standIn(key string, at phase.Phase, status string) {
	r.t.Helper()
	issue := r.issue(key)
	issue.Phase, issue.Status = at, status
	seedIssue(r.t, r.pool, issue)
}

// mergedChild is a rig whose root works in implementing and whose child merged and waits in
// verifying, its first read of the checks not yet applied.
func mergedChild(t *testing.T, rig *waitRig) {
	t.Helper()
	rig.seedRoot(phase.Implementing, "in_progress")
	rig.seedChild(waitChild, phase.Merging, "retro")
	rig.mergeChild()
}

// approvedGate seeds issue's approved spec gate and answers its rows as the store holds them.
func approvedGate(t *testing.T, rig *waitRig, issue string) map[record.DesignStep]record.DesignGate {
	t.Helper()
	seedGate(t, rig.pool, record.DesignGate{Issue: issue, Step: record.DesignStepSpec, ArtifactID: "spec-" + issue, LatestVersion: 2, ApprovedVersion: new(2)})
	return gatesOf(t, rig.pool, issue)
}

// A check that Dispatch computes as a fail after a child's merge starts the child's next change:
// a new generation at admitted with the record's status todo and its Dispatch status written todo,
// the design gate kept, the architect told once of the fail and once of the change, and the check's
// reason recorded with its deciding result and carried by the new generation. The change's first
// step then queues its branch row, which starts the branch at the base the merge went into and
// names the head that merged, before the first worker's start, and that start moves the working
// copy to the branch first (record.StartAt).
func TestAComputedFailStartsTheNextChangeOfAChild(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	mergedChild(t, rig)
	gates := approvedGate(t, rig, waitChild)

	rig.read(waitChild, answerOf(waitChild, nil, failing("c-after", waitChildMerge)))

	rig.started(waitChild, changeGeneration, 1)
	if got := rig.statusWrites(waitChild, "todo"); got != 1 {
		t.Fatalf("%s's todo status writes = %d, want one", waitChild, got)
	}
	if got := gatesOf(t, rig.pool, waitChild); !reflect.DeepEqual(got, gates) {
		t.Fatalf("%s's gates after the start = %+v, want them kept, %+v", waitChild, got, gates)
	}
	if got := noticesOfKind(t, rig.pool, waitChild, "check-failed"); len(got) != 1 {
		t.Fatalf("check-failed notices = %+v, want one", got)
	}
	if got := noticesOfKind(t, rig.pool, waitChild, "next-change"); len(got) != 1 {
		t.Fatalf("next-change notices = %+v, want one", got)
	}
	want := []defectSeen{{generation: waitGeneration, kind: record.DefectKindCheck, check: "c-after", result: "result-c-after", carriedBy: changeGeneration}}
	got := rig.defectRows(waitChild)
	for i := range got {
		got[i].id = 0
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the reasons of %s = %+v, want %+v", waitChild, got, want)
	}

	firstStep(t, rig.pool, rig.engine, waitChild)
	ids, branches := rig.branchRows(waitChild)
	if len(branches) != 1 || !reflect.DeepEqual(branches[0], record.IssueBranch{Generation: changeGeneration,
		From: &record.BranchFrom{Base: "main", MergedHead: headTwo}}) {
		t.Fatalf("the branch rows of %s = %+v, want one of generation %d from main at the merged head %s", waitChild, branches, changeGeneration, headTwo)
	}
	id, start, ok := rig.workerStart(waitChild, changeGeneration)
	if !ok || id < ids[0] {
		t.Fatalf("the first worker start of %s = %d %+v (%t), want one after the branch row %d", waitChild, id, start, ok, ids[0])
	}
	if start.StartAt == nil || *start.StartAt != (record.StartAt{Bookmark: claim.IssueBranch(waitChild), Base: "main"}) {
		t.Fatalf("the first worker start of %s starts its working copy at %+v, want %s from main", waitChild, start.StartAt, claim.IssueBranch(waitChild))
	}
}

// The same fail after a root's merge starts the root's next change through the same function: a
// new generation of the root alone, with its gate kept. The tree's architect is not started again,
// at the start or at the change's first step.
func TestAComputedFailStartsTheNextChangeOfARoot(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	mergedRoot(t, rig)
	gates := approvedGate(t, rig, waitRoot)

	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))

	rig.started(waitRoot, changeGeneration, 1)
	if got := gatesOf(t, rig.pool, waitRoot); !reflect.DeepEqual(got, gates) {
		t.Fatalf("%s's gates after the start = %+v, want them kept, %+v", waitRoot, got, gates)
	}
	if got := noticesOfKind(t, rig.pool, waitRoot, "check-failed"); len(got) != 1 {
		t.Fatalf("check-failed notices = %+v, want one", got)
	}
	if got := noticesOfKind(t, rig.pool, waitRoot, "next-change"); len(got) != 1 {
		t.Fatalf("next-change notices = %+v, want one", got)
	}
	if rows := rig.defectRows(waitRoot); len(rows) != 1 || rows[0].kind != record.DefectKindCheck || rows[0].carriedBy != changeGeneration {
		t.Fatalf("the reasons of %s = %+v, want the check's, carried by %d", waitRoot, rows, changeGeneration)
	}
	if got := rig.architectStarts(waitRoot); got != 0 {
		t.Fatalf("architect starts after the root's next change = %d, want none: the tree's architect runs on", got)
	}
	firstStep(t, rig.pool, rig.engine, waitRoot)
	if _, branches := rig.branchRows(waitRoot); len(branches) != 1 || branches[0].From == nil || branches[0].From.MergedHead != headOne {
		t.Fatalf("the branch rows of %s = %+v, want one from the merged head %s", waitRoot, branches, headOne)
	}
	if got := rig.architectStarts(waitRoot); got != 0 {
		t.Fatalf("architect starts after the change's first step = %d, want none", got)
	}
}

// memberRows is everything of key a root's next change must leave: the record, its phase rows, its
// pull request, its gate and its claims, each row as JSON.
func memberRows(t *testing.T, rig *waitRig, key string) map[string][]string {
	t.Helper()
	rows := map[string][]string{}
	for table, query := range map[string]string{
		"issues":        "select to_jsonb(t)::text from issues t where key = $1",
		"phases":        "select to_jsonb(t)::text from phases t where issue = $1 order by role",
		"pull_requests": "select to_jsonb(t)::text from pull_requests t where issue = $1",
		"design_gates":  "select to_jsonb(t)::text from design_gates t where issue = $1 order by step",
		"claims":        "select to_jsonb(t)::text from claims t where issue = $1 order by role",
	} {
		rows[table] = outboxStrings(t, rig.pool, query, key)
	}
	return rows
}

// A root's next change touches no other member of its tree: a child at work in implementing, with
// its phase rows, its pull request, its gate and its claim, and a child that left the tree done,
// each stand as they stood.
func TestANextChangeOfARootLeavesEachOtherMemberAsItIs(t *testing.T) {
	t.Parallel()
	const done = "LEGION-210"
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	rig.seedChild(waitChild, phase.Implementing, "in_progress")
	rig.seedChild(done, phase.Done, "done")
	seedPhase(t, rig.pool, record.PhaseRow{Issue: waitChild, Role: claim.RoleImplementer, Claim: "child-implementer", EntryTask: "implement it", Entry: 1})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: waitChild, Repo: waitRepo, Number: waitChildNumber,
		Branch: claim.IssueBranch(waitChild), Base: "main", HeadSHA: headTwo, Generation: waitGeneration, Failing: []string{}})
	approvedGate(t, rig, waitChild)
	st := openStore(t, rig)
	for _, key := range []string{waitChild, done} {
		putMemberClaim(t, st, key, supervise.StateIdle, "")
	}
	rig.mergeRoot()
	before := map[string]map[string][]string{waitChild: memberRows(t, rig, waitChild), done: memberRows(t, rig, done)}

	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))

	rig.started(waitRoot, changeGeneration, 1)
	for key, want := range before {
		if got := memberRows(t, rig, key); !reflect.DeepEqual(got, want) {
			t.Errorf("%s after the root's next change = %v, want it as it stood, %v", key, got, want)
		}
	}
}

// A root's next change reaches its first phase with no new approval: the gate's rows stand
// approved, no read of an approval request and no publication of a cleared gate is queued, and
// the first step starts the first phase's worker.
func TestANextChangeOfARootAsksNoApproval(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	mergedRoot(t, rig)
	gates := approvedGate(t, rig, waitRoot)
	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))
	rig.started(waitRoot, changeGeneration, 1)

	firstStep(t, rig.pool, rig.engine, waitRoot)

	if got := gatesOf(t, rig.pool, waitRoot); !reflect.DeepEqual(got, gates) {
		t.Fatalf("the gates after the change's first step = %+v, want them kept, %+v", got, gates)
	}
	for _, kind := range []record.OutboxKind{record.OutboxKindGateSeed, record.OutboxKindDesignGatesPublish} {
		if got := rig.rows(waitRoot, string(kind)); got != 0 {
			t.Fatalf("%s rows = %d, want none: the change asks no person", kind, got)
		}
	}
	if got := rig.issue(waitRoot); got.Phase == phase.Admitted || got.Phase == phase.AwaitingPerson {
		t.Fatalf("%s after the first step stands in %s, want its first phase", waitRoot, got.Phase)
	}
	if _, _, ok := rig.workerStart(waitRoot, changeGeneration); !ok {
		t.Fatalf("%s's change started no worker of its first phase", waitRoot)
	}
}

// ownersPlanRows records the plan the owner of the rig's tree gave issue at generation, whose
// one task walks steps, as register_plan writes it (givenRows).
func ownersPlanRows(t *testing.T, rig *waitRig, issue string, generation uint64, steps ...string) {
	t.Helper()
	fact := ownersPlan(t, issue, generation, 1, givenTask(t, "Build the widget.", steps...))
	version, tasks, planSteps := givenRows(fact, nil, nil, rig.engine.cfg.DefaultSteps, rig.now)
	seedRecord(t, rig.pool, func(tx pgx.Tx) error { return record.NewStore().PutPlan(t.Context(), tx, version, tasks, planSteps) })
}

// planOf is issue's plan of generation as the store holds it, failing where none stands.
func planOf(t *testing.T, rig *waitRig, issue string, generation uint64) *record.StoredPlan {
	t.Helper()
	stored := planStandingOf(t, rig.pool, rig.engine, issue, generation).plan
	if stored == nil {
		t.Fatalf("%s holds no plan at generation %d", issue, generation)
	}
	return stored
}

// Generation 3 ran the owner's plan `implementer reviewer`. Its next change, given no plan, runs
// the project's default steps and not that plan again.
func TestANextChangeRunsTheDefaultStepsAndNotTheLastPlan(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	mergedRoot(t, rig)
	ownersPlanRows(t, rig, waitRoot, waitGeneration, "implementer", "reviewer")
	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))
	rig.started(waitRoot, changeGeneration, 1)

	firstStep(t, rig.pool, rig.engine, waitRoot)

	if stored := planOf(t, rig, waitRoot, changeGeneration); stored.Version.Source != record.PlanSourceDefault {
		t.Fatalf("the change's plan = %+v, want the default steps, not the owner's last plan", stored.Version)
	}
	if got := rig.issue(waitRoot); got.Phase != phase.Planning {
		t.Fatalf("%s's change starts in %s, want planning, the default steps' first", waitRoot, got.Phase)
	}
}

// A plan the owner gives the change before its first step is the plan that step runs.
func TestANextChangeRunsThePlanGivenForIt(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))
	rig.started(waitRoot, changeGeneration, 1)
	mustTake(t, "the change's plan", rig.fact(ownersPlan(t, waitRoot, changeGeneration, 1, givenTask(t, "Fix the widget.", "implementer", "reviewer"))))

	firstStep(t, rig.pool, rig.engine, waitRoot)

	if stored := planOf(t, rig, waitRoot, changeGeneration); stored.Version.Source != record.PlanSourceOwner {
		t.Fatalf("the change's plan = %+v, want the owner's", stored.Version)
	}
	if got := rig.issue(waitRoot); got.Phase != phase.Implementing {
		t.Fatalf("%s's change starts in %s, want implementing, the given plan's first step", waitRoot, got.Phase)
	}
}

// An issue of the experiment's group that runs the default steps runs them on its next change too.
func TestTheExperimentsSecondGroupRunsTheDefaultStepsOnItsNextChange(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	mergedRoot(t, rig)
	drawnInto(t, rig.pool, waitRoot, "planning", "b", "default")
	ownersPlanRows(t, rig, waitRoot, waitGeneration, "implementer", "reviewer")
	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))
	rig.started(waitRoot, changeGeneration, 1)

	firstStep(t, rig.pool, rig.engine, waitRoot)

	if stored := planOf(t, rig, waitRoot, changeGeneration); stored.Version.Source != record.PlanSourceDefault {
		t.Fatalf("the change's plan = %+v, want the default steps", stored.Version)
	}
	if got := rig.issue(waitRoot); got.Phase != phase.Planning {
		t.Fatalf("%s's change starts in %s, want planning", waitRoot, got.Phase)
	}
}

// mergerClaim is the claim of the root's merger, as its start names it.
const mergerClaim = "root-merger"

// owedMerger stands the root's merger at work on its merge: started on its entry, its completion
// not landed.
func owedMerger(t *testing.T, rig *waitRig) {
	t.Helper()
	seedPhase(t, rig.pool, record.PhaseRow{Issue: waitRoot, Role: claim.RoleMerger, Claim: mergerClaim, EntryTask: "Merge pull request #42.", Entry: 1})
}

// A defect the implementer reports in retro starts nothing there, nor at the merge, nor at the
// first read after it while the merger's completion is owed: the change starts at that completion.
func TestADefectReportedInTheRetroStartsNothingBeforeTheMerge(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Retro, "retro")
	seedPhase(t, rig.pool, record.PhaseRow{Issue: waitRoot, Role: claim.RoleImplementer, Claim: "implementer-claim"})
	seedPhase(t, rig.pool, record.PhaseRow{Issue: waitRoot, Role: claim.RoleMerger, Claim: mergerClaim})
	if result := rig.fact(intake.DefectReport{Issue: waitRoot, Role: claim.RoleImplementer, By: "implementer-claim",
		Text: "The retry never stops.", Head: headOne}); result.Refusal != nil || result.Defect == nil {
		t.Fatalf("the report in retro = %+v, want it recorded", result)
	}
	rig.noChange(waitRoot, waitGeneration)

	if result := rig.fact(intake.HandoffComplete{Issue: waitRoot, Role: claim.RoleImplementer, Claim: "implementer-claim", Generation: waitGeneration,
		Summary: "retro done", Commit: headOne}); result.Refusal != nil {
		t.Fatalf("the retro's completion = %+v, want it taken", result.Refusal)
	}
	rig.stands(waitRoot, phase.Merging, "retro")
	rig.noChange(waitRoot, waitGeneration)

	rig.mergeRoot()
	rig.stands(waitRoot, phase.Verifying, "testing")
	rig.noChange(waitRoot, waitGeneration)

	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	rig.stands(waitRoot, phase.Verifying, "testing")
	rig.noChange(waitRoot, waitGeneration)

	if result := rig.fact(intake.HandoffComplete{Issue: waitRoot, Role: claim.RoleMerger, Claim: mergerClaim, Generation: waitGeneration,
		Summary: "merged", Commit: headOne}); result.Refusal != nil {
		t.Fatalf("the merger's completion in verifying = %+v, want it recorded", result.Refusal)
	}
	rig.started(waitRoot, changeGeneration, 1)
	if rows := rig.defectRows(waitRoot); len(rows) != 1 || rows[0].kind != record.DefectKindDefect || rows[0].carriedBy != changeGeneration {
		t.Fatalf("the reasons of %s = %+v, want the reported defect, carried by %d", waitRoot, rows, changeGeneration)
	}
}

// The merger reports a defect in merging and merges. Its completion of the merged generation then
// lands in verifying: it is recorded on the merger's row, refused neither HANDOFF_STALE_GENERATION
// nor HANDOFF_NOT_CURRENT_PHASE, the owed completion is cleared, and the change starts at it. A
// merger that never completes holds the change one hour after the merge, whose read starts it.
func TestTheMergersCompletionLandsBeforeTheNextChange(t *testing.T) {
	t.Parallel()
	setUp := func(t *testing.T) *waitRig {
		t.Helper()
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		owedMerger(t, rig)
		if result := rig.fact(intake.DefectReport{Issue: waitRoot, Role: claim.RoleMerger, By: mergerClaim, Text: "The retry never stops.",
			Head: headOne}); result.Refusal != nil || result.Defect == nil {
			t.Fatalf("the merger's report in merging = %+v, want it recorded", result)
		}
		rig.mergeRoot()
		if got := rig.issue(waitRoot); !got.MergerCompletionOwed {
			t.Fatalf("%s after the merge owes no merger's completion, want it owed", waitRoot)
		}
		return rig
	}

	t.Run("the completion", func(t *testing.T) {
		rig := setUp(t)
		rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		rig.noChange(waitRoot, waitGeneration)

		result := rig.fact(intake.HandoffComplete{Issue: waitRoot, Role: claim.RoleMerger, Claim: mergerClaim, Generation: waitGeneration,
			Summary: "merged", Commit: headOne})
		if result.Refusal != nil {
			t.Fatalf("the merger's completion in verifying = %+v, want it recorded", result.Refusal)
		}
		row := issuePhaseRow(t, rig, waitRoot, claim.RoleMerger)
		if !row.Completed() || row.HandoffCommit != headOne {
			t.Fatalf("the merger's row after its completion = %+v, want it completed at %s", row, headOne)
		}
		got := rig.started(waitRoot, changeGeneration, 1)
		if got.MergerCompletionOwed {
			t.Fatalf("%s still owes the merger's completion after it landed", waitRoot)
		}
		if rows := rig.defectRows(waitRoot); len(rows) != 1 || rows[0].carriedBy != changeGeneration {
			t.Fatalf("the reasons of %s = %+v, want the merger's defect carried by %d", waitRoot, rows, changeGeneration)
		}
	})

	t.Run("no completion, the hour after the merge", func(t *testing.T) {
		rig := setUp(t)
		rig.ranOut(waitRoot, "close_read")
		rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		rig.noChange(waitRoot, waitGeneration)
		reads := issueOutboxOf(t, rig.pool, waitRoot, "close_read")
		if len(reads) != 1 || !reads[0].nextAt.Equal(waitMergedAt.Add(time.Hour)) {
			t.Fatalf("the close_read rows = %+v, want one due an hour after the merge, at %s", reads, waitMergedAt.Add(time.Hour))
		}
		rig.now = waitMergedAt.Add(time.Hour)
		rig.ranOut(waitRoot, "close_read")
		rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		rig.started(waitRoot, changeGeneration, 1)
	})
}

// issuePhaseRow is role's phases row of key.
func issuePhaseRow(t *testing.T, rig *waitRig, key string, role claim.Role) record.PhaseRow {
	t.Helper()
	var row record.PhaseRow
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		rows, err := record.NewStore().Phases(t.Context(), tx, key)
		row = record.PhaseRowOf(rows, role)
		return err
	})
	return row
}

// A defect reported in retro, then the retro's return to implementing for a cause that stays: the
// return carries the defect, so the start task of implementing names it and the defect is carried
// by the generation. That generation's merge marks it merged, so it holds nothing: no next change
// starts for it, and the issue closes when the rest of the close passes.
func TestAReturnCarriesTheDefectsOfItsGeneration(t *testing.T) {
	t.Parallel()
	const text = "The retry never stops past its budget."
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Retro, "retro")
	seedPhase(t, rig.pool, record.PhaseRow{Issue: waitRoot, Role: claim.RoleImplementer, Claim: "implementer-claim"})
	if result := rig.fact(intake.DefectReport{Issue: waitRoot, Role: claim.RoleImplementer, By: "implementer-claim", Text: text,
		Head: headOne}); result.Refusal != nil || result.Defect == nil {
		t.Fatalf("the report in retro = %+v, want it recorded", result)
	}

	if result := rig.fact(intake.BackwardMove{Issue: waitRoot, Requester: claim.RoleImplementer, To: phase.Implementing,
		Reason: "check c-after has no pass at the head"}); result.Refusal != nil {
		t.Fatalf("the retro's return = %+v, want it taken", result.Refusal)
	}
	rig.stands(waitRoot, phase.Implementing, "in_progress")
	rows := rig.defectRows(waitRoot)
	if len(rows) != 1 || rows[0].carriedBy != waitGeneration || rows[0].merged {
		t.Fatalf("the defects after the return = %+v, want the one carried by generation %d", rows, waitGeneration)
	}
	_, start, ok := rig.workerStart(waitRoot, waitGeneration)
	if !ok || start.Role != claim.RoleImplementer || !strings.Contains(start.Task, text) {
		t.Fatalf("the implementer's start after the return = %+v (%t), want its task to name the defect %q", start, ok, text)
	}

	rig.standIn(waitRoot, phase.Merging, "retro")
	rig.mergeRoot()
	rows = rig.defectRows(waitRoot)
	if len(rows) != 1 || rows[0].carriedBy != waitGeneration || !rows[0].merged {
		t.Fatalf("the defects after the merge = %+v, want the one carried by generation %d and merged", rows, waitGeneration)
	}
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	rig.noChange(waitRoot, waitGeneration)
	closedOnChecks(t, rig, waitRoot)
}

// A second fail of the check that started the open change, while that change has not passed its
// last step, joins it: no second reason is recorded and none waits for a change after it. So once
// the change merges, a read that still names that fail starts no change after it.
func TestASecondFailOfTheSameCheckJoinsTheOpenChange(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, failingAt("c-after", waitRootMerge, "result-1")))
	rig.started(waitRoot, changeGeneration, 1)

	rig.readAt(waitRoot, changeGeneration, answerOf(waitRoot, nil, failingAt("c-after", waitRootMerge, "result-1")))

	rows := rig.defectRows(waitRoot)
	if len(rows) != 1 || rows[0].check != "c-after" || rows[0].carriedBy != changeGeneration {
		t.Fatalf("the reasons after the second fail = %+v, want the one of c-after carried by the open change %d", rows, changeGeneration)
	}
	rig.started(waitRoot, changeGeneration, 1)

	rig.changeMerges(waitRoot, 44, waitLaterMerge, headTwo)
	rig.read(waitRoot, answerOf(waitRoot, nil, failingAt("c-after", waitLaterMerge, "result-1")))
	if got := rig.issue(waitRoot); got.Generation != changeGeneration || got.NextChanges != 1 {
		t.Fatalf("%s after its change merged stands at generation %d with %d changes started, want %d and one: the fail joined the change",
			waitRoot, got.Generation, got.NextChanges, changeGeneration)
	}
}

// A reason that comes once the open change has passed its last step waits for the change after
// it: the implementer's defect in the change's retro is carried by no generation until the change
// merges, and then starts the change after it.
func TestAReasonAfterTheLastStepWaitsForTheChangeAfter(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))
	rig.started(waitRoot, changeGeneration, 1)
	rig.standIn(waitRoot, phase.Retro, "retro")
	seedPhase(t, rig.pool, record.PhaseRow{Issue: waitRoot, Role: claim.RoleImplementer, Claim: "implementer-claim"})
	if result := rig.fact(intake.DefectReport{Issue: waitRoot, Role: claim.RoleImplementer, By: "implementer-claim", Text: "The page is blank.",
		Head: headTwo}); result.Refusal != nil || result.Defect == nil {
		t.Fatalf("the report in the change's retro = %+v, want it recorded", result)
	}
	rows := rig.defectRows(waitRoot)
	if len(rows) != 2 || rows[1].kind != record.DefectKindDefect || rows[1].carriedBy != 0 {
		t.Fatalf("the reasons after the report = %+v, want the defect carried by no change yet", rows)
	}

	rig.changeMerges(waitRoot, 44, waitLaterMerge, headTwo)
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitLaterMerge)))

	rig.started(waitRoot, changeGeneration+1, 2)
	if rows := rig.defectRows(waitRoot); len(rows) != 2 || rows[1].carriedBy != changeGeneration+1 {
		t.Fatalf("the reasons after the change merged = %+v, want the defect carried by the change after it, %d", rows, changeGeneration+1)
	}
}

// A change merged, and a read after that merge still names the fail its change carried, by its
// deciding result: that fail adds no reason, and no third generation starts.
func TestAMergedChangesOldFailStartsNoThirdChange(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, failingAt("c-after", waitRootMerge, "result-1")))
	rig.started(waitRoot, changeGeneration, 1)

	rig.changeMerges(waitRoot, 44, waitLaterMerge, headTwo)
	rig.read(waitRoot, answerOf(waitRoot, nil, failingAt("c-after", waitLaterMerge, "result-1")))

	if got := rig.issue(waitRoot); got.Generation != changeGeneration || got.NextChanges != 1 || got.NextChange != 0 {
		t.Fatalf("%s stands at generation %d, %d changes started, next change %d; want %d, one, none open",
			waitRoot, got.Generation, got.NextChanges, got.NextChange, changeGeneration)
	}
	if rows := rig.defectRows(waitRoot); len(rows) != 1 || !rows[0].merged {
		t.Fatalf("the reasons = %+v, want the merged one alone", rows)
	}
}

// newCapWaitRig is newWaitRig with a review round cap of reviewRoundCap.
func newCapWaitRig(t *testing.T, reviewRoundCap int) *waitRig {
	t.Helper()
	rig := &waitRig{t: t, pool: migratedPool(t), now: waitMergedAt.Add(time.Minute)}
	rig.engine = New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour,
		ProductionCheckBound: 48 * time.Hour, ReviewRoundCap: reviewRoundCap, Clock: func() time.Time { return rig.now }}, nil)
	rig.apply = applyFacts(t, rig.pool, rig.engine)
	return rig
}

// With review_round_cap 2, five next changes start one after another, each ending with no pull
// request when its check passes. The second and the fourth each tell the architect and the
// controller once (next-change-count); the first, third and fifth tell neither. No count stops one.
func TestEachMultipleOfTheRoundCapTellsTheCountAndGoesOn(t *testing.T) {
	t.Parallel()
	rig := newCapWaitRig(t, 2)
	mergedRoot(t, rig)
	wantTold := []int{0, 1, 1, 2, 2}
	for i := range 5 {
		generation := uint64(waitGeneration + i)
		rig.readAt(waitRoot, generation, answerOf(waitRoot, nil, failingAt("c-after", waitRootMerge, "result-"+string(rune('a'+i)))))
		rig.started(waitRoot, generation+1, i+1)
		if got := len(noticesOfKind(t, rig.pool, waitRoot, "next-change-count")); got != wantTold[i] {
			t.Fatalf("after change %d the architect's next-change-count notices = %d, want %d", i+1, got, wantTold[i])
		}
		if got := rig.controllerNotices(waitRoot, "next-change-count"); got != wantTold[i] {
			t.Fatalf("after change %d the controller's next-change-count notices = %d, want %d", i+1, got, wantTold[i])
		}
		rig.readAt(waitRoot, generation+1, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
		rig.stands(waitRoot, phase.Verifying, "testing")
	}
}

// A reason that comes while a change is open starts no second generation: the open change has
// passed its last step, so a new check's fail waits for the change after it, and the one hard
// guard (StartNextChange's) holds the start while the change is open.
func TestANextChangeNeverStartsWhileOneIsOpen(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))
	rig.started(waitRoot, changeGeneration, 1)
	rig.standIn(waitRoot, phase.Retro, "retro")

	rig.readAt(waitRoot, changeGeneration, answerOf(waitRoot, nil, failing("c-after", waitRootMerge), failing("c-other", waitRootMerge)))

	if got := rig.issue(waitRoot); got.Generation != changeGeneration || got.NextChange != changeGeneration || got.NextChanges != 1 {
		t.Fatalf("%s stands at generation %d, next change %d of %d started; want the one change %d still open",
			waitRoot, got.Generation, got.NextChange, got.NextChanges, changeGeneration)
	}
	rows := rig.defectRows(waitRoot)
	if len(rows) != 2 || rows[1].check != "c-other" || rows[1].carriedBy != 0 {
		t.Fatalf("the reasons = %+v, want c-other's waiting for the change after the open one", rows)
	}
}

// A change at planning, a read of its generation in which its check passes: the change ends with no
// pull request, back in verifying with the status testing, and close-wait.md says it ended. A
// production check of the change's generation opens at the retained merge, whatever the reasons,
// and the issue closes only at that check's pass.
func TestAChangeWithNoPullRequestEndsWhenItsCheckPasses(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))
	rig.started(waitRoot, changeGeneration, 1)
	firstStep(t, rig.pool, rig.engine, waitRoot)
	rig.stands(waitRoot, phase.Planning, "in_progress")

	rig.readAt(waitRoot, changeGeneration, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))

	rig.stands(waitRoot, phase.Verifying, "testing")
	if got := rig.issue(waitRoot); got.Generation != changeGeneration || got.NextChange != 0 {
		t.Fatalf("%s after the end stands at generation %d with change %d open, want %d with none", waitRoot, got.Generation, got.NextChange, changeGeneration)
	}
	if ended := rig.stored(waitRoot).Ended; ended == nil || ended.Generation != changeGeneration || ended.PullRequest != 0 {
		t.Fatalf("the stored wait's end = %+v, want change %d ended with no pull request", ended, changeGeneration)
	}
	check := rig.check(waitRoot)
	if !check.Open() || check.Generation != changeGeneration || check.MergeSHA != waitRootMerge {
		t.Fatalf("the production check after the end = %+v, want one of generation %d open at %s", check, changeGeneration, waitRootMerge)
	}
	rig.readAt(waitRoot, changeGeneration, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	rig.stands(waitRoot, phase.Verifying, "testing")

	rig.report(waitRoot, 1, "pass", "production carries the fix", nil)
	rig.readAt(waitRoot, changeGeneration, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	closedOnChecks(t, rig, waitRoot)
}

// A next change starts on a reported defect, whose owner withdraws it, and the change ends with no
// pull request. One new production check of the change's generation opens at the retained merge,
// whatever the earlier check's outcome: none, open (which the start closed not_run), recorded,
// not_run or defect. The new check replaces the earlier one's row, and only its pass closes the
// issue.
func TestAReopenedChangeUsesItsOwnProductionAssessment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		seed    bool
		outcome record.CheckOutcome
	}{
		{name: "none"},
		{name: "open", seed: true},
		{name: "recorded", seed: true, outcome: record.CheckRecorded},
		{name: "not_run", seed: true, outcome: record.CheckNotRun},
		{name: "defect", seed: true, outcome: record.CheckDefect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newWaitRig(t, claim.RoleTester)
			mergedRoot(t, rig)
			if tc.seed {
				rig.seedCheck(waitRoot, waitGeneration, waitRootMerge, tc.outcome)
			}
			defect := rig.seedDefect(waitRoot, "The page is blank after a save.")
			rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
			rig.started(waitRoot, changeGeneration, 1)
			if tc.seed && tc.outcome == "" {
				if earlier := rig.check(waitRoot); earlier.Open() || earlier.Outcome != record.CheckNotRun {
					t.Fatalf("the earlier check after the start = %+v, want it closed not_run", earlier)
				}
			}

			rig.withdraw(waitRoot, defect, "same as the merged fix")

			rig.stands(waitRoot, phase.Verifying, "testing")
			fresh := rig.check(waitRoot)
			if !fresh.Open() || fresh.Generation != changeGeneration || fresh.MergeSHA != waitRootMerge || fresh.Looks != 1 {
				t.Fatalf("the production check after the end = %+v, want one of generation %d at look 1 open at %s", fresh, changeGeneration, waitRootMerge)
			}
			rig.readAt(waitRoot, changeGeneration, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
			rig.stands(waitRoot, phase.Verifying, "testing")
			rig.report(waitRoot, 1, "pass", "production carries the fix", nil)
			rig.readAt(waitRoot, changeGeneration, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
			closedOnChecks(t, rig, waitRoot)
		})
	}
}

// The earlier check and the change's own each stood at look 1. A look's end and a wake of the
// earlier check, each naming it by the moment it opened, change nothing of the change's check, and
// neither does a legacy fact that names no check. (A report is fenced by the route, which names
// the check it reports by checkOpenedAt: api's TestHandoffCompleteNamesTheCheckItReports.)
func TestAnOldProductionReportCannotFinishTheNewAssessment(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	earlier := rig.check(waitRoot)
	if earlier.Looks != 1 || !earlier.LookOpen || earlier.Generation != waitGeneration {
		t.Fatalf("the earlier check = %+v, want generation %d's look 1 open", earlier, waitGeneration)
	}
	defect := rig.seedDefect(waitRoot, "The page is blank after a save.")
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	rig.started(waitRoot, changeGeneration, 1)
	rig.withdraw(waitRoot, defect, "same as the merged fix")
	fresh := rig.check(waitRoot)
	if fresh.Generation != changeGeneration || fresh.Looks != 1 || !fresh.LookOpen || fresh.OpenedAt.Equal(earlier.OpenedAt) {
		t.Fatalf("the change's check = %+v, want generation %d's own look 1 open", fresh, changeGeneration)
	}

	rig.fact(intake.CheckLookEnded{Issue: waitRoot, Look: 1, Cause: "look bound", OpenedAt: earlier.OpenedAt})
	rig.fact(intake.CheckWakeDue{Issue: waitRoot, Look: 1, OpenedAt: earlier.OpenedAt, Reason: "the earlier look's wake"})
	rig.fact(intake.CheckLookEnded{Issue: waitRoot, Look: 1, Cause: "look bound"})

	if got := rig.check(waitRoot); !reflect.DeepEqual(got, fresh) {
		t.Fatalf("the change's check after the earlier check's facts = %+v, want it as it stood, %+v", got, fresh)
	}
}

// A change whose pull request is open: a read of its generation in which each check passes, the
// production check of its generation recorded at the newest merge, nothing beneath, and the
// change's own check reason answered by its passing check. The issue stays in the change.
func TestAPassWhileTheNextChangeIsOpenClosesNothing(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	mergedRoot(t, rig)
	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))
	rig.started(waitRoot, changeGeneration, 1)
	rig.standIn(waitRoot, phase.Reviewing, "needs_review")
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: waitRoot, Repo: waitRepo, Number: 44, Branch: claim.IssueBranch(waitRoot),
		Base: "main", HeadSHA: headTwo, Generation: changeGeneration, Failing: []string{}})
	rig.seedCheck(waitRoot, changeGeneration, waitRootMerge, record.CheckRecorded)

	rig.readAt(waitRoot, changeGeneration, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))

	got := rig.issue(waitRoot)
	if got.Phase != phase.Reviewing || got.Generation != changeGeneration || got.NextChange != changeGeneration {
		t.Fatalf("%s stands in %s at generation %d with change %d open, want the change %d still in reviewing",
			waitRoot, got.Phase, got.Generation, got.NextChange, changeGeneration)
	}
	if rows := rig.defectRows(waitRoot); len(rows) != 1 || rows[0].merged || rows[0].carriedBy != changeGeneration {
		t.Fatalf("the reasons = %+v, want the change's own still open", rows)
	}
	if got := rig.statusWrites(waitRoot, "done"); got != 0 {
		t.Fatalf("done status writes = %d, want none while the change is open", got)
	}
}

// The child's next change writes todo on its record and to Dispatch, so Dispatch's echo of that
// write carries the record's own status: it re-enters nothing, and the generation and the gate stand
// as StartNextChange wrote them.
func TestTheEchoOfANextChangesTodoReentersNothing(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	mergedChild(t, rig)
	approvedGate(t, rig, waitChild)
	rig.read(waitChild, answerOf(waitChild, nil, failing("c-after", waitChildMerge)))
	started := rig.started(waitChild, changeGeneration, 1)
	gates := gatesOf(t, rig.pool, waitChild)

	rig.fact(intake.DispatchIssue{Key: waitChild, Seq: started.LastDispatchSeq + 1, Type: "issue.updated", Status: "todo", Title: started.Title,
		Parent: waitRoot, Rank: started.Rank, ResolvedRepo: waitRepo, DaemonActor: true, ClaimKnown: true})

	rig.started(waitChild, changeGeneration, 1)
	if got := gatesOf(t, rig.pool, waitChild); !reflect.DeepEqual(got, gates) {
		t.Fatalf("%s's gates after the echo = %+v, want them as the start left them, %+v", waitChild, got, gates)
	}
	if got := noticesOfKind(t, rig.pool, waitChild, "child-status"); len(got) != 0 {
		t.Fatalf("child-status notices after the echo = %+v, want none: nothing re-entered", got)
	}
}

// openStore is the daemon's store over the rig's pool, for the reads of a tree's fences.
func openStore(t *testing.T, rig *waitRig) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), rig.pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store over the test pool: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// putMemberClaim records key's architect claim in the rig's tree at the rig's generation, in state.
func putMemberClaim(t *testing.T, st *store.Store, key string, state supervise.ClaimState, by supervise.SuspendSource) {
	t.Helper()
	token, err := claim.NewToken("legion", key, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutClaim(t.Context(), supervise.Claim{Token: token, Project: "legion", Tree: waitRoot, Issue: key, Role: claim.RoleArchitect,
		TreeEpoch: waitGeneration, Generation: waitGeneration, State: state, SuspendedBy: by}); err != nil {
		t.Fatalf("record the claim of %s: %v", key, err)
	}
}

// creditsCounted is how many of the design-news items of key told to audience at the rig's
// generation in the tree's run treeRun still count (design_news_credits).
func creditsCounted(t *testing.T, rig *waitRig, key string, audience record.DesignAudience, treeRun uint64) int {
	t.Helper()
	carried, err := json.Marshal(map[string]any{"items": []map[string]any{{"issue": key, "audience": string(audience), "step": string(record.DesignStepSpec),
		"from": 1, "to": 2, "generation": waitGeneration, "tree_generation": treeRun}}})
	if err != nil {
		t.Fatal(err)
	}
	var counted int
	if err := rig.pool.QueryRow(t.Context(), `select count(*) from design_news_credits($1::jsonb)`, carried).Scan(&counted); err != nil {
		t.Fatalf("read the credits of %s: %v", key, err)
	}
	return counted
}

// The workflow's half of the fences of a tree's run: a root's next change, through the judge and
// StartNextChange, raises the root's own generation and not its tree's. A member's queued idle
// release still tears its pod down, the suspension of a member that left done still acts, and a
// member's design-news credit still counts, each read before the change too.
func TestARootsNextChangeKeepsEachMembersFences(t *testing.T) {
	t.Parallel()
	const leftDone = "LEGION-210"
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	rig.seedChild(waitChild, phase.Implementing, "in_progress")
	rig.seedChild(leftDone, phase.Done, "done")
	st := openStore(t, rig)
	if _, err := rig.pool.Exec(t.Context(), `insert into issue_releases
		(project, issue, generation, first_row, boundary_row, last_row, started_row, role, suspended, marked_at)
		values ('legion', $1, $2, 5, 6, 10, 0, 'architect', '{}', now())`, waitChild, waitGeneration); err != nil {
		t.Fatalf("mark the release of %s: %v", waitChild, err)
	}
	putMemberClaim(t, st, waitChild, supervise.StateSuspended, supervise.SuspendedByReleased)
	putMemberClaim(t, st, leftDone, supervise.StateSuspended, supervise.SuspendedByDaemon)
	rig.mergeRoot()
	treeRun := rig.issue(waitRoot).TreeRun()

	fences := func(when string) {
		t.Helper()
		release, err := st.IssueRelease(t.Context(), "legion", store.IssueReleased{Issue: waitChild, Tree: waitRoot, IssueGeneration: waitGeneration,
			TreeGeneration: treeRun, Row: 10}, record.OutOfWorkflow)
		if err != nil || release.Act != store.ReleaseActs {
			t.Errorf("%s: the idle release of %s = %s (%s), %v; want it to tear the pod down", when, waitChild, release.Act, release.Cause, err)
		}
		act, err := st.IssueSuspension(t.Context(), "legion", store.IssueClose{Issue: leftDone, Tree: waitRoot, IssueGeneration: waitGeneration,
			TreeGeneration: treeRun, Row: 10}, record.OutOfWorkflow)
		if err != nil || !act {
			t.Errorf("%s: the suspension of %s = %t, %v; want it to act", when, leftDone, act, err)
		}
		if counted := creditsCounted(t, rig, waitChild, record.AudienceHolder, treeRun); counted != 1 {
			t.Errorf("%s: %d credits of %s's holder count, want its one", when, counted, waitChild)
		}
	}
	fences("before the root's next change")
	if t.Failed() {
		t.FailNow()
	}

	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))

	root := rig.started(waitRoot, changeGeneration, 1)
	if root.TreeRun() != treeRun {
		t.Errorf("the tree's run after the root's next change = %d, want %d", root.TreeRun(), treeRun)
	}
	fences("after the root's next change")
}

// A root in verifying whose tree's architect holds a pending task it has not confirmed, and a gate
// request in flight: its approval ask open and its catch-up fact not yet applied. A computed fail
// starts the root's next change. The pending task stands (ArchitectStartPending answers that the
// architect has not taken it), the gate request's catch-up fact at the tree's run is taken, no
// architect start is queued, and one next-change notice reaches the architect.
func TestARootsNextChangeLeavesItsArchitectsRunStanding(t *testing.T) {
	t.Parallel()
	const entry, ask = 2, "ask-208"
	rig := newWaitRig(t, "")
	mergedRoot(t, rig)
	seedGate(t, rig.pool, record.DesignGate{Issue: waitRoot, Step: record.DesignStepSpec, ArtifactID: "spec-208", LatestVersion: 2, ApprovedVersion: new(2)})
	seedPhase(t, rig.pool, record.PhaseRow{Issue: waitRoot, Role: claim.RoleArchitect, Entry: entry, EntryTask: "Work the tree."})
	treeRun := rig.issue(waitRoot).TreeRun()
	pending := supervise.Delivery{ID: "outbox:7", Task: "Work the tree.", Generation: treeRun, Entry: entry}
	run := record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateIdle, Tree: waitRoot, GivenEntry: entry - 1, Pending: &pending}}

	rig.read(waitRoot, answerOf(waitRoot, nil, failing("c-after", waitRootMerge)))

	root := rig.started(waitRoot, changeGeneration, 1)
	row := issuePhaseRow(t, rig, waitRoot, claim.RoleArchitect)
	if row.Entry != entry || row.EntryTask == "" {
		t.Fatalf("the tree's architect's row after the change = %+v, want its entry %d kept", row, entry)
	}
	if reason, waits := ArchitectStartPending(root, row.Entry, run); !waits {
		t.Fatalf("ArchitectStartPending after the root's next change = %t (%q), want the pending task standing", waits, reason)
	}
	if got := rig.architectStarts(waitRoot); got != 0 {
		t.Fatalf("architect starts after the change = %d, want none", got)
	}
	if got := noticesOfKind(t, rig.pool, waitRoot, "next-change"); len(got) != 1 {
		t.Fatalf("next-change notices = %+v, want one", got)
	}

	rig.fact(intake.GateRequestFound{Issue: waitRoot, Generation: treeRun, ArtifactID: "spec-208", RequestAsk: ask, Since: 2})
	var mirrored *record.RootAsk
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		var err error
		mirrored, err = record.NewStore().RootAsk(t.Context(), tx, waitRoot, ask)
		return err
	})
	if mirrored == nil {
		t.Fatalf("the gate request's catch-up fact at the tree's run %d mirrored nothing, want it taken", treeRun)
	}
}
