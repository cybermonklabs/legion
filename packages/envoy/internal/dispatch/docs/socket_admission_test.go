package docs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Opening a document socket is what every browser tab does on every load and every reconnect, and
// an eviction or a deploy reconnects every browser of a room at once. The tests here are about what
// one open costs the service beyond the socket itself: how often it renders the whole document, how
// it decides the caps without walking every room, and that the one read of the issue it keeps still
// sees an issue closed while the socket loads.

// socketURL is id's document socket on serverURL, at the schema version the dashboard presents, so
// the only thing that can make the socket read-only is the document's issue.
func socketURL(serverURL, id string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http") + "/ws/doc/" + id +
		"?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
}

// readDeadline is when a read of a test's socket gives up: where the shared wait does, the test
// binary's deadline less its margin (testwait.GiveUpAt), and never under -timeout 0.
func readDeadline(t *testing.T) time.Time {
	deadline, _ := testwait.GiveUpAt(t)
	return deadline
}

// admitSocket opens id's document socket as alice's browser does and returns it once the room has
// sent the first frame of its sync, so the socket's admission, and a cold room's load, are done.
func admitSocket(t *testing.T, serverURL, id, what string) *gws.Conn {
	t.Helper()
	connection, response, err := gws.DefaultDialer.Dial(socketURL(serverURL, id), http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		status := "no response"
		if response != nil {
			status = response.Status
		}
		t.Fatalf("%s: the socket was refused (%s: %v), want it admitted", what, status, err)
	}
	testcleanup.Add(t, func() { _ = connection.Close() })
	_ = connection.SetReadDeadline(readDeadline(t))
	if _, message, err := connection.ReadMessage(); err != nil {
		t.Fatalf("%s: the socket's first frame = %q (%v), want the room's sync", what, message, err)
	}
	_ = connection.SetReadDeadline(time.Time{})
	return connection
}

// openedSocket is what a dial off the test's own goroutine answers with (dialSocketAsync): the
// socket it opened, or why it was refused, with the response's status before it.
type openedSocket struct {
	connection *gws.Conn
	err        error
}

// dialSocketAsync dials id's document socket as alice's browser does, on a goroutine of its own,
// and answers on the channel it returns. A test dials this way when the open it is about to make
// holds inside the service - in an admission's rendering, in a room's load - where a dial on the
// test's own goroutine would wait for the very hold that goroutine has to release.
func dialSocketAsync(serverURL, id string) <-chan openedSocket {
	opened := make(chan openedSocket, 1)
	go func() {
		connection, response, err := gws.DefaultDialer.Dial(socketURL(serverURL, id), http.Header{"X-Dispatch-User": []string{"alice"}})
		if err != nil {
			status := "no response"
			if response != nil {
				status = response.Status
			}
			err = fmt.Errorf("%s: %w", status, err)
		}
		opened <- openedSocket{connection, err}
	}()
	return opened
}

// refuseSocket opens id's document socket and requires the service to refuse it with
// ServiceUnavailable before any upgrade, as a cap refuses one.
func refuseSocket(t *testing.T, serverURL, id, what string) {
	t.Helper()
	connection, response, err := gws.DefaultDialer.Dial(socketURL(serverURL, id), http.Header{"X-Dispatch-User": []string{"alice"}})
	if connection != nil {
		leaveDocument(t, connection)
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("%s: response=%v err=%v, want the socket refused with 503", what, response, err)
	}
}

// registeredConnections is how many browser connections the service's room records hold, read
// by the test itself rather than through the service's own count.
func registeredConnections(service *Service) int {
	count := 0
	service.rooms.Range(func(_, value any) bool {
		state := value.(*roomState)
		state.mu.Lock()
		count += len(state.connected)
		state.mu.Unlock()
		return true
	})
	return count
}

