// Package daemon runs one legion: it opens the store its configuration names, brings the schema
// forward, records the boot, supervises every role claim — the worker stream the shims dial, the
// runtime their processes run under, one machine per claim — serves the API, and stops in the
// order the durable record needs.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/agentsecrets"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/memorygroup"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/omplaunch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/settings"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	// bootTimeout bounds the work between the plugin gate passing and the API listening: an
	// unreachable Postgres refuses in milliseconds, but a reachable one that never answers must
	// not leave the daemon hanging with nothing on stderr. The cluster fence and the store's open
	// and restore, which come before the gate, each have a budget of their own of that length. The
	// gate is not bounded by it — it waits out host load for as long as that lasts (pluginGate) —
	// nor are the GitHub App tokens' mint and the image probe, which wait out GitHub's and the
	// cluster's transient trouble within retries of their own (appMintRetry, imageProbeRetry); the
	// work after each has a budget anew.
	bootTimeout = 30 * time.Second
	// catchUpAttemptBound bounds one attempt of the boot's catch-up of the roots that run or wait on
	// a person. Each root has bootTimeout of its own and the roots finished survive a retry, so this
	// bound is no budget for the work, only the end of an attempt that would otherwise never return.
	catchUpAttemptBound = time.Hour
	// stopBudget bounds how long the daemon's stop waits for its own work, from the moment the stop
	// begins (supervision.halt: at the signal, at serve's end, or when supervision fails to start,
	// once the boot is recorded) until the daemon stamps its boot. The halt cancels every decision
	// the daemon makes itself — the machines' own events, the workflow's outbox, the controller
	// keeper — and the boot's own steps, so work that answers to its context ends at once. The API's
	// drain, which starts with the stop, gets the budget's first four fifths (8 s): a decision a
	// route asked for runs on until the drain ends, so an operator's suspension whose process is
	// already exiting is recorded rather than undone at the next boot. The last fifth leaves room
	// for the work the drain's end cuts short to return before the budget runs out. Work still
	// running at the budget's end, a call into a process that does not answer, is logged with each
	// claim still deciding and not waited on: the next boot re-adopts each process a claim records
	// and relaunches each launch left unrecorded. natsCloseTimeout bounds the close of the
	// workflow's NATS connection, stampTimeout the stamp, and storeCloseTimeout the store's close
	// after it, so the daemon returns at most
	// stopBudget+natsCloseTimeout+stampTimeout+storeCloseTimeout (24 s) after its stop begins,
	// inside the 30 seconds a pod is given between SIGTERM and SIGKILL. Before the boot is
	// recorded, a signal ends the daemon through each boot step's own bounds, as Run says.
	stopBudget = 10 * time.Second
	// natsCloseTimeout bounds the close of the workflow's Envoy NATS connection on the stop path
	// (closeWorkflowNATS). nats.Conn.Close flushes the outbound buffer while it holds the
	// connection's own lock (nats.go v1.53.1, nats.go:6098-6101: nc.bw.flush() when the socket is
	// up and no reconnect is in progress), that write goes through timeoutWriter, whose deadline
	// is Opts.FlusherTimeout (nats.go:2295-2296, :6827), and the default is a minute
	// (nats.go:67), which natsauth.Connect leaves as it is. So a server that has stopped reading
	// from a full send buffer would hold the stop there for up to that minute. A connection the
	// daemon stops waiting for costs nothing: the process is ending, and the server drops the
	// socket when it goes.
	natsCloseTimeout  = 2 * time.Second
	stampTimeout      = 10 * time.Second
	storeCloseTimeout = 2 * time.Second
	// streamSocket is the worker stream's unix socket under the state directory: the one address
	// every pane's shim dials under tmux (decision 2 — no configuration key).
	streamSocket = "worker-stream.sock"
	// orphanSweepInterval and orphanGrace are the periodic reconciliation: every minute, a Legion
	// process nothing records is ended once it has idled for two.
	orphanSweepInterval = time.Minute
	orphanGrace         = 2 * time.Minute
	// bootOrphanReconcileAttempts bounds the immediate retry before a previously unrecorded
	// launch may be relaunched. An error is not an absent pane: the claim stays queued and the
	// periodic retry owns it until tmux can say the old pane was reaped or absent.
	bootOrphanReconcileAttempts   = 3
	bootOrphanReconcileRetryDelay = 100 * time.Millisecond
	// launchBackoffFirst is the machines' first wait before a launch is tried again after a
	// charged launch failure (supervise.Timeouts.LaunchBackoff), doubled for each one after it: a
	// three-failure budget's tries span three seconds at least, never a moment one fault lasts.
	launchBackoffFirst = 2 * time.Second
	// busyBackoffFirst is the Sandbox runtime's first wait before a launch's request the API server
	// answered overloaded is sent again (sandbox.Options.BusyBackoff), doubled up to 8 s.
	busyBackoffFirst = 250 * time.Millisecond
	// cleanSessionStopBudget bounds one clean stop of an Envoy session registration — the
	// registry read and the delete that follows it, together (cleanSessionStop). It is longer
	// than the listener's own budget for one route (sendRequestBudget,
	// packages/envoy/cmd/listener/api.go: 3.5 s) and shorter than two of them, so a listener
	// answering promptly answers both inside it, while a read that spends the whole budget sends
	// no delete and one answered late leaves the delete less than its own route's budget — which
	// costs that session at most the wait it already had. It is short enough that a listener
	// which never answers is abandoned while the claim's relaunch is still a launch backoff and
	// a process boot away (launchBackoffFirst), rather than left in flight for as long as the
	// listener likes. What keeps a stop that does land late off the relaunch's registration is
	// the two conditions the stop carries (cleanSessionStop), not this bound.
	cleanSessionStopBudget = 5 * time.Second
)

// overrides are the parts of a daemon a test replaces, and the listener `legion start` inherited;
// the zero value is the real daemon binding its own address.
type overrides struct {
	// runtime builds the runtime over the worker stream in place of the configured one; nil is the
	// configured runtime.
	runtime runtimeFactory
	// clock is the machines' time; nil is the wall clock.
	clock supervise.Clock
	// getenv is the environment the OMP invocation is resolved against; nil is the process's.
	getenv func(string) string
	// environ is the environment provider keys are resolved under (`secrets get`) and the NATS
	// nkey seed is read from when the configuration names no file; nil is the process's.
	environ []string
	// orphanSweep is how often orphans are reconciled; zero is orphanSweepInterval.
	orphanSweep time.Duration
	// controllerRetry is the first wait before a failed daemon-launched controller is retried; zero
	// is controllerRetryFirst.
	controllerRetry time.Duration
	// stopBudget is how long the daemon's stop waits for its own work once it begins; zero is
	// stopBudget.
	stopBudget time.Duration
	// controllerFirstWake is how soon after the workflow runtime starts it applies its first
	// controller tick (workflowRuntime.controllerFirstWake); zero is firstControllerTick.
	controllerFirstWake time.Duration
	// idleTick is how often the daemon decides every pod of its project and fires the backstops
	// that came due (workflowRuntime.idleTick, both ticks); zero is the production minute. A test
	// of release and wake shortens it, so it waits out its own period rather than the daemon's.
	idleTick time.Duration
	// entrySweep is how often the daemon converges the phase entries with no fact arriving
	// (workflowRuntime.entrySweep); zero is the production minute, entrySweepInterval. A test of
	// the sweep shortens it, so it waits out its own period rather than the daemon's.
	entrySweep time.Duration
	// gate stands in for the plugin gate when runtime is replaced: nil is none, since a replaced
	// runtime launches no Oh My Pi to gate. With the tmux runtime, the gate is always the real one.
	gate func(ctx context.Context) error
	// probe stands in for the worker image probe when runtime is replaced under kubernetes: nil is
	// none. With the Agent Sandbox runtime, the probe is always the real one.
	probe func(ctx context.Context, rt runtime.Runtime) (bootprobe.ImageReport, error)
	// workflowTokens replaces the GitHub App token manager in a workflow integration test. The
	// production daemon always mints through appauth.New.
	workflowTokens appauth.Tokens
	// githubAPI is the GitHub REST root a workflow integration test points the workflow at (its
	// required-checks reads and its issue branches' creates) and the Kubernetes runtime's read of
	// the shared public leak check at (sandboxRuntime); empty, in production, is
	// https://api.github.com.
	githubAPI string
	// listen takes the API listener for cfg's bind and port; nil binds them with net.Listen. Run's
	// serves the socket `legion start` inherited from LEGION_LISTEN_FD (handed); a test's hands
	// over a listener it holds.
	listen func(network, address string) (net.Listener, error)
	// strandedReported runs once the boot's report of the claims nothing will wake has finished,
	// whether it logged or not (reportStranded); nil, in production, is nothing. The report runs
	// beside the API, so a test that asserts on its line waits for this rather than for /healthz.
	strandedReported func()
	// settingsRebuilder is handed the daemon's one rebuild observer once boot has built it, so a
	// test can ask it to read the credential files again (rebuilder.rereadCredentials), which is
	// what the file watcher does for a rotation of a file whose path did not change. nil, in
	// production, is nobody.
	settingsRebuilder func(*rebuilder)
	// beforeServingLatched runs once the goroutine that serves the API has begun and before the
	// daemon's readiness latch is set (serve, servingLatch), on a context that ends when serving
	// does, so a hook that waits never outlives the daemon's stop; nil, in production, is nothing.
	// It is the seam a test holds the daemon at the moment it serves HTTP and reports itself not
	// ready, which is what /readyz answers 503 for.
	beforeServingLatched func(ctx context.Context)
	// pluginRelaunchable overrides whether a stale-plugin claim can be relaunched onto a plugin
	// (supervise.Deps.PluginRelaunchable), bypassing the configuration-derived default: nil keeps
	// that default, which a kubernetes-runtime test without a real cluster has no other way to
	// drive, since such a test configures no runtime.kubernetes.plugins and links no plugin pair.
	pluginRelaunchable *bool
	// checkLaunch stands in for the store's own CheckLaunch (supervise.Store), wrapping the real
	// store so every launch's recheck refuses first when this does, then asks the real store
	// (launchGatedStore, internal/daemon/launchgate.go): a test's way to refuse a relaunch the way
	// a reserved tree cleanup or a transient store error would, without a lifecycle this pod has
	// none of to reserve. nil, in production, is nothing: every launch reaches the real store's
	// own check.
	checkLaunch func(context.Context, supervise.Claim) error
}

// Build is what the binary knows of its own build: Revision, the commit it was built from, set
// only by a release build (cmd/legion's ldflags-stamped revision) and empty in every other. The
// state's `daemon` block names it (api.DaemonInfo.Commit), so a reader can tell which release
// runs; a zero Build names none.
type Build struct {
	Revision string
}

// Run is the daemon. It refuses what it cannot run on before it migrates anything — the
// configuration first, then the cluster fence, then the store's open (refusing by the host it
// could not reach) with the settings route's stored changes laid back over the file, then, under
// tmux, the plugin gate, which holds the Oh My Pi plugin every pane will load to this daemon's
// contract — then migrates, takes its API listener and its worker stream, builds the runtime (under
// kubernetes, once Agent Sandbox's install check passes), proves the worker image under
// kubernetes, records the boot, supervises every claim the store holds, serves the API, and blocks
// until ctx is done. Then it stops: it says so, cancels the decisions it makes itself, drains the
// API and the decisions its routes asked for, ends the workflow and supervision, stamps the boot's
// end and closes the pool, in that order, because the stamp needs the pool. The waits before the
// stamp share one budget, which a boot that has not finished supervising its claims shares too
// (stopBudget).
//
// Everything that can refuse comes before the boot record: a recorded boot is a boot that
// served, and a daemon whose port, socket, tmux, cluster, or image refused it never ran.
//
// ctx decides one thing: how long the daemon serves. The boot record and its stamp are the
// daemon's own bookkeeping and run on a context the shutdown did not cancel, so a signal that
// arrives mid-startup still leaves a recorded, stamped boot rather than a row with no end. A
// signal that arrives while the plugin gate, the repository mapping read, the GitHub App tokens'
// mint, or the image probe waits ends the daemon there, cleanly: it has served nothing and records
// nothing.
//
// build is the binary's own build (Build), the commit the state's `daemon` block names.
//
// listener, when not nil, is the API listener the daemon serves on in place of binding cfg's bind
// and port: the socket `legion start` inherited (LEGION_LISTEN_FD), which it checked names both.
// Run closes it once it stops serving, as it closes a listener it binds; a refusal before the
// listener is taken leaves it to the caller to close.
// live is the daemon's one settings holder (config.Live): the resolved configuration it boots
// on, the route overlay laid over it, and the pointer every reader of a value that can change
// while the daemon runs goes through. Boot reads a snapshot of it wherever it genuinely reads
// once; a decision reads it as the decision is made; and one observer of it builds each holder
// of a changed setting again (rebuilder). Nothing in the daemon resolves the configuration a
// second way.
func Run(ctx context.Context, live *config.Live, build Build, log *slog.Logger, listener net.Listener) error {
	return run(ctx, live, build, log, overrides{listen: handed(listener)})
}

// handed is the listen of a daemon handed listener: it takes listener, whatever address it is
// asked for, and binds nothing. A nil listener hands nothing, and the daemon binds its own address.
func handed(listener net.Listener) func(network, address string) (net.Listener, error) {
	if listener == nil {
		return nil
	}
	return func(string, string) (net.Listener, error) { return listener, nil }
}

// bootBudget is a budget of bootTimeout for the boot's next steps, on ctx's values but not its
// cancellation: a signal that arrives mid-startup still leaves a recorded, stamped boot (Run), so
// the steps that lead to the boot record end on this bound rather than on the signal, and the
// steps after it end on the signal through starting, which run derives from it. It carries the
// boot's hold mark (store.WithTransactionTracking), since the boot's steps run one after another
// on it: a step that asks the pool for a connection while one of its own transactions holds one
// is refused at that call (store.ErrNestedAcquire) rather than waiting out a boot that can no
// longer finish.
func bootBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(store.WithTransactionTracking(context.WithoutCancel(ctx)), bootTimeout)
}

