// routes_shapes.go holds what a route's shape is written in: routeShape, the answers it names,
// the query-parameter structs, and the check that every route states one. Each route states its
// own shape in its own file (routeDecl.Shape): what it takes, what it answers, and with which
// status. Paths, auth and prose alone would describe no body, so the shapes are the data the
// OpenAPI document is reflected out of (INFRA-1406). Nothing here changes behaviour: no handler
// reads a type declared below, and the query structs are declarations alone -
// `r.URL.Query().Get` and `.Has` differ on an absent key, so moving forty handlers' reads onto a
// struct would be a behaviour change, and is not this file's business.
package api

import (
	"sort"
)

// routeAnswer is one answer a route can give: a status and a zero value of the Go type its body
// carries, nil for a body-less answer.
type routeAnswer struct {
	Status int
	Body   any
}

// routeShape is what one route takes and answers, in Go types an OpenAPI document is reflected
// out of (INFRA-1406). Each route states its own (routeDecl.Shape), and register refuses to mount
// a route that states none (shapelessRoutes).
//
// Two conventions a reader should know. A route whose answer can carry either advice shape names
// the richer one: writeAdvice holds every field documentWriteAdvice and suggestionsOnlyAdvice
// hold, so a document write with an issue and one without differ by which of its fields are
// present, not by which type describes them. And a route whose success answer is an object this
// server does not model - a relay's bytes, an event stream, a file's bytes - carries no Response
// and says why in Opaque.
//
// A third: a route names the shape its handler writes, trees and all. A read that shows a stored
// text answers a shaped read whose fields carry that text's tree (display_trees.go), and the
// route names that read rather than the model inside it, or the document would describe a body
// without the fields Dispatch sends and a client generated from it could not hold them. The test
// that reads a route's emitted answer for the tree fields its shape carries
// (display_trees_test.go) holds the rule for the routes its table lists, and a new read that
// carries a tree adds a row there; the routes it does not list rest on this rule alone. Two
// things that look like the same fault and are not: a write answers the row it wrote, so POST
// /artifacts/{id}/reviews, POST .../versions/{number}/reviews and POST .../plan-reviews name
// model.ArtifactReview while the GET beside them names []artifactReviewRead - the handlers write
// what docs.QueueReview returned, and the dashboard draws the read rather than the write's answer
// (ApprovalChip.tsx invalidates and refetches); and a model that carries its own tree field needs
// no shaped read at all, which is why model.MyAnswersResponse, model.MyRepliesResponse and
// model.SearchResponse stand as they are.
type routeShape struct {
	// Status is the status the route answers when it succeeds.
	Status int
	// Response is a zero value of the Go type the success answer carries: nil for a route that
	// answers no body, and nil with Opaque set for one whose bytes Dispatch does not model.
	Response any
	// Nullable is a route whose success answer is Response or the JSON literal `null`, which is
	// a shape of the answer rather than a second answer: OpenAPI keys a response on its status,
	// and both arms are the same status.
	Nullable bool
	// Also are the route's other answers, each at a status of its own: the second success of a
	// route that answers 200 or 201 by state, and the 304 of a conditional read.
	Also []routeAnswer
	// Request is a zero value of the request body's Go type, nil for a route that takes none.
	Request any
	// OptionalRequest is a route whose handler takes an empty body as well as one it decodes
	// (decodeOptionalJSON, or a handler that reads a field off bytes it relays). Every other
	// route with a Request refuses an empty body (decodeJSON), which is what makes the body
	// required. It stands here rather than in a table of its own so that each fact about a row's
	// request is stated once, under the row, and cannot go stale when the row is renamed.
	OptionalRequest bool
	// Multipart is a route that takes multipart/form-data as well as JSON.
	Multipart bool
	// Params is a zero value of the route's query-parameter struct, each field tagged
	// `query:"<key>"`; nil for a route that reads none. Path parameters are not here: they are
	// read off the pattern.
	Params any
	// Headers are the request headers the route reads beyond the ones every route reads, each with
	// whether a request must send it, which the emitter declares as header parameters.
	Headers []routeHeader
	// Advice is a zero value of the advice shape the success answer carries beside its payload
	// (withAdvice, withRawAdvice and withSuggestions merge an `advice` sibling into the payload
	// object rather than nesting it: withAdvice and withRawAdvice in write_advice.go,
	// withSuggestions in suggestions.go); nil for an answer that carries none.
	Advice any
	// Refusals are this route's refusal bodies richer than errorBody, each with its status.
	Refusals []routeAnswer
	// Opaque is why this route's answer carries no schema, for a route that relays or pre-renders
	// its bytes rather than modelling them; empty for every other route.
	Opaque string
	// OpaqueContentTypes are the media types an Opaque answer is served as, one for each kind of
	// bytes the route answers; empty where Opaque is.
	OpaqueContentTypes []string
	// OpaqueRequest is why this route's body carries no schema, for a route that reads the bytes
	// whole rather than modelling them; empty for every other route, and never beside Request.
	OpaqueRequest string
}

