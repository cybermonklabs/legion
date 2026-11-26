package replay

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// The load models' record of a window and the signals they give a replay (spec R11): what the
// three models read of the window's record (modelRecord), and, from a fitted model and a replay's
// own timeline (ReplayTimeline), each modelled signal at a moment (ModelledSignals), read by
// package signals' definitions as the window's own record is (signals.go), so a modelled signal is
// the same quantity as the recorded one: the provider's headroom and the refusals by the project's
// own limit, the kills of the hour before, and the merge queue's wait. At the record's own timeline
// (LoadModels.RecordTimeline) the models of the kills and of the merge queue give the record by
// their form, which R11's daily test reads (FitLoadModels). The replay keeps each work segment's
// recorded length (assumption 1), so a kill or a pull request keeps its offset in its segment.

// ReplayTimeline is a replay's own course, as the load models read it: the issues in flight in the
// replay from each moment on, in time order, every issue counted and drawn work among them
// (InFlight); each stretch in which an issue whose work the record never ran, a drawn scenario's,
// stands in flight (Drawn); the replay's start of each recorded work segment that it ran (Segments),
// a segment it did not run being absent; and each pull request of work the record never ran
// entering the merge queue (Entries). A recorded pull request enters where the start of its
// segment puts it, which the merge queue's model places itself (queueForm).
type ReplayTimeline struct {
	InFlight []FlightStep
	Drawn    []DrawnFlight
	Segments []SegmentStart
	Entries  []QueueEntry
}

// FlightStep is the issues in flight from At on, up to the next step; none before the first.
type FlightStep struct {
	At     time.Time
	Issues int
}

// DrawnFlight is one stretch [From, To) in which Issue, whose work the record never ran, stands in
// flight in a replay; To zero while it stands at the replay's end.
type DrawnFlight struct {
	Issue    string
	From, To time.Time
}

// SegmentStart is one recorded work segment of Issue, by the moment the record started it
// (WorkSegment.Start), and the moment the replay started its run of it.
type SegmentStart struct {
	Issue              string
	Recorded, Replayed time.Time
}

// QueueEntry is a pull request of work the record never ran entering the merge queue of Repository
// (owner/name, compared without regard to case) at At in a replay.
type QueueEntry struct {
	Repository string
	At         time.Time
}

// ModelledSignals is the three modelled signals of one replay (LoadModels.Replay), each read at a
// moment as the window's own record is read, held where the window holds the signal's record. It
// counts the kills the record could not place in a segment of their issue, kept at their recorded
// times; the pull requests the record shows in no segment of an issue, entering at their recorded
// times; and the pull requests that no recorded exit and no rate serves, which stand in the queue.
type ModelledSignals struct {
	KillsApart   int
	EntriesApart int
	Unserved     int

	w Window
	// minutes is each modelled minute of the provider, by its Unix time, and refused each of them
	// the project's own limit refuses in the model.
	minutes map[int64]signals.Minute
	refused map[int64]bool
	// kills is the moment of each modelled kill, in order; waits each modelled wait in the merge
	// queue, by the moment it began.
	kills []time.Time
	waits []signals.Wait
}

// Headroom is the provider's headroom signal at at, as the model gives the minute before at
// (signals.ProviderAt): its modelled tokens over the project's own limit in force.
func (s *ModelledSignals) Headroom(at time.Time) signals.Value {
	return signals.ProviderAt(s.series, at)
}

// Refusals is the provider's refusals at at (signals.RefusalsAt): the project's own as the model
// gives them, the provider's as recorded.
func (s *ModelledSignals) Refusals(at time.Time) signals.RefusalCount {
	return signals.RefusalsAt(s.series, at)
}

// Kills is the kills signal at at (signals.KillsAt): the modelled kills of the hour before at.
func (s *ModelledSignals) Kills(at time.Time) signals.Value {
	return signals.KillsAt(s.held(SignalKills), s.kills, at)
}

// Queue is the merge queue's signal at at, in hours (signals.QueueAt), over the modelled waits.
func (s *ModelledSignals) Queue(at time.Time) signals.Value {
	return signals.QueueAt(s.held(SignalQueue), s.waits, at)
}

// series is the modelled minute of the provider that starts at start, and whether the record holds
// that minute.
func (s *ModelledSignals) series(start time.Time) (signals.Minute, bool) {
	minute, held := s.minutes[start.Unix()]
	return minute, held
}