func run(ctx context.Context, live *config.Live, build Build, log *slog.Logger, o overrides) error {
	if log == nil {
		log = slog.Default()
	}
	// The cluster's refusals come first, before the daemon dials anything: Agent Sandbox must be
	// installed, and no per-claim Sandbox of the layout before issue pods may remain. They are
	// read-only and build nothing, and they run against the file's own configuration, which is
	// all the daemon holds until its store is open (clusterFence). An operator whose cluster
	// still carries that layout is told so, rather than shown whatever the daemon met next. The
	// fence has a budget of its own, as the store's open and restore below have theirs.
	fencing, cancelFencing := bootBudget(ctx)
	defer cancelFencing()
	fence, err := clusterFence(*live.Snapshot(), o, log)
	if err != nil {
		return err
	}
	if fence != nil {
		if err := fence(fencing); err != nil {
			return err
		}
	}
	// The store opens next, and before anything is built from the configuration: the settings
	// changes this daemon took from its route are laid back over the file here, so the holders
	// the boot then builds, the credentials it reads and the launch specs it settles are built
	// from what the daemon is actually running on, and not from a file a change of that key sits
	// above (internal/settings' Keeper.Restore). Nothing of the schema is written yet — the
	// claims' half of the layout fence and Migrate both still come later.
	opening, cancelOpening := bootBudget(ctx)
	defer cancelOpening()
	st, err := store.Open(opening, live.Snapshot().PostgresDSN)
	if err != nil {
		return err
	}
	keeper := settings.New(live, settings.NewStore(st.Pool(), live.Snapshot().Project), log)
	restored, err := keeper.Restore(opening)
	if err != nil {
		st.Close()
		return err
	}
	// The boot-time value, read once the route overlay is back over the file: every read that
	// happens once — the sockets, the state directory, the runtime's own options — is this one,
	// and every reader that must see a change goes through live instead.
	cfg := *live.Snapshot()
	rebuild := newRebuilder(log, live)
	live.OnChange(rebuild.observe)
	if o.settingsRebuilder != nil {
		o.settingsRebuilder(rebuild)
	}
	plan, err := prepare(cfg, log, o)
	if err != nil {
		st.Close()
		return err
	}
	plan.settings, plan.restored = keeper, restored
	plan.live, plan.rebuild, plan.commit = live, rebuild, build.Revision
	if cfg.Runtime.Name == "kubernetes" {
		logPlugins(log, plan.plugins, plan.pluginsSource)
		logProbeBudget(log, cfg)
	}
	if cfg.DispatchURL != "" {
		log.Info("legion workflow boot stage", "stage", "prompts")
	}
	if plan.gate != nil {
		if err := plan.gate(ctx); err != nil {
			st.Close()
			if ctx.Err() != nil {
				log.Info("legion daemon stopped before its plugin gate passed", "project", cfg.Project)
				return nil
			}
			return err
		}
	}

	// The boot's next budget starts once the plugin gate has passed, which no budget bounds
	// (bootTimeout). The loops the boot starts take marks of their own, since they run beside each
	// other (store.ForConcurrentWorker).
	boot, cancelBoot := bootBudget(ctx)
	defer cancelBoot()
	// The claims' half of the layout fence, before the schema is migrated: no stored claim may
	// still carry a per-claim Sandbox locator.
	if plan.claimsCheck != nil {
		if err := plan.claimsCheck(boot, st); err != nil {
			st.Close()
			return err
		}
	}
	applied, err := st.Migrate(boot)
	if err != nil {
		st.Close()
		return err
	}
	if cfg.DispatchURL != "" {
		log.Info("legion workflow boot stage", "stage", "store")
	}
	// One limiter for the whole daemon, built before either lane that reserves from it: every
	// wake the daemon causes — the role processes supervision launches, the stale-plugin lane's
	// relaunches among them, and the catch-up the workflow engine writes in place of a launch
	// — draws from the same rate, because what the organization's cap counts is the uncached
	// input of the minute after the wake, whichever lane caused it. Its jitter is a draw from
	// [0, interval), the limiter's own interval: without one, two daemons restarted together
	// would let their wakes out on the same instants, and the organization they share would see
	// both bursts in one minute. A rate of zero is the pacing off, and each lane is handed no
	// Reserve at all rather than one that paces nothing, which is how both read the pacing off
	// (workflow.Config.Reserve, supervise.WakePace.Reserve).
	var reserveWake func(time.Time) time.Time
	if cfg.WorkerWakesPerMinute > 0 {
		reserveWake = wakepace.New(cfg.WorkerWakesPerMinute, cfg.WorkerWakeBurst, rand.N[time.Duration]).Reserve
	}
	// The one gate of the stale-plugin refresh lane, built here because K is that same rate: the
	// lane may hold one minute of the pacing at a time, and the sweep a minute later admits the
	// next minute's worth (supervise.NewRefreshGate, which says what a lane without it costs).
	// It is built at every rate, the pacing off included, where it grants every request: one
	// gate to hand the machines and the refresh pass, and one place that reads a rate of zero.
	refreshGate := supervise.NewRefreshGate(cfg.WorkerWakesPerMinute)
	// The mapping read waits out an unreachable Dispatch without limit, and the App mint waits out
	// GitHub's transient failures within appMintRetry; a repository whose installation read fails
	// is left out and read again, never a refused boot. The boot budget bounds none of them, as it
	// does not bound the plugin gate or the image probe: the work after them has a budget of its own.
	workflow, err := openWorkflow(ctx, cfg, live, rebuild, st, plan.project, plan.dispatchToken, plan.hookPrompts, plan.repositories, log, o.workflowTokens, o.githubAPI, reserveWake)
	if err != nil {
		st.Close()
		if ctx.Err() != nil {
			log.Info("legion daemon stopped before it read its repository mapping and minted its GitHub App tokens", "project", cfg.Project)
			return nil
		}
		return err
	}
	if workflow != nil {
		var cancelAfterMint context.CancelFunc
		// A fresh budget for the rest of the boot, since the boot budget bounds neither the
		// mapping read nor the App mint above.
		boot, cancelAfterMint = bootBudget(ctx)
		defer cancelAfterMint()
		workflow.controllerFirstWake, workflow.idleTick, workflow.entrySweep = o.controllerFirstWake, o.idleTick, o.entrySweep
		plan.identity = workflow.identity
		plan.repository = workflow.issueRepository
		plan.ensureWorkspace = workflow.ensureWorkspace
		// The memory group is read from the daemon's own store (treeMemoryGroup), not from here:
		// the operator's spawn draws one with no workflow configured, and the launch that follows
		// it must honour the draw it made.
		// Only a daemon with the workflow configured has issue records to read a phase or a tree
		// from; Stage 2's supervision runs on claims alone, where every delivery holds and every
		// tree closes.
		plan.phaseHolds, plan.rootHears, plan.treeClosable = workflow.phaseHolds, workflow.rootHears, workflow.treeClosable
		plan.rootTask = workflow.rootTask
		plan.taskConfirmed = workflow.taskConfirmed
		plan.sessionLost, plan.sessionStoreFault = workflow.sessionLost, workflow.sessionStoreFault
		plan.providerErrorRunReached = workflow.providerErrorRunReached
	}
	if cfg.Runtime.Name == "tmux" {
		executable, err := os.Executable()
		if err != nil {
			workflow.stop()
			st.Close()
			return fmt.Errorf("resolve this daemon's executable for the pane legion launcher: %w", err)
		}
		if err := workerbin.Install(cfg.StateDir, executable); err != nil {
			workflow.stop()
			st.Close()
			return err
		}
		if err := reconfigureCloneCredential(boot, cfg, plan.tools, log); err != nil {
			workflow.stop()
			st.Close()
			return err
		}
		if workflow != nil {
			log.Info("legion workflow boot stage", "stage", "launcher")
		}
	}
	// The API serves on the socket it was handed (Run's listener) or binds cfg's address here.
	address := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))
	listen := net.Listen
	if o.listen != nil {
		listen = o.listen
	}
	listener, err := listen("tcp", address)
	if err != nil {
		workflow.stop()
		st.Close()
		return fmt.Errorf("listen on %s: %w", address, err)
	}

	var apps appauth.Tokens
	if workflow != nil {
		apps = workflow.tokens
	}
	s, err := openSupervision(boot, cfg, log, plan, st, apps, reserveWake, refreshGate)
	if err != nil {
		listener.Close()
		workflow.stop()
		st.Close()
		return err
	}
	if plan.probe != nil {
		report, err := plan.probe(ctx, s.runtime)
		if err != nil {
			s.stop()
			listener.Close()
			workflow.stop()
			st.Close()
			if ctx.Err() != nil {
				log.Info("legion daemon stopped before its worker image passed its probe", "project", cfg.Project)
				return nil
			}
			return err
		}
		s.imageReport, s.probed = report, true
		// The probe waits out a cold node and an image pull, which the boot budget does not bound,
		// as it does not bound the plugin gate: the work after the probe has a budget of its own.
		var cancelAfterProbe context.CancelFunc
		boot, cancelAfterProbe = bootBudget(ctx)
		defer cancelAfterProbe()
	} else if plan.modelFallback != nil {
		// The tmux counterpart of the probe's model-fallback mark, read from the host's Oh My Pi
		// under the gate's environment now that the gate has passed.
		s.imageReport.ModelFallback = plan.modelFallback(ctx)
	}
	// The deployment's capability report (LEGION-578, "The check"): a gap is logged here, shown in
	// `legion state` and named to the controller on every tick, and never refused.
	s.reportCapabilities()
	if workflow != nil {
		workflow.admission.ReportCapabilities(s.reportCapabilities)
		// The durable consumers exist before the listing is read: a consumer created now delivers
		// only what is published after it, so everything earlier is the listing's, and what the
		// listing misses (a move published while it is read) the consumer delivers. Both wait out
		// an unreachable dependency on ctx, not the bounded boot budget just spent on everything
		// before them, through the same bootprobe.Run mechanism mintAtBoot already uses, here with
		// Attempts left at its unbounded zero. natsauth.Unreachable and dispatch.Unreachable each
		// judge their own dependency; their own docs are the record of what each one waits on and
		// what it still refuses loud. reconcile's own Postgres transaction is judged by neither: a
		// design choice, not an inability to tell its failures apart from NATS's or Dispatch's — an
		// unreachable Postgres refuses earlier, at store.Open
		// (TestRunRefusesAnUnreachablePostgresByHostAndNotByPassword, scripts/e2e/stage1-skeleton.sh).
		plan.nats.log(log)
		// The three probes run one after another on this goroutine, so they share one hold mark
		// (a sequential flow); whether the daemon is stopping is still read from ctx.
		probing := store.WithTransactionTracking(ctx)
		err := bootprobe.Run(probing, "connect Envoy NATS", readinessRetry, log,
			readinessAttempt(func(attempt context.Context) error {
				return workflow.connect(attempt, *live.Snapshot(), rebuild, environLookup(plan.environ))
			}, bootTimeout, natsauth.Unreachable))
		if err == nil {
			err = bootprobe.Run(probing, "list Dispatch issues for admission", readinessRetry, log,
				readinessAttempt(workflow.reconcile, bootTimeout, dispatch.Unreachable))
		}
		// The catch-up of the roots that run or wait on a person is its own probe, after the
		// reconcile that held them (holdForCatchUp). It reads Dispatch once or twice per root (the
		// issue's latest sequence, its event log after the root's cursor, and the approval of a
		// gate whose document the asks' mirror holds no request of), so it bounds each root by bootTimeout itself and keeps across
		// retries the roots it finished (catchUpAwaiting); its attempt's own bound only stops a
		// catch-up that would never end. The durable consumers start only after it, so no live
		// fact meets a root still held.
		if err == nil {
			err = bootprobe.Run(probing, "catch up the roots that run or wait on a person", readinessRetry, log,
				readinessAttempt(workflow.catchUpAwaiting, catchUpAttemptBound, dispatch.Unreachable))
		}
		if err != nil {
			s.stop()
			listener.Close()
			workflow.stop()
			st.Close()
			if ctx.Err() != nil {
				log.Info("legion daemon stopped before its workflow dependencies were reachable", "project", cfg.Project)
				return nil
			}
			return err
		}
		log.Info("legion workflow boot stage", "stage", "admission")
		workflow.attach(s)
		// Both waited out whatever they waited out on ctx, which the boot budget does not bound:
		// what follows gets a budget of its own, as the App mint and the image probe's callers
		// already do, for the boot record and supervision's start (starting).
		var cancelAfterReady context.CancelFunc
		boot, cancelAfterReady = bootBudget(ctx)
		defer cancelAfterReady()
	}

	startedAt := time.Now().UTC()
	bootID, err := st.RecordBoot(boot, cfg.Project, startedAt)
	if err != nil {
		s.stop()
		listener.Close()
		workflow.stop()
		st.Close()
		return err
	}
	// From here every way the daemon ends halts supervision before anything waits: the signal, at
	// once, serve's end, or a start that failed. The signal also ends the boot's own steps, which run
	// on starting. Everything after the boot record runs on a goroutine of its own, so run waits for
	// it only until the stop's budget runs out (awaitStop), and then stamps the boot whatever is
	// still running.
	starting, endStarting := context.WithCancel(boot)
	defer endStarting()
	halting := context.AfterFunc(ctx, func() {
		s.halt(context.Cause(ctx))
		endStarting()
	})
	defer halting()
	failed := make(chan error, 1)
	lived := make(chan error, 1)
	// The replay runs on ctx with a budget per fact (replayTerminal), not on starting: the boot's
	// budget would end it on the number of facts no daemon applied. It is one more of the boot's
	// sequential steps, on start's goroutine after the restore, so its context carries a hold mark.
	var settle func() error
	if workflow != nil {
		settle = func() error {
			return workflow.replayTerminal(store.WithTransactionTracking(ctx), s.claims, bootTimeout)
		}
	}
	go func() {
		err := s.start(starting, settle)
		switch {
		case ctx.Err() != nil:
			// The signal came while the daemon booted, and cut its own steps short or would have: the
			// daemon is stopping, not failing to start, and it never started.
			log.Info("legion daemon stopped its boot short", "project", cfg.Project, "boot", bootID, "error", err)
			err = nil
			listener.Close()
		case err == nil:
			log.Info("legion daemon started",
				"project", cfg.Project,
				"address", listener.Addr().String(),
				"workerStream", s.stream.Addr(),
				"runtime", cfg.Runtime.Name,
				"admissionCap", cfg.AdmissionCap,
				"migrationsApplied", applied,
				"claims", s.supervisor.count(),
				"boot", bootID,
			)
			if workflow != nil {
				log.Info("legion workflow boot stage", "stage", "api")
			}
			err = serve(ctx, cfg, st, startedAt, bootID, listener, s, plan, workflow)
		default:
			failed <- err
			s.halt(err)
			listener.Close()
		}
		s.stop()
		workflow.stop()
		lived <- err
	}()
	liveErr := s.awaitStop(lived, failed, bootID)
	// awaitStop can return through its deadline arm while the goroutine above is still inside
	// supervision.stop, whose wait is unbounded (stop's own comment), so the connection the stop
	// asked workflowRuntime.stop to close is still open when run returns, and the daemon dials
	// under natsauth.ReconnectForever, which never gives a connection up by itself. Only the
	// connection is closed here, not the whole of workflowRuntime.stop: that also closes the token
	// manager, whose Close waits for every running token_command with no bound
	// (internal/appauth/tokencommand.go, Manager.Close), while the daemon's stop contract is that
	// it returns at most stopBudget+natsCloseTimeout+stampTimeout+storeCloseTimeout (24 s) after
	// its stop begins, inside a pod's 30 s grace. The close itself is bounded as well
	// (closeWorkflowNATS), because nats.Conn.Close flushes the connection's outbound buffer under
	// its own lock with the default minute's write deadline, which a server that has stopped
	// reading would otherwise hold the stop in, long past that contract. The close is harmless
	// when the goroutine later reaches workflowRuntime.stop, because closing the connection is
	// idempotent.
	closeWorkflowNATS(workflow, log)

	// The stop cancelled store queries in flight, and a pooled connection one of them ran on can
	// still carry the read deadline that cancellation set, which failed the stamp with `stop boot N:
	// timeout: read tcp …: i/o timeout` (a socket deadline, not the stamp's context). The stamp runs
	// on a connection opened after the reset.
	st.Pool().Reset()
	stamp, cancelStamp := context.WithTimeout(context.WithoutCancel(ctx), stampTimeout)
	defer cancelStamp()
	stopErr := st.StopBoot(stamp, bootID, time.Now().UTC())
	closeStore(st, log)
	log.Info("legion daemon stopped", "project", cfg.Project, "boot", bootID)

	return errors.Join(liveErr, stopErr)
}

// awaitStop waits, once the stop has begun (stopping), for the daemon's own work to end and report
// on lived, until the stop's budget runs out (stopBy). Then it logs each claim still deciding and
// leaves it to the next boot; a start that failed, reported on failed before the stop began, is
// returned either way.
func (s *supervision) awaitStop(lived, failed <-chan error, bootID int64) error {
	<-s.stopping.Done()
	deadline := time.NewTimer(time.Until(s.stopBy))
	defer deadline.Stop()
	select {
	case err := <-lived:
		return err
	case <-deadline.C:
	}
	s.log.Warn("legion daemon stopped waiting for its work", "project", s.cfg.Project, "boot", bootID,
		"after", s.plan.stopBudget.String(), "deciding", s.supervisor.inDecision())
	select {
	case err := <-failed:
		return err
	default:
		return nil
	}
}

// closeWorkflowNATS closes the workflow's Envoy NATS connection and nothing else
// (workflowRuntime.closeNATS), waiting for it up to natsCloseTimeout: nats.Conn.Close flushes the
// outbound buffer under the connection's own lock, with a write deadline of a minute by default,
// which the stop has no room for (natsCloseTimeout). It is safe on a nil workflow, as
// workflowRuntime.closeNATS is.
func closeWorkflowNATS(workflow *workflowRuntime, log *slog.Logger) {
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		workflow.closeNATS()
	}()
	timer := time.NewTimer(natsCloseTimeout)
	defer timer.Stop()
	select {
	case <-closed:
	case <-timer.C:
		log.Warn("legion daemon stopped waiting for its NATS connection to close", "after", natsCloseTimeout.String())
	}
}

// closeStore closes the store, waiting for it up to storeCloseTimeout: the pool's close waits for
// every connection a query holds, and a query the stop stopped waiting for may be waiting on
// Postgres.
func closeStore(st *store.Store, log *slog.Logger) {
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		st.Close()
	}()
	timer := time.NewTimer(storeCloseTimeout)
	defer timer.Stop()
	select {
	case <-closed:
	case <-timer.C:
		log.Warn("legion daemon stopped waiting for its store to close", "after", storeCloseTimeout.String())
	}
}

// plan is what the daemon resolved from its configuration before touching anything.
type plan struct {
	// live is the daemon's settings holder, and rebuild the one observer of it that builds each
	// holder of a changed setting again. run sets both once prepare has returned, so everything
	// the boot builds after that registers its holder with the rebuilder and reads what it must
	// read live through live.
	live    *config.Live
	rebuild *rebuilder
	// settings is the daemon's two settings arms over live: the route's (the settings route's
	// ApplySettings) and the file's (the watcher's), and the store that keeps what the route
	// changed. restored is what Restore laid back over the file at this boot, which the state's
	// provenance takes as the source of those paths (api.SettingProvenance.Restore).
	settings *settings.Keeper
	restored []settings.Change
	// commit is the commit this binary was built from (Build.Revision), which the state's daemon
	// block names so a reader can tell which release runs; "" in a build that is not a release.
	commit string
	// environ is the environment the daemon's own reads resolve under (overrides.environment):
	// the launch secrets a rebuilt launch spec reads again are read under it, as boot's were.
	environ []string
	// identity is the role's App bot identity for the owner whose installation holds the
	// repository it works, from the workflow's token source (workflowRuntime.identity, which maps
	// the record's repository to the one GitHub serves); nil without one.
	identity func(ctx context.Context, role claim.Role, repository ghrepo.Repository) (runtime.GitIdentity, error)
	// repository is the repository an issue works and its default branch
	// (workflowRuntime.issueRepository: its record's, or the project's default for an issue the
	// workflow does not record); nil without a workflow.
	repository func(ctx context.Context, issue string) (ghrepo.Repository, string, error)
	// ensureWorkspace provisions an issue's workspace on the daemon's host when a launch finds it
	// missing or not checked out (workflowRuntime.ensureWorkspace); nil without a workflow.
	ensureWorkspace func(ctx context.Context, issue string, repo ghrepo.Repository, defaultBranch string) error
	// repositories is the project's repository mapping as the workflow serves it, empty until the
	// workflow reads it at boot: the set every role's gh bundle is built over, a directory per
	// owner of it and one per repository the pod's own owner serves beside the one it writes to
	// (appauth.GitHubCredential), and, under the Kubernetes runtime, the mapping a provisioning
	// mint reads the rename rule from (sandboxRuntime, repositories.serving).
	repositories *repositories
	// phaseHolds, rootHears, rootTask and treeClosable are the workflow's answers to the
	// supervisor's predicates; nil without a workflow, where every delivery is sent as it was
	// queued and every tree closes.
	phaseHolds   func(ctx context.Context, issue string, p phase.Phase, generation uint64) (bool, error)
	rootHears    func(ctx context.Context, c supervise.Claim, d supervise.Delivery) (bool, error)
	rootTask     func(ctx context.Context, c supervise.Claim, d supervise.Delivery) (string, []byte, error)
	treeClosable func(ctx context.Context, c supervise.Claim) (bool, error)
	// taskConfirmed tells the workflow each agent that took its task; nil without a workflow.
	taskConfirmed func(c supervise.Claim)
	// sessionLost and sessionStoreFault tell the workflow that a claim's recorded session is in no
	// session store any more, and that a start could not reach that store at all; nil without a
	// workflow, where neither is reported to anyone.
	sessionLost       func(c supervise.Claim, detail string)
	sessionStoreFault func(c supervise.Claim, detail string)
	// providerErrorRunReached tells the project's controller of a claim whose turns keep ending
	// in a provider error; nil without a workflow, where nobody is told.
	providerErrorRunReached func(c supervise.Claim, reason string)
	// tools are the git and jj boot resolved on the host, by name, for workspace provisioning;
	// nil without a repository, and under a runtime whose agents run the worker image's own.
	tools         map[string]string
	project       string
	operatorToken string
	secrets       map[string]string
	// nats is the user the daemon's own NATS connection authenticates as (natsConnection); no launch
	// carries its seed unless it is the pane seed.
	nats         natsConnection
	instructions string
	// memoryInstructions is the daemon's copy of the project's memory instructions
	// (config.WriteMemoryInstructions), the last fragment of a memory-group launch's system
	// prompt; "" when the project names no memory block or its block names no instructions file.
	memoryInstructions string
	// dispatchToken is the Dispatch bearer dispatch_token_file names; "" without Dispatch.
	dispatchToken string
	// hookPrompts are the words boot read for each hook whose `prompt_file` names a file, by
	// hook name (readBoot): the engine is given words and never a path, in the hook list boot
	// builds once from them (openWorkflow, engineHooks).
	hookPrompts map[string]string
	prompts     *prompts.Composer
	// roleReferences are the task agents and skills the shared role prompts name
	// (prompts.RoleReferences), which the gate on either runtime resolves beside the plugin's own.
	roleReferences promptrefs.Names
	// stream is the worker stream's address: the listener binds it, and every agent's shim dials
	// it, or advertise_host at its port when the file sets one (shimAddress).
	stream string
	// runtimeFor is the factory this daemon's runtime is built with, for the configuration it is
	// built from: the boot's own build and every rebuild call it (config.HolderRuntime), so the
	// two can never build different things. Everything it is built from but the configuration —
	// the launchers, the provider key directory, the cluster client's own reads — is resolved at
	// boot and read again where the configuration names it. A test that hands the daemon its
	// runtime answers that one for every configuration.
	runtimeFor func(cfg config.Config) (runtimeFactory, error)
	// gate is the plugin gate, run once the store is open and before the schema is migrated
	// (pluginGate), on no boot budget; nil under a runtime with no host Oh My Pi, and for a
	// replaced runtime without one.
	gate func(ctx context.Context) error
	// probe proves the runtime's worker image once the runtime is built and before the boot is
	// recorded, answering what its OK line reported of the image; nil under tmux, and for a
	// replaced runtime without one.
	probe func(ctx context.Context, rt runtime.Runtime) (bootprobe.ImageReport, error)
	// claimsCheck is the Kubernetes runtime's refusal once the store has opened, before it
	// migrates: no stored claim may still carry a per-claim Sandbox locator of the layout before
	// issue pods (store.HasLegacySandboxClaims). Nil under tmux, and for a replaced runtime.
	claimsCheck func(ctx context.Context, st *store.Store) error
	// modelFallback reads, once the gate has passed, whether the host's Oh My Pi falls back to
	// another model (capabilities.ReadModelFallback), as the probe's OK line reports it for a pod:
	// "on", "off", or "" when the read failed, which it logs and never refuses. nil under
	// kubernetes, whose probe reports it, and for a replaced runtime.
	modelFallback func(ctx context.Context) string
	clock         supervise.Clock
	orphanSweep   time.Duration
	// controllerRetry is the controller keeper's first wait before it retries a failed controller.
	controllerRetry time.Duration
	// stopBudget is how long the stop waits for the daemon's own work once it begins (stopBudget).
	stopBudget time.Duration
	// secretsEnroller is the daemon's agent-secrets token login as the machines' Enroller
	// (newSecretsLogin); nil when the deployment enrolls no pod.
	secretsEnroller supervise.Enroller
	// pluginRelaunchable is supervise.Deps.PluginRelaunchable: whether this daemon can relaunch a
	// stale-plugin claim onto a plugin at all. True under tmux always; under Kubernetes, true iff
	// a plugin pair to install is known — plugins below, resolved by ResolvedPlugins from the
	// file's runtime.kubernetes.plugins or the release's linked pair.
	pluginRelaunchable bool
	// plugins is this daemon's resolved Kubernetes plugin pair (ResolvedPlugins), with Contract
	// already api.DaemonAPIVersion: what specs.SpawnSpec installs for a stale-plugin relaunch, and
	// what the boot log and `legion start --check-config` name. nil under tmux, and under
	// Kubernetes when the daemon knows no pair (LEGION-164's degraded mode); pluginsSource is
	// "legion.yaml" or "binary" with a pair, "" without one.
	plugins       *runtime.PluginInstall
	pluginsSource string
	// secretsLogin is the same login's client, read-only, for the state route to show its
	// current status (source.State, agentsecrets.Client.LoginStatus); nil when the deployment
	// enrolls no pod.
	secretsLogin *agentsecrets.Client
	// strandedReported is overrides.strandedReported; nil runs nothing.
	strandedReported func()
	// beforeServingLatched is overrides.beforeServingLatched; nil runs nothing.
	beforeServingLatched func(ctx context.Context)
	// checkLaunch is overrides.checkLaunch; nil wraps the store with nothing, so every launch
	// reaches the real store's own CheckLaunch.
	checkLaunch func(context.Context, supervise.Claim) error
}

