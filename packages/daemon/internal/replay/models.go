package replay

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"time"
)

// The load models of R11 (spec R11, plan t31): a replay keeps each work segment's recorded length,
// so it cannot show a refusal, a memory kill or a longer merge queue that more work in flight would
// have caused, and for those three signals a replay under another rule takes the signal from a
// model fitted on the window's own record, each a function of the work in flight alone (assumption
// 45). FitLoadModels fits the three on a window's record, with no replay, and tests each by R11's
// daily test at the recorded work in flight; a model that fails, or that no day tests, gives no
// limit for its signal. The docs page gives each model's form and parameters (replay.md, "legion
// replay models").

// The names of the three load models of R11.
const (
	ModelTokens = "tokens"
	ModelMemory = "memory"
	ModelQueue  = "queue"
)

// The verdicts of a load model and of each of its days under R11's daily test. The first two are
// the words passWord gives.
const (
	VerdictPasses = "passes"
	VerdictFails  = "fails"
	VerdictNoTest = "no test"
)

// dailyCountFloor is R11's count of a day's refusals or kills from which its daily test reads the
// model within 30% of the record (gapThirty); on a day with fewer recorded, the model passes where it
// gives at most this many.
const dailyCountFloor = 10

// The forms of the three load models, as the output and the docs page name them.
const (
	tokensForm = "tokens a minute = issues in flight × X, X drawn from the record's own uncached input tokens a minute per issue in flight; " +
		"a minute whose modelled tokens pass the project's own limit in force is a refused minute, which counts the record's own refusals " +
		"per refused minute (1 where the record holds no refused minute); the provider's refusals are kept as recorded (assumption 46)"
	memoryForm = "kills over a span = kills per issue-hour in flight × the span's issue-hours in flight, the rate the record's kills over its " +
		"issue-hours in flight; the pods' memory in a minute = bytes per issue in flight × issues in flight, the ratio of the record's " +
		"memory to its issues in flight over the minutes it reads"
	queueForm = "a pull request's wait in the merge queue = issues in flight when it entered × X, X drawn from the record's own waits " +
		"per issue in flight; a day's modelled median wait is the median of the waits the model gives the pull requests that left the queue " +
		"that day, each at its recorded work in flight"
)

// The reasons of a model that gives no test, of a span it skips and of a day with no test.
const (
	reasonNoProviderSpan = "the window holds no record of the provider in its span"
	reasonNoKillsSpan    = "the window holds no record of the memory guard's kills in its span"
	reasonNoQueueSpan    = "the window holds no record of the merge queue in its span"
	reasonNoLimitSpan    = "no limit of the project's own is recorded in the span, which leaves the project's own refusals nothing to test"
	reasonNoFlight       = "no issue was in flight where the window holds the model's record"
	reasonNoExitSpan     = "no pull request left the merge queue in the window's record"
	reasonNoProvider     = "the window holds no record of the provider here, so the model is neither fitted nor tested there (assumption 47)"
	reasonNoLimit        = "no limit of the project's own is recorded here, so the model is fitted and the project's own refusals have nothing to test (assumption 48)"
	reasonNoKills        = "the window holds no record of the memory guard's kills here, so the model is neither fitted nor tested there (assumption 47)"
	reasonNoQueue        = "the window holds no record of the merge queue here, so the model is neither fitted nor tested there (assumption 47)"
	reasonNoExit         = "no pull request left the merge queue in the day's record"
)

// The names of the models' parameters.
const (
	paramMinutesFitted     = "minutes fitted"
	paramTokensMean        = "uncached input tokens a minute per issue in flight: mean"
	paramTokensMedian      = "uncached input tokens a minute per issue in flight: median"
	paramTokensP90         = "uncached input tokens a minute per issue in flight: 90th percentile"
	paramTokensP99         = "uncached input tokens a minute per issue in flight: 99th percentile"
	paramRefusedMinutes    = "refused minutes recorded"
	paramRefusalsPerMinute = "the project's own refusals per refused minute"
	paramKillsRecorded     = "kills recorded"
	paramIssueHours        = "issue-hours in flight"
	paramKillRate          = "kills per issue-hour in flight"
	paramMemoryMinutes     = "minutes of the pods' memory fitted"
	paramBytesPerIssue     = "bytes of the pods' memory per issue in flight"
	paramWaitsFitted       = "waits fitted"
	paramWaitMedian        = "wait per issue in flight, in hours: median"
	paramWaitP90           = "wait per issue in flight, in hours: 90th percentile"
)

