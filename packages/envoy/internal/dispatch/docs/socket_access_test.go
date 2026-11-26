package docs

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// writeAccess runs write in a transaction of its own and commits it, as an access route does: the
// commit is what announces the change to every process's room sync.
func writeAccess(t *testing.T, service *Service, write func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the access write: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := write(ctx, tx); err != nil {
		t.Fatalf("write the access change: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the access change: %v", err)
	}
}

// documentSocketRefusal is how the document socket at serverURL refuses login's upgrade to room:
// its status and its body.
func documentSocketRefusal(t *testing.T, serverURL, room, login string) string {
	t.Helper()
	outcome, upgraded := documentSocketOutcome(t, serverURL, room, login)
	if upgraded {
		t.Fatalf("%s opened the document socket of %s (%s); want it refused", login, room, outcome)
	}
	return outcome
}

// documentSocketOutcome is what login's upgrade to room at serverURL ends in: the refusal's status
// and body, or, for a socket the upgrade opened, the close code the socket ended with.
func documentSocketOutcome(t *testing.T, serverURL, room, login string) (outcome string, upgraded bool) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws/doc/" + room + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	connection, response, err := gws.DefaultDialer.Dial(url, http.Header{"X-Dispatch-User": []string{login}})
	if err == nil {
		defer connection.Close()
		_ = connection.SetReadDeadline(time.Now().Add(roomWait(t)))
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				var closed *gws.CloseError
				if errors.As(err, &closed) {
					return "upgraded, then closed " + strconv.Itoa(closed.Code), true
				}
				return "upgraded, then " + err.Error(), true
			}
		}
	}
	if response == nil {
		t.Fatalf("%s's upgrade to %s: %v, with no answer", login, room, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the refusal of %s's upgrade to %s: %v", login, room, err)
	}
	return response.Status + " " + string(body), false
}

// TestTheDocumentSocketClosesWhenItsPersonLosesTheProject: a document of a private project is a
// room only the project's people open. carol, whom no list names, is refused it exactly as a room
// no document holds is refused; bob, on the access list, opens it, as does alice, its owner. Once
// alice takes bob off the list, the access change's announcement reaches the room sync, which
// closes bob's socket with the code on which the provider reconnects, and bob's reconnect is
// refused as carol's upgrade was. alice's socket stays open.
func TestTheDocumentSocketClosesWhenItsPersonLosesTheProject(t *testing.T) {
	t.Parallel()
	const bob, carol = "bob@outside.example", "carol@example.com"
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	runRoomSync(t, newRoomSync(service))
	owner := model.Actor{Kind: "user", ID: "alice"}
	writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
		if _, _, err := access.SetVisibility(ctx, tx, service.events, "DOC", model.ProjectPrivate, owner); err != nil {
			return err
		}
		_, _, err := access.AddPrincipal(ctx, tx, service.events, "DOC", access.PrincipalPerson, bob, owner)
		return err
	})
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, httpServer.Close)

	missing := documentSocketRefusal(t, httpServer.URL, uuid.NewString(), carol)
	if hidden := documentSocketRefusal(t, httpServer.URL, artifactID, carol); hidden != missing {
		t.Fatalf("carol's upgrade to a document of DOC, which she does not see, is refused %q; a room no document holds is refused %q", hidden, missing)
	}
	listed := connectPeer(t, httpServer.URL, artifactID, bob)
	if err := listed.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
		t.Fatalf("bob, on DOC's list, reads its document: %v", err)
	}
	keeper := connectPeer(t, httpServer.URL, artifactID, "alice")
	if err := keeper.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
		t.Fatalf("alice, DOC's owner, reads its document: %v", err)
	}

	writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := access.RemovePrincipal(ctx, tx, service.events, "DOC", access.PrincipalPerson, bob, owner)
		return err
	})
	select {
	case <-listed.Ended:
	case <-time.After(roomWait(t)):
		t.Fatalf("bob's document socket was still open %s after he was taken off DOC's list", roomWait(t))
	}
	if code := listed.CloseCode(); code != contracts.DocumentAdmissionRevokedCloseCode {
		t.Errorf("bob's document socket closed with %d, want %d, on which the provider reconnects", code, contracts.DocumentAdmissionRevokedCloseCode)
	}
	if again := documentSocketRefusal(t, httpServer.URL, artifactID, bob); again != missing {
		t.Errorf("bob's reconnect after losing DOC is refused %q; a room no document holds is refused %q", again, missing)
	}
	// One pass of the room sync closes every socket it closes at once, so a second after bob's
	// close and his refusal is room enough for alice's to have ended with his.
	select {
	case <-keeper.Ended:
		t.Fatalf("alice's document socket closed (%d), though she still sees DOC", keeper.CloseCode())
	case <-time.After(time.Second):
	}
}

