package supervise

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// stepStands is a request's standing (Standing) for a step its issue still asks for.
func stepStands(ctx context.Context) context.Context {
	return Standing(ctx, func(context.Context) (bool, error) { return true, nil })
}

// A claim whose agent's next step is of another role takes that role while it runs no process: the
// store holds the new role, and the next generation launches with it on the same claim and the same
// session, so the agent does that step with its role's prompt and App. A claim that kept its role
// would do the tester's step as the implementer.
func TestARerolePersistsTheRoleAndRelaunchesOnTheSameSession(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	h.must(RequestSuspend{Claim: testToken, By: SuspendedByOperator})

	if err := h.m.Rerole(stepStands(context.Background()), claim.RoleTester); err != nil {
		t.Fatalf("re-role the suspended claim: %v", err)
	}
	if stored := h.store.load(h.token); stored.Role != claim.RoleTester {
		t.Fatalf("the stored claim's role = %s, want tester", stored.Role)
	}

	h.must(RequestResume{Claim: testToken})
	resume := h.wantCalls("Resume", 1)[0]
	if resume.Spec.Role != claim.RoleTester || resume.Spec.Claim != testToken || resume.Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("relaunched %+v, want the tester's role on %s resuming %s", resume.Spec, testToken, sessionFile)
	}
}

// A claim that still runs a process keeps its role: its process was started with the role's prompt
// and App, so the re-role is refused and the store is not written.
func TestAReroleOfAClaimThatRunsAProcessIsRefused(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)

	err := h.m.Rerole(stepStands(context.Background()), claim.RoleTester)
	if !errors.Is(err, ErrRunsAProcess) {
		t.Fatalf("the re-role of an idle claim = %v, want ErrRunsAProcess", err)
	}
	if got := h.claim().Role; got != claim.RoleImplementer {
		t.Fatalf("the claim's role = %s, want implementer", got)
	}
	if stored := h.store.load(h.token); stored.Role != claim.RoleImplementer {
		t.Fatalf("the stored claim's role = %s, want implementer", stored.Role)
	}
}

// The step a re-role is for is judged again under the machine's lock, on the record as it stands
// then (Standing): a step the issue has left since the start read it is ErrStepLeft, and a reading
// that fails is that failure. Either way the claim keeps its role and the store is not written.
// The control is the same re-role of a step that stands.
func TestAReroleForAStepTheIssueLeftIsRefused(t *testing.T) {
	failed := errors.New("the record did not answer")
	for _, tc := range []struct {
		name   string
		stands func(context.Context) (bool, error)
		want   error
	}{
		{name: "a step the issue still asks for", stands: func(context.Context) (bool, error) { return true, nil }},
		{name: "a step the issue has left", stands: func(context.Context) (bool, error) { return false, nil }, want: ErrStepLeft},
		{name: "a reading of the step that fails", stands: func(context.Context) (bool, error) { return false, failed }, want: failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			h.must(RequestSuspend{Claim: testToken, By: SuspendedByDaemon})

			err := h.m.Rerole(Standing(context.Background(), tc.stands), claim.RoleTester)
			if tc.want == nil {
				if err != nil || h.store.load(h.token).Role != claim.RoleTester {
					t.Fatalf("the re-role of a step that stands = %v, stored role %s; want the tester's", err, h.store.load(h.token).Role)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("the re-role = %v, want %v", err, tc.want)
			}
			if got, stored := h.claim().Role, h.store.load(h.token).Role; got != claim.RoleImplementer || stored != claim.RoleImplementer {
				t.Fatalf("the claim holds %s and the store %s, want the implementer in both", got, stored)
			}
		})
	}
}

// A request made on a reading of the record is judged under the machine's lock at its first act,
// and is the record's from then on (Standing). A suspension of a ready claim whose standing no
// longer holds is refused with ErrStepLeft before anything of it acts: the claim stays ready and no
// process is stopped. One whose standing holds is decided, and the next act of the same request is
// decided too though the standing would answer false by then: a move after the request's first act
// finds the request under way. The standing is read once for the two acts. A request whose reading
// fails is that failure, and changes nothing.
func TestARequestsStandingIsJudgedUnderTheLockAtItsFirstAct(t *testing.T) {
	t.Run("a standing that no longer holds", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		ctx := Standing(context.Background(), func(context.Context) (bool, error) { return false, nil })
		if err := h.m.Handle(ctx, RequestSuspend{Claim: testToken, By: SuspendedByOperator}); !errors.Is(err, ErrStepLeft) {
			t.Fatalf("the suspension = %v, want ErrStepLeft", err)
		}
		if got := h.claim().State; got != StateReady || len(h.calls("Suspend")) != 0 {
			t.Fatalf("the claim is %s with %d stops, want it ready and nothing stopped", got, len(h.calls("Suspend")))
		}
	})
	t.Run("a standing that holds at the first act", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		reads, holds := 0, true
		ctx := Standing(context.Background(), func(context.Context) (bool, error) { reads++; return holds, nil })
		if err := h.m.Handle(ctx, RequestSuspend{Claim: testToken, By: SuspendedByOperator}); err != nil {
			t.Fatalf("the suspension: %v", err)
		}
		holds = false
		if err := h.m.Handle(ctx, RequestResume{Claim: testToken}); err != nil {
			t.Fatalf("the resume of the same request: %v", err)
		}
		if got := h.claim().State; got != StateLaunching || reads != 2 {
			t.Fatalf("the claim is %s after %d reads, want launching after the filter's read and the lock's", got, reads)
		}
	})
	t.Run("a reading that fails", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		failed := errors.New("the record did not answer")
		ctx := Standing(context.Background(), func(context.Context) (bool, error) { return false, failed })
		if err := h.m.Handle(ctx, RequestDeliver{Claim: testToken, Task: "the task"}); !errors.Is(err, failed) {
			t.Fatalf("the delivery = %v, want the reading's failure", err)
		}
		if got := h.claim(); got.State != StateReady || got.Pending != nil {
			t.Fatalf("the claim is %s holding %+v, want it ready holding nothing", got.State, got.Pending)
		}
	})
}

