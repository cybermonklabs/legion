package metrics

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// closingTolerance is how far apart two hours values of one fact may stand: a tenth of an hour,
// what R20 holds the stored facts to against the computation. Counts are held exactly.
const closingTolerance = 0.1

// closingHistory is the facts fixtures' history (legiontest) with every running part an open
// issue can be left in at the history's end, each written over the rig's own:
//
//   - the root of the first tree reopened, whose open child works after the root's own newest
//     input, so the root's time waiting on its children runs on an input of another issue;
//   - an ask a Legion session opened on an open issue and nobody answered;
//   - an open issue admitted with no slot of its own, waiting for one;
//   - an open issue a person parked;
//   - an open issue holding a slot of its own in its implementer's bar with the implementer out of
//     a turn, so its idle hours by role run;
//   - an open child whose root is recorded in `awaiting_person` and then read with no phase (the
//     `null` mark a read that drops a field leaves) before its close, so the child's wait on a
//     person runs on through that read to the root's close.
//
// Each is written at or before the history's end, and the history writes nothing after them.
type closingHistory struct {
	database *store.Store
	fixture  legiontest.Fixture
	now      time.Time
	// cases names the open issue of each running part, by what runs.
	cases map[string]string
}

func seedClosingHistory(t *testing.T) closingHistory {
	t.Helper()
	database := storetest.Open(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{
		Project: "LEGION", Trees: 6, Now: now, Queued: true, InBacklogOrIcebox: true, Merged: true,
	})
	seeded := closingHistory{database: database, fixture: fixture, now: now, cases: map[string]string{}}
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(t.Context(), statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	observe := func(issue, field, value string, at time.Time) {
		t.Helper()
		exec("observe "+field+" of "+issue, `
			insert into legion_observations (project, issue_key, field, value, observed_at)
			values ($1, $2, $3, $4::jsonb, $5)
		`, fixture.Project, issue, field, value, at)
	}
	event := func(issue, kind, actor, payload string, at time.Time) {
		t.Helper()
		exec("write a "+kind+" event of "+issue, `
			insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			values ($1, (select coalesce(max(seq), 0) + 1 from events where issue_key = $1),
			        $2, $3::jsonb, $4::jsonb, false, $5)
		`, issue, kind, actor, payload, at)
	}

	// The first tree's root, reopened: its own inputs end a minute after its close, four hours
	// before the history's end, and its open child starts implementing an hour before the end. The
	// daemon reports the reopened root in `accepting`, the phase a root waits on its children in,
	// a minute after that close: with its newest phase the `done` of its close it would be out of
	// the workflow before its close, which is outside the work time and on no bar
	// (dispatch://INFRA-1077's 9f994275a7), and so it would wait on no child.
	root, child := fixture.Roots[0], fixture.Open[0]
	exec("observe "+root+" back in the workflow", `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		select $1, key, 'phase', '"accepting"'::jsonb, closed_at + interval '1 minute'
		from issues where key = $2
	`, fixture.Project, root)
	exec("reopen "+root, `update issues set closed_at = null, status = 'in_progress' where key = $1`, root)
	observe(child, "phase", `"implementing"`, now.Add(-time.Hour))
	observe(child, "claim.implementer", `"working"`, now.Add(-time.Hour))
	seeded.cases["a root whose child works after the root's own newest input"] = root
	seeded.cases["that child, at work"] = child

	asked := fixture.Open[1]
	event(asked, "ask.opened", legiontest.SessionActor("implementer", asked), `{"id":"closing-open-ask"}`, now.Add(-2*time.Hour))
	seeded.cases["an ask nobody answered"] = asked

	waiting := fixture.Open[2]
	observe(waiting, "phase", `"admitted"`, now.Add(-3*time.Hour))
	seeded.cases["an open slot wait"] = waiting

	parked := fixture.Open[3]
	event(parked, "issue.updated", `{"kind":"user","id":"alice"}`, `{"status":"backlog"}`, now.Add(-2*time.Hour))
	seeded.cases["a parked spell"] = parked

	idle := fixture.Open[4]
	observe(idle, "slot", `3`, now.Add(-5*time.Hour))
	observe(idle, "phase", `"implementing"`, now.Add(-5*time.Hour))
	observe(idle, "claim.implementer", `"working"`, now.Add(-5*time.Hour))
	observe(idle, "claim.implementer", `null`, now.Add(-4*time.Hour))
	seeded.cases["a slot held with its role out of a turn"] = idle

	// The last tree's root is recorded in `awaiting_person` five hours before its close, after its
	// open child's own newest input, and read with no phase an hour later. A `null` mark ends no
	// wait (the timeline's phaseStretches), so the child waits on a person until the root's `done`
	// at its close, which falls more than an hour past the first UTC midnight after that read:
	// facts closed at the read would grow the wait over the hour their rate is measured in and on
	// past the close.
	waitedOn, beneath := fixture.Roots[len(fixture.Roots)-1], fixture.Open[len(fixture.Open)-1]
	var closedAt time.Time
	if err := database.Pool.QueryRow(t.Context(), `select closed_at from issues where key = $1`, waitedOn).Scan(&closedAt); err != nil {
		t.Fatalf("read the close of %s: %v", waitedOn, err)
	}
	awaiting, unread := closedAt.Add(-5*time.Hour), closedAt.Add(-4*time.Hour)
	if midnight := measures.Midnight(unread).AddDate(0, 0, 1); !closedAt.After(midnight.Add(time.Hour)) {
		t.Fatalf("%s closes at %s, within an hour of the midnight %s after its read with no phase at %s, so its child's rate is measured after the wait ends",
			waitedOn, closedAt.UTC(), midnight, unread.UTC())
	}
	observe(waitedOn, "phase", `"awaiting_person"`, awaiting)
	observe(waitedOn, "phase", `null`, unread)
	seeded.cases["a child whose root waits on a person through a read with no phase"] = beneath
	return seeded
}

// closingMoments is the clock times a check reads an issue closed at asOf at, with no input in
// between: a minute, two hours and three days after it, and a minute on either side of the first
// UTC midnight at least two hours after it.
func closingMoments(asOf time.Time) []time.Time {
	midnight := measures.Midnight(asOf.Add(2*time.Hour)).AddDate(0, 0, 1)
	return []time.Time{
		asOf.Add(time.Minute), asOf.Add(2 * time.Hour), asOf.Add(72 * time.Hour),
		midnight.Add(-time.Minute), midnight.Add(time.Minute),
	}
}

// computedAt is the computation of the project's facts at a moment (factsAt) over the history's
// unchanged inputs, as a read made at each moment answers them, each moment read once.
type computedAt struct {
	t       *testing.T
	pool    *store.Pool
	project string
	held    map[time.Time]map[string]IssueFacts
}

func (c *computedAt) at(moment time.Time) map[string]IssueFacts {
	c.t.Helper()
	if facts, held := c.held[moment]; held {
		return facts
	}
	population, err := ReadProject(c.t.Context(), c.pool, c.project, moment)
	if err != nil {
		c.t.Fatalf("read the population at %s: %v", moment.UTC(), err)
	}
	facts := factsAt(population, nil)
	c.held[moment] = facts
	return facts
}

// TestFactsClosedAtAnyMomentAreTheComputationAtThatMoment is the property R21 rests on, over
// every open issue of the facts fixtures with each running part an open issue can be left in
// (closingHistory), and at clock times a minute, two hours and three days after each issue's
// closing moment and a minute on either side of a UTC midnight, with no input in between:
//
//	(a) the facts ProjectFactsClosed answers, grown to the moment by the one computation a
//	    reader of a stored row makes (IssueFacts.Grown, which legionfacts' Row.At calls), are
//	    the computation run at that moment over the same inputs (factsOf), field for field, to a
//	    tenth of an hour and exactly for counts;
//	(b) each fact the computation answers at the moment is its value at the closing moment plus
//	    its rate times the time elapsed, read with this test's own arithmetic rather than with
//	    Grown. It fails the day a rule of the computation comes to depend on elapsed time, and
//	    the day a closing moment misses an input of the issue's tree.
func TestFactsClosedAtAnyMomentAreTheComputationAtThatMoment(t *testing.T) {
	seeded := seedClosingHistory(t)
	pool, project := seeded.database.Pool, seeded.fixture.Project
	population, err := ReadProject(t.Context(), pool, project, seeded.now)
	if err != nil {
		t.Fatalf("read the population: %v", err)
	}
	closed := ProjectFactsClosed(population, nil)
	computed := &computedAt{t: t, pool: pool, project: project, held: map[time.Time]map[string]IssueFacts{}}

	open := []string{}
	for _, key := range slices.Sorted(maps.Keys(closed)) {
		if closed[key].ClosedAt == nil {
			open = append(open, key)
		}
	}
	for what, key := range seeded.cases {
		if !slices.Contains(open, key) {
			t.Fatalf("%s is %s and has no open facts: this test examined nothing of it", key, what)
		}
		if len(closed[key].Growth) == 0 {
			t.Errorf("%s is %s and its facts carry no growth", key, what)
		}
	}

	compared, differedA, differedB := 0, 0, 0
	for _, key := range open {
		facts := closed[key]
		for _, moment := range closingMoments(facts.AsOf) {
			want, has := computed.at(moment)[key]
			if !has {
				t.Errorf("%s has open facts and the computation at %s answers no facts of it", key, moment.UTC())
				continue
			}
			compared++
			for _, difference := range factsDifferences(facts.FactsAt(moment), want) {
				differedA++
				t.Errorf("(a) %s closed at %s (%s after its closing moment %s): %s",
					key, moment.UTC(), moment.Sub(facts.AsOf), facts.AsOf.UTC(), difference)
			}
			for _, difference := range growthDifferences(facts, want, moment) {
				differedB++
				t.Errorf("(b) %s at %s (%s after its closing moment %s): %s",
					key, moment.UTC(), moment.Sub(facts.AsOf), facts.AsOf.UTC(), difference)
			}
		}
	}
	if compared == 0 {
		t.Fatal("no open issue was compared at any moment: this test examined nothing")
	}
	for what, key := range seeded.cases {
		t.Logf("%s: %s, closed at %s, growth %v", what, key, closed[key].AsOf.UTC(), closed[key].Growth)
	}
	t.Logf("%d open issues read at 5 moments each, %d comparisons: %d differences of (a), %d of (b)",
		len(open), compared, differedA, differedB)
}

// factsDifferences names each fact got and want hold differently: the hours to a tenth of an
// hour, and the rest as otherDifferences reads them.
func factsDifferences(got, want IssueFacts) []string {
	differences := []string{}
	hours, wanted := flatHours(got), flatHours(want)
	for _, name := range slices.Sorted(maps.Keys(eitherSide(hours, wanted))) {
		if math.Abs(hours[name]-wanted[name]) > closingTolerance {
			differences = append(differences, fmt.Sprintf("%s is %g and the computation's %g", name, hours[name], wanted[name]))
		}
	}
	return append(differences, otherDifferences(got, want)...)
}

// otherDifferences names each fact but the hours that got and want hold differently: the moments
// to a tenth of an hour, and the counts, the tree and the flags exactly.
func otherDifferences(got, want IssueFacts) []string {
	differences := []string{}
	for _, moment := range []struct {
		name      string
		got, want *time.Time
	}{
		{"opened_at", &got.OpenedAt, &want.OpenedAt}, {"closed_at", got.ClosedAt, want.ClosedAt},
		{"work_first_at", got.WorkFirstAt, want.WorkFirstAt}, {"work_last_at", got.WorkLastAt, want.WorkLastAt},
		{"started_at", got.StartedAt, want.StartedAt}, {"merged_at", got.MergedAt, want.MergedAt},
	} {
		switch {
		case (moment.got == nil) != (moment.want == nil):
			differences = append(differences, fmt.Sprintf("%s is %v and the computation's %v", moment.name, utc(moment.got), utc(moment.want)))
		case moment.got != nil && math.Abs(moment.got.Sub(*moment.want).Hours()) > closingTolerance:
			differences = append(differences, fmt.Sprintf("%s is %s and the computation's %s", moment.name, moment.got.UTC(), moment.want.UTC()))
		}
	}
	exact := []struct {
		name      string
		got, want any
	}{
		{"review_rounds", got.ReviewRounds, want.ReviewRounds}, {"root", got.Root, want.Root},
		{"tree", got.Tree, want.Tree}, {"tree_depth", got.TreeDepth, want.TreeDepth},
		{"sent_back", got.SentBack, want.SentBack}, {"children", got.Children, want.Children},
	}
	for _, role := range slices.Sorted(maps.Keys(eitherSide(got.Bars, want.Bars))) {
		exact = append(exact, struct {
			name      string
			got, want any
		}{"bars." + role + ".holds_hours", got.Bars[role].HoldsHours, want.Bars[role].HoldsHours})
	}
	for _, fact := range exact {
		if fact.got != fact.want {
			differences = append(differences, fmt.Sprintf("%s is %v and the computation's %v", fact.name, fact.got, fact.want))
		}
	}
	return differences
}

// growthDifferences names each fact at whose value the computation at moment stands apart from
// closed's value at its closing moment grown by its rate over the time elapsed, by this test's
// own arithmetic: each hours fact and the last moment of work to a tenth of an hour, and every
// count, flag and other moment as closed holds it.
func growthDifferences(closed ClosedFacts, want IssueFacts, moment time.Time) []string {
	elapsed := moment.Sub(closed.AsOf).Hours()
	differences := []string{}
	at, wanted := flatHours(closed.IssueFacts), flatHours(want)
	for _, name := range slices.Sorted(maps.Keys(eitherSide(at, wanted))) {
		grown := at[name] + closed.Growth[name]*elapsed
		if math.Abs(grown-wanted[name]) > closingTolerance {
			differences = append(differences, fmt.Sprintf("%s is %g at the closing moment and grows %g an hour, so %g, and the computation answers %g",
				name, at[name], closed.Growth[name], grown, wanted[name]))
		}
	}
	unmoved := closed.IssueFacts
	if last := closed.WorkLastAt; last != nil {
		grown := last.Add(time.Duration(closed.Growth["work_last_at"] * float64(moment.Sub(closed.AsOf))))
		unmoved.WorkLastAt = &grown
	}
	return append(differences, otherDifferences(unmoved, want)...)
}

// flatHours is every hours fact of facts by the name Growth keys it by: the field's name in snake
// case, and for an entry of a map the entry's key after a dot. It is this test's own list rather
// than hoursFacts, so a fact the computation comes to grow without this list naming it reads as a
// difference of (a).
func flatHours(facts IssueFacts) map[string]float64 {
	flat := map[string]float64{
		"work_hours": facts.WorkHours, "slot_hours": facts.SlotHours, "parked_hours": facts.ParkedHours,
		"queued_hours": facts.QueuedHours, "slot_wait_hours": facts.SlotWaitHours,
		"implementing_hours_after_first_review": facts.ImplementingHoursAfterFirstReview,
		"testing_hours_after_first_review":      facts.TestingHoursAfterFirstReview,
	}
	for id, hours := range facts.PhaseHours {
		flat["phase_hours."+id] = hours
	}
	for role, bar := range facts.Bars {
		flat["bars."+role+".hours"] = bar.Hours
		for segment, hours := range bar.Segments.ByName() {
			flat["bars."+role+".segments."+string(segment)] = hours
		}
	}
	for role, hours := range facts.IdleByRole {
		flat["idle_by_role."+role] = hours
	}
	return flat
}

// eitherSide is every key either side holds.
func eitherSide[Left, Right any](left map[string]Left, right map[string]Right) map[string]struct{} {
	keys := map[string]struct{}{}
	for key := range left {
		keys[key] = struct{}{}
	}
	for key := range right {
		keys[key] = struct{}{}
	}
	return keys
}

// utc is a moment as a failure prints it: UTC, or nil.
func utc(at *time.Time) any {
	if at == nil {
		return nil
	}
	return at.UTC()
}
