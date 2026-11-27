package workflow

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The tree the tests of a merged issue's close drive: the root LEGION-208, its child LEGION-209,
// their pull requests #42 and #43 on sjawhar/legion, and the merge commits GitHub names for them.
const (
	waitRoot        = "LEGION-208"
	waitChild       = "LEGION-209"
	waitRepo        = "sjawhar/legion"
	waitDocument    = "verification-208"
	waitRootMerge   = "4444444444444444444444444444444444444444"
	waitChildMerge  = "5555555555555555555555555555555555555555"
	waitLaterMerge  = "6666666666666666666666666666666666666666"
	waitGeneration  = 3
	waitRootNumber  = 42
	waitChildNumber = 43
)

// waitMergedAt is GitHub's merged_at of the merges the tests make.
var waitMergedAt = time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)

// waitRig is the workflow's store rig for a merged issue's close: a pool carrying every migration,
// an engine whose clock the test moves, and apply, which applies a fact as the daemon does and
// fails the test on an error. role is the project's production check, "" for none.
type waitRig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	now    time.Time
	engine *Engine
	apply  func(id string, fact intake.Fact) intake.Result
	facts  int
}

func newWaitRig(t *testing.T, role claim.Role) *waitRig {
	t.Helper()
	rig := &waitRig{t: t, pool: migratedPool(t), now: waitMergedAt.Add(time.Minute)}
	rig.engine = New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour,
		ProductionCheckBound: 48 * time.Hour, ProductionCheckRole: role, Clock: func() time.Time { return rig.now }}, nil)
	rig.apply = applyFacts(t, rig.pool, rig.engine)
	return rig
}

// fact applies fact under an id of its own, after the clock moved a second.
func (r *waitRig) fact(fact intake.Fact) intake.Result {
	r.t.Helper()
	r.facts++
	r.now = r.now.Add(time.Second)
	return r.apply(fmt.Sprintf("wait-fact-%d", r.facts), fact)
}

// seedRoot records the root standing at at with status, in a tree that runs, with its merger's row
// and its open pull request #42 from legion/LEGION-208 into main at headOne.
func (r *waitRig) seedRoot(at phase.Phase, status string) {
	r.t.Helper()
	seedIssue(r.t, r.pool, record.Issue{Key: waitRoot, Repo: waitRepo, DefaultBranch: "main", GitHubRepo: waitRepo, Tree: waitRoot, Project: "LEGION",
		Title: "root", Phase: at, Generation: waitGeneration, Status: status, DispatchStatus: status, Rank: "U", LastDispatchSeq: 1})
	seedTreeLifecycle(r.t, r.pool, waitRoot)
	seedPhase(r.t, r.pool, record.PhaseRow{Issue: waitRoot, Role: claim.RoleMerger, Claim: "root-merger", HandoffCommit: headOne})
	seedPR(r.t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: waitRoot, Repo: waitRepo, Number: waitRootNumber,
		Branch: "legion/" + waitRoot, Base: "main", HeadSHA: headOne, Failing: []string{}})
}

// seedChild records key, a child of the root standing at at with status. A child in merging gets
// its merger's row and its open pull request #43 at headTwo.
func (r *waitRig) seedChild(key string, at phase.Phase, status string) {
	r.t.Helper()
	parent := waitRoot
	seedIssue(r.t, r.pool, record.Issue{Key: key, Repo: waitRepo, DefaultBranch: "main", GitHubRepo: waitRepo, Tree: waitRoot, Project: "LEGION",
		Title: key, Parent: &parent, Phase: at, Generation: waitGeneration, Status: status, DispatchStatus: status, Rank: "V", LastDispatchSeq: 1})
	if at == phase.Merging {
		seedPhase(r.t, r.pool, record.PhaseRow{Issue: key, Role: claim.RoleMerger, Claim: "child-merger", HandoffCommit: headTwo})
		seedPR(r.t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: waitRepo, Number: waitChildNumber,
			Branch: "legion/" + key, Base: "main", HeadSHA: headTwo, Failing: []string{}})
	}
}

// mergeRoot is GitHub's merge of the root's pull request at waitRootMerge.
func (r *waitRig) mergeRoot() {
	r.t.Helper()
	r.fact(intake.PullRequestMerged{Repo: waitRepo, Number: waitRootNumber, MergeSHA: waitRootMerge, HeadSHA: headOne, MergedAt: waitMergedAt})
}

// mergeChild is GitHub's merge of the child's pull request at waitChildMerge.
func (r *waitRig) mergeChild() {
	r.t.Helper()
	r.fact(intake.PullRequestMerged{Repo: waitRepo, Number: waitChildNumber, MergeSHA: waitChildMerge, HeadSHA: headTwo, MergedAt: waitMergedAt})
}

