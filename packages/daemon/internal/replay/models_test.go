package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/signals"
)

// fittedModels is the load models of w, which must fit.
func fittedModels(t *testing.T, w Window) LoadModels {
	t.Helper()
	models, err := FitLoadModels(w)
	if err != nil {
		t.Fatalf("FitLoadModels: %v", err)
	}
	return models
}

// modelNamed is the model of models named name, which it must hold.
func modelNamed(t *testing.T, models LoadModels, name string) LoadModel {
	t.Helper()
	for _, model := range models.Models {
		if model.Name == name {
			return model
		}
	}
	t.Fatalf("the load models of the window hold no model %q: %+v", name, models.Models)
	return LoadModel{}
}

// parameterOf is model's parameter named name, which it must hold.
func parameterOf(t *testing.T, model LoadModel, name string) float64 {
	t.Helper()
	for _, parameter := range model.Parameters {
		if parameter.Name == name {
			return parameter.Value
		}
	}
	t.Errorf("the model %s names no parameter %q: %+v", model.Name, name, model.Parameters)
	return 0
}

// assertNearValue fails where got is none, or stands farther from want than a fixture's tolerance.
func assertNearValue[T int | int64 | float64](t *testing.T, name string, got *T, want float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s is none, want %v", name, want)
		return
	}
	assertNear(t, name, float64(*got), want)
}

// wantDay is a day of a model worked out by hand: the hours of it in which the test reads the
// record, the recorded and the modelled value of the test and its verdict, or, for a day of no
// test, its reason.
type wantDay struct {
	day                string
	hours              float64
	recorded, modelled float64
	verdict, reason    string
}

// checkDays fails for each day of model that differs from want, which names every day of the
// window in order.
func checkDays(t *testing.T, model LoadModel, want []wantDay) {
	t.Helper()
	if len(model.Days) != len(want) {
		t.Errorf("the model %s gives %d days, want %d: %+v", model.Name, len(model.Days), len(want), model.Days)
		return
	}
	for i, w := range want {
		got := model.Days[i]
		if got.Day != w.day || got.Verdict != w.verdict || got.Reason != w.reason {
			t.Errorf("the model %s on day %d is %s, %q (%q), want %s, %q (%q)", model.Name, i+1, got.Day, got.Verdict, got.Reason, w.day, w.verdict, w.reason)
			continue
		}
		assertNear(t, model.Name+" "+w.day+" hours of record tested", got.RecordHours, w.hours)
		if w.verdict == VerdictNoTest {
			if got.Recorded != nil || got.Modelled != nil {
				t.Errorf("the model %s on %s gives no test and yet a recorded %v and a modelled %v", model.Name, w.day, got.Recorded, got.Modelled)
			}
			continue
		}
		assertNearValue(t, model.Name+" "+w.day+" recorded", got.Recorded, w.recorded)
		assertNearValue(t, model.Name+" "+w.day+" modelled", got.Modelled, w.modelled)
	}
}

// checkDaysInFlight fails for each day of each model whose work in flight is not the scorecard's
// own reading of the issues in flight (Scorecard.InFlight) over that day of w's record, and where
// the issue-hours in flight over w's whole span are not the scorecard's.
func checkDaysInFlight(t *testing.T, w Window, models LoadModels) {
	t.Helper()
	trace := placedTrace(t, w)
	timeline := lineTimeline(trace, ranksOf(trace), trace.ruleAt)
	measured := newMeasuring(timeline)
	for _, model := range models.Models {
		for _, day := range model.Days {
			assertNear(t, model.Name+" "+day.Day+" in flight", day.InFlight, measured.span(day.From, day.To).InFlight.Average)
		}
	}
	assertNear(t, "issue-hours in flight over the span", inFlightSteps(timeline).hours(w.From, w.To), scoreTrace(trace).InFlight.Hours)
}

// printedModels is models as Write prints them.
func printedModels(t *testing.T, models LoadModels) string {
	t.Helper()
	var out bytes.Buffer
	if err := models.Write(&out); err != nil {
		t.Fatalf("write the load models: %v", err)
	}
	return out.String()
}

// checkPrinted fails for each of lines that printed holds no line of.
func checkPrinted(t *testing.T, printed string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(printed, line+"\n") {
			t.Errorf("the load models print no line %q:\n%s", line, printed)
		}
	}
}

// skipsAlone says whether model skips want and no other span.
func skipsAlone(model LoadModel, want SkippedSpan) bool {
	return len(model.Skipped) == 1 && model.Skipped[0].From.Equal(want.From) && model.Skipped[0].To.Equal(want.To) &&
		model.Skipped[0].Reason == want.Reason
}

// minuteOf is the moment minute minutes after 2001-03-05T00:00Z, the start of the windows of the
// load models' tests.
func minuteOf(minute int) time.Time {
	return clock(0, minute)
}

// queuedWait is one wait of a pull request in the merge queue, from its addition to its merge.
type queuedWait struct {
	added, left time.Time
}

// queueLinesOf is the queue file's lines of waits, each of its own pull request, numbered from 1:
// each one's added_to_merge_queue and its merged, the events numbered from 1 in turn.
func queueLinesOf(waits []queuedWait) []json.RawMessage {
	return repositoryQueueLines(builtRepository, 0, waits)
}

// repositoryQueueLines is a queue file's lines of waits of repo's pull requests, each wait of its
// own pull request, numbered from 1: each one's added_to_merge_queue and its merged, the events
// numbered from first + 1 in turn.
func repositoryQueueLines(repo string, first int, waits []queuedWait) []json.RawMessage {
	var lines []json.RawMessage
	for i, wait := range waits {
		lines = append(lines, queueLineOf(repo, first+2*i+1, i+1, queueAdded, wait.added), queueLineOf(repo, first+2*i+2, i+1, queueMerged, wait.left))
	}
	return lines
}

// waitsEvery is n waits of length long, the first added at start and each next one every minutes
// later.
func waitsEvery(start, n, minutes, long int) []queuedWait {
	var waits []queuedWait
	for i := range n {
		added := start + minutes*i
		waits = append(waits, queuedWait{added: minuteOf(added), left: minuteOf(added + long)})
	}
	return waits
}

// providerLine is the provider's record of minute: its uncached input tokens, the project's own
// limit of limit tokens a minute as a recorded change names it (none for 0), and own refusals by
// that limit.
func providerLine(minute int, uncached, limit, own int64) ProviderMinute {
	line := ProviderMinute{Minute: minuteOf(minute), Uncached: uncached, Own: own}
	if limit > 0 {
		source, scope := "dispatch", projectLimitScope
		line.Limit, line.LimitSource, line.LimitScope = &limit, &source, &scope
	}
	return line
}

// guardKills writes the launcher's line of a kill of the memory guard of pod at each of minutes.
func guardKills(b *windowBuilder, pod string, minutes ...int) {
	for _, minute := range minutes {
		b.window.Launcher = append(b.window.Launcher, LauncherLine{Time: minuteOf(minute), Pod: pod,
			Text: guardKillText(1000+len(b.window.Launcher), "implementer")})
	}
}

