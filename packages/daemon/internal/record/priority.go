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

// band is where p sorts: P0 to P3 in order, then NoPriority.
func (p Priority) band() uint8 {
	if p == NoPriority {
		return uint8(P3) + 1
	}
	return uint8(p)
}

// PriorityOf uses the issue's own priority, then its top issue's, then NoPriority.
// The caller reads top from the same Dispatch chain as its rank. Priority never
// copies from the top issue onto a member's record.
func PriorityOf(issue, top Issue) Priority {
	if issue.Priority != NoPriority {
		return issue.Priority
	}
	return top.Priority
}
