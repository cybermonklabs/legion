package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/persistence"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// connectPeer connects login's browser to artifactID's room through the document server at
// serverURL, at the schema version the dashboard presents, so the room takes its edits.
func connectPeer(t *testing.T, serverURL, artifactID, login string) *docstest.Peer {
	t.Helper()
	url := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws/doc/" + artifactID +
		"?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	return docstest.Dial(t, url, http.Header{"X-Dispatch-User": []string{login}}, artifactID, crdt.New())
}

func TestIssueCloseClosesOpenDocumentConnection(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	service.issueOpenChanged(context.Background(), "DOC-1")
	waitForNoLiveDocument(t, service, artifactID)
	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
	if err := service.srv.Apply(context.Background(), artifactID, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {}); !errors.Is(err, ErrIssueClosed) {
		t.Fatalf("server write after issue close = %v, want ErrIssueClosed", err)
	}
}

// A browser that connects while its issue is closed is admitted read-only, and a connection keeps
// the admission it was authorized with. So a reopen closes the issue's rooms as a close does: the
// browser reconnects and is admitted writable, rather than staying read-only until the person
// reloads, whichever side of the reopen its reconnect after the close happened to land. It does so
// whatever the room's state says: a state a connection's own registration (addConnection) or a
// load after the reopen committed makes holds the default flag, open, while a browser on it was
// admitted read-only.
func TestIssueReopenClosesADocumentConnectionAdmittedWhileClosed(t *testing.T) {
	for _, row := range []struct {
		name string
		// recreated gives the room's state the flag a recreated state starts with.
		recreated bool
	}{
		{name: "the state the close flagged closed"},
		{name: "a recreated state's default open flag", recreated: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, "before")
			httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
			t.Cleanup(httpServer.Close)
			setIssueClosed := func(closed bool) {
				t.Helper()
				if _, err := service.store.Pool.Exec(context.Background(),
					`update issues set closed_at = case when $1::boolean then now() end where key = 'DOC-1'`, closed); err != nil {
					t.Fatalf("set the document issue's closed_at: %v", err)
				}
				service.issueOpenChanged(context.Background(), "DOC-1")
			}
			setIssueClosed(true)
			reader := connectPeer(t, httpServer.URL, artifactID, "alice")
			if err := reader.WaitForText(peerMarkdown, "before\n", 10*time.Second); err != nil {
				t.Fatal(err)
			}

			// The premise: the room admitted the reader read-only, so it drops what the reader
			// types and keeps the connection. The room handles one connection's frames in order, so
			// once it answers the request the reader sends after its keystroke, it has handled the
			// keystroke.
			typeOver(t, reader, "before", "refused")
			for len(reader.Answers) > 0 {
				<-reader.Answers
			}
			if err := reader.AskForDocument(); err != nil {
				t.Fatalf("ask the room for the document after the reader's keystroke: %v", err)
			}
			select {
			case <-reader.Answers:
			case <-reader.Ended:
				t.Fatal("the room closed the read-only reader's connection over its keystroke")
			case <-time.After(10 * time.Second):
				t.Fatal("the room did not answer the reader after its keystroke")
			}
			if text := documentText(t, service, artifactID); text != "before\n" {
				t.Fatalf("the room took the read-only reader's keystroke: it holds %q, want %q", text, "before\n")
			}
			if row.recreated {
				state := service.room(artifactID)
				state.mu.Lock()
				state.closed = false
				state.mu.Unlock()
			}

			setIssueClosed(false)
			// The regression check: the reopen ends the connection admitted read-only, which the
			// browser's provider answers by reconnecting.
			select {
			case <-reader.Ended:
			case <-time.After(5 * time.Second):
				t.Fatal("the connection admitted while the issue was closed was still open 5s after the reopen")
			}
			// The room a connection now loads takes edits. It loads from the store, so its copy
			// also shows the read-only reader's keystroke never reached the store.
			editor := connectPeer(t, httpServer.URL, artifactID, "alice")
			if err := editor.WaitForText(peerMarkdown, "before\n", 10*time.Second); err != nil {
				t.Fatal(err)
			}
			typeOver(t, editor, "before", "after")
			waitForPersistedProofText(t, service.store, artifactID, "after\n")
		})
	}
}

