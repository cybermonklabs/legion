package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

const (
	// testProject is the claim project token (Options.Project): claim tokens, tree lifecycles and
	// the controller record are keyed by it.
	testProject = "legion"
	// testDispatchProject is the Dispatch project key (Options.DispatchProject) the same project's
	// issues and mirror rows are filed under. It deliberately differs from testProject, as a
	// deployment's two always do — Dispatch keys are uppercase and the claim token is their
	// lowercase — so a route that reads a Dispatch-keyed row with the claim token finds nothing
	// and its test fails instead of passing on one string that happens to serve as both.
	testDispatchProject = "LEGION"
	testOperatorToken   = "operator-bearer-for-tests"
)

// testPromptAgents is what the harness's server answers as the task agents a role's prompts
// dispatch (Options.PromptAgents).
var testPromptAgents = []string{"deep-worker", "oracle", "plan-gap-analyst"}

// testStore is a migrated database of the test's own on the devbox or CI Postgres, cloned from
// the package's template: the routes are held to what a real store records, and tests never see
// each other's rows.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	return testpg.CloneStore(t, "legion_api_test")
}

// stillClock is time that never moves: a machine's timers are armed and never fire, so a route
// test sees only what its own requests did.
type stillClock struct{}

func (stillClock) Now() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) }

func (stillClock) AfterFunc(time.Duration, func()) supervise.Cancel { return stopped{} }

func (stillClock) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

type stopped struct{}

func (stopped) Stop() bool { return true }

// armedClock is stillClock with every timer a machine armed kept, so a test can fire the one it
// is about (fire): a route test reaches the registration deadline, which the daemon's own clock
// reaches minutes later, without waiting for it. Nothing fires on its own, as nothing did before.
type armedClock struct {
	stillClock
	mu    sync.Mutex
	armed []*armedTimer
}

// armedTimer is one such timer: how far ahead it was armed, what it runs, and whether it has been
// stopped or already fired. Its clock's lock guards done.
type armedTimer struct {
	clock *armedClock
	after time.Duration
	fire  func()
	done  bool
}

func (c *armedClock) AfterFunc(after time.Duration, fire func()) supervise.Cancel {
	timer := &armedTimer{clock: c, after: after, fire: fire}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = append(c.armed, timer)
	return timer
}

func (t *armedTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	t.done = true
	return true
}

// fire runs every timer still armed for exactly after, and reports how many it ran. The duration
// is the only thing a timer says about itself here, so a test with one claim fires its deadline
// by the bound that deadline was armed at (registrationDeadline).
func (c *armedClock) fire(after time.Duration) int {
	c.mu.Lock()
	var due []*armedTimer
	for _, timer := range c.armed {
		if !timer.done && timer.after == after {
			timer.done = true
			due = append(due, timer)
		}
	}
	c.mu.Unlock()
	for _, timer := range due {
		timer.fire()
	}
	return len(due)
}

// fixedSpecs is a launch the fake runtime accepts: the machine fills in who it is for.
type fixedSpecs struct{}

func (fixedSpecs) SpawnSpec(context.Context, supervise.Claim) (runtime.SpawnSpec, error) {
	return runtime.SpawnSpec{Prompt: runtime.PromptParts{RolePromptPaths: []string{"/dev/null"}}}, nil
}

// testSupervisor is the daemon's claim directory as the routes see it, over real machines and a
// real store.
type testSupervisor struct {
	ctx      context.Context
	deps     supervise.Deps
	store    *store.Store
	mu       sync.Mutex
	machines map[claim.Token]*supervise.Machine
	prompts  map[claim.Token]string
	// stepErr is what the step read answers instead of the store's record, for a test about a
	// refused read (refuseStep); nil while the store answers.
	stepErr error
	// posted is every relaunch a route posted to a claim's machine rather than awaiting it
	// (PostStaleRelaunch), in order, each still holding the route's decision record. The daemon
	// runs these on the claim's own queue goroutine; this harness holds them until a test runs
	// them (harness.runPosted), which is what lets a test see what the route answered before the
	// relaunch its registration raised had run.
	posted []postedRelaunch
}

// postedRelaunch is one such post: the launch whose registration was refused, and the decision
// record whoever takes the post releases.
type postedRelaunch struct {
	token      claim.Token
	generation uint64
	released   func()
}

