package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The rows of this file are the write class's: a write's own wait on another server - the Envoy
// listener, GitHub - with more writes waiting on it than the write class has units
// (networkWaitLimiter holds networkWaitReads of each class). No admission unit, connection or
// transaction is held across such a wait (awayOnNetwork, awayOnNetworkNeverRefused): while the
// row holds the server, the write class holds no unit and queues no request, a write that waits
// on nothing is admitted at once, and every waiter is answered as before once the server answers.
// Before, each waiter held its write unit for the whole wait, the class filled, and the writes
// behind it were refused 429 OVERLOADED after a write's queue wait.

// writeRequestAs is a write as dispatchRequest sends it for login, with body (nil for none), to
// serve on a goroutine of its own (serveTimed).
func writeRequestAs(t *testing.T, method, target, login string, body any) *http.Request {
	t.Helper()
	request := testRequestAs(t, method, target, login)
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the request body: %v", err)
		}
		request.Body, request.ContentLength = io.NopCloser(bytes.NewReader(data)), int64(len(data))
	}
	return request
}

// bearerWriteRequest is a write as bearerRequest sends it, with the agent token, to serve on a
// goroutine of its own.
func bearerWriteRequest(t *testing.T, method, target string, body any) *http.Request {
	t.Helper()
	request := writeRequestAs(t, method, target, "", body)
	request.Header.Del("X-Dispatch-User")
	request.Header.Set("Authorization", "Bearer agent-token")
	return request
}

// requireWaitersHoldNoWriteUnit requires count waiters to have reached the server the row holds
// (waiting counts them as they arrive) - each was admitted, as the waiters hold no unit to queue
// behind - and the write class to hold no unit and queue no request while they wait. A waiter
// reaches the server a moment after it gives its unit back, so the class is read until it holds
// none; a waiter that held its unit across the wait would hold it until the row lets the server
// answer, which comes after this, and one that queued behind such units would be refused at its
// queue wait without reaching the server. Such a refusal, an answer of waiters (nil for a row
// whose requests were answered before their work reached the server, a detached lane's) before
// every waiter got there, fails the row at once with the refused answer: that waiter will never
// reach the server, so waiting on would only end at the test binary's deadline. Each wait's one
// bound is the test binary's deadline.
func requireWaitersHoldNoWriteUnit(t *testing.T, limiter *admission.Limiter, waiters *waiterGroup, waiting func() int32, count int) {
	t.Helper()
	reached := testwait.Until(t, fmt.Sprintf("all %d waiters to reach the server the row holds (the waiters there), or one to be answered first", count), func() (int32, bool) {
		reached := waiting()
		return reached, int(reached) == count || (waiters != nil && waiters.answered.Load() > 0)
	})
	if int(reached) < count {
		early := waiters.early.Load()
		t.Fatalf("a waiter was answered status=%d after %v body=%s with %d of %d at the server the row holds, want every waiter there holding no write unit: a waiter answered before it got there never will",
			early.status, early.elapsed, strings.TrimSpace(early.body), reached, count)
	}
	testwait.Until(t, fmt.Sprintf("the write class to hold no unit and queue no request with %d waiters on the server the row holds (its units in flight and queued)", count), func() (string, bool) {
		stats := limiter.Stats()[admission.Write]
		return fmt.Sprintf("%d and %d", stats.InFlight, stats.Queued), stats.InFlight == 0 && stats.Queued == 0
	})
}

// requireWritesServed sends networkWaiters project creates in turn while the waiters wait, each a
// write that waits on nothing, and requires each answered 201: admitted, as nothing of the write
// class is held by the waiters. The row holds the waiters until after these answers, so a create
// that queued behind them for a write unit would be refused 429 OVERLOADED once its queue wait ran
// out (admission.DefaultWriteWait), or, where the row's write wait is past any run, would not be
// answered before the row lets them go: the 201 itself, answered while they wait, is the proof,
// not how soon it came.
func requireWritesServed(t *testing.T, handler http.Handler, prefix string) {
	t.Helper()
	for attempt := range networkWaiters {
		key := fmt.Sprintf("%s%d", prefix, attempt)
		answer := serveTimed(handler, writeRequestAs(t, http.MethodPost, "/api/v1/projects", "alice", map[string]string{"key": key, "name": key}))
		if answer.status != http.StatusCreated {
			t.Errorf("a project create, request %d while %d writes wait on another server: status=%d after %v body=%s, want 201",
				attempt+1, networkWaiters, answer.status, answer.elapsed, strings.TrimSpace(answer.body))
		}
	}
}

