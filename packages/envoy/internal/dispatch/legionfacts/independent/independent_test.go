package independent_test

import (
	"maps"
	"math"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts/independent"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// hourTolerance is how far apart two readings of one hour figure may stand: 0.1 hour, which is
// what R1 holds the two sides of its check to. Every count beside it is compared exactly.
const hourTolerance = 0.1

// issueFloor is the least number of issues one comparison proves anything over, which R1's check
// states: below it the fixture is too small to have reached the shapes the rules differ on.
const issueFloor = 200

// TestEveryFactEqualsTheIndependentReadingOfTheRequirement is the check of R1 itself: the one
// computation of the per-issue facts, as the stored rows hold them (metrics.ProjectFactsClosed)
// grown to the read's end, against a reading written from the requirement's words (package
// independent), over one history of more than 200 issues with Legion phase work.
//
// The history holds one tree the sampler never saw whose issues ran a plan of the steps
// `implementer reviewer acceptance:owner` (legiontest.Spec's PlannedTree), so the two sides also
// meet where an issue's plan orders its phases before observations (k28).
//
// It fails where the two disagree on any fact, below the floor of issues, where the history's
// issue with no measured work is missing from either side, and where no issue of the planned tree
// has hours that the order of the default steps would put in another phase.
func TestEveryFactEqualsTheIndependentReadingOfTheRequirement(t *testing.T) {
	database := storetest.Open(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: "LEGION", Now: now, AwaitingChildren: true, PlannedTree: true})

	computed, population := computedFacts(t, database.Pool, fixture.Project, now, false)
	computedScope := population.Project
	computedWindow := independent.Window{From: population.From, To: population.To}
	scope := independent.Scope{Project: fixture.Project}
	window := independent.Window{To: now}
	reading, err := independent.Read(t.Context(), database.Pool, scope, window)
	if err != nil {
		t.Fatalf("read the project independently: %v", err)
	}

	planned := map[string]bool{}
	for _, key := range fixture.Planned {
		planned[key] = true
	}
	compared, zeroHours, awaitingChildren, plannedCompared, differences := 0, 0, 0, 0, 0
	for _, key := range slices.Sorted(maps.Keys(union(reading.Issues, computed))) {
		mine, read := reading.Issues[key]
		theirs, computedIt := computed[key]
		switch {
		case !read:
			t.Errorf("%s has facts from the computation and none from the reading", key)
			differences++
			continue
		case !computedIt:
			t.Errorf("%s has facts from the reading and none from the computation", key)
			differences++
			continue
		}
		compared++
		if planned[key] {
			plannedCompared++
		}
		if mine.ZeroHours() {
			zeroHours++
		}
		if mine.Root && mine.PhaseHours["awaiting_children"] > 0 && theirs.PhaseHours["awaiting_children"] > 0 {
			awaitingChildren++
		}
		differences += compareFacts(t, key, mine, theirs)
	}

	if compared < issueFloor {
		t.Errorf("the two sides compared %d issues, want at least %d", compared, issueFloor)
	}
	// The history holds an issue whose work took no measured time, and R1 keeps it. A reading
	// that dropped it would compare every other issue and say nothing about that rule.
	if zeroHours == 0 {
		t.Errorf("no issue of the %d compared has hours that are all zero, and the history holds %d",
			compared, len(fixture.NoMeasuredWork))
	}
	// The history holds parents that waited in `awaiting_children` past their working children's
	// spans, so both sides must compare nonzero `awaiting_children` hours on a real parent.
	if awaitingChildren == 0 {
		t.Errorf("no root of the %d compared issues holds nonzero awaiting_children phase hours on both sides", compared)
	}
	if differences > 0 {
		t.Errorf("%d facts differ between the reading and the computation", differences)
	}
	// The planned tree is what makes this comparison read the plan rule: were none of its hours
	// in another phase by the default steps' order, a reading that ignored every plan would pass.
	defaults, _ := computedFacts(t, database.Pool, fixture.Project, now, true)
	moved := 0
	for _, key := range fixture.Planned {
		if phaseHoursDiffer(computed[key].PhaseHours, defaults[key].PhaseHours) {
			moved++
		}
	}
	if moved == 0 {
		t.Errorf("no issue of the %d of the tree that ran a plan of other steps has hours the default steps would put in another phase, so the comparison read no plan rule",
			len(fixture.Planned))
	}
	t.Logf("the independent reading covered scope %s over window %s; the computation covered scope %s over window %s",
		reading.Scope, reading.Window, computedScope, computedWindow)
	t.Logf("%d issues compared, %d of them with hours that are all zero, %d roots with awaiting_children hours, %d differences",
		compared, zeroHours, awaitingChildren, differences)
	t.Logf("%d of the issues compared are of the tree that ran a plan of the steps implementer reviewer acceptance:owner, and %d of them have hours the default steps would put in another phase",
		plannedCompared, moved)
}

