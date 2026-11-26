package replay

import (
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"time"
)

// The load models of R11 (spec R11): a replay keeps each work segment's recorded length, so it
// cannot show a refusal, a memory kill or a longer merge queue that more work in flight would have
// caused, and for those three signals a replay under another rule takes the signal from a model of
// the window that keeps what the window records and moves with the work in flight in the replay
// (assumption 45). FitLoadModels fits the three on a window's record, with no replay, and tests
// each by R11's daily test at the recorded work in flight; CompareRecords adds R11's test across
// two records of the rate each model rests on (model_rates.go); and LoadModels.Replay gives each
// modelled signal at a moment of a replay (model_replay.go). A model gives a limit for its signal
// only where it passes each of the two tests that runs. No model reads the pods' memory series.
// The docs page gives each model's form and parameters (replay.md, "legion replay models").

// The names of the three load models of R11.
const (
	ModelTokens = "tokens"
	ModelMemory = "memory"
	ModelQueue  = "queue"
)

// The verdicts of a load model and of each of its days under R11's tests. The first two are the
// words passWord gives; VerdictNotRun is the test across two records where none was given.
const (
	VerdictPasses = "passes"
	VerdictFails  = "fails"
	VerdictNoTest = "no test"
	VerdictNotRun = "not run"
)

// dailyCountFloor is R11's count of a day's refusals or kills from which its daily test reads the
// model within 30% of the record (gapThirty); on a day with fewer recorded, the model passes where it
// gives at most this many.
const dailyCountFloor = 10

// The forms of the three load models, as the output and the docs page name them.
const (
	tokensForm = "a minute's uncached input tokens are the minute's recorded tokens for each issue in flight in the record times the issues in flight " +
		"in the replay, and a minute with no issue in flight in the record keeps its recorded tokens; a minute whose modelled tokens pass the " +
		"project's own limit in force is a refused minute, which counts the record's own refusals per refused minute (1 where the record holds " +
		"no refused minute); the provider's refusals are kept as recorded (assumption 46)"
	memoryForm = "each recorded kill of the memory guard belongs to the work segment of the issue whose pod it ended, at its offset in that " +
		"segment, and a replay places it at the same offset of its run of that segment; work that the record never ran takes the day's recorded " +
		"kills for each hour of an issue in flight, each drawn issue on its own: one kill each time its own issue-hours in flight times the day's " +
		"rate pass a whole number, so no other issue and no rule's timing moves its kills; a kill that the record cannot place in a segment of its " +
		"issue is counted apart and kept at its recorded time; no model reads the pods' memory"
	queueForm = "each repository has its own queue; a pull request that the record removed before it reached the head of its queue keeps its " +
		"recorded wait; each other pull request leaves in its order of entry, at the record's next exit from the head of its queue after its " +
		"entry; while the record's queue stood empty, and after the end of the queue's record, the queue serves at the window's own rate of " +
		"exits from its head for each hour in which it held a pull request; a recorded pull request enters at its recorded offset from the start " +
		"of the work segment of its issue in which the record shows its entry, and one that the record shows in no such segment is counted apart " +
		"and enters at its recorded time"
)

// The reasons of a model that gives no test, of a span it skips and of a day with no test.
const (
	reasonNoProviderSpan = "the window holds no record of the provider in its span"
	reasonNoKillsSpan    = "the window holds no record of the memory guard's kills in its span"
	reasonNoQueueSpan    = "the window holds no record of the merge queue in its span"
	reasonNoLimitSpan    = "no limit of the project's own is recorded in the span, which leaves the project's own refusals nothing to test"
	reasonNoExitSpan     = "no pull request left the merge queue in the window's record"
	reasonNoProvider     = "the window holds no record of the provider here, so the model is neither fitted nor tested there (assumption 47)"
	reasonNoLimit        = "no limit of the project's own is recorded here, so the model is fitted and the project's own refusals have nothing to test (assumption 48)"
	reasonNoKills        = "the window holds no record of the memory guard's kills here, so the model is neither fitted nor tested there (assumption 47)"
	reasonNoQueue        = "the window holds no record of the merge queue here, so the model is neither fitted nor tested there (assumption 47)"
	reasonNoExit         = "no pull request left the merge queue in the day's record"
	reasonNoModelledExit = "no pull request leaves the merge queue on the day in the model at the recorded work in flight"
)

