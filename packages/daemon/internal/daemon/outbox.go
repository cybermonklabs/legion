package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
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
	supervisor *supervisor
	trees      outboxTrees
	tokens     appauth.Tokens
	handlers   []intake.Handler
	project    string
	// dispatchProject is the Dispatch project whose issues' rows this outbox claims: the database
	// is shared, and another project's rows are another daemon's.
	dispatchProject string
	stateDir        string
	// gatedSteps are the design-gate steps the project gates (gates.design), which a root
	// architect's catch-up this runner writes or delivers states (workflow.QueueCatchUp,
	// workflow.TreeCatchUp; a child's catch-up states none) and whose kept approval pair a root
	// architect's start task names. A test that builds the runner by hand and leaves it nil holds
	// the list of a deployment that gates no step: the single step spec, which no person approves
	// (record.RequiredSteps).
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
	// one-time rename pass over them (workflowRuntime.podEndsOn, renamePass). It runs at three
	// places only, each once the runtime's own call has answered that the pod is gone: an idle
	// release's teardown, after ReleaseIssue returns and before the release's mark is cleared
	// (releaseIssue); an issue close, after SuspendIssue returns nil on a close the store did not
	// refuse (suspendIssue, the record.IssueSuspend case of execute); and a workflow tree close,
	// after the tree's cleanup, for each issue of the tree (endTreePods). Never after a pod
	// update's ReplacePod, which starts the next pod at once. Set by attach after newOutbox; a test
	// that builds the outbox by hand leaves it nil, and the end of a pod then runs nothing.
	podEnded func(context.Context, []string)
	// prepareHandover readies the hand-over fact consumeHandover applies, as an event's
	// preparation readies a recorded issue's (workflowRuntime.prepareHandover): where the
	// resolution the status executor read names another repository than the record's, it carries
	// what GitHub answered for that repository now, which the correction admission's hand-over
	// makes is written from. Set by attach after newOutbox; a test that builds the outbox by hand
	// leaves it nil, and the fact then carries no read, so its correction writes nothing.
	prepareHandover func(context.Context, intake.Handover) (intake.Handover, error)
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
	// limits is every GitHub rate limit a row met, by the budget it exhausted, held until its
	// deadline: a row that spends one of those budgets waits for the latest deadline among them
	// (standingLimit) rather than meet a limit again. The lanes run rows on many goroutines, so
	// limitsMu guards it (deferral, noteLimit).
	limitsMu sync.Mutex
	limits   map[githubrest.Budget]rateLimit
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

