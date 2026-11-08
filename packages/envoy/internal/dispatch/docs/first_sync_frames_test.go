package docs

import (
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/cputest"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// What a room answers a browser's SyncStep1 with. ygo cuts the room's state into frames of at
// most 64 KiB and ends the run with one SyncStep2 (sjawhar/ygo provider/websocket/peer.go:106 and
// 832-862, sync.EncodeSyncStep2Split, crdt.SplitUpdateV1), so a browser applies one frame in one
// task rather than a whole document in one task of seconds. Nothing in this module calls the cut
// itself, and no row here does either: both read the frames off the socket Service.ServeHTTP
// serves, because the cut is the fork's but the frames are Dispatch's, and the fork's pin
// (packages/envoy/go.mod) decides what a browser of this service is sent.
//
// The rows are here rather than in api or in a browser test because this package serves the
// socket the frames cross: the room server is built in service.go (NewServerWithPersistence) and
// nothing above it sees a frame.

// The Hocuspocus outer tags these rows read and write, beside hocuspocusAwareness
// (awareness_limit.go), as ygo's provider/websocket/server.go:105-118 numbers them: a sync frame,
// the room's acknowledgement of an update a browser sent, and the liveness pair.
const (
	hocuspocusSync       = 0
	hocuspocusSyncStatus = 8
	hocuspocusPing       = 9
	hocuspocusPong       = 10
)

// stateFrameBytes is the cap ygo holds every frame of a room's state to, the document's name and
// the outer tag included (sjawhar/ygo provider/websocket/peer.go:836, stateMessageBytes).
const stateFrameBytes = 64 << 10

// answerDeadline bounds a read of one answer. It is long because the control pin these rows were
// written against answers a chain of clients in seconds of processor time: a row that measures
// that cost has to see it rather than time out inside it.
const answerDeadline = 2 * time.Minute

// roomFrame is one websocket frame a room sent, read far enough to say what it carries and
// measured as it crossed the socket.
type roomFrame struct {
	tag     uint64 // the Hocuspocus outer tag
	sync    int    // the sync message's type on a sync frame, -1 on any other tag
	message []byte // the sync message a sync frame carried, nil on any other tag
	bytes   int    // the whole frame, the document's name and the tag included
}

// writeHocuspocusFrame sends one Hocuspocus frame on connection: the document's name, the outer
// tag, then body, which a sync frame carries raw and a Ping carries none of.
func writeHocuspocusFrame(t *testing.T, connection *gws.Conn, artifactID string, tag uint64, body []byte) {
	t.Helper()
	frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(tag)
		encoder.WriteRaw(body)
	})
	if err := connection.WriteMessage(gws.BinaryMessage, frame); err != nil {
		t.Fatalf("send a frame to document %s's room: %v", artifactID, err)
	}
}

// readToPong sends a Ping and returns every frame the room sent before its Pong. A peer's frames
// are written in the order they were queued (peer.writeSeries), so the Pong follows everything
// queued before the Ping arrived: a frame missing from what comes back is missing rather than
// late, and a row does not wait out a deadline to learn that.
func readToPong(t *testing.T, connection *gws.Conn, artifactID string) []roomFrame {
	t.Helper()
	writeHocuspocusFrame(t, connection, artifactID, hocuspocusPing, nil)

	var frames []roomFrame
	for {
		if err := connection.SetReadDeadline(time.Now().Add(answerDeadline)); err != nil {
			t.Fatalf("set the read deadline on document %s's socket: %v", artifactID, err)
		}
		_, message, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read frame %d from document %s's room: %v", len(frames)+1, artifactID, err)
		}
		frame := roomFrame{sync: -1, bytes: len(message)}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			t.Fatalf("read the document name of frame %d: %v", len(frames)+1, err)
		}
		if frame.tag, err = decoder.ReadVarUint(); err != nil {
			t.Fatalf("read the tag of frame %d: %v", len(frames)+1, err)
		}
		if frame.tag == hocuspocusPong {
			return frames
		}
		if frame.tag == hocuspocusSync {
			frame.message = decoder.RemainingBytes()
			kind, _, err := ygsync.ReadSyncMessage(frame.message)
			if err != nil {
				t.Fatalf("read the sync message of frame %d: %v", len(frames)+1, err)
			}
			frame.sync = kind
		}
		frames = append(frames, frame)
	}
}

