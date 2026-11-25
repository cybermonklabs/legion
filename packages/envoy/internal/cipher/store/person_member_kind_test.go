package store_test

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// TestMemberKindKeepsTheLaterAnswer: a person no kind is kept for is not found, and
// KeepMemberKind writes the kind an answer named unless the row holds one Dispatch gave later by
// its own clock: a later restricted answer replaces a full one; an older full answer written after
// it changes nothing; a later answer naming no kind keeps none; and a later full one writes its
// own. The kind is kept as Dispatch spelled it, one this broker does not know included. The table
// takes a person as record.CanonicalLogin spells one and refuses every other spelling.
//
// Control: KeepMemberKind writing every answer whatever the row holds (its where clause gone); the
// full answer as of t1, written after the restricted one as of t2, reads back, and the test fails
// naming that write.
func TestMemberKindKeepsTheLaterAnswer(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	const ada = "ada@example.com"
	if kind, at, found, err := st.KeptMemberKind(ctx, ada); err != nil || found || kind != "" || !at.IsZero() {
		t.Fatalf("KeptMemberKind(ada) with nothing kept = %q, %v, %t, %v; want not found", kind, at, found, err)
	}
	t1 := time.Date(2026, 11, 22, 0, 0, 1, 0, time.UTC)
	t2, t3, t4, t5 := t1.Add(time.Second), t1.Add(2*time.Second), t1.Add(3*time.Second), t1.Add(4*time.Second)
	for _, step := range []struct {
		kind     string
		asOf     time.Time
		wantKind string
		wantAsOf time.Time
		write    string
	}{
		{"full", t1, "full", t1, "a full answer as of t1"},
		{"restricted", t2, "restricted", t2, "a restricted answer as of t2"},
		{"full", t1, "restricted", t2, "the full answer as of t1 again, older than the row's"},
		{"", t3, "", t3, "an answer as of t3 naming no kind"},
		{"full", t4, "full", t4, "a full answer as of t4"},
		{"guest-of-honour", t5, "guest-of-honour", t5, "an answer as of t5 naming a kind this broker does not know"},
	} {
		if err := st.KeepMemberKind(ctx, ada, step.kind, step.asOf); err != nil {
			t.Fatalf("KeepMemberKind for %s: %v", step.write, err)
		}
		kind, at, found, err := st.KeptMemberKind(ctx, ada)
		if err != nil || !found || kind != step.wantKind || !at.Equal(step.wantAsOf) {
			t.Fatalf("KeptMemberKind(ada) after %s = %q as of %v (found %t, %v); want %q as of %v", step.write, kind, at, found, err, step.wantKind, step.wantAsOf)
		}
	}
	for _, spelled := range []string{"Ada@example.com", " ada@example.com", "ada@example.com ", ""} {
		if err := st.KeepMemberKind(ctx, spelled, "full", t5); !isCheckViolation(err) {
			t.Errorf("KeepMemberKind(%q) = %v, want the table's check to refuse a login not spelled canonically", spelled, err)
		}
	}
}

// TestKeptMemberKindIsNoneOnceAnOlderReleaseKeepsALaterAnswer: a kept kind stands only while the
// hold person_standing keeps for the person rests on no later answer. The hold gate's write of the
// answer whose kind is kept, at the same as_of, leaves it standing. A broker release that keeps no
// kind, running beside this one while it rolls out, writes person_standing alone, by its upsert
// word for word: its later answer, which may be a demotion it read, leaves the earlier full kind
// unserved, read as no kind, so an outage refuses the person rather than serve them on it. A later
// kind from this release stands again, and so does the kind of an answer whose held Dispatch left
// out, which keeps no hold, over the older hold.
//
// The fault: KeptMemberKind answering the kind whatever hold person_standing keeps; ada's full
// answer as of t1 reads back after the older release's answer as of t2, and the test fails naming
// that write.
func TestKeptMemberKindIsNoneOnceAnOlderReleaseKeepsALaterAnswer(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	const ada = "ada@example.com"
	t1 := time.Date(2026, 11, 22, 0, 0, 1, 0, time.UTC)
	t2, t3, t4 := t1.Add(time.Second), t1.Add(2*time.Second), t1.Add(3*time.Second)
	olderRelease := func(asOf time.Time) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, `insert into person_standing (person, held, answered_at, not_held_until) values ($1, $2, $3, $4)
		on conflict (person) do update set held = excluded.held, answered_at = excluded.answered_at, not_held_until = excluded.not_held_until
		where person_standing.answered_at < excluded.answered_at`, ada, false, asOf, nil); err != nil {
			t.Fatalf("the older release's write as of %v: %v", asOf, err)
		}
	}
	kindOf := func(wantKind string, wantAsOf time.Time, after string) {
		t.Helper()
		kind, at, found, err := st.KeptMemberKind(ctx, ada)
		if err != nil || !found || kind != wantKind || !at.Equal(wantAsOf) {
			t.Fatalf("KeptMemberKind(ada) after %s = %q as of %v (found %t, %v); want %q as of %v", after, kind, at, found, err, wantKind, wantAsOf)
		}
	}
	if err := st.KeepMemberKind(ctx, ada, "full", t1); err != nil {
		t.Fatal(err)
	}
	if err := st.KeepPersonStanding(ctx, ada, false, t1, nil); err != nil {
		t.Fatal(err)
	}
	kindOf("full", t1, "a full answer as of t1, its hold kept by the gate")
	olderRelease(t2)
	kindOf("", t1, "the older release's answer as of t2")
	if err := st.KeepMemberKind(ctx, ada, "restricted", t3); err != nil {
		t.Fatal(err)
	}
	kindOf("restricted", t3, "a restricted answer as of t3 from this release")
	if err := st.KeepMemberKind(ctx, ada, "full", t4); err != nil {
		t.Fatal(err)
	}
	kindOf("full", t4, "a full answer as of t4 whose held Dispatch left out, so no hold is kept")
}
