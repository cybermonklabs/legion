package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// sentStatement is one statement a store's connection sent, with its arguments.
type sentStatement struct {
	sql  string
	args []any
}

// statementCounter records every statement the connections of one store send, a pipeline's one
// by one (pgx.QueryTracer and pgx.BatchTracer), so a test counts the statements of some text a
// request sent. It is installed on the store's connection configuration, and every pool the store
// opens later is opened from a copy of it, so it sees the shared pool and the rooms pool alike.
type statementCounter struct {
	mu   sync.Mutex
	sent []sentStatement
}

func (c *statementCounter) record(statements ...sentStatement) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, statements...)
}

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.record(sentStatement{sql: data.SQL, args: data.Args})
	return ctx
}

func (c *statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *statementCounter) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, queued := range data.Batch.QueuedQueries {
		c.record(sentStatement{sql: queued.SQL, args: queued.Arguments})
	}
	return ctx
}

func (c *statementCounter) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (c *statementCounter) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// mark is where the statements recorded from now on start, for a later count since it.
func (c *statementCounter) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

// count is how many statements sent since mark contain text and name first as their first
// argument.
func (c *statementCounter) count(since int, text, first string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, statement := range c.sent[since:] {
		if strings.Contains(statement.sql, text) && len(statement.args) > 0 && fmt.Sprint(statement.args[0]) == first {
			count++
		}
	}
	return count
}

// The statements a socket's admission sends for the document's issue (docs.Service.issueOpen) and,
// with cookie identity, for the caller's session (store.PgSessionStore.CurrentSessionGeneration,
// which identity.CookieIdentity.Login reads once per resolution of the caller).
const (
	issueReadText = "coalesce(i.closed_at is null, true)"
	actorReadText = "from user_sessions where login"
	signedInLogin = "alice@d.example"
)

