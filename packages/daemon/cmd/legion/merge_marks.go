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
// jumped or means to jump: the issues carrying a live jump mark (api.State.MergeJumps), each
// with its reason and the controller session that set it, and then the newest jump of each
// repository that has had one (api.State.MergeJumpRecords), each with what it went in front of.
//
// The marks and the records are one line because they are the two halves of one decision: a
// mark is what the controller has asked for and a record what it got, and a mark that is gone
// with no record beside it is a jump that never happened. Each reason is the controller's own
// word, Go-quoted for the reason every other agent-supplied field on these lines is.
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
			jump.At.UTC().Format(time.RFC3339), quotedOrNone(jump.Reason), displacedBy(jump))
	}
	return fmt.Sprintf("merge jumps: %s; the newest of each repository: %s", marked, strings.Join(spent, "; "))
}

// displacedBy is what one spent jump went in front of, as mergeJumpsSummary names it: the
// entries GitHub's queue held when the daemon submitted, each by number and state, and how long
// that queue was. A refused jump says so with GitHub's own answer, because the mark was spent
// either way and a refusal read as a jump would name a pull request that never moved; a queue
// GitHub did not answer is told apart from an empty one, since the record keeps the two apart
// (record.MergeJumpRecord.EntriesUnknown) and an operator reading "in front of nothing" of an
// unread queue would be reading a guess.
func displacedBy(jump api.MergeJumpRecordView) string {
	if jump.Refused {
		return "refused by GitHub: " + quotedOrNone(jump.Refusal)
	}
	if jump.EntriesUnknown {
		return "in front of a queue GitHub did not answer"
	}
	if len(jump.Entries) == 0 {
		return "in front of an empty queue"
	}
	ahead := make([]string, len(jump.Entries))
	for i, entry := range jump.Entries {
		ahead[i] = fmt.Sprintf("#%d %s", entry.Number, entry.State)
	}
	return fmt.Sprintf("in front of %d queued: %s", jump.EntriesTotal, strings.Join(ahead, ", "))
}
