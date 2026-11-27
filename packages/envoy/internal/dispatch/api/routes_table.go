package api

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// routeAuth names who may call a route. It describes the check the handler performs; the
// handler stays the enforcement point.
type routeAuth string

const (
	// authPublic needs no credential.
	authPublic routeAuth = "public"
	// authAny accepts any authenticated caller: a browser cookie or a bearer token. What such a
	// caller may do there is the route's own scopes' to decide (routeDecl.OAuthScopes), which a
	// person's credential holds and a credential the deployment configured holds none of.
	authAny routeAuth = "any"
	// authBearer accepts a bearer token only.
	authBearer routeAuth = "bearer"
)

// writeScope is what a mutating route's write belongs to, which is what lets one check decide a
// caller whose credential names one project (project_scope.go). Unlike routeAuth, which
// describes a check its handler makes, this is the enforcement point itself: register wraps
// every mutating route in it.
type writeScope string

const (
	// scopeProject is a write to an object that belongs to one project: the issue-key rows, the
	// project-key rows, the artifact-id rows, the ask-id rows, the comment-id rows, and the
	// message rows that carry an issue. A caller whose credential names one project may write
	// such an object of any project. The one exception is a private project it does not name
	// (access.CredentialCovers). A target that resolves to no project or to nothing is refused.
	scopeProject writeScope = "project"
	// scopeSession is a write that belongs to the calling session rather than to any project:
	// an agent conversation's own artifact, messages and stream frames, the two routes a
	// session answers its own delivery on, and the caller-private `me` rows. There is no
	// project to compare, and each row is held to its caller another way, each one read:
	//
	//   - The rows whose path names a session — the `agents/{session_id}` writes and
	//     `PUT /api/v1/me/agents/{session_id}/state` — are held to it by withinSessionScope,
	//     which refuses a credential that proved another session (project_scope.go). Only a
	//     broker-issued token proves one; `POST /api/v1/agents/{session_id}/messages` and
	//     `POST /api/v2/agents/{session_id}/messages`, the stream publish and the `me` state row
	//     need the `person` scope besides, since a person writing to a session is writing to
	//     someone else's conversation by design, and that scope is what lets their own
	//     credential past the session check (withinSessionScope).
	//   - `POST /api/v1/messages/{id}/deliveries/{attempt}/accept` and
	//     `POST /api/v1/messages/{id}/reply`, and their /api/v2 routes
	//     `PUT /api/v2/messages/{id}/deliveries/{attempt}/state` and
	//     `POST /api/v2/messages/{id}/replies`, take the session from the caller's own credential
	//     or `actor` and refuse an attempt that is not that session's, `403 ACCEPT_FORBIDDEN`
	//     and `403 REPLY_FORBIDDEN`.
	//   - The `me` token rows name no session at all: they are the calling person's, read and
	//     written by a credential holding the `person` scope and keyed on that person.
	scopeSession writeScope = "session"
	// scopeDeployment is a write that belongs to the deployment rather than to any project: a
	// credential that names one project may not create another or change a deployment-wide
	// setting, so a project-scoped caller is refused these by name.
	scopeDeployment writeScope = "deployment"
	// scopeProjectInBody is a write that belongs to one project the request's body names rather
	// than its path: the issue create, whose project comes from the body, and the question of an
	// answer run. There is no object to resolve before the handler runs. So createIssueFrom, the
	// create both versions make, and createGoingRequestFrom each read that project's visibility and
	// ask access.CredentialCovers once they have read that project. Each answers a private project
	// that the credential does not name, and a project that does not exist, with one answer at that
	// one step: createIssueFrom 404 NOT_FOUND, and createGoingRequestFrom 404 PROJECT_NOT_FOUND.
	// withinProjectScope passes the row through by this class rather than by a name it compares.
	scopeProjectInBody writeScope = "project-in-body"
	// scopeNoWrite is a row that writes nothing and reads no row: its answer is computed from
	// its own body alone. There is no object whose project could be resolved and no session the
	// answer belongs to, so the project check passes it through (withinProjectScope) and this
	// class is what says the row was decided rather than forgotten. POST
	// /api/v1/projects/{key}/related writes nothing either and is scopeProject, because it
	// reads the project its path names; this class is for a row that reads nothing at all.
	scopeNoWrite writeScope = "no-write"
)