// askForTheDocument asks the room for everything it holds, as a browser's provider does as it
// connects - a SyncStep1 naming no state - and returns the frames the room answers with.
func askForTheDocument(t *testing.T, connection *gws.Conn, artifactID string) []roomFrame {
	t.Helper()
	writeHocuspocusFrame(t, connection, artifactID, hocuspocusSync, ygsync.EncodeSyncStep1(crdt.New()))
	return readToPong(t, connection, artifactID)
}

// answerFrames is the frames of an answer that carry the room's state. The room's own SyncStep1,
// which asks this browser for what it holds, and the awareness it sends a browser as it joins
// are no part of the answer and are left out. A SyncStatus is no part of it either, and there
// should be none at all: the room sends one for each update a browser sends it, and this browser
// sent none, so one here would mean the room answered something other than what was asked.
func answerFrames(t *testing.T, frames []roomFrame) []roomFrame {
	t.Helper()
	var answer []roomFrame
	for i, frame := range frames {
		switch {
		case frame.tag == hocuspocusSyncStatus:
			t.Fatalf("frame %d of the answer is a SyncStatus, want none: this browser sent the room no update to acknowledge", i+1)
		case frame.tag != hocuspocusSync, frame.sync == ygsync.MsgSyncStep1:
			continue
		}
		answer = append(answer, frame)
	}
	return answer
}

// renderedRoom is doc's rendering, the markdown an upload of the document would carry.
func renderedRoom(t *testing.T, doc *crdt.Doc) string {
	t.Helper()
	tree, err := treeOf(doc)
	if err != nil {
		t.Fatalf("read the document's tree: %v", err)
	}
	markdown, err := documentMarkdown(tree)
	if err != nil {
		t.Fatalf("render the document: %v", err)
	}
	return markdown
}

// pastedItem is the one text item the document of the row below holds that no frame can carry
// whole: prose a browser pasted, which it writes as a single struct however long it is. Twice the
// frame cap means the cut has to cut the item itself rather than pack it, and still leaves a
// remainder for a frame of its own.
var pastedItem = strings.Repeat("prose a browser pasted as one item of text. ", 2*stateFrameBytes/44+1)