// recordTimelineOf is the record's own timeline of models, which must read.
func recordTimelineOf(t *testing.T, models LoadModels) ReplayTimeline {
	t.Helper()
	tl, err := models.RecordTimeline()
	if err != nil {
		t.Fatalf("the record's timeline: %v", err)
	}
	return tl
}

// segmentMoved is tl with the work segment of issue that the record started at recorded started at
// replayed in the replay; tl must hold that segment.
func segmentMoved(t *testing.T, tl ReplayTimeline, issue string, recorded, replayed time.Time) ReplayTimeline {
	t.Helper()
	segments := slices.Clone(tl.Segments)
	for i, segment := range segments {
		if segment.Issue == issue && segment.Recorded.Equal(recorded) {
			segments[i].Replayed = replayed
			tl.Segments = segments
			return tl
		}
	}
	t.Fatalf("the record's timeline holds no work segment of %s started at %s: %+v", issue, stamp(recorded), tl.Segments)
	return tl
}

// modelledSignals is the signals models give the replay whose timeline is tl, which must model.
func modelledSignals(t *testing.T, models LoadModels, tl ReplayTimeline) *ModelledSignals {
	t.Helper()
	modelled, err := models.Replay(tl)
	if err != nil {
		t.Fatalf("model the replay's signals: %v", err)
	}
	return modelled
}

// assertSignal fails where got is unrecorded, or stands farther from want than a fixture's
// tolerance.
func assertSignal(t *testing.T, name string, got signals.Value, want float64) {
	t.Helper()
	if !got.Recorded {
		t.Errorf("%s is unrecorded, want %v", name, want)
		return
	}
	assertNear(t, name, got.Value, want)
}

// sameMoment says whether a and b stand within a millisecond of each other, the rounding of a
// moment the models reckon from a rate.
func sameMoment(a, b time.Time) bool {
	return a.Sub(b).Abs() <= time.Millisecond
}

// assertMoments fails where got is not want, moment by moment (sameMoment).
func assertMoments(t *testing.T, name string, got, want []time.Time) {
	t.Helper()
	if !slices.EqualFunc(got, want, sameMoment) {
		t.Errorf("%s are %v, want %v", name, got, want)
	}
}

// assertWaits fails where got is not want, wait by wait in order (sameMoment).
func assertWaits(t *testing.T, name string, got []signals.Wait, want []queuedWait) {
	t.Helper()
	if !slices.EqualFunc(got, want, func(a signals.Wait, b queuedWait) bool {
		return sameMoment(a.Added, b.added) && sameMoment(a.Left, b.left)
	}) {
		t.Errorf("%s are %v, want %v", name, got, want)
	}
}

// passFailWindow is a window of four days from 2001-03-05T00:00Z whose one issue, ACME-1, is in
// flight from the window's start to its end, its one work segment open from the start. It holds a
// record of the memory guard's kills from the start to 2001-03-07T11:00Z and from
// 2001-03-08T00:00Z to 09:00Z, of the merge queue and of the provider over the whole span. The
// launcher's lines record kills of ACME-1's pod, 20 on the first day, 38 on the second, 5 in the
// record's 11 hours of the third and 5 in its 9 hours of the fourth, and 3 more on each of the last
// two days outside the record. Nine pull requests, of no issue, leave the merge queue: on the first
// day after waits of 1, 1.2 and 1.4 hours, on the second after 0.5, 2 and 2 hours, on the third
// after 0.6, 0.8 and 1 hour, and none on the fourth; no two stand in the queue at once. The
// provider's record holds every minute under the project's own limit of 1,000 tokens a minute, 500
// tokens a minute but where a day says otherwise: on the first day, 20 minutes of 2,000 tokens in
// which the project's own limit refused 2 calls each, and 4 of 2,000 tokens with no refusal; on the
// second, 10 such refused minutes over the limit and 10 refused minutes of 500 tokens; on the
// third, 5 refused minutes over the limit and 6 over it with no refusal; on the fourth, 3 and 6.
func passFailWindow(t *testing.T) Window {
	b := newWindowBuilder(t, minuteOf(0), minuteOf(96*60))
	b.boot(minuteOf(-60))
	promotedRoot(b, minuteOf(0), "ACME-1", "a1")
	var kills []int
	for i := range 20 {
		kills = append(kills, 30+60*i)
	}
	for i := range 38 {
		kills = append(kills, 1440+15+35*i)
	}
	for i := range 5 {
		kills = append(kills, 2880+60+120*i)
	}
	kills = append(kills, 3600, 3720, 3840)
	for i := range 5 {
		kills = append(kills, 4320+30+90*i)
	}
	kills = append(kills, 5100, 5280, 5460)
	guardKills(b, "legion-acme-acme-1", kills...)
	var waits []queuedWait
	for _, wait := range [][2]int{{120, 60}, {240, 72}, {360, 84}, {1560, 30}, {1680, 120}, {1860, 120}, {3000, 36}, {3120, 48}, {3240, 60}} {
		waits = append(waits, queuedWait{added: minuteOf(wait[0]), left: minuteOf(wait[0] + wait[1])})
	}
	b.window.Queue = map[string][]json.RawMessage{builtRepository: queueLinesOf(waits)}
	minutes := make(map[int][2]int64)
	refused := func(from, n int, uncached, own int64) {
		for minute := from; minute < from+n; minute++ {
			minutes[minute] = [2]int64{uncached, own}
		}
	}
	refused(100, 20, 2000, 2)
	refused(200, 4, 2000, 0)
	refused(1540, 10, 2000, 2)
	refused(1600, 10, 500, 2)
	refused(2980, 5, 2000, 2)
	refused(3000, 6, 2000, 0)
	refused(4420, 3, 2000, 2)
	refused(4500, 6, 2000, 0)
	for minute := range 96 * 60 {
		line, special := minutes[minute]
		if !special {
			line = [2]int64{500, 0}
		}
		b.window.Provider = append(b.window.Provider, providerLine(minute, line[0], 1000, line[1]))
	}
	b.window.Signals = map[string][]Span{
		SignalKills:    {{From: minuteOf(0), To: minuteOf(59 * 60)}, {From: minuteOf(72 * 60), To: minuteOf(81 * 60)}},
		SignalQueue:    {{From: minuteOf(0), To: minuteOf(96 * 60)}},
		SignalProvider: {{From: minuteOf(0), To: minuteOf(96 * 60)}},
	}
	return b.build()
}

