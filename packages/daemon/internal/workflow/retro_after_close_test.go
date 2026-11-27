package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// retroOpened is when the clock of every rig of the retro Legion runs after a close starts. A test
// that needs a moment apart from it, a retro's bound or the end of a linger, moves the clock by
// hand (retroRig.at).
var retroOpened = time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)

// retroRig is a tree whose closes the tests of the retro after a close drive (INFRA-5602): the
// pool, the engine on a clock the test moves, the Dispatch sequence the next event of the tree
// takes, and apply, which applies a fact as the daemon does and fails the test on an error. The
// tree is the root LEGION-208 and the issues a test seeds under it; each one's run starts through
// the admission's own start of a first phase (StartFirstPhase) and moves on through the engine's
// transitions, so the record of the run is the one the engine itself writes.
type retroRig struct {
	pool   *pgxpool.Pool
	engine *Engine
	clock  *checkClock
	seq    int64
	apply  func(id string, fact intake.Fact) intake.Result
}

// newRetroRig is a rig over a clone of its own, under the project's default steps with the design
// gate off, a one-hour linger and the tester as the role of the production check.
func newRetroRig(t *testing.T) *retroRig {
	t.Helper()
	pool := migratedPool(t)
	clock := &checkClock{now: retroOpened}
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateOff.Steps(), ReviewRoundCap: 3, Linger: time.Hour,
		ProductionCheckBound: 48 * time.Hour, ProductionCheckRole: claim.RoleTester, ReviewAppLogin: testReviewApp, Repositories: testRepositories,
		Clock: clock.Now}, nil)
	return &retroRig{pool: pool, engine: engine, clock: clock, seq: 10, apply: applyFacts(t, pool, engine)}
}

// at moves the rig's clock to d past retroOpened.
func (r *retroRig) at(d time.Duration) { r.clock.now = retroOpened.Add(d) }

// root seeds the tree's root LEGION-208 standing at at, generation 1, its open pull request #42 and
// its tree lifecycle under the workflow (stepRoot).
func (r *retroRig) root(t *testing.T, at phase.Phase) {
	t.Helper()
	stepRoot(t, r.pool, at)
}

// member seeds key, a member of LEGION-208's tree beneath parent, standing at at, generation 3,
// titled by its key, and its open pull request numbered pr (none for 0).
func (r *retroRig) member(t *testing.T, key, parent string, at phase.Phase, pr int) {
	t.Helper()
	seedIssue(t, r.pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: key, Parent: &parent, Phase: at, Generation: 3, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	if pr != 0 {
		seedPR(t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "sjawhar/legion", Number: pr,
			Branch: "legion/" + key, HeadSHA: "head-" + key, Failing: []string{}})
	}
}

// runTo starts key's first phase as the admission does (StartFirstPhase) and moves it on to at
// (walkTo).
func (r *retroRig) runTo(t *testing.T, key string, at phase.Phase) {
	t.Helper()
	firstStep(t, r.pool, r.engine, key)
	r.walkTo(t, key, at)
}

// walkTo moves key on through the default steps from where it stands until it stands in at:
// implementing, testing, reviewing, accepting, retro or merging. The move into retro is the
// owner's sign-off, which starts the retro's step row and queues the implementer's start of the
// retro.
func (r *retroRig) walkTo(t *testing.T, key string, at phase.Phase) {
	t.Helper()
	for _, step := range []struct {
		to      phase.Phase
		trigger TriggerKind
	}{
		{phase.Implementing, TriggerPlannerCompleted},
		{phase.Testing, TriggerImplementationReady},
		{phase.Reviewing, TriggerTesterPassed},
		{phase.Accepting, TriggerReviewApproved},
		{phase.Retro, TriggerSignOff},
		{phase.Merging, TriggerRetroCompleted},
	} {
		if issueAt(t, r.pool, key) == at {
			return
		}
		move(t, r.pool, r.engine, key, step.trigger, "", record.PhaseRow{})
		if got := issueAt(t, r.pool, key); got != step.to {
			t.Fatalf("the %s of %s left it in %s, want %s", step.trigger, key, got, step.to)
		}
	}
	issueStandsIn(t, r.pool, key, at)
}

// moved is a Dispatch event of key moving it to status, as the record's own copy of the issue
// spells it, at the rig's next sequence, written by session ("" for a person or the daemon).
func (r *retroRig) moved(t *testing.T, key, status, session string) intake.DispatchIssue {
	t.Helper()
	issue := issueRecord(t, r.pool, key)
	if issue == nil {
		t.Fatalf("%s is not recorded", key)
	}
	r.seq++
	fact := intake.DispatchIssue{Key: key, ResolvedRepo: "sjawhar/legion", Seq: r.seq, Type: "issue.updated", Status: status, Title: issue.Title,
		Rank: issue.Rank, ActorSession: session}
	if issue.Parent != nil {
		fact.Parent = *issue.Parent
	}
	return fact
}

// person is a person's move of key to status in the Dispatch dashboard, applied as the daemon
// applies it; a refusal fails the test.
func (r *retroRig) person(t *testing.T, key, status string) {
	t.Helper()
	fact := r.moved(t, key, status, "")
	if result := r.apply(fmt.Sprintf("%s-%s-%d", key, status, fact.Seq), fact); result.Refusal != nil {
		t.Fatalf("a person's %s on %s was refused: %+v", status, key, result.Refusal)
	}
}

// ownerSession is the session of the root's architect, which holds its claim in the tree, so its
// status write on a child is the owning architect's (owningArchitectsMove).
const ownerSession = "ses-root-architect"

// owner is the root architect's move of key to status, from its session (ownerSession, which
// seedOwner records).
func (r *retroRig) owner(t *testing.T, key, status string) {
	t.Helper()
	fact := r.moved(t, key, status, ownerSession)
	if result := r.apply(fmt.Sprintf("%s-%s-%d-by-owner", key, status, fact.Seq), fact); result.Refusal != nil {
		t.Fatalf("the owning architect's %s on %s was refused: %+v", status, key, result.Refusal)
	}
}

// seedOwner records the root architect's claim holding ownerSession, as its launch leaves it.
func (r *retroRig) seedOwner(t *testing.T) {
	t.Helper()
	seedRunClaim(t, r.pool, "LEGION-208", claim.RoleArchitect, 1, "working", "", ownerSession)
}

// report is the report of key's retro after its close, from the implementer's claim, on run
// generation, with a rating of the plan: the fact the completion route applies (intake.RetroReport).
func (r *retroRig) report(t *testing.T, key string, generation uint64) intake.Result {
	t.Helper()
	return r.apply(fmt.Sprintf("retro-after-close:%s:%d", key, generation), intake.RetroReport{Issue: key,
		Claim: claim.Token(treeRoleToken(t, key, claim.RoleImplementer)), Generation: generation,
		PlanRating: record.PlanRating{Rating: 4, Reason: "The plan put the tests first."}})
}

// expireLinger is the end of the root's linger at generation 1 coming due, as the outbox's
// linger_close row applies it.
func (r *retroRig) expireLinger(t *testing.T) {
	t.Helper()
	r.apply("linger:LEGION-208:1", intake.LingerExpired{Issue: "LEGION-208", Generation: 1})
}

// mark is the id of the newest outbox row, 0 for none: every row a later fact writes is newer.
func (r *retroRig) mark(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := r.pool.QueryRow(t.Context(), `select coalesce(max(id), 0) from outbox`).Scan(&id); err != nil {
		t.Fatalf("read the newest outbox row: %v", err)
	}
	return id
}

// issueStandsIn fails unless key stands in want.
func issueStandsIn(t *testing.T, pool *pgxpool.Pool, key string, want phase.Phase) {
	t.Helper()
	if got := issueAt(t, pool, key); got != want {
		t.Fatalf("%s stands in %s, want %s", key, got, want)
	}
}

// lingering fails unless key is a root that left the workflow and whose tree lingers.
func lingering(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	if issue := issueRecord(t, pool, key); issue == nil || issue.Phase != phase.Done || !issue.Lingers() {
		t.Fatalf("%s = %+v, want it done with its tree lingering", key, issue)
	}
}

