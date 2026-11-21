package api

// v2_logs.go is the five logs of /api/v2 (R5 of dispatch://INFRA-4071/spec): the
// global event log, an issue's and a project document's event logs, the reference graph's edges of
// one node, and the caller's Inbox threads. Each is a list read in order by one position field,
// which its row names (routeRow.Position) and its index entry and description carry: after and
// before read that field, and a value that is no position of it, or an after above the last one
// the log has written, is 400 POSITION_INVALID naming the parameter and the field. Each takes limit
// and offset beside them, and answers the envelope with its exact total - but the global event log
// and the caller's dismissed threads, whose total is null (logPage): counting the global log costs
// about ten times its page for a reader near the oldest event, and counting the dismissed threads
// is a count of every thread the caller has ever dismissed.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The refusals every /api/v2 read of a list declares, one code per condition (R7): a query key the
// route does not declare, and a limit or an offset outside the route's bounds (R4, R1.4); and a
// log's position that names none of its positions (R5).
var (
	undeclaredKeyRefusal = apicode.Refusal{Condition: apicode.QueryKeyUndeclared, Code: apicode.QueryKeyUnknown}
	limitRefusal         = apicode.Refusal{Condition: apicode.LimitOutOfBounds, Code: apicode.InvalidLimit}
	offsetRefusal        = apicode.Refusal{Condition: apicode.OffsetOutOfBounds, Code: apicode.InvalidOffset}
	positionRefusal      = apicode.Refusal{Condition: apicode.PositionMalformed, Code: apicode.PositionInvalid}
)

// listRefusals are the refusals of a list route, with the route's own after them.
func listRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append([]apicode.Refusal{undeclaredKeyRefusal, limitRefusal, offsetRefusal}, more...)
}

// The position fields of the five logs (R5): an owner's events by seq, the global log by id, the
// references by the source_seq of the event that introduced each mention, and the caller's
// dismissed threads by when each was dismissed, ties broken by the thread's id.
var (
	seqPosition       = positionField{Field: "seq", Type: positionInteger}
	eventIDPosition   = positionField{Field: "id", Type: positionInteger}
	sourceSeqPosition = positionField{Field: "source_seq", Type: positionInteger}
	dismissedPosition = positionField{Field: "dismissed_at,thread_id", Type: positionTimeAndID}
)

// ownerLogText is what the descriptions of an issue's and a document's event log say alike: the
// position, the filters, the order, the bounds and the refusal of a position.
const ownerLogText = "Position: seq, each event's place in its owner's log. after=<seq> keeps the events above it and " +
	"before=<seq> those below it, both together a window; ids=<id>,<id>... (event ids, at most 50) keeps those events alone. " +
	"sort=seq (the default, oldest first) or -seq; with no sort, a before alone reads down from it, newest first, so a before " +
	"past the last seq answers the newest page. limit 1 to 200 (default 200) and offset; {items, total, limit, offset}, total " +
	"the exact count of the events the filters keep. An after above the log's last seq, or an after or a before that is no " +
	"integer of 0 or more, is 400 POSITION_INVALID naming the parameter and seq; more than 50 ids is 400 INVALID_QUERY."