// R11's daily test of the load models (spec R11), on the four days of passFailWindow at the
// recorded work in flight, one issue all through. The tokens model tests the refused minutes: the
// minutes whose recorded tokens pass the limit are its refused minutes there, and the record's are
// the minutes with a refusal of the project's own. With at least 10 recorded, the first day's 24
// modelled is within 30% of its 20 recorded and passes, and the second's 10 is not within 30% of its
// 20 and fails; with fewer recorded, the third day's 11 modelled is more than 10 and fails, and the
// fourth's 9 passes. The record's 96 own refusals in 48 refused minutes give 2 for each refused
// minute, so the first day's 24 modelled refused minutes give 48 own refusals beside its recorded
// 40. The kill model places each kill at its own offset in ACME-1's work segment, and the merge
// queue's model serves each pull request at its recorded exit, so both give the record on each day
// at the recorded work in flight, by their form, pass and say so: the kills 20, 38, 5 and 5 (68
// kills over 68 issue-hours, 1 for each hour of an issue in flight, the first day's own 20 over 24),
// and the median waits 1.2, 2 and 0.8 hours, the fourth day having no exit and no test. The tokens
// model fails and gives no limit; the other two give theirs on the daily test alone, since the test
// across two records did not run.
func TestModelsPassAndFailRsDailyTest(t *testing.T) {
	t.Parallel()
	window := passFailWindow(t)
	models := fittedModels(t, window)
	checkDaysInFlight(t, window, models)
	if models.Across != nil {
		t.Errorf("the load models fitted on one window give a test across two records: %+v", models.Across)
	}

	tokens := modelNamed(t, models, ModelTokens)
	assertNear(t, "the tokens model's refusals per refused minute", parameterOf(t, tokens, paramRefusalsPerMinute), 2)
	assertNear(t, "the tokens model's refused minutes", parameterOf(t, tokens, paramRefusedMinutes), 48)
	checkDays(t, tokens, []wantDay{
		{day: "2001-03-05", hours: 24, recorded: 20, modelled: 24, verdict: VerdictPasses},
		{day: "2001-03-06", hours: 24, recorded: 20, modelled: 10, verdict: VerdictFails},
		{day: "2001-03-07", hours: 24, recorded: 5, modelled: 11, verdict: VerdictFails},
		{day: "2001-03-08", hours: 24, recorded: 3, modelled: 9, verdict: VerdictPasses},
	})
	if len(tokens.Days) == 4 {
		assertNearValue(t, "the own refusals recorded on the first day", tokens.Days[0].RefusalsRecorded, 40)
		assertNearValue(t, "the own refusals modelled on the first day", tokens.Days[0].RefusalsModelled, 48)
		assertNearValue(t, "the tokens a minute modelled on the first day", tokens.Days[0].LoadModelled, 525)
	}
	if tokens.Verdict != VerdictFails || tokens.GivesLimit || tokens.KeepsRecord {
		t.Errorf("the tokens model %s, gives a limit %t and keeps the record %t; want it to fail, give none and not keep the record by its form",
			tokens.Verdict, tokens.GivesLimit, tokens.KeepsRecord)
	}

	memory := modelNamed(t, models, ModelMemory)
	if !memory.Fitted || memory.Signal != SignalKills {
		t.Errorf("the memory model is fitted %t on the signal %q, want fitted on %q", memory.Fitted, memory.Signal, SignalKills)
	}
	assertNear(t, "the kill model's rate", parameterOf(t, memory, paramKillRate), 1)
	assertNear(t, "the kill model's kills apart", parameterOf(t, memory, paramKillsApart), 0)
	checkDays(t, memory, []wantDay{
		{day: "2001-03-05", hours: 24, recorded: 20, modelled: 20, verdict: VerdictPasses},
		{day: "2001-03-06", hours: 24, recorded: 38, modelled: 38, verdict: VerdictPasses},
		{day: "2001-03-07", hours: 11, recorded: 5, modelled: 5, verdict: VerdictPasses},
		{day: "2001-03-08", hours: 9, recorded: 5, modelled: 5, verdict: VerdictPasses},
	})
	if len(memory.Days) == 4 {
		assertNearValue(t, "the first day's kills for each hour of an issue in flight", memory.Days[0].KillRate, 20.0/24)
		assertNearValue(t, "the first day's kills apart", memory.Days[0].KillsApart, 0)
	}
	if memory.Verdict != VerdictPasses || !memory.KeepsRecord || !memory.GivesLimit || memory.Across != VerdictNotRun {
		t.Errorf("the memory model %s, keeps the record %t, gives a limit %t, across records %s; want it to pass, keep the record and give one, not tested across",
			memory.Verdict, memory.KeepsRecord, memory.GivesLimit, memory.Across)
	}

	queue := modelNamed(t, models, ModelQueue)
	if !queue.Fitted || queue.Signal != SignalQueue {
		t.Errorf("the merge queue's model is fitted %t on the signal %q, want fitted on %q", queue.Fitted, queue.Signal, SignalQueue)
	}
	checkDays(t, queue, []wantDay{
		{day: "2001-03-05", hours: 24, recorded: 1.2, modelled: 1.2, verdict: VerdictPasses},
		{day: "2001-03-06", hours: 24, recorded: 2, modelled: 2, verdict: VerdictPasses},
		{day: "2001-03-07", hours: 24, recorded: 0.8, modelled: 0.8, verdict: VerdictPasses},
		{day: "2001-03-08", hours: 24, verdict: VerdictNoTest, reason: reasonNoExit},
	})
	if queue.Verdict != VerdictPasses || !queue.KeepsRecord || !queue.GivesLimit {
		t.Errorf("the merge queue's model %s, keeps the record %t, gives a limit %t; want it to pass, keep the record and give one",
			queue.Verdict, queue.KeepsRecord, queue.GivesLimit)
	}

	checkPrinted(t, printedModels(t, models),
		"R11's test across two records did not run, so a model that passes the daily test gives its limit on that test alone.",
		"The model fails R11's daily test.",
		"So it gives no limit for the signal `provider`.",
		"The model passes R11's daily test on each day it is tested.",
		"At the recorded work in flight the model gives the record by its form, so R11's daily test shows only that it keeps the record.",
		"R11's test across two records did not run.",
		"So it gives a limit for the signal `kills`, on R11's daily test alone.",
		"So it gives a limit for the signal `queue`, on R11's daily test alone.",
		"- from 2001-03-07T11:00:00Z to 2001-03-08T00:00:00Z: "+reasonNoKills,
		"- from 2001-03-08T09:00:00Z to 2001-03-09T00:00:00Z: "+reasonNoKills,
		"| 2001-03-06 | 1 | 24 | 20 | 10 | fails | 40 | 20 | 510.4167 | 510.4167 | 0 |",
	)
}

