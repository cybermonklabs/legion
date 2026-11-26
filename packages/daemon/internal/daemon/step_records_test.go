// The whole daemon writes the record of each step of an issue onto the real Dispatch of this
// package's tests (startScratchDispatchOn), and a reader reads the records of the issue back with
// GET /api/v1/issues/{key}/steps, the read `dispatch steps` and the tab Steps make.
//
// Each test boots the whole daemon (startDaemonWithin) over the package's broker, the real store,
// the real outbox and the real Dispatch client, and walks an issue, from planning to done or
// through the part of the walk the records it reads need (planning alone, or a child of the walk's
// issue from its review to its retro), through the transports production has: each role's agent
// over the worker stream and the daemon's HTTP routes, its handoff record written to Dispatch under
// its own session, a person's review and the merge as GitHub's envelopes on the stream, and the
// architect's sign-off through its own route.
// What is stood in for is outside the records: GitHub's REST API (stepRecordsGitHub), and the
// panes, which the fake runtime launches and the test plays.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// stepRecordsHead is the head of the issue's pull request, which every completion and review
	// of the walk names.
	stepRecordsHead = "5e7f00d5a1b2c3d4e5f60718293a4b5c6d7e8f90"
	// stepRecordsPerson is the account with write access whose reviews decide each round.
	stepRecordsPerson = "a-person"
	// stepRecordsModel is the model an agent of the walk answers with when its launch's spec names
	// none, as every launch of Legion's own roles does, until its test gives the agent another
	// (stepClaimant.model), as Oh My Pi's message_end names it: provider and model.
	stepRecordsModel = "stand-in/model"
	// stepRecordsWait bounds each wait of the walk on the daemon, which a loaded machine slows.
	stepRecordsWait = 2 * time.Minute
)

// stepRecordsTen is the walk's ten records, as a walk to done with one failed review leaves them:
// each of the task default; the failed review returns the issue to step 2, implementing.
var stepRecordsTen = []stepWant{
	{kind: "step", step: 1, attempt: 1, role: "planner", typ: "planning", result: "passed"},
	{kind: "step", step: 2, attempt: 1, role: "implementer", typ: "implementation", result: "passed"},
	{kind: "step", step: 2, attempt: 2, role: "implementer", typ: "implementation", result: "passed"},
	{kind: "step", step: 3, attempt: 1, role: "tester", typ: "testing", result: "passed"},
	{kind: "step", step: 3, attempt: 2, role: "tester", typ: "testing", result: "passed"},
	{kind: "step", step: 4, attempt: 1, role: "reviewer", typ: "review", result: "failed", returnedTo: new(2)},
	{kind: "step", step: 4, attempt: 2, role: "reviewer", typ: "review", result: "passed"},
	{kind: "step", step: 5, attempt: 1, role: "acceptance", typ: "review", result: "passed"},
	{kind: "retro", step: 0, attempt: 1, role: "implementer", result: "passed"},
	{kind: "merge", step: 0, attempt: 1, role: "merger", result: "passed"},
}

// The read of an issue's steps: one issue walks to done with one failed review, and the read of the
// issue lists its ten steps, each with its role, type, agent, attempt, start, end and result, and
// each record of kind step with the model that answered it. The control deletes the failed review's
// record from Dispatch's store, its node and its tuples, and the same assertion must then name the
// missing record.
func TestTheReadOfAnIssueListsEachStepThatRan(t *testing.T) {
	t.Parallel()
	r := newStepRecordsRig(t, 1)
	r.plan()
	r.implement()
	r.test()
	r.review("CHANGES_REQUESTED", phase.Implementing)
	r.implement()
	r.test()
	r.review("APPROVED", phase.Accepting)
	r.accept()
	r.retro()
	r.merge()
	var read stepRecordsRead
	r.waitSteps("the ten records of the walk", func(got stepRecordsRead) error { read = got; return stepsHold(got, stepRecordsTen) })
	failed := read.find("step", 4, 1)

	t.Run("control", func(t *testing.T) {
		r.deleteNode(failed.ID)
		read, err := r.readSteps()
		if err != nil {
			t.Fatalf("read the issue's steps after the delete: %v", err)
		}
		err = stepsHold(read, stepRecordsTen)
		if err == nil || !strings.Contains(err.Error(), "step 4 attempt 1") {
			t.Fatalf("the assertion over a read without the failed review's record = %v, want an error naming step 4 attempt 1", err)
		}
	})
}