// The names of the models' parameters, and of the rates the test across two records reads.
const (
	paramMinutesFitted     = "minutes fitted"
	paramTokensRate        = "uncached input tokens for each minute of an issue in flight"
	paramRefusedMinutes    = "refused minutes recorded"
	paramRefusalsPerMinute = "the project's own refusals per refused minute"
	paramKillsRecorded     = "kills recorded"
	paramKillsPlaced       = "kills placed in a work segment of their issue"
	paramKillsApart        = "kills counted apart"
	paramIssueHours        = "issue-hours in flight"
	paramKillRate          = "kills for each hour of an issue in flight"
	paramHeadExits         = "exits from the head of the queue"
	paramEarlyExits        = "pull requests removed before they reached the head"
	paramBusyHours         = "hours in which the queue held a pull request"
	paramExitRate          = "exits from the head for each hour in which the queue held a pull request"
	paramEntriesApart      = "pull requests counted apart"
)

// LoadModels is the three load models of R11 fitted on one window's record (FitLoadModels): the
// window's project and span, each model in the order tokens, memory, queue, and R11's test across
// two records where CompareRecords gave it (nil where it did not run). It holds the window's record
// the models read (fit), which LoadModels.Replay models a replay's signals from and a value read
// back from JSON lacks. `legion replay models --json` prints it, and the limit test reads it.
type LoadModels struct {
	Project string         `json:"project"`
	From    time.Time      `json:"from"`
	To      time.Time      `json:"to"`
	Models  []LoadModel    `json:"models"`
	Across  *AcrossRecords `json:"across"`

	fit *modelRecord
}

// LoadModel is one load model fitted on a window's record: its name, the signal of R11 it gives a
// replay (provider, kills, queue), its form, whether it is fitted and with which parameters; its
// verdict under R11's daily test (VerdictPasses, VerdictFails, VerdictNoTest with its reason), and,
// for the models of the kills and of the merge queue, whether it gives the record at the recorded
// work in flight on each day it tests, as its form does, so that the daily test shows only that
// it keeps the record; its verdict under R11's test across two records (VerdictNotRun where none
// was given, VerdictNoTest with its reason); whether it gives a limit for its signal (it passes the
// daily test and the test across records where that ran); each span of the window's span it skips
// with the reason; each day of the span; and its signal's rate in each range of issues in flight
// that the window records (LoadBand).
type LoadModel struct {
	Name         string           `json:"name"`
	Signal       string           `json:"signal"`
	Form         string           `json:"form"`
	Fitted       bool             `json:"fitted"`
	Parameters   []ModelParameter `json:"parameters"`
	Verdict      string           `json:"verdict"`
	Reason       string           `json:"reason,omitempty"`
	KeepsRecord  bool             `json:"keepsRecord"`
	Across       string           `json:"across"`
	AcrossReason string           `json:"acrossReason,omitempty"`
	GivesLimit   bool             `json:"givesLimit"`
	Skipped      []SkippedSpan    `json:"skipped"`
	Days         []ModelDay       `json:"days"`
	Load         []LoadBand       `json:"load"`
}

