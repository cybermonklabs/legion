package api

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// roundAt is a pod round of the fixture's pod, the round every claim of that pod shares.
var roundAt = supervise.PodRound(statePodUID)

// askedAt and answeredAt are the two clocks an update round writes on a claim: when its task was
// last sent and when its worker answered.
var (
	askedAt    = time.Date(2026, 9, 22, 9, 16, 0, 0, time.UTC)
	answeredAt = time.Date(2026, 9, 22, 9, 17, 0, 0, time.UTC)
)

// inRound is a claim the round above stands over, as the supervisor stores it, with the update
// columns the case sets. The claim holds a process in the round's own pod, which is what makes
// it a member of that round: a claim that left the pod records the round it left besides
// (supervise.Claim.UpdateLeftRound).
func inRound(token claim.Token, set func(c *supervise.Claim)) supervise.Claim {
	c := supervise.Claim{
		Token: token, Project: testProject, Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RoleImplementer, State: supervise.StateIdle, Session: "ses_" + string(token),
		UpdateRound: roundAt, UpdateOpenedAt: &askedAt,
		Locator: &runtime.Locator{
			Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: statePodUID + "/1",
			Sandbox: &runtime.SandboxLocator{
				Namespace: "legion", Name: "legion-legion-legion-208", PodUID: statePodUID,
				Container: "roles", Generation: 1,
			},
		},
	}
	if set != nil {
		set(&c)
	}
	return c
}

// The five states, each read from the claim's own update columns and from nothing else. The
// order of the questions is what the last three rows hold: an answer stands whatever the ask
// clock holds, and a mark with no ask clock is the one history the columns tell apart.
func TestUpdateStateOfAnswersOneOfTheFiveStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(c *supervise.Claim)
		want UpdateState
	}{
		{name: "no round is open", set: func(c *supervise.Claim) {
			c.UpdateRound, c.UpdateOpenedAt = "", nil
		}, want: UpdateStateNone},
		{name: "a round is open and its task has not gone out", want: UpdateStateOpen},
		{name: "the round asked this worker", set: func(c *supervise.Claim) {
			c.UpdateTaskedAt = &askedAt
		}, want: UpdateStateAsked},
		{name: "the round means to ask a second time for the push check", set: func(c *supervise.Claim) {
			c.UpdateReaskedForPushCheck = true
		}, want: UpdateStatePushCheckReask},
		{name: "the worker answered", set: func(c *supervise.Claim) {
			c.UpdateTaskedAt, c.UpdateSignalledAt = &askedAt, &answeredAt
			c.UpdateHead = "4c1b8e2a6f9d3047b5e1c8a2f6d940b3e7c5a128"
		}, want: UpdateStateAnswered},
		// A planner before its first push answers with no head at all, which is the reading
		// `updateHead` alone cannot tell from a claim nobody ever asked.
		{name: "the worker answered and reported no head", set: func(c *supervise.Claim) {
			c.UpdateTaskedAt, c.UpdateSignalledAt = &askedAt, &answeredAt
		}, want: UpdateStateAnswered},
		// A turn the daemon sent nothing for took the answer back, and the ask's own clock went
		// with it (supervise's takeBackSignal). The round is open with no ask outstanding, which
		// is `open` and never a claim nobody asked.
		{name: "a turn took the answer back", set: func(c *supervise.Claim) {
			c.UpdateTaskedAt, c.UpdateSignalledAt, c.UpdateHead = nil, nil, ""
		}, want: UpdateStateOpen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := inRound("legion-legion-legion-208-implementer", tc.set)
			if got := UpdateStateOf(c); got != tc.want {
				t.Fatalf("UpdateStateOf(%+v) = %q, want %q", c, got, tc.want)
			}
		})
	}
}

// The take-back answer of a claim whose answer a turn took back says a further turn takes
// nothing back: there is no answer left to take. The state beside it is `open`.
func TestAClaimWhoseAnswerATurnTookBackTakesNothingBack(t *testing.T) {
	c := inRound("legion-legion-legion-208-implementer", func(c *supervise.Claim) {
		c.UpdateTaskedAt, c.UpdateSignalledAt = nil, nil
	})

	if got := UpdateStateOf(c); got != UpdateStateOpen {
		t.Errorf("UpdateStateOf = %q, want %q: the round is open with no ask outstanding", got, UpdateStateOpen)
	}
	if c.UnpromptedTurnTakesAnswerBack() {
		t.Errorf("the take-back answer of %+v is true, want false: no answer stands to be taken back", c)
	}
}

