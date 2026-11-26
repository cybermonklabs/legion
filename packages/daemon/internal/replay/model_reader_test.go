package replay

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/signals"
)

// courseEvent is one change of a replay's course as a ModelReader is fed it, at its moment.
type courseEvent struct {
	at   time.Time
	feed func(*ModelReader) error
}

// courseOf is tl as the events a run feeds a reader, in time order: each step of the issues in
// flight, each segment's start at its replayed start, each drawn stretch's entry and leave, and each
// drawn pull request's entry at its moment.
func courseOf(tl ReplayTimeline) []courseEvent {
	var events []courseEvent
	for _, step := range tl.InFlight {
		events = append(events, courseEvent{at: step.At, feed: func(x *ModelReader) error { return x.Flight(step.At, step.Issues) }})
	}
	for _, start := range tl.Segments {
		events = append(events, courseEvent{at: start.Replayed, feed: func(x *ModelReader) error {
			x.Segment(start.Issue, start.Recorded, start.Replayed)
			return nil
		}})
	}
	for _, stretch := range tl.Drawn {
		events = append(events, courseEvent{at: stretch.From, feed: func(x *ModelReader) error {
			x.DrawnEnters(stretch.Issue, stretch.From)
			return nil
		}})
		if !stretch.To.IsZero() {
			events = append(events, courseEvent{at: stretch.To, feed: func(x *ModelReader) error {
				x.DrawnLeaves(stretch.Issue, stretch.To)
				return nil
			}})
		}
	}
	for _, entry := range tl.Entries {
		events = append(events, courseEvent{at: entry.At, feed: func(x *ModelReader) error {
			x.Entry(entry.Repository, entry.At)
			return nil
		}})
	}
	slices.SortStableFunc(events, func(a, b courseEvent) int { return a.at.Compare(b.at) })
	return events
}

// frozenAt is tl as it stands at at, if no new event came: its steps of the issues in flight, its
// segment starts and its entries up to at, and each drawn stretch begun by at, standing to the end
// where it had not ended by at.
func frozenAt(tl ReplayTimeline, at time.Time) ReplayTimeline {
	var frozen ReplayTimeline
	for _, step := range tl.InFlight {
		if !step.At.After(at) {
			frozen.InFlight = append(frozen.InFlight, step)
		}
	}
	for _, start := range tl.Segments {
		if !start.Replayed.After(at) {
			frozen.Segments = append(frozen.Segments, start)
		}
	}
	for _, stretch := range tl.Drawn {
		if stretch.From.After(at) {
			continue
		}
		if stretch.To.After(at) {
			stretch.To = time.Time{}
		}
		frozen.Drawn = append(frozen.Drawn, stretch)
	}
	for _, entry := range tl.Entries {
		if !entry.At.After(at) {
			frozen.Entries = append(frozen.Entries, entry)
		}
	}
	return frozen
}

// sameValue says whether a and b are one signal's value: both unrecorded, or both recorded at one
// value.
func sameValue(a, b signals.Value) bool {
	return a.Recorded == b.Recorded && (!a.Recorded || a.Value == b.Value)
}

// sameCrossing says whether two next crossings are one: both none, or both due at one moment.
func sameCrossing(a time.Time, aDue bool, b time.Time, bDue bool) bool {
	return aDue == bDue && (!aDue || a.Equal(b))
}

