package admit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// keyedToken is the claim token of the agent named name on issue, in the test project.
func keyedToken(t *testing.T, issue string, name claim.Agent) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.AgentToken(project, issue, name)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// seedKeyedPlan gives key the plan `implementer:a tester:a reviewer` at its generation, key a named
// `implementer`, with task a standing at step 2, the tester's.
func seedKeyedPlan(t *testing.T, pool *pgxpool.Pool, key string, generation uint64) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		architect := keyedToken(t, key, claim.Agent(claim.RoleArchitect))
		v := record.PlanVersion{Issue: key, Generation: generation, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner, GivenBy: architect, GivenAt: fixedNow}
		task := record.PlanTask{Issue: key, Generation: generation, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
		steps := []record.PlanStep{
			{Issue: key, Generation: generation, Task: "a", Position: 1, Role: "implementer", Type: "implementation", AgentKey: "a", Agent: "implementer"},
			{Issue: key, Generation: generation, Task: "a", Position: 2, Role: "tester", Type: "testing", AgentKey: "a", Agent: "implementer"},
			{Issue: key, Generation: generation, Task: "a", Position: 3, Role: "reviewer", Type: "review", Agent: "reviewer"},
		}
		for _, put := range []error{
			records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps),
			records.MoveTask(ctx, tx, key, generation, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning),
		} {
			if put != nil {
				t.Fatalf("seed the plan of %s: %v", key, put)
			}
		}
	})
}