func TestClosedColdRoomAuthorizesReadOnly(t *testing.T) {
	service, artifactID := newTestService(t)
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/ws/doc/"+artifactID, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	request.SetPathValue("room", artifactID)
	request = request.WithContext(context.WithValue(request.Context(), connectionContextKey{}, &connectionState{}))
	config, ok := service.authorize(request)
	if !ok || !config.ReadOnly {
		t.Fatalf("cold closed room authorization = %#v, %t; want read-only acceptance", config, ok)
	}
}

func TestSchemaVersionAdmissionAuthorizesMismatchAndVersionlessClientsReadOnly(t *testing.T) {
	for _, schemaVersion := range []string{"0", ""} {
		t.Run(schemaVersion, func(t *testing.T) {
			service, artifactID := newTestService(t)
			request := httptest.NewRequest(
				http.MethodGet,
				"/ws/doc/"+artifactID+"?schema_version="+schemaVersion,
				nil,
			)
			request.Header.Set("X-Dispatch-User", "alice")
			request.SetPathValue("room", artifactID)
			request = request.WithContext(
				context.WithValue(request.Context(), connectionContextKey{}, &connectionState{}),
			)

			config, ok := service.authorize(request)
			if !ok || !config.ReadOnly {
				t.Fatalf(
					"schema version %q authorization = %#v, %t; want read-only acceptance",
					schemaVersion,
					config,
					ok,
				)
			}
		})
	}
}

func TestSchemaVersionAdmissionCommunicatesReadOnlyScope(t *testing.T) {
	service, artifactID := newTestService(t)

	mismatched, err := service.authorizeSchemaVersion(artifactID, "0")
	if err != nil {
		t.Fatalf("authorize mismatched schema version: %v", err)
	}
	if !mismatched.ReadOnly {
		t.Fatalf("mismatched schema admission = %#v, want read-only", mismatched)
	}

	current, err := service.authorizeSchemaVersion(artifactID, fmt.Sprintf("%d", pmdoc.SchemaVersion()))
	if err != nil {
		t.Fatalf("authorize current schema version: %v", err)
	}
	if current.ReadOnly {
		t.Fatalf("current schema admission = %#v, want read-write", current)
	}
}

func TestHocuspocusAuthenticationCommunicatesSchemaReadOnly(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	auth := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(2) // Hocuspocus authentication message.
		encoder.WriteVarUint(0) // Token authentication payload.
		encoder.WriteVarString("0")
	})
	if err := connection.WriteMessage(gws.BinaryMessage, auth); err != nil {
		t.Fatalf("send schema authentication: %v", err)
	}

	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		_, message, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read schema authentication reply: %v", err)
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			t.Fatalf("read Hocuspocus document name: %v", err)
		}
		kind, err := decoder.ReadVarUint()
		if err != nil {
			t.Fatalf("read Hocuspocus message kind: %v", err)
		}
		if kind != 2 {
			continue
		}
		subtype, err := decoder.ReadVarUint()
		if err != nil {
			t.Fatalf("read Hocuspocus authentication subtype: %v", err)
		}
		scope, err := decoder.ReadVarString()
		if err != nil {
			t.Fatalf("read Hocuspocus authentication scope: %v", err)
		}
		if subtype != 2 || scope != "readonly" {
			t.Fatalf("schema authentication = subtype %d scope %q, want authenticated readonly", subtype, scope)
		}
		return
	}
}

func TestLoadFailureMakesDocumentServiceUnavailable(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), loadErr: errors.New("load failed")},
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("failed-room connection: response=%#v err=%v, want HTTP 503", response, err)
	}
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("load failure = %v, want ErrServiceUnavailable", err)
	}
}

