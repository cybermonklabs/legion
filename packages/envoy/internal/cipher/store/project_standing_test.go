package store_test

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// TestProjectStandingKeepsTheLaterAnswer: an answer for a person and project is written unless the
// row holds one asked later, so an older answer written after a newer one leaves the newer. An
// answer that Dispatch holds no such project is kept for no person, and read for every person
// beside their own answer, the later of the two by when each was asked: so a project deleted after
// a person's answer reads as no project for them, and one made again reads as their later answer,
// and as nothing for a person not asked since. Nothing is kept for a person and project never
// answered.
//
// Control: KeepProjectStanding writing every answer whatever the row holds (its where clause
// gone); the older visible answer as of t1 reads back, and the test fails naming that write.
func TestProjectStandingKeepsTheLaterAnswer(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	const ada, bob = "ada@example.com", "bob@example.com"
	if got, found, err := st.KeptProjectStanding(ctx, ada, "PRIV"); err != nil || found || got != (store.ProjectStanding{}) {
		t.Fatalf("KeptProjectStanding(ada, PRIV) with nothing kept = %+v, %t, %v; want not found", got, found, err)
	}
	t1 := time.Date(2026, 11, 22, 0, 0, 1, 0, time.UTC)
	t2, t3, t4 := t1.Add(time.Second), t1.Add(2*time.Second), t1.Add(3*time.Second)
	// A read whose at is zero wants nothing kept.
	type read struct {
		person, visibility string
		visible            bool
		at                 time.Time
	}
	for _, step := range []struct {
		person, visibility string
		visible            bool
		asked              time.Time
		want               []read
		write              string
	}{
		{ada, "private", false, t2, []read{{ada, "private", false, t2}}, "ada off PRIV's list as of t2"},
		{ada, "private", true, t1, []read{{ada, "private", false, t2}}, "ada on PRIV's list as of t1, older than the row's"},
		{bob, "private", true, t2, []read{{ada, "private", false, t2}, {bob, "private", true, t2}}, "bob on PRIV's list as of t2"},
		{bob, "", false, t3, []read{{ada, "", false, t3}, {bob, "", false, t3}}, "no such project as of t3, asked for bob"},
		{ada, "public", true, t4, []read{{ada, "public", true, t4}, {bob, "", false, time.Time{}}}, "PRIV made again public, ada's answer as of t4"},
	} {
		if err := st.KeepProjectStanding(ctx, step.person, "PRIV", step.visibility, step.visible, step.asked); err != nil {
			t.Fatalf("KeepProjectStanding for %s: %v", step.write, err)
		}
		for _, want := range step.want {
			got, found, err := st.KeptProjectStanding(ctx, want.person, "PRIV")
			vis, visible, at := got.Visibility, got.Visible, got.AskedAt
			if want.at.IsZero() {
				if err != nil || found {
					t.Fatalf("KeptProjectStanding(%s, PRIV) after %s = %q visible %t as of %v (found %t, %v); want nothing kept",
						want.person, step.write, vis, visible, at, found, err)
				}
				continue
			}
			if err != nil || !found || vis != want.visibility || visible != want.visible || !at.Equal(want.at) {
				t.Fatalf("KeptProjectStanding(%s, PRIV) after %s = %q visible %t as of %v (found %t, %v); want %q visible %t as of %v",
					want.person, step.write, vis, visible, at, found, err, want.visibility, want.visible, want.at)
			}
		}
	}
}

