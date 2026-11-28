package agent

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
)

// TestARunThatEndedLeavesNoContextInTheAgent: a run's worker works under a context of its own,
// made from the agent's (newWorker), and once its turn has ended - it answered, or its plan waits
// for its person - that context is ended too. A context that is never ended stays among the
// children of the agent's, with whatever was made from it, until the agent's process exits: check
// c21 of INFRA-1588 measured the agent's live heap growing by about 490 bytes for each run and for
// each plan that waits (1,000 plans: +457,632 bytes; 4,000: +1,965,120), and the agent's heap
// profile over that soak put the growth in context.(*cancelCtx).propagateCancel and
// context.withCancel under agent.newWorker and agent.(*worker).step. c21 says the agent holds no
// heap for the plans that wait.
func TestARunThatEndedLeavesNoContextInTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply agenttest.Reply
		state string
	}{
		{name: "a run that answered", reply: agenttest.Reply{Text: "No issue says so."}, state: agentwire.StateDone},
		{name: "a run whose plan waits", reply: agenttest.Reply{Calls: []agenttest.Call{{Name: toolProposePlan, Arguments: map[string]any{
			"summary": "Retitle it.",
			"changes": []any{map[string]any{"tool": "dispatch_issue_update", "arguments": map[string]any{"issue": "DEMO-1", "title": "New"}}},
		}}}}, state: agentwire.StateWaiting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := agenttest.New(t, func(*agenttest.Request) agenttest.Reply { return tc.reply })
			server, client := newFakeServer(t, func(agentwire.CallRequest) agentwire.CallAnswer {
				return agentwire.CallAnswer{Status: 200, Body: []byte(`{}`), ContentType: "application/json"}
			})
			agentCtx, stop := context.WithCancel(t.Context())
			defer stop()
			a := &agent{client: client, config: Config{Model: fake.Model()}, held: map[string]*worker{}, claimed: make(chan struct{}, 1), exit: make(chan struct{})}
			run := &agentwire.ClaimedRun{ID: "run-1", Words: "retitle DEMO-1 to New", Level: "fast", Mode: "ask", LeaseToken: "lease-1"}
			w := newWorker(agentCtx, a, run, func() {})
			w.work()
			end, _ := server.end(t)
			t.Logf("the run ended %s %q", end.State, end.Reason)
			if w.ctx.Err() == nil {
				t.Errorf("the run ended %s, and its context is still live under the agent's: the agent keeps it, and all made from it, until it exits", end.State)
			}
		})
	}
}