// outboxTrees is the store's tree barrier (store.Store) as the outbox runs it: an issue close's
// suspension, an idle release's own two decisions, the check of its workspace and the decline
// that check can record, a finished issue's drop of the record of its pods' disks, a stop's wait
// for a stored claim's machine, a workflow tree close's cleanup, a delivery wake and the open
// resume that refuses one, a project pause's stop of a role and which pause stopped a role, and
// the end of a pause's hold of a pod a deletion took.
type outboxTrees interface {
	Pool() *store.Pool
	IssueSuspension(ctx context.Context, project string, closing store.IssueClose, outOfWorkflow func(status string) bool) (bool, error)
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
	ReleaseIdleIssue(ctx context.Context, project, issue string, decide store.IdleReleaseDecide) (bool, error)
	ClearIssueRelease(ctx context.Context, project, issue string) error
	DeleteIssuePodDisks(ctx context.Context, issue string) error
	NoteReleaseTeardown(ctx context.Context, project, issue string, release int64, teardown store.ReleaseTeardown) error
	StoredClaimMayRun(ctx context.Context, token claim.Token, row int64) (bool, error)
	StoredClaimMayRunIn(ctx context.Context, token claim.Token, tree string) (bool, error)
	ReserveWorkflowTreeCleanup(ctx context.Context, project, tree string, rootGeneration uint64) (treelifecycle.Lifecycle, bool, error)
	CleanupReservedTree(ctx context.Context, project, tree string, epoch uint64, releaser store.TreeReleaser) error
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
		log: log, now: time.Now, clock: supervise.RealClock{},
		// WarmCodegraphIndexInBackground runs here, never in provisionWorkspace: every outbox
		// test injects its own `provision`, so only this production closure starts codegraph.
		provision: func(ctx context.Context, request workspace.Request) (workspace.Workspace, error) {
			provisioned, err := workspace.Provision(ctx, workspace.NewRunner(workspace.CommandTimeout, tools), request)
			if err != nil {
				return workspace.Workspace{}, err
			}
			workspace.WarmCodegraphIndexInBackground(provisioned.Dir)
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
// architect; a merge-queue removal, a merge forward's message, a controller notice and a role
// message a project's pause still owes its role (heldMessageOwed) wait; and any other role
// message, a hook's prompt and an update ask finish undelivered. So an operator's settle of the
// name lets a waiting row's next attempt through, while a row that finished undelivered is not
// sent again (review round 13 of #4220, note 36). A store that does not answer publishes nothing
// and fails the attempt, which the row's backoff retries.
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
	if err := r.heldBack(ctx, topic, dedupeKey); err != nil {
		return err
	}
	return r.notices.Publish(ctx, topic, message, payload, dedupeKey)
}

// publishKept is publish with keep (notify.Publisher.PublishKept), hookPrompt's publish through
// publishKeptToRole, held as publish is: a topic heldBack refuses is not published, kept or not,
// and answers the gate's notify.ErrNoHolder.
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
	return nil
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
// record hold different statuses from here on. A hand-over's row drops its fence instead: the
// write that would hand the child to Legion never lands, so no admission consumes the fence, and
// one left behind would keep naming the child's own tree as its Sandbox's home
// (store.IssueSandboxHome) for good. The child stays where it is until a person labels it, which
// admits it as an orphan. A message row Dispatch refused tells the architect that owns the issue
// (noticeArchitect) what was not posted and Dispatch's own refusal (messagePostRefused), since
// nothing else would: the message is the daemon's only word on the issue for what it carries.
// Each of those notice rows is written in the finish's own transaction, which owns the commit hook
// that write registers (projectRecords, store.OnCommit), so the notice's lane is woken as the
// finish commits and the notice is published without a sweep. Only the hand-over's finish takes
// the lock every fact applies under and settles as a fact's does (settling), since the fence it
// drops is an input of the end of a merge's run. Every other refused row's finish writes nothing
// a fact reads, so it runs in a plain transaction (store.Transact) and waits behind no fact. Each
// line says the refusal as the row and the notice keep it (storetext.FarSideError); settle logged
// it whole.
func (r *outbox) finishRefused(ctx context.Context, row record.OutboxRow, cause error) error {
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		return fmt.Errorf("decode outbox row %d: %w", row.ID, err)
	}
	kept := storetext.FarSideError(cause.Error())
	// follow is what the transaction does beside the finish, handed the transaction's ctx, and told
	// the line logged once it commits, as its hook. dropsFence is whether follow deletes a
	// hand-over's fence.
	var follow func(context.Context, pgx.Tx) error
	var told func()
	var dropsFence bool
	switch value := payload.(type) {
	case record.StatusWrite:
		refusal, _ := dispatch.PermanentRefusal(cause)
		r.log.Error("outbox status write refused by Dispatch and dropped; the issue's later writes go on",
			"row", row.ID, "issue", row.Issue, "status", refusal.Status, "code", refusal.Code, "error", kept)
		rule, err := ruleFor(value.Kind)
		if err != nil {
			return fmt.Errorf("outbox row %d: %w", row.ID, err)
		}
		follow, told, dropsFence = rule.refused(r, row, value, cause)
	case record.MessagePost:
		notice, err := record.NewOutboxRow(row.Issue, messagePostRefused(row.Issue, value, cause), r.now().UTC())
		if err != nil {
			return err
		}
		follow = func(ctx context.Context, tx pgx.Tx) error { return r.records.Enqueue(ctx, tx, notice) }
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
		// change of a claim queues a fresh publication of the live set (roleHoldersChanged, the
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
	}
	finish := func(ctx context.Context, write func(context.Context, pgx.Tx) error) error {
		return store.Transact(ctx, r.pool, write)
	}
	if dropsFence {
		finish = r.settling
	}
	if err := finish(ctx, func(ctx context.Context, tx pgx.Tx) error {
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
// every kind but the hand-over makes: the hand-over has a fence to drop and nothing to report,
// and each of the others leaves the architect to settle what the issue holds now
// (statusWriteRefused). Which of the row's two writes stand is known only to the run that made
// them, so the status executor carries that back on the failure it ends with
// (statusWriteFailure); a cause carrying none ended the run before either write — the issue read
// that opens it — and leaves both still to be made. The notice drops no fence, so its finish
// takes no fact lock (finishRefused).
func statusRefusalTold(r *outbox, row record.OutboxRow, payload record.StatusWrite, cause error) (func(context.Context, pgx.Tx) error, func(), bool) {
	var failure *statusWriteFailure
	var wrote statusWrites
	if errors.As(cause, &failure) {
		wrote = failure.wrote
	}
	notice, err := record.NewOutboxRow(row.Issue, statusWriteRefused(row.Issue, payload, cause, wrote), r.now().UTC())
	if err != nil {
		return func(context.Context, pgx.Tx) error { return err }, nil, false
	}
	follow := func(ctx context.Context, tx pgx.Tx) error { return r.records.Enqueue(ctx, tx, notice) }
	told := func() {
		r.log.Error("outbox status write refused by Dispatch; its architect is told what the issue holds now",
			"row", row.ID, "issue", row.Issue, "status", payload.Status,
			"status_set", wrote.status, "reason_posted", wrote.reason, "error", storetext.FarSideError(cause.Error()))
	}
	return follow, told, false
}

// retry returns row to the outbox, due again after its backoff, or after the wait GitHub asks for
// when cause carries GitHub's rate limit and asks for longer, or at the moment cause names when it
// is a wait that knows when it ends (wait.Until) and that moment is still ahead: a lingering
// tree's close waiting for a member's production check names the check's bound, and the row runs
// again then rather than climbing its backoff for days. A named moment already past is the backoff's.
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
	default:
		return false
	}
}

// routineWait is a wait nobody has to act on for Legion (routineWaiting): a Dispatch claim release
// waiting on the claim's holder or on a contended answer (claimHeldWait, claimContendedWait),
// which the hour claimReleaseWait caps rather than an operator, and a child's architect's first
// start waiting for its tree to run again (childArchitectFirstStart), which a person's move of the
// tree or of the child ends rather than an operator, and then for the branch row a re-admission
// queued for the child (startRoot), whose own failures that row logs, a delivery row waiting for a
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
// handler ran. For a kind whose route a Dispatch may not serve yet (waitsForTheRoute) that answer
// is a wait, not a failure, so the daemon and Dispatch deploy in either order: the row runs again
// on its backoff and is logged as a wait is (settle), INFO at its first run, DEBUG after, and WARN
// from waitWarnAttempts on, about two minutes after the first run, once a minute at the backoff's
// cap. The wait wraps nothing of Dispatch's refusal, as the claim release's waits do
// (claimHeldWait), so no reading of permanentRefusal can end the row on it. Every other answer,
// and that answer for every other kind, is decided as its executor and permanentRefusal decide it.
func (r *outbox) execute(ctx context.Context, row record.OutboxRow) error {
	err := r.run(ctx, row)
	if waitsForTheRoute(row.Kind) && dispatch.NoRoute(err) {
		return wait.Errorf("outbox row %d (%s) of %s waits for a Dispatch that serves the route: %v", row.ID, row.Kind, row.Issue, err)
	}
	return err
}

// waitsForTheRoute is whether a write of kind waits for a Dispatch that has no route for it: the
// records of the record lane (a step's start and end, a retro's rating and a run's plan) and the
// role holders' publication, whose routes came after the Dispatch a daemon may meet. Every other
// kind's route stands on every Dispatch it is sent to, so that answer there means a dispatch_url
// that reaches no such Dispatch, and it is the refusal it says: a status write, a message post and
// a claim release end on it with their follow-ups (dispatch.PermanentRefusal), and a status write
// left waiting would hold every later status of its issue behind it; the gated steps' and the
// design gates' publications retry it as a failure.
func waitsForTheRoute(kind record.OutboxKind) bool {
	return record.LaneOf(kind) == record.LaneRecord || kind == record.OutboxKindRoleHoldersPublish
}

// run is the executor of row's kind.
func (r *outbox) run(ctx context.Context, row record.OutboxRow) error {
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		return fmt.Errorf("decode outbox row %d: %w", row.ID, err)
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
		// a lingering tree — the decisions it named are settled here (settleKept), reading from the
		// claim what became of the task: the run itself is over, and what it did is on the claim.
		return r.settleKept(ctx, row, value, false)
	case record.GateSeed:
		return r.seedGate(ctx, row, value)
	case record.LingerClose:
		return r.linger(ctx, row, value)
	case record.IssueSuspend:
		return r.suspendIssue(ctx, row, value)
	case record.IssueWorkspaceCheck:
		return r.checkIssueWorkspace(ctx, row, value)
	case record.IssueRelease:
		return r.releaseIssue(ctx, row, value)
	case record.WorkspaceRemove:
		return r.removeWorkspace(ctx, row, value)
	case record.RoleMessage:
		return r.roleMessage(ctx, row, value)
	case record.GatedStepsPublish:
		return r.publishGatedSteps(ctx, value)
	case record.DesignGatesPublish:
		return r.publishDesignGates(ctx, value)
	case record.RoleHoldersPublish:
		return r.publishRoleHolders(ctx, value)
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
	case record.HookPrompt:
		return r.hookPrompt(ctx, row, value)
	case record.UpdateAsk:
		return r.updateAsk(ctx, row, value)
	case record.MergeQueueRead:
		return r.mergeQueueRead(ctx, row, value)
	case record.MergeQueueRemoval:
		return r.mergeQueueRemoval(ctx, row, value)
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

func (r *outbox) status(ctx context.Context, row record.OutboxRow, payload record.StatusWrite) error {
	if r.dispatch == nil {
		return errors.New("dispatch status executor has no Dispatch client")
	}
	rule, err := ruleFor(payload.Kind)
	if err != nil {
		return fmt.Errorf("outbox row %d: %w", row.ID, err)
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
	// superseded and the row finishes having written nothing. A kind with something of its own
	// to settle when that happens — a hand-over's fence — has just settled it above, which is
	// why the shared answer is taken after the rule rather than before it.
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
	// making the write that was always going to land.
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
// that older number would be refused almost every time.
//
// ISSUE_CHANGED is no failure of the write and no permanent refusal (dispatch.PermanentRefusal),
// so nothing is dropped over it. The row reads the issue again and writes again only while that
// fresh read passes the rule its own kind is judged by (stillWanted). A read already at the status
// the row carries is this write's work, done by someone else; a read at any other status is a
// change made after the row's read, which the row never writes over, whether it took the issue
// out of the workflow or moved it along within one.
//
// What this read never does is reach the engine. Dispatch's read says what the issue is now and
// never who wrote it, so the engine would have to take every change it shows for a person's, and
// two of the daemon's own rows exist precisely because a change was not a person's: a session's
// own close or park, which the re-assert undoes, would be taken for a person ending the tree,
// lingering it and freeing its slot. A change Dispatch recorded reaches the engine through its
// own event, with its actor (dispatch://INFRA-714/spec R4); this read decides one thing, whether
// to write.
//
// rule is the row's kind's rule, which its caller resolved once for the row (ruleFor) before
// anything was written. It is resolved there rather than here, at the refusal that first asks
// for it, so a kind nobody gave a rule fails its row before the first write and reaches Dispatch
// with nothing, whichever path brought the row here.
func (r *outbox) writeStatus(ctx context.Context, row record.OutboxRow, payload record.StatusWrite, rule statusRowRule, read dispatch.Issue) error {
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
		_, err := r.dispatch.SetStatusAt(ctx, read, payload.Status)
		if err == nil {
			return nil
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
		if read.Status != payload.ObservedStatus {
			r.log.Info("outbox status write finished without writing: its issue was moved to another status after the row read it",
				"row", row.ID, "issue", row.Issue, "status", payload.Status, "observed", payload.ObservedStatus, "dispatch", read.Status)
			return nil
		}
		wanted, err := rule.stillWanted(ctx, r, row, payload, read)
		if err != nil || !wanted {
			return err
		}
	}
	return fmt.Errorf("set Dispatch issue %s to %s: it changed under each of the %d reads this attempt made",
		row.Issue, payload.Status, statusWriteReads)
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
	// for them the ordinary rule reads backwards: a handover of either sort brings an issue into
	// the workflow, a re-assert undoes a session's own move out of it, and a close is the move out. Asking
	// record.OutOfWorkflow of any of them would finish it without writing and log that its issue
	// had left a workflow it never entered, or had left one this very row exists to put it back
	// in.
	stillWanted(ctx context.Context, r *outbox, row record.OutboxRow, payload record.StatusWrite, read dispatch.Issue) (bool, error)
	// refused is what this kind leaves behind when Dispatch's refusal of the write is permanent
	// and the row is dropped (finishRefused): follow runs in the finishing transaction, handed
	// that transaction's ctx, told logs once that transaction commits, and nil for either says
	// this kind leaves nothing behind. dropsFence says follow deletes a hand-over's fence, an
	// input of the end of a merge's run, so the finishing transaction takes the lock every fact
	// applies under and settles as a fact's does (settling); a kind that answers false finishes
	// in a plain transaction, behind no fact. It is here rather than in finishRefused's own
	// branch for the reason the other two are: it is a decision taken by the row's kind, and a
	// kind whose clean-up nobody wrote would otherwise be dropped as an ordinary row leaves
	// nothing, in silence.
	refused(r *outbox, row record.OutboxRow, payload record.StatusWrite, cause error) (follow func(context.Context, pgx.Tx) error, told func(), dropsFence bool)
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
	case record.HandOverStatusRow:
		return handOverRule{}, nil
	case record.CloseStatusRow:
		return closeRule{}, nil
	default:
		return nil, fmt.Errorf("status row kind %q has no rule", kind)
	}
}

// statusMoved says whether somebody moved the issue since the row read the status it observed:
// Dispatch shows neither that status nor the one the row carries, so the row's write is
// superseded by a change made after it was queued.
func statusMoved(payload record.StatusWrite, issue dispatch.Issue) bool {
	return issue.Status != payload.ObservedStatus && issue.Status != payload.Status
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
func (ordinaryRule) refused(r *outbox, row record.OutboxRow, payload record.StatusWrite, cause error) (func(context.Context, pgx.Tx) error, func(), bool) {
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
func (personHandoverRule) refused(r *outbox, row record.OutboxRow, payload record.StatusWrite, cause error) (func(context.Context, pgx.Tx) error, func(), bool) {
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
func (reassertRule) refused(r *outbox, row record.OutboxRow, payload record.StatusWrite, cause error) (func(context.Context, pgx.Tx) error, func(), bool) {
	return statusRefusalTold(r, row, payload, cause)
}

// handOverRule judges a child of a closed tree handed to Legion (record.HandOverStatusRow): the
// one kind whose first attempt makes the Dispatch write itself, since the status and the label
// go over together.
type handOverRule struct{}

// firstAttempt hands the child over, so the write its caller would make is never reached: every
// path of it finishes the row.
func (handOverRule) firstAttempt(ctx context.Context, r *outbox, row record.OutboxRow, payload record.StatusWrite, issue dispatch.Issue) (bool, error) {
	if statusMoved(payload, issue) {
		// A person's move superseded the hand-over, so the fence it left goes with it.
		return false, r.dropHandover(ctx, row.Issue, payload)
	}
	if holds, err := r.handOverHolds(ctx, row, payload); err != nil || !holds {
		return false, err
	}
	if issue.Status == payload.Status && issue.HandedOver {
		return false, r.consumeHandover(ctx, row, payload, issue)
	}
	// Every hand-over write is conditional on the sequence just read (dispatch HandOver), the
	// label added when the child lacks it, so a person's move since that read stands.
	if err := r.dispatch.HandOver(ctx, issue, payload.Status); err != nil {
		return false, fmt.Errorf("hand Dispatch issue %s to Legion as %s: %w", row.Issue, payload.Status, err)
	}
	return false, r.consumeHandover(ctx, row, payload, issue)
}

// stillWanted is unreachable and says so: a hand-over's write is made by its own first attempt,
// which finishes the row whichever path it takes, so no hand-over row ever reaches the retry the
// ISSUE_CHANGED refusal sends an ordinary write to. The interface asks every kind for both
// halves, and the honest answer for this one is that the question cannot arise: a row that got
// here came from a path nobody wrote, which is a defect to see rather than a write to guess at.
func (handOverRule) stillWanted(_ context.Context, _ *outbox, row record.OutboxRow, _ record.StatusWrite, _ dispatch.Issue) (bool, error) {
	return false, fmt.Errorf("outbox row %d: a hand-over's first attempt finishes its row, so it cannot reach the retry's rule", row.ID)
}

// refused drops the fence this hand-over left: the write that would hand the child to Legion
// never lands, so no admission consumes the fence, and one left behind would keep naming the
// child's own tree as its Sandbox's home (store.IssueSandboxHome) for good. The fence is an input
// of the end of a merge's run, so the drop answers dropsFence, and its finish takes the fact lock
// and settles as a fact's does.
func (handOverRule) refused(r *outbox, row record.OutboxRow, payload record.StatusWrite, _ error) (func(context.Context, pgx.Tx) error, func(), bool) {
	follow := func(ctx context.Context, tx pgx.Tx) error {
		return r.records.DeleteHandoverAt(ctx, tx, row.Issue, payload.HandOverTree, payload.HandOverLinger)
	}
	told := func() {
		r.log.Warn("outbox hand-over refused by Dispatch; its fence is dropped and the child is not handed to Legion",
			"row", row.ID, "issue", row.Issue, "tree", payload.HandOverTree, "linger", payload.HandOverLinger)
	}
	return follow, told, true
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

// stillWanted asks the same question after Dispatch refused the write. The caller has already
// required Dispatch to show the status the row observed, so nobody has moved the issue since, and
// what is left is whether the close is still owed. The ordinary rule cannot be asked here — it
// drops a row whose issue has left the workflow, which is every close, so one refusal would leave
// the issue open in Dispatch for good while the tree lingered behind a close nobody ever wrote.
// That is the reason this kind exists.
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
func (closeRule) refused(r *outbox, row record.OutboxRow, payload record.StatusWrite, cause error) (func(context.Context, pgx.Tx) error, func(), bool) {
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
func (r *outbox) statusStillWanted(ctx context.Context, row record.OutboxRow, payload record.StatusWrite, read dispatch.Issue) (bool, error) {
	left := record.OutOfWorkflow(read.Status)
	if !left {
		issue, err := r.recordedIssue(ctx, row.Issue)
		if err != nil {
			return false, fmt.Errorf("judge the status write of %s: %w", row.Issue, err)
		}
		left = issue != nil && !workflow.InWorkflow(*issue)
	}
	if left {
		r.log.Info("outbox status write finished without writing: its issue left the workflow while the write was on its way",
			"row", row.ID, "issue", row.Issue, "status", payload.Status, "dispatch", read.Status)
		return false, nil
	}
	return true, nil
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

// publishRoleHolders writes the session of each live Legion role of the issue onto the issue's
// Dispatch row. The set is read from the claims now, in a transaction of its own, not when the row
// was queued: a claim that ended after the queue is gone from it, and two claim changes of one
// issue never publish a stale set. It is sent whole, empty included, since it replaces what
// Dispatch holds, so a role whose claim ended leaves the set with no delete route.
//
// A Dispatch that has not got the route yet answers its router's 404, which execute makes a wait,
// so the daemon and Dispatch deploy in either order. Every other answer is decided by
// permanentRefusal: the route's own 404 ISSUE_NOT_FOUND and a coded 400 end the row, and a 5xx, a
// codeless 4xx, a handler's 404 NOT_FOUND "not found" and a transport failure are failures retried
// on the row's backoff.
func (r *outbox) publishRoleHolders(ctx context.Context, payload record.RoleHoldersPublish) error {
	if r.dispatch == nil {
		return errors.New("role holders executor has no Dispatch client")
	}
	var live []record.RoleHolder
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		live, err = r.records.LiveRoleHolders(ctx, tx, payload.Issue)
		return err
	}); err != nil {
		return fmt.Errorf("read the live role holders of %s: %w", payload.Issue, err)
	}
	holders := make([]dispatch.RoleHolder, len(live))
	for i, holder := range live {
		holders[i] = dispatch.RoleHolder{Role: string(holder.Role), Session: holder.Session}
	}
	if err := r.dispatch.PutRoleHolders(ctx, payload.Issue, holders); err != nil {
		return fmt.Errorf("publish the role holders of %s: %w", payload.Issue, err)
	}
	return nil
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

// consumeHandover applies the hand-over to the record once Dispatch shows it: the row's hand-over
// write just set it, or Dispatch already showed the todo and label (which a write would make no
// event for). It does not wait for the write's event: a restart between the write and that event's
// publication lets the boot read record the event's sequence, and the live event then stops at the
// sequence fence. The fact is idempotent (admit's consumeHandover reads the fence it deletes), so
// the event, or this row's retry after a failure here, changes nothing more.
//
// read is the issue as the status executor read it from Dispatch before any of this. The fact
// carries its resolution, so the re-admission lands on the repository Dispatch resolves the issue
// to now (admit's consumeHandover): no event and no PATCH of this door says the repository
// changed, so without it the new generation would run on the repository the record holds until
// the issue's next event or the next boot. A read that answered no repository carries none, and
// corrects nothing. The fact carries the read's sequence too, so a record that took a later event
// meanwhile, a person's change of the repository among them, is not corrected back to this read.
// The fact is prepared first (prepareHandover), outside the fact lock, so a resolution that moves
// the issue carries GitHub's read of the repository it moves onto, which the correction writes
// and nothing the daemon kept.
func (r *outbox) consumeHandover(ctx context.Context, row record.OutboxRow, payload record.StatusWrite, read dispatch.Issue) error {
	fact := intake.Handover{Issue: row.Issue, Tree: payload.HandOverTree, Generation: payload.HandOverLinger,
		ResolvedRepo: read.ResolvedRepo, Seq: read.LastSeq}
	if r.prepareHandover != nil {
		prepared, err := r.prepareHandover(ctx, fact)
		if err != nil {
			return fmt.Errorf("prepare the hand-over of %s: %w", row.Issue, err)
		}
		fact = prepared
	}
	_, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("handover:%s:%d", row.Issue, row.ID), fact, r.handlers...)
	return err
}

// handOverHolds is an early check only; the durable fence is enforced again by intake when the
// PATCH's daemon event arrives. Its read confirms this row still owns the child and the linger it
// names, avoiding a needless write once a root reopen already removed that fence. The read writes
// nothing a fact reads, so it runs in a plain transaction and waits behind no fact. A fence a
// re-admission cancelled is dropped, and the fence is an input of the end of a merge's run, so
// the drop runs in a transaction of its own that takes the lock every fact applies under and
// settles as a fact does (settling). DeleteHandoverAt matches the tree and the linger generation,
// not the cancel, so the drop reads the fence again under the lock and deletes it only while it
// is still cancelled: a fact that put the fence again since the read committed before the lock
// was taken, and its fence stands; one that puts it later waits for the drop and stands after it.
func (r *outbox) handOverHolds(ctx context.Context, row record.OutboxRow, payload record.StatusWrite) (bool, error) {
	var issue, root *record.Issue
	var handover *record.Handover
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(ctx, tx, row.Issue)
		if err != nil || issue == nil {
			return err
		}
		handover, err = r.records.Handover(ctx, tx, row.Issue)
		if err != nil || handover == nil || handover.Cancelled {
			return err
		}
		root, err = r.records.Issue(ctx, tx, handover.Tree)
		return err
	}); err != nil {
		return false, fmt.Errorf("read hand-over of %s: %w", row.Issue, err)
	}
	if handover != nil && handover.Cancelled {
		if err := r.settling(ctx, func(ctx context.Context, tx pgx.Tx) error {
			fence, err := r.records.Handover(ctx, tx, row.Issue)
			if err != nil || fence == nil || !fence.Cancelled {
				return err
			}
			return r.records.DeleteHandoverAt(ctx, tx, row.Issue, payload.HandOverTree, payload.HandOverLinger)
		}); err != nil {
			return false, fmt.Errorf("drop the cancelled hand-over of %s: %w", row.Issue, err)
		}
	}
	if issue != nil && handover != nil && !handover.Cancelled && root != nil && issue.Tree == handover.Tree && handover.Tree == payload.HandOverTree &&
		handover.Generation == payload.HandOverLinger && root.LingersAt(handover.Generation) {
		return true, nil
	}
	r.log.Info("outbox hand-over no longer holds; finished without writing", "row", row.ID, "issue", row.Issue,
		"tree", payload.HandOverTree, "linger", payload.HandOverLinger)
	return false, nil
}

// dropHandover deletes the fence of a hand-over a person's move superseded, but only this row's
// own: the tree and linger generation the row names. A newer linger's fence on the same child is
// its own row's to settle. The fence is an input of the end of a merge's run, so the delete
// settles as a fact does (settling).
func (r *outbox) dropHandover(ctx context.Context, issue string, payload record.StatusWrite) error {
	return r.settling(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.DeleteHandoverAt(ctx, tx, issue, payload.HandOverTree, payload.HandOverLinger)
	})
}

// settling runs write, a write of the outbox that changes an input of the end of a step's run — a
// hand-over's fence — so that it settles as a fact does: in one transaction (store.Transact), which
// takes the lock every fact applies under (intake.SerializeFacts), runs write on a ctx carrying
// what the settle reads (intake.OpenSettlers over the outbox's handlers) and the commit hooks its
// writes register, and settles the runs it ended before the commit (workflow's settleRuns). Every
// write in it takes that ctx, so the store notes the issues those writes touch. Only a transaction
// that drops a fence runs here: the lock puts it behind every fact, and a write that changes no
// input of the end has neither a fact to wait for nor a run to settle, so it runs in a plain
// store.Transact.
func (r *outbox) settling(ctx context.Context, write func(ctx context.Context, tx pgx.Tx) error) error {
	return store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		ctx, settle := intake.OpenSettlers(ctx, r.handlers...)
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return fmt.Errorf("serialize with every fact: %w", err)
		}
		if err := write(ctx, tx); err != nil {
			return err
		}
		return settle(ctx, tx)
	})
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
	slugs, err := r.dispatch.ArtifactSlugs(ctx, row.Issue)
	if err == nil && slices.Contains(slugs, artifact.Name) {
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
// and every other notice to the architect that owns its issue. Every notice kind is for an
// architect, and no claim subscribes to an issue topic (packages/pi-legion/src/claim-session.ts),
// so no issue topic carries one. The architect, the earlier notices it waits behind, and whether
// its tree lingers are read from one snapshot of the tree (notice_routing.go). A notice waits
// behind an earlier notice of its tree that is held for the same architect, so each architect is
// told in the order the notices were written: on its backoff (errNoticeWaits), or, when the
// earlier row is itself due at a moment still ahead, at that moment and once (wait.Until). A role
// topic with no live holder refuses the publish (notify.ErrNoHolder): while the architect's claim
// can hold its role again — relaunching, a root the operator suspended and can resume, or the
// architect of a catch-up's own issue suspended while that issue is in the workflow, which its
// next start resumes — the row is held for it; once nobody will hold that role for this notice —
// the claim has failed or retired, or its tree lingers or has closed — the row finishes
// undelivered with one log line. So does a row whose architect cannot be resolved from the record
// (errNoticeUnroutable), which holds back no later notice either, and, before any publish, a
// changes request the person's approval followed while it waited (changesSuperseded), a row whose
// architect stopped with its finished tree (stoppedWithTree), and a child's catch-up while the
// child is out of the workflow by its status or its phase (record.InWorkflow), whose next launch
// writes a fresh one. A person's spec decision in either of the two cases a notice finishes
// undelivered because nobody will hold the role (its architect stopped with its finished tree, or
// will not hold its role again) goes to the controller instead (parkDecision).
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
// catch-up a newer ready dropped, or a changes request a clear of its generation dropped,
// errNoticeRowGone) is found gone in the tree's snapshot and finishes without a publish, and so
// does a catch-up a newer one superseded (catchUpSuperseded, read against the
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
func (r *outbox) notice(ctx context.Context, row record.OutboxRow, payload record.Notice) error {
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
// How long the wake waited is the row's own record, not a clock and not its next_at: the engine
// carried both the moment it reserved and the moment it reserved from, so PacedAt less PacedFrom
// is what the pacing held this wake for, whatever a retry since has written into next_at
// (record.Notice.PacedAt, PacedFrom, the store's RetryOutbox). The pair is one clock's: the row's
// created_at is the database's now() at the start of the fact's transaction, so a wake read
// against it would be the engine's clock less the database's, and a row the burst let straight
// out — the first wake of a wave, reserved for the instant it was composed — would report the
// transaction's own duration as a wait. It waited nothing and says so, as supervise's line does;
// reading the wait from the runner's clock instead would call every such row held, by however
// long the lane took to pick it up.
//
// The pace the line names is the one the daemon holds its machines to (supervise.WakePace, set in
// openSupervision): the rate and the burst alone, never its Reserve — the reservation that
// decided when this row was due is the workflow engine's (workflow.Config.Reserve), taken from
// the same limiter. A test that builds the runner by hand over a supervisor with no pace reads
// the pacing off, which is that line's own reading of it.
func (r *outbox) wakeRan(row record.OutboxRow, notice record.Notice) {
	architect, _ := claim.NewToken(r.project, row.Issue, claim.RoleArchitect)
	pace := r.supervisor.deps.WakePace
	fields := append([]any{"claim", string(architect)},
		supervise.WakeRanFields(supervise.WakeCatchUp, true, notice.PacedAt.Sub(notice.PacedFrom), pace.Rate, pace.Burst)...)
	r.log.Info(supervise.WakeRanMessage, fields...)
}

// deliverNotice is the notice executor's own work: routing the notice, publishing it, and settling
// what a catch-up that will never publish named. Its endings are notice's to read.
//
// A changes request the person's approval followed finishes first, with nothing published and
// one line that names the approved version (stillOwed's changesSuperseded): the workflow
// asked when it queued the notice (workflow's tellChanges), but a row waits here while its
// architect's pod is released or relaunching, and a copy a listener's report queued again
// (reholdNotice) comes through here too, so the delivery asks again on every attempt. It comes
// before the routing because nothing after it can make the request the person's word again: a
// released pod is not woken for it (wakeForDelivery), a later notice of the same architect, the
// approval's own design-approved notice among them, is not held behind it (earlierNoticeFor),
// and a tree whose architect stopped with it would not relay it to the controller either
// (parkDecision also asks workflow.NoticeSuperseded). One window is left: an approval that commits
// between that read and the publish, which reaches the architect after the request it answered.
//
// A catch-up is published as the issues its architect owns stand at this delivery, not as they
// stood when the row was queued: the delivery's one read of the tree composes it again for the
// row's own issue (readNoticeTree, workflow.TreeCatchUp: a root's whole tree, a child's own issue
// and every issue under it), and its record half (record.CatchUpTree) is replaced whole with what
// that read states, as the notice's Reason is, so a gate registered or an issue added while the
// row waited reaches the architect with the catch-up (dispatch://INFRA-2819). The Reason keeps in
// front of it the sentence of what the architect's last process ended that the row was queued
// with (record.CatchUp.Relaunch), which stays the row's. Decided is written again in that read
// from the decisions the row claimed, each asked of the gates as they stand then
// (workflow.SupersedingApproval), so a decision that stopped being the person's word while the row
// waited is not named. The order is the fence's: whether a newer catch-up supersedes this one
// (catchUpSuperseded) is read on the payload as the row holds it, before anything is replaced,
// because the fence compares the row's Generation and Launch with the row's issue and its
// architect's claim as they are now, and a payload composed from the issue now would compare the
// issue with itself and never be superseded. So Generation and Launch stay the row's. So does
// Kept: the row's own queueing claimed those decisions for this row (workflow.EnqueueNaming), and
// the settling that follows the publish (settleTold, supersededKept) is keyed to what the row
// claimed, not to what a read at delivery would name.
func (r *outbox) deliverNotice(ctx context.Context, row record.OutboxRow, payload record.Notice) error {
	if r.notices == nil {
		return errors.New("notice executor has no Envoy publisher")
	}
	if r.supervisor == nil {
		return errors.New("notice executor has no claim supervisor")
	}
	issue, tree, recomposed, err := r.readNoticeTree(ctx, row, payload)
	if errors.Is(err, errNoticeRowGone) {
		r.log.Info("outbox notice finished without publishing: its row was deleted under its lease", "row", row.ID, "kind", payload.Kind, "issue", row.Issue)
		return nil
	}
	var superseded changesSuperseded
	if errors.As(err, &superseded) {
		r.log.Info("outbox changes request finished without publishing: the person's approval followed it", "row", row.ID, "issue", row.Issue,
			"version", payload.Version, "approved", superseded.approved)
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
	// superseded by the child's architect's later launch, never by its tree root's.
	architect, err := noticeArchitect(tree.project, tree.issues, issue, payload, r.holdsItsRole)
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
			if err := r.supersededKept(ctx, tree.root.Key, row.ID, told); err != nil {
				return err
			}
			r.log.Info("outbox catch-up finished without publishing: a newer catch-up supersedes it", "row", row.ID, "issue", row.Issue,
				"generation", payload.CatchUp.Generation, "launch", payload.CatchUp.Launch)
			return nil
		}
		// A child out of the workflow (record.InWorkflow: by its status, or done, as a parked child
		// stays when a person writes a running status on it without a todo) has its architect
		// suspended or retired with its launch and its generation kept, so nothing supersedes the
		// row. It finishes here, before any publish, since no ready queues a catch-up while the
		// child is out of the workflow (workflow's claimReady, the same rule) and its next launch
		// writes a fresh one. A root's catch-up is not judged by this check: besides the endings
		// every notice has, it ends when a newer one supersedes it (catchUpSuperseded, above), when
		// its architect stopped with its finished tree (stoppedWithTree, below), or when no session
		// holds its role and its claim will not hold it again (noticeAnswered's architectEnded).
		if !claim.IsTreeRoot(issue.Key, issue.Tree) && !record.InWorkflow(issue) {
			r.log.Info("outbox catch-up finished undelivered: its issue is out of the workflow", "row", row.ID, "issue", row.Issue,
				"status", issue.Status, "phase", issue.Phase, "generation", payload.CatchUp.Generation, "launch", payload.CatchUp.Launch)
			return nil
		}
		delivered := *payload.CatchUp
		delivered.CatchUpTree, delivered.Decided = recomposed.tree, recomposed.decided
		payload.CatchUp, payload.Reason = &delivered, recomposed.reason
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
	if held := r.supervisedClaim(architect); stoppedWithTree(tree.root.Lingers(), held) {
		if err := r.parkDecision(ctx, row.Issue, tree.root.Key, payload, key, row.ID); err != nil {
			return err
		}
		r.log.Info("outbox notice finished undelivered: its architect stopped with its finished tree",
			"row", row.ID, "kind", payload.Kind, "issue", row.Issue, "architect", architect, "state", held.State, "stopping", held.Stopping)
		return nil
	}
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
	if earlier, due := earlierNoticeFor(tree, architect, r.holdsItsRole); earlier != 0 {
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
	return r.noticeAnswered(ctx, row, payload, tree, architect, published)
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
//  5. the issue leaving the workflow, a lingering tree included: a released claim is owed only
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

// hookPrompt publishes the words of a hook-prompt row (record.HookPrompt) to the Envoy topic of
// the role the deployment's hook named, the claim of that role on the row's issue, and to nothing
// else. The row is the daemon's own facts of the firing — the hook, the move back and the counts —
// and then the deployment's own words for the round that move back opened, unchanged
// (record.HookPrompt.Message). It rides the role-message lane (record.LaneRoleMessage), so it
// waits behind no other row of its issue and reaches the role while that round is the round the
// role works. It stops nothing it reaches: the session takes it at its next model turn, and the
// role's turn, jobs and process run on.
//
// The row publishes with keep (publishKeptToRole, notify.Publisher.PublishKept): a topic with no
// live holder is answered kept, the listener holding the prompt on the role's kept lane for one
// hour for the next session that holds the role live, and the row ends with one log line, the
// listener being the prompt's keeper. A released role's woken session is that next holder: the
// row's first attempt wakes the pod (wakeForDelivery) and waits on its backoff, or, while the wake
// pacing holds that resume, for its turn, reading the claim again at most parkedWakeBound apart
// (parkedWait), and the attempt after the resume has launched publishes before the woken session
// has claimed the role. A listener before #4079 keeps nothing for a claim whose holder is not
// live; publishKeptToRole says what each answer does to the row.
//
// The hook wrote its words for this round, and nothing repeats them: the move back they were
// written for has been made, and no later task of the daemon's carries them, so the kept copy is
// the prompt's one keeper. An unheld topic is besides the ordinary state for a role an issue has
// never had — the deployment's default hook writes a row for the reviewer, and the work of an
// issue may never have reached one — so a reviewer started inside the hour can be handed an
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
	// A role whose pod was released is not "no live session holds the role": its session is
	// intact and its pod is one wake away, so the row wakes it and waits on its backoff, and the
	// row's first attempt after that resume has launched publishes (wakeForDelivery). A launch the
	// wake pacing holds is waited for until it runs, the claim read again at its turn or
	// parkedWakeBound on, whichever comes first (parkedWait). The daemon's own wake brings the pod
	// back whatever the listener does with the publish, a listener before #4079 included; the
	// listener's own report of the kept publish meets the role's open resume, and the pod starts
	// once.
	if waits, turn, err := r.wakeForDelivery(ctx, token, "the "+payload.Hook+" hook's prompt for "+row.Issue, row.HeldByPause); err != nil {
		return err
	} else if !turn.IsZero() {
		return r.parkedWait(turn, fmt.Sprintf("the %s hook's prompt for %s waits for its %s's launch, which the wake pacing holds until %s",
			payload.Hook, row.Issue, payload.Role, turn.UTC().Format(time.RFC3339)))
	} else if waits {
		return wait.Errorf("the %s hook's prompt for %s waits for its %s's released pod to come back", payload.Hook, row.Issue, payload.Role)
	}
	if err := r.publishKeptToRole(ctx, row, notify.RoleTopicPrefix+string(token), payload.Message(row.Issue),
		"row", row.ID, "issue", row.Issue, "hook", payload.Hook, "role", payload.Role, "count", payload.Count); err != nil {
		return fmt.Errorf("publish the %s hook's prompt for %s to its %s: %w", payload.Hook, row.Issue, payload.Role, err)
	}
	return nil
}

// updateAsk publishes an update round's ask (record.UpdateAsk) to the Envoy topic of the role the
// row names, the claim of that role on the row's issue, and to nothing else. The text is the
// sweep's, built for the round when it opened (supervise.UpdateTask), and it reaches a worker and
// the issue's architect alike: every role whose process runs in the pod loses that process when
// the round replaces the pod. It rides the role-message lane (record.LaneRoleMessage), so it
// waits behind no architect notice of its issue.
//
// It wakes nothing, where hookPrompt, mergeQueueRemoval and mergeForwardMessage call
// wakeForDelivery first. The round does not ask a claim the daemon suspended (its own rule, a
// root's exit, an idle release, or an unknown source), and does not wait for it
// (dispatch://INFRA-4109 R1, the sweep's roundAsks). A claim a project's pause stopped is the one
// exception while the pause still keeps it (the sweep's podHolds.owes, roundClock.passesOver): the
// pause suspends it as released, and the round waits for it and the sweep asks it once its agent
// is ready.
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
// (notify.RoleTopicPrefix and the claim's token), under the row's own key, so a retried row is
// one delivery. It is updateAsk's publish, with keep: false; hookPrompt publishes with keep
// through publishKeptToRole, which also says how each executor that publishes to a role's topic
// answers the publisher.
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

// publishKeptToRole publishes message to topic, the Envoy topic of one role's claim
// (notify.RoleTopicPrefix and the claim's token), under the row's own key, and asks the listener
// to keep it for the role's next live holder, through the gate every publish of the outbox goes
// through (publishKept). A retry of the row while its copy is still on the role's kept lane
// stores nothing new; notify.HTTPPublisher.PublishKept gives the listener's answers and what a
// repeat after the copy's hand-over does. It is hookPrompt's publish; hookPrompt decides what
// comes before it, and wraps the error it returns. What each answer does to the row:
//
//   - a live holder took the message: the row ends, with nothing logged;
//   - the listener kept the message on the role's kept lane for one hour: the row ends, logging
//     "outbox hook prompt finished: the listener keeps it for the role's next holder" at Info with
//     fields, the listener being the message's keeper from there;
//   - no live session holds the role and the listener kept nothing (notify.ErrNoHolder), or the
//     role's claim records a session it does not hold and nothing was published (the gate's
//     notify.ErrNoHolder): the row ends undelivered, logging at Info with fields
//     "outbox hook prompt finished undelivered: no live session holds the role";
//   - a holder the listener calls unresponsive (notify.ErrHolderUnresponsive), and every other
//     failure: the error is returned as the publisher gave it, and the row keeps its retry.
//
// The row is not held for a holder as an architect's notice is held, so the kept copy is the
// message's one keeper, and it can reach a later session than the one it was written for: for a
// role nobody claims, the listener hands it to the first session that claims the role inside the
// hour, which can be a later round's. That is intended — the role's topic names the issue's role,
// not one session of it — and the hour bounds it: the listener drops a copy no live holder took
// inside its hour (roleKeepWindow, packages/envoy/cmd/listener/role_keep.go), so no session is
// handed one older than that, and a forwarded copy carries when it was kept (Envoy's
// contracts.Envelope.KeptAt) beside what the message names of its round, so its reader judges
// which round it was for. An unheld topic of a role that never claims it — a session that has not
// registered, or one that failed — holds no row either: the row ends, and the kept copy is
// dropped once its hour ends.
//
// Five other executors publish to a role's topic, each with its own rules for what the publisher
// answers: updateAsk, through publishToRole, ends the row for a topic with no holder and retries
// every other failure; deliverNotice, an architect's notice, holds the row for
// a topic with no holder while the owning architect can hold its role again (noticeAnswered);
// roleMessage ends the row for a topic with no holder, waits for a holder the listener calls
// unresponsive (notify.ErrHolderUnresponsive), and ends the row for a holder that refuses the
// delivery mode (notify.ErrRefusedByHolder) or a forward the listener's own NATS grant denies
// (notify.ErrPublishDenied); mergeQueueRemoval, the merger's message of a removal from the
// merge queue, holds the row for a topic with no holder and for an unresponsive holder while the
// merger's claim has neither failed nor retired, and retries every other failure
// (merge_queue_removal.go); and mergeForwardMessage, a message of a merge forward, holds its row
// the same way while the role's claim has neither failed nor retired and the merge forward still
// stands at the row's head (merge_forward.go). Each of those five publishes with keep: false:
// deliverNotice, mergeQueueRemoval and mergeForwardMessage hold their own row, and a kept copy
// beside it would be a second keeper of one message; roleMessage's message is stale by the time a
// role nobody holds is claimed; and updateAsk's round ends at its bound whether or not the role
// answers. So whoever changes the rule for a topic with no holder must read all six executors, not
// this function alone.
func (r *outbox) publishKeptToRole(ctx context.Context, row record.OutboxRow, topic, message string, fields ...any) error {
	keeps, err := r.publishKept(ctx, topic, message, message, record.OutboxKey(row.ID))
	switch {
	case errors.Is(err, notify.ErrNoHolder):
		r.log.Info("outbox hook prompt finished undelivered: no live session holds the role", fields...)
		return nil
	case err == nil && keeps:
		r.log.Info("outbox hook prompt finished: the listener keeps it for the role's next holder", fields...)
	}
	return err
}

// controllerNotice publishes a controller notice row to the controller topic of the daemon's own
// project, the one its controller subscribes to, and to nothing else: the Notice it is, under the
// row's own key.
//
// A relay of a person's changes request (record.ChangesRequest) is asked first whether its
// publish is still owed (stillOwed): the relay asked when it wrote the row (parkDecision), but the
// row waits here for as long as no session holds the controller's claim, and an approval that
// lands meanwhile would reach the controller as a request to act on. The read asks of the
// decision the row carries (record.RelayParkedDecision). A row deleted under its lease, which is
// what a clear of its generation does to a request the person's approval followed, and a request
// the approval followed each finish with nothing published and one line, the second naming the
// approved version. A request a clear found standing keeps its row while the clear deletes its
// gates, so that row is asked of the gates as they stand when it runs. One window is left, as in
// deliverNotice: an approval that commits between that read and the publish, which reaches the
// controller after the request it answered. Every other notice, a parked decision of any other
// kind among them, is published as it is, with no read.
func (r *outbox) controllerNotice(ctx context.Context, row record.OutboxRow, payload record.ControllerNotice) error {
	if r.notices == nil {
		return errors.New("controller notice executor has no Envoy publisher")
	}
	if _, asks := record.ChangesRequest(payload); asks {
		read := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
		err := store.TransactWith(ctx, r.pool, read, func(ctx context.Context, tx pgx.Tx) error {
			return r.stillOwed(ctx, tx, row, payload)
		})
		args := []any{"row", row.ID, "issue", row.Issue, "root", payload.Root, "ref", payload.Ref,
			"version", payload.Version}
		var superseded changesSuperseded
		switch {
		case errors.Is(err, errNoticeRowGone):
			r.log.Info("outbox parked changes request finished without publishing: "+
				"its row was deleted under its lease", args...)
			return nil
		case errors.As(err, &superseded):
			r.log.Info("outbox parked changes request finished without publishing: "+
				"the person's approval followed it", append(args, "approved", superseded.approved)...)
			return nil
		case err != nil:
			return fmt.Errorf("read whether the changes request relayed for %s is still the "+
				"person's word: %w", row.Issue, err)
		}
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
	// A close row of the issue's own tree runs through that tree's cleanup reservation. A suspend
	// or tree close of a tree the issue has since left runs through stopInLeftTree; the checks below
	// decide every other row.
	if payload.Op == "tree_close" && payload.Tree == issue.Tree {
		if handled, err := r.reservedTreeClose(ctx, row, payload); handled || err != nil {
			return err
		}
	}
	if (payload.Op == "suspend" || payload.Op == "tree_close") && payload.Tree != issue.Tree {
		return r.stopInLeftTree(ctx, row, payload, issue)
	}
	// A check start (CheckLook) runs after the close: its issue holds no phase, its generation may
	// be a later run's and its tree may linger, so it is fenced on the open check — the instance the
	// row names — and its look (checkLookHolds) where a phase start is fenced on the generation and
	// the phase.
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
	if payload.Tree != issue.Tree {
		return fmt.Errorf("supervise row %d tree %s does not match issue %s tree %s", row.ID, payload.Tree, issue.Key, issue.Tree)
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
		// An issue the workflow has taken out — closed, parked, iceboxed, sent back to triage —
		// has had its Sandbox given back by the departure that took it out, so a start that
		// reaches the runner behind that departure would build the pod again and pick up the
		// claim that departure had just put down. The row returns here, above machine.StartedBy,
		// so it records no start against the claim: the departure's own stop is not superseded by
		// it, and the suspension that follows that stop is not dropped for a start that started
		// nothing. admit.(*Admission).finishFencedStarts deletes an owed start of the same shape
		// on the same predicate, so a row that never reaches a lane and one that does are
		// declined alike.
		//
		// The status read is the issue's own recorded Status and not its DispatchStatus: a close
		// the daemon authored writes Status with the rows that carry it out and leaves
		// DispatchStatus at what the board last showed until Dispatch's echo arrives, while
		// Engine.keepStatus writes DispatchStatus and keeps Status for a status the daemon sets
		// back, which leaves the issue in the workflow and its start a start. It is the issue's own
		// status and not its root's, because a child carried into a tree of its own stands in the
		// workflow while the tree it left lingers, and the tree is what the guard below reads. A
		// production check's look is exempt — it is the one start an issue out of the workflow
		// still runs — which the enclosing payload.CheckLook == 0 gives it.
		if payload.CheckLook == 0 {
			if record.OutOfWorkflow(issue.Status) {
				r.log.Info("outbox start of an issue that has left the workflow; finished without acting", "row", row.ID,
					"issue", issue.Key, "status", issue.Status, "role", payload.Role)
				return nil
			}
			// The role of a stopped task's step starts again only when its task does
			// (workflow.StoppedStepRole): the start that a later version of the plan, or a person's
			// todo on a stopped root, queues for it comes with the hold cleared. Every other start of
			// that role while the hold stands — a promotion's start of a held member's roles after a
			// hand-over or a re-admission, a wake — finishes here, the one place every start row of
			// a role runs, above StartedBy, so it records no start and the claim stays where the
			// stop left it.
			if workflow.StoppedStepRole(issue) == payload.Role {
				r.log.Info("outbox start of the role of a stopped task's step; finished without acting", "row", row.ID,
					"issue", issue.Key, "role", payload.Role, "from", issue.Hold.From)
				return nil
			}
			// A tree that lingers has left the workflow, and linger holds each member where it
			// stood: a start that reaches its claim after the close (queued before it and backing
			// off) finishes without acting and records no start, so the close's suspend still
			// applies. Re-admission starts the member again (admit's startMidPhaseMembers). A
			// check start is the one start a lingering tree runs: a look of a closed issue's
			// production check, which resumes the role that check records
			// (record.ProductionCheck.Role) the linger suspended.
			//
			// One start waits on its backoff rather than finishing: the first start of a child's own
			// architect that finds no claim (childArchitectFirstStart), for as long as its tree
			// lingers or its root waits in the line for the tree to be opened again
			// (record.QueuedRoot), and then until the branch row the re-admission queued for the
			// child has finished. Re-admission brings back a member's own architect only where it
			// holds a claim, so that start finished here is one nothing queues again, and the child
			// would run to its close with no architect of its own.
			first := !found && childArchitectFirstStart(issue, payload)
			root, waitsFor, err := r.startRoot(ctx, issue, first)
			if err != nil {
				return err
			}
			if waitsFor != "" {
				return routineWaiting{err: wait.Errorf("the first start of the own architect of %s waits for %s", issue.Key, waitsFor)}
			}
			if root != nil && root.Lingers() {
				r.log.Info("outbox start of a member of a lingering tree; finished without acting", "row", row.ID, "issue", issue.Key,
					"tree", issue.Tree, "role", payload.Role)
				return nil
			}
		}
		news, finished, err := r.startNewsOf(ctx, row, issue, payload, machine, found, token)
		if err != nil || finished {
			return err
		}
		// A child carried into a tree of its own — a finished root's hand-over, or a person's label
		// on a child its tree left mid-run — may still run a role of its old tree: one in a turn when
		// the old tree's close held its suspension, which that close's row, and the stop the move
		// queued, still stop when they run (stopInLeftTree), and this start can come first. No role of
		// the new tree starts while one runs.
		if err := r.stopElsewhere(ctx, issue); err != nil {
			return err
		}
		// A child of a closed tree re-admitted as a root of its own keeps its roles' claims, which
		// still name the tree it left; each is re-pointed before anything else here, so it binds this
		// tree's lifecycle and launches in this tree's resources, and so does whatever resumes it
		// while this start waits (an operator's resume of a claim the operator suspended). The issue
		// the supervisor keeps beside the machine (volumeLost) never moves with it: a claim's issue is
		// fixed for its life.
		if found && machine.Claim().Tree != issue.Tree {
			if err := machine.Retree(ctx, issue.Tree); err != nil {
				return err
			}
		}
		// An operator's suspension holds its claim until the operator resumes it: the start, and the
		// task it gives, wait on the row's backoff. StartedBy refuses the same in the claim's own
		// decision. A check start does not wait: its look cannot run, and ends (checkLookHeld).
		if found {
			if held := machine.Claim(); held.RuleHolds() {
				if payload.CheckLook > 0 {
					return r.checkLookHeld(ctx, row, payload, *check)
				}
				return fmt.Errorf("start %s of %s: %w", payload.Role, issue.Key, held.RuleHoldError())
			}
		}
		if !found {
			if err := r.provisionWorkspace(ctx, issue); err != nil {
				return err
			}
			machine, _, err = r.supervisor.Create(ctx, supervise.Claim{
				Token: token, Project: r.project, Tree: issue.Tree, Issue: issue.Key, Role: payload.Role, State: supervise.StateQueued,
			}, "")
			if err != nil {
				return fmt.Errorf("create claim %s: %w", token, err)
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
		// it was given that entry. A check start's look
		// and check instance travel the same way, and are what say the completion this task's
		// turn reports is a report on that look rather than a phase completion
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
		// (supervise's Delivery.message). A resume task of an answered root carries no text of its
		// own, and the wait it answers instead (supervise.Delivery.Resume): it is queued with the
		// news its start carries as its whole text, empty where it carries none, and at each send
		// its text is written from the record as it stands then (the daemon's rootTask), with that
		// news rendered after it (narrowedNews). The news the task carries travels the
		// same way (supervise.Delivery.News): the task is the start's own text and the news after
		// it, the round's two sentences after the news where the task names them (renderNews), the
		// send renders that news again from the record (the daemon's designNews), and the write
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
		if payload.Task != "" || payload.Awaiting != nil || payload.News {
			kept, err := keptJSON(payload.Kept)
			if err != nil {
				return err
			}
			resume, err := resumeJSON(payload.Awaiting)
			if err != nil {
				return err
			}
			task = &supervise.RequestDeliver{Claim: token, Task: news.task, Phase: payload.Phase, Generation: payload.Generation,
				Entry: payload.Entry, ResendEntry: payload.ResendEntry, RoundNamed: news.roundNamed,
				CheckLook: supervise.CheckLook{Look: payload.CheckLook, OpenedAt: payload.CheckOpenedAt},
				ID:        fmt.Sprintf("%s%d", outboxDeliveryPrefix, row.ID), Row: row.ID, Kept: kept, Resume: resume,
				News: news.news, NewsOnly: payload.News}
		}
		// The claim remembers the newest start run against it, so a stop written before this one
		// is finished rather than acted on however late it arrives (see "suspend" below). A claim
		// this row created has no older stop to fence, and recording it here rather than only for
		// a claim that already existed keeps one rule instead of a special case.
		if err := machine.StartedBy(ctx, row.ID, task); err != nil {
			return fmt.Errorf("record the start of claim %s: %w", token, err)
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
			// The role owns the issue's phase this start is for: the rule resumes it, unless an
			// operator suspended it meanwhile, when the task's delivery below waits instead. A check
			// start resumes the role the check records the close or the linger suspended, in a Sandbox the
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
			// A production check look's start is the half-open trial of a checking role that
			// failed: it retries the claim with one attempt left of each budget its failure spent,
			// never a fresh budget, so a role that cannot start launches once a look rather than
			// until a whole budget runs out at every look up to the check's bound
			// (supervise.RequestRetry.OneAttemptLeft). A check start never asks for a new session
			// (workflow's startLook); the resets a person or the controller makes, which give the
			// whole budget, are the API's retry and fresh session (supervise.RequestOperatorRetry).
			again := supervise.RequestRetry{Claim: token, FreshSession: payload.FreshSession, OneAttemptLeft: payload.CheckLook > 0}
			if err := machine.Handle(ctx, again); err != nil {
				return fmt.Errorf("retry claim %s: %w", token, err)
			}
		case supervise.StateRetired:
			// A retired claim was closed with its issue (a child closed as done) or with its tree
			// (whose linger also removed the workspace), its agent reported its own exit (a phase
			// worker or a child's own architect, which supervise's exit retires), or an operator
			// stopped it. So the workspace is provisioned, which brings back one a linger removed,
			// before the kept session relaunches (a new one once supervise's Machine.forgetSession
			// has dropped it).
			if err := r.provisionWorkspace(ctx, issue); err != nil {
				return err
			}
			if err := machine.Handle(ctx, supervise.RequestRetry{Claim: token, FreshSession: payload.FreshSession}); err != nil {
				return fmt.Errorf("relaunch retired claim %s: %w", token, err)
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
		// lived on: a tree's re-admission for a root's architect; for a child's own, the tree's
		// re-admission while the child stands past admitted (admit's startMidPhaseMembers) or the
		// child's next first phase start (a rerun, or a person's move back to todo). The linger
		// suspends every claim of the tree, and a child's leave suspends or closes the child's
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
		// Whether the suspend still acts is workflow.StopActs's rule, which promotion reads too, on
		// the claim claimToStop answered. Its LastStartRow is the newest start's even where that is the
		// published view: the start rows that write it (supervise.Machine.StartedBy) run on this row's
		// lane one row at a time, so none is in flight while this row runs. A left pod's close's
		// suspend is also held by the turn fence below.
		if last := held.LastStartRow; !workflow.StopActs(row.ID, payload, last, nil) {
			r.log.Info("outbox "+string(payload.Op)+" superseded by a newer start; finished without acting",
				"row", row.ID, "issue", issue.Key, "role", payload.Role, "phase", issue.Phase, "start-row", last)
			return nil
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
		// A linger, a park, a hand-over, a close and a stopped task all stop a claim through this
		// one body, and each takes a released claim over: the machine re-stamps the suspension's
		// source and this drops the backstop the release left that role, so no later backstop acts
		// on a claim this suspension now owns. Another role's backstop stays: a worker waiting on a
		// deploy has nothing else to bring it back. The decline of an idle release stays: the issue
		// comes back.
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
		// A child's close as done: the claim is closed as a tree close closes it, retired with its
		// session kept, since the issue is finished and its next run, should a person set it todo
		// again, resumes that session on a pod and a disk of its own (a new one once supervise's
		// Machine.forgetSession has dropped it). It is fenced as a suspend is, not as a tree
		// close: no linger is involved, and a newer start means a person set the child todo again,
		// whose run the close must not end.
		if !found {
			return r.stopWithoutMachine(ctx, row, token)
		}
		if last := machine.View().Claim.LastStartRow; !workflow.StopActs(row.ID, payload, last, nil) {
			r.log.Info("outbox issue close superseded by a newer start; finished without acting",
				"row", row.ID, "issue", issue.Key, "role", payload.Role, "start-row", last)
			return nil
		}
		if err := machine.Handle(ctx, supervise.RequestIssueClose{Claim: token}); err != nil {
			return fmt.Errorf("close the issue of claim %s: %w", token, err)
		}
		// The issue is done, so no idle release of its pod is coming: the decline of one goes
		// with the backstop.
		r.forgetIdleRelease(ctx, row.Issue)
		return nil
	case "tree_close":
		if !found {
			return nil
		}
		// The close belongs to the linger it expired, the root generation it names
		// (workflow.StopActs). Once re-admission ends that linger, a close still backing off (a
		// release the runtime refused) would retire the claim the tree's new run relaunched, and the
		// task it holds with it, even in a later linger of the tree; it finishes without acting.
		root, err := r.root(ctx, issue)
		if err != nil {
			return err
		}
		if !workflow.StopActs(row.ID, payload, machine.View().Claim.LastStartRow, root) {
			r.log.Info("outbox tree close of a linger that has ended; finished without acting", "row", row.ID, "issue", issue.Key,
				"tree", issue.Tree, "role", payload.Role, "linger", payload.Linger)
			return nil
		}
		// A close that reserved, or found reserved, its tree's cleanup ran through reservedTreeClose
		// already; one reaching here closes its claim alone.
		if err := machine.Handle(ctx, supervise.RequestTreeClose{Claim: token}); err != nil {
			return fmt.Errorf("close the tree of claim %s: %w", token, err)
		}
		// The tree is closing, so no idle release of this member's pod is coming.
		r.forgetIdleRelease(ctx, row.Issue)
		return nil
	default:
		return fmt.Errorf("outbox row %d has unknown supervise operation %q", row.ID, payload.Op)
	}
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

// childArchitectFirstStart says whether payload is the plain start of issue's own architect that a
// child's first phase start queues (workflow's childArchitectStart, from gateOpened): the
// architect's role, on an issue that is not its tree's root, naming no phase, carrying no task, and
// queued for more than news. Other rows of that shape — a failed architect's revive (workflow's
// reviveArchitect), a released one's resume (releaseResume) — start a claim that exists, so
// supervise asks this only of a start that found none, which is the one that creates the claim.
//
// A start queued for news alone (record.SuperviseRequest.News) has the same shape, since the
// reconcile queues an architect's with no phase (the daemon's settleNews), and it is no first
// start. It carries news to a claim that already holds its role, and creates none: one that
// reaches no claim finishes (newsOnlyRefusal). The news stays owed on the record, and the
// reconcile the entry sweep queues (queueOwedDesignNews) tells it once a claim holds the role, since
// the start that creates or revives a child's architect (workflow's childArchitectStart) carries no
// news. Read as a first start it would wait on its backoff while the tree lingers, its root waits
// in the line, or the child's branch row is queued, where it should finish.
//
// The first start waits on the row's backoff, where every other start of a member of a tree that
// lingers finishes without acting, for as long as the tree lingers (record.Issue.Lingers) or its
// root waits in the line for the tree to be opened again (record.QueuedRoot), and then for as long
// as a branch row of the child is still queued, which the re-admission queues newer than this start
// (startRoot). Finishing it would leave the child with no architect of its own for the rest of its
// run, because nothing queues the start again: a re-admission brings back a member's own architect
// only where it holds a claim (admit's startMidPhaseMembers), and the child's later phase starts
// revive only a claim that failed (workflow's reviveArchitect). The child's notices would go to the
// architect above it (record.OwningArchitect) to its close, which R2 of dispatch://INFRA-3915 keeps
// only for a child started before that change (plan D9), and this child's first phase start did
// queue the start.
//
// The linger is read as Lingers reads it, a deadline armed, and not as one still running at the
// moment (record.Lingering), since a tree can run again after its deadline: a root past it is
// re-admitted all the same (record.Issue.Readmittable reads Lingers), even before the expiry
// applies, which itself waits for any open production check of the tree (workflow's
// lingerExpired); and a parked root whose expiry closed the tree hands over only its merging
// members, so its later re-admission resumes every other member where it stood. The root waiting
// in the line is read beside it because a re-admission clears the linger at once (admit's
// readmit) while the tree is opened only by the promotion that gives the root its slot.
//
// So the row ends in one of three ways: the tree runs again and the child's branch row finishes,
// and the start creates and spawns the claim as every first start of a child's architect does; the
// child is handed over, adopted into a live tree or taken as a root of its own, each of which moves
// it to a later generation, and supervise's generation fence finishes the start; or the child
// leaves the workflow, and the fence on its own status finishes it. Its one cost is a parked tree
// that is never re-admitted: the row retries at the backoff's 64-second cap for as long as it
// stands, two reads each try (the child and its root), and only a child whose tree left the
// workflow between its first phase start and that start's run ever has such a row.
func childArchitectFirstStart(issue record.Issue, payload record.SuperviseRequest) bool {
	return payload.Role == claim.RoleArchitect && !claim.IsTreeRoot(issue.Key, issue.Tree) && payload.Phase == "" && payload.Task == "" && !payload.News
}

// stopElsewhere suspends, as the daemon and keeping its session, every claim of issue that still
// runs a process in a tree other than the issue's, and is a wait (wait.ErrWaiting) until none
// does. A claim already in the issue's tree is skipped on its view, which only Retree changes, so
// a start waits on no decision a sibling claim has in flight. Once this call has stopped a claim,
// it makes close rows of that claim's former tree due now: its cleanup may have been waiting on
// that claim.
func (r *outbox) stopElsewhere(ctx context.Context, issue record.Issue) error {
	var running []string
	var left []string
	for _, role := range claim.Roles {
		token, err := claim.NewToken(r.project, issue.Key, role)
		if err != nil {
			return fmt.Errorf("derive the %s claim of %s: %w", role, issue.Key, err)
		}
		machine, found := r.supervisor.Machine(token)
		if !found {
			continue
		}
		tree := machine.View().Claim.Tree
		if tree == issue.Tree {
			continue
		}
		still, stopped, err := r.stopCarried(ctx, machine, issue)
		if err != nil {
			return err
		}
		if still != "" {
			running = append(running, still)
		} else if stopped && !slices.Contains(left, tree) {
			left = append(left, tree)
		}
	}
	if len(running) > 0 {
		return wait.Errorf("start of %s in tree %s waits for %s to stop", issue.Key, issue.Tree, strings.Join(running, ", "))
	}
	for _, tree := range left {
		if err := r.wakeTreeCloses(ctx, tree); err != nil {
			return err
		}
	}
	return nil
}

// stopCarried suspends machine's claim, as the daemon and keeping its session, while it runs a
// process in a tree other than issue's, its stop bounded where that tree's close would have ended
// the turn (carriedStopDeadline). It answers the claim as "token:state in tree T" while it still
// runs there; stopped says it ran when this call began and runs nothing when it returns. A
// suspension the claim holds for its turn or its boot is no error: the caller waits on its backoff
// and asks again.
//
// It reads the claim through supervise.Machine.Claim, before the stop and after it: each read
// decides whether the claim still has to stop, which is "a check a decision must not race"
// (supervise.Machine.Claim).
func (r *outbox) stopCarried(ctx context.Context, machine *supervise.Machine, issue record.Issue) (still string, stopped bool, err error) {
	held := machine.Claim()
	token := held.Token
	if held.Tree == issue.Tree || held.RunsNothing() {
		return "", false, nil
	}
	var oldRoot *record.Issue
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		oldRoot, err = r.records.Issue(ctx, tx, held.Tree)
		return err
	}); err != nil {
		return "", false, fmt.Errorf("read the root of tree %s, which claim %s still runs in: %w", held.Tree, token, err)
	}
	err = machine.Handle(ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByDaemon,
		Deadline: carriedStopDeadline(oldRoot, r.now(), r.supervisor.deps.Timeouts.Stop),
		Reason:   fmt.Sprintf("%s moved from tree %s to tree %s", issue.Key, held.Tree, issue.Tree)})
	var refused *supervise.RefusedError
	if err != nil && !errors.Is(err, supervise.ErrSuspendHeld) && !errors.As(err, &refused) {
		return "", false, fmt.Errorf("suspend claim %s, still running in tree %s: %w", token, held.Tree, err)
	}
	if held = machine.Claim(); !held.RunsNothing() {
		return fmt.Sprintf("%s:%s in tree %s", token, held.State, held.Tree), false, nil
	}
	return "", true, nil
}

// stopInLeftTree handles a suspend or tree close that names a tree its issue has left. It stops a
// machine only while the claim still stands in payload.Tree, waits for a stored claim in that tree
// until its machine is available, and otherwise finishes without acting. This row and a new-tree
// start run on the issue's lifecycle lane, so the claim cannot be re-pointed between this check and
// the suspension. A stop this call completes wakes the old tree's close rows; this row releases no
// tree resources itself.
func (r *outbox) stopInLeftTree(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest, issue record.Issue) error {
	token, err := claim.NewToken(r.project, row.Issue, payload.Role)
	if err != nil {
		return fmt.Errorf("derive claim for outbox row %d: %w", row.ID, err)
	}
	machine, found := r.supervisor.Machine(token)
	if !found {
		may, err := r.trees.StoredClaimMayRunIn(ctx, token, payload.Tree)
		if err != nil {
			return err
		}
		if may {
			return wait.Errorf("%s of tree %s, which %s has left for tree %s, waits for the supervising machine of its stored claim %s",
				payload.Op, payload.Tree, issue.Key, issue.Tree, token)
		}
		return nil
	}
	if machine.View().Claim.Tree != payload.Tree {
		return nil
	}
	still, stopped, err := r.stopCarried(ctx, machine, issue)
	if err != nil {
		return err
	}
	if still != "" {
		return wait.Errorf("%s of tree %s, which %s has left for tree %s, waits for %s to stop", payload.Op, payload.Tree,
			issue.Key, issue.Tree, still)
	}
	if !stopped {
		return nil
	}
	return r.wakeTreeCloses(ctx, payload.Tree)
}

// wakeTreeCloses makes every close row of tree on the issues still in it due now (TreeCloses), in
// a transaction of its own, which owns the commit hooks the expedites register (projectRecords,
// store.OnCommit), each moved row's lane woken once it commits, and the hook that logs the rows it
// moved.
func (r *outbox) wakeTreeCloses(ctx context.Context, tree string) error {
	now := r.now()
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		closes, err := r.records.TreeCloses(ctx, tx, tree)
		if err != nil {
			return err
		}
		var woken []int64
		for _, due := range closes {
			moved, err := r.records.ExpediteOutbox(ctx, tx, due, now)
			if err != nil {
				return err
			}
			if moved {
				woken = append(woken, due.ID)
			}
		}
		if len(woken) > 0 {
			store.OnCommit(ctx, func() { r.log.Info("outbox close rows woken", "tree", tree, "rows", woken) })
		}
		return nil
	}); err != nil {
		return fmt.Errorf("wake the close rows of tree %s: %w", tree, err)
	}
	return nil
}

// carriedStopDeadline is when a carried child's role still running in its old tree, under oldRoot,
// is stopped if its turn has not ended: at the old root's linger expiry, where that tree's close
// would have ended it, so a turn the linger leaves room for ends on its own. A root that no longer
// lingers (or is not recorded) has no close left to wait for, so the stop is bounded as the
// operator's is, at stop after now.
func carriedStopDeadline(oldRoot *record.Issue, now time.Time, stop time.Duration) time.Time {
	if oldRoot != nil && oldRoot.LingerUntil != nil {
		return *oldRoot.LingerUntil
	}
	return now.Add(stop)
}

// reservedTreeClose runs a workflow tree close through its tree's durable cleanup reservation, and
// does so before any stale-close fence. A close reserves only while the root still lingers at the
// close's generation. Once it has, a re-admission that ends that linger cannot open the tree's next
// epoch until the cleanup confirms, so every close row of the tree, however stale its own or its
// root's generation has become, still retires its claim and drives the cleanup; were the fences to
// finish those rows without acting, the reservation would never confirm and the re-admitted tree
// would wait forever. The cleanup (store.CleanupReservedTree) waits until every stored claim of the
// tree has retired, then has the runtime release what it holds for the tree and confirms the
// reservation. It reports false, acting on nothing, when the tree holds no unconfirmed
// reservation and this close could not take one; the ordinary fences then decide. A cleanup that
// finished ends the pods of the tree's issues (endTreePods).
func (r *outbox) reservedTreeClose(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest) (bool, error) {
	lifecycle, reserved, err := r.trees.ReserveWorkflowTreeCleanup(ctx, r.project, payload.Tree, payload.Linger)
	if err != nil {
		return true, fmt.Errorf("reserve cleanup of workflow tree %s: %w", payload.Tree, err)
	}
	if !reserved {
		return false, nil
	}
	token, err := claim.NewToken(r.project, row.Issue, payload.Role)
	if err != nil {
		return true, fmt.Errorf("derive claim for outbox row %d: %w", row.ID, err)
	}
	if machine, found := r.supervisor.Machine(token); found {
		if err := machine.Handle(ctx, supervise.RequestTreeClose{Claim: token}); err != nil {
			return true, fmt.Errorf("close the tree of claim %s: %w", token, err)
		}
	}
	r.forgetIdleRelease(ctx, row.Issue)
	if err := r.trees.CleanupReservedTree(ctx, r.project, payload.Tree, lifecycle.Epoch, r.supervisor.deps.Runtime); err != nil {
		return true, err
	}
	r.endTreePods(ctx, row.Issue, payload.Tree)
	return true, nil
}

// endPods runs the end of issues' pods (podEnded), where attach set it.
func (r *outbox) endPods(ctx context.Context, issues ...string) {
	if r.podEnded != nil {
		r.podEnded(ctx, issues)
	}
}

// endTreePods runs the end of the pods of tree's issues (podEnded) once the tree's cleanup released
// what the runtime held for it, which under the Sandbox runtime waits until no Sandbox of the tree
// is left: each issue the record holds in tree, and issue, the close row's own. A read of the
// tree's issues that fails is logged and ends the pod of issue alone, since the cleanup is done and
// a retry of the row would not run it again; the next boot's rename pass writes what this one left.
func (r *outbox) endTreePods(ctx context.Context, issue, tree string) {
	if r.podEnded == nil {
		return
	}
	var members []record.Issue
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		members, err = r.records.TreeIssues(ctx, tx, tree)
		return err
	}); err != nil {
		r.log.Warn("outbox: the issues of a cleaned-up tree could not be read for the end of their pods; the row's own issue's pod is ended alone",
			"tree", tree, "issue", issue, "error", err)
	}
	issues := []string{issue}
	for _, member := range members {
		if member.Key != issue {
			issues = append(issues, member.Key)
		}
	}
	r.podEnded(ctx, issues)
}

// root is issue's tree root as recorded, nil when it is not, read in a transaction of its own
// just before the executor acts on a row.
func (r *outbox) root(ctx context.Context, issue record.Issue) (*record.Issue, error) {
	var root *record.Issue
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		root, err = r.records.Issue(ctx, tx, issue.Tree)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read the tree root of %s: %w", issue.Key, err)
	}
	return root, nil
}

// startRoot is root for a start and, when first asks, what the first start of a child's own
// architect that found no claim (childArchitectFirstStart) still waits for, "" for nothing: the
// tree of root to run again, while root lingers or waits in the line for its tree to be opened
// (record.QueuedRoot, under the design-gate steps the project gates); then the branch of issue,
// while a branch row of it is still in the outbox (record.Store.BranchQueued). The ordering rule
// holds a start behind an older branch row of its issue alone (record's outboxInOrder), and a
// re-admission queues the child's branch row again, newer than this start, which may have waited
// out the whole linger: so the wait on the row keeps the order the child's first phase start
// queued the two in (workflow's gateOpened), and no role of the issue starts before its branch
// exists. It is all read in one read-only repeatable-read transaction, so the rule judges one state
// of the root, its gate and the outbox, once per try and for that start alone: the gate is not read
// of a root that lingers, nor the branch row of a tree that does not run.
func (r *outbox) startRoot(ctx context.Context, issue record.Issue, first bool) (*record.Issue, string, error) {
	var root *record.Issue
	waitsFor := ""
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if root, err = r.records.Issue(ctx, tx, issue.Tree); err != nil || root == nil || !first {
			return err
		}
		waits := root.Lingers()
		if !waits {
			lifecycle, gate, err := record.RootFacts(ctx, r.records, tx, *root, r.gatedSteps)
			if err != nil {
				return err
			}
			waits = record.QueuedRoot(*root, lifecycle, gate)
		}
		if waits {
			waitsFor = fmt.Sprintf("the tree of %s to run again", root.Key)
			return nil
		}
		branch, err := r.records.BranchQueued(ctx, tx, issue.Key)
		if err != nil {
			return err
		}
		if branch {
			waitsFor = fmt.Sprintf("the branch of %s, which a row of the outbox has still to create", issue.Key)
		}
		return nil
	}); err != nil {
		return nil, "", fmt.Errorf("read the tree root of %s: %w", issue.Key, err)
	}
	return root, waitsFor, nil
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

// suspendIssue is the issue_suspend executor, the durable effect of a close, a park, a linger or a
// production check's look: the runtime suspends the issue's Sandbox under its pod lock once the
// store authorizes the close (store.IssueSuspension), waits its pod out and deletes it
// (runtime.IssueSuspender). A pod the runtime deleted, or found gone, once the store authorized
// it, ends the pause's hold of that pod (podDeleted); a pod whose close the store did not refuse
// ends (endPods).
func (r *outbox) suspendIssue(ctx context.Context, row record.OutboxRow, value record.IssueSuspend) error {
	if r.supervisor == nil {
		return errors.New("issue suspension executor has no claim supervisor")
	}
	suspender, ok := r.supervisor.deps.Runtime.(runtime.IssueSuspender)
	if !ok {
		return nil
	}
	closing := store.IssueClose{Issue: row.Issue, Tree: value.Tree, IssueGeneration: value.Generation,
		TreeGeneration: value.TreeGeneration, Row: row.ID, TurnStart: value.TurnStart}
	// ended and authorized are the store's answer under the runtime's pod lock, read by two
	// rules. A close the store refused finishes without acting and may leave the pod standing, so
	// it turns both false. The pause's hold of the pod is the store's own record, so it ends only
	// on a close the store authorized: authorized starts false. The end of the pod also follows a
	// runtime that stops the pod without asking the store, its nil return being the stop: ended
	// starts true.
	ended, authorized := true, false
	if err := suspender.SuspendIssue(ctx, row.Issue, value.Tree, func(ctx context.Context) (bool, error) {
		act, err := r.trees.IssueSuspension(ctx, r.project, closing, record.OutOfWorkflow)
		ended = act
		authorized = act && err == nil
		return act, err
	}); err != nil {
		return err
	}
	if authorized {
		if err := r.podDeleted(ctx, row.Issue, "the issue's close, park, linger or production check deleted its pod"); err != nil {
			return err
		}
	}
	if ended {
		r.endPods(ctx, row.Issue)
	}
	return nil
}

// podDeleted ends the hold a project's pause has on issue's pod once a row has deleted that pod
// (decision 13 of dispatch://INFRA-3759): the issue's suspension (suspendIssue) and a release's
// teardown (releaseIssue). The tree's cleanup ends the holds of its pods in its own transaction
// (store.CleanupReservedTree). The end takes the issue's lock (store.EndPausePodHoldTx), as every
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

// checkIssueWorkspace is the first act of an idle pod's release: it asks the shim of the role the
// release named — a process still running in the pod, because this row runs before the `release`
// rows that stop them — to read whether the workspace every role of the issue shares holds work
// that no branch or tag on origin holds (runtime.WorkspaceWorkReader, the shim's
// workspaceWorkRevset): a commit under a working copy or a local bookmark that no remote branch or
// tag reaches, or a change in the working copy. Only a shim can read it: it runs the `jj` of the
// workspace it already works in, and it dies with its role's process. The daemon keeps no copy of
// the workspace: a pod whose workspace holds such work is not released at all.
//
// A read that finds nothing lets the release go on to its role stops and its teardown, which
// deletes the pod's disk: nothing on it is lost, since all of it is on origin. A read that finds
// work declines the release, and so does a read that fails, since it cannot show that the
// workspace holds nothing (declineRelease). The daemon pushes nothing on a role's behalf.
//
// A check of a release that is over does nothing. One that finds the project paused cancels the
// release as before, and so does one that finds the pod given work since the release's mark — a
// wake, a start, a claim in a turn or a turn after the mark's newest — before any read
// (store.IssueReleaseCheck, ReleaseCausePaused and ReleaseCauseOvertaken).
func (r *outbox) checkIssueWorkspace(ctx context.Context, row record.OutboxRow, payload record.IssueWorkspaceCheck) error {
	if r.supervisor == nil {
		return errors.New("issue workspace check executor has no claim supervisor")
	}
	if _, ok := r.supervisor.deps.Runtime.(runtime.IssueReleaser); !ok {
		// A runtime with no shared issue resources has no pod to release, so nothing to read.
		return nil
	}
	if len(payload.CloseRoles) > 0 {
		return r.checkLeftPodWorkspace(ctx, row, payload)
	}
	decision, err := r.trees.IssueReleaseCheck(ctx, r.project, row.Issue, row.ID)
	if err != nil {
		return err
	}
	switch decision.Act {
	case store.ReleaseOver:
		r.log.Info("outbox release workspace check finished without acting: the pod's release is over",
			"row", row.ID, "issue", row.Issue)
		return nil
	case store.ReleaseCancel:
		// The project is paused (ReleaseCausePaused): the pause deletes no pod, so the release
		// is cancelled before any role stops, whatever reached its roles first. Or the pod was
		// given work since the mark (ReleaseCauseOvertaken): a read beside that work could miss
		// what it makes, so the release is cancelled before the read, and the pass decides the
		// pod again.
		hold, why := releaseHold(decision.Cause)
		return r.cancelRelease(ctx, row.Issue, payload.Tree, hold, why)
	}
	// The read's moment is taken before it starts, so a turn that starts while it runs is a turn
	// after the decline it may record (pods.IdleDecline.Stands).
	readAt := r.now().UTC()
	work, failed := r.readIssueWorkspace(ctx, row.Issue, payload.Role)
	if failed != nil || work.HoldsWork() {
		return r.declineRelease(ctx, row.Issue, payload, readAt, work, failed)
	}
	// The workspace holds none of the work an earlier decline named, so that decline goes: kept,
	// it would hold the pod for work the daemon's own newer read found gone, whatever becomes of
	// this release (store.Store.ForgetIdleDecline).
	if err := r.trees.ForgetIdleDecline(ctx, r.project, row.Issue); err != nil {
		return err
	}
	r.log.Info("outbox: the idle release read the issue's workspace and found nothing that no branch or tag on origin holds; its roles stop next",
		"issue", row.Issue, "role", payload.Role, "head", work.Head)
	return nil
}

// readIssueWorkspace is the workspace check's one read, through role's shim
// (runtime.WorkspaceWorkReader), and the error of a read that could not be made: a runtime that
// cannot read a workspace is one.
func (r *outbox) readIssueWorkspace(ctx context.Context, issue string, role claim.Role) (runtime.WorkspaceWork, error) {
	reader, ok := r.supervisor.deps.Runtime.(runtime.WorkspaceWorkReader)
	if !ok {
		return runtime.WorkspaceWork{}, errors.New("the runtime cannot read the issue workspace")
	}
	return reader.WorkspaceWork(ctx, issue, role)
}

// checkLeftPodWorkspace is the first act of the close of a pod whose issue left the workflow while
// its tree runs on (workflow.LeftPodClose), where a live role runs in the pod: the close deletes a
// disk that role worked on, so the disk is read first, as the idle release reads it. A read that
// finds work that origin does not hold, or that fails, declines the close (declineFor,
// store.DeclineLeftPodClose): the pod stays with its roles as they are, the role read through is
// told once, and the rule holds the pod while the decline stands. Its hold carries the read's
// sentence as its detail, where a cancel's carries none, and is otherwise recorded as a cancel's is
// (cancelRelease), as is its line. A read that finds nothing queues the close's stops and the issue's
// Sandbox suspension (leftPodStopRows) in one transaction with a view read again
// (workflow.LeftPodCloseAfterCheck), so a role that started a turn since the pass that queued the
// check (payload.TurnStart) — one that ended while the read ran too — or that is in one or was
// given work now, is not stopped; the next pass decides that pod again. A clean read also forgets
// the decline on record, whose work it found gone, in that same transaction.
//
// The check takes the lifecycle serializer (treelifecycle.Serialize) before it touches a row of the
// issue, as every writer of the issue's lifecycle does: a clean read that deleted the decline row
// before ReleaseIdleIssue took the serializer would hold that row while waiting for the serializer,
// and a decline written under the serializer by the lane that holds the row now would wait for that
// row, a deadlock.
//
// Either way the check finishes its own row in the transaction that records what it decided
// (FinishLeasedOutbox), so that transaction commits only while this run's lease holds the row: a
// run whose lease another lane took writes nothing, and the lane that holds the row decides the
// close. And a check whose commit outcome is unknown leaves no row to run again: the stops it
// committed run on the lane, and a retry would read the workspace after they took the role's shim
// away, and decline a close already done, under a hold that no pass ends, since no pass visits a
// pod with no Sandbox.
func (r *outbox) checkLeftPodWorkspace(ctx context.Context, row record.OutboxRow, payload record.IssueWorkspaceCheck) error {
	readAt := r.now().UTC()
	work, failed := r.readIssueWorkspace(ctx, row.Issue, payload.Role)
	now := r.now().UTC()
	return store.Transact(ctx, r.trees.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		if failed != nil || work.HoldsWork() {
			decline, hold, why, err := r.declineFor(row.Issue, payload.Role, readAt, work, failed)
			if err != nil {
				return err
			}
			told, err := r.trees.DeclineLeftPodClose(ctx, r.project, row.Issue, decline)
			if err != nil {
				return err
			}
			if err := store.RecordPodHoldTx(ctx, tx, store.PodHold{Project: r.project, Issue: row.Issue,
				Reason: hold.String(), Detail: why, At: r.now().UTC()}); err != nil {
				return err
			}
			store.OnCommit(ctx, func() {
				r.log.Info("outbox: the close of the pod of an issue that left the workflow is declined, and the pod stays with its workspace",
					"issue", row.Issue, "hold", hold.String(), "reason", why, "role", payload.Role, "told", told)
			})
		} else {
			if err := r.trees.ForgetIdleDecline(ctx, r.project, row.Issue); err != nil {
				return err
			}
			closed, err := r.trees.ReleaseIdleIssue(ctx, r.project, row.Issue, leftPodStopsAfterCheck(now, payload.TurnStart))
			if err != nil {
				return err
			}
			store.OnCommit(ctx, func() {
				r.log.Info("outbox: the close of the pod of an issue that left the workflow read its workspace and found nothing that no branch or tag on origin holds",
					"issue", row.Issue, "role", payload.Role, "head", work.Head, "closing", closed)
			})
		}
		// The check's row is finished twice: here, fenced on this run's lease, so a lease another
		// lane holds fails the whole transaction (record.ErrOutboxLeaseLost); and once the check
		// returns, by the lane's own finish (outbox.finish), whose FinishOutbox then deletes
		// nothing and answers nil.
		return r.records.FinishLeasedOutbox(ctx, tx, row.ID, row.LeaseToken)
	})
}

// declineFor is the decline a workspace check records for what its read found (work, failed nil)
// or for a read that failed (failed), and the hold and the sentence it is recorded under. The
// decline's reason is its hold's name (workflow's idleHoldNames spells the two by the reasons), and
// its pod is the one the runtime holds for the issue now, so a later pass can tell a decline taken
// in a pod a relaunch since replaced (pods.IdleDecline.Stands). The role the workspace was read
// through is told that the release waits for its push (declineMessage), and the store queues that
// message only where the decline names other work than the decline on record
// (store's recordDecline). A read that failed tells nobody, since there is nothing the role can push
// on its word; it is a log line and a hold record, and the decline lets the pass ask again after
// an hour (pods.DeclineUnreadRetry). The decline is stamped with readAt, the moment the read began,
// so a turn that started while the read ran — one that may have pushed the work the read found —
// is a turn after the decline, which ends it (pods.IdleDecline.Stands).
func (r *outbox) declineFor(issue string, role claim.Role, readAt time.Time, work runtime.WorkspaceWork, failed error) (store.IdleReleaseDecline, workflow.IdleHold, string, error) {
	pod := ""
	if podRuntime, ok := r.supervisor.deps.Runtime.(issuePods); ok {
		if uid, read := podRuntime.IssuePod(issue); read {
			pod = uid
		}
	}
	at := readAt
	if failed != nil {
		hold := workflow.IdleHoldWorkspaceUnread
		return store.IdleReleaseDecline{At: at, Role: role, Reason: hold.String(), PodUID: pod}, hold,
			"the workspace could not be read: " + storetext.FarSideError(failed.Error()), nil
	}
	hold := workflow.IdleHoldUnpushedWork
	decline := store.IdleReleaseDecline{At: at, Role: role, Reason: hold.String(),
		Head: work.Head, Commits: work.Count, Changed: work.Changed, PodUID: pod}
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		return store.IdleReleaseDecline{}, hold, "", fmt.Errorf("name the role told of %s's declined release: %w", issue, err)
	}
	message, err := record.NewOutboxRow(issue, record.RoleMessage{Role: string(token), Body: declineMessage(issue, work)}, at)
	if err != nil {
		return store.IdleReleaseDecline{}, hold, "", fmt.Errorf("encode the message of %s's declined release: %w", issue, err)
	}
	decline.Message = &store.IdleReleaseRow{Kind: string(message.Kind), Payload: message.Payload}
	return decline, hold, declineReason(work), nil
}

// declineRelease ends a release whose workspace check found work that no branch or tag on origin
// holds on a working copy or a local bookmark (failed nil), or could not read the workspace
// (failed, the read's error). The release is cancelled before any role stopped, so the pod stays
// exactly as it was, and in the cancel's own transaction the store records the decline
// (store.DeclineIssueRelease, declineFor), which the idle rule reads to keep the pod under that
// reason with no new mark and no new read while it stands (pods.IdleDecline.Stands). So a pod that
// keeps its work is asked once, and its role is told once: a read after a turn that finds the same
// work keeps the decline and tells nobody. Its hold carries the read's sentence as its detail,
// where a cancel's carries none, and is otherwise recorded as a cancel's is (cancelRelease), as is
// its line.
func (r *outbox) declineRelease(ctx context.Context, issue string, payload record.IssueWorkspaceCheck, readAt time.Time, work runtime.WorkspaceWork, failed error) error {
	decline, hold, why, err := r.declineFor(issue, payload.Role, readAt, work, failed)
	if err != nil {
		return err
	}
	return store.Transact(ctx, r.trees.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		declined, err := r.trees.DeclineIssueRelease(ctx, r.project, issue, decline, r.releaseResume(issue, payload.Tree), record.OutOfWorkflow,
			store.ReleaseCancelPause{At: r.now(), Backstop: r.backstop, OpenPhaseRole: workflow.OpenPhaseRole})
		if err != nil {
			return err
		}
		if !declined.Declined {
			store.OnCommit(ctx, func() {
				r.log.Info("outbox release workspace check finished without acting: the pod's release was over when it declined",
					"issue", issue, "hold", hold.String(), "reason", why)
			})
			return nil
		}
		if err := store.RecordPodHoldTx(ctx, tx, store.PodHold{Project: r.project, Issue: issue,
			Reason: hold.String(), Detail: why, At: r.now().UTC()}); err != nil {
			return err
		}
		store.OnCommit(ctx, func() {
			r.log.Info("outbox: the idle release of an issue's pod is declined, and the pod stays with its workspace; no release is asked again until a role of the issue starts a turn",
				"issue", issue, "hold", hold.String(), "reason", why, "role", payload.Role, "told", declined.Told)
		})
		return nil
	})
}

