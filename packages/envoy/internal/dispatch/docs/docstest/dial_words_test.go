package docstest

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// unreadableUpgrade serves, until t ends, a stand-in that answers every upgrade with a status line
// whose version is redacttest.Code, a URL that carries the probe's credentials, which gorilla cannot
// read and quotes in its error (it reads the answer with http.ReadResponse, net/http/response.go of
// go1.27.3, lines 187-189). It answers the room's ws:// URL.
func unreadableUpgrade(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
					_, _ = io.WriteString(conn, redacttest.Code+" 101 Switching Protocols\r\n\r\n")
				}
			}()
		}
	}()
	return "ws://" + listener.Addr().String() + "/ws/doc/art-1"
}

// TestADialPrintsNoWordOfAStatusLineItCouldNotRead: whatever answers at a room's address answers a
// peer's upgrade with a status line gorilla cannot read, which quotes a URL with the probe's
// credentials. gorilla's own error quotes it (the twin); DialContext's error, which a load driver
// prints (loadtest/browsertype), names the failure by its class. Captures
// docstest.dial-words/status-line.
func TestADialPrintsNoWordOfAStatusLineItCouldNotRead(t *testing.T) {
	room := unreadableUpgrade(t)
	t.Run("twin", func(t *testing.T) {
		conn, _, err := gws.DefaultDialer.Dial(room, nil)
		if err == nil {
			_ = conn.Close()
		}
		if err == nil || !strings.Contains(err.Error(), redacttest.Token) {
			t.Fatalf("gorilla's own error holds no word of the status line, so the row cannot see one: %v", err)
		}
	})
	t.Run("status-line", func(t *testing.T) {
		peer, err := DialContext(context.Background(), room, nil, "art-1", crdt.New())
		if err == nil {
			peer.Close()
			t.Fatal("DialContext connected to a room that answered no status line it can read")
		}
		redacttest.RequireNone(t, "docstest.dial-words/status-line", err.Error())
		if want := "connect a peer to document art-1: " + redact.ClassRequestFailed; err.Error() != want {
			t.Errorf("DialContext = %q, want %q", err, want)
		}
	})
}
