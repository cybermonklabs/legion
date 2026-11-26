package replay

import (
	"maps"
	"slices"
	"time"
)

// waitGaveBack is one wait on another issue that gave a slot back in a window's record, as the
// window's record.json holds it (windowRecord.WaitsGaveBack): the issue, the moment the wait began
// and the moment it ended, or the window's end where it stands there (Open), so End less Start is
// its length, an open wait's to the window's end.
type waitGaveBack struct {
	Issue string    `json:"issue"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Open  bool      `json:"open"`
}

// waitsGaveBack is each wait on another issue that gave a slot back in the record t is the trace
// of, by issue and then by the moment of the slot's release: for each slot the trace holds taken
// back for a wait (SlotSpan.ForWait) at a moment of the window's span, the issue's wait that stands
// there (waitAt). A release whose issue the trace holds no such wait of has no known length and is
// left out. It reads the record alone, whatever rule the window's daemon ran, so it is one list for
// every rule a comparison runs, and each rule's grace is read against it (graceKeeps). It is never
// nil, so a record.json written before it held the list is told apart (windowRecordFor).
func waitsGaveBack(t Trace) []waitGaveBack {
	waits := []waitGaveBack{}
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		iss := t.Issues[key]
		for _, span := range iss.Slots {
			if !span.ForWait || span.End.Before(t.From) || !span.End.Before(t.To) {
				continue
			}
			if wait, ok := waitAt(iss, span.End, t.To); ok {
				waits = append(waits, wait)
			}
		}
	}
	return waits
}

// waitAt is the wait on another issue of iss that stands at at, the moment a pass took its slot
// back for it, and whether one stands there: from the start of the earliest of its waits on a link
// or on its children (WaitLink, WaitChildren) begun by at and not ended by then, to the end of the
// last such wait that runs on from it unbroken, each begun by the end of the one before, since the
// issue waits on another issue all that time; to, the window's end, where the wait stands there.
func waitAt(iss *IssueTrace, at, to time.Time) (waitGaveBack, bool) {
	wait := waitGaveBack{Issue: iss.Key}
	found := false
	for _, w := range iss.Waits { // by the moment each began
		if w.Kind != WaitLink && w.Kind != WaitChildren {
			continue
		}
		if !found {
			if w.Start.After(at) {
				return waitGaveBack{}, false
			}
			if w.End.IsZero() || w.End.After(at) {
				wait.Start, wait.End, found = w.Start, w.End, true
			}
			continue
		}
		if wait.End.IsZero() || w.Start.After(wait.End) {
			break
		}
		if w.End.IsZero() || w.End.After(wait.End) {
			wait.End = w.End
		}
	}
	if !found {
		return waitGaveBack{}, false
	}
	if wait.End.IsZero() || !wait.End.Before(to) {
		wait.End, wait.Open = to, true
	}
	return wait, true
}

// graceKept is what a rule's grace keeps over a window's waits that gave a slot back
// (graceKeeps): how many of the waits it keeps the slot through, of how many, and the slot-hours
// it keeps.
type graceKept struct {
	Kept, Waits int
	SlotHours   float64
}

// graceKeeps is what grace keeps over waits, the waits on another issue that gave a slot back in a
// window's record (waitsGaveBack), under a rule whose wait gives the slot back only once it has
// lasted grace (record.SlotRule.WaitGrace). It keeps the slot through each wait that lasts no
// longer than the grace: one that ended by the grace's end, which the pass at that moment finds
// over, and one still open at the window's end whose length to the end is no longer, its slot kept
// to the end. Its slot-hours are the sum over every wait of the shorter of the wait's length and
// the grace: the slot each wait holds under the grace, a kept wait's for its whole length and a
// longer one's for the grace, which is the slot the line goes without. The lengths of the kept
// waits alone would leave out the grace held on every longer wait, where most of its cost falls.
// The slot of a longer wait goes back at the first pass at or after the grace's end
// (record.SlotRule.GraceOver), a moment later than the grace, which the slot-hours leave out.
func graceKeeps(grace time.Duration, waits []waitGaveBack) graceKept {
	kept := graceKept{Waits: len(waits)}
	var held time.Duration
	for _, wait := range waits {
		length := wait.End.Sub(wait.Start)
		if length <= grace {
			kept.Kept++
		}
		held += min(length, grace)
	}
	kept.SlotHours = held.Hours()
	return kept
}
