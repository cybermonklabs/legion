package api

// going_agent_words.go answers the agent's words call (agentwire.KindWords): how many words the
// bar draws of a reply in prose that would end a run's turn, which the agent holds to its bound
// (going/agent's length.go says why, from what the eval measured). The server counts since the
// words the bar draws are those of the text it stores (goingStoredAnswer) and of the items that
// text links, as the run's person reads them, which the agent does not hold.

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/going/chips"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// wordsCall answers how many words the bar draws of the reply the call carries (CallRequest.Text)
// as the run's answer (agentwire.WordsAnswer): the text as the step that saves the reply would
// store it in its frame (goingStoredAnswer), counted as chips.DrawnWords counts it, each item it
// links read as the run's person, login, may see it. It records nothing.
func (a *AgentListener) wordsCall(ctx context.Context, request agentwire.CallRequest, login string) (agentwire.CallAnswer, *agentwire.Error) {
	q := a.s.deps.Store.Pool
	items := &goingAnswerItems{ctx: ctx, q: q, run: request.Run, reader: refs.VisibilityArg(model.Actor{Kind: "user", ID: login}), asked: true}
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
