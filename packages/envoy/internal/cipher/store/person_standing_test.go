package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// answerHeld and answerNotHeld are the held of an answer that names it, as KeepPersonAnswer takes
// it.
var (
	answerHeld    = new(true)
	answerNotHeld = new(false)
)

// TestPersonStandingKeepsTheLaterAnswerOfACanonicalLogin: a person the broker kept no answer for
// is not found, and KeepPersonAnswer writes an answer's hold unless the row holds one Dispatch gave
// later by its own clock, so an older answer written after a newer one leaves the newer. The
// table takes a person as record.CanonicalLogin spells one and refuses every other spelling.
func TestPersonStandingKeepsTheLaterAnswerOfACanonicalLogin(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	const ada = "ada@example.com"
	if standing, found, err := st.PersonStanding(ctx, ada); err != nil || found || standing != (store.Standing{}) {
		t.Fatalf("PersonStanding(ada) with nothing kept = %+v, %t, %v; want not found", standing, found, err)
	}
	t1 := time.Date(2026, 11, 16, 0, 0, 1, 0, time.UTC)
	t2, t3 := t1.Add(time.Second), t1.Add(2*time.Second)
	read := func(wantHeld bool, wantAt time.Time, after string) {
		t.Helper()
		standing, found, err := st.PersonStanding(ctx, ada)
		if err != nil || !found || standing.Held != wantHeld || !standing.AsOf.Equal(wantAt) {
			t.Fatalf("PersonStanding(ada) after %s = %+v, %t, %v; want held %t as of %v", after, standing, found, err, wantHeld, wantAt)
		}
	}
	for _, write := range []struct {
		held *bool
		asOf time.Time
	}{{answerHeld, t2}, {answerNotHeld, t1}} {
		if err := st.KeepPersonAnswer(ctx, ada, "full", write.held, write.asOf, nil); err != nil {
			t.Fatalf("KeepPersonAnswer(ada, %t, %v): %v", *write.held, write.asOf, err)
		}
	}
	read(true, t2, "a held answer as of t2, then a not-held one as of t1")
	if err := st.KeepPersonAnswer(ctx, ada, "full", answerNotHeld, t3, nil); err != nil {
		t.Fatal(err)
	}
	read(false, t3, "a not-held answer as of t3")
	for _, spelled := range []string{"Ada@example.com", " ada@example.com", "ada@example.com ", ""} {
		if err := st.KeepPersonAnswer(ctx, spelled, "full", answerHeld, t3, nil); !isCheckViolation(err) {
			t.Errorf("KeepPersonAnswer(%q) = %v, want the tables' checks to refuse a login not spelled canonically", spelled, err)
		}
	}
}

// TestPersonStandingKeepsTheDeadlineWithItsAnswer: an answer's not_held_until, the deadline an IAP
// deployment's Dispatch gives a not-held answer, is written with that answer under the same rule,
// so the row's deadline is always the latest answer's: a later held answer, which names none,
// clears it; an older not-held answer written after it changes nothing; and a later not-held one
// writes its own. An answer naming no deadline, as outside IAP, reads back as none.
//
// Control: KeepPersonAnswer's update of person_standing leaving not_held_until out of its set
// list; the held answer as of t2 reads back with the deadline of the not-held answer before it,
// and the test fails naming that write.
func TestPersonStandingKeepsTheDeadlineWithItsAnswer(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	const ada = "ada@example.com"
	t1 := time.Date(2026, 11, 16, 0, 0, 1, 0, time.UTC)
	t2, t3, t4 := t1.Add(time.Second), t1.Add(2*time.Second), t1.Add(3*time.Second)
	deadline := func(at time.Time) *time.Time { until := at.Add(24 * time.Hour); return &until }
	for _, step := range []struct {
		held      *bool
		asOf      time.Time
		until     *time.Time
		wantHeld  bool
		wantAsOf  time.Time
		wantUntil *time.Time
		write     string
	}{
		{answerNotHeld, t1, deadline(t1), false, t1, deadline(t1), "a not-held answer as of t1 with its deadline"},
		{answerHeld, t2, nil, true, t2, nil, "a held answer as of t2, which names no deadline"},
		{answerNotHeld, t1, deadline(t1), true, t2, nil, "the not-held answer as of t1 again, older than the row's"},
		{answerNotHeld, t3, deadline(t3), false, t3, deadline(t3), "a not-held answer as of t3 with its deadline"},
		{answerNotHeld, t4, nil, false, t4, nil, "a not-held answer as of t4 naming no deadline"},
	} {
		if err := st.KeepPersonAnswer(ctx, ada, "full", step.held, step.asOf, step.until); err != nil {
			t.Fatalf("KeepPersonAnswer for %s: %v", step.write, err)
		}
		standing, found, err := st.PersonStanding(ctx, ada)
		held, at, until := standing.Held, standing.AsOf, standing.NotHeldUntil
		if err != nil || !found || held != step.wantHeld || !at.Equal(step.wantAsOf) || (until == nil) != (step.wantUntil == nil) ||
			(until != nil && !until.Equal(*step.wantUntil)) {
			t.Fatalf("PersonStanding(ada) after %s = held %t as of %v until %v (found %t, %v); want held %t as of %v until %v",
				step.write, held, at, until, found, err, step.wantHeld, step.wantAsOf, step.wantUntil)
		}
	}
}

