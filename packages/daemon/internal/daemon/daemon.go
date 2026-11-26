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
	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/cipherclient"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/cipher"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
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
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	// bootTimeout bounds each stretch of the work between the plugin gate passing and the API
	// listening, so a dependency that takes a connection and never answers does not leave the
	// daemon hanging with nothing on stderr. The gate is not bounded by it — it waits out host
	// load for as long as that lasts (pluginGate) — nor are the store's open, the GitHub Apps'
	// logins and key proof, and the image probe, which wait out a connection error the next attempt
	// can clear, GitHub's transient trouble and the cluster's within retries of their own
	// (storeOpenRetry, appCallBounds.retry, imageProbeRetry); the work after each has a budget anew.
	bootTimeout = 30 * time.Second
	// storeOpenAttempt bounds one attempt to open the store (storeOpenRetry). With no
	// connect_timeout in the DSN, pgxpool bounds a connection it opens at two minutes, so without
	// this bound a connection the database takes and never answers would hold the attempt that long.
	storeOpenAttempt = 10 * time.Second
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
	// for the work the drain's end cuts short to return before the budget runs out. A commit whose
	// caller's context the stop or the drain's end cancels waits at most store.CommitSettleBound
	// more for its outcome, counted from that cancel (store.Transact), which fits the budget's last
	// fifth (TestTheCommitSettleFitsTheStopsLastFifth). Work still running at the budget's end, a
	// call into a process that does not answer, is logged with each claim still deciding and not
	// waited on: the next boot re-adopts each process a claim records and relaunches each launch
	// left unrecorded. natsCloseTimeout bounds the close of the
	// workflow's NATS connection, stampTimeout the stamp, and storeCloseTimeout the store's close
	// after it, so the daemon returns at most
	// stopBudget+natsCloseTimeout+stampTimeout+storeCloseTimeout (24 s) after its stop begins,
	// inside the 30 seconds a pod is given between SIGTERM and SIGKILL. Before the boot is
	// recorded, a signal ends the daemon through each boot step's own bounds, as Run says.
	stopBudget = 10 * time.Second
	// natsCloseTimeout bounds the close of the workflow's Envoy NATS connection on the stop path
	// (closeWorkflowNATS). nats.Conn.Close flushes the outbound buffer while it holds the
	// connection's own lock (nats.go v1.55.0, nats.go:6242-6312: nc.bw.flush() when the socket is
	// up and no reconnect is in progress), that write goes through timeoutWriter, whose deadline
	// is Opts.FlusherTimeout (nats.go:2341-2346, :7026-7029), and the default is a minute
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
	// listener likes, and it is at most half of stopBudget, so a stop the daemon's stop waits for
	// (sessionStopsInFlight) that was sent before the halt or in the first half of the stop's
	// budget ends inside that budget. What keeps a stop that does land late off the relaunch's
	// registration is the two conditions the stop carries (cleanSessionStop), not this bound.
	cleanSessionStopBudget = 5 * time.Second
)

// storeOpenRetry is the wait between two attempts to open the store whose error the next attempt
// can clear (store.OpenRetryable): 1 s doubling to 16 s, ten attempts. The waits are 1, 2, 4, 8
// and five times 16 s, 95 s in all, so with each attempt at its limit (storeOpenAttempt) the start
// refuses at most 3 min 15 s after its first attempt, naming the last error. The numbers are a
// choice, not a measurement: no production start has met a connection error on this path, and
// the bound is longer than the 60 s the Cloud SQL proxy sidecar's startup probe lets a restarted
// proxy take, and shorter than the 14-minute progress deadline of the daemon's Deployment
// (dispatch://INFRA-2597/spec R3).
var storeOpenRetry = bootprobe.Retry{Initial: time.Second, Max: 16 * time.Second, Attempts: 10}

// storeOpenLongest is the longest the store's attempts take under retry: every attempt at its
// limit (storeOpenAttempt), and every wait between two of them as bootprobe.Run waits it
// (bootprobe.Retry.Delay).
func storeOpenLongest(retry bootprobe.Retry) time.Duration {
	longest := time.Duration(retry.Attempts) * storeOpenAttempt
	for transients := range retry.Attempts - 1 {
		longest += retry.Delay(transients)
	}
	return longest
}

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
	// outboxSweep is how often the outbox sweeps its lanes (outbox.sweepEvery, which attach hands
	// it); zero is outboxLease.
	outboxSweep time.Duration
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
	// mergeStateRetry is how long the merge-state ladder, the merge-queue read and the second read
	// of a conflicting head wait between two reads of GitHub, and the unit the reads after a
	// ladder wait in (workflowRuntime.mergeStateRetry, read through mergeStateWait); zero is the
	// production mergeStateLadderWait. A whole-daemon test of those reads shortens it, so it waits
	// out its own wait rather than half a minute a read.
	mergeStateRetry time.Duration
	// catchUpRetry is the wait between two reads of one root the boot's catch-up makes after
	// Dispatch refused it with an error it may stop giving (workflowRuntime.catchUpRetryDelay);
	// zero is the production catchUpRetryDelay. A whole-daemon test whose Dispatch stand-in refuses
	// the catch-up's reads shortens it, so each of its boots does not wait out two seconds a read.
	catchUpRetry time.Duration
	// appCalls are the bounds of the start's App calls (appCallBounds): their attempt, the retry
	// between attempts that failed transiently, the rate-limit policy, and the clock every wait of
	// theirs reads and waits on; nil is productionAppCalls. A test of the boot's App calls shortens
	// them, or puts their waits on a clock of its own, for the daemons it boots alone, so it runs
	// beside every other test.
	appCalls *appCallBounds
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
	// storeOpenRetry is the policy of the boot's attempts to open the store; nil is the production
	// storeOpenRetry. A test of the ten attempts shortens the waits between them, so it waits out
	// milliseconds rather than 95 s.
	storeOpenRetry *bootprobe.Retry
	// workflowTokens replaces the GitHub App token manager in a workflow integration test. The
	// production daemon always mints through appauth.New.
	workflowTokens appauth.Source
	// githubAPI is the GitHub REST root a workflow integration test points the workflow at (its
	// required-checks reads and its issue branches' creates) and the Kubernetes runtime's read of
	// the shared public leak check at (sandboxRuntime); empty, in production, is
	// https://api.github.com.
	githubAPI string
	// listen takes the API listener for cfg's bind and port; nil binds them with net.Listen. Run's
	// serves the socket `legion start` inherited from LEGION_LISTEN_FD (handed); a test's hands
	// over a listener it holds.
	listen func(network, address string) (net.Listener, error)
	// streamListen takes the worker stream's listener for the address the stream binds
	// (stream.Options.Listen); nil, in production, binds it. A test whose two daemons must listen
	// on one worker_stream_port hands over a socket it holds across both, so the port is never free
	// between its pick and a bind (controllerSandboxRig).
	streamListen func(network, address string) (net.Listener, error)
	// strandedReported runs once the boot's report of the claims nothing will wake has finished,
	// whether it logged or not (reportStranded); nil, in production, is nothing. The report runs
	// beside the API, so a test that asserts on its line waits for this rather than for /healthz.
	strandedReported func()
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
	// supervising hands a whole-daemon test the supervisor run built and a function that applies
	// one fact with the workflow's own handlers (intake.ApplyFact over the workflow's pool, as the
	// daemon's consumers, its API and its own passes apply each fact), once run has built both and
	// before any claim is restored (supervision.start). It is the seam the pause's tests post the
	// machines' events through and apply the listener's wakes, the CI settlements and the handoffs
	// through (pause_rig_test.go). A daemon with no workflow hands nothing. nil, in production, is
	// nothing.
	supervising func(sup *supervisor, apply func(ctx context.Context, source, eventID string, fact intake.Fact) (intake.Result, error))
	// streamWaitBegan runs, naming the claim, once an agent's request has queued its mark on the
	// worker stream and begins to wait for the claim's machine to handle what the stream read
	// before it (supervision.awaitStreamWithin); nil, in production, is nothing. It is the seam a
	// test learns at that the request has reached that wait, which nothing outside the daemon can
	// read: the wait writes nothing and answers nothing until the machine lets it go or its bound
	// runs out.
	streamWaitBegan func(claim.Token)
	// beforeDecision runs on a claim's feed before its machine decides each event the feed took
	// (supervisor.add), naming the claim and the event, with no lock of the machine's held; nil, in
	// production, is nothing. It is the seam a test holds an event at while the stream has read it
	// and the machine has not decided it, with the machine free for every reader: a hold inside
	// the decision would hold the machine's lock, which an agent's request waits on at its grant
	// check (supervise.Machine.Claim).
	beforeDecision func(claim.Token, supervise.Event)
	// experimentSource is the random source admission and the workflow draw every `random`
	// experiment with: admission's draw of each tree and issue it records, and the workflow's of
	// each child it records (experiment.Draw). nil, in production, is experiment.CryptoSource; a
	// test that compares a run's draws with a seed's hands in a source of that seed. The API's draw
	// at an operator's spawn keeps its own: it draws only the tree-unit experiments, and an
	// experiment that sets how an issue is done draws for each issue (config's checkExperimentPlans).
	experimentSource func(uint64) (uint64, error)
}

