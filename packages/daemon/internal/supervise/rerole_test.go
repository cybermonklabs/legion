package supervise

import (
	"context"
	"errors"
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

	h.must(RequestResume{Claim: testToken, By: SuspendedByOperator})
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
		if err := h.m.Handle(ctx, RequestResume{Claim: testToken, By: SuspendedByOperator}); err != nil {
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