// Dispatch does not answer while step records are due, and no phase waits for it.
// The handoff records of planning, implementing and testing are Dispatch's own, which the daemon
// reads at each of those completions, so Dispatch stops once the issue is in reviewing, with the
// record of every step from there on still to be written; the review, the acceptance, the retro
// and the merge then walk the issue to done, and once Dispatch is back on the same database the
// issue's seven records reach it, each ended. The control holds the outage real: while Dispatch is
// stopped, the outbox holds the issue's step records unfinished and the read finds no server.
func TestAPhaseNeverWaitsForDispatch(t *testing.T) {
	t.Parallel()
	r := newStepRecordsRig(t, 1)
	r.plan()
	r.implement()
	r.test()
	r.dispatch.stop()
	r.review("APPROVED", phase.Accepting)
	r.accept()
	r.retro()
	r.merge()

	t.Run("control", func(t *testing.T) {
		if unfinished := r.unfinishedRows(record.OutboxKindStepRecord); unfinished == 0 {
			t.Fatal("the outbox holds no unfinished step record of the issue while Dispatch is stopped: either Dispatch took each record (no outage) or the daemon wrote none")
		}
		if _, err := r.readSteps(); err == nil {
			t.Fatal("GET steps answered while Dispatch is stopped, so the walk rode through no outage")
		}
	})

	r.dispatch.start()
	want := stepRecordsSeven
	// A row Dispatch refused while it was stopped waits out its backoff, at most
	// record.RetryBackoffMost, before it runs again, and the sweep finds a lease that lapsed within
	// one more lease.
	within := record.RetryBackoffMost + outboxLease
	started := time.Now()
	if !testwait.Holds(within, time.Second, func() bool { got, err := r.readSteps(); return err == nil && stepsHold(got, want) == nil }) {
		got, err := r.readSteps()
		t.Fatalf("within %s of Dispatch's start the issue's steps read %+v (%v), and the assertion says: %v", within, got, err, stepsHold(got, want))
	}
	t.Logf("the seven records reached Dispatch %s after its start", time.Since(started).Round(time.Second))
}

// The daemon restarts in the middle of a step. It stops while the issue is in
// testing, with the tester at work, and boots again on the same store and the same Dispatch; each
// pane outlives the daemon, as a tmux pane does, and its shim connects to the new daemon with the
// boot token it holds. The tester answers again and completes, the walk ends at done, and the read
// holds exactly one record of testing for that run, ended, under the id its start wrote.
func TestTheDaemonRestartsInTheMiddleOfAStep(t *testing.T) {
	t.Parallel()
	r := newStepRecordsRig(t, 1)
	r.plan()
	r.implement()
	tester := r.worker(claim.RoleTester)
	tester.take()
	var started stepRecordsRead
	r.waitSteps("the start of testing", func(got stepRecordsRead) error {
		started = got
		if found := got.find("step", 3, 1); found.ID == "" || found.StartedAt == nil {
			return errors.New("no started record of step 3 attempt 1")
		}
		return nil
	})
	r.restart()
	// A restarted daemon knows no model until the agent answers again (supervise.StreamAnswered).
	tester.ownTurn()
	r.finish(tester, "test", testCompletion, phase.Reviewing)
	r.review("APPROVED", phase.Accepting)
	r.accept()
	r.retro()
	r.merge()
	var read stepRecordsRead
	r.waitSteps("the seven records of the walk across the restart", func(got stepRecordsRead) error { read = got; return stepsHold(got, stepRecordsSeven) })
	if before, after := started.find("step", 3, 1), read.find("step", 3, 1); before.ID != after.ID {
		t.Fatalf("the testing record read after the restart is %s, want the one its start wrote before it, %s", after.ID, before.ID)
	}
}

// stepRecordsSeven is the seven records of a walk whose first review approves.
var stepRecordsSeven = []stepWant{
	{kind: "step", step: 1, attempt: 1, role: "planner", typ: "planning", result: "passed"},
	{kind: "step", step: 2, attempt: 1, role: "implementer", typ: "implementation", result: "passed"},
	{kind: "step", step: 3, attempt: 1, role: "tester", typ: "testing", result: "passed"},
	{kind: "step", step: 4, attempt: 1, role: "reviewer", typ: "review", result: "passed"},
	{kind: "step", step: 5, attempt: 1, role: "acceptance", typ: "review", result: "passed"},
	{kind: "retro", step: 0, attempt: 1, role: "implementer", result: "passed"},
	{kind: "merge", step: 0, attempt: 1, role: "merger", result: "passed"},
}

