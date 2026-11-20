package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// GET /api/v1/me/inbox is the one read of what is tied to a person (dispatch://INFRA-1152 R1): every
// node that holds a current relation to the caller's own person node, at either end of it, each
// answered once with every relation kind that ties it to them. It takes its relation kinds from the
// store and from no list here, so a kind a later write appends is an entry of this read the day it is
// appended. This file names two kinds, neither as an entry's: the identity tuple
// (graphstore.TupleAliasOf), which ties a session the server proved to its person and puts nothing
// in the answer, and the delivery kind, which runs out of no node of the person's and which the reads
// out of those nodes leave out for an index's sake (meInboxOutOfPerson).
//
// The whole answer is one statement (meInboxStatement), which a reader observes rather than infers
// (TestOneReadAnswersEveryRelationToThePersonsNode). Its rows are the entries, one row an entry and
// nothing else, so count(*) over () over them is the count of the entries, taken after the
// visibility predicate and the kind narrowing. A row's `page` column says which of the page's three
// kinds of row the entry is - ask, thread or agent - or none. What those rows draw is read once for
// the whole page, each kind by the statements the read it replaces runs today over every row of
// that kind on the page, as those reads run them over theirs (meInboxPageReads), so no row is built
// twice and none is built for an entry the page does not show; the first row carries it, as one
// record of arrays of records, each array in the order its statement answers in and each record
// read back by that statement's own scanner (meInboxRecord) and kept by the node it belongs to
// (meInboxPage).
//
// The person is graphstore.PersonNode's canonical login, and a session the server proved theirs
// (the identity tuple into that node, graphstore.TupleAliasOf) is their node too. The read is at
// the event horizon notifications read at (sinceHorizon): a tuple whose event is above it is read
// as if it were not there yet, and a tuple written under no event of its own (the sentinel 0) is
// read whatever the horizon is.

// The two ends of a relation an entry carries: a tuple into the person's node, whose near end is
// the node it comes from, and one from the person's node, whose far end is the node it points at.
const (
	meInboxInbound  = "inbound"
	meInboxOutbound = "outbound"
)

// meInboxOutOfPerson is stated beside the kind a column binds in the two reads out of the person's
// nodes, the skip over their kinds and the walk out of them, so a generic plan of each can take
// graph_tuples_current where that index is partial on leaving the delivery kind out: a column proves
// nothing of that predicate, and this literal does. No delivery runs out of a person or a session,
// so it leaves out nothing those reads would answer.
const meInboxOutOfPerson = `t.kind <> '` + graphstore.TupleDeliveredTo + `'`

// The kinds of row the page draws, as an entry's page column names them (meInboxPageRows).
const (
	meInboxPageAsk    = "ask"
	meInboxPageThread = "thread"
	meInboxPageAgent  = "agent"
)

// meInboxQueryKeys is every query key the route declares (meInboxQuery in routes_shapes.go), and
// meInboxIdentityKeys the identity selectors it refuses by the same rule: the read answers for the
// credential's own person alone, so a key naming somebody else selects nothing it could answer.
var (
	meInboxQueryKeys    = []string{"limit", "offset", "kind", "state", "cursor", "issue", "artifact", "project"}
	meInboxIdentityKeys = []string{"login", "person", "assignee", "session"}
)

// meInboxNode is the node at the other end of an entry's relations.
type meInboxNode struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// meInboxRelation is one relation kind that ties an entry's node to the person: the end of it the
// person's node holds, and the moment of the newest current tuple of that kind and end.
type meInboxRelation struct {
	Kind      string    `json:"kind"`
	Direction string    `json:"direction" enum:"inbound,outbound"`
	At        time.Time `json:"at"`
}

// meInboxAgent is an Agent row of the page with the conversation it draws: the one-off message the
// person sent a session, and every reply under it in order, each with its deliveries.
type meInboxAgent struct {
	model.MyReplyRow
	Conversation messageRead `json:"conversation"`
}

// meInboxEntry is one node tied to the person. Ask, Thread and Agent are the row the page draws for
// that node, at most one of them, and none for a node the page draws no row for.
type meInboxEntry struct {
	Node      meInboxNode       `json:"node"`
	Relations []meInboxRelation `json:"relations"`
	// Moment is when the entry last moved: under a state the time that state's own list orders the
	// row by (the newest reply somebody else wrote, or the dismissal), and otherwise the newest of
	// its relations.
	Moment time.Time         `json:"moment"`
	Ask    *inboxAsk         `json:"ask,omitempty"`
	Thread *model.MyReplyRow `json:"thread,omitempty"`
	Agent  *meInboxAgent     `json:"agent,omitempty"`
}

// meInboxResponse is what GET /api/v1/me/inbox answers.
type meInboxResponse struct {
	Person     string         `json:"person"`
	AsOf       time.Time      `json:"as_of"`
	Horizon    int64          `json:"horizon"`
	Total      int            `json:"total"`
	Limit      int            `json:"limit"`
	Offset     int            `json:"offset"`
	NextCursor string         `json:"next_cursor,omitzero"`
	Entries    []meInboxEntry `json:"entries"`
}

// meInboxRequest is a request's query, read and checked (parseMeInboxRequest).
type meInboxRequest struct {
	// state is the page's narrowing - unread, inbox or dismissed - or "" for the tuple walk.
	state string
	// kinds narrows the answer to the entries of these relation kinds, nil for every kind.
	kinds    []string
	limit    *int
	offset   int
	cursor   *meInboxCursor
	issue    string
	artifact *string
	project  string
}

// owned is whether the request names one owner, an issue or a document. No attribute every node
// carries says which issue or document it belongs to, so an owner keeps the page's rows of that
// owner alone (meInboxPageRows) and no other entry; an Agent row has no owner, so it is none of
// them. A project is a base attribute of every node that has one (graph_nodes.project_key), and
// narrows every entry by it.
func (request meInboxRequest) owned() bool {
	return request.issue != "" || request.artifact != nil
}

// meInboxCursor is where a page resumes: the last entry the page before it answered, in the order
// the entries are read in.
type meInboxCursor struct {
	moment   time.Time
	kind, id string
}

func (cursor meInboxCursor) String() string {
	return base64.RawURLEncoding.EncodeToString([]byte(
		cursor.moment.UTC().Format(time.RFC3339Nano) + "\n" + cursor.kind + "\n" + cursor.id))
}

// parseMeInboxCursor reads one ?cursor=, which is a next_cursor this route answered.
func parseMeInboxCursor(raw string) (*meInboxCursor, error) {
	refusal := errorf(http.StatusBadRequest, "INVALID_QUERY", "cursor is one this route answered in next_cursor")
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, refusal
	}
	parts := strings.SplitN(string(decoded), "\n", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return nil, refusal
	}
	moment, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, refusal
	}
	return &meInboxCursor{moment: moment, kind: parts[1], id: parts[2]}, nil
}

