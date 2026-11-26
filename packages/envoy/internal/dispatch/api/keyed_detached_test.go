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
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// keyedPostContext posts body to the rig's route on server as alice under key, under ctx: the
// request, the answer's head and its body all end when ctx does. It answers the status and body,
// or the client's own failure.
func keyedPostContext(ctx context.Context, server *httptest.Server, path, body, key string) (int, string, http.Header, error) {
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

// keyedReply is what keyedPostContext answered: the status, body and headers, or the client's
// failure.
type keyedReply struct {
	status int
	body   string
	header http.Header
	err    error
}

// keyedPostAnswered is keyedPostContext under the test's own context, which carries no deadline and
// ends only when the test does, its answer taken with testwait.Receive: its one bound is the test
// binary's deadline less testwait's margin, so a server a loaded machine slows still answers, and
// one that never answers fails the test by name, with what.
func keyedPostAnswered(t *testing.T, what string, server *httptest.Server, path, body, key string) keyedReply {
	t.Helper()
	ctx := t.Context()
	answered := make(chan keyedReply, 1)
	go func() {
		status, body, header, err := keyedPostContext(ctx, server, path, body, key)
		answered <- keyedReply{status: status, body: body, header: header, err: err}
	}()
	return testwait.Receive(t, what, answered)
}

// INFRA-258. A write the server takes longer to finish than its client waits still lands, once:
// the handler runs on past the client's giving up, and the client's repeat under its key is told
// the first run is still going (409 KEY_IN_FLIGHT), then answered the first run's own stored
// answer. A handler bound to its client's cancellation rolled back at the client's deadline, and
// every repeat started again from nothing, so such a write could never land. The run is held at a
// gate the test lets go only once the client has given up and a repeat was told the run is still
// going, so the order is the test's and no clock decides it.
func TestAKeyedWriteSlowerThanItsClientFinishesAndIsReplayed(t *testing.T) {
	t.Parallel()
	running := make(chan struct{})
	release, releaseOnce := heldUntilReleased(t)
	rig := newWritingRig(t, func(run int32, w http.ResponseWriter, r *http.Request, write rigWrite) {
		// Only the first run is held: a later run, which only a key the first run lost makes,
		// answers at once, so a repeat that ran it fails by its answer rather than waiting.
		if run == 1 {
			close(running)
			select {
			case <-release:
			case <-r.Context().Done():
				writeError(w, "CUT_OFF", http.StatusServiceUnavailable, "the run was cut off with its client")
				return
			}
		}
		if err := write(r.Context(), nil); err != nil {
			t.Errorf("run %d's write: %v", run, err)
		}
		answerRun(run, w, r)
	})
	server := httptest.NewServer(rig.handler)
	t.Cleanup(server.Close)

	// The first attempt's client gives up while its run is held, at the moment the test cancels it.
	client, giveUp := context.WithCancel(context.Background())
	defer giveUp()
	first := httptest.NewRequestWithContext(client, http.MethodPost, "/test/a/writes", strings.NewReader(`{"n":1}`))
	first.Header.Set("Content-Type", "application/json")
	first.Header.Set("X-Dispatch-User", "alice")
	first.Header.Set(contracts.IdempotencyKeyHeader, "slow-press")
	served := make(chan struct{})
	go func() {
		defer close(served)
		rig.handler.ServeHTTP(httptest.NewRecorder(), first)
	}()
	testwait.Receive(t, "the first attempt's run to start", running)
	giveUp()
	repeat := keyedPostAnswered(t, "the answer to the repeat after the first attempt's client gave up", server, "/test/a/writes", `{"n":1}`, "slow-press")
	if repeat.err != nil || repeat.status != http.StatusConflict || !strings.Contains(repeat.body, contracts.IdempotencyKeyInFlight) ||
		repeat.header.Get("Retry-After") != "1" {
		t.Fatalf("the repeat after the client gave up: %d %s (%v), want 409 %s: the first run is still going",
			repeat.status, repeat.body, repeat.err, contracts.IdempotencyKeyInFlight)
	}
	releaseOnce()
	answer := testwait.UntilPolling(t, 20*time.Millisecond, "a repeat answered other than 409 once the first run was let go (the last answer)", func() (keyedReply, bool) {
		later := keyedPostAnswered(t, "the answer to a later repeat", server, "/test/a/writes", `{"n":1}`, "slow-press")
		if later.err != nil {
			t.Fatalf("a later repeat: %v", later.err)
		}
		return later, later.status != http.StatusConflict
	})
	if answer.status != http.StatusCreated || answer.header.Get(contracts.IdempotencyReplayedHeader) != "?1" || !strings.Contains(answer.body, `"run":1`) {
		t.Fatalf("the repeat once the first run finished: %d %s replayed=%q, want the first run's 201 replayed",
			answer.status, answer.body, answer.header.Get(contracts.IdempotencyReplayedHeader))
	}
	testwait.Receive(t, "the first attempt's request to end", served)
	if runs, writes := rig.runs.Load(), rig.writes(t); runs != 1 || len(writes) != 1 {
		t.Fatalf("the handler ran %d times and wrote %v, want once and one row", runs, writes)
	}
}

// A run that outlives its key's lease keeps the key: it renews the lease while it runs, so a repeat
// past the lease is still told the first run is going rather than taking the key over and making
// the write a second time. Only a run whose renewals stopped - its process died - is taken over.
// The lease is the deployment's minute and is read against the database's now(), so the test moves
// a lease back past it rather than waiting one out, and a renewal a loaded machine makes late still
// keeps the key.
func TestARunningHandlerRenewsItsKeyPastTheLease(t *testing.T) {
	t.Parallel()
	release, releaseOnce := heldUntilReleased(t)
	running := make(chan struct{})
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		if run == 1 {
			close(running)
			<-release
		}
		answerRun(run, w, r)
	})
	rig.server.requestKeyRenewal = 200 * time.Millisecond
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.send(t, "a", nil, "alice", "long-run") }()
	<-running
	// A whole lease with no renewal, as the run's lease stands once that long has passed: the wait
	// is for the running handler's next renewal to make it live again, which a run that does not
	// renew never does.
	lapseRequestKeyLease(t, rig, "long-run")
	testwait.UntilPolling(t, 20*time.Millisecond, "the running handler to renew its key's lease (live)", func() (bool, bool) {
		var live bool
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select coalesce(leased_at >= now() - make_interval(secs => $2), false) from request_keys where idempotency_key = $1
		`, "long-run", rig.server.keyLease().Seconds()).Scan(&live); err != nil {
			t.Fatalf("read the lease of the key long-run: %v", err)
		}
		return live, live
	})
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
	holdRequestKey(t, rig.database, heldKey{login: "alice", key: "dead-run", request: dead, pattern: keyedRigPattern})
	if inside := rig.send(t, "a", nil, "alice", "dead-run"); inside.Code != http.StatusConflict {
		t.Fatalf("a repeat inside a dead holder's lease: status=%d body=%s, want 409", inside.Code, inside.Body.String())
	}
	lapseRequestKeyLease(t, rig, "dead-run")
	if takeover := rig.send(t, "a", nil, "alice", "dead-run"); takeover.Code != http.StatusCreated || replayed(takeover) {
		t.Fatalf("a repeat once the dead holder's lease lapsed: status=%d body=%s, want the request run", takeover.Code, takeover.Body.String())
	}
}

// lapseRequestKeyLease moves key's lease two minutes back, past a whole lease (IdempotencyLease),
// as that long with no renewal leaves it: the keyed layer reads a lease against the database's
// now().
func lapseRequestKeyLease(t *testing.T, rig *keyedRig, key string) {
	t.Helper()
	tag, err := rig.database.Pool.Exec(context.Background(),
		`update request_keys set leased_at = now() - interval '2 minutes' where idempotency_key = $1 and leased_at is not null`, key)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("lapse the lease of the key %s: %v (%d rows)", key, err, tag.RowsAffected())
	}
}

// The handler's context is the server's, not the client's: the client's cancellation mid-run does
// not reach it, and the server's own bound, keyedHandlerLimit, does. The bound is read as the
// deadline the server set: keyedHandlerLimit after a moment between the request's start and the
// handler's entry, which the server's own start of the run lies between, however long either took.
func TestAKeyedHandlerRunsUnderTheServersOwnBound(t *testing.T) {
	t.Parallel()
	running, proceed := make(chan struct{}), make(chan struct{})
	type seen struct {
		err      error
		entered  time.Time
		deadline time.Time
		bounded  bool
	}
	saw := make(chan seen, 1)
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		entered := time.Now()
		close(running)
		<-proceed
		observed := seen{err: r.Context().Err(), entered: entered}
		observed.deadline, observed.bounded = r.Context().Deadline()
		saw <- observed
		answerRun(run, w, r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/test/a/writes", bytes.NewReader([]byte("{}")))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set(contracts.IdempotencyKeyHeader, "bounded")
	response := httptest.NewRecorder()
	began := time.Now()
	served := make(chan struct{})
	go func() {
		defer close(served)
		rig.handler.ServeHTTP(response, request)
	}()
	<-running
	cancel()
	close(proceed)
	<-served
	observed := <-saw
	if observed.err != nil || response.Code != http.StatusCreated {
		t.Fatalf("a run whose client went away mid-run: context error %v, status %d, want it run to its answer",
			observed.err, response.Code)
	}
	if earliest, latest := began.Add(keyedHandlerLimit), observed.entered.Add(keyedHandlerLimit); !observed.bounded ||
		observed.deadline.Before(earliest) || observed.deadline.After(latest) {
		t.Fatalf("the handler's deadline is %v (set: %v), want the server's %v bound from the run's start, between %v and %v",
			observed.deadline, observed.bounded, keyedHandlerLimit, earliest, latest)
	}
}

// A keyed handler still running is part of the server's background work: shutdown's wait does not
// return before it does, so the database pool is never closed under it.
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
	<-running
	waited := make(chan struct{})
	go func() {
		rig.server.detached.wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("the background wait returned while a keyed handler still ran")
	case <-time.After(500 * time.Millisecond):
	}
	releaseOnce()
	awaitResponse(t, answered)
	testwait.Receive(t, "the background wait to return once the handler answered", waited)
}

// backgroundWork's wait returns only once nothing runs, however work starts and ends around it:
// work that starts while it waits, which a sync.WaitGroup forbids, is waited for too.
func TestTheBackgroundWaitWaitsForWorkStartedWhileItWaits(t *testing.T) {
	t.Parallel()
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
	select {
	case <-waited:
		t.Fatal("the wait returned with work still running")
	case <-time.After(100 * time.Millisecond):
	}
	work.done()
	testwait.Receive(t, "the wait to return once the work ended", waited)
}

// The slow-write test hook delays every keyed run of the method and path it was armed for, on the
// handler's own context, and is mounted only under test hooks. A run whose context ends during the
// delay never reaches its handler.
func TestTheSlowWriteHookDelaysEveryKeyedRunOfItsPath(t *testing.T) {
	t.Parallel()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{testHooks: true})
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
	arm(700)
	// Each armed run is held to its arm as the hook records the wait it made (slowWriteRunsOn, read
	// on each side of the run), not by a clock around the request, which a run that skipped its
	// delay on a slow machine would pass. keyedHandlerLimit is ten minutes, so load cannot turn an
	// armed run into a 503 either.
	for _, key := range []string{"slow-1", "slow-2"} {
		_, before := served.slowWriteRunsOn(http.MethodPost + " " + path)
		if created := keyedRequest(t, handler, http.MethodPost, path, map[string]any{"body": key}, "alice", key); created.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", key, created.Code, created.Body.String())
		}
		if _, after := served.slowWriteRunsOn(http.MethodPost + " " + path); after-before < 700*time.Millisecond {
			t.Fatalf("%s's run waited %v in the hook's delay, as the hook records it, inside the armed 700ms", key, after-before)
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
	if armedRuns != 2 {
		t.Fatalf("the hook delayed %d runs of %s, waiting %v altogether, want the two armed runs",
			armedRuns, route, armedWaited)
	}
	arm(0)
	if delay, stillArmed := served.slowWrites.Load(route); stillArmed {
		t.Fatalf("%s is still armed for %v after delay_ms 0, want it disarmed", route, delay)
	}
	// No upper bound on how long the disarmed run takes: the row drives the handler in this
	// process, so go test's own timeout is the guard against it hanging, and a wall-clock budget
	// here would only fail a loaded runner.
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
// would answer 500.
func TestAnInFlightRepeatIsJudgedWithoutSpoolingItsBody(t *testing.T) {
	const pattern = "/api/v1/issues/{key}/artifacts"
	rig := newKeyedRigAt(t, pattern, answerRun)
	body := `{"name":"big.bin","content":"` + strings.Repeat("a", 2<<20) + `"}`
	first := httptest.NewRequest(http.MethodPost, "/api/v1/issues/UP-1/artifacts", strings.NewReader(body))
	first.Header.Set("Content-Type", "application/json")
	first.SetPathValue("key", "UP-1")
	holdRequestKey(t, rig.database, heldKey{login: "alice", key: "slow-upload", request: first, pattern: pattern})

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
	// Long enough for a claim bound to the client's context to be cancelled in the database.
	time.Sleep(300 * time.Millisecond)
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
	running := make(chan struct{})
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		if run == 1 {
			close(running)
			<-release
		}
		answerRun(run, w, r)
	})
	rig.server.requestKeyLease, rig.server.requestKeyRenewal, rig.server.requestKeyRunLimit =
		2*time.Second, 200*time.Millisecond, time.Second
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.send(t, "a", nil, "alice", "overrun") }()
	<-running
	// The limit ends the run's renewals, so its lease lapses a lease after the last of them while
	// the handler still runs; the wait reads the lease as the keyed layer does, against the
	// database's now(), and a run that renewed on past its limit never lets it lapse.
	testwait.UntilPolling(t, 20*time.Millisecond, "the overrunning run's lease to lapse while its handler still runs (lapsed)", func() (bool, bool) {
		var lapsed bool
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select coalesce(leased_at < now() - make_interval(secs => $3), false)
			from request_keys where login = $1 and idempotency_key = $2
		`, "alice", "overrun", rig.server.keyLease().Seconds()).Scan(&lapsed); err != nil {
			t.Fatalf("read the lease of the key overrun: %v", err)
		}
		return lapsed, lapsed
	})
	takeover := rig.send(t, "a", nil, "alice", "overrun")
	if takeover.Code != http.StatusCreated || replayed(takeover) || !strings.Contains(takeover.Body.String(), `"run":2`) {
		t.Fatalf("a repeat a lease past the run's limit: status=%d body=%s, want the key taken over and run again",
			takeover.Code, takeover.Body.String())
	}
	releaseOnce()
	awaitResponse(t, answered)
}

