// Package reviewround writes the two sentences that end each task a reviewer is given: the
// commit the reviewer's newest recorded deciding review read, and the entry of its round. Two
// places write them, and both through Sentences: the workflow, onto each task of the reviewer it
// queues (workflow's namingTheReviewRound), and the claim's send of a task that no such queue
// named, a task the release before this one queued or an operator wrote (supervise's
// Delivery.message), which names neither. One reads them back off a task's end, through Split:
// the daemon's render of a task that carries news of the approved design, which puts that news
// before them so that they stay the task's last two.
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

// Split is task cut before the two sentences Sentences writes at its very end: body, the text
// before them, and sentences, the two exactly as they stand, with named true. It reads only that
// form: a commit of 40 lowercase hexadecimal characters (the head a review's Legion footer names,
// which the record keeps) or `none`, an entry above 0 in digits or `none`, and nothing after the
// entry's period. Any other task answers task whole, "" and false, so a task whose earlier text
// quotes the words, a retry's reason among it, is never cut there.
func Split(task string) (body, sentences string, named bool) {
	label := " " + CommitLabel + ": "
	at := strings.LastIndex(task, label)
	if at < 0 {
		return task, "", false
	}
	commit, entry, ok := strings.Cut(task[at+len(label):], ". "+EntryLabel+": ")
	if !ok || !(commit == none || isHead(commit)) {
		return task, "", false
	}
	entry, ok = strings.CutSuffix(entry, ".")
	if !ok {
		return task, "", false
	}
	var number int64
	if entry != none {
		parsed, err := strconv.ParseInt(entry, 10, 64)
		if err != nil || parsed <= 0 {
			return task, "", false
		}
		number = parsed
	}
	if commit == none {
		commit = ""
	}
	// The two sentences as Sentences writes them from what was read: a number written with a sign
	// or a leading zero reads as one Sentences never writes, and leaves the task whole.
	if Sentences(commit, number) != task[at:] {
		return task, "", false
	}
	return task[:at], task[at:], true
}

// isHead says whether commit is spelled as a head is: 40 lowercase hexadecimal characters.
func isHead(commit string) bool {
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