// parseMeInboxRequest reads the query of GET /api/v1/me/inbox. A key the route does not declare is
// refused by name rather than ignored, and so is an identity selector, by the same rule: the
// Params struct of a route is documentation no handler reads (routes_shapes.go), so this is where
// the route holds its query to what it declares.
func parseMeInboxRequest(r *http.Request) (meInboxRequest, error) {
	query := r.URL.Query()
	for _, key := range slices.Sorted(func(yield func(string) bool) {
		for key := range query {
			if !yield(key) {
				return
			}
		}
	}) {
		switch {
		case slices.Contains(meInboxQueryKeys, key):
		case slices.Contains(meInboxIdentityKeys, key):
			return meInboxRequest{}, errorf(http.StatusBadRequest, "INVALID_QUERY",
				"%s is not a query parameter of GET /api/v1/me/inbox, which answers for the credential's own person alone", key)
		default:
			return meInboxRequest{}, errorf(http.StatusBadRequest, "INVALID_QUERY",
				"%s is not a query parameter of GET /api/v1/me/inbox, which takes %s", key, strings.Join(meInboxQueryKeys, ", "))
		}
	}
	var request meInboxRequest
	switch value := strings.TrimSpace(query.Get("state")); value {
	case "", myRepliesStateUnread, myRepliesStateInbox, myRepliesStateDismissed:
		request.state = value
	default:
		return meInboxRequest{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "state is %s, %s or %s",
			myRepliesStateUnread, myRepliesStateInbox, myRepliesStateDismissed)
	}
	for _, listed := range query["kind"] {
		for _, kind := range strings.Split(listed, ",") {
			if kind = strings.TrimSpace(kind); kind != "" && !slices.Contains(request.kinds, kind) {
				request.kinds = append(request.kinds, kind)
			}
		}
	}
	if value, present, valid := parseQueryInt(query, "limit", 1, myAnswersMaxLimit); present {
		if !valid {
			return meInboxRequest{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "limit must be one integer from 1 to %d", myAnswersMaxLimit)
		}
		request.limit = &value
	}
	if value, present, valid := parseQueryInt(query, "offset", 0, math.MaxInt); present {
		if !valid {
			return meInboxRequest{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "offset must be one non-negative integer")
		}
		request.offset = value
	}
	if raw := strings.TrimSpace(query.Get("cursor")); raw != "" {
		cursor, err := parseMeInboxCursor(raw)
		if err != nil {
			return meInboxRequest{}, err
		}
		request.cursor = cursor
	}
	issue, artifact, err := parseThreadOwner(query.Get("issue"), query.Get("artifact"))
	if err != nil {
		return meInboxRequest{}, err
	}
	request.issue, request.artifact = issue, artifact
	request.project = strings.TrimSpace(query.Get("project"))
	return request, nil
}

// listMeInbox answers GET /api/v1/me/inbox: the entries tied to the caller's person, newest moment
// first, in one statement, and the rows the page draws for those it draws.
//
// The statement is the whole of the database work the handler does: actingPerson resolves the
// person before it, the horizon is read in process (sinceHorizon, whose observation runs on the
// horizon's own pool), and what follows it is Go over the rows it answered. The display trees the
// page asks for (Dispatch-Trees: 1) are read in it too, and a text it holds no tree of is read for
// display and stored back after the answer, off the request, as every reader's misses are. The
// listener's roster is read off the stored answer in it, and asked of the listener alone where that
// answer is stale.
func (s *server) listMeInbox(w http.ResponseWriter, r *http.Request) {
	request, err := parseMeInboxRequest(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	horizon, err := s.sinceHorizon(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees := wantsDisplayTrees(r)
	readsRoster := s.deps.Envoy != nil && !s.deps.ListenerRosterUncached
	built, known := s.rosters.known()
	statement, args, err := meInboxStatement(caller, horizon, request, trees, readsRoster, known)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), statement, args...)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("read the inbox: %w", err))
		return
	}
	read, err := readMeInboxRows(rows, s.deps.ServerURL)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	response := meInboxResponse{
		Person: model.CanonicalLogin(caller.ID), AsOf: read.asOf, Horizon: horizon,
		Total: read.total, Offset: request.offset, Entries: []meInboxEntry{},
	}
	if request.limit != nil {
		response.Limit = *request.limit
	}
	reader := s.readDisplayTreesHeld(r.Context(), trees, read.trees)
	if err := s.meInboxAsks(r.Context(), read, built, reader); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for index := range read.entries {
		entry := &read.entries[index]
		if entry.Agent != nil {
			entry.Agent.Conversation.Message.shape(reader)
			entry.Agent.Conversation.Replies = shapedMessages(entry.agentReplies, reader)
		}
		// An entry's rows are pointers, so a tree the reader fills below lands in the answer.
		response.Entries = append(response.Entries, entry.meInboxEntry)
	}
	reader.resolve()
	// Each Reply and Agent row's subject carries the reading of its own line, as listMyReplies's
	// rows do: the thread root's first line cut by refs.FirstLine, which no writer stores a tree of,
	// so each is read where it stands (shapeBuilt), neither asked of the table nor stored in it.
	shapeBuilt(trees, func(built *displayTreeReader) {
		for _, entry := range response.Entries {
			if entry.Thread != nil {
				built.text(&entry.Thread.SubjectTree, entry.Thread.Subject)
			}
			if entry.Agent != nil {
				built.text(&entry.Agent.SubjectTree, entry.Agent.Subject)
			}
		}
	})
	if count := len(response.Entries); count > 0 && read.remaining > request.offset+count {
		last := response.Entries[count-1]
		response.NextCursor = meInboxCursor{moment: last.Moment, kind: last.Node.Kind, id: last.Node.ID}.String()
	}
	writeMeInboxAnswer(w, response)
}

// writeMeInboxAnswer writes response with the bytes WriteJSON writes for it, its entries one at a
// time: the envelope is encoded with no entry and each entry into its array in turn, so no buffer
// holds the whole of a page at once - several megabytes for the person whose history is a hundred
// times the median, which the encoder otherwise grows by doubling and the collector then scans.
func writeMeInboxAnswer(w http.ResponseWriter, response meInboxResponse) {
	entries := response.Entries
	response.Entries = []meInboxEntry{}
	envelope, err := json.Marshal(response)
	if err != nil {
		response.Entries = entries
		WriteJSON(w, http.StatusOK, response)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	out := bufio.NewWriterSize(w, 64<<10)
	// The envelope ends with its empty entries, `[]}`, so the entries go between its last two bytes.
	_, err = out.Write(envelope[:len(envelope)-2])
	for index := 0; err == nil && index < len(entries); index++ {
		if index > 0 {
			err = out.WriteByte(',')
		}
		var entry []byte
		if entry, err = json.Marshal(entries[index]); err == nil {
			_, err = out.Write(entry)
		}
	}
	if err == nil {
		_, err = out.WriteString("]}\n")
	}
	if err == nil {
		err = out.Flush()
	}
	if err != nil {
		slog.Warn("dispatch: write json failed", "error", err)
	}
}

// meInboxAsks finishes the ask rows of the page: the details every list of asks carries beside its
// row (queueAskDetails: the identity event, the inbound edge count and the part ids), each thread's
// replies with the targets each names, its attempts and its block, its rewordings, followers and
// reach (loadAskThreads, the route's reach read off the roster), then each row's shape, in the
// order listInbox takes them.
func (s *server) meInboxAsks(ctx context.Context, read *meInboxRead, built routeRoster, reader *displayTreeReader) error {
	var asks []*model.Ask
	for index := range read.entries {
		if ask := read.entries[index].Ask; ask != nil {
			asks = append(asks, &ask.Ask)
		}
	}
	if len(asks) == 0 {
		return nil
	}
	if err := applyOpenedEventIDs(asks, read.openedEvents); err != nil {
		return err
	}
	applyAskBacklinkCounts(asks, read.backlinkCounts)
	applyAskPartIDs(asks, read.askParts.IDs(asks))
	routed := false
	for index := range read.entries {
		if thread := read.entries[index].askThread; thread != nil && thread.Route != nil {
			routed = true
		}
	}
	var roster routeRoster
	if routed {
		roster = s.meInboxRoster(ctx, read.roster, built)
	}
	for index := range read.entries {
		entry := &read.entries[index]
		if entry.Ask == nil {
			continue
		}
		thread := *entry.askThread
		if routed {
			thread.IssueRouteReach = roster.reach(thread.Route)
		}
		thread.Replies = shapedComments(entry.askReplies, reader)
		thread.Edits = askEditReads(entry.askEdits, reader)
		entry.Ask.Thread = thread
		// The row's shape is taken once the ask is final: the part ids of its options and of its
		// answer are written into the row these fields stand in front of above.
		entry.Ask.shape(reader)
	}
	return nil
}

// meInboxRoster is the listener's roster for the page's routes, as routeRosterOrUnknown answers it:
// none where no listener is configured, the listener's own answer where this process asks it on
// every read, and otherwise the stored answer the statement read (record, read beside built, the
// roster this process had built when the statement was written), fresh or fetched again. A stored
// answer this process cannot read is logged, and every route then reads unknown.
func (s *server) meInboxRoster(ctx context.Context, record meInboxRecord, built routeRoster) routeRoster {
	if s.deps.Envoy == nil {
		return routeRoster{reason: model.RouteReasonUnreachable}
	}
	if s.deps.ListenerRosterUncached {
		return s.listRouteRoster(ctx)
	}
	roster, fresh, now, seen, err := s.rosters.stored(record, built)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: routes read unknown", "error", fmt.Errorf("read the stored listener answer: %w", err))
		}
		return routeRoster{reason: model.RouteReasonUnreachable}
	}
	return s.routeRosterAfterStored(ctx, roster, fresh, now, seen)
}