// addConnection registers a browser connected to room as a document socket's admission does: it
// takes a place under the connection cap (reserveConnection), as ServeHTTP does, and registers the
// browser against it (registerConnectionLocked), as authorize does. An id registered already keeps
// its one place, so the count counts each id once however often a test registers it. A test that
// registers past the cap panics, since the browser it means to connect would have been refused.
func (s *Service) addConnection(room string, id uint64, actor model.Actor) {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	if _, registered := state.connected[id]; !registered && !s.reserveConnection() {
		panic(fmt.Sprintf("register connection %d on room %s: the connection cap (%d) is reached", id, room, s.connectionCap))
	}
	s.registerConnectionLocked(state, id, actor)
}

// refuseSpelling dials spelling's document socket as a browser does and requires the service to
// refuse it 404 before any upgrade, with the sentence that tells a developer where the document
// really is: its id as Dispatch writes it, canonical.
func refuseSpelling(t *testing.T, serverURL, spelling, canonical, what string) {
	t.Helper()
	connection, response, err := gws.DefaultDialer.Dial(socketURL(serverURL, spelling), http.Header{"X-Dispatch-User": []string{"alice"}})
	if connection != nil {
		leaveDocument(t, connection)
	}
	if err == nil || response == nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("%s: response=%v err=%v, want the socket refused 404", what, response, err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatalf("%s: read the refusal's body: %v", what, readErr)
	}
	want := "document not found: a document's address is its id as Dispatch writes it, lower case with dashes, here " + canonical
	if got := strings.TrimRight(string(body), "\n"); got != want {
		t.Fatalf("%s: the refusal says %q, want %q", what, got, want)
	}
}

// A document's address is its id as Dispatch writes it, lower case with dashes, and a socket
// naming it any other way uuid.Parse would take - upper case, braced, a dash-free run of hex,
// urn:uuid:<id> - is refused. Refusing is what keeps one document one room, and therefore one
// lock: a durable write takes its lock on the room's own name (lockDocumentRoom) while the
// stream it appends to is keyed by the document's uuid, so two rooms of one id would be two
// editor states over one stream, neither browser seeing the other's typing and the stored
// markdown ending as text that matched neither. Naming the room canonically and admitting the
// socket anyway is no answer either: the client routes each frame by the name it asked for, so
// such a browser holds a connection and a room's place and never syncs. What the refusal must
// not break is the canonical spelling itself, which is why this drives it first: two browsers
// on it share one room and what one types reaches the other.
func TestASocketNamingADocumentByAnotherSpellingOfItsIdIsRefused(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "Shared.")
	service, server := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "Shared.")
	waitForLandedAppends(t, service, artifactID)
	stored := storedMarkdown(t, database, artifactID)
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", stored)
	bob := connectWithScope(t, server.URL, artifactID, "bob", "read-write", stored)
	if rooms := service.ResidentRooms(); !slices.Equal(rooms, []string{artifactID}) {
		t.Fatalf("the process holds the rooms %v, want the one room %s", rooms, artifactID)
	}
	typeOver(t, alice, "Shared.", "Alice typed this.")
	if err := bob.WaitForText(t, peerMarkdown, "Alice typed this.\n"); err != nil {
		t.Fatalf("bob's browser, on the canonical id %s, never saw what alice typed in the same room: %v", artifactID, err)
	}

	for _, spelling := range []struct{ what, room string }{
		{"the id in upper case", strings.ToUpper(artifactID)},
		{"the id in braces", "{" + artifactID + "}"},
		{"the id without its dashes", strings.ReplaceAll(artifactID, "-", "")},
		{"the id as a urn", "urn:uuid:" + artifactID},
	} {
		refuseSpelling(t, server.URL, spelling.room, artifactID, "a socket naming "+spelling.what)
		if rooms := service.ResidentRooms(); !slices.Equal(rooms, []string{artifactID}) {
			t.Fatalf("after a socket naming %s the process holds the rooms %v, want the one room %s and no other",
				spelling.what, rooms, artifactID)
		}
	}
}