// held is where the window's record holds signal.
func (s *ModelledSignals) held(signal string) signals.Coverage {
	return signals.Coverage{Held: func(at time.Time) bool { return s.w.Recorded(signal, at) }}
}

// RecordTimeline is the record's own timeline: its issues in flight as the scorecard counts them,
// each recorded work segment started where the record started it, and no drawn work. The models at
// it give the record's own course, which R11's daily test reads. It refuses models that hold no
// window's record (a value read back from JSON).
func (m LoadModels) RecordTimeline() (ReplayTimeline, error) {
	if m.fit == nil {
		return ReplayTimeline{}, errNoModelRecord
	}
	return m.fit.recordTimeline(), nil
}

// errNoModelRecord is the refusal of load models that hold no window's record.
var errNoModelRecord = errors.New("replay: the load models hold no window's record; fit them on the window with FitLoadModels")

// Replay is each modelled signal of the replay whose timeline is tl (tokensForm, memoryForm,
// queueForm). It refuses models that hold no window's record, steps of issues in flight out of
// time order or below zero, and a drawn stretch that ends before it begins.
func (m LoadModels) Replay(tl ReplayTimeline) (*ModelledSignals, error) {
	r := m.fit
	if r == nil {
		return nil, errNoModelRecord
	}
	flight, err := flightCount(tl.InFlight)
	if err != nil {
		return nil, err
	}
	for _, stretch := range tl.Drawn {
		if !stretch.To.IsZero() && stretch.To.Before(stretch.From) {
			return nil, fmt.Errorf("replay: the drawn work of %s stands in flight from %s to %s, which ends before it begins", stretch.Issue,
				stamp(stretch.From), stamp(stretch.To))
		}
	}
	starts := make(map[segmentKey]time.Time, len(tl.Segments))
	for _, start := range tl.Segments {
		starts[segmentKey{issue: start.Issue, start: start.Recorded.UnixNano()}] = start.Replayed
	}
	s := &ModelledSignals{w: r.w}
	s.minutes, s.refused = r.replayTokens(flight)
	s.kills, s.KillsApart = r.replayKills(starts, tl.Drawn)
	s.waits, s.EntriesApart, s.Unserved = r.replayQueues(starts, tl.Entries)
	return s, nil
}

// flightCount is steps as a count: how many issues stand in flight from each step's moment on.
func flightCount(steps []FlightStep) (stretchCount, error) {
	var count stretchCount
	for i, step := range steps {
		switch {
		case step.Issues < 0:
			return stretchCount{}, fmt.Errorf("replay: the replay's issues in flight at %s are %d, below zero", stamp(step.At), step.Issues)
		case i > 0 && step.At.Before(steps[i-1].At):
			return stretchCount{}, fmt.Errorf("replay: the replay's issues in flight at %s come after those at %s", stamp(step.At), stamp(steps[i-1].At))
		case i > 0 && step.At.Equal(steps[i-1].At):
			count.counts[len(count.counts)-1] = step.Issues
		default:
			count.from, count.counts = append(count.from, step.At), append(count.counts, step.Issues)
		}
	}
	return count, nil
}

// modelRecord is what the load models read of a window's record (newModelRecord): the window, its
// issues in flight as the scorecard counts them, and the days of its span; each minute of the
// provider's record, the refused minutes of its span and the record's own refusals per refused
// minute; each kill of the memory guard with the work segment it belongs to, and each day's kills
// and issue-hours in flight where the window holds the record of the kills; each recorded work
// segment of the project's issues; and each repository's merge queue, with the end of the queue's
// record.
type modelRecord struct {
	w      Window
	flight stretchCount
	days   []Span

	minutes        map[int64]ProviderMinute
	refusedMinutes int
	rho            float64

	kills    []recordedKill
	dayRates []dayRate

	segments []segmentSpan
	queues   []*recordedQueue
	queueEnd time.Time
}

// segmentKey is a recorded work segment by its issue and the Unix nanosecond of its start.
type segmentKey struct {
	issue string
	start int64
}

// segmentSpan is a recorded work segment of issue: from its start to its end, zero where the
// record shows it unended.
type segmentSpan struct {
	issue      string
	start, end time.Time
}

// key is s's segmentKey.
func (s segmentSpan) key() segmentKey { return segmentKey{issue: s.issue, start: s.start.UnixNano()} }

