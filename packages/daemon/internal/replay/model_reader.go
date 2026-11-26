package replay

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/signals"
)

// The load models read as a replay runs (spec R11). A replay's driver feeds a ModelReader its own
// course in time order, as it happens: the issues in flight (Flight), each recorded work segment as
// the replay starts it (Segment), each stretch in flight of an issue whose work the record never
// ran (DrawnEnters, DrawnLeaves), and each pull request of such work entering the merge queue
// (DrawnSegment). It reads each modelled signal at the moment it has reached, and the next moment
// after it at which the signal would cross a limit if no new event came.
//
// Each modelled value at a moment reads only the course up to that moment. A minute's tokens read
// the issues in flight in that minute, and a refused minute the refused minutes before it. A kill
// reads the start of its own segment, or its drawn issue's own hours in flight before it. A pull
// request's exit reads the entries before it in its own queue. A segment, a stretch or an entry
// that the replay starts now lies at or after now. So what the reader has given stays given, and
// it does each moment's work once: it models each minute once, when the minute ends, and each pull
// request once, when it enters. A pass costs what the moments near it cost, not the window's
// length. At each moment the reader gives what LoadModels.Replay gives over the replay's whole
// timeline (TestTheModelReaderGivesWhatTheWholeTimelineGives).
//
// A minute that has not ended, and the minutes after it, are read at the issues in flight now, as
// they stand if no new event came. A Next function reads the course so: the provider's headroom
// through each minute boundary up to the end of the provider's record (HeadroomNext), at the
// minute's threshold of issues in flight (minuteThresholds), so that no crossing costs more than
// a search over the minutes ahead.

// ModelReader is the three modelled signals of one replay, read as the replay runs (LoadModels.Reader).
// Its counts are LoadModels.Replay's: the kills the record could not place in a segment of their
// issue, which are kept at their recorded times; the pull requests the record shows in no segment
// of an issue, which enter at their recorded times; and the pull requests that no recorded exit and
// no rate serves, which stand in the queue. SuppliedMinutes counts the minutes it has modelled
// against the project's token limit its caller gives, the record holding no limit of the
// project's own there (assumption 49). Before the replay's start, the course is the record's own:
// the record's issues in flight, and each segment where the record started it.
type ModelReader struct {
	KillsApart      int
	EntriesApart    int
	Unserved        int
	SuppliedMinutes int

	r *modelRecord
	// now is the latest moment fed or read. Nothing before it changes.
	now time.Time
	// flight is the issues in flight: the record's steps before the replay's start, then the
	// replay's own.
	flight stretchCount

	// The provider's record is held as a run of minutes from first, one for each minute up to the
	// record's last minute. Each entry is the minute's line, where the record holds one, with the
	// record's issue-hours in flight over it. made is each minute the model has given so far: every
	// minute that ended by now, in order. refused is how many of them the project's own limit
	// refused; the record's refusals per refused minute are rounded by that count. covered is each
	// moment, in order, at which the record begins or stops holding the minute before.
	first    time.Time
	minutes  []readerMinute
	made     []signals.Minute
	refused  int
	covered  []time.Time
	headroom map[float64]*minuteThresholds
	refusing *minuteThresholds

	// starts is the replay's start of each recorded segment it has run, and segmentKills each
	// recorded kill's offset in its segment, by segment. kills is each modelled kill the reader
	// has placed, in order. parts is each part of the span with a kill rate, which drawn work takes.
	// drawn is each drawn issue's accumulator, and drawnNext the earliest kill due of the drawn
	// issues in flight, zero for none.
	starts       map[segmentKey]time.Time
	segmentKills map[segmentKey][]time.Duration
	kills        []time.Time
	parts        []ratedPart
	drawn        map[string]*drawnAccrual
	drawnOrder   []string
	drawnNext    time.Time

	// queues is each repository's queue, by its name in lower case, and queueOrder the same in the
	// order of their names; segmentPulls each recorded pull request placed in a segment, by that
	// segment; waits each modelled wait that can still count in the queue signal at now or later:
	// each that began, standing or leaving in the 6 hours before now or after it, and each that
	// keeps its recorded wait and has not begun.
	queues       map[string]*queueReader
	queueOrder   []*queueReader
	segmentPulls map[segmentKey][]queuedPull
	waits        []signals.Wait

	// sources says which setbacks the back-off folds. fold is how far the reader has folded them.
	sources SetbackSources
	fold    setbackFold
}

