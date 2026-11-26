package replay

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// firstDay is the span of a window's first day, which a scorecard of a longer window also gives
// apart (Scorecard.FirstDayApart, assumption 6).
const firstDay = 24 * time.Hour

// Measure is the measures of R4 over tl's span (spec R4; `replay.md`, "The measures", and the
// fixture windows' README for the rules it adds): the issues that reach done and the work left;
// the two readings of started work that waits for a slot and the issues past their review with a
// slot and without; the share of pairs of a holder and an issue that waits for a slot in which
// the holder ranks below, by rank alone and with the priority band first; the rounds and hours of
// the send-back loops by cause and the hours of the waits on other issues by what ended them; the
// slot-hours with a worker in a turn and with none; the issues in flight; the slots given out each
// day, the median wait for one and each issue's first; the wait of urgent work, which runs on past
// the span to the end of the run (urgentWaits); and, for a window longer than a day, the same but
// the wait of urgent work over its first day apart. It is the one code every scorecard is read by,
// the record's (Score) and a replay's two timelines' (Run), so a gap between them is a gap of the
// timelines and not of two readings. It fills the measures alone; the run or the record names the
// rest.
//
// It reads each state of the timeline once, in time order, after one sort of all of them, and
// keeps the pairs of priority taken by the places of their members in each of two orders of the
// board (rankOrders), so a window of hundreds of thousands of states is measured in O(n log n).
// The sort, the orders and the issues' keys are read once for both spans (newMeasuring).
func Measure(tl Timeline) Scorecard {
	m := newMeasuring(tl)
	card := m.span(tl.From, tl.To)
	card.UrgentWaits = m.urgentWaits()
	if tl.To.Sub(tl.From) > firstDay {
		day := m.span(tl.From, tl.From.Add(firstDay))
		card.FirstDayApart = &day
	}
	return card
}

// measuring is what every span Measure reads of tl reads alike: its issues' keys in order, the
// place on the board of each of its holders and issues that wait for a slot in each order priority
// taken is read in (rankOrders), and each stint of each issue as a change, in time order and by key
// within a moment.
type measuring struct {
	tl      Timeline
	keys    []string
	places  [boardOrders]map[ranked]int
	changes []stintChange
}

// stintChange is one stint of an issue as a sweep reads it: its moment, the issue and the state it
// stands in from then.
type stintChange struct {
	at    time.Time
	key   string
	state IssueState
}

// newMeasuring reads tl for its spans (measuring).
func newMeasuring(tl Timeline) measuring {
	m := measuring{tl: tl, keys: slices.Sorted(maps.Keys(tl.Issues)), places: rankOrders(tl)}
	for _, key := range m.keys {
		for _, stint := range tl.Issues[key].States {
			m.changes = append(m.changes, stintChange{at: stint.At, key: key, state: stint.IssueState})
		}
	}
	slices.SortStableFunc(m.changes, func(a, b stintChange) int { return a.at.Compare(b.at) })
	return m
}

