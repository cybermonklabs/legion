package api

// R6 of dispatch://INFRA-2883: the daemon charges no launch budget for a generation whose
// registration or ready it refused itself. The memory is this package's (Answering), the decision
// over it internal/supervise's (LaunchEnding, chargesLaunchBudget), and these rows drive the two
// together over a real machine: the refusal is written where the daemon writes it — the start
// handler, or a route that could not answer inside its bound — and the launch that then ends is
// held to the budget it left behind.
//
// Every row here serves the daemon's routes on its memory first (servingHarness), so the ending
// each drives is read from a refusal or from nothing. The ending read from a daemon that has not
// served a route yet is internal/daemon's own row
// (TestALaunchEndedBeforeTheFirstRouteWasServedChargesNothing).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// servingHarness is a harness whose server remembers what it refused, whose supervision reads
// that memory, and whose routes are already serving on it, as the daemon's boot leaves them
// (Answering.Routes).
func servingHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarnessWith(t, harnessOptions{relaunchable: true, refusals: true})
	h.refusals.Routes(h.handler)
	return h
}

// refusedWhileStarting posts one registration to the API this process serves while the daemon
// starts: the start handler answers its uncoded 503 and remembers the launch by the hash of the
// boot token the body carried, having read no store, since there is none open to read.
func (h *harness) refusedWhileStarting(bootToken, session string) {
	h.t.Helper()
	body, err := json.Marshal(claim.RegisterRequest{
		BootToken: bootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: "agent-" + session,
	})
	if err != nil {
		h.t.Fatalf("build the registration: %v", err)
	}
	recorder := httptest.NewRecorder()
	h.refusals.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/legion/v1/claims/register", bytes.NewReader(body)))
	wantRefusal(h.t, recorder, http.StatusServiceUnavailable, startRefusal)
}

// budgets is the claim's launch budget as its machine holds it, with the decision that moved it
// finished.
func (h *harness) budgets(token claim.Token) supervise.Budgets {
	h.t.Helper()
	m, supervised := h.supervisor.Machine(token)
	if !supervised {
		h.t.Fatalf("no machine for %s", token)
	}
	m.Wait()
	return m.Claim().Budgets
}

// wantLaunchBudget holds a claim to a launch failure count, in the machine and in the store
// alike: a charge the machine made and did not write would be charged again at the next boot.
func (h *harness) wantLaunchBudget(token claim.Token, want int, why string) {
	h.t.Helper()
	if got := h.budgets(token).LaunchFailures; got != want {
		h.t.Fatalf("%s's launch failures = %d, want %d: %s", token, got, want, why)
	}
	if got := h.stored(token).Budgets.LaunchFailures; got != want {
		h.t.Fatalf("%s's stored launch failures = %d, want %d: %s", token, got, want, why)
	}
}

// A generation whose registration this daemon refused with its start-or-busy answer, and whose
// process then ended for want of that answer, costs the claim nothing: the daemon's own slowness
// is what ended it. The refusal is the one a route writes when it could not answer inside its
// bound (refuseRegistrationUnanswered); the claim beside it, whose launch nothing refused, is
// what says the harness still charges.
func TestALaunchEndedAfterTheDaemonsOwnRefusalChargesNothing(t *testing.T) {
	h := servingHarness(t)
	refused, boot := h.launch("LEGION-2883", claim.RoleImplementer)
	answered, _ := h.launch("LEGION-2884", claim.RoleImplementer)
	h.refusals.RefusedRegistration(boot)

	h.relaunch(refused)
	h.relaunch(answered)

	h.wantLaunchBudget(refused, 0, "this daemon refused that launch's registration itself")
	h.wantLaunchBudget(answered, 1, "nothing refused that launch")
	if generation := h.stored(refused).Generation; generation != 2 {
		t.Fatalf("the waived claim is at generation %d, want it relaunched to 2", generation)
	}
}

// A registration the start handler refuses carries no claim: it arrives before the daemon has a
// store to resolve its boot token in, so the refusal is remembered by that token's hash alone.
// The waiver is what that hash is for — the claim whose launch minted the token carries the same
// hash (supervise.Claim.BootTokenHash), and the launch that ends once the daemon is up is found
// by it.
func TestARefusalGivenBeforeTheStoreOpensStillWaivesItsCharge(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{relaunchable: true, refusals: true})
	refused, boot := h.launch("LEGION-2883", claim.RoleImplementer)
	answered, _ := h.launch("LEGION-2884", claim.RoleImplementer)

	// The daemon is still starting: one of the two agents registers and is refused.
	h.refusedWhileStarting(boot, "ses_implementer")
	// It comes up, and both processes are found gone.
	h.refusals.Routes(h.handler)
	h.relaunch(refused)
	h.relaunch(answered)

	h.wantLaunchBudget(refused, 0, "the start handler refused that launch's registration")
	h.wantLaunchBudget(answered, 1, "the start handler never saw that launch")
}