// meInboxRead is what the statement answered: the page's entries in order, and what it read once for
// every row of the page (meInboxPage), each record read by its own statement's scanner. serverURL is
// this server's deployment, which a Reply and an Agent row's lines are cut against (refs.FirstLine).
type meInboxRead struct {
	asOf             time.Time
	total, remaining int
	entries          []meInboxRow
	openedEvents     map[string]int64
	backlinkCounts   map[string]int
	askParts         refs.AskPartRows
	trees            map[[32]byte]json.RawMessage
	roster           meInboxRecord
	page             meInboxPage
	serverURL        string
}

// meInboxPage is what the page's rows draw, read once for the whole page (meInboxPageReads) and kept
// by the node each part belongs to: each Ask row with its reach, its thread's replies (each with the
// targets it names, its attempts and the block it names), its rewordings and its followers; each
// Reply row; and each Agent row's root, the replies under it (each with its attempts and its block)
// and its session's name. The ask details every list of asks carries beside its rows are kept where
// meInboxAsks reads them (openedEvents, backlinkCounts and askParts).
type meInboxPage struct {
	asks      map[string]inboxAsk
	reaches   map[string]askReach
	replies   map[string][]model.Comment
	edits     map[string][]model.AskEdit
	followers map[string][]model.AskFollower
	threads   map[string]model.MyReplyRow
	roots     map[string]model.Message
	chains    map[string][]model.Message
	titles    map[string]string
}

// meInboxRow is one entry as the statement answered it, with the parts of its page row that are
// finished once every row is read (meInboxAsks) or shaped for the request's reader.
type meInboxRow struct {
	meInboxEntry
	// page is the kind of page row the entry is, its row's page column, or "" for none.
	page         string
	askThread    *askThread
	askReplies   []model.Comment
	askEdits     []model.AskEdit
	agentReplies []model.Message
}

// meInboxRecord is one record a column of the statement holds, read as a row of the statement it
// was spliced from (pgx.Row), so that statement's own scanner reads it: the field count is held to
// the scanner's, and each field is decoded by its own type, as a column of that statement would be.
type meInboxRecord struct {
	types *pgtype.Map
	raw   []byte
}

func (record meInboxRecord) Scan(dest ...any) error {
	if fields := pgtype.NewCompositeBinaryScanner(record.types, record.raw).FieldCount(); fields != len(dest) {
		return fmt.Errorf("read a record of %d fields into %d", fields, len(dest))
	}
	return record.types.Scan(pgtype.RecordOID, pgtype.BinaryFormatCode, record.raw, pgtype.CompositeFields(dest))
}

// meInboxColumns is what each row of the statement holds, in its select's order.
type meInboxColumns struct {
	asOf             time.Time
	roster           pgtype.UndecodedBytes
	total, remaining *int64
	// carries says the row carries what the statement read once for the page (once, one record of
	// meInboxOnceColumns' fields, null on every other row): the first entry's row, or the one row a
	// page of no entry answers.
	carries                bool
	once                   pgtype.UndecodedBytes
	nodeKind, nodeID, page *string
	moment                 *time.Time
	kinds, directions      []string
	ats                    []time.Time
	agent                  pgtype.UndecodedBytes
}

// meInboxOnceColumns is what the statement read once for the page, each an array of the records one
// statement answered over every row of its kind (meInboxPageReads), in that statement's order.
type meInboxOnceColumns struct {
	asks, opened, backlinks, parts, reach            []pgtype.UndecodedBytes
	replies, replyNamed, replyAttempts               []pgtype.UndecodedBytes
	askBlocks, commentBlocks, edits, followers       []pgtype.UndecodedBytes
	threads                                          []pgtype.UndecodedBytes
	roots, messages, attempts, messageBlocks, titles []pgtype.UndecodedBytes
	trees                                            []pgtype.UndecodedBytes
}

func (once *meInboxOnceColumns) targets() []any {
	return []any{
		&once.asks, &once.opened, &once.backlinks, &once.parts, &once.reach,
		&once.replies, &once.replyNamed, &once.replyAttempts, &once.askBlocks, &once.commentBlocks, &once.edits, &once.followers,
		&once.threads,
		&once.roots, &once.messages, &once.attempts, &once.messageBlocks, &once.titles,
		&once.trees,
	}
}

func (columns *meInboxColumns) targets() []any {
	return []any{
		&columns.asOf, &columns.roster, &columns.total, &columns.remaining, &columns.carries, &columns.once,
		&columns.nodeKind, &columns.nodeID, &columns.moment, &columns.kinds, &columns.directions, &columns.ats,
		&columns.page, &columns.agent,
	}
}