// routeHeader is one request header a route reads (routeShape.Headers): its name, whether a
// request must send it, and what it carries.
type routeHeader struct {
	Name        string
	Required    bool
	Description string
}

// placementIfMatch is the If-Match header of a node's move: the node's placement_seq as the tree
// read answers it, as a quoted entity tag, compared strongly (RFC 9110 section 13.1.1).
var placementIfMatch = routeHeader{
	Name: "If-Match", Required: true,
	Description: `the node's placement_seq, the seq of its current part_of that the tree read answers on every node, as a ` +
		`quoted entity tag ("123"): none is 428 PRECONDITION_REQUIRED, and a tag that is no longer the node's ` +
		`placement_seq is 412 PRECONDITION_FAILED`,
}

// deleteIfMatch is the If-Match header of a node's delete: the placement_seq of the node, as the
// move names it (placementIfMatch), which the delete itself must send and its dry run reads not.
var deleteIfMatch = routeHeader{
	Name: "If-Match",
	Description: `the node's placement_seq, the seq of its current part_of that the tree read answers on every node, as a ` +
		`quoted entity tag ("123"), which the delete itself must send: none is 428 PRECONDITION_REQUIRED, and a tag that is ` +
		`no longer the node's placement_seq is 412 PRECONDITION_FAILED; the dry run (dry_run=true) reads none`,
}

// --- the query-parameter structs -------------------------------------------------------------
//
// One struct per route, or per set of routes reading the same keys, each field carrying the key
// it is read from. They are declarations: the handlers keep their own reads. Two keys of the API
// are named by no struct here. `session` is read by actingPerson (server.go) on every route that
// resolves a person, so it belongs to the whole API rather than to any one operation and the
// emitter adds it everywhere; the delivery facet of the same name is left out with it. `cursor`
// on GET /api/v1/issues is read only to refuse it (400 INVALID_QUERY, issue_page.go), and naming
// it would offer a client a parameter that cannot work. No struct here is ever a request body,
// and the emitter reads only each field's `query` and `enum` tags and the Go type it carries.

// projectArtifactsQuery is GET /api/v1/projects/{key}/artifacts's query.
type projectArtifactsQuery struct {
	// Unlinked, `true`, lists only the project's documents that belong to no issue.
	Unlinked bool `query:"unlinked"`
}

// nodeDeleteQuery is DELETE /api/v1/issues/{key}/nodes/{id}'s query (node_delete.go).
type nodeDeleteQuery struct {
	// DryRun, `true`, writes nothing and answers {count}, the number of nodes the delete would take
	// that a read of the issue's tree returns to the caller, after every refusal the delete reads
	// before it writes; any value but true or false is 400 DELETE_INPUT.
	DryRun bool `query:"dry_run"`
	// ExpectedCount is the count the dry run answered, which the delete itself must name (400
	// DELETE_INPUT without it) and takes or refuses: a count that has moved since is 409
	// DELETE_COUNT_MOVED, carrying the count now. The dry run reads none.
	ExpectedCount int `query:"expected_count"`
}

