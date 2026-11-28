package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
)

// readWrites is what one GET row of the table writes when it serves a request: whether it is a
// read, which a read-only token may call, or a GET that writes, which no read-only token may
// call. A read-only token is any credential that holds read: a token the secrets broker minted
// from a read entry or narrowed on request, and a personal token minted with read.
type readWrites int

const (
	// readWritesUnset is the zero value: an entry that decides nothing. It is never a class, so
	// an entry that leaves Writes unset is a row nobody has read yet.
	readWritesUnset readWrites = iota
	// readWritesNothing is a read: a route a read-only credential may call. Wherever its writes
	// run, on the request's own context or on the server's context in work the request starts or
	// waits on, such a route writes nothing but these eight:
	//
	//   - the insert into dpop_proofs that spends a broker token's proof;
	//   - the update of a personal token's last_used_at;
	//   - the insert into text_trees that a read with Dispatch-Trees: 1 makes;
	//   - the upsert of a stale route_roster;
	//   - the event horizon's observer persisting event_horizon;
	//   - a project's term statistics refresh, in search_term_stats, search_term_totals and
	//     search_term_queue;
	//   - the GitHub client recording GitHub's rate limit in github_rate_limits;
	//   - a compaction's rewrite and delete in doc_updates and doc_compaction_owed.
	//
	// A read that writes any of them on the server's context carries a WriteNote saying so. Every
	// other GET route is a GET that writes (readWritesState), which no read-only credential may
	// call.
	readWritesNothing
	// readWritesState is a GET that writes: anything a read may not, wherever it runs. Its
	// WriteNote names what it writes.
	readWritesState
)

// asOfKind is how one GET row of the table answers a read at a past time, the cut: a request's
// as_of query parameter, or the cut a read-only token carries (model.Credential.Cut).
type asOfKind int

const (
	// asOfUnset is the zero value: an entry that decides nothing. It is never a kind, so an
	// entry that leaves AsOf unset is a row nobody has decided for as_of yet.
	asOfUnset asOfKind = iota
	// asOfAnswers is a row that answers what existed at the cut, from the records Dispatch
	// keeps (package asof): the cut of an as_of, or the cut of a read-only token, which wins
	// over any as_of the request sends (asOfGate). The read class's table of these rows is the
	// plan's table "Routes and their kinds" (INFRA-4620).
	asOfAnswers
	// asOfNoHistory is a row that holds no history: it answers the present only, refuses every
	// as_of 400 AS_OF_NOT_SERVED (asOfGate), or 400 QUERY_KEY_UNKNOWN on a row whose query the
	// decoder reads, which declares no as_of (asOfGate, apiRoute.decoded), and refuses a read-only
	// token that carries a cut 403 AS_OF_TOKEN_NOT_SERVED (refuseLimitedToken).
	asOfNoHistory
	// asOfCaller is GET /api/v1/whoami and its /api/v2 successor GET /api/v2/me alone
	// (asOfCallerRows): each describes the caller now, the cut its token carries included, and
	// refuses as_of as a row that holds no history does. readClassRefusals refuses it on every
	// other row. The limited-token gate lets a token that carries a cut reach such a row
	// (refuseLimitedToken), and the as_of gate runs the row with no cut on its reads (asOfGate),
	// so the row would answer that token with the present.
	asOfCaller
)

// asOfCallerRow is the /api/v1 row of kind asOfCaller.
const asOfCallerRow = "GET /api/v1/whoami"

// asOfCallerV2Row is the /api/v2 row of kind asOfCaller, GET /api/v1/whoami's successor, which
// names the cut its token carries as whoami does (getMeV2).
const asOfCallerV2Row = "GET /api/v2/me"

// asOfCallerRows reports whether name, "<method> <pattern>", is a row that may be of kind
// asOfCaller: GET /api/v1/whoami and GET /api/v2/me.
func asOfCallerRows(name string) bool {
	return name == asOfCallerRow || name == asOfCallerV2Row
}