// missingRecordWindow is a window of three days from 2001-03-05T00:00Z whose two issues, ACME-1
// and ACME-2, are in flight from its start to its end. The provider's record holds every minute,
// 1,000 uncached input tokens each, with the project's own limit of 6,000 a minute on the first
// and the third day and no limit observed on the second, on which the provider refuses 4 calls;
// the window holds a record of the provider's headroom in the minutes with a limit, as collect
// writes it. The memory guard runs from the second day: the record of its kills starts there, and
// the launcher's lines hold 12 kills on each of the second and the third day, and 30 on the first,
// before the record. The window holds no record of the merge queue.
func missingRecordWindow(t *testing.T) Window {
	b := newWindowBuilder(t, minuteOf(0), minuteOf(72*60))
	b.boot(minuteOf(-60))
	promotedRoot(b, minuteOf(0), "ACME-1", "a1")
	promotedRoot(b, minuteOf(0), "ACME-2", "a2")
	for minute := range 72 * 60 {
		limit := int64(0)
		if minute < 24*60 || minute >= 48*60 {
			limit = 6000
		}
		line := providerLine(minute, 1000, limit, 0)
		if minute == 30*60 {
			line.Provider = 4
		}
		b.window.Provider = append(b.window.Provider, line)
	}
	b.window.Launcher = []LauncherLine{{Time: minuteOf(24 * 60), Pod: "legion-acme-acme-1",
		Text: "launcher: memory guard: started act_percent=85 floor_percent=10 wait=5s interval=100ms limit=8589934592 reading=23052288"}}
	pid := 2000
	kill := func(minute int, pod string) {
		pid++
		b.window.Launcher = append(b.window.Launcher, LauncherLine{Time: minuteOf(minute), Pod: pod, Text: guardKillText(pid, "tester")})
	}
	for i := range 30 {
		kill(30+40*i, "legion-acme-acme-2")
	}
	for i := range 24 {
		kill(1440+30+120*i, "legion-acme-acme-1")
	}
	b.window.Signals = map[string][]Span{
		SignalProvider: {{From: minuteOf(0), To: minuteOf(24 * 60)}, {From: minuteOf(48 * 60), To: minuteOf(72 * 60)}},
		SignalKills:    {{From: minuteOf(24 * 60), To: minuteOf(72 * 60)}},
	}
	return b.build()
}

// A load model is fitted and tested only in the spans where the window holds its record (plan Q4,
// departure 10), on missingRecordWindow at two issues in flight. The memory guard's record starts
// on the second day: the kill model's rate is its 24 kills over 96 issue-hours, 0.25 kills for each
// hour of an issue in flight, the first day's 30 kills read by none; it gives 12 kills on each of
// the second and the third day, as recorded, and the first day has no test, a span the output
// names. The tokens model is fitted on every minute of the provider's record, 500 tokens for each
// minute of an issue in flight, and still where no limit is recorded; it gives no refused minute at
// a limit of 6,000 on the first and the third day, as recorded, and on the second day, which
// records no limit, the refused minutes have no test, never a pass, while the provider's 4
// refusals are kept as recorded. The merge queue's model, with no record anywhere, is not fitted,
// gives no test and gives no limit.
func TestAModelIsNotFittedWhereItsRecordIsMissing(t *testing.T) {
	t.Parallel()
	window := missingRecordWindow(t)
	models := fittedModels(t, window)
	checkDaysInFlight(t, window, models)

	memory := modelNamed(t, models, ModelMemory)
	assertNear(t, "the kill model's rate", parameterOf(t, memory, paramKillRate), 0.25)
	checkDays(t, memory, []wantDay{
		{day: "2001-03-05", verdict: VerdictNoTest, reason: reasonNoKills},
		{day: "2001-03-06", hours: 24, recorded: 12, modelled: 12, verdict: VerdictPasses},
		{day: "2001-03-07", hours: 24, recorded: 12, modelled: 12, verdict: VerdictPasses},
	})
	if !skipsAlone(memory, SkippedSpan{From: minuteOf(0), To: minuteOf(24 * 60), Reason: reasonNoKills}) {
		t.Errorf("the memory model skips %+v, want the first day alone, for want of the memory guard's record", memory.Skipped)
	}
	if memory.Verdict != VerdictPasses || !memory.GivesLimit {
		t.Errorf("the memory model %s and gives a limit %t, want it to pass and give one", memory.Verdict, memory.GivesLimit)
	}

	tokens := modelNamed(t, models, ModelTokens)
	if !tokens.Fitted || tokens.Signal != SignalProvider {
		t.Errorf("the tokens model is fitted %t on the signal %q, want fitted on %q", tokens.Fitted, tokens.Signal, SignalProvider)
	}
	assertNear(t, "the tokens model's minutes", parameterOf(t, tokens, paramMinutesFitted), 3*24*60)
	assertNear(t, "the tokens model's tokens for each minute of an issue in flight", parameterOf(t, tokens, paramTokensRate), 500)
	checkDays(t, tokens, []wantDay{
		{day: "2001-03-05", hours: 24, recorded: 0, modelled: 0, verdict: VerdictPasses},
		{day: "2001-03-06", verdict: VerdictNoTest, reason: reasonNoLimit},
		{day: "2001-03-07", hours: 24, recorded: 0, modelled: 0, verdict: VerdictPasses},
	})
	if len(tokens.Days) == 3 {
		assertNearValue(t, "the provider's refusals kept on the second day", tokens.Days[1].ProviderRefusals, 4)
		assertNearValue(t, "the tokens a minute recorded on the second day", tokens.Days[1].LoadRecorded, 1000)
		assertNearValue(t, "the tokens a minute modelled on the second day", tokens.Days[1].LoadModelled, 1000)
	}
	if !skipsAlone(tokens, SkippedSpan{From: minuteOf(24 * 60), To: minuteOf(48 * 60), Reason: reasonNoLimit}) {
		t.Errorf("the tokens model skips %+v, want the second day alone, for want of a limit of the project's own", tokens.Skipped)
	}

	queue := modelNamed(t, models, ModelQueue)
	if queue.Fitted || queue.Verdict != VerdictNoTest || queue.GivesLimit || queue.Reason == "" {
		t.Errorf("the merge queue's model is fitted %t, %s (%q), gives a limit %t; want it not fitted, no test with its reason, and no limit",
			queue.Fitted, queue.Verdict, queue.Reason, queue.GivesLimit)
	}
	if !skipsAlone(queue, SkippedSpan{From: window.From, To: window.To, Reason: reasonNoQueue}) {
		t.Errorf("the merge queue's model skips %+v, want the whole span, for want of its record", queue.Skipped)
	}

	checkPrinted(t, printedModels(t, models),
		"- from 2001-03-05T00:00:00Z to 2001-03-06T00:00:00Z: "+reasonNoKills,
		"- from 2001-03-06T00:00:00Z to 2001-03-07T00:00:00Z: "+reasonNoLimit,
		"- from 2001-03-05T00:00:00Z to 2001-03-08T00:00:00Z: "+reasonNoQueue,
		fmt.Sprintf("The model gives no daily test: %s.", queue.Reason),
		"So it gives no limit for the signal `queue`.",
		"So it gives a limit for the signal `kills`, on R11's daily test alone.",
	)
}

