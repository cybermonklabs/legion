package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// workflowRuntime is the daemon-owned integration surface around the pure transition engine.
// It is absent for Stage 2 configurations, whose unchanged supervision proof deliberately has no
// Dispatch, Apps, or Envoy stream configured.
type workflowRuntime struct {
	// pool is the daemon's guarded pool (store.Pool): every transaction the runtime's loops,
	// lanes and callbacks open comes from it, and each of them marks a context of its own so a
	// second connection asked for under a held one is refused at that call rather than waited for
	// (store.ErrNestedAcquire).
	pool      *store.Pool
	records   record.Store
	engine    *workflow.Engine
	admission *admit.Admission
	handlers  []intake.Handler
	dispatch  dispatch.Client
	// gatedSteps are the design-gate steps the project gates, which the send-time narrowing of a
	// held architect task reads: a catch-up it writes to another tree's architect is composed
	// under them (rootTask, workflow.CatchUpTheTreesOwed). The empty list gates no step
	// (config.Gates.Steps).
	gatedSteps []record.DesignStep
	// tokens mints every lease the workflow's own reads and writes use. apps is that same token
	// source whole, the leases and the owners each App is installed on (appauth.Source), which
	// the daemon hands on to the runtime it builds and to the API's routes (run, serve): both
	// build gh bundles, which need the owners too. The workflow itself asks for no owner list,
	// so its own code and its tests hold the narrower type.
	tokens appauth.Tokens
	apps   appauth.Source
	// forget drops the token source's cached leases for an App and owner, a token GitHub refused
	// (tokenRefused).
	forget          appauth.Forgetter
	grants          *credential.Grants
	projectID       string
	dispatchProject string
	stateDir        string
	log             *slog.Logger
	// conn is the Envoy NATS connection. It is written only on run's own goroutine while the
	// daemon boots, once per connect attempt: connect runs there, inside bootprobe.Run, which
	// retries it, and a failed attempt stores nil (daemon.go). Every one of those writes is
	// before the serving goroutine starts. It is read afterwards from several others — the serving
	// goroutine's workflowRuntime.stop, reconnectWatchdog, the intake group — and from run itself
	// once awaitStop has returned. Two of those close it at once when the stop's budget runs out
	// (closeNATS), so the field is held atomically rather than leaving each reader to prove its
	// own ordering, and internal/daemon runs under the race detector in CI
	// (.github/workflows/ci.yaml, the step "Test the concurrent packages under the race
	// detector").
	conn      atomic.Pointer[nats.Conn]
	consumers *intake.Consumers
	// setRepositories replaces the repositories GitHub intake hears, the one route the runtime
	// reaches that stream call by (updateSubscription). The boot leaves it nil, which means
	// w.consumers.SetRepositories: the boot opens the consumers (connect) before anything that
	// hears a repository runs — a fact that records an issue, a launch, the refresher's backfill —
	// so no caller asks whether they are open. A test sets it to a stand-in, since consumers is the
	// concrete *intake.Consumers: a test of the heard set's publication, which has to hold the call
	// while it proves no transaction waits on it (INFRA-3169), and a test whose runtime opens no
	// consumers and whose launch or backfill hears a repository (hearsNothing).
	setRepositories func(ctx context.Context, repositories []ghrepo.Repository) error
	outbox          *outbox
	// outboxWake is the outbox as the records' wake reaches it (wakeOutbox): the records are built
	// at open, before the outbox exists, and the facts the boot applies before attach write rows
	// through them, so the wake forwards to whatever attach has stored, and to nothing before
	// then — the outbox's boot sweep finds every row written before it ran. The engine's read of a
	// step's agent reaches the supervisor through it too (stepAgent), and reads nothing before.
	outboxWake atomic.Pointer[outbox]
	catchUps   *catchUpPacing
	// pauses is the project's pause (projectPause, dispatch://INFRA-3759): the pause service the
	// API's routes are handed, with the stop transaction, the let-out stamp and the after-commit
	// steps the machines and the service call. run builds it once this runtime is open and before
	// supervision, since a pause's stops run on this runtime's outbox; a runtime a test builds by
	// hand holds none.
	pauses *projectPause
	// limits are where the deployment's claim budgets run out (supervise.Limits, built once at
	// daemon.go's Deps), taken from the supervisor at attach: a failed claim's fact carries them
	// to the workflow engine (applyTerminal), which holds neither the claim nor the configuration
	// and so has no other way to read them.
	limits supervise.Limits
	// bootID disambiguates this boot's synthetic Dispatch consumer position facts from another
	// boot's, or another project's daemon: processed_events is keyed only on (source, event_id),
	// shared by every project's daemon in the database, and a stream recreated by a later boot
	// would otherwise repeat the same (ack floor, idle) pairs and have its own releases swallowed
	// as duplicates of the previous boot's.
	bootID string
	// caughtUp names each root (key and generation) the boot's catch-up finished, so a retry of the
	// catch-up resumes after it rather than reading it again (catchUpAwaiting), and the reconcile's
	// retry does not hold it again (holdForCatchUp). The boot alone writes it, from its one
	// readiness goroutine.
	caughtUp map[string]struct{}
	// catchUpRootTimeout bounds the boot's catch-up of one root; zero means bootTimeout. A test
	// shortens it.
	catchUpRootTimeout time.Duration
	// catchUpRetryDelay is the wait between two reads of one root Dispatch refused with an error it
	// may stop giving; zero means catchUpRetryDelay's default. A test shortens it.
	catchUpRetryDelay time.Duration
	// holdPollInterval is pollHoldRelease's ticker period; zero means the production default. A
	// test shortens it to bound how long a release takes to observe.
	holdPollInterval time.Duration
	// controllerWake is tickController's period (`controller_wake_interval_seconds`); zero, in a
	// test that builds the runtime by hand, means the configuration's default hour.
	controllerWake time.Duration
	// controllerFirstWake replaces firstControllerTick in a test; zero is the production delay.
	controllerFirstWake time.Duration
	// idlePeriod is how long an issue pod must have had no turn in progress and no turn due
	// before the daemon releases it (runtime.kubernetes.idle_release.idle_minutes); zero turns
	// release off, as `idle_minutes: 0` and every runtime but Kubernetes do. idleStore is the
	// daemon's store, which holds the idle view and the release's mark, and idleRuntime the
	// runtime the pass asks about the pod itself — its image, its Sandbox (issuePods); both nil
	// when release is off. idleTick is the tick's period, zero being idleTickInterval; a test
	// shortens it.
	idlePeriod  time.Duration
	idleStore   *store.Store
	idleRuntime issuePods
	idleTick    time.Duration
	// now is the daemon's one clock (overrides.clock, the machines' clock, the wall clock in
	// production), which the engine stamps its deadlines with (workflow.Config.Clock) and the
	// idle, wake and stuck passes read their now from, so a test clock moves all of them
	// together; nil, in a runtime a test builds by hand, is the wall clock (clockNow).
	now func() time.Time
	// entrySweep is tickEntrySweep's period, zero being entrySweepInterval; a test shortens it
	// so it waits out its own period rather than the daemon's minute.
	entrySweep time.Duration
	// outboxSweep is how often the outbox sweeps its lanes (overrides.outboxSweep), which attach
	// hands the outbox (outbox.sweepEvery); zero is outboxLease.
	outboxSweep time.Duration
	// holdWarnAfter and holdWarnEvery bound the watchdog log a hold that never releases gets: zero
	// means the production defaults. A test shortens both to bound how long the warning takes to
	// observe.
	holdWarnAfter, holdWarnEvery time.Duration
	// reconnectPollInterval and reconnectWarnEvery bound reconnectWatchdog's ticker and how often
	// it logs while NATS has not reconnected: zero means the production defaults. A test
	// shortens both to bound how long the warning takes to observe.
	reconnectPollInterval, reconnectWarnEvery time.Duration
	// githubAPI is the GitHub REST root the workflow calls: the per-issue read reads under it, and
	// the outbox's issue_branch rows call under it. Empty, in production, is
	// https://api.github.com, and a test points it at a stand-in.
	githubAPI string
	// defaultBranches is each repository as GitHub names it and its default branch, read under
	// githubAPI and kept defaultBranchTTL: what an issue's record takes its repository and default
	// branch from when it is first recorded (ResolveRepository, through the repository state that
	// its preparations fill). Every read of it is made of the repository the rename rule serves
	// the asked-for one as (repositories.serving), so a name recorded before a configured rename's
	// transfer is read on the installation that holds the repository now.
	defaultBranches *defaultBranches
	// repositories is the repository state: what GitHub answered for each repository the daemon
	// prepared, and the repositories the live records name. installs answers each App's
	// installation on a repository (prepareName). recovering is each repository,
	// lowercased, GitHub answered for again whose issues are still to recover from Dispatch's
	// listing (recoverRepositories): one a preparation left unread and whose reads a later
	// preparation got answered, or that a recovery found an entry of another name answering for
	// (readAnswered, deferUnreadIssues), under recoveringMu. silenced is, by the same
	// repository, each issue whose apply said nothing because the repository was unread
	// (noteUnread), which the repository's recovery takes into its scope whatever the issue
	// resolves to by then; releaseWaits is, by repository, each child a release_children left
	// unrecorded on it (awaitRecovery), whose architect the recovery tells of each it did not
	// record (tellReleaseWaits), and which leave with the repository's own marker where a pass
	// stops reading GitHub for it (settleRecovery, tellLetGoWaits); recoveryHeldSince is when a
	// recovery's listing was first found held with waits to tell (zero for none), so a hold past
	// the read bounds' holdBound (repositoryReadBounds) tells them, and recoveryBoundChecked
	// whether a run of the recovery has begun with that bound passed (recoverRepositories), after
	// which the refresher no longer cuts its wait to it.
	// recheckAfter is the refresher's wait before it runs a job of its own (refreshRepositories),
	// time.After when nil; a test sees each wait through it.
	repositories         *repositories
	installs             appauth.Installations
	recoveringMu         sync.Mutex
	recovering           map[string]bool
	silenced             map[string]map[string]bool
	releaseWaits         map[string]map[string]releaseWait
	recoveryHeldSince    time.Time
	recoveryBoundChecked bool
	recheckAfter         func(time.Duration) <-chan time.Time
	// rereadWake, recoverWake, heardWake, heldWake and liveWake are the refresher's wakes, one
	// request held at most each (wake): a bad answer kept outside the refresher (its second job's
	// clock), a repository left unread or answered again (its third, recoverRepositories), a
	// consumer update JetStream refused (its fourth, republishHeard), a default branch held through
	// a rate limit (its fifth, rereadHeldBranches), and a write that can change the live records
	// (its sixth, refreshLive). heldReads is each repository whose default branch is held through a
	// rate limit, by its lowercased <owner>/<name>, with the limit's end (heldThroughLimit), under
	// heldMu. backfillPending marks a record the backfill left without its repository columns,
	// which the second job tries again (backfillLater).
	rereadWake, recoverWake, heardWake, heldWake, liveWake chan struct{}
	heldMu                                                 sync.Mutex
	heldReads                                              map[string]limitedRead
	backfillPending                                        atomic.Bool
	// heard is what GitHub intake hears (heardRepositories): the boot's set (heardAtBoot), each
	// repository a record first names, a launch works or the refresher's backfill writes on a
	// record added to it with no read of the records (hearOrPend, hearRepository), and the whole set
	// published again after JetStream refused an addition (republishHeard). Every write holds
	// heardMu, and publishes a set that holds every repository the one before it held; a recording
	// reads it with no lock at all, so one whose repository it names waits on nothing
	// (hearRepository, which says why reading it without the lock is sound).
	// heardPending marks a consumer update JetStream refused, and heardMissed is each repository
	// whose own addition it refused, under heardMu, which the refresher's next publication adds.
	// unread is each repository whose GitHub reads a preparation could not make (leaveUnread) or
	// the boot's listing did not make (leaveForRecovery), lowercased, with why, under
	// recoveringMu: while any is, the refresher recovers again of its own and so reads GitHub again
	// for the issues each left. unreadMarks is, by the same repository, the last mark a
	// preparation's failure or GitHub's answer gave it (markUnread, readAnswered), each a value of
	// the sequence unreadMarked no mark held before, under recoveringMu. It exists because a
	// marker already present is no evidence that nothing happened: leaveUnread returns at once
	// when the repository was unread already, recording nothing, so without the mark a preparation
	// that failed inside a recovery's own window — an issue moved to todo after its listing was
	// read — is invisible to the pass that then lets the repository go, and that issue's note of
	// its silence is read by no later pass (settleRecovery). cacheRepos is what the runtime keeps
	// a repository cache of (cacheRepositories), published for the sweep to read without a lock.
	heardMu      sync.Mutex
	heard        atomic.Pointer[[]ghrepo.Repository]
	heardPending atomic.Bool
	heardMissed  []ghrepo.Repository
	unread       map[string]string
	unreadMarks  map[string]uint64
	unreadMarked uint64
	cacheReposMu sync.Mutex
	cacheRepos   atomic.Pointer[[]ghrepo.Repository]
	// launched is the repository and default branch kept for each issue the workflow does not
	// record, by its lowercased key, at the first launch of a claim on it (unrecordedRepository),
	// under launchedMu; workspaceHeld is whether an issue still holds its workspace, which a kept
	// one is answered only while it does (keptRepository): its Sandbox under Kubernetes, its
	// workspace directory otherwise, set at attach, and nil — no workspace held — before it.
	launchedMu    sync.Mutex
	launched      map[string]ghbranch.Repository
	workspaceHeld func(issue string, repo ghrepo.Repository) bool
	// reviewAppLogin is the review App's bot login from its boot lease, as the engine holds it: a
	// review it submits decides a round by that login and the reviewer's recorded session
	// (workflow's DecidesRound), so reviewerCanWrite asks GitHub nothing about it.
	reviewAppLogin string
	// permissions holds, by repository and login, until when reviewerCanWrite's read of GitHub
	// stands as no write access; permissionTTL is how long one stands, zero, in production, being
	// reviewPermissionTTL, and a test shortens it. limitedUntil is when GitHub's last rate limit
	// stops asking to be left alone: while it stands, no read is made.
	permissionsMu sync.Mutex
	permissions   map[permissionKey]time.Time
	permissionTTL time.Duration
	limitedUntil  time.Time
	// refusedReviews is every review, by GitHub's review id, whose author a read of a pull
	// request's reviews found the workflow does not let decide (reviewToApply), so this process
	// judges each such review once, as a webhook's review is judged once when it is delivered: an
	// account given write access later never turns a review the read refused into a decision. It is
	// held in memory only, so a restart's first read judges again with the permission GitHub gives
	// then.
	//
	// readMemoMu guards it: the per-issue read (readIssuePullRequest) judges reviews on a
	// goroutine of its own (reviewRefused).
	readMemoMu     sync.Mutex
	refusedReviews map[int64]bool
	// failed carries the first supervision terminal fact that could not be applied. serve stops
	// the daemon with it: the claim's terminal state is durable, so the next boot's replay applies
	// the fact the failed callback lost.
	failed chan error
	// readied is the architects claimReady recorded whose waiting notices releaseReadied has not
	// released yet.
	readied *wakeSet[claim.Token]
	// handedOff is each issue whose agent's handoff the API recorded (handoffCompleted) that
	// readHandedOff has not read the pull request of yet, which is what makes a second completion
	// of the same issue, while the first is still queued, one read rather than two.
	handedOff *wakeSet[string]
	// mergeStates is each issue a fact owes a read of GitHub's merge state (queueMergeState, the
	// engine's Config.ReadMergeState: a round waiting on GitHub's answer, and by the gate
	// classify.MergeStateReadOwed a head move in implementing through merging), each the start read
	// found GitHub still computing (readIssuesAtStart), and each a second read of a conflicting head
	// left reading no answer (queueMergeStateFollow), that readMergeStates has not started a ladder
	// for yet; mergeStateQueued is what each of them asked for, the pull request its ladder hands
	// to the reads of its base, as the read that handed it over held it, or nil for a climb
	// (queueMergeStateFollow); and mergeStateRunning each issue whose ladder is still climbing
	// (mergeStateRun), which is why a second fact of an issue already being read starts no second
	// ladder. mergeStateRetry is how long a ladder waits between reads of a state GitHub answers
	// `unknown` for, the unit the reads after a ladder wait in, and how long the outbox's
	// merge-queue read and second conflict read wait before they read again (mergeStateWait, which
	// attach hands the outbox), mergeStateLadderWait when zero, which a test shortens
	// (overrides.mergeStateRetry).
	//
	// baseReads is each base of a repository whose owed pull requests the daemon reads in batches
	// (merge_state_batches.go's baseRead): a push to that base owes them a read (queueBaseMergeStates,
	// the engine's Config.ReadBaseMergeStates), or a ladder handed it a head GitHub still answers
	// `unknown` for (followOnBase). baseReadOf is the base each owed issue is read under,
	// baseWake the bases readBaseMergeStates has not started a batch ladder for yet, and baseAsks
	// the number of the last request put in any batch (putLocked, baseOwed.asked): one count for
	// the runtime, so a batch made again for a base whose last batch ended reuses no number a climb
	// or a read still holds. All of it is guarded by mergeStateMu, so a request moves an issue
	// between a ladder of its own and its base's batch under one lock. baseChunk is how many pull
	// requests one query of a batch asks for, baseReadChunk when zero, which a test shortens.
	mergeStates       *wakeSet[string]
	mergeStateMu      sync.Mutex
	mergeStateQueued  map[string]*record.PullRequest
	mergeStateRunning map[string]*mergeStateRun
	mergeStateRetry   time.Duration
	baseWake          *wakeSet[string]
	baseReads         map[string]*baseRead
	baseReadOf        map[string]string
	baseAsks          uint64
	baseChunk         int
	// gates is each installation's GraphQL queries, by the implement App's scope for the owner
	// (implementScope; merge_state_gate.go's queryGate): one rate-limit hold, one pace and one
	// query at a time, shared by every base's batch of that installation. gateMu guards the map.
	// graphQLShare is the share of the time each gate's queries may keep GitHub at work, the
	// configuration's `github_graphql_share` (queryShare).
	gateMu       sync.Mutex
	gates        map[string]*queryGate
	graphQLShare float64
	// etags is what the conditional reads of a pull request and of its base's rules keep
	// (githubrest.ETags, readPullRequest, readBaseRules): GitHub answers an unchanged one 304, which
	// costs the implement App's installation none of its rate limit.
	etags *githubrest.ETags
	// appCalls are the start's App-call bounds the daemon was given (overrides.appCalls), nil for
	// the production ones (callBounds): they bound the start's App calls and the runtime's reads
	// that take the same bound (recordedRepositories, recordedBranches, rereadHeldBranches).
	appCalls *appCallBounds
	// readBounds are the bounds of the runtime's reads for a repository and of the refresher's
	// waits, nil for the production ones (reads). Only a test sets them (withReads).
	readBounds *repositoryReadBounds
}