// A rollback of this release and its roll forward, through the whole daemon and the store it
// left. The walk stops with Dispatch down and the review sent back, so the implementer's second
// run is open and the outbox holds the issue's step records and its other writes unsent, beside a
// count of starts of each step that ran. With the daemon stopped, the operator's way back runs as
// the roll forward's header carries it (internal/textcheck holds it to docs/kubernetes.md), and
// every outbox row and every count stands after it as it stood before. The earlier release came
// before the owner's plan too, so the rollback also takes the way back of the plan's migration
// (rollBackOwnersPlans), which drops the issue's plan rows: kept, the place of its task would stand
// at implementing while the earlier release moves the issue to testing. The earlier release's move
// is then written as the rows this release reads of it: the issue in testing, and the tester's
// entry raised again with its last handoff cleared; none of that release's deliveries stands in
// the store. The newer image boots, its Migrate applying the roll forward and the plan's migration
// again, which leaves the issue with no plan rows, so it walks today's line. The walk ends at done,
// and the read holds the implementer's run that the earlier release ended stopped at the moment
// the way back kept, under the id its start wrote, the tester's run that release started as a
// record of its own at the next count, started when its entry was raised, and the tester's run
// this release counted under its own id.
func TestARollbackAndItsRollForwardKeepEveryRecordOfTheWalk(t *testing.T) {
	t.Parallel()
	r := newStepRecordsRig(t, 1)
	r.plan()
	r.implement()
	r.test()
	r.dispatch.stop()
	r.review("CHANGES_REQUESTED", phase.Implementing)
	// The implementer's second run: its pane holds the task the review sent back, and no turn of it
	// reaches this release.
	implementer := r.worker(claim.RoleImplementer)
	implementer.receive()
	r.d.stop()

	outbox, starts := r.storeRows(stepRecordsOutboxRows), r.storeRows(stepRecordsStartRows)
	var stepRows, otherRows int
	if err := r.st.Pool().QueryRow(context.Background(), `select count(*) filter (where kind = $1), count(*) filter (where kind <> $1) from outbox`,
		string(record.OutboxKindStepRecord)).Scan(&stepRows, &otherRows); err != nil {
		t.Fatalf("count the outbox's rows: %v", err)
	}
	if stepRows == 0 || otherRows == 0 || !slices.ContainsFunc(starts, func(row string) bool {
		return strings.Contains(row, `"place":2`) && strings.Contains(row, `"starts":2`) && strings.Contains(row, `"run":"open"`)
	}) {
		t.Fatalf("at the rollback the outbox holds %d step records and %d other rows and step_starts holds %q; want both kinds of row unsent and implementing's second start open, or the way back runs over nothing it could lose",
			stepRows, otherRows, starts)
	}
	var tasks int
	if err := r.st.Pool().QueryRow(context.Background(), `select count(*) from plan_tasks where issue = $1`, r.key).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("at the rollback %s holds %d plan tasks (%v), want its one task default: a way back that drops nothing proves nothing", r.key, tasks, err)
	}
	r.rollBackStepRecords()
	// The way back keeps the moment of the rollback beside the issue's witness, and the roll forward
	// writes it as the end of a run the earlier release ended (step_starts_rollback.taken_at).
	var rolledBackAt time.Time
	if err := r.st.Pool().QueryRow(context.Background(), `select taken_at from step_starts_rollback where issue = $1`, r.key).Scan(&rolledBackAt); err != nil {
		t.Fatalf("read the moment the way back kept for %s: %v", r.key, err)
	}
	if after := r.storeRows(stepRecordsOutboxRows); !slices.Equal(after, outbox) {
		t.Fatalf("the way back changed the outbox:\nbefore %q\nafter  %q", outbox, after)
	}
	if after := r.storeRows(stepRecordsStartRows); !slices.Equal(after, starts) {
		t.Fatalf("the way back changed step_starts:\nbefore %q\nafter  %q", starts, after)
	}

	// The earlier release: the implementer works that task and completes, which ends its turn,
	// moves the issue to testing, clears the tester's last handoff and raises the tester's entry,
	// whose run it starts and counts nowhere.
	testerRow := r.phaseOf(claim.RoleTester)
	if testerRow.CompletedAt.IsZero() || testerRow.HandoffRecord == "" {
		t.Fatalf("at the rollback the tester's row is %+v, want its first round completed: a clear of nothing proves nothing", testerRow)
	}
	implementer.endTurnUnseen()
	earlierStart := time.Now().UTC().Truncate(time.Millisecond)
	r.exec(`update issues set phase = 'testing' where key = $1`, r.key)
	r.exec(`update phases set handoff_commit = '', handoff_record = '', handoff_version = 0, verdict = '', summary = '', decision = null,
		completed_at = $3, entry = entry + 1, entry_raised_at = $2 where issue = $1 and role = 'tester'`, r.key, earlierStart, time.Time{})
	// A transition into testing clears the handoff the tester reported for its last round
	// (workflow's clearHandoff, which empties what its phaseHandoff lists) before it raises the
	// tester's entry, so the new entry reads as not completed; the claim, the record the tester
	// last named and the rounds stand.
	if got := r.phaseOf(claim.RoleTester); got.HandoffCommit != "" || got.HandoffRecord != "" || got.HandoffVersion != 0 ||
		got.Verdict != "" || got.Summary != "" || got.Decision != nil || !got.CompletedAt.IsZero() ||
		got.Claim != testerRow.Claim || got.LastHandoff != testerRow.LastHandoff || got.Rounds != testerRow.Rounds || got.Pass != testerRow.Pass ||
		got.Entry != testerRow.Entry+1 || !got.EntryRaisedAt.Equal(earlierStart) {
		t.Fatalf("the tester's row the earlier release moved into testing is %+v, want its last round's handoff cleared and its entry raised at %s, the rest of %+v kept",
			got, earlierStart, testerRow)
	}
	// The way back of the plan's migration drops the columns this release's read of a phases row
	// names, so it runs once the reads above are done; the earlier release's writes name neither
	// those columns nor the plan's tables, so the store is the same as had it run first.
	r.rollBackOwnersPlans()
	var left *string
	if err := r.st.Pool().QueryRow(context.Background(), `select to_regclass('plan_tasks')::text`).Scan(&left); err != nil || left != nil {
		t.Fatalf("after the way back of the owner's plan the store holds the table %v (%v), want none", left, err)
	}
	// The earlier release keys pull_requests by the issue alone, so the walk took the way back of
	// the pull requests by task, which reads plan_tasks, before the plan's.
	var key string
	if err := r.st.Pool().QueryRow(context.Background(), `select string_agg(a.attname::text, ',' order by k.ord) from pg_constraint c
		cross join lateral unnest(c.conkey) with ordinality k(attnum, ord) join pg_attribute a on a.attrelid = c.conrelid and a.attnum = k.attnum
		where c.conrelid = 'pull_requests'::regclass and c.contype = 'p'`).Scan(&key); err != nil || key != "issue" {
		t.Fatalf("after the rollback pull_requests is keyed by %q (%v), want issue: the way back of the pull requests by task did not run", key, err)
	}
	r.dispatch.start()
	r.reboot()
	// No delivery of the earlier release stands in the store, so the tester's claim still serves
	// the entry it confirmed before the rollback, and the boot's admission sends it the task of the
	// entry its row names (workflow's SendOwedEntry), which the tester works. That send starts no
	// run: only a raise of the entry does (workflow's enter).
	tester := r.worker(claim.RoleTester)
	tester.take()
	r.finish(tester, "test", testCompletion, phase.Reviewing)
	r.review("APPROVED", phase.Accepting)
	r.accept()
	r.retro()
	r.merge()

	// The roll forward wrote the end of the implementer's run open at the rollback: no agent was
	// read for that run, so its record names the implementer's claim and no session and no model.
	want := slices.Clone(stepRecordsTen)
	want[2].result, want[2].owner = "stopped", &stepOwner{agent: string(r.token(claim.RoleImplementer))}
	var read stepRecordsRead
	r.waitSteps("the walk's records across the rollback", func(got stepRecordsRead) error { read = got; return stepsHold(got, want) })
	id := func(step int, count int64) string {
		return workflow.StepID(r.key, 1, record.StepPlace{Task: "default", Kind: "step", Place: step}, count)
	}
	if ended := read.find("step", 2, 2); ended.ID != id(2, 2) || ended.EndedAt == nil || !ended.EndedAt.Equal(rolledBackAt) {
		t.Fatalf("the implementer's run open at the rollback reads %+v (ended %v), want the record %s its start wrote, the implementer's, stopped at the moment the way back kept, %s",
			ended, ended.EndedAt, id(2, 2), rolledBackAt)
	}
	if counted := read.find("step", 3, 1); counted.ID != id(3, 1) {
		t.Fatalf("the tester's run this release counted reads %+v, want its own record %s", counted, id(3, 1))
	}
	if earlier := read.find("step", 3, 2); earlier.ID != id(3, 2) || earlier.Agent != string(r.token(claim.RoleTester)) ||
		!earlier.StartedAt.Equal(earlierStart) {
		t.Fatalf("the tester's run the earlier release started reads %+v (started %v), want the record %s of the next count, the tester's, started at %s",
			earlier, earlier.StartedAt, id(3, 2), earlierStart)
	}
}

