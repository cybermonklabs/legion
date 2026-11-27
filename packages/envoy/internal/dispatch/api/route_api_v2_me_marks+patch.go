package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPatch, Pattern: "/me/marks", Auth: authAny, Versions: onlyV2,
		Description: "Write several of the caller's marks at once, answering {updated}, how many it changed (0 for a " +
			"request that changes nothing, which appends no event). The body is either {items}, a named set, each " +
			"item {kind, id, ...fields} of one mark written as PATCH /api/v2/me/marks/{kind}/{id} writes it, every " +
			"target held to exist before anything is written, a kind that is none of the six 400 INVALID_KIND, and " +
			"every item written in one transaction, so a refusal of any item writes none of them; or {filter?, set}, " +
			"the Inbox's own " +
			"narrowing of GET /api/v2/me/threads - filter {kind (reply, agent; both by default), priority (P0, P1, P2, " +
			"P3, none), urgency (low, med, high, blocking, none), project (a key, none), issue or artifact}, each " +
			"absent list meaning every value - with set {read_through}, marking read every unread thread it narrows " +
			"to whose newest reply is at or before read_through, an agent reply marked read by id; {dismissed_through}, " +
			"taking out of the Inbox every thread it narrows to whose newest reply is at or before it; or {dismissed: " +
			"false, dismissed_through?}, putting back every dismissed thread it narrows to, dismissed at or before " +
			"dismissed_through where it is given. Each time is RFC3339 and no more than a minute ahead of the server. " +
			"Both forms at once, neither, a malformed filter, and items naming one mark twice are 400 INVALID_QUERY. " +
			markWriteText + " Needs the person scope, which a project-scoped session's token does not hold. A " +
			"credential that proved a session, a token the secrets broker issued, writes through items the agent " +
			"marks of that session alone: an agent item of another session is 403 SESSION_SCOPE, and no item is " +
			"written. The filter form names no session, so with kind agent it marks the agent replies of every " +
			"session, as POST /api/v1/me/inbox/read does.",
		Handler:  s.patchMarksV2,
		Resource: "marks", Kind: kindCollection, Key: "{kind}/{id}",
		Body: requestBody{Kind: bodyJSON, Type: marksPatchV2Input{}},
		Shape: routeShape{
			Status: http.StatusOK, Response: markUpdatesV2{},
			Refusals: []routeAnswer{{Status: http.StatusConflict, Body: markStaleV2{}}},
		},
		OAuthScopes: v2PersonScope,
		Scope:       scopeSession,
		Refusals:    markWriteRefusals(),
	}
})
