package docs

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The rows here are about a document socket whose admission went through and whose room's load did
// not: ygo answers such a join with its own 500 "room unavailable" (sjawhar/ygo v1.55.1-sami.2,
// provider/websocket/server.go:2088-2096), and the socket answers its own refusal instead when the
// load found a pool busy or the server ended the socket's request while the room loaded. The
// tester's row (TestASocketWhoseRoomLoadFindsTheRoomsPoolBusyIsRefused503) holds the status of the
// first case; these hold the rest of that answer, the joins that wait on a load another caller
// began, and the loads no socket's admission holds. The last rows are the other answers of ygo's a
// busy pool reached: the 401 of its authorization (server.go:2066-2070), whose read of the live
// server processes and whose failed room a busy pool can make, and the 500 every join waiting on
// a load got when the request that began the load ended.

// A cold socket whose room's load finds the rooms pool busy is refused as the socket's other
// refusals are, 503 with Retry-After and a WARN line naming the room, the pool and the busy pool's
// class, having run that pool's acquire bound out once (store.AcquireStats.Timeouts): the load
// reads the store once, the head its preload is checked against and the document's pending
// settlement in one statement (takePreload), and a failure of that read is the load's, where a busy
// pool once ran its bound out twice - once for the head and once more for the whole history read
// again on the same pool - before ygo answered 500. The load renders nothing either: the
// admission's rendering is the only one a cold socket makes, refused or not.
func TestARoomLoadThatFindsThePoolBusyRefusesItsSocketWithinOneBound(t *testing.T) {
	logs := captureWarnings(t)
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	const bound = 2 * time.Second
	service.store.Pool.SetAcquireBoundForTest(store.RoomsPool, bound)
	artifactID := createDocuments(t, database, 1)[0]
	holds := make(chan *heldPool, 1)
	var renders atomic.Int32
	service.admissionRendered = func(room string) {
		if room != artifactID || renders.Add(1) != 1 {
			return
		}
		holds <- takeTheRoomsPool(t, service)
	}

	busyBefore := poolAcquireStats(service, store.RoomsPool).Timeouts
	answer := <-answerSocketAsync(server.URL, artifactID)
	busy := poolAcquireStats(service, store.RoomsPool).Timeouts - busyBefore
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	var held *heldPool
	select {
	case held = <-holds:
		defer held.release()
	default:
		t.Fatal("the socket's admission never rendered the document, so its room's load met no busy pool")
	}
	took := answer.at.Sub(held.at)
	t.Logf("the socket's handshake was answered %d %q %s after the rooms pool filled", answer.status, answer.body, took.Round(time.Millisecond))
	if answer.status != http.StatusServiceUnavailable {
		t.Fatalf("a socket whose room's load found the rooms pool busy was answered %d %q, want 503", answer.status, answer.body)
	}
	if answer.retryAfter != admission.RetryAfter {
		t.Errorf("the refused socket's Retry-After = %q, want %q, as every other 503 of the server carries", answer.retryAfter, admission.RetryAfter)
	}
	if busy != 1 {
		t.Errorf("the rooms pool's acquire bound ran out %d times before the refused socket was answered, want once: the load's one read of the busy pool", busy)
	}
	if got := renders.Load(); got != 1 {
		t.Errorf("the cold socket rendered its document %d times, want once: the admission's rendering", got)
	}
	line := logs.Lines("a document socket was refused")
	if line == "" {
		t.Fatalf("the refused socket logged no WARN line naming why; the log was:\n%s", logs.String())
	}
	for _, want := range []string{"level=WARN", "room=" + artifactID, "pool=" + store.RoomsPool, "error=pool_busy"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refused socket's line %q does not carry %q", line, want)
		}
	}
}

// A cold socket's room load takes one rooms-pool connection: the admission read and decoded the
// document and the issue already, and the load's one read of the store is the head its preload is
// checked against with the document's pending-settlement row beside it (takePreload), which the
// load's hook takes rather than reading the row a second time (onLoadDocument). Each connection
// the load took was one more wait on a pool of four under load, and one more bound a busy pool
// could run out before the socket was answered.
func TestAnAdmittedRoomLoadTakesOneRoomsPoolConnection(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	var renders atomic.Int32
	var atRender atomic.Uint64
	service.admissionRendered = func(room string) {
		if room != artifactID {
			return
		}
		renders.Add(1)
		atRender.CompareAndSwap(0, poolAcquires(service, store.RoomsPool))
	}

	admitSocket(t, server.URL, artifactID, "a cold open")
	if got := renders.Load(); got != 1 {
		t.Fatalf("the cold socket rendered its document %d times, want once: its room's load took the admission's preload", got)
	}
	if took := poolAcquires(service, store.RoomsPool) - atRender.Load(); took != 1 {
		t.Errorf("the cold socket's room load took %d rooms-pool connections after its admission read the document, want one", took)
	}
}

