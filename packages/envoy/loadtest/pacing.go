package main

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// overScheduleLimit is the set-aside rule's threshold: a step is set aside when, in any second
// of its measured window, the generator sent more requests, or more event writes, than that
// second's schedule held by more than this share of it - and also, whatever its seconds, when a
// kind it was to send recorded nothing scheduled (judgePacing, paceTargets). It is a tenth, the
// architect's threshold (dispatch://INFRA-592, 2026-11-15), because a tenth lies far from both of
// the things it tells apart. A generator that keeps to its schedule sends in each second what the
// schedule put there, give or take the few arrivals sent a moment late across a second's edge. A
// generator the box stalled sends, once it runs again, every arrival it owes at once (openLoop:
// sleepUntil returns at once for a time already past), so the second after a stall holds the
// seconds it missed: in the failing 10x step /proc/net/snmp counted 700 to 3,540 new connections
// a second against a schedule of 640. Such a step did not deliver the load it claims, and its
// connection failures measure the test bed rather than Dispatch.
const overScheduleLimit = 0.10

// minJudgedSchedule is the smallest schedule a second is judged against. Below ten arrivals one
// arrival sent a moment late, across the edge into the next second, puts that second more than a
// tenth over its schedule (a second scheduled 4 that sends 5 is 25% over), so the ratio of so
// small a second measures where an arrival fell against the clock and not a burst; a second
// scheduled 0 or 1 has no ratio worth the name at all. So a second whose schedule holds fewer
// than ten is judged as if it held ten: one or two arrivals across its edge never break the rule,
// while a burst into it still does once it sends more than 11, which is the backlog of a stall
// and no edge effect. At ten, one arrival across the edge is a tenth over, which is not more than
// a tenth.
const minJudgedSchedule = 10

// lateTolerance is how late an arrival may be sent and still count in the second its schedule put
// it in; an arrival sent later than that counts in the second it was sent. It is 100 ms, the
// architect's decision (dispatch://INFRA-592, open-items.md, 2026-11-15), because it lies far
// above the lateness of a generator on time and far below that of a stalled one. A generator on
// time sends each arrival a few milliseconds after its time (sleepUntil's wake-up and its
// goroutine's turn), and at a rate drop those milliseconds carry the faster second's last arrivals
// into the slower second after it: counted in the second each was sent, the tester's recovery step
// read 60 sent against 57 scheduled in its drop second, and 1x:60s,10x:60s,1x:60s at a constant
// 5 ms lateness was set aside in 18 of 400 seeds, at 12 ms in 241. A generator the box stalled is
// seconds late, and the second it catches up in holds 5.5 to 18 times its schedule. So 100 ms
// moves the rate edge's carry-over back to the second it belongs to, and leaves a stalled
// generator's burst in the second it lands in - all but the newest 100 ms of its backlog, which
// is sent within the tolerance and counts back in the seconds its schedule put it in (record).
// So a stall shorter than about 200 ms is seldom set aside. What a stall moves into the second it
// ends in is the arrivals it held up for more than 100 ms that the schedule put in an earlier
// second: at most the stall's length less 100 ms of arrivals, which at a steady rate passes the
// rule's tenth of a second's schedule (100 ms of arrivals) only once the stall passes 200 ms, and
// then only when it crosses a second's start. That costs the rule nothing it is for: a generator
// on time is milliseconds late, a stall under 200 ms still sends every arrival it held up within
// a fifth of a second of its time, and a stall that takes from a step the load it claims is
// seconds long, as the failing 10x step's were.
const lateTolerance = 100 * time.Millisecond

// The two kinds of arrival the rule judges, as summary.json and arrivals.json name them.
const (
	paceRequests = "requests"
	paceEvents   = "events"
)

// secondCount is one second's arrivals of one generator: those its schedule put in that second,
// and those it sent in it.
type secondCount struct {
	scheduled, sent int
}