// seedRunClaim records the claim of role on issue in LEGION-208's tree, serving run generation, in
// state (suspended where suspendedBy names who suspended it), holding session.
func seedRunClaim(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role, generation uint64, state, suspendedBy, session string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		suspended_by, serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', 'LEGION-208', $2, $3, 1, $4, '', $5, $6, $7, 0, 0, 0, 0)
		on conflict (token) do update set state = excluded.state, suspended_by = excluded.suspended_by, session = excluded.session,
		serving_generation = excluded.serving_generation`,
		treeRoleToken(t, issue, role), issue, string(role), session, state, suspendedBy, int64(generation)); err != nil {
		t.Fatalf("seed the %s claim of %s: %v", role, issue, err)
	}
}

// deliverRetro stands for the outbox having run the start of key's retro, which the move into
// retro queued: the start's row is gone, as the outbox deletes a row it ran, and the implementer's
// claim is working the retro of run generation. A retro whose start is still queued is one the
// outbox has not delivered (c20); this one is running (c13, s9).
func deliverRetro(t *testing.T, pool *pgxpool.Pool, key string, generation uint64) {
	t.Helper()
	tag, err := pool.Exec(t.Context(), `delete from outbox where kind = 'supervise' and issue = $1
		and payload->>'op' = 'start' and payload->>'phase' = 'retro'`, key)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("deliver the start of %s's retro: %d rows, %v; want its one queued start", key, tag.RowsAffected(), err)
	}
	seedRunClaim(t, pool, key, claim.RoleImplementer, generation, "working", "", "ses-implementer-"+key)
}

// rowsSince is issue's outbox rows of kind newer than mark, oldest first.
func rowsSince(t *testing.T, pool *pgxpool.Pool, issue, kind string, mark int64) []outboxRow {
	t.Helper()
	var got []outboxRow
	for _, row := range issueOutboxOf(t, pool, issue, kind) {
		if row.id > mark {
			got = append(got, row)
		}
	}
	return got
}

// supervisesSince decodes issue's supervise rows newer than mark, oldest first.
func supervisesSince(t *testing.T, pool *pgxpool.Pool, issue string, mark int64) []record.SuperviseRequest {
	t.Helper()
	var got []record.SuperviseRequest
	for _, row := range rowsSince(t, pool, issue, string(record.OutboxKindSupervise), mark) {
		got = append(got, decodeSupervise(t, row))
	}
	return got
}

// retroStartsOf is every start among requests that delivers a retro after a close: it names the
// run the retro is of (record.SuperviseRequest.RetroRun).
func retroStartsOf(requests []record.SuperviseRequest) []record.SuperviseRequest {
	var got []record.SuperviseRequest
	for _, request := range requests {
		if request.Op == "start" && request.RetroRun != 0 {
			got = append(got, request)
		}
	}
	return got
}

// phaseStartsOf is every start among requests of the phase at.
func phaseStartsOf(requests []record.SuperviseRequest, at phase.Phase) []record.SuperviseRequest {
	var got []record.SuperviseRequest
	for _, request := range requests {
		if request.Op == "start" && request.Phase == at {
			got = append(got, request)
		}
	}
	return got
}

// checkStartsOf is every start among requests of a production check's look.
func checkStartsOf(requests []record.SuperviseRequest) []record.SuperviseRequest {
	var got []record.SuperviseRequest
	for _, request := range requests {
		if request.Op == "start" && request.CheckLook != 0 {
			got = append(got, request)
		}
	}
	return got
}

// stopsOf is every row among requests that stops role's claim: a close of the issue, a suspend and
// a close of the tree.
func stopsOf(requests []record.SuperviseRequest, role claim.Role) []record.SuperviseRequest {
	var got []record.SuperviseRequest
	for _, request := range requests {
		if request.Role == role && (request.Op == "issue_close" || request.Op == "suspend" || request.Op == "tree_close") {
			got = append(got, request)
		}
	}
	return got
}

// retroStepsSince is the writes of issue's step records newer than mark that are of its retro: the
// starts, the ends, and every write carrying a rating of the plan, each in the order written.
func retroStepsSince(t *testing.T, pool *pgxpool.Pool, issue string, mark int64) (starts, ends, rated []record.StepRecord) {
	t.Helper()
	for _, row := range rowsSince(t, pool, issue, string(record.OutboxKindStepRecord), mark) {
		var write record.StepRecord
		if err := json.Unmarshal(row.payload, &write); err != nil {
			t.Fatalf("decode step record row %d of %s: %v", row.id, issue, err)
		}
		switch {
		case write.PlanRating != nil:
			rated = append(rated, write)
		case write.Kind != "retro":
		case isStart(write):
			starts = append(starts, write)
		case isEnd(write):
			ends = append(ends, write)
		}
	}
	return starts, ends, rated
}

// retroStartedAt is when the newest start of key's retro step row started, from the step records
// its outbox holds.
func retroStartedAt(t *testing.T, pool *pgxpool.Pool, key string) time.Time {
	t.Helper()
	starts, _, _ := retroStepsSince(t, pool, key, 0)
	if len(starts) == 0 || starts[len(starts)-1].StartedAt == nil {
		t.Fatalf("%s's step records hold no start of its retro: %+v", key, starts)
	}
	return *starts[len(starts)-1].StartedAt
}

// runRetroRow is one row of run_retros as SQL reads it: the record of one run of an issue, the one
// source of whether the run started, whether it had its retro, and of the retro after its close.
type runRetroRow struct {
	generation                               uint64
	tree                                     string
	startedAt                                time.Time
	retroStartedAt, openedAt, boundAt, ended *time.Time
	agent                                    string
	entry                                    int64
	outcome                                  record.RetroOutcome
	endedBy                                  record.RetroEndCause
}

// open is whether the row holds a retro after a close that has not ended.
func (row runRetroRow) open() bool { return row.openedAt != nil && row.ended == nil }

// runRetrosOf reads issue's rows of run_retros, newest run first. It reads the table through SQL,
// not through the record's methods, so a row is judged by what the database holds.
func runRetrosOf(t *testing.T, pool *pgxpool.Pool, issue string) []runRetroRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select generation, tree, started_at, retro_started_at, opened_at, bound_at, agent, entry, ended_at, outcome, ended_by
		from run_retros where issue = $1 order by generation desc`, issue)
	if err != nil {
		t.Fatalf("read the run_retros rows of %s: %v", issue, err)
	}
	defer rows.Close()
	var got []runRetroRow
	for rows.Next() {
		var row runRetroRow
		var generation int64
		if err := rows.Scan(&generation, &row.tree, &row.startedAt, &row.retroStartedAt, &row.openedAt, &row.boundAt, &row.agent, &row.entry, &row.ended,
			&row.outcome, &row.endedBy); err != nil {
			t.Fatalf("scan a run_retros row of %s: %v", issue, err)
		}
		row.generation = uint64(generation)
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the run_retros rows of %s: %v", issue, err)
	}
	return got
}

// runRetroOf is issue's row of run generation, and whether run_retros holds one.
func runRetroOf(t *testing.T, pool *pgxpool.Pool, issue string, generation uint64) (runRetroRow, bool) {
	t.Helper()
	for _, row := range runRetrosOf(t, pool, issue) {
		if row.generation == generation {
			return row, true
		}
	}
	return runRetroRow{}, false
}

// assertOneRetroStart fails unless the rows since mark hold exactly one start of key's retro after
// its close, and no start of the retro phase: the implementer's (the retro's agent on main), naming
// run, no phase, no entry and no look, at the issue's generation as it stands, whose task opens as
// every retro task does (taskOpening at the retro: the working copy line, which a rig that places
// no working copy writes naming the branch alone, then the continue line). It answers that start.
func assertOneRetroStart(t *testing.T, pool *pgxpool.Pool, key string, run uint64, mark int64) record.SuperviseRequest {
	t.Helper()
	issue := issueRecord(t, pool, key)
	supervises := supervisesSince(t, pool, key, mark)
	starts := retroStartsOf(supervises)
	if len(starts) != 1 {
		t.Fatalf("starts of %s's retro after its close = %+v, want exactly one, of run %d", key, starts, run)
	}
	start := starts[0]
	if start.Role != claim.RoleImplementer || start.RetroRun != run || start.Phase != "" || start.Entry != 0 || start.CheckLook != 0 ||
		start.RetroWake || start.WakeKind != "" || start.News || start.NeedsSlot || start.Tree != issue.Tree || start.Generation != issue.Generation {
		t.Fatalf("the start of %s's retro after its close = %+v, want the implementer's, of run %d, with no phase, entry or look, at generation %d of tree %s",
			key, start, run, issue.Generation, issue.Tree)
	}
	retro := *issue
	retro.Phase = phase.Retro
	if lead := taskOpening(retro, WorkingCopy(retro, nil)); !strings.HasPrefix(start.Task, lead) {
		t.Fatalf("the task of %s's retro after its close = %q, want it to open %q", key, start.Task, lead)
	}
	if phased := phaseStartsOf(supervises, phase.Retro); len(phased) != 0 {
		t.Fatalf("starts of %s's retro phase after its close = %+v, want none: the retro after a close names no phase", key, phased)
	}
	return start
}

// assertSpared fails unless nothing since mark stops a claim of roles on key, and no IssueSuspend
// of key is queued since mark: the close keeps the retro's agent, and the pod it works in, with
// any role it spares beside it.
func assertSpared(t *testing.T, pool *pgxpool.Pool, key string, mark int64, roles ...claim.Role) {
	t.Helper()
	supervises := supervisesSince(t, pool, key, mark)
	for _, role := range roles {
		if stops := stopsOf(supervises, role); len(stops) != 0 {
			t.Fatalf("the close of %s stopped its %s: %+v, want the claim spared", key, role, stops)
		}
	}
	if got := rowsSince(t, pool, key, string(record.OutboxKindIssueSuspend), mark); len(got) != 0 {
		t.Fatalf("issue_suspend rows of %s since its close = %d, want none while its spared claims hold the pod", key, len(got))
	}
}

// assertRunRetroOpen fails unless the row of key's run holds a retro after its close that is open,
// run by agent and bound at bound, and the bound row is queued for that run at that moment.
func assertRunRetroOpen(t *testing.T, pool *pgxpool.Pool, key string, run uint64, agent string, bound time.Time, mark int64) {
	t.Helper()
	bounds := rowsSince(t, pool, key, string(record.OutboxKindRetroBound), mark)
	if len(bounds) != 1 || !bounds[0].nextAt.Equal(bound) {
		t.Fatalf("retro_bound rows of %s since its close = %+v, want one due at %s", key, bounds, bound)
	}
	var payload record.RetroBound
	if err := json.Unmarshal(bounds[0].payload, &payload); err != nil || payload.Generation != run {
		t.Fatalf("the retro_bound row of %s = %s (%v), want it of run %d", key, bounds[0].payload, err, run)
	}
	row, found := runRetroOf(t, pool, key, run)
	if !found || !row.open() || row.boundAt == nil || !row.boundAt.Equal(bound) || row.agent != agent {
		t.Fatalf("the run_retros row of %s run %d = %+v (found %t), want a retro after its close open, by %s, bound at %s", key, run, row, found, agent, bound)
	}
}