// readyReleaseTimeout bounds one release of ready architects' waiting notices
// (outbox.releaseWaiting): a read of a project's waiting notices and their trees, then one update,
// each well under a second. A release that runs this long holds a database that stopped answering;
// it is logged, and the notices it would have released still go on their schedule.
const readyReleaseTimeout = 30 * time.Second

// appCallBounds are the bounds of the start's App calls, the logins read (appLoginsAtBoot) and the
// key proof (proveAppKeys), which bound some of the runtime's own reads too (callBounds). A daemon
// is given its bounds (overrides.appCalls, openWorkflow's appCalls), the production ones
// (productionAppCalls) where it is given none, so a test that shortens them shortens them for the
// daemons it boots alone.
type appCallBounds struct {
	// attempt bounds one attempt at an App call: the bot identity lookups and each App's read of
	// its installations under its JWT answer in well under a second, so an attempt that runs this
	// long holds a request GitHub never answered.
	attempt time.Duration
	// retry is the wait between attempts that failed transiently.
	retry bootprobe.Retry
	// rateLimits is how the start waits for GitHub's rate limit (rateLimitPolicy).
	rateLimits rateLimitPolicy
	// clock is the time every wait of the start's App calls reads and waits on: GitHub's rate
	// limit (startWaits) and the waits between attempts, transient, pending or waiting
	// (bootprobe.RunOn). Production's is the wall clock; a test's moves on by each wait at once, so
	// a wait for GitHub's reset or for the minute a refusal that names no time asks costs the test
	// nothing. It is not openWorkflow's clock: the machines' test clocks (daemon_test.go's
	// stillClock) return from Sleep at once without moving, so a rate-limit wait on them would never
	// end and the start would spin.
	clock bootprobe.Clock
}

// productionAppCalls are the bounds a daemon's start runs on. An attempt lasts 15 s. The wait
// between attempts that failed transiently is 5 s doubling to 30 s, five attempts. At worst the
// boot waits 2 min 20 s (five 15 s attempts and 65 s between them) before it refuses, naming the
// last failure. The API listens only after the key proof, so /healthz is refused meanwhile; the
// rest of a boot takes a second or two, which leaves a boot that passes on the last attempt inside
// the 180 s Stage 3 gives a daemon to answer /healthz. A wait for GitHub's rate limit is none of
// these attempts: it lasts until the time GitHub names, within the start's own bound (appAttempt,
// rateLimits). Of that policy, a wait lasts at most an hour, the window of GitHub's REST limit, so
// a spent counter is waited until its reset and a Retry-After longer than that is not waited
// whole. All waits of one start end 70 minutes after the first, the hour and ten minutes more:
// the refusals of 2026-11-14 and 2026-11-15 ended 5.6 to 13.9 minutes after the first exit. A
// refusal that names no time waits a minute, the least GitHub's REST documentation gives.
func productionAppCalls() appCallBounds {
	return appCallBounds{
		attempt:    15 * time.Second,
		retry:      bootprobe.Retry{Initial: 5 * time.Second, Max: 30 * time.Second, Attempts: 5},
		rateLimits: rateLimitPolicy{Max: time.Hour, Bound: 70 * time.Minute, NoTime: time.Minute},
		clock:      bootprobe.WallClock{},
	}
}

// callBounds are the bounds this runtime's daemon was given (appCalls), the production ones
// where it was given none.
func (w *workflowRuntime) callBounds() appCallBounds {
	if w.appCalls != nil {
		return *w.appCalls
	}
	return productionAppCalls()
}

// appTransient is a failure GitHub reports as its own trouble (appauth.TransientError).
func appTransient(err error) bool {
	var transient *appauth.TransientError
	return errors.As(err, &transient)
}

// appAttempt is one attempt at call, an App call of the start (the App logins read or the key
// proof), bounded by the start's attempt bound (appCallBounds.attempt). The error the attempt
// returns decides its outcome, in this
// order. No error passes. A token_command still running past the bound (appauth.ErrCommandPending,
// most often for a grant that waits on a person's approval) is pending, and a pending attempt
// spends no retry: the command goes on, and the next attempt joins it, so the start waits for the
// grant with one request and never kills it. GitHub's rate limit is a wait on the start's waits
// until the time GitHub names, and spends no retry either, since it says nothing about the App;
// once those waits reach the start's end, it is a refusal (limitOutcome). GitHub's own trouble
// (appTransient) is transient. So is an attempt that failed because its own bound ended while the
// start's ctx is still alive: a request GitHub has not answered is GitHub's trouble, not the
// App's, and it runs again within the start's retry (appCallBounds.retry) rather than refusing the
// start (#1347). Any other
// error is a refusal.
//
// Under an App's JWT the next attempt of its installation read joins the shared read still in
// flight (appauth.Manager.InstalledOwners), which appauth drops at its installationReadLimit
// (1 min) when GitHub never answers it, inside the production retry's worst case of 2 min 20 s, so an
// attempt after that asks GitHub afresh. In token mode the installation read and the logins read
// are both the App's token_command's release of every installation, whose run appauth's
// runTokenCommand shares: an attempt whose bound ends while the command runs is pending
// (ErrCommandPending), the next attempt joins the run still going, and a run that finishes
// between two attempts is kept for the next one (appauth's finishedRunKeep), so the command does
// not run again for it. The logins read shares those runs; appauth's anyInstallation says why the
// key proof never takes a release the logins read was answered with. The logins read keeps the
// installation it took until it has read the App's login under it (appauth's loginRelease), and
// an attempt of it reads only the Apps whose login no attempt before it read (appLoginsAtBoot):
// each App's command runs again for the logins read only after a run of it failed, or once the
// release it took has come within appauth's refreshWindow of its expiry.
func appAttempt(waits *startWaits, call string, attempt func(context.Context) error) func(context.Context) bootprobe.Outcome {
	return func(ctx context.Context) bootprobe.Outcome {
		bounded, cancel := context.WithTimeout(ctx, waits.bounds.attempt)
		defer cancel()
		err := attempt(bounded)
		if err == nil {
			return bootprobe.Outcome{Passed: true}
		}
		if errors.Is(err, appauth.ErrCommandPending) {
			return bootprobe.Outcome{Pending: err.Error()}
		}
		if outcome, limited := limitOutcome(err, waits, call); limited {
			return outcome
		}
		if appTransient(err) || errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return bootprobe.Outcome{Detail: err.Error()}
		}
		return bootprobe.Outcome{Refusal: err}
	}
}

// limitOutcome is the outcome of an attempt at the start's call that failed with err, when err is
// GitHub's rate limit on an App call (bootLimit): a wait until the time the start's waits give
// (startWaits.wait), with the limit as GitHub's answer gave it (limitSummary), or, once those waits
// have reached their end, a refusal naming the call, the time the start waited, and GitHub's last
// answer with its message. limited is false for every other err.
func limitOutcome(err error, waits *startWaits, call string) (outcome bootprobe.Outcome, limited bool) {
	answer, limited := bootLimit(err)
	if !limited {
		return bootprobe.Outcome{}, false
	}
	until, waited, ended := waits.wait(call, answer)
	if ended {
		return bootprobe.Outcome{Refusal: fmt.Errorf("the %s waited %s for GitHub's rate limit, the most a start waits, and GitHub still refuses it: GitHub answered %s %s with %s: %s: %w",
			call, bootprobe.ShownWait(waited), answer.Method, answer.Path, limitSummary(answer), answer.Body, err)}, true
	}
	return bootprobe.Outcome{RateLimit: limitSummary(answer), Until: until, Waited: waited, Detail: err.Error()}, true
}

// limitSummary is what an answer that refused a start's call for GitHub's rate limit says of the
// limit: its status and the rate-limit headers that decide a wait, as GitHub sent them ("" for
// each it did not send), as limitAttributes gives them on the default-branch read's lines.
func limitSummary(answer *githubrest.Answer) string {
	limit := answer.Limit
	return fmt.Sprintf("%d (Retry-After %q, X-RateLimit-Remaining %q, X-RateLimit-Reset %q, X-RateLimit-Resource %q)",
		answer.Status, limit.RetryAfter, limit.Remaining, limit.Reset, limit.Resource)
}

// bootLimit is GitHub's answer to an App call of the start that failed with err — the App logins
// read or the key proof, whose error carries the answer appauth read (appauth.AnswerError) — when
// that answer is GitHub's rate limit (answerLimit). How long GitHub asks the start to wait the
// start reads from the answer's headers at its own time (startWaits.wait).
func bootLimit(err error) (*githubrest.Answer, bool) {
	var refused *appauth.AnswerError
	if !errors.As(err, &refused) {
		return nil, false
	}
	if _, limited := answerLimit(refused.Answer); !limited {
		return nil, false
	}
	return refused.Answer, true
}

// rateLimitPolicy is how the start waits for GitHub's rate limit (dispatch://INFRA-2597/spec R4):
// Max, the longest one wait lasts, whatever GitHub names; Bound, how long after the start's first
// such wait every one of them ends, none ending at all when it is zero; and NoTime, the wait for a
// call's first refusal that names no time, doubled for each further one of the same call.
type rateLimitPolicy struct {
	Max, Bound, NoTime time.Duration
}

// startWaits is one start's waits for GitHub's rate limit on one start's bounds (their
// rateLimits) and their clock, which bootprobe.RunOn waits on too: the App logins read and then the
// key proof, which openWorkflow runs one after the other (wait). end is the start's end, zero
// until its first wait sets it Bound after that wait begins; noTime is the last wait each App call
// took for a refusal that named no time; and waited is the time the start has spent in its waits.
// bounds are the start's App-call bounds, whose attempt and retry the start's calls run on
// (appAttempt, appLoginsAtBoot, proveAppKeys).
type startWaits struct {
	bounds appCallBounds
	end    time.Time
	noTime map[string]time.Duration
	waited time.Duration
}

// newStartWaits is one start's waits on bounds.
func newStartWaits(bounds appCallBounds) *startWaits {
	return &startWaits{bounds: bounds, noTime: map[string]time.Duration{}}
}

// wait is the wait for GitHub's rate limit on the start's App call call (the logins read, the key
// proof), asked as soon as the call's attempt returned with answer, GitHub's refusal. Where GitHub
// named a time, the wait is the one its headers ask for at the bounds' clock's now
// (LimitHeaders.WaitAt) with at most Max; where it named none, NoTime for the call's first such
// refusal of the start and two times its wait before for each further one. It says until when, at
// the start's end at the latest, and the time the start has waited so far, this wait included
// (register); or, once now is at or past the start's end, ended, with no wait: the call then ends
// the start. GitHub's wait is read at now rather than taken from answer.RetryAfter, which
// githubrest read on the host's clock, so the wait, its until and the start's end are on one
// clock.
func (s *startWaits) wait(call string, answer *githubrest.Answer) (until time.Time, waited time.Duration, ended bool) {
	now := s.bounds.clock.Now()
	if !s.end.IsZero() && !now.Before(s.end) {
		return now, s.waited, true
	}
	wait, named := answer.Limit.WaitAt(now)
	wait = min(wait, s.bounds.rateLimits.Max)
	if !named {
		wait = s.bounds.rateLimits.NoTime
		if last, ok := s.noTime[call]; ok {
			wait = 2 * last
		}
		s.noTime[call] = wait
	}
	until, waited = s.register(now, now.Add(wait))
	return until, waited, false
}

// register counts a wait from begin until until, cut at the start's end, and says until and the
// time the start has waited so far: the sum of its waits, each cut at the end. The start's calls
// run one after the other, so no two of its waits overlap. The start's first wait sets its end,
// Bound after that wait begins.
func (s *startWaits) register(begin, until time.Time) (time.Time, time.Duration) {
	if s.end.IsZero() && s.bounds.rateLimits.Bound > 0 {
		s.end = begin.Add(s.bounds.rateLimits.Bound)
	}
	if !s.end.IsZero() && until.After(s.end) {
		until = s.end
	}
	s.waited += until.Sub(begin)
	return until, s.waited
}

// appLoginsAtBoot reads each App's bot login under its own JWT (appauth.Logins), which needs no
// installation, so a daemon serving no repository yet still knows the review App's login, as one
// attempt run again after a failure GitHub reports as its own trouble (appauth.TransientError) and
// after GitHub's rate limit, waited out on the start's waits (appAttempt); any other failure is
// refused at once. An attempt after one App's login was read reads only the other's, as the key
// proof skips an App it proved (proveAppKeys): in token mode each read of an App is a run of its
// token_command, which may wait on a person's approval of a grant. It returns the review App's
// login: the engine tells the review App's reviews from every other account's by the login that
// authored them, and the reviewer's own among those by the session their Legion footer names,
// which must be the one the reviewer's claim records (workflow's byReviewer), so one App
// configured for both roles would submit every role's review under the one login, and is
// refused, as is a review App that names no login.
func appLoginsAtBoot(ctx context.Context, apps appauth.Logins, waits *startWaits, log *slog.Logger) (string, error) {
	const call = "GitHub App logins read"
	logins := map[appauth.AppRole]string{}
	err := bootprobe.RunOn(ctx, waits.bounds.clock, call, waits.bounds.retry, log, appAttempt(waits, call, func(ctx context.Context) error {
		for _, role := range appauth.Roles {
			if _, read := logins[role]; read {
				continue
			}
			login, err := apps.AppLogin(ctx, role)
			if err != nil {
				return err
			}
			logins[role] = login
		}
		return nil
	}))
	if err != nil {
		return "", err
	}
	if logins[appauth.Review] == "" || logins[appauth.Review] == logins[appauth.Implement] {
		return "", fmt.Errorf("the review App names bot login %q and the implement App %q; the workflow needs two different Apps", logins[appauth.Review], logins[appauth.Implement])
	}
	return logins[appauth.Review], nil
}

// proveAppKeys proves each role App's key with GitHub's App-level read of its installations
// (appauth.OwnerInstallations.InstalledOwners: GET /app/installations under the App's JWT, or, for
// an App on token_command, the command's release of every installation), one App after the other,
// both reads as one attempt run again after a failure GitHub reports as its own trouble
// (appauth.TransientError), with a pending command spending no attempt (appAttempt): the two Apps
// share one retry budget, so the boot spends at most the start's retry on GitHub's trouble however the
// failures fall between them, and an attempt after one App's read answered reads only the other's.
// GitHub's rate limit on either read spends no attempt either: it is waited out on the start's
// waits until the time GitHub names (appAttempt), and a read GitHub still refuses for it once
// those waits have reached the start's end stops the boot, naming the call, the time waited,
// GitHub's answer and the App (limitOutcome). Any other failure stops the boot with a refusal that
// names the App. A key GitHub takes is the whole proof: an App with no installation boots, and one
// WARN line names it, since a project works in each repository both role Apps reach and an
// operator may install an App after the daemon is up. The owners each read answers are recorded
// as the boot's read of that App's installations (appauth.Installed.RecordProof) once both are
// proved, so every launch's addressing names its App's own owners from the first launch on
// (plan.appOwners). That read is kept apart from a bundle's: the drift check never compares a pod
// against it (appauth.GitHubDirs), since no mint has run at boot to say which of those owners a
// bundle leaves out. Nothing of the boot mints a token for an owner.
func proveAppKeys(ctx context.Context, apps appauth.OwnerInstallations, installed *appauth.Installed, waits *startWaits, log *slog.Logger) error {
	const call = "GitHub App key proof"
	owners := make(map[appauth.AppRole][]string, len(appauth.Roles))
	err := bootprobe.RunOn(ctx, waits.bounds.clock, call, waits.bounds.retry, log, appAttempt(waits, call, func(ctx context.Context) error {
		for _, role := range appauth.Roles {
			if _, proved := owners[role]; proved {
				continue
			}
			read, err := apps.InstalledOwners(ctx, role)
			if err != nil {
				return fmt.Errorf("prove the %s App's key by reading its installations: %w", role, err)
			}
			owners[role] = read
		}
		return nil
	}))
	if err != nil {
		return err
	}
	for _, role := range appauth.Roles {
		installed.RecordProof(role, owners[role])
		if len(owners[role]) == 0 {
			log.Warn("workflow: a GitHub App is installed on no account, so its role reaches no repository until an account installs it",
				"app", string(role))
			continue
		}
		log.Info("legion workflow proved a GitHub App's key", "app", string(role), "installedOn", owners[role])
	}
	return nil
}

