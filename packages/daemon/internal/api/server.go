package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/memorygroup"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// readHeaderTimeout bounds how long a client may take to send its request headers; without it a
// slow-header client holds a listener slot for as long as it likes.
const readHeaderTimeout = 10 * time.Second

// stateReadBound is how long the state route gives its whole read — its repeatable-read
// transaction and the statements inside it — before it answers a failure and ends that
// transaction (stateRoute, Options.StateReadBound). Until INFRA-3169 the route's only bound was
// whether its client was still waiting, so a read that never finished held its connection for as
// long as the client held the socket, and 32 of those are the whole pool.
//
// Sixty seconds, because the healthy read is already slow and grows with the project: it measured
// 8.34 s and 4,055,424 bytes on the production deployment on 2026-11-12, so a bound under about
// 30 seconds would cut a legitimate read of the very deployment this fixes, and 60 is seven times
// today's healthy read.
const stateReadBound = 60 * time.Second

// stateRollbackBound is what the state route gives the rollback that ends its transaction once
// the read is over, on a context of its own (stateRoute). Five seconds: the rollback is one
// round trip to a server that just answered, so the only thing this waits out is a server that
// has stopped answering altogether, and waiting longer on one of those would hold the very
// connection the rollback is there to give back.
const stateRollbackBound = 5 * time.Second

// Options are what the routes answer from.
type Options struct {
	// State answers GET /legion/v1/state.
	State StateSource
	// StateTransactions opens the repeatable-read, read-only snapshot the state projection uses.
	StateTransactions StateTransactions
	// StateReadBound is how long the state route's read may take before it answers a failure and
	// gives its connection back; zero is stateReadBound, which is the value the daemon runs. A
	// test sets a smaller one so it need not wait out a minute.
	StateReadBound time.Duration
	// ConsumerLag reads the lag of the daemon's durable consumers from NATS (the daemon's
	// intake.Consumers.Lag), which the state route reads after its transaction commits and serves as
	// `consumers`. Nil, a daemon with no workflow and so no consumers, serves none.
	ConsumerLag func(ctx context.Context) []intake.ConsumerLag
	// Supervisor is the claims the claim and operator routes post their requests to.
	Supervisor Supervisor
	// PodUpdateRounds answers what the daemon's last pod-update sweep decided, which the claim
	// listing carries beside the claims (the daemon's podUpdateRoundsView). Nil answers the zero
	// record, which says the daemon has not swept yet: a deployment that serves the route with no
	// sweep behind it says so rather than read as a sweep that held no issue back.
	PodUpdateRounds func() PodUpdateRounds
	// BootTokens resolves a registration's boot token.
	BootTokens *BootTokens
	// Project and DispatchProject are the two names this one project answers to, and the daemon
	// holds both because a row written under one is invisible to a read with the other. Project is
	// the claim project token (`claim.ProjectToken`): lowercased and alphanumeric, the form every
	// claim token, tree lifecycle and controller record is keyed by. DispatchProject is the
	// project key Dispatch itself files the issues under (config.Config.Project, which Dispatch
	// constrains to `^[A-Z][A-Z0-9]{1,9}$`), and so the key of the issue records and of the
	// blockers mirror the release routes read. The two can never be equal in a deployment, since
	// the token is the lowercase of a key that must start uppercase.
	Project         string
	DispatchProject string
	// OperatorToken is the bearer every operator route compares against; empty admits no one.
	OperatorToken string
	// Controller is the project's controller record, which the controller secret route mints,
	// the claim registration route registers a session on, and the grants route authenticates
	// the registered session against.
	Controller ControllerStore
	// Steps are the design-gate steps the deployment gates (config.Gates.Steps), in the order it
	// named them: what register_gate resolves a registration's step against, and what the
	// controller secret route answers `legion controller start`. The empty list gates no step.
	Steps []record.DesignStep
	// ControllerLaunched is the daemon launching the project's controller itself (`controller:
	// daemon`): one controller runs per project, so the controller secret route refuses the operator
	// and a registration registers the controller only from a launch of the controller's claim.
	// Unset (`controller: operator`), only the operator's capability registers it.
	ControllerLaunched bool
	// PluginRelaunchable is supervise.Deps.PluginRelaunchable as the API sees it: whether this
	// daemon can relaunch a stale process onto a plugin at all. False is Kubernetes's degraded
	// mode, where a stale registration is recorded and the process is left running: nothing here
	// refuses it either, since refusing calls with no relaunch under way would stall agents that
	// work today.
	PluginRelaunchable bool
	// NoPluginInstall is supervise.Deps.NoPluginInstall as the API sees it: whether this claim's
	// launch installs no plugin at all, which only the daemon knows, since a role's backend and
	// the runtime it launches into are its configuration (internal/daemon's noPluginInstall — a
	// Claude Code role's plugin in a pod is its image's own claude-envoy). Nothing of this
	// daemon relaunches such a claim for its contract and its registration stands, so the routes
	// serve it on the contract it speaks (staleClaim). Nil is a daemon whose every claim a
	// relaunch can carry.
	NoPluginInstall func(c supervise.Claim) bool
	// Backend is the harness one role's claims run on (config.Config.BackendFor, the daemon's
	// own reading of `worker_backends` against runtime.Backends). A role's harness is the
	// daemon's configuration and nothing the claim itself records, so the one route that has to
	// know is handed it here: a compaction is an Oh My Pi command, and a Claude Code claim has
	// no Oh My Pi session to compact (compact). Nil answers Oh My Pi for every role, which is
	// what a deployment that configures no `worker_backends` runs.
	Backend func(role claim.Role) runtime.Backend
	// CallerPod is the runtime's pod at a request's address: the uid of the project's pod that
	// holds addr, and false for an address no pod of the project holds (the Sandbox runtime's
	// PodAt). GET /legion/v1/state authenticates nobody, so this is how it knows which claims'
	// plugin asked (stateFor). Nil is a runtime that runs no pods, whose every caller is served
	// the state as this contract writes it.
	CallerPod func(addr netip.Addr) (string, bool)
	// Ready reads the daemon's readiness latch, which GET /readyz answers from: false until the
	// daemon has built its runtime, installed its launcher acceptor and begun serving this API,
	// true from that moment on, and never again false of itself (the daemon's servingLatch). It
	// reads no database, no pool and nothing of the daemon's load, so the probe answers while
	// every connection is busy. Nil is a server that is ready: a caller that sets no readiness
	// option is serving by the time anything can ask it.
	Ready func() bool
	// Stopping is done once the daemon's stop begins, before the stop closes the worker stream
	// connections no route holds (RouteDecisions): an operator's request that would change a claim
	// is refused from then on (stopped), and a route whose decision fails logs it at Info
	// (logFailure), since the stop is what cut it short. A route reading on the request's own
	// context reports through logRequestFailure instead, which answers to the request's own ending
	// first and reaches this for every other failure: while the request is live, and once it has
	// ended when the call failed with something that is not that ending. Nil is never.
	Stopping context.Context
	// Drained is done once the daemon's stop has drained the API, or the drain ran out, and ends
	// every decision a route asked of a machine (decision). A decision in flight at the stop's
	// start gets the drain to finish, so an operator's suspension whose process is already exiting
	// is recorded. Nil is never.
	Drained context.Context
	// Decisions is where the routes record the claims whose decisions they are running, for the
	// daemon's stop to read (RouteDecisions). Nil is a set nothing reads.
	Decisions *RouteDecisions
	// Log receives what the routes decide; nil is slog.Default().
	Log *slog.Logger
	// Tokens mints the GitHub App leases credential routes return after redeeming a grant.
	Tokens appauth.Tokens
	// GitHubRepositories are the project's repositories as the daemon serves them
	// (daemon's repositories.Accepted): the controller's gh bundle holds a read-only token per
	// owner of them, each narrowed to that owner's repositories (appauth.ControllerCredential).
	GitHubRepositories func() []ghrepo.Repository
	// RepositoryRenames is the deployment's repository equivalence
	// (config.Config.RepositoryRenames): the controller's gh bundle holds one entry for a
	// configured rename's two owners (appauth.ControllerCredential). The zero value configures none.
	RepositoryRenames ghrepo.Renames
	// Serving is the repository GitHub holds a recorded one under now, which every GitHub call
	// this server makes is minted for and addressed to (serving): the daemon's one rule
	// (daemon's repositories.serving, which the Sandbox runtime's own token source is handed
	// too), so the mapping and the configured renames are read in one place and no caller here
	// writes a second copy of it. Nil serves a recorded repository under its own name, which a
	// test that configures no rename and a daemon with no GitHub Apps both want.
	Serving func(recorded ghrepo.Repository) ghrepo.Repository
	// Releaser releases what the runtime holds for a tree whose operator close reserved and
	// finished its cleanup: the daemon's runtime.
	Releaser store.TreeReleaser
	// Trees is the store's durable tree barrier, which the operator routes open a root's tree
	// through and reserve and finish a closed tree's cleanup through.
	Trees TreeLifecycles
	// Memory is the project's `memory` block (config.Project.Memory), under whose mode the
	// operator's spawn draws the memory group of a tree it opens for the first time
	// (memorygroup.DrawAndRecord). The zero block is a project that names none, which draws
	// every tree `none`. MemoryCoin is the coin the `random` mode is drawn with; nil is
	// memorygroup.CryptoCoin, and a test pins it. Now is the clock that draw's instant is read
	// from; nil is time.Now, and a test pins it.
	Memory     config.Memory
	MemoryCoin func() (bool, error)
	Now        func() time.Time
	// GitHubAPI is GitHub's REST root, which the daemon's own reads of a pull request and its
	// compares go under; empty, in production, is https://api.github.com, and a
	// test points it at a stand-in.
	GitHubAPI string
	// Grants mints and redeems the daemon-local one-command credential handles.
	Grants *credential.Grants
	// Pool is the daemon's guarded pool: the routes open their transactions on it, and a route
	// that asks it for a second connection while it holds one is refused at that call
	// (store.ErrNestedAcquire), which is why every route's context is marked where the mux is
	// built (NewServer).
	Pool     *store.Pool
	Handlers []intake.Handler
	// PrepareRepository readies what the workflow reads in memory of a Dispatch issue's repository
	// before release_children applies its view of a child (the daemon's prepareObserved, handed the
	// child as Dispatch's detail shows it, intake.ObservedIssue). Nil prepares nothing.
	PrepareRepository func(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error)
	// RepositoryUnread says whether a repository is left out for now because its GitHub App
	// installation read failed (the daemon's repositoryUnread): the daemon recovers the issues on
	// it from Dispatch's listing once GitHub answers. release_children never ends a wave on a todo
	// child it leaves unrecorded for one: it answers the child released and goes on, telling the
	// architect at once when the recovery will not record it (enterTodoChild). Nil leaves none out.
	RepositoryUnread func(resolved string) bool
	// AwaitRecovery leaves a child release_children left unrecorded on an unread repository to that
	// repository's recovery (the daemon's awaitRecovery), which tells the tree's architect of each
	// such child it did not record. Nil leaves none.
	AwaitRecovery func(repo, tree, child, parent string)
	// RoleFor is the role that works a phase (phase.RoleFor). The operator's retry of a failed
	// phase worker applies its issue's held phase only when that phase's role is the claim's
	// (retryClaim); nil retries no held phase, leaving every one to its architect.
	RoleFor func(phase.Phase) claim.Role
	// Record is the issue record the routes read and write (record.NewStore()). Nil is defaulted
	// to one: the Postgres implementation is stateless, so a server given none still draws an
	// operator tree's memory group. A route that needs a database as well tests Pool.
	Record   record.Store
	Dispatch dispatch.Client
	// ClaimReady is told of each claim whose agent's ready the claim took, on the session it
	// registered: a launched or relaunched agent once it has booted and can be prompted, or a
	// live one that took its Envoy role back. A boot's ready comes before the role, which its
	// session claims at its first turn (supervise.SaidReady). A ready the claim refuses tells
	// no one. Nil tells no one. It is called before the route answers the agent, so it returns
	// at once.
	ClaimReady func(c supervise.Claim)
	// HandoffCompleted is told of the issue of each phase completion the workflow recorded, once
	// the fact has committed: the daemon reads that issue's pull request from GitHub then, as the
	// backstop for a webhook that never arrived (the daemon's readIssuePullRequest). A completion
	// the route or the workflow refused, and a production check report, tell no one. Nil tells no
	// one. It is called before the route answers the agent, so it returns at once, and this
	// package reads no GitHub for it.
	HandoffCompleted func(issue string)
	// PromptAgents is the sorted names of every task agent the role prompts dispatch
	// (prompts.RoleReferences), which the registration answers so a session's capability report
	// measures the agents its role is told to run (LEGION-663). Nil or empty answers an empty list.
	PromptAgents []string
	// CapabilityReported is told, after a ready the claim took, of the capability report that ready
	// carried, normalised (capabilities.Normalize), with the claim as the ready left it. A ready that
	// carries no report — the controller's — tells no one, and so does a ready the claim refuses.
	// Nil tells no one. It may persist the report before it returns — the daemon's writes the
	// store — on a context that outlives the agent's connection, and the route answers the agent
	// once it has returned.
	CapabilityReported func(ctx context.Context, c supervise.Claim, report capabilities.Report)
	// ReleaseRecords is the store's record of what the idle release has done (store.Store), which
	// the claim route `POST /legion/v1/claims/release-records` serves a pod. Nil answers that route
	// 500 and changes nothing else.
	ReleaseRecords ReleaseRecordStore
	// ClaimActivity records what a claim's session reports it is doing (store.Store), which the
	// claim route `POST /legion/v1/claims/activity` writes. Nil answers that route 500 and
	// changes nothing else.
	ClaimActivity ClaimActivityStore
}

