package api

// v2_misc.go is the routes of /api/v2 no other group holds (R1 of dispatch://INFRA-4071/spec):
// the agents and what people wrote to one of them, the block links, the reference targets (the
// row #4178 added, which no row of the route map names) and the block schema, search
// and its coverage, the going actions and requests, the limits, the users, the dashboard builds,
// the session groups, one deploy run, the markdown round trip and the CI reconcile passes. Every
// route runs the work its /api/v1 route runs (agentRows, sessionanswers.List, blockLinkDocuments,
// referenceTargets, searchAnswer, goingAsker, createGoingRequestFrom, signedInUsers,
// liveDashboardEntries, createDeliverySessionGroupFrom, updateDeliverySessionGroupFrom,
// deleteDeliverySessionGroupAs, markdownRoundTrip), or is that route's handler where it reads no
// query and answers the same, but the CI reconcile passes, which /api/v1 reads as the last 24
// hours whole
// (cireconcile.RecentPasses) and /api/v2 within a window it names (cireconcile.Passes). A list
// answers the envelope; a report, bytes, a stream and one object answer
// none (R1.4's departure). The {id} of a session group answers SESSION_GROUP_NOT_FOUND and of a
// going request GOING_REQUEST_NOT_FOUND (R7); {session_id} keeps the answer /api/v1 gives it
// (R7), and a deploy run keeps its own 404s, since its id is GitHub's and its repository the
// deployment's, which is also why its route is GET /api/v2/deploy-runs/{id} and no project's.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/cireconcile"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
)

// The refusals of the objects a route of this file names in its path (dispatch://INFRA-4071/spec
// R7).
var (
	sessionGroupMissingRefusal = apicode.Refusal{Condition: apicode.SessionGroupMissing, Code: apicode.SessionGroupNotFound}
	goingRequestMissingRefusal = apicode.Refusal{Condition: apicode.GoingRequestMissing, Code: apicode.GoingRequestNotFound}
	deployRunMissingRefusal    = apicode.Refusal{Condition: apicode.DeployRunMissing, Code: apicode.NotFound}
	runIDMalformedRefusal      = apicode.Refusal{Condition: apicode.RunIDMalformed, Code: apicode.RunIDInput}
)

// The refusals of what a search, a going request and a session group write name.
var (
	searchAuthorInvalidRefusal    = apicode.Refusal{Condition: apicode.SearchAuthorInvalid, Code: apicode.InvalidAuthor}
	numberProjectMalformedRefusal = apicode.Refusal{Condition: apicode.NumberProjectMalformed, Code: apicode.InvalidNumberProject}
	searchCoverageTimedOutRefusal = apicode.Refusal{Condition: apicode.SearchCoverageTimedOut, Code: apicode.SearchCoverageTimeout}
	goingAskerNamesNobodyRefusal  = apicode.Refusal{Condition: apicode.CredentialNamesNobody, Code: apicode.GoingAskerNamesNobody}
	goingWordsBlankRefusal        = apicode.Refusal{Condition: apicode.GoingWordsBlank, Code: apicode.GoingInput}
	bodyProjectMalformedRefusal   = apicode.Refusal{Condition: apicode.BodyProjectMalformed, Code: apicode.InvalidProject}
	sessionGroupInvalidRefusal    = apicode.Refusal{Condition: apicode.SessionGroupInvalid, Code: apicode.SessionGroupInput}
	sessionAlreadyGroupedRefusal  = apicode.Refusal{Condition: apicode.SessionAlreadyGrouped, Code: apicode.SessionInGroup}
	sessionGroupNameHeldRefusal   = apicode.Refusal{Condition: apicode.SessionGroupNameHeld, Code: apicode.SessionGroupNameTaken}
	sessionGroupNameTitledRefusal = apicode.Refusal{Condition: apicode.SessionGroupNameTitled, Code: apicode.SessionGroupNameIsSessionTitle}
)

// sessionGroupWriteRefusals are the refusals of a session group's create and of its edit, which
// write a group's name and sessions alike (createDeliverySessionGroupFrom,
// updateDeliverySessionGroupFrom), after more, the route's own.
func sessionGroupWriteRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append(more, sessionGroupInvalidRefusal, sessionAlreadyGroupedRefusal, sessionGroupNameHeldRefusal, sessionGroupNameTitledRefusal)
}

// deliveryUnconfiguredRefusal is the refusal of a route that reads the deployment's delivery
// settings (deliverySettings) on a Dispatch that holds none.
var deliveryUnconfiguredRefusal = apicode.Refusal{Condition: apicode.DeliveryUnconfigured, Code: apicode.DeliveryNotConfigured}

// sessionGroupText is what the three session-group writes say of a group and of who may write
// one.
const sessionGroupText = "A group is a stable id, a name and the agent sessions it holds, and the Delivery page " +
	"shows it as the Parent agent of every pull request whose first session it holds. Any signed-in person or " +
	"agent writes one, a bearer naming its session in actor, and no event is appended; a credential scoped to " +
	"one project is refused, since a group belongs to none."

