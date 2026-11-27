package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pacedGenerator walks a Poisson schedule of rate per second from stream of rngSeed under the
// -schedule sched (nil for none), the schedule openLoopPlan gives runEvents and runRequests, and
// paces each arrival as sent at send(at): a generator whose clock is send.
func pacedGenerator(rngSeed uint64, rate float64, stream uint64, sched schedule, start, winStart, winEnd time.Time, send func(at time.Time) time.Time) *pacer {
	p := newPacer(start, winStart, winEnd)
	openLoopPlan(rngSeed, stream, ratePlan(rate, start, winStart, sched), winStart, winEnd, func(_ *rand.Rand, at time.Time, _ bool) bool {
		p.record(at, send(at))
		return true
	})
	return p
}

// pacingWindow is a step of a 10 s warm-up and a 60 s window.
func pacingWindow() (start, winStart, winEnd time.Time) {
	start = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	return start, start.Add(10 * time.Second), start.Add(70 * time.Second)
}

// rateDrop is the reviewer's step: a 10 s warm-up, then -schedule 1x:60s,10x:60s,1x:60s, whose
// drop from 10x to 1x opens second 120 of the window (drop).
func rateDrop(t *testing.T) (sched schedule, start, winStart, winEnd, drop time.Time) {
	t.Helper()
	sched, err := parseSchedule("1x:60s,10x:60s,1x:60s")
	if err != nil {
		t.Fatal(err)
	}
	start = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	winStart = start.Add(10 * time.Second)
	return sched, start, winStart, winStart.Add(sched.total()), winStart.Add(120 * time.Second)
}

// bothKinds is a step that was to send requests and event writes.
var bothKinds = paceTargets{requests: true, events: true}

// ms is d in ms, as arrivals.json and summary.json record a lateness.
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// msIs says a lateness field holds want ms, or is null for want -1.
func msIs(got *float64, want float64) bool {
	if want < 0 {
		return got == nil
	}
	return got != nil && *got == want
}