// openWorkflow opens the workflow in the order a refusal names its cause: the configuration, the
// Apps' logins, the proof of each App's key (proveAppKeys, which records the owners each App is
// installed on into installed), the records and the pool, the fill of any record written before
// the record carried its repository (backfillRepositories, which leaves a record it cannot fill
// for the refresher), then the deployment's design-gate steps queued for the project's Dispatch
// row (queueGatedStepsPublish) and the recorded gate of every issue Dispatch has not closed queued
// for its own (queueRecordedGatesPublish). The key proof under the App's JWT mints no token for an
// owner. It reads GitHub for a repository only to fill a record that lacks its repository columns,
// one read per such record bounded by the read attempt bound (repositoryReadBounds.attempt,
// backfillRead), which mints the token that
// read needs, a read that fails leaving the record for the refresher; the preparation of the
// repositories the records name is the refresher's first job, once the API serves
// (prepareAtStart), beside the boot, so no other read of GitHub for a repository here can stop or
// slow the boot. The boot's one-time rename pass, which runs after this (renameAtBoot), reads
// GitHub for a configured pair only while a stored row still names the pair's `from`.
//
// dispatchTransport presents the Dispatch credential boot settled (dispatchCredential): the bearer
// dispatch_token_file names, or the daemon's own broker token under dispatch_token_broker. Every
// Dispatch call of the workflow goes through it.
//
// hookPrompts are the words boot read for every hook whose `prompt_file` names a file, by hook
// name (readBoot): the engine is given each hook's words, never a path.
//
// pacer is the daemon's one wake pacer as the engine's catch-ups reach it (run builds it, before
// this and before supervision, whose launches wait in the same two queues): the catch-up a start
// writes in place of a launch joins its second queue (workflow's claimReady, catchUpPacing), and
// nil while the operator paces nothing, which is the engine's reading of the pacing off — the
// catch-up is then due at once, as it always was.
//
// clock is the daemon's one clock (the machines', overrides.clock), which the engine and this
// runtime's idle, wake and stuck passes read (workflowRuntime.now); production passes the wall
// clock.
//
// source is the random source every `random` experiment is drawn with (overrides.experimentSource):
// the engine holds it, and the admission draws with the engine's (workflow.Engine's
// ExperimentSource). nil is the engine's own, experiment.CryptoSource.
//
// appCalls are the start's App-call bounds (overrides.appCalls): nil, in production, is
// productionAppCalls.
func openWorkflow(ctx context.Context, cfg config.Config, st *store.Store, projectID string, dispatchTransport http.RoundTripper, hookPrompts map[string]string, repos *repositories, installed *appauth.Installed, log *slog.Logger, suppliedTokens appauth.Source, githubAPI string, pacer *wakepace.Pacer, clock func() time.Time, source func(uint64) (uint64, error), appCalls *appCallBounds) (opened *workflowRuntime, _ error) {
	if cfg.DispatchURL == "" {
		return nil, nil
	}
	// The steps below are the boot's and run one after another, so they share one hold mark for
	// the pool (store.WithTransactionTracking): the repository backfill and the two publications
	// it queues are refused a second connection taken under one of their own transactions
	// (store.ErrNestedAcquire) instead of waiting for a boot that can no longer finish.
	ctx = store.WithTransactionTracking(ctx)
	log.Info("legion workflow boot stage", "stage", "config")
	tokens := suppliedTokens
	if tokens == nil {
		// The workflow owns a manager it makes: a boot that fails before the workflow is returned
		// closes it, so a token_command still waiting on a grant does not outlive the boot.
		owned := appauth.New(cfg.GitHubApps, appauth.Options{Log: log})
		tokens = owned
		defer func() {
			if opened == nil {
				owned.Close()
			}
		}()
	}
	installs, ok := tokens.(appauth.Installations)
	if !ok {
		return nil, fmt.Errorf("the GitHub App token source answers no App installation on a repository, which the daemon reads both role Apps' installations on each repository it works with")
	}
	logins, ok := tokens.(appauth.Logins)
	if !ok {
		return nil, fmt.Errorf("the GitHub App token source answers no App login, which the workflow judges pushes and reviews by")
	}
	forget, ok := tokens.(appauth.Forgetter)
	if !ok {
		return nil, fmt.Errorf("the GitHub App token source cannot drop a token GitHub refused, which a review's reads mint again")
	}
	w := &workflowRuntime{
		tokens: tokens, apps: tokens, forget: forget, installs: installs, dispatch: dispatch.New(cfg.DispatchURL, dispatchTransport), repositories: repos,
		grants: credential.New(nil), projectID: projectID, dispatchProject: cfg.Project, stateDir: cfg.StateDir, log: log,
		failed:     make(chan error, 1),
		rereadWake: make(chan struct{}, 1), recoverWake: make(chan struct{}, 1), heardWake: make(chan struct{}, 1),
		heldWake: make(chan struct{}, 1), liveWake: make(chan struct{}, 1),
		readied: newWakeSet[claim.Token](), handedOff: newWakeSet[string](), mergeStates: newWakeSet[string](),
		mergeStateRunning: map[string]*mergeStateRun{}, baseWake: newWakeSet[string](), etags: githubrest.NewETags(etagBytes),
		controllerWake: cfg.ControllerWakeInterval, githubAPI: githubAPI, now: clock, graphQLShare: cfg.GitHubGraphQLShare, appCalls: appCalls,
		defaultBranches: newDefaultBranches(tokens, repos, githubAPI, defaultBranchTTL, time.Now),
	}
	// A read of a default branch GitHub's rate limit refuses, answered from the branch held, is
	// logged and read again once the limit has passed, wherever it is made.
	w.defaultBranches.onHold = w.heldThroughLimit
	log.Info("legion workflow boot stage", "stage", "appauth")
	// One policy and one clock for every wait of the start for GitHub's rate limit: the App logins
	// read, then the key proof (appCallBounds.rateLimits).
	waits := newStartWaits(w.callBounds())
	reviewAppLogin, err := appLoginsAtBoot(ctx, logins, waits, log)
	if err != nil {
		return nil, err
	}
	if err := proveAppKeys(ctx, tokens, installed, waits, log); err != nil {
		return nil, err
	}
	// The database is shared by every project's daemon: the workflow reads this project's issues.
	// The rows the workflow writes through these records wake the outbox's lanes (projectRecords),
	// through the runtime, since the outbox itself is built later (attach), and each write that can
	// change the live records asks the refresher to read them again at its commit (wakeLive).
	w.catchUps = &catchUpPacing{ctx: ctx, pool: st.Pool(), pacer: pacer, now: clock, wake: w.wakeOutbox, log: log}
	w.records = projectRecords{Store: record.NewStore(), project: cfg.Project, wake: w.wakeOutbox, live: w.wakeLive, log: log, catchUps: w.catchUps}
	w.pool = st.Pool()
	engineCfg := engineConfig(cfg, reviewAppLogin, hookPrompts)
	// The engine reads and writes the pod tables of the idle release through the store's own
	// view of this project (store.Store.PodRecordsOf), which derives the claim token's spelling
	// of the project the tables are keyed by.
	if engineCfg.Pods, err = st.PodRecordsOf(cfg.Project); err != nil {
		return nil, err
	}
	engineCfg.Repositories, engineCfg.NoteUnread = w, w.noteUnread
	engineCfg.ReadMergeState, engineCfg.HearRepository = w.queueMergeState, w.hearRecorded
	engineCfg.ReadBaseMergeStates = w.queueBaseMergeStates
	engineCfg.StepAgent = w.stepAgent
	// The one wake the engine causes is the catch-up a start writes in place of a launch
	// (workflow's claimReady), and it is paced with the launches it stands among, in the one
	// pacer run built for both lanes. A deployment that paces nothing gives the engine no hold at
	// all, which is the pacing off: the engine then writes that catch-up due at once, as it
	// always did.
	if pacer != nil {
		engineCfg.HoldCatchUp = w.catchUps.hold
	}
	engineCfg.MessageMarked = w.messageMarked
	// The engine stamps its deadlines on the daemon's one clock, which the machines and this
	// runtime's passes read too (dispatch://INFRA-3759).
	engineCfg.Clock = clock
	engineCfg.ExperimentSource = source
	w.engine = workflow.New(w.records, engineCfg, log)
	w.admission = admit.New(w.records, w.engine, cfg.AdmissionCap, cfg.Project, log)
	// The project's declared experiments decide what each unit admission records is drawn for.
	// A project whose file writes neither the `memory` block nor an `experiments` key declares
	// the memory experiment all the same, at mode off, whose draw records each new tree at no
	// variant (config.Project.ExperimentsDeclared).
	w.admission.UseExperiments(cfg.ExperimentsDeclared())
	// `stalled_tree_notice_hours` is the bound the admission pass tells the controller a tree
	// has stopped at: the loader gives six hours to a file that names no key, so the pass
	// always runs under a number somebody chose.
	w.admission.UseStalledTreeNotice(cfg.StalledTreeNotice)
	// The bounds `worker_silence_minutes` and `worker_background_hours` set are what the tick's
	// silent list is read against (admit's UseWorkerSilence, config.Config.WorkerSilence and
	// config.Config.WorkerBackground).
	w.admission.UseWorkerSilence(cfg.WorkerSilence, cfg.WorkerBackground)
	// The mirror runs before both deciders, and that order is load-bearing. The engine decides
	// whether to start a child and admission whether to admit a root inside this same
	// transaction, from the mirror's rows, so the row an event carries must already be written
	// when they look. Neither could write it itself: the engine returns early on an event whose
	// sequence or status it has already seen (Engine.dispatchIssue), which is exactly the
	// blocked-by-only edit this mirror exists for, and admission runs second in any case.
	w.handlers, w.reviewAppLogin = []intake.Handler{blockers.NewMirror(w.records, cfg.Project), w.engine, w.admission}, reviewAppLogin
	w.gatedSteps = cfg.Gates.Steps()
	left, err := backfillRepositories(ctx, w.pool, w.records, w.dispatch, cfg.Project, cfg.StateDir, w.defaultBranches, cfg.RepositoryRenames, w.reads(), log)
	if err != nil {
		return nil, err
	}
	w.backfillPending.Store(left)
	if err := w.queueGatedStepsPublish(ctx); err != nil {
		return nil, err
	}
	if err := w.queueRecordedGatesPublish(ctx); err != nil {
		return nil, err
	}
	if err := w.queueRoleHoldersPublish(ctx); err != nil {
		return nil, err
	}
	return w, nil
}

// queueGatedStepsPublish queues the deployment's design-gate steps for the project's Dispatch
// row. The boot queues it rather than writing it, so a Dispatch that is down or has not got the
// route refuses no boot; the outbox's first sweep after attach runs the row.
//
// It queues at every boot, because the daemon stores no list it last published: the route writes
// only a difference (`gated_steps is distinct from`), so a boot on an unchanged list touches no
// row and appends no event. A row of the kind still in the outbox is replaced rather than left
// standing (EnqueueGatedSteps), so the queued row always carries this boot's list: the row can
// have waited there since a boot on a list the operator has changed since, and a restart loop
// still queues no second row.
func (w *workflowRuntime) queueGatedStepsPublish(ctx context.Context) error {
	publish := record.GatedStepsPublish{Project: w.dispatchProject, Steps: w.gatedSteps}
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		return w.records.EnqueueGatedSteps(ctx, tx, publish, time.Now().UTC())
	}); err != nil {
		return fmt.Errorf("queue the gated-steps publish of %s: %w", w.dispatchProject, err)
	}
	return nil
}

// queueRecordedGatesPublish queues one design-gates publication for each recorded issue that
// holds a gate and that Dispatch does not show as done. Every gate of this deployment was written
// before the kind existed, so nothing has published any of them: without this pass Dispatch would
// learn an issue's documents only once something moved that set again, which for a tree already
// waiting on its approval is never. A boot that cannot write the rows fails the boot, as the
// project's gated-steps publication does.
//
// A done issue is left out, by the read itself (record.GatedIssues), and that is a publish-cost
// filter and not the liveness rule: Dispatch decides liveness at the read, where a gate counts
// only while its issue is in the workflow there, which leaves out `triage`, `icebox` and
// `backlog` as well (api.supersededApprovalGates, model.IssueStatusesOutOfWorkflow). A closed
// issue is the one of those four that never comes back, so its set is the one nobody need
// publish; a parked issue's is published, and its tree answers for it the moment a person brings
// it back. The status read is dispatch_status, what Dispatch last showed, rather than status,
// which is what the daemon means by the issue.
//
// It queues at every boot, as the project's list does, because the daemon keeps no record of what
// it last published, so each boot does cost Dispatch one write for every live gated issue: the
// enqueue's payload comparison (EnqueueDesignGates) matches only a row still waiting in the
// outbox, and a row the lane has delivered is gone. What it buys is a restart loop, where the
// rows of the boot before are still pending and a second boot queues no row beside them. A boot
// costs the record two queries — the issues to publish, then their gates — and one enqueue each.
// The lane is woken on commit (projectRecords), which before attach reaches nothing: the outbox's
// first sweep runs whatever this pass left.
//
// Each set says which of its rows hold their tree under this boot's steps (record.GateDocuments),
// so a boot on a `gates.design` the operator has changed since corrects what Dispatch holds, and
// a row still pending from an older daemon, whose set says nothing of it, takes this boot's set
// in its place (the enqueue writes a payload that differs).
func (w *workflowRuntime) queueRecordedGatesPublish(ctx context.Context) error {
	now := time.Now().UTC()
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		live, err := w.records.GatedIssues(ctx, tx, w.dispatchProject)
		if err != nil {
			return err
		}
		gates, err := w.records.Gates(ctx, tx, live)
		if err != nil {
			return err
		}
		for _, issue := range live {
			publish := record.DesignGatesPublish{Issue: issue, Gates: record.GateDocuments(w.gatedSteps, gates[issue])}
			if err := w.records.EnqueueDesignGates(ctx, tx, publish, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("queue the recorded design-gates publications of %s: %w", w.dispatchProject, err)
	}
	return nil
}

// roleHoldersPublishWindow is how far back the boot looks for a claim change that a release
// before this one made and did not publish (queueRoleHoldersPublish): a claim whose last write is
// older and that is not live is not read. No plan gives the number a reason beyond bounding the
// boot's read. Each issue inside it costs the boot one outbox row and its lane one PUT, so a
// longer window costs a boot one more of each for every issue whose claims last changed inside the
// longer span.
const roleHoldersPublishWindow = 7 * 24 * time.Hour

// queueRoleHoldersPublish queues one role-holder publication for each issue of the project that has
// a live claim, or a claim that changed in the last roleHoldersPublishWindow (RecentClaimIssues),
// in one transaction. Every claim write of this release that changes a holder queues its own
// publication in its own transaction (store's putClaim), so this pass recovers no change of this
// release. It publishes what a release that published nothing left: a daemon that upgrades from
// such a release starts over running sessions that Dispatch has never been told of, and a roll
// forward after a rollback to one finds the claims that release changed. A claim that release
// ended longer than roleHoldersPublishWindow before the roll forward stays listed on Dispatch
// until the next change of a claim on its issue. A boot that cannot write the rows fails the boot,
// as the design-gates pass does, and queueing rather than writing means a Dispatch that is down
// refuses no boot. The lane is woken on commit (enqueueRoleHolders), which before attach reaches
// nothing: the outbox's first sweep runs whatever this pass left.
func (w *workflowRuntime) queueRoleHoldersPublish(ctx context.Context) error {
	now := time.Now().UTC()
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		issues, err := w.records.RecentClaimIssues(ctx, tx, w.dispatchProject, now.Add(-roleHoldersPublishWindow))
		if err != nil {
			return err
		}
		for _, issue := range issues {
			if err := w.enqueueRoleHolders(ctx, tx, issue, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("queue the role-holder publications of %s: %w", w.dispatchProject, err)
	}
	return nil
}

// roleHoldersChanged queues the publication of c's issue's role sessions for the ready route
// (api's Options.RoleHoldersChanged), before the claim takes a boot's or a role-regain's ready, in
// a transaction of its own, which owns the commit hook its write registers, so the board lane is
// woken once it commits; it queues none for an issue of another project (enqueueRoleHolders).
// An error is the route's to answer: it refuses the ready with 503, so no task goes first.
func (w *workflowRuntime) roleHoldersChanged(ctx context.Context, c supervise.Claim) error {
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		return w.enqueueRoleHolders(ctx, tx, c.Issue, time.Now().UTC())
	}); err != nil {
		return fmt.Errorf("queue the role-holder publication of %s: %w", c.Issue, err)
	}
	return nil
}

// enqueueRoleHolders queues the publication of issue's role sessions in tx
// (store.EnqueueRoleHolders), and wakes the issue's board lane once tx commits, through the commit
// scope of ctx (store.Transact), which both callers open. An issue of another project
// queues nothing and wakes nothing, since only that project's daemon runs a row keyed by its issue
// (claim.KeyOfProject). That is an ordinary path, not a fault: every ready of the operator's spawn
// of such an issue takes it, so it is passed over here, before the store, and logs nothing. The
// store's own refusal of such a key stands behind it for every other caller.
func (w *workflowRuntime) enqueueRoleHolders(ctx context.Context, tx pgx.Tx, issue string, nextAt time.Time) error {
	if !claim.KeyOfProject(w.dispatchProject, issue) {
		return nil
	}
	if _, err := store.EnqueueRoleHolders(ctx, tx, w.dispatchProject, issue, nextAt); err != nil {
		return err
	}
	store.OnCommit(ctx, func() { w.wakeRoleHolders(issue) })
	return nil
}