// read is the outbox's read of issue's checks coming back (intake.CloseChecksRead), begun a minute
// after the clock stood.
func (r *waitRig) read(issue string, answer record.CheckReadAnswer) {
	r.t.Helper()
	r.now = r.now.Add(time.Minute)
	r.fact(intake.CloseChecksRead{Issue: issue, Generation: waitGeneration, ReadAt: r.now, Answer: &answer})
}

// ranOut deletes the issue's rows of kind, as their executor finishing them does.
func (r *waitRig) ranOut(issue, kind string) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.t.Context(), "delete from outbox where issue = $1 and kind = $2", issue, kind); err != nil {
		r.t.Fatalf("finish %s's %s rows: %v", issue, kind, err)
	}
}

// issue is key's record as the store holds it.
func (r *waitRig) issue(key string) record.Issue {
	r.t.Helper()
	var issue *record.Issue
	seedRecord(r.t, r.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = record.NewStore().Issue(r.t.Context(), tx, key)
		return err
	})
	if issue == nil {
		r.t.Fatalf("%s is not recorded", key)
	}
	return *issue
}

// stored is key's stored wait (issues.close_wait).
func (r *waitRig) stored(key string) record.StoredWait {
	r.t.Helper()
	stored, err := storedWaitOf(r.issue(key))
	if err != nil {
		r.t.Fatal(err)
	}
	return stored
}

// stands fails the test unless key stands at want with the record's status status.
func (r *waitRig) stands(key string, want phase.Phase, status string) {
	r.t.Helper()
	issue := r.issue(key)
	if issue.Phase != want || issue.Status != status {
		r.t.Fatalf("%s stands at %s with status %s, want %s with %s", key, issue.Phase, issue.Status, want, status)
	}
}

// rows is the number of issue's outbox rows of kind.
func (r *waitRig) rows(issue, kind string) int {
	r.t.Helper()
	return len(issueOutboxOf(r.t, r.pool, issue, kind))
}

// statusWrites is the number of issue's dispatch_status rows that write status.
func (r *waitRig) statusWrites(issue, status string) int {
	r.t.Helper()
	var count int
	if err := r.pool.QueryRow(r.t.Context(), `select count(*) from outbox where kind = 'dispatch_status' and issue = $1 and payload->>'status' = $2`,
		issue, status).Scan(&count); err != nil {
		r.t.Fatalf("count %s's %s status writes: %v", issue, status, err)
	}
	return count
}

// firstID is the id of issue's oldest outbox row of kind whose payload holds field at value ("" for
// any), 0 for none.
func (r *waitRig) firstID(issue, kind, field, value string) int64 {
	r.t.Helper()
	var id *int64
	if err := r.pool.QueryRow(r.t.Context(), `select min(id) from outbox where issue = $1 and kind = $2 and ($3 = '' or payload->>$3 = $4)`,
		issue, kind, field, value).Scan(&id); err != nil {
		r.t.Fatalf("read %s's first %s row: %v", issue, kind, err)
	}
	if id == nil {
		return 0
	}
	return *id
}

// seedCheck records issue's production check of generation at merge with outcome, closed now, or
// open where outcome is "", on issue's own pull request.
func (r *waitRig) seedCheck(issue string, generation uint64, merge string, outcome record.CheckOutcome) {
	r.t.Helper()
	opened := r.now.Truncate(time.Microsecond)
	number := waitRootNumber
	if issue == waitChild {
		number = waitChildNumber
	}
	check := record.ProductionCheck{Issue: issue, Project: "LEGION", Tree: waitRoot, Repo: waitRepo, Number: number, MergeSHA: merge,
		OpenedAt: opened, BoundAt: opened.Add(48 * time.Hour), Role: claim.RoleTester, Looks: 1, Generation: generation}
	if outcome != "" {
		closed := opened
		check.ClosedAt, check.Outcome = &closed, outcome
	} else {
		check.LookOpen = true
	}
	seedRecord(r.t, r.pool, func(tx pgx.Tx) error { return record.NewStore().PutProductionCheck(r.t.Context(), tx, check) })
}

// seedDefect records an open defect of issue, reported in retro at the root's head, and answers its
// id.
func (r *waitRig) seedDefect(issue, text string) int64 {
	r.t.Helper()
	var id int64
	seedRecord(r.t, r.pool, func(tx pgx.Tx) error {
		var err error
		id, err = record.NewStore().PutDefect(r.t.Context(), tx, record.Defect{Issue: issue, Generation: waitGeneration, Kind: record.DefectKindDefect,
			Text: text, Role: claim.RoleImplementer, Head: headOne, ReportedAt: r.now})
		return err
	})
	return id
}

