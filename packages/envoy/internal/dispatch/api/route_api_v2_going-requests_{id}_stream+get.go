package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/going-requests/{id}/stream", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("One run's answer as server-sent events, which no envelope can carry: each frame is "+
			"`id:` its sequence, `event:` its name (%s, %s, %s, %s, %s, %s, %s) and `data:` the JSON that frame "+
			"carries, flushed as it is written, and the stream ends on %s or %s. Its position is the Last-Event-ID "+
			"header: the stream answers the frames after that sequence, so a reader that drops rereads nothing. Any "+
			"pod serves it, since the frames are rows: a run past its ends_at that stored no ending is ended by the "+
			"reader itself, with the error %s. Once a comment or an issue message whose text the run gave its model "+
			"is deleted, every read of the stream gives none of the run's frames and only the error %s, under the id "+
			"of the run's ending, and nothing to a reader that already holds that id. A run the record does not "+
			"hold, an id that is not a uuid, and a run that is another caller's are one answer, 404 %s, so an id "+
			"tells a caller nothing it did not already know.", contracts.GoingEventText, contracts.GoingEventItem,
			contracts.GoingEventCut, contracts.GoingEventChanged, contracts.GoingEventNone, contracts.GoingEventEnd,
			contracts.GoingEventError, contracts.GoingEventEnd, contracts.GoingEventError,
			contracts.GoingErrorAnswerDidNotArrive, contracts.GoingErrorAnswerWithdrawn,
			apicode.GoingRequestNotFound.Name()),
		Handler:  s.streamGoingAnswer,
		Resource: "stream", Kind: kindStream,
		Shape: routeShape{
			Status:             http.StatusOK,
			Opaque:             "the run's answer is a text/event-stream of hand-written id:/event:/data: frames, not one JSON body",
			OpaqueContentTypes: []string{"text/event-stream"},
		},
		IDs:      &idRoute{Reason: "a going request's run id, which the route reads for its own author alone and answers for itself"},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, goingRequestMissingRefusal},
	}
})
