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
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// stepRecordsHead is the head of the issue's pull request, which every completion and review
	// of the walk names.
	stepRecordsHead = "5e7f00d5a1b2c3d4e5f60718293a4b5c6d7e8f90"
	// stepRecordsPerson is the account with write access whose reviews decide each round.
	stepRecordsPerson = "a-person"
	// stepRecordsModel is the model an agent of the walk answers with until its test gives the
	// agent another (stepClaimant.model), as Oh My Pi's message_end names it: provider and model.
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
	// A row Dispatch refused while it was stopped waits out its backoff, at most retryBackoff's
	// cap, before it runs again, and the sweep finds a lease that lapsed within one more lease.
	within := retryBackoff(6) + outboxLease
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
// every outbox row and every count stands after it as it stood before. The earlier release's move
// is then written as the rows this release reads of it: the issue in testing, and the tester's
// entry raised again with its last handoff cleared; none of that release's deliveries stands in
// the store. The newer image boots, its Migrate applying the roll forward. The walk ends at done,
// and the read holds the implementer's run that the earlier release ended stopped at the moment
// the way back kept, under the id its start wrote, the tester's run that release started as a
// record of its own at the next count, started when its entry was raised, and the tester's run
// this release counted under its own id.
func TestARollbackAndItsRollForwardKeepEveryRecordOfTheWalk(t *testing.T) {
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
	// stepRecordsRollForward is the version of the migration whose header carries the step records'
	// way back, which the way back re-arms and the newer image's Migrate applies again.
	stepRecordsRollForward = 20261117064152
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
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
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
	block, err := migrations.WayBack(stepRecordsRollForward)
	if err != nil {
		r.t.Fatalf("read the way back of the roll forward: %v", err)
	}
	r.exec(block)
}

// stepRecordsRig is one walk: the booted daemon over the real Dispatch, the stand-ins it reads,
// the issues of the run and their pull requests, and each role's agent as the test plays it. The
// walk's steps act on one issue of issues at a time, its walkIssue, which on moves to another.
type stepRecordsRig struct {
	liveProof
	walkIssue
	rt       *fake.Runtime
	built    *built
	dispatch *scratchDispatch
	github   *stepRecordsGitHub
	issues   []walkIssue
	claims   map[claim.Token]*stepClaimant
	// reviews counts the reviews published, which numbers each.
	reviews int
	// waits names the claim of each agent's request that began its wait for the claim's machine
	// (overrides.streamWaitBegan), as heldRequest reads it; a wait sent while it is full is not
	// kept.
	waits chan claim.Token
	// held is the answer heldRequest holds on its claim's feed (overrides.beforeDecision), nil
	// while it holds none.
	held atomic.Pointer[heldAnswer]
}

// heldAnswer is the answer heldRequest holds: the feed of token's claim stops before its machine
// decides the answer naming model, says so by closing reached, and decides it once release closes.
type heldAnswer struct {
	token   claim.Token
	model   string
	reached chan struct{}
	release chan struct{}
}

// walkIssue is one issue of the run, the title Dispatch was given for it, and the number of its
// pull request.
type walkIssue struct {
	key    string
	title  string
	number int
}

// newStepRecordsRig is newStepRecordsRigWith for a run of issues issues over the package's broker
// and a Dispatch of this checkout, which the package's own tests run. The live drivers call
// newStepRecordsRigWith themselves, through newStepRecordsLive (step_records_live_test.go).
func newStepRecordsRig(t *testing.T, issues int) *stepRecordsRig {
	t.Helper()
	return newStepRecordsRigWith(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), issues)
}

// newStepRecordsRigWith sets up dispatch, a running Dispatch, with the run's project, its
// repository mapping and issues issues, seeds the daemon's store of proof with each issue at
// admitted, its gate approved, on an open pull request of its own GitHub reports clean, boots the
// daemon at Dispatch and registers each tree's architect. The admission pass then starts planning,
// the first step of each walk. The walk's steps act on the first issue until on moves them.
func newStepRecordsRigWith(t *testing.T, proof liveProof, dispatch *scratchDispatch, issues int) *stepRecordsRig {
	t.Helper()
	// A Dispatch project key is at most ten characters, and the daemon's project is its Dispatch
	// project, so the run's project is a short one of its own.
	proof.cfg.Project = "SR" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	proof.cfg.AdmissionCap = 4
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &stepRecordsRig{liveProof: proof, dispatch: dispatch, github: newStepRecordsGitHub(t), claims: map[claim.Token]*stepClaimant{},
		waits: make(chan claim.Token, 16)}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Step records"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/settings/repo-projects/"+liveRepo, map[string]any{"project": r.cfg.Project, "default": true}, http.StatusOK, nil)
	for place := range issues {
		var created createdIssue
		r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": walkTitles[place]}, http.StatusCreated, &created)
		r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, &created)
		r.issues = append(r.issues, walkIssue{key: created.Key, title: walkTitles[place], number: int(number) + 1 + place})
		r.on(created.Key)
		r.github.open("legion/"+r.key, r.number)
		r.seed(created.Rank, created.LastSeq)
	}
	r.boot()
	// The architect's pane is met at the boot's relaunch, so it is a live session before any
	// restart: a restarted daemon re-observes a claim it finds live and does not launch it again.
	for _, issue := range r.issues {
		r.on(issue.key)
		r.worker(claim.RoleArchitect)
	}
	r.on(r.issues[0].key)
	return r
}