// The push-check re-ask in its three cases. The mark stands to the end of the round, so it is
// read beside whatever state the claim has reached: only the claim the round has still to ask
// again reads `push-check-reask`, and the two the round moved on from read `asked` and
// `answered` with the mark still set.
func TestThePushCheckReaskIsReadBesideTheStateTheClaimReaches(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(c *supervise.Claim)
		want UpdateState
	}{
		{name: "the mark stands and the ask clock is empty", set: func(c *supervise.Claim) {
			c.UpdateReaskedForPushCheck = true
		}, want: UpdateStatePushCheckReask},
		{name: "the round asked it again", set: func(c *supervise.Claim) {
			c.UpdateReaskedForPushCheck, c.UpdateTaskedAt = true, &askedAt
		}, want: UpdateStateAsked},
		{name: "the worker answered the second ask", set: func(c *supervise.Claim) {
			c.UpdateReaskedForPushCheck, c.UpdateTaskedAt = true, &askedAt
			c.UpdateSignalledAt, c.UpdateAnsweredByTurnEnd = &answeredAt, true
		}, want: UpdateStateAnswered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := inRound("legion-legion-legion-208-implementer", tc.set)
			if got := UpdateStateOf(c); got != tc.want {
				t.Errorf("UpdateStateOf = %q, want %q", got, tc.want)
			}
			if !c.UpdateReaskedForPushCheck {
				t.Errorf("the push-check mark of %+v is false, want the mark the round still holds", c)
			}
		})
	}
	// The round's end clears the column with the round, so no claim in no round carries it.
	ended := inRound("legion-legion-legion-208-implementer", func(c *supervise.Claim) {
		c.UpdateRound, c.UpdateOpenedAt = "", nil
	})
	if UpdateStateOf(ended) != UpdateStateNone || ended.UpdateReaskedForPushCheck {
		t.Errorf("a claim in no round reads %q with the mark %v, want %q with no mark",
			UpdateStateOf(ended), ended.UpdateReaskedForPushCheck, UpdateStateNone)
	}
}

// An answer recorded while the ask clock is empty reads `answered` whichever way the clock came
// to be empty: a take-back cleared it, or a push-check re-ask was decided and its second task
// has not gone out. The answer stands in both, and a turn the daemon sends nothing for would
// take it back, because the worker gave it with the ready_for_update call.
func TestAnAnswerWithNoAskClockReadsAnswered(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(c *supervise.Claim)
	}{
		{name: "after a take-back", set: func(c *supervise.Claim) {
			c.UpdateTaskedAt, c.UpdateSignalledAt = nil, &answeredAt
		}},
		{name: "after a push-check re-ask", set: func(c *supervise.Claim) {
			c.UpdateTaskedAt, c.UpdateSignalledAt, c.UpdateReaskedForPushCheck = nil, &answeredAt, true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := inRound("legion-legion-legion-208-implementer", tc.set)
			if got := UpdateStateOf(c); got != UpdateStateAnswered {
				t.Errorf("UpdateStateOf = %q, want %q: the answer stands whatever the ask clock holds", got, UpdateStateAnswered)
			}
			if !c.UnpromptedTurnTakesAnswerBack() {
				t.Errorf("the take-back answer of %+v is false, want true: the worker answered with the call", c)
			}
		})
	}
}

// machineHolding is a machine of this harness over the claim named, admitted with its update
// columns exactly as the case set them. It is how a case reaches a column no route of this
// package writes (supervise.Claim.UpdateLeftRound), and the machine it answers decides a turn
// start as the daemon's own machines do.
func (h *harness) machineHolding(c supervise.Claim) *supervise.Machine {
	h.t.Helper()
	if _, err := h.store.OpenTreeLifecycle(h.ctx, testProject, c.Tree, treelifecycle.AuthorityWorkflow); err != nil {
		h.t.Fatalf("open the lifecycle of %s: %v", c.Tree, err)
	}
	m, _, err := h.supervisor.Create(h.ctx, c, "role prompt")
	if err != nil {
		h.t.Fatalf("create %s: %v", c.Token, err)
	}
	return m
}