// A socket's admission refuses a room outside the Proof schema, which it decides by rendering the
// document. A warm room has already been rendered by its update observer after every update, so
// its verdict is known and the open renders nothing; a cold open renders the document it loads
// once, not once to admit the socket and again when the room it loads takes the same state. The
// verdict is still right: a room an update took outside the schema is refused, and the same room
// repaired by a replacement from markdown admits sockets again.
func TestAWarmSocketOpenRendersNothingAndAColdOpenRendersOnce(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	var renders atomic.Int64
	service.admissionRendered = func(room string) {
		if room == artifactID {
			renders.Add(1)
		}
	}

	before := renders.Load()
	admitSocket(t, server.URL, artifactID, "a cold open")
	if cold := renders.Load() - before; cold != 1 {
		t.Errorf("a cold socket open rendered the whole document %d times, want once", cold)
	}

	before = renders.Load()
	admitSocket(t, server.URL, artifactID, "a warm open")
	if warm := renders.Load() - before; warm != 0 {
		t.Errorf("a warm socket open rendered the whole document %d times, want none: the room's update observer already rendered it", warm)
	}

	writeSchemaInvalidElement(t, service, artifactID)
	connection, response, err := gws.DefaultDialer.Dial(socketURL(server.URL, artifactID), http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		t.Fatalf("open the room an update took outside the schema: response=%v err=%v, want an upgrade the server closes", response, err)
	}
	testcleanup.Add(t, func() { _ = connection.Close() })
	_ = connection.SetReadDeadline(readDeadline(t))
	_, message, err := connection.ReadMessage()
	var closed *gws.CloseError
	if !errors.As(err, &closed) || closed.Code != documentSchemaCloseCode {
		t.Fatalf("first frame of a socket to a room outside the schema = %q (%v), want close %d before any sync", message, err, documentSchemaCloseCode)
	}

	if _, err := joinedReplaceText(service, artifactID, "Repaired.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("repair the document by a replacement from markdown: %v", err)
	}
	admitSocket(t, server.URL, artifactID, "a socket to the repaired room")
}

// A cold socket whose admission another socket's load overtakes - its own read of the store began
// while the room was cold, and the room became resident before it could offer what it read - keeps
// nothing of that read. The room's load will never read its offer, so the offer goes at that load:
// neither the whole history it carries nor its hold on the document's head-tracker entry, which the
// liveness ping follows (holdsWhatTheTrackerProtects), waits for the socket to close, which can be
// hours later. The socket itself is admitted and current all the same.
func TestAPreloadOvertakenByTheRoomsLoadIsReleasedWhileItsSocketStaysOpen(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	// The first admission of this room holds here, where it has read and rendered the document and
	// has not yet offered either to the room's load (admitDocument). Every admission after it goes
	// through, so the socket that loads the room is admitted while this one waits.
	rendered := make(chan struct{})
	release := make(chan struct{})
	var overtaken atomic.Bool
	var releasedOnce sync.Once
	releaseOvertaken := func() { releasedOnce.Do(func() { close(release) }) }
	testcleanup.Add(t, releaseOvertaken)
	service.admissionRendered = func(room string) {
		if room != artifactID || overtaken.Swap(true) {
			return
		}
		close(rendered)
		<-release
	}

	cold := dialSocketAsync(server.URL, artifactID)
	testwait.Receive(t, "the first cold socket to render the document it read (nothing here is tested until it does)", rendered)

	admitSocket(t, server.URL, artifactID, "the socket whose load makes the room resident")
	releaseOvertaken()
	overtakenSocket := testwait.Receive(t, "the cold socket's dial to answer through the product's admission", cold)
	if overtakenSocket.err != nil {
		t.Fatalf("the overtaken socket was refused (%v), want it admitted to the resident room", overtakenSocket.err)
	}
	testcleanup.Add(t, func() { _ = overtakenSocket.connection.Close() })
	_ = overtakenSocket.connection.SetReadDeadline(readDeadline(t))
	if _, message, err := overtakenSocket.connection.ReadMessage(); err != nil {
		t.Fatalf("the overtaken socket's first frame = %q (%v), want the room's sync", message, err)
	}

	if _, kept := service.preloads.Load(artifactID); kept {
		t.Error("a preload is held for a room already loaded here, want none: no load of this room will ever read it")
	}

	// The socket joined the room it never loaded, so what the room commits reaches it: the
	// release above took its offer back, not its place in the room.
	if _, err := joinedReplaceText(service, artifactID, "Overtaken.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write the document the overtaken socket joined: %v", err)
	}
	_ = overtakenSocket.connection.SetReadDeadline(readDeadline(t))
	if _, message, err := overtakenSocket.connection.ReadMessage(); err != nil {
		t.Fatalf("the overtaken socket's frame after a write = %q (%v), want the write's update", message, err)
	}
}