// v2MiscRows are the other routes: rows 2, 3, 43, 174, 175 and 177, 176, 75, 76, 77,
// 116, 188, 64, 68, 69, 70, 67, 119 and 47 of the route map, and #4178's reference targets.
func (s *server) v2MiscRows() []routeRow {
	person := routeScopeSet{model.ScopePerson}
	sessionGroupIDs := func() *idRoute { return &idRoute{Reason: "a Delivery session group id"} }
	return []routeRow{
		{
			Method: http.MethodGet, Pattern: "/agents", Auth: authAny, Versions: onlyV2,
			Description: "The sessions of Dispatch's copy of the Envoy listener's session list, at most 5 s old: each " +
				"session as the listener describes it (its state, live or unresponsive, roles, capabilities, host and " +
				"last heartbeat), with open_asks, the open asks it asked on open issues and on documents, and " +
				"last_activity, the time of its newest event, null for none. The newest heartbeat first, then by session " +
				"id. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}. 503 ENVOY_UNAVAILABLE where " +
				"Dispatch has no listener configured, 503 ENVOY_REFILLING with a Retry-After while the listener refills " +
				"its list, and 503 for as long as the listener has not answered.",
			Handler:  s.listAgentsV2,
			Resource: "agents", Kind: kindCollection, Key: "{session_id}",
			Query:    agentsV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: page[agentResponse]{}},
			Refusals: listRefusals(envoyListenerRefusals()...),
		},
		{
			Method: http.MethodGet, Pattern: "/agents/{session_id}/answers", Auth: authAny, Versions: onlyV2,
			Description: "What people wrote that Dispatch delivered to one session, newest first by at, then by id: " +
				"each ask's current answer (replaced when it changed an earlier answer), each comment at its current body " +
				"(edited_at when edited; a resolved comment stays), each message at its current body (edited_at when " +
				"edited), a deleted comment or message in its place with deleted_at and deleted_by and an empty text, " +
				"and each ask a person declined to answer, once however it arrived, each naming its relation to the " +
				"session, its author, its words (text, " +
				"and on declined_your_ask alone dispatch_words, the sentences Dispatch wrote for the asking agent), the " +
				"ask and its whole question where it has one, whoever asked it, its owner, dispatch:// ref and href. A " +
				"session owns an ask when it wrote the ask or follows it, and only an owner is told that an ask is its " +
				"own: answered_your_ask, declined_your_ask and replied_on_your_ask (which outranks mentioned_you) for an " +
				"owner, answered_an_ask and declined_an_ask for any other session, which reads a reply in an ask's thread " +
				"by the reason that delivered it. It never lists an agent's or the system's words. from and to window " +
				"at, RFC3339, from inclusive and to exclusive: from defaults to 72 " +
				"hours ago, and one older than 7 days or in the future is 400 INVALID_QUERY, as is a to not after from. " +
				"limit 1 to 200 (default 50) and offset page the window; max_chars (1 to 100000) then stops the page " +
				"after the item whose words take the running sum past it, counting the UTF-16 units of text and of " +
				"dispatch_words, while total counts every item in the window. {session_id, from, to, items, total, " +
				"limit, offset}, to null where the request names none. A session_id that is not a session id is 400 " +
				"INVALID_SESSION_ID; a session Dispatch holds nothing for answers an empty page. Not an authorization " +
				"boundary: a person may read any session's list.",
			Handler:  s.listSessionAnswersV2,
			Resource: "answers", Kind: kindCollection, TimeFields: []string{"at"},
			Query:    sessionAnswersV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: sessionAnswersV2{}},
			Refusals: listRefusals(sessionIDMalformedRefusal),
		},
		{
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
			OAuthScopes: person,
			Refusals:    listRefusals(),
		},
		{
			Method: http.MethodGet, Pattern: "/block-links", Auth: authAny, Versions: onlyV2,
			Description: "Links from blocks of one document to blocks of another: from=<document ref> and to=<document " +
				"ref> (dispatch://KEY/spec, dispatch://KEY/artifact/<slug> or dispatch://PROJECT/artifact/<slug>; one " +
				"document may be both), each link with both block nodes, its edge kind, and when and in which version of " +
				"its document the source block first held it, newest first; a link counts only while both its blocks are " +
				"current blocks of their documents. {from, to, items, total, limit, offset}, from and to the two document " +
				"nodes; limit 1 to 200 (default 50) and offset. A from or a to that names no document reference is 400 " +
				"INVALID_REFERENCE, and one naming a document that does not exist 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.getBlockLinksV2,
			Resource: "block-links", Kind: kindCollection,
			Query:    blockLinksV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: blockLinksV2{}},
			Refusals: listRefusals(documentMissingRefusal, referenceInvalidRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/reference-targets", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("What many dispatch:// references point at, in one read: ref=<dispatch ref> once per "+
				"reference, at most %d of them; more than that is 400 CAP_EXCEEDED naming the cap. One member per ref in "+
				"the request's order (a reference named twice answered twice), each {ref, kind, "+
				"name, gone}: ref as the request wrote it, kind the kind of node it names, name the words shown for the target "+
				"in place of its text (an issue's title, a document's name, an ask's question, a comment's or a message's body, "+
				"a component's title, a block's text), and gone whether nothing answers to that reference now, once an id it "+
				"writes short is resolved, which carries no name. A member naming a comment or a message, or a block of "+
				"one, also carries edited_at where the post's author edited it, and deleted_at and deleted_by where its "+
				"author deleted it: a deleted post's member is not gone and its name is \"\", and a member naming a block "+
				"of it is gone, with no name and no text_tree. A caller that sent Dispatch-Trees: 1 is also given text_tree "+
				"on each member whose kind is block: the block's whole text as the tree a reader is shown, links and references "+
				"kept and nothing cut, a typed block's being of what stands between its opening line and its closing one, read "+
				"by the block's kind as GET /api/v1/reference-targets reads it. No ref is 400 INVALID_REFERENCE, as is a "+
				"reference the grammar cannot read, naming it; one that reads but names nothing is answered gone, never "+
				"refused. An item id or a "+
				"block id written short (8 characters or more, never a whole uuid) is resolved in the scope the reference "+
				"names, as GET /api/v2/references resolves its to=, and ref still echoes it as written: a prefix that nothing "+
				"carries is answered gone, and a prefix that two or more items carry answers that member gone with ambiguous: "+
				"true, which no other member carries, and every other member is answered; this read refuses no prefix. limit 1 "+
				"to %d (default %d, so one read of at most the cap is one page) and offset page the members; {items, total, "+
				"limit, offset}, total the count of refs named. The answer carries an ETag over the page, each tree and each "+
				"ambiguous included; If-None-Match answers 304. This answers no edges: GET /api/v2/references reads one "+
				"node's edges.", referenceTargetsMax, referenceTargetsMax, referenceTargetsMax),
			Handler:  s.listReferenceTargetsV2,
			Resource: "reference-targets", Kind: kindCollection,
			Query: referenceTargetsV2Query{},
			List:  listBounds{Default: referenceTargetsMax, Max: referenceTargetsMax},
			Shape: routeShape{
				Status: http.StatusOK, Response: page[model.ReferenceTarget]{},
				Also: []routeAnswer{{Status: http.StatusNotModified}},
			},
			Refusals: listRefusals(referenceInvalidRefusal, valuePastBoundRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/block-schema", Auth: authPublic, Versions: onlyV2,
			Description: "The typed document block schema as JSON, the bytes pre-rendered.",
			Handler:     s.getBlockSchema,
			Resource:    "block-schema", Kind: kindBytes,
			Shape: routeShape{
				Status:             http.StatusOK,
				Opaque:             "the block schema is pre-rendered JSON bytes (pmdoc.SchemaJSON), written as they stand",
				OpaqueContentTypes: []string{"application/json"},
			},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/search-results", Auth: authAny, Versions: onlyV2,
			Description: searchResultsV2Description,
			Handler:     s.searchResultsV2,
			Resource:    "search-results", Kind: kindCollection,
			TimeFields: []string{"written_at"}, CrossProject: true,
			Query: searchResultsV2Query{},
			Shape: routeShape{Status: http.StatusOK, Response: searchResultsV2{}},
			List:  listBounds{Default: searchDefaultLimit, Max: searchMaxLimit},
			Refusals: listRefusals(queryProjectMalformedRefusal, numberProjectMalformedRefusal, issueStatusInvalidRefusal,
				searchAuthorInvalidRefusal, valuePastBoundRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/nodes", Auth: authAny, Versions: onlyV2,
			Description: "The nodes of one person, newest first, read from the node and tuple store across every project: " +
				"what they wrote and what reached a session of theirs. person=<login> names the person and is required " +
				"(400 INVALID_QUERY without it). Each row carries the node's kind and id, the relations that put it on " +
				"the list (authored_by, delivered_to, or both), the first line of its text, when it was written and the " +
				"dashboard path it opens at. A comment its author edited carries edited_at, and one its author deleted " +
				"keeps its row with deleted_at and deleted_by and an empty title. project=<key>, repeatable, keeps the " +
				"rows of each project it names; none " +
				"keeps every project's (400 INVALID_PROJECT for a value that is no project key). {items, total, limit, " +
				"offset}, limit 1 to 200 (default 50). Needs the person scope. A person none of whose nodes the reader " +
				"may see, a login nobody holds among them, lists no row, so the answer never says whether that person " +
				"exists; the reader's own login lists the rows they may see. GET /api/v1/people/{login}/nodes answers " +
				"the same rows under rows, and a 404 where this lists none for another login.",
			Handler:  s.listPersonNodesV2,
			Resource: "nodes", Kind: kindCollection, CrossProject: true,
			Query:       personNodesV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[personNode]{}},
			OAuthScopes: routeScopeSet{model.ScopePerson},
			Refusals:    listRefusals(queryProjectMalformedRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/search-coverage", Auth: authAny, Versions: onlyV2,
			Description: "Meaning search's coverage {identity, place, model, total, kinds, not_indexed}: the configured " +
				"embedder's identity, its place (bedrock, azure, or dev for a local harness) and model, all three null " +
				"with none configured; then, in total and for each kind (issue, document_block, comment_block, " +
				"message_block, ask_part), the searchable items, how many carry a current vector from that identity, how " +
				"many lack one, how many failed for good, and how many seconds the oldest lacking item has waited (null " +
				"when none lacks). not_indexed names every kind of node neither leg of search holds a row of - component, " +
				"session, document_version, result and handoff - each {kind, items}, items being how many of it the " +
				"database holds. Every number is read in one statement, so they all read one snapshot, under a 10 s " +
				"statement_timeout: a statement that runs past it counts nothing and answers 503 " +
				"SEARCH_COVERAGE_TIMEOUT, to be asked again later.",
			Handler:  s.searchCoverage,
			Resource: "search-coverage", Kind: kindReport,
			Shape:    routeShape{Status: http.StatusOK, Response: model.SearchCoverage{}},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, searchCoverageTimedOutRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/going-actions", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("The catalogue of operations an agent may propose on Dispatch's graph, at their /api/v2 "+
				"routes: %d of the %d tool specs agents operate Dispatch with, every one that writes but dispatch_review, "+
				"each carried whole - its own name (action), its own description, the arguments it takes (request), each "+
				"with the bounds its spec states, min and max (a text's length in UTF-16 units, a list's count of items or a "+
				"number's value, both left out where it states none), with "+
				"an example call, the /api/v2 rows it writes in the order a call is matched against them (routes, each "+
				"{method, pattern, body?}), and the graph relations its verbs write. A route's body is the fixed part of "+
				"the body a call of it sends beside the action's arguments, such as {\"state\": \"resolved\"} on PUT "+
				"/api/v2/asks/{id}/state; an argument that picks between two routes of one action (dispatch_claim's "+
				"release, dispatch_follow's action, dispatch_resolve_ask's kind) is sent to neither. It leaves "+
				"dispatch_review out on purpose: a review records an approval or a changes request in a person's name, "+
				"and the loop that proposes operations must never propose one. Every action is on one page unless the "+
				"request names a limit (any of 1 or more) and an offset; {items, total, limit, offset}.",
				len(goingToolRoutesV2), len(contracts.DispatchTools)),
			Handler:  s.listGoingActionsV2,
			Resource: "going-actions", Kind: kindCollection,
			Query:    goingActionsV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: page[goingActionV2]{}},
			List:     listBounds{Whole: true},
			Refusals: listRefusals(),
		},
		{
			Method: http.MethodPost, Pattern: "/going-requests", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("Ask a question of what Dispatch holds {words, project}: records the question and "+
				"answers 201 with its run - {id, author, project, words, created_at, ends_at}, whose ends_at is the %s "+
				"the run has to answer in - and answers it behind that, each frame of the answer stored for GET "+
				"/api/v2/going-requests/{id}/stream to carry. words is required (400 GOING_INPUT when it is blank) and at "+
				"most %d characters (400 CAP_EXCEEDED); project is the project key the question is asked of, empty "+
				"asking of every project (400 INVALID_PROJECT when it is not a key), and a credential scoped to one "+
				"project asks only of that project. The run belongs to the credential that asked it, which the stream "+
				"compares its reader with, so a credential that names nobody - the deployment's shared token - is "+
				"refused 403 %s and records nothing. With no model configured the run is recorded all the same and its "+
				"stream carries the error frame %s.", contracts.GoingRunWindow, contracts.GoingWordsMax,
				apicode.GoingAskerNamesNobody.Name(), contracts.GoingErrorNoModelConfigured),
			Handler:  s.createGoingRequestV2,
			Resource: "going-requests", Kind: kindCollection, Key: "{id}",
			// project is optional in the body, so a question may belong to no project: a collection
			// of no one project's, which stands at the top level (R1.2 of dispatch://INFRA-2946/spec).
			Body:  requestBody{Kind: bodyJSON, Type: createGoingRequestInput{}},
			Shape: routeShape{Status: http.StatusCreated, Response: goingRequest{}},
			Scope: scopeProjectInBody,
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, goingAskerNamesNobodyRefusal, goingWordsBlankRefusal,
				valuePastBoundRefusal, bodyProjectMalformedRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/going-requests/{id}/stream", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("One run's answer as server-sent events, which no envelope can carry: each frame is "+
				"`id:` its sequence, `event:` its name (%s, %s, %s, %s, %s, %s, %s) and `data:` the JSON that frame "+
				"carries, flushed as it is written, and the stream ends on %s or %s. Its position is the Last-Event-ID "+
				"header: the stream answers the frames after that sequence, so a reader that drops rereads nothing. Any "+
				"pod serves it, since the frames are rows: a run past its ends_at that stored no ending is ended by the "+
				"reader itself, with the error %s. Once a comment or an issue message whose text the run gave its model "+
				"is deleted, every read of the stream gives none of the run's frames and only the error %s, under the id "+
				"of the run's ending, and nothing to a reader that already holds that id. A run the record does not "+
				"hold, an id that is not a uuid, and a run that is another caller's are one answer, 404 %s, so an id "+
				"tells a caller nothing it did not already know.", contracts.GoingEventText, contracts.GoingEventItem,
				contracts.GoingEventCut, contracts.GoingEventChanged, contracts.GoingEventNone, contracts.GoingEventEnd,
				contracts.GoingEventError, contracts.GoingEventEnd, contracts.GoingEventError,
				contracts.GoingErrorAnswerDidNotArrive, contracts.GoingErrorAnswerWithdrawn,
				apicode.GoingRequestNotFound.Name()),
			Handler:  s.streamGoingAnswer,
			Resource: "stream", Kind: kindStream,
			Shape: routeShape{
				Status:             http.StatusOK,
				Opaque:             "the run's answer is a text/event-stream of hand-written id:/event:/data: frames, not one JSON body",
				OpaqueContentTypes: []string{"text/event-stream"},
			},
			IDs:      &idRoute{Reason: "a going request's run id, which the route reads for its own author alone and answers for itself"},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, goingRequestMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/limit-set", Auth: authAny, Versions: onlyV2,
			Description: "The bounds a write is held to, each read from the constant that enforces it: block_units, how " +
				"long one block of a body may be; item_blocks, how many blocks a body may hold; ask_options and " +
				"ask_units, how many options an ask may have and how long its question and options are together; " +
				"event_body_units, how long a body an event carries whole may be; and request_bytes, document_bytes and " +
				"handoff_bytes, the bytes a request, a document and a handoff body may hold.",
			Handler:  s.getLimits,
			Resource: "limit-set", Kind: kindSingleton,
			Shape:    routeShape{Status: http.StatusOK, Response: model.DispatchLimits{}},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/users", Auth: authAny, Versions: onlyV2,
			Description: "Everyone who has signed in, by lowercase email, each {login}: the assignee picker's options. " +
				"Needs the person scope. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}.",
			Handler:  s.listUsersV2,
			Resource: "users", Kind: kindCollection,
			Query:       usersV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[dispatchUser]{}},
			OAuthScopes: person,
			Refusals:    listRefusals(),
		},
		{
			Method: http.MethodGet, Pattern: "/dashboard-builds", Auth: authAny, Versions: onlyV2,
			Description: "Which dashboard builds are served: each {entry, serving}, entry the entry chunk " +
				"(/assets/index-*.js) of a build some live server process serves, sorted, and serving whether it is the " +
				"one this process serves. A tab offers its reload notice only when total is 1 and that entry is not the " +
				"tab's own: while a rolling update or a rollback runs two builds, each tab keeps the page it has. limit 1 " +
				"to 200 (default 50) and offset; {items, total, limit, offset}.",
			Handler:  s.listDashboardBuildsV2,
			Resource: "dashboard-builds", Kind: kindCollection,
			Query:    dashboardBuildsV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: page[dashboardBuildV2]{}},
			Refusals: listRefusals(),
		},
		{
			Method: http.MethodPost, Pattern: "/session-groups", Auth: authAny, Versions: onlyV2,
			Description: "Create a Delivery session group {name, sessions, actor?}, answering 201 the group {id, name, " +
				"sessions, created_by, created_at, updated_by, updated_at}. " + sessionGroupText + " A blank name, no " +
				"session, or a blank session id is 400 SESSION_GROUP_INPUT; a session another group holds is 409 " +
				"SESSION_IN_GROUP, a name another group has 409 SESSION_GROUP_NAME_TAKEN, and a name that is the title of " +
				"a session outside the group 409 SESSION_GROUP_NAME_IS_SESSION_TITLE.",
			Handler:  s.createSessionGroupV2,
			Resource: "session-groups", Kind: kindCollection, Key: "{id}",
			Body:     requestBody{Kind: bodyJSON, Type: createDeliverySessionGroupInput{}},
			Shape:    routeShape{Status: http.StatusCreated, Response: delivery.SessionGroup{}},
			Scope:    scopeDeployment,
			Refusals: sessionGroupWriteRefusals(undeclaredKeyRefusal),
		},
		{
			Method: http.MethodDelete, Pattern: "/session-groups/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Delete a Delivery session group {actor?}: the group and its memberships go, and its sessions " +
				"show as themselves again. 204. " + sessionGroupText + " 404 SESSION_GROUP_NOT_FOUND for an id no group " +
				"carries.",
			Handler:  s.deleteSessionGroupV2,
			Resource: "session-groups", Kind: kindItem, Key: "{id}",
			Body:     requestBody{Kind: bodyJSON, Type: actorInput{}, Optional: true},
			Shape:    routeShape{Status: http.StatusNoContent},
			Scope:    scopeDeployment,
			IDs:      sessionGroupIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, sessionGroupMissingRefusal},
		},
		{
			Method: http.MethodPatch, Pattern: "/session-groups/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Rename a Delivery session group, add sessions to it and remove sessions from it {name?, add?, " +
				"remove?, actor?}, answering the group, with the create's refusals; 400 SESSION_GROUP_INPUT for a blank " +
				"name or an edit that would leave it empty. " + sessionGroupText + " 404 SESSION_GROUP_NOT_FOUND for an " +
				"id no group carries.",
			Handler:  s.updateSessionGroupV2,
			Resource: "session-groups", Kind: kindItem, Key: "{id}",
			Body:     requestBody{Kind: bodyJSON, Type: updateDeliverySessionGroupInput{}},
			Shape:    routeShape{Status: http.StatusOK, Response: delivery.SessionGroup{}},
			Scope:    scopeDeployment,
			IDs:      sessionGroupIDs(),
			Refusals: sessionGroupWriteRefusals(undeclaredKeyRefusal, sessionGroupMissingRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/deploy-runs/{id}", Auth: authAny, Versions: onlyV2,
			Description: "One run of the deployment's deploy repository, by its GitHub run id, with every job it ran, by " +
				"start, each with its result and timings: {id, url, jobs}. The repository is the deployment's own " +
				"(GET /api/v2/settings/delivery), so the path names no project. 400 RUN_ID_INPUT for an id that is not " +
				"a positive integer, 404 NOT_FOUND for no such run of that repository, 404 DELIVERY_NOT_CONFIGURED " +
				"without the delivery settings.",
			Handler:  s.getDeliveryRun,
			Resource: "deploy-runs", Kind: kindItem, Key: "{id}",
			Shape:    routeShape{Status: http.StatusOK, Response: delivery.DeliveryRunDetailView{}},
			IDs:      &idRoute{Reason: "a deploy-repository run id, a positive integer and no uuid at all"},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, runIDMalformedRefusal, deployRunMissingRefusal, deliveryUnconfiguredRefusal},
		},
		{
			Method: http.MethodPost, Pattern: "/markdown-round-trips", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("The markdown Dispatch would store for the text a body gives it {markdown}: the "+
				"parse and the render every write of a whole document makes, answered 200 {markdown}. The route writes "+
				"nothing and reads no row. A caller compares the answer with the text it holds, and refuses its own write "+
				"where the two differ. Text the parse or the render refuses, and text longer than a document may hold, "+
				"comes back as it was given, so the write that follows refuses it with its own error. The body may hold "+
				"%d bytes, as much as a document's text written as a JSON string; past that it is 413 REQUEST_TOO_LARGE.",
				markdownRoundTripLimit),
			Handler:  s.postMarkdownRoundTripV2,
			Resource: "markdown-round-trips", Kind: kindComputation,
			Body:      requestBody{Kind: bodyJSON, Type: markdownRoundTripInput{}},
			Shape:     routeShape{Status: http.StatusOK, Response: markdownRoundTripResponse{}},
			Scope:     scopeNoWrite,
			BodyLimit: markdownRoundTripBodyLimit,
			Refusals:  []apicode.Refusal{undeclaredKeyRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/ci-reconcile-passes", Auth: authAny, Versions: onlyV2,
			Description: "The CI reconcile passes that started within from and to, RFC3339, from inclusive and to " +
				"exclusive, newest first; from defaults to 24 hours before to, or before now where to is absent, and an " +
				"absent to sets no end, so a request naming neither reads every pass from 24 hours ago on, and a from not " +
				"before to is 400 INVALID_QUERY. Rows are kept for 48 " +
				"hours and pruned by the pass that ends. Each pass is its id, the server process that ran it (instance), " +
				"started_at and ended_at, what it did (counts: graphql_queries, graphql_points, rest_requests, " +
				"rest_not_modified, heads_over_rest, records, heads, heads_known) and where it left the cursor, which is " +
				"where the pass after it takes the order up (cursor: repository, from, number, rest, counted); both are " +
				"null until the pass ends. state is running, complete, deadline (its own five-minute bound cut it " +
				"short), rate_limit (a shared GitHub limit ended it), cancelled (its process shut down under it) or " +
				"ended_without_completing, a pass whose process died before it could record its end: a pass with no end " +
				"reads running only while its process is live, the lease is held and it is the newest pass of all, " +
				"inside the window or not. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}.",
			Handler:  s.listCIReconcilePassesV2,
			Resource: "ci-reconcile-passes", Kind: kindCollection, TimeFields: []string{"started_at"},
			Query:    ciReconcilePassesV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: page[cireconcile.PassRecord]{}},
			Refusals: listRefusals(),
		},
	}
}

