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
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/admission"
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
	// message rows that carry an issue. A caller whose credential names another project is
	// refused, naming both projects.
	scopeProject writeScope = "project"
	// scopeSession is a write that belongs to the calling session rather than to any project:
	// an agent conversation's own artifact, messages and stream frames, the two routes a
	// session answers its own delivery on, and the caller-private `me` rows. There is no
	// project to compare, and each row is held to its caller another way, each one read:
	//
	//   - The rows whose path names a session — the `agents/{session_id}` writes and
	//     `PUT /api/v1/me/agents/{session_id}/state` — are held to it by withinSessionScope,
	//     which refuses a credential that proved another session (project_scope.go). Only a
	//     broker-issued token proves one; `POST /api/v1/agents/{session_id}/messages`, the
	//     stream publish and the `me` state row need the `person` scope besides, since a person
	//     writing to a session is writing to someone else's conversation by design, and that
	//     scope is what lets their own credential past the session check (withinSessionScope).
	//   - `POST /api/v1/messages/{id}/deliveries/{attempt}/accept` and
	//     `POST /api/v1/messages/{id}/reply` take the session from the caller's own credential
	//     or `actor` and refuse an attempt that is not that session's, `403 ACCEPT_FORBIDDEN`
	//     and `403 REPLY_FORBIDDEN`.
	//   - The `me` token rows name no session at all: they are the calling person's, read and
	//     written by a credential holding the `person` scope and keyed on that person.
	scopeSession writeScope = "session"
	// scopeDeployment is a write that belongs to the deployment rather than to any project: a
	// credential that may write one project may not create another or change a deployment-wide
	// setting, so a project-scoped caller is refused these by name.
	scopeDeployment writeScope = "deployment"
	// scopeProjectInBody is a write that belongs to one project the request's body names rather
	// than its path: the issue create, whose project comes from the body. There is no object to
	// resolve before the handler runs, so createIssue makes the check itself once it has read
	// that project (refuseWriteOutsideProjectScope), and withinProjectScope passes the row
	// through by this class rather than by a name it compares.
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
// per route and not per kind. BlockNotFound is the same for the {block_id}. notFoundNone is the
// route that answers no 404 at all for an id it does not hold, which then keeps that answer.
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
	NotFound      string
	BlockNotFound string
	Reason        string
}

// notFoundNone is the NotFound of a route that answers no 404 for an id it does not hold, so a
// prefix that names nothing is left as written and that route answers what it always answers.
// Only DELETE /api/v1/me/asks/{id}/snooze is one: un-snoozing a row nobody snoozed is the state
// the caller asked for, and it answers 204 whatever ask the id names.
const notFoundNone = "none"

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
// what 404 that route answers for one it does not hold, one that resolves nothing and names such
// an answer anyway, and one that states an ID for a pattern that carries no id.
func undecidedIDRoutes(routes []apiRoute) []string {
	var undecided []string
	for _, route := range routes {
		name := route.Method + " " + route.Pattern
		entry := route.ID
		if !hasIDWildcard(route.Pattern) {
			if entry != nil {
				undecided = append(undecided, name+": its ID says what an id names there, and its pattern carries no id")
			}
			continue
		}
		switch {
		case entry == nil:
			undecided = append(undecided, name+": its declaration states no ID, which says neither what its id names nor why no prefix is resolved there")
		case entry.resolves() && entry.Reason != "":
			undecided = append(undecided, name+": its ID names a kind to resolve and a reason not to")
		case !entry.resolves() && strings.TrimSpace(entry.Reason) == "":
			undecided = append(undecided, name+": its ID resolves no prefix there and gives no reason")
		case entry.Kind != "" && entry.NotFound == "":
			undecided = append(undecided, name+": its ID resolves an id there and does not say what 404 the route answers for one it does not hold")
		case entry.Blocks && entry.BlockNotFound == "":
			undecided = append(undecided, name+": its ID resolves a block id there and does not say what 404 the route answers for one the document does not hold")
		case !entry.resolves() && (entry.NotFound != "" || entry.BlockNotFound != ""):
			undecided = append(undecided, name+": its ID resolves nothing there and names a not-found answer anyway")
		}
	}
	sort.Strings(undecided)
	return slices.Compact(undecided)
}

