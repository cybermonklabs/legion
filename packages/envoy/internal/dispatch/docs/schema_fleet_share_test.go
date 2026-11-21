package docs

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The rows here are about the read of the live server processes the document sockets' admissions
// share for a second (admissionFleet, fleetCacheTTL). They name the second as a literal, not that
// constant, so they also build against the code from before the share, where each of them fails.

// fleetShare is fleetCacheTTL, which these rows cannot name (above).
const fleetShare = time.Second

// registerFleetProcess writes the server_processes row of another server process straight into
// the registry, as fleetProcess does, and leaves the read of the fleet the service's sockets share
// as it was, for a row that watches that read itself.
func registerFleetProcess(t *testing.T, service *Service, version, creatable int, age time.Duration) {
	t.Helper()
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into server_processes (instance, signing_keys, schema_version, schema_creatable, heartbeat_at)
		values (gen_random_uuid(), '{fingerprint}', $1, $2, now() - make_interval(secs => $3))`,
		version, creatable, age.Seconds()); err != nil {
		t.Fatalf("add a server process reading schema version %d: %v", version, err)
	}
}

// movableClock sets service's clock to a moment the test moves itself, and returns how to move it:
// the clock the fleet's shared read is aged by, so a row sees that read's second pass without
// sleeping through it.
func movableClock(service *Service) func(by time.Duration) {
	started := time.Now()
	var moved atomic.Int64
	service.now = func() time.Time { return started.Add(time.Duration(moved.Load())) }
	return func(by time.Duration) { moved.Store(int64(by)) }
}

// A process that joins the fleet reaches a socket's admission once the read of the fleet the
// sockets share is a second old, and not before: a socket admitted within that second takes the
// answer read before the process registered. Here the process is of a build that predates the fleet
// columns, whose row counts as reading version 0, so no page is writable while it lives; this
// process's own current page is writable without it.
func TestASocketsAdmissionSeesAFleetChangeOnceTheSharedReadIsASecondOld(t *testing.T) {
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	moveTo := movableClock(service)
	var reads atomic.Int32
	service.beforeSocketFleetRead = func(string) { reads.Add(1) }
	current := page(pmdoc.SchemaVersion(), pmdoc.SchemaCreatable())
	token := strconv.Itoa(pmdoc.SchemaVersion())

	if socket, authenticated := admittedAs(t, server.URL, artifactID, current, token); !socket || !authenticated {
		t.Fatalf("a current page with no other process live was admitted writable=%t by its socket and %t by its token, want read-write", socket, authenticated)
	}
	registerFleetProcess(t, service, 0, 0, 0)

	moveTo(fleetShare - time.Millisecond)
	if socket, authenticated := admittedAs(t, server.URL, artifactID, current, token); !socket || !authenticated {
		t.Errorf("a socket admitted within the second of the fleet's last read was admitted writable=%t by its socket and %t by its token, want read-write: it shares that read, which the process registered since had not joined", socket, authenticated)
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("two sockets within one second read the live server processes %d times, want once", got)
	}

	moveTo(fleetShare)
	if socket, authenticated := admittedAs(t, server.URL, artifactID, current, token); socket || authenticated {
		t.Errorf("a socket admitted once the fleet's last read was a second old was admitted writable=%t by its socket and %t by its token, want read-only: a process that reads version 0 has joined", socket, authenticated)
	}
	if got := reads.Load(); got != 2 {
		t.Errorf("the live server processes were read %d times once the first read was a second old, want twice", got)
	}
}

// Cold sockets that open at once share one read of the live server processes: the first to find
// no answer of the last second reads it, and the rest wait on that read rather than making their
// own. The first read is held until the others have had half a second to make theirs, which none
// of them may; the clock stands still, so the read the first makes is fresh for all of them.
func TestConcurrentColdSocketsShareOneFleetRead(t *testing.T) {
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	movableClock(service)
	var reads atomic.Int32
	reading := make(chan struct{})
	release := make(chan struct{})
	var released atomic.Bool
	letRead := func() {
		if !released.Swap(true) {
			close(release)
		}
	}
	t.Cleanup(letRead)
	service.beforeSocketFleetRead = func(string) {
		if reads.Add(1) == 1 {
			close(reading)
			<-release
		}
	}

	const sockets = 5
	opened := []<-chan openedSocket{dialSocketAsync(server.URL, artifactID)}
	select {
	case <-reading:
	case <-time.After(roomWait(t)):
		t.Fatal("the first socket never read the live server processes, so nothing here is tested")
	}
	for range sockets - 1 {
		opened = append(opened, dialSocketAsync(server.URL, artifactID))
	}
	if testwait.Holds(500*time.Millisecond, 5*time.Millisecond, func() bool { return reads.Load() > 1 }) {
		t.Errorf("a socket read the live server processes itself while the first socket's read of them was in flight: %d reads", reads.Load())
	}
	letRead()
	for i, answer := range opened {
		socket := <-answer
		if socket.err != nil {
			t.Errorf("socket %d was refused (%v), want it admitted", i+1, socket.err)
			continue
		}
		t.Cleanup(func() { _ = socket.connection.Close() })
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("%d cold sockets opened at once read the live server processes %d times, want once", sockets, got)
	}
}

// A read of the live server processes that fails is not kept: its socket is refused 503, as every
// socket's is whose read fails, and the next socket, within the same second, reads them again
// and is admitted as the fleet admits it. The first read finds every request-pool connection held,
// and the clock stands still, so a failure kept as an answer would be the second socket's too.
func TestAFailedFleetReadIsNotKept(t *testing.T) {
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	movableClock(service)
	held := make(chan []*pgxpool.Conn, 1)
	release := func() {
		select {
		case conns := <-held:
			for _, conn := range conns {
				conn.Release()
			}
		default:
		}
	}
	t.Cleanup(release)
	var reads atomic.Int32
	service.beforeSocketFleetRead = func(string) {
		if reads.Add(1) != 1 {
			return
		}
		var conns []*pgxpool.Conn
		for range database.Pool.Stat().MaxConns() {
			conn, err := database.Pool.Acquire(context.Background())
			if err != nil {
				t.Errorf("hold a request-pool connection: %v", err)
				break
			}
			conns = append(conns, conn)
		}
		held <- conns
	}

	refused := <-answerSocketAsync(server.URL, artifactID)
	release()
	if refused.err != nil {
		t.Fatal(refused.err)
	}
	if refused.status != http.StatusServiceUnavailable {
		t.Fatalf("a socket whose read of the live server processes found the request pool busy was answered %d %q, want 503", refused.status, refused.body)
	}

	current := page(pmdoc.SchemaVersion(), pmdoc.SchemaCreatable())
	if socket, authenticated := admittedAs(t, server.URL, artifactID, current, strconv.Itoa(pmdoc.SchemaVersion())); !socket || !authenticated {
		t.Errorf("the socket after a failed read of the live server processes was admitted writable=%t by its socket and %t by its token, want read-write: the failed read was kept as an answer", socket, authenticated)
	}
	if got := reads.Load(); got != 2 {
		t.Errorf("the live server processes were read %d times across a failed read and the socket after it, want twice", got)
	}
}
