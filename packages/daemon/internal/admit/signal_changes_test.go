package admit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// changeSeries is one series of the writer's table (signalSteps) as these tests drive it: S6's
// step of it, and four values a float reads exactly as the test writes them: base, the value last
// written; atStep, base moved by exactly the step, whose difference from base a float holds just
// under the step; below, a move of less than the step; and downBelow, atStep moved back by less
// than the step.
type changeSeries struct {
	step, base, atStep, below, downBelow float64
}

// changeRows is one row for each series of store.LiveSeries(), which TestSignalChangeInventory
// holds to the writer's table.
var changeRows = map[string]changeSeries{
	signals.Idle:      {step: 0.05, base: 0.30, atStep: 0.35, below: 0.34, downBelow: 0.31},
	signals.Queue:     {step: 0.25, base: 0.10, atStep: 0.35, below: 0.34, downBelow: 0.11},
	signals.Provider:  {step: 0.10, base: 0.20, atStep: 0.30, below: 0.29, downBelow: 0.21},
	signals.Kills:     {step: 5, base: 3, atStep: 8, below: 7, downBelow: 4},
	signals.Approvals: {step: 5, base: 3, atStep: 8, below: 7, downBelow: 4},
}

// Every series of store.LiveSeries() is a row of the writer's table at S6's step (idle 0.05, queue
// 0.25 hours, the provider's headroom 0.10, the kills and the approvals 5), and a row of
// changeRows, which every row of TestSignalChanges runs over: a series with no row fails here, and
// so does a row of the writer's of a series the live daemon does not compute.
func TestSignalChangeInventory(t *testing.T) {
	for _, name := range store.LiveSeries() {
		row, held := changeRows[name]
		if !held {
			t.Errorf("the series %s has no row of changeRows", name)
			continue
		}
		if got, set := signalSteps[name]; !set || got != row.step {
			t.Errorf("the writer's step of %s is %v (set %t), want S6's %v", name, got, set, row.step)
		}
	}
	if len(signalSteps) != len(store.LiveSeries()) || len(changeRows) != len(store.LiveSeries()) {
		t.Errorf("the writer's table holds %d series and changeRows %d, want the %d of store.LiveSeries()", len(signalSteps), len(changeRows), len(store.LiveSeries()))
	}
}

// seriesAt is name reading value at limit (nil for none), holding where admission by signals is on
// and the value binds the limit, as LiveSignals.Reading reads a series.
func seriesAt(name string, value float64, limit *float64, on bool) SeriesReading {
	series := SeriesReading{Name: name, Value: signals.Value{Recorded: true, Value: value}, Limit: limit}
	series.Holds = on && signals.Reading{Value: series.Value, Limit: limit}.Holds()
	return series
}

// absentAt is name not recorded, for why.
func absentAt(name, why string, limit *float64) SeriesReading {
	return SeriesReading{Name: name, Absent: why, Limit: limit}
}

// newestOf is the record's newest entries after entries, written over newest in their order.
func newestOf(newest map[string]store.SeriesEntries, entries ...store.SignalChange) map[string]store.SeriesEntries {
	next := make(map[string]store.SeriesEntries, len(newest))
	for name, of := range newest {
		next[name] = of
	}
	for i := range entries {
		entry := entries[i]
		part := ""
		if entry.Part != nil {
			part = *entry.Part
		}
		series, _ := store.SignalSeries(*entry.Signal, part)
		of := next[series]
		if entry.What == store.SignalChangeValue {
			of.Value = &entry
		} else {
			of.Holds = &entry
		}
		next[series] = of
	}
	return next
}

// written is the record holding value as name's last written value (nil for one the daemon could
// not compute) and holds as its last hold.
func written(name string, value *float64, holds bool) map[string]store.SeriesEntries {
	return newestOf(nil, store.SeriesValueChange(name, fixedNow.Add(-time.Hour), value, "it was not computed"),
		store.SeriesHoldsChange(name, fixedNow.Add(-time.Hour), holds))
}

// entryWords is entries as "<signal>/<part> <what> <value or absent>", one each, in their order.
func entryWords(entries []store.SignalChange) []string {
	words := make([]string, 0, len(entries))
	for _, entry := range entries {
		part, value := "", "null"
		if entry.Part != nil {
			part = *entry.Part
		}
		if entry.Value != nil {
			value = fmt.Sprint(*entry.Value)
		}
		if entry.Absent != nil {
			value += " (" + *entry.Absent + ")"
		}
		signal := "switch"
		if entry.Signal != nil {
			signal = *entry.Signal
		}
		words = append(words, fmt.Sprintf("%s/%s %s %s", signal, part, entry.What, value))
	}
	return words
}

