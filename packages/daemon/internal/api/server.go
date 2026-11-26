package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
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

// Options are what the routes answer from.
type Options struct {
	// State answers GET /legion/v1/state.
	State StateSource
	// StateTransactions opens the repeatable-read, read-only snapshot the state projection uses:
	// the daemon's store pool, which the daemon's serve sets. A test sets a fake that opens no
	// database transaction. Unset, the state route answers a failure.
	StateTransactions store.TxPool
	// StateReadBound is how long the state route's read may take before it answers a failure and
	// gives its connection back; zero is stateReadBound, which is the value the daemon runs. A
	// test sets a smaller one so it need not wait out a minute.
	StateReadBound time.Duration
	// ConsumerLag reads the lag of the daemon's durable consumers from NATS (the daemon's
	// intake.Consumers.Lag), which the state route reads after its transaction commits and serves as
	// `consumers`. Nil, a daemon with no workflow and so no consumers, serves none.
	ConsumerLag func(ctx context.Context) []intake.ConsumerLag
	// Intake is the replay and the discard of the project's give-ups (intake.GiveUps), which the
	// four give-up routes run. Nil, a daemon with no workflow and so no intake, answers those
	// routes 503.
	Intake GiveUps
	// Supervisor is the claims the claim and operator routes post their requests to.
	Supervisor Supervisor
	// AwaitStream waits, bounded, until the claim's machine has handled every worker-stream event
	// the daemon had read when it was called (the daemon's awaitStream). apply calls it before a
	// fact of an agent's own request where the record names that agent the agent of a run the fact
	// may end, so the record of that step names the model whose answer the stream read ahead of
	// the request. Nil waits for nothing.
	AwaitStream func(ctx context.Context, token claim.Token)
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
	// Tokens mints the GitHub App leases credential routes return after redeeming a grant, and
	// answers the owners each App is installed on, which the controller's gh bundle holds one
	// directory per owner of (appauth.Source, appauth.ControllerCredential).
	Tokens appauth.Source
	// Served is whether both role Apps answered installed on a repository (daemon's
	// repositories.servesRepository), the one question the rename rule asks of a repository: the
	// controller's gh bundle holds a directory for each owner a configured rename moved a served
	// repository away from (appauth.ControllerCredential). Nil serves no repository, which leaves
	// every configured rename unresolved.
	Served func(ghrepo.Repository) bool
	// RepositoryRenames is the deployment's repository equivalence
	// (config.Config.RepositoryRenames): the controller's gh bundle holds one entry for a
	// configured rename's two owners (appauth.ControllerCredential). The zero value configures none.
	RepositoryRenames ghrepo.Renames
	// Serving is the repository GitHub holds a recorded one under now, which every GitHub call
	// this server makes is minted for and addressed to (serving): the daemon's one rule
	// (daemon's repositories.serving, which the Sandbox runtime's own token source is handed
	// too), so the installation answers and the configured renames are read in one place and no
	// caller here writes a second copy of it. Nil serves a recorded repository under its own name,
	// which a test that configures no rename and a daemon with no GitHub Apps both want.
	Serving func(recorded ghrepo.Repository) ghrepo.Repository
	// Releaser releases what the runtime holds for a tree whose operator close reserved and
	// finished its cleanup: the daemon's runtime.
	Releaser store.TreeReleaser
	// Trees is the store's durable tree barrier, which the operator routes open a root's tree
	// through and reserve and finish a closed tree's cleanup through.
	Trees TreeLifecycles
	// Experiments are the project's declared experiments (config.Project.ExperimentsDeclared),
	// of which the operator's spawn draws every tree-unit one for a tree it opens for the first
	// time (drawExperiments). Every project the loader reads declares the memory experiment, at
	// mode off where the file writes no `memory` block, and a draw at mode off records the tree
	// at no variant; the empty list, which draws nothing, is a caller that declared none.
	// ExperimentSource is the random source a `random` mode is drawn with; nil is
	// experiment.CryptoSource, and a test pins it. Now is the clock a draw's instant is read
	// from; nil is time.Now, and a test pins it.
	Experiments      []config.Experiment
	ExperimentSource func(uint64) (uint64, error)
	Now              func() time.Time
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
	// RepositoryUnread says whether a repository is left out for now because GitHub did not answer
	// its read of it, its GitHub App installation read or its default-branch read past GitHub's
	// rate limit (the daemon's repositoryUnread): the daemon recovers the issues on
	// it from Dispatch's listing once GitHub answers. release_children never ends a wave on a todo
	// child it leaves unrecorded for one: it answers the child released and goes on, telling the
	// architect at once when the recovery will not record it (enterTodoChild). Nil leaves none out.
	RepositoryUnread func(resolved string) bool
	// AwaitRecovery leaves a child release_children left unrecorded on an unread repository to that
	// repository's recovery (the daemon's awaitRecovery), which tells the architect of each such
	// child it did not record — the one that owns the child's parent when the record holds that
	// parent in tree, else the tree's — and answers whether it did. False means the daemon no longer
	// holds that repository unread — its recovery stopped waiting on GitHub for it between the
	// route's RepositoryUnread read and this call — so no recovery will come for the child and none
	// will tell its architect: the route must then schedule the child-release retry
	// (scheduleChildReleaseRetry), exactly as for a child on no unread repository, rather than
	// answer it released and wait. Nil leaves none, and holds every child released as before.
	AwaitRecovery func(repo, tree, child, parent string) bool
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
	// RoleHoldersChanged is told, before the claim takes a ready that carries no capability report
	// — a boot's ready, or a role-regain's — of the claim as it stands, when it has an issue and is
	// not the controller: for an issue of the daemon's own project, the daemon queues the
	// publication of that issue's role sessions to Dispatch, which reads the live set from the
	// store when it runs. The ready is what sends the claim its first task, so the publication is
	// queued before any task goes. An error refuses the ready with 503: the machine has not taken
	// it, no task goes, and the plugin tries the ready again. Nil queues nothing.
	RoleHoldersChanged func(ctx context.Context, c supervise.Claim) error
	// ReleaseRecords is the store's record of what the idle release has done (store.Store), which
	// the claim route `POST /legion/v1/claims/release-records` serves a pod. Nil answers that route
	// 500 and changes nothing else.
	ReleaseRecords ReleaseRecordStore
	// ClaimActivity records what a claim's session reports it is doing (store.Store), which the
	// claim route `POST /legion/v1/claims/activity` writes. Nil answers that route 500 and
	// changes nothing else.
	ClaimActivity ClaimActivityStore
	// Pause is the project's pause service (internal/pause's Service), which the four pause and
	// resume routes act through and whose flag the claim routes read to refuse a turn while the
	// project is paused (refusedWhilePaused). Nil is a daemon with no workflow, which has no pause:
	// it answers every pause and resume PAUSE_UNAVAILABLE and refuses no turn for one.
	Pause PauseService
}