// holds says whether at is in s.
func (s segmentSpan) holds(at time.Time) bool {
	return !at.Before(s.start) && (s.end.IsZero() || at.Before(s.end))
}

// recordedKill is one kill of the memory guard the launcher's lines record, and the work segment of
// its issue it lies in, where placed says it lies in one.
type recordedKill struct {
	at      time.Time
	segment segmentSpan
	placed  bool
}

// dayRate is one day of the window's span as the kill model reads it: the day, the parts of it in
// which the window holds the record of the kills, and the kills and the issue-hours in flight
// there.
type dayRate struct {
	span  Span
	parts []Span
	kills int
	hours float64
}

// rate is the day's kills for each hour of an issue in flight, and whether it has one: none where
// no issue was in flight in its record.
func (d dayRate) rate() (float64, bool) {
	if d.hours <= 0 {
		return 0, false
	}
	return float64(d.kills) / d.hours, true
}

// newModelRecord reads what the load models read of w's record: its trace t, its issues in flight
// flight, the kills of its launcher's lines and the events of its queue files.
func newModelRecord(w Window, t Trace, flight stretchCount, kills []GuardKill, events []signals.QueueEvent) *modelRecord {
	r := &modelRecord{w: w, flight: flight, days: windowDays(w.From, w.To), minutes: make(map[int64]ProviderMinute, len(w.Provider)), rho: 1}
	for _, line := range w.Provider {
		r.minutes[line.Minute.Unix()] = line
	}
	var own int64
	for start := w.From.Truncate(time.Minute); start.Before(w.To); start = start.Add(time.Minute) {
		if line, held := r.minutes[start.Unix()]; held && line.Own > 0 {
			own, r.refusedMinutes = own+line.Own, r.refusedMinutes+1
		}
	}
	if r.refusedMinutes > 0 {
		r.rho = float64(own) / float64(r.refusedMinutes)
	}
	segments := issueSegments(t)
	for _, key := range slices.Sorted(maps.Keys(segments)) {
		r.segments = append(r.segments, segments[key]...)
	}
	pods := issuePods(t)
	for _, kill := range kills {
		segment, placed := segmentAt(segments[pods[kill.Pod]], kill.At)
		r.kills = append(r.kills, recordedKill{at: kill.At, segment: segment, placed: placed})
	}
	recorded, _ := r.killMoments()
	record := clipSpans(w.Signals[SignalKills], Span{From: w.From, To: w.To})
	for _, span := range r.days {
		day := dayRate{span: span, parts: clipSpans(record, span)}
		for _, part := range day.parts {
			day.kills += countWithin(recorded, part)
			day.hours += flight.hours(part.From, part.To)
		}
		r.dayRates = append(r.dayRates, day)
	}
	r.queues = queuePulls(events, pullIssues(t), segments)
	for _, span := range w.Signals[SignalQueue] {
		r.queueEnd = maxTime(r.queueEnd, span.To)
	}
	queueRecord := clipSpans(w.Signals[SignalQueue], Span{From: w.From, To: w.To})
	for _, q := range r.queues {
		q.fit(queueRecord)
	}
	return r
}

// recordTimeline is the record's own timeline (LoadModels.RecordTimeline).
func (r *modelRecord) recordTimeline() ReplayTimeline {
	tl := ReplayTimeline{InFlight: make([]FlightStep, len(r.flight.from)), Segments: make([]SegmentStart, 0, len(r.segments))}
	for i, at := range r.flight.from {
		tl.InFlight[i] = FlightStep{At: at, Issues: r.flight.counts[i]}
	}
	for _, segment := range r.segments {
		tl.Segments = append(tl.Segments, SegmentStart{Issue: segment.issue, Recorded: segment.start, Replayed: segment.start})
	}
	return tl
}

// headroomLimit is the project's own limit in force in line's minute where the window holds a
// record of the provider's headroom there, as providerAt reads it, and whether it does.
func (r *modelRecord) headroomLimit(line ProviderMinute) (int64, bool) {
	limit, ok := line.projectLimit()
	return limit, ok && r.w.Recorded(SignalProvider, line.Minute)
}

// killMoments is the moment of each recorded kill, and of each the record cannot place in a
// segment of its issue, each in time order.
func (r *modelRecord) killMoments() (recorded, apart []time.Time) {
	for _, kill := range r.kills {
		recorded = append(recorded, kill.at)
		if !kill.placed {
			apart = append(apart, kill.at)
		}
	}
	return recorded, apart
}