// check is one check block of the root's Verification document as the results read answers it:
// an after_merge check of id compared with main of the issue's own repository, whose merge Dispatch
// holds at merge, with Dispatch's answer and reason. A fail names its deciding result at merge.
func check(id, merge, answer, reason string) dispatch.CheckBlock {
	block := dispatch.CheckBlock{BlockID: id, ArtifactID: waitDocument, When: dispatch.CheckAfterMerge,
		ComparedWith: []dispatch.CheckBasis{{Kind: dispatch.BasisBranch, Branch: "main", MergeCommit: merge}},
		PassStands:   dispatch.PassStands{Answer: answer, Reason: reason}}
	if reason == dispatch.PassStandsReasonFail {
		block.PassStands.Results, block.PassStands.Commit = []string{"result-" + id}, merge
	}
	return block
}

// passing is check id passing at merge.
func passing(id, merge string) dispatch.CheckBlock {
	return check(id, merge, dispatch.PassStandsYes, "")
}

// noResult is check id with no result at merge yet.
func noResult(id, merge string) dispatch.CheckBlock {
	return check(id, merge, dispatch.PassStandsNo, dispatch.PassStandsReasonNoResult)
}

// answerOf is a read of issue's checks that answers checks and lists children.
func answerOf(issue string, children []dispatch.IssueChild, checks ...dispatch.CheckBlock) record.CheckReadAnswer {
	return record.CheckReadAnswer{Results: dispatch.IssueResults{Issue: issue, VerificationArtifactID: waitDocument, Checks: checks}, Children: children}
}

// enteredVerifying fails the test unless issue's merge stood it in verifying as an entry does: the
// status testing written once, the merge snapshot on its row, the stored wait with the moment of
// the entry, one row for close-wait.md, one read of its checks, one close-waits notice, no done and
// no Dispatch message.
func (r *waitRig) enteredVerifying(issue string, number int, merge, head string) {
	r.t.Helper()
	r.stands(issue, phase.Verifying, "testing")
	if got := r.statusWrites(issue, "testing"); got != 1 {
		r.t.Fatalf("%s's testing status writes = %d, want one", issue, got)
	}
	if got := r.statusWrites(issue, "done"); got != 0 {
		r.t.Fatalf("%s's done status writes = %d, want none: the merge closes nothing", issue, got)
	}
	got := r.issue(issue)
	if want := (recordMerge{number, head, merge, "main", waitRepo}); recordMergeOf(got) != want || !got.Merge.MergedAt.Equal(waitMergedAt) {
		r.t.Fatalf("%s's merge snapshot = %+v, want %+v merged at %s", issue, got.Merge, want, waitMergedAt)
	}
	if got.CloseWaitSince == nil || !got.CloseWaitSince.Equal(waitMergedAt) {
		r.t.Fatalf("%s waits since %v, want GitHub's merged_at %s", issue, got.CloseWaitSince, waitMergedAt)
	}
	if stored := r.stored(issue); stored.Entered.IsZero() || stored.Closed != nil || stored.Unread == nil {
		r.t.Fatalf("%s's stored wait = %+v, want the entry's moment and the read it waits for", issue, stored)
	}
	for kind, want := range map[string]int{"close_wait_publish": 1, "close_read": 1, "dispatch_message": 0} {
		if got := r.rows(issue, kind); got != want {
			r.t.Fatalf("%s's %s rows = %d, want %d", issue, kind, got, want)
		}
	}
	if got := noticesOfKind(r.t, r.pool, issue, "close-waits"); len(got) != 1 {
		r.t.Fatalf("%s's close-waits notices = %+v, want one", issue, got)
	}
}

// recordMerge is a merge snapshot's fields a test compares, all but its time.
type recordMerge struct {
	number                int
	head, sha, base, repo string
}

func recordMergeOf(issue record.Issue) recordMerge {
	return recordMerge{issue.Merge.Number, issue.Merge.Head, issue.Merge.SHA, issue.Merge.Base, issue.Merge.Repo}
}

// A merge in merging writes no done while a check has no pass. The issue stands in verifying with
// the status testing, on no slot, with its merge on its own row, one close-waits notice, its stored
// wait and the one row that writes it to close-wait.md, and a read of its checks, and no message
// on Dispatch. The read's answer of an after_merge check with no result keeps it there.
func TestAMergeInMergingDoesNotCloseWhileACheckHasNoPass(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	rig.enteredVerifying(waitRoot, waitRootNumber, waitRootMerge, headOne)
	if got := slottedIssues(t, rig.pool); len(got) != 0 {
		t.Fatalf("slots = %v, want none: verifying holds no slot", got)
	}
	if got := rig.statusWrites(waitRoot, "testing"); got != 1 || rig.firstID(waitRoot, "dispatch_status", "observedStatus", "retro") == 0 {
		t.Fatalf("the testing write is not one written over retro")
	}

	rig.seedCheck(waitRoot, waitGeneration, waitRootMerge, record.CheckRecorded)
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	rig.stands(waitRoot, phase.Verifying, "testing")
	if got := rig.statusWrites(waitRoot, "done"); got != 0 {
		t.Fatalf("done status writes = %d, want none while a check has no pass", got)
	}
	if stored := rig.stored(waitRoot); len(stored.Checks) != 1 || stored.Checks[0].ID != "c-after" || stored.Checks[0].Reason != dispatch.PassStandsReasonNoResult {
		t.Fatalf("the stored wait's checks = %+v, want c-after with no result", stored.Checks)
	}
}