// LoadModels is the three load models of R11 fitted on one window's record (FitLoadModels): the
// window's project and span, and each model in the order tokens, memory, queue. `legion replay
// models --json` prints it, and the limit test reads it.
type LoadModels struct {
	Project string      `json:"project"`
	From    time.Time   `json:"from"`
	To      time.Time   `json:"to"`
	Models  []LoadModel `json:"models"`
}

// LoadModel is one load model fitted on a window's record: its name, the signal of R11 it gives a
// replay (provider, kills, queue), its form, whether it is fitted and with which parameters, its
// verdict under R11's daily test (VerdictPasses, VerdictFails, VerdictNoTest with its reason),
// whether it gives a limit for its signal (it passes: a model that fails or gives no test gives
// none), each span of the window's span it skips with the reason, and each day of the span.
type LoadModel struct {
	Name       string           `json:"name"`
	Signal     string           `json:"signal"`
	Form       string           `json:"form"`
	Fitted     bool             `json:"fitted"`
	Parameters []ModelParameter `json:"parameters"`
	Verdict    string           `json:"verdict"`
	Reason     string           `json:"reason,omitempty"`
	GivesLimit bool             `json:"givesLimit"`
	Skipped    []SkippedSpan    `json:"skipped"`
	Days       []ModelDay       `json:"days"`
}

// ModelParameter is one fitted parameter of a load model, by its name.
type ModelParameter struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

// SkippedSpan is a span [From, To) of a window's span in which a load model is not fitted, or not
// tested, and why.
type SkippedSpan struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Reason string    `json:"reason"`
}

// ModelDay is one UTC day of a window's span, clipped to it, under one load model: the day as
// 2006-01-02 and its span; the issues in flight over it, the scorecard's reading (Scorecard.InFlight
// over the day); the hours of it in which the test reads the window's record; the recorded and the
// modelled value R11's daily test compares, none on a day of no test (the tokens model's refusals
// by the project's own limit, the memory model's kills, the merge queue's median wait in hours);
// and the day's verdict, with its reason where it gives no test. Beside the test, and testing
// nothing: the load the model reads, recorded and modelled at the recorded work in flight (the
// tokens model's uncached input tokens a minute and the memory model's pods' memory in bytes, each a
// mean over the day's minutes the model reads; none for the merge queue's model and where the
// record holds no such minute), and the tokens model's refusals by the provider, kept as recorded
// (assumption 46).
type ModelDay struct {
	Day              string    `json:"day"`
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	InFlight         float64   `json:"inFlight"`
	RecordHours      float64   `json:"recordHours"`
	Recorded         *float64  `json:"recorded"`
	Modelled         *float64  `json:"modelled"`
	Verdict          string    `json:"verdict"`
	Reason           string    `json:"reason,omitempty"`
	LoadRecorded     *float64  `json:"loadRecorded,omitempty"`
	LoadModelled     *float64  `json:"loadModelled,omitempty"`
	ProviderRefusals *int64    `json:"providerRefusals,omitempty"`
}

