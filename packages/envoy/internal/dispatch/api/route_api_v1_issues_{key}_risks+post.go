package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/risks",
	Auth:        authAny,
	Description: "Write one risk under a pre-mortem lens of the issue {lens, heading, early_warning, prevention, prevented_by?, visibility?}: the failure it names, the sign it is seen coming by and what keeps it from happening, as attributes of the risk, placed under the lens by its part_of tuple. prevented_by is the address of a check block that covers the risk, of any issue of the issue's project, a prevented_by tuple, read as the pointer route reads one. A field left blank is 400 RISK_INPUT; a lens the caller may not see under the issue, or a check the caller may not see in any issue, is 404 NOT_FOUND, as one that does not exist; a gone lens is 409 NODE_GONE, naming it; a lens of any kind but pre-mortem, a node that is no lens, a prevented_by at a block that is no check, a prevented_by at a check of another project and a prevented_by at a check of a retired document, whose checks nobody runs, each naming the check, are 400 INVALID_POINTER, the first saying a risk belongs under a lens of kind pre-mortem; visibility author is 403 AUTHOR_NAMES_NOBODY on a credential that proves no author. With no visibility the risk takes its lens's; visibility project under a lens reserved for its author is 400 VISIBILITY_WIDER_THAN_PARENT, naming the risk and the lens, and stores nothing. A refused write stores nothing, and the write appends no event. Answers 201 with the risk: its address dispatch://risk/<uuid>, lens, heading, early_warning, prevention, prevented_by (each check's address), author, created_at, state and visibility.",
	Handler:     (*server).writeRisk,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: riskWritten{}, Request: riskInput{},
	},
})