// A socket whose join waits on a load another caller began - a settlement's warm-up, which no
// admission holds - and that load finds the rooms pool busy, is refused 503 as a socket whose own
// load found it so: ygo hands every caller waiting on a load that load's error, and answers the
// socket's join 500 with it, so the socket reads the load's failure where the load recorded it,
// on the room's record the socket's admission holds (noteLoadFailed). The warm-up itself fails as
// it always did, with the busy pool's error.
func TestASocketWaitingOnALoadThatFindsThePoolBusyIsRefused503(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	const bound = 2 * time.Second
	service.store.Pool.SetAcquireBoundForTest(store.RoomsPool, bound)
	artifactID := createDocuments(t, database, 1)[0]
	warmed := make(chan error, 1)
	loadBegun := waitForRoomLoad(service, artifactID, roomWait(t))
	var once atomic.Bool
	service.admissionRendered = func(room string) {
		if room != artifactID || once.Swap(true) {
			return
		}
		// The pool fills once the admission has read the document, and the warm-up's load begins
		// before the socket offers what it read, so the socket's join finds that load under way and
		// waits on it rather than loading the room itself.
		takeTheRoomsPool(t, service)
		go func() { warmed <- warmUpAsASettlementDoes(service, artifactID) }()
		if err := loadBegun(); err != nil {
			t.Error(err)
		}
	}

	answer := <-answerSocketAsync(server.URL, artifactID)
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	if answer.status != http.StatusServiceUnavailable {
		t.Errorf("a socket waiting on a load that found the rooms pool busy was answered %d %q, want 503", answer.status, answer.body)
	}
	if answer.retryAfter != admission.RetryAfter {
		t.Errorf("the refused socket's Retry-After = %q, want %q", answer.retryAfter, admission.RetryAfter)
	}
	select {
	case err := <-warmed:
		if !store.IsPoolBusy(err) {
			t.Errorf("the settlement's warm-up load ended %v, want the busy pool's error, as a load no admission holds always failed", err)
		}
	case <-time.After(roomWait(t)):
		t.Fatal("the settlement's warm-up load never ended")
	}
}

// A socket whose request the server ends while its join waits on the room's load - the heartbeat's
// re-resolution of the caller failed, which under load is a busy pool, or a re-check revoked the
// socket's admission - is refused 503 with that cause's class logged, and never its text, as its
// admission refuses a request the server ended under it. ygo answers the join 500 with the
// context's error; the browser is still waiting, and nothing of the room failed.
func TestASocketWhoseRequestTheServerEndsWhileItsRoomLoadsIsRefused503(t *testing.T) {
	logs := captureWarnings(t)
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	// Long enough that the warm-up's load still waits for a connection when the request ends.
	service.store.Pool.SetAcquireBoundForTest(store.RoomsPool, time.Minute)
	artifactID := createDocuments(t, database, 1)[0]
	var endRequest atomic.Pointer[context.CancelCauseFunc]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancelCause(r.Context())
		defer cancel(nil)
		endRequest.Store(&cancel)
		service.ServeHTTP(w, r.WithContext(ctx))
	}))
	testcleanup.Add(t, server.Close)
	loadBegun := waitForRoomLoad(service, artifactID, roomWait(t))
	var once atomic.Bool
	service.admissionRendered = func(room string) {
		if room != artifactID || once.Swap(true) {
			return
		}
		takeTheRoomsPool(t, service)
		go func() { _ = warmUpAsASettlementDoes(service, artifactID) }()
		if err := loadBegun(); err != nil {
			t.Error(err)
		}
	}

	answers := answerSocketAsync(server.URL, artifactID)
	// ygo authorizes the socket before it joins the room, so a socket registered on the room's
	// record is one whose join is waiting on the warm-up's load.
	waitFor(t, roomWait(t), "the socket's authorization, so its join waits on the room's load", func() bool {
		return registeredConnections(service) != 0
	})
	cause := fmt.Errorf("re-resolve the socket's caller: %w", store.ErrPoolBusy)
	(*endRequest.Load())(cause)

	answer := <-answers
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	if answer.status != http.StatusServiceUnavailable {
		t.Fatalf("a socket whose request the server ended while its room loaded was answered %d %q, want 503", answer.status, answer.body)
	}
	if answer.retryAfter != admission.RetryAfter {
		t.Errorf("the refused socket's Retry-After = %q, want %q", answer.retryAfter, admission.RetryAfter)
	}
	line := logs.Lines("a document socket was refused")
	if line == "" {
		t.Fatalf("the refused socket logged no WARN line naming why; the log was:\n%s", logs.String())
	}
	for _, want := range []string{"room=" + artifactID, "error=request_ended", "cause=pool_busy"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refused socket's line %q does not carry %q", line, want)
		}
	}
	if strings.Contains(line, "re-resolve the socket's caller") {
		t.Errorf("the refused socket's line %q carries its cause's text, want only the cause's class", line)
	}
}

// A load no socket's admission holds - a settlement's warm-up, here, and a committed write's
// publish, which loads the room the same way - reads the rooms pool as it always did: under a pool
// that stays full it fails with the busy pool's error once it has run one acquire bound out, the
// bound of its read of the stored history, and leaves no room; once a connection comes free while
// it waits for one within that bound it loads. Neither the merged read of a preload's check nor the
// socket's refusal is any of its concern: it has no preload to check and no socket to answer. The
// bounds it ran out are the pool's own count of them (store.AcquireStats.Timeouts), and the
// connection is freed once the pool counts the load waiting for one, not at a moment of the clock.
func TestALoadNoAdmissionHoldsFailsAfterOneBoundOrLoadsOnceAConnectionFrees(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	const bound = time.Second
	service.store.Pool.SetAcquireBoundForTest(store.RoomsPool, bound)
	artifactID := createDocuments(t, database, 1)[0]

	release := holdTheRoomsPool(t, service)
	busyBefore := poolAcquireStats(service, store.RoomsPool).Timeouts
	start := time.Now()
	err := warmUpAsASettlementDoes(service, artifactID)
	took := time.Since(start)
	busy := poolAcquireStats(service, store.RoomsPool).Timeouts - busyBefore
	release()
	t.Logf("the warm-up under a full rooms pool ended after %s: %v", took.Round(time.Millisecond), err)
	if !store.IsPoolBusy(err) {
		t.Fatalf("the warm-up under a full rooms pool ended %v, want the busy pool's error", err)
	}
	if took < bound {
		t.Errorf("the warm-up under a full rooms pool failed after %s, want after its acquire bound (%s)", took.Round(time.Millisecond), bound)
	}
	if busy != 1 {
		t.Errorf("the warm-up under a full rooms pool ran the pool's acquire bound out %d times, want once: its one read of the stored history", busy)
	}
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("the failed warm-up left its room resident")
	}

	rooms, err := service.store.Pool.Rooms()
	if err != nil {
		t.Fatalf("open the rooms pool: %v", err)
	}
	held := holdPool(t, rooms, store.RoomsPool, service.store.Pool)
	defer held.release()
	warmed := make(chan error, 1)
	go func() { warmed <- warmUpAsASettlementDoes(service, artifactID) }()
	waitFor(t, roomWait(t), "the warm-up to wait for a connection of the full rooms pool", func() bool {
		return poolAcquireStats(service, store.RoomsPool).Waiting > 0
	})
	held.releaseOne()
	if err := testwait.Receive(t, "the warm-up once a rooms connection came free", warmed); err != nil {
		t.Fatalf("the warm-up with a rooms connection freed within the bound: %v, want the room loaded", err)
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("the warm-up returned with no room resident")
	}
}