// runtimeFactory builds the runtime over the worker stream (C3): ctx is supervision's lifetime,
// listener the stream listener, address the one every agent's shim dials (shimAddress), tokens the
// workflow's App tokens and repository the workflow's repository of an issue (issueRepository;
// both nil without a workflow), st the store the runtime reads, and mapped the repositories
// Dispatch's mapping names now (plan.repositories, which the workflow fills; none without a
// workflow): the set every role's gh bundle is built over, and the set a configured rename is
// resolved against, one repository at a time (appauth.GitHubCredential), which the Sandbox
// runtime's repository caches follow too. openSupervision hands it the
// plan the workflow completed, so no factory reads a field the workflow sets after prepare.
type runtimeFactory func(ctx context.Context, listener *stream.Listener, address string, tokens appauth.Tokens,
	repository func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, mapped func() []ghrepo.Repository) (runtime.Runtime, error)

// prepare is every refusal that needs nothing but the configuration and the machine (readBoot's,
// then what writes or runs something: the state directory, the instructions copy, and what the
// runtime needs) — all before the plugin gate runs and before any agent can launch.
func prepare(cfg config.Config, log *slog.Logger, o overrides) (plan, error) {
	getenv := o.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	// A test that replaced the runtime runs no host Oh My Pi, so boot resolves no OMP invocation.
	hostOMP := o.runtime == nil
	reads, err := readBoot(cfg, environLookup(o.environment()), getenv, hostOMP, log)
	if err != nil {
		return plan{}, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return plan{}, fmt.Errorf("create state directory %s: %w", cfg.StateDir, err)
	}
	composer, err := prompts.New(cfg.StateDir)
	if err != nil {
		return plan{}, fmt.Errorf("construct role prompts: %w", err)
	}

	instructions := ""
	if reads.instructions != nil {
		if instructions, err = config.WriteDeploymentInstructions(reads.instructions, cfg.StateDir, cfg.Project); err != nil {
			return plan{}, err
		}
	}

	memoryInstructions := ""
	if reads.memoryInstructions != nil {
		if memoryInstructions, err = config.WriteMemoryInstructions(reads.memoryInstructions, cfg.StateDir, cfg.Project); err != nil {
			return plan{}, err
		}
	}

	clock := o.clock
	if clock == nil {
		clock = supervise.RealClock{}
	}
	orphanSweep := o.orphanSweep
	if orphanSweep == 0 {
		orphanSweep = orphanSweepInterval
	}
	controllerRetry := o.controllerRetry
	if controllerRetry == 0 {
		controllerRetry = controllerRetryFirst
	}
	stop := o.stopBudget
	if stop == 0 {
		stop = stopBudget
	}
	secretsEnroller, secretsLogin := newSecretsLogin(cfg, log)
	plugins, pluginsSource := ResolvedPlugins(cfg)
	pluginRelaunchable := cfg.Runtime.Name == "tmux" || plugins != nil
	if o.pluginRelaunchable != nil {
		pluginRelaunchable = *o.pluginRelaunchable
	}
	p := plan{
		repositories: newRepositories(cfg.Project, cfg.RepositoryRenames),
		project:      reads.project, operatorToken: reads.operatorToken, secrets: reads.secrets, nats: reads.nats, instructions: instructions,
		memoryInstructions: memoryInstructions,
		dispatchToken:      reads.dispatchToken, hookPrompts: reads.hookPrompts, prompts: composer, roleReferences: prompts.RoleReferences(),
		tools: reads.tmux.tools, clock: clock, orphanSweep: orphanSweep, controllerRetry: controllerRetry, stopBudget: stop,
		secretsEnroller: secretsEnroller, secretsLogin: secretsLogin, strandedReported: o.strandedReported,
		beforeServingLatched: o.beforeServingLatched,
		pluginRelaunchable:   pluginRelaunchable, plugins: plugins, pluginsSource: pluginsSource, checkLaunch: o.checkLaunch,
		environ: o.environment(),
	}
	if cfg.Runtime.Name == "kubernetes" {
		err = prepareSandbox(cfg, log, o, reads, &p)
	} else {
		err = prepareTmux(cfg, log, o, reads.dispatchToken, reads.tmux, &p)
	}
	if err != nil {
		return plan{}, err
	}
	return p, nil
}

// prepareTmux is what panes on this host need beyond readBoot's: the plugin gate on the OMP
// invocation readBoot resolved, the Dispatch bearer written where every pane reads it, and the
// provider keys resolved from secretsd into the files every pane's shim reads. The worker stream is
// a unix socket under the state directory (decision 2 — no configuration key). The runtime gets a
// Launcher for each backend readBoot resolved (tmuxReads.launches); the gate and the model-fallback
// read are Oh My Pi's, since the architect runs on it alone (runtime.Backends).
func prepareTmux(cfg config.Config, log *slog.Logger, o overrides, dispatchToken string, reads tmuxReads, p *plan) error {
	invocation := reads.launches[runtime.BackendOmp].invocation
	p.gate = o.gate
	if o.runtime != nil {
		// A test handed the daemon its runtime: every build, the boot's and every rebuild's,
		// answers that one, since nothing of it comes from the configuration.
		p.runtimeFor = func(config.Config) (runtimeFactory, error) { return o.runtime, nil }
	}
	p.stream = "unix://" + filepath.Join(cfg.StateDir, streamSocket)
	launchers := map[runtime.Backend]tmux.Launcher{}
	for _, backend := range cfg.Backends() {
		read, ok := reads.launches[backend]
		if !ok {
			continue
		}
		launchers[backend] = read.launcher
		if p.runtimeFor == nil {
			read.log(log)
		}
	}
	dispatchTokenFile := ""
	if dispatchToken != "" {
		var err error
		if dispatchTokenFile, err = runtime.WriteDispatchTokenFile(cfg.StateDir, dispatchToken); err != nil {
			return fmt.Errorf("write the pane Dispatch token file: %w", err)
		}
	}
	// Last of the refusals: resolving a human-tier key may cost a YubiKey tap, which a
	// configuration refused a line earlier should never have asked for.
	providerEnvDir, err := config.MaterializeProviderKeys(cfg.ProviderKeys, cfg.StateDir, o.environment(), log)
	if err != nil {
		return err
	}
	if p.runtimeFor == nil {
		env, err := gateEnvironment(os.Environ(), cfg.StateDir, providerEnvDir, cfg.Runtime.Tmux)
		if err != nil {
			return err
		}
		gate := pluginGate{
			env:            env,
			workDir:        cfg.StateDir,
			invocation:     invocation,
			prefix:         cfg.OmpLaunchPrefix,
			timeout:        cfg.SlowCommandTimeout,
			retry:          bootprobe.Daemon,
			contract:       api.DaemonAPIVersion,
			roleReferences: p.roleReferences,
			log:            log,
		}
		image := capabilities.Image{Launch: omplaunch.WithPrefix(cfg.OmpLaunchPrefix, invocation), Env: environPairs(env), WorkDir: cfg.StateDir}
		// The role overlays first: a cheap read whose refusal names the operator's file, before the
		// plugin gate's slower load probes.
		p.gate = func(ctx context.Context) error {
			if err := checkPaneRoleOverlays(ctx, cfg, image, log); err != nil {
				return err
			}
			return gate.verify(ctx)
		}
		p.modelFallback = func(ctx context.Context) string {
			state, err := capabilities.ReadModelFallback(ctx, image)
			if err != nil {
				log.Warn("the daemon could not read whether the host's Oh My Pi falls back to another model; the model-fallback capability is reported as not read", "error", err)
				return ""
			}
			return state
		}
		// Everything but the configuration — the launchers, the provider key directory, the
		// Dispatch token file, the gh boot resolved — is this boot's; the rest is read again from
		// the configuration each build is given, the boot's own included. A configuration that
		// is not this runtime's is refused rather than built from (ownRuntime), as the Sandbox
		// build refuses one (readSandbox).
		p.runtimeFor = func(c config.Config) (runtimeFactory, error) {
			if err := ownRuntime("tmux", c); err != nil {
				return nil, err
			}
			return tmuxRuntime(c, p.project, launchers, providerEnvDir, dispatchTokenFile, p.tools["gh"], p.repositories.Accepted, log), nil
		}
	}
	return nil
}

// checkPaneRoleOverlays is the boot gate's read of the overlays each role names beyond the
// operator's own, as that role's pane will start Oh My Pi: its PI_CONFIG_FILES,
// runtime.tmux.pi_config_files then the role's own files (role_config_files) and, for a workflow
// role of a project whose block names memory overlays, those after them, each file
// read and the role's own each read alone by image's Oh My Pi (capabilities.CheckOverlays), so a
// file a pane's Oh My Pi would refuse refuses the boot, naming the key and the file, rather than
// every launch that names it. Legion names no overlay of its own, so those lists are the whole of
// a pane's PI_CONFIG_FILES. The controller is in no memory group, so it is never probed with
// them; the mode is not read here, since a tree already drawn into the memory group names those
// overlays whatever the mode is now. A run that never reached an exit
// of its own is logged, not refused, as the model-fallback read is.
func checkPaneRoleOverlays(ctx context.Context, cfg config.Config, image capabilities.Image, log *slog.Logger) error {
	var every []string
	if block := cfg.Runtime.Tmux; block != nil {
		every = block.PiConfigFiles
	}
	memory := cfg.Projects[cfg.Project].Memory
	memoryField := "projects." + cfg.Project + ".memory.config_files"
	// probed is each PI_CONFIG_FILES value already read, so one value shared by six roles costs
	// one read of each of its files rather than six.
	probed := make(map[string]struct{}, len(config.RoleConfigRoles))
	for _, role := range config.RoleConfigRoles {
		own := cfg.RoleConfigFiles[role]
		field := "role_config_files." + string(role)
		// A memory-group pane of this role names the memory overlays last, so the gate reads the
		// value that pane would start on rather than the role's half of it.
		if len(memory.ConfigFiles) > 0 && role != claim.RoleController {
			own = slices.Concat(own, memory.ConfigFiles)
			if len(cfg.RoleConfigFiles[role]) == 0 {
				field = memoryField
			} else {
				field += " with " + memoryField
			}
		}
		if len(own) == 0 {
			continue
		}
		// Every workflow role of a memory-group project that names no overlays of its own is
		// given one and the same value, so each distinct value is read once rather than once per
		// role: the runs Oh My Pi is asked for are identical, and so is their verdict. The first
		// role holding a value Oh My Pi refuses is still the role the refusal names, since the
		// loop reaches it before any other role of that value.
		value := strings.Join(own, ":")
		if _, read := probed[value]; read {
			continue
		}
		probed[value] = struct{}{}
		why, err := capabilities.CheckOverlays(ctx, image, slices.Concat(every, own), own)
		if err != nil {
			log.Warn("boot gate: Oh My Pi did not answer on some of a role's overlays; its files were read", "role", role, "error", err)
		}
		if why != "" {
			return fmt.Errorf("boot gate: %s: the %s's Oh My Pi would not start: %s", field, role, why)
		}
	}
	return nil
}

// treeMemoryGroup answers whether tree's root record carries the memory group, the word the draw
// stored on it once and every later launch of the tree reads (record.Issue.MemoryGroup,
// memorygroup.GroupMemory). An issue's memory group is its tree root's, and the root key is the
// claim's own tree, so there is no ancestor walk; a tree with no record, and a root first opened
// before the setting existed, carry no group and run without memory.
//
// It reads the daemon's own store rather than the workflow's, in a transaction of its own as the
// supervisor's predicates read a record: the operator's spawn draws a tree's group through the
// same store with no workflow configured (api's drawMemory), and a launch that could not read
// that draw back would run the tree against a group the daemon had already recorded for it.
func treeMemoryGroup(st *store.Store) func(ctx context.Context, tree string) (bool, error) {
	records := record.NewStore()
	return func(ctx context.Context, tree string) (bool, error) {
		var issue *record.Issue
		err := st.Tx(ctx, func(tx pgx.Tx) error {
			var err error
			issue, err = records.Issue(ctx, tx, tree)
			return err
		})
		if err != nil {
			return false, err
		}
		return issue != nil && issue.MemoryGroup == string(memorygroup.GroupMemory), nil
	}
}

// tmuxRuntime builds the tmux runtime over the worker stream: the listener is its connection
// directory, and the listener's address is the `--connect` every pane's shim is started with;
// providerEnvDir, when set, is the `--provider-env-dir` beside it. The workflow's App tokens, when
// the daemon has them, are every tree pane's gh bundle (appauth.GitHubCredential) and the
// controller pane's own (appauth.ControllerCredential), which its refresh asks by the default
// owner its launch recorded, both built over
// repos (the project's mapping's served repositories), with gh, the real gh boot resolved
// (daemonTools), the one each pane's `legion gh` runs, and the repository each claim's issue
// works (claimRepository over the workflow's repository), which a pane adopted after a restart
// holds its write token for; a daemon with no GitHub Apps hands the runtime none, and its panes
// hold no gh files. The private server's environment is scrubbed before anything is launched on
// it.
func tmuxRuntime(cfg config.Config, project string, launchers map[runtime.Backend]tmux.Launcher, providerEnvDir, dispatchTokenFile, gh string, repos func() []ghrepo.Repository, log *slog.Logger) runtimeFactory {
	return func(ctx context.Context, listener *stream.Listener, streamAddress string, tokens appauth.Tokens,
		repository func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
		opts := tmuxOptions(cfg, project, launchers, providerEnvDir, dispatchTokenFile, log)
		opts.StreamAddress, opts.Conns = streamAddress, listener
		if tokens != nil {
			// The one record of what the mints found GitHub refuses, shared by both credentials
			// as the Kubernetes runtime's three share one (appauth.LeftOut, kubernetes.go's
			// sandboxRuntime): a pane projects nothing, so nothing reads it here, and a
			// directory left out of one pane's bundle is left out of the next pane's by the
			// mint, not by this record.
			left := appauth.NewLeftOut()
			opts.GitHubCredential, opts.GH = appauth.GitHubCredential(tokens, repos, cfg.RepositoryRenames, left, log), gh
			opts.ControllerCredential = appauth.ControllerCredential(tokens, repos, cfg.RepositoryRenames, left, log)
			opts.ClaimRepository = claimRepository(st.ClaimIssue, repository)
		}
		rt, err := tmux.New(opts)
		if err != nil {
			return nil, err
		}
		removed, err := rt.ScrubServerEnvironment(ctx)
		if err != nil {
			return nil, fmt.Errorf("scrub the private tmux server's environment: %w", err)
		}
		if len(removed) > 0 {
			log.Info("tmux runtime: removed variables the pane environment does not carry", "removed", removed)
		}
		return rt, nil
	}
}