// FitLoadModels fits the three load models of R11 on w's record alone, with no replay, each as a
// function of the work in flight, the record's issues in flight at each moment as the scorecard
// counts them (inFlightSteps), and tests each on each day of w's span by R11's daily test. A model
// is fitted and tested only in the spans where w holds its record (plan Q4, assumption 47): the
// tokens model in the minutes of the provider's record, its test of the project's own refusals in
// the minutes where w holds a record of the provider's headroom (a limit of the project's own,
// assumption 48); the memory model where w holds the record of the memory guard's kills; the merge
// queue's model where w holds the record of the queue. It refuses a window whose trace does not
// build, and a line of the kills' or the queue's record their readers refuse.
func FitLoadModels(w Window) (LoadModels, error) {
	t, err := BuildTrace(w)
	if err != nil {
		return LoadModels{}, err
	}
	kills, err := launcherKills(w.Launcher)
	if err != nil {
		return LoadModels{}, err
	}
	waits, err := queueWaits(w.Queue)
	if err != nil {
		return LoadModels{}, err
	}
	f := modelFit{w: w, flight: inFlightSteps(lineTimeline(t, ranksOf(t), t.ruleAt)), days: windowDays(w.From, w.To)}
	return LoadModels{Project: w.Project, From: w.From, To: w.To, Models: []LoadModel{f.tokens(), f.memory(kills), f.queue(waits)}}, nil
}

// inFlightSteps is how many issues stand in flight from each moment of tl on, as the scorecard
// counts them (Scorecard.InFlight, Measure's sweep): each issue whose state (IssueState.InFlight)
// is in flight, from the stint it enters flight in to the stint it leaves it in, or on to the
// timeline's end.
func inFlightSteps(tl Timeline) stretchCount {
	var stretches [][2]time.Time
	for _, c := range tl.Issues {
		var since time.Time
		flying := false
		for _, stint := range c.States {
			switch {
			case stint.InFlight && !flying:
				since, flying = stint.At, true
			case !stint.InFlight && flying:
				stretches, flying = append(stretches, [2]time.Time{since, stint.At}), false
			}
		}
		if flying {
			stretches = append(stretches, [2]time.Time{since, {}})
		}
	}
	return countStretches(stretches)
}

// hours is the integral of the count c gives over [from, to), in hours: issue-hours for a count of
// issues; 0 for an empty span.
func (c stretchCount) hours(from, to time.Time) float64 {
	if !to.After(from) {
		return 0
	}
	i := upTo(c.from, from)
	standing, at, total := 0, from, 0.0
	if i > 0 {
		standing = c.counts[i-1]
	}
	for ; i < len(c.from) && c.from[i].Before(to); i++ {
		total += float64(standing) * c.from[i].Sub(at).Hours()
		standing, at = c.counts[i], c.from[i]
	}
	return total + float64(standing)*to.Sub(at).Hours()
}

// windowDays is each UTC day of [from, to), clipped to it, in order.
func windowDays(from, to time.Time) []Span {
	var days []Span
	for start := from; start.Before(to); {
		midnight := start.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		end := minTime(midnight, to)
		days = append(days, Span{From: start, To: end})
		start = end
	}
	return days
}

// clipSpans is the parts of spans inside within, in order.
func clipSpans(spans []Span, within Span) []Span {
	var parts []Span
	for _, span := range spans {
		if from, to := maxTime(span.From, within.From), minTime(span.To, within.To); to.After(from) {
			parts = append(parts, Span{From: from, To: to})
		}
	}
	return parts
}

// spansHours is the hours spans hold together.
func spansHours(spans []Span) float64 {
	var held time.Duration
	for _, span := range spans {
		held += span.To.Sub(span.From)
	}
	return held.Hours()
}

// countWithin is how many of moments, in time order, stand in span.
func countWithin(moments []time.Time, span Span) int {
	first := func(at time.Time) int {
		return sort.Search(len(moments), func(i int) bool { return !moments[i].Before(at) })
	}
	return first(span.To) - first(span.From)
}

// countVerdict is R11's daily test of a day's count of refusals or kills: on a day with at least
// dailyCountFloor recorded, the model within 30% of the record (gapThirty); on each other day, at
// most dailyCountFloor.
func countVerdict(recorded, modelled float64) string {
	if recorded >= dailyCountFloor {
		return passWord(gapThirty.Within(recorded, modelled))
	}
	return passWord(modelled <= dailyCountFloor+tolerance(dailyCountFloor, modelled))
}

// modelFit is what each load model is fitted from: the window, its issues in flight
// (inFlightSteps), and the days of its span.
type modelFit struct {
	w      Window
	flight stretchCount
	days   []Span
}