// walkTitles is the title of each issue of a run, in order. Dispatch refuses a new issue whose
// title is a near copy of an open one's (POSSIBLE_DUPLICATE), so no two share a word that counts.
var walkTitles = []string{"One issue walks to done", "A second change rates its plan", "Third piece merges after its retro", "Fourth job refused unrated"}

// on moves the walk's steps to issue key of the run.
func (r *stepRecordsRig) on(key string) {
	r.t.Helper()
	for _, issue := range r.issues {
		if issue.key == key {
			r.walkIssue = issue
			return
		}
	}
	r.t.Fatalf("%s is no issue of the run %v", key, r.issues)
}

// createdIssue is an issue as Dispatch answers its creation: its key, its rank and the sequence
// the daemon's store records as the last it saw of the issue.
type createdIssue struct {
	Key     string `json:"key"`
	Rank    string `json:"rank"`
	LastSeq int64  `json:"last_seq"`
}

// openPullRequest is pull request number of issue, open at stepRecordsHead on the branch
// legion/<issue>, which GitHub reports clean, as the daemon's store records it.
func openPullRequest(issue string, number int) record.PullRequest {
	return record.PullRequest{State: record.PullRequestOpen, Issue: issue, Repo: liveRepo, Number: number,
		Branch: "legion/" + issue, HeadSHA: stepRecordsHead, MergeState: record.MergeStateClean, Failing: []string{}}
}

// seed records the issue as the daemon holds it the moment its architect's spec is approved: a
// root handed to Legion at admitted, its gate opened, its lifecycle under the workflow and its
// architect's claim a launch a previous daemon began and never recorded, which the boot relaunches.
// Its pull request is recorded open at stepRecordsHead, which GitHub reports clean, so a review
// that approves that head ends its round and the implementer's completions find it.
func (r *stepRecordsRig) seed(rank string, seq int64) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	approved := 1
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: "One issue walks to done", Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: rank, LastDispatchSeq: seq, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := r.records.PutGate(ctx, tx, record.DesignGate{
			Issue: r.key, ArtifactID: "spec-" + r.key, LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true,
		}); err != nil {
			return err
		}
		return r.records.PutPullRequest(ctx, tx, openPullRequest(r.key, r.number))
	}); err != nil {
		t.Fatalf("seed %s: %v", r.key, err)
	}
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.key, err)
	}
	architect := r.token(claim.RoleArchitect)
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: architect, Project: r.project, Tree: r.key, Issue: r.key, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	}); err != nil {
		t.Fatalf("record the architect's unfinished launch on %s: %v", r.key, err)
	}
	writePrompt(t, r.cfg, architect)
}

// boot starts the whole daemon on the store as it stands, over a fake runtime of its own.
func (r *stepRecordsRig) boot() {
	r.t.Helper()
	r.rt, r.built = fake.NewRuntime(), &built{}
	// The runtime provisions each claim's workspace itself, so a launch reaches the fake without
	// the daemon cloning acme/widgets from github.com.
	r.rt.InPod = true
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github.url
	o.streamWaitBegan = func(token claim.Token) {
		select {
		case r.waits <- token:
		default:
		}
	}
	o.beforeDecision = func(token claim.Token, ev supervise.Event) {
		held := r.held.Load()
		if answered, ok := ev.(supervise.StreamAnswered); ok && held != nil && token == held.token && answered.Model == held.model {
			close(held.reached)
			<-held.release
		}
	}
	r.d = startDaemonWithin(r.t, r.cfg, o, r.log(), stepRecordsWait)
}

// restart stops the daemon and boots it again (reboot).
func (r *stepRecordsRig) restart() {
	r.t.Helper()
	r.restartWith(func() {})
}

// restartWith is restart with between run on the store while no daemon runs, which is where a
// deploy leaves what the daemon of the release before wrote and the next one boots on.
func (r *stepRecordsRig) restartWith(between func()) {
	r.t.Helper()
	r.d.stop()
	between()
	r.reboot()
}

// reboot boots the stopped daemon again on the same store, the same Dispatch and the same ports.
// Each pane outlives the daemon, as a tmux pane does: the new daemon re-observes each live claim
// rather than launching it again, and each pane's shim connects to it with the boot token it has
// held since its launch, its turn and its session as they were.
func (r *stepRecordsRig) reboot() {
	r.t.Helper()
	rebindHeldPorts(r.t, &r.cfg)
	r.boot()
	for _, c := range r.claims {
		c.reconnect()
	}
}

// plan is the planner taking its task, answering, writing its plan to Dispatch and completing.
func (r *stepRecordsRig) plan() {
	r.t.Helper()
	r.work(claim.RolePlanner, "plan", api.HandoffCompleteRequest{Summary: "planned"}, phase.Implementing)
}

// implement is one round of the implementer on the pull request's head.
func (r *stepRecordsRig) implement() {
	r.t.Helper()
	r.work(claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Testing)
}

// testCompletion is the tester's completion of a round it passes.
var testCompletion = api.HandoffCompleteRequest{Summary: "every criterion holds", Verdict: "pass"}

// test is one round of the tester, which passes.
func (r *stepRecordsRig) test() {
	r.t.Helper()
	r.work(claim.RoleTester, "test", testCompletion, phase.Reviewing)
}

// retro is the implementer's retro rating the plan 4 (rate).
func (r *stepRecordsRig) retro() {
	r.t.Helper()
	r.rate(&intake.PlanRating{Score: 4, Reason: "The plan fit the work."})
}