// computedFacts is the computation's facts of every issue of project at now, as the stored rows
// hold them (metrics.ProjectFactsClosed) grown to now, beside the population they were computed
// from. Under defaultSteps every issue's plans are taken out of the population first, so each
// issue reads the order of the default steps: what that order makes of the same history.
func computedFacts(t *testing.T, pool *store.Pool, project string, now time.Time, defaultSteps bool) (map[string]metrics.IssueFacts, *metrics.Population) {
	t.Helper()
	population, err := metrics.ReadProject(t.Context(), pool, project, now)
	if err != nil {
		t.Fatalf("read the population of %s: %v", project, err)
	}
	if defaultSteps {
		for _, issue := range population.Issues {
			issue.Plans = nil
		}
	}
	computed := map[string]metrics.IssueFacts{}
	for key, closed := range metrics.ProjectFactsClosed(population, nil) {
		computed[key] = closed.FactsAt(population.To)
	}
	return computed, population
}

// phaseHoursDiffer is whether two readings of one issue's hours by phase put more than a tenth of
// an hour of some phase apart, over every phase either names.
func phaseHoursDiffer(left, right map[string]float64) bool {
	for phase := range union(left, right) {
		if math.Abs(left[phase]-right[phase]) > hourTolerance {
			return true
		}
	}
	return false
}

// defaultSteps is the one task of the plan the daemon writes for a run whose owner gave none.
var defaultSteps = []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}

// planCase is one issue, alone in a project of its own and never observed, whose phases come from
// its event log alone and are ordered by the plans its runs ran.
type planCase struct {
	t       *testing.T
	pool    *store.Pool
	project string
	key     string
	start   time.Time
	seq     int
}

// hour is the moment hours after the case's issue was created.
func (c *planCase) hour(hours float64) time.Time {
	return c.start.Add(time.Duration(hours * float64(time.Hour)))
}

// exec runs one statement of the case's seed and fails the test where it fails.
func (c *planCase) exec(what, sql string, args ...any) {
	c.t.Helper()
	if _, err := c.pool.Exec(c.t.Context(), sql, args...); err != nil {
		c.t.Fatalf("%s of %s: %v", what, c.key, err)
	}
}

// event writes one event of the issue at its next sequence number.
func (c *planCase) event(kind, actor, payload string, hours float64) {
	c.t.Helper()
	c.seq++
	c.exec("write an event", `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ($1, $2, $3, $4::jsonb, $5::jsonb, false, $6)
	`, c.key, c.seq, kind, actor, payload, c.hour(hours))
}

// worked writes an event of the session titled for role working the issue: that role's first
// event on it, where it is the role's first.
func (c *planCase) worked(role string, hours float64) {
	c.t.Helper()
	c.event("comment.created", legiontest.SessionActor(role, c.key), `{}`, hours)
}

// status writes a status a person gave the issue.
func (c *planCase) status(status string, hours float64) {
	c.t.Helper()
	c.event("issue.updated", `{"kind":"user","id":"alice"}`, `{"status":"`+status+`"}`, hours)
}