// relatedQuery is GET /api/v1/projects/{key}/related's query: the related-item check an issue
// create runs, over a title and a text, writing nothing.
type relatedQuery struct {
	// Title is checked as the new issue's title; either it or Text alone is enough.
	Title string `query:"title"`
	// Text is checked as the new issue's text, under the title and a newline.
	Text string `query:"text"`
	// ExcludeIssue leaves that issue of the project and everything it holds out of the results
	// and out of the word statistics; an issue of another project is 400 EXCLUDE_ISSUE_INPUT.
	ExcludeIssue string `query:"exclude_issue"`
	// ExcludeIssues, repeatable, leaves each of at most relatedExcludeLimit issues of the
	// project out of the results alone, leaving the word statistics as they are; a longer list,
	// or a key the project does not hold, is 400 EXCLUDE_ISSUE_INPUT.
	ExcludeIssues []string `query:"exclude_issues"`
	// KeywordsOnly, `true`, skips meaning search; any value but true or false is 400.
	KeywordsOnly bool `query:"keywords_only"`
}

// projectPlansQuery is GET /api/v1/projects/{key}/plans's query: the window on the finished
// issues' close.
type projectPlansQuery struct {
	// Since lists only an issue closed at or after it, RFC 3339.
	Since string `query:"since"`
	// Until lists only an issue closed before it, RFC 3339; a since not before it is 400
	// INVALID_QUERY.
	Until string `query:"until"`
}

// deliveryWindowQuery is the query GET /api/v1/delivery/timeline and GET /api/v1/delivery/measures
// share: the window, the search and the repeatable facets. The `session` facet is read by the
// same code (deliveryFacets) and named here by no field, since `session` is the whole API's
// parameter.
type deliveryWindowQuery struct {
	// From is the window's start, RFC3339; absent it is seven days before To.
	From string `query:"from"`
	// To is the window's end, RFC3339, exclusive; absent it is now.
	To string `query:"to"`
	// Q searches a pull request's title or its id.
	Q string `query:"q"`
	// Repo selects pull requests of these repositories.
	Repo []string `query:"repo"`
	// ParentAgent selects pull requests whose first session belongs to these parents;
	// __no_session__ selects those whose commits name no session.
	ParentAgent []string `query:"parent_agent"`
	// Issue selects pull requests of these issues; __no_issue__ selects those naming none.
	Issue []string `query:"issue"`
	// Priority selects P0-P3; __no_priority__ selects an issue with none.
	Priority []string `query:"priority"`
	// Component selects these components, a parent including its children;
	// __no_component__ selects an issue with none.
	Component []string `query:"component"`
	// Author selects these pull-request authors.
	Author []string `query:"author"`
	// Rework selects by whether the work was rework.
	Rework []string `query:"rework"`
	// Deployed selects by deployed_status: deployed, waiting or not_tracked.
	Deployed []string `query:"deployed"`
}

// issueResultsQuery is GET /api/v1/issues/{key}/results's query.
type issueResultsQuery struct {
	// At replaces what every check of the issue is compared with by this one commit, 40
	// lowercase hex characters; any other value is 400 INVALID_QUERY. The only read of GitHub it
	// leaves is the check runs of a check CI proves, read at each open pull request head, or with
	// none open the final head of the newest merged one.
	At string `query:"at"`
}

// deliveryPipelineQuery is GET /api/v1/delivery/pipeline's query.
type deliveryPipelineQuery struct {
	// At fixes the day the seven-day Pipeline window ends on, RFC3339; absent it is now.
	// A malformed value is 400 INVALID_QUERY.
	At string `query:"at"`
}