// A socket whose read of the live server processes finds the request pool busy is refused by its
// admission, 503 with Retry-After and a WARN line naming the read and the pool. That read was ygo's
// authorization's (authorize), whose refusal ygo answers 401 "unauthorized", which tells a browser
// that is signed in that it is not. The pool fills once the admission has read the document's
// issue, which reads it too, and before it reads the live server processes (beforeSocketFleetRead),
// so the read of it the socket meets next is the fleet's.
func TestASocketWhoseFleetReadFindsTheRequestPoolBusyIsRefused503(t *testing.T) {
	logs := captureWarnings(t)
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	var once atomic.Bool
	service.beforeSocketFleetRead = func(room string) {
		if room != artifactID || once.Swap(true) {
			return
		}
		// Held until the test ends, whose cleanup gives them back (takePool).
		if _, err := takePool(t, database.Pool, store.RequestsPool, database.Pool); err != nil {
			t.Error(err)
		}
	}

	answer := <-answerSocketAsync(server.URL, artifactID)
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	t.Logf("the socket's handshake was answered %d %q", answer.status, answer.body)
	if answer.status != http.StatusServiceUnavailable {
		t.Fatalf("a socket whose read of the live server processes found the request pool busy was answered %d %q, want 503", answer.status, answer.body)
	}
	if answer.retryAfter != admission.RetryAfter {
		t.Errorf("the refused socket's Retry-After = %q, want %q", answer.retryAfter, admission.RetryAfter)
	}
	line := logs.Lines("a document socket was refused")
	if line == "" {
		t.Fatalf("the refused socket logged no WARN line naming why; the log was:\n%s", logs.String())
	}
	for _, want := range []string{"the live server processes could not be read", "room=" + artifactID, "pool=" + store.RequestsPool, "error=pool_busy"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refused socket's line %q does not carry %q", line, want)
		}
	}
}

// A socket ygo's authorization refuses because its room has failed (authorize) is refused 503 with
// Retry-After when a busy pool failed the room - a busy room-appends pool failing the room's
// append, a busy rooms pool failing its load - where ygo answered 401 "unauthorized" to a browser
// that is signed in: the room reloads once its eviction has run, and the browser retries. A room
// that failed any other way keeps ygo's 401, as it always had. The room fails once the socket's
// admission has offered what it read, and its eviction is held off until the test ends, so the
// socket's authorization meets it failed.
func TestASocketToARoomABusyPoolFailedIsRefused503(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		cause  error
		status int
	}{
		{"a busy pool failed the room", fmt.Errorf("append the room's browser updates: %w", store.ErrPoolBusy), http.StatusServiceUnavailable},
		{"the room failed another way", errors.New("decode the room's stored history: unexpected end of input"), http.StatusUnauthorized},
	} {
		t.Run(row.name, func(t *testing.T) {
			database := storetest.Open(t)
			service, server := newTestServiceInstance(t, database)
			artifactID := createDocuments(t, database, 1)[0]
			evict := make(chan struct{})
			testcleanup.Add(t, func() { close(evict) })
			service.beforeFailedEviction = func(string) { <-evict }
			var once atomic.Bool
			service.afterSocketPreload = func(room string, _ *persistence.LoadResult) {
				if room == artifactID && !once.Swap(true) {
					service.failRoom(room, row.cause)
				}
			}

			answer := <-answerSocketAsync(server.URL, artifactID)
			if answer.err != nil {
				t.Fatal(answer.err)
			}
			t.Logf("the socket's handshake was answered %d %q", answer.status, answer.body)
			if answer.status != row.status {
				t.Fatalf("a socket to a room that failed (%v) was answered %d %q, want %d", row.cause, answer.status, answer.body, row.status)
			}
			if row.status == http.StatusServiceUnavailable && answer.retryAfter != admission.RetryAfter {
				t.Errorf("the refused socket's Retry-After = %q, want %q", answer.retryAfter, admission.RetryAfter)
			}
		})
	}
}

