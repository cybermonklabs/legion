package admit

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The tests of a reason that a re-admission left carried (record.CarriedNow), each reached through
// the doors that re-admit an issue, so admission's readmit runs: a person's backlog and todo of a
// root whose work never merged, and the hand-over of a child whose tree closed while its next change
// was open. Each re-admission raises the issue's generation past the change that carried the reason,
// and that change never merged it.

const (
	// rcRoot is the root of each case, and rcChild the child the check's case hands over.
	rcRoot  = "LEGION-880"
	rcChild = "LEGION-881"
	// rcDefect is the defect the implementer reports in the root's retro.
	rcDefect = "The retry never stops past its budget."
	// rcHead is the head of the pull request a role saw the defect at, 40 lowercase hex as the record
	// requires of a reported defect.
	rcHead = "4d2f6a0c8e1b3d5f7a9c0e2b4d6f8a1c3e5b7d90"
)

// rcApply applies fact under id through the workflow's handler and admission's, as the daemon
// does, and answers its result.
func rcApply(t *testing.T, pool *pgxpool.Pool, admission *Admission, id string, fact intake.Fact) intake.Result {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), pool, "test", id, fact, admission.engine, admission)
	if err != nil {
		t.Fatalf("apply %s: %v", id, err)
	}
	return result
}

// rcMove is Dispatch's move of key to status at seq under parent ("" for a root), carrying the
// label: a person's event where byPerson is set, and the daemon's own write otherwise.
func rcMove(key, parent, status string, seq int64, byPerson bool) intake.DispatchIssue {
	fact := intake.DispatchIssue{Key: key, Seq: seq, Type: "issue.updated", Status: status, Title: key, Parent: parent, Rank: "B",
		HandedOver: true, ResolvedRepo: "acme/widgets"}
	if status == record.DoneStatus {
		fact.Type = "issue.closed"
	}
	if byPerson {
		fact.PersonActor = "a-person"
	} else {
		fact.DaemonActor = true
	}
	return fact
}

// rcMergeOf is the merge commit GitHub names for pull request number.
func rcMergeOf(number int) string { return "merge-" + strconv.Itoa(number) }

// rcPullRequest records key's open pull request number from its branch into main, at head-<number>.
func rcPullRequest(t *testing.T, pool *pgxpool.Pool, key string, number int) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{State: record.PullRequestOpen, Issue: key,
			Repo: "acme/widgets", Number: number, Branch: claim.IssueBranch(key), Base: "main", HeadSHA: "head-" + strconv.Itoa(number),
			Failing: []string{}}); err != nil {
			t.Fatalf("record the pull request #%d of %s: %v", number, key, err)
		}
	})
}

// rcMerging stands key's record in merging at the generation it holds, on its open pull request
// number: the re-run of a re-admission reaching its merge, which these tests do not walk step by
// step.
func rcMerging(t *testing.T, pool *pgxpool.Pool, key string, number int) {
	t.Helper()
	merging := issue(t, pool, key)
	merging.Phase, merging.Status, merging.DispatchStatus = phase.Merging, "retro", "retro"
	putIssue(t, pool, merging)
	rcPullRequest(t, pool, key, number)
}

// rcMerge is GitHub's merge of key's pull request number, which stands key in verifying.
func rcMerge(t *testing.T, pool *pgxpool.Pool, admission *Admission, key string, number int) {
	t.Helper()
	rcApply(t, pool, admission, key+"-merged-"+strconv.Itoa(number), intake.PullRequestMerged{Repo: "acme/widgets", Number: number,
		MergeSHA: rcMergeOf(number), HeadSHA: "head-" + strconv.Itoa(number)})
	if got := issue(t, pool, key); got.Phase != phase.Verifying {
		t.Fatalf("%s after the merge of #%d stands in %s, want verifying", key, number, got.Phase)
	}
}

// rcCheck is check c-after of key's Verification document as the results read answers it at the
// merge of pull request number: passing where result is "", and failing with result as its deciding
// result otherwise.
func rcCheck(key string, number int, result string) dispatch.CheckBlock {
	block := dispatch.CheckBlock{BlockID: "c-after", ArtifactID: "verification-" + key, When: dispatch.CheckAfterMerge,
		ComparedWith: []dispatch.CheckBasis{{Kind: dispatch.BasisBranch, Branch: "main", MergeCommit: rcMergeOf(number)}},
		PassStands:   dispatch.PassStands{Answer: dispatch.PassStandsYes}}
	if result != "" {
		block.PassStands = dispatch.PassStands{Answer: dispatch.PassStandsNo, Reason: dispatch.PassStandsReasonFail, Results: []string{result},
			Commit: rcMergeOf(number)}
	}
	return block
}

