package docs

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

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
