package signals

import "time"

// The pure half of admission by signals: what a pass reads of the signals (Snapshot), the kind of
// pass that reads it (PassKind), the mode it admits in (Mode), and what the rule decides for one
// entry below P0 that takes a slot of the cap (Decide). The daemon's admission pass and a replay's
// run the same function; each supplies its own snapshot.

// Mode is how a pass admits an entry below P0 that takes a slot of the cap.
type Mode string

const (
	// Off is the cap as it stands today: a start takes a slot while the holders below P0 are
	// fewer than the cap, at every kind of pass. It is the zero value.
	Off Mode = ""
	// Ceiling is admission by signals under the cap as a ceiling: a start takes a slot while no
	// signal that has a limit binds and the holders below P0 are fewer than the ceiling.
	Ceiling Mode = "ceiling"
	// Backoff is Ceiling with an allowance (Allowance) in place of the ceiling.
	Backoff Mode = "backoff"
)

// Or is m where it names a mode, and other where it is Off.
func (m Mode) Or(other Mode) Mode {
	if m != Off {
		return m
	}
	return other
}

// PassKind is the kind of the fact whose admission pass is judged. A pass admits below P0 by
// signals only at an event or at a signal's deadline, never at the daemon's tick.
type PassKind string

const (
	// PassEvent is the pass of every fact but the tick and the deadline.
	PassEvent PassKind = "event"
	// PassTick is the pass of the daemon's periodic tick.
	PassTick PassKind = "tick"
	// PassDeadline is the pass of a signal's deadline: the moment a signal would cross its limit
	// with no event.
	PassDeadline PassKind = "deadline"
)

// Reading is one signal as a pass reads it: its value at the pass and its limit, nil where none is
// set. A signal with no limit holds no start, and neither does one that is not recorded.
type Reading struct {
	Value
	Limit *float64
}

// Holds says whether the signal holds a start: a limit is set, the signal is recorded and at or
// above the limit (Value.Binds).
func (r Reading) Holds() bool { return r.Limit != nil && r.Value.Binds(*r.Limit) }

// Setback is each refusal by the project's own limit and each kill the history holds at a moment:
// the events the back-off folds, Count of them at At.
type Setback struct {
	At    time.Time
	Count int64
}

// Snapshot is what a pass reads of the signals, supplied by its caller once for the pass: the mode
// it admits in where its rule names none, the moment of the pass, each signal's reading by name,
// and for the back-off the history of setbacks up to the pass, in time order. A signal with no
// reading holds no start.
type Snapshot struct {
	Mode     Mode
	At       time.Time
	Readings map[string]Reading
	Setbacks []Setback
}

// Limit is the limit set on the signal name, and whether one is set.
func (s Snapshot) Limit(name string) (float64, bool) {
	limit := s.Readings[name].Limit
	if limit == nil {
		return 0, false
	}
	return *limit, true
}

// Holding is the name of each signal that holds a start, in R11's order.
func (s Snapshot) Holding() []string {
	var holding []string
	for _, name := range order {
		if s.Readings[name].Holds() {
			holding = append(holding, name)
		}
	}
	return holding
}

// Verdict is what a rule decides for one entry below P0 that takes a slot of the cap.
type Verdict string

const (
	// Starts lets the entry take a slot.
	Starts Verdict = "starts"
	// HeldByTick holds it because the pass is a tick, which starts nothing below P0 by signals.
	HeldByTick Verdict = "tick"
	// HeldByCap holds it because the holders below P0 fill the ceiling or the allowance.
	HeldByCap Verdict = "cap"
	// HeldBySignal holds it because a signal with a limit binds.
	HeldBySignal Verdict = "signal"
)

// Lets says whether the verdict lets the entry take a slot.
func (v Verdict) Lets() bool { return v == Starts }

// Decide is what the rule decides, in mode, for an entry below P0 that takes a slot of the cap at a
// pass of kind, given the snapshot s the pass read, the holders below P0 that the cap counts
// (capped) and the cap as a ceiling. Under Off it is the cap as it stands today. Otherwise a tick
// lets nothing start; a pass at an event or a deadline holds the entry where the holders fill the
// ceiling, or under Backoff the allowance, and where a signal with a limit binds. It is a pure
// function of its arguments, and it takes no slot from a holder.
func Decide(mode Mode, kind PassKind, s Snapshot, capped, ceiling int) Verdict {
	if mode == Off {
		if capped < ceiling {
			return Starts
		}
		return HeldByCap
	}
	if kind == PassTick {
		return HeldByTick
	}
	limit := ceiling
	if mode == Backoff {
		limit = Allowance(ceiling, s.Setbacks, s.At)
	}
	switch {
	case capped >= limit:
		return HeldByCap
	case len(s.Holding()) > 0:
		return HeldBySignal
	default:
		return Starts
	}
}

// Allowance is the back-off's allowance at at: it starts at ceiling, is multiplied by 90% in
// integers, rounded down and at least 1, at each setback of setbacks up to at, and rises by 1 for
// each whole hour since the last setback with none between, up to ceiling. setbacks are in time
// order. A caller that changes its ceiling gets the history folded under the ceiling it gives.
func Allowance(ceiling int, setbacks []Setback, at time.Time) int {
	allowance := ceiling
	var last time.Time
	for _, setback := range setbacks {
		if setback.At.After(at) {
			break
		}
		allowance = risen(allowance, ceiling, last, setback.At)
		for i := int64(0); i < setback.Count && allowance > 1; i++ {
			allowance = max(allowance*9/10, 1)
		}
		last = setback.At
	}
	return risen(allowance, ceiling, last, at)
}

// risen is allowance after the quiet hours from from to to: one more for each whole hour, up to
// ceiling.
func risen(allowance, ceiling int, from, to time.Time) int {
	return min(ceiling, allowance+int(to.Sub(from)/time.Hour))
}

// NextRise is the next moment after at at which the allowance rises, the setbacks standing as they
// do at at, and whether one is due: none where the allowance is at the ceiling.
func NextRise(ceiling int, setbacks []Setback, at time.Time) (time.Time, bool) {
	if Allowance(ceiling, setbacks, at) >= ceiling {
		return time.Time{}, false
	}
	var last time.Time
	for _, setback := range setbacks {
		if setback.At.After(at) {
			break
		}
		last = setback.At
	}
	quiet := int64(at.Sub(last) / time.Hour)
	return last.Add(time.Duration(quiet+1) * time.Hour), true
}

// Crossing is the moment a signal, or the allowance, would next cross with no event.
type Crossing struct {
	Signal string
	At     time.Time
}

// Earliest is the earliest of crossings, the first of them among crossings at one moment, and
// whether there is one: the project's deadline.
func Earliest(crossings []Crossing) (Crossing, bool) {
	var first Crossing
	found := false
	for _, crossing := range crossings {
		if !found || crossing.At.Before(first.At) {
			first, found = crossing, true
		}
	}
	return first, found
}