// SetbackSources is which events of the modelled signals the back-off folds as its setbacks: the
// refusals by the project's own limit (Refusals) and the kills (Kills). A model that gives no limit
// for its signal gives none.
type SetbackSources struct {
	Refusals, Kills bool
}

// readerMinute is one minute of the provider's record as the reader holds it: whether the record
// holds it, its line, the record's issue-hours in flight over it, and the limit in force in it
// where limited says it has one (Window.limitIn), assumed saying that limit is the caller's token
// limit.
type readerMinute struct {
	held     bool
	line     ProviderMinute
	recorded float64
	limit    int64
	limited  bool
	assumed  bool
}

// drawnAccrual is one drawn issue's accumulator: its sum, whether it stands in flight now, and from
// when; while it does, the part of the span (ModelReader.parts) and the moment in it its sum
// stands at, and its next kill, zero for none.
type drawnAccrual struct {
	sum    float64
	open   bool
	from   time.Time
	part   int
	cursor time.Time
	next   time.Time
}

// queueReader is one repository's queue as the reader serves it: each entry of a pull request that
// reaches its head and has not been served yet, in time order, and how far its service has come.
type queueReader struct {
	q       *recordedQueue
	pending []time.Time
	service queueService
}

// queuedPull is a recorded pull request placed in a segment, with its repository's queue.
type queuedPull struct {
	queue *queueReader
	pull  recordedPull
}

// setbackFold is how far the reader has folded the setbacks under one ceiling (ModelReader.Setbacks):
// the last moment folded (through), the allowance after the last setback folded and its moment, and
// the setbacks that still count (kept): those from the last setback before which the allowance
// stood at the ceiling on.
type setbackFold struct {
	ceiling   int
	through   time.Time
	allowance int
	last      time.Time
	kept      []signals.Setback
	begun     bool
}

// minuteThreshold is the largest number of issues in flight the reader models a minute at. No
// threshold reads past it.
const minuteThreshold = 1 << 20

// Reader is a reader of the modelled signals of a replay that starts at from (ModelReader), its
// setbacks as sources lets them, tokenLimit being the project's token limit in uncached input
// tokens a minute that it models a minute of the provider against where the record holds no limit
// of the project's own, zero for none (assumption 49). Before from, the reader holds the record's
// own course. It refuses models that hold no window's record.
func (m LoadModels) Reader(from time.Time, sources SetbackSources, tokenLimit int64) (*ModelReader, error) {
	r := m.fit
	if r == nil {
		return nil, errNoModelRecord
	}
	x := &ModelReader{r: r, now: from, headroom: map[float64]*minuteThresholds{}, starts: map[segmentKey]time.Time{},
		segmentKills: map[segmentKey][]time.Duration{}, parts: r.ratedParts(), drawn: map[string]*drawnAccrual{},
		queues: map[string]*queueReader{}, segmentPulls: map[segmentKey][]queuedPull{}, sources: sources}
	for i, at := range r.flight.from {
		if !at.Before(from) {
			break
		}
		x.flight.from, x.flight.counts = append(x.flight.from, at), append(x.flight.counts, r.flight.counts[i])
	}
	x.readMinutes(tokenLimit)
	for _, segment := range r.segments {
		if segment.start.Before(from) {
			x.starts[segment.key()] = segment.start
		}
	}
	for _, kill := range r.kills {
		switch {
		case !kill.placed:
			x.kills, x.KillsApart = append(x.kills, kill.at), x.KillsApart+1
		case kill.segment.start.Before(from):
			x.kills = append(x.kills, kill.at)
		default:
			key := kill.segment.key()
			x.segmentKills[key] = append(x.segmentKills[key], kill.at.Sub(kill.segment.start))
		}
	}
	slices.SortFunc(x.kills, time.Time.Compare)
	for _, q := range r.queues {
		qr := &queueReader{q: q}
		x.queues[q.key] = qr
		x.queueOrder = append(x.queueOrder, qr)
		for _, pull := range q.pulls {
			switch {
			case !pull.placed:
				x.EntriesApart++
			case !pull.segment.start.Before(from):
				key := pull.segment.key()
				x.segmentPulls[key] = append(x.segmentPulls[key], queuedPull{queue: qr, pull: pull})
				continue
			}
			x.enter(qr, pull, pull.added)
		}
	}
	return x, nil
}