// handoffAuthors is the role whose session writes each handoff word.
var handoffAuthors = map[string]string{"plan": "planner", "implement": "implementer", "test": "tester", "review": "reviewer"}

// handedOff writes one handoff of the issue under word, by the session of the role that hands it
// on.
func (c *planCase) handedOff(word string, hours float64) {
	c.t.Helper()
	c.exec("write a handoff", `
		insert into handoffs (issue_key, phase, version, body, author, created_at)
		values ($1, $2, 1, '{}', $3::jsonb, $4)
	`, c.key, word, legiontest.SessionActor(handoffAuthors[word], c.key), c.hour(hours))
}

// ran writes the plan of the issue's run of generation, made at hours, as the daemon writes it
// (workflowstore.PutPlan): one task for each list of steps, the source default where that is the
// default steps alone and the owner's document otherwise.
func (c *planCase) ran(generation int64, hours float64, tasks ...[]string) {
	c.t.Helper()
	write := workflowstore.PlanWrite{ID: uuid.NewString(), Issue: c.key, Generation: new(generation)}
	for index, steps := range tasks {
		write.Tasks = append(write.Tasks, workflowstore.PlanTask{ID: "task-" + string(rune('a'+index)), Steps: steps})
	}
	if len(tasks) == 1 && slices.Equal(tasks[0], defaultSteps) {
		write.Source = new(workflowstore.PlanSourceDefault)
	} else {
		write.Source = new(workflowstore.PlanSourceOwner)
		write.Document = workflowstore.PlanDocument{ArtifactID: uuid.NewString(), Version: 1}
	}
	daemon := model.Actor{Kind: "session", ID: "legion-daemon:" + c.project}
	if err := pgx.BeginFunc(c.t.Context(), c.pool, func(tx pgx.Tx) error {
		_, err := workflowstore.PutPlan(c.t.Context(), tx, write, daemon, c.hour(hours))
		return err
	}); err != nil {
		c.t.Fatalf("write the plan of run %d of %s: %v", generation, c.key, err)
	}
}

