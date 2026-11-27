package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/going/capability",
	Auth:        authAny,
	Description: "What the command bar can do on this deployment, each part apart: {answer: {configured, model, blocks}, decision: {configured, model, reachable}, session: {configured}, label: {configured}}. answer is the model that answers, named as configured, its blocks true once it accepted a structured reply, false once its service refused the structured output itself (a refusal for another cause, such as a content filter, is that run's alone), \"unknown\" before either; decision is the measured decision model, a name the committed record does not hold reading unset, and reachable whether its token file reads now; session and label are the two new-issue settings. It calls no model, and names no endpoint and no credential. Any caller Dispatch admits reads it, a pod's own agent token included.",
	Handler:     (*server).getGoingCapability,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: goingCapability{}},
})