// readEntry is the read class of one GET row of the table: what the row writes, the note that
// says so where the class alone does not, how the row answers a read at a past time, and, for a
// row that answers one, what it answers at a cut.
type readEntry struct {
	// Writes is the row's class: readWritesNothing or readWritesState, never readWritesUnset.
	Writes readWrites
	// WriteNote names what the row writes, and the statement or the function that writes it. A
	// readWritesState row always carries one. A readWritesNothing row carries one when its
	// handler makes the server write on its own context, or when its class differs from what a
	// first reading of its handler suggests, and then it says why.
	WriteNote string
	// AsOf is the row's as_of kind: asOfAnswers, asOfNoHistory or asOfCaller, never asOfUnset.
	AsOf asOfKind
	// AtACut is what a row of kind answers (asOfAnswers) answers at a past time, the cut. routes()
	// puts it in the row's description before the sentences that every such row shares
	// (asOfDescription). Every row of kind answers carries one, and no other row does
	// (readClassRefusals).
	AtACut string
}

// docsCompactionNote is the WriteNote of every read that reads a document's live text through
// the document service (docs.Service's Text, TextWithBlocks, TextWithToken,
// TextWithTokenAndBlocks, Blocks and BlockPath, each through its readDocument).
const docsCompactionNote = "Writes nothing itself. A cold read of a document whose stored history holds more than one doc_updates row queues the document on the document service's compaction queue (noteStoredUpdates), and the queue's worker compacts it later on its own context: it updates and deletes doc_updates and deletes doc_compaction_owed, as the hourly sweep does for every due document, whoever reads it. A compaction changes nothing a reader reads."

// asOfDescription is the paragraph that routes() adds to the description of each GET row, from
// the row's read class (routeDecl.Read): how the row answers as_of and a read-only token that
// carries a cut. It is written once here for every row of a kind, so the route index, the OpenAPI
// document and each client generated from it say the same of each such row. The members that the
// answer of a row can leave out at a cut are read from asof.AbsentFields, the table that the
// answer's Dispatch-As-Of-Absent is held to (asof.WriteHeaders), so the description cannot name
// another list. What a row of kind answers answers at a cut is the row's own sentence
// (readEntry.AtACut).
func asOfDescription(name string, entry readEntry) string {
	switch entry.AsOf {
	case asOfAnswers:
		return " " + strings.Join([]string{entry.AtACut, asOfAnswersDescription, asOfAbsentDescription(name), asOfCutErrorDescription}, " ")
	case asOfCaller:
		return " " + asOfCallerDescription
	case asOfNoHistory:
		if entry.Writes == readWritesState {
			return " " + asOfNoHistoryWriteDescription
		}
		return " " + asOfNoHistoryDescription
	}
	return ""
}

// asOfAnswersDescription is what every row of kind answers says of as_of and of a read-only token
// that carries a cut (asOfGate, serveAtCut).
const asOfAnswersDescription = "as_of is an RFC 3339 time with an offset, which Dispatch rounds down to the microsecond: the cut. " +
	"A malformed as_of, a cut later than the database's present time, and a second as_of are 400 " + contracts.DispatchInvalidAsOf + ". " +
	"A read-only token that carries a cut reads at its own cut, and Dispatch then reads no as_of of the request. " +
	"An answer at a cut names the cut in the " + contracts.DispatchAsOfHeader + " header, in UTC with six fractional digits."

// asOfCutErrorDescription is the error of every cut: a row at a cut reads each record by the time
// its transaction began (events.created_at is the transaction's now()), and nothing records when
// a transaction committed. Its figures are production's, measured on 2026-11-19: the longest
// write request in a sample of the access log, and the largest gap between the id order of the
// event log and its created_at order.
const asOfCutErrorDescription = "Dispatch dates each write by the start of its transaction, and records no time at which a write committed. " +
	"So a write that began before the cut and committed after it reads as made at its start. " +
	"In a sample of production's access log, the write requests of 2026-11-16 to 2026-11-19, the longest took 47 s. " +
	"Production's event log held an event 249 s older than an event with a lower id."

// asOfAbsentDescription names the members that the answer of the route named name can leave out
// at a cut, in the order of asof.AbsentFields.
func asOfAbsentDescription(name string) string {
	fields := asof.AbsentFields[name]
	if len(fields) == 0 {
		return "At a cut the answer leaves out no member."
	}
	pointers := make([]string, len(fields))
	for index, field := range fields {
		pointers[index] = field.Pointer
	}
	return "At a cut the answer can leave out these members, because the records at the cut do not give them: " +
		strings.Join(pointers, ", ") + ". The answer names each member that it leaves out in the " + contracts.DispatchAsOfAbsentHeader +
		" header, as RFC 6901 JSON Pointers in which * stands for each member of an array. It never writes such a member as null."
}

