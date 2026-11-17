package workflow

import (
	"strconv"
	"strings"

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
// Each sentence is in a task once. The only task that holds them before it reaches here is the
// stored task of an entry (enter), which the send of that entry owed again hands the queue as it
// was stored (sendOwedEntry), and it ends with the sentence of the entry the row stands at, which
// no other task ends with: so a task that ends with that sentence is returned as it is. It is the
// entry's sentence the guard reads and not both, since the entry's number is all a stored task and
// its row agree on for certain: the commit the row holds can have moved since the entry was
// raised, and the task is then sent as stored, naming the commit it was stored with, or none, so
// the record holds exactly what the worker is sent. A row that holds no entry's task stores no
// task, so no task the queue is handed for it holds the commit's sentence, and the commit is added
// with no guard.
func namingTheReviewRound(task string, role claim.Role, at phase.Phase, row record.PhaseRow) string {
	if role != claim.RoleReviewer || at != phase.Reviewing {
		return task
	}
	commit := reviewedCommit(row.ReviewedHead).part()
	if row.EntryTask == "" {
		return task + commit
	}
	entry := reviewEntry(row.Entry).part()
	if strings.HasSuffix(task, entry) {
		return task
	}
	return task + commit + entry
}