// v2LogRows are the five logs: rows 72, 103, 21, 170 and 134 of the route map.
func (s *server) v2LogRows() []routeRow {
	return []routeRow{
		{
			Method: http.MethodGet, Pattern: "/events", Auth: authAny, Versions: onlyV2,
			Description: "Page the whole event log, whatever owns each event (an issue, a document, a project or none), read up to the " +
				"log's safe horizon, so paging to the end of an answer skips no event committed before it. Position: id, each " +
				"event's id. after=<id> keeps the events above it and before=<id> those below it; sort=id (the default, oldest " +
				"first) or -id, and with no sort a before alone reads down from it, newest first. limit 1 to 1000 (default 1000) " +
				"and offset; {items, total, limit, offset}, and total is null on this route and on the caller's dismissed threads " +
				"(GET /api/v2/me/threads?state=dismissed), and on no other list: counting the log costs about ten " +
				"times its page for a reader near the oldest event (measured 2026-11-16), so a reader pages by after until a page " +
				"comes back short. An after above the highest event id written, or an after or a before that is no integer of 0 " +
				"or more, is 400 POSITION_INVALID naming the parameter and id. An event's actor carries proven: true when the " +
				"write's credential proved it (a browser's person, a broker-issued token's session), and no proven when the " +
				"request only named it.",
			Handler:  s.listEventLogV2,
			Resource: "events", Kind: kindLog, Position: eventIDPosition,
			Query:    eventLogV2Query{},
			List:     listBounds{Default: maxEventReplay, Max: maxEventReplay},
			Shape:    routeShape{Status: http.StatusOK, Response: logPage[model.Event]{}},
			Refusals: listRefusals(positionRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/events", Auth: authAny, Versions: onlyV2,
			Description: "Page an issue's event log. " + ownerLogText + " The answer carries an ETag over the issue's event " +
				"sequence and the query; If-None-Match answers 304. An event's actor carries proven: true when the write's " +
				"credential proved it (a browser's person, a broker-issued token's session), and no proven when the request " +
				"only named it. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
			Handler:  s.listIssueEventsV2,
			Resource: "events", Kind: kindLog, Position: seqPosition,
			Query: ownerEventsV2Query{},
			List:  listBounds{Default: maxEventPage, Max: maxEventPage},
			Shape: routeShape{
				Status: http.StatusOK, Response: page[model.Event]{},
				Also: []routeAnswer{{Status: http.StatusNotModified}},
			},
			Refusals: listRefusals(positionRefusal, apicode.Refusal{Condition: apicode.IssueMissing, Code: apicode.IssueNotFound}),
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/events", Auth: authAny, Versions: onlyV2,
			Description: "Page a project document's event log. " + ownerLogText + " 404 ARTIFACT_NOT_FOUND for a document that " +
				"does not exist; a document of an issue is 400 ARTIFACT_LINKED, its events being its issue's log, and a file of " +
				"an agent's conversation 400 ARTIFACT_AGENT_OWNED.",
			Handler:  s.listArtifactEventsV2,
			Resource: "events", Kind: kindLog, Position: seqPosition,
			Query:    ownerEventsV2Query{},
			List:     listBounds{Default: maxEventPage, Max: maxEventPage},
			Shape:    routeShape{Status: http.StatusOK, Response: page[model.Event]{}},
			IDs:      &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
			Refusals: listRefusals(positionRefusal, apicode.Refusal{Condition: apicode.DocumentMissing, Code: apicode.ArtifactNotFound}),
		},
		{
			Method: http.MethodGet, Pattern: "/references", Auth: authAny, Versions: onlyV2,
			Description: "Edges of one node in the reference graph: to=<dispatch ref> its backlinks or from=<dispatch ref> its " +
				"links, exactly one (400 INVALID_REFERENCE); kind=<kind>,<kind>... keeps those edge kinds, and a kind this build " +
				"does not emit is listed in unknown_kinds beside the page rather than refused. With no kind, an ask's backlinks " +
				"leave out its own replies and parts, and a document's, an ask's, a comment's or a message's leave out part_of. " +
				"Position: source_seq, the id of the event that introduced a mention. after=<source_seq> keeps the mentions above " +
				"it, structural edges carrying none, read up to the event log's safe horizon: every mention committed before the " +
				"read, unless an open transaction holds the horizon back past 1 s, when the read answers the mentions at or below " +
				"it and a later read the rest; a reader that starts from after=0 reads on from the highest source_seq it was " +
				"shown. before=<source_seq> keeps the mentions below it. sort=-created_at (the default, newest first), " +
				"created_at, source_seq or -source_seq, and with no sort an after reads up source_seq and a before alone down it. " +
				"limit 1 to 200 (default 50) and offset; {node, unknown_kinds, items, total, limit, offset}, total the exact count " +
				"of the edges the filters keep. 404 NOT_FOUND for a node nothing holds, 409 ID_AMBIGUOUS for a short id two items " +
				"carry; an after above the highest event id written, or an after or a before that is no integer of 0 or more, is " +
				"400 POSITION_INVALID naming the parameter and source_seq.",
			Handler:  s.listReferencesV2,
			Resource: "references", Kind: kindLog, Position: sourceSeqPosition,
			Query:    referencesV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: referencesV2{}},
			Refusals: listRefusals(positionRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/me/threads", Auth: authAny, Versions: onlyV2,
			Description: "The caller's Inbox threads (the person scope), of the kinds kind=<kind>,<kind> names: reply (the " +
				"default) lists the threads they wrote in - an ask, a comment thread, an issue message thread - that somebody " +
				"else has written in, less an open ask on an open issue that waits on them, which GET /api/v2/asks?waiting_on=me " +
				"lists; agent lists their one-off messages to an agent session (a BTW, a broadcast's copy, or a Send or Aside the " +
				"session did not take as a turn of its live conversation) holding a reply from that session; reply,agent lists " +
				"both in one order. state is unread (the default), the rows somebody else has written in since they read them, " +
				"newest reply first; inbox, every row in their Inbox, read or not, dismissed ones aside, newest reply first; or " +
				"dismissed, what they have dismissed out of it, newest dismissal first. Position, of state=dismissed alone: " +
				"dismissed_at,thread_id, each bound <RFC3339 time>,<thread id>, the position of a row a page answered: " +
				"after keeps the rows dismissed after that row, oldest dismissal first, and before the rows dismissed before " +
				"it, newest first where no after is given (after or before under any other state is 400 INVALID_QUERY; one " +
				"that is no time and thread id, and an after above the newest dismissal, are 400 POSITION_INVALID naming the " +
				"parameter and dismissed_at,thread_id). issue=<key> or " +
				"artifact=<document id> narrows to one owner (both is 400 INVALID_QUERY), and an agent row, which has neither, is " +
				"then in no answer. limit 1 to 200 (default 50) and offset; {items, total, limit, offset, as_of}, total the exact " +
				"count of both kinds the filters keep, and null under state=dismissed, which is read a page at a time from a " +
				"position, as the global event log is, rather than counted; as_of the database time of the one read-only " +
				"repeatable-read snapshot " +
				"every row was read in, which is what a mark-all passes back as its read mark. Each row names the thread, its " +
				"owner, its issue's priority, its project, the thread's subject, its root's deep link (an agent row's is the " +
				"session's live view), the newest reply's author, first line and time, the caller's read mark, whether it is " +
				"unread and how many writes in it they have not read, the root's document anchor when it has one, its dismissal " +
				"mark when it has been dismissed, and, on an agent row, the session and the ids of its unread replies.",
			Handler:  s.listMyThreadsV2,
			Resource: "threads", Kind: kindLog, Position: dismissedPosition,
			Query:       myThreadsV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: myThreadsV2{}},
			OAuthScopes: routeScopeSet{model.ScopePerson},
			Refusals:    listRefusals(positionRefusal),
		},
	}
}