// tokensWindow is a window from 2001-03-05T00:00Z to 03:00Z whose two issues, ACME-1 and ACME-2,
// are promoted at 00:10 and stand in flight to its end, so the record holds no issue in flight
// over its first ten minutes. The provider's record holds every minute, under the project's own
// limit of 8,000 tokens a minute, with a record of the headroom over the whole span: 1,000 uncached
// input tokens a minute, 7,000 in the minute from 00:05 and 5,000 in the minute from 01:00. The
// provider refuses 3 calls in the minute from 01:30, and the project's own limit refuses none.
func tokensWindow(t *testing.T) Window {
	b := newWindowBuilder(t, minuteOf(0), minuteOf(180))
	b.boot(minuteOf(-60))
	promotedRoot(b, minuteOf(10), "ACME-1", "a1")
	promotedRoot(b, minuteOf(10), "ACME-2", "a2")
	for minute := range 180 {
		uncached := int64(1000)
		switch minute {
		case 5:
			uncached = 7000
		case 60:
			uncached = 5000
		}
		line := providerLine(minute, uncached, 8000, 0)
		if minute == 90 {
			line.Provider = 3
		}
		b.window.Provider = append(b.window.Provider, line)
	}
	b.window.Signals = map[string][]Span{SignalProvider: {{From: minuteOf(0), To: minuteOf(180)}}}
	return b.build()
}

// The tokens model gives each minute its recorded tokens for each issue in flight, times the
// replay's issues in flight (spec R11), on tokensWindow replayed with 4 issues in flight from its
// start. The minute from 01:00 held 5,000 tokens at 2 issues in flight in the record, 2,500 for
// each, so the replay gives it 10,000: past the limit of 8,000, where the record's 5,000 was not, a
// headroom of 1.25 at 01:01 against the record's 0.625, and one refused minute, which counts the
// record's own refusals per refused minute, 1 where the record holds none, so the project's own
// refusals of the hour to 01:31 are 1 in the replay and 0 in the record. The next minute's 1,000
// at 2 issues becomes 2,000, a headroom of 0.25. The minute from 00:05, with no issue in flight in
// the record, keeps its 7,000 recorded tokens, 0.875 at 00:06 in both. The provider's 3 refusals of
// the minute from 01:30 stay as recorded in the replay.
func TestTheTokensModelScalesEachMinuteByTheReplaysWorkInFlight(t *testing.T) {
	t.Parallel()
	models := fittedModels(t, tokensWindow(t))
	record := modelledSignals(t, models, recordTimelineOf(t, models))
	replay := modelledSignals(t, models, ReplayTimeline{InFlight: []FlightStep{{At: minuteOf(0), Issues: 4}}})

	assertSignal(t, "the replay's headroom at 01:01", replay.Headroom(minuteOf(61)), 1.25)
	assertSignal(t, "the record's headroom at 01:01", record.Headroom(minuteOf(61)), 0.625)
	assertSignal(t, "the replay's headroom at 01:02", replay.Headroom(minuteOf(62)), 0.25)
	assertSignal(t, "the replay's headroom at 00:06", replay.Headroom(minuteOf(6)), 0.875)
	assertSignal(t, "the record's headroom at 00:06", record.Headroom(minuteOf(6)), 0.875)
	if got := len(replay.refused); got != 1 || !replay.refused[minuteOf(60).Unix()] {
		t.Errorf("the replay's refused minutes are %v, want the one from 01:00", replay.refused)
	}
	if len(record.refused) != 0 {
		t.Errorf("the record's refused minutes are %v, want none", record.refused)
	}
	for _, c := range []struct {
		name          string
		got           signals.RefusalCount
		own, provider int64
	}{
		{"the replay's", replay.Refusals(minuteOf(91)), 1, 3},
		{"the record's", record.Refusals(minuteOf(91)), 0, 3},
	} {
		if !c.got.Recorded || c.got.Own != c.own || c.got.Provider != c.provider {
			t.Errorf("%s refusals at 01:31 are %+v, want %d of the project's own and %d of the provider's", c.name, c.got, c.own, c.provider)
		}
	}
}

// killWorkWindow is a window from 2001-03-05T00:00Z to 06:00Z with a record of the memory guard's
// kills over its whole span. ACME-1 is promoted at its start and stands in flight to its end, its
// one work segment open from the start; ACME-2 is handed to Legion at its start and waits for a
// slot all through, with no work segment. The launcher's lines record kills of ACME-1's pod at
// 00:20 and 04:10, and of ACME-2's pod at 03:20.
func killWorkWindow(t *testing.T) Window {
	b := newWindowBuilder(t, minuteOf(0), minuteOf(360))
	b.boot(minuteOf(-60))
	promotedRoot(b, minuteOf(0), "ACME-1", "a1")
	b.file(minuteOf(0), "ACME-2", filing{Title: "Count the widgets per shelf", Status: "todo", Rank: "a2", Labels: handedToLegion})
	guardKills(b, "legion-acme-acme-1", 20)
	guardKills(b, "legion-acme-acme-2", 200)
	guardKills(b, "legion-acme-acme-1", 250)
	b.window.Signals = map[string][]Span{SignalKills: {{From: minuteOf(0), To: minuteOf(360)}}}
	return b.build()
}