// inFlight is the average count of issues in flight over [from, to), 0 for an empty span.
func (f modelFit) inFlight(from, to time.Time) float64 {
	if !to.After(from) {
		return 0
	}
	return f.flight.hours(from, to) / to.Sub(from).Hours()
}

// minute is the part of the window's span the minute from start covers.
func (f modelFit) minute(start time.Time) (time.Time, time.Time) {
	return maxTime(start, f.w.From), minTime(start.Add(time.Minute), f.w.To)
}

// day is span as a day of a model, with its issues in flight and no verdict yet.
func (f modelFit) day(span Span) ModelDay {
	return ModelDay{Day: span.From.UTC().Format(time.DateOnly), From: span.From, To: span.To, InFlight: f.inFlight(span.From, span.To)}
}

// newModel is the model name of signal in form, fitted on nothing yet.
func newModel(name, signal, form string) LoadModel {
	return LoadModel{Name: name, Signal: signal, Form: form, Parameters: []ModelParameter{}, Skipped: []SkippedSpan{}, Days: []ModelDay{}}
}

// recordOf is the spans of the window's record of signal inside its span, in order, each part of
// the span outside them added to m's skipped spans for reason.
func (f modelFit) recordOf(signal string, m *LoadModel, reason string) []Span {
	record := clipSpans(f.w.Signals[signal], Span{From: f.w.From, To: f.w.To})
	at := f.w.From
	for _, span := range record {
		if span.From.After(at) {
			m.skip(at, span.From, reason)
		}
		at = span.To
	}
	if f.w.To.After(at) {
		m.skip(at, f.w.To, reason)
	}
	return record
}

// unfitted is m not fitted, for reason: it gives no test, and each day gives none, for unrecorded
// where record holds no part of it and for reason otherwise.
func (f modelFit) unfitted(m LoadModel, record []Span, reason, unrecorded string) LoadModel {
	for _, span := range f.days {
		d := f.day(span)
		d.Verdict, d.Reason = VerdictNoTest, reason
		if parts := clipSpans(record, span); len(parts) == 0 {
			d.Reason = unrecorded
		} else {
			d.RecordHours = spansHours(parts)
		}
		m.Days = append(m.Days, d)
	}
	m.Verdict, m.Reason = VerdictNoTest, reason
	return m
}

// skip adds [from, to) to m's skipped spans for reason, joined to the last one where it goes on
// from it for the same reason.
func (m *LoadModel) skip(from, to time.Time, reason string) {
	if n := len(m.Skipped); n > 0 && m.Skipped[n-1].Reason == reason && m.Skipped[n-1].To.Equal(from) {
		m.Skipped[n-1].To = to
		return
	}
	m.Skipped = append(m.Skipped, SkippedSpan{From: from, To: to, Reason: reason})
}

// judge gives m, fitted, its verdict from its days: it fails where a day fails, gives no test, for
// untested, where no day is tested, and passes otherwise; it gives a limit for its signal where it
// passes.
func (m *LoadModel) judge(untested string) {
	m.Fitted, m.Verdict = true, VerdictNoTest
	for _, day := range m.Days {
		switch {
		case day.Verdict == VerdictFails:
			m.Verdict = VerdictFails
		case day.Verdict == VerdictPasses && m.Verdict == VerdictNoTest:
			m.Verdict = VerdictPasses
		}
	}
	if m.Verdict == VerdictNoTest {
		m.Reason = untested
	}
	m.GivesLimit = m.Verdict == VerdictPasses
}

// noTest is d giving no test, for reason.
func (d *ModelDay) noTest(reason string) {
	d.Verdict, d.Reason = VerdictNoTest, reason
}

// tested is d tested on recorded and modelled, with verdict.
func (d *ModelDay) tested(recorded, modelled float64, verdict string) {
	d.Recorded, d.Modelled, d.Verdict = &recorded, &modelled, verdict
}