// A history that does not decode refuses the socket before any upgrade, and a read of it is the
// state a rebuild repairs (ErrDocumentUnloadable) rather than any failed room's ErrServiceUnavailable.
func TestCorruptLoadRefusesTheSocketAndReadsAsUnloadable(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), loadUpdate: []byte{0xff}},
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("corrupt-room connection: response=%#v err=%v, want HTTP 503", response, err)
	}
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocumentUnloadable) || errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("corrupt load = %v, want ErrDocumentUnloadable", err)
	}
}

// refusedForTheItemCap checks a refusal naming the item count an update's header declares, the
// count read from that header alone: the refusal the store writes for that count
// (itemCapRefusal), rather than the sentence written again here.
func refusedForTheItemCap(t *testing.T, what string, err error, items uint64) {
	t.Helper()
	want := itemCapRefusal(items, maxUpdateItems)
	if !errors.Is(err, ErrDocumentTooLarge) || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("%s: %v, want %v", what, err, want)
	}
}

// The store judges an update by the item count its header declares (updateItems) rather than by
// decoding it: a transaction's append of an update this process encoded is refused past the cap
// from that header (AppendUpdateTx, refuseUpdateItemCap), and so is a stored history a room is
// about to take (servicePersistenceAdapter.LoadDoc, loadableHistory), which took that decode off
// every cold load of a document one client wrote (sjawhar/legion#2519).
//
// A header that reads says nothing about the body behind it, so the append of a malformed update
// is taken, and the refusal comes when the room's own load applies that history to its document
// (onLoadDocument, bootstrapLoadedState). That refusal is the load's own failure, not one of this
// service's answers, so the hook fails the room there and drops the record the load began: the
// room is left failed for the next access to reload, and the record goes because no instance will
// ever take it (beginRoomLoad, keepInstance).
func TestTheStoreJudgesAnUpdateByItsHeader(t *testing.T) {
	overTheCap := encoding.NewEncoder()
	overTheCap.WriteVarUint(1)
	overTheCap.WriteVarUint(maxUpdateItems + 1)
	malformed := encoding.NewEncoder()
	malformed.WriteVarUint(1)
	malformed.WriteVarUint(1)
	malformed.WriteRaw([]byte{0xff, 0xff, 0xff})
	for _, test := range []struct {
		name     string
		update   []byte
		appended func(*testing.T, error)
		loaded   func(*testing.T, error)
	}{
		{
			name:   "a header declaring more items than one update may hold",
			update: overTheCap.Bytes(),
			appended: func(t *testing.T, err error) {
				refusedForTheItemCap(t, "a transaction's append of an update over the cap", err, maxUpdateItems+1)
			},
			loaded: func(t *testing.T, err error) {
				refusedForTheItemCap(t, "a room's load of a history over the cap", err, maxUpdateItems+1)
			},
		},
		{
			name:   "a header that reads over a body that does not",
			update: malformed.Bytes(),
			appended: func(t *testing.T, err error) {
				if err != nil {
					t.Fatalf("a transaction's append of a malformed update: %v, want it taken: its header is all this check reads", err)
				}
			},
			loaded: func(t *testing.T, err error) {
				if err == nil {
					t.Fatal("a room took a history ygo cannot decode, want its load refused")
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := storetest.Open(t)
			artifactID := createDocument(t, database, "")
			ctx := context.Background()
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the append: %v", err)
			}
			defer tx.Rollback(ctx)
			_, appended := NewPgVersioned(database).AppendUpdateTx(ctx, tx, artifactID, test.update, true)
			test.appended(t, appended)

			service := New(Deps{
				Store:       database,
				Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), loadUpdate: test.update},
				Events:      events.NewBroker(),
				Identity:    headerIdentity(database),
				Settle:      time.Hour,
			})
			t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
			// The failed room's eviction is held, so the failure it carries is still the room's
			// to read below: the eviction releases the room's state as it goes.
			hold := make(chan struct{})
			defer close(hold)
			service.beforeFailedEviction = func(room string) {
				if room == artifactID {
					<-hold
				}
			}

			test.loaded(t, service.warmLiveDocument(ctx, artifactID))
			if failure := service.roomFailure(artifactID); failure == nil {
				t.Error("the refused load left the room healthy, so nothing reloads the document")
			}
			if _, kept := service.roomLoads.Load(artifactID); kept {
				t.Error("the refused load left its record behind, which no instance will take")
			}
		})
	}
}

