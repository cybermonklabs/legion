package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
		if err := st.KeepProjectStanding(ctx, step.person, "PRIV", store.ProjectStanding{Visibility: step.visibility, Visible: step.visible, AskedAt: step.asked}); err != nil {
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
		if err := st.KeepProjectStanding(ctx, step.person, "ACME", store.ProjectStanding{Visibility: step.visibility, Visible: step.visible, AskedAt: step.asked}); err != nil {
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
	if err := st.KeepProjectStanding(ctx, "", "PUB", store.ProjectStanding{Visibility: "public", Visible: true, AskedAt: asked}); err != nil {
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
		if err := st.KeepProjectStanding(ctx, step.person, "PUBP", store.ProjectStanding{Visibility: step.visibility, Visible: step.visible, AskedAt: step.asked}); err != nil {
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

// TestANoSuchProjectAnswerDeletesEveryEarlierAnswerOfItsProject: an answer that Dispatch holds no
// such project deletes every person's answer for that project asked before it, and leaves one
// asked after it. ada and bob are on PRIV's list as of t1; PRIV is deleted, no project as of t2;
// PRIV is made again, carol on its list as of t3, and its no-person answer, private, as of t4
// replaces the no-project answer. ada's answer as of t1, about the PRIV deleted at t2, is kept
// nothing: a read while Dispatch cannot be asked refuses her as one never asked rather than show
// her the new PRIV's rows. carol keeps her own answer.
//
// The fault: KeepProjectStanding keeping the person answers a no-project answer is later than;
// ada's and bob's rows are still there after it, and ada reads private and visible as of t1 after
// the no-person answer, and the test fails naming both.
func TestANoSuchProjectAnswerDeletesEveryEarlierAnswerOfItsProject(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	const ada, bob, carol = "ada@example.com", "bob@example.com", "carol@example.com"
	t1 := time.Date(2026, 11, 22, 0, 0, 1, 0, time.UTC)
	t2, t3, t4 := t1.Add(time.Second), t1.Add(2*time.Second), t1.Add(3*time.Second)
	for _, step := range []struct {
		person, visibility string
		visible            bool
		asked              time.Time
	}{
		{ada, "private", true, t1},
		{bob, "private", true, t1},
		{carol, "private", true, t3},
		{bob, "", false, t2},
	} {
		if err := st.KeepProjectStanding(ctx, step.person, "PRIV", store.ProjectStanding{Visibility: step.visibility, Visible: step.visible, AskedAt: step.asked}); err != nil {
			t.Fatalf("KeepProjectStanding(%q, PRIV, %q) as of %v: %v", step.person, step.visibility, step.asked, err)
		}
	}
	var people []string
	rows, err := st.Pool.Query(ctx, `select person from project_standing where project = 'PRIV' order by person`)
	if err != nil {
		t.Fatal(err)
	}
	if people, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(people, []string{"", carol}) {
		t.Errorf("PRIV's rows after the no-project answer as of t2 are for %q; want the no-project answer (\"\") and carol's, asked after it", people)
	}
	if err := st.KeepProjectStanding(ctx, "", "PRIV", store.ProjectStanding{Visibility: "private", AskedAt: t4}); err != nil {
		t.Fatal(err)
	}
	if got, found, err := st.KeptProjectStanding(ctx, ada, "PRIV"); err != nil || found {
		t.Fatalf("KeptProjectStanding(ada, PRIV) after PRIV was deleted and made again = %+v (found %t, %v); want nothing kept", got, found, err)
	}
	if got, found, err := st.KeptProjectStanding(ctx, carol, "PRIV"); err != nil || !found || !sameStanding(got, store.ProjectStanding{Visibility: "private", Visible: true, AskedAt: t3}) {
		t.Fatalf("KeptProjectStanding(carol, PRIV) = %+v (found %t, %v); want her own answer, private and visible as of %v", got, found, err, t3)
	}
}

// TestTheBrokerKeepsAtMostMaxKeptMissingProjectsNoSuchProjectAnswers: a project key is any word a
// caller names, so an answer that Dispatch holds no such project is kept for at most
// store.MaxKeptMissingProjects keys. A write past it ends the oldest asked, which a read while
// Dispatch cannot be asked then refuses as never asked, and keeps every later one, its own
// included. A no-project answer beside a person's answer of its project asked before it, which a
// slower process wrote after it, is not ended: it is what hides that answer.
//
// Faults: KeepProjectStanding with no cap; K1 still reads no project. The cap with no exception;
// ada reads her answer as of before HIDES was deleted.
func TestTheBrokerKeepsAtMostMaxKeptMissingProjectsNoSuchProjectAnswers(t *testing.T) {
	st := storetest.Open(t)
	ctx := t.Context()
	const ada, carol = "ada@example.com", "carol@example.com"
	t0 := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	// K1 to K<cap>, one a second from t0, each no project; HIDES no project as of t0, beside
	// ada's answer that she may see it as of the second before.
	if _, err := st.Pool.Exec(ctx, `insert into project_standing (person, project, visibility, visible, asked_at)
		select '', 'K' || i, null, false, $1::timestamptz + make_interval(secs => i) from generate_series(1, $2::int) i
		union all select '', 'HIDES', null, false, $1::timestamptz
		union all select $3, 'HIDES', 'private', true, $1::timestamptz - interval '1 second'`,
		t0, store.MaxKeptMissingProjects, ada); err != nil {
		t.Fatal(err)
	}
	newest := t0.Add(time.Duration(store.MaxKeptMissingProjects+1) * time.Second)
	if err := st.KeepProjectStanding(ctx, carol, "NEWEST", store.ProjectStanding{AskedAt: newest}); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := st.Pool.QueryRow(ctx, `select count(*) from project_standing where person = '' and visibility is null`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != store.MaxKeptMissingProjects+1 {
		t.Errorf("%d no-project answers kept; want %d, the cap and HIDES", kept, store.MaxKeptMissingProjects+1)
	}
	if got, found, err := st.KeptProjectStanding(ctx, carol, "K1"); err != nil || found {
		t.Errorf("KeptProjectStanding(carol, K1), the oldest past the cap, = %+v (found %t, %v); want nothing kept", got, found, err)
	}
	for project, asked := range map[string]time.Time{"K2": t0.Add(2 * time.Second), "NEWEST": newest} {
		if got, found, err := st.KeptProjectStanding(ctx, carol, project); err != nil || !found || !sameStanding(got, store.ProjectStanding{AskedAt: asked}) {
			t.Errorf("KeptProjectStanding(carol, %s) = %+v (found %t, %v); want no project as of %v", project, got, found, err, asked)
		}
	}
	if got, found, err := st.KeptProjectStanding(ctx, ada, "HIDES"); err != nil || !found || !sameStanding(got, store.ProjectStanding{AskedAt: t0}) {
		t.Errorf("KeptProjectStanding(ada, HIDES) = %+v (found %t, %v); want no project as of %v, which hides her earlier answer", got, found, err, t0)
	}
}

// sameStanding says whether got is want, the times compared as instants.
func sameStanding(got, want store.ProjectStanding) bool {
	return got.Visibility == want.Visibility && got.Visible == want.Visible && got.AskedAt.Equal(want.AskedAt)
}
