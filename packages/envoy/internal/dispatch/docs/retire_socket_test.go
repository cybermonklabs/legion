package docs

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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

// A retirement closes every room of the document through one announcement
// (AnnounceDocumentRemoved), which a process whose notification hub was between connections never
// hears. The removal lane's pass after its next LISTEN reads the rows of its rooms
// (recheckRemovedRooms) and closes the room of a retired document, whose row stays, as it closes
// one whose row has gone: a room left open would take a person's typing, which settlement stores
// and writes no version of. The service here runs no notification hub, so the announcement reaches
// it never, as it reaches no process whose listener was down.
func TestTheRecheckAfterAListenClosesTheRoomOfARetiredDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "Context\n")
	editLiveTree(t, service, artifactID, replaceRun("Context", "Context, typed in the browser"))
	waitForLandedAppends(t, service, artifactID)
	if !roomResident(service, artifactID) {
		t.Fatalf("the service holds the rooms %v, want one of %s for the retirement to close", service.ResidentRooms(), artifactID)
	}

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
	if !roomResident(service, artifactID) {
		t.Fatal("the retired document's room closed with no recheck, so this test would prove nothing")
	}

	service.recheckRemovedRooms(ctx)
	if roomResident(service, artifactID) {
		t.Fatalf("after the recheck the service still holds the rooms %v, want the retired document %s's room closed", service.ResidentRooms(), artifactID)
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
