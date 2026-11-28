package signals

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// clock is the moment hour:minute of 2001-03-05 UTC, the day of the tests below.
func clock(hour, minute int) time.Time {
	return time.Date(2001, time.March, 5, hour, minute, 0, 0, time.UTC)
}

// A signal binds where it is recorded and its value is at or above its limit, and nowhere else: an
// unrecorded signal binds nothing whatever its number, and a limit no number reaches is no bind.
func TestAValueBindsOnlyWhereRecordedAndAtOrAboveItsLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value Value
		limit float64
		want  bool
	}{
		{"at its limit", Value{Recorded: true, Value: 2}, 2, true},
		{"above its limit", Value{Recorded: true, Value: 3}, 2, true},
		{"just under its limit", Value{Recorded: true, Value: 2}, math.Nextafter(2, 3), false},
		{"unrecorded, whatever the number", Value{Value: 9}, 0, false},
		{"a limit of zero, which every recorded value reaches", Value{Recorded: true}, 0, true},
		{"a limit that is no number", Value{Recorded: true, Value: 9}, math.NaN(), false},
	} {
		if got := tc.value.Binds(tc.limit); got != tc.want {
			t.Errorf("%s: %+v binds a limit of %v: %t, want %t", tc.name, tc.value, tc.limit, got, tc.want)
		}
	}
}

// A signal that is not recorded and a signal with no limit hold no start, and the ones that do are
// named in R11's order.
func TestASignalWithNoRecordOrNoLimitHoldsNoStart(t *testing.T) {
	t.Parallel()
	s := Snapshot{Readings: map[string]Reading{
		Approvals: {Value: Value{Recorded: true, Value: 3}, Limit: new(1.0)},
		Kills:     {Value: Value{Recorded: true, Value: 2}, Limit: new(2.0)},
		Idle:      {Value: Value{}, Limit: new(0.0)},
		Queue:     {Value: Value{Recorded: true, Value: 99}},
	}}
	if got, want := s.Holding(), []string{Kills, Approvals}; !slices.Equal(got, want) {
		t.Errorf("the signals that hold a start = %v, want %v: the unrecorded idle and the queue with no limit hold none", got, want)
	}
	if got, set := s.Limit(Queue); set || got != 0 {
		t.Errorf("the queue's limit = %v, %t, want none", got, set)
	}
	if got, set := s.Limit(Kills); !set || got != 2 {
		t.Errorf("the kills' limit = %v, %t, want 2", got, set)
	}
	if held := (Snapshot{}).Holding(); len(held) != 0 {
		t.Errorf("a snapshot with no reading holds %v, want nothing", held)
	}
}