// TestAPersonWhoLosesTheRoomDuringTheirOwnAdmissionIsRefusedAsAReaderWhoCannotReadIt: bob, on DOC's
// list, opens a socket to DOC's document, and alice takes him off the list while that socket is
// being admitted. He gets what carol, whom no list names, gets for the same room - the refusal of a
// room no document holds - and is never upgraded onto it: not ygo's 500 "room unavailable", which a
// cold room's load answers once the socket's context is ended, and not an upgrade the re-check
// closes afterwards, which a warm room's join would let send him the document. Each way an access
// re-check reaches his socket is taken: one that begins after his read of the room and before his
// socket is recorded, so his admission reads him again (recheckAdmission), and one that begins once
// his socket is recorded and walks it (recheckAccess), each on a cold room and on a warm one, which
// alice holds open.
func TestAPersonWhoLosesTheRoomDuringTheirOwnAdmissionIsRefusedAsAReaderWhoCannotReadIt(t *testing.T) {
	t.Parallel()
	const bob, carol = "bob@outside.example", "carol@example.com"
	owner := model.Actor{Kind: "user", ID: "alice"}
	for _, recheck := range []struct {
		name string
		// hook is the service's hook the re-check runs in.
		hook func(service *Service, run func(room string))
	}{
		{"a re-check that misses his socket", func(service *Service, run func(string)) { service.afterSocketAdmitted = run }},
		{"a re-check that walks his socket", func(service *Service, run func(string)) { service.afterSocketRecorded = run }},
	} {
		for _, warm := range []bool{false, true} {
			name := recheck.name + ", on a cold room"
			if warm {
				name = recheck.name + ", on a warm room"
			}
			t.Run(name, func(t *testing.T) {
				service, artifactID := newTestService(t)
				seedServiceText(t, service, artifactID, "before")
				writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
					if _, _, err := access.SetVisibility(ctx, tx, service.events, "DOC", model.ProjectPrivate, owner); err != nil {
						return err
					}
					_, _, err := access.AddPrincipal(ctx, tx, service.events, "DOC", access.PrincipalPerson, bob, owner)
					return err
				})
				httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
				testcleanup.Add(t, httpServer.Close)
				if warm {
					keeper := connectPeer(t, httpServer.URL, artifactID, "alice")
					if err := keeper.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
						t.Fatalf("alice opens the room: %v", err)
					}
				}
				var armed atomic.Bool
				recheck.hook(service, func(room string) {
					if room != artifactID || !armed.CompareAndSwap(true, false) {
						return
					}
					writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
						_, _, err := access.RemovePrincipal(ctx, tx, service.events, "DOC", access.PrincipalPerson, bob, owner)
						return err
					})
					service.recheckAccess(context.Background())
				})
				refused := documentSocketRefusal(t, httpServer.URL, artifactID, carol)
				armed.Store(true)
				got, _ := documentSocketOutcome(t, httpServer.URL, artifactID, bob)
				if armed.Load() {
					t.Fatal("bob's admission never reached the hook the re-check runs in")
				}
				if got != refused {
					t.Errorf("bob, taken off DOC's list during his own admission, got %q; carol, whom no list names, gets %q", got, refused)
				}
			})
		}
	}
}

