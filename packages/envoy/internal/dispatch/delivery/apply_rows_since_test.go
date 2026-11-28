package delivery

import (
	"testing"
	"time"
)

func TestApplyRowsSinceKeepsAnsweredDays(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	middle, third := first.AddDate(0, 0, 1), first.AddDate(0, 0, 2)
	rows := ApplyRows{
		from:    first,
		windows: map[int64]bool{first.Unix(): true, third.Unix(): true},
		Runs:    []DeliveryRun{{RunID: 1, CreatedAt: &first}, {RunID: 3, CreatedAt: &third}},
	}

	part, err := rows.Since(first)
	if err != nil {
		t.Fatalf("take first day: %v", err)
	}
	if _, err := part.Since(middle); err == nil {
		t.Fatalf("part answered the unanswered middle day %v", middle)
	}
	last, err := part.Since(third)
	if err != nil {
		t.Fatalf("take third day from the part: %v", err)
	}
	if len(last.Runs) != 1 || last.Runs[0].RunID != 3 {
		t.Fatalf("third day runs = %+v, want only run 3", last.Runs)
	}
	if len(last.windows) != 1 || !last.windows[third.Unix()] {
		t.Fatalf("third day windows = %v, want only %v", last.windows, third)
	}
}

func TestApplyRowsSinceKeepsUnrestrictedWindowsNil(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	second, third := first.AddDate(0, 0, 1), first.AddDate(0, 0, 2)
	rows := ApplyRows{from: first, Runs: []DeliveryRun{{RunID: 3, CreatedAt: &third}}}

	part, err := rows.Since(second)
	if err != nil {
		t.Fatalf("take second day from unrestricted rows: %v", err)
	}
	if part.windows != nil {
		t.Fatalf("unrestricted part has windows %v, want nil", part.windows)
	}
	if _, err := part.Since(first); err == nil {
		t.Fatalf("part answered %v, before its start %v", first, second)
	}
	for _, day := range []time.Time{second, third} {
		if _, err := part.Since(day); err != nil {
			t.Fatalf("take %v from unrestricted part: %v", day, err)
		}
	}
}