// routeScopeSet is the OAuth scopes one route needs of the credential that calls it, every one
// of which that credential must hold (gateRouteScopes). An empty set is a route any authenticated
// caller may reach.
type routeScopeSet []string

// keyedRefusal answers a refusal the keyed layer decides itself in a route's own shape
// (keyedRoute.refuse): err, with recovery the point the key held when the layer decided it.
type keyedRefusal = func(w http.ResponseWriter, r *http.Request, err error, recovery json.RawMessage)

// routeDecl is one route as the file that declares it states it. Every route of this package is a
// file of its own, route_<path>+<method>.go (internal/dispatch/routefile says how a pattern is
// written into the name, and TestEachRouteIsDeclaredInTheFileItsNameNames holds each file to it),
// and that file holds one declaration, `var _ = declareRoute(routeDecl{...})`, whose fields are
// every fact Dispatch knows of the route: two changes that add different routes touch no file in
// common. Add a route by adding such a file, never a mux.HandleFunc line: routes() mounts what is
// declared, GET /api/v1 lists it, and the OpenAPI document is emitted from it.
type routeDecl struct {
	Method      string
	Pattern     string
	Auth        routeAuth
	Description string
	// Handler is the server's method that serves the route, as a method expression:
	// (*server).getIssue. routes() binds it to the server it builds the table for.
	Handler func(*server, http.ResponseWriter, *http.Request)

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
	// /ws/live beside them,
	// answers it 403 APPROVE_ONLY_TOKEN before anything else the route does
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

	// Download marks a file version download: the read whose deadline bounds the work it does
	// before its answer rather than the whole of it (downloadDeadline, or the test's
	// Deps.DownloadDeadline), stopping at the answer's first byte, after which the answer's
	// transfer bound alone decides how long the bytes may take (servedAsRead,
	// admission.WithDeadlineUntilAnswer). A download pins no connection either (pinsConnection).
	// Only a GET route declares it.
	Download bool
	// Streams marks a read whose answer is a stream its reader holds open long past the work
	// behind it: the going run's server-sent events, which a reader keeps for the run's whole
	// window while the frames the run stores reach it (streamGoingAnswer). Such a route
	// takes no admission unit and pins no connection, since a reader parked on a stream for
	// half a minute would hold a unit of the read class and a request-pool connection for all
	// of it, where the polls that fill it take a connection only while each read runs; the
	// deadline bounds the reads before its first frame alone, as a Download's does
	// (servedAsRead), and the run behind it decides the rest. Its answer goes out under the
	// transfer bounds every answer has, so a reader that stops reading is still cut.
	Streams bool
	// Weight is how many of its class's admission units a request takes (admission.Limiter): the
	// most request-pool connections it holds at once. Zero is one; concurrentReadWeight and
	// questionReadWeight are the two the routes that run reads side by side declare.
	Weight int
	// Lane is the heavy read lane a request of the route takes before it asks its class for
	// units (admission.Lane): a limit of its own for one route whose reads cost the
	// database far more than the rest. admission.NoLane for every other route.
	Lane admission.Lane
	// BodyLimit is the most bytes the route reads of a request body, which is also how much the
	// keyed layer spools, how much of an early-answered request's body the server drains before
	// it answers, and which 413 a body past it is answered with (bodyLimit, drainBeforeAnswer,
	// bodyTooLarge). Zero is maxJSONRequestBytes, every JSON route's limit.
	BodyLimit int64
	// Replay answers a repeat of this route's keyed write whose first run committed a recovery
	// point (keyedReplay): nil for a route that commits none, which is every route but the writes
	// that also deliver to sessions. The function binds the replay to the server that mounts the
	// route (routes), as Handler, a method expression, is bound there:
	// `func(s *server) keyedReplay { return replayRecovered(s, s.replayComment) }`.
	Replay func(s *server) keyedReplay
	// Resumes is whether a repeat of this route's keyed write whose holder is gone takes the key
	// over and runs the steps its recovery point does not hold (keyedRoute.resumes): true
	// for the status change alone, a write made of steps that each commit.
	Resumes bool
	// Refuse answers a refusal the keyed layer decides itself in this route's own shape
	// (keyedRoute.refuse): nil for every route but the status change, whose refusals carry the
	// entries of the steps that committed. Bound to the server that mounts the route in routes(),
	// as Replay is.
	Refuse func(s *server) keyedRefusal
	// RefusesClosedIssue marks the upload whose issue must still be open
	// (refuseUploadToAClosedIssue).
	RefusesClosedIssue bool

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

// readClassRefusals names, sorted, every way the table's declarations fail to class their reads,
// each line naming its route: a GET route stating no class, one whose Writes is no class at all, a
// GET that writes (readWritesState) stating no WriteNote to name what it writes, and a route that
// is not a GET stating a class, which would open a write to a credential minted only to read.
// register refuses to mount a table this names anything for, so a GET route added later is served
// to no read-only credential until someone has read its handler and classed it.
//
// There is no "a class naming no route" case, which the map beside the table needed: a class is a
// field of the route's own declaration, so it cannot name a route that is not there.
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
	sort.Strings(refused)
	return refused
}