// wakeOutbox is the records' wake (projectRecords.wake): it reaches the outbox attach stored, and
// nothing before attach.
func (w *workflowRuntime) wakeOutbox(issue string, kind record.OutboxKind) {
	if r := w.outboxWake.Load(); r != nil {
		r.wake(issue, kind)
	}
}

// wakeRoleHolders wakes the board lane of issue, which runs the issue's role-holder publication:
// the wake the store calls once a claim write that queued the publication commits
// (store.Store.PublishRoleHolders), and the one the boot pass and the ready route register.
func (w *workflowRuntime) wakeRoleHolders(issue string) {
	w.wakeOutbox(issue, record.OutboxKindRoleHoldersPublish)
}

// stepAgent is the engine's read of the agent of a step where it writes the step's end, the one
// write of the record that names a session or a model (workflow.Config.StepAgent): the session the
// claim's machine published (supervise.Machine.View) and the model that answered last in that
// session (supervise.Machine.LastAnswer). It reaches the supervisor through the outbox attach
// stored, so a fact the boot applies before attach reads nothing, as does a claim this daemon does
// not supervise. It finds the machine without the supervisor's lock (supervisedMachine), since the
// engine reads here inside a fact's transaction, which holds the fact lock that a Create holding
// that lock waits on. Neither read of the machine waits on a decision in flight, so the engine's
// transaction may make them. The session is read on both sides of the model, and a model read while
// the session changed is left out, so the pair never names a model with a session it did not serve;
// the end is the record's one write of either, so that record then names no model.
//
// Both come from the agent's own frames, so each is held here to the form Dispatch holds a step's
// agent text to (steprecord.AgentTextHolds), and one outside it is left out of the record rather
// than sent: Dispatch would refuse the whole record over it, and a space or a newline in it would
// shift the columns of `dispatch steps` or forge a line of it.
//
// The session is named only while the claim holds it (plan E's t-e-54): a claim the boot could not
// suspend can record another claim's session, or one no claim holds, and its step's record would
// show that session as the one at work, as the role-holder publication would (LiveRoleHolders,
// which leaves it out). The check only reads. It asks the machine first (Machine.Admits: no lock,
// no store call), and where the machine has not seen the answer, as after a restart, it reads
// claim_sessions in the fact's own transaction (record.SessionHeld), one select under a
// savepoint on the connection that already holds the fact lock: no store write, and no wait on
// another connection while that lock is held, where the store's hold (HoldSessions) would write
// through the pool. A read that fails names no session, and the record's end then names none.
func (w *workflowRuntime) stepAgent(ctx context.Context, tx pgx.Tx, token claim.Token) workflow.StepAgent {
	r := w.outboxWake.Load()
	if r == nil || r.supervisor == nil {
		return workflow.StepAgent{}
	}
	machine, ok := r.supervisor.supervisedMachine(token)
	if !ok {
		return workflow.StepAgent{}
	}
	view := machine.View()
	session := view.Session
	model := machine.LastAnswer().Model
	if machine.View().Session != session {
		model = ""
	}
	if session != "" && !machine.Admits(session, view.SessionFile) && !w.sessionHeld(ctx, tx, view.Claim) {
		session = ""
	}
	return workflow.StepAgent{Session: w.agentText(token, "session", session), Model: w.agentText(token, "model", model)}
}

// sessionHeld is stepAgent's read of whether c holds the session it records, in the fact's
// transaction tx (record.SessionHeld); false for no transaction and for a read that failed, which
// is logged.
func (w *workflowRuntime) sessionHeld(ctx context.Context, tx pgx.Tx, c supervise.Claim) bool {
	if tx == nil {
		return false
	}
	held, err := record.SessionHeld(ctx, tx, c.Project, c.Token, c.Session, c.SessionFile)
	if err != nil {
		w.log.Warn("legion step record names no session: the read of whether its claim holds it failed", "claim", string(c.Token), "error", err)
	}
	return held
}

// agentText is text when it is in the form of a step's agent text (steprecord.AgentTextHolds), and
// "" when it is not, logged with its length and its first 256 bytes, quoted, so an operator can see
// what the record leaves out without the log carrying text of any length. The text comes from the
// agent's frames, so redact.Bytes reads it whole through the URL rule before the cut: a cut made
// first could end inside a URL's userinfo, before its "@", where the log's rule no longer finds it.
func (w *workflowRuntime) agentText(token claim.Token, field, text string) string {
	if text == "" || steprecord.AgentTextHolds(text) {
		return text
	}
	quoted, _ := redact.Bytes(text, 256)
	w.log.Warn("legion step record leaves out an agent's text outside its form", "claim", string(token), "field", field,
		"bytes", len(text), "text", strconv.Quote(quoted))
	return ""
}

// engineConfig is the workflow engine's configuration from the daemon's and the review App's bot
// login from its boot lease: its own project's production_check is the role that works the checks
// a merge opens (empty, it opens none), its own project's default steps are what an issue whose
// owner gives no plan runs, and its own project's declared experiments are what each issue the
// engine records is drawn for. hookPrompts are the words boot read for the hooks that name a file
// (readBoot), which engineHooks puts in the place of each such hook's path.
func engineConfig(cfg config.Config, reviewAppLogin string, hookPrompts map[string]string) workflow.Config {
	project := cfg.Projects[cfg.Project]
	return workflow.Config{
		Project: cfg.Project, Steps: cfg.Gates.Steps(), ReviewRoundCap: cfg.ReviewRoundCap,
		Linger: cfg.Linger, ProductionCheckBound: cfg.ProductionCheckBound,
		ProductionCheckRole: project.ProductionCheck,
		DefaultSteps:        project.RunsByDefault(),
		ReviewAppLogin:      reviewAppLogin,
		RepositoryRenames:   cfg.RepositoryRenames, Hooks: engineHooks(cfg.Hooks, hookPrompts),
		Experiments:  cfg.ExperimentsDeclared(),
		WorkspaceDir: engineWorkspaceDir(cfg),
	}
}

// engineWorkspaceDir is the directory the runtime cfg names starts an issue's role processes in,
// which every task the engine composes names (workflow.Config.WorkspaceDir): each runtime's own
// function that places the workspace, sandbox.WorkspaceDir under Kubernetes and tmux.WorkspaceDir
// under the state directory under tmux, read for the repository the issue's record names, as the
// launch reads it (issueRepository). An issue whose recorded repository does not parse, which no
// launch can work either, names none, and its task names the branch alone.
func engineWorkspaceDir(cfg config.Config) func(record.Issue) string {
	place := func(repo ghrepo.Repository, issue string) (string, error) {
		return tmux.WorkspaceDir(cfg.StateDir, repo, issue)
	}
	if cfg.Runtime.Name == "kubernetes" {
		place = sandbox.WorkspaceDir
	}
	return func(issue record.Issue) string {
		repo, err := issue.Repository()
		if err != nil {
			return ""
		}
		dir, err := place(repo, issue.Key)
		if err != nil {
			return ""
		}
		return dir
	}
}

// engineHooks is the deployment's hooks as the engine runs them (workflow.Hook): each hook's own
// words, from the key where it wrote them and from prompts where it named a file instead, so the
// engine reads no file and holds no path. A deployment that configures no hook gets none.
func engineHooks(hooks []config.Hook, prompts map[string]string) []workflow.Hook {
	if len(hooks) == 0 {
		return nil
	}
	running := make([]workflow.Hook, 0, len(hooks))
	for _, hook := range hooks {
		words := hook.Prompt
		if hook.PromptFile != "" {
			words = prompts[hook.Name]
		}
		running = append(running, workflow.Hook{
			Name: hook.Name, On: hook.On, Moves: hook.Moves, Every: hook.Every,
			Roles: hook.Roles, Prompt: words,
		})
	}
	return running
}

// connect opens Envoy's JetStream and this project's durable consumers, so a missing
// notification stream refuses boot rather than leaving a daemon that reads no events. Boot runs it
// before reconcile, whose Dispatch listing covers only what precedes a consumer created now. It
// connects as nc, the user readBoot chose (natsConnection), and logs what the server reports about
// the connection: every permission it refuses at error, every disconnect at warn and every
// reconnect at info (natsauth.LogEvents), under natsauth.ReconnectForever so a reconnect never
// gives up: a NATS outage mid-run never closes the connection for good. A failed attempt here
// closes whatever it opened and clears it, so a later call — this one retried after an unreachable
// NATS — starts clean rather than leaking the connection this one could not finish setting up.
// Close comes before natsauth.WithLastError on both failure paths, never after: WithLastError only
// looks once the connection reports itself closed, and Close does not clear what the connection
// last recorded.
func (w *workflowRuntime) connect(ctx context.Context, cfg config.Config, nc natsConnection) error {
	conn, err := natsauth.Connect(cfg.NatsURLs, nc.seed, natsauth.ConnectionName("legion-daemon "+cfg.Project, cfg.Revision), natsauth.LogEvents(w.log), natsauth.ReconnectForever())
	if err != nil {
		return fmt.Errorf("connect Envoy NATS: %w", err)
	}
	w.conn.Store(conn)
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		w.conn.Store(nil)
		return fmt.Errorf("open Envoy JetStream: %w", natsauth.WithLastError(err, conn))
	}
	w.log.Info("legion workflow boot stage", "stage", "intake")
	logIntakeHandlerLimit(ctx, w.log, cfg)
	heard, err := w.heardAtBoot(ctx)
	if err != nil {
		conn.Close()
		w.conn.Store(nil)
		return err
	}
	// servesNone is whether the live records the boot just read (heardAtBoot) name no repository,
	// as a fresh install's do: the daemon then serves none until an issue names one, and says so
	// once, at info, for an operator to read (dispatch://INFRA-4401 A2). It says it once the
	// consumers are open, so an attempt that fails after the read, whose next attempt reads the
	// records again, says nothing.
	servesNone := len(w.repositories.liveNames()) == 0
	// The give-up bound takes intake's production values (ConsumerSpec's defaults), and a give-up's
	// notice wakes its outbox lane through the records' own lane wake once it commits.
	w.consumers, err = intake.OpenConsumers(ctx, js, w.pool, intake.ConsumerSpec{
		Project: cfg.Project, Repositories: heard, Renames: cfg.RepositoryRenames,
		AckWait: cfg.WorkerRPCTimeout, NakDelay: time.Second, Logger: w.log,
		ReviewPermission: w.reviewerCanWrite, ReviewBody: w.reviewBody,
		PrepareRepository: w.prepareRepository, PrepareArtifact: w.prepareArtifact,
		WakeOutbox: w.wakeOutbox,
	})
	if err != nil {
		conn.Close()
		w.conn.Store(nil)
		return natsauth.WithLastError(err, conn)
	}
	if servesNone {
		w.log.Info("workflow: no live record of the project names a repository, so the daemon serves no repository until an issue names one",
			"project", cfg.Project)
	}
	w.bootID = fmt.Sprintf("%d", time.Now().UnixNano())
	return nil
}

// phaseHolds answers supervise's Deps.PhaseHolds from the issue record: a task carries the phase
// and the run (generation) it was queued for, so once the issue is in another phase, or on another
// run, a delivery still queued for that one is finished work and is dropped rather than sent. The
// phase is compared, not the role that works it: one role runs several phases — the implementer
// runs implementing and retro, and the role the project's `production_check` named at the open
// (record.ProductionCheck.Role) runs each look of the check — and a task written for the first is
// not the work of the third. The run is compared because a re-admission can restart the issue in the phase
// it stood in: an operator suspension keeps a claim's queued task (supervise's settle), so without
// it the operator's resume would send the run that is over's task into the new run, ahead of the
// new run's own. A task of no run (generation 0) is held to its phase alone.
//
// A delivery of no phase holds here. Only the workflow names one, so an operator's delivery made
// by hand through the API is not dropped for the phase its issue is in: the operator asked for
// it, was answered 200, and dropping it would make the work silently not happen. An architect's
// task belongs to no phase and names none for the same reason. This predicate is not the only
// way a delivery ends: a suspension retires an unconfirmed one that names a phase, the phase it
// is ending (supervise's settle).
//
// Nothing else on the delivery would answer this: the id is rotated by a prompt retry, and the
// task text is the workflow's prose. The phase is typed data, persisted with the delivery, and no
// path rewrites it.
//
// A claim on an issue the daemon does not record holds too. The workflow never deletes an issue,
// so there is no record only for a claim the workflow never made — the operator's own spawn, whose
// issue key need not be an issue key at all. The comparison itself is record.PhaseHolds, which a
// test that wires this answer without a daemon calls too.
func (w *workflowRuntime) phaseHolds(ctx context.Context, key string, queuedFor phase.Phase, generation uint64) (bool, error) {
	issue, err := w.recordedIssue(ctx, key)
	if err != nil {
		return false, fmt.Errorf("read %s for the phase of its task: %w", key, err)
	}
	return record.PhaseHolds(issue, queuedFor, generation), nil
}

// phaseRole answers supervise's Deps.PhaseRole from the issue record: the worker of key's open
// phase (workflow.OpenPhaseRole), which the pod start gate puts first among the starts no request
// raised — deaths, a roles container's restart, a pod an update round replaced
// (dispatch://INFRA-5297/spec R3). An issue in a phase the architect works (admitted, accepting)
// has no phase worker, so the order falls back to claim.Roles there, and so does an issue the
// daemon does not record, which has no phase: the answer is "" for both, as it is for a phase no
// role works.
func (w *workflowRuntime) phaseRole(ctx context.Context, key string) (claim.Role, error) {
	issue, err := w.recordedIssue(ctx, key)
	if err != nil {
		return "", fmt.Errorf("read %s for the role of its phase: %w", key, err)
	}
	if issue == nil {
		return "", nil
	}
	return workflow.OpenPhaseRole(issue.Phase), nil
}

// rootHears answers supervise's Deps.RootHears from the issue record: a root architect's task of a
// run names no phase, so no phase ever drops it, and it waits on the claim until that claim next
// runs — which a park, a hand back to `todo` or a linger can put generations later. The decisions
// such a task names are held by its delivery and told when the turn that takes it starts
// (store's holdKeptDecisions, tellKeptDecisions), so sending it while the tree has nobody to act on
// it would spend a person's decision on an architect no run of the root owns. The answer is the
// workflow's one reading of that question
// (workflow.ArchitectHears), which the hand-over of a released decision reads too, in a short
// read-only transaction of its own: a root that hears is one that is its tree's root, runs, does
// not linger and does not wait in `todo` for its slot.
//
// A claim on an issue the daemon does not record hears, as it holds for the phase: there is no
// record only for a claim the workflow never made.
func (w *workflowRuntime) rootHears(ctx context.Context, c supervise.Claim, _ supervise.Delivery) (bool, error) {
	var hears bool
	if err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		root, err := w.records.Issue(ctx, tx, c.Tree)
		if err != nil || root == nil {
			hears = err == nil
			return err
		}
		hears, err = workflow.ArchitectHears(ctx, tx, w.records, *root, w.gatedSteps)
		return err
	}); err != nil {
		return false, fmt.Errorf("read whether the root of %s has an architect to hear its task: %w", c.Tree, err)
	}
	return hears, nil
}