// span is the measures of the timeline over [from, to).
func (m measuring) span(from, to time.Time) Scorecard {
	card := Scorecard{From: from, To: to, Loops: map[string]LoopHours{}, Waits: map[string]WaitHours{},
		SlotsGivenByDay: map[string]int{}, FirstSlot: map[string]time.Time{}, DoneAt: map[string]time.Time{}}
	span := to.Sub(from).Hours()
	counted := newSweep(from, to, m.places).run(m.changes)
	occupy := func(o *occupancy) Occupancy {
		if span <= 0 {
			return Occupancy{Max: o.max}
		}
		return Occupancy{Average: o.hours / span, Max: o.max, Hours: o.hours}
	}
	card.InFlight = occupy(&counted.inFlight)
	card.StartedWithoutSlot = occupy(&counted.startedWithoutSlot)
	card.WaitOnlyForSlot = occupy(&counted.waitOnlyForSlot)
	card.PastReviewHolding = occupy(&counted.pastReviewHolding)
	card.PastReviewNotHolding = occupy(&counted.pastReviewNotHolding)
	card.SlotHoursInTurn = counted.inTurn.hours
	card.SlotHoursNoTurn = counted.noTurn.hours
	if span > 0 {
		card.PriorityTaken = counted.pairs[byRank].priority / span
		card.PriorityTakenBanded = counted.pairs[byBand].priority / span
	}
	within := func(at time.Time) bool { return !at.Before(from) && at.Before(to) }
	var slotWaits []float64
	nextInto := map[phase.Phase]time.Time{}
	for _, key := range m.keys {
		c := m.tl.Issues[key]
		if state, ok := c.stateBefore(to); ok && state.InFlight {
			card.WorkLeft++
		}
		measureLoops(&card, key, c.Moves, from, to, nextInto)
		measureRounds(&card, c.Rounds, from, to)
		for _, wait := range c.Waits {
			start, end := maxTime(wait.Start, from), to
			if !wait.End.IsZero() {
				end = minTime(wait.End, to)
			}
			if !end.After(start) {
				continue
			}
			word := wait.EndedBy
			if wait.End.IsZero() || !wait.End.Before(to) {
				word = WaitOpen
			}
			waits := card.Waits[string(word)]
			waits.Count++
			waits.Hours += end.Sub(start).Hours()
			card.Waits[string(word)] = waits
		}
		// The runs of the course's waits for a slot are read at its first slot given in the span.
		var runs []int
		for _, given := range c.Given {
			if !within(given) {
				continue
			}
			card.SlotsGivenByDay[given.UTC().Format(time.DateOnly)]++
			if first, ok := card.FirstSlot[key]; !ok || given.Before(first) {
				card.FirstSlot[key] = given
			}
			if runs == nil {
				runs = c.waitRuns()
			}
			slotWaits = append(slotWaits, given.Sub(c.waitingSince(given, runs)).Minutes())
		}
	}
	card.Done = len(card.DoneAt)
	card.MedianSlotWaitMinutes = median(slotWaits)
	return card
}

// urgentPriorities is the priorities the wait of urgent work reads, each apart (urgentWaits).
var urgentPriorities = [...]record.Priority{record.P0, record.P1}

// urgentWaits is the wait of urgent work over the timeline (spec R4; `replay.md`, "The wait of
// urgent work"), by priority, P0's and P1's apart (UrgentWait): the median and the 90th percentile
// of each priority's waits (urgentReadings), and how many of them are cut. A priority no issue
// counts in has no entry.
func (m measuring) urgentWaits() map[string]UrgentWait {
	urgent := map[string]UrgentWait{}
	for i, w := range m.urgentReadings() {
		if len(w.toSlot) == 0 {
			continue
		}
		urgent[urgentPriorities[i].String()] = UrgentWait{Issues: len(w.toSlot),
			FirstSlot: WaitMinutes{MedianMinutes: median(w.toSlot), P90Minutes: percentile(w.toSlot, 0.9), Cut: w.slotCut},
			Done:      WaitMinutes{MedianMinutes: median(w.toDone), P90Minutes: percentile(w.toDone, 0.9), Cut: w.doneCut}}
	}
	return urgent
}

// urgentMeans is the wait of urgent work over the timeline as means (UrgentMean), by priority,
// P0's and P1's apart: the mean of each priority's waits to the first slot and to done
// (urgentReadings), a cut wait at its length. A priority no issue counts in has no entry.
func (m measuring) urgentMeans() map[string]UrgentMean {
	means := map[string]UrgentMean{}
	for i, w := range m.urgentReadings() {
		if len(w.toSlot) == 0 {
			continue
		}
		toSlot, _, _ := spread(w.toSlot)
		toDone, _, _ := spread(w.toDone)
		means[urgentPriorities[i].String()] = UrgentMean{FirstSlotMinutes: toSlot, DoneMinutes: toDone}
	}
	return means
}

// urgentReading is the waits of the urgent work of one priority over a timeline (urgentReadings):
// each issue's minutes to its first slot and to its done, and how many of each are cut.
type urgentReading struct {
	toSlot, toDone   []float64
	slotCut, doneCut int
}

