package workflow

import (
	"strconv"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ReviewedCommitLabel labels the commit a later review round's task names (reviewedCommit.part),
// the commit the daemon last recorded for the reviewer (record.PhaseRow.ReviewedHead). The
// reviewer's prompt and the merge gate of skill://legion-worker name it to the reviewer, which a
// test ties to it, and the tests of the packages that read a reviewer's task name it by this
// constant rather than type it again.
const ReviewedCommitLabel = "Last reviewed commit"

// ReviewEntryLabel labels the entry of the round a reviewer's task in reviewing names
// (reviewEntry.part), the number the reviewer's phases row holds (record.PhaseRow.Entry). The
// reviewer writes that number into the Legion footer of each review it submits for the task
// (intake.PullRequestReview.LegionEntry), and only a review that names the entry that stands moves
// the commit the reviewer read (review). The reviewer's prompt and the merge gate of
// skill://legion-worker name it to the reviewer, which a test ties to it.
const ReviewEntryLabel = "Review entry"

// reviewedCommit is the commit the daemon last recorded for the reviewer, as the reviewer's task
// names it (namingTheReviewRound), and "" where the task names none. It holds the commit and not
// the sentence that names it, and part writes that sentence, so no value of the type reaches a
// task as anything but that sentence or nothing.
type reviewedCommit string

// part is the sentence a task names c with, " Last reviewed commit: <sha>."
// (ReviewedCommitLabel), and "" for no commit: the one place the sentence is written.
func (c reviewedCommit) part() string {
	if c == "" {
		return ""
	}
	return " " + ReviewedCommitLabel + ": " + string(c) + "."
}

// reviewEntry is the entry of the round a reviewer's task in reviewing names (ReviewEntryLabel),
// 0 where the task names none. Like reviewedCommit it has a type of its own, so no other number
// reaches a task through part.
type reviewEntry int64

// part is the sentence a task names n with, " Review entry: <n>." (ReviewEntryLabel), and "" for
// no entry: the one place the sentence is written.
func (n reviewEntry) part() string {
	if n <= 0 {
		return ""
	}
	return " " + ReviewEntryLabel + ": " + strconv.FormatInt(int64(n), 10) + "."
}

// namingTheReviewRound is task as role is given it, standing in phase at with row its phases row,
// ending with the sentences of the reviewer's review round. For the reviewer of an issue in
// reviewing they are the commit the daemon last recorded for it (record.PhaseRow.ReviewedHead,
// reviewedCommit.part) while the row holds one, and then the entry the row stands at
// (reviewEntry.part) while the row holds the task of that entry (record.PhaseRow.EntryTask), which
// is the task's LAST sentence, the shape the reviewer reads it in. For any other role or phase,
// and for a row that holds neither, it is task as it is. A row a clear emptied
// (record.Store.ClearGeneration) holds neither: no review moves the commit for its entry until
// the next entry is raised (review), and the first review of a generation has no commit before it.
//
// It is the one place either sentence enters a task, and both callers are the places every
// reviewer's task passes: the queue, for every task it queues (queuedRow), and the raise of an
// entry, which stores its task through it (enter). So every task the reviewer is given in
// reviewing names them, a wake's task (WakeTask) and each resume included, and a start added
// later cannot be queued without them: the reviewer reads the difference from that commit to the
// head, and writes the number of the newest entry a task of its session named into the footer of
// its review, and only a review that names the entry that stands moves the commit it read.
//
// It appends the sentences to task whatever task's text holds: a task's text can hold the words of
// either sentence, as a retry's reason that quotes them does, and the daemon's own sentences still
// follow it. Each sentence is in a task once because of a fact the start carries, never a reading
// of its text: the stored task of an entry (enter), which already holds them, is sent marked so
// (record.SuperviseRequest.RoundNamed), and the queue hands it none. It is then sent as stored,
// naming the commit it was stored with, or none, though the row's commit has moved since, so the
// record holds exactly what the worker is sent.
func namingTheReviewRound(task string, role claim.Role, at phase.Phase, row record.PhaseRow) string {
	if role != claim.RoleReviewer || at != phase.Reviewing {
		return task
	}
	task += reviewedCommit(row.ReviewedHead).part()
	if row.EntryTask == "" {
		return task
	}
	return task + reviewEntry(row.Entry).part()
}