// agentsV2Query is GET /api/v2/agents's query: one page.
type agentsV2Query struct {
	pageQuery
}

// listAgentsV2 answers GET /api/v2/agents: one page of the list GET /api/v1/agents answers whole
// (agentRows).
func (s *server) listAgentsV2(w http.ResponseWriter, r *http.Request) {
	rows, ok := s.agentRows(w, r)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(rows, v2Query[agentsV2Query](r).pageQuery))
}

// sessionAnswersV2Query is GET /api/v2/agents/{session_id}/answers's query: the window on at, the
// page, and the cap on the page's words.
type sessionAnswersV2Query struct {
	pageQuery
	From     *time.Time `query:"from"`
	To       *time.Time `query:"to"`
	MaxChars *int       `query:"max_chars" min:"1" max:"100000"`
}

// sessionAnswersV2 is GET /api/v2/agents/{session_id}/answers's answer: the envelope, beside the
// session and the window it was read over, to null where the request named none.
type sessionAnswersV2 struct {
	SessionID string     `json:"session_id"`
	From      time.Time  `json:"from"`
	To        *time.Time `json:"to"`
	page[model.SessionAnswer]
}

// listSessionAnswersV2 answers GET /api/v2/agents/{session_id}/answers: the items GET
// /api/v1/agents/{session_id}/answers reads (sessionanswers.List), over [from, to), one page of
// them, the page cut at max_chars as /api/v1 cuts its whole list (sessionAnswersWithin).
func (s *server) listSessionAnswersV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	session, err := requestSessionID(r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	query := v2Query[sessionAnswersV2Query](r)
	now := time.Now().UTC()
	from := now.Add(-sessionAnswersWindow)
	if query.From != nil {
		if from, err = sessionAnswersStart("from", *query.From, now); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	if query.To != nil && !query.To.After(from) {
		writeError(w, r, apicode.InvalidQuery, "to must be after from")
		return
	}
	items, err := sessionanswers.List(r.Context(), s.deps.Store.Pool, session, from)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if query.To != nil {
		items = slices.DeleteFunc(items, func(item model.SessionAnswer) bool { return !item.At.Before(*query.To) })
	}
	answer := sessionAnswersV2{SessionID: session, From: from, To: query.To, page: pageOf(items, query.pageQuery)}
	if query.MaxChars != nil {
		answer.Items = sessionAnswersWithin(answer.Items, *query.MaxChars)
	}
	WriteJSON(w, http.StatusOK, answer)
}

// blockLinksV2Query is GET /api/v2/block-links's query: the two documents, and the page.
type blockLinksV2Query struct {
	pageQuery
	// From is the document whose blocks hold the links, as a dispatch:// reference.
	From string `query:"from"`
	// To is the document whose blocks the links reach.
	To string `query:"to"`
}

// blockLinksV2 is GET /api/v2/block-links's answer: the links in the envelope, the two document
// nodes beside it.
type blockLinksV2 struct {
	From model.GraphNode `json:"from"`
	To   model.GraphNode `json:"to"`
	page[model.BlockLink]
}

// getBlockLinksV2 answers GET /api/v2/block-links: one page of the links GET /api/v1/block-links
// answers whole, a document that does not exist answered with its own 404.
func (s *server) getBlockLinksV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[blockLinksV2Query](r)
	who := s.requestReader(r)
	documents, err := s.blockLinkDocuments(r.Context(), who, [2]string{query.From, query.To}, apicode.ArtifactNotFound)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	links, err := refs.BlockLinks(r.Context(), s.deps.Store.Pool, who, documents[0].ID, documents[1].ID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, blockLinksV2{From: documents[0], To: documents[1], page: pageOf(links, query.pageQuery)})
}

