package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// convergeGeneration is the generation every tree of this file runs at. One is enough: what the
// convergence reads is the entry a role's phases row names against the entry its claim was
// given, and both are counted within one generation.
const convergeGeneration = 1

// convergeRow is one shape the convergence is asked about: a running tree, a member of it
// standing in a worker phase and holding a slot, the role's phases row at seededEntry naming the
// work of it, and the claim as the one field this row is about leaves it.
//
// A row the convergence sends the work of that entry sets started. A row it leaves alone names a
// flip as well: the same fixture under keys of its own with that one field changed, which must be
// sent the work. Without it a row would read as quiet for a reason nobody stated — a tree the
// fixture never opened, a phase that names no role — and prove nothing about the field it names.
type convergeRow struct {
	name string
	// root and key are the tree's root and the member the convergence judges.
	root, key string
	// flipRoot and flipKey are the tree and member of the flipped copy, seeded beside this row
	// when flip is set.
	flipRoot, flipKey string
	// flip changes the one field this row is about into the shape that is sent the work. It is
	// required of a row that is not started and is never set on one that is.
	flip func(*convergeRow)
	// stands is the phase the member is in, which names the role that works it
	// (workflow.RoleFor).
	stands phase.Phase
	// lingers records the root as a park left it: phase done at a status that is not todo, with
	// a linger deadline ahead.
	lingers bool
	// readyPending gives the member the READY a closed design gate holds, which is what makes an
	// issue in merging one no worker of it has work in (record.AtWork).
	readyPending bool
	// unslotted leaves the member holding none of the cap's slots.
	unslotted bool
	// claimed records a claim of the role, in state and suspended by suspendedBy. A row without
	// one is a role the daemon holds no claim of at all, which was given nothing.
	claimed     bool
	state       supervise.ClaimState
	suspendedBy supervise.SuspendSource
	// given is the entry the claim was last queued a task of (record.RoleClaim.GivenEntry).
	given int64
	// holdsTask gives the claim a confirmed delivery of the member's own generation and phase:
	// the work of the entry it was given, which it is still holding.
	holdsTask bool
	// started is whether the convergence sends the member's role the work seededEntry names.
	started bool
	// why is what this row reads, and for a row that is left alone, the field its flip changes.
	why string
}