const (
	// stepRecordsOutboxRows is every row of the daemon's outbox, whole, in its order.
	stepRecordsOutboxRows = `select row_to_json(o)::text from outbox o order by id`
	// stepRecordsStartRows is every row of step_starts, whole, in its key's order.
	stepRecordsStartRows = `select row_to_json(s)::text from step_starts s order by issue, task, kind, place`
)

// storeRows is each row query answers on the daemon's store, as text.
func (r *stepRecordsRig) storeRows(query string) []string {
	r.t.Helper()
	rows, err := r.st.Pool().Query(context.Background(), query)
	if err != nil {
		r.t.Fatalf("%s: %v", query, err)
	}
	all, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		r.t.Fatalf("%s: %v", query, err)
	}
	return all
}

// exec runs one statement with args on the daemon's store, as the earlier release's writes land.
func (r *stepRecordsRig) exec(statement string, args ...any) {
	r.t.Helper()
	if _, err := r.st.Pool().Exec(context.Background(), statement, args...); err != nil {
		r.t.Fatalf("%s: %v", statement, err)
	}
}

// phaseOf is the phases row of role on the walk's issue, as the store holds it.
func (r *stepRecordsRig) phaseOf(role claim.Role) record.PhaseRow {
	r.t.Helper()
	ctx := context.Background()
	var rows []record.PhaseRow
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = r.records.Phases(ctx, tx, r.key)
		return err
	}); err != nil {
		r.t.Fatalf("read the phases of %s: %v", r.key, err)
	}
	for _, row := range rows {
		if row.Role == role {
			return row
		}
	}
	r.t.Fatalf("%s has no phases row of %s", r.key, role)
	return record.PhaseRow{}
}

// rollBackStepRecords runs the way back the roll forward's migration carries in its header
// (migrations.WayBack), as an operator runs it with the daemon stopped.
func (r *stepRecordsRig) rollBackStepRecords() {
	r.t.Helper()
	block, err := migrations.WayBack(migrations.StepRecordsRollForward)
	if err != nil {
		r.t.Fatalf("read the way back of the roll forward: %v", err)
	}
	r.exec(block)
}