// TreeLifecycles is the store's durable tree barrier (store.Store) as the operator routes use it:
// an operator root opens its tree's lifecycle before its claim is stored, and the tree's close
// reserves the tree's cleanup, then finishes it once every claim of the tree has retired. The
// open's created runs in the open's own transaction where it created the tree's row, and is where
// the tree's memory group is drawn (store.Store.OpenOperatorTreeLifecycle).
type TreeLifecycles interface {
	OpenOperatorTreeLifecycle(ctx context.Context, project, tree string,
		created func(ctx context.Context, tx pgx.Tx) error) (treelifecycle.Lifecycle, error)
	ReserveOperatorTreeCleanup(ctx context.Context, project, tree string) (treelifecycle.Lifecycle, bool, error)
	CleanupReservedTree(ctx context.Context, project, tree string, epoch uint64, releaser store.TreeReleaser) error
}

// ReleaseRecordStore is the store's idle-release records (store.Store) as the claim route reads
// them: every release, resume and hold of the project recorded at or after a time.
type ReleaseRecordStore interface {
	PodReleaseRecords(ctx context.Context, project string, since time.Time) (store.PodReleaseRecords, error)
}

// ClaimActivityStore is the store's write of a session's activity report (store.Store) as the
// claim route makes it: the report lives on the claim's row and nowhere in the claim's machine,
// so the route reaches it here rather than through the daemon's pool. It writes only onto the
// launch generation names and answers whether the store holds the claim at that launch at all;
// a report older than the one recorded is dropped and still answers true.
type ClaimActivityStore interface {
	PutClaimActivity(ctx context.Context, token claim.Token, generation uint64, activity claim.SessionActivity) (bool, error)
}

type server struct {
	state             StateSource
	stateTransactions StateTransactions
	// stateBound is Options.StateReadBound, resolved: stateReadBound where the caller set none.
	stateBound  time.Duration
	consumerLag func(ctx context.Context) []intake.ConsumerLag
	callerPod   func(addr netip.Addr) (string, bool)
	supervisor  Supervisor
	// podUpdateRounds is Options.PodUpdateRounds.
	podUpdateRounds func() PodUpdateRounds
	bootTokens      *BootTokens
	// project and dispatchProject are Options.Project and Options.DispatchProject.
	project         string
	dispatchProject string
	operatorSet     bool
	operatorHash    [sha256.Size]byte
	controller      ControllerStore
	// steps is Options.Steps, resolved: the deployment's design-gate steps, nil never.
	steps []record.DesignStep
	// controllerLaunched is Options.ControllerLaunched.
	controllerLaunched bool
	// pluginRelaunchable is Options.PluginRelaunchable, noPluginInstall Options.NoPluginInstall.
	pluginRelaunchable bool
	noPluginInstall    func(c supervise.Claim) bool
	// backend is Options.Backend: the harness one role's claims run on.
	backend func(role claim.Role) runtime.Backend
	// controllerMu orders a capability mint against a registration and a controller grant, so a
	// grant the replaced registration authorised is never recorded after the mint revoked them.
	controllerMu       sync.Mutex
	tokens             appauth.Tokens
	githubRepositories func() []ghrepo.Repository
	// renames is the deployment's repository equivalence (Options.RepositoryRenames): the rule
	// that says which owner's installation holds a repository recorded under the name a transfer
	// moved it away from, which the controller's gh bundle reads (appauth.ControllerCredential).
	// The zero value configures none.
	renames ghrepo.Renames
	// serving is Options.Serving, the daemon's one rule for the repository GitHub holds a
	// recorded one under now, never nil here: NewServer serves a recorded repository under its
	// own name where the caller hands none.
	serving   func(recorded ghrepo.Repository) ghrepo.Repository
	githubAPI string
	grants    *credential.Grants
	releaser  store.TreeReleaser
	trees     TreeLifecycles
	// memory, coin and now are Options.Memory, Options.MemoryCoin and Options.Now: what the
	// spawn's own draw of a tree it opens for the first time reads (drawMemory).
	memory     config.Memory
	coin       func() (bool, error)
	now        func() time.Time
	pool       *store.Pool
	handlers   []intake.Handler
	prepare    func(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error)
	unread     func(resolved string) bool
	await      func(repo, tree, child, parent string)
	roleFor    func(phase.Phase) claim.Role
	records    record.Store
	dispatch   dispatch.Client
	claimReady func(c supervise.Claim)
	// handoffCompleted is Options.HandoffCompleted.
	handoffCompleted func(issue string)
	// promptAgents and capabilityReported are Options.PromptAgents and Options.CapabilityReported,
	// releaseRecordStore Options.ReleaseRecords and claimActivity Options.ClaimActivity.
	promptAgents       []string
	capabilityReported func(ctx context.Context, c supervise.Claim, report capabilities.Report)
	releaseRecordStore ReleaseRecordStore
	claimActivity      ClaimActivityStore
	// readiness is Options.Ready: the daemon's readiness latch, which /readyz answers from. nil is
	// a server that is ready, since a caller that sets no latch is serving already.
	readiness func() bool
	// stopping and drained are Options.Stopping and Options.Drained, decisions Options.Decisions.
	stopping  context.Context
	drained   context.Context
	decisions *RouteDecisions
	log       *slog.Logger
	// beforeForgetSessionHandle pauses the route's preflight in its concurrency regression test.
	beforeForgetSessionHandle func()
}