// A keyed close whose settlement, before its transaction, waits on a lock ends at the keyed run's
// bound (keyedRunLimit), as every keyed handler does, and releases its key, so the repeat runs it
// once the lock is free. The close settles the issue's documents before its transaction and
// sweeps them after it (the next test), on a context that must outlive the client; a keyed
// request's context already does, and context.WithoutCancel on it dropped the bound with the
// cancellation, so a close held on the issue's owner lock kept its background-work slot, and the
// shutdown's wait for it, as long as that lock stayed held.
//
// The bound runs on the server's clock (Deps.Clock), which the test moves past it only once the
// close waits on the lock, and never during the repeat: how long a loaded machine takes to bring
// either close there decides nothing.
func TestAKeyedCloseWhoseSettlementIsHeldOnTheIssuesOwnerLockEndsAtItsRunsBound(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{clock: fake})
	served.requestKeyRunLimit = time.Second
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
	bound := fake.Now().Add(served.requestKeyRunLimit)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- keyedRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, closing, "alice", "close-held")
	}()
	if early, answeredFirst, _ := storetest.AwaitOwnerLockWaiter(t, database, "the close's settlement", answered); answeredFirst {
		t.Fatalf("the close answered status=%d body=%s before its settlement waited on the issue's owner lock, with its run's bound not yet come",
			early.Code, early.Body.String())
	}
	requireRunBoundArmed(t, fake, bound)
	fake.Set(bound)
	// The owner lock is still held, so a close its run's bound did not end never answers.
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
// committed and before the sweep takes the issue's owner lock (sweepClosedIssueAsksTx), records the
// backend the sweep's transaction runs on in sweep, closes committed and returns only once release
// is closed. The close's own transaction joins first, and that join passes straight through: the
// count of joins since the hold was installed is what tells the two apart, and only the sweep's
// pauses.
type closeCommitHold struct {
	docs.API
	joins     atomic.Int32
	sweep     uint32
	committed chan struct{}
	release   chan struct{}
}