// ModelParameter is one fitted parameter of a load model, by its name, and for the merge queue's
// model the repository whose queue it is of.
type ModelParameter struct {
	Name       string  `json:"name"`
	Repository string  `json:"repository,omitempty"`
	Value      float64 `json:"value"`
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
// modelled value R11's daily test compares at the recorded work in flight, none on a day of no test
// (the tokens model's refused minutes, the minutes in which the project's own limit refused a
// request; the kill model's kills; the merge queue's median wait in hours); and the day's verdict,
// with its reason where it gives no test. Beside the test, and testing nothing: for the tokens
// model, its uncached input tokens a minute, recorded and modelled, a mean over the day's minutes
// of the provider's record, the project's own refusals, recorded and modelled, over the minutes
// the test reads, and the provider's refusals, kept as recorded (assumption 46); for the kill
// model, the kills the record cannot place in a segment of their issue, and the day's kills for
// each hour of an issue in flight, which work the record never ran takes.
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
	RefusalsRecorded *float64  `json:"refusalsRecorded,omitempty"`
	RefusalsModelled *float64  `json:"refusalsModelled,omitempty"`
	ProviderRefusals *int64    `json:"providerRefusals,omitempty"`
	KillsApart       *int      `json:"killsApart,omitempty"`
	KillRate         *float64  `json:"killRate,omitempty"`
}

// FitLoadModels fits the three load models of R11 on w's record alone, with no replay
// (newModelRecord), and tests each on each day of w's span by R11's daily test, the models'
// signals taken at the record's own timeline (LoadModels.RecordTimeline): the record's issues in
// flight at each moment as the scorecard counts them (inFlightSteps) and each work segment where
// the record started it. A model is fitted and tested only in the spans where w holds its record
// (plan Q4, assumption 47): the tokens model in the minutes of the provider's record, its test of
// the refused minutes in the minutes where w holds a record of the provider's headroom (a limit of
// the project's own, assumption 48); the kill model where w holds the record of the memory guard's
// kills; the merge queue's model where w holds the record of the queue. The test across two
// records does not run here (CompareRecords). It refuses a window whose trace does not build, and a
// line of the kills' or the queue's record their readers refuse.
func FitLoadModels(w Window) (LoadModels, error) {
	t, err := BuildTrace(w)
	if err != nil {
		return LoadModels{}, err
	}
	kills, err := launcherKills(w.Launcher)
	if err != nil {
		return LoadModels{}, err
	}
	events, err := queueEvents(w.Queue)
	if err != nil {
		return LoadModels{}, err
	}
	r := newModelRecord(w, t, inFlightSteps(lineTimeline(t, ranksOf(t), t.ruleAt)), kills, events)
	models := LoadModels{Project: w.Project, From: w.From, To: w.To, fit: r}
	at, err := models.Replay(r.recordTimeline())
	if err != nil {
		return LoadModels{}, err
	}
	models.Models = []LoadModel{r.tokens(at), r.memory(at), r.queue(at)}
	return models, nil
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
	total := 0.0
	c.pieces(from, to, func(from, to time.Time, count int) { total += float64(count) * to.Sub(from).Hours() })
	return total
}

// pieces calls each for each part of [from, to) over which c's count stands still, in order, with
// that count: none for an empty span.
func (c stretchCount) pieces(from, to time.Time, each func(from, to time.Time, count int)) {
	if !to.After(from) {
		return
	}
	i := upTo(c.from, from)
	standing, at := 0, from
	if i > 0 {
		standing = c.counts[i-1]
	}
	for ; i < len(c.from) && c.from[i].Before(to); i++ {
		each(at, c.from[i], standing)
		standing, at = c.counts[i], c.from[i]
	}
	each(at, to, standing)
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
	return len(momentsWithin(moments, span))
}

// momentsWithin is the moments, in time order, that stand in span.
func momentsWithin(moments []time.Time, span Span) []time.Time {
	first := func(at time.Time) int {
		return sort.Search(len(moments), func(i int) bool { return !moments[i].Before(at) })
	}
	return moments[first(span.From):first(span.To)]
}

// countVerdict is R11's daily test of a day's count of refused minutes or kills: on a day with at
// least dailyCountFloor recorded, the model within 30% of the record (gapThirty); on each other
// day, at most dailyCountFloor.
func countVerdict(recorded, modelled float64) string {
	if recorded >= dailyCountFloor {
		return passWord(gapThirty.Within(recorded, modelled))
	}
	return passWord(modelled <= dailyCountFloor+tolerance(dailyCountFloor, modelled))
}