// TreeLifecycles is the store's durable tree barrier (store.Store) as the operator routes use it:
// an operator root opens its tree's lifecycle before its claim is stored, and the tree's close
// reserves the tree's cleanup, then finishes it once every claim of the tree has retired. The
// open's created runs in the open's own transaction where it created the tree's row, and is where
// the tree's experiments are drawn (store.Store.OpenOperatorTreeLifecycle).
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
	stateTransactions store.TxPool
	// stateBound is Options.StateReadBound, resolved: stateReadBound where the caller set none.
	stateBound  time.Duration
	consumerLag func(ctx context.Context) []intake.ConsumerLag
	giveUps     GiveUps
	callerPod   func(addr netip.Addr) (string, bool)
	supervisor  Supervisor
	// awaitStream is Options.AwaitStream (apply).
	awaitStream func(ctx context.Context, token claim.Token)
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
	controllerMu sync.Mutex
	tokens       appauth.Source
	// served is Options.Served, never nil here: NewServer serves no repository where the caller
	// hands none.
	served func(ghrepo.Repository) bool
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
	// experiments, source and now are Options.Experiments, Options.ExperimentSource and
	// Options.Now: what the spawn's own draw for a tree it opens for the first time reads
	// (drawExperiments).
	experiments []config.Experiment
	source      func(uint64) (uint64, error)
	now         func() time.Time
	pool        *store.Pool
	handlers    []intake.Handler
	prepare     func(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error)
	unread      func(resolved string) bool
	await       func(repo, tree, child, parent string) bool
	roleFor     func(phase.Phase) claim.Role
	records     record.Store
	dispatch    dispatch.Client
	claimReady  func(c supervise.Claim)
	// handoffCompleted is Options.HandoffCompleted.
	handoffCompleted func(issue string)
	// promptAgents and capabilityReported are Options.PromptAgents and Options.CapabilityReported,
	// releaseRecordStore Options.ReleaseRecords and claimActivity Options.ClaimActivity.
	promptAgents       []string
	capabilityReported func(ctx context.Context, c supervise.Claim, report capabilities.Report)
	// roleHoldersChanged is Options.RoleHoldersChanged.
	roleHoldersChanged func(ctx context.Context, c supervise.Claim) error
	releaseRecordStore ReleaseRecordStore
	claimActivity      ClaimActivityStore
	// pause is Options.Pause, nil for a daemon with no workflow.
	pause PauseService
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
	served := opts.Served
	if served == nil {
		served = func(ghrepo.Repository) bool { return false }
	}
	s := &server{
		state:              opts.State,
		stateTransactions:  opts.StateTransactions,
		stateBound:         opts.StateReadBound,
		consumerLag:        opts.ConsumerLag,
		giveUps:            opts.Intake,
		callerPod:          opts.CallerPod,
		supervisor:         opts.Supervisor,
		awaitStream:        opts.AwaitStream,
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
		served:             served,
		renames:            opts.RepositoryRenames,
		serving:            serving,
		githubAPI:          opts.GitHubAPI,
		releaser:           opts.Releaser,
		trees:              opts.Trees,
		experiments:        opts.Experiments,
		source:             opts.ExperimentSource,
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
		roleHoldersChanged: opts.RoleHoldersChanged,
		releaseRecordStore: opts.ReleaseRecords,
		claimActivity:      opts.ClaimActivity,
		pause:              opts.Pause,
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
	if s.source == nil {
		s.source = experiment.CryptoSource
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
	for _, route := range s.routes() {
		mux.HandleFunc(route.pattern, route.handler)
	}
	return &http.Server{
		Addr:              net.JoinHostPort(bind, strconv.Itoa(port)),
		Handler:           statingContract{handler: trackTransactions(noteRequesters(mux))},
		ReadHeaderTimeout: readHeaderTimeout,
	}
}

// daemonContractHeader is the response header this daemon states its own contract in
// (DaemonAPIVersion), so a caller reads the number the body it just received was written at
// rather than inferring it from the body's keys.
const daemonContractHeader = "Legion-Daemon-Contract"

// statingContract is the one handler every request of this daemon passes through: it states the
// daemon's contract and hands the request on to handler, the handler NewServer wraps. It is a
// handler around the whole mux and not a line in a writer, because the header rides on every
// answer — the catch-all's 404 and `GET /healthz` as much as a `/legion/v1` route, a refusal as
// much as a success, and the four routes that answer 204 with no body at all, which write no body
// for a writer to add it to.
//
// It is a type of its own rather than a closure so that a test can read the server's Handler and
// see that the mux is wrapped, which is what makes a route that misses the header impossible.
type statingContract struct {
	handler http.Handler
}

func (s statingContract) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(daemonContractHeader, strconv.Itoa(DaemonAPIVersion()))
	s.handler.ServeHTTP(w, r)
}

