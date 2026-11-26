package api

// routes_versions.go is how one route table serves two versions of the API (R2 of
// dispatch://INFRA-4071/spec). Three kinds of row feed it, and routes() makes one apiRoute of
// each (version, row) pair they declare:
//
//   - A v1Row (v1Rows, routes_table.go) is a route of /api/v1 as it stood when /api/v2 began, at
//     its literal /api/v1 pattern. Its facts stand beside the table in the side maps, keyed by
//     "<method> <pattern>" (writeClasses, routeScopes, idRoutes, routeShapes, keyedReplays,
//     answerHoldsSecret, keyedUploads) and in the pattern constants (issueUploadRoute, and the
//     routes with a body limit of their own: handoffWriteRoute, markdownRoundTripRoute,
//     fileRoundTripRoute), and it names the route that answers it under
//     /api/v2 in its Successor. /api/v1 alone serves it, and testdata/v1-only-routes.txt holds
//     every one of them: those rows go when /api/v1 ends, so they keep the keys every open route
//     pull request names rather than being re-keyed.
//   - A routeRow (the v2<Group>Rows functions of the v2_<group>.go files) is a route written once,
//     with a version-free pattern, and every fact a wrapper of register or a reader of the route
//     reads is one of its named columns. It is served at /api/v2 and, unless its Versions says
//     otherwise, at /api/v1 too: a row a later change adds is served in both versions by one
//     declaration (R2), and the decoder and the authority gate read and hold its Query, its Body
//     and its Authorities at both paths (apiRoute.decoded). A row that answers for /api/v1 rows
//     which keep their own answer is served at /api/v2 alone (Versions: onlyV2).
//   - A testHookRow (testHookRows) is a test hook, served at its /api/v1 path and at
//     /api/v2/_test/<that path without /api/v1 and /_test>, both only when Deps.TestHooksEnabled.
//     Its facts are its side-map entries, as a v1Row's are.
//
// Every wrapper of register reads the apiRoute and never a string key: routes() copies a v1Row's
// side-map entries into the columns of the routeRow its apiRoute carries, so a wrapper reads one
// field whichever kind of row declared the route.

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// routeVersions is which versions of the API serve a routeRow. Its zero value is both. A route
// only /api/v1 serves is no routeRow: it is a v1Row, which names its successor, and the frozen
// list testdata/v1-only-routes.txt holds it (TestAV1OnlyRowOutsideTheFrozenListFails).
type routeVersions int

const (
	// bothVersions serves the row at /api/v1<Pattern> and at /api/v2<Pattern>.
	bothVersions routeVersions = iota
	// onlyV2 serves the row at /api/v2<Pattern> alone: a route of the new shape, which answers
	// what the v1Rows that name it as their Successor answered, while those rows keep their own
	// answer at /api/v1. A row with no older /api/v1 answer takes the zero value instead.
	onlyV2
)

// versionPrefix is the path every route of version is mounted under.
func versionPrefix(version apiVersion) string {
	if version == apiV2 {
		return "/api/v2"
	}
	return "/api/v1"
}

// v1Row is a row of v1Rows: the five columns every route has, written as a positional literal
// in the order of today's /api/v1 table, and the route that answers it under /api/v2. Its other
// facts stand in the side maps beside the table, keyed by "<method> <pattern>" (routes_versions.go,
// the file comment).
type v1Row struct {
	Method      string
	Pattern     string
	Auth        routeAuth
	Description string
	Handler     http.HandlerFunc
	Successor   successor
}