// A join waiting on a room's load is served when the request whose join began the load ends while
// it runs: the load belongs to the room, and its reads run detached from that request's end
// (onLoadDocument). The load's reads ran under that request's context, so the end of it - its
// browser left, or the server ended it - failed the load, and ygo answered every other join
// waiting on it 500. The first socket's preload is taken away, so its load reads the document's
// pending-settlement row itself, under that context, as the load of a socket whose admission
// found the room resident and whose join found it gone does; the load is held as it decodes the
// history until the second socket waits on it, and the first socket's request is ended then.
func TestAJoinWaitingOnALoadIsServedWhenTheRequestThatBeganTheLoadEnds(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	var requests atomic.Int32
	var endFirst atomic.Pointer[context.CancelCauseFunc]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancelCause(r.Context())
		defer cancel(nil)
		if requests.Add(1) == 1 {
			endFirst.Store(&cancel)
		}
		service.ServeHTTP(w, r.WithContext(ctx))
	}))
	testcleanup.Add(t, server.Close)
	var offered atomic.Bool
	service.afterSocketPreload = func(room string, _ *persistence.LoadResult) {
		if room == artifactID && !offered.Swap(true) {
			service.preloads.Delete(room)
		}
	}
	loading := make(chan struct{})
	proceed := make(chan struct{})
	var releaseOnce atomic.Bool
	release := func() {
		if !releaseOnce.Swap(true) {
			close(proceed)
		}
	}
	testcleanup.Add(t, release)
	var decoding atomic.Bool
	service.bootstrapping = func(room string, _ []byte) {
		if room != artifactID || decoding.Swap(true) {
			return
		}
		close(loading)
		<-proceed
	}

	first := answerSocketAsync(server.URL, artifactID)
	select {
	case <-loading:
	case <-time.After(roomWait(t)):
		t.Fatal("the first socket's room never began to load, so nothing here is tested")
	}
	second := dialSocketAsync(t, server.URL, artifactID)
	// ygo authorizes a socket just before its join, so a second socket registered on the room's
	// record is one about to wait on the first one's load; the pause is for it to reach that wait.
	waitFor(t, roomWait(t), "the second socket's authorization, so it waits on the first one's load", func() bool {
		return registeredConnections(service) >= 2
	})
	time.Sleep(100 * time.Millisecond)
	(*endFirst.Load())(nil)
	release()

	opened := <-second
	if opened.err != nil {
		t.Fatalf("the socket waiting on the load the first socket's request began was refused (%v), want it admitted: that request's end is no failure of the room's load", opened.err)
	}
	testcleanup.Add(t, func() { _ = opened.connection.Close() })
	_ = opened.connection.SetReadDeadline(time.Now().Add(roomWait(t)))
	if _, message, err := opened.connection.ReadMessage(); err != nil {
		t.Fatalf("the waiting socket's first frame = %q (%v), want the room's sync", message, err)
	}
	<-first
}

// A socket whose join waits on a load that finds the rooms pool busy is refused 503 even when the
// room's record its admission holds was handed over before that load failed: a failed room's
// eviction hands the record's place, with the browsers connected to it, to a successor
// (handOverFailedRoomLocked), and the load records its failure on the record registered then
// (noteLoadFailed), which the socket reads under the room's name (loadFailedSince). The room
// fails, and its eviction runs, once the socket's admission has offered what it read, with a
// browser connected so the record has a successor; a settlement's warm-up then begins the load
// the socket's join waits on, with the rooms pool full.
func TestASocketWaitingOnALoadAfterItsRoomRecordWasHandedOverIsRefused503(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	const bound = 2 * time.Second
	service.store.Pool.SetAcquireBoundForTest(store.RoomsPool, bound)
	artifactID := createDocuments(t, database, 1)[0]
	warmed := make(chan error, 1)
	loadBegun := waitForRoomLoad(service, artifactID, roomWait(t))
	var once atomic.Bool
	service.afterSocketPreload = func(room string, _ *persistence.LoadResult) {
		if room != artifactID || once.Swap(true) {
			return
		}
		service.addConnection(room, 1<<62, model.Actor{Kind: "user", ID: "bob"})
		service.failRoom(room, errors.New("a test's failure of the room"))
		if err := service.awaitRoomRecovery(context.Background(), room); err != nil {
			t.Errorf("wait for the failed room's eviction: %v", err)
		}
		takeTheRoomsPool(t, service)
		go func() { warmed <- warmUpAsASettlementDoes(service, artifactID) }()
		if err := loadBegun(); err != nil {
			t.Error(err)
		}
	}

	answer := <-answerSocketAsync(server.URL, artifactID)
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	t.Logf("the socket's handshake was answered %d %q", answer.status, answer.body)
	if answer.status != http.StatusServiceUnavailable {
		t.Errorf("a socket waiting on a load that found the rooms pool busy, after its room's record was handed over, was answered %d %q, want 503", answer.status, answer.body)
	}
	select {
	case err := <-warmed:
		if !store.IsPoolBusy(err) {
			t.Errorf("the settlement's warm-up load ended %v, want the busy pool's error", err)
		}
	case <-time.After(roomWait(t)):
		t.Fatal("the settlement's warm-up load never ended")
	}
}

// A socket whose room's load runs past its reads' own bound, a cold read's (coldReadTimeout),
// before the database answers them is refused 503 with Retry-After and a WARN line naming the
// bound's class, as a socket whose load found a pool busy is, where it kept ygo's 500 "room
// unavailable": the bound is this server's own and says nothing of the document
// (errLoadReadsBound). The socket's preload is taken away, so its load reads the document's
// pending-settlement row itself, under that bound (onLoadDocument), and every rooms-pool
// connection is taken as the load decodes the history, with the pool's acquire bound a minute,
// so the reads' bound, shortened to a second, is what ends that read.
func TestASocketWhoseRoomLoadRunsPastItsReadsBoundIsRefused503(t *testing.T) {
	logs := captureWarnings(t)
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	service.store.Pool.SetAcquireBoundForTest(store.RoomsPool, time.Minute)
	previous := coldReadTimeout
	coldReadTimeout = time.Second
	testcleanup.Add(t, func() { coldReadTimeout = previous })
	artifactID := createDocuments(t, database, 1)[0]
	var offered atomic.Bool
	service.afterSocketPreload = func(room string, _ *persistence.LoadResult) {
		if room == artifactID && !offered.Swap(true) {
			service.preloads.Delete(room)
		}
	}
	var decoding atomic.Bool
	service.bootstrapping = func(room string, _ []byte) {
		if room == artifactID && !decoding.Swap(true) {
			takeTheRoomsPool(t, service)
		}
	}

	answer := <-answerSocketAsync(server.URL, artifactID)
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	if !offered.Load() || !decoding.Load() {
		t.Fatal("the socket's preload was never taken away or its room never decoded, so its load read nothing under its reads' bound")
	}
	t.Logf("the socket's handshake was answered %d %q", answer.status, answer.body)
	if answer.status != http.StatusServiceUnavailable {
		t.Fatalf("a socket whose room's load ran past its reads' bound was answered %d %q, want 503", answer.status, answer.body)
	}
	if answer.retryAfter != admission.RetryAfter {
		t.Errorf("the refused socket's Retry-After = %q, want %q", answer.retryAfter, admission.RetryAfter)
	}
	line := logs.Lines("a document socket was refused")
	if line == "" {
		t.Fatalf("the refused socket logged no WARN line naming why; the log was:\n%s", logs.String())
	}
	for _, want := range []string{"its room could not load", "room=" + artifactID, "pool=" + store.RoomsPool, "error=deadline_passed"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refused socket's line %q does not carry %q", line, want)
		}
	}
}

