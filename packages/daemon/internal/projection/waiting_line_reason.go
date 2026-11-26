// Package projection: this file holds the waiting line's own sentence — what every member of
// the line is told about its place in it (api.WaitingLineView.Reason) — and the two helpers
// that count in English for it. The fill of the line is its one caller (Project, project.go),
// through waitingLineReasonUnder under the rule the state is read under, and
// waiting_line_reason_test.go beside it renders the entries themselves and reads no
// database. The sentences of `why` are the file next to it (why.go).
package projection

import (
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// waitingLineReason is the sentence a person reads for one member of the waiting line
// (api.WaitingLineView.Reason): what put it there and when, the priority it waits at and whose
// that is, where it stands in its band's part of the line, and what that place means for the slot
// it waits for. It reads the entry the line holds for that member (record.LineEntry), partLength,
// the length of the part of its priority's band the member stands in, and otherPart, the length of
// that band's other part, and nothing else — no record of its own, no second count — so the
// sentence has one source and every number in it is the member's own. The two lengths are the
// caller's because the entry carries neither: a part's length is a fact about the line rather
// than about the member.
//
// The last clause is the holder's wherever the member already holds one of the cap's slots
// (record.LineEntry.Holds), in every kind of both parts and never read off the kind: a member
// of any kind can hold a slot and any kind can stand in the line without one, and telling a
// holder that it queues for a free slot is the defect this sentence exists to stop. A member of
// the returned part whose record knows no moment (record.LineEntry.Since) names none, and a
// member of the starting part with nothing in the other part says nothing about it: both
// clauses end at the words that are true. A member that gave its slot back to wait on another
// issue (record.LineWaitingOnIssue) is told it waits its turn (waitsItsTurn) only while its
// entry still names a wait (record.LineEntry.Waits); with none left, the sentence says the wait
// has ended and ends on the clause the rest of the returned part reads.
//
// This is the sentence under the daemon's own rule, which bands the line by priority; the fill
// renders it under the rule the state is read under (waitingLineReasonUnder).
func waitingLineReason(entry record.LineEntry, partLength, otherPart int) string {
	return waitingLineReasonUnder(true, entry, partLength, otherPart)
}

// waitingLineReasonUnder is waitingLineReason under a rule that bands the line by priority, or
// under one that does not (banded, record.SlotRule.PriorityFirst). The daemon's own rule bands
// it, and the sentence names the priority that placed the member and counts within its band,
// which is waitingLineReason. Under a rule that does not, no priority placed a member
// (record.LineEntry.Priority is NoPriority on every entry), so the sentence names none and counts
// within the member's part of the whole line.
func waitingLineReasonUnder(banded bool, entry record.LineEntry, partLength, otherPart int) string {
	// placed opens the place clause and band follows each count, each with the space that joins
	// it, so a sentence under a rule that bands nothing reads without either.
	placed, band := "", ""
	if banded {
		placed, band = priorityPlaced(entry)+" ", " "+bandWords(entry.Priority)
	}
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
		case record.LineWaitingOnIssue:
			came = "It gave its admission slot back to wait on another issue"
			if len(entry.Waits) > 0 {
				came = "It gave its admission slot back to wait on " + strings.Join(entry.Waits, ", ")
			}
		}
		if !entry.Since.IsZero() {
			came += " at " + entry.Since.UTC().Format(time.RFC3339)
		}
		// A mark that still stands with nothing left to wait on: its blockers closed while the
		// cap was full, and it now takes the next free slot like the rest of its part.
		if entry.Kind == record.LineWaitingOnIssue && len(entry.Waits) == 0 {
			came += ", and that wait has ended"
		}
		// The priority sentence opens the place clause, so every return below carries it.
		place := fmt.Sprintf("%sIt is %s of %s%s whose work came back",
			placed, englishOrdinal(entry.PartPlace), issuesCounted(partLength), band)
		if entry.Holds {
			return came + ". " + place + "." + holdsItsSlot
		}
		// The promotion passes over a member that still waits on an issue (its Waits), so it
		// does not take the free slots before the rest of its band (admit.releaseSlotsForWaits).
		// A member whose waits all ended takes them like the rest of its part.
		if entry.Kind == record.LineWaitingOnIssue && len(entry.Waits) > 0 {
			return came + ". " + place + "." + waitsItsTurn
		}
		takes := "they take"
		if partLength == 1 {
			takes = "it takes"
		}
		// Within its band the returned part goes first; every band of a higher priority goes
		// before it, so the clause names what it goes before and nothing more.
		return fmt.Sprintf("%s. %s, and %s the free slots before the issues%s that have not started.", came, place, takes, band)
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
		place := fmt.Sprintf("%sIt is %s of %s%s that %s not started, by %s",
			placed, englishOrdinal(entry.PartPlace), issuesCounted(partLength), band, started, rank)
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
		// The returned part of its own band alone stands ahead of it there: otherPart counts
		// that band's returned part and no other band's.
		return fmt.Sprintf("It has not started. %s, and %s%s whose work came back %s ahead of all of them.",
			place, issuesCounted(otherPart), band, stand)
	}
	// Every entry the line builds stands in one of the two parts, set where the line places it
	// (record.Waiting's final loops), so no member reaches this. A part this release has no
	// sentence for renders nothing, and its member is served no reason rather than a sentence
	// about a line it does not stand in.
	return ""
}

