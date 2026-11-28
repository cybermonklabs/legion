package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The issue whose fix starts, the head its check failed at, and the page of its failed job.
const (
	fixLineIssue = "LEGION-208"
	fixLineHead  = "c0de0000000000000000000000000000000000aa"
	fixLinePage  = "https://ci.example/run/7"
)

// fixLineRig is LEGION-208 in reviewing on its plan's task a, `implementer reviewer`, a task that
// takes a fix, with fix 1 reading its failed CI job at fixLineHead; the engine and the admission a
// fact is applied through, the outbox's own list of handlers; and the outbox's start executor over
// a supervisor of its own, whose machines answer whether a task is still its issue's work as the
// daemon wires them (workflowRuntime.phaseHolds) and adopt a phase task's working copy as the
// daemon does, with the implementer's and the reviewer's agents each on a fake connection.
type fixLineRig struct {
	t        *testing.T
	ctx      context.Context
	pool     *store.Pool
	records  *record.Postgres
	handlers []intake.Handler
	runner   *outbox
	sup      *supervisor
	rt       *fake.Runtime
	tokens   map[claim.Role]claim.Token
	conns    map[claim.Role]*fake.Conn
	logs     *syncBuffer
	applied  int
}

func newFixLineRig(t *testing.T) *fixLineRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	// An admitted tree, as admission leaves a root it promoted: its slot and its lifecycle. Without
	// them the admission among the fact's handlers reads the tree as not running.
	seedWorkflowTreeIn(t, st, "legion", record.Issue{Key: fixLineIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: fixLineIssue, Title: "Workflow", Phase: phase.Reviewing, Generation: 1, Status: "in_progress", Rank: "U",
		HandedOver: true})
	tokens := map[claim.Role]claim.Token{}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleReviewer} {
		token, err := claim.NewToken("legion", fixLineIssue, role)
		if err != nil {
			t.Fatalf("claim token: %v", err)
		}
		tokens[role] = token
	}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		v := record.PlanVersion{Issue: fixLineIssue, Generation: 1, Version: 1, ArtifactID: "plan-" + fixLineIssue, Source: record.PlanSourceOwner,
			GivenBy: "owner", GivenAt: time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)}
		tasks := []record.PlanTask{{Issue: fixLineIssue, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Task a.", State: record.TaskWaiting, FixesCI: true}}
		steps := []record.PlanStep{
			{Issue: fixLineIssue, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: fixLineIssue, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: "review"}}
		if err := records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			return err
		}
		if err := records.MoveTask(ctx, tx, fixLineIssue, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning); err != nil {
			return err
		}
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: fixLineIssue, Repo: "acme/widgets", Number: 42,
			Branch: "legion/" + fixLineIssue, HeadSHA: fixLineHead, MergeState: record.MergeStateClean, Failing: []string{}}); err != nil {
			return err
		}
		for role, token := range tokens {
			if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: fixLineIssue, Role: role, Claim: token, Entry: 1,
				EntryTask: "the " + string(role) + "'s step", EntryRaisedAt: time.Now().UTC()}); err != nil {
				return err
			}
		}
		return records.PutFix(ctx, tx, record.TaskFix{Issue: fixLineIssue, Generation: 1, Task: "a", Fix: 1, State: record.FixReading,
			Check: "k-ci", Head: fixLineHead, Place: 1})
	}); err != nil {
		t.Fatalf("seed %s beside fix 1 reading its job: %v", fixLineIssue, err)
	}
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(records, engine, 2, "LEGION", log)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records, log: log}).phaseHolds
	adoptingAsTheDaemonDoes(sup)
	conns := map[claim.Role]*fake.Conn{}
	for role, token := range tokens {
		conns[role] = fake.NewConn()
		sup.deps.Conns.(*fake.Conns).Register(token, conns[role])
	}
	runner := &outbox{log: log, pool: pool, dispatchProject: "LEGION", records: records,
		supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + fixLineIssue}, nil
		},
	}
	return &fixLineRig{t: t, ctx: ctx, pool: pool, records: records, handlers: []intake.Handler{engine, admission}, runner: runner,
		sup: sup, rt: rt, tokens: tokens, conns: conns, logs: logs}
}