// A ready carries no boot token, so a ready this daemon refused is remembered by the claim and
// the generation the request named, and waives that generation's launch and no other claim's.
func TestARefusedReadyWaivesByItsClaimAndGeneration(t *testing.T) {
	h := servingHarness(t)
	refused, _ := h.launch("LEGION-2883", claim.RoleImplementer)
	answered, _ := h.launch("LEGION-2884", claim.RoleImplementer)
	h.refusals.RefusedReady(refused, 1)

	h.relaunch(refused)
	h.relaunch(answered)

	h.wantLaunchBudget(refused, 0, "this daemon refused that generation's ready itself")
	h.wantLaunchBudget(answered, 1, "the refused ready named another claim")
}

// The waiver goes with the generation it was given for. The refusals of a generation that has
// ended are forgotten with it, so the launch that follows starts with no memory of them and is
// charged like any other: a daemon that was slow once does not excuse a claim for ever.
func TestTheWaiverGoesWithItsGeneration(t *testing.T) {
	h := servingHarness(t)
	token, boot := h.launch("LEGION-2883", claim.RoleImplementer)
	h.refusals.RefusedRegistration(boot)
	h.refusals.RefusedReady(token, 1)

	h.relaunch(token)

	h.wantLaunchBudget(token, 0, "this daemon refused the first generation itself")
	if h.refusals.Refused(token, 1, supervise.HashBootToken(boot)) {
		t.Fatal("the ended generation's refusals are still remembered, want them forgotten with it")
	}

	// The second generation ends with nothing refused of it.
	h.relaunch(token)

	h.wantLaunchBudget(token, 1, "nothing was refused of the second generation")
}

// The waiver is bounded. A claim whose generations this daemon waives launch_failure_limit times
// in a row has spent it: the charge stands again however the next launch ended, and the claim
// fails at its limit, which holds the issue and tells the architect as any other fault does. The
// count is the daemon's own and goes with it, so a restart between two waived generations starts
// the run again — a claim must not inherit the slowness of a daemon that is no longer running.
func TestTheChargeStandsAgainAfterTheWaiverLimit(t *testing.T) {
	t.Run("the run of waived generations is spent", func(t *testing.T) {
		h := servingHarness(t)
		limit := h.supervisor.deps.Limits.LaunchFailures
		token, boot := h.launch("LEGION-2883", claim.RoleImplementer)

		for range limit {
			h.refusals.RefusedRegistration(boot)
			boot = h.relaunch(token)
		}

		h.wantLaunchBudget(token, 0, "every one of those generations was refused by this daemon")
		for charge := 1; charge <= limit; charge++ {
			h.refusals.RefusedRegistration(boot)
			boot = h.relaunch(token)
			if charge < limit {
				h.wantLaunchBudget(token, charge, "the waiver was spent before this launch")
			}
		}

		h.wantLaunchBudget(token, limit, "the charge stands once the waiver is spent")
		if state := h.stored(token).State; state != supervise.StateFailed {
			t.Fatalf("the claim is %s, want it failed once its launch budget ran out", state)
		}
	})

	t.Run("a daemon restart starts the run again", func(t *testing.T) {
		h := servingHarness(t)
		limit := h.supervisor.deps.Limits.LaunchFailures
		token, boot := h.launch("LEGION-2883", claim.RoleImplementer)
		for range limit {
			h.refusals.RefusedRegistration(boot)
			boot = h.relaunch(token)
		}

		// The daemon restarts: a memory of its own, which holds nothing of what the daemon
		// before it refused or waived, and the claim is re-adopted from the store under it.
		restarted := testAnswering(t)
		restarted.Routes(h.handler)
		h.refusals = restarted
		h.supervisor.deps.LaunchEnded = restarted.LaunchEnded
		h.supervisor.deps.LaunchWaived = restarted.LaunchWaived
		h.supervisor.restart(t, token)

		for range limit {
			h.refusals.RefusedRegistration(boot)
			boot = h.relaunch(token)
		}

		h.wantLaunchBudget(token, 0, "the restarted daemon's own run of waivers is not yet spent")
		if state := h.stored(token).State; state == supervise.StateFailed {
			t.Fatal("the claim failed, want the restarted daemon to have waived its own generations")
		}
	})
}

// A registration this daemon answers and the claim's machine accepts ends the run of waived
// generations and clears what the daemon refused of that claim: the launch got what every waived
// launch was refused, so a claim that reaches a registration between two slow boots never walks
// into the waiver's bound.
func TestAnAcceptedRegistrationClearsTheClaimsRefusalsAndItsWaivers(t *testing.T) {
	h := servingHarness(t)
	limit := h.supervisor.deps.Limits.LaunchFailures
	token, boot := h.launch("LEGION-2883", claim.RoleImplementer)
	for range limit {
		h.refusals.RefusedRegistration(boot)
		boot = h.relaunch(token)
	}

	h.registered(boot, "ses_implementer")

	if h.refusals.Refused(token, h.stored(token).Generation, supervise.HashBootToken(boot)) {
		t.Fatal("the accepted launch is still remembered as refused, want the memory cleared")
	}
	// The run is over, so the next launch this daemon refuses is waived again rather than
	// charged against a budget the earlier slowness had spent.
	boot = h.relaunch(token)
	h.refusals.RefusedRegistration(boot)
	h.relaunch(token)

	h.wantLaunchBudget(token, 1, "only the generation no refusal ended is charged")
}
