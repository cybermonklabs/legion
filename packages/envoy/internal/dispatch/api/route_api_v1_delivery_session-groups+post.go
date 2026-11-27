package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/delivery/session-groups",
	Auth:        authAny,
	Description: "Create a Delivery session group {name, sessions}: the group becomes the Parent agent of every pull request whose first session it holds; 409 SESSION_IN_GROUP for a session another group holds, 409 SESSION_GROUP_NAME_TAKEN for a name another group has, 409 SESSION_GROUP_NAME_IS_SESSION_TITLE for a name that is the title of a session outside the group. The timeline lists every group (session_groups).",
	Handler:     (*server).createDeliverySessionGroup,
	Successor:   successorIs(http.MethodPost, "/session-groups"),
	WriteClass:  scopeDeployment,
	Shape: routeShape{
		Status: http.StatusCreated, Response: delivery.SessionGroup{}, Request: createDeliverySessionGroupInput{},
	},
})