// TestPacingSetsAsideAGeneratorThatSleepsThenCatchesUp: a generator the box stalls from second 20
// to second 35 of the window sends nothing in those seconds, then every arrival it owes at once,
// as openLoop does once sleepUntil returns for a time already past. The second it catches up in
// holds fifteen seconds of schedule, so the step is set aside, naming that second for the
// requests and for the event writes, and arrivals.json keeps every arrival: those the stall
// delayed are sent late, not lost. Their lateness is seconds, far past lateTolerance, so the
// tolerance leaves the burst in the second it lands in; the largest lateness of each kind, the
// first arrival the stall held, is recorded, and the warm-up's, 2 ms, apart.
func TestPacingSetsAsideAGeneratorThatSleepsThenCatchesUp(t *testing.T) {
	start, winStart, winEnd := pacingWindow()
	stallFrom, stallTo := winStart.Add(20*time.Second), winStart.Add(35*time.Second)
	send := func(at time.Time) time.Time {
		if !at.Before(stallFrom) && at.Before(stallTo) {
			return stallTo.Add(200 * time.Millisecond)
		}
		return at.Add(2 * time.Millisecond)
	}
	// latest is the largest lateness of a window arrival each generator below sent.
	latest := map[uint64]time.Duration{}
	tracked := func(stream uint64) func(time.Time) time.Time {
		return func(at time.Time) time.Time {
			sent := send(at)
			if !at.Before(winStart) {
				latest[stream] = max(latest[stream], sent.Sub(at))
			}
			return sent
		}
	}
	reqs := pacedGenerator(1, 60, requestStream, nil, start, winStart, winEnd, tracked(requestStream))
	events := pacedGenerator(1, 4, eventStream, nil, start, winStart, winEnd, tracked(eventStream))
	f := arrivalsOf([]*pacer{reqs}, []*pacer{events})
	if len(f.Warmup) != 10 || len(f.Window) != 60 {
		t.Fatalf("arrivals.json has %d warm-up and %d window seconds, want 10 and 60", len(f.Warmup), len(f.Window))
	}
	// Second 20 may hold an arrival of second 19 sent a moment late; from 21 on nothing is sent.
	for s := 21; s < 35; s++ {
		if got := f.Window[s]; got.RequestsSent != 0 || got.EventsSent != 0 || got.RequestsScheduled == 0 {
			t.Fatalf("stalled second %d: %+v, want arrivals scheduled and none sent", s, got)
		}
	}
	pacing, aside := judgePacing(f, bothKinds)
	if !allSent(pacing.Requests) || !allSent(pacing.Events) {
		t.Errorf("the window sent %+v requests and %+v events; the stall delays arrivals, it loses none", pacing.Requests, pacing.Events)
	}
	if aside == nil {
		t.Fatalf("a generator that caught up fifteen seconds at once is not set aside: %+v", pacing)
	}
	want := map[string]bool{"35 requests": false, "35 events": false}
	for _, s := range aside.Seconds {
		key := fmt.Sprintf("%d %s", s.Second, s.Kind)
		if _, ok := want[key]; !ok {
			t.Errorf("second %d (%s) broke the rule: %+v; only second 35 caught up", s.Second, s.Kind, s)
		}
		want[key] = true
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("set_aside does not name second %s: %+v", key, aside.Seconds)
		}
	}
	if *pacing.Requests.MaxOverSecond != 35 || *pacing.Requests.MaxOver < 10 {
		t.Errorf("the requests' furthest second is %d at %.2fx, want 35 at 10x or more", *pacing.Requests.MaxOverSecond, *pacing.Requests.MaxOver)
	}
	for _, w := range []string{"more than 10% over its schedule in 2 seconds of the window", "the worst, second 35:"} {
		if !strings.Contains(aside.Why, w) {
			t.Errorf("set_aside.why %q lacks %q", aside.Why, w)
		}
	}
	for _, c := range []struct {
		kind   string
		k      pacingKind
		latest time.Duration
	}{{paceRequests, pacing.Requests, latest[requestStream]}, {paceEvents, pacing.Events, latest[eventStream]}} {
		if c.k.MaxLatenessMs == nil || *c.k.MaxLatenessMs != ms(c.latest) || *c.k.MaxLatenessMs < 14_000 {
			t.Errorf("the %s' largest lateness is %v ms, want the stall's %v ms, seconds", c.kind, c.k.MaxLatenessMs, ms(c.latest))
		}
		if c.k.WarmupMaxLatenessMs == nil || *c.k.WarmupMaxLatenessMs != 2 {
			t.Errorf("the %s' largest lateness in the warm-up is %v ms, want 2", c.kind, c.k.WarmupMaxLatenessMs)
		}
	}
}

// TestPacingKeepsAGeneratorOnSchedule: a generator that sends each arrival a few ms after its
// time, some of them across a second's edge, is not set aside: at 1x (60 requests and 4 event
// writes a second) and at 10x (600 and 40). Its furthest second is within the tenth.
func TestPacingKeepsAGeneratorOnSchedule(t *testing.T) {
	start, winStart, winEnd := pacingWindow()
	late := func(at time.Time) time.Time { return at.Add(3 * time.Millisecond) }
	for _, rates := range [][2]float64{{60, 4}, {600, 40}} {
		f := arrivalsOf([]*pacer{pacedGenerator(1, rates[0], requestStream, nil, start, winStart, winEnd, late)},
			[]*pacer{pacedGenerator(1, rates[1], eventStream, nil, start, winStart, winEnd, late)})
		pacing, aside := judgePacing(f, bothKinds)
		if aside != nil {
			t.Errorf("%g req/s and %g events/s on schedule is set aside: %s", rates[0], rates[1], aside.Why)
		}
		if *pacing.Requests.MaxOver > 1+overScheduleLimit || *pacing.Events.MaxOver > 1+overScheduleLimit {
			t.Errorf("%g req/s on schedule: furthest seconds %.2fx and %.2fx, want within the tenth", rates[0], *pacing.Requests.MaxOver, *pacing.Events.MaxOver)
		}
		if !allSent(pacing.Requests) || pacing.Requests.SentAfterWindow > 2 {
			t.Errorf("%g req/s: %+v; want every arrival sent in the window, but the few late across its edges", rates[0], pacing.Requests)
		}
	}
}