// idRoute is what the id wildcards of one route's pattern name, which is what the prefix layer
// reads before the scope checks and the handler see those values (resolveIDPrefixes).
//
// Kind is the kind the pattern's {id} names — ask, comment, message or artifact, as
// refs.ResolvePrefix reads a kind — and WithinIssue whether a prefix of it is looked for among
// the items of the issue the pattern's {key} names rather than among every item of that kind.
// Blocks is whether the pattern's {block_id} is a block of the document its {id} names, matched
// among that document's blocks once the {id} itself is resolved.
//
// NotFound is the code of the 404 this route already answers for a whole id of its kind that
// names no item, which is the answer a prefix that names none gets too: a short id resolves
// wherever a whole one does, so it fails where a whole one fails. It is read from the route
// rather than assumed, and the routes disagree — a document's read answers ARTIFACT_NOT_FOUND,
// a comment's thread NOT_FOUND and a comment's delivery COMMENT_NOT_FOUND — so it is written
// per route and not per kind. BlockNotFound is the same for the {block_id}. NoNotFound marks the
// route that answers no 404 at all for an id it does not hold, which names no NotFound: a prefix
// that names nothing is left as written there, and the route answers what it always answers. Only
// DELETE /api/v1/me/asks/{id}/snooze is one: un-snoozing a row nobody snoozed is the state the
// caller asked for, and it answers 204 whatever ask the id names.
//
// Reason is why a route looks up no prefix, written exactly where Kind is empty and Blocks is
// false: an id of a kind refs.ResolvePrefix does not read, so nothing is looked for and the
// handler makes of the value what it always made of it. Such a route is wrapped all the same,
// because its {id} is still rewritten to the canonical spelling where it is a whole uuid
// written another of uuid.Parse's ways, as it is on every route that takes an id
// (canonicalizeUUIDPath).
type idRoute struct {
	Kind          string
	WithinIssue   bool
	Blocks        bool
	NotFound      apicode.Code
	NoNotFound    bool
	BlockNotFound apicode.Code
	Reason        string
}

// resolves reports whether the route looks a prefix up at all, which is what makes Reason
// required. It does not decide the wrapping: register wraps every route that states an ID,
// since one that resolves nothing still has its {id} canonicalized there (resolveIDPrefixes).
func (route idRoute) resolves() bool { return route.Kind != "" || route.Blocks }

// hasIDWildcard reports whether a pattern carries an id the prefix layer could resolve.
func hasIDWildcard(pattern string) bool {
	return strings.Contains(pattern, "{id}") || strings.Contains(pattern, "{block_id}")
}

// undecidedIDRoutes names every route whose ID leaves its id undecided: a route whose pattern
// carries an id wildcard and that states no ID, one that resolves nothing and gives no reason,
// one that names a kind and a reason at once, one that resolves an id or a block without saying
// what 404 that route answers for one it does not hold, one that says it answers no 404 for an id
// it does not hold and names one anyway, one that resolves nothing and names such an answer
// anyway, and one that states an ID for a pattern that carries no id. A route file states its ID
// in its declaration (routeDecl.ID) and a routeRow in its IDs column; both reach the mounted
// route's IDs, so a v2 route whose column is missing refuses to mount (register) where a v1
// route's missing ID does.
func undecidedIDRoutes(routes []apiRoute) []string {
	var undecided []string
	for _, route := range routes {
		name := route.Method + " " + route.Pattern
		entry := route.IDs
		// A route file's declaration states its ID, and a routeRow states it in its IDs column.
		source := "its ID"
		if route.columns {
			source = "its IDs column"
		}
		if !hasIDWildcard(route.Pattern) {
			if entry != nil {
				undecided = append(undecided, name+": "+source+" says what an id names there, and its pattern carries no id")
			}
			continue
		}
		switch {
		case entry == nil && route.columns:
			undecided = append(undecided, name+": its IDs column says neither what its id names nor why no prefix is resolved there")
		case entry == nil:
			undecided = append(undecided, name+": it states no ID, which says neither what its id names nor why no prefix is resolved there")
		case entry.resolves() && entry.Reason != "":
			undecided = append(undecided, name+": "+source+" names a kind to resolve and a reason not to")
		case !entry.resolves() && strings.TrimSpace(entry.Reason) == "":
			undecided = append(undecided, name+": "+source+" resolves no prefix there and gives no reason")
		case entry.Kind != "" && entry.NotFound.IsZero() && !entry.NoNotFound:
			undecided = append(undecided, name+": "+source+" resolves an id there and does not say what 404 the route answers for one it does not hold")
		case entry.NoNotFound && !entry.NotFound.IsZero():
			undecided = append(undecided, name+": "+source+" says the route answers no 404 for an id it does not hold and names one anyway")
		case entry.Blocks && entry.BlockNotFound.IsZero():
			undecided = append(undecided, name+": "+source+" resolves a block id there and does not say what 404 the route answers for one the document does not hold")
		case !entry.resolves() && (!entry.NotFound.IsZero() || !entry.BlockNotFound.IsZero() || entry.NoNotFound):
			undecided = append(undecided, name+": "+source+" resolves nothing there and names a not-found answer anyway")
		}
	}
	sort.Strings(undecided)
	return slices.Compact(undecided)
}

