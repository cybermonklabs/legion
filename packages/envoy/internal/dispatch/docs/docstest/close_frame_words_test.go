package docstest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// echoedCloseCode is the code of the close frame echoingCloseRoom sends.
const echoedCloseCode = 4000

// echoingCloseRoom serves, until t ends, a stand-in room that upgrades every peer, reads its first
// frame (DialContext's sync step 1, so that write never meets a close the peer has answered), and
// then closes the connection with a close frame of echoedCloseCode whose text is the Authorization
// the upgrade carried, as a room that quotes the request in its reason may. It reads on until the
// peer answers the close. It answers the room's ws:// URL.
func echoingCloseRoom(t *testing.T) string {
	t.Helper()
	var upgrader gws.Upgrader
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		if _, _, err := connection.ReadMessage(); err != nil {
			return
		}
		frame := gws.FormatCloseMessage(echoedCloseCode, r.Header.Get("Authorization"))
		if err := connection.WriteControl(gws.CloseMessage, frame, time.Now().Add(time.Minute)); err != nil {
			return
		}
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/art-1"
}

// TestAPeersLastReadPrintsNoWordOfTheRoomsCloseFrame: a room closes a peer's connection with a
// close frame whose text echoes the bearer the dial's header carried. gorilla's error of that read
// quotes the frame's text, bearer and all (the twin); the error DroppedWithoutCloseFrame returns
// for diagnostics, which the document socket tests print beside the frame's code, names the
// failure by its class and still unwraps to gorilla's close error. Captures
// docstest.close-frame-words/last-read.
func TestAPeersLastReadPrintsNoWordOfTheRoomsCloseFrame(t *testing.T) {
	room := echoingCloseRoom(t)
	header := http.Header{"Authorization": {"Bearer " + redacttest.Token}}
	t.Run("twin", func(t *testing.T) {
		connection, _, err := gws.DefaultDialer.Dial(room, header)
		if err != nil {
			t.Fatalf("dial the room: %v", err)
		}
		defer connection.Close()
		if err := connection.WriteMessage(gws.BinaryMessage, []byte{0}); err != nil {
			t.Fatalf("write the first frame: %v", err)
		}
		_, _, err = connection.ReadMessage()
		if !gws.IsCloseError(err, echoedCloseCode) || !strings.Contains(err.Error(), redacttest.Token) {
			t.Fatalf("gorilla's error of the room's close frame is %v, want a close of %d that quotes the bearer: the row cannot see one printed", err, echoedCloseCode)
		}
	})
	t.Run("last-read", func(t *testing.T) {
		peer, err := DialContext(context.Background(), room, header, "art-1", crdt.New())
		if err != nil {
			t.Fatalf("connect a peer to the room: %v", err)
		}
		defer peer.Close()
		dropped, last := peer.DroppedWithoutCloseFrame()
		if code := peer.CloseFrame(); dropped || code != echoedCloseCode {
			t.Fatalf("the peer's read ended with a close frame of %d, dropped %v; want the room's frame of %d, not dropped", code, dropped, echoedCloseCode)
		}
		text := fmt.Sprintf("its last read: %v", last)
		redacttest.RequireNoPiece(t, "docstest.close-frame-words/last-read", text, header.Get("Authorization"))
		if want := "its last read: " + redact.ClassRequestFailed; text != want {
			t.Errorf("DroppedWithoutCloseFrame's error prints %q, want %q", text, want)
		}
		var closed *gws.CloseError
		if !errors.As(last, &closed) || closed.Code != echoedCloseCode {
			t.Errorf("DroppedWithoutCloseFrame's error reaches no close error of %d, want it to unwrap to the read's own", echoedCloseCode)
		}
	})
}
