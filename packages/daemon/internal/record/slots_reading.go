package record

import "time"

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
// line's own order (Waiting). A holder ranks below it where it comes after it in RankLess order
// over its tree root's record and then its own.
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
// (Waiting's LineEntry, with its part, kind, slot, place and ranks), whether a wait holds it
// back (blockers.LineWaitsOn gives it something), and whether it stands in admission.blocked or
// in admission.held.
type SlotsLineMember struct {
	Entry    LineEntry
	HeldBack bool
	Blocked  bool
	Held     bool
}

// SlotHolder is one holder of an admission slot as the slots reading needs it: its record, the
// Dispatch rank of its tree root, which with its own rank places it against the first issue that
// waits for a slot, and its state with the time it entered it (HolderStateOf).
type SlotHolder struct {
	Issue    Issue
	RootRank string
	State    HolderState
	Since    *time.Time
}

// SlotsReadingOf is the slots reading of a project whose admission cap is cap, whose slot holders
// are holders and whose waiting line is line, in the line's own order. Every reader of the
// reading builds it here, the state projection and the admission pass's tick alike, so the text,
// the JSON and the tick notice give the same numbers.
func SlotsReadingOf(cap int, holders []SlotHolder, line []SlotsLineMember) SlotsReading {
	return SlotsReading{}
}

// SlotsLine is the slots reading as one sentence, the line `legion state` prints after its
// admission count line and the reading carries as Line:
//
//	slots: <inUse> of <cap> in use: <n> working (oldest <t>), <n> stopped (oldest <t>), <n> person, <n> issue, <n> idle (oldest <t>). Started work without a slot: <n> wait only for a slot (oldest <t>), <n> in all (oldest <t>). <n> holders rank below <KEY>, which waits for a slot.
//
// A count with no known time prints no oldest, a time prints as 2006-01-02T15:04Z, and where no
// issue waits for a slot the last sentence reads "No issue waits for a slot."
func SlotsLine(r SlotsReading) string {
	return ""
}