// A merge that comes while the issue stands in retro tells the architect and moves nothing. The
// retro's completion then moves the issue into merging on a merged pull request, where it enters
// verifying as a merge in merging does, and starts no merger.
func TestAMergeInRetroWaitsWhenTheIssueReachesMerging(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	rig.seedRoot(phase.Retro, "retro")
	seedGate(t, rig.pool, record.DesignGate{Issue: waitRoot, ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(4)})
	seedPhase(t, rig.pool, record.PhaseRow{Issue: waitRoot, Role: claim.RoleImplementer, Claim: "implementer-claim"})
	rig.mergeRoot()
	rig.stands(waitRoot, phase.Retro, "retro")
	if got := noticesOfKind(t, rig.pool, waitRoot, "pr-merged"); len(got) != 1 {
		t.Fatalf("pr-merged notices = %+v, want one", got)
	}

	if result := rig.fact(intake.HandoffComplete{Issue: waitRoot, Role: claim.RoleImplementer, Claim: "implementer-claim", Generation: waitGeneration,
		Summary: "retro done", Commit: headOne}); result.Refusal != nil {
		t.Fatalf("the retro's completion = %+v, want it taken", result.Refusal)
	}
	rig.enteredVerifying(waitRoot, waitRootNumber, waitRootMerge, headOne)
	for _, start := range superviseRowsOf(t, rig.pool, waitRoot, "start") {
		if start.Role == claim.RoleMerger {
			t.Fatalf("start rows hold the merger's %+v, want none: the pull request merged already", start)
		}
	}
}

// From verifying, a fresh read of all passes, with the production check of the issue's generation
// recorded at the newest merge, closes the issue: done in Dispatch, its tree lingers, and the close
// is marked as one on its checks.
func TestAReadOfAllPassesClosesTheIssue(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	rig.seedCheck(waitRoot, waitGeneration, waitRootMerge, record.CheckRecorded)
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	closedOnChecks(t, rig, waitRoot)
	if got := rig.rows(waitRoot, "linger_close"); got != 1 {
		t.Fatalf("linger_close rows = %d, want the root's tree lingering", got)
	}
}

// closedOnChecks fails the test unless issue closed on its checks: done on the record and in one
// status write, closed_on_checks_at set, and close-wait.md's last version saying it closed, queued
// before the status write of done.
func closedOnChecks(t *testing.T, rig *waitRig, issue string) {
	t.Helper()
	rig.stands(issue, phase.Done, "done")
	if got := rig.statusWrites(issue, "done"); got != 1 {
		t.Fatalf("%s's done status writes = %d, want one", issue, got)
	}
	closed := rig.issue(issue)
	if closed.ClosedOnChecksAt == nil {
		t.Fatalf("%s is not marked closed on its checks", issue)
	}
	if stored := rig.stored(issue); stored.Closed == nil || !stored.Closed.Equal(*closed.ClosedOnChecksAt) {
		t.Fatalf("%s's stored wait = %+v, want it closed at %s", issue, stored, closed.ClosedOnChecksAt)
	}
	publish, done := lastID(t, rig, issue, "close_wait_publish"), rig.firstID(issue, "dispatch_status", "status", "done")
	if publish == 0 || done == 0 || publish > done {
		t.Fatalf("close-wait.md's last row %d and the done write %d, want the document's row first", publish, done)
	}
}

// lastID is the id of issue's newest outbox row of kind, 0 for none.
func lastID(t *testing.T, rig *waitRig, issue, kind string) int64 {
	t.Helper()
	rows := issueOutboxOf(t, rig.pool, issue, kind)
	if len(rows) == 0 {
		return 0
	}
	return rows[len(rows)-1].id
}

// Under a project that runs no production check, a merge whose every check passes still waits in
// verifying for the read after it, and that read closes it.
func TestAMergeWithNothingToWaitForClosesAtTheRead(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	rig.stands(waitRoot, phase.Verifying, "testing")
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	closedOnChecks(t, rig, waitRoot)
}

