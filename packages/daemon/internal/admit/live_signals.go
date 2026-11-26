package admit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// SignalSettings is where the live signals read a project's switch of admission by signals and the
// limit of each series (S3), in the pass's own transaction, so a change a person made is in force
// from the next pass on.
type SignalSettings interface {
	SignalSwitch(ctx context.Context, tx pgx.Tx, project string) (SignalSwitch, error)
}

// SignalSwitch is a project's settings of admission by signals: whether it is on, and the limit of
// each series that has one, by its name of signals.Names(). A series with no limit holds nothing.
// stored is the store's settings it was read from (StoreSignalSettings), with who set each and
// why, which the state shows beside the reading without reading them again; zero for settings of
// another source.
type SignalSwitch struct {
	On     bool
	Limits map[string]float64
	stored store.SignalSettings
}

// StoreSignalSettings is the settings a person set (store.SignalSettingsTx), the daemon's own: a
// fresh project is off with no limit, and each change a person makes is in force from the pass of
// the fact it is applied as (intake.SignalSettingChanged).
type StoreSignalSettings struct{}

func (StoreSignalSettings) SignalSwitch(ctx context.Context, tx pgx.Tx, project string) (SignalSwitch, error) {
	settings, err := store.SignalSettingsTx(ctx, tx, project)
	if err != nil {
		return SignalSwitch{}, err
	}
	sw := SignalSwitch{On: settings.On(), Limits: map[string]float64{}, stored: settings}
	for _, series := range signals.Names() {
		if limit, set := settings.Limit(series); set {
			sw.Limits[series] = limit
		}
	}
	return sw, nil
}

// signalSteps is the step of each series' value entries (S6): a pass records a value of a series
// where it differs from the last one recorded by at least the step.
var signalSteps = map[string]float64{
	signals.Idle:      0.05,
	signals.Queue:     0.25,
	signals.Provider:  0.10,
	signals.Refusals:  5,
	signals.Kills:     5,
	signals.Approvals: 5,
}

// SeriesReading is one series as the live signals read it at a moment: its value, or why the daemon
// cannot compute it (Absent, with Value unrecorded), its limit (nil for none), whether it holds
// admission (only while admission by signals is on), and the next moment after it at which it
// would cross its limit with no new event, where Due says one is (only for a series with a limit,
// and never for the approvals, which move only at events).
type SeriesReading struct {
	Name   string
	Value  signals.Value
	Absent string
	Limit  *float64
	Holds  bool
	Next   time.Time
	Due    bool
}

// LiveReading is the six series at At, in the order of signals.Names(), and whether admission by
// signals is on.
type LiveReading struct {
	At     time.Time
	On     bool
	Series []SeriesReading
}

// Snapshot is the reading as a pass admits by it: the mode signals.Ceiling while admission by
// signals is on and signals.Off while it is off, never signals.Backoff, and each series' value and
// limit.
func (r LiveReading) Snapshot() signals.Snapshot {
	snapshot := signals.Snapshot{At: r.At, Readings: make(map[string]signals.Reading, len(r.Series))}
	if r.On {
		snapshot.Mode = signals.Ceiling
	}
	for _, series := range r.Series {
		snapshot.Readings[series.Name] = signals.Reading{Value: series.Value, Limit: series.Limit}
	}
	return snapshot
}

// Deadline is the project's deadline at the reading: the earliest next crossing of the series with
// a limit (signals.Earliest), and whether one is due. None is due while admission by signals is
// off, since no crossing then moves a start.
func (r LiveReading) Deadline() (signals.Crossing, bool) {
	if !r.On {
		return signals.Crossing{}, false
	}
	var crossings []signals.Crossing
	for _, series := range r.Series {
		if series.Due {
			crossings = append(crossings, signals.Crossing{Signal: series.Name, At: series.Next})
		}
	}
	return signals.Earliest(crossings)
}

