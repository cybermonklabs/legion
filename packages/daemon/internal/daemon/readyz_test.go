package daemon

// The daemon saying when it is serving (INFRA-3169, R5): a whole daemon over a pool of one
// connection, read on real HTTP at the two moments that decide the probe — while it serves HTTP
// and has not latched, and once it has.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// GET /readyz is 503 while the daemon has not reached serving and 200 from the moment it has, and
// it reads no database on the way.
//
// The daemon is held at the moment that decides it: the seam the boot offers a test
// (overrides.beforeServingLatched) runs once the goroutine that serves the API has begun and
// before the readiness latch is set, so the daemon answers HTTP — it has reported itself ready to
// serve, which is what startDaemon waited for, before the seam runs — while /readyz must still
// refuse, naming what it waits for. Released, the latch is set and the same read is 200.
//
// That 200 is answered while the daemon's pool has no free connection, which is what proves the
// route reads no database. The daemon's DSN gives its pool one connection and nothing else shares
// the application name (daemonDSN), the test's own transaction holds an access exclusive lock on
// `claims` — a table the state projection reads (source.State's ClaimsTx) — and a
// GET /legion/v1/state sent from a goroutine waits for that lock inside the transaction the route
// holds that connection with. The test reads pg_stat_activity on its own pool to see the daemon's
// one backend waiting on a lock before it asks, and checks that the state request is still
// unanswered after, so the connection was busy for the whole of the readiness read. A route that
// touched the pool would wait there with everything else, and the test's client bounds every
// request at ten seconds (startDaemon), so such a regression fails this test rather than hanging
// the package.
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

	held := testwait.Context(t)
	testwait.Receive(t, "the daemon reaching the seam before its readiness latch", serving)
	if status, body := d.request(http.MethodGet, "/readyz", nil, false); status != http.StatusServiceUnavailable || !strings.Contains(string(body), "the daemon to serve") {
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
		return daemonBackends(t, pool, application, backendWaitingOnALock) == 1
	})

	close(release)
	// The subject here is /readyz's own answer once the latch is released, so the answer is what
	// the test reads.
	testwait.Until(t, "GET /readyz to answer 200 once the daemon's latch is released", func() (string, bool) {
		status, body := d.request(http.MethodGet, "/readyz", nil, false)
		return fmt.Sprintf("%d %s", status, body), status == http.StatusOK
	})
	select {
	case answered := <-state:
		t.Fatalf("the state request answered %d before the readiness read, so the daemon's connection was free for it", answered)
	default:
	}
	if waiting := daemonBackends(t, pool, application, backendWaitingOnALock); waiting != 1 {
		t.Fatalf("%d of the daemon's backends wait on the test's lock after the readiness read, want its one connection still held", waiting)
	}

	// The lock goes, and the request that held the connection finishes: nothing of this test is
	// left waiting on the daemon when it stops.
	if err := tx.Commit(held); err != nil {
		t.Fatalf("commit the locking transaction: %v", err)
	}
	if answered := testwait.Receive(t, "the state request's answer once the lock was released", state); answered != http.StatusOK {
		t.Fatalf("GET the state once the lock was released = %d, want 200", answered)
	}
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
