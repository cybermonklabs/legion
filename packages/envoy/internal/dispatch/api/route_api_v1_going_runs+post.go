package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/contracts"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/going/runs",
	Auth:        authAny,
	Description: fmt.Sprintf("Start a command-bar run {words, context?, kind?, level?, mode?, continues?, line?}: the agent works it in the background, as the caller, until it ends, stops at a bound of its level, or waits for its person to answer a plan. words is required (400 %s when blank) and at most %d characters (400 CAP_EXCEEDED); context is the bar's chip {path, project, issue, block}; kind %s is the line File as a new issue: a Fast run that files the words as one new issue, its title, project, parent, priority and spec written by the agent from the words and the chip, with the label and the route the deployment names for a new issue, whose create this request approves, so it waits for nobody, and whose end names the issue and offers Undo, or, where nothing was filed, ends failed saying why (400 %s for any other kind, and for a level but fast); level is fast, standard (the default) or deep, which set its budgets of model calls, working time and output tokens; mode is ask or auto_review, the caller's own setting (GET /api/v1/me/going/settings) where it is absent; continues names the run it follows up, which must be the caller's own and listed in History (404 %s), the newest run of its conversation (409 %s) and ended (409 %s), and the new run joins its conversation; line is the line on top the person pressed Enter on to start the run, ask, do or file (400 %s for any other), which the run records, absent for a run not started from the line. The run acts with this request's own credential, which must be a person's own sign-in or personal agent token: a broker-issued token, the deployment's shared token and a service-account token are refused 403 %s, with nothing written. One person may have at most %d runs queued (429 %s). Answers 201 with the run, queued, as GET /api/v1/going/runs/{id} answers it.", codeGoingInput, contracts.GoingWordsMax, agentwire.RunNewIssue, codeGoingInput, codeGoingRunNotFound, codeGoingRunNotLatest, codeGoingRunNotEnded, codeGoingInput, codeRunNeedsPersonCredential, goingQueuedPerPerson, codeGoingRunQueueFull),
	Handler:     (*server).createGoingRun,
	WriteClass:  scopeSession,
	Shape: routeShape{
		Status: http.StatusCreated, Response: goingRun{}, Request: goingRunInput{},
	},
})
