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
	"github.com/jackc/pgx/v5/pgxpool"

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

// Options are what the routes answer from.
type Options struct {
	// State answers GET /legion/v1/state.
	State StateSource
	// StateTransactions opens the repeatable-read, read-only snapshot the state projection uses.
	StateTransactions StateTransactions
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
	// GitHubAPI is GitHub's REST root, which the handoff route reads an issue branch's head, its
	// handoff file and READY's checks under; empty, in production, is https://api.github.com, and a
	// test points it at a stand-in.
	GitHubAPI string
	// MergeRules is the merge policy an issue's project configures. Nil has no special policy.
	MergeRules func(project string) config.MergeRules
	// Grants mints and redeems the daemon-local one-command credential handles.
	Grants   *credential.Grants
	Pool     *pgxpool.Pool
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
	// RoleFor is the role that works a phase (the daemon's workflow.RoleFor, handed in because the
	// workflow's tests import this package). The operator's retry of a failed phase worker applies
	// its issue's held phase only when that phase's role is the claim's (retryClaim); nil retries no
	// held phase, leaving every one to its architect.
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
// so the route reaches it here rather than through the daemon's pool. It answers whether the
// store holds the claim at all; a report older than the one recorded is dropped and still
// answers true.
type ClaimActivityStore interface {
	PutClaimActivity(ctx context.Context, token claim.Token, activity claim.SessionActivity) (bool, error)
}

type server struct {
	state             StateSource
	stateTransactions StateTransactions
	consumerLag       func(ctx context.Context) []intake.ConsumerLag
	callerPod         func(addr netip.Addr) (string, bool)
	supervisor        Supervisor
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
	serving    func(recorded ghrepo.Repository) ghrepo.Repository
	githubAPI  string
	mergeRules func(project string) config.MergeRules
	grants     *credential.Grants
	releaser   store.TreeReleaser
	trees      TreeLifecycles
	// memory, coin and now are Options.Memory, Options.MemoryCoin and Options.Now: what the
	// spawn's own draw of a tree it opens for the first time reads (drawMemory).
	memory     config.Memory
	coin       func() (bool, error)
	now        func() time.Time
	pool       *pgxpool.Pool
	handlers   []intake.Handler
	prepare    func(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error)
	unread     func(resolved string) bool
	await      func(repo, tree, child, parent string)
	roleFor    func(phase.Phase) claim.Role
	records    record.Store
	dispatch   dispatch.Client
	claimReady func(c supervise.Claim)
	// promptAgents and capabilityReported are Options.PromptAgents and Options.CapabilityReported,
	// releaseRecordStore Options.ReleaseRecords and claimActivity Options.ClaimActivity.
	promptAgents       []string
	capabilityReported func(ctx context.Context, c supervise.Claim, report capabilities.Report)
	releaseRecordStore ReleaseRecordStore
	claimActivity      ClaimActivityStore
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
		mergeRules:         opts.MergeRules,
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
		promptAgents:       opts.PromptAgents,
		capabilityReported: opts.CapabilityReported,
		releaseRecordStore: opts.ReleaseRecords,
		claimActivity:      opts.ClaimActivity,
		log:                opts.Log,
		stopping:           opts.Stopping,
		drained:            opts.Drained,
		decisions:          opts.Decisions,
	}
	if opts.OperatorToken != "" {
		s.operatorSet, s.operatorHash = true, sha256.Sum256([]byte(opts.OperatorToken))
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

	mux.HandleFunc("POST /legion/v1/claims/register", s.register)
	mux.HandleFunc("POST /legion/v1/claims/ready", s.ready)
	mux.HandleFunc("POST /legion/v1/claims/ready-for-update", s.readyForUpdate)
	mux.HandleFunc("POST /legion/v1/claims/exit", s.exit)
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
		s.writeAnswer(w, r, AnswerRefusal, http.StatusNotFound, errorBody("no route"))
	})

	return &http.Server{
		Addr:              net.JoinHostPort(bind, strconv.Itoa(port)),
		Handler:           statingContract{mux: mux},
		ReadHeaderTimeout: readHeaderTimeout,
	}
}

// daemonContractHeader is the response header this daemon states its own contract in
// (DaemonAPIVersion), so a caller reads the number the body it just received was written at
// rather than inferring it from the body's keys.
const daemonContractHeader = "Legion-Daemon-Contract"

// statingContract is the one handler every request of this daemon passes through: it states the
// daemon's contract and hands the request to the mux. It is a handler around the whole mux and
// not a line in a writer, because the header rides on every answer — the catch-all's 404 and
// `GET /healthz` as much as a `/legion/v1` route, a refusal as much as a success, and the four
// routes that answer 204 with no body at all, which write no body for a writer to add it to.
//
// It is a type of its own rather than a closure so that a test can read the server's Handler and
// see that the mux is wrapped, which is what makes a route that misses the header impossible.
type statingContract struct {
	mux *http.ServeMux
}