// claimRepository is the repository a claim's issue works: the claim's issue (issueOf, the
// daemon's own claims; a token spells its issue lowercased), then the repository it works
// (repository, the workflow's issueRepository: its record's, or the project's default for an
// issue the workflow does not record). It is what a tmux pane adopted after a restart holds its
// write token for (tmux.Options.ClaimRepository), the repository its launch was minted for, read
// from the daemon's own state and nothing a pane can write.
func claimRepository(issueOf func(context.Context, claim.Token) (string, error),
	repository func(context.Context, string) (ghrepo.Repository, string, error)) func(context.Context, claim.Token) (ghrepo.Repository, error) {
	return func(ctx context.Context, token claim.Token) (ghrepo.Repository, error) {
		issue, err := issueOf(ctx, token)
		if err != nil {
			return ghrepo.Repository{}, err
		}
		repo, _, err := repository(ctx, issue)
		if err != nil {
			return ghrepo.Repository{}, err
		}
		return repo, nil
	}
}

// tmuxOptions translates the configuration into the tmux runtime's Options, all but the worker
// stream and the GitHub credential function, which boot hands the factory: the `runtime.tmux`
// block's pane variables, Oh My Pi overlays and take_over among them, each role's own overlays
// (role_config_files), and the project's memory overlays (`memory.config_files`), handed over
// whatever the mode is: the mode decides which new trees are drawn into the memory group, and a
// tree already drawn into it names those overlays under any mode. A project with no block hands
// over none, which is the pane every deployment built before the block existed.
func tmuxOptions(cfg config.Config, project string, launchers map[runtime.Backend]tmux.Launcher, providerEnvDir, dispatchTokenFile string, log *slog.Logger) tmux.Options {
	opts := tmux.Options{
		Project:           project,
		StateDir:          cfg.StateDir,
		DaemonURL:         cfg.DaemonURL,
		EnvoyURL:          cfg.EnvoyURL,
		NatsURLs:          cfg.NatsURLs,
		DispatchURL:       cfg.DispatchURL,
		DispatchTokenFile: dispatchTokenFile,
		Launchers:         launchers,
		StopGrace:         cfg.WorkerStopTimeout,
		ProbeInterval:     cfg.ProbeInterval,
		AdoptTimeout:      cfg.SlowCommandTimeout,
		ProviderEnvDir:    providerEnvDir,
		RoleConfigFiles:   cfg.RoleConfigFiles,
		Log:               log,
	}
	if block := cfg.Runtime.Tmux; block != nil {
		opts.PaneEnv, opts.PiConfigFiles, opts.TakeOver = block.PaneEnv, block.PiConfigFiles, block.TakeOver
	}
	opts.MemoryConfigFiles = cfg.Projects[cfg.Project].Memory.ConfigFiles
	return opts
}

// reconfigureCloneCredential brings every shared clone an earlier daemon provisioned under the state
// directory, one per repository, to the helper every clone gets now
// (workspace.ConfigureRepositoryCredential): a clone provisioned before each pane read GitHub from
// its own gh files names that daemon's `legion credential` by the pane launcher's path, which
// answers no pane now, so its first push would fail. It runs the git boot resolved (tools), as the
// outbox's provisioning does, and logs each clone it rewrote. A state directory with no clone yet
// is nothing to do: provisioning writes the helper into a clone it makes.
func reconfigureCloneCredential(ctx context.Context, cfg config.Config, tools map[string]string, log *slog.Logger) error {
	if tools == nil {
		return nil
	}
	clones, err := sharedClones(cfg.StateDir)
	if err != nil {
		return err
	}
	for _, clone := range clones {
		changed, err := workspace.ConfigureRepositoryCredential(ctx, workspace.NewRunner(workspace.CommandTimeout, tools), clone.Clone)
		if err != nil {
			return fmt.Errorf("set the shared clone %s's git credential helper: %w", clone.Clone, err)
		}
		if changed {
			log.Info("legion daemon set the shared clone's git credential helper", "clone", clone.Clone, "helper", workspace.GitHubCredentialHelper)
		}
	}
	return nil
}

// sharedClones is every shared clone a daemon provisioned under stateDir, one per repository,
// with the repository as the clone's path spells it, in path order. A directory of the layout that
// holds no git checkout is not one.
func sharedClones(stateDir string) ([]workspace.Workspace, error) {
	paths, err := filepath.Glob(filepath.Join(workspace.CloneRoot(stateDir), "*", "*"))
	if err != nil {
		return nil, fmt.Errorf("list the shared clones: %w", err)
	}
	var clones []workspace.Workspace
	for _, path := range paths {
		if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read the shared clone %s: %w", path, err)
		}
		repo, err := ghrepo.Parse("the shared clone "+path, filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path))
		if err != nil {
			return nil, err
		}
		clones = append(clones, workspace.Workspace{Clone: path, Repo: repo})
	}
	return clones, nil
}

// supervision is everything that runs a claim: the worker stream, the runtime, and the machines,
// with the goroutines that feed them.
type supervision struct {
	// cfg is the configuration boot settled, which everything boot settled is read from: the
	// project, the state directory, the runtime it supervises in. A value a change must reach
	// at once is read through live instead, never from here.
	cfg        config.Config
	log        *slog.Logger
	plan       plan
	stream     *stream.Listener
	runtime    runtime.Runtime
	supervisor *supervisor
	tokens     *api.BootTokens
	claims     []supervise.Claim
	// refreshGate is the daemon's one stale-plugin refresh gate, the same object every machine of
	// this supervision holds on its Deps (supervise.RefreshGate, built in run and handed to
	// openSupervision beside the wake limiter). Supervision keeps the reference because the
	// refresh pass is its own: each sweep corrects the gate from the wakes the machines publish
	// (waitingWakes) and then asks it for each stale claim it is about to post
	// (refreshStalePlugins). Every supervision holds one, never nil: a test that builds one by
	// hand gives it supervise.NewRefreshGate(0), the gate of a daemon that paces nothing, which
	// grants every refresh.
	refreshGate supervise.RefreshAdmitter
	// imageReport is what the worker image's passed probe reported: of the image, from its OK line,
	// and of the pool, the attempts that waited on its capacity (bootprobe.ImageReport); under
	// tmux, the model-fallback mark alone, read by plan.modelFallback. probed is whether the probe
	// passed (kubernetes), so the image rows of the capability report read present.
	imageReport bootprobe.ImageReport
	probed      bool
	// reportMu guards reportedGaps and reports: the tick's reads, the API's readies and boot's run
	// on different goroutines. reportedGaps are the open capabilities the last report logged
	// (reportCapabilities). reports is each claim's latest capability report (LEGION-663), as its
	// session's ready carried it (capabilityReported) or the store held it at boot (openSupervision),
	// whatever process reported: deployment fences each against its claim's current process.
	reportMu     sync.Mutex
	reportedGaps []string
	reports      map[claim.Token]capabilities.Report
	// podUpdateMu guards podUpdateOpenedAt and podUpdateOpened, the whole of what the update-round
	// sweep keeps between ticks (reconcilePodUpdates): when this daemon last opened a pod round,
	// and the rounds it has opened whose claims have not recorded one yet, by issue. The two pace
	// the sweep's openings under `pod_update_rounds_per_minute` and `pod_update_max_open_rounds`;
	// nothing of either is in the store, because neither decides a round, and a daemon that has
	// just booted opens its first round at its first sweep. The sweep is one goroutine, and the
	// mutex is what makes that a fact of this code rather than of its caller.
	podUpdateMu       sync.Mutex
	podUpdateOpenedAt time.Time
	podUpdateOpened   map[string]string
	// podUpdateRoundsMu guards podUpdateRounds, what the last sweep decided under those two limits
	// as the operator's claim listing serves it (api.PodUpdateRounds, podUpdateRoundsView): the
	// issues that held an open round and the issues each limit held back. It is the sweep's own
	// word and no claim carries it — an issue a limit held back is posted nothing at all — and it
	// is kept in the shape the route answers, so nothing between the sweep and the wire translates
	// it. A mutex of its own, because the API's reader is another goroutine than the sweep and
	// neither waits on what the other is doing.
	podUpdateRoundsMu sync.Mutex
	podUpdateRounds   api.PodUpdateRounds
	// reportStore persists each report as it arrives (store.Store), so a restart renders the
	// sessions it re-adopts without waiting for them to report again.
	reportStore capabilityReportStore
	// enqueue writes outbox rows of an issue, all of them in one transaction, the workflow's own
	// enqueue (workflowRuntime.enqueueOutbox); nil in a deployment with no workflow, which has no
	// outbox to write to and no Dispatch issue to write on. It is the whole of the record the
	// update-round sweep needs (reconcilePodUpdates): the controller notice it publishes and the
	// message it writes on the issue are both outbox rows, and threading the workflow engine into
	// supervision to post two rows would tie every claim decision to the workflow. Those two go in
	// one call, since a stall told the controller but not the issue would be told again at every
	// tick (tellUpdateStalled). The workflow sets it where it attaches, before supervision
	// restores or launches any claim, as it sets the supervisor's terminal callback there.
	enqueue func(ctx context.Context, issue string, payloads ...record.OutboxPayload) error
	// applySweepFacts applies the facts a sweep of supervision read, through the workflow's own
	// handlers (workflowRuntime.applySweepFacts); nil in a deployment with no workflow, which
	// records no issue a fact could be about. It is the whole of what the silence sweep needs of
	// the workflow (tellSilentPhaseWorkers): the sweep filters on the live claims, and every
	// decision about a silence is the handler's, inside the fact's own
	// transaction (workflow.(*Engine).workerSilent). The facts of one sweep go in one call so the
	// ids it minted can be read against what this daemon has applied in one query rather than one
	// transaction each. The workflow sets it where it attaches, before supervision restores or
	// launches any claim, as it sets enqueue above.
	applySweepFacts func(ctx context.Context, facts []sweptFact) error
	// backstopMu guards backstopOwed: the owner notices of turns the stall backstop ended that
	// the store has refused, by claim, waiting for the next sweep to write them
	// (stallBackstopped, tellStallsOwed). A mutex, because the machine's own goroutine records
	// one and the sweep writes it. Nothing of it goes in the store, as the pod-update sweep's
	// own pace does not (podUpdateOpenings), and one claim owes at most one: a later stall of
	// the same claim carries the record its owner wants.
	backstopMu   sync.Mutex
	backstopOwed map[claim.Token]stallNoticeOwed

	cancel context.CancelFunc
	// draining is the worker stream's life and what the API's routes run their decisions on
	// (api.Options.Drained), one context because they end together: when the API's drain does
	// (endDrain). decided is the claims those routes are deciding (api.RouteDecisions), whose
	// connections the halt keeps open. stopping ends when the stop begins (halt), for awaitStop and
	// the API's routes (api.Options.Stopping); stopBy is when the stop must end by, and drainBy when
	// the API's drain must, both set before.
	draining  context.Context
	endDrain  context.CancelFunc
	decided   *api.RouteDecisions
	stopping  context.Context
	beginStop context.CancelFunc
	stopBy    time.Time
	drainBy   time.Time
	haltOnce  sync.Once
	wg        sync.WaitGroup
	stopOnce  sync.Once
}

// live is the configuration the daemon is running on now (plan.live): where every decision of
// this supervision reads a value the class table answers `at once` for — the update round's
// notice bound and the deployment's recorded capability decisions — so an operator's change
// reaches the next sweep and the next read of the state with nothing rebuilt.
//
// Every supervision is built with that holder, a test's own as much as the daemon's: cfg is
// what boot settled and nothing moves it, so reading a live value from it would answer the
// boot's value for ever.
func (s *supervision) live() *config.Config {
	return s.plan.live.Snapshot()
}

// promptAgents is the sorted names of every task agent the role prompts dispatch
// (prompts.RoleReferences), which the registration answers (api.Options.PromptAgents, whose
// register writes an empty list for none).
func promptAgents(references promptrefs.Names) []string {
	return slices.Sorted(maps.Keys(references[promptrefs.TaskAgents]))
}

// shimAddress is the address every agent's shim dials: the listener's bound address, or, when
// advertiseHost (the top-level advertise_host, which only runtime: kubernetes accepts) names one,
// that host at the bound port, the kernel's choice when worker_stream_port was 0. A bound address
// it cannot split into tcp://host:port beside an advertiseHost is refused, never handed to pods.
func shimAddress(bound, advertiseHost string) (string, error) {
	if advertiseHost == "" {
		return bound, nil
	}
	hostport, ok := strings.CutPrefix(bound, "tcp://")
	if !ok {
		return "", fmt.Errorf("advertise_host %s needs a tcp:// worker stream, and the listener bound %s", advertiseHost, bound)
	}
	_, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", fmt.Errorf("advertise_host %s: the worker stream listener's address %s: %w", advertiseHost, bound, err)
	}
	return "tcp://" + net.JoinHostPort(advertiseHost, port), nil
}

// waitingWakes is every wake this daemon's machines hold, as each machine last published itself,
// which the state names beside the catch-ups waiting on their rows (api.WakesWaitingOf,
// source.wakes): the published view rather than the claim, so the read never waits on a decision
// in flight — a Sandbox relaunch holds its machine for as long as the runtime waits out its pods,
// and with many claims one is nearly always relaunching — exactly as the operator's claim listing
// and the capability report read their claims. Only the wakes are taken, and nothing is allocated
// while none waits, which is every moment outside a wave (supervise.Machine.WaitingWake). It reads
// memory alone, which is where a machine's wake lives: no store holds one, and a restart raises
// the wave again through its own paths.
func (s *supervision) waitingWakes() []supervise.WaitingWake {
	if s.supervisor == nil {
		return nil
	}
	var waiting []supervise.WaitingWake
	for _, m := range s.supervisor.supervised() {
		if wake := m.WaitingWake(); wake != nil {
			waiting = append(waiting, *wake)
		}
	}
	return waiting
}

// pacedCatchUps is every catch-up the wake pacing is still holding, as the state names them beside
// the machines' own wakes (api.State.WakesWaiting): the notice a start writes to an architect
// already running in place of a launch waits on its row's next_at rather than in a machine
// (workflow's claimReady, record.Notice.PacedAt), so this lane of the wave is read from the store —
// in the state's own snapshot, with everything else the state reads, rather than from a second
// transaction that could see another instant.
//
// The rows are the ones a ready would otherwise release and the pacing holds back (the outbox's
// releaseWaiting reads the same set through the same WaitingNotices and leaves behind exactly the
// rows whose reserved moment is still ahead of now): a row whose moment has come is a wake that
// is running or about to, or one whose attempt failed and is waiting on a backoff of its own, and
// neither is a wake the pacing holds. Each goes to the root architect of its issue, whose token
// it is composed for exactly as the line that wake writes composes it (the outbox's wakeRan),
// since the row names the issue and the kind alone. A project whose outbox holds no such row
// allocates nothing.
func pacedCatchUps(ctx context.Context, tx pgx.Tx, records record.Store, project, dispatchProject string, now time.Time) ([]api.WakeWaiting, error) {
	waiting, err := records.WaitingNotices(ctx, tx, dispatchProject, now)
	if err != nil {
		return nil, err
	}
	var wakes []api.WakeWaiting
	for _, row := range waiting {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			return nil, fmt.Errorf("decode waiting notice row %d: %w", row.ID, err)
		}
		notice, ok := payload.(record.Notice)
		if !ok || !notice.PacedAt.After(now) {
			continue
		}
		architect, err := claim.NewToken(project, row.Issue, claim.RoleArchitect)
		if err != nil {
			return nil, fmt.Errorf("the architect of the catch-up on row %d: %w", row.ID, err)
		}
		wakes = append(wakes, api.WakeWaiting{
			Claim: architect, Issue: row.Issue, Role: claim.RoleArchitect, Kind: supervise.WakeCatchUp, DueAt: row.NextAt.UTC(),
		})
	}
	return wakes, nil
}