// convergeRows is the whole table: the shapes the convergence sends the work of their entry to,
// and the shapes it leaves alone, each of the second kind with the flip that must be sent it.
func convergeRows() []convergeRow {
	return []convergeRow{
		{
			name: "a live claim behind its entry", root: "LEGION-900", key: "LEGION-901", stands: phase.Testing,
			claimed: true, state: supervise.StateWorking, given: seededEntry - 1, started: true,
			why: "the claim runs and holds none of this entry's work: its turn outlived the phase its task was of, and nothing but this sends the work the entry names",
		},
		{
			name: "a role the daemon holds no claim of", root: "LEGION-902", key: "LEGION-903", stands: phase.Testing,
			started: true,
			why:     "a role with no claim row was given nothing, so it is behind every entry its phases row names, and the start creates the claim (workflow.StartOrOwe)",
		},
		{
			name: "a released claim behind its entry", root: "LEGION-904", key: "LEGION-905", stands: phase.Testing,
			claimed: true, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByReleased,
			given: seededEntry - 1, started: true,
			why: "no wake is armed for work the released claim was never given, so sending that entry's task is what brings its pod back",
		},
		{
			name: "a claim level with its entry", root: "LEGION-910", key: "LEGION-911", stands: phase.Testing,
			flipRoot: "LEGION-912", flipKey: "LEGION-913", flip: func(r *convergeRow) { r.given, r.started = seededEntry-1, true },
			claimed: true, state: supervise.StateWorking, given: seededEntry,
			why: "the claim was queued this entry's task already, and a second start would give the phase two tasks of one entry",
		},
		{
			name: "an idle claim already holding the work of its entry", root: "LEGION-914", key: "LEGION-915", stands: phase.Testing,
			flipRoot: "LEGION-916", flipKey: "LEGION-917", flip: func(r *convergeRow) { r.given, r.started = seededEntry-1, true },
			claimed: true, state: supervise.StateIdle, given: seededEntry, holdsTask: true,
			why: "the outbox held the start's task back because the claim already holds its phase's task and recorded the entry as given (supervise's EntryGiven), so the record says the work arrived and no second copy is sent",
		},
		{
			name: "a retired claim", root: "LEGION-920", key: "LEGION-921", stands: phase.Testing,
			flipRoot: "LEGION-922", flipKey: "LEGION-923",
			flip: func(r *convergeRow) {
				r.state, r.suspendedBy, r.started = supervise.StateSuspended, supervise.SuspendedByDaemon, true
			},
			claimed: true, state: supervise.StateRetired, given: seededEntry - 1,
			why: "this row changes the claim's state alone, to the one a stop leaves: starting the phase again would hand the operator back the worker they stopped",
		},
		{
			name: "a claim an operator suspended", root: "LEGION-924", key: "LEGION-925", stands: phase.Testing,
			flipRoot: "LEGION-926", flipKey: "LEGION-927",
			flip:    func(r *convergeRow) { r.suspendedBy, r.started = supervise.SuspendedByDaemon, true },
			claimed: true, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByOperator, given: seededEntry - 1,
			why: "this row changes suspended_by alone, from the daemon to the operator: a suspension that holds for the operator is woken by no start the daemon queues (supervise's RuleHolds), so only their own resume starts it",
		},
		{
			name: "a released claim level with its entry", root: "LEGION-930", key: "LEGION-931", stands: phase.Testing,
			flipRoot: "LEGION-932", flipKey: "LEGION-933", flip: func(r *convergeRow) { r.given, r.started = seededEntry-1, true },
			claimed: true, state: supervise.StateSuspended, suspendedBy: supervise.SuspendedByReleased, given: seededEntry,
			why: "this row changes the entry the claim was given alone, to its row's own: the released pod holds that entry's work and its wake is what brings it back",
		},
		{
			name: "a member of a tree that lingers", root: "LEGION-934", key: "LEGION-935", stands: phase.Testing,
			flipRoot: "LEGION-936", flipKey: "LEGION-937", flip: func(r *convergeRow) { r.lingers, r.started = false, true },
			lingers: true, claimed: true, state: supervise.StateWorking, given: seededEntry - 1,
			why: "this row changes the root's own record alone, to the one a park left: nothing of a tree that has left the workflow is started again until its re-admission",
		},
		{
			name: "an issue that is not at work", root: "LEGION-940", key: "LEGION-941", stands: phase.Merging,
			flipRoot: "LEGION-942", flipKey: "LEGION-943", flip: func(r *convergeRow) { r.readyPending, r.started = false, true },
			readyPending: true, claimed: true, state: supervise.StateWorking, given: seededEntry - 1,
			why: "this row changes the READY the member holds alone: a merger waiting on a person's approval of a closed design gate has no work to be sent (record.AtWork)",
		},
		{
			name: "a child's own sub-architect", root: "LEGION-944", key: "LEGION-945", stands: phase.Accepting,
			flipRoot: "LEGION-946", flipKey: "LEGION-947", flip: func(r *convergeRow) { r.stands, r.started = phase.Testing, true },
			claimed: true, state: supervise.StateWorking, given: seededEntry - 1,
			why: "this row changes the phase alone, to one a phase worker works: the member is no tree root, so its own sub-architect claim is the one claim the architect's reach leaves out (workflow.ArchitectAtWork), since no daemon start creates one",
		},
		{
			name: "an issue at work holding no slot", root: "LEGION-950", key: "LEGION-951", stands: phase.Testing,
			flipRoot: "LEGION-952", flipKey: "LEGION-953", flip: func(r *convergeRow) { r.unslotted, r.started = false, true },
			unslotted: true, claimed: true, state: supervise.StateWorking, given: seededEntry - 1,
			why: "this row changes the member's slot alone: the convergence walks the cap's slots, so an issue the daemon holds no slot for is given no delivery whatever its entry says",
		},
	}
}

// convergeCase is one seeded member and what the convergence owes it: its key, the role of its
// phase, whether that role is sent the work of seededEntry, and why.
type convergeCase struct {
	key     string
	role    claim.Role
	started bool
	why     string
}

// seedConvergeRows records every row of the table and the flipped copy of each row that is left
// alone, and answers the cases to read afterwards with the number of the cap's slots the fixture
// took. The cap is that number, so the pass promotes nothing and every start row a case reads is
// the convergence's own.
func seedConvergeRows(t *testing.T, pool *pgxpool.Pool, rows []convergeRow) ([]convergeCase, int) {
	t.Helper()
	var cases []convergeCase
	slots := 0
	seed := func(row convergeRow) {
		seedConvergeRow(t, pool, row)
		if !row.unslotted {
			slots++
		}
		cases = append(cases, convergeCase{key: row.key, role: workflow.RoleFor(row.stands), started: row.started, why: row.why})
	}
	for _, row := range rows {
		if row.started != (row.flip == nil) {
			t.Fatalf("the row %q is started = %t with a flip = %t, want a flip on the rows the convergence leaves alone and none on the rows it sends work to",
				row.name, row.started, row.flip != nil)
		}
		seed(row)
		if row.flip == nil {
			continue
		}
		flipped := row
		flipped.root, flipped.key, flipped.flip = row.flipRoot, row.flipKey, nil
		row.flip(&flipped)
		if !flipped.started {
			t.Fatalf("the flip of %q is not started, want the one field it changes to be what the convergence reads", row.name)
		}
		flipped.why = "the flip of " + row.name + ": " + row.why
		seed(flipped)
	}
	return cases, slots
}