// seedStoredClaim records token, a claim of issue in tree whose stored role is role, in state,
// suspended by suspendedBy ("" for none).
func seedStoredClaim(t *testing.T, pool *pgxpool.Pool, tree, issue string, token claim.Token, role claim.Role, state, suspendedBy string) {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
		values ($1, $2, $3, $4, $5, 1, 'ses', '/sessions/ses.jsonl', $6, 0, 0, 0, 0, $7)`,
		string(token), project, tree, issue, string(role), state, suspendedBy); err != nil {
		t.Fatalf("seed the claim %s: %v", token, err)
	}
}

// startsReaching is every supervise start of issue by the claim it reaches when it runs, by the rule
// the outbox's start acts on (record.QueuedClaim): the claim the row names, else the claim of the
// role's worker as the record holds it now.
func startsReaching(t *testing.T, pool *pgxpool.Pool, issue string) map[claim.Token][]record.SuperviseRequest {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	reached := map[claim.Token][]record.SuperviseRequest{}
	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		phases, err := record.NewStore().Phases(ctx, tx, issue)
		if err != nil {
			t.Fatalf("read the phases rows of %s: %v", issue, err)
		}
		rows, err := tx.Query(ctx, `select payload from outbox where kind = 'supervise' and issue = $1 order by id`, issue)
		if err != nil {
			t.Fatalf("read the supervise rows of %s: %v", issue, err)
		}
		defer rows.Close()
		for rows.Next() {
			var payload []byte
			if err := rows.Scan(&payload); err != nil {
				t.Fatalf("read a supervise row of %s: %v", issue, err)
			}
			var request record.SuperviseRequest
			if err := json.Unmarshal(payload, &request); err != nil {
				t.Fatalf("decode a supervise row of %s: %v", issue, err)
			}
			if request.Op != "start" {
				continue
			}
			token, err := record.QueuedClaim(project, issue, request.Role, request.Claim, phases)
			if err != nil {
				t.Fatalf("the claim a start of %s's %s reaches: %v", issue, request.Role, err)
			}
			reached[token] = append(reached[token], request)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the supervise rows of %s: %v", issue, err)
		}
	})
	return reached
}

// A member mid-testing of `implementer:a tester:a reviewer` holds key a's claim `…-implementer`,
// whose stored role is tester, and the re-admission of its tree brings it back. The promotion
// starts the tester's step on the run of that claim, so one start reaches `…-implementer`, the
// promotion's, and none reaches `…-tester`. A walk that reads the role's own token reads the run of
// `…-tester`, which holds no claim, and also starts the role implementer with no task, so the agent
// gets two starts. Beside it, the member holds `…-tester`, the fresh tester of version 1: the walk
// brings it back on one start that names it, since a row that named the role alone would reach
// `…-implementer`.
func TestAReadmittedMemberResumesItsKeyedAgentOnItsSecondStep(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale bool
	}{
		{"the keyed agent alone", false},
		{"beside version 1's tester", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Title: "root", Tree: "LEGION-208", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LastDispatchSeq: 1}
			putIssue(t, pool, root)
			parentKey := root.Key
			const member = "LEGION-209"
			putIssue(t, pool, record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Title: "mid-phase child", Tree: root.Key, Parent: &parentKey,
				Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1})
			seedKeyedPlan(t, pool, member, 1)
			keyed := keyedToken(t, member, "implementer")
			stale := keyedToken(t, member, "tester")
			inTx(t, pool, func(tx pgx.Tx) {
				for _, row := range []record.PhaseRow{
					{Issue: member, Role: claim.RoleImplementer, Claim: keyed, StepTask: "a", StepPlace: 1, Entry: 1},
					{Issue: member, Role: claim.RoleTester, Claim: keyed, StepTask: "a", StepPlace: 2, Entry: 2},
				} {
					if err := record.NewStore().PutPhase(context.Background(), tx, row); err != nil {
						t.Fatalf("seed the %s row: %v", row.Role, err)
					}
				}
			})
			seedStoredClaim(t, pool, root.Key, member, keyed, claim.RoleTester, "suspended", "daemon")
			if tc.stale {
				seedStoredClaim(t, pool, root.Key, member, stale, claim.RoleTester, "suspended", "daemon")
			}

			apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: root.Title, Rank: root.Rank, HandedOver: handed}, engine)

			reached := startsReaching(t, pool, member)
			if got := reached[keyed]; len(got) != 1 || got[0].Role != claim.RoleTester || got[0].Phase != phase.Testing || got[0].Task == "" {
				t.Errorf("the starts that reach key a's agent %s = %+v, want the one promotion of the tester's step", keyed, got)
			}
			want := 0
			if tc.stale {
				want = 1
			}
			got := reached[stale]
			if len(got) != want || (want == 1 && got[0].Claim != stale) {
				t.Errorf("the starts that reach %s = %+v, want %d naming it", stale, got, want)
			}
		})
	}
}

// A slot holder testing on `implementer:a tester:a reviewer` is read as the claim of its step's
// agent, `…-implementer`, which the tester's row names: the repair over phase workers weighs the
// run of the agent that works the step, not the run of the role's own token, which holds no claim.
func TestASlotHolderIsReadAsTheClaimOfItsStepsAgent(t *testing.T) {
	pool := migratedPool(t)
	const key = "LEGION-310"
	seedSlottedInPhase(t, pool, testProject, key, "A", phase.Testing, nil)
	keyed := keyedToken(t, key, "implementer")
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx,
			record.PhaseRow{Issue: key, Role: claim.RoleTester, Claim: keyed, StepTask: "a", StepPlace: 2, Entry: 2}); err != nil {
			t.Fatalf("seed the tester's row: %v", err)
		}
	})
	seedStoredClaim(t, pool, key, key, keyed, claim.RoleTester, "ready", "")
	admission := newAdmission(t, 1, nil)

	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		issues, slots, facts, err := admission.reread(ctx, tx)
		if err != nil {
			t.Fatalf("read the line: %v", err)
		}
		read, err := admission.readSlotHolders(ctx, tx, issues, slots, facts)
		if err != nil {
			t.Fatalf("read the slot holders: %v", err)
		}
		var tester []claim.Token
		for _, held := range read.holders {
			if held.issue.Key == key && held.role == claim.RoleTester {
				tester = append(tester, held.token)
			}
		}
		if len(tester) != 1 || tester[0] != keyed || read.runs[keyed].Claim == nil {
			t.Fatalf("the tester held of %s = %v, want the claim %s with its run", key, tester, keyed)
		}
	})
}

// A start the tester of `implementer:a tester:a reviewer` owes, held for its slot, would reach key
// a's claim `…-implementer`, which the tester's row names. That claim was stopped after the row was
// written, so the row would only start a stopped worker, and the repair finishes it. A repair that
// weighed the claim of the role's own token, `…-tester`, finds no claim and keeps the row.
func TestAHeldStartOfAKeyedAgentStoppedSinceIsFinished(t *testing.T) {
	pool := migratedPool(t)
	const key = "LEGION-310"
	seedSlottedInPhase(t, pool, testProject, key, "A", phase.Testing, nil)
	keyed := keyedToken(t, key, "implementer")
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx,
			record.PhaseRow{Issue: key, Role: claim.RoleTester, Claim: keyed, StepTask: "a", StepPlace: 2, Entry: 2}); err != nil {
			t.Fatalf("seed the tester's row: %v", err)
		}
		if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), key, record.SuperviseRequest{Op: "start", Tree: key,
			Role: claim.RoleTester, Generation: 1, Phase: phase.Testing, Task: "Test the widget.", NeedsSlot: true}, fixedNow); err != nil {
			t.Fatalf("queue the tester's held start: %v", err)
		}
	})
	seedStoredClaim(t, pool, key, key, keyed, claim.RoleTester, "retired", "")
	if _, err := pool.Exec(context.Background(), `update claims set retired_at_write_mark = (select max(write_mark) + 1 from outbox) where token = $1`,
		string(keyed)); err != nil {
		t.Fatalf("mark the stop after the held start: %v", err)
	}
	admission := newAdmission(t, 1, nil)
	var held int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'supervise' and issue = $1`, key).Scan(&held); err != nil || held != 1 {
		t.Fatalf("the held starts of %s = %d, %v; want the tester's one: the case's premise", key, held, err)
	}

	// The Dispatch fact is applied through the mirror, as the daemon applies it, whose write of the
	// issue's row is the record the pass runs for (Admission.Apply's pass rule).
	applyMirrored(t, pool, admission, "observe", intake.DispatchIssue{Key: "LEGION-9", Seq: 5, Type: "issue.updated", Status: "backlog",
		Title: "someone else's", Rank: "Z", ResolvedRepo: "acme/widgets"}, workflow.New(record.NewStore(), workflow.Config{Project: testProject,
		Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil))

	var after int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'supervise' and issue = $1 and payload->>'role' = 'tester'`, key).Scan(&after); err != nil {
		t.Fatalf("read the held starts of %s: %v", key, err)
	}
	if after != 0 {
		t.Fatalf("the held start of the tester's step stands, want it finished: key a's agent %s was stopped after it", keyed)
	}
}

// A held start is weighed against the claim it names, the claim it was queued for and the one the
// outbox acts on, never against the claim the role's row names by then. The tester's row of
// `implementer:a tester:a reviewer` names key a's claim `…-implementer`, stopped after the start was
// written. A start that names key b, an agent of the tester's role at another step, is no start of
// key a's: the repair keeps it, and the outbox's own fence decides it. Before, the repair weighed
// every held start against the claim the row names and finished key b's start for key a's stop.
// The controls: the same start naming key a is finished, and so is a start the release before
// wrote, which names no claim and is weighed against the claim of the row, as before.
func TestAHeldStartIsWeighedAgainstTheClaimItNames(t *testing.T) {
	const key = "LEGION-312"
	for _, tc := range []struct {
		name     string
		named    string
		finished bool
	}{
		{"a start of key b, named reviewer, while the row names key a, stopped since", "reviewer", false},
		{"the control: a start naming key a", "implementer", true},
		{"a start the release before wrote, naming no claim", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedSlottedInPhase(t, pool, testProject, key, "A", phase.Testing, nil)
			keyed := keyedToken(t, key, "implementer")
			start := record.SuperviseRequest{Op: "start", Tree: key, Role: claim.RoleTester, Generation: 1, Phase: phase.Testing,
				Task: "Test the widget.", NeedsSlot: true}
			if tc.named != "" {
				start.Claim = keyedToken(t, key, claim.Agent(tc.named))
			}
			inTx(t, pool, func(tx pgx.Tx) {
				if err := record.NewStore().PutPhase(context.Background(), tx,
					record.PhaseRow{Issue: key, Role: claim.RoleTester, Claim: keyed, StepTask: "a", StepPlace: 2, Entry: 2}); err != nil {
					t.Fatalf("seed the tester's row: %v", err)
				}
				if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), key, start, fixedNow); err != nil {
					t.Fatalf("queue the tester's held start: %v", err)
				}
			})
			seedStoredClaim(t, pool, key, key, keyed, claim.RoleTester, "retired", "")
			if _, err := pool.Exec(context.Background(), `update claims set retired_at_write_mark = (select max(write_mark) + 1 from outbox) where token = $1`,
				string(keyed)); err != nil {
				t.Fatalf("mark the stop after the held start: %v", err)
			}
			admission := newAdmission(t, 1, nil)

			// Applied through the mirror, as the daemon applies a Dispatch fact, whose write of the
			// issue's row is the record the pass runs for (Admission.Apply's pass rule).
			applyMirrored(t, pool, admission, "observe", intake.DispatchIssue{Key: "LEGION-9", Seq: 5, Type: "issue.updated", Status: "backlog",
				Title: "someone else's", Rank: "Z", ResolvedRepo: "acme/widgets"}, workflow.New(record.NewStore(), workflow.Config{Project: testProject,
				Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil))

			var after int
			if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'supervise' and issue = $1 and payload->>'role' = 'tester'`,
				key).Scan(&after); err != nil {
				t.Fatalf("read the held starts of %s: %v", key, err)
			}
			if finished := after == 0; finished != tc.finished {
				t.Fatalf("the held start naming %q: finished %t, want %t; key a's agent %s was stopped after it", start.Claim, finished, tc.finished, keyed)
			}
		})
	}
}

