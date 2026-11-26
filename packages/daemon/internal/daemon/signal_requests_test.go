package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/controller"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// requestsProject is the Dispatch project the requests below are kept for, and requestsIssue the
// issue whose roles report them.
const (
	requestsProject = "LEGION"
	requestsIssue   = "LEGION-301"
	// requestsLimit is the token limit the rig's project configures, and requestsModels the
	// models whose tokens count against it.
	requestsLimit  = 6000
	requestsModels = "^claude-"
)

// requestsBase is the moment the rig's daemon boots, which the provider's minutes are kept from:
// minute k of a row below is requestsBase plus k minutes.
var requestsBase = time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

// minute is the start of minute k after requestsBase.
func minute(k int) time.Time { return requestsBase.Add(time.Duration(k) * time.Minute) }

// requestsRig is the daemon's own path of a host's report of its model requests, from the HTTP
// route to the store: the API server the daemon serves (api.NewServer) over an isolated pool, its
// handlers the ones the workflow applies every fact through (admit.Handlers), its claims a
// supervisor's machines writing through the daemon's claim store, so a turn's edges are recorded
// as a machine records them. clock is the machines' time, by which a turn's start and end are
// stamped, and now admission's, by which it keeps the requests. The daemon has booted once at
// requestsBase, configured with requestsLimit and requestsModels; stale is the same daemon as a
// deployment that relaunches a process of another plugin contract, which refuses that process's
// calls while it is out of a turn.
type requestsRig struct {
	t         *testing.T
	ctx       context.Context
	pool      *store.Pool
	st        *store.Store
	records   *record.Postgres
	sup       *supervisor
	rt        *fake.Runtime
	clock     *heldClock
	now       time.Time
	admission *admit.Admission
	handlers  []intake.Handler
	server    *httptest.Server
	stale     *httptest.Server
}

func newRequestsRig(t *testing.T) *requestsRig {
	t.Helper()
	ctx := context.Background()
	r := &requestsRig{t: t, ctx: ctx, pool: isolatedOutboxPool(t), records: record.NewStore(), clock: &heldClock{at: requestsBase}, now: requestsBase}
	st := outboxTreeStore(t, r.pool, requestsIssue)
	r.st = st
	r.sup, r.rt = newOutboxSupervisor(t, "legion", t.TempDir())
	r.sup.deps.Store, r.sup.deps.Clock, r.sup.deps.PluginContract = st, r.clock, api.DaemonAPIVersion()
	engine := workflow.New(r.records, workflow.Config{Project: requestsProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	r.admission = admit.New(r.records, engine, 1, requestsProject, quietLogger())
	r.admission.UseClock(func() time.Time { return r.now })
	r.configure(requestsLimit, requestsModels)
	r.handlers = admit.Handlers(r.records, requestsProject, engine, r.admission)
	r.boot(1)
	serve := func(relaunchable bool) *httptest.Server {
		server := httptest.NewServer(api.NewServer("127.0.0.1", 0, api.Options{
			Supervisor: r.sup, Project: "legion", DispatchProject: requestsProject, Pool: r.pool, Handlers: r.handlers,
			Record: r.records, BootTokens: api.NewBootTokens(st), Controller: st, PluginRelaunchable: relaunchable, Log: quietLogger(),
		}).Handler)
		t.Cleanup(server.Close)
		return server
	}
	r.server, r.stale = serve(false), serve(true)
	return r
}

// configure gives admission the project's token limit and model selector, as the daemon's boot
// gives it the project's file (config.Project.Provider).
func (r *requestsRig) configure(limit int64, models string) {
	r.t.Helper()
	pattern, err := config.SharedPattern(models)
	if err != nil {
		r.t.Fatal(err)
	}
	r.admission.UseModelRequests(limit, pattern)
}

// boot applies the daemon's boot fact, as each start of the daemon does, at admission's now.
func (r *requestsRig) boot(boot int64) {
	r.t.Helper()
	if _, err := intake.ApplyFact(r.ctx, r.pool, "daemon", fmt.Sprintf("boot:%s:%d", requestsProject, boot), intake.DaemonBooted{Boot: boot}, r.handlers...); err != nil {
		r.t.Fatalf("apply boot %d: %v", boot, err)
	}
}

// host is one claim's host as the daemon knows it: the claim, its launch's generation and boot
// token, and the session and secret its registration was given.
type host struct {
	token      claim.Token
	generation uint64
	bootToken  string
	session    string
	secret     string
}

// launch creates, launches and registers a claim of role on requestsIssue (the controller's, for
// claim.RoleController) on a host that speaks plugin contract, as its plugin does once its agent
// has booted.
func (r *requestsRig) launch(role claim.Role, contract int) host {
	r.t.Helper()
	c := supervise.Claim{Project: "legion", Tree: requestsIssue, Issue: requestsIssue, Role: role, State: supervise.StateQueued}
	if role == claim.RoleController {
		c = supervise.Claim{Token: claim.ControllerToken("legion"), Project: "legion", Role: role, State: supervise.StateQueued}
	} else {
		c.Token = mustClaimToken(r.t, requestsIssue, role)
	}
	machine, _, err := r.sup.Create(r.ctx, c, "")
	if err != nil {
		r.t.Fatalf("create the %s's claim: %v", role, err)
	}
	r.sup.deps.Conns.(*fake.Conns).Register(c.Token, fake.NewConn())
	if err := machine.Handle(r.ctx, supervise.RequestSpawn{Claim: c.Token}); err != nil {
		r.t.Fatalf("launch the %s: %v", role, err)
	}
	generation := machine.Claim().Generation
	h := host{token: c.Token, generation: generation, bootToken: lastLaunch(r.t, r.rt, c.Token).BootToken,
		session: "ses-" + string(role), secret: "secret-of-" + string(role)}
	hash := sha256.Sum256([]byte(h.secret))
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: c.Token, Generation: generation},
		supervise.RequestRegister{Claim: c.Token, Generation: generation, Session: h.session, SessionFile: "/tmp/" + h.session + ".jsonl",
			CapabilityHash: hash[:], PluginContract: contract},
		supervise.RequestReady{Claim: c.Token, Generation: generation, Session: h.session},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			r.t.Fatalf("handle %T of %s: %v", ev, c.Token, err)
		}
	}
	return h
}