// A claim that takes another role keeps its session, so the transcript its next generation resumes
// can hold a GitHub token of the earlier role's App that the agent printed, which GitHub accepts
// for up to an hour. The first prompt the agent takes in its new role says that every such token is
// no longer its to use, though GitHub may accept it until it expires, and that its gh and git now
// act as the new role's App; a later prompt says it no more. A daemon restart between the re-role
// and that prompt loses none of it. The control is the same claim suspended, restarted and resumed
// in the role it held: no prompt of its next generation says anything of an earlier role.
func TestTheFirstPromptAfterARoleChangeSaysTheEarlierRolesTokensAreNoLongerItsOwn(t *testing.T) {
	const (
		void = "Any GitHub token you saw in an earlier role is no longer yours to use, though GitHub may accept it until it expires: do not use it."
		app  = "Your gh and git now act as the GitHub App of the tester role."
	)
	for _, tc := range []struct {
		name string
		role claim.Role
		says bool
	}{
		{"a claim that took another role", claim.RoleTester, true},
		{"a claim that kept its role", claim.RoleImplementer, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			h.must(RequestSuspend{Claim: testToken, By: SuspendedByDaemon})
			if err := h.m.Rerole(stepStands(context.Background()), tc.role); err != nil {
				t.Fatalf("give the suspended claim the %s's role: %v", tc.role, err)
			}
			h.restart()
			before := len(h.prompts())
			h.must(RequestRuleResume{Claim: testToken, Reason: "its agent's next step is the " + string(tc.role) + "'s"})
			h.relaunched()
			// The suspension's record of what it ended can reach the agent first, in a delivery of
			// the daemon's own; a claim that holds none is given the step's task.
			if h.claim().Pending == nil {
				h.must(RequestDeliver{Claim: testToken, Task: "test the change"})
			}
			sent := h.prompts()[before:]
			if len(sent) == 0 {
				t.Fatal("the resumed generation was sent no prompt")
			}
			first := sent[0].Message
			if got := strings.Contains(first, void) && strings.Contains(first, app); got != tc.says {
				t.Fatalf("the resumed generation's first prompt says the earlier role's tokens are void = %v, want %v: %q",
					got, tc.says, first)
			}
			if !tc.says && strings.Contains(first, "earlier role") {
				t.Fatalf("the first prompt of a claim that kept its role names an earlier role: %q", first)
			}

			h.must(StreamTurnStart{Claim: testToken})
			h.must(StreamTurnEnd{Claim: testToken})
			before = len(h.prompts())
			h.must(RequestDeliver{Claim: testToken, Task: "test it again"})
			later := h.prompts()[before:]
			if len(later) == 0 {
				t.Fatal("the second task was sent as no prompt, so nothing here reads whether it repeats the sentence")
			}
			for _, prompt := range later {
				if strings.Contains(prompt.Message, "earlier role") {
					t.Fatalf("a prompt after the one a turn took names an earlier role again: %q", prompt.Message)
				}
			}
		})
	}
}

// A re-roled generation can come back holding no task and no untold record of what its let-go
// ended: the suspension's record was told already, and nothing else gives the claim a delivery. It
// is still told at its ready, in a delivery of the daemon's own that carries the sentence alone, so
// no turn another input starts runs in the new role before a prompt says the earlier role's tokens
// are no longer the agent's to use. The control is the same claim in the role it kept, which is
// sent nothing.
func TestARoleChangeIsToldToAGenerationThatHoldsNoTask(t *testing.T) {
	const (
		notYours = "Any GitHub token you saw in an earlier role is no longer yours to use"
		app      = "Your gh and git now act as the GitHub App of the tester role."
	)
	for _, tc := range []struct {
		name string
		role claim.Role
		told bool
	}{
		{"a claim that took another role", claim.RoleTester, true},
		{"a claim that kept its role", claim.RoleImplementer, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			h.must(RequestSuspend{Claim: testToken, By: SuspendedByDaemon})
			if err := h.m.Rerole(stepStands(context.Background()), tc.role); err != nil {
				t.Fatalf("give the suspended claim the %s's role: %v", tc.role, err)
			}
			stored := h.store.load(h.token)
			stored.EndedWork.Told = true
			h.store.PutClaim(h.ctx, stored)
			h.restart()
			if h.claim().Pending != nil || h.claim().EndedWork.Tells() {
				t.Fatalf("the claim holds %+v and a record that tells = %v, want neither", h.claim().Pending, h.claim().EndedWork.Tells())
			}

			before := len(h.prompts())
			h.must(RequestRuleResume{Claim: testToken, Reason: "its agent's next step is the " + string(tc.role) + "'s"})
			h.relaunched()
			sent := h.prompts()[before:]
			if !tc.told {
				if len(sent) != 0 {
					t.Fatalf("the generation of a claim that kept its role was sent %+v, want nothing", sent)
				}
				return
			}
			if len(sent) != 1 || !strings.Contains(sent[0].Message, notYours) || !strings.Contains(sent[0].Message, app) {
				t.Fatalf("the re-roled generation was sent %+v, want one prompt that says the earlier role's tokens are no longer its to use", sent)
			}
		})
	}
}