// issueSegments is each recorded work segment of each issue of t's project, by its issue, in the
// order of its visits: each visit's segment, ended at its end or, where the record gives none, at
// the visit's exit, zero for a visit that stands at the window's end.
func issueSegments(t Trace) map[string][]segmentSpan {
	segments := make(map[string][]segmentSpan)
	for key, iss := range t.Issues {
		if iss.Project != t.Project {
			continue
		}
		for _, visit := range iss.Visits {
			if visit.Segment == nil {
				continue
			}
			end := visit.Segment.End
			if end.IsZero() {
				end = visit.Exit
			}
			segments[key] = append(segments[key], segmentSpan{issue: key, start: visit.Segment.Start, end: end})
		}
	}
	return segments
}

// segmentAt is the first of segments that holds at, and whether one does.
func segmentAt(segments []segmentSpan, at time.Time) (segmentSpan, bool) {
	for _, segment := range segments {
		if segment.holds(at) {
			return segment, true
		}
	}
	return segmentSpan{}, false
}

// issuePods is each issue of t's project by the name of its pod, as the launcher names it,
// `legion-<project>-<issue>` (GuardKill): the part of the issue's claim tokens before their role.
func issuePods(t Trace) map[string]string {
	pods := make(map[string]string)
	project, err := claim.ProjectToken(t.Project)
	if err != nil {
		return pods
	}
	for key, iss := range t.Issues {
		if iss.Project != t.Project {
			continue
		}
		token, err := claim.NewToken(project, key, claim.RoleArchitect)
		if err != nil {
			continue
		}
		if pod, _, ok := token.Cut(); ok {
			pods[pod] = key
		}
	}
	return pods
}

// replayTokens is each minute of the provider's record as the tokens model gives it at the
// replay's issues in flight (tokensForm), by its Unix time, and each such minute the project's own
// limit refuses. A minute's issues in flight, in the record and in the replay, are their average
// over the whole minute. A refused minute counts the record's own refusals per refused minute,
// rounded so that the first n refused minutes count round(n × rho) together; a minute with no
// record of the headroom keeps its recorded refusals, since the model has no limit to read there.
func (r *modelRecord) replayTokens(flight stretchCount) (map[int64]signals.Minute, map[int64]bool) {
	minutes := make(map[int64]signals.Minute, len(r.minutes))
	refused := make(map[int64]bool)
	count := 0
	for _, key := range slices.Sorted(maps.Keys(r.minutes)) {
		line := r.minutes[key]
		start, end := line.Minute, line.Minute.Add(time.Minute)
		tokens := float64(line.Uncached)
		if recorded := r.flight.hours(start, end); recorded > 0 {
			tokens *= flight.hours(start, end) / recorded
		}
		minute := signals.Minute{Uncached: int64(math.Round(tokens)), Own: line.Own, Provider: line.Provider}
		if limit, limited := r.headroomLimit(line); limited {
			minute.Limit, minute.Own = limit, 0
			if tokens > float64(limit) {
				count++
				minute.Own = int64(math.Round(float64(count)*r.rho)) - int64(math.Round(float64(count-1)*r.rho))
				refused[key] = true
			}
		}
		minutes[key] = minute
	}
	return minutes, refused
}

// replayKills is each modelled kill of the replay, in time order (memoryForm), and how many the
// record cannot place: each recorded kill at its offset in the replay's run of its segment, none
// where the replay did not run it; each kill that lies in no segment of its issue at its recorded
// time; and each drawn issue's kills at each day's rate.
func (r *modelRecord) replayKills(starts map[segmentKey]time.Time, drawn []DrawnFlight) ([]time.Time, int) {
	var kills []time.Time
	apart := 0
	for _, kill := range r.kills {
		if !kill.placed {
			kills, apart = append(kills, kill.at), apart+1
			continue
		}
		if start, ran := starts[kill.segment.key()]; ran {
			kills = append(kills, start.Add(kill.at.Sub(kill.segment.start)))
		}
	}
	byIssue := make(map[string][]DrawnFlight)
	for _, stretch := range drawn {
		byIssue[stretch.Issue] = append(byIssue[stretch.Issue], stretch)
	}
	for _, issue := range slices.Sorted(maps.Keys(byIssue)) {
		stretches := byIssue[issue]
		slices.SortStableFunc(stretches, func(a, b DrawnFlight) int { return a.From.Compare(b.From) })
		kills = append(kills, r.drawnKills(stretches)...)
	}
	slices.SortFunc(kills, time.Time.Compare)
	return kills, apart
}

