package api

import (
	"cmp"
	"net/http"
	"slices"
	"strings"
)

// routeIndexV2Query is the query GET /api/v2 reads: the page of the index it answers.
type routeIndexV2Query struct {
	pageQuery
}

// routeIndexV2Entry is the public shape of a route in GET /api/v2: its routeIndexEntry, every
// member present whether the route declares it or not, and the declarations of R1 a client
// reads to call it. A route that declares none of them - a test hook, a row of the INFRA-979
// tree - answers them empty.
type routeIndexV2Entry struct {
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Auth        routeAuth `json:"auth"`
	Scopes      []string  `json:"scopes"`
	Description string    `json:"description"`
	// Resource and Kind are what the route serves (routeRow.Resource, routeRow.Kind), and Key the
	// path segments that name one of its items.
	Resource string `json:"resource"`
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	// TimeFields are the fields a read over time windows on (routeRow.TimeFields).
	TimeFields []string `json:"time_fields"`
	// Position is the field a log's after and before read; null for a route that is no log.
	Position *routeIndexPosition `json:"position"`
	// List is a list's limit bounds (listBounds.limits); null for a route whose query takes no
	// limit, which is no list.
	List *routeIndexList `json:"list"`
	// CrossProject is a read across projects, filtered by a repeatable project (R1.3).
	CrossProject bool `json:"cross_project"`
}

// routeIndexPosition is a log's position field in the index (positionField).
type routeIndexPosition struct {
	Field string `json:"field"`
	Type  string `json:"type"`
}

// routeIndexList is a list's limit bounds in the index (listBounds): the limit a request that
// names none is answered with, the largest it may name, and the least, which is 0 for a list
// whose limit 0 answers its total and what stands beside items alone (listBounds.Counts) and 1 for
// every other. The first two are null for a catalogue read whole, which answers every item when
// no limit is named and takes any limit of 1 or more.
type routeIndexList struct {
	DefaultLimit *int `json:"default_limit"`
	MaxLimit     *int `json:"max_limit"`
	MinLimit     int  `json:"min_limit"`
}

// routeIndexV2 is the body GET /api/v2 answers: the list envelope of R1.4, with the agent-facing
// guide beside it.
type routeIndexV2 struct {
	Docs string `json:"docs"`
	page[routeIndexV2Entry]
}

// routeIndexV2Entries renders the routes /api/v2 serves for GET /api/v2, sorted by path then
// method, as GET /api/v1's are.
func routeIndexV2Entries(routes []apiRoute) []routeIndexV2Entry {
	entries := make([]routeIndexV2Entry, len(routes))
	for index, route := range routes {
		entry := routeIndexV2Entry{
			Method:       route.Method,
			Path:         route.Pattern,
			Auth:         route.Auth,
			Scopes:       append([]string{}, route.OAuthScopes...),
			Description:  route.Description,
			Resource:     route.Resource,
			Kind:         string(route.Kind),
			Key:          route.Key,
			TimeFields:   append([]string{}, route.TimeFields...),
			CrossProject: route.CrossProject,
		}
		if route.Position != (positionField{}) {
			entry.Position = &routeIndexPosition{Field: route.Position.Field, Type: string(route.Position.Type)}
		}
		if query, err := compileV2Query(route.Query); err == nil {
			if _, pages := query.field("limit"); pages {
				least, fallback, most := route.List.limits()
				entry.List = &routeIndexList{MinLimit: least}
				if !route.List.Whole {
					entry.List.DefaultLimit, entry.List.MaxLimit = &fallback, &most
				}
			}
		}
		entries[index] = entry
	}
	slices.SortFunc(entries, func(left, right routeIndexV2Entry) int {
		return cmp.Or(strings.Compare(left.Path, right.Path), strings.Compare(left.Method, right.Method))
	})
	return entries
}

// routeIndexV2Body is the page query asks for of the rendered routes, in the body GET /api/v2
// answers: every route, limit the whole list, where the query names no limit (pageOf), which is
// the body WriteRouteIndex writes.
func routeIndexV2Body(entries []routeIndexV2Entry, query pageQuery) routeIndexV2 {
	return routeIndexV2{Docs: routeIndexDocs, page: pageOf(entries, query)}
}

func (s *server) getRouteIndexV2(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, routeIndexV2Body(s.routeIndexV2, v2Query[routeIndexV2Query](r).pageQuery))
}