// Run is the daemon. It refuses what it cannot run on before it touches anything — the
// configuration first, then, under tmux, the plugin gate, which holds the Oh My Pi plugin every
// pane will load to this daemon's contract, then the credential its Dispatch client presents
// (dispatchCredential) — then opens the store (trying again a connection error the next attempt
// can clear, and refusing by the host it could not reach), migrates, takes its API listener and
// its worker stream, builds the runtime (under kubernetes, once Agent Sandbox's install check
// passes), proves the worker image under kubernetes, records the boot, supervises every claim the
// store holds, serves the API, and blocks until ctx is done. Then it stops: it says so, cancels
// the decisions it makes itself, drains the API and the decisions its routes asked for, ends the
// workflow and supervision, stamps the boot's end and closes the pool, in that order, because the
// stamp needs the pool. The waits before the stamp share one budget, which a boot that has not
// finished supervising its claims shares too (stopBudget).
//
// Everything that can refuse comes before the boot record: a recorded boot is a boot that
// served, and a daemon whose port, socket, tmux, cluster, or image refused it never ran.
//
// ctx decides one thing: how long the daemon serves. The boot record and its stamp are the
// daemon's own bookkeeping and run on a context the shutdown did not cancel, so a signal that
// arrives once the store is open still leaves a recorded, stamped boot rather than a row with no
// end. A signal that arrives while the plugin gate, the Dispatch credential's trade or read, the
// store's attempts to open, the GitHub Apps' logins and key proof, or the image probe waits ends
// the daemon there, cleanly: it has served nothing and records nothing.
//
// listener, when not nil, is the API listener the daemon serves on in place of binding cfg's bind
// and port: the socket `legion start` inherited (LEGION_LISTEN_FD), which it checked names both.
// Run closes it once it stops serving, as it closes a listener it binds; a refusal before the
// listener is taken leaves it to the caller to close.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger, listener net.Listener) error {
	return run(ctx, cfg, log, overrides{listen: handed(listener)})
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
// cancellation: a signal that arrives once the store is open still leaves a recorded, stamped boot
// (Run), so the steps between the open and the boot record end on this bound rather than on the
// signal, and the steps after it end on the signal through starting, which run derives from it.
// The store's open itself runs on ctx and outside any budget (storeOpenRetry). It carries the
// boot's hold mark (store.WithTransactionTracking), since the boot's steps run one after another
// on it: a step that asks the pool for a connection while one of its own transactions holds one
// is refused at that call (store.ErrNestedAcquire) rather than waiting out a boot that can no
// longer finish.
func bootBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(store.WithTransactionTracking(context.WithoutCancel(ctx)), bootTimeout)
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, o overrides) error {
	if log == nil {
		log = slog.Default()
	}
	plan, err := prepare(cfg, log, o)
	if err != nil {
		return err
	}
	logLeakCheck(log, cfg.LeakCheck)
	if cfg.Runtime.Name == "kubernetes" {
		logPlugins(log, plan.plugins, plan.pluginsSource)
		logProbeBudget(log, cfg)
	}
	if cfg.DispatchURL != "" {
		log.Info("legion workflow boot stage", "stage", "prompts")
	}
	if plan.gate != nil {
		if err := plan.gate(ctx); err != nil {
			if ctx.Err() != nil {
				log.Info("legion daemon stopped before its plugin gate passed", "project", cfg.Project)
				return nil
			}
			return err
		}
	}
	// The credential every Dispatch call of the daemon's presents: the bearer dispatch_token_file
	// names, or, under dispatch_token_broker, the daemon's own pod identity traded at the secrets
	// broker, whose first trade and Dispatch's reading of it wait out a broker or a Dispatch that
	// does not answer, which the boot budget does not bound, as it does not bound the plugin gate.
	environ := o.environment()
	dispatchTransport, err := dispatchCredential(ctx, cfg, plan.dispatchToken, func(name string) string {
		value, _ := envValue(environ, name)
		return value
	}, log)
	if err != nil {
		if ctx.Err() != nil {
			log.Info("legion daemon stopped before it settled its Dispatch credential", "project", cfg.Project)
			return nil
		}
		return err
	}

	// The cluster's refusals run before the store opens, so before any schema write, image probe or
	// reconcile: Agent Sandbox must be installed, and no per-claim Sandbox of the layout before issue
	// pods may remain. The claims' half of that layout fence runs once the store opens, before it
	// migrates. The check has a budget of its own (bootTimeout), which starts once the plugin gate
	// has passed, since no budget bounds the gate, and ends with the check.
	if plan.clusterCheck != nil {
		check, cancelCheck := bootBudget(ctx)
		err := plan.clusterCheck(check)
		cancelCheck()
		if err != nil {
			return err
		}
	}

	// The store opens on ctx, outside the budget the cluster check spent: a connection error the
	// next attempt can clear (store.OpenRetryable) is tried again within storeOpenRetry, each
	// attempt bounded by storeOpenAttempt, and a stop ends the attempts at once. The steps after
	// the open get a budget of their own.
	openRetry := storeOpenRetry
	if o.storeOpenRetry != nil {
		openRetry = *o.storeOpenRetry
	}
	log.Info("daemon: opening the store; a connection error that the next attempt can clear is tried again",
		"attempts", openRetry.Attempts, "attemptLimit", storeOpenAttempt.String(), "firstWait", openRetry.Initial.String(),
		"longestWait", openRetry.Max.String(), "longestTotal", storeOpenLongest(openRetry).String())
	var st *store.Store
	err = bootprobe.Run(store.WithTransactionTracking(ctx), "Postgres store", openRetry, log,
		readinessAttempt(func(attempt context.Context) error {
			var err error
			st, err = store.Open(attempt, cfg.PostgresDSN)
			return err
		}, storeOpenAttempt, store.OpenRetryable))
	if err != nil {
		if st != nil {
			// The stop came as the attempt that opened the store returned.
			st.Close()
		}
		if ctx.Err() != nil {
			log.Info("legion daemon stopped before it opened its store", "project", cfg.Project)
			return nil
		}
		return err
	}
	// The boot's budget for the steps after the open. The loops the boot starts take marks of their
	// own, since they run beside each other (store.ForConcurrentWorker).
	boot, cancelBoot := bootBudget(ctx)
	defer cancelBoot()
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
	// (workflow.Config.Reserve, supervise.WakePace.Reserve). wakeInterval is what one wake of it
	// costs, which the gate reads a launch's lateness by (supervise.WakePace.Interval).
	var (
		reserveWake  func(time.Time) time.Time
		wakeInterval time.Duration
	)
	if cfg.WorkerWakesPerMinute > 0 {
		limiter := wakepace.New(cfg.WorkerWakesPerMinute, cfg.WorkerWakeBurst, rand.N[time.Duration])
		reserveWake, wakeInterval = limiter.Reserve, limiter.Interval()
	}
	// The one gate of the stale-plugin refresh lane, built here because K is that same rate: the
	// lane may hold one minute of the pacing at a time, and the sweep a minute later admits the
	// next minute's worth (supervise.NewRefreshGate, which says what a lane without it costs).
	// It is built at every rate, the pacing off included, where it grants every request: one
	// gate to hand the machines and the refresh pass, and one place that reads a rate of zero.
	refreshGate := supervise.NewRefreshGate(cfg.WorkerWakesPerMinute)
	// The Apps' logins and the proof of each App's key wait out GitHub's transient failures within
	// the start's retry and its rate limit until the time GitHub names, within the start's own bound
	// (appCallBounds), and the boot budget bounds neither, as it does not bound the plugin gate
	// or the image probe: the work after them has a budget of its own. The key proof under the
	// App's JWT mints no token for an owner (proveAppKeys); every other step of the boot that reads
	// GitHub or writes a cache's credential mints the token that step needs. The fill of a record
	// that lacks its repository columns (backfillRepositories) makes one read per such record,
	// bounded by the read attempt bound (repositoryReadBounds.attempt), and a read that fails leaves the record for the refresher
	// rather than refusing the boot. The one-time rename pass (renameAtBoot, below) reads GitHub for
	// a configured pair only while a stored row still names the pair's `from` (renamePass.run). The
	// refresher prepares the records' repositories once the API serves (prepareAtStart), beside
	// the boot.
	// The workflow reads the machines' clock (overrides.clock, the wall clock in production), so a
	// test clock moves the engine's deadlines and the idle, wake and stuck passes with the
	// machines (dispatch://INFRA-3759).
	workflow, err := openWorkflow(ctx, cfg, st, plan.project, dispatchTransport, plan.hookPrompts, plan.repositories, plan.installed, log, o.workflowTokens, o.githubAPI, reserveWake, plan.clock.Now, o.experimentSource, o.appCalls)
	if err != nil {
		st.Close()
		if ctx.Err() != nil {
			log.Info("legion daemon stopped before it proved its GitHub Apps' keys", "project", cfg.Project)
			return nil
		}
		return err
	}
	if workflow != nil {
		var cancelAfterMint context.CancelFunc
		// A fresh budget for the rest of the boot, since the boot budget bounds neither the
		// Apps' logins nor their key proof above.
		boot, cancelAfterMint = bootBudget(ctx)
		defer cancelAfterMint()
		workflow.controllerFirstWake, workflow.idleTick, workflow.mergeStateRetry, workflow.entrySweep = o.controllerFirstWake, o.idleTick, o.mergeStateRetry, o.entrySweep
		workflow.outboxSweep = o.outboxSweep
		workflow.catchUpRetryDelay = o.catchUpRetry
		plan.identity = workflow.identity
		plan.knownIdentity = workflow.knownIdentity
		plan.repository, plan.launchRepository = workflow.issueRepository, workflow.launchRepository
		plan.ensureWorkspace = workflow.ensureWorkspace
		plan.owner = workflow.owningArchitect
		// The arms a launch runs in are read from the daemon's own store (launchVariants), not
		// from here: the operator's spawn draws a tree's experiments with no workflow
		// configured, and the launch that follows it must honour the draw it made.
		// Only a daemon with the workflow configured has issue records to read a phase or a tree
		// from; Stage 2's supervision runs on claims alone, where every delivery holds and every
		// tree closes.
		plan.phaseHolds, plan.rootHears, plan.treeClosable = workflow.phaseHolds, workflow.rootHears, workflow.treeClosable
		plan.phaseRole = workflow.phaseRole
		plan.cacheRepositories = workflow.cacheRepositories
		plan.rootTask, plan.designNews = workflow.rootTask, workflow.designNews
		plan.taskConfirmed = workflow.taskConfirmed
		plan.sessionLost, plan.sessionStoreFault = workflow.sessionLost, workflow.sessionStoreFault
		// From here on, a claim write that changes a role holder queues its issue's publication in
		// its own transaction, and the store tells the issue once that commits, so its lane is woken
		// at once rather than at its next sweep. Only a daemon with a workflow runs an outbox to send
		// the row, so only this one turns the publication on (store.PublishRoleHolders). It is turned
		// on before supervision restores a claim, so no claim write of this boot is left out.
		st.PublishRoleHolders(workflow.wakeRoleHolders)
		plan.providerErrorRunReached = workflow.providerErrorRunReached
		// The project's pause (dispatch://INFRA-3759) runs its stops on this runtime's outbox, so
		// it is built with the workflow, before supervision: the machines are handed its flag, its
		// stop transaction and its let-out stamp (openSupervision), and the boot loads its flag
		// before any claim is restored (supervision.start).
		pauses, err := newProjectPause(cfg, st, plan.project, workflow, plan.clock, log)
		if err != nil {
			workflow.stop()
			st.Close()
			return err
		}
		workflow.pauses = pauses
		// The memory guards' kill holder is filled here, once the workflow is open
		// (memoryGuardKills).
		if plan.memoryGuardKills != nil {
			plan.memoryGuardKills.bind(workflow.pool, workflow.handlers)
		}
		// The cache trimmers of this project's issue pods report through the pool and the
		// handlers the workflow has just built, which is what turns a sweep of a filling volume
		// into the fact that tells the issue's architect (volumeTrimReports).
		if plan.volumeReports != nil {
			plan.volumeReports.bind(workflow.pool, workflow.handlers)
		}
	}
	if cfg.Runtime.Name == "tmux" {
		executable, err := os.Executable()
		if err != nil {
			workflow.stop()
			st.Close()
			return fmt.Errorf("resolve this daemon's executable for the pane legion launcher: %w", err)
		}
		// The panes inherit this daemon's PATH (tmux.PaneEnvironment), so Install reads its first
		// git there to decide whether the launcher directory holds a git wrapper.
		if err := workerbin.Install(cfg.StateDir, executable, os.Getenv("PATH")); err != nil {
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

	var apps appauth.Source
	var pauses *projectPause
	if workflow != nil {
		apps, pauses = workflow.apps, workflow.pauses
	}
	s, err := openSupervision(boot, cfg, log, plan, st, apps, reserveWake, wakeInterval, refreshGate, pauses)
	if err != nil {
		listener.Close()
		workflow.stop()
		st.Close()
		return err
	}
	// The Sandbox runtime's launches read the update round over a pod they would keep through the
	// supervision, which exists only now; the runtime was built inside openSupervision before it
	// (launchRounds). No machine exists yet, so no launch has asked.
	if plan.launchRounds != nil {
		plan.launchRounds.bind(s.roundPodEndsAtLaunch)
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
		// This release's one-time rename pass (renameAtBoot, dispatch://INFRA-4401 A10), after
		// supervision opened and the worker image passed its probe, so the runtime answers which
		// issues' pods it holds, and before the consumers open, so no fact and no launch reads a
		// record while the pass writes it. Each write is bounded by its own budget, and nothing the
		// pass meets stops the boot. dispatch://INFRA-3532's own change removes it.
		workflow.renameAtBoot(store.WithTransactionTracking(ctx), s.runtime)
		// The durable consumers exist before the listing is read: a consumer created now delivers
		// only what is published after it, so everything earlier is the listing's, and what the
		// listing misses (a move published while it is read) the consumer delivers. Both wait out
		// an unreachable dependency on ctx, not the bounded boot budget just spent on everything
		// before them, through the same bootprobe.Run mechanism the Apps' key proof uses, here
		// with Attempts left at its unbounded zero. natsauth.Unreachable and dispatch.Unreachable each
		// judge their own dependency; their own docs are the record of what each one waits on and
		// what it still refuses loud. reconcile's own Postgres transaction is judged by neither: a
		// design choice, not an inability to tell its failures apart from NATS's or Dispatch's — the
		// start judges a Postgres it cannot reach earlier, where it opens the store: it tries again
		// a connection error the next attempt can clear and refuses the rest (store.OpenRetryable,
		// TestRunOpensItsStoreOnTheThirdAttemptAfterTwoResets,
		// TestRunExitsAtOnceWhenPostgresRefusesThePassword,
		// TestRunRefusesAnUnreachablePostgresByHostAndNotByPassword).
		plan.nats.log(log)
		// The open of the uncounted runs, the three probes and the cutover repair after them run
		// one after another on this goroutine, so they share one hold mark (a sequential flow);
		// whether the daemon is stopping is still read from ctx.
		probing := store.WithTransactionTracking(ctx)
		// The runs no start of this release counted, which an earlier release began, get their
		// open row first (openUncountedRuns): before the listing's reconcile, the boot's first
		// admission pass, and before any fact, so the settle of each fact ends such a run as an
		// open run, and a fact its agent's request applies waits for that agent's stream. It is
		// one Postgres transaction with a bound of its own, and a failure of it stops the boot as
		// the reconcile's own transaction's does.
		opening, cancelOpening := context.WithTimeout(probing, bootTimeout)
		err := workflow.openUncountedRuns(opening)
		cancelOpening()
		if err == nil {
			err = bootprobe.Run(probing, "connect Envoy NATS", readinessRetry, log,
				readinessAttempt(func(attempt context.Context) error {
					return workflow.connect(attempt, cfg, plan.nats)
				}, bootTimeout, natsauth.Unreachable))
		}
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
		// The repair of the mergers the `awaiting_merge` cutover left stopped is a boot step of its
		// own (repairCutoverMergers, dispatch://INFRA-3274/spec R2): after the listing's reconcile,
		// which is the boot's first admission pass, and after the roots' catch-up, and before attach
		// and the consumers, so no consumer's fact and no wake of this boot comes before it. It is
		// one Postgres transaction, and a failure of it stops the boot as the reconcile's own
		// transaction's does; its bound is its own, so a database that stops answering cannot hang
		// the boot.
		if err == nil {
			repair, cancelRepair := context.WithTimeout(probing, bootTimeout)
			err = workflow.repairCutoverMergers(repair)
			cancelRepair()
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
		if o.supervising != nil {
			o.supervising(s.supervisor, func(ctx context.Context, source, eventID string, fact intake.Fact) (intake.Result, error) {
				return intake.ApplyFact(ctx, workflow.pool, source, eventID, fact, workflow.handlers...)
			})
		}
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

// dispatchCredentialLine is the boot's one line naming the credential the daemon's Dispatch client
// presents (dispatchCredential): credential=broker and the session its token names, or
// credential=file and the file, beside transitionFile where the configuration names one. It names
// no token.
const dispatchCredentialLine = "legion daemon's Dispatch credential"

// dispatchCredential settles, at boot and before anything opens (run), the credential every
// Dispatch call of the daemon's presents, as the transport openWorkflow's client sends through.
// Without dispatch_url there is none, and it logs nothing.
//
// Without dispatch_token_broker the transport presents dispatchToken, the bearer
// dispatch_token_file names (readBoot), as the client always has (dispatch.BearerTransport). With
// it, the daemon's own pod identity, which getenv names as it does for every machine client of the
// broker (daemonBrokerClient), is traded at the secrets broker for a token of the session
// legion-daemon:<PROJECT> (dispatch.DaemonSession), presented as `Authorization: DPoP` beside a
// proof of the request's own URL (cipherclient.NewTokenTransport), and renewed ahead of its expiry
// with no restart by the client's own renewal loop, off the request path, for as long as ctx lives
// (renewDispatchCredential). Those reads and the first trade are boot's alone, never
// `legion config check`'s (readBoot; dispatch://INFRA-4425 R1).
//
// The first trade runs here, at the boot gate's pacing (readinessRetry): a refusal no retry cures
// (cipherclient.RefusedError.Permanent) refuses the boot in the broker's words, and any other
// failure is logged and tried again. Then Dispatch's reading of the token, GET /api/v1/whoami,
// waited out as the boot waits out a Dispatch that does not answer (dispatch.Unreachable), must
// name the daemon's session, its project and a person (checkDaemonCaller). After boot, a failed
// trade is a failed Dispatch call that sent nothing (dispatch.TransientError), which the outbox
// waits out as it waits out a Dispatch outage.
//
// The transition token dispatch_token_transition_file holds (sandbox.DispatchCredential.Transition)
// is never this credential: it is the sandbox runtime's alone, for a launch into a pod built before
// the setting.
func dispatchCredential(ctx context.Context, cfg config.Config, dispatchToken string, getenv func(string) string, log *slog.Logger) (http.RoundTripper, error) {
	switch {
	case cfg.DispatchURL == "":
		return nil, nil
	case !cfg.DispatchTokenBroker:
		log.Info(dispatchCredentialLine, "credential", "file", "file", cfg.DispatchTokenFile)
		return dispatch.BearerTransport(dispatchToken, http.DefaultTransport), nil
	}
	client, broker, err := daemonBrokerClient(cfg, getenv, log)
	if err != nil {
		return nil, err
	}
	session := dispatch.DaemonSession(cfg.Project)
	var held *cipherclient.Token
	err = bootprobe.Run(ctx, "daemon's Dispatch credential trade", readinessRetry, log, readinessAttempt(func(ctx context.Context) error {
		var err error
		held, err = client.Token(ctx)
		return err
	}, bootTimeout, tradeTransient))
	if err != nil {
		return nil, fmt.Errorf("dispatch_token_broker: the daemon's Dispatch credential, a token of the session %s, cannot be traded at %s: %w", session, broker, err)
	}
	transport := cipherclient.NewTokenTransport(client, http.DefaultTransport)
	daemonDispatch := dispatch.New(cfg.DispatchURL, transport)
	var caller dispatch.Caller
	err = bootprobe.Run(ctx, "Dispatch whoami of the daemon's credential", readinessRetry, log, readinessAttempt(func(ctx context.Context) error {
		var err error
		caller, err = daemonDispatch.Whoami(ctx)
		return err
	}, bootTimeout, dispatch.Unreachable))
	if err != nil {
		return nil, err
	}
	if err := checkDaemonCaller(caller, cfg.Project); err != nil {
		return nil, err
	}
	go renewDispatchCredential(ctx, client, held, session, log)
	attrs := []any{"credential", "broker", "session", session}
	if cfg.DispatchTokenTransitionFile != "" {
		attrs = append(attrs, "transitionFile", cfg.DispatchTokenTransitionFile)
	}
	log.Info(dispatchCredentialLine, attrs...)
	return transport, nil
}

// renewDispatchCredential renews the daemon's Dispatch token off the request path until ctx ends,
// so a Dispatch call presents the token in hand while it is still good, without trading for the
// next one or waiting on that trade (cipherclient.NewTokenTransport). Boot has just traded for
// held, so the loop starts when held comes due for renewal (cipherclient.Client.RenewalAt) rather
// than trade again at once. The loop is the client's own (cipherclient.Client.Run): it trades
// RenewBefore ahead of each token's expiry, and waits out a broker that does not answer from 15 s
// doubling to 5 minutes. A refusal no retry cures ends it, logged once at Error; the transport then
// trades on the call that finds no token still good, as it would with no loop, and that call
// answers the refusal.
func renewDispatchCredential(ctx context.Context, client *cipherclient.Client, held *cipherclient.Token, session string, log *slog.Logger) {
	due := time.NewTimer(time.Until(client.RenewalAt(held)))
	defer due.Stop()
	select {
	case <-ctx.Done():
		return
	case <-due.C:
	}
	if err := client.Run(ctx, func(context.Context, *cipherclient.Token) error { return nil }); err != nil {
		log.Error("legion daemon's Dispatch credential renewal stopped", "session", session, "error", err)
	}
}

// daemonBrokerClient is the broker client of the daemon's own pod identity, and the broker it
// trades at: the identity getenv names as it does for every machine client of the broker
// (cipherclient.MachineIdentity), at the broker AGENT_SECRETS_URL names, proving
// cipherclient.MachineKey's key, under the daemon's session (dispatch.DaemonSession), and declining
// the launcher credential, since the daemon enrols nothing with this identity. A variable the
// identity needs and does not have is refused by name: AGENT_SECRETS_MACHINE_IDENTITY, the
// variables the identity it names reads (MachineIdentity's own words), and AGENT_SECRETS_URL.
func daemonBrokerClient(cfg config.Config, getenv func(string) string, log *slog.Logger) (*cipherclient.Client, cipheraddr.Address, error) {
	identity, err := cipherclient.MachineIdentity(getenv)
	if err != nil {
		return nil, cipheraddr.Address{}, fmt.Errorf("dispatch_token_broker is set, so the daemon trades its own pod identity for its Dispatch credential: %w", err)
	}
	if identity == nil {
		return nil, cipheraddr.Address{}, fmt.Errorf("dispatch_token_broker is set, so the daemon's Dispatch credential is its own pod identity traded at the secrets broker, and %s names none: a daemon pod sets it to %s, with %s naming its projected service-account token",
			cipherclient.IdentityVar, cipherclient.IdentityKubernetes, cipherclient.MachineTokenFileVar)
	}
	broker := cipheraddr.New(strings.TrimSpace(getenv(cipherclient.BrokerURLVar)))
	if broker.Empty() {
		return nil, cipheraddr.Address{}, fmt.Errorf("dispatch_token_broker is set, so the daemon trades its own pod identity for its Dispatch credential, and %s names no secrets broker to trade it at",
			cipherclient.BrokerURLVar)
	}
	key, err := cipherclient.MachineKey(getenv)
	if err != nil {
		return nil, cipheraddr.Address{}, fmt.Errorf("dispatch_token_broker: the key the daemon's trade proves: %w", err)
	}
	client, err := cipherclient.New(cipherclient.Config{
		BrokerURL: broker, Key: key, Subject: identity, Session: dispatch.DaemonSession(cfg.Project),
		NoLauncherCredential: true, HTTP: cipherclient.HTTPClient(), Log: log,
	})
	if err != nil {
		return nil, cipheraddr.Address{}, fmt.Errorf("dispatch_token_broker: %w", err)
	}
	return client, broker, nil
}

// tradeTransient is every failed trade but a refusal no retry cures
// (cipherclient.RefusedError.Permanent): a broker that did not answer or answered 5xx, a 408 or its
// 429, a DPoP refusal, a pod session it holds no registration for yet, and an identity this pod
// could not read yet, such as a projected token the kubelet has not written.
func tradeTransient(err error) bool {
	var refused *cipherclient.RefusedError
	return !(errors.As(err, &refused) && refused.Permanent())
}

// checkDaemonCaller refuses a broker token Dispatch takes for a caller whose every write it would
// refuse: another session than the daemon's (403 ACTOR_SESSION_MISMATCH, every write of the
// daemon's naming its own session as the actor), another project than the daemon's (403
// PROJECT_SCOPE) or no person (SIGN_IN_REQUIRED). The outbox reads a 401 and a 403 as an outage
// (dispatch.PermanentRefusal), so it would wait on each such write for ever; each is a fault of
// the secrets broker's exchange entry for the daemon's own account, which the refusal names.
func checkDaemonCaller(caller dispatch.Caller, project string) error {
	session := dispatch.DaemonSession(project)
	const entry = "the deployment must fix the secrets broker's exchange entry for the daemon's own account (BROKER_EXCHANGE_ACCOUNTS)"
	switch {
	case caller.Session == nil || *caller.Session != session:
		return fmt.Errorf("GET /api/v1/whoami answers the session %s for the daemon's broker token, not %s, and Dispatch refuses every write the daemon makes as %s with it (403 ACTOR_SESSION_MISMATCH): %s so that it fixes the session %s",
			whoamiValue(caller.Session), session, session, entry, session)
	case caller.Project == nil || *caller.Project != project:
		return fmt.Errorf("GET /api/v1/whoami answers the project %s for the daemon's broker token, not %s, and Dispatch refuses every write of the daemon's to %s with it (403 PROJECT_SCOPE): %s so that it names the project %s",
			whoamiValue(caller.Project), project, project, entry, project)
	case caller.Owner == nil:
		return fmt.Errorf("GET /api/v1/whoami answers the owner null for the daemon's broker token, and Dispatch refuses every write of a token that acts for no person (SIGN_IN_REQUIRED): %s so that it names the person the daemon acts for",
			entry)
	}
	return nil
}

// whoamiValue is a field of GET /api/v1/whoami's answer as a refusal quotes it: quoted, or null.
func whoamiValue(value *string) string {
	if value == nil {
		return "null"
	}
	return strconv.Quote(*value)
}

// plan is what the daemon resolved from its configuration before touching anything.
type plan struct {
	// identity is the role's App bot identity for the owner whose installation holds the
	// repository it works, from the workflow's token source (workflowRuntime.identity, which maps
	// the record's repository to the one GitHub serves); nil without one.
	identity func(ctx context.Context, role claim.Role, repository ghrepo.Repository) (runtime.GitIdentity, error)
	// knownIdentity is the bot identity of a role's App that the workflow's token source already
	// holds, read with no mint (workflowRuntime.knownIdentity); nil without a workflow.
	knownIdentity func(role claim.Role) (runtime.GitIdentity, bool)
	// repository is the repository an issue works and its default branch
	// (workflowRuntime.issueRepository: its record's, or, for an issue the workflow does not
	// record, the one Dispatch resolves it to, read at the first launch and kept while the issue
	// holds its workspace), which a working copy's adoption and a tmux pane's credential refresh
	// read; launchRepository is the same as every launch reads it (workflowRuntime.launchRepository,
	// which prepares a recorded issue's repository the cache set does not hold). Both nil without
	// a workflow.
	repository       func(ctx context.Context, issue string) (ghrepo.Repository, string, error)
	launchRepository func(ctx context.Context, issue string) (ghrepo.Repository, string, error)
	// ensureWorkspace provisions an issue's workspace on the daemon's host when a launch finds it
	// missing or not checked out (workflowRuntime.ensureWorkspace); nil without a workflow.
	ensureWorkspace func(ctx context.Context, issue string, repo ghrepo.Repository, defaultBranch string) error
	// owner is the architect that owns a claim on an issue that is not its tree's root, read from
	// the claims the store records at each launch (workflowRuntime.owningArchitect), which the
	// launch's addressing names; nil without a workflow.
	owner func(ctx context.Context, project string, c supervise.Claim) (claim.Token, error)
	// repositories is the repository state the workflow fills, empty until its preparations read
	// GitHub: the rename rule every gh bundle resolves a configured pair against
	// (repositories.servesRepository, appauth.GitHubCredential) and, under the Kubernetes runtime,
	// the rule a provisioning mint reads (sandboxRuntime, repositories.serving).
	repositories *repositories
	// installed is the record of the owner accounts each App was last read installed on, made here
	// rather than inside a runtime factory because more than one reader shares it: the boot's proof
	// of each App's key writes its own read first (proveAppKeys, appauth.Installed.RecordProof),
	// the credentials the factory builds write theirs at every bundle (appauth.GitHubCredential,
	// with the LeftOut record beside it), every launch's addressing reads its own App's owners out
	// of it (appOwners, daemon.RepositoriesClause), the boot's read until a bundle of the App is
	// built, and the drift check reads the bundles' reads alone (appauth.GitHubDirs). Never nil: a
	// daemon with no GitHub Apps proves no key and builds no bundle, so nothing is ever recorded
	// in it and its addressing names no owner.
	installed *appauth.Installed
	// left is the record of the gh bundle directories a mint could not fill (appauth.LeftOut),
	// made here beside installed, and not inside a runtime factory, for the same reason: more
	// than one reader shares it. The credentials the factory builds write it, and what a bundle
	// of an App really holds is the installed owners less what this record names
	// (appauth.GitHubDirs): the Kubernetes runtime's drift check reads that, and a launch's
	// addressing reads the same rule here (appOwners, appauth.AddressingDirs), so that it names no
	// owner the role's bundle holds no directory for. A record each factory made for itself
	// would be one the plan could not reach. The configured renames that rule also reads are the
	// repository state's own (repositories.renames, which is cfg.RepositoryRenames). Never nil,
	// as installed is not.
	left *appauth.LeftOut
	// phaseHolds, rootHears, rootTask, designNews and treeClosable are the workflow's answers to
	// the supervisor's predicates; nil without a workflow, where every delivery is sent as it was
	// queued and every tree closes.
	phaseHolds   func(ctx context.Context, issue string, p phase.Phase, generation uint64) (bool, error)
	rootHears    func(ctx context.Context, c supervise.Claim, d supervise.Delivery) (bool, error)
	rootTask     func(ctx context.Context, c supervise.Claim, d supervise.Delivery) (string, []byte, []byte, error)
	designNews   func(ctx context.Context, c supervise.Claim, d supervise.Delivery) (string, []byte, error)
	treeClosable func(ctx context.Context, c supervise.Claim) (bool, error)
	// phaseRole is the workflow's answer to which role works an issue's current phase, which the
	// pod start gate puts first among the starts no request raised (supervise.Deps.PhaseRole); nil
	// without a workflow, where no role is the phase worker.
	phaseRole func(ctx context.Context, issue string) (claim.Role, error)
	// cacheRepositories is the repositories the runtime keeps a repository cache of: those of the
	// project's live issue records and of the launches since their last read
	// (workflowRuntime.cacheRepositories). A daemon with no workflow keeps none.
	cacheRepositories func() []ghrepo.Repository
	// memoryGuardKills is the memory guards' kill holder (memoryGuardKills); nil under tmux, and
	// for a replaced runtime, where no launcher reports.
	memoryGuardKills *memoryGuardKills
	// volumeReports is how the Kubernetes runtime's launcher sessions reach the fact path with
	// one issue pod's volume sweep (volumeTrimReports, prepareSandbox): a pointer, so the
	// factory built before the workflow opens and the boot that fills it once it has hold the
	// one holder. Nil under tmux, and for a replaced runtime, where no launcher reports.
	volumeReports *volumeTrimReports
	// launchRounds is how the Kubernetes runtime's launches read the update round over a pod
	// they would keep (launchRounds, prepareSandbox, sandbox.Options.RoundPodEnds): a pointer, so
	// the runtime built inside openSupervision and the boot that fills it once the supervision is
	// open hold the one holder. Nil under tmux, whose panes run in no pod, and for a replaced
	// runtime, which is handed no such reading.
	launchRounds *launchRounds
	// taskConfirmed tells the workflow each agent that took its task; nil without a workflow.
	taskConfirmed func(c supervise.Claim)
	// sessionLost and sessionStoreFault tell the workflow that a claim forgot its recorded session
	// and why, and that a start could not reach the session store at all; nil without a
	// workflow, where neither is reported to anyone.
	sessionLost       func(c supervise.Claim, cause supervise.SessionLoss, detail string)
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
	// privateNameRules is the deployment's private-name rule file as boot compiled it
	// (bootReads.privateNameRules), which the outbox's branch create runs (ghbranch.Create); nil
	// when the deployment names none, and the create then runs no private-name rule.
	privateNameRules *leakguard.RuleSet
	// nats is the user the daemon's own NATS connection authenticates as (natsConnection); no launch
	// carries its seed unless it is the pane seed.
	nats natsConnection
	// instructions is the path of the daemon's copy of the deployment's instructions, "" when the
	// configuration names none, and instructionsCopies every copy of an instructions file, which
	// each launch reads again first (writeInstructionsCopies).
	instructions       string
	instructionsCopies *instructionsCopies
	// dispatchToken is the Dispatch bearer dispatch_token_file names; "" without Dispatch, and
	// under dispatch_token_broker, whose credential is the daemon's own (dispatchCredential).
	dispatchToken string
	// hookPrompts are the words boot read for each hook whose `prompt_file` names a file, by
	// hook name (readBoot): the engine is given words and never a path (engineConfig).
	hookPrompts map[string]string
	prompts     *prompts.Composer
	// roleReferences are the task agents and skills the shared role prompts name
	// (prompts.RoleReferences), which the gate on either runtime resolves beside the plugin's own.
	roleReferences promptrefs.Names
	// stream is the worker stream's address: the listener binds it, and every agent's shim dials
	// it, or advertise_host at its port when the file sets one (shimAddress).
	stream     string
	newRuntime runtimeFactory
	// gate is the plugin gate run before anything is opened (pluginGate); nil under a runtime with
	// no host Oh My Pi, and for a replaced runtime without one.
	gate func(ctx context.Context) error
	// probe proves the runtime's worker image once the runtime is built and before the boot is
	// recorded, answering what its OK line reported of the image; nil under tmux, and for a
	// replaced runtime without one.
	probe func(ctx context.Context, rt runtime.Runtime) (bootprobe.ImageReport, error)
	// clusterCheck is the Kubernetes runtime's refusals before the store opens: Agent Sandbox's
	// install check, then the census of Sandboxes of a layout before this one — per-claim pods, or
	// issue pods on one tree volume (sandbox.CensusLegacyIssueSandboxes). Nil under tmux, and for a
	// replaced runtime.
	clusterCheck func(ctx context.Context) error
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
	// secretsRegistrar is the same login as the machines' Registrar (newRegistrar); nil when the
	// deployment names no exchange_audience, and then no pod session is ever registered.
	secretsRegistrar supervise.Registrar
	// pluginRelaunchable is supervise.Deps.PluginRelaunchable: whether this daemon can relaunch a
	// stale-plugin claim onto a plugin at all. True under tmux always; under Kubernetes, true iff
	// a plugin pair to install is known — plugins below, resolved by ResolvedPlugins from the
	// file's runtime.kubernetes.plugins or the release's linked pair.
	pluginRelaunchable bool
	// plugins is this daemon's resolved Kubernetes plugin pair (ResolvedPlugins), with Contract
	// already api.DaemonAPIVersion: what specs.SpawnSpec installs for a stale-plugin relaunch, and
	// what the boot log and `legion config check` name. nil under tmux, and under
	// Kubernetes when the daemon knows no pair (LEGION-164's degraded mode); pluginsSource is
	// "legion.yaml" or "binary" with a pair, "" without one.
	plugins       *runtime.PluginInstall
	pluginsSource string
	// secretsLogin is the same login's client, read-only, for the state route to show its
	// current status (source.State, cipher.Client.LoginStatus); nil when the deployment
	// enrolls no pod.
	secretsLogin *cipher.Client
	// strandedReported is overrides.strandedReported; nil runs nothing.
	strandedReported func()
	// beforeServingLatched is overrides.beforeServingLatched; nil runs nothing.
	beforeServingLatched func(ctx context.Context)
	// checkLaunch is overrides.checkLaunch; nil wraps the store with nothing, so every launch
	// reaches the real store's own CheckLaunch.
	checkLaunch func(context.Context, supervise.Claim) error
	// streamWaitBegan is overrides.streamWaitBegan; nil runs nothing.
	streamWaitBegan func(claim.Token)
	// beforeDecision is overrides.beforeDecision; nil runs nothing.
	beforeDecision func(claim.Token, supervise.Event)
	// streamListen is overrides.streamListen; nil binds the worker stream's address.
	streamListen func(network, address string) (net.Listener, error)
}

// appOwners is, for a daemon of runtimeName, the owner accounts the addressing of every launch
// of a role names as the ones its own GitHub App is installed on and its token reaches
// (specs.appOwners, daemon.RepositoriesClause): the owners that App was last read installed on
// (appauth.Installed: the last gh bundle of it, or, while none has been built, the boot's proof
// of its key), which costs the launch no read of its own, less every one the role's container
// holds no directory for.
//
// An installed owner is not always one the role's container holds a directory for. A mint GitHub
// answers definitively about the owner's installation — not installed there after all,
// suspended, granted none of the role's permissions, the permissions refused — leaves that
// owner's directory out of the bundle (appauth's mintBundle, recorded in left), and so does a
// login that is no directory's name (appauth's bundleDirs). Under the Kubernetes runtime there
// is a third way: an issue pod projects only the directories every one of its roles' bundles
// holds (sandbox's gitHubBundles), and its roles run as two Apps, so an owner the other App is
// not installed on, or whose directory the other App's mint left out, is in no container of
// the pod, the implementer's included (sandbox.PodAppRoles). A tmux pane holds its own role's
// bundle and no other, so there the role's own App is the whole answer. The clause says the
// role's token reaches every repository of each owner it names, which is false of any such
// owner, so it is not named. The rule is asked of the one place that holds it and not written
// a second time: appauth.AddressingDirs is the directories a bundle of those Apps built now would
// hold — GitHubDirs' set, which is what the Kubernetes drift check reads a pod against, but read
// from the boot's proof of an App's key while no bundle of it has been built — and the owners
// kept are the installed ones it names. It names one more directory for each owner a configured
// rename moved a repository away from, which is no installed owner and so is never one of these.
//
// The boot's proof of each App's key records its owners before any launch (proveAppKeys), so
// every launch of a daemon with a workflow has an answer to name; a daemon with no GitHub Apps has
// none, and its addressing names no owner. GitHubDirs itself names nothing until a bundle of
// those Apps is built, since no pod may be compared against the boot's read, so it is not what
// this asks.
func (p plan) appOwners(runtimeName string) func(claim.Role) []string {
	return func(role claim.Role) []string {
		app := appauth.AppRoleFor(role)
		apps := []appauth.AppRole{app}
		if runtimeName == "kubernetes" {
			apps = sandbox.PodAppRoles(role)
		}
		owners, read := p.installed.Owners(app)
		for _, other := range apps {
			if _, otherRead := p.installed.Owners(other); !otherRead {
				read = false
			}
		}
		if !read {
			return nil
		}
		held := appauth.AddressingDirs(p.repositories.servesRepository, p.repositories.renames, p.installed, p.left, apps, ghrepo.Repository{})
		return slices.DeleteFunc(owners, func(owner string) bool { return !slices.Contains(held, owner) })
	}
}

// runtimeFactory builds the runtime over the worker stream (C3): ctx is supervision's lifetime,
// listener the stream listener, address the one every agent's shim dials (shimAddress), tokens the
// workflow's App tokens and repository the workflow's repository of an issue (issueRepository;
// both nil without a workflow), st the store the runtime reads, and cached the repositories the
// Sandbox runtime keeps a repository cache of: the repositories of the project's live issue
// records and of the launches since their last read (workflowRuntime.cacheRepositories; none
// without a workflow). The rename rule every role's gh bundle resolves a configured pair by, one
// repository at a time (appauth.GitHubCredential, repositories.servesRepository), is the repository
// state's own and is given to the factory where it is built. openSupervision hands it the plan the
// workflow completed, so no factory reads a field the workflow sets after prepare.
type runtimeFactory func(ctx context.Context, listener *stream.Listener, address string, tokens appauth.Source,
	repository func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, cached func() []ghrepo.Repository) (runtime.Runtime, error)

// prepare is every refusal that needs nothing but the configuration and the machine (readBoot's,
// then what writes or runs something: the state directory, the instructions copy, and what the
// runtime needs) — all before the plugin gate runs and before any agent can launch. It logs the
// boot's one line on the deployment's private-name rules: privateNameRulesNone before readBoot,
// the first thing that can stop the boot, where the deployment names no rule file; and the file,
// its sha256 and its number of rules once readBoot has compiled it where it names one, since a
// file readBoot cannot use stops the boot with its own message instead.
func prepare(cfg config.Config, log *slog.Logger, o overrides) (plan, error) {
	getenv := o.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if cfg.PrivateNameRulesFile == "" {
		log.Info(privateNameRulesNone)
	}
	// A test that replaced the runtime runs no host Oh My Pi, so boot resolves no OMP invocation.
	hostOMP := o.runtime == nil
	reads, err := readBoot(cfg, environLookup(o.environment()), getenv, hostOMP, log)
	if err != nil {
		return plan{}, err
	}
	if reads.privateNameRules != nil {
		logPrivateNameRules(log, cfg.PrivateNameRulesFile, reads.privateNameRules)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return plan{}, fmt.Errorf("create state directory %s: %w", cfg.StateDir, err)
	}
	composer, err := prompts.New(cfg.StateDir)
	if err != nil {
		return plan{}, fmt.Errorf("construct role prompts: %w", err)
	}

	copies, instructions, err := writeInstructionsCopies(cfg, reads, log)
	if err != nil {
		return plan{}, err
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
	secretsEnroller, secretsRegistrar, secretsLogin := newSecretsLogin(cfg, log)
	plugins, pluginsSource := ResolvedPlugins(cfg)
	pluginRelaunchable := cfg.Runtime.Name == "tmux" || plugins != nil
	if o.pluginRelaunchable != nil {
		pluginRelaunchable = *o.pluginRelaunchable
	}
	p := plan{
		repositories: newRepositories(cfg.Project, cfg.RepositoryRenames), installed: appauth.NewInstalled(), left: appauth.NewLeftOut(),
		project: reads.project, operatorToken: reads.operatorToken, secrets: reads.secrets, privateNameRules: reads.privateNameRules, nats: reads.nats, instructions: instructions, instructionsCopies: copies,
		dispatchToken: reads.dispatchToken, hookPrompts: reads.hookPrompts, prompts: composer, roleReferences: prompts.RoleReferences(),
		tools: reads.tmux.tools, clock: clock, orphanSweep: orphanSweep, controllerRetry: controllerRetry, stopBudget: stop,
		secretsEnroller: secretsEnroller, secretsRegistrar: secretsRegistrar, secretsLogin: secretsLogin, strandedReported: o.strandedReported,
		beforeServingLatched: o.beforeServingLatched,
		pluginRelaunchable:   pluginRelaunchable, plugins: plugins, pluginsSource: pluginsSource, checkLaunch: o.checkLaunch,
		streamWaitBegan: o.streamWaitBegan, beforeDecision: o.beforeDecision, streamListen: o.streamListen,
	}
	if cfg.Runtime.Name == "kubernetes" {
		err = prepareSandbox(cfg, o, reads.sandbox, &p)
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
	p.newRuntime, p.gate = o.runtime, o.gate
	p.stream = "unix://" + filepath.Join(cfg.StateDir, streamSocket)
	launchers := map[runtime.Backend]tmux.Launcher{}
	for _, backend := range cfg.Backends() {
		read, ok := reads.launches[backend]
		if !ok {
			continue
		}
		launchers[backend] = read.launcher
		if p.newRuntime == nil {
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
	if p.newRuntime == nil {
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
			contract:       api.DaemonAPIVersion(),
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
		p.newRuntime = tmuxRuntime(cfg, p.project, launchers, providerEnvDir, dispatchTokenFile, p.tools["gh"], p.repositories.servesRepository, p.installed, p.left, log)
	}
	return nil
}

// checkPaneRoleOverlays is the boot gate's read of the overlays each role names beyond the
// operator's own, as that role's pane will start Oh My Pi: its PI_CONFIG_FILES,
// runtime.tmux.pi_config_files then the role's own files (role_config_files) and, for a workflow
// role of a project that declares an arm naming overlays, that arm's files after them
// (runtime.RoleOverlayValues, which the image probe's manifest builds its values with too), each
// file read and each read alone by image's Oh My Pi (capabilities.CheckOverlays), so a file a
// pane's Oh My Pi would refuse refuses the boot, naming the key and the file, rather than every
// launch that names it. Legion names no overlay of its own, so those lists are the whole of a
// pane's PI_CONFIG_FILES.
//
// Each distinct file is read alone once and each distinct whole value is read once, over every
// role and every arm (capabilities.OverlayReads, which the image probe's own read holds too).
// The refusal still names the first role and the first value that holds what Oh My Pi refuses,
// since the loops reach them before any other.
//
// No mode is read here, since a unit already drawn into an arm names that arm's overlays
// whatever the mode is now. A run that never reached an exit of its own is logged, not refused,
// as the model-fallback read is.
func checkPaneRoleOverlays(ctx context.Context, cfg config.Config, image capabilities.Image, log *slog.Logger) error {
	var every []string
	if block := cfg.Runtime.Tmux; block != nil {
		every = block.PiConfigFiles
	}
	declared := experiment.DeclaredVariants(cfg.ExperimentsDeclared())
	reads := capabilities.OverlayReads{}
	for _, role := range config.RoleConfigRoles {
		own := cfg.RoleConfigFiles[role]
		ownField := "role_config_files." + string(role)
		for _, value := range runtime.RoleOverlayValues(role, own, declared) {
			unread, first := reads.Unread(value.Files)
			if !first {
				continue
			}
			field := ownField
			if value.Variant != nil {
				field = value.Variant.ConfigFilesField
				if len(own) > 0 {
					field = ownField + " with " + field
				}
			}
			why, err := capabilities.CheckOverlays(ctx, image, slices.Concat(every, value.Files), unread)
			if err != nil {
				log.Warn("boot gate: Oh My Pi did not answer on some of a role's overlays; its files were read", "role", role, "error", err)
			}
			if why != "" {
				return fmt.Errorf("boot gate: %s: the %s's Oh My Pi would not start: %s", field, role, why)
			}
		}
	}
	return nil
}

// launchVariants is the one reader a launch's arms come from: the experiments the project
// declares, resolved against the answers the daemon's own store recorded for the launch's unit
// and against the launch's role (experiment.Launch, which decides which keys to read and which
// arms the rows put the launch in). Everything here is the transaction that reads those rows.
//
// It reads the daemon's own store rather than the workflow's, in a transaction of its own as the
// supervisor's predicates read a record: the operator's spawn draws a tree's experiments through
// the same store with no workflow configured (api's drawExperiments), and a launch that could not
// read that draw back would run the unit against an arm the daemon had already recorded for it.
// A declaration no launch could use names no key to read, so such a deployment opens no
// transaction per worker spawn at all (experiment.Launch.Keys).
func launchVariants(st *store.Store, declared []config.Experiment, stateDir string) func(ctx context.Context, tree, issue string, role claim.Role) (experiment.Arms, error) {
	launch := experiment.ForLaunch(declared, stateDir)
	records := record.NewStore()
	return func(ctx context.Context, tree, issue string, role claim.Role) (experiment.Arms, error) {
		keys := launch.Keys(tree, issue)
		if len(keys) == 0 {
			return experiment.Arms{}, nil
		}
		var recorded map[string][]record.Assignment
		if err := store.TransactWith(ctx, st.Pool(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			recorded, err = records.AssignmentsFor(ctx, tx, keys)
			return err
		}); err != nil {
			return experiment.Arms{}, err
		}
		return launch.Arms(recorded, tree, issue, role), nil
	}
}

// tmuxRuntime builds the tmux runtime over the worker stream: the listener is its connection
// directory, and the listener's address is the `--connect` every pane's shim is started with;
// providerEnvDir, when set, is the `--provider-env-dir` beside it. The workflow's App tokens, when
// the daemon has them, are every tree pane's gh bundle (appauth.GitHubCredential) and the
// controller pane's own (appauth.ControllerCredential), which its refresh asks by the default
// owner its launch recorded, both built over the owner accounts each App is installed on, which
// the bundle reads at every build (appauth.Manager.InstalledOwners: from GitHub each time under
// the App's JWT; in token mode the App's token_command's release of every installation, which may
// be that of a run that finished up to appauth's finishedRunKeep before) and no repository list
// bounds, and over served, the rename rule's answer of whether a repository is served
// (repositories.servesRepository), with gh, the real gh boot resolved
// (daemonTools), the one each pane's `legion gh` runs, and the repository each claim's issue
// works (claimRepository over the workflow's repository), which a pane adopted after a restart
// holds its write token for; a daemon with no GitHub Apps hands the runtime none, and its panes
// hold no gh files. The private server's environment is scrubbed before anything is launched on
// it.
func tmuxRuntime(cfg config.Config, project string, launchers map[runtime.Backend]tmux.Launcher, providerEnvDir, dispatchTokenFile, gh string, served func(ghrepo.Repository) bool, installed *appauth.Installed, left *appauth.LeftOut, log *slog.Logger) runtimeFactory {
	return func(ctx context.Context, listener *stream.Listener, streamAddress string, tokens appauth.Source,
		repository func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
		opts := tmuxOptions(cfg, project, launchers, providerEnvDir, dispatchTokenFile, log)
		opts.StreamAddress, opts.Conns = streamAddress, listener
		if tokens != nil {
			// The two records the credentials write, shared by both as the Kubernetes runtime's
			// three share them (appauth.Installed and appauth.LeftOut, kubernetes.go's
			// sandboxRuntime). Both are the daemon's own (plan.installed, plan.left), since
			// every launch's addressing reads the owners its App reaches out of them. A pane
			// projects nothing and nothing compares one against left — a directory left out
			// of one pane's bundle is left out of the next pane's by the mint, not by a
			// record — so the addressing is this runtime's one reader of it.
			opts.GitHubCredential, opts.GH = appauth.GitHubCredential(tokens, served, cfg.RepositoryRenames, installed, left, log), gh
			opts.ControllerCredential = appauth.ControllerCredential(tokens, served, cfg.RepositoryRenames, installed, left, log)
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
// (repository, the workflow's issueRepository: its record's, or, for an issue the workflow does
// not record, the one kept for it at its first launch). It is what a tmux pane adopted after a
// restart holds its write token for (tmux.Options.ClaimRepository), the repository its launch was
// minted for, read from the daemon's own state and nothing a pane can write.
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
// (role_config_files), every declared experiment's arms (experiment.DeclaredVariants), and the
// checker of the deployment's shared leak check (leak_check), which every pane is told.
func tmuxOptions(cfg config.Config, project string, launchers map[runtime.Backend]tmux.Launcher, providerEnvDir, dispatchTokenFile string, log *slog.Logger) tmux.Options {
	opts := tmux.Options{
		Project:                 project,
		StateDir:                cfg.StateDir,
		DaemonURL:               cfg.DaemonURL,
		EnvoyURL:                cfg.EnvoyURL,
		NatsURLs:                cfg.NatsURLs,
		DispatchURL:             cfg.DispatchURL,
		DispatchTokenFile:       dispatchTokenFile,
		Launchers:               launchers,
		StopGrace:               cfg.WorkerStopTimeout,
		ProbeInterval:           cfg.ProbeInterval,
		AdoptTimeout:            cfg.SlowCommandTimeout,
		ProviderEnvDir:          providerEnvDir,
		RoleConfigFiles:         cfg.RoleConfigFiles,
		LeakCheckManifestSHA256: cfg.LeakCheck.ManifestSHA256,
		Log:                     log,
	}
	if block := cfg.Runtime.Tmux; block != nil {
		opts.PaneEnv, opts.PiConfigFiles, opts.TakeOver = block.PaneEnv, block.PiConfigFiles, block.TakeOver
	}
	opts.ExperimentVariants = experiment.DeclaredVariants(cfg.ExperimentsDeclared())
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
	// afterRefreshAdmit is a test's seam between Admit and supervisor.post in
	// refreshStalePlugins, run for each admitted claim with no lock held; nil runs nothing. A
	// test sets it on the supervision it builds by hand, and no production path sets it.
	afterRefreshAdmit func(claim.Token, *supervise.RefreshCredit)
	// pauses is the project's pause (projectPause), nil without a workflow: the boot loads its
	// flag before it restores any claim, and asks the stop of every claim it finds live while the
	// project is paused (start).
	pauses *projectPause
	// imageReport is what the worker image's passed probe reported: of the image, from its OK line,
	// and of the pool, the attempts that waited on its capacity (bootprobe.ImageReport); under
	// tmux, the model-fallback mark alone, read by plan.modelFallback. probed is whether the probe
	// passed (kubernetes), so the image rows of the capability report read present.
	imageReport bootprobe.ImageReport
	probed      bool
	// cacheStatuses is the runtime's repository caches, each with its last health read, which the
	// tick's capability report reads their room from (caches) and the state route reads once a
	// request (source.caches): repositoryCachesOf(runtime), nil under any runtime but the Sandbox
	// runtime, which alone keeps caches. A field, so that a test gives the report caches of its own
	// without a cluster.
	cacheStatuses func() []sandbox.CacheStatus
	// reportMu guards reportedGaps and reports: the tick's reads, the API's readies and boot's run
	// on different goroutines. reportedGaps are the open capabilities the last report logged
	// (reportCapabilities). reports is each claim's latest capability report (LEGION-663), as its
	// session's ready carried it (capabilityReported) or the store held it at boot (openSupervision),
	// whatever process reported: deployment fences each against its claim's current process.
	reportMu     sync.Mutex
	reportedGaps []string
	reports      map[claim.Token]capabilities.Report
	// podUpdateMu guards podUpdateOpenedAt and podUpdateOpened, what the update-round sweep keeps
	// between ticks to pace its openings (reconcilePodUpdates): when this daemon last opened a pod
	// round, and the rounds it has opened whose claims have not recorded one yet, by issue. The two
	// pace the sweep's openings under `pod_update_rounds_per_minute` and
	// `pod_update_max_open_rounds`; nothing of either is in the store, because neither decides a
	// round, and a daemon that has just booted opens its first round at its first sweep. The sweep
	// is one goroutine, and the mutex is what makes that a fact of this code rather than of its
	// caller.
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
	// podUpdateWorkspaceMu guards podUpdateWorkspace, what the update-round sweep keeps between
	// ticks of a round's read of its issue's workspace: the one read that a round made before its
	// pod's replacement (readRoundWorkspace), by issue, naming the round it was made for and
	// holding the answer or the read's error. A replacement the read stood in front of can be
	// refused under the pod's launch turn or can fail, and the next sweep that reaches the same
	// round's replacement reuses the answer rather than read again, so the round reads the
	// workspace once and its line and message name that read. The entry goes where the sweep ends
	// the round (endPodUpdates) and where the sweep finds the issue in another round or in none
	// (forgetWorkspaceReads). Nothing of it is in the store, because it decides nothing: the
	// replacement is made whatever the read found. A restart forgets it, and the round's next
	// replacement reads the workspace again. A mutex of its own, because it has nothing to do with
	// the pace podUpdateMu guards; the sweep is its one goroutine all the same.
	podUpdateWorkspaceMu sync.Mutex
	podUpdateWorkspace   map[string]workspaceRead
	// podUpdateEndedMu guards podUpdateEnded, what the update-round sweep keeps between ticks of
	// the rounds it has ended: by issue, the round whose end the sweep last posted to the round's
	// members (endPodUpdates). The post is asynchronous (supervisor.post), and a member whose
	// machine is in a relaunch takes it only once that relaunch is done, so its claim records the
	// round until then. While a claim of the issue records that round, the sweep reads the claim
	// as the end leaves it and decides nothing of that round again (reconcileIssuePodUpdate), and
	// the round holds no open-round slot (beginPodUpdateSweep). The entry goes at the first sweep
	// where no claim of the issue records the round (endedRounds). Within one daemon a post to a
	// machine's queue is never lost (inbox), so the end reaches every member. Nothing of it is in
	// the store: a restart forgets it with the posts not taken yet, and the next boot makes the end
	// once more for a claim the store still holds in the round (dispatch://INFRA-3276 R5). A mutex
	// of its own, for the reason podUpdateWorkspaceMu has one.
	podUpdateEndedMu sync.Mutex
	podUpdateEnded   map[string]string
	// podUpdateLaunchMu guards podUpdateLaunchReplaced, what the launches into an issue's pod
	// decided of the update round open over it (roundPodEndsAtLaunch, launchRound): by issue, the
	// round, whether a launch replaced its pod as the round's own replacement and what that launch
	// read of the round's end, and each claim whose launch into the pod the hook kept the pod for.
	// The hook writes it inside a launching machine's decision and under the pod's launch turn; the
	// next launch into the pod reads the claims it kept the pod for (dispatch://INFRA-4109 R7), and
	// the sweep that reads a replaced pod gone, or another pod in its place, ends the round as the
	// replacement the round made (endRoundALaunchReplaced). The entry goes where the sweep ends the
	// round (endPodUpdates), where the hook writes one of another round of the issue, and at the
	// first sweep that finds no claim of the issue that has not retired (forgetLaunchRounds).
	// Nothing of it is in the store: a restart forgets it, and the sweep then reads a pod a launch
	// replaced as a pod gone, whose message names the members by the same rule (lostClaims), where
	// each machine built over its stored claim reads its set of pods as unknown wherever the stored
	// claim shows a session (supervise's bootRegisteredPods). A mutex of its own, never held across
	// a runtime call, because the hook runs on a launching machine's goroutine beside the sweep.
	podUpdateLaunchMu       sync.Mutex
	podUpdateLaunchReplaced map[string]*launchRound
	// reportStore persists each report as it arrives (store.Store), so a restart renders the
	// sessions it re-adopts without waiting for them to report again.
	reportStore capabilityReportStore
	// enqueue writes outbox rows of an issue, all of them in one transaction, the workflow's own
	// enqueue (workflowRuntime.enqueueOutbox); nil in a deployment with no workflow, which has no
	// outbox to write to and no Dispatch issue to write on. It is the whole of the record the
	// update-round sweep and the stall backstop need (reconcilePodUpdates, tellStallBackstopped):
	// the notices they publish, the asks and the message written on the issue are all outbox
	// rows, and threading the workflow engine into supervision to post them would tie every
	// claim decision to the workflow. Rows that must land together go in one call, since a stall
	// told the controller but not the issue would be told again at every tick
	// (tellUpdateStalled). It answers whether it wrote them: it writes none of a claim of another
	// project's issue, whose rows only that project's daemon runs, and each caller says so rather
	// than report a row it did not write. The workflow sets it where it attaches, before
	// supervision restores or launches any claim, as it sets the supervisor's terminal callback
	// there.
	enqueue func(ctx context.Context, issue string, payloads ...record.OutboxPayload) (bool, error)
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
	// sessionStops counts the clean stops of session registrations in flight, which stop waits for
	// last (sessionStopsInFlight says which).
	sessionStops *sessionStopsInFlight
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
// neither is a wake the pacing holds. Each goes to the architect of its own issue, a root's or a
// child's, whose token it is composed for exactly as the line that wake writes composes it (the
// outbox's wakeRan), since the row names the issue and the kind alone. A project whose outbox
// holds no such row allocates nothing.
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
// reserveWake). interval is that limiter's Interval, and it must come from the same limiter: the
// gate reads how late a launch whose turn the boot reserved may run by it (supervise.WakePace). The
// rate and the burst go with them for the line every wake writes, never for that decision.
//
// refreshGate is the daemon's one stale-plugin refresh gate, threaded exactly as reserve is and
// from the same place (run builds both, from the one rate): it goes on the machines' Deps, so
// every machine of this daemon asks the same gate, and the supervision keeps the same reference,
// because the refresh pass corrects it from the wakes those machines publish and asks it for
// each claim it posts (refreshStalePlugins, waitingWakes).
//
// pauses is the project's pause (projectPause, nil without a workflow): the machines read its flag
// (Deps.Paused, the service's one atomic load), write a pause's stop through its stop transaction
// (Deps.PauseStopped) and stamp each paced launch's let-out moment through it (Deps.WakeLetOut);
// it reads the machines this supervisor holds for its catch-up and its resume.
func openSupervision(boot context.Context, cfg config.Config, log *slog.Logger, p plan, st *store.Store, apps appauth.Source,
	reserve func(time.Time) time.Time, interval time.Duration, refreshGate supervise.RefreshAdmitter, pauses *projectPause) (*supervision, error) {
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
	sup.beforeDecision = p.beforeDecision
	listener, err := stream.Listen(draining, p.stream,
		sup.helloResolver(tokens, cfg.WorkerRPCTimeout), stream.Options{RPCTimeout: cfg.WorkerRPCTimeout, Log: log, Listen: p.streamListen})
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
	cached := p.cacheRepositories
	if cached == nil {
		// A daemon with no workflow has no records to read, so it keeps no repository cache.
		cached = func() []ghrepo.Repository { return nil }
	}
	rt, err := p.newRuntime(supervising, listener, dial, apps, p.repository, st, cached)
	if err != nil {
		cancel()
		endDrain()
		return nil, fmt.Errorf("build the %s runtime: %w", cfg.Runtime.Name, err)
	}
	// The clean stops of session registrations the machines send run past their decisions and are
	// counted here (sessionStopsInFlight).
	var sessionStops sessionStopsInFlight
	sup.deps = supervise.Deps{
		Runtime: rt,
		Conns:   listener,
		Store:   pruning(gateLaunch(tokens.Recording(st), p.checkLaunch), runtime.SecretsDir(cfg.StateDir), log),
		Specs: specs{
			stateDir: cfg.StateDir, project: p.project, instructions: p.instructions, copies: p.instructionsCopies, secrets: p.secrets, prompts: p.prompts,
			identity: p.identity, knownIdentity: p.knownIdentity, repository: p.launchRepository, owner: p.owner, log: log,
			appOwners:   p.appOwners(cfg.Runtime.Name),
			designSteps: cfg.Gates.Steps(), productionCheck: cfg.Projects[cfg.Project].ProductionCheck, slackChannels: cfg.Projects[cfg.Project].SlackChannels,
			runtimeName: cfg.Runtime.Name, plugins: p.plugins,
			commitIssueTrailerRepos: cfg.Projects[cfg.Project].CommitIssueTrailerRepos, same: p.repositories.same,
			backend: cfg.BackendFor, hostWorkspaces: !rt.ProvisionsWorkspaces(), ensureWorkspace: p.ensureWorkspace,
			variants: launchVariants(st, cfg.ExperimentsDeclared(), cfg.StateDir),
			provider: cfg.Projects[cfg.Project].Provider,
		},
		Identity:                adoptIdentity(p.identity, p.repository),
		IssueInTurn:             sup.claimInTurn,
		Secrets:                 p.secretsEnroller,
		Registrar:               p.secretsRegistrar,
		PhaseHolds:              p.phaseHolds,
		PhaseRole:               p.phaseRole,
		RootHears:               p.rootHears,
		RootTask:                p.rootTask,
		DesignNews:              p.designNews,
		TreeClosable:            p.treeClosable,
		ClaimActivity:           st.ClaimActivity,
		TaskConfirmed:           p.taskConfirmed,
		SessionLost:             p.sessionLost,
		SessionStoreFault:       p.sessionStoreFault,
		ProviderErrorRunReached: p.providerErrorRunReached,
		SessionGone:             cleanSessionStop(&sessionStops, cfg.EnvoyURL, p.secrets["ENVOY_TOKEN"], log),
		RoleHolder:              roleHolderLookup(cfg.EnvoyURL, p.secrets["ENVOY_TOKEN"]),
		Clock:                   p.clock,
		Log:                     log,
		// A timer that fires and a prompt's send each apply their decision on a goroutine of
		// their own, beside the machine's feeding goroutine, so each takes a hold mark of its own.
		Worker: store.ForConcurrentWorker,
		// A claim write's bound counts from its connection: a boot relaunches every unfinished
		// claim at once through the one pool, and a bound that counted the queue for it cut the
		// last writes for the writes ahead of them (supervise.Deps.BoundWrite).
		BoundWrite: store.BoundAfterAcquire,
		Limits: supervise.Limits{
			LaunchFailures: cfg.LaunchFailureLimit,
			PromptFailures: cfg.PromptFailureLimit,
			PromptRetires:  cfg.PromptRetireLimit,
		},
		Timeouts: supervise.Timeouts{
			Boot:                  cfg.WorkerBootTimeout,
			RegistrationIntervals: cfg.WorkerBootRegistrationDeadlineIntervals,
			RPC:                   cfg.WorkerRPCTimeout,
			Probe:                 cfg.ProbeInterval,
			Stop:                  cfg.WorkerStopTimeout,
			LaunchBackoff:         launchBackoffFirst,
		},
		WakePace: supervise.WakePace{
			Reserve:  reserve,
			Interval: interval,
			Rate:     cfg.WorkerWakesPerMinute,
			Burst:    cfg.WorkerWakeBurst,
		},
		RefreshGate:        refreshGate,
		PodStarts:          podStarts(cfg, p.clock),
		PluginContract:     api.DaemonAPIVersion(),
		PluginRelaunchable: p.pluginRelaunchable,
		NoPluginInstall:    func(c supervise.Claim) bool { return noPluginInstall(cfg, c) },
	}
	if pauses != nil {
		sup.deps.Paused, sup.deps.PauseStopped, sup.deps.WakeLetOut = pauses.service.Paused, pauses.stopped, pauses.wakeLetOut
		pauses.supervisor = sup
	}
	stopping, beginStop := context.WithCancel(context.Background())
	supervisionOf := &supervision{
		cfg: cfg, log: log, plan: p, stream: listener, runtime: rt, supervisor: sup, tokens: tokens, claims: claims,
		reports: reportsOfClaims(reports, claims), reportStore: st, refreshGate: refreshGate, pauses: pauses,
		cancel: cancel, draining: draining, endDrain: endDrain, decided: api.NewRouteDecisions(),
		stopping: stopping, beginStop: beginStop, sessionStops: &sessionStops, cacheStatuses: repositoryCachesOf(rt),
	}
	// The machines' report of a stalled turn they ended goes to supervision, which is built
	// after their deps: the one callback whose receiver is supervision itself, set here before
	// any machine exists, as the workflow sets enqueue where it attaches.
	sup.deps.StallBackstopped = supervisionOf.stallBackstopped
	return supervisionOf, nil
}

// cleanSessionStop is what a machine calls once a claim's agent process has ended — a probe
// judged it gone, or a suspension's stop returned (supervise.Deps.SessionGone): it deletes that
// session's registration from the Envoy listener, so the listener reads the session `gone` at
// once instead of holding its issue claims for the whole live window (dispatch://INFRA-392), and
// a message to a released role raises the wake that brings it back rather than being stored for
// a session nothing reads (the role claim stays until the listener's reaper frees it,
// notify.HTTPPublisher.DeleteSession). It runs on the same client the daemon's
// notices go through (notify.New), beside controller.NewProber, on the same base URL and bearer.
//
// The machine calls it inside its own decision, under its lock, so the read and the delete run on
// a goroutine of their own, on a context of their own that only cleanSessionStopBudget ends (send):
// not the decision's, which the machine's caller can end the moment the decision returns, and not
// supervision's, which the halt cancels. inFlight is told when the stop starts and, if it counted
// it, when it ends (sessionStopsInFlight says which stops the daemon's stop waits for).
// goneAt, the moment the stop belongs to, is read here, inside the decision, where no lock is
// waited on: for a death before any relaunch (Machine.died calls sessionGone first), and for a
// suspension once the runtime's stop has returned, which both runtimes do only when the process
// has ended, so the process's last heartbeat lands before it.
//
// That delete must never take the registration of the process that replaced the ended one. For
// a death the very decision that asks for the stop goes on to relaunch the claim under the same
// session id (Machine.died); for a suspension a later wake resumes it under that id, which can
// register before the stop's read is answered. Either relaunch registers that id again. So the
// stop names the ended process's own registration — its client instance id, read from the
// listener first (send) — and carries goneAt beside it, and the listener deletes only under
// both. What each condition is for, and why neither alone would do, is on
// notify.DeleteConditions.
//
// The listener keeps the entry when either condition refuses it, so the id rescues no
// registration the moment refuses: a heartbeat whose write lands after the moment, or a listener
// clock ahead of this daemon's, is kept even where the id names the ended process, and that
// session waits for its record to expire.
//
// The read and the delete share one budget, cleanSessionStopBudget, so a listener that never
// answers does not leave the stop in flight for as long as it likes, and nothing else ends them:
// send says why the daemon's stop must not.
//
// A listener that refuses the delete, keeps the registration, or cannot be reached costs the
// session nothing beyond the wait it already had: the registration lapses on its own when its
// record expires (the session bucket's maximum age, 5 minutes by default). With no listener
// configured nothing is told.
func cleanSessionStop(inFlight *sessionStopsInFlight, envoyURL, token string, log *slog.Logger) func(string) {
	if envoyURL == "" {
		return nil
	}
	stop := sessionStop{sessions: notify.New(envoyURL, token), log: log}
	return func(session string) {
		goneAt := time.Now()
		counted := inFlight.start()
		go func() {
			if counted {
				defer inFlight.end()
			}
			stop.send(session, goneAt)
		}()
	}
}

// roleHolderLookup is the supervisor's read of a role's holder at the Envoy listener
// (supervise.Deps.RoleHolder, notify.HTTPPublisher.RoleHolder), which a forget of a session its
// claim does not hold asks before it clears anything (supervise's mayForget). With no listener
// configured it is nil, and nothing is asked.
func roleHolderLookup(envoyURL, token string) func(ctx context.Context, role string) (holder, state string, err error) {
	if envoyURL == "" {
		return nil
	}
	return notify.New(envoyURL, token).RoleHolder
}

// sessionStopsInFlight counts the clean stops of session registrations in flight
// (cleanSessionStop), which no decision waits for and the halt does not end. The daemon's stop
// waits for them last, in a final wait (supervision.stop), and that wait is for every clean stop
// started before it began: one sent before the halt, and one sent after it while the daemon's
// stop still waits for the decisions. Each runs to its own budget (cleanSessionStopBudget), so
// one sent late can still be in flight when the stop's budget runs out: run then stops waiting at
// that budget and logs `legion daemon stopped waiting for its work`, whose `deciding` names no
// claim when that stop is all that is left (awaitStop).
//
// A stop started once the final wait has begun is not counted and nothing waits for it: it runs,
// uncounted, to its own budget. A decision can still send one then: a timer the halt did not
// disarm fires its decision on a goroutine of its own, straight into the machine rather than
// through the supervisor the halt closed (Machine.arm), and a death that decision judges sends a
// stop (Machine.died). A sync.WaitGroup has no such cut-off: an Add that starts its count from
// zero while its Wait runs races that Wait, which the race detector reports, and an Add in the
// instant a Done brings the count to zero under a blocked Wait panics. The zero value counts
// nothing and has no wait begun.
type sessionStopsInFlight struct {
	mu    sync.Mutex
	count int
	// waiting is whether the final wait has begun, and drained, made when it begins, is closed
	// once it has and no counted stop is left.
	waiting bool
	drained chan struct{}
}

// start counts one stop, unless the final wait has begun, and reports whether it counted it. A
// stop it counted calls end when it ends.
func (s *sessionStopsInFlight) start() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waiting {
		return false
	}
	s.count++
	return true
}

// end is the end of a stop start counted.
func (s *sessionStopsInFlight) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count--
	if s.waiting && s.count == 0 {
		close(s.drained)
	}
}

// wait begins the final wait, after which start counts no stop, and blocks until every stop
// start counted has ended.
func (s *sessionStopsInFlight) wait() {
	s.mu.Lock()
	if !s.waiting {
		s.waiting = true
		s.drained = make(chan struct{})
		if s.count == 0 {
			close(s.drained)
		}
	}
	drained := s.drained
	s.mu.Unlock()
	<-drained
}

// sessionStop sends this daemon's clean stops, one call of send each; cleanSessionStop says when a
// machine asks for one and what the delete's two conditions keep. It holds what every stop
// shares: the listener client, the log that says what became of each stop, and the budget a
// stop's read and delete share, cleanSessionStopBudget when zero; only a test sets another.
type sessionStop struct {
	sessions *notify.HTTPPublisher
	log      *slog.Logger
	budget   time.Duration
}

// send is the work of the goroutine cleanSessionStop starts, with goneAt the moment the machine
// told it the process had ended: a probe's judgement of a death, or the return of a suspension's
// stop. It reads what the listener holds for the session and then deletes that registration by
// name: one read, one delete, one answer, and nothing is sent twice.
//
// A read the listener does not answer sends no delete at all. The moment is this daemon's own,
// but the instance id is not: a failed read leaves unknown whether the entry names a client
// process, and a delete sent then would rest on the moment alone for an entry that may well name
// one, which is the case where the id is the relaunch's guard against a daemon clock ahead of
// the listener's. An entry the listener answers with no instance id is known to name none, so
// the moment-only delete is the only delete there is, and it is sent. A registration left
// standing lapses when its record expires, the wait the session already had. An answer of no
// registration is the outcome the stop asked for, already reached.
//
// It reports the stop refused, kept or done, one line either way, since no decision reads its
// answer. The listener keeps a registration for three separate reasons — another client
// instance owns it, it was written after the moment, or the delete lost its compare-and-swap —
// so the line says only that it was kept and carries the listener's own words for which
// (SessionDeleted.Reason), beside the two conditions the stop named and the registration the
// read answered with. The registration is what the read saw, not what the delete compared, so
// where the entry changed between the two the line cannot say whose write refused it; where it
// did not, it tells a stop refused for a successor's registration from one refused for the ended
// process's own last heartbeat without asking the listener anything.
//
// Nothing but its budget ends it: not the decision that sent it, and not the daemon's stop, which
// waits for it or leaves it running (sessionStopsInFlight). The halt ends the machines' context,
// but a decision can return just before the halt with its stop still in flight — an idle release
// the workflow's outbox decided on serve's context, not the machines', or an operator's
// suspension a route decided — and a released claim is the one a message's wake acts on. Cut
// short there, its stop would leave the registration standing, and a message to the role would
// be stored for a session that reads nothing, with no wake raised, until the registration's
// record expires.
func (s sessionStop) send(session string, goneAt time.Time) {
	budget := s.budget
	if budget == 0 {
		budget = cleanSessionStopBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
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
		// A refused delete and an unreachable listener did not stop the session, and an answer
		// that could not be read to its end leaves whether it did unknown; the error says which.
		s.log.Warn("the Envoy listener did not confirm the clean stop of a dead role's session", "session", session, "error", err)
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
// stale-plugin relaunch (refreshStalePlugins) before any claim is relaunched, but for one whose
// record says its process died (recordedDeaths), which is relaunched for that death in the
// unfinished launches' own pass and order, on this daemon's contract; a claim a refused attempt
// leaves stale is retried at the orphan sweep's own cadence
// from then on (reconcileOrphans), not just at this boot pass. A daemon that leaves the controller
// to its operator first stops the controller's claim an earlier
// boot under `controller: daemon` left (stopLaunchedController), which leaves it absent or
// retired. The only controller claim among the unrecorded launches is a
// launch a crash cut short, and by then the stop has retired it, so launchUnfinished would not
// relaunch it: its ReleaseUncertainLaunch acts only on a launch-uncertain claim. Dropping that token
// is a second guard beside that check, so boot does not report the retired claim as an unrecorded
// launch. Before any of that, before the first machine exists, every recorded session name no row
// holds is held (holdRecordedSessions): restore builds each machine with its timers armed, and a
// timer decides on its own goroutine the moment it fires, so a restored claim's deadline could
// otherwise relaunch it into a name the hold had not yet given to no claim, and keep it (review
// round 9 of #4220). Then every claim recording a name it does not hold is suspended
// (suspendSessionConflicts); a restored timer that fires between restore and that suspension
// relaunches its claim into the hold's refusal, which holdRecorded refuses before anything starts
// and charges one launch failure. The steps before the relaunches run on boot, settle on budgets
// of its own, and the relaunches on the machines' context; the daemon's stop ends all three (run).
//
// The project's pause is read before any claim is restored (decision 18 of
// dispatch://INFRA-3759): the flag holds the open pause before any machine exists, so every launch
// this boot makes meets the gate. While the project is paused, the boot then asks the stop of each
// live claim but the controller's that the pause has not stopped (projectPause.askBootStops), and
// the relaunches and the stale-plugin pass below meet the gate, so the boot launches nothing but
// the controller (R5). A boot that cannot read the pause, or cannot ask those stops, does not
// start: it would launch roles a pause holds.
func (s *supervision) start(boot context.Context, settle func() error) error {
	var open store.Pause
	paused := false
	if s.pauses != nil {
		var err error
		if open, paused, err = s.pauses.service.Load(boot); err != nil {
			return fmt.Errorf("read whether the project is paused: %w", err)
		}
	}
	if err := s.holdRecordedSessions(boot); err != nil {
		return err
	}
	unfinished, err := s.supervisor.restore(boot, s.claims)
	if err != nil {
		return err
	}
	if paused {
		if err := s.pauses.askBootStops(boot, open); err != nil {
			return fmt.Errorf("ask the stops of pause %d: %w", open.ID, err)
		}
	}
	if s.cfg.ControllerLaunch != config.ControllerLaunchDaemon {
		if err := s.stopLaunchedController(boot); err != nil {
			return err
		}
		controller := claim.ControllerToken(s.plan.project)
		unfinished = slices.DeleteFunc(unfinished, func(token claim.Token) bool { return token == controller })
	}
	if err := s.suspendSessionConflicts(boot); err != nil {
		return err
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
		s.pump()
	}()
	go func() {
		defer s.wg.Done()
		// The sweep runs on a goroutine of its own beside the machines for supervision's life, so
		// it takes a hold mark of its own.
		s.reconcileOrphans(store.ForConcurrentWorker(s.supervisor.ctx))
	}()
	s.endPluginRounds(s.claims)
	// A claim whose record says its process died is relaunched for that death in the boot's pass
	// over the unfinished launches, in their order (relaunchUnfinished), and that launch is on this
	// daemon's contract; a stale-plugin relaunch posted to it first would raise its launch outside
	// that order and leave the turn the pass reserves it spent. The orphan sweep's own stale-plugin
	// posts pass over a claim whose relaunch waits (parkedWakes), so this is the one post to skip.
	deaths := recordedDeaths(s.claims)
	s.refreshStalePlugins(slices.DeleteFunc(slices.Clone(s.claims), func(c supervise.Claim) bool { return slices.Contains(deaths, c.Token) }), "boot")
	if len(deaths) > 0 {
		s.log.Info("supervise: the previous daemon found these claims' processes dead and their relaunches waiting; this boot raises each relaunch again",
			"claims", len(deaths))
	}
	if len(unfinished) > 0 || len(deaths) > 0 {
		s.relaunchUnfinished(unfinished, deaths, reconciled)
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
// end. That is what makes the order right: the refresh's own decision reads a claim already out
// of its round. The pass's skip is by round kind for the same reason (refreshStalePlugins): this
// pass walks the boot's own reading, where the round still stands, and the refresh pass reads each
// claim as its machine last published it, which still shows the round until the machine has
// decided this end, so either pass can find the round standing.
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
// what the pod round replaces the pod for. So it is left running and is asked its round like any
// other worker, on its role's own Envoy topic (supervision.openPodUpdate). staleRegistration
// accepts such a registration and charges it nothing, for the same reason
// (supervise.Deps.NoPluginInstall).
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
// before the post, never after the machine has decided, so a refresh in flight is counted: the
// gate counts every credit it has granted and not had back (supervise.RefreshGate, its rule of
// return). The pass gives back the credit of a post the supervisor drops, because no machine ever
// decides that post (supervisor.post). Before any of this the gate is corrected: it takes back
// each credit, granted before its previous correction, that the machine holding it no longer
// holds for a refresh wake of its own, which is the backstop for a credit lost to a path
// that gives none back, and then sets its count from the refresh wakes the machines really hold
// — the lane's own kind, the one kind only this lane parks (supervise.WakePluginRefresh,
// parkedWakes) — which raises the count when the lane holds wakes no admission is behind, and
// otherwise leaves it at the credits out. The boot pass corrects an empty gate, which is what a
// restart leaves: no machine of this daemon holds a wake yet, so the reading is zero and the
// fleet's refresh starts with every credit free (supervise.RefreshGate.Correct).
//
// A claim whose machine already holds a waiting wake is passed over before the gate is asked at
// all, read from the same snapshot the correction counts (parkedWakes). The machine decides such
// a claim nothing — refreshPlugin returns over a claim that already holds a wake of any kind, and
// gives back whatever credit the post carried — so asking for one here would spend an admission
// on a claim that raises no relaunch, and spend it for most of the pass, since the machine's
// release lands only when its own goroutine decides the post. At a fleet's worth of stale claims that is
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
// Every term of the lane is read of each claim as its machine last published it
// (supervise.Machine.Published), not as claims holds it: claims says which claims to walk. A
// sweep reads them before its orphan reconciliation, which under Kubernetes lists every Sandbox,
// so by the time this pass walks them a claim can have been suspended, failed or retired, had its
// relaunch start, started a turn, or registered this daemon's contract. A credit posted to such a
// claim comes back only as its machine decides the post, which a machine deciding something else
// first holds until that decision ends — a relaunch waits minutes for its pod — and until then the
// gate counts it out and this pass refuses every claim after it: the lane spends the credit on
// nothing for a sweep or more (dispatch://INFRA-3904). The published claim is the one the machine
// decides the post against unless a decision lands in between, so what is left of that window is
// one decision long rather than a reconciliation long.
//
// The skip of a claim in a turn is as imperfect as the parked skip above, and costs what that one
// costs. The machine decides the post as the claim stands when the post reaches it: a claim read
// idle can have started a turn by then, and one read working can have ended it. The first is
// posted a credit the machine gives straight back (refreshPlugin, endRefreshLane) and is
// relaunched at that turn's end or at a later sweep; the second is posted none, and its machine
// asks the gate for a credit of its own as it decides the post (refreshPlugin's guard), so it is
// relaunched then if a credit is free and at the next sweep otherwise.
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
	noInstall := func(c supervise.Claim) bool { return noPluginInstall(s.cfg, c) }
	parked, refreshes := s.parkedWakes()
	s.refreshGate.Correct(refreshes)
	refused := 0
	for _, c := range claims {
		if m, supervised := s.supervisor.Machine(c.Token); supervised {
			c = m.Published()
		}
		if !staleForTheLane(c, noInstall) {
			continue
		}
		if !s.plan.pluginRelaunchable {
			if trigger == "boot" {
				s.log.Error(supervise.NoPluginSourceMessage,
					"claim", c.Token, "incarnation", c.Locator.Incarnation, "pluginContract", c.PluginContract, "daemonApiVersion", api.DaemonAPIVersion())
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
		credit := s.refreshGate.Admit()
		if credit == nil {
			refused++
			continue
		}
		if s.afterRefreshAdmit != nil {
			s.afterRefreshAdmit(c.Token, credit)
		}
		if !s.supervisor.post(c.Token, supervise.StalePlugin{Claim: c.Token, Trigger: trigger, Credit: credit}) {
			credit.Release()
		}
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
// post and the machine sees a claim in no round at all. What that pass reads is each claim as its
// machine last published it, which still shows the round until the machine has decided that end,
// and the count reads the claims the store holds, where the round stands until that end's own
// write. So a term of "in any round" would pass over exactly the claims the de-round pass has just
// freed and leave each of them a sweep — a whole minute, and a credit of the gate each — behind
// the rest of the fleet, and would have the count hide that work at the one moment it exists to
// be read: a boot over a fleet an earlier daemon left mid-round. A claim whose plugin round is
// somehow still standing when its machine takes the post is decided nothing wrong by it either:
// refreshPlugin relaunches it, and no plugin round holds a relaunch back, since the ask that did
// was deleted with the round. A claim that left its round at a registration speaking this
// daemon's contract (supervise's endCurrentPluginRound) is no longer stale at all, and the
// contract term above passes over it.
//
// noPluginInstall nil is every launch installing a plugin, which only a source a test builds by
// hand holds: serve gives the state's source the rule over this deployment's configuration and
// the lane's own pass builds a closure of it at every sweep, so no daemon reads this term as
// nil, a tmux daemon included (noPluginInstall, serve, refreshStalePlugins).
func staleForTheLane(c supervise.Claim, noPluginInstall func(supervise.Claim) bool) bool {
	if c.Session == "" || c.PluginContract == api.DaemonAPIVersion() || !supervise.HoldsCapability(c.State) || c.InPodRound() {
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
func noPluginInstall(cfg config.Config, c supervise.Claim) bool {
	return cfg.Runtime.Name == "kubernetes" && runtime.Of(cfg.BackendFor(c.Role)) == runtime.BackendClaude
}

// podStarts is the gate the machines of this daemon start their roles' processes through
// (supervise.Deps.PodStarts, dispatch://INFRA-5297): one per daemon, on the machines' own clock,
// under the Kubernetes runtime alone, where every role of an issue runs in the issue's one pod
// under one memory limit. tmux runs each role in a pane of the host's, with no pod for two loads
// to meet in, so it gets none and no start waits.
func podStarts(cfg config.Config, clock supervise.Clock) *supervise.PodStarts {
	if cfg.Runtime.Name != "kubernetes" {
		return nil
	}
	return supervise.NewPodStarts(clock)
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
//     machines' own events, the controller keeper, the boot's relaunches — or on serve's context,
//     which ends at the same moment (the workflow's outbox), so it ends now rather than when its
//     runtime call's own wait runs out: a Sandbox relaunch waits minutes for the previous pod to
//     go, the tree's other pods to finish workspace-init and the new pod to appear. A decision an
//     API route asked for runs on until the drain ends. The clean stop of a session registration
//     a decision has already sent (cleanSessionStop) is not cancelled: it runs to its own budget,
//     and stop waits for the ones sessionStopsInFlight counts (sessionStop.send says why).
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
// the store once it closes. Last it waits for the clean stops of session registrations, which the
// halt does not end (sessionStopsInFlight says which it waits for and what becomes of the rest):
// an idle release decided on serve's context can return just before the halt with its stop in
// flight, and a released claim is the one a wake acts on (sessionStop.send). The panes and pods
// keep running; the next boot re-adopts them. The wait is unbounded here; run stops waiting for
// it at the stop's budget.
func (s *supervision) stop() {
	s.stopOnce.Do(func() {
		s.halt(nil)
		s.endDrain()
		s.supervisor.stop()
		s.supervisor.wait()
		s.wg.Wait()
		s.sessionStops.wait()
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
	// A daemon with no workflow reads the records through the project's scope as one with it does
	// (projectRecords), with no lane to wake.
	records := record.Store(projectRecords{Store: record.NewStore(), project: cfg.Project})
	var handlers []intake.Handler
	var client dispatch.Client
	var tokens appauth.Source
	var grants *credential.Grants
	var claimReady func(supervise.Claim)
	var handoffCompleted func(string)
	var roleHoldersChanged func(context.Context, supervise.Claim) error
	var prepare func(context.Context, intake.DispatchIssue) (intake.DispatchIssue, error)
	var unread func(string) bool
	var await func(repo, tree, child, parent string) bool
	var githubAPI string
	var consumerLag func(context.Context) []intake.ConsumerLag
	var architectStalls func(context.Context, pgx.Tx) (map[string][]record.ArchitectStallArm, error)
	// The pause service exists only with the workflow, which run builds it with (projectPause): a
	// daemon without one leaves pauses the nil interface, which the API reads as no pause service
	// and answers every pause and resume PAUSE_UNAVAILABLE.
	var pauses api.PauseService
	// The replay and the discard of the intake's give-ups, which a daemon with no workflow runs
	// no intake for: the API answers their routes 503 then. It is left a nil interface rather than
	// a nil *intake.GiveUps, which the API would read as an intake.
	var giveUps api.GiveUps
	if workflow != nil {
		records, handlers, client, tokens, grants = workflow.records, workflow.handlers, workflow.dispatch, workflow.apps, workflow.grants
		claimReady, prepare, unread, await = workflow.claimReady, workflow.prepareObserved, workflow.repositoryUnread, workflow.awaitRecovery
		handoffCompleted = workflow.handoffCompleted
		roleHoldersChanged = workflow.roleHoldersChanged
		githubAPI = workflow.githubAPI
		consumerLag = workflow.consumers.Lag
		pauses = workflow.pauses.service
		architectStalls = workflow.admission.ArchitectStallArms
		// A replay applies a stored message through the handlers a live delivery runs, on the
		// consumers' pool and spec, so it is judged as the same message delivered now would be.
		replays, err := intake.NewGiveUps(workflow.consumers.Records(), workflow.handlers...)
		if err != nil {
			return fmt.Errorf("build the replay of the intake's give-ups: %w", err)
		}
		giveUps = replays
	}
	// Under `controller: daemon` the keeper launches and keeps the project's controller, and takes
	// its ready. The liveness sweep runs beside it under either mode, never behind it.
	var keeper *controllerKeeper
	if cfg.ControllerLaunch == config.ControllerLaunchDaemon {
		keeper = newControllerKeeper(s.supervisor.ctx, s.supervisor, p.project, p.controllerRetry, s.log)
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
			admissionCap: cfg.AdmissionCap,
			startedAt:    startedAt,
			now:          p.clock.Now,
			secretsLogin: p.secretsLogin,
			deployment:   s.deploymentWith,
			reports:      s.capabilityReports,
			wakes:        s.waitingWakes,
			repositories: p.repositories,
			caches:       s.cacheStatuses,
			gatedSteps:   cfg.Gates.Steps(),
			experiments:  cfg.ExperimentsDeclared(),
			settings:     api.SettingsOf(cfg),
			// The same rule the lane itself reads, over the same configuration, so the count
			// the state serves and the claims the pass posts are one set (refreshStalePlugins,
			// and the NoPluginInstall option three lines below).
			noPluginInstall: func(c supervise.Claim) bool { return noPluginInstall(cfg, c) },
			architectStalls: architectStalls,
			// Under dispatch_token_broker alone: without it the state carries no
			// `dispatchCredential` (dispatchTokensOf).
			dispatchSession: dispatch.DaemonSession(cfg.Project),
			dispatchTokens:  dispatchTokensOf(cfg, s.runtime),
		},
		ConsumerLag:        consumerLag,
		Intake:             giveUps,
		CallerPod:          callerPodOf(s.runtime),
		Ready:              ready.serving,
		Supervisor:         s.supervisor,
		AwaitStream:        s.awaitStream,
		PodUpdateRounds:    s.podUpdateRoundsView,
		BootTokens:         s.tokens,
		Project:            p.project,
		DispatchProject:    cfg.Project,
		OperatorToken:      p.operatorToken,
		Controller:         st,
		Steps:              cfg.Gates.Steps(),
		ControllerLaunched: keeper != nil,
		PluginRelaunchable: p.pluginRelaunchable,
		NoPluginInstall:    func(c supervise.Claim) bool { return noPluginInstall(cfg, c) },
		Backend:            cfg.BackendFor,
		Stopping:           s.stopping,
		Drained:            s.draining,
		Decisions:          s.decided,
		Log:                s.log,
		Pool:               st.Pool(),
		StateTransactions:  st.Pool(),
		Handlers:           handlers,
		PrepareRepository:  prepare,
		RepositoryUnread:   unread,
		AwaitRecovery:      await,
		RoleFor:            phase.RoleFor,
		Record:             records,
		Dispatch:           client,
		Tokens:             tokens,
		Served:             p.repositories.servesRepository,
		RepositoryRenames:  cfg.RepositoryRenames,
		Serving:            p.repositories.serving,
		GitHubAPI:          githubAPI,
		Grants:             grants,
		Releaser:           s.supervisor.deps.Runtime,
		Trees:              st,
		Experiments:        cfg.ExperimentsDeclared(),
		ClaimReady:         claimReadyHook(keeper, claimReady),
		HandoffCompleted:   handoffCompleted,
		PromptAgents:       promptAgents(p.roleReferences),
		CapabilityReported: s.capabilityReported,
		RoleHoldersChanged: roleHoldersChanged,
		ReleaseRecords:     st,
		ClaimActivity:      st,
		Pause:              pauses,
	})

	group, serving := errgroup.WithContext(ctx)
	serverStarted := make(chan struct{})
	group.Go(func() error {
		close(serverStarted)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve the API on %s: %w", server.Addr, err)
		}
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
		reportStranded(report, st, projectRecords{Store: record.NewStore(), project: cfg.Project}, cfg.Project, p.project, cfg.Gates.Steps(), p.clock.Now, s.log)
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
	if keeper != nil {
		group.Go(func() error {
			keeper.run(store.ForConcurrentWorker(serving), p.orphanSweep)
			return nil
		})
	}
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
	admissionCap int
	startedAt    time.Time
	// now is the daemon's one clock (overrides.clock, the wall clock in production), the moment
	// the state is read at; nil, in a source a test builds by hand, is the wall clock.
	now func() time.Time
	// secretsLogin is the daemon's own agent-secrets token login client (newSecretsLogin), read
	// through LoginStatus for the state route; nil when the deployment enrolls no pod.
	secretsLogin *cipher.Client
	// dispatchSession is the session the daemon's own Dispatch calls run under
	// (dispatch.DaemonSession), and dispatchTokens the Sandbox runtime's counts of the Dispatch
	// tokens its launches hand out (sandbox.Runtime.DispatchTokens), which State serves together
	// as `dispatchCredential`. dispatchTokens is nil without dispatch_token_broker
	// (dispatchTokensOf), and the state then carries no such key.
	dispatchSession string
	dispatchTokens  func() sandbox.DispatchTokens
	// deployment is the deployment's capabilities as this boot knows them with the repository
	// caches it is handed (supervision.deploymentWith), whose report the state carries.
	deployment func([]sandbox.CacheStatus) capabilities.Deployment
	// reports is each claim's latest capability report (supervision.capabilityReports), which the
	// projection shows on the claim's view whatever process reported it.
	reports func() map[claim.Token]capabilities.Report
	// wakes is every wake the daemon's machines hold, as each machine last published it
	// (supervision.waitingWakes); nil for a daemon that supervises nothing.
	wakes func() []supervise.WaitingWake
	// repositories is the repository state the workflow fills, empty for a daemon with no
	// workflow, whose state serves no repository.
	repositories *repositories
	// caches is the Kubernetes runtime's repository caches with their last health reads
	// (supervision.cacheStatuses, sandbox.Runtime.RepositoryCaches); nil under a runtime that keeps
	// none. State reads it once and hands that read to deployment and to the repositories' view
	// alike, so the capability row and `repositories` describe one read.
	caches func() []sandbox.CacheStatus
	// gatedSteps are the design-gate steps the deployment gates (config.Gates.Steps). Every root
	// of the state answers the steps its tree is gated on, which these steps and the root's gate
	// rows make (record.TreeSteps); the empty list gates none.
	gatedSteps []record.DesignStep
	// experiments are the project's declared experiments (config.Project.ExperimentsDeclared),
	// whose order every issue's `assignments` ride in; the empty list declares none. It is read
	// once at boot for the reason settings is.
	experiments []config.Experiment
	// settings is the resolved configuration as the state shows it, built once here: Load reads
	// legion.yaml once and an edit to it restarts the daemon, so once-at-boot is live.
	settings api.Settings
	// noPluginInstall is whether a claim's launch installs no plugin at all (the package's
	// noPluginInstall over this daemon's configuration), the one term of the stale-plugin lane's
	// own rule that the claim itself does not carry, which State needs to count what that lane
	// has left (api.State.ClaimsToRefresh). Nil is every launch installing a plugin, which only
	// a source a test builds by hand reads as: serve sets this field over the daemon's own
	// configuration, whatever the runtime.
	noPluginInstall func(supervise.Claim) bool
	// architectStalls is, by root key, every arm of the architect-stalled reading that holds for
	// that root's architect (admit.Admission.ArchitectStallArms over the workflow's own admission),
	// which State serves as each root's `architectStalls`; nil for a daemon with no workflow,
	// which weighs no architect and serves no arm.
	architectStalls func(context.Context, pgx.Tx) (map[string][]record.ArchitectStallArm, error)
}

// projectRecords scopes the shared daemon database to the daemon's configured project without
// widening record.Store's fixed transaction interface; the workflow, its admission and its
// blockers mirror, the outbox, the API's routes, the project's pause and the state route read the
// store through it.
//
// Its rule is that a record of another project is no record of this daemon's (owns). The issues
// table holds the records of every project on the database, each under its project
// (record.Issue.Project, the Dispatch key, which is project's spelling too), and a record is read
// by key alone (record.Postgres.Issue), or by tree alone (TreeIssues), so a claim of another
// project's issue, which the operator's spawn makes under this daemon's own token, would find the
// record that project's daemon keeps of the issue. A row built from that record would be keyed by
// an issue whose rows the other project's daemon runs, against its own claims, and this daemon's
// outbox never does (record's DueRows reads the rows of its own project's keys); and a drop or a
// write it led to would change that daemon's rows. So each read here that returns issue records
// and names no project answers this project's alone: Issue answers no record, and no error, for
// a record of another project, and TreeIssues, Issues and QueuedRootsUnder leave such records
// out. Every read that starts from them is within this filtered set, and so is every path of a
// claim through the workflow: a claim of another project's issue reads no record at all, as a
// claim of an issue nobody records reads none. The reads that name a project (LiveIssues,
// IssuesOf, AncestorsOf, SlottedIssues, UnfilledIssues, UnnamedIssues) are scoped by their own
// SQL to the project their caller names, which every caller names as this one.
//
// Slots are not filtered here: a slot's index is unique across every project's slots, so
// admission chooses the next one over all of them and counts only its own against its cap
// (admit.ownSlots). The outbox's claim is scoped in its SQL instead, because the lease it takes is
// itself the damage.
//
// Its writes take the same rule from the other side: no row is written into the outbox keyed by
// another project's issue (ownsKey, claim.KeyOfProject). Every write of record.Store that inserts,
// upserts, expedites or deletes outbox rows by a key — Enqueue, EnqueueID, EnqueueStart,
// ExpediteOutbox, EnqueueDesignGates, EnqueueGatedSteps, DropCatchUps, DropStatusWrites, and
// ClearGeneration and ClearTreeGeneration, which queue the publication of the gates they empty —
// refuses a key of another project, and EnqueueID refuses it with claim.ErrKeyOfAnotherProject.
// The rest act on a row by its id under a lease this daemon holds of a row its own scope gave it
// (ClaimRow, RenewOutboxLease, FinishOutbox, FinishLeasedOutbox, RetryOutbox), and need no rule
// here.
//
// It is also where the outbox learns of its rows: every write that makes a row due — Enqueue, and
// ExpediteOutbox — wakes the row's lane (outbox.wake) once the transaction has committed, through
// the transaction's commit hooks (store.OnCommit), so no lane runs a row it cannot yet read, and
// no lane polls for one. wake is nil where there is no workflow to wake, or where the writer wakes
// its own lanes once it commits: the API-only daemon, the state route's reads, the stranded report
// and the project's pause (projectPause.stopped).
//
// And it is where the refresher learns that the live records may have changed: each write that can
// add a record to the live set or take one out of it — PutIssue, which writes an issue's phase and
// status, and ReleaseSlot, ReleaseSlotForWait and ReleaseTreeSlots, which take slots away, the
// tree's close and its root's re-admission among them — asks for one read of the live records once
// its transaction commits (live, the workflow's wakeLive, which holds one request at most), so the
// repository caches and legion state follow the records without a timer. live is nil where no
// refresher runs.
type projectRecords struct {
	record.Store
	project string
	wake    func(issue string, kind record.OutboxKind)
	live    func()
	// log says when a row was written outside a commit scope, and when a write of a row keyed by
	// another project's issue was refused; nil is the default logger.
	log *slog.Logger
}

func (s projectRecords) PutIssue(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	if err := s.Store.PutIssue(ctx, tx, issue); err != nil {
		return err
	}
	s.liveOnCommit(ctx)
	return nil
}

func (s projectRecords) ReleaseSlot(ctx context.Context, tx pgx.Tx, issue string) error {
	if err := s.Store.ReleaseSlot(ctx, tx, issue); err != nil {
		return err
	}
	s.liveOnCommit(ctx)
	return nil
}

func (s projectRecords) ReleaseSlotForWait(ctx context.Context, tx pgx.Tx, issue string, at time.Time) error {
	if err := s.Store.ReleaseSlotForWait(ctx, tx, issue, at); err != nil {
		return err
	}
	s.liveOnCommit(ctx)
	return nil
}

func (s projectRecords) ReleaseTreeSlots(ctx context.Context, tx pgx.Tx, tree string) ([]string, error) {
	released, err := s.Store.ReleaseTreeSlots(ctx, tx, tree)
	if err != nil {
		return nil, err
	}
	s.liveOnCommit(ctx)
	return released, nil
}

// liveOnCommit asks for a read of the live records once the transaction that wrote one commits
// (store.OnCommit), and at once for a write outside a commit scope, whose read then finds the
// write or a later one does.
func (s projectRecords) liveOnCommit(ctx context.Context) {
	if s.live == nil {
		return
	}
	if store.CommitScoped(ctx) {
		store.OnCommit(ctx, s.live)
		return
	}
	s.live()
}

// Issue is the record of key where it is a record of the daemon's project, and nil otherwise.
func (s projectRecords) Issue(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, error) {
	issue, err := s.Store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !s.owns(*issue) {
		return nil, err
	}
	return issue, nil
}

func (s projectRecords) Issues(ctx context.Context, tx pgx.Tx) ([]record.Issue, error) {
	all, err := s.Store.Issues(ctx, tx)
	if err != nil {
		return nil, err
	}
	return s.own(all), nil
}

func (s projectRecords) TreeIssues(ctx context.Context, tx pgx.Tx, tree string) ([]record.Issue, error) {
	all, err := s.Store.TreeIssues(ctx, tx, tree)
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

// own is every issue of all that the daemon's project owns (owns), in all's order.
func (s projectRecords) own(all []record.Issue) []record.Issue {
	issues := make([]record.Issue, 0, len(all))
	for _, issue := range all {
		if s.owns(issue) {
			issues = append(issues, issue)
		}
	}
	return issues
}

// owns is whether issue is a record of the daemon's project, the rule every read of issue records
// here applies (projectRecords).
func (s projectRecords) owns(issue record.Issue) bool {
	return issue.Project == s.project
}

// ownsKey is whether an outbox row of kind keyed key is the daemon's to write (claim.KeyOfProject),
// the rule every write of a row by key here applies (projectRecords). A write it refuses writes
// nothing and, but for EnqueueID, returns no error: the transaction the caller is in goes on, since
// a row the daemon would never run must not fail the caller's own work. The reads answer no record
// of another project's issue (Issue), so no row built from a record is keyed by one; and the rows
// supervision builds from a claim's issue with no record read, for its update rounds and its stall
// backstop, skip another project's issue before they reach here (workflowRuntime.enqueueOutbox),
// since a claim of one is the operator's ordinary spawn. So a refusal here happens only by a
// defect of the writer, and each is logged at Warn with the kind, the key and the project, so
// nothing is dropped in silence.
func (s projectRecords) ownsKey(kind record.OutboxKind, key string) bool {
	if claim.KeyOfProject(s.project, key) {
		return true
	}
	s.logger().Warn("outbox: refused a row keyed by another project's issue; only that project's daemon runs its rows",
		"kind", kind, "issue", key, "project", s.project)
	return false
}

// logger is log, or the default logger where it is nil.
func (s projectRecords) logger() *slog.Logger {
	if s.log == nil {
		return slog.Default()
	}
	return s.log
}

func (s projectRecords) Enqueue(ctx context.Context, tx pgx.Tx, row record.OutboxRow) error {
	if !s.ownsKey(row.Kind, row.Issue) {
		return nil
	}
	if err := s.Store.Enqueue(ctx, tx, row); err != nil {
		return err
	}
	s.wakeOnCommit(ctx, row)
	return nil
}

// EnqueueStart wakes the row's lane as Enqueue does: a phase worker's start is written through
// this statement since the start became a row of its own (workflow.StartOrOwe,
// record.Store.EnqueueStart), so without the wake here the worker whose phase has just been
// handed its work would wait for the lane's next sweep, up to one outbox lease after its
// transition committed.
func (s projectRecords) EnqueueStart(ctx context.Context, tx pgx.Tx, row record.OutboxRow) error {
	if !s.ownsKey(row.Kind, row.Issue) {
		return nil
	}
	if err := s.Store.EnqueueStart(ctx, tx, row); err != nil {
		return err
	}
	s.wakeOnCommit(ctx, row)
	return nil
}

// EnqueueID wakes the row's lane as Enqueue does: a start or catch-up naming kept decisions is
// written with its id (workflow.EnqueueNaming), so the row it claims them for waits for no sweep.
//
// Unlike the other writes, it refuses a row keyed by another project's issue with an error
// (claim.ErrKeyOfAnotherProject), as the store's insertOutboxRow does: every caller names the row
// by the id it answers — the decisions a start claims (ClaimKeptDecisions), the claims a re-held
// catch-up takes over (MoveKeptClaims), the start a pause's stop released — and an id of no row
// would name nothing, so the caller's transaction rolls back instead.
func (s projectRecords) EnqueueID(ctx context.Context, tx pgx.Tx, row record.OutboxRow) (int64, error) {
	if !s.ownsKey(row.Kind, row.Issue) {
		return 0, fmt.Errorf("enqueue %s for %s: %w", row.Kind, row.Issue, claim.ErrKeyOfAnotherProject)
	}
	id, err := s.Store.EnqueueID(ctx, tx, row)
	if err != nil {
		return 0, err
	}
	s.wakeOnCommit(ctx, row)
	return id, nil
}

func (s projectRecords) ExpediteOutbox(ctx context.Context, tx pgx.Tx, row record.OutboxRow, now time.Time) (bool, error) {
	if !s.ownsKey(row.Kind, row.Issue) {
		return false, nil
	}
	moved, err := s.Store.ExpediteOutbox(ctx, tx, row, now)
	if err != nil {
		return false, err
	}
	if moved {
		s.wakeOnCommit(ctx, row)
	}
	return moved, nil
}

// EnqueueDesignGates wakes the issue's board lane as Enqueue does. The enqueue writes its row
// with a statement of its own rather than through Enqueue (record.Postgres.EnqueueDesignGates),
// so this is the one place that can tell the lane a gate change left a publication due.
func (s projectRecords) EnqueueDesignGates(ctx context.Context, tx pgx.Tx, publish record.DesignGatesPublish, nextAt time.Time) error {
	if !s.ownsKey(record.OutboxKindDesignGatesPublish, publish.Issue) {
		return nil
	}
	if err := s.Store.EnqueueDesignGates(ctx, tx, publish, nextAt); err != nil {
		return err
	}
	s.wakeOnCommit(ctx, record.OutboxRow{Issue: publish.Issue, Kind: record.OutboxKindDesignGatesPublish})
	return nil
}

// EnqueueGatedSteps's row is keyed by the bare project key, not by an issue
// (record.Postgres.EnqueueGatedSteps), so that key is the one the rule reads.
func (s projectRecords) EnqueueGatedSteps(ctx context.Context, tx pgx.Tx, publish record.GatedStepsPublish, nextAt time.Time) error {
	if !s.ownsKey(record.OutboxKindGatedStepsPublish, publish.Project) {
		return nil
	}
	return s.Store.EnqueueGatedSteps(ctx, tx, publish, nextAt)
}

func (s projectRecords) DropCatchUps(ctx context.Context, tx pgx.Tx, issue string, now time.Time) error {
	if !s.ownsKey(record.OutboxKindNotice, issue) {
		return nil
	}
	return s.Store.DropCatchUps(ctx, tx, issue, now)
}

func (s projectRecords) DropUnfencedCheckWakes(ctx context.Context, tx pgx.Tx, issue string) error {
	if !s.ownsKey(record.OutboxKindCheckWake, issue) {
		return nil
	}
	return s.Store.DropUnfencedCheckWakes(ctx, tx, issue)
}

func (s projectRecords) DropStatusWrites(ctx context.Context, tx pgx.Tx, issue string) error {
	if !s.ownsKey(record.OutboxKindDispatchStatus, issue) {
		return nil
	}
	return s.Store.DropStatusWrites(ctx, tx, issue)
}

// ClearGeneration queues the publication of every gate it empties, keyed by the issue
// (record.Postgres.clearGates), so a clear of another project's issue is refused whole: the
// facts it would delete are that project's daemon's too.
func (s projectRecords) ClearGeneration(ctx context.Context, tx pgx.Tx, issue string, now time.Time) error {
	if !s.ownsKey(record.OutboxKindDesignGatesPublish, issue) {
		return nil
	}
	return s.Store.ClearGeneration(ctx, tx, issue, now)
}

// ClearTreeGeneration is ClearGeneration for every issue of tree, each of which is of tree's
// project: a record takes its root's project when the workflow writes it.
func (s projectRecords) ClearTreeGeneration(ctx context.Context, tx pgx.Tx, tree string, now time.Time) error {
	if !s.ownsKey(record.OutboxKindDesignGatesPublish, tree) {
		return nil
	}
	return s.Store.ClearTreeGeneration(ctx, tx, tree, now)
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
// outside a commit scope (store.Transact) has no commit to wait for: its lane is woken at
// once, and the write is logged with its caller, since a lane woken before the row is visible
// finds nothing and the row then waits for the sweep — every transaction that writes a row is
// meant to be scoped, and one that is not is a bug to fix where the log names.
func (s projectRecords) wakeOnCommit(ctx context.Context, row record.OutboxRow) {
	if s.wake == nil {
		return
	}
	if store.CommitScoped(ctx) {
		store.OnCommit(ctx, func() { s.wake(row.Issue, row.Kind) })
		return
	}
	// Caller 2 is the caller of Enqueue or ExpediteOutbox, past this helper and the method.
	_, file, line, _ := goruntime.Caller(2)
	s.logger().Error("outbox row written outside a commit scope; its lane is woken at once",
		"kind", row.Kind, "issue", row.Issue, "file", file, "line", line)
	s.wake(row.Issue, row.Kind)
}

// serializedTx runs write in one Transact under the lock every fact applies under
// (intake.SerializeFacts), so no fact applies with a copy of a record read before write wrote it
// or read it: the backfill's writes (fillRepositories, nameRepositories), the rename pass's
// (renamePass.write) and the refresher's read of the live records (refreshLive). write runs on
// the transaction's context, where its commit hooks register (store.OnCommit). A lock not taken
// is the error what names, wrapping the lock's own.
func serializedTx(ctx context.Context, pool *store.Pool, what string, write func(ctx context.Context, tx pgx.Tx) error) error {
	return store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return write(ctx, tx)
	})
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
	// The state's now is the daemon's one clock, which the wakes' stuck clocks are judged against
	// (dispatch://INFRA-3759); a source a test builds by hand reads the wall clock.
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
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
	stranded, err := workflow.Stranded(ctx, tx, records, s.project, s.gatedSteps, claims, resumes, now)
	if err != nil {
		return api.State{}, nil, err
	}
	// A wake waits in one of two places, and the state names both as one list (api.WakesWaitingOf):
	// a launch waits in its claim's machine, in memory alone, since no store holds one and a
	// restart raises the wave again through its own paths; a paced catch-up waits on its
	// outbox row, read here in the snapshot every other durable fact of this state is read from.
	//
	// The two are read before the projection and handed to it, so the state's `wakesWaiting` and
	// each claim view's own `wake` are set from one list. Each read is of this one snapshot.
	paced, err := pacedCatchUps(ctx, tx, records, s.projectToken, s.project, now)
	if err != nil {
		return api.State{}, nil, err
	}
	var held []supervise.WaitingWake
	if s.wakes != nil {
		held = s.wakes()
	}
	// The slots reading names the cap beside the slots in use, and reads each holder's claims
	// through the statement the tick's own reading reads them through, the one that holds when a
	// suspended or a retired claim stopped (record.Store.HolderClaims).
	holderClaims, err := records.HolderClaims(ctx, tx, s.projectToken)
	if err != nil {
		return api.State{}, nil, err
	}
	state, err := projection.Project(ctx, projection.Inputs{
		Tx: tx, Store: records, Project: s.project, Steps: s.gatedSteps, Experiments: s.experiments,
		Claims: claims, Stranded: stranded, Reports: s.reports(),
		Wakes: api.WakesWaitingOf(held, paced), Now: now, Cap: s.admissionCap, HolderClaims: holderClaims,
	})
	if err != nil {
		return api.State{}, nil, err
	}
	state.StrandedClaims = api.StrandedClaimsOf(stranded)
	// Which arms of the architect-stalled reading hold for each root, read in the same snapshot
	// by the reading the admission pass wakes the controller from (admit's ArchitectStallArms),
	// and served on each root's own view whether or not the controller was told. It is the
	// daemon's judgement as strandedClaims is, over records internal/projection may not read, so
	// it is put on the views here, beside that report, rather than judged a second time anywhere.
	if s.architectStalls != nil {
		stalls, err := s.architectStalls(ctx, tx)
		if err != nil {
			return api.State{}, nil, err
		}
		for key, arms := range stalls {
			if view, shown := state.Issues[key]; shown {
				view.ArchitectStalls = arms
				state.Issues[key] = view
			}
		}
	}
	// The standing hold of each issue's pod, read from pod_holds in the same snapshot, one
	// statement for the whole project. The idle rule writes it to that table alone, so this is the
	// one surface that says why a quiet pod still runs. pod_holds is
	// keyed on the claim token's spelling of the project, as the idle pass writes it
	// (releaseUnownedPods says why the two spellings differ).
	holds, err := s.store.StandingPodHoldsTx(ctx, tx, s.projectToken)
	if err != nil {
		return api.State{}, nil, err
	}
	for key, hold := range holds {
		if view, shown := state.Issues[key]; shown {
			view.PodHold = api.PodHoldViewOf(hold)
			state.Issues[key] = view
		}
	}
	state.ClaimsToRefresh = claimsToRefresh(claims, s.noPluginInstall)
	// The project's pause, read through tx as every other durable fact here (api.PauseStateTx):
	// `paused` with the roles it waits on and the rows it holds while the project is paused, and
	// `lastPause` while it runs after a pause has ended (decision 25 of dispatch://INFRA-3759).
	if state.Paused, state.LastPause, err = api.PauseStateTx(ctx, tx, s.project, now); err != nil {
		return api.State{}, nil, err
	}
	state.Daemon = api.DaemonInfo{
		Project:       s.project,
		SchemaVersion: version,
		Boots:         boots,
		FirstBootAt:   firstBootAt,
		StartedAt:     s.startedAt,
	}
	state.Settings = s.settings
	state.Admission.Cap = s.admissionCap
	controller, _, err := s.store.ControllerTx(ctx, tx, s.projectToken)
	if err != nil {
		return api.State{}, nil, err
	}
	state.ControllerLocator = api.ControllerLocatorOf(s.runtime, controller)
	if s.secretsLogin != nil {
		state.AgentSecretsLogin = &api.AgentSecretsLoginView{State: s.secretsLogin.LoginStatus().State}
	}
	if s.dispatchTokens != nil {
		tokens := s.dispatchTokens()
		credential := &api.DispatchCredentialView{
			Session: s.dispatchSession, TransitionLaunches: tokens.TransitionLaunches,
			LiveLaunchesWithDispatchToken: tokens.LiveLaunches,
		}
		if !tokens.LastTransitionLaunch.IsZero() {
			at := tokens.LastTransitionLaunch.UTC()
			credential.LastTransitionLaunchAt = &at
		}
		state.DispatchCredential = credential
	}
	var caches []sandbox.CacheStatus
	if s.caches != nil {
		caches = s.caches()
	}
	state.Capabilities = api.CapabilityStatesOf(s.deployment(caches).Report())
	if s.repositories != nil {
		// The repositories the block lists are those of the live records this same transaction
		// reads, so the list and the issues the body shows agree by construction rather than
		// trailing a record committed since the refresher's last read of the records.
		issues, err := s.records.LiveIssues(ctx, tx, s.project)
		if err != nil {
			return api.State{}, nil, err
		}
		live, err := repositoriesOf(issues)
		if err != nil {
			return api.State{}, nil, err
		}
		state.Repositories = s.repositories.view(live, caches)
	}
	return state, claims, nil
}

// adoptIdentity is the supervisor's identity for a working copy's adoption (supervise.Deps.Identity)
// over identity, for the repository the claim's issue works (repository): its record's, or, for an
// issue the workflow does not record, the one kept for it at its first launch, or read from
// Dispatch where none is (workflowRuntime.issueRepository). The owner the lease is
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

// repositoryCachesOf is rt's repository caches when rt is the Sandbox runtime, the one runtime that
// keeps them, and nil under any other.
func repositoryCachesOf(rt runtime.Runtime) func() []sandbox.CacheStatus {
	if keeper, ok := rt.(*sandbox.Runtime); ok {
		return keeper.RepositoryCaches
	}
	return nil
}

// dispatchTokensOf is rt's counts of the Dispatch tokens its launches hand out
// (sandbox.Runtime.DispatchTokens) when the deployment runs under dispatch_token_broker and rt is
// the Sandbox runtime, the one runtime the loader takes that setting under, and nil otherwise, as
// repositoryCachesOf is: a deployment without the setting serves no `dispatchCredential`.
func dispatchTokensOf(cfg config.Config, rt runtime.Runtime) func() sandbox.DispatchTokens {
	if counter, ok := rt.(*sandbox.Runtime); ok && cfg.DispatchTokenBroker {
		return counter.DispatchTokens
	}
	return nil
}

// callerPodOf is rt's pod at an address when rt is the Sandbox runtime, the one runtime that runs
// pods, and nil under any other (api's Options.CallerPod).
func callerPodOf(rt runtime.Runtime) func(netip.Addr) (string, bool) {
	if pods, ok := rt.(*sandbox.Runtime); ok {
		return pods.PodAt
	}
	return nil
}