// urgentReadings is the waits of urgent work over the timeline, by priority in the order of
// urgentPriorities: each issue whose wait as urgent work begins in the span (Course.Todo), counted
// in the highest of its own Dispatch priorities from that moment to its first slot, the priority at
// the slot's moment included; with the minutes from that moment to its first slot, the first one
// given at or after it, and to its done, its first move to done at or after it, each by the end of
// the run (Timeline.End), and to that end, a cut wait, where it has none by then.
func (m measuring) urgentReadings() [len(urgentPriorities)]urgentReading {
	end := m.tl.End
	if end.Before(m.tl.To) {
		end = m.tl.To
	}
	var read [len(urgentPriorities)]urgentReading
	for _, key := range m.keys {
		c := m.tl.Issues[key]
		if c.Todo.IsZero() {
			continue
		}
		slot, slotted := end, false
		first := sort.Search(len(c.Given), func(i int) bool { return !c.Given[i].Before(c.Todo) })
		if first < len(c.Given) && !c.Given[first].After(end) {
			slot, slotted = c.Given[first], true
		}
		done, finished := end, false
		for _, move := range c.Moves {
			if move.To == phase.Done && !move.At.Before(c.Todo) {
				if !move.At.After(end) {
					done, finished = move.At, true
				}
				break
			}
		}
		class := record.NoPriority
		for _, p := range c.Priorities {
			if p.At.After(slot) {
				break
			}
			if p.Priority != record.NoPriority && (class == record.NoPriority || p.Priority < class) {
				class = p.Priority
			}
		}
		i := slices.Index(urgentPriorities[:], class)
		if i < 0 {
			continue
		}
		w := &read[i]
		w.toSlot = append(w.toSlot, slot.Sub(c.Todo).Minutes())
		w.toDone = append(w.toDone, done.Sub(c.Todo).Minutes())
		if !slotted {
			w.slotCut++
		}
		if !finished {
			w.doneCut++
		}
	}
	return read
}

// forecastReadings fills what the forecast across a change of the rule reads of a line timeline
// beside the measures of R4 that Measure reads from it, by one code for the record's line timeline
// (scoreTrace) and a replay's (Run's line scorecard): each count's mean daily maximum
// (Scorecard.DailyMaxima) and the wait of urgent work as means (Scorecard.UrgentMeans).
func forecastReadings(card *Scorecard, tl Timeline) {
	m := newMeasuring(tl)
	card.DailyMaxima = m.dailyMaxima()
	card.UrgentMeans = m.urgentMeans()
}

// dailyMaximaCounts is each count of issues a scorecard gives a maximum of, by its name, as a sweep
// holds it: the counts Scorecard.DailyMaxima gives the mean daily maximum of, and the forecast reads.
var dailyMaximaCounts = []struct {
	name string
	of   func(*sweep) occupancy
}{
	{"waitOnlyForSlot", func(s *sweep) occupancy { return s.waitOnlyForSlot }},
	{"startedWithoutSlot", func(s *sweep) occupancy { return s.startedWithoutSlot }},
	{"pastReviewHolding", func(s *sweep) occupancy { return s.pastReviewHolding }},
	{"pastReviewNotHolding", func(s *sweep) occupancy { return s.pastReviewNotHolding }},
	{"inFlight", func(s *sweep) occupancy { return s.inFlight }},
}

// dailyMaxima is, for each count of dailyMaximaCounts, the mean over the UTC days of the timeline's
// span of each day's largest count, a day the span cuts read over its part in the span
// (Scorecard.DailyMaxima): one sweep a day, each read as the sweep of the whole span reads its
// maximum. Each count is 0 for a span of no length.
func (m measuring) dailyMaxima() map[string]float64 {
	maxima := make(map[string]float64, len(dailyMaximaCounts))
	for _, c := range dailyMaximaCounts {
		maxima[c.name] = 0
	}
	days := 0
	for from := m.tl.From.UTC(); from.Before(m.tl.To); days++ {
		to := minTime(time.Date(from.Year(), from.Month(), from.Day()+1, 0, 0, 0, 0, time.UTC), m.tl.To)
		day := newSweep(from, to, m.places).run(m.changes)
		for _, c := range dailyMaximaCounts {
			maxima[c.name] += float64(c.of(day).max)
		}
		from = to
	}
	if days > 0 {
		for name := range maxima {
			maxima[name] /= float64(days)
		}
	}
	return maxima
}

// measureLoops adds to card the moves of key in [from, to): its move to done, and each move back
// its cause names, one round of that cause, its hours from the move to the issue's next entry into
// the phase the move left, or to to. The next entries are read in one walk back over the moves,
// into nextInto, which it empties first.
func measureLoops(card *Scorecard, key string, moves []CourseMove, from, to time.Time, nextInto map[phase.Phase]time.Time) {
	clear(nextInto)
	for i := len(moves) - 1; i >= 0; i-- {
		move := moves[i]
		back, ok := nextInto[move.From]
		nextInto[move.To] = move.At
		if move.At.Before(from) || !move.At.Before(to) {
			continue
		}
		if move.To == phase.Done {
			if done, seen := card.DoneAt[key]; !seen || move.At.After(done) {
				card.DoneAt[key] = move.At
			}
		}
		if !phase.MovesBack(move.From, move.To) || move.Cause == "" {
			continue
		}
		end := to
		if ok && back.Before(to) {
			end = back
		}
		card.addRound(move.Cause, end.Sub(move.At))
	}
}

