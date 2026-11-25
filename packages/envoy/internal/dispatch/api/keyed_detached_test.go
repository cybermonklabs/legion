package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// keyedPost posts body to the rig's route on server as alice under key, on ctx: the client gives
// up when ctx ends. It answers the status and body, or the client's own failure.
func keyedPost(ctx context.Context, server *httptest.Server, path, body, key string) (int, string, http.Header, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, strings.NewReader(body))
	if err != nil {
		return 0, "", nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set(contracts.IdempotencyKeyHeader, key)
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		return 0, "", nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	return response.StatusCode, string(answer), response.Header, err
}

// INFRA-258. A write the server takes longer to finish than its client waits still lands, once:
// the handler runs on past the client's giving up, and the client's repeat under its key is told
// the first run is still going (409 KEY_IN_FLIGHT), then answered the first run's own stored
// answer. A handler bound to its client's cancellation rolled back at the client's deadline, and
// every repeat started again from nothing, so such a write could never land. The key's lease is
// stagedAhead, so the first run holds its key however long the machine stalls its renewals:
// on the deployment's lease a renewal stalled for two thirds of it would let a repeat take the key
// over and run the write again.
func TestAKeyedWriteSlowerThanItsClientFinishesAndIsReplayed(t *testing.T) {
	t.Parallel()
	running := make(chan struct{})
	release, releaseOnce := heldUntilReleased(t)
	rig := newWritingRigWith(t, testServerOptions{idempotencyLease: stagedAhead}, func(run int32, w http.ResponseWriter, r *http.Request, write rigWrite) {
		if run == 1 {
			close(running)
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		// Read after the wait, so a run bound to its client's cancellation is cut off whichever of
		// the two came first.
		if r.Context().Err() != nil {
			writeError(w, "CUT_OFF", http.StatusServiceUnavailable, "the run was cut off with its client")
			return
		}
		if err := write(r.Context(), nil); err != nil {
			t.Errorf("run %d's write: %v", run, err)
		}
		answerRun(run, w, r)
	})
	// clientGone closes once the first attempt's request, as the server holds it, has ended: its
	// client's giving up has reached Dispatch by then, and so has reached a run bound to it.
	clientGone := make(chan struct{})
	var first atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			context.AfterFunc(r.Context(), func() { close(clientGone) })
		}
		rig.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	// The client gives up while the run is going, at the moment the test cancels its context.
	client, giveUp := context.WithCancel(context.Background())
	defer giveUp()
	attempted := make(chan keyedAnswered, 1)
	go func() {
		status, body, _, err := keyedPost(client, server, "/test/a/writes", `{"n":1}`, "slow-press")
		attempted <- keyedAnswered{status: status, body: body, err: err}
	}()
	testwait.Receive(t, "the first attempt's run to start", running)
	giveUp()
	if attempt := testwait.Receive(t, "the first attempt's client to give up", attempted); attempt.err == nil {
		t.Fatalf("the first attempt answered %d %s; want the client to have given up", attempt.status, attempt.body)
	}
	testwait.Receive(t, "the first attempt's request to end on the server", clientGone)
	ctx := testwait.Context(t)
	status, body, header, err := keyedPost(ctx, server, "/test/a/writes", `{"n":1}`, "slow-press")
	if err != nil || status != http.StatusConflict || !strings.Contains(body, contracts.IdempotencyKeyInFlight) ||
		header.Get("Retry-After") != "1" {
		t.Fatalf("the repeat after the client gave up: %d %s (%v), want 409 %s: the first run is still going",
			status, body, err, contracts.IdempotencyKeyInFlight)
	}
	releaseOnce()
	type repeated struct {
		status int
		body   string
		header http.Header
	}
	answer := testwait.Until(t, "a repeat answered other than 409 once the first run is released", func() (repeated, bool) {
		status, body, header, err := keyedPost(ctx, server, "/test/a/writes", `{"n":1}`, "slow-press")
		if err != nil {
			t.Fatalf("a later repeat: %v", err)
		}
		return repeated{status: status, body: body, header: header}, status != http.StatusConflict
	})
	if answer.status != http.StatusCreated || answer.header.Get(contracts.IdempotencyReplayedHeader) != "?1" || !strings.Contains(answer.body, `"run":1`) {
		t.Fatalf("the repeat once the first run finished: %d %s replayed=%q, want the first run's 201 replayed",
			answer.status, answer.body, answer.header.Get(contracts.IdempotencyReplayedHeader))
	}
	if runs, writes := rig.runs.Load(), rig.writes(t); runs != 1 || len(writes) != 1 {
		t.Fatalf("the handler ran %d times and wrote %v, want once and one row", runs, writes)
	}
}

