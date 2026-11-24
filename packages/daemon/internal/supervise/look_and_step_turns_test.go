package supervise

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A look of a merge's production check and a step of another task can be one agent's: the check
// goes to its task's last agent of the checking role (workflow's checkClaim), and one key across
// tasks makes that agent the agent of another task's step or retro too. That agent is one claim,
// and a claim runs one turn at a time: a task given while a turn runs waits on the claim, is sent
// into no running turn, and goes once that turn ends. So in each order, a look's task given while
// a step's turn runs and a step's task given while a look's turn runs, the second task is sent
// only after the first task's turn has ended.
//
// The fault it catches is a look and a step of one agent run in one turn, the second task sent into
// the turn of the first (its control: sendPending handing a working claim its pending task, under
// which the second prompt goes while the first turn runs).
func TestALookAndAStepOfOneAgentAreSentOneTurnAtATime(t *testing.T) {
	look := RequestDeliver{Claim: testToken, ID: "outbox:look", Task: "Production check of LEGION-209.", Generation: 3,
		CheckLook: CheckLook{Look: 1, OpenedAt: time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)}}
	step := RequestDeliver{Claim: testToken, ID: "outbox:step", Task: "Continue Workflow. Issue: LEGION-209. Phase: testing.", Generation: 3,
		Phase: phase.Testing, Entry: 1}
	for _, tc := range []struct {
		name          string
		first, second RequestDeliver
	}{
		{"a look's task given while a step's turn runs", step, look},
		{"a step's task given while a look's turn runs", look, step},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			h.must(tc.first)
			if sent := h.wantPrompts(2)[1]; sent.DeliveryID != tc.first.ID {
				t.Fatalf("sent %+v to the idle claim, want the first task, %s", sent, tc.first.ID)
			}
			h.must(StreamTurnStart{Claim: testToken, DeliveryID: tc.first.ID})
			h.wantState(StateWorking)

			h.must(tc.second)
			h.wantPrompts(2)
			if p := h.pending(); p.ID != tc.second.ID || !p.DeliveredAt.IsZero() {
				t.Fatalf("the claim holds %+v while the first task's turn runs, want the second task, %s, waiting unsent", p, tc.second.ID)
			}

			h.must(StreamTurnEnd{Claim: testToken})
			if sent := h.wantPrompts(3)[2]; sent.DeliveryID != tc.second.ID {
				t.Fatalf("sent %+v once the first task's turn ended, want the second task, %s", sent, tc.second.ID)
			}
		})
	}
}
