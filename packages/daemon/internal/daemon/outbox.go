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
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/dispatchblocks"
	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	// outboxDeliveryPrefix namespaces a delivery id the outbox mints from its row id, so a row's
	// id cannot collide with a minted one. What a delivery is for is its phase, not its id: the
	// id is rotated by a prompt retry.
	outboxDeliveryPrefix = "outbox:"
	// outboxLease is how long a claim holds a row, and the sweep's default interval
	// (sweepInterval, outbox_lanes.go): a running row renews its lease every third of it, so a
	// lease lapses only on a run that ended without its finish or retry recorded, and a sweep at
	// most this far apart finds the lapsed row within one more.
	outboxLease = 30 * time.Second
	// outboxSettleTimeout bounds a row's finish or retry once the daemon's stop has cancelled the
	// lane's context: the row's effect ran, and a settle that waits for the stop's context would
	// never be recorded, leaving the row to run again at the next boot as though it had not.
	outboxSettleTimeout = 5 * time.Second
	// heldWarnAfter is how long a due lifecycle row waits behind its issue's running lifecycle row
	// before the running row's lease renewal logs it: a start pulling an image holds the stop
	// behind it for a minute without anything being wrong, and longer than this an operator
	// should look at what the running row is doing.
	heldWarnAfter   = 2 * time.Minute
	messageReadSkew = 5 * time.Second
	// waitWarnAttempts is when a waiting row is logged as a warning: past the backoff's climb to
	// its cap, a wait of about two minutes, what it waits on is not ending on its own, and an
	// operator should look. A wait that ends on its own (routineWait) stays debug at every
	// attempt.
	waitWarnAttempts = 7
	// issueBranchTimeout bounds one issue_branch row's GitHub calls, its token's mint with them:
	// the row runs on its issue's lifecycle lane, one row at a time, so a call GitHub never
	// answers would hold the lane and every later lifecycle row of the issue behind it — the
	// starts that wait for the branch first of all.
	issueBranchTimeout = 30 * time.Second
)

// outbox runs each effect that the workflow transaction committed, one lane per issue and kind of
// work (outbox_lanes.go). Claiming and finishing have separate transactions so a slow external
// service never holds an issue transition lock.
type outbox struct {
	// pool is the daemon's guarded pool (store.Pool): each lane opens its transactions on it
	// under a hold mark of the lane's own (runLane), so a row's work that asks for a second
	// connection while its transaction holds one is refused at that call
	// (store.ErrNestedAcquire). A transaction whose body calls a transaction-owning trees method
	// opens on trees.Pool(), so that method joins it even in a rig whose tree store is separate.
	pool       *store.Pool
	records    record.Store
	dispatch   dispatch.Client
	notices    notify.Publisher
	catchUps   *catchUpPacing
	supervisor *supervisor
	trees      outboxTrees
	tokens     appauth.Tokens
	handlers   []intake.Handler
	// prepare readies Dispatch's answer to a landed status write of an issue the daemon records
	// nothing of, as that issue's event is readied (workflowRuntime.prepareObserved), before it is
	// applied as the daemon's own write (applyOwnWrite); nil applies the answer as it is. attach
	// sets it.
	prepare func(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error)
	project string
	// dispatchProject is the Dispatch project whose issues' rows this outbox claims: the database
	// is shared, and another project's rows are another daemon's.
	dispatchProject string
	stateDir        string
	// gatedSteps are the design-gate steps the project gates (gates.design), which a root
	// architect's catch-up this runner writes states (a child's catch-up states none) and whose
	// kept approval pair a root architect's start task names (workflow.QueueCatchUp). A test that
	// builds the runner by hand leaves it nil, which newOutbox reads as the shipped list, the one
	// step spec.
	gatedSteps []record.DesignStep
	log        *slog.Logger
	// backstop is how long after a release its one open phase's worker is woken whatever else
	// happens (config.IdleReleaseConfig.Backstop, `backstop_minutes`); zero where the deployment
	// releases no pod, and then a release arms none.
	backstop time.Duration
	// readPullRequest reads one pull request from GitHub (one GET /pulls/{number}) and answers the
	// implement App's client of its repository with it; applyMergeability applies what that read
	// says about merging the head, reading the base's rules with that client on a conflicting
	// answer, under the event id it is handed, and answers the fact it applied, which can be a
	// later GraphQL answer than the read's; and mergeStateWait is how long a read row waits
	// before it reads again. The two rows that read a pull request use them: the merge-queue read
	// (mergeQueueRead) and the second read of a conflicting head (conflictConfirm), whose every
	// read is bounded by issueRead. queueMergeStateFollow hands an issue's pull request, as the
	// row's own read of the record held it, to the merge-state reader to follow GitHub's merge
	// state for, which the second read does when its last attempt read no answer. All four are the
	// workflow runtime's own
	// (workflowRuntime.readPullRequest, workflowRuntime.applyMergeability,
	// workflowRuntime.mergeStateWait, workflowRuntime.queueMergeStateFollow), set by attach after
	// newOutbox, so the outbox reads GitHub and applies its answer as the per-issue read does, a
	// whole-daemon test cuts the wait through the runtime's own field (mergeStateRetry), and the
	// reads that follow the second read are the runtime's ladder's own. A test that builds the
	// outbox by hand leaves them nil, and a read row then fails with an error that names the
	// missing read.
	readPullRequest       func(context.Context, record.PullRequest) (githubrest.Client, githubPullRequest, error)
	applyMergeability     func(ctx context.Context, github githubrest.Client, pr record.PullRequest, pull githubPullRequest, readAt time.Time, eventID string) (intake.PullRequestMergeability, error)
	mergeStateWait        func() time.Duration
	queueMergeStateFollow func(issue string, follow *record.PullRequest)
	// podEnded is what the end of an issue's pod runs, for the issues whose pod ended: the drop of
	// a repository kept for each (workflowRuntime.dropLaunchedRepository) and this release's
	// one-time rename pass over them (workflowRuntime.podEndsOn, renamePass). It runs at two
	// places only, each once the runtime's own call has answered that the pod is gone: an idle
	// release's teardown, after ReleaseIssue returns and before the release's mark is cleared
	// (releaseIssue); and an issue close, after SuspendIssue returns nil on a close neither its
	// fence nor the store refused, or under a runtime that runs no pod (tmux) on a close its fence
	// did not refuse (suspendIssue, the record.IssueSuspend case of execute): each issue's own
	// close, since no close ends the pods of a tree (R17 of dispatch://INFRA-3320). Never after a
	// pod update's ReplacePod, which starts the next pod at once. Set by attach after newOutbox; a
	// test that builds the outbox by hand leaves it nil, and the end of a pod then runs nothing.
	podEnded func(context.Context, []string)
	// noteUnknownChain has R16's re-read read a key's Dispatch chain (admit.Admission's
	// NoteUnknownChain, which wakes rereadChains): a re-parent seed that waits on its parent's
	// chain notes that parent (seedReparent). attach sets it; nil, in an outbox a test builds by
	// hand, notes nothing.
	noteUnknownChain func(key string)
	// now is the moment a row is listed, claimed, renewed and retried at; clock arms the lanes'
	// timers — the sweep's, a lane's wait for its next due row, a running row's lease renewal.
	// Both are the wall clock in production, and a test holds both still (heldClock). A test that
	// builds the outbox by hand and only drains it (RunOnce) leaves clock nil, which is the wall
	// clock (timers).
	now   func() time.Time
	clock supervise.Clock
	// sweepEvery is how often Run sweeps the lanes (sweepInterval): zero is outboxLease, the
	// daemon's. A whole-daemon test that changes a row behind the daemon — a lease it lets lapse, a
	// backoff it makes due — sets it short through overrides.outboxSweep (attach), so the row runs
	// at the next sweep rather than up to outboxLease of the wall clock later.
	sweepEvery time.Duration
	provision  func(context.Context, workspace.Request) (workspace.Workspace, error)
	// remove removes a workspace from the daemon's host, handed its repository's store lock, which
	// it holds for every step on the shared clone and lets go before it deletes the workspace's
	// files (workspace.Remove).
	remove func(context.Context, workspace.Workspace, sync.Locker) error
	// checkedOut is whether a workspace on the daemon's host finished its checkout
	// (workspace.CheckedOut): a launch provisions one that did not (ensureWorkspace).
	checkedOut func(context.Context, workspace.Workspace) (bool, error)
	// provisions orders the workspace provisions and removals (provisionLocks): up to
	// provisionConcurrency checkouts at once, one at a time per issue, and one command at a time
	// on each repository's shared clone but for the checkouts — the clone's `jj git fetch`, its
	// git configuration and its bookmarks corrupt or refuse when two write at once. Lanes and
	// launches run issues side by side, so several issues' provisions are in flight together.
	provisions provisionLocks
	// githubAPI is the GitHub REST root a row's GitHub call is made under — an issue_branch row's
	// create: the workflow's (workflowRuntime.githubAPI), which empty, in production, is
	// https://api.github.com.
	githubAPI string
	// repositories is the daemon's view of the project's repositories, which the outbox reads for
	// the deployment's repository equivalence alone (`repository_renames`): which repository
	// GitHub holds a recorded one under now, for the token and the address of every GitHub call
	// it makes (serving). Nil in a test that configures neither, where a recorded repository is
	// served under its own name.
	repositories *repositories
	// privateNameRules is the deployment's private-name rule file as boot compiled it
	// (plan.privateNameRules), which every issue branch's create runs over the ref name before it
	// writes (ghbranch.Create), set by attach after newOutbox. Nil where the deployment names no
	// rule file, and in a test that builds the outbox by hand: the create then runs no
	// private-name rule.
	privateNameRules *leakguard.RuleSet
	// limits is every GitHub rate limit a row met, by the budget it exhausted, held until its
	// deadline: a row that spends one of those budgets waits for the latest deadline among them
	// (standingLimit) rather than meet a limit again. The lanes run rows on many goroutines, so
	// limitsMu guards it (deferral, noteLimit).
	limitsMu sync.Mutex
	limits   map[githubrest.Budget]rateLimit
	// hold answers whether a claimed row is put back unrun for now, due again outboxHoldRecheck
	// later (runRow): a test's seam alone (overrides.holdOutbox), nil in production and in every
	// outbox a test builds by hand, which holds nothing.
	hold func(record.OutboxRow) bool
	// statusRowChecked and statusRowRead are a test's seams in a status row's run
	// (overrides.statusRowChecked, overrides.statusRowRead): the first runs once the row's run and
	// phase entry have passed their check and before the row reads Dispatch, the second after
	// each read a write is fenced on, before the row waits for the record to see that read's
	// sequence, handed it. A test holds a row at either until it has moved the issue. nil, in
	// production and in every outbox a test builds by hand, runs nothing.
	statusRowChecked func(ctx context.Context, row record.OutboxRow)
	statusRowRead    func(ctx context.Context, row record.OutboxRow, seq int64)
	// seenWait is how long a status row's run waits for the record to see the sequence its read
	// gave before it reads the issue's event log instead (awaitSeen): statusSeenWait in
	// production, and zero in an outbox a test builds by hand, which reads the log at once.
	seenWait time.Duration
	// scopeTold is whether this daemon's run has queued the controller's status-scope notice, of
	// which it queues one for all three refusals it is told of (tellStatusScopeOnce).
	scopeTold atomic.Bool
	// refreshPause reads the project's open pause into the daemon's flag
	// (internal/pause's Service.Refresh) at each sweep (decision 18 of dispatch://INFRA-3759), so a
	// pause made or ended through another daemon of a rolling restart holds this daemon's launches
	// within one sweep. nil — a daemon with no pause service, a test's runner — reads nothing.
	refreshPause func(context.Context) error
	// unheldWarned is every claim publish has written its refusal's Warn line for since that
	// claim's hold last passed (a claim.Token), so a claim's held rows — one at each attempt of its
	// backoff, and several of them taking turns — write one line between them. The claim's next
	// publish the gate admits deletes it, so a later refusal is written again. It holds at most one
	// entry a claim the supervisor holds.
	unheldWarned sync.Map
	// controllerLaunched is `controller: daemon`: the daemon launches the project's controller, so
	// its claim holds the controller topic back (heldTopic). False is `controller: operator`.
	controllerLaunched bool
	// beforeUndeliveredEnd, where a test sets it, runs on a held message's row between the read
	// that ends the row undelivered - the workflow's rule of a merge-forward message
	// (mergeForwardMessage), a claim that failed or retired (deliverHeld), or the fence of a
	// merge-forward ask (lateMergeForwardAsk) - and the end (endUndelivered), so the test can make
	// something come in between. nil runs nothing, and no production path sets it.
	beforeUndeliveredEnd func(record.OutboxRow)
	// The lane registry (outbox_lanes.go). mu guards running, laneCtx and lanes; running is true
	// between Run's start and its stop, laneCtx is Run's context, which every lane runs under, and
	// lanes is each live lane by its key. wg counts the lane goroutines, so Run returns only once
	// every lane has.
	mu      sync.Mutex
	running bool
	laneCtx context.Context
	lanes   map[record.LaneKey]*outboxLane
	wg      sync.WaitGroup
}

// outboxTrees is the store's lifecycle barrier (store.Store) as the outbox runs it: an issue
// close's suspension and the cleanup of its own lifecycle that follows it, an idle release's own
// two decisions, the check of its workspace and the decline that check can record, a finished
// issue's drop of the record of its pods' disks, a stop's wait for a stored claim's machine, a
// delivery wake and the open resume that refuses one, a project pause's stop of a role and which
// pause stopped a role, and the end of a pause's hold of a pod a deletion took.
type outboxTrees interface {
	Pool() *store.Pool
	IssueSuspension(ctx context.Context, project string, closing store.IssueClose, outOfWorkflow func(status string) bool) (bool, error)
	ReserveIssueCleanup(ctx context.Context, project, issue string, generation uint64,
		inLegionsWork func(at phase.Phase, status string) bool) (treelifecycle.Lifecycle, bool, error)
	CleanupReservedIssue(ctx context.Context, project, issue string, epoch uint64, releaser store.IssueReleaser) error
	IssueRelease(ctx context.Context, project string, release store.IssueReleased, outOfWorkflow func(status string) bool) (store.ReleaseDecision, error)
	IssueReleaseStop(ctx context.Context, project, issue string, row int64) (store.ReleaseDecision, error)
	IssueReleaseCheck(ctx context.Context, project, issue string, row int64) (store.ReleaseDecision, error)
	ReleaseSuspendedClaim(ctx context.Context, project, issue string, role claim.Role, row int64) error
	CancelIssueRelease(ctx context.Context, project, issue string, resume store.ReleaseResumeRow, outOfWorkflow func(status string) bool,
		paused store.ReleaseCancelPause) (store.ReleaseCancelled, error)
	DeclineIssueRelease(ctx context.Context, project, issue string, decline store.IdleReleaseDecline, resume store.ReleaseResumeRow,
		outOfWorkflow func(status string) bool, paused store.ReleaseCancelPause) (store.ReleaseCancelled, error)
	ForgetIdleDecline(ctx context.Context, project, issue string) error
	DeclineLeftPodClose(ctx context.Context, project, issue string, decline store.IdleReleaseDecline) (bool, error)
	LeftPodTurned(ctx context.Context, project, issue string, turnStart time.Time) (bool, error)
	ReleaseIdleIssue(ctx context.Context, project, issue string, decide store.IdleReleaseDecide) (bool, error)
	ClearIssueRelease(ctx context.Context, project, issue string) error
	DeleteIssuePodDisks(ctx context.Context, issue string) error
	NoteReleaseTeardown(ctx context.Context, project, issue string, release int64, teardown store.ReleaseTeardown) error
	StoredClaimMayRun(ctx context.Context, token claim.Token, row int64) (bool, error)
	TakePodWake(ctx context.Context, project, issue string, token claim.Token, kind string, at time.Time, mark int64) (bool, error)
	OpenPodResume(ctx context.Context, project, issue string, role claim.Role) (id, pause int64, found bool, err error)
	CancelPodBackstopsOf(ctx context.Context, project, issue string, role claim.Role) error
	ReleasedClaimsIn(ctx context.Context, project, issue string) ([]supervise.Claim, error)
	ClaimStatesIn(ctx context.Context, project, issue string) (store.ClaimStates, error)
	ArmPodBackstop(ctx context.Context, backstop store.PodWakeBackstop) error
	PauseByID(ctx context.Context, id int64) (store.Pause, error)
	EndPauseStop(ctx context.Context, pause int64, token claim.Token, end store.PauseStopEnd, at time.Time) (bool, error)
	StoppedByAPause(ctx context.Context, project, issue string, token claim.Token) (int64, error)
	EndPausePodHold(ctx context.Context, project, issue string, end store.PausePodHoldEnd, at time.Time) (bool, error)
}

// newOutbox builds the outbox. controllerLaunched is `controller: daemon` (config.ControllerLaunch),
// which decides whether the launched controller claim holds the controller topic back (heldTopic).
func newOutbox(pool *store.Pool, records record.Store, client dispatch.Client, publisher notify.Publisher, supervisor *supervisor, trees outboxTrees, tokens appauth.Tokens, handlers []intake.Handler, gatedSteps []record.DesignStep, project, dispatchProject, stateDir, githubAPI string, repositories *repositories, tools map[string]string, controllerLaunched bool, log *slog.Logger) *outbox {
	if log == nil {
		log = slog.Default()
	}
	return &outbox{
		pool: pool, records: records, dispatch: client, notices: publisher, supervisor: supervisor, trees: trees, tokens: tokens,
		handlers: handlers, gatedSteps: gatedSteps, controllerLaunched: controllerLaunched,
		project: project, dispatchProject: dispatchProject, stateDir: stateDir, githubAPI: githubAPI, repositories: repositories,
		log: log, now: time.Now, clock: supervise.RealClock{}, seenWait: statusSeenWait,
		// WarmCodegraphIndexInBackground runs here, never in provisionWorkspace: every outbox
		// test injects its own `provision`, so only this production closure starts codegraph.
		provision: func(ctx context.Context, request workspace.Request) (workspace.Workspace, error) {
			provisioned, err := workspace.Provision(ctx, workspace.NewRunner(workspace.CommandTimeout, tools), request)
			if err != nil {
				return workspace.Workspace{}, err
			}
			workspace.WarmCodegraphIndexInBackground(procgroup.NoReaper, provisioned.Dir)
			return provisioned, nil
		},
		remove: func(ctx context.Context, working workspace.Workspace, store sync.Locker) error {
			return workspace.Remove(ctx, workspace.NewRunner(workspace.CommandTimeout, tools), working, store)
		},
		checkedOut: func(ctx context.Context, working workspace.Workspace) (bool, error) {
			return workspace.CheckedOut(ctx, workspace.NewRunner(workspace.CommandTimeout, tools), working)
		},
	}
}

// publish is every executor's publish to Envoy (r.notices), and publishKept its publish with keep,
// both held to the claims this daemon supervises (heldBack). A publish to the role topic of a
// claim the daemon supervises, or to its project's controller topic under `controller: daemon`
// (heldTopic), is made only while that claim holds the session it records
// (supervise.Machine.HoldsItsSession, plan E's t-e-54): a claim the boot could not suspend keeps
// its process, and that process keeps the Envoy role its session claimed, so a notice, a role
// message, a hook's prompt, an update ask, a merge-queue removal, a merge forward's message or a
// controller notice would reach a process working under another claim's session, or one no claim
// holds (review rounds 13 to 16 of #4220). Such a publish is not made and answers
// notify.ErrNoHolder, which every executor already answers its own way: a notice is held for its
// architect; a merge-queue removal, a merge forward's message, a controller notice, a hook's
// prompt or a phase-ended message inside its hour (publishKeptToRole) and a role message a
// project's pause still owes its role (heldMessageOwed) wait; and any other role message and an
// update ask finish undelivered.
// So an operator's settle of the name lets a waiting row's next attempt through, while a row that
// finished undelivered is not sent again (review round 13 of #4220, note 36). A store that does
// not answer publishes nothing and fails the attempt, which the row's backoff retries.
//
// HoldsItsSession takes no machine lock, so no publish waits out a decision in flight, a launch or
// relaunch in the runtime included (LEGION-650). The gate is here rather than in the publisher
// newOutbox is given, so every executor, and every test that builds an outbox, publishes through
// it. A kept publish is held as much as any other: its copy waits on the role's kept lane for the
// role's next live holder, and while the unheld session's process lives, that holder is it. A
// claim's role topics are held whatever its state: a retired or failed claim's unheld session may
// still be a live process the daemon no longer manages, and the Envoy listener does not yet
// withhold the role from it, so the daemon withholds its own messages. A topic of no claim this
// daemon supervises, and every other topic, is published as made.
func (r *outbox) publish(ctx context.Context, topic, message string, payload any, dedupeKey string) error {
	_, err := r.publishTo(ctx, topic, message, payload, dedupeKey)
	return err
}

// publishTo is publish answering the session the listener stored the message for, "" where its
// answer names none (notify.Publisher.Publish): the one evidence of who was told at the write, which
// a held message bound to its role's launch reads (deliverHeld).
func (r *outbox) publishTo(ctx context.Context, topic, message string, payload any, dedupeKey string) (string, error) {
	if err := r.heldBack(ctx, topic, dedupeKey); err != nil {
		return "", err
	}
	return r.notices.Publish(ctx, topic, message, payload, dedupeKey)
}

// publishKept is publish with keep (notify.Publisher.PublishKept), the publish phaseEnded and
// hookPrompt share through publishKeptToRole, held as publish is: a topic heldBack refuses is not
// published, kept or not, and answers the gate's notify.ErrNoHolder.
func (r *outbox) publishKept(ctx context.Context, topic, message string, payload any, dedupeKey string) (bool, error) {
	if err := r.heldBack(ctx, topic, dedupeKey); err != nil {
		return false, err
	}
	return r.notices.PublishKept(ctx, topic, message, payload, dedupeKey)
}

// heldBack is the gate publish and publishKept share: nil when topic may be published to, and
// otherwise why not.
func (r *outbox) heldBack(ctx context.Context, topic, dedupeKey string) error {
	if token, held := r.heldTopic(topic); held && r.supervisor != nil {
		if m, ok := r.supervisor.Machine(token); ok {
			if err := m.HoldsItsSession(ctx); err != nil {
				var refusal claim.Refusal
				if !errors.As(err, &refusal) {
					return fmt.Errorf("ask whether %s holds its session before publishing to it: %w", token, err)
				}
				// One line per claim while its hold refuses, as holdRecorded writes one per
				// refusal (warnRefusal): a held row asks again at each attempt of its backoff,
				// and several rows of one claim take turns (outbox.unheldWarned).
				if _, seen := r.unheldWarned.LoadOrStore(token, true); !seen {
					log := r.log
					if log == nil {
						log = slog.Default()
					}
					log.Warn("outbox: not publishing to a claim that records a session it does not hold",
						"topic", topic, "claim", token, "key", dedupeKey, "error", err)
				}
				return fmt.Errorf("%w: %s records a session it does not hold, so nothing is published to it: %w", notify.ErrNoHolder, token, err)
			}
			r.unheldWarned.Delete(token)
		}
	}
	return nil
}

// heldTopic is the claim whose session a publish to topic is held to (publish): the claim a role
// topic names, or, under `controller: daemon` alone, the project's controller claim for its
// controller topic. Under `controller: operator` the controller topic is the operator's own
// controller's, so no claim the daemon launched holds it back, a launched controller an earlier
// boot retired included (stopLaunchedController); under `controller: daemon` the launched
// controller claim is the deployment's controller, since one runs per project.
func (r *outbox) heldTopic(topic string) (claim.Token, bool) {
	if role, isRole := strings.CutPrefix(topic, notify.RoleTopicPrefix); isRole {
		return claim.Token(role), true
	}
	if r.controllerLaunched && r.project != "" && topic == notify.ControllerTopic(r.project) {
		return claim.ControllerToken(r.project), true
	}
	return "", false
}

// RunOnce drains every row of the project due now, in the order the lanes would run them within
// one issue (next_at, then id), on the caller's goroutine: the tests' synchronous drain. Production
// runs the lanes (Run, outbox_lanes.go), which this never starts. Each row is claimed by id as its
// turn comes (runRow), so a row another runner has leased since the list is skipped; and the list
// is made once, so a row that a listed row's finish makes claimable — an issue's second status
// write, once its first is done — runs on the next drain, as it did when the drain claimed a batch.
func (r *outbox) RunOnce(ctx context.Context) error {
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.pool == nil || r.records == nil {
		return errors.New("outbox requires Postgres and its record store")
	}
	rows, err := r.listDue(ctx, "", nil, r.now().UTC())
	if err != nil {
		return fmt.Errorf("list due outbox rows: %w", err)
	}
	for _, row := range rows {
		// A claim that fails is logged by runRow and its row left for the next drain, as a lane leaves
		// it for the next wake or sweep.
		_ = r.runRow(ctx, row)
	}
	return nil
}

// outboxErrorInFull is the message of the lines that carry a failed run's whole error, in parts
// (settle, storetext.LogWhole), where the row and every other line about it keep only its start.
const outboxErrorInFull = "outbox row's error in full; the row and the lines about it keep its start"

// settle records what a row's run came to, under the lease the run held: a wait or a failure is
// retried on the row's backoff (or after the wait GitHub asks for), a failure no retry repairs is
// finished refused with its follow-up (permanentRefusal, finishRefused), and anything else is
// finished. A row whose finish or retry fails is logged and left leased: its lease expires and the
// row is due again, so one row's failed transaction never stops the rows after it.
//
// A run's error is a far side's text, which can carry a relayed command's whole stderr. Every
// line about it carries it as the row keeps it (storetext.FarSideError, record.Store.RetryOutbox),
// this function's own and finishRefused's alike, so no line holds more than a log entry takes.
// This is the one place the whole text is logged (storetext.LogWhole), once for each failed
// attempt and only when storetext.FarSideError cuts it: every failed attempt passes here once,
// retried or finished refused, and the whole text is in the log even where the retry's or the
// finish's own write then fails.
func (r *outbox) settle(ctx context.Context, row record.OutboxRow, err error) {
	if err != nil {
		r.noteLimit(row, err)
		kept := storetext.FarSideError(err.Error())
		// A failure no retry repairs is not logged as the row failing: once its finish commits,
		// finishRefused's own line says what became of the row, and a finish that does not commit
		// is logged below with it as the cause.
		permanent := permanentRefusal(row, err)
		if errors.Is(err, wait.ErrWaiting) {
			// A wait is not a failure (wait.ErrWaiting): a held notice, a suspension or start held
			// for a turn, a task behind the claim's pending delivery, a tree's cleanup reservation,
			// a close waiting for its stops, a claim release Dispatch has yet to allow, or a child's
			// architect's first start waiting for its tree to run again. The row runs again on its
			// backoff, logged when it first waits and as a warning once it has waited long, unless
			// the wait ends on its own.
			level := slog.LevelDebug
			switch {
			case row.Attempts == 0:
				level = slog.LevelInfo
			case row.Attempts >= waitWarnAttempts && !routineWait(err):
				level = slog.LevelWarn
			}
			r.log.Log(ctx, level, "outbox row waits", "row", row.ID, "kind", row.Kind, "issue", row.Issue,
				"attempts", row.Attempts, "error", kept)
		} else if !permanent {
			r.log.Error("outbox row failed", "row", row.ID, "kind", row.Kind, "error", kept)
		}
		storetext.LogWhole(r.log, outboxErrorInFull, err.Error(), "row", row.ID, "kind", row.Kind, "issue", row.Issue)
		if permanent {
			if finishErr := r.finishRefused(ctx, row, err); finishErr != nil {
				r.log.Error("outbox row finish not recorded; it runs again when its lease expires", "row", row.ID, "error", finishErr, "cause", kept)
			}
			return
		}
		if err := r.retry(ctx, row, err); err != nil {
			r.log.Error("outbox row retry not recorded; it runs again when its lease expires", "row", row.ID, "error", err)
		}
		return
	}
	if err := r.finish(ctx, row); err != nil {
		r.log.Error("outbox row finish not recorded; it runs again when its lease expires", "row", row.ID, "error", err)
	}
}

// deferral is the GitHub rate limit a row of kind would meet at now, if one stands (standingLimit),
// read under limitsMu: the lanes run rows on many goroutines, and a limit one row met holds every
// row of the kinds that spend its budget, whichever lane runs them.
func (r *outbox) deferral(kind record.OutboxKind, now time.Time) (rateLimit, bool) {
	r.limitsMu.Lock()
	defer r.limitsMu.Unlock()
	return standingLimit(r.limits, kind, now)
}

// noteLimit records the GitHub rate limit err carries, when it does, under the budget it exhausted:
// the latest deadline for a budget wins.
func (r *outbox) noteLimit(row record.OutboxRow, err error) {
	after, budget := githubrest.RetryAfter(err)
	if after <= 0 {
		return
	}
	until := r.now().UTC().Add(after)
	r.limitsMu.Lock()
	defer r.limitsMu.Unlock()
	if r.limits == nil {
		r.limits = map[githubrest.Budget]rateLimit{}
	}
	if held, ok := r.limits[budget]; !ok || until.After(held.until) {
		r.limits[budget] = rateLimit{row: row.ID, err: err, until: until}
	}
}

func (r *outbox) finish(ctx context.Context, row record.OutboxRow) error {
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.FinishOutbox(ctx, tx, row.ID, row.LeaseToken)
	}); err != nil {
		return fmt.Errorf("finish outbox row %d: %w", row.ID, err)
	}
	r.wakeWaitingChildren(ctx, row)
	return nil
}

// wakeWaitingChildren wakes the board lane of each issue whose write into work outboxInOrder held
// behind row, a dispatch_status row that has just finished (record.WaitingChildStatusIssues), so
// the child's row runs now rather than at the next sweep: a held row takes no lease and its lane
// exits with nothing due, and nothing else wakes it but a drop of the issue's status writes, whose
// commit wakes the same lanes (projectRecords.DropStatusWrites). It reads once the finish has
// committed, so a child row committed before the read is woken here, and one committed after it is
// listed by its own lane against a board that no longer holds row. A read that fails is logged and
// the sweep wakes the lane within sweepInterval. While no lane runs (RunOnce) nothing is read: a
// wake would reach none.
func (r *outbox) wakeWaitingChildren(ctx context.Context, row record.OutboxRow) {
	if row.Kind != record.OutboxKindDispatchStatus {
		return
	}
	r.mu.Lock()
	running := r.running
	r.mu.Unlock()
	if !running {
		return
	}
	var children []string
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		children, err = record.WaitingChildStatusIssues(ctx, tx, row.Issue)
		return err
	}); err != nil {
		if ctx.Err() == nil {
			r.log.Warn("outbox could not read the status rows held behind a finished status row of their parent; the sweep wakes their lanes",
				"row", row.ID, "issue", row.Issue, "error", err)
		}
		return
	}
	for _, child := range children {
		r.wake(child, record.OutboxKindDispatchStatus)
	}
}

// finishRefused finishes a row whose failure no retry repairs (permanentRefusal), its follow-up in
// the same transaction and only while the row's lease still holds (FinishLeasedOutbox), so a worker
// whose lease expired commits nothing beside the one that holds the row now.
//
// A status row Dispatch refused finishes so it no longer holds its issue's later writes back. Its
// reason and its status are two writes, and the one the issue's lifecycle stands on is the
// status, so a reason Dispatch refuses for good no longer costs the issue its status: the
// executor writes the status after that refusal and carries the refusal back with the writes
// that stand (statusWriteFailure), and the row only reaches this finish once the status stands
// or Dispatch refused the status itself. Dispatch merely failing the status write is no refusal,
// so the row retries on its own backoff and nothing here runs. The architect is told what
// Dispatch refused and which of the two writes stand (the kind's own refused rule), since
// nothing else would say that the reason is on no issue, or that Dispatch and the daemon's own
// record hold different statuses from here on. A message row Dispatch refused tells the architect
// that owns the issue (noticeArchitect) what was not posted and Dispatch's own refusal
// (messagePostRefused), since nothing else would: the message is the daemon's only word on the
// issue for what it carries. A hook prompt that ends undelivered (keptRoleMessageEnded) is named
// in the message on the issue for its role's undelivered prompts, one held for an hour from the
// first of them (tellHookPromptUndelivered), since their words reach no session otherwise. Each
// of those rows is written in the finish's own transaction, which owns the commit hook that write
// registers (projectRecords, store.OnCommit), so the notice's lane is woken as the finish commits
// and the notice is published without a sweep. No refused row's finish writes anything a fact
// reads, so it runs in a plain transaction (store.Transact) and waits behind no fact. Each line
// says the refusal as the row and the notice keep it (storetext.FarSideError); settle logged it
// whole.
func (r *outbox) finishRefused(ctx context.Context, row record.OutboxRow, cause error) error {
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		return fmt.Errorf("decode outbox row %d: %w", row.ID, err)
	}
	kept := storetext.FarSideError(cause.Error())
	// follow is what the transaction does beside the finish, handed the transaction's ctx, and told
	// the line logged once it commits, as its hook.
	var follow func(context.Context, pgx.Tx) error
	var told func()
	switch value := payload.(type) {
	case record.StatusWrite:
		refusal, _ := dispatch.PermanentRefusal(cause)
		r.log.Error("outbox status write refused by Dispatch and dropped; the issue's later writes go on",
			"row", row.ID, "issue", row.Issue, "status", refusal.Status, "code", refusal.Code, "error", kept)
		rule, err := ruleFor(value.Kind)
		if err != nil {
			return fmt.Errorf("outbox row %d: %w", row.ID, err)
		}
		follow, told = rule.refused(r, row, value, cause)
	case record.MessagePost:
		follow = func(ctx context.Context, tx pgx.Tx) error {
			return record.EnqueueFenced(ctx, tx, r.records, row.Issue, messagePostRefused(row.Issue, value, cause), r.now().UTC())
		}
		told = func() {
			r.log.Error("outbox Dispatch message refused and dropped; its architect is told", "row", row.ID, "issue", row.Issue, "error", kept)
		}
	case record.ClaimRelease:
		// Nobody is told: the claim stands where it was, which is the state the daemon would
		// have left behind had the row never run, and the issue's tree is over. The line is
		// what says the row ended, since without it a 404 — an issue Dispatch no longer has —
		// would end it in silence.
		refusal, _ := dispatch.PermanentRefusal(cause)
		told = func() {
			r.log.Warn("outbox claim release refused by Dispatch and dropped", "row", row.ID, "issue", row.Issue,
				"status", refusal.Status, "code", refusal.Code, "error", kept)
		}
	case record.DesignGatesPublish:
		// Nobody is told: the one refusal that ends this row is an issue Dispatch does not know
		// (permanentRefusal, ISSUE_NOT_FOUND), and no architect of such an issue is there to
		// hear it. The line is what says the row ended, since without it the drop would be
		// silent, and it names what Dispatch is left holding: whatever set it last took for
		// that issue, which its write advice goes on reading.
		refusal, _ := dispatch.PermanentRefusal(cause)
		told = func() {
			r.log.Warn("outbox design-gates publish refused by Dispatch and dropped; the set Dispatch holds for the issue is the last one it took",
				"row", row.ID, "issue", row.Issue, "status", refusal.Status, "code", refusal.Code, "error", kept)
		}
	case record.RoleHoldersPublish:
		// Nobody is told, and the line is what says the row ended. Two refusals end it
		// (permanentRefusal): an issue Dispatch does not know, a WARN, since nothing of this
		// daemon is wrong; and a 400 that refuses the set, an ERROR, since this daemon read a set
		// from its own claims that Dispatch will never take. The issue's next ready, end or other
		// change of a claim queues a fresh publication of the set (roleHoldersChanged, the
		// store's putClaim).
		// permanentRefusal ended the row only on a coded *dispatch.Error, so refusal is set.
		var refusal *dispatch.Error
		errors.As(cause, &refusal)
		log := r.log.Warn
		if refusal.Status == http.StatusBadRequest {
			log = r.log.Error
		}
		told = func() {
			log("outbox role-holders publish refused by Dispatch and dropped; the set Dispatch holds for the issue is the last one it took, and the next ready or end of a claim on the issue publishes again",
				"row", row.ID, "issue", row.Issue, "status", refusal.Status, "code", refusal.Code, "error", kept)
		}
	case record.StepRecord:
		// Nobody is told: no architect can change an issue Dispatch does not have, a body it will
		// never take, a writer its route refuses or an id of another issue, and the work the
		// record is of went on without it. The line stands for two refusals. Where Dispatch
		// refused the record's own write, the line is what says the record is not in Dispatch,
		// since nothing else would. Where it refused the variant's write after it took the record
		// (publishStepExperiment), the record stays in Dispatch with no variant. The line's error
		// tells the two apart: only the variant's names "write the variant of the step record".
		told = r.recordDropped(row, "step", value.ID, cause)
	case record.PlanRecord:
		// Nobody is told, for the step record's reasons: the plan is history of a run that went
		// on without it, and the line is what says Dispatch does not hold it.
		told = r.recordDropped(row, "plan", value.ID, cause)
	case record.HookPrompt:
		// The issue is told, since the prompt's words reach nobody otherwise, in one message for
		// each role and each hour (tellHookPromptUndelivered): the first prompt of the role that
		// ends undelivered writes a message held to the end of hookPromptMessageWindow, and every
		// later one of that role on the issue that ends before then is added to it, so a role that
		// is not coming posts one message an hour rather than one per prompt. The prompt that
		// opens the message also tells the issue's architect, once a message. The message row and
		// the notice are written in the finish's own transaction, so the prompt ends and the
		// message names it together, or neither.
		// permanentRefusal ended a hook prompt's row only on a *keptRoleMessageUndelivered, so
		// undelivered is set.
		var undelivered *keptRoleMessageUndelivered
		errors.As(cause, &undelivered)
		var posts time.Time
		follow = func(ctx context.Context, tx pgx.Tx) error {
			var err error
			posts, err = r.tellHookPromptUndelivered(ctx, tx, row.Issue, value, undelivered.why)
			return err
		}
		told = func() {
			r.log.Warn("outbox hook prompt finished undelivered; the issue's message names it", "row", row.ID, "issue", row.Issue,
				"hook", value.Hook, "role", value.Role, "message_posts_at", posts, "error", kept)
		}
	}
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		if follow != nil {
			if err := follow(ctx, tx); err != nil {
				return err
			}
		}
		if err := r.records.FinishLeasedOutbox(ctx, tx, row.ID, row.LeaseToken); err != nil {
			return err
		}
		if told != nil {
			store.OnCommit(ctx, told)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("finish outbox row %d: %w", row.ID, err)
	}
	r.wakeWaitingChildren(ctx, row)
	return nil
}

