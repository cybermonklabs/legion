package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/messages/{id}/deliveries", Auth: authAny, Versions: onlyV2,
		Description: "Retry delivering a targeted message {delivery}: btw, aside or steer (400 MESSAGE_INPUT otherwise); a " +
			"bearer names its session in actor. It resumes the attempt a stopped send left owed, carrying that attempt's " +
			"urgency: in the same mode, or in any mode where no session had been resolved for it. An owed attempt already " +
			"resolved to a session in another mode is failed, whether or not its send went out, and the retry opens an " +
			"attempt of its own, carrying none, as it " +
			"does when none is owed. Answers 201 the attempt as " +
			"its send settled. A message that targets nobody is 404 MESSAGE_NOT_FOUND, as is an id no message has.",
		Handler:  s.createMessageDeliveryV2,
		Resource: "deliveries", Kind: kindCollection, Key: "{attempt}",
		Body:  requestBody{Kind: bodyJSON, Type: createDeliveryInput{}},
		Shape: routeShape{Status: http.StatusCreated, Response: model.MessageDelivery{}},
		Scope: scopeProject, ProjectOf: projectPathMessage,
		IDs: v2MessageIDs(),
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, messageMissingRefusal, messageInputRefusal, postDeletedRefusal, messageTargetMovedRefusal,
		},
	}
})
