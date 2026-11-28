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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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

// readmits is admission's re-admission of key at a person's todo, in the order admit's readmit
// takes it, in the fact of the todo: the record of key's next generation is written first, a root
// of its own at admitted and todo with no linger (a child moves out of its tree into the tree its
// own key names, recording the tree it left), and key's open retro after its close then ends
// (EndOpenRetro), whose step row's end the fact's settle writes. The rest of readmit — the old
// tree's suspends of a child's roles, the carry of a child's run, the clear of the tree's facts —
// is written beside these and is no part of what the retro's end reads.
func (r *retroRig) readmits(t *testing.T, key string) {
	t.Helper()
	if err := store.Transact(r.engine.OpenFact(t.Context()), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		stored, err := r.engine.store.Issue(ctx, tx, key)
		if err != nil || stored == nil {
			return fmt.Errorf("read %s: %v", key, err)
		}
		readmitted := *stored
		if readmitted.Tree != key {
			readmitted.HandedOverFrom, readmitted.TakenBack = readmitted.Tree, false
		}
		readmitted.Tree, readmitted.HandedOver, readmitted.Ancestry = key, true, nil
		readmitted.Phase, readmitted.Hold, readmitted.Awaiting, readmitted.ChildrenWait = phase.Admitted, nil, nil, nil
		readmitted.Generation++
		readmitted.Status, readmitted.DispatchStatus, readmitted.LingerUntil = "todo", "todo", nil
		if err := r.engine.WriteIssue(ctx, tx, readmitted); err != nil {
			return err
		}
		if err := r.engine.EndOpenRetro(ctx, tx, readmitted); err != nil {
			return err
		}
		return r.engine.SettleFact(ctx, tx)
	}); err != nil {
		t.Fatalf("the re-admission of %s at a todo: %v", key, err)
	}
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

// hourAfterRetros is a tree whose last retro after a close ended (P6, P7). The rig's linger is 72
// hours, as the default's, so the hour after that end passes well inside it. In the tree
// newHourAfterRetros stands up, the root LEGION-208 closes while its child LEGION-209, whose run
// started the root's, still works in implementing, so the root's own retro is the tree's one open
// retro, and the root architect's claim is the one the close suspended, as the daemon's own; the
// table of every end of such a retro (TestEveryEndOfATreesLastRetroStopsWhatTheTreeKeptByItsState)
// stands up trees of its own. ended is when that end was, and stop and id are the fact of the
// linger_close row that end queued for the hour after it and the end's own event it applies under
// (LingerAfterRetrosEventID), as the outbox applies it.
type hourAfterRetros struct {
	*retroRig
	ended time.Time
	stop  intake.LingerExpired
	id    string
}

// newHourAfterRetros is that tree. With wake, a finding of the root's retro wakes the architect
// before the root's report, and the wake's start is still queued when the report ends the retro,
// which leaves the architect to that wake; without, the report suspends the architect. Either way
// the end queues one linger_close row, due the hour after the end, naming that end.
func newHourAfterRetros(t *testing.T, wake bool) *hourAfterRetros {
	t.Helper()
	h := &hourAfterRetros{retroRig: newRetroRig(t)}
	h.engine.cfg.Pods = wakePods(t, h.pool)
	h.engine.cfg.Linger = 72 * time.Hour
	h.root(t, phase.Implementing)
	h.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
	h.runTo(t, "LEGION-209", phase.Implementing)
	h.person(t, "LEGION-208", "done")
	lingering(t, h.pool, "LEGION-208")
	seedRunClaim(t, h.pool, "LEGION-208", claim.RoleArchitect, 1, "suspended", "daemon", "session-architect")
	if wake {
		h.wakeArchitect(t, "finding")
	}
	mark := h.mark(t)
	if result := h.report(t, "LEGION-208", 1); result.Refusal != nil {
		t.Fatalf("the report of the root's retro was refused: %+v", result.Refusal)
	}
	if row, found := runRetroOf(t, h.pool, "LEGION-209", 3); found && row.open() {
		t.Fatalf("LEGION-209's run_retros row = %+v, want no retro of it open: the root's was the tree's last", row)
	}
	h.endedBy(t, "LEGION-208", 1, mark)
	return h
}

