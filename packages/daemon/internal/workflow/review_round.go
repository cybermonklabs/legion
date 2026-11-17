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

// noneNamed is what each sentence of a review round names where the reviewer's row holds no value
// for it (reviewedCommit.part, reviewEntry.part): no commit, or no entry's task. A commit is 40
// hexadecimal characters and an entry a number, so neither is ever this word.
const noneNamed = "none"

// reviewedCommit is the commit the daemon last recorded for the reviewer, as the reviewer's task
// names it (namingTheReviewRound), and "" where the row holds none. It holds the commit and not
// the sentence that names it, and part writes that sentence, so no value of the type reaches a
// task as anything but that sentence.
type reviewedCommit string

// part is the sentence a task names c with, " Last reviewed commit: <sha>." (ReviewedCommitLabel),
// and " Last reviewed commit: none." for no commit: the one place the sentence is written.
func (c reviewedCommit) part() string {
	if c == "" {
		c = noneNamed
	}
	return " " + ReviewedCommitLabel + ": " + string(c) + "."
}

// reviewEntry is the entry of the round a reviewer's task in reviewing names (ReviewEntryLabel),
// 0 where the task names none. Like reviewedCommit it has a type of its own, so no other number
// reaches a task through part.
type reviewEntry int64

// part is the sentence a task names n with, " Review entry: <n>." (ReviewEntryLabel), and
// " Review entry: none." for no entry: the one place the sentence is written.
func (n reviewEntry) part() string {
	if n <= 0 {
		return " " + ReviewEntryLabel + ": " + noneNamed + "."
	}
	return " " + ReviewEntryLabel + ": " + strconv.FormatInt(int64(n), 10) + "."
}

// namingTheReviewRound is task as role is given it, standing in phase at with row its phases row.
// For the reviewer of an issue in reviewing it is task followed by the daemon's two sentences of
// the review round, always its last two, in this order: the commit the daemon last recorded for the
// reviewer (record.PhaseRow.ReviewedHead, reviewedCommit.part), or `none` where the row holds none,
// as in the first round of a generation; then the entry the row stands at (reviewEntry.part) while
// the row holds the task of that entry (record.PhaseRow.EntryTask), or `none` where it holds no
// entry's task, as a row a clear emptied (record.Store.ClearGeneration) does until the next entry
// is raised. The entry's sentence is the task's last, the shape the reviewer reads it in. For any
// other role or phase it is task as it is.
//
// The two sentences are the task's last two whatever the row holds, so no text of the task can
// stand where one of the daemon's sentences stands: a retry's reason or a title that ends with the
// words ` Last reviewed commit: <sha>.` is followed by the daemon's own sentence, `none` where the
// row holds no commit. A `none` changes nothing a review can move: the commit moves only for a
// review of the reviewer's own whose footer names the entry its row stands at while the row holds
// that entry's task (reviewedCommitStays), which reads the row, never the task.
//
// It is the one place either sentence enters a task, and both callers are the places every
// reviewer's task passes: the queue, for every task it queues (queuedRow), and the raise of an
// entry, which stores its task through it (enter). So every task the reviewer is given in
// reviewing names them, a wake's task (WakeTask) and each resume included, and a start added
// later cannot be queued without them: the reviewer reads the difference from that commit to the
// head, or the whole change for `none`, and writes the number of the newest entry a task of its
// session named into the footer of its review.
//
// Each sentence is in a task once because of a fact the start carries, never a reading of its
// text: the stored task of an entry (enter), which already holds them, is sent marked so
// (record.SuperviseRequest.RoundNamed), and the queue hands it none. It is then sent as stored,
// naming the commit it was stored with, or `none`, though the row's commit has moved since, so
// the record holds exactly what the worker is sent.
func namingTheReviewRound(task string, role claim.Role, at phase.Phase, row record.PhaseRow) string {
	if role != claim.RoleReviewer || at != phase.Reviewing {
		return task
	}
	var entry reviewEntry
	if row.EntryTask != "" {
		entry = reviewEntry(row.Entry)
	}
	return task + reviewedCommit(row.ReviewedHead).part() + entry.part()
}
