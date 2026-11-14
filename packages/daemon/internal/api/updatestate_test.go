package api

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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

// The four states, each read from the claim's own update columns and from nothing else. The
// order of the questions is what the last row holds: an answer stands whatever the ask clock
// holds.
func TestUpdateStateOfAnswersOneOfTheFourStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(c *supervise.Claim)
		want UpdateState
	}{
		{name: "no round is open", set: func(c *supervise.Claim) {
			c.UpdateRound, c.UpdateOpenedAt = "", nil
		}, want: UpdateStateNone},
		{name: "a round is open and its ask has not gone out", want: UpdateStateOpen},
		{name: "the round asked this worker", set: func(c *supervise.Claim) {
			c.UpdateTaskedAt = &askedAt
		}, want: UpdateStateAsked},
		{name: "the worker answered", set: func(c *supervise.Claim) {
			c.UpdateTaskedAt, c.UpdateSignalledAt = &askedAt, &answeredAt
			c.UpdateHead = "4c1b8e2a6f9d3047b5e1c8a2f6d940b3e7c5a128"
		}, want: UpdateStateAnswered},
		// A planner before its first push answers with no head at all, which is the reading
		// `updateHead` alone cannot tell from a claim nobody ever asked.
		{name: "the worker answered and reported no head", set: func(c *supervise.Claim) {
			c.UpdateTaskedAt, c.UpdateSignalledAt = &askedAt, &answeredAt
		}, want: UpdateStateAnswered},
		// A worker can answer before the sweep publishes the round's ask, so the answer
		// stands with the ask clock still empty (supervise's
		// TestPodUpdateARoundDoesNotTakeTheAnswerOfTheRoundItReplaced).
		{name: "the worker answered before the round published its ask", set: func(c *supervise.Claim) {
			c.UpdateSignalledAt = &answeredAt
		}, want: UpdateStateAnswered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := inRound("legion-legion-legion-208-implementer", tc.set)
			if got := UpdateStateOf(c); got != tc.want {
				t.Fatalf("UpdateStateOf(%+v) = %q, want %q", c, got, tc.want)
			}
		})
	}
}