// eventLogV2Query is GET /api/v2/events's query: one page of the whole event log, by id.
type eventLogV2Query struct {
	pageQuery
	// After keeps the events above this id, and Before those below it: each an event id, read
	// by integerPositions rather than by the decoder, so one that is no id is the log's own
	// refusal, POSITION_INVALID, naming the field (R5).
	After  *string `query:"after"`
	Before *string `query:"before"`
	// Sort is id, oldest first, or -id, newest first.
	Sort querySort `query:"sort" enum:"id" default:"id"`
}

// ownerEventsV2Query is the query of an issue's and a project document's event log: one page of
// it, by seq.
type ownerEventsV2Query struct {
	pageQuery
	// After keeps the events above this seq, and Before those below it.
	After  *string `query:"after"`
	Before *string `query:"before"`
	// IDs keeps these events alone, by event id, at most maxEventIDs of them.
	IDs []int64 `query:"ids,comma" min:"1"`
	// Sort is seq, oldest first, or -seq, newest first.
	Sort querySort `query:"sort" enum:"seq" default:"seq"`
}

// maxEventIDs is the most event ids one read of an owner's log names, as parseEventIDs holds
// /api/v1's to.
const maxEventIDs = 50

// referencesV2Query is GET /api/v2/references's query: the edges of one node.
type referencesV2Query struct {
	pageQuery
	// To reads the backlinks of this dispatch:// reference and From its links; exactly one.
	To   string `query:"to"`
	From string `query:"from"`
	// Kind keeps only these edge kinds.
	Kind []string `query:"kind,comma"`
	// After keeps the mentions above this source_seq, up to the event log's safe horizon, and
	// Before those below it.
	After  *string `query:"after"`
	Before *string `query:"before"`
	// Sort is -created_at, newest first, created_at, source_seq or -source_seq.
	Sort querySort `query:"sort" enum:"created_at,source_seq" default:"-created_at"`
}