// referenceTargetsV2Query is GET /api/v2/reference-targets's query: the references to resolve,
// and the page of their members.
type referenceTargetsV2Query struct {
	pageQuery
	// Ref is one reference to resolve, given once per reference, at most referenceTargetsMax of
	// them; naming none is 400 INVALID_REFERENCE and naming more than the cap 400 CAP_EXCEEDED.
	// An item id or a block id written short in it is resolved in the scope the reference names.
	Ref []string `query:"ref"`
}

// listReferenceTargetsV2 answers GET /api/v2/reference-targets: one page of the members GET
// /api/v1/reference-targets answers whole (referenceTargets), read from the decoded query, under
// an ETag of its own. The trees are resolved before the tag is made, as the v1 read resolves them,
// because the tag hashes them.
func (s *server) listReferenceTargetsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[referenceTargetsV2Query](r)
	trees := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	targets, err := s.referenceTargets(r.Context(), s.requestReader(r), query.Ref, trees)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	trees.resolve()
	answer := pageOf(targets, query.pageQuery)
	if answerNotModified(w, r, referenceTargetsV2ETag(answer)) {
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// referenceTargetsV2ETag is GET /api/v2/reference-targets's validator: the page's members, hashed
// as the v1 read hashes its whole answer (hashReferenceTargets), after the version and the
// page's bounds, so a page and the v1 answer of the same set never share a tag.
func referenceTargetsV2ETag(answer page[model.ReferenceTarget]) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "version=v2\ntotal=%d\nlimit=%d\noffset=%d\n", answer.Total, answer.Limit, answer.Offset)
	hashReferenceTargets(hash, answer.Items)
	return `"` + hex.EncodeToString(hash.Sum(nil)) + `"`
}