// assertRetroOwed fails unless the close since mark owes key the retro of run and starts it: one
// start of it (assertOneRetroStart), one start of the retro's step row under the implementer's
// claim at run, the implementer spared with no IssueSuspend (assertSpared), and the run's record
// open, bound 24 hours after the step row's start (assertRunRetroOpen). It answers the start.
func assertRetroOwed(t *testing.T, pool *pgxpool.Pool, key string, run uint64, mark int64) record.SuperviseRequest {
	t.Helper()
	start := assertOneRetroStart(t, pool, key, run, mark)
	agent := treeRoleToken(t, key, claim.RoleImplementer)
	steps, ends, _ := retroStepsSince(t, pool, key, mark)
	if len(steps) != 1 || steps[0].Agent != agent || steps[0].Generation != run || steps[0].StartedAt == nil || len(ends) != 0 {
		t.Fatalf("the retro step rows of %s since its close: starts %+v, ends %+v; want one start, by %s, of run %d, and no end", key, steps, ends, agent, run)
	}
	assertSpared(t, pool, key, mark, claim.RoleImplementer)
	assertRunRetroOpen(t, pool, key, run, agent, steps[0].StartedAt.Add(RetroAfterCloseBound), mark)
	return start
}

// assertNoRetroQueued fails unless nothing since mark starts a retro of key: no start of a retro
// after a close, no start of the retro phase, no start of a retro step row and no bound row.
func assertNoRetroQueued(t *testing.T, pool *pgxpool.Pool, key string, mark int64) {
	t.Helper()
	supervises := supervisesSince(t, pool, key, mark)
	if starts := retroStartsOf(supervises); len(starts) != 0 {
		t.Fatalf("starts of a retro after the close of %s = %+v, want none", key, starts)
	}
	if starts := phaseStartsOf(supervises, phase.Retro); len(starts) != 0 {
		t.Fatalf("starts of %s's retro phase since its close = %+v, want none", key, starts)
	}
	if steps, _, _ := retroStepsSince(t, pool, key, mark); len(steps) != 0 {
		t.Fatalf("retro step starts of %s since its close = %+v, want none", key, steps)
	}
	if bounds := rowsSince(t, pool, key, string(record.OutboxKindRetroBound), mark); len(bounds) != 0 {
		t.Fatalf("retro_bound rows of %s since its close = %d, want none", key, len(bounds))
	}
}

// assertClosedAsToday fails unless the close since mark stopped key's implementer stops times and
// queued suspensions IssueSuspend rows: what a close that owes no retro does, as it did before.
func assertClosedAsToday(t *testing.T, pool *pgxpool.Pool, key string, mark int64, stops, suspensions int) {
	t.Helper()
	if got := stopsOf(supervisesSince(t, pool, key, mark), claim.RoleImplementer); len(got) != stops {
		t.Fatalf("stops of %s's implementer since its close = %+v, want %d", key, got, stops)
	}
	if got := rowsSince(t, pool, key, string(record.OutboxKindIssueSuspend), mark); len(got) != suspensions {
		t.Fatalf("issue_suspend rows of %s since its close = %d, want %d", key, len(got), suspensions)
	}
}

// assertRetroHad fails unless key's newest run is run and its record says the retro started in it
// before the close, and holds no retro after a close.
func assertRetroHad(t *testing.T, pool *pgxpool.Pool, key string, run uint64) {
	t.Helper()
	rows := runRetrosOf(t, pool, key)
	if len(rows) == 0 || rows[0].generation != run || rows[0].retroStartedAt == nil || rows[0].openedAt != nil {
		t.Fatalf("the run_retros rows of %s = %+v, want run %d newest, its retro started, and no retro after a close", key, rows, run)
	}
}

// assertNeverStarted fails unless run_retros holds no row of key: no run of it ever started.
func assertNeverStarted(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	if rows := runRetrosOf(t, pool, key); len(rows) != 0 {
		t.Fatalf("the run_retros rows of %s = %+v, want none: no run of it started", key, rows)
	}
}

// closedInItsRetro is row c13: a child closed by a person while its retro runs keeps that retro's
// turn, its step row and its claim, the close starts nothing of the retro, and the run's record
// opens on the running retro with the implementer's retro entry, on which its completion is taken.
// newsQueued first queues a start of the implementer for news alone at the retro, as the news of
// an approved design queues one (design_news.go): that start brings the claim no work of the
// retro, so the close still reads the retro as running rather than queued, and opens no second.
func closedInItsRetro(t *testing.T, newsQueued bool) {
	t.Helper()
	r := newRetroRig(t)
	r.root(t, phase.Implementing)
	r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
	r.runTo(t, "LEGION-209", phase.Retro)
	deliverRetro(t, r.pool, "LEGION-209", 3)
	if newsQueued {
		seedOutboxRow(t, r.pool, "LEGION-209", record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer,
			Generation: 3, Phase: phase.Retro, News: true})
	}
	started := retroStartedAt(t, r.pool, "LEGION-209")
	r.at(2 * time.Hour)
	mark := r.mark(t)
	r.person(t, "LEGION-209", "done")
	issueStandsIn(t, r.pool, "LEGION-209", phase.Done)
	supervises := supervisesSince(t, r.pool, "LEGION-209", mark)
	if starts := append(retroStartsOf(supervises), phaseStartsOf(supervises, phase.Retro)...); len(starts) != 0 {
		t.Fatalf("starts of the child's retro since its close = %+v, want none: the running retro keeps its turn", starts)
	}
	if starts, ends, _ := retroStepsSince(t, r.pool, "LEGION-209", mark); len(starts) != 0 || len(ends) != 0 {
		t.Fatalf("the running retro's step row at the close: starts %+v, ends %+v; want neither: the retro keeps its turn and its row", starts, ends)
	}
	assertSpared(t, r.pool, "LEGION-209", mark, claim.RoleImplementer)
	assertRunRetroOpen(t, r.pool, "LEGION-209", 3, treeRoleToken(t, "LEGION-209", claim.RoleImplementer), started.Add(RetroAfterCloseBound), mark)
	var entry int64
	if err := r.pool.QueryRow(t.Context(), `select entry from phases where issue = 'LEGION-209' and role = 'implementer'`).Scan(&entry); err != nil {
		t.Fatalf("read the implementer's phases row of the child: %v", err)
	}
	if row, _ := runRetroOf(t, r.pool, "LEGION-209", 3); entry == 0 || row.entry != entry {
		t.Fatalf("the run_retros row of the child names entry %d, want the implementer's retro entry %d: the running retro's completion is taken on it",
			row.entry, entry)
	}
}