// TestTheIndependentReadingFollowsEachPlanRuleAsTheComputationDoes: before an issue's first
// observation its phases are ordered by its plan, and the reading states that rule in its own code
// and reads the plan rows with a statement of its own. One row for each case of the rule: each
// seeds one issue that is never observed, writes the plan of each of its runs as the daemon does
// (workflowstore.PutPlan) and its events, and compares the reading with metrics.ProjectFactsClosed
// to a tenth of an hour and each count exactly.
//
// Each row also computes the same history with every plan taken away, which reads the default
// steps' order, and says whether the plan moves hours into another phase. Where the rule falls
// back to the default steps (no plan, a plan of two tasks, a step that names no phase) nothing
// moves, though the plan's steps differ from the default ones; in every other row hours move, so a
// reading that kept the default order there differs from the computation.
func TestTheIndependentReadingFollowsEachPlanRuleAsTheComputationDoes(t *testing.T) {
	database := storetest.Open(t)
	start := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	now := start.Add(24 * time.Hour)
	// throughEveryStep is one log of the implementer, the reviewer and the tester, each with its
	// first event and its handoff: by the default steps' order it is implementing 0-2, testing
	// 2-3, reviewing 3-5, accepting 5-8 and reviewing 8-10, and by a plan of the steps
	// `implementer reviewer tester` it is implementing 0-2, reviewing 2-5, testing 5-8 and retro
	// 8-10.
	throughEveryStep := func(c *planCase) {
		c.worked("implementer", 0)
		c.handedOff("implement", 2)
		c.worked("reviewer", 3)
		c.handedOff("review", 5)
		c.worked("tester", 6)
		c.handedOff("test", 8)
	}
	reviewerFirst := []string{"implementer", "reviewer", "tester"}
	for index, row := range []struct {
		name   string
		closed float64
		write  func(c *planCase)
		moves  bool
	}{
		{"no plan reads the default steps", 10, throughEveryStep, false},
		{"a plan of two tasks reads the default steps", 10, func(c *planCase) {
			c.ran(1, 0, reviewerFirst, []string{"implementer"})
			throughEveryStep(c)
		}, false},
		{"a plan with a step that names no phase reads the default steps", 10, func(c *planCase) {
			c.ran(1, 0, []string{"implementer", "reviewer", "tester", "writer"})
			throughEveryStep(c)
		}, false},
		// The implementer's first event and the implement handoff fall before the plan was made,
		// so they read it as the first run's: the handoff starts reviewing.
		{"an event before every plan reads the first run's plan", 10, func(c *planCase) {
			c.ran(1, 2.5, reviewerFirst)
			throughEveryStep(c)
		}, true},
		// The first run ran the default steps and the second run's plan, made at hour 4, puts the
		// reviewer first: the marks before hour 4 read the default steps (implementing 0-2,
		// testing 2-3, reviewing 3-5) and the marks after it the second plan (testing 5-8, retro
		// 8-10).
		{"each moment reads the newest plan made at or before it, a second run's from its making on", 10, func(c *planCase) {
			c.ran(1, 0, defaultSteps)
			c.ran(2, 4, reviewerFirst)
			throughEveryStep(c)
		}, true},
		// Under `implementer reviewer` the implement handoff starts reviewing, and the review
		// handoff, the last step's, starts retro: by the default steps it is testing, then
		// accepting.
		{"a handoff starts the phase after the one its word ends on the plan", 7, func(c *planCase) {
			c.ran(1, 0, []string{"implementer", "reviewer"})
			c.worked("implementer", 0)
			c.handedOff("implement", 2)
			c.handedOff("review", 5)
		}, true},
		// Under `implementer reviewer tester` the tester's first event puts the issue past
		// reviewing, so the reviewer's first event after it starts nothing: testing runs 2-6. By
		// the default steps' order reviewing comes after testing, and starts at hour 4.
		{"a role's first event of a phase the plan puts behind the issue starts nothing", 6, func(c *planCase) {
			c.ran(1, 0, reviewerFirst)
			c.worked("implementer", 0)
			c.worked("tester", 2)
			c.worked("reviewer", 4)
		}, true},
		// A plan whose owner's acceptance is the one step after the implementer's holds no
		// reviewing, so `needs_review` names accepting: by the default steps it names reviewing.
		{"needs_review names the first of reviewing and accepting the plan holds", 4, func(c *planCase) {
			c.ran(1, 0, []string{"implementer", "acceptance:owner"})
			c.worked("implementer", 0)
			c.status("needs_review", 2)
		}, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			project := "PLAN" + string(rune('A'+index))
			c := &planCase{t: t, pool: database.Pool, project: project, key: project + "-1", start: start}
			c.exec("write the project", `insert into projects (key, name, default_assignee) values ($1, $1, 'alice')`, project)
			c.exec("write the issue", `
				insert into issues (key, project_key, number, title, status, rank, created_by, created_at, closed_at)
				values ($1, $2, 1, 'One issue of a plan case', 'done', '00000001', '{"kind":"system","id":"test"}', $3, $4)
			`, c.key, project, start, c.hour(row.closed))
			row.write(c)

			computed, _ := computedFacts(t, database.Pool, project, now, false)
			defaults, _ := computedFacts(t, database.Pool, project, now, true)
			reading, err := independent.Read(t.Context(), database.Pool, independent.Scope{Project: project}, independent.Window{To: now})
			if err != nil {
				t.Fatalf("read %s independently: %v", project, err)
			}
			mine, read := reading.Issues[c.key]
			theirs, computedIt := computed[c.key]
			if !read || !computedIt || len(reading.Issues) != 1 || len(computed) != 1 {
				t.Fatalf("the reading holds %d issues and the computation %d, want %s alone on both sides", len(reading.Issues), len(computed), c.key)
			}
			if differences := compareFacts(t, c.key, mine, theirs); differences > 0 {
				t.Errorf("%d facts of %s differ between the reading and the computation", differences, c.key)
			}
			if moved := phaseHoursDiffer(theirs.PhaseHours, defaults[c.key].PhaseHours); moved != row.moves {
				t.Errorf("the plan moves hours into another phase: %t, want %t; by the plan %v, by the default steps %v",
					moved, row.moves, theirs.PhaseHours, defaults[c.key].PhaseHours)
			}
			t.Logf("%s: by the reading %v, by the computation %v, by the default steps %v",
				c.key, mine.PhaseHours, theirs.PhaseHours, defaults[c.key].PhaseHours)
		})
	}
}