// rcRead is the outbox's read of key's checks at generation coming back with check, begun after
// key's newest entry into verifying, which the fixed clock stands at fixedNow: at is how long after.
func rcRead(t *testing.T, pool *pgxpool.Pool, admission *Admission, key string, generation uint64, at time.Duration, check dispatch.CheckBlock) {
	t.Helper()
	answer := record.CheckReadAnswer{Results: dispatch.IssueResults{Issue: key, VerificationArtifactID: check.ArtifactID,
		Checks: []dispatch.CheckBlock{check}}}
	rcApply(t, pool, admission, key+"-read-"+strconv.FormatUint(generation, 10), intake.CloseChecksRead{Issue: key, Generation: generation,
		ReadAt: fixedNow.Add(at), Answer: &answer})
}

// rcReason is one row of defects as these tests read it.
type rcReason struct {
	generation int64
	kind       record.DefectKind
	result     string
	carriedBy  int64
	open       bool
}

// rcReasons is every defect of key, oldest first, whatever its state.
func rcReasons(t *testing.T, pool *pgxpool.Pool, key string) []rcReason {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select generation, kind, result_id, coalesce(carried_by, 0),
		merged_at is null and withdrawn_at is null from defects where issue = $1 order by id`, key)
	if err != nil {
		t.Fatalf("read the defects of %s: %v", key, err)
	}
	defer rows.Close()
	var got []rcReason
	for rows.Next() {
		var row rcReason
		if err := rows.Scan(&row.generation, &row.kind, &row.result, &row.carriedBy, &row.open); err != nil {
			t.Fatalf("scan a defect of %s: %v", key, err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the defects of %s: %v", key, err)
	}
	return got
}

// rcChanged fails the test unless key stands at generation on its open next change, the count-th
// it has started.
func rcChanged(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, count int) {
	t.Helper()
	if got := issue(t, pool, key); got.Generation != generation || got.NextChange != generation || got.NextChanges != count {
		t.Fatalf("%s stands at generation %d with change %d open, %d started; want its next change %d open, %d started", key,
			got.Generation, got.NextChange, got.NextChanges, generation, count)
	}
}

// A defect a re-admission left carried starts the next change after the re-run's merge. The
// implementer reports a defect in the root's retro, and the retro's return carries it by the
// generation (workflow's carryReturn). A person then moves the root, whose work never merged, to
// backlog and back to todo, and the re-admission runs it again at the next generation with the
// defect still carried by the generation it ended: readmit writes no defect. The re-run merges, and
// the first read after the merge starts the next change carrying the defect, since a carry of a
// generation that never merged it ends nothing. Read as carried while `carried_by` was set at all,
// the defect was no reason and still held the close as an open defect, so the issue waited in
// verifying with no change started and none to come. A report of the same defect in the re-run's
// merging answers it with that next change (workflow's defectReport).
func TestADefectAReadmissionLeftCarriedStartsTheNextChange(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	seedSlottedInPhase(t, pool, testProject, rcRoot, "B", phase.Retro, func(root *record.Issue) {
		root.Status, root.DispatchStatus, root.LastDispatchSeq = "retro", "retro", 1
	})
	rcPullRequest(t, pool, rcRoot, 21)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: rcRoot, Role: claim.RoleImplementer, Claim: "implementer-claim"}); err != nil {
			t.Fatalf("seed the implementer's row of %s: %v", rcRoot, err)
		}
	})
	reported := rcApply(t, pool, admission, "rc-report", intake.DefectReport{Issue: rcRoot, Role: claim.RoleImplementer, By: "implementer-claim",
		Text: rcDefect, Head: rcHead})
	if reported.Refusal != nil || reported.Defect == nil {
		t.Fatalf("the report in retro = %+v, want it recorded", reported)
	}
	if result := rcApply(t, pool, admission, "rc-return", intake.BackwardMove{Issue: rcRoot, Requester: claim.RoleImplementer, To: phase.Implementing,
		Reason: "check c-after has no pass at the head"}); result.Refusal != nil {
		t.Fatalf("the retro's return = %+v, want it taken", result.Refusal)
	}

	apply(t, pool, admission, "rc-backlog", rcMove(rcRoot, "", "backlog", 2, true), admission.engine)
	if got := issue(t, pool, rcRoot); !got.Lingers() || got.Status != "backlog" {
		t.Fatalf("%s after a person's backlog stands in %s with status %s, lingering %t; want it lingering in backlog", rcRoot, got.Phase, got.Status,
			got.Lingers())
	}
	apply(t, pool, admission, "rc-todo", rcMove(rcRoot, "", "todo", 3, true), admission.engine)
	if got := issue(t, pool, rcRoot); got.Generation != 2 || got.Phase != phase.Admitted || got.NextChange != 0 || got.NextChanges != 0 {
		t.Fatalf("%s after a person's todo = generation %d in %s, change %d of %d; want the re-admission's run at generation 2 at admitted, no change",
			rcRoot, got.Generation, got.Phase, got.NextChange, got.NextChanges)
	}
	if rows := rcReasons(t, pool, rcRoot); len(rows) != 1 || rows[0].kind != record.DefectKindDefect || rows[0].generation != 1 ||
		rows[0].carriedBy != 1 || !rows[0].open {
		t.Fatalf("the defects of %s after the re-admission = %+v, want the reported one open, carried by the generation it ended, 1", rcRoot, rows)
	}

	rcMerging(t, pool, rcRoot, 22)
	again := rcApply(t, pool, admission, "rc-report-again", intake.DefectReport{Issue: rcRoot, Role: claim.RoleMerger, By: "merger-claim",
		Text: rcDefect, Head: rcHead})
	if again.Refusal != nil || again.Defect == nil {
		t.Fatalf("the merger's report of the same defect = %+v, want it answered", again)
	}
	if answer := *again.Defect; answer.State != intake.DefectSame || answer.Defect != reported.Defect.Defect || answer.NextChange != 3 {
		t.Errorf("the merger's report of the same defect = %+v, want defect %d answered the same, carried by the next change 3", answer,
			reported.Defect.Defect)
	}
	rcMerge(t, pool, admission, rcRoot, 22)
	rcRead(t, pool, admission, rcRoot, 2, time.Second, rcCheck(rcRoot, 22, ""))

	rcChanged(t, pool, rcRoot, 3, 1)
	if rows := rcReasons(t, pool, rcRoot); len(rows) != 1 || rows[0].carriedBy != 3 || !rows[0].open {
		t.Fatalf("the defects of %s = %+v, want the left defect carried by the next change, 3", rcRoot, rows)
	}
}

// A check's reason that a re-admission left carried is no reason again, and a fail of its check
// after the re-run's merge, by a new deciding result, is one new reason. The child of a running
// root merges and its check fails, which starts the child's next change carrying that fail. A
// person then parks and closes the root, and the end of the root's linger hands the child, its
// change still open, to Legion as a root of its own: the hand-over's todo re-admits it at the next
// generation. The re-run merges and the check fails again: the fail is recorded once, and the next
// change carries it alone, while the old reason stays with the change that carried it. Read as a
// reason again once its carrying generation was left behind, the old fail stood for the new one
// (recordCheckReasons took the check as already a reason, so nothing was recorded) and the next
// change carried the old fail, whose deciding result decides nothing any more.
func TestACheckReasonAReadmissionLeftCarriedIsNoReasonAgain(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	seedSlottedInPhase(t, pool, testProject, rcRoot, "B", phase.Implementing, func(root *record.Issue) {
		root.DispatchStatus, root.LastDispatchSeq = "in_progress", 1
	})
	parent := rcRoot
	putIssue(t, pool, record.Issue{Key: rcChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: rcChild, Tree: rcRoot, Parent: &parent, Phase: phase.Merging, Generation: 1, Status: "retro", DispatchStatus: "retro", Rank: "B1",
		LastDispatchSeq: 1})
	rcPullRequest(t, pool, rcChild, 31)
	rcMerge(t, pool, admission, rcChild, 31)
	rcRead(t, pool, admission, rcChild, 1, time.Second, rcCheck(rcChild, 31, "result-1"))
	rcChanged(t, pool, rcChild, 2, 1)
	if rows := rcReasons(t, pool, rcChild); len(rows) != 1 || rows[0].kind != record.DefectKindCheck || rows[0].result != "result-1" || rows[0].carriedBy != 2 {
		t.Fatalf("the reasons of %s = %+v, want c-after's fail by result-1, carried by its change 2", rcChild, rows)
	}

	apply(t, pool, admission, "rc-root-backlog", rcMove(rcRoot, "", "backlog", 2, true), admission.engine)
	apply(t, pool, admission, "rc-root-done", rcMove(rcRoot, "", record.DoneStatus, 3, true), admission.engine)
	rcApply(t, pool, admission, "rc-linger", intake.LingerExpired{Issue: rcRoot, Generation: 1})
	apply(t, pool, admission, "rc-child-handed-over", rcMove(rcChild, rcRoot, "todo", 10, false), admission.engine)
	if got := issue(t, pool, rcChild); got.Tree != rcChild || got.Generation != 3 || got.NextChange != 0 {
		t.Fatalf("%s after its hand-over = tree %s at generation %d with change %d open; want its own tree at generation 3 with no change",
			rcChild, got.Tree, got.Generation, got.NextChange)
	}

	rcMerging(t, pool, rcChild, 32)
	rcMerge(t, pool, admission, rcChild, 32)
	rcRead(t, pool, admission, rcChild, 3, 2*time.Second, rcCheck(rcChild, 32, "result-2"))

	rcChanged(t, pool, rcChild, 4, 2)
	rows := rcReasons(t, pool, rcChild)
	if len(rows) != 2 || rows[0].result != "result-1" || rows[0].carriedBy != 2 || !rows[0].open ||
		rows[1].kind != record.DefectKindCheck || rows[1].generation != 3 || rows[1].result != "result-2" || rows[1].carriedBy != 4 {
		t.Fatalf("the reasons of %s = %+v, want the old fail by result-1 still with change 2 and one new reason, by result-2, carried by the next change 4",
			rcChild, rows)
	}
}
