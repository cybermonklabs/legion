package docstest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
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
	testcleanup.Add(t, server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// A frame the reader cannot apply stops it with the socket still open, and the peer says so: a
// test that sees Ended must not read that as a connection that ended.
func TestAReaderThatCannotApplyAFrameGivesUpWithItsSocketOpen(t *testing.T) {
	t.Parallel()
	// The stub room's read of the peer's next frame, whatever it ends in: a read that fails sends
	// its error, so the wait below ends either way and the failure is named.
	type read struct {
		message []byte
		err     error
	}
	received := make(chan read, 1)
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
		// No deadline of its own: the read ends on the peer's write, or on the peer's close at
		// the test's end.
		_, message, err := connection.ReadMessage()
		received <- read{message: message, err: err}
	})
	peer := Dial(t, url, nil, "doc", crdt.New())
	testwait.Receive(t, "the peer's reader to stop on the frame it cannot apply", peer.Ended)
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
	got := testwait.Receive(t, "the room's read of the peer's write on the socket the reader gave up on", received)
	if got.err != nil {
		t.Fatalf("the room's read of the peer's write returned an error: %v", got.err)
	}
	// AskForDocument sends a sync step 1 naming no state, in the peer's Hocuspocus frame.
	want := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString("doc")
		encoder.WriteVarUint(messageSync)
		encoder.WriteRaw(ygsync.EncodeSyncStep1(crdt.New()))
	})
	if !bytes.Equal(got.message, want) {
		t.Fatalf("the room read %x, want the peer's sync step 1 frame %x", got.message, want)
	}
}

// A room that drops the connection ends it: the reader did not give up, and the drop is named.
func TestAConnectionTheRoomDropsIsNoReaderThatGaveUp(t *testing.T) {
	t.Parallel()
	url := stubRoom(t, func(connection *gws.Conn) {
		connection.UnderlyingConn().Close() // no close frame
	})
	peer := Dial(t, url, nil, "doc", crdt.New())
	testwait.Receive(t, "the peer's reader to end on the dropped connection", peer.Ended)
	if gaveUp, err := peer.ReaderGaveUp(); gaveUp {
		t.Fatalf("ReaderGaveUp() = true (%v) for a connection the room dropped", err)
	}
	if dropped, err := peer.DroppedWithoutCloseFrame(); !dropped {
		t.Fatalf("DroppedWithoutCloseFrame() = false (%v), want true", err)
	}
}

// giveUpChildEnv tells a copy of this test binary that the row it runs is the child that reads how
// a form's wait gives up (requireGiveUp): the row's own test runs the child's part when it is set.
const giveUpChildEnv = "DOCSTEST_GIVE_UP_CHILD"

// giveUpChildGuard is the -timeout of a give-up child (testwait.RunChild): a hang guard only,
// which a child whose wait never gives up runs into, and never what decides the row. A child's
// whole run, its start included, measured 0.01 to 0.06 s and 0.08 to 0.13 s under -race (the four
// rows' "fails by name" subtests in an issue pod of 2 CPUs); two minutes is over nine hundred
// times the slowest, and a setup a loaded machine holds up to 90 s, where the child's waits give
// up, still reaches its step.
const giveUpChildGuard = 2 * time.Minute

// requireGiveUp runs test, the row's own top-level test, as a child of t (testwait.RunChild) with
// giveUpChildEnv set, and requires the child to fail by name: its wait gives up at the shared
// wait's give-up, which the child takes as a step of its own once its setup is done
// (testwait.SetUpDone), writing "testwait: <test>: gave up waiting for <gaveUp>" as a line of its
// own, and its test fails, with no panic.
func requireGiveUp(t *testing.T, test, gaveUp string) {
	t.Helper()
	stdout, stderr, err := testwait.RunChild(t, os.Args[0], giveUpChildGuard, "^"+test+"$", giveUpChildEnv+"=1")
	text := stdout + stderr
	if err == nil {
		t.Fatalf("the child was meant to fail its row; it passed:\n%s", text)
	}
	if strings.Contains(text, "panic:") {
		t.Fatalf("the child panicked rather than failing its row by name:\n%s", text)
	}
	for _, want := range []string{"testwait: " + test + ": gave up waiting for " + gaveUp + "\n", "--- FAIL: " + test + " ("} {
		if !strings.Contains(text, want) {
			t.Errorf("the child's output does not say %q:\n%s", want, text)
		}
	}
}