// A socket whose read of its document's issue fails is refused by its admission, 503 with
// Retry-After and a WARN line naming the read and the pool, unless the document is not there,
// which alone is 404 "document not found": a busy request pool told a browser that the document it
// was editing did not exist. The sign-in reads on a pool of its own here, so the issue read is the
// first read of the request pool the socket makes, and every connection of that pool is held as
// the socket opens.
func TestASocketWhoseIssueReadFindsTheRequestPoolBusyIsRefused503(t *testing.T) {
	logs := captureWarnings(t)
	database := storetest.Open(t)
	people := storetest.Reopened(t, database, func(*pgxpool.Config) {})
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  identity.HeaderIdentity{Header: "X-Dispatch-User", People: store.NewPgPeopleStore(people.Pool, testSigningKey, nil)},
		ServerURL: "https://dispatch.example",
		Settle:    time.Hour,
	})
	testcleanup.Add(t, func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	server := newTestServer(t, service)
	artifactID := createDocuments(t, database, 1)[0]

	missing := <-answerSocketAsync(server.URL, uuid.NewString())
	if missing.err != nil {
		t.Fatal(missing.err)
	}
	if missing.status != http.StatusNotFound {
		t.Fatalf("a socket to a document that is not there was answered %d %q, want 404", missing.status, missing.body)
	}

	holdPool(t, database.Pool, store.RequestsPool, database.Pool)
	answer := <-answerSocketAsync(server.URL, artifactID)
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	t.Logf("the socket's handshake was answered %d %q", answer.status, answer.body)
	if answer.status != http.StatusServiceUnavailable {
		t.Fatalf("a socket whose issue read found the request pool busy was answered %d %q, want 503", answer.status, answer.body)
	}
	if answer.retryAfter != admission.RetryAfter {
		t.Errorf("the refused socket's Retry-After = %q, want %q", answer.retryAfter, admission.RetryAfter)
	}
	line := logs.Lines("a document socket was refused")
	if line == "" {
		t.Fatalf("the refused socket logged no WARN line naming why; the log was:\n%s", logs.String())
	}
	for _, want := range []string{"its document's issue could not be read", "room=" + artifactID, "pool=" + store.RequestsPool, "error=pool_busy"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refused socket's line %q does not carry %q", line, want)
		}
	}
}

// A socket whose request the server ends while its admission waits for its failed room's recovery
// - the heartbeat's re-resolution of the caller failed, which under load is a busy pool - is refused
// as the admission's other refusals are, 503 with Retry-After and a WARN line with the cause's
// class, and never its text, where it was a bare 503 no line explained. The room fails before the
// socket opens and its eviction is held off, so the admission's wait ends only with the request,
// which the server ends half a second in.
func TestASocketWhoseRequestTheServerEndsWhileItsFailedRoomRecoversIsRefused503(t *testing.T) {
	logs := captureWarnings(t)
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	evict := make(chan struct{})
	testcleanup.Add(t, func() { close(evict) })
	service.beforeFailedEviction = func(string) { <-evict }
	service.failRoom(artifactID, errors.New("a test's failure of the room"))
	cause := fmt.Errorf("re-resolve the socket's caller: %w", store.ErrPoolBusy)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeoutCause(r.Context(), 500*time.Millisecond, cause)
		defer cancel()
		service.ServeHTTP(w, r.WithContext(ctx))
	}))
	testcleanup.Add(t, server.Close)

	answer := <-answerSocketAsync(server.URL, artifactID)
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	t.Logf("the socket's handshake was answered %d %q", answer.status, answer.body)
	if answer.status != http.StatusServiceUnavailable {
		t.Fatalf("a socket whose request the server ended while its room recovered was answered %d %q, want 503", answer.status, answer.body)
	}
	if answer.retryAfter != admission.RetryAfter {
		t.Errorf("the refused socket's Retry-After = %q, want %q", answer.retryAfter, admission.RetryAfter)
	}
	line := logs.Lines("a document socket was refused")
	if line == "" {
		t.Fatalf("the refused socket logged no WARN line naming why; the log was:\n%s", logs.String())
	}
	for _, want := range []string{"its failed room did not recover while it waited", "room=" + artifactID, "error=deadline_passed", "cause=pool_busy"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refused socket's line %q does not carry %q", line, want)
		}
	}
	if strings.Contains(line, "re-resolve the socket's caller") {
		t.Errorf("the refused socket's line %q carries its cause's text, want only the cause's class", line)
	}
}