// NewServer builds the daemon's HTTP server on bind:port, the configured address: every interface
// only when bind is 0.0.0.0 or ::, as a daemon that runs as a pod binds. The caller owns its
// lifecycle (ListenAndServe, Shutdown).
//
// Three audiences, three kinds of route: the state everyone reads; the claim lifecycle an agent's
// plugin drives (register, ready, exit), authenticated by its pane's boot token and then by the
// secret its registration was issued; and the operator's spawn surface, authenticated by the
// operator bearer.
func NewServer(bind string, port int, opts Options) *http.Server {
	serving := opts.Serving
	if serving == nil {
		serving = func(recorded ghrepo.Repository) ghrepo.Repository { return recorded }
	}
	s := &server{
		state:              opts.State,
		stateTransactions:  opts.StateTransactions,
		stateBound:         opts.StateReadBound,
		consumerLag:        opts.ConsumerLag,
		callerPod:          opts.CallerPod,
		supervisor:         opts.Supervisor,
		podUpdateRounds:    opts.PodUpdateRounds,
		bootTokens:         opts.BootTokens,
		project:            opts.Project,
		dispatchProject:    opts.DispatchProject,
		controller:         opts.Controller,
		steps:              opts.Steps,
		controllerLaunched: opts.ControllerLaunched,
		pluginRelaunchable: opts.PluginRelaunchable,
		noPluginInstall:    opts.NoPluginInstall,
		backend:            opts.Backend,
		tokens:             opts.Tokens,
		githubRepositories: opts.GitHubRepositories,
		renames:            opts.RepositoryRenames,
		serving:            serving,
		githubAPI:          opts.GitHubAPI,
		releaser:           opts.Releaser,
		trees:              opts.Trees,
		memory:             opts.Memory,
		coin:               opts.MemoryCoin,
		now:                opts.Now,
		grants:             opts.Grants,
		pool:               opts.Pool,
		handlers:           opts.Handlers,
		prepare:            opts.PrepareRepository,
		unread:             opts.RepositoryUnread,
		await:              opts.AwaitRecovery,
		roleFor:            opts.RoleFor,
		records:            opts.Record,
		dispatch:           opts.Dispatch,
		claimReady:         opts.ClaimReady,
		handoffCompleted:   opts.HandoffCompleted,
		promptAgents:       opts.PromptAgents,
		capabilityReported: opts.CapabilityReported,
		releaseRecordStore: opts.ReleaseRecords,
		claimActivity:      opts.ClaimActivity,
		readiness:          opts.Ready,
		log:                opts.Log,
		stopping:           opts.Stopping,
		drained:            opts.Drained,
		decisions:          opts.Decisions,
	}
	if opts.OperatorToken != "" {
		s.operatorSet, s.operatorHash = true, sha256.Sum256([]byte(opts.OperatorToken))
	}
	if s.stateBound == 0 {
		s.stateBound = stateReadBound
	}
	if s.coin == nil {
		s.coin = memorygroup.CryptoCoin
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.records == nil {
		s.records = record.NewStore()
	}
	if s.grants == nil {
		s.grants = credential.New(nil)
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.stopping == nil {
		s.stopping = context.Background()
	}
	if s.drained == nil {
		s.drained = context.Background()
	}
	if s.decisions == nil {
		s.decisions = NewRouteDecisions()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /legion/v1/state", s.stateRoute)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.readyz)

	mux.HandleFunc("POST /legion/v1/claims/register", s.register)
	mux.HandleFunc("POST /legion/v1/claims/ready", s.ready)
	mux.HandleFunc("POST /legion/v1/claims/ready-for-update", s.readyForUpdate)
	mux.HandleFunc("POST /legion/v1/claims/exit", s.exit)
	mux.HandleFunc("POST /legion/v1/claims/turn-outcome", s.turnOutcome)
	mux.HandleFunc("POST /legion/v1/claims/activity", s.activity)
	mux.HandleFunc("POST /legion/v1/claims/release-records", s.releaseRecords)
	mux.HandleFunc("POST /legion/v1/grants", s.grant)
	mux.HandleFunc("POST /legion/v1/controller/secret", s.controllerSecret)
	mux.HandleFunc("POST /legion/v1/controller/github-credential", s.controllerGitHubCredential)
	mux.HandleFunc("POST /legion/v1/handoff/complete", s.handoffComplete)
	mux.HandleFunc("POST /legion/v1/issues/status", s.issueStatus)
	mux.HandleFunc("POST /legion/v1/claims/fresh-session", s.claimFreshSession)
	mux.HandleFunc("POST /legion/v1/claims/compact", s.claimCompact)
	mux.HandleFunc("POST /legion/v1/gates/register", s.gateRegister)
	mux.HandleFunc("POST /legion/v1/waves/release", s.waveRelease)
	mux.HandleFunc("POST /legion/v1/phase/backward", s.phaseBackward)
	mux.HandleFunc("POST /legion/v1/phase/retry", s.phaseRetry)
	mux.HandleFunc("POST /legion/v1/signoff", s.signOff)
	mux.HandleFunc("POST /legion/v1/roots/close", s.closeRoot)
	mux.HandleFunc("POST /legion/v1/children/park", s.parkChild)
	mux.HandleFunc("POST /legion/v1/children/rerun", s.rerunChild)

	mux.HandleFunc("POST /legion/v1/operator/claims", s.operator(s.spawn))
	mux.HandleFunc("GET /legion/v1/operator/claims", s.operator(s.list))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/deliver", s.operator(s.claimRequest("deliver", s.deliverEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/suspend", s.operator(s.claimRequest("suspend", suspendEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/resume", s.operator(s.claimRequest("resume", resumeEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/stop", s.operator(s.claimRequest("stop", stopEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/retry", s.operator(s.retryClaim))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/compact", s.operator(s.compactClaim))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/forget-session", s.operator(s.forgetSession))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/close", s.operator(s.closeTree))

	// The catch-all answers every unrouted request, including a known path asked for with the
	// wrong method: "no route" is the whole story the plugin, the operator's CLI, or a proof
	// script needs.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no route"})
	})

	return &http.Server{
		Addr:              net.JoinHostPort(bind, strconv.Itoa(port)),
		Handler:           trackTransactions(mux),
		ReadHeaderTimeout: readHeaderTimeout,
	}
}

// trackTransactions marks every request's context for the daemon's pool, in this one place
// because every route is a caller of it: a route that asks the pool for a second connection
// while it holds one — the state route's projection reading past its own transaction, say — is
// refused at that call (store.ErrNestedAcquire) instead of waiting for a connection only its own
// transaction can give back. A request handled on a goroutine of its own, outliving the request
// or running beside it, takes a mark of its own (store.ForConcurrentWorker) rather than this one.
func trackTransactions(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(store.WithTransactionTracking(r.Context())))
	})
}

// readyz answers the kubelet's readiness probe, which carries no token, from the daemon's
// readiness latch alone (Options.Ready): 503 `stopping` from the moment the daemon's stop begins
// (Options.Stopping), to a request on a connection the server still holds through its drain (a new
// connection gets no answer at all, since the drain closes the listener, which a probe reads as a
// failure too); 503 `starting` while the daemon has not reached serving, naming what it waits for,
// which only a request reaching the instant between the server starting and the latch being set
// sees, since the server serves nothing before that; and 200 once it has. The stop is read first,
// because a stop that begins before the daemon ever served is draining and not starting.
// It reads no database, no pool and nothing of the daemon's load, so it answers while every
// connection of the pool is busy.
//
// It is a readiness probe and never a liveness probe. The daemon's Kubernetes Service carries
// this API's port and the worker stream's port under one selector, so a probe that failed while
// the daemon was busy would take the worker stream out of the Service while every pod's launcher
// was dialling it, and the deployment runs one replica under the Recreate strategy, so no second
// pod answers in its place.
func (s *server) readyz(w http.ResponseWriter, _ *http.Request) {
	switch {
	case s.stopping.Err() != nil:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "stopping"})
	case s.readiness != nil && !s.readiness():
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"status": "starting", "waitingFor": "the daemon to serve"})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// decision is the context a route runs a decision of token's claim on. It outlives the request,
// so a caller that hangs up mid-request does not leave a registration or a stop half done, and it
// ends once the daemon's stop has drained the API (Options.Drained). Until the caller calls the
// returned function, which it does once the decision is made, the claim is recorded as decided by
// a route (RouteDecisions). A route that decides other claims too records each of them (begin).
func (s *server) decision(r *http.Request, token claim.Token) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	stop := context.AfterFunc(s.drained, cancel)
	ended := s.decisions.begin([]claim.Token{token})
	return ctx, func() {
		ended()
		stop()
		cancel()
	}
}

// RouteDecisions is the claims whose decisions an API route is running now (server.decision).
// The daemon's stop keeps the worker stream connections of these claims open through the API's
// drain and closes every other one: a route's suspension or stop sends its process the shutdown
// frame over that connection.
type RouteDecisions struct {
	mu     sync.Mutex
	claims map[claim.Token]int
}

// NewRouteDecisions is a set no route has recorded a claim in yet.
func NewRouteDecisions() *RouteDecisions {
	return &RouteDecisions{claims: map[claim.Token]int{}}
}

// Holds is whether a route is running a decision of token's claim now.
func (d *RouteDecisions) Holds(token claim.Token) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.claims[token] > 0
}

// begin records tokens as decided by a route until the returned function runs.
func (d *RouteDecisions) begin(tokens []claim.Token) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, token := range tokens {
		d.claims[token]++
	}
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, token := range tokens {
			if d.claims[token]--; d.claims[token] == 0 {
				delete(d.claims, token)
			}
		}
	}
}

// logFailure logs a route's failure at Error, or at Info once the daemon's stop has begun
// (Options.Stopping): the stop is what cut it short, and the next boot takes the claim up.
func (s *server) logFailure(msg string, args ...any) {
	if s.stopping.Err() != nil {
		s.log.Info(msg, args...)
		return
	}
	s.log.Error(msg, args...)
}

// logRequestFailure logs the failure of a call a route made on the request's own context: at Error
// while the request is live, and at Warn once the request's own context has ended and the call
// failed with that very error. Warn, because the daemon cannot tell a client that merely went away
// from its own sickness arriving as that same cancellation - a full connection pool, a hung
// connect and a held fact lock all reach a route as the request's context ending - so the line is
// not an error for an operator to act on, and not something to hide at the quietest level either:
// Warn leaves it where a reader can filter it on their own terms. The daemon's own stop is the
// other way such a call is cut short, and logFailure owns that branch.
func (s *server) logRequestFailure(r *http.Request, err error, msg string, args ...any) {
	args = append(args, "error", err)
	// The request's own error, not any context error: an inner deadline that passes as the client
	// hangs up is the daemon's own, and stays an error however the request itself ended.
	if ended := r.Context().Err(); ended != nil && errors.Is(err, ended) {
		s.log.Warn(msg, args...)
		return
	}
	s.logFailure(msg, args...)
}