// issueListQuery is GET /api/v1/issues's query: the filters and the offset page.
type issueListQuery struct {
	// Pinned, `true`, lists the caller's pinned issues; it needs the person scope, the pins
	// being one person's.
	Pinned bool `query:"pinned"`
	// Project lists one project's issues.
	Project string `query:"project"`
	// Status, repeatable, lists issues standing in any of these statuses; none, or only blank
	// ones, is every status. A value is matched as written, so one the lifecycle does not hold
	// matches no issue and is never refused: alone it lists none, beside another it adds nothing.
	Status []string `query:"status"`
	// Parent lists the children of one issue.
	Parent string `query:"parent"`
	// Label lists issues carrying every label given.
	Label []string `query:"label"`
	// UpdatedSince lists issues updated at or after this RFC3339 time.
	UpdatedSince string `query:"updated_since"`
	// ClosedSince lists issues closed at or after this RFC3339 time.
	ClosedSince string `query:"closed_since"`
	// ClosedBefore lists issues closed before this RFC3339 time.
	ClosedBefore string `query:"closed_before"`
	// Priority lists issues at these priorities, 0-3, or `none` for unset.
	Priority []string `query:"priority"`
	// Advances lists issues that advance these priorities, by their own relation or by one an
	// issue above them states, over every project; `none` lists those that advance no live
	// priority and `any` those that advance one, which is every other issue.
	Advances []string `query:"advances"`
	// Own, `true`, keeps, of the issues the Advances values answer, those whose own row names a
	// priority a value matches and no issue above them carries it: a child that only inherits a
	// priority, or restates one an issue above it carries, is left out, and `none`, which
	// matches no priority, keeps nothing. Without an Advances value it filters nothing.
	// `advances=any&own=true` over backlog and icebox is the second fault of GET /api/v1/priorities.
	Own bool `query:"own"`
	// Moved, `true`, adds moved_at and moved_by to each row standing in backlog or icebox: when
	// the issue was moved there and who moved it. A listing without it reads neither.
	Moved bool `query:"moved"`
	// RouteStatus lists open issues whose route is live, no_holder or unknown.
	RouteStatus string `query:"route_status"`
	// Open, `true`, lists open issues alone.
	Open bool `query:"open"`
	// Limit is the page size, 1 to contracts.MaxIssuePageLimit; with Offset it makes the
	// answer a page rather than an array.
	Limit int `query:"limit"`
	// Offset is where the page starts, 0 or more; alone it pages at the default limit.
	Offset int `query:"offset"`
}

// prioritiesQuery is GET /api/v1/priorities's query. The list of priorities is the whole
// deployment's and neither key narrows it; both narrow what the route answers beside it, which
// is the counts of the work under each priority and the two fault lists.
type prioritiesQuery struct {
	// Project, repeatable, narrows the counts and the fault lists to those projects and leaves
	// the list of priorities whole. It is the repeatable key issueListQuery's Priority is.
	Project []string `query:"project"`
	// Summary, `true`, answers the two fault counts alone, which is what the navigation rail
	// reads.
	Summary bool `query:"summary"`
}

// issueResolveQuery is GET /api/v1/issues/resolve's query.
type issueResolveQuery struct {
	// Ref is the reference to resolve: an issue key, or owner/repo#n.
	Ref string `query:"ref"`
}

// eventListQuery is the query GET /api/v1/issues/{key}/events and GET /api/v1/artifacts/{id}/events
// share: one page of an event log.
type eventListQuery struct {
	// After reads the events above this position in the owner's event sequence (an event's seq),
	// oldest first, 0 to 2147483647; it combines with neither Before nor order=desc.
	After int `query:"after"`
	// Before reads the events below this position in the owner's event sequence, 0 to
	// 2147483647.
	Before int `query:"before"`
	// IDs reads exactly these event ids, comma-separated.
	IDs string `query:"ids"`
	// Order is asc or desc.
	Order string `query:"order"`
	// Limit is how many events the page holds.
	Limit int `query:"limit"`
	// ExcludeTypes leaves these event types out of the page, comma-separated: at most 50 of the
	// types Dispatch writes, before limit counts the page. Blank leaves out none; an unknown type
	// or an empty item in the list is 400 INVALID_QUERY.
	ExcludeTypes string `query:"exclude_types"`
}

// eventLogQuery is GET /api/v1/events/log's query: one page of the whole event log, which is
// read forward by id and nothing else.
type eventLogQuery struct {
	// After reads the events above this event id, oldest first; 0, its default, reads the log
	// from its start.
	After int `query:"after"`
	// Limit is how many events the page holds, 1 to 1000, and 1000 by default.
	Limit int `query:"limit"`
}