// panickingRoomLoadStore panics in the store read a room's load makes
// (servicePersistenceAdapter.LoadDoc), for the first load of one room and no other read. That
// panic is the one way a load fails with nothing of this service still to run on its goroutine:
// the read hands ygo no bytes, so ygo decodes nothing after it, and a panic out of the hook ygo
// calls next is recovered there (bootstrapLoadedState) or swallowed by ygo's own safeHook.
//
// cancel, where a row sets one, ends the caller's context inside that read and before the panic,
// so the caller whose Apply meets ygo's answer has gone and can fail nothing.
type panickingRoomLoadStore struct {
	VersionedStore
	room   string
	loads  atomic.Int64
	cancel func()
}

func (s *panickingRoomLoadStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if room != s.room || s.loads.Add(1) > 1 {
		return s.VersionedStore.Load(ctx, room)
	}
	if s.cancel != nil {
		s.cancel()
	}
	panic("the store's read of this room's history panicked")
}

// A load that panics is failed where the panic is met, on the load's own goroutine and before ygo
// closes the room's barrier (LoadDoc), as the store's refusal and a history the room's document
// cannot take are failed there (loadableHistory, bootstrapLoadedState). It was once left to whichever
// caller's Apply met ygo's recovery of it (sjawhar/ygo v1.55.1-sami.7,
// provider/websocket/server.go:2010-2029), and such a caller can answer for no load: ygo removes
// the room and closes its barrier before any waiter wakes (server.go:2031-2040), so the record
// under the room's name by the time that caller looks can be a later load's, and failing that
// room would fail the room the later load is building, while dropping its record would leave its
// instance holding none, so the versions appended while it ran would reach no cursor
// (keepInstance, sjawhar/legion#2519). A caller whose own context ended meanwhile answered for
// nothing at all, and the room stayed healthy behind a load that never made one.
//
// So no caller answers for a load now. Whoever is waiting, the panicking load fails its room and
// drops its own record, and the load that begins after it is left alone: it keeps its own record
// and finds a room it can build.
func TestALoadThatPanicsFailsItsRoomWhereThePanicIsMet(t *testing.T) {
	for _, row := range []struct {
		name string
		// callerGone ends the caller's context inside the read that panics.
		callerGone bool
	}{
		{name: "a caller still waiting for the load"},
		{name: "a caller whose context ended while the load ran", callerGone: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			database := storetest.Open(t)
			artifactID := createDocument(t, database, "")
			panicking := &panickingRoomLoadStore{VersionedStore: NewPgVersioned(database), room: artifactID}
			service := New(Deps{
				Store:       database,
				Persistence: panicking,
				Events:      events.NewBroker(),
				Identity:    headerIdentity(database),
				Settle:      time.Hour,
			})
			t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
			// The failed room's eviction is held, so the failure the load leaves is still the
			// room's to read below: the eviction releases the room's state as it goes.
			evicting := make(chan struct{})
			releaseEviction := sync.OnceFunc(func() { close(evicting) })
			defer releaseEviction()
			service.beforeFailedEviction = func(room string) {
				if room == artifactID {
					<-evicting
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if row.callerGone {
				panicking.cancel = cancel
			}

			if err := service.warmLiveDocument(ctx, artifactID); err == nil {
				t.Fatal("the load's read panicked, want the load refused")
			}
			if failure := service.roomFailure(artifactID); failure == nil {
				t.Error("the panicking load left the room healthy, so nothing reloads the document")
			}
			if _, kept := service.roomLoads.Load(artifactID); kept {
				t.Error("the panicking load left its record behind, which no instance will take")
			}

			// The load that begins after it. Its caller waits for the failed room's eviction
			// (awaitRoomRecovery), so this reads what that load left, not what the one before it did.
			releaseEviction()
			if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
				t.Fatalf("the load after the one that panicked: %v, want the document loaded", err)
			}
			if _, kept := service.roomLoads.Load(artifactID); !kept {
				t.Error("the load after the one that panicked kept no record, so an append while it ran reaches no cursor")
			}
			if failure := service.roomFailure(artifactID); failure != nil {
				t.Errorf("the load after the one that panicked met a failed room: %v", failure)
			}
		})
	}
}

