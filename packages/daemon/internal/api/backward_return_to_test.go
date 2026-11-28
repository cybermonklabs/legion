package api

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// request_backward_move takes a role the deployment defines as a phase worker of its own issue, and
// takes returnTo, the step the move names by its place: the checker's move with returnTo 1 is the
// one fact BackwardMove with the checker as requester and returnTo 1, and the owner's move with
// returnTo 2 carries it as well. A returnTo below 1 names no step and is refused before anything
// is recorded. A phase worker's move carries the entry its claim serves (PhaseEntry), which the
// workflow holds to the worker's phases row under the fact's lock: 0 for a claim that took no task
// of an entry, and 2 for the checker that took entry 2's task; the owner's move carries none. A
// phase worker's move names the claim its grant carries (By), which the workflow holds to the claim
// the role's row names on its step.
func TestTheBackwardRouteTakesADeploymentRoleAndReturnTo(t *testing.T) {
	const key = "LEGION-208"
	one, two, zero := 1, 2, 0
	t.Run("the checker's move", func(t *testing.T) {
		h, facts, _ := newArchitectHarness(t, nil, nil)
		seedIssueAt(t, h, key, phase.Step)
		checker := newLiveClaim(t, h, key, "checker")

		answer := h.request(http.MethodPost, "/legion/v1/phase/backward", PhaseBackwardRequest{GrantID: checker.grant(t), To: phase.Implementing,
			Reason: "the build is broken", ReturnTo: &one}, nil)

		if answer.Code != http.StatusOK {
			t.Fatalf("the checker's move = %d %s, want 200", answer.Code, answer.Body)
		}
		want := []intake.Fact{intake.BackwardMove{Issue: key, Requester: "checker", To: phase.Implementing, Reason: "the build is broken", ReturnTo: 1,
			By: checker.token}}
		if got := facts.recorded(); !reflect.DeepEqual(got, want) {
			t.Fatalf("facts = %#v, want %#v", got, want)
		}
	})
	t.Run("the checker's move names the entry its claim serves", func(t *testing.T) {
		h, facts, _ := newArchitectHarness(t, nil, nil)
		seedIssueAt(t, h, key, phase.Step)
		checker := newLiveClaim(t, h, key, "checker")
		takeTask(t, h, checker.token, "outbox:77", "check "+key, 2, 1)

		answer := h.request(http.MethodPost, "/legion/v1/phase/backward", PhaseBackwardRequest{GrantID: checker.grant(t), To: phase.Implementing,
			Reason: "the build is broken"}, nil)

		if answer.Code != http.StatusOK {
			t.Fatalf("the checker's move = %d %s, want 200", answer.Code, answer.Body)
		}
		want := []intake.Fact{intake.BackwardMove{Issue: key, Requester: "checker", To: phase.Implementing, Reason: "the build is broken", PhaseEntry: 2,
			By: checker.token}}
		if got := facts.recorded(); !reflect.DeepEqual(got, want) {
			t.Fatalf("facts = %#v, want %#v", got, want)
		}
	})
	t.Run("the owner's move", func(t *testing.T) {
		h, facts, _ := newArchitectHarness(t, nil, nil)
		seedIssueAt(t, h, key, phase.Accepting)
		architect := newLiveClaim(t, h, key, claim.RoleArchitect)

		answer := h.request(http.MethodPost, "/legion/v1/phase/backward", PhaseBackwardRequest{GrantID: architect.grant(t), Issue: key, To: phase.Step,
			Reason: "the check missed a case", ReturnTo: &two}, nil)

		if answer.Code != http.StatusOK {
			t.Fatalf("the owner's move = %d %s, want 200", answer.Code, answer.Body)
		}
		// The architect's move carries its request (its claim, the issue's generation and the run
		// of the step its read found, none here), which the workflow judges again under the lock.
		want := []intake.Fact{intake.BackwardMove{Issue: key, Requester: claim.RoleArchitect, To: phase.Step, Reason: "the check missed a case", ReturnTo: 2,
			By: architect.token, Generation: 1}}
		if got := facts.recorded(); !reflect.DeepEqual(got, want) {
			t.Fatalf("facts = %#v, want %#v", got, want)
		}
	})
	t.Run("returnTo 0", func(t *testing.T) {
		h, facts, _ := newArchitectHarness(t, nil, nil)
		seedIssueAt(t, h, key, phase.Step)
		checker := newLiveClaim(t, h, key, "checker")

		answer := h.request(http.MethodPost, "/legion/v1/phase/backward", PhaseBackwardRequest{GrantID: checker.grant(t), To: phase.Implementing,
			Reason: "the build is broken", ReturnTo: &zero}, nil)

		assertFailureText(t, answer, http.StatusBadRequest, "INVALID_RETURN_TO",
			"returnTo is 0, and it names a step of the task by its place, 1 for its first; nothing was recorded")
		if got := facts.recorded(); len(got) != 0 {
			t.Fatalf("a refused move recorded %#v, want nothing", got)
		}
	})
}
