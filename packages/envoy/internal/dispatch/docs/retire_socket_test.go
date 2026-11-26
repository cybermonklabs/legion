package docs

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A socket to a retired document's room is refused 409 DOCUMENT_RETIRED before anything reserves a
// room for it (ServeHTTP): the retirement closed every room of the document, and a browser that
// comes back must not open one that takes its typing. The same socket connects before the
// retirement.
func TestASocketToARetiredDocumentIsRefused(t *testing.T) {
	service, artifactID := newTestService(t)
	server := newTestServer(t, service)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID
	header := http.Header{"X-Dispatch-User": []string{"alice"}}
	socket, response, err := gws.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("connect a socket to document %s before its retirement: response=%#v err=%v", artifactID, response, err)
	}
	_ = socket.Close()

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retirement: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := RetireDocument(ctx, tx, artifactID, model.Actor{Kind: "user", ID: "alice"}, "The test retires it."); err != nil {
		t.Fatalf("retire the document: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}

	socket, response, err = gws.DefaultDialer.Dial(url, header)
	if err == nil {
		_ = socket.Close()
		t.Fatalf("a socket to the retired document %s connected, want it refused", artifactID)
	}
	if response == nil {
		t.Fatalf("a socket to the retired document failed with no answer: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusConflict || !strings.Contains(string(body), "DOCUMENT_RETIRED") {
		t.Fatalf("a socket to the retired document answers %d %q, want 409 DOCUMENT_RETIRED", response.StatusCode, body)
	}
}

// A retirement that commits while a socket takes its room and loads the document, after the
// socket's first read found the document standing, refuses that socket too, 409 DOCUMENT_RETIRED
// and no room of the document loaded: the socket reads the retirement again once its room is
// reserved and it is held, the last step before ygo loads the room (ServeHTTP, refuseRetired).
// The retirement commits in that gap through the service's hook (afterSocketIssueRead, which runs
// once the room is reserved), and the close of the document's rooms its commit announces
// (documentRemoved) runs there too and finds no room to close, since ygo has loaded none yet. A
// socket that read the retirement only before it reserved its room would open a room of the
// retired document, which takes its typing and which nothing then closes.
func TestASocketWhoseDocumentIsRetiredAsItTakesItsRoomIsRefused(t *testing.T) {
	service, artifactID := newTestService(t)
	server := newTestServer(t, service)
	var retired atomic.Bool
	service.afterSocketIssueRead = func(room string) {
		if room != artifactID || retired.Swap(true) {
			return
		}
		ctx := context.Background()
		tx, err := service.store.Pool.Begin(ctx)
		if err != nil {
			t.Errorf("begin the retirement: %v", err)
			return
		}
		defer tx.Rollback(ctx)
		if _, err := RetireDocument(ctx, tx, artifactID, model.Actor{Kind: "user", ID: "alice"}, "The test retires it."); err != nil {
			t.Errorf("retire the document: %v", err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("commit the retirement: %v", err)
			return
		}
		service.documentRemoved(ctx, artifactID)
	}

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID
	socket, response, err := gws.DefaultDialer.Dial(url, http.Header{"X-Dispatch-User": []string{"alice"}})
	if !retired.Load() {
		t.Fatal("the socket's admission never reached the hook that retires its document, so nothing here is tested")
	}
	if err == nil {
		_ = socket.Close()
		t.Fatalf("a socket whose document %s was retired as it took its room connected, want it refused", artifactID)
	}
	if response == nil {
		t.Fatalf("a socket whose document was retired as it took its room failed with no answer: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusConflict || !strings.Contains(string(body), "DOCUMENT_RETIRED") {
		t.Fatalf("a socket whose document was retired as it took its room answers %d %q, want 409 DOCUMENT_RETIRED", response.StatusCode, body)
	}
	if service.srv.GetDoc(artifactID) != nil {
		t.Errorf("a room of the retired document %s is loaded once its socket was refused", artifactID)
	}
}

// A socket asking for a document of an issue room reads that document's retirement, not the
// room's: a retirement of the Verification document that commits while its socket takes the
// issue's room, after the socket's first read found it standing, refuses the socket 409
// DOCUMENT_RETIRED once it is held (ServeHTTP, refuseRetired), with no room of the issue loaded;
// a socket asking for it again is refused by the first read (refusedDocument). The room's tree
// artifact is not retired, so a socket that read the room's retirement would connect and type
// into a fragment the room's layout leaves out (roomLayoutSQL). A socket asking for the spec, a
// document of the same room that stands, connects and reads the room.
func TestASocketToARetiredDocumentOfAnIssueRoomIsRefusedAndTheSpecsConnects(t *testing.T) {
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	var retired atomic.Bool
	service.afterSocketIssueRead = func(read string) {
		if read != room.tree || retired.Swap(true) {
			return
		}
		ctx := context.Background()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Errorf("begin the retirement: %v", err)
			return
		}
		defer tx.Rollback(ctx)
		if _, err := RetireDocument(ctx, tx, room.verification, model.Actor{Kind: "user", ID: "alice"}, "The checks moved to the plan."); err != nil {
			t.Errorf("retire the Verification document: %v", err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("commit the retirement: %v", err)
			return
		}
		service.documentRemoved(ctx, room.verification)
		service.closeMovedRoom(ctx, room.tree)
	}

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + room.verification
	header := http.Header{"X-Dispatch-User": []string{"alice"}}
	refused := func(when string) {
		t.Helper()
		socket, response, err := gws.DefaultDialer.Dial(url, header)
		if err == nil {
			_ = socket.Close()
			t.Fatalf("%s, a socket to the retired Verification document %s of the issue room %s connected, want it refused", when, room.verification, room.tree)
		}
		if response == nil {
			t.Fatalf("%s, a socket to the retired Verification document failed with no answer: %v", when, err)
		}
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusConflict || !strings.Contains(string(body), "DOCUMENT_RETIRED") {
			t.Fatalf("%s, a socket to the retired Verification document answers %d %q, want 409 DOCUMENT_RETIRED", when, response.StatusCode, body)
		}
	}
	refused("retired as it took the issue's room")
	if !retired.Load() {
		t.Fatal("the socket's admission never reached the hook that retires its document, so nothing here is tested")
	}
	if service.srv.GetDoc(room.tree) != nil {
		t.Errorf("the issue room %s is loaded once the socket to its retired Verification document was refused", room.tree)
	}
	refused("asked for again once retired")

	peer := connectPeer(t, server.URL, room.spec, "alice")
	if err := peer.WaitForText(fragmentMarkdown(room.plan), planSeed, roomWait(t)); err != nil {
		t.Fatalf("a socket to the spec of the room whose Verification document was retired: %v", err)
	}
}