// rootTask answers supervise's Deps.RootTask: what a root architect's held task of a run says now,
// at the moment it is about to be sent. Such a task names the decisions a person made while nobody
// in the root's tree could hear them, and holds those rows on its own delivery until the turn that
// takes it confirms (the claim store's putDelivery, 20261101163106_parked_decisions). An issue it names can
// have left this tree while the task waited — a child a linger handed to Legion, a parked child a
// person's todo orphaned, a grandchild handed over beside its parent — and the tree it stands in
// now is the one that owes the person an answer, not this one.
//
// So this is where such a decision is dropped from the task, in one write: the rows whose issue
// has left the claim's tree lose this delivery's hold (record.Store.ReleaseHeldDecisions), which
// leaves each KEPT for its own tree to name, and the architect of each tree they are now owed to
// is written a fresh catch-up naming them at once, exactly as a hand-over writes one
// (workflow.CatchUpTheTreesOwed). The decisions whose issue the record still shows in this tree
// stay, in the order the task named them, and the text is written again from them
// (workflow.KeptDecisionsTask), which is the wording the start that queued the task used. An issue
// the record does not hold is kept rather than dropped: a person's decision is never lost, and
// this answer drops one only where the record says another tree is owed it. A task left naming
// nothing is retired rather than sent, and its delivery's write releases whatever it still held.
//
// A task that also carries news of the approved design (supervise.Delivery.News: since #3711 a
// root architect's entry start carries the kept decisions and the news it is owed) is narrowed in
// the start's own text, the one the news follows (record.DeliveryNews.Task), and the narrowed text
// and the news are rendered again by the one render every task of news goes through
// (narrowedNews, designNews, renderNews). So the news it carries is exactly what its text names,
// and no later send renders the text the start wrote, which still names the released decision.
//
// It is the machine that asks, because the machine owns the pending delivery: the hand-over that
// moved the issue commits in a transaction of its own, with no lock on this claim, and a task
// rewritten there would be overwritten by the machine's next write of what memory still held. The
// write here serializes with every fact (intake.SerializeFacts), since it writes the catch-up a
// ready would write, and takes no lock of the daemon's: the machine asks it holding its own.
func (w *workflowRuntime) rootTask(ctx context.Context, c supervise.Claim, d supervise.Delivery) (string, []byte, []byte, error) {
	if len(d.Kept) == 0 {
		return d.Task, d.Kept, d.News, nil
	}
	var named record.KeptDecisions
	if err := json.Unmarshal(d.Kept, &named); err != nil {
		return "", nil, nil, fmt.Errorf("decode what the task held for %s names: %w", c.Token, err)
	}
	stays := record.KeptDecisions{KeptApproval: named.KeptApproval}
	// A pair a task of an earlier release kept of its `decisions` step names a document no step of
	// this release records, so the rewrite names no approval, as the record's read of the pair
	// leaves it (record.Store.UnclaimedKept).
	// kept for one release: design-gate-rows
	if stays.Step != "" && !stays.Step.Valid() {
		stays.KeptApproval = record.KeptApproval{}
	}
	// The record the rewrite reads where the task is of an entry and so carries the first-task
	// instruction (workflow.NarrowRootTask): the tree's root as the record holds it at the
	// narrowing, and what its first task is derived from, read by the reader every start of a
	// root architect reads it through (workflow.ArchitectStartOf). The reads are made only where
	// a decision was dropped, since nothing else rewrites the task.
	var root *record.Issue
	var start workflow.ArchitectStart
	var left []record.DecisionMark
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return fmt.Errorf("serialize the narrowing with every fact: %w", err)
		}
		holds, err := w.records.HeldKeptRefs(ctx, tx, c.Token, d.ID)
		if err != nil {
			return err
		}
		stays.Decisions, left, root = nil, nil, nil
		for _, mark := range named.Decisions {
			if !slices.Contains(holds, mark.Ref) {
				// The row this mark named is not this delivery's any more, and only these take a
				// row a delivery holds: a newer decision on the same reference replaced it, an
				// earlier send released it to the tree its issue moved to, or that tree's start or
				// catch-up claimed or told it once its issue left (this delivery's hold is void
				// there). Either way something else answers the person, and this task names it no
				// longer. What keeps the claim store's put-back (putBackARetakenConfirmation) to
				// its one case is that write's own reading — it puts a row back only where this
				// claim's confirmation told it and the machine is taking that confirmation back —
				// and not this narrowing, which runs at a send and so never at all for a task the
				// root cannot hear yet.
				continue
			}
			issue, err := w.records.Issue(ctx, tx, mark.Issue)
			if err != nil {
				return err
			}
			if issue != nil && issue.Tree != c.Tree {
				left = append(left, mark)
				continue
			}
			stays.Decisions = append(stays.Decisions, mark)
		}
		if len(stays.Decisions) != len(named.Decisions) {
			if root, err = w.records.Issue(ctx, tx, c.Tree); err != nil {
				return err
			}
			if root != nil {
				if start, err = workflow.ArchitectStartOf(ctx, tx, w.records, w.gatedSteps, *root); err != nil {
					return err
				}
			}
		}
		if len(left) == 0 {
			return nil
		}
		trees, err := w.records.ReleaseHeldDecisions(ctx, tx, c.Token, d.ID, left)
		if err != nil {
			return err
		}
		return workflow.CatchUpTheTreesOwed(ctx, tx, w.records, w.gatedSteps, trees, time.Now().UTC())
	}); err != nil {
		return "", nil, nil, fmt.Errorf("read what the task held for %s still names of %s's tree: %w", c.Token, c.Tree, err)
	}
	if len(stays.Decisions) == len(named.Decisions) {
		return d.Task, d.Kept, d.News, nil
	}
	// A task that carries the root's own first task keeps it: a decision leaving this tree says
	// nothing about whether the tree has been taken up, and a rewrite from the decisions alone
	// would hand a fresh architect the empty task that instruction exists to replace. Which task
	// carries it is the entry the delivery is of, never the task's text (workflow.NarrowRootTask
	// says why). A root the record now shows past admitted under its own power is served no first
	// task, so its rewrite is the decisions' own; with no root recorded there is nothing to write
	// one from either, so the rewrite is the decisions' own there too, as it always was.
	var task string
	if root != nil {
		task = workflow.NarrowRootTask(start, d.Entry, stays)
	} else {
		task = workflow.KeptDecisionsTask(c.Tree, stays)
	}
	if task == "" {
		return "", nil, nil, nil
	}
	// A task that names no decision now holds none: the rows it named were released above, and a
	// kept record naming nothing would have this delivery hold what it no longer says.
	var kept []byte
	if len(stays.Decisions) > 0 || stays.Approved != 0 {
		var err error
		if kept, err = keptJSON(&stays); err != nil {
			return "", nil, nil, fmt.Errorf("encode what the task held for %s still names: %w", c.Token, err)
		}
	}
	task, news, err := w.narrowedNews(ctx, c, d, task)
	if err != nil {
		return "", nil, nil, err
	}
	return task, kept, news, nil
}

// narrowedNews is the narrowed text of d, a root architect's held task (rootTask), with the news
// d carries rendered after it: the news's own text (record.DeliveryNews.Task) becomes task, and the
// one render every task of news goes through (designNews) renders the two again, so the news it
// answers is exactly what the text it answers names. A task that carries no news is task alone.
func (w *workflowRuntime) narrowedNews(ctx context.Context, c supervise.Claim, d supervise.Delivery, task string) (string, []byte, error) {
	if len(d.News) == 0 {
		return task, nil, nil
	}
	var carried record.DeliveryNews
	if err := json.Unmarshal(d.News, &carried); err != nil {
		return "", nil, fmt.Errorf("decode the news the task held for %s carries: %w", c.Token, err)
	}
	carried.Task = task
	narrowed := d
	var err error
	if narrowed.News, err = json.Marshal(carried); err != nil {
		return "", nil, fmt.Errorf("encode the news the narrowed task held for %s carries: %w", c.Token, err)
	}
	narrowed.Task = task
	return w.designNews(ctx, c, narrowed)
}

// treeClosable answers supervise's Deps.TreeClosable: a tree a workflow issue backs closes when
// its linger expires, never on an operator's close of its root claim. The machine asks it where
// the close is decided rather than a round trip before it, for the operator's own close and for a
// refused stop of the tree's root, whose refusal then names that close. The workflow's linger
// close is never asked: it holds that same issue record, so asking would refuse exactly the
// closes the workflow is entitled to make. It is not a lock on what it reads: admission and the
// engine commit issue records in their own transactions and take no machine lock, so one can
// still land between this answer and the retire.
func (w *workflowRuntime) treeClosable(ctx context.Context, c supervise.Claim) (bool, error) {
	issue, err := w.recordedIssue(ctx, c.Tree)
	if err != nil {
		return false, fmt.Errorf("read whether a workflow issue backs tree %s: %w", c.Tree, err)
	}
	return issue == nil, nil
}

// recordedIssue reads one issue in a transaction of its own, which is what both supervisor
// predicates need: the machine asks them while holding its own lock, and neither answer may take
// a lock of the daemon's.
func (w *workflowRuntime) recordedIssue(ctx context.Context, key string) (*record.Issue, error) {
	var issue *record.Issue
	err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, err = w.records.Issue(ctx, tx, key)
		return err
	})
	return issue, err
}

// reconcile takes Dispatch's bounded boot read to admission. Only admission acts on a snapshot:
// a move a human made while the daemon was down carries its own event, which the durable stream
// consumer still holds and delivers with the actor that made it, so nothing here re-derives one.
// The listing is read first, then the notification stream's own current position (target), then
// the Dispatch consumer's own: a message published between the listing and target would land in
// the listing but go uncounted by target, and reading the consumer's position last catches it up
// as far as this call can before deciding what a record behind target must still wait for.
//
// The listing reads every status, not only the workflow's own (todo through retro): Dispatch
// already returns its complete project list in one call (HTTPClient.ListIssues filters it
// client-side, at no extra request cost), and a key currently out of that window — moved to
// backlog, or never past triage — while the daemon was down still needs to be held exactly like
// one still in it, so a replayed event that predates the move it fell out on cannot be admitted
// before the move's own event ever arrives.
func (w *workflowRuntime) reconcile(ctx context.Context) error {
	issues, err := w.dispatch.ListIssues(ctx, w.dispatchProject, nil)
	if err != nil {
		return fmt.Errorf("list Dispatch issues for admission: %w", err)
	}
	refused, err := w.reconcileListing(ctx, issues, nil, true)
	if err != nil {
		return err
	}
	// The boot's listing is read before the catch-up, the API and the consumers apply any fact, so
	// no applied fact should be newer than it and it should refuse no summary (reconcileListing).
	// A summary it did refuse leaves its issue unrecorded until that issue's next event or the next
	// boot, so each one is said at Info, naming the issue.
	if len(refused) != 0 {
		w.log.Info("workflow: the boot's listing was older than an issue fact the daemon had applied for these issues, so it recorded nothing of them; each waits for its next event or the next boot",
			"issues", refused)
	}
	return nil
}

// reconcileListing applies issues, a Dispatch listing of the project, as reconcile says: the
// boot's whole listing (keep nil, boot set, which also holds the roots the boot's catch-up reads
// out of admission's decision and refills the project's rows of the copy of Dispatch's issues
// where its mark asks for it), or a recovery's (recoverRepositories), scoped to the summaries
// keep keeps, judged against every record, while every summary of the listing is still the walk
// each chain is read up. Once it commits it answers the keys admission found older than an issue
// fact of theirs the daemon applied and so recorded nothing from (admit's Reconcile), for a
// recovery to keep in the scope of its next pass. The boot's listing finds none: it is read
// after every fact the daemon applied before it starts, and before the consumers apply any.
func (w *workflowRuntime) reconcileListing(ctx context.Context, issues []dispatch.IssueSummary, keep func(records map[string]*record.Issue, summary dispatch.IssueSummary) bool, boot bool) ([]string, error) {
	// The transaction performs no I/O: what admission reads of a todo issue's repository, the
	// Dispatch chain above its parent, and who set the label of a labelled triage root the workflow
	// does not record, are read now, up one walk over the whole listing (listingAncestry); each
	// listed issue's fact carries that chain, or that setter, to admission.
	var records map[string]*record.Issue
	var walk ancestry
	var err error
	if keep != nil {
		if records, walk, err = w.listingAncestry(ctx, issues); err != nil {
			return nil, err
		}
	}
	scope := make([]dispatch.IssueSummary, 0, len(issues))
	listed := make([]intake.DispatchIssue, 0, len(issues))
	for _, summary := range issues {
		if keep != nil && !keep(records, summary) {
			continue
		}
		// Every kept summary is in scope, an issue left out of admission's decision below too:
		// scope feeds the copy of Dispatch's issues, the claims the boot releases, and the boot
		// refill's check of which keys the listing holds.
		scope = append(scope, summary)
		fact := intake.ListedIssue(summary)
		labelledTriageRoot := fact.LabelledTriageRoot()
		if fact.Status == "todo" || labelledTriageRoot {
			if walk == nil {
				if records, walk, err = w.listingAncestry(ctx, issues); err != nil {
					return nil, err
				}
			}
		}
		switch {
		case fact.Status == "todo":
			prepared, err := w.prepareListed(ctx, records, walk, fact, boot)
			if err != nil {
				return nil, fmt.Errorf("prepare the repository of %s for admission: %w", summary.Key, err)
			}
			// What the preparation read: the chain it walked, and the refusal GitHub answered for
			// the repository the issue resolves to, which this listing's own apply says on the issue
			// (intake.DispatchIssue.RepositoryRefusal).
			fact.Ancestry, fact.RepositoryRefusal = prepared.Ancestry, prepared.RepositoryRefusal
		case labelledTriageRoot && records[summary.Key] == nil:
			// The listing carries no actor, so the setter is read from the log as an event's is;
			// a recorded root a person moved back to triage is never queued again, so its log is
			// not read. A log the daemon cannot read leaves the issue out of admission's decision,
			// so one such issue does not stop the listing: no record, no queue, no handover. That
			// is a log the daemon read and cannot decode, or a page of the log the Dispatch client
			// cannot read (an event whose seq does not decode, or a seq that does not ascend), each
			// a *dispatch.EventLogError. The log is read from sequence 0, and Dispatch never
			// rewrites a log, so every boot, and every pass of a repository recovery whose scope
			// holds the root, leaves the same root out with a line naming the same event until a
			// person moves it out of triage or takes the label off. Every other failed read fails
			// the listing, every refusal of the read included: the boot waits out Dispatch out of
			// reach (dispatch.Unreachable) and reads again, it stops on every other failure and
			// names it (readinessAttempt), and a recovery tries again. The events route answers 200
			// for a key no issue holds, and it makes its refusals for every request alike (the
			// credential answers, 400 INVALID_QUERY, the no-route 404), so those say nothing about
			// one issue, and to leave the issue out would pass over every labelled triage root in
			// silence. A 5xx can be about one issue: an ask event in its log whose stored payload
			// Dispatch cannot read answers 500 for that log alone, and the boot waits on it until a
			// person moves the issue out of triage or takes the label off, after which the next
			// attempt reads no log for it. An event that still shows the issue an unrecorded
			// labelled triage root reads the log again, and its preparation fails the same way. A
			// move to todo reads no log, and admission takes the root.
			fact.LabelSetter, err = w.labelSetter(ctx, summary.Key)
			var unreadable *dispatch.EventLogError
			if errors.As(err, &unreadable) {
				w.log.Error("workflow: the listing leaves out of admission an issue whose Dispatch event log cannot be read",
					"issue", summary.Key, "event", unreadable.Event, "seq", unreadable.Seq, "error", err)
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("prepare who set the label of %s for admission: %w", summary.Key, err)
			}
		}
		listed = append(listed, fact)
	}
	target, err := w.consumers.DispatchTarget(ctx)
	if err != nil {
		return nil, fmt.Errorf("read notification stream target: %w", err)
	}
	position, err := w.consumers.ReconcilePosition(ctx)
	if err != nil {
		return nil, fmt.Errorf("read Dispatch consumer position: %w", err)
	}
	// The reconcile is its own transaction, so it owns the commit hooks its admission and workflow
	// changes register (store.OnCommit), as ApplyFact does for a fact, and it records issues, so it
	// takes the lock every fact applies under: a read of the live records for the repository
	// caches (refreshLive) never publishes over an issue it is recording. It settles the runs its
	// writes end as a fact does (intake.OpenSettlers), before it commits.
	var refused []string
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		ctx, settle := intake.OpenSettlers(ctx, w.handlers...)
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return fmt.Errorf("serialize the reconcile with every fact: %w", err)
		}
		// The boot's whole listing refills the project's rows of the copy of Dispatch's issues
		// where the project's mark asks for it (record.Store.TakeDispatchRefill): the first boot
		// of a project on the release of 20261120055717_dispatch_issue_value_sequences empties
		// the project's rows here, and the mirror below fills them from this listing, so the
		// empty, the fill and the mark's stamp commit together, under the fact lock and before
		// admission's pass, the boot's first decision. A transaction that fails leaves the mark,
		// and the next attempt refills. A recovery's listing is a part of the project, and it
		// never refills. Dispatch deletes no issue and moves none to another project, and the
		// listing is unpaged, so a key the refill removes that the listing does not hold is an
		// anomaly the line below names.
		if boot {
			removed, refilled, err := w.records.TakeDispatchRefill(ctx, tx, w.dispatchProject)
			if err != nil {
				return err
			}
			if refilled {
				inListing := make(map[string]bool, len(scope))
				for _, summary := range scope {
					inListing[summary.Key] = true
				}
				unlisted := make([]string, 0, len(removed))
				for _, key := range removed {
					if !inListing[key] {
						unlisted = append(unlisted, key)
					}
				}
				store.OnCommit(ctx, func() {
					w.log.Info("workflow: the first boot on this release refilled the copy of Dispatch's issues from the listing",
						"project", w.dispatchProject, "removed", len(removed), "written", len(scope), "removedUnlisted", unlisted)
				})
			}
		}
		// The mirror takes every summary, before admission decides anything from it — including a
		// summary admission defers to the Dispatch consumer: a deferred key's listed status is
		// still the freshest thing the daemon holds about it. Each value a summary carries
		// replaces the copy's only where an older reading wrote it or none did
		// (record.Store.PutDispatchIssue), so a listing no newer than an event already applied
		// changes nothing that event wrote. At the boot that refills, no reading of this boot has
		// reached the copy before this listing: the durable consumers start only in serve, after
		// the catch-up that follows this listing, and the API serves only from serve too (run), so
		// the rows the refill emptied are filled from this listing alone.
		if err := blockers.Reconcile(ctx, tx, w.records, w.dispatchProject, scope); err != nil {
			return err
		}
		// Every root the boot's catch-up will read is held out of the decision whether it waits on
		// a person before admission's pass here, until its catch-up has applied what Dispatch
		// recorded while the daemon was down (catchUpAwaiting).
		if boot {
			if err := w.holdForCatchUp(ctx, tx); err != nil {
				return err
			}
			if err := w.releaseParkedClaims(ctx, tx, scope); err != nil {
				return err
			}
		}
		var err error
		if refused, err = w.admission.Reconcile(ctx, tx, listed, target, position); err != nil {
			return err
		}
		return settle(ctx, tx)
	}); err != nil {
		return nil, fmt.Errorf("reconcile admission: %w", err)
	}
	return refused, nil
}

