package api

// going_agent_words.go answers the agent's words call (agentwire.KindWords): how many words the
// bar draws of a reply in prose that would end a run's turn. An answer of the command bar's agent
// has fewer than 120 words unless the person's words ask for more, and the prompt's words did not
// hold that bound: at 8a3e562b, 6 of 11 answers to "What is the state of this issue's open
// children?" drew 120 words or more, each chip counted as the bar draws it, its key, its status
// and its title. So the agent asks the server for the count before such a reply ends the turn,
// and gives the model one more turn for a reply of 120 or more (going/agent's endProse). The
// server counts since the words the bar draws are those of the text it stores (goingStoredAnswer)
// and of the items that text links, as the run's person reads them, which the agent does not hold.

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/going/chips"
)

// wordsCall answers how many words the bar draws of the reply the call carries (CallRequest.Text)
// as the run's answer (agentwire.WordsAnswer): the text as the step that saves the reply would
// store it in its frame (goingStoredAnswer), counted as chips.DrawnWords counts it, each item it
// links read as the run's person may see it. It records nothing.
func (a *AgentListener) wordsCall(ctx context.Context, request agentwire.CallRequest) (agentwire.CallAnswer, *agentwire.Error) {
	q := a.s.deps.Store.Pool
	items := &goingAnswerItems{ctx: ctx, q: q, run: request.Run}
	text, err := goingStoredAnswer(request.Text, &goingRunResults{ctx: ctx, q: q, run: request.Run}, items)
	if err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	words := chips.DrawnWords(text, items)
	if items.err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, items.err)
	}
	// A struct of one number always marshals.
	body, _ := json.Marshal(agentwire.WordsAnswer{Words: words})
	return agentwire.CallAnswer{Status: http.StatusOK, Body: body, ContentType: "application/json"}, nil
}