// searchResultsV2Description is GET /api/v2/search-results's description.
var searchResultsV2Description = fmt.Sprintf("Full-text search q= across issues, documents, asks, comments and "+
	"messages: every match ranked on one list across the kinds by one score, the keyword leg's length-normalised "+
	"rank and the meaning leg's cosine weighted onto one scale and summed, with total, every match, and reachable, "+
	"how many the pages can reach (each leg's best %d). A comment's, a message's and an ask's hit carries author, "+
	"the actor that wrote it. A q that is one issue key in any form a person writes it - "+
	"INFRA-1042, infra-1042, INFRA 1042, or the number alone, which names that issue of the project the number is "+
	"read in, or of every project at once, all of them then heading the list in project order - puts that issue "+
	"first. The number is read in the project the search is narrowed to where it names one, and otherwise in "+
	"number_project, the project of the page the search was made from, which narrows nothing. A q is searched as "+
	"typed, websearch_to_tsquery's AND of its words; only a q that matches nothing at all is searched a second time "+
	"for its %d rarest words ORed, and the answer's query {operators, broadened} names the forms it read and whether "+
	"that second search answered. project=<key>, repeatable, narrows every leg to those projects, with no default "+
	"project; status= the status of the issue a row belongs to (a row with no issue matches none); author= the "+
	"actor id that wrote the item a row names; from and to, RFC3339, from inclusive and to exclusive, when that "+
	"item was last written. filters echoes {project, status, author, from, to}, project [] for none. "+
	"kind=, repeatable, one of issue, document, comment, ask and message, keeps the rows of the kinds named alone, "+
	"before anything is ranked, counted or paged, and none is every kind, as GET /api/v1/search/results reads it. "+
	"keywords_only=true runs the keyword leg alone and makes no embedding call, and its answer is not stamped "+
	"degraded; degraded names why a search that wanted meaning answered by keywords alone. Ordered by score, then "+
	"recency, then id. limit 1 to %d (default %d) and offset; {items, total, limit, offset, reachable, took_ms, "+
	"degraded?, query, filters}. A q under 2 characters and a from not before to are 400 INVALID_QUERY, and a q over "+
	"%d characters 400 CAP_EXCEEDED; a project or number_project that is not a project key is 400 INVALID_PROJECT or "+
	"INVALID_NUMBER_PROJECT, and a status or an author that is none is 400 INVALID_STATUS or INVALID_AUTHOR.",
	contracts.SearchKindDepth, searchBroadenWords, searchMaxLimit,
	searchDefaultLimit, contracts.SearchQueryMax)