// apiRoute is one mounted route: its declaration, read through it by promotion, with its handler
// bound to the server that mounts it. Register mounts the table and GET /api/v1 lists it, so the
// two can never disagree about what the server serves. Handler is the bound handler, and it
// shadows the declaration's own, which is a method expression of no server yet.
type apiRoute struct {
	routeDecl
	Handler http.HandlerFunc
	// Replay and Refuse are the declaration's, bound to the server (routes): they shadow the
	// declaration's own, which are functions of no server yet, as Handler shadows its method
	// expression. What the keyed layer reads (keyedRoute) is these.
	Replay keyedReplay
	Refuse keyedRefusal
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

// goingCatalogueDescription is the description GET /api/v1/going/catalogue serves. Both counts
// in it are read from what this build holds - the specs contracts carries, and the tools the
// catalogue names a row of - rather than written as words: a number written as prose has no
// test and no merge conflict to protect it, and this one was falsified twice, once by this
// change's own four specs and once by a merge that added a tool (INFRA-1523).
func goingCatalogueDescription() string {
	return fmt.Sprintf("The catalogue of operations an agent may propose on Dispatch's graph: %d of the %d tool specs agents operate Dispatch with, every one that writes but dispatch_review and dispatch_project_update, each carried whole - its own name, its own description, the arguments it takes with an example call, the rows of this table it writes, and the graph relations its verbs write. It leaves the two out on purpose: a review records an approval or a changes request in a person's name, dispatch_project_update turns a check of a project on or off, and the loop that proposes operations must never propose either. The catalogue is a list read whole, so the route takes no query and serves no page. An action the catalogue does not hold is no action at all: nothing serves it.",
		len(goingToolRoutes), len(contracts.DispatchTools))
}

// routes is every declared route this server mounts, each handler bound to s: every declaration
// but the test hooks', which are mounted only where Deps.TestHooksEnabled is set. They come
// sorted by pattern and then method. No reader depends on the order - Register mounts each by its
// own pattern, and GET /api/v1, `envoy-dispatch routes` and the OpenAPI document each sort what
// they write - so it is fixed only so that every reader walks one table in one order, whichever
// order the route files were initialized in.
func (s *server) routes() []apiRoute {
	routes := make([]apiRoute, 0, len(routeRegistry))
	for _, route := range routeRegistry {
		if route.TestHook && !s.deps.TestHooksEnabled {
			continue
		}
		mounted := apiRoute{routeDecl: route, Handler: s.bound(route.Handler)}
		if route.Replay != nil {
			mounted.Replay = route.Replay(s)
		}
		if route.Refuse != nil {
			mounted.Refuse = route.Refuse(s)
		}
		routes = append(routes, mounted)
	}
	slices.SortFunc(routes, func(left, right apiRoute) int {
		return cmp.Or(strings.Compare(left.Pattern, right.Pattern), strings.Compare(left.Method, right.Method))
	})
	return routes
}

// bound is handler, a method of the server, served by s.
func (s *server) bound(handler func(*server, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { handler(s, w, r) }
}

// routeIndexEntries renders the table for GET /api/v1. routes() answers the table sorted by
// pattern then method, which is the order the index is read in, so nothing is sorted again here.
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

// WriteRouteIndex writes the body GET /api/v1 answers on a server without test hooks, read
// from the declarations alone: no database, no listener. `envoy-dispatch routes` prints it, and
// the docs site's API reference is generated from that output.
func WriteRouteIndex(w io.Writer) error {
	return json.NewEncoder(w).Encode(routeIndexBody(routeIndexEntries((&server{}).routes())))
}

// pinsConnection reports whether a request of the route runs every request-pool statement on one
// connection, the one its first statement takes, held until its handler returns
// (store.PinConnection): an issue read ran twelve statements, a page of its comments four, and
// each waited for a connection of its own. Every admitted read pins but three kinds:
//   - a read of weight two (concurrentReadWeight), whose reads run side by side, each on a
//     connection of its own (store.ForConcurrentRead), which one pinned connection cannot serve;
//   - a file version download (Download), which moves the file's bytes from the file store for
//     minutes after its last statement, holding no connection meanwhile;
//   - a stream (Streams), which its reader holds open for the run behind it, polling for the
//     frames that run stores, and which is not admitted at all.
//
// A write never pins: it waits for its documents' writer gates between statements, which a caller
// holding a connection is refused (docs.ErrGateWhileHoldingConnection).
func (r apiRoute) pinsConnection() bool {
	return admission.Reads(r.Method) && !r.TestHook && !r.Streams && r.Weight <= 1 && !r.Download
}

// downloadDeadline is the deadline of the four file version downloads: five minutes for the work
// a download does before its answer begins - its row's read and the file store's open, which the
// store bounds at files.OpenTimeout of its own - where a read of the database gets
// defaultReadDeadline. It bounds none of the transfer that follows: from the answer's first byte
// the file's bytes move at the reader's pace, which the answer's own bound measures (no silence
// longer than the idle bound, and the floor after the grace), so a file up to
// files.MaxObjectSize reaches a slow but steady reader however long that takes.
const downloadDeadline = 5 * time.Minute

// concurrentReadWeight is the weight of a route that runs its reads side by side, at most two at
// once (readDeliveryRows, readPipelinePopulation, and the two stages of a related-item check
// whose write is no question, which runs no cross-project leg): two request-pool connections.
const concurrentReadWeight = 2

// questionReadWeight is the weight of a route whose related-item check is a question's, which
// runs a third read beside the two: the answered questions of the other projects
// (relatedOtherProjects, taken only where relatedInput.question). Three request-pool
// connections.
const questionReadWeight = 3

// bodyLimit is the most bytes a request of the route may carry: the declaration's own, and
// maxJSONRequestBytes (decodeJSON) for every row that sets none. It is the most the keyed layer
// spools before its handler runs - a body past it is refused whatever its key, so the layer
// answers the 413 itself (bodyTooLarge) rather than spool it - and the most an early answer
// discards before it is written (drainBeforeAnswer).
func (r apiRoute) bodyLimit() int64 {
	return cmp.Or(r.BodyLimit, maxJSONRequestBytes)
}