// readMeInboxRows reads every row the statement answered. The statement answers one row however
// few entries there are, so that its now() is read: a row naming no node is that row alone. Each
// entry's own columns are read as its row arrives, and its page row is put together from what the
// statement read once for the page (assemble) once every row is in.
func readMeInboxRows(rows pgx.Rows, serverURL string) (*meInboxRead, error) {
	defer rows.Close()
	read := &meInboxRead{
		openedEvents: map[string]int64{}, backlinkCounts: map[string]int{},
		askParts: refs.AskPartRows{}, trees: map[[32]byte]json.RawMessage{}, serverURL: serverURL,
	}
	types := rows.Conn().TypeMap()
	answered := false
	for rows.Next() {
		answered = true
		var columns meInboxColumns
		if err := rows.Scan(columns.targets()...); err != nil {
			return nil, fmt.Errorf("read the inbox: %w", err)
		}
		read.asOf = columns.asOf
		// The counts are the entries' window counts, read off the head row so that a page past
		// the last entry still says how many there are.
		read.total, read.remaining = int(*columns.total), int(*columns.remaining)
		if columns.roster != nil {
			read.roster = meInboxRecord{types: types, raw: columns.roster}
		}
		if columns.carries {
			var once meInboxOnceColumns
			if err := (meInboxRecord{types: types, raw: columns.once}).Scan(once.targets()...); err != nil {
				return nil, fmt.Errorf("read what the inbox read for its page: %w", err)
			}
			if err := read.readPage(types, &once); err != nil {
				return nil, err
			}
		}
		if columns.nodeKind == nil {
			continue
		}
		row, err := read.entry(types, &columns)
		if err != nil {
			return nil, err
		}
		read.entries = append(read.entries, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the inbox: %w", err)
	}
	if !answered {
		return nil, errMeInboxNoRow
	}
	for index := range read.entries {
		if err := read.assemble(&read.entries[index]); err != nil {
			return nil, err
		}
	}
	return read, nil
}

// records is each of raw as a record of the statement.
func records(types *pgtype.Map, raw []pgtype.UndecodedBytes) []meInboxRecord {
	read := make([]meInboxRecord, len(raw))
	for index := range raw {
		read[index] = meInboxRecord{types: types, raw: raw[index]}
	}
	return read
}

// entry reads one row naming a node: its relations, its page column, and an Agent row's row itself
// (scanMyAgentRow), which the page rows read once for their order and the entry keeps.
func (read *meInboxRead) entry(types *pgtype.Map, columns *meInboxColumns) (meInboxRow, error) {
	row := meInboxRow{meInboxEntry: meInboxEntry{
		Node:      meInboxNode{Kind: *columns.nodeKind, ID: *columns.nodeID},
		Relations: make([]meInboxRelation, len(columns.kinds)),
		Moment:    *columns.moment,
	}}
	for index := range columns.kinds {
		row.Relations[index] = meInboxRelation{Kind: columns.kinds[index], Direction: columns.directions[index], At: columns.ats[index]}
	}
	if columns.page != nil {
		row.page = *columns.page
	}
	if row.page == meInboxPageAgent {
		listed, err := scanMyAgentRow(meInboxRecord{types: types, raw: columns.agent}, read.serverURL)
		if err != nil {
			return meInboxRow{}, fmt.Errorf("read the %s row of %s %s: %w", row.page, row.Node.Kind, row.Node.ID, err)
		}
		row.Agent = &meInboxAgent{MyReplyRow: listed}
	}
	return row, nil
}

// assemble puts an entry's page row together from the page (meInboxPage): an Ask row as listInbox
// reads one, with its thread as loadAskThreads reads it - the reply chain, the rewordings, the
// followers and the claim and route, answers left empty as an Inbox row is an open ask; a Reply row
// as listMyReplies reads one; and an Agent row's session named as nameAgentSessions names it, with
// the conversation it draws as hydrateMessageThreads reads one, the root and every reply under it
// oldest first.
func (read *meInboxRead) assemble(row *meInboxRow) error {
	page := &read.page
	missing := func(what string) error {
		return fmt.Errorf("read the %s row of %s %s: the statement read no %s of it", row.page, row.Node.Kind, row.Node.ID, what)
	}
	switch row.page {
	case meInboxPageAsk:
		ask, held := page.asks[row.Node.ID]
		if !held {
			return missing("ask")
		}
		thread := askThread{
			Answers:   []model.AskAnswerWithTree{},
			Followers: append([]model.AskFollower{}, page.followers[row.Node.ID]...),
		}
		if reach, held := page.reaches[row.Node.ID]; held {
			thread.askReach = reach
		}
		row.Ask, row.askThread = &ask, &thread
		row.askReplies = append([]model.Comment{}, page.replies[row.Node.ID]...)
		row.askEdits = append([]model.AskEdit{}, page.edits[row.Node.ID]...)
	case meInboxPageThread:
		thread, held := page.threads[row.Node.ID]
		if !held {
			return missing("thread")
		}
		row.Thread = &thread
	case meInboxPageAgent:
		root, held := page.roots[row.Node.ID]
		if !held {
			return missing("message")
		}
		row.Agent.Session.Name = delivery.SessionTitle(page.titles, row.Agent.Session.ID)
		row.Agent.Conversation.Message.Message = root
		row.agentReplies = append([]model.Message{}, page.chains[row.Node.ID]...)
	}
	return nil
}

// readPage reads what the statement read once for the page (meInboxPageReads) into read: the Ask
// rows as listInbox reads them (scanAskRead) with the details loadAskDetails reads, each thread's
// replies with the targets each names, its attempts and its named block (hydrateCommentSideTables),
// the rewordings and followers; the Reply rows (scanMyReplyRow); each Agent row's root and the
// replies under it, each message's attempts and the block it names, and the sessions' names; and
// every stored tree their texts read.
func (read *meInboxRead) readPage(types *pgtype.Map, columns *meInboxOnceColumns) error {
	page := meInboxPage{
		asks: map[string]inboxAsk{}, reaches: map[string]askReach{}, replies: map[string][]model.Comment{},
		edits: map[string][]model.AskEdit{}, followers: map[string][]model.AskFollower{},
		threads: map[string]model.MyReplyRow{}, roots: map[string]model.Message{},
		chains: map[string][]model.Message{}, titles: map[string]string{},
	}
	if err := read.readAsks(types, columns, &page); err != nil {
		return err
	}
	for _, record := range records(types, columns.threads) {
		thread, err := scanMyReplyRow(record, read.serverURL)
		if err != nil {
			return fmt.Errorf("read the inbox's Reply rows: %w", err)
		}
		page.threads[thread.ThreadID] = thread
	}
	if err := readAgentConversations(types, columns, &page); err != nil {
		return err
	}
	for _, record := range records(types, columns.trees) {
		if err := texttrees.HoldTree(record, read.trees); err != nil {
			return err
		}
	}
	read.page = page
	return nil
}

// readAsks reads the page's Ask rows and what their threads hold into page, and the details every
// list of asks carries beside its rows into read.
func (read *meInboxRead) readAsks(types *pgtype.Map, columns *meInboxOnceColumns, page *meInboxPage) error {
	for _, record := range records(types, columns.asks) {
		var ask inboxAsk
		var issueKey, issueTitle, issueAssignee, documentProject, documentSlug, documentName *string
		var snoozedUntil *time.Time
		scanned, reply, err := scanAskRead(record,
			&issueKey, &issueTitle, &issueAssignee, &ask.Priority, &documentProject, &documentSlug, &documentName, &snoozedUntil)
		if err != nil {
			return err
		}
		ask.Ask = scanned
		ask.LastReply = reply
		if snoozedUntil != nil {
			stamp := timestampValue(*snoozedUntil)
			ask.SnoozedUntil = &stamp
		}
		if issueKey != nil {
			ask.Issue = &inboxIssue{Key: *issueKey, Title: *issueTitle, Assignee: issueAssignee}
		}
		if documentProject != nil {
			ask.Document = &inboxDocument{Project: *documentProject, Slug: *documentSlug, Name: *documentName}
		}
		page.asks[ask.ID] = ask
	}
	for _, record := range records(types, columns.opened) {
		askID, event, err := events.ScanOpenedEventID(record)
		if err != nil {
			return err
		}
		read.openedEvents[askID] = event
	}
	for _, record := range records(types, columns.backlinks) {
		var target string
		var count int
		if err := record.Scan(&target, &count); err != nil {
			return fmt.Errorf("scan ask backlink count: %w", err)
		}
		read.backlinkCounts[target] = count
	}
	for _, record := range records(types, columns.parts) {
		if err := read.askParts.Scan(record); err != nil {
			return err
		}
	}
	for _, record := range records(types, columns.reach) {
		askID, reach, err := scanAskReach(record)
		if err != nil {
			return err
		}
		page.reaches[askID] = reach
	}
	named := map[string][]model.Mention{}
	for _, record := range records(types, columns.replyNamed) {
		commentID, mention, err := scanCommentMention(record)
		if err != nil {
			return err
		}
		named[commentID] = append(named[commentID], mention)
	}
	attempts := map[string][]model.CommentDelivery{}
	for _, record := range records(types, columns.replyAttempts) {
		attempt, err := scanCommentDelivery(record)
		if err != nil {
			return err
		}
		attempts[attempt.CommentID] = append(attempts[attempt.CommentID], attempt)
	}
	blocks := map[string]*model.ReplyBlock{}
	for kind, owned := range map[refs.ItemKind][]pgtype.UndecodedBytes{refs.ItemAsk: columns.askBlocks, refs.ItemComment: columns.commentBlocks} {
		for _, record := range records(types, owned) {
			replyID, block, err := refs.ScanReplyBlock(record, kind)
			if err != nil {
				return err
			}
			blocks[replyID] = block
		}
	}
	for _, record := range records(types, columns.replies) {
		var askID string
		comment, err := scanComment(record, &askID)
		if err != nil {
			return fmt.Errorf("scan inbox ask reply: %w", err)
		}
		if held, found := named[comment.ID]; found {
			comment.Mentions = held
		}
		if held, found := attempts[comment.ID]; found {
			comment.Deliveries = held
		}
		comment.ReplyBlock = blocks[comment.ID]
		page.replies[askID] = append(page.replies[askID], comment)
	}
	for _, record := range records(types, columns.edits) {
		askID, edit, err := scanInboxAskEdit(record)
		if err != nil {
			return err
		}
		page.edits[askID] = append(page.edits[askID], edit)
	}
	for _, record := range records(types, columns.followers) {
		askID, follower, err := scanInboxAskFollower(record)
		if err != nil {
			return err
		}
		page.followers[askID] = append(page.followers[askID], follower)
	}
	return nil
}

// readAgentConversations reads each Agent row's root and the replies under it into page, each
// message with its attempts and the block it names, and the name of each session they are with.
func readAgentConversations(types *pgtype.Map, columns *meInboxOnceColumns, page *meInboxPage) error {
	attempts := map[string][]model.MessageDelivery{}
	for _, record := range records(types, columns.attempts) {
		attempt, err := scanMessageDelivery(record)
		if err != nil {
			return err
		}
		attempts[attempt.MessageID] = append(attempts[attempt.MessageID], attempt)
	}
	blocks := map[string]*model.ReplyBlock{}
	for _, record := range records(types, columns.messageBlocks) {
		replyID, block, err := refs.ScanReplyBlock(record, refs.ItemMessage)
		if err != nil {
			return err
		}
		blocks[replyID] = block
	}
	held := func(message *model.Message) {
		if made, found := attempts[message.ID]; found {
			message.Deliveries = made
		}
		message.ReplyBlock = blocks[message.ID]
	}
	for _, record := range records(types, columns.roots) {
		root, err := scanMessage(record)
		if err != nil {
			return fmt.Errorf("scan the agent row's message: %w", err)
		}
		held(&root)
		page.roots[root.ID] = root
	}
	for _, record := range records(types, columns.messages) {
		var seedID string
		reply, err := scanMessage(record, &seedID)
		if err != nil {
			return fmt.Errorf("scan the agent row's reply: %w", err)
		}
		held(&reply)
		page.chains[seedID] = append(page.chains[seedID], reply)
	}
	for _, record := range records(types, columns.titles) {
		var session, title string
		if err := record.Scan(&session, &title); err != nil {
			return fmt.Errorf("scan the agent row's session name: %w", err)
		}
		page.titles[session] = title
	}
	return nil
}

// meInboxPlaceholder is a placeholder of a statement spliced into the read's own.
var meInboxPlaceholder = regexp.MustCompile(`\$([0-9]+)`)

// bindFragment is fragment, a statement of another read written over $1, $2..., with each of its
// placeholders bound to the expression of this statement values names at that position: a
// placeholder of this statement, or an expression naming the entry the fragment reads for. Every
// fragment is a constant of this build, so a placeholder past values is a fault of this file, which
// the route's own tests reach on every row kind.
func bindFragment(fragment string, values ...string) string {
	return meInboxPlaceholder.ReplaceAllStringFunc(fragment, func(placeholder string) string {
		position, _ := strconv.Atoi(placeholder[1:])
		if position < 1 || position > len(values) {
			panic(fmt.Sprintf("bindFragment: %s is past the %d values bound to its fragment", placeholder, len(values)))
		}
		return values[position-1]
	})
}

// meInboxArgs is the statement's arguments, each new one bound at the next placeholder.
type meInboxArgs struct {
	values []any
}

func (args *meInboxArgs) bind(value any) string {
	args.values = append(args.values, value)
	return "$" + strconv.Itoa(len(args.values))
}

// meInboxStatement is the one statement of the read and its arguments: the text of the request's
// shape (meInboxShape), built once a process, and the request's own arguments before that shape's
// constants.
func meInboxStatement(caller model.Actor, horizon int64, request meInboxRequest, trees, readsRoster bool, known *time.Time) (string, []any, error) {
	shape := meInboxShape{state: request.state, owned: request.owned(), trees: trees, readsRoster: readsRoster}
	text := meInboxTextOf(shape)
	if text.err != nil {
		return "", nil, text.err
	}
	return text.statement, append(meInboxRequestArgs(caller, horizon, request, shape, known), text.constants...), nil
}

// meInboxShape is what the text of the read's statement depends on, and nothing a request carries
// but this: its state, whether an owner narrows it, whether the page asks for display trees and
// whether the listener's stored roster is read. Every other value of a request is an argument, so
// a shape is one text, built once a process (meInboxTextOf) and prepared once on each connection,
// whose plan Postgres plans once and reuses from the sixth execution: the text is tens of kilobytes
// and takes tens of milliseconds to plan and to build, which a request pays only where it is new.
type meInboxShape struct {
	state              string
	owned              bool
	trees, readsRoster bool
}

// The arguments every request binds first, in meInboxRequestArgs's order. $1, $2 and $3 are
// unreadDirectRepliesArgs': the canonical login, the raw actor id and the session an Agent read
// narrows to, none here. They are bound first because the fragments of the Reply and Agent rows
// this statement splices in name them as $1 to $3 (myRepliesPageRest, myAgentUnreadRoots and their
// siblings), and the person's node is that same canonical login. Then these, at these
// placeholders, and after them the ones only some shapes bind (meInboxSlots).
const (
	meInboxVisibility   = "$4"
	meInboxHorizon      = "$5"
	meInboxKindsParam   = "$6"
	meInboxIssue        = "$7"
	meInboxArtifact     = "$8"
	meInboxProject      = "$9"
	meInboxCursorMoment = "$10"
	meInboxCursorKind   = "$11"
	meInboxCursorID     = "$12"
	meInboxOffset       = "$13"
	meInboxLimit        = "$14"
	// meInboxBound is how many arguments every request binds.
	meInboxBound = 14
)

// meInboxSlots is where a shape names the arguments only some shapes bind - the display trees'
// version and mode where the page asks for trees, and the moment of the roster this process built
// where the stored roster is read - and first, how many arguments a request of the shape binds,
// after which the shape's own constants follow.
type meInboxSlots struct {
	version, mode, known string
	first                int
}

func (shape meInboxShape) slots() meInboxSlots {
	slots := meInboxSlots{first: meInboxBound}
	next := func() string {
		slots.first++
		return "$" + strconv.Itoa(slots.first)
	}
	if shape.trees {
		slots.version = next()
		slots.mode = next()
	}
	if shape.readsRoster {
		slots.known = next()
	}
	return slots
}

// meInboxRequestArgs is a request's own arguments, in the order its shape's text names them
// (meInboxBound, meInboxSlots).
func meInboxRequestArgs(caller model.Actor, horizon int64, request meInboxRequest, shape meInboxShape, known *time.Time) []any {
	var kinds, artifact, moment, kind, id, limit any
	if len(request.kinds) > 0 {
		kinds = request.kinds
	}
	if request.artifact != nil {
		artifact = *request.artifact
	}
	if request.cursor != nil {
		moment, kind, id = request.cursor.moment, request.cursor.kind, request.cursor.id
	}
	if request.limit != nil {
		limit = *request.limit
	}
	args := append(unreadDirectRepliesArgs(caller.ID, nil), refs.VisibilityArg(caller), horizon, kinds,
		request.issue, artifact, request.project, moment, kind, id, request.offset, limit)
	if shape.trees {
		args = append(args, pmdoc.DisplayTreeVersionNow(), int16(pmdoc.DisplayLatest))
	}
	if shape.readsRoster {
		args = append(args, known)
	}
	return args
}

// meInboxText is one shape's statement and the constants it binds after a request's own arguments.
type meInboxText struct {
	statement string
	constants []any
	err       error
}

// meInboxTexts holds each shape's text once it is built, for the life of the process.
var meInboxTexts sync.Map

// meInboxTextOf is shape's text, built on its first request.
func meInboxTextOf(shape meInboxShape) *meInboxText {
	if built, held := meInboxTexts.Load(shape); held {
		return built.(*meInboxText)
	}
	statement, constants, err := meInboxBuild(shape)
	built, _ := meInboxTexts.LoadOrStore(shape, &meInboxText{statement: statement, constants: constants, err: err})
	return built.(*meInboxText)
}

// meInboxBuild writes shape's statement over the request's arguments (meInboxBound, meInboxSlots)
// and answers it with the constants it binds after them.
//
// The relations are read once either way, by the walk: the distinct relation kinds that touch each
// of the person's nodes, read by a skip over an index led by the far end and the kind
// (graph_tuples_as_of_in) and one over graph_tuples_current (one index probe a kind, kinds_in and
// kinds_out), then each kind's current tuples through
// graphstore.CurrentTupleRead, one index range a kind, each relation kind and end once a node
// (tied). With no state the entries are those nodes. Under a state they are the page's own rows
// in that state, read from the caches the page's lists read (meInboxPageRows). Either way the page's
// rows and the walk's relations are put together by grouping the two by node (sides) rather than by
// joining one to the other: the read costs one pass over the person's current tuples and one over
// their page whatever the planner takes either to hold, and never the product of the two, which a
// probe of the tuples between each row's node and the person's made it (the scale check, c-p99:
// 8,002 probes, 71 s of 74, for the person whose history is a hundred times the median,
// since every probe the planner chose read all the person's tuples of a kind and kept the row's),
// and which a join made it under the plan Postgres keeps from the sixth execution, a plan for no
// value of the arguments that takes the page to hold one row and loops over the whole walk for
// each (12.2 s for that person, against 1.6 s for the plan of its own arguments). The visibility
// predicate is over the node's own row of graph_nodes, read by its key for each node, in this
// statement and bound once, and the counts are taken after it. What the page's rows draw is read
// once for the page (meInboxPageReads).
func meInboxBuild(shape meInboxShape) (string, []any, error) {
	slots := shape.slots()
	args := &meInboxArgs{values: make([]any, slots.first)}
	atHorizon := `(t.source_event <= ` + meInboxHorizon + ` or t.source_event = 0)`
	// A relation kind the walk reads: one the data holds, the identity tuple left out, and inside the
	// narrowing when the request names one.
	readKind := func(kind string) string {
		return kind + ` is not null and ` + kind + ` <> '` + graphstore.TupleAliasOf + `'
			and (` + meInboxKindsParam + `::text[] is null or ` + kind + ` = any(` + meInboxKindsParam + `::text[]))`
	}
	// A project narrows every entry by its node's own project, and a node the store holds no row
	// for cannot be judged, so it is left out under the narrowing. An owner keeps the page's rows of
	// that owner alone (meInboxRequest.owned); under a state the entries are those rows already.
	inProject := `(` + meInboxProject + `::text = '' or n.project_key = ` + meInboxProject + `::text)`
	visible := `coalesce((select ` + refs.VisibleRow("n", meInboxVisibility) + ` and ` + inProject + `
			from graph_nodes n where n.kind = g.node_kind and n.id = g.node_id), ` + meInboxProject + `::text = '')`

	var statement strings.Builder
	statement.WriteString(`
	with recursive
	-- The three parameters of unreadDirectRepliesArgs, typed once: a request narrowed to an
	-- owner splices in no Agent fragment, which is what reads the last two.
	direct_args(login, actor_id, session_id) as (select $1::text, $2::text, $3::text),
	person_nodes(kind, id) as (
		select '` + graphstore.KindPerson + `'::text, $1::text
		union
		select proved.from_kind, proved.from_id
		from (` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
		`t.to_kind = '`+graphstore.KindPerson+`' and t.to_id = $1 and t.kind = '`+graphstore.TupleAliasOf+`'
				and t.to_kind <> '`+graphstore.KindValue+`' and `+atHorizon) + `) proved
		where proved.asserted and proved.from_kind = '` + graphstore.KindSession + `'
	),
	` + meInboxKindSkip("kinds_in", "to") + `,
	` + meInboxKindSkip("kinds_out", "from") + `,
	page_rows(node_kind, node_id, page, moment, agent_session, agent) as (` +
		meInboxPageRows(shape, meInboxIssue, meInboxArtifact) + `
	),
	walked(near_kind, near_id, kind, direction, at) as (
		select stated.from_kind, stated.from_id, k.kind, '` + meInboxInbound + `', stated.at
		from kinds_in k
		cross join lateral (` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.at, t.asserted`,
		`t.to_kind = k.node_kind and t.to_id = k.node_id and t.kind = k.kind
				and t.to_kind <> '`+graphstore.KindValue+`' and `+atHorizon) + `) stated
		where ` + readKind("k.kind") + ` and stated.asserted
		union all
		select stated.to_kind, stated.to_id, k.kind, '` + meInboxOutbound + `', stated.at
		from kinds_out k
		cross join lateral (` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.at, t.asserted`,
		`t.from_kind = k.node_kind and t.from_id = k.node_id and t.kind = k.kind
				and t.to_kind <> '`+graphstore.KindValue+`' and `+meInboxOutOfPerson+` and `+atHorizon) + `) stated
		where ` + readKind("k.kind") + ` and stated.asserted
	),
	-- Each relation kind and end that ties a node to the person, once, with its newest moment: the
	-- walk read whole once.
	tied(node_kind, node_id, kind, direction, at) as (
		select w.near_kind, w.near_id, w.kind, w.direction, max(w.at)
		from walked w
		where (w.near_kind, w.near_id) not in (select kind, id from person_nodes)
		group by w.near_kind, w.near_id, w.kind, w.direction
	),
	-- The page's rows and the walk's relations side by side, a node at most once among the page's
	-- rows, and then each node once with what either side holds of it and every relation kind that
	-- ties it to the person: a grouping, which costs the rows of the two whatever the plan, where a
	-- join of the two could loop over one for each row of the other.
	sides(node_kind, node_id, page, page_moment, agent_session, agent, kind, direction, at) as (
		select pr.node_kind, pr.node_id, pr.page, pr.moment, pr.agent_session, pr.agent,
		       null::text, null::text, null::timestamptz
		from page_rows pr
		union all
		select t.node_kind, t.node_id, null, null, null, null, t.kind, t.direction, t.at
		from tied t
	),
	grouped(node_kind, node_id, page, page_moment, agent_session, agent, walk_moment, kinds, directions, ats) as (
		select s.node_kind, s.node_id, max(s.page), max(s.page_moment), max(s.agent_session),
		       (array_agg(s.agent) filter (where s.page = '` + meInboxPageAgent + `'))[1],
		       max(s.at),
		       array_agg(s.kind order by s.kind, s.direction) filter (where s.kind is not null),
		       array_agg(s.direction order by s.kind, s.direction) filter (where s.kind is not null),
		       array_agg(s.at order by s.kind, s.direction) filter (where s.kind is not null)
		from sides s
		group by s.node_kind, s.node_id
	),`)
	if shape.state == "" {
		ownedRows := ``
		if shape.owned {
			ownedRows = `
		  and g.page is not null`
		}
		statement.WriteString(`
	entries as (
		select g.node_kind, g.node_id, g.walk_moment as moment, g.kinds, g.directions, g.ats,
		       g.page, g.agent_session, g.agent
		from grouped g
		where g.kinds is not null` + ownedRows + `
		  and ` + visible + `
	),`)
	} else {
		statement.WriteString(`
	entries as (
		select g.node_kind, g.node_id, g.page_moment as moment,
		       coalesce(g.kinds, '{}') as kinds, coalesce(g.directions, '{}') as directions, coalesce(g.ats, '{}') as ats,
		       g.page, g.agent_session, g.agent
		from grouped g
		where g.page is not null
		  and (` + meInboxKindsParam + `::text[] is null or g.kinds is not null)
		  and ` + visible + `
	),`)
	}
	statement.WriteString(`
	counted as (
		select e.*, count(*) over () as total from entries e
	),
	remaining as (
		select c.*, count(*) over () as remaining from counted c
		where ` + meInboxCursorMoment + `::timestamptz is null
		   or (c.moment, c.node_kind, c.node_id) < (` + meInboxCursorMoment + `::timestamptz, ` + meInboxCursorKind + `::text, ` + meInboxCursorID + `::text)
	),
	-- The page, each entry numbered in the order it is answered in.
	paged as (
		select cut.*, row_number() over () as ord
		from (
			select * from remaining
			order by moment desc, node_kind desc, node_id desc
			offset ` + meInboxOffset + ` limit ` + meInboxLimit + `
		) cut
	),`)
	reads, err := meInboxPageReads(args, slots)
	if err != nil {
		return "", nil, err
	}
	statement.WriteString(reads.ctes)
	roster := `null::record`
	if shape.readsRoster {
		roster = `(select row(stored.*) from (` + bindFragment(routeRosterQuery, slots.known) + `) stored)`
	}
	// The first entry's row carries what was read once for the page, as one record, and a page of
	// no entry its one row; every other row carries none of it.
	const carries = `(entry.ord is null or entry.ord = 1)`
	statement.WriteString(`
	select head.as_of, head.roster, head.total, head.remaining, ` + carries + `,
	       case when ` + carries + ` then row(` + strings.Join(reads.columns, `,
	         `) + `) end,
	       entry.node_kind, entry.node_id, entry.moment, entry.kinds, entry.directions, entry.ats,
	       entry.page, entry.agent
	from (
		select now() as as_of, ` + roster + ` as roster,
		       coalesce((select c.total from counted c limit 1), 0) as total,
		       coalesce((select r.remaining from remaining r limit 1), 0) as remaining
	) head
	left join paged entry on true
	order by entry.moment desc, entry.node_kind desc, entry.node_id desc`)
	return statement.String(), args.values[slots.first:], nil
}

