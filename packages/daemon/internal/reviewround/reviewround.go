// Package reviewround writes the two sentences that end each task a reviewer is given: the
// commit the reviewer's newest recorded deciding review read, and the entry of its round. Two
// places write them, and both through Sentences: the workflow, onto each task of the reviewer it
// queues (workflow's namingTheReviewRound) and onto a reviewer's start queued for news alone,
// which the daemon's start executor names when it composes that task (workflow.ReviewerTask,
// from the daemon's newsForStart); and the claim's send of a task that no such queue named, a
// task the release before this one queued or an operator wrote (supervise's Delivery.message),
// which names neither. One reads them back off a task's end, through Split: the daemon's render of
// a task that carries news of the approved design, which puts that news before them so that they
// stay the task's last two.
package reviewround

import (
	"strconv"
	"strings"
)

// CommitLabel labels the commit a reviewer's task names (record.PhaseRow.ReviewedHead). The
// tests of the packages that read a reviewer's task name it by this constant rather than type it
// again.
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

// Split is task cut before the two sentences Sentences writes at its very end: body, the text
// before them, and sentences, the two exactly as they stand, with named true. It reads only that
// form: a commit spelled as a head is (IsHead, the spelling a review's Legion footer must give
// for the record to keep it) or `none`, an entry above 0 in digits or `none`, and nothing after
// the entry's period. Any other task answers task whole, "" and false, so a task whose earlier
// text quotes the words, a retry's reason among it, is never cut there.
func Split(task string) (body, sentences string, named bool) {
	label := " " + CommitLabel + ": "
	at := strings.LastIndex(task, label)
	if at < 0 {
		return task, "", false
	}
	commit, entry, ok := strings.Cut(task[at+len(label):], ". "+EntryLabel+": ")
	if !ok || !(commit == none || IsHead(commit)) {
		return task, "", false
	}
	entry, ok = strings.CutSuffix(entry, ".")
	if !ok {
		return task, "", false
	}
	var number int64
	if entry != none {
		var err error
		if number, err = strconv.ParseInt(entry, 10, 64); err != nil {
			return task, "", false
		}
	}
	// The two sentences as Sentences writes them from what was read. An entry it never writes
	// leaves the task whole: zero or below, which it writes `none`, and a number with a sign or a
	// leading zero. A commit of `none` reads as no commit, which it writes `none` too.
	if Sentences(commit, number) != task[at:] {
		return task, "", false
	}
	return task[:at], task[at:], true
}

// IsHead says whether commit is spelled as GitHub spells every head it reports: exactly 40
// lowercase hexadecimal characters. Nothing shorter is compared with a head, since a prefix names
// a commit only by chance. It is the one test of that spelling: the head a review's Legion footer
// names is read through it (intake.PullRequestReview.LegionHead), so a commit the record keeps
// for the reviewer (record.PhaseRow.ReviewedHead), which Sentences names, is one Split reads back.
func IsHead(commit string) bool {
	if len(commit) != 40 {
		return false
	}
	for _, c := range []byte(commit) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