// searchResultsV2Query is GET /api/v2/search-results's query.
type searchResultsV2Query struct {
	pageQuery
	Q string `query:"q"`
	// Project narrows every leg to these projects; none is every project.
	Project []string `query:"project"`
	// NumberProject is the project a bare issue number is read in where the search names none.
	NumberProject string     `query:"number_project"`
	Status        *string    `query:"status"`
	Author        *string    `query:"author"`
	From          *time.Time `query:"from"`
	To            *time.Time `query:"to"`
	KeywordsOnly  bool       `query:"keywords_only"`
	// Kind keeps the rows of the kinds named alone; none is every kind (searchKinds).
	Kind []string `query:"kind" enum:"issue,document,comment,ask,message"`
}

// searchFiltersV2 is what a /api/v2 search narrowed every leg by: the projects, [] for every
// project, and the status, the author and the window, each left out where the request named none.
type searchFiltersV2 struct {
	Project []string `json:"project"`
	Status  string   `json:"status,omitempty"`
	Author  string   `json:"author,omitempty"`
	From    string   `json:"from,omitempty"`
	To      string   `json:"to,omitempty"`
}

// searchResultsV2 is GET /api/v2/search-results's answer: GET /api/v1/search/results's, its
// results the envelope's items, and what describes the page beside them.
type searchResultsV2 struct {
	page[model.SearchResult]
	Reachable int                      `json:"reachable"`
	TookMS    int64                    `json:"took_ms"`
	Degraded  string                   `json:"degraded,omitempty"`
	Query     model.SearchQueryReading `json:"query"`
	Filters   searchFiltersV2          `json:"filters"`
}

