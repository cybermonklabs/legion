package docs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// TestADocumentSocketAsksItsRuleBeforeEachBatchOfFrames: a document socket asks whether its person
// still reads its room before each batch of the room's frames goes out (recheckedConn), as the
// live agent view asks before each batch of a session's frames, so a change that announces nothing
// - a session's new tie or pair for a session's document, here an SQL statement alone - ends the
// socket before the room's next frame reaches the person, not only on the access heartbeat. carol,
// a full member no list names, and alice, DOC's owner, hold sockets to public DOC's document; no
// access heartbeat and no room sync run here, so nothing else can see the change. DOC is then made
// private by SQL alone, and alice types: carol's socket closes with the code a revoked admission
// gets, and her copy never holds alice's edit; alice, who still reads DOC, keeps her socket.
//
// Control: the connection asking nothing before a batch (revocableSocketWriter hands ygo the
// connection as it is); carol's copy holds "after" and her socket stays open.
func TestADocumentSocketAsksItsRuleBeforeEachBatchOfFrames(t *testing.T) {
	const carol = "carol@example.com"
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)

	losing := connectPeer(t, httpServer.URL, artifactID, carol)
	if err := losing.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
		t.Fatalf("carol reads public DOC's document: %v", err)
	}
	editor := connectPeer(t, httpServer.URL, artifactID, "alice")
	if err := editor.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
		t.Fatalf("alice, DOC's owner, reads its document: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `update projects set visibility = 'private' where key = 'DOC'`); err != nil {
		t.Fatalf("make DOC private by SQL alone: %v", err)
	}
	typeOver(t, editor, "before", "after")
	if err := editor.WaitForText(peerMarkdown, "after\n", roomWait(t)); err != nil {
		t.Fatalf("alice's own copy holds her edit: %v", err)
	}
	select {
	case <-losing.Ended:
	case <-editor.Ended:
		t.Fatalf("alice's document socket closed (%d) while she still reads DOC", editor.CloseCode())
	case <-time.After(roomWait(t)):
		t.Fatalf("carol's document socket was still open after DOC was made private and alice typed: the room's frames went out unasked")
	}
	if code := losing.CloseCode(); code != contracts.DocumentAdmissionRevokedCloseCode {
		t.Errorf("carol's document socket closed with %d, want %d, as a revoked admission closes", code, contracts.DocumentAdmissionRevokedCloseCode)
	}
	if text := peerMarkdown(losing); strings.Contains(text, "after") {
		t.Errorf("carol's copy holds %q once DOC was private: a frame of the room reached her after she lost it", text)
	}
}
