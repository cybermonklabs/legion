package docs

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// A read of a document takes no connection of the rooms pool, whether a room holds the document or
// none does: it reads through the pool its own caller reads through (readQuerier), and its fold of
// a stored history takes a request-pool connection of its own (foldContext). The rooms pool is for
// the loads a request's connection cannot serve - ygo's room load, which no context reaches, and a
// write's cold fork, which is inside its transaction - and a read that queued on it put every room
// load, a settlement's warm-up among them, behind the reads of requests (INFRA-592).
//
// Each read runs with every connection of the rooms pool held and that pool's acquire bound
// shortened, so a read that asked for one is refused ErrPoolBusy within the test rather than after
// production's three seconds, and with the pool's acquires counted before and after, so a read
// that took one is caught even where the pool has a connection free.
func TestADocumentIsReadWhileTheRoomsPoolIsFull(t *testing.T) {
	t.Parallel()
	t.Run("the room is resident", func(t *testing.T) {
		service, artifactID := newTestService(t)
		service.settle = time.Hour
		shortenRoomsAcquireBound(service)
		seedServiceText(t, service, artifactID, "before\n")
		// A browser's edit leaves the room resident, and the settlement after it leaves the
		// room's cursor at the stored head, which is what a read of the live room reads.
		editLiveTree(t, service, artifactID, replaceRun("before", "after"))
		waitForLandedAppends(t, service, artifactID)
		settleCurrentGeneration(t, service, artifactID)
		if service.srv.GetDoc(artifactID) == nil {
			t.Fatal("the document's room is gone, want it resident for a read of the live room")
		}
		release := holdTheRoomsPool(t, service)
		defer release()

		acquires := roomsAcquires(t, service)
		text, err := service.Text(context.Background(), artifactID)
		if err != nil {
			t.Fatalf("read a resident document with the rooms pool full: %v", err)
		}
		if text != "after\n" {
			t.Fatalf("read = %q, want %q", text, "after\n")
		}
		if took := roomsAcquires(t, service) - acquires; took != 0 {
			t.Fatalf("the read took %d rooms-pool connections, want none", took)
		}
	})

	t.Run("no room holds the document", func(t *testing.T) {
		database := storetest.Open(t)
		versioned := NewPgVersioned(database)
		artifactID := createDocument(t, database, "cold")
		writer := newProofHistory(t, 101)
		appendUpdates(t, versioned, artifactID, writer.write("cold\n"))
		service := New(Deps{
			Store:       database,
			Persistence: versioned,
			Events:      events.NewBroker(),
			Identity:    headerIdentity(database),
			ServerURL:   "https://dispatch.example",
			Settle:      time.Hour,
		})
		testcleanup.Add(t, func() {
			if err := service.Shutdown(context.Background()); err != nil {
				t.Errorf("shutdown document service: %v", err)
			}
		})
		if service.srv.GetDoc(artifactID) != nil {
			t.Fatal("the document is resident, want a cold read")
		}
		shortenRoomsAcquireBound(service)
		release := holdTheRoomsPool(t, service)
		defer release()

		acquires := roomsAcquires(t, service)
		text, err := service.Text(context.Background(), artifactID)
		if err != nil {
			t.Fatalf("read a document no room holds with the rooms pool full: %v", err)
		}
		if text != "cold\n" {
			t.Fatalf("read = %q, want %q", text, "cold\n")
		}
		if took := roomsAcquires(t, service) - acquires; took != 0 {
			t.Fatalf("the cold read took %d rooms-pool connections, want none", took)
		}
		if service.srv.GetDoc(artifactID) != nil {
			t.Fatal("the cold read loaded the document's room")
		}
	})
}

// shortenRoomsAcquireBound makes an acquire of the rooms pool that waits be refused inside the
// test rather than after production's three seconds. The pool takes the bound when it opens, so
// every caller sets it before the first load.
func shortenRoomsAcquireBound(service *Service) {
	service.store.Pool.SetAcquireBoundForTest(store.RoomsPool, 200*time.Millisecond)
}

// holdTheRoomsPool takes every connection of the rooms pool (holdPool) and returns their release.
func holdTheRoomsPool(t *testing.T, service *Service) func() {
	t.Helper()
	rooms, err := service.store.Pool.Rooms()
	if err != nil {
		t.Fatalf("open the rooms pool: %v", err)
	}
	return holdPool(t, rooms, store.RoomsPool, service.store.Pool).release
}