// asOfCallerDescription is what GET /api/v1/whoami (asOfCaller) says of as_of and of a read-only
// token that carries a cut (asOfGate, refuseLimitedToken).
const asOfCallerDescription = "This route describes the caller now and keeps no history. " +
	"It refuses as_of with 400 " + contracts.DispatchAsOfNotServed + ", except from a read-only token that carries a cut, whose as_of Dispatch does not read. " +
	"It answers a read-only token that carries a cut, and its cut member names that cut. " +
	"A route that answers as_of answers such a token as of its cut. " +
	"Every other read of the read class, and GET /ws/live, refuse it with 403 " + contracts.DispatchAsOfTokenNotServed + "."

// asOfNoHistoryDescription is what a read (readWritesNothing) that holds no history
// (asOfNoHistory) says of as_of and of a read-only token that carries a cut (asOfGate,
// refuseLimitedToken).
const asOfNoHistoryDescription = "This route keeps no history and answers the present only. " +
	"It refuses as_of with 400 " + contracts.DispatchAsOfNotServed + ", and a read-only token that carries a cut with 403 " +
	contracts.DispatchAsOfTokenNotServed + "."

// asOfNoHistoryWriteDescription is what a GET that writes (readWritesState), which holds no
// history (asOfNoHistory), says of as_of and of a read-only token (asOfGate, refuseLimitedToken).
const asOfNoHistoryWriteDescription = "This route keeps no history and answers the present only. " +
	"It refuses as_of with 400 " + contracts.DispatchAsOfNotServed + ". " +
	"As a GET that writes, it refuses every read-only token with 403 " + contracts.DispatchReadOnlyToken + ", a token that carries a cut too."

// asOfSearchAtACut is what both search rows answer at a cut (search.go, the past-time legs).
const asOfSearchAtACut = "At a cut the search reads the text that each item had at the cut: the keyword legs read the keyword table of past text, " +
	"and the document leg reads each document's newest version at the cut. " +
	"The meaning legs use the window of an item only when the item did not change after the cut, and the window was embedded by then with the query's model. " +
	"A post deleted after the cut gives no hit and no snippet. " +
	"A search at a cut does not broaden a query that matches nothing. " +
	"Until the fill of the keyword table reaches the cut, a search at the cut is 409 " + contracts.DispatchAsOfSearchNotFilled + ". " +
	"Its " + contracts.DispatchAsOfFilledThroughHeader + " header names the time that the fill reached, where the fill reached a time."

// asOfEventLogAtACut is what the three event logs answer at a cut beside their own sentence.
const asOfEventLogAtACut = "A post deleted after the cut answers its own events with no body, as it does now. " +
	"A preview of it that another post's event holds keeps its text."

// asOfDeletedCommentAtACut and asOfDeletedMessageAtACut are what a thread read answers at a cut
// for a post that its author deleted after the cut: the post reads as deleted, as the present
// read gives it, and the body that the present read gives empty is left out and named absent.
const (
	asOfDeletedCommentAtACut = "A comment deleted after the cut reads as deleted, with its deleted_at, and the answer leaves out its body and names it in " +
		contracts.DispatchAsOfAbsentHeader + "."
	asOfDeletedMessageAtACut = "A message deleted after the cut reads as deleted, with its deleted_at, and the answer leaves out its body and names it in " +
		contracts.DispatchAsOfAbsentHeader + "."
)

// asOfRemovedDocumentAtACut is what a read of one document or file answers at a cut for one that
// did not exist then.
const asOfRemovedDocumentAtACut = "A document made after the cut, or removed after it, is 404 ARTIFACT_NOT_FOUND: a removal keeps no row of the document."

// asOfRemovedDocumentNotListed is what a list of documents answers at a cut for one removed after
// the cut.
const asOfRemovedDocumentNotListed = "A document removed after the cut is not listed: a removal keeps no row of the document."

// asOfDocumentAtACut is what a read of one document answers at a cut.
const asOfDocumentAtACut = "At a cut the route reads the document as it stood at the cut: its name, its versions made by then and its approval at the cut. " +
	asOfRemovedDocumentAtACut

// asOfBlocksAtACut is what both block routes answer at a cut.
const asOfBlocksAtACut = "At a cut the route answers the blocks of the document's newest version at the cut."