// routeScopeSet is the OAuth scopes one route needs of the credential that calls it, every one
// of which that credential must hold (gateRouteScopes). An empty set is a route any authenticated
// caller may reach.
type routeScopeSet []string

// v2PersonScope and v2SettlesScope are the OAuth scope sets /api/v2 row files share: person for
// routes acting as the caller's person, and person plus approve for routes that settle what a
// person settles.
var (
	v2PersonScope  = routeScopeSet{model.ScopePerson}
	v2SettlesScope = routeScopeSet{model.ScopePerson, model.ScopeApprove}
)

// routeDecl is one route as the file that declares it states it. Every route of this package is a
// file of its own, route_<path>+<method>.go (internal/dispatch/routefile says how a pattern is
// written into the name, and TestEachRouteIsDeclaredInTheFileItsNameNames holds each file to it),
// and that file holds one declaration, `var _ = declareRoute(routeDecl{...})` for /api/v1 or
// `var _ = declareRow(func(s *server) routeRow { ... })` for /api/v2, whose fields are every fact
// Dispatch knows of the route: two changes that add different routes touch no file in common.
// routeDecl is a /api/v1 route as /api/v2 began, each naming the /api/v2 route that answers it
// (Successor): add a route as a routeRow in a row file route_api_v2_<path>+<method>.go holding
// one declareRow, never as a routeDecl file and never as a mux.HandleFunc line (routes()).
type routeDecl struct {
	Method      string
	Pattern     string
	Auth        routeAuth
	Description string
	// Handler is the server's method that serves the route, as a method expression:
	// (*server).getIssue. routes() binds it to the server it builds the table for.
	Handler func(*server, http.ResponseWriter, *http.Request)

	// Successor is the /api/v2 route that answers this /api/v1 route, or that a client moves to
	// where this route is deleted when /api/v1 ends (successorIs, deletedFor): its Link header
	// names it (linkSuccessor), and the map tests hold every /api/v1 route to one. Every route
	// file but a test hook's states one; a test hook's successor is its own /api/v2/_test path
	// (testHookRoutes). A route added here is served at /api/v1 alone: a route added for both
	// versions is a routeRow in a row file route_api_v2_<path>+<method>.go holding one declareRow
	// (routes()).
	Successor successor

	// TestHook is a route mounted only with Deps.TestHooksEnabled: a hook a test arms or reads,
	// which no deployment serves. What each hook does, and whether it acts on every process
	// serving the database or on the one its call lands on, is its own file's to say.
	TestHook bool

	// WriteClass is what a mutating route's write belongs to, which Register's project-scope
	// check reads (project_scope.go). Every route whose method is not GET states one and a GET
	// route states none: a mutating route with no class is one that check cannot decide, which
	// undecidableWrites names, TestEveryMutatingRouteIsClassed fails on and register refuses to
	// mount.
	WriteClass writeScope

	// ID is what the {id} and {block_id} of a route whose pattern carries one name: the kind
	// whose prefix is resolved into it and the 404 that route answers for an id of that kind it
	// does not hold, or the reason it resolves none (idRoute). It is stated per route and not
	// per kind or per pattern, because two methods of one pattern can answer a missing row
	// differently: PUT /api/v1/me/asks/{id}/snooze answers 404 and DELETE answers 204. Every
	// route with an id wildcard states one and no other route does, which undecidedIDRoutes
	// holds each declaration to (TestEveryIDRouteIsNamed), so a route added later cannot
	// silently stop resolving prefixes.
	//
	// The ids this layer never sees are no part of it: an id a request's body carries
	// (reply_to, ask_id, in_reply_to) and the ?artifact= filters are in no path value, and the
	// resolver that rewrites a body's short ids as its text is indexed is their one reader
	// (refs.Reader).
	ID *idRoute

	// OAuthScopes is the set the route needs of the credential that calls it, which
	// gateRouteScopes enforces; GET /api/v1 and `envoy-dispatch routes` list it.
	//
	// The routes that name any are the routes Dispatch refused every bearer token on until
	// INFRA-1017, and two scopes decide them now: `person`, "this credential acts as its
	// person", which every one of them needs, and `approve`, the right to settle what a person
	// settles, which the routes that decide a credential, a sign-in, a review, a decision's
	// Keep or a token need as well. A person's browser holds both and `p0` besides, which no
	// route needs: the one value of a field that needs it, a priority of 0 on an issue write, is
	// checked in the write itself (refuseP0WithoutScope), since a route's declaration names a
	// route and not what a request writes. Their personal tokens hold the set each token's row
	// names. A broker-issued token that acts for its person holds `person` alone
	// (brokerTokenActor), except an approve-only token, which holds `person` and `approve` and
	// calls only the routes that state ApproveOnly; no broker-issued token holds `p0`. The shared
	// token, a pod's service-account token, a project-scoped session's token and a broker-issued
	// token that names a service and no person hold none of them.
	//
	// The scope `read` a read-only token holds is never one of these: a scope here is one the
	// caller must hold, and no person holds that one. The routes such a credential may call are
	// the GET routes whose declarations class them as reads (Read), which its own gate reads
	// (refuseReadOnlyToken).
	//
	// A route that takes an id written short needs no scope of its own for it: the prefix layer
	// (resolveIDPrefixes, ID) runs inside this gate - register wraps the gate around it, so a
	// credential that holds none of the route's scopes is refused before any id is read, and
	// the one check a request meets ahead of the route's scopes is whether a read-only token may
	// call the route at all (refuseReadOnlyToken) - and it only rewrites the path value, so a
	// short id reaches exactly the scopes the whole id reaches, and a route that needs none with
	// a whole id needs none with a prefix.
	OAuthScopes routeScopeSet

	// Read is the read class of a GET route: what it writes when it serves a request, and the
	// note that says so (readEntry, read_classes.go). A route a read-only credential may call is
	// one whose class is readWritesNothing; every other route, and GET /ws/live beside them,
	// answers such a caller 403 READ_ONLY_TOKEN before anything else the route does
	// (refuseReadOnlyToken), so a route added later is closed to it until someone has read the
	// handler and classed it here.
	//
	// Every GET route states a class, and no other route states one: a write is not a read
	// whatever its handler does, and register refuses to mount a table that breaks either rule
	// (readClassRefusals). The class is not a scope, so the route index (GET /api/v1), the
	// OpenAPI document and the reference generated from it do not carry it, and only the
	// read-only gate reads it.
	Read readEntry

	// ApproveOnly is a route an approve-only token may call: the token the secrets broker mints
	// for a person's approve grant, binds to its own pod's key and spends through its proxy, never
	// released (model.Credential.ApproveOnly). It acts as its person with the scopes person and
	// approve (brokerApproveOnlyScopes), and every route that does not state ApproveOnly, and GET
	// /ws/live beside them, answers it 403 APPROVE_ONLY_TOKEN before anything else the route does
	// (refuseApproveOnlyToken), so a route added later stays closed to it until its file states
	// it.
	//
	// The routes that state it are the routes whose OAuthScopes name approve, the routes that
	// settle what a person settles, less two: the mint and the change of a personal token, since a
	// token they made or widened would be an approve credential written to the box the session
	// runs on, which is what the proxy exists to keep off it. The revoke of a personal token
	// states it. Beside them is the answer of an ask, which checks approve in its handler for an
	// approval request and refuses an approve-only token the answer of every other ask, 403
	// APPROVE_ONLY_TOKEN, once the ask's kind is read (ask_transitions.go). Like Read, it is no
	// scope: the route index and the OpenAPI document do not carry it, and only the approve-only
	// gate reads it.
	ApproveOnly bool
	// ApproveState is, on a route that states ApproveOnly and whose /api/v2 successor takes
	// several operations by the body's `state`, the one state of that successor that answers
	// this route, which is the state an approve-only token may send the successor
	// (applyApprovePolicy, gateApproveOnlyStates); empty on every other route.
	ApproveState string
	// ApproveApprovalAsks is, on the answer of an ask, that an approve-only token may answer an
	// approval request alone, on both versions (gateApproveOnlyAskAnswer).
	ApproveApprovalAsks bool

	// Shape is what the route takes and answers, in the Go types the OpenAPI document is
	// reflected out of (routeShape, routes_shapes.go). Every route states one, which
	// shapelessRoutes holds the table to; the OpenAPI emitter is what reads it.
	Shape routeShape
}

