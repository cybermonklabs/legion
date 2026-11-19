package api

import (
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

// routeAuth names who may call a route. It describes the check the handler performs; the
// handler stays the enforcement point.
type routeAuth string

const (
	// authPublic needs no credential.
	authPublic routeAuth = "public"
	// authAny accepts any authenticated caller: a browser cookie or a bearer token. What such a
	// caller may do there is the route's own scopes' to decide (routeScopes), which a person's
	// credential holds and a credential the deployment configured holds none of.
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
	// message rows that carry an issue. The handler reads the object as the caller reads it, so
	// one of a project the caller does not see answers as an object nobody holds
	// (TestNoWriteReachesAHiddenObject, which writeProbes lets call every such row).
	scopeProject writeScope = "project"
	// scopeSession is a write that belongs to the calling session rather than to any project:
	// an agent conversation's own artifact, messages and stream frames, the two routes a
	// session answers its own delivery on, and the caller-private `me` rows. There is no
	// project to compare, and each row is held to its caller another way, each one read:
	//
	//   - The rows whose path names a session — the `agents/{session_id}` writes and
	//     `PUT /api/v1/me/agents/{session_id}/state` — are held to it by withinSessionScope,
	//     which refuses a credential that proved another session, and answers a session the
	//     caller may not see as one nobody holds (project_scope.go). Only a
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
	// scopeDeployment is a write that belongs to the deployment rather than to any project, so a
	// viewer that is not open - a restricted member, or a credential that lists projects - is
	// refused these by name (withinProjectScope).
	scopeDeployment writeScope = "deployment"
	// scopeProjectInBody is a write that belongs to one project the request's body names rather
	// than its path: the issue create, whose project comes from the body, or from the
	// repository an external ref names, and the going request. A credential that lists projects
	// is refused a project outside its list by the handler once that project is settled
	// (refuseOutsideCallerProjects).
	scopeProjectInBody writeScope = "project-in-body"
	// scopeNoWrite is a row that writes nothing and reads no row: its answer is computed from
	// its own body alone. There is no object whose project could be resolved and no session the
	// answer belongs to, so the project check passes it through (withinProjectScope) and this
	// class is what says the row was decided rather than forgotten. POST
	// /api/v1/projects/{key}/related writes nothing either and is scopeProject, because it
	// reads the project its path names; this class is for a row that reads nothing at all.
	scopeNoWrite writeScope = "no-write"
)

// writeClasses is the class of every mutating route, by "<method> <pattern>". It stands beside
// the table rather than in each row because Go leaves no field out of a positional composite
// literal: a sixth value on the row would have to be written on every row of the table, every GET row
// included, and a GET row needs no class. routes() stamps it onto the row, so apiRoute.Scope is
// what every reader of a route sees, and undecidableWrites holds the two in exact correspondence
// — a mutating row this table does not name, and a name that is no mutating row, each fail
// TestEveryMutatingRouteIsClassed and refuse to mount (register).
var writeClasses = map[string]writeScope{
	// The rows that write an object belonging to one project: its issue, its project, its
	// document, its ask, its comment, or a message on one of its issues. The issue create
	// leads them by path and is the one of them whose project its address does not name.
	"POST /api/v1/issues":                                  scopeProjectInBody,
	"PATCH /api/v1/issues/{key}":                           scopeProject,
	"POST /api/v1/issues/{key}/claim":                      scopeProject,
	"DELETE /api/v1/issues/{key}/claim":                    scopeProject,
	"DELETE /api/v1/issues/{key}/subscribers/{session_id}": scopeProject,
	"POST /api/v1/issues/{key}/artifacts":                  scopeProject,
	"POST /api/v1/issues/{key}/artifacts/{slug}/versions":  scopeProject,
	"POST /api/v1/issues/{key}/artifacts/{slug}/edits":     scopeProject,
	"POST /api/v1/issues/{key}/messages":                   scopeProject,
	"POST /api/v1/issues/{key}/asks":                       scopeProject,
	"POST /api/v1/issues/{key}/comments":                   scopeProject,
	"POST /api/v1/issues/{key}/results":                    scopeProject,
	"POST /api/v1/issues/{key}/handoffs/{phase}":           scopeProject,
	"PUT /api/v1/issues/{key}/design-gates":                scopeProject,
	"PUT /api/v1/issues/{key}/role-holders":                scopeProject,

	// A result's retraction: the row belongs to the project of the issue the result was
	// recorded on, one hop through check_results (lockRetractedResult).
	"POST /api/v1/results/{id}/retract": scopeProject,

	"POST /api/v1/projects/{key}/artifacts":                 scopeProject,
	"POST /api/v1/projects/{key}/artifacts/{slug}/versions": scopeProject,
	"POST /api/v1/projects/{key}/artifacts/{slug}/edits":    scopeProject,
	"PUT /api/v1/projects/{key}/default-assignee":           scopeProject,
	// A project's access list, owner and visibility belong to that project; only its owner writes
	// them (project_access.go).
	"PATCH /api/v1/projects/{key}/access":                                  scopeProject,
	"PUT /api/v1/projects/{key}/access/people/{email}":                     scopeProject,
	"DELETE /api/v1/projects/{key}/access/people/{email}":                  scopeProject,
	"PUT /api/v1/projects/{key}/access/groups/{name}":                      scopeProject,
	"DELETE /api/v1/projects/{key}/access/groups/{name}":                   scopeProject,
	"PUT /api/v1/projects/{key}/gated-steps":                               scopeProject,
	"PUT /api/v1/projects/{key}/architecture-sources/{owner}/{repo}":       scopeProject,
	"DELETE /api/v1/projects/{key}/architecture-sources/{owner}/{repo}":    scopeProject,
	"POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync": scopeProject,
	"POST /api/v1/projects/{key}/architecture-sources/sync":                scopeProject,
	"POST /api/v1/projects/{key}/architecture-source/sync":                 scopeProject,
	// The POST form of the related-item check writes nothing. Its scope class is the project its
	// path names, which is the project it searches and the one a scoped credential may run it
	// over. Its answer also carries the answered questions of other projects (other_projects),
	// only of the projects the caller sees (relatedOtherProjects binds its viewer), so it
	// discloses nothing the caller's own reads of those projects do not.
	"POST /api/v1/projects/{key}/related": scopeProject,

	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep":            scopeProject,
	"POST /api/v1/artifacts/{id}/asks":                              scopeProject,
	"POST /api/v1/artifacts/{id}/comments":                          scopeProject,
	"DELETE /api/v1/artifacts/{id}/subscribers/{session_id}":        scopeProject,
	"POST /api/v1/artifacts/{id}/rebuild":                           scopeProject,
	"PATCH /api/v1/artifacts/{id}":                                  scopeProject,
	"DELETE /api/v1/artifacts/{id}":                                 scopeProject,
	"POST /api/v1/artifacts/{id}/reviews":                           scopeProject,
	"POST /api/v1/artifacts/{id}/versions/{number}/reviews":         scopeProject,
	"POST /api/v1/artifacts/{id}/plan-reviews":                      scopeProject,
	"POST /api/v1/artifacts/{id}/approval-requests":                 scopeProject,
	"POST /api/v1/artifacts/{id}/versions":                          scopeProject,
	"POST /api/v1/artifacts/{id}/edits":                             scopeProject,
	"POST /api/v1/artifacts/{id}/_test/outside-schema":              scopeProject,
	"POST /api/v1/artifacts/{id}/_test/newer-content":               scopeProject,
	"POST /api/v1/artifacts/{id}/_test/hold-persistence":            scopeProject,
	"PUT /api/v1/artifacts/{id}/_test/live-write-pauses/{point}":    scopeProject,
	"DELETE /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": scopeProject,

	"PUT /api/v1/asks/{id}/followers/{session_id}":    scopeProject,
	"DELETE /api/v1/asks/{id}/followers/{session_id}": scopeProject,
	"PATCH /api/v1/asks/{id}":                         scopeProject,
	"PUT /api/v1/asks/{id}/asked-of":                  scopeProject,
	"POST /api/v1/asks/{id}/answer":                   scopeProject,
	"POST /api/v1/asks/{id}/resolve":                  scopeProject,
	"POST /api/v1/asks/{id}/decline":                  scopeProject,

	"POST /api/v1/comments/{id}/resolve":    scopeProject,
	"POST /api/v1/comments/{id}/reopen":     scopeProject,
	"POST /api/v1/comments/{id}/deliveries": scopeProject,
	"POST /api/v1/comments/{id}/reply":      scopeProject,
	"POST /api/v1/comments/{id}/accept":     scopeProject,
	"PATCH /api/v1/comments/{id}":           scopeProject,
	"POST /api/v1/comments/{id}/reject":     scopeProject,

	"POST /api/v1/messages/{id}/deliveries": scopeProject,

	// The question an answer run is recorded for belongs to the project its body names, as an
	// issue create's does, so a credential scoped to one project asks only of that project
	// (createGoingRequest makes the check itself).
	"POST /api/v1/going/requests": scopeProjectInBody,

	// The rows that write what belongs to the calling session: its own conversation, its answer
	// to its own delivery, and the caller-private `me` rows.
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept": scopeSession,
	"POST /api/v1/messages/{id}/reply":                       scopeSession,
	"POST /api/v1/agents/{session_id}/messages":              scopeSession,
	"POST /api/v1/agents/{session_id}/artifacts":             scopeSession,
	"POST /api/v1/agents/{session_id}/stream/_test/publish":  scopeSession,
	"PUT /api/v1/me/documents/{id}/read":                     scopeSession,
	"POST /api/v1/me/agent-tokens":                           scopeSession,
	"PATCH /api/v1/me/agent-tokens/{id}":                     scopeSession,
	"DELETE /api/v1/me/agent-tokens/{id}":                    scopeSession,
	"PUT /api/v1/me/threads/{id}/read":                       scopeSession,
	"POST /api/v1/me/inbox/read":                             scopeSession,
	"POST /api/v1/me/inbox/dismiss":                          scopeSession,
	"PUT /api/v1/me/issues/{key}/state":                      scopeSession,
	"PUT /api/v1/me/agents/{session_id}/state":               scopeSession,
	"PUT /api/v1/me/asks/{id}/snooze":                        scopeSession,
	"DELETE /api/v1/me/asks/{id}/snooze":                     scopeSession,

	// The rows that write what belongs to the deployment: a project-scoped credential may not
	// create another project, write a priority the whole deployment is steered by, remap a
	// repository, change a deployment-wide setting, broadcast, decide a credential, or reach
	// into this process.
	"POST /api/v1/projects":                                scopeDeployment,
	"PUT /api/v1/access-groups/{name}":                     scopeDeployment,
	"DELETE /api/v1/access-groups/{name}":                  scopeDeployment,
	"POST /api/v1/priorities":                              scopeDeployment,
	"PATCH /api/v1/priorities/{id}":                        scopeDeployment,
	"DELETE /api/v1/priorities/{id}":                       scopeDeployment,
	"PUT /api/v1/settings/repo-projects/{owner}/{repo}":    scopeDeployment,
	"DELETE /api/v1/settings/repo-projects/{owner}/{repo}": scopeDeployment,
	"PUT /api/v1/settings/delivery":                        scopeDeployment,
	// Which daemon a project's Legion state is read from: addressed under the project it serves,
	// and not belonging to it. Pointing Dispatch at a host is the deployment's write, so a
	// credential that may write one project's issues is refused it by name, the project its path
	// names included.
	"PUT /api/v1/projects/{key}/legion/daemon":        scopeDeployment,
	"DELETE /api/v1/projects/{key}/legion/daemon":     scopeDeployment,
	"POST /api/v1/delivery/session-groups":            scopeDeployment,
	"PATCH /api/v1/delivery/session-groups/{id}":      scopeDeployment,
	"DELETE /api/v1/delivery/session-groups/{id}":     scopeDeployment,
	"POST /api/v1/broadcasts":                         scopeDeployment,
	"POST /api/v1/credential-requests/{id}/approve":   scopeDeployment,
	"POST /api/v1/credential-requests/{id}/deny":      scopeDeployment,
	"POST /api/v1/credential-requests/machine-lookup": scopeDeployment,
	"POST /api/v1/credential-grants/{id}/revoke":      scopeDeployment,
	"POST /api/v1/machine-logins/{id}/revoke":         scopeDeployment,
	"POST /api/v1/signins/{id}/revoke":                scopeDeployment,
	"POST /api/v1/live/_test/disconnect":              scopeDeployment,
	"POST /api/v1/live/_test/rotate":                  scopeDeployment,
	"POST /api/v1/artifacts/_test/quiesce":            scopeDeployment,
	"POST /api/v1/settings/new-issue/_test":           scopeDeployment,
	"POST /api/v1/writes/_test/slow":                  scopeDeployment,

	// The row that writes nothing and reads no row: the markdown round trip, which answers what
	// a document write would store for the text it is given.
	"POST /api/v1/markdown/round-trip": scopeNoWrite,
}

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
// required. It does not decide the wrapping: register wraps every route idRoutes names, since
// one that resolves nothing still has its {id} canonicalized there (resolveIDPrefixes).
func (route idRoute) resolves() bool { return route.Kind != "" || route.Blocks }

// idRoutes is, by "<method> <pattern>", what the {id} and {block_id} of every route that
// carries one name: the kind whose prefix is resolved into it and the 404 that route answers
// for an id of that kind it does not hold, or the reason it resolves none. It stands beside the
// table for writeClasses' reason — the columns would have to be written on every row of the table, and
// most rows carry no id at all — and it is keyed by method and pattern as writeClasses is,
// because two methods of one pattern can answer a missing row differently: PUT
// /api/v1/me/asks/{id}/snooze answers 404 and DELETE answers 204.
//
// TestEveryIDRouteIsNamed holds the two in exact correspondence: a route with an id wildcard
// this table does not name, a row that names neither a kind nor a reason, a resolving row with
// no 404 of its own, and a name that is no route the server serves, each fail it, so a route
// added later cannot silently stop resolving prefixes.
//
// The ids this layer never sees are no part of it: an id a request's body carries (reply_to,
// ask_id, in_reply_to) and the ?artifact= filters are in no path value, and the resolver that
// rewrites a body's short ids as its text is indexed is their one reader (refs.Resolver).
var idRoutes = map[string]idRoute{
	// A document's own routes, and the caller's read mark on one: the {id} is an artifact,
	// looked for among every document and file, as a whole artifact id is. Every one of them
	// reads the row through loadArtifact, which answers ARTIFACT_NOT_FOUND.
	"GET /api/v1/artifacts/{id}":                             {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/_test/outside-schema":       {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/_test/newer-content":        {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/approval-requests":          {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/asks":                        {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/asks":                       {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/blocks":                      {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/comments":                    {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/comments":                   {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/edits":                      {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/events":                      {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/plan-reviews":               {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/rebuild":                    {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"PATCH /api/v1/artifacts/{id}":                           {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"DELETE /api/v1/artifacts/{id}":                          {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/references":                  {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/reviews":                     {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/reviews":                    {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/versions/{number}/reviews":  {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/subscribers":                 {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"DELETE /api/v1/artifacts/{id}/subscribers/{session_id}": {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/text":                        {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/versions":                   {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/artifacts/{id}/versions/{number}":           {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"GET /api/v1/me/documents/{id}/read":                     {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	"PUT /api/v1/me/documents/{id}/read":                     {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},

	// The two routes that also name a block of that document, each answering TARGET_NOT_FOUND
	// for a block id the document does not hold.
	"GET /api/v1/artifacts/{id}/blocks/{block_id}":       {Kind: "artifact", Blocks: true, NotFound: "ARTIFACT_NOT_FOUND", BlockNotFound: "TARGET_NOT_FOUND"},
	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep": {Kind: "artifact", Blocks: true, NotFound: "ARTIFACT_NOT_FOUND", BlockNotFound: "TARGET_NOT_FOUND"},

	// An ask's own routes and the caller's snooze of one. Each reads the row through loadAsk
	// or loadAskOwner, whose missing row is writeHandlerError's NOT_FOUND; the snooze write
	// answers its own NOT_FOUND, and the un-snooze answers 204 for any ask at all.
	"GET /api/v1/asks/{id}":                           {Kind: "ask", NotFound: "NOT_FOUND"},
	"PATCH /api/v1/asks/{id}":                         {Kind: "ask", NotFound: "NOT_FOUND"},
	"PUT /api/v1/asks/{id}/asked-of":                  {Kind: "ask", NotFound: "NOT_FOUND"},
	"POST /api/v1/asks/{id}/answer":                   {Kind: "ask", NotFound: "NOT_FOUND"},
	"POST /api/v1/asks/{id}/resolve":                  {Kind: "ask", NotFound: "NOT_FOUND"},
	"POST /api/v1/asks/{id}/decline":                  {Kind: "ask", NotFound: "NOT_FOUND"},
	"GET /api/v1/asks/{id}/followers":                 {Kind: "ask", NotFound: "NOT_FOUND"},
	"PUT /api/v1/asks/{id}/followers/{session_id}":    {Kind: "ask", NotFound: "NOT_FOUND"},
	"DELETE /api/v1/asks/{id}/followers/{session_id}": {Kind: "ask", NotFound: "NOT_FOUND"},
	"PUT /api/v1/me/asks/{id}/snooze":                 {Kind: "ask", NotFound: "NOT_FOUND"},
	"DELETE /api/v1/me/asks/{id}/snooze":              {Kind: "ask", NotFound: notFoundNone},

	// A comment's own routes. The reads and the thread writes answer writeHandlerError's
	// NOT_FOUND; the two delivery routes answer COMMENT_NOT_FOUND of their own.
	"GET /api/v1/comments/{id}":             {Kind: "comment", NotFound: "NOT_FOUND"},
	"PATCH /api/v1/comments/{id}":           {Kind: "comment", NotFound: "NOT_FOUND"},
	"GET /api/v1/comments/{id}/blocks":      {Kind: "comment", NotFound: "NOT_FOUND"},
	"POST /api/v1/comments/{id}/accept":     {Kind: "comment", NotFound: "NOT_FOUND"},
	"POST /api/v1/comments/{id}/reject":     {Kind: "comment", NotFound: "NOT_FOUND"},
	"POST /api/v1/comments/{id}/reopen":     {Kind: "comment", NotFound: "NOT_FOUND"},
	"POST /api/v1/comments/{id}/resolve":    {Kind: "comment", NotFound: "NOT_FOUND"},
	"POST /api/v1/comments/{id}/deliveries": {Kind: "comment", NotFound: "COMMENT_NOT_FOUND"},
	"POST /api/v1/comments/{id}/reply":      {Kind: "comment", NotFound: "COMMENT_NOT_FOUND"},

	// A message, by any id in its conversation. The issue form names an owner, so a prefix of
	// it is looked for among that issue's messages alone; the owner-free forms look among
	// every message, a direct conversation's included. The reads answer NOT_FOUND and the
	// three delivery routes MESSAGE_NOT_FOUND.
	"GET /api/v1/messages/{id}":                              {Kind: "message", NotFound: "NOT_FOUND"},
	"GET /api/v1/messages/{id}/blocks":                       {Kind: "message", NotFound: "NOT_FOUND"},
	"POST /api/v1/messages/{id}/deliveries":                  {Kind: "message", NotFound: "MESSAGE_NOT_FOUND"},
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept": {Kind: "message", NotFound: "MESSAGE_NOT_FOUND"},
	"POST /api/v1/messages/{id}/reply":                       {Kind: "message", NotFound: "MESSAGE_NOT_FOUND"},
	"GET /api/v1/issues/{key}/messages/{id}":                 {Kind: "message", WithinIssue: true, NotFound: "NOT_FOUND"},

	// The routes whose id names none of the five kinds a prefix shortens — an ask, a comment,
	// a message, a document, a document block — so no prefix is looked for and the handler
	// answers for the value as it always has. A whole uuid an {id} spells another of
	// uuid.Parse's ways is still rewritten to the canonical spelling, as it is on every route
	// that takes an id (canonicalizeUUIDPath).
	"GET /api/v1/broadcasts/{id}":                   {Reason: "a broadcast id: one send to many sessions"},
	"POST /api/v1/credential-grants/{id}/revoke":    {Reason: "a credential grant id"},
	"POST /api/v1/credential-requests/{id}/approve": {Reason: "a credential request id"},
	"POST /api/v1/credential-requests/{id}/deny":    {Reason: "a credential request id"},
	"GET /api/v1/delivery/runs/{id}":                {Reason: "a deploy-repository run id, a positive integer and no uuid at all"},
	"PATCH /api/v1/delivery/session-groups/{id}":    {Reason: "a Delivery session group id"},
	"DELETE /api/v1/delivery/session-groups/{id}":   {Reason: "a Delivery session group id"},
	"GET /api/v1/going/requests/{id}/stream":        {Reason: "a going request's run id, which the route reads for its own author alone and answers for itself"},
	"GET /api/v1/handoffs/{id}":                     {Reason: "a handoff id: one phase's record on an issue"},
	"GET /api/v1/near-copy-checks/{id}":             {Reason: "a near-copy check id: one answer of the near-copy gate"},
	"POST /api/v1/machine-logins/{id}/revoke":       {Reason: "a machine login id"},
	"POST /api/v1/signins/{id}/revoke":              {Reason: "a sign-in id at the secrets broker"},
	"PATCH /api/v1/priorities/{id}":                 {Reason: "a priority id, a slug of its own and no uuid at all"},
	"DELETE /api/v1/priorities/{id}":                {Reason: "a priority id, a slug of its own and no uuid at all"},
	"GET /api/v1/priorities/{id}/history":           {Reason: "a priority id, a slug of its own and no uuid at all"},
	"DELETE /api/v1/me/agent-tokens/{id}":           {Reason: "a personal agent token id"},
	"PATCH /api/v1/me/agent-tokens/{id}":            {Reason: "a personal agent token id"},
	"PUT /api/v1/me/threads/{id}/read":              {Reason: "a thread id: the root of a conversation, and no read of the message it names"},
	"POST /api/v1/results/{id}/retract":             {Reason: "a check result id"},

	// The document test hooks, which address the live room this process holds under the
	// document's own id and read no row of the artifacts table, so they answer no 404 of a
	// document that does not exist and resolve no prefix: a short id written at one of them
	// reaches the hook as the caller wrote it, as every other value does.
	"POST /api/v1/artifacts/{id}/_test/hold-persistence":            {Reason: "a test hook's document id, naming a live room rather than a stored document"},
	"GET /api/v1/artifacts/{id}/_test/room-counters":                {Reason: "a test hook's document id, naming a live room rather than a stored document"},
	"PUT /api/v1/artifacts/{id}/_test/live-write-pauses/{point}":    {Reason: "a test hook's document id, naming a live room rather than a stored document"},
	"GET /api/v1/artifacts/{id}/_test/live-write-pauses/{point}":    {Reason: "a test hook's document id, naming a live room rather than a stored document"},
	"DELETE /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": {Reason: "a test hook's document id, naming a live room rather than a stored document"},
}

// hasIDWildcard reports whether a pattern carries an id the prefix layer could resolve.
func hasIDWildcard(pattern string) bool {
	return strings.Contains(pattern, "{id}") || strings.Contains(pattern, "{block_id}")
}

// undecidedIDRoutes names every route whose pattern carries an id wildcard that idRoutes
// leaves undecided: one it does not name, one that resolves nothing and gives no reason, one
// that names a kind and a reason at once, one that resolves an id or a block without saying
// what 404 that route answers for one it does not hold, and one that resolves nothing and
// names such an answer anyway.
func undecidedIDRoutes(routes []apiRoute) []string {
	var undecided []string
	for _, route := range routes {
		if !hasIDWildcard(route.Pattern) {
			continue
		}
		name := route.Method + " " + route.Pattern
		entry, named := idRoutes[name]
		switch {
		case !named:
			undecided = append(undecided, name+": idRoutes says neither what its id names nor why no prefix is resolved there")
		case entry.resolves() && entry.Reason != "":
			undecided = append(undecided, name+": idRoutes names a kind to resolve and a reason not to")
		case !entry.resolves() && strings.TrimSpace(entry.Reason) == "":
			undecided = append(undecided, name+": idRoutes resolves no prefix there and gives no reason")
		case entry.Kind != "" && entry.NotFound == "":
			undecided = append(undecided, name+": idRoutes resolves an id there and does not say what 404 the route answers for one it does not hold")
		case entry.Blocks && entry.BlockNotFound == "":
			undecided = append(undecided, name+": idRoutes resolves a block id there and does not say what 404 the route answers for one the document does not hold")
		case !entry.resolves() && (entry.NotFound != "" || entry.BlockNotFound != ""):
			undecided = append(undecided, name+": idRoutes resolves nothing there and names a not-found answer anyway")
		}
	}
	sort.Strings(undecided)
	return slices.Compact(undecided)
}

// routeScopeSet is the OAuth scopes one route needs of the credential that calls it, every one
// of which that credential must hold (gateRouteScopes). An empty set is a route any authenticated
// caller may reach.
type routeScopeSet []string

// routeScopes is the set each route needs, by "<method> <pattern>", and it stands beside the
// table for the reason writeClasses does: Go leaves no field out of a positional composite
// literal, so a sixth value on the row would have to be written on every row of the table, and most rows
// need no scope. routes() stamps it onto the row, so apiRoute.OAuthScopes is what every reader
// of a route sees — GET /api/v1 and `envoy-dispatch routes` among them — and
// scopedRoutesNotMounted holds the map to the table, a name that is no row refusing to mount
// (register).
//
// These are the routes Dispatch refused every bearer token on until INFRA-1017, and two scopes
// decide them now: `person`, "this credential acts as its person", which every row here needs,
// and `approve`, the right to settle what a person settles, which the eleven rows below that
// decide a credential, a sign-in, a review or a token need as well. A person's browser, their
// personal tokens and their machine logins hold both; the shared token, a pod's service-account
// token, a project-scoped session's token and a broker-issued token that names a service and no
// person hold neither.
//
// The scope `read` a read-only token holds is no value of this map: a scope here is one the
// caller must hold, and no person holds that one. The routes such a token may call are a list of
// their own, readScopeRoutes below, which its own gate reads (refuseReadOnlyToken).
//
// A route that takes an id written short needs no scope of its own for it: the prefix layer
// (resolveIDPrefixes, idRoutes) runs inside this gate - register wraps the gate around it, so a
// credential that holds none of the row's scopes is refused before any id is read, and the one
// check a request meets ahead of the row's scopes is whether a read-only token may call the row
// at all (refuseReadOnlyToken) - and it only rewrites the path value, so a short id reaches
// exactly the scopes the whole id reaches, and a row that needs none with a whole id needs none
// with a prefix.
var routeScopes = map[string]routeScopeSet{
	// The rows that read or write what belongs to the calling person: their inbox, their own
	// answers and replies, their read marks, their conversations with sessions, and the people
	// and default assignees an assignee picker reads.
	"GET /api/v1/settings/project-default-assignees":         {model.ScopePerson},
	"PUT /api/v1/projects/{key}/default-assignee":            {model.ScopePerson},
	"GET /api/v1/projects/{key}/access":                      {model.ScopePerson},
	"PATCH /api/v1/projects/{key}/access":                    {model.ScopePerson},
	"PUT /api/v1/projects/{key}/access/people/{email}":       {model.ScopePerson},
	"DELETE /api/v1/projects/{key}/access/people/{email}":    {model.ScopePerson},
	"PUT /api/v1/projects/{key}/access/groups/{name}":        {model.ScopePerson},
	"DELETE /api/v1/projects/{key}/access/groups/{name}":     {model.ScopePerson},
	"GET /api/v1/access-groups":                              {model.ScopePerson},
	"PUT /api/v1/access-groups/{name}":                       {model.ScopePerson},
	"DELETE /api/v1/access-groups/{name}":                    {model.ScopePerson},
	"GET /api/v1/users":                                      {model.ScopePerson},
	"GET /api/v1/people/{login}/nodes":                       {model.ScopePerson},
	"GET /api/v1/inbox":                                      {model.ScopePerson},
	"GET /api/v1/me/answers":                                 {model.ScopePerson},
	"GET /api/v1/me/replies":                                 {model.ScopePerson},
	"PUT /api/v1/me/threads/{id}/read":                       {model.ScopePerson},
	"POST /api/v1/me/inbox/read":                             {model.ScopePerson},
	"POST /api/v1/me/inbox/dismiss":                          {model.ScopePerson},
	"GET /api/v1/me/state":                                   {model.ScopePerson},
	"PUT /api/v1/me/issues/{key}/state":                      {model.ScopePerson},
	"GET /api/v1/me/agents/state":                            {model.ScopePerson},
	"PUT /api/v1/me/agents/{session_id}/state":               {model.ScopePerson},
	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep":     {model.ScopePerson},
	"GET /api/v1/me/documents/{id}/read":                     {model.ScopePerson},
	"PUT /api/v1/me/documents/{id}/read":                     {model.ScopePerson},
	"PUT /api/v1/me/asks/{id}/snooze":                        {model.ScopePerson},
	"DELETE /api/v1/me/asks/{id}/snooze":                     {model.ScopePerson},
	"GET /api/v1/agents/{session_id}/messages":               {model.ScopePerson},
	"POST /api/v1/agents/{session_id}/messages":              {model.ScopePerson},
	"POST /api/v1/broadcasts":                                {model.ScopePerson},
	"GET /api/v1/broadcasts":                                 {model.ScopePerson},
	"GET /api/v1/broadcasts/{id}":                            {model.ScopePerson},
	"GET /api/v1/issues/{key}/subscribers":                   {model.ScopePerson},
	"DELETE /api/v1/issues/{key}/subscribers/{session_id}":   {model.ScopePerson},
	"GET /api/v1/artifacts/{id}/subscribers":                 {model.ScopePerson},
	"DELETE /api/v1/artifacts/{id}/subscribers/{session_id}": {model.ScopePerson},
	"POST /api/v1/artifacts/{id}/rebuild":                    {model.ScopePerson},
	"PATCH /api/v1/artifacts/{id}":                           {model.ScopePerson},
	"DELETE /api/v1/artifacts/{id}":                          {model.ScopePerson},
	"PUT /api/v1/asks/{id}/asked-of":                         {model.ScopePerson},
	"POST /api/v1/asks/{id}/answer":                          {model.ScopePerson},
	"POST /api/v1/asks/{id}/decline":                         {model.ScopePerson},
	"POST /api/v1/comments/{id}/reopen":                      {model.ScopePerson},
	"POST /api/v1/comments/{id}/accept":                      {model.ScopePerson},
	"PATCH /api/v1/comments/{id}":                            {model.ScopePerson},
	"POST /api/v1/comments/{id}/reject":                      {model.ScopePerson},
	"GET /api/v1/me/agent-tokens":                            {model.ScopePerson},
	"GET /api/v1/credential-requests":                        {model.ScopePerson},
	"GET /api/v1/credential-grants":                          {model.ScopePerson},
	"GET /api/v1/machine-logins":                             {model.ScopePerson},
	"GET /api/v1/signins":                                    {model.ScopePerson},
	"GET /api/v1/credentials/capabilities":                   {model.ScopePerson},
	"POST /api/v1/agents/{session_id}/stream/_test/publish":  {model.ScopePerson},

	// The rows that settle what a person settles: a document's review, a credential's fate, and
	// a personal token's own scopes, which a credential may not widen past its own.
	"POST /api/v1/artifacts/{id}/reviews":             {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/credential-requests/{id}/approve":   {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/credential-requests/{id}/deny":      {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/credential-requests/machine-lookup": {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/credential-grants/{id}/revoke":      {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/machine-logins/{id}/revoke":         {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/signins/{id}/revoke":                {model.ScopePerson, model.ScopeApprove},
	"POST /api/v1/me/agent-tokens":                    {model.ScopePerson, model.ScopeApprove},
	"PATCH /api/v1/me/agent-tokens/{id}":              {model.ScopePerson, model.ScopeApprove},
	"DELETE /api/v1/me/agent-tokens/{id}":             {model.ScopePerson, model.ScopeApprove},
	// The review that names the version its caller read settles what the review above settles,
	// with that version as a guard.
	"POST /api/v1/artifacts/{id}/versions/{number}/reviews": {model.ScopePerson, model.ScopeApprove},
}

// readScopeRoutes is every route a read-only token may call, by "<method> <pattern>": a token the
// secrets broker minted from a read entry, which holds the scope read alone and names a service
// and a session and no person and no project (model.Credential.ReadOnly). Every other row of the
// table, and GET /ws/live beside it, answers such a token 403 READ_ONLY_TOKEN before anything
// else the route does (refuseReadOnlyToken), so a route added later stays closed to it until it
// is listed here.
//
// A route is listed only when, for such a caller, it writes nothing but the row that records its
// DPoP proof as spent (store.DPoPProofs) and caches Dispatch rebuilds from what it already holds:
// the display trees a read with Dispatch-Trees: 1 stores (displayTreeReader.storeOf) and the
// stored listener roster (route_roster). TestTheReadOnlyListedRouteWritesOnlyItsProofAndCaches
// holds the route listed today to that.
//
// It is a list beside the table and not a value of routeScopes: a scope there is one the caller
// must hold, and no person holds read, so the list says what a credential that holds read alone
// may still call rather than what a route requires. For the same reason the route index (GET
// /api/v1), the OpenAPI document and the reference generated from it do not carry it. routes()
// stamps it onto the row (apiRoute.ReadScope), and register refuses to mount a table whose list
// names anything but a GET row it mounts (readRoutesNotMountedAsGET).
var readScopeRoutes = []string{"GET /api/v1/projects/{key}/architecture"}

// routesNotMounted names every route of names that is no row of the table, sorted. The walk is
// over the table with its test hooks in, since a hook's row is a row of the table wherever a
// deployment leaves it out.
func routesNotMounted(names iter.Seq[string]) []string {
	table := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	mounted := make(map[string]bool, len(table))
	for _, route := range table {
		mounted[route.Method+" "+route.Pattern] = true
	}
	var unmounted []string
	for name := range names {
		if !mounted[name] {
			unmounted = append(unmounted, name)
		}
	}
	sort.Strings(unmounted)
	return unmounted
}

// scopedRoutesNotMounted names every route routeScopes gives scopes that the table does not
// hold. A scope named on no row is a scope nothing enforces, which is what register refuses to
// mount, as it refuses a write class it cannot decide (undecidableWrites). Its scope says what
// a row needs when the deployment serves it, test hooks included.
func scopedRoutesNotMounted() []string {
	return routesNotMounted(maps.Keys(routeScopes))
}

// readRoutesNotMountedAsGET names every route of names that is not a GET row of the table,
// sorted: one no row holds (routesNotMounted) and one whose method is not GET. register refuses
// to mount a read list that names either (readScopeRoutes). A name no row holds opens nothing
// while the list reads as though it opened a route - a row renamed or deleted and its entry left
// behind - and a POST, PUT, PATCH or DELETE row is a write, which a read-only token is minted
// never to make. The walk is over the table with its test hooks in, as routesNotMounted's is.
func readRoutesNotMountedAsGET(names []string) []string {
	refused := routesNotMounted(slices.Values(names))
	for _, name := range names {
		if method, _, _ := strings.Cut(name, " "); method != http.MethodGet && !slices.Contains(refused, name) {
			refused = append(refused, name)
		}
	}
	sort.Strings(refused)
	return refused
}

// routeRow is a row as the table writes it: the five columns every route has, written as a
// positional literal, which is why the write class is not among them (writeClasses).
type routeRow struct {
	Method      string
	Pattern     string
	Auth        routeAuth
	Description string
	Handler     http.HandlerFunc
}

// apiRoute is one mounted route: the table's row, the class routes() stamps onto it from
// writeClasses, the OAuth scopes it stamps from routeScopes, the read classes it stamps from
// readClasses, and whether a read-only token may call it, which it stamps from readScopeRoutes.
// Register mounts the table and GET /api/v1 lists it, so the two can never disagree about what
// the server serves. Every mutating row carries a Scope, and a GET row carries none
// (project_scope.go); every GET row carries Reads, and a mutating row carries none
// (read_classes.go); OAuthScopes is empty on every row any authenticated caller may reach;
// ReadScope is true on the rows the read list names and on no other, and only the read-only gate
// reads it (refuseReadOnlyToken), never the route index or the OpenAPI document; the row's own
// five columns are read through it by promotion. What the route takes and answers is
// routeShapes', which only the OpenAPI emitter reads, so no row carries it.
type apiRoute struct {
	routeRow
	Scope       writeScope
	OAuthScopes routeScopeSet
	Reads       []readclass.Class
	ReadScope   bool
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
// which of the nine members that route declares and why there are two routes.
func searchDescription(declares string) string {
	return fmt.Sprintf("Full-text search ?q= across issues, documents, asks, comments, and messages, within ?project= when given: every match ranked on one list across the kinds by one score, the keyword leg's length-normalised rank and the meaning leg's cosine weighted onto one scale and summed, one page of ?limit= (1-50, default 20) from ?offset=, with the total match count and how many the pages can reach (each leg's best %d). A q that is one issue key in any form a person writes it - INFRA-1042, infra-1042, INFRA 1042, or the number alone, which names that issue of the project the number is read in, or of every project at once, all of them then heading the list in project order - puts that issue first. The number is read in ?project= when the search is narrowed to one, and otherwise in ?number_project=, the project of the page the search was made from: that parameter narrows nothing at all, so a search carrying it answers every project's rows, and a request naming both reads the number in the ?project= it filters by. A q is searched as typed, websearch_to_tsquery's AND of its words, whatever its length; only a q that matches nothing at all is searched a second time for its %d rarest words ORed, rarest by the document frequency the project's own term statistics hold. The \"phrase\", OR and -word forms are read as always, and the answer's query {operators, broadened} names the forms it read and whether that second search is what answered. ?status=, ?author= and ?updated_since= narrow every leg of the search beside ?project=: the status of the issue a row belongs to (a row with no issue, such as a project document, matches none), the actor id that wrote the item a row names (a person's login or a session id; an issue's and a document's is their creator), and the RFC3339 time at or after which that item was last written. The answer's filters echoes those four and never ?number_project=, which narrowed nothing. ?keywords_only=true runs the keyword leg alone and makes no embedding call, whatever embedder the deployment configured; its answer is not stamped degraded, because nothing is unavailable - the caller asked for the words alone. The command bar sends it on every keystroke, so a person typing waits on no model call and no reader of this route should take the bar's type-ahead for the hybrid path. A request leaving it out searches as it always did, by meaning too wherever an embedder is configured. %s A q over %d characters is 400 CAP_EXCEEDED, a project or number_project that is not a project key 400 INVALID_PROJECT or INVALID_NUMBER_PROJECT, a repeated, blank or unreadable status, author or updated_since 400 INVALID_STATUS, INVALID_AUTHOR or INVALID_UPDATED_SINCE, and a keywords_only that is neither true nor false 400 INVALID_KEYWORDS_ONLY.",
		contracts.SearchKindDepth, searchBroadenWords, declares, contracts.SearchQueryMax)
}

// searchDeclaresFour is the paragraph of GET /api/v1/search: five of the nine members this
// handler reads are declared elsewhere, and a client sends them elsewhere.
const searchDeclaresFour = "This route declares ?q=, ?project=, ?limit= and ?offset= alone, and GET /api/v1/search/results declares all nine: a client sends a search carrying ?number_project=, ?status=, ?author=, ?updated_since= or ?keywords_only= there, because a build released before those five answers this route 200 with the member dropped, which is unnarrowed rows under the filters a person chose, where it answers that path 404 no route. This route still reads all nine, so a page served by an earlier release that sends one of the five here is still answered narrowed."

// searchDeclaresNine is the paragraph of GET /api/v1/search/results, the route that declares
// every member the handler reads.
const searchDeclaresNine = "This route declares all nine members and answers exactly what GET /api/v1/search answers, from the same handler: a client sends a search carrying ?number_project=, ?status=, ?author=, ?updated_since= or ?keywords_only= here, because a build released before those five answers this path 404 no route, where it answers GET /api/v1/search 200 with the member dropped, which is unnarrowed rows under the filters a person chose. GET /api/v1/search still reads all nine, so a page served by an earlier release that sends one of the five there is still answered narrowed."

// routes lists every native-workspace route. Add a row here, never a mux.HandleFunc line. A
// mutating row's write class comes from writeClasses, which Register's project-scope check
// reads; a row that names no class there is one that check cannot decide, and Register refuses
// to mount it (undecidableWrites).
func (s *server) routes() []apiRoute {
	rows := []routeRow{
		{http.MethodGet, "/api/v1", authPublic, "This index: every route with its method, auth, and purpose.", s.getRouteIndex},
		{http.MethodGet, "/api/v1/schema/blocks", authPublic, "The typed document block schema as JSON.", s.getBlockSchema},
		{http.MethodGet, "/api/v1/limits", authAny, "The bounds a write is held to: how long one block of a body may be and how many blocks a body may hold, how many options an ask may have and how long its question and options are together, how long a body an event carries whole may be, and the bytes a request, a document and a handoff body may hold.", s.getLimits},
		{http.MethodPost, "/api/v1/markdown/round-trip", authAny, "The markdown Dispatch would store for the text a body gives it {markdown}: the parse and the render every write of a whole document makes. The route writes nothing and reads no row. A caller compares the answer with the text it holds, and refuses its own write where the two differ. Text the parse or the render refuses comes back as it was given, so the write that follows refuses it with its own error.", s.postMarkdownRoundTrip},
		{http.MethodGet, "/api/v1/projects", authAny, "List the projects the caller sees; a private project carries visibility private, and a public one no visibility. A project's default assignee is not listed here: GET /api/v1/settings/project-default-assignees reads it, with the person scope.", s.listProjects},
		{http.MethodPost, "/api/v1/projects", authAny, fmt.Sprintf("Create a project {key, name, default_assignee}: any signed-in person or agent session; a bearer names its session in actor. A restricted member, or a credential acting for one, is 403 RESTRICTED_MEMBER. Every project names the person its unowned work lands on, the last step of the chain that decides who a question reaches: default_assignee is the email of a person who has signed in, and a call that names none takes the person creating the project, else the person its credential acts for. The project's owner is the person creating it, else the default_assignee the call names. A credential that acts for nobody and names no default_assignee is 400 INVALID_PROJECT; a login nobody has signed in with, the one the call names or the one its credential acts for, is 400 ASSIGNEE_NOT_ALLOWED. A key that is not a project key, an empty name, a name holding a control character or a bidi embedding, override or isolate, or a name with no visible character (a letter, number, punctuation or symbol other than the blank fillers U+115F, U+1160, U+3164, U+FFA0 and U+2800, or a code point Go's Unicode tables leave unassigned, such as an emoji newer than them, other than a noncharacter (U+FDD0-U+FDEF and each code point ending in FFFE or FFFF) or a reserved default-ignorable (U+2065, U+FFF0-U+FFF8, U+E0000-U+E0FFF); combining marks, format characters, private-use characters and whitespace don't count) is 400 INVALID_PROJECT; a name over %d characters is 400 CAP_EXCEEDED; a key that already exists is 409 PROJECT_EXISTS. The answer is the project with the default_assignee the create stored. The owner a create writes is a full member: a default_assignee who is not one is 400 OWNER_NOT_ALLOWED when no person creates the project.", contracts.ProjectNameMax), s.createProject},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts", authAny, "List a project's documents.", s.listProjectArtifacts},
		{http.MethodPost, "/api/v1/projects/{key}/artifacts", authAny, "Upload a project document or file (JSON or multipart); a file's kind is read from its bytes, and a new artifact's name held to its bound, as an issue upload's are.", s.uploadProjectArtifact},
		{http.MethodGet, "/api/v1/projects/{key}/related", authAny, fmt.Sprintf("Run the related-item check an issue create runs, over ?title= and ?text= (checked as the title, a newline and the text; either alone is enough), without writing anything: no issue, ask, suggestion or related_checks record. ?exclude_issue= leaves that issue of the project and everything it holds out of the results and out of the word statistics (400 EXCLUDE_ISSUE_INPUT when it is not an issue of the project); ?exclude_issues=, repeated, leaves each of up to %d issues of the project out of the results alone, so a draft can leave out a whole subtree without changing how rare its words are (400 EXCLUDE_ISSUE_INPUT for a longer list or for a key the project does not hold); ?keywords_only=true skips meaning search. It waits at most 5 seconds and answers the create's advice.suggestions shape: outcome found, none or not_made (a blank text is 200 not_made, the reason in missing), search, search_note, search_query and related, with up to 3 open issues before any item of another kind and everything a done issue owns last. This form answers no other_projects: its caller is the dashboard's new-issue dialog, which runs the check for each burst of typing and draws the items of the asking project alone. Title and text over %d characters together are 400 CAP_EXCEEDED; POST takes a longer text. An unknown project is 404 PROJECT_NOT_FOUND.", relatedExcludeLimit, contracts.SearchQueryMax), s.getRelated},
		{http.MethodPost, "/api/v1/projects/{key}/related", authAny, fmt.Sprintf("The GET form of this route with its fields in a JSON body {title, text, exclude_issue, exclude_issues, keywords_only}, exclude_issues a list of issue keys: the related-item check over a title and a text as long as an issue's spec (the body's 1 MiB bound; a title over %d characters is 400 CAP_EXCEEDED), writing nothing and waiting at most 5 seconds. The two forms do not answer alike. This one is the check an agent runs to ask a question, so it answers other_projects beside related: at most 2 answered question asks of other projects, each with its ask state, its answer and the project it stands in (owner.project), ranked by meaning alone and offered with no claim that they answer the question. The GET form answers none.", contracts.IssueTitleMax), s.postRelated},
		{http.MethodGet, "/api/v1/settings/repo-projects", authAny, "List GitHub repository to project mappings, each marked whether it is its project's default and what repository it was renamed to, null for one that was never renamed.", s.listRepoProjects},
		{http.MethodPut, "/api/v1/settings/repo-projects/{owner}/{repo}", authAny, `Map a GitHub repository to a project {project, default?, renamed_to?}; default true makes it the project's one default. renamed_to is the owner/name this repository is called today, which every comparison of a repository canonicalises through: absent or null leaves the stored one alone, "" clears it, and a pair naming this repository, one naming a repository that is itself renamed, or one on a repository another mapping is already renamed to is 400 INVALID_REPO_PROJECT. A mapping whose GitHub subject lane, for its name or its renamed_to, compared without case, is also a lane of another project's mapping, where either project is private, is 409 REPOSITORY_LANE_SHARED. A target project the caller cannot see is answered as an unknown project. A repository mapped to a project the caller cannot see is neither moved nor named. A mapping into a private project is 409 PRIVATE_PROJECTS_UNAVAILABLE, with nothing changed, while the hub does not take the project's key in its envoy_private_projects bucket.`, s.putRepoProject},
		{http.MethodDelete, "/api/v1/settings/repo-projects/{owner}/{repo}", authAny, "Remove a repository mapping, whether or not an issue names the repository as its repo: an issue's own repository needs no mapping, so such an issue keeps it. An issue that resolved to the mapping as its project's default resolves to no repository until the project has another default. A mapping of a project the caller cannot see answers 204 as no mapping does, and is not removed.", s.deleteRepoProject},
		{http.MethodGet, "/api/v1/github/app-installation/{owner}/{repo}", authAny, "Whether Dispatch's GitHub App, which GitHub sends the repository's webhook events through, is installed on the repository: 200 {installed}, false when GitHub answers 404; 503 GITHUB_APP_UNCONFIGURED when Dispatch holds no App key, 502 GITHUB_UNREADABLE naming the repository and GitHub's error on any other failure. A repository the caller may not see - one mapped to no project it sees, or one no project maps for a restricted member or a credential that lists projects - answers what one the App is not installed on answers.", s.getAppInstallation},
		{http.MethodGet, "/api/v1/settings/architecture-sources", authAny, "List every project's architecture sources, by project and then in the order each project added them.", s.listArchitectureSources},
		{http.MethodGet, "/api/v1/projects/{key}/architecture-sources", authAny, "A project's architecture sources, one per repository, in the order they were added; [] when it has none. The project's component model is every source's components together.", s.listProjectArchitectureSources},
		{http.MethodGet, "/api/v1/projects/{key}/architecture-source", authAny, "Kept for dispatch clients older than this release, logged at warn: the project's first architecture source, or null when it has none. GET /api/v1/projects/{key}/architecture-sources replaces it.", s.getArchitectureSource},
		{http.MethodPut, "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}", authAny, "Add a repository as an architecture source of a project, or change the branch of the source it is {branch}, after proving the GitHub App can read it and that branch is the name of a branch it has; a missing branch, a commit SHA, a relative ref (main~1, HEAD) or a refs/ path is 409 SOURCE_ACCESS naming the value and the repository, as a credential problem is, and nothing is written. A re-save resets the source's sync bookkeeping and keeps its components.", s.putArchitectureSource},
		{http.MethodDelete, "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}", authAny, "Remove one architecture source of a project and the components it projected; every other source's components stay.", s.deleteArchitectureSource},
		{http.MethodPost, "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync", authAny, "Import one architecture source's model now; 200 carries its updated row (last_error set when the import failed), 409 SOURCE_ACCESS a credential or branch problem.", s.syncArchitectureSource},
		{http.MethodPost, "/api/v1/projects/{key}/architecture-sources/sync", authAny, "Import every architecture source of a project now, each whatever another's import did; 200 carries every updated row in the order the sources were added, each with its own last_error (an access failure included).", s.syncArchitectureSources},
		{http.MethodPost, "/api/v1/projects/{key}/architecture-source/sync", authAny, "Kept for dispatch clients older than this release, logged at warn: import every architecture source of a project now and answer one row, the first in id order whose last_error is set or else the first; 404 SOURCE_NOT_FOUND without a source. POST /api/v1/projects/{key}/architecture-sources/sync replaces it.", s.syncSingularArchitectureSource},
		{http.MethodGet, "/api/v1/projects/{key}/architecture", authAny, "A project's component tree with the work attached: its sources' sync bookkeeping, each component with its source repository, per-component counts and issue rows, plus unassigned, not-architectural, and retired-link issues; 404 SOURCE_NOT_FOUND without a source.", s.getArchitectureTree},
		{http.MethodGet, "/api/v1/settings/project-default-assignees", authAny, "List every project with the person it defaults to, by project key: every project names one, and that person is the last step of the chain that decides who a question reaches. Needs the person scope, as the default names a person: a credential the deployment configured, the shared token or a pod's, holds none.", s.listProjectDefaultAssignees},
		{http.MethodGet, "/api/v1/settings/new-issue", authAny, "What the dashboard's search bar files a person's unmatched typed words as: {label, session}, the label that new issue carries and the session it is routed to and its first message goes to, as this deployment configured them (DISPATCH_NEW_ISSUE_LABEL, DISPATCH_NEW_ISSUE_SESSION). Each is \"\" where it configured none: no label on the issue, and no conversation started on it. The bar writes the issue itself, through POST /api/v1/issues and the issue's own artifact, patch and message routes.", s.getNewIssueSettings},
		{http.MethodPut, "/api/v1/projects/{key}/default-assignee", authAny, "Move a project's default assignee to another person {assignee} (the email of a person who has signed in): who an issue created with no assignee, by nobody, under no parent or under a parent with nobody on it, goes to, and who a question on an issue nobody holds reaches. A project always names a person, so an assignee that is absent, null or blank is 400 INVALID_PROJECT and the default cannot be cleared. Needs the person scope, as the default names a person: a credential the deployment configured, the shared token or a pod's, holds none.", s.putProjectDefaultAssignee},
		{http.MethodGet, "/api/v1/projects/{key}/access", authAny, "Who may see a project and who changes that: {project, owner, visibility, people, groups, slack_channels, not_signed_in}, the owner's email, public or private, the emails and group names its access list names, the Slack channel ids its credentials may post to, and the people of that list nobody has signed in as yet, each list sorted and never null. slack_channels_changed_at is when the list last changed, null until a write changes the list. An unknown key is 404 PROJECT_NOT_FOUND. Needs the person scope.", s.getProjectAccess},
		{http.MethodPatch, "/api/v1/projects/{key}/access", authAny, "Give a project to another owner, set its visibility or name its Slack channels {owner?, visibility?, slack_channels?}, at any time: owner is the email of a full member who has signed in, and visibility public or private. slack_channels names the Slack channel ids the project's credentials may post to, at most 16, set only by the owner; it replaces the whole list, which is stored sorted with each id once, and an id that is no Slack channel id is 400 INVALID_SLACK_CHANNEL naming it while more than 16 is 400 CAP_EXCEEDED. A private project's write of a Slack channel that another private project lists is 409 SLACK_CHANNEL_SHARED. Only the project's owner calls it (403 PROJECT_OWNER_REQUIRED); an owner who is not a full member is 400 OWNER_NOT_ALLOWED; a body naming none of the three, or a visibility that is neither, is 400 PROJECT_ACCESS_INPUT. private is 409 PRIVATE_PROJECTS_OFF when the deployment names no private project, 409 PRIVATE_PROJECT_NOT_PROVISIONED when DISPATCH_PRIVATE_PROJECTS does not name this one, and 409 PRIVATE_PROJECTS_INCOMPLETE until the store's backfills have run; public is never refused by them. private is 409 REPOSITORY_LANE_SHARED when one of the project's repositories shares a GitHub subject lane with another project's. private is 409 PRIVATE_PROJECTS_UNAVAILABLE, with nothing changed, while the hub does not take the project's key in its envoy_private_projects bucket. Appends project.access_changed and answers the project's access as GET answers it. Needs the person scope.", s.patchProjectAccess},
		{http.MethodPut, "/api/v1/projects/{key}/access/people/{email}", authAny, "Name a person on a project's access list by email (stored trimmed and lowercase); a person it names already stays as they were. Only the project's owner calls it (403 PROJECT_OWNER_REQUIRED); a value that is no email is 400 PROJECT_ACCESS_INPUT; a list of 500 people and groups is 409 ACCESS_LIST_FULL. Appends project.access_changed and answers the project's access. Needs the person scope.", s.putProjectAccessPerson},
		{http.MethodDelete, "/api/v1/projects/{key}/access/people/{email}", authAny, "Take a person off a project's access list; one it does not name leaves it as it was. Only the project's owner calls it (403 PROJECT_OWNER_REQUIRED). Appends project.access_changed and answers the project's access. Needs the person scope.", s.deleteProjectAccessPerson},
		{http.MethodPut, "/api/v1/projects/{key}/access/groups/{name}", authAny, "Name a group on a project's access list; a group admits only its members who are full members. Only the project's owner calls it (403 PROJECT_OWNER_REQUIRED); a group no row holds is 404 GROUP_NOT_FOUND; a list of 500 people and groups is 409 ACCESS_LIST_FULL. Appends project.access_changed and answers the project's access. Needs the person scope.", s.putProjectAccessGroup},
		{http.MethodDelete, "/api/v1/projects/{key}/access/groups/{name}", authAny, "Take a group off a project's access list; one it does not name leaves it as it was. Only the project's owner calls it (403 PROJECT_OWNER_REQUIRED). Appends project.access_changed and answers the project's access. Needs the person scope.", s.deleteProjectAccessGroup},
		{http.MethodGet, "/api/v1/access-groups", authAny, "Every group an access list can name, by name: {name, owner, members, not_signed_in}, its owner the full member who made it, its members' emails sorted, and the members nobody has signed in as yet, whom the group admits only once they sign in as full members; [] for none. The owner may hand the group to another full member. Needs the person scope. It pages, at most limit groups (1-100, default 100) after the name after, and next names where the next page starts.", s.listAccessGroups},
		{http.MethodPut, "/api/v1/access-groups/{name}", authAny, "Make a group or write it again {members, owner?}: name is lowercase letters, digits, dots, underscores and dashes, at most 63, and members the emails of the people it holds, exactly (at most 1000). The first write makes the group and its writer becomes its owner; after that only the owner writes it (403 GROUP_OWNER_REQUIRED), and owner gives it to another full member. A restricted member cannot make a group (403 RESTRICTED_MEMBER), a member who is a restricted member is 400 GROUP_MEMBER_RESTRICTED, and a bad name or email is 400 PROJECT_ACCESS_INPUT. Appends access_group.changed and answers the group. Needs the person scope. An owner holds at most 100 groups; one more is 400 CAP_EXCEEDED.", s.putAccessGroup},
		{http.MethodDelete, "/api/v1/access-groups/{name}", authAny, "Delete a group, which takes it off every access list that named it; only its owner calls it (403 GROUP_OWNER_REQUIRED), and a group no row holds is 404 GROUP_NOT_FOUND. Appends project.access_changed for each such list and access_group.changed, and answers 204. Needs the person scope.", s.deleteAccessGroup},
		{http.MethodGet, "/api/v1/access/projects/{key}", authAny, "The membership answer the secrets broker reads: {project, visibility, owner, person, member_kind, visible, repositories, slack_channels} for ?person= (full, restricted, or none for a login nobody has signed in with; left out, member_kind is none and visible false): whether the read rule lets that person see the project at a browser, a person with no people row read as a full member, the repositories mapped to the project by their mapped and renamed names, and slack_channels, the owner's list, [] for none. slack_channels_changed_at is when the list last changed, null until a write changes the list. A project the person cannot see still answers 200; an unknown key is 404 PROJECT_NOT_FOUND. Only a service account DISPATCH_ACCESS_ACCOUNTS names reads it, with its projected token; a caller with no credential, or one whose credential does not verify, is refused as every route refuses it (401 NO_IDENTITY for none); every other caller is 403 ACCESS_ACCOUNT_REQUIRED.", s.getAccessProject},
		{http.MethodGet, "/api/v1/access/person", authAny, "One person's member kind as the membership answer spells it: {person, member_kind} for ?person= (full, restricted, or none for a login nobody has signed in with); no person is 400 INVALID_QUERY. Only a service account DISPATCH_ACCESS_ACCOUNTS names reads it; a caller with no credential, or one whose credential does not verify, is refused as every route refuses it (401 NO_IDENTITY for none); every other caller is 403 ACCESS_ACCOUNT_REQUIRED.", s.getAccessPerson},
		{http.MethodGet, "/api/v1/access/private-projects", authAny, "Every private project's Slack channels and when they last changed, for the secrets broker's own service-account token; a caller with no credential, or one whose credential does not verify, is refused as every route refuses it (401 NO_IDENTITY for none); every other caller is 403 ACCESS_ACCOUNT_REQUIRED.", s.getAccessPrivateProjects},
		{http.MethodPut, "/api/v1/projects/{key}/gated-steps", authAny, "Publish the design-gate steps the Legion deployment that serves this project gates {steps}: a list of decisions, criteria and spec, in the order it gates them, or [] for a deployment that gates none. The deployment's daemon writes it at every boot; a publish of the list the row already holds changes nothing and appends no event. A body without steps, or one naming anything else, is 400 INVALID_PROJECT, and an unknown project 404 PROJECT_NOT_FOUND. It declares no personal scope: the credential the deployment configured writes it, as it does the issue PATCH.", s.putProjectGatedSteps},
		{http.MethodGet, "/api/v1/settings/delivery", authAny, "The delivery timeline's one configuration record (deploy repository, workflow paths, production job names, optional production apply step, population authors, excluded repositories, and the Pipeline page's optional staging end-to-end, staging deploy and PR-checks roll-up job names), or null when it has not been set.", s.getDeliverySettings},
		{http.MethodPut, "/api/v1/settings/delivery", authAny, "Set the delivery timeline's configuration after proving the GitHub App can read the deploy repository; 409 DELIVERY_SETTINGS_ACCESS a credential or repository problem.", s.putDeliverySettings},
		{http.MethodGet, "/api/v1/projects/{key}/legion/daemon", authAny, "The Legion daemon this project's state is read from, the one Dispatch reads `GET /legion/v1/state` from once a minute, with the actor that first named it and when it was last written; null when the project names none. No route lists every project's daemon: a page that shows them all reads each project's. 400 INVALID_PROJECT for a key that is not a project key, 404 PROJECT_NOT_FOUND for a project that does not exist.", s.getLegionSetting},
		{http.MethodPut, "/api/v1/projects/{key}/legion/daemon", authAny, "Name the Legion daemon this project's state is read from {state_url}: an absolute http(s) URL with a host and no path, else 400 LEGION_SETTING_INPUT. The daemon belongs to the deployment, so a project-scoped credential is refused it, the project this path names included. 400 INVALID_PROJECT for a key that is not a project key, 404 PROJECT_NOT_FOUND for a project that does not exist.", s.putLegionSetting},
		{http.MethodDelete, "/api/v1/projects/{key}/legion/daemon", authAny, "Stop reading this project's Legion daemon; what was already read stays. 404 NOT_FOUND when the project names none, 400 INVALID_PROJECT for a key that is not a project key and 404 PROJECT_NOT_FOUND for a project that does not exist.", s.deleteLegionSetting},
		{http.MethodGet, "/api/v1/projects/{key}/legion/board", authAny, "The Legion board of the project its path names, from what the sampler recorded of that project's daemon: eleven columns (queue, design, planning, implementing, testing, reviewing, accepting, retro, merging, awaiting_person, held), each card naming its issue's key, title, priority, phase and when that phase began, the slot it holds, the roles in a turn now, the issues a blocker link holds it on, what it waits on from a person, its tree root and the children of its tree that have not started; and a header with the slots in use, the slot cap, the queue's length and the last read of the daemon with its failure. 404 LEGION_NOT_CONFIGURED when the project names no daemon, 400 INVALID_PROJECT for a key that is not a project key, 404 PROJECT_NOT_FOUND for a project that does not exist.", s.getLegionBoard},
		{http.MethodGet, "/api/v1/delivery/timeline", authAny, "The delivery timeline within [from, to) (default the last 7 days), the search q (title or PR id) and the given facets (repeatable repo, parent_agent, session, issue, priority (P0-P3), component (a parent includes its children), author, rework, deployed; __no_issue__, __no_priority__, __no_component__ and __no_session__ select by cause): merges with each PR's issue title, priority and effective components, successful production deploys with every window PR they shipped first, pipeline failures with each run's production job and failing step, and merged work waiting to deploy, each PR's deployed_status (deployed, waiting, not_tracked) computed from stored facts; facet_counts (each facet counted with every other facet applied and its own ignored), color_counts (with none applied), the components and issue titles the window names, data freshness, and measures (what GET /api/v1/delivery/measures answers for the same window, search and facets); 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row. Only the pull requests whose issue the caller sees, and, for a full member or a credential with no person that lists no project, those that name no issue; every count, facet, measure and title is computed from them, and a run with none of its pull requests left is left out.", s.getDeliveryTimeline},
		{http.MethodGet, "/api/v1/delivery/runs/{id}", authAny, "One deploy-repository run with every job it ran, by start, each with its result and timings; 400 RUN_ID_INPUT for an id that is not a positive integer, 404 NOT_FOUND for no such run, 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row. A run the timeline's rule does not show the caller answers 404 NOT_FOUND, as a run id no run holds does.", s.getDeliveryRun},
		{http.MethodGet, "/api/v1/delivery/measures", authAny, "The four delivery measures and Sami's KPI targets for [from, to) (default the last 7 days) and the same facets the timeline takes: deploys a day, lead time (merge, first commit and open to production; open to merge), change failure rate per PR and per deploy (flags_source says whether stored flags fed it), time to restore, rework share, deploy-run success, a per-UTC-day series, the open P0 issues with no owner, and whether each target is met; 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row. Only the pull requests whose issue the caller sees, and, for a full member or a credential with no person that lists no project, those that name no issue; every count, facet, measure and title is computed from them, and a run with none of its pull requests left is left out.", s.getDeliveryMeasures},
		{http.MethodGet, "/api/v1/delivery/pipeline", authAny, "The Pipeline page: over the seven UTC days ending today (now, or ?at= for a fixed day), by the day each run started, the PR-checks workflow's run wall time and success, its five slowest jobs (50 timed results to rank) and the jobs that failed in the last 24 hours, the duration and result of the staging end-to-end, staging deploy and production deploy jobs the settings name in push runs on main, and merge-to-production and PR-open-to-merge per merge day against their targets; not filtered by the timeline's window or facets; 400 INVALID_QUERY for a malformed at, 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row. Its lead times, deploy job timings and PR checks count only the pull requests and runs the timeline's rule shows the caller.", s.getDeliveryPipeline},
		{http.MethodPost, "/api/v1/delivery/session-groups", authAny, "Create a Delivery session group {name, sessions}: the group becomes the Parent agent of every pull request whose first session it holds; 409 SESSION_IN_GROUP for a session another group holds, 409 SESSION_GROUP_NAME_TAKEN for a name another group has, 409 SESSION_GROUP_NAME_IS_SESSION_TITLE for a name that is the title of a session outside the group. The timeline lists every group whose every session the caller may see (session_groups).", s.createDeliverySessionGroup},
		{http.MethodPatch, "/api/v1/delivery/session-groups/{id}", authAny, "Rename a Delivery session group, add sessions to it and remove sessions from it {name?, add?, remove?}, with the create route's refusals; 400 SESSION_GROUP_INPUT for an edit that would leave it empty.", s.updateDeliverySessionGroup},
		{http.MethodDelete, "/api/v1/delivery/session-groups/{id}", authAny, "Delete a Delivery session group; its sessions show as themselves again.", s.deleteDeliverySessionGroup},
		{http.MethodGet, "/api/v1/ci-reconcile/passes", authAny, "The CI reconcile passes of the last 24 hours, newest first: each one's id, the server process that ran it, when it started and ended, what it did (graphql_queries, graphql_points, rest_requests, rest_not_modified, heads_over_rest, records, heads, heads_known) and where it left the cursor, which is where the pass after it takes the order up: the repository, from (the number naming the page of its open pull requests, one above the last head of the page before it), number (the first pull request of that page the pass did not finish, 0 for the whole page), rest (the page is read from the repository's REST listing rather than from a query) and counted (its heads are already among the heads the cycle knows the repository has). Both the counts and the cursor are null until the pass ends. state is running, complete, deadline (its own five-minute bound cut it short), rate_limit (a shared GitHub limit ended it), cancelled (its process shut down under it) or ended_without_completing, which is a pass whose process died before it could record its end. Rows are kept for 48 hours and pruned by the pass that ends. An open viewer reads the passes; a restricted member or a credential that lists projects is 403 DEPLOYMENT_READ_REFUSED.", s.listCIReconcilePasses},
		{http.MethodGet, "/api/v1/me/agent-tokens", authAny, "List the caller's personal agent tokens, each with the scopes it holds and the projects it lists, absent when it lists none. Each list holds only the projects the caller's own credential sees, so a token whose every project is hidden from the caller reads as one that lists none. At the person's own browser, projects_hidden is true on a token that lists a project the person no longer sees; it is absent everywhere else.", s.listAgentTokens},
		{http.MethodPost, "/api/v1/me/agent-tokens", authAny, "Mint a personal agent token {name, scopes?, projects?}: scopes is the set the new token holds, defaulting to the scopes the caller's own credential holds. A scope this deployment does not know, or approve without person, is 400 TOKEN_SCOPES_INPUT; a scope the caller does not hold itself is 403 OAUTH_SCOPE_REQUIRED. projects is the list of projects the token may use, at most 16, each one the caller sees; 400 TOKEN_PROJECTS_INPUT otherwise. It defaults to the caller's own credential's list, and a credential that lists projects gives a token only some of its own. The answer is the one time the token's plaintext is shown.", s.createAgentToken},
		{http.MethodPatch, "/api/v1/me/agent-tokens/{id}", authAny, "Set the scopes, and the projects, of one of the caller's personal agent tokens {scopes, projects?}, answering the updated token: scopes is the whole set it is to hold, with the mint's refusals. projects is the list of projects the token may use, at most 16, each one the caller sees; 400 TOKEN_PROJECTS_INPUT otherwise. Left out, the token keeps its list, and the answer holds only the projects of it the caller sees, with projects_hidden as the list answers it. A token of another person's, or none, is 404 NOT_FOUND.", s.updateAgentTokenScopes},
		{http.MethodDelete, "/api/v1/me/agent-tokens/{id}", authAny, "Revoke a personal agent token.", s.revokeAgentToken},
		{http.MethodGet, "/api/v1/users", authAny, "Everyone who has signed in, by lowercase email: the assignee picker's options. Only the people of the projects the caller sees, for a restricted member or a credential that lists projects.", s.listUsers},
		{http.MethodGet, "/api/v1/people/{login}/nodes", authAny, "One person's page: what they wrote and what reached them, newest first, read from the node and tuple store. Each row carries the node's kind and id, the relations that put it there (authored_by, delivered_to, or both), the first line of its text and the dashboard path it opens at. It takes the login and a page - limit (1-200, default 50) and offset - and no kind, filter or edge to walk. {person, rows, total, limit, offset}. Needs the person scope, so the deployment's shared token is 403 OAUTH_SCOPE_REQUIRED. A login the store holds no person for is 404 NOT_FOUND, the answer an address nobody holds gets; so is a person the caller may not see, or none of whose writes it may see, except the caller's own login.", s.listPersonNodes},
		{http.MethodGet, "/api/v1/whoami", authAny, "Who the server takes the caller for: {kind: user, login, scopes} (login is the person's lowercase email) or {kind: agent, owner, service, session, project, scopes} (owner is a personal token's owner or a broker-issued token's person by lowercase email, null for the shared token and for a broker-issued token that names a service; service is a verified service-account token's Kubernetes subject, or the fleet a broker-issued token naming no person belongs to, null for every other credential; session is the session the caller's credential proved, null for a credential that proves none, which every one but an access token the secrets broker issued is; project is the one project key that credential lets the caller write, null where it names none). scopes is the OAuth scopes the credential holds, an empty array for one that holds none: person acts as the person on every route the dashboard reaches, and approve settles what a person settles. projects is the list of projects the credential may use, sorted, absent when it lists none. private_projects lists those of projects the caller must treat as private: a private project, or one the credential no longer sees, sorted, absent when there is none. project is deprecated, and is the one key of a one-key list, null otherwise. A write records its actor with proven: true only where this credential proved that actor, the person at a browser or the session above, and never for a session a request names under a credential that proves none. member_kind is restricted for a restricted member, and absent for everyone else; a member kind that cannot be read is 503 IDENTITY_UNAVAILABLE.", s.whoami},
		{http.MethodGet, "/api/v1/dashboard/builds", authAny, "Which dashboard builds are served: {serving, live}, serving the entry chunk (/assets/index-*.js) of the build this process serves (empty when none), live the distinct entry chunks of the builds every live server process serves, sorted. A tab offers its reload notice only when live holds exactly one entry and it is not the tab's own.", s.getDashboardBuilds},
		{http.MethodGet, "/api/v1/priorities", authAny, fmt.Sprintf("Every live priority, in rank order: id, title, description (markdown of any length), owner, rank (its place in the one order), state (live; a deleted priority is gone and is not listed), and when and by whom it was made, each beside the work that advances it (work): by_status, the open issues in each lifecycle status, claimed, open_asks, closed_last_7_days and newest_event_at. The work under a priority is the issues that state the relation and every issue below one of them. Beside the list it answers the two faults: work_under_no_priority, the open issues in flight (in_progress, testing, needs_review, retro) that advance no live priority, and priority_work_parked, the issues that advance one and stand in backlog or icebox, each as a whole count, the same count by project or by priority, and the newest %d rows. Each row carries advances as the issue read does, one {id, inherited} entry per live priority the issue advances, inherited true for one it takes from an issue above it; a row of work_under_no_priority carries the empty list. A priority belongs to no project, so this read is of the whole deployment: an open viewer reads it, the shared token every pod carries included, and a restricted member or a credential that lists projects is 403 DEPLOYMENT_READ_REFUSED. Each count counts only the work of the projects the caller sees. ?project=<KEY>, repeatable, narrows the work counted and both fault lists to those projects and never the priorities themselves; ?summary=true answers the two fault counts alone, with no priority row and no fault row, which is what the navigation rail reads.", priorityFaultRows), s.listPriorities},
		{http.MethodPost, "/api/v1/priorities", authAny, fmt.Sprintf("Write one priority {id?, title, description?, owner?}: title is required, and a title with nothing in it or over %d characters (UTF-16 units, counted as an issue title is) is 400 PRIORITY_INPUT; id is the caller's and defaults to the title written as a slug, and an id that is no slug of lowercase letters, digits and single hyphens, one over %d characters, the ids none and any (the issue listing reads advances=none as the work under no priority and advances=any as the work under any), and a title no slug can be written from are each 400 PRIORITY_INPUT, an id derived from the title as much as one given; an id the store already holds is 409 PRIORITY_EXISTS. The new priority takes the last place of the order. A credential scoped to one project is refused, since a priority belongs to none.", contracts.IssueTitleMax, priorityIDMax), s.createPriority},
		{http.MethodPatch, "/api/v1/priorities/{id}", authAny, fmt.Sprintf("Write one priority's title, description or owner, and move its order {title?, description?, owner?, rank?}: each field named is written and each left out is left alone. rank is {before, after}, the priority this one lands above and the one it lands below, as the issue board's own drag names them; a rank naming neither, a neighbour that is this priority, and one no live priority carries (an id the store does not hold, or one whose state is gone) are each 400 RANK_INPUT. A title with nothing in it or over %d characters (UTF-16 units, counted as an issue title is, so a rename is held to the bound a create is) and a body naming nothing to write are 400 PRIORITY_INPUT, and an id no priority carries is 404 PRIORITY_NOT_FOUND.", contracts.IssueTitleMax), s.patchPriority},
		{http.MethodDelete, "/api/v1/priorities/{id}", authAny, "Delete a priority: its state is written gone and no row is removed, so the listing answers without it while its history still names who ended it and when. An id no priority carries is 404 PRIORITY_NOT_FOUND; 204 otherwise.", s.deletePriority},
		{http.MethodGet, "/api/v1/priorities/{id}/history", authAny, "One priority's own history, newest first: every act stated about it as {field, value, issue, asserted, actor, at}. field is what was stated — title, description, owner and rank, the four a person writes, state (live when it was made, gone when it was deleted) and visibility; advances is the relation an issue states on it. value is what an attribute write wrote and is empty on an advances act; issue is the issue at the other end of an advances act and is empty on an attribute write; asserted is false on the act that retracted an advances and true on every other. A priority belongs to no project, so an open viewer reads this, the shared token every pod carries included; a restricted member or a credential that lists projects is 403 DEPLOYMENT_READ_REFUSED. A deleted priority still answers its history, the delete its newest act; an id no priority carries is 404 PRIORITY_NOT_FOUND.", s.priorityHistory},
		{http.MethodGet, "/api/v1/issues", authAny, fmt.Sprintf("List issues; filters project, status (repeatable: the issues standing in any of those statuses; a blank value names none, and a value is matched as written, so a status the lifecycle does not hold matches no issue: alone it answers none, and beside another status it adds nothing), parent, label, priority (repeatable: 0-3, or none for unset), advances (repeatable: a priority id, none for the work that advances no live priority or any for the work that advances one, own or inherited; several are a union), open, updated_since, closed_since and closed_before (RFC3339; the issues closed at or after closed_since and before closed_before, so an open issue matches neither), route_status (live, no_holder or unknown; open issues only); ?pinned=true needs the person scope, since the pins are one person's. A malformed updated_since, closed_since or closed_before is 400 INVALID_UPDATED_SINCE, INVALID_CLOSED_SINCE or INVALID_CLOSED_BEFORE, and a closed_before at or before closed_since 400 INVALID_CLOSED_WINDOW. Each issue carries closed_at, when it was closed or null while it is open, and progress: {tasks, children}, its spec's task-list items and its direct children as {done, total}, each null when it has none. ?moved=true adds moved_at and moved_by to each issue standing in backlog or icebox: when it was moved there and who moved it, read from its newest issue.updated event that set one of the two statuses, as GET /api/v1/priorities' parked work reads them; an issue standing elsewhere, or whose events do not reach back to the move, carries neither, and a listing without moved=true carries neither on any issue. Pages with limit (1-%d) and offset (0 or more; alone it pages %d): a paged answer is {issues, total, limit, offset}, total counting every issue the filters match; without either it is every matching issue as an array. cursor is 400 INVALID_QUERY.", contracts.MaxIssuePageLimit, contracts.DefaultIssuePageLimit), s.listIssues},
		{http.MethodPost, "/api/v1/issues", authAny, fmt.Sprintf("Create an issue (native, or from a GitHub owner/repo#n ref); assignee defaults to the creating person, the personal token's owner, the parent's assignee, or the project's default assignee. A title over %d characters is 400 CAP_EXCEEDED. After the write commits, a related-item check searches the project by meaning and by its rarest words and waits at most 5 seconds; the response's advice.suggestions says whether it found related items (outcome found, none or not_made with the reason in missing), how it searched (search, search_note), the three items most like the new issue, one per owner, with each ask item's answer state, words to search with by hand (search_query), and under force the near copies forced past (title_matches), or the likely duplicate an unforced create was told (likely_duplicate).", contracts.IssueTitleMax), s.createIssue},
		{http.MethodGet, "/api/v1/issues/resolve", authAny, "Resolve ?ref=<KEY | owner/repo#n> to an issue key.", s.resolveIssue},
		{http.MethodGet, "/api/v1/issues/{key}", authAny, "Read an issue with its open asks, primary document, children, and progress: {tasks, children}, its spec's task-list items (`- [ ]` / `- [x]`, nested lists included) and its direct children as {done, total}, each null when it has none.", s.getIssue},
		{http.MethodGet, "/api/v1/issues/{key}/summary", authAny, "What a reference to an issue shows, read without the full issue: {key, project, title, status, documents}, documents every artifact of the issue in the order the issue read lists them, each {id, slug, name, kind, latest_version, primary}, primary whether the document is the issue's spec; 404 NOT_FOUND for no such issue.", s.getIssueSummary},
		{http.MethodPatch, "/api/v1/issues/{key}", authAny, fmt.Sprintf("Update title (over %d characters is 400 CAP_EXCEEDED), status, priority, labels (or labels_add, which adds to the labels the issue holds when the write commits rather than replacing them; not with labels), route, parent, rank, assignee (the email of a person who has signed in, or null; anyone may reassign), or components ({mode: inherit|explicit|none, ids, reason}; allowed on a closed issue). Closing the issue (status done) withdraws every open ask it holds, each recorded resolved with resolution kind withdrawn naming the closer; reopening it opens exactly those asks again. expected_sequence makes the write conditional: 409 ISSUE_CHANGED when the issue's last event is no longer that sequence.", contracts.IssueTitleMax), s.patchIssue},
		{http.MethodPost, "/api/v1/issues/{key}/claim", authAny, "Claim the issue for the calling session (or human): records who is working it. The status does not move. The answer is the issue with legion_managed_status beside it: true when the issue carries the legion label in any case, or sits under one that does, which means the Legion daemon owns the status and a status another writer sets is undone. 409 ISSUE_CLAIMED names a live holder (or a human one, whose claim has no session to end); a caller whose credential holds the person scope may pass {force: true} to take it anyway, and one that does not is 403 OAUTH_SCOPE_REQUIRED. 409 CLAIM_CONTENDED means the holder changed twice while the request ran, so nothing was applied and nobody's liveness was checked: read the issue and retry.", s.claimIssue},
		{http.MethodDelete, "/api/v1/issues/{key}/claim", authAny, "Release the issue's claim: its holder, a caller whose credential holds the person scope, or anyone when the holding session is no longer live. The status does not move. The answer is the issue with legion_managed_status beside it: true when the issue carries the legion label in any case, or sits under one that does, which means the Legion daemon owns the status and a status another writer sets is undone. 409 ISSUE_CLAIMED names the holder this caller may not release (there is no force on this route); 409 CLAIM_CONTENDED means the holder changed twice while the request ran, so nothing was applied and nobody's liveness was checked.", s.releaseIssueClaim},
		{http.MethodGet, "/api/v1/issues/{key}/events", authAny, "Page an issue's event log (after, before, ids, order, limit). The answer carries an ETag over the issue's event sequence and the paging query; If-None-Match answers 304. An event's actor carries proven: true when the write's credential proved it (a browser's person, a broker-issued token's session), and no proven when the request only named it.", s.listIssueEvents},
		{http.MethodGet, "/api/v1/issues/{key}/references", authAny, "The eight-hop closure of documents the issue's content references: {members, truncated}, each member a document in the closure with its depth and the node it was reached via, under an ETag (If-None-Match answers 304). ?versions=latest leaves each member's artifact.versions holding only its latest version, under its own ETag; absent, empty or all is the full answer; another value, or versions given more than once, is 400 INVALID_QUERY.", s.getIssueReferences},
		{http.MethodGet, "/api/v1/issues/{key}/collaborators", authAny, "Everyone who worked an issue, running or not, as {issue_key, people, agents, legion_registry}. people (kind user) is one row per canonical login: the assignee, everyone who claimed it, posted a comment, message, ask or document version on it or its documents, or answered one of its asks. agents (kind session) is one row per session id: the same event actors that are sessions (the Legion daemon's legion-daemon:<PROJECT> writer left out), merged with every session the listener's Legion agent registry records for the issue, which adds legion {role, role_token, started_at, replaced_by, replaced_at} and its title. Each row carries reasons (assignee, claimed, posted, answered) and last_at; no running state. legion_registry is unavailable when the listener did not answer, and the rest still answers.", s.getIssueCollaborators},
		{http.MethodGet, "/api/v1/references", authAny, "Edges of one node in the reference graph: ?to=<dispatch ref> backlinks or ?from= links; ?kind=, ?since=<events.id> (mentions only, read up to the event log's safe horizon: every mention committed before the read, unless an open transaction holds the horizon back past 1 s, when the read answers the mentions at or below it and a later read the rest). A caller with no cursor starts from since=0, which lists every mention committed before the read that carries a source_seq, and after that reads again from the highest source_seq an answer showed it. A mention written with no event (a rebuild, a backfill, an older build) carries no source_seq until a later write of its source stamps it with that write's event, and until then only a read without since lists it. A signed-in page may start from GET /auth/whoami's event_head instead, which takes a cookie or a trusted header and not a bearer, so a bearer caller starts from 0. Never take a since from max(events.id) read straight from the database: event ids are not commit order, and an id taken that way can stand above an edge that commits later. An empty answer is not proof that nothing is new, so the caller reads again with the id it holds. A ?to= reference whose item id or block id is written short (8 characters or more, never a whole uuid) names the one item of the owner the reference names that carries it: the issue of dispatch://<KEY>/ask/<prefix>, the document of a #b-<prefix>; the node answered carries the whole id, a prefix two of them carry is 409 ID_AMBIGUOUS listing both, and one nothing carries is the 404 the reference as written always gave. A whole id is read whatever owner the reference names, as it always was.", s.getReferences},
		{http.MethodGet, "/api/v1/references/questions", authAny, "The question map of one reference source's document: ?kind= one of artifact, ask, comment or message and ?id= that source's whole uuid or a unique prefix of 8 characters or more. Answers {document, questions}: document the uuid of the document the source's text belongs to - its own for a document version, the document an ask or a comment is written on or anchored to, its issue's spec for an issue message - and questions each `:::ask{#id}` block that document's latest version declares, by block id, with the whole id of the ask settlement made for it and that ask's `dispatch://` address, which is what a `#id` in the source's text names and the address its edge is stored under (empty for an ask on an agent conversation's document, which no address names). A block no ask is settled for yet is absent, and a document declaring none answers []. A source whose text belongs to no document - a direct message, an id nothing holds - is 404 NOT_FOUND; a kind that is none of the four is 400 INVALID_KIND; a missing id is 400 INVALID_QUERY; an id prefix two sources carry is 409 ID_AMBIGUOUS.", s.getReferenceQuestions},
		{http.MethodGet, "/api/v1/reference-targets", authAny, fmt.Sprintf("What many dispatch:// references point at, in one read: ?ref=<dispatch ref> once per reference, at most %d of them (past that 400 CAP_EXCEEDED naming the cap). {targets}, one member per ?ref= in the request's order (a reference named twice answered twice), each {ref, kind, name, gone}: ref as the request wrote it, kind the kind of node it names, name the words shown for the target in place of its text (an issue's title, a document's name, an ask's question, a comment's or a message's body, a component's title, a block's text), and gone whether nothing answers to that reference now, once an id it writes short is resolved, which carries no name. A caller that sent Dispatch-Trees: 1 is also given text_tree on each member whose kind is block: the block's whole text as the tree a reader is shown, links and references kept and nothing cut, a typed block's being of what stands between its opening line and its closing one. The lines between a typed block's fences are read by the block's kind: a document's block has the closing fence's indentation taken out of each line, which a block in a list item or a footnote definition carries, and a block of a comment, a message or an ask, stored from the start of its line, has its opening line's indentation, up to three spaces, taken off before the directive is read and up to as many columns taken off each later line, as its own text's parse reads them, the indentation left on a line, and the spaces or tabs after a list marker or a quote marker, written as spaces with each tab counted to the next multiple of four from its own column. With its opening line at column 0, such a block keeps every line as written; an opening line four spaces or a tab in opens no typed block, so that text is given the tree of its whole stored text. A typed block in a quote is given the tree of its whole stored text, its opening line included, and so is a block of a comment, a message or an ask whose closing fence stands four spaces or a tab in, since that fence closes nothing in its text. A reference the grammar cannot read is 400 INVALID_REFERENCE naming it; one that reads but names nothing is answered gone, never refused. An item id or a block id written short (8 characters or more, never a whole uuid) is resolved in the scope the reference names, as GET /api/v1/references resolves its ?to=, and ref still echoes it as written: a prefix that nothing carries is answered gone, and a prefix that two or more items carry answers that member gone with ambiguous: true, which no other member carries, and every other member is answered; GET /api/v1/references refuses such a prefix with 409 ID_AMBIGUOUS, and this read refuses none. Under an ETag over the whole answer, each tree and each ambiguous included (If-None-Match answers 304). This answers no edges: GET /api/v1/references reads one node's edges.", referenceTargetsMax), s.getReferenceTargets},
		{http.MethodGet, "/api/v1/block-links", authAny, "Links from blocks of one document to blocks of another: ?from=<document ref>&to=<document ref>; both block nodes, the edge kind, and when and in which version the source wrote each, newest first.", s.getBlockLinks},
		{http.MethodGet, "/api/v1/issues/{key}/subscribers", authAny, "Sessions subscribed to an issue's topics.", s.listIssueSubscribers},
		{http.MethodDelete, "/api/v1/issues/{key}/subscribers/{session_id}", authAny, "Unsubscribe a session from an issue.", s.unsubscribeIssueSession},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts", authAny, "List an issue's documents and files.", s.listArtifacts},
		{http.MethodPost, "/api/v1/issues/{key}/artifacts", authAny, fmt.Sprintf("Upload an issue document or file (JSON or multipart). A text/markdown upload is a document; any other is an image only when its bytes are a PNG, JPEG, GIF or WebP, stored under the type its bytes are whatever was declared, and otherwise a file under its declared type. A name no artifact has yet is 400 INVALID_ARTIFACT when it, or the reference key <owner>/<slug> it gives the new artifact, is over %d bytes; an upload under an existing artifact's name is never refused for its length.", maxNewArtifactKeyBytes), s.uploadArtifact},
		{http.MethodGet, "/api/v1/issues/{key}/results", authAny, "The checks of the issue's Verification document in document order, each with its attributes (when, surface, run, negative_control, ci_check), the version its words last changed in and their digest, and every result recorded against it from any issue, newest first: {issue, verification_artifact_id, checks}. A check's text is its words with every link and dispatch:// reference dropped; a caller that sent Dispatch-Trees: 1 is also given text_tree, the check's whole content as the tree a reader is shown, links and references kept. A check's results come newest first by created_at, the time the recording transaction began, with id descending breaking a tie, and a client reading one result per check reads the first. Two results recorded in transactions that overlapped can come back in either order. Each check carries compared_with, the commits and branches its results are compared with, in the order they are asked about. For a before_merge check that is every pull request head Dispatch holds for the check's issue, each {kind: pull_request, commit, pull_request, learned_at: {at, kind}}, the kind of time being read_at when Dispatch last read that pull request from GitHub (checked_at) and updated_at when the pull request's own update time, as its newest webhook event carried it (event_at), is the later of the two. For an after_merge check it is {kind: branch, branch} for the branch that issue's pull request merged into and for the repository's default branch, the default branch alone where Dispatch holds no merge of the issue. For a default_branch check it is the repository's default branch alone. Where that list is empty the check carries compared_nothing with its reason: no_pull_request_or_branch (the issue resolves to no repository, or a before_merge check's issue holds no pull request), repository_unreadable (Dispatch's GitHub App cannot read the repository) or read_failed (a read Dispatch makes did not answer). Each check also carries pass_stands {answer, roles}: yes with the roles whose passes stand, no, or unknown where the repository could not be read, and for a check CI proves whose check runs GitHub could not be read (ci state unread). A pass stands when its commit is at one of the compared commits, the check's words have not moved since it was recorded, and no retraction stands against it. Each result states two facts apart from each other. commit_stands {answer, basis, reason} is where its commit stands: at_basis with the basis it matched (for a check compared with branches, the branch that carries the commit), other_commit, or unknown with the reason (repository_unreadable, read_failed). check_words_moved is whether the check's words moved after the result was recorded, which the digests decide, and the result's own check_version stands beside the check's version. A result a retraction stands against carries retracted {reason, actor, created_at} and counts as no evidence. With no ?at=, each result also answers currency (current or older) where either word applies, for dispatch clients older than pi-envoy 17.1.0; read commit_stands, check_words_moved and retracted instead. A check CI proves is one whose ci_check names a GitHub check run, with when before_merge, no run, a negative_control, and no result recorded against it. It carries ci {run, state, case, detail, line}, read from that run at the head of the issue's open pull request of the highest number, else at the final head of its merged pull request of the highest number: state pass, fail, no_result or unread; case the conclusion that decided it (success, failure, timed_out, action_required, startup_failure, stale, or another conclusion as GitHub wrote it), or neutral, skipped, cancelled, not_completed, absent (no run of that name at the head), no_head (Dispatch holds no open or merged pull request head of the issue) or github_unreadable (state unread); detail one sentence naming the case, with GitHub's own answer where GitHub could not be read; line the pull-request body line for the check. Its one result is derived from that run on every read, for a pass or a fail alone, and is stored nowhere: id ci-run:<check run id>, role ci, actor {kind: github_app, id: the slug of the App that made the run}, session \"\", verdict pass or fail, commit_sha the head read, created_at the run's completed_at, observed the conclusion, the run's output title and summary cut to 2,000 runes, and its URL, one a line, check_sha256 the check's own, and no event_seq. It is compared as a recorded result is. ?at=<commit> replaces compared_with for every check of the issue by that one commit, {kind: named, commit}. The only read of GitHub it leaves is the check runs of a check CI proves, read at the issue's newest pull request head as on every read, so a derived result stands at the named commit only where that commit is that head. A value that is not 40 lowercase hex characters is 400 INVALID_QUERY. An issue with no Verification document answers no checks.", s.listCheckResults},
		{http.MethodPost, "/api/v1/issues/{key}/results", authAny, "Record one result of one check: {check, role, verdict, commit, surface?, run?, observed?, negative_control?, failed?, skipped?, excluded?, screenshots?}. {key} is the issue the result is recorded on; check names a check block of its Verification document or of one of its ancestors', by block id or, when a block id is not unique in that set, by its canonical reference (409 CHECK_AMBIGUOUS); any other id is 400 UNKNOWN_CHECK. A check CI proves (GET's definition: ci_check set, when before_merge, no run, a negative_control, and no result recorded against it) is 409 CHECK_PROVED_BY_CI for every role, naming the block and the check run, since its result is read from that run. role is at most 32 characters of a-z and -, verdict is pass or fail, commit is 40 lowercase hex, and the counts default to 0; a pass counting any failed, skipped or excluded case is 400 RESULT_CONTRADICTS. A commit the repository the check's issue resolves to does not hold is 400 RESULT_COMMIT_UNKNOWN, naming the commit and that owner/repo; a repository Dispatch cannot read, or none at all, stores the result as before. A screenshot names an image document of the recording issue or of the check's issue. The result is appended as result.recorded on the check's issue. A closed issue, recording or the check's, takes a result only for a check whose when is after_merge or default_branch; every other result there is 409 ISSUE_CLOSED. The answer is {result, line}, line being the pull-request body line the result implies, which carries no link to Dispatch.", s.recordCheckResult},
		{http.MethodPost, "/api/v1/results/{id}/retract", authAny, "Retract one recorded result, with the reason it is wrong: {reason, actor?}. {id} is the result's own id. The retraction is a record of its own: the result is neither updated nor deleted, and both records stay readable with the reason and the actor who wrote it. No surface counts a retracted result as evidence. Three callers are admitted and every other is 403 RETRACTION_FORBIDDEN: the session that recorded the result, a session holding the Dispatch claim of the issue the result was recorded on, and a caller whose credential carries a person, which is a personal token or a signed-in person's own session. A closed issue and a check of any when are retracted here, which is what recording refuses. A blank reason is 400 RETRACTION_INPUT, an id no result has is 404 RESULT_NOT_FOUND, and a result a retraction already stands against is 409 RESULT_RETRACTED. The answer is the retraction: {result_id, reason, session, actor, created_at, event_seq}. It is appended as result.retracted on the check's issue.", s.retractCheckResult},
		{http.MethodGet, "/api/v1/issues/{key}/coverage", authAny, "What each requirement of the issue's spec lacks: {issue, spec_artifact_id, has_plan, requirements, gaps}. A requirement is a `requirement` block of the spec; requirements lists them in document order, each with its reference, its words, and how many `check` and `plan_task` blocks of any document of the issue or of any descendant issue, at any status, name it in serves. A requirement's text is its words with every link and dispatch:// reference dropped; a caller that sent Dispatch-Trees: 1 is also given text_tree, the requirement's whole content as the tree a reader is shown, links and references kept. gaps is one line per gap - `<block id> has no check`, and `<block id> has no plan task` only once the issue has a plan document - or the single line `the spec marks no requirements` for a spec that marks none. Nothing is ever refused for a gap. This is not GET /api/v1/search/coverage, which reports how much of what meaning search ranks carries a vector.", s.getIssueCoverage},
		{http.MethodPost, "/api/v1/issues/{key}/messages", authAny, "Post an issue message; target and delivery address a session or role. A reply names the message it answers in in_reply_to, and one block of it in in_reply_to_block.", s.createMessage},
		{http.MethodGet, "/api/v1/issues/{key}/messages", authAny, "List an issue's message threads: every root message, oldest first, each with its deliveries and its replies.", s.listIssueMessages},
		{http.MethodGet, "/api/v1/issues/{key}/messages/{id}", authAny, "Read one message with its deliveries and replies.", s.getMessage},
		{http.MethodPost, "/api/v1/issues/{key}/handoffs/{phase}", authBearer, fmt.Sprintf("Record a session's handoff for one phase of an issue {actor, body}: the session names itself in actor (400 ACTOR_KIND otherwise; a cookie caller is 403 HANDOFF_FORBIDDEN), phase is a lowercase slug (%s, 400 HANDOFF_INPUT otherwise), and body is one JSON object as a string (400 HANDOFF_INPUT otherwise) of at most %d bytes (413 CAP_EXCEEDED), stored exactly as sent. The record is the phase's next version, from 1; a closed issue is 409 ISSUE_CLOSED. Answers 201 {id, issue_key, phase, version, author, created_at} and appends handoff.recorded {handoff_id, phase, version}, which never carries the body.", contracts.HandoffPhasePattern, contracts.DispatchHandoffBodyMax), s.createHandoff},
		{http.MethodGet, "/api/v1/issues/{key}/handoffs", authAny, "List an issue's handoffs, newest first, each {id, issue_key, phase, version, author, created_at} without its body; a closed issue's read too.", s.listHandoffs},
		{http.MethodGet, "/api/v1/issues/{key}/handoffs/{phase}/latest", authAny, "The newest handoff of one phase of an issue with its body, the exact text written; 404 HANDOFF_NOT_FOUND when the phase has none. A closed issue's read too.", s.getLatestHandoff},
		{http.MethodPut, "/api/v1/issues/{key}/design-gates", authAny, "Publish the documents this issue's design-gate steps record {gates}: each entry {step, artifact_id}, step one of decisions, criteria and spec and artifact_id the document that step's gate recorded, or [] for an issue whose gates record none. The deployment's daemon writes it and replaces the issue's whole set, appending no issue event and notifying nobody whether the set changed or not. Answers 200 {issue, gates} with the stored set in step order. A body without gates, a step that is none of the three, a step named twice, and an artifact_id that is empty are each 400 INVALID_ISSUE; an issue Dispatch does not know is 404 ISSUE_NOT_FOUND.", s.putIssueDesignGates},
		{http.MethodPut, "/api/v1/issues/{key}/role-holders", authAny, fmt.Sprintf("Publish the session that holds each Legion role of this issue {holders}: each entry {role, session}, role one of architect, planner, implementer, tester, reviewer and merger and session a session id of at most %d bytes, or [] for an issue where no session holds a role. The actor must be session legion-daemon:<project>, the key up to its first -, the session the Legion daemon of the issue's project writes as; any other actor is 403 ROLE_HOLDER_FORBIDDEN. Before the airlock a bearer caller names its own session, so any caller whose credential proves no session can name that one. The body is the issue's whole set and replaces the stored set under a lock of the issue row, so a session that it does not name holds no role of the issue, two writes at once store the set of the one that finishes last, and a pair that did not change keeps its recorded_at. The write appends no issue event, notifies nobody and takes a closed issue. Answers 200 {issue, holders} with the set this write stored, in workflow order, each {role, session, recorded_at}. A body without holders, a role that is not one of the six, a session that is not a session id or is over %d bytes, a role or a session named twice and an empty session are each 400 INVALID_ROLE_HOLDER, and nothing is stored; an issue Dispatch does not know is 404 ISSUE_NOT_FOUND. The session of an ended claim keeps its role until the daemon's next publication lands, and its process can still run in that window.", maxRoleHolderSessionBytes, maxRoleHolderSessionBytes), s.putIssueRoleHolders},
		{http.MethodGet, "/api/v1/issues/{key}/role-holders", authAny, "The session that holds each Legion role of this issue, as the Legion daemon last published it: {issue, holders}, each {role, session, recorded_at} in workflow order, [] when no session holds a role. A closed issue's read too. An issue Dispatch does not know is 404 ISSUE_NOT_FOUND.", s.getIssueRoleHolders},
		{http.MethodGet, "/api/v1/handoffs/{id}", authAny, "One handoff by id with its body, the exact text written; 404 HANDOFF_NOT_FOUND when there is none, 400 HANDOFF_ID_INPUT for an id that is not a uuid.", s.getHandoff},
		{http.MethodGet, "/api/v1/messages/{id}", authAny, "Read the conversation a message belongs to by any message id in it, issue-less or not: the thread root with its deliveries and every reply, oldest first. An issue thread follows the issue's rule. A direct (issue-less) thread is read whole by a credential holding the person scope, since the conversation is that person's own; every other credential names its session in ?session= and reads only one whose root targets that session or that it replied in (403 THREAD_FORBIDDEN), a guard against reading another session's conversation by mistake, not an authorization boundary. A direct conversation is read by its parties, and by an open viewer that may see every session in it; a restricted member or a credential that lists projects reads only its own. A thread the caller may not read answers as an id nobody holds, and the live socket sends its text by the same rule.", s.getMessageThread},
		{http.MethodGet, "/api/v1/messages/{id}/blocks", authAny, "A message's current blocks in body order: {item, indexed, blocks: [{id, type, text, ref, from, to, embedding}]}, item the message's graph node, from and to UTF-16 code units into its stored body, ref absent for a direct message's block (it has no address), embedding where meaning search stands on the block (embedded, pending, or none while no window covers it). Read under GET /api/v1/messages/{id}'s rule: a credential without the person scope reading a direct message names its session in ?session= (400 SESSION_REQUIRED, 403 THREAD_FORBIDDEN). While the message is owed a block index, indexed is false and blocks empty; the read writes nothing.", s.getMessageBlocks},
		{http.MethodPost, "/api/v1/messages/{id}/deliveries", authAny, "Retry delivering a targeted message.", s.createDelivery},
		{http.MethodPost, "/api/v1/messages/{id}/deliveries/{attempt}/accept", authBearer, "The attempt's session records that it took the message as its user's own turn; the session names itself in actor (403 ACCEPT_FORBIDDEN for another's attempt). One compare-and-set: only a direct message to that session, on no issue, neither a broadcast's copy nor a reply in a broadcast's thread, in a thread whose root targets it (409 ACCEPT_NOT_DIRECT), that a person wrote (409 ACCEPT_NOT_WRITTEN_BY_PERSON), sent as a Send or an Aside (409 ACCEPT_NOT_ASIDE_OR_STEER); only the message's latest attempt (409 ACCEPT_SUPERSEDED), when no attempt of it was accepted before (409 ACCEPT_ALREADY_ACCEPTED), which a person requested (409 ACCEPT_NOT_REQUESTED_BY_PERSON), that person being the message's author (409 ACCEPT_NOT_REQUESTED_BY_AUTHOR), which did not fail (409 ACCEPT_FAILED; a pending, sent or queued attempt is taken), within the last minute (409 ACCEPT_STALE). Answers the attempt with accepted_at and accepted_as and the message's stored body; leaves the attempt's state to the send, and appends message.accepted.", s.acceptDelivery},
		{http.MethodPost, "/api/v1/messages/{id}/reply", authBearer, "The targeted session's reply to a delivery; the session names itself in actor. Once the attempt is answered, ?follow_up=true posts other text as the session's follow-up, threaded under its first reply; with follow_up=false or absent, or with text the session already posted there, nothing is posted and the stored message comes back marked duplicate. Any other follow_up is 400 MESSAGE_INPUT.", s.replyMessage},
		{http.MethodGet, "/api/v1/inbox", authAny, "Open asks waiting on the caller, grouped by whose turn it is; ?project= and ?assignee=me|unassigned|<email> filter on the person each ask waits on, which is three steps resolved as the ask is read: the person the question was handed to, then the issue's assignee, then the project's default assignee (unassigned is every ask nobody waits on; an email nobody has signed in with is 400 ASSIGNEE_NOT_ALLOWED).", s.listInbox},
		{http.MethodGet, "/api/v1/me/answers", authAny, "The caller's own answers and replies on asks across issues and project documents, newest first: each row is an answer (the ask.answered event's answer, and whether it is still the ask's current one) or a reply, with the ask's question, state, owner and deep link; pages with limit (1-200, default 50) and offset; {rows, total, limit, offset}.", s.listMyAnswers},
		{http.MethodGet, "/api/v1/me/replies", authAny, "The caller's Inbox threads, of the kinds ?kind= names: reply (the default) lists the threads they wrote in - an ask, a comment thread, an issue message thread - that somebody else has written in, less an open ask on an open issue that waits on them, which the Inbox already lists; agent lists their one-off messages to an agent session (a BTW, a broadcast's copy, or a Send or Aside the session did not take as a turn of its live conversation) holding a reply from that session; reply,agent lists both in one order. ?state= is unread (the default), which lists the rows somebody else has written in since they read them, newest reply first; inbox, every row in their Inbox, read or not, dismissed ones aside, newest reply first; or dismissed, what they have dismissed out of it, newest dismissal first, 200 rows at a time from ?cursor= (the next_cursor of the page before it; limit and offset are 400 INVALID_QUERY there, and cursor is 400 INVALID_QUERY under any other state). Each row names the thread, its owner, its issue's priority, its project, the thread's subject, its root's deep link (an agent row's is the session's live view), the newest reply's author, first line and time, the caller's read mark, whether it is unread and how many writes in it they have not read, the root's document anchor when it has one, its dismissal mark when it has been dismissed, and, on an agent row, the session and the ids of its unread replies. ?issue= or ?artifact= narrows to one owner (both is 400 INVALID_QUERY), and an agent row, which has neither, is then in no answer. An absent limit answers every row, as the Inbox reads them; a named limit is 1-200 (limit=0 is 400 INVALID_QUERY) from ?offset=. {rows, total (both kinds, and under state=dismissed the page the read answers rather than the whole pile, which nothing counts), limit (0 when none was named), offset, as_of, next_cursor (a dismissed page that filled)}: as_of is the database time of the one read-only repeatable-read snapshot every row was read in, which is what a mark-all passes back as its read mark.", s.listMyReplies},
		{http.MethodPut, "/api/v1/me/threads/{id}/read", authAny, "Move the caller's read mark on one thread forward to {read_through} (RFC3339; stored as no later than now; more than a minute ahead of the server is 400 INVALID_STATE). It only moves forward; a thread the caller never wrote in writes nothing; 204 either way. A non-uuid id is 400 THREAD_ID_INPUT.", s.putThreadRead},
		{http.MethodPost, "/api/v1/me/inbox/read", authAny, "Mark every unread Inbox thread read in one write: {read_through} (RFC3339, required; stored as no later than now; missing, unparsable or more than a minute ahead of the server is 400 INVALID_STATE), with the Inbox's own narrowing - kind (reply, agent; both by default), priority (P0, P1, P2, P3 and none), urgency (low, med, high, blocking and none), project (a project key and none), and issue or artifact - each absent list meaning every value. It marks exactly the threads GET /api/v1/me/replies?state=unread answers under that narrowing whose newest reply is at or before read_through: a thread written in since stays unread, and one already read past the mark keeps its later mark. Marking read takes no thread out of the Inbox; POST /api/v1/me/inbox/dismiss does. An Agent row has no issue, document, project, priority or urgency, so any of those narrowings leaves it out; a Reply row has no urgency either, so an urgency list without none marks nothing. An agent reply is marked read by its id, so no session's own read mark moves and the live conversation keeps what it had. issue with artifact, an unknown kind, priority or urgency value, and an artifact that is not a document id are 400 INVALID_QUERY. Answers {marked}, the number of threads marked; a request that moves nothing answers 0 and appends no event.", s.markInboxRead},
		{http.MethodPost, "/api/v1/me/inbox/dismiss", authAny, "Take threads out of the caller's Inbox, or put them back: {threads} (the row ids reply:<thread id> and agent:<root message id>) with {seen_replies}, or the Inbox's own narrowing - kind, priority, urgency, project, and issue or artifact, as POST /api/v1/me/inbox/read reads them - with {dismiss_through} (RFC3339, required with the narrowing; stored as no later than now; missing, unparsable or more than a minute ahead of the server is 400 INVALID_STATE), never both (400 INVALID_QUERY). seen_replies is what the page drew in each row threads names, keyed by that same row id, and a dismissal naming threads without it is 400 INVALID_QUERY: a reply: row's entry is {version}, that row's version as GET /api/v1/me/replies answered it, and an agent: row's is {newest_reply, unread_replies}, that row's reply.id and the ids it drew as unread. The mark is written only while the row still holds what the page drew, so a dismissal buries no write nobody has read. A restore takes no seen_replies, and neither does the narrowing, where the server draws the rows itself (each 400 INVALID_QUERY). With the narrowing it changes exactly the threads GET /api/v1/me/replies answers under it whose newest reply, or whose dismissal, is at or before dismiss_through. {restore: true} clears the marks instead of writing them, so the threads are in the Inbox again. Dismissing marks a thread read as well, so a dismissed thread never counts as unread, and a later write by somebody else clears the mark and brings the thread back unread, while the caller's own reply leaves it dismissed. An Ask row's id, a row id of neither shape and an id that is not a uuid are 400 INVALID_QUERY. Answers {dismissed}, the number of threads it changed; a request that changes nothing answers 0 and appends no event. It takes the lock order and appends the events POST /api/v1/me/inbox/read does: one user_thread_state.updated per project and one user_agent_state.updated per session it touched.", s.dismissInbox},
		{http.MethodGet, "/api/v1/search", authAny, searchDescription(searchDeclaresFour), s.search},
		{http.MethodGet, "/api/v1/search/results", authAny, searchDescription(searchDeclaresNine), s.search},
		{http.MethodGet, "/api/v1/search/coverage", authAny, "Meaning search's coverage {identity, place, model, total, kinds, not_indexed}: the configured embedder's identity, its place (bedrock, azure, or dev for a local harness) and model, all three null with none configured; then, in total and for each kind (issue, document_block, comment_block, message_block, ask_part), the searchable items, how many carry a current vector from that identity, how many lack one, how many failed for good, and how many seconds the oldest lacking item has waited (null when none lacks). not_indexed names every kind of node neither leg of search holds a row of - component, session, document_version, result and handoff - each {kind, items}, items being how many of it the database holds, counted from the table that holds it; such a kind is named rather than left out, so a reader never reads it as a covered kind with nothing lacking, and it is in no total.", s.searchCoverage},
		{http.MethodGet, goingCataloguePath, authAny, "The catalogue of operations an agent may propose on Dispatch's graph: twenty of the twenty-one dispatch tools that write, of the thirty-five tool specs agents operate Dispatch with, each carried whole - its own name, its own description, the arguments it takes with an example call, the rows of this table it writes, and the graph relations its verbs write. The one it leaves out is dispatch_review, on purpose: a review records an approval or a changes request in a person's name, and the loop that proposes operations must never propose one. Twenty actions is a list read whole, so the route takes no query and serves no page. An action the catalogue does not hold is no action at all: nothing serves it.", s.getGoingCatalogue},
		{http.MethodGet, "/api/v1/related-checks", authAny, "Count the related-item checks recorded when an issue, an ask or a new decision block was written, within [from, to) (RFC3339, each optional) and of ?project= and ?source_kind= (issue, ask or decision_block; any other is 400 INVALID_QUERY) when given: {project, source_kind, from, to} echo the filters (null when absent), total, by_source_kind, by_outcome (found, none, not_made), by_search (meaning_and_keywords, keywords_only, not_made), by_reason (each reason a check was not made or searched by keywords only), completed (found or none), skipped_for_time (reason timeout), other_project_offers, and keyword_ms and total_ms each {count, p50, p95, max, over_300ms}: the checks holding that time, its discrete percentiles and largest value (null with no check), and how many took over 300 ms. other_project_offers is how many ask creates of the window offered at least one answered question of another project (other_projects), a count of writes and not of items, read from write_suggestions over the same window and under the same filters, and zero for a source kind no such row carries; the two records are written at different moments and only the check's own row is retried, so a count at a window's edge can name one fewer write than the window's total. The meaning_ms recorded for such a check holds the whole meaning stage, the wait for the other projects' answers with it, which is at most 300 ms after the asking project's own query answered.", s.getRelatedChecks},
		{http.MethodGet, "/api/v1/near-copy-checks", authAny, fmt.Sprintf("List one project's near-copy gate answers, one row for each create the gate answered (an external create runs no gate and has none), recorded when the create was refused or once it was written: the rows of ?project= (required; 400 INVALID_PROJECT when it is not a key) within [from, to) (RFC3339, each optional; 400 INVALID_QUERY when from is not before to), oldest first, at most ?limit= (1 to %d, default %d), after the row whose id ?after= names (400 INVALID_QUERY when it is no row of the project). A window should end some time before now: a row written in its create's transaction holds that transaction's start as created_at and is seen only once the transaction commits, so a reader that pages up to now and resumes with ?after= can miss it. {project, from, to} echo the query (null when absent), checks are the rows, and next_after is the id to pass as ?after= for the next page, null on the last. A row is {id, created_at, project, actor_kind, actor_id, outcome, reason, force, title, spec_sha256, parent, issue, candidates, refuse_score, nudge_score, build_commit}, with no spec: GET /api/v1/near-copy-checks/{id} reads one row with it. outcome is refused (the gate refused it and nothing was written; every 409 POSSIBLE_DUPLICATE has this row, written before the answer), forced (written past a refusal with force), nudged (written, its closest open issue named as a likely duplicate), passed (written with nothing named) or not_made (no check that could stop the create was made, and it went in unchecked); reason is bound, failed or unrecorded for not_made and null otherwise; force is whether the create asked for it; spec_sha256 is the sha256 of the spec as sent, on every row; issue is the key written, null for a refusal; candidates are the issues the gate read at or above nudge_score, at most %d, best first, each {key, title, score}; refuse_score and nudge_score are the build's bars and build_commit the commit it was built from, null for an unstamped build. A refusal is answered only once its row is known to be written: otherwise it refuses nothing, and the create goes in as not_made, reason unrecorded, writing its row in its own transaction. The row of every other create that went in is written after the commit, on a context of its own bounded to %s, and tried once more in the background for up to %s when a timeout or a lock stopped it, so one whose row still could not be written has none and is otherwise unchanged.", nearCopyChecksPageMax, nearCopyChecksPageMax, duplicateCandidateLimit, nearCopyRecordTimeout, relatedRecordRetryTimeout), s.getNearCopyChecks},
		{http.MethodGet, "/api/v1/near-copy-checks/{id}", authAny, "One near-copy gate answer by id: the row GET /api/v1/near-copy-checks lists, with spec, the spec as sent, on a refused row alone (absent on every other, whose issue holds it; empty for a refused create that sent none); 404 NEAR_COPY_CHECK_NOT_FOUND when there is none, 400 NEAR_COPY_CHECK_ID_INPUT for an id that is not a uuid.", s.getNearCopyCheck},
		{http.MethodPost, "/api/v1/going/requests", authAny, fmt.Sprintf("Ask a question of what Dispatch holds {words, project}: records the question and answers 201 with its run - {id, author, project, words, created_at, ends_at}, whose ends_at is the %s the run has to answer in - and answers it behind that, each frame of the answer stored for GET /api/v1/going/requests/{id}/stream to carry. words is required (400 GOING_INPUT when it is blank) and at most %d characters (400 CAP_EXCEEDED); project is the project key the question is asked of, empty asking of every project (400 INVALID_PROJECT when it is not a key), and a credential scoped to one project asks only of that project. The run belongs to the credential that asked it, which the stream compares its reader with, so a credential that names nobody - the deployment's shared token - is refused 403 %s and records nothing: no reader of that run could ever be served it. With no model configured the run is recorded all the same and its stream carries the error frame %s, so a reader learns that from the stream, where every other failure of a run is reported, and not from a status of this route's own.", contracts.GoingRunWindow, contracts.GoingWordsMax, codeGoingAskerNamesNobody, contracts.GoingErrorNoModelConfigured), s.createGoingRequest},
		{http.MethodGet, "/api/v1/going/requests/{id}/stream", authAny, fmt.Sprintf("One run's answer as server-sent events: each frame is `id:` its sequence, `event:` its name (%s, %s, %s, %s, %s, %s, %s) and `data:` the JSON that frame carries, flushed as it is written, and the stream ends on %s or %s. Last-Event-ID resumes after that sequence, so a reader that drops rereads nothing. Any pod serves it, since the frames are rows and not the memory of the process that took the POST: a run past its ends_at that stored no ending is ended by the reader itself, with the error %s, which is how a stream ends when the pod that took the POST is gone. A run the record does not hold, an id that is not a uuid, and a run that is another caller's are one answer, 404 %s, so an id tells a caller nothing it did not already know.", contracts.GoingEventText, contracts.GoingEventItem, contracts.GoingEventCut, contracts.GoingEventChanged, contracts.GoingEventNone, contracts.GoingEventEnd, contracts.GoingEventError, contracts.GoingEventEnd, contracts.GoingEventError, contracts.GoingErrorAnswerDidNotArrive, codeGoingRequestNotFound), s.streamGoingAnswer},
		{http.MethodGet, "/api/v1/going/capability", authAny, "Whether this deployment has a text model configured: {model_configured} and nothing else, so no endpoint, deployment name or credential can appear in it. Any caller Dispatch admits reads it, a pod's own agent token included.", s.getGoingCapability},
		{http.MethodGet, "/api/v1/agents", authAny, "Live sessions with roles, capabilities, open asks, and last activity, from Dispatch's copy of the Envoy listener's session list, at most 5 s old. Only the sessions the caller may see: a session of a project it sees, or its own.", s.listAgents},
		{http.MethodGet, "/api/v1/agents/{session_id}/answers", authAny, "What people wrote that Dispatch delivered to one session, newest first by at, then by id: each ask's current answer (replaced when it changed an earlier answer), each comment at its current body (edited_at when edited; a resolved comment stays), each message, and each ask a person declined to answer, once however it arrived. It reads the ask.answered, comment.created, message.created, message.answered and ask.resolved events a person wrote whose published_destinations hold the session's own topic, the session's message deliveries the listener took (targeted and broadcast messages, handed to the running session or queued for it) and its comment deliveries (mentions) in those same two states; of an ask's resolutions it lists the decline alone, and it never lists an agent's or the system's words, a retraction, a withdrawal, another resolution, a hand-back, or a delivery to a role: route's topic. Each item names its relation to the session (mentioned_you over replied_on_your_ask over replied_in_a_comment_thread over commented_on_a_document over commented_on_an_issue for a comment; replied_to_your_message over sent_you_a_message for a message; answered_your_ask for an answer; declined_your_ask for a decline), the ask and its whole question on an answer, on a decline and on a reply in an ask's thread, its owner, dispatch:// ref and href (null owner and ref, and /agents/<session_id>/live, for a direct message). A decline carries a kind of its own, declined, at the resolution's own time, its id the ask's, and its words in two parts: text, the person's own note, empty when they wrote none, and dispatch_words, the sentences Dispatch wrote for the asking agent; nobody chose an option, so it carries no selected. ?since= (RFC3339, default 72 hours ago; older than 7 days or in the future is 400 INVALID_QUERY) starts the window; ?max_chars= (1-100000, else 400 INVALID_QUERY) stops the list after the item whose words take the running sum past it, counting the UTF-16 units of text and, on a decline, of dispatch_words beside it, while total counts every item in the window; {session_id, since, total, items}. A session_id that is not a session id is 400 INVALID_SESSION_ID, as on the agent artifact routes. A caller reads the list of a session it may see, and only the answers of projects it sees and of its own direct conversations.", s.listSessionAnswers},
		{http.MethodGet, "/api/v1/agents/{session_id}/messages", authAny, "A session's targeted messages, newest first.", s.listAgentMessages},
		{http.MethodPost, "/api/v1/agents/{session_id}/messages", authAny, "Send an issue-less targeted message to a session: {body, delivery, in_reply_to?, connected_only?}, answered once its own delivery attempt has settled, on past a client that gives up. A session the Envoy listener knows and that is not running is a recipient: the listener stores the message and hands it over when that session runs again, and the attempt is recorded queued rather than sent. A session the listener knows nothing about (\"no live session <id>\") is queued too: Dispatch publishes the message on that session's own subject itself, and the attempt fails with \"no live session <id>\" only where Dispatch has no NATS to publish on or the publish fails. connected_only asks for a session that is running now and fails the attempt with \"the session is stopped\", nothing stored; every later attempt of the message asks what its first asked. A model or thinking switch the session would run as its own turn - a person's own Send or Aside, not one an agent wrote for them, whose whole body is one /model or /effort line, which would otherwise apply at an unknown later time - is held to a running session at every send whether or not the request set connected_only. Under an Idempotency-Key header (as every POST and PATCH takes one), a repeat with the same request answers 201 with the message the key made and its deliveries as they stand, Idempotency-Replayed: ?1, first sending the one attempt it still owes (none yet, or one stranded past its claim) and nothing else; a listener it cannot reach is recorded on the attempt, so a repeat is answered while the listener is down. An attempt a stopped process left owed is resumed by the server's delivery sweep whether anyone asks again or not.", s.createAgentMessage},
		{http.MethodPost, "/api/v1/agents/{session_id}/artifacts", authAny, "Upload a file or image to an agent's conversation (multipart, as an issue upload, its kind read from its bytes): ref_key agent/<session_id>/<slug>, addressed dispatch://agent/<session_id>/artifact/<slug>. It holds files and images only: a markdown document (JSON content or a text/markdown file) is 400 ARTIFACT_INPUT, since a document belongs to an issue or a project. No event is appended. A caller whose credential proves a session of its own uploads only into that session: any other session_id is 403 SESSION_SCOPE. A session_id that is empty or holds /, ?, #, whitespace, a control character or a character that ends a reference in text (<>\"'`[]|) is 400 INVALID_SESSION_ID on every agent artifact route.", s.uploadAgentArtifact},
		{http.MethodGet, "/api/v1/agents/{session_id}/artifacts/{slug}", authAny, "Read an agent's conversation's file or image and its versions by slug.", s.getArtifact},
		{http.MethodGet, "/api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}", authAny, "Download one version of an agent's conversation's file or image, as the issue form serves it: its bytes under its own content type, with Content-Disposition: attachment naming the file's name, -v<number> before the extension for an older version (report-v2.pdf), and Cache-Control: private, max-age=31536000, immutable. With ?download=true the answer carries Content-Disposition: attachment with that name and Cache-Control: private, no-cache. A conversation holds no document, so no version here answers Markdown.", s.getArtifactVersion},
		{http.MethodPost, "/api/v1/broadcasts", authAny, "Send one message to many sessions: {body, delivery, session_ids}, one targeted message per recipient under a shared broadcast, answered once it is committed while each recipient's delivery runs behind the answer. A selected session the Envoy listener knows and that is not running is a recipient like any other, its copy stored for it and its attempt recorded queued; one the listener knows nothing about, or that does not advertise the mode, is excluded and named in the response, never switched to another mode. Under an Idempotency-Key header (as every POST and PATCH takes one), a repeat with the same request answers 201 at once with the broadcast the key made, every recipient's attempts as they stand and the first answer's exclusions, Idempotency-Replayed: ?1, and puts every recipient it still owes on its way behind the answer. A recipient a stopped process left owed is resumed by the server's delivery sweep whether anyone asks again or not.", s.createBroadcast},
		{http.MethodGet, "/api/v1/broadcasts", authAny, "Recent broadcasts, newest first, with their recipient and reply counts. Only broadcasts the caller wrote, or whose every recipient it may see.", s.listBroadcasts},
		{http.MethodGet, "/api/v1/broadcasts/{id}", authAny, "One broadcast with every recipient's delivery attempts and replies, recipients in the order the send named them; a broadcast from before that order was stored falls back to created_at, id. Only broadcasts the caller wrote, or whose every recipient it may see.", s.getBroadcast},
		{http.MethodPost, "/api/v1/issues/{key}/asks", authAny, "Open a question ask on an issue (options optional). After the write commits, a related-item check searches the project for the question and waits at most 5 seconds; the response's advice.suggestions says whether it found related items (outcome, with the reason in missing when not made), how it searched, and the three items most like it with each ask item's answer state.", s.createAsk},
		{http.MethodGet, "/api/v1/issues/{key}/asks", authAny, "List an issue's asks, each with its newest reply; ?state= filters, and ?thread=1 adds each ask's thread (replies, rewordings, answers, followers, claimant, route, route_holder).", s.listIssueAsks},
		{http.MethodGet, "/api/v1/issues/{key}/decisions", authAny, "The issue's decision records: {document, open_questions, decisions}. document is the dispatch:// address of its decisions.md, null (with no decisions) when it has none. open_questions are the issue's open asks whose block is in its spec, oldest first. Each decision is one `##` section of decisions.md in document order - a `###` section under `## Replaced` for a replaced one - with its heading block and markdown, the ask ids its blocks mention (kind person when it has any, agent otherwise), whether a paragraph of it starts `Unsure:`, the version its text last changed in, the Keep a person recorded, and one entry per link from its blocks to a spec block, each stale when the decision changed after it first linked that block and the block has not changed since.", s.getIssueDecisions},
		{http.MethodGet, "/api/v1/asks/open", authAny, "Open asks in one scope, with counts: ?author_session= for one session's own, or ?project= for every open ask on a project's issues and documents; exactly one is required.", s.listOpenAsks},
		{http.MethodGet, "/api/v1/asks/{id}", authAny, "Read one ask with its replies, edit history, answers and followers, plus claimant (the session holding its issue's claim) and route (its issue's route), whom a person's answer and replies also reach, and route_holder (the live session that route reaches now: the session itself or the role's holder); each is null when absent, route_holder also when nobody holds the route or the Envoy listener did not answer, and all are null on a project document's ask.", s.getAsk},
		{http.MethodGet, "/api/v1/asks/{id}/followers", authAny, "List the sessions an ask's answer and replies reach.", s.listAskFollowers},
		{http.MethodPut, "/api/v1/asks/{id}/followers/{session_id}", authAny, "Follow an ask (a credential holding the person scope for any session; every other for its own session alone).", s.followAsk},
		{http.MethodDelete, "/api/v1/asks/{id}/followers/{session_id}", authAny, "Unfollow an ask (a credential holding the person scope for any session; every other for its own session alone).", s.unfollowAsk},
		{http.MethodPatch, "/api/v1/asks/{id}", authAny, "Reword an open ask (its author, or a credential holding the person scope); a block ask's `:::ask` block is rewritten with it.", s.editAsk},
		{http.MethodPut, "/api/v1/asks/{id}/asked-of", authAny, "Hand an open ask to another person {asked_of}, the email of somebody who has signed in (400 ASSIGNEE_NOT_ALLOWED otherwise): the ask waits on them from now on, wherever its issue goes, and the Inbox lists it under them. It needs the person scope, since handing a question on is one person asking another. An ask that is not open is 409 ASK_NOT_OPEN and one whose issue is closed 409 ISSUE_CLOSED. Naming the assignee of the ask's own issue empties the field instead, so the ask follows its issue again; either way the row records who handed it on and when (asked_of_by, asked_of_at), and one ask.asked_of event names the person it waited on before and the one it waits on now. Answers the ask.", s.putAskAskedOf},
		{http.MethodPost, "/api/v1/asks/{id}/answer", authAny, "Answer an ask with selected options and/or text.", s.answerAsk},
		{http.MethodPost, "/api/v1/asks/{id}/resolve", authAny, "Retract or resolve an ask without answering it (kind retracted or resolved; withdrawn is written only by an issue's close, and an ask on a closed issue is 409 ISSUE_CLOSED); a block ask's block records it too. A person who will not answer declines instead, on a route of their own, POST /api/v1/asks/{id}/decline.", s.resolveAsk},
		{http.MethodPost, "/api/v1/asks/{id}/decline", authAny, "Decline to answer an ask: the person closes it with no answer, and Dispatch writes beside their note the words that tell the asking agent to decide without one. The note is optional, and at most the event body limit (400 CAP_EXCEEDED); the ask resolves with the kind declined, a block ask's block records it as every other resolution does, and an approval request is 409 ASK_NOT_DECLINABLE, being answered with Approve or Request changes.", s.declineAsk},
		{http.MethodGet, "/api/v1/issues/{key}/comments", authAny, "List an issue's comments; ?artifact= scopes to a document. The answer carries an ETag, which moves for every write the list shows, a delivery attempt that appends no event included; If-None-Match answers 304. A comment's author carries proven: true when the write's credential proved it (a browser's person, a broker-issued token's session), and no proven when the request only named it.", s.listComments},
		{http.MethodPost, "/api/v1/issues/{key}/comments", authAny, "Comment on an issue, a document quote, or an ask. A reply names the comment it answers in reply_to, and one block of it in reply_to_block; with ask_id in place of reply_to, reply_to_block names one part of that ask.", s.createComment},
		{http.MethodPost, "/api/v1/comments/{id}/resolve", authAny, "Resolve a comment thread.", s.resolveComment},
		{http.MethodPost, "/api/v1/comments/{id}/reopen", authAny, "Reopen a resolved comment thread.", s.reopenComment},
		{http.MethodGet, "/api/v1/comments/{id}", authAny, "Read one comment with its thread.", s.getComment},
		{http.MethodGet, "/api/v1/comments/{id}/blocks", authAny, "A comment's current blocks in body order: {item, revision, indexed, blocks: [{id, type, text, ref, from, to, changed_revision, changed_at, embedding}]}, item the comment's graph node, revision its indexed revision, from and to UTF-16 code units into its stored body, changed_revision and changed_at the revision its block's text last changed in, embedding where meaning search stands on the block (embedded, pending, or none while no window covers it). While the comment is owed a block index, or was indexed from another body than it stores, indexed is false and blocks empty; the read writes nothing. An id written short - 8 characters or more of the hex digits and hyphens an id starts with - names the one comment that carries it, is 409 ID_AMBIGUOUS where two or more do, and is 404 NOT_FOUND where none does; any other non-uuid id is 400 COMMENT_ID_INPUT.", s.getCommentBlocks},
		{http.MethodPost, "/api/v1/comments/{id}/deliveries", authAny, "Retry a comment mention delivery; target is required when it has multiple mentions.", s.createCommentDelivery},
		{http.MethodPost, "/api/v1/comments/{id}/reply", authBearer, "The mentioned session's reply to a delivery; the session names itself in actor.", s.replyComment},
		{http.MethodPost, "/api/v1/comments/{id}/accept", authAny, "Accept a suggestion, applying its replacement.", s.acceptComment},
		{http.MethodPatch, "/api/v1/comments/{id}", authAny, "Edit a comment's body. The dashboard editor also sends blocks ([{id, from}], from in UTF-16 code units into the new body) and base_revision, the revision GET /api/v1/comments/{id}/blocks gave, so each block keeps its id; without them the ids follow the text.", s.editComment},
		{http.MethodPost, "/api/v1/comments/{id}/reject", authAny, "Reject a suggestion.", s.rejectComment},
		{http.MethodGet, "/api/v1/artifacts/{id}/asks", authAny, "List a document's asks.", s.listArtifactAsks},
		{http.MethodPost, "/api/v1/artifacts/{id}/asks", authAny, "Open an ask on a document. After the write commits, a related-item check searches the document's project for the question and waits at most 5 seconds; the response's advice.suggestions says whether it found related items (outcome, with the reason in missing when not made), how it searched, and the three items most like it with each ask item's answer state.", s.createArtifactAsk},
		{http.MethodGet, "/api/v1/artifacts/{id}/comments", authAny, "List a document's comments. The answer carries an ETag, which moves for every write the list shows, a delivery attempt that appends no event included; If-None-Match answers 304.", s.listArtifactComments},
		{http.MethodPost, "/api/v1/artifacts/{id}/comments", authAny, "Comment on a document or one of its quotes.", s.createArtifactComment},
		{http.MethodGet, "/api/v1/artifacts/{id}/events", authAny, "Page a document's event log.", s.listArtifactEvents},
		{http.MethodGet, "/api/v1/artifacts/{id}/references", authAny, "Cross-references to and from a document.", s.getArtifactReferences},
		{http.MethodGet, "/api/v1/artifacts/{id}/subscribers", authAny, "Sessions subscribed to a document's topics.", s.listArtifactSubscribers},
		{http.MethodDelete, "/api/v1/artifacts/{id}/subscribers/{session_id}", authAny, "Unsubscribe a session from a document.", s.unsubscribeArtifactSession},
		{http.MethodGet, "/api/v1/artifacts/{id}", authAny, "Read a document's metadata and current version.", s.getArtifact},
		{http.MethodPatch, "/api/v1/artifacts/{id}", authAny, "Correct a document's title: {name}. It writes the name alone - the slug, the reference key it gives and the role the document plays do not move - and answers the document as it now is. The owner's name it already holds is 409 ARTIFACT_NAME_TAKEN, a name whose slug is one of the documents Dispatch makes itself, on a document that is not that one, 400 ARTIFACT_ROLE_NAME, the issue's spec 400 ARTIFACT_PRIMARY, an artifact that is no document 400 ARTIFACT_KIND_MISMATCH, and a name that is empty or too long 400 INVALID_ARTIFACT.", s.renameArtifact},
		{http.MethodDelete, "/api/v1/artifacts/{id}", authAny, "Remove a document of an issue that is still untouched: made by typing a title and never written in. It answers 204 and appends artifact.removed on the issue, and it leaves no row behind. A document that holds writing or a later version, an edit that has not settled, or a comment, ask, review, approval request, check result, read mark or link from anything in Dispatch is 409 ARTIFACT_NOT_UNTOUCHED, saying which of those it is; the issue's spec, a document that plays a role and an artifact that is no document read the same refusal; and a project's document is 400 ARTIFACT_PROJECT_DOCUMENT, since a project document's events are its own and its removal event would need the row the removal deletes.", s.removeArtifact},
		{http.MethodPost, "/api/v1/artifacts/{id}/rebuild", authAny, "Rebuild a document history ygo cannot load from its latest saved markdown; a live document is 409 DOCUMENT_LIVE and one that loads is 409 DOCUMENT_LOADS.", s.rebuildArtifact},
		{http.MethodGet, "/api/v1/artifacts/{id}/reviews", authAny, "List a document's approval reviews.", s.listArtifactReviews},
		{http.MethodPost, "/api/v1/artifacts/{id}/reviews", authAny, "Approve or request changes on a document's latest settled version; answers the open approval ask when present, preserving its thread, and reviews every document that ask named.", s.createArtifactReview},
		{http.MethodPost, "/api/v1/artifacts/{id}/versions/{number}/reviews", authAny, "Approve or request changes on one version of a document {state, reason?}: {number} is the version the caller read, and the route requires it. The review is recorded only when {number} is the version it would record, the document's latest settled version or, with an approval ask open, the version that ask names for the document; any other is 409 REVIEW_VERSION_MOVED, naming the version the review would record, with nothing written, and a {number} that is not a whole number of 1 or more is 400 INVALID_REVIEW. With an approval ask open the review answers it, preserving its thread, and reviews every document the ask named, each at the version the ask names. This is the route a client sends when the version is a guard: a build without it answers no route, rather than recording the review without the guard.", s.createArtifactVersionReview},
		{http.MethodPost, "/api/v1/artifacts/{id}/plan-reviews", authAny, "Record a plan review's approval of a plan version: {version}, on an issue's plan.md alone (409 NOT_PLAN_DOCUMENT otherwise). A plan is never sent to a person for approval, so POST /api/v1/artifacts/{id}/approval-requests refuses it with 409 PLAN_APPROVED_BY_REVIEW; a later plan version makes this approval stale, as a spec's goes stale.", s.createArtifactPlanReview},
		{http.MethodPost, "/api/v1/artifacts/{id}/approval-requests", authAny, "Open an approval ask for a document's latest version, with an optional summary, and with include_verification for the issue's Verification document beside it (409 NO_VERIFICATION_DOCUMENT when it has none), each at its own current version; the answer reviews every document the ask names. include_verification is for the issue's spec alone: on any other document, the Verification document itself included, it is 409 NOT_SPEC_DOCUMENT. A document a human approved before carries that approval on the ask (approval.approved: version, at, by). An open ask follows later versions of any document it names and waits on its agent until this route hands the same row back to a human, save that an ask waiting on the human keeps its turn, quietly, over a version a settlement writes that holds byte for byte the text the ask names; a version a route writes (a named version, an upload, an edit) moves it to its agent whatever text it holds. Every answer carries coverage, one line per requirement of the issue's spec with no check, or the single line `the spec marks no requirements` for a spec that marks none (GET /api/v1/issues/{key}/coverage), and none at all for a spec every requirement of which has a check; it is empty for a document that is not a spec, never names a missing plan task, and a gap refuses nothing. A coverage read that fails answers coverage_unread true and records the request all the same.", s.requestArtifactApproval},
		{http.MethodGet, "/api/v1/artifacts/{id}/blocks", authAny, "A document's blocks with markdown ranges, tokens, reference counts and each block's embedding, where meaning search stands on it (embedded, pending, or none while no window covers it); a typed block carries attributes, its type's attributes in declaration order as its opening line writes them ({name, value, kind}, the value unescaped and unquoted and the kind the one the block schema declares it, so a reader follows a block[] value as the pointers it holds and reads every other kind as text), and a table block lists its rows and cells by the indexes delete_row and delete_column take.", s.getArtifactBlocks},
		{http.MethodGet, "/api/v1/artifacts/{id}/blocks/{block_id}", authAny, "Where one block stands in a document: its path from the top-level block down (type, id, child index), and for a table block, row or cell the table's id, the row index (0 is the header), the cell's column index, the text of the header cell drawn above it and the row's cells; 404 TARGET_NOT_FOUND for an id the live document does not hold.", s.getArtifactBlockPath},
		{http.MethodPost, "/api/v1/artifacts/{id}/blocks/{block_id}/keep", authAny, "Record that the caller agrees with the decision that block opens, as the text they read it at: {text_sha256}, the decision's own text_sha256 from GET /api/v1/issues/{key}/decisions. A decision whose text changed since is 409 DECISION_CHANGED with nothing written, and a body without the sum is 400 INVALID_KEEP; a repeat of the sum as it stands moves the row. The block must be the heading block of a decision of an issue's decisions.md outside `## Replaced`: any other block is 404 TARGET_NOT_FOUND. Answers the keep and appends decision.kept, which wakes nobody.", s.keepDecision},
		{http.MethodGet, "/api/v1/artifacts/{id}/text", authAny, "A document's canonical markdown, its whole-document token and its version. With ?blocks=true the answer carries the document's blocks as well, each as GET /api/v1/artifacts/{id}/blocks reports it, rendered from the one tree the markdown and the token come from: a listing read this way cannot be split by another session's write.", s.getArtifactText},
		{http.MethodGet, "/api/v1/artifacts/{id}/versions/{number}", authAny, "One version of a document, a file or an image. A named or settled document version answers as JSON; a file or an image version answers its bytes under its own content type, with Content-Disposition: attachment naming the artifact's name, -v<number> before the extension for an older version (report-v2.pdf), and Cache-Control: private, max-age=31536000, immutable. With ?download=true every kind answers Content-Disposition: attachment with that name and Cache-Control: private, no-cache, and a document answers its Markdown as text/markdown, its name ending in .md when the stored name has no Markdown extension. A version's author carries proven: true only when the credential of every write that author made to the version proved it.", s.getArtifactVersion},
		{http.MethodPost, "/api/v1/artifacts/{id}/versions", authAny, "Name the current document version.", s.createNamedVersion},
		{http.MethodPost, "/api/v1/artifacts/{id}/edits", authAny, "Apply quote-anchored edit ops with an optional optimistic-concurrency precondition.", s.editArtifact},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts/{slug}", authAny, "Read an issue document by slug or unambiguous filename.", s.getArtifact},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts/{slug}/text", authAny, "An issue document's canonical markdown, whole-document token and version, by slug or unambiguous filename; ?blocks=true carries its blocks beside them, from the one tree.", s.getArtifactText},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts/{slug}/blocks", authAny, "An issue document's blocks and tokens, by slug or unambiguous filename.", s.getArtifactBlocks},
		{http.MethodGet, "/api/v1/issues/{key}/artifacts/{slug}/versions/{number}", authAny, "One version of an issue's document, file or image, by slug or unambiguous filename, as GET /api/v1/artifacts/{id}/versions/{number} answers it: a document as JSON, a file or an image as its bytes with Content-Disposition: attachment naming the artifact's name, -v<number> before the extension for an older version. With ?download=true every kind answers Content-Disposition: attachment with that name and Cache-Control: private, no-cache, and a document answers its Markdown as text/markdown.", s.getArtifactVersion},
		{http.MethodPost, "/api/v1/issues/{key}/artifacts/{slug}/versions", authAny, "Name the current version of an issue document, by slug or unambiguous filename.", s.createNamedVersion},
		{http.MethodPost, "/api/v1/issues/{key}/artifacts/{slug}/edits", authAny, "Apply edit ops with an optional optimistic-concurrency precondition to an issue document, by slug or unambiguous filename.", s.editArtifact},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}", authAny, "Read a project document by slug or unambiguous filename.", s.getArtifact},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/summary", authAny, "What a reference to a project document shows, read without its versions: {id, project, slug, name, kind, latest_version}, the slug or unambiguous filename resolved as the project document read resolves it (404 ARTIFACT_NOT_FOUND, 400 ARTIFACT_REFERENCE_AMBIGUOUS).", s.getProjectDocumentSummary},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/text", authAny, "A project document's canonical markdown, whole-document token and version, by slug or unambiguous filename; ?blocks=true carries its blocks beside them, from the one tree.", s.getArtifactText},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/versions/{number}", authAny, "One version of a project's document, file or image, by slug or unambiguous filename, as GET /api/v1/artifacts/{id}/versions/{number} answers it: a document as JSON, a file or an image as its bytes with Content-Disposition: attachment naming the artifact's name, -v<number> before the extension for an older version. With ?download=true every kind answers Content-Disposition: attachment with that name and Cache-Control: private, no-cache, and a document answers its Markdown as text/markdown.", s.getArtifactVersion},
		{http.MethodGet, "/api/v1/projects/{key}/artifacts/{slug}/blocks", authAny, "A project document's blocks and tokens, by slug or unambiguous filename.", s.getArtifactBlocks},
		{http.MethodPost, "/api/v1/projects/{key}/artifacts/{slug}/versions", authAny, "Name the current version of a project document, by slug or unambiguous filename.", s.createNamedVersion},
		{http.MethodPost, "/api/v1/projects/{key}/artifacts/{slug}/edits", authAny, "Apply edit ops with an optional optimistic-concurrency precondition to a project document, by slug or unambiguous filename.", s.editArtifact},
		{http.MethodGet, "/api/v1/me/state", authAny, "The caller's per-issue read state.", s.getUserState},
		{http.MethodPut, "/api/v1/me/issues/{key}/state", authAny, "Update the caller's read state for an issue.", s.putUserState},
		{http.MethodGet, "/api/v1/me/agents/state", authAny, "The caller's per-agent conversation state: cleared_before, read_through, unread_replies (the session's replies to the caller's direct messages newer than both and not read by id), and pinned_at (when the caller pinned the session, which puts it in their sidebar's Agents list).", s.getUserAgentState},
		{http.MethodPut, "/api/v1/me/agents/{session_id}/state", authAny, "Clear an agent's conversation for the caller (cleared_before hides exchanges at or before it), mark it read (read_through only moves forward), mark some of its replies read by id (read_replies, the session's own messages), and/or pin it to the caller's sidebar (pinned: true pins it, false unpins it, and neither touches the Clear); answers the session's state.", s.putUserAgentState},
		{http.MethodGet, "/api/v1/me/documents/{id}/read", authAny, "The caller's last read version of a document {artifact_id, version, read_at}; version and read_at are null when they have read none.", s.getDocumentRead},
		{http.MethodPut, "/api/v1/me/documents/{id}/read", authAny, "Record the caller's last read version of a document {version}: the Spec tab marks each block that changed since it. Answers the stored read; a version that is missing or negative is 400 INVALID_STATE.", s.putDocumentRead},
		{http.MethodPut, "/api/v1/me/asks/{id}/snooze", authAny, "Snooze an inbox row for the caller until snoozed_until.", s.putAskSnooze},
		{http.MethodDelete, "/api/v1/me/asks/{id}/snooze", authAny, "Un-snooze an inbox row for the caller.", s.deleteAskSnooze},
		{http.MethodGet, "/api/v1/events/log", authAny, "Page the whole event log by id, whatever owns each event (an issue, a document, a project or none): the events with id > after (default 0), oldest first, at most limit (1 to 1000, default 1000). An event's actor carries proven: true when the write's credential proved it (a browser's person, a broker-issued token's session), and no proven when the request only named it.", s.listEventLog},
		{http.MethodGet, "/api/v1/credential-requests", authAny, "List credential requests the caller may decide (?approver=me only): those naming the caller, a shared secret's and a service's machine login, which anyone signed in decides, each secret request carrying approve_url, the secrets broker's page where it is decided; null without a configured secrets broker.", s.listCredentialPending},
		{http.MethodPost, "/api/v1/credential-requests/{id}/approve", authAny, "Approve a machine login as the caller with its typed code; the broker decides whether the caller is its approver, and refuses a secret request with 403 DECIDE_ON_BROKER_PAGE naming its page on the secrets broker.", s.approveCredentialRecord},
		{http.MethodPost, "/api/v1/credential-requests/{id}/deny", authAny, "Deny a machine login as the caller with its typed code; the broker decides whether the caller is its approver, and refuses a secret request with 403 DECIDE_ON_BROKER_PAGE naming its page on the secrets broker.", s.denyCredentialRecord},
		{http.MethodPost, "/api/v1/credential-requests/machine-lookup", authAny, "Resolve a pending machine login by its typed confirmation code, returning its facts.", s.lookupMachineCredential},
		{http.MethodGet, "/api/v1/credential-grants", authAny, "List credential grants the caller may revoke (?approver=me only).", s.listCredentialGrants},
		{http.MethodPost, "/api/v1/credential-grants/{id}/revoke", authAny, "Revoke a credential grant as the caller, its approver or its enrollment's operator.", s.revokeCredentialGrant},
		{http.MethodGet, "/api/v1/machine-logins", authAny, "List the machine logins the caller may revoke that can still reach a secret: the secrets broker's launcher credentials for every service's login, whoever approved it, and for the caller's own machines, unexpired or expired with a session still running, each naming who approved it.", s.listMachineLogins},
		{http.MethodPost, "/api/v1/machine-logins/{id}/revoke", authAny, "Revoke a machine login, expired or not, ending every session it enrolled, a service's pods included: anyone signed in revokes a service's login, and only the person who approved a person's machine login revokes that one.", s.revokeMachineLogin},
		{http.MethodGet, "/api/v1/signins", authAny, "List the caller's own sign-ins on their machines (`agent-secrets login`), one per box key they have not revoked, each naming the machine's account, the key's thumbprint, when the person signed in there and when that machine last used it. Nobody lists another person's: Dispatch names the person from its own session.", s.listSignIns},
		{http.MethodPost, "/api/v1/signins/{id}/revoke", authAny, "End one of the caller's own sign-ins: that machine trades no token for them again and every access token already minted under it ends, while their sign-ins on other machines keep working. A sign-in of anyone else is an unknown id.", s.revokeSignIn},
		{http.MethodGet, "/api/v1/credentials/capabilities", authAny, "What the credential pages may offer against this deployment's secrets broker, relayed from the broker's own RFC 8414 metadata rather than from any Dispatch setting: machine_code_page says whether the broker still serves the device grant, so whether a machine ever prints a code for a person to type.", s.getCredentialCapabilities},
	}
	// The live-socket disconnect, live-socket rotate, quiesce and publish hooks (the 1st, 2nd, 3rd
	// and 6th below) act on every process serving the database, answering once each has
	// (test_hooks_fanout.go); outside-schema and newer-content (the 4th and 5th) write the stored
	// history, which every process reads, and the room, room-counter, persistence and live-write
	// hooks, the slow-write arm, the new-issue settings and the queued messages (the last three)
	// each act on the process whose call they land on.
	if s.deps.TestHooksEnabled {
		rows = append(rows,
			routeRow{http.MethodPost, "/api/v1/live/_test/disconnect", authAny, "Test hook: close every open live socket on every process with 1012, as a restart does.", s.disconnectLiveSockets},
			routeRow{http.MethodPost, "/api/v1/live/_test/rotate", authAny, "Test hook: close every open live socket on every process with 4001 (max age), as its age does.", s.rotateLiveSockets},
			routeRow{http.MethodPost, "/api/v1/artifacts/_test/quiesce", authAny, "Test hook: close every live document on every process and finish the settlements in flight.", s.quiesceDocuments},
			routeRow{http.MethodPost, "/api/v1/artifacts/{id}/_test/outside-schema", authAny, "Test hook: write a crafted tree outside the Proof schema.", s.injectArtifactSchemaFailure},
			routeRow{http.MethodPost, "/api/v1/artifacts/{id}/_test/newer-content", authAny, "Test hook: append an element no build knows, so the document holds newer content.", s.injectArtifactNewerContent},
			routeRow{http.MethodPost, "/api/v1/agents/{session_id}/stream/_test/publish", authAny, "Test hook: publish one frame to a session's conversation viewers on every process.", s.publishAgentStreamFrame},
			routeRow{http.MethodGet, "/api/v1/artifacts/_test/rooms", authAny, "Test hook: list the documents this process holds a room of.", s.listResidentRooms},
			routeRow{http.MethodGet, "/api/v1/artifacts/{id}/_test/room-counters", authAny, "Test hook: count this process's room failures, lock-waiting compactions and room closes of a document, time its settlement resumptions of it, and read the awareness client entries its room holds now.", s.getRoomCounters},
			routeRow{http.MethodPost, "/api/v1/artifacts/{id}/_test/hold-persistence", authAny, "Test hook: hold this process's appends of a document's browser updates for a duration.", s.holdRoomPersistence},
			routeRow{http.MethodPut, "/api/v1/artifacts/{id}/_test/live-write-pauses/{point}", authAny, "Test hook: park the document's next live write at a point (after-first-append, after-commit).", s.armLiveWritePause},
			routeRow{http.MethodGet, "/api/v1/artifacts/{id}/_test/live-write-pauses/{point}", authAny, "Test hook: wait until a live write parks at the armed pause.", s.awaitLiveWriteParked},
			routeRow{http.MethodDelete, "/api/v1/artifacts/{id}/_test/live-write-pauses/{point}", authAny, "Test hook: release the live write parked at the pause and disarm it.", s.releaseLiveWritePause},
			routeRow{http.MethodPost, "/api/v1/writes/_test/slow", authAny, "Test hook: {method, path, delay_ms} delays every keyed run of that method and path by delay_ms after its key is claimed, on the handler's own context; 0 disarms.", s.armSlowWrite},
			routeRow{http.MethodPost, "/api/v1/settings/new-issue/_test", authAny, "Test hook: {label, session} replaces, on the process the call lands on, the new-issue label and session GET /api/v1/settings/new-issue answers from its environment; an empty value is none.", s.setNewIssueSettings},
			routeRow{http.MethodGet, "/api/v1/messages/_test/queued", authAny, "Test hook: list the targeted messages this process queued itself for a session neither the listener's registry nor its known-session store held, each as the envelope its session's subject would hold: {queued: [{subject, dedupe_key, event_id, source, payload_summary, payload, urgency}]}, oldest first; 404 QUEUED_ON_BUS on a process that publishes them to NATS.", s.listQueuedMessages},
		)
	}
	routes := make([]apiRoute, len(rows))
	for index, row := range rows {
		name := row.Method + " " + row.Pattern
		routes[index] = apiRoute{row, writeClasses[name], routeScopes[name], readClasses[name].Classes, slices.Contains(readScopeRoutes, name)}
	}
	return routes
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

// WriteRouteIndex writes the body GET /api/v1 answers on a server without test hooks, read
// from the table alone: no database, no listener. `envoy-dispatch routes` prints it, and the
// docs site's API reference is generated from that output.
func WriteRouteIndex(w io.Writer) error {
	return json.NewEncoder(w).Encode(routeIndexBody(routeIndexEntries((&server{}).routes())))
}