// rollBackOwnersPlans runs the way back of the migration that adds the plan rows
// (inFlightPlanMigration) as an operator runs it with the daemon stopped: the statements its
// header gives from `begin;` to `commit;` (headerWayBack), with <PROJECT> the run's project. Each
// way back here is read so, and not by migrations.WayBack, which reads the last paragraph of a
// header alone: the header of the name of each step's agent gives its way back before prose of its
// own. A rollback of the release whose owner's plan picks the steps to any release before it takes
// that way back too (docs/kubernetes.md, "Rolling back the release whose owner's plan picks the
// steps"), which drops the plan's tables and columns, so the roll forward creates them again
// empty. Before it, newest first, it takes the way back of each later migration that builds on the
// plan's tables (buildsOnPlanTables), as a rollback past those releases does: the plan's way back
// drops the tables they altered or read and leaves their version rows standing, so a roll forward
// would apply none of them again and the plan's tables would come back without their columns. The
// name of each step's agent (planStepAgentMigration) is one of them: it adds a column to
// plan_steps, and left recorded, its version would keep the roll forward from adding the column to
// the plan_steps it creates again, and every later write of a plan would fail. The givers of a plan
// version (the migration whose name ends _plan_earlier_givers.up.sql) is another: it adds a column
// to issue_plans, and the guide allows its way back before or after the plan's. The pull requests
// by task (20261122122826) is a third: it alters no plan table, but its way back reads plan_tasks
// to keep each issue's merge row, so it runs while that table stands.
func (r *stepRecordsRig) rollBackOwnersPlans() {
	r.t.Helper()
	all, err := migrations.All()
	if err != nil {
		r.t.Fatalf("read the embedded migrations: %v", err)
	}
	plan := slices.IndexFunc(all, func(migration migrations.Migration) bool {
		return strings.HasSuffix(migration.Name, inFlightPlanMigration)
	})
	if plan < 0 {
		r.t.Fatalf("no embedded migration ends %s", inFlightPlanMigration)
	}
	for later := len(all) - 1; later > plan; later-- {
		if buildsOnPlanTables(all[later].SQL) {
			r.exec(r.headerWayBack(all[later]))
		}
	}
	r.exec(r.headerWayBack(all[plan]))
}

// headerTransaction is the way back the header of the embedded migration whose name ends with
// suffix gives, from its `begin;` to its `commit;`, each line with the comment marker and its
// indent taken off. It fails the test where no embedded migration ends with suffix, or where its
// header holds no such block.
func (r *stepRecordsRig) headerTransaction(suffix string) string {
	r.t.Helper()
	all, err := migrations.All()
	if err != nil {
		r.t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if strings.HasSuffix(migration.Name, suffix) {
			return r.headerBlock(migration)
		}
	}
	r.t.Fatalf("no embedded migration ends %s", suffix)
	return ""
}

// headerWayBack is the way back migration's header gives from `begin;` to `commit;`, with
// <PROJECT> the run's project.
func (r *stepRecordsRig) headerWayBack(migration migrations.Migration) string {
	r.t.Helper()
	return strings.ReplaceAll(r.headerBlock(migration), "<PROJECT>", r.cfg.Project)
}

// headerBlock is the way back migration's header gives, from its `begin;` to its `commit;`, each
// line with the comment marker and its indent taken off. It fails the test where the header holds
// no such block.
func (r *stepRecordsRig) headerBlock(migration migrations.Migration) string {
	r.t.Helper()
	var block []string
	for _, line := range strings.Split(migration.SQL, "\n") {
		statement, comment := strings.CutPrefix(line, "--")
		if statement = strings.TrimSpace(statement); !comment || (len(block) == 0 && statement != "begin;") {
			continue
		}
		if block = append(block, statement); statement == "commit;" {
			return strings.Join(block, "\n")
		}
	}
	r.t.Fatalf("%s carries no way back from begin; to commit; in its header, read %d lines of it", migration.Name, len(block))
	return ""
}

// buildsOnPlanTables says whether sql, a migration, names one of the plan's three tables in a
// statement of its own or in the way back its header gives (from `-- begin;` to `-- commit;`), and
// not in the rest of its comments: it alters one or reads one, or its way back reads one, which
// must then run while the plan's tables stand.
func buildsOnPlanTables(sql string) bool {
	wayBack := false
	for _, line := range strings.Split(sql, "\n") {
		statement := strings.ToLower(strings.TrimSpace(line))
		if body, comment := strings.CutPrefix(statement, "--"); comment {
			switch body = strings.TrimSpace(body); {
			case body == "begin;", body == "commit;":
				wayBack = body == "begin;"
				continue
			case !wayBack:
				continue
			}
			statement = body
		}
		for _, word := range strings.FieldsFunc(statement, func(r rune) bool { return !unicode.IsLetter(r) && r != '_' }) {
			if slices.Contains([]string{"issue_plans", "plan_tasks", "plan_steps"}, word) {
				return true
			}
		}
	}
	return false
}

