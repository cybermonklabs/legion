package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testwait"
)

// runDocumentRoomSync runs documentService's room sync on a listener hub of its own, as
// cmd/dispatch does, until the test ends, and returns once the hub's first LISTEN stands: a
// notification committed before that reaches nobody. Its one bound is the test binary's deadline
// (testwait.Receive).
func runDocumentRoomSync(t *testing.T, database *store.Store, documentService *docs.Service) {
	t.Helper()
	listening := runDocumentRoomSyncDialing(t, documentService, documentListenerDial(database, documentService))
	testwait.Receive(t, "the document service's listener hub to listen", listening)
}

// documentListenerDial is documentService's listener connection as cmd/dispatch dials it: a
// dedicated connection of database named for the process, which is what makes the process a
// member of the room sync.
func documentListenerDial(database *store.Store, documentService *docs.Service) func(context.Context) (*pgx.Conn, error) {
	return func(ctx context.Context) (*pgx.Conn, error) {
		return database.Pool.Dedicated(ctx, docs.ListenerApplicationPrefix+documentService.ProcessID())
	}
}

// runDocumentRoomSyncDialing is runDocumentRoomSync over dial, the hub's own connection maker,
// and it waits for no LISTEN: a test whose hub cannot connect yet wraps documentListenerDial
// with a dial that fails or waits, and has to write while that hub is down. The channel takes a
// token on each LISTEN that stands, and drops one it has no room for.
func runDocumentRoomSyncDialing(
	t *testing.T, documentService *docs.Service, dial func(context.Context) (*pgx.Conn, error),
) (listening <-chan struct{}) {
	t.Helper()
	hub := &pglisten.Hub{Owner: "dispatch test", Dial: dial}
	run := documentService.RegisterRoomSync(hub)
	connected := make(chan struct{}, 1)
	hub.OnConnected(func() {
		select {
		case connected <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Add(2)
	go func() {
		defer running.Done()
		run(ctx)
	}()
	go func() {
		defer running.Done()
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		running.Wait()
	})
	return connected
}

// Closing an issue is durable the moment its transaction commits, and the commit itself tells
// every process's document rooms (migration 20261101181500's trigger,
// docs.Service.issueOpenChanged): the route makes no call of its own after it, so nothing about
// the request - a client that has hung up included - can keep a room taking edits from the
// browsers connected to it.
func TestClosingAnIssueClosesItsRoomsFromItsCommit(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{
			Store:    database,
			Identity: headerIdentity(database),
			Settle:   time.Hour,
		})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	runDocumentRoomSync(t, database, documentService)
	issue := createInteractionIssue(t, handler, "TEST", "Closing issue", "A spec")
	documentServer := httptest.NewServer(http.HandlerFunc(documentService.ServeHTTP))
	t.Cleanup(documentServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(documentServer.URL, "http") + "/ws/doc/" + issue.PrimaryArtifactID
	connection, wsResponse, err := dialLive(t, gws.DefaultDialer, wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", wsResponse, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	body, err := json.Marshal(map[string]string{"status": "done"})
	if err != nil {
		t.Fatalf("encode close request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/issues/"+issue.Key, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", response.Code, response.Body.String())
	}

	waitForDocumentConnectionClose(t, connection)
	if _, err := replaceDocumentText(
		database, documentService, issue.PrimaryArtifactID, "after", model.Actor{Kind: "user", ID: "alice"},
	); !errors.Is(err, docs.ErrIssueClosed) {
		t.Fatalf("write to the closed issue's document = %v, want ErrIssueClosed", err)
	}
}

// slowWithdrawal is how long the close below holds its transaction open after writing the
// withdrawn ask's block, before a browser types: longer than the document gate's closeMargin (two
// seconds, docs/append_gate.go), by a margin no scheduling delay of the test eats.
const slowWithdrawal = 4 * time.Second

// A close's transaction writes closed_at, then withdraws the issue's open asks, writing their blocks
// into the issue's documents under each document's advisory lock, which it holds until it commits.
// A browser's keystroke in a room of such a document on another process waits on that lock, so it
// reaches the append gate only after the close committed. The keystroke was made before the close
// took effect, so it is stored however long the withdrawal took: the gate measures closeMargin from
// the close's commit (the close stamps closed_at again just before it commits), not from the start
// of its transaction. The commit's notification then closes the room, and the browser reconnects
// read-only, holding the keystroke. Here the close holds its transaction open for longer than
// closeMargin after writing the withdrawn block, and the browser types then.
func TestAKeystrokeWaitingOnASlowClosesDocumentLockIsStored(t *testing.T) {
	wrapper := &peerInsertDocs{}
	var documentService *docs.Service
	handler, database, deps := newInteractionDeps(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Identity: headerIdentity(database), Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		wrapper.API = documentService
		return wrapper
	})
	runDocumentRoomSync(t, database, documentService)
	issue := createInteractionIssue(t, handler, "TEST", "A slow close", closeDecision)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
	awaitSettled(t, database, issue.PrimaryArtifactID)

	// The browser's room is on another process, which hears the close from its commit. A room
	// stores a browser's updates on a pool of its own (store.Pool.RoomAppends), opened from the
	// process's pool configuration, so that process's pool carries a tracer that names the backend
	// of each of its connections that takes a document's lock (roomLockBackends). The close runs
	// on a connection of its own (oneConnectionHandler), and the wait for the keystroke's append
	// watches those backends behind the close's (waitForAppendBehind): no other session waiting on
	// the document's lock - a settlement of the first process - is taken for it.
	appends := &roomLockBackends{}
	second := &store.Store{Pool: storetest.Traced(t, database.Pool, appends, nil)}
	other := docs.New(docs.Deps{Store: second, Identity: headerIdentity(second), Settle: time.Hour})
	t.Cleanup(func() { _ = other.Shutdown(context.Background()) })
	runDocumentRoomSync(t, second, other)
	peer := connectBrowserPeer(t, other, issue.PrimaryArtifactID)
	closeHandler, closePID := oneConnectionHandler(t, deps)
	wrapper.insert = func() {
		// The close's transaction began before its withdrawal wrote the block, so the keystroke
		// is typed more than closeMargin after that start.
		time.Sleep(slowWithdrawal)
		peer.appendToFirstParagraph(t, " typed during the close")
		waitForAppendBehind(t, database, closePID, appends)
	}
	closed := dispatchRequest(t, closeHandler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}

	testwait.Receive(t, "the close to end the browser's room on the other process", peer.connection().Ended)
	testwait.UntilPolling(t, 20*time.Millisecond, "the keystroke typed while the close's write held the document's lock to be stored (the store's text)", func() (string, bool) {
		stored := storedDocumentText(t, database, issue.PrimaryArtifactID)
		return stored, strings.Contains(stored, "typed during the close")
	})
	peer.reconnect(t)
	// docstest's Authenticate waits under a bound of its own, so it is handed one no run reaches,
	// and the room's answer is waited for here instead, whose one bound is the test binary's
	// deadline (testwait.Receive).
	type authenticated struct {
		scope string
		err   error
	}
	answered := make(chan authenticated, 1)
	go func() {
		scope, err := peer.connection().Authenticate(strconv.Itoa(pmdoc.SchemaVersion()), math.MaxInt64)
		answered <- authenticated{scope, err}
	}()
	reply := testwait.Receive(t, "the room's answer to the reconnected browser's auth token", answered)
	scope, err := reply.scope, reply.err
	if err != nil {
		t.Fatalf("authenticate the reconnected browser: %v", err)
	}
	if scope != "readonly" {
		t.Fatalf("the browser reconnected to the closed issue's document with scope %q, want readonly", scope)
	}
}

// roomLockBackends is a tracer (storetest.Traced) that records the backend of each connection
// that sends a document room's lock, `select pg_advisory_lock(hashtext($1))`: a room's append
// opens with it (docs' room_appends.go), in a batch, and a room's other writers send it alone
// (docs' persistence.go). A pool opened from the traced configuration - a process's room-appends
// pool among them - carries it, so it names the backends of that process's room writers, whose
// pids no test can know before they connect.
type roomLockBackends struct {
	mu   sync.Mutex
	pids []uint32
}

// roomLockStatement is the statement roomLockBackends watches for.
const roomLockStatement = `select pg_advisory_lock(hashtext($1))`

func (r *roomLockBackends) note(conn *pgx.Conn, sql string) {
	if !strings.Contains(sql, roomLockStatement) {
		return
	}
	pid := conn.PgConn().PID()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.pids, pid) {
		r.pids = append(r.pids, pid)
	}
}

// backends is every backend recorded so far.
func (r *roomLockBackends) backends() []uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.pids)
}