// roomFrame is a Hocuspocus frame the room sends on document "doc": its name, kind, then what body
// writes.
func roomFrame(kind uint64, body func(*encoding.Encoder)) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString("doc")
		encoder.WriteVarUint(kind)
		body(encoder)
	})
}

// readUntilEnd reads connection until a read fails: on the peer's close at the test's end, or on
// the room's own close.
func readUntilEnd(connection *gws.Conn) {
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			return
		}
	}
}

// bodyText renders the peer's copy of the document: its "body" text, which textRoom writes.
func bodyText(p *Peer) string {
	return p.Doc.GetText("body").ToString()
}

// textRoom is a stub room's act (stubRoom) that answers the sync step 1s the peer sends after its
// first, one for each of bodies in turn, with a sync step 2 of a document whose "body" text is that
// body, and answers none once bodies are spent. It reads until the connection ends.
func textRoom(bodies ...string) func(*gws.Conn) {
	return func(connection *gws.Conn) {
		for {
			_, frame, err := connection.ReadMessage()
			if err != nil {
				return
			}
			step1, asked := syncStep1(frame)
			if !asked || len(bodies) == 0 {
				continue
			}
			document := crdt.New()
			if bodies[0] != "" {
				// The handle is resolved outside the transaction: GetText takes the lock Transact
				// holds.
				body := document.GetText("body")
				document.Transact(func(txn *crdt.Transaction) { body.Insert(txn, 0, bodies[0], nil) })
			}
			bodies = bodies[1:]
			step2, err := ygsync.EncodeSyncStep2(document, step1)
			if err != nil {
				return
			}
			if err := connection.WriteMessage(gws.BinaryMessage, roomFrame(messageSync, func(encoder *encoding.Encoder) { encoder.WriteRaw(step2) })); err != nil {
				return
			}
		}
	}
}

// syncStep1 is the sync step 1 a frame the peer sent carries, and whether it carries one.
func syncStep1(frame []byte) ([]byte, bool) {
	decoder := encoding.NewDecoder(frame)
	if _, err := decoder.ReadVarString(); err != nil {
		return nil, false
	}
	if kind, err := decoder.ReadVarUint(); err != nil || kind != messageSync {
		return nil, false
	}
	message := decoder.RemainingBytes()
	kind, _, err := ygsync.ReadSyncMessage(message)
	return message, err == nil && kind == ygsync.MsgSyncStep1
}