// planStepAgentMigration is the migration that adds plan_steps.agent, the name of each step's
// agent.
const planStepAgentMigration = "_plan_step_agent.up.sql"

// The record of a member of a tree: a child's acceptance is the work of the architect that owns
// the child when its step starts, so the record of its step 5 names that architect, with the
// session its claim holds and the model that answered it last. The child stands in reviewing under
// the walk's root, on a pull request of its own; a person's approval moves it to accepting.
//   - The child's own architect holds its role: a launch an earlier daemon began, which the boot
//     relaunches. It signs the child off through its own route while its last answer waits on its
//     claim's feed (heldRequest), so the record's model is the one its stream read ahead of the
//     request: the request waited for the child architect's stream, which the record names.
//   - The control: the child's own architect was never started, so it holds no role, the tree's
//     architect owns the child, and it signs the child off.
func TestAChildsAcceptanceRecordNamesItsOwningArchitect(t *testing.T) {
	t.Parallel()
	acceptance := func(r *stepRecordsRig, child string, agent claim.Token, session, model string) {
		r.t.Helper()
		r.waitStepsOf(child, "the child's acceptance as its owning architect's", func(read stepRecordsRead) error {
			got := read.find("step", 5, 1)
			switch {
			case got.ID == "":
				return fmt.Errorf("the read of %s holds no record of step 5: %+v", child, read)
			case got.Role != "acceptance" || got.EndedAt == nil || got.Result != "passed":
				return fmt.Errorf("the acceptance of %s is %+v, want role acceptance, ended passed", child, got)
			case got.Agent != string(agent) || got.Session != session || got.Model != model:
				return fmt.Errorf("the acceptance of %s names agent %q, session %q and model %q; want %s, %s and %s", child, got.Agent, got.Session, got.Model, agent, session, model)
			}
			return nil
		})
	}
	t.Run("the child's own architect holds its role", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		child, number := r.seedChildInReviewing()
		own := r.tokenOf(child, claim.RoleArchitect)
		if _, err := r.st.AdmitClaim(context.Background(), supervise.Claim{
			Token: own, Project: r.project, Tree: r.key, Issue: child, Role: claim.RoleArchitect,
			Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
		}); err != nil {
			t.Fatalf("record the unfinished launch of %s's own architect: %v", child, err)
		}
		writePrompt(t, r.cfg, own)
		r.restart()
		architect := r.workerOf(child, claim.RoleArchitect)
		r.publishReview(number, "APPROVED")
		r.waitPhaseOf(child, phase.Accepting)
		architect.model = "stand-in/child-architect-first"
		architect.answer()
		architect.send(shimwire.AgentStart{})
		architect.waitState(supervise.StateWorking)
		if status, body := r.heldRequest(architect, "stand-in/child-architect-second", "/legion/v1/signoff", map[string]any{
			"grantId": architect.grant(), "issue": child,
		}); status != http.StatusOK {
			t.Fatalf("the sign-off of %s by its own architect = %d %s, want 200", child, status, body)
		}
		r.waitPhaseOf(child, phase.Retro)
		acceptance(r, child, own, architect.session, "stand-in/child-architect-second")
	})
	t.Run("control: the child's own architect was never started", func(t *testing.T) {
		r := newStepRecordsRig(t, 1)
		child := r.childInAccepting()
		r.signOff(child)
		architect := r.worker(claim.RoleArchitect)
		acceptance(r, child, r.token(claim.RoleArchitect), architect.session, stepRecordsModel)
	})
}

// childInAccepting seeds a child of the walk's root in reviewing (seedChildInReviewing) and has a
// person approve its pull request's head (publishReview), which moves it to accepting, the step of
// the architect that owns it; it answers the child's key once the child stands there.
func (r *stepRecordsRig) childInAccepting() string {
	r.t.Helper()
	child, number := r.seedChildInReviewing()
	r.publishReview(number, "APPROVED")
	r.waitPhaseOf(child, phase.Accepting)
	return child
}

// A step's record, by the daemon's own wiring: the planner's completion reaches the daemon on its
// HTTP request while the answer its model gave last has been read by the stream and not yet
// decided by the claim's machine (heldRequest). The completion route waits for that machine
// (api.Options.AwaitStream, which serve gives the daemon's awaitStream), so the record of planning
// ends naming the planner's session and that answer's model, not the model of the answer before.
func TestACompletionNamesTheModelItsStreamReadAheadOfIt(t *testing.T) {
	t.Parallel()
	r := newStepRecordsRig(t, 1)
	planner := r.worker(claim.RolePlanner)
	planner.model = "stand-in/planner-first"
	planner.take()
	req := api.HandoffCompleteRequest{Summary: "planned"}
	req.HandoffID, req.HandoffVersion = r.writeHandoff(planner.session, "plan")
	req.GrantID = planner.grant()
	if status, body := r.heldRequest(planner, "stand-in/planner-second", "/legion/v1/handoff/complete", req); status != http.StatusOK {
		t.Fatalf("the planner's completion = %d %s, want 200", status, body)
	}
	planner.endTurn()
	r.waitPhase(phase.Implementing)
	r.waitSteps("the end of planning", func(got stepRecordsRead) error {
		switch found := got.find("step", 1, 1); {
		case found.ID == "" || found.EndedAt == nil || found.Result != "passed":
			return fmt.Errorf("planning reads %+v, want it ended passed", found)
		case found.Session != planner.session || found.Model != "stand-in/planner-second":
			return fmt.Errorf("planning names session %q and model %q; want %q and the model that answered last, stand-in/planner-second",
				found.Session, found.Model, planner.session)
		}
		return nil
	})
}