// A socket's admission reads the document's issue once, before it loads the document. An issue
// closed while that load runs - a person closes the issue as a browser reconnects - still reaches
// the socket: it is admitted read-only, as a socket opened after the close is.
func TestAnIssueClosedWhileASocketLoadsAdmitsItReadOnly(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	var closed atomic.Bool
	service.afterSocketIssueRead = func(room string) {
		if room != artifactID || closed.Swap(true) {
			return
		}
		ctx := context.Background()
		if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
			t.Errorf("close the document's issue while the socket loads: %v", err)
			return
		}
		service.issueOpenChanged(ctx, "DOC-1")
	}

	peer := connectPeer(t, server.URL, artifactID, "alice")
	if !closed.Load() {
		t.Fatal("the socket's admission never reached the hook that closes the issue, so nothing here is tested")
	}
	// The room's answer to the peer's sync step 1 says its load is done, as the first frame does
	// for any socket here: the load is what reads the issue the close left (admittedIssueRead),
	// and the scope the room grants is what it recorded (authorizeSchemaVersion).
	testwait.Receive(t, "the socket to receive the document from the room it loaded", peer.Answers)
	scope, err := peer.Authenticate(t, strconv.Itoa(pmdoc.SchemaVersion()))
	if err != nil || scope != "readonly" {
		t.Fatalf("the socket whose issue closed while it loaded is authenticated %q (%v), want readonly", scope, err)
	}
}

// warmRooms loads each document's room through the service, a few at a time, as API reads warm
// them. The hand-out goes through the shared wait with the join: a load that never returns holds
// its worker, and once every worker is held the hand-out too, and the wait then fails naming it.
func warmRooms(t *testing.T, service *Service, ids []string) {
	t.Helper()
	work := make(chan string)
	var failures sync.Map
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range work {
				if err := service.warmLiveDocument(context.Background(), id); err != nil {
					failures.Store(id, err)
				}
			}
		}()
	}
	testwait.Returns(t, fmt.Sprintf("the loads of %d rooms to return", len(ids)), func() {
		for _, id := range ids {
			work <- id
		}
		close(work)
		workers.Wait()
	})
	failures.Range(func(id, err any) bool {
		t.Fatalf("warm the room of document %s: %v", id, err)
		return false
	})
}