// TestMemberKindKeepsTheLaterAnswer: a person no kind is kept for is not found, and
// KeepPersonAnswer writes the kind an answer named unless the row holds one Dispatch gave later by
// its own clock, whether or not the answer names held: a later restricted answer replaces a full
// one; an older full answer written after it changes nothing; a later answer naming no kind keeps
// none; and a later full one writes its own. The kind is kept as Dispatch spelled it, one this
// broker does not know included.
//
// Control: KeepPersonAnswer writing every kind whatever the row holds (its where clause gone); the
// full answer as of t1, written after the restricted one as of t2, reads back, and the test fails
// naming that write.
func TestMemberKindKeepsTheLaterAnswer(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	const ada = "ada@example.com"
	if member, found, err := st.KeptMemberKind(ctx, ada); err != nil || found || member != (store.MemberKind{}) {
		t.Fatalf("KeptMemberKind(ada) with nothing kept = %+v, %t, %v; want not found", member, found, err)
	}
	t1 := time.Date(2026, 11, 22, 0, 0, 1, 0, time.UTC)
	t2, t3, t4, t5 := t1.Add(time.Second), t1.Add(2*time.Second), t1.Add(3*time.Second), t1.Add(4*time.Second)
	for _, step := range []struct {
		kind     string
		held     *bool
		asOf     time.Time
		wantKind string
		wantAsOf time.Time
		write    string
	}{
		{"full", answerNotHeld, t1, "full", t1, "a full answer as of t1"},
		{"restricted", nil, t2, "restricted", t2, "a restricted answer as of t2 whose held Dispatch left out"},
		{"full", answerNotHeld, t1, "restricted", t2, "the full answer as of t1 again, older than the row's"},
		{"", answerNotHeld, t3, "", t3, "an answer as of t3 naming no kind"},
		{"full", answerHeld, t4, "full", t4, "a full answer as of t4"},
		{"guest-of-honour", answerNotHeld, t5, "guest-of-honour", t5, "an answer as of t5 naming a kind this broker does not know"},
	} {
		if err := st.KeepPersonAnswer(ctx, ada, step.kind, step.held, step.asOf, nil); err != nil {
			t.Fatalf("KeepPersonAnswer for %s: %v", step.write, err)
		}
		member, found, err := st.KeptMemberKind(ctx, ada)
		if err != nil || !found || member.Kind != step.wantKind || !member.AsOf.Equal(step.wantAsOf) {
			t.Fatalf("KeptMemberKind(ada) after %s = %+v (found %t, %v); want kind %q as of %v", step.write, member, found, err, step.wantKind, step.wantAsOf)
		}
	}
	for _, spelled := range []string{"Ada@example.com", " ada@example.com", "ada@example.com ", ""} {
		if err := st.KeepPersonAnswer(ctx, spelled, "full", nil, t5, nil); !isCheckViolation(err) {
			t.Errorf("KeepPersonAnswer(%q) of its kind alone = %v, want the table's check to refuse a login not spelled canonically", spelled, err)
		}
	}
}

// TestKeepPersonAnswerWritesBothRowsOrNeither: one answer of the person route is kept in both its
// rows at its one as_of, or in neither. An answer naming held writes its kind and its hold; one
// whose held Dispatch left out writes its kind alone and leaves the hold kept before it. A later
// answer whose hold cannot be written (person_standing refusing writes, as a trigger makes it)
// fails, and leaves the kind kept before it as it was, so the kind is never ahead of the hold an
// answer carried.
//
// The fault: KeepPersonAnswer's two writes made on the pool, outside one transaction; the
// restricted kind of the answer as of t3 is kept though its hold was not, and the test fails
// naming that write.
func TestKeepPersonAnswerWritesBothRowsOrNeither(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	const ada = "ada@example.com"
	t1 := time.Date(2026, 11, 22, 0, 0, 1, 0, time.UTC)
	t2, t3 := t1.Add(time.Second), t1.Add(2*time.Second)
	rows := func(wantKind string, wantKindAsOf time.Time, wantHeld bool, wantHeldAsOf time.Time, after string) {
		t.Helper()
		member, kindFound, kindErr := st.KeptMemberKind(ctx, ada)
		standing, heldFound, heldErr := st.PersonStanding(ctx, ada)
		if kindErr != nil || heldErr != nil || !kindFound || !heldFound || member.Kind != wantKind || !member.AsOf.Equal(wantKindAsOf) ||
			standing.Held != wantHeld || !standing.AsOf.Equal(wantHeldAsOf) {
			t.Fatalf("after %s: kind %+v (found %t, %v), standing %+v (found %t, %v); want kind %q as of %v, held %t as of %v",
				after, member, kindFound, kindErr, standing, heldFound, heldErr, wantKind, wantKindAsOf, wantHeld, wantHeldAsOf)
		}
	}
	if err := st.KeepPersonAnswer(ctx, ada, "full", answerNotHeld, t1, nil); err != nil {
		t.Fatal(err)
	}
	rows("full", t1, false, t1, "a full, not-held answer as of t1")
	if err := st.KeepPersonAnswer(ctx, ada, "full", nil, t2, nil); err != nil {
		t.Fatal(err)
	}
	rows("full", t2, false, t1, "a full answer as of t2 whose held Dispatch left out")
	refuseStandingWrites(t, st)
	if err := st.KeepPersonAnswer(ctx, ada, "restricted", answerHeld, t3, nil); err == nil {
		t.Fatal("KeepPersonAnswer as of t3 with person_standing refusing writes = nil, want its failure")
	}
	rows("full", t2, false, t1, "a restricted, held answer as of t3 whose hold could not be written")
}

// refuseStandingWrites makes person_standing refuse every insert and update, as a table whose
// writes fail while person_member_kind's do not.
func refuseStandingWrites(t *testing.T, st *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`create function refuse_person_standing_writes() returns trigger language plpgsql as $$
			begin raise exception 'this test refuses every write to person_standing'; end $$`,
		`create trigger person_standing_refused before insert or update on person_standing
			for each row execute function refuse_person_standing_writes()`,
	} {
		if _, err := st.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
}