// The record of the architect's own step: a move back out of accepting ends the acceptance, the
// step of the architect that owns the child (the tree's: the child's own was never started), as
// its sign-off does, so its record names the model whose answer the
// architect's stream read ahead of the request, though the claim's machine has not decided that
// answer yet when the request arrives (heldRequest). The architect answers with one model and then
// with another, and the child's acceptance ends failed, back at implementing, with the architect's
// session and the second model.
func TestAnArchitectsMoveBackOutOfAcceptingNamesTheModelThatAnsweredLast(t *testing.T) {
	t.Parallel()
	r := newStepRecordsRig(t, 1)
	child := r.childInAccepting()
	architect := r.worker(claim.RoleArchitect)
	architect.model = "stand-in/architect-first"
	architect.answer()
	architect.send(shimwire.AgentStart{})
	architect.waitState(supervise.StateWorking)
	grant := architect.grant()
	if status, body := r.heldRequest(architect, "stand-in/architect-second", "/legion/v1/phase/backward", map[string]any{
		"grantId": grant, "issue": child, "to": "implementing", "reason": "the acceptance found a gap",
	}); status != http.StatusOK {
		t.Fatalf("the tree architect's move back of %s = %d %s, want 200", child, status, body)
	}
	r.waitPhaseOf(child, phase.Implementing)
	tree := r.token(claim.RoleArchitect)
	r.waitStepsOf(child, "the acceptance the architect moved back", func(read stepRecordsRead) error {
		switch got := read.find("step", 5, 1); {
		case got.ID == "" || got.EndedAt == nil || got.Result != "failed" || got.ReturnedTo == nil || *got.ReturnedTo != 2:
			return fmt.Errorf("the acceptance of %s is %+v, want it ended failed, returned to step 2", child, got)
		case got.Agent != string(tree) || got.Session != architect.session || got.Model != "stand-in/architect-second":
			return fmt.Errorf("the acceptance of %s names agent %q, session %q and model %q; want %s, %s and the model that answered last, stand-in/architect-second",
				child, got.Agent, got.Session, got.Model, tree, architect.session)
		}
		return nil
	})
}

// heldRequest sends c's request to path, one that ends c's step, while the answer c's model gave
// last waits on the claim's feed, and answers the request's status and body. c's model answers as
// model, which c.model holds from then on, and the feed stops before the machine decides that
// answer (overrides.beforeDecision): the stream has read it, so whatever the stream reads after it
// comes after it, and the machine holds no lock while it waits, so the request reads the claim's
// machine as it reads any live claim's. A hold inside a decision would not do: the request's grant
// check reads the claim (supervise.Machine.Claim), which waits for the decision in flight. The
// request is sent once the answer is held; the hold ends once the daemon says the request began
// its wait for c's machine (overrides.streamWaitBegan), or once the request has answered without
// one; r.waited says which.
func (r *stepRecordsRig) heldRequest(c *stepClaimant, model, path string, body any) (int, []byte) {
	t := r.t
	t.Helper()
	held := &heldAnswer{token: c.token, model: model, reached: make(chan struct{}), release: make(chan struct{})}
	r.held.Store(held)
	release := sync.OnceFunc(func() {
		r.held.Store(nil)
		close(held.release)
	})
	t.Cleanup(release)
	c.model = model
	c.answer()
	select {
	case <-held.reached:
	case <-time.After(stepRecordsWait):
		t.Fatalf("the feed of %s never came to the answer of %s within %s", c.token, model, stepRecordsWait)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, r.d.base+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	for len(r.waits) > 0 {
		<-r.waits
	}
	type answer struct {
		status int
		body   []byte
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		response, err := r.d.client.Do(request)
		if err != nil {
			answered <- answer{err: err}
			return
		}
		defer response.Body.Close()
		read, err := io.ReadAll(response.Body)
		answered <- answer{status: response.StatusCode, body: read, err: err}
	}()
	r.waited = false
	deadline := time.After(stepRecordsWait)
	for {
		select {
		case token := <-r.waits:
			if token == c.token {
				r.waited = true
				release()
			}
		case got := <-answered:
			release()
			if got.err != nil {
				t.Fatalf("POST %s: %v", path, got.err)
			}
			return got.status, got.body
		case <-deadline:
			t.Fatalf("POST %s for %s neither began its wait nor answered within %s", path, c.token, stepRecordsWait)
		}
	}
}

// seedChildInReviewing records a child of the walk's root, created in Dispatch and held by the
// daemon's store in reviewing at generation 1, on an open pull request of its own at
// stepRecordsHead that GitHub reports clean, and answers its key and its pull request's number.
func (r *stepRecordsRig) seedChildInReviewing() (string, int) {
	t := r.t
	t.Helper()
	var created createdIssue
	r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": "A child in the walk's tree"}, http.StatusCreated, &created)
	number := r.number + 1
	parent := r.key
	ctx := context.Background()
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: created.Key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.key, Parent: &parent, Project: r.cfg.Project,
			Title: "A child in the walk's tree", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review",
			Rank: created.Rank, LastDispatchSeq: created.LastSeq, HandedOver: true,
		}); err != nil {
			return err
		}
		return r.records.PutPullRequest(ctx, tx, stepRecordsPullRequest(created.Key, number))
	}); err != nil {
		t.Fatalf("seed %s: %v", created.Key, err)
	}
	return created.Key, number
}