// recordDropped is the one line that says Dispatch refused a record of the record lane for good
// and its row dropped it: kind is the record's kind, step or plan, which names the record in the
// message and is the log key its id goes under. The refusal is read as Dispatch's own error, not
// through dispatch.PermanentRefusal, which answers nothing for a 403 such as STEP_WRITER;
// recordRefused ended the row only on a *dispatch.Error, so the refusal is set.
func (r *outbox) recordDropped(row record.OutboxRow, kind, id string, cause error) func() {
	var refusal *dispatch.Error
	errors.As(cause, &refusal)
	return func() {
		r.log.Warn("outbox "+kind+" record refused by Dispatch and dropped", "row", row.ID, "issue", row.Issue,
			kind, id, "status", refusal.Status, "code", refusal.Code, "error", cause)
	}
}

// messageRefusedPreview is how much of a refused message's first line its notice quotes: enough to
// know which message it was, short enough that the notice is one readable paragraph whatever the
// message was.
const messageRefusedPreview = 200

// messagePostRefused is the architect's notice of a Dispatch message the daemon could not post:
// Dispatch refused the body the way it would refuse every attempt (dispatch.PermanentRefusal), a
// block over its own bound among them, so the row is finished and the message is on no issue.
// Nothing else would tell anyone, since the message is the daemon's only word on the issue for
// what it carries. The reason names the issue, how long the message is, its first line (a READY
// packet opens with its own `READY #<n> at <sha>` line) and Dispatch's own refusal, bounded as a
// far side's error text is (storetext.FarSideError).
func messagePostRefused(issue string, payload record.MessagePost, cause error) record.Notice {
	return record.Notice{Kind: "message-refused", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("Dispatch refused the message Legion was posting on %s, so it is on no issue and the daemon will not post it again. The message is %d characters and begins %q. Dispatch answered: %s",
			issue, utf8.RuneCountInString(payload.Body), refusedPreview(payload.Body), storetext.FarSideError(cause.Error()))}
}

// refusedPreview is the first line of a body a notice quotes, cut to messageRefusedPreview.
func refusedPreview(body string) string {
	first, _, _ := strings.Cut(body, "\n")
	if runes := []rune(first); len(runes) > messageRefusedPreview {
		return string(runes[:messageRefusedPreview]) + "…"
	}
	return first
}

// statusWrites is which of a status row's two writes — the reason posted beside the status, and
// the status itself — stand when the row's run ends. Only that run knows: nothing the finish can
// read afterwards holds it, since the reason is one message among the issue's and the status
// Dispatch holds says nothing about which write set it. The architect's notice asks something
// else of the architect in each outcome (statusWriteRefused), so a run that ends with a refusal
// carries both facts back on it (statusWriteFailure).
type statusWrites struct {
	// status is whether the status this row writes is the status Dispatch holds. A row whose
	// reason Dispatch refused for good writes the status all the same and counts it set, and so
	// does a run that finds the issue already at that status: either way no status write of this
	// row's is left to make.
	status bool
	// reason is whether the reason the row posts beside the status is on the issue. It is false
	// for a row that carries no reason, whose notice says nothing of one.
	reason bool
}

// statusWriteFailure is the failure that ended a status row's run, carried back by status()
// together with the writes that stand (statusWrites). permanentRefusal reads Dispatch's own
// refusal through it, so a refusal is finished as any permanent refusal is and anything else
// retries, both exactly as they would without the wrapper. A failure before either write — the
// issue read that opens the run — is returned as it is, and finishRefused reads it as a run
// whose writes both still stand to be made.
type statusWriteFailure struct {
	err   error
	wrote statusWrites
}

func (e *statusWriteFailure) Error() string { return e.err.Error() }

func (e *statusWriteFailure) Unwrap() error { return e.err }

// statusWriteRefused is the architect's notice of a status row Dispatch refused: what it refused,
// and which of the row's two writes stand (wrote, which finishRefused reads off the failure).
// Each outcome leaves the architect something else to do. A reason refused beside a status that
// is set leaves the daemon's only word on why the issue ended on no issue, so the notice says how
// long that reason was and how it began, and the architect posts what a person still needs
// itself. A status Dispatch refused leaves the issue at the status it had while the daemon's own
// record reads the one the row wrote, which only a person's move on the issue settles, and the
// reason beside it may be posted already, which nobody should post a second time.
func statusWriteRefused(issue string, payload record.StatusWrite, cause error, wrote statusWrites) record.Notice {
	told := fmt.Sprintf("Dispatch refused a write Legion was making on %s and the daemon will not make it again. Dispatch answered: %s.", issue, storetext.FarSideError(cause.Error()))
	if wrote.status {
		told += fmt.Sprintf(" The status %s was set on the issue all the same.", payload.Status)
	} else {
		told += fmt.Sprintf(" The status %s is not set, so the issue keeps the status it has while the daemon's own record reads %s: the two disagree, and settling that is yours — ask a person with `dispatch ask` to set the status on the issue.",
			payload.Status, payload.Status)
	}
	if payload.Reason != "" {
		if wrote.reason {
			told += " The reason the write posts beside the status is on the issue already, so no one need post it again."
		} else {
			told += fmt.Sprintf(" The reason the write posts beside the status is on no issue. It is %d characters and begins %q; post what a person still needs yourself.",
				utf8.RuneCountInString(payload.Reason), refusedPreview(payload.Reason))
		}
	}
	return record.Notice{Kind: "message-refused", Role: claim.RoleArchitect, Reason: told}
}

// statusRefusalTold is the architect's notice of a status row Dispatch refused for good, which
// every kind makes: each leaves the architect to settle what the issue holds now
// (statusWriteRefused). Which of the row's two writes stand is known only to the run that made
// them, so the status executor carries that back on the failure it ends with
// (statusWriteFailure); a cause carrying none ended the run before either write — the issue read
// that opens it — and leaves both still to be made.
func statusRefusalTold(r *outbox, row record.OutboxRow, payload record.StatusWrite, cause error) (func(context.Context, pgx.Tx) error, func()) {
	var failure *statusWriteFailure
	var wrote statusWrites
	if errors.As(cause, &failure) {
		wrote = failure.wrote
	}
	follow := func(ctx context.Context, tx pgx.Tx) error {
		return record.EnqueueFenced(ctx, tx, r.records, row.Issue, statusWriteRefused(row.Issue, payload, cause, wrote), r.now().UTC())
	}
	told := func() {
		r.log.Error("outbox status write refused by Dispatch; its architect is told what the issue holds now",
			"row", row.ID, "issue", row.Issue, "status", payload.Status,
			"status_set", wrote.status, "reason_posted", wrote.reason, "error", storetext.FarSideError(cause.Error()))
	}
	return follow, told
}

// retry returns row to the outbox, due again after its backoff, or after the wait GitHub asks for
// when cause carries GitHub's rate limit and asks for longer, or at the moment cause names when it
// is a wait that knows when it ends (wait.Until) and that moment is still ahead: a production
// check's look that began before a pause ended names its bound, and the row runs again then rather
// than climbing its backoff for days. A named moment already past is the backoff's.
func (r *outbox) retry(ctx context.Context, row record.OutboxRow, cause error) error {
	now := r.now().UTC()
	delay := record.RetryBackoff(row.Attempts)
	if after, _ := githubrest.RetryAfter(cause); after > delay {
		delay = after
	}
	nextAt := now.Add(delay)
	if at, ok := wait.UntilAt(cause); ok && at.After(now) {
		nextAt = at.UTC()
		r.log.Debug("outbox row waits until", "row", row.ID, "kind", row.Kind, "issue", row.Issue, "until", nextAt)
	}
	return r.retryAt(ctx, row, nextAt, cause)
}

// retryAt returns row to the outbox, due again at nextAt, with cause as its last error. The row
// keeps cause's text as record.Store.RetryOutbox stores it (storetext.FarSideError). The whole
// text is logged where the failed attempt is settled (settle), not here: a deferral behind
// another row's GitHub rate limit, the one other caller (outbox_lanes.go), made no attempt, and
// the error its cause wraps is the limited row's own, which that row's settle logged.
func (r *outbox) retryAt(ctx context.Context, row record.OutboxRow, nextAt time.Time, cause error) error {
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.RetryOutbox(ctx, tx, row.ID, row.LeaseToken, nextAt, cause.Error())
	}); err != nil {
		return fmt.Errorf("retry outbox row %d: %w", row.ID, err)
	}
	return nil
}

// rateLimit is a GitHub rate limit a row met: the row, its error, and when the limit ends.
type rateLimit struct {
	row   int64
	err   error
	until time.Time
}

// githubBudgets is the GitHub budgets a row of each kind spends, as the implement App: its REST
// calls (an issue branch's create) the REST core's, and every call GitHub's secondary limit. A
// row of a kind it does not name is never deferred for a limit another row met, and every such
// kind but two calls no GitHub API. The two are the rows that read one pull request, the
// merge-queue read (record.MergeQueueRead, mergeQueueRead) and the second read of a conflicting
// head (record.ConflictConfirm, conflictConfirm), which stay out of this map on purpose: a
// deferral is a retry, which spends one of the read's mergeStateLadderReads attempts with no read
// made. A read GitHub refuses for its rate limit is a failed read instead, and the next read
// waits for the longer of the runtime's wait and GitHub's Retry-After. The read of the base's
// rules either makes on a conflicting answer (readBaseRules) is no exception: a rate limit there
// reads as rules the daemon could not read.
var githubBudgets = map[record.OutboxKind][]githubrest.Budget{
	record.OutboxKindIssueBranch: {githubrest.BudgetCore, githubrest.BudgetSecondary},
}

// standingLimit is the limit in limits, among the budgets a row of kind spends, that still stands
// at now and ends last, and whether there is one: that row would meet a limit until then.
func standingLimit(limits map[githubrest.Budget]rateLimit, kind record.OutboxKind, now time.Time) (rateLimit, bool) {
	var latest rateLimit
	for _, budget := range githubBudgets[kind] {
		if limit, ok := limits[budget]; ok && limit.until.After(now) && limit.until.After(latest.until) {
			latest = limit
		}
	}
	return latest, !latest.until.IsZero()
}

// permanentRefusal is a row's failure no retry repairs, so the row is finished with its follow-up
// (finishRefused) rather than retried: a Dispatch status write or message post Dispatch refuses
// the same way every time (dispatch.PermanentRefusal) — a refused status would hold every later
// status of its issue behind it, and a refused post, a block over Dispatch's own bound among
// them, would retry on its backoff for ever and tell nobody. A claim release is the shared
// Dispatch rule whole: its executor answers both of Dispatch's 409s as waits of its own before
// this is reached (claimHeldWait, claimContendedWait), and each wraps no *dispatch.Error on
// purpose, because this is computed before settle tests wait.ErrWaiting and a wait that let the
// refusal through here would be finished rather than retried. A 404, and every other coded 4xx,
// stay permanent and end the row. A step record and a plan record end on exactly the refusals
// their routes make for good (recordRefused), so a credential answer retries. The router's 404 of a
// Dispatch that has no route for a record's or the role holders' write never reaches this: execute
// makes it a wait (waitsForTheRoute), which wraps nothing of it. For every other kind it is the
// coded 404 it says.
func permanentRefusal(row record.OutboxRow, err error) bool {
	switch row.Kind {
	case record.OutboxKindDispatchStatus, record.OutboxKindDispatchMessage, record.OutboxKindClaimRelease:
		_, permanent := dispatch.PermanentRefusal(err)
		return permanent
	case record.OutboxKindDesignGatesPublish:
		// Permanent on the route's own 404 ISSUE_NOT_FOUND alone: an issue Dispatch does not know
		// is one it will not know on the retry either, and nothing an operator does here would
		// land that set. dispatch.PermanentRefusal is deliberately not used, because it calls
		// every coded 4xx permanent, which would end every publication against a Dispatch older
		// than the route: that server's router answers the unknown path 404 under the code
		// NOT_FOUND, a coded refusal that is not this one. It, a 5xx and a transport failure all
		// end on their own, and the set is sent again when they do.
		var refusal *dispatch.Error
		return errors.As(err, &refusal) && refusal.Status == http.StatusNotFound && refusal.Code == dispatch.IssueNotFound
	case record.OutboxKindRoleHoldersPublish:
		// The design-gates rule, and a 400 that carries Dispatch's code besides: the route refuses
		// a set it will never store (INVALID_ROLE_HOLDER), and the set is read from the same claims
		// on the retry, so a retry would send it for as long as the issue lives. A 400 with no code
		// is from whatever stands in front of Dispatch, and it is retried as an outage.
		var refusal *dispatch.Error
		if !errors.As(err, &refusal) || refusal.Code == "" {
			return false
		}
		return refusal.Status == http.StatusBadRequest || (refusal.Status == http.StatusNotFound && refusal.Code == dispatch.IssueNotFound)
	case record.OutboxKindStepRecord, record.OutboxKindPlanRecord:
		return recordRefused(err)
	case record.OutboxKindHookPrompt:
		// A prompt whose role will not hold its topic in time (keptRoleMessageEnded): a retry
		// meets the same answer, and the finish posts the message on the issue that says so.
		var undelivered *keptRoleMessageUndelivered
		return errors.As(err, &undelivered)
	default:
		return false
	}
}

// routineWait is a wait nobody has to act on for Legion (routineWaiting): a Dispatch claim release
// waiting on the claim's holder or on a contended answer (claimHeldWait, claimContendedWait),
// which the hour claimReleaseWait caps rather than an operator, a delivery row waiting for a
// launch the wake pacing parked (parkedWait), which that launch's turn ends, and a role message a
// pause held waiting for the role a pause stopped to hold its topic (roleMessage's wait on
// notify.ErrNoHolder), which the role's return or an end of its debt ends (heldMessageOwed).
func routineWait(err error) bool {
	var routine interface{ Routine() bool }
	return errors.As(err, &routine) && routine.Routine()
}

// routineWaiting is a wait that is routine (routineWait), so every attempt of its row stays at
// debug after the first rather than turning into a warning from the seventh (waitWarnAttempts),
// however long the wait lasts. err is the wait itself (wait.Errorf, or wait.Until for a wait that
// names its moment, as parkedWait's does), which it unwraps to, so settle still reads the row as
// waiting (wait.ErrWaiting) and retries it on its backoff, or at that moment (wait.UntilAt).
type routineWaiting struct{ err error }

func (w routineWaiting) Error() string { return w.err.Error() }

func (w routineWaiting) Unwrap() error { return w.err }

func (routineWaiting) Routine() bool { return true }

// execute runs one row, and is the one place each row's run passes, so the rule for a Dispatch
// that has no route for a write is here: its router's 404 (dispatch.NoRoute), which a Dispatch
// older than the route answers and so does the older build while a release rolls out, so no
// handler ran. For a row whose route a Dispatch may not serve yet (waitsForTheRoute) that answer
// is a wait, not a failure, so the daemon and Dispatch deploy in either order: the row runs again
// on its backoff and is logged as a wait is (settle), INFO at its first run, DEBUG after, and WARN
// from waitWarnAttempts on, about two minutes after the first run, once a minute at the backoff's
// cap; a status row's wait is routine (routineWaiting), so it logs once, at its first run. The
// wait wraps nothing of Dispatch's refusal, as the claim release's waits do (claimHeldWait), so no
// reading of permanentRefusal can end the row on it. Every other answer, and that answer for
// every other row, is decided as its executor and permanentRefusal decide it.
func (r *outbox) execute(ctx context.Context, row record.OutboxRow) error {
	err := r.run(ctx, row)
	if dispatch.NoRoute(err) && waitsForTheRoute(row) {
		waiting := wait.Errorf("outbox row %d (%s) of %s waits for a Dispatch that serves the route: %v", row.ID, row.Kind, row.Issue, err)
		if row.Kind == record.OutboxKindDispatchStatus {
			return routineWaiting{err: waiting}
		}
		return waiting
	}
	return err
}

// waitsForTheRoute is whether row's write waits for a Dispatch that has no route for it: the
// records of the record lane (a step's start and end, a retro's rating and a run's plan), the role
// holders' publication, and a status write with an option of the status-change route
// (record.StatusWrite.HasOption; decision 6 of dispatch://INFRA-4730), whose routes came after the
// Dispatch a daemon may meet. A waiting status row holds its issue's later status rows behind it
// on the board lane, so a later plain write cannot overtake the write that waits for the route;
// the rollout order of dispatch://INFRA-3320 serves the route before this release, so the wait
// lasts only while an older Dispatch still answers. Every other row's route stands on every
// Dispatch it is sent to, so that answer there means a dispatch_url that reaches no such Dispatch,
// and it is the refusal it says: a status write with no option, a message post and a claim
// release end on it with their follow-ups (dispatch.PermanentRefusal); the gated steps' and the
// design gates' publications retry it as a failure.
func waitsForTheRoute(row record.OutboxRow) bool {
	if record.LaneOf(row.Kind) == record.LaneRecord || row.Kind == record.OutboxKindRoleHoldersPublish {
		return true
	}
	if row.Kind != record.OutboxKindDispatchStatus {
		return false
	}
	payload, err := record.DecodeOutboxPayload(row)
	write, ok := payload.(record.StatusWrite)
	return err == nil && ok && write.HasOption()
}

// run is the executor of row's kind. An effect whose executor follows no protocol of its own
// across its run — a write to Dispatch, a message to a role, a seed, the branch, a production
// check's row — is checked against its issue's fence first (outlived, fencedHere): a late one of
// an earlier run or phase entry finishes there without acting. A late merge-forward ask acts no
// more than any other late row, but it ends through the ask's own end, which the ready of the
// asked role leaves the ask to (lateMergeForwardAsk). The notice, the supervise stops, the pod's
// suspension, workspace check and release check theirs where they act, each in its executor.
func (r *outbox) run(ctx context.Context, row record.OutboxRow) error {
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		return fmt.Errorf("decode outbox row %d: %w", row.ID, err)
	}
	if fencedHere(payload) {
		if ask, isAsk := payload.(record.MergeForwardMessage); isAsk && ask.Step == record.MergeForwardAsk {
			if late, err := r.lateMergeForwardAsk(ctx, row, ask); err != nil || late {
				return err
			}
		} else if late, err := r.outlived(ctx, row, payload); err != nil || late {
			return err
		}
	}
	switch value := payload.(type) {
	case record.StatusWrite:
		return r.status(ctx, row, value)
	case record.MessagePost:
		return r.message(ctx, row, value)
	case record.ClaimRelease:
		return r.releaseClaim(ctx, row)
	case record.Notice:
		return r.notice(ctx, row, value)
	case record.ControllerNotice:
		return r.controllerNotice(ctx, row, value)
	case record.SuperviseRequest:
		if err := r.supervise(ctx, row, value); err != nil {
			return err
		}
		// Whatever the row made of itself — a task delivered, an earlier generation, a phase left,
		// an issue out of Legion's work — the decisions it named are settled here (settleKept), reading from the
		// claim what became of the task: the run itself is over, and what it did is on the claim.
		return r.settleKept(ctx, row, value, false)
	case record.GateSeed:
		return r.seedGate(ctx, row, value)
	case record.PendingBasisSeed:
		return r.seedPendingBasis(ctx, row, value)
	case record.ReparentSeed:
		return r.seedReparent(ctx, row, value)
	case record.IssueSuspend:
		return r.suspendIssue(ctx, row, value)
	case record.IssueWorkspaceCheck:
		return r.checkIssueWorkspace(ctx, row, value)
	case record.IssueRelease:
		return r.releaseIssue(ctx, row, value)
	case record.RetentionEnd:
		return r.retentionEnd(ctx, row, value)
	case record.RoleMessage:
		return r.roleMessage(ctx, row, value)
	case record.GatedStepsPublish:
		return r.publishGatedSteps(ctx, value)
	case record.DesignGatesPublish:
		return r.publishDesignGates(ctx, value)
	case record.RoleHoldersPublish:
		return r.publishRoleHolders(ctx, row, value)
	case record.StepRecord:
		// A step record carrying a rating is the rating a retro gave the plan, which Dispatch
		// takes on the record's rating route alone (publishStepRating). One naming the experiment
		// and the variant of its issue sends them after that write, on the record's experiment
		// route alone (publishStepExperiment), so a Dispatch older than that route takes the
		// record and only the variant waits.
		publish := r.publishStepRecord
		if value.PlanRating != nil {
			publish = r.publishStepRating
		}
		if err := publish(ctx, row, value); err != nil || value.StepExperiment == nil {
			return err
		}
		return r.publishStepExperiment(ctx, row, value)
	case record.PlanRecord:
		return r.publishPlanRecord(ctx, row, value)
	case record.IssueBranch:
		return r.issueBranch(ctx, row, value)
	case record.CheckWakeDue:
		return r.checkWake(ctx, row, value)
	case record.CheckLook:
		return r.checkLook(ctx, row, value)
	case record.CheckBound:
		return r.checkBound(ctx, row)
	case record.PhaseEnded:
		return r.phaseEnded(ctx, row, value)
	case record.HookPrompt:
		return r.hookPrompt(ctx, row, value)
	case record.UpdateAsk:
		return r.updateAsk(ctx, row, value)
	case record.MergeQueueRead:
		return r.mergeQueueRead(ctx, row, value)
	case record.MergeQueueRemoval:
		return r.mergeQueueRemoval(ctx, row, value)
	case record.MergeHoldsChanged:
		return r.mergeHoldsChanged(ctx, row, value)
	case record.DesignNewsReconcile:
		return r.designChanges(ctx, row)
	case record.ConflictConfirm:
		return r.conflictConfirm(ctx, row, value)
	case record.MergeForwardMessage:
		return r.mergeForwardMessage(ctx, row, value)
	default:
		return fmt.Errorf("outbox row %d: no executor for %T", row.ID, payload)
	}
}

// fencedHere says whether run checks payload's fence before its executor runs (outlived): every
// fenced kind (record.FenceOf) but the notice, whose catch-up hands over what it named when it
// does not publish (deliverNotice), the supervise stops (supervise), and the pod's suspension,
// workspace check and release, which check theirs under the store's and the runtime's own fences
// (suspendIssue, checkIssueWorkspace, releaseIssue).
func fencedHere(payload record.OutboxPayload) bool {
	switch payload.(type) {
	case record.Notice, record.SuperviseRequest, record.IssueSuspend, record.IssueWorkspaceCheck, record.IssueRelease:
		return false
	}
	_, carries := record.FenceOf(payload)
	return carries
}

// status is the executor of a Dispatch status row, the daemon's one way of writing an issue's
// status (R20, R24 and R26 of dispatch://INFRA-3320; decisions 6, 11 and 12 of
// dispatch://INFRA-4730). run has checked the row's fence on the record before it reaches here:
// a row of an earlier run or phase entry finishes there, unwritten. A row that names the row it
// follows (record.StatusWrite.After) waits until that row has finished. A row with an option of
// the status-change route whose attempt is still open sends that attempt again as it is, with no
// read of Dispatch first (changeStatus). Otherwise the row reads the issue, settles what its kind
// settles against that read, posts its reason, and writes at the sequence of its last read
// (writeStatus), each write only once the record has seen Dispatch's events up to that sequence
// and still stands at the row's run and phase entry (readStands).
func (r *outbox) status(ctx context.Context, row record.OutboxRow, payload record.StatusWrite) error {
	if r.dispatch == nil {
		return errors.New("dispatch status executor has no Dispatch client")
	}
	rule, err := ruleFor(payload.Kind)
	if err != nil {
		return fmt.Errorf("outbox row %d: %w", row.ID, err)
	}
	if r.statusRowChecked != nil {
		r.statusRowChecked(ctx, row)
	}
	if err := r.awaitAfter(ctx, row, payload); err != nil {
		return err
	}
	if payload.HasOption() {
		attempt, err := r.attemptOf(ctx, row)
		if err != nil {
			return err
		}
		if attempt.Open() {
			return r.changeStatus(ctx, row, payload, rule, nil, attempt)
		}
	}
	issue, err := r.dispatch.GetIssue(ctx, row.Issue)
	if err != nil {
		return fmt.Errorf("read Dispatch issue %s before status write: %w", row.Issue, err)
	}
	proceed, err := rule.firstAttempt(ctx, r, row, payload, issue)
	if err != nil || !proceed {
		return err
	}
	// Somebody moved the issue after the row read it, so the write this row carries is
	// superseded and the row finishes having written nothing. The shared answer is taken after the
	// rule, so a kind's own question of the row is asked whichever way the issue stands.
	if statusMoved(payload, issue) {
		return nil
	}
	// A write's reason goes on the issue first, so it is there before the status moves the issue:
	// a done write's before the close, and a workflow move's before the move it explains.
	// It is posted as this row's message, so a retry after a failed write finds it and posts none.
	// The post is an event of the issue and moves its sequence, so the issue read above is stale
	// the moment it lands and the write is fenced on a read made after it (writeStatus). Fenced on
	// the older read, the first write of every row carrying a reason would be refused
	// (ISSUE_CHANGED) and pay another issue read, and for an ordinary row a record read, before
	// making the write that was always going to land. The post's own event reaches the consumer as
	// every event does and moves the issue's cursor (record.Issue.SeenSeq), so it holds the wait
	// before the write (readStands) no longer than the consumer takes to deliver it.
	//
	// A reason Dispatch refuses the same way on every attempt (dispatch.PermanentRefusal, a block
	// over its own bound among them) must not cost the issue its status, which is the write its
	// lifecycle stands on: the status is written all the same, and the refusal goes back wrapped
	// (statusWriteFailure) so the row is finished refused with the architect told that the status
	// stands. Any other failure of the post goes back as it is and the row retries on its backoff.
	var refused error
	if payload.Reason != "" {
		if err := r.message(ctx, row, record.MessagePost{Body: payload.Reason}); err != nil {
			if _, permanent := dispatch.PermanentRefusal(err); !permanent {
				return err
			}
			refused = err
		}
		if issue, err = r.dispatch.GetIssue(ctx, row.Issue); err != nil {
			return fmt.Errorf("read Dispatch issue %s after posting the reason of its status write: %w", row.Issue, err)
		}
	}
	if err := r.writeStatus(ctx, row, payload, rule, issue); err != nil {
		return &statusWriteFailure{err: err, wrote: statusWrites{reason: refused == nil && payload.Reason != ""}}
	}
	// The status stands, so a reason Dispatch refused for good is reported with that fact rather
	// than retried: the row is finished refused and the architect told which of the two writes
	// the issue holds (the kind's refused rule).
	if refused != nil {
		return &statusWriteFailure{err: refused, wrote: statusWrites{status: true}}
	}
	return nil
}

// statusWriteReads bounds how often one status row reads its issue again after Dispatch refused
// the write at the sequence that row read (ISSUE_CHANGED). Every event moves an issue's sequence,
// the daemon's own message posts among them, so a refusal usually says only that the read was
// overtaken and the next one wins; a run of them is an issue being edited faster than the daemon
// writes, which the row's own backoff waits out rather than this loop spinning on.
const statusWriteReads = 3

// writeStatus writes payload's status on the sequence of the read this row has just made
// (dispatch.Client.SetStatusAt), so a change Dispatch recorded after that read is refused rather
// than written over (dispatch://INFRA-714/spec R4). The sequence is the read's, never the one the
// phase decision saw: every event moves it, the daemon's own posts included, so a row carrying
// that older number would be refused almost every time. A row with an option of the
// status-change route is written there instead (changeStatus).
//
// Each write waits until the record has seen Dispatch's events up to the read's sequence, and
// is made only while the record still stands at the row's run and phase entry (readStands): the
// read can show a stop and a return the daemon has not yet applied, whose entry the row must not
// write across (R20 of dispatch://INFRA-3320).
//
// A set-back into work waits one more thing (awaitParentInWork): that Dispatch shows the issue's
// parent in work where the record has it in work, since Dispatch's parent rule (R24 of
// dispatch://INFRA-3320) refuses a child's write into work under a parent out of work. A row whose
// parent has a write into work queued is not claimable until that write has finished
// (record's outboxInOrder); the wait covers a parent whose own event, and so whose row, the daemon
// has not yet applied.
//
// ISSUE_CHANGED is no failure of the write and no permanent refusal (dispatch.PermanentRefusal),
// so nothing is dropped over it. The row reads the issue again and writes again only while that
// fresh read passes the rule its own kind is judged by (stillWanted). A read already at the status
// the row carries is this write's work, done by someone else; a read at any status the row does
// not take the issue at (statusMoved) is a change made after the row's read, which the row never
// writes over, whether it took the issue out of the workflow or moved it along within one. A
// PARENT_STATUS_CONFLICT and a 403 LEGION_STATUS_SCOPE_REQUIRED are the refusals the row answers
// itself (statusRefusal).
//
// What this read never does is reach the engine. Dispatch's read says what the issue is now and
// never who wrote it, so the engine would have to take every change it shows for a person's, and
// two of the daemon's own rows exist precisely because a change was not a person's: a session's
// own close or park, which the re-assert undoes, would be taken for a person's move of the issue
// out of Legion's work, freeing its slot. A change Dispatch recorded reaches the engine through its
// own event, with its actor (dispatch://INFRA-714/spec R4); this read decides one thing, whether
// to write.
//
// rule is the row's kind's rule, which its caller resolved once for the row (ruleFor) before
// anything was written. It is resolved there rather than here, at the refusal that first asks
// for it, so a kind nobody gave a rule fails its row before the first write and reaches Dispatch
// with nothing, whichever path brought the row here.
func (r *outbox) writeStatus(ctx context.Context, row record.OutboxRow, payload record.StatusWrite, rule statusRowRule, read dispatch.Issue) error {
	if payload.HasOption() {
		// The row's last attempt, ended, numbers the next: its key is never sent again.
		attempt, err := r.attemptOf(ctx, row)
		if err != nil {
			return err
		}
		attempt.Body = nil
		return r.changeStatus(ctx, row, payload, rule, &read, attempt)
	}
	for range statusWriteReads {
		// A status Dispatch already shows is this row's work, done: the write is not made again,
		// and the row counts as written. The repair queues a row on every turn while the record's
		// own DispatchStatus is stale (status_repair.go), so without this the daemon would write
		// a status Dispatch holds once a turn for as long as that staleness lasts.
		if read.Status == payload.Status {
			r.log.Info("outbox status write found Dispatch already at its status and wrote nothing",
				"row", row.ID, "issue", row.Issue, "status", payload.Status)
			return nil
		}
		if stands, err := r.readStands(ctx, row, payload, read); err != nil || !stands {
			return err
		}
		if err := r.awaitParentInWork(ctx, row, payload, read); err != nil {
			return err
		}
		written, err := r.dispatch.SetStatusAt(ctx, read, payload.Status)
		if err == nil {
			return r.applyOwnWrite(ctx, row, written)
		}
		if answered, err := r.statusRefusal(ctx, row, payload, err); answered {
			return err
		}
		var refusal *dispatch.Error
		if !errors.As(err, &refusal) || refusal.Code != "ISSUE_CHANGED" {
			return fmt.Errorf("set Dispatch issue %s to %s: %w", row.Issue, payload.Status, err)
		}
		if read, err = r.dispatch.GetIssue(ctx, row.Issue); err != nil {
			return fmt.Errorf("read Dispatch issue %s again after it changed under the status write: %w", row.Issue, err)
		}
		if read.Status == payload.Status {
			r.log.Info("outbox status write found Dispatch already at its status after the issue changed under it",
				"row", row.ID, "issue", row.Issue, "status", payload.Status)
			return nil
		}
		if wanted, err := r.rereadWanted(ctx, row, payload, rule, read); err != nil || !wanted {
			return err
		}
	}
	return fmt.Errorf("set Dispatch issue %s to %s: it changed under each of the %d reads this attempt made",
		row.Issue, payload.Status, statusWriteReads)
}

// rereadWanted says whether a row may write again after its issue changed under it, read as read
// shows it now and at another status than the row's: never over a status the row does not take
// the issue at, which is somebody's move made after the row's read (statusMoved), and otherwise as
// the row's kind judges (stillWanted).
func (r *outbox) rereadWanted(ctx context.Context, row record.OutboxRow, payload record.StatusWrite, rule statusRowRule, read dispatch.Issue) (bool, error) {
	if statusMoved(payload, read) {
		r.log.Info("outbox status write finished without writing: its issue was moved to another status after the row read it",
			"row", row.ID, "issue", row.Issue, "status", payload.Status, "observed", payload.ObservedStatus, "dispatch", read.Status)
		return false, nil
	}
	return rule.stillWanted(ctx, r, row, payload, read)
}

// readStands is the wait and the check each write of a status row makes after the read it is
// fenced on (R20 of dispatch://INFRA-3320; decision 11 of dispatch://INFRA-4730): the record has
// seen Dispatch's events up to the read's sequence (awaitSeen), and only then is the row's run and
// phase entry checked on the record again (outlived), so a stop and a return Dispatch recorded
// before the read, which the daemon had not applied yet, are applied and refuse the write rather
// than being written across. It answers false, having logged the finish, when the write is of an
// earlier run or phase entry, and a wait while the record has not seen the read's sequence yet.
// The seam a test holds a read row at (statusRowRead) runs first.
func (r *outbox) readStands(ctx context.Context, row record.OutboxRow, payload record.StatusWrite, read dispatch.Issue) (bool, error) {
	if r.statusRowRead != nil {
		r.statusRowRead(ctx, row, read.LastSeq)
	}
	if err := r.awaitSeen(ctx, row, read.LastSeq); err != nil {
		return false, err
	}
	late, err := r.outlived(ctx, row, payload)
	return err == nil && !late, err
}

