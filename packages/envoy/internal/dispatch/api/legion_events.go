// legion_events.go serves POST /api/v1/legion/events: a Legion daemon pushes its change events
// here (dispatch://INFRA-979, "The daemon sends its changes to Dispatch"), and the board reads
// what they leave in legion_observations.
//
// Auth is the token the rest of the API takes. The daemon and its workers share one Dispatch
// token today, so the route trusts any caller that may write the project, as the status writes
// already do; that shared token is a known limit of this prototype.
package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// legionEventsRoute's body may carry a full snapshot of a daemon's state, past the 1 MiB every
// other JSON route takes (routeBody).
const (
	legionEventsRoute              = http.MethodPost + " /api/v1/legion/events"
	legionEventsRequestLimit int64 = 32 << 20
)

var errLegionEventsTooLarge = errorf(http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body exceeds %d bytes", legionEventsRequestLimit)

// legionEventsInput is POST /api/v1/legion/events's body: one daemon's push of change events for
// one project, with the actor the caller acts as.
type legionEventsInput struct {
	legion.Push
	Actor *model.Actor `json:"actor"`
}

// legionEventsResponse is what that route answers: the last sequence number the project holds
// with no gap below it.
type legionEventsResponse struct {
	LastSeq int64 `json:"last_seq"`
}

// postLegionEvents stores one delivery of events and answers {last_seq}, the last sequence number
// the project holds with no gap below it. 404 LEGION_NOT_CONFIGURED for a project with no Legion
// setting; 400 LEGION_EVENTS_INPUT for a body it cannot store.
func (s *server) postLegionEvents(w http.ResponseWriter, r *http.Request) {
	var input legionEventsInput
	if err := decodeJSONWithin(r, &input, legionEventsRequestLimit, errLegionEventsTooLarge); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, ok := s.requireActor(w, r, input.Actor); !ok {
		return
	}
	push := input.Push
	push.Project = strings.TrimSpace(push.Project)
	if covers, scoped := s.callerProject(r); scoped && covers != push.Project {
		refuseWriteOutsideProjectScope(w, covers, push.Project)
		return
	}
	if err := push.Validate(); err != nil {
		writeError(w, "LEGION_EVENTS_INPUT", http.StatusBadRequest, err.Error())
		return
	}
	project, ok := s.legionProjectNamed(w, r, push.Project, "LEGION_EVENTS_INPUT")
	if !ok {
		return
	}
	if _, err := legion.GetSettings(r.Context(), s.deps.Store.Pool, project); err != nil {
		if errors.Is(err, legion.ErrNoSettings) {
			writeError(w, legionNotConfiguredCode, http.StatusNotFound, legionNotConfiguredMessage)
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	lastSeq, err := legion.ApplyPush(r.Context(), s.deps.Store.Pool, push)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, legionEventsResponse{LastSeq: lastSeq})
}
