package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// upgradedPair is a websocket connection a test server upgraded, and the client's end of it.
func upgradedPair(t *testing.T) (served, client *gws.Conn) {
	t.Helper()
	upgraded := make(chan *gws.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&gws.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		upgraded <- conn
	}))
	t.Cleanup(server.Close)
	client, response, err := dialLive(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial the test server: %v", err)
	}
	_ = response.Body.Close()
	t.Cleanup(func() { _ = client.Close() })
	served = testwait.Receive(t, "the server's end of the upgraded connection", upgraded)
	t.Cleanup(func() { _ = served.Close() })
	return served, client
}

// A read deadline renewed after its timer has already ended the reads replaces the deadline that
// timer set on the connection, as a SetReadDeadline replaces an expired one. A pong already in the
// read buffer when the deadline falls due renews it before the next network read (the pong
// handler's awaitPong), and that read must then wait for the page, not time out on the deadline the
// pong replaced. The socket's clock is a fake the test moves: the move to the deadline returns once
// the timer's function has ended the reads, so the renewal comes after it, as in that race.
func TestARenewedLiveSocketReadDeadlineLiftsTheOneItsTimerApplied(t *testing.T) {
	t.Parallel()
	served, client := upgradedPair(t)
	deadlines := clocktest.New(time.Now())
	socket := newLiveSocket(t.Context(), served, model.Actor{}, deadlines)
	t.Cleanup(socket.stopReadDeadline)

	const wait = time.Second
	socket.awaitPong(wait)
	deadlines.Advance(wait)
	socket.awaitPong(wait)

	if err := client.WriteMessage(gws.TextMessage, []byte("after the renewal")); err != nil {
		t.Fatalf("the client's write: %v", err)
	}
	type read struct {
		data []byte
		err  error
	}
	reads := make(chan read, 1)
	go func() {
		_, data, err := served.ReadMessage()
		reads <- read{data, err}
	}()
	got := testwait.Receive(t, "the server's read after the renewal", reads)
	if got.err != nil {
		t.Fatalf("the read after the renewed deadline failed (%v): the renewal left the deadline its timer had set on the connection, so a page that answered in time was cut off", got.err)
	}
	if string(got.data) != "after the renewal" {
		t.Fatalf("the read after the renewal got %q, want the client's message", got.data)
	}
}