// The service admits at most maxLiveRooms live rooms and maxRoomConnections browser connections.
// Each socket open asks both, so each is a count the service keeps as rooms load and go and as
// browsers come and leave, not a walk over every room record and every live room name: an
// eviction or a deploy reconnects every browser at once, and each of those walks costs every room.
// The counts stay right across a failed room's eviction, whose record goes and hands the browsers
// registered on it to the record that replaces it (handOverFailedRoomLocked).
//
// The rooms are real: the service loads a thousand of them, as API reads do. The connections
// beyond the test's own sockets are registered as a socket's admission registers its browser
// (addConnection), since a thousand real sockets would test the operating system's limits more
// than the service's count.
func TestTheConnectionAndRoomCapsCountWithoutScanning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	// Only to keep ygo's idle sweep away: no room may idle out while the test fills the cap. The
	// sweep reads the wall clock inside ygo (provider/websocket/idle_sweep.go), so no clock of the
	// test's reaches it; it reads the timeout when the first room starts it, so it is set before
	// any room.
	service.srv.RoomIdleTimeout = stagedAhead
	ids := createDocuments(t, database, maxLiveRooms+2)
	resident, spare := ids[:maxLiveRooms], ids[maxLiveRooms:]
	warmRooms(t, service, resident)
	if live := len(service.srv.Rooms()); live != maxLiveRooms {
		t.Fatalf("%d rooms are live after the test loaded %d, so the cap is not what is tested", live, maxLiveRooms)
	}

	// The room cap: the 1,001st room is refused, while a live room still admits a socket.
	refuseSocket(t, server.URL, spare[0], "the 1,001st room")
	leaveDocument(t, admitSocket(t, server.URL, resident[0], "a live room while the room cap is reached"))
	waitFor(t, "the socket that left to be forgotten", func() bool {
		return registeredConnections(service) == 0
	})

	// A failed room's eviction: its room goes, so the cap admits one new room and only one, and
	// the browsers registered on its record move to the record that replaces it.
	failed := resident[1]
	const handedOver = 500
	alice := model.Actor{Kind: "user", ID: "alice"}
	registered := uint64(1) << 40 // far above the ids the service gives real sockets
	for range handedOver {
		registered++
		service.addConnection(failed, registered, alice)
	}
	firstHandedOver := registered
	service.failRoom(failed, errors.New("the test fails the room"))
	if err := service.awaitRoomRecovery(ctx, failed); err != nil {
		t.Fatalf("wait for the failed room's eviction: %v", err)
	}
	if service.srv.GetDoc(failed) != nil {
		t.Fatal("the failed room is still live after its eviction")
	}
	if got := registeredConnections(service); got != handedOver {
		t.Fatalf("the service's records hold %d connections after the failed room's hand-over, want the %d registered on it", got, handedOver)
	}
	admitSocket(t, server.URL, spare[0], "a new room once a failed room's eviction freed its place")
	refuseSocket(t, server.URL, spare[1], "the 1,001st room after the failed room's place was taken")

	// The connection cap, counting the browsers the hand-over carried: with 999 connections the
	// next socket is the 1,000th and is admitted, the one after it is refused, and once one of the
	// handed-over browsers leaves a socket is admitted again.
	for registeredConnections(service) < maxRoomConnections-1 {
		registered++
		service.addConnection(resident[2], registered, alice)
	}
	admitSocket(t, server.URL, resident[3], "the 1,000th connection")
	refuseSocket(t, server.URL, resident[4], "the 1,001st connection")
	service.removeConnection(failed, firstHandedOver)
	admitSocket(t, server.URL, resident[4], "a connection once a handed-over browser left")
	refuseSocket(t, server.URL, resident[5], "the 1,001st connection after a handed-over browser left")
}

