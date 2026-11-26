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
	// its waits on other issues included, and every started parent in awaiting_children that
	// holds none (IssueState.AwaitingChildren), which stands in no line.
	StartedWithoutSlot Occupancy `json:"startedWithoutSlot"`
	// PastReviewHolding and PastReviewNotHolding are the issues past their review that hold a
	// slot and that hold none.
	PastReviewHolding    Occupancy `json:"pastReviewHolding"`
	PastReviewNotHolding Occupancy `json:"pastReviewNotHolding"`
	// PriorityTaken and PriorityTakenBanded are the time-average share of the pairs of a holder and
	// an issue that waits for a slot in which the holder ranks below the waiting issue, each read in
	// one order for every rule: PriorityTaken by rank alone (record.RankLess over the tree root's
	// rank, then the issue's own), the order of the lines the recorded windows ran, which R5 and
	// the forecast read; PriorityTakenBanded in today's line's order (record.Waiting under the
	// daemon's own rule, SlotRule.PriorityFirst), the band of the priority that places each issue
	// first (record.PriorityOf) and then the same, which the comparison judges the rules on.
	PriorityTaken       float64 `json:"priorityTaken"`
	PriorityTakenBanded float64 `json:"priorityTakenBanded"`
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
	// UrgentWaits is the wait of urgent work, P0's and P1's apart, each where an issue counts in it
	// (UrgentWait, urgentWaits): it runs on past To to the end of the run, the window's run-out, and
	// the first day apart holds none. A record read before it held them has it nil.
	UrgentWaits map[string]UrgentWait `json:"urgentWaits"`
	// DoneByAnyone is how many issues of the window's project Dispatch shows reaching done in
	// [From, To), by any actor: Legion's own done (Done) and a close made outside Legion alike.
	// GateClosedHours is, for each tree, by its root's key, the hours of [From, To) its design gate
	// stood closed, from its first gate fact on. Both are read from a trace alone (traceMeasures),
	// the record's (Score) and a replay's own log and events (RunResult.LineScorecard), by one code;
	// a snapshot scorecard holds neither, nor does the first day apart. A record read before it held
	// the gates has GateClosedHours nil, and one read since an empty map where no tree has a gate.
	DoneByAnyone    int                `json:"doneByAnyone"`
	GateClosedHours map[string]float64 `json:"gateClosedHours"`
	// Filed is each issue of the window's project that Dispatch shows filed in [From, To), by key,
	// at its issue.created event, and SlotsGiven each moment in [From, To) a pass gave an issue of
	// the project a slot, in order, the moments SlotsGivenByDay counts. Both are read from a trace
	// alone (traceMeasures), as DoneByAnyone is: R5's branch of the recorded conflicts names each
	// issue the record filed that a replay leaves out (Filed), and the cap backtest counts the
	// slots given out in each span of one cap (SlotsGiven).
	Filed      map[string]time.Time `json:"filed"`
	SlotsGiven []time.Time          `json:"slotsGiven"`
	// DailyMaxima and UrgentMeans are what the forecast across a change of the rule reads of a
	// line timeline beside the measures of R4 (forecastReadings): DailyMaxima is, for each count
	// a scorecard gives a maximum of (waitOnlyForSlot, startedWithoutSlot, pastReviewHolding,
	// pastReviewNotHolding and inFlight), the mean over the UTC days of [From, To) of each day's
	// largest count, a day the span cuts read over its part in the span; UrgentMeans is the wait of
	// urgent work as means over its issues (UrgentMean), P0's and P1's apart, each where an issue
	// counts in it.
	//
	// A snapshot scorecard holds none of these four, nor does the first day apart. A record or a
	// run read before a scorecard held them has them nil, and is refused (ReadRuns, readRun).
	DailyMaxima map[string]float64    `json:"dailyMaxima"`
	UrgentMeans map[string]UrgentMean `json:"urgentMeans"`
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

// UrgentWait is the wait of the urgent work of one priority in a window (urgentWaits): how many
// issues count in it, and their waits from the start of each issue's wait to its first slot and to
// its done.
type UrgentWait struct {
	Issues    int         `json:"issues"`
	FirstSlot WaitMinutes `json:"firstSlot"`
	Done      WaitMinutes `json:"done"`
}

// WaitMinutes is one wait of the urgent work of one priority over its issues: the median and the
// 90th percentile (nearest rank) of their minutes, a cut wait counted to the end of the run, and
// how many of them are cut.
type WaitMinutes struct {
	MedianMinutes float64 `json:"medianMinutes"`
	P90Minutes    float64 `json:"p90Minutes"`
	Cut           int     `json:"cut"`
}

// UrgentMean is the wait of the urgent work of one priority as means over its issues
// (urgentMeans): the mean minutes from the start of each issue's wait to its first slot and to its
// done, a cut wait at its length to the end of the run.
type UrgentMean struct {
	FirstSlotMinutes float64 `json:"firstSlotMinutes"`
	DoneMinutes      float64 `json:"doneMinutes"`
}

// Score is the scorecard of w read from its record alone, with no replay (spec R4: "The tool also
// reads each measure from the record of a window"): the line timeline of its trace (lineTimeline),
// the ranks its own Dispatch events show and, at each moment, the rule its daemon ran then, read by
// Measure as a replay's two timelines are. Unplaced is how many issues a fact the trace could not
// place leaves in doubt at the window's start (unplacedAtStart); a record runs under no rule,
// branch, seed or scenario and rests on none of a replay's assumptions, so it names none.
func Score(w Window) (Scorecard, error) {
	t, err := BuildTrace(w)
	if err != nil {
		return Scorecard{}, err
	}
	return scoreTrace(t), nil
}

// scoreTrace is the scorecard of the record t is the trace of (Score), for a reader that builds the
// trace once for more than the scorecard, with what its trace alone gives (traceMeasures) and what
// the forecast reads of its line timeline (forecastReadings).
func scoreTrace(t Trace) Scorecard {
	timeline := lineTimeline(t, ranksOf(t), t.ruleAt)
	card := Measure(timeline)
	card.Source, card.Unplaced, card.Assumptions = "record", unplacedAtStart(t), []int{}
	traceMeasures(&card, t)
	forecastReadings(&card, timeline)
	return card
}

// unplacedAtStart is how many issues of t a fact the trace could not place leaves in doubt at the
// window's start (bearsOnStart), each once: the issues a start state at the window's start names
// for the trace's own doubts (SeedAndReport).
func unplacedAtStart(t Trace) int {
	issues := map[string]bool{}
	for _, fact := range startDoubts(t, t.From) {
		issues[fact.Issue] = true
	}
	return len(issues)
}
