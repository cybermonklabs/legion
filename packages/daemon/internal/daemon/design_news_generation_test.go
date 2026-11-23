package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The cases here hold to one rule: what a prompt told counts, and folds, only in the run it was
// rendered in. A clear of a generation deletes what the holders of each issue it covers took
// (record.Postgres.clearGeneration), and the write beside it raises the issue's generation (a child
// set back to todo, an issue moved into another tree) or its tree root's (a root set back to todo).
// A delivery's copy of what its prompt carried outlives that clear: the claim is kept, and the turn
// of a prompt sent before the clear can start, or end, after it. Counted by issue, audience, step
// and version alone, that copy would tell the next run news nothing told it, write the next run's
// row before its own baseline, and clear an opening the next run owes under a number its row holds
// again. Each case runs through the daemon's own parts, as the newsPathRig cases do.

// generationRig is a tree of a root in implementing and a child whose reviewer holds its phase
// between turns, every audience having taken version 4 of the spec, which stands approved.
func generationRig(t *testing.T) (*newsPathRig, *supervise.Machine, *fake.Conn, claim.Token) {
	t.Helper()
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Implementing},
		pathMember{key: pathChild, parent: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 4)
	r.told(pathRoot, record.AudienceArchitect, 4)
	r.told(pathChild, record.AudienceHolder, 4)
	reviewer, conn, token := r.reviewerBetweenTurns(pathChild)
	return r, reviewer, conn, token
}

// approve is a writer saving version of the root's spec and a person approving it: the gate closes
// at the save and opens at the approval, and the role messages either queued are run.
func (r *newsPathRig) approve(version int) {
	r.t.Helper()
	r.dispatchEvent(intake.DispatchArtifactVersion, version)
	r.dispatchEvent(intake.DispatchArtifactApproved, version)
	r.run(ofKind(record.OutboxKindRoleMessage))
}

// statusEvent is a Dispatch event of key carrying status and the label that hands the issue to
// Legion, as a person's move of it in Dispatch reaches the daemon, applied through the engine and
// then admission.
func (r *newsPathRig) statusEvent(key, status string) {
	r.t.Helper()
	issue := readIssue(r.t, r.pool, key)
	fact := intake.DispatchIssue{Key: key, ResolvedRepo: issue.Repo, Seq: issue.LastDispatchSeq + 1, Type: "issue.updated",
		Status: status, Title: issue.Title, Rank: issue.Rank, HandedOver: true}
	if issue.Parent != nil {
		fact.Parent = *issue.Parent
	}
	applyFact(r.t, r.pool, "dispatch", fmt.Sprintf("status-%s-%s-%d", key, status, fact.Seq), fact, r.engine, r.admission)
}

// reenter is a person setting key, a child of the rig's live tree, back to todo: its run starts
// again as its next generation, and the clear of the last one empties what its holders took
// (workflow's ReenterChild).
func (r *newsPathRig) reenter(key string) {
	r.t.Helper()
	before := readIssue(r.t, r.pool, key)
	r.statusEvent(key, "todo")
	if after := readIssue(r.t, r.pool, key); after.Generation != before.Generation+1 || after.Tree != before.Tree {
		r.t.Fatalf("%s = %+v after its todo, want generation %d in the tree of %s: the re-entry this case is about never happened",
			key, after, before.Generation+1, before.Tree)
	}
}