// TestIndependentReadAsOfSnapshotAndTitleTieBreak: Read stands what it takes from the log as of
// Window.To, in one snapshot. An issue created after To is absent; an issue closed after To is open
// and its work time runs to To; a retitle after To does not take a session's title away; a handoff
// after To makes no issue one that Legion worked. Where two titled events of a session share a
// moment the first inserted (the lower id) names it, as session_titles' trigger and fill name it.
// A non-zero Window.From and a zero Window.To are refused, not ignored.
func TestIndependentReadAsOfSnapshotAndTitleTieBreak(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	to := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	exec("seed project", `insert into projects (key, name, default_assignee) values ('SNAP', 'Snapshot', 'alice')`)
	// SNAP-1 opened before To and closed after it. SNAP-2 was created after To, and a Legion role
	// wrote on it before To, so only its own creation keeps it out as of To. SNAP-3 was created
	// before To and handed on only after it.
	exec("seed issues", `
		insert into issues (key, project_key, number, title, status, rank, created_by, created_at, closed_at) values
			('SNAP-1', 'SNAP', 1, 'Open at to', 'done', '00000001', '{"kind":"system","id":"test"}', $1, $2),
			('SNAP-2', 'SNAP', 2, 'Created after to', 'in_progress', '00000002', '{"kind":"system","id":"test"}', $3, null),
			('SNAP-3', 'SNAP', 3, 'Handed on after to', 'todo', '00000003', '{"kind":"system","id":"test"}', $4, null)
	`, to.Add(-4*time.Hour), to.Add(2*time.Hour), to.Add(time.Hour), to.Add(-2*time.Hour))
	// Two titled events of ses-tie at one moment: the first inserted must name the session, and
	// a retitle after To must not.
	exec("seed the first title of the moment", `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ('SNAP-1', 1, 'comment.created', '{"kind":"session","id":"ses-tie","origin":{"session_title":"Legion implementer · SNAP-1"}}', '{}', false, $1)
	`, to.Add(-3*time.Hour))
	exec("seed the second title of the moment", `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ('SNAP-1', 2, 'comment.created', '{"kind":"session","id":"ses-tie","origin":{"session_title":"Not a Legion title"}}', '{}', false, $1)
	`, to.Add(-3*time.Hour))
	exec("seed the retitle after to", `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ('SNAP-1', 3, 'comment.created', '{"kind":"session","id":"ses-tie","origin":{"session_title":"Renamed after to"}}', '{}', false, $1)
	`, to.Add(time.Hour))
	exec("seed the role event before to of the issue created after to", `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ('SNAP-2', 1, 'comment.created', '{"kind":"session","id":"ses-snap2","origin":{"session_title":"Legion implementer · SNAP-2"}}', '{}', false, $1)
	`, to.Add(-time.Hour))
	exec("seed the handoff after to", `
		insert into handoffs (issue_key, phase, version, body, author, created_at)
		values ('SNAP-3', 'implement', 1, '{}', '{"kind":"session","id":"ses-snap3"}', $1)
	`, to.Add(time.Hour))

	scope := independent.Scope{Project: "SNAP"}
	if _, err := independent.Read(ctx, database.Pool, scope, independent.Window{From: to.Add(-time.Hour), To: to}); err == nil {
		t.Fatal("independent.Read accepted a non-zero Window.From; want an error")
	}
	if _, err := independent.Read(ctx, database.Pool, scope, independent.Window{}); err == nil {
		t.Fatal("independent.Read accepted a zero Window.To; want an error")
	}

	reading, err := independent.Read(ctx, database.Pool, scope, independent.Window{To: to})
	if err != nil {
		t.Fatalf("independent.Read as of %s: %v", to.Format(time.RFC3339), err)
	}
	for _, absent := range []struct{ key, why string }{
		{"SNAP-2", "it was created after then"},
		{"SNAP-3", "no Legion work of it had been recorded by then"},
	} {
		if _, seen := reading.Issues[absent.key]; seen {
			t.Errorf("%s has facts as of %s though %s", absent.key, to.Format(time.RFC3339), absent.why)
		}
	}
	got, ok := reading.Issues["SNAP-1"]
	if !ok {
		t.Fatalf("SNAP-1 has no facts as of %s: the title of the moment's first event, or the retitle after it, was misread", to.Format(time.RFC3339))
	}
	if got.WorkHours != 3 || got.PhaseHours["implementing"] != 3 {
		t.Errorf("SNAP-1 as of %s = %+v, want 3 h of work, all of it implementing: it was open then, and its first worker event was 3 h before",
			to.Format(time.RFC3339), got)
	}

	later, err := independent.Read(ctx, database.Pool, scope, independent.Window{To: to.Add(3 * time.Hour)})
	if err != nil {
		t.Fatalf("independent.Read after to: %v", err)
	}
	for _, key := range []string{"SNAP-2", "SNAP-3"} {
		if _, seen := later.Issues[key]; !seen {
			t.Errorf("%s has no facts as of %s, after its Legion work was recorded", key, to.Add(3*time.Hour).Format(time.RFC3339))
		}
	}
}