// seedConvergeRow records one row: its tree, the member and its slot, the role's phases row at
// seededEntry naming the work of that entry, and the claim with the entry it was given and the
// task it holds. The phases row names the role's claim token whether or not a claim row exists,
// as a transition writes it.
func seedConvergeRow(t *testing.T, pool *pgxpool.Pool, row convergeRow) {
	t.Helper()
	ctx := context.Background()
	seedTreeAndMember(t, pool, row.root, row.key, row.stands, convergeGeneration, row.lingers)
	if row.readyPending {
		holdReady(t, pool, row.key)
	}
	if !row.unslotted {
		takeSlot(t, pool, row.key)
	}
	role := workflow.RoleFor(row.stands)
	if role == "" {
		t.Fatalf("the phase %s of %s names no role, and every row of this table stands in a phase one works", row.stands, row.key)
	}
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", testProject, err)
	}
	token, err := claim.NewToken(project, row.key, role)
	if err != nil {
		t.Fatalf("claim token of the %s of %s: %v", role, row.key, err)
	}
	if row.claimed {
		if seeded := putWorkerClaim(t, pool, row.root, row.key, role, convergeGeneration, row.state, row.suspendedBy); seeded != token {
			t.Fatalf("the claim of the %s of %s is %s, want the %s its phases row names", role, row.key, seeded, token)
		}
		if row.given != 0 {
			markGivenEntry(t, pool, token, row.given)
		}
		if row.holdsTask {
			putWorkerTask(t, pool, token, convergeGeneration, row.stands, true)
		}
	}
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(ctx, tx, record.PhaseRow{Issue: row.key, Role: role, Claim: token,
			Entry: seededEntry, EntryTask: seededEntryTask(row.key), EntryRaisedAt: fixedNow}); err != nil {
			t.Fatalf("record the %s's phases row of %s: %v", role, row.key, err)
		}
	})
}

// holdReady gives key the READY a closed design gate holds, which is what says a merger of it
// waits on a person rather than works (record.ReadyPending, record.AtWork).
func holdReady(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	ctx := context.Background()
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		stored, err := records.Issue(ctx, tx, key)
		if err != nil || stored == nil {
			t.Fatalf("read %s to hold its READY: %+v, %v", key, stored, err)
		}
		stored.ReadyPending = &record.ReadyPending{Step: record.DesignStepSpec, Version: 1}
		if err := records.PutIssue(ctx, tx, *stored); err != nil {
			t.Fatalf("hold the READY of %s: %v", key, err)
		}
	})
}

// convergeStart is one start row of a member: its id, which says whether a later run wrote the
// row again — a replacement is a row of its own (record.Store.EnqueueStart) — with the role it
// starts, the task it carries and the entry that task is of.
type convergeStart struct {
	id    int64
	role  claim.Role
	task  string
	entry int64
}

// convergeStarts is every supervise start row of issue, oldest first.
func convergeStarts(t *testing.T, pool *pgxpool.Pool, issue string) []convergeStart {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, coalesce(payload->>'role', ''), coalesce(payload->>'task', ''),
		coalesce((payload->>'entry')::bigint, 0) from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, issue)
	if err != nil {
		t.Fatalf("list the start rows of %s: %v", issue, err)
	}
	defer rows.Close()
	var starts []convergeStart
	for rows.Next() {
		var start convergeStart
		var role string
		if err := rows.Scan(&start.id, &role, &start.task, &start.entry); err != nil {
			t.Fatalf("read a start row of %s: %v", issue, err)
		}
		start.role = claim.Role(role)
		starts = append(starts, start)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the start rows of %s: %v", issue, err)
	}
	return starts
}

// assertConverged reads every case against the start rows its member holds: one row carrying the
// work of its phase entry, at that entry, for a case the convergence sends the work to, and no
// row at all for one it leaves alone.
func assertConverged(t *testing.T, pool *pgxpool.Pool, cases []convergeCase) {
	t.Helper()
	for _, c := range cases {
		starts := convergeStarts(t, pool, c.key)
		if !c.started {
			if len(starts) != 0 {
				t.Errorf("the start rows of %s = %+v, want none: %s", c.key, starts, c.why)
			}
			continue
		}
		if len(starts) != 1 {
			t.Errorf("the start rows of %s = %+v, want the one row the start is written as (workflow.StartOrOwe): %s", c.key, starts, c.why)
			continue
		}
		want := convergeStart{id: starts[0].id, role: c.role, task: seededEntryTask(c.key), entry: seededEntry}
		if starts[0] != want {
			t.Errorf("the start of %s = %+v, want %+v: the convergence sends the role the work its phases row names, at that entry, so no completion in flight is refused for an entry nothing delivered. %s",
				c.key, starts[0], want, c.why)
		}
	}
}

