package workflow

import (
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
)

// namingTheReviewRound is task as role is given it, standing in phase at with row its phases row,
// among rows, the issue's phases rows as the caller read them. For the reviewer it is task followed
// by the daemon's two sentences of the review round (reviewround.Sentences), always its last two,
// in each phase. In reviewing they name the commit the daemon last recorded for the reviewer
// (record.PhaseRow.ReviewedHead), or `none` where the row holds none, as in the first round of a
// generation, and then the entry the row stands at while the row holds the task of that entry
// (record.PhaseRow.EntryTask), or `none` where it holds no entry's task, as a row a clear emptied
// (record.Store.ClearGeneration) does until the next entry is raised. Outside reviewing they name
// `none` for both: a wake queued there is no task of the round, and the outbox can deliver it
// after the issue is back in reviewing, since a wake names no phase (wakeRow). The entry's
// sentence is the task's last, the shape the reviewer reads it in.
//
// A row's commit is its own agent's (stepRunRow, record.AgentsReviewedHead). Where the row holds no
// commit, as for an agent with no deciding review of its own on the task, the reviewer reads the
// whole change, and in reviewing each of its tasks, wakes and resumes included, names before the
// two sentences the commit another agent's review on the task read (record.AnotherAgentsReview,
// reviewround.AnotherAgentsSentence), where one read one, so it can see what changed since. For any
// other role it is task as it is.
//
// The two sentences are the task's last two whatever the row holds, so no text of the task can
// stand where one of the daemon's sentences stands: a retry's reason or a title that ends with the
// words ` Last reviewed commit: <sha>.` is followed by the daemon's own sentence, `none` where the
// row holds no commit. A `none` changes nothing a review can move: the commit moves only for a
// review of the reviewer's own whose footer names the entry its row stands at while the row holds
// that entry's task (reviewedCommitStays), which reads the row, never the task.
//
// It is one of the two places the sentences enter a task, and its two callers are the places every
// reviewer's task this release composes passes: the queue, for every task it queues (queuedRow,
// through RoundTask, which the daemon's start of a role queued for news alone reads too),
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
// A role the deployment defines whose step at step is of the type review is named the same commits,
// read from the same row by the same rules, in its own sentences: the commit its row holds,
// ` Last reviewed commit: <sha>.` (record.PhaseRow.ReviewedHead, which the workflow's
// recordStepReview writes for a review step alone), and where its row holds none, the commit
// another agent's review step on the task read, ` Commit another agent's review read: <sha>.`, so a
// review step whose agent has no deciding review of its own on the task reads the whole change and
// sees what changed since, as the reviewer does. It is told no `none`: where it gets neither
// sentence it has no commit to read from, as in its first run of a generation with no other agent's
// review before it. Such a step reports with its verdict and submits no review of GitHub's, so it
// gets no entry's sentence.
func namingTheReviewRound(task string, role claim.Role, at phase.Phase, row record.PhaseRow, rows []record.PhaseRow) string {
	if role != claim.RoleReviewer {
		switch {
		case at != phase.Step || claim.IsBuiltin(role):
			return task
		case row.ReviewedHead != "":
			return task + " " + reviewround.CommitLabel + ": " + row.ReviewedHead + "."
		}
		return task + reviewround.AnotherAgentsSentence(record.AnotherAgentsReview(rows, row))
	}
	if at != phase.Reviewing {
		return task + reviewround.Sentences("", 0)
	}
	var entry int64
	if row.EntryTask != "" {
		entry = row.Entry
	}
	return task + reviewround.AnotherAgentsSentence(record.AnotherAgentsReview(rows, row)) + reviewround.Sentences(row.ReviewedHead, entry)
}

// NamesTheRound says whether namingTheReviewRound can add a sentence to a task of role: the
// reviewer's, and a role the deployment defines, whose review step names its commit. So a stored
// task of such a role is marked as named (record.SuperviseRequest.RoundNamed), the queue names
// the others (queuedRow), and the daemon's start executor names such a start queued for news alone
// (its newsForStart).
func NamesTheRound(role claim.Role) bool {
	return role == claim.RoleReviewer || !claim.IsBuiltin(role)
}