// A member held at step 3 of `implementer:a reviewer:b implementer:b`, the step of key b, named
// reviewer, is re-admitted with its tree, and its held phase is retried: step 3's work reaches key
// b alone. The member keeps its generation, its plan and its task's place through the re-admission,
// so its rows keep the steps they ran (record.Store.ClearTreeGeneration) and every reader of the
// implementer's worker reads key b off step 3's row (record.WorkerClaim), the claim the retry's
// start names (workflow's restart). Before, the clear folded the implementer's rows into one of no
// step, which every reader took for the role's own token, key a's: the admission pass then read key
// a as behind step 3's entry and sent it that work beside the retry's start for key b, so two
// agents worked one step in one working copy.
func TestARetriedHeldMemberOfAReadmittedTreeSendsItsStepToItsOwnAgentAlone(t *testing.T) {
	pool := migratedPool(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	admission := newAdmission(t, 3, quiet)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, quiet)
	root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Title: "root",
		Tree: "LEGION-208", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LastDispatchSeq: 1}
	putIssue(t, pool, root)
	parentKey := root.Key
	const member = "LEGION-209"
	putIssue(t, pool, record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Title: "held child", Tree: root.Key, Parent: &parentKey, Phase: phase.Held, Hold: &record.Hold{From: phase.Implementing}, Generation: 1,
		Status: "in_progress", Rank: "B", LastDispatchSeq: 1})
	keyA, keyB := keyedToken(t, member, "implementer"), keyedToken(t, member, "reviewer")
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		v := record.PlanVersion{Issue: member, Generation: 1, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: keyedToken(t, root.Key, claim.Agent(claim.RoleArchitect)), GivenAt: fixedNow}
		task := record.PlanTask{Issue: member, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
		steps := []record.PlanStep{
			{Issue: member, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation", AgentKey: "a", Agent: "implementer"},
			{Issue: member, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: "review", AgentKey: "b", Agent: "reviewer"},
			{Issue: member, Generation: 1, Task: "a", Position: 3, Role: "implementer", Type: "implementation", AgentKey: "b", Agent: "reviewer"},
		}
		for _, put := range []error{
			records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps),
			records.MoveTask(ctx, tx, member, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 3}, record.TaskRunning),
			records.PutPhase(ctx, tx, record.PhaseRow{Issue: member, Role: claim.RoleImplementer, Claim: keyA, StepTask: "a", StepPlace: 1, Entry: 1,
				EntryRaisedAt: fixedNow.Add(-3 * time.Hour)}),
			records.PutPhase(ctx, tx, record.PhaseRow{Issue: member, Role: claim.RoleReviewer, Claim: keyB, StepTask: "a", StepPlace: 2, Entry: 2,
				EntryRaisedAt: fixedNow.Add(-2 * time.Hour)}),
			records.PutPhase(ctx, tx, record.PhaseRow{Issue: member, Role: claim.RoleImplementer, Claim: keyB, StepTask: "a", StepPlace: 3, Entry: 3,
				EntryTask: "Implement it again. Issue: LEGION-209. Phase: implementing.", EntryRaisedAt: fixedNow.Add(-time.Hour)}),
		} {
			if put != nil {
				t.Fatalf("seed %s held at step 3: %v", member, put)
			}
		}
	})
	seedStoredClaim(t, pool, root.Key, member, keyA, claim.RoleImplementer, "ready", "")
	seedStoredClaim(t, pool, root.Key, member, keyB, claim.RoleImplementer, "ready", "")

	apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: root.Title, Rank: root.Rank, HandedOver: handed}, engine)
	if got := issue(t, pool, member); got.Phase != phase.Held || got.Generation != 1 {
		t.Fatalf("%s after its tree's re-admission stands in %s at generation %d, want held at generation 1", member, got.Phase, got.Generation)
	}
	result, err := intake.ApplyFact(context.Background(), pool, "api", "retry-held-step-3",
		intake.RetryOrEscalate{Issue: member, Decision: intake.RetryDecision, Reason: "try again"}, engine, admission)
	if err != nil || result.Refusal != nil {
		t.Fatalf("the retry of %s's held step 3 = %+v, %v; want it taken", member, result.Refusal, err)
	}
	if got := issue(t, pool, member); got.Phase != phase.Implementing {
		t.Fatalf("%s after the retry stands in %s, want implementing", member, got.Phase)
	}

	// A start that carries a task is work; the promotion brings each stored claim back with none.
	work := map[claim.Token][]record.SuperviseRequest{}
	for token, starts := range startsReaching(t, pool, member) {
		for _, start := range starts {
			if start.Role == claim.RoleImplementer && start.Task != "" {
				work[token] = append(work[token], start)
			}
		}
	}
	if got := work[keyB]; len(got) != 1 || got[0].Claim != keyB || got[0].Entry != 3 {
		t.Errorf("the implementer's work that reaches key b %s = %+v, want the retry's one start of step 3 at entry 3", keyB, got)
	}
	if got := work[keyA]; len(got) != 0 {
		t.Errorf("the implementer's work that reaches key a %s = %+v, want none: step 3 is key b's", keyA, got)
	}
}