// A merge in merging with an issue beneath it in the workflow enters verifying: the issue beneath
// is a term of what the close waits for (F01), named in the stored wait and in the close-waits
// notice. The child's leave judges the root again, and the read it queues closes it.
func TestAMergeWithAnIssueBeneathWaitsInVerifying(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	rig.seedChild(waitChild, phase.Implementing, "in_progress")
	rig.mergeRoot()
	rig.enteredVerifying(waitRoot, waitRootNumber, waitRootMerge, headOne)
	if stored := rig.stored(waitRoot); len(stored.Beneath) != 1 || stored.Beneath[0].Key != waitChild || stored.Beneath[0].Status != "in_progress" {
		t.Fatalf("the stored wait's issues beneath = %+v, want %s in progress", stored.Beneath, waitChild)
	}
	if got := noticesOfKind(t, rig.pool, waitRoot, "close-waits"); !strings.Contains(got[0].Reason, waitChild) {
		t.Fatalf("the close-waits notice = %q, want it to name %s", got[0].Reason, waitChild)
	}
	rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "in_progress"}}, passing("c-after", waitRootMerge)))
	rig.stands(waitRoot, phase.Verifying, "testing")

	rig.ranOut(waitRoot, "close_read")
	rig.fact(intake.DispatchIssue{Key: waitChild, Seq: 2, Type: "issue.updated", Status: "backlog", Title: waitChild, Parent: waitRoot, Rank: "V"})
	rig.stands(waitRoot, phase.Verifying, "testing")
	if got := rig.rows(waitRoot, "close_read"); got != 1 {
		t.Fatalf("the root's close_read rows after its child left = %d, want one", got)
	}
	rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "backlog"}}, passing("c-after", waitRootMerge)))
	closedOnChecks(t, rig, waitRoot)
}

// Each read of the checks reads the issue's children from Dispatch: a child in backlog that the
// record does not hold keeps the close waiting, and closeWait's open children name it. Its move to
// icebox frees the close at the next read.
func TestTheCloseReadsTheChildrenFromDispatch(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	filed := []dispatch.IssueChild{{Key: "LEGION-300", Status: "backlog"}}
	rig.read(waitRoot, answerOf(waitRoot, filed, passing("c-after", waitRootMerge)))
	rig.stands(waitRoot, phase.Verifying, "testing")
	var view closeView
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		var err error
		view, err = rig.engine.closeWait(t.Context(), tx, rig.issue(waitRoot))
		return err
	})
	if !slices.Equal(view.wait.OpenChildren, []string{"LEGION-300"}) {
		t.Fatalf("closeWait's open children = %v, want LEGION-300", view.wait.OpenChildren)
	}
	if stored := rig.stored(waitRoot); len(stored.OpenChildren) != 1 || stored.OpenChildren[0].Key != "LEGION-300" {
		t.Fatalf("the stored wait's open children = %+v, want LEGION-300", stored.OpenChildren)
	}
	rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: "LEGION-300", Status: "icebox"}}, passing("c-after", waitRootMerge)))
	closedOnChecks(t, rig, waitRoot)
}

// The wait is one document replaced at each change. The entry stores it, with since when the issue
// waits and since when each item stands, and queues one row for close-wait.md. A change of an
// item's state queues one more where none stands, and a read that changes no item queues none. No
// Dispatch message is queued from the entry to the close, and the row of the last version comes
// before the status write of done.
func TestTheWaitIsOneDocumentReplacedAtEachChange(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	entered := rig.stored(waitRoot)
	if entered.Entered.IsZero() || entered.Unread == nil || !entered.Unread.Since.Equal(entered.Entered) {
		t.Fatalf("the stored wait at the entry = %+v, want the entry's moment and the unread read since then", entered)
	}
	if got := rig.rows(waitRoot, "close_wait_publish"); got != 1 {
		t.Fatalf("close_wait_publish rows at the entry = %d, want one", got)
	}
	rig.ranOut(waitRoot, "close_wait_publish")

	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	first := rig.stored(waitRoot)
	if first.Unread != nil || len(first.Checks) != 1 || !first.Checks[0].Since.Equal(rig.now) {
		t.Fatalf("the stored wait after the first read = %+v, want c-after standing since the read", first)
	}
	if got := rig.rows(waitRoot, "close_wait_publish"); got != 1 {
		t.Fatalf("close_wait_publish rows after a change = %d, want one", got)
	}
	rig.ranOut(waitRoot, "close_wait_publish")

	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	if got := rig.rows(waitRoot, "close_wait_publish"); got != 0 {
		t.Fatalf("close_wait_publish rows after a read that changed nothing = %d, want none", got)
	}
	if again := rig.stored(waitRoot); !again.Checks[0].Since.Equal(first.Checks[0].Since) {
		t.Fatalf("c-after stands since %s after a read that changed nothing, want %s", again.Checks[0].Since, first.Checks[0].Since)
	}

	rig.read(waitRoot, answerOf(waitRoot, nil, check("c-after", waitRootMerge, dispatch.PassStandsUnknown, dispatch.PassStandsReasonNotPlaced)))
	if got := rig.rows(waitRoot, "close_wait_publish"); got != 1 {
		t.Fatalf("close_wait_publish rows after the check's state changed = %d, want one", got)
	}
	rig.read(waitRoot, answerOf(waitRoot, nil, check("c-after", waitRootMerge, dispatch.PassStandsNo, dispatch.PassStandsReasonNoResult)))
	if got := rig.rows(waitRoot, "close_wait_publish"); got != 1 {
		t.Fatalf("close_wait_publish rows while one stands = %d, want that one", got)
	}
	rig.ranOut(waitRoot, "close_wait_publish")

	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	closedOnChecks(t, rig, waitRoot)
	if got := rig.rows(waitRoot, "dispatch_message"); got != 0 {
		t.Fatalf("Dispatch messages from the entry to the close = %d, want none", got)
	}
}

