package record

import (
	"fmt"
	"strings"
	"time"
)

// StateCount is how many slot holders, or members of the waiting line, one count of the slots
// reading names, and the oldest time among them: the time each entered the state the count is of.
type StateCount struct {
	Count int `json:"count"`
	// Oldest is the oldest of the times known among the counted, nil where the record holds a
	// time for none of them (HolderStateOf's time, a returned member's place in the line).
	Oldest *time.Time `json:"oldest,omitempty"`
}

// BelowFirstWaiting is how many slot holders rank below the first issue that waits for a slot,
// and which issue that is. An issue that waits for a slot is a member of the waiting line that
// holds no slot, that no wait holds back (blockers.LineWaitsOn gives it nothing), and that stands
// in neither admission.blocked nor admission.held; the first is the first such member in the
// line's own order (Waiting). A holder ranks below it where it comes after it in that same order,
// which puts priority first: under a rule that bands the line (SlotRule.PriorityFirst, the
// daemon's own) by the band of the priority that places each (PlacingPriority, P0 first and an
// issue with none after P3), and within one band, or under a rule that does not band the line, in
// RankLess order over its tree root's record and then its own.
type BelowFirstWaiting struct {
	Count int `json:"count"`
	// First is the key of the first issue that waits for a slot, "" where none waits.
	First string `json:"first,omitempty"`
}

// SlotsReading is what the slots in use are spent on, as `legion state`, its JSON body
// (api.Admission.Slots) and the controller's tick notice (Notice.Slots) all give it: the slots in
// use and the cap, the holders counted by state (HolderState), the two readings of started work
// that waits without a slot, and the holders that rank below the first issue that waits for a
// slot. Line is the same reading as one sentence (SlotsLine), so the three surfaces print one
// text.
type SlotsReading struct {
	InUse   int        `json:"inUse"`
	Cap     int        `json:"cap"`
	Working StateCount `json:"working"`
	Stopped StateCount `json:"stopped"`
	Person  StateCount `json:"person"`
	Issue   StateCount `json:"issue"`
	Idle    StateCount `json:"idle"`
	// WaitOnlyForSlot is reading (a): the members of the line's returned part that hold no slot
	// and stand in no row of admission.blocked, a member released for a wait whose wait has ended
	// among them although its kind stays waiting_on_issue until its promotion.
	WaitOnlyForSlot StateCount `json:"waitOnlyForSlot"`
	// StartedWithoutSlot is reading (b): every member of the line's returned part that holds no
	// slot.
	StartedWithoutSlot StateCount        `json:"startedWithoutSlot"`
	BelowFirstWaiting  BelowFirstWaiting `json:"belowFirstWaiting"`
	// Line is the reading as the one sentence SlotsLine writes, and nothing else writes.
	Line string `json:"line"`
}

// SlotsLineMember is one member of the waiting line as the slots reading needs it: its entry
// (Waiting's LineEntry, with its part, kind, slot, place, ranks and the priority that placed it),
// the Dispatch rank of its tree root, whether a wait holds it back (blockers.LineWaitsOn gives it
// something), and whether it stands in admission.blocked or in admission.held. The root's rank is
// carried beside the entry because the entry carries it for a member of the starting part alone
// (LineEntry.RootRank), and the first issue that waits for a slot can be a member of either part.
type SlotsLineMember struct {
	Entry    LineEntry
	RootRank string
	HeldBack bool
	Blocked  bool
	Held     bool
}

// SlotHolder is one holder of an admission slot as the slots reading needs it: its record, the
// priority that places it and the Dispatch rank of its tree root, which with its own rank place
// it against the first issue that waits for a slot in the line's own order, and its state with
// the time it entered it (HolderStateOf).
type SlotHolder struct {
	Issue Issue
	// Priority is the priority that would place the holder in the waiting line, as
	// LineEntry.Priority places an entry (PlacingPriority, over the record of its tree root that
	// RootRank is read from): NoPriority under a rule that does not band the line.
	Priority Priority
	RootRank string
	State    HolderState
	Since    *time.Time
}

