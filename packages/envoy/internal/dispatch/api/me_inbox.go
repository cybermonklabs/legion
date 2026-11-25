package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// GET /api/v1/me/inbox is the one read of what is tied to a person (the spec's R1): every
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
// so it leaves out nothing those reads would answer. Under state=dismissed the walk reads the
// relations out of the person into each of the page's nodes by graph_tuples_as_of_in, and states
// none of it (meInboxBuild).
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
	Ask    *openAskRow       `json:"ask,omitempty"`
	Thread *model.MyReplyRow `json:"thread,omitempty"`
	Agent  *meInboxAgent     `json:"agent,omitempty"`
	// ApprovalGate is the approval gate of the issue the entry's row belongs to (approvalGate), as
	// GET /api/v1/issues/{key} answers it, and absent for an entry of no issue: an Agent row, an
	// ask or a thread on a project document, and an entry the page draws no row for.
	ApprovalGate []approvalGateLine `json:"approval_gate,omitempty"`
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

// parseMeInboxCursor reads one ?cursor=, which is a next_cursor this route answered. Under
// state=dismissed the key it resumes after is the moment and the id of a thread root or an Agent
// root (meInboxOrder), which this route writes as a uuid's own text, so a cursor naming anything else
// there is none it answered.
func parseMeInboxCursor(raw, state string) (*meInboxCursor, error) {
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
	if state == myRepliesStateDismissed {
		if id, err := uuid.Parse(parts[2]); err != nil || id.String() != parts[2] {
			return nil, refusal
		}
	}
	return &meInboxCursor{moment: moment, kind: parts[1], id: parts[2]}, nil
}