// statusSeenWait is how long one run of a status row waits for the Dispatch consumer to see the
// sequence its read gave before it reads the issue's event log instead (awaitSeen): long enough
// for the consumer to deliver the events a read just overtook, its own reason message's first,
// and short beside the row's lease, which its run renews.
const statusSeenWait = 5 * time.Second

// statusSeenPoll is how often that wait reads the record's cursor.
const statusSeenPoll = 25 * time.Millisecond

// awaitSeen waits until the record of row's issue has seen Dispatch's events up to seq, the
// sequence the row's read gave (decision 11 of dispatch://INFRA-4730): its cursor
// (record.Issue.SeenSeq), which the consumer moves with every event of the issue it handles or
// drops, whatever its kind, has reached seq. It reads the cursor until seenWait has passed, and
// then once reads the issue's event log past the cursor (dispatch.Client.EventsAfter): when every
// event up to seq is one the consumer applied or one no handler takes
// (intake.DispatchEventsSeen), the record has had all of them — the events the consumer handled
// before the cursor was kept are read so — and the cursor is moved to seq. Otherwise the row
// waits on its backoff. An issue the daemon records nothing of, and an outbox with no store,
// wait for nothing: there is no record to have applied anything.
func (r *outbox) awaitSeen(ctx context.Context, row record.OutboxRow, seq int64) error {
	if r.pool == nil {
		return nil
	}
	deadline := time.Now().Add(r.seenWait)
	for {
		issue, err := r.recordedIssue(ctx, row.Issue)
		if err != nil {
			return err
		}
		if issue == nil || issue.SeenSeq >= seq {
			return nil
		}
		if !time.Now().Before(deadline) {
			return r.seenInTheLog(ctx, row, issue.SeenSeq, seq)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(statusSeenPoll):
		}
	}
}

// seenInTheLog is awaitSeen's read of the issue's event log past the cursor seen, up to seq.
func (r *outbox) seenInTheLog(ctx context.Context, row record.OutboxRow, seen, seq int64) error {
	events, err := r.dispatch.EventsAfter(ctx, row.Issue, seen)
	if err != nil {
		return fmt.Errorf("read the events of %s after %d, which the record has not seen: %w", row.Issue, seen, err)
	}
	applied, err := intake.DispatchEventsSeen(ctx, r.pool, row.Issue, events, seq)
	if err != nil {
		return err
	}
	if !applied {
		return wait.Errorf("outbox status row %d of %s read Dispatch at sequence %d and the record has applied its events only to %d: the write waits until it has",
			row.ID, row.Issue, seq, seen)
	}
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.AdvanceSeenSeq(ctx, tx, row.Issue, seq)
	}); err != nil {
		return fmt.Errorf("record that the events of %s up to %d were seen: %w", row.Issue, seq, err)
	}
	return nil
}

// awaitParentInWork holds a set-back into work (record.ReassertStatusRow, record.InWorkStatus) of
// an issue whose Dispatch parent is still in Legion's work in the daemon's record while Dispatch
// shows that parent out of work: when a session moves a parent and its child out of work together
// (with include_children), Dispatch emits the child's event first, so the child's set-back can run
// before the parent's event has reached the daemon and queued the parent's set-back (which
// outboxInOrder otherwise holds the child behind). Waiting here avoids a PARENT_STATUS_CONFLICT
// refusal during that window; once the parent's set-back lands (or a person's move takes the parent
// out of Legion's work in the record), this passes. It is also the one wait of a child the Dispatch
// copy names no parent of (record's outboxParentSQL), which outboxInOrder holds behind nothing: the
// parent waited on is the one Dispatch's own read of the child names (read.Parent), never the
// record's issues.parent, so a child taken out from under its parent waits for nothing of it.
func (r *outbox) awaitParentInWork(ctx context.Context, row record.OutboxRow, payload record.StatusWrite, read dispatch.Issue) error {
	if payload.Kind != record.ReassertStatusRow || payload.IncludeParent || !record.InWorkStatus(payload.Status) {
		return nil
	}
	above := parentKey(read.Parent)
	if r.pool == nil || above == "" || above == row.Issue {
		return nil
	}
	parentRec, err := r.recordedIssue(ctx, above)
	if err != nil {
		return err
	}
	if parentRec == nil || !record.InLegionsWork(*parentRec) {
		return nil
	}
	parent, err := r.dispatch.GetIssue(ctx, above)
	if err != nil {
		if _, permanent := dispatch.PermanentRefusal(err); permanent {
			return nil
		}
		return fmt.Errorf("read Dispatch parent %s before setting %s back to %s: %w", above, row.Issue, payload.Status, err)
	}
	if !record.InWorkStatus(parent.Status) {
		return wait.Errorf("outbox status row %d of %s waits for its parent %s (Dispatch shows %s, record shows %s) to be in work before setting %s back to %s",
			row.ID, row.Issue, above, parent.Status, parentRec.Status, row.Issue, payload.Status)
	}
	return nil
}

// awaitAfter holds a row that names the row it follows (record.StatusWrite.After) until that row
// has finished: a child's write released in the same wave as its parent's waits for the parent's
// (decision 5 of dispatch://INFRA-4730), which runs on its own issue's lane.
func (r *outbox) awaitAfter(ctx context.Context, row record.OutboxRow, payload record.StatusWrite) error {
	if payload.After == 0 || r.pool == nil {
		return nil
	}
	var queued bool
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		queued, err = r.records.OutboxQueued(ctx, tx, payload.After)
		return err
	}); err != nil {
		return fmt.Errorf("read whether the write %s follows is done: %w", row.Issue, err)
	}
	if queued {
		return wait.Errorf("outbox status row %d of %s waits for row %d, the write it follows, to finish", row.ID, row.Issue, payload.After)
	}
	return nil
}

// statusScopeCode is Dispatch's code of the 403 it answers a status write of an issue carrying
// the legion label made with a token that lacks the scope its DISPATCH_LEGION_STATUS_SCOPE asks
// for (R26 of dispatch://INFRA-3320). No mechanism shares a value between this module and
// Dispatch's, so the two spellings change together.
const statusScopeCode = "LEGION_STATUS_SCOPE_REQUIRED"

// roleHolderForbiddenCode is Dispatch's code of the 403 it answers a publication of an issue's
// role holders from a token it does not take as the daemon's own (putIssueRoleHolders in
// packages/envoy/internal/dispatch/api/issue_role_holders.go; R26 of dispatch://INFRA-3320). With
// DISPATCH_LEGION_STATUS_SCOPE on, Dispatch takes a publication only from a token that proves the
// session legion-daemon:<PROJECT> of the issue's project and holds the legion-status scope. The
// secrets broker grants that scope only to the token it mints for a BROKER_EXCHANGE_ACCOUNTS entry
// that names PROJECT and fixes that session (isLegionDaemon in
// packages/envoy/internal/cipher/exchange/exchange.go), so no bearer that dispatch_token_file names
// passes. With the setting off, Dispatch answers it a publication whose actor is not that session.
// The two spellings change together, as statusScopeCode's do.
const roleHolderForbiddenCode = "ROLE_HOLDER_FORBIDDEN"

// roleHolderNotProvenCode is Dispatch's code of the 403 it answers a publication of an issue's
// role holders on a project whose proven_sessions is on, from a token that does not prove the
// session legion-daemon:<PROJECT> (putIssueRoleHolders, after the body's checks and the read of
// the issue). publishRoleHolders reads it as it reads roleHolderForbiddenCode. The two spellings
// change together, as statusScopeCode's do.
const roleHolderNotProvenCode = "ROLE_HOLDER_NOT_PROVEN"

// statusScopeRefusedLine is statusRefusal's line for each 403 LEGION_STATUS_SCOPE_REQUIRED of a
// status write (statusScopeCode), logged with the code and the issue.
const statusScopeRefusedLine = "outbox status write refused by Dispatch: the daemon's token lacks the scope Dispatch asks of a Legion issue's status; the write is kept and written again"

// roleHolderRefusedLine is publishRoleHolders' line for each 403 ROLE_HOLDER_FORBIDDEN or
// ROLE_HOLDER_NOT_PROVEN of a publication (roleHolderForbiddenCode, roleHolderNotProvenCode),
// logged with the code and the issue.
const roleHolderRefusedLine = "outbox role-holder publication refused by Dispatch: the daemon's token does not prove the daemon's own session with the legion-status scope; the publication is kept and written again"

// statusRefusal answers the two refusals of a status write that the row decides itself rather
// than as dispatch.PermanentRefusal reads them, and says whether err was one of them; the error it
// answers is the row's. A PARENT_STATUS_CONFLICT (R24 of dispatch://INFRA-3320) ends the row: on a
// close, by the refused close (intake.CloseRefused), which keeps the record's pending close and
// tells the architect the children Dispatch named; on any other write, by the undo of the status
// the record counted before its echo (intake.StatusRefused, decision 12 of dispatch://INFRA-4730).
// A 403 LEGION_STATUS_SCOPE_REQUIRED keeps the row, as every 403 is kept, to be written again with
// backoff once the daemon's token holds the scope: each refusal logs a line naming the code and
// the issue, and the first refusal in this daemon's run of it or of a role-holder publication's
// ROLE_HOLDER_FORBIDDEN or ROLE_HOLDER_NOT_PROVEN queues the controller's one status-scope notice
// (tellStatusScopeOnce).
func (r *outbox) statusRefusal(ctx context.Context, row record.OutboxRow, payload record.StatusWrite, err error) (bool, error) {
	var refusal *dispatch.Error
	if !errors.As(err, &refusal) {
		return false, nil
	}
	switch {
	case dispatch.ParentStatusConflict(err):
		return true, r.parentRuleRefused(ctx, row, payload, refusal)
	case refusal.Status == http.StatusForbidden && refusal.Code == statusScopeCode:
		r.log.Warn(statusScopeRefusedLine,
			"row", row.ID, "issue", row.Issue, "status", payload.Status, "code", refusal.Code)
		written := fmt.Errorf("set Dispatch issue %s to %s: %w", row.Issue, payload.Status, err)
		if tellErr := r.tellStatusScopeOnce(ctx, row, fmt.Sprintf(
			"Dispatch refused the daemon's status write on %s with %d %s. Dispatch takes a write of a Legion issue's status from a token that holds the legion-status scope, and the token the daemon writes Dispatch with does not hold it. The daemon keeps the write and writes it again. Every later status write of the daemon waits the same way. %s",
			row.Issue, refusal.Status, refusal.Code, statusScopeRemedy(row.Issue))); tellErr != nil {
			return true, errors.Join(written, tellErr)
		}
		return true, written
	}
	return false, nil
}

// parentRuleRefused is a PARENT_STATUS_CONFLICT on row's write, applied as the fact its kind
// makes of it (statusRefusal), under an id of the row's own so a run that applies it again after a
// lost finish changes nothing more. The row then finishes.
func (r *outbox) parentRuleRefused(ctx context.Context, row record.OutboxRow, payload record.StatusWrite, refusal *dispatch.Error) error {
	if r.pool == nil {
		return fmt.Errorf("outbox row %d: Dispatch refused the write of %s on %s with %s, and this outbox has no store to record it in", row.ID, payload.Status, row.Issue, refusal.Code)
	}
	named := make([]string, 0, len(refusal.Issues))
	for _, issue := range refusal.Issues {
		if issue.Status == "" {
			named = append(named, issue.Key)
			continue
		}
		named = append(named, fmt.Sprintf("%s (%s)", issue.Key, issue.Status))
	}
	var fact intake.Fact = intake.StatusRefused{Issue: row.Issue, Generation: payload.Generation, Entry: payload.Entry,
		Status: payload.Status, Issues: named, Controller: payload.Controller}
	if payload.Kind == record.CloseStatusRow {
		fact = intake.CloseRefused{Issue: row.Issue, Generation: payload.Generation, Entry: payload.Entry, Children: named}
	}
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("status-refused:%s:%d", row.Issue, row.ID), fact, r.handlers...); err != nil {
		return fmt.Errorf("apply Dispatch's refusal of the write of %s on %s: %w", payload.Status, row.Issue, err)
	}
	r.log.Warn("outbox status write refused by Dispatch's parent status rule; the row ends and its caller is told",
		"row", row.ID, "issue", row.Issue, "status", payload.Status, "kind", payload.Kind, "code", refusal.Code, "issues", named)
	return nil
}

// statusScopeRemedy is the end of the controller's status-scope notice for a refused write on
// issue: the three changes a person makes so that the daemon's token holds the legion-status
// scope, and so proves the daemon's own session, legion-daemon:<PROJECT> (dispatch.DaemonSession),
// with that scope. Under cybermonklabs/legion#4601 the secrets broker alone grants the scope, to
// the token it mints for a BROKER_EXCHANGE_ACCOUNTS entry that names one project and fixes the
// session legion-daemon:<that project> (isLegionDaemon in
// packages/envoy/internal/cipher/exchange/exchange.go; writeProjectScope in
// packages/envoy/internal/cipher/oauth/exchange.go). Dispatch reads it only on a broker token
// named to one project (brokerTokenActor in packages/envoy/internal/dispatch/api/broker_token.go),
// and no personal token or bearer can hold it (model.ScopeLegionStatus), so the bearer
// dispatch_token_file names is no fix. A broker release without #4601 grants no such scope, so
// the notice names the change by which a person knows the release, and the scopes whoami answers
// for the daemon's token once it holds the scope.
func statusScopeRemedy(issue string) string {
	return fmt.Sprintf("A person must give the daemon a Dispatch token that holds the legion-status scope. Only the secrets broker mints such a token, for the daemon's own account entry. No bearer token that dispatch_token_file names can hold the scope. The person makes three changes. First, run the daemon with dispatch_token_broker: true in place of dispatch_token_file. Second, give the daemon's ServiceAccount a BROKER_EXCHANGE_ACCOUNTS entry that names the project %s and fixes the session %s. Third, run a secrets broker release that has the change that added the legion-status scope to the broker, cybermonklabs/legion pull request #4601. A token that the broker mints for that entry after that release holds the scope, and an earlier token does not. For a token that holds the scope, Dispatch's GET /api/v1/whoami answers \"scopes\": [\"legion-status\"].",
		claim.KeyProject(issue), dispatch.DaemonSession(issue))
}

// tellStatusScopeOnce queues the controller's status-scope notice of row's refusal, whose reason
// names the code and the issue and tells a person the three changes that give the daemon a token
// that holds the legion-status scope (statusScopeRemedy), unless this daemon's run has queued it
// already: one notice for the run, whichever of the three refusals it is told of comes first
// (statusRefusal's LEGION_STATUS_SCOPE_REQUIRED, publishRoleHolders' ROLE_HOLDER_FORBIDDEN and
// ROLE_HOLDER_NOT_PROVEN; decision 12 of dispatch://INFRA-4730, and
// dispatch://INFRA-4730/comment/14e96b87-f9a1-4b67-8fda-7d0c6605be9b for ROLE_HOLDER_NOT_PROVEN).
// The notice is queued in a transaction of its own whose commit wakes its lane; a queue that fails
// leaves the notice to the next refusal.
func (r *outbox) tellStatusScopeOnce(ctx context.Context, row record.OutboxRow, reason string) error {
	if !r.scopeTold.CompareAndSwap(false, true) {
		return nil
	}
	if r.pool == nil {
		r.scopeTold.Store(false)
		return fmt.Errorf("outbox row %d: no store to queue the controller's status-scope notice in", row.ID)
	}
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return record.EnqueueFenced(ctx, tx, r.records, row.Issue,
			record.ControllerNotice{Kind: record.StatusScopeNotice, Reason: reason}, r.now().UTC())
	}); err != nil {
		r.scopeTold.Store(false)
		return err
	}
	return nil
}

// changeStatus writes payload, a row with an option, on Dispatch's status-change route
// (dispatch.Client.ChangeStatus; decision 6 of dispatch://INFRA-4730), under Dispatch's request-key
// rule: a key holds the first body it was sent with, a stored answer is answered again for it, and
// a 5xx, a 401, a 408 and a 429 release it. Each attempt has a key of its own, made of the row's
// id and the attempt's number, and is recorded with the body it sends, expected_sequence included
// (record.OutboxAttempt), before it is sent, so no attempt ever sends a changed body under a key
// it used. attempt is the row's open attempt, which goes again as it is, with no read of Dispatch
// first; read is the issue as the row's caller last read it, nil for none.
//
// The same key and body go again on a later run after no answer, a transport failure, a 5xx, a
// 401, a 408, a 429, a codeless 4xx from whatever stands in front of Dispatch, the router's 404 of
// a Dispatch older than the route (execute makes it a wait), or a 409 KEY_IN_FLIGHT, after its
// Retry-After: Dispatch finishes, from the recovery point the route binds at each step, what the
// run that stopped left. Every other refusal is a stored answer and ends the attempt, the committed
// steps' entries it carries applied (applyEntries): after ISSUE_CHANGED — whole, or at a later
// step — the row reads Dispatch again and starts a new attempt at the new sequence, which finishes
// the rest, since the route leaves each issue already in the status's group as it is; after
// KEY_COMMITTED, a run that wrote and kept no answer, the same, its write read as landed; after
// KEY_REUSED, which these rules never cause, the same once the line is logged; a
// PARENT_STATUS_CONFLICT and a 403 LEGION_STATUS_SCOPE_REQUIRED as statusRefusal answers them;
// and any other code as dispatch.PermanentRefusal reads it, a kept one's next run sending a new
// attempt, since the stored answer would only be replayed. The key layer's codes are read here,
// before PermanentRefusal, which reads each of them as final. A 2xx applies its entries and ends
// the row. A named issue Dispatch already shows at the row's status is written all the same: its
// own step may have committed while a later one was refused.
func (r *outbox) changeStatus(ctx context.Context, row record.OutboxRow, payload record.StatusWrite, rule statusRowRule, read *dispatch.Issue, attempt record.OutboxAttempt) error {
	for range statusWriteReads {
		if !attempt.Open() {
			if read == nil {
				issue, err := r.dispatch.GetIssue(ctx, row.Issue)
				if err != nil {
					return fmt.Errorf("read Dispatch issue %s for a new attempt of its status change: %w", row.Issue, err)
				}
				if issue.Status != payload.Status {
					if wanted, err := r.rereadWanted(ctx, row, payload, rule, issue); err != nil || !wanted {
						return err
					}
				}
				read = &issue
			}
			if stands, err := r.readStands(ctx, row, payload, *read); err != nil || !stands {
				return err
			}
			body, err := json.Marshal(statusChangeBody{Status: payload.Status, ExpectedSequence: read.LastSeq,
				IncludeChildren: payload.IncludeChildren, UnlinkChildren: payload.UnlinkChildren, IncludeParent: payload.IncludeParent})
			if err != nil {
				return fmt.Errorf("encode the status change of %s: %w", row.Issue, err)
			}
			attempt = record.OutboxAttempt{Key: nextAttemptKey(row, attempt.Key), Body: body}
			if err := r.putAttempt(ctx, row, attempt); err != nil {
				return err
			}
		}
		var sent statusChangeBody
		if err := json.Unmarshal(attempt.Body, &sent); err != nil {
			return fmt.Errorf("outbox row %d: its attempt %s holds a body that does not decode: %w", row.ID, attempt.Key, err)
		}
		result, err := r.dispatch.ChangeStatus(ctx, row.Issue, dispatch.StatusChange{Status: sent.Status, ExpectedSequence: sent.ExpectedSequence,
			IncludeChildren: sent.IncludeChildren, UnlinkChildren: sent.UnlinkChildren, IncludeParent: sent.IncludeParent, IdempotencyKey: attempt.Key})
		if err == nil {
			return r.applyEntries(ctx, row, attempt.Key, result.Changed)
		}
		var refusal *dispatch.Error
		if !errors.As(err, &refusal) || sentAgain(err, refusal) {
			if refusal != nil && refusal.Code == keyInFlight {
				return wait.Until(r.now().UTC().Add(max(refusal.RetryAfter, time.Second)), fmt.Sprintf(
					"outbox status row %d of %s sends its attempt %s again once Dispatch's first run of it has ended (%s)", row.ID, row.Issue, attempt.Key, refusal.Code))
			}
			return fmt.Errorf("change Dispatch issue %s to %s under %s: %w", row.Issue, payload.Status, attempt.Key, err)
		}
		attempt.Body = nil
		if err := r.putAttempt(ctx, row, attempt); err != nil {
			return err
		}
		if err := r.applyEntries(ctx, row, attempt.Key, refusal.Changed); err != nil {
			return err
		}
		switch refusal.Code {
		case "ISSUE_CHANGED", keyCommitted:
			read = nil
			continue
		case keyReused:
			r.log.Error("outbox status change sent under a key another body used; the row reads Dispatch and starts a new attempt",
				"row", row.ID, "issue", row.Issue, "key", attempt.Key, "error", err)
			read = nil
			continue
		}
		if answered, err := r.statusRefusal(ctx, row, payload, err); answered {
			return err
		}
		return fmt.Errorf("change Dispatch issue %s to %s under %s: %w", row.Issue, payload.Status, attempt.Key, err)
	}
	return fmt.Errorf("change Dispatch issue %s to %s: it changed under each of the %d attempts this run made",
		row.Issue, payload.Status, statusWriteReads)
}

// The request-key layer's codes (packages/envoy/internal/dispatch/api/request_keys.go): a repeat
// whose first run still runs, a run that wrote and kept no answer, and another body under a used
// key. No mechanism shares a value between this module and Dispatch's, so the spellings change
// together.
const (
	keyInFlight  = "KEY_IN_FLIGHT"
	keyCommitted = "KEY_COMMITTED"
	keyReused    = "KEY_REUSED"
)

// sentAgain says whether Dispatch's answer err leaves the attempt open, to be sent again under its
// key with its body (changeStatus): an answer the key layer stored nothing for — a 5xx, a 401, a
// 408 and a 429 release the key, a codeless 4xx is from whatever stands in front of Dispatch, and
// the router's 404 of a Dispatch older than the route ran no handler — and a 409 KEY_IN_FLIGHT,
// whose first run still holds the key.
func sentAgain(err error, refusal *dispatch.Error) bool {
	switch {
	case refusal.Status >= 500, refusal.Code == "", dispatch.NoRoute(err), refusal.Code == keyInFlight:
		return true
	}
	switch refusal.Status {
	case http.StatusUnauthorized, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return false
}

// statusChangeBody is the body an attempt of a status change sends, as the row keeps it
// (record.OutboxAttempt.Body): what of dispatch.StatusChange the route's body carries, the actor
// aside, which the client adds to every write.
type statusChangeBody struct {
	Status           string `json:"status"`
	ExpectedSequence int64  `json:"expected_sequence,omitempty"`
	IncludeChildren  bool   `json:"include_children,omitempty"`
	UnlinkChildren   bool   `json:"unlink_children,omitempty"`
	IncludeParent    bool   `json:"include_parent,omitempty"`
}

// nextAttemptKey is the request key of row's next attempt after the one under last ("" for
// none): `outbox-<issue>-<row id>-<attempt number>`, numbered from 1. The issue keeps two daemons
// that share a Dispatch token but no database from naming two writes alike.
func nextAttemptKey(row record.OutboxRow, last string) string {
	number := 0
	if i := strings.LastIndexByte(last, '-'); i >= 0 {
		number, _ = strconv.Atoi(last[i+1:])
	}
	return fmt.Sprintf("outbox-%s-%d-%d", row.Issue, row.ID, number+1)
}

// attemptOf is row's attempt as the outbox holds it (record.OutboxAttempt): the zero one in an
// outbox with no store, whose rows send every attempt anew.
func (r *outbox) attemptOf(ctx context.Context, row record.OutboxRow) (record.OutboxAttempt, error) {
	if r.pool == nil {
		return record.OutboxAttempt{}, nil
	}
	var attempt record.OutboxAttempt
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, _, err = r.records.OutboxAttempt(ctx, tx, row.ID)
		return err
	}); err != nil {
		return record.OutboxAttempt{}, fmt.Errorf("read the attempt of outbox row %d: %w", row.ID, err)
	}
	return attempt, nil
}

// putAttempt records attempt as row's, under the row's lease: nothing is sent under a key whose
// body the row did not keep first. An outbox with no store keeps nothing.
func (r *outbox) putAttempt(ctx context.Context, row record.OutboxRow, attempt record.OutboxAttempt) error {
	if r.pool == nil {
		return nil
	}
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.PutOutboxAttempt(ctx, tx, row.ID, row.LeaseToken, attempt)
	}); err != nil {
		return fmt.Errorf("record the attempt of outbox row %d: %w", row.ID, err)
	}
	return nil
}

// applyEntries applies to the record each entry of a status-change answer under key, a 2xx's or
// the committed steps a refusal carries (dispatch://INFRA-4730's t4 rule), as the daemon's own
// status write (intake.OwnStatusWrite, through the issue's Observed view): the step's issue, at the
// step's sequence, written by the daemon. An entry is applied only to an issue the daemon records
// whose record stands below the entry's issue's sequence, so an entry whose event the stream
// delivered first changes nothing; and an entry of an issue other than the row's only where the
// record stands at the entry's previous_last_seq, since a record below it has events before the
// step the stream has yet to deliver, which the entry would leave unread. The entry of the row's
// own issue, where the daemon records nothing of it, is applied as any landed write of such an
// issue is (applyOwnWrite). Every other entry is left to the events. Each is applied under an id
// of its own, so a run that applies it again changes nothing more.
func (r *outbox) applyEntries(ctx context.Context, row record.OutboxRow, key string, entries []dispatch.StatusChangeEntry) error {
	if r.pool == nil {
		return nil
	}
	for _, entry := range entries {
		if entry.Issue == nil {
			continue
		}
		recorded, err := r.recordedIssue(ctx, entry.Key)
		if err != nil {
			return err
		}
		if recorded == nil && entry.Key == row.Issue {
			if err := r.applyOwnWrite(ctx, row, *entry.Issue); err != nil {
				return err
			}
			continue
		}
		if recorded == nil || entry.Issue.LastSeq <= recorded.LastDispatchSeq ||
			entry.Key != row.Issue && recorded.LastDispatchSeq != entry.PreviousLastSeq {
			continue
		}
		event := intake.OwnStatusWrite(*entry.Issue)
		fact := intake.ObservedIssue(*entry.Issue)
		fact.Delivered = &event
		if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("status-change:%s:%s:%d", key, entry.Key, entry.Issue.LastSeq), fact, r.handlers...); err != nil {
			return fmt.Errorf("apply the status change of %s that %s committed: %w", entry.Key, key, err)
		}
	}
	return nil
}

// applyOwnWrite applies Dispatch's answer to row's landed write, written, where the daemon records
// nothing of its issue (api.ApplyOwnStatusWrite): a write an API route queued of an issue it could
// not count on a record, such as the controller's set_status todo on an issue carrying the legion
// label, which this records and admission stands in the waiting line before the write's event
// arrives (decision 5 of dispatch://INFRA-4730). A write of a recorded issue was counted on its
// record when it was queued, and its echo moves only the record's view of Dispatch (the workflow's
// dispatchIssue). The write has landed, so a record that does not take the answer is logged and
// left to the write's event, and the row ends.
func (r *outbox) applyOwnWrite(ctx context.Context, row record.OutboxRow, written dispatch.Issue) error {
	if r.pool == nil || written.Key == "" {
		return nil
	}
	recorded, err := r.recordedIssue(ctx, written.Key)
	if err != nil || recorded != nil {
		return err
	}
	own := api.OwnStatusWrite{DispatchProject: r.dispatchProject, Prepare: r.prepare,
		Apply: func(ctx context.Context, eventID string, fact intake.Fact) (intake.Result, error) {
			return intake.ApplyFact(ctx, r.pool, "outbox", eventID, fact, r.handlers...)
		}}
	if failure := api.ApplyOwnStatusWrite(ctx, own, fmt.Sprintf("status-write:%s:%d:%d", written.Key, row.ID, written.LastSeq), written); failure != nil {
		r.log.Warn("outbox status write landed, but the record did not take Dispatch's answer; the write's event records it",
			"row", row.ID, "issue", written.Key, "status", written.Status, "code", failure.Code, "reason", failure.Message)
	}
	return nil
}

// statusRowRule is what one kind of status row (record.StatusRowKind) does: what its first
// attempt settles against the issue Dispatch shows, and the rule its retry is judged by once
// Dispatch has refused that write (ISSUE_CHANGED). Both decisions live on one value, and this
// interface asks for both, so a kind given one and not the other does not build. A kind with a
// first attempt of its own and no retry rule, or the reverse, would bring back the silence the
// kinds exist to end: the half nobody wrote is lent to the row by whichever branch it falls
// through to, so the row is judged by a rule written for another kind and nothing says so.
type statusRowRule interface {
	// firstAttempt settles what this kind has to settle before the row's first write, against
	// the issue as Dispatch shows it, and says whether that write is still to be made. A rule
	// that has finished the row itself — the write is not wanted, or this kind makes its own —
	// answers proceed false, and the row ends with the error it returns.
	firstAttempt(ctx context.Context, r *outbox, row record.OutboxRow, payload record.StatusWrite, issue dispatch.Issue) (proceed bool, err error)
	// stillWanted says whether a row whose write Dispatch refused may write again. The caller
	// has already taken the two answers every kind shares — the status is the row's own, so
	// someone else did this work, or it is neither the row's nor the one it observed, so
	// somebody moved the issue since — and what is left is the question each kind answers
	// differently.
	//
	// Every kind but the ordinary one is queued at a status that reads as out of the workflow, and
	// for them the ordinary rule reads backwards: a person's handover brings an issue into the
	// workflow, a re-assert undoes a session's own move out of it, and a close is the move out. Asking
	// record.OutOfWorkflow of any of them would finish it without writing and log that its issue
	// had left a workflow it never entered, or had left one this very row exists to put it back
	// in.
	stillWanted(ctx context.Context, r *outbox, row record.OutboxRow, payload record.StatusWrite, read dispatch.Issue) (bool, error)
	// refused is what this kind leaves behind when Dispatch's refusal of the write is permanent
	// and the row is dropped (finishRefused): follow runs in the finishing transaction, handed
	// that transaction's ctx, told logs once that transaction commits, and nil for either says
	// this kind leaves nothing behind. It is here rather than in finishRefused's own branch for
	// the reason the other two are: it is a decision taken by the row's kind, and a kind whose
	// clean-up nobody wrote would otherwise be dropped as an ordinary row leaves nothing, in
	// silence.
	refused(r *outbox, row record.OutboxRow, payload record.StatusWrite, cause error) (follow func(context.Context, pgx.Tx) error, told func())
}

// ruleFor is the one place a status row's kind is read: a value per kind and no fallback, so a
// kind added to record.StatusRowKinds without one here fails its row, naming itself, rather than
// being judged in silence by whichever rule a default would lend it. An ordinary default reads
// backwards for every other kind, each queued at or writing a status out of the workflow, and
// would drop the very row that exists to bring the issue back into one.
// TestEveryStatusRowKindHasARule holds the list and this switch to each other, and
// TestAStatusRowKindWithNoRuleFailsBothPathsWithoutWriting holds a row of a kind this switch has
// no value for to failing on both paths with nothing written.
func ruleFor(kind record.StatusRowKind) (statusRowRule, error) {
	switch kind {
	case record.OrdinaryStatusRow:
		return ordinaryRule{}, nil
	case record.PersonHandoverStatusRow:
		return personHandoverRule{}, nil
	case record.ReassertStatusRow:
		return reassertRule{}, nil
	case record.CloseStatusRow:
		return closeRule{}, nil
	default:
		return nil, fmt.Errorf("status row kind %q has no rule", kind)
	}
}

// statusMoved says whether somebody moved the issue since the row was queued: Dispatch shows
// neither the status it observed, nor the status its record last saw Dispatch show when it was
// queued (record.StatusWrite.DispatchStatus, which only an ordinary row carries), nor the one the
// row carries, so the row's write is superseded by a change made after it was queued. The status
// an ordinary row observed is the one its record counts (R16 of dispatch://INFRA-3320), which an
// earlier write of the issue still queued puts on Dispatch only if it runs: a write of an earlier
// phase entry finishes unwritten at a later one (record.LateAt), and Dispatch then shows what the
// record last saw it show, a move the daemon had already applied and answered before it queued
// this row, which this row writes over. A move the daemon had not applied when it queued the row
// is at neither, and stands.
//
// The daemon's own close (record.CloseStatusRow) is superseded so only by a status out of the
// workflow (record.OutOfWorkflow): a person's stop or close, which ends the run the close was
// queued for. Any other status Dispatch shows is still in work, and the close writes over it: the
// record still owes the close (record.PendingClose) at its run and phase entry, which the write
// checks once the record has applied Dispatch's events up to its read (readStands, closeStands),
// and nothing writes the close again once its row is gone, so the issue would wait in its phase
// for a done nobody writes. The status the close observed is the record's own, which a status
// write of an earlier phase entry the entry fence refused can leave Dispatch behind (R20 and R24
// of dispatch://INFRA-3320).
func statusMoved(payload record.StatusWrite, issue dispatch.Issue) bool {
	if issue.Status == payload.ObservedStatus || issue.Status == payload.Status || payload.DispatchStatus != "" && issue.Status == payload.DispatchStatus {
		return false
	}
	return payload.Kind != record.CloseStatusRow || record.OutOfWorkflow(issue.Status)
}

// ordinaryRule judges the workflow moving an issue along inside the workflow
// (record.OrdinaryStatusRow).
type ordinaryRule struct{}

// firstAttempt settles nothing of its own: an ordinary row's fences are the ones every kind
// shares, which its caller has taken.
func (ordinaryRule) firstAttempt(context.Context, *outbox, record.OutboxRow, record.StatusWrite, dispatch.Issue) (bool, error) {
	return true, nil
}

func (ordinaryRule) stillWanted(ctx context.Context, r *outbox, row record.OutboxRow, payload record.StatusWrite, read dispatch.Issue) (bool, error) {
	return r.statusStillWanted(ctx, row, payload, read)
}

// refused leaves nothing behind: an ordinary row's whole effect is the write Dispatch refused.
func (ordinaryRule) refused(r *outbox, row record.OutboxRow, payload record.StatusWrite, cause error) (func(context.Context, pgx.Tx) error, func()) {
	return statusRefusalTold(r, row, payload, cause)
}

// personHandoverRule judges a person's handover (record.PersonHandoverStatusRow), which stands
// only while the issue still carries the label that person put on it.
type personHandoverRule struct{}

func (personHandoverRule) firstAttempt(_ context.Context, r *outbox, row record.OutboxRow, payload record.StatusWrite, issue dispatch.Issue) (bool, error) {
	// A row whose issue has moved since is superseded, which the caller settles for every kind
	// alike, so the label is asked about only while the move this row answers still stands: a
	// person who parked the root since is let stand silently rather than reported as one who
	// took the label off.
	if statusMoved(payload, issue) {
		return true, nil
	}
	return r.personHandoverStands(row, issue), nil
}

func (personHandoverRule) stillWanted(_ context.Context, r *outbox, row record.OutboxRow, _ record.StatusWrite, read dispatch.Issue) (bool, error) {
	return r.personHandoverStands(row, read), nil
}

// refused leaves nothing behind: the label this row would have put on the issue is part of the
// write Dispatch refused, and the record holds nothing else for it.
func (personHandoverRule) refused(r *outbox, row record.OutboxRow, payload record.StatusWrite, cause error) (func(context.Context, pgx.Tx) error, func()) {
	return statusRefusalTold(r, row, payload, cause)
}

