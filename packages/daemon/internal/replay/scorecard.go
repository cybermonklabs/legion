package replay

import "time"

// Scorecard is the measures of one window, read from the record (Source "record") or from one
// replay of it (Source "replay"), by one code over one timeline, so a gap between the two is a gap
// of the replay and not of two readings. Its JSON names are fixed: a fixture window's expect.json
// holds one per rule under {"rules": {"<rule>": <Scorecard>}}, and `legion replay scorecard
// --json` prints one.
//
// An average is the integral over [From, To) divided by the window's span, Hours is that integral
// in issue-hours, and Max is the largest count at any instant.
type Scorecard struct {
	// Source is "record" or "replay".
	Source string `json:"source"`
	// Rule, Branch, Seed and Slowdown are the replay's (RunConfig); a record's carries none.
	Rule     string    `json:"rule,omitempty"`
	Branch   string    `json:"branch,omitempty"`
	Seed     int64     `json:"seed,omitempty"`
	Slowdown float64   `json:"slowdown,omitempty"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	// FirstDayApart is the same measures over [From, From+24h) alone, the day the window's start
	// state weighs on most (assumption 6); nil for a window of a day or less.
	FirstDayApart *Scorecard `json:"firstDayApart,omitempty"`
	// Unplaced is how many issues the start state could not place.
	Unplaced int `json:"unplaced"`
	// Done is how many issues reach done in [From, To).
	Done int `json:"done"`
	// WorkLeft is how many started issues are not done at To (assumption 15).
	WorkLeft int `json:"workLeft"`
	// WaitOnlyForSlot is reading (a): the returned members of the line that hold no slot and that
	// no wait holds (admission.blocked names none of them).
	WaitOnlyForSlot Occupancy `json:"waitOnlyForSlot"`
	// StartedWithoutSlot is reading (b): every returned member of the line that holds no slot,
	// its waits on other issues included.
	StartedWithoutSlot Occupancy `json:"startedWithoutSlot"`
	// PastReviewHolding and PastReviewNotHolding are the issues past their review that hold a
	// slot and that hold none.
	PastReviewHolding    Occupancy `json:"pastReviewHolding"`
	PastReviewNotHolding Occupancy `json:"pastReviewNotHolding"`
	// PriorityTaken is the time-average share of the pairs of a holder and an issue that waits for
	// a slot in which the holder ranks below the waiting issue (record.RankLess over the tree
	// root's rank, then the issue's own).
	PriorityTaken float64 `json:"priorityTaken"`
	// Loops is the rounds and hours of the send-back loops, by the word of their cause.
	Loops map[string]LoopHours `json:"loops"`
	// Waits is the count and hours of the waits on another issue, by what ended each.
	Waits map[string]WaitHours `json:"waits"`
	// SlotHoursInTurn and SlotHoursNoTurn are the slot-hours held while a worker of the holder is
	// in a turn, and while none is.
	SlotHoursInTurn float64 `json:"slotHoursInTurn"`
	SlotHoursNoTurn float64 `json:"slotHoursNoTurn"`
	// InFlight is the issues the daemon has started and that are not done.
	InFlight Occupancy `json:"inFlight"`
	// SlotsGivenByDay is how many slots were given out on each UTC day, by the day as 2006-01-02.
	SlotsGivenByDay map[string]int `json:"slotsGivenByDay"`
	// MedianSlotWaitMinutes is the median, over the slots given out, of how long the issue that
	// took one waited for it.
	MedianSlotWaitMinutes float64 `json:"medianSlotWaitMinutes"`
	// FirstSlot is each issue's first slot in the window, by issue key.
	FirstSlot map[string]time.Time `json:"firstSlot"`
	// DoneAt is when each issue that reached done in the window did, by issue key.
	DoneAt map[string]time.Time `json:"doneAt"`
	// Assumptions is the assumptions of the spec this run's measures rest on, by number.
	Assumptions []int `json:"assumptions"`
	// Scenario is the never-started scenario the run took (Branch.NeverStarted).
	Scenario string `json:"scenario"`
}

// Occupancy is a count of issues over a window: its average over time, its largest value at any
// instant, and its integral in issue-hours.
type Occupancy struct {
	Average float64 `json:"average"`
	Max     int     `json:"max"`
	Hours   float64 `json:"hours"`
}

// LoopHours is the send-back loops of one cause: how many rounds, and their hours.
type LoopHours struct {
	Rounds int     `json:"rounds"`
	Hours  float64 `json:"hours"`
}

// WaitHours is the waits on another issue that one kind of event ended: how many, and their hours.
type WaitHours struct {
	Count int     `json:"count"`
	Hours float64 `json:"hours"`
}