// rate is the implementer's retro, whose completion names no record, the head it leaves, as each
// completion of the implementer's does, and the rating of the plan with its reason, which a pane
// at api.PlanRatingContract or later must give (PLAN_RATING_REQUIRED). A nil rating is a retro
// with none, a pane's of the plugin before that contract.
func (r *stepRecordsRig) rate(rating *intake.PlanRating) {
	r.t.Helper()
	r.work(claim.RoleImplementer, "", r.retroCompletion(rating), phase.Merging)
}

// retroCompletion is the implementer's completion of its retro with rating and its reason, and with
// neither for a nil rating.
func (r *stepRecordsRig) retroCompletion(rating *intake.PlanRating) api.HandoffCompleteRequest {
	req := api.HandoffCompleteRequest{Summary: "retro written", Commit: stepRecordsHead}
	if rating != nil {
		score, reason := rating.Score, rating.Reason
		req.PlanRating, req.PlanReason = &score, &reason
	}
	return req
}

// walkToRetro walks the issue through planning, implementing, testing, an approving review and
// the architect's sign-off, to its retro.
func (r *stepRecordsRig) walkToRetro() {
	r.t.Helper()
	r.plan()
	r.implement()
	r.test()
	r.review("APPROVED", phase.Accepting)
	r.accept()
}

// unratedRetroRefused is c's completion of the retro with no rating, refused 400
// PLAN_RATING_REQUIRED, and answers the refusal's body. It fails first when c's claim records a
// plugin contract below api.PlanRatingContract, whose retro the daemon takes unrated, so a pass
// says the refusal is the contract's.
func (r *stepRecordsRig) unratedRetroRefused(c *stepClaimant) string {
	t := r.t
	t.Helper()
	contract := r.d.claim(c.token).PluginContract
	if contract < api.PlanRatingContract {
		t.Fatalf("the %s of %s registered plugin contract %d, below PlanRatingContract %d: this check proves nothing", c.role, r.key, contract, api.PlanRatingContract)
	}
	status, body := r.complete(c, r.retroCompletion(nil))
	if status != http.StatusBadRequest || !strings.Contains(body, "PLAN_RATING_REQUIRED") {
		t.Fatalf("the retro's completion of %s with no rating from the %s at plugin contract %d = %d %s, want 400 PLAN_RATING_REQUIRED",
			r.key, c.role, contract, status, body)
	}
	return body
}

// work is one role's turn of the walk: it takes the task, its agent's model answering (c.model),
// and finishes it.
func (r *stepRecordsRig) work(role claim.Role, word string, req api.HandoffCompleteRequest, next phase.Phase) {
	r.t.Helper()
	c := r.worker(role)
	c.take()
	r.finish(c, word, req, next)
}

// finish is the end of c's turn: it writes its handoff record of word to Dispatch under its own
// session (none for word ""), completes, ends its turn, and waits for the issue to reach next.
func (r *stepRecordsRig) finish(c *stepClaimant, word string, req api.HandoffCompleteRequest, next phase.Phase) {
	t := r.t
	t.Helper()
	if word != "" {
		req.HandoffID, req.HandoffVersion = r.writeHandoff(c.session, word)
	}
	if status, body := r.complete(c, req); status != http.StatusOK {
		t.Fatalf("the %s's completion = %d %s, want 200", c.role, status, body)
	}
	c.endTurn()
	r.waitPhase(next)
}

// complete posts c's completion req with a grant minted for it now, and answers the daemon's
// status and body.
func (r *stepRecordsRig) complete(c *stepClaimant, req api.HandoffCompleteRequest) (int, string) {
	r.t.Helper()
	req.GrantID = c.grant()
	status, body := r.d.request(http.MethodPost, "/legion/v1/handoff/complete", req, false)
	return status, string(body)
}

// review is the reviewer taking its task and answering, and then a person submitting state on the
// walk's pull request (publishReview); the issue then reaches next.
func (r *stepRecordsRig) review(state string, next phase.Phase) {
	r.t.Helper()
	c := r.worker(claim.RoleReviewer)
	c.take()
	c.endTurn()
	r.publishReview(r.number, state)
	r.waitPhase(next)
}

// publishReview is a person with write access submitting state on pull request number at
// stepRecordsHead, as Envoy relays GitHub's review.
func (r *stepRecordsRig) publishReview(number int, state string) {
	t := r.t
	t.Helper()
	r.reviews++
	payload, err := json.Marshal(map[string]string{
		"action": "submitted", "kind": "review", "repo": liveRepo, "number": strconv.Itoa(number), "state": state,
		"commit_id": stepRecordsHead, "head_sha": stepRecordsHead, "author": stepRecordsPerson, "body": "round " + strconv.Itoa(r.reviews),
		"review_id": strconv.Itoa(number*10 + r.reviews), "submitted_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.review", r.subjectRepo(), number)
	r.publish(subject, "github", fmt.Sprintf("review-%d-%d", number, r.reviews), string(payload))
}

// accept is the tree architect answering and signing off the walk's issue (signOff).
func (r *stepRecordsRig) accept() {
	r.t.Helper()
	r.signOff(r.key)
}

// signOff is the tree architect answering in a turn of its own and signing off issue, a member of
// its tree, through its own route; issue then reaches its retro.
func (r *stepRecordsRig) signOff(issue string) {
	t := r.t
	t.Helper()
	c := r.worker(claim.RoleArchitect)
	c.ownTurn()
	status, body := r.d.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": c.grant(), "issue": issue}, false)
	if status != http.StatusOK {
		t.Fatalf("the architect's sign-off of %s = %d %s, want 200", issue, status, body)
	}
	r.waitPhaseOf(issue, phase.Retro)
}

// merge waits for the merger's launch and publishes GitHub's merge of the pull request, which ends
// the walk at done.
func (r *stepRecordsRig) merge() {
	t := r.t
	t.Helper()
	r.awaitLaunch(r.token(claim.RoleMerger))
	r.github.merged(r.number)
	payload, err := json.Marshal(map[string]string{
		"action": "closed", "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(r.number), "merged": "true",
		"merge_commit_sha": "merge-" + stepRecordsHead, "head_sha": stepRecordsHead, "base_ref": "main", "head_ref": "legion/" + r.key,
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), r.number)
	r.publish(subject, "github", fmt.Sprintf("pr-%d-merged", r.number), string(payload))
	r.waitPhase(phase.Done)
}

// waitPhase waits for the issue to stand in want.
func (r *stepRecordsRig) waitPhase(want phase.Phase) {
	r.t.Helper()
	r.waitPhaseOf(r.key, want)
}

// waitPhaseOf waits for issue to stand in want.
func (r *stepRecordsRig) waitPhaseOf(issue string, want phase.Phase) {
	r.t.Helper()
	var last string
	if !testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool {
		got, ok := r.d.state().Issues[issue]
		last = string(got.Phase)
		return ok && last == string(want)
	}) {
		r.t.Fatalf("%s stands in %q, want %s", issue, last, want)
	}
}