// The load models read as a replay runs give, at each moment the run has reached, what they give
// over the replay's whole timeline (ModelReader, LoadModels.Replay), so a reading at a pass and a
// crossing a run computes there rest on nothing the run has not reached, and no value the reader
// gave changes later. It holds on three windows of the models' own tests: tokensWindow, its replay
// standing at 4 issues in flight from its start and at 1 from 01:30; killWorkWindow, its ACME-1
// segment an hour later and two drawn issues in flight; and queueWorkWindow, its ACME-1 segment an
// hour later and two drawn pull requests. The course is fed in time order, and at every five minutes
// of each window, and at each moment the course changes, the reader's headroom, refusals, kills and
// merge queue equal the whole timeline's, and so does each next crossing: of the kills, the queue
// and the refusals over the whole timeline, which those crossings read only up to the moment, and of
// the headroom over the timeline as it stands at that moment, whose minutes ahead are read at the
// issues in flight then. At the end, the reader's kills and its counts equal the whole timeline's.
func TestTheModelReaderGivesWhatTheWholeTimelineGives(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		window func(*testing.T) Window
		course func(*testing.T, LoadModels) ReplayTimeline
		limits []float64
	}{
		{"tokens", tokensWindow, func(*testing.T, LoadModels) ReplayTimeline {
			return ReplayTimeline{InFlight: []FlightStep{{At: minuteOf(0), Issues: 4}, {At: minuteOf(90), Issues: 1}, {At: minuteOf(125), Issues: 3}}}
		}, []float64{0.2, 0.9, 1.0, 1.2}},
		{"kills", killWorkWindow, func(t *testing.T, models LoadModels) ReplayTimeline {
			tl := segmentMoved(t, recordTimelineOf(t, models), "ACME-1", minuteOf(0), minuteOf(60))
			tl.Drawn = []DrawnFlight{{Issue: "ACME-7", From: minuteOf(120), To: minuteOf(330)}, {Issue: "ACME-8", From: minuteOf(180), To: minuteOf(240)},
				{Issue: "ACME-8", From: minuteOf(270)}}
			return tl
		}, []float64{1, 2}},
		{"queue", queueWorkWindow, func(t *testing.T, models LoadModels) ReplayTimeline {
			tl := segmentMoved(t, recordTimelineOf(t, models), "ACME-1", minuteOf(0), minuteOf(60))
			tl.Entries = []QueueEntry{{Repository: builtRepository, At: minuteOf(300)}, {Repository: "ACME/Gadgets", At: minuteOf(45)},
				{Repository: "acme/elsewhere", At: minuteOf(200)}}
			return tl
		}, []float64{0.5, 1, 1.5}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := c.window(t)
			models := fittedModels(t, w)
			tl := c.course(t, models)
			whole := modelledSignals(t, models, tl)
			reader, err := models.Reader(w.From, SetbackSources{}, 0)
			if err != nil {
				t.Fatalf("read the models: %v", err)
			}
			events := courseOf(tl)
			var moments []time.Time
			for at := w.From; at.Before(w.To); at = at.Add(5 * time.Minute) {
				moments = append(moments, at)
			}
			for _, event := range events {
				moments = append(moments, event.at)
			}
			slices.SortFunc(moments, time.Time.Compare)
			moments = slices.CompactFunc(moments, time.Time.Equal)
			through := time.Time{}
			for _, line := range w.Provider {
				through = maxTime(through, line.Minute.Add(2*time.Minute))
			}
			fed := 0
			for _, at := range moments {
				for ; fed < len(events) && !events[fed].at.After(at); fed++ {
					if err := events[fed].feed(reader); err != nil {
						t.Fatalf("feed the course at %s: %v", stamp(events[fed].at), err)
					}
				}
				check := func(what string, got, want signals.Value) {
					if !sameValue(got, want) {
						t.Errorf("%s at %s: the reader gives %s, the whole timeline %s", what, stamp(at), describeValue(got), describeValue(want))
					}
				}
				check("the headroom", reader.Headroom(at), whole.Headroom(at))
				check("the refusals", reader.Refusals(at).Value(), whole.Refusals(at).Value())
				check("the kills", reader.Kills(at), whole.Kills(at))
				check("the queue", reader.Queue(at), whole.Queue(at))
				if got, want := reader.Refusals(at), whole.Refusals(at); got != want {
					t.Errorf("the refusals at %s: the reader gives %+v, the whole timeline %+v", stamp(at), got, want)
				}
				frozen := modelledSignals(t, models, frozenAt(tl, at))
				for _, limit := range c.limits {
					compare := func(what string, got time.Time, gotDue bool, want time.Time, wantDue bool) {
						if !sameCrossing(got, gotDue, want, wantDue) {
							t.Errorf("%s's next crossing of %v after %s: the reader gives %s, the whole timeline %s", what, limit, stamp(at),
								describeCrossing(got, gotDue), describeCrossing(want, wantDue))
						}
					}
					got, gotDue := reader.HeadroomNext(at, limit)
					want, wantDue := signals.ProviderNext(frozen.series, at, through, limit)
					compare("the headroom", got, gotDue, want, wantDue)
					got, gotDue = reader.KillsNext(at, limit)
					want, wantDue = signals.KillsNext(w.coverageOf(SignalKills, at), whole.kills, at, limit)
					compare("the kills", got, gotDue, want, wantDue)
					got, gotDue = reader.QueueNext(at, limit)
					want, wantDue = signals.QueueNext(w.coverageOf(SignalQueue, at), whole.waits, at, limit)
					compare("the queue", got, gotDue, want, wantDue)
					got, gotDue = reader.RefusalsNext(at, limit)
					want, wantDue = signals.RefusalsNext(whole.series, at, limit, reader.covered)
					compare("the refusals", got, gotDue, want, wantDue)
				}
			}
			reader.advance(w.End())
			if !slices.EqualFunc(reader.kills, whole.kills, time.Time.Equal) {
				t.Errorf("the reader's kills are %v, the whole timeline's %v", reader.kills, whole.kills)
			}
			if got, want := fmt.Sprint(reader.KillsApart, reader.EntriesApart, reader.Unserved), fmt.Sprint(whole.KillsApart, whole.EntriesApart, whole.Unserved); got != want {
				t.Errorf("the reader counts %s kills apart, pull requests apart and unserved, the whole timeline %s", got, want)
			}
		})
	}
}