// recordingHijacker is a response writer that records what is written to it and whether its
// connection was taken, for an upgrade that takes it.
type recordingHijacker struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (w *recordingHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	server, client := net.Pipe()
	_ = client.Close()
	return server, bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server)), nil
}

// TestASocketRevokedBeforeItsUpgradeIsNeverUpgraded: a socket whose person lost the room after ygo
// joined it to the room, and before the upgrade took its connection, is not upgraded - an upgrade
// would send the person the room's document - and is answered as a hidden room is. A socket whose
// admission stands is upgraded, with nothing written in its place.
func TestASocketRevokedBeforeItsUpgradeIsNeverUpgraded(t *testing.T) {
	t.Parallel()
	hidden := httptest.NewRecorder()
	refuseHiddenRoom(hidden)
	for _, revoked := range []bool{false, true} {
		ctx, cancel := context.WithCancelCause(context.Background())
		testcleanup.Add(t, func() { cancel(nil) })
		if revoked {
			cancel(errAccessRevoked)
		}
		under := &recordingHijacker{ResponseRecorder: httptest.NewRecorder()}
		conn, _, err := (&revocableSocketWriter{ResponseWriter: under, ctx: ctx}).Hijack()
		if conn != nil {
			_ = conn.Close()
		}
		switch {
		case revoked && (err == nil || under.hijacked):
			t.Errorf("the upgrade of a revoked socket took its connection (err %v)", err)
		case revoked && (under.Code != hidden.Code || under.Body.String() != hidden.Body.String()):
			t.Errorf("a revoked socket's upgrade was answered %d %q, want a hidden room's %d %q", under.Code, under.Body.String(), hidden.Code, hidden.Body.String())
		case !revoked && (err != nil || !under.hijacked || under.Body.Len() != 0):
			t.Errorf("the upgrade of a socket whose admission stands: err %v, connection taken %t, written %q", err, under.hijacked, under.Body.String())
		}
	}
}

func TestARevocableSocketWriterAnswersPlainTextBeforeUpgrade(t *testing.T) {
	t.Parallel()
	under := httptest.NewRecorder()
	writer := &revocableSocketWriter{ResponseWriter: under, ctx: context.Background()}
	if _, err := writer.Write([]byte("<socket error>")); err != nil {
		t.Fatalf("write pre-upgrade response: %v", err)
	}
	if got := under.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := under.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := under.Body.String(); got != "<socket error>" {
		t.Errorf("body = %q, want <socket error>", got)
	}
}

// TestARecheckAccessWhileTheServerStopsRevokesNoSocket: shutdown sets stopping before it closes
// the rooms, and recheckAccess returns when stopping is true, so a re-check that runs while the
// server stops never revokes a socket with errAccessRevoked. The close code then tells the truth:
// shutdown's own going-away, not a loss of access.
//
// Control: with the stopping guard removed, recheckAccess under a cancelled lifetime context
// closes the socket with DocumentAdmissionRevokedCloseCode (the m12 test's "the re-check's context
// has ended" row).
func TestARecheckAccessWhileTheServerStopsRevokesNoSocket(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	service := New(Deps{
		Store: database, Events: events.NewBroker(), Identity: headerIdentity(database),
		Origins: text.NewOrigins("https://dispatch.example"),
		Settle:  time.Hour, MaxLiveRooms: 2, MaxDocConnections: 6,
	})
	testcleanup.Add(t, func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	service.srv.RoomIdleTimeout = time.Hour
	ids := createDocuments(t, database, 1)
	owner := model.Actor{Kind: "user", ID: "alice"}
	writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := access.SetVisibility(ctx, tx, service.events, "DOC", model.ProjectPrivate, owner)
		return err
	})
	writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := access.AddPrincipal(ctx, tx, service.events, "DOC", access.PrincipalPerson, "alice@example.com", owner)
		return err
	})
	runRoomSync(t, newRoomSync(service))
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, server.Close)
	peer := connectPeer(t, server.URL, ids[0], "alice@example.com")

	// Mark the server as stopping, then run the re-check with a cancelled context.
	service.stopping.Store(true)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	service.recheckAccess(cancelled)

	// The socket is still alive: the stopping guard returned before the loop.
	select {
	case <-peer.Ended:
		t.Fatalf("socket was revoked while the server stops (close code %d); want no revocation", peer.CloseCode())
	case <-time.After(time.Second):
	}

	// Restore stopping so the live-context control runs the loop.
	service.stopping.Store(false)
	service.recheckAccess(context.Background())
	select {
	case <-peer.Ended:
		t.Fatalf("socket was revoked on a live context while alice is still on the list (close code %d)", peer.CloseCode())
	case <-time.After(time.Second):
	}
	peer.Close()
}

