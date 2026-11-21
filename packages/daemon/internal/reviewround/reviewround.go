// Package reviewround writes the two sentences that end each task a reviewer is given: the
// commit the reviewer's newest recorded deciding review read, and the entry of its round. Two
// places write them, and both through Ending: the workflow, onto each task of the reviewer it
// queues (workflow's namingTheReviewRound), and the claim's send of a task that no such queue
// named, a task the release before this one queued or an operator wrote (supervise's
// Delivery.message), which names neither.
package reviewround

import (
	"strconv"
	"strings"
)

// CommitLabel labels the commit a reviewer's task names (record.PhaseRow.ReviewedHead). The
// reviewer's prompt and the merge gate of skill://legion-worker name it to the reviewer, which a
// test ties to it, and the tests of the packages that read a reviewer's task name it by this
// constant rather than type it again.
const CommitLabel = "Last reviewed commit"

// EntryLabel labels the entry of the round a reviewer's task names (record.PhaseRow.Entry). The
// reviewer writes that number into the Legion footer of each review it submits for the task
// (intake.PullRequestReview.LegionEntry), and only a review that names the entry that stands moves
// the commit the reviewer read (workflow's reviewedCommitStays).
const EntryLabel = "Review entry"

// none is what each sentence names where there is no value for it: no commit, or no entry. A
// commit is 40 hexadecimal characters and an entry a number, so neither is ever this word.
const none = "none"

// Sentences is the two sentences that end a reviewer's task, in this order:
// " Last reviewed commit: <commit>." and " Review entry: <entry>.", each naming `none` where
// commit is "" or entry is not above 0. It is the one place either sentence is written.
func Sentences(commit string, entry int64) string {
	if commit == "" {
		commit = none
	}
	named := none
	if entry > 0 {
		named = strconv.FormatInt(entry, 10)
	}
	return " " + CommitLabel + ": " + commit + ". " + EntryLabel + ": " + named + "."
}

// Ending is task followed by the two sentences (Sentences) on a line of their own, the task's
// last line, with no space in front of them. A task of the daemon is a record rendered as lines
// of fields and blocks (texts.Render), so the sentences stand after everything else, a block of
// another author's words included, and join no field. The two sentences stay word for word as
// the reviewer's prompt names them, so ` Review entry: <n>.` is still the task's last sentence.
// An empty task is the line alone.
func Ending(task, commit string, entry int64) string {
	line := strings.TrimPrefix(Sentences(commit, entry), " ")
	if task == "" {
		return line
	}
	return task + "\n" + line
}