// A document socket's admission reads - its caller's session, its document's issue, the live
// server processes - hold one place of the request pool's work outside admission
// (admission.Outside) for their whole length, and give it back once the last has run, before the
// document's load, which reads the rooms pool and decodes and renders for as long as the document
// takes. A socket that finds every place held waits for one, and is admitted when one comes free
// within its wait; one no place comes free for is refused once its second has run out, 503 with
// Retry-After and a WARN line, having read nothing of the request pool. A socket admitted holds its
// place while it reads, none while its document loads - the admission's read and render of it, and
// its room's load - and none once its socket is open. The places are the limiter's the API admits
// through, which cmd/dispatch hands the service (Deps.Outside).
func TestASocketsAdmissionReadsHoldAPlaceOutsideAdmission(t *testing.T) {
	logs := captureWarnings(t)
	database := storetest.Open(t)
	outside := admission.New(admission.DefaultLimits()).Outside()
	deps := testServiceDeps(database)
	deps.Settle = time.Hour
	deps.Outside = outside
	service := New(deps)
	testcleanup.Add(t, func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	server := newTestServer(t, service)
	artifactID := createDocuments(t, database, 1)[0]
	for range admission.OutsideLimit {
		if err := outside.Take(context.Background(), admission.HeartbeatCheck); err != nil {
			t.Fatalf("take a place outside admission: %v", err)
		}
	}
	before := poolAcquires(service, store.RequestsPool)
	asked := time.Now()
	answer := <-answerSocketAsync(server.URL, artifactID)
	waited := answer.at.Sub(asked)
	read := poolAcquires(service, store.RequestsPool) - before
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	t.Logf("with every place held the socket's handshake was answered %d %q after %v and %d request-pool reads", answer.status, answer.body, waited, read)
	if answer.status != http.StatusServiceUnavailable {
		t.Fatalf("a socket no place outside admission came free for was answered %d %q, want 503", answer.status, answer.body)
	}
	if waited < admission.DefaultReadWait {
		t.Errorf("the socket was refused %v after it asked, want it to wait its %v for a place first", waited, admission.DefaultReadWait)
	}
	if answer.retryAfter != admission.RetryAfter {
		t.Errorf("the refused socket's Retry-After = %q, want %q", answer.retryAfter, admission.RetryAfter)
	}
	if read != 0 {
		t.Errorf("the refused socket took %d request-pool connections, want none: it is refused before its first read", read)
	}
	if refused := outside.Stats().Refused[admission.SocketAdmission]; refused != (admission.OutsideRefusals{WaitExceeded: 1}) {
		t.Errorf("the places outside admission counted %+v refused socket admissions, want one whose wait ran out", refused)
	}
	line := logs.Lines("a document socket was refused")
	if line == "" {
		t.Fatalf("the refused socket logged no WARN line naming why; the log was:\n%s", logs.String())
	}
	for _, want := range []string{"places outside admission are all held", "room=" + artifactID, "error=" + string(admission.WaitExceeded)} {
		if !strings.Contains(line, want) {
			t.Errorf("the refused socket's line %q does not carry %q", line, want)
		}
	}

	for range admission.OutsideLimit {
		outside.Release()
	}

	// Each figure is the places held at one moment of the admitted socket's open, -1 until it
	// comes.
	var whileReading, whileAdmissionLoads, whileRoomLoads atomic.Int64
	whileReading.Store(-1)
	whileAdmissionLoads.Store(-1)
	whileRoomLoads.Store(-1)
	service.beforeSocketFleetRead = func(room string) {
		if room == artifactID {
			whileReading.Store(int64(outside.Stats().InUse))
		}
	}
	service.admissionRendered = func(room string) {
		if room == artifactID {
			whileAdmissionLoads.CompareAndSwap(-1, int64(outside.Stats().InUse))
		}
	}
	service.bootstrapping = func(room string, _ []byte) {
		if room == artifactID {
			whileRoomLoads.CompareAndSwap(-1, int64(outside.Stats().InUse))
		}
	}
	admitSocket(t, server.URL, artifactID, "a socket with places free")
	if got := whileReading.Load(); got != 1 {
		t.Errorf("the places held while the admitted socket read the live server processes = %d, want 1: its own", got)
	}
	if got := whileAdmissionLoads.Load(); got != 0 {
		t.Errorf("the places held while the admitted socket's admission loaded and rendered its document = %d, want 0: that load reads no request pool", got)
	}
	if got := whileRoomLoads.Load(); got != 0 {
		t.Errorf("the places held while the admitted socket's room loaded = %d, want 0", got)
	}
	if got := outside.Stats().InUse; got != 0 {
		t.Errorf("the places held once the admitted socket was handed to ygo = %d, want 0: the socket's reads gave theirs back while it stays open", got)
	}

	// A place that comes free while a socket waits is that socket's, which it gives back once its
	// reads have run.
	for range admission.OutsideLimit {
		if err := outside.Take(context.Background(), admission.HeartbeatCheck); err != nil {
			t.Fatalf("take a place outside admission: %v", err)
		}
	}
	waiting := answerSocketAsync(server.URL, artifactID)
	deadline := time.Now().Add(admission.DefaultReadWait / 2)
	for outside.Stats().Queued != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("no socket waited for a place within %v of asking", admission.DefaultReadWait/2)
		}
		time.Sleep(time.Millisecond)
	}
	outside.Release()
	if served := <-waiting; served.err != nil || served.status != http.StatusSwitchingProtocols {
		t.Fatalf("a socket a place came free for while it waited was answered %d %q (%v), want it admitted", served.status, served.body, served.err)
	}
	for range admission.OutsideLimit - 1 {
		outside.Release()
	}
	if got := outside.Stats().InUse; got != 0 {
		t.Fatalf("%d places held once the test gave its own back, want none: the admitted socket kept its own", got)
	}
}