// TestASocketAdmittedAcrossAnAccessCheckIsReadAgainForItsPerson: an access re-check walks the
// sockets recorded when it begins, so a socket whose admission read its room before a re-check
// began, and that is recorded only after it, is one that re-check never walked. Its admission reads
// again whether its person may read the room, which it does after the access change that re-check
// is for has committed: bob, still on DOC's list, keeps his socket, as a socket admitted across a
// re-check of somebody else's access must, and bob, taken off the list between his read of the room
// and his socket's record, has it revoked with the cause on which the provider reconnects, and
// refused before ygo joins him to the room (revocableSocketWriter answers it as a hidden room).
func TestASocketAdmittedAcrossAnAccessCheckIsReadAgainForItsPerson(t *testing.T) {
	t.Parallel()
	const bob = "bob@outside.example"
	owner := model.Actor{Kind: "user", ID: "alice"}
	for _, row := range []struct {
		name    string
		removed bool
		want    error
	}{
		{name: "bob, still on DOC's list, keeps his socket", want: nil},
		{name: "bob, taken off DOC's list meanwhile, has his socket closed", removed: true, want: errAccessRevoked},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
				if _, _, err := access.SetVisibility(ctx, tx, service.events, "DOC", model.ProjectPrivate, owner); err != nil {
					return err
				}
				_, _, err := access.AddPrincipal(ctx, tx, service.events, "DOC", access.PrincipalPerson, bob, owner)
				return err
			})
			request := httptest.NewRequest(http.MethodGet, "/ws/doc/"+artifactID, nil)
			request.Header.Set("X-Dispatch-User", bob)
			request.SetPathValue("room", artifactID)
			request = admittedRequest(t, service, request)
			// bob's read of the room, as ServeHTTP makes it, before the re-check begins.
			connection := request.Context().Value(connectionContextKey{}).(*connectionState)
			connection.actor = model.Actor{Kind: "user", ID: bob}
			connection.admission.person = bob
			connection.admission.accessChecks = service.schemaAdmissions.accessCheckCount()
			if row.removed {
				writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
					_, _, err := access.RemovePrincipal(ctx, tx, service.events, "DOC", access.PrincipalPerson, bob, owner)
					return err
				})
			}
			if held := service.schemaAdmissions.beginAccessCheck(); len(held) != 0 {
				t.Fatalf("the re-check walks %v before bob's socket is recorded", held)
			}
			_, ok := service.authorize(request)
			if ok != (row.want == nil) {
				t.Fatalf("bob's socket, admitted across a re-check, authorized %t; want %t", ok, row.want == nil)
			}
			if cause := context.Cause(request.Context()); cause != row.want {
				t.Errorf("bob's socket, admitted across a re-check, ends with %v, want %v", cause, row.want)
			}
		})
	}
}