// pacer counts one generator's arrivals per second of the step, by when the schedule put each one
// (scheduled) and by when the generator let it go (sent), the warm-up's seconds counted from the
// step's start and the window's from the window's start. An arrival counts as sent in its
// scheduled second unless it was sent more than lateTolerance after its time, and then in the
// second it was sent; such an arrival sent at or after the window's end is counted in afterWindow.
// warmupLatest and windowLatest are the largest lateness of the arrivals the warm-up's schedule
// and the window's held. A pacer belongs to the one goroutine that walks its generator's schedule,
// and is read once that goroutine has returned, so it takes no lock.
type pacer struct {
	start, winStart, winEnd    time.Time
	warmup, window             []secondCount
	afterWindow                int
	warmupLatest, windowLatest latest
}

// latest is the largest lateness, sent minus scheduled, of the arrivals of one span; ok is false
// while the span has none.
type latest struct {
	lateness time.Duration
	ok       bool
}

func (l *latest) add(lateness time.Duration) {
	if !l.ok || lateness > l.lateness {
		l.lateness, l.ok = lateness, true
	}
}

func newPacer(start, winStart, winEnd time.Time) *pacer {
	return &pacer{start: start, winStart: winStart, winEnd: winEnd,
		warmup: make([]secondCount, wholeSeconds(winStart.Sub(start))),
		window: make([]secondCount, wholeSeconds(winEnd.Sub(winStart)))}
}

// wholeSeconds is how many seconds, the last one perhaps partial, d spans.
func wholeSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

// slot is the count of the second t falls in, nil outside [start, winEnd).
func (p *pacer) slot(t time.Time) *secondCount {
	var secs []secondCount
	var from time.Time
	switch {
	case t.Before(p.start) || !t.Before(p.winEnd):
		return nil
	case t.Before(p.winStart):
		secs, from = p.warmup, p.start
	default:
		secs, from = p.window, p.winStart
	}
	if i := int(t.Sub(from) / time.Second); i < len(secs) {
		return &secs[i]
	}
	return nil
}

// record counts one arrival the schedule put at scheduled and the generator sent at sent: as
// scheduled in scheduled's second, and as sent there too unless it was sent more than
// lateTolerance late, when it counts as sent in sent's second.
func (p *pacer) record(scheduled, sent time.Time) {
	lateness := sent.Sub(scheduled)
	c := p.slot(scheduled)
	if c != nil {
		c.scheduled++
		if scheduled.Before(p.winStart) {
			p.warmupLatest.add(lateness)
		} else {
			p.windowLatest.add(lateness)
		}
	}
	if c == nil || lateness > lateTolerance {
		c = p.slot(sent)
	}
	switch {
	case c != nil:
		c.sent++
	case !sent.Before(p.winEnd):
		p.afterWindow++
	}
}

// arrivalSecond is one second of arrivals.json: the requests and event writes the schedule put
// in it and those the generators sent in it, every population and every generator of each kind
// summed.
type arrivalSecond struct {
	Second            int `json:"second"`
	RequestsScheduled int `json:"requests_scheduled"`
	RequestsSent      int `json:"requests_sent"`
	EventsScheduled   int `json:"events_scheduled"`
	EventsSent        int `json:"events_sent"`
}

// arrivalsFile is arrivals.json: the step's arrivals per second of the warm-up and of the window,
// those sent once the window had ended, and each kind's largest lateness (sent minus scheduled,
// in ms) of the arrivals the warm-up's schedule held and of the window's, null for a span that
// held none.
type arrivalsFile struct {
	Warmup                      []arrivalSecond `json:"warmup"`
	Window                      []arrivalSecond `json:"window"`
	RequestsSentAfterWindow     int             `json:"requests_sent_after_window"`
	EventsSentAfterWindow       int             `json:"events_sent_after_window"`
	RequestsMaxLatenessMs       *float64        `json:"requests_max_lateness_ms"`
	RequestsWarmupMaxLatenessMs *float64        `json:"requests_warmup_max_lateness_ms"`
	EventsMaxLatenessMs         *float64        `json:"events_max_lateness_ms"`
	EventsWarmupMaxLatenessMs   *float64        `json:"events_warmup_max_lateness_ms"`
}