// The last child's leave queues one read of the checks of its root, which waits in verifying for
// that child alone, and the root closes at that read, with no controller tick (M4). The child here
// leaves by its own close on its checks, a leave no Dispatch event of the child brings, so only
// the leave's own read can wake the root. A child that moves to icebox in Dispatch queues one read
// as well.
func TestTheLastChildsLeaveQueuesARead(t *testing.T) {
	t.Parallel()
	t.Run("the child closes on its checks", func(t *testing.T) {
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		rig.seedChild(waitChild, phase.Merging, "retro")
		rig.mergeRoot()
		rig.mergeChild()
		rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "testing"}}, passing("c-after", waitRootMerge)))
		if stored := rig.stored(waitRoot); len(stored.Beneath) != 1 || len(stored.Checks) != 0 || stored.Unread != nil {
			t.Fatalf("the root's stored wait = %+v, want its child alone", stored)
		}
		rig.ranOut(waitRoot, "close_read")

		rig.read(waitChild, answerOf(waitChild, nil, passing("c-child", waitChildMerge)))
		closedOnChecks(t, rig, waitChild)
		if got := rig.rows(waitRoot, "close_read"); got != 1 {
			t.Fatalf("the root's close_read rows after its child's close = %d, want one", got)
		}
		rig.stands(waitRoot, phase.Verifying, "testing")
		rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "done"}}, passing("c-after", waitRootMerge)))
		closedOnChecks(t, rig, waitRoot)
	})

	t.Run("the child moves to icebox", func(t *testing.T) {
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		rig.seedChild(waitChild, phase.Implementing, "in_progress")
		rig.mergeRoot()
		rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "in_progress"}}, passing("c-after", waitRootMerge)))
		rig.ranOut(waitRoot, "close_read")

		rig.fact(intake.DispatchIssue{Key: waitChild, Seq: 2, Type: "issue.updated", Status: "icebox", Title: waitChild, Parent: waitRoot, Rank: "V"})
		if got := rig.rows(waitRoot, "close_read"); got != 1 {
			t.Fatalf("the root's close_read rows = %d, want one", got)
		}
		rig.stands(waitRoot, phase.Verifying, "testing")
		rig.read(waitRoot, answerOf(waitRoot, []dispatch.IssueChild{{Key: waitChild, Status: "icebox"}}, passing("c-after", waitRootMerge)))
		closedOnChecks(t, rig, waitRoot)
	})
}

// A member of a lingering tree whose merge stood it in verifying closes on its checks there: the
// close writes the member's done directly, as the lingering branch does, with child-closed to the
// architect above it.
func TestAMemberOfALingeringTreeClosesOnItsChecks(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	until := rig.now.Add(time.Hour)
	seedIssue(t, rig.pool, record.Issue{Key: waitRoot, Repo: waitRepo, DefaultBranch: "main", GitHubRepo: waitRepo, Tree: waitRoot, Project: "LEGION",
		Title: "root", Phase: phase.Done, Generation: waitGeneration, Status: "done", DispatchStatus: "done", Rank: "U", LingerUntil: &until, LastDispatchSeq: 1})
	rig.seedChild(waitChild, phase.Merging, "retro")
	rig.mergeChild()
	rig.stands(waitChild, phase.Verifying, "testing")
	if got := noticesOfKind(t, rig.pool, waitChild, "child-closed"); len(got) != 0 {
		t.Fatalf("child-closed notices at the merge = %+v, want none: the member waits", got)
	}
	rig.read(waitChild, answerOf(waitChild, nil, passing("c-after", waitChildMerge)))
	closedOnChecks(t, rig, waitChild)
	if got := noticesOfKind(t, rig.pool, waitChild, "child-closed"); len(got) != 1 {
		t.Fatalf("child-closed notices at the close = %+v, want one", got)
	}
}