// measureRounds adds to card each round of a send-back loop that moved no phase (Course.Rounds)
// beginning in [from, to): one round of its cause, its hours from its start to its end, or to to.
func measureRounds(card *Scorecard, rounds []CourseRound, from, to time.Time) {
	for _, round := range rounds {
		if round.Start.Before(from) || !round.Start.Before(to) {
			continue
		}
		end := to
		if !round.End.IsZero() && round.End.Before(to) {
			end = round.End
		}
		card.addRound(round.Cause, end.Sub(round.Start))
	}
}

// addRound adds one round of a send-back loop of cause, lasting lasted, to card's loops.
func (card *Scorecard) addRound(cause Cause, lasted time.Duration) {
	loop := card.Loops[string(cause)]
	loop.Rounds++
	loop.Hours += lasted.Hours()
	card.Loops[string(cause)] = loop
}

// traceMeasures fills what a scorecard reads from a trace alone, beside the measures of R4 that
// Measure reads from its timeline, by one code for the record's trace (scoreTrace) and a replay's
// own (Run's line scorecard): the issues of t's project Dispatch shows reaching done in [From, To)
// by any actor (Scorecard.DoneByAnyone), each tree's hours of [From, To) its design gate stood
// closed, read as the line timeline reads the gate (gateStepsOf), for each tree whose gate stood
// closed at all (Scorecard.GateClosedHours), each issue of the project filed in [From, To), at its
// issue.created event (Scorecard.Filed), and each moment in [From, To) a pass gave an issue of the
// project a slot, as the line timeline reads it from the slot lines (Scorecard.SlotsGiven).
func traceMeasures(card *Scorecard, t Trace) {
	within := func(at time.Time) bool { return !at.Before(t.From) && at.Before(t.To) }
	card.GateClosedHours, card.Filed, card.SlotsGiven = map[string]float64{}, map[string]time.Time{}, []time.Time{}
	for _, move := range t.Slots {
		if iss, ok := t.Issues[move.Issue]; ok && move.Given && iss.Project == t.Project && within(move.At) {
			card.SlotsGiven = append(card.SlotsGiven, move.At)
		}
	}
	slices.SortStableFunc(card.SlotsGiven, time.Time.Compare)
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		iss := t.Issues[key]
		if iss.Project != t.Project {
			continue
		}
		if doneIn(iss, t.From, t.To) {
			card.DoneByAnyone++
		}
		for _, event := range iss.Dispatch {
			if event.Type == "issue.created" {
				if within(event.At) {
					card.Filed[key] = event.At
				}
				break
			}
		}
		if len(iss.Gates) == 0 {
			continue
		}
		gate, hours := gateStepsOf(iss, t.GateLines), 0.0
		for i, at := range gate.at {
			if gate.open[i] {
				continue
			}
			start, end := maxTime(at, t.From), t.To
			if i+1 < len(gate.at) {
				end = minTime(gate.at[i+1], t.To)
			}
			if end.After(start) {
				hours += end.Sub(start).Hours()
			}
		}
		if hours > 0 {
			card.GateClosedHours[key] = hours
		}
	}
}

// doneIn says whether iss's Dispatch status turned done in [from, to), by any actor: an issue event
// in the span that shows it done where the event before it did not.
func doneIn(iss *IssueTrace, from, to time.Time) bool {
	wasDone := false
	for _, event := range iss.Dispatch {
		if event.Issue == nil {
			continue
		}
		done := event.Issue.Status == record.DoneStatus
		if done && !wasDone && !event.At.Before(from) && event.At.Before(to) {
			return true
		}
		wasDone = done
	}
	return false
}

// heldHours is the slot-hours the issues keys names hold in tl's span: the time each stands in a
// state that holds a slot, summed in the order of their keys.
func (tl Timeline) heldHours(keys map[string]bool) float64 {
	hours := 0.0
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		c := tl.Issues[key]
		if c == nil {
			continue
		}
		for i, stint := range c.States {
			if !stint.Holds {
				continue
			}
			start, end := maxTime(stint.At, tl.From), tl.To
			if i+1 < len(c.States) {
				end = minTime(c.States[i+1].At, tl.To)
			}
			if end.After(start) {
				hours += end.Sub(start).Hours()
			}
		}
	}
	return hours
}