// LiveSignals reads the six series of a project from what the daemon keeps of their inputs, with
// the one definition of each (package signals) that the replay of a recorded window reads its own
// record with: the slot holders and their states (signals.IdleAt), the merge queue's events
// (signals.QueueAt), the provider's minutes of the model requests the hosts report
// (signals.ProviderAt, signals.RefusalsAt), the kills of the project's issue pods
// (signals.KillsAt) and the asks that wait on the project's default assignee (record.Approvals,
// by signals.ApprovalState's rule). Each read runs in the transaction given, so a pass reads what
// its own fact kept.
type LiveSignals struct {
	admission *Admission
	settings  SignalSettings
	// reporting is the first plugin contract whose host reports its model requests: a turn of a
	// host below it leaves its minutes absent (record.Store.ProviderMinutes).
	reporting int
}

// NewLiveSignals is the live signals of admission's project, its switch and limits read from
// settings, and reporting the first plugin contract whose host reports its model requests.
func NewLiveSignals(admission *Admission, settings SignalSettings, reporting int) *LiveSignals {
	return &LiveSignals{admission: admission, settings: settings, reporting: reporting}
}

// Read is the reader the daemon registers as the pass's gate (Admission.UseSignals): the snapshot
// of the six series at at, and whether admission by signals is on. The pass keeps the reading it
// answered (keepPassReading), each series with the reason it cannot be computed, which the
// snapshot does not carry, so the record of changes the pass writes is of its own reading.
func (l *LiveSignals) Read(ctx context.Context, tx pgx.Tx, at time.Time) (signals.Snapshot, bool, error) {
	reading, err := l.Reading(ctx, tx, at)
	if err != nil {
		return signals.Snapshot{}, false, err
	}
	keepPassReading(ctx, reading)
	return reading.Snapshot(), reading.On, nil
}

// Reading is each of the six series at at, through tx: its value or why it cannot be computed, its
// limit, whether it holds admission and its next crossing of that limit.
//
// A series that counts over a span is recorded only from the moment the daemon began keeping its
// input (record.Store.SignalInputStatus) plus that span, and the end of that gap is an edge of its
// coverage, which its next crossing names: the kills from an hour after, and the merge queue from 6
// hours after, where no boot's read of GitHub's merge queue failed since. The provider's minutes
// are whole minutes, held only where every request of them was reported
// (record.Store.ProviderMinutes): the headroom reads the minute before at, and the refusals are
// recorded only where every minute of the hour before at is held, so an hour with a minute no host
// reported is absent rather than counted short. So is an hour, or the minute under way, in which a
// session of a host that cannot report the 429s its transport retries had a request: the refusals
// are recorded again an hour after that minute ends, an edge of their coverage, and the headroom
// reads that host's tokens as any other's. The minute under way at at counts its tokens so far for
// the headroom's next crossing alone, at the boundary where it becomes the minute before, and
// never as the minute before; its refusals count from its end, as signals.RefusalsNext counts a
// minute's.
func (l *LiveSignals) Reading(ctx context.Context, tx pgx.Tx, at time.Time) (LiveReading, error) {
	reading, _, _, err := l.read(ctx, tx, at)
	return reading, err
}

// read is Reading, the provider's minutes its headroom and refusals read, from the start of the
// hour before at to the end of the minute under way, and the store's settings its switch and
// limits were read from (SignalSwitch), which the state shows beside them.
func (l *LiveSignals) read(ctx context.Context, tx pgx.Tx, at time.Time) (LiveReading, record.ProviderMinutes, store.SignalSettings, error) {
	a := l.admission
	var minutes record.ProviderMinutes
	settings, err := l.settings.SignalSwitch(ctx, tx, a.project)
	if err != nil {
		return LiveReading{}, minutes, store.SignalSettings{}, fmt.Errorf("read the settings of admission by signals: %w", err)
	}
	fail := func(err error) (LiveReading, record.ProviderMinutes, store.SignalSettings, error) {
		return LiveReading{}, minutes, store.SignalSettings{}, err
	}
	reading := LiveReading{At: at, On: settings.On}
	limit := func(name string) *float64 {
		value, set := settings.Limits[name]
		if !set {
			return nil
		}
		return &value
	}
	add := func(series SeriesReading) {
		series.Limit = limit(series.Name)
		series.Holds = settings.On && signals.Reading{Value: series.Value, Limit: series.Limit}.Holds()
		reading.Series = append(reading.Series, series)
	}

	holders, idleSince, err := a.idleHolders(ctx, tx, at)
	if err != nil {
		return fail(err)
	}
	idle := SeriesReading{Name: signals.Idle, Value: signals.IdleAt(signals.Coverage{}, holders, idleSince, at, signals.DefaultIdleAfter)}
	if bound := limit(signals.Idle); bound != nil {
		idle.Next, idle.Due = signals.IdleNext(signals.Coverage{}, holders, idleSince, at, signals.DefaultIdleAfter, *bound)
	}
	add(idle)

	queue, err := l.queue(ctx, tx, at, limit(signals.Queue))
	if err != nil {
		return fail(err)
	}
	add(queue)

	headroom, refusals, minutes, err := l.provider(ctx, tx, at, limit(signals.Provider), limit(signals.Refusals))
	if err != nil {
		return fail(err)
	}
	add(headroom)
	add(refusals)

	kills, err := l.kills(ctx, tx, at, limit(signals.Kills))
	if err != nil {
		return fail(err)
	}
	add(kills)

	approvals, err := a.store.ApprovalsOf(ctx, tx, a.project)
	if err != nil {
		return fail(err)
	}
	add(SeriesReading{Name: signals.Approvals, Value: approvals.Value(), Absent: approvals.Unrecorded})
	return reading, minutes, settings.stored, nil
}