// stopped answers 503 and reports true once the daemon's stop has begun (Options.Stopping): an
// operator's request that would change a claim is refused then, rather than decided without the
// claim's worker stream connection, which the stop closes for every claim no route holds. The
// caller asks only after it has recorded its claims (decision, RouteDecisions.begin), so a request
// recorded too late for the stop to keep its connections always finds the stop begun; closeTree's
// second record, after the root's close, is the one exception, and says why. A request reaches
// this once the stop has begun only if the server accepted it before the stop began, or while the
// halt waits on the supervisor's lock before the daemon's serve shuts the server down, which closes
// its listener.
func (s *server) stopped(w http.ResponseWriter, request string) bool {
	refusal := s.stopRefusal(request)
	if refusal == "" {
		return false
	}
	writeJSON(w, http.StatusServiceUnavailable, errorBody(refusal))
	return true
}

// stopRefusal is that refusal's sentence, and empty while the daemon runs: one caller writes it
// as its own answer (stopped), and one hands it back for a caller that answers something else
// around it (restartWorkedPhase, whose fresh session started the claim and reports only that
// nothing restarted its phase). The log line is written here, so a request refused by the stop
// is logged once whichever of the two gives it.
func (s *server) stopRefusal(request string) string {
	if s.stopping.Err() == nil {
		return ""
	}
	s.log.Info("api: refused an operator request: the daemon is stopping", "request", request)
	return request + " refused: the daemon is stopping; ask again once it is back"
}