// referencesV2 is the body GET /api/v2/references answers: the envelope of the node's edges, with
// the node and the kinds this build does not emit beside it.
type referencesV2 struct {
	Node         graphNodeRead `json:"node"`
	UnknownKinds []string      `json:"unknown_kinds,omitempty"`
	page[model.GraphEdge]
}

// myThreadsV2Query is GET /api/v2/me/threads's query.
type myThreadsV2Query struct {
	pageQuery
	// Kind is the kinds of row answered; none is reply alone.
	Kind []string `query:"kind,comma" enum:"reply,agent"`
	// State is unread, inbox or dismissed.
	State string `query:"state" enum:"unread,inbox,dismissed" default:"unread"`
	// After and Before bound a page of the dismissed threads, each the dismissed_at and thread_id
	// of a row: After keeps the rows dismissed after it, oldest dismissal first, and Before the
	// rows dismissed before it, newest dismissal first where no After is given.
	After  *string `query:"after"`
	Before *string `query:"before"`
	// Issue and Artifact narrow the rows to one owner's threads.
	Issue    string `query:"issue"`
	Artifact string `query:"artifact"`
}

// myThreadsV2 is the body GET /api/v2/me/threads answers: the envelope, its total null under
// state=dismissed (logPage), with the snapshot every row was read in beside it.
type myThreadsV2 struct {
	logPage[model.MyReplyRow]
	AsOf time.Time `json:"as_of"`
}

// logPositions are the after and before one read of a log names, each nil where it names none.
type logPositions struct {
	after, before *int64
}

// integerPositions reads the after and before a request gives a log whose position is an integer
// (R5): each an integer of 0 or more, or 400 POSITION_INVALID naming the parameter and the field.
// An after above the last position the log has written is the same refusal, as there is nothing
// above it to read; last reads that position, and only when an after is given.
func integerPositions(after, before *string, field string, last func() (int64, error)) (logPositions, error) {
	var positions logPositions
	for _, position := range []struct {
		name string
		raw  *string
		into **int64
	}{{"after", after, &positions.after}, {"before", before, &positions.before}} {
		if position.raw == nil {
			continue
		}
		value, err := strconv.ParseInt(*position.raw, 10, 64)
		if err != nil || value < 0 {
			return logPositions{}, errorf(apicode.PositionInvalid,
				"%s reads this log's %s, an integer of 0 or more; %q is none", position.name, field, *position.raw)
		}
		*position.into = &value
	}
	if positions.after == nil {
		return positions, nil
	}
	highest, err := last()
	if err != nil {
		return logPositions{}, err
	}
	if *positions.after > highest {
		return logPositions{}, errorf(apicode.PositionInvalid,
			"after is %d, above this log's last %s, %d: no %s above it has been written", *positions.after, field, highest, field)
	}
	return positions, nil
}

