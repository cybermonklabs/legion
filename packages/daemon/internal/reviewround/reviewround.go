// Package reviewround writes the daemon's sentences of a reviewer's review round: the two that end
// each task a reviewer is given, the commit the reviewer's newest recorded deciding review read and
// the entry of its round (Sentences), and the one before them where the reviewer's row holds no
// commit and another agent's review on the task read one (AnotherAgentsSentence). Two places write
// the two: the workflow, onto each task of the reviewer it queues (workflow's
// namingTheReviewRound) and onto a reviewer's start queued for news alone, which the daemon's start
// executor names when it composes that task (workflow.RoundTask, from the daemon's newsForStart);
// and the claim's send of a task that no such queue named, a task the release before this one
// queued or an operator wrote (supervise's Delivery.message), which names neither. Only the
// workflow writes the one before them, on a reviewer's task and on the task of a deployment role's
// review step. One reads the two back off a task's end, through Split: the daemon's render of a
// task that carries news of the approved design, which puts that news before them so that they
// stay the task's last two.
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
// the commit the reviewer read (workflow's reviewedCommitStays). A review step that ends by its
// verdict records the commit its report names instead (workflow's recordStepReview), and the
// owner's act at a review step of its own the pull request's head (workflow's recordOwnersReview).
const EntryLabel = "Review entry"

// none is what each sentence names where there is no value for it: no commit, or no entry. A
// commit is 40 hexadecimal characters and an entry a number, so neither is ever this word.
const none = "none"

// AnotherAgentLabel labels the commit another agent's review on the same task read, which a review
// step's task names where the step's own row holds no commit (record.AnotherAgentsReview): its
// agent has no deciding review of its own on the task, so its own sentence names none and it reads
// the whole change, and this sentence lets it see what changed since the other agent's review.
const AnotherAgentLabel = "Commit another agent's review read"

// AnotherAgentsSentence is " Commit another agent's review read: <commit>.", and "" where commit
// is "". On a reviewer's task it stands before the two sentences of the round (Sentences), which
// stay the task's last two, so the way back of the reviewed-head migration, which strips those two
// at a task's end (store/migrations/20261116234034_phase_reviewed_head.up.sql, its header's three
// regexp_replace updates), strips them alone.
func AnotherAgentsSentence(commit string) string {
	if commit == "" {
		return ""
	}
	return " " + AnotherAgentLabel + ": " + commit + "."
}

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
// names is read through it (intake.PullRequestReview.LegionHead), and so is the commit a completion
// of a deployment role's review step names (workflow's recordStepReview), so each commit either
// writes as a review's (record.PhaseRow.ReviewedHead) is spelled as a head, and the one Sentences
// names for the reviewer is one Split reads back.
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