// routeRegistry is every route a route file declares, in the order the files were initialized.
// Nothing appends to it once the package is initialized: routes() and declaredRoute read it.
var routeRegistry []routeDecl

// declareRoute adds one route to routeRegistry and answers it. Each route file calls it once,
// from a package-level declaration, so the registry is whole before any function of the package
// runs: Go initializes every package-level variable before main or a test starts, in the order
// their dependencies decide, which lets a description call fmt.Sprintf over a constant or a
// variable another file declares.
func declareRoute(route routeDecl) routeDecl {
	routeRegistry = append(routeRegistry, route)
	return route
}

// rowRegistry is every routeRow a /api/v2 row file declares, in the order the files were
// initialized. Nothing appends to it once the package is initialized: routes() reads it.
var rowRegistry []func(*server) routeRow

// declareRow adds one /api/v2 routeRow constructor to rowRegistry and returns true. Each /api/v2
// row file calls it once, from a package-level declaration, so the registry is whole before any
// function of the package runs, in the order Go's package initialization decides; routes() builds
// each row with s and sorts them by pattern and then method.
func declareRow(row func(*server) routeRow) bool {
	rowRegistry = append(rowRegistry, row)
	return true
}

// declaredRoute is the declaration of one route, by "<method> <pattern>", test hooks included,
// and false where no route file declares it: a routeRow that shares a core with a /api/v1 route
// copies a fact of that route's declaration through it (declaredRead).
func declaredRoute(name string) (routeDecl, bool) {
	for _, route := range routeRegistry {
		if route.Method+" "+route.Pattern == name {
			return route, true
		}
	}
	return routeDecl{}, false
}