func (s *testSupervisor) PostStaleRelaunch(token claim.Token, generation uint64, released func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posted = append(s.posted, postedRelaunch{token: token, generation: generation, released: released})
}

// take is every post made so far, leaving none.
func (s *testSupervisor) take() []postedRelaunch {
	s.mu.Lock()
	defer s.mu.Unlock()
	posted := s.posted
	s.posted = nil
	return posted
}

func (s *testSupervisor) Machine(token claim.Token) (*supervise.Machine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.machines[token]
	return m, ok
}

func (s *testSupervisor) Create(ctx context.Context, c supervise.Claim, rolePrompt string) (*supervise.Machine, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.machines[c.Token]; ok {
		return m, false, nil
	}
	bound, err := s.deps.Store.AdmitClaim(ctx, c)
	if err != nil {
		return nil, false, err
	}
	c = bound
	m, err := supervise.NewMachine(s.ctx, s.deps, c)
	if err != nil {
		return nil, false, err
	}
	s.machines[c.Token] = m
	s.prompts[c.Token] = rolePrompt
	return m, true, nil
}

// restart rebuilds a claim's machine from the store, as a daemon restart re-adopting it does:
// everything the machine held only in memory is gone.
func (s *testSupervisor) restart(t *testing.T, token claim.Token) *supervise.Machine {
	t.Helper()
	claims, err := s.store.Claims(s.ctx)
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	for _, c := range claims {
		if c.Token != token {
			continue
		}
		m, err := supervise.NewMachine(s.ctx, s.deps, c)
		if err != nil {
			t.Fatalf("rebuild the machine: %v", err)
		}
		s.mu.Lock()
		s.machines[token] = m
		s.mu.Unlock()
		return m
	}
	t.Fatalf("no stored claim for %s", token)
	return nil
}

func (s *testSupervisor) Claims(ctx context.Context) ([]supervise.Claim, error) {
	return s.store.Claims(ctx)
}

// Step is the narrow read behind every operator answer about one claim, the store's own
// (store.Store.ClaimStep), unless a test has had the store refuse it (refuseStep).
func (s *testSupervisor) Step(ctx context.Context, token claim.Token) (supervise.StepRecord, error) {
	s.mu.Lock()
	refused := s.stepErr
	s.mu.Unlock()
	if refused != nil {
		return supervise.StepRecord{}, refused
	}
	return s.store.ClaimStep(ctx, token)
}

// refuseStep has every later read of one claim's step record fail with err, as a store that
// refuses that narrow read does. The claims read the listing makes is a different read and is
// left alone: a route about one claim makes this one and no other.
func (s *testSupervisor) refuseStep(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stepErr = err
}

type harness struct {
	t          *testing.T
	ctx        context.Context
	store      *store.Store
	runtime    *fake.Runtime
	conns      *fake.Conns
	clock      *armedClock
	tokens     *BootTokens
	supervisor *testSupervisor
	handler    http.Handler
	// decisions is the set the routes record the claims they are deciding in (Options.Decisions),
	// which the daemon's stop reads and which work a route posted holds until it has run.
	decisions *RouteDecisions
	// refusals is this server's memory of the launches it refused without answering
	// (Options.Refusals), nil for a harness built with none.
	refusals *Answering
	// readied is each claim ClaimReady was told of, in order.
	readied *[]supervise.Claim
	// reported is each capability report CapabilityReported was told of, in order, and
	// reportedContextErr the Err of the context each was told on, so a test can see whether that
	// context was the agent's request's or one that outlives its connection.
	reported           *[]capabilities.Report
	reportedContextErr *[]error
	// podUpdateRounds is what Options.PodUpdateRounds answers, which a test writes before it lists
	// the claims: the sweep that fills it in production is the daemon's, which this package has
	// none of.
	podUpdateRounds *PodUpdateRounds
	// handoffs is the Dispatch a completion route's harness reads handoff records from
	// (newHandoffHarness), which writeHandoff fills; nil for a harness serving no completion.
	handoffs *statusRecorder
}

func newHarness(t *testing.T) *harness {
	return newHarnessWith(t, harnessOptions{relaunchable: true})
}

