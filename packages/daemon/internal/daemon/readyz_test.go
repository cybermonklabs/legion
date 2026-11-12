package daemon

// The daemon saying when it is serving (INFRA-3169, R5): a whole daemon over a pool of one
// connection, read on real HTTP at the two moments that decide the probe — while it serves HTTP
// and has not latched, and once it has.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// GET /readyz is 503 while the daemon has not reached serving and 200 from the moment it has, and
// it reads no database on the way.
//
// The daemon is held at the moment that decides it: the seam the boot offers a test
// (overrides.beforeServingLatched) runs once the goroutine that serves the API has begun and
// before the readiness latch is set, so the daemon answers HTTP — /healthz is 200, which is what
// startDaemon waited for — while /readyz must still refuse, naming what it waits for. Released,
// the latch is set and the same read is 200.
//
// That 200 is answered while the daemon's pool has no free connection, which is what proves the
// route reads no database. The daemon's DSN gives its pool one connection and nothing else shares
// the application name (daemonDSN), the test's own transaction holds an access exclusive lock on
// `claims` — a table the state projection reads (source.State's ClaimsTx) — and a
// GET /legion/v1/state sent from a goroutine waits for that lock inside the transaction the route
// holds that connection with. The test reads pg_stat_activity on its own pool to see the daemon's
// one backend waiting on a lock before it asks, and checks that the state request is still
// unanswered after, so the connection was busy for the whole of the readiness read. A route that
// touched the pool would wait there with everything else, and each read carries a deadline of its
// own, so such a regression fails this test rather than hanging the package.
//
// On this change's base the same read answers 404 with the catch-all body that names no route
// (`{"error":"no route"}`): the mux has no /readyz at all.
func TestReadyzIsFalseUntilTheDaemonServes(t *testing.T) {
	// A database of this test's own and a pool of its own on it: the lock below and the reads of
	// pg_stat_activity go over a connection the daemon neither owns nor can wait for.
	pool := isolatedOutboxPool(t)
	cfg := testConfig(t)
	application := "legion-daemon-" + strings.ToLower(randomSuffix(t))
	cfg.PostgresDSN = daemonDSN(t, pool, application)

	o := fakeRuntime(fake.NewRuntime(), &built{})
	serving, release := make(chan struct{}), make(chan struct{})
	o.beforeServingLatched = func(ctx context.Context) {
		close(serving)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	d := startDaemon(t, cfg, o)

	held, cancelHeld := context.WithTimeout(context.Background(), testwait.Bounded(t, 30*time.Second, time.Second))
	defer cancelHeld()
	select {
	case <-serving:
	case <-held.Done():
		t.Fatalf("the daemon answered /healthz but never reached the seam before its latch: %v", held.Err())
	}
	if status, body := readReadyz(t, d); status != http.StatusServiceUnavailable || !strings.Contains(body, "the daemon to serve") {
		t.Fatalf("GET /readyz while the daemon has not latched = %d %s, want 503 naming the serving it waits for", status, body)
	}

	// The daemon's one connection is made busy and held there, before the latch is released: the
	// lock is taken first, then the state request, which the lock stops inside the route's own
	// transaction.
	tx, err := pool.Begin(held)
	if err != nil {
		t.Fatalf("begin the locking transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(held, "lock table claims in access exclusive mode"); err != nil {
		t.Fatalf("lock the claims table against the daemon: %v", err)
	}
	state := make(chan int, 1)
	go func() { state <- getStatus(held, d, "/legion/v1/state") }()
	testwait.Eventually(t, "the daemon's one connection to wait on the test's lock", func() bool {
		return lockedBackends(t, pool, application) == 1
	})

	close(release)
	var status int
	var body string
	if !testwait.Holds(testwait.Bounded(t, 10*time.Second, time.Second), 20*time.Millisecond, func() bool {
		status, body = readReadyz(t, d)
		return status == http.StatusOK
	}) {
		t.Fatalf("GET /readyz once the daemon serves = %d %s, want 200", status, body)
	}
	select {
	case answered := <-state:
		t.Fatalf("the state request answered %d before the readiness read, so the daemon's connection was free for it", answered)
	default:
	}
	if waiting := lockedBackends(t, pool, application); waiting != 1 {
		t.Fatalf("%d of the daemon's backends wait on the test's lock after the readiness read, want its one connection still held", waiting)
	}

	// The lock goes, and the request that held the connection finishes: nothing of this test is
	// left waiting on the daemon when it stops.
	if err := tx.Commit(held); err != nil {
		t.Fatalf("commit the locking transaction: %v", err)
	}
	select {
	case answered := <-state:
		if answered != http.StatusOK {
			t.Fatalf("GET the state once the lock was released = %d, want 200", answered)
		}
	case <-held.Done():
		t.Fatalf("the state request never answered after the lock was released: %v", held.Err())
	}
}

// readReadyz is one GET /readyz with the status and the body it answered, on a deadline of the
// read's own: a route that waited on the daemon's pool would otherwise hang the package until the
// binary's timeout, where this fails the test where it is measured.
func readReadyz(t *testing.T, d *daemon) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testwait.Bounded(t, 10*time.Second, time.Second))
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/readyz", nil)
	if err != nil {
		t.Fatalf("build the readiness request: %v", err)
	}
	response, err := d.client.Do(request)
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the readiness body: %v", err)
	}
	return response.StatusCode, strings.TrimSpace(string(body))
}

// getStatus is the status of one GET of path, and 0 for a request that never answered: it runs on
// a goroutine of the test's, which may fail nothing itself, and what the caller reads is whether
// the request is still in flight at all.
func getStatus(ctx context.Context, d *daemon, path string) int {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return 0
	}
	response, err := d.client.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

// lockedBackends is how many of the daemon's backends wait on a lock now, read on the test's own
// connection by the application name its DSN carries. The daemon's pool holds one connection at
// most (daemonDSN's pool_max_conns=1), so one such backend is a pool with no free connection: a
// read the daemon makes from then on waits for the test's transaction to end.
func lockedBackends(t *testing.T, pool *store.Pool, application string) int {
	t.Helper()
	var waiting int
	if err := pool.QueryRow(context.Background(),
		"select count(*) from pg_stat_activity where application_name = $1 and wait_event_type = 'Lock'",
		application).Scan(&waiting); err != nil {
		t.Fatalf("read what the backends of %s wait on: %v", application, err)
	}
	return waiting
}