// A kill moves with the work of its own issue (spec R11), on killWorkWindow: one day of 6
// issue-hours in flight and 3 kills, 0.5 for each hour of an issue in flight. ACME-1's kills lie 20
// minutes and 4 hours 10 minutes into its segment, so a replay that starts that segment an hour
// later places them at 01:20 and 05:10; ACME-2's kill lies in no segment of its issue, so it is
// counted apart and kept at 03:20. Two issues the record never ran stand in flight in the replay,
// ACME-7 from 02:00 to 05:30 and ACME-8 from 03:00 to 04:00, each taking the day's 0.5 kills for
// each of its own hours in flight: ACME-7 reaches one kill after 2 hours, at 04:00, and ACME-8,
// with half a kill, none; summed over both, the kills would come at 03:30 and 05:00. So the hour to
// 01:21 holds 1 kill in the replay and none in the record, and the hour to 04:01 holds 2 in the
// replay (03:20 and 04:00) and 1 in the record. A replay that does not run ACME-1's segment has
// none of its kills. At the record's own timeline the model gives the record: the day's 3 kills,
// one of them apart.
func TestAKillMovesWithTheWorkOfItsOwnIssue(t *testing.T) {
	t.Parallel()
	models := fittedModels(t, killWorkWindow(t))
	record := recordTimelineOf(t, models)
	for _, segment := range record.Segments {
		if segment.Issue == "ACME-2" {
			t.Errorf("the record's timeline gives ACME-2, which never started, the work segment %+v", segment)
		}
	}
	replay := segmentMoved(t, record, "ACME-1", minuteOf(0), minuteOf(60))
	replay.Drawn = []DrawnFlight{{Issue: "ACME-7", From: minuteOf(120), To: minuteOf(330)}, {Issue: "ACME-8", From: minuteOf(180), To: minuteOf(240)}}
	modelled := modelledSignals(t, models, replay)
	assertMoments(t, "the replay's kills", modelled.kills, []time.Time{minuteOf(80), minuteOf(200), minuteOf(240), minuteOf(310)})
	if modelled.KillsApart != 1 {
		t.Errorf("the replay counts %d kills apart, want 1", modelled.KillsApart)
	}
	recorded := modelledSignals(t, models, record)
	assertMoments(t, "the record's kills", recorded.kills, []time.Time{minuteOf(20), minuteOf(200), minuteOf(250)})
	assertSignal(t, "the replay's kills at 01:21", modelled.Kills(minuteOf(81)), 1)
	assertSignal(t, "the record's kills at 01:21", recorded.Kills(minuteOf(81)), 0)
	assertSignal(t, "the replay's kills at 04:01", modelled.Kills(minuteOf(241)), 2)
	assertSignal(t, "the record's kills at 04:01", recorded.Kills(minuteOf(241)), 1)

	unrun := record
	unrun.Segments = nil
	assertMoments(t, "the kills of a replay that does not run ACME-1's segment", modelledSignals(t, models, unrun).kills, []time.Time{minuteOf(200)})

	memory := modelNamed(t, models, ModelMemory)
	checkDays(t, memory, []wantDay{{day: "2001-03-05", hours: 6, recorded: 3, modelled: 3, verdict: VerdictPasses}})
	if len(memory.Days) == 1 {
		assertNearValue(t, "the day's kills for each hour of an issue in flight", memory.Days[0].KillRate, 0.5)
		assertNearValue(t, "the day's kills apart", memory.Days[0].KillsApart, 1)
	}
	assertNear(t, "the kill model's kills apart", parameterOf(t, memory, paramKillsApart), 1)
	if !memory.KeepsRecord {
		t.Errorf("the kill model does not give the record at the record's own timeline: %+v", memory.Days)
	}
}

// queueWorkWindow is a window from 2001-03-05T00:00Z to 08:00Z with a record of the merge queue
// over its whole span. ACME-1 is promoted at its start and stands in flight to its end, its one
// work segment open from the start, and its Dispatch events link pull request 1 of acme/widgets.
// acme/widgets' queue holds #1 from 00:30 to its merge at 00:50, #2 from 01:35 to 02:00, #3 from
// 02:30 to 04:45, and #4 from 02:40 until it is removed at 02:50, while #3, ahead of it, still
// stands; acme/gadgets' holds #1 from 00:40 to 01:28. No issue links a pull request but ACME-1.
func queueWorkWindow(t *testing.T) Window {
	b := newWindowBuilder(t, minuteOf(0), minuteOf(480))
	b.boot(minuteOf(-60))
	promotedRoot(b, minuteOf(0), "ACME-1", "a1")
	widgets := repositoryQueueLines(builtRepository, 0, []queuedWait{{added: minuteOf(30), left: minuteOf(50)}, {added: minuteOf(95), left: minuteOf(120)},
		{added: minuteOf(150), left: minuteOf(285)}})
	widgets = append(widgets, queueLineOf(builtRepository, 7, 4, queueAdded, minuteOf(160)), queueLineOf(builtRepository, 8, 4, queueRemoved, minuteOf(170)))
	b.window.Queue = map[string][]json.RawMessage{
		builtRepository: widgets,
		"acme/gadgets":  repositoryQueueLines("acme/gadgets", 100, []queuedWait{{added: minuteOf(40), left: minuteOf(88)}}),
	}
	b.window.Signals = map[string][]Span{SignalQueue: {{From: minuteOf(0), To: minuteOf(480)}}}
	return linkedPullRequest(t, b.build(), "ACME-1", "https://github.com/acme/widgets/pull/1")
}

// Each repository has its own merge queue (spec R11), on queueWorkWindow. acme/widgets' record
// exits from its head at 00:50, 02:00 and 04:45, its queue holding a pull request for 3 hours of
// the span, 1 exit an hour; #4, removed while #3 stood ahead of it, keeps its 10-minute wait.
// acme/gadgets' exits once in its 48 minutes that held a pull request, 1.25 an hour. A replay
// starts ACME-1's segment an hour later, so #1 enters at its recorded offset of 30 minutes, at
// 01:30, and adds two pull requests the record never had, one to acme/widgets at 05:00 and one to
// acme/gadgets at 00:45. In acme/widgets' order of entry, #1 at 01:30 leaves at the record's next
// exit from the head after its entry, 02:00; #2, at 01:35, reaches the head at 02:00, when the
// record's queue stood empty, and the queue serves it at its rate an hour later, at 03:00; #3, at
// 02:30, reaches the head at 03:00, inside the record's span that held #3, and leaves at the next
// recorded exit, 04:45; the new one, at 05:00 after the record's last exit, leaves at the rate, at
// 06:00. In acme/gadgets, #1 takes its own recorded exit at 01:28, and the new one, behind it,
// leaves 48 minutes later, at 02:16, by gadgets' own rate: neither queue reads the other's exits,
// and the widgets waits are the same with no gadgets entry. At 01:40 the replay's longest wait is
// the gadgets entry's 55 minutes, against the 48-minute exit of the 6 hours before. At the record's
// own timeline the model gives every recorded wait.
func TestEachRepositorysQueueServesItsOwnPullRequests(t *testing.T) {
	t.Parallel()
	models := fittedModels(t, queueWorkWindow(t))
	record := recordTimelineOf(t, models)
	recorded := modelledSignals(t, models, record)
	assertWaits(t, "the waits at the record's own timeline", recorded.waits, []queuedWait{
		{added: minuteOf(30), left: minuteOf(50)}, {added: minuteOf(40), left: minuteOf(88)}, {added: minuteOf(95), left: minuteOf(120)},
		{added: minuteOf(150), left: minuteOf(285)}, {added: minuteOf(160), left: minuteOf(170)},
	})
	if recorded.EntriesApart != 4 || recorded.Unserved != 0 {
		t.Errorf("the record's own timeline counts %d pull requests apart and %d unserved, want 4 and none", recorded.EntriesApart, recorded.Unserved)
	}

	replay := segmentMoved(t, record, "ACME-1", minuteOf(0), minuteOf(60))
	widgetsOnly := replay
	widgetsOnly.Entries = []QueueEntry{{Repository: builtRepository, At: minuteOf(300)}}
	replay.Entries = []QueueEntry{{Repository: builtRepository, At: minuteOf(300)}, {Repository: "ACME/Gadgets", At: minuteOf(45)}}
	widgets := []queuedWait{
		{added: minuteOf(90), left: minuteOf(120)}, {added: minuteOf(95), left: minuteOf(180)}, {added: minuteOf(150), left: minuteOf(285)},
		{added: minuteOf(160), left: minuteOf(170)}, {added: minuteOf(300), left: minuteOf(360)},
	}
	gadgets := []queuedWait{{added: minuteOf(40), left: minuteOf(88)}, {added: minuteOf(45), left: minuteOf(136)}}
	modelled := modelledSignals(t, models, replay)
	assertWaits(t, "the replay's waits", modelled.waits, append(slices.Clone(gadgets), widgets...))
	assertWaits(t, "the replay's waits with no gadgets entry", modelledSignals(t, models, widgetsOnly).waits, append(gadgets[:1:1], widgets...))
	assertSignal(t, "the replay's queue at 01:40", modelled.Queue(minuteOf(100)), 55.0/60)

	queue := modelNamed(t, models, ModelQueue)
	for _, c := range []struct {
		repo, name string
		want       float64
	}{
		{builtRepository, paramHeadExits, 3}, {builtRepository, paramEarlyExits, 1}, {builtRepository, paramBusyHours, 3}, {builtRepository, paramExitRate, 1},
		{"acme/gadgets", paramHeadExits, 1}, {"acme/gadgets", paramBusyHours, 0.8}, {"acme/gadgets", paramExitRate, 1.25},
	} {
		i := slices.IndexFunc(queue.Parameters, func(p ModelParameter) bool { return p.Repository == c.repo && p.Name == c.name })
		if i < 0 {
			t.Errorf("the merge queue's model names no parameter %q of %s: %+v", c.name, c.repo, queue.Parameters)
			continue
		}
		assertNear(t, c.repo+" "+c.name, queue.Parameters[i].Value, c.want)
	}
	if queue.Verdict != VerdictPasses || !queue.KeepsRecord {
		t.Errorf("the merge queue's model %s and keeps the record %t, want it to pass and keep the record: %+v", queue.Verdict, queue.KeepsRecord, queue.Days)
	}
}

