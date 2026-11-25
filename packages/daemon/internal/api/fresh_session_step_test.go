package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// movingWorkflow is the workflow a route's fact reaches, with move committed first: a fact that
// landed between the route's acts on its claim and the fact the route applies. It runs move in the
// fact's own transaction, once, before the engine reads the record, as a fact serialized before
// this one would have left it.
type movingWorkflow struct {
	engine *workflow.Engine
	move   func(ctx context.Context, tx pgx.Tx) error
}

func (w *movingWorkflow) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if move := w.move; move != nil {
		w.move = nil
		if err := move(ctx, tx); err != nil {
			return intake.Result{}, err
		}
	}
	return w.engine.Apply(ctx, tx, fact)
}

// testerStarts is the entry each start of the tester LEGION-209's outbox holds carries, oldest
// first: the start a phase's restart queues hands the entry the tester's row names to the claim of
// the agent on that step (workflow.Engine.restart), so its entry says whose step it starts.
func testerStarts(t *testing.T, h *harness) []string {
	t.Helper()
	rows, err := h.store.Pool().Query(context.Background(), `select coalesce(payload->>'entry', '') from outbox
		where issue = 'LEGION-209' and kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'tester' order by id`)
	if err != nil {
		t.Fatalf("read the tester's starts: %v", err)
	}
	defer rows.Close()
	starts := []string{}
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			t.Fatalf("scan a tester's start: %v", err)
		}
		starts = append(starts, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the tester's starts: %v", err)
	}
	return starts
}

// Key a's agent, `…-implementer`, works the tester's step of LEGION-209 at entry 2 and is suspended
// between turns, recording its session, when the controller asks a fresh session of (LEGION-209,
// tester). The route reads the claim of the agent on that step, leaves its session behind, brings it
// back and starts the tester's phase again for it (intake.RestartPhase), which queues the start of
// the entry the tester's row names. A later entry of the step for another agent, `…-reviewer`, at
// entry 3, that commits after the route's read moves the step from under the request:
//
//   - before the request's first act on the claim, the request is refused with nothing of the
//     claim changed: its session stays recorded and it stays suspended, and no start of the
//     tester's step is queued;
//   - after that act and before the phase's restart, the claim is back in a new session, as the
//     request had run before the move, and the restart is refused, since it names the claim it was
//     asked for and the step is another's: the step `…-reviewer` has just begun is not started
//     again with the retry's task, and the answer says the phase was not restarted.
//
// The control is the same request with nothing moving: the claim is back in a new session and the
// tester's step is started again at its own entry, 2.
func TestAControllersFreshSessionWhoseStepMovesUnderItRestartsNoOtherAgentsStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		// at is where the later entry lands: "" for the control, "act" before the request's first
		// act on the claim, "fact" after the acts and before the phase's restart.
		at string
	}{
		{name: "control: nothing moves the step while the fresh session runs"},
		{name: "a later entry of the step lands before the request's first act on the claim", at: "act"},
		{name: "a later entry of the step lands before the phase's restart", at: "fact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
				Steps: config.DesignGateRootIssues.Steps()}, nil)
			moving := &movingWorkflow{engine: engine}
			h := freshSessionHarness(t, moving)
			keyed, err := claim.NewToken(testProject, "LEGION-209", claim.RoleImplementer)
			if err != nil {
				t.Fatal(err)
			}
			later, err := claim.NewToken(testProject, "LEGION-209", claim.RoleReviewer)
			if err != nil {
				t.Fatal(err)
			}
			keyedTester(t, h, keyed)
			if _, err := h.store.OpenTreeLifecycle(ctx, testProject, "LEGION-208", treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("open the lifecycle of LEGION-208: %v", err)
			}
			m, _, err := h.supervisor.Create(ctx, supervise.Claim{Token: keyed, Project: testProject, Tree: "LEGION-208", Issue: "LEGION-209",
				Role: claim.RoleTester, State: supervise.StateQueued}, "role prompt")
			if err != nil {
				t.Fatalf("create key a's claim: %v", err)
			}
			if err := m.Handle(ctx, supervise.RequestSpawn{Claim: keyed}); err != nil {
				t.Fatalf("spawn key a's agent: %v", err)
			}
			h.ready(keyed, "ses_keyed", m.Claim().Generation)
			if err := m.Handle(ctx, supervise.RequestSuspend{Claim: keyed, By: supervise.SuspendedByDaemon, Reason: "its turn ended"}); err != nil {
				t.Fatalf("suspend key a's agent: %v", err)
			}
			if c := m.Claim(); c.State != supervise.StateSuspended || c.Session != "ses_keyed" {
				t.Fatalf("key a's claim is %s recording %q, want it suspended on its session: the case's premise", c.State, c.Session)
			}
			moved := record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleTester, Claim: later, StepTask: "a", StepPlace: 3, Entry: 3}
			switch tc.at {
			case "act":
				h.supervisor.beforeMachine = func() {
					if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error { return record.NewStore().PutPhase(ctx, tx, moved) }); err != nil {
						t.Errorf("move the tester's step: %v", err)
					}
				}
			case "fact":
				moving.move = func(ctx context.Context, tx pgx.Tx) error { return record.NewStore().PutPhase(ctx, tx, moved) }
			}

			recorder := h.request(http.MethodPost, "/legion/v1/claims/fresh-session",
				ClaimRequest{GrantID: controllerGrant(t, h).GrantID, Issue: "LEGION-209", Role: string(claim.RoleTester)}, nil)
			starts, after := testerStarts(t, h), m.Claim()
			switch tc.at {
			case "":
				if recorder.Code != http.StatusOK || after.Session != "" || len(starts) != 1 || starts[0] != "2" {
					t.Fatalf("the fresh session = %d (%s), the claim records %q, the tester's starts carry the entries %v; want 200, no session, and one start of entry 2",
						recorder.Code, recorder.Body, after.Session, starts)
				}
			case "act":
				if recorder.Code != http.StatusConflict {
					t.Fatalf("the fresh session of a step that moved before its first act = %d (%s), want 409", recorder.Code, recorder.Body)
				}
				if after.State != supervise.StateSuspended || after.Session != "ses_keyed" {
					t.Fatalf("key a's claim is %s recording %q, want it untouched: suspended on ses_keyed", after.State, after.Session)
				}
				if len(starts) != 0 {
					t.Fatalf("the tester's starts carry the entries %v, want none: the step is %s's at entry 3", starts, later)
				}
			case "fact":
				var answer FreshSessionResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || recorder.Code != http.StatusOK || answer.PhaseNotRestarted == "" {
					t.Fatalf("the fresh session = %d (%s), want 200 saying the phase was not restarted", recorder.Code, recorder.Body)
				}
				if after.Session != "" {
					t.Fatalf("key a's claim records %q, want it back in a new session: the request's acts ran before the move", after.Session)
				}
				if len(starts) != 0 {
					t.Fatalf("the tester's starts carry the entries %v, want none: the step is %s's at entry 3, not restarted for another claim", starts, later)
				}
			}
		})
	}
}

