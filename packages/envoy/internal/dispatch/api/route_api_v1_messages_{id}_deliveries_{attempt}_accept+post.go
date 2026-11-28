package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/messages/{id}/deliveries/{attempt}/accept",
	Auth:        authBearer,
	Description: "The attempt's session records that it took the message as its user's own turn; the session names itself in actor (403 ACCEPT_FORBIDDEN for another's attempt). An attempt the message does not have is 404 MESSAGE_NOT_FOUND, and so is one that is not a positive number of at most 2147483647 (the largest the store holds). One compare-and-set: only a direct message to that session, on no issue, neither a broadcast's copy nor a reply in a broadcast's thread, in a thread whose root targets it (409 ACCEPT_NOT_DIRECT), that a person wrote (409 ACCEPT_NOT_WRITTEN_BY_PERSON), sent as a Send or an Aside (409 ACCEPT_NOT_ASIDE_OR_STEER); only the message's latest attempt (409 ACCEPT_SUPERSEDED), when no attempt of it was accepted before (409 ACCEPT_ALREADY_ACCEPTED), which a person requested (409 ACCEPT_NOT_REQUESTED_BY_PERSON), that person being the message's author (409 ACCEPT_NOT_REQUESTED_BY_AUTHOR), which did not fail (409 ACCEPT_FAILED; a pending, sent or queued attempt is taken), within the last minute (409 ACCEPT_STALE). Answers the attempt with accepted_at and accepted_as and the message's stored body; leaves the attempt's state to the send, and appends message.accepted.",
	Handler:     (*server).acceptDelivery,
	Successor:   successorIs(http.MethodPut, "/messages/{id}/deliveries/{attempt}/state"),
	// Answering a delivery belongs to the session the attempt was sent to, not to any project.
	WriteClass: scopeSession,
	ID:         &idRoute{Kind: "message", NotFound: apicode.MessageNotFound},
	Shape: routeShape{
		Status: http.StatusOK, Response: acceptedDelivery{}, Request: actorInput{},
	},
})