// allSent says the window sent what it scheduled, give or take two arrivals sent across its
// edges: one of the warm-up's sent in its first second, one of its own after its end.
func allSent(k pacingKind) bool {
	d := k.Sent + k.SentAfterWindow - k.Scheduled
	return d >= -2 && d <= 2
}

// TestPacingJudgesARateDropOnTime: a generator on time that sends every arrival 12 ms after its
// time carries the 10x second's last arrivals into the 1x second after the drop. Counted in the
// second each was sent, that carry-over set most seeds aside (the reviewer measured 241 of 400 at
// 12 ms). Counted in its scheduled second, as an arrival at most lateTolerance late is, every
// second of the window sends exactly what it scheduled, the drop second included, so no seed is
// set aside: at 600 requests a second dropping to 60, and at 400 event writes a second dropping to
// 40.
func TestPacingJudgesARateDropOnTime(t *testing.T) {
	sched, start, winStart, winEnd, _ := rateDrop(t)
	late := func(at time.Time) time.Time { return at.Add(12 * time.Millisecond) }
	for seed := uint64(1); seed <= 20; seed++ {
		f := arrivalsOf([]*pacer{pacedGenerator(seed, 60, requestStream, sched, start, winStart, winEnd, late)},
			[]*pacer{pacedGenerator(seed, 40, eventStream, sched, start, winStart, winEnd, late)})
		if _, aside := judgePacing(f, bothKinds); aside != nil {
			t.Errorf("seed %d: 12 ms late across the 10x to 1x drop is set aside: %s", seed, aside.Why)
		}
		for _, s := range f.Window {
			if s.RequestsSent != s.RequestsScheduled || s.EventsSent != s.EventsScheduled {
				t.Errorf("seed %d, second %d: %+v; want each kind sent as scheduled", seed, s.Second, s)
				break
			}
		}
	}
}

// TestPacingCountsAnArrivalPastTheToleranceWhereItWasSent: at the same drop, the arrivals
// scheduled in the last 150 ms before it are sent late, every other 12 ms late. Sent exactly
// lateTolerance late, they count in their scheduled second, so the drop second sends what it
// scheduled and the step is on time. Sent 1 ms later, those that cross into the drop second count
// there: it sends that many more than it scheduled, the second before it that many fewer, and
// the step is set aside at the drop second alone, for the requests and for the event writes.
func TestPacingCountsAnArrivalPastTheToleranceWhereItWasSent(t *testing.T) {
	sched, start, winStart, winEnd, drop := rateDrop(t)
	for _, lateness := range []time.Duration{lateTolerance, lateTolerance + time.Millisecond} {
		crossed := map[uint64]int{} // per stream, the arrivals sent into the drop second from before it
		generator := func(rate float64, stream uint64) *pacer {
			return pacedGenerator(1, rate, stream, sched, start, winStart, winEnd, func(at time.Time) time.Time {
				if !at.Before(drop.Add(-150*time.Millisecond)) && at.Before(drop) {
					sent := at.Add(lateness)
					if !sent.Before(drop) {
						crossed[stream]++
					}
					return sent
				}
				return at.Add(12 * time.Millisecond)
			})
		}
		f := arrivalsOf([]*pacer{generator(60, requestStream)}, []*pacer{generator(40, eventStream)})
		_, aside := judgePacing(f, bothKinds)
		before, at := f.Window[119], f.Window[120]
		if crossed[requestStream] < 30 || crossed[eventStream] < 20 {
			t.Fatalf("%s late: %d requests and %d event writes crossed into the drop second; the fixture means about 60 and 40", lateness, crossed[requestStream], crossed[eventStream])
		}
		if lateness <= lateTolerance {
			if aside != nil {
				t.Errorf("%s late, not more than the tolerance, is set aside: %s", lateness, aside.Why)
			}
			if at.RequestsSent != at.RequestsScheduled || at.EventsSent != at.EventsScheduled || before.RequestsSent != before.RequestsScheduled {
				t.Errorf("%s late: drop second %+v, the second before %+v; want each sent as scheduled", lateness, at, before)
			}
			continue
		}
		if at.RequestsSent-at.RequestsScheduled != crossed[requestStream] || at.EventsSent-at.EventsScheduled != crossed[eventStream] ||
			before.RequestsScheduled-before.RequestsSent != crossed[requestStream] {
			t.Errorf("%s late: drop second %+v, the second before %+v; want the %d requests and %d event writes that crossed counted in the drop second",
				lateness, at, before, crossed[requestStream], crossed[eventStream])
		}
		if aside == nil {
			t.Fatalf("%s late, past the tolerance, carried %d requests into the drop second and is not set aside", lateness, crossed[requestStream])
		}
		var seconds []string
		for _, s := range aside.Seconds {
			seconds = append(seconds, fmt.Sprintf("%d %s", s.Second, s.Kind))
		}
		if !slices.Equal(seconds, []string{"120 requests", "120 events"}) {
			t.Errorf("%s late: set aside at %q, want the drop second alone, for each kind", lateness, seconds)
		}
	}
}