// declineReason is what the hold record and the log line say a read that found work found: the
// count of commits on a working copy or a local bookmark that no branch or tag on origin holds, and
// whether the working copy holds a change, on the working-copy commit the read was made at.
func declineReason(work runtime.WorkspaceWork) string {
	return fmt.Sprintf("%d commit(s) not on origin, working-copy change %t, head %s", work.Count, work.Changed, work.Head)
}

// declineMessage is what the role is told when the release's check finds work in the workspace
// that origin does not hold: what the read found, that the release waits for the role's push, and
// that the daemon pushes nothing. The role reads it in its session; people read it in the role's
// history, so it follows ASD-STE100. The commit ids in it are 40 hexadecimal digits each, since the
// daemon reads no other answer from the shim (shimwire.WorkspaceWorkResult.Validate).
func declineMessage(issue string, work runtime.WorkspaceWork) string {
	found := []string{"The daemon did not release this issue's idle pod. Its workspace holds work that no branch or tag on origin holds."}
	if work.Changed {
		found = append(found, fmt.Sprintf("The working copy holds a change (commit %s).", work.Head))
	}
	commits := strings.Join(work.Commits, ", ")
	switch {
	case work.Count > len(work.Commits):
		found = append(found, fmt.Sprintf("The workspace holds %d commits on a working copy or a bookmark that origin does not hold. The first %d of these commits are: %s.",
			work.Count, len(work.Commits), commits))
	case work.Count == 1:
		found = append(found, fmt.Sprintf("The workspace holds one commit on a working copy or a bookmark that origin does not hold: %s.", commits))
	case work.Count > 1:
		found = append(found, fmt.Sprintf("The workspace holds %d commits on a working copy or a bookmark that origin does not hold: %s.", work.Count, commits))
	}
	found = append(found,
		"The idle release does not take a pod whose workspace holds such work, so the pod stays and the release waits for your push. "+
			"The daemon does not push for you.",
		fmt.Sprintf("Push the work that you keep to legion/%s, and abandon the work that you do not keep. "+
			"You are told this once: the daemon reads the workspace again after a turn of a role of this issue, and tells you again only if it finds other work.", issue))
	return strings.Join(found, " ")
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
// It is the last act of the other release rule too, and that release's one row: the release of
// a pod no role can use (workflow.UnownedPodRelease), whose every claim is already failed,
// retired or suspended. Nothing ran before it there — no check, since no shim is left to read
// the workspace, and no role stop, since no role is running — and its teardown is told from an idle
// one by the kind its mark carries (store.ReleaseDecision.SuspendsNoRole). Such a release
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
	releasing := store.IssueReleased{Issue: row.Issue, Tree: payload.Tree, IssueGeneration: payload.Generation,
		TreeGeneration: payload.TreeGeneration, Row: row.ID}
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
	// The runtime reads the store again under its pod lock, which is what orders the teardown
	// against a physical start, and that read is the one the pod is taken on: it is kept here
	// and acted on below. A runtime that takes the pod without asking leaves this read's answer
	// standing, which is the same release.
	decision := first
	authorize := func(ctx context.Context) (bool, error) {
		inner, err := r.trees.IssueRelease(ctx, r.project, releasing, record.OutOfWorkflow)
		if err != nil {
			return false, err
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
	released, err := releaser.ReleaseIssue(ctx, row.Issue, payload.Tree, authorize, writing)
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
// A cancel whose issue has left the workflow, or whose tree lingers, queues no resume and
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

func (r *outbox) seedGate(ctx context.Context, row record.OutboxRow, payload record.GateSeed) error {
	if r.dispatch == nil {
		return errors.New("gate seed executor has no Dispatch client")
	}
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
	// the tree's linger and its review may meet a parked tree.
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

func (r *outbox) linger(ctx context.Context, row record.OutboxRow, payload record.LingerClose) error {
	_, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("linger:%s:%d", row.Issue, payload.Generation), intake.LingerExpired{
		Issue: row.Issue, Generation: payload.Generation,
	}, r.handlers...)
	if err != nil {
		return fmt.Errorf("apply linger expiration for %s: %w", row.Issue, err)
	}
	return nil
}

// designGateClosed says whether the design gate of the issue's tree is closed, which holds the
// merger's start (supervise): the gate is the tree root's, so every member of the tree reads the
// same rows, and it is closed when a step the tree is gated on holds a row no approval has opened
// (classify.StandingGate, the first such step in the configured order). The steps are the
// deployment's and a criteria row the root's architect registered beside a deployment of spec
// alone (record.TreeSteps), so a new version of that Verification document holds the merger as a
// new spec version does. A tree with no row for a step — nothing registered one, or the
// project's design gate is off, which registers it approved — holds nothing on that step: nobody
// can be waiting on a document the record does not know. It is read just before the row acts, as
// every other fence of a row is.
func (r *outbox) designGateClosed(ctx context.Context, issue record.Issue) (bool, error) {
	var gates map[record.DesignStep]record.DesignGate
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		gates, err = r.records.GatesOf(ctx, tx, issue.Tree)
		return err
	}); err != nil {
		return false, fmt.Errorf("read the design gate of %s: %w", issue.Tree, err)
	}
	_, closed := classify.StandingGate(r.gatedSteps, gates)
	return closed, nil
}

// startNewsOf is what a start carries of news of the approved design, or that the start finishes
// without acting (finished), in the order the start executor asks it (supervise):
//
//  1. A start queued for news alone (record.SuperviseRequest.News) reaches only a claim such a
//     start may reach as the claim stands now (newsOnlyRefusal): one that exists and was given the
//     entry its role's phases row names, has not ended, and that no operator suspended. A root's
//     architect is read so too, since it holds a phases row that states the work it is owed, as a
//     phase worker does (dispatch://INFRA-2614): an architect behind its row's entry is owed that
//     entry's start, which carries its summary. It never creates a claim, revives one or resumes an
//     operator's, and one it may not reach finishes it, since the start that brings that claim back
//     carries the news. The generation, the phase, the workflow and the linger have fenced it before
//     this, as they fence any start.
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
// while its tree's design gate is closed, because a new version of the spec is with its reviewer
// and what may merge is what that review decides (dispatch://INFRA-714/spec R5). It answers a
// wait error for a merger start the gate holds, which leaves the start on the row's backoff as a
// check start waits on its look (checkLookHolds), and nil for every other role and an open gate.
// The gate opening releases the row's next try, and tells every phase worker of the tree and the
// root's architect as well (workflow's tellGateMoved).
//
// It also waits while the merger's member is owed news of the approved design that no start can
// carry yet, its change set not recorded, read or unavailable, or its earlier version neither read
// nor recorded unread with no reconcile of the tree pending (mergerNewsUnrecorded): a merger
// released without the change list could merge before it reads what changed. That wait ends within
// the design_changes row's bounded attempts, since a pair Dispatch cannot read is recorded
// unavailable, and a review history it cannot read is recorded unread (designChangesAttempts), and
// the start then carries the news (newsForStart).
//
// Every held message to a released merger (deliverHeld: a merge-queue removal, and a merge
// forward's hold and landed) reads this same rule before it wakes it, so no such message brings
// back a merger that its own start would hold.
func (r *outbox) mergerWaitsForTheDesignGate(ctx context.Context, issue record.Issue, role claim.Role) error {
	if role != claim.RoleMerger {
		return nil
	}
	closed, err := r.designGateClosed(ctx, issue)
	if err != nil {
		return err
	}
	if closed {
		return wait.Errorf("the merger of %s waits for the design gate of %s to open", issue.Key, issue.Tree)
	}
	unrecorded, err := r.mergerNewsUnrecorded(ctx, issue)
	if err != nil || !unrecorded {
		return err
	}
	return wait.Errorf("the merger of %s waits for what changed in the approved design of %s to be recorded", issue.Key, issue.Tree)
}

// checkLookHolds is the production check a check start row still has a look to start in, nil for
// none: its issue's production check is open, the one the row names (record.ProductionCheck.NamedBy),
// at the look the row names, with that look open, and the issue out of the workflow. A check closed
// since (a report, its last look), a check a later merge replaced, a look its bound or a hold ended,
// a later look's start, or an issue running again (reopened inside its linger since the row was
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

// checkLookHeld is a check start that met an operator's suspension of the check's own role
// (supervise.Claim.RuleHolds): a phase start waits for the operator's resume, but a look cannot
// run while the operator holds that role, and its bound would end it anyway, so the look
// ends now as one nobody reported (intake.CheckLookEnded, cause `operator hold`, of check, the
// one checkLookHolds read) and the row finishes without acting. The event is the issue, the check
// (record.ProductionCheck.ID) and the look: the same look held twice is one fact, and a later
// check's look 1 another than the replaced check's.
func (r *outbox) checkLookHeld(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest, check record.ProductionCheck) error {
	r.log.Info("production check start finishes without acting", "row", row.ID, "issue", row.Issue, "look", payload.CheckLook,
		"why", "the operator suspended the check's role; the look ends")
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("check-look-hold:%s:%s:%d", row.Issue, check.ID(), payload.CheckLook),
		intake.CheckLookEnded{Issue: row.Issue, Look: payload.CheckLook, Cause: "operator hold", OpenedAt: check.OpenedAt}, r.handlers...); err != nil {
		return fmt.Errorf("end look %d of %s's production check on the operator's hold: %w", payload.CheckLook, row.Issue, err)
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
// the issue has left, or of a tree that lingers, finishes without acting: the starts it held back
// act on neither.
func (r *outbox) issueBranch(ctx context.Context, row record.OutboxRow, payload record.IssueBranch) error {
	issue, err := r.issue(ctx, row.Issue)
	if err != nil {
		return err
	}
	if payload.Generation != issue.Generation {
		r.log.Info("outbox issue branch serves an earlier generation; finished without acting", "row", row.ID, "issue", issue.Key,
			"generation", payload.Generation, "current", issue.Generation)
		return nil
	}
	root, err := r.root(ctx, issue)
	if err != nil {
		return err
	}
	if root != nil && root.Lingers() {
		r.log.Info("outbox issue branch of a member of a lingering tree; finished without acting", "row", row.ID, "issue", issue.Key, "tree", issue.Tree)
		return nil
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
// lost it.
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
	return ghbranch.Create(ctx, github, repo, workspace.Bookmark(issue.Key), issue.DefaultBranch)
}

// removeWorkspace removes the workspace of the row's issue once its tree's linger expired. The
// removal belongs to the tree whose linger queued it and to that linger's root generation: an issue
// adopted into another tree since (workflow.Engine.Adopt) works in its new tree, and a tree
// re-admitted since runs in the workspace again, so either row finishes without acting. A row
// queued by an earlier release names no tree and is fenced by its linger alone, against the
// issue's current tree.
func (r *outbox) removeWorkspace(ctx context.Context, row record.OutboxRow, payload record.WorkspaceRemove) error {
	if r.supervisor == nil {
		return errors.New("workspace removal has no claim supervisor")
	}
	if r.podsProvision() {
		return nil
	}
	issue, err := r.issue(ctx, row.Issue)
	if err != nil {
		return err
	}
	if payload.Tree != "" && payload.Tree != issue.Tree {
		r.log.Info("outbox workspace removal of a tree the issue left; finished without acting", "row", row.ID, "issue", issue.Key,
			"tree", payload.Tree, "current", issue.Tree, "linger", payload.Linger)
		return nil
	}
	// The removal belongs to the linger it expired, the root generation it names. Once
	// re-admission ends that linger, the tree's new run may already work in the workspace again,
	// even in a later linger of the tree, so a removal still backing off finishes without acting.
	// Past the fence above, the tree the row names is the issue's own.
	root, err := r.root(ctx, issue)
	if err != nil {
		return err
	}
	if root == nil || !root.LingersAt(payload.Linger) {
		r.log.Info("outbox workspace removal of a linger that has ended; finished without acting", "row", row.ID, "issue", issue.Key,
			"tree", issue.Tree, "linger", payload.Linger)
		return nil
	}
	// The workspace goes only once the close has retired every claim of the issue: a claim whose
	// release the runtime refused still runs there, and one that is re-admitted before its close
	// lands goes on in it. A retired claim's relaunch provisions the workspace again.
	for _, role := range claim.Roles {
		token, err := claim.NewToken(r.project, row.Issue, role)
		if err != nil {
			return fmt.Errorf("derive claim for outbox row %d: %w", row.ID, err)
		}
		if machine, found := r.supervisor.Machine(token); found && machine.Claim().State != supervise.StateRetired {
			return wait.Errorf("workspace removal of %s waits for the tree's close to retire claim %s (%s)", row.Issue, token, machine.Claim().State)
		}
	}
	repo, err := issue.Repository()
	if err != nil {
		return fmt.Errorf("locate workspace for %s: %w", row.Issue, err)
	}
	working, err := workspace.Location(r.stateDir, repo, row.Issue)
	if err != nil {
		return fmt.Errorf("locate workspace for %s: %w", row.Issue, err)
	}
	// The removal forgets the workspace in the same shared clone a provision adds to: it takes its
	// issue's turn, so it never runs under that issue's provision, then hands workspace.Remove the
	// clone's store lock, the order a provision takes them in. Remove holds the lock for every step
	// on the clone and the rename aside, and lets it go before it deletes the workspace's files, so
	// nothing of another issue's provision waits for that delete.
	issueTurn, releaseIssue := r.provisions.issues.take(working.Dir)
	defer releaseIssue()
	issueTurn.Lock()
	defer issueTurn.Unlock()
	store, releaseStore := r.provisions.stores.take(working.Clone)
	defer releaseStore()
	if err := r.remove(ctx, working, store); err != nil {
		return fmt.Errorf("remove workspace for %s: %w", row.Issue, err)
	}
	return nil
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
