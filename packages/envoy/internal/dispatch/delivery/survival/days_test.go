package survival_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
)

// statuses is a day status of state for each day from from to to, both included.
func statuses(from, to int, state survival.DayState) []survival.DayStatus {
	var out []survival.DayStatus
	for n := from; n <= to; n++ {
		out = append(out, survival.DayStatus{Day: day(n), State: state})
	}
	return out
}

// with is days, each day of extra in place of the status days held for it.
func with(days []survival.DayStatus, extra ...survival.DayStatus) []survival.DayStatus {
	out := slices.DeleteFunc(slices.Clone(days), func(status survival.DayStatus) bool {
		return slices.ContainsFunc(extra, func(e survival.DayStatus) bool { return e.Day.Equal(status.Day) })
	})
	return append(out, extra...)
}

// selectOn is the days a run selects on now, with start the start day and stored the days with a
// state.
func selectOn(now time.Time, start int, stored []survival.DayStatus) []time.Time {
	return survival.SelectDays(survival.Selection{Now: now, StartDay: day(start), Days: stored})
}

func TestADayWaitsFromItsLastMarkPlusTwoDays(t *testing.T) {
	t.Parallel()
	last := at(0, 23, 59)
	if got, want := survival.Mark(last), at(30, 23, 59); !got.Equal(want) {
		t.Errorf("the mark of a change at %s is %s, want %s", last, got, want)
	}
	if got, want := survival.WaitsFrom(survival.DayOf(last)), day(33); !got.Equal(want) {
		t.Errorf("the day of a change at %s waits from %s, want %s", last, got, want)
	}
	if got := survival.DayOf(last); !got.Equal(day(0)) || got.Location() != time.UTC {
		t.Errorf("the day of %s is %s, want %s", last, got, day(0))
	}
	// Six days wait before day 0 (the start day is day -6); day 0 makes the seventh, and a run
	// starts, from day 0 + 33 days 00:00 and not a second before.
	if got := selectOn(day(33).Add(-time.Second), -6, nil); len(got) != 0 {
		t.Errorf("a second before day 0 waits, the run selects %v", got)
	}
	if got, want := selectOn(day(33), -6, nil), days(-6, 0); !reflect.DeepEqual(got, want) {
		t.Errorf("at day 0 + 33 days the run selects %v, want %v", got, want)
	}
}

func TestRunsTakeTheOldestDaysAndAtMostTwentyEight(t *testing.T) {
	t.Parallel()
	now := day(39 + 33) // days 0 to 39 wait
	if got, want := selectOn(now, 0, nil), days(0, 27); !reflect.DeepEqual(got, want) {
		t.Errorf("with 40 days waiting the run selects %v, want the oldest 28: %v", got, want)
	}
	if got, want := selectOn(now, 0, statuses(0, 9, survival.DayMeasured)), days(10, 37); !reflect.DeepEqual(got, want) {
		t.Errorf("with days 0 to 9 measured the run selects %v, want %v", got, want)
	}
	stored := append(statuses(0, 9, survival.DayMeasured), statuses(10, 30, survival.DayReplayed)...)
	if got, want := selectOn(now, 0, stored), days(10, 37); !reflect.DeepEqual(got, want) {
		t.Errorf("with days 10 to 30 replayed (a fingerprint and no row) the run selects %v, want %v", got, want)
	}
	if got := selectOn(now, 0, statuses(0, 33, survival.DayMeasured)); len(got) != 0 {
		t.Errorf("with six days waiting the run selects %v, want no run", got)
	}
	if got, want := selectOn(now, 0, statuses(0, 32, survival.DayMeasured)), days(33, 39); !reflect.DeepEqual(got, want) {
		t.Errorf("with seven days waiting the run selects %v, want %v", got, want)
	}
}

func TestNoDayBeforeTheStartDayIsSelected(t *testing.T) {
	t.Parallel()
	now := at(200, 15, 0)
	start := survival.StartDay(now)
	if want := day(200 - 33 - 83); !start.Equal(want) {
		t.Fatalf("the start day on %s is %s, want %s: 84 days that end on the newest waiting day", now, start, want)
	}
	if got, want := survival.SelectDays(survival.Selection{Now: now, StartDay: start}), days(84, 111); !reflect.DeepEqual(got, want) {
		t.Errorf("the first run selects %v, want the 28 days from the start day: %v", got, want)
	}
	// Days that reach 40 days before the start day, in every state that would select them, are
	// never selected, whatever waits after the start day.
	before := append(statuses(44, 59, survival.DayMarked), statuses(60, 63, survival.DayReplayed)...)
	for n := 64; n < 84; n++ {
		before = append(before, survival.DayStatus{Day: day(n), State: survival.DayBlocked, TriedAt: day(100)})
	}
	for _, measured := range []int{167, 163, 110, 105, 100, 84} {
		stored := append(slices.Clone(before), statuses(84, measured, survival.DayMeasured)...)
		got := survival.SelectDays(survival.Selection{Now: now, StartDay: start, Days: stored})
		for _, selected := range got {
			if selected.Before(start) {
				t.Errorf("with days 84 to %d measured the run selects %s, before the start day %s", measured,
					selected.Format(time.DateOnly), start.Format(time.DateOnly))
			}
		}
		var want []time.Time
		if waiting := 167 - measured; waiting >= 7 {
			want = days(measured+1, min(measured+28, 167))
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("with days 84 to %d measured the run selects %v, want %v", measured, got, want)
		}
	}
}