// TestPacingDoesNotJudgeASecondWithTooSmallASchedule: a second scheduled fewer than
// minJudgedSchedule arrivals is judged against minJudgedSchedule: one or two arrivals across its
// edge (scheduled 1, sent 3; scheduled 4, sent 6) break nothing, while a burst into it (scheduled
// 2, sent 12) still does. At the minimum, one arrival over is exactly a tenth, which is not more.
func TestPacingDoesNotJudgeASecondWithTooSmallASchedule(t *testing.T) {
	for _, c := range []struct {
		scheduled, sent int
		breaks          bool
	}{
		{0, 1, false}, {1, 3, false}, {4, 6, false}, {0, 11, false},
		{2, 12, true}, {0, 30, true},
		{10, 11, false}, {10, 12, true}, {600, 660, false}, {600, 661, true}, {640, 3540, true},
	} {
		if _, breaks := (secondCount{scheduled: c.scheduled, sent: c.sent}).overSchedule(); breaks != c.breaks {
			t.Errorf("a second scheduled %d that sent %d breaks the rule: %t, want %t", c.scheduled, c.sent, breaks, c.breaks)
		}
	}
	// In a step: the event writes at 4 a second, every one sent across the next second's edge
	// from second 30 on, set nothing aside.
	f := arrivalsFile{Window: []arrivalSecond{
		{Second: 0, EventsScheduled: 3, EventsSent: 1, RequestsScheduled: 60, RequestsSent: 60},
		{Second: 1, EventsScheduled: 1, EventsSent: 4, RequestsScheduled: 58, RequestsSent: 59},
		{Second: 2, EventsScheduled: 0, EventsSent: 2, RequestsScheduled: 61, RequestsSent: 60},
	}}
	if _, aside := judgePacing(f, bothKinds); aside != nil {
		t.Errorf("seconds too small to judge set the step aside: %s", aside.Why)
	}
}

// TestPacerCountsWhereEachArrivalFalls: the warm-up's seconds count from the step's start, the
// window's from its start. An arrival counts as sent in its scheduled second unless it was sent
// more than lateTolerance late, and then in the second it was sent, which for one sent at or
// after the window's end is after the window. So of the warm-up's last second, an arrival sent
// 80 ms into the window counts in the warm-up and one sent 200 ms late in the window; of the
// window's last second, one sent 70 ms after its end counts in that second, one sent 1 s after
// it after the window.
func TestPacerCountsWhereEachArrivalFalls(t *testing.T) {
	start, winStart, winEnd := pacingWindow()
	p := newPacer(start, winStart, winEnd)
	p.record(start.Add(1500*time.Millisecond), start.Add(1500*time.Millisecond).Add(lateTolerance))
	p.record(start.Add(2950*time.Millisecond), start.Add(2950*time.Millisecond).Add(lateTolerance+time.Millisecond))
	p.record(winStart.Add(-50*time.Millisecond), winStart.Add(30*time.Millisecond))
	p.record(winStart.Add(-100*time.Millisecond), winStart.Add(100*time.Millisecond))
	p.record(winEnd.Add(-20*time.Millisecond), winEnd.Add(50*time.Millisecond))
	p.record(winEnd.Add(-10*time.Millisecond), winEnd.Add(time.Second))
	for _, c := range []struct {
		name      string
		got, want secondCount
	}{
		{"warm-up second 1 (sent exactly lateTolerance late)", p.warmup[1], secondCount{1, 1}},
		{"warm-up second 2 (sent 1 ms past the tolerance)", p.warmup[2], secondCount{1, 0}},
		{"warm-up second 3", p.warmup[3], secondCount{0, 1}},
		{"warm-up second 9", p.warmup[9], secondCount{2, 1}},
		{"window second 0", p.window[0], secondCount{0, 1}},
		{"window second 59", p.window[59], secondCount{2, 1}},
	} {
		if c.got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, c.got, c.want)
		}
	}
	if p.afterWindow != 1 {
		t.Errorf("%d arrivals counted after the window, want 1", p.afterWindow)
	}
}