// spanCoverage is the coverage of a series counted over span whose input the daemon began keeping
// at since (kept), and the reason it is not recorded at at, "" where it is: recorded from since
// plus span, which is an edge after at until then.
func spanCoverage(what string, since time.Time, kept bool, span time.Duration, at time.Time) (signals.Coverage, string) {
	if !kept {
		return signals.Coverage{Held: func(time.Time) bool { return false }}, fmt.Sprintf("the daemon has not begun keeping %s", what)
	}
	whole := since.Add(span)
	coverage := signals.Coverage{Held: func(t time.Time) bool { return !t.Before(whole) }}
	if whole.After(at) {
		coverage.Edges = []time.Time{whole}
		return coverage, fmt.Sprintf("the daemon keeps %s from %s, so the span this signal counts is whole from %s",
			what, since.Format(time.RFC3339), whole.Format(time.RFC3339))
	}
	return coverage, ""
}

// queue is the merge queue's wait at at, in hours (signals.QueueAt), over every kept event of each
// pull request that stands in the queue or left it in the 6 hours before at.
func (l *LiveSignals) queue(ctx context.Context, tx pgx.Tx, at time.Time, limit *float64) (SeriesReading, error) {
	a := l.admission
	since, kept, readError, err := a.store.SignalInputStatus(ctx, tx, a.project, signals.Queue)
	if err != nil {
		return SeriesReading{}, err
	}
	coverage, absent := spanCoverage("the merge queue's events", since, kept, signals.QueueExitSpan, at)
	if readError != "" {
		coverage, absent = signals.Coverage{Held: func(time.Time) bool { return false }}, "the boot's read of GitHub's merge queue failed: "+readError
	}
	events, err := a.store.QueueEventsAt(ctx, tx, a.project, at)
	if err != nil {
		return SeriesReading{}, err
	}
	waits := signals.WaitsOf(events)
	series := SeriesReading{Name: signals.Queue, Value: signals.QueueAt(coverage, waits, at)}
	if !series.Value.Recorded {
		series.Absent = absent
	}
	if limit != nil {
		series.Next, series.Due = signals.QueueNext(coverage, waits, at, *limit)
	}
	return series, nil
}

// kills is the kills of the project's issue pods in the hour before at (signals.KillsAt).
func (l *LiveSignals) kills(ctx context.Context, tx pgx.Tx, at time.Time, limit *float64) (SeriesReading, error) {
	a := l.admission
	since, kept, _, err := a.store.SignalInputStatus(ctx, tx, a.project, signals.Kills)
	if err != nil {
		return SeriesReading{}, err
	}
	coverage, absent := spanCoverage("the memory kills of the project's issue pods", since, kept, signals.KillSpan, at)
	kills, err := a.store.MemoryKillsAfter(ctx, tx, a.project, at.Add(-signals.KillSpan))
	if err != nil {
		return SeriesReading{}, err
	}
	series := SeriesReading{Name: signals.Kills, Value: signals.KillsAt(coverage, kills, at)}
	if !series.Value.Recorded {
		series.Absent = absent
	}
	if limit != nil {
		series.Next, series.Due = signals.KillsNext(coverage, kills, at, *limit)
	}
	return series, nil
}

