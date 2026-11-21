package main

// relay_socket_close_drop_test.go: a tester's red row for #4658 at 8bd6ede5e7. A socket the
// listener closes (4401, 4403, 1013, a binary frame's 1003) writes its close frame, and once the
// client has answered it the listener drops the connection: end's own comment says so ("drops the
// connection once the client has answered it or relaySocketCloseGrace has passed"). A Bun
// WebSocket, the TypeScript client's, fires `close` only when the TCP connection ends, so a
// connection held to the end of the grace delays every reopen by the grace, a 4401's "at once"
// included. At 6808b36ea3 the reader dropped the connection after the client's answer
// (`socket.end(0, "")` then `conn.Close()`); at 8bd6ede5e7 end(0) drops it only when end has not
// run yet, and after a close the listener began it already has, so the connection waits out the
// grace.

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestARelaySocketTheListenerClosedDropsItsConnectionOnceTheClientAnswers(t *testing.T) {
	rig := newRelaySocketRig(t)
	client, _ := rig.open(t, "ses_edge_close_drop")
	// A binary frame closes the socket 1003 text_frames_only at once: a close the listener
	// begins, as a 4401 at a token's expiry is.
	client.writeMu.Lock()
	err := client.conn.WriteMessage(websocket.BinaryMessage, []byte(`{"op":"unsub","sid":1}`))
	client.writeMu.Unlock()
	if err != nil {
		t.Fatalf("send a binary frame: %v", err)
	}
	// gorilla's client answers a close frame with its own before the read returns it.
	if code, text := client.closeCode(5 * time.Second); code != websocket.CloseUnsupportedData || text != relaySocketReasonTextOnly {
		t.Fatalf("the socket closed %d %q, want %d %q", code, text, websocket.CloseUnsupportedData, relaySocketReasonTextOnly)
	}
	answered := time.Now()
	raw := client.conn.UnderlyingConn()
	_ = raw.SetReadDeadline(time.Now().Add(3 * time.Second))
	buffer := make([]byte, 256)
	for {
		if _, err = raw.Read(buffer); err != nil {
			break
		}
	}
	took := time.Since(answered)
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("the listener had not dropped the connection 3 s after the client answered its close")
	}
	if want := relaySocketCloseGrace / 4; took > want {
		t.Fatalf("the listener dropped the connection %s after the client answered its close, want within %s: a Bun client reads the close only once the connection ends, so it opens again that much later (last read: %v)", took.Round(time.Millisecond), want, err)
	}
	t.Logf("the listener dropped the connection %s after the client answered its close (%v)", took.Round(time.Millisecond), err)
}