// openSupervision reads the claims the store holds and their sessions' capability reports, takes
// the worker stream, and builds the runtime over it, for supervision's lifetime and with the
// workflow's App tokens (nil without a workflow): every step of supervision that can refuse, so a
// daemon that cannot supervise refuses before its boot is recorded.
//
// reserve is the daemon's one wake limiter as the machines reach it (run builds it, before the
// workflow, which reserves from the same one): the moment each launch may run, and nil while the
// operator paces nothing, which is the gate's reading of the pacing off (supervise's
// reserveWake). The rate and the burst go with it for the line every wake writes, never for that
// decision (supervise.WakePace).
//
// refreshGate is the daemon's one stale-plugin refresh gate, threaded exactly as reserve is and
// from the same place (run builds both, from the one rate): it goes on the machines' Deps, so
// every machine of this daemon asks the same gate, and the supervision keeps the same reference,
// because the refresh pass corrects it from the wakes those machines publish and asks it for
// each claim it posts (refreshStalePlugins, waitingWakes).
func openSupervision(boot context.Context, cfg config.Config, log *slog.Logger, p plan, st *store.Store, apps appauth.Tokens, reserve func(time.Time) time.Time, refreshGate supervise.RefreshAdmitter) (*supervision, error) {
	claims, err := st.Claims(boot)
	if err != nil {
		return nil, err
	}
	claims = ofProject(claims, p.project)
	reports, err := st.CapabilityReports(boot)
	if err != nil {
		return nil, err
	}

	supervising, cancel := context.WithCancel(context.Background())
	draining, endDrain := context.WithCancel(context.Background())
	tokens := api.NewBootTokens(st)
	sup := newSupervisor(supervising, st, p.project, cfg.StateDir, log)
	// The listener's RPC bound is a holder of its own: the socket is bound once here and never
	// rebound, and a change of worker_rpc_timeout_seconds swaps the bound alone
	// (workerStreamHolder), so no worker connection drops.
	listener, err := stream.Listen(draining, p.stream,
		sup.helloResolver(tokens, cfg.WorkerRPCTimeout), stream.Options{RPCTimeout: cfg.WorkerRPCTimeout, Log: log})
	if err != nil {
		cancel()
		endDrain()
		return nil, err
	}
	dial, err := shimAddress(listener.Addr(), cfg.AdvertiseHost)
	if err != nil {
		cancel()
		endDrain()
		return nil, err
	}
	// buildRuntime is the one way this daemon's runtime is built, the boot's build and every
	// rebuild alike: the factory for the configuration it is given, over this same listener and
	// address, on a context of the runtime's own. That context is supervision's lifetime cut
	// short by the stop the build answers, so the holder can end the informers, the caches and
	// the credential refresher of a runtime it replaces (runtimeHolder.rebuild).
	buildRuntime := func(c config.Config) (runtime.Runtime, context.CancelFunc, error) {
		factory, err := p.runtimeFor(c)
		if err != nil {
			return nil, nil, err
		}
		living, stop := context.WithCancel(supervising)
		next, err := factory(living, listener, dial, apps, p.repository, st, p.repositories.Accepted)
		if err != nil {
			stop()
			return nil, nil, err
		}
		return next, stop, nil
	}
	built, stopBuilt, err := buildRuntime(cfg)
	if err != nil {
		cancel()
		endDrain()
		return nil, fmt.Errorf("build the %s runtime: %w", cfg.Runtime.Name, err)
	}
	// The runtime behind its holder: a `runtime.*` change builds the runtime again over this same
	// listener and address and swaps it, and every reader — the machines, the supervision, the
	// API's tree releaser — holds the holder.
	rt := newRuntimeHolder(built, stopBuilt, log, buildRuntime)
	// The launch specs behind theirs: everything a launch reads that the claim does not carry,
	// built again from the configuration a `launch` change settled, read by the next launch.
	buildSpecs := func(c config.Config) (specs, error) {
		secrets, err := readLaunchSecrets(c, environLookup(p.environ))
		if err != nil {
			return specs{}, err
		}
		instructions := p.instructions
		if contents, err := readInstructions(c); err != nil {
			return specs{}, err
		} else if contents != nil {
			if instructions, err = config.WriteDeploymentInstructions(contents, c.StateDir, c.Project); err != nil {
				return specs{}, err
			}
		}
		return specs{
			stateDir: c.StateDir, project: p.project, instructions: instructions, secrets: secrets, prompts: p.prompts,
			identity: p.identity, repository: p.repository,
			repositories: func() api.RepositoriesView { return p.repositories.view(nil) },
			designSteps:  c.Gates.Steps(), slackChannels: c.Projects[c.Project].SlackChannels,
			runtimeName: c.Runtime.Name, plugins: p.plugins,
			commitIssueTrailerRepos: c.Projects[c.Project].CommitIssueTrailerRepos, same: p.repositories.same,
			backend: c.BackendFor, hostWorkspaces: !rt.ProvisionsWorkspaces(), ensureWorkspace: p.ensureWorkspace,
			memoryInstructions: p.memoryInstructions, memoryGroup: treeMemoryGroup(st),
		}, nil
	}
	settled, err := buildSpecs(cfg)
	if err != nil {
		cancel()
		endDrain()
		return nil, err
	}
	launchSpecs := newSpecsHolder(settled, buildSpecs)
	p.rebuild.register(newWorkerStreamHolder(listener), rt, launchSpecs)
	sup.deps = supervise.Deps{
		Runtime:                 rt,
		Conns:                   listener,
		Store:                   pruning(gateLaunch(tokens.Recording(st), p.checkLaunch), runtime.SecretsDir(cfg.StateDir), log),
		Specs:                   launchSpecs,
		Identity:                adoptIdentity(p.identity, p.repository),
		IssueInTurn:             sup.claimInTurn,
		Secrets:                 p.secretsEnroller,
		PhaseHolds:              p.phaseHolds,
		RootHears:               p.rootHears,
		RootTask:                p.rootTask,
		TreeClosable:            p.treeClosable,
		TaskConfirmed:           p.taskConfirmed,
		SessionLost:             p.sessionLost,
		SessionStoreFault:       p.sessionStoreFault,
		ProviderErrorRunReached: p.providerErrorRunReached,
		SessionGone:             cleanSessionStop(supervising, cfg.EnvoyURL, p.secrets["ENVOY_TOKEN"], log),
		Clock:                   p.clock,
		Log:                     log,
		// Every budget and every bound a decision is held to is read as that decision is made
		// (liveBudgets): `launch_failure_limit`, `worker_rpc_timeout_seconds`,
		// `pod_update_retask_hours` and their neighbours take effect at once, with no machine
		// rebuilt.
		Settings: liveBudgets{live: p.live},
		// A timer that fires and a prompt's send each apply their decision on a goroutine of
		// their own, beside the machine's feeding goroutine, so each takes a hold mark of its own.
		Worker: store.ForConcurrentWorker,
		WakePace: supervise.WakePace{
			Reserve: reserve,
			Rate:    cfg.WorkerWakesPerMinute,
			Burst:   cfg.WorkerWakeBurst,
		},
		RefreshGate:        refreshGate,
		PluginContract:     api.DaemonAPIVersion,
		PluginRelaunchable: p.pluginRelaunchable,
		NoPluginInstall:    func(c supervise.Claim) bool { return noPluginInstall(cfg, p.live, c) },
	}
	stopping, beginStop := context.WithCancel(context.Background())
	supervisionOf := &supervision{
		cfg: cfg, log: log, plan: p, stream: listener, runtime: rt, supervisor: sup, tokens: tokens, claims: claims,
		reports: reportsOfClaims(reports, claims), reportStore: st, refreshGate: refreshGate,
		cancel: cancel, draining: draining, endDrain: endDrain, decided: api.NewRouteDecisions(),
		stopping: stopping, beginStop: beginStop,
	}
	// The machines' report of a stalled turn they ended goes to supervision, which is built
	// after their deps: the one callback whose receiver is supervision itself, set here before
	// any machine exists, as the workflow sets enqueue where it attaches.
	sup.deps.StallBackstopped = supervisionOf.stallBackstopped
	return supervisionOf, nil
}

// cleanSessionStop is what a machine calls once a probe has judged a claim's agent process gone
// (supervise.Deps.SessionGone): it deletes that session's registration from the Envoy listener,
// so the listener reads the session `gone` at once instead of holding its role claim and its
// issue claims for the whole live window (dispatch://INFRA-392). It runs on the same client the
// daemon's notices go through (notify.New), beside controller.NewProber, on the same base URL
// and bearer.
//
// The machine calls it inside its own decision, under its lock, so the call is made on a
// goroutine of its own, on supervising — supervision's lifetime, not the decision's context,
// which the machine's caller can end the moment the decision returns. goneAt, the moment the
// stop belongs to, is read here, inside the decision and before any relaunch (Machine.died
// calls sessionGone first), where no lock is waited on.
//
// That delete must never take the registration of the process that replaced the dead one: the
// very decision that asks for the stop goes on to relaunch the claim under the same session id
// (Machine.died), and the relaunch registers that id again. So the stop names the dead
// process's own registration — its client instance id, read from the listener first (send) —
// and carries goneAt beside it, and the listener deletes only under both. What each condition
// is for, and why neither alone would do, is on notify.DeleteConditions.
//
// The listener keeps the entry when either condition refuses it, so the id rescues no
// registration the moment refuses: a heartbeat whose write lands after the probe, or a listener
// clock ahead of this daemon's, is kept even where the id names the dead process, and that
// session waits the gone horizon out.
//
// The read and the delete share one budget, cleanSessionStopBudget, so a listener that never
// answers does not leave the stop in flight for as long as it likes, and supervision's own end
// ends it too.
//
// A listener that refuses the delete, keeps the registration, or cannot be reached costs the
// session nothing beyond the wait it already had: the registration lapses on its own at the
// gone horizon. With no listener configured nothing is told.
func cleanSessionStop(supervising context.Context, envoyURL, token string, log *slog.Logger) func(string) {
	if envoyURL == "" {
		return nil
	}
	stop := sessionStop{sessions: notify.New(envoyURL, token), log: log}
	return func(session string) {
		goneAt := time.Now()
		go stop.send(supervising, session, goneAt)
	}
}

// sessionStop is one role's clean stop in flight: the listener client it goes to, and the log
// that says what became of it.
type sessionStop struct {
	sessions *notify.HTTPPublisher
	log      *slog.Logger
}

// send is the goroutine cleanSessionStop starts, with goneAt the moment the probe judged the
// process gone. It reads what the listener holds for the session and then deletes that
// registration by name: one read, one delete, one answer, and nothing is sent twice.
//
// A read the listener does not answer sends no delete at all. The moment is this daemon's own,
// but the instance id is not: a failed read leaves unknown whether the entry names a client
// process, and a delete sent then would rest on the moment alone for an entry that may well name
// one, which is the case where the id is the relaunch's guard against a daemon clock ahead of
// the listener's. An entry the listener answers with no instance id is known to name none, so
// the moment-only delete is the only delete there is, and it is sent. A registration left
// standing lapses at the gone horizon, the wait the session already had. An answer of no
// registration is the outcome the stop asked for, already reached.
//
// It reports the stop refused, kept or done, one line either way, since nothing waits on its
// answer. The listener keeps a registration for three separate reasons — another client
// instance owns it, it was written after the moment, or the delete lost its compare-and-swap —
// so the line says only that it was kept and carries the listener's own words for which
// (SessionDeleted.Reason), beside the two conditions the stop named and the registration the
// read answered with. The registration is what the read saw, not what the delete compared, so
// where the entry changed between the two the line cannot say whose write refused it; where it
// did not, it tells a stop refused for a successor's registration from one refused for the dead
// process's own last heartbeat without asking the listener anything.
func (s sessionStop) send(supervising context.Context, session string, goneAt time.Time) {
	ctx, cancel := context.WithTimeout(supervising, cleanSessionStopBudget)
	defer cancel()
	registration, err := s.sessions.ReadSessionRegistration(ctx, session)
	if err != nil {
		s.log.Warn("the Envoy listener did not answer what a dead role's session registration is, so no clean stop was sent",
			"session", session, "error", err)
		return
	}
	if !registration.Registered {
		s.log.Info("the Envoy listener answered no registration of a dead role's session, so none was stopped", "session", session)
		return
	}
	stopped, err := s.sessions.DeleteSession(ctx, session, notify.DeleteConditions{
		ClientInstanceID:   registration.ClientInstanceID,
		NotRegisteredAfter: goneAt,
	})
	if err != nil {
		s.log.Warn("the Envoy listener did not take the clean stop of a dead role's session", "session", session, "error", err)
		return
	}
	if !stopped.Deleted {
		s.log.Info("the Envoy listener kept the registration of a dead role's session",
			"session", session, "goneAt", goneAt.UnixMilli(),
			"clientInstanceID", registration.ClientInstanceID, "lastSeen", registration.LastSeen,
			"reason", stopped.Reason)
		return
	}
	s.log.Info("the Envoy session of a role whose process is gone was stopped", "session", session)
}

// start supervises every claim the store holds. A claim with a live locator is re-adopted — its
// process told to the runtime and observed from now on, never launched again. Once every claim has
// its machine, the boot orphan reconciliation has run and settle (the workflow's terminal replay,
// nil without a workflow) has applied the stored claims' ready and failure facts, hellos are
// resolved: a shim reconnecting across the restart is admitted by a claim already supervised. The
// boot settles before any hello is admitted because a restart brings every live shim back at once:
// each admitted hello's second resolve, its machine's claim write and the facts that machine
// then applies would compete with the replay for the database and the fact lock, and a boot
// replaying a few hundred claims behind a few hundred hellos ran out of its budget and exited at
// every restart while the shims kept redialing. An unrecorded launch is relaunched only
// after that reconciliation proves any pre-crash pane absent or reaped; a failed listing leaves it
// launch_uncertain until the bounded retry succeeds. The relaunches run after hellos are
// resolved, on a goroutine of their own, so start returns, and the API serves, without waiting for
// any (relaunchUnfinished): every re-adopted claim on another plugin contract is posted its boot
// stale-plugin relaunch first (refreshStalePlugins), over every one before any of them is
// relaunched; a claim a refused attempt leaves stale is retried at the orphan sweep's own cadence
// from then on (reconcileOrphans), not just at this boot pass. A daemon that leaves the controller
// to its operator first stops the controller's claim an earlier
// boot under `controller: daemon` left (stopLaunchedController), which leaves it absent or
// retired. The only controller claim among the unrecorded launches is a
// launch a crash cut short, and by then the stop has retired it, so launchUnfinished would not
// relaunch it: its ReleaseUncertainLaunch acts only on a launch-uncertain claim. Dropping that token
// is a second guard beside that check, so boot does not report the retired claim as an unrecorded
// launch. The steps before the relaunches run on boot, settle on budgets of its own, and the
// relaunches on the machines' context; the daemon's stop ends all three (run).
func (s *supervision) start(boot context.Context, settle func() error) error {
	unfinished, err := s.supervisor.restore(boot, s.claims)
	if err != nil {
		return err
	}
	if s.cfg.ControllerLaunch != config.ControllerLaunchDaemon {
		if err := s.stopLaunchedController(boot); err != nil {
			return err
		}
		controller := claim.ControllerToken(s.plan.project)
		unfinished = slices.DeleteFunc(unfinished, func(token claim.Token) bool { return token == controller })
	}
	pruneAllBut(runtime.SecretsDir(s.cfg.StateDir), s.claims, s.log)
	reconciled := s.reconcileBootOrphans(boot)
	if !reconciled {
		for _, token := range unfinished {
			s.log.Warn("supervise: unrecorded launch remains uncertain; not relaunching", "claim", token)
		}
	}
	if settle != nil {
		if err := settle(); err != nil {
			return err
		}
	}
	close(s.supervisor.restored)

	observations, err := s.runtime.Observe(s.supervisor.ctx)
	if err != nil {
		return fmt.Errorf("observe the runtime: %w", err)
	}
	s.wg.Add(3)
	go func() {
		defer s.wg.Done()
		for observation := range observations {
			s.supervisor.post(observation.Locator.Claim, supervise.RuntimeObservation{Observation: observation})
		}
	}()
	go func() {
		defer s.wg.Done()
		for ev := range s.stream.Events() {
			mapped, err := superviseEvent(ev)
			if err != nil {
				s.log.Error("worker stream: an event no machine can take", "error", err)
				continue
			}
			s.supervisor.post(claimOf(mapped), mapped)
		}
	}()
	go func() {
		defer s.wg.Done()
		// The sweep runs on a goroutine of its own beside the machines for supervision's life, so
		// it takes a hold mark of its own.
		s.reconcileOrphans(store.ForConcurrentWorker(s.supervisor.ctx))
	}()
	s.endPluginRounds(s.claims)
	s.refreshStalePlugins(s.claims, "boot")
	if len(unfinished) > 0 {
		s.relaunchUnfinished(unfinished, reconciled)
	}
	return nil
}

// endPluginRounds takes every claim this boot restored out of the plugin update round an earlier
// daemon left on it, whatever state the claim is in (supervise.PodUpdateEnded, endPodUpdate).
// Nothing of this release opens a plugin round — the stale-plugin lane replaces a stale process
// and asks its worker nothing (supervise's refreshPlugin, relaunchStale) — but the daemon this
// one replaces could, and a round nobody ends holds the claim's next task behind it for ever:
// the round's own answer is what releases that task, and no sweep of this release opens, carries
// or ends a plugin round at all (reconcilePodUpdates reads pod rounds). So the boot ends them,
// once, over the claims it read from the store (dispatch://INFRA-2712 R5).
//
// The end takes the claim out of the round whole: the ask the round left unread goes, every
// round field goes, the claim is written, and the task the round was holding back is sent
// (supervise's endPodUpdate, leaveRound). A claim with no agent is sent nothing by that last
// step and needs nothing more — sendPending hands nothing to a claim that is not ready or idle
// — so the suspended and the failed claims of a round are served by this same post.
//
// It runs before the stale-plugin pass rather than after it, and one machine's inbox is FIFO
// (supervisor.inbox: "unbounded … and in order, so a claim sees its events as its sources
// reported them"), so the StalePlugin that pass posts a moment later is decided behind this
// end. That is what makes the order right: the refresh reads a claim already out of its round,
// and the pass's own skip is by round kind for the same reason (refreshStalePlugins), since the
// claims both passes walk are the boot's own reading, where the round still stands.
//
// A retired claim is posted nothing it needs: its round went with it (supervise's retire calls
// clearUpdate), and the end is ignored in that state anyway. A worker that answers such a round
// after this boot — a turn that was in flight when the daemon restarted — is refused as
// supervise's podUpdateSignalled already refuses an answer for a round its claim is not in:
// nothing is recorded, and the claim is neither failed nor retired for it.
//
// One line is written for the pass, with the number of claims it found, because that number is
// what an operator reads this boot's inheritance by; a daemon that finds none writes nothing,
// which is every boot after the release that deleted the round.
func (s *supervision) endPluginRounds(claims []supervise.Claim) {
	found := 0
	for _, c := range claims {
		if !c.InPluginRound() {
			continue
		}
		found++
		s.supervisor.post(c.Token, supervise.PodUpdateEnded{Claim: c.Token, Round: c.UpdateRound})
	}
	if found > 0 {
		s.log.Info("supervise: an earlier daemon left these claims in a plugin update round; this boot takes each of them out of it",
			"claims", found)
	}
}