// acrossWindow is a window of three days from 2001-03-05T00:00Z whose issues ACME-1 to ACME-<n>
// are promoted at its start and stand in flight to its end, with a record of the memory guard's
// kills and of the merge queue over its whole span. The provider's record holds every minute,
// uncached tokens a minute, with no limit of the project's own. The launcher's lines record kills
// of ACME-1's pod at each of kills, in minutes from the start, and the queue files hold waits of
// acme/widgets' and acme/gadgets' pull requests.
func acrossWindow(t *testing.T, n int, tokens int64, kills []int, widgets, gadgets []queuedWait) Window {
	b := newWindowBuilder(t, minuteOf(0), minuteOf(72*60))
	b.boot(minuteOf(-60))
	for i := 1; i <= n; i++ {
		promotedRoot(b, minuteOf(0), fmt.Sprintf("ACME-%d", i), fmt.Sprintf("a%d", i))
	}
	for minute := range 72 * 60 {
		b.window.Provider = append(b.window.Provider, providerLine(minute, tokens, 0, 0))
	}
	guardKills(b, "legion-acme-acme-1", kills...)
	b.window.Queue = map[string][]json.RawMessage{builtRepository: repositoryQueueLines(builtRepository, 0, widgets)}
	if len(gadgets) > 0 {
		b.window.Queue["acme/gadgets"] = repositoryQueueLines("acme/gadgets", 1000, gadgets)
	}
	b.window.Signals = map[string][]Span{
		SignalKills: {{From: minuteOf(0), To: minuteOf(72 * 60)}},
		SignalQueue: {{From: minuteOf(0), To: minuteOf(72 * 60)}},
	}
	return b.build()
}

// rateNamed is the rate of across of model, of repository for the merge queue's, which it must
// hold.
func rateNamed(t *testing.T, across *AcrossRecords, model, repository string) AcrossRate {
	t.Helper()
	if across == nil {
		t.Fatal("the load models hold no test across two records")
	}
	for _, rate := range across.Rates {
		if rate.Model == model && rate.Repository == repository {
			return rate
		}
	}
	t.Fatalf("the test across two records holds no rate of %s %q: %+v", model, repository, across.Rates)
	return AcrossRate{}
}

// checkRate fails where rate's readings or verdict differ from the ones worked out by hand: its
// rate on this window and on the other record, their ratio and its verdict.
func checkRate(t *testing.T, rate AcrossRate, window, against, ratio float64, verdict string) {
	t.Helper()
	name := strings.TrimSpace(rate.Model + " " + rate.Repository)
	assertNearValue(t, name+" on this window", rate.Window.Rate, window)
	assertNearValue(t, name+" on the other record", rate.Against.Rate, against)
	assertNearValue(t, name+" ratio", rate.Ratio, ratio)
	if rate.Verdict != verdict {
		t.Errorf("%s %s the test across two records, want %s", name, rate.Verdict, verdict)
	}
}