// TestProjectStandingNoSuchProjectEndsAtAnAnswerThatItExists: an answer that Dispatch holds no
// such project, kept for no person, stands for every person only until an answer that the project
// exists, kept for anyone, was asked after it: the project was made since, so a person with no
// answer of their own asked after that is kept nothing, and a read during an outage refuses them as
// one never asked rather than show them the project's rows as no project. A reason names ACME-3
// before ACME exists: no project as of t1, for everyone, dave included, whose own answer as of t0
// was about an ACME deleted before t1. ACME is then made private, its list naming ada, whose answer
// as of t2 is kept: carol, never asked, dave and a question naming no person are kept nothing, and
// ada reads her own answer. ACME deleted again, no project as of t3, stands for everyone again.
//
// The fault: KeptProjectStanding reading the no-project row whatever was kept after it; carol
// reads no project as of t1 after ada's answer as of t2, and the test fails naming her.
func TestProjectStandingNoSuchProjectEndsAtAnAnswerThatItExists(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	const ada, bob, carol, dave = "ada@example.com", "bob@example.com", "carol@example.com", "dave@example.com"
	t0 := time.Date(2026, 11, 22, 0, 0, 1, 0, time.UTC)
	t1, t2, t3 := t0.Add(time.Second), t0.Add(2*time.Second), t0.Add(3*time.Second)
	// A read whose at is zero wants nothing kept.
	type read struct {
		person, visibility string
		visible            bool
		at                 time.Time
	}
	for _, step := range []struct {
		person, visibility string
		visible            bool
		asked              time.Time
		want               []read
		write              string
	}{
		{dave, "private", true, t0, []read{{dave, "private", true, t0}}, "dave on an earlier ACME's list as of t0"},
		{bob, "", false, t1, []read{{carol, "", false, t1}, {dave, "", false, t1}}, "no such project as of t1, asked for bob"},
		{ada, "private", true, t2, []read{{ada, "private", true, t2}, {carol, "", false, time.Time{}}, {dave, "", false, time.Time{}},
			{bob, "", false, time.Time{}}, {"", "", false, time.Time{}}}, "ACME made private, ada on its list as of t2"},
		{carol, "", false, t3, []read{{ada, "", false, t3}, {carol, "", false, t3}, {dave, "", false, t3}}, "no such project again as of t3, asked for carol"},
	} {
		if err := st.KeepProjectStanding(ctx, step.person, "ACME", step.visibility, step.visible, step.asked); err != nil {
			t.Fatalf("KeepProjectStanding for %s: %v", step.write, err)
		}
		for _, want := range step.want {
			got, found, err := st.KeptProjectStanding(ctx, want.person, "ACME")
			vis, visible, at := got.Visibility, got.Visible, got.AskedAt
			if want.at.IsZero() {
				if err != nil || found {
					t.Fatalf("KeptProjectStanding(%q, ACME) after %s = %q visible %t as of %v (found %t, %v); want nothing kept",
						want.person, step.write, vis, visible, at, found, err)
				}
				continue
			}
			if err != nil || !found || vis != want.visibility || visible != want.visible || !at.Equal(want.at) {
				t.Fatalf("KeptProjectStanding(%q, ACME) after %s = %q visible %t as of %v (found %t, %v); want %q visible %t as of %v",
					want.person, step.write, vis, visible, at, found, err, want.visibility, want.visible, want.at)
			}
		}
	}
}

// TestProjectStandingKeepsANoPersonAnswerForNoPersonAlone: the answer to a question naming no
// person, whether the project is public, is kept for no person and read back for a read naming no
// person alone. A person with no answer of their own is kept nothing, though that answer is
// public, so a read during an outage refuses them as one never asked; only an answer that Dispatch
// holds no such project is read for everyone.
//
// The fault: KeptProjectStanding reading every row kept for no person for everyone, a project's
// public answer among them; carol reads PUB public, and the test fails naming her.
func TestProjectStandingKeepsANoPersonAnswerForNoPersonAlone(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	asked := time.Date(2026, 11, 22, 0, 0, 1, 0, time.UTC)
	if err := st.KeepProjectStanding(ctx, "", "PUB", "public", true, asked); err != nil {
		t.Fatal(err)
	}
	if got, found, err := st.KeptProjectStanding(ctx, "", "PUB"); err != nil || !found || got.Visibility != "public" || !got.Visible || !got.AskedAt.Equal(asked) {
		t.Fatalf("KeptProjectStanding(no person, PUB) = %+v (found %t, %v); want public and visible as of %v", got, found, err, asked)
	}
	if got, found, err := st.KeptProjectStanding(ctx, "carol@example.com", "PUB"); err != nil || found {
		t.Fatalf("KeptProjectStanding(carol, PUB) after the no-person answer = %+v (found %t, %v); want nothing kept", got, found, err)
	}
}