// The take-back answer agrees with the machine that makes the decision, over the three exits a
// turn the daemon sent nothing for reaches: no answer standing, the claim gone from its round's
// pod, and an answer the end of the task's turn gave. The fourth is the answer the worker gave
// with the call, which such a turn takes back.
//
// Each row is a claim of its own, started into a turn the daemon sent nothing for — no delivery
// is pending, so the machine reads the start as no task's (supervise's turnStarted) — and what
// the machine then did with the answer is held to what the report said it would do.
func TestTheTakeBackAnswerAgreesWithTheMachine(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name string
		role claim.Role
		set  func(c *supervise.Claim)
		want bool
	}{
		{name: "no round is open", role: claim.RolePlanner, set: func(c *supervise.Claim) {
			c.UpdateRound, c.UpdateOpenedAt, c.UpdateTaskedAt = "", nil, nil
		}},
		{name: "no answer stands", role: claim.RoleImplementer, set: func(c *supervise.Claim) {
			c.UpdateTaskedAt = &askedAt
		}},
		{name: "the claim left its round's pod", role: claim.RoleTester, set: func(c *supervise.Claim) {
			c.UpdateTaskedAt, c.UpdateSignalledAt = &askedAt, &answeredAt
			c.UpdateLeftRound = roundAt
		}},
		{name: "the end of the task's turn gave the answer", role: claim.RoleReviewer, set: func(c *supervise.Claim) {
			c.UpdateTaskedAt, c.UpdateSignalledAt = &askedAt, &answeredAt
			c.UpdateAnsweredByTurnEnd = true
		}},
		{name: "the worker answered with the call", role: claim.RoleArchitect, set: func(c *supervise.Claim) {
			c.UpdateTaskedAt, c.UpdateSignalledAt = &askedAt, &answeredAt
		}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := claim.NewToken(testProject, "LEGION-208", tc.role)
			if err != nil {
				t.Fatalf("token: %v", err)
			}
			c := inRound(token, tc.set)
			c.Role = tc.role
			m := h.machineHolding(c)
			stood := m.Claim().UpdateSignalledAt != nil
			reported := m.Claim().UnpromptedTurnTakesAnswerBack()
			if reported != tc.want {
				t.Errorf("the take-back answer = %v, want %v", reported, tc.want)
			}

			if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
				t.Fatalf("a turn the daemon sent nothing for: %v", err)
			}

			took := stood && m.Claim().UpdateSignalledAt == nil
			if took != reported {
				t.Fatalf("the machine took the answer back = %v, and the report said %v: the two must agree", took, reported)
			}
		})
	}
}

// The report answers for a turn the daemon sent nothing for and for no other turn. The round's
// own task's turn is the fourth exit of the machine's rule, and it keeps the answer: a claim
// whose report says a turn would take its answer back keeps that answer through a turn of its
// round's own ask.
func TestTheTakeBackAnswerClaimsNothingOfTheRoundsOwnTask(t *testing.T) {
	h := newHarness(t)
	token, err := claim.NewToken(testProject, "LEGION-210", claim.RoleImplementer)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	// The ask the round made, as every ask of a round carries it: the round's own id in the
	// delivery id is what says the turn is this round's task's (supervise's updatesRound).
	ask := "update:" + roundAt + ":ASKWORD"
	c := inRound(token, func(c *supervise.Claim) {
		c.Tree, c.Issue = "LEGION-210", "LEGION-210"
		c.UpdateTaskedAt, c.UpdateSignalledAt = &askedAt, &answeredAt
		c.Pending = &supervise.Delivery{ID: ask, Task: "push your work", QueuedAt: askedAt}
	})
	m := h.machineHolding(c)
	if !m.Claim().UnpromptedTurnTakesAnswerBack() {
		t.Fatalf("the take-back answer of %+v is false, want true: the worker answered with the call", m.Claim())
	}

	if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token, DeliveryID: ask}); err != nil {
		t.Fatalf("the turn of the round's own ask: %v", err)
	}

	if m.Claim().UpdateSignalledAt == nil {
		t.Fatalf("the turn of the round's own ask took the answer back; the report answers for a turn the daemon sent nothing for alone")
	}
}