// readClassRefusals names, sorted, every way the table's declarations fail to class their reads,
// each line naming its route: a GET route stating no class, one whose Writes is no class at all, a
// GET that writes (readWritesState) stating no WriteNote to name what it writes, and a route that
// is not a GET stating a class, which would open a write to a credential minted only to read.
// register refuses to mount a table this names anything for, so a GET route added later is served
// to no read-only credential until someone has read its handler and classed it.
//
// A route file's class is a field of its own declaration, so it cannot name a route that is not
// there. A routeRow's class is an entry of v2ReadClasses, keyed by the row's /api/v2 pattern, and an
// entry that classes no GET routeRow the table mounts is named too (v2ReadClassesUnmounted).
func readClassRefusals(routes []apiRoute) []string {
	var refused []string
	for _, route := range routes {
		name, entry := route.Method+" "+route.Pattern, route.Read
		switch {
		case route.Method != http.MethodGet:
			if entry.Writes != readWritesUnset {
				refused = append(refused, name+": a route that is not a GET states a read class, and a write is no read")
			}
		case entry.Writes == readWritesUnset:
			refused = append(refused, name+": a GET route stating no read class")
		case entry.Writes != readWritesNothing && entry.Writes != readWritesState:
			refused = append(refused, name+": its read class is "+strconv.Itoa(int(entry.Writes))+", which is neither readWritesNothing nor readWritesState")
		case entry.Writes == readWritesState && strings.TrimSpace(entry.WriteNote) == "":
			refused = append(refused, name+": a GET that writes (readWritesState) stating no WriteNote naming what it writes")
		}
	}
	refused = append(refused, v2ReadClassesUnmounted(routes)...)
	sort.Strings(refused)
	return refused
}

// routeIndexEntry is the public shape of a route in GET /api/v1. Scopes is left out of a route
// that needs none, so the index describes each route exactly as the table declares it.
type routeIndexEntry struct {
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Auth        routeAuth `json:"auth"`
	Scopes      []string  `json:"scopes,omitempty"`
	Description string    `json:"description"`
}

// routeIndexDocs names the agent-facing guide for the routes below.
const routeIndexDocs = "skills/dispatch/SKILL.md"