// A drawn issue's kills are its own (spec R11, memoryForm): each drawn issue has an accumulator
// of its own, so two courses that differ in every other issue, the issues in flight and each
// recorded segment's start, but give one drawn issue the same stretches in flight, give it the same
// kills, as two rules at one seed would. On killWorkWindow, whose one day's rate is 0.5 kills for
// each hour of an issue in flight, ACME-7 stands in flight from 02:00 to 05:30 in both courses, a
// kill at 04:00 when its own two hours pass 1, in each. The first course starts ACME-1's segment at
// 01:00, its kills at 01:20 and 05:10; the second at 00:30, its kills at 00:50 and 04:40, and adds
// ACME-8 from 01:00 to 05:00, whose own hours pass 1 at 03:00 and 2 at its leave, 05:00. ACME-2's
// kill, in no segment, stays at 03:20 in both. ACME-8's kills move none of ACME-7's.
func TestADrawnIssuesKillsAreItsOwnInEveryCourse(t *testing.T) {
	t.Parallel()
	w := killWorkWindow(t)
	models := fittedModels(t, w)
	drawnKills := func(tl ReplayTimeline) []time.Time {
		reader, err := models.Reader(w.From, SetbackSources{}, 0)
		if err != nil {
			t.Fatalf("read the models: %v", err)
		}
		for _, event := range courseOf(tl) {
			if err := event.feed(reader); err != nil {
				t.Fatalf("feed the course at %s: %v", stamp(event.at), err)
			}
		}
		reader.advance(w.End())
		return reader.kills
	}
	record := recordTimelineOf(t, models)
	first := segmentMoved(t, record, "ACME-1", minuteOf(0), minuteOf(60))
	first.Drawn = []DrawnFlight{{Issue: "ACME-7", From: minuteOf(120), To: minuteOf(330)}}
	first.InFlight = []FlightStep{{At: minuteOf(0), Issues: 1}, {At: minuteOf(120), Issues: 2}, {At: minuteOf(330), Issues: 1}}
	second := segmentMoved(t, record, "ACME-1", minuteOf(0), minuteOf(30))
	second.Drawn = []DrawnFlight{{Issue: "ACME-7", From: minuteOf(120), To: minuteOf(330)}, {Issue: "ACME-8", From: minuteOf(60), To: minuteOf(300)}}
	second.InFlight = []FlightStep{{At: minuteOf(0), Issues: 3}, {At: minuteOf(60), Issues: 5}, {At: minuteOf(300), Issues: 4}}
	firstKills, secondKills := drawnKills(first), drawnKills(second)
	assertMoments(t, "the first course's kills", firstKills, []time.Time{minuteOf(80), minuteOf(200), minuteOf(240), minuteOf(310)})
	assertMoments(t, "the second course's kills", secondKills, []time.Time{minuteOf(50), minuteOf(180), minuteOf(200), minuteOf(240), minuteOf(280),
		minuteOf(300)})
	if !slices.Contains(secondKills, minuteOf(240)) || !slices.Contains(firstKills, minuteOf(240)) {
		t.Errorf("ACME-7's kill at 04:00 is in the first course %t and in the second %t, want both", slices.Contains(firstKills, minuteOf(240)),
			slices.Contains(secondKills, minuteOf(240)))
	}
}