// compareFacts reads each of one issue's five facts against the other side's, and answers how
// many of them differ: the two hour figures to a tenth of an hour, and every count, the root
// flag and the tree root exactly.
func compareFacts(t *testing.T, key string, mine independent.Facts, theirs metrics.IssueFacts) int {
	t.Helper()
	differences := 0
	if math.Abs(mine.WorkHours-theirs.WorkHours) > hourTolerance {
		t.Errorf("%s worked %g h by the reading and %g h by the computation",
			key, mine.WorkHours, theirs.WorkHours)
		differences++
	}
	if mine.ReviewRounds != theirs.ReviewRounds {
		t.Errorf("%s has %d review rounds by the reading and %d by the computation",
			key, mine.ReviewRounds, theirs.ReviewRounds)
		differences++
	}
	if mine.Root != theirs.Root {
		t.Errorf("%s is a root by the reading (%t) and by the computation (%t)", key, mine.Root, theirs.Root)
		differences++
	}
	if mine.Tree != theirs.Tree {
		t.Errorf("%s is in tree %s by the reading and in tree %s by the computation",
			key, mine.Tree, theirs.Tree)
		differences++
	}
	// A phase one side left out holds no hours of that phase, so the two are compared over
	// every phase either side names.
	for _, phase := range slices.Sorted(maps.Keys(union(mine.PhaseHours, theirs.PhaseHours))) {
		if math.Abs(mine.PhaseHours[phase]-theirs.PhaseHours[phase]) > hourTolerance {
			t.Errorf("%s spent %g h in %s by the reading and %g h by the computation",
				key, mine.PhaseHours[phase], phase, theirs.PhaseHours[phase])
			differences++
		}
	}
	return differences
}

// union is every key either side holds.
func union[Left, Right any](left map[string]Left, right map[string]Right) map[string]struct{} {
	keys := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		keys[key] = struct{}{}
	}
	for key := range right {
		keys[key] = struct{}{}
	}
	return keys
}