// SlotsReadingOf is the slots reading of a project whose admission cap is cap, whose slot holders
// are holders and whose waiting line is line, in the line's own order. Every reader of the
// reading builds it here, the state projection and the admission pass's tick alike, so the text,
// the JSON and the tick notice give the same numbers.
//
// Each count's oldest time is the oldest of its members' known times: a holder's is the time it
// entered its state (SlotHolder.Since), and a member of the line's is the moment its work came
// back (LineEntry.Since). A member released for a wait whose wait has ended keeps the kind
// waiting_on_issue until its promotion, so reading (a) reads the part, the slot and
// admission.blocked and never the kind.
//
// A holder is counted below the first issue that waits for a slot where it comes after it in the
// line's own order (rankedIssue.before): first by the band of the priority that places each, the
// entry's LineEntry.Priority and the holder's SlotHolder.Priority, which are NoPriority on both
// sides under a rule that does not band the line; then by the Dispatch rank of its tree root and
// its own.
func SlotsReadingOf(cap int, holders []SlotHolder, line []SlotsLineMember) SlotsReading {
	reading := SlotsReading{InUse: len(holders), Cap: cap}
	counts := map[HolderState]*StateCount{
		HolderWorking: &reading.Working, HolderStopped: &reading.Stopped, HolderPerson: &reading.Person,
		HolderIssue: &reading.Issue, HolderIdle: &reading.Idle,
	}
	for _, holder := range holders {
		if count, known := counts[holder.State]; known {
			count.add(holder.Since)
		}
	}
	var first *SlotsLineMember
	for i, member := range line {
		if member.Entry.Holds {
			continue
		}
		if member.Entry.Part == PartReturned {
			since := KnownTime(member.Entry.Since)
			reading.StartedWithoutSlot.add(since)
			if !member.Blocked {
				reading.WaitOnlyForSlot.add(since)
			}
		}
		if first == nil && !member.HeldBack && !member.Blocked && !member.Held {
			first = &line[i]
		}
	}
	if first != nil {
		reading.BelowFirstWaiting.First = first.Entry.Issue.Key
		waiting := rankedIssue{issue: first.Entry.Issue, priority: first.Entry.Priority, rootRank: first.RootRank}
		for _, holder := range holders {
			if waiting.before(rankedIssue{issue: holder.Issue, priority: holder.Priority, rootRank: holder.RootRank}) {
				reading.BelowFirstWaiting.Count++
			}
		}
	}
	reading.Line = SlotsLine(reading)
	return reading
}

// add counts one more member whose time is at, nil where the record holds none, keeping the
// oldest known time.
func (c *StateCount) add(at *time.Time) {
	c.Count++
	if at != nil && (c.Oldest == nil || at.Before(*c.Oldest)) {
		utc := at.UTC()
		c.Oldest = &utc
	}
}

// rankedIssue is an issue with the priority that places it in the waiting line and the Dispatch
// rank of its tree root: what the line orders it by (Waiting's band of the priority, and within
// one band its starting part's byRank), and so how a holder is placed against the first issue
// that waits for a slot.
type rankedIssue struct {
	issue    Issue
	priority Priority
	rootRank string
}

// before says whether r comes before other in the line's own order: by the band of the priority
// that places each (Priority.band, P0 first and none after P3), which is one band where the rule
// does not band the line, since each then carries NoPriority; and within one band in the board's
// order, by their tree roots (RankLess over each root's rank, and its key, which is the tree's)
// and within one tree by their own records, as the line's starting part is ordered.
func (r rankedIssue) before(other rankedIssue) bool {
	if band, otherBand := r.priority.band(), other.priority.band(); band != otherBand {
		return band < otherBand
	}
	if r.issue.Tree != other.issue.Tree {
		return RankLess(Issue{Key: r.issue.Tree, Rank: r.rootRank}, Issue{Key: other.issue.Tree, Rank: other.rootRank})
	}
	return RankLess(r.issue, other.issue)
}

// slotsLineMinute is how the slots line prints a time: UTC, to the minute.
const slotsLineMinute = "2006-01-02T15:04Z"

// SlotsLine is the slots reading as one sentence, the line `legion state` prints after its
// admission count line and the reading carries as Line:
//
//	slots: <inUse> of <cap> in use: <n> working (oldest <t>), <n> stopped (oldest <t>), <n> person (oldest <t>), <n> issue (oldest <t>), <n> idle (oldest <t>). Started work without a slot: <n> wait only for a slot (oldest <t>), <n> in all (oldest <t>). <n> holders rank below <KEY>, which waits for a slot.
//
// Each of the five states prints its number and the oldest time in it. A count with no known time
// prints no oldest, a time prints as 2006-01-02T15:04Z, one holder reads "1 holder ranks below",
// and where no issue waits for a slot the last sentence reads "No issue waits for a slot."
func SlotsLine(r SlotsReading) string {
	var line strings.Builder
	fmt.Fprintf(&line, "slots: %d of %d in use: %s, %s, %s, %s, %s. ", r.InUse, r.Cap,
		r.Working.said("working"), r.Stopped.said("stopped"), r.Person.said("person"), r.Issue.said("issue"),
		r.Idle.said("idle"))
	fmt.Fprintf(&line, "Started work without a slot: %s, %s. ", r.WaitOnlyForSlot.said("wait only for a slot"),
		r.StartedWithoutSlot.said("in all"))
	switch below := r.BelowFirstWaiting; {
	case below.First == "":
		line.WriteString("No issue waits for a slot.")
	case below.Count == 1:
		fmt.Fprintf(&line, "1 holder ranks below %s, which waits for a slot.", below.First)
	default:
		fmt.Fprintf(&line, "%d holders rank below %s, which waits for a slot.", below.Count, below.First)
	}
	return line.String()
}

// said is one count as the slots line prints it: the number, what it counts, and the oldest time
// where the count has members and a time is known for one of them.
func (c StateCount) said(what string) string {
	if c.Count == 0 || c.Oldest == nil {
		return fmt.Sprintf("%d %s", c.Count, what)
	}
	return fmt.Sprintf("%d %s (oldest %s)", c.Count, what, c.Oldest.UTC().Format(slotsLineMinute))
}
