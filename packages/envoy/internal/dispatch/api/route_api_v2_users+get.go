package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/users", Auth: authAny, Versions: onlyV2,
		Description: "Everyone who has signed in, by lowercase email, each {login}: the assignee picker's options. " +
			"Needs the person scope. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}.",
		Handler:  s.listUsersV2,
		Resource: "users", Kind: kindCollection,
		Query:       usersV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[dispatchUser]{}},
		OAuthScopes: v2PersonScope,
		Refusals:    listRefusals(),
	}
})
