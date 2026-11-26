package admit

import (
	"context"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The record of changes of admission by signals (S6) as admission writes it. A person's change of
// the switch or of a limit writes the setting and its entry together, in the transaction of the
// fact it is applied as (keepSignalSetting, store.ChangeSignalSettingTx), before that fact's pass,
// so each change is kept with who made it and why even while the project is paused, and the pass
// that follows computes the limit and the switch the newest entry of each holds. Each admission
// pass then writes, from its own reading of the six series (the reading its gate read, Read) and
// whether it gave out or took back a slot, the entries of what it computed against the newest
// entry of each series and kind (writeSignalChanges, passChanges): each hold that changed, with the
// value of that series beside it, and each other value that moved by its series' step from the
// last one written. The newest entries are the last committed pass's, which the series-moved pass rule
// for a report and a turn edge compares with too (signalsMoved). No clock writes an entry: only a
// pass that runs does.

// SignalHoldLine is the line of each change of a series' hold, once the pass that changed it
// commits: the series by its signal and part, whether it holds now, its value and its limit.
const SignalHoldLine = "admission: a signal's hold changed"

// SignalSettingLine is the line of each change of a setting of admission by signals, once the
// fact of the change commits: the setting, its new value (none for a cleared limit), who changed it
// and why.
const SignalSettingLine = "admission: a setting of admission by signals changed"

// passReadingKey keys, in a pass's ctx, the reading the pass's gate read (withPassReading).
type passReadingKey struct{}

// passReading is the live reading one pass's gate read, where read says it read one.
type passReading struct {
	reading LiveReading
	read    bool
}

// withPassReading is ctx keeping the reading the pass it runs reads at its gate (keepPassReading):
// one pass at a time runs under one fact's ctx, so the record of changes the pass writes at its end
// is of the reading its every judgement shared.
func withPassReading(ctx context.Context) context.Context {
	return context.WithValue(ctx, passReadingKey{}, &passReading{})
}

// keepPassReading keeps reading as the pass's in ctx; a ctx that keeps none keeps nothing.
func keepPassReading(ctx context.Context, reading LiveReading) {
	if held, ok := ctx.Value(passReadingKey{}).(*passReading); ok {
		held.reading, held.read = reading, true
	}
}

// passReadingOf is the reading the pass of ctx read at its gate, and whether it read one.
func passReadingOf(ctx context.Context) (LiveReading, bool) {
	held, ok := ctx.Value(passReadingKey{}).(*passReading)
	if !ok || !held.read {
		return LiveReading{}, false
	}
	return held.reading, true
}

// holdsWritten is whether the newest holds entry of a series says it holds admission; a series no
// pass wrote a hold of holds nothing, as every series does while admission by signals is off.
func holdsWritten(newest store.SeriesEntries) bool {
	return newest.Holds != nil && newest.Holds.Value != nil && *newest.Holds.Value == 1
}

// valueEntryDue says whether series' value qualifies for a value entry against newest, the last one
// written (S6): the first value of the series, a change between a value and one the daemon cannot
// compute, or a move of at least the series' step, a difference equal to the step included. A
// float's difference of a step is read with a margin of a billionth of the step, so a share of 0.35
// against 0.30, whose difference a float holds as just under 0.05, moves by the step it reads as.
func valueEntryDue(series SeriesReading, newest store.SeriesEntries) bool {
	last := newest.Value
	switch {
	case last == nil, series.Value.Recorded != (last.Value != nil):
		return true
	case !series.Value.Recorded:
		return false
	}
	return math.Abs(series.Value.Value-*last.Value) >= signalSteps[series.Name]*(1-1e-9)
}

// signalsMoved is the series-moved pass rule, which decides whether a report of model requests or
// the edge of a slot holder's turn runs an admission pass: whether reading moved what the last
// committed pass recorded (newest, the record's newest entries of each series): a series whose
// hold differs from the hold that pass recorded, or one of the five series but the provider's
// headroom whose value qualifies for a value entry against its last written value
// (valueEntryDue). It compares with the last committed pass and never with the state before the
// fact at the same moment, so a crossing that came with no event since that pass moves it.
func signalsMoved(reading LiveReading, newest map[string]store.SeriesEntries) bool {
	for _, series := range reading.Series {
		last := newest[series.Name]
		if series.Holds != holdsWritten(last) || series.Name != signals.Provider && valueEntryDue(series, last) {
			return true
		}
	}
	return false
}

// passChanges is the entries a pass whose gate read reading writes against newest, the record's
// newest entries of each series, where moved says the pass gave out or took back a slot (S6): for
// each series in the order of signals.Names(), its value where its hold changed (once, whatever
// its step) or where it qualifies by its step (valueEntryDue), the provider's headroom only at a
// pass that moved a slot or changed its hold, and then its hold where it changed. A hold is 0
// while admission by signals is off and for a value the daemon cannot compute (SeriesReading.Holds).
// The switch and the limits are written with their change (keepSignalSetting), so a pass writes
// at most two entries of each of the six series, within the 19 of S6.
func passChanges(reading LiveReading, moved bool, newest map[string]store.SeriesEntries) []store.SignalChange {
	var entries []store.SignalChange
	for _, series := range reading.Series {
		last := newest[series.Name]
		held := series.Holds != holdsWritten(last)
		if held || valueEntryDue(series, last) && (series.Name != signals.Provider || moved) {
			var value *float64
			if series.Value.Recorded {
				value = &series.Value.Value
			}
			entries = append(entries, store.SeriesValueChange(series.Name, reading.At, value, series.Absent))
		}
		if held {
			entries = append(entries, store.SeriesHoldsChange(series.Name, reading.At, series.Holds))
		}
	}
	return entries
}

// writeSignalChanges writes the entries of the pass whose gate read reading (passChanges), in the
// pass's own transaction, and one line for each hold that changed once it commits.
func (a *Admission) writeSignalChanges(ctx context.Context, tx pgx.Tx, reading LiveReading, moved bool) error {
	newest, err := store.NewestSeriesEntriesTx(ctx, tx, a.project)
	if err != nil {
		return err
	}
	if err := store.AppendSignalChangesTx(ctx, tx, a.project, passChanges(reading, moved, newest)); err != nil {
		return err
	}
	for _, series := range reading.Series {
		if series.Holds == holdsWritten(newest[series.Name]) {
			continue
		}
		signal, part := store.SignalPart(series.Name)
		a.logOnCommit(ctx, SignalHoldLine, "signal", signal, "part", partOrNone(part), "holds", series.Holds, "value", valueOrAbsent(series), "limit", numberOrNone(series.Limit))
	}
	return nil
}

// numberOrNone is a limit as a line names it: the number, or nothing for none.
func numberOrNone(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

// partOrNone is a series' part as a line names it (store.SignalPart): the part, or nothing for a
// series of none.
func partOrNone(part string) any {
	if part == "" {
		return nil
	}
	return part
}

// valueOrAbsent is series' value as a line names it: the number, or why the daemon cannot compute it.
func valueOrAbsent(series SeriesReading) any {
	if series.Value.Recorded {
		return series.Value.Value
	}
	return series.Absent
}

// keepSignalSetting writes a person's change of a setting of admission by signals and its entry
// (store.ChangeSignalSettingTx) in the fact's transaction, and answers whether it moved the
// setting. Where it did it keeps the mark that runs the fact's pass (record.NoteSignalsMoved),
// which reads the change, and writes one line once the fact commits; a change to the value the
// setting holds already writes nothing and runs no pass.
func (a *Admission) keepSignalSetting(ctx context.Context, tx pgx.Tx, change store.SignalSettingChange) (bool, error) {
	changed, err := store.ChangeSignalSettingTx(ctx, tx, a.project, change)
	if err != nil || !changed {
		return false, err
	}
	record.NoteSignalsMoved(ctx)
	var signal, part any
	if change.Setting != store.SignalSwitchSetting {
		named, of := store.SignalPart(change.Setting)
		signal, part = named, partOrNone(of)
	}
	a.logOnCommit(ctx, SignalSettingLine, "setting", change.Setting, "signal", signal, "part", part, "value", numberOrNone(change.Value),
		"by", string(change.Set.By.Kind), "session", change.Set.By.Session, "reason", change.Set.Reason, "at", change.Set.At)
	return true, nil
}

// State is admission by signals as the state shows it at at (store.SignalsReading), read through
// tx, the state's own snapshot: the switch and who set it, each series as the live signals read it
// at at from the inputs the daemon keeps (Reading), never from an entry, with its limit and who set
// it, since when its hold stands as the record's newest holds entry dates it, and the provider's
// whole minutes of the hour before the minute under way.
func (l *LiveSignals) State(ctx context.Context, tx pgx.Tx, at time.Time) (store.SignalsReading, error) {
	a := l.admission
	reading, minutes, settings, err := l.read(ctx, tx, at)
	if err != nil {
		return store.SignalsReading{}, err
	}
	newest, err := store.NewestSeriesEntriesTx(ctx, tx, a.project)
	if err != nil {
		return store.SignalsReading{}, err
	}
	state := store.SignalsReading{On: reading.On, Switch: settings.View().Switch, Series: make([]store.SignalSeriesReading, 0, len(reading.Series))}
	for _, series := range reading.Series {
		signal, part := store.SignalPart(series.Name)
		row := store.SignalSeriesReading{Signal: signal, Limit: series.Limit, Holds: series.Holds}
		if part != "" {
			row.Part = &part
		}
		if series.Value.Recorded {
			value := series.Value.Value
			row.Value = &value
		} else {
			absent := series.Absent
			row.Absent = &absent
		}
		if setting, set := settings.Limits[series.Name]; set {
			last := setting.Set
			row.LimitSet = &last
		}
		if holds := newest[series.Name].Holds; holds != nil && holdsWritten(newest[series.Name]) == series.Holds {
			since := holds.At
			row.Since = &since
		}
		state.Series = append(state.Series, row)
	}
	current := at.Truncate(time.Minute)
	state.Provider.Minutes = make([]store.ProviderMinuteReading, 0, int(signals.RefusalSpan/time.Minute))
	for start := current.Add(-signals.RefusalSpan); start.Before(current); start = start.Add(time.Minute) {
		minute, held := minutes.Read(start)
		state.Provider.Minutes = append(state.Provider.Minutes, store.ProviderMinuteReading{At: start.UTC(), Held: held,
			UncachedInputTokens: minute.Uncached, TokenLimit: minute.Limit, OwnRefusals: minute.Own, ProviderRefusals: minute.Provider})
	}
	return state, nil
}
