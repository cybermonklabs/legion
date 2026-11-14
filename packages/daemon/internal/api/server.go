package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
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
	// plugin asked (contractsFor). Nil is a runtime that runs no pods, whose every caller is served
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
	mux.HandleFunc("GET /legion/v1/issues/{key}/record", s.issueRecordRoute)
	mux.HandleFunc("GET /healthz", s.healthRoute)
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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.refuse(w, r, http.StatusNotFound, "no route")
	})

	return &http.Server{
		Addr:              net.JoinHostPort(bind, strconv.Itoa(port)),
		Handler:           statingContract{handler: trackTransactions(mux)},
		ReadHeaderTimeout: readHeaderTimeout,
	}
}

// daemonContractHeader is the response header this daemon states its own contract in
// (DaemonAPIVersion), so a caller reads the number the body it just received was written at
// rather than inferring it from the body's keys.
const daemonContractHeader = "Legion-Daemon-Contract"

// statingContract is the one handler every request of this daemon passes through: it states the
// daemon's contract and hands the request on to handler, which is the mux with every request's
// context marked for the pool (trackTransactions). It is a handler around the whole mux and
// not a line in a writer, because the header rides on every answer — the catch-all's 404 and
// `GET /healthz` as much as a `/legion/v1` route, a refusal as much as a success, and the four
// routes that answer 204 with no body at all, which write no body for a writer to add it to.
//
// It is a type of its own rather than a closure so that a test can read the server's Handler and
// see that the mux is wrapped, which is what makes a route that misses the header impossible.
type statingContract struct {
	handler http.Handler
}