// Names is R11's five signals in its order, a fresh list each time: the provider's signal is its
// headroom alone, and its refusals are none.
func TestNamesAreR11sSignalsInItsOrder(t *testing.T) {
	t.Parallel()
	want := []string{"idle", "queue", "provider", "kills", "approvals"}
	got := Names()
	if !slices.Equal(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	got[0] = "changed"
	if again := Names(); !slices.Equal(again, want) {
		t.Errorf("a change to a list Names answered reached the next answer: %v", again)
	}
}

// The provider's refusals hold no start (spec R11, v52): a reading a caller names "refusals", far
// above any limit the refusals ever had, holds nothing at any kind of pass that admits by signals,
// under the cap as a ceiling or under the back-off, beside kills under their limit; the kills at
// their limit still hold the start.
func TestRecordedRefusalsHoldNoStart(t *testing.T) {
	t.Parallel()
	snapshot := func(kills float64) Snapshot {
		return Snapshot{At: clock(10, 30), Readings: map[string]Reading{
			"refusals": {Value: Value{Recorded: true, Value: 1_000_000}, Limit: new(1.0)},
			Kills:      {Value: Value{Recorded: true, Value: kills}, Limit: new(2.0)},
		}}
	}
	if held := snapshot(1).Holding(); len(held) != 0 {
		t.Errorf("the signals that hold a start = %v, want none: the refusals are no signal, and the kills are under their limit", held)
	}
	for _, mode := range []Mode{Ceiling, Backoff} {
		for _, kind := range []PassKind{PassEvent, PassDeadline} {
			if got := Decide(mode, kind, snapshot(1), 1, 3); got != Starts {
				t.Errorf("under %q at a pass of %q, a million refusals and kills under their limit decide %q, want %q", mode, kind, got, Starts)
			}
			if got := Decide(mode, kind, snapshot(2), 1, 3); got != HeldBySignal {
				t.Errorf("under %q at a pass of %q, the kills at their limit decide %q, want %q", mode, kind, got, HeldBySignal)
			}
		}
	}
	if err := CheckLimit("refusals", 4); err == nil || !strings.Contains(err.Error(), "no signal a limit can name") {
		t.Errorf("a limit of 4 on the refusals answered %v, want the refusal of a name that is no signal", err)
	}
}

// An idle holder counts from its idle time plus d: a nanosecond before, it does not; at that
// moment, it does. A moment with no holder reads 0.
func TestAnIdleHolderCountsAtItsIdleTimePlusD(t *testing.T) {
	t.Parallel()
	since := clock(9, 30)
	for _, tc := range []struct {
		name    string
		at      time.Time
		holders int
		want    float64
	}{
		{"a nanosecond before d has passed", since.Add(time.Hour - time.Nanosecond), 2, 0},
		{"exactly d after", since.Add(time.Hour), 2, 0.5},
		{"long after d", since.Add(3 * time.Hour), 2, 0.5},
		{"no holder", since.Add(time.Hour), 0, 0},
	} {
		if got := IdleShare(tc.holders, []time.Time{since}, tc.at, time.Hour); got != tc.want {
			t.Errorf("%s: the idle share is %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The idle signal's next crossing is the moment a holder reaches d, and an edge of the record
// where the record ends while the signal binds: a holder leaves idle only at an event.
func TestTheIdleCrossingIsTheMomentAHolderReachesDOrTheEndOfTheRecord(t *testing.T) {
	t.Parallel()
	idle := []time.Time{clock(9, 30)}
	whole := Coverage{}
	ending := Coverage{Held: func(at time.Time) bool { return at.Before(clock(14, 0)) }, Edges: []time.Time{clock(14, 0)}}
	for _, tc := range []struct {
		name  string
		c     Coverage
		at    time.Time
		limit float64
		want  time.Time
	}{
		{"up: the holder reaches an hour at 10:30", whole, clock(10, 0), 0.5, clock(10, 30)},
		{"none: it binds already and nothing lowers it with no event", whole, clock(11, 0), 0.5, time.Time{}},
		{"down: only the record's end at 14:00 ends the binding", ending, clock(11, 0), 0.5, clock(14, 0)},
		{"none: a limit no share reaches", whole, clock(10, 0), 2, time.Time{}},
	} {
		got, due := IdleNext(tc.c, 2, idle, tc.at, time.Hour, tc.limit)
		if due != !tc.want.IsZero() || !got.Equal(tc.want) {
			t.Errorf("%s: the next crossing is %v, %t, want %v", tc.name, got, due, tc.want)
		}
	}
	if got := IdleAt(ending, 2, idle, clock(14, 0), time.Hour); got.Recorded {
		t.Errorf("the idle share at the end of the record is %+v, want it unrecorded", got)
	}
}

// A kill counts from its own moment and leaves the hour exactly an hour later, and the count a
// crossing reads is the count of the record as it stands at the moment the crossing is asked from.
func TestAKillCountsFromItsMomentAndLeavesTheHourAnHourLater(t *testing.T) {
	t.Parallel()
	kills := []time.Time{clock(9, 40), clock(9, 50), clock(10, 35)}
	for _, tc := range []struct {
		at   time.Time
		want int
	}{
		{clock(9, 39), 0},
		{clock(9, 40), 1},
		{clock(10, 39), 3},
		{clock(10, 40), 2},
		{clock(10, 50), 1},
		{clock(11, 35), 0},
	} {
		if got := KillCount(kills, tc.at, tc.at); got != tc.want {
			t.Errorf("the kills of the hour before %s = %d, want %d", tc.at.Format("15:04"), got, tc.want)
		}
	}
	if got := KillCount(kills, clock(10, 40), clock(9, 50)); got != 1 {
		t.Errorf("the kills at 10:40 as the record stands at 09:50 = %d, want 1: the kill of 10:35 is not yet known", got)
	}
	for _, tc := range []struct {
		at    time.Time
		limit float64
		want  time.Time
	}{
		{clock(9, 50), 2, clock(10, 40)},
		{clock(10, 35), 2, clock(10, 50)},
		{clock(9, 45), 2, time.Time{}},
	} {
		got, due := KillsNext(Coverage{}, kills, tc.at, tc.limit)
		if due != !tc.want.IsZero() || !got.Equal(tc.want) {
			t.Errorf("the next crossing of %v kills from %s is %v, %t, want %v", tc.limit, tc.at.Format("15:04"), got, due, tc.want)
		}
	}
	if got := KillsAt(Coverage{Held: func(time.Time) bool { return false }}, kills, clock(10, 0)); got.Recorded {
		t.Errorf("the kills where the record holds none = %+v, want unrecorded", got)
	}
}

// The queue's crossing is the first moment at which the value a reading gives, in hours, reaches
// the limit: the moment before it does not, whatever floating-point error a product of the limit
// and an hour carries. A limit that no wait reaches sets no crossing.
func TestTheQueueCrossingIsTheFirstMomentAReadingBinds(t *testing.T) {
	t.Parallel()
	entered := clock(6, 0)
	waits := []Wait{{Added: entered}}
	at := entered.Add(time.Minute)
	for _, limitHours := range []float64{0.1, 0.3, 1.1, 2.7, 4, 7.0 / 3} {
		got, due := QueueNext(Coverage{}, waits, at, limitHours)
		if !due {
			t.Errorf("a limit of %v hours: no crossing is due for a pull request that waits on", limitHours)
			continue
		}
		if !QueueAt(Coverage{}, waits, got).Binds(limitHours) {
			t.Errorf("a limit of %v hours: the reading at the crossing %s does not bind", limitHours, got.Format(time.RFC3339Nano))
		}
		if QueueAt(Coverage{}, waits, got.Add(-time.Nanosecond)).Binds(limitHours) {
			t.Errorf("a limit of %v hours: the reading a nanosecond before the crossing %s binds already", limitHours, got.Format(time.RFC3339Nano))
		}
	}
	if got, due := QueueNext(Coverage{}, waits, at, 1e300); due {
		t.Errorf("a limit no wait reaches has a crossing at %v, want none", got)
	}
	if got, due := QueueNext(Coverage{}, waits, at, math.NaN()); due {
		t.Errorf("a limit that is no number has a crossing at %v, want none", got)
	}
}

// The queue's value is the larger of the median wait of the pull requests that left in the 6 hours
// before the moment and the wait so far of the one longest in the queue, and an exit leaves the 6
// hours exactly 6 hours later.
func TestTheQueueValueIsTheLargerOfTheMedianExitAndTheLongestWait(t *testing.T) {
	t.Parallel()
	waits := []Wait{
		{Added: clock(2, 0), Left: clock(3, 0)},
		{Added: clock(4, 0), Left: clock(6, 0)},
		{Added: clock(7, 0)},
	}
	for _, tc := range []struct {
		at   time.Time
		want float64
		why  string
	}{
		{clock(2, 30), 0.5, "the first pull request has waited half an hour"},
		{clock(3, 30), 1, "it left after an hour; nothing else has entered"},
		{clock(6, 30), 1.5, "the median of the exits of 1 hour and 2 hours"},
		{clock(7, 30), 1.5, "the one that entered at 07:00 has waited half an hour, and the median stands"},
		{clock(8, 0), 1.5, "the first exit, at 03:00, is still in the 6 hours before 08:00"},
		{clock(9, 0), 2, "the first exit left the 6 hours at 09:00, the second's 2 hours is the median, and the wait so far of the third is 2 hours"},
	} {
		if got := QueueAt(Coverage{}, waits, tc.at); !got.Recorded || got.Value != tc.want {
			t.Errorf("the queue at %s = %+v, want %v: %s", tc.at.Format("15:04"), got, tc.want, tc.why)
		}
	}
}

// The provider's headroom reads the minute before the moment over the limit in force in it, and
// reads nothing where the record holds no such minute or no limit binds in it.
func TestTheProviderHeadroomReadsTheMinuteBeforeOverItsLimit(t *testing.T) {
	t.Parallel()
	minutes := map[int64]Minute{
		clock(9, 0).Unix(): {Uncached: 3000, Limit: 6000},
		clock(9, 1).Unix(): {Uncached: 4800, Limit: 6000},
		clock(9, 2).Unix(): {Uncached: 100},
		clock(9, 4).Unix(): {Uncached: 6000, Limit: 6000},
	}
	read := Series(func(start time.Time) (Minute, bool) {
		minute, held := minutes[start.Unix()]
		return minute, held
	})
	for _, tc := range []struct {
		at   time.Time
		want Value
		why  string
	}{
		{clock(9, 0).Add(30 * time.Second), Value{}, "the minute before, 08:59, is not held"},
		{clock(9, 1).Add(30 * time.Second), Value{Recorded: true, Value: 0.5}, "09:00: 3,000 of 6,000"},
		{clock(9, 2), Value{Recorded: true, Value: 0.8}, "09:01: 4,800 of 6,000"},
		{clock(9, 3), Value{}, "09:02 is held, with no limit that binds"},
		{clock(9, 4), Value{}, "09:03 is not held"},
		{clock(9, 5), Value{Recorded: true, Value: 1}, "09:04: the whole limit"},
	} {
		if got := ProviderAt(read, tc.at); got != tc.want {
			t.Errorf("the headroom at %s = %+v, want %+v: %s", tc.at.Format("15:04:05"), got, tc.want, tc.why)
		}
	}
	for _, tc := range []struct {
		at    time.Time
		limit float64
		want  time.Time
		why   string
	}{
		{clock(9, 1), 0.75, clock(9, 2), "up: 09:01 uses 0.8"},
		{clock(9, 2), 0.75, clock(9, 3), "down: the minute 09:02 has no limit that binds"},
		{clock(9, 2), 2, time.Time{}, "none: no minute reaches 2"},
	} {
		got, due := ProviderNext(read, tc.at, clock(9, 6), tc.limit)
		if due != !tc.want.IsZero() || !got.Equal(tc.want) {
			t.Errorf("the next crossing of %v from %s = %v, %t, want %v: %s", tc.limit, tc.at.Format("15:04"), got, due, tc.want, tc.why)
		}
	}
}

// An ask waits on its asked_of person, else its issue's assignee, else the project's default
// assignee, and counts for the approvals where that person is the default assignee.
func TestAnAskCountsWhereItWaitsOnTheDefaultAssignee(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                       string
		askedOf, assignee, project string
		want                       bool
	}{
		{"asked of the default assignee", "owner", "other", "owner", true},
		{"asked of someone else, whoever the issue is assigned to", "other", "owner", "owner", false},
		{"no asked_of, the issue assigned to someone else", "", "other", "owner", false},
		{"no asked_of, the issue assigned to the default assignee", "", "owner", "owner", true},
		{"no asked_of and no assignee: the default assignee", "", "", "owner", true},
		{"no default assignee: nobody", "", "", "", false},
	} {
		if got := WaitsOnDefault(tc.askedOf, tc.assignee, tc.project); got != tc.want {
			t.Errorf("%s: waits on the default assignee: %t, want %t", tc.name, got, tc.want)
		}
	}
}

// The back-off folds each setback, a kill, at 90% in integers, rounded down and at least 1, and the
// allowance rises by 1 for each whole quiet hour since the last, up to the ceiling.
func TestTheBackoffAllowanceFoldsEachSetbackAndRisesOnQuietHours(t *testing.T) {
	t.Parallel()
	setback := func(hour, minute int, count int64) Setback { return Setback{At: clock(hour, minute), Count: count} }
	for _, tc := range []struct {
		name     string
		ceiling  int
		setbacks []Setback
		now      time.Time
		want     int
	}{
		{"no setback: the ceiling", 3, nil, clock(10, 0), 3},
		{"allowance 3 and a kill gives 2", 3, []Setback{setback(10, 0, 1)}, clock(10, 0), 2},
		{"allowance 2 and another gives 1", 3, []Setback{setback(10, 0, 1), setback(10, 5, 1)}, clock(10, 5), 1},
		{"allowance 1 and a third kill stays 1", 3, []Setback{setback(10, 0, 2), setback(10, 1, 1)}, clock(10, 1), 1},
		{"a count is that many setbacks at one moment", 3, []Setback{setback(10, 0, 2)}, clock(10, 0), 1},
		{"90% rounded down: 10 gives 9", 10, []Setback{setback(10, 0, 1)}, clock(10, 0), 9},
		{"90% rounded down: 9 gives 8", 10, []Setback{setback(10, 0, 2)}, clock(10, 0), 8},
		{"90% rounded down: 8 gives 7", 10, []Setback{setback(10, 0, 3)}, clock(10, 0), 7},
		{"allowance 2 below a ceiling of 3, an hour of quiet gives 3", 3, []Setback{setback(10, 0, 1)}, clock(11, 0), 3},
		{"a second short of the hour", 3, []Setback{setback(10, 0, 1)}, clock(11, 0).Add(-time.Second), 2},
		{"a rise stops at the ceiling", 3, []Setback{setback(10, 0, 1)}, clock(20, 0), 3},
		{"the rises between two setbacks count", 3, []Setback{setback(10, 0, 1), setback(11, 30, 1)}, clock(11, 30), 2},
		{"each quiet hour after the last raises it by one", 5, []Setback{setback(10, 0, 2)}, clock(12, 0), 5},
		{"a setback after the pass is not yet known", 3, []Setback{setback(10, 0, 1), setback(12, 0, 1)}, clock(10, 30), 2},
	} {
		if got := Allowance(tc.ceiling, tc.setbacks, tc.now); got != tc.want {
			t.Errorf("%s: the allowance is %d, want %d", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		name     string
		ceiling  int
		setbacks []Setback
		now      time.Time
		want     time.Time
	}{
		{"the next hour after the last setback", 3, []Setback{setback(10, 0, 1)}, clock(10, 30), clock(11, 0)},
		{"the moment of the setback itself", 3, []Setback{setback(10, 0, 1)}, clock(10, 0), clock(11, 0)},
		{"on the hour, the hour after", 5, []Setback{setback(10, 0, 2)}, clock(11, 0), clock(12, 0)},
		{"none at the ceiling", 3, []Setback{setback(10, 0, 1)}, clock(11, 0), time.Time{}},
		{"none with no setback", 3, nil, clock(11, 0), time.Time{}},
	} {
		got, due := NextRise(tc.ceiling, tc.setbacks, tc.now)
		if due != !tc.want.IsZero() || !got.Equal(tc.want) {
			t.Errorf("%s: the next rise is %v, %t, want %v", tc.name, got, due, tc.want)
		}
	}
}

// The decision for one entry below P0: a tick lets none start under a mode, an event and a deadline
// do while no signal that has a limit binds and the holders fill neither the ceiling nor the
// allowance, a signal binds at its limit and not below it, and under Off every pass is the cap.
func TestTheDecisionForAnEntryBelowP0ByModeAndPassKind(t *testing.T) {
	t.Parallel()
	killsRead := func(value Value, limit *float64) Snapshot {
		return Snapshot{At: clock(10, 30), Readings: map[string]Reading{Kills: {Value: value, Limit: limit}}}
	}
	binding := killsRead(Value{Recorded: true, Value: 2}, new(2.0))
	under := killsRead(Value{Recorded: true, Value: 1}, new(2.0))
	unrecorded := killsRead(Value{Value: 9}, new(2.0))
	unlimited := killsRead(Value{Recorded: true, Value: 9}, nil)
	zero := killsRead(Value{Recorded: true}, new(0.0))
	backed := func(s Snapshot, setbacks ...Setback) Snapshot {
		s.Setbacks = setbacks
		return s
	}
	kill := Setback{At: clock(10, 0), Count: 1}
	for _, tc := range []struct {
		name   string
		mode   Mode
		kind   PassKind
		s      Snapshot
		capped int
		want   Verdict
	}{
		{"off: a tick starts what the cap lets, a signal over its limit or not", Off, PassTick, binding, 2, Starts},
		{"off: the cap full", Off, PassEvent, under, 3, HeldByCap},
		{"every signal under its limit, an event", Ceiling, PassEvent, under, 2, Starts},
		{"every signal under its limit, a deadline", Ceiling, PassDeadline, under, 2, Starts},
		{"every signal under its limit, a tick", Ceiling, PassTick, under, 2, HeldByTick},
		{"a signal at its limit, an event", Ceiling, PassEvent, binding, 2, HeldBySignal},
		{"a signal at its limit, a deadline", Ceiling, PassDeadline, binding, 2, HeldBySignal},
		{"a signal at its limit, a tick", Ceiling, PassTick, binding, 2, HeldByTick},
		{"the holders at the ceiling, an event", Ceiling, PassEvent, under, 3, HeldByCap},
		{"the holders at the ceiling, a deadline", Ceiling, PassDeadline, under, 3, HeldByCap},
		{"a signal with no record in the span", Ceiling, PassEvent, unrecorded, 2, Starts},
		{"a signal with no limit", Ceiling, PassEvent, unlimited, 2, Starts},
		{"a limit of zero on a recorded signal", Ceiling, PassEvent, zero, 0, HeldBySignal},
		{"a snapshot with no reading", Ceiling, PassEvent, Snapshot{}, 2, Starts},
		{"back-off after a kill, the holders at the allowance of 2", Backoff, PassEvent, backed(under, kill), 2, HeldByCap},
		{"back-off after a kill, one holder below the allowance", Backoff, PassEvent, backed(under, kill), 1, Starts},
		{"back-off a quiet hour later, the allowance back at 3", Backoff, PassDeadline, backed(under, Setback{At: clock(9, 30), Count: 1}), 2, Starts},
		{"back-off with a signal over its limit", Backoff, PassEvent, binding, 0, HeldBySignal},
		{"back-off at a tick", Backoff, PassTick, under, 0, HeldByTick},
	} {
		if got := Decide(tc.mode, tc.kind, tc.s, tc.capped, 3); got != tc.want {
			t.Errorf("%s: the decision is %q, want %q", tc.name, got, tc.want)
		}
	}
	if !Starts.Lets() || HeldBySignal.Lets() || HeldByTick.Lets() || HeldByCap.Lets() {
		t.Error("only Starts lets an entry take a slot")
	}
	if Off.Or(Ceiling) != Ceiling || Backoff.Or(Ceiling) != Backoff || Off.Or(Off) != Off {
		t.Error("a mode a rule names wins over the snapshot's, and Off names none")
	}
}

// The project's deadline is the earliest crossing, the first of two at one moment.
func TestTheDeadlineIsTheEarliestCrossing(t *testing.T) {
	t.Parallel()
	got, due := Earliest([]Crossing{{Signal: Queue, At: clock(11, 0)}, {Signal: Kills, At: clock(10, 40)}, {Signal: Idle, At: clock(10, 40)}})
	if !due || got.Signal != Kills || !got.At.Equal(clock(10, 40)) {
		t.Errorf("the earliest crossing = %+v, %t, want the kills at 10:40, the first of the two at that moment", got, due)
	}
	if got, due := Earliest(nil); due {
		t.Errorf("the earliest of no crossing = %+v, want none", got)
	}
}

// FirstChange answers the first moment after at at which the answer differs from the answer at at,
// each moment once, in time order whatever the order given.
func TestFirstChangeIsTheFirstMomentAfterAtWhereTheAnswerChanges(t *testing.T) {
	t.Parallel()
	binds := func(at time.Time) bool { return !at.Before(clock(10, 0)) && at.Before(clock(11, 0)) }
	moments := []time.Time{clock(11, 0), clock(10, 0), clock(10, 0), clock(9, 0)}
	if got, due := FirstChange(clock(9, 30), slices.Clone(moments), binds); !due || !got.Equal(clock(10, 0)) {
		t.Errorf("from 09:30 the first change is %v, %t, want 10:00", got, due)
	}
	if got, due := FirstChange(clock(10, 30), slices.Clone(moments), binds); !due || !got.Equal(clock(11, 0)) {
		t.Errorf("from 10:30 the first change is %v, %t, want 11:00", got, due)
	}
	if got, due := FirstChange(clock(11, 30), slices.Clone(moments), binds); due {
		t.Errorf("from 11:30 the first change is %v, want none", got)
	}
}
