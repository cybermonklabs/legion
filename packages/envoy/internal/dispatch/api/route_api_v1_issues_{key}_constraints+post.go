package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/constraints",
	Auth:        authAny,
	Description: "Write one constraint under the issue, {standard, reference} or {measurement, value, taken_where}, with visibility?: what forced a requirement from outside the conversation, a standard with its reference or a measurement with its value and where it was taken, as attributes of the constraint, placed under the issue by its part_of tuple. A body that is neither shape, leaves a field of its shape blank or names fields of both is 400 CONSTRAINT_INPUT; visibility author is 403 AUTHOR_NAMES_NOBODY on a credential that proves no author; an issue Dispatch does not hold is 404 NOT_FOUND. A refused write stores nothing, and the write appends no event. A requirement points at the constraint through POST /api/v1/issues/{key}/nodes/{id}/pointers (constrained_by). Answers 201 with the constraint as GET /api/v1/issues/{key}/constraints lists it.",
	Handler:     (*server).writeConstraint,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: constraintRead{}, Request: constraintInput{},
	},
})