// readMinutes reads the provider's record into the run of minutes, each with the limit in force in
// it, the record's or else tokenLimit (Window.limitIn), and the moments at which the record begins
// or stops holding the minute before (as SignalRecord reads them). A line whose moment is not the
// start of a whole minute is no minute a definition reads, and the run holds none of it.
func (x *ModelReader) readMinutes(tokenLimit int64) {
	r := x.r
	var first, last time.Time
	for _, line := range r.minutes {
		if !line.Minute.Equal(line.Minute.Truncate(time.Minute)) {
			continue
		}
		if first.IsZero() || line.Minute.Before(first) {
			first = line.Minute
		}
		last = maxTime(last, line.Minute)
	}
	if first.IsZero() {
		return
	}
	x.first = first
	x.minutes = make([]readerMinute, int(last.Sub(first)/time.Minute)+1)
	for _, line := range r.minutes {
		if !line.Minute.Equal(line.Minute.Truncate(time.Minute)) {
			continue
		}
		start := line.Minute
		limit, limited, assumed := r.w.limitIn(line, tokenLimit)
		x.minutes[int(start.Sub(first)/time.Minute)] = readerMinute{held: true, line: line, recorded: r.flight.hours(start, start.Add(time.Minute)),
			limit: limit, limited: limited, assumed: assumed}
	}
	for i, minute := range x.minutes {
		if !minute.held {
			continue
		}
		start := x.minuteStart(i)
		if i == 0 || !x.minutes[i-1].held {
			x.covered = append(x.covered, start.Add(time.Minute))
		}
		if i+1 == len(x.minutes) || !x.minutes[i+1].held {
			x.covered = append(x.covered, start.Add(2*time.Minute))
		}
	}
}

// minuteStart is the start of the minute at index i of the run of minutes.
func (x *ModelReader) minuteStart(i int) time.Time {
	return x.first.Add(time.Duration(i) * time.Minute)
}

// minuteIndex is the index in the run of minutes of the minute that starts at start, which may lie
// outside it.
func (x *ModelReader) minuteIndex(start time.Time) int {
	d := start.Sub(x.first)
	i := int(d / time.Minute)
	if d%time.Minute < 0 {
		i--
	}
	return i
}

// Flight records that issues stand in flight in the replay from at on. It refuses a moment before
// one the reader has reached and a count below zero.
func (x *ModelReader) Flight(at time.Time, issues int) error {
	switch {
	case issues < 0:
		return fmt.Errorf("replay: the replay's issues in flight at %s are %d, below zero", stamp(at), issues)
	case at.Before(x.now):
		return fmt.Errorf("replay: the replay's issues in flight at %s come after a reading at %s", stamp(at), stamp(x.now))
	}
	x.now = at
	n := len(x.flight.from)
	switch {
	case n > 0 && x.flight.from[n-1].Equal(at):
		x.flight.counts[n-1] = issues
	case n > 0 && x.flight.counts[n-1] == issues:
	default:
		x.flight.from, x.flight.counts = append(x.flight.from, at), append(x.flight.counts, issues)
	}
	return nil
}

// inFlight is the issues in flight now, and from then on if no new event came.
func (x *ModelReader) inFlight() int {
	if n := len(x.flight.counts); n > 0 {
		return x.flight.counts[n-1]
	}
	return 0
}

// Segment records that the replay started its run of issue's recorded work segment, which the
// record started at recorded, at replayed: each recorded kill of the segment comes at its offset in
// the replay's run, and each pull request of it enters the merge queue so (memoryForm, queueForm). A
// segment runs once. A later start of it changes nothing.
func (x *ModelReader) Segment(issue string, recorded, replayed time.Time) {
	key := segmentKey{issue: issue, start: recorded.UnixNano()}
	if _, ran := x.starts[key]; ran {
		return
	}
	x.starts[key] = replayed
	for _, offset := range x.segmentKills[key] {
		x.placeKill(replayed.Add(offset))
	}
	for _, queued := range x.segmentPulls[key] {
		x.enter(queued.queue, queued.pull, replayed.Add(queued.pull.added.Sub(queued.pull.segment.start)))
	}
}

