package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
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
func assertNearValue[T int64 | float64](t *testing.T, name string, got *T, want float64) {
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
// each one's added_to_merge_queue and its merged.
func queueLinesOf(waits []queuedWait) []json.RawMessage {
	var lines []json.RawMessage
	for i, wait := range waits {
		lines = append(lines, queueLine(i+1, queueAdded, wait.added), queueLine(i+1, queueMerged, wait.left))
	}
	return lines
}

// passFailWindow is a window of four days from 2001-03-05T00:00Z whose one issue, ACME-1, is in
// flight from the window's start to its end. It holds a record of the memory guard's kills from
// the start to 2001-03-07T11:00Z and from 2001-03-08T00:00Z to 09:00Z, and of the merge queue over
// the whole span; and none of the provider. The launcher's lines record 20 kills on the first day,
// 38 on the second, 5 in the record's 11 hours of the third and 5 in its 9 hours of the fourth,
// and 3 more on each of the last two days outside the record. Nine pull requests leave the merge
// queue: on the first day after waits of 1, 1.2 and 1.4 hours, on the second after 0.5, 2 and 2
// hours, on the third after 0.6, 0.8 and 1 hour, and none on the fourth.
func passFailWindow(t *testing.T) Window {
	b := newWindowBuilder(t, minuteOf(0), minuteOf(96*60))
	b.boot(minuteOf(-60))
	promotedRoot(b, minuteOf(0), "ACME-1", "a1")
	pid := 1000
	kill := func(minute int) {
		pid++
		b.window.Launcher = append(b.window.Launcher, LauncherLine{Time: minuteOf(minute), Pod: "legion-acme-acme-1", Text: guardKillText(pid, "implementer")})
	}
	for i := range 20 {
		kill(30 + 60*i)
	}
	for i := range 38 {
		kill(1440 + 15 + 35*i)
	}
	for i := range 5 {
		kill(2880 + 60 + 120*i)
	}
	for _, minute := range []int{3600, 3720, 3840} {
		kill(minute)
	}
	for i := range 5 {
		kill(4320 + 30 + 90*i)
	}
	for _, minute := range []int{5100, 5280, 5460} {
		kill(minute)
	}
	var waits []queuedWait
	for _, wait := range [][2]int{{120, 60}, {240, 72}, {360, 84}, {1560, 30}, {1680, 120}, {1860, 120}, {3000, 36}, {3120, 48}, {3240, 60}} {
		waits = append(waits, queuedWait{added: minuteOf(wait[0]), left: minuteOf(wait[0] + wait[1])})
	}
	b.window.Queue = map[string][]json.RawMessage{builtRepository: queueLinesOf(waits)}
	b.window.Signals = map[string][]Span{
		SignalKills: {{From: minuteOf(0), To: minuteOf(59 * 60)}, {From: minuteOf(72 * 60), To: minuteOf(81 * 60)}},
		SignalQueue: {{From: minuteOf(0), To: minuteOf(96 * 60)}},
	}
	return b.build()
}

// R11's daily test of the load models (spec R11, plan t31), on the four days of passFailWindow, at
// one issue in flight all through. The kill model is fitted on the 68 issue-hours of its record,
// which holds 68 kills, so it gives 1 kill per issue-hour in flight: 24 on each of the first two
// days, 11 in the third's 11 hours of record and 9 in the fourth's 9. With at least 10 kills
// recorded, the first day's 20 is within 30% of 24 and passes, and the second's 38 is not and
// fails; with fewer recorded, the third day's 11 modelled is more than 10 and fails, and the
// fourth's 9 passes. The merge queue's model gives a wait of the issues in flight times one of the
// record's waits per issue in flight, so at one issue in flight each day's modelled median is the
// median of all nine waits, 1 hour: within 30% of the first day's 1.2 and the third's 0.8, and not
// of the second's 2; the fourth day, on which no pull request left the queue, has no test. Each
// model that fails a day fails, and the output says it gives no limit for its signal; the provider,
// of which the window holds no record, is not fitted and gives no test.
func TestModelsPassAndFailRsDailyTest(t *testing.T) {
	window := passFailWindow(t)
	models := fittedModels(t, window)
	checkDaysInFlight(t, window, models)

	memory := modelNamed(t, models, ModelMemory)
	if !memory.Fitted || memory.Signal != SignalKills {
		t.Errorf("the memory model is fitted %t on the signal %q, want fitted on %q", memory.Fitted, memory.Signal, SignalKills)
	}
	assertNear(t, "the kill model's rate", parameterOf(t, memory, paramKillRate), 1)
	checkDays(t, memory, []wantDay{
		{day: "2001-03-05", hours: 24, recorded: 20, modelled: 24, verdict: VerdictPasses},
		{day: "2001-03-06", hours: 24, recorded: 38, modelled: 24, verdict: VerdictFails},
		{day: "2001-03-07", hours: 11, recorded: 5, modelled: 11, verdict: VerdictFails},
		{day: "2001-03-08", hours: 9, recorded: 5, modelled: 9, verdict: VerdictPasses},
	})
	if memory.Verdict != VerdictFails || memory.GivesLimit {
		t.Errorf("the memory model %s and gives a limit %t, want it to fail and give none", memory.Verdict, memory.GivesLimit)
	}

	queue := modelNamed(t, models, ModelQueue)
	if !queue.Fitted || queue.Signal != SignalQueue {
		t.Errorf("the merge queue's model is fitted %t on the signal %q, want fitted on %q", queue.Fitted, queue.Signal, SignalQueue)
	}
	checkDays(t, queue, []wantDay{
		{day: "2001-03-05", hours: 24, recorded: 1.2, modelled: 1, verdict: VerdictPasses},
		{day: "2001-03-06", hours: 24, recorded: 2, modelled: 1, verdict: VerdictFails},
		{day: "2001-03-07", hours: 24, recorded: 0.8, modelled: 1, verdict: VerdictPasses},
		{day: "2001-03-08", hours: 24, verdict: VerdictNoTest, reason: reasonNoExit},
	})
	if queue.Verdict != VerdictFails || queue.GivesLimit {
		t.Errorf("the merge queue's model %s and gives a limit %t, want it to fail and give none", queue.Verdict, queue.GivesLimit)
	}

	tokens := modelNamed(t, models, ModelTokens)
	if tokens.Fitted || tokens.Verdict != VerdictNoTest || tokens.GivesLimit || tokens.Reason == "" {
		t.Errorf("the tokens model is fitted %t, %s (%q), gives a limit %t; want it not fitted, no test with its reason, and no limit",
			tokens.Fitted, tokens.Verdict, tokens.Reason, tokens.GivesLimit)
	}

	printed := printedModels(t, models)
	for _, line := range []string{
		"The model fails R11's daily test, so it gives no limit for the signal `kills`.",
		"The model fails R11's daily test, so it gives no limit for the signal `queue`.",
		fmt.Sprintf("The model gives no test: %s, so it gives no limit for the signal `provider`.", tokens.Reason),
		"- from 2001-03-07T11:00:00Z to 2001-03-08T00:00:00Z: " + reasonNoKills,
		"- from 2001-03-08T09:00:00Z to 2001-03-09T00:00:00Z: " + reasonNoKills,
	} {
		if !strings.Contains(printed, line+"\n") {
			t.Errorf("the load models print no line %q:\n%s", line, printed)
		}
	}
}

// missingRecordWindow is a window of three days from 2001-03-05T00:00Z whose two issues, ACME-1
// and ACME-2, are in flight from its start to its end. The provider's record holds every minute,
// 1,000 uncached input tokens each, with the project's own limit of 6,000 a minute on the first
// and the third day and no limit observed on the second, on which the provider refuses 4 calls;
// the window holds a record of the provider's headroom in the minutes with a limit, as collect
// writes it. The memory guard runs from the second day: the record of its kills starts there, the
// launcher's lines hold 12 kills on each of the second and the third day, and 30 on the first,
// before the record; and the pods' memory, read each hour, is 3 GB a pod from the second day and 9
// GB a pod on the first. The window holds no record of the merge queue.
func missingRecordWindow(t *testing.T) Window {
	b := newWindowBuilder(t, minuteOf(0), minuteOf(72*60))
	b.boot(minuteOf(-60))
	promotedRoot(b, minuteOf(0), "ACME-1", "a1")
	promotedRoot(b, minuteOf(0), "ACME-2", "a2")
	source, scope := "dispatch", projectLimitScope
	for minute := range 72 * 60 {
		line := ProviderMinute{Minute: minuteOf(minute), Uncached: 1000}
		if minute < 24*60 || minute >= 48*60 {
			limit := int64(6000)
			line.Limit, line.LimitSource, line.LimitScope = &limit, &source, &scope
		}
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
	for hour := range 72 {
		used := int64(3_000_000_000)
		if hour < 24 {
			used = 9_000_000_000
		}
		for _, pod := range []string{"legion-acme-acme-1", "legion-acme-acme-2"} {
			b.window.Memory = append(b.window.Memory, MemorySample{Minute: minuteOf(60 * hour), Pod: pod, Bytes: used})
		}
	}
	b.window.Signals = map[string][]Span{
		SignalProvider: {{From: minuteOf(0), To: minuteOf(24 * 60)}, {From: minuteOf(48 * 60), To: minuteOf(72 * 60)}},
		SignalKills:    {{From: minuteOf(24 * 60), To: minuteOf(72 * 60)}},
	}
	return b.build()
}

// A load model is fitted and tested only in the spans where the window holds its record (plan Q4,
// departure 10), on missingRecordWindow at two issues in flight. The memory guard's record starts
// on the second day: the kill model is fitted on its 24 kills over 96 issue-hours, 0.25 kills an
// issue-hour, and the pods' memory on 6 GB at two issues, 3 GB an issue, the first day's 30 kills
// and 9 GB pods read by neither; it gives 12 kills on each of the second and the third day, as
// recorded, and the first day has no test, a span the output names. The tokens model is fitted on
// every minute of the provider's record, 500 tokens an issue in flight, and still where no limit is
// recorded; it gives no refusal at a limit of 6,000 on the first and the third day, as recorded,
// and on the second day, which records no limit, the project's own refusals have no test, never a
// pass, while the provider's 4 refusals are kept as recorded. The merge queue's model, with no
// record anywhere, is not fitted, gives no test and gives no limit.
func TestAModelIsNotFittedWhereItsRecordIsMissing(t *testing.T) {
	window := missingRecordWindow(t)
	models := fittedModels(t, window)
	checkDaysInFlight(t, window, models)

	memory := modelNamed(t, models, ModelMemory)
	assertNear(t, "the kill model's rate", parameterOf(t, memory, paramKillRate), 0.25)
	assertNear(t, "the memory model's bytes per issue in flight", parameterOf(t, memory, paramBytesPerIssue), 3e9)
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
	if len(memory.Days) == 3 {
		assertNearValue(t, "the pods' memory recorded on the second day", memory.Days[1].LoadRecorded, 6e9)
		assertNearValue(t, "the pods' memory modelled on the second day", memory.Days[1].LoadModelled, 6e9)
	}

	tokens := modelNamed(t, models, ModelTokens)
	if !tokens.Fitted || tokens.Signal != SignalProvider {
		t.Errorf("the tokens model is fitted %t on the signal %q, want fitted on %q", tokens.Fitted, tokens.Signal, SignalProvider)
	}
	assertNear(t, "the tokens model's minutes", parameterOf(t, tokens, paramMinutesFitted), 3*24*60)
	assertNear(t, "the tokens model's mean tokens an issue", parameterOf(t, tokens, paramTokensMean), 500)
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

	printed := printedModels(t, models)
	for _, line := range []string{
		"- from 2001-03-05T00:00:00Z to 2001-03-06T00:00:00Z: " + reasonNoKills,
		"- from 2001-03-06T00:00:00Z to 2001-03-07T00:00:00Z: " + reasonNoLimit,
		"- from 2001-03-05T00:00:00Z to 2001-03-08T00:00:00Z: " + reasonNoQueue,
		fmt.Sprintf("The model gives no test: %s, so it gives no limit for the signal `queue`.", queue.Reason),
		"The model passes R11's daily test on each day it is tested, so it gives a limit for the signal `kills`.",
	} {
		if !strings.Contains(printed, line+"\n") {
			t.Errorf("the load models print no line %q:\n%s", line, printed)
		}
	}
}