func (s statingContract) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(daemonContractHeader, strconv.Itoa(DaemonAPIVersion))
	s.mux.ServeHTTP(w, r)
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
	s.writeAnswer(w, r, AnswerRefusal, http.StatusServiceUnavailable, errorBody(refusal))
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
func (s *server) projectState(w http.ResponseWriter, r *http.Request) (State, []supervise.Claim, bool) {
	if s.stateTransactions == nil {
		s.log.Error("api: state transaction source is unset")
		s.writeAnswer(w, r, AnswerRefusal, http.StatusInternalServerError, errorBody("state unavailable"))
		return State{}, nil, false
	}
	tx, err := s.stateTransactions.BeginTx(r.Context(), pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		s.logRequestFailure(r, err, "api: begin state transaction failed")
		s.writeAnswer(w, r, AnswerRefusal, http.StatusInternalServerError, errorBody("state unavailable"))
		return State{}, nil, false
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	state, claims, err := s.state.State(r.Context(), tx)
	if err != nil {
		s.logRequestFailure(r, err, "api: state source failed")
		s.writeAnswer(w, r, AnswerRefusal, http.StatusInternalServerError, errorBody("state unavailable"))
		return State{}, nil, false
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.logRequestFailure(r, err, "api: commit state transaction failed")
		s.writeAnswer(w, r, AnswerRefusal, http.StatusInternalServerError, errorBody("state unavailable"))
		return State{}, nil, false
	}
	// The consumers' lag is read from NATS after the commit, so a slow or silent NATS server holds
	// no snapshot open; what it does not answer is each consumer's `unavailable`, never a refusal.
	if s.consumerLag != nil {
		state.Consumers = ConsumerLagViewsOf(s.consumerLag(r.Context()))
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
// is listening. The controller's boot check reads the project from `state.daemon.project` today,
// which costs it the whole state for one word.
type Health struct {
	Status  string `json:"status"`
	Project string `json:"project"`
}

// healthRoute answers that this daemon is serving, and the project it serves. It reads no store,
// so a daemon whose database is unreachable still answers it.
func (s *server) healthRoute(w http.ResponseWriter, r *http.Request) {
	s.writeAnswer(w, r, AnswerHealth, http.StatusOK, Health{Status: "ok", Project: s.project})
}

// pluginContractHeader is the request header a caller states the contract its own plugin speaks
// in (version.go's 81). A caller that states one is placed by what it states and by nothing
// else, whatever address the request came from, which is how a caller the daemon can place in no
// pod — a pane, an operator's command, a process of a pod whose claims record nothing — reaches
// the body its own reader parses.
const pluginContractHeader = "Legion-Plugin-Contract"

// statedContract is the contract the caller states in pluginContractHeader, and false where it
// states none or states what is no positive number. The header refuses nothing: a caller whose
// header this cannot read is served exactly as one that states nothing is.
func statedContract(r *http.Request) (int, bool) {
	stated := r.Header.Get(pluginContractHeader)
	if stated == "" {
		return 0, false
	}
	contract, err := strconv.Atoi(stated)
	if err != nil || contract <= 0 {
		return 0, false
	}
	return contract, true
}

// contractsFor is the pair of contracts an answer to r is shaped at: restore, the contract whose
// removed keys are written back into the body, and strip, the contract whose later keys are left
// out of it. shapeAnswer applies both.
//
// A caller that states a contract (pluginContractHeader) decides both numbers itself. A caller
// that states none keeps today's reading of its pod: restore is the lowest contract the claims
// whose locator names that pod record, and strip the highest of the same set. A claim recording
// no contract moves neither; a claim with no locator, or in another pod, is in no pod's
// reckoning at all, which is why a retired, failed or suspended claim counts for nothing. A
// caller the daemon cannot place in a pod — every caller under the tmux runtime, where
// Options.CallerPod is nil — and one whose pod holds no such claim are served the body this
// contract writes, both numbers DaemonAPIVersion.
//
// The two halves differ because the two kinds of change differ. A key the daemon stopped serving
// is restored at the lowest contract the pod records, which is safe for every process there: a
// reader at a later contract drops a key it does not know (dispatch://INFRA-1048), and a reader
// below the contract that removed the key needs it. A key a later contract added is left out at
// the highest contract the pod records, because no one body serves both a strict reader that
// refuses `check.role` and a reader that requires it, and this rule keeps the newer process
// working. Strip by the lowest instead and a process that works today stops: `check.role` would
// be taken from the reader that requires it.
//
// Four edges:
//
//   - A contract below ServingFloor takes the floor's shape on the restore side, the lowest
//     shape this daemon describes, and keeps every strip its own number gives it, so such a
//     reader loses no key it loses today.
//   - A contract above DaemonAPIVersion takes the body this contract writes, on both sides.
//   - A contract between the floor and this daemon's own that SupportedContracts does not hold —
//     47 and 50, the two numbers no change ever shipped under — takes the nearest recorded
//     contract below it, on both sides.
//   - The first and the last write one log line each (servedContracts).
func (s *server) contractsFor(r *http.Request, claims []supervise.Claim) (restore, strip int) {
	if stated, ok := statedContract(r); ok {
		return s.servedContracts(r, stated, stated, "")
	}
	// With no claims to read a contract from, no pod reports one: the pair is this daemon's own
	// whatever pod the caller is in, and reading that pod decides nothing. Under the Sandbox
	// runtime the reading lists every cached pod and scans it, which every answer written with
	// no claims would pay for — each refusal, each grant, the catch-all's 404 and `GET /healthz`
	// (writeAnswer). The two routes that answer from the state's own snapshot hand their claims
	// in and are placed by their pod as before (writeAnswerFrom).
	if len(claims) == 0 {
		return DaemonAPIVersion, DaemonAPIVersion
	}
	// A daemon that does not know its callers' pods serves every caller the body this contract
	// writes, and scans nothing for it: under runtime tmux that is every request of every pane
	// and of `legion state`.
	if s.callerPod == nil {
		return DaemonAPIVersion, DaemonAPIVersion
	}
	caller, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return DaemonAPIVersion, DaemonAPIVersion
	}
	pod, inPod := s.callerPod(caller.Addr().Unmap())
	if !inPod {
		return DaemonAPIVersion, DaemonAPIVersion
	}
	lowest, highest, recorded := podContracts(claims, pod)
	if lowest == 0 {
		return DaemonAPIVersion, DaemonAPIVersion
	}
	return s.servedContracts(r, lowest, highest, recorded)
}

// servedContracts is the pair lowest and highest are served at, the four edges of contractsFor
// applied, and the one log line each of the outer two writes. recorded is the claim the lowest
// was read from, and empty for a caller that stated its contract itself, which no claim answers
// for.
func (s *server) servedContracts(r *http.Request, lowest, highest int, recorded claim.Token) (restore, strip int) {
	if lowest < ServingFloor {
		args := []any{"pluginContract", lowest, "servingFloor", ServingFloor}
		if recorded == "" {
			args = append(args, "caller", r.RemoteAddr)
		} else {
			args = append(args, "claim", string(recorded))
		}
		s.log.Warn("api: serving a caller below the lowest contract this daemon shapes an answer for", args...)
	}
	if highest > DaemonAPIVersion {
		s.log.Warn("api: serving a caller above this daemon's own contract",
			"pluginContract", highest, "daemonApiVersion", DaemonAPIVersion, "caller", r.RemoteAddr)
	}
	return restoreContract(lowest), stripContract(highest)
}

// restoreContract is the contract a caller at n has removed keys restored at: the floor for a
// caller below it, which is the lowest shape this daemon describes, and otherwise the recorded
// contract n itself takes.
func restoreContract(n int) int {
	if n < ServingFloor {
		return ServingFloor
	}
	return recordedContractAtOrBelow(n)
}

// stripContract is the contract a caller at n has later keys left out at: its own number for a
// caller below the floor, which keeps every strip that number gives it today, and otherwise the
// recorded contract n itself takes.
func stripContract(n int) int {
	if n < ServingFloor {
		return n
	}
	return recordedContractAtOrBelow(n)
}

// recordedContractAtOrBelow is n itself where SupportedContracts records it, the nearest recorded
// contract below n where it does not (47 takes 46 and 50 takes 49), and DaemonAPIVersion for an n
// above this daemon's own. Its callers hand it nothing below ServingFloor.
func recordedContractAtOrBelow(n int) int {
	if n >= DaemonAPIVersion {
		return DaemonAPIVersion
	}
	nearest := ServingFloor
	for _, recorded := range SupportedContracts {
		if recorded <= n && recorded > nearest {
			nearest = recorded
		}
	}
	return nearest
}

// shapeAnswer is body, the answer id names, as the plugin that asked for it reads it: the keys
// this daemon stopped writing put back at restore, and the keys it began writing after strip
// left out again (contractsFor decides the two).
//
// Every answer of the table has a row here, and the rows of the answers whose shape has not
// moved since ServingFloor are written out one by one rather than left to a default. A shape
// change that lands with no row of its own is then an answer this function does not name, which
// TestShapeAnswerShapesEveryAnswer catches, where a default would quietly serve the new key to a
// reader that refuses the whole body for it.
//
// The state's row is shapeState, and it carries the reasoning of every serving-down this daemon
// makes. A plugin older than dispatch://INFRA-1048 — which is who this is kept for — reads the
// state with a strict schema and refuses the whole body for one key it does not know, where
// today's plugin strips one; and an update round serves a claim of the contract before it until
// its pod is replaced (version.go's 46). So a caller below supervise.PodUpdateContract, the
// contract that added `updateRound` and `updateHead` to the claim views, is served the state
// without them (withoutUpdateRounds), which is the body that contract's schema describes; a
// caller below memoryContract is served it without the issues' three memory fields
// (withoutMemory); a caller below treeContract without `admission.trees` and the issues' `tree`
// (withoutTrees, withoutTree); a caller below wakesWaitingContract without `wakesWaiting`
// (withoutWakesWaiting); a caller below gatedStepsContract without the issues' `gatedSteps`
// (withoutGatedSteps); a caller below retiredStrandedContract is served the report of claims
// nothing will wake without its retired entries (withoutRetiredStranded); a caller below
// stallRecordContract without the claim views' `step` (withoutStallRecord); a caller below
// waitingLineContract without each waiting issue's `waitingLine` (withoutWaitingLine); a caller
// below backwardMovesContract without the workers' `backwardMoves` (withoutBackwardMoves); a
// caller below checkRoleContract is served each issue's check without `role`
// (withoutCheckRole); and a caller below shapedAnswersContract is served the body without
// `lowestPluginContract` (withoutLowestPluginContract) and its claim views without
// `updateState`, `updateReaskedForPushCheck`, `unpromptedTurnTakesAnswerBack` and `wake`
// (withoutClaimRoundReport). The twelve compose: a caller below all of them is served a body
// carrying none of the eighteen keys, each of them absent rather than empty, since a strict
// reader before the contract that added a key refuses it whether or not it names anything, and
// with no retired entry under `strandedClaims`. Every later reader takes those keys as optional
// and all three stranded states — `packages/contracts/src/legion-api.ts` marks them optional for
// exactly these bodies.
//
// The thirteenth row takes a value rather than a key, and composes with the twelve: a caller
// below waitAnswerContract is served each issue's wait without its answered pair where
// `answeredBy` is a value that caller's contract does not admit (withoutUnadmittedWaitAnswer),
// because a strict reader refuses the whole body for a value its enumeration does not name as
// surely as for a key it does not know (answers.go's EnumeratedFields).
//
// The record's row is shapeIssue, the per-issue half of the state's own row: the record route
// answers the very value the whole-project body holds under `issues.<key>` (issueRecordRoute),
// so one function shapes the issue a caller reads on its own and the issue it reads inside the
// whole body, and the two cannot part company.
//
// On the other side, a caller below fixAttemptsContract is served the two keys that contract
// removed, as stand-ins its own reader requires (withFixAttempts, withMaxFixAttempts). Any
// caller at this daemon's own contract — a pod at it, the operator's `legion state`, a tmux
// pane — is served the state as this contract writes it, neither restored nor stripped.
func shapeAnswer(id AnswerID, body any, restore, strip int) any {
	switch id {
	case AnswerState:
		state, isState := body.(State)
		if !isState {
			return body
		}
		return shapeState(state, restore, strip)
	case AnswerIssueRecord:
		issue, isIssue := body.(Issue)
		if !isIssue {
			return body
		}
		return shapeIssue(issue, restore, strip)
	case AnswerHealth, AnswerRegister, AnswerRegisterController, AnswerNoContent,
		AnswerReleaseRecords, AnswerGrant, AnswerHandoffComplete, AnswerEmptyObject, AnswerFreshSession,
		AnswerCompact, AnswerWaveRelease, AnswerRefusal:
		// The identity, one row each: no key of these answers has arrived or gone since
		// ServingFloor, so every contract this daemon serves reads the body it writes.
		return body
	case answerUnparsed:
		// The identity too, and for a reason of its own: no plugin parses these bodies, so no
		// contract describes a shape for them (writeUnparsedAnswer).
		return body
	default:
		panic("api: shapeAnswer has no row for the answer " + string(id))
	}
}

// shapeState is the state's row of shapeAnswer: the keys restore's reader requires written back,
// and the keys strip's reader refuses left out.
//
// The body's own keys are shaped here and every key of an issue in shapeIssue, which the record
// route serves one value of on its own (AnswerIssueRecord). One pass over the issue map applies
// every per-issue serving-down, so the map is copied once however many of them a caller takes,
// and a caller at this daemon's own contract, whom no row of either function moves, is served
// the map the source answered.
//
// The claims a caller's contract was read from are the state's own snapshot's, because the body
// is: a claim and the state it was projected into cannot come from two moments (store.ClaimsTx).
// A state whose claims the source could not read is no state at all — its claim views are built
// from them — so the route refuses it before this runs, and no readable state is left whose
// caller's contract is unknown.
func shapeState(state State, restore, strip int) State {
	if restore < fixAttemptsContract {
		state = withMaxFixAttempts(state)
	}
	if strip < treeContract && carriesTrees(state) {
		state = withoutTrees(state)
	}
	if strip < wakesWaitingContract && carriesWakesWaiting(state) {
		state = withoutWakesWaiting(state)
	}
	if strip < retiredStrandedContract && carriesRetiredStranded(state) {
		state = withoutRetiredStranded(state)
	}
	if strip < shapedAnswersContract && carriesLowestPluginContract(state) {
		state = withoutLowestPluginContract(state)
	}
	// Every row of shapeIssue is below this daemon's own contract, so a caller at it takes none
	// of them and is served the issue map the source answered, with no copy made of it.
	if restore < DaemonAPIVersion || strip < DaemonAPIVersion {
		issues := make(map[string]Issue, len(state.Issues))
		for key, issue := range state.Issues {
			issues[key] = shapeIssue(issue, restore, strip)
		}
		state.Issues = issues
	}
	return state
}

// shapeIssue is one issue of the state as the caller reads it: the per-issue half of shapeState,
// and the record's row of shapeAnswer, since the record route answers the very value the
// whole-project body holds for that issue (issueRecordRoute). The two readings of one issue are
// shaped here and nowhere else, so a caller served the state and a caller served the record read
// the same issue field for field at every contract.
//
// The issue is a value of its own, and each pointer and map it carries is copied before it is
// written, so the state the source answered is left as it was.
func shapeIssue(issue Issue, restore, strip int) Issue {
	if restore < fixAttemptsContract {
		issue = withFixAttempts(issue)
	}
	if strip < supervise.PodUpdateContract {
		issue = withoutUpdateRounds(issue)
	}
	if strip < memoryContract {
		issue = withoutMemory(issue)
	}
	if strip < treeContract {
		issue = withoutTree(issue)
	}
	if strip < checkRoleContract {
		issue = withoutCheckRole(issue)
	}
	if strip < gatedStepsContract {
		issue = withoutGatedSteps(issue)
	}
	if strip < stallRecordContract {
		issue = withoutStallRecord(issue)
	}
	if strip < waitingLineContract {
		issue = withoutWaitingLine(issue)
	}
	if strip < backwardMovesContract {
		issue = withoutBackwardMoves(issue)
	}
	if strip < waitAnswerContract {
		issue = withoutUnadmittedWaitAnswer(issue, strip)
	}
	if strip < shapedAnswersContract {
		issue = withoutClaimRoundReport(issue)
	}
	return issue
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
// binary serves, and that number only rises — so a pod at this contract or at a later one is
// served the record and only a pod still running an older plugin is not.
const stallRecordContract = 57

// waitingLineContract is the daemon contract that added each waiting issue's place in the line
// to the state (version.go's 58). A plugin before it is served the state without the field:
// one built before sjawhar/legion#2788 reads it with a strict schema and would refuse the
// whole body for a key it does not name, and one built after strips the key instead, so the
// strip is what keeps the field from a caller that did not ask for it rather than what keeps
// such a caller reading at all.
const waitingLineContract = 58

// checkRoleContract is the daemon contract that added each production check view's `role`, the
// role the check records as the one that works it (version.go's 54): a plugin before it reads
// the check view with a strict schema that knows no `role` and refuses the whole body for it.
// The boot gate alone covered that reader until this daemon shaped its answer (version.go's 81).
const checkRoleContract = 54

// fixAttemptsContract is the daemon contract that took the fix-attempt count out of the state
// (version.go's 59): each pull request's `fixAttempts`, which counted the heads pushed onto a red
// verdict, and the settings' `max_fix_attempts`, which capped it. A plugin before it parses the
// pull request with a strict schema that requires `fixAttempts` and refuses the whole body
// without it, and every worker's `read_record` parses the state through that reader, so a caller
// below this contract is served both keys as stand-ins (withFixAttempts, withMaxFixAttempts).
// The boot gate alone covered that reader until this daemon shaped its answer (version.go's 81).
const fixAttemptsContract = 59

// backwardMovesContract is the daemon contract that added each worker's counts of the moves back
// its issue has made to the state (version.go's 62). A plugin before it is served the state
// without the field: one built before sjawhar/legion#2788 reads it with a strict schema and
// would refuse the whole body for a key it does not name, and one built after strips the key
// instead, so the strip is what keeps the counts from a caller that did not ask for them rather
// than what keeps such a caller reading at all.
const backwardMovesContract = 62

// waitAnswerContract is the newest contract any value of a declared enumerated field arrived at
// (answers.go's EnumeratedFields): 68 today, the contract that widened `answeredBy` on an
// `awaiting_person` root's wait to a ninth value, `declined` (version.go's 68,
// record.AnsweredByDecline). A caller at or above it admits every value this daemon can write,
// so the row below it asks the table for no such caller; a value that arrives later moves this
// constant to that value's own contract, and the row then serves the new value down by the same
// reading of the same table.
const waitAnswerContract = 68

// shapedAnswersContract is the daemon contract this change lands under (version.go's 81), and
// the contract that added five keys of its own: `lowestPluginContract` on the state, the
// lowest contract any claim of the project records, and each claim view's `updateState`,
// `updateReaskedForPushCheck`, `unpromptedTurnTakesAnswerBack` and `wake`. A plugin before it
// reads the state with a schema that names none of the five and refuses the whole body for one
// of them, so a caller below this contract is served the state without them
// (withoutLowestPluginContract, withoutClaimRoundReport).
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes
// moves that constant, and the contract these keys arrived at stays where it is. It moves only
// with this change's own number, as the arrival contracts of this change's two new answers do
// (answers.go's AnswerIssueRecord and AnswerHealth).
const shapedAnswersContract = 81

// carriesTrees is whether state carries `admission.trees`, the key of treeContract's serving-down
// that is the body's own; each issue's `tree`, the other key, is shapeIssue's (withoutTree). It
// carries `admission.trees` whatever the daemon holds, since the admission block writes that key
// on every body, empty rather than null (Admission.MarshalJSON), and the strict reader before the
// contract refuses the empty key as surely as one naming a tree. So this is true of every state
// the route writes; it is asked beside carriesWakesWaiting, carriesRetiredStranded and
// carriesLowestPluginContract all the same, because shapeState puts the same question to the four
// and because a later contract that stops writing `trees` on every body answers it here.
func carriesTrees(State) bool { return true }

// carriesWakesWaiting is whether state carries the key wakesWaitingContract added: `wakesWaiting`,
// the wakes the pacing holds. It carries that key whatever the daemon holds, since the state
// writes it on every body, empty rather than null (State.MarshalJSON), and the strict reader
// before the contract refuses the empty key as surely as one naming a wake. So this is true of
// every state the route writes; it is asked beside carriesTrees, carriesRetiredStranded and
// carriesLowestPluginContract all the same, because shapeState puts the same question to the four
// and because a later contract that stops writing `wakesWaiting` on every body answers it here.
func carriesWakesWaiting(State) bool { return true }

// withoutGatedSteps is the issue with no `gatedSteps`, the field gatedStepsContract added to the
// state; nothing else of that contract is on the route.
func withoutGatedSteps(issue Issue) Issue {
	issue.GatedSteps = nil
	return issue
}

// carriesRetiredStranded is whether any claim of state's report of claims nothing will wake is
// retired, the state retiredStrandedContract added.
func carriesRetiredStranded(state State) bool {
	return slices.ContainsFunc(state.StrandedClaims, func(c StrandedClaim) bool {
		return c.State == string(supervise.StateRetired)
	})
}

// withoutWaitingLine is the issue with no `waitingLine`, the field waitingLineContract added to
// the state; nothing else of that contract is on the route.
func withoutWaitingLine(issue Issue) Issue {
	issue.WaitingLine = nil
	return issue
}

// withoutBackwardMoves is the issue with no worker carrying `backwardMoves`, the one key
// backwardMovesContract added to this route; nothing else of that contract is on it, the
// settings' `hooks` riding under a block every plugin reads loosely. The worker map is copied
// only for an issue a worker of which carries the counts, since the field rides on the
// implementer of an issue that has moved back and on nobody else, so the issues without one are
// left as the source answered them rather than rebuilt.
func withoutBackwardMoves(issue Issue) Issue {
	if !issueCarriesBackwardMoves(issue) {
		return issue
	}
	workers := make(map[claim.Role]PhaseView, len(issue.Workers))
	for role, worker := range issue.Workers {
		worker.BackwardMoves = nil
		workers[role] = worker
	}
	issue.Workers = workers
	return issue
}

// issueCarriesBackwardMoves is whether a worker of this one issue carries the counts. The field
// is written only where there is one — an issue that has had no move back carries no such key —
// so the strip asks this rather than copying every worker map it meets.
func issueCarriesBackwardMoves(issue Issue) bool {
	for _, worker := range issue.Workers {
		if len(worker.BackwardMoves) > 0 {
			return true
		}
	}
	return false
}

// recordsContract is whether c holds a process in a pod and records the contract that process's
// plugin speaks. It is the one test both readings of the claims make, so a caller's pod and the
// fleet count the same claims (podContracts, lowestPluginContract).
//
// A claim with no locator, and one whose locator names no pod, holds no such process:
// suspending, failing or retiring a claim ends what the machine had with its process, and the
// claim gives up its locator with it (supervise's letGo), so such a claim calls nothing and no
// shape is owed to it. A claim recording no contract reports nothing either.
func recordsContract(c supervise.Claim) bool {
	return c.Locator != nil && c.Locator.Sandbox != nil && c.PluginContract != 0
}

// podContracts is the lowest and the highest plugin contract the claims whose process runs in pod
// record, and the claim the lowest was read from. All three are zero when no claim of the pod
// records a contract: a claim in another pod, and a claim recordsContract answers false for, are
// in no pod's reckoning.
//
// Every role of a pod is served by the plugin that pod runs, so the lowest is the contract whose
// removed keys every process there can read, and the highest the contract whose later keys the
// pod's newest process requires (contractsFor says why the two halves differ).
func podContracts(claims []supervise.Claim, pod string) (lowest, highest int, recorded claim.Token) {
	for _, c := range claims {
		if !recordsContract(c) || c.Locator.Sandbox.PodUID != pod {
			continue
		}
		if lowest == 0 || c.PluginContract < lowest {
			lowest, recorded = c.PluginContract, c.Token
		}
		if c.PluginContract > highest {
			highest = c.PluginContract
		}
	}
	return lowest, highest, recorded
}

// lowestPluginContract is the lowest contract the claims that hold a process record, over every
// pod of the project rather than one caller's: the claims recordsContract answers true for, with
// the pod left out of the question podContracts asks. Nil where no such claim exists.
func lowestPluginContract(claims []supervise.Claim) *int {
	lowest := 0
	for _, c := range claims {
		if !recordsContract(c) {
			continue
		}
		if lowest == 0 || c.PluginContract < lowest {
			lowest = c.PluginContract
		}
	}
	if lowest == 0 {
		return nil
	}
	return &lowest
}

// withoutUpdateRounds is the issue with no claim view carrying `updateRound` or `updateHead`, the
// two fields supervise.PodUpdateContract added to the state; nothing else of that contract is on
// the route but `settings` keys, which every plugin reads loosely. The architect's view and the
// worker map are copied, so the state the source answered is left as it was.
func withoutUpdateRounds(issue Issue) Issue {
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
	return issue
}

// withoutMemory is the issue with no `memoryGroup`, `memoryDrawMode` or `memoryDrawnAt`, the
// three fields memoryContract added to the state; nothing else of that contract is on the route
// but `settings` keys, which every plugin reads loosely.
func withoutMemory(issue Issue) Issue {
	issue.MemoryGroup, issue.MemoryDrawMode, issue.MemoryDrawnAt = "", "", nil
	return issue
}

// withoutTrees is state with no `admission.trees`, the first of the two keys treeContract added;
// each issue's `tree`, the second, is withoutTree's. Nothing else of that contract is a key of
// its own: `active` and `waiting` keep their names and count issues rather than trees, which no
// downgrade can carry, and which the boot gate and `legion probe-image` keep a plugin of the
// contract before this one from reading (version.go's 51). The admission block is a value of the
// state's own, so the state the source answered is left as it was.
func withoutTrees(state State) State {
	state.Admission.Trees, state.Admission.treesHidden = nil, true
	return state
}

// withoutTree is the issue with no `tree`, the second of the two keys treeContract added
// (withoutTrees carries the first).
func withoutTree(issue Issue) Issue {
	issue.Tree = ""
	return issue
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

// withoutUnadmittedWaitAnswer is the issue whose wait carries no `answeredAt` and no
// `answeredBy` where that `answeredBy` is a value strip's reader does not admit: today the
// `declined` waitAnswerContract added, and tomorrow any value the table gives a later arrival
// (answers.go's EnumeratedFields). The rule is the table's and not one value's, so a value
// added later is served down by this same row.
//
// The pair goes and the `waitingOn` block stays, which is where this parts company with
// withoutRetiredStranded: a retired stranded entry has nothing true left in it once its state
// is gone, and most of a wait is true at every contract. A block dropped whole would tell a
// reader below the contract that the root waits on nobody, which is false and which that reader
// cannot tell apart from a root that truly waits on no person; dropping the pair leaves `kind`,
// `ask` or `gateVersion` and `artifactId`, and `since`, every one of them true at that
// contract, and loses only the answer, which is the part no older value spells.
//
// The pair is readable at every contract, measured on `packages/contracts/src/legion-api.ts`:
// `legionWaitTimes` declares `since`, `answeredAt` and `answeredBy` each `.optional()`, and
// `legionWaitingOn`'s `refine` asks only that `answeredAt` and `answeredBy` appear together,
// which dropping both satisfies. A wait nobody has answered carries no pair at all, and that is
// the ordinary body of every `awaiting_person` root at every contract, so a `waitingOn` with no
// answered pair is a shape every reader already parses. The two go together or not at all:
// either one alone refuses the body on that `refine`.
//
// The wait is a pointer of the issue's own, and it is copied before it is written, so the state
// the source answered is left as it was.
//
// A caller below the floor is read as a caller at it: a value the table records at ServingFloor
// is older than the floor rather than arrived there (answers.go's EnumeratedFields), and the
// floor is the lowest shape this daemon describes, so no reader at all is below such a value's
// arrival. stripContract hands this a caller's own number below the floor, which the max is
// here for.
func withoutUnadmittedWaitAnswer(issue Issue, strip int) Issue {
	if issue.WaitingOn == nil || issue.WaitingOn.AnsweredBy == "" {
		return issue
	}
	if waitAnswerArrival(issue.WaitingOn.AnsweredBy) <= max(strip, ServingFloor) {
		return issue
	}
	wait := *issue.WaitingOn
	wait.AnsweredAt, wait.AnsweredBy = nil, ""
	issue.WaitingOn = &wait
	return issue
}

// waitAnswerArrival is the contract value arrived at, read from the declared table. A value no
// row of the table names reads as this daemon's own contract: no older reader is known to admit
// it, so none is served it, and the gate reds on it at every contract until the table declares
// it (contract_shapes_test.go).
func waitAnswerArrival(value record.Answer) int {
	if since, declared := waitAnswerArrivals[string(value)]; declared {
		return since
	}
	return DaemonAPIVersion
}

// waitAnswerArrivals is the contract each value of `answeredBy` arrived at, read once from the
// table. The state's row answers for the record's too: the two declare one field of the daemon's
// own (answers.go's EnumeratedFields), and shapeIssue shapes the issue behind both answers.
var waitAnswerArrivals = arrivalsOf(AnswerState, "issues.*.waitingOn.answeredBy")

// arrivalsOf is the contract each value of the declared enumerated field arrived at. It panics
// where the table declares no such field, at the package's first use: a serving-down of a value
// reads the row that explains it, and a row renamed away leaves the serving-down with no rule.
func arrivalsOf(answer AnswerID, keyPath string) map[string]int {
	for _, field := range EnumeratedFields {
		if field.Answer != answer || field.KeyPath != keyPath {
			continue
		}
		arrivals := make(map[string]int, len(field.Values))
		for _, value := range field.Values {
			arrivals[value.Value] = value.Since
		}
		return arrivals
	}
	panic("api: answers.go declares no enumerated field `" + keyPath + "` of the " + string(answer) + " answer")
}

// withoutStallRecord is the issue with no claim view carrying `step`, the one key
// stallRecordContract added to this route; nothing else of that contract is on it. The
// architect's view and the worker map are copied, so the state the source answered is left as it
// was, as withoutUpdateRounds leaves it.
func withoutStallRecord(issue Issue) Issue {
	if issue.Architect != nil {
		architect := *issue.Architect
		architect.Step = nil
		issue.Architect = &architect
	}
	if issue.Workers != nil {
		workers := make(map[claim.Role]PhaseView, len(issue.Workers))
		for role, worker := range issue.Workers {
			worker.Claim.Step = nil
			workers[role] = worker
		}
		issue.Workers = workers
	}
	return issue
}

// carriesLowestPluginContract is whether state carries the key shapedAnswersContract added to
// the body itself: `lowestPluginContract`, the lowest contract any claim of the project that
// holds a process records. A project with no such claim reports no number and carries the key
// for nobody (State.LowestPluginContract).
func carriesLowestPluginContract(state State) bool { return state.LowestPluginContract != nil }

// withoutLowestPluginContract is state with no `lowestPluginContract`, the one key
// shapedAnswersContract added to the body itself. The field is the state's own value, so the
// state the source answered is left as it was, as withoutWakesWaiting leaves it.
func withoutLowestPluginContract(state State) State {
	state.LowestPluginContract = nil
	return state
}

// withoutClaimRoundReport is the issue with no claim view carrying `updateState`,
// `updateReaskedForPushCheck`, `unpromptedTurnTakesAnswerBack` or `wake`, the four keys
// shapedAnswersContract added to the claim views. The architect's view and every worker's are
// each stripped, as withoutStallRecord strips them. The architect's view and the worker map are
// copied, so the state the source answered is left as it was.
func withoutClaimRoundReport(issue Issue) Issue {
	strip := func(v *ClaimView) {
		v.UpdateState, v.UpdateReaskedForPushCheck, v.UnpromptedTurnTakesAnswerBack, v.Wake = "", false, false, nil
	}
	if issue.Architect != nil {
		architect := *issue.Architect
		strip(&architect)
		issue.Architect = &architect
	}
	if issue.Workers != nil {
		workers := make(map[claim.Role]PhaseView, len(issue.Workers))
		for role, worker := range issue.Workers {
			strip(&worker.Claim)
			workers[role] = worker
		}
		issue.Workers = workers
	}
	return issue
}

// withoutCheckRole is the issue whose check carries no `role`, the one key checkRoleContract
// added to this route. The key is left off rather than emptied, because the strict reader before
// the contract refuses it as surely when it is empty as when it names a role, which is why
// CheckView.MarshalJSON writes the key on every body but this one (CheckView.roleHidden), as
// Admission.MarshalJSON leaves off `trees`. The flag is set on a check of the issue's own, so the
// state the source answered is left as it was.
func withoutCheckRole(issue Issue) Issue {
	if issue.Check != nil {
		check := *issue.Check
		check.roleHidden = true
		issue.Check = &check
	}
	return issue
}

// fixAttemptsStandIn is the `fixAttempts` a caller below fixAttemptsContract is served on every
// pull request: zero, what the stopped `fix_attempts` column takes on an insert and never leaves
// (record's PutPullRequest). Nothing has counted a fix attempt since dispatch://INFRA-956, and no
// plugin reads the value — the count drove a block this daemon no longer applies — so the
// stand-in gives that contract's reader the key its schema requires and reports nothing. One
// value stands for every pull request of every body, since nothing writes through the pointer.
var fixAttemptsStandIn = 0

// maxFixAttemptsStandIn is the `max_fix_attempts` a caller below fixAttemptsContract is served in
// the settings block: three, the setting's default before that contract removed it from
// legion.yaml. No plugin reads it either, for the same reason fixAttemptsStandIn is read by none.
var maxFixAttemptsStandIn = 3

// withFixAttempts is the issue whose pull request carries `fixAttempts`, the key
// fixAttemptsContract took off this route, as the stand-in a reader below that contract requires.
// The pull request view is copied, so the state the source answered is left as it was.
func withFixAttempts(issue Issue) Issue {
	if issue.PullRequest != nil {
		pullRequest := *issue.PullRequest
		pullRequest.fixAttempts = &fixAttemptsStandIn
		issue.PullRequest = &pullRequest
	}
	return issue
}

// withMaxFixAttempts is state whose settings block carries `max_fix_attempts`, the other key
// fixAttemptsContract took off this route, as the stand-in a reader below that contract is
// served beside `fixAttempts`. The block is a value of the state's own, so the state the source
// answered is left as it was.
func withMaxFixAttempts(state State) State {
	state.Settings.maxFixAttempts = &maxFixAttemptsStandIn
	return state
}

// answerUnparsed names a body no plugin parses: the operator's own answers and the controller's
// secret and credential, which `legion` the command and the daemon's own controller read. It is
// no row of Answers, which is the plugin's table alone, and it shapes to nothing, since no
// contract describes a shape for a body no plugin reads (writeUnparsedAnswer).
const answerUnparsed AnswerID = "unparsed"

// writeAnswer writes body as the answer id names it, shaped down to the contracts the caller
// reads at (contractsFor, shapeAnswer). Every body this package writes goes through it, or
// through writeAnswerFrom which it stands on, and nothing else calls writeJSON, so a shape a
// later contract moves cannot be written past the shaping.
//
// It hands contractsFor no claims, which costs a caller nothing: a caller that states its own
// contract is placed by its header alone, and the state and the record are the two answers whose
// shape has moved since ServingFloor. Both are written by writeAnswerFrom, which hands the
// claims of the very snapshot each answers. Every other answer is the identity at every contract
// this daemon serves, so a read of the claims for one of them would decide nothing: such an
// answer is written without one, and contractsFor reads no pod for it either.
func (s *server) writeAnswer(w http.ResponseWriter, r *http.Request, id AnswerID, status int, body any) {
	s.writeAnswerFrom(w, r, id, status, body, nil)
}

// writeAnswerFrom is writeAnswer for an answer whose own claims place the caller: claims are the
// project's claims as the body being answered holds them, which the state route reads through
// the transaction it projected that state in.
func (s *server) writeAnswerFrom(w http.ResponseWriter, r *http.Request, id AnswerID, status int, body any, claims []supervise.Claim) {
	restore, strip := s.contractsFor(r, claims)
	writeJSON(w, status, shapeAnswer(id, body, restore, strip))
}

// writeUnparsedAnswer writes a success body no plugin parses: the operator's claim answers and
// the controller's secret and credential (answerUnparsed). It is written through the one writer
// like every other body, so a route cannot reach writeJSON around the shaping; what it says is
// that there is no contract to shape this body to. A refusal on those same routes is a refusal
// like any other, and is written as AnswerRefusal.
func (s *server) writeUnparsedAnswer(w http.ResponseWriter, r *http.Request, status int, body any) {
	s.writeAnswer(w, r, answerUnparsed, status, body)
}

// writeJSON is the one writer of a response body in this package. Only writeAnswerFrom calls it,
// and it stays unexported for that reason: a body written around it is a body no contract shaped.
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