// knownPosition is a last position integerPositions reads where the caller already holds it.
func knownPosition(last int64) func() (int64, error) {
	return func() (int64, error) { return last, nil }
}

// highestEventID is the highest event id written, the last position of the global event log and
// of the references: max(id), not the safe horizon, so an after the next read will reach is no
// refusal.
func (s *server) highestEventID(ctx context.Context) func() (int64, error) {
	return func() (int64, error) {
		var highest int64
		if err := s.deps.Store.Pool.QueryRow(ctx, `select coalesce(max(id), 0) from events`).Scan(&highest); err != nil {
			return 0, fmt.Errorf("read the highest event id: %w", err)
		}
		return highest, nil
	}
}

// logOrder is the order one read of a log is answered in: the sort the request names, and where it
// names none, up the log's position field from an after and down it from a before that comes
// alone, and otherwise the route's own default. A before reads down because a page ends at it: a
// before past the end of the log answers its newest page (R5).
func logOrder(r *http.Request, sort querySort, field string, positions logPositions) querySort {
	if decoded, ok := decodedV2(r); ok {
		if _, named := decoded.values["sort"]; named {
			return sort
		}
	}
	switch {
	case positions.after != nil:
		return querySort{Field: field}
	case positions.before != nil:
		return querySort{Field: field, Descending: true}
	}
	return sort
}

// sqlDirection is the ORDER BY direction of a sort.
func sqlDirection(sort querySort) string {
	if sort.Descending {
		return "desc"
	}
	return "asc"
}

// listEventLogV2 answers GET /api/v2/events: one page of the global event log by id, capped at the
// safe horizon as GET /api/v1/events/log is (listEventLog), with a null total.
func (s *server) listEventLogV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[eventLogV2Query](r)
	positions, err := integerPositions(query.After, query.Before, eventIDPosition.Field, s.highestEventID(r.Context()))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	through, err := s.sinceHorizon(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	order := logOrder(r, query.Sort, eventIDPosition.Field, positions)
	after := int64(0)
	if positions.after != nil {
		after = *positions.after
	}
	arguments := []any{after, through}
	statement := eventSelect + ` where e.id > $1 and e.id <= $2`
	if positions.before != nil {
		arguments = append(arguments, *positions.before)
		statement += ` and e.id < $3`
	}
	arguments = append(arguments, query.Limit, query.Offset)
	statement += fmt.Sprintf(` order by e.id %s limit $%d offset $%d`, sqlDirection(order), len(arguments)-1, len(arguments))
	events, err := s.readEventRows(r.Context(), statement, arguments...)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, logPage[model.Event]{Items: events, Total: nil, Limit: query.Limit, Offset: query.Offset})
}

// ownerEventsRead is one read of an owner's event log, as an /api/v2 request names it.
type ownerEventsRead struct {
	logPositions
	ids  []int64
	sort querySort
	page pageQuery
}

// ownerEventsReadOf reads an owner's log's query: its positions against last, the log's last seq,
// its ids, and its order.
func ownerEventsReadOf(r *http.Request, query ownerEventsV2Query, last int64) (ownerEventsRead, error) {
	positions, err := integerPositions(query.After, query.Before, seqPosition.Field, knownPosition(last))
	if err != nil {
		return ownerEventsRead{}, err
	}
	if len(query.IDs) > maxEventIDs {
		return ownerEventsRead{}, errorf(apicode.InvalidQuery, "ids names %d events; it names at most %d", len(query.IDs), maxEventIDs)
	}
	return ownerEventsRead{
		logPositions: positions, ids: query.IDs,
		sort: logOrder(r, query.Sort, seqPosition.Field, positions), page: query.pageQuery,
	}, nil
}