// Every way an issue reaches done today, each a row of the inventory of INFRA-5602's plan (R1 and
// R3 are over the class of closes): a close of a run that started — by the issue's own first phase
// or by the first phase of an issue beneath it — whose retro never started owes that run its retro,
// and the close starts it after the fact, as a production check's look starts after a merge. The
// start is the implementer's (the agent of the retro on main), names the run and no phase, and
// carries the retro's task; the implementer's claim is spared, with any checking role, so no
// IssueSuspend takes the pod; the retro's step row starts under that claim, and the run's record
// opens the retro with its bound, 24 hours after that start. A close of a run that had its retro —
// one that passed, one that moved the work back, one after an earlier close of the run — owes
// none, nor does a close of an issue Legion never started; such a close stops the claims as it
// did before. An issue parked in backlog, icebox or triage and then closed gets its retro at the
// close, never at the park. A retro running at the close keeps its turn and its step row; one whose
// start is queued and not delivered goes out with no phase on the same step row.
func TestEveryCloseOfAStartedRunWithNoRetroStartsItsRetro(t *testing.T) {
	t.Parallel()
	parks := []string{"backlog", "icebox", "triage"}
	type row struct {
		name string
		run  func(t *testing.T)
	}
	rows := []row{
		{"c1 a child in implementing, its owning architect's done", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Implementing)
			r.seedOwner(t)
			mark := r.mark(t)
			r.owner(t, "LEGION-209", "done")
			issueStandsIn(t, r.pool, "LEGION-209", phase.Done)
			assertRetroOwed(t, r.pool, "LEGION-209", 3, mark)
		}},
		{"c2 a child in testing, a person's done", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Testing)
			mark := r.mark(t)
			r.person(t, "LEGION-209", "done")
			issueStandsIn(t, r.pool, "LEGION-209", phase.Done)
			assertRetroOwed(t, r.pool, "LEGION-209", 3, mark)
		}},
		{"c3 a running root after its first phase, a person's done", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Admitted)
			r.runTo(t, "LEGION-208", phase.Implementing)
			mark := r.mark(t)
			r.person(t, "LEGION-208", "done")
			lingering(t, r.pool, "LEGION-208")
			assertRetroOwed(t, r.pool, "LEGION-208", 1, mark)
		}},
	}
	for _, park := range parks {
		rows = append(rows,
			row{"c4 a child moved to " + park + ", then a person's done", func(t *testing.T) {
				r := newRetroRig(t)
				r.root(t, phase.Implementing)
				r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
				r.runTo(t, "LEGION-209", phase.Implementing)
				parked := r.mark(t)
				r.person(t, "LEGION-209", park)
				issueStandsIn(t, r.pool, "LEGION-209", phase.Done)
				assertNoRetroQueued(t, r.pool, "LEGION-209", parked)
				mark := r.mark(t)
				r.person(t, "LEGION-209", "done")
				assertRetroOwed(t, r.pool, "LEGION-209", 3, mark)
			}},
			row{"c5 a root moved to " + park + ", then a person's done inside its linger", func(t *testing.T) {
				r := newRetroRig(t)
				r.root(t, phase.Admitted)
				r.runTo(t, "LEGION-208", phase.Implementing)
				r.person(t, "LEGION-208", park)
				lingering(t, r.pool, "LEGION-208")
				mark := r.mark(t)
				r.person(t, "LEGION-208", "done")
				assertRetroOwed(t, r.pool, "LEGION-208", 1, mark)
			}},
			row{"c6 a root moved to " + park + ", its linger expired, then a person's done", func(t *testing.T) {
				r := newRetroRig(t)
				r.root(t, phase.Admitted)
				r.runTo(t, "LEGION-208", phase.Implementing)
				r.person(t, "LEGION-208", park)
				r.at(2 * time.Hour)
				r.expireLinger(t)
				if closes := superviseRequests(t, r.pool, "tree_close"); closes["LEGION-208/implementer"] != 1 {
					t.Fatalf("tree_close rows after the linger's end = %v, want the root's implementer retired", closes)
				}
				mark := r.mark(t)
				r.person(t, "LEGION-208", "done")
				assertRetroOwed(t, r.pool, "LEGION-208", 1, mark)
			}},
			row{"c24 a child of a tree whose root moved to " + park + " and whose linger expired, a person's done", func(t *testing.T) {
				r := newRetroRig(t)
				r.root(t, phase.Implementing)
				r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
				r.runTo(t, "LEGION-209", phase.Implementing)
				r.person(t, "LEGION-208", park)
				r.at(2 * time.Hour)
				r.expireLinger(t)
				if closes := superviseRequests(t, r.pool, "tree_close"); closes["LEGION-209/implementer"] != 1 {
					t.Fatalf("tree_close rows after the linger's end = %v, want the child's implementer retired", closes)
				}
				issueStandsIn(t, r.pool, "LEGION-209", phase.Implementing)
				mark := r.mark(t)
				r.person(t, "LEGION-209", "done")
				assertRetroOwed(t, r.pool, "LEGION-209", 3, mark)
			}},
		)
	}
	rows = append(rows,
		row{"c7 a child of a lingering tree, a person's done", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Implementing)
			r.person(t, "LEGION-208", "done")
			lingering(t, r.pool, "LEGION-208")
			issueStandsIn(t, r.pool, "LEGION-209", phase.Implementing)
			mark := r.mark(t)
			r.person(t, "LEGION-209", "done")
			assertRetroOwed(t, r.pool, "LEGION-209", 3, mark)
		}},
		row{"c8 a parent held before its retro for its children, a person's done", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Admitted)
			seedStepsPlan(t, r.pool, "LEGION-208", 1, "implementer", "reviewer")
			r.member(t, heldParentChild, "LEGION-208", phase.Implementing, 43)
			move(t, r.pool, r.engine, "LEGION-208", TriggerGateOpened, "", record.PhaseRow{})
			move(t, r.pool, r.engine, "LEGION-208", TriggerImplementationReady, "", record.PhaseRow{})
			approveReview(t, r.pool, r.engine)
			if held := issueRecord(t, r.pool, "LEGION-208"); !record.HeldForChildren(*held) {
				t.Fatalf("the parent = %+v, want it held before its retro for its children", held)
			}
			mark := r.mark(t)
			r.person(t, "LEGION-208", "done")
			assertRetroOwed(t, r.pool, "LEGION-208", 1, mark)
		}},
		row{"c9 a parent that never started a phase of its own, whose child started, a person's done", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 0)
			r.member(t, "LEGION-210", "LEGION-209", phase.Admitted, 44)
			r.runTo(t, "LEGION-210", phase.Planning)
			issueStandsIn(t, r.pool, "LEGION-209", phase.Admitted)
			if starts := roleStartsOf(t, r.pool, "LEGION-209", claim.RoleImplementer); len(starts) != 0 {
				t.Fatalf("starts of the parent's implementer = %+v, want none: it never launched", starts)
			}
			var claims int
			if err := r.pool.QueryRow(t.Context(), `select count(*) from claims where token = $1`,
				treeRoleToken(t, "LEGION-209", claim.RoleImplementer)).Scan(&claims); err != nil || claims != 0 {
				t.Fatalf("claims of the parent's implementer = %d, %v; want none", claims, err)
			}
			mark := r.mark(t)
			r.person(t, "LEGION-209", "done")
			assertRetroOwed(t, r.pool, "LEGION-209", 3, mark)
		}},
		row{"c10 close_root on a root in admitted whose child started and closed", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Admitted)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Planning)
			r.person(t, "LEGION-209", "done")
			issueStandsIn(t, r.pool, "LEGION-209", phase.Done)
			issueStandsIn(t, r.pool, "LEGION-208", phase.Admitted)
			mark := r.mark(t)
			if result := r.apply("close-root", intake.CloseRoot{Issue: "LEGION-208", Reason: "the person decided the tree is done"}); result.Refusal != nil {
				t.Fatalf("close_root was refused: %+v", result.Refusal)
			}
			lingering(t, r.pool, "LEGION-208")
			assertRetroOwed(t, r.pool, "LEGION-208", 1, mark)
		}},
		row{"c11 a member of a lingering tree whose pull request merged early, a person's done", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Implementing)
			r.person(t, "LEGION-208", "done")
			lingering(t, r.pool, "LEGION-208")
			r.apply("merged-43", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
			issueStandsIn(t, r.pool, "LEGION-209", phase.Implementing)
			if notices := noticesOf(t, r.pool, "LEGION-209", "pr-merged"); len(notices) != 1 {
				t.Fatalf("pr-merged notices of the member = %+v, want the one the early merge sends", notices)
			}
			mark := r.mark(t)
			r.person(t, "LEGION-209", "done")
			assertRetroOwed(t, r.pool, "LEGION-209", 3, mark)
			if checks := checkStartsOf(supervisesSince(t, r.pool, "LEGION-209", mark)); len(checks) != 1 || checks[0].Role != claim.RoleTester {
				t.Fatalf("starts of the member's check = %+v, want the tester's look beside the retro", checks)
			}
			assertSpared(t, r.pool, "LEGION-209", mark, claim.RoleTester)
		}},
		row{"c12 a child in testing whose pull request merged, its architect's done", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Testing)
			r.apply("merged-43", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
			issueStandsIn(t, r.pool, "LEGION-209", phase.Testing)
			r.seedOwner(t)
			mark := r.mark(t)
			r.owner(t, "LEGION-209", "done")
			assertRetroOwed(t, r.pool, "LEGION-209", 3, mark)
			if checks := checkStartsOf(supervisesSince(t, r.pool, "LEGION-209", mark)); len(checks) != 1 || checks[0].Role != claim.RoleTester {
				t.Fatalf("starts of the child's check = %+v, want the tester's look beside the retro", checks)
			}
			assertSpared(t, r.pool, "LEGION-209", mark, claim.RoleTester, claim.RoleImplementer)
		}},
		row{"c13 an issue in retro, a person's done", func(t *testing.T) { closedInItsRetro(t, false) }},
		row{"c13 an issue in retro with a start of the implementer queued for news alone, a person's done", func(t *testing.T) {
			closedInItsRetro(t, true)
		}},
		row{"c14 an issue closed by its merge from merging", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Merging)
			mark := r.mark(t)
			r.apply("merged-43", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
			issueStandsIn(t, r.pool, "LEGION-209", phase.Done)
			assertNoRetroQueued(t, r.pool, "LEGION-209", mark)
			assertClosedAsToday(t, r.pool, "LEGION-209", mark, 1, 0)
			assertRetroHad(t, r.pool, "LEGION-209", 3)
		}},
		row{"c15 a merged parent that closes when the last issue beneath it leaves", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Merging)
			r.member(t, "LEGION-210", "LEGION-209", phase.Implementing, 0)
			r.apply("merged-43", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
			issueStandsIn(t, r.pool, "LEGION-209", phase.Merging)
			mark := r.mark(t)
			r.person(t, "LEGION-210", "done")
			issueStandsIn(t, r.pool, "LEGION-209", phase.Done)
			assertNoRetroQueued(t, r.pool, "LEGION-209", mark)
			assertClosedAsToday(t, r.pool, "LEGION-209", mark, 1, 0)
			assertRetroHad(t, r.pool, "LEGION-209", 3)
		}},
		row{"c16 a retro passed, the merger returned the work, then a person's done", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Merging)
			move(t, r.pool, r.engine, "LEGION-209", TriggerBackward, phase.Implementing, record.PhaseRow{})
			issueStandsIn(t, r.pool, "LEGION-209", phase.Implementing)
			mark := r.mark(t)
			r.person(t, "LEGION-209", "done")
			assertNoRetroQueued(t, r.pool, "LEGION-209", mark)
			assertClosedAsToday(t, r.pool, "LEGION-209", mark, 1, 1)
			assertRetroHad(t, r.pool, "LEGION-209", 3)
		}},
		row{"c17 a child closed before its first phase", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			mark := r.mark(t)
			r.person(t, "LEGION-209", "done")
			issueStandsIn(t, r.pool, "LEGION-209", phase.Done)
			assertNoRetroQueued(t, r.pool, "LEGION-209", mark)
			assertClosedAsToday(t, r.pool, "LEGION-209", mark, 1, 1)
			assertNeverStarted(t, r.pool, "LEGION-209")
		}},
		row{"c17 a root close_root'd in admitted with nothing started beneath", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Admitted)
			mark := r.mark(t)
			if result := r.apply("close-root", intake.CloseRoot{Issue: "LEGION-208", Reason: "the person decided no change is needed"}); result.Refusal != nil {
				t.Fatalf("close_root was refused: %+v", result.Refusal)
			}
			lingering(t, r.pool, "LEGION-208")
			assertNoRetroQueued(t, r.pool, "LEGION-208", mark)
			assertClosedAsToday(t, r.pool, "LEGION-208", mark, 1, 1)
			assertNeverStarted(t, r.pool, "LEGION-208")
		}},
		row{"c18 a child set back to todo and closed before run 2's first phase, run 1 with no retro row", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Implementing)
			r.person(t, "LEGION-209", "backlog")
			r.person(t, "LEGION-209", "todo")
			if child := issueRecord(t, r.pool, "LEGION-209"); child.Phase != phase.Admitted || child.Generation != 4 {
				t.Fatalf("the child set back to todo = %+v, want run 2 (generation 4) at admitted", child)
			}
			mark := r.mark(t)
			r.person(t, "LEGION-209", "done")
			assertRetroOwed(t, r.pool, "LEGION-209", 3, mark)
		}},
		row{"c18 a child set back to todo and closed before run 2's first phase, a retro row of run 1 started", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Retro)
			r.person(t, "LEGION-209", "backlog")
			r.person(t, "LEGION-209", "todo")
			if child := issueRecord(t, r.pool, "LEGION-209"); child.Phase != phase.Admitted || child.Generation != 4 {
				t.Fatalf("the child set back to todo = %+v, want run 2 (generation 4) at admitted", child)
			}
			mark := r.mark(t)
			r.person(t, "LEGION-209", "done")
			assertNoRetroQueued(t, r.pool, "LEGION-209", mark)
			assertClosedAsToday(t, r.pool, "LEGION-209", mark, 1, 1)
			assertRetroHad(t, r.pool, "LEGION-209", 3)
		}},
		row{"c18 a child set back to todo and closed before run 2's first phase, run 1 had a retro after its close", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Implementing)
			r.person(t, "LEGION-209", "done")
			r.person(t, "LEGION-209", "todo")
			if child := issueRecord(t, r.pool, "LEGION-209"); child.Phase != phase.Admitted || child.Generation != 4 {
				t.Fatalf("the child set back to todo = %+v, want run 2 (generation 4) at admitted", child)
			}
			mark := r.mark(t)
			r.person(t, "LEGION-209", "done")
			assertNoRetroQueued(t, r.pool, "LEGION-209", mark)
			assertClosedAsToday(t, r.pool, "LEGION-209", mark, 1, 1)
			rows := runRetrosOf(t, r.pool, "LEGION-209")
			if len(rows) != 1 || rows[0].generation != 3 || rows[0].openedAt == nil || rows[0].ended == nil {
				t.Fatalf("the run_retros rows of the child = %+v, want run 1 (generation 3) alone, its retro after the close opened and ended", rows)
			}
		}},
		row{"c19 a waiting root that leaves while the operator runs a tree under its key", func(t *testing.T) {
			r := newRetroRig(t)
			seedIssue(t, r.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
				Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			seedOperatorTree(t, r.pool, "LEGION-208")
			mark := r.mark(t)
			r.person(t, "LEGION-208", "done")
			issueStandsIn(t, r.pool, "LEGION-208", phase.Done)
			assertNoRetroQueued(t, r.pool, "LEGION-208", mark)
			assertClosedAsToday(t, r.pool, "LEGION-208", mark, 0, 0)
			assertNeverStarted(t, r.pool, "LEGION-208")
		}},
		row{"c20 a retro start queued and not yet delivered, a person's done", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Retro)
			if queued := phaseStartsOf(superviseRowsOf(t, r.pool, "LEGION-209", "start"), phase.Retro); len(queued) != 1 {
				t.Fatalf("queued starts of the retro = %+v, want the one the move into retro queued", queued)
			}
			started := retroStartedAt(t, r.pool, "LEGION-209")
			r.at(2 * time.Hour)
			mark := r.mark(t)
			r.person(t, "LEGION-209", "done")
			assertOneRetroStart(t, r.pool, "LEGION-209", 3, mark)
			if starts, ends, _ := retroStepsSince(t, r.pool, "LEGION-209", mark); len(starts) != 0 || len(ends) != 0 {
				t.Fatalf("the retro step rows of the child since its close: starts %+v, ends %+v; want neither: the task goes out on the row already started", starts, ends)
			}
			agent := treeRoleToken(t, "LEGION-209", claim.RoleImplementer)
			assertSpared(t, r.pool, "LEGION-209", mark, claim.RoleImplementer)
			assertRunRetroOpen(t, r.pool, "LEGION-209", 3, agent, started.Add(RetroAfterCloseBound), mark)
		}},
		row{"c23 a member of a lingering tree in merging whose pull request merges", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Merging)
			r.person(t, "LEGION-208", "done")
			lingering(t, r.pool, "LEGION-208")
			mark := r.mark(t)
			r.apply("merged-43", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
			issueStandsIn(t, r.pool, "LEGION-209", phase.Done)
			assertNoRetroQueued(t, r.pool, "LEGION-209", mark)
			assertClosedAsToday(t, r.pool, "LEGION-209", mark, 0, 0)
			assertRetroHad(t, r.pool, "LEGION-209", 3)
		}},
		row{"c26 an issue in merging with an open pull request, a person's done", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Merging)
			mark := r.mark(t)
			r.person(t, "LEGION-209", "done")
			assertNoRetroQueued(t, r.pool, "LEGION-209", mark)
			assertClosedAsToday(t, r.pool, "LEGION-209", mark, 1, 1)
			assertRetroHad(t, r.pool, "LEGION-209", 3)
		}},
		row{"c27 an issue whose retro moved the work back, then a person's done in implementing", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Retro)
			move(t, r.pool, r.engine, "LEGION-209", TriggerBackward, phase.Implementing, record.PhaseRow{})
			issueStandsIn(t, r.pool, "LEGION-209", phase.Implementing)
			if _, ends, _ := retroStepsSince(t, r.pool, "LEGION-209", 0); len(ends) != 1 || resultOf(ends[0]) != string(ResultFailed) {
				t.Fatalf("ends of the retro's step row = %+v, want the one the move back ended failed", ends)
			}
			mark := r.mark(t)
			r.person(t, "LEGION-209", "done")
			assertNoRetroQueued(t, r.pool, "LEGION-209", mark)
			assertClosedAsToday(t, r.pool, "LEGION-209", mark, 1, 1)
			assertRetroHad(t, r.pool, "LEGION-209", 3)
		}},
	)
	for _, end := range []struct {
		name    string
		outcome record.RetroOutcome
		end     func(t *testing.T, r *retroRig)
	}{
		{"passed", record.RetroOutcomePassed, func(t *testing.T, r *retroRig) {
			if result := r.report(t, "LEGION-209", 3); result.Refusal != nil {
				t.Fatalf("the retro's report was refused: %+v", result.Refusal)
			}
		}},
		{"stopped at its bound", record.RetroOutcomeStopped, func(t *testing.T, r *retroRig) {
			r.at(RetroAfterCloseBound + time.Hour)
			r.apply("retro-bound:LEGION-209:3", intake.RetroBoundEnded{Issue: "LEGION-209", Generation: 3})
		}},
	} {
		rows = append(rows, row{"s13 a retro after a close that ended " + end.name + ", then a move to backlog and a done again", func(t *testing.T) {
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			r.runTo(t, "LEGION-209", phase.Implementing)
			r.person(t, "LEGION-209", "done")
			end.end(t, r)
			r.person(t, "LEGION-209", "backlog")
			mark := r.mark(t)
			r.person(t, "LEGION-209", "done")
			assertNoRetroQueued(t, r.pool, "LEGION-209", mark)
			assertClosedAsToday(t, r.pool, "LEGION-209", mark, 1, 1)
			rows := runRetrosOf(t, r.pool, "LEGION-209")
			if len(rows) != 1 || rows[0].openedAt == nil || rows[0].ended == nil || rows[0].outcome != end.outcome {
				t.Fatalf("the run_retros rows of the child = %+v, want its one retro after a close, ended %s", rows, end.outcome)
			}
		}})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

// The start of a retro after a close is the row the outbox delivers to the retro's agent, as a
// production check's look is: a start of the implementer that names the run of the retro and no
// phase, no entry, no look, no wake and no slot, which the record's own decode takes (the outbox
// reads every row through it), and whose task opens as the retro's; and the retro's step row has
// its start, under that claim, written with it. The delivery itself is the outbox's
// (internal/daemon).
func TestTheStartOfARetroAfterACloseNamesNoPhaseAndItsRun(t *testing.T) {
	t.Parallel()
	r := newRetroRig(t)
	r.root(t, phase.Implementing)
	r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
	r.runTo(t, "LEGION-209", phase.Implementing)
	mark := r.mark(t)
	r.person(t, "LEGION-209", "done")

	var delivered []outboxRow
	for _, row := range rowsSince(t, r.pool, "LEGION-209", string(record.OutboxKindSupervise), mark) {
		if request := decodeSupervise(t, row); request.Op == "start" && request.RetroRun != 0 {
			delivered = append(delivered, row)
		}
	}
	if len(delivered) != 1 {
		t.Fatalf("starts of the child's retro after its close = %d, want the one the outbox delivers", len(delivered))
	}
	decoded, err := record.DecodeOutboxPayload(record.OutboxRow{ID: delivered[0].id, Kind: record.OutboxKindSupervise, Issue: "LEGION-209", Payload: delivered[0].payload})
	if err != nil {
		t.Fatalf("the record's decode of the retro's start: %v", err)
	}
	start, ok := decoded.(record.SuperviseRequest)
	if !ok {
		t.Fatalf("the retro's start decodes to %T, want a supervise request", decoded)
	}
	if start.Op != "start" || start.Role != claim.RoleImplementer || start.Tree != "LEGION-208" || start.Generation != 3 || start.RetroRun != 3 ||
		start.Phase != "" || start.Entry != 0 || start.CheckLook != 0 || !start.CheckOpenedAt.IsZero() || start.WakeKind != "" || start.RetroWake ||
		start.News || start.NeedsSlot || start.Task == "" {
		t.Fatalf("the retro's start = %+v, want the implementer's of run 3 at generation 3 with its task and no phase, entry, look, wake or slot", start)
	}
	steps, _, _ := retroStepsSince(t, r.pool, "LEGION-209", mark)
	agent := treeRoleToken(t, "LEGION-209", claim.RoleImplementer)
	if len(steps) != 1 || steps[0].Agent != agent || steps[0].Generation != 3 || steps[0].StartedAt == nil || !steps[0].StartedAt.Equal(retroOpened) {
		t.Fatalf("the retro step starts written with it = %+v, want one, by %s, of run 3, started at the close", steps, agent)
	}
}

// The report of a retro after a close (intake.RetroReport, the completion route's fact) from the
// claim that serves it ends the retro's step row passed and writes the plan's rating onto that
// run's record, as a retro's completion does, and writes no status: the issue stays done, as the
// close left it. The run's record ends passed, by the report. The retro's agent is done with the
// issue, so its claim is closed; the pod goes with it (an IssueSuspend) where nothing else of the
// issue holds it, and stays where a production check of the issue is still open, whose close
// writes the suspension.
func TestTheReportOfARetroAfterACloseEndsItsRowAndRatesThePlan(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// checked runs the child to testing and merges its pull request early, so its architect's
		// done opens its production check beside the retro (c12).
		checked     bool
		suspensions int
	}{
		{name: "nothing else of the issue is spared", suspensions: 1},
		{name: "the tester's production check is still open", checked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRetroRig(t)
			r.root(t, phase.Implementing)
			r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
			if tc.checked {
				r.runTo(t, "LEGION-209", phase.Testing)
				r.apply("merged-43", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
			} else {
				r.runTo(t, "LEGION-209", phase.Implementing)
			}
			r.person(t, "LEGION-209", "done")
			before := issueRecord(t, r.pool, "LEGION-209")
			mark := r.mark(t)
			if result := r.report(t, "LEGION-209", 3); result.Refusal != nil {
				t.Fatalf("the retro's report was refused: %+v", result.Refusal)
			}

			agent := treeRoleToken(t, "LEGION-209", claim.RoleImplementer)
			_, ends, rated := retroStepsSince(t, r.pool, "LEGION-209", mark)
			if len(ends) != 1 || resultOf(ends[0]) != string(ResultPassed) || ends[0].Agent != agent || ends[0].Generation != 3 {
				t.Fatalf("ends of the retro's step row at its report = %+v, want one, passed, by %s, of run 3", ends, agent)
			}
			if len(rated) != 1 || rated[0].ID != ends[0].ID || rated[0].PlanRating == nil || *rated[0].PlanRating != (record.PlanRating{Rating: 4, Reason: "The plan put the tests first."}) {
				t.Fatalf("ratings written at the report = %+v, want the report's on the record of the retro's run %s", rated, ends[0].ID)
			}
			if writes := rowsSince(t, r.pool, "LEGION-209", string(record.OutboxKindDispatchStatus), mark); len(writes) != 0 {
				t.Fatalf("status writes at the report = %d, want none: the status stays as the close left it", len(writes))
			}
			if after := issueRecord(t, r.pool, "LEGION-209"); after.Phase != phase.Done || after.Status != before.Status {
				t.Fatalf("the child after the report = %s/%s, want it done at %s", after.Phase, after.Status, before.Status)
			}
			supervises := supervisesSince(t, r.pool, "LEGION-209", mark)
			if closes := stopsOf(supervises, claim.RoleImplementer); len(closes) != 1 || closes[0].Op != "issue_close" {
				t.Fatalf("stops of the retro's agent at its report = %+v, want its claim closed", closes)
			}
			if tc.checked {
				if stops := stopsOf(supervises, claim.RoleTester); len(stops) != 0 {
					t.Fatalf("stops of the tester at the retro's report = %+v, want none: its check is open", stops)
				}
			}
			if got := rowsSince(t, r.pool, "LEGION-209", string(record.OutboxKindIssueSuspend), mark); len(got) != tc.suspensions {
				t.Fatalf("issue_suspend rows at the report = %d, want %d", len(got), tc.suspensions)
			}
			row, found := runRetroOf(t, r.pool, "LEGION-209", 3)
			if !found || row.ended == nil || row.outcome != record.RetroOutcomePassed || row.endedBy != record.RetroEndReport {
				t.Fatalf("the run_retros row after the report = %+v (found %t), want it ended passed by the report", row, found)
			}
		})
	}
}