// retrosEndedRow reads the end of key's retro of run, which ended the last open retro after a close
// of the tree, and fails unless the root's outbox holds one linger_close row newer than mark, due
// the hour after that end, at the root's generation 1 and naming the end (record.LingerClose with
// RetrosEnded). It answers the end and the row's fact.
func retrosEndedRow(t *testing.T, pool *pgxpool.Pool, key string, run uint64, mark int64) (time.Time, intake.LingerExpired) {
	t.Helper()
	row, found := runRetroOf(t, pool, key, run)
	if !found || row.ended == nil {
		t.Fatalf("%s's run_retros row of run %d = %+v (found %t), want it ended", key, run, row, found)
	}
	ended := *row.ended
	queued := rowsSince(t, pool, "LEGION-208", string(record.OutboxKindLingerClose), mark)
	if len(queued) != 1 || !queued[0].nextAt.Equal(ended.Add(pods.RetroWakeWindow)) {
		t.Fatalf("linger_close rows of the root at the end of the tree's last retro = %+v, want one due the hour after the end, %s",
			queued, ended.Add(pods.RetroWakeWindow))
	}
	var payload record.LingerClose
	if err := json.Unmarshal(queued[0].payload, &payload); err != nil || payload.Generation != 1 || payload.RetrosEnded == nil || !payload.RetrosEnded.Equal(ended) {
		t.Fatalf("the linger_close row = %s (%v), want generation 1 naming the retro's end %s", queued[0].payload, err, ended)
	}
	return ended, intake.LingerExpired{Issue: "LEGION-208", Generation: 1, RetrosEnded: payload.RetrosEnded}
}

// endedBy keeps the end of key's retro of run, the tree's last, the fact of the linger_close row
// that end queued newer than mark (retrosEndedRow), and the end's own event the outbox applies it
// under (LingerAfterRetrosEventID).
func (h *hourAfterRetros) endedBy(t *testing.T, key string, run uint64, mark int64) {
	t.Helper()
	h.ended, h.stop = retrosEndedRow(t, h.pool, key, run, mark)
	h.id = LingerAfterRetrosEventID("LEGION-208", 1, *h.stop.RetrosEnded)
}

// assertRetroIssueStops fails unless the rows newer than mark stop what the retro after key's close
// kept of key where stopped is set, and nothing of it otherwise: one stop of its retro agent (the
// implementer), the close of its claim, and one issue_suspend row of its pod, as a report or a
// bound of an issue still closed writes them. A todo takes key into a new run, whose claims and pod
// are the new run's.
func assertRetroIssueStops(t *testing.T, pool *pgxpool.Pool, key string, mark int64, stopped bool) {
	t.Helper()
	stops := stopsOf(supervisesSince(t, pool, key, mark), claim.RoleImplementer)
	suspensions, want := len(rowsSince(t, pool, key, string(record.OutboxKindIssueSuspend), mark)), 0
	if stopped {
		want = 1
	}
	if len(stops) != want || want == 1 && stops[0].Op != "issue_close" || suspensions != want {
		t.Fatalf("stops of %s's retro agent %+v and %d issue_suspend rows of %s at the end of its retro, want %d of each, a stop being its claim's close", key, stops, suspensions, key, want)
	}
}

// after moves the rig's clock to d past the end of the tree's last retro.
func (h *hourAfterRetros) after(d time.Duration) { h.clock.now = h.ended.Add(d) }

// fromOutbox applies fact under id as the outbox applies a row's fact, and answers its error.
func (h *hourAfterRetros) fromOutbox(id string, fact intake.Fact) error {
	_, err := intake.ApplyFact(context.Background(), h.pool, "outbox", id, fact, h.engine, admissionStub{})
	return err
}

// applyStop applies the fact of the linger_close row the end queued.
func (h *hourAfterRetros) applyStop() error { return h.fromOutbox(h.id, h.stop) }

// wakeArchitect is a message to the root architect's role topic while no live session holds it,
// as the listener reports it (report names the report), and fails unless it queues one start of
// a retro's wake of the architect.
func (h *hourAfterRetros) wakeArchitect(t *testing.T, report string) {
	t.Helper()
	mark := h.mark(t)
	h.apply(report, intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: treeRoleToken(t, "LEGION-208", claim.RoleArchitect),
		At: h.clock.now, Report: report})
	var wakes int
	for _, request := range supervisesSince(t, h.pool, "LEGION-208", mark) {
		if request.Op == "start" && request.RetroWake {
			wakes++
		}
	}
	if wakes != 1 {
		t.Fatalf("retro wakes of the root architect for %s = %d, want one", report, wakes)
	}
}