func (s statingContract) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(daemonContractHeader, strconv.Itoa(DaemonAPIVersion))
	s.handler.ServeHTTP(w, r)
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
// connection of the pool is busy. No plugin reads its body, so the body is written as one no
// contract shapes (writeUnparsedAnswer), which reads no claims either.
//
// It is a readiness probe and never a liveness probe. The daemon's Kubernetes Service carries
// this API's port and the worker stream's port under one selector, so a probe that failed while
// the daemon was busy would take the worker stream out of the Service while every pod's launcher
// was dialling it, and the deployment runs one replica under the Recreate strategy, so no second
// pod answers in its place.
func (s *server) readyz(w http.ResponseWriter, r *http.Request) {
	switch {
	case s.stopping.Err() != nil:
		s.writeUnparsedAnswer(w, r, http.StatusServiceUnavailable, map[string]string{"status": "stopping"})
	case s.readiness != nil && !s.readiness():
		s.writeUnparsedAnswer(w, r, http.StatusServiceUnavailable,
			map[string]string{"status": "starting", "waitingFor": "the daemon to serve"})
	default:
		s.writeUnparsedAnswer(w, r, http.StatusOK, map[string]string{"status": "ok"})
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
func (s *server) stopped(w http.ResponseWriter, r *http.Request, request string) bool {
	refusal := s.stopRefusal(request)
	if refusal == "" {
		return false
	}
	s.refuse(w, r, http.StatusServiceUnavailable, refusal)
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

// projectState is the project's own value, as every route that reads it reads it: one
// repeatable-read read-only snapshot, one read of the source, and the consumers' lag read after
// that snapshot commits. It writes its own refusal and answers false where it wrote one, so the
// two reading routes share the transaction, the read and the refusals and differ only in what
// each answers from the value.
//
// The claims are the snapshot's own, which is what a caller's contract is read from
// (writeAnswerFrom): a claim and the state it was projected into cannot come from two moments.
//
// The read runs under a bound of the daemon's own (stateReadBound, Options.StateReadBound).
// Before INFRA-3169 the only thing that ended this read was the client giving up, so a read that
// never finished held its connection as long as the client held its socket, and 32 of those are
// the whole pool.
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
// response into a buffer of its own before writing it, and the state's body is buffered once
// already — writeJSON hands the body to json.Marshal, which materialises all of it before a byte
// is written — so the handler would hold a second full copy of a body that measured 4,055,424
// bytes on the production deployment on 2026-11-12. One such copy per concurrent read is a
// second resource failure inside the first one this bounds.
func (s *server) projectState(w http.ResponseWriter, r *http.Request) (State, []supervise.Claim, bool) {
	if s.stateTransactions == nil {
		s.log.Error("api: state transaction source is unset")
		s.refuse(w, r, http.StatusInternalServerError, "state unavailable")
		return State{}, nil, false
	}
	reading, cancel := context.WithTimeout(r.Context(), s.stateBound)
	defer cancel()
	tx, err := s.stateTransactions.BeginTx(reading, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		s.logRequestFailure(r, err, "api: begin state transaction failed")
		s.refuse(w, r, http.StatusInternalServerError, "state unavailable")
		return State{}, nil, false
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
		s.refuse(w, r, http.StatusInternalServerError, "state unavailable")
		return State{}, nil, false
	}
	state, claims, err := s.state.State(reading, tx)
	if err != nil {
		s.logRequestFailure(r, err, "api: state source failed")
		s.refuse(w, r, http.StatusInternalServerError, "state unavailable")
		return State{}, nil, false
	}
	if err := tx.Commit(reading); err != nil {
		s.logRequestFailure(r, err, "api: commit state transaction failed")
		s.refuse(w, r, http.StatusInternalServerError, "state unavailable")
		return State{}, nil, false
	}
	// The consumers' lag is read from NATS after the commit, so a slow or silent NATS server holds
	// no snapshot open; what it does not answer is each consumer's `unavailable`, never a refusal.
	if s.consumerLag != nil {
		state.Consumers = ConsumerLagViewsOf(s.consumerLag(reading))
	}
	// The fleet's lowest plugin contract is read from the claims of the very snapshot the state
	// was projected from, so the number the body reports and the shapes the body was served at
	// are read from one moment (State.LowestPluginContract).
	state.LowestPluginContract = lowestPluginContract(claims)
	return state, claims, true
}

func (s *server) stateRoute(w http.ResponseWriter, r *http.Request) {
	state, claims, ok := s.projectState(w, r)
	if !ok {
		return
	}
	s.writeAnswerFrom(w, r, AnswerState, http.StatusOK, state, claims)
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

// recordNotFound is the code of the one refusal this route makes, and no other refusal of this
// daemon uses it: a caller that reads one issue's record tells "this daemon holds no record for
// the issue you named" from every other 404 by the code alone (`ISSUE_NOT_FOUND` is the
// workflow routes', api's handoff.go and architect.go).
const recordNotFound = "RECORD_NOT_FOUND"

// issueRecordRoute answers one issue's record, the value the whole-project body holds under
// `issues.<key>`, for any issue the caller names and not the caller's own alone. It
// authenticates nobody and places its caller by its address, as the state route does.
//
// It builds the project's state exactly as the state route does (projectState) and answers
// `state.Issues[key]` out of it, rather than asking the store a cheaper question of one issue,
// because the record must equal the record of that issue inside the whole-project body field for
// field: `waitingLine` and `slot` come from the project's admission, `check` from its open
// production checks, `gatedSteps` from the deployment's steps and the three memory fields from
// the tree's draw. None of the six is a fact of the issue's own row. Building the answer from
// the same value the whole-project body is built from makes that equality hold by construction,
// and a test holds a later change to it.
//
// An issue the daemon holds no record for is a 404 carrying recordNotFound and the sentence the
// plugin's own `read_record` answers for an unknown issue today
// (`packages/pi-shared/src/legion-phase-ops.ts`), so a role reads the same words whichever side
// found the record missing.
func (s *server) issueRecordRoute(w http.ResponseWriter, r *http.Request) {
	state, claims, ok := s.projectState(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	issue, recorded := state.Issues[key]
	if !recorded {
		s.writeFailure(w, r, http.StatusNotFound, recordNotFound, "The daemon has no record for "+key)
		return
	}
	s.writeAnswerFrom(w, r, AnswerIssueRecord, http.StatusOK, issue, claims)
}

// Health is the body of `GET /healthz`: that the daemon answered, and the project it serves, so
// a caller that reached a daemon learns which project it reached rather than only that something
// is listening. The controller's boot check reads the project here, where it read
// `state.daemon.project` and paid for the whole state to learn one word.
type Health struct {
	Status  string `json:"status"`
	Project string `json:"project"`
}

// healthRoute answers that this daemon is serving, and the project it serves. It reads no store,
// so a daemon whose database is unreachable still answers it.
func (s *server) healthRoute(w http.ResponseWriter, r *http.Request) {
	s.writeAnswer(w, r, AnswerHealth, http.StatusOK, Health{Status: "ok", Project: s.project})
}