// newHarnessUnrelaunchable is newHarness's negative control for the stale-caller refusals
// (LEGION-164): its supervise deps can relaunch nothing (Kubernetes's degraded mode, no
// plugin source known), so the server's own PluginRelaunchable is false too — the claim it names
// is never refused on this account, whatever contract it is recorded at.
func newHarnessUnrelaunchable(t *testing.T) *harness {
	return newHarnessWith(t, harnessOptions{})
}

// harnessOptions are what one harness differs from another in.
type harnessOptions struct {
	// relaunchable is whether this daemon can relaunch a stale-plugin claim at all
	// (PluginRelaunchable, wired identically onto supervise.Deps and Options so the two always
	// agree, as daemon.go wires them in production).
	relaunchable bool
	// bound is what the registration and the ready answer within (Options.WorkerRPCTimeout); zero
	// is testWorkerRPCTimeout, which is long enough that no ordinary route test meets it.
	bound time.Duration
	// refusals builds this server an Answering of its own, so a test can read the launches those
	// two refused without answering (Answering.Refused).
	refusals bool
}

// newHarnessWith is newHarness under opts. PluginContract is always this daemon's own
// (DaemonAPIVersion): the stale-plugin trigger lives in internal/supervise, exercised by its own
// tests.
func newHarnessWith(t *testing.T, opts harnessOptions) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := testStore(t)
	tokens := NewBootTokens(st)
	rt, conns := fake.NewRuntime(), fake.NewConns()
	clock := &armedClock{}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	relaunchable := opts.relaunchable
	// The memory of what this server refused, built before the supervision that reads it: the
	// daemon wires the same object into both (openSupervision's LaunchEnded and Options.Refusals),
	// so a harness that has one has the whole of R6's path.
	var refusals *Answering
	if opts.refusals {
		refusals = testAnswering(t)
	}
	sup := &testSupervisor{
		ctx: ctx,
		deps: supervise.Deps{
			Runtime: rt, Conns: conns, Store: tokens.Recording(st), Specs: fixedSpecs{}, Clock: clock, Log: quiet,
			Limits:             supervise.Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2},
			Timeouts:           testTimeouts,
			PluginContract:     DaemonAPIVersion,
			PluginRelaunchable: relaunchable,
			// The daemon's own wiring: a tree a workflow issue backs does not close here.
			TreeClosable: func(ctx context.Context, c supervise.Claim) (bool, error) {
				var issue *record.Issue
				if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
					var err error
					issue, err = record.NewStore().Issue(ctx, tx, c.Tree)
					return err
				}); err != nil {
					return false, err
				}
				return issue == nil, nil
			},
			// The daemon's own reading of a launch that did not survive, over the memory this
			// server writes: a launch it refused itself, or one that ended before it served a
			// route, is relaunched at no charge (dispatch://INFRA-2883 R6).
			LaunchEnded:  launchEndedBy(refusals),
			LaunchWaived: launchWaivedBy(refusals),
		},
		store:    st,
		machines: map[claim.Token]*supervise.Machine{},
		prompts:  map[claim.Token]string{},
	}
	readied := &[]supervise.Claim{}
	reported, reportedContextErr := &[]capabilities.Report{}, &[]error{}
	rounds := &PodUpdateRounds{}
	bound := opts.bound
	if bound == 0 {
		bound = testWorkerRPCTimeout
	}
	h := &harness{
		t: t, ctx: ctx, store: st, runtime: rt, conns: conns, clock: clock, tokens: tokens, supervisor: sup,
		decisions:          NewRouteDecisions(),
		readied:            readied,
		reported:           reported,
		reportedContextErr: reportedContextErr,
		podUpdateRounds:    rounds,
	}
	h.refusals = refusals
	handler := NewServer(Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: sup, BootTokens: tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: st, Log: quiet,
		Pool: st.Pool(), Record: record.NewStore(), Releaser: rt, Trees: st, ReleaseRecords: st, ClaimActivity: st,
		PluginRelaunchable: relaunchable,
		WorkerRPCTimeout:   bound,
		// The deployment's two settings multiplied, as daemon.go's serve multiplies them: the
		// supervision above and the server are wired from the one set of timeouts, so the
		// length a refusal names is the deadline that supervision arms (Options.RegistrationDeadline).
		RegistrationDeadline: testRegistrationDeadline,
		Refusals:             h.refusals,
		Decisions:            h.decisions,
		ClaimReady:           func(c supervise.Claim) { *readied = append(*readied, c) },
		PromptAgents:         testPromptAgents,
		PodUpdateRounds:      func() PodUpdateRounds { return *rounds },
		CapabilityReported: func(ctx context.Context, _ supervise.Claim, report capabilities.Report) {
			*reported = append(*reported, report)
			*reportedContextErr = append(*reportedContextErr, ctx.Err())
		},
	})
	h.handler = handler
	return h
}

