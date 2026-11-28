package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// keyedTester records LEGION-209, a child of LEGION-208 testing, and its tester row as the
// tester's step of `implementer:a tester:a reviewer` leaves it: it names key a's claim,
// `…-implementer`, and runs step 2 of task a.
func keyedTester(t *testing.T, h *harness, keyed claim.Token) {
	t.Helper()
	ctx := context.Background()
	parent := "LEGION-208"
	h.recordIssue(record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: parent,
		Project: testProject, Title: "LEGION-209", Parent: &parent, Phase: phase.Testing, Generation: 1, Status: "in_progress"})
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutPhase(ctx, tx, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleTester, Claim: keyed,
			StepTask: "a", StepPlace: 2, Entry: 2})
	}); err != nil {
		t.Fatalf("seed the tester's row of LEGION-209: %v", err)
	}
}

// While the tester's step of `implementer:a tester:a reviewer` runs, an operator's spawn of
// (LEGION-209, tester) acts on the claim that works that step, key a's `…-implementer`, which is
// launching, so the spawn is refused as one of a launched claim and launches nothing. A route that
// derived the role's own token would create and launch `…-tester`, a claim that runs no step.
func TestAnOperatorsSpawnOfARoleReachesTheClaimOfItsStepsAgent(t *testing.T) {
	h := newHarness(t)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()); recorder.Code != http.StatusCreated {
		t.Fatalf("spawn the tree's architect = %d; body %s", recorder.Code, recorder.Body)
	}
	worker := SpawnRequest{Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleImplementer, Prompt: "Reply ready and wait."}
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", worker); recorder.Code != http.StatusCreated {
		t.Fatalf("spawn key a's agent = %d; body %s", recorder.Code, recorder.Body)
	}
	keyed, err := claim.NewToken(testProject, "LEGION-209", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	keyedTester(t, h, keyed)

	tester := worker
	tester.Role = claim.RoleTester
	wantRefusal(t, h.operator(http.MethodPost, "/legion/v1/operator/claims", tester), http.StatusConflict,
		"spawn refused: the claim is launching (the claim is already launched)")
	if spawns := h.runtime.CallsOf("Spawn"); len(spawns) != 2 {
		t.Fatalf("spawns = %d, want the architect's and key a's agent's alone", len(spawns))
	}
	stale, err := claim.NewToken(testProject, "LEGION-209", claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	if _, supervised := h.supervisor.Machine(stale); supervised {
		t.Fatalf("the spawn of the tester made the claim %s, which works no step", stale)
	}
}

// While the tester's step of `implementer:a tester:a reviewer` runs, the controller's fresh
// session of (LEGION-209, tester) names the claim that works that step, key a's `…-implementer`:
// it is launching, so the fresh session is refused naming it. A route that derived the role's own
// token would answer that it supervises no claim `…-tester`.
func TestTheControllersFreshSessionOfARoleNamesTheClaimOfItsStepsAgent(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	keyed, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	keyedTester(t, h, keyed)

	recorder := h.request(http.MethodPost, "/legion/v1/claims/fresh-session",
		ClaimRequest{GrantID: controllerGrant(t, h).GrantID, Issue: "LEGION-209", Role: string(claim.RoleTester)}, nil)

	wantFreshSessionRefusal(t, recorder, http.StatusConflict, claimUnsettled,
		unsettledRefusal(keyed, supervise.StateLaunching, wantFreshSessionReaches))
}

// wantFreshSessionRefusal holds a refusal to its status, its code and its sentence: the shape
// both doors give the refusals the fresh session writes itself.
func wantFreshSessionRefusal(t *testing.T, recorder *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, status, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if failure.Code != code || failure.Error != message {
		t.Fatalf("refusal = %+v, want %q and %q", failure, code, message)
	}
}

// movesTheTesterStep writes the tester's row of LEGION-209 as a later entry of its step leaves it
// when another agent, `…-reviewer`, works that step: the fact that overtakes a route between its
// read of the step's claim and its act on that claim.
func movesTheTesterStep(t *testing.T, h *harness) claim.Token {
	t.Helper()
	later, err := claim.NewToken(testProject, "LEGION-209", claim.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutPhase(ctx, tx, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleTester, Claim: later,
			StepTask: "a", StepPlace: 2, Entry: 3})
	}); err != nil {
		t.Fatalf("move the tester's step of LEGION-209 to %s: %v", later, err)
	}
	return later
}

// An operator's spawn of (LEGION-209, tester) acts on the claim that works the tester's step, and
// a later entry of that step that commits for another agent after the route read the claim and
// before it launched it moves the step from under the request: the spawn is refused naming both
// claims, launches nothing and gives the operator's task to no one, and the claim it had just
// created is retired, so no tree waits on it. The route reads the step's claim again once it holds
// the claim's machine, as the outbox's start reads its issue again after it creates a claim. The
// control is the same spawn with nothing moving: it launches key a's agent as the tester.
func TestAnOperatorsSpawnWhoseStepMovesUnderItLaunchesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		moves bool
	}{
		{name: "control: nothing moves the step while the spawn runs"},
		{name: "a later entry of the step names another agent before the launch", moves: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()); recorder.Code != http.StatusCreated {
				t.Fatalf("spawn the tree's architect = %d; body %s", recorder.Code, recorder.Body)
			}
			keyed, err := claim.NewToken(testProject, "LEGION-209", claim.RoleImplementer)
			if err != nil {
				t.Fatal(err)
			}
			keyedTester(t, h, keyed)
			var later claim.Token
			if tc.moves {
				h.supervisor.beforeCreate = func() { later = movesTheTesterStep(t, h) }
			}
			spawns := len(h.runtime.CallsOf("Spawn"))

			recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims",
				SpawnRequest{Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleTester, Prompt: "Reply ready and wait.", Task: "Test it."})
			launched := h.runtime.CallsOf("Spawn")[spawns:]
			if !tc.moves {
				if recorder.Code != http.StatusCreated || len(launched) != 1 || launched[0].Spec.Claim != keyed || launched[0].Spec.Role != claim.RoleTester {
					t.Fatalf("the spawn = %d launching %+v; body %s; want key a's agent %s launched as the tester", recorder.Code, launched, recorder.Body, keyed)
				}
				return
			}
			wantRefusal(t, recorder, http.StatusConflict, fmt.Sprintf(
				"spawn refused: the tester's step of LEGION-209 moved from claim %s to claim %s under the request; nothing was launched or sent, so ask again", keyed, later))
			if len(launched) != 0 {
				t.Fatalf("the spawn launched %+v, want nothing: the step it was for moved to %s", launched, later)
			}
			if m, ok := h.supervisor.Machine(keyed); ok && (m.Claim().State != supervise.StateRetired || m.Claim().Pending != nil) {
				t.Fatalf("the claim the spawn created is %s holding %+v, want it retired holding no task", m.Claim().State, m.Claim().Pending)
			}
		})
	}
}