// successor is what answers a v1Row under /api/v2 (R2): the route the route map names for it
// (dispatch://INFRA-2946/artifact/route-map-md, section (a)), but for row 67, which maps to the
// deployment-wide GET /deploy-runs/{id} since its run is read from the deployment's own
// repository, and for the two near-copy rows of #4103, which the map predates and which map to GET
// /projects/{key}/near-copy-checks and GET /near-copy-checks/{id}.
//
// A `{name}` in Query, and on the right of a Path binding, is filled from the v1 request: its path
// value of that name, or, where the v1 pattern holds no such wildcard, its query value of that
// name (`ref={ref}` of GET /api/v1/issues/resolve). A wildcard of the v2 Pattern that no binding
// names is the v1 path value of the same name. One that the v1 pattern does not hold either is a
// value the v1 handler resolves while it answers - the artifact a slug names, the issue a
// document belongs to - and records for the Link header (R3.1); only rows 6, 17, 89, 91,
// 92, 161, 164, 165 and 178 of the map need one (TestEveryV1RowNamesItsV2Route).
type successor struct {
	// Method and Pattern name the /api/v2 route, its pattern version-free as a routeRow writes
	// it: the route the v2<Group>Rows mount (`GET /issues/{key}`, and `GET ` with an empty
	// pattern for the index at /api/v2 itself).
	Method  string
	Pattern string
	// Query is the query the v2 route answers this row under, for a row that became a filter
	// or a selector of it (`slug={slug}`, `include=blocks`, `state=open`); empty otherwise.
	Query string
	// Path binds a wildcard of Pattern the v1 pattern names otherwise or not at all: a mark's
	// `{kind}` is the literal kind (`agent`) and its `{id}` the v1 `{session_id}`. Nil for most
	// rows.
	Path map[string]string
	// Deleted marks a row with no /api/v2 route of its own, which Method and Pattern then name
	// the route that answers for (a filter, a selector, a duplicate); every other row names its
	// own route, at the same path, renamed, or merged with other rows into one.
	Deleted bool
	// None is the version-free path of the list a client finds the values of the wildcard its
	// handler resolves in, which a successful answer that resolved none links (Link, R3.1):
	// row 178, every project's architecture sources, links each project it lists and /projects
	// when it lists none. Empty for every other row.
	None string
}

// successorIs is the successor of a v1Row that keeps a route of its own in /api/v2: target is
// the version-free pattern with the successor's query after a `?` where it has one, and each
// binding is `<wildcard>=<value>` (successor.Path).
func successorIs(method, target string, bindings ...string) successor {
	pattern, query, _ := strings.Cut(target, "?")
	next := successor{Method: method, Pattern: pattern, Query: query}
	for _, binding := range bindings {
		name, value, _ := strings.Cut(binding, "=")
		if next.Path == nil {
			next.Path = map[string]string{}
		}
		next.Path[name] = value
	}
	return next
}

// deletedFor is the successor of a v1Row /api/v2 has no route for, written as successorIs
// writes one: target is the v2 route that answers for it.
func deletedFor(method, target string, bindings ...string) successor {
	next := successorIs(method, target, bindings...)
	next.Deleted = true
	return next
}

// whenNone is next for a row whose answer lists every value of the wildcard its handler
// resolves, where an answer listing none links pattern (successor.None).
func (next successor) whenNone(pattern string) successor {
	next.None = pattern
	return next
}

// testHookRow is a test hook's row: the five columns as a v1Row writes them, its pattern the
// /api/v1 path the hook has always answered at. A hook names no successor of its own: it is
// served in both versions (testHookV2Pattern), its /api/v1 answer links its /api/v2 path, and it
// is none of R1's rows.
type testHookRow struct {
	Method      string
	Pattern     string
	Auth        routeAuth
	Description string
	Handler     http.HandlerFunc
}

// testHookV2Pattern is the /api/v2 path of a hook at pattern: /api/v2/_test/ and the hook's v1
// path without /api/v1 and without its /_test segment, so POST
// /api/v1/artifacts/{id}/_test/outside-schema also answers POST
// /api/v2/_test/artifacts/{id}/outside-schema.
func testHookV2Pattern(pattern string) string {
	rest := strings.TrimPrefix(pattern, "/api/v1")
	rest = strings.Replace(rest, "/_test", "", 1)
	return "/api/v2/_test" + rest
}

