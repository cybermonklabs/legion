package api

import (
	"fmt"
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/going/requests/{id}/cancel",
	Auth:        authAny,
	Description: fmt.Sprintf("Cancel one run its author typed past or closed: answers 200 {id, cancelled_at}, cancelled_at the first cancel's moment for a run cancelled before. From then on the run stores no frame that does not end its stream, so its answer ends at its next delta on whichever pod is writing it, and at once on the pod this call lands on; the frames it stored before stay for its stream to carry. A run the record does not hold, an id that is not a uuid, and a run that is another caller's are one answer, 404 %s, as the stream answers them.", codeGoingRequestNotFound),
	Handler:     (*server).cancelGoingRequest,
	// Cancelling an answer run belongs to the credential that asked it, not to any project.
	WriteClass: scopeSession,
	ID:         &idRoute{Reason: "a going request's run id, which the route cancels for its own author alone and answers for itself"},
	Shape:      routeShape{Status: http.StatusOK, Response: goingCancellation{}},
})