// A cold socket's admission check hands the room's load the history it decoded, so the history
// is read once and the room loads with that very state - but only while it is still the stored
// one. An update appended in between raises the head, and the load reads the store again rather
// than open the room without that update.
func TestASocketsPreloadIsServedOnlyWhileTheHistoryHasNotMoved(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	var mu sync.Mutex
	var recording bool
	var decoded [][]byte
	service.bootstrapping = func(room string, state []byte) {
		mu.Lock()
		defer mu.Unlock()
		if recording && room == artifactID {
			decoded = append(decoded, state)
		}
	}
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	closeDocumentRoom(t, service, artifactID)
	preload := func(t *testing.T) []byte {
		t.Helper()
		tree, loaded, _, err := service.loadTree(context.Background(), artifactID)
		if err != nil || loaded == nil || len(loaded.Update) == 0 {
			t.Fatalf("load cold document: loaded=%v err=%v, want its durable history", loaded, err)
		}
		service.offerPreload(artifactID, loaded, admissionRendering{tree: tree})
		return loaded.Update
	}
	loadRoom := func(t *testing.T, want string) []byte {
		t.Helper()
		mu.Lock()
		recording, decoded = true, nil
		mu.Unlock()
		if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
			t.Fatalf("load the room: %v", err)
		}
		if text, err := service.Text(context.Background(), artifactID); err != nil || !strings.Contains(text, want) {
			t.Fatalf("the loaded room reads %q (%v), want %q", text, err, want)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(decoded) != 1 {
			t.Fatalf("the room's load decoded %d states, want one", len(decoded))
		}
		return decoded[0]
	}

	held := preload(t)
	if served := loadRoom(t, "before"); &served[0] != &held[0] {
		t.Fatalf("the room loaded a %d-byte state, want the preloaded history itself", len(served))
	}
	if _, kept := service.preloads.Load(artifactID); kept {
		t.Fatal("a served preload stayed behind for a later load")
	}
	closeDocumentRoom(t, service, artifactID)

	held = preload(t)
	moved, err := encodeDocumentTree(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{
		Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "appended"}},
	}}})
	if err != nil {
		t.Fatalf("encode appended update: %v", err)
	}
	if _, err := service.persistence.AppendUpdate(context.Background(), artifactID, moved); err != nil {
		t.Fatalf("append update after the check: %v", err)
	}
	if served := loadRoom(t, "appended"); &served[0] == &held[0] {
		t.Fatal("the room loaded the stale preload")
	}
}

// A browser editor normalizes a tree it cannot represent and writes the result back, so the
// document websocket admits no connection to a room outside the Proof schema: resident or only
// stored, refused by the tree reader or only by the renderer. It completes the upgrade only to
// close with documentSchemaCloseCode before any sync, a code a browser can read, and neither loads
// nor fails the room.
func TestDocumentSocketRefusesARoomOutsideTheSchema(t *testing.T) {
	for _, test := range []struct {
		name    string
		corrupt func(*testing.T, *Service, string)
	}{
		{"a resident room the reader refuses", writeSchemaInvalidElement},
		{"a stored history only the renderer refuses", func(t *testing.T, service *Service, artifactID string) {
			if err := AppendRenderOnlySchemaViolationForTest(context.Background(), service.store, artifactID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, "before")
			test.corrupt(t, service, artifactID)
			httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
			t.Cleanup(httpServer.Close)
			headers := http.Header{"X-Dispatch-User": []string{"alice"}}
			wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
			connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
			if err != nil {
				t.Fatalf("connect outside-schema document: response=%#v err=%v, want an upgrade the server closes", response, err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			connection.SetReadDeadline(time.Now().Add(time.Second))
			_, message, err := connection.ReadMessage()
			var closed *gws.CloseError
			if !errors.As(err, &closed) || closed.Code != documentSchemaCloseCode {
				t.Fatalf("first frame = %q (%v), want close %d before any sync", message, err, documentSchemaCloseCode)
			}
			if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocOutsideSchema) {
				t.Fatalf("read after refused socket: %v, want ErrDocOutsideSchema", err)
			}
		})
	}
}

func TestShutdownClosesDocumentPeers(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store: database, Events: events.NewBroker(),
		Identity: headerIdentity(database),
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	defer connection.Close()
	waitFor(t, time.Second, "document peer connection", func() bool {
		state := service.room(artifactID)
		state.mu.Lock()
		defer state.mu.Unlock()
		return len(state.connected) == 1
	})
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(context.Background()) }()
	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
	if err := <-shutdown; err != nil {
		t.Fatalf("shutdown with document peer: %v", err)
	}
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "before\n" {
		t.Fatalf("text after closing document peer = %q (%v), want before", got, err)
	}
}