// reassertRule judges the daemon setting its own status back over one a session wrote
// (record.ReassertStatusRow).
type reassertRule struct{}

// firstAttempt asks the question of this kind's own before the row's first write: whether the
// status changed after the write this row undoes (setBackOwed). It is asked before the stop at a
// third status that every kind shares (statusMoved), because Dispatch can show the observed status
// again after a later move: the owning architect's icebox and then backlog end where a worker's
// backlog stood, and only the events between them say that the move came. When the issue read it
// is given ends at that write (issue.LastSeq <= payload.ObservedSeq), no event follows the write,
// so the write is owed and the events are not read. A change that lands after that read is refused
// ISSUE_CHANGED by the write, which is fenced on the read's sequence (writeStatus), and stillWanted
// asks setBackOwed then.
func (reassertRule) firstAttempt(ctx context.Context, r *outbox, row record.OutboxRow, payload record.StatusWrite, issue dispatch.Issue) (bool, error) {
	if issue.LastSeq <= payload.ObservedSeq {
		return true, nil
	}
	return r.setBackOwed(ctx, row, payload)
}

func (reassertRule) stillWanted(ctx context.Context, r *outbox, row record.OutboxRow, payload record.StatusWrite, _ dispatch.Issue) (bool, error) {
	// The read shows the status the session wrote, which the caller has just required. The record
	// cannot be consulted for the rest, since the status this row restores is the one the record
	// kept (workflow's keepStatus), and the issue's being out of the workflow in Dispatch's eyes
	// is the very thing being undone. What is left is the question of the first attempt, asked
	// again of the events that came since.
	return r.setBackOwed(ctx, row, payload)
}

// setBackOwed says whether the set-back payload still has a write to make: no event after the
// write it undoes (payload.ObservedSeq) changed the issue's status, other than the daemon's own
// (intake.StatusChange.DaemonActor). A later status change is a move this row must not write
// over: the owning architect's move of a child, which the workflow takes as a person's
// (dispatch://INFRA-3178 R3), a person's, or the session's own next status, which a set-back of
// its own answers. Only Dispatch's log can say that such a change came, because the row runs
// within milliseconds of the write it undoes, before the consumer can apply a later event. A
// comment, a message or a write of the status the issue already shows changes no status, so the
// set-back still writes (intake.StatusChangeOf). The write itself stays fenced on the sequence of
// the issue read made before this check (writeStatus), so a change that lands after that read is
// refused ISSUE_CHANGED, and the retry asks this again. For that reason the first attempt asks
// this only when that read shows an event after the write the row undoes
// (reassertRule.firstAttempt). A row with no sequence, which an earlier release queued, is owed
// its write as that release judged it.
func (r *outbox) setBackOwed(ctx context.Context, row record.OutboxRow, payload record.StatusWrite) (bool, error) {
	if payload.ObservedSeq == 0 {
		return true, nil
	}
	events, err := r.dispatch.EventsAfter(ctx, row.Issue, payload.ObservedSeq)
	if err != nil {
		return false, fmt.Errorf("read the Dispatch events of %s after %d before its set-back: %w", row.Issue, payload.ObservedSeq, err)
	}
	for _, raw := range events {
		change, err := intake.StatusChangeOf(raw)
		if err != nil {
			return false, fmt.Errorf("read a Dispatch event of %s after %d before its set-back: %w", row.Issue, payload.ObservedSeq, err)
		}
		if !change.Changed || change.DaemonActor {
			continue
		}
		r.log.Info("outbox set-back finished without writing: the issue's status changed after the write it undoes",
			"row", row.ID, "issue", row.Issue, "seq", change.Seq, "actor", change.Actor.Name())
		return false, nil
	}
	return true, nil
}

// refused leaves nothing behind: the status the record kept is unchanged by a refusal, and the
// next event about the issue decides what happens to it.
func (reassertRule) refused(r *outbox, row record.OutboxRow, payload record.StatusWrite, cause error) (func(context.Context, pgx.Tx) error, func()) {
	return statusRefusalTold(r, row, payload, cause)
}

// closeRule judges the daemon's own close of an issue (record.CloseStatusRow): the done a merge
// or an architect's close_root writes, queued in the transaction that took the record out of the
// workflow.
type closeRule struct{}

// firstAttempt asks the one question a close has of its own (closeStands): whether the close this
// row carries is still the record's. A row's first attempt is not its first chance to be stale —
// an attempt that ended in a backoff, a Dispatch that answered 5xx, or a daemon that restarted
// before the write leaves the row to a fresh attempt hours later — and a person's reopen in that
// time would otherwise be closed again by the write this row has been holding.
func (closeRule) firstAttempt(ctx context.Context, r *outbox, row record.OutboxRow, payload record.StatusWrite, _ dispatch.Issue) (bool, error) {
	return closeStands(ctx, r, row, payload)
}

// stillWanted asks the same question after Dispatch refused the write. A close is written over
// any status in work Dispatch shows (statusMoved), so what is left is whether the close is still
// owed. The ordinary rule cannot be asked here — it drops a row whose issue has left the workflow,
// which is every close, so one refusal would leave the issue open in Dispatch for good behind a
// close nobody ever wrote. That is the reason this kind exists.
func (closeRule) stillWanted(ctx context.Context, r *outbox, row record.OutboxRow, payload record.StatusWrite, _ dispatch.Issue) (bool, error) {
	return closeStands(ctx, r, row, payload)
}

// closeStands says whether the close row still carries the record's own close: the record stands
// at the generation the row names. A person's reopen re-admits the issue in a generation of its
// own, and that close is no longer owed, whichever attempt of the row reaches this question —
// Dispatch holds what the person put there. A row with no generation is one migration
// 20261102003600 gave its kind, queued by a release that fenced the write on nothing: it writes
// as that release wrote it. An issue the daemon keeps no record of is closed as the row says, as
// every other rule judges such an issue by the row and the read alone.
func closeStands(ctx context.Context, r *outbox, row record.OutboxRow, payload record.StatusWrite) (bool, error) {
	if payload.CloseGeneration == 0 {
		return true, nil
	}
	issue, err := r.recordedIssue(ctx, row.Issue)
	if err != nil {
		return false, fmt.Errorf("judge the close queued for %s: %w", row.Issue, err)
	}
	if issue != nil && issue.Generation != payload.CloseGeneration {
		r.log.Info("outbox close finished without writing: the issue was taken back into the workflow in a generation of its own",
			"row", row.ID, "issue", row.Issue, "closed", payload.CloseGeneration, "recorded", issue.Generation)
		return false, nil
	}
	return true, nil
}

// refused leaves nothing behind: the close's whole effect is the write Dispatch refused, and the
// record already stands where the close left it.
func (closeRule) refused(r *outbox, row record.OutboxRow, payload record.StatusWrite, cause error) (func(context.Context, pgx.Tx) error, func()) {
	return statusRefusalTold(r, row, payload, cause)
}

func (r *outbox) personHandoverStands(row record.OutboxRow, issue dispatch.Issue) bool {
	if issue.HandedOver {
		return true
	}
	r.log.Info("outbox: a person's handover is not written; the label is gone", "row", row.ID, "issue", row.Issue)
	return false
}

// statusStillWanted says whether a row may write its status after its issue changed under it: the
// fresh read shows a status still in the workflow — neither done nor parked
// (record.OutOfWorkflow) — and the record, read as it stands, still has the issue in the
// workflow too. The read is not applied to the record: it carries no actor, and a change Dispatch
// recorded reaches the engine through its own event (writeStatus). A close or a park is the issue's state now, and the status this
// row carries is of a workflow the issue has left, so the row finishes having written nothing. An
// issue the daemon keeps no record of is judged by the read alone.
//
// A row whose status stands on the other side of the workflow's edge from the status the read
// shows (crossesWorkflow) is an API call's write — set_status, rerun_child or release_children
// taking an issue into the workflow or out of it (decision 5 of dispatch://INFRA-4730) — or a
// workflow move over a status out of the workflow that its record last saw Dispatch show
// (record.StatusWrite.DispatchStatus), a session's that the daemon set back, and the rule above
// reads it backwards. Its caller has already finished it where Dispatch shows a status the row does
// not take the issue at (statusMoved), and it is wanted while the record, which counted the write
// before its echo (R16 of dispatch://INFRA-3320), still stands on the side of the edge the write's
// status is on: a person's move since, which the engine took, has moved the record back across it.
// An issue the daemon keeps no record of is judged by the read alone.
func (r *outbox) statusStillWanted(ctx context.Context, row record.OutboxRow, payload record.StatusWrite, read dispatch.Issue) (bool, error) {
	if crossesWorkflow(payload, read.Status) {
		issue, err := r.recordedIssue(ctx, row.Issue)
		if err != nil {
			return false, fmt.Errorf("judge the status write of %s: %w", row.Issue, err)
		}
		if issue != nil && record.InWorkflow(*issue) == record.OutOfWorkflow(payload.Status) {
			r.log.Info("outbox status write finished without writing: the record no longer stands where the write takes its issue",
				"row", row.ID, "issue", row.Issue, "status", payload.Status, "record", issue.Status, "dispatch", read.Status)
			return false, nil
		}
		return true, nil
	}
	left := record.OutOfWorkflow(read.Status)
	if !left {
		issue, err := r.recordedIssue(ctx, row.Issue)
		if err != nil {
			return false, fmt.Errorf("judge the status write of %s: %w", row.Issue, err)
		}
		left = issue != nil && !record.InWorkflow(*issue)
	}
	if left {
		r.log.Info("outbox status write finished without writing: its issue left the workflow while the write was on its way",
			"row", row.ID, "issue", row.Issue, "status", payload.Status, "dispatch", read.Status)
		return false, nil
	}
	return true, nil
}

// crossesWorkflow says whether payload, an ordinary status row, writes a status on the other side
// of the workflow's edge from shown, the status a read shows that the row takes the issue at
// (statusMoved): its observed status, or the one its record last saw Dispatch show when it was
// queued (record.OutOfWorkflow): a todo over a backlog, a backlog over an in_progress. The
// workflow's own moves write within the workflow from the status they observed, and its closes are
// close rows, so what crosses is an API call's write, or a workflow move over a status out of the
// workflow its record last saw, a session's that the daemon set back (statusStillWanted).
func crossesWorkflow(payload record.StatusWrite, shown string) bool {
	return shown != "" && record.OutOfWorkflow(payload.Status) != record.OutOfWorkflow(shown)
}

// publishGatedSteps writes the deployment's design-gate steps onto the project's Dispatch row.
// The row's own issue is that project key, and the payload names it too, so the write addresses
// the project the boot that queued it served however the row is read back.
//
// A Dispatch that has not got the route yet answers 404, and nothing here treats that as final:
// the kind is no permanent refusal, so the lane's backoff holds the row until the server has the
// route, which is what lets the daemon and Dispatch deploy in either order.
func (r *outbox) publishGatedSteps(ctx context.Context, payload record.GatedStepsPublish) error {
	if r.dispatch == nil {
		return errors.New("gated steps executor has no Dispatch client")
	}
	if err := r.dispatch.PutGatedSteps(ctx, payload.Project, record.StepNames(payload.Steps)); err != nil {
		return fmt.Errorf("publish the gated steps of %s: %w", payload.Project, err)
	}
	return nil
}

// publishDesignGates writes the document each of the issue's design-gate steps records onto the
// issue's Dispatch row. The row's own issue is that issue, and the payload names it too, so the
// write addresses the issue whose gate change queued it however the row is read back. The set is
// sent whole, empty included: it replaces what Dispatch holds, so an issue whose gates record
// nothing clears the set rather than leaving the last one standing. Each entry carries whether it
// holds the issue's tree as the payload says it (record.DesignGateDocument.Holds), null for a row
// an older daemon queued.
//
// A Dispatch that has not got the route yet answers its router's 404 under the code NOT_FOUND,
// and nothing here treats that as final (permanentRefusal, which ends the row on ISSUE_NOT_FOUND
// alone): the lane's backoff holds the row until the server has the route, which is what lets the
// daemon and Dispatch deploy in either order.
func (r *outbox) publishDesignGates(ctx context.Context, payload record.DesignGatesPublish) error {
	if r.dispatch == nil {
		return errors.New("design gates executor has no Dispatch client")
	}
	gates := make([]dispatch.DesignGateDocument, len(payload.Gates))
	for i, gate := range payload.Gates {
		gates[i] = dispatch.DesignGateDocument{Step: string(gate.Step), ArtifactID: gate.ArtifactID, Holds: gate.Holds}
	}
	if err := r.dispatch.PutDesignGates(ctx, payload.Issue, gates); err != nil {
		return fmt.Errorf("publish the design gates of %s: %w", payload.Issue, err)
	}
	return nil
}

// publishRoleHolders writes the session of each Legion role of the issue whose claim holds it
// (supervise.HoldsRole, record.Postgres.RoleHolders) onto the issue's Dispatch row: a claim the
// idle release suspended keeps its role, so neither the release of an idle pod nor the wake that
// brings it back changes the set. The set is read from the claims now, in a transaction of its
// own, not when the row was queued: a claim that ended after the queue is gone from it, and two
// claim changes of one issue never publish a stale set. It is sent whole, empty included, since it
// replaces what Dispatch holds, so a role whose claim ended leaves the set with no delete route.
//
// A Dispatch that has not got the route yet answers its router's 404, which execute makes a wait,
// so the daemon and Dispatch deploy in either order. Every other answer is decided by
// permanentRefusal: the route's own 404 ISSUE_NOT_FOUND and a coded 400 end the row, and a 5xx, a
// codeless 4xx, a 401, a 403, a handler's 404 NOT_FOUND "not found" and a transport failure are
// failures retried on the row's backoff. Of those, two 403s refuse the daemon's token
// (putIssueRoleHolders in packages/envoy/internal/dispatch/api/issue_role_holders.go). A Dispatch
// whose DISPATCH_LEGION_STATUS_SCOPE is on answers ROLE_HOLDER_FORBIDDEN to every token but one
// that proves the session legion-daemon:<PROJECT> of the issue's project and holds the
// legion-status scope (roleHolderForbiddenCode). On a project whose proven_sessions is on, Dispatch
// answers ROLE_HOLDER_NOT_PROVEN to a token that does not prove that session
// (roleHolderNotProvenCode). Each is read as a status write's LEGION_STATUS_SCOPE_REQUIRED is
// (decision 12 of dispatch://INFRA-4730; dispatch://INFRA-4730/comment/14e96b87-f9a1-4b67-8fda-7d0c6605be9b
// for ROLE_HOLDER_NOT_PROVEN): the row is kept and written again, each refusal logs a line naming
// the code and the issue, and the first refusal of any of the three codes in this daemon's run
// queues the controller's one status-scope notice (tellStatusScopeOnce). That notice tells a person
// how to give the daemon such a token (statusScopeRemedy).
func (r *outbox) publishRoleHolders(ctx context.Context, row record.OutboxRow, payload record.RoleHoldersPublish) error {
	if r.dispatch == nil {
		return errors.New("role holders executor has no Dispatch client")
	}
	var held []record.RoleHolder
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		held, err = r.records.RoleHolders(ctx, tx, payload.Issue)
		return err
	}); err != nil {
		return fmt.Errorf("read the role holders of %s: %w", payload.Issue, err)
	}
	holders := make([]dispatch.RoleHolder, len(held))
	for i, holder := range held {
		holders[i] = dispatch.RoleHolder{Role: string(holder.Role), Session: holder.Session}
	}
	err := r.dispatch.PutRoleHolders(ctx, payload.Issue, holders)
	if err == nil {
		return nil
	}
	published := fmt.Errorf("publish the role holders of %s: %w", payload.Issue, err)
	var refusal *dispatch.Error
	if !errors.As(err, &refusal) || refusal.Status != http.StatusForbidden ||
		(refusal.Code != roleHolderForbiddenCode && refusal.Code != roleHolderNotProvenCode) {
		return published
	}
	r.log.Warn(roleHolderRefusedLine,
		"row", row.ID, "issue", payload.Issue, "code", refusal.Code)
	if tellErr := r.tellStatusScopeOnce(ctx, row, fmt.Sprintf(
		"Dispatch refused the daemon's publication of the role holders of %s with %d %s. Dispatch takes a publication from a token that proves the session %s and holds the legion-status scope, and the token the daemon writes Dispatch with is not such a token. The daemon keeps the publication and writes it again. Dispatch lets the issue's architect write the status of the issues below it only once the daemon's own token has published that architect. %s",
		payload.Issue, refusal.Status, refusal.Code, dispatch.DaemonSession(payload.Issue), statusScopeRemedy(payload.Issue))); tellErr != nil {
		return errors.Join(published, tellErr)
	}
	return published
}

// publishStepRecord writes one start or one end of a step onto Dispatch's record of it, under the
// row's issue and the record's own id. Dispatch writes each field once and keeps what the write
// leaves out, so the start, the end, the rating and the variant land in any order, and a write
// that runs again after a lost answer, or after its variant waited, changes nothing.
//
// A Dispatch that has not got the route yet answers its router's 404, which execute makes a wait,
// so the daemon and Dispatch deploy in either order, and once Dispatch serves the route the row's
// next run writes the record and ends it. Every other answer is decided by recordRefused: a refusal
// its route makes for good ends the row, and anything else is a failure retried on the row's
// backoff.
func (r *outbox) publishStepRecord(ctx context.Context, row record.OutboxRow, payload record.StepRecord) error {
	if r.dispatch == nil {
		return errors.New("step record executor has no Dispatch client")
	}
	if err := r.dispatch.PutStep(ctx, row.Issue, payload.ID, stepWriteOf(payload)); err != nil {
		return fmt.Errorf("write the step record %s of %s: %w", payload.ID, row.Issue, err)
	}
	return nil
}

// publishStepRating writes the rating a retro gave the plan, which payload carries, onto the
// retro's record on Dispatch, under the row's issue and the record's own id, through the record's
// rating route (dispatch.Client.PutStepRating), with the record's identity, so the write makes the
// record where the retro's start and end have not landed yet. Its answers are decided as a step
// record's are (publishStepRecord).
func (r *outbox) publishStepRating(ctx context.Context, row record.OutboxRow, payload record.StepRecord) error {
	if r.dispatch == nil {
		return errors.New("step record executor has no Dispatch client")
	}
	if err := r.dispatch.PutStepRating(ctx, row.Issue, payload.ID, stepWriteOf(payload), dispatch.PlanRating(*payload.PlanRating)); err != nil {
		return fmt.Errorf("write the rating of the step record %s of %s: %w", payload.ID, row.Issue, err)
	}
	return nil
}

// publishStepExperiment writes the experiment and the variant of the record's issue, which payload
// carries, onto its record on Dispatch, under the row's issue and the record's own id, through the
// record's experiment route (dispatch.Client.PutStepExperiment), with the run's identity. run sends
// it only once the record's own write was taken, so a Dispatch older than that route still holds
// the record: its router's 404 makes the row wait (execute), and each run of the waiting row sends
// the record's own write again before the pair, which changes nothing Dispatch holds. Its answers
// are decided as a step record's are (publishStepRecord).
func (r *outbox) publishStepExperiment(ctx context.Context, row record.OutboxRow, payload record.StepRecord) error {
	if r.dispatch == nil {
		return errors.New("step record executor has no Dispatch client")
	}
	if err := r.dispatch.PutStepExperiment(ctx, row.Issue, payload.ID, stepWriteOf(payload), dispatch.StepExperiment(*payload.StepExperiment)); err != nil {
		return fmt.Errorf("write the variant of the step record %s of %s: %w", payload.ID, row.Issue, err)
	}
	return nil
}

// stepWriteOf is the step write of payload's fields, the rating and the experiment aside: the
// identity of the run, and each field of its start or its end that payload carries.
func stepWriteOf(payload record.StepRecord) dispatch.StepWrite {
	return dispatch.StepWrite{Task: payload.Task, Step: payload.Step, Kind: payload.Kind, Role: payload.Role,
		Type: payload.Type, Agent: payload.Agent, Generation: payload.Generation, Session: payload.Session,
		Model: payload.Model, StartedAt: payload.StartedAt, EndedAt: payload.EndedAt, Result: payload.Result,
		ReturnedTo: payload.ReturnedTo, Report: payload.Report}
}

// publishPlanRecord writes the plan one run of an issue carries out onto Dispatch, under the row's
// issue and the plan's own id, one for each run. The source, the document, the version and the
// tasks are one fact, which each write carries whole: Dispatch replaces all four for a version
// above the one it holds and changes nothing for one at or below it, a version left out (the
// source default) counting as 0, so a write that runs again after a lost answer, or arrives after a
// later version's, changes nothing. A Dispatch that has not got the route yet answers its router's
// 404, which execute makes a wait; every other answer is decided by recordRefused, as a step
// record's is.
func (r *outbox) publishPlanRecord(ctx context.Context, row record.OutboxRow, payload record.PlanRecord) error {
	if r.dispatch == nil {
		return errors.New("plan record executor has no Dispatch client")
	}
	tasks := make([]dispatch.PlanTask, len(payload.Tasks))
	for i, task := range payload.Tasks {
		tasks[i] = dispatch.PlanTask(task)
	}
	plan := dispatch.PlanWrite{Generation: payload.Generation, Source: payload.Source,
		PlanDocument: (*dispatch.PlanDocument)(payload.PlanDocument), Tasks: tasks}
	if err := r.dispatch.PutPlan(ctx, row.Issue, payload.ID, plan); err != nil {
		return fmt.Errorf("write the plan record %s of %s: %w", payload.ID, row.Issue, err)
	}
	return nil
}

// recordRefused is whether Dispatch refused a step record, a rating, a variant or a plan in a way
// no retry changes: an issue it does not have (404 ISSUE_NOT_FOUND), a body it will never take (a
// coded 400 the shared rule calls final, which leaves out the refusal of a field this daemon added
// by a Dispatch from before #2461; a refusal naming the rating is no such refusal, since no
// Dispatch refused the rating by name), a writer its route refuses (403 STEP_WRITER, which the
// step, the rating, the experiment and the plan routes all answer: the daemon names its own
// session on every write, so the route's rule and the daemon's disagree) and an id of another
// issue (409 STEP_OTHER_ISSUE, or PLAN_OTHER_ISSUE for a plan).
// Everything else retries: a handler's 404 NOT_FOUND, which the code, not the status, tells from
// ISSUE_NOT_FOUND, an outage, "come back later", a codeless answer and a credential an operator
// restores. The router's NOT_FOUND of a Dispatch older than the route never reaches it: execute
// makes that answer a wait.
func recordRefused(err error) bool {
	var refusal *dispatch.Error
	if !errors.As(err, &refusal) {
		return false
	}
	switch refusal.Status {
	case http.StatusNotFound:
		return refusal.Code == dispatch.IssueNotFound
	case http.StatusBadRequest:
		_, permanent := dispatch.PermanentRefusal(err)
		return permanent
	case http.StatusForbidden:
		return refusal.Code == steprecord.CodeWriter
	case http.StatusConflict:
		return refusal.Code == steprecord.CodeOtherIssue || refusal.Code == steprecord.CodePlanOtherIssue
	}
	return false
}

// message posts a message row once: a retried row that finds its own marker on the issue posts
// nothing more. A row whose message links an artifact puts that document on the issue first
// (messageArtifact), so the link never names a document that is not there yet.
func (r *outbox) message(ctx context.Context, row record.OutboxRow, payload record.MessagePost) error {
	if r.dispatch == nil {
		return errors.New("dispatch message executor has no Dispatch client")
	}
	marker := record.MessageMarker(row.ID)
	bodies, err := r.dispatch.MessageBodiesSince(ctx, row.Issue, row.CreatedAt.Add(-messageReadSkew))
	if err != nil {
		return fmt.Errorf("read Dispatch messages for %s: %w", row.Issue, err)
	}
	for _, body := range bodies {
		if strings.Contains(body, marker) {
			return nil
		}
	}
	if payload.Artifact != nil {
		refusal, err := r.messageArtifact(ctx, row, *payload.Artifact)
		if err != nil {
			return err
		}
		if refusal != nil {
			payload.Body += fmt.Sprintf("\n\nDispatch refused the full list that this message links, with status %d and code %s.", refusal.Status, refusal.Code)
		}
	}
	if err := r.dispatch.PostMessage(ctx, row.Issue, payload.Posted(row.ID)); err != nil {
		return fmt.Errorf("post Dispatch message for %s: %w", row.Issue, err)
	}
	return nil
}

// messageArtifact puts on the issue the document a message links. It reads the issue's artifacts
// and uploads only when none has the document's name, so a row that runs again after its upload
// writes no second version. A read or an upload Dispatch refuses the same way on every run
// (dispatch.PermanentRefusal) is logged and answered, and the message is posted all the same with
// a sentence that says so: the message is the issue's one word on what it carries, and a document
// it could not hold must not cost it that (dispatch://INFRA-2704). Any other failure fails the
// run, and the row runs again. So does an upload Dispatch filed under a slug other than the
// name, since the link names the document by that name.
func (r *outbox) messageArtifact(ctx context.Context, row record.OutboxRow, artifact record.MessageArtifact) (*dispatch.Error, error) {
	held, err := r.dispatch.Artifacts(ctx, row.Issue)
	if err == nil && slices.ContainsFunc(held, func(document dispatch.Artifact) bool { return document.Slug == artifact.Name }) {
		return nil, nil
	}
	slug := ""
	if err == nil {
		slug, err = r.dispatch.UploadArtifact(ctx, row.Issue, artifact.Name, artifact.Content)
	}
	if refusal, permanent := dispatch.PermanentRefusal(err); permanent {
		r.log.Error("outbox Dispatch message's document refused by Dispatch; the message is posted and says so",
			"row", row.ID, "issue", row.Issue, "artifact", artifact.Name, "status", refusal.Status, "code", refusal.Code, "error", err)
		return refusal, nil
	}
	if err != nil {
		return nil, fmt.Errorf("put the document %s of a Dispatch message on %s: %w", artifact.Name, row.Issue, err)
	}
	if slug != artifact.Name {
		return nil, fmt.Errorf("put the document %s of a Dispatch message on %s: Dispatch filed it under %s, so the message's link would name no document", artifact.Name, row.Issue, slug)
	}
	return nil, nil
}

// notice publishes a notice row to its architect (noticeArchitect), on that architect's own role
// topic, and to nothing else: a catch-up to the architect of its own issue, whose state it states,
// a reparented notice to the architect it names, and every other notice to the architect that
// owns its issue on the Dispatch chain (R18 of dispatch://INFRA-3320). Every notice kind is for an
// architect, and no claim subscribes to an issue topic (packages/pi-legion/src/claim-session.ts),
// so no issue topic carries one. The architect and the earlier notices it waits behind are read
// from one snapshot of the chains (notice_routing.go). A notice waits behind an earlier notice of
// the project that is held for the same architect, so each architect is told in the order the
// notices were written: on its backoff (errNoticeWaits), or, when the earlier row is itself due at
// a moment still ahead, at that moment and once (wait.Until). A notice whose owner walk meets a
// chain the copy cannot show to its top waits too, until the chain is known. A role topic with no
// live holder refuses the publish (notify.ErrNoHolder): while the architect's claim can hold its
// role again — relaunching, suspended by the operator, by the controller or by a stop of its issue
// (the operator's or the controller's suspension until that holder's resume, or the operator's,
// lifts it), or released for being idle — the row is held for it; once nobody will hold that role
// for this notice — the claim has failed or retired, or this daemon does not supervise it — the
// row finishes undelivered with one log line. So does a row whose architect cannot be resolved
// from the record (errNoticeUnroutable), which holds back no later notice either, and, before any
// publish, a catch-up while its issue is out of the workflow by its status or its phase
// (record.InWorkflow), whose next launch writes a fresh one. A person's spec decision in a notice
// that finishes undelivered because nobody will hold the role goes to the controller instead
// (parkDecision), and so does a notice of the issue pod's disk (tellControllerOfKeptPod).
// What the listener's answer to the publish then decides — a delivery the architect's session did
// not confirm, a holder that cannot take the payload, a forward the listener's own NATS grant
// denies, a holder only unresponsive, a role nobody holds — is the re-hold policy's
// (noticeAnswered, notice_exceptions.go), which a role-lane exception of an earlier listener
// reaches by the same route (rehold). A listener of this release never answers that a session
// did not confirm a delivery (`receipt_timeout`): no session confirms a forward any more, and
// every forward its stream did not store is `delivery_failed` (dispatch://INFRA-771), so that
// answer reaches the policy only from a listener of the deployed, older build, until that
// rollout finishes.
// The runner executes a row it leased from memory, so a row deleted under its lease since (a
// catch-up a newer ready dropped) is found gone in the tree's snapshot and finishes without a
// publish, and so does a catch-up a newer one superseded (catchUpSuperseded, read against the
// row's own issue as the snapshot holds it and its architect's claim as it runs now): the re-hold
// reads the claim from memory and writes its copy outside ApplyFact's lock, so it can lose the
// race to a ready and commit an older copy after the fresh catch-up. A catch-up that publishes
// records the telling and forgets the decisions it named (toldKept, settleTold); one a newer
// catch-up superseded hands them to a fresh catch-up instead (supersededKept), since the newer
// one was composed without them; one that finishes any other way leaves them claimed until its
// row's deletion releases them, for the architect's next ready to name. However the row ends, a
// catch-up that told its decisions in any run forgets them here rather than let its ending release
// what the architect has already heard.
//
// A row the wake pacing held to its turn (record.Notice.PacedAt) writes the line a wake writes,
// once, on the attempt that publishes it (noticeAnswered's wakeRan, notice_exceptions.go):
// running the row is the wake only if the architect is given the catch-up to read.
//
// A notice that is no catch-up is of the run and phase entry its issue stood at when it was
// queued (record.Notice's fence), and its delivery reads the run alone (record.LateAt, the
// architect's ruling of 2026-11-21 on dispatch://INFRA-3320): one of an earlier phase entry of the
// run the issue stands in is still news for its architect and is delivered, while one of an
// earlier run tells no architect anything of the issue as it stands now and finishes here,
// unpublished and unrouted (outlived). Its delivery starts and resumes no claim: it waits for an
// architect claim a stop suspended, and wakes only one an idle release suspended (deliverNotice).
// A catch-up is judged by its own supersession instead (deliverNotice), against its run and its
// architect's launch, since it names what the issue is owed whatever entry it stands at and what
// it claims is handed over only by that rule.
func (r *outbox) notice(ctx context.Context, row record.OutboxRow, payload record.Notice) error {
	if payload.CatchUp == nil {
		if late, err := r.outlived(ctx, row, payload); err != nil || late {
			return err
		}
	}
	if err := r.deliverNotice(ctx, row, payload); err != nil {
		return err
	}
	return r.settleTold(ctx, row, payload)
}

// wakeRan is the one line a wake the outbox runs writes, where it runs: the catch-up a start row
// wrote in place of a launch, which the pacing held until the moment it reserved (workflow's
// claimReady). It is the shape internal/supervise composes for every wake of the daemon,
// prepended with the claim this one wakes (supervise.WakeRanMessage, WakeRanFields): the shape is
// what matters to its reader, since one grep over the daemon's log finds every wake of a restart
// or a plugin wave, whichever lane caused it, and counts the wakes of a minute against the rate
// and burst each line names. The claim is the architect the catch-up goes to, the architect of
// the row's own issue (noticeArchitect; QueueCatchUp writes the row on that issue), spelled as a
// machine's own lines spell a claim, since this logger is the runner's and carries no claim of its
// own — where a machine's logger carries one already, which is why the field is this caller's to
// add.
//
// The line is written on the attempt that publishes, and so once for the row: a row that waits
// behind an earlier notice of the same architect (errNoticeWaits, wait.Until), finds its role
// topic unheld, or meets a transient publish failure runs again on its backoff, and a line per
// attempt would count one wake many times in the minute it lands in (scripts/wake-windows.py
// counts every paced line and builds a window for each).
//
// How long the wake waited is the row's own record, not a clock and not its next_at: the notice
// carries both the moment the pacing let it out and the moment its hold began, so PacedAt less
// PacedFrom is what the pacing held this wake for, whatever a retry since has written into
// next_at (record.Notice.PacedAt, PacedFrom, the store's RetryOutbox). The pair is one clock's:
// the row's created_at is the database's now() at the start of the fact's transaction, so a wake
// read against it would be the engine's clock less the database's. A row the pacer let straight
// out — the first wake of a wave, let out at the instant it joined, after the commit of the fact
// that wrote it — has PacedAt written as PacedFrom (record.ReleaseHeldCatchUp), so it does not
// report that transaction's or its commit's duration as a wait. It waited nothing and says so,
// as supervise's line does; reading the wait from the runner's clock instead would call every
// such row held, by however long the lane took to pick it up.
//
// The pace the line names is the one the daemon holds its machines to (supervise.WakePace, set in
// openSupervision), the rate and the burst alone. The queue is the pacer's second, which every
// catch-up waits in (catchUpPacing, dispatch://INFRA-5682 R2), and the turns are the ones its
// let-out recorded on the notice (record.Notice.PacedTurns), which a re-held copy keeps. A
// catch-up carries no message, so the line never says it came late for one. A test that builds
// the runner by hand over a supervisor with no pace reads the pacing off, which is that line's
// own reading of it.
func (r *outbox) wakeRan(row record.OutboxRow, notice record.Notice) {
	architect, _ := claim.NewToken(r.project, row.Issue, claim.RoleArchitect)
	pace := r.supervisor.deps.WakePace
	fields := append([]any{"claim", string(architect)},
		supervise.WakeRanFields(supervise.WakeCatchUp, true, notice.PacedAt.Sub(notice.PacedFrom),
			wakepace.Let{At: notice.PacedAt, Queue: wakepace.Second, Turns: notice.PacedTurns}, false, pace.Rate, pace.Burst)...)
	r.log.Info(supervise.WakeRanMessage, fields...)
}

