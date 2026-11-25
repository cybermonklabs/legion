package api

import (
	"slices"
	"sort"
	"strings"
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
// rewrites a body's short ids as its text is indexed is their one reader (refs.Reader).
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
	"GET /api/v1/artifacts/{id}/changes":                     {Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
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
	"DELETE /api/v1/comments/{id}":          {Kind: "comment", NotFound: "NOT_FOUND"},
	"GET /api/v1/comments/{id}/blocks":      {Kind: "comment", NotFound: "NOT_FOUND"},
	"GET /api/v1/comments/{id}/history":     {Kind: "comment", NotFound: "NOT_FOUND"},
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
	"GET /api/v1/messages/{id}/history":                      {Kind: "message", NotFound: "NOT_FOUND"},
	"PATCH /api/v1/messages/{id}":                            {Kind: "message", NotFound: "NOT_FOUND"},
	"DELETE /api/v1/messages/{id}":                           {Kind: "message", NotFound: "NOT_FOUND"},
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
	"POST /api/v1/credential-requests/{id}/dismiss": {Reason: "a credential request id"},
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
	"PUT /api/v1/issues/{key}/steps/{id}":           {Reason: "a step record id: the uuid the Legion daemon derives for one run of one step"},
	"PUT /api/v1/issues/{key}/steps/{id}/rating":    {Reason: "a step record id: the uuid the Legion daemon derives for the run of a retro"},
	"PUT /api/v1/issues/{key}/plans/{id}":           {Reason: "a plan id: the uuid the Legion daemon derives for the plan of one run of an issue"},

	// A node's routes under an issue, whose {id} is the node's store address (`lens/<uuid>`, a
	// block's node id, or its whole `dispatch://` address), one percent-encoded path segment that
	// FindNodeUnderIssue decodes and no prefix shortens: the route answers NOT_FOUND wherever it
	// names no node the caller may see under the issue.
	"POST /api/v1/issues/{key}/nodes/{id}/findings": {Reason: "a node's store address under the issue, which FindNodeUnderIssue decodes and no prefix shortens"},
	"POST /api/v1/issues/{key}/nodes/{id}/pointers": {Reason: "a node's store address under the issue, which FindNodeUnderIssue decodes and no prefix shortens"},
	"POST /api/v1/issues/{key}/nodes/{id}/state":    {Reason: "a node's store address under the issue, which FindNodeUnderIssue decodes and no prefix shortens"},
	"POST /api/v1/issues/{key}/nodes/{id}/settle":   {Reason: "a node's store address under the issue, which FindNodeUnderIssue decodes and no prefix shortens"},
	"PATCH /api/v1/issues/{key}/nodes/{id}":         {Reason: "a node's store address under the issue, which FindNodeUnderIssue decodes and no prefix shortens"},
	"DELETE /api/v1/issues/{key}/nodes/{id}":        {Reason: "a node's store address under the issue, which FindNodeUnderIssue decodes and no prefix shortens"},

	// The document test hooks, which address the live room this process holds under the
	// document's own id and read no row of the artifacts table, so they answer no 404 of a
	// document that does not exist and resolve no prefix: a short id written at one of them
	// reaches the hook as the caller wrote it, as every other value does.
	"POST /api/v1/artifacts/{id}/_test/hold-persistence":            {Reason: "a test hook's document id, naming a live room rather than a stored document"},
	"GET /api/v1/artifacts/{id}/_test/ask-attributes":               {Reason: "a test hook's document id, naming a live room rather than a stored document"},
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