// ranWake stands for the outbox having run the queued start of a retro's wake of the architect:
// its row is gone, as the outbox deletes a row it ran.
func (h *hourAfterRetros) ranWake(t *testing.T) {
	t.Helper()
	tag, err := h.pool.Exec(t.Context(), `delete from outbox where issue = 'LEGION-208' and kind = 'supervise'
		and payload->>'op' = 'start' and coalesce((payload->>'retroWake')::boolean, false)`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("run the start of the retro's wake of the root architect: %d rows, %v; want its one queued start", tag.RowsAffected(), err)
	}
}

// architectAt records the root architect's claim in state (suspended by suspendedBy), holding the
// task of a retro's wake (supervise.RetroWakeTaskID) where wakeTask is set, and no task otherwise.
func (h *hourAfterRetros) architectAt(t *testing.T, state, suspendedBy string, wakeTask bool) {
	t.Helper()
	seedRunClaim(t, h.pool, "LEGION-208", claim.RoleArchitect, 1, state, suspendedBy, "session-architect")
	token := treeRoleToken(t, "LEGION-208", claim.RoleArchitect)
	if _, err := h.pool.Exec(t.Context(), `delete from pending_task_deliveries where claim_token = $1`, token); err != nil {
		t.Fatalf("clear the root architect's task: %v", err)
	}
	if !wakeTask {
		return
	}
	if _, err := h.pool.Exec(t.Context(), `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at) values ($1, $2, $3, now())`,
		token, supervise.RetroWakeTaskID("outbox-delivery:wake"), retroArchitectWakeTask(token)); err != nil {
		t.Fatalf("seed the task of the retro's wake on the root architect's claim: %v", err)
	}
}

// stopsSince is the rows newer than mark that stop the root architect, and the root's
// issue_suspend rows newer than mark.
func (h *hourAfterRetros) stopsSince(t *testing.T, mark int64) ([]record.SuperviseRequest, int) {
	t.Helper()
	return stopsOf(supervisesSince(t, h.pool, "LEGION-208", mark), claim.RoleArchitect),
		len(rowsSince(t, h.pool, "LEGION-208", string(record.OutboxKindIssueSuspend), mark))
}

// closesSince is the root's close rows newer than mark: its tree_close rows of each role, and its
// workspace removals under "workspace".
func (h *hourAfterRetros) closesSince(t *testing.T, mark int64) map[string]int {
	t.Helper()
	got := map[string]int{}
	for _, request := range supervisesSince(t, h.pool, "LEGION-208", mark) {
		if request.Op == "tree_close" {
			got[string(request.Role)]++
		}
	}
	if removals := len(rowsSince(t, h.pool, "LEGION-208", string(record.OutboxKindWorkspaceRemove), mark)); removals > 0 {
		got["workspace"] = removals
	}
	return got
}

// lingerEnds applies the linger's own end, under the linger's event (LingerEventID), as the
// outbox applies the row the root's close queued, and answers the root's close rows it wrote; the
// clock moves past the deadline first.
func (h *hourAfterRetros) lingerEnds(t *testing.T) map[string]int {
	t.Helper()
	root := issueRecord(t, h.pool, "LEGION-208")
	if root == nil || root.LingerUntil == nil {
		t.Fatalf("the root = %+v, want it lingering", root)
	}
	h.clock.now = root.LingerUntil.Add(time.Minute)
	mark := h.mark(t)
	if err := h.fromOutbox(LingerEventID("LEGION-208", 1), intake.LingerExpired{Issue: "LEGION-208", Generation: 1}); err != nil {
		t.Fatalf("the linger's end = %v, want it applied", err)
	}
	return h.closesSince(t, mark)
}

// everyRootClose is closesSince's count of the whole root's close: one tree_close of each role and
// one removal of its workspace.
func everyRootClose() map[string]int {
	want := map[string]int{"workspace": 1}
	for _, role := range claim.Roles {
		want[string(role)] = 1
	}
	return want
}