// apply applies fact through the rig's handlers, the engine and the admission, as the outbox
// applies the facts of its reads, and fails the test on an error or a refusal.
func (r *fixLineRig) apply(fact intake.Fact) {
	r.t.Helper()
	r.applied++
	result, err := intake.ApplyFact(r.ctx, r.pool, "outbox", fmt.Sprintf("fix-line:%d", r.applied), fact, r.handlers...)
	if err != nil || result.Refusal != nil {
		r.t.Fatalf("apply %T = %+v, %v", fact, result.Refusal, err)
	}
}

// readJob applies the answer of the read of fix 1's failed job: its name and its page.
func (r *fixLineRig) readJob() {
	r.t.Helper()
	r.apply(intake.FixJobRead{Key: fixLineIssue, Generation: 1, Task: "a", Fix: 1, Name: "unit", URL: fixLinePage})
}

// owe queues the start of role in the line of starts that wait for a slot, as a phase's start is
// queued (record.Store.EnqueueStart): at phase p, for entry entry of its row.
func (r *fixLineRig) owe(role claim.Role, p phase.Phase, entry int64) {
	r.t.Helper()
	start := record.SuperviseRequest{Op: "start", Tree: fixLineIssue, Role: role, Generation: 1, Phase: p, Entry: entry,
		Task: "the " + string(role) + "'s step again", NeedsSlot: true}
	row := mustOutboxRow(r.t, fixLineIssue, start, time.Now())
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.EnqueueStart(ctx, tx, row)
	}); err != nil {
		r.t.Fatalf("queue the %s's start: %v", role, err)
	}
}