// A child whose production check closed recorded while it stood in verifying has its pod suspended
// once at its close on its checks: the check's close closed the tester's claim and deleted nothing
// of the pod, and the close queues the one IssueSuspend. Where its project runs no production
// check, its leave queues the one suspension, and the judge queues none.
func TestAChildThatClosesOnItsChecksIsSuspendedOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		role claim.Role
	}{
		{name: "production check recorded", role: claim.RoleTester},
		{name: "production check off", role: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWaitRig(t, tc.role)
			rig.seedRoot(phase.Implementing, "in_progress")
			rig.seedChild(waitChild, phase.Merging, "retro")
			rig.mergeChild()
			rig.stands(waitChild, phase.Verifying, "testing")
			if got := issueSuspensions(t, rig.pool, waitChild); len(got) != 0 {
				t.Fatalf("suspensions at the merge = %+v, want none", got)
			}
			if tc.role != "" {
				rig.read(waitChild, answerOf(waitChild, nil, passing("c-after", waitChildMerge)))
				rig.report(waitChild, 1, "pass", "the check passed in production", nil)
				if got := issueSuspensions(t, rig.pool, waitChild); len(got) != 0 {
					t.Fatalf("suspensions at the check's close in verifying = %+v, want none", got)
				}
			}
			rig.read(waitChild, answerOf(waitChild, nil, passing("c-after", waitChildMerge)))
			closedOnChecks(t, rig, waitChild)
			if got := issueSuspensions(t, rig.pool, waitChild); len(got) != 1 {
				t.Fatalf("suspensions of the child = %+v, want one", got)
			}
			if closed := superviseRequests(t, rig.pool, "issue_close"); closed[waitChild+"/"+string(claim.RoleTester)] != 1 {
				t.Fatalf("issue_close rows = %v, want the tester's claim closed once", closed)
			}
		})
	}
}

// A store that a release before left with an issue in merging, its pull request merged and a child
// running beneath it: the boot moves it into verifying, writes its merge snapshot from the merged
// row, and reads its checks.
func TestTheFirstBootMovesAMergedIssueInMergingIntoVerifying(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	seedIssue(t, rig.pool, record.Issue{Key: waitRoot, Repo: waitRepo, DefaultBranch: "main", GitHubRepo: waitRepo, Tree: waitRoot, Project: "LEGION",
		Title: "root", Phase: phase.Merging, Generation: waitGeneration, Status: "retro", DispatchStatus: "retro", Rank: "U", LastDispatchSeq: 1})
	seedTreeLifecycle(t, rig.pool, waitRoot)
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestMerged, Issue: waitRoot, Repo: waitRepo, Number: waitRootNumber, Branch: "legion/" + waitRoot,
		Base: "main", HeadSHA: headOne, MergeSHA: waitRootMerge, Failing: []string{}})
	rig.seedChild(waitChild, phase.Implementing, "in_progress")

	rig.fact(intake.DaemonBooted{Boot: 1})
	rig.stands(waitRoot, phase.Verifying, "testing")
	root := rig.issue(waitRoot)
	if root.Merge.Number != waitRootNumber || root.Merge.SHA != waitRootMerge || root.Merge.Head != headOne {
		t.Fatalf("the merge snapshot after the boot = %+v, want #42 at %s", root.Merge, waitRootMerge)
	}
	if stored := rig.stored(waitRoot); len(stored.Beneath) != 1 || stored.Beneath[0].Key != waitChild {
		t.Fatalf("the stored wait after the boot = %+v, want %s beneath", stored, waitChild)
	}
	if got := rig.rows(waitRoot, "close_read"); got != 1 {
		t.Fatalf("close_read rows after the boot = %d, want one", got)
	}
}

// No role's statement closes the issue: in verifying with a check without a result, a sign-off, a
// merger's completion and a production pass reported on no open check leave it there.
func TestNoRoleStatementClosesTheIssue(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	for _, fact := range []intake.Fact{
		intake.SignOff{Issue: waitRoot, Generation: waitGeneration},
		intake.HandoffComplete{Issue: waitRoot, Role: claim.RoleMerger, Claim: "root-merger", Generation: waitGeneration, Summary: "merged, done", Commit: headOne},
		intake.CheckReport{Issue: waitRoot, Look: 1, Verdict: "pass", Summary: "all good in production"},
	} {
		rig.fact(fact)
		rig.stands(waitRoot, phase.Verifying, "testing")
	}
	if got := rig.statusWrites(waitRoot, "done"); got != 0 {
		t.Fatalf("done status writes = %d, want none: no role's word closes the issue", got)
	}
}

