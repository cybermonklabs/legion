package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/messages/{id}/deliveries/{attempt}/state", Auth: authBearer, Versions: onlyV2,
		Description: "The attempt's session records that it took the message as its user's own turn {state: accepted, " +
			"actor}, under a bearer credential, the session naming itself in actor: a cookie caller and another " +
			"session's attempt are 403 ACCEPT_FORBIDDEN. One compare-and-set: only a direct message to " +
			"that session, on no issue, neither a broadcast's copy nor a reply in a broadcast's thread, in a thread whose " +
			"root targets it (409 ACCEPT_NOT_DIRECT), that a person wrote (409 ACCEPT_NOT_WRITTEN_BY_PERSON) themselves, " +
			"not an agent for them (409 ACCEPT_WRITTEN_BY_AGENT), sent as a Send or an Aside (409 " +
			"ACCEPT_NOT_ASIDE_OR_STEER); only the message's latest attempt (409 ACCEPT_SUPERSEDED), when no attempt of it " +
			"was accepted before (409 ACCEPT_ALREADY_ACCEPTED), which a person requested (409 " +
			"ACCEPT_NOT_REQUESTED_BY_PERSON), that person being the message's author (409 " +
			"ACCEPT_NOT_REQUESTED_BY_AUTHOR), which did not fail (409 ACCEPT_FAILED; a pending, sent or queued attempt is " +
			"taken), within the last minute (409 ACCEPT_STALE). Answers the attempt with accepted_at and accepted_as and " +
			"the message's stored body; leaves the attempt's state to the send, and appends message.accepted. The same " +
			"session accepting an attempt it already accepted answers 200 the same and writes nothing. 400 STATE_INVALID " +
			"for any other state, 404 MESSAGE_NOT_FOUND, 404 DELIVERY_NOT_FOUND for an attempt the message does not have, " +
			"and so for one that is not a whole number from 1 to 2147483647 (the largest attempt the store holds).",
		Handler:  s.putDeliveryStateV2,
		Resource: "state", Kind: kindState,
		Body: requestBody{Kind: bodyStateUnion, Arms: map[string]any{
			"accepted": acceptDeliveryStateInput{},
		}},
		Shape: routeShape{Status: http.StatusOK, Response: acceptedDelivery{}},
		Scope: scopeSession,
		IDs:   v2MessageIDs(),
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, messageMissingRefusal, deliveryMissingRefusal,
			{Condition: apicode.AcceptNotTheRecipient, Code: apicode.AcceptForbidden},
			{Condition: apicode.AcceptOfIndirectMessage, Code: apicode.AcceptNotDirect},
			{Condition: apicode.AcceptOfMessageNoPersonWrote, Code: apicode.AcceptNotWrittenByPerson},
			{Condition: apicode.AcceptOfAgentWrittenMessage, Code: apicode.AcceptWrittenByAgent},
			{Condition: apicode.AcceptOfOtherMode, Code: apicode.AcceptNotAsideOrSteer},
			{Condition: apicode.AcceptOfOlderAttempt, Code: apicode.AcceptSuperseded},
			{Condition: apicode.AcceptAfterAccept, Code: apicode.AcceptAlreadyAccepted},
			{Condition: apicode.AcceptOfAttemptNoPersonAsked, Code: apicode.AcceptNotRequestedByPerson},
			{Condition: apicode.AcceptOfAttemptAnotherAsked, Code: apicode.AcceptNotRequestedByAuthor},
			{Condition: apicode.AcceptOfFailedAttempt, Code: apicode.AcceptFailed},
			{Condition: apicode.AcceptOfStaleAttempt, Code: apicode.AcceptStale},
		},
	}
})