// valueWord is name's value entry as entryWords names it.
func valueWord(name string, value float64) string {
	signal, part := store.SignalPart(name)
	return fmt.Sprintf("%s/%s value %v", signal, part, value)
}

// absentWord is name's value entry of a value the daemon cannot compute, for why.
func absentWord(name, why string) string {
	signal, part := store.SignalPart(name)
	return fmt.Sprintf("%s/%s value null (%s)", signal, part, why)
}

// holdsWord is name's holds entry as entryWords names it.
func holdsWord(name string, holds bool) string {
	signal, part := store.SignalPart(name)
	flag := 0
	if holds {
		flag = 1
	}
	return fmt.Sprintf("%s/%s holds %d", signal, part, flag)
}

// The entries one pass writes (passChanges, S6 and the paragraphs after it), plan section 5's
// table, each row over every series of the writer's table.
func TestSignalChanges(t *testing.T) {
	limit := func(value float64) *float64 { return &value }
	pass := func(newest map[string]store.SeriesEntries, moved bool, on bool, series ...SeriesReading) []string {
		return entryWords(passChanges(LiveReading{At: fixedNow, On: on, Series: series}, moved, newest))
	}
	expect := func(t *testing.T, what string, got []string, want ...string) {
		t.Helper()
		if want == nil {
			want = []string{}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s wrote %q, want %q", what, got, want)
		}
	}
	// first: the first value of each series writes, the headroom's at a pass that moves a slot, and
	// so does a first value the daemon cannot compute, with its reason.
	t.Run("first", func(t *testing.T) {
		for _, name := range store.LiveSeries() {
			row := changeRows[name]
			expect(t, name+"'s first value", pass(nil, true, true, seriesAt(name, row.base, nil, true)), valueWord(name, row.base))
			expect(t, name+"'s first value not computed", pass(nil, true, true, absentAt(name, "no input", nil)), absentWord(name, "no input"))
		}
	})
	// coverage: a change from a value to one the daemon cannot compute writes, with the reason and
	// no value, and so does the way back, with the value and no reason.
	t.Run("coverage", func(t *testing.T) {
		for _, name := range store.LiveSeries() {
			row := changeRows[name]
			expect(t, name+" losing its input", pass(written(name, &row.base, false), true, true, absentAt(name, "the input stopped", nil)),
				absentWord(name, "the input stopped"))
			expect(t, name+" regaining its input", pass(written(name, nil, false), true, true, seriesAt(name, row.base, nil, true)),
				valueWord(name, row.base))
			expect(t, name+" still not computed", pass(written(name, nil, false), true, true, absentAt(name, "another reason", nil)))
		}
	})
	// step: a move of less than the step writes nothing, one of exactly the step (a difference a
	// float holds just under it) and one above it write, in both directions, and the move is from
	// the last value written, never from a value a pass read and did not write.
	t.Run("step", func(t *testing.T) {
		for _, name := range store.LiveSeries() {
			row := changeRows[name]
			from := written(name, &row.base, false)
			expect(t, name+" moved below its step", pass(from, true, true, seriesAt(name, row.below, nil, true)))
			expect(t, name+" moved by its step", pass(from, true, true, seriesAt(name, row.atStep, nil, true)), valueWord(name, row.atStep))
			expect(t, name+" moved above its step", pass(from, true, true, seriesAt(name, row.base+2*row.step, nil, true)), valueWord(name, row.base+2*row.step))
			back := written(name, &row.atStep, false)
			expect(t, name+" moved back by its step", pass(back, true, true, seriesAt(name, row.base, nil, true)), valueWord(name, row.base))
			expect(t, name+" moved back below its step", pass(back, true, true, seriesAt(name, row.downBelow, nil, true)))
			// Two passes: the first reads below the step and writes nothing, so the second, at the
			// step from the last written value, writes.
			first := passChanges(LiveReading{At: fixedNow, On: true, Series: []SeriesReading{seriesAt(name, row.below, nil, true)}}, true, from)
			expect(t, name+"'s first of two passes", entryWords(first))
			expect(t, name+"'s second of two passes", pass(newestOf(from, first...), true, true, seriesAt(name, row.atStep, nil, true)), valueWord(name, row.atStep))
		}
	})
	// headroom-no-slot: the provider's headroom writes no value at a pass that moves no slot and
	// changes no hold, however far it moved, across its coverage too, and not its first value.
	t.Run("headroom-no-slot", func(t *testing.T) {
		row := changeRows[signals.Provider]
		from := written(signals.Provider, &row.base, false)
		expect(t, "the headroom moved far", pass(from, false, true, seriesAt(signals.Provider, 0.9, nil, true)))
		expect(t, "the headroom losing its input", pass(from, false, true, absentAt(signals.Provider, "no token limit", nil)))
		expect(t, "the headroom's first value", pass(nil, false, true, seriesAt(signals.Provider, row.base, nil, true)))
	})
	// headroom-slot: at a pass that gives out or takes back a slot, the headroom writes a value that
	// moved by its step, and none that moved less.
	t.Run("headroom-slot", func(t *testing.T) {
		row := changeRows[signals.Provider]
		from := written(signals.Provider, &row.base, false)
		expect(t, "the headroom moved by its step at a slot's move", pass(from, true, true, seriesAt(signals.Provider, row.atStep, nil, true)),
			valueWord(signals.Provider, row.atStep))
		expect(t, "the headroom moved below its step at a slot's move", pass(from, true, true, seriesAt(signals.Provider, row.below, nil, true)))
	})
	// forced-value: a pass where a series' hold changes writes its value beside the hold, once,
	// though the value moved by less than its step and, for the headroom, though no slot moved:
	// admission turned on, turned off, the input lost, and the limit raised.
	t.Run("forced-value", func(t *testing.T) {
		for _, name := range store.LiveSeries() {
			row := changeRows[name]
			binding := limit(row.base)
			below := row.base + row.step/10
			expect(t, name+" starting to hold", pass(written(name, &row.base, false), false, true, seriesAt(name, below, binding, true)),
				valueWord(name, below), holdsWord(name, true))
			held := written(name, &row.base, true)
			expect(t, name+" with admission by signals turned off", pass(held, false, false, seriesAt(name, below, binding, false)),
				valueWord(name, below), holdsWord(name, false))
			expect(t, name+" losing its input while it holds", pass(held, false, true, absentAt(name, "the input stopped", binding)),
				absentWord(name, "the input stopped"), holdsWord(name, false))
			expect(t, name+" under a limit raised above it", pass(held, false, true, seriesAt(name, below, limit(row.base+row.step), true)),
				valueWord(name, below), holdsWord(name, false))
		}
		// All five series starting to hold at one pass write ten entries, within S6's 16 (five
		// series, three kinds and the switch); the switch and the limits are their change's own.
		var all []SeriesReading
		for _, name := range store.LiveSeries() {
			all = append(all, seriesAt(name, changeRows[name].base, limit(changeRows[name].base), true))
		}
		if got := passChanges(LiveReading{At: fixedNow, On: true, Series: all}, false, nil); len(got) != 10 || len(got) > 16 {
			t.Errorf("every series starting to hold wrote %d entries, want 10, at most 16: %q", len(got), entryWords(got))
		}
	})
	// no-extras: a pass that computes what the record holds writes nothing, at the same moment or
	// later, since no clock writes; two passes close together that each qualify each write, since
	// no rate limit holds an entry.
	t.Run("no-extras", func(t *testing.T) {
		for _, name := range store.LiveSeries() {
			row := changeRows[name]
			from := written(name, &row.base, true)
			again := seriesAt(name, row.base, limit(row.base), true)
			expect(t, name+" as it stands", pass(from, true, true, again))
			expect(t, name+" as it stands an hour on", entryWords(passChanges(LiveReading{At: fixedNow.Add(time.Hour), On: true, Series: []SeriesReading{again}}, true, from)))
			first := passChanges(LiveReading{At: fixedNow, On: true, Series: []SeriesReading{seriesAt(name, row.atStep, limit(row.base), true)}}, true, from)
			expect(t, name+"'s first of two close passes", entryWords(first), valueWord(name, row.atStep))
			second := passChanges(LiveReading{At: fixedNow.Add(time.Second), On: true, Series: []SeriesReading{seriesAt(name, row.atStep+row.step, limit(row.base), true)}},
				true, newestOf(from, first...))
			expect(t, name+"'s second of two close passes", entryWords(second), valueWord(name, row.atStep+row.step))
		}
	})
	// off: while admission by signals is off the passes write each value that qualifies and no hold
	// of 1, whatever the limits, and the line starts as it always did.
	t.Run("off", func(t *testing.T) {
		for _, name := range store.LiveSeries() {
			row := changeRows[name]
			expect(t, name+"'s first value while off", pass(nil, true, false, seriesAt(name, row.base, limit(row.base), false)), valueWord(name, row.base))
		}
		pool := migratedPool(t)
		lt := newLiveTest(t, 3, nil, SignalSwitch{Limits: map[string]float64{signals.Kills: 1}})
		lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
		lt.kill(t, pool, 1, fixedNow.Add(-10*time.Minute))
		acmeQueuedRoot(t, pool, gateQueued, "b")
		lt.resume(t, pool, "off-pass")
		if got, want := openedWord(t, pool, gateQueued), "the trees open of [ACME-301] are [ACME-301]"; got != want {
			t.Errorf("after the pass with admission by signals off: %s, want %s", got, want)
		}
		entries := recordOf(t, pool)
		if words := entryWords(entries); !slices.Contains(words, valueWord(signals.Kills, 1)) {
			t.Errorf("the record holds %q, want the kills' value 1 written while off", words)
		}
		for _, entry := range entries {
			if entry.What == store.SignalChangeHolds && *entry.Value != 0 {
				t.Errorf("the record holds a hold of 1 while admission by signals is off: %q", entryWords([]store.SignalChange{entry}))
			}
		}
	})
	// no-cap: a project whose cap admits nothing starts nothing, and its passes still write what
	// they read of each series.
	t.Run("no-cap", func(t *testing.T) {
		pool := migratedPool(t)
		lt := newLiveTest(t, 0, nil, SignalSwitch{Limits: map[string]float64{signals.Kills: 1}})
		lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
		lt.kill(t, pool, 1, fixedNow.Add(-10*time.Minute))
		if words := entryWords(recordOf(t, pool)); !slices.Contains(words, valueWord(signals.Kills, 1)) {
			t.Errorf("the record of a project of no cap holds %q, want the kills' value 1 its pass read", words)
		}
	})
	// settings: a person's change of the switch and of a limit writes its entry, with who made it,
	// when and why, set, cleared and repeated; a change to the value a setting holds writes none; a
	// refused change and a rolled-back one write nothing and leave no gap; the ids rise by one.
	t.Run("settings", func(t *testing.T) {
		pool := migratedPool(t)
		lt := storeSettingsTest(t, &testlog.Buffer{})
		lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
		operator := store.PauseActor{Kind: store.PauseByOperator}
		controller := store.PauseActor{Kind: store.PauseByController, Session: "ses_controller"}
		at := fixedNow.Add(-time.Minute)
		changes := []struct {
			name   string
			change store.SignalSettingChange
			want   []string
		}{
			// A fresh project is off, so turning it off moves nothing and writes no entry.
			{"off on a fresh project", settingChange(store.SignalSwitchSetting, limit(0), operator, at.Add(-time.Second), "the owner turned it off"), nil},
			{"on", settingChange(store.SignalSwitchSetting, limit(1), operator, at, "the owner turned it on"), []string{"switch/ switch 1"}},
			{"set", settingChange(signals.Kills, limit(5), controller, at.Add(time.Second), "the owner set it"), []string{"kills/ limit 5"}},
			{"set the headroom", settingChange(signals.Provider, limit(0.8), operator, at.Add(2*time.Second), "the owner set it"), []string{"provider/headroom limit 0.8"}},
			{"set again", settingChange(signals.Kills, limit(5), operator, at.Add(3*time.Second), "the owner set it again"), nil},
			{"clear", settingChange(signals.Kills, nil, controller, at.Add(4*time.Second), "the owner cleared it"), []string{"kills/ limit null"}},
			{"clear again", settingChange(signals.Kills, nil, controller, at.Add(5*time.Second), "the owner cleared it again"), nil},
			{"off", settingChange(store.SignalSwitchSetting, limit(0), operator, at.Add(6*time.Second), "the owner turned it off"), []string{"switch/ switch 0"}},
		}
		before := recordOf(t, pool)
		for i, tc := range changes {
			applyThrough(t, pool, lt.admission, "api", fmt.Sprintf("setting-%d", i), intake.SignalSettingChanged{Change: tc.change})
			after := recordOf(t, pool)
			var settingEntries []store.SignalChange
			for _, entry := range after[:len(after)-len(before)] {
				if entry.What == store.SignalChangeSwitch || entry.What == store.SignalChangeLimit {
					settingEntries = append(settingEntries, entry)
					if entry.SetBy == nil || *entry.SetBy != tc.change.Set.By || entry.Reason == nil || *entry.Reason != tc.change.Set.Reason ||
						!entry.At.Equal(tc.change.Set.At) {
						t.Errorf("%s wrote %+v, want who made it (%+v), why (%q) and when (%s)", tc.name, entry, tc.change.Set.By, tc.change.Set.Reason, tc.change.Set.At)
					}
				} else if entry.SetBy != nil || entry.Reason != nil {
					t.Errorf("%s's pass wrote %+v, want no setter and no reason on an entry of a value or a hold", tc.name, entry)
				}
			}
			slices.Reverse(settingEntries)
			if got := entryWords(settingEntries); !slices.Equal(got, append([]string{}, tc.want...)) && !(len(got) == 0 && tc.want == nil) {
				t.Errorf("%s wrote %q of a setting, want %q", tc.name, got, tc.want)
			}
			before = after
		}
		// A limit of 0 is outside the kills' range, and the live daemon computes no series of the
		// provider's refusals (store.LiveSeries), so neither is a setting it keeps.
		for i, refused := range []store.SignalSettingChange{
			settingChange(signals.Kills, limit(0), operator, at.Add(7*time.Second), "a limit of 0"),
			settingChange(signals.Refusals, limit(2), operator, at.Add(7*time.Second), "a limit of the provider's refusals"),
		} {
			if _, err := intake.ApplyFact(context.Background(), pool, "api", fmt.Sprintf("setting-refused-%d", i), intake.SignalSettingChanged{Change: refused},
				Handlers(record.NewStore(), acmeProject, lt.admission.engine, lt.admission)...); err == nil {
				t.Fatalf("%s was written, want it refused", refused.Set.Reason)
			}
		}
		failing := handlerOf(func(context.Context, pgx.Tx, intake.Fact) (intake.Result, error) {
			return intake.Result{}, errors.New("the transaction fails after admission")
		})
		if _, err := intake.ApplyFact(context.Background(), pool, "api", "setting-rolled-back",
			intake.SignalSettingChanged{Change: settingChange(signals.Queue, limit(2), operator, at.Add(8*time.Second), "rolled back")},
			append(Handlers(record.NewStore(), acmeProject, lt.admission.engine, lt.admission), failing)...); err == nil {
			t.Fatal("the rolled-back change committed, want it rolled back")
		}
		if after := recordOf(t, pool); len(after) != len(before) {
			t.Fatalf("the refused and the rolled-back change wrote %q, want nothing", entryWords(after[:len(after)-len(before)]))
		}
		applyThrough(t, pool, lt.admission, "api", "setting-after", intake.SignalSettingChanged{Change: settingChange(signals.Queue, limit(2), operator, at.Add(9*time.Second), "kept")})
		entries := recordOf(t, pool)
		for i, entry := range entries {
			if want := int64(len(entries) - i); entry.ID != want {
				t.Fatalf("the record's ids are %v newest first, want them to rise by one from 1 with no gap", idsOf(entries))
			}
		}
		if newest := entries[0]; newest.What != store.SignalChangeLimit || *newest.Signal != signals.Queue || *newest.Value != 2 {
			t.Errorf("the change after the refused and the rolled-back one wrote %q, want the queue's limit 2", entryWords(entries[:1]))
		}
	})
	// log: one line once each change of the switch, of a limit and of a hold commits, and none for a
	// value alone or a change rolled back.
	t.Run("log", func(t *testing.T) {
		pool := migratedPool(t)
		logged := &testlog.Buffer{}
		lt := storeSettingsTest(t, logged)
		lt.boot(t, pool, fixedNow.Add(-7*time.Hour), "acme-owner@example.invalid")
		operator := store.PauseActor{Kind: store.PauseByOperator}
		applyThrough(t, pool, lt.admission, "api", "on", intake.SignalSettingChanged{Change: settingChange(store.SignalSwitchSetting, limit(1), operator, fixedNow, "on")})
		applyThrough(t, pool, lt.admission, "api", "limit", intake.SignalSettingChanged{Change: settingChange(signals.Kills, limit(1), operator, fixedNow, "at one kill")})
		if got, want := changeLines(t, logged), []string{"setting switch 1", "setting kills 1"}; !slices.Equal(got, want) {
			t.Errorf("the two settings logged %q, want %q", got, want)
		}
		lt.kill(t, pool, 1, fixedNow.Add(-10*time.Minute))
		if got, want := changeLines(t, logged), []string{"setting switch 1", "setting kills 1", "holds kills true"}; !slices.Equal(got, want) {
			t.Errorf("the kill at the limit logged %q, want %q", got, want)
		}
		// Five more kills move the kills by their step while they hold: a value entry, and no line.
		for sequence := uint64(2); sequence <= 6; sequence++ {
			lt.kill(t, pool, sequence, fixedNow.Add(-5*time.Minute))
		}
		if words := entryWords(recordOf(t, pool)); !slices.Contains(words, valueWord(signals.Kills, 6)) {
			t.Fatalf("the record holds %q after six kills, want the kills' value 6", words)
		}
		failing := handlerOf(func(context.Context, pgx.Tx, intake.Fact) (intake.Result, error) {
			return intake.Result{}, errors.New("the transaction fails after admission")
		})
		if _, err := intake.ApplyFact(context.Background(), pool, "api", "off-rolled-back",
			intake.SignalSettingChanged{Change: settingChange(store.SignalSwitchSetting, limit(0), operator, fixedNow, "off")},
			append(Handlers(record.NewStore(), acmeProject, lt.admission.engine, lt.admission), failing)...); err == nil {
			t.Fatal("the rolled-back change committed, want it rolled back")
		}
		if got, want := changeLines(t, logged), []string{"setting switch 1", "setting kills 1", "holds kills true"}; !slices.Equal(got, want) {
			t.Errorf("a value moved by its step and a rolled-back change logged %q, want no more than %q", got, want)
		}
	})
}