// searchDescription is the prose of both search rows, which serve one handler and give one
// answer. It is one definition because a description is no comment: openapi.go emits it as the
// operation's description, so a sentence left behind on one row reaches the committed document
// and every generated client. The rows differ in one paragraph alone, `declares`, which says
// which of the ten members that route declares and why there are two routes.
func searchDescription(declares string) string {
	return fmt.Sprintf("Full-text search ?q= across issues, documents, asks, comments, and messages, within ?project= when given: every match ranked on one list across the kinds by one score, the keyword leg's length-normalised rank and the meaning leg's cosine weighted onto one scale and summed, one page of ?limit= (1-50, default 20) from ?offset=, with the total match count and how many the pages can reach (each leg's best %d). A comment's, a message's and an ask's hit carries author, the actor that wrote it. ?kind= names one of issue, document, comment, ask and message and is repeated for each kind wanted: those kinds alone are ranked, counted and paged, and with no kind named every kind is. A q that is one issue key in any form a person writes it - CORE-1042, core-1042, CORE 1042, or the number alone, which names that issue of the project the number is read in, or of every project at once, all of them then heading the list in project order - puts that issue first. The number is read in ?project= when the search is narrowed to one, and otherwise in ?number_project=, the project of the page the search was made from: that parameter narrows nothing at all, so a search carrying it answers every project's rows, and a request naming both reads the number in the ?project= it filters by. A q is searched as typed, websearch_to_tsquery's AND of its words, whatever its length; only a q that matches nothing at all is searched a second time for its %d rarest words ORed, rarest by the document frequency the project's own term statistics hold. The \"phrase\", OR and -word forms are read as always, and the answer's query {operators, broadened} names the forms it read and whether that second search is what answered. ?status=, ?author= and ?updated_since= narrow every leg of the search beside ?project=: the status of the issue a row belongs to (a row with no issue, such as a project document, matches none), the actor id that wrote the item a row names (a person's login or a session id; an issue's and a document's is their creator), and the RFC3339 time at or after which that item was last written. The answer's filters echoes those four and never ?number_project=, which narrowed nothing. ?keywords_only=true runs the keyword leg alone and makes no embedding call, whatever embedder the deployment configured; its answer is not stamped degraded, because nothing is unavailable - the caller asked for the words alone. The command bar sends it on every keystroke, so a person typing waits on no model call and no reader of this route should take the bar's type-ahead for the hybrid path. A request leaving it out searches as it always did, by meaning too wherever an embedder is configured. %s A q under 2 characters and a kind outside those five are 400 INVALID_QUERY, a q over %d characters is 400 CAP_EXCEEDED, a project or number_project that is not a project key 400 INVALID_PROJECT or INVALID_NUMBER_PROJECT, a repeated, blank or unreadable status, author or updated_since 400 INVALID_STATUS, INVALID_AUTHOR or INVALID_UPDATED_SINCE, and a keywords_only that is neither true nor false 400 INVALID_KEYWORDS_ONLY.",
		contracts.SearchKindDepth, searchBroadenWords, declares, contracts.SearchQueryMax)
}

// searchDeclaresFour is the paragraph of GET /api/v1/search: six of the ten members this
// handler reads are declared elsewhere, and a client sends them elsewhere.
const searchDeclaresFour = "This route declares ?q=, ?project=, ?limit= and ?offset= alone, and GET /api/v1/search/results declares all ten: a client sends a search carrying ?number_project=, ?status=, ?author=, ?updated_since= or ?keywords_only= there, because a build released before those five answers this route 200 with the member dropped, which is unnarrowed rows under the filters a person chose, where it answers that path 404 no route. ?kind= is younger than that path, so a build serving it but released before ?kind= answers either route 200 with ?kind= dropped, which is every kind's rows where one kind's were asked for: sending ?kind= there buys no refusal, and a client that must know the rows are narrowed reads each result's own kind. This route still reads all ten, so a page served by an earlier release that sends one of the six here is still answered narrowed."

// searchDeclaresTen is the paragraph of GET /api/v1/search/results, the route that declares
// every member the handler reads.
const searchDeclaresTen = "This route declares all ten members and answers exactly what GET /api/v1/search answers, from the same handler: a client sends a search carrying ?number_project=, ?status=, ?author=, ?updated_since= or ?keywords_only= here, because a build released before those five answers this path 404 no route, where it answers GET /api/v1/search 200 with the member dropped, which is unnarrowed rows under the filters a person chose. ?kind= is younger than this path, so a build serving it but released before ?kind= answers both routes 200 with ?kind= dropped, which is every kind's rows where one kind's were asked for; a client that must know the rows are narrowed reads each result's own kind. GET /api/v1/search still reads all ten, so a page served by an earlier release that sends one of the six there is still answered narrowed."

// commentPartiesDescription is the paragraph both comment-create rows carry: who a comment
// delivers to is part of its contract, and a mention is a reference, so the body itself names
// the parties. It is one definition for the reason searchDescription is: the
// emitter writes a description into the committed OpenAPI document and every generated client,
// where a sentence left behind on one row would stand as the other route's contract.
const commentPartiesDescription = "A party address the body writes names that party, as an entry of mentions does. dispatch://agent/<session id> names a session and dispatch://role/<token> names a role, and the comment is delivered to each in the mode delivery names - btw, aside or steer - and in steer when the request names none. The same address inside a fenced code block, an indented code block or a code span names nobody. A person address, dispatch://person/<login>, names a reader of the record, and nothing is delivered to a person. The array and the body make one party set: over 10 delivering parties in it is 400 MENTION_INPUT. A target of a form no delivery route takes is also 400 MENTION_INPUT, whether the array or the body names it. For example, a session ID that contains a dot or a colon cannot be used as a delivery target."

// unsettledText is what every read of a document's text says of unsettled (versionOfText): the
// three text rows of /api/v1 and the include=text of /api/v2's document reads
// (artifactIncludeText). A description is no comment, so the sentence is written once.
const unsettledText = "unsettled is true when the markdown answered equals no stored version's markdown: the " +
	"document holds edits no version holds yet, or its newest version was rendered by an older build; version is " +
	"then the newest version, not the version of this text"