// issueReferencesQuery is GET /api/v1/issues/{key}/references's query.
type issueReferencesQuery struct {
	// Versions, `latest`, leaves each member's artifact.versions holding only its latest
	// version, under its own ETag; absent, empty or `all` is the full answer, another value or
	// a repeat is 400 INVALID_QUERY.
	Versions string `query:"versions"`
}

// referenceTargetsQuery is GET /api/v1/reference-targets's query: the references to resolve.
type referenceTargetsQuery struct {
	// Ref is one reference to resolve, given once per reference, at most
	// referenceTargetsMax of them; naming none is 400 INVALID_REFERENCE and naming more
	// than the cap is 400 CAP_EXCEEDED. An item id or a block id written short in it is
	// resolved in the scope the reference names, and a prefix two or more items carry
	// answers that member gone with ambiguous: true, and every other member is answered.
	Ref []string `query:"ref"`
}

// referencesQuery is GET /api/v1/references's query: the edges of one node.
type referencesQuery struct {
	// To reads the backlinks of this dispatch:// reference; exactly one of To and From is
	// required.
	To string `query:"to"`
	// From reads the links out of this dispatch:// reference.
	From string `query:"from"`
	// Kind keeps only these edge kinds, comma-separated.
	Kind string `query:"kind"`
	// Since reads the mentions above this events.id, up to the event log's safe horizon.
	Since int64 `query:"since"`
}

// referenceQuestionsQuery is GET /api/v1/references/questions's query: the one source whose
// document's question blocks are read.
type referenceQuestionsQuery struct {
	// Kind is the kind of the reference source, one of artifact, ask, comment or message;
	// anything else is 400 INVALID_KIND.
	Kind string `query:"kind" enum:"artifact,ask,comment,message"`
	// ID is that source's whole uuid or a unique prefix of 8 characters or more; it is
	// required, a prefix two sources carry is 409 ID_AMBIGUOUS.
	ID string `query:"id"`
}

// blockLinksQuery is GET /api/v1/block-links's query: both documents are required.
type blockLinksQuery struct {
	// From is the document whose blocks hold the links, as a dispatch:// reference.
	From string `query:"from"`
	// To is the document whose blocks the links reach.
	To string `query:"to"`
}

// messageReplyQuery is POST /api/v1/messages/{id}/reply's query.
type messageReplyQuery struct {
	// FollowUp, `true`, posts other text as the session's follow-up under its first reply once
	// the attempt is answered; false or absent posts nothing, any other value is 400
	// MESSAGE_INPUT.
	FollowUp bool `query:"follow_up"`
}

// postHistoryQuery is the query GET /api/v1/comments/{id}/history and
// GET /api/v1/messages/{id}/history share: where the page after the first one starts.
type postHistoryQuery struct {
	// Before answers the rows written strictly before this RFC3339 time, which is how the
	// caller reads the page after one of 200 rows: the oldest edited_at it was answered.
	// Anything that is not an RFC3339 time is 400 INVALID_QUERY.
	Before string `query:"before"`
}

// inboxQuery is GET /api/v1/inbox's query.
type inboxQuery struct {
	// Project keeps the open asks of one project.
	Project string `query:"project"`
	// Assignee is me, unassigned (document asks included) or an email; an email nobody has
	// signed in with is 400 ASSIGNEE_NOT_ALLOWED.
	Assignee string `query:"assignee"`
}

// myAnswersQuery is the offset page GET /api/v1/me/answers and
// GET /api/v1/people/{login}/nodes each answer: the page, and nothing else. Neither route
// takes a kind, a filter or a traversal from its caller, so this struct is the whole of what
// either reads (answers.go's parseMyAnswersPage, which both parse their page with).
type myAnswersQuery struct {
	// Limit is the page size, 1 to 200, default 50.
	Limit int `query:"limit"`
	// Offset is where the page starts.
	Offset int `query:"offset"`
}