// arrivalsOf sums the pacers of each kind into arrivals.json. Every pacer of a step has the
// step's start and window, so their seconds line up.
func arrivalsOf(requests, events []*pacer) arrivalsFile {
	var f arrivalsFile
	add := func(pacers []*pacer, kind string) {
		for _, p := range pacers {
			f.Warmup = addSeconds(f.Warmup, p.warmup, kind)
			f.Window = addSeconds(f.Window, p.window, kind)
			if kind == paceRequests {
				f.RequestsSentAfterWindow += p.afterWindow
				f.RequestsMaxLatenessMs = laterMs(f.RequestsMaxLatenessMs, p.windowLatest)
				f.RequestsWarmupMaxLatenessMs = laterMs(f.RequestsWarmupMaxLatenessMs, p.warmupLatest)
			} else {
				f.EventsSentAfterWindow += p.afterWindow
				f.EventsMaxLatenessMs = laterMs(f.EventsMaxLatenessMs, p.windowLatest)
				f.EventsWarmupMaxLatenessMs = laterMs(f.EventsWarmupMaxLatenessMs, p.warmupLatest)
			}
		}
	}
	add(requests, paceRequests)
	add(events, paceEvents)
	return f
}

// laterMs is the larger of ms and l, in ms; nil while neither holds a lateness.
func laterMs(ms *float64, l latest) *float64 {
	if !l.ok {
		return ms
	}
	v := float64(l.lateness) / float64(time.Millisecond)
	if ms != nil && *ms >= v {
		return ms
	}
	return &v
}

// addSeconds adds one pacer's seconds of kind into rows.
func addSeconds(rows []arrivalSecond, secs []secondCount, kind string) []arrivalSecond {
	for len(rows) < len(secs) {
		rows = append(rows, arrivalSecond{Second: len(rows)})
	}
	for i, c := range secs {
		if kind == paceRequests {
			rows[i].RequestsScheduled += c.scheduled
			rows[i].RequestsSent += c.sent
		} else {
			rows[i].EventsScheduled += c.scheduled
			rows[i].EventsSent += c.sent
		}
	}
	return rows
}

// of is the second's count of kind.
func (s arrivalSecond) of(kind string) secondCount {
	if kind == paceRequests {
		return secondCount{scheduled: s.RequestsScheduled, sent: s.RequestsSent}
	}
	return secondCount{scheduled: s.EventsScheduled, sent: s.EventsSent}
}

// overSchedule is how far a second ran over its schedule, sent / max(scheduled,
// minJudgedSchedule), and whether that breaks the set-aside rule: more than overScheduleLimit
// over.
func (c secondCount) overSchedule() (ratio float64, breaks bool) {
	base := max(c.scheduled, minJudgedSchedule)
	return float64(c.sent) / float64(base), float64(c.sent-base) > overScheduleLimit*float64(base)
}

// pacingSummary is summary.json's pacing: what the generators delivered against their schedule,
// per kind, and the rule's three constants.
type pacingSummary struct {
	OverScheduleLimit float64    `json:"over_schedule_limit"`
	MinJudgedSchedule int        `json:"min_judged_schedule"`
	LateToleranceMs   float64    `json:"late_tolerance_ms"`
	Requests          pacingKind `json:"requests"`
	Events            pacingKind `json:"events"`
}

// pacingKind is one kind's arrivals: the window's scheduled and sent counts and those sent after
// it, the second of the window, and of the warm-up, that ran furthest over its schedule
// (overSchedule's ratio), and the largest lateness of the arrivals each span's schedule held
// (arrivals.json). A kind that had nothing scheduled or sent in a span has no such second, and
// one whose span held no arrival no lateness: those fields are null.
type pacingKind struct {
	Scheduled           int      `json:"scheduled"`
	Sent                int      `json:"sent"`
	SentAfterWindow     int      `json:"sent_after_window"`
	MaxOver             *float64 `json:"max_over"`
	MaxOverSecond       *int     `json:"max_over_second"`
	WarmupMaxOver       *float64 `json:"warmup_max_over"`
	WarmupMaxOverSecond *int     `json:"warmup_max_over_second"`
	MaxLatenessMs       *float64 `json:"max_lateness_ms"`
	WarmupMaxLatenessMs *float64 `json:"warmup_max_lateness_ms"`
}

// setAsideSummary is summary.json's set_aside: why the step is set aside, every second of the
// window that broke the rule, in order, and the kinds the rule refused to judge (paceTargets:
// a target above zero, nothing scheduled recorded). It is absent for a step that kept to its
// schedule.
type setAsideSummary struct {
	Why        string       `json:"why"`
	Seconds    []overSecond `json:"seconds"`
	Unrecorded []string     `json:"unrecorded,omitempty"`
}