// deliverNotice is the notice executor's own work: routing the notice, publishing it, and settling
// what a catch-up that will never publish named. Its endings are notice's to read.
func (r *outbox) deliverNotice(ctx context.Context, row record.OutboxRow, payload record.Notice) error {
	if r.notices == nil {
		return errors.New("notice executor has no Envoy publisher")
	}
	if r.supervisor == nil {
		return errors.New("notice executor has no claim supervisor")
	}
	issue, snapshot, err := r.readNoticeTree(ctx, row)
	if errors.Is(err, errNoticeRowGone) {
		r.log.Info("outbox notice finished without publishing: its row was deleted under its lease", "row", row.ID, "kind", payload.Kind, "issue", row.Issue)
		return nil
	}
	if errors.Is(err, errNoticeUnroutable) {
		r.log.Error("outbox notice finished undelivered: it has no architect", "row", row.ID, "kind", payload.Kind, "issue", row.Issue, "error", err)
		return nil
	}
	if err != nil {
		return err
	}
	// A catch-up goes to the architect of its own issue and to no other (noticeArchitect), so it is
	// judged against that architect's claim and that issue's generation: a child's catch-up is
	// superseded by the child's architect's later launch, never by the one above it. A notice whose
	// owner walk meets a chain the copy cannot show to its top waits (errNoticeWaits).
	architect, err := noticeArchitect(snapshot.noticeRoute, issue, payload, r.holdsItsRole)
	if err != nil {
		return fmt.Errorf("the architect of %s: %w", row.Issue, err)
	}
	if payload.CatchUp != nil {
		held := r.supervisedClaim(architect).Claim
		if catchUpSuperseded(*payload.CatchUp, held, &issue) {
			// A row that published before told what it named and settles after this run
			// (settleTold); one that never published hands its decisions to a fresh catch-up, since
			// the newer catch-up was composed from what nobody claimed and left them out.
			_, told := payload.Published(row.ID)
			if err := r.supersededKept(ctx, row.Issue, row.ID, told); err != nil {
				return err
			}
			r.log.Info("outbox catch-up finished without publishing: a newer catch-up supersedes it", "row", row.ID, "issue", row.Issue,
				"generation", payload.CatchUp.Generation, "launch", payload.CatchUp.Launch)
			return nil
		}
		// An issue out of the workflow (record.InWorkflow: by its status, or done, as a parked
		// issue stays when a person writes a running status on it without a todo) has its architect
		// suspended or retired with its launch and its generation kept, so nothing supersedes the
		// row. It finishes here, before any publish, since no ready queues a catch-up while the
		// issue is out of the workflow (workflow's claimReady, the same rule) and its next launch
		// writes a fresh one. A root is no exception: a stop leaves every issue's claims suspended
		// alike (R20 of dispatch://INFRA-3320).
		if !record.InWorkflow(issue) {
			r.log.Info("outbox catch-up finished undelivered: its issue is out of the workflow", "row", row.ID, "issue", row.Issue,
				"status", issue.Status, "phase", issue.Phase, "generation", payload.CatchUp.Generation, "launch", payload.CatchUp.Launch)
			return nil
		}
	}
	// The architect whose stop_task stopped the task is answered by its own call, so a
	// task-stopped notice that names it finishes here, and the one the issue's notices reach is
	// told only when that is another claim (record.Notice.StoppedBy). Published keeps the field,
	// so a re-held copy is judged the same way.
	if payload.StoppedBy == architect {
		r.log.Info("outbox notice finished without publishing: its architect stopped the task itself",
			"row", row.ID, "kind", payload.Kind, "issue", row.Issue, "architect", architect)
		return nil
	}
	notice, key := payload.Published(row.ID)
	// A notice whose owning architect's pod was released is that architect's still (holdsRole),
	// and this is the wake that brings it back for it: the row waits on its backoff, as it waits
	// for an architect that is relaunching, and publishes once the woken session holds its role.
	// A launch of the architect the wake pacing holds is waited for until it runs, the claim read
	// again at its turn or parkedWakeBound on, whichever comes first (parkedWait), and the wait is
	// a notice held for its architect still (errNoticeWaits).
	if waits, turn, err := r.wakeForDelivery(ctx, architect, "a notice of "+row.Issue, row.HeldByPause); err != nil {
		return err
	} else if !turn.IsZero() {
		return fmt.Errorf("%w: %w", errNoticeWaits, r.parkedWait(turn, fmt.Sprintf("notice row %d waits for %s's launch, which the wake pacing holds until %s",
			row.ID, architect, turn.UTC().Format(time.RFC3339))))
	} else if waits {
		return fmt.Errorf("%w: %s's pod was released and is coming back for notice row %d", errNoticeWaits, architect, row.ID)
	}
	if earlier, due, held := earlierNoticeFor(snapshot, architect, r.holdsItsRole); earlier != 0 {
		// A catch-up the wake pacing still holds in its queue has no moment of its own
		// (earlierNoticeFor's held, record.CatchUpIsHeld), so this row waits for the pacer's
		// estimate of its place (catchUpPacing.estimate), or parkedWakeBound where that comes
		// first (parkedWait): a change to the queues or to the pace can move the estimate earlier
		// or later, and a held row with no place yet has none, so the row reads again within the
		// bound.
		if held {
			estimate := r.catchUps.estimate(earlier)
			if !estimate.After(r.now()) {
				estimate = r.now().Add(parkedWakeBound)
			}
			return r.parkedWait(estimate, fmt.Sprintf("%s's notice row %d waits behind its catch-up, row %d, which the wake pacing holds",
				architect, row.ID, earlier))
		}
		// The row the fence holds this one behind is due at a known moment when the wake pacing
		// holds it: the moment the pacing reserved for a catch-up (record.Notice.PacedAt), or its
		// next read of the architect's parked launch, at most parkedWakeBound out (parkedWait).
		// This row runs once, then, rather than climbing its backoff to it — over a hold of tens
		// of minutes that is a Warn every attempt from the seventh (settle), about one wake's
		// worth of noise per architect in a wave. The hold itself is right: it keeps the
		// architect to the one wake its catch-up is.
		if now := r.now().UTC(); due.After(now) {
			return wait.Until(due, fmt.Sprintf("%s's notice row %d waits behind its row %d, due at %s",
				architect, row.ID, earlier, due.Format(time.RFC3339)))
		}
		return fmt.Errorf("%w: %s's notice row %d waits behind its row %d", errNoticeWaits, architect, row.ID, earlier)
	}
	published := r.publish(ctx, notify.RoleTopicPrefix+string(architect), noticeSummary(payload.Kind, row.Issue), notice, key)
	return r.noticeAnswered(ctx, row, payload, snapshot, architect, published)
}

// roleMessage publishes one message to a role's topic, keyed by the row so a retried row is one
// delivery (record.RoleMessage). A role with no live holder is told nothing and the row is done:
// the message reports a moment - a head's checks as they settled - that has passed by the time
// anyone claims the role, and every recipient reads the pull request on GitHub for itself, so
// waiting would retry forever for a message that is stale when it lands. A holder that refuses
// the message's delivery mode (notify.ErrRefusedByHolder), and the listener's own NATS grant
// denying the forward (notify.ErrPublishDenied), end the row the same way, since both refuse
// every attempt until a session or an operator changes them; a holder that did not confirm in
// time (notify.ErrNotDelivered) is retried on the row's backoff as any failed publish is. So is a
// holder the listener calls only unresponsive (notify.ErrHolderUnresponsive): its session is past
// the listener's live window but still holds the role, so the message is kept for it rather than
// dropped as a message to a role nobody holds (dispatch://INFRA-392).
//
// A role whose claim is released ends the row before the publish, with no wake (record.RoleMessage
// says why). The claim is read from its machine's View, as wakeForDelivery reads it. One window is
// left: a release between this read and the publish, which the listener refuses (holder_lapsed)
// and wakes the role on. An outbox with no supervisor (a test's), or a role no machine of this
// daemon holds, publishes as before.
//
// A row a project's pause held (record.OutboxRow.HeldByPause) is ended neither for its released
// claim nor for its role having no live holder while it is owed to its role (R4 of
// dispatch://INFRA-3759): a pause, not a release, took the session the message was for
// (store.StoppedByAPause), and without the pause the role would have had it. The row waits for the
// role to come back, and is published to the role's session once it is back. Its publish is what
// brings the role back: the listener answers that the role has no live holder
// (notify.ErrNoHolder), which for this row is a routine wait (routineWaiting), and reports the
// role's wake beside that answer, and the workflow's wake of a role the pause stopped is that
// pause's held wake, whose task says the pause stopped the session (workflow.WakeTask). The row
// takes no wake of its own, since a delivery wake resumes the role with no task. While the wake
// pacing holds the role's launch over no process, the row waits for that launch's turn
// (parkedLaunch, parkedWait), as a hook prompt does.
//
// The debt reads no ready: the woken agent says it is ready (supervise.SaidReady) before its
// plugin claims the role's topic, and the listener answers a publish in that gap as it answers the
// stopped session, so a live role is owed the row too. Nor does it ask which pause held the row: a
// second pause that begins before the role is back stops nothing of the released role but holds
// the row again, and its resume marks the row with that second pause. The row ends as any row once
// its publish reaches a holder. Which rows are owed, and the events alone that end the debt
// undelivered, are heldMessageOwed's, the one statement of the rule: the role is owed the row
// while its claim is not gone (supervise.Claim.Gone) or is released with an issue that can be
// woken. A row whose debt has ended takes the path above unchanged.
func (r *outbox) roleMessage(ctx context.Context, row record.OutboxRow, payload record.RoleMessage) error {
	if r.notices == nil {
		return errors.New("role message executor has no Envoy publisher")
	}
	// stopped is the pause of the stop under which the row is owed to its role, and 0 for a row
	// not owed.
	var stopped int64
	if r.supervisor != nil {
		held := r.supervisedClaim(claim.Token(payload.Role))
		var err error
		if stopped, err = r.heldMessageOwed(ctx, row, held); err != nil {
			return err
		}
		if stopped != 0 {
			if turn, parked := parkedLaunch(held); parked {
				return r.parkedWait(turn, fmt.Sprintf("%s's role message for %s, owed to the role since pause %d stopped it, waits for the role's launch, which the wake pacing holds until %s",
					row.Issue, payload.Role, stopped, turn.UTC().Format(time.RFC3339)))
			}
		} else if held.Released() {
			r.log.Info("outbox role message finished undelivered: its role's pod is released, and the message is not worth a wake",
				"row", row.ID, "issue", row.Issue, "role", payload.Role)
			return nil
		}
	}
	err := r.publish(ctx, notify.RoleTopicPrefix+payload.Role, payload.Body, payload.Body, record.OutboxKey(row.ID))
	if errors.Is(err, notify.ErrHolderUnresponsive) {
		// A wait, not a failure: settle logs a wait.ErrWaiting at INFO, then DEBUG, and at WARN
		// from attempt waitWarnAttempts on, rather than ERROR on every attempt, as the notice
		// executor's own unresponsive branch does (notice_exceptions.go).
		return wait.Errorf("%s's role message for %s waits for its holder, whose session the listener reports %s: %w",
			row.Issue, payload.Role, notify.StateUnresponsive, err)
	}
	if stopped != 0 && errors.Is(err, notify.ErrNoHolder) {
		// A wait as well: the role is coming back for the row, the listener reporting its wake
		// beside this answer, or its session is up and has not claimed the role's topic yet. It is
		// routine (routineWaiting): the role's return ends it, or one of the debt's end events
		// (heldMessageOwed) does, so nobody has to act on it, and a return that outlasts the
		// backoff's climb to its cap, about two minutes, would otherwise be logged as a warning at
		// every attempt from the seventh (waitWarnAttempts).
		return routineWaiting{err: wait.Errorf("%s's role message for %s waits for the role's session to hold its topic: pause %d stopped the role, which is coming back or has not claimed its topic yet: %w",
			row.Issue, payload.Role, stopped, err)}
	}
	var refused notify.ErrRefusedByHolder
	var denied notify.ErrPublishDenied
	switch {
	case errors.Is(err, notify.ErrNoHolder):
		r.log.Info("outbox role message finished undelivered: its role has no live holder, or its claim records a session it does not hold",
			"row", row.ID, "issue", row.Issue, "role", payload.Role, "error", err)
		return nil
	case errors.As(err, &refused):
		r.log.Info("outbox role message finished undelivered: its role's holder does not advertise the delivery mode",
			"row", row.ID, "issue", row.Issue, "role", payload.Role, "holder", refused.Holder)
		return nil
	case errors.As(err, &denied):
		r.log.Error("outbox role message finished undelivered: the listener's own NATS grant denied the forward",
			"row", row.ID, "issue", row.Issue, "role", payload.Role, "denial", denied.Message)
		return nil
	}
	if err != nil {
		return fmt.Errorf("publish %s's message to role %s: %w", row.Issue, payload.Role, err)
	}
	return nil
}

// heldMessageOwed is the held role message's debt, and the one statement of it (R4 of
// dispatch://INFRA-3759, roleMessage): the pause of the stop under which role message row is owed
// to the role of claim view, and 0 where it is not owed. A row a pause held
// (record.OutboxRow.HeldByPause) for a role a pause stopped, the claim's newest stop that ended
// `stopped` (store.StoppedByAPause), whichever pause made that stop and whichever held the row,
// stays owed until the role holds its topic and the row's publish reaches it. The debt ends
// undelivered on these events alone:
//
//  1. a pod release of the issue recorded after that stop (store.StoppedByAPause);
//  2. a takeover suspension: the claim suspended by any source but the release, an operator's,
//     the daemon's and a root's own exit among them;
//  3. a failure;
//  4. a retirement;
//  5. the issue leaving Legion's work: a released claim is owed only
//     while its issue can be woken (wakeableIssue), as the workflow's wake of a released role
//     reads it (workflow.wakeReleased).
//
// Events 2 to 4 leave the claim gone (supervise.Claim.Gone) and not released; every claim not
// gone, launch_uncertain among them, is on its way back or live. A later stop of the role that is
// still open, or that ended cancelled, taken over or failed, changes nothing; a later stop that
// ended `stopped` carries the debt itself, under its own pause and with its own pod-release
// comparison. A claim this daemon does not supervise has the zero view, whose issue is empty, and
// answers 0.
func (r *outbox) heldMessageOwed(ctx context.Context, row record.OutboxRow, view supervise.ClaimView) (int64, error) {
	if row.HeldByPause == 0 || view.Issue == "" {
		return 0, nil
	}
	if view.Gone() && !view.Released() {
		return 0, nil
	}
	pause, err := r.trees.StoppedByAPause(ctx, r.project, view.Issue, view.Token)
	if err != nil || pause == 0 {
		return 0, err
	}
	if view.Released() {
		if _, wakeable, err := r.wakeableIssue(ctx, view.Issue); err != nil || !wakeable {
			return 0, err
		}
	}
	return pause, nil
}

// phaseEnded publishes the message of a phase-ended row (record.PhaseEnded) to the Envoy topic of
// the worker whose phase the admission pass ended when it moved a parent into awaiting_children
// (workflow's StopForChildren), the claim of that role on the row's issue, and to nothing else. It
// is no architect notice: it carries one sentence of the daemon's own rather than a Notice, waits
// behind no other row of its issue (its own lane, record.LaneRoleMessage), and its recipient is the
// role the workflow named rather than whichever architect owns the issue. The message stops nothing
// it reaches: pi-envoy hands it to an Oh My Pi session at its next model turn and ends a running
// wait, the Claude Code bridge puts it in that session's inbox, and the role's turn, jobs and
// process run on.
//
// The row publishes with keep and names no source session, through the flow it shares with a hook
// prompt (publishKeptToRole, which says what each answer does to the row). A listener before
// cybermonklabs/legion#5412 answers a topic with no live holder kept: it holds the message on the
// role's kept lane for one hour and hands it to the next session that holds the role live in that
// hour, and the row ends with one log line, since the listener is then the message's keeper. A
// listener from that change on refuses to keep a publish that names no source session, since no
// sender would be told if the copy expired, and answers notify.ErrNoHolder with nothing kept, so
// the row is the message's keeper and waits, tried again on its backoff, as a hook prompt's row
// does. That is what reaches a released role: the row's first attempt wakes the role's pod and
// waits, and the attempts after the resume has launched publish, the first of them before the
// woken session has claimed the role, and one after it delivers. The row's end, a claim that
// failed or was retired or an hour that passed with no wake coming (keptRoleMessageEnded), ends
// the row with the Info line "outbox phase-ended message finished undelivered: no live session
// holds the role" and the end's why, and no message on the issue names it, as one names an
// undelivered hook prompt: the role's next task from the daemon names its phase anyway.
//
// The row is not held for a holder as an architect's notice is held: publishKeptToRole says why a
// kept copy or a waiting row can reach a later round's session and what bounds it, and the phases
// the message names tell its reader which round it was for.
//
// The three answers of the confirmed publish route are different failures and keep the ordinary
// retry: a holder that did not confirm in time (notify.ErrNotDelivered) can take it on the next
// attempt, a holder that does not advertise the delivery mode (notify.ErrRefusedByHolder) can be
// replaced by another session on the role, and a forward the listener's own NATS grant denies
// (notify.ErrPublishDenied) goes through once an operator changes that grant.
func (r *outbox) phaseEnded(ctx context.Context, row record.OutboxRow, payload record.PhaseEnded) error {
	if r.notices == nil {
		return errors.New("phase-ended executor has no Envoy publisher")
	}
	token, err := roleToken(r.project, row, payload.Role)
	if err != nil {
		return err
	}
	fields := []any{"row", row.ID, "issue", row.Issue, "role", payload.Role, "from", payload.From, "to", payload.To}
	ended, err := r.publishKeptToRole(ctx, row, token, keptRoleMessage{
		what: "the phase-ended message", subject: "the end of " + row.Issue + "'s " + string(payload.From) + " phase",
		who: "the role", after: "the phase ended", took: "the message", role: payload.Role, message: payload.Message(row.Issue),
		kept: "outbox phase-ended message finished: the listener keeps it for the role's next holder", fields: fields,
	})
	if ended != nil {
		r.log.Info("outbox phase-ended message finished undelivered: no live session holds the role", append(fields, "why", ended.why)...)
		return nil
	}
	return err
}

// retireStoppedTask runs a `retire_task` row (record.SuperviseRequest): the admission pass's move
// of a parent into awaiting_children stopped the phase its worker was in (workflow's
// StopForChildren), and the task that worker's claim holds for that phase goes
// (supervise.Machine.RetireStoppedTask), so no relaunch, fresh session or wake during the wait,
// and no send after the return to that same phase, carries the stopped phase's work. It stops
// nothing and starts no phase. The one prompt it can lead to is the claim's untold record of what
// its last let-go ended, sent alone in place of the retired task that would have carried it
// (supervise's relaunchAfterDrop).
//
// A row of an earlier run finishes without acting, as every supervise row of one does, at the
// generation check supervise makes before it hands the row here. The phase is not fenced: the
// row names the phase the issue has left, and the machine retires only a task of that phase, that
// run and an entry no later than the row's, so the task of the return's own start is never one it
// takes. A claim this daemon does not supervise holds no task to retire. A task on its way to the
// agent keeps the row (supervise.ErrTaskSending, a wait), which runs again once the send has
// settled.
func (r *outbox) retireStoppedTask(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest, issue record.Issue) error {
	token, err := claim.NewToken(r.project, row.Issue, payload.Role)
	if err != nil {
		return fmt.Errorf("derive claim for outbox row %d: %w", row.ID, err)
	}
	machine, found := r.supervisor.Machine(token)
	if !found {
		return nil
	}
	retired, err := machine.RetireStoppedTask(ctx, payload.Phase, payload.Generation, payload.Entry)
	if err != nil {
		return fmt.Errorf("retire the %s task of %s's %s: %w", payload.Phase, row.Issue, payload.Role, err)
	}
	r.log.Info("outbox retired the task of a stopped phase", "row", row.ID, "issue", issue.Key, "role", payload.Role,
		"phase", payload.Phase, "entry", payload.Entry, "held", retired)
	return nil
}

// hookPrompt publishes the words of a hook-prompt row (record.HookPrompt) to the Envoy topic of
// the role the deployment's hook named, the claim of that role on the row's issue, and to nothing
// else. The row is the daemon's own facts of the firing — the hook, the move back and the counts —
// and then the deployment's own words for the round that move back opened, unchanged
// (record.HookPrompt.Message). It rides the role-message lane (record.LaneRoleMessage), so it
// waits behind no other row of its issue and reaches the role while that round is the round the
// role works. It stops nothing it reaches: the session takes it at its next model turn, and the
// role's turn, jobs and process run on.
//
// The row publishes with keep and names no source session, since the daemon is none, through the
// flow it shares with a phase-ended message (publishKeptToRole, which says what each answer does
// to the row). A listener that keeps such a publish for a role no live session holds answers it
// kept, holding the prompt on the role's kept lane for one hour for the next session that holds the
// role live, and the row ends with one log line, the listener being the prompt's keeper. A listener
// that refuses to keep it — the Envoy listener keeps nothing for a publish naming no source
// session, since no notice of an expired copy could reach its sender — answers notify.ErrNoHolder,
// and the row is then the prompt's keeper: it waits on its backoff while a session of the role can
// still come within keptRoleMessageHoldBound of the row's making, or while a wake of the role is
// coming. Its end, a claim that failed or was retired or an hour that passed with no wake coming
// (keptRoleMessageEnded), is a failure no retry repairs, which finishes the row undelivered with a
// message on the issue (permanentRefusal, finishRefused, tellHookPromptUndelivered). A released
// role's woken session is the next holder either way: the row's first attempt wakes the pod and
// waits, and the attempts after the resume has launched publish, the first of them before the
// woken session has claimed the role.
//
// The hook wrote its words for this round, and nothing repeats them: the move back they were
// written for has been made, and no later task of the daemon's carries them, so the kept copy or
// the row is the prompt's one keeper. An unheld topic is besides the ordinary state for a role an
// issue has never had — the deployment's default hook writes a row for the reviewer, and the work
// of an issue may never have reached one — so a reviewer started inside the hour can be handed an
// earlier round's prompt: publishKeptToRole says why that is intended and what bounds it, and the
// move back and the counts the prompt names tell its reader which round it was for.
//
// The three answers of the confirmed publish route are different failures and keep the ordinary
// retry: a holder that did not confirm in time (notify.ErrNotDelivered) can take the prompt on the
// next attempt, a holder that does not advertise the delivery mode (notify.ErrRefusedByHolder) can
// be replaced by another session on the role, and a forward the listener's own NATS grant denies
// (notify.ErrPublishDenied) goes through once an operator changes that grant.
func (r *outbox) hookPrompt(ctx context.Context, row record.OutboxRow, payload record.HookPrompt) error {
	if r.notices == nil {
		return errors.New("hook prompt executor has no Envoy publisher")
	}
	token, err := roleToken(r.project, row, payload.Role)
	if err != nil {
		return err
	}
	ended, err := r.publishKeptToRole(ctx, row, token, keptRoleMessage{
		what: "the hook prompt", subject: "the " + payload.Hook + " hook's prompt for " + row.Issue,
		who: "the " + string(payload.Role), after: "the hook fired", took: "it", role: payload.Role, message: payload.Message(row.Issue),
		kept:   "outbox hook prompt finished: the listener keeps it for the role's next holder",
		fields: []any{"row", row.ID, "issue", row.Issue, "hook", payload.Hook, "role", payload.Role, "count", payload.Count},
	})
	if ended != nil {
		return ended
	}
	return err
}

// updateAsk publishes an update round's ask (record.UpdateAsk) to the Envoy topic of the role the
// row names, the claim of that role on the row's issue, and to nothing else. The text is the
// sweep's, built for the round when it opened (supervise.UpdateTask), and it reaches a worker and
// the issue's architect alike: every role whose process runs in the pod loses that process when
// the round replaces the pod. It rides the role-message lane (record.LaneRoleMessage), so it
// waits behind no architect notice of its issue.
//
// It wakes nothing, where mergeQueueRemoval, mergeHoldsChanged and mergeForwardMessage call
// wakeForDelivery first, and phaseEnded and hookPrompt its body, deliveryWake, through
// publishKeptToRole. The round does not ask a claim the daemon suspended (its own rule, a root's
// exit, an idle release, or an unknown source), and does not wait for it (dispatch://INFRA-4109
// R1, the sweep's roundAsks). A claim a project's pause stopped is the one exception while the
// pause still keeps it (the sweep's podHolds.owes, roundClock.passesOver): the pause suspends it
// as released, and the round waits for it and the sweep asks it once its agent is ready.
// A row the sweep queued before any such suspension still runs, so the machine refuses the ask of
// every claim the daemon suspended, a claim the pause stopped included, before any publish
// (supervise.Machine.PublishUpdateAsk answers UpdateAskSuspended), and the row finishes with one
// line of its own. A publish would go to a role with no holder, and the Envoy listener raises a
// role wake for every publish to a role with no holder (packages/envoy/cmd/listener/confirmed.go,
// roleSend.noHolder). That wake would bring back a pod that an idle release took, or a role that
// the pause stopped, and the round brings back neither.
//
// An outbox row cannot be withdrawn, and the round it names can end while the row waits. So the
// row reads the claim before it publishes, through the machine's view as every other read of a
// claim here does (supervisedClaim), and finishes without a publish where the claim no longer
// records the row's round: the round ended, another round replaced it, or this daemon no longer
// supervises the claim. A role is never asked about a round that is not its own.
//
// The publish and the claim's ask mark are one step of the claim's machine
// (supervise.Machine.PublishUpdateAsk), whose comment holds the order and why it neither loses an
// ask nor gives one twice. In short: the machine's lock is taken without waiting, the claim is
// read under it, the ask is published, and the claim is marked asked only once the publish
// reached a session, all before the row can end. A machine whose lock is held is a wait: this
// issue's role-message lane runs one row at a time (record.LaneKey), and a Sandbox relaunch holds
// that lock for minutes, so waiting for it would hold every role message of this issue behind one
// decision. The row keeps its place, nothing was published, and its next attempt is the first
// publish. A claim an earlier ask of the round already reached is published nothing, so a second
// row of one round asks no role twice. A claim that holds an answer of the round is published
// nothing either (supervise.Machine.PublishUpdateAsk answers UpdateAskAnswered), and the row
// finishes with one line of its own: the sweep read the claim before the row ran, and the role
// answered in between. A claim the daemon suspended after it answered ends there too, on that
// line, rather than at UpdateAskSuspended.
//
// A topic with no live holder (notify.ErrNoHolder) is logged on one line and ends the row with
// nothing marked: the ask is lost, not given, and the sweep's next reading of the round asks the
// claim again (askRound). The round waits for every claim it asks until its bound, and a claim
// the ask never reached has nothing to answer: a fresh session that has not claimed its topic yet
// is the ordinary case, and a mark that counted such an ask as given held BET-1291's and BET-690's
// rounds for hours on 2026-11-17 (dispatch://INFRA-4109 R4). Every other failure of the publish
// keeps the ordinary retry, as hookPrompt's does: a holder that did not confirm in time, one that
// does not advertise the delivery mode, and a forward the listener's own NATS grant denies can
// each go through on a later attempt. A mark the store did not take keeps the row too, and the
// next attempt writes it and publishes nothing.
func (r *outbox) updateAsk(ctx context.Context, row record.OutboxRow, payload record.UpdateAsk) error {
	if r.notices == nil {
		return errors.New("update ask executor has no Envoy publisher")
	}
	if r.supervisor == nil {
		return errors.New("update ask executor has no claim supervisor")
	}
	token, err := roleToken(r.project, row, payload.Role)
	if err != nil {
		return err
	}
	machine, found := r.supervisor.Machine(token)
	if current := r.supervisedClaim(token).UpdateRound; !found || current != payload.Round {
		r.updateAskLeftRound(row, payload, token, current)
		return nil
	}
	topic := notify.RoleTopicPrefix + string(token)
	fields := []any{"row", row.ID, "issue", row.Issue, "round", payload.Round, "pod", payload.Pod, "claim", string(token), "role", payload.Role, "topic", topic}
	outcome, err := machine.PublishUpdateAsk(ctx, payload.Round, func(ctx context.Context) (bool, error) {
		return r.publishToRole(ctx, row, topic, payload.Message(row.Issue),
			"outbox update ask finished undelivered: no live session holds the role", fields...)
	})
	switch {
	case errors.Is(err, supervise.ErrMachineBusy):
		return wait.Errorf("the ask of round %s for %s waits for its %s's machine, whose lock is held; nothing is published yet: %w",
			payload.Round, row.Issue, payload.Role, err)
	case err != nil && (outcome == supervise.UpdateAskDelivered || outcome == supervise.UpdateAskAlreadyDelivered):
		return fmt.Errorf("the ask of round %s for %s reached its %s, and the claim write that marks it asked is not made yet: %w",
			payload.Round, row.Issue, payload.Role, err)
	case err != nil:
		return fmt.Errorf("publish the ask of round %s for %s to its %s: %w", payload.Round, row.Issue, payload.Role, err)
	}
	switch outcome {
	case supervise.UpdateAskDelivered:
		r.log.Info("outbox update ask published to the role topic", fields...)
	case supervise.UpdateAskAlreadyDelivered:
		r.log.Info("outbox update ask finished without a publish: an earlier ask of its round reached the role", fields...)
	case supervise.UpdateAskOutOfRound:
		r.updateAskLeftRound(row, payload, token, machine.View().UpdateRound)
	case supervise.UpdateAskSuspended:
		r.log.Info("outbox update ask finished without a publish: the daemon suspended the claim", fields...)
	case supervise.UpdateAskAnswered:
		r.log.Info("outbox update ask finished without a publish: the role has answered its round", fields...)
	}
	return nil
}

// updateAskLeftRound is the one line of an update-ask row that finishes without a publish because
// the claim no longer records the row's round, current being the round it records now.
func (r *outbox) updateAskLeftRound(row record.OutboxRow, payload record.UpdateAsk, token claim.Token, current string) {
	r.log.Info("outbox update ask finished without a publish: the claim no longer records its round",
		"row", row.ID, "issue", row.Issue, "round", payload.Round, "pod", payload.Pod, "current", current, "claim", string(token), "role", payload.Role)
}

// roleToken is the claim of role on the row's issue in project, whose Envoy topic a role message
// of the row is published to (publishToRole and publishKeptToRole, under notify.RoleTopicPrefix).
func roleToken(project string, row record.OutboxRow, role claim.Role) (claim.Token, error) {
	token, err := claim.NewToken(project, row.Issue, role)
	if err != nil {
		return "", fmt.Errorf("the %s of %s: %w", role, row.Issue, err)
	}
	return token, nil
}

// publishToRole publishes message to topic, the Envoy topic of one role's claim
// (notify.RoleTopicPrefix and the claim's token), under the row's own key, so a retried row is one
// delivery. It is updateAsk's publish, with keep: false; phaseEnded and hookPrompt publish with
// keep through publishKeptToRole, which also says how each executor that publishes to a role's
// topic answers the publisher.
//
// A topic with no live holder (notify.ErrNoHolder) ends the row: noHolder is logged at Info with
// fields, and the answer is false with no error. Every other failure is returned as the
// publisher gave it, and the row keeps its retry. The answer is true when the publisher took the
// message.
func (r *outbox) publishToRole(ctx context.Context, row record.OutboxRow, topic, message, noHolder string, fields ...any) (bool, error) {
	err := r.publish(ctx, topic, message, message, record.OutboxKey(row.ID))
	if errors.Is(err, notify.ErrNoHolder) {
		r.log.Info(noHolder, append(fields, "error", err)...)
		return false, nil
	}
	return err == nil, err
}

// controllerNotice publishes a controller notice row to the controller topic of the daemon's own
// project, the one its controller subscribes to, and to nothing else: the Notice it is, under the
// row's own key. It is of the run and phase entry of the row's issue when it was queued, as an
// architect's notice is, and its delivery reads the run alone (record.LateAt): one of an earlier
// run finishes unpublished at run's fence check (fencedHere, outlived), before this is reached. A
// row keyed by the project, or by an issue the daemon records nothing of, names the first fence
// and is never late.
func (r *outbox) controllerNotice(ctx context.Context, row record.OutboxRow, payload record.ControllerNotice) error {
	if r.notices == nil {
		return errors.New("controller notice executor has no Envoy publisher")
	}
	err := r.publish(ctx, notify.ControllerTopic(r.project), noticeSummary(payload.Kind, row.Issue), record.Notice(payload), record.OutboxKey(row.ID))
	if errors.Is(err, notify.ErrNoHolder) {
		// A wait, not a failure, as a merge-queue removal's is: settle logs a wait at INFO, then
		// DEBUG, and at WARN from attempt waitWarnAttempts on, rather than an Error at every
		// attempt, and the row runs again on its backoff, so an operator's settle of the
		// controller claim's name lets the next attempt through.
		return wait.Errorf("the controller notice for %s waits for the controller's claim to hold its session: %w", row.Issue, err)
	}
	if err != nil {
		return fmt.Errorf("publish controller notice for %s: %w", row.Issue, err)
	}
	return nil
}

// startRetry is the retry a start row asks of a failed or retired claim (supervise.RequestRetry):
// a new session where the row asks for one, and a decided retry where a person, the controller or
// the issue's architect decided the start (record.SuperviseRequest.Decided: the held phase's retry
// and the start of a promoted root's architect). Every other start of such a claim is one the
// daemon makes by itself, which the machine refuses for a claim held for OpenAI's cyber-policy
// refusal (supervise.ErrHeldForCyberPolicy, heldStartRefused).
func startRetry(token claim.Token, payload record.SuperviseRequest) supervise.RequestRetry {
	return supervise.RequestRetry{Claim: token, FreshSession: payload.FreshSession, Decided: payload.Decided}
}

// retryRefused is a start row's answer to the machine's refusal of its retry (startRetry): the
// refusal of a claim held for OpenAI's cyber-policy refusal is heldStartRefused's to answer, and
// every other is the row's error, as before, naming what the row asked (`retry claim`, `launch
// again the agent of retired claim`), the claim and the refusal.
func (r *outbox) retryRefused(ctx context.Context, row record.OutboxRow, issue record.Issue, payload record.SuperviseRequest,
	machine *supervise.Machine, err error, asked string) error {
	if !errors.Is(err, supervise.ErrHeldForCyberPolicy) {
		return fmt.Errorf("%s %s: %w", asked, machine.Claim().Token, err)
	}
	return r.heldStartRefused(ctx, row, issue, payload, machine.Claim(), err)
}

// heldStartRefused finishes a start row whose retry the machine refused for a claim held for
// OpenAI's cyber-policy refusal, held as it stands. No start of the daemon's own may launch that
// claim's agent again, and nothing the daemon does later lifts the hold, so a row kept would retry
// for ever. The line is an Error, since the claim's role runs no more until a person or the
// issue's architect decides, and it carries the refusal alone, which names the operator's command
// (supervise.Claim.CyberPolicyRefusal).
//
// A start of the issue's phase by the phase worker that works it — the row's phase is the issue's,
// which the fence above holds it to, its role is the one that phase's RoleFor answers, and it is
// no production check's look — is the one start that meets the held claim with the phase waiting
// on it, and nothing would tell anyone: a start of that phase that came by any road but the held
// phase's retry (a todo that brings back a held issue a stop took out of Legion's work, whose
// release starts the held phase's worker at a new entry, workflow's ReturnToWork; the first phase
// of a new run, the architect's park_child and then rerun_child), or a row of the held phase's
// retry that a release before record.SuperviseRequest.Decided wrote. So the claim's failure is
// told to the workflow again, built from the claim as it stands (intake.ClaimFailedOf), which
// holds the issue in that phase and tells its architect (workflow's claimFailed), whose retry then
// starts the agent again in a new session. The fact's event is this row's, so a run of the row
// again applies it once, and admission's repair passes over a held claim (admit's owedOf), so no
// second row of the daemon's own reaches it. Every other start of a held claim tells nothing: an
// architect's, which works no phase, at admitted, where RoleFor answers the architect, as at every
// other, since the worker-died notice of its own failure told the architect above it already; and
// a production check's look, which ends at its bound as a look whose role never reported.
func (r *outbox) heldStartRefused(ctx context.Context, row record.OutboxRow, issue record.Issue, payload record.SuperviseRequest,
	held supervise.Claim, err error) error {
	r.log.Error("outbox start finishes without acting: its claim is held for OpenAI's cyber_policy refusal of its session's content, and no start of the daemon's own launches its agent again",
		"row", row.ID, "issue", row.Issue, "role", payload.Role, "phase", payload.Phase, "claim", held.Token, "error", err)
	if payload.CheckLook > 0 || payload.Phase == "" || payload.Phase != issue.Phase || payload.Role == claim.RoleArchitect ||
		workflow.RoleFor(issue.Phase) != payload.Role {
		return nil
	}
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("start-held:%s:%d", held.Token, row.ID),
		intake.ClaimFailedOf(held, r.supervisor.deps.Limits), r.handlers...); err != nil {
		return fmt.Errorf("hold %s, whose %s is held for OpenAI's cyber_policy refusal: %w", issue.Key, payload.Role, err)
	}
	return nil
}