// tokenMinute is one minute of the provider's record as the tokens model reads it: the part of the
// window's span it covers, its line, the issues in flight over it, and the project's own limit in
// force in it where the window holds a record of the provider's headroom there.
type tokenMinute struct {
	from, to time.Time
	line     ProviderMinute
	inFlight float64
	limit    int64
	limited  bool
}

// tokens is the tokens model fitted on the window's record (tokensForm): each minute of the
// provider's record gives one draw of the tokens per issue in flight, its uncached input tokens over
// its issues in flight, a minute with none in flight giving none; a minute the record does not hold
// is skipped. A day's modelled refusals by the project's own limit are, over its minutes with a
// record of the headroom (headroomLimit), the share of the draws above the limit over the minute's
// issues in flight times the record's own refusals per refused minute; its recorded refusals are
// the record's own of the same minutes. A minute with no limit of the project's own is fitted and
// tests nothing (the architect's D13).
func (f modelFit) tokens() LoadModel {
	m := newModel(ModelTokens, SignalProvider, tokensForm)
	held := make(map[int64]ProviderMinute, len(f.w.Provider))
	for _, line := range f.w.Provider {
		held[line.Minute.Unix()] = line
	}
	var read []tokenMinute
	var draws []float64
	var own, refused int64
	for start := f.w.From.Truncate(time.Minute); start.Before(f.w.To); start = start.Add(time.Minute) {
		from, to := f.minute(start)
		line, ok := held[start.Unix()]
		if !ok {
			m.skip(from, to, reasonNoProvider)
			continue
		}
		minute := tokenMinute{from: from, to: to, line: line, inFlight: f.inFlight(from, to)}
		if minute.limit, minute.limited = f.headroomLimit(line); !minute.limited {
			m.skip(from, to, reasonNoLimit)
		}
		read = append(read, minute)
		if minute.inFlight > 0 {
			draws = append(draws, float64(line.Uncached)/minute.inFlight)
		}
		if line.Own > 0 {
			own, refused = own+line.Own, refused+1
		}
	}
	if len(read) == 0 {
		return f.unfitted(m, nil, reasonNoProviderSpan, reasonNoProvider)
	}
	slices.Sort(draws)
	mean, perRefused := 0.0, 1.0
	for _, draw := range draws {
		mean += draw
	}
	if len(draws) > 0 {
		mean /= float64(len(draws))
	}
	if refused > 0 {
		perRefused = float64(own) / float64(refused)
	}
	// above is the share of the draws above threshold.
	above := func(threshold float64) float64 {
		return float64(len(draws)-sort.Search(len(draws), func(i int) bool { return draws[i] > threshold })) / float64(len(draws))
	}
	next := 0
	for _, span := range f.days {
		d := f.day(span)
		var minutes int
		var tokens, modelledTokens, recorded, refusedShare float64
		var providerRefusals int64
		var limited time.Duration
		for ; next < len(read) && read[next].from.Before(span.To); next++ {
			minute := read[next]
			minutes++
			tokens += float64(minute.line.Uncached)
			modelledTokens += mean * minute.inFlight
			providerRefusals += minute.line.Provider
			if !minute.limited {
				continue
			}
			limited += minute.to.Sub(minute.from)
			recorded += float64(minute.line.Own)
			if minute.inFlight > 0 && len(draws) > 0 {
				refusedShare += above(float64(minute.limit) / minute.inFlight)
			}
		}
		switch {
		case minutes == 0:
			d.noTest(reasonNoProvider)
		case len(draws) == 0:
			d.noTest(reasonNoFlight)
		default:
			recordedTokens, modelled := tokens/float64(minutes), modelledTokens/float64(minutes)
			d.LoadRecorded, d.LoadModelled, d.ProviderRefusals = &recordedTokens, &modelled, &providerRefusals
			if limited == 0 {
				d.noTest(reasonNoLimit)
				break
			}
			d.RecordHours = limited.Hours()
			modelledRefusals := perRefused * refusedShare
			d.tested(recorded, modelledRefusals, countVerdict(recorded, modelledRefusals))
		}
		m.Days = append(m.Days, d)
	}
	if len(draws) == 0 {
		m.Verdict, m.Reason = VerdictNoTest, reasonNoFlight
		return m
	}
	m.Parameters = []ModelParameter{
		{Name: paramMinutesFitted, Value: float64(len(draws))},
		{Name: paramTokensMean, Value: mean},
		{Name: paramTokensMedian, Value: sortedMedian(draws)},
		{Name: paramTokensP90, Value: nearestRank(draws, 0.9)},
		{Name: paramTokensP99, Value: nearestRank(draws, 0.99)},
		{Name: paramRefusedMinutes, Value: float64(refused)},
		{Name: paramRefusalsPerMinute, Value: perRefused},
	}
	m.judge(reasonNoLimitSpan)
	return m
}