// testWorkerRPCTimeout is the bound every harness answers a registration and a ready within
// unless a test asks for another (harnessOptions.bound): long enough that a route whose decision
// writes to the test's own Postgres never meets it, so only a test about the bound meets it.
const testWorkerRPCTimeout = 20 * time.Second

// testTimeouts are the supervise bounds every harness runs on, and the two the registration
// deadline is drawn from (testRegistrationDeadline).
var testTimeouts = supervise.Timeouts{
	Boot: time.Minute, RegistrationIntervals: 3, RPC: 5 * time.Second, Probe: 30 * time.Second,
	Stop: 10 * time.Second, LaunchBackoff: time.Millisecond,
}

// testRegistrationDeadline is the registration deadline every harness's server names in the
// refusal of a shim_connected claim (Options.RegistrationDeadline): this harness's own two
// timeouts multiplied, through the computation that owns the deadline, with the zero grace a
// claim past its hello gets (runtime.RegistrationDeadline, supervise's armRegistration).
var testRegistrationDeadline = runtime.RegistrationDeadline(testTimeouts.Boot, testTimeouts.RegistrationIntervals, 0)

// testAnswering is an Answering of this test's own, on a listener of its own, for the one thing a
// route reads it for: the memory of the launches it refused without answering (Options.Refusals).
// It serves the start handler and nothing asks it anything; the test's end stands it down.
func testAnswering(t *testing.T) *Answering {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	answering := NewAnswering(listener, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(answering.Stop)
	return answering
}

// launchEndedBy and launchWaivedBy are the daemon's two wirings of that memory onto the
// supervision deps (openSupervision), and nil for a harness built with no memory at all: a Deps
// with neither charges every launch failure, which is what every other test in this package
// expects of it.
func launchEndedBy(a *Answering) func(supervise.Claim, int) supervise.LaunchEnding {
	if a == nil {
		return nil
	}
	return a.LaunchEnded
}

func launchWaivedBy(a *Answering) func(supervise.Claim) {
	if a == nil {
		return nil
	}
	return a.LaunchWaived
}

// runPosted runs every relaunch the routes have posted since the last run, on the claim's own
// machine, as the goroutine that feeds that machine does (internal/daemon's PostStaleRelaunch),
// and reports how many it ran. Each post's decision record is released with it.
func (h *harness) runPosted() int {
	h.t.Helper()
	posted := h.supervisor.take()
	for _, post := range posted {
		m, supervised := h.supervisor.Machine(post.token)
		if !supervised {
			h.t.Fatalf("a relaunch was posted for %s, which this daemon does not supervise", post.token)
		}
		if err := m.RelaunchStaleRegistration(h.ctx, post.generation); err != nil {
			h.t.Fatalf("the relaunch posted for %s at generation %d: %v", post.token, post.generation, err)
		}
		m.Wait()
		post.released()
	}
	return len(posted)
}

// request sends one request through the server's own handler. A nil body sends none; a string is
// sent as it is, anything else as its JSON.
func (h *harness) request(method, target string, body any, header http.Header) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	switch body := body.(type) {
	case nil:
	case string:
		reader = bytes.NewBufferString(body)
	default:
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode the request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, target, reader)
	for name, values := range header {
		req.Header[name] = values
	}
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, req)
	return recorder
}

func (h *harness) operator(method, target string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(method, target, body, http.Header{"Authorization": {"Bearer " + testOperatorToken}})
}

// treeCleanup is whether tree's cleanup was reserved, and whether it was confirmed.
func (h *harness) treeCleanup(tree string) (started, confirmed bool) {
	h.t.Helper()
	if err := h.store.Pool().QueryRow(h.ctx, `select cleanup_started, cleanup_confirmed_at is not null from tree_lifecycles
		where project = $1 and tree = $2`, testProject, tree).Scan(&started, &confirmed); err != nil {
		h.t.Fatalf("read the lifecycle of %s: %v", tree, err)
	}
	return started, confirmed
}