// TestPacingRecordsTheLargestLateness: arrivals.json and summary.json's pacing hold each kind's
// largest lateness, sent minus scheduled, over every generator of the kind, of the arrivals the
// warm-up's schedule held and of the window's apart (an arrival of the window sent after its end
// counts in the window's), null for a span that held none; run.log and the tables show it.
func TestPacingRecordsTheLargestLateness(t *testing.T) {
	start, winStart, winEnd := pacingWindow()
	a, b, events := newPacer(start, winStart, winEnd), newPacer(start, winStart, winEnd), newPacer(start, winStart, winEnd)
	a.record(start.Add(time.Second), start.Add(time.Second+40*time.Millisecond))
	a.record(winStart.Add(5*time.Second), winStart.Add(5*time.Second+3*time.Millisecond))
	b.record(winStart.Add(-time.Second), winStart.Add(-time.Second+5*time.Millisecond))
	b.record(winEnd.Add(-time.Second), winEnd.Add(1500*time.Millisecond))
	events.record(winStart.Add(time.Second), winStart.Add(time.Second+7*time.Millisecond))
	f := arrivalsOf([]*pacer{a, b}, []*pacer{events})
	for _, c := range []struct {
		name string
		got  *float64
		want float64 // -1: null
	}{
		{"requests_max_lateness_ms", f.RequestsMaxLatenessMs, 2500},
		{"requests_warmup_max_lateness_ms", f.RequestsWarmupMaxLatenessMs, 40},
		{"events_max_lateness_ms", f.EventsMaxLatenessMs, 7},
		{"events_warmup_max_lateness_ms", f.EventsWarmupMaxLatenessMs, -1},
	} {
		if !msIs(c.got, c.want) {
			t.Errorf("arrivals.json %s is %v, want %v (-1: null)", c.name, c.got, c.want)
		}
	}
	pacing, _ := judgePacing(f, bothKinds)
	if !msIs(pacing.Requests.MaxLatenessMs, 2500) || !msIs(pacing.Requests.WarmupMaxLatenessMs, 40) || !msIs(pacing.Events.MaxLatenessMs, 7) || !msIs(pacing.Events.WarmupMaxLatenessMs, -1) {
		t.Errorf("pacing holds %+v and %+v; want the arrivals' largest lateness", pacing.Requests, pacing.Events)
	}
	for _, c := range []struct {
		file string
		v    any
		want []string
	}{
		{"arrivals.json", f, []string{`"requests_max_lateness_ms":2500`, `"requests_warmup_max_lateness_ms":40`, `"events_max_lateness_ms":7`, `"events_warmup_max_lateness_ms":null`}},
		{"summary.json pacing", pacing, []string{`"late_tolerance_ms":100`, `"max_lateness_ms":2500`, `"warmup_max_lateness_ms":40`, `"warmup_max_lateness_ms":null`}},
	} {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range c.want {
			if !strings.Contains(string(b), w) {
				t.Errorf("%s %s lacks %s", c.file, b, w)
			}
		}
	}
	if text := pacing.Requests.text(); !strings.Contains(text, "latest 2,500 ms (warm-up 40 ms)") {
		t.Errorf("run.log's pacing line %q does not give the largest lateness", text)
	}
	if text := pacing.Events.text(); !strings.Contains(text, "latest 7 ms (warm-up - ms)") {
		t.Errorf("run.log's pacing line %q does not give the events' lateness, - for the warm-up's none", text)
	}
}