// DrawnSegment records that the replay started, at replayed, a piece of drawn work that copies the
// work segment of source the record started at recorded: each pull request the record shows
// entering the merge queue in that segment enters there at its offset in the replay's run. It is
// work the record never ran, so each such pull request reaches the head of its queue in order of
// entry (queueForm).
func (x *ModelReader) DrawnSegment(source string, recorded, replayed time.Time) {
	key := segmentKey{issue: source, start: recorded.UnixNano()}
	for _, q := range x.r.queues {
		for _, pull := range q.pulls {
			if pull.placed && pull.segment.key() == key {
				x.Entry(q.repo, replayed.Add(pull.added.Sub(pull.segment.start)))
			}
		}
	}
}

// Entry records a pull request of work the record never ran entering the merge queue of repo
// (owner/name, compared without regard to case) at at. A repository whose queue the record does not
// hold has no exit and no rate, so the pull request stands.
func (x *ModelReader) Entry(repo string, at time.Time) {
	qr, known := x.queues[strings.ToLower(repo)]
	if !known {
		x.waits, x.Unserved = append(x.waits, signals.Wait{Added: at}), x.Unserved+1
		return
	}
	insertTime(&qr.pending, at)
}

// enter enters pull, of qr's queue, at entry: one the record removed before the head keeps its
// recorded wait, and every other waits to reach the head.
func (x *ModelReader) enter(qr *queueReader, pull recordedPull, entry time.Time) {
	if pull.early {
		x.waits = append(x.waits, signals.Wait{Added: entry, Left: entry.Add(pull.left.Sub(pull.added))})
		return
	}
	insertTime(&qr.pending, entry)
}

// DrawnEnters records that issue, whose work the record never ran, stands in flight from at on. Its
// accumulator goes on from where its last stretch left it (drawnKills).
func (x *ModelReader) DrawnEnters(issue string, at time.Time) {
	a := x.drawn[issue]
	if a == nil {
		a = &drawnAccrual{}
		x.drawn[issue] = a
		x.drawnOrder = append(x.drawnOrder, issue)
	}
	if a.open {
		return
	}
	a.open, a.from = true, at
	a.part = sort.Search(len(x.parts), func(i int) bool { return x.parts[i].span.To.After(at) })
	if a.part < len(x.parts) {
		a.cursor = maxTime(at, x.parts[a.part].span.From)
	}
	x.planDrawn(a)
}

// DrawnLeaves records that issue's stretch in flight ends at at: its accumulator takes its hours up
// to at, with each kill they bring.
func (x *ModelReader) DrawnLeaves(issue string, at time.Time) {
	a := x.drawn[issue]
	if a == nil || !a.open {
		return
	}
	for p := a.part; p < len(x.parts) && x.parts[p].span.From.Before(at); p++ {
		part := x.parts[p]
		from := maxTime(a.from, part.span.From)
		if p == a.part {
			from = a.cursor
		}
		a.sum = part.accrue(from, minTime(at, part.span.To), a.sum, x.placeKill)
	}
	a.open, a.next = false, time.Time{}
	x.drawnDue()
}

// planDrawn sets a's next kill, as its accumulator stands if its stretch in flight goes on, and the
// reader's earliest drawn kill due.
func (x *ModelReader) planDrawn(a *drawnAccrual) {
	a.next = time.Time{}
	if next, ok := x.nextDrawn(a); ok {
		a.next = next.cursor
	}
	x.drawnDue()
}

// nextDrawn is the accumulator a stands at after its next kill, if its stretch in flight goes on,
// and whether one comes before the span's last part ends.
func (x *ModelReader) nextDrawn(a *drawnAccrual) (drawnAccrual, bool) {
	sum := a.sum
	for p := a.part; p < len(x.parts); p++ {
		part := x.parts[p]
		from := maxTime(a.from, part.span.From)
		if p == a.part {
			from = a.cursor
		}
		if !from.Before(part.span.To) {
			continue
		}
		at, next, killed := part.step(from, part.span.To, sum)
		if !killed {
			sum = next
			continue
		}
		step := *a
		step.part, step.cursor, step.sum = p, at, next
		return step, true
	}
	return drawnAccrual{}, false
}