// identity is the bot a role's commits are authored by: its App's, from the lease the token
// source mints for the owner whose installation holds the repository the role works (the identity
// is fetched once per App and cached).
//
// recorded is the repository the issue's record names, and the mint is made for the owner GitHub
// serves it under now (repositories.serving): the record keeps the name it was written under
// until the one-time rename pass writes the pair's `to` over it (renamePass), while after a
// transfer the installation that covers the repository is the owner it moved to's. Taking the
// repository rather than an owner is what keeps that rule here, in
// the one place both callers reach — a launch's spec (specs.SpawnSpec) and a working copy's
// adoption (adoptIdentity) — so neither can mint on an owner the repository has left and fail
// every launch of a tree open at the transfer.
func (w *workflowRuntime) identity(ctx context.Context, role claim.Role, recorded ghrepo.Repository) (runtime.GitIdentity, error) {
	served := w.repositories.serving(recorded)
	lease, err := w.tokens.Token(ctx, appauth.AppRoleFor(role), served.Owner())
	if err != nil {
		return runtime.GitIdentity{}, fmt.Errorf("mint the %s App lease for %s on %s: %w", appauth.AppRoleFor(role), role, served, err)
	}
	return lease.Identity, nil
}

// knownIdentity is the bot identity role's App commits as where the token source already holds
// it (appauth.KnownIdentities: the identity a lease of that App fetched), read with no mint and
// no call to GitHub; false before that App's first lease, and for a token source that answers no
// such read, a test's fake among them. A bot identity is the App's own and no owner's, so it
// needs no repository. A launch's table of every role's identity reads it for the roles of the
// other App than the claim's own (specs.knownIdentity), so it costs that App no mint while the
// daemon knows its identity.
func (w *workflowRuntime) knownIdentity(role claim.Role) (runtime.GitIdentity, bool) {
	known, ok := w.tokens.(appauth.KnownIdentities)
	if !ok {
		return runtime.GitIdentity{}, false
	}
	return known.KnownIdentity(appauth.AppRoleFor(role))
}

// ensureWorkspace provisions an issue's workspace on the daemon's host when a launch finds it
// missing or not checked out (outbox.ensureWorkspace), through the outbox, which orders every
// provision of the shared clones (provisionLocks). The outbox exists from attach on, and attach
// runs before supervision restores or launches any claim.
func (w *workflowRuntime) ensureWorkspace(ctx context.Context, issue string, repo ghrepo.Repository, defaultBranch string) error {
	if w.outbox == nil {
		return fmt.Errorf("provision the workspace of %s: the workflow's outbox is not attached yet", issue)
	}
	return w.outbox.ensureWorkspace(ctx, issue, repo, defaultBranch)
}

// enqueueOutbox writes outbox rows of issue, due now, in one transaction of its own, which owns
// the commit hook that write registers (projectRecords, store.OnCommit) so each row's lane is
// woken as it commits — exactly as the re-hold of a notice does (reholdNotice). It is what
// supervision's update-round sweep and its stall backstop write their notices, their asks and
// their issue message through (supervision.enqueue), and the narrowest thing that gives
// supervision an outbox at all: rows to write, and no engine, no admission and no Dispatch client
// behind it. Rows given together land together or not at all, so a stall is never half told.
//
// It answers whether it wrote them. Supervision builds its rows from a claim's issue with no
// record read, and a claim of another project's issue, which the operator's spawn makes under
// this daemon's token, is in its sweeps as every claim is: a row keyed by that issue is one only
// that project's daemon runs (claim.KeyOfProject). So for such an issue it writes nothing, logs
// nothing and answers false, and each caller says what it did instead; the store's refusal of
// such a key (projectRecords.ownsKey) is never reached from here.
func (w *workflowRuntime) enqueueOutbox(ctx context.Context, issue string, payloads ...record.OutboxPayload) (bool, error) {
	if !claim.KeyOfProject(w.dispatchProject, issue) {
		return false, nil
	}
	rows := make([]record.OutboxRow, 0, len(payloads))
	for _, payload := range payloads {
		row, err := record.NewOutboxRow(issue, payload, time.Now().UTC())
		if err != nil {
			return false, fmt.Errorf("enqueue a %s row of %s: %w", payload.OutboxKind(), issue, err)
		}
		rows = append(rows, row)
	}
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		for _, row := range rows {
			if err := w.records.Enqueue(ctx, tx, row); err != nil {
				return fmt.Errorf("enqueue a %s row of %s: %w", row.Kind, issue, err)
			}
		}
		return nil
	}); err != nil {
		return false, err
	}
	return true, nil
}

// sweptFact is one fact a sweep of supervision read and the id it is applied under. It is held
// here, beside the two that use it, because the sweep that mints them and applySweepFacts are
// both this package's (silence.go's tellSilentPhaseWorkers).
type sweptFact struct {
	ID   string
	Fact intake.Fact
}