// pooledConnections is a pool a test holds every connection of (takePool): the request pool or a
// wrapped pool (store.Pool), or a raw one (store.RawPool).
type pooledConnections interface {
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
	Stat() *pgxpool.Stat
}

// heldPool is the connections of one pool a test took (takePool), and the moment it had taken the
// last of them or gave up. Its release gives each connection back once, however many times it
// runs - the test's own call and the cleanup takePool registers both run it - and a connection
// added once it has run goes back at once.
type heldPool struct {
	at       time.Time
	mu       sync.Mutex
	conns    []*pgxpool.Conn
	released bool
}

// add holds conn, and is whether it does: once the release has run, conn goes back at once.
func (held *heldPool) add(conn *pgxpool.Conn) bool {
	held.mu.Lock()
	defer held.mu.Unlock()
	if held.released {
		conn.Release()
		return false
	}
	held.conns = append(held.conns, conn)
	return true
}

// take hands over the connection taken last, nil when none is held, for its caller to give back
// (releaseOne) or to hold again (add); the release does not reach it meanwhile.
func (held *heldPool) take() *pgxpool.Conn {
	held.mu.Lock()
	defer held.mu.Unlock()
	last := len(held.conns) - 1
	if last < 0 {
		return nil
	}
	conn := held.conns[last]
	held.conns = held.conns[:last]
	return conn
}

// releaseOne gives back the connection taken last, for a test that frees one while it holds the
// rest.
func (held *heldPool) releaseOne() {
	if conn := held.take(); conn != nil {
		conn.Release()
	}
}

// release gives back every connection held.
func (held *heldPool) release() {
	held.mu.Lock()
	conns := held.conns
	held.conns, held.released = nil, true
	held.mu.Unlock()
	for _, conn := range conns {
		conn.Release()
	}
}

// count is how many connections are held.
func (held *heldPool) count() int {
	held.mu.Lock()
	defer held.mu.Unlock()
	return len(held.conns)
}

// pids is the server process of each connection held.
func (held *heldPool) pids() map[int32]bool {
	held.mu.Lock()
	defer held.mu.Unlock()
	pids := make(map[int32]bool, len(held.conns))
	for _, conn := range held.conns {
		pids[int32(conn.Conn().PgConn().PID())] = true
	}
	return pids
}

// holdPool is takePool on the test's own goroutine: it fails the test where takePool could not take
// every connection, whose release takePool registered among t's cleanups before it took the first.
func holdPool(t *testing.T, pool pooledConnections, name string, requests *store.Pool) *heldPool {
	t.Helper()
	held, err := takePool(t, pool, name, requests)
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// holdLimit is how long takePool keeps asking for a connection the pool is still making.
const holdLimit = 30 * time.Second

// takePool takes every connection of pool, the pool named name, and is what it took, with why it
// could not take the rest. Before it takes the first it registers their release among t's
// cleanups, which run before the pool's own Close (storetest.Open registered that first), so a
// connection taken goes back on every path: a pool closing with one still out waits for it without
// end. It fails no test itself, so a hook on the server's goroutine may call it and report its error.
//
// Each acquire waits under the pool's acquire bound, which a test shortens for the read it measures,
// and an acquire that finds no connection idle makes one inside that bound: a dial and the new
// connection's setup (store's connected). The read under test is what meets the shortened bound,
// not the hold it is measured under, so an acquire whose bound ran out while no other caller held a
// connection of the pool, and the pool was making one or had one idle, is asked again: the pool
// carries a connection its acquire gave up on through to an idle one (puddle's initResourceValue),
// which the next acquire takes. takePool asks again for up to holdLimit in all. Any other failure -
// a connection another caller holds, a pool making nothing, an error that is not the bound, or
// holdLimit spent - is takePool's error, which names what held the pool (poolHolders); requests, the
// request pool, is what the pool's sessions are read through when takePool holds none of its
// connections to read them on.
func takePool(t *testing.T, pool pooledConnections, name string, requests *store.Pool) (*heldPool, error) {
	held := &heldPool{}
	testcleanup.Add(t, held.release)
	size := int(pool.Stat().MaxConns())
	until := time.Now().Add(holdLimit)
	for taken := 0; taken < size; {
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			stat := pool.Stat()
			if store.IsPoolBusy(err) && int(stat.AcquiredConns()) <= taken &&
				stat.ConstructingConns()+stat.IdleConns() > 0 && time.Now().Before(until) {
				t.Logf("hold a %s connection, %d of %d: %v, with no other holder and the pool making one (constructing=%d idle=%d); asking again",
					name, taken+1, size, err, stat.ConstructingConns(), stat.IdleConns())
				continue
			}
			held.at = time.Now()
			return held, fmt.Errorf("hold a %s connection, %d of %d: %w; %s", name, taken+1, size, err, poolHolders(stat, name, held, requests))
		}
		if !held.add(conn) {
			held.at = time.Now()
			return held, fmt.Errorf("hold a %s connection, %d of %d: the test's cleanup gave back what was taken", name, taken+1, size)
		}
		taken++
	}
	held.at = time.Now()
	return held, nil
}