// TestIndependentReadLeavesOutTheEventsAndObservationsAfterTo: Read stands its events and its
// observations as of Window.To, as Window says. AFTR-1's only Legion work is a role event an hour
// after To, by a session that took its Legion title before To on another project's issue, so as of
// To no Legion work of AFTR-1 is recorded and it has no facts. AFTR-2's `tree` row, written an hour
// after To, names AFTR-1, so as of To AFTR-2 is still in a tree of its own. A read past both rows
// sees each of them, so the two answers at To are the bound's and not a rule's.
func TestIndependentReadLeavesOutTheEventsAndObservationsAfterTo(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	to := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	exec("seed projects", `insert into projects (key, name, default_assignee) values ('AFTR', 'After to', 'alice'), ('OTHR', 'Other', 'alice')`)
	exec("seed the other project's issue", `
		insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ('OTHR-1', 'OTHR', 1, 'Where the session took its title', 'in_progress', '00000001', '{"kind":"system","id":"test"}', $1)
	`, to.Add(-6*time.Hour))
	exec("seed the title AFTR-1's session took before to", `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ('OTHR-1', 1, 'comment.created', '{"kind":"session","id":"ses-aftr1","origin":{"session_title":"Legion implementer · AFTR-1"}}', '{}', false, $1)
	`, to.Add(-5*time.Hour))
	exec("seed issues", `
		insert into issues (key, project_key, number, title, status, rank, created_by, created_at) values
			('AFTR-1', 'AFTR', 1, 'Worked only after to', 'in_progress', '00000001', '{"kind":"system","id":"test"}', $1),
			('AFTR-2', 'AFTR', 2, 'Put in another tree after to', 'in_progress', '00000002', '{"kind":"system","id":"test"}', $1)
	`, to.Add(-6*time.Hour))
	exec("seed AFTR-2's role event before to", `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ('AFTR-2', 1, 'comment.created', '{"kind":"session","id":"ses-aftr2","origin":{"session_title":"Legion implementer · AFTR-2"}}', '{}', false, $1)
	`, to.Add(-3*time.Hour))
	exec("seed AFTR-1's role event after to", `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ('AFTR-1', 1, 'comment.created', '{"kind":"session","id":"ses-aftr1","origin":{"session_title":"Legion implementer · AFTR-1"}}', '{}', false, $1)
	`, to.Add(time.Hour))
	exec("seed AFTR-2's tree row after to", `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		values ('AFTR', 'AFTR-2', 'tree', '"AFTR-1"', $1)
	`, to.Add(time.Hour))

	scope := independent.Scope{Project: "AFTR"}
	reading, err := independent.Read(ctx, database.Pool, scope, independent.Window{To: to})
	if err != nil {
		t.Fatalf("independent.Read as of %s: %v", to.Format(time.RFC3339), err)
	}
	if facts, seen := reading.Issues["AFTR-1"]; seen {
		t.Errorf("AFTR-1 as of %s = %+v, want no facts: its only Legion work was recorded an hour after", to.Format(time.RFC3339), facts)
	}
	got, ok := reading.Issues["AFTR-2"]
	if !ok {
		t.Fatalf("AFTR-2 has no facts as of %s, though its first worker event was 3 h before", to.Format(time.RFC3339))
	}
	if got.Tree != "AFTR-2" {
		t.Errorf("AFTR-2 as of %s is in tree %s, want its own: the tree row that names AFTR-1 was written an hour after", to.Format(time.RFC3339), got.Tree)
	}

	later := to.Add(3 * time.Hour)
	after, err := independent.Read(ctx, database.Pool, scope, independent.Window{To: later})
	if err != nil {
		t.Fatalf("independent.Read as of %s: %v", later.Format(time.RFC3339), err)
	}
	if _, seen := after.Issues["AFTR-1"]; !seen {
		t.Errorf("AFTR-1 has no facts as of %s, after its Legion work was recorded", later.Format(time.RFC3339))
	}
	if tree := after.Issues["AFTR-2"].Tree; tree != "AFTR-1" {
		t.Errorf("AFTR-2 as of %s is in tree %s, want AFTR-1, which its tree row names by then", later.Format(time.RFC3339), tree)
	}
}
