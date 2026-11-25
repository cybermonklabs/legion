package api

import (
	"net/http"
	"sort"
	"strconv"
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
	// readWritesNothing is a read. Wherever its writes run, on the request's own context or on
	// the server's context in work the request starts or waits on, the row writes nothing but
	// what readClasses lists: the proof's spent row, a personal token's last use, the caches
	// Dispatch rebuilds from what it holds, and the shared work the server does for every reader
	// alike. A read that writes any of it on the server's context carries a WriteNote saying so.
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
	// as_of 400 AS_OF_NOT_SERVED (asOfGate), and refuses a read-only token that carries a cut
	// 403 AS_OF_TOKEN_NOT_SERVED (refuseReadOnlyToken).
	asOfNoHistory
	// asOfCaller is GET /api/v1/whoami alone (asOfCallerRow): it describes the caller now, the
	// cut its token carries included, and refuses as_of as a row that holds no history does.
	// readClassRefusals refuses it on every other row. The read-only gate lets a token that
	// carries a cut reach such a row (refuseReadOnlyToken), and the as_of gate runs the row with
	// no cut on its reads (asOfGate), so the row would answer that token with the present.
	asOfCaller
)

// asOfCallerRow is the one row of kind asOfCaller.
const asOfCallerRow = "GET /api/v1/whoami"

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
// the row's read class (readClasses): how the row answers as_of and a read-only token that
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
// token that carries a cut (asOfGate, refuseReadOnlyToken).
const asOfCallerDescription = "This route describes the caller now and keeps no history. " +
	"It refuses as_of with 400 " + contracts.DispatchAsOfNotServed + ", except from a read-only token that carries a cut, whose as_of Dispatch does not read. " +
	"It answers a read-only token that carries a cut, and its cut member names that cut. " +
	"A route that answers as_of answers such a token as of its cut. " +
	"Every other read of the read class, and GET /ws/live, refuse it with 403 " + contracts.DispatchAsOfTokenNotServed + "."

// asOfNoHistoryDescription is what a read (readWritesNothing) that holds no history
// (asOfNoHistory) says of as_of and of a read-only token that carries a cut (asOfGate,
// refuseReadOnlyToken).
const asOfNoHistoryDescription = "This route keeps no history and answers the present only. " +
	"It refuses as_of with 400 " + contracts.DispatchAsOfNotServed + ", and a read-only token that carries a cut with 403 " +
	contracts.DispatchAsOfTokenNotServed + "."

// asOfNoHistoryWriteDescription is what a GET that writes (readWritesState), which holds no
// history (asOfNoHistory), says of as_of and of a read-only token (asOfGate, refuseReadOnlyToken).
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