// The reviewer's case of P7 (review thread PRRT_kwDORFy7ds69BE_B). For the hour after the end of the
// last retro after a close of a tree inside its linger, at T, the root's pod is the architect's,
// whose idle release stops it once its turn ends. After that hour, the idle rule keeps the pod for
// the linger's close. So a root architect that a retro's wake brought back, and whose turn ended
// after T+50m, or that a message woke again at T+55m, kept its pod until the linger's end, 72 hours
// on, and the root's IssueSuspend waited as long. The end now queues a linger_close row due at T
// plus the hour (suspendArchitectAfterRetros). That row waits while the hour has not passed, and
// while the architect's claim holds the task of a retro's wake, which its turn holds until it ends.
// It then suspends the architect and queues the root's IssueSuspend while the linger still runs.
// It applies under the end's own event, never the linger's, so the linger's end still applies at
// its deadline after it and closes the tree.
func TestAWokenRootArchitectIsStoppedOnceTheHourAfterItsTreesLastRetroHasPassed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// wake is whether a finding wakes the architect before the report, which then leaves the
		// architect to that wake; without, the report suspends it and a message wakes it at T+55m.
		wake bool
	}{
		{name: "the architect's turn on the finding ends after T+50m", wake: true},
		{name: "a message wakes the architect again at T+55m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHourAfterRetros(t, tc.wake)
			if tc.wake {
				h.ranWake(t)
				h.architectAt(t, "working", "", true)
				h.after(50 * time.Minute)
				mark := h.mark(t)
				if err := h.applyStop(); !errors.Is(err, wait.ErrWaiting) {
					t.Fatalf("the stop before the hour after the retro's end has passed = %v, want a wait", err)
				}
				if stops, suspensions := h.stopsSince(t, mark); len(stops) != 0 || suspensions != 0 {
					t.Fatalf("before the hour has passed the stop wrote stops %+v and %d issue_suspend rows of the root, want none", stops, suspensions)
				}
				h.after(51 * time.Minute)
				h.architectAt(t, "idle", "", false)
			} else {
				h.after(55 * time.Minute)
				h.wakeArchitect(t, "finding-again")
				h.ranWake(t)
				h.architectAt(t, "working", "", true)
				h.after(pods.RetroWakeWindow + time.Minute)
				mark := h.mark(t)
				if err := h.applyStop(); !errors.Is(err, wait.ErrWaiting) {
					t.Fatalf("the stop once the hour has passed, the architect's turn on the wake still running = %v, want a wait", err)
				}
				if stops, suspensions := h.stopsSince(t, mark); len(stops) != 0 || suspensions != 0 {
					t.Fatalf("while the architect's turn on the wake runs the stop wrote stops %+v and %d issue_suspend rows of the root, want none", stops, suspensions)
				}
				h.after(pods.RetroWakeWindow + 5*time.Minute)
				h.architectAt(t, "idle", "", false)
			}

			h.after(pods.RetroWakeWindow + 6*time.Minute)
			mark := h.mark(t)
			if err := h.applyStop(); err != nil {
				t.Fatalf("the stop once the hour has passed and the architect's turn has ended = %v, want it applied", err)
			}
			stops, suspensions := h.stopsSince(t, mark)
			if len(stops) != 1 || stops[0].Op != "suspend" || stops[0].Generation != 1 {
				t.Fatalf("stops of the root architect once the hour has passed = %+v, want one suspend", stops)
			}
			if suspensions != 1 {
				t.Fatalf("issue_suspend rows of the root once the hour has passed = %d, want one", suspensions)
			}
			root := issueRecord(t, h.pool, "LEGION-208")
			if root == nil || !record.Lingering(*root, h.clock.now) {
				t.Fatalf("the root once its architect was stopped = %+v, want its linger still running at %s", root, h.clock.now)
			}
			if got, want := h.lingerEnds(t), everyRootClose(); !maps.Equal(got, want) {
				t.Fatalf("close rows of the root at the linger's end after the stop = %v, want %v: the linger's own end still applies and closes the tree", got, want)
			}
		})
	}
}