func (r *outbox) supervise(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest) error {
	if r.supervisor == nil {
		return errors.New("supervise executor has no claim supervisor")
	}
	// A pause's stop is fenced on its pause alone (pauseStop): it stops every live role of the
	// project whatever the issue's generation, phase or tree, as the pause asked it to.
	if payload.Op == "pause_stop" {
		return r.pauseStop(ctx, row, payload)
	}
	issue, err := r.issue(ctx, row.Issue)
	if err != nil {
		return err
	}
	// A check start (CheckLook) runs after the close: its issue holds no phase and its generation
	// may be a later run's, so it is fenced on the open check — the instance the row names — and
	// its look (checkLookHolds) where a phase start is fenced on the generation and the phase.
	var check *record.ProductionCheck
	if payload.CheckLook > 0 {
		if check, err = r.checkLookHolds(ctx, row, payload, issue); err != nil || check == nil {
			return err
		}
	} else {
		if payload.Generation != issue.Generation {
			r.log.Info("outbox supervise row serves an earlier generation; finished without acting", "row", row.ID, "issue", issue.Key,
				"generation", payload.Generation, "current", issue.Generation, "op", payload.Op, "role", payload.Role)
			return nil
		}
		// A task's retirement names the phase that was stopped, which the issue has left by
		// design — it waits in awaiting_children, or has come back to that phase on an entry of
		// its own — so it is decided here, after the generation check and before the phase and
		// entry fences below, which would finish it unrun. Its own bounds are the run and the
		// entry it names (retireStoppedTask). It carries no check look (record's validation
		// refuses one on any operation but a start), so it always reaches this branch.
		if payload.Op == "retire_task" {
			return r.retireStoppedTask(ctx, row, payload, issue)
		}
		if payload.Phase != "" && payload.Phase != issue.Phase {
			r.log.Info("outbox start serves a phase the issue has left; finished without acting", "row", row.ID, "issue", issue.Key,
				"phase", payload.Phase, "current", issue.Phase, "role", payload.Role)
			return nil
		}
		// A start carries the phase entry its task is of (record.SuperviseRequest.Entry), and the
		// role's phases row names the entry its phase is on now (record.PhaseRow.Entry). A row
		// below that number carries work a later entry of the same phase has superseded, and that
		// entry's own start carries that entry's task, so running this one would hand the worker
		// work the record has moved past. Finishing it here is what lets a start row be a message
		// with no state of its own: a row that outlives its entry is harmless rather than
		// something to rewrite.
		//
		// Only a row that carries an entry and names a phase is held to it: a production check's
		// look carries neither and is fenced on its check above, every start of no entry
		// (record.SuperviseRequest.Entry, which lists them) is held to the generation alone, and
		// so is a root architect's start, which carries an entry and names no phase, since that
		// architect serves every phase of its tree and its row must act whatever phase the root
		// stands in. What keeps one entry to one task for the architect is the claim's own rule:
		// a delivery of an entry it was already queued a task of queues no second one
		// (supervise.Claim.GivenEntry).
		if payload.Entry > 0 && payload.Phase != "" {
			entry, err := r.phaseEntry(ctx, row.Issue, payload.Role)
			if err != nil {
				return err
			}
			if payload.Entry < entry {
				r.log.Info("outbox start serves an entry its role has left; finished without acting", "row", row.ID, "issue", issue.Key,
					"phase", payload.Phase, "entry", payload.Entry, "current", entry, "role", payload.Role)
				return nil
			}
		}
	}
	token, err := claim.NewToken(r.project, row.Issue, payload.Role)
	if err != nil {
		return fmt.Errorf("derive claim for outbox row %d: %w", row.ID, err)
	}
	machine, found := r.supervisor.Machine(token)
	switch payload.Op {
	case "start":
		// A wake's start (WakeKind) brings a released role back, and one a park, a close or a
		// moved phase took over while the row waited — the pause holds a wake's row for as long as
		// it stands — is that path's to start again, not the wake's: the row finishes here, above
		// StartedBy, so it records no start against the claim (decision 10 of dispatch://INFRA-3759).
		if payload.WakeKind != "" && found {
			if held := machine.Claim(); held.State == supervise.StateSuspended && held.SuspendedBy == supervise.SuspendedByDaemon {
				r.log.Info("outbox wake start of a role the daemon took over; finished without acting", "row", row.ID,
					"issue", issue.Key, "role", payload.Role, "wake", payload.WakeKind, "pause", payload.Pause)
				return nil
			}
		}
		// An issue out of Legion's work — closed, stopped, sent back to triage — has had its
		// Sandbox given back by the leave that took it out, so a start that reaches the runner
		// behind that leave would build the pod again and pick up the claim that leave had just put
		// down. The row returns here, above machine.StartedBy, so it records no start against the
		// claim: the leave's own stop is not superseded by it, and the suspension that follows that
		// stop is not dropped for a start that started nothing. admit.(*Admission).finishFencedStarts
		// deletes an owed start of the same shape on the same predicate, so a row that never reaches
		// a lane and one that does are declined alike.
		//
		// The read is the issue's own record (record.InLegionsWork, R20 of dispatch://INFRA-3320):
		// its recorded Status and not its DispatchStatus, since a close the daemon authored writes
		// Status with the rows that carry it out and leaves DispatchStatus at what the board last
		// showed until Dispatch's echo arrives, while Engine.keepStatus writes DispatchStatus and
		// keeps Status for a status the daemon sets back, which leaves the issue in work and its
		// start a start. No other issue's record is read: an issue's work is its own. A production
		// check's look is exempt — it is the one start an issue out of work still runs — which the
		// enclosing payload.CheckLook == 0 gives it.
		if payload.CheckLook == 0 && !record.InLegionsWork(issue) {
			r.log.Info("outbox start of an issue out of Legion's work; finished without acting", "row", row.ID,
				"issue", issue.Key, "status", issue.Status, "phase", issue.Phase, "role", payload.Role)
			return nil
		}
		// The role of a stopped task's step starts again only when its task does
		// (workflow.StoppedStepRole): the start that a later version of the plan, or a person's
		// todo on a stopped root, queues for it comes with the hold cleared. Every other start of
		// that role while the hold stands, a wake's among them, finishes here, the one place every
		// start row of a role runs, above StartedBy, so it records no start and the claim stays
		// where the stop left it.
		if payload.CheckLook == 0 && workflow.StoppedStepRole(issue) == payload.Role {
			r.log.Info("outbox start of the role of a stopped task's step; finished without acting", "row", row.ID,
				"issue", issue.Key, "role", payload.Role, "from", issue.Hold.From)
			return nil
		}
		news, finished, err := r.startNewsOf(ctx, row, issue, payload, machine, found, token)
		if err != nil || finished {
			return err
		}
		// A suspension that holds — the operator's or the controller's — holds its
		// claim until a resume lifts it: the start, and the task it gives, wait on the row's
		// backoff. StartedBy refuses the same in the claim's own decision. A check start does not
		// wait: its look cannot run, and ends, its cause naming the holder (checkLookHeld).
		if found {
			if held := machine.Claim(); held.RuleHolds() {
				if payload.CheckLook > 0 {
					return r.checkLookHeld(ctx, row, payload, *check, held.Holder())
				}
				return fmt.Errorf("start %s of %s: %w", payload.Role, issue.Key, held.RuleHoldError())
			}
		}
		// The issue's gate root and whether it holds its own design gate are read on the Dispatch
		// chain at each start and written on the claim (supervise.Machine.MarkLaunch), since
		// supervise holds no store: the gate root is what the launch names as its LEGION_TREE and
		// the label of a pod it makes (decision 13 of dispatch://INFRA-4730), and the mark is what
		// the machine reads a gate holder's architect by, for the stop refusal, the exit
		// suspension, the superseding of a task and the root's hearing guard (R18 of
		// dispatch://INFRA-3320). The tree the claim stores is its issue's stored tree, which the
		// claim's admission writes for the release before (store.Store.AdmitClaim) and no decision
		// reads.
		launch, err := r.launchOf(ctx, issue)
		if err != nil {
			return err
		}
		if !found {
			if err := r.provisionWorkspace(ctx, issue); err != nil {
				return err
			}
			machine, _, err = r.supervisor.Create(ctx, supervise.Claim{
				Token: token, Project: r.project, Tree: issue.Tree, LaunchTree: launch.tree, Issue: issue.Key, Role: payload.Role,
				State: supervise.StateQueued, HoldsGate: launch.holdsGate,
			}, "")
			if err != nil {
				return fmt.Errorf("create claim %s: %w", token, err)
			}
		} else {
			if err := machine.MarkLaunch(ctx, launch.tree, launch.holdsGate); err != nil {
				return fmt.Errorf("mark the launch of claim %s: %w", token, err)
			}
			// An architect whose process runs under another gate root is relaunched in its
			// session under this one, at its turn's end, so its plugin registers that gate root;
			// one with no process launches under it by the mark above (supervise.GateRootMoved,
			// R22 of dispatch://INFRA-3320). A phase worker keeps the LEGION_TREE of its launch
			// until its next one.
			if payload.Role == claim.RoleArchitect {
				if err := machine.Handle(ctx, supervise.GateRootMoved{Claim: token, Tree: launch.tree}); err != nil {
					return fmt.Errorf("relaunch claim %s under its gate root %s: %w", token, launch.tree, err)
				}
			}
		}
		// The task this start gives the claim, nil for a start that carries none. It is built here,
		// before the start is recorded, because the record reads it: a task the claim holds back
		// for a root that had no architect to hear it goes at the start, unless this start's own
		// task takes its place (supervise.Machine.StartedBy), and that is decided against the
		// request delivered below, so the two never read different starts.
		//
		// The row's phase, already held to the issue's above, travels with the delivery: it is
		// what says the task is still the work to do once the delivery has outlived its id. The
		// row's entry travels with it too, the number the start's own transaction raised on the
		// role's phases row (record.PhaseRow.Entry): it is what the completion this task's turn
		// reports is held to (api/handoff.go), and what says a later row carrying the same
		// entry queues no second task of it (supervise.Claim.GivenEntry). A start that hands
		// the role the work of the entry its row already names travels with the mark that
		// says so (record.SuperviseRequest.ResendEntry), and the claim takes its task though
		// it was given that entry. A check start's look and check instance travel the same way,
		// and attribute a report that names its check to the look this task gave the worker
		// (supervise.Claim.ServingCheck, api/handoff.go). The row travels as well: the claim
		// writes it with the delivery, so a retry of this row (a
		// settle or a ClaimReady below that failed, a crash before the row finished) queues the
		// task again neither while it is pending nor once the claim has retired it
		// (supervise.Claim.DeliveredRows), and that same record is what says, when the row
		// settles what its task named, that the claim has it (settleKept). What the task names
		// of what its tree is owed travels with it as well (supervise.Delivery.Kept): the
		// claim's write of the delivery takes those rows from this start row and holds them
		// under that claim and delivery id, and the write recording the turn's confirmation
		// tells them (the claim store's putDelivery, 20261101163106_parked_decisions). So does the
		// fact that a reviewer's task ends with the daemon's two sentences of its review round
		// (record.SuperviseRequest.RoundNamed, or the round newsForStart names on a reviewer's start
		// queued for news alone: startNews.roundNamed): the one place each prompt is composed reads
		// it there, and gives a reviewer's task of a row that lacks it, which only a row of the
		// release before this one can be, those sentences naming no commit and no entry
		// (supervise's Delivery.message). The news the task carries travels the same way
		// (supervise.Delivery.News): the task is the start's own text and the news after it, the
		// round's two sentences after the news where the task names them (renderNews), the send
		// renders that news again from the record (the daemon's designNews), and the write
		// recording the turn's confirmation makes what the prompt of that turn carried the
		// delivery's credit, which the write recording the turn's result folds into the told rows
		// (supervise.Delivery.Carried, the claim store's settleDesignNews). A start queued for news
		// alone travels marked so (supervise.RequestDeliver.NewsOnly, stored as
		// pending_task_deliveries.news_only): its task holds no work, so the machine refuses it
		// behind any task that does, the one a running turn works among them, and the row waits on
		// its backoff until that turn has ended. A task of news alone the worker has not taken
		// gives way to a start of an entry, whose own task carries that same news (supervise's
		// Machine.supersedes), so the start that StartedBy is told of below reads it too.
		var task *supervise.RequestDeliver
		if payload.Task != "" || payload.News {
			kept, err := keptJSON(payload.Kept)
			if err != nil {
				return err
			}
			task = &supervise.RequestDeliver{Claim: token, Task: news.task, Phase: payload.Phase, Generation: payload.Generation,
				Entry: payload.Entry, ResendEntry: payload.ResendEntry, RoundNamed: news.roundNamed,
				CheckLook: supervise.CheckLook{Look: payload.CheckLook, OpenedAt: payload.CheckOpenedAt},
				ID:        fmt.Sprintf("%s%d", outboxDeliveryPrefix, row.ID), Row: row.ID, Kept: kept, News: news.news, NewsOnly: payload.News}
		}
		// The claim remembers the newest start run against it, so a stop written before this one
		// is finished rather than acted on however late it arrives (see "suspend" below). A claim
		// this row created has no older stop to fence, and recording it here rather than only for
		// a claim that already existed keeps one rule instead of a special case.
		if err := machine.StartedBy(ctx, row.ID, task); err != nil {
			return fmt.Errorf("record the start of claim %s: %w", token, err)
		}
		// A start whose wake carried an Envoy message marks its claim before it asks for the
		// launch, so that launch waits in the pacing's message queue (dispatch://INFRA-5682 R1).
		// The listener's report that raised the wake marked the claim in its own transaction
		// (workflow's listenerWake), and that mark stands unless the role has taken a turn since,
		// so the write here keeps it; it marks a row a daemon before this release queued. The
		// mark is the row's own creation time, which is the moment the wake that wrote it was
		// raised (the outbox's created_at), and not the moment this attempt runs: a row retried
		// or held until after the role has taken a turn would otherwise write a mark later than
		// that turn, which read the message, and the mark would stand, so the claim's next wake,
		// whatever raised it, would take the message queue. Marked at the row's time, such a
		// retry writes a mark that does not stand (the store's MarkClaimMessageTx). The
		// machine is told the mark the store holds, in whatever state the claim is: a start that
		// meets a wake already waiting asks for no launch, and that wake moves by the mark alone.
		if payload.WakeKind == pods.WakeRoleMessage || payload.WakeKind == pods.WakeAgentMessage {
			mark, err := store.MarkClaimMessage(ctx, r.pool, token, row.CreatedAt.UTC())
			if err != nil {
				return err
			}
			if !mark.IsZero() {
				if err := machine.Handle(ctx, supervise.MessageMarked{Claim: token, At: mark}); err != nil {
					return fmt.Errorf("mark claim %s for its waiting message: %w", token, err)
				}
			}
		}
		state := machine.Claim().State
		// A phase worker's start is one row that carries its task, so the claim is brought back
		// and told its work here, in that order, and nothing of the start waits on another row.
		// Whether the row waited for the issue's admission slot before the lane took it is
		// settled by then (workflow.StartOrOwe); by here it is a start like any other.
		switch state {
		case supervise.StateQueued:
			if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: token}); err != nil {
				return fmt.Errorf("start claim %s: %w", token, err)
			}
		case supervise.StateSuspended:
			// The role owns the issue's phase this start is for: the rule resumes it, unless the
			// operator or the controller suspended it meanwhile (supervise's
			// Claim.RuleHolds), when the task's delivery below waits instead. A check start resumes
			// the role the check records the close or a pending look suspended, in a Sandbox the
			// resume relaunches (runtime/sandbox's relaunch).
			reason := fmt.Sprintf("start row %d of %s (phase %q)", row.ID, issue.Key, payload.Phase)
			if payload.CheckLook > 0 {
				reason = fmt.Sprintf("start row %d of %s (production check look %d)", row.ID, issue.Key, payload.CheckLook)
			}
			if err := machine.Handle(ctx, supervise.RequestRuleResume{Claim: token, Reason: reason}); err != nil {
				return fmt.Errorf("resume claim %s: %w", token, err)
			}
		case supervise.StateFailed:
			// A boot rule-resume that spent its launch budget is terminal for this row: retrying it
			// with RequestRetry would reset that budget and launch forever. The failure's workflow
			// fact and boot report name it. Other workflow starts retain their existing retry rule.
			if payload.RuleResume {
				r.log.Info("outbox boot rule-resume finished: its claim spent the launch budget", "row", row.ID, "issue", issue.Key, "role", payload.Role)
				return nil
			}
			// A start that asks for a new session has the retry forget the recorded one, inside
			// that one decision: the operator's `legion claims retry --fresh-session`, which is
			// refused by whatever refuses this row rather than having discarded the session first.
			// So does the held phase's retry of a claim held for OpenAI's cyber-policy refusal,
			// asked or not, and every other start of such a claim is refused (startRetry,
			// supervise's freshSession).
			// A production check look's start is the half-open trial of a checking role that
			// failed: it retries the claim with one attempt left of each budget its failure spent,
			// never a fresh budget, so a role that cannot start launches once a look rather than
			// until a whole budget runs out at every look up to the check's bound
			// (supervise.RequestRetry.OneAttemptLeft). A check start never asks for a new session
			// (workflow's startLook); the resets a person or the controller makes, which give the
			// whole budget, are the API's retry and fresh session (supervise.RequestAskedRetry).
			again := startRetry(token, payload)
			again.OneAttemptLeft = payload.CheckLook > 0
			if err := machine.Handle(ctx, again); err != nil {
				return r.retryRefused(ctx, row, issue, payload, machine, err, "retry claim")
			}
		case supervise.StateRetired:
			// A retired claim was closed with its issue (an issue closed as done, or one whose
			// retention ended, which also removed its workspace), its agent reported its own exit (a phase
			// worker or a child's own architect, which supervise's exit retires), or an operator
			// stopped it. So the workspace is provisioned, which brings back one the end of a retention removed,
			// before the agent is launched again on the kept session (a new one once supervise's
			// Machine.forgetSession has dropped it). A claim the retry is held from
			// (supervise.Claim.RetryHeld) has no agent launched, so nothing is provisioned for it,
			// and the machine's refusal finishes the row.
			retry := startRetry(token, payload)
			if !machine.Claim().RetryHeld(retry) {
				if err := r.provisionWorkspace(ctx, issue); err != nil {
					return err
				}
			}
			if err := machine.Handle(ctx, retry); err != nil {
				return r.retryRefused(ctx, row, issue, payload, machine, err, "launch again the agent of retired claim")
			}
		}
		// A claim that, once brought back above, still holds a task of this row's phase and run that
		// it will work is given that task: one not yet confirmed (a claim whose process is gone keeps
		// the task it held, and a death in a turn takes the task back unconfirmed, behind the sentence
		// saying so), or one confirmed in the turn the claim is working. The row's own task would
		// follow it as a second prompt for work already under way, so it is not delivered when it is
		// the phase's resume task (promotion's) or this row retried a claim that was running nothing —
		// one that failed, as the architect's retry of the held phase relaunches it, or one an
		// operator stopped, as the operator's retry of its phase relaunches it — since the retry sends
		// the kept task itself. A confirmed task on a claim not working is over: the decision above
		// retired it here, or retires it first when a launch whose outcome was uncertain is released
		// and spawned again, after this start, so the row's task goes in its place. A check start names
		// no phase, so it is never one of these: each look's task is delivered, its row id its
		// delivery's, and a delivery with no phase is held by nothing (supervise's phaseHolds), so
		// the task reaches the role the check records whatever the issue's phase.
		held, gave := machine.Claim(), false
		if (payload.ResumeTask || supervise.FailedOrRetired(state)) && keepsStartTask(held, payload) {
			r.log.Info("outbox start of a claim that holds its phase's task; not delivered again", "row", row.ID, "issue", issue.Key,
				"role", payload.Role, "phase", payload.Phase, "held", held.Pending.ID)
			// Which entry the claim is recorded as given here is the entry of the task it is
			// holding, never the entry this row carried, and the two differ.
			//
			// A start of the entry whose work the claim holds records that entry
			// (supervise.Machine.EntryGiven): the claim was given that work, so the difference
			// between the entry the role's phases row names and the entry its claim was given
			// closes without a second task reaching the worker. Where the delivery that queued
			// that task recorded the entry already — supervise's queue writes Claim.GivenEntry in
			// the transaction that writes the delivery — EntryGiven leaves the number where it
			// stands, since it only rises.
			//
			// A start of a later entry, whose task the claim is still working, records nothing.
			// That claim was given the earlier entry's work and no other, and recording this row's
			// entry on it is the defect the entry exists to close, rebuilt here: the claim would
			// read level with its phases row, workflow.OwedEntry would answer false, nothing would
			// send the row's own task, and every completion of the task the worker actually holds
			// would be refused as behind. Recording nothing leaves no worker with nothing to do.
			// Which turn that leaves depends on the state this branch was reached in. A working
			// claim finishes the turn it is in; its completion of the earlier entry is refused
			// 409 with nothing recorded, which is right, since the phase was entered again. A
			// failed claim is in no turn at all: the state switch above relaunched it
			// (supervise.RequestRetry), and the relaunch hands it the earlier entry's task,
			// which it still holds unconfirmed (supervise's sendPending), so its first
			// completion is the one refused instead. Either way the admission pass sends the
			// row's own entry's task on the first pass after that task is retired, since it
			// passes over a claim whose pending delivery is still worked (admit's
			// resumeSlottedWorkers, workflow's SendOwedEntry).
			//
			// The gave flag below stays false either way, since what it answers is whether the
			// task was handed over (settleKept), not whether the entry was recorded.
			if payload.Entry > 0 && held.Pending.Entry == payload.Entry {
				if err := machine.EntryGiven(ctx, payload.Entry); err != nil {
					return fmt.Errorf("record entry %d as given to claim %s: %w", payload.Entry, token, err)
				}
			}
		} else if task != nil {
			if err := machine.Handle(ctx, *task); err != nil {
				return fmt.Errorf("deliver to claim %s: %w", token, err)
			}
			gave = true
		}
		// A start that finds an architect running is a return of its issue while its session
		// lived on: the issue's return to Legion's work, or, for a child's own, a start of the child's
		// mid-phase work (admit's promotion) or the child's next first phase start (a
		// rerun, or a person's move back to todo). A stop suspends every claim of its issue, and a
		// child's leave suspends or closes the child's
		// architect. After either, this happens only when the claim was mid-launch when the
		// suspend ran (the machine ignores a suspend while launching), or the start ran before
		// it. A running child that a person sets back to todo goes through no leave, so its
		// start finds the architect running unless something else stopped it. No ready follows,
		// and a ready is what tells the architect the issues it owns (workflow's claimReady),
		// so the start applies that fact itself, for the running launch, once per start row,
		// on the start's own issue: the architect is told its issues at their new generation, a
		// root's whole tree and a child's own subtree, each through the workflow's one composer
		// (workflow.QueueCatchUp). Its catch-up names what nobody claims, which this row's task
		// may name too, so the decisions the task carries are settled first (settleKept) and the
		// catch-up is composed without them; the settle after the row then finds nothing left to
		// do. A retry of the row queues the task no second time and applies the fact once.
		//
		// That catch-up stands in place of a launch, so it is a wake of the architect and takes a
		// turn in the daemon's pacing like the launch it replaces: the fact says so (Paced), the
		// engine takes the turn inside the fact's transaction and writes the row due at it
		// (workflow's claimReady), and this runner writes the line that wake writes when the row
		// runs (wakeRan). A start that launches instead took its turn at the machine's gate above
		// (supervise's reserveWake), so one start row is one wake either way. This holds for every
		// architect, a root's or a child's own. A start queued for news alone
		// (record.SuperviseRequest.News) is no re-admission of either, and tells the architect its
		// news and nothing else.
		if !payload.News && supervise.SaidReady(state) && payload.Role == claim.RoleArchitect {
			if err := r.settleKept(ctx, row, payload, gave); err != nil {
				return err
			}
			if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("start-running:%s:%d", token, row.ID),
				intake.ClaimReady{Issue: issue.Key, Role: payload.Role, Launch: machine.Claim().Generation, Paced: true}, r.handlers...); err != nil {
				return fmt.Errorf("tell the running architect of %s its issues: %w", issue.Key, err)
			}
		}
		return nil
	case "suspend", "step_stop":
		// A step stop is a suspend of the process of the step a stopped task ran (workflow's
		// stopTask): the same fence, the same source (the daemon), so the session stays, the
		// step's pending task retires with it (supervise's RetiresPhaseTask), the stranded report
		// passes over it and an idle release cannot take it over. It is an op of its own only so
		// the open-issue invariant can tell it from the stop of an issue the workflow takes out.
		if !found {
			return r.stopWithoutMachine(ctx, row, token)
		}
		held, stoppable := claimToStop(machine)
		if !stoppable {
			// The claim runs nothing, so there is nothing to suspend.
			return nil
		}
		// Whether the suspend still acts is workflow.StopActs's rule, which promotion reads too,
		// with the fence its issue stands at and the pod its claim runs in (stopActs), on the claim
		// claimToStop answered. Its LastStartRow is the newest start's even where that is the
		// published view: the start rows that write it (supervise.Machine.StartedBy) run on this
		// row's lane one row at a time, so none is in flight while this row runs. A left pod's
		// close's suspend is also held by the turn fence below.
		if acts, err := r.stopActs(ctx, row, payload, held); err != nil || !acts {
			return err
		}
		// The suspend of a left pod's close is fenced on the turn start its workspace check read
		// (record.SuperviseRequest.TurnStart): a turn of the claim that started after it may have
		// made work the check's read did not see, so the stop finishes without acting, as a
		// release's role stop is cancelled by a turn after its mark. The claim stays live in its
		// pod, the close's Sandbox suspension carries the same fence and finishes without acting
		// (store.IssueSuspension), so nothing of the close outlives the hold, and the next idle
		// pass decides the close again with a read of its own (workflow.LeftPodClose).
		//
		// The two compare on one precision. The fence is claims.last_turn_start, which the
		// database keeps to the microsecond, and the machine's stamp of the same turn carries the
		// nanoseconds of the clock that made it (supervise.Claim.LastTurnStart), so the stamp is
		// read to the microsecond too: the turn the check saw then reads equal to the fence, not
		// after it, and only a later turn holds the stop.
		if fence, turned := payload.TurnStart, machine.Claim().LastTurnStart.Truncate(time.Microsecond); fence != nil && turned.After(*fence) {
			r.log.Info("outbox suspend of a left pod's close held: the claim started a turn after the close's check; finished without acting",
				"row", row.ID, "issue", issue.Key, "role", payload.Role, "turn-start", turned, "check-turn-start", *fence)
			return nil
		}
		if err := machine.Handle(ctx, supervise.RequestSuspend{Claim: token, Reason: payload.Reason, By: supervise.SuspendedByDaemon}); err != nil {
			return fmt.Errorf("suspend claim %s: %w", token, err)
		}
		// A stop and the stop of a stopped task's step suspend a claim through this one body, as a
		// close retires one through issue_close, and each takes a released claim over: the machine
		// re-stamps the suspension's source and this drops the backstop the release left that role,
		// so no later backstop acts on a claim this suspension now owns. Another role's backstop
		// stays (decision 17 of dispatch://INFRA-3759): a worker waiting on a deploy has nothing
		// else to bring it back. The decline of an idle release stays: the issue comes back.
		r.takeOverRelease(ctx, row.Issue, payload.Role)
		return nil
	case "release":
		// One role stop of an idle pod's release. Its fence is the release's own mark and nothing
		// else (store.IssueReleaseStop), where a suspend reads workflow.StopActs: a start that
		// overtook this stop, and at the boundary a delivery the pod must answer, cancel the
		// whole release rather than skipping one role of it, and a stop of a release that is
		// over does nothing at all.
		//
		// That fence is read before anything about the claim, because the boundary's read is the
		// release's and not the claim's: the first role stop of a release is the one row that
		// answers a delivery queued around the mark, and a boundary whose own claim has failed,
		// retired or lost its machine would otherwise return with that delivery unanswered,
		// leaving the release to run on and the pod to be taken for a wake to bring back.
		decision, err := r.trees.IssueReleaseStop(ctx, r.project, row.Issue, row.ID)
		if err != nil {
			return err
		}
		switch decision.Act {
		case store.ReleaseOver:
			r.log.Info("outbox release finished without acting: the pod's release is over",
				"row", row.ID, "issue", issue.Key, "role", payload.Role)
			return nil
		case store.ReleaseCancel:
			hold, why := releaseHold(decision.Cause)
			return r.cancelRelease(ctx, row.Issue, payload.Tree, hold, why)
		}
		// A role stop of an earlier run or phase entry than the issue stands at now is a release
		// the pod was given work after (R20 of dispatch://INFRA-3320): the release is cancelled as
		// any release a start overtook is, rather than this one role being skipped.
		if late, err := r.outlived(ctx, row, payload); err != nil {
			return err
		} else if late {
			return r.cancelRelease(ctx, row.Issue, payload.Tree, workflow.IdleHoldOvertaken, "the issue was handed a later phase entry or run after its release was marked")
		}
		if !found {
			return r.stopWithoutMachine(ctx, row, token)
		}
		if _, stoppable := claimToStop(machine); !stoppable {
			// The claim runs nothing, so there is nothing to release.
			return nil
		}
		if err := machine.Handle(ctx, supervise.RequestSuspend{Claim: token, Reason: payload.Reason, By: supervise.SuspendedByReleased}); err != nil {
			return fmt.Errorf("release claim %s: %w", token, err)
		}
		// The claim is suspended as `released`, so the release's mark records it: it is that set
		// the cancel resumes, the teardown records as the release's roles, and a later row reads
		// to tell a claim this release suspended that something resumed since from one still in
		// the pod (store.ReleaseSuspendedClaim). The row returns its error rather than swallowing
		// it, so a retry suspends and records together.
		return r.trees.ReleaseSuspendedClaim(ctx, r.project, row.Issue, payload.Role, row.ID)
	case "issue_close":
		// An issue's close as done: the claim is retired with its session kept, since the issue
		// is finished and its next run, should a person set it todo again, resumes that session
		// on a pod and a disk of its own (a new one once supervise's Machine.forgetSession has
		// dropped it). It is fenced as a suspend is: a newer start means a person set the issue
		// todo again, whose run the close must not end.
		if !found {
			return r.stopWithoutMachine(ctx, row, token)
		}
		if acts, err := r.stopActs(ctx, row, payload, machine.View().Claim); err != nil || !acts {
			return err
		}
		if err := machine.Handle(ctx, supervise.RequestIssueClose{Claim: token}); err != nil {
			return fmt.Errorf("close the issue of claim %s: %w", token, err)
		}
		// The issue is done, so no idle release of its pod is coming: the decline of one goes
		// with the backstop.
		r.forgetIdleRelease(ctx, row.Issue)
		return nil
	default:
		return fmt.Errorf("outbox row %d has unknown supervise operation %q", row.ID, payload.Op)
	}
}

// stopActs is whether payload, a suspend or an issue close the outbox runs as row, still acts on
// held, the claim it stops: workflow.StopActs's rule over the row ids, weighed with the fence its
// issue stands at now (a stop of an earlier run or phase entry stops nothing of a later one), and
// the pod held runs in, which must be the pod the stop was queued for where both name one
// (record.OutlivesPod). It logs why a stop does not act.
func (r *outbox) stopActs(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest, held supervise.Claim) (bool, error) {
	fence, err := r.fenceOf(ctx, row.Issue, false)
	if err != nil {
		return false, err
	}
	if !workflow.StopActs(row.ID, payload, held.LastStartRow, &fence) {
		r.log.Info("outbox stop superseded by a newer start, or of an earlier run or phase entry; finished without acting",
			"row", row.ID, "op", payload.Op, "issue", row.Issue, "role", payload.Role, "start-row", held.LastStartRow,
			"generation", payload.Generation, "entry", payload.Entry, "current_generation", fence.Generation, "current_entry", fence.Entry)
		return false, nil
	}
	if pod := podOf(held.Locator); record.OutlivesPod(payload.PodUID, pod) {
		r.log.Info("outbox stop of a pod its claim no longer runs in; finished without acting",
			"row", row.ID, "op", payload.Op, "issue", row.Issue, "role", payload.Role, "pod", payload.PodUID, "current_pod", pod)
		return false, nil
	}
	return true, nil
}

// claimToStop is machine's claim as a row that stops it reads it before the stop, and whether the
// claim has anything to stop: a claim its last decision left failed or retired
// (supervise.FailedOrRetired) has let its process go and has no launch under way. It reads the
// published view (supervise.Machine.View), which never waits, and where the view reads the claim
// failed or retired it reads the claim once more through supervise.Machine.Claim before it gives
// up. Giving up decides whether to stop the claim at all, which is "a check a decision must not
// race" (supervise.Machine.Claim): the view reads a failed claim failed until a retry's launch is
// first written (supervise's writeTry), so a row that gave up on the view while a retry's decision
// was in flight would leave that retry to launch the claim the row was to stop. The second read
// waits out the whole decision, the retry's start in the runtime included, which under a Sandbox
// can take minutes; a claim the view reads live is never read that way. The caller's stop decides
// on the claim answered.
func claimToStop(machine *supervise.Machine) (supervise.Claim, bool) {
	held := machine.View().Claim
	if supervise.FailedOrRetired(held.State) {
		held = machine.Claim()
	}
	return held, !supervise.FailedOrRetired(held.State)
}

// keepsStartTask says whether held still holds a task of this start's phase and run that it will
// work: one not yet confirmed — a failed claim keeps the task it held, and a death in a turn takes
// the task back unconfirmed — or one confirmed in the turn the claim is working
// (supervise.Delivery.StillWorked). A confirmed task on a claim not working is over: the claim's
// next decision retires it. A start naming no phase holds none, which is what a production check's
// look is: each look's task is delivered whatever the claim holds. Nor is a task of news alone
// (supervise.Delivery.NewsOnly) the phase's task: a retry of a failed claim that holds only that is
// given its own task, which takes the news task's place where the retry carries an entry, and so
// the news with it (supervise's Machine.supersedes), and otherwise waits behind the news task until
// a turn has taken it.
func keepsStartTask(held supervise.Claim, payload record.SuperviseRequest) bool {
	pending := held.Pending
	return pending != nil && payload.Phase != "" && pending.StillWorked(payload.Generation, payload.Phase, held.State)
}

// stopWithoutMachine is a stop row — a suspend or an issue close — that finds no machine for its
// claim. Creation persists a claim before publishing its machine in memory, and the stop must not
// be consumed in that interval: while the stored claim may still hold a process, the row waits for
// the machine the later publish brings; a claim that runs nothing, or none at all, leaves it
// nothing to stop.
func (r *outbox) stopWithoutMachine(ctx context.Context, row record.OutboxRow, token claim.Token) error {
	pending, err := r.trees.StoredClaimMayRun(ctx, token, row.ID)
	if err != nil {
		return err
	}
	if pending {
		return wait.Errorf("stop claim %s: waiting for its stored claim's supervising machine", token)
	}
	return nil
}