// meInboxKindSkip lists the distinct relation kinds that touch each of the person's nodes at one end
// - to, the tuples into it, read through an index led by the far end and the kind
// (graph_tuples_as_of_in), or from, those out of it, through graph_tuples_current, beside
// meInboxOutOfPerson - by a recursive skip: each step reads the least
// kind past the one before off the index, so a kind costs one probe however many tuples it holds,
// and no kind is named in Go. graphstore.TuplesIntoNode says why a statement naming no kind would
// otherwise read the log.
func meInboxKindSkip(name, end string) string {
	out := ``
	if end == "from" {
		out = ` and ` + meInboxOutOfPerson
	}
	at := func(kind, id string) string {
		return `t.` + end + `_kind = ` + kind + ` and t.` + end + `_id = ` + id + ` and t.to_kind <> '` + graphstore.KindValue + `'` + out
	}
	return name + `(node_kind, node_id, kind) as (
		select n.kind, n.id, (select min(t.kind) from graph_tuples t where ` + at("n.kind", "n.id") + `)
		from person_nodes n
		union all
		select k.node_kind, k.node_id, (select min(t.kind) from graph_tuples t
			where ` + at("k.node_kind", "k.node_id") + ` and t.kind > k.kind)
		from ` + name + ` k
		where k.kind is not null
	)`
}