// refreshStalePlugins posts StalePlugin to every live claim whose recorded plugin contract is not
// this daemon's — an earlier daemon's, or none at all, at the first boot on this release, or one a
// prior relaunch attempt the tree lifecycle or the store refused left stale (launch returns such a
// refusal before the claim's state, locator, or contract change). Trigger says which pass is
// posting: "boot", once over the restored claims before any of them is relaunched
// (relaunchUnfinished runs after this); "sweep", at every orphan sweep's own cadence, over the
// claims it already reads each tick, so a relaunch the boot pass could not complete is retried
// without waiting for the next restart. refreshPlugin's own re-check makes a re-post for a claim a
// prior attempt already moved on — relaunched, or registered current — a no-op. A claim still
// launching is left to its own registration, which decides the comparison the moment it arrives,
// so only the four states whose process has registered are read.
//
// A claim whose launch installs no plugin is never posted, whatever contract it records and
// round or no round (noPluginInstall). A relaunch is the lane's whole act, and for such a claim
// the relaunch cannot work: a Claude Code role's plugin under Kubernetes is the worker image's
// own claude-envoy, which no install reaches, so the launch keeps the pod the claim already runs
// in, the process comes back on that same plugin, registers the same contract again, and the next
// registration is the second relaunch in a row — which staleRegistration would charge, until the
// launch budget fails the claim. The contract decides nothing: a claim at this release's own
// contract is as unreachable by an install as one below it, and the next bump would walk exactly
// that claim to failed. Nothing here can move such a worker's plugin; a new image can, which is
// what the pod round replaces the pod for. So it is left running and answers its round like any
// other worker, with the op or by ending the turn the round's task starts (supervise's
// turnEnded). staleRegistration accepts such a registration and charges it nothing, for the same
// reason (supervise.Deps.NoPluginInstall).
//
// A claim in a pod round is passed over and a claim in a plugin round is not. That term is the
// lane's own rather than this pass's: staleForTheLane holds it and says what each kind of round
// reads as and why, because the count of what the lane has left must name the claims this pass
// posts and no others (claimsToRefresh).
//
// A daemon that cannot relaunch onto a plugin (PluginRelaunchable false, LEGION-164's degraded
// Kubernetes mode) posts nothing; it logs one Error line per stale claim at boot only — a sweep's
// finding would only repeat a line the boot already wrote, since nothing relaunchable has changed
// in between.
//
// Every claim it does post takes a credit of the refresh gate but the two named below, and a
// claim the gate refuses is not posted at all (supervise.RefreshGate, dispatch://INFRA-2712
// R1). That is what holds the lane to one minute of the daemon's wake pacing: the gate grants K
// credits, K the rate, and the next sweep — a minute later, by which time this sweep's
// admissions have had their turns — posts the next K. Nothing of the refresh is lost by a
// refusal, since this pass re-reads every stale claim at every sweep, which is why the refused
// claim is passed over in silence rather than remembered anywhere. The admission is taken
// before the post, never after the machine has decided, so a refresh in flight is counted: that
// is what the gate's own floor is for (Correct). The gate is put right before any of this, from
// the refresh wakes the machines really hold — the lane's own kind, the one kind only this lane
// parks (supervise.WakePluginRefresh, parkedWakes). That correction is what takes back a credit
// lost to a path that returns none, which the gate itself names: a machine gives its credit
// back at the end of the decision that holds it, unless a refresh wake of its own is waiting,
// and what is left to the correction is a post to a claim this daemon stopped supervising
// (supervise.RefreshGate, supervise's endRefreshLane). It is the pass's own first act rather
// than part of the read it counts from, because it resets the gate's admission window: a second
// caller asking which claims hold a wake — a question parkedWakes is named for and nothing of
// the gate's — would reset that window mid-sweep and let the lane admit a second K. The boot
// pass corrects an empty gate, which is what a restart leaves: no machine of this daemon holds
// a wake yet, so the reading is zero and the fleet's refresh starts with every credit free
// (supervise.RefreshGate.Correct, which states what each half of it costs).
//
// A claim whose machine already holds a waiting wake is passed over before the gate is asked at
// all, read from the same snapshot the correction counts (parkedWakes). The machine decides such
// a claim nothing — refreshPlugin returns over a claim that already holds a wake of any kind, and
// gives back whatever credit the post carried — so asking for one here would spend an admission
// on a claim that raises no relaunch, and spend it for the whole of the pass, since the machine's
// release lands after the pass has walked every claim. At a fleet's worth of stale claims that is
// the lane admitting the K claims it already has waiting and none of the ones it has not, which
// is the drain this change exists to shorten standing still for a sweep.
//
// A claim in a turn is posted with no credit at all, for that same reason read at the other
// end: its relaunch takes no wake now. The machine holds it for the turn's end
// (holdStaleRelaunch), and the relaunch that end runs asks the gate for a credit of its own
// (supervise's refreshPlugin), which is the design requirement R2 states — "A claim in a turn
// takes no admission; its turn's end asks the gate, so it is relaunched then if a credit is
// free and at the next sweep otherwise" (afterTurn).
// Admitted here instead, such a claim would spend one of the K on a relaunch that raises no
// wake, and spend it until the machine's release landed, which is after this pass has walked
// every claim; and a turn is minutes while the sweep is one, so the same claim would spend one
// at every tick of its turn. The tail is where that bites hardest: a refreshed idle claim
// leaves the stale set while a working one stays in it, so the set grows richer in claims in a
// turn as the wave drains, and the last idle stragglers would wait sweeps behind credits that
// raised nothing.
//
// The skip is as imperfect as the one above it, and costs exactly what that one costs. This
// pass reads the claims of a snapshot, and the machine decides the post as the claim stands
// then: a claim read idle can have started a turn by the time its post is decided, and one read
// working can have ended it. The first is posted a credit the machine gives straight back
// (refreshPlugin, endRefreshLane) and is relaunched at that turn's end or at a later sweep; the
// second is posted none, and raises its relaunch at the next sweep, which is one minute. One
// sweep is what an imperfect skip costs here, as it is for the wake the parked skip reads.
//
// One line is written for the refusals of a pass, naming how many claims are left for a later
// sweep, rather than one per claim: a boot over a fleet of several hundred refuses hundreds at
// every tick of the sweep, and a line each would bury the log the operator reads the wave from.
// A claim in a turn is in none of that count: nothing refused it, and it is left to no later
// sweep of this pass but to the end of its own turn.
//
// The project controller's claim is posted whatever the gate holds, and takes no credit: its
// relaunch is exempt from the pacing altogether (supervise's reserveWake, claim.IsController), so
// it holds none of what the gate counts, and it is the one session an operator's own reading of a
// running wave relies on. It is posted mid-turn as well, since its turn is never waited out
// (refreshPlugin).
func (s *supervision) refreshStalePlugins(claims []supervise.Claim, trigger string) {
	noInstall := func(c supervise.Claim) bool { return noPluginInstall(s.cfg, s.plan.live, c) }
	parked, refreshes := s.parkedWakes()
	s.refreshGate.Correct(refreshes)
	refused := 0
	for _, c := range claims {
		if !staleForTheLane(c, noInstall) {
			continue
		}
		if !s.plan.pluginRelaunchable {
			if trigger == "boot" {
				s.log.Error(supervise.NoPluginSourceMessage,
					"claim", c.Token, "incarnation", c.Locator.Incarnation, "pluginContract", c.PluginContract, "daemonApiVersion", api.DaemonAPIVersion)
			}
			continue
		}
		if parked[c.Token] {
			continue
		}
		// The two posts that take no credit, each for the reason above it: the controller's
		// claim, whose relaunch is outside the pacing the gate bounds, and a claim in a turn,
		// whose relaunch takes no wake until that turn's end, where it asks the gate itself.
		if claim.IsController(c.Role, c.Issue, c.Tree) || c.State == supervise.StateWorking {
			s.supervisor.post(c.Token, supervise.StalePlugin{Claim: c.Token, Trigger: trigger})
			continue
		}
		if !s.refreshGate.Admit() {
			refused++
			continue
		}
		s.supervisor.post(c.Token, supervise.StalePlugin{Claim: c.Token, Trigger: trigger, Admitted: true})
	}
	if refused > 0 {
		s.log.Info("supervise: the stale-plugin refresh gate is full; these claims are left to a later sweep",
			"claims", refused, "trigger", trigger)
	}
}

// staleForTheLane is the stale-plugin lane's own stale claim, in the five terms its two readers
// read alike: a recorded session, a process that holds a capability, a recorded plugin contract
// that is not this daemon's, no pod update round, and a launch that installs a plugin at all.
// The readers are the pass that posts the lane's relaunches (refreshStalePlugins) and the count
// of what the lane still has to do (claimsToRefresh), and they are one predicate because a term
// added to one of them alone makes the count name work the lane never does or hide work it will
// — which is the one thing the count exists not to do (dispatch://INFRA-2712 R4).
//
// A claim in a pod round is out of the set, whatever contract it records (InPodRound). That
// round is carrying it, and the process is replaced when the round ends (reconcilePodUpdates),
// so a StalePlugin post would relaunch mid-turn a claim the round is waiting on and take the
// pod's replacement its answer; the count says the same, because a claim the pass will not
// post is not work the lane has left.
//
// A claim in a plugin round is in the set, and the term is by round kind for that reason. The
// boot's own de-round pass has posted such a claim its end a moment before the refresh pass
// runs (endPluginRounds), and one machine's inbox is FIFO, so that end is decided ahead of the
// post and the machine sees a claim in no round at all. What that pass reads is the claims the
// boot restored, where the round still stands, so a term of "in any round" would pass over
// exactly the claims the de-round pass has just freed and leave each of them a sweep — a whole
// minute, and a credit of the gate each — behind the rest of the fleet, and would have the
// count hide that work at the one moment it exists to be read: a boot over a fleet an earlier
// daemon left mid-round. A claim whose plugin round is somehow still standing when its machine
// takes the post is decided nothing wrong by it either: refreshPlugin relaunches it, and no
// plugin round holds a relaunch back, since the ask that did was deleted with the round. A
// claim that left its round at a registration speaking this daemon's contract (supervise's
// endCurrentPluginRound) is no longer stale at all, and the contract term above passes over it.
//
// noPluginInstall nil is every launch installing a plugin, which only a source a test builds by
// hand holds: serve gives the state's source the rule over this deployment's configuration and
// the lane's own pass builds a closure of it at every sweep, so no daemon reads this term as
// nil, a tmux daemon included (noPluginInstall, serve, refreshStalePlugins).
func staleForTheLane(c supervise.Claim, noPluginInstall func(supervise.Claim) bool) bool {
	if c.Session == "" || c.PluginContract == api.DaemonAPIVersion || !supervise.HoldsCapability(c.State) || c.InPodRound() {
		return false
	}
	return noPluginInstall == nil || !noPluginInstall(c)
}

// claimsToRefresh is how many of claims the stale-plugin lane still has to relaunch onto this
// daemon's plugin, which the state serves as `claimsToRefresh` (api.State.ClaimsToRefresh,
// contract 73): a reader of a running wave can then tell what the lane has left from what it
// has already admitted into the pacing, which `wakesWaiting` alone cannot say now that the gate
// holds no more than a minute of that pacing at a time (dispatch://INFRA-2712 R4).
//
// The set is the one refreshStalePlugins walks, read through that pass's own predicate and the
// whole of it (staleForTheLane): the round term is in there with the other four, so no claim
// the pass would post is left out of this count and none it passes over is named by it. It is
// computed here, where the lane's own rule lives, rather than in internal/api, which does not
// own that rule and which cuts the body to its caller only after it is built (api's stateFor).
//
// A claim in a turn is counted, for the same reason the pass posts it: the lane has it left to
// refresh, its relaunch is raised at that turn's end (supervise's holdStaleRelaunch, afterTurn),
// and a count that left it out would read as a drained lane over a fleet still working.
//
// Whether this daemon can relaunch onto a plugin at all is deliberately not a term of it
// (plan.pluginRelaunchable). On a daemon that cannot, the pass posts nothing and writes one
// Error line per stale claim at boot, and this count is what that gap costs, standing at the
// claims the lane cannot act on beside those lines. A count zeroed there would read as a lane
// with nothing left, which is the one thing such a daemon is not.
func claimsToRefresh(claims []supervise.Claim, noPluginInstall func(supervise.Claim) bool) int {
	left := 0
	for _, c := range claims {
		if staleForTheLane(c, noPluginInstall) {
			left++
		}
	}
	return left
}

// parkedWakes is every claim of this daemon whose machine holds a waiting wake of any kind,
// with the refreshes of that same reading counted beside it: the two answers the refresh pass
// needs come from one walk of the published views, so the claims it passes over and the count
// it puts the gate right from are of one instant rather than of two (waitingWakes; the pass
// makes the gate's own correction with this count, supervise.RefreshGate.Correct).
//
// Every kind is in the set, not the lane's own: what the pass reads it for is whether the machine
// would decide this claim anything, and refreshPlugin decides a claim that holds a wake of any
// kind nothing at all. The count beside it is the lane's own kind alone, because that is what the
// gate counts.
//
// The reading is of the views each machine last published, so it lags a decision in flight in both
// directions (Machine.WaitingWake): a claim whose wake was parked a moment ago can be missing from
// it, and one whose wake has just run can still be in it. Neither costs anything here. The first
// is a post the machine answers by giving its credit straight back, and the second is a claim the
// pass leaves to the next sweep, a minute later.
func (s *supervision) parkedWakes() (map[claim.Token]bool, int) {
	parked := map[claim.Token]bool{}
	refreshes := 0
	for _, wake := range s.waitingWakes() {
		parked[wake.Claim] = true
		if wake.Kind == supervise.WakePluginRefresh {
			refreshes++
		}
	}
	return parked, refreshes
}

// noPluginInstall is c's launch installing no plugin at all, whatever contract the claim records:
// the runtime is Kubernetes and the claim's role runs on Claude Code, whose plugin there is the
// worker image's own claude-envoy. No install reaches it (specs.stalePluginInstall installs none
// for that backend, and no workflow releases claude-envoy) and no relaunch moves it, because the
// launch keeps the pod the claim already runs in: only a new image carries another claude-envoy,
// which is what the pod round replaces the pod for.
//
// Under tmux a Claude Code role is exempt from nothing. Its pane loads the claude-envoy held at
// LEGION_CLAUDE_PLUGIN_DIR (resolveClaude; tmux's ClaudeLaunch, whose MCP configuration and
// plugin argument every launch writes afresh), so a relaunch once that pane's turn ends brings
// the worker up on whatever that directory holds now, exactly as an Oh My Pi pane comes back on
// the plugin installed on the daemon's host. No tmux launch installs a plugin for any backend
// (specs.stalePluginInstall), so a relaunch is the whole of the lane there — and for this role
// it is enough.
//
// It is one rule read in three places, each of which would otherwise act on a claim no install
// of this deployment can move: this package's own stale-plugin pass (refreshStalePlugins); the
// machine's stale registration, which supervise cannot decide for itself because a role's
// backend is the daemon's configuration (supervise.Deps.NoPluginInstall, handed this same
// function); and the API's stale-caller refusal, which would otherwise hold such a worker's
// grants, its handoff_complete and its ready on a relaunch that never comes
// (api.Options.NoPluginInstall, api's staleClaim).
//
// Being one rule, it reads each half from the one configuration that half lives in, and every
// site hands it both: the runtime name from the configuration the daemon booted on, since
// `runtime` is fixed for the process and no pod moves to another runtime; and the role's
// backend from the settings holder at the moment of the call, since `worker_backends` is a
// launch key a route change moves under the running daemon. Each site reading a configuration
// of its own is what this signature refuses: the API answering for the backend an operator
// just set while the stale-plugin pass went on answering for the one the daemon booted on
// relaunches a claim the machine then charges for the registration that follows.
func noPluginInstall(boot config.Config, live *config.Live, c supervise.Claim) bool {
	return boot.Runtime.Name == "kubernetes" &&
		runtime.Of(live.Snapshot().BackendFor(c.Role)) == runtime.BackendClaude
}

// reconcileBootOrphans retries only the boot reconciliation, boundedly. A listing error does not
// prove an unrecorded launch's pane is gone, so callers must not launch the claim again until this
// returns true. Each attempt reads the claims as they are now: a later retry must know the claims
// suspended or retired since boot as they are, not as the boot read them.
func (s *supervision) reconcileBootOrphans(ctx context.Context) bool {
	for attempt := 1; attempt <= bootOrphanReconcileAttempts; attempt++ {
		claims, err := s.supervisor.Claims(ctx)
		if err == nil {
			err = s.runtime.ReconcileOrphans(ctx, knownClaims(claims), 0)
		}
		if err == nil {
			return true
		}
		if attempt == bootOrphanReconcileAttempts {
			s.log.Warn("reconcile orphans at boot left panes uncertain", "attempts", attempt, "error", err)
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		s.log.Warn("reconcile orphans at boot failed; retrying", "attempt", attempt, "error", err)
		timer := time.NewTimer(bootOrphanReconcileRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}

// launchUnfinished starts only claims that remain queued with no locator, each on a goroutine of
// its own, and returns once every one has: one relaunch never waits out another's launch, which
// under a Sandbox can take minutes. A still-live old pane can reconnect while reconciliation was
// uncertain; its hello changes the state before a later retry, and it must never be joined by a
// second pane. It returns the claims whose release the store did not take, outside the daemon's
// stop: each is still launch_uncertain, in memory as in the store, and nothing else in this boot
// releases it, so the caller retries it (relaunchUnfinished). ctx is the relaunches' context; each
// relaunch derives a context of its own from it.
func (s *supervision) launchUnfinished(ctx context.Context, tokens []claim.Token) []claim.Token {
	refused := make([]bool, len(tokens))
	var launching sync.WaitGroup
	for i, token := range tokens {
		m, ok := s.supervisor.Machine(token)
		if !ok {
			continue
		}
		launching.Add(1)
		go func() {
			defer launching.Done()
			// Each relaunch runs on a goroutine of its own beside the others, so each takes a
			// hold mark of its own rather than ctx's.
			refused[i] = s.launchUnfinishedClaim(store.ForConcurrentWorker(ctx), token, m)
		}()
	}
	launching.Wait()
	var failed []claim.Token
	for i, token := range tokens {
		if refused[i] {
			failed = append(failed, token)
		}
	}
	return failed
}

// launchUnfinishedClaim releases one unfinished launch and launches it again, both on ctx, and
// reports whether the store refused the release (launchUnfinished).
func (s *supervision) launchUnfinishedClaim(ctx context.Context, token claim.Token, m *supervise.Machine) bool {
	released, err := m.ReleaseUncertainLaunch(ctx)
	if err != nil {
		return s.supervisor.decisionFailed(token, "release of an uncertain launch", err,
			"supervise: release an uncertain launch; retrying at the next orphan sweep")
	}
	c := m.Claim()
	if !released || c.State != supervise.StateQueued || c.Locator != nil {
		s.log.Info("supervise: unrecorded launch settled without relaunch", "claim", token, "state", c.State)
		return false
	}
	s.log.Warn("supervise: launching again a launch the previous daemon did not finish", "claim", token)
	spawn := supervise.RequestSpawn{Claim: token}
	if err := m.Handle(ctx, spawn); err != nil {
		s.supervisor.decisionFailed(token, fmt.Sprintf("%T", spawn), err, "supervise: launch an unfinished launch again")
	}
	return false
}

// relaunchUnfinished launches tokens' unfinished launches on a goroutine of its own, never on the
// boot: at once when the boot reconciliation succeeded (reconciled), then on the normal orphan-sweep
// cadence, until none is left or the daemon stops. A relaunched worker reaches the daemon only once
// the daemon is up — a Sandbox pod's launcher dials it through its Service, which routes only to a
// pod whose probes pass, so only once the API answers /healthz, and a shim's hello is resolved only
// once restoration has closed — so a relaunch the boot waited on could not register: it spent its
// whole launch budget, launch_failure_limit × worker_boot_timeout_seconds, with the API unanswered.
//
// The pre-restart process of such a launch, when one still runs, may now say hello before its
// relaunch writes the next generation, and be admitted: its claim, launch_uncertain or queued until
// then, ignores the hello and refuses its registration, and at the next generation the fence drops
// it. Its connection holds the claim's stream slot only while that process lives, and the relaunch
// ends it before it starts the next generation: a tmux boot's reconciliation kills every pane no
// claim records, and a Sandbox relaunch stops its launcher's earlier generation first. Once the next
// generation is written, the old process's boot token resolves stale.
//
// Until the boot reconciliation has succeeded, each sweep retries it first — each reconciliation
// has the bounded retry above, and only a success releases these claims to launch; once it has, a
// sweep retries only the releases the store did not take (launchUnfinished), since the
// reconciliation's proof that no predecessor runs still holds.
func (s *supervision) relaunchUnfinished(tokens []claim.Token, reconciled bool) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// The relaunches run on a goroutine of their own beside the machines and the orphan sweep,
		// so this goroutine takes a hold mark of its own.
		relaunching := store.ForConcurrentWorker(s.supervisor.ctx)
		if reconciled {
			tokens = s.launchUnfinished(relaunching, tokens)
		}
		ticker := time.NewTicker(s.plan.orphanSweep)
		defer ticker.Stop()
		for len(tokens) > 0 {
			select {
			case <-s.supervisor.ctx.Done():
				return
			case <-ticker.C:
			}
			if !reconciled {
				if reconciled = s.reconcileBootOrphans(relaunching); !reconciled {
					continue
				}
			}
			tokens = s.launchUnfinished(relaunching, tokens)
		}
	}()
}

// reconcileOrphans ends, every sweep interval, whatever the runtime holds that belongs to none of
// the claims the daemon has not retired, once it has idled past the grace, re-posts StalePlugin
// for every live claim whose contract still differs — the retry a relaunch the boot pass
// (refreshStalePlugins) could not complete needs — carries every update round a step
// (reconcilePodUpdates), ends the turn of every claim whose agent has stopped stepping
// (backstopStalledTurns), and tells the workflow of every phase worker that nothing will prompt
// (tellSilentPhaseWorkers). All four ride the sweep because it is the one periodic pass that
// already reads every claim, rather than running a timer of their own; its minute is far below
// the smallest backstop bound a configuration can hold, and far below the floor under
// `worker_silence_minutes`.
func (s *supervision) reconcileOrphans(ctx context.Context) {
	ticker := time.NewTicker(s.plan.orphanSweep)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		claims, err := s.supervisor.Claims(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("reconcile orphans: read the claims", "error", err)
			}
			continue
		}
		if err := s.runtime.ReconcileOrphans(ctx, knownClaims(claims), orphanGrace); err != nil && ctx.Err() == nil {
			s.log.Error("reconcile orphans", "error", err)
		}
		s.refreshStalePlugins(claims, "sweep")
		s.reconcilePodUpdates(ctx, claims)
		s.backstopStalledTurns(ctx, claims)
		s.tellSilentPhaseWorkers(ctx, claims)
	}
}