// provider is the provider's headroom and refusals at at (signals.ProviderAt, signals.RefusalsAt),
// over the project's minutes from the start of the hour before at to the end of the minute under
// way (record.Store.ProviderMinutes), as Reading says, and those minutes.
func (l *LiveSignals) provider(ctx context.Context, tx pgx.Tx, at time.Time, headroomLimit, refusalsLimit *float64) (SeriesReading, SeriesReading, record.ProviderMinutes, error) {
	a := l.admission
	current := at.Truncate(time.Minute)
	first := current.Add(-signals.RefusalSpan)
	minutes, err := a.store.ProviderMinutes(ctx, tx, a.project, first, current.Add(time.Minute), l.reporting)
	if err != nil {
		return SeriesReading{}, SeriesReading{}, record.ProviderMinutes{}, err
	}
	kept := minutes.Kept()
	// The record as a prediction reads it, the minutes standing as they do at at: the minute under
	// way with its tokens so far and none of its refusals, and each minute after it as no request
	// came, held with no token and no refusal where the minute under way is held.
	under, underHeld := minutes.Read(current)
	predicted := func(start time.Time) (signals.Minute, bool) {
		switch {
		case start.Before(current):
			return minutes.Read(start)
		case start.Equal(current):
			return signals.Minute{Uncached: under.Uncached, Limit: under.Limit}, underHeld
		}
		return signals.Minute{Limit: under.Limit}, underHeld
	}

	headroom := SeriesReading{Name: signals.Provider, Value: signals.ProviderAt(minutes.Read, at)}
	if !headroom.Value.Recorded {
		minute, held := minutes.Read(signals.MinuteBefore(at))
		switch {
		case !kept:
			headroom.Absent = "the daemon has not begun keeping the project's model requests"
		case !held:
			headroom.Absent = "a request of the minute before was not reported: a host of the project reports none, or the daemon began keeping them after the minute began"
		case minute.Limit <= 0:
			headroom.Absent = "the daemon's configuration gives the project no token limit a minute (signal_token_limit_per_minute)"
		}
	}
	if headroomLimit != nil {
		headroom.Next, headroom.Due = signals.ProviderNext(predicted, at, current.Add(2*time.Minute), *headroomLimit)
	}

	refusals := SeriesReading{Name: signals.Refusals}
	var unreported unreportedRetries
	unreported.minute, unreported.found = minutes.RetriesUnreported()
	covered := coveredRefusals(predicted, unreported, at)
	refusals.Value = signals.RefusalsAt(covered, at).Value()
	if !refusals.Value.Recorded {
		refusals.Absent = refusalsGap(predicted, unreported, at)
	}
	if !kept {
		refusals.Absent = "the daemon has not begun keeping the project's model requests"
	}
	if refusalsLimit != nil {
		refusals.Next, refusals.Due = signals.RefusalsNext(covered, at, *refusalsLimit, refusalsEdges(predicted, unreported, at))
	}
	return headroom, refusals, minutes, nil
}

// unreportedRetries is the latest minute of the provider's record that holds a request of a host
// that does not report the 429s its transport retries (record.ProviderMinutes.RetriesUnreported),
// where found: such a host's session may have been refused by a 429 no report names, so the
// refusals of any moment whose hour reaches that minute are known to count short.
type unreportedRetries struct {
	minute time.Time
	found  bool
}

// shortAt says whether the refusals at at count short for that minute: it is one of the hour of
// minutes they read, or the minute under way, whose refusals they read once it ends. A request
// ends before its report, so the reading at the report's moment is already short, and no later
// moment turns it short without a report of its own.
func (u unreportedRetries) shortAt(at time.Time) bool {
	return u.found && !u.minute.Before(at.Truncate(time.Minute).Add(-signals.RefusalSpan))
}

// whole is the moment the minute leaves the hour the refusals read, an hour after its end, from
// which they count whole again where no later such request came: an edge of their coverage.
func (u unreportedRetries) whole() time.Time {
	return u.minute.Add(time.Minute + signals.RefusalSpan)
}