// Key a's agent, `…-implementer`, works the tester's step of LEGION-209, and the tester's own
// claim of an earlier version of the plan, `…-tester`, is live beside it: two claims of one role.
// A notice of provider errors names the tester's own claim by its token, and the controller asks a
// fresh session of that claim (ClaimRequest.Claim, contract 259). The route reaches the claim the
// body names: the tester's own claim is back in a new session, and key a's agent, the step's
// worker, is left as it was, suspended on its session. The named claim works no step, so no start
// of the tester's step is queued for it, and the answer says the phase was not restarted. Naming
// no claim still reaches the step's worker. A body naming a claim of another issue, or a claim
// that holds another role, is refused, and nothing of either claim changes: so is a body naming the
// implementer with key a's agent's own token, `…-implementer`, the token the implementer's role
// would reach, since that claim holds the tester's role now.
func TestAControllersFreshSessionReachesTheClaimItNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		// role is the role the body names, the tester's where it is "".
		role claim.Role
		// named is the claim the body names, as a function of the tester's own claim and key a's.
		named func(own, keyed claim.Token) string
		code  int
		// refusal is the refusal's code the answer carries, "" for an answer that is no refusal.
		refusal string
		// fresh is the claim the request brings back in a new session, "" for none.
		fresh func(own, keyed claim.Token) claim.Token
	}{
		{name: "the tester's own claim, by its token", named: func(own, _ claim.Token) string { return string(own) },
			code: http.StatusOK, fresh: func(own, _ claim.Token) claim.Token { return own }},
		{name: "no claim: the step's worker", named: func(_, _ claim.Token) string { return "" },
			code: http.StatusOK, fresh: func(_, keyed claim.Token) claim.Token { return keyed }},
		{name: "a claim of another issue", named: func(_, _ claim.Token) string { return "legion-legion-legion-210-tester" },
			code: http.StatusBadRequest, refusal: "INVALID_CLAIM", fresh: func(_, _ claim.Token) claim.Token { return "" }},
		{name: "a claim of the issue that holds another role", named: func(_, _ claim.Token) string { return "legion-legion-legion-209-reviewer" },
			code: http.StatusConflict, refusal: "CLAIM_NOT_OF_ROLE", fresh: func(_, _ claim.Token) claim.Token { return "" }},
		{name: "key a's agent's own token, named as the implementer it no longer is", role: claim.RoleImplementer,
			named: func(_, keyed claim.Token) string { return string(keyed) },
			code:  http.StatusConflict, refusal: "CLAIM_NOT_OF_ROLE", fresh: func(_, _ claim.Token) claim.Token { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
				Steps: config.DesignGateRootIssues.Steps()}, nil)
			h := freshSessionHarness(t, engine)
			keyed, err := claim.NewToken(testProject, "LEGION-209", claim.RoleImplementer)
			if err != nil {
				t.Fatal(err)
			}
			own, err := claim.NewToken(testProject, "LEGION-209", claim.RoleTester)
			if err != nil {
				t.Fatal(err)
			}
			keyedTester(t, h, keyed)
			if _, err := h.store.OpenTreeLifecycle(ctx, testProject, "LEGION-208", treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("open the lifecycle of LEGION-208: %v", err)
			}
			machines := map[claim.Token]*supervise.Machine{}
			for token, role := range map[claim.Token]claim.Role{keyed: claim.RoleTester, own: claim.RoleTester, "legion-legion-legion-209-reviewer": claim.RoleReviewer} {
				m, _, err := h.supervisor.Create(ctx, supervise.Claim{Token: token, Project: testProject, Tree: "LEGION-208", Issue: "LEGION-209",
					Role: role, State: supervise.StateQueued}, "role prompt")
				if err != nil {
					t.Fatalf("create %s: %v", token, err)
				}
				if err := m.Handle(ctx, supervise.RequestSpawn{Claim: token}); err != nil {
					t.Fatalf("spawn %s: %v", token, err)
				}
				h.ready(token, "ses_"+string(token), m.Claim().Generation)
				if err := m.Handle(ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByDaemon, Reason: "its turn ended"}); err != nil {
					t.Fatalf("suspend %s: %v", token, err)
				}
				machines[token] = m
			}

			role := tc.role
			if role == "" {
				role = claim.RoleTester
			}
			recorder := h.request(http.MethodPost, "/legion/v1/claims/fresh-session",
				ClaimRequest{GrantID: controllerGrant(t, h).GrantID, Issue: "LEGION-209", Role: string(role),
					Claim: tc.named(own, keyed)}, nil)
			if recorder.Code != tc.code {
				t.Fatalf("the fresh session = %d (%s), want %d", recorder.Code, recorder.Body, tc.code)
			}
			if tc.refusal != "" {
				var refused Failure
				if err := json.Unmarshal(recorder.Body.Bytes(), &refused); err != nil || refused.Code != tc.refusal {
					t.Fatalf("the refusal = %+v (%v), want the code %s: %s", refused, err, tc.refusal, recorder.Body)
				}
			}
			fresh := tc.fresh(own, keyed)
			for token, m := range machines {
				c := m.Claim()
				if token == fresh {
					if c.Session != "" {
						t.Errorf("%s records %q, want it back in a new session: the request named it", token, c.Session)
					}
					continue
				}
				if c.State != supervise.StateSuspended || c.Session != "ses_"+string(token) {
					t.Errorf("%s is %s recording %q, want it untouched: suspended on its session", token, c.State, c.Session)
				}
			}
			starts := testerStarts(t, h)
			switch fresh {
			case own:
				var answer FreshSessionResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil || answer.PhaseNotRestarted == "" {
					t.Fatalf("the fresh session answered %s, want it saying the phase was not restarted: the tester's own claim works no step", recorder.Body)
				}
				if len(starts) != 0 {
					t.Fatalf("the tester's starts carry the entries %v, want none: the step is key a's", starts)
				}
			case keyed:
				if len(starts) != 1 || starts[0] != "2" {
					t.Fatalf("the tester's starts carry the entries %v, want one start of entry 2 for the step's worker", starts)
				}
			default:
				if len(starts) != 0 {
					t.Fatalf("the tester's starts carry the entries %v, want none: the request was refused", starts)
				}
			}
		})
	}
}
