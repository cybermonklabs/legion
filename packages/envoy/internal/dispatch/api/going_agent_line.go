package api

// going_agent_line.go answers the agent's line call (agentwire.KindLine): which line on top the
// decision model Jev reads a run's words as. A run started from the line on top records the line
// on the screen at the key (going_runs.line), and an Ask there may be Jev's answer or the line
// the bar reads before Jev answers within its wait, so the agent asks again for a run of Ask or of
// none, as the turn of its words starts. A turn Jev reads as Ask asks for no change, so its reply
// in prose needs no check for a plan it left out (going/agent/noplan.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/going/agentwire"
)

// lineCall puts the run's words to Jev, with the page the run was started on and the actions the
// run's person may be offered, as the bar's own decision call puts them (goingDecide), and answers
// the line on top Jev's choice gives (agentwire.LineAnswer): "" where the server names no decision
// model, and where the decision call failed, timed out or named no action it was offered, which
// goingDecide logs once. Jev's call is bounded by the decider's own time (goingDecisionTime), and
// no answer of it fails the call. The actions are the catalogue as the run's grant may run it: a
// sign-in's, a browser's, takes no bearer-only row, and a personal token's holds its scopes.
func (a *AgentListener) lineCall(ctx context.Context, request agentwire.CallRequest, caller resolvedCaller) (agentwire.CallAnswer, *agentwire.Error) {
	var words string
	var chip []byte
	if err := a.s.deps.Store.Pool.QueryRow(ctx, `select words, context from going_runs where id = $1::uuid`, request.Run).Scan(&words, &chip); err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, fmt.Errorf("read the run's words: %w", err))
	}
	credential := goingCredential{cookie: caller.actor.Credential == nil || caller.actor.Credential.TokenID == "", credential: caller.actor.Credential}
	start := goingRunStart{actions: a.s.going.runnableBy(credential)}
	if chip != nil {
		start.chip = &goingContext{}
		if err := json.Unmarshal(chip, start.chip); err != nil {
			return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, fmt.Errorf("read the run's page: %w", err))
		}
	}
	var answer agentwire.LineAnswer
	if decision, decided := a.s.goingDecide(ctx, words, start); decided {
		answer.Line = decision.Line
	}
	// A struct of one string always marshals.
	body, _ := json.Marshal(answer)
	return agentwire.CallAnswer{Status: http.StatusOK, Body: body, ContentType: "application/json"}, nil
}