// parseMeInboxRequest reads the query of GET /api/v1/me/inbox. A key the route does not declare is
// refused by name rather than ignored, and so is an identity selector, by the same rule: the
// Params struct of a route is documentation no handler reads (routes_shapes.go), so this is where
// the route holds its query to what it declares.
func parseMeInboxRequest(r *http.Request) (meInboxRequest, error) {
	query := r.URL.Query()
	for _, key := range slices.Sorted(maps.Keys(query)) {
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
	var err error
	if request.limit, request.offset, err = parseLimitOffset(query); err != nil {
		return meInboxRequest{}, err
	}
	// The Dismissed view is a pile that only grows, so its read answers one page at a time: with
	// no limit it answers myRepliesDismissedPage entries, as GET /api/v1/me/replies does, and the
	// arms are cut to that page before the walk (meInboxDismissedCut). A limit above it is refused
	// by its key above, as it is in every state.
	if request.limit == nil && request.state == myRepliesStateDismissed {
		page := myRepliesDismissedPage
		request.limit = &page
	}
	if raw := strings.TrimSpace(query.Get("cursor")); raw != "" {
		cursor, err := parseMeInboxCursor(raw, request.state)
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
// horizon's own pool), and what follows it is Go over the rows it answered. It runs in a read-only
// transaction of its own, sent with it as one batch (readMeInbox). The display trees the page asks
// for (Dispatch-Trees: 1) are read in it too, and a text it holds no tree of is read for display and
// stored back after the answer, off the request, as every reader's misses are. The listener's
// roster is read off the stored answer in it, and asked of the listener alone where that answer is
// stale.
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
	read, err := s.readMeInbox(r.Context(), statement, args)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	response := meInboxResponse{
		Person: model.CanonicalLogin(caller.ID), AsOf: read.asOf, Horizon: horizon,
		Total: read.total, Offset: request.offset, Entries: make([]meInboxEntry, 0, len(read.entries)),
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

// meInboxSettings are what the read's own transaction sets before its statement (readMeInbox), in
// order: meInboxNoJIT, then meInboxGenericPlan. The read states the settings it was measured under,
// as the search coverage read states its own (embedqueue's coverageSettings).
var meInboxSettings = []string{meInboxNoJIT, meInboxGenericPlan}

const (
	// meInboxNoJIT turns JIT off, so that what the read costs is its plan's and not the server's
	// JIT setting. A server built with LLVM compiles a statement whose estimated cost passes
	// jit_above_cost (100,000 by default), and this statement's estimate passes it once a person's
	// pile is large, whatever page it answers. At the dismissed scale test's seed, 110,000
	// dismissed threads, the Dismissed page's plans are estimated at 109,000 (the generic plan) to
	// 209,000, and Postgres compiled about 1,400 functions on every execution of each of the four
	// the scale test explains (page 1 and the page 50,000 deep, each under the plan of its own
	// arguments and the generic plan): 450 to 490 ms of JIT in an execution of 480 to 520 ms,
	// where the page itself was built in about 30 ms. At 200 dismissed threads, or none, the
	// estimate stays under the threshold and nothing is compiled. So with JIT on the cut page read
	// a p50 of 464 to 795 ms, against 86 ms on a Postgres built without LLVM, and 25 ms with it
	// once JIT was off; every other measurement of this read ran without JIT too - a pod's test
	// server sets it off - and production's setting was never read.
	meInboxNoJIT = `set local jit = off`
	// meInboxGenericPlan has each connection plan the read once and run that plan from then on.
	// pgx sends the statement as a named prepared statement of the connection (its default mode,
	// QueryExecModeCacheStatement, which the batch takes too), and Postgres plans a prepared
	// statement afresh, for the values of that execution, on each of its first five executions on
	// a connection, and only from the sixth may it keep the generic plan. Planning this statement
	// costs about what running it does: at the dismissed scale test's seed the unread count
	// (state=unread&limit=1) ran in about 27 ms and planned in about 27 ms more, so after its one
	// warm-up run its first four measured runs read 54 to 60 ms and every run after them about
	// 27.5 ms - a p95 of 57.6 ms against its 50 ms bound with the pile, and 52.2 ms with no
	// dismissed row at all. The statement is built so that its generic plan, the plan every request
	// runs, reads one page of the Dismissed view however many threads the person has dismissed:
	// each arm is cut to the page by a cursor compared as one row (meInboxDismissedCut), and an
	// owner's test is written only into the shape that names that owner (meInboxPageRows), since a
	// test of an argument the request does not use took that plan to read a whole arm before it cut
	// it - every one of 960 dismissed Reply threads for a page of 201
	// (TestTheDismissedPageReadsOnePageUnderTheGenericPlan). So no request's own values are worth
	// planning it again for.
	meInboxGenericPlan = `set local plan_cache_mode = force_generic_plan`
)

// readMeInbox runs the read's statement and reads what it answered (readMeInboxRows), in a
// read-only transaction of its own that sets meInboxSettings first: begin, the settings, the
// statement and commit, sent as one batch. A batch is one round trip, so the transaction costs the
// read no round trip it did not make already, and the statement stays the read's one statement and
// the only one that reads anything (TestOneReadAnswersEveryRelationToThePersonsNode). The settings
// come before the statement rather than inside it because Postgres decides which plan to run a
// statement by, and whether to compile it, when it plans it, which the server does at that
// statement's Bind, after it has run the messages before it; and `set local` ends with the
// transaction, so no setting reaches a statement this connection runs for another request. A
// statement that fails leaves the transaction aborted, and the pool closes a connection handed
// back to it in one rather than reuse it.
func (s *server) readMeInbox(ctx context.Context, statement string, args []any) (*meInboxRead, error) {
	batch := &pgx.Batch{}
	batch.Queue(`begin transaction read only`)
	for _, setting := range meInboxSettings {
		batch.Queue(setting)
	}
	batch.Queue(statement, args...)
	batch.Queue(`commit`)
	results := s.deps.Store.Pool.SendBatch(ctx, batch)
	defer results.Close()
	for range 1 + len(meInboxSettings) {
		if _, err := results.Exec(); err != nil {
			return nil, fmt.Errorf("begin the inbox's read: %w", err)
		}
	}
	rows, err := results.Query()
	if err != nil {
		return nil, fmt.Errorf("read the inbox: %w", err)
	}
	read, err := readMeInboxRows(rows, s.deps.ServerURL)
	if err != nil {
		return nil, err
	}
	if _, err := results.Exec(); err != nil {
		return nil, fmt.Errorf("end the inbox's read: %w", err)
	}
	if err := results.Close(); err != nil {
		return nil, fmt.Errorf("end the inbox's read: %w", err)
	}
	return read, nil
}

// writeMeInboxAnswer writes response with the bytes WriteJSON writes for it (writeMeInboxAnswerBy),
// its entries encoded by as many goroutines as the process runs at once, up to
// meInboxEncodeWorkers.
func writeMeInboxAnswer(w http.ResponseWriter, response meInboxResponse) {
	writeMeInboxAnswerBy(w, response, min(runtime.GOMAXPROCS(0), meInboxEncodeWorkers))
}

// writeMeInboxAnswerBy writes response with the bytes WriteJSON writes for it, its entries a few at
// a time by up to workers goroutines: the envelope is encoded with no entry and the entries into
// its array in turn (writeMeInboxEntries), so no buffer holds the whole of a page at once - several
// megabytes for the person whose history is a hundred times the median, which the encoder
// otherwise grows by doubling and the collector then scans.
func writeMeInboxAnswerBy(w http.ResponseWriter, response meInboxResponse, workers int) {
	entries := response.Entries
	response.Entries = []meInboxEntry{}
	envelope, err := json.Marshal(response)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err != nil {
		// What WriteJSON answers for a body it cannot encode: the status, no body, and the failure
		// logged. The whole response holds the envelope that failed, so it would fail as well.
		slog.Warn("dispatch: write json failed", "error", err)
		return
	}
	out := bufio.NewWriterSize(w, 64<<10)
	// The envelope ends with its empty entries, `[]}`, so the entries go between its last two bytes.
	_, err = out.Write(envelope[:len(envelope)-2])
	if err == nil {
		err = writeMeInboxEntries(out, entries, workers)
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

const (
	// meInboxEncodeChunk is how many entries one worker encodes at a time, about a hundred
	// kilobytes of the heavy history's answer.
	meInboxEncodeChunk = 64
	// meInboxEncodeWorkers is the most goroutines that encode entries at once.
	meInboxEncodeWorkers = 4
)

// meInboxChunks holds the buffers a chunk of entries is encoded into.
var meInboxChunks = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// meInboxEncoded is one chunk of entries as it encodes: the entries' JSON, comma-separated, or the
// error the first one that failed gave.
type meInboxEncoded struct {
	buffer *bytes.Buffer
	err    error
}

// encodeMeInboxChunk is entries encoded one after another, comma-separated, into a buffer of the
// pool; the writer returns it once it has written it. Each entry is encoded by one encoder of the
// chunk, which writes json.Marshal's bytes and a newline, and the newline becomes the comma before
// the next entry.
func encodeMeInboxChunk(entries []meInboxEntry) meInboxEncoded {
	buffer := meInboxChunks.Get().(*bytes.Buffer)
	buffer.Reset()
	encoder := json.NewEncoder(buffer)
	for index := range entries {
		if err := encoder.Encode(entries[index]); err != nil {
			meInboxChunks.Put(buffer)
			return meInboxEncoded{err: err}
		}
		buffer.Bytes()[buffer.Len()-1] = ','
	}
	if buffer.Len() > 0 {
		buffer.Truncate(buffer.Len() - 1)
	}
	return meInboxEncoded{buffer: buffer}
}

// writeMeInboxEntries writes entries to out, each as json.Marshal encodes it and a comma between
// each two. An answer of several chunks has them encoded by up to workers goroutines, no more than
// twice that many ahead of the one that writes them in their order, so the bytes are the ones a
// single pass writes and no more of the page is held at once than those chunks.
func writeMeInboxEntries(out *bufio.Writer, entries []meInboxEntry, workers int) error {
	chunks := (len(entries) + meInboxEncodeChunk - 1) / meInboxEncodeChunk
	chunkOf := func(index int) []meInboxEntry {
		return entries[index*meInboxEncodeChunk : min((index+1)*meInboxEncodeChunk, len(entries))]
	}
	// write writes chunk index once it is encoded, after a comma unless it is the first, and gives
	// its buffer back to the pool.
	write := func(index int, encoded meInboxEncoded) error {
		if encoded.err != nil {
			return encoded.err
		}
		err := error(nil)
		if index > 0 {
			err = out.WriteByte(',')
		}
		if err == nil {
			_, err = out.Write(encoded.buffer.Bytes())
		}
		meInboxChunks.Put(encoded.buffer)
		return err
	}
	if chunks < 2 || workers < 2 {
		for index := range chunks {
			if err := write(index, encodeMeInboxChunk(chunkOf(index))); err != nil {
				return err
			}
		}
		return nil
	}
	// A ring of result slots, a chunk's slot its index modulo their number. A chunk takes a window
	// token before it is handed out and the writer gives it back once it has received that chunk, so
	// no more chunks are out at once than there are slots: chunk index+len(results), which shares
	// chunk index's slot, is handed out only once chunk index has left it, and no worker waits to
	// deliver a chunk.
	results := make([]chan meInboxEncoded, min(2*workers, chunks))
	for index := range results {
		results[index] = make(chan meInboxEncoded, 1)
	}
	window := make(chan struct{}, len(results))
	next := make(chan int)
	stop := make(chan struct{})
	var group sync.WaitGroup
	defer group.Wait()
	defer close(stop)
	go func() {
		defer close(next)
		for index := range chunks {
			select {
			case window <- struct{}{}:
			case <-stop:
				return
			}
			select {
			case next <- index:
			case <-stop:
				return
			}
		}
	}()
	for range workers {
		group.Go(func() {
			for index := range next {
				results[index%len(results)] <- encodeMeInboxChunk(chunkOf(index))
			}
		})
	}
	for index := range chunks {
		encoded := <-results[index%len(results)]
		<-window
		if err := write(index, encoded); err != nil {
			return err
		}
	}
	return nil
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
// Reply row; each Agent row's root, the replies under it (each with its attempts and its block)
// and its session's name; and the approval gate of each issue a row belongs to, by issue key. The
// ask details every list of asks carries beside its rows are kept where meInboxAsks reads them
// (openedEvents, backlinkCounts and askParts).
type meInboxPage struct {
	asks      map[string]openAskRow
	reaches   map[string]askReach
	replies   map[string][]model.Comment
	edits     map[string][]model.AskEdit
	followers map[string][]model.AskFollower
	threads   map[string]model.MyReplyRow
	roots     map[string]model.Message
	chains    map[string][]model.Message
	titles    map[string]string
	gates     map[string][]approvalGateLine
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
// plans is the field plans the records of its column share (meInboxPlans), nil for a record read
// alone.
type meInboxRecord struct {
	types *pgtype.Map
	raw   []byte
	plans *meInboxPlans
}

func (record meInboxRecord) Scan(dest ...any) error {
	scanner := pgtype.NewCompositeBinaryScanner(record.types, record.raw)
	if fields := scanner.FieldCount(); fields != len(dest) {
		return fmt.Errorf("read a record of %d fields into %d", fields, len(dest))
	}
	if record.raw == nil || record.plans == nil {
		return record.types.Scan(pgtype.RecordOID, pgtype.BinaryFormatCode, record.raw, pgtype.CompositeFields(dest))
	}
	// What pgx's record plan does for each field (pgtype's
	// scanPlanBinaryRecordToCompositeIndexScanner), with the field's plan taken from plans.
	for index := 0; scanner.Next(); index++ {
		if index >= len(dest) {
			return fmt.Errorf("cannot scan composite field %d into CompositeFields of length %d", index, len(dest))
		}
		target := dest[index]
		if target == nil {
			continue
		}
		if err := record.plans.plan(record.types, index, scanner.OID(), target).Scan(scanner.Bytes(), target); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// meInboxPlans is the scan plan of each field of the records one column holds, made for its first
// record and kept for every later one. pgx plans each field of a record afresh on every record
// (pgtype.Map.PlanScan memoizes nothing), which for the person whose history is a hundred times
// the median was every field of some 10,000 records a read. The records of one column are of one
// row type and their scanner reads each into targets of one type, so the plan of a field reads that
// field of each of them, as pgx's own rows reuse a column's plan from one row to the next; a field
// whose type or target type differs from the one its plan was made for is planned again, as those
// rows replan a column whose target changed.
type meInboxPlans struct {
	fields []meInboxFieldPlan
}

type meInboxFieldPlan struct {
	oid    uint32
	target reflect.Type
	plan   pgtype.ScanPlan
}

// plan is the plan of field index, of type oid, scanned into target.
func (plans *meInboxPlans) plan(types *pgtype.Map, index int, oid uint32, target any) pgtype.ScanPlan {
	for len(plans.fields) <= index {
		plans.fields = append(plans.fields, meInboxFieldPlan{})
	}
	field := &plans.fields[index]
	if targetType := reflect.TypeOf(target); field.plan == nil || field.oid != oid || field.target != targetType {
		*field = meInboxFieldPlan{oid: oid, target: targetType, plan: types.PlanScan(oid, pgtype.BinaryFormatCode, target)}
	}
	return field.plan
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
	asks, opened, backlinks, parts, reach                []pgtype.UndecodedBytes
	replies, replyNamed, replyAttempts                   []pgtype.UndecodedBytes
	askBlocks, commentBlocks, edits, followers           []pgtype.UndecodedBytes
	threads, threadAnswers                               []pgtype.UndecodedBytes
	roots, messages, attempts, messageBlocks, titles     []pgtype.UndecodedBytes
	gateIssues, gateDocuments, gateReviews, gateRequests []pgtype.UndecodedBytes
	trees                                                []pgtype.UndecodedBytes
}

func (once *meInboxOnceColumns) targets() []any {
	return []any{
		&once.asks, &once.opened, &once.backlinks, &once.parts, &once.reach,
		&once.replies, &once.replyNamed, &once.replyAttempts, &once.askBlocks, &once.commentBlocks, &once.edits, &once.followers,
		&once.threads, &once.threadAnswers,
		&once.roots, &once.messages, &once.attempts, &once.messageBlocks, &once.titles,
		&once.gateIssues, &once.gateDocuments, &once.gateReviews, &once.gateRequests,
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
	var columns meInboxColumns
	targets := columns.targets()
	agentPlans := &meInboxPlans{}
	answered := false
	for rows.Next() {
		answered = true
		columns = meInboxColumns{}
		if err := rows.Scan(targets...); err != nil {
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
		row, err := read.entry(types, &columns, agentPlans)
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
	plans := &meInboxPlans{}
	read := make([]meInboxRecord, len(raw))
	for index := range raw {
		read[index] = meInboxRecord{types: types, raw: raw[index], plans: plans}
	}
	return read
}

// entry reads one row naming a node: its relations, its page column, and an Agent row's row itself
// (scanMyAgentRow), which the page rows read once for their order and the entry keeps.
func (read *meInboxRead) entry(types *pgtype.Map, columns *meInboxColumns, agentPlans *meInboxPlans) (meInboxRow, error) {
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
		listed, err := scanMyAgentRow(meInboxRecord{types: types, raw: columns.agent, plans: agentPlans}, read.serverURL)
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
// oldest first. An Ask or a Reply row of an issue carries that issue's approval gate.
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
		if ask.Issue != nil {
			row.ApprovalGate = page.gates[ask.Issue.Key]
		}
	case meInboxPageThread:
		thread, held := page.threads[row.Node.ID]
		if !held {
			return missing("thread")
		}
		row.Thread = &thread
		if thread.Issue != nil {
			row.ApprovalGate = page.gates[thread.Issue.Key]
		}
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
// rows as listInbox reads them (scanOpenAskRow) with the details loadAskDetails reads, each thread's
// replies with the targets each names, its attempts and its named block (hydrateCommentSideTables),
// the rewordings and followers; the Reply rows (scanMyReplyRow), each whose newest write by
// somebody else is its ask's answer with that answer as the ask's node holds it (answerReply); each
// Agent row's root and the replies under it, each message's attempts and the block it names, and
// the sessions' names; the approval gate of each issue a row belongs to (readApprovalGates); and
// every stored tree their texts read.
func (read *meInboxRead) readPage(types *pgtype.Map, columns *meInboxOnceColumns) error {
	page := meInboxPage{
		asks: make(map[string]openAskRow, len(columns.asks)), reaches: make(map[string]askReach, len(columns.reach)),
		replies: map[string][]model.Comment{}, edits: map[string][]model.AskEdit{}, followers: map[string][]model.AskFollower{},
		threads: make(map[string]model.MyReplyRow, len(columns.threads)),
		roots:   make(map[string]model.Message, len(columns.roots)),
		chains:  make(map[string][]model.Message, len(columns.roots)),
		titles:  make(map[string]string, len(columns.titles)),
		gates:   map[string][]approvalGateLine{},
	}
	if err := read.readAsks(types, columns, &page); err != nil {
		return err
	}
	answers := map[string]model.Ask{}
	for _, record := range records(types, columns.threadAnswers) {
		ask, err := scanMyReplyAnswer(record)
		if err != nil {
			return fmt.Errorf("read the inbox's Reply rows: %w", err)
		}
		answers[ask.ID] = ask
	}
	for _, record := range records(types, columns.threads) {
		thread, answerWrite, err := scanMyReplyRow(record, read.serverURL)
		if err != nil {
			return fmt.Errorf("read the inbox's Reply rows: %w", err)
		}
		if answerWrite {
			answerReply(&thread.Reply, answers[thread.ThreadID], read.serverURL)
		}
		page.threads[thread.ThreadID] = thread
	}
	if err := readAgentConversations(types, columns, &page); err != nil {
		return err
	}
	if err := readApprovalGates(types, columns, &page); err != nil {
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
		ask, err := scanOpenAskRow(record)
		if err != nil {
			return err
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

// readApprovalGates reads the approval gate of each issue a row of the page belongs to into page
// (meInboxGateReads): each gate document's approval from its latest version, its latest review
// and its open approval request (approvalOf), then each issue's lines (approvalGate).
func readApprovalGates(types *pgtype.Map, columns *meInboxOnceColumns, page *meInboxPage) error {
	reviews := map[string]model.ArtifactReview{}
	for _, record := range records(types, columns.gateReviews) {
		review, err := docs.ScanReview(record)
		if err != nil {
			return fmt.Errorf("read a gate document's latest review: %w", err)
		}
		reviews[review.ArtifactID] = review
	}
	requests := map[string]openApprovalRequest{}
	for _, record := range records(types, columns.gateRequests) {
		artifactID, request, err := scanOpenApprovalRequest(record)
		if err != nil {
			return fmt.Errorf("read a gate document's open approval request: %w", err)
		}
		requests[artifactID] = request
	}
	documents := map[string]approvalGateDocument{}
	for _, record := range records(types, columns.gateDocuments) {
		var id string
		var latest int
		if err := record.Scan(&id, &latest); err != nil {
			return fmt.Errorf("read a gate document: %w", err)
		}
		var review *model.ArtifactReview
		if found, held := reviews[id]; held {
			review = &found
		}
		var request *openApprovalRequest
		if found, held := requests[id]; held {
			request = &found
		}
		// A gate document is one the issue's list holds, which a retired document is not
		// (meInboxGateReads), so none of them is retired.
		approval := approvalOf(latest, false, review, request)
		documents[id] = approvalGateDocument{id: id, approval: &approval}
	}
	for _, record := range records(types, columns.gateIssues) {
		var key string
		var root bool
		var steps []string
		var primary, verification *string
		if err := record.Scan(&key, &root, &steps, &primary, &verification); err != nil {
			return fmt.Errorf("read a gate's issue: %w", err)
		}
		page.gates[key] = approvalGate(root, steps, documents[deref(primary)], documents[deref(verification)])
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
	shape := meInboxShape{
		state: request.state, issue: request.issue != "", artifact: request.artifact != nil,
		trees: trees, readsRoster: readsRoster,
	}
	text := meInboxTextOf(shape)
	if text.err != nil {
		return "", nil, text.err
	}
	return text.statement, append(meInboxRequestArgs(caller, horizon, request, shape, known), text.constants...), nil
}

// meInboxShape is what the text of the read's statement depends on, and nothing a request carries
// but this: its state, which owner narrows it - an issue, a document or neither - whether the page
// asks for display trees and whether the listener's stored roster is read. Every other value of a
// request is an argument, so a shape is one text, built once a process (meInboxTextOf) and prepared
// once on each connection, whose plan Postgres plans once there, on its first execution, and runs
// from then on (meInboxGenericPlan): the text is tens of kilobytes and takes tens of milliseconds to
// plan and to build, which a request pays only where it is new.
type meInboxShape struct {
	state              string
	issue, artifact    bool
	trees, readsRoster bool
}

// owned is whether an owner narrows the shape (meInboxRequest.owned).
func (shape meInboxShape) owned() bool {
	return shape.issue || shape.artifact
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
// The relations of the read with no state are read by the walk: the distinct relation kinds that
// touch each of the person's nodes, read by a skip over an index led by the far end and the kind
// (graph_tuples_as_of_in) and one over graph_tuples_current (one index probe a kind, kinds_in and
// kinds_out), then each kind's current tuples through graphstore.CurrentTupleRead, one index range
// a kind, each relation kind and end once a node (tied), and the entries are those nodes. That walk
// reads every current tuple of the person's, so its cost is their whole history, which is what that
// read answers.
//
// Under a state the entries are the page's own rows in that state, read from the caches the page's
// lists read (meInboxPageRows), and the walk reads only the tuples between the person's nodes and
// the nodes those rows are (page_nodes): for each relation kind that touches the person, one keyed
// probe from each of those nodes, whose own tuples of a kind are few. A relation into the person is
// read by graph_tuples_as_of_out from the node: the read leaves out the `to_kind <> 'value'` test,
// which every person node passes, so graph_tuples_as_of_in - keyed by the person, which read all
// the person's tuples of a kind for each node it was asked about (the scale check, c-p99: 8,002
// probes, 71 s of 74, for the person whose history is a hundred times the median) - and
// graph_tuples_current, whose predicates need that test, are no index its plan can take. Under
// state=inbox and state=unread a relation out of the person is read by graph_tuples_current on its
// whole key, one index descent. Under state=dismissed it is read by graph_tuples_as_of_in into the
// node: the read tests the person's end as an expression, each column concatenated with the empty
// string, so graph_tuples_current and graph_tuples_as_of_out, the two indexes led by that end, are
// no index its plan can take either (the Dismissed page below says what a plan led by the person
// read). So the walk costs the state's rows and not the person's history:
// every dismissal appends a dismissed tuple and every Mark all read a read_through tuple for each
// thread it marks (api/read_marks.go), and a walk of every tuple read all of them for every state.
// At 5,000 dismissed threads it read state=inbox in 71 ms and state=unread in 93 ms, against 12 ms
// and 11 ms at 250, over the same rows in either state; reading that walk and keeping the state's
// nodes left state=inbox at 89 ms, since the plan merged the two by sorting the whole walk
// (TestTheOneReadsInboxStatesDoNotGrowWithTheDismissedHistory holds the keyed walk to the 250's
// figures at 5,000). The counts are the entries', each a row of the state, so they too read the
// state's rows and no tuple past their relations.
//
// Under state=dismissed the page's rows are read from their keys (page_keys,
// meInboxDismissedKeys): the first offset + limit + 1 of the state's rows, in the order the page
// answers in, which are the rows the page and its next_cursor are taken from, or every row where
// ?kind= narrows the entries by their relations, and the page's nodes are those rows. That state is
// the person's whole pile, so its page is cut before the walk: in the dismissed scale test the
// dismissals and Mark all read runs it times append some 35,000 dismissed and read_through tuples to
// the person, and a walk of every tuple then read 11,830 tuples for each of three kinds for a page
// of 200. The person's end of the read out of the person is tested off every index for what this
// state's page met: tested on the columns, that end let the plan probe graph_tuples_as_of_out, which
// no predicate keeps out: led by the person and the kind, it holds every dismissed and every
// read_through tuple the person ever wrote, and the generic plan estimates its probe at one row, as
// it does the probe of graph_tuples_current's whole key, so which of the two a connection took
// turned on the statistics it planned under. With 5,000 threads dismissed in one Dismiss, a plan
// that took graph_tuples_as_of_out read the person's 5,000 tuples of a kind in each of 402 probes,
// two kinds for each of the page's 201 nodes: 1,244 ms for a page the other plan read in 50 ms, in a
// pod. TestTheDismissedPageWalksFromThePagesOwnNodes holds the walk to the page's nodes.
// state=inbox and state=unread answer every row of their state, whose count is the live rows' and
// not the history's, so their page's nodes are every row of the state, walked by the keyed reads
// above.
//
// Either way the page's rows and the walk's relations are put together by grouping the two by node
// (sides) rather than by joining one to the other, so the read costs one pass over the walk and one
// over the page whatever the planner takes either to hold, and never the product of the two, which
// a join made it under the plan Postgres keeps from the sixth execution, a plan for no value of the
// arguments that takes the page to hold one row and loops over the whole walk for each (12.2 s for
// the person whose history is a hundred times the median, against 1.6 s for the plan of its own
// arguments). The visibility predicate is over the node's own row of graph_nodes, read by its key
// for each node, in this statement and bound once, and the counts are taken after it. What the
// page's rows draw is read once for the page (meInboxPageReads).
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
	// for cannot be judged, so it is left out under the narrowing (meInboxVisible). An owner keeps
	// the page's rows of that owner alone (meInboxRequest.owned); under a state the entries are those
	// rows already.
	visible := meInboxVisible("g.node_kind", "g.node_id")
	order := meInboxOrderOf(shape)
	// The walk's reach: the relations of every node tied to the person, one probe a kind, or under a
	// state of the page's own nodes alone (above), each probe with all of a node's kinds
	// (meInboxWalked).
	pageKeys, pageNodes, walkFrom := ``, ``, ``
	inbound := `t.to_kind = k.node_kind and t.to_id = k.node_id and t.kind = k.kind
				and t.to_kind <> '` + graphstore.KindValue + `' and ` + atHorizon
	outbound := `t.from_kind = k.node_kind and t.from_id = k.node_id and t.kind = k.kind
				and t.to_kind <> '` + graphstore.KindValue + `' and ` + meInboxOutOfPerson + ` and ` + atHorizon
	if shape.state != "" {
		cut := ``
		if shape.state == myRepliesStateDismissed {
			pageKeys = `
	page_keys(node_kind, node_id, page, moment) as materialized (` + meInboxDismissedKeys(shape) + `
	),`
			cut = `
		limit ` + meInboxDismissedLimit
		}
		pageNodes = `
	page_nodes(node_kind, node_id) as materialized (
		select pr.node_kind, pr.node_id from page_rows pr` + cut + `
	),`
		walkFrom = `
		cross join page_nodes p`
		inbound = `t.from_kind = p.node_kind and t.from_id = p.node_id and t.kind = any(k.kinds)
				and t.to_kind = k.node_kind and t.to_id = k.node_id and ` + atHorizon
		if shape.state == myRepliesStateDismissed {
			// The person's end is tested off every index (meInboxBuild's comment), so this read
			// takes graph_tuples_as_of_in from the page's node and never an index led by the person.
			outbound = `t.to_kind = p.node_kind and t.to_id = p.node_id and t.kind = any(k.kinds)
				and t.from_kind || '' = k.node_kind and t.from_id || '' = k.node_id
				and t.to_kind <> '` + graphstore.KindValue + `' and ` + atHorizon
		} else {
			// The person's end and then the page's node bind graph_tuples_current's whole key, and
			// the value test and meInboxOutOfPerson state that index's predicate, so this read is one
			// index descent.
			outbound = `t.from_kind = k.node_kind and t.from_id = k.node_id and t.kind = any(k.kinds)
				and t.to_kind = p.node_kind and t.to_id = p.node_id
				and t.to_kind <> '` + graphstore.KindValue + `' and ` + meInboxOutOfPerson + ` and ` + atHorizon
		}
	}

	var statement strings.Builder
	statement.WriteString(`
	with recursive
	-- The three parameters of unreadDirectRepliesArgs, typed once: a request narrowed to an
	-- owner splices in no Agent fragment, which is what reads the last two.
	direct_args(login, actor_id, session_id) as (select $1::text, $2::text, $3::text),
	-- The cursor, typed once: the Dismissed view's key takes no kind (meInboxOrder).
	cursor_args(moment, kind, id) as (select ` + meInboxCursorMoment + `::timestamptz, ` + meInboxCursorKind + `::text, ` + meInboxCursorID + `::text),
	-- The owner, typed once: a shape's text names only the owner it narrows by (meInboxPageRows).
	owner_args(issue, artifact) as (select ` + meInboxIssue + `::text, ` + meInboxArtifact + `::uuid),
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
	` + meInboxKindSkip("kinds_out", "from") + `,` + pageKeys + `
	page_rows(node_kind, node_id, page, moment, agent_session, agent) as (` +
		meInboxPageRows(shape) + `
	),` + pageNodes + meInboxWalked(shape, walkFrom, inbound, outbound, readKind) + `
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
		if shape.owned() {
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
	// Under state=dismissed the total is the page the read answers, and not the whole pile, which
	// nothing counts: the page's keys are cut to the page before the walk (meInboxDismissedKeys), so
	// the entries hold the page and no row past it but the one the cut reads to learn whether
	// another page follows (remaining).
	total := `(select c.total from counted c limit 1)`
	if shape.state == myRepliesStateDismissed {
		total = `(select count(*) from paged)`
	}
	statement.WriteString(`
	counted as (
		select e.*, count(*) over () as total from entries e
	),
	remaining as (
		select c.*, count(*) over () as remaining from counted c
		where ` + meInboxCursorMoment + `::timestamptz is null
		   or ` + order.after("c") + `
	),
	-- The page, each entry numbered in the order it is answered in.
	paged as (
		select cut.*, row_number() over () as ord
		from (
			select * from remaining r
			order by ` + order.by("r") + `
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
	// no entry its one row; every other row carries none of it. The entries are sorted before that
	// record and the columns every row repeats - the read's moment, the roster and the two counts,
	// each read once - are put beside them, so the sort holds the entries alone: sorted with the
	// record, which holds every row the page draws, c-p99's heavy history (4,003 entries) spilled
	// to disk (external merge, 3,984 kB; INFRA-4948). The outer `order by` names exactly the inner
	// one's keys, each by the column of the sorted rows that holds it - the entry's own, or a column
	// selected beside them for a key that is an expression, the Dismissed view's id as a uuid
	// (meInboxOrder.sorted) - so the planner takes the inner sort's order for its own and sorts
	// nothing again.
	const carries = `(entry.ord is null or entry.ord = 1)`
	keyColumns, sortedBy := order.sorted("entry", "sorted")
	statement.WriteString(`
	select now(), case when sorted.carries then ` + roster + ` end, coalesce(` + total + `, 0),
	       coalesce((select r.remaining from remaining r limit 1), 0), sorted.carries,
	       case when sorted.carries then row(` + strings.Join(reads.columns, `,
	         `) + `) end,
	       sorted.node_kind, sorted.node_id, sorted.moment, sorted.kinds, sorted.directions, sorted.ats,
	       sorted.page, sorted.agent
	from (
		select ` + carries + ` as carries,
		       entry.node_kind, entry.node_id, entry.moment, entry.kinds, entry.directions, entry.ats,
		       entry.page, entry.agent` + keyColumns + `
		from (values (true)) as one (present)
		left join paged entry on true
		order by ` + order.by("entry") + `
	) sorted
	order by ` + sortedBy)
	return statement.String(), args.values[slots.first:], nil
}

// meInboxWalked is the walked CTE: the newest asserted tuple of each relation kind that ties a
// node to the person's nodes, at each end. The walk with no state reads one probe a kind
// (kinds_in, kinds_out). Under a state the kinds that touch a person's node at each end are
// listed once a node (kind_lists_in, kind_lists_out) and each page node is probed once a node and
// end with all of them, `kind = any(...)`: one index scan that finds its kinds side by side where
// a probe a kind descended the index once for each, which at c-p99's heavy history was 8,002
// probes an end, 3 to 4 µs each, and is the largest part of the statement (INFRA-4948).
func meInboxWalked(shape meInboxShape, walkFrom, inbound, outbound string, readKind func(string) string) string {
	if shape.state == "" {
		return `
	walked(near_kind, near_id, kind, direction, at) as (
		select stated.from_kind, stated.from_id, k.kind, '` + meInboxInbound + `', stated.at
		from kinds_in k` + walkFrom + `
		cross join lateral (` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.at, t.asserted`, inbound) + `) stated
		where ` + readKind("k.kind") + ` and stated.asserted
		union all
		select stated.to_kind, stated.to_id, k.kind, '` + meInboxOutbound + `', stated.at
		from kinds_out k` + walkFrom + `
		cross join lateral (` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.at, t.asserted`, outbound) + `) stated
		where ` + readKind("k.kind") + ` and stated.asserted
	),`
	}
	kinds := func(name, from string) string {
		return name + `(node_kind, node_id, kinds) as (
		select k.node_kind, k.node_id, array_agg(k.kind) from ` + from + ` k where ` + readKind("k.kind") + `
		group by k.node_kind, k.node_id
	),`
	}
	return `
	` + kinds("kind_lists_in", "kinds_in") + `
	` + kinds("kind_lists_out", "kinds_out") + `
	walked(near_kind, near_id, kind, direction, at) as (
		select stated.from_kind, stated.from_id, stated.kind, '` + meInboxInbound + `', stated.at
		from kind_lists_in k` + walkFrom + `
		cross join lateral (` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.kind`, `t.from_kind, t.from_id, t.kind, t.at, t.asserted`, inbound) + `) stated
		where stated.asserted
		union all
		select stated.to_kind, stated.to_id, stated.kind, '` + meInboxOutbound + `', stated.at
		from kind_lists_out k` + walkFrom + `
		cross join lateral (` + graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.at, t.asserted`, outbound) + `) stated
		where stated.asserted
	),`
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
// A walk with no state draws the rows of the Inbox's own state. Under state=dismissed the rows are
// read from the page's keys (page_keys, meInboxDismissedKeys), the first offset + limit + 1 of the
// state's rows: a Reply row is its key, and an Agent row is read over the Agent keys alone, so a
// page of that state reads the rows it answers however many threads the person has dismissed, and
// walks the threads of the Agent rows among them and of no other root.
//
// The owner's tests are written only into a shape that names that owner. The plan Postgres keeps
// from the sixth execution, and the one this read forces (meInboxGenericPlan), knows nothing of
// the arguments: it takes a test of an argument against a column to keep almost no row. The Reply
// rows read myRepliesOwner's two tests, one of the issue and one of the document, each written as
// an `or` with the argument left out, and a shape binds the owner it does not name to the value
// that names none - the empty text, or null - so that the test is a constant the planner drops;
// the owner it names keeps its own test. Bound to arguments on every shape, the two tests took the
// Dismissed view's Reply arm to keep one row of the 320 it estimated off user_threads_dismissed,
// at a seed of 960 dismissed Reply threads on one issue and no document, and a plan that takes an
// arm to hold one row reads the whole of it and sorts it: all 960 rows read for the 201 the cut
// kept (TestTheDismissedPageReadsOnePageUnderTheGenericPlan). A request narrowed to an issue still
// reads the arm in its index's order, and stops once the page is full, so it reads the page where
// the issue holds the rows the page is made of.
//
// The Ask rows are the open asks first, read by their row's state alone, then the ones of those
// that wait on the person, and last the ones of those that are open as every read of open asks
// across owners picks them, by their row's state and their node's (askOpenOf, R3), each with its
// newest reply. With the owner's tests on every shape that plan took the open asks to be one and
// read each one's issue, document and project by a scan of that whole table, and its newest reply,
// before it knew whether the ask waits on the person at all (13.6 ms of that plan's 21.6 for the
// person whose history is empty, against 0.4 ms in the plan of their own arguments). The node's
// test probes the ask's node, so it reads only the asks that wait on the person, which are
// materialized before it as the open asks are before them.
func meInboxPageRows(shape meInboxShape) string {
	issue, artifact := meInboxOwnerArgs(shape)
	state := shape.state
	if state == "" {
		state = myRepliesStateInbox
	}
	threads, threadMoment := bindFragment(myRepliesInbox, "$1", issue, artifact), `u.replied_at`
	agents, agentMoment := myAgentInboxRoots, `listed.replied_at`
	switch state {
	case myRepliesStateUnread:
		threads, agents = bindFragment(myRepliesUnread, "$1", issue, artifact), myAgentUnreadRoots
	case myRepliesStateDismissed:
		// A Reply row is its key, and the Agent keys are the roots the Agent rows are read over,
		// after the marks their facts read (agentMarkOf).
		threads, threadMoment = `
		from page_keys u(kind, thread_id, page, dismissed_at)
		where u.page = '`+meInboxPageThread+`'`, `u.dismissed_at`
		agents, agentMoment = myAgentMarks+`
	roots as (
		select k.node_id as root_id, k.moment as dismissed_at
		from page_keys k
		where k.page = '`+meInboxPageAgent+`'
	)`, `listed.dismissed_at`
	}
	arms := []string{`
		select u.kind, u.thread_id::text, '` + meInboxPageThread + `', ` + threadMoment + `, null::text, null::record
		` + threads}
	if state == myRepliesStateInbox {
		owner := ``
		if shape.issue {
			owner = `
			  and a.issue_key = ` + meInboxIssue + `::text`
		}
		if shape.artifact {
			owner = `
			  and a.artifact_id = ` + meInboxArtifact + `::uuid`
		}
		arms = append(arms, `
		select '`+graphstore.KindAsk+`', a.id::text, '`+meInboxPageAsk+`', coalesce(lr.created_at, a.created_at), null::text, null::record
		from (
			with open_asks as materialized (
				select id, created_at, issue_key, artifact_id, asked_of, state, node_kind from asks where state = '`+asks.StateOpen+`'
			), waiting as materialized (
				select a.id, a.created_at, a.state, a.node_kind
				from open_asks a`+askedOfPersonJoins+`
				where (i.key is null or i.closed_at is null)
				  and `+askedOfPersonExpression+` = $1`+owner+`
			)
			select a.id, a.created_at
			from waiting a
			where `+askOpenOf("a")+`
		) a`+lastReplyJoin)
	}
	if !shape.owned() {
		arms = append(arms, `
		select '`+graphstore.KindMessage+`', listed.root_id, '`+meInboxPageAgent+`', `+agentMoment+`, listed.session_id, row(listed.*)
		from (`+agents+myAgentRowsSelect+`
		) listed`)
	}
	return strings.Join(arms, `
		union all`)
}

// meInboxOwnerArgs is what a shape binds myRepliesOwner's issue and document to: the argument of
// the owner the shape names, and for an owner it does not name the value that names none, the
// empty text or null, so that its test is a constant the planner drops (meInboxPageRows).
func meInboxOwnerArgs(shape meInboxShape) (issue, artifact string) {
	issue, artifact = `''`, `null`
	if shape.issue {
		issue = meInboxIssue
	}
	if shape.artifact {
		artifact = meInboxArtifact
	}
	return issue, artifact
}

// meInboxDismissedKeys selects the keys of the rows the Dismissed view's page is taken from
// (page_keys): each of the two arms cut to the page (meInboxDismissedCut) - the Reply rows off
// user_threads_dismissed_replied, and the Agent roots off their dismissal marks
// (myAgentDismissedRootsSelect) - then the two merged and cut again to the same offset + limit + 1,
// or not at all where ?kind= narrows the entries (meInboxDismissedLimit), in the order the page
// answers in (meInboxOrder). A key is the row's node, its id as the uuid both indexes hold, the kind
// of row, and its dismissal.
//
// Every test a row of the page passes is passed above its own arm's cut, so the keys the merged cut
// keeps are the rows the page answers, in its order and with its tie-break, and the one past it.
// One test runs after the cut: an Agent root's facts are an inner lateral (myAgentRowFacts), which
// drops a root holding no session reply. No dismissal mark names such a root: a dismissal is
// refused for a root whose newest reply the page did not draw (inboxDismissCheckAgentRoots), no own
// reply writes an Agent dismissal mark for a root no session answered, which its writer reads
// under the root's lock (createStoredMessage, inboxwrite.Recording.Unanswered), and no statement
// deletes a message.
//
// page_rows reads each row from its key (meInboxPageRows), so every step after it meets those rows
// alone, and an Agent root's thread (myAgentRowFacts) is walked for the Agent rows among them and
// for no other: with each arm cut on its own, every step met both arms' offset + limit + 1 rows,
// and the Agent arm walked the thread of every root it kept, however many of the Reply rows newer
// than those roots filled the page. A shape narrowed to an owner has no Agent arm
// (meInboxRequest.owned).
func meInboxDismissedKeys(shape meInboxShape) string {
	issue, artifact := meInboxOwnerArgs(shape)
	arms := []string{`
			select u.kind, u.thread_id, '` + meInboxPageThread + `', u.dismissed_at
			from (
				select u.kind, u.thread_id, u.dismissed_at
				` + bindFragment(myRepliesDismissed, "$1", issue, artifact) +
		meInboxDismissedCut("u.kind", "u.thread_id", "u.dismissed_at") + `
			) u`}
	if !shape.owned() {
		arms = append(arms, `
			select '`+graphstore.KindMessage+`', a.root_id, '`+meInboxPageAgent+`', a.dismissed_at
			from (`+myAgentDismissedRootsSelect+
			meInboxDismissedCut(`'`+graphstore.KindMessage+`'`, "d.message_id", "d.dismissed_at")+`
			) a`)
	}
	return `
		select keyed.node_kind, keyed.node_id, keyed.page, keyed.moment
		from (` + strings.Join(arms, `
			union all`) + `
		) keyed(node_kind, node_id, page, moment)
		order by ` + meInboxOrderOf(shape).by("keyed") + `
		limit ` + meInboxDismissedLimit
}

// meInboxDismissedLimit is how many of the Dismissed view's rows a page is taken from: the first
// offset + limit + 1, the one past the page saying whether another page follows, or every row
// where ?kind= narrows the entries by their relations (meInboxDismissedCut).
const meInboxDismissedLimit = `case when ` + meInboxKindsParam + `::text[] is null
		  then ` + meInboxOffset + `::bigint + ` + meInboxLimit + `::bigint + 1 end`

// meInboxDismissedCut cuts one arm of the Dismissed view to the page before the walk meets it, as
// GET /api/v1/me/replies cuts its own (myRepliesDismissedCut, myAgentDismissedPageCut): the arm's
// rows past the cursor, in the order of the state's own index - the dismissal's moment, then the
// root's id (user_threads_dismissed_replied, user_agent_thread_dismissed_login_dismissed_at;
// meInboxOrder) - and of those the first offset + limit + 1, the one past the page saying whether
// another page follows (remaining, next_cursor). kind, id and moment name the arm's node kind, its
// root's uuid and its dismissal. The two arms' cuts are merged and cut to that many again
// (meInboxDismissedKeys), so the walk, the grouping, the count and the sort meet at most that many
// rows, where they met the whole pile: at the dismissed scale test's seed a page of 200 read a p50
// of 3,610 ms at 110,000 dismissed threads against 46 ms at 200, and with each arm cut, 86 ms at
// either.
//
// Three things make the cut the page the read answers. The cursor is one row comparison over
// coalesced arguments rather than `(cursor is null or ...)`, so it is the index's own condition in
// the generic plan too, the plan every execution runs (meInboxGenericPlan), which knows nothing of
// whether a request carries a cursor: the first page compares against a key past every dismissal.
// Every test an entry of the page passes is passed above the cut - the visibility predicate and the
// project (meInboxVisible) here, the owner in the fragment the cut ends - so no row the page would
// answer lies past a row the cut took and the page dropped. And a request narrowed by relation
// kind keeps the whole arm, since whether a row holds one of those kinds is the walk's to say,
// which the cut comes before; no caller sends one to this state. The request always holds a limit
// under this state, since one it does not name is myRepliesDismissedPage (parseMeInboxRequest), so
// the cut always takes one page. That the generic plan reads the cut off the index in its order and
// stops at it, rather than reading the whole arm and sorting it, rests on the arm's other tests as
// well: an owner's test is in the text only where the request names that owner (meInboxPageRows).
func meInboxDismissedCut(kind, id, moment string) string {
	return `
		  and (` + moment + `, ` + id + `) < (coalesce(` + meInboxCursorMoment + `::timestamptz, 'infinity'),
		      coalesce(` + meInboxCursorID + `::text::uuid, 'ffffffff-ffff-ffff-ffff-ffffffffffff'))
		  and ` + meInboxVisible(kind, id+`::text`) + `
		order by ` + moment + ` desc, ` + id + ` desc
		limit ` + meInboxDismissedLimit
}

// meInboxVisible is whether the caller may see the node kind and id name, inside the request's
// project where it names one: the visibility predicate over the node's own row of graph_nodes, read
// by its key and bound once (refs.VisibleRow at meInboxVisibility), and for a node the store holds
// no row for, true unless a project narrows the read, since such a node cannot be judged.
func meInboxVisible(kind, id string) string {
	inProject := `(` + meInboxProject + `::text = '' or n.project_key = ` + meInboxProject + `::text)`
	return `coalesce((select ` + refs.VisibleRow("n", meInboxVisibility) + ` and ` + inProject + `
			from graph_nodes n where n.kind = ` + kind + ` and n.id = ` + id + `), ` + meInboxProject + `::text = '')`
}

// meInboxOrder is the order a shape's entries are answered in, newest first, and the key a cursor
// resumes after: the moment, then the node's kind and id, which together name one node. Under
// state=dismissed it is the moment and the node's id alone, as a uuid (byID), which is the order
// both of that state's indexes hold and so the order its arms are cut in (meInboxDismissedCut).
// Every row of that state is a thread root or an Agent root, each a uuid of its own, so the id alone
// names the node; and a key the cut did not take would let the page's last entry and the next
// page's cut disagree over a tie, which a dismissal of many threads at once makes, since it gives
// them all one moment.
type meInboxOrder struct {
	byID bool
}

func meInboxOrderOf(shape meInboxShape) meInboxOrder {
	return meInboxOrder{byID: shape.state == myRepliesStateDismissed}
}

// after is whether the entry row names lies past the cursor in this order: the row's keys (keys)
// against the cursor's, each argument typed as the key it is compared with.
func (order meInboxOrder) after(row string) string {
	cursor := meInboxCursorMoment + `::timestamptz, ` + meInboxCursorKind + `::text, ` + meInboxCursorID + `::text`
	if order.byID {
		cursor = meInboxCursorMoment + `::timestamptz, ` + meInboxCursorID + `::text::uuid`
	}
	return `(` + strings.Join(order.keys(row), `, `) + `) < (` + cursor + `)`
}

// keys are this order's keys over the entry rows row names, each an expression its `order by`
// sorts on, newest first (by).
func (order meInboxOrder) keys(row string) []string {
	if order.byID {
		return []string{row + `.moment`, row + `.node_id::uuid`}
	}
	return []string{row + `.moment`, row + `.node_kind`, row + `.node_id`}
}

// by is this order over the entry rows row names, for an `order by`.
func (order meInboxOrder) by(row string) string {
	return strings.Join(order.keys(row), ` desc, `) + ` desc`
}

// sorted is this order once a subquery named outer has selected the entry rows row names and
// sorted them by by(row): the columns the subquery selects beside the rows' own, each led by a
// comma, for a key that is an expression of a column rather than a column - under byID the node's
// id as a uuid - and the order over outer's columns, which names exactly the keys the subquery
// sorted on, so the planner takes the subquery's order for the outer one (meInboxBuild).
func (order meInboxOrder) sorted(row, outer string) (columns, by string) {
	if order.byID {
		return `, ` + row + `.node_id::uuid as node_uuid`, outer + `.moment desc, ` + outer + `.node_uuid desc`
	}
	return ``, order.by(outer)
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
//   - the Ask rows as listInbox selects them (openAskRowColumns over openAskRowFrom: the ask, its
//     issue, document and the caller's snooze), the details loadAskDetails reads
//     (events.OpenedEventIDsStatement, refs.BacklinkCountRead, refs.AskPartRowsStatement) and their
//     threads as loadAskThreads reads them (inboxAskReplyChainsQuery, then the targets each reply
//     names, its attempts and its block, the rewordings, the followers and askReachesQuery);
//   - the Reply rows as listMyReplies selects them over the rows its cut kept (myRepliesPageRest),
//     each read by its user_threads key, and the answers of their asks as answerReplyRows reads
//     them off each ask's node (myReplyAnswersStatement);
//   - the Agent rows' conversations as hydrateMessageThreads reads them - each root message, every
//     reply under it (messageReplyChainsQuery), each message's attempts (messageDeliveriesQuery) and
//     the block it names (refs.ReplyBlocksStatement) - and the names of their sessions
//     (delivery.ResolveSessionTitlesQuery); each row itself is page_rows', read for its order;
//   - the approval gate of each issue an Ask or a Reply row belongs to (meInboxGateReads);
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
			cross join lateral jsonb_array_elements(a.options) choice
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
	gateCTEs, gateColumns := meInboxGateReads(askIDs, threadIDs)
	return meInboxReads{ctes: ctes + gateCTEs, columns: append([]string{
		`array(select row(listed.*) from (
			select ` + openAskRowColumns + `
			` + openAskRowFrom("$1") + `
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
		`array(select row(answered.*) from (` + bindFragment(myReplyAnswersStatement, threadIDs) + `) answered)`,
		`array(select row(r.*) from agent_roots r)`,
		`array(select row(c.*) from agent_chain c)`,
		`array(select row(attempt.*) from (` + bindFragment(messageDeliveriesQuery, messageIDs) + `) attempt)`,
		`array(select row(b.*) from agent_named b)`,
		`array(select row(titled.*) from (` + bindFragment(delivery.ResolveSessionTitlesQuery, sessions) + `) titled)`,
	}, append(gateColumns, trees)...)}, nil
}

// meInboxGateReads reads the approval gate of each issue a row of the page belongs to, once for the
// page, over the ids askIDs and threadIDs name: the issues of the Ask rows (their asks' issue_key)
// and of the Reply rows (their user_threads rows' issue_key), each with whether it is a root of its
// project - no parent, or a parent whose key's project, the part before its first hyphen, is
// another project, gateRoot's rule read off the issue's own row - its project's stored gated
// steps, and its primary and its Verification document, each named as the issue read names it
// (issueRowStatement); each of those documents that the issue's list holds - one not retired, as
// issueArtifactsStatement lists them - with its latest version, one probe of artifact_versions'
// unique (artifact_id, number) rather than a read of its version rows; and their latest reviews
// and open approval requests, by the statements the document reads run (latestReviewsStatement,
// openApprovalAsksStatement). Its columns come in the order meInboxOnceColumns reads them, and
// readApprovalGates reads them.
func meInboxGateReads(askIDs, threadIDs string) (string, []string) {
	documentIDs := `(select ids from gate_documents)`
	ctes := `,
	gate_issues(key, root, steps, primary_id, verification_id) as materialized (
		select i.key, i.parent_key is null or split_part(i.parent_key, '-', 1) <> i.project_key, p.gated_steps,
		       (select a.id from artifacts a where a.issue_key = i.key and a.is_primary),
		       (select a.id from artifacts a where a.issue_key = i.key and ` + aliasedVerificationRole + `)
		from issues i
		join projects p on p.key = i.project_key
		where i.key = any(array(
			select a.issue_key from asks a where a.id = any(` + askIDs + `::uuid[])
			union
			select u.issue_key from user_threads u where u.login = $1 and u.thread_id = any(` + threadIDs + `::uuid[])
		))
	),
	gate_documents(ids) as materialized (
		select coalesce(array_agg(d.id::text), '{}')
		from artifacts d
		where d.id = any(array(
			select g.primary_id from gate_issues g
			union
			select g.verification_id from gate_issues g
		))
		  and d.retired_at is null
	)`
	return ctes, []string{
		`array(select row(g.key, g.root, g.steps, g.primary_id::text, g.verification_id::text) from gate_issues g)`,
		`array(select row(d.id::text, coalesce((select max(v.number) from artifact_versions v where v.artifact_id = d.id), 0))
			from artifacts d where d.id = any(` + documentIDs + `::uuid[]))`,
		`array(select row(review.*) from (` + bindFragment(latestReviewsStatement, documentIDs) + `) review)`,
		`array(select row(request.*) from (` + bindFragment(openApprovalAsksStatement, documentIDs) + `) request)`,
	}
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