// Shutdown returns at its caller's deadline while an editor is connected and a durable append waits
// on a lock, and the append still reaches the store once the lock is released.
func TestShutdownReturnsAtItsDeadlineWithAnEditorConnectedAndAnAppendLocked(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store: database, Events: events.NewBroker(),
		Identity: headerIdentity(database),
		Settle:   time.Hour,
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	defer connection.Close()

	locker, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin append lock transaction: %v", err)
	}
	defer locker.Rollback(context.Background())
	if _, err := locker.Exec(context.Background(), `select pg_advisory_xact_lock(hashtext($1))`, artifactID); err != nil {
		t.Fatalf("lock document append: %v", err)
	}
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	if !service.hasDurableAppend(artifactID) {
		t.Fatal("durable append finished while its advisory lock was held")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := service.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown with an editor connected and an append locked = %v, want deadline exceeded", err)
	}
	if err := locker.Commit(context.Background()); err != nil {
		t.Fatalf("release append lock: %v", err)
	}
	// Not waitForPersistedUpdates: Shutdown's CloseRoom evicts this room once the append lands
	// (room.connected), and the eviction compacts the rows it can (compactRoom), so a row count is
	// not what says the append landed. The durable-append signal alone proves the locked write
	// reached the store; the content check below proves what it reached the store as.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Minute)
	defer waitCancel()
	if err := service.waitForDurableAppends(waitCtx, artifactID); err != nil {
		t.Fatalf("wait for the locked append to be stored: %v", err)
	}
	reloaded := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	defer reloaded.Shutdown(context.Background())
	if got, err := reloaded.Text(context.Background(), artifactID); err != nil || got != "after\n" {
		t.Fatalf("text after the shutdown the locked append outlasted = %q (%v), want after", got, err)
	}
}

func TestAppendFailureClosesDocumentConnectionAndReloadsRoom(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), appendErr: errors.New("append failed")},
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		Settle:      time.Hour,
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
	_ = connection.Close()
	waitForNoLiveDocument(t, service, artifactID)
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "before\n" {
		t.Fatalf("reloaded text after append failure = %q (%v), want persisted text before", got, err)
	}
}

func TestFailedRoomEvictsAndReloadsOnNextAccess(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: &failingOnceVersionedStore{VersionedStore: NewPgVersioned(database)},
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID

	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first failed-room access: response=%#v err=%v, want HTTP 503", response, err)
	}
	connection, response, err = gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("reloaded room access: response=%#v err=%v", response, err)
	}
	_ = connection.Close()
}

func TestWebsocketRejectsUnauthenticatedConnection(t *testing.T) {
	service, _ := newTestService(t)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/room"
	connection, response, err := gws.DefaultDialer.Dial(wsURL, nil)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil {
		t.Fatal("unauthenticated websocket connection succeeded")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated websocket response = %#v, want HTTP 401", response)
	}
}