// resourceKind is what a route's resource is, of the kinds the parent's shape rules decide a
// path, a method and an answer by (dispatch://INFRA-2946/spec, R1).
type resourceKind string

const (
	// kindCollection is a list of items: it answers {items, total, limit, offset} and takes
	// limit and offset (R1.4); a POST to it creates an item (R1.7).
	kindCollection resourceKind = "collection"
	// kindLog is a collection read in order by its position field, after and before (R1.5): an
	// issue's events, a document's, the global event log, the references and the caller's
	// dismissed threads.
	kindLog resourceKind = "log"
	// kindItem is one item, at its collection's path and its key (R1.2).
	kindItem resourceKind = "item"
	// kindSingleton is one object with no key of its own: a deployment setting
	// (/settings/<name>), a project's record of its own (/projects/{key}/settings/<name>), an
	// issue's claim, the caller (/me).
	kindSingleton resourceKind = "singleton"
	// kindState is an item's state, …/{id}/state, which a PUT moves with a typed body for each
	// state (R1.7).
	kindState resourceKind = "state"
	// kindReport is a read computed from what Dispatch stores that is no list: an issue's
	// coverage, the counts of the related checks. It answers no envelope (R1.4).
	kindReport resourceKind = "report"
	// kindComputation is a computation that stores nothing, one route (R1.7): the related
	// search, the markdown round trip, the machine-login lookup.
	kindComputation resourceKind = "computation"
	// kindBytes is a read that answers bytes: a file's version, or a document version that
	// answers the document's JSON or a file's bytes by what the artifact is (R1.4).
	kindBytes resourceKind = "bytes"
	// kindStream is a read that answers an event stream (server-sent events), which no envelope
	// can carry (R1.4).
	kindStream resourceKind = "stream"
)

// positionType is how a log's position field is written in after and before.
type positionType string

const (
	// positionInteger is a whole number: an owner's event seq, the global event id, a
	// reference's source_seq.
	positionInteger positionType = "integer"
	// positionTimeAndID is a time and an id, `<RFC3339 time>,<id>`, for a log ordered by a time
	// with an id to break its ties (R1.5): the caller's dismissed threads, and a post's
	// corrections.
	positionTimeAndID positionType = "time,id"
)

// positionField is the one field a log's after and before read (R5), which its index entry and
// its documentation name.
type positionField struct {
	// Field is the field's name as the answer writes it: seq, id, source_seq, or for a
	// positionTimeAndID both fields, dismissed_at,thread_id or edited_at,id.
	Field string
	Type  positionType
}

// listBounds is a list's limit: the one a request that names none is answered with, and the
// largest it may name, either of which a request outside is refused (R1.4). A route keeps the
// bound it has today; one with none takes 50 and 200 (limits).
type listBounds struct {
	Default int
	Max     int
	// Whole marks a fixed catalogue a request that names no limit reads whole, every item on one
	// page, as the route index and the going actions are (R1.4); Default and Max are 0 there.
	Whole bool
	// Counts marks a list whose limit may also be 0: a read of the total and of what stands beside
	// items, and no item, as GET /api/v2/priorities answers the two fault counts alone. Default and
	// Max are named as on any bounded list, and the handler answers limit 0 with no item itself,
	// since pageOf reads a limit of 0 as a catalogue's every item.
	Counts bool
}

// limits is the limit a request to a list is held to, which the decoder reads it by
// (decodeLimit) and the route index and the OpenAPI document state: the least limit it takes, 0
// for a list whose bounds count and 1 for every other; the one a request that names none is
// answered with; and the largest it may name. A list whose row names no bounds takes 50 and 200.
// A catalogue read whole has neither of the last two, and answers 0 for both: it takes any limit
// of 1 or more, and one that names none reads every item (pageOf).
func (bounds listBounds) limits() (least, fallback, most int) {
	least, fallback, most = 1, bounds.Default, bounds.Max
	if bounds.Counts {
		least = 0
	}
	if bounds == (listBounds{}) {
		fallback, most = defaultListLimit, maxListLimit
	}
	return least, fallback, most
}