// The caps DISPATCH_MAX_LIVE_ROOMS and DISPATCH_MAX_DOC_CONNECTIONS set hold for sockets that open
// at once, as every browser of a room does after an eviction or a deploy, and not only for sockets
// that open one after another: a burst of cold opens never leaves more rooms live, or more browsers
// connected, than the cap, and every socket a cap turns away is answered 503, as the settings say,
// never 401, which tells a browser it is not signed in. On a real Dispatch with the two settings at 2
// and 1,000, ten sockets opened at once on ten documents were all admitted and left ten rooms live;
// with 1,000 and 3, ten sockets on one document left three admitted and seven answered 401.
func TestConcurrentSocketOpensStayWithinTheCapsAndAreRefusedWith503(t *testing.T) {
	t.Parallel()
	const burst = 10
	open := func(t *testing.T, serverURL string, ids []string) (admitted []*gws.Conn, refusals map[int]int) {
		t.Helper()
		var mu sync.Mutex
		refusals = map[int]int{}
		start := make(chan struct{})
		var dials sync.WaitGroup
		// Registered before the burst, so the sockets admitted are closed even when the wait below
		// gives up; a dial that ends after that closes its own.
		closed := false
		testcleanup.Add(t, func() {
			mu.Lock()
			defer mu.Unlock()
			closed = true
			for _, connection := range admitted {
				_ = connection.Close()
			}
		})
		for i := range burst {
			dials.Add(1)
			go func() {
				defer dials.Done()
				<-start
				connection, response, err := gws.DefaultDialer.Dial(socketURL(serverURL, ids[i%len(ids)]), http.Header{"X-Dispatch-User": []string{"alice"}})
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					if closed {
						_ = connection.Close()
						return
					}
					admitted = append(admitted, connection)
					return
				}
				status := 0
				if response != nil {
					status = response.StatusCode
				}
				refusals[status]++
			}()
		}
		close(start)
		testwait.Returns(t, fmt.Sprintf("the %d sockets opened at once to be admitted or refused", burst), dials.Wait)
		return admitted, refusals
	}
	serve := func(t *testing.T, rooms, connections int) (*Service, string) {
		t.Helper()
		database := storetest.Open(t)
		service := New(Deps{
			Store:             database,
			Events:            events.NewBroker(),
			Identity:          headerIdentity(database),
			ServerURL:         "https://dispatch.example",
			Settle:            stagedAhead,
			MaxLiveRooms:      rooms,
			MaxDocConnections: connections,
		})
		testcleanup.Add(t, func() {
			if err := docstest.Shutdown(t, testwait.Context(t), service); err != nil {
				t.Errorf("shutdown document service: %v", err)
			}
		})
		// Only to keep ygo's idle sweep away from the burst's rooms: the sweep reads the wall clock
		// inside ygo (provider/websocket/idle_sweep.go), so no clock of the test's reaches it.
		service.srv.RoomIdleTimeout = stagedAhead
		server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
		testcleanup.Add(t, server.Close)
		return service, server.URL
	}
	refusedOtherwise := func(refusals map[int]int) map[int]int {
		other := map[int]int{}
		for status, count := range refusals {
			if status != http.StatusServiceUnavailable {
				other[status] = count
			}
		}
		return other
	}

	t.Run("room cap", func(t *testing.T) {
		const roomCap = 2
		service, serverURL := serve(t, roomCap, 1_000)
		ids := createDocuments(t, service.store, burst)
		admitted, refusals := open(t, serverURL, ids)
		if live := len(service.srv.Rooms()); live > roomCap || len(admitted) > roomCap {
			t.Errorf("%d sockets opened at once on %d documents: %d admitted and %d rooms live, want at most the room cap of %d (refusals by status: %v)", burst, burst, len(admitted), live, roomCap, refusals)
		}
		if other := refusedOtherwise(refusals); len(other) > 0 {
			t.Errorf("sockets the room cap turned away were answered %v, want 503 for each", other)
		}
	})
	t.Run("connection cap", func(t *testing.T) {
		const connectionCap = 3
		service, serverURL := serve(t, 1_000, connectionCap)
		ids := createDocuments(t, service.store, 1)
		admitted, refusals := open(t, serverURL, ids)
		if len(admitted) > connectionCap {
			t.Errorf("%d sockets opened at once on one document: %d admitted, want at most the connection cap of %d", burst, len(admitted), connectionCap)
		}
		if other := refusedOtherwise(refusals); len(other) > 0 {
			t.Errorf("of %d sockets opened at once on one document with a connection cap of %d, %d were admitted and the rest answered %v: a socket a cap turns away is answered 503, never another status", burst, connectionCap, len(admitted), refusals)
		}
	})
}