// A browser opens a document's socket on every page load and every reconnect, and an eviction or a
// deploy reconnects every browser of a room at once. Admitting one reads what it needs once: the
// document's issue once and the caller once, whether the room is warm or cold and with the
// Hocuspocus authentication frame the provider sends once the socket is open.
func TestASocketOpenReadsTheIssueAndTheCallerOnce(t *testing.T) {
	ctx := context.Background()
	counter := &statementCounter{}
	counted := storetest.TracedStore(t, storetest.Open(t), counter)
	// No heartbeat resolves the caller again while the test counts.
	handler, _, cookie, request, documentService := newCookieServerOn(t, counted, time.Hour)
	if response := request(http.MethodPost, "/api/v1/projects", map[string]string{"key": "TEST", "name": "Test"}); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := request(http.MethodPost, "/api/v1/issues", map[string]string{"project": "TEST", "title": "Socket admission", "spec": "Before.\n"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	room := issue.PrimaryArtifactID
	// The issue's creation may have loaded the document's room; the first socket opens it cold.
	if err := documentService.Evict(ctx, room); err != nil {
		t.Fatalf("evict the document's room: %v", err)
	}

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	schemaVersion := strconv.Itoa(pmdoc.SchemaVersion())
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + room + "?schema_version=" + schemaVersion
	header := http.Header{"Cookie": []string{cookie.Name + "=" + cookie.Value}}
	for _, open := range []string{"a cold open", "a warm open"} {
		since := counter.mark()
		peer := docstest.Dial(t, url, header, room, crdt.New())
		scope, err := peer.Authenticate(schemaVersion, 10*time.Second)
		if err != nil || scope != "read-write" {
			t.Fatalf("%s: the socket is authenticated %q (%v), want read-write", open, scope, err)
		}
		if reads := counter.count(since, issueReadText, room); reads != 1 {
			t.Errorf("%s read the document's issue %d times, want once", open, reads)
		}
		if reads := counter.count(since, actorReadText, signedInLogin); reads != 1 {
			t.Errorf("%s resolved the caller %d times, want once", open, reads)
		}
	}
}

// A socket whose caller's heartbeat check finds the pool busy while its document loads is not
// refused for it, and never taken for a browser that left: a busy pool says nothing of the caller,
// so the heartbeat keeps the socket and checks again (whileCallerResolves,
// heartbeatTransientCeiling), and the socket is answered as its load answers it - admitted when the
// load reads the document, refused 503 with Retry-After when the load finds its own pool busy, as
// the document service refuses such a socket. Neither is the empty 200 logged 499 that a
// cancellation taken for the client's own answered, which is what a busy pool's check did to a
// browser still waiting for its handshake. The browser here keeps its request open throughout:
// the admission's load is held (heldSocketLoadStore), the caller's check finds the pool busy at
// two heartbeats in a row once that load has begun, and the load is let go only once a third
// heartbeat has checked again, while the handshake is still unanswered.
func TestASocketWhoseCallerStopsResolvingDuringTheLoadIsAnsweredAsItsLoadAndNotAsAGoneClient(t *testing.T) {
	for _, load := range []struct {
		name   string
		fails  error
		status int
	}{
		{"the load reads the document", nil, http.StatusSwitchingProtocols},
		{"the load finds its pool busy", fmt.Errorf("read the document's history: %w", store.ErrPoolBusy), http.StatusServiceUnavailable},
	} {
		t.Run(load.name, func(t *testing.T) {
			// Long enough that a check of the caller, one read, ends inside the heartbeat that
			// bounds it on a loaded runner, so the only failures are the two this test makes.
			const heartbeat = 250 * time.Millisecond
			database := storetest.Open(t)
			caller := &busyCallerIdentity{Identity: headerIdentity(database)}
			held := &heldSocketLoadStore{loading: make(chan struct{}), release: make(chan struct{}), fails: load.fails}
			handler, _, deps := newTestServer(t, testServerOptions{
				store: database, identity: caller, browserPeers: true, settle: time.Hour, streamHeartbeat: heartbeat,
				persistence: func(database *store.Store) docs.VersionedStore {
					held.VersionedStore = docs.NewPgVersioned(database)
					return held
				},
			})
			room := createInteractionIssue(t, handler, "BEAT", "Heartbeat during the load", "Before.\n").PrimaryArtifactID
			// The issue's creation may have loaded the document's room; the socket opens it cold,
			// so its admission loads the document from the store.
			if err := deps.Docs.(*docs.Service).Evict(context.Background(), room); err != nil {
				t.Fatalf("evict the document's room: %v", err)
			}
			logged := captureLog(t)
			held.hold(room)
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)

			url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + room + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
			type handshake struct {
				status     int
				retryAfter string
				err        error
			}
			answered := make(chan handshake, 1)
			go func() {
				connection, response, err := gws.DefaultDialer.Dial(url, http.Header{"X-Dispatch-User": []string{"alice"}})
				if connection != nil {
					_ = connection.Close()
				}
				got := handshake{err: err}
				if response != nil {
					got.status, got.retryAfter = response.StatusCode, response.Header.Get("Retry-After")
					_ = response.Body.Close()
				}
				answered <- got
			}()
			select {
			case <-held.loading:
			case <-time.After(30 * time.Second):
				t.Fatal("the socket's admission never loaded its document, so nothing here is tested")
			}

			// The browser is still waiting for its handshake's answer; the next two heartbeats find
			// the pool busy, and the one after them checks the caller again. Two, written out rather
			// than read off heartbeatTransientCeiling, so a ceiling lowered is a row that fails.
			caller.busy.Store(2)
			pollUntil(t, 10*time.Second, "two heartbeat checks that found the pool busy", func() bool { return caller.busy.Load() == 0 })
			checked := caller.asked.Load()
			pollUntil(t, 10*time.Second, "a heartbeat check after the two that found the pool busy", func() bool {
				return caller.asked.Load() > checked
			})
			select {
			case got := <-answered:
				t.Fatalf("the socket's handshake was answered %d (%v) while its load was held, want it still waiting", got.status, got.err)
			default:
			}
			if err := held.ctx.Err(); err != nil {
				t.Fatalf("the held load's context ended (%v: %v) after two heartbeats found the pool busy, want the admission going on", err, context.Cause(held.ctx))
			}

			close(held.release)
			var got handshake
			select {
			case got = <-answered:
			case <-time.After(30 * time.Second):
				t.Fatal("the socket's handshake was never answered once its load was let go")
			}
			if got.status != load.status {
				t.Fatalf("the waiting browser's handshake was answered %d (%v), want %d", got.status, got.err, load.status)
			}
			if load.status == http.StatusServiceUnavailable && got.retryAfter != admission.RetryAfter {
				t.Errorf("the refused socket's Retry-After is %q, want %q", got.retryAfter, admission.RetryAfter)
			}

			var access string
			pollUntil(t, 10*time.Second, "the socket's access-log line", func() bool {
				for _, line := range accessLines(t, logged) {
					if strings.Contains(line, "path=/ws/doc/"+room) {
						access = line
						return true
					}
				}
				return false
			})
			if want := "status=" + strconv.Itoa(load.status); !strings.Contains(access, want) {
				t.Errorf("the socket's access-log line %q does not carry %s, the answer its browser read", access, want)
			}
			// A socket the heartbeat ended is refused with the class of the heartbeat's cause on
			// the document service's line (docs' refuseUnservedSocket), which carries no cause
			// otherwise.
			for line := range strings.Lines(logged.Lines("a document socket was refused")) {
				if strings.Contains(line, " cause=") {
					t.Errorf("a heartbeat that found the pool busy ended the socket: %s", line)
				}
			}
		})
	}
}

// busyCallerIdentity resolves a caller as its Identity does but for as many resolutions as busy
// holds, each of which fails as a resolution under load does - the read of the caller's session
// finds the pool busy - and takes one off busy. asked counts every resolution it is asked for.
type busyCallerIdentity struct {
	identity.Identity
	busy  atomic.Int32
	asked atomic.Int32
}

func (i *busyCallerIdentity) Login(r *http.Request) (string, error) {
	i.asked.Add(1)
	for {
		left := i.busy.Load()
		if left <= 0 {
			return i.Identity.Login(r)
		}
		if i.busy.CompareAndSwap(left, left-1) {
			return "", fmt.Errorf("read the caller's session: %w", store.ErrPoolBusy)
		}
	}
}

// heldSocketLoadStore holds the first Load of the room hold names, closing loading as that Load
// begins with the context it loads on in ctx, until release is closed or that context ends. Let go,
// it answers fails where one is set and loads otherwise; on an ended context it loads on that
// context, which the store answers context.Canceled, as it would had the context ended mid-read.
type heldSocketLoadStore struct {
	docs.VersionedStore
	room    atomic.Pointer[string]
	loading chan struct{}
	release chan struct{}
	fails   error
	ctx     context.Context
	once    sync.Once
}

func (s *heldSocketLoadStore) hold(room string) { s.room.Store(&room) }

func (s *heldSocketLoadStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	first := false
	if held := s.room.Load(); held != nil && *held == room {
		s.once.Do(func() {
			first = true
			s.ctx = ctx
			close(s.loading)
		})
	}
	if first {
		select {
		case <-s.release:
			if s.fails != nil {
				return persistence.LoadResult{}, s.fails
			}
		case <-ctx.Done():
		}
	}
	return s.VersionedStore.Load(ctx, room)
}
