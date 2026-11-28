package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/known-sessions", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("Which of the sessions the repeated id parameter names (1 to %d session ids) "+
			"Dispatch holds any record of, which needs the person scope, so a page links a dispatch://agent/<session "+
			"id> reference only where that conversation exists, an ended session's included: each {session_id, "+
			"title} in the order asked, each once, and no row for an id with no record. A record is the registry "+
			"copy GET /api/v2/agents lists (whatever its age), an event the session wrote, a message targeted at it, "+
			"a message or comment delivery to it, a file or image of its conversation, or an ask it follows; title "+
			"is the registry copy's, null where that copy lists the session with none or does not list it. One "+
			"statement answers the whole batch. limit 1 to %d (default %d, so one page holds the whole batch) and "+
			"offset; {items, total, limit, offset}, total the sessions found. No id, more than %d, a value that is "+
			"no session id, or one longer than %d bytes, the longest session id a reference in text names, is 400 "+
			"INVALID_QUERY naming the parameter. It admits exactly the callers GET "+
			"/api/v2/agents/{session_id}/messages admits.", contracts.KnownSessionsMax, contracts.KnownSessionsMax,
			contracts.KnownSessionsMax, contracts.KnownSessionsMax, knownSessionIDMax),
		Handler:  s.listKnownSessionsV2,
		Resource: "known-sessions", Kind: kindCollection,
		Query:       knownSessionsV2Query{},
		List:        listBounds{Default: contracts.KnownSessionsMax, Max: contracts.KnownSessionsMax},
		Shape:       routeShape{Status: http.StatusOK, Response: page[knownSession]{}},
		OAuthScopes: v2PersonScope,
		Refusals:    listRefusals(),
	}
})