// ygo fires a room's OnUnloadDocument only after it has taken the room out of its map and waited
// for the room's persistence to drain (provider/websocket: CloseRoom, handleDisconnect, the idle
// sweep), so the room can load afresh - a browser reconnecting, every browser at once after a
// forced close - before the room it replaced reports its unload. That late unload is of the old
// room, not of the one resident now: a cold socket whose read began while the room was out of
// ygo's map, and whose offer lands after both the new load and the old room's unload, still finds
// the room loaded here and keeps nothing of its read, neither the history nor its hold on the
// document's head-tracker entry, for as long as it stays open.
func TestAPreloadOfferedAfterAnEarlierRoomsLateUnloadIsNotKeptWhileTheRoomIsLoaded(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	admitSocket(t, server.URL, artifactID, "the socket that loads the first room").Close()

	// The first room's unload waits here, where ygo has already taken the room out of its map.
	unloading := make(chan struct{})
	unloadGo := make(chan struct{})
	var unloadOnce, unloadGoOnce sync.Once
	letUnload := func() { unloadGoOnce.Do(func() { close(unloadGo) }) }
	testcleanup.Add(t, letUnload)
	unload := service.srv.OnUnloadDocument
	service.srv.OnUnloadDocument = func(ctx context.Context, room string) {
		if room == artifactID {
			unloadOnce.Do(func() { close(unloading); <-unloadGo })
		}
		unload(ctx, room)
	}
	closed := make(chan error, 1)
	go func() { closed <- service.srv.CloseRoom(artifactID, true) }()
	testwait.Receive(t, "the first room to reach its unload (nothing here is tested until it does)", unloading)

	// A cold socket reads and renders the document while the room is out of ygo's map, and holds
	// there, before its offer.
	rendered := make(chan struct{})
	release := make(chan struct{})
	var held atomic.Bool
	var releaseOnce sync.Once
	letOffer := func() { releaseOnce.Do(func() { close(release) }) }
	testcleanup.Add(t, letOffer)
	service.admissionRendered = func(room string) {
		if room != artifactID || held.Swap(true) {
			return
		}
		close(rendered)
		<-release
	}
	cold := dialSocketAsync(server.URL, artifactID)
	testwait.Receive(t, "the held cold socket to render the document (nothing here is tested until it does)", rendered)

	// Another socket loads the room afresh, then the first room's unload is let go.
	admitSocket(t, server.URL, artifactID, "the socket that loads the second room")
	letUnload()
	if err := testwait.Receive(t, "the first room's close to return once its unload was let go", closed); err != nil {
		t.Fatalf("close the first room: %v", err)
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("the second room is not resident after the first room's unload")
	}

	letOffer()
	socket := testwait.Receive(t, "the cold socket's dial to answer through the product's admission", cold)
	if socket.err != nil {
		t.Fatalf("the held cold socket was refused (%v), want it admitted to the resident room", socket.err)
	}
	testcleanup.Add(t, func() { _ = socket.connection.Close() })
	_ = socket.connection.SetReadDeadline(readDeadline(t))
	if _, message, err := socket.connection.ReadMessage(); err != nil {
		t.Fatalf("the held socket's first frame = %q (%v), want the room's sync", message, err)
	}
	if _, kept := service.preloads.Load(artifactID); kept {
		t.Error("a preload is held for a room loaded here while its socket stays open, want none: no load of this room will ever read it")
	}
}