// A closed issue moves nowhere, its retro after the close included: a backward move of it while
// that retro runs, from the retro's own agent, is refused BACKWARD_REFUSED and writes nothing, and
// the retro stays open.
func TestABackwardMoveOfAClosedIssueIsRefusedWhileItsRetroRuns(t *testing.T) {
	t.Parallel()
	r := newRetroRig(t)
	r.root(t, phase.Implementing)
	r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
	r.runTo(t, "LEGION-209", phase.Implementing)
	closed := r.mark(t)
	r.person(t, "LEGION-209", "done")
	if starts := retroStartsOf(supervisesSince(t, r.pool, "LEGION-209", closed)); len(starts) != 1 {
		t.Fatalf("starts of the child's retro after its close = %+v, want the one its retro runs on", starts)
	}

	mark := r.mark(t)
	result := r.apply("move-back", intake.BackwardMove{Issue: "LEGION-209", Requester: claim.RoleImplementer, To: phase.Implementing,
		Reason: "the retro found a requirement unmet"})
	if code := refusalCode(result); code != "BACKWARD_REFUSED" {
		t.Fatalf("the move back of the closed child = %q (%+v), want BACKWARD_REFUSED", code, result.Refusal)
	}
	if got := r.mark(t); got != mark {
		t.Fatalf("the refused move back wrote outbox rows up to %d past %d, want none", got, mark)
	}
	issueStandsIn(t, r.pool, "LEGION-209", phase.Done)
	if row, found := runRetroOf(t, r.pool, "LEGION-209", 3); !found || !row.open() {
		t.Fatalf("the run_retros row after the refused move = %+v (found %t), want the retro still open", row, found)
	}
}