// bodyKind is how a route's request body is written, which is how the v2 decoder reads it.
type bodyKind int

const (
	// bodyNone is a route that reads no body.
	bodyNone bodyKind = iota
	// bodyJSON is one JSON object, decoded into Type. A member Type does not declare is ignored
	// (R4).
	bodyJSON
	// bodyUpload is an upload: multipart/form-data holding the file and the fields of Type, or
	// the JSON object Type describes (uploadArtifactFor). The keyed layer spools it, and its limit
	// is an upload's (uploadBodyLimit).
	bodyUpload
	// bodyStateUnion is a JSON object whose `state` member picks the arm it is decoded into
	// (Arms, by state): the transition a PUT to …/{id}/state makes (R1.7).
	bodyStateUnion
	// bodyKindUnion is a JSON object whose `kind` member picks the arm it is decoded into (Arms,
	// by kind): a mark of the caller's (route map, section (e)).
	bodyKindUnion
	// bodyBytes is a body the handler reads whole as bytes it does not model; Opaque says why.
	bodyBytes
)

// requestBody is a route's request body as the table declares it.
type requestBody struct {
	Kind bodyKind
	// Type is a zero value of the Go type a bodyJSON or a bodyUpload body is decoded into; nil
	// for every other kind.
	Type any
	// Arms are a union's arms by the value of its discriminating member, each a zero value of
	// the Go type that arm is decoded into; nil for every kind but a union.
	Arms map[string]any
	// Optional is a route that takes an empty body as well as one it decodes; every other route
	// with a body refuses an empty one (decodeJSON).
	Optional bool
	// Opaque is why a bodyBytes body carries no schema; empty for every other kind.
	Opaque string
	// TextChecked is a bodyJSON body whose handler checks the text it carries itself, as the
	// /api/v1 route it answers for does, and refuses U+0000 in it under the upload's name for that
	// text: the inline round trip's markdown as `content` (postMarkdownRoundTrip) and the file
	// round trip's bytes as the file part (postFileRoundTrip). The decoder then reads it as that
	// route reads it, without its own refusal of U+0000 (readJSONBodyAllowingNUL), so a U+0000 in
	// a member the body does not declare is ignored at both paths. False for every other body,
	// whose U+0000 the decoder refuses.
	TextChecked bool
}

// routeBodyLimit is the largest body a route reads and its answer to a body one byte past it,
// which the drain, the keyed layer and the decoder all read so none of them can disagree with
// another; a route of the v1 table takes it from routeBody.
type routeBodyLimit struct {
	Bytes    int64
	TooLarge *apiError
}

// The bounds a route's body is held to. A routeRow names one in BodyLimit only when its body is
// a JSON body larger than jsonBodyLimit allows; a zero BodyLimit takes the bound of its Body
// (routeRow.bodyLimit).
var (
	// jsonBodyLimit is every JSON body's (decodeJSON): maxJSONRequestBytes.
	jsonBodyLimit = routeBodyLimit{maxJSONRequestBytes, errJSONTooLarge}
	// uploadBodyLimit is an upload's: a file and a megabyte of fields (keyedBodyMax).
	uploadBodyLimit = routeBodyLimit{keyedBodyMax, errUploadTooLarge}
	// handoffBodyLimit is a handoff write's (handoffRequestLimit).
	handoffBodyLimit = routeBodyLimit{handoffRequestLimit, errHandoffTooLarge}
	// markdownRoundTripBodyLimit is the inline markdown round trip's (markdownRoundTripLimit), and
	// fileRoundTripBodyLimit the file round trip's (fileRoundTripLimit).
	markdownRoundTripBodyLimit = routeBodyLimit{markdownRoundTripLimit, errMarkdownRoundTripTooLarge}
	fileRoundTripBodyLimit     = routeBodyLimit{fileRoundTripLimit, errFileRoundTripTooLarge}
)