// token is the claim token of role on the walk's issue, as the daemon spells it.
func (r *stepRecordsRig) token(role claim.Role) claim.Token {
	r.t.Helper()
	return r.tokenOf(r.key, role)
}

// tokenOf is the claim token of role on issue, as the daemon spells it.
func (r *stepRecordsRig) tokenOf(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// awaitLaunch waits for the running daemon to launch token's claim.
func (r *stepRecordsRig) awaitLaunch(token claim.Token) {
	r.t.Helper()
	testwait.WithinPolling(r.t, stepRecordsWait, 50*time.Millisecond, "the daemon's launch of "+string(token), func() bool {
		for _, call := range r.rt.Calls() {
			if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
				return true
			}
		}
		return false
	})
}

// person sends one request to Dispatch as the run's person, through the identity header the
// scratch server trusts, requires want, and decodes the answer into into when it is not nil.
func (r *stepRecordsRig) person(method, path string, body any, want int, into any) {
	t := r.t
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, r.dispatch.base+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "step-records")
	status, answer := r.send(request)
	if status != want {
		t.Fatalf("%s %s = %d %s, want %d", method, path, status, answer, want)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}
}

// writeHandoff writes the issue's handoff record of word under session, as `dispatch
// handoff-write` does with the agent's bearer, and answers its id and version.
func (r *stepRecordsRig) writeHandoff(session, word string) (string, int) {
	t := r.t
	t.Helper()
	body := fmt.Sprintf(`{"schemaVersion": 1, "phase": %q, "issue": %q, "summary": "written by %s"}`, word, r.key, session)
	encoded, err := json.Marshal(map[string]any{"actor": map[string]string{"kind": "session", "id": session}, "body": body})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, r.dispatch.base+"/api/v1/issues/"+r.key+"/handoffs/"+word, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.dispatch.token)
	status, answer := r.send(request)
	if status != http.StatusCreated {
		t.Fatalf("write the %s handoff of %s as %s = %d %s, want 201", word, r.key, session, status, answer)
	}
	var written struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(answer, &written); err != nil || written.ID == "" {
		t.Fatalf("decode the written handoff %s: %v", answer, err)
	}
	return written.ID, written.Version
}

func (r *stepRecordsRig) send(request *http.Request) (int, []byte) {
	r.t.Helper()
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		r.t.Fatalf("%s %s: %v", request.Method, request.URL.Path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		r.t.Fatalf("read %s %s: %v", request.Method, request.URL.Path, err)
	}
	return response.StatusCode, answer
}

// readSteps is GET /api/v1/issues/{key}/steps of the walk's issue as a person of the project reads
// it, or the error that the read met.
func (r *stepRecordsRig) readSteps() (stepRecordsRead, error) { return r.readStepsOf(r.key) }

// readStepsOf is GET /api/v1/issues/{issue}/steps as a person of the project reads it, or the
// error that the read met.
func (r *stepRecordsRig) readStepsOf(issue string) (stepRecordsRead, error) {
	var read stepRecordsRead
	if err := r.personRead("/api/v1/issues/"+issue+"/steps", &read); err != nil {
		return stepRecordsRead{}, err
	}
	return read, nil
}

// personRead is GET path on Dispatch as a person of the project reads it, decoded into into, or
// the error that the read met.
func (r *stepRecordsRig) personRead(path string, into any) error {
	request, err := http.NewRequest(http.MethodGet, r.dispatch.base+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("X-Dispatch-User", "step-records")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		answer, _ := io.ReadAll(response.Body)
		return fmt.Errorf("GET %s = %d %s", path, response.StatusCode, answer)
	}
	return json.NewDecoder(response.Body).Decode(into)
}

// waitSteps waits for the walk's issue's read to satisfy hold, and fails with hold's last answer.
func (r *stepRecordsRig) waitSteps(what string, hold func(stepRecordsRead) error) {
	r.t.Helper()
	r.waitStepsOf(r.key, what, hold)
}

// waitStepsOf waits for issue's read to satisfy hold, and fails with hold's last answer.
func (r *stepRecordsRig) waitStepsOf(issue, what string, hold func(stepRecordsRead) error) {
	r.t.Helper()
	waitRead(r, what, "Dispatch", time.Second, func() (stepRecordsRead, error) { return r.readStepsOf(issue) }, hold)
}