func (h *closeCommitHold) Join(ctx context.Context, tx pgx.Tx) (context.Context, *docs.Ledger) {
	if h.joins.Add(1) == 2 {
		h.sweep = tx.Conn().PgConn().PID()
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
//
// The bound runs on the server's clock (Deps.Clock), which the test moves past it only once the
// sweep waits on the lock. On the wall clock the bound came a second after the run began whatever
// the close had done by then, and a close a loaded machine kept past that second ended before its
// sweep, which the test then waited for until the test binary's deadline (INFRA-3904).
func TestAKeyedCloseWhoseSweepIsHeldOnTheIssuesOwnerLockEndsAtItsRunsBound(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Now())
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{clock: fake})
	served.requestKeyRunLimit = time.Second
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

	bound := fake.Now().Add(served.requestKeyRunLimit)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- keyedRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"status": "done"}, "alice", "close-swept")
	}()
	// The sweep's join, or the close's answer if it ends without sweeping: the held join keeps the
	// close from answering, so only one of the two comes.
	if early := testwait.Until(t, "the close to commit and its sweep to join", func() (*httptest.ResponseRecorder, bool) {
		select {
		case <-hold.committed:
			return nil, true
		case response := <-answered:
			return response, true
		default:
			return nil, false
		}
	}); early != nil {
		t.Fatalf("the close answered status=%d body=%s without sweeping, with its run's bound not yet come",
			early.Code, early.Body.String())
	}
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
	if early, answeredFirst := storetest.WaitForQueuedBehind(t, holder, hold.sweep, answered); answeredFirst {
		t.Fatalf("the close answered status=%d body=%s before its sweep waited on the issue's owner lock, with its run's bound not yet come",
			early.Code, early.Body.String())
	}
	requireRunBoundArmed(t, fake, bound)
	fake.Set(bound)
	// The owner lock is still held, so a sweep its run's bound did not end never lets the close answer.
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

// requireRunBoundArmed fails the test unless the keyed run's bound, at, is pending on fake, the
// server's clock. The run arms it before its handler starts (keyedWrites), so by the time the
// close waits on a lock it is armed or it never will be, and the test names what is pending
// rather than waiting for it.
func requireRunBoundArmed(t *testing.T, fake *clocktest.Fake, at time.Time) {
	t.Helper()
	if armed := fake.Armed(); !slices.ContainsFunc(armed, at.Equal) {
		t.Fatalf("the close's run bound, %v, is not pending on the server's clock, which holds %v: the run is not bounded on that clock",
			at.Format(time.RFC3339Nano), armed)
	}
}
