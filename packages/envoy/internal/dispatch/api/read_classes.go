package api

import (
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

// asOfThreadPageAtACut is what the three issue thread lists (GET /api/v1/issues/{key}/asks,
// /comments and /messages) say of ?order=activity at a cut: a page of threads is answered at the
// present only, and refuseThreadPageAtCut refuses it at a cut before the list reads anything.
const asOfThreadPageAtACut = "At a cut, from as_of or from a token, order is 400 " + contracts.DispatchAsOfNotServed + " whatever its value, because a page of threads is answered at the present only."

// read_classes.go holds what a read class is: the two classes, what each means, and the note a
// class carries, and the as_of kinds, with what each says in a GET route's description
// (asOfDescription). Which class and which kind a route has is the route's own declaration
// (routeDecl.Read, one file per route), which routes() answers as apiRoute.Read and the read-only
// gate and the as_of gate read (refuseReadOnlyToken, asOfGate). The routes of the plan's table
// "Routes and their kinds" (INFRA-4620) answer as_of (asOfAnswers), GET /api/v1/whoami describes
// the caller now (asOfCaller), and every other GET route holds no history (asOfNoHistory); each
// route of kind answers also says what it answers at a cut (AtACut). register refuses to mount a
// table whose GET routes are not all classed with a kind, or whose write states a class
// (readClassRefusals, routes_table.go), so a GET route added later is served to no read-only
// credential and answers no as_of until someone has read its handler and classed it.