// halt begins the stop, once. It sets when the stop and the API's drain must end by (stopBy,
// drainBy), says the daemon is stopping, why (cause) and which claims are deciding, and ends
// stopping, which tells awaitStop the stop has begun, and the API's routes too
// (api.Options.Stopping), from when an operator's request that would change a claim is refused.
// Then, in this order:
//
//  1. No machine is fed another event (supervisor.halt).
//  2. The worker stream stops answering a shim's hello and closes every shim connection except
//     those of the claims an API route is deciding (stream.Listener.Narrow), so every other shim
//     keeps what its agent says in its backlog for the next daemon rather than hand it to one that
//     no longer acts on it. A kept connection carries its process's shutdown frame; what else it
//     carries until the drain ends (endDrain) is not acted on, since its claim is being suspended
//     or stopped, or is registering an agent the next boot re-adopts.
//  3. The machines' context is cancelled. Every decision the daemon makes itself runs on it — the
//     machines' own events, the workflow's outbox, the controller keeper, the boot's relaunches —
//     so it ends now rather than when its runtime call's own wait runs out: a Sandbox relaunch
//     waits minutes for the previous pod to go, the tree's other pods to finish workspace-init and
//     the new pod to appear. A decision an API route asked for runs on until the drain ends.
//
// Three orderings are required. The routes learn the stop has begun before the second step, so an
// operator's request that records its claim too late for Narrow to keep its connection finds the
// stop begun and is refused, rather than decided without the connection. The first step comes
// before the second, so the Closed events of the connections Narrow ends reach no machine. The
// second comes before the third, so a hello whose resolve the cancellation fails is already the
// stop's, and closes unrefused. A launch the cancellation cuts short leaves its claim launching
// with no process recorded, which the next boot relaunches once its orphan reconciliation has run.
// Nothing in halt waits, so the stop's budget (stopBudget) runs from the moment it begins. A nil
// cause is a boot that refused before it was recorded, which served nothing and says nothing more.
func (s *supervision) halt(cause error) {
	s.haltOnce.Do(func() {
		now := time.Now()
		s.stopBy, s.drainBy = now.Add(s.plan.stopBudget), now.Add(drainOf(s.plan.stopBudget))
		if cause != nil {
			s.log.Info("legion daemon stopping", "project", s.cfg.Project, "claims", s.supervisor.count(),
				"deciding", s.supervisor.inDecision(), "cause", cause.Error())
		}
		s.beginStop()
		s.supervisor.halt()
		s.stream.Narrow(s.decided.Holds)
		s.cancel()
	})
}

// drainOf is how long the API's drain may take within a stop budget: its first four fifths
// (stopBudget).
func drainOf(budget time.Duration) time.Duration {
	return budget * 4 / 5
}

// stop ends supervision without ending a single agent: it halts it, if nothing has yet, ends the
// drain (endDrain: the routes' decisions and the worker stream), then waits until every decision
// in flight has returned and every send already out has its outcome handled, so nothing writes to
// the store once it closes. The panes and pods keep running; the next boot re-adopts them. The
// wait is unbounded here; run stops waiting for it at the stop's budget.
func (s *supervision) stop() {
	s.stopOnce.Do(func() {
		s.halt(nil)
		s.endDrain()
		s.supervisor.stop()
		s.supervisor.wait()
		s.wg.Wait()
	})
}

// servingLatch is the one-way latch GET /readyz answers from (api.Options.Ready): false while the
// boot runs, true from the moment the daemon serves — its pod runtime built and its launcher
// acceptor installed (openSupervision, sandboxRuntime's SetLauncherResolver), and the goroutine
// that runs the API's server.Serve begun (serve) — and never false again of itself. Only the
// daemon's stop makes it 503 once more, which the API server knows for itself
// (api.Options.Stopping).
//
// It reports the boot and nothing else: not the pool, not the database and not the load. The
// daemon's Kubernetes Service carries the API port and the worker stream's port under one
// selector, so a probe that failed while the daemon was busy would take the worker stream out of
// the Service while every pod's launcher was trying to reach it, and the deployment runs one
// replica with the Recreate strategy, so no second pod answers in its place. It is a readiness
// probe and never a liveness probe.
type servingLatch struct{ served atomic.Bool }

// reach marks the daemon serving. serve is the only caller: nothing else sets the latch, and
// nothing clears it.
func (l *servingLatch) reach() { l.served.Store(true) }

// serving reports whether the daemon has reached serving; this is what api.Options.Ready reads,
// on whatever goroutine a probe's request runs on.
func (l *servingLatch) serving() bool { return l.served.Load() }

// serve runs the API on the listener the daemon already took until ctx is done or the server
// fails, and returns once it is closed: one goroutine serves, the other shuts down, and the
// shutdown runs on a context of its own so a cancelled ctx still drains the connections it has.
// The moment serving ends is the moment the daemon's stop begins: it halts supervision before
// anything waits — the workflow's outbox and the controller keeper would otherwise wait on a
// machine whose relaunch holds it, and the stop on them — and then drains the API until drainBy,
// letting the decisions its routes asked for finish, and ends them when the drain does (endDrain).
func serve(ctx context.Context, cfg config.Config, st *store.Store, startedAt time.Time, bootID int64, listener net.Listener, s *supervision, p plan, workflow *workflowRuntime) error {
	records := record.Store(record.NewStore())
	var handlers []intake.Handler
	var client dispatch.Client
	var tokens appauth.Tokens
	var grants *credential.Grants
	var claimReady func(supervise.Claim)
	var handoffCompleted func(string)
	var prepare func(context.Context, intake.DispatchIssue) (intake.DispatchIssue, error)
	var unread func(string) bool
	var await func(repo, tree, child, parent string)
	var githubAPI string
	var consumerLag func(context.Context) []intake.ConsumerLag
	if workflow != nil {
		records, handlers, client, tokens, grants = workflow.records, workflow.handlers, workflow.dispatch, workflow.tokens, workflow.grants
		claimReady, prepare, unread, await = workflow.claimReady, workflow.prepareObserved, workflow.repositoryUnread, workflow.awaitRecovery
		handoffCompleted = workflow.handoffCompleted
		githubAPI = workflow.githubAPI
		consumerLag = consumerLagOf(workflow)
	}
	// Under `controller: daemon` the keeper launches and keeps the project's controller, and takes
	// its ready. The liveness sweep runs beside it under either mode, never behind it. Both the
	// launch mode and the wake are a holder of their own (keeperHolder): a change of either stops
	// or starts the keeper's loop and moves the tick, and no running controller is touched.
	var keeper *controllerKeeper
	// ensureKeeper builds the keeper the first time one is needed and answers the one there is:
	// boot builds it under `controller: daemon`, and the holder's start builds it where a change
	// moved the launch mode to the daemon while the daemon runs.
	ensureKeeper := func() *controllerKeeper {
		if keeper == nil {
			keeper = newControllerKeeper(s.supervisor.ctx, s.supervisor, p.project, p.controllerRetry, s.log)
		}
		return keeper
	}
	if cfg.ControllerLaunch == config.ControllerLaunchDaemon {
		ensureKeeper()
	}
	// The settings the API server holds, behind the pointer its routes read through: the operator
	// bearer, the design gate, the repository equivalence and the settings route's own admission
	// rule. Its socket is the one boot took and is never rebound.
	held := newAPIHolder(cfg, p.operatorToken)
	p.rebuild.register(held)
	// What the state reports as `settings`, rebuilt from the configuration at every applied
	// change rather than read at every request: a change through either arm moves the value the
	// state shows, which is what a reader of the Legion view's settings control sees (A5).
	reported := newReportedSettings(cfg)
	p.live.OnChange(reported.observe)
	// Where each setting of the running daemon came from, which the state carries beside its
	// settings: the file this boot read, until a change of that path names another source. It is
	// an observer of the one settings holder, as the rebuilder is, so a change by the file and a
	// change by the route are recorded on the one path both take (config.Live.Apply). The route
	// changes this boot laid back over the file are its first word: they are what the daemon is
	// running on, and the state names them the route's, with who asked and when.
	sources := api.NewSettingProvenance(startedAt)
	sources.Restore(restoredSources(p.restored))
	p.live.OnChange(sources.Observe)
	group, serving := errgroup.WithContext(ctx)
	// The keeper's loop runs under a context of its own, so the holder can stop it when
	// `controller` moves to the operator and start it again when it moves back, and ends with
	// serving either way. The holder is the one thing that starts it (keeperHolder.begin) and
	// the one thing that knows whether this daemon launches the controller now, which the API
	// reads at every request that turns on it. The loop runs beside every other goroutine of the
	// process, so it takes a hold mark of its own (store.ForConcurrentWorker).
	keeping := newKeeperHolder(cfg.ControllerWakeInterval, keeper != nil, func() context.CancelFunc {
		kept, stop := context.WithCancel(serving)
		running := ensureKeeper()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			running.run(store.ForConcurrentWorker(kept), p.orphanSweep)
		}()
		return stop
	})
	p.rebuild.register(keeping)
	if workflow != nil {
		workflow.keeper = keeping
	} else {
		// The project's repository mapping as a holder of the deployment's repository
		// equivalence: a change of `repository_renames` reaches it as it reaches the API
		// server's settings, the intake consumers and the runtime's gh bundles
		// (config.Row.Also). This is the registration of a daemon with no workflow, which
		// holds no intake consumers for the mapping to be ordered against; a daemon with one
		// registers the same holder as it opens them, before them and for the order's sake
		// (workflowRuntime.connect).
		p.rebuild.register(newMappingHolder(p.repositories))
	}
	// The readiness latch the API's /readyz answers from. It is built here, unset: by the time
	// this runs the runtime is built and the launcher acceptor installed, and what is left of the
	// boot is the server goroutine below.
	ready := &servingLatch{}
	server := api.NewServer(cfg.Bind, cfg.Port, api.Options{
		State: &source{
			store:        st,
			records:      records,
			project:      cfg.Project,
			projectToken: p.project,
			runtime:      cfg.Runtime.Name,
			admissionCap: func() int { return p.live.Snapshot().AdmissionCap },
			startedAt:    startedAt,
			secretsLogin: p.secretsLogin,
			deployment:   s.deployment,
			reports:      s.capabilityReports,
			wakes:        s.waitingWakes,
			repositories: p.repositories,
			caches:       repositoryCachesOf(s.runtime),
			live:         p.live,
			settings:     reported.get,
			sources:      sources,
			commit:       p.commit,
			// The same rule the lane itself reads, over the same configuration, so the count
			// the state serves and the claims the pass posts are one set (refreshStalePlugins,
			// and the NoPluginInstall option three lines below).
			noPluginInstall: func(c supervise.Claim) bool { return noPluginInstall(cfg, p.live, c) },
		},
		StateTransactions: st,
		ConsumerLag:       consumerLag,
		CallerPod:         callerPodOf(s.runtime),
		Ready:             ready.serving,
		Supervisor:        s.supervisor,
		PodUpdateRounds:   s.podUpdateRoundsView,
		BootTokens:        s.tokens,
		Project:           p.project,
		DispatchProject:   cfg.Project,
		Settings:          held.settings,
		// Through the rebuild observer, so a path whose holder the change could not build again
		// is answered failed and not `rebuilt` (rebuilder.route).
		ApplySettings:      p.rebuild.route(p.settings.Route),
		Controller:         st,
		ControllerLaunched: keeping.launchedNow,
		PluginRelaunchable: p.pluginRelaunchable,
		NoPluginInstall:    func(c supervise.Claim) bool { return noPluginInstall(cfg, p.live, c) },
		// The harness a role's claims run on is read as the route asks, like every other value
		// the live holder keeps. `worker_backends` is a launch key, so between a change and a
		// role's relaunch this answers for the new harness while the session still runs the old
		// one; the claim records no backend, so the live value is the closest there is.
		Backend:            func(role claim.Role) runtime.Backend { return p.live.Snapshot().BackendFor(role) },
		Stopping:           s.stopping,
		Drained:            s.draining,
		Decisions:          s.decided,
		Log:                s.log,
		Pool:               st.Pool(),
		Handlers:           handlers,
		PrepareRepository:  prepare,
		RepositoryUnread:   unread,
		AwaitRecovery:      await,
		RoleFor:            phase.RoleFor,
		Record:             records,
		Dispatch:           client,
		Tokens:             tokens,
		GitHubRepositories: p.repositories.Accepted,
		Serving:            p.repositories.serving,
		GitHubAPI:          githubAPI,
		Grants:             grants,
		Releaser:           s.supervisor.deps.Runtime,
		Trees:              st,
		Memory:             cfg.Projects[cfg.Project].Memory,
		ClaimReady:         claimReadyHook(keeper, claimReady),
		HandoffCompleted:   handoffCompleted,
		PromptAgents:       promptAgents(p.roleReferences),
		CapabilityReported: s.capabilityReported,
		ReleaseRecords:     st,
		ClaimActivity:      st,
	})

	serverStarted := make(chan struct{})
	group.Go(func() error {
		close(serverStarted)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve the API on %s: %w", server.Addr, err)
		}
		return nil
	})
	// Every applied settings change, by either arm, reaches the daemon's log and the project's
	// controller (settingsAnnouncer), and the file under the daemon is watched for the other arm
	// (settings.Watcher). Both are registered here, after the boot's own restore: what the boot
	// laid back over the file is what the daemon booted on, not a change to announce.
	publisher := newPublisherHolder(envoyPublisher(cfg, p.secrets["ENVOY_TOKEN"]))
	p.rebuild.register(publisher)
	// The controller's topic is the project token's, as every other notice of its is
	// (notify.ControllerTopic), never the Dispatch project key.
	announcer := newSettingsAnnouncer(p.project, publisher, s.log)
	p.live.OnChange(announcer.observe)
	group.Go(func() error {
		announcer.run(serving)
		return nil
	})
	watcher := settings.NewWatcher(p.settings, s.log)
	// A credential rotated in place names no changed setting — its path is what the
	// configuration holds, and the path has not moved — so the file arm is also what asks every
	// holder that reads a credential file to read it again (rebuilder.rereadCredentials). It
	// runs under the settings holder's own lock (config.Live.WhileSettled), where an applied
	// change's rebuilds run, so a reread and a route change never build one holder at once.
	watcher.Settled = func([]config.Change) { p.live.WhileSettled(p.rebuild.rereadCredentials) }
	// The file arm writes what the file took back through the pool (internal/settings'
	// Keeper.File), and runs beside every other goroutine of this group for the life of the
	// process, so it takes a hold mark of its own (store.ForConcurrentWorker).
	group.Go(func() error {
		watcher.Run(store.ForConcurrentWorker(serving))
		return nil
	})
	// The daemon serves from the moment that goroutine has begun, which is what the latch
	// records. While the boot runs the API is not served at all, so /readyz gives no answer;
	// between that goroutine's start and the latch it answers 503, from here 200, and only the
	// stop makes it 503 again (api.Options.Stopping). The hook before the latch is the seam a test
	// holds it at.
	group.Go(func() error {
		select {
		case <-serverStarted:
		case <-serving.Done():
			return nil
		}
		if p.beforeServingLatched != nil {
			p.beforeServingLatched(serving)
		}
		ready.reach()
		return nil
	})
	// The report is advisory: its transaction must never sit in the path from restored claims to a
	// serving API. Start it only after the server goroutine has begun and bound its store read; a
	// lock or unavailable connection is a report error, not a failed or unhealthy boot.
	group.Go(func() error {
		select {
		case <-serverStarted:
		case <-serving.Done():
			return nil
		}
		// The report runs beside every other goroutine of this group for as long as it takes, so
		// it reads under a hold mark of its own (store.ForConcurrentWorker) and its one
		// transaction refuses nothing but itself a second connection.
		report, cancel := context.WithTimeout(store.ForConcurrentWorker(serving), strandedReportTimeout)
		defer cancel()
		reportStranded(report, st, projectRecords{Store: record.NewStore(), project: cfg.Project}, cfg.Project, p.project, cfg.Gates.Steps(), s.log)
		if p.strandedReported != nil {
			p.strandedReported()
		}
		return nil
	})
	group.Go(func() error {
		<-serving.Done()
		s.halt(context.Cause(serving))
		drain, cancel := context.WithDeadline(context.WithoutCancel(ctx), s.drainBy)
		defer cancel()
		err := server.Shutdown(drain)
		s.endDrain()
		if errors.Is(err, context.DeadlineExceeded) {
			// The drain's bound is the stop's, not a failure: what still ran ends now (endDrain).
			s.log.Info("legion daemon stopped draining the API; the requests still in flight end with the stop", "project", cfg.Project)
			return nil
		}
		return err
	})
	if workflow != nil {
		// The workflow's loops, the boot fact, the orphan keeper and the controller watch each run
		// for the life of the process beside the others, so each takes a hold mark of its own
		// (store.ForConcurrentWorker): a mark they shared would have one loop's open transaction
		// refuse the rest their first connection.
		group.Go(func() error { return workflow.run(store.ForConcurrentWorker(serving)) })
		// The boot fact runs beside the already-started API server, so a slow census of claims
		// never delays /healthz or repeats LEGION-653's boot-time deadlock. A failure while the
		// daemon serves is retried (runBooted); only the daemon's stop ends it unapplied.
		group.Go(func() error {
			workflow.runBooted(store.ForConcurrentWorker(serving), bootID)
			return nil
		})
	}
	// The keeper's loop, where this daemon launches the controller itself: started through the
	// holder, which is also what a change of `controller` starts and stops it through, so the
	// loop has one beginning (keeperHolder.begin).
	keeping.begin()
	group.Go(func() error {
		watchController(store.ForConcurrentWorker(serving), st, cfg, p, s.log)
		return nil
	})
	return group.Wait()
}