// readOwnerEventsV2 reads one page of owner's event log, an issue's or a document's, as read names
// it, and the count of every event its filters keep, the page's total.
func (s *server) readOwnerEventsV2(ctx context.Context, owner owner, read ownerEventsRead) (page[model.Event], error) {
	var arguments []any
	argument := func(value any) string {
		arguments = append(arguments, value)
		return "$" + strconv.Itoa(len(arguments))
	}
	var conditions []string
	switch {
	case owner.IssueKey != nil:
		conditions = append(conditions, "e.issue_key = "+argument(*owner.IssueKey))
	case owner.ArtifactID != nil:
		conditions = append(conditions, "e.artifact_id = "+argument(*owner.ArtifactID))
	default:
		return page[model.Event]{}, errorf(apicode.OwnerInvalid, "owner requires exactly one issue or artifact")
	}
	if read.after != nil {
		conditions = append(conditions, "e.seq > "+argument(*read.after))
	}
	if read.before != nil {
		conditions = append(conditions, "e.seq < "+argument(*read.before))
	}
	if len(read.ids) > 0 {
		conditions = append(conditions, "e.id = any("+argument(read.ids)+")")
	}
	where := strings.Join(conditions, " and ")
	var total int
	if err := s.deps.Store.Pool.QueryRow(ctx, `select count(*) from events e where `+where, arguments...).Scan(&total); err != nil {
		return page[model.Event]{}, fmt.Errorf("count the owner's events: %w", err)
	}
	statement := eventSelect + ` where ` + where + ` order by e.seq ` + sqlDirection(read.sort) +
		` limit ` + argument(read.page.Limit) + ` offset ` + argument(read.page.Offset)
	events, err := s.readEventRows(ctx, statement, arguments...)
	if err != nil {
		return page[model.Event]{}, err
	}
	return newPage(events, total, read.page), nil
}

// eventFeedV2Route names the kept answers of GET /api/v2/issues/{key}/events apart from those of
// /api/v1's feed (eventFeedRoute), whose bodies for the same issue and query differ.
const eventFeedV2Route = http.MethodGet + " /api/v2/issues/{key}/events"

// eventFeedV2ETag is eventFeedETag for the /api/v2 feed: the issue, its newest event sequence and
// the query that pages it, read as read names it, under a line naming the version, so no
// validator one version gave matches the other's answer.
func (s *server) eventFeedV2ETag(lastSeq int, createdAt time.Time, read ownerEventsRead, trees bool) string {
	position := func(value *int64) string {
		if value == nil {
			return "none"
		}
		return strconv.FormatInt(*value, 10)
	}
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "version=v2\ncreated=%d\nlast_seq=%d\nafter=%s\nbefore=%s\ndescending=%t\nlimit=%d\noffset=%d\n",
		createdAt.UnixMicro(), lastSeq, position(read.after), position(read.before), read.sort.Descending, read.page.Limit, read.page.Offset)
	for _, id := range read.ids {
		_, _ = fmt.Fprintf(hash, "id=%d\n", id)
	}
	return s.listETag(hash, trees)
}

// listIssueEventsV2 answers GET /api/v2/issues/{key}/events: one page of an issue's event log by
// seq, under the ETag and the kept answers GET /api/v1/issues/{key}/events answers under
// (listIssueEvents), keyed apart from that feed's.
func (s *server) listIssueEventsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	lastSeq, createdAt, held, err := s.issueEventSequence(r.Context(), key)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if !held {
		s.writeHandlerError(w, r, errorf(apicode.IssueNotFound, "no issue %s", key))
		return
	}
	read, err := ownerEventsReadOf(r, v2Query[ownerEventsV2Query](r), int64(lastSeq))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	trees := wantsDisplayTrees(r)
	etag := s.eventFeedV2ETag(lastSeq, createdAt, read, trees)
	if answerNotModified(w, r, etag) {
		return
	}
	kept := listAnswerKeyFor(r, eventFeedV2Route, key, etag)
	if s.listAnswers.answer(w, kept) {
		return
	}
	events, err := s.readOwnerEventsV2(r.Context(), issueOwner(key), read)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	reader := s.readDisplayTrees(r.Context(), trees)
	attachEventTrees(events.Items, reader)
	reader.resolve()
	encoded, err := encodeListAnswer(events)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.writeListAnswer(w, kept, encoded, true)
}