// drawnKills is the kills of one drawn issue in flight over stretches, in order: its issue-hours
// in flight, each times the rate of its day where the window holds the record of the kills, add up
// in an accumulator of the issue's own, and it has a kill at each moment the sum passes a whole
// number. A stretch that stands at the replay's end is read to the window's end, the last day with
// a rate.
func (r *modelRecord) drawnKills(stretches []DrawnFlight) []time.Time {
	var kills []time.Time
	sum := 0.0
	for _, stretch := range stretches {
		to := stretch.To
		if to.IsZero() {
			to = r.w.To
		}
		for _, day := range r.dayRates {
			rate, rated := day.rate()
			if !rated || rate <= 0 {
				continue
			}
			for _, part := range day.parts {
				from, until := maxTime(stretch.From, part.From), minTime(to, part.To)
				for from.Before(until) {
					whole := math.Floor(sum) + 1
					need := (whole - sum) / rate
					if left := until.Sub(from).Hours(); need > left {
						sum += rate * left
						break
					}
					from = from.Add(time.Duration(need * float64(time.Hour)))
					kills, sum = append(kills, from), whole
				}
			}
		}
	}
	return kills
}

// recordedQueue is one repository's merge queue as the record shows it: its name in lower case and
// as the record names it; each wait of its pull requests, in the order of entry; each exit from
// its head, in order; each span in which the record's queue held a pull request, in order, To zero
// for one that stands at the record's end; and, over the window's span where it holds the queue's
// record, its exits from the head, its pull requests removed before the head and the hours in which
// the queue held a pull request, with the rate of the first over the last, rated where that has one.
type recordedQueue struct {
	key, repo  string
	pulls      []recordedPull
	exits      []time.Time
	busy       []Span
	headExits  int
	earlyExits int
	busyHours  float64
	rate       float64
	rated      bool
}

// recordedPull is one wait of a pull request in its repository's queue: its number, its entry and
// its exit (zero while it stands at the record's end), whether the record removed it before it
// reached the head, and the work segment of its issue that holds its entry, where placed says one
// does.
type recordedPull struct {
	number      int
	added, left time.Time
	early       bool
	segment     segmentSpan
	placed      bool
}

// pullRef is a pull request by its repository, owner/name in lower case, and its number.
type pullRef struct {
	repo   string
	number int
}

// pullIssues is each issue of t's project by each pull request it links (IssueTrace's
// LinkedPullRequests, and the number a line names on its own repository), in the order of their
// keys.
func pullIssues(t Trace) map[pullRef][]string {
	issues := make(map[pullRef][]string)
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		iss := t.Issues[key]
		if iss.Project != t.Project {
			continue
		}
		refs := make(map[pullRef]bool)
		for repo, number := range iss.LinkedPullRequests {
			refs[pullRef{repo: strings.ToLower(repo), number: number}] = true
		}
		if iss.PullRequestNumber > 0 && iss.Repo != "" {
			refs[pullRef{repo: strings.ToLower(iss.Repo), number: iss.PullRequestNumber}] = true
		}
		for ref := range refs {
			issues[ref] = append(issues[ref], key)
		}
	}
	return issues
}