// TestPacingRefusesToJudgeAKindItRecordedNothingOf: a kind the step was to send (a target above
// zero) whose pacer recorded nothing scheduled, in the warm-up or the window, never reached the
// rule: the step is set aside with that reason, never read as on time. A kind with no target is
// not refused for recording nothing, nor is one whose arrivals fell in the warm-up alone.
func TestPacingRefusesToJudgeAKindItRecordedNothingOf(t *testing.T) {
	requestsOnly := arrivalsFile{Window: []arrivalSecond{{Second: 0, RequestsScheduled: 60, RequestsSent: 60}}}
	if _, aside := judgePacing(requestsOnly, paceTargets{requests: true}); aside != nil {
		t.Errorf("a step at 0 events/s that recorded no event write is set aside: %s", aside.Why)
	}
	_, aside := judgePacing(requestsOnly, bothKinds)
	if aside == nil {
		t.Fatal("a step that was to send event writes and recorded none is read as on time")
	}
	if !slices.Equal(aside.Unrecorded, []string{paceEvents}) || len(aside.Seconds) != 0 || aside.Seconds == nil ||
		!strings.Contains(aside.Why, "nothing scheduled was recorded of the events, though the step's target for them is above 0") ||
		!strings.Contains(aside.Why, "refuses to judge the step") {
		t.Errorf("set_aside %+v; want unrecorded [events], no second, and why naming the refusal", aside)
	}
	if cell := aside.cell(); cell != "yes: no events recorded" {
		t.Errorf("the pacing table's set-aside cell is %q, want yes: no events recorded", cell)
	}
	_, aside = judgePacing(arrivalsFile{}, bothKinds)
	if aside == nil || !slices.Equal(aside.Unrecorded, []string{paceRequests, paceEvents}) || !strings.Contains(aside.Why, "of the requests or the events") {
		t.Errorf("a step that recorded nothing of either kind: set_aside %+v, want both unrecorded", aside)
	}
	warmupOnly := arrivalsFile{Warmup: []arrivalSecond{{Second: 9, EventsScheduled: 1, EventsSent: 1}}, Window: requestsOnly.Window}
	if _, aside := judgePacing(warmupOnly, bothKinds); aside != nil {
		t.Errorf("event writes recorded in the warm-up alone are refused as unrecorded: %s", aside.Why)
	}
	stalled := arrivalsFile{Window: []arrivalSecond{{Second: 35, RequestsScheduled: 60, RequestsSent: 924}}}
	_, aside = judgePacing(stalled, bothKinds)
	if aside == nil || len(aside.Seconds) != 1 || !slices.Equal(aside.Unrecorded, []string{paceEvents}) ||
		!strings.Contains(aside.Why, "the worst, second 35:") || !strings.Contains(aside.Why, "; nothing scheduled was recorded of the events") {
		t.Errorf("a burst and an unrecorded kind: set_aside %+v, want both reasons", aside)
	} else if cell := aside.cell(); cell != "yes: 1 second, no events recorded" {
		t.Errorf("the set-aside cell is %q, want yes: 1 second, no events recorded", cell)
	}
}

