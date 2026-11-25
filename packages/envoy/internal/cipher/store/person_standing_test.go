package store_test

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// TestPersonStandingKeepsTheLaterAnswerOfACanonicalLogin: a person the broker kept no answer for
// is not found, and KeepPersonStanding writes an answer unless the row holds one Dispatch gave
// later by its own clock, so an older answer written after a newer one leaves the newer. The
// table takes a person as record.CanonicalLogin spells one and refuses every other spelling.
func TestPersonStandingKeepsTheLaterAnswerOfACanonicalLogin(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	const ada = "ada@example.com"
	if held, at, until, found, err := st.PersonStanding(ctx, ada); err != nil || found || held || !at.IsZero() || until != nil {
		t.Fatalf("PersonStanding(ada) with nothing kept = %t, %v, %v, %t, %v; want not found", held, at, until, found, err)
	}
	t1 := time.Date(2026, 11, 16, 0, 0, 1, 0, time.UTC)
	t2, t3 := t1.Add(time.Second), t1.Add(2*time.Second)
	read := func(wantHeld bool, wantAt time.Time, after string) {
		t.Helper()
		held, at, _, found, err := st.PersonStanding(ctx, ada)
		if err != nil || !found || held != wantHeld || !at.Equal(wantAt) {
			t.Fatalf("PersonStanding(ada) after %s = %t, %v, %t, %v; want held %t as of %v", after, held, at, found, err, wantHeld, wantAt)
		}
	}
	for _, write := range []struct {
		held bool
		asOf time.Time
	}{{true, t2}, {false, t1}} {
		if err := st.KeepPersonStanding(ctx, ada, write.held, write.asOf, nil); err != nil {
			t.Fatalf("KeepPersonStanding(ada, %t, %v): %v", write.held, write.asOf, err)
		}
	}
	read(true, t2, "a held answer as of t2, then a not-held one as of t1")
	if err := st.KeepPersonStanding(ctx, ada, false, t3, nil); err != nil {
		t.Fatal(err)
	}
	read(false, t3, "a not-held answer as of t3")
	for _, spelled := range []string{"Ada@example.com", " ada@example.com", "ada@example.com ", ""} {
		if err := st.KeepPersonStanding(ctx, spelled, true, t3, nil); !isCheckViolation(err) {
			t.Errorf("KeepPersonStanding(%q) = %v, want the table's check to refuse a login not spelled canonically", spelled, err)
		}
	}
}

// TestPersonStandingKeepsTheDeadlineWithItsAnswer: an answer's not_held_until, the deadline an IAP
// deployment's Dispatch gives a not-held answer, is written with that answer under the same rule,
// so the row's deadline is always the latest answer's: a later held answer, which names none,
// clears it; an older not-held answer written after it changes nothing; and a later not-held one
// writes its own. An answer naming no deadline, as outside IAP, reads back as none.
//
// Control: KeepPersonStanding's update leaving not_held_until out of its set list; the held
// answer as of t2 reads back with the deadline of the not-held answer before it, and the test
// fails naming that write.
func TestPersonStandingKeepsTheDeadlineWithItsAnswer(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	const ada = "ada@example.com"
	t1 := time.Date(2026, 11, 16, 0, 0, 1, 0, time.UTC)
	t2, t3, t4 := t1.Add(time.Second), t1.Add(2*time.Second), t1.Add(3*time.Second)
	deadline := func(at time.Time) *time.Time { until := at.Add(24 * time.Hour); return &until }
	for _, step := range []struct {
		held      bool
		asOf      time.Time
		until     *time.Time
		wantHeld  bool
		wantAsOf  time.Time
		wantUntil *time.Time
		write     string
	}{
		{false, t1, deadline(t1), false, t1, deadline(t1), "a not-held answer as of t1 with its deadline"},
		{true, t2, nil, true, t2, nil, "a held answer as of t2, which names no deadline"},
		{false, t1, deadline(t1), true, t2, nil, "the not-held answer as of t1 again, older than the row's"},
		{false, t3, deadline(t3), false, t3, deadline(t3), "a not-held answer as of t3 with its deadline"},
		{false, t4, nil, false, t4, nil, "a not-held answer as of t4 naming no deadline"},
	} {
		if err := st.KeepPersonStanding(ctx, ada, step.held, step.asOf, step.until); err != nil {
			t.Fatalf("KeepPersonStanding for %s: %v", step.write, err)
		}
		held, at, until, found, err := st.PersonStanding(ctx, ada)
		if err != nil || !found || held != step.wantHeld || !at.Equal(step.wantAsOf) || (until == nil) != (step.wantUntil == nil) ||
			(until != nil && !until.Equal(*step.wantUntil)) {
			t.Fatalf("PersonStanding(ada) after %s = held %t as of %v until %v (found %t, %v); want held %t as of %v until %v",
				step.write, held, at, until, found, err, step.wantHeld, step.wantAsOf, step.wantUntil)
		}
	}
}