// The linger can end within the hour after its tree's last retro after a close ended, and the stop
// that end queued for that hour then comes due past the linger's deadline (suspendAfterRetros is
// for a tree inside its linger). It is then the close a tree past its linger owes
// (closeAfterRetros), and while the linger's own end has not applied it changes nothing and spends
// its own event: that end closes the whole tree, the root with it, when it applies.
func TestAStopDuePastTheLingersDeadlineLeavesTheTreeToTheLingersEnd(t *testing.T) {
	t.Parallel()
	h := newHourAfterRetros(t, true)
	h.ranWake(t)
	h.architectAt(t, "idle", "", false)
	if _, err := h.pool.Exec(t.Context(), `update issues set linger_until = $1 where key = 'LEGION-208'`, h.ended.Add(30*time.Minute)); err != nil {
		t.Fatalf("bring the root's linger deadline to half an hour after the retro's end: %v", err)
	}
	h.after(pods.RetroWakeWindow + time.Minute)
	mark := h.mark(t)
	if err := h.applyStop(); err != nil {
		t.Fatalf("the stop past the linger's deadline, before the linger's end applied = %v, want it applied", err)
	}
	if stops, suspensions := h.stopsSince(t, mark); len(stops) != 0 || suspensions != 0 {
		t.Fatalf("the stop past the linger's deadline wrote stops %+v and %d issue_suspend rows of the root, want none: the linger's end closes the tree", stops, suspensions)
	}
	if got, want := h.lingerEnds(t), everyRootClose(); !maps.Equal(got, want) {
		t.Fatalf("close rows of the root at the linger's end = %v, want %v", got, want)
	}
}

// The stop at the end of the hour after a lingering tree's last retro after a close
// (suspendAfterRetros) stops nothing while the tree still holds the root architect. A retro of the
// tree that opened again before the hour's end decides again when it ends, so the row finishes. A
// retro wake's start that is still queued, or a retro wake's task that the architect's claim holds,
// makes the row wait and run again: the claim holds such a task until the turn that took it ends. A
// claim that runs nothing has nothing to stop, so the row finishes, even though that claim keeps a
// retro wake's task it never took.
func TestARootArchitectIsNotStoppedAtTheHoursEndWhileItsTreeStillHoldsIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, h *hourAfterRetros)
		waits   bool
	}{
		{name: "a retro of the tree opened again before the hour's end", arrange: func(t *testing.T, h *hourAfterRetros) {
			h.ranWake(t)
			h.architectAt(t, "idle", "", false)
			h.after(30 * time.Minute)
			h.person(t, "LEGION-209", "done")
			if row, found := runRetroOf(t, h.pool, "LEGION-209", 3); !found || !row.open() {
				t.Fatalf("LEGION-209's run_retros row after its close = %+v (found %t), want its retro after the close open", row, found)
			}
		}},
		{name: "the start of a retro's wake of the architect is still queued", waits: true, arrange: func(*testing.T, *hourAfterRetros) {}},
		{name: "the architect's claim holds the task of a retro's wake its turn took", waits: true, arrange: func(t *testing.T, h *hourAfterRetros) {
			h.ranWake(t)
			h.architectAt(t, "working", "", true)
		}},
		{name: "the architect's claim runs nothing, a retro wake's task kept on it", arrange: func(t *testing.T, h *hourAfterRetros) {
			h.ranWake(t)
			h.architectAt(t, "suspended", "daemon", true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHourAfterRetros(t, true)
			tc.arrange(t, h)
			h.after(pods.RetroWakeWindow + time.Minute)
			mark := h.mark(t)
			err := h.applyStop()
			want := "it applied"
			if tc.waits {
				want = "a wait"
			}
			if tc.waits && !errors.Is(err, wait.ErrWaiting) || !tc.waits && err != nil {
				t.Fatalf("the stop once the hour has passed = %v, want %s", err, want)
			}
			if stops, suspensions := h.stopsSince(t, mark); len(stops) != 0 || suspensions != 0 {
				t.Fatalf("the stop wrote stops %+v and %d issue_suspend rows of the root, want none", stops, suspensions)
			}
		})
	}
}

// retroEnd is what ends the last retro after a close of a tree, each as the daemon drives it: the
// report through the completion route's fact, the bound through the fact of its row, a child's
// todo through the engine's re-entry where its tree runs (ReenterChild) and through admission's
// re-admission where it does not (readmits), and the root's own todo through its re-admission.
type retroEnd string

const (
	endReport    retroEnd = "the report"
	endBound     retroEnd = "the bound"
	endChildTodo retroEnd = "a child's todo"
	endRootTodo  retroEnd = "the root's own todo"
)