// A model whose name holds U+0000 is a name no store can hold: PostgreSQL's text and jsonb, the
// daemon's outbox payload and Dispatch's record among them, refuse that character. The planner's
// model answers with a storable name and then with one holding U+0000, and the planner completes:
// the completion moves the issue on, and the record of planning reaches Dispatch ended, with the
// planner's session and no model, as for an agent that named none. The model that answered first
// did not answer last, so the record does not name it.
func TestAModelHoldingANulIsNotKept(t *testing.T) {
	t.Parallel()
	r := newStepRecordsRig(t, 1)
	planner := r.worker(claim.RolePlanner)
	planner.model = "stand-in/first"
	planner.answer()
	planner.model = "stand-in/mo\x00del"
	r.plan()
	r.waitSteps("the end of planning", func(got stepRecordsRead) error {
		switch found := got.find("step", 1, 1); {
		case found.ID == "" || found.EndedAt == nil || found.Result != "passed":
			return fmt.Errorf("planning reads %+v, want it ended passed", found)
		case found.Session != planner.session || found.Model != "":
			return fmt.Errorf("planning names session %q and model %q, want %q and no model", found.Session, found.Model, planner.session)
		}
		return nil
	})
}

// Whose record is whose: each role's agent answers with a model of its own, and the
// implementer, the tester and the reviewer with another on each later attempt, so a record that
// took another claim's session or model, or an earlier attempt's model, names the wrong one. The
// read names, for each of the walk's ten records, the claim token of its agent, that claim's
// session and the model that answered it last; the merger the merge launches never registers, so
// the merge names its token and no session and no model. A step that runs names no session yet:
// its end names the session and the model together.
func TestEachRecordNamesItsOwnAgentsSessionAndModel(t *testing.T) {
	t.Parallel()
	r := newStepRecordsRig(t, 1)
	planner := r.worker(claim.RolePlanner)
	planner.model = "stand-in/planner"
	planner.take()
	r.waitSteps("the start of planning", func(got stepRecordsRead) error {
		switch found := got.find("step", 1, 1); {
		case found.ID == "" || found.StartedAt == nil:
			return errors.New("no started record of planning")
		case found.Session != "" || found.Model != "":
			return fmt.Errorf("planning, still running, names session %q and model %q, want neither", found.Session, found.Model)
		}
		return nil
	})
	r.finish(planner, "plan", api.HandoffCompleteRequest{Summary: "planned"}, phase.Implementing)
	// answering gives role's agent the model its next answers name.
	answering := func(role claim.Role, model string) { r.worker(role).model = model }
	answering(claim.RoleImplementer, "stand-in/implementer")
	r.implement()
	answering(claim.RoleTester, "stand-in/tester")
	r.test()
	answering(claim.RoleReviewer, "stand-in/reviewer")
	r.review("CHANGES_REQUESTED", phase.Implementing)
	answering(claim.RoleImplementer, "stand-in/implementer-2")
	r.implement()
	answering(claim.RoleTester, "stand-in/tester-2")
	r.test()
	answering(claim.RoleReviewer, "stand-in/reviewer-2")
	r.review("APPROVED", phase.Accepting)
	answering(claim.RoleArchitect, "stand-in/architect")
	r.accept()
	answering(claim.RoleImplementer, "stand-in/implementer-3")
	r.retro()
	r.merge()

	own := func(role claim.Role, model string) *stepOwner {
		c := r.claims[r.token(role)]
		return &stepOwner{agent: string(c.token), session: c.session, model: model}
	}
	want := withOwners(t, stepRecordsTen,
		own(claim.RolePlanner, "stand-in/planner"),
		own(claim.RoleImplementer, "stand-in/implementer"),
		own(claim.RoleImplementer, "stand-in/implementer-2"),
		own(claim.RoleTester, "stand-in/tester"),
		own(claim.RoleTester, "stand-in/tester-2"),
		own(claim.RoleReviewer, "stand-in/reviewer"),
		own(claim.RoleReviewer, "stand-in/reviewer-2"),
		own(claim.RoleArchitect, "stand-in/architect"),
		own(claim.RoleImplementer, "stand-in/implementer-3"),
		&stepOwner{agent: string(r.token(claim.RoleMerger))},
	)
	r.waitSteps("the ten records of the walk, each its own agent's", func(got stepRecordsRead) error { return stepsHold(got, want) })
}
