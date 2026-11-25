package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// heldFiring is a socket's read-deadline clock: a fake on which the function of an AfterFunc, once
// the fake fires it, waits before it runs until the row lets it go (let). It stretches the span
// between a real timer's firing and the start of its function's goroutine, which the scheduler
// decides, so the row can move the deadline on inside it. fired closes once the first firing
// waits.
type heldFiring struct {
	*clocktest.Fake
	fired, released chan struct{}
	fire, release   sync.Once
}

func newHeldFiring(t *testing.T) *heldFiring {
	deadlines := &heldFiring{Fake: clocktest.New(drainStart), fired: make(chan struct{}), released: make(chan struct{})}
	// A row that fails while a firing waits lets it go, so the move that fired it returns.
	t.Cleanup(deadlines.let)
	return deadlines
}

func (c *heldFiring) AfterFunc(d time.Duration, f func()) clock.Timer {
	return c.Fake.AfterFunc(d, func() {
		c.fire.Do(func() { close(c.fired) })
		<-c.released
		f()
	})
}

// fireHeld moves the fake to at off the test goroutine (clocktest.Fake.StartMove), since a move
// waits for the function of each timer it fires to return, and waits until the firing of the timer
// due there waits. It answers the move's join, which waits for the move to return, and that
// function with it, and fails the test naming the move and the function it waits for when that
// function never returns.
func (c *heldFiring) fireHeld(t *testing.T, at time.Time) (join func(testing.TB)) {
	t.Helper()
	join = c.StartMove("the move that fired the read deadline due at "+at.Format(time.RFC3339Nano), at)
	testwait.Receive(t, "the read deadline's timer to fire at "+at.Format(time.RFC3339Nano)+" and its function to wait", c.fired)
	return join
}

func (c *heldFiring) let() { c.release.Do(func() { close(c.released) }) }

// The read-deadline timer's function runs on a goroutine of its own once the timer fires, so an
// awaitPong or a drain can move the deadline on between the firing and the function's turn, and
// Reset stops no firing already made. Such a firing ends no reads once its function runs
// (endReadsIfDue). Had it ended them whatever deadline the socket then held, it would cut short
// the drain of a frame past the read limit, bringing back the reset that overtakes the 1009 the
// drain exists to stop, and it would cut a socket whose page's pong had just moved its pong
// deadline on.
//
// The read deadlines run on a fake whose firing waits before its function runs until the row lets
// it go (heldFiring), and the pings on a fake no one moves, so no ping draws a pong the row did not
// send (countedRig). Each case moves the fake to the pong deadline the upgrade armed, so its timer
// fires and its function waits; it then moves the deadline on and waits until the new one is
// armed, and only then lets the firing go and waits for its function to return:
//   - the drain: the page sends a frame past the read limit, the socket closes with 1009, and the
//     drain arms its own deadline, liveSocketCloseWait on, and reads what the page sends next; the
//     page then sends the rest of the frame and its FIN, and the server must have read every byte
//     the page sent when it closes the connection;
//   - the pong: the page sends a pong, which arms the pong deadline two heartbeats on; the socket
//     must then still answer the page's next frame.
//
// Nothing in the row reads the wall clock but gorilla's own deadline on the drain's 1009 write
// (sendOverTheLimit).
func TestAReadDeadlineFiringThatAMoveOvertookEndsNoReads(t *testing.T) {
	pongBy := drainStart.Add(2 * drainHeartbeat)
	t.Run("the drain", func(t *testing.T) {
		deadlines := newHeldFiring(t)
		rig := newCountedRig(t, deadlines)
		socket := rig.openCounted(t)
		deadlines.Fake.AwaitArmedAt(t, "the pong deadline the upgrade armed", pongBy)
		joinMove := deadlines.fireHeld(t, pongBy)

		socket.sendOverTheLimit(t)
		deadlines.Fake.AwaitArmedAt(t, "the drain's own deadline, liveSocketCloseWait on", pongBy.Add(liveSocketCloseWait))
		socket.drainReading(t)
		deadlines.let()
		joinMove(t)
		socket.endFrame(t, "the pong deadline's firing, overtaken by the drain, ended the drain before the frame's end")
		rig.sockets(t, 0)
	})
	t.Run("the pong", func(t *testing.T) {
		deadlines := newHeldFiring(t)
		rig := newCountedRig(t, deadlines)
		client := rig.openCounted(t).client
		deadlines.Fake.AwaitArmedAt(t, "the pong deadline the upgrade armed", pongBy)
		joinMove := deadlines.fireHeld(t, pongBy)

		// The pong's write ends where the shared wait gives up (none under -timeout 0).
		writeBy, _ := testwait.GiveUpAt(t)
		if err := client.conn.WriteControl(gws.PongMessage, nil, writeBy); err != nil {
			t.Fatalf("send a pong: %v", err)
		}
		deadlines.Fake.AwaitArmedAt(t, "the pong deadline the page's pong armed, two heartbeats on", pongBy.Add(2*drainHeartbeat))
		deadlines.let()
		joinMove(t)
		client.send(t, LiveUnsubscribe{Type: "unsubscribe"})
		frame := client.next(t, "the socket's answer to the page's next frame: the pong deadline's firing, overtaken by the pong, must not have cut it")
		if refusal, ok := frame.(LiveError); !ok || refusal.Code != "INVALID_FRAME" {
			t.Fatalf("the socket answered %+v to an unsubscribe without a sub, want INVALID_FRAME", frame)
		}
		rig.sockets(t, 1)
	})
}

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
// pong replaced. The socket's clock is a fake the test moves once the deadline is armed on it: the
// move to the deadline returns once the timer's function has ended the reads, so the renewal comes
// after it, as in that race.
func TestARenewedLiveSocketReadDeadlineLiftsTheOneItsTimerApplied(t *testing.T) {
	t.Parallel()
	served, client := upgradedPair(t)
	deadlines := clocktest.New(drainStart)
	socket := newLiveSocket(t.Context(), served, nil, deadlines)
	t.Cleanup(socket.stopReadDeadline)

	const wait = time.Second
	socket.awaitPong(wait)
	deadlines.AwaitArmedAt(t, "the read deadline the first awaitPong armed", drainStart.Add(wait))
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