// A retro after a close that nobody reports ends at its bound, 24 hours after its step row's start,
// and never sooner: the bound's fact before then is a wait until that moment. At the bound its
// step row ends stopped with no rating, the run's record ends stopped by the bound, the agent's
// claim is closed and the pod goes with it (an IssueSuspend), since nothing else of the issue is
// spared. A report that arrives after that is refused and records nothing.
func TestARetroAfterACloseThatNobodyReportsEndsAtItsBound(t *testing.T) {
	t.Parallel()
	r := newRetroRig(t)
	r.root(t, phase.Implementing)
	r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
	r.runTo(t, "LEGION-209", phase.Implementing)
	r.person(t, "LEGION-209", "done")
	bound := retroOpened.Add(RetroAfterCloseBound)
	ended := intake.RetroBoundEnded{Issue: "LEGION-209", Generation: 3}

	r.at(RetroAfterCloseBound - time.Hour)
	early := r.mark(t)
	_, err := intake.ApplyFact(context.Background(), r.pool, "outbox", "retro-bound:LEGION-209:3", ended, r.engine, admissionStub{})
	if !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the bound's fact an hour before the bound = %v, want a wait", err)
	}
	if at, ok := wait.UntilAt(err); !ok || !at.Equal(bound) {
		t.Fatalf("the wait names %s (%t), want the bound %s, 24 hours after the retro's start", at, ok, bound)
	}
	if got := r.mark(t); got != early {
		t.Fatalf("the bound's fact before the bound wrote outbox rows up to %d past %d, want none", got, early)
	}

	r.at(RetroAfterCloseBound)
	mark := r.mark(t)
	r.apply("retro-bound:LEGION-209:3", ended)
	agent := treeRoleToken(t, "LEGION-209", claim.RoleImplementer)
	_, ends, rated := retroStepsSince(t, r.pool, "LEGION-209", mark)
	if len(ends) != 1 || resultOf(ends[0]) != string(ResultStopped) || ends[0].Agent != agent || len(rated) != 0 {
		t.Fatalf("the retro's step row at its bound: ends %+v, ratings %+v; want one end, stopped, by %s, and no rating", ends, rated, agent)
	}
	if closes := stopsOf(supervisesSince(t, r.pool, "LEGION-209", mark), claim.RoleImplementer); len(closes) != 1 || closes[0].Op != "issue_close" {
		t.Fatalf("stops of the retro's agent at its bound = %+v, want its claim closed", closes)
	}
	if got := rowsSince(t, r.pool, "LEGION-209", string(record.OutboxKindIssueSuspend), mark); len(got) != 1 {
		t.Fatalf("issue_suspend rows at the bound = %d, want the one that takes the pod", len(got))
	}
	if row, found := runRetroOf(t, r.pool, "LEGION-209", 3); !found || row.ended == nil || row.outcome != record.RetroOutcomeStopped || row.endedBy != record.RetroEndBound {
		t.Fatalf("the run_retros row at the bound = %+v (found %t), want it ended stopped by the bound", row, found)
	}

	late := r.mark(t)
	if result := r.report(t, "LEGION-209", 3); result.Refusal == nil {
		t.Fatalf("a report after the bound was taken, want it refused")
	}
	if _, ends, rated := retroStepsSince(t, r.pool, "LEGION-209", late); len(ends) != 0 || len(rated) != 0 {
		t.Fatalf("a refused report after the bound wrote ends %+v and ratings %+v, want nothing", ends, rated)
	}
}

// A todo on a closed child while its retro after the close is open takes the child into a new run
// (ReenterChild) and ends that retro: its step row ends stopped with no rating, as the re-entry
// ends an open production check as not run, and the run's record ends stopped by the todo. The new
// run stands at admitted at the next generation, as any re-entry leaves it.
func TestATodoOnAClosedIssueEndsItsOpenRetro(t *testing.T) {
	t.Parallel()
	r := newRetroRig(t)
	r.root(t, phase.Implementing)
	r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
	r.runTo(t, "LEGION-209", phase.Implementing)
	r.person(t, "LEGION-209", "done")
	mark := r.mark(t)
	r.person(t, "LEGION-209", "todo")
	if child := issueRecord(t, r.pool, "LEGION-209"); child.Phase != phase.Admitted || child.Generation != 4 {
		t.Fatalf("the child set back to todo = %+v, want its new run (generation 4) at admitted", child)
	}
	_, ends, rated := retroStepsSince(t, r.pool, "LEGION-209", mark)
	if len(ends) != 1 || resultOf(ends[0]) != string(ResultStopped) || ends[0].Generation != 3 || len(rated) != 0 {
		t.Fatalf("the retro's step row at the todo: ends %+v, ratings %+v; want one end of run 3, stopped, and no rating", ends, rated)
	}
	if row, found := runRetroOf(t, r.pool, "LEGION-209", 3); !found || row.ended == nil || row.outcome != record.RetroOutcomeStopped || row.endedBy != record.RetroEndTodo {
		t.Fatalf("the run_retros row at the todo = %+v (found %t), want it ended stopped by the todo", row, found)
	}
}

