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

// PriorityOf is the priority issue takes: its
// own, else its tree root's, else NoPriority. root is the record of issue's tree root, the issue
// itself for a root, and the zero Issue where the root has no record. The root's priority is read
// from the root's record each time, never copied onto its members, so a person's change of a
// root's priority reaches every member that has none of its own at the daemon's next read of the
// root, with no event of theirs.
func PriorityOf(issue, root Issue) Priority {
	if issue.Priority != NoPriority {
		return issue.Priority
	}
	return root.Priority
}