// meInboxPageRows is the one fragment that selects the rows the page draws, by today's rules (the
// rows of GET /api/v1/inbox?assignee=me and GET /api/v1/me/replies, P3): an Ask row is an open ask on
// an open issue that waits on the person (askedOfPersonExpression), which only the Inbox's own state
// lists; a Reply row is a user_threads row of theirs in the state the request names, less an open
// ask the Inbox lists already (myRepliesOwner); an Agent row is one of their one-off messages to a
// session holding a reply from it (myAgentUnreadRoots and its siblings), on no issue or document,
// so a request narrowed to an owner lists none. A mention or an addressing reaches the person's
// node and writes no user_threads row, so it is no row of the page. A project narrows the entries
// these rows are, by their nodes' project, where every other entry is narrowed (meInboxBuild).
//
// Each row is the node it draws, the kind of row, the moment that state's list orders it by, and an
// Agent row's session and the row itself (myAgentRowColumns), read once here for its order and kept.
// A walk with no state draws the rows of the Inbox's own state.
//
// The Ask rows are the open asks first, read by their state alone, and then the ones of those that
// wait on the person, each with its newest reply; and an owner's test is written only into a shape
// that names an owner. The plan Postgres keeps from the sixth execution knows nothing of the
// arguments: it takes a test of an argument against a column to keep almost no row, and with the
// owner's tests on every shape it took the open asks to be one and read each one's issue, document
// and project by a scan of that whole table, and its newest reply, before it knew whether the ask
// waits on the person at all (13.6 ms of that plan's 21.6 for the person whose history is empty,
// against 0.4 ms in the plan of their own arguments).
func meInboxPageRows(shape meInboxShape, issue, artifact string) string {
	state := shape.state
	if state == "" {
		state = myRepliesStateInbox
	}
	threads, threadMoment := myRepliesInbox, `u.replied_at`
	agents, agentMoment := myAgentInboxRoots, `listed.replied_at`
	switch state {
	case myRepliesStateUnread:
		threads, agents = myRepliesUnread, myAgentUnreadRoots
	case myRepliesStateDismissed:
		threads, threadMoment = myRepliesDismissed, `u.dismissed_at`
		agents, agentMoment = myAgentDismissedRoots, `listed.dismissed_at`
	}
	arms := []string{`
		select u.kind, u.thread_id::text, '` + meInboxPageThread + `', ` + threadMoment + `, null::text, null::record
		` + bindFragment(threads, "$1", issue, artifact)}
	if state == myRepliesStateInbox {
		owner := ``
		if shape.owned {
			owner = `
			  and (` + issue + `::text = '' or a.issue_key = ` + issue + `::text)
			  and (` + artifact + `::uuid is null or a.artifact_id = ` + artifact + `::uuid)`
		}
		arms = append(arms, `
		select '`+graphstore.KindAsk+`', a.id::text, '`+meInboxPageAsk+`', coalesce(lr.created_at, a.created_at), null::text, null::record
		from (
			with open_asks as materialized (
				select id, created_at, issue_key, artifact_id, asked_of from asks where state = 'open'
			)
			select a.id, a.created_at
			from open_asks a`+askedOfPersonJoins+`
			where (i.key is null or i.closed_at is null)
			  and `+askedOfPersonExpression+` = $1`+owner+`
		) a`+lastReplyJoin)
	}
	if !shape.owned {
		arms = append(arms, `
		select '`+graphstore.KindMessage+`', listed.root_id, '`+meInboxPageAgent+`', `+agentMoment+`, listed.session_id, row(listed.*)
		from (`+agents+myAgentRowsSelect+`
		) listed`)
	}
	return strings.Join(arms, `
		union all`)
}