// TestASocketBurstAtTheCapsWhileAccessIsRecheckedLeavesNoServiceAlive: everything the access
// re-check added to the document socket - each admission's person and re-check count, the
// re-check itself on its room-sync lane and called directly, the access-change listener, a socket
// admitted while a re-check runs, a socket the re-check closes, and the 503 a socket gets past a
// cap - leaves nothing holding the service once its sockets have closed and it has shut down. A
// burst of sockets, alice's and bob's, opens at once against both caps while bob is taken off
// DOC's list and put back on it, each change announced on store.AccessChangedChannel to the
// running room sync, while the re-check runs in a loop beside it, and while one re-check, begun
// before the burst, is held inside its first read until every socket of the burst has been
// answered. Once the subtest that built the service has ended and its cleanups have run, the
// service must be collected within the leak report's budget (leakWaitBudget), as TestMain
// requires of every service a test builds.
func TestASocketBurstAtTheCapsWhileAccessIsRecheckedLeavesNoServiceAlive(t *testing.T) {
	// Not parallel: requireServiceCollected forces collections of the whole process (runtime.GC)
	// until the service is gone, and the collector is the process's, which the package's parallel
	// tests run on.
	var built weak.Pointer[Service]
	t.Run("the burst", func(t *testing.T) { socketBurstDuringAccessRechecks(t, &built) })
	requireServiceCollected(t, built)
}

// heldRecheck marks the context of the one re-check recheckHolder holds.
type heldRecheck struct{}

// recheckHolder is a pgx.QueryTracer that holds the first statement a context marked heldRecheck
// starts until release, and closes entered as that statement waits: the re-check is then inside
// its first read, begun (schemaAdmissions.beginAccessCheck) and not ended, for as long as the
// test chooses. Every other statement runs as it would.
type recheckHolder struct {
	once          sync.Once
	entered, gate chan struct{}
	releaseOnce   sync.Once
}

func newRecheckHolder() *recheckHolder {
	return &recheckHolder{entered: make(chan struct{}), gate: make(chan struct{})}
}

func (h *recheckHolder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if ctx.Value(heldRecheck{}) == nil {
		return ctx
	}
	h.once.Do(func() {
		close(h.entered)
		<-h.gate
	})
	return ctx
}