// ageRequestKeyLease moves the lease of login's key by back by, as that much time passing with no
// renewal does: the layer reads a lease as lapsed by its leased_at against the database's now(). A
// negative by moves it that far ahead instead, which stages a lease the layer reads live however
// long a row runs (stagedAhead).
func ageRequestKeyLease(t *testing.T, database *store.Store, login, key string, by time.Duration) {
	t.Helper()
	if tag, err := database.Pool.Exec(context.Background(), `
		update request_keys set leased_at = leased_at - make_interval(secs => $3)
		where login = $1 and idempotency_key = $2 and leased_at is not null
	`, login, key, by.Seconds()); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("age the lease of the key %s: %v (%d rows)", key, err, tag.RowsAffected())
	}
}

// awaitLeaseRenewedSince waits until login's key holds a lease a statement wrote at or after since,
// a moment of the database's clock: a renewal that ran after that moment wrote it, which no time
// passing afterwards undoes. A try that finds none reads the lease the row holds.
func awaitLeaseRenewedSince(t *testing.T, database *store.Store, login, key string, since time.Time) {
	t.Helper()
	testwait.Until(t, "the running handler to renew the lease of the key "+key+" after "+since.Format(time.RFC3339Nano), func() (string, bool) {
		var leased *time.Time
		if err := database.Pool.QueryRow(context.Background(), `
			select leased_at from request_keys where login = $1 and idempotency_key = $2
		`, login, key).Scan(&leased); err != nil {
			t.Fatalf("read the lease of the key %s: %v", key, err)
		}
		if leased == nil {
			return "the key holds no lease", false
		}
		return "the key is leased at " + leased.Format(time.RFC3339Nano), !leased.Before(since)
	})
}

// A run that outlives its key's lease keeps the key: it renews the lease while it runs, so a repeat
// past the lease is still told the first run is going rather than taking the key over and making
// the write a second time. Only a run whose renewals stopped - its process died - is taken over.
// The lease is judged in the database against its now(), so the test ages a lease rather than
// waiting one out; the renewals run on the server's clock, which the test moves. The renewal is
// read as the lease a statement wrote after a moment of the database's clock taken before the
// move, and the lease it wrote is then staged stagedAhead on, so the repeat reads the run as going
// however long the machine took since the renewal, which alone keeps the lease for one lease. The
// dead holder's lease is staged the same way, and aged a whole lease past that staging for the
// take-over.
func TestARunningHandlerRenewsItsKeyPastTheLease(t *testing.T) {
	t.Parallel()
	release, releaseOnce := heldUntilReleased(t)
	running := make(chan struct{})
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	fake := clocktest.New(start)
	rig := newKeyedRigWith(t, testServerOptions{clock: fake}, keyedRigPattern, func(run int32, w http.ResponseWriter, r *http.Request) {
		if run == 1 {
			close(running)
			<-release
		}
		answerRun(run, w, r)
	})
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.send(t, "a", nil, "alice", "long-run") }()
	testwait.Receive(t, "the first run to start", running)
	// A whole lease with no renewal, as the run's lease stands once that long has passed.
	ageRequestKeyLease(t, rig.database, "alice", "long-run", contracts.IdempotencyLease+time.Second)
	renewal := start.Add(keyedLeaseRenewal)
	fake.AwaitArmedAt(t, "the run's lease renewal", renewal)
	since := testdb.ClockTimestamp(t, rig.database.Pool)
	fake.Set(renewal)
	awaitLeaseRenewedSince(t, rig.database, "alice", "long-run", since)
	ageRequestKeyLease(t, rig.database, "alice", "long-run", -stagedAhead)
	repeat := rig.send(t, "a", nil, "alice", "long-run")
	if code, _ := refusalOf(t, repeat); repeat.Code != http.StatusConflict || code != contracts.IdempotencyKeyInFlight {
		t.Fatalf("a repeat past the lease while the first run renews it: status=%d body=%s, want 409 %s",
			repeat.Code, repeat.Body.String(), contracts.IdempotencyKeyInFlight)
	}
	releaseOnce()
	first := awaitResponse(t, answered)
	if after := rig.send(t, "a", nil, "alice", "long-run"); !replayed(after) || after.Body.String() != first.Body.String() {
		t.Fatalf("the repeat once the first run answered: replayed=%v body=%s, want %s", replayed(after), after.Body.String(), first.Body.String())
	}
	if runs := rig.runs.Load(); runs != 1 {
		t.Fatalf("the handler ran %d times, want once", runs)
	}

	// A holder whose renewals stopped - its process gone - is taken over once its lease lapses.
	dead := httptest.NewRequest(http.MethodPost, "/test/a/writes", nil)
	dead.Header.Set("Content-Type", "application/json")
	dead.SetPathValue("thing", "a")
	holdRequestKey(t, rig.database, heldKey{login: "alice", key: "dead-run", request: dead, pattern: keyedRigPattern, leasedAgo: -stagedAhead})
	if inside := rig.send(t, "a", nil, "alice", "dead-run"); inside.Code != http.StatusConflict {
		t.Fatalf("a repeat inside a dead holder's lease: status=%d body=%s, want 409", inside.Code, inside.Body.String())
	}
	ageRequestKeyLease(t, rig.database, "alice", "dead-run", stagedAhead+contracts.IdempotencyLease+time.Second)
	if takeover := rig.send(t, "a", nil, "alice", "dead-run"); takeover.Code != http.StatusCreated || replayed(takeover) {
		t.Fatalf("a repeat once the dead holder's lease lapsed: status=%d body=%s, want the request run", takeover.Code, takeover.Body.String())
	}
}