// The root's close lingers its tree and suspends every claim of every member, but not the agent of
// a member's open retro after its close: that claim keeps its turn and its pod (no IssueSuspend of
// the member), and every other role of the member is suspended as before. The retro stays open,
// and its report is taken while the tree lingers.
func TestARootsCloseSparesAMembersOpenRetro(t *testing.T) {
	t.Parallel()
	r := newRetroRig(t)
	r.root(t, phase.Implementing)
	r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
	r.runTo(t, "LEGION-209", phase.Implementing)
	r.person(t, "LEGION-209", "done")
	mark := r.mark(t)
	r.person(t, "LEGION-208", "done")
	lingering(t, r.pool, "LEGION-208")

	supervises := supervisesSince(t, r.pool, "LEGION-209", mark)
	if stops := stopsOf(supervises, claim.RoleImplementer); len(stops) != 0 {
		t.Fatalf("the root's close stopped the member's retro agent: %+v, want it spared", stops)
	}
	if got := rowsSince(t, r.pool, "LEGION-209", string(record.OutboxKindIssueSuspend), mark); len(got) != 0 {
		t.Fatalf("issue_suspend rows of the member at the root's close = %d, want none while its retro's agent holds the pod", len(got))
	}
	for _, role := range claim.Roles {
		if role == claim.RoleImplementer {
			continue
		}
		if stops := stopsOf(supervises, role); len(stops) != 1 || stops[0].Op != "suspend" {
			t.Fatalf("stops of the member's %s at the root's close = %+v, want the linger's suspend", role, stops)
		}
	}
	if row, found := runRetroOf(t, r.pool, "LEGION-209", 3); !found || !row.open() {
		t.Fatalf("the member's run_retros row after the root's close = %+v (found %t), want its retro still open", row, found)
	}
	reported := r.mark(t)
	if result := r.report(t, "LEGION-209", 3); result.Refusal != nil {
		t.Fatalf("the member's report while the tree lingers was refused: %+v", result.Refusal)
	}
	if _, ends, _ := retroStepsSince(t, r.pool, "LEGION-209", reported); len(ends) != 1 || resultOf(ends[0]) != string(ResultPassed) {
		t.Fatalf("ends of the member's retro at its report = %+v, want one, passed", ends)
	}
}

// A root closed while its own retro runs keeps that retro's turn: the linger suspends the tree but
// not the implementer working the retro, starts nothing new and ends nothing of the retro's step
// row, and the run's record opens on that running retro. Its completion is taken although the tree
// lingers: the step row ends passed with the rating.
func TestARootClosedInItsRetroKeepsItsTurn(t *testing.T) {
	t.Parallel()
	r := newRetroRig(t)
	r.root(t, phase.Admitted)
	r.runTo(t, "LEGION-208", phase.Retro)
	deliverRetro(t, r.pool, "LEGION-208", 1)
	started := retroStartedAt(t, r.pool, "LEGION-208")
	r.at(2 * time.Hour)
	mark := r.mark(t)
	r.person(t, "LEGION-208", "done")
	lingering(t, r.pool, "LEGION-208")

	supervises := supervisesSince(t, r.pool, "LEGION-208", mark)
	if stops := stopsOf(supervises, claim.RoleImplementer); len(stops) != 0 {
		t.Fatalf("the root's close stopped the implementer working its retro: %+v, want its turn kept", stops)
	}
	if got := rowsSince(t, r.pool, "LEGION-208", string(record.OutboxKindIssueSuspend), mark); len(got) != 0 {
		t.Fatalf("issue_suspend rows of the root at its close = %d, want none while its retro runs", len(got))
	}
	if starts := retroStartsOf(supervises); len(starts) != 0 {
		t.Fatalf("starts of a retro after the root's close = %+v, want none: its retro is running", starts)
	}
	if starts, ends, _ := retroStepsSince(t, r.pool, "LEGION-208", mark); len(starts) != 0 || len(ends) != 0 {
		t.Fatalf("the root's retro step row at its close: starts %+v, ends %+v; want neither", starts, ends)
	}
	assertRunRetroOpen(t, r.pool, "LEGION-208", 1, treeRoleToken(t, "LEGION-208", claim.RoleImplementer), started.Add(RetroAfterCloseBound), mark)
	reported := r.mark(t)
	if result := r.report(t, "LEGION-208", 1); result.Refusal != nil {
		t.Fatalf("the root's retro completion while its tree lingers was refused: %+v", result.Refusal)
	}
	if _, ends, rated := retroStepsSince(t, r.pool, "LEGION-208", reported); len(ends) != 1 || resultOf(ends[0]) != string(ResultPassed) || len(rated) != 1 {
		t.Fatalf("the root's retro at its completion: ends %+v, ratings %+v; want one end, passed, and its rating", ends, rated)
	}
}

// s10: an open retro after a close holds no other issue (P8, as the architect ruled in
// dispatch://INFRA-5602/comment/cb340baf-4cb2-4c11-8ae2-801e623daa5e). The root is closed while a
// child that never started waits at admitted, and the root's run had no retro, so its close opens
// the root's own. The linger's end applies at its deadline with that retro open: it hands the
// waiting child over at once and writes every close row of the tree but the root's, whose retro
// keeps its agent and its pod. The root's report ends the tree's last open retro, past the linger,
// and leaves the close of what the end left to the hour after it, for the wake of a finding the
// retro published (closeTreeAfterRetros): a linger_close row due then. Applied before that hour,
// the close waits; once it has passed, it writes the root's rows, a tree_close of each of its roles
// and the removal of its workspace, and none of the child's again.
func TestALingersEndAtItsDeadlineDefersOnlyTheCloseOfAnOpenRetrosIssue(t *testing.T) {
	t.Parallel()
	r := newRetroRig(t)
	r.root(t, phase.Admitted)
	r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
	r.runTo(t, "LEGION-208", phase.Implementing)
	r.person(t, "LEGION-208", "done")
	recordTheStatusAMoveShows(t, r.pool, "LEGION-208", "done")
	lingering(t, r.pool, "LEGION-208")
	if row, found := runRetroOf(t, r.pool, "LEGION-208", 1); !found || !row.open() {
		t.Fatalf("the root's run_retros row after its close = %+v (found %t), want its retro after the close open", row, found)
	}
	apply := func(id string, fact intake.LingerExpired) error {
		_, err := intake.ApplyFact(context.Background(), r.pool, "outbox", id, fact, r.engine, admissionStub{})
		return err
	}
	// closes counts the tree_close rows of each issue and role, and the workspace removals of each
	// issue, the outbox holds newer than mark.
	closes := func(mark int64) map[string]int {
		t.Helper()
		got := map[string]int{}
		for _, key := range []string{"LEGION-208", "LEGION-209"} {
			for _, request := range supervisesSince(t, r.pool, key, mark) {
				if request.Op == "tree_close" {
					got[key+"/"+string(request.Role)]++
				}
			}
			got[key+"/workspace"] += len(rowsSince(t, r.pool, key, string(record.OutboxKindWorkspaceRemove), mark))
		}
		return got
	}
	everyRow := func(key string) map[string]int {
		want := map[string]int{key + "/workspace": 1}
		for _, role := range claim.Roles {
			want[key+"/"+string(role)] = 1
		}
		return want
	}

	r.at(2 * time.Hour)
	deadline := r.mark(t)
	if err := apply(LingerEventID("LEGION-208", 1), intake.LingerExpired{Issue: "LEGION-208", Generation: 1}); err != nil {
		t.Fatalf("the linger's end with the root's retro open = %v, want it applied at its deadline", err)
	}
	if fences, writes := handoverFences(t, r.pool), rowsSince(t, r.pool, "LEGION-209", string(record.OutboxKindDispatchStatus), deadline); !slices.Equal(fences, []string{"LEGION-209"}) || len(writes) != 1 {
		t.Fatalf("the linger's end handed over %v with %d status writes of LEGION-209, want LEGION-209 handed over at once with one write", fences, len(writes))
	}
	want := everyRow("LEGION-209")
	want["LEGION-208/workspace"] = 0
	if got := closes(deadline); !maps.Equal(got, want) {
		t.Fatalf("close rows at the linger's end = %v, want %v: every row of the child and none of the root, whose retro is open", got, want)
	}

	r.at(3 * time.Hour)
	reported := r.mark(t)
	if result := r.report(t, "LEGION-208", 1); result.Refusal != nil {
		t.Fatalf("the report of the root's retro past its linger was refused: %+v", result.Refusal)
	}
	ended, found := runRetroOf(t, r.pool, "LEGION-208", 1)
	if !found || ended.ended == nil {
		t.Fatalf("the root's run_retros row after its report = %+v (found %t), want it ended", ended, found)
	}
	deferred := rowsSince(t, r.pool, "LEGION-208", string(record.OutboxKindLingerClose), reported)
	if len(deferred) != 1 || !deferred[0].nextAt.Equal(ended.ended.Add(pods.RetroWakeWindow)) {
		t.Fatalf("linger_close rows at the end of the tree's last retro = %+v, want one due the hour after its end, %s", deferred, ended.ended.Add(pods.RetroWakeWindow))
	}
	var queued record.LingerClose
	if err := json.Unmarshal(deferred[0].payload, &queued); err != nil || queued.RetrosEnded == nil || !queued.RetrosEnded.Equal(*ended.ended) {
		t.Fatalf("the deferred close = %s (%v), want it to name the retro's end %s", deferred[0].payload, err, ended.ended)
	}
	if got := closes(reported); got["LEGION-208/architect"] != 0 || got["LEGION-208/workspace"] != 0 {
		t.Fatalf("close rows of the root at its retro's end = %v, want none before the hour has passed", got)
	}
	fact := intake.LingerExpired{Issue: "LEGION-208", Generation: 1, RetrosEnded: queued.RetrosEnded}
	id := LingerAfterRetrosEventID("LEGION-208", 1, *queued.RetrosEnded)
	if err := apply(id, fact); !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the deferred close before its hour = %v, want a wait", err)
	}

	r.at(3*time.Hour + pods.RetroWakeWindow + time.Minute)
	if err := apply(id, fact); err != nil {
		t.Fatalf("the deferred close once its hour has passed = %v, want it applied", err)
	}
	want = everyRow("LEGION-208")
	want["LEGION-209/workspace"] = 0
	if got := closes(reported); !maps.Equal(got, want) {
		t.Fatalf("close rows once the hour after the retro's end has passed = %v, want %v: the root's rows, which the linger's end left, and none of the child's again", got, want)
	}
}