// treeAt is where the tree stands at that end: its root runs, the root's close lingers the tree,
// or the linger's end has applied at its deadline with that retro open (lingerExpired).
type treeAt string

const (
	treeRuns    treeAt = "runs"
	treeLingers treeAt = "lingers"
	treePast    treeAt = "is past its linger"
)

// architectStop is how that end stops the root architect: not at all (stopNone), at once by a
// suspension (stopNow), or left to the retro's wake queued for it and stopped once the hour after
// the end has passed (stopAtHour): suspended inside the linger (suspendAfterRetros), closed with
// the tree past it (closeAfterRetros).
type architectStop int

const (
	stopNone architectStop = iota
	stopNow
	stopAtHour
)

// assertTreeCloses fails unless the close rows of LEGION-208's tree newer than mark are closing's,
// none for nil: a tree_close of every role and the removal of the workspace of each member it
// names. Every such tree_close names the tree and the root's generation 1, whose linger it closes,
// and the store reserves the tree's cleanup for it as the outbox's run of the row does
// (internal/daemon's reservedTreeClose, store's ReserveWorkflowTreeCleanup): the reservation
// without which the tree's cleanup never runs.
func assertTreeCloses(t *testing.T, pool *pgxpool.Pool, mark int64, closing []string) {
	t.Helper()
	got, want := map[string]int{}, map[string]int{}
	var closes []record.SuperviseRequest
	for _, key := range []string{"LEGION-208", "LEGION-209"} {
		for _, request := range supervisesSince(t, pool, key, mark) {
			if request.Op == "tree_close" {
				got[key+"/"+string(request.Role)]++
				closes = append(closes, request)
			}
		}
		if removals := len(rowsSince(t, pool, key, string(record.OutboxKindWorkspaceRemove), mark)); removals > 0 {
			got[key+"/workspace"] = removals
		}
	}
	for _, key := range closing {
		want[key+"/workspace"] = 1
		for _, role := range claim.Roles {
			want[key+"/"+string(role)] = 1
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("close rows of the tree = %v, want %v", got, want)
	}
	if len(closes) == 0 {
		return
	}
	for _, request := range closes {
		if request.Tree != "LEGION-208" || request.Linger != 1 {
			t.Fatalf("a tree_close row = %+v, want it to name the tree LEGION-208 and its linger at generation 1", request)
		}
	}
	lifecycle, reserved, err := releaseStore(t, pool).ReserveWorkflowTreeCleanup(t.Context(), wakeProjectToken(t), closes[0].Tree, closes[0].Linger)
	if err != nil || !reserved {
		t.Fatalf("the reservation of the tree's cleanup for its tree_close row = %+v, reserved %t, %v; want it reserved", lifecycle, reserved, err)
	}
}

// One path for every end of the last retro after a close of a tree (review round 4 of #5630, as
// dispatch://INFRA-5602/comment/a8d1d1de-5630-4793-b2f3-4b1a6d781bb6 narrows it): the report, the
// bound, a child's todo and the root's own todo each stop what the tree kept for its retros as the
// tree's own state says, the tree read from the ended run_retros row whatever has become of the
// retro's issue (finishRetroAfterClose). The tree is LEGION-208 and its child LEGION-209, closed
// together so that each holds a retro after its close, and the root architect's claim is the one
// the root's close suspended. The retro that ends is the tree's last: the child's, once the root's
// own report has ended the root's where the root closed, except at the root's own todo, which ends
// the root's once the child's report has ended the child's. The tree runs (only the child closed),
// lingers, or is past its linger, whose end applied at its deadline with that retro open and
// retired the root architect. With wake, a finding of the retro wakes the root architect before
// the end and the wake's start is still queued at it, and the turn the wake brought has ended by
// the hour after the end.
//
// Each row asserts the root architect's stop (architectStop), the root's pod (an IssueSuspend of
// the root at the end, or none), the tree's close the hour after the end (the close rows of each
// member closes names, enough for the store to reserve the tree's cleanup, or none), and what the
// end stops of the retro's own issue (its agent's claim and its pod, at the report or the bound of
// an issue still closed, never at a todo).
//
// Round 3's case (review thread PRRT_kwDORFy7ds69DTRE) is the child's todo past the linger: the
// child has left the tree, so the close the hour after found no member to close and wrote nothing,
// and the tree's cleanup, of which the linger's end reserved nothing while the retro was open, was
// never reserved. That close now writes the root's rows whenever the tree's cleanup is not reserved
// yet, so past the linger the close after the report and after the bound writes them too, beside
// the child's.
//
// The combinations with no row cannot happen:
//   - the report or the bound of a retro whose issue has left the tree: only a re-admission moves
//     an issue out of its tree, and it is a todo, which ends the issue's retro first;
//   - a child's todo that keeps it in a tree that lingers or is past its linger: that tree is not
//     live, so the todo does not re-enter the child into it (ReenterChild) but re-admits it into a
//     tree of its own, as an orphan or a hand-over (admit's readmit);
//   - the root's own todo leaving its tree: a root's tree is its own key, which its re-admission
//     keeps;
//   - the root's own todo in a tree that runs: a root whose retro after its close is open has
//     closed, and every close of a root lingers its tree (beginLinger);
//   - a wake in a tree that runs: the architect of a root that runs takes no retro's wake
//     (RetroWakeStands).
func TestEveryEndOfATreesLastRetroStopsWhatTheTreeKeptByItsState(t *testing.T) {
	t.Parallel()
	const root, child = "LEGION-208", "LEGION-209"
	both, rootOnly := []string{root, child}, []string{root}
	for _, tc := range []struct {
		end  retroEnd
		tree treeAt
		// left is whether the retro's issue has left the tree at the end: a child's todo in a tree
		// that is not live re-admits the child into a tree of its own.
		left bool
		wake bool
		stop architectStop
		// pod is whether the end queues the IssueSuspend of the root's pod.
		pod bool
		// closes is each member whose close rows the hour after the end writes, nil for none.
		closes []string
		// own is whether the end stops the retro agent's claim and the pod of the retro's issue.
		own bool
	}{
		{end: endReport, tree: treeRuns, own: true},
		{end: endBound, tree: treeRuns, own: true},
		{end: endChildTodo, tree: treeRuns},
		{end: endReport, tree: treeLingers, stop: stopNow, pod: true, own: true},
		{end: endReport, tree: treeLingers, wake: true, stop: stopAtHour, pod: true, own: true},
		{end: endBound, tree: treeLingers, stop: stopNow, pod: true, own: true},
		{end: endBound, tree: treeLingers, wake: true, stop: stopAtHour, pod: true, own: true},
		{end: endChildTodo, tree: treeLingers, left: true, stop: stopNow, pod: true},
		{end: endChildTodo, tree: treeLingers, left: true, wake: true, stop: stopAtHour, pod: true},
		{end: endRootTodo, tree: treeLingers},
		{end: endRootTodo, tree: treeLingers, wake: true},
		{end: endReport, tree: treePast, stop: stopNow, pod: true, closes: both, own: true},
		{end: endReport, tree: treePast, wake: true, stop: stopAtHour, pod: true, closes: both, own: true},
		{end: endBound, tree: treePast, stop: stopNow, pod: true, closes: both, own: true},
		{end: endBound, tree: treePast, wake: true, stop: stopAtHour, pod: true, closes: both, own: true},
		{end: endChildTodo, tree: treePast, left: true, stop: stopNow, pod: true, closes: rootOnly},
		{end: endChildTodo, tree: treePast, left: true, wake: true, stop: stopAtHour, pod: true, closes: rootOnly},
		{end: endRootTodo, tree: treePast},
		{end: endRootTodo, tree: treePast, wake: true},
	} {
		name := fmt.Sprintf("%s while the tree %s", tc.end, tc.tree)
		if tc.left {
			name += ", the child re-admitted into a tree of its own"
		}
		if tc.wake {
			name += ", a retro's wake of the root architect queued"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := &hourAfterRetros{retroRig: newRetroRig(t)}
			h.engine.cfg.Pods = wakePods(t, h.pool)
			h.engine.cfg.Linger = 72 * time.Hour
			h.root(t, phase.Implementing)
			h.member(t, child, root, phase.Admitted, 43)
			h.runTo(t, child, phase.Implementing)
			h.person(t, child, "done")
			// key's retro of run is the one the end ends, the tree's last; first's of firstRun
			// ends by its report before it.
			key, run, first, firstRun := child, uint64(3), root, uint64(1)
			if tc.end == endRootTodo {
				key, run, first, firstRun = root, 1, child, 3
			}
			if tc.tree != treeRuns {
				h.person(t, root, "done")
				lingering(t, h.pool, root)
				seedRunClaim(t, h.pool, root, claim.RoleArchitect, 1, "suspended", "daemon", "session-architect")
				if result := h.report(t, first, firstRun); result.Refusal != nil {
					t.Fatalf("the report of %s's retro was refused: %+v", first, result.Refusal)
				}
			}
			if tc.tree == treePast {
				h.lingerEnds(t)
				// The linger's end's tree_close retires the root architect once the outbox runs it.
				h.architectAt(t, "retired", "", false)
			}
			retro, found := runRetroOf(t, h.pool, key, run)
			if !found || !retro.open() || retro.tree != root {
				t.Fatalf("%s's run_retros row of run %d before the end = %+v (found %t), want its retro after the close open in %s's tree", key, run, retro, found, root)
			}
			h.clock.now = h.clock.now.Add(10 * time.Minute)
			if tc.end == endBound && h.clock.now.Before(*retro.boundAt) {
				h.clock.now = retro.boundAt.Add(time.Minute)
			}
			if tc.wake {
				h.wakeArchitect(t, "finding")
			}

			mark := h.mark(t)
			switch {
			case tc.end == endReport:
				if result := h.report(t, key, run); result.Refusal != nil {
					t.Fatalf("the report of %s's retro was refused: %+v", key, result.Refusal)
				}
			case tc.end == endBound:
				h.apply(fmt.Sprintf("retro-bound:%s:%d", key, run), intake.RetroBoundEnded{Issue: key, Generation: run})
			case tc.end == endChildTodo && !tc.left:
				h.person(t, key, "todo")
			default:
				h.readmits(t, key)
			}
			if row, found := runRetroOf(t, h.pool, key, run); !found || row.ended == nil || row.tree != root {
				t.Fatalf("%s's run_retros row of run %d after the end = %+v (found %t), want it ended in %s's tree", key, run, row, found, root)
			}
			if after := issueRecord(t, h.pool, key); after == nil || (after.Tree != root) != tc.left {
				t.Fatalf("%s after the end = %+v, want it in %s's tree unless it left it (%t)", key, after, root, tc.left)
			}
			assertRetroIssueStops(t, h.pool, key, mark, tc.own)
			stops, suspensions := h.stopsSince(t, mark)
			if tc.stop == stopNow {
				if len(stops) != 1 || stops[0].Op != "suspend" || stops[0].Generation != 1 {
					t.Fatalf("stops of the root architect at the end = %+v, want one suspend", stops)
				}
			} else if len(stops) != 0 {
				t.Fatalf("stops of the root architect at the end = %+v, want none", stops)
			}
			if want := map[bool]int{true: 1}[tc.pod]; suspensions != want {
				t.Fatalf("issue_suspend rows of the root at the end = %d, want %d", suspensions, want)
			}
			assertTreeCloses(t, h.pool, mark, nil)
			if tc.stop == stopNone {
				if got := rowsSince(t, h.pool, root, string(record.OutboxKindLingerClose), mark); len(got) != 0 {
					t.Fatalf("linger_close rows of the root at the end = %+v, want none", got)
				}
				return
			}

			h.endedBy(t, key, run, mark)
			if tc.wake {
				h.ranWake(t)
				h.architectAt(t, "idle", "", false)
			}
			h.after(pods.RetroWakeWindow + time.Minute)
			hour := h.mark(t)
			if err := h.applyStop(); err != nil {
				t.Fatalf("the linger_close row the end queued, once the hour after it has passed = %v, want it applied", err)
			}
			stops, suspensions = h.stopsSince(t, hour)
			var suspends []record.SuperviseRequest
			for _, stop := range stops {
				if stop.Op == "suspend" {
					suspends = append(suspends, stop)
				}
			}
			want := 0
			if tc.tree == treeLingers && tc.stop == stopAtHour {
				want = 1
			}
			if len(suspends) != want || suspensions != want {
				t.Fatalf("at the hour after the end: suspends of the root architect %+v and %d issue_suspend rows of the root, want %d of each", suspends, suspensions, want)
			}
			assertTreeCloses(t, h.pool, hour, tc.closes)
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