// headroomLimit is the project's own limit in force in line's minute where the window holds a
// record of the provider's headroom there, as providerAt reads it, and whether it does.
func (f modelFit) headroomLimit(line ProviderMinute) (int64, bool) {
	limit, ok := line.projectLimit()
	return limit, ok && f.w.Recorded(SignalProvider, line.Minute)
}

// memory is the memory model fitted on the window's record where it holds the memory guard's kills
// (memoryForm): the kills of the launcher's lines over the issue-hours in flight there, and the
// pods' memory of each minute of the memory's record there over its issues in flight. A day's
// modelled kills are the rate times its issue-hours in flight in the record, its recorded kills
// those of the launcher's lines in the same spans.
func (f modelFit) memory(kills []GuardKill) LoadModel {
	m := newModel(ModelMemory, SignalKills, memoryForm)
	record := f.recordOf(SignalKills, &m, reasonNoKills)
	if len(record) == 0 {
		return f.unfitted(m, record, reasonNoKillsSpan, reasonNoKills)
	}
	moments := make([]time.Time, 0, len(kills))
	for _, kill := range kills {
		moments = append(moments, kill.At)
	}
	count, hours := 0, 0.0
	for _, span := range record {
		count += countWithin(moments, span)
		hours += f.flight.hours(span.From, span.To)
	}
	if hours == 0 {
		return f.unfitted(m, record, reasonNoFlight, reasonNoKills)
	}
	rate := float64(count) / hours
	used := make(map[int64]float64)
	for _, sample := range f.w.Memory {
		if !sample.Minute.Before(f.w.From) && sample.Minute.Before(f.w.To) && f.w.Recorded(SignalKills, sample.Minute) {
			used[sample.Minute.Unix()] += float64(sample.Bytes)
		}
	}
	memoryMinutes := slices.Sorted(maps.Keys(used))
	flights := make([]float64, len(memoryMinutes))
	var bytes, issues float64
	for i, minute := range memoryMinutes {
		flights[i] = f.inFlight(f.minute(time.Unix(minute, 0)))
		bytes, issues = bytes+used[minute], issues+flights[i]
	}
	m.Parameters = []ModelParameter{{Name: paramKillsRecorded, Value: float64(count)}, {Name: paramIssueHours, Value: hours},
		{Name: paramKillRate, Value: rate}, {Name: paramMemoryMinutes, Value: float64(len(memoryMinutes))}}
	perIssue, memoryFitted := 0.0, issues > 0
	if memoryFitted {
		perIssue = bytes / issues
		m.Parameters = append(m.Parameters, ModelParameter{Name: paramBytesPerIssue, Value: perIssue})
	}
	next := 0
	for _, span := range f.days {
		d := f.day(span)
		parts := clipSpans(record, span)
		if len(parts) == 0 {
			d.noTest(reasonNoKills)
		} else {
			recorded, inFlight := 0, 0.0
			for _, part := range parts {
				recorded += countWithin(moments, part)
				inFlight += f.flight.hours(part.From, part.To)
			}
			d.RecordHours = spansHours(parts)
			d.tested(float64(recorded), rate*inFlight, countVerdict(float64(recorded), rate*inFlight))
		}
		var minutes int
		var dayBytes, dayIssues float64
		for ; next < len(memoryMinutes) && time.Unix(memoryMinutes[next], 0).Before(span.To); next++ {
			minutes++
			dayBytes, dayIssues = dayBytes+used[memoryMinutes[next]], dayIssues+flights[next]
		}
		if memoryFitted && minutes > 0 {
			recordedBytes, modelledBytes := dayBytes/float64(minutes), perIssue*dayIssues/float64(minutes)
			d.LoadRecorded, d.LoadModelled = &recordedBytes, &modelledBytes
		}
		m.Days = append(m.Days, d)
	}
	m.judge(reasonNoKillsSpan)
	return m
}