// suspendIssue is the issue_suspend executor, the durable effect of a close, a stop or a
// production check's look: the runtime suspends the issue's Sandbox under its pod lock once the
// store authorizes the close (store.IssueSuspension), waits its pod out and deletes it
// (runtime.IssueSuspender). A pod the runtime deleted, or found gone, once the store authorized
// it, ends the pause's hold of that pod (podDeleted); a pod whose close neither its fence nor the
// store refused ends (endPods). A runtime that runs no pod of an issue (tmux) has none to delete,
// and the close is the end of the issue's run there, so a close its fence does not refuse ends
// what a pod's end ends (endPods): each issue's own close is where its run ends, since no close
// ends a tree's. A close of an issue Dispatch shows done then cleans up the issue's own lifecycle
// (cleanupClosedIssue), under either runtime whatever its fence says, since a reservation an
// earlier try of the row made is the row's to finish.
//
// The row is fenced by its issue's own run, phase entry and pod (record.IssueSuspend), read under
// that same lock (podStands), and the runtime holds it to the pod it was queued for as well
// (runtime.IssueSuspender): a row of an earlier run or entry deletes no pod a return made, and one
// queued for a pod the issue's claims no longer run in deletes nothing (R17 of
// dispatch://INFRA-3320: a cleanup deletes only its own issue's pod). The tree the row carries is
// the release before's, and read here by nothing.
func (r *outbox) suspendIssue(ctx context.Context, row record.OutboxRow, value record.IssueSuspend) error {
	if r.supervisor == nil {
		return errors.New("issue suspension executor has no claim supervisor")
	}
	suspender, ok := r.supervisor.deps.Runtime.(runtime.IssueSuspender)
	if !ok {
		stands, err := r.podStands(ctx, row, value)
		if err != nil {
			return err
		}
		if stands {
			r.endPods(ctx, row.Issue)
		}
		return r.cleanupClosedIssue(ctx, row, value)
	}
	closing := store.IssueClose{Issue: row.Issue, Generation: value.Generation, Row: row.ID, TurnStart: value.TurnStart}
	// ended and authorized are the store's answer under the runtime's pod lock, read by two
	// rules. A close the fence (podStands) or the store refused finishes without acting and may
	// leave the pod standing, so it turns both false. The pause's hold of the pod is the store's
	// own record, so it ends only on a close the store authorized: authorized starts false. The end
	// of the pod also follows a runtime that stops the pod without asking the store, its nil return
	// being the stop: ended starts true.
	ended, authorized := true, false
	if err := suspender.SuspendIssue(ctx, row.Issue, value.PodUID, func(ctx context.Context) (bool, error) {
		if stands, err := r.podStands(ctx, row, value); err != nil || !stands {
			ended = false
			return false, err
		}
		// A close that a launcher's clean read let through takes the pod that read found clean or
		// nothing (record.IssueSuspend.ReadPodUID), read under the pod lock: a pod that took its
		// place is one no read showed clean, so the suspension finishes without acting, and the
		// next pass decides that pod.
		if value.ReadPodUID != "" && !r.holdsPod(row.Issue, value.ReadPodUID) {
			r.log.Info("outbox: the pod the close's launcher read is no longer the issue's pod; the suspension takes nothing",
				"issue", row.Issue, "readPod", value.ReadPodUID)
			ended = false
			return false, nil
		}
		act, err := r.trees.IssueSuspension(ctx, r.project, closing, record.OutOfWorkflow)
		ended = act
		authorized = act && err == nil
		return act, err
	}); err != nil {
		return err
	}
	if authorized {
		if err := r.podDeleted(ctx, row.Issue, "the issue's close, stop or production check deleted its pod"); err != nil {
			return err
		}
	}
	if ended {
		r.endPods(ctx, row.Issue)
	}
	return r.cleanupClosedIssue(ctx, row, value)
}

// cleanupClosedIssue ends the issue's own lifecycle once its close is done: an issue Dispatch shows
// done reserves its cleanup (store.Store.ReserveIssueCleanup) and, once every claim of it has
// retired — its close's issue_close rows run ahead of this one on the issue's lane — has the
// runtime delete what it still holds of the issue by the issue's own identity
// (runtime.Runtime.CleanupIssue) and confirms the cleanup (store.Store.CleanupReservedIssue), so
// the orphan sweep reads the issue's resources as nobody's and a later run opens a lifecycle of
// its own. A claim that has not retired yet makes it a wait, and the row runs again on its
// backoff. An issue a stop took out of work keeps its lifecycle: its claims stay suspended and
// come back with it (R20 of dispatch://INFRA-3320), so a fresh reservation is made for a closed
// issue alone, at the run the row is of, and only while no production check of it is open, whose
// look still starts a role. A reservation an earlier try of the row made is finished whatever the
// issue has done since, which is the store's own rule. Nothing of another issue is reserved or
// deleted. The confirmation is applied as a fact (cleanedUp), which an admission pass waiting for
// that cleanup reads at once.
func (r *outbox) cleanupClosedIssue(ctx context.Context, row record.OutboxRow, value record.IssueSuspend) error {
	if r.pool == nil {
		return nil
	}
	check, err := r.productionCheck(ctx, row.Issue)
	if err != nil {
		return err
	}
	// The issue keeps what a cleanup would release while a look of its open production check can
	// still start a role, or while it is not done: an issue a stop took out of work keeps its
	// lifecycle (above).
	open := check != nil && check.Open()
	lifecycle, reserved, err := r.trees.ReserveIssueCleanup(ctx, r.project, row.Issue, value.Generation, func(_ phase.Phase, status string) bool {
		return open || status != record.DoneStatus
	})
	if err != nil {
		return fmt.Errorf("reserve the cleanup of closed issue %s: %w", row.Issue, err)
	}
	if !reserved {
		return nil
	}
	if err := r.trees.CleanupReservedIssue(ctx, r.project, row.Issue, lifecycle.Epoch, r.supervisor.deps.Runtime); err != nil {
		return err
	}
	r.cleanedUp(ctx, row.Issue, lifecycle.Epoch)
	return nil
}

// cleanedUp applies the confirmation of issue's cleanup at epoch (intake.IssueCleanedUp), once
// per epoch, so an admission pass that waited for that cleanup runs now rather than at the next
// fact. The cleanup is confirmed whatever the fact does, so a failure is logged and is not the
// row's: the next fact or tick looks again.
func (r *outbox) cleanedUp(ctx context.Context, issue string, epoch uint64) {
	fact := intake.IssueCleanedUp{Issue: issue, Epoch: epoch}
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("issue-cleaned-up:%s:%d", issue, epoch), fact, r.handlers...); err != nil {
		r.log.Warn("outbox: apply the confirmation of an issue's cleanup failed", "issue", issue, "epoch", epoch, "error", err)
	}
}

// endPods runs the end of issues' pods (podEnded), where attach set it.
func (r *outbox) endPods(ctx context.Context, issues ...string) {
	if r.podEnded != nil {
		r.podEnded(ctx, issues)
	}
}

// podStands says whether payload, row's effect on its issue's pod (a suspension, a workspace check
// or a release), still acts on the pod its issue stands at now: it is of the run and the phase
// entry the issue stands at (record.Fence.Outlives), and the pod it was queued for is the one the
// issue's Sandbox holds now, where both name one (record.OutlivesPod). That pod is the runtime's
// own answer where it gives one (runtime.IssuePodReader), the pod a deletion would take, and
// otherwise the one the issue's claims record (record.Store.IssuePodUID). An outbox with no store
// reads no fence, as outlived says. It logs why it does not act.
func (r *outbox) podStands(ctx context.Context, row record.OutboxRow, payload record.OutboxPayload) (bool, error) {
	if r.pool == nil {
		return true, nil
	}
	queued, _ := record.FenceOf(payload)
	now, err := r.fenceOf(ctx, row.Issue, true)
	if err != nil {
		return false, err
	}
	if r.supervisor != nil {
		if reader, ok := r.supervisor.deps.Runtime.(runtime.IssuePodReader); ok {
			if pod, read := reader.IssuePod(row.Issue); read {
				now.PodUID = pod
			}
		}
	}
	switch {
	case now.Outlives(queued):
		r.log.Info("outbox pod effect of an earlier run or phase entry of its issue; acts on no pod", "row", row.ID,
			"kind", row.Kind, "issue", row.Issue, "generation", queued.Generation, "entry", queued.Entry,
			"current_generation", now.Generation, "current_entry", now.Entry)
		return false, nil
	case record.OutlivesPod(queued.PodUID, now.PodUID):
		r.log.Info("outbox pod effect of a pod its issue no longer runs; acts on no pod", "row", row.ID,
			"kind", row.Kind, "issue", row.Issue, "pod", queued.PodUID, "current_pod", now.PodUID)
		return false, nil
	}
	return true, nil
}

// podDeleted ends the hold a project's pause has on issue's pod once a row has deleted that pod
// (decision 13 of dispatch://INFRA-3759): the issue's suspension (suspendIssue) and a release's
// teardown (releaseIssue). The tree's cleanup ends the holds of its pods in its own transaction
// (store.CleanupReservedIssue). The end takes the issue's lock (store.EndPausePodHoldTx), as every
// writer of a pod hold does. A failed end fails the row, whose retry deletes nothing more, since
// the pod is gone, and ends the hold.
func (r *outbox) podDeleted(ctx context.Context, issue, why string) error {
	ended, err := r.trees.EndPausePodHold(ctx, r.dispatchProject, issue, store.PausePodHoldDeleted, r.now().UTC())
	if err != nil {
		return err
	}
	if ended {
		r.log.Info("pause: a pod's hold ended; the pod was deleted", "issue", issue, "reason", why)
	}
	return nil
}

// releaseIssue is the last act of an idle pod's release, run once the workspace check has found
// nothing in the workspace that origin does not hold (checkIssueWorkspace) and every role stop of
// the release has suspended its claim: it asks the runtime's release (runtime.IssueReleaser),
// which suspends the issue's Sandbox and waits its pod out. The issue stays in the workflow, so the
// runtime keeps a Sandbox that owns the issue's kept volume, Suspended, with that volume, for the
// issue's next pod, and deletes the Secrets that the next launch writes again. It deletes every
// other Sandbox with the role Secrets it owns, and nothing of such a pod's disk survives that.
// That is why the check runs first and the teardown waits for it (store.IssueRelease).
//
// It is the last act of the other release rule too: the release of a pod no role can use
// (workflow.UnownedPodRelease), whose every claim is already failed, retired or suspended. No role
// stop runs before it there, since no role is running; where the release deletes the disk, the
// check before it is the pod's launcher's read (checkIssueWorkspace, record.IssueWorkspaceCheck's
// Launcher), and this teardown runs only once that read found nothing that origin does not hold.
// Its teardown is told from an idle one by the kind its mark carries
// (store.ReleaseDecision.SuspendsNoRole). Such a release
// records no roles, arms no backstop, and logs a line of its own
// naming the Sandbox it released and the state every claim of the issue stood in; each of those
// is explained where it is done.
//
// A release the store no longer authorizes — one whose issue has left the workflow since, or one
// whose pod has been given work again — is cancelled: the pod stays and the claims the release
// suspended are resumed. The authorization the
// teardown acts on is the one the runtime took under its pod lock, not the one this row read
// before it: a notice or a hook prompt runs on a lane of its own, and the wake it makes
// can resume a role between the two reads. So the inner answer is what is carried out — a
// cancel there cancels, and a release that is over by then does nothing — and a pod that still
// stands is never recorded as released, nor given a backstop, nor stripped of its backstops.
//
// A teardown of a release that is over acts on nothing and undoes nothing: a role stop that
// cancelled this release has already resumed what it suspended, and a release that finished
// left the pod gone.
//
// A teardown of the release of a pod no role can use that took nothing records no pod release and
// logs no release line, which would count one release twice, and logs instead that it found no
// Sandbox to release; it still ends the pause's hold on the pod, runs the end of the pod and
// clears the mark, which are owed whoever took the pod. Whether it took the pod is the runtime's
// own answer (runtime.IssueReleaser), not a read made before it, together with the mark's note of
// an earlier attempt that came to the runtime's first write: store.ReleaseTeardown states the rule,
// the protocol and the window it leaves. The idle release's teardown records its release and logs
// its line whatever the runtime answered.
func (r *outbox) releaseIssue(ctx context.Context, row record.OutboxRow, payload record.IssueRelease) error {
	if r.supervisor == nil {
		return errors.New("issue release executor has no claim supervisor")
	}
	releaser, ok := r.supervisor.deps.Runtime.(runtime.IssueReleaser)
	if !ok {
		// A runtime with no shared issue resources has no pod to release.
		return nil
	}
	releasing := store.IssueReleased{Issue: row.Issue, Generation: payload.Generation, Row: row.ID}
	first, err := r.trees.IssueRelease(ctx, r.project, releasing, record.OutOfWorkflow)
	if err != nil {
		return err
	}
	switch first.Act {
	case store.ReleaseOver:
		r.log.Info("outbox release teardown finished without acting: the pod's release is over",
			"row", row.ID, "issue", row.Issue)
		return nil
	case store.ReleaseCancel:
		hold, why := releaseHold(first.Cause)
		return r.cancelRelease(ctx, row.Issue, payload.Tree, hold, why)
	}
	if stands, err := r.podStands(ctx, row, payload); err != nil {
		return err
	} else if !stands {
		return r.cancelRelease(ctx, row.Issue, payload.Tree, workflow.IdleHoldOvertaken, "the issue was handed a later phase entry, run or pod after its release was marked")
	}
	// The runtime reads the store again under its pod lock, which is what orders the teardown
	// against a physical start, and that read is the one the pod is taken on: it is kept here
	// and acted on below. A runtime that takes the pod without asking leaves this read's answer
	// standing, which is the same release. The read under the lock holds the row to its issue's
	// fence too (podStands), whose failure is a cancel as a start that overtook the release is.
	decision := first
	authorize := func(ctx context.Context) (bool, error) {
		if stands, err := r.podStands(ctx, row, payload); err != nil || !stands {
			decision = store.ReleaseDecision{Act: store.ReleaseCancel, Cause: store.ReleaseCauseOvertaken}
			return false, err
		}
		inner, err := r.trees.IssueRelease(ctx, r.project, releasing, record.OutOfWorkflow)
		if err != nil {
			return false, err
		}
		// A release whose launcher read the workspace first takes the pod that read found clean
		// or nothing (record.IssueRelease.ReadPodUID): read here, under the pod lock, a pod that
		// took its place, or no pod at all, is one no read showed clean, and the release is
		// cancelled as one overtaken.
		if inner.Act == store.ReleaseActs && payload.ReadPodUID != "" && !r.holdsPod(row.Issue, payload.ReadPodUID) {
			r.log.Info("outbox: the pod the release's launcher read is no longer the issue's pod; the teardown takes nothing and the release is cancelled",
				"issue", row.Issue, "readPod", payload.ReadPodUID)
			inner.Act, inner.Cause = store.ReleaseCancel, store.ReleaseCauseOvertaken
		}
		decision = inner
		return inner.Act == store.ReleaseActs, nil
	}
	// A release that suspends no role reads the two facts its own line carries — the Sandbox the
	// teardown is about to release, and the state of every claim of the issue — and reads both
	// before the teardown, because the pod and its claims are exactly what the teardown changes.
	var sandboxName string
	var claims store.ClaimStates
	if decision.SuspendsNoRole {
		sandboxName = r.issueSandboxName(row.Issue)
		if claims, err = r.trees.ClaimStatesIn(ctx, r.project, row.Issue); err != nil {
			return err
		}
	}
	// recordsAnyway is whether this teardown records its release whatever the runtime answers: the
	// idle release always does, and the release of a pod no role can use does where its mark says
	// an earlier attempt came to the runtime's first write (store.ReleaseTeardown). Where it does
	// not, the runtime's callback notes that on the mark, just before the runtime's first write,
	// and once; where it does, there is nothing to note and the runtime is given no callback.
	recordsAnyway := !decision.SuspendsNoRole || first.Teardown == store.TeardownReached
	var writing func(context.Context) error
	if !recordsAnyway {
		writing = func(ctx context.Context) error {
			return r.trees.NoteReleaseTeardown(ctx, r.project, row.Issue, decision.Release, store.TeardownReached)
		}
	}
	released, err := releaser.ReleaseIssue(ctx, row.Issue, payload.PodUID, authorize, writing)
	if err != nil {
		return err
	}
	switch decision.Act {
	case store.ReleaseOver:
		r.log.Info("outbox release teardown finished without acting: the pod's release was over when the runtime asked",
			"row", row.ID, "issue", row.Issue)
		return nil
	case store.ReleaseCancel:
		hold, why := releaseHold(decision.Cause)
		return r.cancelRelease(ctx, row.Issue, payload.Tree, hold, why)
	}
	// records is whether this teardown records its release: whatever the runtime answered, or
	// because the runtime took the pod (store.ReleaseTeardown).
	records := recordsAnyway || released
	// The pod is gone, whichever act took it: the hold a project's pause had on it holds nothing
	// now (decision 13 of dispatch://INFRA-3759).
	gone := "a release's teardown deleted the pod"
	if !records {
		gone = "a release's teardown found the pod already gone"
	}
	if err := r.podDeleted(ctx, row.Issue, gone); err != nil {
		return err
	}
	// The end of its pod (podEnded) runs here, on the issue's lifecycle lane and before the
	// release's mark is cleared below, so a start queued behind the release waits for it.
	r.endPods(ctx, row.Issue)
	if decision.SuspendsNoRole {
		// No role was suspended, so the record carries none. No
		// backstop is armed either, which is the one act of the idle release this one must not
		// make: armBackstops cancels the issue's open backstops before arming at most one of
		// its own, and this release owes no wake. It
		// suspended no claim, and a claim an earlier idle release left `released` must keep the
		// backstop that release armed for it — the pod this teardown took down is not the pod
		// that claim is waiting to come back to.
		// The release line follows the record, and a retry whose record was already written logs
		// it no second time (recordPodRelease).
		if records {
			if r.recordPodRelease(ctx, row.Issue, decision.Release, payload.Generation, nil) {
				r.log.Info("outbox: released the pod of an issue no role can use; the release suspended no role, so no session is waiting for a wake",
					"issue", row.Issue, "tree", payload.Tree, "generation", payload.Generation,
					"sandbox", sandboxName, "claims", claims)
			}
		} else {
			r.log.Info("outbox: the release found no Sandbox of the issue to release and released nothing",
				"issue", row.Issue, "tree", payload.Tree, "generation", payload.Generation, "claims", claims)
		}
		return r.trees.ClearIssueRelease(ctx, r.project, row.Issue)
	}
	// The check before the role stops read the workspace and found no commit on a working copy or
	// a local bookmark that origin does not hold and no working-copy change, or this teardown would
	// not run (store.IssueRelease waits for the check, and a check that found work cancelled the
	// release): the line says so, which is what tells a reader of the log what every released pod
	// held. It follows the record, as the other release's line does.
	if r.recordPodRelease(ctx, row.Issue, decision.Release, payload.Generation, decision.Suspended) {
		r.log.Info("outbox: released the idle pod of an issue, whose workspace held no unpushed commit on a working copy or a bookmark and no working-copy change; its roles resume in the same sessions at the next wake",
			"issue", row.Issue, "tree", payload.Tree, "generation", payload.Generation)
	}
	// The pod is gone, so the backstop is armed now and not before: a release that stopped short
	// left the pod standing and needs none. Arming cancels the previous release's backstops too,
	// since that pod is gone and its backstop has nothing left to bring back.
	issue, err := r.issue(ctx, row.Issue)
	if err != nil {
		return err
	}
	if err := r.armBackstops(ctx, issue); err != nil {
		return err
	}
	return r.trees.ClearIssueRelease(ctx, r.project, row.Issue)
}

// issueSandboxName is the name the runtime holds issue's Sandbox under, and "" where there is
// none to be had: a runtime that runs no issue pods, which is the same type assertion the boot
// makes before it attaches the idle pass (issuePods, workflowRuntime.attachIdleRelease), or one
// that no longer holds a Sandbox for this issue. It is read for the line the release of a pod
// no role can use logs, and a name that cannot be read leaves that line's Sandbox empty rather
// than failing the row: the pod is released either way, and a teardown failed for want of a
// name is one its lane would run again.
func (r *outbox) issueSandboxName(issue string) string {
	podRuntime, ok := r.supervisor.deps.Runtime.(issuePods)
	if !ok {
		return ""
	}
	name, _ := podRuntime.IssueSandbox(issue)
	return name
}

// cancelRelease undoes a release that did not finish: the resume of each claim it suspended and
// still holds is queued with the release's mark deleted, in one transaction
// (store.CancelIssueRelease). The resume is the start row a wake
// writes, which the outbox resumes a suspended claim by, so a cancelled release leaves the pod
// exactly as a wake would have left it — and leaves it durably: a resume the daemon cannot run
// now is a row its lane runs later, where a resume held in memory alone was lost with the
// daemon that was running it, and the mark it had already deleted said nothing was owed.
//
// A cancel whose issue has left the workflow queues no resume and
// deletes the mark all the same (store.CancelIssueRelease): that pod is a close's to stop, and a
// resume queued above the close's own rows would supersede them and leave the roles of a done
// issue running in a Sandbox nothing deletes.
//
// A cancel while the project is paused queues no resume: the store records each role the release
// had stopped as stopped by the pause, holds the pod, and arms the open phase's worker's backstop
// as the stop transaction arms one (decision 12 of dispatch://INFRA-3759). The backstop is the
// idle release's own (outbox.backstop), and the open phase's worker is the workflow's rule
// (workflow.OpenPhaseRole).
//
// The hold the cancel records (store.RecordPodHoldTx) commits in the cancel's own transaction,
// which CancelIssueRelease joins under its lifecycle serializer, so it is ordered with every other
// hold record: a record written after the commit, late, would end the streak a newer pass opened.
// A hold that cannot be recorded fails the cancel, and the row's lane runs it again. The line is
// the commit's hook, so a cancel whose reply is lost and that the store says committed still says
// so.
func (r *outbox) cancelRelease(ctx context.Context, issue, tree string, hold workflow.IdleHold, why string) error {
	return store.Transact(ctx, r.trees.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		cancelled, err := r.trees.CancelIssueRelease(ctx, r.project, issue, r.releaseResume(issue, tree), record.OutOfWorkflow,
			store.ReleaseCancelPause{At: r.now(), Backstop: r.backstop, OpenPhaseRole: workflow.OpenPhaseRole})
		if err != nil {
			return err
		}
		// The teardown reads a hold and not the view behind it, so its holds carry no detail.
		if err := store.RecordPodHoldTx(ctx, tx, store.PodHold{Project: r.project, Issue: issue,
			Reason: hold.String(), At: r.now().UTC()}); err != nil {
			return err
		}
		store.OnCommit(ctx, func() {
			// A release that suspended no role resumes nothing, so the empty set of resumed roles
			// is the whole story rather than a release whose claims another path took over, and
			// the line must not say that the issue's roles go on in the pod, since no role of this
			// issue is running anywhere.
			switch {
			case cancelled.SuspendsNoRole:
				r.log.Info("outbox: the release of an issue's pod that no role can use did not finish; the pod stands with every claim of it as it was",
					"issue", issue, "hold", hold.String(), "reason", why)
			case cancelled.Pause != 0:
				r.log.Info("outbox: the idle release of an issue's pod did not finish while its project is paused; its roles stay stopped by the pause in the same pod",
					"issue", issue, "hold", hold.String(), "reason", why, "pause", cancelled.Pause, "stopped", cancelled.Stopped)
			default:
				r.log.Info("outbox: the idle release of an issue's pod did not finish; its roles go on in the same pod",
					"issue", issue, "hold", hold.String(), "reason", why, "resumed", cancelled.Resumed)
			}
		})
		return nil
	})
}

// releaseResume is the outbox row that brings one claim a cancelled release suspended back: the
// `start` row a wake writes (workflow's wakeRow), at the generation the cancel read of the
// issue, naming no phase and carrying no task. It carries no task because nothing happened to
// the role: its process was stopped and is started again in the pod it was always in, where a
// wake's task tells a role its pod went and came back.
func (r *outbox) releaseResume(issue, tree string) store.ReleaseResumeRow {
	return func(role claim.Role, generation uint64) (store.IdleReleaseRow, error) {
		row, err := record.NewOutboxRow(issue, record.SuperviseRequest{Op: "start", Tree: tree,
			Role: role, Generation: generation}, r.now())
		if err != nil {
			return store.IdleReleaseRow{}, fmt.Errorf("encode the resume of %s's %s: %w", issue, role, err)
		}
		return store.IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload}, nil
	}
}

// podsProvision is whether the runtime provisions each claim's workspace in the claim's own pod
// (C4): then the daemon provisions and removes none on its host.
func (r *outbox) podsProvision() bool { return r.supervisor.deps.Runtime.ProvisionsWorkspaces() }

// provisionWorkspace provisions issue's workspace in the repository its record names, at the
// default branch the record names, as the implement App for that repository's owner.
func (r *outbox) provisionWorkspace(ctx context.Context, issue record.Issue) error {
	if r.podsProvision() {
		return nil
	}
	repo, err := issue.Repository()
	if err != nil {
		return fmt.Errorf("provision workspace for %s: %w", issue.Key, err)
	}
	return r.provisionIn(ctx, issue.Key, repo, issue.DefaultBranch, false)
}

// ensureWorkspace provisions key's workspace when a launch finds it missing or not checked out on
// the daemon's host (the launch's spec, specs.ensureWorkspace): a claim resumed, relaunched or
// recovered rather than created, whose workspace this host never had — its issue ran in another
// runtime's pods before — or no longer has, or whose checkout failed or was cut short (a provision
// registers a workspace with no files and checks it out after, workspace.Provision), where the pane
// would start in an empty working copy. A whole workspace is left as it is. Provisioning checks out
// the issue's branch legion/<KEY> when GitHub has it, and starts at defaultBranch otherwise; on a
// workspace already there it finishes the checkout (workspace.Provision). The workspace is read
// under the issue's turn (provisionIn), so a launch never reads one the issue's provision is still
// making.
func (r *outbox) ensureWorkspace(ctx context.Context, key string, repo ghrepo.Repository, defaultBranch string) error {
	if r.podsProvision() {
		return nil
	}
	return r.provisionIn(ctx, key, repo, defaultBranch, true)
}

// unprovisioned is why key's workspace, working, needs a provision before a launch works in it, or
// "" when it is whole: its directory is absent from the daemon's host ("missing"), or its checkout
// never finished ("not checked out", checkedOut).
func (r *outbox) unprovisioned(ctx context.Context, key string, working workspace.Workspace) (string, error) {
	if _, err := os.Stat(working.Dir); errors.Is(err, os.ErrNotExist) {
		return "missing", nil
	} else if err != nil {
		return "", fmt.Errorf("read the workspace of %s: %w", key, err)
	}
	whole, err := r.checkedOut(ctx, working)
	if err != nil {
		return "", fmt.Errorf("read whether the workspace of %s finished its checkout: %w", key, err)
	}
	if whole {
		return "", nil
	}
	return "not checked out", nil
}

// provisionIn provisions key's workspace in repo, at defaultBranch for an issue with no branch, as
// the implement App for repo's owner; with onlyMissing, only when the workspace is still missing or
// not checked out once this provision's turn comes (unprovisioned), so two launches that both need
// it provision it once. Provisions of one issue take turns; once it has its turn, a provision runs
// beside other issues' provisions, handed its repository's store lock (provisionLocks.stores) and
// the wait for one of the provisionConcurrency checkout places (provisionLocks.slot), which
// workspace.Provision takes for the checkout alone, once it has let the store lock go. The order is
// issue turn, store lock, checkout place, and nothing that holds a place waits on a store lock, so
// the three never wait in a cycle, and a provision waiting on its repository's store lock holds no
// place another repository's checkout needs.
func (r *outbox) provisionIn(ctx context.Context, key string, repo ghrepo.Repository, defaultBranch string, onlyMissing bool) error {
	located, err := workspace.Location(r.stateDir, repo, key)
	if err != nil {
		return fmt.Errorf("provision workspace for %s: %w", key, err)
	}
	issue, releaseIssue := r.provisions.issues.take(located.Dir)
	defer releaseIssue()
	issue.Lock()
	defer issue.Unlock()
	if onlyMissing {
		found, err := r.unprovisioned(ctx, key, located)
		if err != nil || found == "" {
			return err
		}
		r.log.Warn("provisioning the workspace a claim's launch found missing or not checked out", "issue", key, "repository", repo.String(), "found", found)
	}
	if r.tokens == nil {
		return errors.New("supervise executor has no GitHub App token manager")
	}
	// The token is minted when the provision opens GitHub, under the repository's store lock
	// (workspace.FromGitHub), never here: a provision may wait on that lock behind its
	// repository's whole queue, longer than a cached token has left to live. Its owner is the one
	// GitHub holds the repository under now (serving), since a configured rename's transfer moved
	// the repository onto that owner's installation; the address below stays the repository the
	// record names. GitHub's own documentation of a transfer says "All links to the previous
	// repository location are automatically redirected to the new location. When you use `git
	// clone`, `git fetch`, or `git push` on a transferred repository, these commands will
	// redirect to the new repository location or URL", so the clone this provisions reaches the
	// repository under either name — and the name it keeps is what this issue's workspace, its
	// shared clone and, under the Sandbox runtime, its pod's feed and repository cache are keyed
	// by, which a rename must not move.
	served := r.repositories.serving(repo)
	mint := func() (string, error) {
		lease, err := r.tokens.Token(ctx, appauth.Implement, served.Owner())
		if err != nil {
			return "", fmt.Errorf("mint the implement App token for %s to provision %s: %w", served, key, err)
		}
		return lease.Token, nil
	}
	store, releaseStore := r.provisions.stores.take(located.Clone)
	defer releaseStore()
	if _, err := r.provision(ctx, workspace.Request{
		StateDir: r.stateDir, Repo: repo, Issue: key, DefaultBranch: defaultBranch, CredentialHelper: workspace.GitHubCredentialHelper,
		Source:       workspace.FromGitHub(mint, r.stateDir),
		Log:          func(line string) { r.log.Warn("provisioning: "+line, "issue", key) },
		StoreLock:    store,
		CheckoutSlot: r.provisions.slot,
	}); err != nil {
		return fmt.Errorf("provision workspace for %s: %w", key, err)
	}
	return nil
}

// seedGate reads the document a gate was registered on and applies what Dispatch shows there
// (seedGateDocument), then, for a step an ancestor's approval opened, the ancestor's reviews of the
// version that approval is of (seedGateBasis). The root's own document is read first: a newer
// version of it closes the step and ends its basis, and a step that no longer stands on a basis
// reads no ancestor. Neither read moves a step that stands open on an ancestor's approval except
// as the root's own document moves it (dispatch://INFRA-3320, R3): the read of the registered
// version returns at it, and a person's changes request on the ancestor's version, made after the
// approval, is passed on with the step left open.
func (r *outbox) seedGate(ctx context.Context, row record.OutboxRow, payload record.GateSeed) error {
	if r.dispatch == nil {
		return errors.New("gate seed executor has no Dispatch client")
	}
	if err := r.seedGateDocument(ctx, row, payload); err != nil {
		return err
	}
	return r.seedGateBasis(ctx, row, payload)
}

// seedGateDocument reads the approval of the document the gate seed names and applies what it
// finds as the event that would have carried it.
func (r *outbox) seedGateDocument(ctx context.Context, row record.OutboxRow, payload record.GateSeed) error {
	approval, err := r.dispatch.Approval(ctx, payload.ArtifactID)
	if err != nil {
		return fmt.Errorf("read Dispatch approval %s: %w", payload.ArtifactID, err)
	}
	// The version Dispatch holds is the current one, even one the architect did not see: approved
	// there, the gate opens at it; changes requested on it, the gate closes with the reason, which
	// tells the architect unless the gate already records that review as told; otherwise a later
	// version still moves the gate to it, closed. A review carries its place in the issue's Dispatch
	// event log (Seq, At), which identifies it in the told record, places it against a root's wait,
	// and names it in the fact's id, so a second registration of one version in one generation,
	// whose seed reads a newer review, is not dropped as the first seed's. A version carries no
	// place; it says instead whether the read showed the approval request waiting on the agent
	// (RequestWaitsOnAgent), as a newer version leaves it, which is what lets it answer a root
	// waiting on its gate. A document Dispatch shows below the version the seed was queued at has a
	// gate above any version Dispatch wrote (a daemon that registered the caller's version unchecked
	// could record one), so the seed resets the gate to Dispatch's version with what it read there
	// (intake.GateAboveDispatch). A review also names its reviewer, since the seed is not fenced on
	// the issue's own run and its review may meet a parked tree.
	fact := intake.DispatchArtifact{Key: row.Issue, ArtifactID: payload.ArtifactID, Kind: intake.DispatchArtifactVersion, Version: approval.LatestVersion,
		RequestWaitsOnAgent: approval.AskID != "" && approval.WaitingOn == "agent"}
	review := record.EventMark{Seq: approval.Seq, At: approval.At}
	changes, reason := approval.ChangesRequestedLatest()
	switch {
	case approval.ApprovedLatest() != 0:
		fact.Kind, fact.Mark, fact.Actor = intake.DispatchArtifactApproved, review, approval.LatestReviewer()
	case changes != 0:
		fact.Kind, fact.Reason, fact.Mark, fact.Actor = intake.DispatchArtifactChangesRequested, reason, review, approval.LatestReviewer()
	case approval.LatestVersion == payload.Version:
		return nil
	}
	if approval.LatestVersion < payload.Version {
		id := fmt.Sprintf("gate-above:%s:%d:%s:%s:%d:%s:%d:%d", row.Issue, payload.Generation, payload.Step, payload.ArtifactID, payload.Version, fact.Kind, fact.Version, fact.Mark.ID())
		if _, err := intake.ApplyFact(ctx, r.pool, "outbox", id, intake.GateAboveDispatch{Generation: payload.Generation, Above: payload.Version, Read: fact}, r.handlers...); err != nil {
			return fmt.Errorf("reset the gate of %s to the version Dispatch holds: %w", row.Issue, err)
		}
		return nil
	}
	id := fmt.Sprintf("gate-seed:%s:%d:%s:%s:%s:%d:%d", row.Issue, payload.Generation, payload.Step, payload.ArtifactID, fact.Kind, fact.Version, fact.Mark.ID())
	// A version fact carries no mark, so its id also says whether the read showed the request with
	// the agent: a later seed of the same version that finds it moved there is another fact, not a
	// duplicate of one that answered nothing.
	if fact.Kind == intake.DispatchArtifactVersion && fact.RequestWaitsOnAgent {
		id += ":with-agent"
	}
	_, err = intake.ApplyFact(ctx, r.pool, "outbox", id, fact, r.handlers...)
	if err != nil {
		return fmt.Errorf("apply approved gate seed for %s: %w", row.Issue, err)
	}
	return nil
}

// seedGateBasis reads, for the step of the row's issue the seed names when it stands open on an
// ancestor's approval (record.DesignGate.OnBasis) on the document the seed names, the ancestor's
// reviews of the version the basis names, and applies each person's changes request on it that
// Dispatch recorded after the approval the step opened on (workflow.BasisChangesRequest, the test
// the live event passes too), oldest first, with the reason the person gave, as a fact of its own
// (intake.BasisChangesRead): a changes request whose event came between register_gate's read and
// the registration's commit, or never came, still reaches the root's architect and the
// ancestor's, naming that reason. The workflow tells each review once, whatever else carried it,
// and leaves the step open. A step on no basis, or on a basis no such review stands on, reads
// nothing more. The document's approval is read as well, for the sequence of the newest review,
// which the review list does not carry.
func (r *outbox) seedGateBasis(ctx context.Context, row record.OutboxRow, payload record.GateSeed) error {
	step := payload.Step.OrSpec()
	var gate *record.DesignGate
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		gates, err := r.records.GatesOf(ctx, tx, row.Issue)
		if held, ok := gates[step]; ok {
			gate = &held
		}
		return err
	}); err != nil {
		return fmt.Errorf("read the design gate of %s: %w", row.Issue, err)
	}
	if gate == nil || !gate.OnBasis() || gate.ArtifactID != payload.ArtifactID {
		return nil
	}
	basis := *gate.Basis
	reviews, err := r.dispatch.Reviews(ctx, basis.ArtifactID)
	if err != nil {
		return fmt.Errorf("read Dispatch reviews of %s, the basis of %s: %w", basis.ArtifactID, row.Issue, err)
	}
	var asked []dispatch.Review
	for _, review := range slices.Backward(reviews) {
		if workflow.BasisChangesRequest(*gate, review.State == "changes_requested", review.Version, review.Actor, record.EventMark{At: review.At}) {
			asked = append(asked, review)
		}
	}
	if len(asked) == 0 {
		return nil
	}
	approval, err := r.dispatch.Approval(ctx, basis.ArtifactID)
	if err != nil {
		return fmt.Errorf("read Dispatch approval %s, the basis of %s: %w", basis.ArtifactID, row.Issue, err)
	}
	for _, review := range asked {
		read := basisReview(review, approval)
		id := fmt.Sprintf("basis-changes:%s:%d:%s:%s:%d:%d", row.Issue, payload.Generation, step, basis.ArtifactID, basis.Version, read.Mark.ID())
		if _, err := intake.ApplyFact(ctx, r.pool, "outbox", id, intake.BasisChangesRead{Issue: row.Issue, Step: step, Generation: payload.Generation,
			BasisArtifactID: basis.ArtifactID, BasisVersion: basis.Version, Changes: read}, r.handlers...); err != nil {
			return fmt.Errorf("apply the gate seed's read of the basis of %s: %w", row.Issue, err)
		}
	}
	return nil
}