// AwaitText asks the room for the document until the peer's copy renders the text it waits for,
// asking again each time the room answers: the room answers the first ask with a document with no
// text and only the second with the text, so a form that ends at the first answer returns a copy
// without it. A connection the room drops first ends the wait with an error naming how it ended,
// not with the give-up. A text the room never sends fails the row by name: the child asks the room
// once in its setup and has its answer, takes the give-up as its next step (testwait.SetUpDone),
// and its wait gives up at its first question, naming the text and the copy's last render.
func TestAwaitTextWaitsForTheRoomsTextAndFailsByName(t *testing.T) {
	t.Parallel()
	if os.Getenv(giveUpChildEnv) != "" {
		peer := Dial(t, stubRoom(t, textRoom("hi")), nil, "doc", crdt.New())
		if err := peer.AskForDocument(); err != nil {
			t.Fatalf("ask the room for the document: %v", err)
		}
		testwait.Receive(t, "the room's answer to the setup's ask", peer.Answers)
		testwait.SetUpDone(t)
		err := peer.AwaitText(t, bodyText, "hello")
		t.Fatalf("AwaitText returned (%v) for a text the room never sends", err)
	}
	test := t.Name()
	t.Run("waits for its fact", func(t *testing.T) {
		peer := Dial(t, stubRoom(t, textRoom("", "hello")), nil, "doc", crdt.New())
		if err := peer.AwaitText(t, bodyText, "hello"); err != nil {
			t.Fatalf("AwaitText: %v", err)
		}
		if got := bodyText(peer); got != "hello" {
			t.Fatalf("AwaitText returned with the copy rendering %q, want %q", got, "hello")
		}
	})
	t.Run("ends with the connection", func(t *testing.T) {
		url := stubRoom(t, func(connection *gws.Conn) {
			connection.UnderlyingConn().Close() // no close frame
		})
		peer := Dial(t, url, nil, "doc", crdt.New())
		testwait.Receive(t, "the peer's reader to end on the dropped connection", peer.Ended)
		err := peer.AwaitText(t, bodyText, "hello")
		if err == nil || !strings.HasPrefix(err.Error(), "the peer's connection ended (close frame 0, ") ||
			!strings.HasSuffix(err.Error(), `) before the room sent the document: its copy renders "", want "hello"`) {
			t.Fatalf("AwaitText on a connection the room dropped = %v, want the error naming the connection's end and the copy's render", err)
		}
	})
	t.Run("fails by name", func(t *testing.T) {
		requireGiveUp(t, test, `the peer's copy of the document to render "hello" 30s before the test binary's deadline; last read: hi`)
	})
}

// The awareness states a row's room sends of client 7.
const (
	notTyping = `{"typing":false}`
	typing    = `{"typing":true}`
)

// awarenessFrame is the room's awareness update of one client, as y-protocols encodes it.
func awarenessFrame(client, clock uint64, state string) []byte {
	update := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(1)
		encoder.WriteVarUint(client)
		encoder.WriteVarUint(clock)
		encoder.WriteVarString(state)
	})
	return roomFrame(messageAwareness, func(encoder *encoding.Encoder) { encoder.WriteVarBytes(update) })
}

// clientTyping holds when the room has sent client 7 as typing.
func clientTyping(states map[uint64]json.RawMessage) bool {
	return string(states[7]) == typing
}

