package api

import (
	"reflect"
	"testing"
)

// The test rows of the agent-actions entry (contract_20261125182642_agent-actions.go): the five
// routes it adds that decode a body a plugin sends, and the fixtures of the two answers it adds.
// It adds no key to the state, so it has no state rows and no band to carry.
func init() {
	registerEntryTestRows(entryTestRows{
		entry: agentActionsContract,
		requests: map[string]reflect.Type{
			"POST /legion/v1/agents/stop":       reflect.TypeFor[ClaimRequest](),
			"POST /legion/v1/agents/retry":      reflect.TypeFor[ClaimRequest](),
			"POST /legion/v1/agents/suspend":    reflect.TypeFor[ClaimRequest](),
			"POST /legion/v1/agents/resume":     reflect.TypeFor[ClaimRequest](),
			"POST /legion/v1/issues/pod-update": reflect.TypeFor[PodUpdateRequest](),
		},
		answers: func(*testing.T) map[AnswerID]answerFixture {
			return map[AnswerID]answerFixture{
				// The agent actions' answer, one body per state a claim can stand in after one, each
				// naming a suspension landed and one held, so every value the table declares at the
				// answer's three enumerated fields is carried by some body (agentActionBodies). The
				// goldens (`agent-*.json`) pin real answers, which name a suspender only where one
				// holds; a fixture body is maximal.
				AnswerAgentAction: {roots: typesOf(AgentActionResponse{}), bodies: agentActionBodies()},
				// The pod update's golden own value (`issue-pod-update.json`): the three keys this
				// answer always writes, none of them omitempty.
				AnswerPodUpdate: {roots: typesOf(PodUpdateResponse{}), bodies: []any{PodUpdateResponse{
					Round:   "pod:9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22",
					Reasons: "the project's controller asked for the replacement of this pod",
					Opened:  true,
				}}},
			}
		},
	})
}

// agentActionBodies is one AgentActionResponse per state of agentActionStates, the sources of
// suspendSources taken in turn for `suspendedBy` and, one further on, for `suspensionHeldBy`: there
// are more states than sources, so every source is carried at both keys.
func agentActionBodies() []any {
	bodies := make([]any, 0, len(agentActionStates))
	for i, state := range agentActionStates {
		bodies = append(bodies, AgentActionResponse{
			State:            state.Value,
			SuspendedBy:      suspendSources[i%len(suspendSources)].Value,
			SuspensionHeld:   true,
			SuspensionHeldBy: suspendSources[(i+1)%len(suspendSources)].Value,
		})
	}
	return bodies
}
