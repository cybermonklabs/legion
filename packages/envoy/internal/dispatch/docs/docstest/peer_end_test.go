package docstest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
)

// stubRoom is a websocket server that reads the peer's first frame, its sync step 1, and then
// does what act says with the connection.
func stubRoom(t *testing.T, act func(*gws.Conn)) string {
	t.Helper()
	upgrader := gws.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer connection.Close()
		if _, _, err := connection.ReadMessage(); err != nil {
			t.Errorf("read the peer's sync step 1: %v", err)
			return
		}
		act(connection)
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// A frame the reader cannot apply stops it with the socket still open, and the peer says so: a
// test that sees Ended must not read that as a connection that ended.
func TestAReaderThatCannotApplyAFrameGivesUpWithItsSocketOpen(t *testing.T) {
	t.Parallel()
	received := make(chan []byte, 1)
	url := stubRoom(t, func(connection *gws.Conn) {
		frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
			encoder.WriteVarString("doc")
			encoder.WriteVarUint(messageSync)
			encoder.WriteRaw([]byte{0xff, 0xff, 0xff}) // no sync message decodes from this
		})
		if err := connection.WriteMessage(gws.BinaryMessage, frame); err != nil {
			t.Errorf("send the frame: %v", err)
			return
		}
		connection.SetReadDeadline(time.Now().Add(time.Minute))
		if _, message, err := connection.ReadMessage(); err == nil {
			received <- message
		}
	})
	peer := Dial(t, url, nil, "doc", crdt.New())
	<-peer.Ended
	gaveUp, err := peer.ReaderGaveUp()
	if !gaveUp || err == nil || !strings.Contains(err.Error(), "sync frame it could not apply") {
		t.Fatalf("ReaderGaveUp() = %t, %v; want true and the sync frame it could not apply", gaveUp, err)
	}
	if code := peer.CloseCode(); code != 0 {
		t.Errorf("CloseCode() = %d, want 0: no connection ended", code)
	}
	if dropped, _ := peer.DroppedWithoutCloseFrame(); dropped {
		t.Error("DroppedWithoutCloseFrame() = true for a socket that is still open")
	}
	if err := peer.AskForDocument(); err != nil {
		t.Fatalf("write on the socket the reader gave up on: %v", err)
	}
	select {
	case <-received:
	case <-time.After(time.Minute):
		t.Fatal("the room never received the peer's write: the socket was not open")
	}
}

// A room that drops the connection ends it: the reader did not give up, and the drop is named.
func TestAConnectionTheRoomDropsIsNoReaderThatGaveUp(t *testing.T) {
	t.Parallel()
	url := stubRoom(t, func(connection *gws.Conn) {
		connection.UnderlyingConn().Close() // no close frame
	})
	peer := Dial(t, url, nil, "doc", crdt.New())
	<-peer.Ended
	if gaveUp, err := peer.ReaderGaveUp(); gaveUp {
		t.Fatalf("ReaderGaveUp() = true (%v) for a connection the room dropped", err)
	}
	if dropped, err := peer.DroppedWithoutCloseFrame(); !dropped {
		t.Fatalf("DroppedWithoutCloseFrame() = false (%v), want true", err)
	}
}