// TestPacingCountsWhatTheGeneratorsSent: the request generator and the event generator, run for 4
// seconds against a server, feed their pacers every arrival they schedule and send. For each
// kind the pacer's scheduled counts are what the generator scheduled, and its sent counts, with
// those after the window, every request of that kind the server received: the event writes'
// scheduled seconds are their write records' own, and the requests the window scheduled are the
// window's request records. A step whose pacers recorded this is judged, not refused as
// unrecorded.
func TestPacingCountsWhatTheGeneratorsSent(t *testing.T) {
	var gets, posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
		} else {
			posts.Add(1)
			w.WriteHeader(http.StatusCreated)
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	mix, err := parseEventMix("comment=1")
	if err != nil {
		t.Fatal(err)
	}
	routes, err := loadRouteMix("")
	if err != nil {
		t.Fatal(err)
	}
	cfg := runConfig{sessions: 8, events: 20, requests: 40, warmup: time.Second, duration: 3 * time.Second,
		human: 0.1, mix: mix, routes: routes, rngSeed: 1}
	lg := log.New(io.Discard, "", 0)
	st := newStep(cfg, planFixtureSeed(8), lg, 1)
	p := st.pops[0]
	a := newAPI(srv.URL, "token", 64)
	p.wr = newWriter(a, p.sf, p.loop, 1, lg)
	st.openWindow(time.Now())
	ctx := context.Background()
	eventsDone := st.runEvents(ctx, p)
	var requestsDone <-chan struct{}
	p.reqs, requestsDone = st.runRequests(ctx, a, p)
	<-eventsDone
	<-requestsDone

	f := st.arrivals()
	type tally struct{ warmupScheduled, windowScheduled, sent int }
	count := func(kind string, afterWindow int) tally {
		tl := tally{sent: afterWindow}
		for _, s := range f.Warmup {
			tl.warmupScheduled += s.of(kind).scheduled
			tl.sent += s.of(kind).sent
		}
		for _, s := range f.Window {
			tl.windowScheduled += s.of(kind).scheduled
			tl.sent += s.of(kind).sent
		}
		return tl
	}
	reqs, evs := count(paceRequests, f.RequestsSentAfterWindow), count(paceEvents, f.EventsSentAfterWindow)

	// The requests: the server received each one the generator sent, and the window's request
	// records are those its schedule put in the window.
	if n := int(gets.Load()); n < 100 || reqs.sent != n || reqs.warmupScheduled+reqs.windowScheduled != n {
		t.Errorf("the request generator sent %d requests; its pacer recorded %+v, want all %d scheduled and sent", n, reqs, n)
	}
	if in := len(p.reqs.recs.snapshot()); reqs.windowScheduled != in || p.reqs.dropped.Load() != 0 {
		t.Errorf("the window holds %d request records; its pacer scheduled %d there", in, reqs.windowScheduled)
	}
	// The event writes: one write record each, holding its scheduled time, and one POST each.
	writes := p.writes.snapshot()
	if n := int(posts.Load()); n < 50 || len(writes) != n || evs.sent != n || evs.warmupScheduled+evs.windowScheduled != n {
		t.Errorf("the event generator sent %d writes (%d records); its pacer recorded %+v, want all scheduled and sent", n, len(writes), evs)
	}
	warmup, window := make([]int, len(f.Warmup)), make([]int, len(f.Window))
	for _, w := range writes {
		at := time.Unix(0, w.Scheduled)
		if w.InWindow {
			window[int(at.Sub(st.winStart)/time.Second)]++
		} else {
			warmup[int(at.Sub(st.start)/time.Second)]++
		}
	}
	for i, n := range warmup {
		if f.Warmup[i].EventsScheduled != n {
			t.Errorf("warm-up second %d: the pacer scheduled %d event writes, the generator %d", i, f.Warmup[i].EventsScheduled, n)
		}
	}
	for i, n := range window {
		if f.Window[i].EventsScheduled != n {
			t.Errorf("window second %d: the pacer scheduled %d event writes, the generator %d", i, f.Window[i].EventsScheduled, n)
		}
	}
	if _, aside := judgePacing(f, st.paceTargets()); aside != nil && len(aside.Unrecorded) > 0 {
		t.Errorf("a step whose generators fed their pacers is refused as unrecorded: %s", aside.Why)
	}
}

// TestPaceTargetsAreTheKindsTheStepSends: a kind is a target when a population's rate for it is
// above 0; a step at 0 sends none of it.
func TestPaceTargetsAreTheKindsTheStepSends(t *testing.T) {
	for _, c := range []struct {
		events, requests float64
		want             paceTargets
	}{{4, 60, bothKinds}, {0, 60, paceTargets{requests: true}}, {4, 0, paceTargets{events: true}}, {0, 0, paceTargets{}}} {
		cfg := planFixtureConfig()
		cfg.events, cfg.requests = c.events, c.requests
		if got := newStep(cfg, planFixtureSeed(4), log.New(io.Discard, "", 0), 0).paceTargets(); got != c.want {
			t.Errorf("%g events/s and %g requests/s: targets %+v, want %+v", c.events, c.requests, got, c.want)
		}
	}
}