// route is one route the API serves: the pattern http.ServeMux matches a request against, and the
// handler that answers it.
type route struct {
	pattern string
	handler http.HandlerFunc
}

// routes is every route the API serves, the one table NewServer registers.
func (s *server) routes() []route {
	return []route{
		{"GET /legion/v1/state", s.stateRoute},
		{"GET /legion/v1/issues/{key}/record", s.issueRecordRoute},
		{"GET /healthz", s.healthRoute},
		{"GET /readyz", s.readyz},

		{"POST /legion/v1/claims/register", s.register},
		{"POST /legion/v1/claims/ready", s.ready},
		{"POST /legion/v1/claims/ready-for-update", s.readyForUpdate},
		{"POST /legion/v1/claims/exit", s.exit},
		{"POST /legion/v1/claims/turn-outcome", s.turnOutcome},
		{"POST /legion/v1/claims/activity", s.activity},
		{"POST /legion/v1/claims/release-records", s.releaseRecords},
		{"POST /legion/v1/grants", s.grant},
		{"POST /legion/v1/controller/secret", s.controllerSecret},
		{"POST /legion/v1/controller/github-credential", s.controllerGitHubCredential},
		{"POST /legion/v1/handoff/complete", s.handoffComplete},
		{"POST /legion/v1/issues/status", s.issueStatus},
		{"POST /legion/v1/claims/fresh-session", s.claimFreshSession},
		{"POST /legion/v1/claims/compact", s.claimCompact},
		{"POST /legion/v1/gates/register", s.gateRegister},
		{"POST /legion/v1/plans/register", s.registerPlan},
		{"POST /legion/v1/tasks/stop", s.stopTask},
		{"POST /legion/v1/waves/release", s.waveRelease},
		{"POST /legion/v1/phase/backward", s.phaseBackward},
		{"POST /legion/v1/phase/retry", s.phaseRetry},
		{"POST /legion/v1/signoff", s.signOff},
		{"POST /legion/v1/roots/close", s.closeRoot},
		{"POST /legion/v1/children/park", s.removedChildRoute},
		{"POST /legion/v1/children/rerun", s.rerunChild},
		{"POST /legion/v1/intake/replay", s.intakeReplay},
		{"POST /legion/v1/intake/discard", s.intakeDiscard},
		{"POST /legion/v1/project/pause", s.projectPause},
		{"POST /legion/v1/project/resume", s.projectResume},

		{"POST /legion/v1/operator/claims", s.operator(s.spawn)},
		{"GET /legion/v1/operator/claims", s.operator(s.list)},
		{"POST /legion/v1/operator/claims/{token}/deliver", s.operator(s.claimRequest("deliver", s.deliverEvent))},
		{"POST /legion/v1/operator/claims/{token}/suspend", s.operator(s.claimRequest("suspend", suspendEvent))},
		{"POST /legion/v1/operator/claims/{token}/resume", s.operator(s.claimRequest("resume", s.resumeEvent))},
		{"POST /legion/v1/operator/claims/{token}/stop", s.operator(s.claimRequest("stop", stopEvent))},
		{"POST /legion/v1/operator/claims/{token}/retry", s.operator(s.retryClaim)},
		{"POST /legion/v1/operator/claims/{token}/compact", s.operator(s.compactClaim)},
		{"POST /legion/v1/operator/claims/{token}/forget-session", s.operator(s.forgetSession)},
		{"POST /legion/v1/operator/claims/{token}/release-names", s.operator(s.releaseNames)},
		{"POST /legion/v1/operator/claims/{token}/close", s.operator(s.closeTree)},
		{"POST /legion/v1/operator/intake/replay", s.operator(s.operatorIntakeReplay)},
		{"POST /legion/v1/operator/intake/discard", s.operator(s.operatorIntakeDiscard)},
		{"POST /legion/v1/operator/project/pause", s.operator(s.operatorProjectPause)},
		{"POST /legion/v1/operator/project/resume", s.operator(s.operatorProjectResume)},

		// The catch-all answers every unrouted request, including a known path asked for with the
		// wrong method: "no route" is the whole story the plugin, the operator's CLI, or a proof
		// script needs.
		{"/", s.noRoute},
	}
}