// refusalsGap is why the daemon's record does not cover the refusals at at, "" where it does: it
// covers them where it holds every minute of the hour before at, read as read reads them, and no
// request of a host that cannot report the 429s its transport retries leaves them short
// (unreportedRetries.shortAt). The replay's record covers its refusals wherever it holds the
// minute before; the daemon's is incomplete in a minute a host reported no request of, and short
// where a host reports no retried 429, so an hour with either reads as not recorded, never as a
// count known to be low.
func refusalsGap(read signals.Series, unreported unreportedRetries, at time.Time) string {
	current := at.Truncate(time.Minute)
	for minute := current.Add(-signals.RefusalSpan); minute.Before(current); minute = minute.Add(time.Minute) {
		if _, held := read(minute); !held {
			return fmt.Sprintf("the minute at %s of the hour before was not wholly reported: a host of the project reports no model requests, "+
				"or the daemon began keeping them after it began", minute.Format(time.RFC3339))
		}
	}
	if unreported.shortAt(at) {
		return fmt.Sprintf("a session of a host that cannot report the 429s its transport retried had a request in the hour, the last in the minute at %s, "+
			"so the refusals count whole from %s", unreported.minute.Format(time.RFC3339), unreported.whole().Format(time.RFC3339))
	}
	return ""
}

// coveredRefusals is predicted, the provider's record as it stands at at, as the one definition
// of the refusals reads it (signals.RefusalsAt, signals.RefusalsNext), with the daemon's coverage
// in it: that definition holds the refusals of a moment where the series holds the minute before
// it, so the minute before at, and each minute after, is held where the record covers the hour
// that minute closes (refusalsGap), and each earlier minute where the record holds it, which is
// where its refusals count. A refusal of the minute before at whose hour the record does not
// cover so counts in no later moment's reading of this prediction; the pass that reads that
// moment counts it.
func coveredRefusals(predicted signals.Series, unreported unreportedRetries, at time.Time) signals.Series {
	before := signals.MinuteBefore(at)
	return func(start time.Time) (signals.Minute, bool) {
		minute, held := predicted(start)
		if start.Before(before) || !held {
			return minute, held
		}
		return minute, refusalsGap(predicted, unreported, start.Add(time.Minute)) == ""
	}
}

// refusalsEdges is each moment after at at which the daemon's coverage of the refusals changes
// with no event (coveredRefusals), the record standing as predicted reads it: the moment a minute
// of the hour before at, or the minute under way, that the record does not hold leaves the hour,
// after which the hour is whole again, and the moment the latest minute of a request of a host
// that cannot report the 429s its transport retries leaves it (unreportedRetries.whole).
func refusalsEdges(predicted signals.Series, unreported unreportedRetries, at time.Time) []time.Time {
	current := at.Truncate(time.Minute)
	var edges []time.Time
	for minute := current.Add(-signals.RefusalSpan); !minute.After(current); minute = minute.Add(time.Minute) {
		if _, held := predicted(minute); !held {
			edges = append(edges, minute.Add(time.Minute+signals.RefusalSpan))
		}
	}
	if unreported.found {
		edges = append(edges, unreported.whole())
	}
	return edges
}

// idleHolders is how many of the project's issues hold a slot, and the moment each holder whose
// state is idle entered it, the holders' states read as the slots reading reads them
// (blockers.SlotsReading, record.HolderStateOf): one judgement of the line over this
// transaction's reads, and the holders' claims.
func (a *Admission) idleHolders(ctx context.Context, tx pgx.Tx, at time.Time) (int, []time.Time, error) {
	issues, slots, facts, err := a.reread(ctx, tx)
	if err != nil {
		return 0, nil, err
	}
	own := ownSlots(issues, slots)
	rows, facts, line, _, err := blockers.JudgeLine(ctx, tx, a.store, a.project, issues, own, facts, nil, at)
	if err != nil {
		return 0, nil, err
	}
	_, holders, err := a.slotHolders(ctx, tx, blockers.SlotsInputs{Cap: a.cap, Issues: issues, Slots: own, Rows: rows, Facts: facts, Line: line})
	if err != nil {
		return 0, nil, err
	}
	var idle []time.Time
	for _, holder := range holders {
		if holder.State == record.HolderIdle && holder.Since != nil {
			idle = append(idle, *holder.Since)
		}
	}
	return len(holders), idle, nil
}