// listArtifactEventsV2 answers GET /api/v2/artifacts/{id}/events: one page of a project document's
// event log by seq, the document held to what GET /api/v1/artifacts/{id}/events holds it to
// (documentOwnerFromRequest).
func (s *server) listArtifactEventsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, owner, err := s.documentOwnerFromRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	var lastSeq int64
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `select last_seq from artifacts where id = $1`, artifact.ID).Scan(&lastSeq); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Deleted since documentOwnerFromRequest read it: a document nothing holds (R7).
			s.writeHandlerError(w, r, errorf(apicode.ArtifactNotFound, "no document %s", artifact.ID))
			return
		}
		s.writeHandlerError(w, r, fmt.Errorf("read the document's event sequence: %w", err))
		return
	}
	read, err := ownerEventsReadOf(r, v2Query[ownerEventsV2Query](r), lastSeq)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	events, err := s.readOwnerEventsV2(r.Context(), owner, read)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	attachEventTrees(events.Items, reader)
	reader.resolve()
	WriteJSON(w, http.StatusOK, events)
}

// listReferencesV2 answers GET /api/v2/references: the read GET /api/v1/references makes
// (readReferences), its since read as after, its edges kept below a before, ordered and paged.
func (s *server) listReferencesV2(w http.ResponseWriter, r *http.Request) {
	_, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return
	}
	query := v2Query[referencesV2Query](r)
	edgeQuery, ref, unknownKinds, err := referencesQueryFor(query.To, query.From, strings.Join(query.Kind, ","), s.deps.ServerURL)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	positions, err := integerPositions(query.After, query.Before, sourceSeqPosition.Field, s.highestEventID(r.Context()))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	edgeQuery.Since = positions.after
	answer, ok := s.readReferences(w, r, edgeQuery, ref, unknownKinds, human)
	if !ok {
		return
	}
	edges := answer.Edges
	if positions.before != nil {
		edges = slices.DeleteFunc(edges, func(edge model.GraphEdge) bool {
			return edge.SourceSeq == nil || *edge.SourceSeq >= *positions.before
		})
	}
	sortReferenceEdges(edges, logOrder(r, query.Sort, sourceSeqPosition.Field, positions))
	WriteJSON(w, http.StatusOK, referencesV2{Node: answer.Node, UnknownKinds: answer.UnknownKinds, page: pageOf(edges, query.pageQuery)})
}

// sortReferenceEdges orders edges, which refs.Edges answers newest first (-created_at), by sort:
// created_at is that order backwards, and source_seq orders by it, an edge with none after every
// edge with one and ties left in the newest-first order.
func sortReferenceEdges(edges []model.GraphEdge, sort querySort) {
	switch {
	case sort.Field == "created_at" && !sort.Descending:
		slices.Reverse(edges)
	case sort.Field == "source_seq":
		slices.SortStableFunc(edges, func(left, right model.GraphEdge) int {
			switch {
			case left.SourceSeq == nil || right.SourceSeq == nil:
				return boolOrder(left.SourceSeq == nil, right.SourceSeq == nil)
			case sort.Descending:
				return cmp.Compare(*right.SourceSeq, *left.SourceSeq)
			}
			return cmp.Compare(*left.SourceSeq, *right.SourceSeq)
		})
	}
}

// boolOrder orders false before true.
func boolOrder(left, right bool) int {
	switch {
	case left == right:
		return 0
	case left:
		return 1
	}
	return -1
}