// inFlight is the average count of issues in flight over [from, to), 0 for an empty span.
func (r *modelRecord) inFlight(from, to time.Time) float64 {
	if !to.After(from) {
		return 0
	}
	return r.flight.hours(from, to) / to.Sub(from).Hours()
}

// minute is the part of the window's span the minute from start covers.
func (r *modelRecord) minute(start time.Time) (time.Time, time.Time) {
	return maxTime(start, r.w.From), minTime(start.Add(time.Minute), r.w.To)
}

// day is span as a day of a model, with its issues in flight and no verdict yet.
func (r *modelRecord) day(span Span) ModelDay {
	return ModelDay{Day: span.From.UTC().Format(time.DateOnly), From: span.From, To: span.To, InFlight: r.inFlight(span.From, span.To)}
}

// newModel is the model name of signal in form, fitted on nothing yet, tested across no records.
func newModel(name, signal, form string) LoadModel {
	return LoadModel{Name: name, Signal: signal, Form: form, Parameters: []ModelParameter{}, Across: VerdictNotRun, Skipped: []SkippedSpan{},
		Days: []ModelDay{}, Load: []LoadBand{}}
}

// recordOf is the spans of the window's record of signal inside its span, in order, each part of
// the span outside them added to m's skipped spans for reason.
func (r *modelRecord) recordOf(signal string, m *LoadModel, reason string) []Span {
	record := clipSpans(r.w.Signals[signal], Span{From: r.w.From, To: r.w.To})
	at := r.w.From
	for _, span := range record {
		if span.From.After(at) {
			m.skip(at, span.From, reason)
		}
		at = span.To
	}
	if r.w.To.After(at) {
		m.skip(at, r.w.To, reason)
	}
	return record
}