// requestPart is the part of a request an authority rule reads.
type requestPart string

const (
	// partBody is a member of the decoded body.
	partBody requestPart = "body"
	// partQuery is a key of the decoded query.
	partQuery requestPart = "query"
)

// authorityWhere is the member of a body or the key of a query an authority rule reads.
type authorityWhere struct {
	In   requestPart
	Name string
}

// authorityRule is the authority one value of a request needs (R1.8), beside the route's own
// OAuthScopes, which every request to it needs: a body member's value (`state` `accepted` needs
// `person`), a body member's presence, where Value is empty (`default_assignee` needs `person`),
// a query value (`waiting_on` `me` needs `person`; a value of a comma list such as `include`
// is one of its members), or, where Absent, a query key's absence: the form of a read that names
// no `slug` is the list of a conversation's files, which needs `person`, while the form naming one
// reads one file. The wrapper that refuses a request lacking one, 403 OAUTH_SCOPE_REQUIRED naming
// the scope, runs after the decoder and before the handler (gateV2Authorities). A check that
// depends on the data, such as who wrote the object, stays in the handler, and the row's
// description names it.
type authorityRule struct {
	Where  authorityWhere
	Value  string
	Absent bool
	Scopes routeScopeSet
}

// projectPath names the object of a route's path whose project a write belongs to, which is the
// resolver the project check calls (withinProjectScope, projectResolver): the project of the
// path's issue, project, artifact, ask, comment, message or check result.
type projectPath int

const (
	// projectPathNone is a path that names no such object.
	projectPathNone projectPath = iota
	projectPathIssue
	projectPathProject
	projectPathArtifact
	projectPathAsk
	projectPathComment
	projectPathMessage
	projectPathResult
)

// documentOwnerKind is whose documents a route's path looks a document up among by its slug or
// filename (loadArtifactForRequest): an issue's or a project's.
type documentOwnerKind int

const (
	// documentOwnerNone is a route that looks no document up by slug.
	documentOwnerNone documentOwnerKind = iota
	documentOwnerIssue
	documentOwnerProject
)