// applySweepFacts applies each fact of one sweep of supervision that this daemon has not
// applied already, and is the whole of what supervision is given of the workflow for that
// (supervision.applySweepFacts), as enqueueOutbox above is the whole of what it is given of the
// outbox. Every such fact is read off the supervisor's claims, so all of them are applied under
// the source a claim's own terminal facts take (terminalSource); their ids share none of its
// ids, and processed_events is keyed on the pair. Each fact is applied on its own, as every
// fact applies, and the first error is returned once every other fact of the batch has had its
// turn: one silence the store refused is no reason to leave the rest of the sweep unapplied.
//
// Which ids are applied already is read first, in one read-only transaction outside the fact lock
// (intake.Applied), and that read is the point of taking the batch rather than one fact at a
// time. A sweep runs every minute over every claim, and a claim that is silent and stays silent
// mints the same id at every sweep for ever, since the id is the claim token and a last step
// that does not move. The reading this work was filed for found hundreds of claims with a last
// step more than an hour old (dispatch://INFRA-3163), and without this read the pass would open
// a transaction for each of them every minute on the one advisory lock every fact of the daemon
// takes (intake.ApplyFact), each one only to be told the fact was applied already.
//
// A failure of that read takes the whole sweep down with it, no fact of it applied, so it is
// logged here in the register the failure of one apply below is logged in: its caller discards
// the error (tellSilentPhaseWorkers) on the strength of every failure being logged where it
// failed, and a sweep that vanished without a line would make that false.
func (w *workflowRuntime) applySweepFacts(ctx context.Context, facts []sweptFact) error {
	ids := make([]string, 0, len(facts))
	for _, fact := range facts {
		ids = append(ids, fact.ID)
	}
	applied, err := intake.Applied(ctx, w.pool, terminalSource, ids)
	if err != nil {
		w.log.Warn("the workflow could not read which facts of a sweep of supervision are applied already; none of the sweep is applied and it is read again at the next sweep",
			"facts", len(facts), "error", err)
		return err
	}
	var first error
	for _, fact := range facts {
		if applied[fact.ID] {
			continue
		}
		if _, err := intake.ApplyFact(ctx, w.pool, terminalSource, fact.ID, fact.Fact, w.handlers...); err != nil {
			w.log.Warn("the workflow could not apply a fact a sweep of supervision read; it is read again at the next sweep",
				"id", fact.ID, "error", err)
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// attach gives the workflow what only supervision has: the outbox, built on its supervisor and
// its runtime, and the deployment's budget limits, which a failed claim's fact carries to the
// workflow engine (applyTerminal). The pace a paced catch-up's wake line names is the
// supervisor's own (supervise.WakePace, the rate and the burst the machines are held to), which
// the outbox reads through that supervisor where it writes the line (wakeRan); the pacer both
// lanes wait in — the workflow's catch-up through its catchUpPacing, the machines at their gate —
// was built before either of them (run). The outbox's two reads of one pull
// request, the merge-queue read and the second read of a conflicting head, read GitHub through
// this runtime's own read of a pull request, apply its answer as every read of this runtime does,
// the base's rules read with it on a conflicting answer, and wait this runtime's wait before they
// read again (readPullRequest, applyMergeability, mergeStateWait); a second read whose last
// attempt read no answer hands the head to this runtime's merge-state reader to follow
// (queueMergeStateFollow). The outbox's branch create runs the private-name rules boot compiled
// (privateNameRules). All are set before the records' wake can reach the outbox
// (outboxWake). Whether an issue still holds its workspace, which a repository kept for an
// unrecorded issue is answered while (keptRepository), is read from the runtime (workspaceHeldBy),
// and so is the rule the end of an issue's pod runs the rename pass under, the outbox's podEnded,
// which also drops that repository (podEndsOn).
func (w *workflowRuntime) attach(supervision *supervision) {
	w.log.Info("legion workflow boot stage", "stage", "outbox")
	w.outbox = newOutbox(w.pool, w.records, w.dispatch, notify.New(supervision.cfg.EnvoyURL, supervision.plan.secrets["ENVOY_TOKEN"]), supervision.supervisor,
		supervision.supervisor.store, w.tokens, w.handlers, supervision.cfg.Gates.Steps(),
		w.projectID, w.dispatchProject, w.stateDir, w.githubAPI,
		w.repositories, supervision.plan.tools, supervision.cfg.ControllerLaunch == config.ControllerLaunchDaemon, w.log)
	w.outbox.readPullRequest, w.outbox.applyMergeability, w.outbox.mergeStateWait = w.readPullRequest, w.applyMergeability, w.mergeStateWait
	w.outbox.queueMergeStateFollow = w.queueMergeStateFollow
	w.outbox.privateNameRules = supervision.plan.privateNameRules
	w.outbox.podEnded = w.podEndsOn(supervision.supervisor.deps.Runtime)
	w.outbox.sweepEvery = w.outboxSweep
	w.outbox.catchUps = w.catchUps
	w.outboxWake.Store(w.outbox)
	w.limits = supervision.supervisor.deps.Limits
	supervision.supervisor.OnDurableState(w.claimDurableState)
	supervision.enqueue = w.enqueueOutbox
	supervision.applySweepFacts = w.applySweepFacts
	if w.pauses != nil {
		// The flag is read again at each sweep (decision 18), so a pause made or ended through
		// another daemon of a rolling restart reaches this daemon's machines within one sweep.
		w.outbox.refreshPause = w.pauses.service.Refresh
	}
	w.attachIdleRelease(supervision)
	w.workspaceHeld = workspaceHeldBy(supervision.supervisor.deps.Runtime, w.stateDir)
}

// messageMarked is workflow.Config.MessageMarked: the claim token's machine is told the mark a
// listener's report left on it in the store, so that a wake of the claim waiting in the pacing's
// second queue moves to the message queue (supervise.MessageMarked). It reaches the supervisor
// attach stored, and nothing before attach: a report applied before then is one the boot's own
// read of the claim finds marked.
func (w *workflowRuntime) messageMarked(token claim.Token, mark time.Time) {
	if r := w.outboxWake.Load(); r != nil {
		r.supervisor.post(token, supervise.MessageMarked{Claim: token, At: mark})
	}
}

// workspaceHeldBy is whether an issue holds its workspace under rt: its Sandbox, where rt keeps
// one per issue (issuePods, the Kubernetes runtime's), and otherwise the workspace directory a
// daemon on its own host provisions for the issue and repo under stateDir (workspace.Location).
//
// Its one reader, keptRepository, asks it of an issue the workflow does not record. IssueSandbox
// answers a Sandbox its store holds absent only where a release kept it with the issue's volume
// (ReleaseIssue) or where it is going: being deleted, or taken by the runtime's own delete. The
// store authorizes neither the release nor the close of an issue it holds no row of
// (store.IssueRelease, store.IssueSuspension). So such an issue's Sandbox answers present for as
// long as it lives, until a delete takes it with the workspace on it.
func workspaceHeldBy(rt runtime.Runtime, stateDir string) func(issue string, repo ghrepo.Repository) bool {
	if pods, ok := rt.(issuePods); ok {
		return func(issue string, _ ghrepo.Repository) bool {
			_, present := pods.IssueSandbox(issue)
			return present
		}
	}
	return func(issue string, repo ghrepo.Repository) bool {
		_, err := workspaceDir(stateDir, repo, issue)
		return err == nil
	}
}

// workspaceDir is the workspace directory a daemon on its own host provisions for issue and repo
// under stateDir (workspace.Location), "" where Location names none, and what os.Stat answers for
// it, Location's refusal where it names none. Each caller keeps its own rule for what that means:
// a held workspace (workspaceHeldBy) and a rename the pass keeps an issue from (workspaceKeep).
func workspaceDir(stateDir string, repo ghrepo.Repository, issue string) (string, error) {
	location, err := workspace.Location(stateDir, repo, issue)
	if err != nil {
		return "", err
	}
	_, err = os.Stat(location.Dir)
	return location.Dir, err
}

// idleRelease is the deployment's idle release, and whether the deployment releases pods at all:
// `runtime: kubernetes` with a non-zero idle_minutes. The idle tick (attachIdleRelease) and the
// backstop a pause's stop arms (releaseBackstop) both read it.
func idleRelease(cfg config.Config) (config.IdleReleaseConfig, bool) {
	k := cfg.Runtime.Kubernetes
	if cfg.Runtime.Name != "kubernetes" || k == nil || !k.IdleRelease.Releases() {
		return config.IdleReleaseConfig{}, false
	}
	return k.IdleRelease, true
}

// attachIdleRelease wires the idle tick, and leaves it off unless the deployment releases pods
// (idleRelease), over a runtime that can say whether an issue's pod runs the image the
// configuration names and whether it still holds the issue's Sandbox (issuePods).
func (w *workflowRuntime) attachIdleRelease(supervision *supervision) {
	release, ok := idleRelease(supervision.cfg)
	if !ok {
		return
	}
	podRuntime, ok := supervision.supervisor.deps.Runtime.(issuePods)
	if !ok {
		w.log.Warn("idle release: the runtime reads no issue pod, so no pod is released")
		return
	}
	w.idlePeriod, w.idleStore, w.idleRuntime = release.Idle(), supervision.supervisor.store, podRuntime
	w.outbox.backstop = releaseBackstop(supervision.cfg)
}

// replayTerminal applies the ready or failure fact of every stored claim in either state that no
// daemon applied: one whose live apply (terminal) failed, or whose fact a daemon's stop cut off
// after the claim's state was written. Nearly all were applied when they happened, and ApplyFact
// would take the fact lock only to find so, so the replay reads which are applied in one query
// (intake.Applied) and applies only the rest, one at a time as every fact applies.
//
// The read and each fact get budget of their own, on ctx, the daemon's lifetime, rather than one
// budget shared by the whole replay. A fact may wait its turn on the lock behind a live one (a
// restored claim's failure, a daemon of another project on the same database), so a replay of
// hundreds of facts takes as long as they take, and the boot never exits on their count; a
// Postgres that never answers still ends the boot within one wait for a connection and one budget
// (below), and the daemon's stop ends the replay. supervision.start runs it before any hello is
// admitted.
//
// Each budget counts from the moment its transaction holds a connection of the pool, and the wait
// for that connection has replayAcquire of its own (store.BoundAfterAcquire). Counted from before
// that wait, a budget was the pool queue's bound as well: the live failures of the restored claims
// apply beside the replay, each on a connection of its own, and the replay's read waited behind
// them for one of the pool's few connections until its budget ran out ("read which supervise facts
// are applied: context deadline exceeded after 5.012s", TestAReplayOfHundredsOfUnappliedFacts
// BesideLiveFailuresOutlastsItsBudget on main's CI).
func (w *workflowRuntime) replayTerminal(ctx context.Context, claims []supervise.Claim, budget time.Duration) error {
	var terminal []supervise.Claim
	var ids []string
	for _, c := range claims {
		if (c.State != supervise.StateReady && c.State != supervise.StateFailed) || c.Role == claim.RoleController {
			continue
		}
		terminal = append(terminal, c)
		ids = append(ids, terminalEventID(c, c.State))
	}
	read, cancel := store.BoundAfterAcquire(ctx, replayAcquire, budget)
	applied, err := intake.Applied(read, w.pool, terminalSource, ids)
	cancel()
	if err != nil {
		return fmt.Errorf("replay the stored claims' terminal facts: %w", err)
	}
	var unapplied []supervise.Claim
	for i, c := range terminal {
		if !applied[ids[i]] {
			unapplied = append(unapplied, c)
		}
	}
	if len(unapplied) > 0 {
		w.log.Info("legion daemon is replaying the terminal facts no daemon applied", "count", len(unapplied), "terminalClaims", len(terminal))
	}
	for _, c := range unapplied {
		fact, cancel := store.BoundAfterAcquire(ctx, replayAcquire, budget)
		err := w.applyTerminal(fact, c, c.State)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

// replayAcquire is how long each read and fact of the boot's terminal replay waits for a
// connection of the pool before its budget starts (replayTerminal). The pool turns over as each
// transaction ends, so thirty seconds is a queue many writes deep, against writes of milliseconds;
// a Postgres that gives no connection at all still ends the boot within it and the budget.
const replayAcquire = 30 * time.Second

// Each retry of the boot fact waits twice the last, from bootRetryFirst to at most bootRetryMax.
const (
	bootRetryFirst = time.Second
	bootRetryMax   = 30 * time.Second
)

// runBooted applies this boot's fact (booted) beside the serving API, retrying every failure — a
// timeout included — on a backoff, each logged at ERROR, until it lands or the daemon stops. The
// fact is keyed by the boot, so a retry after a commit whose answer was lost is a duplicate, not a
// second census. Only the daemon's stop leaves it unapplied, and the next boot applies its own.
func (w *workflowRuntime) runBooted(ctx context.Context, bootID int64) {
	retryUntilApplied(ctx, w.log, bootRetryFirst, bootRetryMax, func(ctx context.Context) error { return w.booted(ctx, bootID) })
}

// retryUntilApplied runs apply until it succeeds or ctx ends, waiting first, then twice as long up
// to most, between attempts, and logging each failure at ERROR. A failure once ctx has ended is the
// stop, and logs nothing.
func retryUntilApplied(ctx context.Context, log *slog.Logger, first, most time.Duration, apply func(context.Context) error) {
	wait := first
	for attempt := 1; ; attempt++ {
		err := apply(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		log.Error("legion daemon could not apply its boot; retrying", "attempt", attempt, "retryIn", wait, "error", err)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		wait = min(2*wait, most)
	}
}

// booted tells the workflow of this boot (intake.DaemonBooted), once every stored claim has its
// machine and the terminal facts are replayed: it starts, through the outbox's start, every claim
// that owns its issue's current phase and that the daemon or its own exit suspended.
func (w *workflowRuntime) booted(ctx context.Context, bootID int64) error {
	fact := intake.DaemonBooted{Boot: bootID}
	if _, err := intake.ApplyFact(ctx, w.pool, "daemon", fmt.Sprintf("boot:%s:%d", w.dispatchProject, bootID), fact, w.handlers...); err != nil {
		return fmt.Errorf("apply the boot of %s: %w", w.dispatchProject, err)
	}
	return nil
}

// repairCutoverMergers applies the boot's one-time repair of the mergers the `awaiting_merge`
// cutover left stopped (intake.CutoverMergerRepair, workflow's cutoverMergerRepair), under the
// one event id the project keeps for it, so processed_events records that it ran and a later
// boot's apply is a duplicate that repairs nothing (dispatch://INFRA-3274/spec R2). run applies
// it after the listing's reconcile and the roots' catch-up and before the consumers start, so no
// wake of the boot comes before it. A duplicate runs no handler, and the boot still writes its one
// record of the repair (R2), so this logs it: the repair owed a start to no merger.
func (w *workflowRuntime) repairCutoverMergers(ctx context.Context) error {
	result, err := intake.ApplyFact(ctx, w.pool, "daemon", "cutover-merger-repair:"+w.dispatchProject, intake.CutoverMergerRepair{}, w.handlers...)
	if err != nil {
		return fmt.Errorf("apply the cutover merger repair of %s: %w", w.dispatchProject, err)
	}
	if result.Duplicate {
		w.log.Info("cutover merger repair: owed a start to 0 mergers; the repair ran at an earlier boot", "count", 0, "issues", []string{})
	}
	return nil
}

// openUncountedRuns gives each run no start of this release counted, which an issue of the
// project stands in at the boot, an open row in step_starts (workflow's OpenUncountedRuns), in one
// transaction of its own under the lock every fact applies under. run runs it before the listing's
// reconcile, the boot's first admission pass, and before any fact, so each fact's settle ends such
// a run as an open run, and a fact its agent's request applies waits for that agent's stream
// first. It queues nothing, and a boot that runs again opens nothing; it logs how many runs it
// opened when it opened any, once its transaction has committed.
func (w *workflowRuntime) openUncountedRuns(ctx context.Context) error {
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return fmt.Errorf("serialize the open of the uncounted runs with every fact: %w", err)
		}
		opened, err := w.engine.OpenUncountedRuns(ctx, tx)
		if err != nil {
			return err
		}
		if opened != 0 {
			store.OnCommit(ctx, func() { w.log.Info("opened the runs a start of an earlier release began", "count", opened) })
		}
		return nil
	}); err != nil {
		return fmt.Errorf("open the runs of %s no start of this release counted: %w", w.dispatchProject, err)
	}
	return nil
}

// terminal applies a claim's ready or failure as it happens, on a context the daemon's stop does
// not end: the fact is the supervisor's observation of a claim that has already reached that
// state, so it is worth landing even while the daemon is going down, and nothing of the stop can
// make it untrue.
//
// No deadline ends it either: the fact waits its turn on the fact serializer (intake.ApplyFact's
// one advisory lock) for as long as the facts queued ahead of it take, and is applied once the
// serializer is free. That lock is a queue behind every fact, and intake gives a message at least
// ten minutes for the same wait (intake.HandlerLimitFor), so a wait of tens of seconds is load and
// not a fault; and an error here ends the workflow (w.failed) and with it the daemon, so a bound
// would turn a slow queue into a restart, which is what started INFRA-3169. The wait still ends
// when the real work ahead of it ends: every connection of the daemon's pool carries the idle
// bound (internal/store's idleBound, 30 s), so a holder of the serializer that sits in its
// transaction with no statement running loses its connection, and the lock with it — the
// transaction nobody ended, which INFRA-3169 measured. The boot's bound stays the boot's own:
// replayTerminal gives each fact it replays a budget (bootTimeout, daemon.go), since a boot that
// waits on a Postgres that never answers would hang with nothing logged.
//
// A bounded attempt retried until it lands, as runBooted applies the boot's fact, was the other
// way, and a safe one: the fact's event id (terminalEventID) makes a retry whose first attempt
// committed a duplicate that applies nothing (ApplyFact's MarkProcessed). It was not taken for two
// reasons. Only a timeout could be retried, since every other error ends the workflow and must
// go on doing so, so the retry would have to sort what pgx returns into a deadline's errors and
// the rest; with no deadline, every error this call returns is a real failure. And an attempt
// that gives up leaves the lock's queue, so its retry waits behind every fact that came in the
// meantime, and a queue that stays longer than one attempt would never let the fact through.
func (w *workflowRuntime) terminal(c supervise.Claim, state supervise.ClaimState) {
	// A machine calls this inside its own decision, on its own goroutine and beside its siblings,
	// so the fact is applied under a hold mark of this call's own (store.ForConcurrentWorker). The
	// context carries no deadline and no cancellation, for the reasons above.
	ctx := store.ForConcurrentWorker(context.Background())
	if err := w.applyTerminal(ctx, c, state); err != nil {
		w.log.Error("apply supervision terminal fact", "claim", c.Token, "state", state, "error", err)
		select {
		case w.failed <- err:
		default:
		}
	}
}

// claimDurableState is the daemon's one durable-state callback, because a supervisor keeps one
// (supervisor.OnDurableState): a claim's ready and its failure are workflow facts (terminal), and
// the same transitions measure a released pod's resume — its replacement pod is up at the first
// ready, and the woken role took its work at the first turn (podResume).
func (w *workflowRuntime) claimDurableState(c supervise.Claim, state supervise.ClaimState) {
	w.podResume(c, state)
	if state == supervise.StateReady || state == supervise.StateFailed {
		w.terminal(c, state)
	}
}

// applyTerminal applies a claim's ready or failure as a workflow fact. The daemon's own controller
// (`controller: daemon`) holds a claim on no issue, so neither is a workflow fact for it: its
// relaunch is the controller keeper's (controllerKeeper).
//
// The ready carries the sentence the claim's untold record of what its last let-go ended is told
// in (supervise.RelaunchSentence, intake.ClaimReady.Relaunch), read from the claim the ready
// wrote, so an architect's catch-up opens with it (workflow's QueueCatchUp). A record that
// still tells here beside an abandoned session still to tell is one of the fresh session's own
// processes (supervise's forgetSession), and the catch-up opens with its sentence as it would for
// any claim. The boot's replay of a ready reads the stored claim, whose record is persisted, and
// carries what is still untold.
//
// The failure carries what a notice of it says beside the sentence: the cause the site that
// failed the claim chose, the claim's budgets as that failure left them, and the limits those
// budgets ran out against, which this runtime took from the supervisor at attach. The boot's
// replay of a failure no daemon applied builds the fact here too (replayTerminal), from the
// stored claim, which carries its cause and its counters across the restart.
func (w *workflowRuntime) applyTerminal(ctx context.Context, c supervise.Claim, state supervise.ClaimState) error {
	if c.Role == claim.RoleController {
		return nil
	}
	var fact intake.Fact
	switch state {
	case supervise.StateReady:
		ready := intake.ClaimReady{Issue: c.Issue, Role: c.Role, Launch: c.Generation}
		if c.EndedWork.Tells() {
			ready.Relaunch = supervise.RelaunchSentence(c.EndedWork)
		}
		fact = ready
	case supervise.StateFailed:
		// Everything the claim persists about its end: the sentence the supervisor wrote when it
		// gave up (Claim.Failure), what the runtime last said of the death and of the start that
		// never produced an agent (Claim.LastDeathCause, Claim.LastStartFailure), and the pod end
		// its process was last observed to have died of (Claim.PodEnd), so a notice written after
		// a daemon restart still names the cause the role never chose — an eviction above all.
		fact = intake.ClaimFailed{Issue: c.Issue, Role: c.Role, Reason: c.Failure, PodEnd: c.PodEnd,
			Death: c.LastDeathCause, StartFailure: c.LastStartFailure,
			Cause: c.FailureCause, Budgets: c.Budgets, Limits: w.limits}
	default:
		return fmt.Errorf("claim %s has non-terminal workflow state %s", c.Token, state)
	}
	_, err := intake.ApplyFact(ctx, w.pool, terminalSource, terminalEventID(c, state), fact, w.handlers...)
	if err != nil {
		return fmt.Errorf("apply %s for %s: %w", state, c.Token, err)
	}
	return nil
}

// terminalSource is the source a claim's ready or failure fact is applied under, and
// terminalEventID its event: one per launch (the claim's generation) and state, so the live apply
// and every boot's replay of the same terminal state are one fact.
const terminalSource = "supervise"

func terminalEventID(c supervise.Claim, state supervise.ClaimState) string {
	return fmt.Sprintf("supervise:%s:%d:%s", c.Token, c.Generation, state)
}

// run drains intake and the outbox until ctx ends. Intake ending, or a terminal fact that could not
// be applied, returns its error so serve stops the daemon: a daemon that answers its API while it
// reads no events or has lost a held or worker-died fact looks healthy and does nothing.
func (w *workflowRuntime) run(ctx context.Context) error {
	group, running := errgroup.WithContext(ctx)
	// Every loop below runs beside the others for the life of the process, so each takes a hold
	// mark of its own for the pool (store.ForConcurrentWorker): on one shared mark the first loop
	// to open a transaction would have the pool refuse every other loop its first connection,
	// while on a mark apiece each is refused only the second connection it asks for while holding
	// one (store.ErrNestedAcquire). The intake marks each message it hands a loop of its own
	// (intake's consumeMessage), and the outbox each of its lanes (outbox.runLane).
	group.Go(func() error {
		if err := w.consumers.Run(store.ForConcurrentWorker(running), w.handlers...); err != nil {
			// A terminal close's cause is the connection's alone (natsauth.WithLastError).
			return fmt.Errorf("workflow intake stopped: %w", natsauth.WithLastError(err, w.conn.Load()))
		}
		return nil
	})
	group.Go(func() error {
		w.outbox.Run(store.ForConcurrentWorker(running))
		return nil
	})
	group.Go(func() error {
		w.pollHoldRelease(store.ForConcurrentWorker(running))
		return nil
	})
	group.Go(func() error {
		w.refreshRepositories(store.ForConcurrentWorker(running))
		return nil
	})
	group.Go(func() error {
		w.reconnectWatchdog(store.ForConcurrentWorker(running))
		return nil
	})
	group.Go(func() error {
		w.tickController(store.ForConcurrentWorker(running))
		return nil
	})
	group.Go(func() error {
		w.tickEntrySweep(store.ForConcurrentWorker(running))
		return nil
	})
	group.Go(func() error {
		// The start read is the daemon's one read of every issue in the workflow: it covers the
		// deliveries GitHub made while the daemon was down, which nothing redelivers.
		w.readIssuesAtStart(store.ForConcurrentWorker(running))
		return nil
	})
	group.Go(func() error {
		w.readHandedOff(store.ForConcurrentWorker(running))
		return nil
	})
	group.Go(func() error {
		w.readMergeStates(store.ForConcurrentWorker(running))
		return nil
	})
	group.Go(func() error {
		w.tickIdle(store.ForConcurrentWorker(running))
		return nil
	})
	group.Go(func() error {
		w.tickWakes(store.ForConcurrentWorker(running))
		return nil
	})
	group.Go(func() error {
		w.releaseReadied(store.ForConcurrentWorker(running))
		return nil
	})
	group.Go(func() error {
		// A notice the listener accepted but could not forward is queued again (outbox.rehold).
		// Each re-hold runs on the subscription's own goroutine, beside every lane of the outbox,
		// so it takes a mark of its own rather than the one this goroutine waits on.
		sub, err := subscribeNoticeExceptions(w.conn.Load(), func(data []byte) {
			if err := w.outbox.rehold(store.ForConcurrentWorker(running), data); err != nil {
				w.log.Error("role-lane exception not re-held", "error", err)
			}
		})
		if err != nil {
			return err
		}
		<-running.Done()
		return sub.Unsubscribe()
	})
	group.Go(func() error {
		select {
		case err := <-w.failed:
			return fmt.Errorf("workflow supervision fact: %w", err)
		case <-running.Done():
			return nil
		}
	})
	return group.Wait()
}

// defaultHoldPollInterval is pollHoldRelease's production ticker period.
const defaultHoldPollInterval = 2 * time.Second

// defaultHoldWarnAfter and defaultHoldWarnEvery bound the watchdog log a hold that has not
// released gets: a Dispatch event whose transaction always fails (an unmet precondition, a
// programming bug) holds the ack floor below it until the intake gives the event up, no sooner
// than an hour after its first failure (intake's giveUpDue), and nothing else says why a labeled
// root waits that long.
const (
	defaultHoldWarnAfter = 2 * time.Minute
	defaultHoldWarnEvery = 5 * time.Minute
)

// positionReader is pollHoldRelease's only dependency on the Dispatch consumer: reading its
// current position. A test substitutes one that fails on demand to prove the poll logs and
// retries rather than silently ending the hold.
type positionReader interface {
	DispatchPosition(ctx context.Context) (intake.DispatchConsumerPosition, error)
}

// pollHoldRelease is the boot-owned release: while
// admission holds anything back, it re-reads the Dispatch consumer's own position on a ticker and
// applies each changed reading as a synthetic fact, independent of any message delivery. A quiet
// stream after its last backlog message delivers nothing further to trigger a release the old
// per-delivery hook depended on, and Ack() does not wait for the server: a position read taken
// immediately after a delivery's own ack can still see the previous one. This ticker is what
// eventually observes the ack once JetStream has applied it, bounded by its own period, and what
// closes a hold nothing will ever redeliver — the recreated-consumer and outbox-lag cases alike.
// A failed read or a failed apply is logged and retried on the next tick — last only adopts a
// position once ApplyFact for it has actually committed, so an unchanged reading after either
// kind of failure is retried rather than skipped as already seen; the hold never ends on the
// strength of an error.
// The event id folds in the project and this boot (bootID): processed_events is one table
// shared by every project's daemon, and a stream a later boot recreates would otherwise repeat an
// earlier boot's (ack floor, idle) pairs and have its own release swallowed as a duplicate of one
// that ran under the old boot. It folds in the hold too, admission's count of its holds and the
// target the hold waits for: a repository's recovery can hold again in the same boot (the boot's
// hold is not the only one), and its release can read the same (ack floor, idle) as an earlier
// hold's, which would otherwise be applied as that earlier release's duplicate, or skipped as
// the reading last applied, and the hold never released.
func (w *workflowRuntime) pollHoldRelease(ctx context.Context) {
	w.pollHoldReleaseWith(ctx, w.consumers)
}

func (w *workflowRuntime) pollHoldReleaseWith(ctx context.Context, reader positionReader) {
	interval := w.holdPollInterval
	if interval <= 0 {
		interval = defaultHoldPollInterval
	}
	warnAfter, warnEvery := w.holdWarnAfter, w.holdWarnEvery
	if warnAfter <= 0 {
		warnAfter = defaultHoldWarnAfter
	}
	if warnEvery <= 0 {
		warnEvery = defaultHoldWarnEvery
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var last intake.DispatchConsumerPosition
	var lastHold uint64
	haveLast := false
	var heldSince, warnedAt time.Time
	wasHeld := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !w.admission.Held() {
			haveLast, heldSince, warnedAt = false, time.Time{}, time.Time{}
			if wasHeld {
				// The hold just cleared: a record a held key's own recording put down between an
				// earlier release's commit and its commit hook (which cleared pending) may still be
				// sitting in the waiting line, held back by that same release's own promote, which
				// ran before pending was actually cleared. One more pass, now that pending is
				// provably empty, promotes it without waiting on an unrelated fact to notice. A
				// fresh, never-reused event id (a closing pass never repeats a boot's own position
				// reading) keeps ApplyFact from treating it as the duplicate of whatever position
				// last released the hold. A failure is retried on the poll's own schedule, same as
				// a failed position apply: wasHeld only clears once the pass actually succeeds.
				if err := w.promoteAfterHoldClears(ctx); err != nil {
					w.log.Warn("apply the post-release promote failed", "error", err)
					continue
				}
			}
			wasHeld = false
			continue
		}
		wasHeld = true
		if heldSince.IsZero() {
			heldSince = time.Now()
		}
		position, err := reader.DispatchPosition(ctx)
		if err != nil {
			w.log.Error("read Dispatch consumer position for a held release", "error", err)
			continue
		}
		// This never releases on the strength of the timer — only a Reached position does that,
		// through the ApplyFact below — so a stuck hold stays stuck, but an operator now sees why:
		// the target it is waiting for and its current ack floor. Not the stream sequence stuck
		// behind that floor: the notification stream also carries GitHub subjects interleaved with
		// Dispatch's, so ack_floor+1 can name a message that has nothing to do with this hold.
		if waiting := time.Since(heldSince); waiting >= warnAfter && time.Since(warnedAt) >= warnEvery {
			warnedAt = time.Now()
			w.log.Warn("admission: a hold has not released", "waiting", waiting.Round(time.Second),
				"target", w.admission.Target(), "ack_floor", position.AckFloorStream)
		}
		hold := w.admission.Holds()
		if haveLast && position == last && hold == lastHold {
			continue
		}
		eventID := fmt.Sprintf("dispatch-position:%s:%s:%d:%d:%d:%t", w.dispatchProject, w.bootID, hold, w.admission.Target(), position.AckFloorStream, position.Idle)
		if _, err := intake.ApplyFact(ctx, w.pool, "dispatch", eventID, position, w.handlers...); err != nil {
			w.log.Warn("apply Dispatch consumer position failed", "error", err)
			continue
		}
		last, lastHold, haveLast = position, hold, true
	}
}

// defaultReconnectPollInterval is reconnectWatchdog's ticker period.
const defaultReconnectPollInterval = 10 * time.Second

// defaultReconnectWarnEvery bounds how often reconnectWatchdog logs while NATS has not
// reconnected. natsauth.ReconnectForever never gives up, and natsauth.LogEvents logs a
// disconnect and a reconnect once each, so a non-auth failure that keeps repeating — a server
// that accepts the TCP connection but never completes the protocol handshake, say — retries
// silently behind that one "NATS connection lost" line, in RECONNECTING, with nothing further to
// say so. A fatal server -ERR or a repeated authorization failure still closes the connection
// and exits loud (natsauth.LogEvents' ClosedCB, workflow.connect's own failure path); this
// watchdog covers only the case ReconnectForever otherwise leaves silent: forever retrying,
// never closing, never reconnecting.
const defaultReconnectWarnEvery = 3 * time.Minute

// connStatus is reconnectWatchdog's only dependency on *nats.Conn: a test substitutes a fake that
// reports disconnected, with a LastError, on demand.
type connStatus interface {
	IsConnected() bool
	LastError() error
}

// reconnectWatchdog logs a warn once the downtime since conn last connected reaches
// reconnectWarnEvery, and every reconnectWarnEvery after that while conn stays disconnected —
// never on the first poll that merely finds it down, which would log a downtime near zero and
// make this almost as noisy as nats.go's own reconnect wait. Each warn names the downtime so far
// and conn's own LastError, usually empty during a plain refused dial (nats.go clears its own
// last-seen error on every failed attempt) and naming the failure's own cause while a handshake
// keeps failing (nats.go leaves that one in place across attempts), so an operator reading the
// log sees that NATS has been down, for how long, and why when nats.go has anything to say,
// instead of just the one disconnect line a reconnect that keeps failing never reaches a
// reconnected or a closed line to follow.
func (w *workflowRuntime) reconnectWatchdog(ctx context.Context) {
	w.reconnectWatchdogWith(ctx, w.conn.Load())
}

func (w *workflowRuntime) reconnectWatchdogWith(ctx context.Context, conn connStatus) {
	interval := w.reconnectPollInterval
	if interval <= 0 {
		interval = defaultReconnectPollInterval
	}
	warnEvery := w.reconnectWarnEvery
	if warnEvery <= 0 {
		warnEvery = defaultReconnectWarnEvery
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var disconnectedSince, warnedAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if conn.IsConnected() {
			disconnectedSince, warnedAt = time.Time{}, time.Time{}
			continue
		}
		if disconnectedSince.IsZero() {
			disconnectedSince = time.Now()
		}
		downtime := time.Since(disconnectedSince)
		if downtime < warnEvery || (!warnedAt.IsZero() && time.Since(warnedAt) < warnEvery) {
			continue
		}
		w.log.Warn("NATS has not reconnected", "downtime", downtime.Round(time.Second).String(), "error", conn.LastError())
		warnedAt = time.Now()
	}
}

// firstControllerTick is how soon after the runtime starts it applies its first ControllerTick:
// a daemon restarted more often than the interval would otherwise never tick at all. A runtime's
// controllerFirstWake, set only by a test, replaces it.
const firstControllerTick = time.Minute

// tickController applies a ControllerTick fact once shortly after the runtime starts
// (firstControllerTick, or the interval when that is shorter) and then every controllerWake, so
// admission wakes the controller even when no event arrives: the last tree finishing, a tree
// waiting on a refused root claim, or a quiet day would otherwise leave the controller idle and the
// daily report unposted until the next wake. Each tick's event id is its
// own (the project, this boot, and the tick's time), since processed_events is shared by every
// project's daemon. A failed apply is logged, and the next tick tries again.
func (w *workflowRuntime) tickController(ctx context.Context) {
	interval := w.controllerWake
	if interval <= 0 {
		interval = time.Hour
	}
	delay := firstControllerTick
	if w.controllerFirstWake > 0 {
		delay = w.controllerFirstWake
	}
	first := time.NewTimer(min(delay, interval))
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case at := <-first.C:
		w.applyControllerTick(ctx, at)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-ticker.C:
			w.applyControllerTick(ctx, at)
		}
	}
}

// entrySweepInterval is how often the daemon converges the phase entries with no fact of its
// project arriving (tickEntrySweep). The difference a sweep closes is a role waiting for work
// its record already names, so a minute is short enough that no worker waits long for it, and
// each run is one admission read of the project's issues, slots and line facts, which every pass
// already makes (the pass rule on admit's Admission.Apply).
const entrySweepInterval = time.Minute

// entrySweepPeriod is the period tickEntrySweep runs at: override, the runtime's own entrySweep
// that a test shortens, where one is set, and entrySweepInterval where none is, which is what
// the shipped binary runs. It is a function of its own so a test reads the period a daemon with
// no override sweeps at, and not the constant alone.
func entrySweepPeriod(override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return entrySweepInterval
}

// tickEntrySweep applies an intake.EntrySweep fact every entrySweepPeriod (entrySweepInterval,
// or the runtime's own entrySweep, which a test shortens), so a role behind the entry its phases
// row names is sent that entry's work, and the controller is woken for a stalled root architect,
// even where nothing else happens in the project: an admission pass runs on the facts the pass
// rule names (admit's Admission.Apply), and a quiet project raises none. Each run's event id is
// its own — the word, the project, this boot and the run's time — since processed_events is
// shared by every project's daemon. A failed apply is logged, and the next run tries again, as
// applyControllerTick does.
//
// The same tick queues the reconcile of each tree's news of its approved design that
// record.OwedNews owes and no queued row will reconcile (queueOwedDesignNews): the backstop for
// news no fact queued a row for, which queues nothing while the project is paused, as the gate of
// the entry sweep's fact answers no then. A failed pass is logged, and the next tick tries again.
func (w *workflowRuntime) tickEntrySweep(ctx context.Context) {
	ticker := time.NewTicker(entrySweepPeriod(w.entrySweep))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-ticker.C:
			w.applyEntrySweep(ctx, at)
			if err := w.queueOwedDesignNews(ctx); err != nil {
				w.log.Warn("queue the reconcile of the news each tree is owed failed", "error", err)
			}
		}
	}
}