// waitRead waits for read to answer something that satisfies hold, reading again every interval,
// and fails naming where the read goes and hold's last answer.
func waitRead[T any](r *stepRecordsRig, what, where string, interval time.Duration, read func() (T, error), hold func(T) error) {
	r.t.Helper()
	var last error
	if !testwait.Holds(stepRecordsWait, interval, func() bool {
		got, err := read()
		if err != nil {
			last = err
			return false
		}
		last = hold(got)
		return last == nil
	}) {
		r.t.Fatalf("%s never reached %s: %v", what, where, last)
	}
}

// deleteNode deletes node id, a step record or a plan, from Dispatch's own store, as the store
// holds it: its node and every tuple that names it.
func (r *stepRecordsRig) deleteNode(id string) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, r.dispatch.database)
	if err != nil {
		t.Fatalf("connect to Dispatch's database: %v", err)
	}
	defer conn.Close(ctx)
	tag, err := conn.Exec(ctx, "delete from workflow_tuples where from_id = $1 or to_id = $1", id)
	if err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("delete the tuples of %s: %d rows, %v", id, tag.RowsAffected(), err)
	}
	if tag, err = conn.Exec(ctx, "delete from workflow_nodes where id = $1", id); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("delete the node %s: %d rows, %v", id, tag.RowsAffected(), err)
	}
}

// unfinishedRows is how many rows of kind of the issue the daemon's outbox holds unfinished.
func (r *stepRecordsRig) unfinishedRows(kind record.OutboxKind) int {
	r.t.Helper()
	var count int
	if err := r.st.Pool().QueryRow(context.Background(),
		"select count(*) from outbox where issue = $1 and kind = $2", r.key, string(kind)).Scan(&count); err != nil {
		r.t.Fatalf("count the issue's %s rows in the outbox: %v", kind, err)
	}
	return count
}

// stepRecordsRead is the read of an issue's steps, as Dispatch answers it.
type stepRecordsRead struct {
	Issue string `json:"issue"`
	Tasks []struct {
		Task  string           `json:"task"`
		Steps []stepRecordRead `json:"steps"`
	} `json:"tasks"`
}