// launch creates a claim the way the spawn route does and launches it, returning its token and
// the boot token its launch minted.
func (h *harness) launch(issue string, role claim.Role) (claim.Token, string) {
	h.t.Helper()
	return h.launchIn(issue, issue, role)
}

// launchIn is launch for an issue of tree: a sub-architect or a worker on a child. The tree's
// lifecycle is opened first under the workflow's own authority, as admission's promotion of the
// root opens it: that row is what says the tree runs (record.TreeLive), so a fixture whose tree
// must be live opens it the way the daemon does. A test about a tree the operator holds opens
// its own row under that authority instead.
func (h *harness) launchIn(tree, issue string, role claim.Role) (claim.Token, string) {
	h.t.Helper()
	token, err := claim.NewToken(testProject, issue, role)
	if err != nil {
		h.t.Fatalf("token: %v", err)
	}
	if _, err := h.store.OpenTreeLifecycle(h.ctx, testProject, tree, treelifecycle.AuthorityWorkflow); err != nil {
		h.t.Fatalf("open the lifecycle of %s: %v", tree, err)
	}
	m, _, err := h.supervisor.Create(h.ctx, supervise.Claim{
		Token: token, Project: testProject, Tree: tree, Issue: issue, Role: role, State: supervise.StateQueued,
	}, "role prompt")
	if err != nil {
		h.t.Fatalf("create %s: %v", token, err)
	}
	if err := m.Handle(h.ctx, supervise.RequestSpawn{Claim: token}); err != nil {
		h.t.Fatalf("spawn %s: %v", token, err)
	}
	return token, h.bootToken(token)
}

// bootToken is the boot token the claim's latest launch carried.
func (h *harness) bootToken(token claim.Token) string {
	h.t.Helper()
	calls := h.runtime.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if (calls[i].Method == "Spawn" || calls[i].Method == "Resume") && calls[i].Spec.Claim == token {
			return calls[i].Spec.BootToken
		}
	}
	h.t.Fatalf("no launch of %s was made", token)
	return ""
}

// relaunch has the claim's process found gone, so the machine launches it again at the next
// generation, and returns the new launch's boot token.
func (h *harness) relaunch(token claim.Token) string {
	h.t.Helper()
	m, _ := h.supervisor.Machine(token)
	loc := m.Claim().Locator
	if loc == nil {
		h.t.Fatalf("%s has no process to lose", token)
	}
	if err := m.Handle(h.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *loc, Kind: runtime.Gone}}); err != nil {
		h.t.Fatalf("lose %s's process: %v", token, err)
	}
	return h.bootToken(token)
}

func (h *harness) stored(token claim.Token) supervise.Claim {
	h.t.Helper()
	claims, err := h.store.Claims(h.ctx)
	if err != nil {
		h.t.Fatalf("read the claims: %v", err)
	}
	for _, c := range claims {
		if c.Token == token {
			return c
		}
	}
	h.t.Fatalf("the store holds no claim %s", token)
	return supervise.Claim{}
}

// recordIssue writes a workflow issue, as admission would have.
func (h *harness) recordIssue(issue record.Issue) {
	h.t.Helper()
	if err := h.store.Tx(h.ctx, func(tx pgx.Tx) error { return record.NewStore().PutIssue(h.ctx, tx, issue) }); err != nil {
		h.t.Fatalf("record issue %s: %v", issue.Key, err)
	}
}

func (h *harness) register(bootToken, session string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: bootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: "agent-" + session, PluginContract: DaemonAPIVersion,
	}, nil)
}

// registered registers session on the launch bootToken names and returns what the agent learned.
func (h *harness) registered(bootToken, session string) claim.RegisterResponse {
	h.t.Helper()
	recorder := h.register(bootToken, session)
	if recorder.Code != http.StatusOK {
		h.t.Fatalf("register %s = %d, want 200; body %s", session, recorder.Code, recorder.Body)
	}
	var response claim.RegisterResponse
	decodeInto(h.t, recorder, &response)
	return response
}