// listMyThreadsV2 answers GET /api/v2/me/threads: the read GET /api/v1/me/replies makes
// (readMyReplies), paged by limit and offset in every state, the dismissed threads between an after
// and a before in place of a cursor, and every state's total counted but the dismissed threads'.
func (s *server) listMyThreadsV2(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	query := v2Query[myThreadsV2Query](r)
	reply, agent, err := parseThreadKinds(query.Kind)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if !reply && !agent {
		reply = true
	}
	issue, artifact, err := parseThreadOwner(query.Issue, query.Artifact)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	var from, after *myRepliesCursor
	for _, position := range []struct {
		name string
		raw  *string
		into **myRepliesCursor
	}{{"after", query.After, &after}, {"before", query.Before, &from}} {
		if position.raw == nil {
			continue
		}
		if query.State != myRepliesStateDismissed {
			s.writeHandlerError(w, r, errorf(apicode.InvalidQuery,
				"%s reads the dismissed threads alone, state=%s, which are ordered by when each was dismissed", position.name, myRepliesStateDismissed))
			return
		}
		if *position.into, err = dismissedThreadPosition(position.name, *position.raw); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	if after != nil {
		if err := s.dismissedAfterWritten(r.Context(), caller, *query.After, after); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	limit := query.Limit
	response, err := s.readMyReplies(r.Context(), caller, myRepliesRead{
		reply: reply, agent: agent, state: query.State,
		limit: &limit, offset: query.Offset, from: from, after: after, issue: issue, artifact: artifact,
	})
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	// The dismissed threads are every thread the caller has ever dismissed, read a page at a time
	// from a position, so their total is not counted (null), as the global event log's is not;
	// every other state's is the exact count readMyReplies read.
	answer := myThreadsV2{logPage: logPage[model.MyReplyRow]{Items: response.Rows, Limit: query.Limit, Offset: query.Offset}, AsOf: response.AsOf}
	if query.State != myRepliesStateDismissed {
		answer.Total = &response.Total
	}
	WriteJSON(w, http.StatusOK, answer)
}

// dismissedThreadPosition reads a position of the dismissed threads, `<RFC3339 time>,<thread id>`,
// that the parameter name gives: the dismissed_at and thread_id of a row, as the answer writes
// them, the id in any spelling uuid.Parse reads, and kept in its canonical spelling, which is the
// one Postgres's uuid reads for every one of them (it refuses urn:uuid:…). One that is not is 400
// POSITION_INVALID naming the parameter and the two fields (R5).
func dismissedThreadPosition(name, raw string) (*myRepliesCursor, error) {
	at, thread, found := strings.Cut(raw, ",")
	refusal := errorf(apicode.PositionInvalid,
		"%s reads the dismissed threads' %s, an RFC3339 time and a thread id joined by a comma; %q is none", name, dismissedPosition.Field, raw)
	if !found {
		return nil, refusal
	}
	if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
		return nil, refusal
	}
	parsed, err := uuid.Parse(thread)
	if err != nil {
		return nil, refusal
	}
	return &myRepliesCursor{at: at, thread: parsed.String()}, nil
}

// dismissedAfterWritten refuses an after above the last position the caller's dismissed threads
// hold, 400 POSITION_INVALID naming after and the two fields (R5): no thread has been dismissed at
// or after it, so it names no row this log has written.
func (s *server) dismissedAfterWritten(ctx context.Context, caller model.Actor, raw string, after *myRepliesCursor) error {
	var reached bool
	if err := s.deps.Store.Pool.QueryRow(ctx, myDismissedReachesQuery,
		model.CanonicalLogin(caller.ID), after.at, after.thread).Scan(&reached); err != nil {
		return fmt.Errorf("read the newest dismissal of %s: %w", caller.ID, err)
	}
	if !reached {
		return errorf(apicode.PositionInvalid,
			"after is %q, above this log's last %s: no thread has been dismissed at or after it", raw, dismissedPosition.Field)
	}
	return nil
}