// source answers the state route out of one repeatable-read snapshot: the daemon itself, the cap
// it admits under, every stored claim filed under the issue it is on, and the project's controller.
// A machine persists its claim before it publishes it to supervision, so the durable snapshot
// prevents a claim view and strandedClaims from describing different instants.
type source struct {
	store        *store.Store
	records      record.Store
	project      string
	projectToken string
	runtime      string
	// admissionCap is the cap the state reports, read at every read of the state: a change of
	// `admission_cap` takes effect at once, with nothing rebuilt.
	admissionCap func() int
	startedAt    time.Time
	// secretsLogin is the daemon's own agent-secrets token login client (newSecretsLogin), read
	// through LoginStatus for the state route; nil when the deployment enrolls no pod.
	secretsLogin *agentsecrets.Client
	// deployment is the deployment's capabilities as this boot knows them (supervision.deployment),
	// whose report the state carries.
	deployment func() capabilities.Deployment
	// reports is each claim's latest capability report (supervision.capabilityReports), which the
	// projection shows on the claim's view whatever process reported it.
	reports func() map[claim.Token]capabilities.Report
	// wakes is every wake the daemon's machines hold, as each machine last published it
	// (supervision.waitingWakes); nil for a daemon that supervises nothing.
	wakes func() []supervise.WaitingWake
	// repositories is the project's repository mapping as the workflow serves it, empty for a
	// daemon with no workflow, whose state serves no repository.
	repositories *repositories
	// caches is the Kubernetes runtime's repository caches with their last health reads
	// (sandbox.Runtime.RepositoryCaches); nil under a runtime that keeps none.
	caches func() []sandbox.CacheStatus
	// live is the daemon's settings holder, which the state's design-gate steps are read from
	// (`gates.design`, gatedStepsOf): the steps the deployment gates, which every root of the
	// state answers its own state of, and the empty list gates none. They are read at every read
	// of the state, so an operator's change of the gate reaches the next read with nothing
	// rebuilt. A source a test built by hand holds none, and reads the shipped list.
	live *config.Live
	// settings is the resolved configuration as the state shows it, read at every read of the
	// state: it is rebuilt at every applied settings change (reportedSettings), so the state
	// reports the value the daemon is running on beside where that value came from.
	settings func() api.Settings
	// sources is where each setting came from (api.SettingProvenance), read at every read of the
	// state: it moves with every change the daemon applies, so the state names the file or the
	// route per path as it stands now.
	sources *api.SettingProvenance
	// commit is the commit this daemon's binary was built from, which the daemon block names.
	commit string
	// noPluginInstall is whether a claim's launch installs no plugin at all (the package's
	// noPluginInstall over this daemon's configuration), the one term of the stale-plugin lane's
	// own rule that the claim itself does not carry, which State needs to count what that lane
	// has left (api.State.ClaimsToRefresh). Nil is every launch installing a plugin, which only
	// a source a test builds by hand reads as: serve sets this field over the daemon's own
	// configuration, whatever the runtime.
	noPluginInstall func(supervise.Claim) bool
}

// projectRecords scopes the shared daemon database to the daemon's configured project without
// widening record.Store's fixed transaction interface; the workflow and the state route read the
// store through it. Every read that starts from Issues or QueuedRootsUnder is within this
// filtered set. Slots are not filtered here: a slot's index is unique across every project's
// slots, so admission chooses the next one over all of them and counts only its own against its
// cap (admit.ownSlots). The outbox's claim is scoped in its SQL instead, because the lease it
// takes is itself the damage.
//
// It is also where the outbox learns of its rows: every write that makes a row due — Enqueue, and
// ExpediteOutbox — wakes the row's lane (outbox.wake) once the transaction has committed, through
// the transaction's commit hooks (intake.OnCommit), so no lane runs a row it cannot yet read, and
// no lane polls for one. wake is nil where there is no workflow to wake: the API-only daemon, the
// state route's reads and the stranded report.
type projectRecords struct {
	record.Store
	project string
	wake    func(issue string, kind record.OutboxKind)
	// log says when a row was written outside a commit scope; nil is the default logger.
	log *slog.Logger
}

func (s projectRecords) Issues(ctx context.Context, tx pgx.Tx) ([]record.Issue, error) {
	all, err := s.Store.Issues(ctx, tx)
	if err != nil {
		return nil, err
	}
	return s.own(all), nil
}

func (s projectRecords) QueuedRootsUnder(ctx context.Context, tx pgx.Tx, key string) ([]record.Issue, error) {
	all, err := s.Store.QueuedRootsUnder(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	return s.own(all), nil
}

// own is every issue of all in the daemon's project, in all's order.
func (s projectRecords) own(all []record.Issue) []record.Issue {
	issues := make([]record.Issue, 0, len(all))
	for _, issue := range all {
		if issue.Project == s.project {
			issues = append(issues, issue)
		}
	}
	return issues
}

func (s projectRecords) Enqueue(ctx context.Context, tx pgx.Tx, row record.OutboxRow) error {
	if err := s.Store.Enqueue(ctx, tx, row); err != nil {
		return err
	}
	s.wakeOnCommit(ctx, row)
	return nil
}

// EnqueueGatedSteps wakes the board lane as Enqueue does. Its row belongs to no issue — its
// issue column is the bare project key (record.GatedStepsPublish) — and it is written by the
// boot, where there is no outbox yet and the wake reaches nothing, and by the holder of
// `gates.design`, where a live change of the design gate would otherwise wait on the lane's
// next sweep before a person reading Dispatch saw what the deployment now holds work for
// (gatedStepsHolder).
func (s projectRecords) EnqueueGatedSteps(ctx context.Context, tx pgx.Tx, publish record.GatedStepsPublish, nextAt time.Time) error {
	if err := s.Store.EnqueueGatedSteps(ctx, tx, publish, nextAt); err != nil {
		return err
	}
	s.wakeOnCommit(ctx, record.OutboxRow{Issue: publish.Project, Kind: record.OutboxKindGatedStepsPublish})
	return nil
}

// EnqueueStart wakes the row's lane as Enqueue does: a phase worker's start is written through
// this statement since the start became a row of its own (workflow.StartOrOwe,
// record.Store.EnqueueStart), so without the wake here the worker whose phase has just been
// handed its work would wait for the lane's next sweep, up to one outbox lease after its
// transition committed.
func (s projectRecords) EnqueueStart(ctx context.Context, tx pgx.Tx, row record.OutboxRow) error {
	if err := s.Store.EnqueueStart(ctx, tx, row); err != nil {
		return err
	}
	s.wakeOnCommit(ctx, row)
	return nil
}

// EnqueueID wakes the row's lane as Enqueue does: a start or catch-up naming kept decisions is
// written with its id (workflow.EnqueueNaming), so the row it claims them for waits for no sweep.
func (s projectRecords) EnqueueID(ctx context.Context, tx pgx.Tx, row record.OutboxRow) (int64, error) {
	id, err := s.Store.EnqueueID(ctx, tx, row)
	if err != nil {
		return 0, err
	}
	s.wakeOnCommit(ctx, row)
	return id, nil
}

func (s projectRecords) ExpediteOutbox(ctx context.Context, tx pgx.Tx, row record.OutboxRow, now time.Time) error {
	if err := s.Store.ExpediteOutbox(ctx, tx, row, now); err != nil {
		return err
	}
	s.wakeOnCommit(ctx, row)
	return nil
}

// EnqueueDesignGates wakes the issue's board lane as Enqueue does. The enqueue writes its row
// with a statement of its own rather than through Enqueue (record.Postgres.EnqueueDesignGates),
// so this is the one place that can tell the lane a gate change left a publication due.
func (s projectRecords) EnqueueDesignGates(ctx context.Context, tx pgx.Tx, publish record.DesignGatesPublish, nextAt time.Time) error {
	if err := s.Store.EnqueueDesignGates(ctx, tx, publish, nextAt); err != nil {
		return err
	}
	s.wakeOnCommit(ctx, record.OutboxRow{Issue: publish.Issue, Kind: record.OutboxKindDesignGatesPublish})
	return nil
}

// PutSlot grants the slot and then wakes the issue's lifecycle lane once the transaction
// commits. A grant writes a slots row and no outbox row, so nothing would otherwise tell the
// lane that anything changed — and the thing the grant releases is an outbox row: the worker
// start the transition left unclaimable until the issue held a slot
// (record.SuperviseRequest.NeedsSlot). Without this wake that task would sit until the lane's
// next sweep, up to one outbox lease behind the grant. The wake belongs here rather than in
// record.Store for the same reason every other wake does: the store is the daemon's
// transactional record and knows nothing of the lanes that run beside it.
func (s projectRecords) PutSlot(ctx context.Context, tx pgx.Tx, slot record.Slot) error {
	if err := s.Store.PutSlot(ctx, tx, slot); err != nil {
		return err
	}
	s.wakeOnCommit(ctx, record.OutboxRow{Issue: slot.Issue, Kind: record.OutboxKindSupervise})
	return nil
}

// wakeOnCommit wakes the row's lane once the transaction that wrote it commits. A write made
// outside a commit scope (intake.WithCommitHooks) has no commit to wait for: its lane is woken at
// once, and the write is logged with its caller, since a lane woken before the row is visible
// finds nothing and the row then waits for the sweep — every transaction that writes a row is
// meant to be scoped, and one that is not is a bug to fix where the log names.
func (s projectRecords) wakeOnCommit(ctx context.Context, row record.OutboxRow) {
	if s.wake == nil {
		return
	}
	if intake.CommitScoped(ctx) {
		intake.OnCommit(ctx, func() { s.wake(row.Issue, row.Kind) })
		return
	}
	log := s.log
	if log == nil {
		log = slog.Default()
	}
	// Caller 2 is the caller of Enqueue or ExpediteOutbox, past this helper and the method.
	_, file, line, _ := goruntime.Caller(2)
	log.Error("outbox row written outside a commit scope; its lane is woken at once",
		"kind", row.Kind, "issue", row.Issue, "file", file, "line", line)
	s.wake(row.Issue, row.Kind)
}

// State is the project's state through tx, and the project's claims it read through tx to build
// it (api.StateSource): the route reads the caller's plugin contract from those claims, so the
// body and the contract it is cut to come from one snapshot.
func (s *source) State(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	version, err := s.store.SchemaVersionTx(ctx, tx)
	if err != nil {
		return api.State{}, nil, err
	}
	boots, firstBootAt, err := s.store.BootsTx(ctx, tx, s.project)
	if err != nil {
		return api.State{}, nil, err
	}
	claims, err := s.store.ClaimsTx(ctx, tx)
	if err != nil {
		return api.State{}, nil, err
	}
	claims = ofProject(claims, s.projectToken)
	records := projectRecords{Store: s.records, project: s.project}
	now := time.Now().UTC()
	// The gate is read once for this state, so the issues' steps and the stranded claims' are
	// the same list even if an operator moves the gate while the body is being built.
	steps := gatedStepsOf(s.live)
	// What nothing will wake, judged before the projection and handed to it: an issue whose
	// claim is stranded carries that answer on its own view (api.WhyView), and the judgement is
	// the daemon's own over the store, the gated steps and the released wakes, which
	// internal/projection may not read. It is read through tx like every other durable fact
	// here, so the body is still one snapshot, and it is what the state serves as
	// `strandedClaims` below.
	resumes, err := strandedReleaseWakes(ctx, s.store, tx, s.projectToken, claims, now)
	if err != nil {
		return api.State{}, nil, err
	}
	stranded, err := workflow.Stranded(ctx, tx, records, s.project, steps, claims, resumes, now)
	if err != nil {
		return api.State{}, nil, err
	}
	state, err := projection.Project(ctx, tx, records, s.project, steps, claims, stranded, s.reports(), now)
	if err != nil {
		return api.State{}, nil, err
	}
	state.StrandedClaims = api.StrandedClaimsOf(stranded)
	// A wake waits in one of two places, and the state names both as one list (api.WakesWaitingOf):
	// a launch waits in its claim's machine, in memory alone, since no store holds one and a
	// restart raises the wave again through its own paths; a paced catch-up waits on its
	// outbox row, read here in the snapshot every other durable fact of this state is read from.
	paced, err := pacedCatchUps(ctx, tx, records, s.projectToken, s.project, now)
	if err != nil {
		return api.State{}, nil, err
	}
	var held []supervise.WaitingWake
	if s.wakes != nil {
		held = s.wakes()
	}
	state.WakesWaiting = api.WakesWaitingOf(held, paced)
	commit := s.commit
	state.ClaimsToRefresh = claimsToRefresh(claims, s.noPluginInstall)
	state.Daemon = api.DaemonInfo{
		Project:       s.project,
		Commit:        &commit,
		SchemaVersion: version,
		Boots:         boots,
		FirstBootAt:   firstBootAt,
		StartedAt:     s.startedAt,
	}
	if s.settings != nil {
		state.Settings = s.settings()
	}
	// Written whatever it holds for a caller at the contract that added it, so the map is never
	// nil here: nil is what withoutSettingSources leaves on the copy served below that contract,
	// and it is the only thing that takes the field off the wire (api.State's `omitzero`).
	state.SettingSources = map[string]api.SettingSource{}
	if s.sources != nil {
		state.SettingSources = s.sources.Sources()
	}
	state.Admission.Cap = s.admissionCap()
	controller, _, err := s.store.ControllerTx(ctx, tx, s.projectToken)
	if err != nil {
		return api.State{}, nil, err
	}
	state.ControllerLocator = api.ControllerLocatorOf(s.runtime, controller)
	if s.secretsLogin != nil {
		state.AgentSecretsLogin = &api.AgentSecretsLoginView{State: s.secretsLogin.LoginStatus().State}
	}
	state.Capabilities = api.CapabilityStatesOf(s.deployment().Report())
	if s.repositories != nil {
		var caches []sandbox.CacheStatus
		if s.caches != nil {
			caches = s.caches()
		}
		state.Repositories = s.repositories.view(caches)
	}
	return state, claims, nil
}

// adoptIdentity is the supervisor's identity for a working copy's adoption (supervise.Deps.Identity)
// over identity, for the repository the claim's issue works (repository): its record's, or the
// project's default repository for an issue the workflow does not record. The owner the lease is
// minted for is identity's own to decide, since a record written before a transfer names an owner
// whose installation no longer holds the repository. nil without a workflow.
func adoptIdentity(identity func(context.Context, claim.Role, ghrepo.Repository) (runtime.GitIdentity, error), repository func(context.Context, string) (ghrepo.Repository, string, error)) func(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
	if identity == nil || repository == nil {
		return nil
	}
	return func(ctx context.Context, role claim.Role, issue string) (runtime.GitIdentity, error) {
		repo, _, err := repository(ctx, issue)
		if err != nil {
			return runtime.GitIdentity{}, err
		}
		return identity(ctx, role, repo)
	}
}

// repositoryCachesOf is rt's repository caches when rt is the Sandbox runtime, the one runtime
// that keeps them, and nil under any other, so the caller hands the API nothing to ask.
//
// The caches are read over the Sandbox runtime behind rt at the moment of the call (sandboxOf),
// which is what makes the answer follow a rebuild of the runtime: the holder's readers hold the
// holder, and the runtime inside it is replaced under them. A runtime that is no Sandbox one by
// then answers nothing. callerPodOf reads its pod the same way.
func repositoryCachesOf(rt runtime.Runtime) func() []sandbox.CacheStatus {
	if _, sandboxed := sandboxOf(rt); !sandboxed {
		return nil
	}
	return func() []sandbox.CacheStatus {
		sandboxed, ok := sandboxOf(rt)
		if !ok {
			return nil
		}
		return sandboxed.RepositoryCaches()
	}
}

// callerPodOf is rt's pod at an address when rt is the Sandbox runtime, the one runtime that
// runs pods, and nil under any other (api's Options.CallerPod).
func callerPodOf(rt runtime.Runtime) func(netip.Addr) (string, bool) {
	if _, sandboxed := sandboxOf(rt); !sandboxed {
		return nil
	}
	return func(addr netip.Addr) (string, bool) {
		sandboxed, ok := sandboxOf(rt)
		if !ok {
			return "", false
		}
		return sandboxed.PodAt(addr)
	}
}

// consumerLagOf is the lag of workflow's durable consumers (api's Options.ConsumerLag), read from
// the consumers its NATS holder holds at each call (natsHolder.intake): serve builds it once, at
// boot, and a rebuild swaps the consumers and closes the connection the boot's were on, so a
// read bound to the boot's would report every consumer unavailable from then on.
func consumerLagOf(workflow *workflowRuntime) func(context.Context) []intake.ConsumerLag {
	return func(ctx context.Context) []intake.ConsumerLag { return workflow.nats.intake().Lag(ctx) }
}