// myRepliesQuery is GET /api/v1/me/replies's query: the kinds of row and the state it reads
// them in, the page that state takes, and the one thread owner it may be narrowed to.
type myRepliesQuery struct {
	// Kind is the kinds of row answered, as one comma list of reply and agent (`reply`,
	// `agent`, `reply,agent`); an absent one reads reply alone, and any other value is
	// 400 INVALID_QUERY. It carries no enum, because a list of two words is not one of them.
	Kind string `query:"kind"`
	// State is unread (the default), inbox or dismissed; anything else is 400 INVALID_QUERY.
	State string `query:"state" enum:"unread,inbox,dismissed"`
	// Limit is the page size of state=unread and state=inbox, 1 to 200; an absent one is every
	// row, which is what the Inbox reads. It is refused under state=dismissed.
	Limit int `query:"limit"`
	// Offset is where that page starts; it is refused under state=dismissed.
	Offset int `query:"offset"`
	// Cursor is where a state=dismissed page resumes: the next_cursor of the page before it.
	// It is a query parameter of that state alone.
	Cursor string `query:"cursor"`
	// Issue narrows the rows to one issue's threads; with Artifact it is 400 INVALID_QUERY.
	Issue string `query:"issue"`
	// Artifact narrows the rows to one document's threads, by document id.
	Artifact string `query:"artifact"`
}

// searchRouteQuery is GET /api/v1/search's query. The name carries `route` because search.go
// already has a searchQuery of its own, the parsed query a search runs on. It names the four
// members every build of the server has served. The handler still reads the other five on this
// route, for a page served by a release before this one, and those five are declared on
// GET /api/v1/search/results (searchResultsRouteQuery), the path a caller asks them of.
type searchRouteQuery struct {
	// Q is the text searched for, at least 2 characters and at most contracts.SearchQueryMax.
	Q string `query:"q"`
	// Project searches within one project; empty searches every project. It narrows every leg
	// of the search rather than striking rows out of the page it answers.
	Project string `query:"project"`
	// Limit is the page size, 1 to 50, default 20.
	Limit int `query:"limit"`
	// Offset is where the page starts.
	Offset int `query:"offset"`
}

// searchResultsRouteQuery is GET /api/v1/search/results's query, all ten members: the four
// searchRouteQuery names and the six a build released before them does not serve, which is why
// that path exists. The ten are written out rather than embedded because the emitter reads a
// query struct's own fields and refuses one carrying no query tag (openapi.go, queryParameters).
type searchResultsRouteQuery struct {
	// Q is the text searched for, at least 2 characters and at most contracts.SearchQueryMax.
	Q string `query:"q"`
	// Project searches within one project; empty searches every project. It narrows every leg
	// of the search rather than striking rows out of the page it answers.
	Project string `query:"project"`
	// NumberProject narrows nothing: it is the project of the page the search was made from,
	// which a query that is a bare issue number is read in. A request naming Project too reads
	// the number in that project instead.
	NumberProject string `query:"number_project"`
	// Status searches the items of issues in one status; a row with no issue belongs to none
	// and is left out.
	Status string `query:"status"`
	// Author searches the items one actor wrote, by actor id: a person's login or a session id.
	Author string `query:"author"`
	// UpdatedSince searches the items last written at or after one RFC3339 time.
	UpdatedSince string `query:"updated_since"`
	// KeywordsOnly runs the keyword leg alone and makes no embedding call, whatever embedder
	// the deployment configured, which is what the bar sends behind a keystroke.
	KeywordsOnly bool `query:"keywords_only"`
	// Limit is the page size, 1 to 50, default 20.
	Limit int `query:"limit"`
	// Offset is where the page starts.
	Offset int `query:"offset"`
	// Kind ranks, counts and pages the kinds it names and no others, repeated once per kind;
	// with none named every kind the index holds is searched. A kind outside the five is
	// 400 INVALID_QUERY (searchIndexedKinds, parseSearchKinds).
	Kind []string `query:"kind" enum:"issue,document,comment,ask,message"`
}

// relatedChecksQuery is GET /api/v1/related-checks's query.
type relatedChecksQuery struct {
	// Project counts the checks of one project.
	Project string `query:"project"`
	// SourceKind counts the checks of one source: issue, ask or decision_block.
	SourceKind string `query:"source_kind"`
	// From is the window's start, RFC3339.
	From string `query:"from"`
	// To is the window's end, RFC3339, exclusive.
	To string `query:"to"`
}