// TestProjectStandingPublicEndsAtAnAnswerThatItIsPrivate: whether a project is public is a fact of
// the project, so a person's kept public answer stands only until an answer that it is private,
// kept for anyone, no person included, was asked at or after it; then that person, with no answer
// of their own since, is kept nothing, and a read during an outage refuses them as one never
// asked rather than show them the project's rows. bob is answered PUBP public as of t1; dave's
// private answer as of t0, older, written after, ends nothing. PUBP is made private, its list
// naming ada, whose answer as of t2 is kept: bob is kept nothing. PUBP is made public again, and
// carol's answer as of t3 is kept: bob is still kept nothing, since PUBP was private after his own
// answer, and ada keeps her private answer, which a switch to public does not end, since whether
// she may see a private project rests on its list. A no-person answer that PUBP is private as of
// t4, as the policy loader's question asks, ends carol's public answer the same way.
//
// Faults: KeptProjectStanding with no public rule; bob reads public as of t1 after ada's answer.
// Every kept visibility ended by a later different one; ada reads nothing after carol's answer.
func TestProjectStandingPublicEndsAtAnAnswerThatItIsPrivate(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	const ada, bob, carol, dave = "ada@example.com", "bob@example.com", "carol@example.com", "dave@example.com"
	t0 := time.Date(2026, 11, 22, 0, 0, 1, 0, time.UTC)
	t1, t2, t3, t4 := t0.Add(time.Second), t0.Add(2*time.Second), t0.Add(3*time.Second), t0.Add(4*time.Second)
	// A read whose at is zero wants nothing kept.
	type read struct {
		person, visibility string
		visible            bool
		at                 time.Time
	}
	for _, step := range []struct {
		person, visibility string
		visible            bool
		asked              time.Time
		want               []read
		write              string
	}{
		{bob, "public", true, t1, []read{{bob, "public", true, t1}}, "bob answered PUBP public as of t1"},
		{dave, "private", true, t0, []read{{bob, "public", true, t1}, {dave, "private", true, t0}}, "dave's private answer as of t0, older, written after"},
		{ada, "private", true, t2, []read{{bob, "", false, time.Time{}}, {ada, "private", true, t2}, {"", "", false, time.Time{}}},
			"PUBP made private, ada on its list as of t2"},
		{carol, "public", true, t3, []read{{bob, "", false, time.Time{}}, {carol, "public", true, t3}, {ada, "private", true, t2}},
			"PUBP made public again, carol's answer as of t3"},
		{"", "private", false, t4, []read{{carol, "", false, time.Time{}}, {ada, "private", true, t2}, {"", "private", false, t4}},
			"PUBP made private again, the no-person answer as of t4"},
	} {
		if err := st.KeepProjectStanding(ctx, step.person, "PUBP", step.visibility, step.visible, step.asked); err != nil {
			t.Fatalf("KeepProjectStanding for %s: %v", step.write, err)
		}
		for _, want := range step.want {
			got, found, err := st.KeptProjectStanding(ctx, want.person, "PUBP")
			vis, visible, at := got.Visibility, got.Visible, got.AskedAt
			if want.at.IsZero() {
				if err != nil || found {
					t.Fatalf("KeptProjectStanding(%q, PUBP) after %s = %q visible %t as of %v (found %t, %v); want nothing kept",
						want.person, step.write, vis, visible, at, found, err)
				}
				continue
			}
			if err != nil || !found || vis != want.visibility || visible != want.visible || !at.Equal(want.at) {
				t.Fatalf("KeptProjectStanding(%q, PUBP) after %s = %q visible %t as of %v (found %t, %v); want %q visible %t as of %v",
					want.person, step.write, vis, visible, at, found, err, want.visibility, want.visible, want.at)
			}
		}
	}
}
