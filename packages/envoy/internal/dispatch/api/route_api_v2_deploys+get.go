package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/deploys", Auth: authAny, Versions: onlyV2,
		Description: "Each program's current deploy in the repository repo= names (owner/repo; any other value, " +
			"none among them, is 400 DEPLOY_INPUT), ordered by program, each with its source, its writer and its " +
			"workflow run attempt, known or unknown. limit 1 to 200 (default 50) and offset; {items, total, limit, " +
			"offset}.",
		Handler:  s.listDeploysV2,
		Resource: "deploys", Kind: kindCollection,
		Query:    deploysV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[model.Deploy]{}},
		Refusals: listRefusals(apicode.Refusal{Condition: apicode.DeployInputInvalid, Code: apicode.DeployInput}),
	}
})