// nearCopyChecksQuery is GET /api/v1/near-copy-checks's query.
type nearCopyChecksQuery struct {
	// Project is the project whose rows are listed; the handler refuses a request without one.
	Project string `query:"project"`
	// From is the window's start, RFC3339.
	From string `query:"from"`
	// To is the window's end, RFC3339, exclusive.
	To string `query:"to"`
	// After is the id of the row the page starts after: the last page's next_after.
	After string `query:"after"`
	// Limit is the page size, 1 to 1000, default 1000.
	Limit int `query:"limit"`
}

// sessionAnswersQuery is GET /api/v1/agents/{session_id}/answers's query.
type sessionAnswersQuery struct {
	// Since is the window's start, one RFC3339 time no older than the route's bound and not
	// after now.
	Since string `query:"since"`
	// MaxChars bounds how much of each item's text the answer carries.
	MaxChars int `query:"max_chars"`
}

// knownSessionsQuery is GET /api/v1/agents/known's query.
type knownSessionsQuery struct {
	// ID is one session id asked about, repeated: 1 to contracts.KnownSessionsMax of them, each a
	// session id (text.IsSessionID) of at most knownSessionIDMax bytes, the longest text names.
	ID []string `query:"id"`
}

// issueAsksQuery is GET /api/v1/issues/{key}/asks's query.
type issueAsksQuery struct {
	// State filters the list: all, open or answered.
	State string `query:"state"`
	// Thread adds each ask's thread: replies, rewordings, answers, followers, claimant, route
	// and route_holder. It is the literal `1` or absent, never a boolean: parseAskListThread
	// (asks.go) refuses every other value with 400 INVALID_ASK_THREAD, so a client that sent
	// `true` would be refused.
	Thread string `query:"thread" enum:"1"`
	// Order, `activity`, answers a page instead of the list. The keys below require it.
	Order string `query:"order" enum:"activity"`
	// Limit is the page size, 1 to 100, default 20.
	Limit int `query:"limit"`
	// Before is the previous page's position: UTC time with six fractional digits, then a comma and a lower-case UUID.
	Before string `query:"before"`
	// Author keeps threads the login or session ID wrote in.
	Author string `query:"author"`
}

// artifactAsksQuery is GET /api/v1/artifacts/{id}/asks's query.
type artifactAsksQuery struct {
	// State filters the list: all, open or answered.
	State string `query:"state"`
}

// artifactTextQuery is the query the three document text reads share: GET
// /api/v1/artifacts/{id}/text, GET /api/v1/issues/{key}/artifacts/{slug}/text and
// GET /api/v1/projects/{key}/artifacts/{slug}/text.
type artifactTextQuery struct {
	// Blocks, `true`, carries the document's blocks beside the markdown, the token and the
	// version, every one of them rendered from the one tree, so no write by another session
	// splits a listing. Any other value carries no blocks.
	Blocks bool `query:"blocks"`
}

// artifactChangesQuery is GET /api/v1/artifacts/{id}/changes's query: the two versions of the
// document compared. Both are required.
type artifactChangesQuery struct {
	// From is the earlier version, a positive number below To.
	From int `query:"from"`
	// To is the later version.
	To int `query:"to"`
}

// artifactVersionQuery is the query the four version reads share: GET
// /api/v1/artifacts/{id}/versions/{number}, GET
// /api/v1/issues/{key}/artifacts/{slug}/versions/{number}, GET
// /api/v1/projects/{key}/artifacts/{slug}/versions/{number} and GET
// /api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}.
type artifactVersionQuery struct {
	// Download, `true`, answers a download: `Content-Disposition: attachment` with the file's
	// name, `Cache-Control: private, no-cache`, and a document as its Markdown rather than the
	// JSON that wraps it. Any other value answers as without it.
	Download bool `query:"download"`
	// Blocks, `true`, carries a document version's typed blocks beside its markdown, each read
	// from that version's own text under the id its opening line writes, with its attributes and
	// its byte range in the markdown the answer carries: the version as this build renders it,
	// which is the stored text unless an earlier renderer wrote it otherwise, with the sha256 and
	// the size of that text. A block's references read zero and it carries no embedding state:
	// both describe the live document, not a version. Any other value carries no blocks.
	Blocks bool `query:"blocks"`
}