func TestAMarkedDayGoesFirst(t *testing.T) {
	t.Parallel()
	stored := with(statuses(0, 19, survival.DayMeasured), survival.DayStatus{Day: day(12), State: survival.DayMarked})
	want := append([]time.Time{day(12)}, days(20, 39)...)
	if got := selectOn(day(39+33), 0, stored); !reflect.DeepEqual(got, want) {
		t.Errorf("with day 12 marked and days 20 to 39 waiting the run selects %v, want %v", got, want)
	}
	// A marked day starts a run of its own, though fewer than seven days wait, and takes the
	// waiting days of its 28 days.
	want = append([]time.Time{day(12)}, days(20, 22)...)
	if got := selectOn(day(22+33), 0, stored); !reflect.DeepEqual(got, want) {
		t.Errorf("with day 12 marked and days 20 to 22 waiting the run selects %v, want %v", got, want)
	}
	// The oldest marked day starts the 28 days: a waiting day past them waits for the next run.
	stored = with(statuses(0, 29, survival.DayMeasured), survival.DayStatus{Day: day(3), State: survival.DayMarked},
		survival.DayStatus{Day: day(25), State: survival.DayMarked})
	want = append([]time.Time{day(3), day(25)}, days(30, 30)...)
	if got := selectOn(day(39+33), 0, stored); !reflect.DeepEqual(got, want) {
		t.Errorf("with days 3 and 25 marked the run selects %v, want %v", got, want)
	}
}

func TestABlockedDayIsRetriedWeeklyAndNotAheadOfWaitingDays(t *testing.T) {
	t.Parallel()
	blocked := func(n int, tried time.Time) survival.DayStatus {
		return survival.DayStatus{Day: day(n), State: survival.DayBlocked, TriedAt: tried}
	}
	measured := statuses(0, 29, survival.DayMeasured)
	for _, row := range []struct {
		name   string
		now    time.Time
		stored []survival.DayStatus
		want   []time.Time
	}{
		{"tried 3 days ago, 3 days waiting", day(32 + 33), with(measured, blocked(5, day(62))), nil},
		{"tried 7 days ago, 3 days waiting", day(32 + 33), with(measured, blocked(5, day(58))),
			append([]time.Time{day(5)}, days(30, 32)...)},
		{"due, and 10 days waiting: the waiting days first", day(39 + 33), with(measured, blocked(5, day(50))), days(30, 39)},
		{"among the waiting days, tried 2 days ago", day(40 + 33), with(statuses(0, 29, survival.DayMeasured), blocked(35, day(71))),
			slices.DeleteFunc(days(30, 40), func(d time.Time) bool { return d.Equal(day(35)) })},
		{"among the waiting days, due", day(40 + 33), with(statuses(0, 29, survival.DayMeasured), blocked(35, day(60))), days(30, 40)},
	} {
		if got := selectOn(row.now, 0, row.stored); !reflect.DeepEqual(got, row.want) {
			t.Errorf("a blocked day %s: the run selects %v, want %v", row.name, got, row.want)
		}
	}
}

func TestAChangedDayMarksEveryMeasuredDayWhoseThirtyDaysHoldIt(t *testing.T) {
	t.Parallel()
	h := synthetic(9, 61)
	stored := survival.Fingerprints(h.chain, day(0), day(60))
	if got := survival.ChangedDays(stored, h.chain); len(got) != 0 {
		t.Fatalf("the chain that gave the fingerprints changes the days %v", got)
	}
	// A force-push: a commit of day 45 gone, and one of day 50 replaced by another.
	var pushed []survival.Commit
	dropped, replaced := false, false
	for _, commit := range h.chain {
		switch {
		case !dropped && survival.DayOf(commit.CommittedAt).Equal(day(45)):
			dropped = true
			continue
		case !replaced && survival.DayOf(commit.CommittedAt).Equal(day(50)):
			replaced = true
			commit.SHA = "f" + commit.SHA[1:]
		}
		pushed = append(pushed, commit)
	}
	// And a commit dated on a day no fingerprint is stored for, which no day compares.
	pushed = append(pushed, survival.Commit{SHA: sha(9999), CommittedAt: at(70, 9, 0)})
	changed := survival.ChangedDays(stored, pushed)
	if want := []time.Time{day(45), day(50)}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("the changed days are %v, want %v", changed, want)
	}
	if got, want := survival.MarkedDays(changed, days(0, 30)), days(14, 30); !reflect.DeepEqual(got, want) {
		t.Errorf("with days 0 to 30 measured the marked days are %v, want %v (day 45 less 31 to day 50)", got, want)
	}
	if got, want := survival.MarkedDays([]time.Time{day(20)}, days(0, 30)), days(0, 20); !reflect.DeepEqual(got, want) {
		t.Errorf("a measured day that changed marks %v, want itself and the measured days of its 31 before: %v", got, want)
	}
	if got := survival.MarkedDays(changed, days(0, 13)); len(got) != 0 {
		t.Errorf("days 0 to 13, whose 30 days hold neither changed day, are marked: %v", got)
	}
}