// assertNoSecondStart reads that a second run of the convergence wrote nothing: every member
// holds the row ids it held before, so no row was replaced and none was added. A replacement
// carries the earlier row's creation time but an id of its own (record.Store.EnqueueStart), so
// the ids are what tell one write from two where the count cannot.
func assertNoSecondStart(t *testing.T, pool *pgxpool.Pool, cases []convergeCase, before map[string][]convergeStart) {
	t.Helper()
	for _, c := range cases {
		got := convergeStarts(t, pool, c.key)
		want := before[c.key]
		if len(got) != len(want) {
			t.Errorf("the start rows of %s after the second run = %+v, want the %+v the first left: %s", c.key, got, want, c.why)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("the start row of %s after the second run = %+v, want the %+v the first wrote: the entry the row carries is the entry its phases row names, so the run that reads it again writes nothing. %s",
					c.key, got[i], want[i], c.why)
			}
		}
	}
}

// startsNow is the start rows every case's member holds, by key.
func startsNow(t *testing.T, pool *pgxpool.Pool, cases []convergeCase) map[string][]convergeStart {
	t.Helper()
	held := make(map[string][]convergeStart, len(cases))
	for _, c := range cases {
		held[c.key] = convergeStarts(t, pool, c.key)
	}
	return held
}

// applySweep applies one entry sweep, the fact the daemon's own ticker applies when nothing else
// of the project does (intake.EntrySweep, the daemon's tickEntrySweep).
func applySweep(t *testing.T, pool *pgxpool.Pool, admission *Admission, eventID string) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "entry-sweep", eventID, intake.EntrySweep{}, engineStub{}, admission); err != nil {
		t.Fatalf("ApplyFact %s: %v", eventID, err)
	}
}

// The admission pass sends every role of an issue at work whose claim is behind the entry its
// phases row names the work of that entry, and leaves every other role alone. The difference it
// closes is the record's own — the entry the transition raised against the entry the outbox
// recorded as given — so the state of the claim is no part of the answer: a live claim, an idle
// one and a role with no claim row at all are all behind their entry while they hold none of its
// work, and all three are sent it.
//
// What is left alone is each of the shapes a start may not reach: a claim an operator stopped or
// suspended, a released pod holding its own entry's work, a member of a tree that has left the
// workflow, an issue no worker of which has work where it stands, a child's own sub-architect,
// and an issue holding none of the cap's slots. Each of those rows is seeded twice — once as it
// is, and once with the one field it names changed — so none of them is quiet for a reason the
// row never claimed. What the rule does reach of a root's own architect is a table of its own
// (architect_sweep_test.go).
//
// The second pass is the pass that must write nothing: the given entry only moves when the outbox
// queues the delivery, and an admission pass runs on every fact of the project, so a pass that
// read the start it had just written as missing would write one for every fact until the lane
// ran.
func TestAnAdmissionPassConvergesEveryRoleBehindTheEntryItsPhasesRowNames(t *testing.T) {
	pool := migratedPool(t)
	cases, slots := seedConvergeRows(t, pool, convergeRows())
	admission := quiet(t, slots)

	tick(t, pool, admission, "converge-pass-1")
	assertConverged(t, pool, cases)

	written := startsNow(t, pool, cases)
	tick(t, pool, admission, "converge-pass-2")
	assertNoSecondStart(t, pool, cases, written)
}

// The sweep is the same convergence run with no fact of the project arriving (intake.EntrySweep,
// which admission answers with convergeEntries): it reads the project's issues, its slots and its
// line facts and sends the same roles the same work, so a quiet project closes the same
// differences a busy one does. Running it twice reads the term that keeps one entry to one start
// row: the second sweep finds the first sweep's row carrying that entry and writes nothing.
//
// It is the same table as the pass's, under a pool of its own, because the two must not be able
// to drift: a shape the pass sends work to and the sweep does not would mean a worker waiting on
// the project's next fact for work the record already names.
func TestTheEntrySweepConvergesEveryRoleBehindItsEntryWithNoFactArriving(t *testing.T) {
	pool := migratedPool(t)
	cases, slots := seedConvergeRows(t, pool, convergeRows())
	admission := quiet(t, slots)

	applySweep(t, pool, admission, "converge-sweep-1")
	assertConverged(t, pool, cases)

	written := startsNow(t, pool, cases)
	applySweep(t, pool, admission, "converge-sweep-2")
	assertNoSecondStart(t, pool, cases, written)
}