// queueExit is one wait of a pull request that left the merge queue, as the merge queue's model
// reads it: when it left, its length in hours, and the issues in flight when it entered.
type queueExit struct {
	left            time.Time
	hours, inFlight float64
}

// queue is the merge queue's model fitted on the window's record where it holds the queue's
// (queueForm): each wait that left the queue in the record gives one draw of the wait per issue in
// flight, its hours over the issues in flight when it entered, a wait that entered with none in
// flight giving none. A day's recorded median wait is that of the waits that left the queue in its
// record, and its modelled median the median, over those waits and every draw, of the wait's issues
// in flight times the draw.
func (f modelFit) queue(waits []queueWait) LoadModel {
	m := newModel(ModelQueue, SignalQueue, queueForm)
	record := f.recordOf(SignalQueue, &m, reasonNoQueue)
	if len(record) == 0 {
		return f.unfitted(m, record, reasonNoQueueSpan, reasonNoQueue)
	}
	var exits []queueExit
	var draws []float64
	for _, wait := range waits {
		if wait.left.IsZero() || wait.left.Before(f.w.From) || !wait.left.Before(f.w.To) || !f.w.Recorded(SignalQueue, wait.left) {
			continue
		}
		exit := queueExit{left: wait.left, hours: wait.left.Sub(wait.added).Hours(), inFlight: float64(f.flight.at(wait.added))}
		exits = append(exits, exit)
		if exit.inFlight > 0 {
			draws = append(draws, exit.hours/exit.inFlight)
		}
	}
	if len(draws) == 0 {
		reason := reasonNoExitSpan
		if len(exits) > 0 {
			reason = reasonNoFlight
		}
		return f.unfitted(m, record, reason, reasonNoQueue)
	}
	slices.Sort(draws)
	slices.SortStableFunc(exits, func(a, b queueExit) int { return a.left.Compare(b.left) })
	m.Parameters = []ModelParameter{{Name: paramWaitsFitted, Value: float64(len(draws))}, {Name: paramWaitMedian, Value: sortedMedian(draws)},
		{Name: paramWaitP90, Value: nearestRank(draws, 0.9)}}
	next := 0
	for _, span := range f.days {
		d := f.day(span)
		var recorded, modelled []float64
		for ; next < len(exits) && exits[next].left.Before(span.To); next++ {
			recorded = append(recorded, exits[next].hours)
			for _, draw := range draws {
				modelled = append(modelled, exits[next].inFlight*draw)
			}
		}
		parts := clipSpans(record, span)
		d.RecordHours = spansHours(parts)
		switch {
		case len(parts) == 0:
			d.noTest(reasonNoQueue)
		case len(recorded) == 0:
			d.noTest(reasonNoExit)
		default:
			slices.Sort(recorded)
			slices.Sort(modelled)
			recordedMedian, modelledMedian := sortedMedian(recorded), sortedMedian(modelled)
			d.tested(recordedMedian, modelledMedian, passWord(gapThirty.Within(recordedMedian, modelledMedian)))
		}
		m.Days = append(m.Days, d)
	}
	m.judge(reasonNoExitSpan)
	return m
}