// requireWritesHoldNoUnitOnService sets the writes of requests waiting on service, which the row
// holds until release, requires them to hold no write unit and queue none while they wait (calls
// counts the ones that reached it) and a write that waits on nothing to be admitted at once
// meanwhile, and every waiter answered want once service answers, the write class holding nothing
// after.
func requireWritesHoldNoUnitOnService(t *testing.T, service string, handler http.Handler, limiter *admission.Limiter,
	requests []*http.Request, calls func() int32, release func(), want int,
) {
	t.Helper()
	waiters := startWaiters(handler, requests)
	t.Cleanup(func() {
		release()
		waiters.working.Wait()
	})
	requireWaitersHoldNoWriteUnit(t, limiter, waiters, calls, len(requests))
	requireWritesServed(t, handler, "WAIT")
	release()
	for _, answer := range waiters.collect(t, len(requests)) {
		if answer.status != want {
			t.Errorf("a write waiting on %s: status=%d after %v body=%s, want %d once it answered",
				service, answer.status, answer.elapsed, strings.TrimSpace(answer.body), want)
		}
	}
	if stats := limiter.Stats()[admission.Write]; stats.InFlight != 0 {
		t.Errorf("once every write waiting on %s was answered the write class holds %d units, want none", service, stats.InFlight)
	}
}

// A lookup before a write: a claim of an issue another session holds asks the listener whether
// that session is still working (judgeHolder, fetchHolderLiveness) before the claim's
// transaction opens, and the listener holds the lookup. The waiters hold no write unit while it
// does, and each is refused 409 ISSUE_CLAIMED, as before, once the listener says the holder is
// live. The write class's queue wait, which six claims arriving at once and their take-backs after
// the release queue in, and the listener client's window on the held lookup are not what the row
// proves, and are past any run.
func TestAClaimWaitingOnTheListenerHoldsNoWriteUnit(t *testing.T) {
	t.Parallel()
	live := &liveRegistry{}
	live.set("session-one")
	address, calls, release := heldService(t, proxyTo(t, claimListener(t, live, nil).URL), func(r *http.Request) bool {
		return strings.HasPrefix(r.URL.Path, "/v1/registry/")
	})
	limiter := networkWaitLimiterPastTheRun()
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: address, envoyTimeout: pastTheRun, admission: limiter})
	key := claimIssueKey(t, handler, "todo")
	if first := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	}); first.Code != http.StatusOK {
		t.Fatalf("first claim: status=%d body=%s", first.Code, first.Body.String())
	}

	requests := make([]*http.Request, 0, networkWaiters)
	for range networkWaiters {
		requests = append(requests, bearerWriteRequest(t, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
			"actor": claimActorBody("session-two", "Second agent"),
		}))
	}
	requireWritesHoldNoUnitOnService(t, "the listener", handler, limiter, requests, calls.Load, release, http.StatusConflict)
}

// sendListener is a stand-in listener whose registry answers every session it is asked about as
// live and advertising every mode, and which takes every send, counting them in sends.
type sendListener struct {
	sends atomic.Int32
}

