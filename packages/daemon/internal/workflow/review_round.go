package workflow

import (
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
)

// namingTheReviewRound is task as role is given it, standing in phase at with row its phases row.
// For the reviewer it is task followed by the daemon's two sentences of the review round
// (reviewround.Sentences), always its last two, in each phase. In reviewing they name the commit
// the daemon last recorded for the reviewer (record.PhaseRow.ReviewedHead), or `none` where the row
// holds none, as in the first round of a generation, and then the entry the row stands at while
// the row holds the task of that entry (record.PhaseRow.EntryTask), or `none` where it holds no
// entry's task, as a row a clear emptied (record.Store.ClearGeneration) does until the next entry
// is raised. Outside reviewing they name `none` for both: a wake queued there is no task of the
// round, and the outbox can deliver it after the issue is back in reviewing, since a wake names no
// phase (wakeRow). The entry's sentence is the task's last, the shape the reviewer reads it in.
// For any other role it is task as it is, with one sentence more where its step is of the type
// review and its row holds a commit (the last paragraph).
//
// The two sentences are the task's last two whatever the row holds, so no text of the task can
// stand where one of the daemon's sentences stands: a retry's reason or a title that ends with the
// words ` Last reviewed commit: <sha>.` is followed by the daemon's own sentence, `none` where the
// row holds no commit. A `none` changes nothing a review can move: the commit moves only for a
// review of the reviewer's own whose footer names the entry its row stands at while the row holds
// that entry's task (reviewedCommitStays), which reads the row, never the task.
//
// It is one of the two places the sentences enter a task, and its two callers are the places every
// reviewer's task this release composes passes: the queue, for every task it queues (queuedRow),
// and the raise of an entry, which stores its task through it (enter). The other is the claim's
// send of a task no such queue named (supervise's Delivery.message), which names `none` for both.
// So the reviewer reads the difference from the commit to the head, or the whole change for
// `none`, and writes the number of the newest entry a task of its session named into the footer of
// its review.
//
// Each sentence is in a task once because of a fact the start carries, never a reading of its
// text: the stored task of an entry (enter), which already holds them, is sent marked so
// (record.SuperviseRequest.RoundNamed), and the queue hands it none. It is then sent as stored,
// naming the commit it was stored with, or `none`, though the row's commit has moved since, so
// the record holds exactly what the worker is sent.
//
// Any other role whose step is of the type review gets one sentence, the commit its own step's
// newest run read, ` Last reviewed commit: <sha>.`, which its row holds (record.PhaseRow.ReviewedHead),
// in the phase its step runs as (plan.RunsAs): a role the deployment defines, at step, and the role
// acceptance, at accepting, whether the fresh agent of acceptance or the owner does it. Only a
// review step's run writes the row's commit (the workflow's recordStepReview, from the step's
// completion; recordOwnersReview, the head the owner's acceptance read), and the first run of a
// second step of the role starts from the row of its step before (stepRunRow), so a role with no
// review step never has one, and none is named where the row holds no commit, as in the step's first
// run of a generation. Such a step reports with its verdict and submits no review of GitHub's, so it
// gets no entry's sentence. A review step of the role reviewer the owner does gets the reviewer's
// two sentences, as the reviewer does: its row is the reviewer's.
func namingTheReviewRound(task string, role claim.Role, at phase.Phase, row record.PhaseRow) string {
	if role != claim.RoleReviewer {
		if row.ReviewedHead != "" && plan.RunsAs(string(role)) == at {
			return task + " " + reviewround.CommitLabel + ": " + row.ReviewedHead + "."
		}
		return task
	}
	if at != phase.Reviewing {
		return task + reviewround.Sentences("", 0)
	}
	var entry int64
	if row.EntryTask != "" {
		entry = row.Entry
	}
	return task + reviewround.Sentences(row.ReviewedHead, entry)
}

// namesTheRound says whether namingTheReviewRound can add a sentence to a task of role: the
// reviewer's, and the task of each role that is none of Legion's six claim roles
// (claim.IsBuiltin), whose review step names its commit: a role the deployment or the plan
// defines, and acceptance. So a stored task of such a role is marked as named
// (record.SuperviseRequest.RoundNamed), and the queue names the others (queuedRow).
func namesTheRound(role claim.Role) bool {
	return role == claim.RoleReviewer || !claim.IsBuiltin(role)
}
