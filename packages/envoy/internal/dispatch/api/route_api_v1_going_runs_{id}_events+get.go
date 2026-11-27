package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/going/runs/{id}/events",
	Auth:        authAny,
	Description: fmt.Sprintf("The caller's own command-bar run's frames as server-sent events: each frame is `id:` its sequence, `event:` its name (the agent's frames, %s for each plan made or answered, and %s, the run's end {state, reason, offers, issue?}, where issue is the key of the issue a new-issue run filed, whose offers then hold %s) and `data:` its JSON, written as the run stores it. Last-Event-ID resumes after that sequence. The stream ends once the run has ended and every frame it stored is written; a stream with no frame to write for %s writes %s and ends, and the reader opens it again from its last sequence. A withdrawn run (GET /api/v1/going/runs/{id}) streams none of its frames: each read of the frames reads whether the run is withdrawn beside them, and once it is, the stream writes one %s {state: stopped, reason starting %s, offers: []} under the id of the run's last end, stopping a run that has not ended first, and ends; a stream resumed at or past that id is written nothing. So a run still working when the post is deleted writes no frame past the delete. A run its own deletes alone withdrew is written its %s frames before that end, which carry a plan's number and state and no word, and none of its other frames, and its end's reason starts %s. One process serves at most %d streams of one run and %d of one person's runs, and refuses the next 429 %s. 404 %s as GET /api/v1/going/runs/{id} answers it.", goingFramePlan, agentwire.FrameEnd, agentwire.OfferUndo, goingRunStreamIdle, goingFrameKeepalive, agentwire.FrameEnd, contracts.GoingErrorAnswerWithdrawn, goingFramePlan, contracts.GoingErrorAnswerWithdrawnByRun, goingReadersPerRun, goingReadersPerPerson, codeGoingRunReadersFull, codeGoingRunNotFound),
	Handler:     (*server).streamGoingRunEvents,
	ID:          &idRoute{Reason: "a going run's id, whose events the route streams to its own author alone and answers for itself"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesState, WriteNote: "When the run is withdrawn and has not ended, whether found so as the stream opens (readCallersGoingRun) or in any later pass over its frames (goingRunWithdrawal), the read stops it on the request's context (stopGoingRun): it updates the run's going_runs row to stopped with its lease freed (stopGoingRunIn), inserts the end frame into going_run_events with a notify on GoingRunEventsChannel (appendGoingRunFrames), appends going_plan.updated to events for a run that was waiting (appendGoingPlanEvent), and notifies GoingRunsChannel."},
	Shape: routeShape{
		Status:             http.StatusOK,
		Opaque:             "the run's frames are a text/event-stream of hand-written id:/event:/data: frames, not one JSON body",
		OpaqueContentTypes: []string{"text/event-stream"},
	},
})
