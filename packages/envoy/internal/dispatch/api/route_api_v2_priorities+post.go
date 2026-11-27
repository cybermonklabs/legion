package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/priorities", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("Write one priority {id?, title, description?, owner?}, a bearer naming its session "+
			"in actor: title is required, and a title with nothing in it or over %[1]d characters is 400 "+
			"PRIORITY_INPUT. A body that names no id takes the title written as a slug (lowercase ASCII letters and "+
			"digits, each other run of characters one hyphen), and a slug over %[2]d characters is cut to its longest "+
			"start of at most %[2]d characters that ends at the end of a word, or to its first %[2]d characters when its "+
			"first word is longer, so a long title is written whole under a cut id. A title with no ASCII letter or "+
			"digit, or one the cut leaves as none or any, takes the id priority- and the first 12 lower-case hex "+
			"characters of the SHA-256 of the title's UTF-8 bytes instead, so every title gives an id. A given id that "+
			"is no slug of lowercase letters, digits and single hyphens, one over %[2]d characters, and none and any "+
			"(the issue listing's keywords) are each 400 PRIORITY_INPUT; an id the store already holds is 409 "+
			"PRIORITY_EXISTS. The new priority takes the last place of the order. A credential scoped to one project "+
			"is refused, since a priority belongs to none. Answers 201 the priority.", contracts.IssueTitleMax, priorityIDMax),
		Handler:  s.createPriorityV2,
		Resource: "priorities", Kind: kindCollection, Key: "{id}",
		Body:     requestBody{Kind: bodyJSON, Type: createPriorityInput{}},
		Shape:    routeShape{Status: http.StatusCreated, Response: model.Priority{}},
		Scope:    scopeDeployment,
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, priorityInputRefusal, priorityIDHeldRefusal},
	}
})