// meInboxReads is what the page's rows draw, as meInboxPageReads writes it: the statement's
// common table expressions that read it, and the columns of the row that carries it, in the order
// meInboxOnceColumns reads them.
type meInboxReads struct {
	ctes    string
	columns []string
}

// meInboxPageReads reads what the page's rows draw, once for the whole page and each kind of row
// by the statements the read it replaces runs today, each over every row of that kind on the page
// as those reads run them over theirs:
//
//   - the Ask rows as listInbox selects them (askReadColumns over askReadFrom, with their issue,
//     document and the caller's snooze), the details loadAskDetails reads
//     (events.OpenedEventIDsStatement, refs.BacklinkCountRead, refs.AskPartRowsStatement) and their
//     threads as loadAskThreads reads them (inboxAskReplyChainsQuery, then the targets each reply
//     names, its attempts and its block, the rewordings, the followers and askReachesQuery);
//   - the Reply rows as listMyReplies selects them over the rows its cut kept (myRepliesPageRest),
//     each read by its user_threads key;
//   - the Agent rows' conversations as hydrateMessageThreads reads them - each root message, every
//     reply under it (messageReplyChainsQuery), each message's attempts (messageDeliveriesQuery) and
//     the block it names (refs.ReplyBlocksStatement) - and the names of their sessions
//     (delivery.ResolveSessionTitlesQuery); each row itself is page_rows', read for its order;
//   - the stored display tree of every text those rows may register with the request's reader,
//     where the page asks for trees (meInboxTrees).
//
// Every statement is over the ids of its kind's rows on the page, so a row is read once whatever
// else the page holds, and a kind the page holds no row of is read over none.
func meInboxPageReads(args *meInboxArgs, slots meInboxSlots) (meInboxReads, error) {
	// The count's statement text is the same whoever counts: who is the acting credential, which
	// this statement has bound once already (meInboxVisibility), and the rest are constants.
	count, rest, err := refs.BacklinkCountRead(model.Actor{}, graphstore.KindAsk, refs.AskBacklinkExclusions)
	if err != nil {
		return meInboxReads{}, err
	}
	askBlocks, err := refs.ReplyBlocksStatement(refs.ItemAsk)
	if err != nil {
		return meInboxReads{}, err
	}
	commentBlocks, err := refs.ReplyBlocksStatement(refs.ItemComment)
	if err != nil {
		return meInboxReads{}, err
	}
	messageBlocks, err := refs.ReplyBlocksStatement(refs.ItemMessage)
	if err != nil {
		return meInboxReads{}, err
	}
	// The ids of each kind's rows, as the array every statement below is written over. They are
	// cast, so that a fragment's `= any($1)` reads each as an array and not as a subquery of one row.
	askIDs := `(select ids from page_asks)::text[]`
	countArgs := []string{askIDs, args.bind(rest[0]), meInboxVisibility}
	for _, value := range rest[2:] {
		countArgs = append(countArgs, args.bind(value))
	}
	replyIDs := `(select ids from ask_reply_ids)`
	threadIDs := `(select ids from page_threads)`
	rootIDs := `(select ids from page_roots)`
	messageIDs := `(select ids from agent_message_ids)`
	sessions := `array(select distinct p.agent_session from paged p where p.page = '` + meInboxPageAgent + `')`
	ctes := `
	page_asks(ids) as materialized (
		select coalesce(array_agg(p.node_id), '{}') from paged p where p.page = '` + meInboxPageAsk + `'
	),
	ask_replies as materialized (
		select * from (` + bindFragment(inboxAskReplyChainsQuery, askIDs) + `) chain
	),
	ask_reply_ids(ids) as materialized (
		select coalesce(array_agg(r.id::uuid), '{}') from ask_replies r
	),
	ask_named as materialized (
		select * from (` + bindFragment(askBlocks, replyIDs, meInboxVisibility) + `) named
	),
	comment_named as materialized (
		select * from (` + bindFragment(commentBlocks, replyIDs, meInboxVisibility) + `) named
	),
	ask_edits(ask_id, previous, edited_by, at) as materialized (
		select * from (` + bindFragment(inboxAskEditsQuery, askIDs) + `) edit
	),
	page_threads(ids) as materialized (
		select coalesce(array_agg(p.node_id::uuid), '{}') from paged p where p.page = '` + meInboxPageThread + `'
	),
	page_roots(ids) as materialized (
		select coalesce(array_agg(p.node_id::uuid), '{}') from paged p where p.page = '` + meInboxPageAgent + `'
	),
	agent_roots as materialized (
		select ` + messageColumns + ` from messages where id = any(` + rootIDs + `::uuid[])
	),
	agent_chain as materialized (
		select * from (` + bindFragment(messageReplyChainsQuery, rootIDs) + `) chain
	),
	agent_message_ids(ids) as materialized (
		select array(select id::uuid from agent_roots union all select id::uuid from agent_chain)
	),
	agent_named as materialized (
		select * from (` + bindFragment(messageBlocks, messageIDs, meInboxVisibility) + `) named
	)`
	trees := `null::record[]`
	if slots.version != "" {
		trees = meInboxTrees(slots, `
			select a.question from asks a where a.id = any(`+askIDs+`::uuid[])
			union all
			select choice.value->>field.name
			from asks a
			cross join lateral jsonb_array_elements(case when jsonb_typeof(a.options) = 'array' then a.options else '[]'::jsonb end) choice
			cross join (values ('label'), ('description')) field(name)
			where a.id = any(`+askIDs+`::uuid[])
			union all
			select a.answer->>'text' from asks a where a.id = any(`+askIDs+`::uuid[])
			union all
			select a.resolution->>'reason' from asks a where a.id = any(`+askIDs+`::uuid[])
			union all
			select r.body from ask_replies r
			union all
			select b.excerpt from ask_named b
			union all
			select b.excerpt from comment_named b
			union all
			select e.previous->>'question' from ask_edits e
			union all
			select choice.value->>field.name
			from ask_edits e
			cross join lateral jsonb_array_elements(case when jsonb_typeof(e.previous->'options') = 'array' then e.previous->'options' else '[]'::jsonb end) choice
			cross join (values ('label'), ('description')) field(name)
			union all
			select r.body from agent_roots r
			union all
			select c.body from agent_chain c
			union all
			select b.excerpt from agent_named b`)
	}
	return meInboxReads{ctes: ctes, columns: []string{
		`array(select row(listed.*) from (
			select ` + askReadColumns + `,
			       i.key, i.title, i.assignee, i.priority, ar.project_key, ar.slug, ar.name,
			       sn.snoozed_until
			` + askReadFrom + `
			left join user_ask_snooze sn on sn.ask_id = a.id and sn.login = $1
			where a.id = any(` + askIDs + `::uuid[])
		) listed)`,
		`array(select row(opened.*) from (` + bindFragment(events.OpenedEventIDsStatement, askIDs) + `) opened)`,
		`array(select row(backlinks.*) from (` + bindFragment(count, countArgs...) + `) backlinks)`,
		`array(select row(parts.*) from (` + bindFragment(refs.AskPartRowsStatement, askIDs) + `) parts)`,
		`array(select row(reach.*) from (` + bindFragment(askReachesQuery, askIDs) + `) reach)`,
		`array(select row(r.*) from ask_replies r)`,
		`array(select row(named.*) from (` + bindFragment(commentMentionsQuery, replyIDs) + `) named)`,
		`array(select row(attempt.*) from (` + bindFragment(commentDeliveriesQuery, replyIDs) + `) attempt)`,
		`array(select row(b.*) from ask_named b)`,
		`array(select row(b.*) from comment_named b)`,
		`array(select row(e.*) from ask_edits e)`,
		`array(select row(follower.*) from (` + bindFragment(inboxAskFollowersQuery, askIDs) + `) follower)`,
		`array(select row(listed.*) from (
			with recursive page as (
				select kept.thread_id, kept.kind, kept.replied_at, kept.dismissed_at, kept.last_reply_id,
				       kept.issue_key, kept.artifact_id, kept.read_through, kept.project_key, kept.unread, kept.version
				from user_threads kept
				where kept.login = $1 and kept.thread_id = any(` + threadIDs + `::uuid[])
			),` + myRepliesPageRest + `
		) listed)`,
		`array(select row(r.*) from agent_roots r)`,
		`array(select row(c.*) from agent_chain c)`,
		`array(select row(attempt.*) from (` + bindFragment(messageDeliveriesQuery, messageIDs) + `) attempt)`,
		`array(select row(b.*) from agent_named b)`,
		`array(select row(titled.*) from (` + bindFragment(delivery.ResolveSessionTitlesQuery, sessions) + `) titled)`,
		trees,
	}}, nil
}

// meInboxTrees is the stored display tree of each of texts that has one (texttrees.ReadStatement),
// keyed as every reader keys them, by the sum of the text (pmdoc.TextSum). texts is every text the
// page's rows may register with the request's reader; a text it leaves out is read for display
// instead, so it errs on the side of naming more.
func meInboxTrees(slots meInboxSlots, texts string) string {
	return `array(select row(tree.*) from (` + bindFragment(texttrees.ReadStatement, slots.version, slots.mode,
		`array(select sha256(convert_to(written.text, 'UTF8')) from (`+texts+`
			) written(text) where written.text <> '')`) + `) tree)`
}

// errMeInboxNoRow is a statement that answered no row at all, which it never does: its head row is
// there however few entries it finds.
var errMeInboxNoRow = errors.New("the inbox statement answered no row")