// A socket whose request presents no sign-in the identity service could resolve - no cookie, or a
// cookie whose signature does not verify - is refused 401 at once and takes no place of the
// request pool's share outside admission (identity.Presents): nothing it could read would admit
// it. Every place is held as it arrives, so a socket that asked for one would wait its second and
// be refused 503 for want of a place, as such a socket was before its cookie was checked first.
func TestASocketPresentingNoSignInIsRefused401BeforeItTakesAPlace(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	outside := admission.New(admission.DefaultLimits()).Outside()
	deps := testServiceDeps(database)
	deps.Settle = time.Hour
	deps.Outside = outside
	deps.Identity = identity.CookieIdentity{SigningKey: testSigningKey, Sessions: store.NewPgSessionStore(database.Pool)}
	service := New(deps)
	testcleanup.Add(t, func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	server := newTestServer(t, service)
	artifactID := createDocuments(t, database, 1)[0]
	for range admission.OutsideLimit {
		if err := outside.Take(context.Background(), admission.HeartbeatCheck); err != nil {
			t.Fatalf("take a place outside admission: %v", err)
		}
	}
	testcleanup.Add(t, func() {
		for range admission.OutsideLimit {
			outside.Release()
		}
	})
	forged, _, _ := strings.Cut(auth.IssueSessionCookie("ada.lovelace@example.com", 1, auth.SingleSigningKey("another-deployments-key"), false), ";")

	for _, row := range []struct {
		name   string
		header http.Header
	}{
		{"no cookie", nil},
		{"a cookie another key signed", http.Header{"Cookie": []string{forged}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			dialer := gws.Dialer{HandshakeTimeout: time.Minute}
			connection, response, err := dialer.Dial(socketURL(server.URL, artifactID), row.header)
			if connection != nil {
				_ = connection.Close()
			}
			if response == nil || response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("a socket presenting %s with every place held was answered %v (%v), want 401", row.name, response, err)
			}
		})
	}
	if stats := outside.Stats(); stats.MaxQueued != 0 || stats.Refused[admission.SocketAdmission] != (admission.OutsideRefusals{}) {
		t.Errorf("sockets presenting no sign-in waited for a place (the longest queue %d) and were refused %+v, want neither: they take none",
			stats.MaxQueued, stats.Refused[admission.SocketAdmission])
	}
}

// A socket whose read of its caller's session finds the request pool busy is refused by its
// admission, 503 with Retry-After and a WARN line naming the read and the pool, where it was 401
// "unauthorized": a busy pool told a signed-in browser that it was not signed in. A caller the read
// finds not signed in is still 401. The caller is read on the request pool, as production reads
// its sign-in cookie's session there (cmd/dispatch's store.NewPgSessionStore on database.Pool), and
// every connection of that pool is held as the socket opens.
func TestASocketWhoseSessionReadFindsTheRequestPoolBusyIsRefused503(t *testing.T) {
	logs := captureWarnings(t)
	database := storetest.Open(t)
	_, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]

	connection, unsigned, err := documentDialer(t).Dial(socketURL(server.URL, artifactID), nil)
	if connection != nil {
		_ = connection.Close()
	}
	if unsigned == nil || unsigned.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a socket that names no caller was answered %v (%v), want 401", unsigned, err)
	}

	holdPool(t, database.Pool, store.RequestsPool, database.Pool)
	answer := <-answerSocketAsync(server.URL, artifactID)
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	t.Logf("the socket's handshake was answered %d %q", answer.status, answer.body)
	if answer.status != http.StatusServiceUnavailable {
		t.Fatalf("a socket whose session read found the request pool busy was answered %d %q, want 503", answer.status, answer.body)
	}
	if answer.retryAfter != admission.RetryAfter {
		t.Errorf("the refused socket's Retry-After = %q, want %q", answer.retryAfter, admission.RetryAfter)
	}
	line := logs.Lines("a document socket was refused")
	if line == "" {
		t.Fatalf("the refused socket logged no WARN line naming why; the log was:\n%s", logs.String())
	}
	for _, want := range []string{"its caller's session could not be read", "room=" + artifactID, "pool=" + store.RequestsPool, "error=pool_busy"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refused socket's line %q does not carry %q", line, want)
		}
	}
}

// lostSessionIdentity resolves no caller: its read of the caller's session loses its connection
// under it, and fails as the store names such a read - a statement the request pool ran outside a
// transaction, whose session Postgres ended after it was sent (store.ErrStatementOutcomeUnknown).
type lostSessionIdentity struct{}

func (lostSessionIdentity) Login(*http.Request) (string, error) {
	ended := &pgconn.PgError{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"}
	return "", fmt.Errorf("read session generation: %s pool: %w: the connection ended under it: %w", store.RequestsPool, store.ErrStatementOutcomeUnknown, ended)
}

// A socket whose read of its caller's session loses its connection under it is refused by its
// admission, 503 with Retry-After and a WARN line naming the read, the pool and the lost
// connection, where it was 401 "unauthorized": the read commits nothing, so a read that lost its
// connection says nothing of whether the caller is signed in (store.UnservedRead), though the
// store names a statement outside a transaction whose answer never came an outcome nobody knows.
func TestASocketWhoseSessionReadLosesItsConnectionIsRefused503(t *testing.T) {
	logs := captureWarnings(t)
	database := storetest.Open(t)
	deps := testServiceDeps(database)
	deps.Settle = time.Hour
	deps.Identity = lostSessionIdentity{}
	service := New(deps)
	testcleanup.Add(t, func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	server := newTestServer(t, service)
	artifactID := createDocuments(t, database, 1)[0]

	answer := <-answerSocketAsync(server.URL, artifactID)
	if answer.err != nil {
		t.Fatal(answer.err)
	}
	t.Logf("the socket's handshake was answered %d %q", answer.status, answer.body)
	if answer.status != http.StatusServiceUnavailable {
		t.Fatalf("a socket whose session read lost its connection was answered %d %q, want 503", answer.status, answer.body)
	}
	if answer.retryAfter != admission.RetryAfter {
		t.Errorf("the refused socket's Retry-After = %q, want %q", answer.retryAfter, admission.RetryAfter)
	}
	line := logs.Lines("a document socket was refused")
	if line == "" {
		t.Fatalf("the refused socket logged no WARN line naming why; the log was:\n%s", logs.String())
	}
	for _, want := range []string{"its caller's session could not be read", "room=" + artifactID, "pool=" + store.RequestsPool, "error=connection_lost"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refused socket's line %q does not carry %q", line, want)
		}
	}
}

// A refused socket's WARN line names no person. The socket's caller is a person's signed and
// current sign-in cookie, whose session is read on the request pool as production reads it
// (store.PgSessionStore, identity.CookieIdentity); every connection of that pool is held, so the
// read fails with the store's own error, which quotes the login it read for. The line carries that
// error's class with the room, the read and the pool, and neither the login nor the cookie's
// session (refuseUnservedSocket): the access-log line is what names the caller.
func TestARefusedSocketsWarnLineNamesNoPerson(t *testing.T) {
	logs := captureWarnings(t)
	database := storetest.Open(t)
	sessions := store.NewPgSessionStore(database.Pool)
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions},
		ServerURL: "https://dispatch.example",
		Settle:    time.Hour,
	})
	testcleanup.Add(t, func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	server := newTestServer(t, service)
	artifactID := createDocuments(t, database, 1)[0]
	const login = "ada.lovelace@example.com"
	generation, err := sessions.EnsureSession(context.Background(), login)
	if err != nil {
		t.Fatalf("open %s's session: %v", login, err)
	}
	cookie, _, _ := strings.Cut(auth.IssueSessionCookie(login, generation, testSigningKey, false), ";")
	_, session, _ := strings.Cut(cookie, "=")

	holdPool(t, database.Pool, store.RequestsPool, database.Pool)
	dialer := gws.Dialer{HandshakeTimeout: time.Minute}
	connection, response, err := dialer.Dial(socketURL(server.URL, artifactID), http.Header{"Cookie": []string{cookie}})
	if connection != nil {
		_ = connection.Close()
	}
	if response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a signed-in socket whose session read found the request pool busy was answered %v (%v), want 503", response, err)
	}
	line := logs.Lines("a document socket was refused")
	if line == "" {
		t.Fatalf("the refused socket logged no WARN line naming why; the log was:\n%s", logs.String())
	}
	t.Logf("the refused socket's line: %s", strings.TrimSpace(line))
	for _, want := range []string{"its caller's session could not be read", "room=" + artifactID, "pool=" + store.RequestsPool, "error=pool_busy"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refused socket's line %q does not carry %q", line, want)
		}
	}
	for _, person := range []string{login, base64.RawURLEncoding.EncodeToString([]byte(login)), session} {
		if strings.Contains(line, person) {
			t.Errorf("the refused socket's line %q carries %q, which names the person whose socket it was", line, person)
		}
	}
}

