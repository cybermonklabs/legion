// going_agent_test.go holds the server's side of agent/v1 as the agent meets it: the client of
// going/agentwire speaking the six calls over a real Unix socket to a server on a real database,
// whose NOTIFYs a listener hub delivers as cmd/dispatch's does.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testtmp"
)

// goingAgentHook stands in front of one route of the server's table called in-process: it answers
// the request itself, or hands it on to the table (inner).
type goingAgentHook func(w http.ResponseWriter, r *http.Request, inner http.Handler)

// goingAgentHooks are the hooks a test sets, by method and path.
type goingAgentHooks struct {
	mu    sync.Mutex
	hooks map[string]goingAgentHook
}

// set stands hook in front of method path until the test ends.
func (h *goingAgentHooks) set(t *testing.T, method, path string, hook goingAgentHook) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hooks[method+" "+path] = hook
	t.Cleanup(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.hooks, method+" "+path)
	})
}

// wrap is inner with the hooks in front of it.
func (h *goingAgentHooks) wrap(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		hook := h.hooks[r.Method+" "+r.URL.Path]
		h.mu.Unlock()
		if hook != nil {
			hook(w, r, inner)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// goingAgentRig is a server whose agent's socket listens under a short temporary directory, the
// client of agentwire holding its secret, hooks in front of its route table called in-process,
// and the plan rows it approved (approve), by run, tool and arguments, which a call of that write
// names (call).
type goingAgentRig struct {
	goingWritesRig
	hooks  *goingAgentHooks
	agent  *AgentListener
	client agentwire.Client
	rows   *sync.Map
}

// newGoingAgentRig is the rig, its claims waiting 2 s for a ready run where the server waits 25 s;
// adjust changes the listener before it serves.
func newGoingAgentRig(t *testing.T, adjust func(*AgentListener)) goingAgentRig {
	t.Helper()
	return newGoingAgentRigOver(t, newGoingWritesRig(t), adjust)
}

// newGoingAgentRigOver is the rig over rig, a writes rig whose Envoy listener the test chose.
func newGoingAgentRigOver(t *testing.T, rig goingWritesRig, adjust func(*AgentListener)) goingAgentRig {
	t.Helper()
	createInteractionIssue(t, rig.mux, "DEMO", "The project's first issue", "Context\n")
	hooks := &goingAgentHooks{hooks: map[string]goingAgentHook{}}
	rig.served.inProcess = hooks.wrap(rig.served.inProcess)
	listenGoingRuns(t, rig.database, rig.served)
	agent, err := rig.served.newAgentListener(testtmp.Dir(t, "agent.sock"), false)
	if err != nil {
		t.Fatalf("listen for the agent: %v", err)
	}
	agent.claimWait = 2 * time.Second
	if adjust != nil {
		adjust(agent)
	}
	agent.start()
	t.Cleanup(func() { agent.Close(time.Now().Add(time.Minute)) })
	return goingAgentRig{goingWritesRig: rig, hooks: hooks, agent: agent,
		client: agentwire.Client{Path: agent.Path(), Secret: agent.Secret()}, rows: &sync.Map{}}
}

// listenGoingRuns runs a listener hub on database that delivers GoingRunsChannel and
// GoingRunEventsChannel to served's signals, as cmd/dispatch's does, and waits for its first
// LISTEN.
func listenGoingRuns(t *testing.T, database *store.Store, served *server) {
	t.Helper()
	listening := make(chan struct{}, 1)
	hub := &pglisten.Hub{Owner: "going-agent-test", Dial: func(ctx context.Context) (*pgx.Conn, error) {
		return database.Pool.Dedicated(ctx, "going-agent-test")
	}}
	hub.Handle(GoingRunsChannel, served.goingRuns.Notify)
	hub.Handle(GoingRunEventsChannel, served.goingRunEvents.Notify)
	hub.OnConnected(func() {
		served.goingRuns.Wake()
		served.goingRunEvents.Wake()
		select {
		case listening <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-ran
	})
	select {
	case <-listening:
	case <-time.After(30 * time.Second):
		t.Fatal("the listener hub did not LISTEN within 30 s")
	}
}

func (rig goingAgentRig) exec(t *testing.T, statement string, arguments ...any) {
	t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), statement, arguments...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

func (rig goingAgentRig) count(t *testing.T, query string, arguments ...any) int {
	t.Helper()
	var count int
	if err := rig.database.Pool.QueryRow(context.Background(), query, arguments...).Scan(&count); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return count
}

// queued records a queued run of login's, of shape, asked in words and granted by the header
// identity, with no page and no kind (a run of the words), the first of a conversation of its own,
// and answers its id.
func (rig goingAgentRig) queued(t *testing.T, login string, shape int, words string) string {
	t.Helper()
	id := uuid.NewString()
	rig.exec(t, `
		insert into going_runs (id, author_login, words, conversation_id, level, mode, state, shape, created_at)
		values ($1::uuid, $2, $3, $1::uuid, 'standard', 'ask', 'queued', $4, clock_timestamp())
	`, id, login, words, shape)
	rig.exec(t, `insert into going_run_grants (run_id, kind, login) values ($1::uuid, 'header', $2)`, id, login)
	return id
}

// working records a run of login's asked in words that an agent works on under a lease, and
// answers the run and its lease token.
func (rig goingAgentRig) working(t *testing.T, login, words string) (run, token string) {
	t.Helper()
	run, token = rig.queued(t, login, goingRunShape, words), uuid.NewString()
	rig.exec(t, `
		update going_runs set state = 'working', lease_token = $2::uuid, lease_owner = 'the-agent-of-this-test',
		  lease_ends_at = now() + interval '10 minutes'
		where id = $1::uuid
	`, run, token)
	return run, token
}

// approve records an approved row of run's plan: tool with arguments, which a later call of that
// write names (call).
func (rig goingAgentRig) approve(t *testing.T, run, tool string, arguments map[string]any) {
	t.Helper()
	change, err := json.Marshal(map[string]any{"tool": tool, "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	rig.exec(t, `insert into going_plans (run_id, plan, state) values ($1::uuid, 1, 'approved') on conflict do nothing`, run)
	var row int
	if err := rig.database.Pool.QueryRow(context.Background(), `
		insert into going_plan_rows (run_id, plan, row, change, state)
		select $1::uuid, 1, coalesce(max(row), 0) + 1, $2::jsonb, 'approved' from going_plan_rows where run_id = $1::uuid and plan = 1
		returning row
	`, run, string(change)).Scan(&row); err != nil {
		t.Fatalf("approve %s: %v", tool, err)
	}
	rig.rows.Store(run+" "+tool+" "+string(encoded), agentwire.PlanRow{Plan: 1, Row: row})
}

// settleQueue stops every queued and working run, so a claim of the test's next row takes only
// what that row queues.
func (rig goingAgentRig) settleQueue(t *testing.T) {
	t.Helper()
	rig.exec(t, `update going_runs set state = 'stopped', lease_token = null, lease_owner = null, lease_ends_at = null
		where state in ('queued', 'working')`)
}

// claim is one claim of instance.
func (rig goingAgentRig) claim(t *testing.T, instance string) *agentwire.ClaimedRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	answer, err := rig.client.Claim(ctx, agentwire.ClaimRequest{Instance: instance})
	if err != nil {
		t.Fatalf("claim for %s: %v", instance, err)
	}
	return answer.Run
}

// routeCall is a route call of run under token: tool with arguments, keyed step and call.
func goingRouteCall(run, token string, step, call int, tool string, arguments map[string]any) agentwire.CallRequest {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		panic(err)
	}
	return agentwire.CallRequest{Run: run, LeaseToken: token, Step: step, Call: call, Kind: agentwire.KindRoute, Tool: tool, Arguments: encoded}
}

// call makes one call, waiting at most 90 s for it. A write of a row the rig approved names it.
func (rig goingAgentRig) call(request agentwire.CallRequest) (agentwire.CallAnswer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if request.Row == nil {
		if row, approved := rig.rows.Load(request.Run + " " + request.Tool + " " + string(request.Arguments)); approved {
			named := row.(agentwire.PlanRow)
			request.Row = &named
		}
	}
	return rig.client.Call(ctx, request)
}

// requireAgentCode fails t unless err is the server's refusal of code.
func requireAgentCode(t *testing.T, err error, code, what string) {
	t.Helper()
	var refused *agentwire.Error
	if !errors.As(err, &refused) || refused.Code != code {
		t.Fatalf("%s was answered %v, want %s", what, err, code)
	}
}

// goingEventually waits up to 30 s for done.
func goingEventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within 30 s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTheAgentWireCalls: the six calls of agent/v1 against the server, over its socket. A call with
// no secret or another one is refused. A stale lease token is refused and writes nothing - a step,
// a write, an end - and so is a write whose lease another claim took while its route ran, which
// its transaction checks as it commits. The server runs 16 calls at once and the 17th waits for
// one to end; a panicking route answers an error and the server answers the next call; an answer
// past 1 MiB is cut. A claim takes no run whose shape is above the build's, a person works at most
// 3 runs at once and queues at most 20, a claim waits for a run that becomes ready, a hello frees
// the leases of this server's earlier instances and of no other server's, and a renew is answered
// at once when its run is stopped.
func TestTheAgentWireCalls(t *testing.T) {
	rig := newGoingAgentRig(t, func(agent *AgentListener) { agent.renewWait = goingRenewWait })
	ctx := context.Background()

	t.Run("a call with no secret or a wrong one is refused", func(t *testing.T) {
		rig.settleQueue(t)
		run := rig.queued(t, "alice", goingRunShape, "a run no stranger takes")
		for name, secret := range map[string][]byte{"no secret": nil, "a wrong secret": make([]byte, agentwire.SecretSize)} {
			stranger := agentwire.Client{Path: rig.agent.Path(), Secret: secret}
			_, err := stranger.Hello(ctx, agentwire.HelloRequest{Instance: "a-stranger"})
			requireAgentCode(t, err, agentwire.CodeUnauthorized, "a hello with "+name)
			_, err = stranger.Claim(ctx, agentwire.ClaimRequest{Instance: "a-stranger"})
			requireAgentCode(t, err, agentwire.CodeUnauthorized, "a claim with "+name)
		}
		if state := rig.count(t, `select count(*) from going_runs where id = $1::uuid and state = 'queued'`, run); state != 1 {
			t.Fatalf("the refused claims left run %s other than queued", run)
		}
	})

	t.Run("a stale lease token is refused and writes nothing", func(t *testing.T) {
		issue := rig.issue(t, "Kept by its owner")
		run, token := rig.working(t, "alice", "rename "+issue)
		rig.approve(t, run, "dispatch_issue_update", map[string]any{"issue": issue, "title": "Renamed by a stale owner"})
		stale := uuid.NewString()
		_, err := rig.client.Step(ctx, agentwire.StepRequest{Run: run, LeaseToken: stale, Step: 1, Kind: agentwire.StepModel,
			Body: json.RawMessage(`{"reply":"a stale owner's"}`), Note: "a stale step",
			Frames: []agentwire.Frame{{Name: "text", Data: json.RawMessage(`"stale"`)}}})
		requireAgentCode(t, err, agentwire.CodeStaleLease, "a step under a stale token")
		_, err = rig.call(goingRouteCall(run, stale, 1, 1, "dispatch_issue_update", map[string]any{"issue": issue, "title": "Renamed by a stale owner"}))
		requireAgentCode(t, err, agentwire.CodeStaleLease, "a write under a stale token")
		err = rig.client.End(ctx, agentwire.EndRequest{Run: run, LeaseToken: stale, State: agentwire.StateDone})
		requireAgentCode(t, err, agentwire.CodeStaleLease, "an end under a stale token")
		if steps := rig.count(t, `select count(*) from going_run_steps where run_id = $1::uuid`, run) +
			rig.count(t, `select count(*) from going_run_events where run_id = $1::uuid`, run); steps != 0 {
			t.Fatalf("the stale step left %d steps and events, want none", steps)
		}
		if title := rig.issueRead(t, issue).Title; title != "Kept by its owner" {
			t.Fatalf("the stale write left the title %q, want it kept", title)
		}
		if working := rig.count(t, `select count(*) from going_runs where id = $1::uuid and state = 'working' and lease_token = $2::uuid`, run, token); working != 1 {
			t.Fatal("the stale end ended the run")
		}

		// The owner's own step is saved, its frames appended in sequence.
		saved, err := rig.client.Step(ctx, agentwire.StepRequest{Run: run, LeaseToken: token, Step: 1, Kind: agentwire.StepModel,
			Body: json.RawMessage(`{"reply":"the owner's"}`), Note: "the owner's step",
			Frames: []agentwire.Frame{{Name: "text", Data: json.RawMessage(`"one"`)}, {Name: "text", Data: json.RawMessage(`"two"`)}}})
		if err != nil || saved.Sequence != 2 {
			t.Fatalf("the owner's step answered %+v, %v; want its two frames at 1 and 2", saved, err)
		}

		// Another claim takes the lease while the write's route runs: the route's transaction
		// checks the lease as it commits, so the change is rolled back.
		rig.hooks.set(t, http.MethodPatch, "/api/v1/issues/"+issue, func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
			if _, err := rig.database.Pool.Exec(context.Background(), `update going_runs set lease_token = gen_random_uuid() where id = $1::uuid`, run); err != nil {
				t.Errorf("take the lease over: %v", err)
			}
			inner.ServeHTTP(w, r)
		})
		_, err = rig.call(goingRouteCall(run, token, 2, 1, "dispatch_issue_update", map[string]any{"issue": issue, "title": "Renamed by a stale owner"}))
		requireAgentCode(t, err, agentwire.CodeStaleLease, "a write whose lease was taken while its route ran")
		if title := rig.issueRead(t, issue).Title; title != "Kept by its owner" {
			t.Fatalf("the write whose lease was taken left the title %q, want it rolled back", title)
		}
		if landings := rig.count(t, `select count(*) from going_landings where run_id = $1::uuid`, run); landings != 0 {
			t.Fatalf("the stale writes left %d landings, want none", landings)
		}
	})

	t.Run("the 17th call waits until one of 16 ends", func(t *testing.T) {
		var entered atomic.Int32
		release := make(chan struct{})
		releaseAll := sync.OnceFunc(func() { close(release) })
		t.Cleanup(releaseAll)
		rig.hooks.set(t, http.MethodGet, "/api/v1/whoami", func(w http.ResponseWriter, r *http.Request, inner http.Handler) {
			entered.Add(1)
			<-release
			inner.ServeHTTP(w, r)
		})
		run, token := rig.working(t, "alice", "who am I")
		// The design's bound, written here rather than read from the code under test.
		const slots = 16
		answers := make(chan error, slots+1)
		for call := range slots + 1 {
			go func() {
				answer, err := rig.call(goingRouteCall(run, token, 1, call, "dispatch_whoami", map[string]any{}))
				if err == nil && answer.Status != http.StatusOK {
					err = fmt.Errorf("answered %d: %s", answer.Status, answer.Body)
				}
				answers <- err
			}()
		}
		goingEventually(t, "16 calls reaching their route", func() bool { return entered.Load() == slots })
		time.Sleep(500 * time.Millisecond)
		if running := entered.Load(); running != slots {
			t.Fatalf("%d calls ran at once, want the 17th to wait for one of 16", running)
		}
		release <- struct{}{}
		goingEventually(t, "the 17th call reaching its route once one ended", func() bool { return entered.Load() == slots+1 })
		releaseAll()
		for range slots + 1 {
			if err := <-answers; err != nil {
				t.Fatalf("a call answered %v, want whoami", err)
			}
		}
	})

	t.Run("a panicking route answers an error and the server lives", func(t *testing.T) {
		rig.hooks.set(t, http.MethodGet, "/api/v1/limits", func(http.ResponseWriter, *http.Request, http.Handler) {
			panic("a route that panics")
		})
		run, token := rig.working(t, "alice", "my limits")
		_, err := rig.call(goingRouteCall(run, token, 1, 1, "dispatch_limits", map[string]any{}))
		requireAgentCode(t, err, agentwire.CodeCallPanicked, "a call whose route panicked")
		answer, err := rig.call(goingRouteCall(run, token, 1, 2, "dispatch_whoami", map[string]any{}))
		if err != nil || answer.Status != http.StatusOK || !strings.Contains(string(answer.Body), "alice") {
			t.Fatalf("the call after the panic answered %d %s, %v; want alice's whoami", answer.Status, answer.Body, err)
		}
	})

	t.Run("an answer past 1 MiB is cut", func(t *testing.T) {
		rig.hooks.set(t, http.MethodGet, "/api/v1/priorities", func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			chunk := []byte(strings.Repeat("x", 64<<10))
			for range 48 {
				_, _ = w.Write(chunk)
			}
		})
		run, token := rig.working(t, "alice", "our priorities")
		answer, err := rig.call(goingRouteCall(run, token, 1, 1, "dispatch_priorities", map[string]any{}))
		if err != nil || answer.Status != http.StatusOK || len(answer.Body) != agentwire.CallBodyLimit || !answer.Cut {
			t.Fatalf("an answer of 3 MiB answered %d with %d bytes, cut %v, %v; want 1 MiB marked cut", answer.Status, len(answer.Body), answer.Cut, err)
		}
	})

	t.Run("a claim takes no run whose shape is above the build's", func(t *testing.T) {
		rig.settleQueue(t)
		newer := rig.queued(t, "bob", goingRunShape+1, "a run a newer build saved")
		readable := rig.queued(t, "bob", goingRunShape, "a run this build reads")
		if claimed := rig.claim(t, "i-shape"); claimed == nil || claimed.ID != readable || claimed.Shape != goingRunShape {
			t.Fatalf("the claim took %+v, want run %s of shape %d and not the newer run %s", claimed, readable, goingRunShape, newer)
		}
		if claimed := rig.claim(t, "i-shape"); claimed != nil {
			t.Fatalf("the claim took run %s of shape %d, want none above the build's %d", claimed.ID, claimed.Shape, goingRunShape)
		}
		if queued := rig.count(t, `select count(*) from going_runs where id = $1::uuid and state = 'queued'`, newer); queued != 1 {
			t.Fatal("the newer run is no longer queued for a build that reads it")
		}
	})

	t.Run("the per-person bounds (3 working, 20 queued) hold", func(t *testing.T) {
		rig.settleQueue(t)
		// The design's bounds, written here rather than read from the code under test.
		const working, queue = 3, 20
		for range working + 1 {
			rig.queued(t, "carol", goingRunShape, "one of carol's runs")
		}
		claimed := map[string]bool{}
		for range working {
			run := rig.claim(t, "i-bounds")
			if run == nil {
				t.Fatalf("a claim took none of carol's runs after %d", len(claimed))
			}
			claimed[run.ID] = true
		}
		if run := rig.claim(t, "i-bounds"); run != nil {
			t.Fatalf("a claim took carol's run %s beside her 3 working, want it left queued", run.ID)
		}
		if working, queued := rig.count(t, `select count(*) from going_runs where author_login = 'carol' and state = 'working'`),
			rig.count(t, `select count(*) from going_runs where author_login = 'carol' and state = 'queued'`); len(claimed) != 3 || working != 3 || queued != 1 {
			t.Fatalf("carol has %d runs claimed, %d working and %d queued, want 3, 3 and 1", len(claimed), working, queued)
		}

		admit := func() error {
			tx, err := rig.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			return admitGoingRun(ctx, tx, "dave")
		}
		for range queue - 1 {
			rig.queued(t, "dave", goingRunShape, "one of dave's runs")
		}
		if err := admit(); err != nil {
			t.Fatalf("dave's 20th run was refused: %v", err)
		}
		rig.queued(t, "dave", goingRunShape, "dave's 20th run")
		var refusal *apiError
		if err := admit(); !errors.As(err, &refusal) || refusal.status != http.StatusTooManyRequests || refusal.code != codeGoingRunQueueFull {
			t.Fatalf("dave's 21st run was answered %v, want 429 %s", err, codeGoingRunQueueFull)
		}
	})

	t.Run("a claim waits for a run that becomes ready", func(t *testing.T) {
		rig.settleQueue(t)
		claimed := make(chan *agentwire.ClaimedRun, 1)
		go func() {
			answer, err := rig.client.Claim(ctx, agentwire.ClaimRequest{Instance: "i-waits"})
			if err != nil {
				t.Errorf("the waiting claim: %v", err)
			}
			claimed <- answer.Run
		}()
		time.Sleep(300 * time.Millisecond)
		run := rig.queued(t, "erin", goingRunShape, "a run that becomes ready")
		if err := rig.served.notifyGoingRuns(ctx, "ready"); err != nil {
			t.Fatal(err)
		}
		if got := <-claimed; got == nil || got.ID != run {
			t.Fatalf("the claim waiting as run %s became ready took %+v, want that run", run, got)
		}
	})

	t.Run("a hello frees the leases of this server's earlier instances", func(t *testing.T) {
		rig.settleQueue(t)
		run := rig.queued(t, "frank", goingRunShape, "a run whose agent died")
		other, _ := rig.working(t, "frank", "a run of another server's agent")
		if _, err := rig.client.Hello(ctx, agentwire.HelloRequest{Instance: "i-first"}); err != nil {
			t.Fatal(err)
		}
		if claimed := rig.claim(t, "i-first"); claimed == nil || claimed.ID != run {
			t.Fatalf("the first instance claimed %+v, want run %s", claimed, run)
		}
		hello, err := rig.client.Hello(ctx, agentwire.HelloRequest{Instance: "i-second"})
		if err != nil || hello.Freed != 1 {
			t.Fatalf("the second instance's hello answered %+v, %v; want the first instance's one lease freed", hello, err)
		}
		if freed := rig.count(t, `select count(*) from going_runs where id = $1::uuid and state = 'queued' and lease_token is null`, run); freed != 1 {
			t.Fatal("the first instance's run is not ready again")
		}
		if kept := rig.count(t, `select count(*) from going_runs where id = $1::uuid and state = 'working' and lease_owner = 'the-agent-of-this-test'`, other); kept != 1 {
			t.Fatal("the hello freed the lease of another server's agent")
		}
		if claimed := rig.claim(t, "i-second"); claimed == nil || claimed.ID != run {
			t.Fatalf("the second instance claimed %+v, want the freed run %s", claimed, run)
		}
	})

	t.Run("a renew is answered at once when its run is stopped", func(t *testing.T) {
		rig.settleQueue(t)
		rig.queued(t, "gina", goingRunShape, "a run its person stops")
		claimed := rig.claim(t, "i-renews")
		if claimed == nil {
			t.Fatal("no run to stop was claimed")
		}
		started := time.Now()
		renewed := make(chan agentwire.RenewAnswer, 1)
		go func() {
			answer, err := rig.client.Renew(ctx, agentwire.RenewRequest{Instance: "i-renews",
				Leases: []agentwire.Lease{{Run: claimed.ID, LeaseToken: claimed.LeaseToken}}})
			if err != nil {
				t.Errorf("the renew: %v", err)
			}
			renewed <- answer
		}()
		time.Sleep(500 * time.Millisecond)
		if stopped, err := rig.served.stopGoingRun(ctx, claimed.ID, "stopped by gina"); err != nil || !stopped {
			t.Fatalf("stop the run: %v, %v", stopped, err)
		}
		answer := <-renewed
		if took := time.Since(started); took > goingRenewWait/2 || len(answer.Stopped) != 1 || answer.Stopped[0] != claimed.ID || len(answer.Renewed) != 0 {
			t.Fatalf("the renew answered %+v after %s, want the run stopped, at once rather than after %s", answer, took, goingRenewWait)
		}
		_, err := rig.client.Step(ctx, agentwire.StepRequest{Run: claimed.ID, LeaseToken: claimed.LeaseToken, Step: 1,
			Kind: agentwire.StepModel, Body: json.RawMessage(`{}`), Note: "after the stop"})
		requireAgentCode(t, err, agentwire.CodeStaleLease, "a step of a stopped run")
	})
}