// A root architect woken while a retro after a close of its tree is open (P6) is the daemon's to
// suspend again once no such retro of the tree is open (P7). The tree here has three: the root's
// own, since its run started when its children's did, and one of each child. The end of the root's
// own retro, and of a child's, while another retro of the tree is still open leaves the architect
// as it is; the end of the last one suspends it and takes the root's pod (an IssueSuspend of the
// root).
func TestAWokenRootArchitectIsSuspendedOnceNoRetroOfItsTreeIsOpen(t *testing.T) {
	t.Parallel()
	r := newRetroRig(t)
	r.root(t, phase.Implementing)
	r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
	r.member(t, "LEGION-210", "LEGION-208", phase.Admitted, 44)
	r.runTo(t, "LEGION-209", phase.Implementing)
	r.runTo(t, "LEGION-210", phase.Implementing)
	r.person(t, "LEGION-209", "done")
	r.person(t, "LEGION-210", "done")
	r.person(t, "LEGION-208", "done")
	lingering(t, r.pool, "LEGION-208")
	seedRunClaim(t, r.pool, "LEGION-208", claim.RoleArchitect, 1, "idle", "", ownerSession)

	for _, open := range []struct {
		key        string
		generation uint64
	}{{"LEGION-208", 1}, {"LEGION-209", 3}} {
		mark := r.mark(t)
		if result := r.report(t, open.key, open.generation); result.Refusal != nil {
			t.Fatalf("the report of %s's retro was refused: %+v", open.key, result.Refusal)
		}
		if stops := stopsOf(supervisesSince(t, r.pool, "LEGION-208", mark), claim.RoleArchitect); len(stops) != 0 {
			t.Fatalf("stops of the root architect at the end of %s's retro, LEGION-210's still open = %+v, want none", open.key, stops)
		}
	}
	last := r.mark(t)
	if result := r.report(t, "LEGION-210", 3); result.Refusal != nil {
		t.Fatalf("the report of LEGION-210's retro was refused: %+v", result.Refusal)
	}
	if stops := stopsOf(supervisesSince(t, r.pool, "LEGION-208", last), claim.RoleArchitect); len(stops) != 1 || stops[0].Op != "suspend" {
		t.Fatalf("stops of the root architect once the tree's last retro ended = %+v, want one suspend", stops)
	}
	if got := rowsSince(t, r.pool, "LEGION-208", string(record.OutboxKindIssueSuspend), last); len(got) != 1 {
		t.Fatalf("issue_suspend rows of the root once the tree's last retro ended = %d, want one", len(got))
	}
}

// The end of a tree's last open retro after a close leaves the root architect to a retro's wake
// that waits for it (P6, P7): the retro's agent published its finding, whose wake is queued, and
// then reported, which ended the retro. A suspension written then would stand above the wake's
// start and stop the architect before it took the wake's task, so none is written; the idle
// release stops the architect once its turn on the finding ends. With no wake waiting, the end
// suspends the architect as before (TestAWokenRootArchitectIsSuspendedOnceNoRetroOfItsTreeIsOpen).
// The tree's one retro here is the root's own.
func TestTheEndOfATreesLastRetroLeavesTheRootArchitectToTheWakeQueuedForIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		wake     bool
		suspends int
	}{
		{name: "a wake of the architect is queued for the retro's finding", wake: true},
		{name: "no wake of the architect is queued", suspends: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRetroRig(t)
			r.engine.cfg.Pods = wakePods(t, r.pool)
			r.root(t, phase.Admitted)
			r.runTo(t, "LEGION-208", phase.Implementing)
			r.person(t, "LEGION-208", "done")
			lingering(t, r.pool, "LEGION-208")
			seedRunClaim(t, r.pool, "LEGION-208", claim.RoleArchitect, 1, "suspended", "daemon", "session-architect")
			if tc.wake {
				woken := r.mark(t)
				r.apply("finding", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: treeRoleToken(t, "LEGION-208", claim.RoleArchitect),
					At: r.clock.now, Report: "report-finding"})
				var wakes int
				for _, request := range supervisesSince(t, r.pool, "LEGION-208", woken) {
					if request.Op == "start" && request.RetroWake {
						wakes++
					}
				}
				if wakes != 1 {
					t.Fatalf("retro wakes of the root architect for the finding = %d, want one", wakes)
				}
			}

			mark := r.mark(t)
			if result := r.report(t, "LEGION-208", 1); result.Refusal != nil {
				t.Fatalf("the report of the root's retro was refused: %+v", result.Refusal)
			}
			var suspends []record.SuperviseRequest
			for _, stop := range stopsOf(supervisesSince(t, r.pool, "LEGION-208", mark), claim.RoleArchitect) {
				if stop.Op == "suspend" {
					suspends = append(suspends, stop)
				}
			}
			if len(suspends) != tc.suspends {
				t.Fatalf("suspensions of the root architect at the end of the tree's last retro = %+v, want %d", suspends, tc.suspends)
			}
		})
	}
}

// A run starts with its issue's own first phase, or with the first phase of an issue beneath it in
// that run: the record holds a row for the issue that started and for each issue above it, at
// each one's own run and in their tree, with the moment it started, and none for an issue whose run
// nothing started (a sibling that waits). A later first phase of an issue whose run already
// started keeps the first moment.
func TestARunIsStartedByItsOwnFirstPhaseOrOneBeneathIt(t *testing.T) {
	t.Parallel()
	r := newRetroRig(t)
	r.root(t, phase.Admitted)
	r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
	r.member(t, "LEGION-210", "LEGION-209", phase.Admitted, 44)
	r.member(t, "LEGION-211", "LEGION-208", phase.Admitted, 45)
	r.runTo(t, "LEGION-210", phase.Planning)

	for _, tc := range []struct {
		key        string
		generation uint64
	}{{"LEGION-210", 3}, {"LEGION-209", 3}, {"LEGION-208", 1}} {
		if row, found := runRetroOf(t, r.pool, tc.key, tc.generation); !found || row.tree != "LEGION-208" || !row.startedAt.Equal(retroOpened) || row.retroStartedAt != nil {
			t.Fatalf("the run_retros row of %s run %d = %+v (found %t), want it started at %s in LEGION-208, no retro", tc.key, tc.generation, row, found, retroOpened)
		}
	}
	if rows := runRetrosOf(t, r.pool, "LEGION-211"); len(rows) != 0 {
		t.Fatalf("the run_retros rows of the sibling that waits = %+v, want none", rows)
	}

	r.at(time.Hour)
	r.runTo(t, "LEGION-209", phase.Planning)
	if row, found := runRetroOf(t, r.pool, "LEGION-209", 3); !found || !row.startedAt.Equal(retroOpened) {
		t.Fatalf("the parent's row after its own first phase = %+v (found %t), want its run's first start %s kept", row, found, retroOpened)
	}
}

// A retro that starts marks its run as having had its retro, at the moment its step row started,
// in the transaction that writes that start; a later retro of the same run, after the work went
// back, keeps the first moment, since any retro row of the run counts.
func TestARetroThatStartsMarksItsRun(t *testing.T) {
	t.Parallel()
	r := newRetroRig(t)
	r.root(t, phase.Implementing)
	r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
	r.runTo(t, "LEGION-209", phase.Accepting)
	r.at(time.Hour)
	move(t, r.pool, r.engine, "LEGION-209", TriggerSignOff, "", record.PhaseRow{})
	first := retroStartedAt(t, r.pool, "LEGION-209")
	if row, found := runRetroOf(t, r.pool, "LEGION-209", 3); !found || row.retroStartedAt == nil || !row.retroStartedAt.Equal(first) {
		t.Fatalf("the run_retros row once the retro started = %+v (found %t), want its retro started at %s", row, found, first)
	}

	r.at(2 * time.Hour)
	move(t, r.pool, r.engine, "LEGION-209", TriggerBackward, phase.Implementing, record.PhaseRow{})
	for _, trigger := range []TriggerKind{TriggerImplementationReady, TriggerTesterPassed, TriggerReviewApproved, TriggerSignOff} {
		move(t, r.pool, r.engine, "LEGION-209", trigger, "", record.PhaseRow{})
	}
	if again := retroStartedAt(t, r.pool, "LEGION-209"); !again.After(first) {
		t.Fatalf("the second retro started at %s, want it after the first %s", again, first)
	}
	if row, found := runRetroOf(t, r.pool, "LEGION-209", 3); !found || row.retroStartedAt == nil || !row.retroStartedAt.Equal(first) {
		t.Fatalf("the run_retros row after a second retro = %+v (found %t), want the first retro's start %s kept", row, found, first)
	}
}