// overSecond is one second of the window, counted from its start, in which one kind's sent
// arrivals broke the rule: its schedule, what was sent and how far over (overSchedule's ratio).
type overSecond struct {
	Second    int     `json:"second"`
	Kind      string  `json:"kind"`
	Scheduled int     `json:"scheduled"`
	Sent      int     `json:"sent"`
	Over      float64 `json:"over"`
}

// paceTargets are the kinds a step was to send, those whose target is above zero. The rule
// judges a kind by what its pacer recorded, so a kind the step was to send whose pacer recorded
// nothing scheduled, in the warm-up or the window, never reached the rule: a generator that does
// not feed its pacer would otherwise read as on time with empty counts.
type paceTargets struct {
	requests, events bool
}

// judgePacing reads arrivals.json's counts into summary.json's pacing, and set_aside when a
// second of the window broke the rule or a kind of targets recorded nothing scheduled, which the
// rule then refuses to judge (nil otherwise). The warm-up is recorded and not judged: nothing in
// it is measured.
func judgePacing(f arrivalsFile, targets paceTargets) (pacingSummary, *setAsideSummary) {
	sum := pacingSummary{OverScheduleLimit: overScheduleLimit, MinJudgedSchedule: minJudgedSchedule,
		LateToleranceMs: float64(lateTolerance) / float64(time.Millisecond)}
	var over []overSecond
	var unrecorded []string
	for _, kind := range []string{paceRequests, paceEvents} {
		k := pacingKind{}
		k.WarmupMaxOver, k.WarmupMaxOverSecond, _ = maxOver(f.Warmup, kind)
		var breaks []overSecond
		k.MaxOver, k.MaxOverSecond, breaks = maxOver(f.Window, kind)
		over = append(over, breaks...)
		for _, s := range f.Window {
			c := s.of(kind)
			k.Scheduled += c.scheduled
			k.Sent += c.sent
		}
		warmupScheduled := 0
		for _, s := range f.Warmup {
			warmupScheduled += s.of(kind).scheduled
		}
		want := targets.events
		if kind == paceRequests {
			want = targets.requests
			k.SentAfterWindow = f.RequestsSentAfterWindow
			k.MaxLatenessMs, k.WarmupMaxLatenessMs = f.RequestsMaxLatenessMs, f.RequestsWarmupMaxLatenessMs
			sum.Requests = k
		} else {
			k.SentAfterWindow = f.EventsSentAfterWindow
			k.MaxLatenessMs, k.WarmupMaxLatenessMs = f.EventsMaxLatenessMs, f.EventsWarmupMaxLatenessMs
			sum.Events = k
		}
		if want && k.Scheduled+warmupScheduled == 0 {
			unrecorded = append(unrecorded, kind)
		}
	}
	if len(over) == 0 && len(unrecorded) == 0 {
		return sum, nil
	}
	var why []string
	if len(over) > 0 {
		// The seconds in order, a second's requests before its events ("requests" > "events").
		slices.SortStableFunc(over, func(a, b overSecond) int {
			return cmp.Or(cmp.Compare(a.Second, b.Second), strings.Compare(b.Kind, a.Kind))
		})
		worst := over[0]
		for _, s := range over[1:] {
			if s.Over > worst.Over {
				worst = s
			}
		}
		why = append(why, fmt.Sprintf("the generator sent more than %s over its schedule in %s of the window; the worst, second %d: %d %s sent against %d scheduled (%sx)",
			percent(overScheduleLimit), plural(len(over), "second"), worst.Second, worst.Sent, worst.Kind, worst.Scheduled, num(worst.Over, 2)))
	} else {
		over = []overSecond{}
	}
	if len(unrecorded) > 0 {
		why = append(why, fmt.Sprintf("nothing scheduled was recorded of the %s, though the step's target for them is above 0: the rule has nothing to judge them by, so it refuses to judge the step rather than read it as on time",
			strings.Join(unrecorded, " or the ")))
	}
	return sum, &setAsideSummary{Why: strings.Join(why, "; "), Seconds: over, Unrecorded: unrecorded}
}