// routeRow is one route as a v2<Group>Rows function declares it, every fact a named column. The
// columns a row leaves at its zero value say "none": no write class on a read, no scope a caller
// needs, no id the prefix layer resolves. routes() mounts the row once per version it serves
// (mountRow), at versionPrefix(version)+Pattern, and the apiRoute it mounts carries the row whole.
//
// The R1 columns (Resource to CrossProject) are what the parent's shape test decides its rules 2
// to 9 from (dispatch://INFRA-2946/spec, R1); the rows of the INFRA-979 tree (OfINFRA979) and the
// test hooks are bound by none of them, and leave them empty.
type routeRow struct {
	// Method is the route's HTTP method.
	Method string
	// Pattern is the route's path as ServeMux reads a pattern, without its version prefix:
	// `/issues/{key}`, which is mounted at /api/v2/issues/{key}. The route index at /api/v2 itself
	// is the one row with an empty Pattern. On an apiRoute it is the whole pattern the route is
	// mounted at.
	Pattern string
	// Auth is who may call the route, as its handler checks it.
	Auth routeAuth
	// Description is the route's prose, which the route index and the OpenAPI document both
	// carry.
	Description string
	// Handler answers the route.
	Handler http.HandlerFunc
	// Versions is which versions serve the row; its zero value is both, and a row served in both
	// is decoded and gated at both paths (apiRoute.decoded). A row that answers for v1Rows which
	// keep their own answer at /api/v1 names onlyV2; a row a later change adds, which has no older
	// /api/v1 answer to keep, takes the zero value and is served at both by its one declaration.
	Versions routeVersions
	// OfINFRA979 marks a row of the INFRA-979 tree, which v2Infra979Rows declares and marks each
	// of, and which that tree's own spec decides. The parent's shape rules bind none of them
	// (dispatch://INFRA-2946/spec, R1): the shape test, the decoder, the v2 document's contract,
	// the code list and the keyed writes of /api/v2 leave out every route mounted from a row it
	// marks, at either version (v2Bound, decoded).
	OfINFRA979 bool

	// Resource is the noun the route's path names what it serves by (R1.1): a plural for a
	// collection and its items (`issues`, `asks`, `marks`), a singular for a singleton (`claim`,
	// `me`).
	Resource string
	// Kind is what the route's resource is (resourceKind).
	Kind resourceKind
	// Key is the path segments that name one item of the resource, as the path writes them:
	// `{key}` for an issue, `{id}` for an ask, `{owner}/{repo}` for a repository, `{kind}/{id}`
	// for a mark. An item's path ends in it (R1.2), and a collection's items are at its path and
	// it.
	Key string
	// TimeFields are the time fields a read over time windows on: one for a read with one window,
	// `from` and `to`, and two for a read over two, whose windows are then `<field>_from` and
	// `<field>_to` (R1.5). Empty for a read that is not over time.
	TimeFields []string
	// Position is the field a log's after and before read (R5); zero for every route that is no
	// log.
	Position positionField
	// CrossProject is a read across projects: a top-level collection that takes a repeatable
	// `project` filter and has no default project (R1.3).
	CrossProject bool

	// Query is a zero value of the struct the route's query is decoded into, each field tagged
	// `query:"<key>"` (and `enum:"…"`, `default:"…"`, `min:"…"`, `max:"…"` where the value is held
	// to them); nil for a route that reads no query. A key it does not declare is refused (R4).
	Query any
	// Body is the route's request body and how it is written.
	Body requestBody
	// Shape is what the route answers: its success status and type, its other answers, the
	// advice it carries and its refusal bodies richer than errorBody. Its request is Query and
	// Body, never Shape's own Params and Request, which apiRoute.shape fills from them for the
	// OpenAPI emitter.
	Shape routeShape
	// Refusals are the refusals the route answers, each a condition and its code, one code per
	// condition across the whole table (R7, TestOneConditionOneCode).
	Refusals []apicode.Refusal
	// List is a list's limit bounds, zero for a list that takes 50 and 200 (listBounds.limits)
	// and for a route that is no list: a route is a list when its Query takes a limit.
	List listBounds

	// OAuthScopes are the scopes every request to the route needs (gateRouteScopes).
	OAuthScopes routeScopeSet
	// Authorities are the scopes one value of a request needs beside OAuthScopes (R1.8).
	Authorities []authorityRule
	// Scope is the write class of a mutating route (writeScope), which the project check decides
	// a caller by; every route whose method is not GET names one.
	Scope writeScope
	// ProjectOf is the object of the path whose project a scopeProject write belongs to, which
	// every scopeProject row names (undecidableWrites).
	ProjectOf projectPath
	// SessionPath is the path value that names the one session a scopeSession write belongs to
	// (`session_id`), which withinSessionScope holds a credential that proved a session to;
	// empty where the path names none.
	SessionPath string
	// IDs is what the path's {id} and {block_id} name, which the prefix layer resolves
	// (resolveIDPrefixes); every route whose pattern carries one names it, and nil means none.
	IDs *idRoute

	// BodyLimit is the largest body the route reads, where its Body is a JSON body larger than a
	// JSON body's own bound; zero takes the bound of its Body (routeRow.bodyLimit). On an
	// apiRoute it is always the bound the route is held to.
	BodyLimit routeBodyLimit
	// RefusesClosedOwner is an upload the owner's closed state refuses before its body is
	// spooled (refuseUploadToAClosedIssue).
	RefusesClosedOwner bool
	// Replay is how a repeat of a keyed write that commits a recovery point is answered
	// (keyedReplay): the writes that also deliver to sessions. Nil for every other route.
	Replay keyedReplay
	// Reread is how a repeat of a keyed write whose kept answer is one comment or one issue
	// message is answered (keyedReread): the post read again as it stands now, so an edit or a
	// delete since is in the answer. Nil for every other route.
	Reread keyedReread
	// Resumes and RefuseKey describe a keyed write with individually committed steps.
	Resumes   bool
	RefuseKey func(http.ResponseWriter, *http.Request, error, json.RawMessage)
	// KeyedFor selects recovery for a decoded body variant without changing the default write.
	KeyedFor func(*http.Request) (keyedRoute, bool)
	// AnswerHoldsSecret is a route whose answer carries a credential, which an Idempotency-Key
	// never keeps (keyedWrites).
	AnswerHoldsSecret bool
	// DocumentOwner is whose documents the path looks a document up among by slug
	// (loadArtifactForRequest); zero for a route that looks up none.
	DocumentOwner documentOwnerKind
}