func newSendListener(t *testing.T) (*httptest.Server, *sendListener) {
	t.Helper()
	state := &sendListener{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			serveLiveRegistrySession(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			fmt.Fprintf(w, `{"event_id":"envelope-%d","recipient":"session"}`, state.sends.Add(1))
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"not found"}`)
		}
	}))
	t.Cleanup(server.Close)
	return server, state
}

// serveLiveRegistrySession answers a stand-in listener's registry read of the session its path
// names as a live session advertising every mode: aside, btw and steer.
func serveLiveRegistrySession(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
	fmt.Fprintf(w, `{"session_id":%q,"title":"worker %s","dir":"/w/legion","machine_id":"m1","roles":[],"capabilities":["aside","btw","steer"],"state":"live","last_seen":%d}`,
		id, id, time.Now().UnixMilli())
}

// isListenerSend is a request of the listener's send route.
func isListenerSend(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send"
}

// A side-effecting call: a keyed direct message is committed and then sent to its session
// (createStoredMessage, deliverMessageResuming), and the listener holds the send. The senders
// hold no write unit while it does. Then the row takes every write unit itself and lets the
// listener answer: each sender's take-back after its send waits for a unit for longer than a
// write's queue wait and is never refused 429, since the listener has taken the message
// (awayOnNetworkNeverRefused), and each is answered 201 once the row gives the units back. A
// keyed repeat of a send is answered from its key and makes no second send.
//
// The write class's queue wait is the deployment's: the take-backs are held past it on purpose.
// So the senders start one at a time, each once the one before is held at the listener, holding
// no unit there (waiterGroup.startInTurn): none queues at admission, where six arriving at once
// queued behind two committing their messages and a loaded machine refused one 429 after its
// two seconds, which then never reached the listener. The listener client's own window on the
// held send is past any run.
func TestAKeyedSendWaitingOnTheListenerHoldsNoWriteUnitAndIsNeverRefusedAfterIt(t *testing.T) {
	t.Parallel()
	listener, sent := newSendListener(t)
	address, calls, release := heldService(t, proxyTo(t, listener.URL), isListenerSend)
	limiter := networkWaitLimiter()
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: address, envoyTimeout: pastTheRun, admission: limiter})
	key := createTestIssue(t, handler, "SEND", "Sends")
	message := func(index int) *http.Request {
		request := writeRequestAs(t, http.MethodPost, "/api/v1/issues/"+key+"/messages", "alice", map[string]any{
			"body": fmt.Sprintf("message %d", index), "target": "session:s1", "delivery": "btw",
		})
		request.Header.Set(contracts.IdempotencyKeyHeader, fmt.Sprintf("send-%d", index))
		return request
	}
	requests := make([]*http.Request, 0, networkWaiters)
	for index := range networkWaiters {
		requests = append(requests, message(index))
	}
	waiters := newWaiterGroup(networkWaiters)
	t.Cleanup(func() {
		release()
		waiters.working.Wait()
	})
	waiters.startInTurn(t, handler, requests, calls.Load)
	requireWaitersHoldNoWriteUnit(t, limiter, waiters, calls.Load, networkWaiters)
	requireWritesServed(t, handler, "SIDE")

	held := make([]*admission.Ticket, 0, networkWaitReads)
	for range networkWaitReads {
		ticket, err := limiter.Admit(t.Context(), admission.Write, 1)
		if err != nil {
			t.Fatalf("the row takes a write unit: %v", err)
		}
		held = append(held, ticket)
	}
	releaseHeld := func() {
		for _, ticket := range held {
			ticket.Release()
		}
	}
	t.Cleanup(releaseHeld)
	release()
	waitUntil(t, fmt.Sprintf("all %d senders to queue to take their write units back after the listener answered, with every unit held", networkWaiters), func() bool {
		return limiter.Stats()[admission.Write].Queued == networkWaiters
	})
	answered := 0
	early := time.After(admission.DefaultWriteWait + time.Second)
waiting:
	for {
		select {
		case answer := <-waiters.answers:
			answered++
			t.Errorf("a send the listener took was answered status=%d after %v body=%s before any write unit came free, want it to wait for one: no take-back after a send is refused",
				answer.status, answer.elapsed, strings.TrimSpace(answer.body))
		case <-early:
			break waiting
		}
	}
	releaseHeld()
	for _, answer := range waiters.collect(t, networkWaiters-answered) {
		if answer.status != http.StatusCreated {
			t.Errorf("a send whose listener answered: status=%d after %v body=%s, want 201", answer.status, answer.elapsed, strings.TrimSpace(answer.body))
		}
	}
	if got := sent.sends.Load(); got != networkWaiters {
		t.Errorf("the listener took %d sends, want %d: one for each message", got, networkWaiters)
	}

	repeat := serveTimed(handler, message(0))
	if repeat.status != http.StatusCreated {
		t.Errorf("a keyed repeat of a send: status=%d body=%s, want 201", repeat.status, strings.TrimSpace(repeat.body))
	}
	if got := sent.sends.Load(); got != networkWaiters {
		t.Errorf("after a keyed repeat of a send the listener took %d sends, want %d: the repeat makes no second send", got, networkWaiters)
	}
	if stats := limiter.Stats()[admission.Write]; stats.InFlight != 0 {
		t.Errorf("once every send was answered the write class holds %d units, want none", stats.InFlight)
	}
}

// heldAnswer is a response writer whose first header write waits until release: a request served
// on it is held answering, still holding whatever it holds, until the row lets it go. answering
// closes once the request has begun to answer.
type heldAnswer struct {
	header    http.Header
	answering chan struct{}
	gate      chan struct{}
	begun     sync.Once
	release   func()
	mu        sync.Mutex
	code      int
	body      bytes.Buffer
}

func newHeldAnswer() *heldAnswer {
	held := &heldAnswer{header: http.Header{}, answering: make(chan struct{}), gate: make(chan struct{})}
	held.release = sync.OnceFunc(func() { close(held.gate) })
	return held
}

func (h *heldAnswer) Header() http.Header { return h.header }

func (h *heldAnswer) WriteHeader(code int) {
	h.begun.Do(func() {
		h.mu.Lock()
		h.code = code
		h.mu.Unlock()
		close(h.answering)
		<-h.gate
	})
}

func (h *heldAnswer) Write(p []byte) (int, error) {
	h.WriteHeader(http.StatusOK)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.body.Write(p)
}

func (h *heldAnswer) status() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.code
}

// requireLaneLeavesTheRequestsUnit serves request, whose work runs on past its answer on a lane
// of its own, on a writer that holds it answering, and requires its lane to reach the listener's
// send (calls reaches want) while the request still answers, holding exactly its own write unit:
// the lane, which waits on the listener through the pairs every send makes, gave nothing of the
// request's unit back, since it runs with no ticket (detachedFromRequest). Then it lets the
// request answer, want.
func requireLaneLeavesTheRequestsUnit(t *testing.T, what string, handler http.Handler, limiter *admission.Limiter,
	calls func() int32, want int32, request *http.Request, status int,
) {
	t.Helper()
	writer := newHeldAnswer()
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.ServeHTTP(writer, request)
	}()
	t.Cleanup(func() {
		writer.release()
		<-served
	})
	testwait.Receive(t, what+" to begin to answer", writer.answering)
	testwait.Until(t, what+"'s lane to reach the listener's send (the sends held)", func() (int32, bool) {
		held := calls()
		return held, held == want
	})
	if stats := limiter.Stats()[admission.Write]; stats.InFlight != 1 {
		t.Errorf("while %s answers and its lane waits on the listener, the write class holds %d units, want 1: the request's own, which nothing off its goroutine parks",
			what, stats.InFlight)
	}
	writer.release()
	<-served
	if writer.status() != status {
		t.Errorf("%s: status=%d body=%s, want %d", what, writer.status(), writer.body.String(), status)
	}
}

// A detached lane: a comment's mention is sent after the comment's answer, on the mention sender's
// lane, and a broadcast's copies on its delivery workers, and the listener holds each send. The
// lanes hold no write unit while they wait (the comments were each answered 201 before), a write
// that waits on nothing is admitted at once, and each send completes once the listener answers.
// A comment and a broadcast whose answer the row holds while their lane waits on the send still
// hold their own unit, and only it: neither lane parks the request's ticket off the request's
// goroutine, as each runs with none (detachedFromRequest).
func TestADetachedLaneRunsWithNoTicketAndHoldsNoWriteUnit(t *testing.T) {
	t.Parallel()
	listener, sent := newSendListener(t)
	address, calls, release := heldService(t, proxyTo(t, listener.URL), isListenerSend)
	limiter := networkWaitLimiter()
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: address, envoyTimeout: time.Minute, admission: limiter})
	t.Cleanup(release)
	key := createTestIssue(t, handler, "LANE", "Lanes")
	comment := func(session string) *http.Request {
		return writeRequestAs(t, http.MethodPost, "/api/v1/issues/"+key+"/comments", "alice", map[string]any{
			"body": "For " + session, "mentions": []map[string]string{{"target": "session:" + session}},
		})
	}
	for index := range networkWaiters {
		answer := serveTimed(handler, comment(fmt.Sprintf("s%d", index)))
		if answer.status != http.StatusCreated {
			t.Fatalf("a comment mentioning s%d: status=%d body=%s", index, answer.status, strings.TrimSpace(answer.body))
		}
	}
	requireWaitersHoldNoWriteUnit(t, limiter, nil, calls.Load, networkWaiters)
	requireWritesServed(t, handler, "LANE")

	requireLaneLeavesTheRequestsUnit(t, "a comment mentioning a session", handler, limiter, calls.Load, networkWaiters+1,
		comment("s6"), http.StatusCreated)
	requireLaneLeavesTheRequestsUnit(t, "a broadcast", handler, limiter, calls.Load, networkWaiters+2,
		writeRequestAs(t, http.MethodPost, "/api/v1/broadcasts", "alice", map[string]any{
			"body": "To everyone", "delivery": "btw", "session_ids": []string{"s7"},
		}), http.StatusCreated)

	release()
	testwait.Until(t, fmt.Sprintf("the listener to take all %d lanes' sends once it answered (the sends taken)", networkWaiters+2), func() (int32, bool) {
		taken := sent.sends.Load()
		return taken, taken == networkWaiters+2
	})
	testwait.Until(t, "the write class to hold no unit once every lane sent (its units in flight)", func() (int, bool) {
		inFlight := limiter.Stats()[admission.Write].InFlight
		return inFlight, inFlight == 0
	})
}

// The import: a source's sync route waits on the import's flight, which waits on GitHub - each
// sync a source of its own, so each its own flight - and GitHub holds each flight's head lookup.
// The routes hold no write unit while they wait, and each is answered 200 with its row once
// GitHub answers. The flights' statements run on the background pool: none takes a request-pool
// connection, and the background pool's count moves. The write class's queue wait, which six
// syncs arriving at once and their take-backs after the release queue in, is not what the row
// proves, and is past any run.
func TestASyncWaitingOnTheImportHoldsNoWriteUnitAndTheImportRunsOnTheBackgroundPool(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHubApp{installations: map[string]string{}, repos: map[string]*fakeArchitectureRepo{}}
	repos := make([]string, 0, networkWaiters)
	for index := range networkWaiters {
		repo := fmt.Sprintf("acme/r%d", index)
		repos = append(repos, repo)
		fake.installations[repo] = "read"
		fake.repos[repo] = &fakeArchitectureRepo{commit: shaArch, files: map[string]string{
			fmt.Sprintf("part%d.md", index): fmt.Sprintf("---\ntitle: Part %d\n---\nOne part.\n", index),
		}}
	}
	github := githubtest.Server(t, githubtest.Spec{Handle: fake.handler(t).ServeHTTP})
	address, calls, release := heldService(t, proxyTo(t, github), func(r *http.Request) bool {
		return strings.Contains(r.URL.Path, "/commits/")
	})
	limiter := networkWaitLimiterPastTheRun()
	options := appInstallationOptions(t, address)
	options.admission = limiter
	handler, database, _ := newTestServer(t, options)
	createTestProject(t, handler, "ARCHW")
	for _, repo := range repos {
		putSource(t, handler, "ARCHW", repo, "main")
	}
	background, err := database.Pool.Background()
	if err != nil {
		t.Fatalf("open the background pool: %v", err)
	}

	requests := make([]*http.Request, 0, networkWaiters)
	for _, repo := range repos {
		requests = append(requests, writeRequestAs(t, http.MethodPost, sourcePath("ARCHW", repo)+"/sync", "alice", nil))
	}
	waiters := startWaiters(handler, requests)
	t.Cleanup(func() {
		release()
		waiters.working.Wait()
	})
	requireWaitersHoldNoWriteUnit(t, limiter, waiters, calls.Load, networkWaiters)
	requireWritesServed(t, handler, "SYNC")
	requestAcquires, backgroundAcquires := database.Pool.Stat().AcquireCount(), background.Stat().AcquireCount()
	release()
	for _, answer := range waiters.collect(t, networkWaiters) {
		if answer.status != http.StatusOK || !strings.Contains(answer.body, `"last_commit":"`+shaArch+`"`) {
			t.Errorf("a sync waiting on the import: status=%d after %v body=%s, want 200 with the source imported at %s",
				answer.status, answer.elapsed, strings.TrimSpace(answer.body), shaArch)
		}
	}
	if got := database.Pool.Stat().AcquireCount() - requestAcquires; got != 0 {
		t.Errorf("the imports took %d request-pool connections once GitHub answered, want none: a route's import runs on the background pool", got)
	}
	if got := background.Stat().AcquireCount() - backgroundAcquires; got == 0 {
		t.Errorf("the imports took no background-pool connection once GitHub answered, want their statements there")
	}
	if stats := limiter.Stats()[admission.Write]; stats.InFlight != 0 {
		t.Errorf("once every sync was answered the write class holds %d units, want none", stats.InFlight)
	}
}

// The read-through: a recorded result's read of the pull requests GitHub names for its commit
// (readThroughPullRequests, delivery.Attributor.ReadThroughCommitPulls) reads GitHub first and
// writes the heads after, in one transaction. GitHub names two pull requests, the first titled
// after the issue and the second after nothing, so the second's attribution reads its commit
// messages, and GitHub holds that read. The records hold no write unit while it does, no
// request-pool connection is held, and no head is written yet: neither pull request's row exists
// while GitHub still answers for the second. Each record is answered 201 once GitHub answers, and
// both heads are written. The write class's queue wait is not what the row proves, and is past any
// run: six records arriving at once queued behind the class's two units, and a loaded machine
// refused one 429 after its two seconds, which then never reached GitHub; their take-backs after
// the release queue behind one another as well.
func TestAResultsReadThroughWaitingOnGitHubHoldsNoWriteUnitAndWritesAfterItsReads(t *testing.T) {
	t.Parallel()
	var issueKey atomic.Pointer[string]
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main"}`, r.PathValue("owner")+"/"+r.PathValue("repo"))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.github.sha")
		fmt.Fprint(w, r.PathValue("sha"))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"number":12,"title":%q,"body":"","html_url":"https://github.com/acme/widgets/pull/12",
			"state":"open","merged":false,"updated_at":"2026-01-01T00:00:00Z",
			"head":{"ref":"legion/work","sha":%q,"repo":{"full_name":"acme/widgets"}},"base":{"ref":"main"}},
			{"number":13,"title":"Tidy the build","body":"","html_url":"https://github.com/acme/widgets/pull/13",
			"state":"open","merged":false,"updated_at":"2026-01-01T00:00:00Z",
			"head":{"ref":"chore/tidy","sha":%q,"repo":{"full_name":"acme/widgets"}},"base":{"ref":"main"}}]`,
			*issueKey.Load()+" work", headCommit, newHeadCommit)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	address, calls, release := heldService(t, proxyTo(t, upstream.URL), func(r *http.Request) bool {
		return strings.HasSuffix(r.URL.Path, "/pulls/13/commits")
	})
	limiter := networkWaitLimiterPastTheRun()
	options := appInstallationOptions(t, address)
	options.admission = limiter
	handler, database, _ := newTestServer(t, options)
	issue := issueInRepository(t, handler, "RTHR", "acme/widgets", "Read through")
	issueKey.Store(&issue.Key)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)

	requests := make([]*http.Request, 0, networkWaiters)
	for range networkWaiters {
		requests = append(requests, writeRequestAs(t, http.MethodPost, "/api/v1/issues/"+issue.Key+"/results", "alice",
			resultBody("k1", "pass", headCommit, nil)))
	}
	waiters := startWaiters(handler, requests)
	t.Cleanup(func() {
		release()
		waiters.working.Wait()
	})
	requireWaitersHoldNoWriteUnit(t, limiter, waiters, calls.Load, networkWaiters)
	testwait.Until(t, fmt.Sprintf("the request pool to lend no connection with %d read-throughs waiting on GitHub: none is held across the wait (the connections lent)", networkWaiters), func() (int32, bool) {
		lent := database.Pool.Stat().AcquiredConns()
		return lent, lent == 0
	})
	heads := func() int {
		var count int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from pull_request_heads where repo = 'acme/widgets'`).Scan(&count); err != nil {
			t.Fatalf("count the heads the read-throughs wrote: %v", err)
		}
		return count
	}
	if written := heads(); written != 0 {
		t.Errorf("while GitHub still answers for the second pull request, %d heads are written, want none: the read-through writes once it has read", written)
	}
	requireWritesServed(t, handler, "READ")
	release()
	for _, answer := range waiters.collect(t, networkWaiters) {
		if answer.status != http.StatusCreated {
			t.Errorf("a record whose read-through waited on GitHub: status=%d after %v body=%s, want 201",
				answer.status, answer.elapsed, strings.TrimSpace(answer.body))
		}
	}
	if written := heads(); written != 2 {
		t.Errorf("once GitHub answered the read-throughs wrote %d heads, want 2: both pull requests GitHub named", written)
	}
	if stats := limiter.Stats()[admission.Write]; stats.InFlight != 0 {
		t.Errorf("once every record was answered the write class holds %d units, want none", stats.InFlight)
	}
}

