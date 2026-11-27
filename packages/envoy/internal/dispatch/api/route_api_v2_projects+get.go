package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/projects", Auth: authAny, Versions: onlyV2,
		Description: "List every project by key: {key, name, open_asks, gated_steps, default_repository?, " +
			"proven_sessions, created_at}, open_asks the open asks on its open issues and its documents, gated_steps " +
			"the design-gate steps the Legion deployment that serves it gates, in the order it gates them ([] for " +
			"none), default_repository " + projectDefaultRepositoryText + ", absent for a project with none, " +
			"proven_sessions " + projectProvenSessionsText + ". include=default_assignee adds " +
			"default_assignee, " + projectDefaultAssigneeText + ", which needs the person scope (403 " +
			"OAUTH_SCOPE_REQUIRED otherwise): the default names a person. limit 1 to 200 (default 50) and offset; " +
			"{items, total, limit, offset}.",
		Handler:  s.listProjectsV2,
		Resource: "projects", Kind: kindCollection, Key: "{key}",
		Query: projectListV2Query{},
		Shape: routeShape{Status: http.StatusOK, Response: page[projectV2]{}},
		Authorities: []authorityRule{{
			Where: authorityWhere{In: partQuery, Name: "include"}, Value: "default_assignee", Scopes: v2PersonScope,
		}},
		Refusals: listRefusals(),
	}
})