// goingCatalogueDescription is the description GET /api/v1/going/catalogue serves. Both counts
// in it are read from what this build holds - the specs contracts carries, and the tools the
// catalogue names a row of - rather than written as words: a number written as prose has no
// test and no merge conflict to protect it, and this one was falsified twice, once by this
// change's own four specs and once by a merge that added a tool (INFRA-1523).
func goingCatalogueDescription() string {
	return fmt.Sprintf("The catalogue of operations an agent may propose on Dispatch's graph: %d of the %d tool specs agents operate Dispatch with, every one that writes but dispatch_review and dispatch_project_update, each carried whole - its own name, its own description, the arguments it takes with an example call, the rows of this table it writes, and the graph relations its verbs write. It leaves the two out on purpose: a review records an approval or a changes request in a person's name, dispatch_project_update turns a check of a project on or off, and the loop that proposes operations must never propose either. The catalogue is a list read whole, so the route takes no query and serves no page. An action the catalogue does not hold is no action at all: nothing serves it.",
		len(goingToolRoutes), len(contracts.DispatchTools))
}

// replyQuoteDescription is what each of the six rows that write a reply to a block says of the
// words of that block the reply quotes (replyQuoteRefusal, namedBlock.setQuote): field is the
// row's block field and code the code its route gives a bad one. It is one definition, as
// searchDescription is, so that the six rows state one rule.
func replyQuoteDescription(field, code string) string {
	return fmt.Sprintf("Beside %[1]s, quote and occurrence name the words of that block the reply quotes: quote is a run of characters of the block's plain text, its text runs joined with no markdown marks and no heading marker, and occurrence is which match of quote in that one block it is, counted from 0 with overlapping matches counted (0 where none is sent and the block holds quote once); the reply stores both. An occurrence without quote, a quote without %[1]s, an empty quote and an occurrence below 0 are each 400 %[2]s, naming the field. A quote the block's text does not hold, or an occurrence past its last match, is 404 TARGET_NOT_FOUND, and a quote it holds more than once with no occurrence is 409 TARGET_AMBIGUOUS.",
		field, code)
}

// threadPageDescription is what each of the three issue thread lists says of its page
// (parseThreadPage, selectThreads): entries is the route's own sentence on what its thread and
// its row are, and filters its own sentences on ?person= and the keys only it takes. It is one
// definition, as replyQuoteDescription is, so that the three rows state one rule.
func threadPageDescription(entries, filters string) string {
	return fmt.Sprintf("?order=activity answers one page of the issue's threads in place of the list, {threads, before, remaining}: the thread with the newest last activity first, and of two with the same last activity the one with the larger id first. %[3]s A row's last_activity_at is the newest time an entry of the thread was written, cut to whole microseconds and written in UTC with six fractional digits; a deleted entry counts and an edit does not. ?limit= is how many threads a page holds at most, an integer from 1 to %[1]d, and %[2]d without it. before is the position of the page's last thread, <last_activity_at>,<id>, and null when no thread follows; ?before= with that value answers the threads that follow the position, in the same order, and a thread written in during a walk moves above it. remaining is the exact number of threads that follow the page under the same filters, counted in the same read, and is 0 only when before is null. ?author= keeps the threads that hold an entry that actor wrote: a person's login, matched with no regard to case, or a session id, matched exactly; remaining counts under it. %[4]s Without ?order= the route answers its list. Each refusal is 400 INVALID_QUERY, names the parameter and the values it takes, and answers no thread: ?limit=, ?before=, ?author= or ?person= without ?order=activity, an order other than activity, a limit that is not an integer from 1 to %[1]d, a before that is not a position this API wrote, an empty author, a person value the route does not take, and any of order, limit, before, author and person given twice.",
		contracts.MaxThreadPageLimit, contracts.DefaultThreadPageLimit, entries, filters)
}