// handlerOf is an intake.Handler of one function.
type handlerOf func(context.Context, pgx.Tx, intake.Fact) (intake.Result, error)

func (f handlerOf) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	return f(ctx, tx, fact)
}

// storeSettingsTest is a liveTest whose settings are the store's (StoreSignalSettings), as the
// daemon registers them, logging to logged.
func storeSettingsTest(t *testing.T, logged *testlog.Buffer) *liveTest {
	t.Helper()
	lt := newLiveTest(t, 3, nil, SignalSwitch{})
	lt.logged = logged
	lt.admission.log = slog.New(slog.NewJSONHandler(logged, nil))
	lt.live = lt.admission.UseLiveSignals(StoreSignalSettings{}, 0, func(record.SignalDeadline) {})
	return lt
}

// settingChange is a person's change of setting to value by by at at, for reason.
func settingChange(setting string, value *float64, by store.PauseActor, at time.Time, reason string) store.SignalSettingChange {
	return store.SignalSettingChange{Setting: setting, Value: value, Set: store.SignalSet{By: by, At: at, Reason: reason}}
}

// recordOf is every entry of acmeProject's record, newest first.
func recordOf(t *testing.T, pool *pgxpool.Pool) []store.SignalChange {
	t.Helper()
	var entries []store.SignalChange
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if entries, err = store.SignalChangesTx(context.Background(), tx, acmeProject, 1000); err != nil {
			t.Fatalf("read the record of changes: %v", err)
		}
	})
	return entries
}

// idsOf is each entry's id.
func idsOf(entries []store.SignalChange) []int64 {
	ids := make([]int64, len(entries))
	for i, entry := range entries {
		ids[i] = entry.ID
	}
	return ids
}

// changeLines is each line logged of a change of a setting or of a hold, as "setting <setting>
// <value>" and "holds <signal> <holds>", in the order logged.
func changeLines(t *testing.T, logged *testlog.Buffer) []string {
	t.Helper()
	var lines []string
	for _, raw := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		var line struct {
			Msg     string   `json:"msg"`
			Setting string   `json:"setting"`
			Value   *float64 `json:"value"`
			Signal  string   `json:"signal"`
			Holds   bool     `json:"holds"`
		}
		if raw == "" || json.Unmarshal([]byte(raw), &line) != nil {
			continue
		}
		switch line.Msg {
		case SignalSettingLine:
			value := "none"
			if line.Value != nil {
				value = fmt.Sprint(*line.Value)
			}
			lines = append(lines, "setting "+line.Setting+" "+value)
		case SignalHoldLine:
			lines = append(lines, fmt.Sprintf("holds %s %t", line.Signal, line.Holds))
		}
	}
	return lines
}