// asOfPostHistoryAtACut is what the history route of a post of kind post ("comment" or
// "message") answers at a cut.
func asOfPostHistoryAtACut(post string) string {
	return "At a cut the route answers only the entries that the " + post + "'s edits and deletes at or before the cut wrote, newest first, paged by ?before= as now. " +
		"deleted_at and deleted_by are the " + post + "'s present values, so a " + post + " deleted after the cut answers as a deleted " + post + " answers now."
}

// asOfCommentListAtACut is what both comment lists answer at a cut (answerCommentList,
// writePastListAnswer).
const asOfCommentListAtACut = "At a cut the route lists the comments that existed then, each with its body, its edited_at and its resolution at the cut. " +
	"The answer goes out under an ETag over the cut and its own bytes. " + asOfDeletedCommentAtACut

// asOfThreadPageAtACut is what the three issue thread lists (GET /api/v1/issues/{key}/asks,
// /comments and /messages) say of ?order=activity at a cut: a page of threads is answered at the
// present only, and refuseThreadPageAtCut refuses it at a cut before the list reads anything.
const asOfThreadPageAtACut = "At a cut, from as_of or from a token, order is 400 " + contracts.DispatchAsOfNotServed + " whatever its value, because a page of threads is answered at the present only."

// read_classes.go holds what a read class is: the two classes, what each means, and the note a
// class carries, and the as_of kinds, with what each says in a GET route's description
// (asOfDescription). Which class and which kind a route has is the route's own declaration
// (routeDecl.Read, one file per route), or a routeRow's entry of v2ReadClasses, which routes()
// answers as apiRoute.Read and the read-only gate and the as_of gate read (refuseLimitedToken,
// asOfGate). The routes of the plan's table "Routes and their kinds" (INFRA-4620) answer as_of
// (asOfAnswers), GET /api/v1/whoami and its /api/v2 successor GET /api/v2/me describe the caller
// now (asOfCaller), and every other GET route holds no history (asOfNoHistory); each route of kind
// answers also says what it answers at a cut (AtACut). register refuses to mount a table whose GET
// routes are not all classed with a kind, or whose write states a class (readClassRefusals,
// routes_table.go), so a GET route added later is served to no read-only credential and answers
// no as_of until someone has read its handler and classed it.