// unfitted is m not fitted, for reason: it gives no test, and each day gives none, for unrecorded
// where record holds no part of it and for reason otherwise.
func (r *modelRecord) unfitted(m LoadModel, record []Span, reason, unrecorded string) LoadModel {
	for _, span := range r.days {
		d := r.day(span)
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
// passes (settle).
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
	m.settle()
}

// settle says whether m gives a limit for its signal: where it passes R11's daily test, and the
// test across two records where that ran.
func (m *LoadModel) settle() {
	m.GivesLimit = m.Verdict == VerdictPasses && (m.Across == VerdictNotRun || m.Across == VerdictPasses)
}

// keepsRecord says whether each day of days that R11's daily test reads gives the record's value,
// and some day does: the models of the kills and of the merge queue give the record by their form
// at the recorded work in flight.
func keepsRecord(days []ModelDay) bool {
	tested := false
	for _, d := range days {
		if d.Verdict == VerdictNoTest {
			continue
		}
		if d.Recorded == nil || d.Modelled == nil || math.Abs(*d.Recorded-*d.Modelled) > tolerance(*d.Recorded, *d.Modelled) {
			return false
		}
		tested = true
	}
	return tested
}

// noTest is d giving no test, for reason.
func (d *ModelDay) noTest(reason string) {
	d.Verdict, d.Reason = VerdictNoTest, reason
}

// tested is d tested on recorded and modelled, with verdict.
func (d *ModelDay) tested(recorded, modelled float64, verdict string) {
	d.Recorded, d.Modelled, d.Verdict = &recorded, &modelled, verdict
}

// tokenMinute is one minute of the provider's record as the tokens model's test reads it: the part
// of the window's span it covers, its line, and the project's own limit in force in it where the
// window holds a record of the provider's headroom there.
type tokenMinute struct {
	from, to time.Time
	line     ProviderMinute
	limit    int64
	limited  bool
}

// tokens is the tokens model on the window's record (tokensForm), its days tested at the record's
// own timeline (at): a minute the provider's record does not hold is skipped. A day's recorded
// refused minutes are its minutes, among those with a record of the headroom (headroomLimit), in
// which the project's own limit refused a request, and its modelled ones those whose modelled
// tokens pass that limit. A minute with no limit of the project's own is fitted and tests nothing
// (the architect's D13).
func (r *modelRecord) tokens(at *ModelledSignals) LoadModel {
	m := newModel(ModelTokens, SignalProvider, tokensForm)
	var read []tokenMinute
	for start := r.w.From.Truncate(time.Minute); start.Before(r.w.To); start = start.Add(time.Minute) {
		from, to := r.minute(start)
		line, ok := r.minutes[start.Unix()]
		if !ok {
			m.skip(from, to, reasonNoProvider)
			continue
		}
		minute := tokenMinute{from: from, to: to, line: line}
		if minute.limit, minute.limited = r.headroomLimit(line); !minute.limited {
			m.skip(from, to, reasonNoLimit)
		}
		read = append(read, minute)
	}
	if len(read) == 0 {
		return r.unfitted(m, nil, reasonNoProviderSpan, reasonNoProvider)
	}
	next := 0
	for _, span := range r.days {
		d := r.day(span)
		var minutes int
		var tokens, modelledTokens, ownRecorded, ownModelled, refusedRecorded, refusedModelled float64
		var providerRefusals int64
		var limited time.Duration
		for ; next < len(read) && read[next].from.Before(span.To); next++ {
			minute := read[next]
			key := minute.line.Minute.Unix()
			modelled := at.minutes[key]
			minutes++
			tokens += float64(minute.line.Uncached)
			modelledTokens += float64(modelled.Uncached)
			providerRefusals += minute.line.Provider
			if !minute.limited {
				continue
			}
			limited += minute.to.Sub(minute.from)
			ownRecorded += float64(minute.line.Own)
			ownModelled += float64(modelled.Own)
			if minute.line.Own > 0 {
				refusedRecorded++
			}
			if at.refused[key] {
				refusedModelled++
			}
		}
		if minutes == 0 {
			d.noTest(reasonNoProvider)
			m.Days = append(m.Days, d)
			continue
		}
		recordedTokens, modelledMean := tokens/float64(minutes), modelledTokens/float64(minutes)
		d.LoadRecorded, d.LoadModelled, d.ProviderRefusals = &recordedTokens, &modelledMean, &providerRefusals
		if limited == 0 {
			d.noTest(reasonNoLimit)
		} else {
			d.RecordHours = limited.Hours()
			d.RefusalsRecorded, d.RefusalsModelled = &ownRecorded, &ownModelled
			d.tested(refusedRecorded, refusedModelled, countVerdict(refusedRecorded, refusedModelled))
		}
		m.Days = append(m.Days, d)
	}
	m.Parameters = append(m.Parameters, ModelParameter{Name: paramMinutesFitted, Value: float64(len(read))})
	if rate := r.tokensRate(); rate.Rate != nil {
		m.Parameters = append(m.Parameters, ModelParameter{Name: paramTokensRate, Value: *rate.Rate})
	}
	m.Parameters = append(m.Parameters, ModelParameter{Name: paramRefusedMinutes, Value: float64(r.refusedMinutes)},
		ModelParameter{Name: paramRefusalsPerMinute, Value: r.rho})
	m.judge(reasonNoLimitSpan)
	m.Load = r.tokenBands()
	return m
}

// memory is the kill model on the window's record where it holds the memory guard's kills
// (memoryForm), its days tested at the record's own timeline (at): a day's recorded kills are the
// launcher's lines' in the record's spans of it, and its modelled kills the model's in the same
// spans. Beside them, each day gives the kills the record cannot place in a segment of their issue
// and its kills for each hour of an issue in flight, which work the record never ran takes.
func (r *modelRecord) memory(at *ModelledSignals) LoadModel {
	m := newModel(ModelMemory, SignalKills, memoryForm)
	record := r.recordOf(SignalKills, &m, reasonNoKills)
	if len(record) == 0 {
		return r.unfitted(m, record, reasonNoKillsSpan, reasonNoKills)
	}
	recorded, apart := r.killMoments()
	count, apartCount := 0, 0
	for _, span := range record {
		count += countWithin(recorded, span)
		apartCount += countWithin(apart, span)
	}
	rate := r.killsRate(r.w.From)
	m.Parameters = append(m.Parameters, ModelParameter{Name: paramKillsRecorded, Value: float64(count)},
		ModelParameter{Name: paramKillsPlaced, Value: float64(count - apartCount)}, ModelParameter{Name: paramKillsApart, Value: float64(apartCount)},
		ModelParameter{Name: paramIssueHours, Value: rate.Exposure})
	if rate.Rate != nil {
		m.Parameters = append(m.Parameters, ModelParameter{Name: paramKillRate, Value: *rate.Rate})
	}
	for i, span := range r.days {
		d := r.day(span)
		day := r.dayRates[i]
		if len(day.parts) == 0 {
			d.noTest(reasonNoKills)
			m.Days = append(m.Days, d)
			continue
		}
		recordedKills, modelledKills, dayApart := 0, 0, 0
		for _, part := range day.parts {
			recordedKills += countWithin(recorded, part)
			modelledKills += countWithin(at.kills, part)
			dayApart += countWithin(apart, part)
		}
		d.RecordHours, d.KillsApart = spansHours(day.parts), &dayApart
		if dayRate, rated := day.rate(); rated {
			d.KillRate = &dayRate
		}
		d.tested(float64(recordedKills), float64(modelledKills), countVerdict(float64(recordedKills), float64(modelledKills)))
		m.Days = append(m.Days, d)
	}
	m.judge(reasonNoKillsSpan)
	m.KeepsRecord = keepsRecord(m.Days)
	m.Load = r.killBands(record)
	return m
}

// queueExit is one wait of a pull request that left the merge queue, as the merge queue's model's
// test reads it: when it left, and its length in hours.
type queueExit struct {
	left  time.Time
	hours float64
}

// queue is the merge queue's model on the window's record where it holds the queue's (queueForm),
// its days tested at the record's own timeline (at): a day's recorded median wait is that of the
// waits that left the queue in its record, and its modelled median that of the waits the model
// gives that leave the queue there.
func (r *modelRecord) queue(at *ModelledSignals) LoadModel {
	m := newModel(ModelQueue, SignalQueue, queueForm)
	record := r.recordOf(SignalQueue, &m, reasonNoQueue)
	if len(record) == 0 {
		return r.unfitted(m, record, reasonNoQueueSpan, reasonNoQueue)
	}
	var recorded, modelled []queueExit
	apart := 0
	for _, q := range r.queues {
		for _, pull := range q.pulls {
			if r.testsExit(pull.left) {
				recorded = append(recorded, queueExit{left: pull.left, hours: pull.left.Sub(pull.added).Hours()})
			}
			if !pull.placed {
				apart++
			}
		}
	}
	if len(recorded) == 0 {
		return r.unfitted(m, record, reasonNoExitSpan, reasonNoQueue)
	}
	for _, wait := range at.waits {
		if r.testsExit(wait.Left) {
			modelled = append(modelled, queueExit{left: wait.Left, hours: wait.Left.Sub(wait.Added).Hours()})
		}
	}
	byLeft := func(a, b queueExit) int { return a.left.Compare(b.left) }
	slices.SortStableFunc(recorded, byLeft)
	slices.SortStableFunc(modelled, byLeft)
	for _, q := range r.queues {
		m.Parameters = append(m.Parameters, q.parameters()...)
	}
	m.Parameters = append(m.Parameters, ModelParameter{Name: paramEntriesApart, Value: float64(apart)})
	nextRecorded, nextModelled := 0, 0
	for _, span := range r.days {
		d := r.day(span)
		var recordedHours, modelledHours []float64
		for ; nextRecorded < len(recorded) && recorded[nextRecorded].left.Before(span.To); nextRecorded++ {
			recordedHours = append(recordedHours, recorded[nextRecorded].hours)
		}
		for ; nextModelled < len(modelled) && modelled[nextModelled].left.Before(span.To); nextModelled++ {
			modelledHours = append(modelledHours, modelled[nextModelled].hours)
		}
		parts := clipSpans(record, span)
		d.RecordHours = spansHours(parts)
		slices.Sort(recordedHours)
		slices.Sort(modelledHours)
		switch {
		case len(parts) == 0:
			d.noTest(reasonNoQueue)
		case len(recordedHours) == 0:
			d.noTest(reasonNoExit)
		case len(modelledHours) == 0:
			recordedMedian := sortedMedian(recordedHours)
			d.Recorded, d.Verdict, d.Reason = &recordedMedian, VerdictFails, reasonNoModelledExit
		default:
			recordedMedian, modelledMedian := sortedMedian(recordedHours), sortedMedian(modelledHours)
			d.tested(recordedMedian, modelledMedian, passWord(gapThirty.Within(recordedMedian, modelledMedian)))
		}
		m.Days = append(m.Days, d)
	}
	m.judge(reasonNoExitSpan)
	m.KeepsRecord = keepsRecord(m.Days)
	m.Load = r.queueBands(record)
	return m
}

// testsExit says whether a wait that ended at left is one the merge queue's daily test reads: one
// that left in the window's span where its record holds the queue.
func (r *modelRecord) testsExit(left time.Time) bool {
	return !left.IsZero() && !left.Before(r.w.From) && left.Before(r.w.To) && r.w.Recorded(SignalQueue, left)
}

// Write prints m as Markdown: what the models read and R11's tests, then for each model its form,
// its parameters, its verdicts and whether it gives a limit for its signal, each span it skips with
// the reason, a table of its days (the issues in flight, the hours of record its test reads, the
// recorded and the modelled value with the test, and what it gives beside them) and its signal's
// rate in each range of 20 issues in flight that the window records; and, where it ran, R11's test
// across two records: each rate on each record, their ratio and the test, and each repository that
// has no test.
func (m LoadModels) Write(w io.Writer) error {
	p := &printer{w: w}
	p.line("# The load models of %s from %s to %s", m.Project, stamp(m.From), stamp(m.To))
	p.line("")
	p.line("Each model keeps what the window records and moves with the work in flight of a replay, as its form says, and no model reads the pods' memory series.")
	p.line("The work in flight is the record's issues in flight at each moment, as the scorecard counts them (`inFlight`).")
	p.line("R11's daily test, at the recorded work in flight: on a day with at least %d refused minutes or kills recorded, the model gives within %s of the record, and on each other day at most %d; "+
		"a refused minute is one in which the project's own limit refused a request; the merge queue's model gives its median wait within %s of the record on each day.",
		dailyCountFloor, gapThirty, dailyCountFloor, gapThirty)
	if m.Across == nil {
		p.line("R11's test across two records did not run, so a model that passes the daily test gives its limit on that test alone.")
	} else {
		p.line("R11's test across two records compares the rate that each model rests on with the same rate on the record of %s from %s to %s, in the last section.",
			m.Across.Project, stamp(m.Across.From), stamp(m.Across.To))
	}
	p.line("A model gives a limit for its signal only where it passes each test that runs.")
	p.line("A model is fitted and tested only where the window holds its record (assumption 47), and the spans it skips are named with the reason.")
	for _, model := range m.Models {
		writeModel(p, model)
	}
	if m.Across != nil {
		m.Across.write(p)
	}
	return p.err
}

// writeModel prints model's section (LoadModels.Write).
func writeModel(p *printer, model LoadModel) {
	p.line("")
	p.line("## Model `%s`, of the signal `%s`: %s", model.Name, model.Signal, model.Verdict)
	p.line("")
	p.line("Form: %s.", model.Form)
	if len(model.Parameters) > 0 {
		p.line("")
		p.line("Parameters:")
		p.line("")
		for _, parameter := range model.Parameters {
			if parameter.Repository != "" {
				p.line("- %s, %s: %s", parameter.Repository, parameter.Name, formatNumber(parameter.Value))
				continue
			}
			p.line("- %s: %s", parameter.Name, formatNumber(parameter.Value))
		}
	}
	p.line("")
	switch model.Verdict {
	case VerdictPasses:
		p.line("The model passes R11's daily test on each day it is tested.")
	case VerdictFails:
		p.line("The model fails R11's daily test.")
	default:
		p.line("The model gives no daily test: %s.", model.Reason)
	}
	if model.Name != ModelTokens && model.Verdict != VerdictNoTest {
		if model.KeepsRecord {
			p.line("At the recorded work in flight the model gives the record by its form, so R11's daily test shows only that it keeps the record.")
		} else {
			p.line("At the recorded work in flight the model does not give the record on each day it tests, as its form should.")
		}
	}
	switch model.Across {
	case VerdictNotRun:
		p.line("R11's test across two records did not run.")
	case VerdictPasses:
		p.line("The rate it rests on passes R11's test across two records.")
	case VerdictFails:
		p.line("The rate it rests on fails R11's test across two records.")
	default:
		p.line("R11's test across two records gives it no test: %s.", model.AcrossReason)
	}
	switch {
	case model.GivesLimit && model.Across == VerdictNotRun:
		p.line("So it gives a limit for the signal `%s`, on R11's daily test alone.", model.Signal)
	case model.GivesLimit:
		p.line("So it gives a limit for the signal `%s`.", model.Signal)
	default:
		p.line("So it gives no limit for the signal `%s`.", model.Signal)
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
	p.line("")
	writeLoadBands(p, model)
}

// writeModelDays prints model's table of days (LoadModels.Write).
func writeModelDays(p *printer, model LoadModel) {
	head := map[string]string{
		ModelTokens: "refused minutes: recorded | modelled | test | own refusals: recorded | modelled | tokens a minute: recorded | modelled | " +
			"the provider's refusals, kept as recorded",
		ModelMemory: "kills: recorded | modelled | test | kills apart | the day's kills for each hour of an issue in flight",
		ModelQueue:  "median wait in hours: recorded | modelled | test",
	}[model.Name]
	columns := map[string]int{ModelTokens: 11, ModelMemory: 8, ModelQueue: 6}[model.Name]
	p.line("| day | in flight | hours of record tested | %s |", head)
	rule := "|"
	for range columns {
		rule += " --- |"
	}
	p.line("%s", rule)
	for _, d := range model.Days {
		test := d.Verdict
		if d.Reason != "" {
			test += ": " + d.Reason
		}
		row := fmt.Sprintf("| %s | %s | %s | %s | %s | %s |", d.Day, formatNumber(d.InFlight), formatNumber(d.RecordHours),
			optionalNumber(d.Recorded), optionalNumber(d.Modelled), test)
		switch model.Name {
		case ModelTokens:
			refusals := "–"
			if d.ProviderRefusals != nil {
				refusals = fmt.Sprint(*d.ProviderRefusals)
			}
			row += fmt.Sprintf(" %s | %s | %s | %s | %s |", optionalNumber(d.RefusalsRecorded), optionalNumber(d.RefusalsModelled),
				optionalNumber(d.LoadRecorded), optionalNumber(d.LoadModelled), refusals)
		case ModelMemory:
			apart := "–"
			if d.KillsApart != nil {
				apart = fmt.Sprint(*d.KillsApart)
			}
			row += fmt.Sprintf(" %s | %s |", apart, optionalNumber(d.KillRate))
		}
		p.line("%s", row)
	}
}

// optionalNumber is value as a report prints it, "–" for none.
func optionalNumber(value *float64) string {
	if value == nil {
		return "–"
	}
	return formatNumber(*value)
}
