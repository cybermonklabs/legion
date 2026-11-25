package blockers

import (
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// KeptWaits is what a slot holder waits on and keeps its slot through, under the slot rule the
// facts carry (record.LineFacts.Rule): the Dispatch issues it is blocked by that are not done
// (Unfinished) and the open children its first phase waits for (record.LineFacts.ChildWaits),
// less the waits the rule gives the slot back for (waitsOf), each named once. It is what the
// holder state `issue` reads (record.HolderFacts.WaitsOnIssue), and what the admission pass marks
// the start of on the slot (admit's markSlotWaits).
//
// It is not record.LineFacts.Waits, which holds the waits that give the slot back and so names a
// started issue the release is about to free, never one that keeps its row. Under the daemon's own
// rule a link wait and a children wait both give the slot back, so a holder keeps its slot through
// a wait only where waitsOf answers nothing for it: a root waiting on a person, a tree's own root
// whose design gate has never opened, and a root that arrived carrying the row of the tree it
// left. A rule whose wait keeps the slot (record.SlotRule.LinkWaitGivesBack false, or
// ChildrenWaitGivesBack false) makes every holder that waits so a holder of this kind, and so does
// a rule whose wait gives the slot back only after a grace (record.SlotRule.WaitGrace) while the
// holder's wait is younger than the grace. Under a rule whose start may take a cover
// (record.TakeCover) the waits are the ones a cover reads (CoverWaits), so a counted waiter's
// mark is the wait that holds its next start, and a link of an issue past its review, which holds
// none, marks nothing.
func KeptWaits(rows map[string]Row, issue record.Issue, facts record.LineFacts) []string {
	given := waitsOf(rows, issue, facts)
	var kept []string
	keep := func(key string) {
		if !slices.Contains(given, key) && !slices.Contains(kept, key) {
			kept = append(kept, key)
		}
	}
	if facts.Rule.OrMain().Take == record.TakeCover {
		for _, key := range CoverWaits(rows, issue, facts) {
			keep(key)
		}
		return kept
	}
	for _, key := range Unfinished(rows, issue.Key) {
		keep(key)
	}
	for _, child := range facts.ChildWaits[issue.Key].Open {
		keep(child.Key)
	}
	return kept
}

// SlotsInputs is everything one project's slots reading is read from, every part of it a read
// or a judgement the caller has already made for its line, so the reading costs no query of its
// own: the admission pass's tick (admit's tickRead) and the state's projection (projection.Project)
// each hand in what they hold.
type SlotsInputs struct {
	// Cap is the project's admission cap.
	Cap int
	// Issues are the records the line was judged from: a holder, a member of the line and a tree
	// root are each read from here, and a root they leave out from Facts.Roots. A holder's tree
	// root gives both its rank and the priority that places the holder (SlotHolder.Priority).
	Issues []record.Issue
	// Slots are the slots of the line's judgement; a slot whose issue Issues does not hold is no
	// holder of this project's.
	Slots []record.Slot
	// Rows, Facts and Line are what JudgeLine answered: the mirror rows, the facts with the held
	// set, the child waits and the started issues' waits filled, and the line.
	Rows  map[string]Row
	Facts record.LineFacts
	Line  []record.LineEntry
	// Blocked and Held are the state's `admission.blocked` and `admission.held` over that same
	// judgement (Blocked, record.HeldList).
	Blocked []record.BlockedIssue
	Held    []record.HeldIssue
	// Claims are the claims of the moment as a holder's state reads them (record.HolderClaim),
	// and Stranded the claims among them nothing will wake (workflow.Stranded).
	Claims   []record.HolderClaim
	Stranded []supervise.Claim
}

// SlotsReading is the project's slots reading (record.SlotsReadingOf) over what in holds, and
// each holder by its key, with the state it is in and the time it entered it: the reading the
// state serves as `admission.slots` and the tick notice carries as `slots`, and the state and time
// each holder's `slot` carries. Each holder is read by SlotHolderState, so the two callers read
// one holder alike, and is placed against the first issue that waits for a slot in the line's own
// order by the priority that would place it in the line under the rule of in.Facts
// (record.PlacingPriority, as Waiting places an entry) and by its tree root's rank, both read off
// the one record of its tree root: the records' own, else the one Facts.Roots carries.
func SlotsReading(in SlotsInputs) (record.SlotsReading, map[string]record.SlotHolder) {
	byKey := make(map[string]record.Issue, len(in.Issues))
	for _, issue := range in.Issues {
		byKey[issue.Key] = issue
	}
	rootOf := func(issue record.Issue) record.Issue {
		if root, recorded := byKey[issue.Tree]; recorded {
			return root
		}
		return in.Facts.Roots[issue.Tree]
	}
	stranded := make(map[claim.Token]bool, len(in.Stranded))
	for _, stopped := range in.Stranded {
		stranded[stopped.Token] = true
	}
	// Each holder's claims are its issue's, gathered once here rather than walked for every
	// holder; the order within an issue is the order the claims were read in.
	claimsOf := make(map[string][]record.HolderClaim)
	for _, c := range in.Claims {
		claimsOf[c.Issue] = append(claimsOf[c.Issue], c)
	}

	holders := make([]record.SlotHolder, 0, len(in.Slots))
	byHolder := make(map[string]record.SlotHolder, len(in.Slots))
	banded := in.Facts.Rule.OrMain().PriorityFirst
	for _, slot := range in.Slots {
		issue, own := byKey[slot.Issue]
		if !own {
			continue
		}
		state, since := SlotHolderState(issue, slot, claimsOf[issue.Key], stranded, in.Rows, in.Facts)
		root := rootOf(issue)
		holder := record.SlotHolder{Issue: issue, Priority: record.PlacingPriority(issue, root, banded),
			RootRank: root.Rank, State: state, Since: since}
		holders = append(holders, holder)
		byHolder[issue.Key] = holder
	}

	blocked := make(map[string]bool, len(in.Blocked))
	for _, row := range in.Blocked {
		blocked[row.Issue] = true
	}
	held := make(map[string]bool, len(in.Held))
	for _, row := range in.Held {
		held[row.Issue] = true
	}
	members := make([]record.SlotsLineMember, len(in.Line))
	for i, entry := range in.Line {
		members[i] = record.SlotsLineMember{Entry: entry, RootRank: rootOf(entry.Issue).Rank,
			HeldBack: len(LineWaitsOn(in.Rows, entry)) > 0, Blocked: blocked[entry.Issue.Key], Held: held[entry.Issue.Key]}
	}
	return record.SlotsReadingOf(in.Cap, holders, members), byHolder
}

// SlotHolderState is the state issue, holding slot, stands in as the slots reading reads a holder,
// and the time it entered it (record.HolderStateOf), from the facts gathered here: its claims
// (claims, its issue's own, record.HolderClaimFacts, with the tokens nothing will wake in
// stranded), the start it is owed (record.LineFacts.Owed), its hold, its root's wait on a person
// that nobody has answered, and the waits it keeps its slot through (KeptWaits, over rows and
// facts). The slots reading reads each holder by it, and so does a replay's snapshot of the state.
func SlotHolderState(issue record.Issue, slot record.Slot, claims []record.HolderClaim, stranded map[claim.Token]bool,
	rows map[string]Row, facts record.LineFacts,
) (record.HolderState, *time.Time) {
	holder := record.HolderFacts{Issue: issue, AdmittedAt: slot.AdmittedAt, WaitSince: slot.WaitsSince}
	record.HolderClaimFacts(&holder, claims, stranded)
	if owed, owes := facts.Owed[issue.Key]; owes {
		holder.Starting = append(holder.Starting, owed.CreatedAt)
	}
	if issue.Hold != nil {
		holder.HeldSince = issue.Hold.Since
	}
	if issue.AwaitingPerson() && !issue.Answered() && claim.IsTreeRoot(issue.Key, issue.Tree) {
		holder.Person = true
		holder.PersonSince = record.KnownTime(issue.Awaiting.Since.At)
	}
	holder.WaitsOnIssue = len(KeptWaits(rows, issue, facts)) > 0
	return record.HolderStateOf(holder)
}