// takeTheRoomsPool takes every connection of service's rooms pool (takePool). It runs in a hook on
// the server's goroutine, where a failure can only be reported.
func takeTheRoomsPool(t *testing.T, service *Service) *heldPool {
	rooms, err := service.store.Pool.Rooms()
	if err != nil {
		t.Errorf("open the rooms pool: %v", err)
		return &heldPool{at: time.Now()}
	}
	held, err := takePool(t, rooms, store.RoomsPool, service.store.Pool)
	if err != nil {
		t.Error(err)
	}
	return held
}

// poolAcquires is how many connections service's pool named name has handed out since it opened,
// 0 for a pool not open. It fails no test, so a hook on the server's goroutine may read it too;
// roomsAcquires is the test goroutine's, which fails the test for a rooms pool not open.
func poolAcquires(service *Service, name string) uint64 {
	return poolAcquireStats(service, name).Count
}

// poolAcquireStats is what the acquire tracer of service's pool named name has counted since the
// pool opened (store.AcquireStats), nothing for a pool not open: the acquires waiting for a
// connection now, and the ones that ran out the pool's acquire bound.
func poolAcquireStats(service *Service, name string) store.AcquireStats {
	for _, pool := range service.store.Pool.Stats() {
		if pool.Name == name {
			return pool.Acquire
		}
	}
	return store.AcquireStats{}
}

// waitForRoomLoad returns a wait, for a hook on the server's goroutine, that returns once a load of
// room has begun - its record stands under the room's name (beginRoomLoad) - or within wait with
// why it did not.
func waitForRoomLoad(service *Service, room string, wait time.Duration) func() error {
	return func() error {
		if !testwait.Holds(wait, time.Millisecond, func() bool {
			_, loading := service.roomLoads.Load(room)
			return loading
		}) {
			return errors.New("the settlement's warm-up never began its load of the room")
		}
		return nil
	}
}

// warmUpAsASettlementDoes loads room as a settlement's warm-up loads the room it is about to settle
// (settleRoom): an Apply that applies nothing, under the service's own mark and on the settlements
// pool, which no socket's admission holds.
func warmUpAsASettlementDoes(service *Service, room string) error {
	settlements, err := service.store.Pool.Settlements()
	if err != nil {
		return err
	}
	ctx := withOwnerVerified(withQueryPool(context.Background(), settlements))
	err = service.srv.Apply(ctx, room, func(*crdt.Doc, func(func(*crdt.Transaction))) {})
	if errors.Is(err, websocket.ErrNoChanges) {
		return nil
	}
	return err
}

// socketAnswer is how a document socket's handshake was answered, when, or why it had no answer.
type socketAnswer struct {
	status     int
	retryAfter string
	body       string
	at         time.Time
	err        error
}

// answerSocketAsync dials id's document socket as alice's browser does, on a goroutine of its own,
// and answers on the channel it returns with how the handshake was answered. A socket it upgraded
// is closed at once.
func answerSocketAsync(serverURL, id string) <-chan socketAnswer {
	answers := make(chan socketAnswer, 1)
	go func() {
		dialer := gws.Dialer{HandshakeTimeout: time.Minute}
		connection, response, err := dialer.Dial(socketURL(serverURL, id), http.Header{"X-Dispatch-User": []string{"alice"}})
		at := time.Now()
		if connection != nil {
			_ = connection.Close()
		}
		if response == nil {
			answers <- socketAnswer{at: at, err: fmt.Errorf("the socket's handshake had no answer: %w", err)}
			return
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		answers <- socketAnswer{
			status:     response.StatusCode,
			retryAfter: response.Header.Get("Retry-After"),
			body:       string(body),
			at:         at,
		}
	}()
	return answers
}