// AwaitAwareness waits until the awareness states the room sent hold what it waits for, asking
// again after they do not: the room sends client 7 not typing, and sends it typing only once the
// row's condition has seen it not typing, so a form that ends at its first answer returns states
// that do not hold. A reader that gives up first, on an awareness frame it cannot decode, ends the
// wait with an error naming that and the states it holds, not with the give-up. States that never
// hold fail the row by name: the child has the room's one state in its setup, takes the give-up as
// its next step (testwait.SetUpDone), and its wait gives up at its first question, naming the
// states it last read.
func TestAwaitAwarenessWaitsForTheStatesToHoldAndFailsByName(t *testing.T) {
	t.Parallel()
	if os.Getenv(giveUpChildEnv) != "" {
		url := stubRoom(t, func(connection *gws.Conn) {
			if connection.WriteMessage(gws.BinaryMessage, awarenessFrame(7, 1, notTyping)) == nil {
				readUntilEnd(connection)
			}
		})
		peer := Dial(t, url, nil, "doc", crdt.New())
		testwait.Receive(t, "the room's awareness state", peer.Awareness)
		testwait.SetUpDone(t)
		err := peer.AwaitAwareness(t, clientTyping)
		t.Fatalf("AwaitAwareness returned (%v) for states that never hold", err)
	}
	test := t.Name()
	t.Run("waits for its fact", func(t *testing.T) {
		seen := make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(seen) }) }
		url := stubRoom(t, func(connection *gws.Conn) {
			if connection.WriteMessage(gws.BinaryMessage, awarenessFrame(7, 1, notTyping)) != nil {
				return
			}
			<-seen
			if connection.WriteMessage(gws.BinaryMessage, awarenessFrame(7, 2, typing)) == nil {
				readUntilEnd(connection)
			}
		})
		// A row that fails before its condition saw client 7 not typing still lets the room go.
		testcleanup.Add(t, release)
		peer := Dial(t, url, nil, "doc", crdt.New())
		err := peer.AwaitAwareness(t, func(states map[uint64]json.RawMessage) bool {
			if string(states[7]) == notTyping {
				release()
			}
			return clientTyping(states)
		})
		if err != nil {
			t.Fatalf("AwaitAwareness: %v", err)
		}
		if got := string(peer.AwarenessStates()[7]); got != typing {
			t.Fatalf("AwaitAwareness returned with client 7's state %s, want %s", got, typing)
		}
	})
	t.Run("ends with the connection", func(t *testing.T) {
		url := stubRoom(t, func(connection *gws.Conn) {
			if connection.WriteMessage(gws.BinaryMessage, awarenessFrame(7, 1, notTyping)) != nil {
				return
			}
			// An awareness frame with no update, which the reader cannot decode.
			if connection.WriteMessage(gws.BinaryMessage, roomFrame(messageAwareness, func(*encoding.Encoder) {})) == nil {
				readUntilEnd(connection)
			}
		})
		peer := Dial(t, url, nil, "doc", crdt.New())
		testwait.Receive(t, "the peer's reader to stop on the awareness frame it cannot decode", peer.Ended)
		err := peer.AwaitAwareness(t, clientTyping)
		if err == nil || !strings.HasPrefix(err.Error(), "the peer's reader gave up with its socket still open (the reader stopped on an awareness frame with no update: ") ||
			!strings.HasSuffix(err.Error(), `) before its awareness states held what was awaited; they hold map[7:{"typing":false}]`) {
			t.Fatalf("AwaitAwareness on a reader that gave up = %v, want the error naming the reader's stop and the states it holds", err)
		}
	})
	t.Run("fails by name", func(t *testing.T) {
		requireGiveUp(t, test, `the awareness states the room sent the peer to hold what was awaited 30s before the test binary's deadline; last read: map[7:{"typing":false}]`)
	})
}

// readToken reads the peer's next frame and returns the auth token it carries, or what it read in
// its place.
func readToken(connection *gws.Conn) string {
	_, frame, err := connection.ReadMessage()
	if err != nil {
		return fmt.Sprintf("no frame (%v)", err)
	}
	decoder := encoding.NewDecoder(frame)
	name, _ := decoder.ReadVarString()
	kind, _ := decoder.ReadVarUint()
	sub, _ := decoder.ReadVarUint()
	token, err := decoder.ReadVarString()
	if name != "doc" || kind != messageAuth || sub != authToken || err != nil {
		return fmt.Sprintf("a frame that carries no auth token: %x", frame)
	}
	return token
}