// toldRows is how many rows the told table itself holds of issue, whatever a credit adds to the
// read of them.
func (r *newsPathRig) toldRows(issue string) int {
	r.t.Helper()
	var rows int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from design_news_told where issue = $1`, issue).Scan(&rows); err != nil {
		r.t.Fatalf("count the told rows of %s: %v", issue, err)
	}
	return rows
}

// wantUntold fails the test unless issue's holder took no version of the spec and is owed no
// opening, as every reader reads it: the state a clear of its generation leaves until its next
// run takes its own baseline.
func (r *newsPathRig) wantUntold(when, issue string) {
	r.t.Helper()
	row := r.toldOf(issue, record.AudienceHolder)
	if took, known := row.Took(record.DesignStepSpec); known || row.OpeningOwed {
		r.t.Fatalf("%s: %s's holder took version %d (known %t) owing the opening %t, want no version and no opening: the clear emptied what its holders took",
			when, issue, took, known, row.OpeningOwed)
	}
}

// A credit confirmed before a child's re-entry is of the run the re-entry ends. The reviewer's
// turn started on the news of version 5, so its delivery holds that news as its credit, and its
// result has not come when a person sets the child back to todo. Version 6 was saved meanwhile, so
// the gate is closed and the child's next run waits for it at admitted. The turn's result then
// comes: the write that records it folds nothing into the next run's row, which does not exist
// yet. A person approves version 6, the gate opens, and the child's first phase takes version 6 as
// its baseline: its work starts from that version. Folded by issue and step alone, the old credit
// would have written the child's row first, and the baseline, which leaves a row already there as
// it stands, would have written nothing.
func TestACreditConfirmedBeforeAChildsReentryNeitherCountsNorFoldsAfterIt(t *testing.T) {
	r, reviewer, _, token := generationRig(t)
	r.approve(5)
	r.queueNewsTask(pathChild)
	r.delivered(reviewer)
	r.startTurn(reviewer)
	r.wantTold("after the reviewer's turn confirmed the news of version 5", pathChild, record.AudienceHolder, 5, false)

	r.dispatchEvent(intake.DispatchArtifactVersion, 6)
	r.reenter(pathChild)
	r.wantUntold("after the re-entry, the old run's credit standing", pathChild)

	endTurn(r.ctx, t, reviewer, token)
	if rows := r.toldRows(pathChild); rows != 0 {
		t.Errorf("the told table holds %d rows of %s after the old run's turn ended, want none: its credit folds into no row of the next run", rows, pathChild)
	}
	r.wantUntold("after the old run's turn ended", pathChild)

	r.dispatchEvent(intake.DispatchArtifactApproved, 6)
	if issue := readIssue(t, r.pool, pathChild); issue.Phase == phase.Admitted {
		t.Fatalf("%s still stands at admitted after the gate opened, want its first phase started: the case's own premise", pathChild)
	}
	r.wantTold("after the next run's first phase started", pathChild, record.AudienceHolder, 6, false)
}

// A prompt sent before a child's re-entry tells the run the re-entry ends, whenever its turn
// starts. The reviewer was sent the news of version 5 and Oh My Pi acknowledged it; its turn has
// not started when a person sets the child back to todo, under a gate a saved version 6 closed.
// The claim is kept, so the turn then starts and the machine confirms the task, and the turn's
// result says it ran. Neither write gives the child's next run anything: its first phase, started
// once version 6 is approved, takes version 6 as its baseline.
func TestAPromptSentBeforeAChildsReentryConfirmsNothingForItsNextRun(t *testing.T) {
	r, reviewer, _, token := generationRig(t)
	r.approve(5)
	r.queueNewsTask(pathChild)
	r.delivered(reviewer)
	r.wantTold("after the news of version 5 was sent, before its turn", pathChild, record.AudienceHolder, 4, true)

	r.dispatchEvent(intake.DispatchArtifactVersion, 6)
	r.reenter(pathChild)
	r.startTurn(reviewer)
	r.wantUntold("after the turn of a prompt sent before the re-entry started", pathChild)

	endTurn(r.ctx, t, reviewer, token)
	if rows := r.toldRows(pathChild); rows != 0 {
		t.Errorf("the told table holds %d rows of %s after the old run's turn ended, want none: its credit folds into no row of the next run", rows, pathChild)
	}
	r.wantUntold("after the old run's turn ended", pathChild)

	r.dispatchEvent(intake.DispatchArtifactApproved, 6)
	if issue := readIssue(t, r.pool, pathChild); issue.Phase == phase.Admitted {
		t.Fatalf("%s still stands at admitted after the gate opened, want its first phase started: the case's own premise", pathChild)
	}
	r.wantTold("after the next run's first phase started", pathChild, record.AudienceHolder, 6, false)
}

// storedOpening is whether the told table itself holds issue's holder as owed the opening, and
// under which number, whatever a standing credit takes off the read of it.
func (r *newsPathRig) storedOpening(issue string) (bool, int) {
	r.t.Helper()
	var owed bool
	var number int
	if err := r.pool.QueryRow(r.ctx, `select opening_owed, opening_seq from design_news_told where issue = $1 and audience = 'holder'`,
		issue).Scan(&owed, &number); err != nil {
		r.t.Fatalf("read the opening the told table holds of %s's holder: %v", issue, err)
	}
	return owed, number
}

// An opening is named by the number its told row held for it, and a clear writes the next run's
// row from number 0 again. The reviewer's turn started on a task that carried the opening the
// child's row owed, and its result has not come when a person sets the child back to todo. The
// gate stands open, so the child's first phase starts at once and takes its baseline. A writer
// then saves version 5 and a person approves it: the gate closes and opens while the child's next
// holder is owed news, so its row owes the opening, and under the very number the old task
// carried. That opening stays owed while the old credit stands, and after the old turn's result:
// the next run's own task tells it. Matched by its number alone, the old copy would have told an
// opening nobody of the next run was told.
func TestAnOpeningTheLastRunCarriedClearsNoOpeningOfTheNextRunUnderTheSameNumber(t *testing.T) {
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Implementing},
		pathMember{key: pathChild, parent: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 4)
	r.told(pathRoot, record.AudienceArchitect, 4)
	r.told(pathChild, record.AudienceHolder, 3)
	reviewer, _, token := r.reviewerBetweenTurns(pathChild)
	r.closeAndReopen()
	r.wantTold("at the reopening", pathChild, record.AudienceHolder, 3, true)
	r.queueNewsTask(pathChild)
	r.delivered(reviewer)
	carried := pendingNewsOf(t, reviewer)
	if len(carried.Openings) != 1 || carried.Openings[0].Closed {
		t.Fatalf("the reviewer's news task carries %+v, want the opening its row owes: the case's own premise", carried)
	}
	number := carried.Openings[0].Seq
	r.startTurn(reviewer)
	r.wantTold("after the reviewer's turn confirmed the task", pathChild, record.AudienceHolder, 4, false)

	r.reenter(pathChild)
	if issue := readIssue(t, r.pool, pathChild); issue.Phase == phase.Admitted {
		t.Fatalf("%s still stands at admitted after its re-entry under an open gate, want its first phase started: the case's own premise", pathChild)
	}
	r.wantTold("after the next run's first phase started", pathChild, record.AudienceHolder, 4, false)
	r.approve(5)
	if owed, held := r.storedOpening(pathChild); !owed || held != number {
		t.Fatalf("the next run's row owes the opening %t under number %d, want it owed under %d, the number the old task carried: the case's own premise",
			owed, held, number)
	}
	r.wantTold("after the gate opened for the next run, the old run's credit standing", pathChild, record.AudienceHolder, 4, true)

	endTurn(r.ctx, t, reviewer, token)
	r.wantTold("after the old run's turn ended", pathChild, record.AudienceHolder, 4, true)
}

// A root set back to todo starts a new generation of its whole tree: the clear empties what the
// holders of every issue of it took, and raises the root's generation alone. The reviewer of a
// child is in the turn that confirmed the news of version 5 when a person closes the root and
// sets it back to todo inside the linger, before the suspension the linger queued has stopped
// that turn. The child keeps its own generation, so its tree root's is what says the credit is of
// the run that ended: the child's holder reads as having taken nothing. The tree's next run then
// learns from the review history that its holder stood on version 4 (record.Store.FillTold, the
// reconcile's write), and that is what it took: version 5 is owed to it, while the old credit
// stands and after the old turn's result. Counted by the child's own generation alone, the old
// credit would have reached from version 4 and told that run version 5.
func TestAMembersCreditCountsNothingOnceItsTreesRootIsSetBackToTodo(t *testing.T) {
	r, reviewer, _, token := generationRig(t)
	r.approve(5)
	r.queueNewsTask(pathChild)
	r.delivered(reviewer)
	r.startTurn(reviewer)
	r.wantTold("after the reviewer's turn confirmed the news of version 5", pathChild, record.AudienceHolder, 5, false)

	r.statusEvent(pathRoot, record.DoneStatus)
	if root := readIssue(t, r.pool, pathRoot); !root.Lingers() {
		t.Fatalf("%s = %+v after a person closed it, want its tree lingering: the case's own premise", pathRoot, root)
	}
	r.statusEvent(pathRoot, "todo")
	root, child := readIssue(t, r.pool, pathRoot), readIssue(t, r.pool, pathChild)
	if root.Generation != 2 || child.Generation != 1 || child.Tree != pathRoot {
		t.Fatalf("after the root's todo %s is on generation %d and %s on generation %d in the tree of %s, want 2 and 1 in the tree of %s: the case's own premise",
			pathRoot, root.Generation, pathChild, child.Generation, child.Tree, pathRoot)
	}
	r.wantUntold("after the root's re-admission, the old run's credit standing", pathChild)

	r.inTx(func(tx pgx.Tx) error {
		return r.records.FillTold(r.ctx, tx, pathChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 4}, time.Now())
	})
	r.wantTold("after the next run read the version its holder stood on, the old run's credit standing", pathChild, record.AudienceHolder, 4, false)

	endTurn(r.ctx, t, reviewer, token)
	r.wantTold("after the old run's turn ended", pathChild, record.AudienceHolder, 4, false)
}

// An issue moved into another tree takes that tree's documents, and its next generation. Here a
// delivery of the tree it left still holds, confirmed and its turn's result not come, a copy that
// told the issue's holder version 7 of that tree's spec with no earlier version. The tree that
// takes the issue stands approved at version 4 of a spec of its own. After the adoption the issue's
// holder has taken nothing of it, while that credit stands and after its turn's result: the issue's
// first phase in its new tree takes that tree's baseline, and every version approved after it is
// news to it. Counted by the step and the version numbers alone, the copy would have said the
// holder took version 7 of a document the new tree has approved only to version 4, and the news of
// versions 5, 6 and 7 of it would never be owed.
//
// No flow of this daemon leaves such a copy standing where an adoption can take the issue: the two
// routes take a member that never started a phase, or the tree of a root that never ran (workflow's
// AdoptionRefusal). So the copy is written through the claim store, as a confirmed prompt's is, and
// the adoption is the real one, an architect's release applied through admission. The rule is held
// at the read all the same, since nothing else stands between a copy and the row of an issue
// another tree took.
func TestACreditOfTheTreeAnIssueLeftCountsNothingInTheTreeThatTakesIt(t *testing.T) {
	const closedRoot, member = "LEGION-900", "LEGION-901"
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Implementing})
	r.told(pathRoot, record.AudienceHolder, 4)
	r.told(pathRoot, record.AudienceArchitect, 4)
	// The closed tree: its root finished, on its third generation, with no linger left to run, and
	// a member that never started a phase.
	ended := time.Now().Add(-time.Hour)
	putOutboxIssue(t, r.pool, r.records, record.Issue{Key: closedRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Title: closedRoot, Tree: closedRoot, Phase: phase.Done, Generation: 3, LingerUntil: &ended,
		Status: record.DoneStatus, DispatchStatus: record.DoneStatus, Rank: "C", HandedOver: true, LastDispatchSeq: 1})
	putOutboxIssue(t, r.pool, r.records, record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Title: member, Tree: closedRoot, Parent: record.ParentOf(closedRoot), Phase: phase.Admitted, Generation: 1,
		Status: "todo", DispatchStatus: "todo", Rank: "D", HandedOver: true, LastDispatchSeq: 1})
	st, ok := r.sup.deps.Store.(*store.Store)
	if !ok {
		t.Fatalf("the rig's claim store is %T, want the daemon's", r.sup.deps.Store)
	}
	holder := supervise.Claim{Token: mustClaimToken(t, closedRoot, claim.RoleArchitect), Project: "legion", Tree: closedRoot,
		Issue: closedRoot, Role: claim.RoleArchitect, Generation: 1, State: supervise.StateWorking}
	// The copy as the render wrote it in the tree the issue left: the member on generation 1, its
	// tree's root on generation 3.
	copied := []byte(`{"task":"Read the design.","items":[{"issue":"` + member + `","audience":"holder","step":"spec",` +
		`"artifact":"artifact-900","from":null,"to":7,"generation":1,"tree_generation":3}]}`)
	at := time.Now().UTC()
	confirmed := supervise.Delivery{ID: "outbox:900", Task: "Read the design.\n\nThe approved design changed.", QueuedAt: at,
		News: copied, Carried: copied, ConfirmedAt: at}
	if err := st.PutClaimAndDelivery(r.ctx, holder, confirmed); err != nil {
		t.Fatalf("write the confirmed task of the tree the issue left: %v", err)
	}

	applyFact(t, r.pool, "api", "release-"+member, intake.ChildAdoption{Tree: pathRoot,
		Children: []intake.AdoptedChild{{Issue: member, Parent: pathRoot}}}, r.engine, r.admission)
	if adopted := readIssue(t, r.pool, member); adopted.Tree != pathRoot || adopted.Generation != 2 {
		t.Fatalf("%s = %+v after the release, want generation 2 in the tree of %s: the adoption this case is about never happened", member, adopted, pathRoot)
	}
	r.wantUntold("after the adoption, the credit of the tree the issue left standing", member)

	final := confirmed
	final.Ran = true
	if err := st.PutDelivery(r.ctx, holder.Token, final); err != nil {
		t.Fatalf("record that the old tree's turn ran: %v", err)
	}
	if rows := r.toldRows(member); rows != 0 {
		t.Errorf("the told table holds %d rows of %s after the old tree's turn ended, want none: its credit folds into no row of the tree that took the issue", rows, member)
	}
	r.wantUntold("after the old tree's turn ended", member)
}