// bodyLimit is the bound the row's body is held to: the one it names, or else an upload's for an
// upload and a JSON body's for every other.
func (row routeRow) bodyLimit() routeBodyLimit {
	switch {
	case row.BodyLimit.TooLarge != nil:
		return row.BodyLimit
	case row.Body.Kind == bodyUpload:
		return uploadBodyLimit
	default:
		return jsonBodyLimit
	}
}

// apiRoute is one mounted route: one (version, row) pair. register mounts each, and GET /api/v1
// and GET /api/v2 each list those of their own version, so the server and its indexes can never
// disagree about what is served. Its routeRow is the row whole, its Pattern the whole pattern
// the route is mounted at, and for a route of the v1 table (a v1Row's, or a test hook's) every
// column the side maps hold is copied in from them (v1Route), so a wrapper reads one field
// whichever kind of row declared the route.
type apiRoute struct {
	routeRow
	// Version is the version the route is mounted under.
	Version apiVersion
	// Successor is the /api/v2 route that answers the route's own under /api/v1, which its Link
	// header names (linkSuccessor): a v1Row's Successor, the /api/v2 mount of a routeRow served in
	// both versions, a test hook's /api/v2/_test path. Zero on every /api/v2 route.
	Successor successor
	// Read is the read class of a mounted GET row, or the zero entry on other methods.
	Read readEntry
	// columns marks the mount of a routeRow, whose facts are its own columns. A route of the v1
	// table - and a route a test builds by hand - carries false, and the mount guards read its
	// facts where the side maps hold them (sideMapName), so a side-map entry stays held to the
	// table.
	columns bool
	// sideMapPattern is the pattern the side maps hold a test hook's facts under, for the hook at
	// its /api/v2 path; empty for every other route, whose own pattern is that key.
	sideMapPattern string
}

// sideMapName is the "<method> <pattern>" the side maps hold the route's facts under, for a route
// whose facts they hold.
func (route apiRoute) sideMapName() string {
	return route.Method + " " + cmp.Or(route.sideMapPattern, route.Pattern)
}

// shape is what the route takes and answers, as the OpenAPI emitter and the mount guards read it:
// for a route of the v1 table its routeShapes entry, and for the mount of a routeRow its Shape
// with its Query and Body as the request. A union's arms are the document's to describe from
// Body.Arms (openAPIUnionArms, unionBody), so they leave Request nil here.
func (route apiRoute) shape() routeShape {
	shape := route.Shape
	if !route.columns {
		return shape
	}
	shape.Params = route.Query
	switch route.Body.Kind {
	case bodyJSON, bodyUpload:
		shape.Request = route.Body.Type
		shape.Multipart = route.Body.Kind == bodyUpload
	case bodyBytes:
		shape.OpaqueRequest = route.Body.Opaque
	}
	shape.OptionalRequest = route.Body.Optional
	return shape
}