// The handler's context is the server's, not the client's: the client's cancellation mid-run does
// not reach it, and the server's own bound, keyedHandlerLimit, does: the run arms that bound on the
// server's clock as it starts.
func TestAKeyedHandlerRunsUnderTheServersOwnBound(t *testing.T) {
	t.Parallel()
	running, proceed := make(chan struct{}), make(chan struct{})
	saw := make(chan error, 1)
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	fake := clocktest.New(start)
	rig := newKeyedRigWith(t, testServerOptions{clock: fake}, keyedRigPattern, func(run int32, w http.ResponseWriter, r *http.Request) {
		close(running)
		<-proceed
		saw <- r.Context().Err()
		answerRun(run, w, r)
	})
	// Registered after the rig's cleanups, so it runs before them: a failure below lets the parked
	// run go.
	proceedOnce := sync.OnceFunc(func() { close(proceed) })
	t.Cleanup(proceedOnce)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/test/a/writes", bytes.NewReader([]byte("{}")))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set(contracts.IdempotencyKeyHeader, "bounded")
	response := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		rig.handler.ServeHTTP(response, request)
	}()
	testwait.Receive(t, "the run to start", running)
	fake.AwaitArmedAt(t, "the run's bound, keyedHandlerLimit from its start", start.Add(keyedHandlerLimit))
	cancel()
	proceedOnce()
	testwait.Receive(t, "the run whose client went away to answer", served)
	if err := testwait.Receive(t, "the run's context error", saw); err != nil || response.Code != http.StatusCreated {
		t.Fatalf("a run whose client went away mid-run: context error %v, status %d, want it run to its answer",
			err, response.Code)
	}
}

// A keyed handler still running is part of the server's background work: shutdown's wait does not
// return before it does, so the database pool is never closed under it. The wait returns only once
// the count of running work is zero (TestTheBackgroundWaitWaitsForWorkStartedWhileItWaits), so the
// count, read while the handler is held, is what would let it return.
func TestTheBackgroundWaitWaitsForARunningKeyedHandler(t *testing.T) {
	t.Parallel()
	release, releaseOnce := heldUntilReleased(t)
	running := make(chan struct{})
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		close(running)
		<-release
		answerRun(run, w, r)
	})
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.send(t, "a", nil, "alice", "draining") }()
	testwait.Receive(t, "the keyed handler to start", running)
	rig.server.detached.mu.Lock()
	counted := rig.server.detached.running
	rig.server.detached.mu.Unlock()
	if counted == 0 {
		t.Fatal("the background work counts nothing while a keyed handler still runs, so the background wait would return under it")
	}
	waited := make(chan struct{})
	go func() {
		rig.server.detached.wait()
		close(waited)
	}()
	releaseOnce()
	awaitResponse(t, answered)
	testwait.Receive(t, "the background wait to return once the handler answered", waited)
}

// backgroundWork's wait returns only once nothing runs, however work starts and ends around it:
// work that starts while it waits, which a sync.WaitGroup forbids, is waited for too. The work is
// in-process alone, so the bubble's synctest.Wait says when the waiter has gone as far as it can,
// and keyedHandlerLimit on the bubble's clock after it (testwait.Idle) says no bound of its own
// shorter than a keyed handler's detached run may last lets it return.
func TestTheBackgroundWaitWaitsForWorkStartedWhileItWaits(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var work backgroundWork
		work.wait()
		work.start()
		waited := make(chan struct{})
		go func() {
			work.wait()
			close(waited)
		}()
		work.start()
		work.done()
		testwait.Idle(t, keyedHandlerLimit, "backgroundWork arms no timer, and among the work it waits for is a keyed "+
			"handler's run past its client, which keyedHandlerLimit ends: a wait that gave up at a bound of its own "+
			"shorter than that would return under a run still inside its limit, and shutdown would close the pool under it")
		select {
		case <-waited:
			t.Fatal("the wait returned with work still running")
		default:
		}
		work.done()
		synctest.Wait()
		select {
		case <-waited:
		default:
			t.Fatal("the wait did not return once the work ended")
		}
	})
}