// laterMerge records a second merge of the root, a next change's at waitLaterMerge, as the newest on
// its row, while it stands in verifying.
func (r *waitRig) laterMerge() {
	r.t.Helper()
	seedRecord(r.t, r.pool, func(tx pgx.Tx) error {
		_, err := record.NewStore().PutMergeSnapshot(r.t.Context(), tx, waitRoot, record.Merge{Number: 77, Head: headThree, SHA: waitLaterMerge, Base: "main",
			Repo: waitRepo, MergedAt: waitMergedAt.Add(time.Hour)})
		return err
	})
}

// After a second merge, a read whose checks name the earlier merge commit decides nothing: the
// close stays unread and is read again. A read that names the newest merge decides it.
func TestAReadThatNamesAnEarlierMergeDecidesNothing(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	rig.laterMerge()
	rig.ranOut(waitRoot, "close_read")
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	rig.stands(waitRoot, phase.Verifying, "testing")
	if stored := rig.stored(waitRoot); stored.Unread == nil || !strings.Contains(stored.Unread.Cause, waitLaterMerge) {
		t.Fatalf("the stored wait = %+v, want it unread for a read that names no basis of %s", stored, waitLaterMerge)
	}
	if rows := issueOutboxOf(t, rig.pool, waitRoot, "close_read"); len(rows) != 1 || !rows[0].nextAt.After(rig.now) {
		t.Fatalf("close_read rows = %+v, want one read again later", rows)
	}
	rig.ranOut(waitRoot, "close_read")
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitLaterMerge)))
	closedOnChecks(t, rig, waitRoot)
}

// A check Dispatch answers unknown because it could not place a result, or could not make a read,
// is read again on the backoff (D4): the read queues one more read, due a minute on, and the same
// answer read again queues the next one further out, as long after the read as the answer has
// stood. A check with no result waits for evidence, so its read queues no read again.
func TestACheckDispatchCouldNotPlaceIsReadAgainOnTheBackoff(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{dispatch.PassStandsReasonNotPlaced, dispatch.PassStandsReasonReadFailed} {
		t.Run(reason, func(t *testing.T) {
			rig := newWaitRig(t, "")
			rig.seedRoot(phase.Merging, "retro")
			rig.mergeRoot()
			rig.ranOut(waitRoot, "close_read")
			unknown := answerOf(waitRoot, nil, check("c-after", waitRootMerge, dispatch.PassStandsUnknown, reason))
			rig.read(waitRoot, unknown)
			rows := issueOutboxOf(t, rig.pool, waitRoot, "close_read")
			if len(rows) != 1 || !rows[0].nextAt.Equal(rig.now.Add(time.Minute)) {
				t.Fatalf("close_read rows after the unknown answer = %+v, want one due a minute after %s", rows, rig.now)
			}
			rig.ranOut(waitRoot, "close_read")
			rig.now = rows[0].nextAt
			rig.read(waitRoot, unknown)
			again := issueOutboxOf(t, rig.pool, waitRoot, "close_read")
			if len(again) != 1 || again[0].nextAt.Sub(rig.now) <= time.Minute {
				t.Fatalf("close_read rows after the same answer again = %+v, want one due more than a minute after %s", again, rig.now)
			}
		})
	}
	t.Run("no result", func(t *testing.T) {
		rig := newWaitRig(t, "")
		rig.seedRoot(phase.Merging, "retro")
		rig.mergeRoot()
		rig.ranOut(waitRoot, "close_read")
		rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
		if got := rig.rows(waitRoot, "close_read"); got != 0 {
			t.Fatalf("close_read rows after a check with no result = %d, want none: it waits for evidence", got)
		}
	})
}

// After a second merge, the production check recorded for the earlier merge decides nothing: the
// production term reads earlier_merge, and the close waits. A reason waits too, so the read opens
// no check of the newest merge, which would stand in the term's place.
func TestAProductionCheckOfAnEarlierMergeDecidesNothing(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	rig.seedCheck(waitRoot, waitGeneration, waitRootMerge, record.CheckRecorded)
	rig.seedDefect(waitRoot, "the retro found the cache key unchecked")
	rig.laterMerge()
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitLaterMerge)))
	rig.stands(waitRoot, phase.Verifying, "testing")
	if stored := rig.stored(waitRoot); stored.Production == nil || stored.Production.State != record.ProductionEarlierMerge {
		t.Fatalf("the stored wait's production check = %+v, want earlier_merge", stored.Production)
	}
}

// A claim that fails while its issue waits in verifying holds nothing: no role works the phase.
func TestAClaimFailureInVerifyingHoldsNothing(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	for _, role := range []claim.Role{claim.RoleMerger, claim.RoleImplementer} {
		rig.fact(intake.ClaimFailed{Issue: waitRoot, Role: role, Reason: "the launch failed"})
		rig.stands(waitRoot, phase.Verifying, "testing")
	}
	if got := noticesOfKind(t, rig.pool, waitRoot, "held"); len(got) != 0 {
		t.Fatalf("held notices = %+v, want none", got)
	}
}
