package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/testlimits"
)

// budgetSampleEvery is how often the row below reads its relays' counts and pg_stat_activity.
const budgetSampleEvery = 100 * time.Millisecond

// budgetTakeOverWithin is what the row below gives the standby to bind both delivery durables once
// it holds both holds: the take-over's own lookups, deletes, subscribes, creates and token writes
// under each durable's consumer lease, which take well under a second unbudgeted, inside one
// takeOverFinishWithin (10 s, internal/dispatch/delivery) with a margin. A take-over that stalls on
// the lease pool never binds, so this is a hang guard.
const budgetTakeOverWithin = 15 * time.Second

// testlimits.MaxProcessesAtOnce Dispatch processes on one database and one NATS server, each with
// its pools scaled to the budget the smallest server a suite runs on gives it
// (testlimits.MinServerConnections plus the server's superuser reserve, where testlimits.PoolBudget
// gives store.MinBudget, every pool at its smallest), boot, serve load at once, hand both delivery
// intakes' durables over from a dead holder, and drain the outbox, and none opens more than that
// budget: read every 100 ms from before the first start until the outbox has drained, no process
// ever holds more connections to Postgres at once than its budget.
//
// Each process reaches Postgres through a relay of its own (connectionRelay), and a process's
// count is the connections its relay holds open to the server: each from the moment the relay's
// dial succeeds, before its first byte reaches Postgres, until both of its directions have closed.
// That counts a connection a little longer than its backend lives, which only makes the budget
// stricter: it never leaves out a connection the process holds. pg_stat_activity cannot say whose
// a backend is. Its client_port is the port the server sees, which behind a relay or a service
// container (CI's Postgres, reached through docker's proxy) is not the port the relay dialed from.
// Its application_name is not the process's either: Dispatch names some of its pools' connections
// itself, whatever the connection string sets - dispatch-rooms, dispatch-room-appends and
// dispatch-compaction, the same in every process (store.Pool.openSeparateLocked) - and its shared
// listener and room-sync sender by a process id it mints at boot (store.Pool.Dedicated,
// docs.ListenerApplicationPrefix). So pg_stat_activity gives the row only the database's total,
// which it logs. The processes together stay inside that server's connections less the reserve and
// the suite's own testlimits.SuiteHeadroom whenever each stays inside its budget, since PoolBudget
// gives each at most a MaxProcessesAtOnce-th of that, so the row counts them one by one.
//
// On a server that small the connection guard gives every process that budget itself
// (requireConnectionCeiling); on a larger one, as every CI lane's, the row gives it, so the claim is
// held wherever the suite runs. What reaches each pool:
//   - The load. Every process holds a live socket on the event feed and a document's room open in
//     an editor that types into it, and serves many more requests at once than its shared pool has
//     connections - issue reads, renames and document reads - each of which must succeed. The
//     processes' requests start together, released at once when every process is set up.
//   - The outbox. Whichever process holds it publishes the renames' events to the stream, every
//     one of them by the row's end.
//   - The intake take-over. Process 0 boots first, with the GitHub App, and binds both delivery
//     durables; process 1 has the App too and stands by for both; the other processes have none, so
//     their intakes are idle. Process 0 is killed, and while its holds lapse the row holds both
//     hold rows locked, so process 1's two takings of them (duty.TryHold) queue and are let go at
//     one moment, and its two take-overs run at once: each holds its consumer lease's connection
//     while its SetToken takes another, which is what the lease pool's minimum of three is argued
//     from (minimumPoolSizes, internal/dispatch/store/pool.go). Both must bind within
//     budgetTakeOverWithin of the release.
//
// None of the processes' logs may show the server out of connections (53300).
func TestEveryProcessOnTheSmallestServerASuiteRunsOnStaysWithinItsBudget(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	seedDeliverySettings(t, database)
	databaseURL := databaseURLOf(t, database)
	_, natsURL := multiprocessNATS(t)
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
	}))
	t.Cleanup(github.Close)
	app := githubAppEnv(t, github.URL)

	sampler, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect the sampler: %v", err)
	}
	t.Cleanup(func() { _ = sampler.Close(context.Background()) })
	reserve := serverInt(t, sampler, "superuser_reserved_connections")
	smallest := testlimits.MinServerConnections() + reserve
	budget, err := testlimits.PoolBudget(smallest, reserve)
	if err != nil {
		t.Fatalf("the budget of a %d-connection server: %v", smallest, err)
	}
	t.Logf("a %d-connection server with %d reserved gives each of %d processes %d connections",
		smallest, reserve, testlimits.MaxProcessesAtOnce, budget)

	relays := make([]*connectionRelay, testlimits.MaxProcessesAtOnce)
	for i := range relays {
		relays[i] = startConnectionRelay(t, databaseURL)
	}
	peaks := sampleConnections(t, sampler, relays)
	start := func(i int, extra map[string]string) *dispatchProcess {
		env := map[string]string{"DISPATCH_TEST_POOL_BUDGET": strconv.Itoa(budget)}
		maps.Copy(env, extra)
		return startDispatchProcessWith(t, relays[i].url, dispatchOptions{
			NATSURL: natsURL, SigningKey: multiprocessSigningKey, Env: env,
		})
	}
	const bound = `msg="dispatch delivery: NATS consumer bound"`
	processes := make([]*dispatchProcess, testlimits.MaxProcessesAtOnce)
	processes[0] = start(0, app)
	processes[0].waitHealthy(t)
	for _, durable := range deliveryDurables {
		processes[0].waitForLog(t, 30*time.Second, bound, "name="+durable)
	}
	processes[1] = start(1, app)
	for i := 2; i < len(processes); i++ {
		processes[i] = start(i, nil)
	}
	for _, process := range processes {
		process.waitHealthy(t)
		process.waitForLog(t, 10*time.Second, "dispatch: pools scaled to the test connection budget", "budget="+strconv.Itoa(budget))
	}
	for _, durable := range deliveryDurables {
		processes[1].waitForLog(t, 30*time.Second, `msg="dispatch delivery: NATS consumer held by another process; standing by"`, "name="+durable)
	}

	var failures sync.Map
	var load sync.WaitGroup
	release := make(chan struct{})
	goAhead := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() {
		goAhead()
		load.Wait()
	})
	for i, process := range processes {
		project := fmt.Sprintf("BUDGET%d", i)
		process.createProject(t, project, "Budget")
		keys := make([]string, 4)
		artifacts := make([]string, len(keys))
		for k := range keys {
			keys[k], artifacts[k] = process.createIssueIn(t, project, fmt.Sprintf("Budget %d", k), "before")
		}
		process.openLiveEvents(t)
		editor := process.openEditor(t, artifacts[0], "before\n")
		replaceText(t, editor, "before", "typed")
		const workers, rounds = 16, 8
		for worker := range workers {
			load.Go(func() {
				<-release
				for round := range rounds {
					key, artifact := keys[(worker+round)%len(keys)], artifacts[(worker+round)%len(artifacts)]
					for _, request := range []struct{ method, path, body string }{
						{http.MethodGet, "/api/v1/issues/" + key, ""},
						{http.MethodPatch, "/api/v1/issues/" + key, fmt.Sprintf(`{"title":"Budget %d %d"}`, worker, round)},
						{http.MethodGet, "/api/v1/artifacts/" + artifact + "/text", ""},
					} {
						answer, err := process.trySend(request.method, request.path, request.body, asUser(dispatchTestLogin))
						if err != nil || answer.status != http.StatusOK {
							failures.Store(fmt.Sprintf("process %d: %s %s", i, request.method, request.path),
								fmt.Sprintf("%d %v %s", answer.status, err, answer.body))
						}
					}
				}
			})
		}
	}
	loaded := time.Now()
	goAhead()
	load.Wait()
	t.Logf("the processes served their load in %v", time.Since(loaded).Round(time.Millisecond))

	// The holder dies with both holds live, as a task killed mid-deploy does, so they lapse rather
	// than being released (a stopped process releases them, and the standby's polls find them free
	// one at a time).
	if err := processes[0].Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the intakes' holder: %v", err)
	}
	<-processes[0].Exited
	holds := make([]string, len(deliveryDurables))
	for i, durable := range deliveryDurables {
		holds[i] = "delivery-intake-" + durable
	}
	locker, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the intakes' hold rows: %v", err)
	}
	t.Cleanup(func() { _ = locker.Rollback(context.Background()) })
	rows, err := locker.Query(ctx, `select name from duty_holds where name = any($1) for update`, holds)
	if err != nil {
		t.Fatalf("lock the intakes' hold rows: %v", err)
	}
	if locked, err := pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(locked) != len(holds) {
		t.Fatalf("locked the hold rows %v (%v), want %v: process 0 held no hold to lapse", locked, err, holds)
	}
	// Each lapses at most 15 s (the hold's TTL) after process 0's last renewal, and the standby
	// tries for it at the lapse: each of its takings queues behind the row lock held here. The wait
	// ends on that fact, one backend for each hold, and its one bound is the test binary's deadline
	// (less testwait's margin), which fails the test naming the count it last read.
	storetest.WaitForLockWaiters(t, locker, len(holds))
	if err := locker.Rollback(ctx); err != nil {
		t.Fatalf("let the standby's takings of the holds go: %v", err)
	}
	released := time.Now()
	for _, durable := range deliveryDurables {
		for !processes[1].logged(bound, "name="+durable) {
			if time.Since(released) > budgetTakeOverWithin {
				t.Fatalf("the standby did not bind %s within %v of taking its hold, with both durables' take-overs under way at once: its take-over stalled on a lease pool of its budget\n%s",
					durable, budgetTakeOverWithin, processes[1].Output.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	t.Logf("the standby bound both durables %v after it took both holds at once", time.Since(released).Round(time.Millisecond))
	waitForEveryEventPublished(t, database, 60*time.Second)

	byProcess, total, samples := peaks()
	failures.Range(func(request, failure any) bool {
		t.Errorf("%s answered %s, want 200", request, failure)
		return true
	})
	t.Logf("%d samples: peak connections held open to Postgres by process %v, budget %d; at most %d client backends on the database at once, the test's own among them",
		samples, byProcess, budget, total)
	if samples < 10 {
		t.Fatalf("the connections were read %d times, too few to hold the row to", samples)
	}
	for i, peak := range byProcess {
		if peak == 0 {
			t.Errorf("process %d never showed a connection: its relay held none, so the count does not see the process", i)
		}
		if peak > budget {
			t.Errorf("process %d held %d connections to Postgres at once, over its budget of %d", i, peak, budget)
		}
	}
	for i, process := range processes {
		if output := process.Output.String(); strings.Contains(output, "53300") || strings.Contains(output, "remaining connection slots") {
			t.Errorf("process %d met a server out of connections:\n%s", i, output)
		}
	}
}

// connectionRelay forwards one Dispatch process's database connections to the test's Postgres and
// counts the connections it holds open there (held), so a row whose processes share one database
// counts each process's connections by the relay they pass through.
type connectionRelay struct {
	url  string
	held atomic.Int64
}

// startConnectionRelay starts a relay to the server databaseURL names, until the test ends, and
// returns it with url, the same URL through the relay. A connection is counted in held from the
// moment the relay's dial to the server succeeds, before the first byte reaches Postgres, until
// both its directions have closed.
func startConnectionRelay(t *testing.T, databaseURL string) *connectionRelay {
	t.Helper()
	target, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse the database URL: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the connection relay: %v", err)
	}
	through := *target
	through.Host = listener.Addr().String()
	relay := &connectionRelay{url: through.String()}
	var open sync.Map
	t.Cleanup(func() {
		_ = listener.Close()
		open.Range(func(conn, _ any) bool {
			_ = conn.(net.Conn).Close()
			return true
		})
	})
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target.Host)
			if err != nil {
				_ = client.Close()
				continue
			}
			relay.held.Add(1)
			open.Store(client, true)
			open.Store(server, true)
			go func() {
				var both sync.WaitGroup
				both.Go(func() {
					_, _ = io.Copy(server, client)
					_ = server.(*net.TCPConn).CloseWrite()
				})
				both.Go(func() {
					_, _ = io.Copy(client, server)
					_ = client.(*net.TCPConn).CloseWrite()
				})
				both.Wait()
				_ = client.Close()
				_ = server.Close()
				open.Delete(client)
				open.Delete(server)
				relay.held.Add(-1)
			}()
		}
	}()
	return relay
}