// queuePulls is each repository's queue (recordedQueue) from the queue files' events, in the order
// of its name: each pull request's waits as package signals pairs them (signals.WaitsOf, each event
// id once, at the first line that carries it), so they are the queue signal's own; each wait
// placed in the first segment of an issue linking its pull request that holds its entry. A wait is
// one the record removed before it reached the head where it left while a pull request that stood
// in the queue at its entry, having entered at or before it, still stood.
func queuePulls(events []signals.QueueEvent, issues map[pullRef][]string, segments map[string][]segmentSpan) []*recordedQueue {
	type pull struct {
		repo   string
		number int
	}
	seen := make(map[int64]bool)
	histories := make(map[pull][]signals.QueueEvent)
	for _, e := range events {
		if e.ID > 0 {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
		}
		histories[pull{repo: e.Repo, number: e.PullRequest}] = append(histories[pull{repo: e.Repo, number: e.PullRequest}], e)
	}
	queues := make(map[string]*recordedQueue)
	for _, p := range slices.SortedFunc(maps.Keys(histories), func(a, b pull) int {
		if c := strings.Compare(a.repo, b.repo); c != 0 {
			return c
		}
		return a.number - b.number
	}) {
		ref := pullRef{repo: strings.ToLower(p.repo), number: p.number}
		q := queues[ref.repo]
		if q == nil {
			q = &recordedQueue{key: ref.repo, repo: p.repo}
			queues[ref.repo] = q
		}
		for _, wait := range signals.WaitsOf(histories[p]) {
			entry := recordedPull{number: p.number, added: wait.Added, left: wait.Left}
			for _, key := range issues[ref] {
				if entry.segment, entry.placed = segmentAt(segments[key], wait.Added); entry.placed {
					break
				}
			}
			q.pulls = append(q.pulls, entry)
		}
	}
	var ordered []*recordedQueue
	for _, key := range slices.Sorted(maps.Keys(queues)) {
		q := queues[key]
		slices.SortStableFunc(q.pulls, func(a, b recordedPull) int { return a.added.Compare(b.added) })
		q.classify()
		ordered = append(ordered, q)
	}
	return ordered
}

// classify marks each of q's waits the record removed before it reached the head (queuePulls),
// and reads q's exits from the head and the spans in which its queue held a pull request.
func (q *recordedQueue) classify() {
	for i := range q.pulls {
		x := &q.pulls[i]
		if x.left.IsZero() {
			continue
		}
		for j, y := range q.pulls {
			if j == i || y.added.After(x.added) || !y.left.IsZero() && !y.left.After(x.added) {
				continue
			}
			if y.left.IsZero() || y.left.After(x.left) {
				x.early = true
				break
			}
		}
	}
	for _, pull := range q.pulls {
		if !pull.early && !pull.left.IsZero() {
			q.exits = append(q.exits, pull.left)
		}
		n := len(q.busy)
		switch {
		case n > 0 && (q.busy[n-1].To.IsZero() || !pull.added.After(q.busy[n-1].To)):
			if !q.busy[n-1].To.IsZero() && (pull.left.IsZero() || pull.left.After(q.busy[n-1].To)) {
				q.busy[n-1].To = pull.left
			}
		default:
			q.busy = append(q.busy, Span{From: pull.added, To: pull.left})
		}
	}
	slices.SortFunc(q.exits, time.Time.Compare)
}

// fit reads q's exits from the head, its waits removed before the head and its hours that held a
// pull request over record, the parts of the window's span in which it holds the queue's record,
// and its rate of exits from the head for each such hour.
func (q *recordedQueue) fit(record []Span) {
	for _, pull := range q.pulls {
		if pull.left.IsZero() || !slices.ContainsFunc(record, func(span Span) bool { return span.Holds(pull.left) }) {
			continue
		}
		if pull.early {
			q.earlyExits++
		} else {
			q.headExits++
		}
	}
	for _, part := range record {
		for _, busy := range q.busy {
			to := busy.To
			if to.IsZero() {
				to = part.To
			}
			if from, until := maxTime(busy.From, part.From), minTime(to, part.To); until.After(from) {
				q.busyHours += until.Sub(from).Hours()
			}
		}
	}
	if q.headExits > 0 && q.busyHours > 0 {
		q.rate, q.rated = float64(q.headExits)/q.busyHours, true
	}
}

// parameters is q's parameters as the merge queue's model prints them.
func (q *recordedQueue) parameters() []ModelParameter {
	parameters := []ModelParameter{{Name: paramHeadExits, Repository: q.repo, Value: float64(q.headExits)},
		{Name: paramEarlyExits, Repository: q.repo, Value: float64(q.earlyExits)}, {Name: paramBusyHours, Repository: q.repo, Value: q.busyHours}}
	if q.rated {
		parameters = append(parameters, ModelParameter{Name: paramExitRate, Repository: q.repo, Value: q.rate})
	}
	return parameters
}