// basisReview is review as a seed's fact carries it (intake.BasisReview): its time, who gave it
// and the reason it gave, and, where it is the review the document's approval shows, that
// approval's sequence, which the review list does not carry.
func basisReview(review dispatch.Review, approval dispatch.Approval) intake.BasisReview {
	read := intake.BasisReview{Mark: record.EventMark{At: review.At}, Actor: review.Actor, Reason: review.Reason}
	if approval.Version != nil && *approval.Version == review.Version && approval.State == review.State && approval.At.Equal(review.At) {
		read.Mark.Seq = approval.Seq
	}
	return read
}

// seedPendingBasis reads, for the row's issue, a root whose step waits, closed, on an ancestor's
// approval request (record.PendingBasisSeed), Dispatch's state of the ancestor's document and
// issue, and of the request's close where the document's approval shows it closed (readBasis),
// and applies the read as a fact of its own (intake.PendingBasisRead), never the gate seed's, so
// the read reaches the root's step alone and not the ancestor's own gate. It is the one read that
// opens or ends such a wait: the registration queues it, the boot queues it for every such step,
// and every event that is a cue for the step queues it (workflow's pendingBasisCue). The workflow
// judges it by its one verdict (workflow's judgeBasisRead): the person's approval of the version,
// as the document's latest review of its latest version, opens the step on an ancestor that still
// meets R1's first condition, and a changes request on that version, a newer version of the
// document, the request no longer open, which the end names with whoever closed it, or the
// ancestor's leave ends the wait. The workflow applies the fact only while the step still waits on
// that request and registration in that generation, so a read of a wait that has opened or ended
// since changes nothing. The fact's id names the step, the request, the registration it reads for
// (the seed's RegisteredAt, which record.PendingBasisSeedOf normalized once as it queued the seed)
// and what the read found (basisReadFound), so the same read again of the same wait is a
// duplicate, while a read of a later registration on the same request or a read that finds
// anything else, another closer included, is a fact of its own. A failed read, of any of the
// three, is the row's error, and the row is retried on its backoff (permanentRefusal names no
// pending-basis seed): no event waits on it.
func (r *outbox) seedPendingBasis(ctx context.Context, row record.OutboxRow, payload record.PendingBasisSeed) error {
	if r.dispatch == nil {
		return errors.New("pending-basis seed executor has no Dispatch client")
	}
	read, err := readBasis(ctx, r.dispatch, payload)
	if err != nil {
		return err
	}
	fact := intake.PendingBasisRead{Issue: row.Issue, Step: payload.Step, Generation: payload.Generation, RegisteredAt: payload.RegisteredAt,
		ArtifactID: payload.ArtifactID, BasisArtifactID: payload.BasisArtifactID, BasisVersion: payload.BasisVersion, Ask: payload.Ask, Read: read}
	id := fmt.Sprintf("pending-basis:%s:%d:%s:%s:%d:%s:%d:%s", row.Issue, payload.Generation, payload.Step, payload.Ask, payload.RegisteredAt.UnixMicro(),
		payload.BasisArtifactID, payload.BasisVersion, basisReadFound(read))
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", id, fact, r.handlers...); err != nil {
		return fmt.Errorf("apply the pending-basis seed of %s: %w", row.Issue, err)
	}
	return nil
}

// readBasis is Dispatch's state now of what seed's step waits on, closed (intake.BasisRead): the
// approval of the ancestor's document the seed names, and the issue Dispatch names the document
// on, read as register_gate reads it. Where that approval shows the request the step waits on no
// longer open with no approval of the version (workflow.RequestClosed), and there alone, it reads
// that request by its id as well (dispatch.Client.Ask), for who closed it and how
// (intake.BasisRead.Closed): every other read makes two calls of Dispatch and this one three. A
// request the third call shows still open is no close Dispatch shows: its approval and its ask
// were read either side of a reopen that restored it, so that read is an error, and the seed's
// next run reads all three again.
func readBasis(ctx context.Context, client dispatch.Client, seed record.PendingBasisSeed) (intake.BasisRead, error) {
	approval, err := client.Approval(ctx, seed.BasisArtifactID)
	if err != nil {
		return intake.BasisRead{}, fmt.Errorf("read Dispatch approval %s: %w", seed.BasisArtifactID, err)
	}
	ancestor, err := client.GetIssue(ctx, approval.IssueKey)
	if err != nil {
		return intake.BasisRead{}, fmt.Errorf("read Dispatch issue %s, the ancestor whose document %s is: %w", approval.IssueKey, seed.BasisArtifactID, err)
	}
	read := intake.BasisRead{Approval: approval, Ancestor: intake.AncestorRead{Status: ancestor.Status, HandedOver: ancestor.HandedOver}}
	if !workflow.RequestClosed(approval, seed.BasisVersion, seed.Ask) {
		return read, nil
	}
	ask, err := client.Ask(ctx, seed.Ask)
	if err != nil {
		return intake.BasisRead{}, fmt.Errorf("read Dispatch ask %s, the approval request of version %d of %s that its approval shows closed: %w",
			seed.Ask, seed.BasisVersion, seed.BasisArtifactID, err)
	}
	if ask.State == "open" {
		return intake.BasisRead{}, fmt.Errorf("Dispatch shows approval request %s of version %d of %s open, though the document's approval shows it closed",
			seed.Ask, seed.BasisVersion, seed.BasisArtifactID)
	}
	read.Closed = &intake.BasisClose{Actor: ask.ClosedBy, State: ask.State}
	return read, nil
}

// basisReadFound names what read found, for the id of the fact that carries it: the document's
// approval state, its latest version, the version and the place of its latest review and who gave
// it, the request open on it, the ancestor's status and label, and, for a read of the request's
// close, who closed it and its state. Two reads that name the same found the same, which the
// workflow judges alike.
func basisReadFound(read intake.BasisRead) string {
	approval := read.Approval
	version := 0
	if approval.Version != nil {
		version = *approval.Version
	}
	closer, closed := "", ""
	if read.Closed != nil {
		closer, closed = read.Closed.Actor.Name(), read.Closed.State
	}
	return fmt.Sprintf("%s:%d:%d:%d:%s:%s:%s:%t:%s:%s", approval.State, approval.LatestVersion, version, record.EventMark{Seq: approval.Seq, At: approval.At}.ID(),
		approval.By.Name(), approval.AskID, read.Ancestor.Status, read.Ancestor.HandedOver, closer, closed)
}

// seedReparent reads, for the row's issue, an issue in Legion's work whose Dispatch parent changed
// (record.ReparentSeed, R18 of dispatch://INFRA-3320), whether R1's path is open on its new parent
// (R1 and R2; reparentBasisVersion), and applies what it read as a fact (intake.ReparentRead),
// whose workflow handler queues one reparented notice for each distinct architect among the
// issue's own, the one above it before and the one above it now. The read decides only the words:
// a parent R1's path is closed on, and a move to no parent, get no word of it. A row of
// INFRA-4728's shape is read and applied the same way, and the handler reads what it lacks from
// the record. The fact's id is the row's, so a run of the same row again is a duplicate, and the
// seed of a later move is a fact of its own.
//
// While the seed waits on the chain of its new parent (workflow.ReparentChainPending), each
// attempt notes that parent for R16's re-read (noteUnknownChain), which reads it and the links
// above it from Dispatch and applies what it read, so a later attempt finds the chain known: no
// admission pass need reach that parent, since the issue may stand at phase done or under a later
// parent. The re-read holds a noted key once, on its own backoff, so a note at each attempt adds
// no Dispatch read.
func (r *outbox) seedReparent(ctx context.Context, row record.OutboxRow, payload record.ReparentSeed) error {
	if r.dispatch == nil {
		return errors.New("re-parent seed executor has no Dispatch client")
	}
	// Wait before reading Dispatch. The workflow checks again when the fact applies,
	// because the copy can change while this executor is reading the parent.
	var version int
	if payload.Parent != "" {
		var pending bool
		if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			pending, err = workflow.ReparentChainPending(ctx, tx, r.records, row.Issue, payload)
			return err
		}); err != nil {
			return fmt.Errorf("read the chain for the re-parent seed of %s: %w", row.Issue, err)
		}
		if pending {
			if r.noteUnknownChain != nil {
				r.noteUnknownChain(payload.Parent)
			}
			return wait.Errorf("re-parent seed of %s waits for the chain of %s: %w", row.Issue, payload.Parent, record.ErrChainUnknown)
		}
		var err error
		if version, err = r.reparentBasisVersion(ctx, payload.Parent); err != nil {
			return err
		}
	}
	id := fmt.Sprintf("reparent-seed:%s:%d", row.Issue, row.ID)
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", id, intake.ReparentRead{Issue: row.Issue, Seed: payload, BasisVersion: version}, r.handlers...); err != nil {
		return fmt.Errorf("apply the re-parent seed of %s: %w", row.Issue, err)
	}
	return nil
}

// reparentBasisVersion is the version of parent's primary document that R1's path can rest a
// child root on, 0 where the path is closed: parent carries the legion label, Dispatch shows it
// not accepted (record.AcceptedStatus), a person approved the latest version of its primary
// document, and that version marks a requirement block that names an id, as register_gate reads
// one (dispatchblocks.RequirementIDs).
func (r *outbox) reparentBasisVersion(ctx context.Context, parent string) (int, error) {
	issue, err := r.dispatch.GetIssue(ctx, parent)
	if err != nil {
		return 0, fmt.Errorf("read Dispatch issue %s, a new parent: %w", parent, err)
	}
	if !dispatch.CarriesLegionLabel(issue.Labels) || record.AcceptedStatus(issue.Status) || issue.PrimaryArtifactID == "" {
		return 0, nil
	}
	approval, err := r.dispatch.Approval(ctx, issue.PrimaryArtifactID)
	if err != nil {
		return 0, fmt.Errorf("read Dispatch approval %s, the primary document of %s: %w", issue.PrimaryArtifactID, parent, err)
	}
	version := approval.ApprovedLatest()
	if version == 0 || !approval.By.Person() {
		return 0, nil
	}
	markdown, err := r.dispatch.DocumentVersion(ctx, issue.PrimaryArtifactID, version)
	if err != nil {
		return 0, fmt.Errorf("read version %d of %s, the primary document of %s: %w", version, issue.PrimaryArtifactID, parent, err)
	}
	if len(dispatchblocks.RequirementIDs(markdown)) == 0 {
		return 0, nil
	}
	return version, nil
}

// issueLaunch is what a start reads of its issue on the Dispatch chain as the copy shows it now
// (launchOf): tree, the gate root its launch names (record.Chains.StateTree: the issue's gate root
// while it is in Legion's work, R22 of dispatch://INFRA-3320), and holdsGate, whether it holds its
// own design gate (record.Chains.HoldsOwnGate, R15: a root, or an issue that ran as one and keeps
// its gate under a parent in Legion's work).
type issueLaunch struct {
	tree      string
	holdsGate bool
}

// launchOf reads issue's launch (issueLaunch) in a read-only transaction of its own just before a
// start acts, as every fence of a row is.
func (r *outbox) launchOf(ctx context.Context, issue record.Issue) (issueLaunch, error) {
	var launch issueLaunch
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		chains, err := record.ReadChainsOf(ctx, tx, r.records, issue.Project, []string{issue.Key})
		if err != nil {
			return err
		}
		launch = issueLaunch{tree: chains.StateTree(issue.Key), holdsGate: chains.HoldsOwnGate(issue.Key)}
		return nil
	}); err != nil {
		return issueLaunch{}, fmt.Errorf("read the gate root of %s: %w", issue.Key, err)
	}
	return launch, nil
}

// gateAbove is the design gate of an issue's gate root as a hold of a row reads it just before the
// row acts (readGateAbove): root is the gate root, the nearest issue at or above the issue in
// Legion's work that holds its own gate on the Dispatch chain (record.Chains.StateTree), so every
// issue under that root reads the same rows; ownGate says whether that root is the issue itself
// (record.Chains.HoldsOwnGate); and rows are the root's rows by step.
type gateAbove struct {
	root    string
	ownGate bool
	rows    map[record.DesignStep]record.DesignGate
}

// readGateAbove reads issue's gateAbove in a read-only transaction of its own, as every other
// fence of a row is read.
func (r *outbox) readGateAbove(ctx context.Context, issue record.Issue) (gateAbove, error) {
	var above gateAbove
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		chains, err := record.ReadChainsOf(ctx, tx, r.records, issue.Project, []string{issue.Key})
		if err != nil {
			return err
		}
		above.root = chains.StateTree(issue.Key)
		above.ownGate = above.root == issue.Key && chains.HoldsOwnGate(issue.Key)
		above.rows, err = r.records.GatesOf(ctx, tx, above.root)
		return err
	}); err != nil {
		return gateAbove{}, fmt.Errorf("read the design gate above %s: %w", issue.Key, err)
	}
	return above, nil
}

// designGateClosed says whether the design gate of the issue's gate root is closed for a role
// other than the merger, which holds a held message to such a role (deliverHeld): it is closed
// when a step the tree is gated on holds a row no approval has opened (classify.StandingGate, the
// first such step in the configured order), the rule the message of a gate that closed or opened
// reads too (workflow's tellGateMoved), so the hold keeps no role from a message after it was told
// the gate opened. The steps are the deployment's and a criteria row the gate root's architect
// registered beside a deployment of spec alone (record.TreeSteps). A gate root with no row for a
// step — nothing registered one, or the project's design gate is off, which registers it approved
// — holds nothing on that step: no role was told of a gate the record does not know. root is the
// gate root read, for the wait's words. The merger's own hold is mergeHold.
func (r *outbox) designGateClosed(ctx context.Context, issue record.Issue) (root string, closed bool, err error) {
	above, err := r.readGateAbove(ctx, issue)
	if err != nil {
		return "", false, err
	}
	_, closed = classify.StandingGate(r.gatedSteps, above.rows)
	return above.root, closed, nil
}

// mergeHold is what holds the merge of issue now (classify.MergeHold over the rows of its gate
// root, readGateAbove): the gate root read, the step that holds the merge ("" for none) and
// whether a row of that step is registered. An issue that holds its own gate and none of whose
// rows has ever stood open is held by a gated step that has no row, as its start is, unless a
// hand-over of the release before carried it in without a gate, which its own record says; every
// other issue is held by a registered step that no approval has opened, as designGateClosed reads
// it.
func (r *outbox) mergeHold(ctx context.Context, issue record.Issue) (root string, step record.DesignStep, registered bool, err error) {
	above, err := r.readGateAbove(ctx, issue)
	if err != nil {
		return "", "", false, err
	}
	step, _, held := classify.MergeHold(r.gatedSteps, above.rows, above.ownGate, issue)
	if !held {
		return above.root, "", false, nil
	}
	_, registered = above.rows[step]
	return above.root, step, registered, nil
}

// startNewsOf is what a start carries of news of the approved design, or that the start finishes
// without acting (finished), in the order the start executor asks it (supervise):
//
//  1. A start queued for news alone (record.SuperviseRequest.News) reaches only a claim such a
//     start may reach as the claim stands now (newsOnlyRefusal): one that exists and was given the
//     entry its role's phases row names, has not ended, and that no suspension that holds (the
//     operator's or the controller's) is over. A root's architect is read so too,
//     since it holds a phases row that states the work it is owed, as a phase worker does
//     (dispatch://INFRA-2614): an architect behind its row's entry is owed that entry's start,
//     which carries its summary. It never creates a claim, revives one or resumes an agent such a
//     suspension holds, and one it may not reach finishes it, since the start that brings that
//     agent back carries the news. The generation, the phase, the workflow and the linger have
//     fenced it before this, as they fence any start.
//  2. A merger's start waits while its tree's design gate is closed or its news is not recorded
//     (mergerWaitsForTheDesignGate), after the refusal, so a news-only start that reaches no claim
//     finishes rather than waits.
//  3. Every phase start (an entry's work, the held phase's retry among them) and every news-only
//     start carries the news its role is owed, read from the record as it stands now
//     (newsForStart), with no Dispatch read: an item whose change set is not recorded is left
//     owed. A news-only start the record owes nothing now finishes. Any other start carries its
//     own text alone.
func (r *outbox) startNewsOf(ctx context.Context, row record.OutboxRow, issue record.Issue, payload record.SuperviseRequest,
	machine *supervise.Machine, found bool, token claim.Token) (news startNews, finished bool, err error) {
	if payload.News {
		entry, err := r.phaseEntry(ctx, row.Issue, payload.Role)
		if err != nil {
			return startNews{}, false, err
		}
		if why := newsOnlyRefusal(machine, found, entry); why != "" {
			r.log.Info("outbox news-only start reaches no claim; finished without acting", "row", row.ID, "issue", issue.Key,
				"role", payload.Role, "why", why)
			return startNews{}, true, nil
		}
	}
	if err := r.mergerWaitsForTheDesignGate(ctx, issue, payload.Role); err != nil {
		return startNews{}, false, err
	}
	if !payload.News && payload.Entry == 0 {
		return startNews{task: payload.Task, roundNamed: payload.RoundNamed}, false, nil
	}
	if news, err = r.newsForStart(ctx, issue, payload, token); err != nil {
		return startNews{}, false, err
	}
	if payload.News && !news.says {
		r.log.Info("outbox news-only start finds nothing owed; finished without acting", "row", row.ID, "issue", issue.Key,
			"role", payload.Role)
		return startNews{}, true, nil
	}
	return news, false, nil
}

// mergerWaitsForTheDesignGate is the one role fence a start carries: the merger merges nothing
// while a step of its tree's design gate holds the merge (mergeHold), because a new version of the
// spec is with its reviewer and what may merge is what that review decides
// (dispatch://INFRA-714/spec R5), or because nobody has approved the design of an issue that holds
// its own gate and that no hand-over of the release before carried in without a gate (R19 of
// dispatch://INFRA-3320: its merger waits for the gates its start waits for).
// It answers a wait error for a merger start the gate holds, which leaves the start on the row's
// backoff as a check start waits on its look (checkLookHolds), and nil for every other role and
// an open gate. The wait's words name the step; for a step with no registered document they say
// that the issue's architect registers the document of that step and requests the person's
// approval, which its catch-up says to it too (workflow's treeCatchUp). The gate opening releases
// the row's next try, and tells every phase worker of the tree and the root's architect as well
// (workflow's tellGateMoved).
//
// It also waits while the merger's member is owed news of the approved design that no start can
// carry yet, its change set not recorded, read or unavailable, or its earlier version neither read
// nor recorded unread with no reconcile of the tree pending (mergerNewsUnrecorded): a merger
// released without the change list could merge before it reads what changed. That wait ends within
// the design_changes row's bounded attempts, since a pair Dispatch cannot read is recorded
// unavailable, and a review history it cannot read is recorded unread (designChangesAttempts), and
// the start then carries the news (newsForStart).
//
// Every held message to a released merger (deliverHeld: a merge-queue removal, a merge forward's
// hold and landed, and a merge-holds change) reads this same rule before it wakes it, so no such
// message brings back a merger that its own start would hold.
func (r *outbox) mergerWaitsForTheDesignGate(ctx context.Context, issue record.Issue, role claim.Role) error {
	if role != claim.RoleMerger {
		return nil
	}
	root, step, registered, err := r.mergeHold(ctx, issue)
	if err != nil {
		return err
	}
	switch {
	case step != "" && !registered:
		return wait.Errorf("the merger of %s waits for the %s step of the design gate of %s, which has no registered document: the architect of %s registers the document of that step with register_gate and requests the person's approval",
			issue.Key, step, root, root)
	case step != "":
		return wait.Errorf("the merger of %s waits for the design gate of %s to open at its %s step", issue.Key, root, step)
	}
	unrecorded, err := r.mergerNewsUnrecorded(ctx, issue)
	if err != nil || !unrecorded {
		return err
	}
	return wait.Errorf("the merger of %s waits for what changed in the approved design of %s to be recorded", issue.Key, root)
}

// checkLookHolds is the production check a check start row still has a look to start in, nil for
// none: its issue's production check is open, the one the row names (record.ProductionCheck.NamedBy),
// at the look the row names, with that look open, and the issue out of the workflow. A check closed
// since (a report, its last look), a check a later merge replaced, a look its bound or a hold ended,
// a later look's start, or an issue running again (reopened since the row was
// queued: a look would resume the check's own role and relaunch the Sandbox under the run; the look ends
// at its own bound) leave the row nothing to start, and it finishes without acting, logged with why.
func (r *outbox) checkLookHolds(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest, issue record.Issue) (*record.ProductionCheck, error) {
	check, err := r.productionCheck(ctx, row.Issue)
	if err != nil {
		return nil, err
	}
	var why string
	switch {
	case check == nil:
		why = "the issue has no production check"
	case record.InWorkflow(issue):
		why = fmt.Sprintf("the issue runs again (%s, %s)", issue.Phase, issue.Status)
	case !check.Open():
		why = fmt.Sprintf("the check closed as %s", check.Outcome)
	case !check.NamedBy(payload.CheckOpenedAt):
		why = fmt.Sprintf("the check it names was replaced by the one opened %s", check.OpenedAt.UTC().Format(time.RFC3339))
	case check.Looks != payload.CheckLook:
		why = fmt.Sprintf("the check is at look %d", check.Looks)
	case !check.LookOpen:
		why = "the look has ended"
	default:
		return check, nil
	}
	r.log.Info("production check start finishes without acting", "row", row.ID, "issue", row.Issue, "look", payload.CheckLook, "why", why)
	return nil, nil
}

// checkLookHeld is a check start that met a hold of the check's own role: a suspension of the
// operator's or the controller's (supervise.Claim.RuleHolds), holds naming whose
// (supervise.Claim.Holder). A phase start waits for the resume that lifts it, but a look cannot
// run while the role is held, and its bound would end it anyway, so the look ends now as one
// nobody reported (intake.CheckLookEnded, of check, the one checkLookHolds read) and the row
// finishes without acting. Its cause names the holder — `operator hold` or `controller hold` —
// so the retry the look names says whose resume it waited on. The event is the
// issue, the check (record.ProductionCheck.ID) and the look: the same look held twice is one
// fact, and a later check's look 1 another than the replaced check's.
func (r *outbox) checkLookHeld(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest, check record.ProductionCheck,
	holds supervise.SuspendSource) error {
	r.log.Info("production check start finishes without acting", "row", row.ID, "issue", row.Issue, "look", payload.CheckLook,
		"why", fmt.Sprintf("the %s suspended the check's role; the look ends", holds))
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("check-look-hold:%s:%s:%d", row.Issue, check.ID(), payload.CheckLook),
		intake.CheckLookEnded{Issue: row.Issue, Look: payload.CheckLook, Cause: string(holds) + " hold", OpenedAt: check.OpenedAt}, r.handlers...); err != nil {
		return fmt.Errorf("end look %d of %s's production check on the %s's hold: %w", payload.CheckLook, row.Issue, holds, err)
	}
	return nil
}

// checkLook is a look's bound coming due (record.CheckLook): the look it names ends as one nobody
// reported (intake.CheckLookEnded, cause `look bound`), which the workflow applies only while the
// check is open at that look with the look open, so a look reported in time changes nothing. The
// check is read first: a row of no open check, or of a check a later merge replaced (NamedBy),
// finishes without acting, and the event is the issue, the check read (record.ProductionCheck.ID)
// and the look, so the row run again is a duplicate and a later check's look 1 is a fact of its
// own rather than the replaced check's. A look that began before the end of the project's latest
// pause has its bound counted from the time its role could take a turn again, and the row waits
// until then (lookBoundAfterPause).
func (r *outbox) checkLook(ctx context.Context, row record.OutboxRow, payload record.CheckLook) error {
	check, err := r.productionCheck(ctx, row.Issue)
	if err != nil {
		return err
	}
	var why string
	switch {
	case check == nil:
		why = "the issue has no production check"
	case !check.Open():
		why = fmt.Sprintf("the check closed as %s", check.Outcome)
	case !check.NamedBy(payload.OpenedAt):
		why = fmt.Sprintf("the check it names was replaced by the one opened %s", check.OpenedAt.UTC().Format(time.RFC3339))
	}
	if why != "" {
		r.log.Info("outbox check look bound finished without acting", "row", row.ID, "issue", row.Issue, "look", payload.Look, "why", why)
		return nil
	}
	if err := r.lookBoundAfterPause(ctx, row, payload.Look, *check); err != nil {
		return err
	}
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("check-look:%s:%s:%d", row.Issue, check.ID(), payload.Look),
		intake.CheckLookEnded{Issue: row.Issue, Look: payload.Look, Cause: "look bound", OpenedAt: check.OpenedAt}, r.handlers...); err != nil {
		return fmt.Errorf("end look %d of %s's production check at its bound: %w", payload.Look, row.Issue, err)
	}
	return nil
}

// lookBoundAfterPause is the wait (wait.Until) of a check_look row whose look is the check's open
// one, began before the end of the project's latest pause, and has not yet had its bound
// (workflow.CheckLookBound) since its role could take a turn again; nil for every other row. The
// pause stopped the check's role and held the look's rows, and the resume let the role's launch
// out through the wake pacing, so the bound is counted from the later of the pause's end and the
// role's last ready where that ready is after the end (record.RoleClaim.LastReady), and, while the
// role's claim is still coming up — queued, launch_uncertain, launching, shim_connected or
// registered — from no earlier than now, since its ready is still ahead. A look that began after
// the latest pause ended, or in a project no pause has ended, keeps the bound its row was due at.
// The pause is read as admit's readStallFloor reads it (store.OpenOrLastPauseTx), with the role's
// claim, in one read-only transaction. While the project is paused the row is held and does not
// run, so a pause read open here began after the row was claimed, and the row ends the look as it
// would have.
func (r *outbox) lookBoundAfterPause(ctx context.Context, row record.OutboxRow, look int, check record.ProductionCheck) error {
	if !check.LookOpen || check.Looks != look || check.LookWakeAt == nil {
		return nil
	}
	var latest store.Pause
	var held *record.RoleClaim
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var found bool
		var err error
		latest, found, err = store.OpenOrLastPauseTx(ctx, tx, r.dispatchProject)
		if err != nil || !found || !latest.Ended() || !check.LookWakeAt.Before(latest.EndedAt) {
			latest = store.Pause{}
			return err
		}
		token, err := claim.NewToken(r.project, row.Issue, check.Role)
		if err != nil {
			return err
		}
		claims, err := r.records.RoleClaims(ctx, tx, []claim.Token{token})
		held = claims[token]
		return err
	}); err != nil {
		return fmt.Errorf("read the latest pause of %s and the role of %s's production check: %w", r.dispatchProject, row.Issue, err)
	}
	if latest.ID == 0 {
		return nil
	}
	from, now := latest.EndedAt, r.now().UTC()
	if held != nil {
		if held.LastReady.After(from) {
			from = held.LastReady
		}
		comingUp := !supervise.SaidReady(held.State) && !supervise.IsGone(held.State)
		if comingUp && now.After(from) {
			from = now
		}
	}
	due := from.Add(workflow.CheckLookBound)
	if !now.Before(due) {
		return nil
	}
	return wait.Until(due, fmt.Sprintf("look %d of %s's production check began before pause %d ended at %s, so its bound is counted from %s, when its %s could take a turn again",
		look, row.Issue, latest.ID, latest.EndedAt.UTC().Format(time.RFC3339), from.UTC().Format(time.RFC3339), check.Role))
}

// checkWake is a check's named wake coming due (record.CheckWakeDue): the look the check stands
// at is woken (intake.CheckWakeDue), which starts the next look where the check is still open at
// the look and the instance the row names, with no look open and its issue out of the workflow,
// and otherwise changes nothing or queues the row again, as the workflow decides
// (workflow.checkWakeDue). The row carries the check instance it was named for, so nothing is
// read here before the fact: the workflow reads the check in the fact's own transaction, where no
// report can land between the read and the look it starts. The event is the issue and this row,
// as the check's bound is (checkBound), since the workflow queues the wake again as a new row
// while the issue runs again, and that row's apply must be fresh rather than a duplicate of this
// one's.
func (r *outbox) checkWake(ctx context.Context, row record.OutboxRow, payload record.CheckWakeDue) error {
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("check-wake:%s:%d", row.Issue, row.ID),
		intake.CheckWakeOf(row.Issue, payload), r.handlers...); err != nil {
		return fmt.Errorf("wake %s's production check at look %d: %w", row.Issue, payload.Look, err)
	}
	return nil
}

// checkBound is the whole check's bound coming due (record.CheckBound): intake.CheckBound, under
// an event of this row's own rather than the check's, since a bound the workflow defers — a look
// open at the bound, an earlier check's bound — is queued again as a new row, whose apply must be
// fresh rather than a duplicate of this one's.
func (r *outbox) checkBound(ctx context.Context, row record.OutboxRow) error {
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("check-bound:%s:%d", row.Issue, row.ID),
		intake.CheckBound{Issue: row.Issue}, r.handlers...); err != nil {
		return fmt.Errorf("apply the bound of %s's production check: %w", row.Issue, err)
	}
	return nil
}

// productionCheck is issue's production check as recorded, nil for none, read in a transaction of
// its own just before the executor acts on a row.
func (r *outbox) productionCheck(ctx context.Context, issue string) (*record.ProductionCheck, error) {
	var check *record.ProductionCheck
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		check, err = r.records.ProductionCheck(ctx, tx, issue)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read the production check of %s: %w", issue, err)
	}
	return check, nil
}

// phaseEntry is the phase entry role's phases row of issue names (record.PhaseRow.Entry), read in
// a transaction of its own just before the executor acts on a row, and 0 for a role whose row
// holds no entry and for one the record has no row of at all. The transition writes that number
// in its own transaction before the start it queues can run (workflow.Engine.enter), so a start
// reading a number above its own is reading a later entry of the same phase.
func (r *outbox) phaseEntry(ctx context.Context, issue string, role claim.Role) (int64, error) {
	var entry int64
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := r.records.Phases(ctx, tx, issue)
		if err != nil {
			return err
		}
		entry = record.PhaseRowOf(rows, role).Entry
		return nil
	}); err != nil {
		return 0, fmt.Errorf("read the phase entry of %s's %s: %w", issue, role, err)
	}
	return entry, nil
}

// issueBranch creates the issue's branch, legion/<KEY>, on GitHub at the repository's default
// branch, so no role's push is the one that creates it: GitHub can refuse such a push on a large
// repository (its check for changed workflow files, which it runs for an App without the workflows
// permission, times out on a new ref), where it takes one that moves the branch. A branch GitHub
// already has is left where it is. A start of the issue waits for this row (the lifecycle lane's
// ordering rule, record.Store.DueRows and ClaimRow), so a create GitHub refuses holds back the
// issue's roles, each attempt logged with GitHub's answer, until one passes. A row of a generation
// the issue has left finishes without acting before it gets here (run's fence, outlived): the
// starts it held back act on none.
func (r *outbox) issueBranch(ctx context.Context, row record.OutboxRow, _ record.IssueBranch) error {
	issue, err := r.issue(ctx, row.Issue)
	if err != nil {
		return err
	}
	if err := r.createBranch(ctx, issue); err != nil {
		return fmt.Errorf("create the branch of %s, which its roles' starts wait for: %w", issue.Key, err)
	}
	return nil
}

// createBranch creates issue's branch on GitHub, in the repository GitHub holds the one its
// record names under now (serving), at the default branch the record names, unless GitHub
// already has it, as the implement App for that repository's owner: the App of the roles that
// write an issue branch (appauth.AppRoleFor), and the one workspace provisioning clones with.
// The create is a REST write, so a tree recorded before a transfer gets its branch on the
// repository as GitHub holds it today rather than a write to an owner whose installation has
// lost it. The ref name passes the deployment's private-name rules first, the ones boot compiled
// (privateNameRules), so the create runs the same file every role runs.
func (r *outbox) createBranch(ctx context.Context, issue record.Issue) error {
	ctx, cancel := context.WithTimeout(ctx, issueBranchTimeout)
	defer cancel()
	recorded, err := issue.Repository()
	if err != nil {
		return err
	}
	repo := r.repositories.serving(recorded)
	github, err := implementClient(ctx, r.tokens, r.githubAPI, repo)
	if err != nil {
		return fmt.Errorf("mint the implement App token for %s: %w", repo, err)
	}
	return ghbranch.Create(ctx, github, repo, workspace.Bookmark(issue.Key), issue.DefaultBranch, r.privateNameRules)
}

// recordedIssue is key's issue as the record holds it, nil when the daemon keeps none: the one
// read every rule and executor of this package asks the record through, so there is one
// transaction shape and one failure message for it.
func (r *outbox) recordedIssue(ctx context.Context, key string) (*record.Issue, error) {
	var issue *record.Issue
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(ctx, tx, key)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read workflow issue %s: %w", key, err)
	}
	return issue, nil
}

// issue is recordedIssue where the caller's work has no meaning without the record: an issue the
// daemon keeps none of is that caller's failure, not an answer.
func (r *outbox) issue(ctx context.Context, key string) (record.Issue, error) {
	issue, err := r.recordedIssue(ctx, key)
	if err != nil {
		return record.Issue{}, err
	}
	if issue == nil {
		return record.Issue{}, fmt.Errorf("workflow issue %s is not recorded", key)
	}
	return *issue, nil
}

// fenceOf is the fence issue stands at as the record holds it now (record.ReadFence), with the
// pod its claims run in where withPod: what each effect of it is checked against where it
// commits.
func (r *outbox) fenceOf(ctx context.Context, issue string, withPod bool) (record.Fence, error) {
	var fence record.Fence
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		fence, err = record.ReadFence(ctx, tx, r.records, issue, withPod)
		return err
	}); err != nil {
		return record.Fence{}, fmt.Errorf("read the fence of %s: %w", issue, err)
	}
	return fence, nil
}

// lateAt answers the fence payload, row's effect, was queued at (record.FenceOf), the fence row's
// issue stands at now (fenceOf), and whether payload does nothing to the issue there
// (record.LateAt): the one read of a row's fence before its executor runs, which outlived and a
// merge-forward ask's late end (lateMergeForwardAsk) each make once and log in their own words. A
// kind that carries no fence, and a row that names none (one a release before this one queued), is
// late at nothing, and so is every row of an outbox with no store (a test's, as an outbox with no
// supervisor publishes as before): it holds no record to read a fence from. For none of them is
// anything read.
func (r *outbox) lateAt(ctx context.Context, row record.OutboxRow, payload record.OutboxPayload) (queued, now record.Fence, late bool, err error) {
	queued, carries := record.FenceOf(payload)
	if !carries || queued.Generation == 0 && queued.Entry == 0 || r.pool == nil {
		return queued, record.Fence{}, false, nil
	}
	if now, err = r.fenceOf(ctx, row.Issue, false); err != nil {
		return queued, record.Fence{}, false, err
	}
	return queued, now, record.LateAt(now, payload), nil
}

// outlived says whether payload, row's effect, queued at the fence it names, does nothing to its
// issue as the record holds it now (lateAt), which finishes without acting (R17 and R20 of
// dispatch://INFRA-3320): an effect that acts, of an earlier run or phase entry, and a notice of an
// earlier run, since a notice's delivery reads its run alone (the architect's ruling of 2026-11-21
// on dispatch://INFRA-3320). It logs the finish.
func (r *outbox) outlived(ctx context.Context, row record.OutboxRow, payload record.OutboxPayload) (bool, error) {
	queued, now, late, err := r.lateAt(ctx, row, payload)
	if err != nil || !late {
		return false, err
	}
	r.log.Info("outbox row of an earlier run or phase entry of its issue; finished without acting", "row", row.ID,
		"kind", row.Kind, "issue", row.Issue, "generation", queued.Generation, "entry", queued.Entry,
		"current_generation", now.Generation, "current_entry", now.Entry)
	return true, nil
}
