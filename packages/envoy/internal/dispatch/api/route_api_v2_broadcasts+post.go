package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/broadcasts", Auth: authAny, Versions: onlyV2,
		Description: "Send one message to many sessions {body, delivery, session_ids}, which needs the person scope: one " +
			"targeted message per recipient under a shared broadcast, answered 201 once it is committed while each " +
			"recipient's delivery runs behind the answer. A selected session the Envoy listener knows and that is not " +
			"running is a recipient like any other, its copy stored for it; one the listener knows nothing about, or that " +
			"does not advertise the mode, is excluded and named in excluded, never switched to another mode, and a send " +
			"none of whose sessions can take it is 400 BROADCAST_EMPTY. A repeat under its Idempotency-Key answers 201 the " +
			"broadcast the key made, every recipient's attempts as they stand and the first answer's exclusions. 400 " +
			"BROADCAST_INPUT or INVALID_MESSAGE for a send it refuses, 503 ENVOY_UNAVAILABLE without a listener.",
		Handler:  s.createBroadcastV2,
		Resource: "broadcasts", Kind: kindCollection, Key: "{id}",
		Body:        requestBody{Kind: bodyJSON, Type: createBroadcastV2Input{}},
		Shape:       routeShape{Status: http.StatusCreated, Response: broadcastCreated{}},
		OAuthScopes: v2PersonScope,
		Scope:       scopeDeployment,
		Replay:      replayRecovered(s, s.replayBroadcast),
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, messageInvalidRefusal, valuePastBoundRefusal, bodyDecisionBlockRefusal,
			envoyUnreachableRefusal,
			{Condition: apicode.BroadcastInputInvalid, Code: apicode.BroadcastInput},
			{Condition: apicode.BroadcastReachesNobody, Code: apicode.BroadcastEmpty},
		},
	}
})