// turn runs one turn of h's agent from start to end on the machines' clock, as its stream reports
// the two edges.
func (r *requestsRig) turn(h host, start, end time.Time) {
	r.t.Helper()
	machine, found := r.sup.Machine(h.token)
	if !found {
		r.t.Fatalf("no machine of %s", h.token)
	}
	r.clock.advance(start.Sub(r.clock.Now()))
	if err := machine.Handle(r.ctx, supervise.StreamTurnStart{Claim: h.token}); err != nil {
		r.t.Fatalf("start a turn of %s: %v", h.token, err)
	}
	r.clock.advance(end.Sub(r.clock.Now()))
	if err := machine.Handle(r.ctx, supervise.StreamTurnEnd{Claim: h.token}); err != nil {
		r.t.Fatalf("end the turn of %s: %v", h.token, err)
	}
}

// post sends body to path on server and answers the status and the answer's bytes.
func (r *requestsRig) post(server *httptest.Server, path string, body any) (int, string) {
	r.t.Helper()
	encoded, ok := body.([]byte)
	if !ok {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			r.t.Fatal(err)
		}
	}
	response, err := http.Post(server.URL+path, "application/json", bytes.NewReader(encoded))
	if err != nil {
		r.t.Fatalf("POST %s: %v", path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	return response.StatusCode, strings.TrimSpace(string(answer))
}

// report is h's host sending requests in one batch, as its own secret's caller (claim.AgentCaller).
func (r *requestsRig) report(h host, requests ...claim.ModelRequest) (int, string) {
	r.t.Helper()
	return r.post(r.server, "/legion/v1/claims/model-requests", claim.ModelRequestsRequest{
		AgentCaller: claim.AgentCaller{ClaimToken: h.token, SessionID: h.session, Secret: h.secret},
		Generation:  h.generation, Requests: requests,
	})
}

// kept requires the answer of a report to be 200 naming exactly ids as kept.
func (r *requestsRig) kept(status int, answer string, ids ...string) {
	r.t.Helper()
	want, err := json.Marshal(claim.ModelRequestsResponse{Kept: ids})
	if err != nil {
		r.t.Fatal(err)
	}
	if status != http.StatusOK || answer != string(want) {
		r.t.Fatalf("the report was answered %d %s, want 200 %s", status, answer, want)
	}
}

// minutes is the provider's record of the project over [from, to), as the live reading takes it
// (record.Postgres.ProviderMinutes), a host being one that reports its requests from the
// admission-signals contract on.
func (r *requestsRig) minutes(from, to time.Time) record.ProviderMinutes {
	r.t.Helper()
	var minutes record.ProviderMinutes
	if err := store.TransactWith(r.ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		minutes, err = r.records.ProviderMinutes(ctx, tx, requestsProject, from, to, api.ModelRequestsContract())
		return err
	}); err != nil {
		r.t.Fatalf("read the provider's minutes: %v", err)
	}
	return minutes
}