// maxOver is the second of secs that ran furthest over kind's schedule, nil for a span with
// nothing of kind scheduled or sent, and every second of it that broke the rule.
func maxOver(secs []arrivalSecond, kind string) (*float64, *int, []overSecond) {
	var best *float64
	var at *int
	var breaks []overSecond
	for _, s := range secs {
		c := s.of(kind)
		if c.scheduled == 0 && c.sent == 0 {
			continue
		}
		ratio, broke := c.overSchedule()
		if best == nil || ratio > *best {
			best, at = &ratio, &s.Second
		}
		if broke {
			breaks = append(breaks, overSecond{Second: s.Second, Kind: kind, Scheduled: c.scheduled, Sent: c.sent, Over: ratio})
		}
	}
	return best, at, breaks
}

// percent is a share as a percentage, to a tenth of a percent: 0.1 is "10%".
func percent(share float64) string { return fmt.Sprintf("%g%%", math.Round(share*1000)/10) }

// sentCell is the window's "sent / scheduled".
func (k pacingKind) sentCell() string {
	return num(float64(k.Sent), 0) + " / " + num(float64(k.Scheduled), 0)
}

// overCell is a span's second furthest over its schedule, "1.04x (s 17)", or "-" for a span with
// nothing scheduled or sent.
func overCell(ratio *float64, second *int) string {
	if ratio == nil || second == nil {
		return "-"
	}
	return fmt.Sprintf("%sx (s %d)", num(*ratio, 2), *second)
}

// latenessCell is a span's largest lateness in ms, "12", or "-" for a span that held no arrival.
func latenessCell(ms *float64) string {
	if ms == nil {
		return "-"
	}
	return num(*ms, 0)
}

// text is the kind's pacing as run.log writes it.
func (k pacingKind) text() string {
	return fmt.Sprintf("sent %s scheduled in the window, most over schedule %s (warm-up %s), latest %s ms (warm-up %s ms), %d sent after the window",
		k.sentCell(), overCell(k.MaxOver, k.MaxOverSecond), overCell(k.WarmupMaxOver, k.WarmupMaxOverSecond),
		latenessCell(k.MaxLatenessMs), latenessCell(k.WarmupMaxLatenessMs), k.SentAfterWindow)
}

// cell is the pacing table's set-aside cell: "no", or "yes:" with the seconds that broke the rule
// and each kind the rule refused to judge.
func (a *setAsideSummary) cell() string {
	if a == nil {
		return "no"
	}
	var parts []string
	if len(a.Seconds) > 0 {
		parts = append(parts, plural(len(a.Seconds), "second"))
	}
	for _, kind := range a.Unrecorded {
		parts = append(parts, "no "+kind+" recorded")
	}
	return "yes: " + strings.Join(parts, ", ")
}

// setAsideMark marks the step's name in every table of TABLES.md when the step is set aside.
const setAsideMark = "(set aside)"

// setAsideWord is what a set-aside step reads as where another step reads pass or fail: the
// curves' Pass table and curves.csv, and loadgen step-check's clause.
const setAsideWord = "set aside"

// setAsideNote is TABLES.md's first lines when a step is set aside: each such step, why, and what
// set aside means, so it is the first thing a reader of the tables sees.
func setAsideNote(md *markdown, steps []stepData) {
	var aside []string
	for _, s := range steps {
		if s.sum.SetAside != nil {
			aside = append(aside, fmt.Sprintf("- **%s**: %s.", s.name, mdEscape(s.sum.SetAside.Why)))
		}
	}
	if len(aside) == 0 {
		return
	}
	md.line("**SET ASIDE: no pass and no failure.** Nothing shows that the generator of each step below delivered the load the step claims: in a second of its window it sent more than %s over that second's schedule (counted in its scheduled second unless sent more than %s late), the burst of a generator the box stalled, which then fires every arrival it owes at once; or it recorded nothing scheduled of a kind the step was to send, which the rule then refuses to judge. So its rows below describe the test bed and not Dispatch (summary.json `set_aside`, arrivals.json). Run it again where the generator keeps, and records, its schedule.\n",
		percent(overScheduleLimit), lateTolerance)
	md.line("%s\n", strings.Join(aside, "\n"))
}
