package daemon

import (
	"fmt"
	"slices"
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
// rendered in, under the gate root it was rendered under. A clear of a generation deletes what the
// holders of the issue it covers took (record.Postgres.clearGeneration), and the write beside it
// raises that issue's generation (a new run, workflow's NewRun: here a child the backlog took out of
// work and its architect's rerun_child ran again); a change of Dispatch parent that moves an issue
// under another gate root forgets what its holder took (record.Store.ForgetTold). A delivery's copy
// of what its prompt carried outlives either: the claim is kept, and the turn of a prompt sent
// before can start, or end, after it. Counted by issue, audience, step and version alone, that copy
// would tell the next run news nothing told it, write the next run's row before its own baseline,
// and clear an opening the next run owes under a number its row holds again. A leave and a return
// to work end no run, so a credit stands across them. Each case runs through the daemon's own
// parts, as the newsPathRig cases do.

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

// reenter is a new run of key, a child of the rig's live tree, as this branch opens one (decision 8
// of dispatch://INFRA-4730, workflow's NewRun): a person moves it to the backlog, which takes it
// out of Legion's work and keeps its phase, and the root's architect runs it again (rerun_child),
// which opens its next generation in the call's own fact, and the clear of the last one empties
// what its holders took. A person's todo on a child in work opens no run (R20 of
// dispatch://INFRA-3320, workflow's backInWork). Dispatch then takes the call's todo, which moves
// nothing more.
func (r *newsPathRig) reenter(key string) {
	r.t.Helper()
	before := readIssue(r.t, r.pool, key)
	parent := ""
	if before.Parent != nil {
		parent = *before.Parent
	}
	putCopyRow(r.t, r.pool, key, parent, "backlog")
	r.statusEvent(key, "backlog")
	applyFact(r.t, r.pool, "api", fmt.Sprintf("rerun-%s-%d", key, before.Generation), intake.StatusCall{Op: intake.RerunChildCall,
		Architect: mustClaimToken(r.t, r.root, claim.RoleArchitect), Issues: []intake.StatusCallIssue{{Key: key, Status: "todo"}}},
		r.engine, r.admission)
	putCopyRow(r.t, r.pool, key, parent, "todo")
	r.statusEvent(key, "todo")
	if after := readIssue(r.t, r.pool, key); after.Generation != before.Generation+1 {
		r.t.Fatalf("%s = %+v after its backlog and rerun_child, want generation %d: the new run this case is about never happened",
			key, after, before.Generation+1)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// Main's case of a root set back to todo, as this branch runs it (R15 and R20 of
// dispatch://INFRA-3320). A person's done and then todo on a root in Legion's work is a leave and a
// return to work in the same run (workflow's ReturnToWork): the root keeps its phase and its
// generation, and nothing is cleared. But while the root is out of work its child is a root (no
// issue above it is in work), and the pass that finds it so marks it as holding its own gate
// (record.Store.MarkHoldsGate), a mark nothing clears: after the root's return the child's gate
// root on the Dispatch chain is the child itself. The reviewer of the child is in the turn that
// confirmed the news of version 5, rendered under the root's gate, when a person closes the root
// and sets it back to todo: read under the child's own gate root, that credit counts nothing, and
// the turn's result folds nothing of it (the claim store's fold holds each item to the gate root
// its issue stands under now), so the child's own row keeps version 4 and the opening it owed.
// Counted without its gate root, the credit would have told the child version 5 of a document of a
// gate it no longer stands under.
func TestAMembersCreditCountsNothingOnceItHoldsItsOwnGate(t *testing.T) {
	t.Parallel()
	r, reviewer, _, token := generationRig(t)
	r.approve(5)
	r.queueNewsTask(pathChild)
	r.delivered(reviewer)
	r.startTurn(reviewer)
	r.wantTold("after the reviewer's turn confirmed the news of version 5", pathChild, record.AudienceHolder, 5, false)

	putCopyRow(t, r.pool, pathRoot, "", record.DoneStatus)
	r.statusEvent(pathRoot, record.DoneStatus)
	putCopyRow(t, r.pool, pathRoot, "", "todo")
	r.statusEvent(pathRoot, "todo")
	root, child := readIssue(t, r.pool, pathRoot), readIssue(t, r.pool, pathChild)
	if root.Generation != 1 || child.Generation != 1 || !record.InLegionsWork(*root) || !child.HoldsGate {
		t.Fatalf("after the root's done and todo %s = %+v and %s = %+v, want both on generation 1, the root back in Legion's work and the child holding its own gate: the case's own premise",
			pathRoot, root, pathChild, child)
	}
	ownGate := func(when string) {
		t.Helper()
		var rows []record.DesignTold
		r.inTx(func(tx pgx.Tx) error {
			var err error
			rows, err = r.records.TreeTold(r.ctx, tx, pathChild, []string{pathChild})
			return err
		})
		for _, row := range rows {
			if row.Issue != pathChild || row.Audience != record.AudienceHolder {
				continue
			}
			if took, known := row.Took(record.DesignStepSpec); !known || took != 4 || !row.OpeningOwed {
				t.Fatalf("%s: %s's holder, read under its own gate root, took version %d (known %t) owing the opening %t, want version 4 owing it: the credit rendered under %s counts nothing there",
					when, pathChild, took, known, row.OpeningOwed, pathRoot)
			}
			return
		}
		t.Fatalf("%s: no holder row of %s under its own gate root among %+v", when, pathChild, rows)
	}
	ownGate("after the root returned to work, the credit standing")

	endTurn(r.ctx, t, reviewer, token)
	ownGate("after the turn ended")
}

// An issue whose Dispatch parent moves it under another gate root takes that gate's documents.
// Here a delivery of the gate it left still holds, confirmed and its turn's result not come, a copy
// that told the issue's holder version 7 of that gate's spec with no earlier version. The gate it
// moves under stands approved at version 4 of a spec of its own and open, so the issue's first
// phase starts under it at the move and takes that gate's baseline, version 4, and every version
// approved after it is news to it. That is all its holder has taken, while that credit stands and
// after its turn's result. Counted by the step and the version numbers alone, the copy would have
// said the holder took version 7 of a document the new gate has approved only to version 4, and
// the news of versions 5, 6 and 7 of it would never be owed. The issue's own generation and the
// old gate root's still stand, so the run alone does not stop the fold: the gate root the daemon
// reads for the issue on the Dispatch chain does (store.ReadGateRootsWith,
// workflowRuntime.gateRootsOf).
//
// The copy is written through the claim store, as a confirmed prompt's is, and the move is the
// real one, Dispatch's new parent in the copy of its issues and its event applied through the
// engine and admission.
func TestACreditOfTheTreeAnIssueLeftCountsNothingInTheTreeThatTakesIt(t *testing.T) {
	t.Parallel()
	const leftRoot, member = "LEGION-900", "LEGION-901"
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Implementing})
	r.told(pathRoot, record.AudienceHolder, 4)
	r.told(pathRoot, record.AudienceArchitect, 4)
	// The gate the issue leaves: a root in Legion's work on its third generation, and a member
	// under it that never started a phase.
	putChainIssue(t, r.pool, r.records, record.Issue{Key: leftRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Title: leftRoot, Tree: leftRoot, Phase: phase.Implementing, Generation: 3,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: "C", HandedOver: true, LastDispatchSeq: 1})
	putChainIssue(t, r.pool, r.records, record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Title: member, Tree: leftRoot, Parent: record.ParentOf(leftRoot), Phase: phase.Admitted, Generation: 1,
		Status: "todo", DispatchStatus: "todo", Rank: "D", HandedOver: true, LastDispatchSeq: 1})
	st, ok := r.sup.deps.Store.(*store.Store)
	if !ok {
		t.Fatalf("the rig's claim store is %T, want the daemon's", r.sup.deps.Store)
	}
	holder := supervise.Claim{Token: mustClaimToken(t, leftRoot, claim.RoleArchitect), Project: "legion", Tree: leftRoot,
		Issue: leftRoot, Role: claim.RoleArchitect, Generation: 3, State: supervise.StateWorking}
	// The copy as the render wrote it under the gate the issue leaves: the member on generation 1,
	// its gate root on generation 3.
	copied := []byte(`{"task":"Read the design.","items":[{"issue":"` + member + `","audience":"holder","step":"spec",` +
		`"artifact":"artifact-900","from":null,"to":7,"generation":1,"root":"` + leftRoot + `","tree_generation":3}]}`)
	at := time.Now().UTC()
	confirmed := supervise.Delivery{ID: "outbox:900", Task: "Read the design.\n\nThe approved design changed.", QueuedAt: at,
		News: copied, Carried: copied, ConfirmedAt: at}
	if err := st.PutClaimAndDelivery(r.ctx, holder, confirmed); err != nil {
		t.Fatalf("write the confirmed task of the gate the issue leaves: %v", err)
	}
	// The negative control: under the gate it was rendered under, the credit counts.
	var before []record.DesignTold
	r.inTx(func(tx pgx.Tx) error {
		var err error
		before, err = r.records.TreeTold(r.ctx, tx, leftRoot, []string{member})
		return err
	})
	if !slices.ContainsFunc(before, func(row record.DesignTold) bool {
		took, known := row.Took(record.DesignStepSpec)
		return row.Issue == member && row.Audience == record.AudienceHolder && known && took == 7
	}) {
		t.Fatalf("the told rows of %s under %s = %+v before the move, want its holder to have taken version 7: the credit this case is about never stood", member, leftRoot, before)
	}

	putCopyRow(t, r.pool, member, pathRoot, "todo")
	issue := readIssue(t, r.pool, member)
	applyFact(t, r.pool, "dispatch", "reparent-"+member, intake.DispatchIssue{Key: member, ResolvedRepo: issue.Repo,
		Seq: issue.LastDispatchSeq + 1, Type: "issue.updated", Status: "todo", Title: issue.Title, Rank: issue.Rank,
		HandedOver: true, Parent: pathRoot}, r.engine, r.admission)
	if moved := readIssue(t, r.pool, member); moved.Parent == nil || *moved.Parent != pathRoot || moved.Generation != 1 || moved.Phase == phase.Admitted {
		t.Fatalf("%s = %+v after the move, want generation 1 under %s with its first phase started: the move this case is about never happened", member, moved, pathRoot)
	}
	r.wantTold("after the move, the credit of the gate the issue left standing", member, record.AudienceHolder, 4, false)

	final := confirmed
	final.Ran = true
	if err := st.PutDelivery(r.ctx, holder.Token, final); err != nil {
		t.Fatalf("record that the old gate's turn ran: %v", err)
	}
	r.wantTold("after the old gate's turn ended, its credit folding into no row under the gate root that took the issue", member,
		record.AudienceHolder, 4, false)
}
