package record

import "encoding/json"

// Priority is an issue's own Dispatch priority as the record keeps it (Issue.Priority): P0, the
// most urgent, to P3, or NoPriority for an issue Dispatch shows none. NoPriority is the zero
// value, so a record stored before the daemon read priorities, and every record built without
// one, reads as an issue with none.
//
// Dispatch serves the priority as an integer from 0 for P0 to 3 for P3, or null for none, and the
// store keeps that integer in the issues table's priority column, so the column reads as Dispatch's
// own issue does. DispatchPriority reads that integer and Level writes it: the offset between it
// and Priority lives in these two functions alone.
type Priority uint8

// The four priorities Dispatch has, and none.
const (
	NoPriority Priority = iota
	P0
	P1
	P2
	P3
)

// DispatchPriority is level, the priority Dispatch serves on an issue — 0 for P0 to 3 for P3, nil
// for none — as the record keeps it. Dispatch refuses any other level (its INVALID_PRIORITY), so a
// level outside 0 to 3 is none here, and the second answer is false to say it could not be read.
func DispatchPriority(level *int) (Priority, bool) {
	if level == nil {
		return NoPriority, true
	}
	if *level < 0 || *level > 3 {
		return NoPriority, false
	}
	return P0 + Priority(*level), true
}

// Level is p as Dispatch serves it, 0 for P0 to 3 for P3, and nil for none: the inverse of
// DispatchPriority. The store writes it to the issues table's priority column, and the API to the
// wire.
func (p Priority) Level() *int {
	if p < P0 || p > P3 {
		return nil
	}
	level := int(p - P0)
	return &level
}

// DecodePriority reads raw, a priority as Dispatch writes it in JSON (an integer from 0 to 3, or
// null), through DispatchPriority. An error says raw is no integer or null at all; false says it
// is a level Dispatch never writes, which reads as none.
func DecodePriority(raw []byte) (Priority, bool, error) {
	var level *int
	if err := json.Unmarshal(raw, &level); err != nil {
		return NoPriority, false, err
	}
	priority, read := DispatchPriority(level)
	return priority, read, nil
}

// String is the priority as Dispatch names it, P0 to P3, and the empty string for none.
func (p Priority) String() string {
	switch p {
	case P0:
		return "P0"
	case P1:
		return "P1"
	case P2:
		return "P2"
	case P3:
		return "P3"
	}
	return ""
}

// band is where p's band stands in the waiting line, lower first: P0 to P3 in their order, and
// NoPriority, an entry neither it nor its tree root has a priority for, after P3 (Waiting).
func (p Priority) band() uint8 {
	if p == NoPriority {
		return uint8(P3) + 1
	}
	return uint8(p)
}

// Resolved is the priority Dispatch resolves an issue to (`resolved_priority`, J1 of
// dispatch://INFRA-5793): the issue's own where it has one, else that of the nearest issue above it
// on its Dispatch parent chain that has one, in any project. It is the priority admission reads (C1
// and C2 of dispatch://INFRA-5636). Known says Dispatch sent `resolved_priority` at all: a Dispatch
// release before INFRA-5793 sends none, and the zero Resolved, a record stored before the daemon read
// it, is that too. Priority is what Dispatch sent, NoPriority for a present null, which is no
// priority and replaces any resolved priority before it. From is `priority_from`, the key of the
// issue the priority comes from, empty where the priority is the issue's own or none.
//
// Every path that carries the issue's own priority carries this beside it: an issue event, the
// boot listing, a detail read and the issue an accepted status write answers, and every fact the
// daemon builds from its own record, which keeps both as they stand.
type Resolved struct {
	Priority Priority
	From     string
	Known    bool
}

// AdmissionPriority is the priority admission reads for issue: the one Dispatch resolves it to
// where Dispatch sent one (Resolved.Known), else its own. So with a Dispatch release that resolves
// no priority only an issue's own P0 makes it P0, and no Legion tree fallback ever does: the
// daemon never walks a parent chain of its own to find one.
func (i Issue) AdmissionPriority() Priority {
	if i.Resolved.Known {
		return i.Resolved.Priority
	}
	return i.Priority
}

// PriorityOf is the priority that places issue in the waiting line: the one Dispatch resolves it
// to where Dispatch sent one (Issue.AdmissionPriority), else, for a Dispatch release that resolves
// none, its own, else its tree root's, else NoPriority. root is the record of issue's tree root,
// the issue itself for a root, and the zero Issue where the root has no record. The root's
// priority is read from the root's record each time, never copied onto its members, so a person's
// change of a root's priority reaches every member that has none of its own at the daemon's next
// read of the root, with no event of theirs. The tree root's stands for an older Dispatch's order
// of the line alone: it never makes an issue P0 for a start (Issue.StartsNow).
func PriorityOf(issue, root Issue) Priority {
	if issue.Resolved.Known || issue.Priority != NoPriority {
		return issue.AdmissionPriority()
	}
	return root.Priority
}

// StartsNow says whether issue starts outside the admission cap (R10 of dispatch://INFRA-3199, C2
// of dispatch://INFRA-5636): whether admission reads it as P0 (Issue.AdmissionPriority), directly
// or by Dispatch's inheritance. It never reads PriorityOf's tree root: a P0 Legion tree root makes
// none of its children P0.
func (i Issue) StartsNow() bool { return i.AdmissionPriority() == P0 }

// SlotHolders is a project's holders of an admission slot as the cap and the P0 bound count them:
// P0, the holders whose own priority is P0 (Issue.StartsNow), which the cap does not count and the
// P0 bound does, and Capped, every other holder, which the cap counts. A holder is counted by the
// priority its record carries at the read, so one raised to P0 leaves the cap's count at the next
// read and one lowered below P0 counts against the cap from then on, while its slot stays.
type SlotHolders struct {
	Capped int
	P0     int
}

// HoldersOf counts the slots of slots whose issue is one of issues, by whether that issue starts
// outside the cap. issues is one project's records and slots may be every project's: a slot of an
// issue not among issues is counted nowhere. Every reader of the cap counts through it — the
// promotion, its free-slot reading, the stall measure and the state — so all of them count the same
// holders of one record.
func HoldersOf(issues []Issue, slots []Slot) SlotHolders {
	byKey := make(map[string]Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	var held SlotHolders
	for _, slot := range slots {
		if issue, known := byKey[slot.Issue]; known {
			held.Add(issue)
		}
	}
	return held
}

// Add counts issue as one more holder.
func (h *SlotHolders) Add(issue Issue) {
	if issue.StartsNow() {
		h.P0++
		return
	}
	h.Capped++
}

// Total is every holder, P0 and capped together: the slots in use.
func (h SlotHolders) Total() int { return h.Capped + h.P0 }