// routes is every route the server serves, one apiRoute per (version, route): each route file's
// declaration at its /api/v1 pattern (v1Route), each routeRow of rowRegistry at /api/v2 and, unless
// it says otherwise, at /api/v1 (mountRow), and with Deps.TestHooksEnabled each test hook's
// declaration at its two paths (testHookRoutes). Both declarations and rows are walked by pattern
// and then method (byPatternThenMethod, byRowPatternThenMethod), so every reader walks one table in
// one order, whichever order the route files were initialized in. Add a route as a routeRow in a
// row file route_api_v2_<path>+<method>.go holding one declareRow, never as a routeDecl file and
// never as a mux.HandleFunc line: /api/v1 alone serves a routeDecl file, and
// TestAV1OnlyRowOutsideTheFrozenListFails refuses one testdata/v1-only-routes.txt does not hold. A
// routeRow served in both versions is decoded and gated at both paths, its Query, its Body and its
// Authorities included (apiRoute.decoded); one that answers for /api/v1 routes which keep their own
// answer at /api/v1 is served at /api/v2 alone (Versions: onlyV2). register mounts what this
// answers and refuses a table whose facts it cannot decide (undecidableWrites, shapelessRoutes,
// undecidedIDRoutes, undecodableRoutes, readClassRefusals). It refuses an approve list that does
// not map onto the table itself (applyApprovePolicy), by panic as it builds the table, with each
// fault, which names its kind and its route, so no reader of the table meets one.
func (s *server) routes() []apiRoute {
	replays := s.keyedReplays()
	declared := slices.SortedFunc(slices.Values(routeRegistry), byPatternThenMethod)
	var routes []apiRoute
	for _, declaration := range declared {
		switch {
		case !declaration.TestHook:
			routes = append(routes, v1Route(s, declaration, replays))
		case s.deps.TestHooksEnabled:
			routes = append(routes, testHookRoutes(s, declaration)...)
		}
	}
	rows := make([]routeRow, 0, len(rowRegistry))
	for _, build := range rowRegistry {
		rows = append(rows, build(s))
	}
	slices.SortFunc(rows, byRowPatternThenMethod)
	for _, row := range rows {
		routes = append(routes, mountRow(row)...)
	}
	stampV2ReadClasses(routes)
	// The approve-only token's admission is compiled here, once, and the routes carry the result
	// every layer that reads it uses: the three checks and the refusal's wording (approve_policy.go).
	if faults := applyApprovePolicy(routes, approveList(declared)); len(faults) > 0 {
		panic("api: the route table's approve list is an invalid mapping: " + strings.Join(faults, "; "))
	}
	return routes
}

// byPatternThenMethod is the one order every reader of the declarations walks them in: by pattern,
// and by method where one pattern has more than one. routes() sorts what it mounts with it, and so
// does the approve-only refusal, whose message promises the order routes() mounts them in
// (approveOnlyRefusal).
func byPatternThenMethod(left, right routeDecl) int {
	return cmp.Or(strings.Compare(left.Pattern, right.Pattern), strings.Compare(left.Method, right.Method))
}

// byRowPatternThenMethod is the same order for routeRow values, so routes() mounts them in one
// order whichever order the row files were initialized in.
func byRowPatternThenMethod(left, right routeRow) int {
	return cmp.Or(strings.Compare(left.Pattern, right.Pattern), strings.Compare(left.Method, right.Method))
}

// bound is handler, a method of the server, served by s.
func (s *server) bound(handler func(*server, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { handler(s, w, r) }
}

// routeIndexEntries renders the table for GET /api/v1, sorted by path then method.
func routeIndexEntries(routes []apiRoute) []routeIndexEntry {
	entries := make([]routeIndexEntry, len(routes))
	for index, route := range routes {
		entries[index] = routeIndexEntry{
			Method:      route.Method,
			Path:        route.Pattern,
			Auth:        route.Auth,
			Scopes:      route.OAuthScopes,
			Description: route.Description,
		}
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Path != entries[right].Path {
			return entries[left].Path < entries[right].Path
		}
		return entries[left].Method < entries[right].Method
	})
	return entries
}

// routeIndex is the body GET /api/v1 answers.
type routeIndex struct {
	Docs   string            `json:"docs"`
	Routes []routeIndexEntry `json:"routes"`
}

// routeIndexBody wraps the rendered rows in that body, which is the one place the agent-facing
// guide is named on an answer.
func routeIndexBody(entries []routeIndexEntry) routeIndex {
	return routeIndex{Docs: routeIndexDocs, Routes: entries}
}

func (s *server) getRouteIndex(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, routeIndexBody(s.routeIndex))
}

// WriteRouteIndex writes the body the index of one version of the API answers on a server without
// test hooks, read from the table alone: no database, no listener. version 1 is GET /api/v1's,
// version 2 GET /api/v2's, and any other is refused. `envoy-dispatch routes` prints it, and the
// docs site's API reference is generated from that output.
func WriteRouteIndex(w io.Writer, version int) error {
	routes := (&server{}).routes()
	switch apiVersion(version) {
	case apiV1:
		return json.NewEncoder(w).Encode(routeIndexBody(routeIndexEntries(servedAt(apiV1, routes))))
	case apiV2:
		return json.NewEncoder(w).Encode(routeIndexV2Body(routeIndexV2Entries(servedAt(apiV2, routes)), pageQuery{}))
	}
	return fmt.Errorf("the API has no version %d: its versions are 1 and 2", version)
}
