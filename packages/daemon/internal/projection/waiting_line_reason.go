// Package projection: this file holds the waiting line's own sentence — what every member of
// the line is told about its place in it (api.WaitingLineView.Reason) — and the two helpers
// that count in English for it. The fill of the line is its one caller (Project, project.go),
// and waiting_line_reason_test.go beside it renders the entries themselves and reads no
// database. The sentences of `why` are the file next to it (why.go).
package projection

import (
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// waitingLineReason is the sentence a person reads for one member of the waiting line
// (api.WaitingLineView.Reason): what put it there and when, where it stands in its own part of
// the line, and what that place means for the slot it waits for. It reads the entry the line
// holds for that member (record.LineEntry), partLength, the length of the part the member
// stands in, and otherPart, the length of the part it does not, and nothing else — no record of
// its own, no second count — so the sentence has one source and every number in it is the
// member's own. The two lengths are the caller's because the entry carries neither: a part's
// length is a fact about the line rather than about the member.
//
// The last clause is the holder's wherever the member already holds one of the cap's slots
// (record.LineEntry.Holds), in every kind of both parts and never read off the kind: a member
// of any kind can hold a slot and any kind can stand in the line without one, and telling a
// holder that it queues for a free slot is the defect this sentence exists to stop. A member of
// the returned part whose record knows no moment (record.LineEntry.Since) names none, and a
// member of the starting part with nothing in the other part says nothing about it: both
// clauses end at the words that are true.
func waitingLineReason(entry record.LineEntry, partLength, otherPart int) string {
	switch entry.Part {
	case record.PartReturned:
		came := ""
		switch entry.Kind {
		case record.LineReturning:
			came = "Its work came back"
		case record.LineAnswered:
			came = "A person answered it, and its work came back"
		case record.LineFirstPhase:
			came = "Its design gate opened"
		}
		if !entry.Since.IsZero() {
			came += " at " + entry.Since.UTC().Format(time.RFC3339)
		}
		place := fmt.Sprintf("It is %s of %s whose work came back", englishOrdinal(entry.PartPlace), issuesCounted(partLength))
		if entry.Holds {
			return came + ". " + place + "." + holdsItsSlot
		}
		takes := "they take"
		if partLength == 1 {
			takes = "it takes"
		}
		return fmt.Sprintf("%s. %s, and %s the free slots first.", came, place, takes)
	case record.PartStarting:
		rank := ""
		switch entry.Kind {
		case record.LineQueuedRoot:
			rank = "its own Dispatch rank"
		case record.LineFirstPhase:
			// The key of the tree root whose rank placed this member, read off the entry's own
			// issue, which is where the line leaves it (record.LineEntry.RootRank).
			rank = "the Dispatch rank of its tree root " + entry.Issue.Tree
		}
		started := "have"
		if partLength == 1 {
			started = "has"
		}
		place := fmt.Sprintf("It is %s of %s that %s not started, by %s",
			englishOrdinal(entry.PartPlace), issuesCounted(partLength), started, rank)
		switch {
		case entry.Holds:
			return "It has not started. " + place + "." + holdsItsSlot
		case otherPart == 0:
			return "It has not started. " + place + "."
		}
		stand := "stand"
		if otherPart == 1 {
			stand = "stands"
		}
		return fmt.Sprintf("It has not started. %s, and %s whose work came back %s ahead of all of them.",
			place, issuesCounted(otherPart), stand)
	}
	// Every entry the line builds stands in one of the two parts, set where the line places it
	// (record.Waiting's final loops), so no member reaches this. A part this release has no
	// sentence for renders nothing, and its member is served no reason rather than a sentence
	// about a line it does not stand in.
	return ""
}

// holdsItsSlot is the last clause of every member that already holds one of the cap's slots: it
// waits for no slot to free, so the clause that tells the rest of the line what it queues for is
// this truth instead (waitingLineReason). It opens on a space because it follows the full stop
// the place clause ends on.
const holdsItsSlot = " It already holds its admission slot, so it resumes at the next pass whatever the cap says."

// englishOrdinal is n as a person writes a place: 1st, 2nd, 3rd and `th` for the rest, with 11,
// 12 and 13 taking `th` whatever their last digit says — so 11th, 12th and 13th, and 21st, 22nd,
// 23rd, 101st and 111th. The places of the line count from 1 (record.LineEntry.PartPlace), so no
// other number reaches it.
func englishOrdinal(n int) string {
	suffix := "th"
	if tens := n % 100; tens < 11 || tens > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// issuesCounted is a count of issues with the noun a person reads it with: `1 issue`, and
// `7 issues` for every other count. The verb that follows is the caller's, since the clauses
// that carry this count take different ones (waitingLineReason).
func issuesCounted(n int) string {
	if n == 1 {
		return "1 issue"
	}
	return fmt.Sprintf("%d issues", n)
}