func TestUnauthenticatedDocumentRequestsDoNotAllocateRooms(t *testing.T) {
	service, _ := newTestService(t)

	for number := range 1000 {
		response := httptest.NewRecorder()
		service.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/ws/doc/random-%d", number), nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("request %d status = %d, want %d", number, response.Code, http.StatusUnauthorized)
		}
	}

	rooms := 0
	service.rooms.Range(func(_, _ any) bool {
		rooms++
		return true
	})
	if rooms != 0 {
		t.Fatalf("unauthenticated requests left %d room states, want 0", rooms)
	}
}

// Answering a browser's SyncStep1 is not a read of the room's state and a send of it: ygo cuts
// what that browser is missing into frames each small enough for a browser to apply in one task
// and holds every one of them until they are queued (sync.EncodeSyncStep2Split), on the peer's
// read loop, and nothing in ygo bounds how many of those answers run together. Each answer takes
// a grant of the pod's copy heap, weighed by the path its room's answer takes (answerWeight),
// holds it until the answer has been sent, and gives it back.
//
// A room that parks nothing is cut from its own structs (crdt.SplitStateAsUpdateV1), which
// decodes nothing, so a browser joining it is answered under what that cut costs
// (cutAnswerWeight) rather than under a whole copy of the document.
func TestAnAnswerFromARoomThatParksNothingIsGrantedWhatItsCutCosts(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement takes a grant while the browser joins
	seedServiceText(t, service, artifactID, "One paragraph of ordinary prose.")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	awaitAnswer(t, connectPeer(t, httpServer.URL, artifactID, "alice"), "the document")
	live, replica := answeringRoom(t, service, artifactID)
	if parks(live) {
		t.Fatal("the room parks something, so its answer is cut from its encoded state")
	}

	granted := grantOfAJoin(t, httpServer.URL, artifactID)
	state, structs := replica.roomStateBytes(), int64(live.StructCount())
	if want := cutAnswerWeight(state, structs); granted != want {
		t.Fatalf("the answer to a room that parks nothing held %d bytes of the pod's copy heap, want the %d its cut costs; a room that parks is granted %d",
			granted, want, parkedAnswerWeight(state, structs))
	}
	t.Logf("an answer carrying %d structs of %d bytes of tracked state, cut from the room's structs, held %d bytes of the pod's copy heap",
		structs, state, granted)
}

// A room holding a parked struct is cut from its encoded state instead: the encode merges what
// the room parks into its state and the cut decodes the merge back (crdt.SplitUpdateV1), which
// decodes the whole document twice, so a browser joining it is answered under that path's grant
// (parkedAnswerWeight). The parked struct is a browser's keystroke sent before the keystroke it
// was typed after, as a browser whose earlier update was lost sends it.
func TestAnAnswerFromARoomHoldingAParkedStructIsGrantedWhatItsDecodesCost(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement takes a grant while the browser joins
	seedServiceText(t, service, artifactID, "One paragraph of ordinary prose.")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	writer := connectPeer(t, httpServer.URL, artifactID, "alice")
	awaitAnswer(t, writer, "the document")
	typist := crdt.New()
	if err := crdt.ApplyUpdateV1(typist, crdt.EncodeStateAsUpdateV1(writer.Doc, nil), nil); err != nil {
		t.Fatalf("copy the browser's document: %v", err)
	}
	docstest.Transact(typist, typeParagraph(typist, "first"))
	second := docstest.Transact(typist, typeParagraph(typist, "second"))
	if err := writer.Write(ygsync.EncodeUpdate(second)); err != nil {
		t.Fatalf("send the second keystroke: %v", err)
	}
	waitFor(t, 10*time.Second, "the room to park the second keystroke", func() bool {
		live := service.srv.GetDoc(artifactID)
		return live != nil && live.PendingStats().Items > 0
	})
	live, replica := answeringRoom(t, service, artifactID)

	granted := grantOfAJoin(t, httpServer.URL, artifactID)
	state, structs := replica.roomStateBytes(), int64(live.StructCount()+live.PendingStats().Items)
	if want := parkedAnswerWeight(state, structs); granted != want {
		t.Fatalf("the answer to a room holding a parked struct held %d bytes of the pod's copy heap, want the %d its decodes cost; a room that parks nothing is granted %d",
			granted, want, cutAnswerWeight(state, structs))
	}
	t.Logf("an answer carrying %d structs of %d bytes of tracked state, cut from the room's encoded state, held %d bytes of the pod's copy heap",
		structs, state, granted)
}