// drawnDue sets the earliest kill due of the drawn issues in flight.
func (x *ModelReader) drawnDue() {
	x.drawnNext = time.Time{}
	for _, issue := range x.drawnOrder {
		if a := x.drawn[issue]; a.open && !a.next.IsZero() && (x.drawnNext.IsZero() || a.next.Before(x.drawnNext)) {
			x.drawnNext = a.next
		}
	}
}

// placeKill places a modelled kill at at, in order.
func (x *ModelReader) placeKill(at time.Time) {
	if x.sources.Kills && !at.After(x.fold.through) {
		x.fold = setbackFold{}
	}
	insertTime(&x.kills, at)
}

// insertTime inserts at into times, which stay in order, after each moment equal to it.
func insertTime(times *[]time.Time, at time.Time) {
	i := sort.Search(len(*times), func(i int) bool { return (*times)[i].After(at) })
	*times = slices.Insert(*times, i, at)
}

// advance brings the reader to at: each minute that ended by at is modelled, counted among
// SuppliedMinutes where it takes the caller's token limit, each drawn kill due by at is placed, each
// pull request that entered by at is served, and each wait that can no longer count in the queue
// signal is let go.
func (x *ModelReader) advance(at time.Time) {
	if at.After(x.now) {
		x.now = at
	}
	for i := len(x.made); i < len(x.minutes) && !x.minuteStart(i).Add(time.Minute).After(x.now); i++ {
		minute, refuses := x.model(i)
		if refuses {
			x.refused++
		}
		if x.minutes[i].assumed {
			x.SuppliedMinutes++
		}
		x.made = append(x.made, minute)
	}
	for !x.drawnNext.IsZero() && !x.drawnNext.After(x.now) {
		for _, issue := range x.drawnOrder {
			a := x.drawn[issue]
			for a.open && !a.next.IsZero() && !a.next.After(x.now) {
				step, _ := x.nextDrawn(a)
				*a = step
				x.placeKill(a.cursor)
				x.planDrawn(a)
			}
		}
		x.drawnDue()
	}
	for _, qr := range x.queueOrder {
		served := 0
		for ; served < len(qr.pending) && !qr.pending[served].After(x.now); served++ {
			wait, ok := qr.service.serve(qr.q, qr.pending[served], x.r.queueEnd)
			if !ok {
				x.Unserved++
			}
			x.waits = append(x.waits, wait)
		}
		qr.pending = qr.pending[served:]
	}
	exitsFrom := x.now.Add(-signals.QueueExitSpan)
	x.waits = slices.DeleteFunc(x.waits, func(w signals.Wait) bool { return !w.Left.IsZero() && !w.Left.After(exitsFrom) })
}

// model is the minute at index i as the model gives it at the issues in flight as they stand,
// against the limit in force in it, and whether that limit refuses it (modelRecord.modelMinute). A
// minute the record does not hold is none.
func (x *ModelReader) model(i int) (signals.Minute, bool) {
	minute := x.minutes[i]
	if !minute.held {
		return signals.Minute{}, false
	}
	start := x.minuteStart(i)
	return x.r.modelMinute(minute.line, minute.limit, minute.limited, minute.recorded, x.flight.hours(start, start.Add(time.Minute)), x.refused)
}

// series is the provider's minutes as the definitions read them: each minute that ended by now as
// the model gave it, and a later one as the model gives it at the issues in flight now.
func (x *ModelReader) series(start time.Time) (signals.Minute, bool) {
	i := x.minuteIndex(start)
	if i < 0 || i >= len(x.minutes) || !x.minutes[i].held || !x.minuteStart(i).Equal(start) {
		return signals.Minute{}, false
	}
	if i < len(x.made) {
		return x.made[i], true
	}
	minute, _ := x.model(i)
	return minute, true
}

// Headroom is the provider's headroom signal at at (signals.ProviderAt): the modelled tokens of the
// minute before at over the project's own limit in force.
func (x *ModelReader) Headroom(at time.Time) signals.Value {
	x.advance(at)
	return signals.ProviderAt(x.series, at)
}