// v1Route is the apiRoute of a route of the v1 table at its literal /api/v1 pattern: the row's
// five columns, and every fact the side maps and the pattern constants hold for it copied into
// the columns a routeRow declares them in. For a v1 route these are the facts register has always
// read, so every /api/v1 behaviour stays what it was.
func v1Route(row routeRow, replays map[string]keyedRoute) apiRoute {
	name := row.Method + " " + row.Pattern
	row.Scope = writeClasses[name]
	row.OAuthScopes = routeScopes[name]
	row.Shape = routeShapes[name]
	if entry, named := idRoutes[name]; named {
		row.IDs = &entry
	}
	row.ProjectOf = projectPathOf(row.Pattern)
	if strings.Contains(row.Pattern, "{session_id}") {
		row.SessionPath = "session_id"
	}
	limit, tooLarge := routeBody(row.Method, row.Pattern)
	row.BodyLimit = routeBodyLimit{limit, tooLarge}
	row.RefusesClosedOwner = name == issueUploadRoute
	keyed := replays[name]
	row.Replay, row.Resumes, row.RefuseKey = keyed.replay, keyed.resumes, keyed.refuse
	row.Reread = keyedRereads[name]
	row.AnswerHoldsSecret = answerHoldsSecret[name]
	row.DocumentOwner = documentOwnerOf(row.Pattern)
	return apiRoute{routeRow: row, Version: apiV1}
}

// documentOwnerOf is whose documents a /api/v1 pattern looks a document up among by its slug or
// filename, read from its shape as projectPathOf reads the object a pattern names, which routes()
// copies onto a v1 route's DocumentOwner column (v1Route): an issue's under
// /api/v1/issues/{key}/artifacts/{slug}, a project's under /api/v1/projects/{key}/artifacts/{slug},
// each with anything under it.
func documentOwnerOf(pattern string) documentOwnerKind {
	switch {
	case addresses(pattern, "/api/v1/issues/{key}/artifacts/{slug}"):
		return documentOwnerIssue
	case addresses(pattern, "/api/v1/projects/{key}/artifacts/{slug}"):
		return documentOwnerProject
	}
	return documentOwnerNone
}

// mountRow is the apiRoutes of one routeRow: one at /api/v2, and one at /api/v1 first where its
// Versions is both, its zero value, whose successor is the /api/v2 one. Each carries the row's
// own columns, its body bound resolved.
func mountRow(row routeRow) []apiRoute {
	versions := []apiVersion{apiV1, apiV2}
	if row.Versions == onlyV2 {
		versions = []apiVersion{apiV2}
	}
	routes := make([]apiRoute, 0, len(versions))
	for _, version := range versions {
		mounted := apiRoute{routeRow: row, Version: version, columns: true}
		mounted.Pattern = versionPrefix(version) + row.Pattern
		mounted.BodyLimit = row.bodyLimit()
		if version == apiV1 {
			mounted.Successor = successor{Method: row.Method, Pattern: row.Pattern}
		}
		routes = append(routes, mounted)
	}
	return routes
}

// testHookRoutes is a hook's two routes: at its /api/v1 path, whose successor is the other, and
// at its /api/v2/_test path with the same facts, read from the side maps under the v1 path.
func testHookRoutes(hook testHookRow) []apiRoute {
	atV1 := v1Route(routeRow{Method: hook.Method, Pattern: hook.Pattern, Auth: hook.Auth, Description: hook.Description, Handler: hook.Handler}, nil)
	atV2 := atV1
	atV2.Pattern = testHookV2Pattern(hook.Pattern)
	atV2.Version = apiV2
	atV2.sideMapPattern = hook.Pattern
	atV1.Successor = successor{Method: hook.Method, Pattern: strings.TrimPrefix(atV2.Pattern, "/api/v2")}
	return []apiRoute{atV1, atV2}
}

// servedAt is the routes version serves, in the table's order.
func servedAt(version apiVersion, routes []apiRoute) []apiRoute {
	return slices.DeleteFunc(slices.Clone(routes), func(route apiRoute) bool { return route.Version != version })
}