// R11's test across two records (spec R11) of the rates the models rest on, on two windows of
// three days (acrossWindow). The first has one issue in flight, 500 tokens a minute, kills every
// half hour on its first day (48) and every two hours on the other two (12 a day), 50 waits of 5
// minutes in acme/widgets' queue, one every 10 minutes, and 10 in acme/gadgets'; the second has two
// issues in flight, 2,000 tokens a minute, a kill every hour (72) and 60 waits of 4 minutes in
// acme/widgets', and no queue of acme/gadgets. Counted from the second day, the kills are 24 over
// 48 issue-hours on the first, 0.5, and 48 over 96 on the second, 0.5: within 30%, passing.
// Counted from the start, the first's are 72 over 72, 1, twice the second's: the test fails, and
// the kill model gives no limit, though it passes its daily test. The tokens are 500 and 1,000 for
// each minute of an issue in flight: a ratio of 0.5, outside 30%, failing. acme/widgets' exits from
// the head are 50 over 250 minutes that held a pull request, 12 an hour, against 60 over 240, 15 an
// hour: a ratio of 0.8, within 30% of 15, passing, so the merge queue's model passes and gives its
// limit. acme/gadgets, with 10 exits on the first and none on the second, has no test, and the
// result names it.
func TestTheRatesAcrossTwoRecords(t *testing.T) {
	t.Parallel()
	var firstKills, secondKills []int
	for i := range 48 {
		firstKills = append(firstKills, 15+30*i)
	}
	for i := range 24 {
		firstKills = append(firstKills, 1440+60+120*i)
	}
	for i := range 72 {
		secondKills = append(secondKills, 30+60*i)
	}
	first := fittedModels(t, acrossWindow(t, 1, 500, firstKills, waitsEvery(60, 50, 10, 5), waitsEvery(1500, 10, 10, 5)))
	second := fittedModels(t, acrossWindow(t, 2, 2000, secondKills, waitsEvery(60, 60, 10, 4), nil))

	fromSecondDay, err := CompareRecords(first, second, minuteOf(24*60))
	if err != nil {
		t.Fatal(err)
	}
	across := fromSecondDay.Across
	checkRate(t, rateNamed(t, across, ModelMemory, ""), 0.5, 0.5, 1, VerdictPasses)
	checkRate(t, rateNamed(t, across, ModelTokens, ""), 500, 1000, 0.5, VerdictFails)
	checkRate(t, rateNamed(t, across, ModelQueue, builtRepository), 12, 15, 0.8, VerdictPasses)
	if want := []UntestedQueue{{Repository: "acme/gadgets", WindowExits: 10}}; !slices.Equal(across.Untested, want) {
		t.Errorf("the untested repositories are %+v, want %+v", across.Untested, want)
	}
	if !across.KillsFrom.Equal(minuteOf(24 * 60)) {
		t.Errorf("the test counts the kills from %s, want 2001-03-06T00:00:00Z", stamp(across.KillsFrom))
	}
	for _, c := range []struct {
		name, across string
		limit        bool
	}{{ModelTokens, VerdictFails, false}, {ModelMemory, VerdictPasses, true}, {ModelQueue, VerdictPasses, true}} {
		if model := modelNamed(t, fromSecondDay, c.name); model.Across != c.across || model.GivesLimit != c.limit {
			t.Errorf("the model %s %s across two records and gives a limit %t, want %s and %t", c.name, model.Across, model.GivesLimit, c.across, c.limit)
		}
	}
	checkPrinted(t, printedModels(t, fromSecondDay),
		"## R11's test across two records",
		"The other record is the window of ACME from 2001-03-05T00:00:00Z to 2001-03-08T00:00:00Z, and the kills are counted from 2001-03-06T00:00:00Z on each record.",
		"| memory | kills for each hour of an issue in flight | – | 0.5 (24 over 48) | 0.5 (48 over 96) | 1 | passes |",
		"- acme/gadgets: 10 exits from its head on this window, 0 on the other record",
		"The rate it rests on fails R11's test across two records.",
		"So it gives a limit for the signal `queue`.",
	)

	fromStart, err := CompareRecords(first, second, minuteOf(0))
	if err != nil {
		t.Fatal(err)
	}
	checkRate(t, rateNamed(t, fromStart.Across, ModelMemory, ""), 1, 0.5, 2, VerdictFails)
	if memory := modelNamed(t, fromStart, ModelMemory); memory.Verdict != VerdictPasses || memory.Across != VerdictFails || memory.GivesLimit {
		t.Errorf("the kill model counted from the start %s its daily test, %s across records and gives a limit %t; want it to pass, fail and give none",
			memory.Verdict, memory.Across, memory.GivesLimit)
	}
}

// loadWindow is a window from 2001-03-05T00:00Z to 03:00Z under a cap of 40, with a record of the
// memory guard's kills and of the merge queue over its whole span. ACME-1 to ACME-15 are promoted
// at its start and ACME-16 to ACME-25 at 01:00, and all stand in flight to its end: 15 issues for
// an hour, then 25 for two. The provider's record holds 1,500 uncached input tokens a minute in the
// first hour and 5,000 in the next two. The launcher's lines record 3 kills in the first hour and
// 10 in the next two; acme/widgets' queue holds a pull request from 00:20 to 00:30 and one from
// 01:40 to 02:00.
func loadWindow(t *testing.T) Window {
	b := newWindowBuilder(t, minuteOf(0), minuteOf(180))
	b.capped(40)
	b.boot(minuteOf(-60))
	for i := 1; i <= 25; i++ {
		at := minuteOf(0)
		if i > 15 {
			at = minuteOf(60)
		}
		promotedRoot(b, at, fmt.Sprintf("ACME-%d", i), fmt.Sprintf("a%02d", i))
	}
	for minute := range 180 {
		uncached := int64(1500)
		if minute >= 60 {
			uncached = 5000
		}
		b.window.Provider = append(b.window.Provider, providerLine(minute, uncached, 0, 0))
	}
	guardKills(b, "legion-acme-acme-1", 10, 30, 50)
	guardKills(b, "legion-acme-acme-16", 70, 80, 90, 100, 110, 120, 130, 140, 150, 160)
	b.window.Queue = map[string][]json.RawMessage{builtRepository: queueLinesOf([]queuedWait{
		{added: minuteOf(20), left: minuteOf(30)}, {added: minuteOf(100), left: minuteOf(120)},
	})}
	b.window.Signals = map[string][]Span{
		SignalKills: {{From: minuteOf(0), To: minuteOf(180)}},
		SignalQueue: {{From: minuteOf(0), To: minuteOf(180)}},
	}
	return b.build()
}

// Beside each model, its signal's rate in each range of 20 issues in flight that the window records,
// with the hours the window spent in it (spec R11), on loadWindow: an hour in [0, 20) and two in
// [20, 40). The tokens are 90,000 over 900 issue-minutes in the first, 100 for each, and 600,000
// over 3,000 in the second, 200. The kills are 3 over 15 issue-hours and 10 over 50, 0.2 in each.
// acme/widgets' queue exits from its head once in each range, after 10 minutes that held a pull
// request in the first, 6 an hour, and after 20 in the second, 3 an hour.
func TestTheRatesByLoadOfEachModel(t *testing.T) {
	t.Parallel()
	models := fittedModels(t, loadWindow(t))
	for _, c := range []struct {
		model string
		want  []LoadBand
	}{
		{ModelTokens, []LoadBand{{From: 0, To: 20, Hours: 1, Count: 90_000, Exposure: 900}, {From: 20, To: 40, Hours: 2, Count: 600_000, Exposure: 3000}}},
		{ModelMemory, []LoadBand{{From: 0, To: 20, Hours: 1, Count: 3, Exposure: 15}, {From: 20, To: 40, Hours: 2, Count: 10, Exposure: 50}}},
		{ModelQueue, []LoadBand{{Repository: builtRepository, From: 0, To: 20, Hours: 1, Count: 1, Exposure: 1.0 / 6},
			{Repository: builtRepository, From: 20, To: 40, Hours: 2, Count: 1, Exposure: 1.0 / 3}}},
	} {
		got := modelNamed(t, models, c.model).Load
		if len(got) != len(c.want) {
			t.Errorf("the model %s gives the rates by load %+v, want %+v", c.model, got, c.want)
			continue
		}
		for i, want := range c.want {
			band := got[i]
			name := fmt.Sprintf("%s [%d, %d)", c.model, want.From, want.To)
			if band.Repository != want.Repository || band.From != want.From || band.To != want.To {
				t.Errorf("%s is the range %s [%d, %d)", name, band.Repository, band.From, band.To)
				continue
			}
			assertNear(t, name+" hours", band.Hours, want.Hours)
			assertNear(t, name+" count", band.Count, want.Count)
			assertNear(t, name+" exposure", band.Exposure, want.Exposure)
			assertNearValue(t, name+" rate", band.Rate, want.Count/want.Exposure)
		}
	}
	checkPrinted(t, printedModels(t, models),
		"| [0, 20) | 1 | 90000 | 900 | 100 |",
		"| acme/widgets | [20, 40) | 2 | 1 | 0.3333 | 3 |",
	)
}