// Refusals is the provider's refusals at at (signals.RefusalsAt): the project's own as the model
// gives them, the provider's as recorded.
func (x *ModelReader) Refusals(at time.Time) signals.RefusalCount {
	x.advance(at)
	return signals.RefusalsAt(x.series, at)
}

// Kills is the kills signal at at (signals.KillsAt): the modelled kills of the hour before at.
func (x *ModelReader) Kills(at time.Time) signals.Value {
	x.advance(at)
	return signals.KillsAt(x.r.w.coverageOf(SignalKills, at), x.kills, at)
}

// Queue is the merge queue's signal at at, in hours (signals.QueueAt), over the modelled waits.
func (x *ModelReader) Queue(at time.Time) signals.Value {
	x.advance(at)
	return signals.QueueAt(x.r.w.coverageOf(SignalQueue, at), x.waits, at)
}

// KillsNext is the kills signal's next crossing of limit after at (signals.KillsNext), the kills
// standing as they do at at.
func (x *ModelReader) KillsNext(at time.Time, limit float64) (time.Time, bool) {
	x.advance(at)
	return signals.KillsNext(x.r.w.coverageOf(SignalKills, at), x.kills, at, limit)
}

// QueueNext is the queue signal's next crossing of limit after at (signals.QueueNext), the waits
// standing as they do at at.
func (x *ModelReader) QueueNext(at time.Time, limit float64) (time.Time, bool) {
	x.advance(at)
	return signals.QueueNext(x.r.w.coverageOf(SignalQueue, at), x.waits, at, limit)
}

// RefusalsNext is the refusals' next crossing of limit after at (signals.RefusalsNext), the
// refusals standing as they do at at, or a moment at which the record begins or stops holding the
// minute before.
func (x *ModelReader) RefusalsNext(at time.Time, limit float64) (time.Time, bool) {
	x.advance(at)
	i := sort.Search(len(x.covered), func(i int) bool { return x.covered[i].After(at) })
	return signals.RefusalsNext(x.series, at, limit, x.covered[i:])
}

// HeadroomNext is the headroom's next crossing of limit after at, as signals.ProviderNext gives it
// over the modelled minutes up to the boundary after the record's last minute, the issues in flight
// standing as they do at at. The minute under way is modelled whole. Each minute after it is read at
// its threshold of issues in flight for the limit (minuteThresholds), so the crossing is a search
// over the minutes ahead and no scan of them.
func (x *ModelReader) HeadroomNext(at time.Time, limit float64) (time.Time, bool) {
	x.advance(at)
	if len(x.minutes) == 0 {
		return time.Time{}, false
	}
	binding := signals.ProviderAt(x.series, at).Binds(limit)
	through := x.minuteStart(len(x.minutes) + 1)
	current := at.Truncate(time.Minute)
	boundary := current.Add(time.Minute)
	if boundary.After(through) {
		return time.Time{}, false
	}
	if signals.ProviderAt(x.series, boundary).Binds(limit) != binding {
		return boundary, true
	}
	from := max(x.minuteIndex(current)+1, 0)
	index := x.headroomThresholds(limit).first(from, !binding, x.inFlight())
	if index < 0 {
		if !binding {
			return time.Time{}, false
		}
		index = len(x.minutes)
	}
	if index > len(x.minutes) {
		return time.Time{}, false
	}
	return x.minuteStart(index).Add(time.Minute), true
}

// NextKill is the first modelled kill after after, of those placed and of the drawn issues in flight
// as they stand, and whether one comes.
func (x *ModelReader) NextKill(after time.Time) (time.Time, bool) {
	var next time.Time
	if i := sort.Search(len(x.kills), func(i int) bool { return x.kills[i].After(after) }); i < len(x.kills) {
		next = x.kills[i]
	}
	for _, issue := range x.drawnOrder {
		if a := x.drawn[issue]; a.open && a.next.After(after) && (next.IsZero() || a.next.Before(next)) {
			next = a.next
		}
	}
	return next, !next.IsZero()
}