// v2ReadClasses is the read class of every GET routeRow, keyed "GET /api/v2<pattern>", for each
// version the row is served in: a routeRow states no class of its own, and one served in both
// versions has one class at both paths (stampV2ReadClasses). It records each mounted GET
// explicitly, so a new GET needs a review even when it resembles an existing read. Most rows copy
// the class and note of the /api/v1 route they share a core with, from its declaration, with the
// as_of kind /api/v2 takes (declaredRead); the INFRA-979 rows served in both versions, and each row
// whose read is not its /api/v1 route's or that has none (GET /api/v2/issues/{key},
// /artifacts/{id}, /asks, /me/marks, /references and others), state their own. No row answers
// as_of (asOfAnswers): /api/v2 stands at main's freeze commit 67bcd7c7, which answered no read at a
// past time (dispatch://INFRA-4071/spec R2), so every row holds no history but GET /api/v2/me,
// which describes the caller now as GET /api/v1/whoami does (asOfCaller). The INFRA-979 rows hold
// no history at both paths, as their /api/v1 route files said on main before they became rows.
func v2ReadClasses() map[string]readEntry {
	read := readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory}
	withNote := func(note string) readEntry {
		return readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: note}
	}
	return map[string]readEntry{
		"GET /api/v2":        declaredRead("GET /api/v1"),
		"GET /api/v2/agents": declaredRead("GET /api/v1/agents"),
		"GET /api/v2/agents/{session_id}/answers":                  declaredRead("GET /api/v1/agents/{session_id}/answers"),
		"GET /api/v2/agents/{session_id}/artifacts":                read, // Reads the conversation's artifact rows and stored versions.
		"GET /api/v2/agents/{session_id}/messages":                 declaredRead("GET /api/v1/agents/{session_id}/messages"),
		"GET /api/v2/artifacts/{id}":                               withNote("include=text or blocks reads live document text or blocks. " + docsCompactionNote),
		"GET /api/v2/artifacts/{id}/asks":                          declaredRead("GET /api/v1/artifacts/{id}/asks"),
		"GET /api/v2/artifacts/{id}/blocks/{block_id}":             declaredRead("GET /api/v1/artifacts/{id}/blocks/{block_id}"),
		"GET /api/v2/artifacts/{id}/changes":                       declaredRead("GET /api/v1/artifacts/{id}/changes"),
		"GET /api/v2/artifacts/{id}/comments":                      declaredRead("GET /api/v1/artifacts/{id}/comments"),
		"GET /api/v2/artifacts/{id}/events":                        declaredRead("GET /api/v1/artifacts/{id}/events"),
		"GET /api/v2/artifacts/{id}/references":                    declaredRead("GET /api/v1/artifacts/{id}/references"),
		"GET /api/v2/artifacts/{id}/reviews":                       declaredRead("GET /api/v1/artifacts/{id}/reviews"),
		"GET /api/v2/artifacts/{id}/subscribers":                   declaredRead("GET /api/v1/artifacts/{id}/subscribers"),
		"GET /api/v2/artifacts/{id}/versions":                      read, // Reads the artifact's stored version rows.
		"GET /api/v2/artifacts/{id}/versions/{number}":             declaredRead("GET /api/v1/artifacts/{id}/versions/{number}"),
		"GET /api/v2/asks":                                         read, // Reads ask rows, optional source mapping and caller marks.
		"GET /api/v2/asks/{id}":                                    declaredRead("GET /api/v1/asks/{id}"),
		"GET /api/v2/block-links":                                  declaredRead("GET /api/v1/block-links"),
		"GET /api/v2/block-schema":                                 declaredRead("GET /api/v1/schema/blocks"),
		"GET /api/v2/broadcasts":                                   declaredRead("GET /api/v1/broadcasts"),
		"GET /api/v2/broadcasts/{id}":                              declaredRead("GET /api/v1/broadcasts/{id}"),
		"GET /api/v2/ci-reconcile-passes":                          declaredRead("GET /api/v1/ci-reconcile/passes"),
		"GET /api/v2/comments/{id}":                                declaredRead("GET /api/v1/comments/{id}"),
		"GET /api/v2/comments/{id}/corrections":                    declaredRead("GET /api/v1/comments/{id}/history"),
		"GET /api/v2/credential-grants":                            declaredRead("GET /api/v1/credential-grants"),
		"GET /api/v2/credential-requests":                          declaredRead("GET /api/v1/credential-requests"),
		"GET /api/v2/dashboard-builds":                             declaredRead("GET /api/v1/dashboard/builds"),
		"GET /api/v2/delivery/measures":                            {Writes: readWritesState, AsOf: asOfNoHistory, WriteNote: "As the timeline's: an insert into delivery_run_warnings for a deploy run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},
		"GET /api/v2/delivery/pipeline":                            {Writes: readWritesState, AsOf: asOfNoHistory, WriteNote: "As the timeline's, for the deploy runs on main since the first day it shows: an insert into delivery_run_warnings for a run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},
		"GET /api/v2/delivery/timeline":                            {Writes: readWritesState, AsOf: asOfNoHistory, WriteNote: "When a deploy run has no configured production job, or its production job has no configured apply step, the read inserts a delivery_run_warnings row on the request's context (firstToWarn, on conflict doing nothing), once per process for each run and configuration."},
		"GET /api/v2/deploy-runs/{id}":                             declaredRead("GET /api/v1/delivery/runs/{id}"),
		"GET /api/v2/deploys":                                      declaredRead("GET /api/v1/deploys"),
		"GET /api/v2/events":                                       declaredRead("GET /api/v1/events/log"),
		"GET /api/v2/going-actions":                                declaredRead("GET " + goingCataloguePath),
		"GET /api/v2/going-requests/{id}/stream":                   declaredRead("GET /api/v1/going/requests/{id}/stream"),
		"GET /api/v2/handoffs/{id}":                                declaredRead("GET /api/v1/handoffs/{id}"),
		"GET /api/v2/issues":                                       declaredRead("GET /api/v1/issues"),
		"GET /api/v2/issues/{key}":                                 read, // The includes read issue facts, reach, design gates and sampled Legion state.
		"GET /api/v2/issues/{key}/artifacts":                       withNote("include=text or blocks reads live document text or blocks. " + docsCompactionNote),
		"GET /api/v2/issues/{key}/asks":                            declaredRead("GET /api/v1/issues/{key}/asks"),
		"GET /api/v2/issues/{key}/checks":                          declaredRead("GET /api/v1/issues/{key}/results"),
		"GET /api/v2/issues/{key}/collaborators":                   declaredRead("GET /api/v1/issues/{key}/collaborators"),
		"GET /api/v2/issues/{key}/comments":                        declaredRead("GET /api/v1/issues/{key}/comments"),
		"GET /api/v2/issues/{key}/coverage":                        declaredRead("GET /api/v1/issues/{key}/coverage"),
		"GET /api/v2/issues/{key}/decisions":                       declaredRead("GET /api/v1/issues/{key}/decisions"),
		"GET /api/v2/issues/{key}/events":                          declaredRead("GET /api/v1/issues/{key}/events"),
		"GET /api/v2/issues/{key}/handoffs":                        declaredRead("GET /api/v1/issues/{key}/handoffs"),
		"GET /api/v2/issues/{key}/messages":                        declaredRead("GET /api/v1/issues/{key}/messages"),
		"GET /api/v2/issues/{key}/references":                      declaredRead("GET /api/v1/issues/{key}/references"),
		"GET /api/v2/issues/{key}/role-holder-set":                 declaredRead("GET /api/v1/issues/{key}/role-holders"),
		"GET /api/v2/issues/{key}/step-tasks":                      declaredRead("GET /api/v1/issues/{key}/steps"),
		"GET /api/v2/issues/{key}/steps":                           declaredRead("GET /api/v1/issues/{key}/steps"),
		"GET /api/v2/issues/{key}/subscribers":                     declaredRead("GET /api/v1/issues/{key}/subscribers"),
		"GET /api/v2/known-sessions":                               declaredRead("GET /api/v1/agents/known"),
		"GET /api/v2/limit-set":                                    declaredRead("GET /api/v1/limits"),
		"GET /api/v2/machine-logins":                               declaredRead("GET /api/v1/machine-logins"),
		"GET /api/v2/me":                                           declaredRead(asOfCallerRow),
		"GET /api/v2/me/agent-tokens":                              declaredRead("GET /api/v1/me/agent-tokens"),
		"GET /api/v2/me/answers":                                   declaredRead("GET /api/v1/me/answers"),
		"GET /api/v2/me/marks":                                     read, // Pages stored issue, agent, document, thread, message and ask marks.
		"GET /api/v2/me/marks/{kind}/{id}":                         read, // Reads one stored mark without setting it.
		"GET /api/v2/me/signins":                                   declaredRead("GET /api/v1/signins"),
		"GET /api/v2/me/threads":                                   declaredRead("GET /api/v1/me/replies"),
		"GET /api/v2/messages/{id}":                                declaredRead("GET /api/v1/messages/{id}"),
		"GET /api/v2/messages/{id}/corrections":                    declaredRead("GET /api/v1/messages/{id}/history"),
		"GET /api/v2/near-copy-checks/{id}":                        declaredRead("GET /api/v1/near-copy-checks/{id}"),
		"GET /api/v2/nodes":                                        declaredRead("GET /api/v1/people/{login}/nodes"),
		"GET /api/v2/priorities":                                   declaredRead("GET /api/v1/priorities"),
		"GET /api/v2/priorities/{id}/acts":                         declaredRead("GET /api/v1/priorities/{id}/history"),
		"GET /api/v2/projects":                                     read, // The optional default assignee comes from stored project settings.
		"GET /api/v2/projects/{key}/architecture":                  declaredRead("GET /api/v1/projects/{key}/architecture"),
		"GET /api/v2/projects/{key}/architecture-sources":          declaredRead("GET /api/v1/projects/{key}/architecture-sources"),
		"GET /api/v2/projects/{key}/artifacts":                     withNote("include=text or blocks reads live document text or blocks. " + docsCompactionNote),
		"GET /api/v2/projects/{key}/issues":                        declaredRead("GET /api/v1/issues"),
		"GET /api/v2/projects/{key}/legion/board":                  read,
		"GET /api/v2/projects/{key}/legion/daemon":                 read,
		"GET /api/v2/projects/{key}/near-copy-checks":              declaredRead("GET /api/v1/near-copy-checks"),
		"GET /api/v2/projects/{key}/plans":                         declaredRead("GET /api/v1/projects/{key}/plans"),
		"GET /api/v2/reference-targets":                            declaredRead("GET /api/v1/reference-targets"),
		"GET /api/v2/references":                                   withNote("Writes nothing itself. With an after position, readReferences waits on the event horizon; the observation may persist event_horizon through writePersisted on its own context, as with since on /api/v1."),
		"GET /api/v2/related-check-stats":                          declaredRead("GET /api/v1/related-checks"),
		"GET /api/v2/repositories":                                 declaredRead("GET /api/v1/settings/repo-projects"),
		"GET /api/v2/repositories/{owner}/{repo}/app-installation": declaredRead("GET /api/v1/github/app-installation/{owner}/{repo}"),
		"GET /api/v2/search-coverage":                              declaredRead("GET /api/v1/search/coverage"),
		"GET /api/v2/search-results":                               declaredRead("GET /api/v1/search/results"),
		"GET /api/v2/settings/credential-broker":                   declaredRead("GET /api/v1/credentials/capabilities"),
		"GET /api/v2/settings/delivery":                            read,
		"GET /api/v2/settings/new-issue":                           declaredRead("GET /api/v1/settings/new-issue"),
		"GET /api/v2/settings/text-model":                          declaredRead("GET /api/v1/going/capability"),
		"GET /api/v2/users":                                        declaredRead("GET /api/v1/users"),
	}
}

// declaredRead is the read class the route file of name declares, by "<method> <pattern>", as a
// routeRow that shares that route's core takes it (v2ReadClasses): the route's class and note,
// and the as_of kind of a /api/v2 row (v2AsOf). It is the zero entry, which classes nothing, where
// no route file declares name: a routeRow that names a route nobody declares is then a GET with no
// class, which register refuses (readClassRefusals).
func declaredRead(name string) readEntry {
	route, declared := declaredRoute(name)
	if !declared {
		return readEntry{}
	}
	return v2AsOf(route.Read)
}

// v2AsOf is entry, a /api/v1 route's read class, with the as_of kind the /api/v2 row that shares
// its core takes. A route that describes the caller now (asOfCaller, GET /api/v1/whoami) keeps it:
// its successor GET /api/v2/me names the cut a token carries as whoami does. Every other kind
// holds no history at /api/v2 (asOfNoHistory), and says nothing of a cut. /api/v2 stands at the
// freeze, which answered no read at a past time (v2ReadClasses), and a past-time answer names the
// members it leaves out by the /api/v1 route's own answer (asof.AbsentFields), which is no
// /api/v2 answer's shape: a /api/v2 row that answered a cut would name members its answer does not
// hold. So a /api/v2 row refuses a read-only token that carries a cut 403 AS_OF_TOKEN_NOT_SERVED
// (refuseLimitedToken) and an as_of 400 QUERY_KEY_UNKNOWN (asOfGate), and never answers either
// with the present.
func v2AsOf(entry readEntry) readEntry {
	if entry.AsOf != asOfCaller {
		entry.AsOf, entry.AtACut = asOfNoHistory, ""
	}
	return entry
}

// stampV2ReadClasses stamps each GET routeRow's class from v2ReadClasses onto its mounts, at
// /api/v2 and, for a row served in both versions, at /api/v1. A route file's declaration carries
// its own class (v1Route).
func stampV2ReadClasses(routes []apiRoute) {
	classes := v2ReadClasses()
	for index, route := range routes {
		if route.columns && route.Method == http.MethodGet {
			routes[index].Read = classes[v2ReadClassKey(route)]
		}
	}
}

// v2ReadClassKey is the key v2ReadClasses holds a routeRow mount's class under: GET and the row's
// /api/v2 pattern, whichever version the mount serves.
func v2ReadClassKey(route apiRoute) string {
	return route.Method + " /api/v2" + strings.TrimPrefix(route.Pattern, versionPrefix(route.Version))
}

// v2ReadClassesUnmounted names, sorted, each entry of v2ReadClasses that classes no GET routeRow
// of routes: a row renamed or deleted and its entry left behind reads as though it classed a
// route that is not there (readClassRefusals).
func v2ReadClassesUnmounted(routes []apiRoute) []string {
	mounted := map[string]bool{}
	for _, route := range routes {
		if route.columns && route.Method == http.MethodGet {
			mounted[v2ReadClassKey(route)] = true
		}
	}
	var unmounted []string
	for name := range v2ReadClasses() {
		if !mounted[name] {
			unmounted = append(unmounted, name+": v2ReadClasses names it, and it is no GET routeRow the table mounts")
		}
	}
	sort.Strings(unmounted)
	return unmounted
}
