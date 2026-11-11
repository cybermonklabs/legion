// Package main: this file holds the two lines `legion state` prints for the controller's merge
// marks — the holds that keep merges back with the merges they are costing (mergeHoldsSummary)
// and the jump marks with the jumps already spent (mergeJumpsSummary) — as the state route
// serves them (api.State.MergeHolds, MergeHeld, MergeJumps, MergeJumpRecords).
package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// mergeHoldsSummary is the one line an operator reads for the paths the controller is holding
// still (api.State.MergeHolds) and the merges those holds are costing (api.State.MergeHeld):
// how many holds stand, each with its paths, the repository it holds them in, its reason, the
// controller session that set the list and when it stops holding; then the issues whose merge
// is waiting on one of them, each with the moment its row runs again.
//
// The two are one line because neither answers the operator's question alone: a hold with no
// merge behind it is costing nothing, and a held merge with no hold named would read as a merge
// the daemon has lost. The paths and the reason are the controller's own words, which nothing
// verifies, so both are Go-quoted: a newline in one would otherwise forge a row of this summary
// (claims.go's quotedOrNone). A hold list that is empty while a merge is still held is a real
// moment — the hold ended and the row it caught has not run again yet — so the line says so
// rather than reading as nothing held.
func mergeHoldsSummary(holds []api.MergeHoldView, held []api.MergeHeldView) string {
	if len(holds) == 0 && len(held) == 0 {
		return "merge holds: none"
	}
	standing := "none set"
	if len(holds) > 0 {
		named := make([]string, len(holds))
		for i, hold := range holds {
			paths := make([]string, len(hold.Paths))
			for j, path := range hold.Paths {
				paths[j] = strconv.Quote(path)
			}
			where := "every repository"
			if hold.Repository != "" {
				where = hold.Repository
			}
			named[i] = fmt.Sprintf("%s in %s, %s, set by %s until %s", strings.Join(paths, " "), where,
				quotedOrNone(hold.Reason), orNone(hold.Session), hold.EndsAt.UTC().Format(time.RFC3339))
		}
		standing = fmt.Sprintf("%d — %s", len(holds), strings.Join(named, "; "))
	}
	if len(held) == 0 {
		return "merge holds: " + standing + "; no merge is held"
	}
	waiting := make([]string, len(held))
	for i, issue := range held {
		waiting[i] = issue.Issue + " due " + issue.DueAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("merge holds: %s; holding %d: %s", standing, len(held), strings.Join(waiting, ", "))
}

// mergeJumpsSummary is the one line an operator reads for the merge queue the controller has
// asked to jump or means to ask: the issues carrying a live jump mark (api.State.MergeJumps),
// each with its reason and the controller session that set it, and then the newest jump of each
// repository that has had one (api.State.MergeJumpRecords), each with the queue it asked to go
// ahead of.
//
// The marks and the records are one line because they are the two halves of one decision: a
// mark is what the controller has asked for and a record what Legion then asked GitHub for, and
// a mark that is gone with no record beside it is a jump that was never asked. Each reason is
// the controller's own word, Go-quoted for the reason every other agent-supplied field on these
// lines is.
func mergeJumpsSummary(jumps []api.MergeJumpView, records []api.MergeJumpRecordView) string {
	if len(jumps) == 0 && len(records) == 0 {
		return "merge jumps: none"
	}
	marked := "no issue is marked"
	if len(jumps) > 0 {
		named := make([]string, len(jumps))
		for i, jump := range jumps {
			named[i] = fmt.Sprintf("%s %s, set by %s", jump.Issue, quotedOrNone(jump.Reason), orNone(jump.Session))
		}
		marked = fmt.Sprintf("%d marked — %s", len(jumps), strings.Join(named, "; "))
	}
	if len(records) == 0 {
		return "merge jumps: " + marked + "; none has been spent"
	}
	spent := make([]string, len(records))
	for i, jump := range records {
		spent[i] = fmt.Sprintf("%s %s #%d at %s %s, %s", jump.Repository, jump.Issue, jump.Number,
			jump.At.UTC().Format(time.RFC3339), quotedOrNone(jump.Reason), jumpAsked(jump))
	}
	return fmt.Sprintf("merge jumps: %s; the newest of each repository: %s", marked, strings.Join(spent, "; "))
}

// jumpAsked is what one spent jump asked for and what became of it, as mergeJumpsSummary names
// it: the queue GitHub answered just before the request, each entry by number and state and how
// long that queue was (jumpQueueAsked), and then the record's own outcome (jumpOutcomeTold).
//
// The queue is the queue the jump asked to go ahead of and never where the merge stands,
// because the record is written before the mutation is sent and is read again here long after:
// what GitHub did with the request is on GitHub. That holds for every outcome, the one GitHub
// took among them, so no line of this summary places a merge in a queue.
func jumpAsked(jump api.MergeJumpRecordView) string {
	return jumpQueueAsked(jump) + ", " + jumpOutcomeTold(jump)
}

// jumpQueueAsked is the queue one jump was asked of, as the record read it just before the
// request. A queue GitHub did not answer is told apart from an empty one, since the record keeps
// the two apart (record.MergeJumpRecord.EntriesUnknown) and an operator reading "an empty queue"
// of an unread queue would be reading a guess.
func jumpQueueAsked(jump api.MergeJumpRecordView) string {
	if jump.EntriesUnknown {
		return "asked to go ahead of a queue GitHub did not answer"
	}
	if len(jump.Entries) == 0 {
		return "asked to go ahead of an empty queue"
	}
	ahead := make([]string, len(jump.Entries))
	for i, entry := range jump.Entries {
		ahead[i] = fmt.Sprintf("#%d %s", entry.Number, entry.State)
	}
	return fmt.Sprintf("asked to go ahead of %d queued: %s", jump.EntriesTotal, strings.Join(ahead, ", "))
}

// jumpOutcomeTold is what became of one jump's own request, a line of its own for each of the
// four values the record carries (record.MergeJumpOutcome). Each says what the daemon learned
// and stops there: `asked` is a request whose end Legion never learned, not a jump that went
// through; `accepted` is GitHub taking the request, which is no place in the queue; and the two
// that did not take print the words that came back and add no verdict of their own, since
// "refused by GitHub" over a request GitHub never saw sends an operator to the repository's
// merge-queue settings for a fault in the network.
//
// A value this build does not know is printed as itself rather than read as any of the four: a
// daemon one release ahead of this command would otherwise have its outcome rendered as the
// nearest one here.
func jumpOutcomeTold(jump api.MergeJumpRecordView) string {
	switch jump.Outcome {
	case record.MergeJumpAsked:
		return "and Legion never learned what GitHub did with the request"
	case record.MergeJumpAccepted:
		return "and GitHub took the request"
	case record.MergeJumpRefused:
		return "and the jump did not take: " + quotedOrNone(jump.Answer)
	case record.MergeJumpUnknown:
		return "and what became of the request is unknown: " + quotedOrNone(jump.Answer)
	}
	return fmt.Sprintf("and the record carries an outcome this command does not know, %s: %s",
		quotedOrNone(string(jump.Outcome)), quotedOrNone(jump.Answer))
}