// Write prints m as Markdown: what the models read and R11's daily test, then for each model its
// form, its parameters, its verdict and whether it gives a limit for its signal, each span it skips
// with the reason, and a table of its days: the issues in flight, the hours of record its test
// reads, the recorded and the modelled value with the test, and the load beside it.
func (m LoadModels) Write(w io.Writer) error {
	p := &printer{w: w}
	p.line("# The load models of %s from %s to %s", m.Project, stamp(m.From), stamp(m.To))
	p.line("")
	p.line("Each model is fitted on the window's record alone, with no replay, as a function of the work in flight: the issues in flight at each moment, as the scorecard counts them (`inFlight`).")
	p.line("R11's daily test: on a day with at least %d refusals or kills recorded, the model gives within %s of the record, and on each other day at most %d; "+
		"the merge queue's model gives its median wait within %s of the record on each day.", dailyCountFloor, gapThirty, dailyCountFloor, gapThirty)
	p.line("A model is fitted and tested only where the window holds its record (assumption 47), and the spans it skips are named with the reason.")
	for _, model := range m.Models {
		p.line("")
		p.line("## Model `%s`, of the signal `%s`: %s", model.Name, model.Signal, model.Verdict)
		p.line("")
		p.line("Form: %s.", model.Form)
		if len(model.Parameters) > 0 {
			p.line("")
			p.line("Parameters:")
			p.line("")
			for _, parameter := range model.Parameters {
				p.line("- %s: %s", parameter.Name, formatNumber(parameter.Value))
			}
		}
		p.line("")
		switch model.Verdict {
		case VerdictPasses:
			p.line("The model passes R11's daily test on each day it is tested, so it gives a limit for the signal `%s`.", model.Signal)
		case VerdictFails:
			p.line("The model fails R11's daily test, so it gives no limit for the signal `%s`.", model.Signal)
		default:
			p.line("The model gives no test: %s, so it gives no limit for the signal `%s`.", model.Reason, model.Signal)
		}
		p.line("")
		if len(model.Skipped) == 0 {
			p.line("The model reads its record over the whole span.")
		} else {
			p.line("The spans it skips:")
			p.line("")
			for _, span := range model.Skipped {
				p.line("- from %s to %s: %s", stamp(span.From), stamp(span.To), span.Reason)
			}
		}
		p.line("")
		writeModelDays(p, model)
	}
	return p.err
}

// gibibyte is the bytes of a GiB, the unit the output gives the pods' memory in.
const gibibyte = 1 << 30

// writeModelDays prints model's table of days (LoadModels.Write).
func writeModelDays(p *printer, model LoadModel) {
	head := map[string]string{
		ModelTokens: "own refusals: recorded | modelled | test | tokens a minute: recorded | modelled | the provider's refusals, kept as recorded",
		ModelMemory: "kills: recorded | modelled | test | the pods' memory in GiB: recorded | modelled",
		ModelQueue:  "median wait in hours: recorded | modelled | test",
	}[model.Name]
	columns := 6
	if model.Name != ModelQueue {
		columns = 8
	}
	if model.Name == ModelTokens {
		columns = 9
	}
	p.line("| day | in flight | hours of record tested | %s |", head)
	rule := "|"
	for range columns {
		rule += " --- |"
	}
	p.line("%s", rule)
	for _, d := range model.Days {
		test := d.Verdict
		if d.Verdict == VerdictNoTest {
			test = VerdictNoTest + ": " + d.Reason
		}
		row := fmt.Sprintf("| %s | %s | %s | %s | %s | %s |", d.Day, formatNumber(d.InFlight), formatNumber(d.RecordHours),
			optionalNumber(d.Recorded, 1), optionalNumber(d.Modelled, 1), test)
		switch model.Name {
		case ModelTokens:
			refusals := "–"
			if d.ProviderRefusals != nil {
				refusals = fmt.Sprint(*d.ProviderRefusals)
			}
			row += fmt.Sprintf(" %s | %s | %s |", optionalNumber(d.LoadRecorded, 1), optionalNumber(d.LoadModelled, 1), refusals)
		case ModelMemory:
			row += fmt.Sprintf(" %s | %s |", optionalNumber(d.LoadRecorded, gibibyte), optionalNumber(d.LoadModelled, gibibyte))
		}
		p.line("%s", row)
	}
}

// optionalNumber is value over unit as a report prints it, "–" for none.
func optionalNumber(value *float64, unit float64) string {
	if value == nil {
		return "–"
	}
	return formatNumber(*value / unit)
}