// stateBefore is the state the course stands in just before at, and false where it stands in
// none yet.
func (c *Course) stateBefore(at time.Time) (IssueState, bool) {
	i := c.stintBefore(at)
	if i < 0 {
		return IssueState{}, false
	}
	return c.States[i].IssueState, true
}

// stintBefore is the index of the stint the course stands in just before at, -1 for none.
func (c *Course) stintBefore(at time.Time) int {
	return sort.Search(len(c.States), func(i int) bool { return !c.States[i].At.Before(at) }) - 1
}

// waitsForSlot says whether state is an issue that waits for a slot.
func waitsForSlot(state IssueState) bool { return state.Waiting && !state.Holds }

// waitRuns is, for each stint of the course, the index of the first stint of the run of stints in
// which the issue waits for a slot that it stands in, or its own index where it does not wait.
func (c *Course) waitRuns() []int {
	runs := make([]int, len(c.States))
	for i, stint := range c.States {
		runs[i] = i
		if i > 0 && waitsForSlot(stint.IssueState) && waitsForSlot(c.States[i-1].IssueState) {
			runs[i] = runs[i-1]
		}
	}
	return runs
}

// waitingSince is the moment the issue that took a slot at at began to wait for it: the start of
// the run of stints in which it waited for a slot that stands just before at (runs, waitRuns); at
// itself where it did not wait just before, having begun to wait in the pass that gave it the
// slot.
func (c *Course) waitingSince(at time.Time, runs []int) time.Time {
	i := c.stintBefore(at)
	if i < 0 || !waitsForSlot(c.States[i].IssueState) {
		return at
	}
	return c.States[runs[i]].At
}

// median is the median of values, the mean of the two middle ones for an even count, 0 for none.
func median(values []float64) float64 {
	return sortedMedian(slices.Sorted(slices.Values(values)))
}