// stepRecordRead is one record of the read.
type stepRecordRead struct {
	ID         string     `json:"id"`
	Task       string     `json:"task"`
	Step       int        `json:"step"`
	Kind       string     `json:"kind"`
	Role       string     `json:"role"`
	Type       string     `json:"type"`
	Agent      string     `json:"agent"`
	Session    string     `json:"session"`
	Model      string     `json:"model"`
	Generation uint64     `json:"generation"`
	Attempt    int        `json:"attempt"`
	StartedAt  *time.Time `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	Result     string     `json:"result"`
	ReturnedTo *int       `json:"returned_to"`
}

// records is every record of the read, task by task.
func (read stepRecordsRead) records() []stepRecordRead {
	var all []stepRecordRead
	for _, task := range read.Tasks {
		all = append(all, task.Steps...)
	}
	return all
}

// find is the record of kind, step and attempt, the zero record when the read holds none.
func (read stepRecordsRead) find(kind string, step, attempt int) stepRecordRead {
	for _, record := range read.records() {
		if record.Kind == kind && record.Step == step && record.Attempt == attempt {
			return record
		}
	}
	return stepRecordRead{}
}

// stepWant is one record a read must hold.
type stepWant struct {
	kind              string
	step, attempt     int
	role, typ, result string
	returnedTo        *int
	// owner, when set, is whose the record is exactly: its agent, its session and its model, an
	// empty session and model meaning none (the merger the merge launches never registers, and a run
	// whose end the roll forward of a rollback wrote). Unset, the record is any agent's, and one of
	// kind step names a session and stepRecordsModel.
	owner *stepOwner
}

// stepOwner is whose a record is: the claim token of its agent, that claim's session and the
// model that answered it.
type stepOwner struct {
	agent, session, model string
}

// withOwners is want with owners[i] the owner of want[i], want itself left as it is; it fails the
// test, naming both counts, when owners is not one owner for each record of want.
func withOwners(t *testing.T, want []stepWant, owners ...*stepOwner) []stepWant {
	t.Helper()
	if len(owners) != len(want) {
		t.Fatalf("%d owners for %d records, want one for each, owners[i] want[i]'s", len(owners), len(want))
	}
	owned := slices.Clone(want)
	for i := range owned {
		owned[i].owner = owners[i]
	}
	return owned
}

// stepsHold says whether read holds exactly the records of want, each of the task default, ended,
// with its role, type, agent, start, end, result and return; each of an owner named for that owner
// exactly (owner), and each other of kind step with its session and the model that answered it.
// The error names the first record that is missing or wrong.
func stepsHold(read stepRecordsRead, want []stepWant) error {
	for _, w := range want {
		got := read.find(w.kind, w.step, w.attempt)
		name := fmt.Sprintf("%s %d attempt %d", w.kind, w.step, w.attempt)
		switch {
		case got.ID == "":
			return fmt.Errorf("the read holds no record of %s", name)
		case got.Task != "default" || got.Role != w.role || got.Type != w.typ || got.Agent == "":
			return fmt.Errorf("%s is task %q, role %q, type %q, agent %q; want task default, role %q, type %q and an agent", name, got.Task, got.Role, got.Type, got.Agent, w.role, w.typ)
		case got.StartedAt == nil || got.EndedAt == nil || got.Result != w.result:
			return fmt.Errorf("%s started %v, ended %v with result %q; want a start, an end and %q", name, got.StartedAt, got.EndedAt, got.Result, w.result)
		case (got.ReturnedTo == nil) != (w.returnedTo == nil) || got.ReturnedTo != nil && *got.ReturnedTo != *w.returnedTo:
			return fmt.Errorf("%s returned to %v, want %v", name, got.ReturnedTo, w.returnedTo)
		case w.owner != nil && (got.Agent != w.owner.agent || got.Session != w.owner.session || got.Model != w.owner.model):
			return fmt.Errorf("%s names agent %q, session %q and model %q; want %q, %q and %q", name, got.Agent, got.Session, got.Model, w.owner.agent, w.owner.session, w.owner.model)
		case w.kind == "step" && w.owner == nil && (got.Session == "" || got.Model != stepRecordsModel):
			return fmt.Errorf("%s names session %q and model %q, want its agent's session and %s", name, got.Session, got.Model, stepRecordsModel)
		}
	}
	if got := len(read.records()); got != len(want) {
		return fmt.Errorf("the read holds %d records, want %d", got, len(want))
	}
	return nil
}

// stepClaimant is one role's claim as its pane reaches the daemon: the shim connected with the
// launch's boot token, the session registered and ready, and a reader that answers what the daemon
// asks of the shim and hands each prompt to the walk, starting its turn as Oh My Pi does.
type stepClaimant struct {
	r         *stepRecordsRig
	token     claim.Token
	issue     string
	role      claim.Role
	session   string
	secret    string
	bootToken string
	prompts   chan shimwire.Prompt
	mu        sync.Mutex
	// model is the model the agent's answers name, `<provider>/<model>`: stepRecordsModel until
	// its test gives the agent another.
	model string
	// sh is the shim's connection to the daemon now running, which a restart replaces.
	sh *shim
	// open is the id of each prompt whose turn has started and not ended.
	open []string
}

// worker is role's claimant: the one already registered, or the launch the daemon makes now,
// registered and ready under a session of its own.
func (r *stepRecordsRig) worker(role claim.Role) *stepClaimant {
	t := r.t
	t.Helper()
	token := r.token(role)
	if c, ok := r.claims[token]; ok {
		return c
	}
	r.awaitLaunch(token)
	launch := lastLaunch(t, r.rt, token)
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	c := &stepClaimant{r: r, token: token, issue: r.key, role: role, session: "ses_" + string(role) + "_" + randomSuffix(t)[:6], model: stepRecordsModel,
		bootToken: launch.BootToken, sh: dialShim(t, address, launch.BootToken), prompts: make(chan shimwire.Prompt, 8)}
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: c.session, OmpSessionFile: "/sessions/" + c.session + ".jsonl",
		AgentID: c.session, PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s = %d %s, want 200", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", token, err)
	}
	c.secret = registered.Secret
	go c.serve(c.sh)
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: c.session, Secret: c.secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", token, status, body)
	}
	r.claims[token] = c
	return c
}

// reconnect is the pane's shim connecting to the daemon that runs now with the boot token it has
// held since its launch, as it does after the daemon restarts.
func (c *stepClaimant) reconnect() {
	c.r.built.mu.Lock()
	address := c.r.built.address
	c.r.built.mu.Unlock()
	sh := dialShimUntilAcked(c.r.t, address, c.bootToken)
	c.mu.Lock()
	c.sh = sh
	c.mu.Unlock()
	go c.serve(sh)
}

// serve reads every frame the daemon sends sh until the connection closes: it answers the
// protocol negotiation and the state probe, and acknowledges each prompt and hands it to the walk,
// whose take starts its turn.
func (c *stepClaimant) serve(sh *shim) {
	reply := func(frame shimwire.Frame) {
		if err := sh.writer.WriteFrame(frame); err != nil {
			c.r.t.Errorf("send %s for %s: %v", frame.FrameType(), c.token, err)
		}
	}
	reader := sh.lines
	_ = sh.conn.SetReadDeadline(time.Time{})
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			continue
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			reply(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		case shimwire.GetState:
			c.mu.Lock()
			streaming := len(c.open) > 0
			c.mu.Unlock()
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			reply(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
		case shimwire.Prompt:
			reply(shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true})
			c.mu.Lock()
			c.open = append(c.open, f.ID)
			c.mu.Unlock()
			c.prompts <- f
		}
	}
}

// send writes one frame to the daemon that runs now.
func (c *stepClaimant) send(frame shimwire.Frame) {
	c.mu.Lock()
	sh := c.sh
	c.mu.Unlock()
	if err := sh.writer.WriteFrame(frame); err != nil {
		c.r.t.Errorf("send %s for %s: %v", frame.FrameType(), c.token, err)
	}
}

// take waits for the daemon's next task of this claim and starts the turn that works it, its
// model answering first: the claim's machine reads a connection's frames in order, so once the
// turn's start shows the claim working, the answer before it has been read too.
func (c *stepClaimant) take() {
	c.r.t.Helper()
	prompt := c.receive()
	c.answer()
	c.send(shimwire.AgentStart{DeliveryID: prompt.DeliveryID})
	c.waitState(supervise.StateWorking)
}

// receive waits for the daemon's next task of this claim, the prompt the pane has acknowledged
// and holds until a turn works it.
func (c *stepClaimant) receive() shimwire.Prompt {
	c.r.t.Helper()
	select {
	case prompt := <-c.prompts:
		return prompt
	case <-time.After(stepRecordsWait):
		c.r.t.Fatalf("%s was prompted with no task within %s", c.token, stepRecordsWait)
		return shimwire.Prompt{}
	}
}

// endTurnUnseen ends every turn a prompt started with no daemon of this release there to read it,
// as the earlier release runs the agent after a rollback: the pane holds no open turn when its
// shim next connects, so the daemon's state probe finds it idle.
func (c *stepClaimant) endTurnUnseen() {
	c.mu.Lock()
	c.open = nil
	c.mu.Unlock()
}

// ownTurn is a turn of the agent's own in which its model answers: it returns once the claim is
// out of it, so the answer, read before the turn's end, is the claim's last.
func (c *stepClaimant) ownTurn() {
	c.r.t.Helper()
	c.send(shimwire.AgentStart{})
	c.waitState(supervise.StateWorking)
	c.answer()
	c.send(shimwire.AgentEnd{})
	c.waitState(supervise.StateIdle)
}

// waitState waits for the daemon to hold the claim in state.
func (c *stepClaimant) waitState(state supervise.ClaimState) {
	c.r.t.Helper()
	var last string
	if !testwait.Holds(stepRecordsWait, 20*time.Millisecond, func() bool {
		last = c.r.d.claim(c.token).State
		return last == string(state)
	}) {
		c.r.t.Fatalf("%s is %s, want %s", c.token, last, state)
	}
}

// answer is the agent's model (c.model, `<provider>/<model>`) answering, which Oh My Pi's
// message_end names as message.provider and message.model.
func (c *stepClaimant) answer() {
	t := c.r.t
	t.Helper()
	provider, model, _ := strings.Cut(c.model, "/")
	frame, err := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "provider": provider, "model": model}})
	if err != nil {
		t.Fatal(err)
	}
	c.send(shimwire.Raw{Type: "message_end", JSON: frame})
}

// endTurn ends every turn a prompt started: the agent_end, and the result of each prompt.
func (c *stepClaimant) endTurn() {
	c.mu.Lock()
	open := c.open
	c.open = nil
	c.mu.Unlock()
	if len(open) == 0 {
		return
	}
	c.send(shimwire.AgentEnd{})
	for _, id := range open {
		c.send(shimwire.PromptResult{ID: id, AgentInvoked: true})
	}
}

// grant is a capability grant minted for this session now, as its `legion` tool mints one per
// operation.
func (c *stepClaimant) grant() string {
	t := c.r.t
	t.Helper()
	status, body := c.r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: c.session, Secret: c.secret, Tree: c.issue, Issue: c.issue}, false)
	if status != http.StatusOK {
		t.Fatalf("the grant of %s = %d %s, want 200", c.token, status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		t.Fatalf("decode the grant of %s: %v", c.token, err)
	}
	return granted.GrantID
}

// stepRecordsGitHub is GitHub's REST API as the walk's daemon reads it: every repository with
// default branch main, the pull request of each issue's branch at stepRecordsHead, clean until it
// merges, the person of the reviews with write access, and a GraphQL root that knows no pull
// request. Every other route is a 404, so nothing reaches github.com.
type stepRecordsGitHub struct {
	url string
	mu  sync.Mutex
	// pulls is each pull request opened, in the order they were opened.
	pulls []stepRecordsPull
}

// stepRecordsPull is one pull request of the walk: its number, the branch it is open on, and
// whether GitHub has merged it.
type stepRecordsPull struct {
	number int
	branch string
	merged bool
}

func newStepRecordsGitHub(t *testing.T) *stepRecordsGitHub {
	t.Helper()
	g := &stepRecordsGitHub{}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/"+liveRepo)
		pull, reviews := strings.CutSuffix(strings.TrimPrefix(rest, "/pulls/"), "/reviews")
		answer, known := "", false
		if number, err := strconv.Atoi(pull); err == nil && strings.HasPrefix(rest, "/pulls/") {
			answer, known = g.pullRequest(number)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/") && strings.Count(r.URL.Path, "/") == 3:
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
			_, _ = io.WriteString(w, `{"full_name":"`+parts[0]+"/"+parts[1]+`","default_branch":"main"}`)
		case r.Method == http.MethodGet && isRepository && rest == "/git/ref/heads/main":
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"base-main"}}`)
		case r.Method == http.MethodPost && isRepository && rest == "/git/refs":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && isRepository && rest == "/pulls":
			_, _ = fmt.Fprintf(w, "[%s]", strings.Join(g.pullRequests(), ","))
		case r.Method == http.MethodGet && isRepository && known && !reviews:
			_, _ = io.WriteString(w, answer)
		case r.Method == http.MethodGet && isRepository && known && reviews:
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && isRepository && rest == "/collaborators/"+stepRecordsPerson+"/permission":
			_, _ = io.WriteString(w, `{"permission":"write","user":{"login":"`+stepRecordsPerson+`"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		}
	}})
	return g
}

// open is the pull request number of branch, open at stepRecordsHead.
func (g *stepRecordsGitHub) open(branch string, number int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls = append(g.pulls, stepRecordsPull{number: number, branch: branch})
}

// merged is GitHub's merge of pull request number, which the walk opened: every read from now on
// answers it merged.
func (g *stepRecordsGitHub) merged(number int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.find(number).merged = true
}

// find is the pull request number, nil where none was opened. Its caller holds g.mu.
func (g *stepRecordsGitHub) find(number int) *stepRecordsPull {
	for place := range g.pulls {
		if g.pulls[place].number == number {
			return &g.pulls[place]
		}
	}
	return nil
}

// pullRequests is each pull request as GitHub answers it now, in the order they were opened.
func (g *stepRecordsGitHub) pullRequests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	answers := make([]string, len(g.pulls))
	for place, pull := range g.pulls {
		answers[place] = pull.answer()
	}
	return answers
}

// pullRequest is pull request number as GitHub answers it now, and whether it was opened.
func (g *stepRecordsGitHub) pullRequest(number int) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull := g.find(number)
	if pull == nil {
		return "", false
	}
	return pull.answer(), true
}

// answer is the pull request as GitHub's REST API answers it: open at stepRecordsHead and clean
// until it merges, closed and merged from then on.
func (p stepRecordsPull) answer() string {
	state, merged, mergeable := "open", false, "clean"
	if p.merged {
		state, merged, mergeable = "closed", true, "unknown"
	}
	return fmt.Sprintf(`{"number":%d,"state":%q,"merged":%t,"merge_commit_sha":%q,"mergeable":true,"mergeable_state":%q,"head":{"ref":%q,"sha":%q},"base":{"ref":"main"},"updated_at":"2026-11-17T00:00:00Z"}`,
		p.number, state, merged, "merge-"+stepRecordsHead, mergeable, p.branch, stepRecordsHead)
}

// The record of a member of a tree: a child's acceptance is its tree's work, so the record of its
// step 5 names the architect of the tree's root, with the session that architect's claim holds and
// the model that answered it, and never an architect of the child's own, whom no claim serves. The
// child stands in reviewing under the walk's root, on a pull request of its own; a person's
// approval moves it to accepting, and the root's architect signs it off through its own route.
func TestAChildsAcceptanceRecordNamesItsTreeArchitect(t *testing.T) {
	r := newStepRecordsRig(t, 1)
	child := r.childInAccepting()
	r.signOff(child)
	architect := r.worker(claim.RoleArchitect)
	own, tree := r.tokenOf(child, claim.RoleArchitect), r.token(claim.RoleArchitect)
	r.waitStepsOf(child, "the child's acceptance as the tree architect's", func(read stepRecordsRead) error {
		got := read.find("step", 5, 1)
		switch {
		case got.ID == "":
			return fmt.Errorf("the read of %s holds no record of step 5: %+v", child, read)
		case got.Role != "acceptance" || got.EndedAt == nil || got.Result != "passed":
			return fmt.Errorf("the acceptance of %s is %+v, want role acceptance, ended passed", child, got)
		case got.Agent != string(tree) || got.Agent == string(own):
			return fmt.Errorf("the acceptance of %s names agent %q, want the tree architect %s, not the child's own %s", child, got.Agent, tree, own)
		case got.Session != architect.session || got.Model != stepRecordsModel:
			return fmt.Errorf("the acceptance of %s names session %q and model %q, want the tree architect's %s and %s", child, got.Session, got.Model, architect.session, stepRecordsModel)
		}
		return nil
	})
}

// childInAccepting seeds a child of the walk's root in reviewing (seedChildInReviewing) and has a
// person approve its pull request's head (publishReview), which moves it to accepting, the tree
// architect's step; it answers the child's key once the child stands there.
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
// tree architect's step, as its sign-off does, so its record names the model whose answer the
// architect's stream read ahead of the request, though the claim's machine has not decided that
// answer yet when the request arrives (heldRequest). The architect answers with one model and then
// with another, and the child's acceptance ends failed, back at implementing, with the architect's
// session and the second model.
func TestAnArchitectsMoveBackOutOfAcceptingNamesTheModelThatAnsweredLast(t *testing.T) {
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

// A park of a child in accepting ends the acceptance, the tree architect's own step, as a move
// back out of accepting does, so its record names the model whose answer the architect's stream
// read ahead of the request (heldRequest). The child leaves the workflow, and its acceptance ends
// stopped, returned nowhere, with the architect's session and the second model.
func TestAParkOfAChildInAcceptingNamesTheModelThatAnsweredLast(t *testing.T) {
	r := newStepRecordsRig(t, 1)
	child := r.childInAccepting()
	architect := r.worker(claim.RoleArchitect)
	architect.model = "stand-in/architect-first"
	architect.answer()
	architect.send(shimwire.AgentStart{})
	architect.waitState(supervise.StateWorking)
	grant := architect.grant()
	if status, body := r.heldRequest(architect, "stand-in/architect-second", "/legion/v1/children/park", map[string]any{
		"grantId": grant, "issue": child,
	}); status != http.StatusOK {
		t.Fatalf("the tree architect's park of %s = %d %s, want 200", child, status, body)
	}
	r.waitPhaseOf(child, phase.Done)
	tree := r.token(claim.RoleArchitect)
	r.waitStepsOf(child, "the acceptance the architect's park ended", func(read stepRecordsRead) error {
		switch got := read.find("step", 5, 1); {
		case got.ID == "" || got.EndedAt == nil || got.Result != "stopped" || got.ReturnedTo != nil:
			return fmt.Errorf("the acceptance of %s is %+v, want it ended stopped, returned nowhere", child, got)
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
// one.
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
	deadline := time.After(stepRecordsWait)
	for {
		select {
		case token := <-r.waits:
			if token == c.token {
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
	r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": "A child the tree's architect accepts"}, http.StatusCreated, &created)
	number := r.number + 1
	parent := r.key
	ctx := context.Background()
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: created.Key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.key, Parent: &parent, Project: r.cfg.Project,
			Title: "A child the tree's architect accepts", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review",
			Rank: created.Rank, LastDispatchSeq: created.LastSeq, HandedOver: true,
		}); err != nil {
			return err
		}
		return r.records.PutPullRequest(ctx, tx, openPullRequest(created.Key, number))
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