// searchResultsV2 answers GET /api/v2/search-results: the search GET /api/v1/search/results runs
// (searchAnswer), over the projects the request names, read as the acting person (actingReader),
// as /api/v1's is.
func (s *server) searchResultsV2(w http.ResponseWriter, r *http.Request) {
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	query := v2Query[searchResultsV2Query](r)
	parameters, problem := searchParametersV2(r, query)
	if problem != nil {
		s.writeHandlerError(w, r, problem)
		return
	}
	parameters.who = who
	response, err := s.searchAnswer(r, parameters)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	filters := searchFiltersV2{Project: parameters.projects, Status: parameters.status, Author: parameters.author}
	switch {
	case parameters.project != "":
		filters.Project = []string{parameters.project}
	case filters.Project == nil:
		filters.Project = []string{}
	}
	if parameters.since != nil {
		filters.From = parameters.since.Format(time.RFC3339Nano)
	}
	if parameters.until != nil {
		filters.To = parameters.until.Format(time.RFC3339Nano)
	}
	WriteJSON(w, http.StatusOK, searchResultsV2{
		page:      newPage(response.Results, response.Total, query.pageQuery),
		Reachable: response.Reachable, TookMS: response.TookMS, Degraded: response.Degraded,
		Query: response.Query, Filters: filters,
	})
}

// searchParametersV2 is the search a /api/v2 query asks for, or the refusal of a value no version
// of the search route takes, with /api/v1's code for each.
func searchParametersV2(r *http.Request, query searchResultsV2Query) (searchParameters, *apiError) {
	text := strings.TrimSpace(query.Q)
	if problem := searchTextProblem(text); problem != nil {
		return searchParameters{}, problem
	}
	var projects []string
	for _, value := range query.Project {
		project := strings.TrimSpace(value)
		if !projectKeyPattern.MatchString(project) {
			return searchParameters{}, errorf(apicode.InvalidProject, "%s", projectKeyProblem("project", project))
		}
		if !slices.Contains(projects, project) {
			projects = append(projects, project)
		}
	}
	numberProject := strings.TrimSpace(query.NumberProject)
	if numberProject != "" && !projectKeyPattern.MatchString(numberProject) {
		return searchParameters{}, errorf(apicode.InvalidNumberProject, "%s", projectKeyProblem("number_project", numberProject))
	}
	// The decoder holds each kind to searchIndexedKinds, the enum above, so every value is known.
	kinds, _ := parseSearchKinds(query.Kind)
	parameters := searchParameters{text: text, numberProject: numberProject, since: query.From, until: query.To,
		kinds: kinds, limit: query.Limit, offset: query.Offset, keywordsOnly: query.KeywordsOnly, trees: wantsDisplayTrees(r)}
	if query.Status != nil {
		if parameters.status = strings.TrimSpace(*query.Status); !model.IsIssueStatus(parameters.status) {
			return searchParameters{}, searchStatusRefusal()
		}
	}
	if query.Author != nil {
		if parameters.author = strings.TrimSpace(*query.Author); !searchAuthorTaken(parameters.author) {
			return searchParameters{}, searchAuthorRefusal()
		}
	}
	if query.From != nil && query.To != nil && !query.From.Before(*query.To) {
		return searchParameters{}, errorf(apicode.InvalidQuery, "from must be before to")
	}
	if len(projects) == 1 {
		parameters.project = projects[0]
	} else {
		parameters.projects = projects
	}
	reading := readSearchQuery(text)
	parameters.reading = &reading
	return parameters, nil
}