// NextQueueChange is the first moment after after at which a modelled pull request enters or leaves
// the merge queue, as the queue stands, and whether one comes.
func (x *ModelReader) NextQueueChange(after time.Time) (time.Time, bool) {
	var next time.Time
	earliest := func(at time.Time) {
		if at.After(after) && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	for _, qr := range x.queues {
		for _, entry := range qr.pending {
			if entry.After(after) {
				earliest(entry)
				break
			}
		}
	}
	for _, wait := range x.waits {
		earliest(wait.Added)
		earliest(wait.Left)
	}
	return next, !next.IsZero()
}

// NextRefusal is the end of the first minute after after that may hold a refusal, the issues in
// flight standing as they do: one of the provider's, one of the project's own in a minute with no
// limit in force, or a minute whose modelled tokens pass the limit in force. It answers false where
// none comes. A minute's refusals are known at its end.
func (x *ModelReader) NextRefusal(after time.Time) (time.Time, bool) {
	x.advance(after)
	current := max(x.minuteIndex(after.Truncate(time.Minute)), len(x.made))
	if current < len(x.minutes) && current >= 0 {
		if minute, refuses := x.model(current); refuses || minute.Own+minute.Provider > 0 {
			return x.minuteStart(current).Add(time.Minute), true
		}
	}
	index := x.refusalThresholds().first(max(current+1, 0), true, x.inFlight())
	if index < 0 || index >= len(x.minutes) {
		return time.Time{}, false
	}
	return x.minuteStart(index).Add(time.Minute), true
}

// Setbacks is the setbacks the back-off folds at at, under ceiling, in time order (signals.Setback):
// the refusals by the project's own limit of each minute that ended by at, at the minute's end, and
// each modelled kill by at, each as SetbackSources lets them. It leaves out each setback before the
// last one ahead of which the allowance stood at the ceiling again, which the allowance no longer
// reads (signals.Allowance): the setbacks it answers fold to the same allowance as all of them.
func (x *ModelReader) Setbacks(at time.Time, ceiling int) []signals.Setback {
	x.advance(at)
	if !x.fold.begun || x.fold.ceiling != ceiling {
		x.fold = setbackFold{ceiling: ceiling, begun: true}
	}
	for _, setback := range x.setbacksIn(x.fold.through, at, !x.fold.through.IsZero()) {
		allowance := ceiling
		if len(x.fold.kept) > 0 {
			allowance = min(ceiling, x.fold.allowance+int(setback.At.Sub(x.fold.last)/time.Hour))
		}
		if allowance >= ceiling {
			x.fold.kept = nil
		}
		for i := int64(0); i < setback.Count && allowance > 1; i++ {
			allowance = max(allowance*9/10, 1)
		}
		x.fold.allowance, x.fold.last, x.fold.kept = allowance, setback.At, append(x.fold.kept, setback)
	}
	if at.After(x.fold.through) {
		x.fold.through = at
	}
	return slices.Clip(x.fold.kept)
}

// setbacksIn is each setback in (from, at], or [from, at] where open is false, in time order: a
// minute's own refusals at its end before a kill of the same moment.
func (x *ModelReader) setbacksIn(from, at time.Time, open bool) []signals.Setback {
	after := func(t time.Time) bool { return t.After(from) || !open && t.Equal(from) }
	var minutes, kills []signals.Setback
	if x.sources.Refusals && len(x.made) > 0 {
		begin := max(x.minuteIndex(from.Add(-time.Minute)), 0)
		for i := begin; i < len(x.made); i++ {
			end := x.minuteStart(i).Add(time.Minute)
			if end.After(at) {
				break
			}
			if own := x.made[i].Own; own > 0 && after(end) {
				minutes = append(minutes, signals.Setback{At: end, Count: own})
			}
		}
	}
	if x.sources.Kills {
		for i := sort.Search(len(x.kills), func(i int) bool { return !x.kills[i].Before(from) }); i < len(x.kills) && !x.kills[i].After(at); i++ {
			if after(x.kills[i]) {
				kills = append(kills, signals.Setback{At: x.kills[i], Count: 1})
			}
		}
	}
	merged := make([]signals.Setback, 0, len(minutes)+len(kills))
	for len(minutes) > 0 || len(kills) > 0 {
		if len(kills) == 0 || len(minutes) > 0 && !minutes[0].At.After(kills[0].At) {
			merged, minutes = append(merged, minutes[0]), minutes[1:]
			continue
		}
		merged, kills = append(merged, kills[0]), kills[1:]
	}
	return merged
}

// headroomThresholds is the threshold of each minute of the run for the headroom's limit: the
// fewest issues in flight at which the minute, modelled whole at that count, binds the limit
// (signals.ProviderAt, signals.Value.Binds).
func (x *ModelReader) headroomThresholds(limit float64) *minuteThresholds {
	if t, built := x.headroom[limit]; built {
		return t
	}
	t := x.thresholds(func(minute signals.Minute, _ bool) bool {
		return signals.ProviderAt(func(time.Time) (signals.Minute, bool) { return minute, true }, time.Time{}).Binds(limit)
	})
	x.headroom[limit] = t
	return t
}

// refusalThresholds is the threshold of each minute of the run for a refusal: the fewest issues in
// flight at which the minute, modelled whole at that count, may hold a refusal (NextRefusal), 0 for
// a minute whose recorded refusals the model keeps.
func (x *ModelReader) refusalThresholds() *minuteThresholds {
	if x.refusing == nil {
		x.refusing = x.thresholds(func(minute signals.Minute, refuses bool) bool { return refuses || minute.Own+minute.Provider > 0 })
	}
	return x.refusing
}

// thresholds is, for each minute of the run, the fewest issues in flight at which holds answers true
// of the minute modelled whole at that count, as a search tree (minuteThresholds). The modelled
// tokens do not fall as the count rises, and holds does not turn false as they rise, so each
// threshold is found by bisection. A minute that holds at no count up to minuteThreshold has none.
func (x *ModelReader) thresholds(holds func(signals.Minute, bool) bool) *minuteThresholds {
	at := func(i, issues int) bool {
		minute := x.minutes[i]
		if !minute.held {
			return false
		}
		modelled, refuses := x.r.modelMinute(minute.line, minute.limit, minute.limited, minute.recorded, float64(issues)*time.Minute.Hours(), 0)
		return holds(modelled, refuses)
	}
	counts := make([]int, len(x.minutes))
	for i := range x.minutes {
		counts[i] = math.MaxInt
		if !at(i, minuteThreshold) {
			continue
		}
		counts[i] = sort.Search(minuteThreshold, func(issues int) bool { return at(i, issues) })
	}
	return newMinuteThresholds(counts)
}

// minuteThresholds is a search tree over the threshold of each minute of a run of minutes: the
// least and the most threshold under each node, so that the first minute from an index on that
// holds at a count of issues in flight, or does not, is found in a walk down the tree.
type minuteThresholds struct {
	n        int
	low, top []int
}

// newMinuteThresholds is the search tree over counts.
func newMinuteThresholds(counts []int) *minuteThresholds {
	size := 1
	for size < len(counts) {
		size *= 2
	}
	t := &minuteThresholds{n: len(counts), low: make([]int, 2*size), top: make([]int, 2*size)}
	for i := range size {
		t.low[size+i], t.top[size+i] = math.MaxInt, math.MinInt
		if i < len(counts) {
			t.low[size+i], t.top[size+i] = counts[i], counts[i]
		}
	}
	for i := size - 1; i > 0; i-- {
		t.low[i], t.top[i] = min(t.low[2*i], t.low[2*i+1]), max(t.top[2*i], t.top[2*i+1])
	}
	return t
}

// first is the first index from from on whose minute holds at issues in flight where holds is true
// (its threshold at or below issues), or does not where it is false; -1 for none.
func (t *minuteThresholds) first(from int, holds bool, issues int) int {
	if from >= t.n {
		return -1
	}
	return t.find(1, 0, len(t.low)/2, from, holds, issues)
}

// find is first in the node over [lo, hi).
func (t *minuteThresholds) find(node, lo, hi, from int, holds bool, issues int) int {
	if hi <= from || holds && t.low[node] > issues || !holds && t.top[node] <= issues {
		return -1
	}
	if hi-lo == 1 {
		if lo >= t.n {
			return -1
		}
		return lo
	}
	mid := (lo + hi) / 2
	if found := t.find(2*node, lo, mid, from, holds, issues); found >= 0 {
		return found
	}
	return t.find(2*node+1, mid, hi, from, holds, issues)
}