// AwaitAuthentication sends the token and waits for the room's answer, which the room sends only
// once it has read the token, and returns the scope the room granted. A connection the room ends
// first with a close frame ends the wait with an error naming the frame, not with the give-up. A
// room that never answers fails the row by name: the child dials the room in its setup, takes the
// give-up as its next step (testwait.SetUpDone), and its wait gives up at its first question,
// naming the answer it waited for.
func TestAwaitAuthenticationWaitsForTheRoomsAnswerAndFailsByName(t *testing.T) {
	t.Parallel()
	if os.Getenv(giveUpChildEnv) != "" {
		url := stubRoom(t, func(connection *gws.Conn) {
			readToken(connection)
			readUntilEnd(connection)
		})
		peer := Dial(t, url, nil, "doc", crdt.New())
		testwait.SetUpDone(t)
		scope, err := peer.AwaitAuthentication(t, "v1")
		t.Fatalf("AwaitAuthentication returned (%q, %v) for a room that never answers", scope, err)
	}
	test := t.Name()
	t.Run("waits for its fact", func(t *testing.T) {
		tokens := make(chan string, 1)
		url := stubRoom(t, func(connection *gws.Conn) {
			tokens <- readToken(connection)
			granted := roomFrame(messageAuth, func(encoder *encoding.Encoder) {
				encoder.WriteVarUint(authAuthenticated)
				encoder.WriteVarString("read-write")
			})
			if connection.WriteMessage(gws.BinaryMessage, granted) == nil {
				readUntilEnd(connection)
			}
		})
		peer := Dial(t, url, nil, "doc", crdt.New())
		scope, err := peer.AwaitAuthentication(t, "v1")
		if err != nil || scope != "read-write" {
			t.Fatalf("AwaitAuthentication(v1) = %q, %v; want read-write and no error", scope, err)
		}
		if token := testwait.Receive(t, "the room's read of the auth token", tokens); token != "v1" {
			t.Fatalf("the room read %s, want the auth token v1", token)
		}
	})
	t.Run("ends with the connection", func(t *testing.T) {
		url := stubRoom(t, func(connection *gws.Conn) {
			readToken(connection)
			if connection.WriteMessage(gws.CloseMessage, gws.FormatCloseMessage(4001, "the room ends the connection")) == nil {
				readUntilEnd(connection)
			}
		})
		peer := Dial(t, url, nil, "doc", crdt.New())
		scope, err := peer.AwaitAuthentication(t, "v1")
		const want = "the peer's connection ended (close frame 4001, close code 4001, read error: websocket: close 4001: the room ends the connection) before the room answered the auth token"
		if err == nil || err.Error() != want {
			t.Fatalf("AwaitAuthentication(v1) on a connection the room closed = %q, %v; want the error %q", scope, err, want)
		}
	})
	t.Run("fails by name", func(t *testing.T) {
		requireGiveUp(t, test, "the room's answer to the auth token 30s before the test binary's deadline; last read: no answer")
	})
}

// shutdownFunc is a document service as Shutdown takes it: its Shutdown is the function.
type shutdownFunc func(context.Context) error

func (f shutdownFunc) Shutdown(ctx context.Context) error { return f(ctx) }

// Shutdown answers the service's own Shutdown error, on the context the row passed, once that call
// has returned: a form that returned before the call did would have no error of the call's to
// answer. A shutdown that never returns fails the row by name: the child's service holds its
// Shutdown until the child ends, the child takes the give-up as its step (testwait.SetUpDone), and
// Shutdown's wait gives up naming the shutdown.
func TestShutdownWaitsForTheServicesShutdownAndFailsByName(t *testing.T) {
	t.Parallel()
	if os.Getenv(giveUpChildEnv) != "" {
		released := make(chan struct{})
		testcleanup.Add(t, func() { close(released) })
		testwait.SetUpDone(t)
		err := Shutdown(t, context.Background(), shutdownFunc(func(context.Context) error {
			<-released
			return nil
		}))
		t.Fatalf("Shutdown returned (%v) for a service whose shutdown never returns", err)
	}
	test := t.Name()
	t.Run("waits for its fact", func(t *testing.T) {
		type rowKey struct{}
		errShutDown := errors.New("the service shut down")
		ctx := context.WithValue(context.Background(), rowKey{}, "the row's context")
		err := Shutdown(t, ctx, shutdownFunc(func(ctx context.Context) error {
			return fmt.Errorf("%w on %v", errShutDown, ctx.Value(rowKey{}))
		}))
		if !errors.Is(err, errShutDown) || err.Error() != "the service shut down on the row's context" {
			t.Fatalf("Shutdown = %v, want the service's own error, on the row's context", err)
		}
	})
	t.Run("fails by name", func(t *testing.T) {
		requireGiveUp(t, test, "the document service's shutdown to return 30s before the test binary's deadline")
	})
}