// sortedMedian is the median of sorted, values in order, as median gives it.
func sortedMedian(sorted []float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// minTime and maxTime are the earlier and the later of a and b.
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// occupancy is a count's integral in issue-hours over a span and its largest value over any
// stretch of it.
type occupancy struct {
	count int
	hours float64
	max   int
}

// sweep reads a timeline's states in time order over [from, to): the counts of the occupancies
// and the pairs of a holder and an issue that waits for a slot in each order of the board, and
// their integrals.
type sweep struct {
	from, to time.Time

	inFlight, startedWithoutSlot, waitOnlyForSlot, pastReviewHolding, pastReviewNotHolding, inTurn, noTurn occupancy
	// nHolders and nWaiting count the holders and the issues that wait for a slot, and pairs those
	// of them in each order of the board (boardOrders).
	nHolders, nWaiting int
	pairs              [boardOrders]pairCount
}

// The orders of the board priority taken is read in (Scorecard.PriorityTaken and
// Scorecard.PriorityTakenBanded): by rank alone (ranked.compare), the order of the lines the
// recorded windows ran, and with the priority band first (ranked.compareBanded), today's line's.
const (
	byRank = iota
	byBand
	boardOrders
)

// pairCount counts the pairs of a holder and an issue that waits for a slot in one order of the
// board: places is the place of each holder and waiting issue in that order (rankOrders); holders
// and waiting count the holders and the issues that wait for a slot at each place, inverted is the
// pairs of them in which the waiting issue ranks above the holder, and priority the integral of
// the share of those pairs.
type pairCount struct {
	places           map[ranked]int
	holders, waiting fenwick
	inverted         int
	priority         float64
}

// ranked is what places an issue on the board: its key, its tree root's key, the Dispatch ranks
// of the two, and the priority that places it in today's line's band (IssueState.Priority).
type ranked struct {
	key, tree, rank, rootRank string
	priority                  record.Priority
}

// rankedOf is what places state's issue key on the board.
func rankedOf(key string, state IssueState) ranked {
	tree := state.Tree
	if tree == "" {
		tree = key
	}
	return ranked{key: key, tree: tree, rank: state.Rank, rootRank: state.RootRank, priority: state.Priority}
}

// compare orders r against other on the board by rank alone: by their tree roots (record.RankLess
// over each root's rank and key), and within one tree root's place by their own (record.RankLess
// over the issue's rank and key), as the waiting line orders its starting part under a rule that
// does not band it (record.Waiting) and the slots reading places a holder against the first issue
// that waits for a slot (record.SlotsReadingOf).
func (r ranked) compare(other ranked) int {
	root, otherRoot := record.Issue{Key: r.tree, Rank: r.rootRank}, record.Issue{Key: other.tree, Rank: other.rootRank}
	own, otherOwn := record.Issue{Key: r.key, Rank: r.rank}, record.Issue{Key: other.key, Rank: other.rank}
	switch {
	case record.RankLess(root, otherRoot):
		return -1
	case record.RankLess(otherRoot, root):
		return 1
	case record.RankLess(own, otherOwn):
		return -1
	case record.RankLess(otherOwn, own):
		return 1
	}
	return 0
}

// compareBanded orders r against other in today's line's order, as record.Waiting lays the line
// out under a rule that bands it (record.SlotRule.PriorityFirst, the daemon's own): first by the
// band of the priority that places each (lineBand), and within one band by rank (compare).
func (r ranked) compareBanded(other ranked) int {
	return cmp.Or(cmp.Compare(lineBand(r.priority), lineBand(other.priority)), r.compare(other))
}

// lineBand is where the band of priority p stands in today's line, lower first: P0 to P3 in their
// order, and none after P3, as record.Waiting bands the line.
func lineBand(p record.Priority) int {
	if p == record.NoPriority {
		return int(record.P3) + 1
	}
	return int(p)
}

// rankOrders is the place on the board of every issue tl holds as a holder or as an issue that
// waits for a slot, each with the ranks and the priority it stood at then, in each order of the
// board (boardOrders), each one sorted order.
func rankOrders(tl Timeline) [boardOrders]map[ranked]int {
	seen := map[ranked]bool{}
	for key, c := range tl.Issues {
		for _, stint := range c.States {
			if stint.Holds || waitsForSlot(stint.IssueState) {
				seen[rankedOf(key, stint.IssueState)] = true
			}
		}
	}
	var places [boardOrders]map[ranked]int
	for i, compare := range [boardOrders]func(ranked, ranked) int{byRank: ranked.compare, byBand: ranked.compareBanded} {
		order := slices.SortedFunc(maps.Keys(seen), compare)
		places[i] = make(map[ranked]int, len(order))
		for place, r := range order {
			places[i][r] = place
		}
	}
	return places
}

// fenwick counts the members standing at each place of an order and says how many stand below a
// place, each in O(log n): a binary indexed tree.
type fenwick []int

// newFenwick is a fenwick of places places.
func newFenwick(places int) fenwick { return make(fenwick, places+1) }

// add adds delta members at place.
func (f fenwick) add(place, delta int) {
	for i := place + 1; i < len(f); i += i & -i {
		f[i] += delta
	}
}

// below is how many members stand at a place below place.
func (f fenwick) below(place int) int {
	n := 0
	for i := place; i > 0; i -= i & -i {
		n += f[i]
	}
	return n
}

// newSweep is a sweep over [from, to) of the holders and issues that wait for a slot at places, in
// each order of the board.
func newSweep(from, to time.Time, places [boardOrders]map[ranked]int) *sweep {
	s := &sweep{from: from, to: to}
	for i := range s.pairs {
		s.pairs[i] = pairCount{places: places[i], holders: newFenwick(len(places[i])), waiting: newFenwick(len(places[i]))}
	}
	return s
}

// run reads every change of changes, stints in time order (measuring), each moment's together, and
// adds each stretch between two moments to the integrals. A moment at or after the span's end ends
// the reading: add clips every stretch from it on to nothing.
func (s *sweep) run(changes []stintChange) *sweep {
	states := map[string]IssueState{}
	previous := s.from
	for i := 0; i < len(changes); {
		at := changes[i].at
		s.add(previous, at)
		if !at.Before(s.to) {
			return s
		}
		for ; i < len(changes) && changes[i].at.Equal(at); i++ {
			s.apply(changes[i].key, states[changes[i].key], changes[i].state)
			states[changes[i].key] = changes[i].state
		}
		previous = at
	}
	s.add(previous, s.to)
	return s
}

// add adds the stretch [a, b), clipped to the sweep's span, to every integral at the counts that
// stand over it.
func (s *sweep) add(a, b time.Time) {
	a, b = maxTime(a, s.from), minTime(b, s.to)
	if !b.After(a) {
		return
	}
	hours := b.Sub(a).Hours()
	for _, o := range []*occupancy{&s.inFlight, &s.startedWithoutSlot, &s.waitOnlyForSlot, &s.pastReviewHolding,
		&s.pastReviewNotHolding, &s.inTurn, &s.noTurn} {
		o.hours += float64(o.count) * hours
		o.max = max(o.max, o.count)
	}
	if pairs := s.nHolders * s.nWaiting; pairs > 0 {
		for i := range s.pairs {
			s.pairs[i].priority += float64(s.pairs[i].inverted) / float64(pairs) * hours
		}
	}
}

// apply moves the counts from key's old state to its new one.
func (s *sweep) apply(key string, old, state IssueState) {
	count := func(o *occupancy, was, is bool) {
		if was {
			o.count--
		}
		if is {
			o.count++
		}
	}
	returned := func(st IssueState) bool { return st.Part == record.PartReturned && !st.Holds }
	// Reading (b) is every started issue before its review that holds no slot, its waits on other
	// issues included: a returned member of the line, and a parent in awaiting_children, which
	// stands in no line and waits on its children, never only for a slot.
	withoutSlot := func(st IssueState) bool { return returned(st) || st.AwaitingChildren && !st.Holds }
	count(&s.inFlight, old.InFlight, state.InFlight)
	count(&s.startedWithoutSlot, withoutSlot(old), withoutSlot(state))
	count(&s.waitOnlyForSlot, returned(old) && !old.Blocked, returned(state) && !state.Blocked)
	count(&s.pastReviewHolding, old.PastReview && old.Holds, state.PastReview && state.Holds)
	count(&s.pastReviewNotHolding, old.PastReview && !old.Holds, state.PastReview && !state.Holds)
	count(&s.inTurn, old.Holds && old.InTurn, state.Holds && state.InTurn)
	count(&s.noTurn, old.Holds && !old.InTurn, state.Holds && !state.InTurn)
	if old.Holds {
		s.holder(rankedOf(key, old), -1)
	}
	if waitsForSlot(old) {
		s.waiter(rankedOf(key, old), -1)
	}
	if state.Holds {
		s.holder(rankedOf(key, state), 1)
	}
	if waitsForSlot(state) {
		s.waiter(rankedOf(key, state), 1)
	}
}

// holder adds delta holders at r's place in each order of the board, and with them the pairs in
// which an issue that waits for a slot at a place below ranks above it.
func (s *sweep) holder(r ranked, delta int) {
	for i := range s.pairs {
		c := &s.pairs[i]
		place := c.places[r]
		c.inverted += delta * c.waiting.below(place)
		c.holders.add(place, delta)
	}
	s.nHolders += delta
}

// waiter adds delta issues that wait for a slot at r's place in each order of the board, and with
// them the pairs in which a holder at a place above ranks below it.
func (s *sweep) waiter(r ranked, delta int) {
	for i := range s.pairs {
		c := &s.pairs[i]
		place := c.places[r]
		c.inverted += delta * (s.nHolders - c.holders.below(place+1))
		c.waiting.add(place, delta)
	}
	s.nWaiting += delta
}

// Measured is one number of a scorecard by its name, the JSON path of its value in the Scorecard
// (inFlight.hours, loops[send_back].rounds, slotsGivenByDay[2001-03-05]), and its value.
type Measured struct {
	Name  string
	Value float64
}

// MeasuresOf is every number of card, in the order the Scorecard holds them, each loop's cause in
// the order of Causes and each other map's entries by key, and the first day's apart under
// firstDayApart.: what `legion replay scorecard` prints and what a gap between two scorecards is
// read over (Gaps). Each issue's first slot and done are times, which MeasuresOf leaves to Gaps,
// and the run's own numbers (seed, slowdown) and its assumptions name no measure.
func MeasuresOf(card Scorecard) []Measured {
	measured := measuresOf(card, "")
	if card.FirstDayApart != nil {
		measured = append(measured, measuresOf(*card.FirstDayApart, firstDayPrefix)...)
	}
	return measured
}

// measuresOf is MeasuresOf over card's own numbers, each name under prefix.
func measuresOf(card Scorecard, prefix string) []Measured {
	var measured []Measured
	add := func(name string, value float64) {
		measured = append(measured, Measured{Name: prefix + name, Value: value})
	}
	occupancy := func(name string, o Occupancy) {
		add(name+".average", o.Average)
		add(name+".max", float64(o.Max))
		add(name+".hours", o.Hours)
	}
	add("unplaced", float64(card.Unplaced))
	add("done", float64(card.Done))
	add("workLeft", float64(card.WorkLeft))
	occupancy("waitOnlyForSlot", card.WaitOnlyForSlot)
	occupancy("startedWithoutSlot", card.StartedWithoutSlot)
	occupancy("pastReviewHolding", card.PastReviewHolding)
	occupancy("pastReviewNotHolding", card.PastReviewNotHolding)
	add("priorityTaken", card.PriorityTaken)
	add("priorityTakenBanded", card.PriorityTakenBanded)
	for _, cause := range Causes {
		if loop, ok := card.Loops[string(cause)]; ok {
			add(fmt.Sprintf("loops[%s].rounds", cause), float64(loop.Rounds))
			add(fmt.Sprintf("loops[%s].hours", cause), loop.Hours)
		}
	}
	for _, word := range slices.Sorted(maps.Keys(card.Waits)) {
		add(fmt.Sprintf("waits[%s].count", word), float64(card.Waits[word].Count))
		add(fmt.Sprintf("waits[%s].hours", word), card.Waits[word].Hours)
	}
	add("slotHoursInTurn", card.SlotHoursInTurn)
	add("slotHoursNoTurn", card.SlotHoursNoTurn)
	occupancy("inFlight", card.InFlight)
	for _, day := range slices.Sorted(maps.Keys(card.SlotsGivenByDay)) {
		add(fmt.Sprintf("slotsGivenByDay[%s]", day), float64(card.SlotsGivenByDay[day]))
	}
	add("medianSlotWaitMinutes", card.MedianSlotWaitMinutes)
	return measured
}

// MeasureGap is one measure of a replay read from its two timelines (design 10.1): its value in
// the snapshot timeline, which the admission passes' own views give, and in the line timeline,
// which BuildTrace reads from the replay's log through the window's line kinds as it reads a
// record. Their difference is the error of the line reconstruction the record's scorecard rests on.
type MeasureGap struct {
	Measure  string  `json:"measure"`
	Snapshot float64 `json:"snapshot"`
	Line     float64 `json:"line"`
}

// Gap is the line timeline's value less the snapshot timeline's.
func (g MeasureGap) Gap() float64 { return g.Line - g.Snapshot }

// Gaps is each measure of snapshot and of line side by side (MeasuresOf), a measure one of them
// lacks at zero there, and each issue's first slot and done, firstSlot[KEY] and doneAt[KEY], in
// minutes from the window's start: an issue one of them gives no time for stands there at the
// window's end, the soonest it could come.
func Gaps(snapshot, line Scorecard) []MeasureGap {
	lineMeasures := MeasuresOf(line)
	lineValues := make(map[string]float64, len(lineMeasures))
	for _, measure := range lineMeasures {
		lineValues[measure.Name] = measure.Value
	}
	var gaps []MeasureGap
	seen := map[string]bool{}
	for _, measure := range MeasuresOf(snapshot) {
		seen[measure.Name] = true
		gaps = append(gaps, MeasureGap{Measure: measure.Name, Snapshot: measure.Value, Line: lineValues[measure.Name]})
	}
	for _, measure := range lineMeasures {
		if !seen[measure.Name] {
			gaps = append(gaps, MeasureGap{Measure: measure.Name, Line: measure.Value})
		}
	}
	span := snapshot.To.Sub(snapshot.From).Minutes()
	minutes := func(at time.Time, ok bool) float64 {
		if !ok {
			return span
		}
		return at.Sub(snapshot.From).Minutes()
	}
	times := func(name string, a, b map[string]time.Time) {
		for _, key := range slices.Sorted(maps.Keys(unionKeys(a, b))) {
			x, inA := a[key]
			y, inB := b[key]
			gaps = append(gaps, MeasureGap{Measure: fmt.Sprintf("%s[%s]", name, key), Snapshot: minutes(x, inA), Line: minutes(y, inB)})
		}
	}
	times("firstSlot", snapshot.FirstSlot, line.FirstSlot)
	times("doneAt", snapshot.DoneAt, line.DoneAt)
	return gaps
}

// unionKeys is the keys of a and b together, each once.
func unionKeys[V any](a, b map[string]V) map[string]struct{} {
	keys := make(map[string]struct{}, len(a)+len(b))
	for key := range a {
		keys[key] = struct{}{}
	}
	for key := range b {
		keys[key] = struct{}{}
	}
	return keys
}