// sampleConnections reads, every budgetSampleEvery until the returned stop is called, how many
// connections each relay holds open to Postgres and how many client backends pg_stat_activity
// lists on conn's database, conn's own left out; stop answers the highest count each relay's
// process reached, the highest count of client backends in one read, and how many reads were
// made. The test's cleanup stops the reads too, before conn closes, for a test that ends before it
// calls stop.
func sampleConnections(t *testing.T, conn *pgx.Conn, relays []*connectionRelay) (stop func() (byProcess []int, total, samples int)) {
	t.Helper()
	done, finished := make(chan struct{}), make(chan struct{})
	byProcess := make([]int, len(relays))
	var total, samples int
	var failed error
	go func() {
		defer close(finished)
		ticker := time.NewTicker(budgetSampleEvery)
		defer ticker.Stop()
		for {
			var backends int
			if err := conn.QueryRow(context.Background(), `
				select count(*) from pg_stat_activity
				where datname = current_database() and backend_type = 'client backend' and pid <> pg_backend_pid()`,
			).Scan(&backends); err != nil {
				failed = err
				return
			}
			for i, relay := range relays {
				byProcess[i] = max(byProcess[i], int(relay.held.Load()))
			}
			total = max(total, backends)
			samples++
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	halt := sync.OnceFunc(func() {
		close(done)
		<-finished
	})
	t.Cleanup(halt)
	return func() ([]int, int, int) {
		halt()
		if failed != nil {
			t.Fatalf("read pg_stat_activity: %v", failed)
		}
		return byProcess, total, samples
	}
}