// stateRoute answers the state out of one repeatable-read, read-only snapshot, under a bound of
// the daemon's own (stateReadBound, Options.StateReadBound). Before INFRA-3169 the only thing
// that ended this read was the client giving up, so a read that never finished held its
// connection as long as the client held its socket, and 32 of those are the whole pool.
//
// The bound is laid twice over the same work, because the two ends it has to catch are different
// ones. The context deadline ends the handler's own waiting — on the Go work between statements
// and on anything that is not a statement — and statement_timeout ends a statement that runs
// long inside the transaction, a lock wait included, which no context deadline reaches until the
// driver's cancel lands. It is `set local`, so it holds for this transaction alone and never for
// the pool: a migration legitimately runs long DDL in one statement (internal/store's apply),
// and a statement bound on the pool would cut it. The pool carries the idle bound instead, which
// is the one that is true of every transaction (internal/store's idleBound).
//
// http.TimeoutHandler is not the way to do this and must not be added: it buffers the whole
// response into a buffer of its own before writing it, and this route's body is buffered once
// already — writeJSON hands the body to json.Marshal, which materialises all of it before a byte
// is written — so the handler would hold a second full copy of a body that measured 4,055,424
// bytes on the production deployment on 2026-11-12. One such copy per concurrent read is a
// second resource failure inside the first one this bounds.
func (s *server) stateRoute(w http.ResponseWriter, r *http.Request) {
	if s.stateTransactions == nil {
		s.log.Error("api: state transaction source is unset")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "state unavailable"})
		return
	}
	reading, cancel := context.WithTimeout(r.Context(), s.stateBound)
	defer cancel()
	tx, err := s.stateTransactions.BeginTx(reading, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		s.logRequestFailure(r, err, "api: begin state transaction failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "state unavailable"})
		return
	}
	defer func() {
		// The rollback runs on a context detached from the read's, with a little budget of its
		// own: the moment the read's deadline passes is exactly the moment this has to reach the
		// server, and a rollback asked for on the context that just expired is never sent at all,
		// which gives the pool back a connection to throw away rather than one with its
		// transaction ended.
		ending, endRollback := context.WithTimeout(context.WithoutCancel(r.Context()), stateRollbackBound)
		defer endRollback()
		_ = tx.Rollback(ending)
	}()

	if _, err := tx.Exec(reading, "set local statement_timeout = "+strconv.FormatInt(statementBound(s.stateBound), 10)); err != nil {
		s.logRequestFailure(r, err, "api: bound the state transaction's statements failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "state unavailable"})
		return
	}
	state, claims, err := s.state.State(reading, tx)
	if err != nil {
		s.logRequestFailure(r, err, "api: state source failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "state unavailable"})
		return
	}
	if err := tx.Commit(reading); err != nil {
		s.logRequestFailure(r, err, "api: commit state transaction failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "state unavailable"})
		return
	}
	// The consumers' lag is read from NATS after the commit, so a slow or silent NATS server holds
	// no snapshot open; what it does not answer is each consumer's `unavailable`, never a refusal.
	if s.consumerLag != nil {
		state.Consumers = ConsumerLagViewsOf(s.consumerLag(reading))
	}
	writeJSON(w, http.StatusOK, s.stateFor(r, state, claims))
}

// statementBound is bound as PostgreSQL's statement_timeout takes it: whole milliseconds, and
// never the 0 a bound under a millisecond truncates to. In PostgreSQL 0 is no statement timeout
// at all, which is the opposite of what a caller that set a bound asked for, so a non-zero bound
// renders as the one millisecond that is still a bound. No caller sets a sub-millisecond bound
// today (stateReadBound is 60 s); this is what keeps one that did from switching the bound off.
func statementBound(bound time.Duration) int64 {
	milliseconds := bound.Milliseconds()
	if milliseconds == 0 && bound != 0 {
		return 1
	}
	return milliseconds
}

// stateFor is state as the plugin that asked for it reads it, decided from claims, the project's
// claims the state's own snapshot holds. The route authenticates nobody, so the caller is known
// by its address alone: a pod of the project (Options.CallerPod), whose claims record the plugin
// contract each registration declared. A plugin older than INFRA-1048 — which is who this
// function is kept for — reads the state with a strict schema and refuses the whole body for one
// key it does not know, where today's plugin strips one; and an update round serves a claim of
// the contract before it until its pod is replaced (version.go's 46). So a caller in a pod whose
// claims record a contract below supervise.PodUpdateContract, the contract that added
// `updateRound` and `updateHead` to the claim views, is served the state without them
// (withoutUpdateRounds), which is the body that contract's schema describes; a caller below
// memoryContract, the contract that added the issues' memory group, is served it without those
// three fields (withoutMemory); a caller below treeContract, the contract that named the live
// trees beside the issues holding a slot, is served it without `admission.trees` and without the
// issues' `tree` (withoutTrees); a caller below wakesWaitingContract, the contract that added
// the wakes the pacing holds, is served it without `wakesWaiting` (withoutWakesWaiting); a
// caller below gatedStepsContract, the contract that added the issues' gated steps, is served it
// without that field (withoutGatedSteps); a caller below retiredStrandedContract, the contract
// that let a stranded claim's state be `retired`, is served the report of claims nothing will
// wake without its retired entries (withoutRetiredStranded); a caller below stallRecordContract,
// the contract that added the claim views' step record, is served it without `step`
// (withoutStallRecord); a caller below waitingLineContract, the contract that added each
// waiting issue's place in the line, is served it without `waitingLine` (withoutWaitingLine); a
// caller below backwardMovesContract, the contract that added each worker's counts of the moves
// back its issue has made, is served it without `backwardMoves` (withoutBackwardMoves); a
// caller below claimsToRefreshContract, the contract that added the count of claims the
// stale-plugin lane still has to refresh, is served it without `claimsToRefresh`
// (withoutClaimsToRefresh); a caller below recordFieldsContract, the contract that decided
// every field of the claim record and of the issue record, is served it without the nine keys
// that contract added together (withoutRecordFields): a claim view's `servingGeneration`,
// `updateReasons`, `providerErrorRun`, `pendingTask`, `failure` and `pendingCause`, and an
// issue's `heldFrom`, `lingerUntil` and `ancestry`, and without the `contract` mark that same
// contract added beside the daemon's block; a caller below whyAndLineFieldsContract, the
// contract that added the one rendered answer an issue that is not working now carries and the
// waiting line's own fields beside it, is served every issue without `why` and every member of
// the line with its `place` and its `length` alone (withoutWhyAndLineFields), which is the body
// waitingLineContract's own schema describes; a caller below leakCheckCauseContract, the
// contract that added the sentence a claim's launch was refused with because the shared public
// leak check could not be installed, is served every claim view without `leakCheckCause`
// (withoutLeakCheckCause); a caller below phaseEntriesContract, the contract that added the
// entry the issue's current phase stands on and the entry its role was given, is served it
// without `phaseEntry` and `givenEntry` (withoutPhaseEntries); a caller below heldContract, the
// contract that named the issues the daemon's record holds in the workflow and queues nowhere,
// is served it without `admission.held` (withoutHeld); a caller below quietTimeContract, the
// contract that added each issue's measure of its tree's quiet time, is served it without
// `treeMovedAt` and `quietHours` (withoutQuietTime); and a caller below mergeWaitContract, the
// contract that added what a reviewing issue's round waits on from GitHub, is served it without
// `mergeWait` (withoutMergeWait).
// The narrowings compose: each is applied when the state carries something of its own, so a
// caller below all of them — a pod below supervise.PodUpdateContract — is served a body
// carrying none of the keys they strip, the line's own fields being fields of a view such a
// caller is served none of at all, each of them absent rather than empty, since a strict reader
// before the contract that added a key refuses it whether or not it names anything, and with no
// retired entry under `strandedClaims`. Every later reader takes each key a row of
// stateNarrowings strips as optional for exactly these bodies, and
// `packages/contracts/src/legion-api.ts` marks each one so. Any other
// caller — a pod at this contract, the operator's `legion state`, a tmux pane — is served the
// state as this contract writes it.
//
// Each narrowing is written to the daemon's own log as it is applied, one line naming the
// caller, the contract that caller's pod records, the contract this daemon declares and what
// that arm dropped (stateNarrowings, narrowedTheState): the narrowed body cannot say of itself
// that it was narrowed — a key added to it is what the narrowing exists to prevent — so an
// absent fact and a withheld fact would otherwise read alike (dispatch://INFRA-3083, R12). What
// the body does carry, for a caller at or above recordFieldsContract and for no caller below
// it, is the two numbers those lines name (`contract`, ContractView), which is how the one
// reader that is never narrowed — the operator's `legion state --json` — says which contract
// its answer was built at.
//
// The claims are the snapshot's own because the body is: a claim and the state it was projected
// into cannot come from two moments (store.ClaimsTx). A state whose claims the source could not
// read is no state at all - its claim views are built from them - so the route refuses it before
// this runs, and no readable state is left whose caller's contract is unknown.
func (s *server) stateFor(r *http.Request, state State, claims []supervise.Claim) State {
	// The mark is written before anything is taken out, so the narrowing that strips it is the
	// pair of the question that finds it (carriesRecordFields, withoutRecordFields) rather than
	// a key written only where it is allowed: a caller below that contract is served no mark,
	// and the line that records its narrowing names the mark among what it dropped.
	state.Contract = &ContractView{Daemon: DaemonAPIVersion}
	// A daemon that does not know its callers' pods serves every caller the state as it is, and
	// scans nothing for it: under runtime tmux that is every request of every pane and of
	// `legion state`.
	if s.callerPod == nil {
		return state
	}
	caller, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return state
	}
	pod, inPod := s.callerPod(caller.Addr().Unmap())
	if !inPod {
		return state
	}
	// Before treeContract, a state with no update round on any claim view and no memory group on
	// any issue was the body every contract reads, and was served without looking at the claims
	// at all. There is no such body since: the admission block writes `trees` on every one of
	// them (carriesTrees), so a caller in a pod has its contract read for every state request.
	// What a downgrade would strip is looked for only once the contract that needs it is known,
	// so a caller at this contract — every caller, outside the window in which a pod of the
	// contract before it is still being replaced — costs no scan of the issues.
	contract := podContract(claims, pod)
	if contract == 0 {
		return state
	}
	state.Contract.Caller = contract
	for _, narrowing := range stateNarrowings {
		if contract >= narrowing.contract || !narrowing.carries(state) {
			continue
		}
		state = narrowing.without(state)
		s.log.Info(narrowedTheState, "caller", r.RemoteAddr, "pod", pod,
			"pluginContract", contract, "daemonApiVersion", DaemonAPIVersion,
			"dropped", narrowing.dropped)
	}
	return state
}

// narrowedTheState is the message of the line the daemon writes for each narrowing it applies,
// named here because the test that reads the daemon's own log reads the same string
// (TestANarrowedBodySaysSo).
const narrowedTheState = "api: narrowed the state to the caller's plugin contract"

// stateNarrowing is one of the narrowings stateFor applies: the contract that added the
// keys to this route, whether a state carries anything of it, the body served without it, and
// what the log line names as dropped. Each arm carries its own `dropped` rather than the
// contract number alone, because a reader of the log who is told only a number has to find out
// elsewhere what left the body.
type stateNarrowing struct {
	contract int
	carries  func(State) bool
	without  func(State) State
	dropped  string
}

// stateNarrowings are the narrowings, lowest contract first, which is the order stateFor applies
// them in: a caller below all of them is served a body with every one of these keys gone. The
// order is the history of the route and nothing a strip depends on, so a row added at the end
// needs no reading of the rows before it. One pair does touch one key, and it is the reason
// this sentence names a pair rather than claiming none: waitingLineContract's row drops the
// issues' `waitingLine` whole, and whyAndLineFieldsContract's takes the members that contract
// added from inside that same field. They still do not depend on their order, because a caller
// below both is served no `waitingLine` at all whichever runs first — the later one then finds
// the key gone and does nothing — and a caller below only the higher one keeps the field with
// its `place` and its `length`, which is what the lower row's own contract describes. A
// later row that takes something out of a field an earlier row drops whole is sound on
// those same terms; one that puts a key back, or reads what an earlier row left, is not, and
// this table is the wrong shape for it. The order is kept beyond that so a reader of this
// table and a reader of version.go walk the contracts the same way.
var stateNarrowings = []stateNarrowing{
	{supervise.PodUpdateContract, carriesUpdateRounds, withoutUpdateRounds, "the claim views' updateRound and updateHead"},
	{memoryContract, carriesMemory, withoutMemory, "the issues' memoryGroup, memoryDrawMode and memoryDrawnAt"},
	{treeContract, carriesTrees, withoutTrees, "admission.trees and the issues' tree"},
	{wakesWaitingContract, carriesWakesWaiting, withoutWakesWaiting, "wakesWaiting"},
	{gatedStepsContract, carriesGatedSteps, withoutGatedSteps, "the issues' gatedSteps"},
	{retiredStrandedContract, carriesRetiredStranded, withoutRetiredStranded, "the retired entries of strandedClaims"},
	{stallRecordContract, carriesStallRecord, withoutStallRecord, "the claim views' step"},
	{waitingLineContract, carriesWaitingLine, withoutWaitingLine, "the issues' waitingLine"},
	{backwardMovesContract, carriesBackwardMoves, withoutBackwardMoves, "the workers' backwardMoves"},
	{claimsToRefreshContract, carriesClaimsToRefresh, withoutClaimsToRefresh, "claimsToRefresh"},
	{recordFieldsContract, carriesRecordFields, withoutRecordFields, "the claim views' servingGeneration, " +
		"updateReasons, providerErrorRun, pendingTask, failure and pendingCause, the issues' heldFrom, " +
		"lingerUntil and ancestry, and the contract mark"},
	{whyAndLineFieldsContract, carriesWhyAndLineFields, withoutWhyAndLineFields, "the issues' why and " +
		"every field of the waiting line beside place and length"},
	{leakCheckCauseContract, carriesLeakCheckCause, withoutLeakCheckCause, "the claim views' leakCheckCause"},
	{phaseEntriesContract, carriesPhaseEntries, withoutPhaseEntries, "the issues' phaseEntry and givenEntry"},
	{heldContract, carriesHeld, withoutHeld, "admission.held"},
	{quietTimeContract, carriesQuietTime, withoutQuietTime, "the issues' treeMovedAt and quietHours"},
	{mergeWaitContract, carriesMergeWait, withoutMergeWait, "the issues' mergeWait"},
}

// memoryContract is the daemon contract that added the issues' memory group to the state
// (version.go's 49): a plugin before it reads the state with a strict schema that knows none of
// `memoryGroup`, `memoryDrawMode` and `memoryDrawnAt` and refuses the whole body for one of them.
const memoryContract = 49

// treeContract is the daemon contract that made the admission cap count the issues at work rather
// than the trees (version.go's 51), which added `admission.trees`, the root of every live tree,
// and each issue's `tree`, its own tree's root: a plugin before it reads the state with a strict
// schema that knows neither key and refuses the whole body for one of them.
const treeContract = 51

// heldContract is the daemon contract that added `admission.held` to the state (version.go's
// 98), the issues the daemon's record holds in the workflow that stand in neither `active` nor
// `waiting`, each with the one word for why: a plugin before it reads the state with a strict
// schema that knows no such key and refuses the whole body for it, empty as surely as filled.
const heldContract = 98

// wakesWaitingContract is the daemon contract that added `wakesWaiting` to the state (version.go's
// 53): a plugin before it reads the state with a strict schema that knows no such key and refuses
// the whole body for it.
const wakesWaitingContract = 53

// gatedStepsContract is the daemon contract that added the issues' gated steps to the state
// (version.go's 55): a plugin before it reads the state with a strict schema that knows no
// `gatedSteps` and refuses the whole body for it.
const gatedStepsContract = 55

// retiredStrandedContract is the daemon contract whose entry in version.go added `retired` to the
// states a stranded claim may be in (version.go's 56): a plugin before it reads
// `strandedClaims[].state` with a strict enum of two values, `suspended` and `failed`, and refuses
// the whole body for a third.
const retiredStrandedContract = 56

// stallRecordContract is the daemon contract that added each claim view's `step`, what the daemon
// last saw that claim's agent do (dispatch://INFRA-1274). A plugin before it is served the state
// without the key: one built before sjawhar/legion#2788 reads it with a strict schema and would
// refuse the whole body for a key it does not name, and one built after strips the key instead.
//
// It is 57 because that is the number DaemonAPIVersion took at the bump this record forced: main
// held 56, INFRA-1480's retired stranded state, which carries no step record, so that branch took
// the first number above it, and the release that carries the record declares that number. This
// daemon's own contract is never below it — DaemonAPIVersion (version.go) declares what this
// binary serves, and that number only rises — so a pod at either is served the record and only a
// pod still running an older plugin is not.
const stallRecordContract = 57

// waitingLineContract is the daemon contract that added each waiting issue's place in the line
// to the state (version.go's 58). A plugin before it is served the state without the field:
// one built before sjawhar/legion#2788 reads it with a strict schema and would refuse the
// whole body for a key it does not name, and one built after strips the key instead, so the
// strip is what keeps the field from a caller that did not ask for it rather than what keeps
// such a caller reading at all.
const waitingLineContract = 58

// backwardMovesContract is the daemon contract that added each worker's counts of the moves back
// its issue has made to the state (version.go's 62). A plugin before it is served the state
// without the field: one built before sjawhar/legion#2788 reads it with a strict schema and
// would refuse the whole body for a key it does not name, and one built after strips the key
// instead, so the strip is what keeps the counts from a caller that did not ask for them rather
// than what keeps such a caller reading at all.
const backwardMovesContract = 62

// claimsToRefreshContract is the daemon contract that added `claimsToRefresh`, how many claims
// the stale-plugin lane still has to relaunch onto this daemon's plugin (version.go's 73). A
// plugin before it is served the state without the key: one built before sjawhar/legion#2788
// reads it with a strict schema and would refuse the whole body for a key it does not name, and
// one built after strips the key instead, so the strip is what keeps the count from a caller
// that did not ask for it rather than what keeps such a caller reading at all. It was the
// highest of the bands when it was taken, this daemon's own contract then; recordFieldsContract,
// whyAndLineFieldsContract and every later row of stateNarrowings stand above it now, so a
// caller in a pod at 73 or 74 is served the count and none of the keys those later rows add.
const claimsToRefreshContract = 73

// recordFieldsContract is the daemon contract that decided every field of the claim record and
// of the issue record, served or withheld (dispatch://INFRA-3083, ClaimRecordFields and
// IssueRecordFields). It added to this route a claim view's `servingGeneration`,
// `updateReasons`, `providerErrorRun`, `pendingTask`, `failure` and `pendingCause`, an issue's
// `heldFrom`, `lingerUntil` and `ancestry`, and the `contract` mark beside the daemon's block,
// which is what its row of stateNarrowings names as dropped. A plugin before it is served the
// state without any of them: one built before sjawhar/legion#2788 reads it with a strict schema
// and would refuse the whole body for a key it does not name, and one built after strips the key
// instead.
//
// They arrive at this one number and leave together, so the route takes one triple for them
// (carriesRecordFields, withoutRecordFields) rather than one for each, which would copy the
// issue map once for each in one request.
const recordFieldsContract = 75

// whyAndLineFieldsContract is the daemon contract that added the one rendered answer every issue
// that is not working now carries, `why` (WhyView), and the waiting line's own fields beside it:
// `part`, `kind`, `partPlace`, `partLength`, `holdsSlot`, `since`, `rootRank`, `ownRank`,
// `rootKey` and `reason` on WaitingLineView (version.go's 85). The two halves ride one contract
// and so one strip (withoutWhyAndLineFields): they are one answer read two ways, the sentence a
// person reads and the fields a program acts on, one pass of the projection renders both from
// the same facts, and both arrive in this one change, so no caller stands between them and no
// second number could tell them apart. A plugin below 85 is served every issue without `why` and
// every member of the line with its `place` and its `length` alone, the two waitingLineContract
// added — no `partPlace` and no other field of this contract reaches such a caller: one built
// before sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole
// body for a key it does not name, and one built after strips the key instead, so the strip is
// what keeps these fields from a caller that did not ask for them rather than what keeps such a
// caller reading at all.
const whyAndLineFieldsContract = 85

// leakCheckCauseContract is the daemon contract that added each claim view's `leakCheckCause`,
// the sentence the claim's latest launch try was refused with because the shared public leak
// check could not be installed in the role's pod (version.go's 87, ClaimView.LeakCheckCause). A
// plugin below it is served every claim view without the key: one built before
// sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole body for a
// key it does not name, and one built after strips the key instead, so the strip is what keeps
// the sentence from a caller that did not ask for it rather than what keeps such a caller
// reading at all.
const leakCheckCauseContract = 87

// phaseEntriesContract is the daemon contract that added each issue's `phaseEntry` and
// `givenEntry`, the entry the role of its current phase stands on and the entry that role was
// given (version.go's 94). A plugin before it is served the state without the two keys: one
// built before sjawhar/legion#2788 reads it with a strict schema and would refuse the whole body
// for a key it does not name, and one built after strips the keys instead, so the strip is what
// keeps the numbers from a caller that did not ask for them rather than what keeps such a caller
// reading at all.
const phaseEntriesContract = 94

// quietTimeContract is the daemon contract that added each issue's measure of its tree's quiet
// time to the state (version.go's 111): `treeMovedAt`, the newest movement the daemon has seen
// anywhere in that tree, and `quietHours`, how long it has been still. A plugin before it is
// served the state without the two keys: one built before sjawhar/legion#2788 reads it with a
// strict schema and would refuse the whole body for a key it does not name, and one built after
// strips them instead, so the strip is what keeps the pair from a caller that did not ask for
// it rather than what keeps such a caller reading at all. The two are one gate because they are
// one measure: a caller served the movement without the quiet time beside it, or the other way
// about, would hold half of what one read of the tree said.
const quietTimeContract = 111

// mergeWaitContract is the daemon contract that added `mergeWait`, what a reviewing issue's
// round waits on from GitHub (version.go's 115). A plugin before it is served the state without
// the object: one built before sjawhar/legion#2788 reads it with a strict schema and would
// refuse the whole body for a key it does not name, and one built after strips the key instead,
// so the strip is what keeps the object from a caller that did not ask for it rather than what
// keeps such a caller reading at all. A key's contract is the one that added it, so this is the
// number dispatch://INFRA-3027 declares and no band of another change: a caller from a pod
// below it is served the state without `mergeWait`, whatever else the contracts it does reach
// give it.
const mergeWaitContract = 115

// carriesUpdateRounds is whether any claim view of state carries `updateRound` or `updateHead`.
func carriesUpdateRounds(state State) bool {
	carries := func(v ClaimView) bool { return v.UpdateRound != "" || v.UpdateHead != "" }
	for _, issue := range state.Issues {
		if issue.Architect != nil && carries(*issue.Architect) {
			return true
		}
		for _, worker := range issue.Workers {
			if carries(worker.Claim) {
				return true
			}
		}
	}
	return false
}

// carriesMemory is whether any issue of state carries its tree's memory draw.
func carriesMemory(state State) bool {
	for _, issue := range state.Issues {
		if issue.MemoryGroup != "" || issue.MemoryDrawMode != "" || issue.MemoryDrawnAt != nil {
			return true
		}
	}
	return false
}

// carriesTrees is whether state carries either key treeContract added: `admission.trees` or an
// issue's `tree`. It carries `admission.trees` whatever the daemon holds, since the admission block
// writes that key on every body, empty rather than null (Admission.MarshalJSON), and the strict
// reader before the contract refuses the empty key as surely as one naming a tree. So this is true
// of every state the route writes; it is asked beside every other carries function all the same,
// because stateFor puts the same question to every row and because a later contract that stops
// writing `trees` on every body answers it here.
func carriesTrees(State) bool { return true }

// carriesHeld is whether state carries the key heldContract added: `admission.held`, the issues the
// daemon's record holds in the workflow that stand in neither `active` nor `waiting`. It carries
// that key whatever the daemon holds, since the admission block writes it on every body, empty
// rather than null (Admission.MarshalJSON), and the strict reader before the contract refuses the
// empty key as surely as one naming an issue. So this is true of every state the route writes; it
// is asked beside every other carries function all the same, because stateFor puts the same
// question to every row and because a later contract that stops writing `held` on every body
// answers it here.
func carriesHeld(State) bool { return true }

// carriesWakesWaiting is whether state carries the key wakesWaitingContract added: `wakesWaiting`,
// the wakes the pacing holds. It carries that key whatever the daemon holds, since the state writes
// it on every body, empty rather than null (State.MarshalJSON), and the strict reader before the
// contract refuses the empty key as surely as one naming a wake. So this is true of every state the
// route writes; it is asked beside every other carries function all the same, because stateFor puts
// the same question to every row and because a later contract that stops writing `wakesWaiting` on
// every body answers it here.
func carriesWakesWaiting(State) bool { return true }

// carriesClaimsToRefresh is whether state carries the key claimsToRefreshContract added:
// `claimsToRefresh`, how many claims the stale-plugin lane still has left. It carries that key
// whatever the daemon holds, since the state writes the number on every body, 0 rather than absent
// (State.MarshalJSON), and the strict reader before the contract refuses the key when it is 0 as
// surely as when it names a number. So this is true of every state the route writes, as
// carriesWakesWaiting is; it is asked beside every other carries function all the same, because
// stateFor puts the same question to every row and because a later contract that stops writing
// `claimsToRefresh` on every body answers it here.
func carriesClaimsToRefresh(State) bool { return true }

// carriesGatedSteps is whether any issue of state carries the deployment's gated steps.
func carriesGatedSteps(state State) bool {
	for _, issue := range state.Issues {
		if len(issue.GatedSteps) > 0 {
			return true
		}
	}
	return false
}

// carriesStallRecord is whether any claim view of state carries the key stallRecordContract
// added: `step`, the claim's record of its agent's last step. Unlike `trees` and `wakesWaiting`
// it is written only where there is one, so this is asked of the state rather than answered
// true, as carriesUpdateRounds and carriesMemory are.
func carriesStallRecord(state State) bool {
	return anyClaimView(state, func(view ClaimView) bool { return view.Step != nil })
}

// carriesRecordFields is whether state carries any key recordFieldsContract added: the `contract`
// mark beside the daemon's block, a claim view's `servingGeneration`, `updateReasons`,
// `providerErrorRun`, `pendingTask`, `failure` or `pendingCause`, or an issue's `heldFrom`,
// `lingerUntil` or `ancestry`. It carries the mark whatever the daemon holds, since stateFor writes
// it on every body before it narrows one (ContractView), and the strict reader before the contract
// refuses that key as surely as the nine. So this is true of every state the route puts the
// question to; it is asked beside every other carries function all the same, because stateFor puts
// the same question to every row and because a later contract that stops writing the mark on every
// body answers it here, as carriesTrees answers for `admission.trees`. The question the mark
// pre-empts is issueCarriesRecordFields, which that contract would ask of every issue here and
// which the strip below reads one issue at a time.
func carriesRecordFields(State) bool { return true }

// issueCarriesRecordFields is whether a claim view of this one issue carries any key the record
// added to a claim view. It is what the strip below reads to decide whether to build an issue's
// views again, and it asks that strip itself rather than list its keys a second time: a view the
// strip would change is a view carrying one of them, so the question and the strip cannot part
// company over what carrying means — a key added to stripRecordFields is answered here by the
// same line — as issueCarriesBackwardMoves holds that for the counts. The comparison is the
// compiler's: every field of ClaimView is a string, a number or a pointer, so the type is
// comparable and a build that puts a slice or a map on it fails here.
func issueCarriesRecordFields(issue Issue) bool {
	carries := func(v ClaimView) bool { return stripRecordFields(v) != v }
	if issue.Architect != nil && carries(*issue.Architect) {
		return true
	}
	for _, worker := range issue.Workers {
		if carries(worker.Claim) {
			return true
		}
	}
	return false
}

// withoutGatedSteps is state with no issue carrying `gatedSteps`, the field gatedStepsContract
// added to the state; nothing else of that contract is on the route. The issue map is copied, so
// the state the source answered is left as it was.
func withoutGatedSteps(state State) State {
	issues := make(map[string]Issue, len(state.Issues))
	for key, issue := range state.Issues {
		issue.GatedSteps = nil
		issues[key] = issue
	}
	state.Issues = issues
	return state
}

// carriesRetiredStranded is whether any claim of state's report of claims nothing will wake is
// retired, the state retiredStrandedContract added.
func carriesRetiredStranded(state State) bool {
	return slices.ContainsFunc(state.StrandedClaims, func(c StrandedClaim) bool {
		return c.State == string(supervise.StateRetired)
	})
}

// carriesWaitingLine is whether any issue of state carries its place in the waiting line.
func carriesWaitingLine(state State) bool {
	for _, issue := range state.Issues {
		if issue.WaitingLine != nil {
			return true
		}
	}
	return false
}

// withoutWaitingLine is state with no issue carrying `waitingLine`, the field waitingLineContract
// added to the state; nothing else of that contract is on the route. The issue map is copied, so
// the state the source answered is left as it was.
func withoutWaitingLine(state State) State {
	issues := make(map[string]Issue, len(state.Issues))
	for key, issue := range state.Issues {
		issue.WaitingLine = nil
		issues[key] = issue
	}
	state.Issues = issues
	return state
}

// carriesMergeWait is whether any issue of state carries what its review round waits on from
// GitHub. Like the step record and the place in the line it is written only where there is one —
// an issue that is not reviewing, and a reviewing issue whose round waits on nothing at GitHub,
// carries no such key — so this is asked of the state rather than answered true.
func carriesMergeWait(state State) bool {
	for _, issue := range state.Issues {
		if issue.MergeWait != nil {
			return true
		}
	}
	return false
}

// withoutMergeWait is state with no issue carrying `mergeWait`, the object mergeWaitContract
// added to the state; nothing else of that contract is on the route. The issue map is copied, so
// the state the source answered is left as it was.
func withoutMergeWait(state State) State {
	issues := make(map[string]Issue, len(state.Issues))
	for key, issue := range state.Issues {
		issue.MergeWait = nil
		issues[key] = issue
	}
	state.Issues = issues
	return state
}

// carriesBackwardMoves is whether any worker view of state carries its issue's counts of the
// moves back. Like the step record it is written only where there is one — an issue that has had
// no move back carries no such key — so this is asked of the state rather than answered true.
func carriesBackwardMoves(state State) bool {
	for _, issue := range state.Issues {
		if issueCarriesBackwardMoves(issue) {
			return true
		}
	}
	return false
}

// issueCarriesBackwardMoves is whether a worker of this one issue carries the counts. It is what
// both the question asked of the whole state and the strip below read, so the two never part
// company over what carrying means.
func issueCarriesBackwardMoves(issue Issue) bool {
	for _, worker := range issue.Workers {
		if len(worker.BackwardMoves) > 0 {
			return true
		}
	}
	return false
}

// withoutBackwardMoves is state with no worker carrying `backwardMoves`, the field
// backwardMovesContract added to the state; nothing else of that contract is on the route, the
// settings' `hooks` riding under a block the plugin reads loosely. The issue map is copied, as
// every sibling strip copies it, so the state the source answered is left as it was. A worker
// map is copied only for an issue a worker of which carries the counts: the field rides on the
// implementer of an issue that has moved back and on nobody else, and this strip runs on every
// state read of every pod below the contract, so the issues without one are left where they are
// rather than rebuilt.
func withoutBackwardMoves(state State) State {
	issues := make(map[string]Issue, len(state.Issues))
	for key, issue := range state.Issues {
		if issueCarriesBackwardMoves(issue) {
			workers := make(map[claim.Role]PhaseView, len(issue.Workers))
			for role, worker := range issue.Workers {
				worker.BackwardMoves = nil
				workers[role] = worker
			}
			issue.Workers = workers
		}
		issues[key] = issue
	}
	state.Issues = issues
	return state
}

// carriesPhaseEntries is whether any issue of state carries the two keys phaseEntriesContract
// added: `phaseEntry` and `givenEntry`. Like the step record and the counts of the moves back
// they are written only where the projection has a number to write — an issue whose current
// phase names no phase worker role carries neither — so this is asked of the state rather than
// answered true.
func carriesPhaseEntries(state State) bool {
	for _, issue := range state.Issues {
		if issue.PhaseEntry != 0 || issue.GivenEntry != 0 {
			return true
		}
	}
	return false
}

// withoutPhaseEntries is state with no issue carrying `phaseEntry` or `givenEntry`, the two
// fields phaseEntriesContract added to the state; nothing else of that contract is on the route.
// The issue map is copied, as every sibling strip copies it, so the state the source answered is
// left as it was.
func withoutPhaseEntries(state State) State {
	issues := make(map[string]Issue, len(state.Issues))
	for key, issue := range state.Issues {
		issue.PhaseEntry, issue.GivenEntry = 0, 0
		issues[key] = issue
	}
	state.Issues = issues
	return state
}

// carriesQuietTime is whether any issue of state carries its tree's quiet time, either half of
// it: the projection writes the two together, and a state carrying one alone is still a state
// the reader before the contract refuses.
func carriesQuietTime(state State) bool {
	for _, issue := range state.Issues {
		if issue.TreeMovedAt != nil || issue.QuietHours != nil {
			return true
		}
	}
	return false
}

// withoutQuietTime is state with no issue carrying `treeMovedAt` or `quietHours`, the two fields
// quietTimeContract added to the state; nothing else of that contract is on the route. The issue
// map is copied, so the state the source answered is left as it was.
func withoutQuietTime(state State) State {
	issues := make(map[string]Issue, len(state.Issues))
	for key, issue := range state.Issues {
		issue.TreeMovedAt, issue.QuietHours = nil, nil
		issues[key] = issue
	}
	state.Issues = issues
	return state
}

// podContract is the lowest plugin contract a claim whose process runs in pod records, and 0 when
// none records one: every role of a pod is served by the plugin that pod runs, and the body the
// lowest of them reads is one every higher contract reads too.
func podContract(claims []supervise.Claim, pod string) int {
	lowest := 0
	for _, c := range claims {
		if c.Locator == nil || c.Locator.Sandbox == nil || c.Locator.Sandbox.PodUID != pod || c.PluginContract == 0 {
			continue
		}
		if lowest == 0 || c.PluginContract < lowest {
			lowest = c.PluginContract
		}
	}
	return lowest
}

// withoutUpdateRounds is state with no claim view carrying `updateRound` or `updateHead`, the two
// fields supervise.PodUpdateContract added to the state; nothing else of that contract is on the
// route but `settings` keys, which every plugin reads loosely. The issue maps are copied, so the
// state the source answered is left as it was.
func withoutUpdateRounds(state State) State {
	issues := make(map[string]Issue, len(state.Issues))
	for key, issue := range state.Issues {
		if issue.Architect != nil {
			architect := *issue.Architect
			architect.UpdateRound, architect.UpdateHead = "", ""
			issue.Architect = &architect
		}
		if issue.Workers != nil {
			workers := make(map[claim.Role]PhaseView, len(issue.Workers))
			for role, worker := range issue.Workers {
				worker.Claim.UpdateRound, worker.Claim.UpdateHead = "", ""
				workers[role] = worker
			}
			issue.Workers = workers
		}
		issues[key] = issue
	}
	state.Issues = issues
	return state
}

// withoutMemory is state with no issue carrying `memoryGroup`, `memoryDrawMode` or
// `memoryDrawnAt`, the three fields memoryContract added to the state; nothing else of that
// contract is on the route but `settings` keys, which every plugin reads loosely. The issue map is
// copied, so the state the source answered is left as it was.
func withoutMemory(state State) State {
	issues := make(map[string]Issue, len(state.Issues))
	for key, issue := range state.Issues {
		issue.MemoryGroup, issue.MemoryDrawMode, issue.MemoryDrawnAt = "", "", nil
		issues[key] = issue
	}
	state.Issues = issues
	return state
}

// withoutTrees is state with no `admission.trees` and no issue carrying `tree`, the two keys
// treeContract added to the state. Nothing else of that contract is a key of its own: `active`
// and `waiting` keep their names and count issues rather than trees, which no downgrade can
// carry, and which the boot gate and `legion probe-image` keep a plugin of the contract before
// this one from reading (version.go's 51). The admission block is a value of the state's own and
// the issue map is copied, so the state the source answered is left as it was.
func withoutTrees(state State) State {
	state.Admission.Trees, state.Admission.treesHidden = nil, true
	issues := make(map[string]Issue, len(state.Issues))
	for key, issue := range state.Issues {
		issue.Tree = ""
		issues[key] = issue
	}
	state.Issues = issues
	return state
}

// withoutHeld is state with no `admission.held`, the one key heldContract added; nothing else of
// that contract is on the route. The key is left off rather than emptied, because the strict
// reader before the contract refuses it as surely when it is empty as when it names an issue,
// which is why Admission.MarshalJSON writes the key on every body but this one
// (Admission.heldHidden), as it leaves off `trees` for withoutTrees. The admission block is a
// value of the state's own, so the state the source answered is left as it was.
func withoutHeld(state State) State {
	state.Admission.Held, state.Admission.heldHidden = nil, true
	return state
}

// withoutWakesWaiting is state with no `wakesWaiting`, the top-level array wakesWaitingContract
// added; nothing else of that contract is on the route but `settings` keys, which every plugin
// reads loosely. The key is left off rather than emptied, because the strict reader before the
// contract refuses it as surely when it is empty as when it names a wake, which is why
// State.MarshalJSON writes the key on every body but this one (State.wakesWaitingHidden), as
// Admission.MarshalJSON leaves off `trees` for withoutTrees. The flag is set on a value of the
// state's own, so the state the source answered is left as it was.
func withoutWakesWaiting(state State) State {
	state.WakesWaiting, state.wakesWaitingHidden = nil, true
	return state
}

// withoutClaimsToRefresh is state with no `claimsToRefresh`, the one key claimsToRefreshContract
// added to this route; nothing else of that contract is on it, the new wake kind riding under
// `wakesWaiting[].kind`, which the schema leaves an unconstrained string. The key is left off
// rather than zeroed, because the strict reader before the contract refuses it when it is 0 as
// surely as when it names a number, which is why State.MarshalJSON writes the count on every
// body but this one (State.claimsToRefreshHidden), as withoutWakesWaiting leaves off
// `wakesWaiting`. The count is zeroed with the flag so the hidden body holds no number a later
// reader of the value could take for one it was served; the flag is set on a value of the
// state's own, so the state the source answered is left as it was. A caller below
// wakesWaitingContract is below this contract too, so both strips run on one body and it
// carries neither key.
func withoutClaimsToRefresh(state State) State {
	state.ClaimsToRefresh, state.claimsToRefreshHidden = 0, true
	return state
}

// carriesWhyAndLineFields is whether any issue of state carries either half of what
// whyAndLineFieldsContract added: its own `why`, or a member view of the waiting line holding a
// field beside `place` and `length`. Like the step record and the counts of the moves back,
// both are written only where there is one — an issue at work carries no answer, and a state
// the projection has not filled a line for carries bare places — so this is asked of the state
// rather than answered true, as carriesTrees and carriesWakesWaiting are.
func carriesWhyAndLineFields(state State) bool {
	for _, issue := range state.Issues {
		if issue.Why != nil {
			return true
		}
		if issue.WaitingLine != nil && lineFieldsCarried(*issue.WaitingLine) {
			return true
		}
	}
	return false
}

// lineFieldsCarried is whether one member's view carries a field of whyAndLineFieldsContract's
// half of the line. It is read as whatever the view is beyond the two fields waitingLineContract
// added, rather than as a list of the ten names, so a field added to WaitingLineView later is
// seen by this and emptied by withoutWhyAndLineFields without either being told of it; it is
// what the question asked of the whole state and the strip below both read, so the two never
// part company over what carrying means. The view is comparable and its two pointers compare as
// pointers, which is what is wanted: a `holdsSlot` pointing at `false` is a field the member
// carries, as much as one pointing at `true`.
func lineFieldsCarried(view WaitingLineView) bool {
	return view != WaitingLineView{Place: view.Place, Length: view.Length}
}

// withoutWhyAndLineFields is state with no issue carrying `why` and no member of the waiting
// line carrying a field beside its `place` and its `length`, the two halves
// whyAndLineFieldsContract added to the state; nothing else of that contract is on the route.
// It is unlike every strip before it in one way: withoutWaitingLine drops `waitingLine` whole
// for a caller below waitingLineContract, where this serves a caller between the two contracts
// the field that one describes — the place and the length it asked for — and takes away only
// what this contract added. That is why the member is rebuilt from those two rather than
// emptied field by field. Every field left out is omitempty and `holdsSlot` is a pointer for
// exactly this (WaitingLineView), so the rebuilt member puts no key of this contract on the
// wire rather than writing one as its zero. The issue map is copied, as every sibling strip
// copies it, and the member is a new view rather than a write through the pointer the snapshot
// holds, so the state the source answered is left as it was for the next caller.
func withoutWhyAndLineFields(state State) State {
	issues := make(map[string]Issue, len(state.Issues))
	for key, issue := range state.Issues {
		issue.Why = nil
		if line := issue.WaitingLine; line != nil && lineFieldsCarried(*line) {
			issue.WaitingLine = &WaitingLineView{Place: line.Place, Length: line.Length}
		}
		issues[key] = issue
	}
	state.Issues = issues
	return state
}

// withoutRetiredStranded is state whose report of claims nothing will wake names no retired claim,
// the one state retiredStrandedContract added; nothing else of that contract is on this route, the
// operator claim view's three delivery fields being served where no plugin reads. The entry is
// dropped rather than rewritten, since its state is the claim's own and no older value stands for
// it, and a reader before the contract refuses the whole body for one of them. The slice is
// copied, so the state the source answered is left as it was, and it stays non-nil when every
// entry goes: `strandedClaims` is never null on the wire (state.go's normalisation).
func withoutRetiredStranded(state State) State {
	stranded := make([]StrandedClaim, 0, len(state.StrandedClaims))
	for _, c := range state.StrandedClaims {
		if c.State == string(supervise.StateRetired) {
			continue
		}
		stranded = append(stranded, c)
	}
	state.StrandedClaims = stranded
	return state
}

// withoutStallRecord is state with no claim view carrying `step`, the one key stallRecordContract
// added to this route; nothing else of that contract is on it. The state the source answered is
// left as it was (mapClaimViews), as withoutUpdateRounds leaves it.
func withoutStallRecord(state State) State {
	return mapClaimViews(state, func(view ClaimView) ClaimView {
		view.Step = nil
		return view
	})
}

// carriesLeakCheckCause is whether any claim view of state carries the key
// leakCheckCauseContract added: `leakCheckCause`, the sentence a launch try was refused with
// because the shared public leak check could not be installed. Like the step record it is
// written only where there is one, so this is asked of the state rather than answered true.
func carriesLeakCheckCause(state State) bool {
	return anyClaimView(state, func(view ClaimView) bool { return view.LeakCheckCause != "" })
}

// withoutLeakCheckCause is state with no claim view carrying `leakCheckCause`, the one key
// leakCheckCauseContract added to this route; nothing else of that contract is on it. The state
// the source answered is left as it was (mapClaimViews), as withoutStallRecord leaves it.
func withoutLeakCheckCause(state State) State {
	return mapClaimViews(state, func(view ClaimView) ClaimView {
		view.LeakCheckCause = ""
		return view
	})
}

// anyClaimView is whether test holds of any claim view of state: an issue's architect, or the
// claim of any of its workers.
func anyClaimView(state State, test func(ClaimView) bool) bool {
	for _, issue := range state.Issues {
		if issue.Architect != nil && test(*issue.Architect) {
			return true
		}
		for _, worker := range issue.Workers {
			if test(worker.Claim) {
				return true
			}
		}
	}
	return false
}

// mapClaimViews is state with every claim view — each issue's architect and the claim of each of
// its workers — replaced by what change makes of it. The issues map is rebuilt, and each
// architect and Workers map copied before change sees it, so the state the source answered is
// left as it was.
func mapClaimViews(state State, change func(ClaimView) ClaimView) State {
	issues := make(map[string]Issue, len(state.Issues))
	for key, issue := range state.Issues {
		if issue.Architect != nil {
			architect := change(*issue.Architect)
			issue.Architect = &architect
		}
		if issue.Workers != nil {
			workers := make(map[claim.Role]PhaseView, len(issue.Workers))
			for role, worker := range issue.Workers {
				worker.Claim = change(worker.Claim)
				workers[role] = worker
			}
			issue.Workers = workers
		}
		issues[key] = issue
	}
	state.Issues = issues
	return state
}

// stripRecordFields is view with none of the keys recordFieldsContract added to a claim view:
// `servingGeneration`, `updateReasons`, `providerErrorRun`, `pendingTask`, `failure` and
// `pendingCause`. It stands on its own because two callers must agree on that list to the key —
// the narrowing below, which takes them off the views of a caller under the contract, and
// issueCarriesRecordFields, which asks whether a view carries any of them and asks it by this
// function — so a key added here is answered by both and cannot be added to one alone.
func stripRecordFields(view ClaimView) ClaimView {
	view.ServingGeneration = 0
	view.UpdateReasons = ""
	view.ProviderErrorRun = 0
	view.PendingTask = nil
	view.Failure = nil
	view.PendingCause = ""
	return view
}

// withoutRecordFields is state with no `contract` mark, no claim view carrying any of the keys
// stripRecordFields takes off, and no issue carrying `heldFrom`, `lingerUntil` or `ancestry`,
// which is everything recordFieldsContract added to this route (that contract's row of
// stateNarrowings names them); nothing else of that contract is on it. The record's keys are
// stripped in this one copy of the issue map, because they arrive and leave together and a pass
// for each would copy that map once for each. The maps are copied, so the state the source
// answered is left as it was, as withoutStallRecord leaves it. An issue's own three are cleared
// on the copy the map rebuild makes anyway; its architect and its worker map are built again
// only where a claim view of that issue carries one of the strip's keys, as
// withoutBackwardMoves rebuilds only the worker maps that carry the counts, because this strip
// runs on every state read of every pod below the contract.
//
// The mark goes with them, and must: it names the two contracts, and a key this caller's plugin
// does not know refuses the whole body at it, which is the one thing the narrowing is for. What
// such a caller is served instead is nothing — the daemon records the narrowing in its own log
// (stateFor), where no plugin's schema can refuse it.
func withoutRecordFields(state State) State {
	state.Contract = nil
	issues := make(map[string]Issue, len(state.Issues))
	for key, issue := range state.Issues {
		issue.HeldFrom = ""
		issue.LingerUntil = nil
		issue.Ancestry = nil
		if issueCarriesRecordFields(issue) {
			if issue.Architect != nil {
				architect := stripRecordFields(*issue.Architect)
				issue.Architect = &architect
			}
			if issue.Workers != nil {
				workers := make(map[claim.Role]PhaseView, len(issue.Workers))
				for role, worker := range issue.Workers {
					worker.Claim = stripRecordFields(worker.Claim)
					workers[role] = worker
				}
				issue.Workers = workers
			}
		}
		issues[key] = issue
	}
	state.Issues = issues
	return state
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		slog.Error("api: encode response", "error", err)
		http.Error(w, `{"error":"encode failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(append(encoded, '\n')); err != nil {
		slog.Debug("api: write response", "error", err)
	}
}