// The slow-write test hook delays every keyed run of the method and path it was armed for, on the
// handler's own context and the server's clock, and is mounted only under test hooks. A run whose
// context ends during the delay never reaches its handler.
func TestTheSlowWriteHookDelaysEveryKeyedRunOfItsPath(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{testHooks: true, clock: fake})
	issue := createTestIssue(t, handler, "SLOW", "Slow writes")
	path := "/api/v1/issues/" + issue + "/comments"
	for name, body := range map[string]map[string]any{
		"a method that takes no key": {"method": "PUT", "path": path, "delay_ms": 10},
		"a path that is not a path":  {"method": "POST", "path": "comments", "delay_ms": 10},
		"a negative delay":           {"method": "POST", "path": path, "delay_ms": -1},
		"an unknown field":           {"method": "POST", "path": path, "delay_ms": 10, "once": true},
	} {
		if refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/writes/_test/slow", body, "alice"); refused.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s, want 400", name, refused.Code, refused.Body.String())
		}
	}
	arm := func(delay int) {
		t.Helper()
		if armed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/writes/_test/slow",
			map[string]any{"method": "POST", "path": path, "delay_ms": delay}, "alice"); armed.Code != http.StatusOK {
			t.Fatalf("arm %d ms: status=%d body=%s", delay, armed.Code, armed.Body.String())
		}
	}
	const delay = 700 * time.Millisecond
	arm(int(delay.Milliseconds()))
	// Each armed run arms the delay on the server's clock and answers only once the clock is past
	// it: the clock moves only when the test moves it, so a run that answers before then did not
	// wait the delay out.
	for _, key := range []string{"slow-1", "slow-2"} {
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			answered <- keyedRequest(t, handler, http.MethodPost, path, map[string]any{"body": key}, "alice", key)
		}()
		due := fake.Now().Add(delay)
		testwait.Until(t, key+"'s run to arm the hook's delay, due at "+due.String(), func() ([]time.Time, bool) {
			select {
			case created := <-answered:
				t.Fatalf("%s answered %d %s inside the armed delay", key, created.Code, created.Body.String())
			default:
			}
			armed := fake.Armed()
			return armed, slices.Contains(armed, due)
		})
		fake.Set(due)
		if created := testwait.Receive(t, key+"'s run to answer once its delay was past", answered); created.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", key, created.Code, created.Body.String())
		}
	}
	// A run whose context ends inside the delay never reaches the handler: answered 503, its key
	// released and nothing written.
	direct := directServer(Deps{TestHooksEnabled: true})
	direct.slowWrites.Store(http.MethodPost+" /cut", time.Minute)
	reached := false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cut := httptest.NewRecorder()
	direct.slowedByTestHook(func(http.ResponseWriter, *http.Request) { reached = true })(cut,
		httptest.NewRequestWithContext(ctx, http.MethodPost, "/cut", nil))
	if reached || cut.Code != http.StatusServiceUnavailable {
		t.Fatalf("a run cut off inside the delay: reached=%v status=%d, want 503 with the handler never run", reached, cut.Code)
	}
	route := http.MethodPost + " " + path
	armedRuns, armedWaited := served.slowWriteRunsOn(route)
	if armedRuns != 2 || armedWaited != 2*delay {
		t.Fatalf("the hook delayed %d runs of %s, waiting %v altogether, want the two armed runs' %v",
			armedRuns, route, armedWaited, 2*delay)
	}
	arm(0)
	if delay, stillArmed := served.slowWrites.Load(route); stillArmed {
		t.Fatalf("%s is still armed for %v after delay_ms 0, want it disarmed", route, delay)
	}
	if created := keyedRequest(t, handler, http.MethodPost, path, map[string]any{"body": "disarmed"}, "alice", "fast"); created.Code != http.StatusCreated {
		t.Fatalf("a run once disarmed: status=%d body=%s, want 201", created.Code, created.Body.String())
	}
	if runs, waited := served.slowWriteRunsOn(route); runs != armedRuns {
		t.Fatalf("the disarmed run went through the hook's delay: %s is at %d runs having waited %v, want the %d armed runs' %v untouched",
			route, runs, waited, armedRuns, armedWaited)
	}
	var comments int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from comments where issue_key = $1`, issue).Scan(&comments); err != nil {
		t.Fatal(err)
	}
	if comments != 3 {
		t.Fatalf("the issue holds %d comments, want the three keyed runs'", comments)
	}

	plain, _, _ := newTestServer(t, testServerOptions{})
	if response := dispatchRequest(t, plain, http.MethodPost, "/api/v1/writes/_test/slow",
		map[string]any{"method": "POST", "path": path, "delay_ms": 10}, "alice"); response.Code != http.StatusNotFound {
		t.Fatalf("the hook without DISPATCH_TEST_HOOKS: status=%d, want 404", response.Code)
	}
}

// countingBody is a request body that counts what is read from it.
type countingBody struct {
	io.Reader
	read atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read.Add(int64(n))
	return n, err
}

// INFRA-258. A repeat of a keyed write whose first request still
// runs is never spooled: the key is looked up by its login and key before the body is read, so a
// slow upload's repeats - the page asks again about once a second - are never written to disk again
// only to be told to wait. Its body is read to its end, discarded and written only to the digest it
// is judged by once it has arrived (TestARepeatFoundInFlightIsJudgedOnceItsBodyHasArrived); here
// the first still runs, so it is 409 KEY_IN_FLIGHT. With the temporary directory gone, a spool
// would answer 500. The first's lease is staged stagedAhead past the database's now(), so the
// repeat reads it as still running however long the 2 MiB body takes to arrive.
func TestAnInFlightRepeatIsJudgedWithoutSpoolingItsBody(t *testing.T) {
	const pattern = "/api/v1/issues/{key}/artifacts"
	rig := newKeyedRigAt(t, pattern, answerRun)
	body := `{"name":"big.bin","content":"` + strings.Repeat("a", 2<<20) + `"}`
	first := httptest.NewRequest(http.MethodPost, "/api/v1/issues/UP-1/artifacts", strings.NewReader(body))
	first.Header.Set("Content-Type", "application/json")
	first.SetPathValue("key", "UP-1")
	holdRequestKey(t, rig.database, heldKey{login: "alice", key: "slow-upload", request: first, pattern: pattern, leasedAgo: -stagedAhead})

	sent := &countingBody{Reader: strings.NewReader(body)}
	repeat := httptest.NewRequest(http.MethodPost, "/api/v1/issues/UP-1/artifacts", sent)
	repeat.Header.Set("Content-Type", "application/json")
	repeat.Header.Set("X-Dispatch-User", "alice")
	repeat.Header.Set(contracts.IdempotencyKeyHeader, "slow-upload")
	response := httptest.NewRecorder()
	unspooled(t, func() { rig.handler.ServeHTTP(response, repeat) })
	if code, _ := refusalOf(t, response); response.Code != http.StatusConflict || code != contracts.IdempotencyKeyInFlight ||
		response.Header().Get("Retry-After") != "1" || response.Header().Get(contracts.IdempotencyKeyHeader) != "slow-upload" {
		t.Fatalf("a repeat while the first upload runs: status=%d headers=%v body=%s, want 409 %s naming its key, unspooled",
			response.Code, response.Header(), response.Body.String(), contracts.IdempotencyKeyInFlight)
	}
	if read := sent.read.Load(); read != int64(len(body)) {
		t.Fatalf("the repeat was judged with %d of its %d bytes read, want all of them", read, len(body))
	}
	if runs := rig.runs.Load(); runs != 0 {
		t.Fatalf("the handler ran %d times, want none", runs)
	}
}

// A client that gives up while its key is being claimed leaves the claim to finish: the claim's
// statements run on a context the client's cancellation does not reach, so a claim the database
// made is never left in flight with no handler behind it, holding every repeat at KEY_IN_FLIGHT
// for a lease. Here the claim waits behind another transaction's hold on the key, the client gives
// up meanwhile, and once the hold goes the request is claimed, run once and answered under its key.
func TestAKeyedClaimFinishesWhenItsClientGivesUpDuringIt(t *testing.T) {
	t.Parallel()
	rig := newKeyedRig(t, answerRun)
	ctx := context.Background()
	hold, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Rollback(ctx) })
	if _, err := hold.Exec(ctx, `
		insert into request_keys (login, idempotency_key, request_digest, method, path)
		values ('alice', 'cut-claim', 'held', 'POST', '/test/a/writes')
	`); err != nil {
		t.Fatalf("hold the key: %v", err)
	}
	client, giveUp := context.WithCancel(ctx)
	defer giveUp()
	request := httptest.NewRequestWithContext(client, http.MethodPost, "/test/a/writes", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set(contracts.IdempotencyKeyHeader, "cut-claim")
	response := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		rig.handler.ServeHTTP(response, request)
	}()
	waitForDatabaseLocks(t, hold, 1)
	giveUp()
	testwait.Span(t, 300*time.Millisecond, "a claim bound to the client's context is cut when pgx, at that context's cancel, "+
		"sets a deadline of now on the statement's own connection from a goroutine of its own (pgconn's default "+
		"BuildContextWatcherHandler, a DeadlineContextWatcherHandler run by context.AfterFunc): no state the test "+
		"reads records it before the statement's read fails on it, the statement is real PostgreSQL I/O (no "+
		"synctest), and no later action of the test is ordered after that deadline")
	if err := hold.Rollback(ctx); err != nil {
		t.Fatalf("release the hold: %v", err)
	}
	testwait.Receive(t, "the request to finish once the hold on its key was released", served)
	if response.Code != http.StatusCreated || rig.runs.Load() != 1 {
		t.Fatalf("a request whose client gave up mid-claim: status=%d body=%s, ran %d times; want it claimed, run once and answered",
			response.Code, response.Body.String(), rig.runs.Load())
	}
	var status *int
	if err := rig.database.Pool.QueryRow(ctx,
		`select status from request_keys where login = 'alice' and idempotency_key = 'cut-claim'`).Scan(&status); err != nil {
		t.Fatalf("read the key: %v", err)
	}
	if status == nil || *status != http.StatusCreated {
		t.Fatalf("the key holds status %v, want the run's 201 kept for its repeats", status)
	}
}

// A handler that runs on past its limit - its context ended and it did not stop - holds its key no
// longer: the lease is renewed until the limit and no further, so once it lapses a repeat is
// answered as one whose holder died, here taking over a key the run made nothing under, rather than
// told KEY_IN_FLIGHT for as long as the handler ignores its context.
func TestARunPastItsLimitStopsRenewingItsLease(t *testing.T) {
	t.Parallel()
	release, releaseOnce := heldUntilReleased(t)
	running, bounded := make(chan struct{}), make(chan struct{})
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	fake := clocktest.New(start)
	rig := newKeyedRigWith(t, testServerOptions{clock: fake}, keyedRigPattern, func(run int32, w http.ResponseWriter, r *http.Request) {
		if run == 1 {
			close(running)
			// The run sees its bound end its context and does not stop.
			<-r.Context().Done()
			close(bounded)
			<-release
		}
		answerRun(run, w, r)
	})
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.send(t, "a", nil, "alice", "overrun") }()
	testwait.Receive(t, "the first run to start", running)
	bound := start.Add(keyedHandlerLimit)
	fake.AwaitArmedAt(t, "the run's bound", bound)
	// Every renewal the run is owed before its bound, each waited for once it has run and armed the
	// next, so none is still under way when the bound comes.
	renewal := start.Add(keyedLeaseRenewal)
	for ; renewal.Before(bound); renewal = renewal.Add(keyedLeaseRenewal) {
		fake.AwaitArmedAt(t, "the run's next lease renewal", renewal)
		fake.Set(renewal)
	}
	fake.AwaitArmedAt(t, "the run's renewal due at or past its bound", renewal)
	fake.Set(bound)
	testwait.Receive(t, "the run's bound to end its context", bounded)
	// The renewals end with the run's bound. The renewal due with it may still run: the loop's
	// select has the bound and that renewal ready together and may take the renewal - the bound's
	// lease past the limit, which the doc above allows - after which it arms the next renewal and
	// returns at its select, which stops that one. None is left armed after it, so a run that
	// ignores its context renews its lease no further however far the clock moves.
	testwait.Span(t, 2*time.Second, "the renewal loop's choice and its return are reported to no state the test "+
		"reads; the span starts once the bound has ended the run's context, with the loop runnable, so by its end "+
		"the loop has returned or taken the renewal due with the bound, whose statement's own bound is armed on the "+
		"server's clock until the statement ends")
	// A renewal the loop took ends before the lease is aged below, or it would land after the aging
	// and hold the repeat at KEY_IN_FLIGHT; a loop that renews past the bound keeps that renewal
	// armed, and the wait gives up naming it.
	inFlight := bound.Add(keyedBookkeeping)
	renewing := func(at time.Time) bool {
		return at.Equal(inFlight) || (at.After(bound) && at.Sub(start)%keyedLeaseRenewal == 0)
	}
	testwait.Until(t, "the renewal due with the run's bound, if the loop took it, to end, and no lease renewal armed past the bound",
		func() ([]time.Time, bool) {
			armed := fake.Armed()
			return armed, !slices.ContainsFunc(armed, renewing)
		})
	// Then a whole lease past the last renewal it allows.
	ageRequestKeyLease(t, rig.database, "alice", "overrun", contracts.IdempotencyLease+time.Second)
	takeover := rig.send(t, "a", nil, "alice", "overrun")
	if takeover.Code != http.StatusCreated || replayed(takeover) || !strings.Contains(takeover.Body.String(), `"run":2`) {
		t.Fatalf("a repeat a lease past the run's limit: status=%d body=%s, want the key taken over and run again",
			takeover.Code, takeover.Body.String())
	}
	releaseOnce()
	awaitResponse(t, answered)
}

// A running keyed handler's lease renewals keep a fixed rate on the server's clock, as the
// ticker before the server's clock kept them: a renewal that waits on the key's row past the
// moment a renewal paced from its end would be due leaves the next one where the rate puts it, one
// keyedLeaseRenewal after it was due, not one after it ended. So two renewals that each run out of
// keyedBookkeeping still leave the third due inside IdempotencyLease, as the renewal's own
// interval promises. Here the first renewal waits on the row, the clock moves half an interval
// meanwhile, and the next renewal is armed at two intervals, not two and a half.
func TestALeaseRenewalThatWaitsLeavesTheNextAtItsFixedRate(t *testing.T) {
	release, releaseOnce := heldUntilReleased(t)
	running := make(chan struct{})
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	fake := clocktest.New(start)
	rig := newKeyedRigWith(t, testServerOptions{clock: fake}, keyedRigPattern, func(run int32, w http.ResponseWriter, r *http.Request) {
		if run == 1 {
			close(running)
			<-release
		}
		answerRun(run, w, r)
	})
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.send(t, "a", nil, "alice", "fixed-rate") }()
	testwait.Receive(t, "the run to start", running)
	first := start.Add(keyedLeaseRenewal)
	fake.AwaitArmedAt(t, "the run's first lease renewal", first)

	ctx := context.Background()
	locker, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Rollback(ctx) })
	if _, err := locker.Exec(ctx,
		`select 1 from request_keys where login = 'alice' and idempotency_key = 'fixed-rate' for update`); err != nil {
		t.Fatalf("hold the key's row: %v", err)
	}
	fake.Set(first)
	waitForDatabaseLocks(t, locker, 1)
	// The renewal waits on the row while the clock moves half an interval past it.
	ended := first.Add(keyedLeaseRenewal / 2)
	fake.Set(ended)
	if err := locker.Rollback(ctx); err != nil {
		t.Fatalf("release the key's row: %v", err)
	}

	atRate, fromEnd := first.Add(keyedLeaseRenewal), ended.Add(keyedLeaseRenewal)
	armed := testwait.Until(t, "the run's next lease renewal to be armed once the waiting one ended", func() ([]time.Time, bool) {
		armed := fake.Armed()
		return armed, slices.ContainsFunc(armed, atRate.Equal) || slices.ContainsFunc(armed, fromEnd.Equal)
	})
	if !slices.ContainsFunc(armed, atRate.Equal) || slices.ContainsFunc(armed, fromEnd.Equal) {
		t.Fatalf("armed %v once the renewal that waited ended at %s; want the next renewal at %s, one interval after the "+
			"waiting one was due, and none at %s, one interval after it ended", armed, ended, atRate, fromEnd)
	}
	releaseOnce()
	awaitResponse(t, answered)
}

// A keyed close whose settlement, before its transaction, waits on a lock ends at the keyed run's
// bound (keyedHandlerLimit), as every keyed handler does, and releases its key, so the repeat runs
// it once the lock is free. The close settles the issue's documents before its transaction and
// sweeps them after it (the next test), on a context that must outlive the client; a keyed
// request's context already does, and context.WithoutCancel on it dropped the bound with the
// cancellation, so a close held on the issue's owner lock kept its background-work slot, and the
// shutdown's wait for it, as long as that lock stayed held.
func TestAKeyedCloseWhoseSettlementIsHeldOnTheIssuesOwnerLockEndsAtItsRunsBound(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	fake := clocktest.New(start)
	handler, database, _, _ := newRegisteredTestServer(t, testServerOptions{clock: fake})
	issue := createInteractionIssue(t, handler, "TEST", "Closed while its owner lock is held", "Context\n")
	awaitSettled(t, database, issue.PrimaryArtifactID)
	ctx := context.Background()
	// The spec owes a settlement, which the close runs before its own transaction, and another
	// writer holds the issue's owner lock, which that settlement's transaction waits for: an
	// issue's document is owned by the issue (store.IssueOwner), and every writer of an owner takes
	// that lock in its read phase, before it writes anything (INFRA-636).
	if _, err := database.Pool.Exec(ctx,
		`insert into doc_settlements_pending (artifact_id) values ($1)`, issue.PrimaryArtifactID); err != nil {
		t.Fatalf("mark the spec owed a settlement: %v", err)
	}
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder's transaction: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if err := store.LockOwner(ctx, holder, store.IssueOwner(issue.Key)); err != nil {
		t.Fatalf("hold the issue's owner lock: %v", err)
	}

	closing := map[string]any{"status": "done"}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- keyedRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, closing, "alice", "close-held")
	}()
	waitForDatabaseLocks(t, holder, 1)
	bound := start.Add(keyedHandlerLimit)
	fake.AwaitArmedAt(t, "the close's run bound", bound)
	fake.Set(bound)
	// The owner lock is still held, so a close the bound did not end never answers.
	if response := testwait.Receive(t, "the close held on the issue's owner lock to end at its run's bound", answered); response.Code < http.StatusInternalServerError {
		t.Fatalf("a close held past its run's bound: status=%d body=%s, want a 5xx that releases its key",
			response.Code, response.Body.String())
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the issue's owner lock: %v", err)
	}
	var open bool
	if err := database.Pool.QueryRow(ctx,
		`select closed_at is null from issues where key = $1`, issue.Key).Scan(&open); err != nil {
		t.Fatalf("read the issue: %v", err)
	}
	if !open {
		t.Fatal("the close that ended at its bound closed the issue")
	}
	repeat := keyedRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, closing, "alice", "close-held")
	if repeat.Code != http.StatusOK || replayed(repeat) {
		t.Fatalf("the repeat once the owner lock is free: status=%d body=%s, want the key released and the close run",
			repeat.Code, repeat.Body.String())
	}
}

// closeCommitHold is the document service with a pause between a close's commit and its sweep: the
// sweep's own Join, the first document call it makes, which comes once the close's transaction has
// committed and before the sweep takes the issue's owner lock (sweepClosedIssueAsksTx), closes committed
// and returns only once release is closed. The close's own transaction joins first, and that join
// passes straight through: the count of joins since the hold was installed is what tells the two
// apart, and only the sweep's pauses.
type closeCommitHold struct {
	docs.API
	joins     atomic.Int32
	committed chan struct{}
	release   chan struct{}
}

func (h *closeCommitHold) Join(ctx context.Context, tx pgx.Tx) (context.Context, *docs.Ledger) {
	if h.joins.Add(1) == 2 {
		close(h.committed)
		<-h.release
	}
	return h.API.Join(ctx, tx)
}

// A keyed close whose sweep, after its commit, waits on the issue's owner lock ends at the keyed
// run's bound too, and answers the close it committed. The sweep takes that lock in a transaction
// of its own (sweepClosedIssueAsksTx); on context.WithoutCancel of the keyed request's context it
// waited for as long as another writer held the lock, past the bound, holding the keyed run's
// background-work slot all the while.
func TestAKeyedCloseWhoseSweepIsHeldOnTheIssuesOwnerLockEndsAtItsRunsBound(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	fake := clocktest.New(start)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{clock: fake})
	issue := createInteractionIssue(t, handler, "TEST", "Swept while its row is held", "Context\n")
	awaitSettled(t, database, issue.PrimaryArtifactID)
	ctx := context.Background()
	// Installed once the issue is seeded and settled, so the close's own transaction is the first
	// write to join the document service through the hold and its sweep the second.
	hold := &closeCommitHold{API: served.deps.Docs, committed: make(chan struct{}), release: make(chan struct{})}
	served.deps.Docs = hold
	// Registered after the server's cleanups, so it runs before them: a failure below lets the held
	// sweep go on.
	releaseHold := sync.OnceFunc(func() { close(hold.release) })
	t.Cleanup(releaseHold)

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- keyedRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"status": "done"}, "alice", "close-swept")
	}()
	testwait.Receive(t, "the close to commit", hold.committed)
	// Another writer takes the issue's owner lock between the close's commit and its sweep.
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		releaseHold()
		t.Fatalf("begin the holder's transaction: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if err := store.LockOwner(ctx, holder, store.IssueOwner(issue.Key)); err != nil {
		releaseHold()
		t.Fatalf("hold the issue's owner lock: %v", err)
	}
	releaseHold()
	waitForDatabaseLocks(t, holder, 1)
	bound := start.Add(keyedHandlerLimit)
	fake.AwaitArmedAt(t, "the close's run bound", bound)
	fake.Set(bound)
	// The owner lock is still held, so a sweep the bound did not end never lets the close answer.
	if response := testwait.Receive(t, "the close whose sweep is held on the issue's owner lock to answer at its run's bound", answered); response.Code != http.StatusOK {
		t.Fatalf("a close whose sweep ended at its run's bound: status=%d body=%s, want the committed close's 200",
			response.Code, response.Body.String())
	}
	var closed bool
	if err := database.Pool.QueryRow(ctx,
		`select closed_at is not null from issues where key = $1`, issue.Key).Scan(&closed); err != nil {
		t.Fatalf("read the issue: %v", err)
	}
	if !closed {
		t.Fatal("the close whose sweep ended at its bound left the issue open")
	}
}