// goingActionsV2Query is GET /api/v2/going-actions's query: a page of the catalogue, which is read
// whole where the request names no limit.
type goingActionsV2Query struct {
	pageQuery
}

// listGoingActionsV2 answers GET /api/v2/going-actions: the /api/v2 catalogue (goingToolRoutesV2),
// as GET /api/v1/going/catalogue answers the /api/v1 one.
func (s *server) listGoingActionsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(s.goingV2.actionsV2, v2Query[goingActionsV2Query](r).pageQuery))
}

// createGoingRequestV2 answers POST /api/v2/going-requests: the question POST
// /api/v1/going/requests records and answers (createGoingRequestFrom).
func (s *server) createGoingRequestV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.goingAsker(w, r)
	if !ok {
		return
	}
	s.createGoingRequestFrom(w, r, actor, v2Body[createGoingRequestInput](r))
}

// usersV2Query is GET /api/v2/users's query: one page.
type usersV2Query struct {
	pageQuery
}

// listUsersV2 answers GET /api/v2/users: one page of the people GET /api/v1/users lists whole
// (signedInUsers).
func (s *server) listUsersV2(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	users, err := s.signedInUsers(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(users, v2Query[usersV2Query](r).pageQuery))
}

// dashboardBuildsV2Query is GET /api/v2/dashboard-builds's query: one page.
type dashboardBuildsV2Query struct {
	pageQuery
}

// dashboardBuildV2 is one dashboard build a live server process serves: its entry chunk, and
// whether it is the build this process serves.
type dashboardBuildV2 struct {
	Entry   string `json:"entry"`
	Serving bool   `json:"serving"`
}

// listDashboardBuildsV2 answers GET /api/v2/dashboard-builds: the builds GET
// /api/v1/dashboard/builds answers as live (liveDashboardEntries), each marked whether this process
// serves it.
func (s *server) listDashboardBuildsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	entries, err := s.liveDashboardEntries(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	builds := make([]dashboardBuildV2, len(entries))
	for index, entry := range entries {
		builds[index] = dashboardBuildV2{Entry: entry, Serving: entry == s.deps.DashboardEntry}
	}
	WriteJSON(w, http.StatusOK, pageOf(builds, v2Query[dashboardBuildsV2Query](r).pageQuery))
}

// createSessionGroupV2 answers POST /api/v2/session-groups: the group POST
// /api/v1/delivery/session-groups creates (createDeliverySessionGroupFrom).
func (s *server) createSessionGroupV2(w http.ResponseWriter, r *http.Request) {
	s.createDeliverySessionGroupFrom(w, r, v2Body[createDeliverySessionGroupInput](r))
}

// requireSessionGroupV2 answers whether the group id names exists, and where it does not writes 404
// SESSION_GROUP_NOT_FOUND (requireHeldV2).
func (s *server) requireSessionGroupV2(w http.ResponseWriter, r *http.Request, id string) bool {
	return s.requireHeldV2(w, r, id, "session group", `select exists (select 1 from delivery_session_groups where id = $1::uuid)`,
		apicode.SessionGroupNotFound, "no session group "+id)
}

// deleteSessionGroupV2 answers DELETE /api/v2/session-groups/{id}: the delete DELETE
// /api/v1/delivery/session-groups/{id} makes (deleteDeliverySessionGroupAs), the group held to
// exist first.
func (s *server) deleteSessionGroupV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireSessionGroupV2(w, r, r.PathValue("id")) {
		return
	}
	s.deleteDeliverySessionGroupAs(w, r, v2Body[actorInput](r))
}

// updateSessionGroupV2 answers PATCH /api/v2/session-groups/{id}: the edit PATCH
// /api/v1/delivery/session-groups/{id} writes (updateDeliverySessionGroupFrom), the group held to
// exist first.
func (s *server) updateSessionGroupV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireSessionGroupV2(w, r, r.PathValue("id")) {
		return
	}
	s.updateDeliverySessionGroupFrom(w, r, v2Body[updateDeliverySessionGroupInput](r))
}

// postMarkdownRoundTripV2 answers POST /api/v2/markdown-round-trips: what POST
// /api/v1/markdown/round-trip answers (markdownRoundTrip), its body read by the decoder within the
// row's own bound.
func (s *server) postMarkdownRoundTripV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	WriteJSON(w, http.StatusOK, markdownRoundTrip(v2Body[markdownRoundTripInput](r)))
}

// ciReconcilePassesV2Query is GET /api/v2/ci-reconcile-passes's query: the window on started_at,
// and the page.
type ciReconcilePassesV2Query struct {
	pageQuery
	From *time.Time `query:"from"`
	To   *time.Time `query:"to"`
}

// listCIReconcilePassesV2 answers GET /api/v2/ci-reconcile-passes: the passes that started within
// [from, to) (cireconcile.Passes), one page of them, where GET /api/v1/ci-reconcile/passes answers
// the last 24 hours whole (cireconcile.RecentPasses). A request naming neither bound reads from 24
// hours before the database's clock on, that instant included, which /api/v1's window leaves out;
// one naming to alone reads the 24 hours before it.
func (s *server) listCIReconcilePassesV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[ciReconcilePassesV2Query](r)
	from := query.From
	if from == nil && query.To != nil {
		from = new(query.To.Add(-cireconcile.PassWindow))
	}
	if from != nil && query.To != nil && !from.Before(*query.To) {
		writeError(w, r, apicode.InvalidQuery, "from must be before to")
		return
	}
	passes, err := cireconcile.Passes(r.Context(), s.deps.Store.Pool, from, query.To)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(passes, query.pageQuery))
}