// answeringRoom is artifactID's resident room and the replica its load listed it with, which
// tracks the state bytes an answer to it is weighed by.
func answeringRoom(t *testing.T, service *Service, artifactID string) (*crdt.Doc, *renderedReplica) {
	t.Helper()
	live := service.srv.GetDoc(artifactID)
	replica := service.replicaOf(live)
	if live == nil || replica == nil {
		t.Fatal("the document's room is not resident with a browser connected")
	}
	return live, replica
}

// grantOfAJoin is the bytes of the pod's copy heap the answer to one more browser's first sync
// held, read once that browser has its answer and the grant has been given back. The pool it
// joins under has a whole slot free, far above a test document's answer, so nothing waits, and
// the answer is the only taker: the room is resident, so the socket's admission takes nothing,
// and the browser's own empty document sends an update far under unslottedApplyBytes.
func grantOfAJoin(t *testing.T, serverURL, artifactID string) int64 {
	t.Helper()
	pool, outstanding := loadSlotsWithRoomFor(t, 2, loadSlotBytes)
	var taken slotsHeld
	pool.taken = taken.record
	awaitAnswer(t, connectPeer(t, serverURL, artifactID, "bob"), "the document")
	waitFor(t, 10*time.Second, "the answer to give its grant back", func() bool {
		return pool.held.Load() == outstanding
	})
	grants := taken.all()
	if len(grants) == 0 {
		t.Fatal("the room answered the browser holding nothing of the pod's copy heap")
	}
	return slices.Max(grants) - outstanding
}

// A browser whose answer the pool has no room for inside the server's wait bound is closed
// DOC_BUSY, the transient close it comes back from, rather than left waiting on a sync that is
// not coming; and the browser already in the room is untouched by that refusal.
func TestABrowserWhoseAnswerThePodsCopyHeapHasNoRoomForIsClosedBusy(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "One paragraph of ordinary prose.")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	reader := connectPeer(t, httpServer.URL, artifactID, "alice")
	awaitAnswer(t, reader, "the document")

	// With the whole pool held by somebody else, the browser that joins is closed rather than
	// left waiting: the answer cannot be built without the bytes.
	useLoadSlots(t, newLoadSlots(1))
	filler := newLoadSlot()
	if !filler.tryTake(loadSlotBytes) {
		t.Fatal("the empty pool refused its one slot")
	}
	t.Cleanup(filler.release)
	started := time.Now()
	refused := connectPeer(t, httpServer.URL, artifactID, "bob")
	select {
	case <-refused.Ended:
	case <-refused.Answers:
		t.Fatal("the room answered a browser's first sync with no room in the pod's copy heap for the answer")
	case <-time.After(WriteWaitBound + time.Minute):
		t.Fatal("the room neither answered nor closed the browser whose answer the copy heap had no room for")
	}
	waited := time.Since(started)
	if closed := refused.Closed(); closed == nil || closed.Code != contracts.DocumentBusyCloseCode || closed.Text != contracts.DocumentBusyCloseReason {
		t.Fatalf("the room ended the browser with %v, want %d %s", closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
	}
	if waited+time.Second < WriteWaitBound {
		t.Fatalf("the browser was closed after %s, want it to have waited the server's %s bound for the pool", waited, WriteWaitBound)
	}
	t.Logf("a browser whose answer the full pool could not admit was closed %d %s after %s",
		contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason, waited.Round(time.Millisecond))

	// The browser already in the room is untouched by the refusal of another's answer.
	select {
	case <-reader.Ended:
		t.Fatalf("the room ended the browser already in it: %v", reader.Closed())
	default:
	}
}
