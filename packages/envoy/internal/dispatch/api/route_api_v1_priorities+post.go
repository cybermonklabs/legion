package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/priorities",
	Auth:        authAny,
	Description: fmt.Sprintf("Write one priority {id?, title, description?, owner?}: title is required, and a title with nothing in it or over %[1]d characters (UTF-16 units, counted as an issue title is) is 400 PRIORITY_INPUT. id is the caller's; a body that names none takes the title written as a slug (lowercase ASCII letters and digits, each other run of characters one hyphen), and a slug over %[2]d characters is cut to its longest start of at most %[2]d characters that ends at the end of a word, or to its first %[2]d characters when its first word is longer, so a long title is written whole under a cut id. A title with no ASCII letter or digit, or one the cut leaves as none or any, takes the id priority- and the first 12 lower-case hex characters of the SHA-256 of the title's UTF-8 bytes instead, so every title gives an id. A given id that is no slug of lowercase letters, digits and single hyphens, a given id over %[2]d characters, and a given id of none or any (the issue listing reads advances=none as the work under no priority and advances=any as the work under any) are each 400 PRIORITY_INPUT; an id the store already holds is 409 PRIORITY_EXISTS. The new priority takes the last place of the order. A credential scoped to one project is refused, since a priority belongs to none.", contracts.IssueTitleMax, priorityIDMax),
	Handler:     (*server).createPriority,
	WriteClass:  scopeDeployment,
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.Priority{}, Request: createPriorityInput{},
	},
})