func (h *recheckHolder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// release lets the held statement run.
func (h *recheckHolder) release() { h.releaseOnce.Do(func() { close(h.gate) }) }

// socketBurstDuringAccessRechecks runs the burst, and points built at the service it builds, weakly,
// as soon as it is built: a burst that fails part way leaves a service to collect all the same.
func socketBurstDuringAccessRechecks(t *testing.T, built *weak.Pointer[Service]) {
	// An odd number of changes, the first a removal, so bob ends off the list.
	const bob, burst, changes = "bob@outside.example", 24, 7
	holder := newRecheckHolder()
	testcleanup.Add(t, holder.release)
	database := storetest.TracedStore(t, storetest.Open(t), holder)
	service := New(Deps{
		Store: database, Events: events.NewBroker(), Identity: headerIdentity(database), Origins: text.NewOrigins("https://dispatch.example"),
		Settle: time.Hour, MaxLiveRooms: 2, MaxDocConnections: 6,
	})
	*built = weak.Make(service)
	testcleanup.Add(t, func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	service.srv.RoomIdleTimeout = time.Hour
	ids := createDocuments(t, database, 4)
	owner := model.Actor{Kind: "user", ID: "alice"}
	setBob := func(listed bool) error {
		ctx := context.Background()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if listed {
			_, _, err = access.AddPrincipal(ctx, tx, service.events, "DOC", access.PrincipalPerson, bob, owner)
		} else {
			_, _, err = access.RemovePrincipal(ctx, tx, service.events, "DOC", access.PrincipalPerson, bob, owner)
		}
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := access.SetVisibility(ctx, tx, service.events, "DOC", model.ProjectPrivate, owner)
		return err
	})
	if err := setBob(true); err != nil {
		t.Fatalf("put bob on DOC's list: %v", err)
	}
	runRoomSync(t, newRoomSync(service))
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, server.Close)
	// bob's socket, held through the burst, which the first removal closes. It is also the person
	// the held re-check reads first, so that re-check makes a statement to be held in.
	held := connectPeer(t, server.URL, ids[0], bob)
	if err := held.WaitForText(peerMarkdown, "Room cap probe.\n", roomWait(t)); err != nil {
		t.Fatalf("bob reads the first document: %v", err)
	}

	// The held re-check begins before the burst and is inside its first read when the burst
	// starts; it is let go only once every socket of the burst has been answered.
	heldDone := make(chan struct{})
	go func() {
		defer close(heldDone)
		service.recheckAccess(context.WithValue(context.Background(), heldRecheck{}, true))
	}()
	select {
	case <-holder.entered:
	case <-time.After(roomWait(t)):
		t.Fatalf("the held re-check made no statement within %s", roomWait(t))
	}

	var mu sync.Mutex
	var admitted []*gws.Conn
	refusals := map[int]int{}
	var rechecks atomic.Int64
	done := make(chan struct{})
	var beside sync.WaitGroup
	beside.Add(2)
	go func() {
		defer beside.Done()
		for {
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
			}
			service.recheckAccess(context.Background())
			rechecks.Add(1)
		}
	}()
	go func() {
		defer beside.Done()
		for change := range changes {
			if err := setBob(change%2 == 1); err != nil {
				t.Errorf("change bob's place on DOC's list: %v", err)
				return
			}
		}
	}()
	start := make(chan struct{})
	var dials sync.WaitGroup
	for i := range burst {
		dials.Add(1)
		go func() {
			defer dials.Done()
			<-start
			login := "alice"
			if i%2 == 1 {
				login = bob
			}
			connection, response, err := gws.DefaultDialer.Dial(socketURL(server.URL, ids[i%len(ids)]), http.Header{"X-Dispatch-User": []string{login}})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				admitted = append(admitted, connection)
				return
			}
			status := 0
			if response != nil {
				status = response.StatusCode
				_ = response.Body.Close()
			}
			refusals[status]++
		}()
	}
	close(start)
	dials.Wait()
	// A re-check beside the burst is one still open once every socket of it has been answered.
	heldAcross := false
	select {
	case <-heldDone:
	default:
		heldAcross = true
	}
	holder.release()
	<-heldDone
	if heldAcross {
		rechecks.Add(1)
	}
	close(done)
	beside.Wait()
	for _, connection := range admitted {
		_ = connection.Close()
	}
	select {
	case <-held.Ended:
	case <-time.After(roomWait(t)):
		t.Fatalf("bob's held socket was still open %s after he was taken off DOC's list", roomWait(t))
	}
	if refusals[http.StatusServiceUnavailable] == 0 || len(admitted) == 0 || !heldAcross {
		t.Fatalf("the burst admitted %d sockets, refused %v, and ran %d re-checks beside them, the held one still open as it ended: %t: want admissions, 503s at the caps and a re-check open across the burst, or it tested nothing",
			len(admitted), refusals, rechecks.Load(), heldAcross)
	}
	t.Logf("the burst admitted %d sockets, refused %v, ran %d re-checks beside them, and closed bob's held socket with %d",
		len(admitted), refusals, rechecks.Load(), held.CloseCode())
}

// requireServiceCollected fails unless built is collected within leakWaitBudget, forcing
// collections as the package's leak report does (reportLeakedServices).
func requireServiceCollected(t *testing.T, built weak.Pointer[Service]) {
	t.Helper()
	if built == (weak.Pointer[Service]{}) {
		t.Fatal("no service was built, so its collection proves nothing")
	}
	began := time.Now()
	for {
		runtime.GC()
		runtime.GC()
		if built.Value() == nil {
			t.Logf("the service was collected %s after its test ended", time.Since(began).Round(time.Millisecond))
			return
		}
		if time.Since(began) >= leakWaitBudget {
			t.Fatalf("the service is still alive %s after its test ended and shut it down", time.Since(began).Round(time.Millisecond))
		}
		time.Sleep(leakWaitStep)
	}
}