// ready registers session on the claim's latest launch and has its agent say it is ready, at the
// generation that launch carried.
func (h *harness) ready(token claim.Token, session string, generation uint64) {
	h.t.Helper()
	registered := h.registered(h.bootToken(token), session)
	if recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: generation,
	}, nil); recorder.Code != http.StatusNoContent {
		h.t.Fatalf("ready %s at generation %d = %d; body %s", token, generation, recorder.Code, recorder.Body)
	}
}

// registerContract registers session on the launch bootToken names, declaring contract as the
// plugin's. A current contract (DaemonAPIVersion) is accepted, returning what the agent learned,
// exactly as registered does. A contract that differs is recorded too — register writes
// PluginContract before supervise.staleRegistration decides anything — but a harness whose
// supervise deps can relaunch it (relaunchable true) relaunches that claim onto the current
// contract at once and refuses the registration itself with the 409 pi-legion exits on, so the
// caller gets no secret for it; a harness built with no relaunch (newHarnessUnrelaunchable)
// records the stale contract and answers 200 instead, since nothing here relaunches it.
func (h *harness) registerContract(bootToken, session string, contract int, relaunchable bool) claim.RegisterResponse {
	h.t.Helper()
	recorder := h.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: bootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: "agent-" + session, PluginContract: contract,
	}, nil)
	want := http.StatusOK
	if contract != DaemonAPIVersion && relaunchable {
		want = http.StatusConflict
	}
	if recorder.Code != want {
		h.t.Fatalf("register %s at contract %d = %d, want %d; body %s", session, contract, recorder.Code, want, recorder.Body)
	}
	if want != http.StatusOK {
		return claim.RegisterResponse{}
	}
	var response claim.RegisterResponse
	decodeInto(h.t, recorder, &response)
	return response
}

// makeStale seeds token's claim as the API's stale-caller rule reads it: recorded at contract
// rather than the daemon's own, live and holding a capability — exactly the window between the
// daemon finding a process stale (at boot, or at the registration that recorded the mismatch) and
// its relaunch taking effect (supervise.refreshPlugin, supervise.staleRegistration), before
// Machine.persist clears the capability a relaunched claim never keeps (supervise.HoldsCapability). The
// claim is written to the store directly and the machine rebuilt from it (testSupervisor.restart),
// as a daemon restart re-adopting a claim recorded this way would.
func (h *harness) makeStale(t *testing.T, token claim.Token, session, secret string, contract int) {
	t.Helper()
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	c := machine.Claim()
	c.Session = session
	c.CapabilityHash = secretHash(secret)
	c.PluginContract = contract
	if err := h.store.PutClaim(context.Background(), c); err != nil {
		t.Fatalf("seed a stale claim for %s: %v", token, err)
	}
	h.supervisor.restart(t, token)
}

func decodeInto(t *testing.T, recorder *httptest.ResponseRecorder, into any) {
	t.Helper()
	decoder := json.NewDecoder(recorder.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("decode the body: %v", err)
	}
}

// wantRefusal holds a response to the status and the sentence of one refusal.
func wantRefusal(t *testing.T, recorder *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, status, recorder.Body)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body, err)
	}
	if body["error"] != message {
		t.Fatalf("refusal = %q, want %q", body["error"], message)
	}
	if len(body) != 1 {
		t.Fatalf("refusal body %s carries more than its sentence", recorder.Body)
	}
}

func sortedTokens(claims []OperatorClaim) []string {
	tokens := make([]string, len(claims))
	for i, c := range claims {
		tokens[i] = string(c.Token)
	}
	sort.Strings(tokens)
	return tokens
}

// openTree opens tree's lifecycle under the workflow's authority, as admission's promotion of a
// root does. That row, and not a slot, is what says a tree runs (record.TreeLive) now that a
// slot counts one issue at work, so every fixture that seeds a running tree opens it. project is
// the Dispatch project key the tree's issues are recorded under.
func (h *harness) openTree(t *testing.T, project, tree string) {
	t.Helper()
	token, err := claim.ProjectToken(project)
	if err != nil {
		t.Fatalf("project token of %s: %v", project, err)
	}
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		_, err := record.NewStore().OpenTreeLifecycle(context.Background(), tx, token, tree, treelifecycle.AuthorityWorkflow)
		return err
	}); err != nil {
		t.Fatalf("open the tree lifecycle of %s: %v", tree, err)
	}
}