func (r *roomLockBackends) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.note(conn, data.SQL)
	return ctx
}

func (r *roomLockBackends) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *roomLockBackends) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, queued := range data.Batch.QueuedQueries {
		r.note(conn, queued.SQL)
	}
	return ctx
}

func (r *roomLockBackends) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (r *roomLockBackends) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// waitForAppendBehind returns once one of the backends appends recorded - a room's append, which
// takes the document's advisory lock (lockRoom) - waits behind a lock the backend holderPID owns
// (storetest.QueuedBehindBackend), as a close holds that lock while it writes the document. Its
// one bound is the test binary's deadline (testwait.UntilPolling).
func waitForAppendBehind(t *testing.T, database *store.Store, holderPID uint32, appends *roomLockBackends) {
	t.Helper()
	testwait.UntilPolling(t, 10*time.Millisecond, fmt.Sprintf("a room writer of the other process to wait behind backend %d (the writers' backends)", holderPID), func() ([]uint32, bool) {
		backends := appends.backends()
		for _, pid := range backends {
			queued, err := storetest.QueuedBehindBackend(context.Background(), database, holderPID, pid)
			if err != nil {
				t.Fatal(err)
			}
			if queued {
				return backends, true
			}
		}
		return backends, false
	})
}

// storedDocumentText renders every stored update of artifactID, as a room loading it would hold it.
func storedDocumentText(t *testing.T, database *store.Store, artifactID string) string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select update from doc_updates where artifact_id = $1 order by version`, artifactID)
	if err != nil {
		t.Fatalf("read the stored document updates: %v", err)
	}
	updates, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("read the stored document updates: %v", err)
	}
	document := crdt.New()
	for _, update := range updates {
		if err := crdt.ApplyUpdateV1(document, update, nil); err != nil {
			t.Fatalf("apply a stored document update: %v", err)
		}
	}
	return renderDocument(t, document)
}

// waitForDocumentConnectionClose waits for the server to close a connected client's connection.
// The connection is read on a goroutine under no read deadline, and the wait's one bound is the
// test binary's deadline (testwait.Receive); the test's cleanup closes a connection the server
// left open, which ends that goroutine.
func waitForDocumentConnectionClose(t *testing.T, connection *gws.Conn) {
	t.Helper()
	closed := make(chan error, 1)
	go func() {
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				closed <- err
				return
			}
		}
	}()
	testwait.Receive(t, "the server to close the document connection once the issue closed", closed)
}