// openAsksQuery is GET /api/v1/asks/open's query: exactly one of the two scopes is required.
type openAsksQuery struct {
	// AuthorSession lists the open asks one session authored.
	AuthorSession string `query:"author_session"`
	// Project lists every open ask on a project's issues and documents.
	Project string `query:"project"`
	// Since lists the asks opened at or after this one RFC3339 time.
	Since string `query:"since"`
}

// issueCommentsQuery is GET /api/v1/issues/{key}/comments's query.
type issueCommentsQuery struct {
	// Artifact scopes the comments to one document of the issue.
	Artifact string `query:"artifact"`
	// NodeAnchor asks for each comment's node_anchor (R31 of the issue-tree spec).
	NodeAnchor bool `query:"node_anchor"`
	// Order, `activity`, answers a page instead of the list. The keys below require it.
	Order string `query:"order" enum:"activity"`
	// Limit is the page size, 1 to 100, default 20.
	Limit int `query:"limit"`
	// Before is the previous page's position: UTC time with six fractional digits, then a comma and a lower-case UUID.
	Before string `query:"before"`
	// Author keeps threads the login or session ID wrote in.
	Author string `query:"author"`
	// Person, `in`, keeps threads involving a person or on text.
	Person string `query:"person" enum:"in"`
}

// commentReadQuery is GET /api/v1/comments/{id}'s query.
type commentReadQuery struct {
	// NodeAnchor asks for the comment's node_anchor (R31 of the issue-tree spec).
	NodeAnchor bool `query:"node_anchor"`
}

// issueMessagesQuery is GET /api/v1/issues/{key}/messages's query.
type issueMessagesQuery struct {
	// Order, `activity`, answers a page instead of the list. The keys below require it.
	Order string `query:"order" enum:"activity"`
	// Limit is the page size, 1 to 100, default 20.
	Limit int `query:"limit"`
	// Before is the previous page's position: UTC time with six fractional digits, then a comma and a lower-case UUID.
	Before string `query:"before"`
	// Author keeps threads the login or session ID wrote in.
	Author string `query:"author"`
	// Person, `in`, keeps threads involving a person or with no target. `none` keeps targeted
	// threads with no person. Both omit roots from the issue project's Legion daemon.
	Person string `query:"person" enum:"in,none"`
}

// credentialApproverQuery is the query GET /api/v1/credential-requests and
// GET /api/v1/credential-grants share.
type credentialApproverQuery struct {
	// Approver must be the literal `me`: the viewer's own list, never a login they name.
	Approver string `query:"approver"`
}

// publishFrameQuery is POST /api/v1/agents/{session_id}/stream/_test/publish's query.
type publishFrameQuery struct {
	// As says what the posted bytes are: `replay` a transcript frame, `drop` an ended view,
	// `responder` the {"responding"} body; absent, one ordinary frame.
	As string `query:"as"`
}

// liveWritePauseQuery is GET /api/v1/artifacts/{id}/_test/live-write-pauses/{point}'s query.
type liveWritePauseQuery struct {
	// Wait is how long to wait for a write to park, a positive Go duration such as "10s".
	Wait string `query:"wait"`
}

// stated reports whether a route declared this shape. Every shape answers with some status, so a
// Shape whose Status is 0 is one the route never stated.
func (shape routeShape) stated() bool { return shape.Status != 0 }

// shapelessRoutes names every route whose declaration states no shape. A route with no shape is a
// route the generated document would leave out, which is how "one definition of the API" stops
// holding: a client would be given no type for it and would hand-write one. register refuses to
// mount such a table, as it refuses a write class it cannot decide (undecidableWrites).
func shapelessRoutes(routes []apiRoute) []string {
	var shapeless []string
	for _, route := range routes {
		if !route.Shape.stated() {
			shapeless = append(shapeless, route.Method+" "+route.Pattern+": no shape (its declaration's Shape)")
		}
	}
	sort.Strings(shapeless)
	return shapeless
}