// applyEntrySweep applies one sweep, when one is owed. The fact itself reads nothing before it:
// every handler decides inside the fact's own transaction, and what the sweep judges is the
// daemon's own record. What runs before it is the gate — the very selections the sweep's pass
// acts on, asked as the one question this package has of them (admit's OwesEntryWork), in a
// read-only transaction of its own and outside the fact lock: whether any role of this project is
// owed the work of the entry its phases row names (the selection admit's resumeSlottedWorkers
// sends the work of), or, where none is, whether an arm of the stall reading holds for a slotted
// root's architect that no wake has told the controller yet (the wakes that pass sends after its
// sends). So a stall alone runs a sweep within a period of the arm holding, in a project where no
// other fact arrives, and once its wake is told the gate is shut again. The fact takes the one
// lock every fact takes and writes a row of processed_events, which nothing prunes, so the daemon
// asks before it applies one; and it asks the question only one piece of code answers, so no gate
// of its own can disagree with the pass and apply a fact a minute for work that pass would never
// do. A read that fails applies nothing and is logged: the next run asks again.
func (w *workflowRuntime) applyEntrySweep(ctx context.Context, at time.Time) {
	owed, err := w.owesEntryWork(ctx)
	if err != nil {
		w.log.Warn("read whether any role is owed the work of its phase entry failed", "error", err)
		return
	}
	if !owed {
		return
	}
	eventID := fmt.Sprintf("entry-sweep:%s:%s:%d", w.dispatchProject, w.bootID, at.UnixNano())
	if _, err := intake.ApplyFact(ctx, w.pool, "entry-sweep", eventID, intake.EntrySweep{}, w.handlers...); err != nil {
		w.log.Warn("apply the entry sweep failed", "error", err)
	}
}

// owesEntryWork is the sweep's gate read, in one short read-only transaction: admission's own
// answer to whether the pass the fact would make has anything to do — a role of the project owed
// the work of the entry its phases row names, or a stalled root architect no wake has told —
// decided over the project exactly as that pass decides it.
func (w *workflowRuntime) owesEntryWork(ctx context.Context) (bool, error) {
	var owed bool
	err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		owed, err = w.admission.OwesEntryWork(ctx, tx)
		return err
	})
	return owed, err
}

// promoteAfterHoldClears applies one synthetic DispatchConsumerPosition fact, its event id never
// reused, once nothing is held: Admission.Apply routes any DispatchConsumerPosition straight to
// release, which — with pending now provably empty — promotes exactly as a normal promote() would,
// picking up any candidate a held key's own recording left waiting behind it. The position's
// own values do not matter here: caughtUp is false whenever pending is empty, so release only
// reaches its own promoteHolds call, never re-applies a summary.
func (w *workflowRuntime) promoteAfterHoldClears(ctx context.Context) error {
	eventID := fmt.Sprintf("dispatch-position:%s:%s:closing:%d", w.dispatchProject, w.bootID, time.Now().UnixNano())
	_, err := intake.ApplyFact(ctx, w.pool, "dispatch", eventID, intake.DispatchConsumerPosition{}, w.handlers...)
	return err
}

// claimReady records an architect whose claim is ready, for releaseReadied to release the notices
// waiting for a later attempt that it owns (outbox.releaseWaiting): a relaunched architect is told
// what it missed at once, ahead of what follows. The ready route calls it before it answers the
// agent, so it only records the claim and wakes the release.
func (w *workflowRuntime) claimReady(c supervise.Claim) {
	if c.Role != claim.RoleArchitect {
		return
	}
	w.readied.add(c.Token)
}

// taskConfirmed applies the fact that a role's agent took the work the daemon gave it
// (supervise's Deps.TaskConfirmed): admission decides on it whether a root whose architect owed
// that task now waits on a person (workflow's ReconcileAwaiting), which otherwise waits for the
// next tick. The machine calls it inside its own decision, so the fact is applied on a goroutine
// of its own, bounded by readyReleaseTimeout; a failure is logged and not the daemon's end, since
// the tick decides the same thing again.
func (w *workflowRuntime) taskConfirmed(c supervise.Claim) {
	if c.Issue == "" || c.Role == claim.RoleController {
		return
	}
	go func() {
		// This goroutine runs beside the machine that started it and every other, so it takes a
		// hold mark of its own (store.ForConcurrentWorker).
		ctx, cancel := context.WithTimeout(store.ForConcurrentWorker(context.Background()), readyReleaseTimeout)
		defer cancel()
		fact := intake.TaskConfirmed{Issue: c.Issue, Role: c.Role, Generation: c.Generation}
		id := fmt.Sprintf("task-confirmed:%s:%d", c.Token, c.Generation)
		if p := c.Pending; p != nil {
			id = fmt.Sprintf("task-confirmed:%s:%s", c.Token, p.ID)
		}
		if _, err := intake.ApplyFact(ctx, w.pool, "supervise", id, fact, w.handlers...); err != nil {
			w.log.Warn("apply the confirmation of a task", "claim", c.Token, "error", err)
		}
	}()
}

// sessionLost applies the fact that a claim forgot its recorded session, for cause (supervise's
// Deps.SessionLost): the workflow tells the issue, its architect and the controller. The machine
// tells it at the forget, before it spawns the fresh agent that takes the session's place, with
// the claim as the forget left it, so c.Abandoned.Session is the session forgotten, which the fact
// carries. The event id names that session, so one forget of a session is one fact, however many
// launches forget it: the forget is in memory alone until the fresh agent's launch writes the
// claim, and a daemon stop between the two — a fresh spawn waiting on its pod, say — leaves the
// claim recording the session, which the next boot's launch forgets again at a generation of its
// own. That second fact is marked applied already (intake.ApplyFact), so nobody is told twice.
// Telling only after the claim is written would not do: the fact commits on a goroutine of its
// own (applySessionFact), so a stop between the write and the tell would tell nobody at all.
func (w *workflowRuntime) sessionLost(c supervise.Claim, cause supervise.SessionLoss, detail string) {
	w.applySessionFact(c, fmt.Sprintf("session-lost:%s:%s", c.Token, c.Abandoned.Session),
		intake.SessionLost{Issue: c.Issue, Role: c.Role, Detail: detail, Cause: cause, Session: c.Abandoned.Session},
		"apply a claim's lost session")
}

// sessionStoreFault applies the fact that a start could not reach the session store at all
// (supervise's Deps.SessionStoreFault): the workflow tells the issue and the controller. The
// machine decides how often this is called — at the first such start, then hourly — so each call
// is a report, and the event id names the moment rather than the launch.
func (w *workflowRuntime) sessionStoreFault(c supervise.Claim, detail string) {
	w.applySessionFact(c, fmt.Sprintf("session-store-fault:%s:%d", c.Token, time.Now().UnixNano()),
		intake.SessionStoreFault{Issue: c.Issue, Role: c.Role, Detail: detail}, "apply a session store fault")
}

// applySessionFact applies one of the two session facts on a goroutine of its own, bounded by
// readyReleaseTimeout: the machine calls both inside its own decision, so neither waits for the
// database. A failure is logged and is not the daemon's end — the report is a report, and the
// start it describes has already been decided. A claim on no issue, the daemon's own controller,
// has nobody to tell.
func (w *workflowRuntime) applySessionFact(c supervise.Claim, id string, fact intake.Fact, what string) {
	if c.Issue == "" || c.Role == claim.RoleController {
		return
	}
	go func() {
		// This goroutine runs beside the machine that started it and every other, so it takes a
		// hold mark of its own (store.ForConcurrentWorker).
		ctx, cancel := context.WithTimeout(store.ForConcurrentWorker(context.Background()), readyReleaseTimeout)
		defer cancel()
		if _, err := intake.ApplyFact(ctx, w.pool, "supervise", id, fact, w.handlers...); err != nil {
			w.log.Warn(what, "claim", c.Token, "error", err)
		}
	}()
}

// providerErrorRunReached tells the project's controller that a role's turns keep ending in a
// provider error (supervise's Deps.ProviderErrorRunReached): the machine counts the run and
// decides when to tell, and this names the notice kind, since supervise records nothing of
// internal/record. Admission's own check stops a second notice while one still waits in the
// outbox (TellController). The machine calls it inside its own decision, so the notice is
// queued on a goroutine of its own, bounded by readyReleaseTimeout; a failure is logged and is
// not the daemon's end, since the run goes on and the task is sent again either way.
func (w *workflowRuntime) providerErrorRunReached(c supervise.Claim, reason string) {
	if c.Issue == "" || w.admission == nil {
		return
	}
	notice := record.ControllerNotice{Kind: record.ProviderErrorsNotice, Role: c.Role, Reason: reason}
	go func() {
		// This goroutine runs beside the machine that started it and every other, so it takes a
		// hold mark of its own (store.ForConcurrentWorker). The notice is its own transaction, so
		// it owns the commit hook its write registers (projectRecords, store.OnCommit), as
		// ApplyFact does for a fact's rows: the controller's lane is woken once the notice commits.
		ctx, cancel := context.WithTimeout(store.ForConcurrentWorker(context.Background()), readyReleaseTimeout)
		defer cancel()
		if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
			return w.admission.TellController(ctx, tx, notice, c.Issue)
		}); err != nil {
			w.log.Warn("tell the controller of a run of provider errors", "claim", c.Token, "error", err)
		}
	}()
}

// releaseReadied releases, until ctx ends, the waiting notices of the architects claimReady
// recorded: each wake takes every architect recorded since the last one, in one release.
func (w *workflowRuntime) releaseReadied(ctx context.Context) {
	for {
		architects, ok := w.readied.take(ctx)
		if !ok {
			return
		}
		if len(architects) == 0 {
			continue
		}
		release, cancel := context.WithTimeout(ctx, readyReleaseTimeout)
		err := w.outbox.releaseWaiting(release, architects...)
		cancel()
		if err != nil && ctx.Err() == nil {
			w.log.Error("release the notices waiting for ready architects", "architects", architects, "error", err)
		}
	}
}

// closeNATS closes the workflow's Envoy NATS connection and nothing else, so run can close it at
// the stop's budget without waiting for the rest of stop. It is safe on a nil runtime, as stop is,
// and safe to call more than once and from any goroutine: nats.Conn.Close is idempotent, and the
// connection is read through an atomic pointer.
func (w *workflowRuntime) closeNATS() {
	if w == nil {
		return
	}
	if conn := w.conn.Load(); conn != nil {
		conn.Close()
	}
}

// stop closes the workflow's connections (closeNATS) and ends every token_command its token
// manager still runs (appauth.Manager.Close), waiting for each to exit, so a command waiting on a
// person's approval does not outlive the daemon.
func (w *workflowRuntime) stop() {
	if w == nil {
		return
	}
	w.closeNATS()
	if closer, ok := w.tokens.(interface{ Close() }); ok {
		closer.Close()
	}
}