// bandWords names the band an entry waits in, as the counts of its sentence read it: `at P1`, or
// `with no priority` for an entry Dispatch resolves no priority for, or, with an older Dispatch
// that resolves none, one that neither it nor its tree root has a priority for.
func bandWords(priority record.Priority) string {
	if priority == record.NoPriority {
		return "with no priority"
	}
	return "at " + priority.String()
}

// priorityPlaced is the sentence that says which priority placed entry in the line and whose it
// is (record.LineEntry.Priority). Where Dispatch resolves the issue's priority
// (record.Resolved.Known) it is the issue's own, or the one of the issue Dispatch names in
// `priority_from`, the nearest above it on its Dispatch parent chain that has one, never the Legion
// tree root; else, on a Dispatch release that resolves none, the issue's own, its tree root's, or
// none on either. An entry with none waits after P3.
func priorityPlaced(entry record.LineEntry) string {
	if resolved := entry.Issue.Resolved; resolved.Known {
		switch {
		case resolved.Priority != record.NoPriority && resolved.From != "":
			return "It waits at " + entry.Priority.String() + ", the priority of " + resolved.From + ", the nearest issue above it in Dispatch that has one."
		case resolved.Priority != record.NoPriority:
			return "It waits at " + entry.Priority.String() + ", its own priority."
		case entry.Issue.Parent == nil:
			return "It has no priority, so it waits after every issue at P0 to P3."
		}
		return "Neither it nor any issue above it in Dispatch has a priority, so it waits after every issue at P0 to P3."
	}
	switch {
	case entry.Issue.Priority != record.NoPriority:
		return "It waits at " + entry.Priority.String() + ", its own priority."
	case entry.Priority != record.NoPriority:
		return "It waits at " + entry.Priority.String() + ", the priority of its tree root " + entry.Issue.Tree + "."
	case entry.Issue.Key == entry.Issue.Tree:
		return "It has no priority, so it waits after every issue at P0 to P3."
	}
	return "Neither it nor its tree root " + entry.Issue.Tree + " has a priority, so it waits after every issue at P0 to P3."
}

// holdsItsSlot is the last clause of every member that already holds one of the cap's slots: it
// waits for no slot to free, so the clause that tells the rest of the line what it queues for is
// this truth instead (waitingLineReason). It opens on a space because it follows the full stop
// the place clause ends on.
const holdsItsSlot = " It already holds its admission slot, so it resumes at the next pass whatever the cap says."

// waitsItsTurn is the last clause of a member that gave its slot back to wait on another issue
// (record.LineWaitingOnIssue), holds none, and still names a wait (record.LineEntry.Waits, read
// there rather than off the kind, since the kind outlasts the wait): the promotion passes over
// it while the wait lasts, so telling it that it takes the free slots first would be false, and
// the clause says that it takes one by itself after the wait instead. It opens on a space for
// the reason holdsItsSlot does. The rule is admit.releaseSlotsForWaits.
const waitsItsTurn = " The promotion passes over it while that wait lasts, and after the wait it takes a free slot again by itself."

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