// wantMinutes requires the record over the minutes of want, from its first to its last, to hold
// exactly want's minutes, each as want gives it, and no other: a minute absent from want is one
// the record must not hold.
func (r *requestsRig) wantMinutes(from, to int, want map[int]signals.Minute) {
	r.t.Helper()
	kept := r.minutes(minute(from), minute(to))
	for k := from; k < to; k++ {
		got, held := kept.Read(minute(k))
		wanted, wantHeld := want[k]
		switch {
		case held != wantHeld:
			r.t.Errorf("minute %d (%s) is held %t (%+v), want held %t", k, minute(k).Format(time.TimeOnly), held, got, wantHeld)
		case held && got != wanted:
			r.t.Errorf("minute %d (%s) reads %+v, want %+v", k, minute(k).Format(time.TimeOnly), got, wanted)
		}
	}
}

// rows is every request the store keeps of the project, by id, as
// `<id> session=<s> model=<m> tokens=<n|none> refusal=<class|none> counted=<bool>`.
func (r *requestsRig) rows() []string {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select id, session, model, uncached, refusal, counted from model_requests where project = $1 order by id`, requestsProject)
	if err != nil {
		r.t.Fatalf("read the kept requests: %v", err)
	}
	kept, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var id, session, model string
		var uncached *int64
		var refusal *string
		var counted bool
		err := row.Scan(&id, &session, &model, &uncached, &refusal, &counted)
		tokens, class := "none", "none"
		if uncached != nil {
			tokens = fmt.Sprint(*uncached)
		}
		if refusal != nil {
			class = *refusal
		}
		return fmt.Sprintf("%s session=%s model=%s tokens=%s refusal=%s counted=%t", id, session, model, tokens, class, counted), err
	})
	if err != nil {
		r.t.Fatalf("read the kept requests: %v", err)
	}
	return kept
}

// request is one model request of session, of model, ended at, with tokens uncached input tokens
// (none for a negative count) and refused by refusal ("" for an answered one).
func request(id, session, model string, at time.Time, tokens int64, refusal claim.RefusalClass) claim.ModelRequest {
	req := claim.ModelRequest{ID: id, Session: session, Model: model, EndedAt: at, Refusal: refusal}
	if tokens >= 0 {
		req.UncachedInputTokens = &tokens
	}
	return req
}

// Each model request a host reports through the daemon's own route is kept once, by the
// claim, its launch generation and its stable id, in the UTC minute it ended in, and the
// provider's record of a range of minutes answers each minute's uncached input tokens of the
// models the project counts, its refusals by the project's own limit (unknown ones among them,
// as the replay counts them) and by the provider, and the limit in force when it began. A minute
// some turn of a host that reports no requests overlaps is absent, never zero; a minute every
// host of which reports its requests is held, zero where none ran.
func TestSignalRequestReports(t *testing.T) {
	t.Parallel()
	opus := "claude-opus-5-5"
	t.Run("minute-boundary", func(t *testing.T) {
		t.Parallel()
		r := newRequestsRig(t)
		role := r.launch(claim.RoleImplementer, api.DaemonAPIVersion())
		// One batch of two requests that end either side of a minute's end: each counts in its
		// own minute, by the moment it ended, and in neither the batch's arrival nor the other's.
		status, answer := r.report(role,
			request("req-1", role.session, opus, minute(3).Add(59*time.Second+900*time.Millisecond), 1200, ""),
			request("req-2", role.session, opus, minute(4).Add(100*time.Millisecond), 800, ""))
		r.kept(status, answer, "req-1", "req-2")
		r.wantMinutes(2, 6, map[int]signals.Minute{
			2: {Limit: requestsLimit}, 3: {Uncached: 1200, Limit: requestsLimit}, 4: {Uncached: 800, Limit: requestsLimit}, 5: {Limit: requestsLimit},
		})
	})
	t.Run("redelivery", func(t *testing.T) {
		t.Parallel()
		r := newRequestsRig(t)
		role := r.launch(claim.RoleImplementer, api.DaemonAPIVersion())
		first := []claim.ModelRequest{
			request("req-a", role.session, opus, minute(3).Add(10*time.Second), 1000, ""),
			request("req-b", role.session, opus, minute(3).Add(20*time.Second), 2000, ""),
		}
		// The batch sent again, as a host retries one whose answer it never read, and a batch
		// that carries one id the daemon holds already beside a new one, as a host that sends the
		// requests it holds unacknowledged with the next: each id counts once.
		status, answer := r.report(role, first...)
		r.kept(status, answer, "req-a", "req-b")
		status, answer = r.report(role, first...)
		r.kept(status, answer, "req-a", "req-b")
		status, answer = r.report(role, first[1], request("req-c", role.session, opus, minute(3).Add(30*time.Second), 400, claim.RefusalProvider))
		r.kept(status, answer, "req-b", "req-c")
		r.wantMinutes(3, 4, map[int]signals.Minute{3: {Uncached: 3400, Limit: requestsLimit, Provider: 1}})
		if got := len(r.rows()); got != 3 {
			t.Fatalf("the store keeps %d requests, want the 3 ids reported: %q", got, r.rows())
		}
	})
	t.Run("variants", func(t *testing.T) {
		t.Parallel()
		r := newRequestsRig(t)
		role := r.launch(claim.RoleImplementer, api.DaemonAPIVersion())
		at := func(s int) time.Time { return minute(5).Add(time.Duration(s) * time.Second) }
		status, answer := r.report(role,
			request("clean", role.session, opus, at(1), 1500, ""),
			request("own", role.session, opus, at(2), -1, claim.RefusalOwn),
			request("provider", role.session, opus, at(3), -1, claim.RefusalProvider),
			request("unknown", role.session, opus, at(4), 300, claim.RefusalUnknown),
			request("untold", role.session, opus, at(5), -1, ""),
			request("other-model", role.session, "gpt-9", at(6), 7000, ""),
			request("other-model-refused", role.session, "gpt-9", at(7), -1, claim.RefusalProvider))
		r.kept(status, answer, "clean", "own", "provider", "unknown", "untold", "other-model", "other-model-refused")
		// The tokens are the uncached input of the requests of the models the project counts, a
		// request whose host learnt none counting none; an unknown refusal counts as the project's
		// own, and a refusal of a model whose tokens count nowhere counts all the same.
		r.wantMinutes(5, 6, map[int]signals.Minute{5: {Uncached: 1800, Limit: requestsLimit, Own: 2, Provider: 2}})
		// Storage keeps the unknown refusal apart from the own one, and a request of another model
		// is kept, counted nowhere.
		want := []string{
			"clean session=ses-implementer model=claude-opus-5-5 tokens=1500 refusal=none counted=true",
			"other-model session=ses-implementer model=gpt-9 tokens=7000 refusal=none counted=false",
			"other-model-refused session=ses-implementer model=gpt-9 tokens=none refusal=provider counted=false",
			"own session=ses-implementer model=claude-opus-5-5 tokens=none refusal=own counted=true",
			"provider session=ses-implementer model=claude-opus-5-5 tokens=none refusal=provider counted=true",
			"unknown session=ses-implementer model=claude-opus-5-5 tokens=300 refusal=unknown counted=true",
			"untold session=ses-implementer model=claude-opus-5-5 tokens=none refusal=none counted=true",
		}
		if got := r.rows(); !slices.Equal(got, want) {
			t.Fatalf("the store keeps %q, want %q", got, want)
		}
		// A host reports the uncached input alone, fresh input and cache writes: a body naming
		// the cache reads is refused whole, so nothing of them is ever counted, and so is a body
		// carrying text of the request or a class the daemon does not know.
		for _, body := range []string{
			`{"claimToken":"%s","sessionId":"%s","secret":"%s","generation":%d,"requests":[{"id":"cached","session":"s","model":"claude-opus-5-5","endedAt":"2026-11-26T10:05:09Z","uncachedInputTokens":10,"cacheReadInputTokens":90000}]}`,
			`{"claimToken":"%s","sessionId":"%s","secret":"%s","generation":%d,"requests":[{"id":"texted","session":"s","model":"claude-opus-5-5","endedAt":"2026-11-26T10:05:09Z","error":"rate limit exceeded"}]}`,
			`{"claimToken":"%s","sessionId":"%s","secret":"%s","generation":%d,"requests":[{"id":"classed","session":"s","model":"claude-opus-5-5","endedAt":"2026-11-26T10:05:09Z","refusal":"quota"}]}`,
		} {
			if status, answer := r.post(r.server, "/legion/v1/claims/model-requests", []byte(fmt.Sprintf(body, role.token, role.session, role.secret, role.generation))); status != http.StatusBadRequest {
				t.Errorf("a body %s was answered %d %s, want 400", body, status, answer)
			}
		}
		if got := len(r.rows()); got != len(want) {
			t.Fatalf("the store keeps %d requests after the refused bodies, want %d", got, len(want))
		}
	})
	t.Run("claim-scope", func(t *testing.T) {
		t.Parallel()
		r := newRequestsRig(t)
		role := r.launch(claim.RoleImplementer, api.DaemonAPIVersion())
		bridge := r.launch(claim.RoleTester, api.DaemonAPIVersion())
		controller := r.launch(claim.RoleController, api.DaemonAPIVersion())
		at := minute(7).Add(10 * time.Second)
		// The role's own session, a subagent's session reported by the role's host, the Claude Code
		// bridge's role by its boot token, and the controller's session: each counted once.
		status, answer := r.report(role, request("role-1", role.session, opus, at, 100, ""), request("sub-1", "ses-subagent-7", opus, at, 200, ""))
		r.kept(status, answer, "role-1", "sub-1")
		status, answer = r.post(r.server, "/legion/v1/claims/model-requests", claim.ModelRequestsRequest{
			BootCaller: claim.BootCaller{BootToken: bridge.bootToken}, Generation: bridge.generation,
			Requests: []claim.ModelRequest{request("bridge-1", "claude-session-9", opus, at, 400, "")}})
		r.kept(status, answer, "bridge-1")
		status, answer = r.report(controller, request("controller-1", controller.session, opus, at, 800, ""))
		r.kept(status, answer, "controller-1")
		r.wantMinutes(7, 8, map[int]signals.Minute{7: {Uncached: 1500, Limit: requestsLimit}})

		older := r.launch(claim.RoleReviewer, api.ModelRequestsContract()-1)
		other := request("refused-1", role.session, opus, at, 5000, "")
		for _, tc := range []struct {
			name   string
			server *httptest.Server
			body   any
			status int
		}{
			{"another claim's token with this claim's secret", r.server, claim.ModelRequestsRequest{
				AgentCaller: claim.AgentCaller{ClaimToken: bridge.token, SessionID: role.session, Secret: role.secret},
				Generation:  role.generation, Requests: []claim.ModelRequest{other}}, http.StatusForbidden},
			{"a wrong secret", r.server, claim.ModelRequestsRequest{
				AgentCaller: claim.AgentCaller{ClaimToken: role.token, SessionID: role.session, Secret: "not-the-secret"},
				Generation:  role.generation, Requests: []claim.ModelRequest{other}}, http.StatusForbidden},
			{"a stale claim out of its turn", r.stale, claim.ModelRequestsRequest{
				AgentCaller: claim.AgentCaller{ClaimToken: older.token, SessionID: older.session, Secret: older.secret},
				Generation:  older.generation, Requests: []claim.ModelRequest{other}}, http.StatusConflict},
			{"a boot token of another generation", r.server, claim.ModelRequestsRequest{
				BootCaller: claim.BootCaller{BootToken: bridge.bootToken}, Generation: bridge.generation + 1,
				Requests: []claim.ModelRequest{other}}, http.StatusConflict},
			{"a body naming its project", r.server, []byte(fmt.Sprintf(
				`{"claimToken":%q,"sessionId":%q,"secret":%q,"generation":%d,"project":"OTHER","requests":[{"id":"refused-1","session":"s","model":"claude-opus-5-5","endedAt":"2026-11-26T10:07:10Z","uncachedInputTokens":5000}]}`,
				role.token, role.session, role.secret, role.generation)), http.StatusBadRequest},
		} {
			if status, answer := r.post(tc.server, "/legion/v1/claims/model-requests", tc.body); status != tc.status {
				t.Errorf("%s was answered %d %s, want %d", tc.name, status, answer, tc.status)
			}
		}
		r.wantMinutes(7, 8, map[int]signals.Minute{7: {Uncached: 1500, Limit: requestsLimit}})
	})
	// The operator's controller (`controller: operator`) holds no claim the daemon supervises: it
	// names its own token with the session and secret its registration was answered, which the
	// daemon checks against the controller record as it checks the controller's grants.
	t.Run("operator-controller", func(t *testing.T) {
		t.Parallel()
		r := newRequestsRig(t)
		const session, secret = "ses_operator_controller", "the-operator-controllers-secret"
		generation, err := r.st.MintController(r.ctx, "legion", controller.UnheldCapability())
		if err != nil {
			t.Fatalf("mint the controller's capability: %v", err)
		}
		hash := sha256.Sum256([]byte(secret))
		if registered, err := r.st.RegisterController(r.ctx, "legion", generation, session, hash[:], requestsBase, api.DaemonAPIVersion()); err != nil || !registered {
			t.Fatalf("register the controller = %t, %v", registered, err)
		}
		as := func(token claim.Token, session, secret string, generation uint64, id string) (int, string) {
			return r.post(r.server, "/legion/v1/claims/model-requests", claim.ModelRequestsRequest{
				AgentCaller: claim.AgentCaller{ClaimToken: token, SessionID: session, Secret: secret}, Generation: generation,
				Requests: []claim.ModelRequest{request(id, session, opus, minute(6).Add(10*time.Second), 700, "")}})
		}
		token := claim.ControllerToken("legion")
		status, answer := as(token, session, secret, generation, "controller-op-1")
		r.kept(status, answer, "controller-op-1")
		for name, got := range map[string]func() (int, string){
			"a wrong secret":     func() (int, string) { return as(token, session, "not-the-secret", generation, "refused-a") },
			"another session":    func() (int, string) { return as(token, "ses_other", secret, generation, "refused-b") },
			"another generation": func() (int, string) { return as(token, session, secret, generation+1, "refused-c") },
			"a claim token of a role": func() (int, string) {
				return as(mustClaimToken(t, requestsIssue, claim.RoleTester), session, secret, generation, "refused-d")
			},
		} {
			if status, answer := got(); status != http.StatusForbidden && status != http.StatusConflict {
				t.Errorf("%s was answered %d %s, want a refusal", name, status, answer)
			}
		}
		r.wantMinutes(6, 7, map[int]signals.Minute{6: {Uncached: 700, Limit: requestsLimit}})
		if got, want := r.rows(), []string{"controller-op-1 session=ses_operator_controller model=claude-opus-5-5 tokens=700 refusal=none counted=true"}; !slices.Equal(got, want) {
			t.Fatalf("the store keeps %q, want only the controller's report", got)
		}
	})
	t.Run("model-selection", func(t *testing.T) {
		t.Parallel()
		r := newRequestsRig(t)
		role := r.launch(claim.RoleImplementer, api.DaemonAPIVersion())
		status, answer := r.report(role,
			request("opus", role.session, opus, minute(2).Add(5*time.Second), 2500, ""),
			request("sonnet", role.session, "claude-sonnet-5", minute(2).Add(6*time.Second), 500, ""),
			request("excluded", role.session, "gpt-9", minute(2).Add(7*time.Second), 9000, ""))
		r.kept(status, answer, "opus", "sonnet", "excluded")
		// The daemon restarts at minute 10 with another limit and selector: each minute keeps the
		// limit in force when it began, and a request kept from then on is counted by the new
		// selector, so the change reads no earlier minute again.
		r.now = minute(10)
		r.configure(9000, "^gpt-")
		r.boot(2)
		status, answer = r.report(role,
			request("opus-later", role.session, opus, minute(11).Add(5*time.Second), 2500, ""),
			request("gpt-later", role.session, "gpt-9", minute(11).Add(6*time.Second), 9000, ""))
		r.kept(status, answer, "opus-later", "gpt-later")
		r.wantMinutes(2, 12, map[int]signals.Minute{
			2: {Uncached: 3000, Limit: requestsLimit}, 3: {Limit: requestsLimit}, 4: {Limit: requestsLimit}, 5: {Limit: requestsLimit},
			6: {Limit: requestsLimit}, 7: {Limit: requestsLimit}, 8: {Limit: requestsLimit}, 9: {Limit: requestsLimit},
			10: {Limit: 9000}, 11: {Uncached: 9000, Limit: 9000},
		})
	})
	t.Run("older-contract", func(t *testing.T) {
		t.Parallel()
		r := newRequestsRig(t)
		older := r.launch(claim.RoleImplementer, api.ModelRequestsContract()-1)
		// The turn-outcome report a host of an earlier contract sends at its turn's end is taken
		// exactly as before, and its answer is the bytes it always was.
		r.turn(older, minute(10).Add(20*time.Second), minute(12).Add(30*time.Second))
		status, answer := r.post(r.server, "/legion/v1/claims/turn-outcome", claim.TurnOutcomeRequest{
			AgentCaller: claim.AgentCaller{ClaimToken: older.token, SessionID: older.session, Secret: older.secret},
			Generation:  older.generation, Sequence: 1, Outcome: claim.TurnClean})
		if status != http.StatusOK || answer != `{"refusedRun":0,"failed":false}` {
			t.Fatalf("the older host's turn outcome was answered %d %s, want 200 {\"refusedRun\":0,\"failed\":false}", status, answer)
		}
		// Its turn spends tokens no report names, so each minute the turn overlaps is absent from
		// the record, never zero, and the minutes either side of it are held.
		r.wantMinutes(9, 14, map[int]signals.Minute{9: {Limit: requestsLimit}, 13: {Limit: requestsLimit}})
		// A turn still running reaches to now: from its start on, no minute is held.
		machine, _ := r.sup.Machine(older.token)
		r.clock.advance(minute(15).Add(5 * time.Second).Sub(r.clock.Now()))
		if err := machine.Handle(r.ctx, supervise.StreamTurnStart{Claim: older.token}); err != nil {
			t.Fatalf("start the older host's next turn: %v", err)
		}
		r.wantMinutes(13, 18, map[int]signals.Minute{13: {Limit: requestsLimit}, 14: {Limit: requestsLimit}})
	})
	t.Run("empty-observed", func(t *testing.T) {
		t.Parallel()
		r := newRequestsRig(t)
		role := r.launch(claim.RoleImplementer, api.DaemonAPIVersion())
		// A host of this contract reports every request it makes, so a turn of it that made none
		// leaves each of its minutes held at zero, where an older host's turn leaves them absent.
		r.turn(role, minute(20).Add(15*time.Second), minute(22).Add(10*time.Second))
		r.wantMinutes(19, 24, map[int]signals.Minute{
			19: {Limit: requestsLimit}, 20: {Limit: requestsLimit}, 21: {Limit: requestsLimit}, 22: {Limit: requestsLimit}, 23: {Limit: requestsLimit},
		})
		// Before the boot that began keeping the requests, no minute is held.
		r.wantMinutes(-3, 0, map[int]signals.Minute{})
	})
}