// noRoute is the catch-all's answer, the daemon's refusal shape like every other refusal.
func (s *server) noRoute(w http.ResponseWriter, r *http.Request) {
	s.refuse(w, r, http.StatusNotFound, "no route")
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

// refusedWhilePaused refuses request of c, answering PROJECT_PAUSED, while the project is paused
// and c is not the controller's claim (decision 20): a turn a paused project's role would start —
// a compaction or a fresh session the controller asks for, the operator's resume, retry,
// compaction, spawn or delivery — waits for the resume, and nothing of the claim changes. The
// pause flag decides (pause.Service.Paused), as it decides every launch the machines hold; the
// refusal names the standing pause when the store's read finds it. A stop, a suspension, an
// issue's status and the daemon's own status are not turns, and no caller of this asks of them.
// A claim of another project's issue, which the operator's spawn makes under the daemon's own
// project token, is refused as every other claim is, its spawn included: the daemon runs it.
func (s *server) refusedWhilePaused(w http.ResponseWriter, r *http.Request, request string, c supervise.Claim) bool {
	if s.pause == nil || claim.IsController(c.Role, c.Issue, c.Tree) {
		return false
	}
	id, paused := s.pause.Paused()
	if !paused {
		return false
	}
	body := PauseFailure{Failure: Failure{Code: projectPaused}}
	named := fmt.Sprintf("pause %d", id)
	standing, found, err := s.pause.OpenPause(r.Context())
	switch {
	case err != nil:
		s.logRequestFailure(r, err, "api: read the open pause for a refusal", "pause", id)
	case found:
		view := pauseViewOf(standing)
		body.Pause, named = &view, describePause(standing)
	}
	body.Error = fmt.Sprintf("%s of %s refused: the project is paused (%s), and a paused project starts no turn of a role but the controller's; nothing of %s changed. Resume the project first",
		request, c.Token, named, c.Token)
	s.log.Info("api: refused a request: the project is paused", "request", request, "claim", c.Token, "pause", id)
	s.writeAnswer(w, r, AnswerRefusal, http.StatusConflict, body)
	return true
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
// already — writeAnswerFrom hands the body to json.Marshal, which materialises all of it before
// a byte is written — so the handler would hold a second full copy of a body that measured
// 4,055,424 bytes on the production deployment on 2026-11-12. One such copy per concurrent read
// is a second resource failure inside the first one this bounds.
func (s *server) projectState(w http.ResponseWriter, r *http.Request) (State, []supervise.Claim, bool) {
	if s.stateTransactions == nil {
		s.log.Error("api: state transaction source is unset")
		s.refuse(w, r, http.StatusInternalServerError, "state unavailable")
		return State{}, nil, false
	}
	reading, cancel := context.WithTimeout(r.Context(), s.stateBound)
	defer cancel()
	var state State
	var claims []supervise.Claim
	// The step that failed names the log line, as each did before the read ran through
	// store.TransactWith: the begin, until the body runs; the statement bound; the state source;
	// and the commit, for a failure after a body that succeeded.
	failed := "api: begin state transaction failed"
	err := store.TransactWith(reading, s.stateTransactions, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	}, func(ctx context.Context, tx pgx.Tx) error {
		failed = "api: commit state transaction failed"
		if _, err := tx.Exec(ctx, "set local statement_timeout = "+strconv.FormatInt(statementBound(s.stateBound), 10)); err != nil {
			failed = "api: bound the state transaction's statements failed"
			return err
		}
		var err error
		state, claims, err = s.state.State(ctx, tx)
		if err != nil {
			failed = "api: state source failed"
		}
		return err
	})
	if err != nil {
		s.logRequestFailure(r, err, failed)
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
// production checks, `gatedSteps` from the tree's steps (record.TreeSteps: the deployment's, and a
// criteria row its root registered beside them) and the three memory fields from the tree's draw.
// None of the six is a fact of the issue's own row. Building the answer from the same value the
// whole-project body is built from makes that equality hold by construction, and a test holds a
// later change to it.
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