// replayQueues is each modelled wait in the merge queue of the replay, by the moment it began
// (queueForm), each repository's queue on its own; how many recorded pull requests the record shows
// in no segment of an issue; and how many pull requests stand unserved. A drawn pull request of a
// repository whose queue the record does not hold has no exit and no rate, and stands.
func (r *modelRecord) replayQueues(starts map[segmentKey]time.Time, entries []QueueEntry) ([]signals.Wait, int, int) {
	drawn := make(map[string][]time.Time)
	for _, entry := range entries {
		key := strings.ToLower(entry.Repository)
		drawn[key] = append(drawn[key], entry.At)
	}
	var waits []signals.Wait
	apart, unserved := 0, 0
	for _, q := range r.queues {
		queued, qApart, qUnserved := q.replay(starts, drawn[q.key], r.queueEnd)
		waits, apart, unserved = append(waits, queued...), apart+qApart, unserved+qUnserved
		delete(drawn, q.key)
	}
	for _, key := range slices.Sorted(maps.Keys(drawn)) {
		for _, at := range drawn[key] {
			waits, unserved = append(waits, signals.Wait{Added: at}), unserved+1
		}
	}
	slices.SortStableFunc(waits, func(a, b signals.Wait) int { return a.Added.Compare(b.Added) })
	return waits, apart, unserved
}

// replay is the waits of q's pull requests in a replay that started each recorded segment at
// starts and adds the drawn entries, with how many of its recorded pull requests lie in no segment
// of an issue and how many stand unserved. A recorded pull request enters at its offset in its
// segment, and not at all where the replay did not run that segment, or at its recorded entry
// where it lies in none. One the record removed before the head keeps its recorded wait. Every
// other one leaves in its order of entry, after the one before it: at the record's first exit from
// the head after both that no one before it took, or by the queue's own rate where that comes
// first (service); one that neither serves stands in the queue, and so does each after it.
func (q *recordedQueue) replay(starts map[segmentKey]time.Time, drawn []time.Time, end time.Time) ([]signals.Wait, int, int) {
	var waits []signals.Wait
	var head []time.Time
	apart := 0
	for _, pull := range q.pulls {
		entry := pull.added
		if pull.placed {
			start, ran := starts[pull.segment.key()]
			if !ran {
				continue
			}
			entry = start.Add(pull.added.Sub(pull.segment.start))
		} else {
			apart++
		}
		if pull.early {
			waits = append(waits, signals.Wait{Added: entry, Left: entry.Add(pull.left.Sub(pull.added))})
			continue
		}
		head = append(head, entry)
	}
	head = append(head, drawn...)
	slices.SortStableFunc(head, time.Time.Compare)
	used, unserved := 0, 0
	var prev time.Time
	stalled := false
	for _, entry := range head {
		if stalled {
			waits, unserved = append(waits, signals.Wait{Added: entry}), unserved+1
			continue
		}
		ready := maxTime(entry, prev)
		next := max(used, sort.Search(len(q.exits), func(i int) bool { return !q.exits[i].Before(ready) }))
		served, by := q.service(ready, end)
		switch {
		case next < len(q.exits) && (!served || !by.Before(q.exits[next])):
			prev, used = q.exits[next], next+1
		case served:
			prev = by
		default:
			stalled = true
			waits, unserved = append(waits, signals.Wait{Added: entry}), unserved+1
			continue
		}
		waits = append(waits, signals.Wait{Added: entry, Left: prev})
	}
	return waits, apart, unserved
}

// service is the moment a pull request that reaches the head of q's queue at ready leaves by the
// queue's own rate, and whether the rate serves it: it is served from ready where the record's
// queue stood empty then or ready is at or after end, the end of the queue's record; otherwise from
// the end of the record's busy span holding ready, or from end where that comes first; and it leaves
// one rate's interval after that. A queue with no rate serves nothing, and one whose record has no
// end serves nothing while its record's queue stands to the end.
func (q *recordedQueue) service(ready, end time.Time) (bool, time.Time) {
	if !q.rated {
		return false, time.Time{}
	}
	from := ready
	if end.IsZero() || ready.Before(end) {
		i := sort.Search(len(q.busy), func(i int) bool { return q.busy[i].From.After(ready) }) - 1
		if i >= 0 && (q.busy[i].To.IsZero() || ready.Before(q.busy[i].To)) {
			switch to := q.busy[i].To; {
			case to.IsZero() && end.IsZero():
				return false, time.Time{}
			case to.IsZero() || !end.IsZero() && to.After(end):
				from = end
			default:
				from = to
			}
		}
	}
	return true, from.Add(time.Duration(float64(time.Hour) / q.rate))
}