// poolHolders names what held the pool named name, as stat counted it when an acquire of takePool's
// failed: another caller holding one of its connections, or the pool still making one - each a
// cause of its own - with the pool's counts and its sessions (poolSessions).
func poolHolders(stat *pgxpool.Stat, name string, held *heldPool, requests *store.Pool) string {
	holding := held.count()
	var cause string
	switch {
	case int(stat.AcquiredConns()) > holding:
		cause = fmt.Sprintf("another holder: %d acquired with the helper holding %d", stat.AcquiredConns(), holding)
	case stat.ConstructingConns() > 0:
		cause = fmt.Sprintf("a connection was still being made: constructing=%d", stat.ConstructingConns())
	default:
		cause = fmt.Sprintf("no other holder and no connection being made: %d acquired with the helper holding %d", stat.AcquiredConns(), holding)
	}
	return fmt.Sprintf("%s (acquired=%d constructing=%d idle=%d total=%d max=%d); the server's sessions of the %s pool: %s",
		cause, stat.AcquiredConns(), stat.ConstructingConns(), stat.IdleConns(), stat.TotalConns(), stat.MaxConns(), name,
		poolSessions(name, held, requests))
}

// poolSessionsQuery lists the sessions of the test's own database whose application_name ends in the
// name of the pool $1 names, as each pool names its connections (store's applicationName:
// dispatch-<pool>, after the connection string's own name and a slash where it names one).
const poolSessionsQuery = `
	select pid, format('pid=%s app=%s state=%s backend_start=%s xact_start=%s state_change=%s wait=%s/%s query=%s',
		pid, application_name, state, backend_start, xact_start, state_change, wait_event_type, wait_event,
		regexp_replace(left(query, 120), '\s+', ' ', 'g'))
	from pg_stat_activity
	where datname = current_database() and application_name like '%dispatch-' || $1
	order by backend_start`

// poolSessions is the server's sessions of the pool named name (poolSessionsQuery), each marked where
// the helper holds it, read on a connection the helper holds, which works whichever pool is full, or
// through requests where it holds none.
func poolSessions(name string, held *heldPool, requests *store.Pool) string {
	mine := held.pids()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var rows pgx.Rows
	var err error
	if conn := held.take(); conn != nil {
		defer held.add(conn)
		rows, err = conn.Query(ctx, poolSessionsQuery, name)
	} else {
		rows, err = requests.Query(ctx, poolSessionsQuery, name)
	}
	if err != nil {
		return "not read: " + err.Error()
	}
	defer rows.Close()
	var sessions []string
	for rows.Next() {
		var pid int32
		var session string
		if err := rows.Scan(&pid, &session); err != nil {
			return "not read: " + err.Error()
		}
		if mine[pid] {
			session += " (the helper's)"
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return "not read: " + err.Error()
	}
	if len(sessions) == 0 {
		return "none"
	}
	return "\n\t" + strings.Join(sessions, "\n\t")
}

// roomsAcquires is how many connections the rooms pool has handed out since it opened
// (poolAcquires). Its callers hold the whole pool first (holdTheRoomsPool), so a count of none is
// a pool that is not open, and the test fails rather than measure a read against it.
func roomsAcquires(t *testing.T, service *Service) uint64 {
	t.Helper()
	count := poolAcquires(service, store.RoomsPool)
	if count == 0 {
		t.Fatal("the rooms pool is not open")
	}
	return count
}