// A side-effecting call through a listener take-over: the listener answers every send 503 while
// the row holds its take-over, so each direct message's sender tries again after each backoff
// (takeoverRetry.again: a quarter second, doubling to two) inside a take-over window longer than
// the row's hold, resolving the session again and renewing its claim between tries. The senders
// hold no write unit across a send, a backoff or a resolution (takeoverRetry.retry runs each
// stretch of them in one wait), only for the claim's renewal between stretches, so while the
// take-over lasts the write class comes to hold no unit and queue no request, and a write that
// waits on nothing is admitted at once. Once the take-over ends each sender's next try lands and
// it is answered 201. Before, each sender held its unit across every backoff, the class filled,
// and the writes behind it were refused 429 after a write's queue wait.
//
// The row proves this by the order of events, so the bounds a loaded machine could run out first
// are past any run: the take-over window, the listener client's own window on each call, and the
// write class's queue wait, which the senders queued for its two units to renew their claims, and
// to take them back once the take-over ends, wait in.
func TestASendThroughAListenerTakeOverHoldsNoWriteUnitAcrossItsBackoffs(t *testing.T) {
	t.Parallel()
	var takingOver atomic.Bool
	takingOver.Store(true)
	var sends, refused atomic.Int32
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			serveLiveRegistrySession(w, r)
		case isListenerSend(r) && takingOver.Load():
			refused.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":"the listener is being replaced"}`)
		case isListenerSend(r):
			fmt.Fprintf(w, `{"event_id":"envelope-%d","recipient":"session"}`, sends.Add(1))
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"not found"}`)
		}
	}))
	t.Cleanup(listener.Close)
	limiter := admission.New(admission.Limits{Read: networkWaitReads, Write: networkWaitReads, WriteWait: pastTheRun})
	handler, _, _ := newTestServer(t, testServerOptions{
		envoyURL: listener.URL, envoyTimeout: pastTheRun, admission: limiter, takeoverWindow: pastTheRun,
	})
	key := createTestIssue(t, handler, "TAKE", "Take-over")
	requests := make([]*http.Request, 0, networkWaiters)
	for index := range networkWaiters {
		requests = append(requests, writeRequestAs(t, http.MethodPost, "/api/v1/issues/"+key+"/messages", "alice", map[string]any{
			"body": fmt.Sprintf("message %d", index), "target": "session:s1", "delivery": "btw",
		}))
	}
	waiters := startWaiters(handler, requests)
	t.Cleanup(func() {
		takingOver.Store(false)
		waiters.working.Wait()
	})
	// Every sender has been refused twice, so each is in its take-over loop, backing off.
	testwait.Until(t, fmt.Sprintf("the listener to refuse at least %d sends during its take-over, two for each sender (the sends refused)", 2*networkWaiters), func() (int32, bool) {
		got := refused.Load()
		return got, got >= 2*networkWaiters
	})
	testwait.Until(t, fmt.Sprintf("the write class to hold no unit and queue no request with %d senders backing off through a listener take-over (its units in flight and queued)", networkWaiters), func() (string, bool) {
		stats := limiter.Stats()[admission.Write]
		return fmt.Sprintf("%d and %d", stats.InFlight, stats.Queued), stats.InFlight == 0 && stats.Queued == 0
	})
	requireWritesServed(t, handler, "BACK")
	takingOver.Store(false)
	for _, answer := range waiters.collect(t, networkWaiters) {
		if answer.status != http.StatusCreated {
			t.Errorf("a send through a listener take-over: status=%d after %v body=%s, want 201 once the take-over ended",
				answer.status, answer.elapsed, strings.TrimSpace(answer.body))
		}
	}
	if got := sends.Load(); got != networkWaiters {
		t.Errorf("once the take-over ended the listener took %d sends, want %d: one for each message", got, networkWaiters)
	}
	if stats := limiter.Stats()[admission.Write]; stats.InFlight != 0 {
		t.Errorf("once every sender was answered the write class holds %d units, want none", stats.InFlight)
	}
}

// ticketSeeingAnswerer is the going routes' fake answerer, noting how many runs it ran and how
// many of them ran on a context carrying an admission ticket.
type ticketSeeingAnswerer struct {
	*fakeAnswerer
	runs, carried atomic.Int32
}

func (a *ticketSeeingAnswerer) Run(ctx context.Context, request going.Request, emit func(going.Event) error) error {
	a.runs.Add(1)
	if admission.TicketFrom(ctx) != nil {
		a.carried.Add(1)
	}
	return a.fakeAnswerer.Run(ctx, request, emit)
}

// A going run is detached from its request (createGoingRequest) and outlives it, and its searches
// wait on the embedder through the pairs a request's own read makes (runSearch's awayOnNetwork):
// its context carries no admission ticket (detachedFromRequest), so nothing of the run can park or
// resume its request's ticket off the request's goroutine.
func TestAGoingRunCarriesNoAdmissionTicketOfItsRequest(t *testing.T) {
	t.Parallel()
	answerer := &ticketSeeingAnswerer{fakeAnswerer: &fakeAnswerer{frames: []going.Event{going.End(1, 0)}}}
	rig := newGoingRoutesRig(t, answerer)
	seedGoingProject(t, rig.handler, "GOT")
	response := rig.ask(t, "alice", map[string]any{"words": "when do the pelicans arrive?", "project": "GOT"})
	if response.Code != http.StatusCreated {
		t.Fatalf("ask: status=%d body=%s", response.Code, response.Body.String())
	}
	rig.awaitTerminal(t, decodeBody[askedRun](t, response).ID)
	if runs, carried := answerer.runs.Load(), answerer.carried.Load(); runs != 1 || carried != 0 {
		t.Errorf("%d runs ran, %d of them on a context carrying an admission ticket, want 1 and none", runs, carried)
	}
}