// A cold socket that offers its read while the room's load is running - after the load has looked
// for a preload (LoadDoc's takePreload) and before it records the room as loaded here
// (onLoadDocument) - offers to a load that will never read it. The load takes that offer out as it
// records itself, so the socket keeps neither the history nor its hold on the document's
// head-tracker entry while it stays open. The load is held inside its own read of the store
// (insideFirstLoadStore), which LoadDoc makes once takePreload has found nothing.
func TestAPreloadOfferedWhileTheRoomLoadsIsReleasedAtThatLoad(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocuments(t, database, 1)[0]
	reading, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	letLoad := func() { releaseOnce.Do(func() { close(release) }) }
	// The load holds inside its own read of the store, once that read has returned and before
	// Load hands it back, which is where LoadDoc makes it: after takePreload has found nothing.
	store := &insideFirstLoadStore{VersionedStore: NewPgVersioned(database), room: artifactID, during: func() {
		close(reading)
		<-release
	}}
	store.armed.Store(true)
	service := New(Deps{
		Store:       database,
		Persistence: store,
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		ServerURL:   "https://dispatch.example",
		Settle:      stagedAhead,
	})
	testcleanup.Add(t, func() {
		letLoad()
		if err := docstest.Shutdown(t, testwait.Context(t), service); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, server.Close)
	// Registered after the server, so it runs before its Close, which waits for the cold socket's
	// handler: a row that fails while the room's load is held lets it go rather than leave that
	// handler waiting on it until the socket's dial gives up.
	testcleanup.Add(t, letLoad)
	offered := make(chan struct{})
	var offeredOnce sync.Once
	service.afterSocketPreload = func(room string, _ *persistence.LoadResult) {
		if room == artifactID {
			offeredOnce.Do(func() { close(offered) })
		}
	}
	loaded := make(chan error, 1)
	go func() { loaded <- service.warmLiveDocument(context.Background(), artifactID) }()
	testwait.Receive(t, "the room's load to read the store (nothing here is tested until it does)", reading)

	cold := dialSocketAsync(server.URL, artifactID)
	testwait.Receive(t, "the cold socket to offer its read while the room loads (nothing here is tested until it does)", offered)
	if _, stored := service.preloads.Load(artifactID); !stored {
		t.Fatal("the cold socket's offer was not stored while the room loaded, so nothing here is tested")
	}

	letLoad()
	if err := testwait.Receive(t, "the room load to return once it was released", loaded); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	socket := testwait.Receive(t, "the cold socket's dial to answer through the product's admission", cold)
	if socket.err != nil {
		t.Fatalf("the cold socket was refused (%v), want it admitted to the resident room", socket.err)
	}
	testcleanup.Add(t, func() { _ = socket.connection.Close() })
	_ = socket.connection.SetReadDeadline(readDeadline(t))
	if _, message, err := socket.connection.ReadMessage(); err != nil {
		t.Fatalf("the cold socket's first frame = %q (%v), want the room's sync", message, err)
	}
	if _, kept := service.preloads.Load(artifactID); kept {
		t.Error("a preload offered while the room loaded is still held after that load, want none: no load of this room will ever read it")
	}
}

// A socket's upgrade made through processHeaderWriter, as ServeHTTP makes it in a process that
// knows its host name, answers with the process's line among the upgrade's headers, and the
// frames after that answer pass through the connection unchanged.
func TestTheUpgradeThroughTheProcessHeaderWriterNamesTheProcess(t *testing.T) {
	t.Parallel()
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w = &processHeaderWriter{ResponseWriter: w, line: []byte(ProcessHeader + ": task-a\r\n")}
		connection, err := (&gws.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		if kind, message, err := connection.ReadMessage(); err == nil {
			_ = connection.WriteMessage(kind, message)
		}
	}))
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	testcleanup.Add(t, server.Close)
	connection, response, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/doc/room", nil)
	if err != nil {
		t.Fatalf("dial the socket: %v", err)
	}
	testcleanup.Add(t, func() { _ = connection.Close() })
	if served := response.Header.Values(ProcessHeader); !slices.Equal(served, []string{"task-a"}) {
		t.Fatalf("the upgrade names %q as its process, want exactly [task-a]; header %v", served, response.Header)
	}
	if err := connection.WriteMessage(gws.BinaryMessage, []byte("frame")); err != nil {
		t.Fatalf("write a frame: %v", err)
	}
	_ = connection.SetReadDeadline(readDeadline(t))
	if _, message, err := connection.ReadMessage(); err != nil || string(message) != "frame" {
		t.Fatalf("the echoed frame = %q (%v), want %q", message, err, "frame")
	}
}