// readClasses is the read class of every GET row of the table with its test hooks in, keyed
// "GET <pattern>" as routeScopes is: exactly one entry for each GET row and none for anything
// else. A read (readWritesNothing) writes nothing, on the request's own context or on the
// server's context in work the request starts or waits on, but these:
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
// Every other GET row is a GET that writes (readWritesState), which no read-only token may call.
//
// Each entry also names the row's as_of kind (AsOf). The rows of the plan's table "Routes and
// their kinds" (INFRA-4620) answer as_of (asOfAnswers), GET /api/v1/whoami describes the caller
// now (asOfCaller), and every other GET row holds no history (asOfNoHistory). The as_of gate
// reads the kind (asOfGate). Each entry of kind answers also says what the row answers at a cut
// (AtACut), which routes() writes into the row's description (asOfDescription).
//
// routes() stamps each entry onto its row (apiRoute.Read), which is what the read-only gate and
// the as_of gate read (refuseReadOnlyToken, asOfGate). register refuses to mount the table when
// readClassRefusals names anything: a GET row this map does not class, an entry that leaves AsOf
// unset, asOfCaller on a row but GET /api/v1/whoami, a row of kind answers with no AtACut or a row
// of another kind with one, and an entry that names no GET row the table mounts. So a GET row
// added later is served to no read-only token and answers no as_of until someone has read its
// handler and classed it here.
//
// The entries are grouped by class, each group in the order of the table.
var readClasses = map[string]readEntry{
	// The GETs that write.
	"GET /api/v1/delivery/timeline":          {Writes: readWritesState, AsOf: asOfNoHistory, WriteNote: "When a deploy run has no configured production job, or its production job has no configured apply step, the read inserts a delivery_run_warnings row on the request's context (firstToWarn, on conflict doing nothing), once per process for each run and configuration."},
	"GET /api/v1/delivery/measures":          {Writes: readWritesState, AsOf: asOfNoHistory, WriteNote: "As the timeline's: an insert into delivery_run_warnings for a deploy run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},
	"GET /api/v1/delivery/pipeline":          {Writes: readWritesState, AsOf: asOfNoHistory, WriteNote: "As the timeline's, for the deploy runs on main since the first day it shows: an insert into delivery_run_warnings for a run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},
	"GET /api/v1/going/requests/{id}/stream": {Writes: readWritesState, AsOf: asOfNoHistory, WriteNote: "When the run is past its ends_at and holds no terminal frame, the read inserts the run's end frame, the error that its answer did not arrive, into going_events on the request's context (endGoingRun), doing nothing on conflict when another writer ended the run first."},

	// The reads whose handler makes the server write on its own context, or whose class a first
	// reading of the handler would get wrong.
	"GET /api/v1/projects/{key}/related":                 {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: "When the project has no search_term_totals row, the related-item check starts a refresh of the project's term statistics on the server's own context (ensureSearchTermStats, once per project and process): it inserts and updates search_term_totals, deletes and inserts search_term_stats, and deletes the project's search_term_queue rows. The statistics are a cache of what the project holds, which every related check reads and the first to find them missing rebuilds; nothing else is written."},
	"GET /api/v1/github/app-installation/{owner}/{repo}": {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: "Writes nothing itself. When GitHub answers the installation lookup with a rate limit, the GitHub client records that limit in github_rate_limits on a context detached from the request (ratelimit.Record), as it does for every caller that reads GitHub: the row is GitHub's own state, not a fact of Dispatch's."},
	"GET /api/v1/references":                             {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: "Writes nothing itself. With ?since=, the read waits on the event horizon, as the event log does, and the horizon's observation, which may run on this request's goroutine, writes event_horizon on a context of its own (writePersisted), at most once a second, after its waiters have their answer: the server's write for every reader alike. Without ?since= it does not read the horizon."},
	"GET /api/v1/issues/{key}/results":                   {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route reads the checks of the Verification document's newest version at the cut, with the results and the retractions recorded by then. It reads nothing from GitHub and writes nothing. So it leaves out the GitHub currency of each check and result, and the result that CI derives (ci-run:…). At a cut, from as_of or from a token, ?at= is 400 " + contracts.DispatchAsOfNotServed + ".", WriteNote: "A read-only caller's read reads GitHub and stores nothing. A writer's read stores what it read from GitHub, as it always has: the pull request heads of a result's commit (pull_request_heads), with the branch commits (branch_commits) and the attribution events (events) that come with them. Like every reader of GitHub, the GitHub client may record GitHub's rate limit in github_rate_limits on a detached context, and the check blocks are read through the document service, whose cold read may queue a compaction (docsCompactionNote)."},
	"GET /api/v1/issues/{key}/coverage":                  {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: docsCompactionNote},
	"GET /api/v1/issues/{key}/decisions":                 {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: docsCompactionNote},
	"GET /api/v1/asks/{id}":                              {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route reads the ask at its newest state event at or before the cut, with the replies, the edits, the answers and the followers that it had then.", WriteNote: "The ask's anchor block is read through the document service. " + docsCompactionNote},
	"GET /api/v1/comments/{id}":                          {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route reads the comment and its thread as they stood at the cut. " + asOfDeletedCommentAtACut, WriteNote: "The comment's anchor block is read through the document service. " + docsCompactionNote},
	"GET /api/v1/artifacts/{id}/blocks":                  {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: docsCompactionNote},
	"GET /api/v1/artifacts/{id}/blocks/{block_id}":       {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: docsCompactionNote},
	"GET /api/v1/artifacts/{id}/text":                    {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route answers the markdown and the document token of the document's newest version at the cut, and with ?blocks=true the blocks of that version.", WriteNote: docsCompactionNote},
	"GET /api/v1/issues/{key}/artifacts/{slug}/text":     {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: docsCompactionNote},
	"GET /api/v1/issues/{key}/artifacts/{slug}/blocks":   {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfBlocksAtACut, WriteNote: docsCompactionNote},
	"GET /api/v1/projects/{key}/artifacts/{slug}/text":   {Writes: readWritesNothing, AsOf: asOfNoHistory, WriteNote: docsCompactionNote},
	"GET /api/v1/projects/{key}/artifacts/{slug}/blocks": {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfBlocksAtACut, WriteNote: docsCompactionNote},
	"GET /api/v1/events/log":                             {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route pages only the events written at or before the cut, and no page reaches past the largest id among them. " + asOfEventLogAtACut, WriteNote: "Writes nothing itself. Reading the safe horizon (events.Horizon) may run the horizon's observation on this request's goroutine, and that observation writes event_horizon on a context of its own (writePersisted), at most once a second, after its waiters have their answer: the server's write for every reader of the event log alike, not this request's."},

	// The other reads.
	"GET /api/v1":                                                        {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/schema/blocks":                                          {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/limits":                                                 {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects":                                               {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/artifacts":                               {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the documents that the project held then, each with its name, its versions and its approval at the cut. " + asOfRemovedDocumentNotListed},
	"GET /api/v1/settings/repo-projects":                                 {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/settings/architecture-sources":                          {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/architecture-sources":                    {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/architecture-source":                     {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/architecture":                            {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/plans":                                   {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/settings/project-default-assignees":                     {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/settings/new-issue":                                     {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/settings/delivery":                                      {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/legion/daemon":                           {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/legion/board":                            {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/delivery/runs/{id}":                                     {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/ci-reconcile/passes":                                    {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/me/agent-tokens":                                        {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/users":                                                  {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/people/{login}/nodes":                                   {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/whoami":                                                 {Writes: readWritesNothing, AsOf: asOfCaller},
	"GET /api/v1/dashboard/builds":                                       {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/priorities":                                             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/priorities/{id}/history":                                {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues":                                                 {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the issues that existed then, each as it stood, under the same filters, order and pages as now. At a cut, from as_of or from a token, route_status and pinned are 400 " + contracts.DispatchAsOfNotServed + " whatever their value, because the route roster and a person's pins keep no history."},
	"GET /api/v1/issues/resolve":                                         {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route resolves owner/repo#n by the external links that the issues held at the cut."},
	"GET /api/v1/issues/{key}":                                           {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route reads the issue from its newest state event at or before the cut, with the asks, the documents and the children that existed then. progress.tasks counts the task items of the primary document's newest version at the cut. An issue made after the cut is 404."},
	"GET /api/v1/issues/{key}/summary":                                   {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/events":                                    {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route pages only the events written at or before the cut, under an ETag over the cut and the answer's own bytes. " + asOfEventLogAtACut},
	"GET /api/v1/issues/{key}/references":                                {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/collaborators":                             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/references/questions":                                   {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/reference-targets":                                      {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/block-links":                                            {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/subscribers":                               {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/artifacts":                                 {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the documents and files that the issue held then, each with its name, its versions and its approval at the cut. " + asOfRemovedDocumentNotListed},
	"GET /api/v1/issues/{key}/messages":                                  {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the message threads that existed then, each message with the text that it had at the cut. " + asOfDeletedMessageAtACut},
	"GET /api/v1/issues/{key}/messages/{id}":                             {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route reads the message and its replies as they stood at the cut, each with the text that it had then. " + asOfDeletedMessageAtACut},
	"GET /api/v1/issues/{key}/handoffs":                                  {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/handoffs/{phase}/latest":                   {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/steps":                                     {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/role-holders":                              {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/legion":                                    {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/handoffs/{id}":                                          {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/messages/{id}":                                          {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route reads the conversation as it stood at the cut, each message with the text that it had then. " + asOfDeletedMessageAtACut},
	"GET /api/v1/messages/{id}/blocks":                                   {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/messages/{id}/history":                                  {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfPostHistoryAtACut("message")},
	"GET /api/v1/inbox":                                                  {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the asks open at the cut, on an issue open then or on a project document, that waited on the person at the cut by the same three steps. Snoozes keep no history, so the answer leaves out snoozed_until. An ask whose person is a project default that the records at the cut do not give matches no ?assignee=."},
	"GET /api/v1/me/answers":                                             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/me/replies":                                             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/search":                                                 {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfSearchAtACut},
	"GET /api/v1/search/results":                                         {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfSearchAtACut},
	"GET /api/v1/search/coverage":                                        {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET " + goingCataloguePath:                                          {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/related-checks":                                         {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/near-copy-checks":                                       {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/near-copy-checks/{id}":                                  {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/going/capability":                                       {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/agents":                                                 {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/agents/{session_id}/answers":                            {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the window counts from the cut: since defaults to 72 hours before the cut, a since more than 7 days before the cut is 400 INVALID_QUERY, and a since after the cut answers no items. Each item has its words at the cut."},
	"GET /api/v1/agents/{session_id}/messages":                           {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/agents/{session_id}/artifacts/{slug}":                   {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route reads the conversation's file or image as it stood at the cut: its name and the versions made by then. One made after the cut, or removed after it, is 404 ARTIFACT_NOT_FOUND: a removal keeps no row of it."},
	"GET /api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}": {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/broadcasts":                                             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/broadcasts/{id}":                                        {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/asks":                                      {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the asks that existed then, each at its newest state event at or before the cut, with its newest reply at the cut. ?thread=1 gives each thread as it stood at the cut."},
	"GET /api/v1/asks/open":                                              {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the asks that were open at the cut. The answer's as_of is the cut, and each age_seconds counts from the cut."},
	"GET /api/v1/asks/{id}/followers":                                    {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/comments":                                  {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfCommentListAtACut},
	"GET /api/v1/comments/{id}/blocks":                                   {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/comments/{id}/history":                                  {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfPostHistoryAtACut("comment")},
	"GET /api/v1/artifacts/{id}/asks":                                    {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the document's asks that existed then, each at its newest state event at or before the cut."},
	"GET /api/v1/artifacts/{id}/comments":                                {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfCommentListAtACut},
	"GET /api/v1/artifacts/{id}/events":                                  {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route pages only the events written at or before the cut. " + asOfEventLogAtACut},
	"GET /api/v1/artifacts/{id}/references":                              {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/artifacts/{id}/subscribers":                             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/artifacts/{id}":                                         {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfDocumentAtACut},
	"GET /api/v1/artifacts/{id}/reviews":                                 {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/artifacts/{id}/versions/{number}":                       {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut a version made after the cut is 404, as JSON and as a download."},
	"GET /api/v1/artifacts/{id}/changes":                                 {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/issues/{key}/artifacts/{slug}":                          {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfDocumentAtACut},
	"GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number}":        {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/artifacts/{slug}":                        {Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfDocumentAtACut},
	"GET /api/v1/projects/{key}/artifacts/{slug}/summary":                {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/projects/{key}/artifacts/{slug}/versions/{number}":      {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/me/state":                                               {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/me/agents/state":                                        {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/me/documents/{id}/read":                                 {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/credential-requests":                                    {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/credential-grants":                                      {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/machine-logins":                                         {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/signins":                                                {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/credentials/capabilities":                               {Writes: readWritesNothing, AsOf: asOfNoHistory},
	// The test hooks' GET rows, which read this process's memory.
	"GET /api/v1/artifacts/_test/rooms":                          {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/artifacts/{id}/_test/room-counters":             {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/artifacts/{id}/_test/live-write-pauses/{point}": {Writes: readWritesNothing, AsOf: asOfNoHistory},
	"GET /api/v1/messages/_test/queued":                          {Writes: readWritesNothing, AsOf: asOfNoHistory},
}

// readClassRefusals names every way readClasses fails to class the table, sorted, each line
// naming its row: a GET row with no entry, an entry whose Writes is unset or no class at all, a
// GET that writes (readWritesState) whose entry has no WriteNote, an entry whose AsOf is unset or
// no kind at all, an entry of kind asOfCaller on a row but GET /api/v1/whoami (asOfCallerRow), an
// entry of kind answers with no AtACut, an entry of another kind with one, and an entry that
// names no GET row the table mounts, a POST row's name or a name no row holds. The walk is over
// the table with its test hooks in, as routesNotMounted's is, since a hook's row is a row of the
// table wherever a deployment leaves it out. register refuses to mount a table it names anything
// for, and TestEveryGETRowHasOneReadEntry reads this same answer, so the two cannot drift.
func readClassRefusals() []string {
	gets := map[string]bool{}
	var refused []string
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		if route.Method != http.MethodGet {
			continue
		}
		name := route.Method + " " + route.Pattern
		gets[name] = true
		entry, classed := readClasses[name]
		if !classed {
			refused = append(refused, name+": a GET row with no entry in readClasses")
			continue
		}
		switch {
		case entry.Writes == readWritesUnset:
			refused = append(refused, name+": its entry leaves Writes unset")
		case entry.Writes != readWritesNothing && entry.Writes != readWritesState:
			refused = append(refused, name+": its entry's Writes is "+strconv.Itoa(int(entry.Writes))+", which is neither readWritesNothing nor readWritesState")
		case entry.Writes == readWritesState && strings.TrimSpace(entry.WriteNote) == "":
			refused = append(refused, name+": a GET that writes (readWritesState) whose entry has no WriteNote naming what it writes")
		}
		switch entry.AsOf {
		case asOfAnswers, asOfNoHistory:
		case asOfCaller:
			if name != asOfCallerRow {
				refused = append(refused, name+": its entry's AsOf is asOfCaller, which only "+asOfCallerRow+" may hold: the row would answer a read-only token's cut with the present")
			}
		case asOfUnset:
			refused = append(refused, name+": its entry leaves AsOf unset")
		default:
			refused = append(refused, name+": its entry's AsOf is "+strconv.Itoa(int(entry.AsOf))+", which is neither asOfAnswers, asOfNoHistory nor asOfCaller")
		}
		switch {
		case entry.AsOf == asOfAnswers && strings.TrimSpace(entry.AtACut) == "":
			refused = append(refused, name+": a row of kind asOfAnswers whose entry has no AtACut saying what it answers at a cut")
		case entry.AsOf != asOfAnswers && entry.AtACut != "":
			refused = append(refused, name+": its entry has an AtACut, and the row is not of kind asOfAnswers")
		}
	}
	for name := range readClasses {
		if !gets[name] {
			refused = append(refused, name+": readClasses names it, and it is no GET row the table mounts (test hooks in)")
		}
	}
	sort.Strings(refused)
	return refused
}