// TestSetAsideReachesTheTables: a set-aside step says so before the first table, carries the mark
// in every table's step cell, and the pacing table reads yes with its seconds, or with the kind
// it recorded nothing of, and each kind's largest lateness; a step on schedule carries no mark,
// and a summary.json from before pacing reads - in the pacing cells.
func TestSetAsideReachesTheTables(t *testing.T) {
	over, second, prompt := 1.02, 17, 3.2
	onTime := &pacingSummary{Requests: pacingKind{Scheduled: 10_800, Sent: 10_800, MaxOver: &over, MaxOverSecond: &second, MaxLatenessMs: &prompt},
		Events: pacingKind{Scheduled: 720, Sent: 720, MaxOver: &over, MaxOverSecond: &second, MaxLatenessMs: &prompt}}
	worst, at, stall := 15.40, 35, 15_180.0
	stalled := &pacingSummary{Requests: pacingKind{Scheduled: 10_800, Sent: 10_800, MaxOver: &worst, MaxOverSecond: &at, MaxLatenessMs: &stall},
		Events: pacingKind{Scheduled: 720, Sent: 720, MaxOver: &over, MaxOverSecond: &second, MaxLatenessMs: &prompt}}
	_, aside := judgePacing(arrivalsFile{Window: []arrivalSecond{{Second: 35, RequestsScheduled: 60, RequestsSent: 924}}}, paceTargets{requests: true})
	unfed, refused := judgePacing(arrivalsFile{Window: []arrivalSecond{{Second: 0, RequestsScheduled: 60, RequestsSent: 60}}}, bothKinds)
	if aside == nil || refused == nil {
		t.Fatalf("the stalled step's set_aside %+v and the step that recorded no event write's %+v: want both set aside", aside, refused)
	}
	steps := []stepData{
		{name: "1x", sum: summary{Config: summaryConfig{Step: "1x"}, Pacing: onTime}},
		{name: "10x", sum: summary{Config: summaryConfig{Step: "10x"}, Pacing: stalled, SetAside: aside}},
		{name: "unfed", sum: summary{Config: summaryConfig{Step: "unfed"}, Pacing: &unfed, SetAside: refused}},
		{name: "old", sum: summary{Config: summaryConfig{Step: "old"}}},
	}
	md := renderTables(steps)
	head, _, _ := strings.Cut(md, "\n## ")
	if !strings.Contains(head, "**SET ASIDE: no pass and no failure.**") || !strings.Contains(head, "- **10x**: "+aside.Why) ||
		!strings.Contains(head, "- **unfed**: "+refused.Why) {
		t.Errorf("TABLES.md does not open with the set-aside steps:\n%s", head)
	}
	if strings.Contains(head, "**1x**") {
		t.Errorf("TABLES.md names the step on schedule as set aside:\n%s", head)
	}
	for _, title := range []string{"Events, delivery and requests", "Generator pacing and accept backlog", "Resources"} {
		if rowCells(section(md, title), "10x "+setAsideMark) == nil {
			t.Errorf("section %q has no row marked %s:\n%s", title, setAsideMark, section(md, title))
		}
	}
	pace := section(md, "Generator pacing and accept backlog")
	for _, want := range []string{
		"| 10x (set aside) | 10,800 / 10,800 | 15.40x (s 35) | 15,180 | 720 / 720 | 1.02x (s 17) | 3 | yes: 1 second | - | - | - |",
		"| unfed (set aside) | 60 / 60 | 1.00x (s 0) | - | 0 / 0 | - | - | yes: no events recorded | - | - | - |",
		"| 1x | 10,800 / 10,800 | 1.02x (s 17) | 3 | 720 / 720 | 1.02x (s 17) | 3 | no | - | - | - |",
		"| old | - | - | - | - | - | - | - | - | - | - |",
	} {
		if !strings.Contains(pace, want+"\n") {
			t.Errorf("the pacing table lacks %q:\n%s", want, pace)
		}
	}
	if strings.Contains(md, "1x (set aside)") {
		t.Errorf("the step on schedule is marked set aside:\n%s", md)
	}
	checkRowWidths(t, md)
}