// A browser's first sync is answered in frames it can apply one at a time. Every frame of the
// answer is within the cap, each but the last is an Update that integrates on its own - a receiver
// that applies one parks nothing and waits for nothing - and the last is one SyncStep2, which a
// client reads as the end of its sync. What the frames leave the receiver holding is what the room
// holds.
//
// A room that answered in one message answered a large document in one message of megabytes,
// which a browser applies in one task of seconds: the tab is still until it ends. The document
// here is what a room of people leaves - several browsers writing in turn, each against what the
// others have already written - and holds one pasted item longer than the cap, which no V1 update
// can carry whole and the cut has to cut inside (crdt.SplitUpdateV1, cuttableContent).
func TestEveryFrameOfTheAnswerToABrowsersSyncStep1FitsTheFrameCap(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# The room\n")
	server := newTestServer(t, service)

	peers := make([]*docstest.Peer, 4)
	for number := range peers {
		peers[number] = connectPeer(t, server.URL, artifactID, "alice")
	}
	waitForTheRoomAndItsPeersToAgree(t, service, artifactID, peers)
	for round := range 3 {
		for number, peer := range peers {
			line := fmt.Sprintf("peer %d in round %d: %s", number, round, strings.Repeat("a line this browser typed. ", 3))
			if _, err := peer.Send(typeParagraph(peer.Doc, line)); err != nil {
				t.Fatalf("peer %d's keystroke in round %d: %v", number, round, err)
			}
			// Each peer writes against what the others have already written, so the structs of
			// the document interleave as a room of people leaves them rather than running
			// client by client.
			waitForTheRoomAndItsPeersToAgree(t, service, artifactID, peers)
		}
	}
	if _, err := peers[0].Send(typeParagraph(peers[0].Doc, pastedItem)); err != nil {
		t.Fatalf("the pasted item of %d bytes: %v", len(pastedItem), err)
	}
	waitForTheRoomAndItsPeersToAgree(t, service, artifactID, peers)
	// The room has stored what its browsers wrote before the reader joins: an update still on
	// its way to the room's persistence reaches a browser as a frame of its own.
	waitFor(t, 30*time.Second, "the room to have stored what its browsers wrote", func() bool {
		return !service.hasPendingUpdates(artifactID) && !service.hasDurableAppend(artifactID)
	})

	connection, response, err := gws.DefaultDialer.Dial(socketURL(server.URL, artifactID), http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		t.Fatalf("open a reader's document socket: response=%v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	// What the room sends a browser as it joins - its own SyncStep1, and the room's awareness -
	// is read off before the ask, so what follows the ask is the answer to it.
	readToPong(t, connection, artifactID)
	answer := answerFrames(t, askForTheDocument(t, connection, artifactID))

	sizes := make([]int, len(answer))
	for i, frame := range answer {
		sizes[i] = frame.bytes
	}
	if len(answer) < 2 {
		t.Fatalf("the room answered in %d frame(s) of %v bytes, want a document of this size cut into several", len(answer), sizes)
	}
	for i, frame := range answer {
		want := ygsync.MsgUpdate
		if i == len(answer)-1 {
			want = ygsync.MsgSyncStep2
		}
		if frame.sync != want {
			t.Errorf("frame %d of %d is sync message type %d, want %d: the answer is Updates and then one SyncStep2, last", i+1, len(answer), frame.sync, want)
		}
		if frame.bytes > stateFrameBytes {
			t.Errorf("frame %d of %d is %d bytes, want at most %d: a browser applies one frame in one task (frames %v)", i+1, len(answer), frame.bytes, stateFrameBytes, sizes)
		}
	}

	rebuilt := newDocumentCopy()
	for i, frame := range answer {
		if _, err := ygsync.ApplySyncMessage(rebuilt, frame.message, nil); err != nil {
			t.Fatalf("apply frame %d of %d: %v", i+1, len(answer), err)
		}
		if pending := rebuilt.PendingStats(); pending.Items != 0 || pending.DeleteRanges != 0 {
			t.Fatalf("frame %d of %d left %d struct(s) and %d delete range(s) parked, want none: a frame of the answer integrates on its own",
				i+1, len(answer), pending.Items, pending.DeleteRanges)
		}
	}

	room := service.srv.GetDoc(artifactID)
	if got, want := rebuilt.StateVector(), room.StateVector(); !maps.Equal(got, want) {
		t.Errorf("the document rebuilt from the answer holds %v, want the room's %v", got, want)
	}
	if got, want := renderedRoom(t, rebuilt), renderedRoom(t, room); got != want {
		t.Errorf("the document rebuilt from the answer renders %d bytes, want the room's %d", len(got), len(want))
	}
	t.Logf("the answer is %d frames of %v bytes, the last a SyncStep2", len(answer), sizes)
}

// waitForTheRoomAndItsPeersToAgree waits for the room to hold what each peer has written and for
// each peer to hold what the room holds, so the edit a peer makes next is written against what
// the others have already written.
//
// The room is waited for first, and its clocks are read again on each turn, because a peer's
// update is on its way to the room when Send returns: a wait that read the room's clocks once,
// before the room had taken that update, would wait for nothing and leave the update's
// broadcast still to come - which would reach a browser that joined meanwhile as a frame of its
// own, before any answer to what that browser asks.
func waitForTheRoomAndItsPeersToAgree(t *testing.T, service *Service, artifactID string, peers []*docstest.Peer) {
	t.Helper()
	waitFor(t, 30*time.Second, "the room to hold what its peers wrote", func() bool {
		room := service.srv.GetDoc(artifactID).StateVector()
		for _, peer := range peers {
			client := peer.Doc.ClientID()
			if room.Clock(client) < peer.Doc.StateVector().Clock(client) {
				return false
			}
		}
		return true
	})
	waitFor(t, 30*time.Second, "every peer to hold the room's document", func() bool {
		room := service.srv.GetDoc(artifactID).StateVector()
		for _, peer := range peers {
			held := peer.Doc.StateVector()
			for client, clock := range room {
				if held.Clock(client) < clock {
					return false
				}
			}
		}
		return true
	})
}

// The clients of the chain the row below builds, from this one up. The number is nothing but a
// readable start: a room's clients are random (crdt.NewClientID).
const firstChainedClient = crdt.ClientID(100_000)

// chainedParagraph is what each client of the chain writes: one short paragraph, so that the
// chain's cost is the clients rather than the bytes.
const chainedParagraph = "one line appended in turn."

// The chain lengths the row below compares. They are eight times apart because the bound has to
// tell a walk linear in the clients from one quadratic in them: a quadratic walk grows sixty-four
// times over this step, a linear one eight, and the noise of a loaded two-processor pod does not
// cover the distance between them.
const (
	chainedClientsSmall = 1_000
	chainedClientsLarge = 8_000
)

// chainedClientsSlack is how many times a linear growth may overrun the growth of the clients
// themselves before this row calls it more than linear, so the bound is chainedClientsSlack times
// eight. The cut's own work is not pure walking - it decodes the update, encodes each frame and
// keeps a heap of the structs waiting on each client, which costs a logarithm above linear - and
// the rung at a thousand clients is short enough that the fixed cost of an answer is a visible
// part of it, so the ratio measured over this step is above eight without any walk being
// quadratic. Three is the slack pmdoc's linear-time rows allow (linearSlack) and leaves the bound,
// twenty-four, well clear on both sides of what was measured on a two-processor pod: 11 ms and
// 120 ms, a ratio of 10.9, at the pin this was written for, and 58 ms and 5.45 s, a ratio of
// 94.0, at v1.55.1-sami.6, whose cut is the one whose walk grows with the square of the clients.
const chainedClientsSlack = 3

// Answering a browser's SyncStep1 costs no more than linearly in the clients whose structs chain,
// which is what a room where people append in turn looks like: the item of each client names the
// item of the client before it as its left origin, so the structs depend on one another in the
// order the clients arrived.
//
// The cut walked the clients in the order the encoder writes them, which is the reverse of the
// order such a chain runs in, and repeated whole passes until a pass placed nothing: each pass
// placed about one struct, so the walk made a pass per client and the work grew with the square of
// them (sjawhar/ygo v1.55.1-sami.7 release notes). A browser syncing such a room was sent nothing
// at all for the whole of that wait - no frame, no close and no error - and every later load paid
// the same cost, which a client can build on purpose. Measured on a pod of this shape, the cut
// alone took 56.5 ms at 1,000 chained clients, 227.5 ms at 2,000 and 1.015 s at 4,000 before the
// fix, against about 5 ms, 26 ms and, at 40,000 clients, 290 ms after it.
//
// What is asserted is the shape and not a millisecond count, which on a two-processor pod says
// nothing: the answer at chainedClientsLarge may cost no more than chainedClientsSlack times the
// growth of the clients over the answer at chainedClientsSmall. Each rung is timed in this
// process's CPU, which the room's cut spends on the goroutine of the socket this row reads
// (cputest.Measure); the smaller rung is timed again after the larger one and the faster of the
// two kept, so neither a cold start nor a load spike on the first rung reads as the larger rung
// growing. No test in this package calls t.Parallel.
func TestAnsweringASyncStep1CostsNoMoreThanLinearlyInTheRoomsChainedClients(t *testing.T) {
	service, small := newTestService(t)
	service.settle = time.Hour
	large := createDocument(t, service.store, "# A room people append to")
	server := newTestServer(t, service)

	rungs := []struct {
		room    string
		clients int
	}{{small, chainedClientsSmall}, {large, chainedClientsLarge}}
	for _, rung := range rungs {
		seedServiceText(t, service, rung.room, "# A room people append to\n")
	}
	answers := make([]func() time.Duration, len(rungs))
	for i, rung := range rungs {
		injector := connectPeer(t, server.URL, rung.room, "alice")
		if err := injector.Write(ygsync.EncodeUpdate(chainedClientsUpdate(t, rung.clients))); err != nil {
			t.Fatalf("send the room its %d chained clients: %v", rung.clients, err)
		}
		waitFor(t, answerDeadline, fmt.Sprintf("the room to hold %d chained clients", rung.clients), func() bool {
			room := service.srv.GetDoc(rung.room)
			return room != nil && len(room.StateVector()) > rung.clients
		})
		waitFor(t, answerDeadline, "the room to have stored what the chain wrote", func() bool {
			return !service.hasPendingUpdates(rung.room) && !service.hasDurableAppend(rung.room)
		})
		connection, response, err := gws.DefaultDialer.Dial(socketURL(server.URL, rung.room), http.Header{"X-Dispatch-User": []string{"alice"}})
		if err != nil {
			t.Fatalf("open a reader's socket on the room of %d chained clients: response=%v err=%v", rung.clients, response, err)
		}
		t.Cleanup(func() { _ = connection.Close() })
		readToPong(t, connection, rung.room)
		answers[i] = func() time.Duration {
			// A batch has no error of its own to report: a frame that does not arrive, or does
			// not read, fails the test where it is read.
			took, _ := cputest.Measure(func(runs int) (time.Duration, error) {
				before := cputest.Process()
				for range runs {
					askForTheDocument(t, connection, rung.room)
				}
				return cputest.Process() - before, nil
			})
			return took
		}
	}

	smallTook, largeTook := answers[0](), answers[1]()
	smallTook = min(smallTook, answers[0]())
	grew := float64(largeTook) / float64(smallTook)
	bound := float64(chainedClientsSlack * chainedClientsLarge / chainedClientsSmall)
	t.Logf("answering a SyncStep1: %s at %d chained clients, %s at %d, %.1f times for %d times the clients (at most %.0f)",
		smallTook.Round(time.Millisecond), chainedClientsSmall, largeTook.Round(time.Millisecond), chainedClientsLarge,
		grew, chainedClientsLarge/chainedClientsSmall, bound)
	if grew > bound {
		t.Fatalf("answering a SyncStep1 took %s at %d chained clients and %s at %d, %.1f times for %d times the clients: a cut whose walk is linear in them may grow %.0f times at most (chainedClientsSlack)",
			smallTook.Round(time.Millisecond), chainedClientsSmall, largeTook.Round(time.Millisecond), chainedClientsLarge,
			grew, chainedClientsLarge/chainedClientsSmall, bound)
	}
}

// chainedClientsUpdate is the state of a document clients have appended a paragraph to in turn:
// the paragraph of each names the paragraph of the one before it as its left origin, so the
// structs chain from the first client to the last and no struct but the first integrates until
// the one before it has.
//
// Each link is built from two documents of its own rather than from the whole chain, so building
// the chain costs the clients and not their square: the writer of a link holds nothing but the
// paragraph of the client before it, written as a document of its own, which carries the same
// struct ids as that client's paragraph in the chain and so leaves the writer naming the same left
// origin.
func chainedClientsUpdate(t *testing.T, clients int) []byte {
	t.Helper()
	chain := newDocumentCopy()
	add := func(update []byte, what string) {
		if err := crdt.ApplyUpdateV1(chain, update, nil); err != nil {
			t.Fatalf("add %s to the chain: %v", what, err)
		}
	}
	var alone []byte // the paragraph of the client before this one, as a document of its own
	for i := range clients {
		client := firstChainedClient + crdt.ClientID(i)
		standalone := crdt.New(crdt.WithClientID(client))
		standaloneUpdate := docstest.Transact(standalone, paragraphAt(standalone, 0, chainedParagraph))
		if i == 0 {
			add(standaloneUpdate, "the chain's first paragraph")
		} else {
			writer := crdt.New(crdt.WithClientID(client))
			if err := crdt.ApplyUpdateV1(writer, alone, nil); err != nil {
				t.Fatalf("give client %d the paragraph it appends after: %v", i, err)
			}
			add(docstest.Transact(writer, paragraphAt(writer, 1, chainedParagraph)), fmt.Sprintf("the paragraph of client %d", i))
		}
		alone = standaloneUpdate
	}
	if held := len(chain.StateVector()); held != clients {
		t.Fatalf("the chain holds the structs of %d clients, want %d", held, clients)
	}
	return crdt.EncodeStateAsUpdateV1(chain, nil)
}

// paragraphAt is one transaction appending a paragraph of text as the fragment's child at index,
// which is the end of what the document holds: the paragraph there names the one before it as its
// left origin.
func paragraphAt(document *crdt.Doc, index int, text string) func(*crdt.Transaction) {
	fragment := document.GetXmlFragment(fragmentName)
	return func(txn *crdt.Transaction) {
		paragraph := crdt.NewYXmlElement("paragraph")
		fragment.InsertElement(txn, index, paragraph)
		run := crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, run)
		run.Insert(txn, 0, text, nil)
	}
}