// startRows is each start of role the outbox holds for the issue, oldest first, of a fix where fix
// says so and of none otherwise.
func (r *fixLineRig) startRows(role claim.Role, fix bool) []record.OutboxRow {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = $2 order by id`, fixLineIssue, string(role))
	if err != nil {
		r.t.Fatalf("read the starts of the %s: %v", role, err)
	}
	defer rows.Close()
	var starts []record.OutboxRow
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			r.t.Fatalf("scan a start of the %s: %v", role, err)
		}
		var start record.SuperviseRequest
		if err := json.Unmarshal(row.Payload, &start); err != nil {
			r.t.Fatalf("decode the start %d: %v", row.ID, err)
		}
		if (start.Fix > 0) == fix {
			starts = append(starts, row)
		}
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the starts of the %s: %v", role, err)
	}
	return starts
}

// sends runs row, a start of role, through the start executor, readies role's claim, and waits
// for its agent to be sent the task; it answers the adoption the runtime made before that task.
func (r *fixLineRig) sends(role claim.Role, row record.OutboxRow) runtime.Adoption {
	r.t.Helper()
	if err := r.runner.execute(r.ctx, row); err != nil {
		r.t.Fatalf("run the %s's start %d: %v", role, row.ID, err)
	}
	machine, found := r.sup.Machine(r.tokens[role])
	if !found {
		r.t.Fatalf("the %s's start %d made no claim; the log:\n%s", role, row.ID, r.logs.String())
	}
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: r.tokens[role], Generation: generation},
		supervise.RequestRegister{Claim: r.tokens[role], Generation: generation, Session: "ses-" + string(role), SessionFile: "/sessions/" + string(role) + ".jsonl"},
		supervise.RequestReady{Claim: r.tokens[role], Generation: generation, Session: "ses-" + string(role)},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			r.t.Fatalf("handle %T for the %s: %v", ev, role, err)
		}
	}
	machine.Wait()
	testwait.Eventually(r.t, "the "+string(role)+"'s task to reach its agent", func() bool { return len(r.conns[role].Prompts()) == 1 })
	app := "legion-" + string(role) + "[bot]"
	var made []runtime.Adoption
	for _, call := range r.rt.CallsOf("AdoptWorkingCopy") {
		if call.Identity.Name == app {
			made = append(made, call.Adoption)
		}
	}
	if len(made) != 1 {
		r.t.Fatalf("the %s's adoptions = %+v, want one before its task", role, made)
	}
	return made[0]
}

// The read of the fix's failed job, applied as the outbox applies it, through the engine and the
// admission, starts the fix while the issue stands in reviewing: the fix's start is written outside
// the line of starts that wait for a slot, so the admission's pass in the same fact, which finishes
// each owed start of a phase the issue does not stand in, leaves it standing when the fact commits,
// and its task reaches the implementer's agent after an adoption of the fix's own working copy at
// the head its check failed at. The controls, kept: a start of the implementer written in the line
// of starts, as the fix's start was written before, is finished by the admission's pass of the next
// fact; and the fix's start with its working copy's head cleared before the executor runs it adopts
// no copy of its own, which the adoption check names.
func TestAFixStartSurvivesAdmissionInItsOwnFact(t *testing.T) {
	t.Parallel()
	r := newFixLineRig(t)

	r.readJob()

	starts := r.startRows(claim.RoleImplementer, true)
	if len(starts) != 1 {
		t.Fatalf("the fix's starts after its job's read = %d, want one standing when the fact committed", len(starts))
	}
	branch, err := claim.BesideBranch(fixLineIssue, 1, "a", 1, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("name the fix's own branch: %v", err)
	}
	if got, want := r.sends(claim.RoleImplementer, starts[0]), (runtime.Adoption{FixFrom: fixLineHead, FixBranch: branch}); got != want {
		t.Fatalf("the fix's adoption = %+v, want %+v: a working copy of its own at the head its check failed at, on its own branch", got, want)
	}

	t.Run("control: a start in the line of starts is finished by the admission", func(t *testing.T) {
		t.Parallel()
		r := newFixLineRig(t)
		r.owe(claim.RoleImplementer, phase.Implementing, 1)
		r.apply(intake.ControllerTick{})
		if starts := r.startRows(claim.RoleImplementer, false); len(starts) != 0 {
			t.Fatalf("the implementer's start in the line of starts = %d rows after the admission's pass, want none", len(starts))
		}
	})

	t.Run("control: the fix's start with no head of its copy adopts the shared copy", func(t *testing.T) {
		t.Parallel()
		r := newFixLineRig(t)
		r.readJob()
		starts := r.startRows(claim.RoleImplementer, true)
		if len(starts) != 1 {
			t.Fatalf("the fix's starts = %d, want one", len(starts))
		}
		var start record.SuperviseRequest
		if err := json.Unmarshal(starts[0].Payload, &start); err != nil {
			t.Fatal(err)
		}
		start.FixFrom = ""
		cleared, err := json.Marshal(start)
		if err != nil {
			t.Fatal(err)
		}
		row := starts[0]
		row.Payload = cleared
		if got := r.sends(claim.RoleImplementer, row); got.FixFrom != "" {
			t.Fatalf("the adoption of a start with no head of its copy = %+v, want no copy of its own", got)
		}
	})
}

// A fix's start and a reviewer's start both stand, whichever is written first: the reviewer's is
// written in the line of starts that wait for a slot, where a newer start of the issue takes an
// older one's place, and the fix's beside that line, so neither takes the other's place. In both
// orders both rows stand after the admission's pass, and each task reaches its agent.
func TestAFixStartAndAReviewerStartBothStand(t *testing.T) {
	t.Parallel()
	for _, reviewerFirst := range []bool{true, false} {
		name := "the fix's start first"
		if reviewerFirst {
			name = "the reviewer's start first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newFixLineRig(t)
			if reviewerFirst {
				r.owe(claim.RoleReviewer, phase.Reviewing, 1)
				r.readJob()
			} else {
				r.readJob()
				r.owe(claim.RoleReviewer, phase.Reviewing, 1)
			}
			r.apply(intake.ControllerTick{})

			fixes, reviews := r.startRows(claim.RoleImplementer, true), r.startRows(claim.RoleReviewer, false)
			if len(fixes) != 1 || len(reviews) != 1 {
				t.Fatalf("the starts standing = %d of the fix and %d of the reviewer, want one of each; the log:\n%s", len(fixes), len(reviews), r.logs.String())
			}
			if got := r.sends(claim.RoleImplementer, fixes[0]); got.FixFrom != fixLineHead {
				t.Fatalf("the fix's adoption = %+v, want its own copy at %s", got, fixLineHead)
			}
			if got := r.sends(claim.RoleReviewer, reviews[0]); got.FixFrom != "" {
				t.Fatalf("the reviewer's adoption = %+v, want the task's shared copy", got)
			}
		})
	}
}
