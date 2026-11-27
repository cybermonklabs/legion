// Package workflow applies the daemon-owned phase transition table inside intake's transaction.
package workflow

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Hook is one hook of the deployment as the engine runs it: its words are the text, never a
// path. The configuration holds the same hook with the path a deployment may have written
// instead (config.Hook), and boot reads that file, so nothing here opens one.
type Hook struct {
	Name   string
	On     string   // the event it stands on
	Moves  []string // empty selects every backward move
	Every  int
	Roles  []claim.Role
	Prompt string // the words, read at boot
}

// HookEventWorkBack is the one event a hook of this engine stands on: an issue's work went back
// a phase, which the daemon counts by the move's own `<from>-<to>` pair (phase.MovePairs).
// It is the word the configuration spells as well (config.HookEventWorkBack), written out again
// here because this package imports no configuration: a hook reaches the engine with its event
// as the operator wrote it, and the move back fires the hooks that name this one
// (fireWorkBackHooks).
const HookEventWorkBack = "work_back"

// Config supplies the project-scoped workflow limits and the clock used only to stamp durable
// outbox deadlines. Zero limits take their shipped defaults.
type Config struct {
	// Project is the project key, which is what the record's own tables hold: an issue's
	// `project`, a production check's, the Dispatch mirror's.
	Project string
	// Pods is the daemon's record of the project's released pods, which the wakes read and write
	// (Pods, wake.go). It holds the project as the claim-keyed tables spell it, derived from
	// Project once where it is built (store.Store.PodRecordsOf), so this configuration carries
	// one spelling of the project and a wake can file no row under another. Nil, as in every
	// engine built without the pod tables, wakes nothing and records no listener report.
	Pods Pods
	// Steps are the design-gate steps the deployment gates (config.Gates.Steps), in the order a
	// root's architect takes them. Every hold the gate puts on a tree's work reads the whole list
	// (classify.DesignGatesOpen), and the empty list gates no step, which is `gates.design: off`.
	Steps          []record.DesignStep
	ReviewRoundCap int
	Linger         time.Duration
	// ProductionCheckBound is how long a closed issue's production check waits for its deploy
	// before its last look (config.Config.ProductionCheckBound, `production_check_hours`): the
	// check's bound_at is its open plus this. Zero takes the shipped 14 days.
	ProductionCheckBound time.Duration
	// ProductionCheckRole is the project's `production_check` (config.Project.ProductionCheck):
	// the role that works the production check a merge closing an issue opens, and empty for the
	// project that opens none. It decides the checks opened under this configuration, never one
	// already open, whose role its own row holds.
	ProductionCheckRole claim.Role
	// DefaultSteps are the project's default steps (config.Project.RunsByDefault): the steps of
	// the one task default of an issue whose owner gives no plan or whose experiment's group takes
	// none, and of a task whose block names neither steps nor a sub-owner. Nil takes the steps of
	// a project that names none.
	DefaultSteps []plan.Step
	// Deployment is the roles a step of this deployment names, each with its type
	// (config.Config.Deployment): Legion's own and each role of `roles`. A stored step takes the
	// type of its role here (planSteps). A Config that sets no roles takes Legion's own
	// (plan.BuiltinRoles).
	Deployment plan.Deployment
	Clock      func() time.Time
	// ReviewAppLogin is the review App's bot login (<slug>[bot]) from its boot token lease. A review
	// it submits is the reviewer's own only when its Legion footer also names the session the
	// reviewer's claim records (byReviewer), and only then does it decide a round whatever GitHub
	// gives its bot account (DecidesRound) or answer a round the reviewer left undecided
	// (reviewersAnswer); while that claim records no session, it waits (RefusedWhileNoSession). A
	// push by it is recorded as the review App's (record.ClassifiedPush.ByReviewApp). Empty matches
	// no one. A workflow boot never leaves it empty: it refuses a review App that names no login
	// (daemon.appLoginsAtBoot).
	ReviewAppLogin string
	// Repositories is the daemon's in-memory view of the project's repositories, which the workflow
	// records an issue's repository and default branch from when it first records the issue.
	Repositories Repositories
	// RepositoryRenames is the deployment's repository equivalence (`repository_renames`,
	// config.Config.RepositoryRenames): wherever the engine compares a repository's name — an
	// issue's recorded one against a GitHub fact's, a check's against the build's — the two names
	// of a configured rename are one repository, since GitHub's events name the repository a
	// transfer moved to while the records keep the name they were written under. The zero value,
	// which answers exactly as a comparison without case does, while none is configured.
	RepositoryRenames ghrepo.Renames
	// NoteUnread is told, inside the fact's transaction, of each issue the workflow did not record
	// because its repository resolved Unread (left out for now, GitHub not answering its
	// installation read, or its read of the repository's name and default branch): the daemon
	// takes it into that repository's recovery whatever it resolves to by then. It keeps no state
	// of the issue's own. Nil notes none.
	NoteUnread func(key, resolved string)
	// HearRepository is told, inside the transaction that writes a new issue record, of the
	// repository GitHub spells that record's: the daemon has GitHub intake hear it and the
	// runtime keep a repository cache of it from that moment, on the transaction's commit, so
	// the first issue on a repository gets the same event and cache service as every later
	// one (daemon's hearRecorded). It is told every recording, including one on a
	// repository already heard, which costs the daemon one comparison. Nil tells none.
	HearRepository func(ctx context.Context, repo string)
	// ReadMergeState is asked, on the commit of a fact of one pull request that owes a read of
	// GitHub's merge state (askMergeState, by mergeStateOwed), to read it for the pull
	// request's head: a fact that leaves a review round waiting on GitHub's answer, which is what
	// moves an approved issue on to its task's next step (accepting, in the default steps); and, by
	// the gate classify.MergeStateReadOwed, a head move of an issue where a merge forward stands
	// (classify.MergeForwardStands: implementing, or a phase after it on the task's line), which can
	// turn the head conflicting with its base (dispatch://INFRA-4374 R0). It names the task whose
	// pull request owes the read, since an issue with plan rows has one pull request per task and a
	// read keyed by the issue alone replaces another task's; "" is the task the record judges, for
	// an issue whose pull request the record does not name. The daemon answers it
	// with a ladder of reads (workflowRuntime.queueMergeState); nil asks for none, which is the
	// API-only daemon and a test that drives the reads itself.
	ReadMergeState func(issue, task string)
	// ReadBaseMergeStates is asked, on the commit of a push to base of repo (basePush), to read
	// GitHub's merge state for each pull request on that base the push owes a read, by issue and
	// task: the
	// same reasons askMergeState reads a fact of one pull request by (mergeStateOwed:
	// classify.MergeStateReadOwed's base-push reason, a phase after implementing on the task's
	// line, or a review round waiting on GitHub), named
	// all at once, so that one push makes one batch of reads of its base rather than one read of
	// each pull request (workflowRuntime.queueBaseMergeStates, GraphQL queries of a few pull
	// requests each, asked for by number). owed is
	// never empty. Nil asks for none, as ReadMergeState's nil does.
	ReadBaseMergeStates func(repo, base string, owed []TaskPullRequest)
	// HoldCatchUp is told the outbox row of a catch-up the engine wrote held, once the fact that
	// wrote it has committed (claimReady, for an intake.ClaimReady the outbox's start row marked
	// paced): the daemon joins that row to its wake pacing's queue, where it waits its turn with
	// the role processes supervision launches, and makes it due when the pacer lets it out (the
	// daemon's catchUpPacing). nil is the pacing off, which is this package's convention for a
	// collaborator a deployment does not have, as NoteUnread is: the catch-up is then due at
	// once, exactly as every other catch-up is. The engine reads no configuration of the pacing
	// and knows no pacer; what the rate and the burst were is the outbox's to log, where the wake
	// runs (the daemon's wakeRan).
	HoldCatchUp func(id int64, issue string)
	// MessageMarked is told the claim a listener's report marked for a waiting Envoy message, with
	// the mark the store holds, once the report's fact has committed (listenerWake): the daemon
	// posts it to the claim's machine, whose waiting wake moves to the pacing's message queue
	// (supervise.MessageMarked). nil tells nobody, and the mark still stands in the store for the
	// next launch and the next boot to read.
	MessageMarked func(token claim.Token, mark time.Time)
	// Hooks are the deployment's hooks (`hooks`, config.Config.Hooks), each put in front of the
	// roles it names every Every moves back of what it selects. The empty list fires nothing:
	// the one hook a deployment that writes no key runs is the configuration's default
	// (config.DefaultHooks), never this engine's, so a deployment that configures none gets
	// none. Every hook's words arrive resolved, so the engine never reads a file and never sees
	// a path.
	Hooks []Hook
	// Experiments are the project's declared experiments (config.Project.ExperimentsDeclared),
	// of which every issue-unit one is drawn for each issue this engine records for the first
	// time, against that issue's own key (recordChild). Every project the loader reads declares
	// the memory experiment, whose unit is the tree and which is therefore drawn where a tree is
	// opened and never here, so a project that declares no experiment of its own has this engine
	// draw nothing. ExperimentSource is the random source a `random` mode is drawn with: nil is
	// experiment.CryptoSource, the daemon's own, and a test hands in one of its own. It is the one
	// source of the daemon's draws: admission draws the trees and issues it records with it too
	// (Engine.ExperimentSource).
	Experiments      []config.Experiment
	ExperimentSource func(uint64) (uint64, error)
	// StepAgent is the daemon's read of the agent of a step at the moment the engine writes the
	// step's end (endRun): the session its claim records and the model that answered it last in
	// that session (supervise.Machine.LastAnswer), read from the claim's machine without waiting
	// on a decision in flight. tx is the fact's own transaction, which holds the fact lock: a read
	// the agent's record needs is made in it, never on another connection and never a write. Nil
	// reads nothing, so a record names no session and no model, which is the API-only daemon and
	// every test that reads no agent.
	StepAgent func(ctx context.Context, tx pgx.Tx, agent claim.Token) StepAgent
	// WorkspaceDir is the directory of the working copy of the issue's task on branch, which the
	// task of each start and resume of a phase's worker names (the working copy line of task and
	// of resumeText): for the issue's own branch the directory the runtime starts the issue's role
	// processes in, and for a task's branch of its own the directory beside it the runtime places
	// that task's working copy at. The daemon reads it from the runtime it runs
	// (engineWorkspaceDir in internal/daemon), the one place each runtime puts that directory.
	// Nil, or "" for an issue and branch, names no directory, and the task names the branch alone:
	// an API-only daemon and every test that runs no runtime.
	WorkspaceDir func(issue record.Issue, branch string) string
}

// Engine interprets the table of each issue's walk (Walk: Table, or the table of its plan's line)
// and writes only records and outbox rows through the supplied Store.
type Engine struct {
	store record.Store
	cfg   Config
	log   *slog.Logger
	// droppedSettlements is the identities of the checks settlements whose drop checks has logged,
	// the last intake.DroppedSettlementsKept of them (logDroppedSettlement). It is marked once a
	// fact's transaction commits, and facts commit on each consumer's goroutine and the API's, so
	// the set takes its own lock (intake.RecentKeys).
	droppedSettlements *intake.RecentKeys[intake.SettlementIdentity]
}

var _ intake.Handler = (*Engine)(nil)

// New constructs a workflow engine, taking the shipped review round cap, production check bound,
// default steps, deployment and clock for every one of them a caller omits. A deployment it takes
// is a fresh map of Legion's own roles for each engine, so a role one engine adds reaches no other.
func New(store record.Store, cfg Config, log *slog.Logger) *Engine {
	if cfg.ReviewRoundCap <= 0 {
		cfg.ReviewRoundCap = 3
	}
	if cfg.ProductionCheckBound <= 0 {
		cfg.ProductionCheckBound = 336 * time.Hour
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.ExperimentSource == nil {
		cfg.ExperimentSource = experiment.CryptoSource
	}
	if cfg.DefaultSteps == nil {
		cfg.DefaultSteps = config.Project{}.RunsByDefault()
	}
	if cfg.Deployment.Roles == nil {
		cfg.Deployment = plan.Deployment{Roles: plan.BuiltinRoles()}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Engine{store: store, cfg: cfg, log: log, droppedSettlements: intake.NewRecentKeys[intake.SettlementIdentity](intake.DroppedSettlementsKept)}
}

func (e *Engine) now() time.Time { return e.cfg.Clock().UTC() }

// ExperimentSource is the random source every `random` experiment is drawn with
// (Config.ExperimentSource): this engine's draw of each child it records, and admission's of each
// tree and issue it records, which reads it here rather than holding a second one.
func (e *Engine) ExperimentSource() func(uint64) (uint64, error) { return e.cfg.ExperimentSource }

// byReviewApp says whether login is the review App's bot (Config.ReviewAppLogin). With no login
// configured, it is no one's.
func (e *Engine) byReviewApp(login string) bool {
	return e.cfg.ReviewAppLogin != "" && login == e.cfg.ReviewAppLogin
}

// sameRepository is whether a and b name one repository: the same name, compared without case as
// GitHub compares names, or the two names of a configured rename (Config.RepositoryRenames).
func (e *Engine) sameRepository(a, b string) bool { return e.cfg.RepositoryRenames.Same(a, b) }

// Apply changes workflow-owned phase state for one durable intake fact. It never performs I/O;
// every externally visible consequence is an outbox row committed with the record change.
//
// Every fact the handlers are done with goes through askMergeState, which asks the daemon to read
// GitHub's merge state when the fact is one of those that can change it and the round it left
// waits on the answer. It is here rather than in the handlers so that a handler's early return
// cannot silently stop the ladder, and so the facts that ask are one list the compiler checks
// rather than nine calls and a comment (dispatch://INFRA-714 R3).
func (e *Engine) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	result, err := e.apply(ctx, tx, fact)
	if err != nil {
		return result, err
	}
	return result, e.askMergeState(ctx, tx, fact)
}

func (e *Engine) apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	// An architect's request is judged again on the record as the fact reads it before its
	// handler runs (requestRefusal); its refusal writes nothing and is left unspent.
	if refusal, err := e.requestRefusal(ctx, tx, fact); err != nil || refusal != nil {
		return intake.Result{Refusal: refusal, Unspent: refusal != nil}, err
	}
	switch fact := fact.(type) {
	case intake.DispatchIssue:
		return e.dispatchIssue(ctx, tx, fact)
	case intake.GateRegistered:
		return e.gateRegistered(ctx, tx, fact)
	case intake.DispatchArtifact:
		return e.dispatchArtifact(ctx, tx, fact)
	case intake.GateAboveDispatch:
		return e.gateAboveDispatch(ctx, tx, fact)
	case intake.PendingBasisRead:
		return e.pendingBasisRead(ctx, tx, fact)
	case intake.BasisChangesRead:
		return e.basisChangesRead(ctx, tx, fact)
	case intake.ReparentRead:
		return e.reparentRead(ctx, tx, fact)
	case intake.GateRequestFound:
		return e.gateRequestFound(ctx, tx, fact)
	case intake.CatchUpApplied:
		return e.catchUpApplied(ctx, tx, fact)
	case intake.DispatchAskOpened:
		return e.dispatchAskOpened(ctx, tx, fact)
	case intake.DispatchAskClosed:
		return e.dispatchAskClosed(ctx, tx, fact)
	case intake.DispatchAskReply:
		return e.dispatchAskReply(ctx, tx, fact)
	case intake.DispatchApprovalMoved:
		return e.dispatchApprovalMoved(ctx, tx, fact)
	case intake.DispatchAskRestored:
		return e.dispatchAskRestored(ctx, tx, fact)
	case intake.HandoffComplete:
		return e.handoff(ctx, tx, fact)
	case intake.PullRequestOpened:
		return e.pullRequestOpened(ctx, tx, fact)
	case intake.PullRequestSynchronized:
		return e.pullRequestSynchronized(ctx, tx, fact)
	case intake.Push:
		result, err := e.push(ctx, tx, fact)
		if err != nil {
			return result, err
		}
		// A task's branch is also the base of the pull request of each task that needs it (one
		// need: its pull request is based on that need's branch), and a push on it is the only
		// notice that base moved. The push decodes as this branch's own Push, since the branch is
		// an issue's (intake's decodePush), so it is a base push of the pull requests on that
		// branch as well; a branch no pull request is based on finds none.
		_, err = e.basePush(ctx, tx, intake.BasePush{Repo: fact.Repo, Branch: fact.Branch, After: fact.After})
		return result, err
	case intake.BasePush:
		return e.basePush(ctx, tx, fact)
	case intake.PullRequestChecks:
		// A settlement on the head of a released pod's pull request wakes that pod before the
		// settlement is applied, so the woken role's task names the settlement this very
		// transaction records (wakeOnSettlement).
		// The role it woke is told no role message of that settlement: its wake task already
		// names it (tellChecks).
		woken, err := e.wakeOnSettlement(ctx, tx, fact)
		if err != nil {
			return intake.Result{}, err
		}
		return e.checks(ctx, tx, fact, woken)
	case intake.PullRequestMergeability:
		return e.mergeability(ctx, tx, fact)
	case intake.PullRequestReview:
		return e.review(ctx, tx, fact)
	case intake.PullRequestMerged:
		return e.merged(ctx, tx, fact)
	case intake.PullRequestClosed:
		return e.closed(ctx, tx, fact)
	case intake.PullRequestDequeued:
		return e.dequeued(ctx, tx, fact)
	case intake.PullRequestReviewRequests:
		// It records nothing: the read it asks for (askMergeState) counts the requests.
		return intake.Result{}, nil
	case intake.PullRequestMergeStateUnread:
		return e.mergeStateUnread(ctx, tx, fact)
	case intake.MergeQueueReadDone:
		return e.mergeQueueReadDone(ctx, tx, fact)
	case intake.MergeHoldsChanged:
		return e.mergeHoldsChanged(ctx, tx, fact)
	case intake.ConflictConfirmDone:
		return e.conflictConfirmDone(ctx, tx, fact)
	case intake.PullRequestMissing:
		return e.pullRequestMissing(ctx, tx, fact)
	case intake.ClaimReady:
		return e.claimReady(ctx, tx, fact)
	case intake.ClaimFailed:
		return e.claimFailed(ctx, tx, fact)
	case intake.SessionLost:
		return e.sessionLost(ctx, tx, fact)
	case intake.SessionStoreFault:
		return e.sessionStoreFault(ctx, tx, fact)
	case intake.MemoryGuardKill:
		return e.memoryGuardKill(ctx, tx, fact)
	case intake.ControllerMemoryGuardKill:
		return e.controllerMemoryGuardKill(ctx, tx, fact)
	case intake.RetryOrEscalate:
		return e.retryOrEscalate(ctx, tx, fact)
	case intake.RestartPhase:
		return e.restartPhase(ctx, tx, fact)
	case intake.BackwardMove:
		return e.backward(ctx, tx, fact)
	case intake.SignOff:
		return e.signOff(ctx, tx, fact)
	case intake.CloseRoot:
		return e.closeRoot(ctx, tx, fact)
	case intake.TaskStopped:
		return e.taskStopped(ctx, tx, fact)
	case intake.TaskJoined:
		return e.taskJoined(ctx, tx, fact)
	case intake.PlanGiven:
		return e.planGiven(ctx, tx, fact)
	case intake.LingerExpired:
		return e.lingerExpired(ctx, tx, fact)
	case intake.DaemonBooted:
		return e.booted(ctx, tx, fact)
	case intake.CutoverMergerRepair:
		return e.cutoverMergerRepair(ctx, tx, fact)
	case intake.CheckReport:
		return e.checkReported(ctx, tx, fact)
	case intake.CheckLookEnded:
		return e.checkLookEnded(ctx, tx, fact)
	case intake.CheckWakeDue:
		return e.checkWakeDue(ctx, tx, fact)
	case intake.CheckBound:
		return e.checkBound(ctx, tx, fact)
	case intake.ListenerWake:
		return e.listenerWake(ctx, tx, fact)
	case intake.PodBackstopDue:
		return e.podBackstopDue(ctx, tx, fact)
	case intake.VolumeNearFull:
		return e.volumeNearFull(ctx, tx, fact)
	case intake.WorkerSilent:
		return e.workerSilent(ctx, tx, fact)
	case intake.WorkerBackgroundStalled:
		return e.workerBackgroundStalled(ctx, tx, fact)
	case intake.ControllerTick:
		return e.statusRepairTick(ctx, tx)
	default:
		return intake.Result{}, nil
	}
}

func (e *Engine) dispatchIssue(ctx context.Context, tx pgx.Tx, fact intake.DispatchIssue) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil {
		return intake.Result{}, err
	}
	// The issue may be an ancestor that steps of other roots wait on, closed, whatever tree it is
	// in or none (dispatch://INFRA-3320, R1), so this, which queues the pending-basis seed of each
	// step its event is a cue for, runs before the record's own fences and before an unrecorded
	// issue's return.
	if err := e.readBasisOnCue(ctx, tx, fact); err != nil {
		return intake.Result{}, err
	}
	if fact, err = SettleRetainedRepository(fact, issue); err != nil {
		return intake.Result{}, err
	}
	if issue == nil {
		return e.recordChildUnderLiveTree(ctx, tx, fact)
	}
	// The record keeps the repository it was recorded with, so a newer event resolving the issue
	// to another is not applied, which the issue is told. This runs before any fence below: an
	// edit of the repository alone changes no status. So does the telling of a root a person has
	// made a child of a live tree (tellReparented), which is a parent change and often no status
	// change at all.
	if fact.Seq == 0 || fact.Seq > issue.LastDispatchSeq {
		if err := e.NoteRepositoryKept(ctx, tx, *issue, fact.ResolvedRepo); err != nil {
			return intake.Result{}, err
		}
		if err := e.tellReparented(ctx, tx, issue, fact); err != nil {
			return intake.Result{}, err
		}
	}
	if consumed, err := e.rejectStaleHandover(ctx, tx, *issue, fact); err != nil || consumed {
		return intake.Result{}, err
	}
	// Admission, which runs after this handler, records every newer observation of a recorded
	// issue (its title, rank, parent, and status) and owns a todo's re-admission. The engine reacts
	// only to a newer status, and records the observation itself when it keeps its own status over it.
	//
	// This is the sequence fence, which the engine and admission both keep: an event the record
	// has already applied — a redelivery, or one older than the newest the record took — teaches
	// the record nothing, so nothing below here runs for it. The engine's own reaction waits
	// behind a second fence, the status one, and that fence stands below the mark: an event the
	// record has not applied is news of the issue whether or not it moves its status.
	if fact.Seq != 0 && fact.Seq <= issue.LastDispatchSeq {
		return intake.Result{}, nil
	}
	// The daemon has seen the issue move (record.Issue.MovedAt). This is the first of the eleven
	// points that write that mark, and the rule every one of them follows is written here.
	//
	// The mark is written where the record changes, never from a list of fact kinds. A list would
	// silently miss a kind a later change adds — the mark would stop being written for it, with
	// nothing failing to say so — and three of the six movements are no fact of their own anyway:
	// a phase change, a start and a recorded handoff are consequences several different facts
	// reach. So each site sits at the write it is about. Nine are in this package: this one, the
	// Dispatch event of the issue; the pull request's row (Engine.putPullRequest); the phase
	// write (Engine.transition and Engine.retryPhase); the hold a failed claim writes
	// (Engine.claimFailed); the handoff's phase row and the completion that records it
	// (Engine.handoff); the wait on a person a root enters (enterAwaiting); the resume of a root
	// a person answered, which starts its architect again (ResumeAwaitedRoot, for the promotion
	// that answers the wait and for the root told in place alike); and the start (StartOrOwe).
	//
	// The other two are the admission pass's own writes of a phase and of a start, which no
	// fact of the issue carries and which this package therefore never sees: the re-admission
	// that opens a new generation of a tree (admit's readmit), and the promotion of a root the
	// line has been holding, which opens its tree and queues its architect's start (admit's
	// promoteHolds). Unmarked, a root the pass had just started would carry the stamp of the
	// moment a person labelled it, and the next pass would read a tree the daemon opened a
	// second ago as a tree that had stopped (admit's tellStalledTrees).
	//
	// Here the mark goes past the sequence fence above and before the status fence below, because
	// a movement is every Dispatch event of the issue that is new to the record, and not only one
	// that moves its status (dispatch://INFRA-2560, R3: a Dispatch event of the issue). An event
	// the record has already applied stopped at that sequence fence and moves nothing; every event
	// past it changes the record, whatever this handler then does with it. Where its status is new
	// to the record the handler's own write is that change: an observation kept over the daemon's
	// own status (keepStatus), a status the daemon sets back after a session wrote it
	// (sessionStatusWrite), a leave, a child's re-entry. Where it is not — the steady state of a
	// running issue, the record and the event carrying one status between them — the change is the
	// observation admission writes below this handler (admit's recordObservation, or a
	// re-admission): a rename, a re-rank, a re-parent or a label change, each of them news of the
	// issue, and the event's own sequence with it. Marking below the status fence instead would
	// leave every edit of a running issue unmarked, which is the whole steady state of a tree at
	// work, and the tree would read as quieter than it is.
	//
	// What it leaves out, by fact.DaemonActor, is the daemon talking to itself: the status the
	// daemon writes to Dispatch comes back as an event its own session authored
	// (intake.OwnStatusWrite, and the decode's daemonActor), both for the write a route makes and
	// for the re-assertion sessionStatusWrite queues when somebody else wrote a status. Counting
	// those would make a stopped tree move for ever: nothing outside the daemon happened, and the
	// daemon's own re-assertion would keep refreshing the mark of an issue nobody is working.
	//
	// Four other paths are deliberately left unmarked. The boot's read of the Dispatch listing
	// records an observation of its own (admit.Admission.Reconcile and applySummary), which is
	// the daemon catching up with what it already knew rather than news of the issue; it is the
	// observation that is left unmarked, so a re-admission the same listing brings is marked
	// all the same, at the write site in admit's readmit, since the generation it opens is news
	// of the issue whatever read brought it. The status repair (statusRepairTick) queues the
	// status of the phase the issue already stands in, so it writes no movement of its own. A
	// claim's activity report or state change writes the claims table and never the issue: a
	// session that is up is no proof that the issue it holds has moved, which is the whole of
	// what this mark is for. And the daemon's own sends of work the record already owes a phase
	// worker are starts that mark nothing (startOrOwe's reassertion): every send of an owed
	// entry — admission's sweep on every pass and each minute, its slot repair, readyWorkerTask,
	// the boot's resume by rule, the cutover's one-time repair (cutoverMergerRepair) and the
	// silence repair (sendOwedEntry) — and the phase's resume task the boot's resume, the
	// cutover's repair and the silence repair send a claim level with its entry. The
	// movement is the fact that raised the entry, which the transition's phase write and its
	// start mark below (Engine.transition, StartOrOwe), and not each delivery of it.
	// An event that changed nothing of the issue but the priority Dispatch resolves it to — an
	// ancestor's priority or parent moved (intake.DispatchIssue.ResolvedOnly) — is no work of the
	// issue's own, so it marks no movement either, while the record still takes its priority.
	if !fact.DaemonActor && !fact.ResolvedOnly {
		if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
			return intake.Result{}, err
		}
	}
	// The engine reacts to a newer status alone: an event carrying the status the record holds is
	// an edit, which it leaves to the observation admission records (admit's recordObservation).
	if fact.Status == issue.Status {
		return intake.Result{}, nil
	}
	// An event carries the whole issue, so it writes a status only when its status differs from the
	// one Dispatch showed at the event before it. One that does not is an edit — a rank, title or
	// label change — made while a status the daemon queued has not reached Dispatch, whoever made it.
	// A route's own write (intake.DispatchIssue.StatusMove) is a status Dispatch confirmed the daemon
	// wrote, applied ahead of events the consumer has not delivered, so it is a move whatever the
	// record last saw Dispatch show.
	if fact.Status == issue.DispatchStatus && !fact.StatusMove {
		return intake.Result{}, e.keepStatus(ctx, tx, *issue, fact)
	}
	if reasserted, err := e.sessionStatusWrite(ctx, tx, *issue, fact); err != nil || reasserted {
		return intake.Result{}, err
	}
	if record.OutOfWorkflow(fact.Status) {
		// The move is the issue's status now, so no status write the daemon queued before it lands
		// over it: a set-back queued over an outside backlog would otherwise find a person's later
		// backlog on Dispatch and write over that.
		if err := e.store.DropStatusWrites(ctx, tx, issue.Key); err != nil {
			return intake.Result{}, err
		}
		return intake.Result{}, e.leave(ctx, tx, *issue, fact.Status)
	}
	// A todo written on a held root whose tree does not linger releases the hold
	// (releaseHeldRoot). Its test is the complement of admission's own, by construction and not
	// by a reader's care: both ask record.Issue.Readmittable, so the two can never both fire. A
	// root that lingers or stands done is re-admitted there at a new generation, and a root held
	// from a phase stands at neither, which keeps the park and un-park of a lingering root on
	// its new generation. A held child is untouched, and comes back through ReenterChild below.
	if fact.Status == "todo" && claim.IsTreeRoot(issue.Key, issue.Tree) && issue.Phase == phase.Held && issue.Hold != nil && !issue.Readmittable() {
		return intake.Result{}, e.releaseHeldRoot(ctx, tx, *issue, fact)
	}
	if fact.Status == "todo" && !claim.IsTreeRoot(issue.Key, issue.Tree) {
		return intake.Result{}, e.ReenterChild(ctx, tx, *issue, fact)
	}
	return intake.Result{}, nil
}

// releaseHeldRoot takes a todo written on a held root whose tree does not linger: the one status
// a person's move in the Dispatch dashboard, the controller's set_status and an operator's
// `legion issue status` all reach the record as (R7). The hold is cleared, the issue returns to the
// phase the hold came from and that phase's role is started with its task, which is the effect
// of the architect's retry_or_escalate with retry. Nothing else moves: no new generation, no
// reset of the rounds, no new pod, and no change of place in the admission line.
//
// The observation is taken into the value the retry writes (observedIssue), so the one row this
// transaction writes for the issue carries both: the recorded status kept, which stops
// admission — running after this handler — at its sequence fence so it never writes
// `status: todo` over the release, and the phase the hold came from.
//
// The Dispatch status of the phase the issue ends in is queued whatever path it ended on
// (retryPhase answers that phase), as a phase transition queues it, so the board shows the phase
// the issue works and a later todo on the same issue is a status change again. It is enqueued
// here rather than through Engine.status, which writes nothing when the record's own status
// already holds the status asked for: a hold changes no status, so the record still reads the
// status of the phase it was held from — in_progress, testing, needs_review — while Dispatch
// reads todo, and the write the board needs is the one Engine.status would drop.
// correctStatuses bypasses Engine.status for this same trap.
//
// Where the release's own path moved the issue — a standing red that sends it back to
// implementing, a review round that settles — that move queued a row of its own through
// Engine.status, and that row is harmless beside this one: it is keyed on the status the record
// held rather than on the todo Dispatch shows (record.StatusWrite.ObservedStatus), so the
// executor finishes it without a write (the daemon's outbox status), and where the status it
// would have written is the one the record already held it was dropped at the queue and there is
// no row at all. The row this line queues names the todo, so it is the one that writes.
//
// A hold of the root's plan (planHold) is not a failure a retry answers. A todo on a root held for
// its stopped task ends the stop: a task whose work ran starts again at its first step
// (startAtFirstStep), and a task stopped before its first step went out waits for that step again
// (waitForFirstStep). A todo on a root held before its retro (record.HoldForChildren) leaves the
// hold, since the retro starts by itself once nothing holds the root, keeps the observation, and
// logs why. It queues the status the record keeps, the last step's, over the todo, with the reason
// the hold still stands posted beside it (heldParentWait's reason), so the person who set the todo
// reads why the board went back, as the release of any other hold queues the status of the phase
// it ends in: every later status write of the issue is keyed on the record's status
// (Engine.status), the retro's that ends the hold among them (StartHeldRetro), and finds the board
// at it only once this row has put it back.
//
// A root whose plan holds several tasks is held where no task runs and one is held or stopped
// (issuePhaseOfTasks), so its hold is that of its held and stopped tasks together. The todo names
// no task, so each of them is released on the copy of the root read for it (viewsOf, and view of
// the issue as the release before wrote it for each after the first), by the rule above, as the
// one task of a plan of one is released on the root itself; and the status queued for the tasks
// retried is that of the phase the root then stands in (phaseNow). Once each is released, each
// task that may start then starts (startReadyNow), once for the todo: a task released or started
// again at its parts with no wait lets its parts that wait start.
func (e *Engine) releaseHeldRoot(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) error {
	released, err := e.observedIssue(ctx, tx, issue, fact)
	if err != nil {
		return err
	}
	scopes, err := e.scopes(ctx, tx, released)
	if err != nil {
		return err
	}
	retried, releasing := false, false
	for _, held := range scopes {
		if held.issue.Phase != phase.Held || held.issue.Hold == nil {
			continue
		}
		if releasing {
			// The release before wrote the issue, the observation with it, so this task is read on
			// the issue as that write left it, not on the copy read before it, whose status is gone.
			current, err := e.store.Issue(ctx, tx, issue.Key)
			if err != nil {
				return err
			}
			if current == nil {
				return fmt.Errorf("release the held task %s of %s: the issue is no longer recorded", held.walk.Task, issue.Key)
			}
			if held, err = e.ScopeOf(ctx, tx, *current, held.walk.Task); err != nil {
				return err
			}
		}
		releasing = true
		switch {
		case !planHold(held.issue, held.walk.Stored):
			if _, err = e.retryPhase(ctx, tx, held, held.issue.Hold.From, releaseReason, false); err != nil {
				return err
			}
			retried = true
		case record.HeldForChildren(held.issue):
			wait, err := e.heldParentWaitOf(ctx, tx, held.issue)
			if err != nil {
				return err
			}
			e.logOnCommit(ctx, "workflow: a todo leaves the hold of a root that waits before its retro; its retro starts once nothing holds it",
				"issue", held.issue.Key, "hold", wait.held.code, "holding", wait.held.names)
			if _, err := e.putScope(ctx, tx, &held); err != nil {
				return err
			}
			if err := e.enqueue(ctx, tx, held.issue.Key, record.OrdinaryStatusWrite(held.issue.Status, fact.Status, wait.reason())); err != nil {
				return err
			}
		case !held.walk.Place.Started():
			if err := e.waitForFirstStep(ctx, tx, held, restartReason); err != nil {
				return err
			}
		default:
			if err := e.startAtFirstStep(ctx, tx, held, restartReason); err != nil {
				return err
			}
		}
	}
	if releasing {
		if err := e.startReadyNow(ctx, tx, issue.Key); err != nil {
			return err
		}
	}
	if !retried {
		return nil
	}
	// The status queued is that of the phase the root stands in once each held task is retried
	// (phaseNow), the phase the release put it in, or the one a move the release made took it to.
	ended, err := e.phaseNow(ctx, tx, released)
	if err != nil {
		return err
	}
	return e.enqueue(ctx, tx, issue.Key, record.OrdinaryStatusWrite(PhaseStatus(ended), fact.Status, ""))
}

// releaseReason is the daemon's own reason for that release, which the restarted role's task and
// the log line of the release both carry: it names the status write that made it, since no caller
// of the status route names a reason of its own (R7).
const releaseReason = "the issue was set to todo while its phase was held"

// restartReason is the daemon's own reason for a todo's end of a stopped task's stop, which the task
// of its first step's worker and the log line of the restart carry, for the same reason, and the log
// line of a task stopped before its first step that waits for that step again.
const restartReason = "the issue was set to todo while its task was stopped"

// sessionStatusWrite takes a lifecycle status change written by a session whose write the workflow
// does not act on. A session holding a claim in the issue's tree is an agent, never a human move,
// since a phase ends only by its completion (the `legion` tool's `handoff_complete`) and a tree
// only by the architect's sign-off. Any other session on the root of a running tree — one holding
// its admission slot, or one waiting on a person, whose tree stays live and which keeps its slot
// — another tree's agent, or one outside Legion that took the issue for its own, has no part
// in the tree it would end or park, so the tree's architect is told who wrote what. Either way the
// workflow does not react: the observation is recorded with the status kept (keepStatus), and the
// daemon re-asserts its own status through the outbox. A person's move and the daemon's own write
// carry no session (intake.DispatchIssue.ActorSession): they are the workflow's to act on.
//
// One write of a session of the tree is not set back: the owning architect's done, backlog,
// icebox or triage on a child of its tree (owningArchitectsMove). The architect decides that the
// work of a child is done, folded into another issue or no longer needed, and the child's status
// in Dispatch is the one field that records that decision (dispatch://INFRA-3178). For that write
// the function answers that it did not handle it, so dispatchIssue takes the move as it takes a
// person's: it drops the status writes it queued for the child, and the child leaves the workflow
// (leave). Every other write of a session of the tree is set back. That is a status that moves
// the workflow or todo, any status of a phase worker, a child's own architect's status on its own
// issue, an architect's status on an issue it does not own, and any status on the tree root.
func (e *Engine) sessionStatusWrite(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) (bool, error) {
	if fact.ActorSession == "" {
		return false, nil
	}
	agent, err := e.store.SessionClaimsTree(ctx, tx, issue.Tree, fact.ActorSession)
	if err != nil {
		return false, err
	}
	if agent {
		if stands, err := e.owningArchitectsMove(ctx, tx, issue, fact); err != nil || stands {
			return false, err
		}
	} else {
		// Whether the write lands on the root of a tree that is running now, by the one rule every
		// reader of that question asks (record.TreeRunning, record.TreeRuns): its lifecycle stands
		// open under the workflow's own authority, it does not linger, and it is not itself waiting
		// in the line for its tree to be opened. It is the lifecycle rather than a slot because a
		// slot counts one issue at work now, and a running tree holds none of its own while every
		// issue in it waits in the line.
		//
		// It is deliberately not record.TreeLive, which also calls a root waiting in the line for
		// its tree to be opened live, so that a todo child of such a root records into the tree
		// that will adopt it. The question here is whether a session's lifecycle status write lands
		// on a tree already running, and a root waiting for its slot runs nothing — a root reopened
		// inside its own linger among them, whose readmit cleared the linger while the ended run's
		// lifecycle still stands open: where no tree runs, an outside session's write is a person's
		// move, which is the whole of the question.
		waiting := claim.IsTreeRoot(issue.Key, issue.Tree) && issue.AwaitingPerson() && !issue.Lingers()
		if admitted, err := record.TreeRunning(ctx, e.store, tx, issue, e.cfg.Steps); err != nil || !admitted && !waiting {
			return false, err
		}
	}
	e.logOnCommit(ctx, "workflow: a session wrote a lifecycle status; the daemon re-asserts its own", "issue", issue.Key,
		"session", fact.ActorSession, "claims_tree", agent, "wrote", fact.Status, "status", issue.Status)
	if err := e.keepStatus(ctx, tx, issue, fact); err != nil {
		return false, err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.ReassertStatusWrite(issue.Status, fact.Status, fact.Seq)); err != nil {
		return false, err
	}
	if agent {
		return true, nil
	}
	return true, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "status-reasserted", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("session %s set %s to %s, and the daemon set it back to %s", fact.ActorSession, issue.Key, fact.Status, issue.Status)})
}

// owningArchitectsMove says whether fact, the write of a session that holds a claim in issue's
// tree, is the owning architect's move of issue out of the workflow, which sessionStatusWrite does
// not set back. Four conditions make it one:
//   - The status is done, backlog, icebox or triage (record.OutOfWorkflow).
//   - The issue is not the tree root, which leaves the workflow only by the architect's sign-off
//     or close_root.
//   - The session holds the architect claim of an issue of the tree, in any state
//     (record.Store.ArchitectIssueOfSession).
//   - That architect owns the issue by the rule of the API's operations on a child
//     (record.OwnsUnder). The tree root's architect owns every issue of its tree, so its move is
//     judged with no read of the tree. A sub-architect's move is judged over the tree's records.
//
// A move that stands writes its own log line, which names the architect's issue.
func (e *Engine) owningArchitectsMove(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) (bool, error) {
	if !record.OutOfWorkflow(fact.Status) || claim.IsTreeRoot(issue.Key, issue.Tree) {
		return false, nil
	}
	architect, held, err := e.store.ArchitectIssueOfSession(ctx, tx, issue.Tree, fact.ActorSession)
	if err != nil || !held {
		return false, err
	}
	if !claim.IsTreeRoot(architect, issue.Tree) {
		members, err := e.treeMembers(ctx, tx, issue)
		if err != nil {
			return false, err
		}
		if !record.OwnsUnder(members, issue.Tree, architect, issue) {
			return false, nil
		}
	}
	e.logOnCommit(ctx, "workflow: the owning architect's status on its child stands", "issue", issue.Key,
		"session", fact.ActorSession, "architect", architect, "status", fact.Status)
	return true, nil
}

// observedIssue is issue with everything an observation of fact records on it: its title, rank,
// priority, parent, label, sequence, the status Dispatch showed and the Dispatch chain its
// preparation read (Ancestry), with the recorded status kept. It writes nothing, so a caller that
// gives the issue a phase of its own puts the two in one row (releaseHeldRoot).
func (e *Engine) observedIssue(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) (record.Issue, error) {
	ancestry, err := e.Ancestry(ctx, tx, issue, fact.Ancestry)
	if err != nil {
		return issue, err
	}
	issue.Title, issue.Rank, issue.Priority, issue.Resolved, issue.Parent, issue.LastDispatchSeq = fact.Title, fact.Rank, fact.Priority, fact.Resolved, record.ParentOf(fact.Parent), fact.Seq
	issue.HandedOver, issue.DispatchStatus, issue.Ancestry = fact.HandedOver, fact.Status, ancestry
	return issue, nil
}

// keepStatus writes that observation (observedIssue). Admission, running after this handler,
// then records none of that status: a live event stops at its sequence fence, and a boot listing
// the engine applied this way keeps the recorded status (admit's applySummary), so neither frees
// the slot nor re-admits on it.
func (e *Engine) keepStatus(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) error {
	observed, err := e.observedIssue(ctx, tx, issue, fact)
	if err != nil {
		return err
	}
	return e.WriteIssue(ctx, tx, observed)
}

// rejectStaleHandover consumes a daemon-authored todo that was written for a closed tree but
// arrived after its root came back. The durable hand-over fence is checked in intake's serialized
// transaction, not before the outbox PATCH: otherwise the reopen and this event could both act on
// the child. Keep the child in its live tree, reassert its workflow status, and fence admission
// from seeing this todo by recording the event sequence.
//
// It runs before dispatchIssue's sequence fence, so the todo can reach it after a newer event of
// the child was applied: delivered late, or replayed from a message the intake gave up on, however
// late. Such a todo is still consumed, its fence row deleted and the status reasserted, but it
// records nothing on the child: the newer event's title, sequence and Dispatch status stand, and
// the sequence they hold already fences admission from it.
func (e *Engine) rejectStaleHandover(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) (bool, error) {
	if !fact.DaemonActor || !fact.HandedOver || fact.Status != "todo" {
		return false, nil
	}
	handover, err := e.store.Handover(ctx, tx, issue.Key)
	if err != nil || handover == nil {
		return false, err
	}
	root, err := e.store.Issue(ctx, tx, handover.Tree)
	if err != nil {
		return false, err
	}
	if !handover.Cancelled && issue.Tree == handover.Tree && root != nil && root.LingersAt(handover.Generation) {
		return false, nil
	}
	if err := e.store.DeleteHandover(ctx, tx, issue.Key); err != nil {
		return false, err
	}
	if fact.Seq == 0 || fact.Seq > issue.LastDispatchSeq {
		if err := e.keepStatus(ctx, tx, issue, fact); err != nil {
			return false, err
		}
	}
	if issue.Status != fact.Status {
		if err := e.enqueue(ctx, tx, issue.Key, record.ReassertStatusWrite(issue.Status, fact.Status, fact.Seq)); err != nil {
			return false, err
		}
	}
	e.logOnCommit(ctx, "workflow: rejected stale hand-over event", "issue", issue.Key, "tree", handover.Tree, "linger", handover.Generation)
	return true, nil
}

// recordChildUnderLiveTree owns the otherwise unrecorded-child edge from decision 13. Admission
// records only roots and orphans because this runs first in intake.ApplyFact.
func (e *Engine) recordChildUnderLiveTree(ctx context.Context, tx pgx.Tx, fact intake.DispatchIssue) (intake.Result, error) {
	if fact.Status != "todo" || fact.Parent == "" {
		return intake.Result{}, nil
	}
	parent, err := e.store.Issue(ctx, tx, fact.Parent)
	if err != nil || parent == nil {
		return intake.Result{}, err
	}
	root, err := e.store.Issue(ctx, tx, parent.Tree)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	live, err := record.TreeLive(ctx, e.store, tx, *root, e.cfg.Steps)
	if err != nil || !live {
		return intake.Result{}, err
	}
	return intake.Result{}, e.recordChild(ctx, tx, *root, fact)
}

// recordChild records an unrecorded todo child under root's live tree on the repository its event
// resolves to, or, when that is none or one GitHub does not answer for as a repository a role App
// reaches, records nothing and says why on the child and to the architect that owns its parent
// (refuseChild). A child recorded on a repository nothing named before has that repository heard
// and cached from here (Config.HearRepository).
//
// The daemon holds no record of this child, so this entry is the one moment the issue-unit
// experiments the project declares are drawn for it (drawExperiments), in the transaction that
// records it: a draw covers a unit from the moment the daemon records that unit, so a re-entry, a
// rerun and an adoption, each of a record the daemon already holds, draw nothing, and an
// experiment declared after a child was recorded never reaches it.
func (e *Engine) recordChild(ctx context.Context, tx pgx.Tx, root record.Issue, fact intake.DispatchIssue) error {
	resolution, err := e.ResolveIssueRepository(fact)
	if err != nil {
		return err
	}
	if resolution.Refusal != "" {
		return e.refuseChild(ctx, tx, root, fact.Key, fact.Parent, fact.ResolvedRepo, resolution)
	}
	if err := e.enterChild(ctx, tx, root, fact, root.Generation, resolution); err != nil {
		return err
	}
	e.HearRepository(ctx, resolution.GitHubRepo)
	return e.drawExperiments(ctx, tx, fact.Key)
}

// HearRepository QUEUES the repository an issue has just been recorded on, inside the transaction
// that writes the record (Config.HearRepository). It makes no stream call under the lock and
// cannot fail the transaction: hearRecorded publishes once the transaction has returned, and only
// then does GitHub intake hear the repository and the runtime keep a repository cache of it.
// Admission calls it for the roots and orphans it records; a deployment that configures none is
// told nothing.
func (e *Engine) HearRepository(ctx context.Context, repo string) {
	if hear := e.cfg.HearRepository; hear != nil {
		hear(ctx, repo)
	}
}

// RecordedFact is the todo fact an issue's own record already says, for a run no live event
// brought: admission's promotion of a stranded child (ReenterChild) and Adopt's entry of a queued
// root's todo members. It is a fact the daemon builds from its own record, so it keeps the stored
// priority by copying it: a fact built from what Dispatch sent carries Dispatch's, null included,
// and replaces it.
func RecordedFact(issue record.Issue) intake.DispatchIssue {
	fact := intake.DispatchIssue{
		Key: issue.Key, Seq: issue.LastDispatchSeq, Status: issue.Status, Title: issue.Title,
		Rank: issue.Rank, Priority: issue.Priority, Resolved: issue.Resolved, HandedOver: issue.HandedOver, ResolvedRepo: issue.Repo,
	}
	if issue.Parent != nil {
		fact.Parent = *issue.Parent
	}
	return fact
}

// recordedResolution is the repository an issue was first recorded on, which a re-entered or
// adopted record keeps.
func recordedResolution(issue record.Issue) Resolution {
	return Resolution{Repo: issue.Repo, DefaultBranch: issue.DefaultBranch, GitHubRepo: issue.GitHubRepo}
}

// ReenterChild takes a recorded child set back to todo into a new run under its live tree, the way
// recordChildUnderLiveTree enters an unrecorded one: the run is the child's next generation, so no
// row its previous run queued acts on it; the previous run's facts are cleared, and the architect
// above the child is told. A child whose tree is not live is an orphan, which admission, running
// after this handler, admits as a root of its own. fact carries the values the new generation
// takes — a live todo event's for a reopened child, or the child's own already-recorded ones for
// admission's promotion of a stranded child no live event ever reopened, so the run is exported
// for that caller too: one re-entry, not two.
//
// A done child's open production check closes as not run before the new run starts
// (markCheckClosed). The run's generation fences a completion left by an interrupted phase, and
// the closed check refuses an old look's report, so both claims stay live.
func (e *Engine) ReenterChild(ctx context.Context, tx pgx.Tx, child record.Issue, fact intake.DispatchIssue) error {
	root, err := e.store.Issue(ctx, tx, child.Tree)
	if err != nil || root == nil {
		return err
	}
	live, err := record.TreeLive(ctx, e.store, tx, *root, e.cfg.Steps)
	if err != nil || !live {
		return err
	}
	next := child.Generation + 1
	// The worker of the phase the child was taken out of may still finish its old turn, but its
	// completion serves the generation it took and the API refuses it once this generation begins.
	// Roles that finished earlier phases of the run stay in whatever state they stood in: this
	// re-entry starts and stops none of them, so for a child that had left the workflow each is as
	// workflow's leave and what came after it left it. The new run's starts hand each its task;
	// whether a start brings a stopped claim back is supervise's table to decide, and a start
	// resumes the recorded session, or a new one once supervise's Machine.forgetSession has dropped
	// it. These starts ask for no new session themselves, and decide nothing: none of them
	// launches again the agent of a claim held for OpenAI's cyber-policy refusal
	// (supervise.ErrHeldForCyberPolicy), and the outbox's refusal of the start of its phase holds
	// the issue for the architect above it (the daemon outbox's heldStartRefused), whose retry
	// starts that agent again in a new session.
	checks, err := e.store.ProductionChecksOf(ctx, tx, child.Key)
	if err != nil {
		return err
	}
	for i := range checks {
		if !checks[i].Open() {
			continue
		}
		if err := e.markCheckClosed(ctx, tx, &checks[i], record.CheckNotRun); err != nil {
			return err
		}
	}
	if err := e.store.ClearGeneration(ctx, tx, child.Key, e.now()); err != nil {
		return err
	}
	if err := e.enterChild(ctx, tx, *root, fact, next, recordedResolution(child)); err != nil {
		return err
	}
	return e.notice(ctx, tx, child.Key, ChildReenteredNotice(child.Key, root.Key))
}

// enterChild records a todo child under root's live tree, admitted at generation on the
// repository at names, at its default branch and with GitHub's spelling of it. A re-entered child
// keeps the repository it was first recorded with, but not the mark of a run it had before
// (record.Issue.TakenBack): the run this opens is a new one, which has stood in no phase yet, and
// a blocker it now waits on holds it like any other child's.
//
// It is the one door every child enters its tree through — the architect's release_children, a
// person's move of a child to todo, and a rerun or reopen through ReenterChild — and it starts
// nothing. A child is an issue the admission cap counts now, so what starts its first phase is
// its slot: it waits at phase.Admitted in the waiting line (record.Waiting), behind its tree's
// design gate and behind whatever unfinished Dispatch issue it names, and admission's promotion
// starts it (gateOpened) when the line reaches it. The gate and the blockers are therefore read
// where the line is read, once for the whole project, rather than here once per child.
//
// A child entering beneath an ancestor the daemon has already accepted waits while that ancestor
// stands in retro or merging before anything starts it (record.AcceptedAncestor, which the
// waiting line reads),
// and the architect above the child is told so here. The two doors an agent uses refuse such a
// child outright (api's CHILD_UNDER_ACCEPTED_PARENT), so what reaches this is a person's own move
// to todo, which the daemon records rather than undoes: the record is the honest one, and the
// notice says what the daemon will and will not do with it.
//
// It draws nothing: the one entry the daemon holds no record of the child before is
// recordChild's, which draws that child's issue-unit experiments in this same transaction once
// this has recorded it.
func (e *Engine) enterChild(ctx context.Context, tx pgx.Tx, root record.Issue, fact intake.DispatchIssue, generation uint64,
	at Resolution) error {
	parentKey := fact.Parent
	child := record.Issue{Key: fact.Key, Tree: root.Tree, Project: root.Project, Title: fact.Title, Parent: &parentKey, Phase: phase.Admitted,
		Generation: generation, Status: fact.Status, Rank: fact.Rank, Priority: fact.Priority, Resolved: fact.Resolved, HandedOver: fact.HandedOver, LastDispatchSeq: fact.Seq, DispatchStatus: fact.Status,
		Repo: at.Repo, DefaultBranch: at.DefaultBranch, GitHubRepo: at.GitHubRepo}
	if err := e.WriteIssue(ctx, tx, child); err != nil {
		return err
	}
	ancestor, err := record.AcceptedAncestorOf(ctx, e.store, tx, root.Tree, child.Key, parentKey)
	if err != nil || ancestor == "" {
		return err
	}
	return e.notice(ctx, tx, child.Key, ChildUnderAcceptedParentNotice(child.Key, ancestor))
}

// drawExperiments draws every issue-unit experiment the project declares (Config.Experiments)
// for issue and records the answers against its own key, in the caller's transaction
// (experiment.DrawAndRecord): one answer per experiment, under the mode the project declares it
// at and at this engine's clock. The tree-unit experiments of the tree the issue stands in are
// no part of this: they are drawn once for the whole tree, when the tree is opened (admit's
// promoteHolds).
func (e *Engine) drawExperiments(ctx context.Context, tx pgx.Tx, issue string) error {
	return experiment.DrawAndRecord(ctx, tx, e.store, issue, e.cfg.Experiments, config.ExperimentIssue, e.cfg.ExperimentSource, e.now())
}

// leftItsPhase says whether role's completion of a phase its issue has already left is recorded
// rather than refused. ranUnreported decides whether this run still owes its report.
// The daemon may have moved the issue on without this completion, such as on a review event.
// Its handoff gives the next phase findings a refusal would lose. Five completions remain refused:
// one from a role whose handoff already stands, which reports a phase it finished a second time;
// one from a role the daemon never started on this issue, which has held no phase to report;
// any while no role works the issue's phase - on the backlog, closed, held or waiting on its
// children - which is past recording work at all; any while it stands at admitted; and one of a
// phase whose entry was raised before the issue last moved into awaiting_children
// (record.Issue.ChildrenWaitedAt), which outlives the wait: the wait stopped that phase, and the
// return starts the work again on an entry of its own (dispatch://INFRA-3320 R10; ranUnreported
// reads it). Admitted is the architect's own phase (phase.RoleFor), which completes nothing
// (handoff), and it comes before every phase a worker works, so no worker's phase there is one the
// issue moved on from: the guard keeps the refusal a completion at admitted had while no role was
// named for that phase. The two holds of an owner's plan are read where their
// issue stands (record.StandsAt), so each report of a run an event ended before the hold is taken
// as it is without the hold. A parent held for its children stands at its retro: the pass of its
// last step moved it there in place of the retro, as an approval moves an issue into accepting, so
// the report of the run that pass ended is taken as it is there. A task its owner stopped stands at
// the step the stop ended: the report of a run that left that phase earlier - a review the review
// event moved back to implementing before the stop - is taken as it is there, and the report of the
// role that works the phase it stands at is none of a phase that role has left, so it is answered
// as the run the stop ended (pastReportOf). A task with plan rows takes it the same way, whatever
// step it stands at now; the test of the run itself is ranUnreported.
func (e *Engine) leftItsPhase(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, token claim.Token) (bool, error) {
	at := issue.Phase
	if record.HeldForChildren(issue) || record.HeldFor(issue, record.HoldTaskStopped) {
		at = record.StandsAt(issue)
	}
	if at == phase.Admitted || phase.LeftBy(role, at) == "" {
		return false, nil
	}
	// No role works a phase no step runs at, and a role that works one there, the step its stopped
	// task stood at among them (rolesAt), has left no phase.
	works, err := rolesAt(ctx, e.store, tx, issue, at)
	if err != nil || len(works) == 0 || slices.Contains(works, role) {
		return false, err
	}
	return e.ranUnreported(ctx, tx, issue, role, token)
}

// ranUnreported says whether role started its step in this run and has not reported it.
// EntryTask is the durable start: enter writes it with the entry, generation clears remove it,
// and a carried run keeps it. Entry alone survives a clear, and a claim can exist only for a
// production check's look. Neither proves a started step. A completion without a token reports
// no run. The first late report is recorded by leftItsPhase, or refused and spent by lateReport
// when the task's stop ended that started step.
func (e *Engine) ranUnreported(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, token claim.Token) (bool, error) {
	if token == "" {
		return false, nil
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, role)
	if err != nil || row.Completed() {
		return false, err
	}
	if issue.ChildrenWaitedAt != nil && row.EntryRaisedAt.Before(*issue.ChildrenWaitedAt) {
		return false, nil
	}
	return row.EntryTask != "", nil
}

// pastCompletion answers role's completion of reported, a phase issue no longer stands in, in one
// of three ways: now, the refusal handoff answers at once, where the completion is of no run it
// records or refuses as late (NotCurrentPhase); late, the refusal handoff answers after the
// lingering tree's, for a task with plan rows whose stop ended the step's run (lateReport); or
// neither, where handoff records the completion of a run the daemon moved the issue on from
// without it (leftPhaseReported), on its own row, moving nothing, with or without plan rows.
func (e *Engine) pastCompletion(ctx context.Context, tx pgx.Tx, s TaskScope, fact intake.HandoffComplete, reported phase.Phase) (now, late *intake.Refusal, err error) {
	issue := s.issue
	past, err := e.pastReportOf(ctx, tx, s, fact, reported)
	if err != nil {
		return nil, nil, err
	}
	if late, err = e.lateReport(ctx, tx, s, fact, reported, past); err != nil {
		return nil, nil, err
	}
	if past != leftPhaseReported && late == nil {
		return intake.NotCurrentPhase(fact.Role, issue.Phase, issue.Key), nil, nil
	}
	return nil, late, nil
}

// returnToOf is the place of the earlier step a failed step's report names as the one its task
// returns to (fact.ReturnTo), the zero place where it names none, or the refusal of the step it
// names (returnStep). It is judged against the place of the step that reports: where the task
// stands for a current completion, and the place of the role's own step for a role that has left
// it (Walk.placeOfRole), so of two steps that run as step, the reporting role's own. It is judged
// before anything is written, so a refusal records nothing, its handoff record among it, and the
// agent reports again.
func (e *Engine) returnToOf(s TaskScope, fact intake.HandoffComplete, reported phase.Phase, current bool) (record.TaskPlace, *intake.Refusal, error) {
	if fact.ReturnTo <= 0 {
		return record.TaskPlace{}, nil, nil
	}
	issue, w := s.issue, s.walk
	site := standingIn(reported)
	if !current {
		place, _ := w.placeOfRole(fact.Role)
		site = placeNamed(place, reported)
	}
	target, why := returnStep(issue, w, site, fact.ReturnTo, failReport)
	if why == "" {
		return target, nil, nil
	}
	return record.TaskPlace{}, refused("RETURN_REFUSED", why).Refusal, nil
}

// recordStepReview writes the commit a completion of a step of the type review that runs as step
// read onto the role's phases row (record.PhaseRow.ReviewedHead), so the task of the step's next
// run names it (namingTheReviewRound), as the reviewer's next round names the commit its own
// review read. A role runs at most one step of an issue, so its row's commit is its step's, and
// the clear of a generation empties it with the reviewer's (record.Store.ClearGeneration). It
// writes only for a completion of the entry that stands, by equality of an entry number and no
// time compared: row, the role's phases row the completion read and wrote in its own transaction
// before the move raises any entry, holds the task of an entry (EntryTask, which the clear of a
// generation empties and only a raise writes again) and stands on the entry the completion carries
// (fact.Entry, the one value the entry rule reads). A completion the route read before a reset of
// the tree's generation and the engine applies after it so writes no commit of the generation
// before into the new one, whether or not the promotion raised the entry since. A row a start ran
// stands on 1 or above, so a completion of entry 0 writes none. Otherwise it writes nothing: the
// commit stays where it stood, and the next run reads more. A step of another type writes none,
// and nor does a completion naming no commit.
func (e *Engine) recordStepReview(ctx context.Context, tx pgx.Tx, s TaskScope, row record.PhaseRow, fact intake.HandoffComplete) error {
	if fact.Commit == "" || row.EntryTask == "" || fact.Entry != row.Entry {
		return nil
	}
	w := s.walk
	step, ok := w.planStepAt(w.Place)
	if !w.Stored || !ok || claim.Role(step.Role) != fact.Role || step.Type != plan.TypeReview {
		return nil
	}
	row.ReviewedHead = fact.Commit
	return e.store.PutPhase(ctx, tx, row)
}

func (e *Engine) handoff(ctx context.Context, tx pgx.Tx, fact intake.HandoffComplete) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	// Every completion names a run: the route reads it from the claim, or, for a claim serving
	// none, from the task the claim is holding when that task is of the issue's own current
	// generation and phase, and it refuses a completion it can attribute to no run, so a fact
	// reaching here always carries one.
	if fact.Generation != issue.Generation {
		return refused("HANDOFF_STALE_GENERATION", fmt.Sprintf("%s is on generation %d and this completion is generation %d's; the run it reports is over",
			issue.Key, issue.Generation, fact.Generation)), nil
	}
	// phase.RoleFor(accepting) is the architect, which accepts with sign_off: no completion of its own
	// moves an issue, so an architect's is refused before the phase gate would pass it.
	if fact.Role == claim.RoleArchitect {
		return refused("ACCEPTANCE_IS_SIGN_OFF", fmt.Sprintf("the architect accepts %s with sign_off, not handoff_complete; this completion changed nothing", issue.Key)), nil
	}
	// A completion is of one task's step (completionScope), whatever the plan's count of tasks:
	// the rest of this handler reads the copy of the issue read for that task, which stands in its
	// own phase, and moves that task alone. A completion of an agent that runs no step of the plan
	// is of no phase of it.
	s, ok, err := e.completionScope(ctx, tx, *issue, fact)
	if err != nil {
		return intake.Result{}, err
	}
	if !ok {
		return intake.Result{Refusal: intake.NotCurrentPhase(fact.Role, issue.Phase, issue.Key)}, nil
	}
	view := s.issue
	issue = &view
	// A role can still be mid-turn in a phase the issue has left: the review event moves reviewing
	// on, so a reviewer's own request for changes starts the implementer before the reviewer's
	// handoff lands. Its completion is recorded on its own row and moves nothing (leftItsPhase),
	// and the handoff it names is the report of the run of its step the move ended
	// (reportEndedStep).
	//
	// reported is the phase this completion is of (phase.Of): the issue's own, or, for a role the
	// issue has moved on without, the phase that role has left, which pastCompletion answers. At
	// step, a completion is current only from the role of a running step there
	// (record.Issue.RunningAt): two steps of roles the deployment defines both run as step, and the
	// other's report is of a step the task has left.
	reported := phase.Of(fact.Role, issue.Phase)
	current := reported == issue.Phase && (issue.Phase != phase.Step ||
		slices.ContainsFunc(issue.RunningAt(phase.Step), func(step record.RunningStep) bool { return step.Role == fact.Role }))
	var late *intake.Refusal
	if !current {
		var now *intake.Refusal
		if now, late, err = e.pastCompletion(ctx, tx, s, fact, reported); err != nil || now != nil {
			return intake.Result{Refusal: now}, err
		}
	}
	// A worker still in its turn when its tree's root closed can yet complete; linger holds the
	// member where it stood (record.TreeLingers), so the completion records nothing.
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return intake.Result{}, err
	}
	if lingers {
		return refusedLingering(*issue, fmt.Sprintf("the %s's completion of phase %s", fact.Role, reported)), nil
	}
	if late != nil {
		if err := e.spendLateHandoff(ctx, tx, fact); err != nil {
			return intake.Result{}, err
		}
		return intake.Result{Refusal: late}, nil
	}
	returnTo, refusal, err := e.returnToOf(s, fact, reported, current)
	if err != nil || refusal != nil {
		return intake.Result{Refusal: refusal}, err
	}
	row, err := e.stepRow(ctx, tx, s, fact.Role)
	if err != nil {
		return intake.Result{}, err
	}
	pr, err := e.taskPullRequest(ctx, tx, s)
	if err != nil {
		return intake.Result{}, err
	}
	// The route judged the entry this completion is of on its own read of the role's row, before
	// this lock (api's answerCompletion). An entry raised between that read and this transaction —
	// a move back and the phase entered again, or a re-admission's promotion — would otherwise take
	// the old turn's report as the new entry's: the transition below acts on the row as it stands
	// now, a step, a retro and a merge end with no record that says which entry's work a completion
	// is (HANDOFF_NOT_NEW holds only the phases with one), and a completion of a phase the role has
	// left since would be recorded on the row over the run it stands on (leftItsPhase). So the
	// route's one rule (intake.EntryFence) is judged again here, on the row and the pull request
	// this transaction read, before anything is written, and its refusal is left unspent, as the
	// route's writes nothing, so the same report applies once it is the work of the entry that
	// stands.
	if refusal := intake.EntryFence(*issue, fact.Role, fact.Entry, row.Entry, row.EntryMovedBack, fact.Commit, pr, nil); refusal != nil {
		return intake.Result{Refusal: refusal, Unspent: true}, nil
	}
	// A completion of the phase the issue stands in that is at the head a merge forward replaced
	// reports code the pull request no longer stands on, and one that names no head once such a
	// merge landed says nothing about which head it judged: each is refused before anything of it
	// is recorded (mergedForwardRefusal). A completion of a phase the issue has left is not
	// fenced: it moves nothing, and its handoff is the report of the work the role did before the
	// move, which the next phase reads (leftItsPhase) and no later completion can make again,
	// since the review a landed merge forward asks of the reviewer is made with none
	// (record.MergeForwardReview).
	if current {
		if refusal, fenced := mergedForwardRefusal(*issue, fact.Role, reported, fact.Commit, pr); fenced {
			return refusal, nil
		}
	}
	if word, _, ok := phase.Handoff(reported); ok {
		// A phase with a handoff ends with a Dispatch handoff record, which the route read back
		// and held to this issue, the phase's word, the reported version and the claim's session:
		// the record the role named for its previous phase means this phase wrote none.
		if fact.HandoffRecord == "" {
			return refused("HANDOFF_RECORD_REQUIRED", fmt.Sprintf("the %s's completion of phase %s of %s names no handoff record: write it with `dispatch handoff-write --issue %s --phase %s --body-file <file>`, then complete again; this completion changed nothing",
				fact.Role, reported, issue.Key, issue.Key, word)), nil
		}
		if fact.HandoffRecord == row.LastHandoff {
			return refused("HANDOFF_NOT_NEW", fmt.Sprintf("the %s named handoff record %s for its previous phase of %s; write this phase's handoff with `dispatch handoff-write --issue %s --phase %s --body-file <file>` before completing",
				fact.Role, fact.HandoffRecord, issue.Key, issue.Key, word)), nil
		}
		row.LastHandoff = fact.HandoffRecord
	}
	// The rating of the plan is judged here (planRatingRefusal), past every refusal above, so a
	// held issue's, a stale run's or a lingering tree's completion is told that first, and before
	// anything is written. It reads the issue this transaction read under the fact's lock, which is
	// the read the retro ends on below (TriggerRetroCompleted): the route's read came before that
	// lock, and the issue can move between the two, as a retried hold does. Its refusal writes
	// nothing and leaves the fact unspent, so the same completion, corrected, applies under its key.
	if refusal := planRatingRefusal(fact, current && issue.Phase == phase.Retro, reported); refusal != nil {
		return intake.Result{Refusal: refusal, Unspent: true}, nil
	}
	// The round as the completion found it is what a notice is deduped against (settleRound): a
	// round already stuck at this head before the completion was told then, and the completion
	// adds nothing to it; a round the completion itself leaves stuck - one it completed with no
	// decision, or with an approval of other code - was not stuck before it, and is told.
	found := e.reviewRound(*issue, row, pr)
	row.Claim, row.HandoffCommit, row.Verdict, row.Summary, row.CompletedAt = fact.Claim, fact.Commit, fact.Verdict, fact.Summary, e.now()
	row.HandoffRecord, row.HandoffVersion = fact.HandoffRecord, fact.HandoffVersion
	if err := e.store.PutPhase(ctx, tx, row); err != nil {
		return intake.Result{}, err
	}
	// Two of the six movements are this one write (the rule in dispatchIssue): the handoff the
	// role recorded, and the completion that reports it. Every refusal above returned before it,
	// so a completion that changed nothing marks nothing, and the completions that move the issue
	// no further than this row — a tester reporting neither verdict, an implementer whose pull
	// request has not been recorded yet, a role reporting a phase the issue has already left —
	// are movements all the same, which is why the mark is here and not only in the transitions
	// below.
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return intake.Result{}, err
	}
	if !current {
		return intake.Result{}, e.reportEndedStep(ctx, tx, s, reported, fact.Role, fact.HandoffRecord)
	}
	switch issue.Phase {
	case phase.Planning:
		return intake.Result{}, e.transition(ctx, tx, s, TriggerPlannerCompleted, record.TaskPlace{}, row, pr, "")
	case phase.Implementing:
		if pr == nil {
			return intake.Result{}, nil
		}
		return intake.Result{}, e.implementationReady(ctx, tx, s, row, pr)
	case phase.Testing:
		if fact.Verdict == "fail" {
			return intake.Result{}, e.transition(ctx, tx, s, TriggerTesterFailed, returnTo, row, pr, "")
		}
		if fact.Verdict == "pass" {
			return intake.Result{}, e.transition(ctx, tx, s, TriggerTesterPassed, record.TaskPlace{}, row, pr, "")
		}
	case phase.Reviewing:
		_, err := e.settleRound(ctx, tx, s, row, pr, found, byCompletion)
		return intake.Result{}, err
	case phase.Step:
		// A role the deployment defines reports pass or fail and writes no handoff record; its fail
		// goes back to the step its returnTo names, or to the one its plan reads (returnTarget). A
		// step of the type review keeps the commit its completion read first, before the move
		// raises any entry (recordStepReview).
		if err := e.recordStepReview(ctx, tx, s, row, fact); err != nil {
			return intake.Result{}, err
		}
		switch fact.Verdict {
		case "fail":
			return intake.Result{}, e.transition(ctx, tx, s, TriggerStepFailed, returnTo, row, pr, "")
		case "pass":
			return intake.Result{}, e.transition(ctx, tx, s, TriggerStepPassed, record.TaskPlace{}, row, pr, "")
		}
	case phase.Retro:
		// The completion that ends the retro carries the rating its agent gave the plan, which
		// planRatingRefusal above took on no other completion, and a plugin before the rating
		// sends none, so its retro writes no rating.
		if err := e.transition(ctx, tx, s, TriggerRetroCompleted, record.TaskPlace{}, row, pr, ""); err != nil || fact.PlanRating == nil {
			return intake.Result{}, err
		}
		return intake.Result{}, e.rateRetro(ctx, tx, s, *fact.PlanRating)
	}
	return intake.Result{}, nil
}

// claimFailed holds the step whose phase worker's claim failed — a budget ran out — and tells the
// architect of the hold and of the worker-died that comes with it. The hold is of the task of the
// claim's agent's step (scopeOfAgent, record.Store.HoldTask): an issue whose plan holds several
// tasks is held only where no other task runs, and the held notice names the task and its step
// (heldSummary). The controller is told neither: the hold is the architect's to retry or
// escalate, and only the escalation reaches the controller (retryOrEscalate). The tree's architect
// failing holds nothing, since a phase is its worker's; it is told as a worker-died of the
// architect, lingering or not, and while the root is not done the controller hears it from a row
// of its own, because every notice of the tree that no child's own architect takes reaches that
// architect and nobody inside the tree is left to act on its own. A root that is done is parked,
// lingers or is closed, and the controller does nothing for it, so that copy is not written
// (#4065). That is why the architect's branch comes before the lingering tree's guard. The row
// sent to the architect's own role finishes undelivered once its claim has failed (the daemon's
// notice executor).
//
// The hold and the worker-died beside it name the failure the fact carries: the one word for
// what failed the claim, the sentence written beside it and the four counts the failure left,
// each against the limit it ran out at (noticeFailure). The worker-died of a role that does not
// work the issue's phase names it too, since that role's architect decides whether to start it
// again on the same evidence. The tree architect's own worker-died carries no failure record, so
// its reason carries the sentence instead, with the pod end when the claim holds one, or else,
// for a claim that failed because its process died, the first line of the runtime's account of
// that death (deathReason): it is the one notice of that failure the architect and the
// controller are sent.
//
// A child's own architect works no phase but its child's acceptance (accepting), where no worker
// runs: the daemon starts it with the child's first phase (StartFirstPhase). Its claim failing
// holds nothing, even at accepting, since its branch below comes before any read of the phase, and
// is told as a worker-died of the architect on the child, which reaches the architect above it,
// since the owner walk starts at the child's parent for an architect's own worker-died
// (record.OwningArchitect). That architect takes the child's notices from then on, because the
// failed claim no longer holds its role, until the child's architect runs again: the daemon starts
// it again at the child's next phase start (start), and so do a re-admission of the tree while the
// child stands in a phase (admit's startMidPhaseMembers), an operator's retry and the retry of an
// architect above it through the agent door (api's retryAgent). The worker-died names the failure,
// as a finished role's does, so the architect above it reads why the child's architect stopped; no
// controller copy goes with it, since an architect inside the tree is left to act on it. A child
// that left the workflow or a member of a lingering tree tells nobody, as below.
//
// A phase role that finished its phase stays live until its issue closes, so its claim can fail
// while another role works the issue's phase, or while the issue waits on a person: the architect
// is told in a worker-died of that role, its reason saying the role does not work the issue's phase,
// and nothing is held, since the phase is not that role's. A later start of the role relaunches its
// session (the outbox executor's retry of a failed claim). An issue that left the workflow
// suspended its claims (a parked child) or closed them (a child closed as done), and a lingering
// tree holds its members where they stood, so neither tells anyone.
//
// The checking role of a closed issue failing inside a look of its production check ends that look
// as a report of pending would (endFailedLook): a done issue has no phase to hold, and the look's
// next wake starts the claim again, a backoff out that doubles with each failed look in a row. A
// failure of that role between looks counts the same (failedBetweenLooks), and either is told.
//
// A claim that failed on OpenAI's cyber-policy refusal (supervise.CauseCyberPolicy) is the
// exception to each "starts it again" above: no start of the daemon's own launches its agent again
// (supervise.ErrHeldForCyberPolicy), so a notice that would promise one says that the role does
// not start again by itself instead (noStartOfItsOwn), and nothing of the cause or of who brings it
// back: agents are told what they do, which is the same for every failure. A held phase is held
// as for any cause, and its retry, the architect's or a person's decision, starts the agent in a
// new session. A role outside its issue's phase holds nothing now, and the start of its phase,
// should the issue come to it, holds the issue then (the daemon outbox's heldStartRefused).
func (e *Engine) claimFailed(ctx context.Context, tx pgx.Tx, fact intake.ClaimFailed) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if claim.IsTreeArchitect(fact.Role, issue.Key, issue.Tree) {
		died := record.Notice{Kind: "worker-died", Role: fact.Role, Phase: issue.Phase, Reason: deathReason(fact, false)}
		if issue.Phase == phase.Done {
			return intake.Result{}, e.notice(ctx, tx, issue.Key, died)
		}
		return intake.Result{}, e.noticeWithController(ctx, tx, issue.Key, died)
	}
	if fact.Role == claim.RoleArchitect {
		if !record.InWorkflow(*issue) {
			return intake.Result{}, nil
		}
		if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
			return intake.Result{}, err
		}
		again := noStartOfItsOwn(fact)
		if again == "" {
			again = fmt.Sprintf("the daemon starts it again at %s's next phase start", issue.Key)
		}
		return intake.Result{}, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "worker-died", Role: fact.Role, Phase: issue.Phase, Failure: noticeFailure(fact),
			Reason: fmt.Sprintf("%s's own architect failed; its notices come to you until it runs again, and %s", issue.Key, again)})
	}
	// The failure of the role an open check records ends the look of its closed issue's production
	// check, as its bound would, for each open check of the issue whose look is open, and counts it a
	// failed look, so the next look is named a backoff out that doubles with each failed look in a
	// row (endFailedLook). Which role that is each check's own row says (record.ProductionCheck.Role,
	// the role it opened under), never the setting as it stands now, so a check opened before the
	// project changed its `production_check` still ends its look on its own role's failure. A
	// failure after the look ended, of an open check with none open, counts the same and names the
	// next look the same backoff out, from the failure, where no later wake stands
	// (failedBetweenLooks). Unlike an ordinary pending report, nobody remains to report the look or
	// wait for the next wake, so the architect is told now, in either case and once for each check,
	// with the cause, how the last process ended, and when the next look retries the claim and how
	// (checkRoleFailedReason). An issue in the workflow (one reopened inside its linger with a look
	// still open at the reopen) takes the live-phase path below instead, where the failure holds the
	// phase and is told; its stale look ends at the look's own bound (check_look), which
	// endLookPending keeps for the run.
	if !record.InWorkflow(*issue) {
		checks, err := e.store.ProductionChecksOf(ctx, tx, issue.Key)
		if err != nil {
			return intake.Result{}, err
		}
		told := false
		for i := range checks {
			check := &checks[i]
			if !check.Open() || fact.Role != check.Role {
				continue
			}
			lost := fmt.Sprintf("production check look %d lost its %s", check.Looks, check.Role)
			if check.LookOpen {
				err = e.endFailedLook(ctx, tx, check)
			} else {
				lost = fmt.Sprintf("production check of %s lost its %s after look %d ended", issue.Key, check.Role, check.Looks)
				err = e.failedBetweenLooks(ctx, tx, check)
			}
			if err != nil {
				return intake.Result{}, err
			}
			if err := e.notice(ctx, tx, issue.Key, record.Notice{
				Kind: "worker-died", Role: fact.Role, Reason: lost + ": " + checkRoleFailedReason(fact, *check),
			}); err != nil {
				return intake.Result{}, err
			}
			told = true
		}
		if told {
			return intake.Result{}, nil
		}
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers || issue.Phase == phase.Done {
		return intake.Result{}, err
	}
	// The failure is of the step the claim runs (scopeOfAgent), whatever the plan's count of tasks:
	// the copy of the issue read for that step's task, which stands in that task's own phase and
	// hold. A claim that runs no step of the plan, and stands at no held or stopped one, works none
	// of its phases, and the notice names the issue's own.
	s, ok, err := e.scopeOfAgent(ctx, tx, *issue, fact.Role, fact.Claim)
	if err != nil {
		return intake.Result{}, err
	}
	view := *issue
	if ok {
		view = s.issue
	}
	working := view.WorkingPhase()
	failure := noticeFailure(fact)
	works := false
	if ok {
		if works, err = worksAt(ctx, e.store, tx, view, working, fact.Role); err != nil {
			return intake.Result{}, err
		}
	}
	if !works {
		reason := fmt.Sprintf("the %s does not work %s's phase %s; nothing is held", fact.Role, issue.Key, working)
		return intake.Result{}, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "worker-died", Role: fact.Role, Phase: view.Phase, Failure: failure,
			Reason: joinReason(reason, noStartOfItsOwn(fact))})
	}
	if view.Phase == phase.Held {
		return intake.Result{}, nil
	}
	from := view.Phase
	// The hold keeps the task's place (moveTask): the task does not move, and held overlays the
	// phase its place derives, which Hold.From names. The task is held with it, for the failed
	// claim (record.Store.HoldTask), so a plan of several tasks holds this task alone and the
	// others go on: the issue is written where its tasks put it (putScope), held only where no
	// other task runs.
	s.issue.Phase, s.issue.Hold = phase.Held, &record.Hold{From: from}
	if s.walk.Stored {
		if err := e.store.HoldTask(ctx, tx, s.issue.Key, s.issue.Generation, s.walk.Task, ""); err != nil {
			return intake.Result{}, err
		}
		s.walk.State, s.walk.HoldReason = record.TaskHeld, ""
	}
	if _, err := e.putScope(ctx, tx, &s); err != nil {
		return intake.Result{}, err
	}
	// A claim that failed takes its issue into held, which is a phase change and so a movement
	// (the rule in dispatchIssue). The early return above is what keeps it honest: an issue
	// already held writes nothing here, so a second report of the same dead claim moves the
	// tree nowhere. It matters for the measure rather than for the record: a tree stops at the
	// moment its worker dies, and marking the movement here is what makes the quiet time run
	// from that moment instead of from whatever last touched the issue before it. The hold of
	// one task of several is a movement of the same kind: that task stopped where it stood.
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return intake.Result{}, err
	}
	held := record.Notice{Kind: "held", Role: fact.Role, Phase: from, Failure: failure, Summary: heldSummary(s)}
	if err := e.notice(ctx, tx, issue.Key, held); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "worker-died", Role: fact.Role, Phase: from,
		Failure: failure, Reason: deathReason(fact, failure != nil)})
}

// heldTaskSummary is what the held notice of a failed claim of one task of a plan of several says
// of the place it holds, so its owner reads which of its tasks is held: `task <id>, step <i>`, or
// the place of the task's act (actWords): its parts, its report, its retro or its merge.
func heldTaskSummary(w Walk) string {
	if words := actWords(w.Place.Kind); words != "" {
		return fmt.Sprintf("task %s, %s", w.Task, words)
	}
	return fmt.Sprintf("task %s, step %d", w.Task, w.Place.Position)
}

// noStartOfItsOwn is the clause a worker-died notice of a claim that failed on OpenAI's
// cyber-policy refusal (supervise.CauseCyberPolicy) says in place of a start of the daemon's own
// that will not come, and "" for a fact of any other cause: a child's own architect's and a role's
// outside its issue's phase (claimFailed), and the checking role's of a production check
// (checkRoleFailedReason). It is the one place that cause is read for a notice. No start of the
// daemon's own launches such a claim's agent again (supervise.ErrHeldForCyberPolicy), and the
// clause says that and no more: it names neither the cause's mechanics nor who brings the agent
// back, since the notice goes to an agent, which acts on a failure the same way whatever failed
// it. The operator's command lives in the operator's surfaces (supervise.RetryCommand). Two
// notices carry no clause, since neither promises a start: the held phase's, whose retry the
// architect decides and which starts the agent in a new session (supervise's freshSession), and the
// tree architect's, which the controller answers by restarting the tree, whose promotion starts
// the architect's agent in a new session (StartPromotedArchitect).
func noStartOfItsOwn(fact intake.ClaimFailed) string {
	if fact.Cause != supervise.CauseCyberPolicy {
		return ""
	}
	return "it does not start again by itself"
}

// joinReason is a worker-died's reason with clause after it (noStartOfItsOwn), joined as
// deathReason joins its own parts, and either alone where the other is empty.
func joinReason(reason, clause string) string {
	switch {
	case clause == "":
		return reason
	case reason == "":
		return clause
	}
	return reason + "; " + clause
}

// deathReason is what a worker-died notice says of how a claim ended: the whole reason on the two
// paths that build none of their own — the tree architect's and the held phase's — and the start
// of the checking role's (checkRoleFailedReason), which adds what the check does next. It names
// the supervisor's own sentence (supervise.Claim.Failure, intake.ClaimFailed.Reason), then what
// ended the process: the pod end it was last observed to have died of (supervise.Claim.PodEnd),
// which is the cause no role chose — an eviction for ephemeral storage above all — or, where the
// claim holds no pod end and failed because its process died, the first line of the runtime's
// account of that death
// (supervise.Claim.LastDeathCause, intake.ClaimFailed.Death). They are joined with "; ", and any
// of them the fact does not hold is left out. The sentence is made one line (deathSentence) and
// the death's first line is taken without its log tail (deathFirstLine), and each is written
// quoted and cut to a bound (quoteBounded), so neither a provider's message nor a role's log
// writes a line of its own into the reason, nor a tab, an escape or a clause that reads as this
// function's own — `; the pod ended: Evicted: …` at the end of a provider's message stays inside
// the sentence's quotes — and the reason's size is one this package chose; the pod end is the
// kubelet's reason and message, bounded where it is built (runtime.PodEnd). All three are
// persisted on the claim, so the boot's replay of a failure no daemon applied says the same thing
// (the daemon's replayTerminal).
//
// The death is named only where there is no pod end, because on a pod the cluster ended it is
// the runtime's whole detail (sandbox's Runtime.ended): the pod's reason and the kubelet's
// message, which are the pod end, and after them a tail of the role's log and the container's.
// Its first line holds that reason and message too, so beside the pod end it would name the
// kubelet's message twice. The death begins with the verdict the runtime reached (supervise's
// deathCause), `gone` above all, so it is written after "the process was last found "
// (deathFirstLine).
//
// It is named only where the fact's cause is supervise.CauseDied, too: the death is written at
// every death of the claim and cleared by nothing (supervise's died), so a claim that died, was
// launched again and then failed for something else — its provider's refusals, its prompt
// retirements, an agent that never registered — still holds the account of the earlier death,
// and beside that failure the reason would name an end that did not cause it: an old eviction
// beside a provider error, which reads as the cluster's fault. A fact whose cause is "" or
// `unrecorded` does not say the process's death failed it either, so it names no death. The pod
// end needs no such test: each launch clears it (supervise's launchNow), so a fact that carries
// one failed with no launch since the death that wrote it.
//
// failureCarried says the notice carries a failure record beside this reason, as the held
// phase's does whenever the fact names a cause. That record's `detail` already carries the
// sentence and its `death` the runtime's account (noticeFailure), so such a reason is the pod
// end alone, and empty when there is none, since repeating either would say one thing twice.
// The tree architect's notice carries no failure record, and neither does a held phase's whose
// fact names no cause, so their reason carries the sentence and the pod end, and the tree
// architect's, where there is no pod end and its claim failed because it died, the death: without
// them the notice would say nothing of why the claim failed. A held phase's fact that names no
// cause does not name `died` either, so its reason names no death.
//
// A role that does not work the issue's phase keeps a reason of its own, the daemon's words alone,
// since what its failure means for that issue is more than this says. A checking role of a closed
// issue writes this after the look and the role it names (claimFailed), with failureCarried false
// since that notice carries no failure record, and where this says nothing, a sentence of the
// daemon's own; then the cause and the look that retries the claim.
func deathReason(fact intake.ClaimFailed, failureCarried bool) string {
	said := make([]string, 0, 2)
	if !failureCarried && fact.Reason != "" {
		said = append(said, deathSentence(fact.Reason))
	}
	switch {
	case fact.PodEnd != "":
		said = append(said, "the pod ended: "+fact.PodEnd)
	case !failureCarried && fact.Cause == supervise.CauseDied && fact.Death != "":
		said = append(said, "the process was last found "+quoteBounded(deathFirstLine(fact.Death), false))
	}
	return strings.Join(said, "; ")
}

// deathReasonCut follows the closing quote of a part of a worker-died's reason that quoteBounded
// cut, so a reader sees both where the quoted text ends and that it was cut.
const deathReasonCut = "… (cut)"

// deathSentence is the supervisor's sentence as a worker-died's reason carries it: each run of
// line breaks between two pieces of its text written as " / " and any at either end dropped, then
// quoted and cut as quoteBounded writes it. A failure an earlier daemon wrote for a run of refused
// turns holds the provider's message as the role's own plugin posted it (the turn-outcome route),
// which can run to a mebibyte over many lines; the claim and a failure record's `detail` keep it
// whole (noticeFailure). The sentence is cut at runtime.PodEndRunes runes before its line breaks
// are written, so no more of a mebibyte is read than the bound keeps.
func deathSentence(sentence string) string {
	cut := false
	if at := runeOffset(sentence, runtime.PodEndRunes); at < len(sentence) {
		sentence, cut = sentence[:at], true
	}
	return quoteBounded(strings.Join(strings.FieldsFunc(sentence, isLineBreak), " / "), cut)
}

// quoteBounded is one part of a worker-died's reason that the daemon did not write itself — the
// supervisor's sentence, the first line of the runtime's account of a death — as the reason
// carries it: quoted with %q, so a line break, a tab, an escape or a quote in it is written
// escaped and nothing in it reads as the reason's own words, and cut to runtime.PodEndRunes
// runes, the bound the pod end beside it is cut to, where it was longer or cut says it was cut
// already: the text is then cut to that bound less deathReasonCut's runes, quoted, and followed
// by deathReasonCut after its closing quote.
func quoteBounded(text string, cut bool) string {
	if !cut && runeOffset(text, runtime.PodEndRunes) == len(text) {
		return strconv.Quote(text)
	}
	keep := runtime.PodEndRunes - utf8.RuneCountInString(deathReasonCut)
	return strconv.Quote(text[:runeOffset(text, keep)]) + deathReasonCut
}

// isLineBreak is whether r ends a line where the reason is read: a line feed, a carriage return,
// a vertical tab, a form feed, a next line, or Unicode's line and paragraph separators.
func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', '\u0085', '\u2028', '\u2029':
		return true
	}
	return false
}

// runeOffset is the byte offset of the rune n runes into s, and len(s) where s holds n runes or
// fewer.
func runeOffset(s string, n int) int {
	at := 0
	for ; n > 0 && at < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[at:])
		at += size
	}
	return at
}

// deathFirstLine is the part of a death the worker-died's reason carries: its first line — the
// verdict and the start of the runtime's account — without the header of a log tail that ends
// it. The sandbox runtime ends that line with "; last lines of <slot>:", the slot of the claim's
// agent, and writes the tail of that slot's log and of the container's on the lines after it,
// among them lines an agent wrote:
// carried whole, each of those became a line of the architect's notice. The whole account stays
// on the claim (supervise.Claim.LastDeathCause), where an operator reads it, and in the `death`
// of the failure record a notice carries when the fact names a cause (noticeFailure). The line
// itself can run long — for a pod left unscheduled past the boot timeout it holds the
// scheduler's message and the pod's events joined (sandbox's Runtime.verdict) — so it is quoted
// and cut where it is written (deathReason, quoteBounded), and whatever else is in it stays
// inside the reason's one line.
func deathFirstLine(death string) string {
	first, _, cut := strings.Cut(death, "\n")
	if at := strings.LastIndex(first, "; last lines of "); cut && at >= 0 && strings.HasSuffix(first, ":") {
		first = first[:at]
	}
	return first
}

// The bounds on what an issue's architect is told about its pod's volume filling, and the two
// log lines that say which way a report went.
//
// A filling pod can report every sweep, and the notice is a wake of the architect's session.
// VolumeNoticeCap is the most an architect hears for one trimmer and one episode: three is
// enough to show a trend — it is filling, the trimmer is not keeping up, here is what is in it —
// and a fourth says nothing the third did not. Two kinds of report pass a bound
// (volumeNoticeHeldBy): one whose episode a rise raised skips the hour below and not the cap, so
// each rise episode costs up to three notices however recent the pod's last one was, and the
// report that the trimmer switched itself off skips both. Every other report of one pod waits
// VolumeNoticePodInterval from the pod's newest notice, whatever trimmer or episode wrote it, so
// an episode that ends and another that opens a minute later is still one notice an hour.
//
// Both lines begin with VolumeNoticeLinePrefix and each names the issue, the pod, the trimmer,
// the episode and the count, because the issue's production check reads them from the daemon's
// log to prove the bounds hold. They are constants so the check and this package name one
// string.
const (
	VolumeNoticeCap         = 3
	VolumeNoticePodInterval = time.Hour

	VolumeNoticeLinePrefix = "volume near full: "
	VolumeNoticeWritten    = VolumeNoticeLinePrefix + "the issue's architect was told its pod's volume is filling"
	VolumeNoticeSuppressed = VolumeNoticeLinePrefix + "a bound held the notice of a sweep that left the pod's volume above its low mark"
)

// volumeNearFull applies one sweep of an issue pod's volume as that pod's launcher reported it
// (intake.VolumeNearFull). The launcher reports only a sweep that ended with the volume still
// above its low mark, and the one report no sweep makes — the trimmer having switched itself off
// after three faults — so every fact that arrives here is one the architect may need; what this
// decides is which of them it is told, and it records the telling in the same transaction as the
// notice.
//
// An issue the daemon does not record, and one whose tree lingers after leaving the workflow,
// write nothing: there is nobody in such a tree to act, as there is nobody for a worker-died of
// a lingering member. An engine built without the pod tables records nothing either, exactly as
// it wakes nothing (Config.Pods).
//
// The bounds are read and written through the store, never kept here, because a daemon that
// restarts in the middle of a filling pod's episode would otherwise start the count over and
// tell that architect three more times — on exactly the pod that keeps reporting.
func (e *Engine) volumeNearFull(ctx context.Context, tx pgx.Tx, fact intake.VolumeNearFull) (intake.Result, error) {
	if e.cfg.Pods == nil {
		return intake.Result{}, nil
	}
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return intake.Result{}, err
	}
	report := fact.Report
	// The pod is the session's, never the report's: a launcher is authenticated for one pod and
	// cannot name another's volume (sandbox's volumeReported).
	report.PodUID = fact.PodUID
	counts, err := e.cfg.Pods.VolumeNoticeCounts(ctx, tx, issue.Key, fact.PodUID, report.Trimmer, report.Episode)
	if err != nil {
		return intake.Result{}, err
	}
	now := e.now()
	if held := volumeNoticeHeldBy(report, counts, now); held != "" {
		e.logOnCommit(ctx, VolumeNoticeSuppressed, "issue", issue.Key, "pod", fact.PodUID, "trimmer", report.Trimmer,
			"episode", report.Episode, "notices", counts.Notices, "bound", held,
			"raisedBy", report.RaisedBy, "usedBytes", report.UsedBytes, "freeBytes", report.FreeBytes)
		return intake.Result{}, nil
	}
	// The count and the notice are one write: a notice the listener cannot hand to an architect
	// that is not there is still a notice spent, and a transaction that rolls back has spent
	// none.
	notices, err := e.cfg.Pods.RecordVolumeNotice(ctx, tx, issue.Key, fact.PodUID, report.Trimmer, report.Episode, now)
	if err != nil {
		return intake.Result{}, err
	}
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: record.VolumeNearFullNotice, Role: claim.RoleArchitect,
		Reason: volumeNoticeReason(issue.Key, report), Volume: &report}); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, VolumeNoticeWritten, "issue", issue.Key, "pod", fact.PodUID, "trimmer", report.Trimmer,
		"episode", report.Episode, "notices", notices,
		"raisedBy", report.RaisedBy, "usedBytes", report.UsedBytes, "freeBytes", report.FreeBytes,
		"switchedOff", report.SwitchedOff)
	return intake.Result{}, nil
}

// volumeNoticeHeldBy is the bound that holds report back, and "" for a report that is sent. The
// order is the order the rules override each other in:
//
//   - A trimmer that has switched itself off is sent whatever the counts stand at. The
//     protection this notice exists for has stopped, on exactly the pod that has been reporting,
//     and the bounds would otherwise swallow the one report that says so.
//   - Three notices for one trimmer and episode is all an architect hears of that episode,
//     however long it runs and whatever raises a reading inside it. A new trimmer identifier is
//     a new line and starts the count over, which the key gives: a restarted roles container
//     mints one.
//   - A report whose episode a rise raised (shimwire.VolumeEpisodeRise) is sent however recent
//     the pod's last notice was, where one whose episode a recovery raised waits for the hour.
//     The launcher raises an episode that way at the start of a reported sweep, where the lower
//     of that start and the one before it stands five points or more above the episode's
//     anchor, and at no smaller climb (internal/launcher's riseAt). The anchor is the lowest
//     start of the episode's reported sweeps where no rise raised it, and the floor that raised
//     it where one did, which never moves, so until a recovery a chain whose anchor is A at its
//     first rise holds at most (100 − A) / 5 rises, rounded down: (100 − high mark) / 5 for one
//     anchored at the high mark. An anchor above 95% can never be passed, since a reading is at
//     most 100%, so no rise follows one; the bound above holds every episode to three notices
//     however it was raised. This function reads the word alone.
//   - Everything else waits an hour from the pod's newest notice, whatever trimmer or episode
//     wrote it — a recovery-raised episode included, which is a volume doing what it did before.
func volumeNoticeHeldBy(report record.VolumeNearFull, counts store.VolumeNoticeCounts, now time.Time) string {
	if report.SwitchedOff {
		return ""
	}
	if counts.Notices >= VolumeNoticeCap {
		return fmt.Sprintf("%d notices already written for this trimmer and episode", counts.Notices)
	}
	if report.RaisedBy == shimwire.VolumeEpisodeRise {
		return ""
	}
	if !counts.LastAt.IsZero() && now.Sub(counts.LastAt) < VolumeNoticePodInterval {
		return fmt.Sprintf("the pod's last notice was %s ago, inside the %s this bound gives one pod",
			now.Sub(counts.LastAt).Round(time.Second), VolumeNoticePodInterval)
	}
	return ""
}

// volumeNoticeReason is the sentence the architect reads: which pod of which issue, how full the
// volume is, what the sweep freed, why it stopped, each /proc read the kernel refused, and what
// is on the volume with the two age splits that tell a working set from cold bytes. The notice's
// own Volume member carries the same facts in a shape a reader can compute with; this is what a
// person or an agent reads in the message.
//
// It is written in the past tense, because the frame carries no time for the sweep it reports and
// a report the launcher held through an outage reaches the daemon after it (launcher's deliver):
// the volume was above its low mark when the sweep ended, which says nothing of it now. The freed
// bytes are the sweep's first reading's used bytes less its last's (launcher's sweep), so they are
// negative when the volume's used bytes grew; such a sweep is told as the volume growing by that
// many bytes while it ran, and never as a negative number freed.
//
// Everything that comes from the pod rather than from this package is rendered with %q: a
// name two levels under /legion, a directory's or a file's, is one a role of that pod chose —
// /legion/scratch is writable by the role uid — and the sentence is read by the architect's own
// agent session, so a newline in such a name must be text inside the sentence and not a line of
// its own, with nothing to mark it as data. The trimmer's identifier and what raised its episode
// are rendered the same way, although the launcher mints the one and takes the other from one of
// three constants: the frame's own rule checks only that the identifier is present and reads
// neither one's contents (shimwire.LauncherVolumeReport.Validate), so the sentence's safety rests
// on this daemon rather than on the sender being Legion's own launcher. The structured Volume
// member keeps the raw name, which is data a reader computes
// with rather than prose.
//
// How many entries each list carries is the launcher's bound (internal/launcher's bound), and
// where that bound left entries out the sentence says so: a reader must not take a cut list for
// the whole of what the trimmer measured.
//
// A size is what the launcher's walk measured (internal/launcher's measureTree): the apparent
// size of every regular file at or under its path, counted once for each name the walk met it
// under. So a sparse file counts at its length rather than at the blocks it holds, a file linked
// under two names — bun links its install cache into a workspace's node_modules — counts under
// both, and the sizes can sum past the used bytes this same sentence gives. The sentence says
// so in one clause after the last size, once however many sizes it carries, and not at all where
// the report carries none.
//
// The report of a switched-off trimmer is made by no sweep (launcher's switchOff), so its
// sentence names no bytes freed and no episode: its freed bytes are zero because nothing swept,
// and its episode and what raised it are filled in for the frame's own rule where the trimmer
// never raised one, so the sentence cannot tell an episode the trimmer raised from one the frame
// was given. The structured Volume member still carries all three. Its used and free bytes are
// the trimmer's last reading of the volume, whenever that was, and both zero where it made none,
// which is how the sentence tells the two apart: the frame carries no mark of its own for a
// trimmer that made no reading, and a reading of a mounted volume does not have both at zero.
//
// The opening clause of a switched-off trimmer counts nothing, and what the report carries does
// the counting: what stops the trimmer is three recovered panics in a row of one kind of its
// work, its readings of the volume, a row that also counts its wait for settings and its interval
// clock with the line that starts it, or its sweeps and their reports (volumeFaultLimit and
// faultRow, launcher's volumetrim.go), which the stop reason the launcher sends says and the
// sentence quotes, while Faults holds the newest three faults of both rows, those recovered since
// the trimmer's last report and those of a report it still held, which goes into this one
// (launcher's switchOff), so an earlier one it left out makes four.
func volumeNoticeReason(issue string, report record.VolumeNearFull) string {
	var out strings.Builder
	switch {
	case report.SwitchedOff:
		fmt.Fprintf(&out, "the cache trimmer on %s's pod %s has switched itself off and frees nothing more until the pod's roles container restarts (trimmer %q)",
			issue, report.PodUID, report.Trimmer)
		if report.UsedBytes == 0 && report.FreeBytes == 0 {
			out.WriteString(": it made no reading of the volume")
		} else {
			fmt.Fprintf(&out, ": its last reading of the volume was %d bytes used, %d free", report.UsedBytes, report.FreeBytes)
		}
	default:
		fmt.Fprintf(&out, "the volume %s's roles share on pod %s was still above its low mark when the sweep ended (trimmer %q, episode %d raised by %q): %d bytes used, %d free, ",
			issue, report.PodUID, report.Trimmer, report.Episode, report.RaisedBy, report.UsedBytes, report.FreeBytes)
		if report.FreedBytes < 0 {
			// The magnitude as uint64, which holds it for every int64, the least one included.
			fmt.Fprintf(&out, "and the volume grew by %d bytes while the sweep ran", uint64(-report.FreedBytes))
		} else {
			fmt.Fprintf(&out, "%d freed by the sweep", report.FreedBytes)
		}
	}
	if report.StopReason != "" {
		fmt.Fprintf(&out, "; it stopped because %q", report.StopReason)
	}
	if len(report.RefusedReads) > 0 {
		fmt.Fprintf(&out, "; the kernel refused the /proc of %s", quotedList(report.RefusedReads))
		if report.RefusedReadsOmitted > 0 {
			fmt.Fprintf(&out, " and %d more this report leaves out", report.RefusedReadsOmitted)
		}
	}
	for _, size := range report.Sizes {
		if size.Refused == "" {
			fmt.Fprintf(&out, "; %q holds %d bytes (%d older than an hour, %d older than a day)",
				size.Path, size.Bytes, size.BytesOverHour, size.BytesOverDay)
			continue
		}
		// A size the walk could read only in part — the kernel refused a read under it, or it
		// holds a directory deeper than the walk descends — is what the walk could read and no
		// more, so it is told as a lower bound with the first read the walk did not make, which
		// is where a reader looks for the rest (record.VolumeSize).
		fmt.Fprintf(&out, "; %q holds at least %d bytes (%d older than an hour, %d older than a day, of what the walk could read): the walk could not make %d of its reads under it, the first at %q",
			size.Path, size.Bytes, size.BytesOverHour, size.BytesOverDay, size.Refusals, size.Refused)
	}
	if len(report.Sizes) > 0 {
		out.WriteString("; each size is the apparent size of the files at or under its path, with a file linked under more than one name counted under each name, so the sizes can sum past the bytes the volume uses")
	}
	if report.SizesOmitted > 0 {
		fmt.Fprintf(&out, "; %d more sizes are left out of this report", report.SizesOmitted)
	}
	// Two facts and not one: a walk that was cut ended early, and a target the walk measured
	// none of, or the volume root or an immediate child of it that it did not list in full, may
	// be one whose read the kernel refused, which cuts no walk — a directory it could not open or
	// list, a file whose lstat was refused — and is told with the read the kernel refused
	// (record.VolumeNearFull, record.VolumeMissing).
	if report.SizesPartial {
		out.WriteString("; the size walk was cut before it reached every target")
	}
	if len(report.SizesMissing) > 0 {
		fmt.Fprintf(&out, "; the size walk measured none of %s", missingList(report.SizesMissing))
		if report.SizesMissingOmitted > 0 {
			fmt.Fprintf(&out, " and %d more it leaves out", report.SizesMissingOmitted)
		}
	}
	if !report.SizesAt.IsZero() {
		fmt.Fprintf(&out, "; measured at %s", report.SizesAt.UTC().Format(time.RFC3339))
	}
	// The count and each fault's first line alone. A fault is a recovered panic with its whole
	// stack behind it (the trimmer's recover, launcher's volumetrim.go), kilobytes of it, and
	// Volume.Faults carries them whole already: this sentence is stored in an outbox row and
	// published to a role topic, so the stack is left out of it. The first line's length is the
	// sender's: this package cuts at the first newline and applies no rune limit.
	if len(report.Faults) > 0 {
		fmt.Fprintf(&out, "; faults the trimmer recovered from: %d", len(report.Faults)+report.FaultsOmitted)
		for _, fault := range report.Faults {
			first, _, _ := strings.Cut(fault, "\n")
			fmt.Fprintf(&out, "; %q", first)
		}
		if report.FaultsOmitted > 0 {
			fmt.Fprintf(&out, "; %d older faults are left out of this report", report.FaultsOmitted)
		}
	}
	return out.String()
}

// quotedList is each element of list quoted and joined, which is how every list the pod chose
// the contents of reaches the sentence above: a name with a newline or a quotation mark in it is
// text inside the sentence rather than a line of its own.
func quotedList(list []string) string {
	quoted := make([]string, len(list))
	for at, one := range list {
		quoted[at] = strconv.Quote(one)
	}
	return strings.Join(quoted, ", ")
}

// missingList is the targets a size walk measured none of, and the volume root or an immediate
// child of it where the walk did not list one in full, as quotedList writes a list, each followed
// by the reason the kernel gave where it has one, quoted the same way: the reason comes from the
// pod as the path does.
func missingList(list []record.VolumeMissing) string {
	quoted := make([]string, len(list))
	for at, one := range list {
		quoted[at] = strconv.Quote(one.Path)
		if one.Reason != "" {
			quoted[at] += " (because " + strconv.Quote(one.Reason) + ")"
		}
	}
	return strings.Join(quoted, ", ")
}

// sessionLost tells the issue and its architect that a worker forgot its recorded session, so the
// agent working the issue now has read none of its own earlier work: it runs from a fresh session,
// and only what it pushed and committed survives. Nothing of the issue moves — the worker is
// running — and nothing is held: the report is the whole point, since a worker that lost its
// memory is otherwise indistinguishable from one that is merely slow. The controller gets no copy:
// the architect is the one who acts on it, and a person reads the issue (#4065). An issue the
// record does not hold tells nobody.
//
// The fact's cause chooses the words, the notice kind and the warning line. A session no store
// holds (gone, and a fact with no cause) and a Claude Code claim's session with no transcript file
// are told as lost, in a notice of the kind session-lost. A session that the pod's launcher, older
// than this release, could not look up by its id, and that Oh My Pi did not resume by that id, is
// told as forgotten, in a notice of the kind session-forgotten, and nothing says it was lost:
// nothing of it was deleted, and a store may hold it still (dispatch://INFRA-4108 R3). The kind is
// part of what the architect hears, since the outbox publishes each notice as `<kind> on <issue>`
// (the daemon's noticeSummary), so that forget has a kind of its own.
//
// The detail of a lost session can be the runtime's own account of the failed resume, which
// carries a session file name and a transport error from outside the daemon, and this sentence is
// read into an agent's prompt, so it is quoted as a Go string (strconv.Quote): the quotes bound it,
// and a quote or a newline inside it is escaped rather than closing the one around it.
// sessionStoreFault quotes its own detail for the same reason. The words of a forgotten session
// carry no detail; they name the session the claim forgot (intake.SessionLost.Session), which the
// agent's registration chose, quoted the same way.
//
// A claim is its agent's, so a keyed agent's claim can lose its session on a step of another role
// than its name: the sentence then names the agent and the step's role, and says that each later
// step of that agent runs in the fresh session, which is the key's. A claim named by its role, the
// claim of every step of an issue with no plan rows, keeps the sentence it always had.
func (e *Engine) sessionLost(ctx context.Context, tx pgx.Tx, fact intake.SessionLost) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	kind := record.NoticeKind("session-lost")
	const fresh = "It is running a fresh agent, which has read none of its own earlier turns; only what it pushed and committed survives."
	// subject names who lost the session and runner who runs the fresh agent: the step's role, or,
	// for a keyed agent's claim, whose name is another role's, that agent on its step, whose later
	// steps run in the fresh session too (later).
	subject, runner, later := fmt.Sprintf("The %s of %s", fact.Role, issue.Key), "The "+string(fact.Role), ""
	if _, agent, ok := fact.Claim.Cut(); ok && agent != claim.Agent(fact.Role) {
		subject = fmt.Sprintf("The agent %s of %s, on its %s step,", agent, issue.Key, fact.Role)
		runner, later = "The agent "+string(agent), " Each later step of that agent runs in the fresh session."
	}
	told := fmt.Sprintf("%s lost the session it recorded: %s. %s%s", subject, strconv.Quote(fact.Detail), fresh, later)
	line := "workflow: a worker's recorded session was in no session store"
	fields := []any{"issue", issue.Key, "role", fact.Role, "claim", fact.Claim, "detail", fact.Detail}
	if fact.Cause == supervise.SessionLossNotResumedByID {
		kind = "session-forgotten"
		told = fmt.Sprintf("%s forgot the session it recorded, %s: its pod's launcher is older than this release and could not look the session up by its id, and Oh My Pi did not resume it by that id. "+
			"The daemon deleted nothing of that session. %s is running a fresh agent, which has read none of its own earlier turns; only what it pushed and committed survives.%s",
			subject, strconv.Quote(fact.Session), runner, later)
		line = "workflow: a worker forgot its recorded session, which its pod's launcher could not look up and Oh My Pi did not resume by its id"
		fields = []any{"issue", issue.Key, "role", fact.Role, "claim", fact.Claim, "session", fact.Session, "cause", fact.Cause, "detail", fact.Detail}
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.MessagePost{Body: told}); err != nil {
		return intake.Result{}, err
	}
	e.warnOnCommit(ctx, line, fields...)
	return intake.Result{}, e.notice(ctx, tx, issue.Key, record.Notice{
		Kind: kind, Role: fact.Role, Phase: issue.Phase, Reason: told})
}

// sessionStoreFault tells the issue and the controller that no worker of the issue can start
// because the session store cannot be asked. It is not the claim's failure and holds no phase: the
// daemon keeps the session and starts again every minute, and this is how the people who can fix
// the store learn that they must. The supervisor decides how often it is told — at the first such
// start, then hourly — so every fact that reaches here is reported.
func (e *Engine) sessionStoreFault(ctx context.Context, tx pgx.Tx, fact intake.SessionStoreFault) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	told := fmt.Sprintf("The %s of %s cannot be started: the session store could not be asked for its session: %s. Nothing is lost and nothing is charged; the daemon tries again every minute, and no worker of this issue runs until the store answers.",
		fact.Role, issue.Key, strconv.Quote(fact.Detail))
	if err := e.enqueue(ctx, tx, issue.Key, record.MessagePost{Body: told}); err != nil {
		return intake.Result{}, err
	}
	e.warnOnCommit(ctx, "workflow: the session store could not be asked for a start", "issue", issue.Key, "role", fact.Role, "detail", fact.Detail)
	return intake.Result{}, e.enqueue(ctx, tx, issue.Key, record.ControllerNotice(record.Notice{
		Kind: "session-store-fault", Role: fact.Role, Phase: issue.Phase, Reason: told}))
}

// memoryGuardKill tells the role whose process an issue pod's memory guard ended, or a microVM
// pod's own kernel stopped, why that process is gone (intake.MemoryGuardKill): one role message on
// that role's own topic, enqueued in the fact's transaction as every role message is (tellChecks,
// tellGateMoved), so a fact that rolls back tells nobody and the outbox carries the telling across
// a restart. The topic is the session's claim token, the role the launcher's connection
// authenticated for, and never one the report named. A role whose session holds no topic when the
// row runs is told nothing, and the outbox finishes the row (roleMessage); the kill's line in the
// pod's own log stays.
//
// The report's cause chooses the note: the kernel's (kernelKillNote) for
// shimwire.MemoryKillCauseKernel, a command of the role's; the kernel's for one of the processes
// that run the role's session (kernelProtectedKillNote) for
// shimwire.MemoryKillCauseKernelProtected; and the guard's (memoryGuardNote) for
// shimwire.MemoryKillCauseGuard, the only other cause the launcher session lets through
// (shimwire.LauncherMemoryKill.Validate). The line logged names the cause.
//
// An issue the daemon does not record has no role of the workflow's to tell, so the fact is
// logged and dropped.
func (e *Engine) memoryGuardKill(ctx context.Context, tx pgx.Tx, fact intake.MemoryGuardKill) (intake.Result, error) {
	report := fact.Report
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	if issue == nil {
		e.logOnCommit(ctx, "workflow: a memory guard kill names an issue the daemon does not record, so no role is told",
			"issue", fact.Issue, "role", fact.Token, "pod", fact.PodUID, "guard", report.Guard, "sequence", report.Sequence, "pid", report.PID, "cause", report.Cause)
		return intake.Result{}, nil
	}
	var note, told string
	switch report.Cause {
	case shimwire.MemoryKillCauseKernel:
		note, told = kernelKillNote(report), "workflow: the role whose process the pod's kernel stopped is told why"
	case shimwire.MemoryKillCauseKernelProtected:
		note, told = kernelProtectedKillNote(report), "workflow: the role whose session's own process the pod's kernel stopped is told why"
	default:
		note, told = memoryGuardNote(report), "workflow: the role whose process the memory guard ended is told why"
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.RoleMessage{Role: string(fact.Token), Body: note}); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, told,
		"issue", issue.Key, "role", fact.Token, "pod", fact.PodUID, "guard", report.Guard, "sequence", report.Sequence, "pid", report.PID,
		"cause", report.Cause, "sizeBytes", report.SizeBytes, "readingBytes", report.ReadingBytes, "counterBytes", report.CounterBytes,
		"limitBytes", report.LimitBytes)
	return intake.Result{}, nil
}

// The ends of the notes of a role's process stopped for memory. memoryKillGoesOn says what goes
// on after a command of the role's is stopped, in the guard's note and the kernel's note of a
// command; it is not true of a process that runs the role's session, whose note says what happens
// instead (kernelProtectedKillEnd). memoryKillHeavyWork, where heavy work runs instead, ends every
// note.
const (
	memoryKillGoesOn    = "The pod's other processes and your session go on. "
	memoryKillHeavyWork = "Heavy work — a build, a whole test suite, anything that needs much memory — runs where your deployment's instructions say, not in this pod: " +
		"read skill://legion-worker/references/tools.md before you run it again."
)

// kernelProtectedKillEnd is what a role reads of one of the processes that run its session
// stopped by the pod's kernel (shimwire.MemoryKillCauseKernelProtected): the role-init, the shim,
// the agent and the helpers the launcher's classifier protects. The agent's end ends the shim's
// (the shim returns the agent's exit), the shim's ends the role-init's, which ends every process
// of the role with it, and the launcher reports that generation's exit. The daemon reads that as
// the role's process gone, as it reads every exit a role's process makes on its own: it takes
// back the task the turn was running, charges one launch failure, and relaunches the same
// session, which is sent that task again, or fails the claim where the launch failures have
// reached their limit (supervise's died, interrupted, relaunchAfterFailure). A helper of the
// agent's may end alone, which is why the note says what follows only of the agent.
const kernelProtectedKillEnd = "It was one of the processes that run your session, not a command you ran. " +
	"If it was your agent, your turn ended with it, and so did every other process of your role. " +
	"The daemon then starts the same session again and sends again any task that the turn was running. " +
	"It counts each such exit of a role's process as a failed launch, and at the limit of failed launches it fails your claim instead. "

// memoryGuardNote is what a role reads when the memory guard ended one of its processes: which
// process, how much it held, how near the limit the pod was, that nothing else of the role
// stopped, and where heavy work runs instead. The sizes are binary units a reader can compare at
// a glance (binarySize); the process is named by its executable as the launcher sent it, the base
// name escaped and cut to shimwire.MemoryKillExecutableBytes there, with none of its arguments,
// which the role that started it already has, set as a code span nothing in it can end
// (codeSpan).
func memoryGuardNote(report shimwire.LauncherMemoryKill) string {
	return fmt.Sprintf("The launcher's memory guard stopped one of your processes with SIGKILL because the pod was near its memory limit: "+
		"%s (pid %d) held %s of anonymous memory while the pod's anonymous memory read %s of its %s limit. "+memoryKillGoesOn+memoryKillHeavyWork,
		codeSpan(report.Executable), report.PID, binarySize(report.SizeBytes), binarySize(report.ReadingBytes), binarySize(report.LimitBytes))
}

// kernelKillNote is what a role reads when a microVM pod's own kernel stopped one of its commands
// because the pod ran out of memory (shimwire.MemoryKillCauseKernel): which process, how much it
// held, the pod's memory counter against its limit (kernelKillLead), that nothing else of the role
// stopped, and where heavy work runs instead.
func kernelKillNote(report shimwire.LauncherMemoryKill) string {
	return kernelKillLead(report) + memoryKillGoesOn + memoryKillHeavyWork
}

// kernelProtectedKillNote is what a role reads when a microVM pod's own kernel stopped one of the
// processes that run its session (shimwire.MemoryKillCauseKernelProtected): which process, how
// much it held, the pod's memory counter against its limit (kernelKillLead), what that process's
// end does to the role (kernelProtectedKillEnd), and where heavy work runs instead. It never says
// the session goes on, which the end of the agent's own process makes false.
func kernelProtectedKillNote(report shimwire.LauncherMemoryKill) string {
	return kernelKillLead(report) + kernelProtectedKillEnd + memoryKillHeavyWork
}

// kernelKillLead opens each note of a process the pod's kernel stopped: which process, how much it
// held, and the pod's memory counter against its limit, the process and the sizes written as
// memoryGuardNote writes them.
func kernelKillLead(report shimwire.LauncherMemoryKill) string {
	return fmt.Sprintf("The pod's kernel stopped one of your processes because the pod ran out of memory: "+
		"%s (pid %d) held %s when the pod's memory read %s of its %s limit. ",
		codeSpan(report.Executable), report.PID, binarySize(report.SizeBytes), binarySize(report.CounterBytes), binarySize(report.LimitBytes))
}

// controllerMemoryGuardKill tells the project's controller which of its own programs the memory
// guard of the controller's pod ended, or the pod's own kernel stopped, and why
// (intake.ControllerMemoryGuardKill): one controller notice of kind memory-guard-kill, filed under
// the project key as a tick is, and enqueued in the fact's transaction, so a fact that rolls back
// tells nobody and the outbox carries the telling across a restart. No architect is told: the
// controller's pod is of no issue, and the program was the controller's.
//
// The report's cause chooses the note, as memoryGuardKill's does. The controller's pod in gVisor
// runs the guard, whose reports carry no cause; the controller's pod in a microVM runs the guest
// watch over the launcher's controllerClassifier in its place, and reports the kernel's kills
// with their causes, where the daemon's acknowledgement carried KernelKills (runtime/sandbox's
// launcherHelloAck sets it on the controller's). So the kernel's note of a command
// (controllerKernelKillNote) answers shimwire.MemoryKillCauseKernel; the kernel's note of one of
// the processes that run the controller's session (controllerKernelProtectedKillNote) answers
// shimwire.MemoryKillCauseKernelProtected; and the guard's (controllerMemoryGuardNote) answers
// shimwire.MemoryKillCauseGuard, the only other cause the launcher session lets through
// (shimwire.LauncherMemoryKill.Validate). The line logged names the cause.
//
// Each kill is a row of its own, written here and never through admission's wakeController,
// whose rule keeps one unsent notice per kind and project: a second kill while the first one's
// notice waits is a second program ended, and the controller must hear of each. What keeps one
// kill to one row however often its report arrives is the fact's event id (the daemon's
// memoryGuardKillEventID), which ApplyFact applies once.
//
// The token is the session's, the claim the launcher's connection authenticated for. A token that
// is not this project's controller's (claim.ControllerToken of the project as every token spells
// it, claim.ProjectToken, which is how the sandbox names the controller pod's claim) is no kill of
// this controller's program: the runtime hands only the controller's pod a session of no issue, so
// it is logged and dropped rather than told to a reader whose program it was not.
func (e *Engine) controllerMemoryGuardKill(ctx context.Context, tx pgx.Tx, fact intake.ControllerMemoryGuardKill) (intake.Result, error) {
	report := fact.Report
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return intake.Result{}, err
	}
	if fact.Token != claim.ControllerToken(project) {
		e.logOnCommit(ctx, "workflow: a controller memory guard kill names a token that is not this project's controller, so nobody is told",
			"role", fact.Token, "pod", fact.PodUID, "guard", report.Guard, "sequence", report.Sequence, "pid", report.PID, "cause", report.Cause)
		return intake.Result{}, nil
	}
	var note, told string
	switch report.Cause {
	case shimwire.MemoryKillCauseKernel:
		note, told = controllerKernelKillNote(report), "workflow: the controller whose program the pod's kernel stopped is told why"
	case shimwire.MemoryKillCauseKernelProtected:
		note, told = controllerKernelProtectedKillNote(report), "workflow: the controller is told why the pod's kernel stopped one of the processes that run its session"
	default:
		note, told = controllerMemoryGuardNote(report), "workflow: the controller whose program the memory guard ended is told why"
	}
	if err := e.enqueue(ctx, tx, e.cfg.Project, record.ControllerNotice(record.Notice{
		Kind: record.MemoryGuardKillNotice, Role: claim.RoleController, Reason: note})); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, told,
		"role", fact.Token, "pod", fact.PodUID, "guard", report.Guard, "sequence", report.Sequence, "pid", report.PID,
		"cause", report.Cause, "sizeBytes", report.SizeBytes, "readingBytes", report.ReadingBytes, "counterBytes", report.CounterBytes,
		"limitBytes", report.LimitBytes)
	return intake.Result{}, nil
}

// The ends of the controller's notes of a process stopped for memory, which it reads in place of
// a role's (memoryKillGoesOn, memoryKillHeavyWork). The controller has no worker's tools
// reference, and its skill has no section for this notice, so each note gives its own brief
// advice and points to no skill (dispatch://INFRA-3368/ask/aa99c6ba-8a58-4830-998c-7f8102f65dd0).
// controllerMemoryKillGoesOn says what goes on after a command of the controller's is stopped, in
// the guard's note and the kernel's note of a command; it is not true of a process that runs the
// controller's session, whose note says what happens instead (controllerKernelProtectedKillEnd).
// controllerMemoryKillRunAgain, what to run in place of the same program, ends both notes of a
// command.
const (
	controllerMemoryKillGoesOn   = "Your session and the pod's other processes go on. "
	controllerMemoryKillRunAgain = "Do not run the same program again unchanged: bound its output (a page, a limit, a stream), " +
		"or send heavy work where your deployment's instructions say."
)

// controllerKernelProtectedKillEnd is what the controller reads of one of the processes that run
// its session stopped by its microVM pod's kernel (shimwire.MemoryKillCauseKernelProtected): the
// shim and its direct children, the agent among them, which the launcher's controllerClassifier
// marks as the session's. The controller's pod has no role-init: its shim is the launcher's
// generation itself. The agent's end ends the shim's (the shim returns the agent's exit), and the
// shim's is the generation's exit: the launcher, the container's PID 1, ends what the generation
// left (cleanExitedGeneration) and reports that exit. The daemon reads that as the controller's
// process gone, as it reads every exit of that process (supervise's died): it takes back the task
// the turn was running, charges one launch failure and relaunches the same session, and where the
// claim's budget has run out and the claim fails, the controller keeper retries it with fresh
// budgets after a wait (the daemon's controllerKeeper). Either way the same session starts again,
// which is all the note says of it. A helper of the agent's may end alone, which is why the note
// says what follows only of the agent. It never says the session goes on, which the end of the
// agent's own process makes false, and it does not say "do not run the same program again", since
// the process was not one the controller ran.
const controllerKernelProtectedKillEnd = "It was one of the processes that run your session, not a command you ran. " +
	"If it was your agent, your turn ended with it, and so did every command it was running. " +
	"The daemon then starts the same session again, as it does after any exit of your session's process. " +
	"Bound the output of what you run (a page, a limit, a stream), and send heavy work where your deployment's instructions say."

// controllerMemoryGuardNote is the reason of the notice the controller reads when the memory guard
// of its pod ended one of its programs: which process, how much it held, how near the limit the
// pod was, that its session and the pod's other processes go on (controllerMemoryKillGoesOn), and
// what to run in place of the same program (controllerMemoryKillRunAgain). It is written as
// memoryGuardNote is: sizes in binary units (binarySize), and the process by its executable alone
// in a code span (codeSpan), with none of its arguments.
func controllerMemoryGuardNote(report shimwire.LauncherMemoryKill) string {
	return fmt.Sprintf("The launcher's memory guard stopped one of your processes with SIGKILL because your pod was near its memory limit: "+
		"%s (pid %d) held %s of anonymous memory while the pod's anonymous memory read %s of its %s limit. "+
		controllerMemoryKillGoesOn+controllerMemoryKillRunAgain,
		codeSpan(report.Executable), report.PID, binarySize(report.SizeBytes), binarySize(report.ReadingBytes), binarySize(report.LimitBytes))
}

// controllerKernelKillNote is the reason of the notice the controller reads when its microVM
// pod's own kernel stopped one of its commands because the pod ran out of memory
// (shimwire.MemoryKillCauseKernel): which process, how much it held, the pod's memory counter
// against its limit, as kernelKillNote's lead gives them (kernelKillLead), then what the guard's
// note says after its lead: that the controller's session and the pod's other processes go on,
// and what to run in place of the same program.
func controllerKernelKillNote(report shimwire.LauncherMemoryKill) string {
	return kernelKillLead(report) + controllerMemoryKillGoesOn + controllerMemoryKillRunAgain
}

// controllerKernelProtectedKillNote is the reason of the notice the controller reads when its
// microVM pod's own kernel stopped one of the processes that run its session
// (shimwire.MemoryKillCauseKernelProtected): which process, how much it held, the pod's memory
// counter against its limit (kernelKillLead), and what that process's end does to the controller
// (controllerKernelProtectedKillEnd), as kernelProtectedKillNote tells a role.
func controllerKernelProtectedKillNote(report shimwire.LauncherMemoryKill) string {
	return kernelKillLead(report) + controllerKernelProtectedKillEnd
}

// codeSpan is text as one Markdown code span that nothing in text ends early or spreads over
// lines. The fence is one backtick longer than the longest run of backticks text holds, so a
// backtick in a file name is text inside the span; text that begins or ends with a backtick, or
// that begins and ends with a space, is padded with one space on each side, which a Markdown
// reader strips, and empty text is one space, since two backticks alone are no span. Every
// character that does not print is written as its Go escape (`\n`, `\x1b`, `\u202e`): the
// launcher sends none (it escapes each), but the note is read into an agent's prompt, so its
// shape rests on this daemon and not on the sender being Legion's own launcher, as the session
// facts' quoted details do (sessionLost).
func codeSpan(text string) string {
	var inner strings.Builder
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
		if !unicode.IsPrint(r) {
			quoted := strconv.QuoteRune(r)
			inner.WriteString(quoted[1 : len(quoted)-1])
			continue
		}
		inner.WriteRune(r)
	}
	body := inner.String()
	switch {
	case body == "":
		body = " "
	case strings.HasPrefix(body, "`") || strings.HasSuffix(body, "`") ||
		(strings.HasPrefix(body, " ") && strings.HasSuffix(body, " ")):
		body = " " + body + " "
	}
	fence := strings.Repeat("`", longest+1)
	return fence + body + fence
}

// binaryUnits are the units binarySize writes a size of a KiB or more in, smallest first.
var binaryUnits = []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

// binarySize is a size in bytes as a person reads one: in the largest binary unit it reaches, with
// two decimals (3.13 GiB, 812.40 MiB), and in whole bytes below a KiB. A value that two decimals
// would round up to 1024.00 is written in the next unit, as 1.00.
func binarySize(bytes uint64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	value, unit := float64(bytes)/1024, 0
	for value >= 1023.995 && unit < len(binaryUnits)-1 {
		value /= 1024
		unit++
	}
	return fmt.Sprintf("%.2f %s", value, binaryUnits[unit])
}

// noticeFailure is the failure record the notices of fact carry: the cause the site that failed
// the claim chose, the sentence it wrote beside that word, and the four counts the failure left,
// each against the limit it ran out at (record.NoticeFailure). Deaths are measured against the
// launch failure limit, as the supervisor measures them (supervise.Limits).
//
// The hold and the worker-died are built from this one value, so the two cannot name the same
// failure differently. It is nil for a fact carrying no cause — a failure recorded before the
// daemon kept one, replayed at a boot — and such a notice carries no failure record at all rather
// than a record of zeroes, which would read as a claim that failed with every budget untouched.
func noticeFailure(fact intake.ClaimFailed) *record.NoticeFailure {
	if fact.Cause == "" {
		return nil
	}
	return &record.NoticeFailure{
		Cause:          string(fact.Cause),
		Detail:         fact.Reason,
		LaunchFailures: record.FailureBudget{Count: fact.Budgets.LaunchFailures, Limit: fact.Limits.LaunchFailures},
		Deaths:         record.FailureBudget{Count: fact.Budgets.Deaths, Limit: fact.Limits.LaunchFailures},
		PromptFailures: record.FailureBudget{Count: fact.Budgets.PromptFailures, Limit: fact.Limits.PromptFailures},
		PromptRetires:  record.FailureBudget{Count: fact.Budgets.PromptRetires, Limit: fact.Limits.PromptRetires},
		// What the runtime and the launcher last said of this claim, each where it recorded one:
		// the cause word says which ended it, and these say what the two events were (R6).
		Death:        fact.Death,
		StartFailure: fact.StartFailure,
	}
}

// retryOrEscalate is the architect's decision on a held issue: a retry restores the phase the
// hold is from and starts its role again (retryPhase), and an escalation records the reason
// `escalated` and tells the controller. A hold its plan holds the issue in, a stopped task or a
// parent held for its children, is neither one's to release, and both are refused
// (planHoldRefusal) with nothing changed. Of a plan of several tasks the decision is of one held
// task (heldView): the one it names, the one whose step its claim runs, or the one held task, read
// for that task, so its release and its escalation hold or release that task alone; a decision
// that names none while two or more are held, or a named task that is not held, is refused.
func (e *Engine) retryOrEscalate(ctx context.Context, tx pgx.Tx, fact intake.RetryOrEscalate) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	s, refusal, err := e.heldScope(ctx, tx, *issue, fact.Task, fact.Claim)
	if err != nil || refusal != nil || s.issue.Phase != phase.Held || s.issue.Hold == nil {
		return intake.Result{Refusal: refusal}, err
	}
	if refusal, err := e.planHoldRefusal(ctx, tx, s); err != nil || refusal != nil {
		return intake.Result{Refusal: refusal}, err
	}
	if fact.Decision == intake.EscalateDecision {
		// Recorded on the issue as well as sent: the controller reads holds from the record at every
		// start, so an escalation made while none ran still reaches it. The held task takes the
		// reason too (record.Store.HoldTask), which the issue's hold is derived from.
		escalated := *s.issue.Hold
		escalated.Reason = record.HoldEscalated
		s.issue.Hold = &escalated
		if s.walk.Stored {
			if err := e.store.HoldTask(ctx, tx, s.issue.Key, s.issue.Generation, s.walk.Task, record.HoldEscalated); err != nil {
				return intake.Result{}, err
			}
			s.walk.State, s.walk.HoldReason = record.TaskHeld, record.HoldEscalated
		}
		if _, err := e.putScope(ctx, tx, &s); err != nil {
			return intake.Result{}, err
		}
		// The architect's own words where it named any, beside the cause the hold already names:
		// the controller must act on this hold, and why the architect escalated it is what it
		// acts on. They travel in a field of their own, since Reason is the word `escalated`
		// both agents' texts branch on (record.Notice.Escalation).
		held := record.Notice{Kind: "held", Phase: escalated.From, Reason: string(record.HoldEscalated), Escalation: fact.Reason, Summary: heldSummary(s)}
		return intake.Result{}, e.noticeWithController(ctx, tx, issue.Key, held)
	}
	if fact.Decision != intake.RetryDecision {
		return intake.Result{}, nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return intake.Result{}, err
	}
	held := heldStepRole(s, s.issue.Hold.From)
	if refusal, err := e.stepMovedRefusal(ctx, tx, s.issue, held, fact.Claim); err != nil || refusal != nil {
		return intake.Result{Refusal: refusal}, err
	}
	// The architect's own words where it named any, which the restarted role's task and the log
	// line for the release both carry; a decision that names none keeps the daemon's. A task
	// released at its parts lets its parts that wait start (startReadyNow).
	if _, err := e.retryPhase(ctx, tx, s, s.issue.Hold.From, cmp.Or(fact.Reason, "retry held phase"), false); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.startReadyNow(ctx, tx, issue.Key)
}

// heldStepRole is the role of the step the hold of s keeps at from, the phase the hold is from
// (record.Issue.RunningAt, which keeps a held step at the phase its place derives): for a hold from
// step, the role of the step it was held at, since phase.RoleFor knows no role of step. It is the
// step of s's task, and of a run with no plan rows its one step there.
func heldStepRole(s TaskScope, from phase.Phase) claim.Role {
	for _, step := range s.issue.RunningAt(from) {
		if !s.Planned() || step.Task == s.Task() {
			return step.Role
		}
	}
	return ""
}

// stepMovedRefusal refuses a restart of role's phase of issue asked for one claim, asked, where the
// record names another claim on role's step now (workerClaim), read here under the fact's lock: the
// start the restart queues hands the step's entry to that other claim, whose step was entered for
// it after the caller read the step, and starting it again would send the retry's task to an agent
// the caller did not ask about. A restart that names no claim is the architect's or the operator's
// of the role's step as it stands, and is never refused here. Only a phase worker's claim is named
// (api's restartWorkedPhase), so role is a phase worker's wherever asked is set.
func (e *Engine) stepMovedRefusal(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, asked claim.Token) (*intake.Refusal, error) {
	if asked == "" {
		return nil, nil
	}
	now, err := e.workerClaim(ctx, tx, issue, role)
	if err != nil || now == asked {
		return nil, err
	}
	return refused("PHASE_NOT_RESTARTED", fmt.Sprintf("the %s's step of %s is the claim %s's now, not %s's, so nothing restarted it",
		role, issue.Key, now, asked)).Refusal, nil
}

// restartPhase is the operator's retry of a phase worker through `legion claims retry` (api's
// retryClaim): the worker failed, or an operator stopped it, and a stop is no death, so no hold
// may be waiting for its architect to decide. The phase the worker works is started again, which
// is what gives it the task of that phase; an issue held for this role is restored from its hold
// first, as the architect's retry does. One whose phase another role works, or that has left the
// workflow, is refused, since starting that phase under this claim's name would run the wrong
// role. A restart that asks for a new session (`--fresh-session`) says so on the start it
// queues, so the claim forgets its recorded session in the retry that start makes and no refusal
// above can leave a session discarded for a restart that never happened. An issue its plan holds
// is refused as the architect's retry is (planHoldRefusal). Whatever the plan's count of tasks the
// phase is that of the step the claim runs, or of the held or stopped task whose step has that
// claim (scopeOfAgent), read for that step's task, so the restart releases and starts that task
// alone, and a stopped task's or a held parent's is refused for its own hold; a claim that runs no
// step works no phase.
func (e *Engine) restartPhase(ctx context.Context, tx pgx.Tx, fact intake.RestartPhase) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	s, ok, err := e.scopeOfAgent(ctx, tx, *issue, fact.Role, fact.Claim)
	if err != nil {
		return intake.Result{}, err
	}
	view := *issue
	if ok {
		view = s.issue
	}
	working := view.WorkingPhase()
	works := false
	if ok {
		if works, err = worksAt(ctx, e.store, tx, view, working, fact.Role); err != nil {
			return intake.Result{}, err
		}
	}
	if !InWorkflow(*issue) || !works {
		return refused("PHASE_NOT_RESTARTED", fmt.Sprintf("the %s does not work %s's phase %s, so nothing restarted it",
			fact.Role, issue.Key, working)), nil
	}
	if refusal, err := e.planHoldRefusal(ctx, tx, s); err != nil || refusal != nil {
		return intake.Result{Refusal: refusal}, err
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return intake.Result{}, err
	}
	if lingers {
		return refusedLingering(*issue, fmt.Sprintf("the operator's restart of the %s", fact.Role)), nil
	}
	if refusal, err := e.stepMovedRefusal(ctx, tx, s.issue, fact.Role, fact.Claim); err != nil || refusal != nil {
		return intake.Result{Refusal: refusal}, err
	}
	if _, err := e.retryPhase(ctx, tx, s, working, "the operator retried this phase's worker", fact.FreshSession); err != nil {
		return intake.Result{}, err
	}
	// A task released at its parts lets its parts that wait start (startReadyNow).
	return intake.Result{}, e.startReadyNow(ctx, tx, issue.Key)
}

// retryPhase gives issue the phase from back — a held issue leaves its hold — and starts that
// phase's worker, told why it runs again. It re-decides no CI
// result: a settlement that reached the issue while it was held moved nothing, as it would
// have moved nothing had it arrived with the issue in its phase (dispatch://INFRA-714/spec
// R1), so there is no standing red left for the retry to act on, and the restored worker reads
// the head's checks itself. From
// reviewing a review round held open may already have both of its halves - the reviewer
// completed before the hold, and its review was recorded while held - and ends here
// (reviewRound); the reviewer is started only if the round is still open, since a second
// completion of the same commit would be refused as not new. A round its reviewer completed that
// nothing would end is stuck: the reviewer's task says why, so the restarted reviewer decides
// it. The architect, who ordered the retry, is told nothing: a notice now would read as the
// reviewer's answer to its request. The pull request the round and the restarted worker's task
// read is the one of the work issue is read for (pullRequestOf): of a copy read for one task of a
// plan of several, that task's own.
//
// freshSession is the operator's `--fresh-session`: the start it queues retries the claim in a
// new session rather than the one it recorded. An architect's retry of a held phase asks for
// none, and resumes the recorded session, whatever the claim's run of refused turns (supervise's
// freshSession).
//
// The release is logged as it commits: the issue, the phase it comes back to and the reason,
// which is the architect's own words where its retry named any.
//
// A release is a phase start like any other (dispatch://INFRA-3320 R9): a release into planning
// through accepting of a parent whose Dispatch child is open moves it to awaiting_children instead,
// holding the retried phase with the task the retry would have given its worker (task) and the
// retry's reason (awaitChildren), and starts nobody. The round of a reviewing release is not read
// then: the held reviewing goes back as the first step of its walk after the implementer's
// (record.ReturnStep), testing on today's line, whatever that round came to. A retry of testing
// tells the tester of a parent to test the head merged with its base's tip, as every testing task
// of a parent does: the restart's start ends the task the retry composes with that sentence
// (restart, withBaseMergeRun).
// The release restarts the held phase on the entry it was raised on (restart raises none), so a
// phase whose role stands on an entry a role's move back raised out of retro or merging is
// restarted whatever its children are, as the admission pass leaves it (acceptedEntry,
// StopForChildren): R9 holds only the starts each from a phase before retro.
//
// Its callers have established that the tree does not linger, which is what holds every member
// where it stood. A task released at its parts runs there with no wait, which lets its parts that
// wait start: each caller, the door of its fact, runs startReadyNow once after it (retryOrEscalate,
// restartPhase, releaseHeldRoot), so no door starts waiting tasks twice.
//
// It answers the phase the issue ends in, which the caller that must write a status of its own
// reads (releaseHeldRoot): from, where nothing moved, awaiting_children where a child holds the
// start, and otherwise the phase the move took the issue to, read back from the record because the
// move's own row chose that target (transition).
func (e *Engine) retryPhase(ctx context.Context, tx pgx.Tx, s TaskScope, from phase.Phase, reason string, freshSession bool) (phase.Phase, error) {
	// The retry keeps the task's place (moveTask): it puts back Hold.From, the phase that place
	// derives, so the task never moved. A held task is released at that place first
	// (record.Store.ReleaseTask), and the issue is written where its tasks then put it (putScope).
	// A task the release before left running beside its issue's hold has nothing to release.
	if s.walk.Stored && s.walk.State == record.TaskHeld {
		if err := e.store.ReleaseTask(ctx, tx, s.issue.Key, s.issue.Generation, s.walk.Task); err != nil {
			return "", err
		}
		s.walk.State = record.TaskRunning
	}
	s.issue.Phase, s.issue.Hold = from, nil
	// The daemon's own line for the release (R11), written whether or not a worker is started
	// below: the hold is gone either way, and the reason is the one the restarted role's task
	// carries.
	e.logOnCommit(ctx, "workflow: a held phase is released", "issue", s.issue.Key, "phase", from, "reason", reason)
	// The guard reads the pull request of s's task (childrenHoldingSamePhaseStart), which the held
	// task and the review round below read too.
	open, pr, err := e.childrenHoldingSamePhaseStart(ctx, tx, s)
	if err != nil {
		return "", err
	}
	if len(open) > 0 {
		at, err := e.standingPart(ctx, tx, s, readPull(pr))
		if err != nil {
			return "", err
		}
		return phase.AwaitingChildren, e.awaitChildren(ctx, tx, s, childrenHold{Start: from,
			Task: task(s.issue, at, record.PhaseRow{}, reason), Cause: reason, Open: open})
	}
	if _, err := e.putScope(ctx, tx, &s); err != nil {
		return "", err
	}
	issue := s.issue
	// The retry puts the issue back in the phase its worker failed in, which is a phase write
	// like any other, so it is a movement (the rule in dispatchIssue).
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return "", err
	}
	if from == phase.Reviewing {
		reviewer, err := e.stepRow(ctx, tx, s, claim.RoleReviewer)
		if err != nil {
			return "", err
		}
		// The retry tells nothing of a stuck round: passed as its own before, it is stuck the same way.
		r := e.reviewRound(issue, reviewer, pr)
		settled, err := e.settleRound(ctx, tx, s, reviewer, pr, r, "")
		if err != nil {
			return "", err
		}
		if settled {
			return e.phaseNow(ctx, tx, issue)
		}
		if r.outcome == roundStuck {
			reason += ": " + r.reason
		}
	}
	// The role of the step the retry starts again: the step the hold kept at from, of the task the
	// retry is of (heldStepRole).
	role := heldStepRole(s, from)
	if err := e.restart(ctx, tx, s, role, readPull(pr), func(at Standing) string {
		return task(issue, at, record.PhaseRow{}, reason)
	}, freshSession); err != nil {
		return "", err
	}
	// The release puts the issue back in a phase, so it queues the reconcile of the tree's news as
	// a move does after its start (queueMovedNews). It puts the issue back in the phase the hold is
	// from, which is no move between phases, so the merge forward stands as the hold left it.
	return from, e.queueMovedNews(ctx, tx, issue)
}

// backwardRefusal is why the guard refuses fact, and "" where it does not: the one sentence that
// names which of the guard's readings failed. served is whether the table holds a backward row of
// the issue's phase for the move's target. two is the two earlier places of w's task that run as
// step, nearest first in the task's order (Walk.backwardPlace), where the move names step and
// more than one serves it: the move does not say which, and the refusal names both and the field
// that names one.
//
// It is a function of the issue, the walk and the fact alone and reads no pull request, so the
// rule that a merged change never moves back is none of its readings: backward, which holds the
// transaction, reads the pull request of the move's task after this answers "" and refuses that
// move itself.
func backwardRefusal(issue record.Issue, w Walk, fact intake.BackwardMove, served bool, two []record.TaskPlace) string {
	switch {
	case fact.To == "":
		return fmt.Sprintf("a backward move names the phase it moves to; the %s's move of %s named none, so it changed nothing",
			fact.Requester, issue.Key)
	case len(two) == 2:
		return fmt.Sprintf("task %s of %s has two earlier steps that run as step, %d (%s) and %d (%s), so a move back to step does not say "+
			"which one the task returns to; this move changed nothing. The agent of a step names the place with handoff_complete, "+
			"the verdict fail and returnTo; the owner names it with request_backward_move and returnTo.",
			w.Task, issue.Key, two[0].Position, w.RoleAt(two[0]), two[1].Position, w.RoleAt(two[1]))
	case !served:
		return fmt.Sprintf("a backward move reaches an earlier phase, and no backward row of %s serves %s; the %s's move of %s changed nothing",
			issue.Phase, fact.To, fact.Requester, issue.Key)
	case !slices.ContainsFunc(issue.RunningAt(issue.Phase), func(step record.RunningStep) bool { return step.Role == fact.Requester }):
		owners := "no role's"
		if at := issue.RunningAt(issue.Phase); len(at) > 0 {
			roles := make([]string, len(at))
			for i, step := range at {
				roles[i] = string(step.Role)
			}
			owners = "the " + strings.Join(roles, " and the ") + "'s"
		}
		return fmt.Sprintf("a backward move is the role running the issue's phase: %s of %s is %s, so the %s's move to %s changed nothing",
			issue.Phase, issue.Key, owners, fact.Requester, fact.To)
	}
	return ""
}

// backwardReturn is the place a move back that names returnTo reaches, or why it is refused,
// which changes nothing: returnTo is judged by the one judge of a named place, the one a failed
// step's returnTo meets (returnStep, with the move's own words), against the step the task stands
// at, and the move's to must be the phase the named step runs as. A task that stands at its retro
// or its merge stands at no step, so a move from there names none.
func backwardReturn(issue record.Issue, w Walk, fact intake.BackwardMove) (record.TaskPlace, string) {
	if w.Stored && w.Place.Kind != record.PlaceStep {
		return record.TaskPlace{}, fmt.Sprintf("returnTo is %d, and task %s of %s stands at its %s, not at a step; send the move again without returnTo; nothing was recorded",
			fact.ReturnTo, w.Task, issue.Key, w.Place.Kind)
	}
	place, why := returnStep(issue, w, placeNamed(w.Place, issue.Phase), fact.ReturnTo, backwardMove)
	if why != "" {
		return record.TaskPlace{}, why
	}
	if runs := issuePhaseOf(w, place, record.TaskRunning); runs != fact.To {
		return record.TaskPlace{}, fmt.Sprintf("returnTo %d names step %d (%s) of task %s, which runs as %s, and to names %s; this move changed nothing.",
			fact.ReturnTo, place.Position, w.Steps[place.Position-1].Role, w.Task, runs, fact.To)
	}
	return place, ""
}

// phaseNow is the phase issue stands in now, read back after a move wrote it (retryPhase, and
// the first step StartFirstPhase reports): the move's own row decides which phase it reaches, so
// the record is asked rather than the target named here a second time. An issue the move took
// out of the record stands where the caller last wrote it.
func (e *Engine) phaseNow(ctx context.Context, tx pgx.Tx, issue record.Issue) (phase.Phase, error) {
	moved, err := e.store.Issue(ctx, tx, issue.Key)
	if err != nil || moved == nil {
		return issue.Phase, err
	}
	return moved.Phase, nil
}

// backward applies a backward move (request_backward_move): the role running the issue's phase
// sends it back to an earlier phase, with its reason. It is refused BACKWARD_REFUSED, changing
// nothing, where the table's guard refuses it (backwardRefusal), and where the issue's pull
// request has merged, in whichever phase the issue stands: a merged change never moves back.
// GitHub reopens no merged pull request and its change is already on the base branch, so an
// earlier phase has no pull request left to carry new work; that work goes to an issue whose pull
// request is still open, which the architect decides.
//
// The move is the owner's from accepting or the phase worker's that runs the phase. Of a plan of
// several tasks it moves one task (backwardView): the owner's move is of the task it names, or of
// the one task at accepting where it names none, and a worker's of the task of the step its
// claim's agent runs, which the route names; the rest reads the copy of the issue read for that
// task, and its pull request is that task's own.
func (e *Engine) backward(ctx context.Context, tx pgx.Tx, fact intake.BackwardMove) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	// A parent waiting on its children stands in no phase a move back acts on, so the move is
	// refused with the wait's own code before its task is read and before the table is, which
	// would only say that no backward row serves the phase (dispatch://INFRA-3320 R11). The
	// refusal names where the return goes on the walk of the task that waits (waitingScope).
	if issue.Phase == phase.AwaitingChildren {
		waiting, err := e.waitingScope(ctx, tx, *issue, fact.Task)
		if err != nil {
			return intake.Result{}, err
		}
		return e.waitingRefusal(ctx, tx, *issue, waiting.walk, actBackwardMove, fact.Requester)
	}
	s, refusal, err := e.backwardScope(ctx, tx, *issue, fact)
	if err != nil || refusal != nil {
		return intake.Result{Refusal: refusal}, err
	}
	view := s.issue
	issue = &view
	// The table is the guard, and it reads three things: the move names a target, a backward row
	// of the issue's phase serves that target - anything but an earlier phase has none - and the
	// caller is the role running that phase. An empty target is read first, since the row lookup
	// takes it to match any row.
	//
	// The refusal says which of the three failed, because the three are different mistakes and a
	// sentence naming only the last had two readers of this file guess wrong on one day: a target
	// the table does not serve is the caller's own phase misread, while a requester mismatch is a
	// role acting on a phase that is not its own.
	//
	// The table is the one of the issue's walk, so a move back to a phase its task's line does not
	// hold is served by no row.
	w := s.walk
	// The move reaches the one earlier place of its task that runs as its target (backwardPlace);
	// where none or more than one does, no row serves it. A move that names its place (returnTo)
	// goes there instead, judged by the one judge of a named place a failed step's returnTo meets
	// (returnStep), and its target must be the phase that place runs as. A move that names no
	// target is refused first, below, whatever it names.
	target, found, two := w.backwardPlace(fact.To)
	if fact.ReturnTo > 0 && fact.To != "" {
		place, why := backwardReturn(*issue, w, fact)
		if why != "" {
			return refused("BACKWARD_REFUSED", why), nil
		}
		target, found, two = place, true, nil
	}
	served := false
	if found {
		_, served = w.row(issue.Phase, TriggerBackward, target, Snapshot{Phase: issue.Phase})
	}
	if why := backwardRefusal(*issue, w, fact, served, two); why != "" {
		return refused("BACKWARD_REFUSED", why), nil
	}
	// A merged pull request refuses the move in every phase: in merging, where a root's close can
	// wait on the rest of its tree (closeMerged), and in an earlier phase whose pull request merged
	// early, which the architect is told of (merged). The pull request is the one of the move's
	// task (taskPullRequest), which the transition below takes too.
	pr, err := e.taskPullRequest(ctx, tx, s)
	if err != nil {
		return intake.Result{}, err
	}
	if pr != nil && pr.State == record.PullRequestMerged {
		return refused("BACKWARD_REFUSED", fmt.Sprintf("the %s's move of %s to %s changed nothing: its pull request has merged; a merged change never moves back",
			fact.Requester, issue.Key, fact.To)), nil
	}
	// The move posts its reason on the issue, so a reason Dispatch would refuse there is refused
	// here, before anything moves (backwardReasonRefusal).
	if code, why := backwardReasonRefusal(issue.Phase, fact.To, fact.Requester, fact.Reason); code != "" {
		return refused(code, why), nil
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return intake.Result{}, err
	}
	if lingers {
		return refusedLingering(*issue, fmt.Sprintf("the %s's backward move to %s", fact.Requester, fact.To)), nil
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, fact.Requester)
	if err != nil {
		return intake.Result{}, err
	}
	// A phase worker's move is the decision of a turn on the task of the entry its claim serves
	// (PhaseEntry, which its route read). An entry raised since — the phase handed to the role
	// again after the work went back, while that turn still ran — is work that turn never saw, and
	// the move would send it back on a reason written about the work before it. So the move is
	// judged here, on the row this transaction reads, before anything is written, and refused
	// unspent where the row stands on a later entry.
	if fact.PhaseEntry != 0 && fact.PhaseEntry < row.Entry {
		return intake.Result{Refusal: staleBackwardMove(issue.Key, fact.Requester, issue.Phase, row.Entry, fact.PhaseEntry), Unspent: true}, nil
	}
	return intake.Result{}, e.transition(ctx, tx, s, TriggerBackward, target, row, pr, fact.Reason)
}

// staleBackwardMove is the 409 BACKWARD_REFUSED refusal of requester's move of issue back from p,
// made in a turn on entry served of its phase while its phases row stands on entry current: the
// phase was handed to the role again after that turn took its task, so the move is no answer for
// the work the current entry's task asks for, and it changed nothing. The worker does what a
// worker refused a completion of an entry behind does (intake.StaleEntry): it ends the turn, and
// the current entry's task, owed to its claim, comes by itself.
func staleBackwardMove(issue string, requester claim.Role, p phase.Phase, current, served int64) *intake.Refusal {
	return &intake.Refusal{Status: 409, Code: "BACKWARD_REFUSED", Message: fmt.Sprintf(
		"%s handed the %s phase %s again: this move back is of entry %d, which is %d behind the entry %d the phase is on, so it changed nothing. "+
			"End this turn without moving the issue. The task of the entry the phase is on is owed to this claim, and the daemon sends it by itself: judge that task's work at the head it names, and move the issue back in its turn if that work calls for it",
		issue, requester, p, served, current-served, current)}
}

// endChildrenFirst is what the refusals over children, sign_off's and close_root's while an issue
// beneath is still in the workflow or a Dispatch child is open (acceptanceHolds), tell the
// architect to do: wait for its status write to be taken, end a child by that write
// (record.EndChildRule), and ask the person about a child that no move fits. hold is what the
// architect holds until the person answers.
func endChildrenFirst(hold string) string {
	return endChildWords + " When a child can neither finish nor take one of these moves, ask the person. Hold the " + hold + " until they answer."
}

// endChildWords is the sentence on a status the daemon has not taken yet, then the rules for
// ending a child (record.StatusNotTakenYet, record.EndChildRule), which the architect's refusals
// over children still in the workflow, the pr-merged notice and the retro-held notice carry.
// endChildrenFirst puts it before its sentence on asking the person (sign_off's and close_root's
// refusals, acceptanceHolds); the architect's WAITING_ON_CHILDREN refusal (waitingRefusal),
// waitingForTree and retroHeldNotice end with it. The notices over open children end a child by
// childrenLeave instead.
const endChildWords = record.StatusNotTakenYet + " " + record.EndChildRule

// childrenLeave is what the awaiting-children notice (awaitingChildrenNotice) and the
// children-first notice (ChildrenFirstNotice) say of ending a child: a released child and an
// unreleased one leave by the architect's own status write, by the rules for ending a child
// (record.EndChildRule).
const childrenLeave = "A child you released and a child you did not release leave by the same status write. " + record.EndChildRule

// The three holds on an issue's acceptance (signOff) and on a root's close (closeRoot) after the
// work under it, by their refusal codes, in the order the daemon answers them when more than one
// holds (dispatch://INFRA-3320, R8).
const (
	signOffChildInWorkflow = "SIGNOFF_CHILD_IN_WORKFLOW"
	signOffRestingRoot     = "SIGNOFF_RESTING_ROOT_IN_WORKFLOW"
	signOffChildOpen       = "SIGNOFF_CHILD_OPEN"
)

// acceptanceHold is the first of the three holds that holds an issue's acceptance or its close
// (acceptanceHeld): its refusal code, "" for none, and the issues it names, each as the refusal
// names it.
type acceptanceHold struct {
	code  string
	names string
}

// The words each hold acceptanceHeld answers names its issues with: the clause every text that
// names the issues of a hold reads before them (holdTexts.holding, and its refusals).
const (
	beneathHolding   = "these issues beneath it are still in the workflow"
	restingHolding   = "these roots that rest on its approval are still in the workflow"
	openChildHolding = "these Dispatch children of it are open"
)

// holding is what holds the issue h holds, in the words the refusals of h use, followed by each
// issue h names: "these issues beneath it are still in the workflow: LEGION-209". The retro-held
// notice (retroHeldNotice), the refusal of a release of that hold and the reason beside a todo
// set back on a held root (heldParentWait) read it.
func (h acceptanceHold) holding() string {
	return acceptanceHolds[h.code].holding + ": " + h.names
}

// The sentences that more than one text of the holds acceptanceHeld answers says
// (acceptanceHolds): beneathAccepted, what a parent with issues beneath it is accepted after;
// restingAfter, a format of the issue's key, the start of why a resting root holds the issue,
// which restingAcceptedAfter (the sign-off's refusal and the retro-held notice) and the close's
// refusal end each in its own words; restingTreeMover, who moves a resting root; and
// childrenAcceptedAfter and ownTreeChildMover, what the sign-off's refusal over an open Dispatch
// child and the retro-held notice of a parent such a child holds tell the owner.
const (
	beneathAccepted       = "A parent is accepted only once no issue beneath it is still in the workflow"
	restingAfter          = "The design-gate step of each rests on an approval of %[1]s, so %[1]s"
	restingTreeMover      = "Each is a tree of its own: its own architect, or a person, moves it."
	restingAcceptedAfter  = restingAfter + " is accepted after them. " + restingTreeMover
	childrenAcceptedAfter = "A parent is accepted after its children, a child that runs as a tree of its own included: run each one to done, releasing one that was never released."
	ownTreeChildMover     = "A child that runs as a tree of its own is moved by its own architect or a person."
)

// architectAct is an act a hold on an issue refuses, named as the request that makes it names it:
// the sign-off (signOff) and the close of a root (closeRoot), which an acceptance hold refuses
// (acceptanceHold.refusal), and the move back (backward, an architect's or a worker's) and the
// owner's stop of a task (taskStopped), which a parent's wait on its children refuses with the
// sign-off (waitingRefusal).
type architectAct string

const (
	actSignOff      architectAct = "sign_off"
	actCloseRoot    architectAct = "close_root"
	actBackwardMove architectAct = "request_backward_move"
	actStopTask     architectAct = "stop_task"
)

// holdTexts is what one hold acceptanceHeld answers says wherever it is named. holding is the
// clause every text that names the hold's issues reads before them (acceptanceHold.holding).
// refusals is the refusal text of each act the hold refuses (acceptanceHold.refusal): a format of
// the issue's key (%[1]s) and the names the hold carries (%[2]s), and a sign-off's of the step a
// held acceptance goes back to (%[3]s, heldAcceptanceBack); the two holds over children end with
// endChildrenFirst's words, which carry no formatting verb. retro is what the retro-held notice of
// a parent the hold holds before its retro tells the owner to do about its issues
// (retroHeldNotice), a format of the parent's key (%[1]s) that the notice's own format carries,
// and childRules says that notice ends with the rules for ending a child (endChildWords).
type holdTexts struct {
	holding    string
	refusals   map[architectAct]string
	retro      string
	childRules bool
}

// acceptanceHolds is the texts of each hold acceptanceHeld answers, by its code: the one place a
// hold's words are written, which its refusals (acceptanceHold.refusal), the words naming what
// holds a parent before its retro (acceptanceHold.holding) and that parent's retro-held notice
// (retroHeldNotice) all read. So a hold added to acceptanceHeld is written here for each of them,
// which TestEveryAcceptanceHoldRefusesBothActs holds every entry to, and one the table misses for
// an act still refuses that act (acceptanceHold.refusal).
var acceptanceHolds = map[string]holdTexts{
	signOffChildInWorkflow: {
		holding: beneathHolding,
		refusals: map[architectAct]string{
			actSignOff:   "%[1]s is not accepted while " + beneathHolding + ": %[2]s. " + beneathAccepted + ", on a head that need not carry its children's merges, once each check a role runs has a pass at that head whose observed line names a base commit that carries every needed child's merge. Run each one to done: unless the pull request of %[1]s has merged, the next admission pass moves it to awaiting_children, and once no child under it is open it goes back by itself to the start it held%[3]s; otherwise sign off again. " + endChildrenFirst("acceptance"),
			actCloseRoot: "%[1]s is not closed while " + beneathHolding + ": %[2]s. Closing it would linger the tree and stop them mid-turn. Close the tree again when each of them has left the workflow. " + endChildrenFirst("close"),
		},
		retro:      beneathAccepted + ". Run each one to done, or end it.",
		childRules: true,
	},
	signOffRestingRoot: {
		holding: restingHolding,
		refusals: map[architectAct]string{
			actSignOff:   "%[1]s is not accepted while " + restingHolding + ": %[2]s. " + restingAcceptedAfter + " Sign off again once each one is done or out of the workflow",
			actCloseRoot: "%[1]s is not closed while " + restingHolding + ": %[2]s. " + restingAfter + " closes after them. " + restingTreeMover + " Close the tree again once each one is done or out of the workflow",
		},
		retro: restingAcceptedAfter,
	},
	signOffChildOpen: {
		holding: openChildHolding,
		refusals: map[architectAct]string{
			actSignOff:   "%[1]s is not accepted while " + openChildHolding + ": %[2]s. " + childrenAcceptedAfter + " Unless the pull request of %[1]s has merged, the next admission pass moves it to awaiting_children, and once no child under it is open it goes back by itself to the start it held%[3]s; otherwise sign off again. " + ownTreeChildMover + " " + endChildrenFirst("acceptance"),
			actCloseRoot: "%[1]s is not closed while " + openChildHolding + ": %[2]s. A parent closes after its children, and a child that was never released holds the close until it is done or in icebox: close the tree again once none of them is open. " + ownTreeChildMover + " " + endChildrenFirst("close"),
		},
		retro:      childrenAcceptedAfter + " " + ownTreeChildMover,
		childRules: true,
	},
}

// refusal is the refusal of act on the issue key that h makes, and false where h holds nothing:
// the hold's own code with that act's text (acceptanceHolds), back being the words of the step a
// held acceptance of key goes back to (heldAcceptanceBack), which only a sign-off's text reads.
// A hold the table names no text of for act refuses it all the same, with its code and the issues
// it names, so no hold lets a sign-off or a close through for want of its words.
func (h acceptanceHold) refusal(act architectAct, key, back string) (intake.Result, bool) {
	if h.code == "" {
		return intake.Result{}, false
	}
	text, ok := acceptanceHolds[h.code].refusals[act]
	if !ok {
		return refused(h.code, fmt.Sprintf("%s is held by %s, which names %s, so this %s changed nothing", key, h.code, h.names, act)), true
	}
	return refused(h.code, fmt.Sprintf(text, key, h.names, back)), true
}

// heldAcceptanceBack is the words a sign-off's refusal over children adds to the start a parent
// held in accepting goes back to once no child under it is open: the first step of w, the walk of
// its run, after the implementer's (record.ReturnStep), so ", a held acceptance as testing" on
// today's line, and nothing on a walk with no step between the implementer's and the owner's
// acceptance, where it goes back to accepting, the start it held.
func heldAcceptanceBack(w Walk) string {
	if back := record.ReturnStep(w.steps(), phase.Accepting); back != phase.Accepting {
		return ", a held acceptance as " + string(back)
	}
	return ""
}

// acceptanceHeld reads the three holds on issue's acceptance and on its close, in R8's order, and
// answers the first that holds; each later one is read only when the one before it holds nothing.
// First, an issue beneath it in its own tree that is still in the workflow (record.OpenBeneath,
// SIGNOFF_CHILD_IN_WORKFLOW). Then a root of any tree whose design-gate step rests on its approval
// and is still in the workflow (record.RestingRoots, SIGNOFF_RESTING_ROOT_IN_WORKFLOW): the checks
// of the issues under an ancestor prove its requirements together, so it is accepted only after
// them. Then a Dispatch child of it that is still open, one that runs as a tree of its own included
// (childrenOpen, SIGNOFF_CHILD_OPEN). A root that rests on the issue's approval and is a Dispatch
// child of it is named by the second, as it holds there first. It is the one statement of the
// rule for the sign-off (signOff), the close of a root (closeRoot), and the retro of a parent that
// no sign-off accepts, which it holds (retroHeld) and releases (StartHeldRetro).
func (e *Engine) acceptanceHeld(ctx context.Context, tx pgx.Tx, issue record.Issue) (acceptanceHold, error) {
	members, err := e.treeMembers(ctx, tx, issue)
	if err != nil {
		return acceptanceHold{}, err
	}
	if open := record.OpenBeneath(issue, members); len(open) > 0 {
		return acceptanceHold{code: signOffChildInWorkflow, names: strings.Join(open, ", ")}, nil
	}
	resting, err := record.RestingRoots(ctx, tx, e.store, issue.Key)
	if err != nil {
		return acceptanceHold{}, err
	}
	if len(resting) > 0 {
		return acceptanceHold{code: signOffRestingRoot, names: restingNames(resting)}, nil
	}
	children, err := e.childrenOpen(ctx, tx, issue, members)
	if err != nil || len(children) == 0 {
		return acceptanceHold{}, err
	}
	return acceptanceHold{code: signOffChildOpen, names: openChildNames(children)}, nil
}

// childrenOpen is each Dispatch child of issue that is still open (record.ChildOpen), with the
// status Dispatch shows it in, whether the daemon released it, and the issue above it its own
// blocked_by link names, in key order: what the daemon's mirror of Dispatch's issues shows under
// issue (Store.DispatchChildren), judged with the daemon's own record of each child, so a child
// that runs as a tree of its own is open while it is in the workflow there, and a child filed and
// never released is open until it is closed or in icebox. The judgement is the admission pass's
// own of every started parent (record.OpenChildWaits), which leaves out a child of issue's own
// tree that the daemon itself holds at admitted under an accepted ancestor, as OpenBeneath leaves
// it out: otherwise the sign-off's hold and that ancestor's merge, which waits for issue, would
// wait on each other for ever. members is issue's tree where the caller has read it already
// (acceptanceHeld), and nil for one to read it only when Dispatch shows a child under issue at
// all, which is what the guard on every phase start reads (childrenHolding).
func (e *Engine) childrenOpen(ctx context.Context, tx pgx.Tx, issue record.Issue, members []record.Issue) ([]record.OpenChild, error) {
	mirrored, err := e.store.DispatchChildren(ctx, tx, e.cfg.Project, []string{issue.Key})
	if err != nil {
		return nil, err
	}
	rows := mirrored[issue.Key]
	if len(rows) == 0 {
		return nil, nil
	}
	if members == nil {
		if members, err = e.treeMembers(ctx, tx, issue); err != nil {
			return nil, err
		}
	}
	waits, err := record.OpenChildWaits(ctx, tx, e.store, e.cfg.Project, members, map[string][]record.DispatchIssueRow{issue.Key: rows}, nil)
	return waits[issue.Key].Open, err
}

// signOff is the architect's acceptance of the step before it: it moves the issue from accepting
// to the task's next step, or to retro where the acceptance is the task's last step (the default
// steps), and starts that step's role told the report of the step the owner accepted: the handoff
// on the phases row of the role of the step before accepting, with the pull request. An issue with
// no plan rows walks today's line, where that step is the review, so its retro is told the
// review's handoff, as the approval told it before acceptance existed. Outside accepting it
// changes nothing and says so.
//
// It is refused while any issue beneath the one being accepted is still in the workflow
// (OpenBeneath), with SIGNOFF_CHILD_IN_WORKFLOW naming them, since a parent is accepted only once
// no issue beneath it is still in the workflow (dispatch://INFRA-980). It is accepted on a head
// that need not carry its children's merges, once each check a role runs has a pass at that head
// whose observed line names a base commit that carries every needed child's merge, and no base
// carries that merge before the child merges: accepted first, the parent would reach retro and the
// merge with no check of it run on its children's work, which is what dispatch://INFRA-980
// forbids. The acceptance was held this way once before (dispatch://INFRA-229) and the hold was
// taken off because a parent's approved pull request then waited outside the merge queue and went
// stale against its base. Now no phase of a parent starts while an open child stands under it, and
// the admission pass that sees one moves a parent in accepting to awaiting_children, where it holds
// no slot and from which it comes back as the first step of its walk after the implementer's,
// testing on today's line, whose round runs on its head merged with the base's tip once that tip
// carries the children's merges (dispatch://INFRA-3320 R9, R12). So the refusal is the backstop
// for a sign-off that comes before that pass, and the hold itself for a parent whose pull request
// has merged, which the wait leaves where it stands.
//
// Past that, it is refused while a root whose step rests on the issue's approval is still in the
// workflow (SIGNOFF_RESTING_ROOT_IN_WORKFLOW), and then while a Dispatch child of the issue is open
// (SIGNOFF_CHILD_OPEN), each naming what holds it, in that order (acceptanceHeld,
// dispatch://INFRA-3320 R8): the issue's requirements are proved by the checks of the issues under
// it together, so it is accepted after them, before the admission pass that would move it. A
// parent that the wait has already moved stands in awaiting_children, no phase that sign_off acts
// on, and is refused with the wait's own code, naming its open children (waitingRefusal, R11).
//
// Of a plan of several tasks it is the sign-off of one task at accepting (actView): the one it
// names, or the one task there where it names none, read for that task, so it moves that task
// alone and tells its step its own pull request; a sign-off that names none while two or more
// tasks stand at accepting, or that names a task that does not, is refused and changes nothing.
// A parent in awaiting_children is refused before any task is read, on the walk of the task that
// waits (waitingScope).
func (e *Engine) signOff(ctx context.Context, tx pgx.Tx, fact intake.SignOff) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if issue.Phase == phase.AwaitingChildren {
		waiting, err := e.waitingScope(ctx, tx, *issue, fact.Task)
		if err != nil {
			return intake.Result{}, err
		}
		return e.waitingRefusal(ctx, tx, *issue, waiting.walk, actSignOff, claim.RoleArchitect)
	}
	s, ok, refusal, err := e.actScope(ctx, tx, *issue, fact.Task, phase.Accepting)
	if err != nil || refusal != nil {
		return intake.Result{Refusal: refusal}, err
	}
	view := *issue
	if ok {
		view = s.issue
	}
	issue = &view
	if !ok || issue.Phase != phase.Accepting {
		return refused("SIGNOFF_OUTSIDE_ACCEPTING", fmt.Sprintf("%s is in phase %s, not accepting; this sign-off changed nothing", issue.Key, issue.Phase)), nil
	}
	held, err := e.acceptanceHeld(ctx, tx, *issue)
	if err != nil {
		return intake.Result{}, err
	}
	if held.code != "" {
		refusal, _ := held.refusal(actSignOff, issue.Key, heldAcceptanceBack(s.walk))
		return refusal, nil
	}
	accepted, err := e.acceptedRole(s)
	if err != nil {
		return intake.Result{}, err
	}
	report, err := e.stepRow(ctx, tx, s, accepted)
	if err != nil {
		return intake.Result{}, err
	}
	pr, err := e.taskPullRequest(ctx, tx, s)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.transition(ctx, tx, s, TriggerSignOff, record.TaskPlace{}, report, pr, "")
}

// acceptedRole is the role of the step the owner's acceptance of s's task accepts: the step before
// the acceptance's place on the task's walk, by place and never by type, so the step after the
// sign-off reads the report the owner read. A run with no plan rows walks today's line, where the
// review stands before accepting.
func (e *Engine) acceptedRole(s TaskScope) (claim.Role, error) {
	w := s.walk
	if !w.Stored {
		return claim.RoleReviewer, nil
	}
	place, ok := w.PlaceOf(phase.Accepting)
	if !ok || place.Kind != record.PlaceStep || place.Position < 2 {
		return "", fmt.Errorf("sign off %s: task %s of its line %v has no step before its acceptance", s.issue.Key, w.Task, w.Line)
	}
	return claim.Role(w.Steps[place.Position-2].Role), nil
}

func (e *Engine) transition(ctx context.Context, tx pgx.Tx, s TaskScope, trigger TriggerKind, target record.TaskPlace, handoff record.PhaseRow, pr *record.PullRequest, reason string) error {
	// The row comes from the table of the task's walk: the line of its plan's task, or today's
	// line for a run with no plan rows (Walk). issue and w are s's copy and walk, read again from s
	// after each write of s below.
	issue, w := s.issue, s.walk
	// ended is the role whose step this move ends, read once before the task moves
	// (Walk.endingRole): a role the deployment defines at step is named by its own role.
	ended := w.endingRole(issue.Phase)
	snapshot := Snapshot{Phase: issue.Phase, HasOpenOrMergedPR: record.LetsImplementationMove(pr)}
	row, ok := w.row(issue.Phase, trigger, target, snapshot)
	// A failed step of a task with plan rows that names no step to return to returns its work to
	// the step its plan reads, or its task stops where no step before it can take the work
	// (returnOf). A step with no step before it has no row of its fail at all, and stops the same
	// way (returnsUnnamed).
	if !ok && !w.returnsUnnamed(issue, trigger, target) {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	row, stops, err := w.returnOf(issue, trigger, target, row, snapshot)
	if err != nil {
		return err
	}
	if stops {
		return e.stopUnreturned(ctx, tx, s, endOfTransition(w, trigger, record.StandsAt(issue), stepSite{}, handoff))
	}
	// A move into the step of a role the deployment no longer defines starts no role: the task
	// stops where it stands, naming the step (stopLostRole), and the step it stood at ends as the
	// move would have ended it. A move stands in a phase a hold stands in, so a refusal of the
	// stop is an error.
	if lost, ok := w.lostAt(row.ToPlace, e.cfg.Deployment); ok {
		refusal, err := e.stopLostRole(ctx, tx, s, lost, endOfTransition(w, trigger, record.StandsAt(issue), stepSite{}, handoff))
		if err == nil && refusal != nil {
			err = fmt.Errorf("stop task %s of %s at its lost role: %s", w.Task, issue.Key, refusal.Message)
		}
		return err
	}
	// Two counters move here, and they count different things. Every move back to an earlier phase
	// counts the task a pass: the tester's fail, a review's request for changes and a worker's
	// backward move. The pass tells each pass through a phase from the last
	// (api/handoff.go): a move that counted none would give a completion reporting the last pass's
	// carrying commit that pass's key, answered as already received instead of refused as a handoff
	// not written anew. The move's two phases go with the pass, because the move is counted under
	// its own pair and a hook of the deployment stands on that pair (recordPass,
	// fireWorkBackHooks), so a hook sees every way the work goes back and not the review's alone.
	// The round is the review's alone — it counts the review rounds review_round_cap bounds, so
	// only a review that requested changes moves it, and a send-back for anything else leaves it
	// where it stands. The pull request goes with both because each reads it: a move made while
	// the record holds this head in a confirmed conflict with its base (record.PullRequest's
	// ConflictingHead) is a conflict round, which review_round_cap does not count and no pair
	// counts either (recordRound, recordPass). Both count on the walk's task: its plan_tasks row
	// for an issue with plan rows, the implementer's phases row for an issue with none
	// (record.Store.ReturnCounts).
	//
	// The same reading goes onto the entry the start below raises, in this transaction, because
	// it is the one thing the entry fence cannot read from anywhere else: a phase re-entered by a
	// move back asks for its work to be done again, and the completion of the entry before it
	// does not answer for that however its commit reads (record.PhaseRow.EntryMovedBack,
	// api/handoff.go). It is read here and not below because issue.Phase is moved onto row.To a
	// few lines down, which would leave the start comparing a phase with itself; the start is
	// handed it with the phase the move left (enter). A move back is one by the order of the walk's
	// task (Walk.MovesBack), its places for an issue with plan rows, so a task in another order than
	// today's counts its own moves back, a move from one step that runs as step back to another
	// counts as one, and an issue with no plan rows counts by today's line.
	//
	// A parent with an open Dispatch child starts no phase of planning through accepting
	// (dispatch://INFRA-3320 R9): the guard reads its open children after the row is chosen
	// (childrenHolding), and where one holds the start the parent moves to awaiting_children in
	// its place (holdForChildren), whatever its tree's design gate reads. It is read before the
	// counts because the counts run as they run today and say that the parent waits (R10): a move
	// back still counts its pass and its pair, but no hook of the deployment fires while no role
	// of the parent has a phase, since the hooks fire after the start (fireWorkBackHooks), which
	// the move into awaiting_children returns before, and a request for changes still counts its
	// round, whose cap message says the parent waits on its children rather than that its worker
	// started (recordRound).
	open, err := e.childrenHolding(ctx, tx, issue, row.To, pr)
	if err != nil {
		return err
	}
	movedBack := w.MovesBack(row)
	var backwardMoves map[string]int
	if movedBack {
		if backwardMoves, err = e.recordPass(ctx, tx, issue, w.Task, issue.Phase, row.To, pr); err != nil {
			return err
		}
	}
	// The fail of a step of the type review that runs as step and whose agent is not the owner
	// counts a review round too (Walk.reviewFails).
	if trigger == TriggerReviewRejected || trigger == TriggerStepFailed && w.reviewFails() {
		if err := e.recordRound(ctx, tx, issue, w, pr, open); err != nil {
			return err
		}
	}
	// A parent the guard holds moves into awaiting_children in place of the start of row.To, on
	// the walk the row came from: the move names the end of the step the issue stood in as the move
	// would have, a tester's pass as passed and a fail as failed, since the parent's wait in
	// awaiting_children is what follows the step, not how it ended (holdForChildren). Its task
	// moves to the row's place, as the move would have moved it.
	if len(open) > 0 {
		return e.holdForChildren(ctx, tx, s, trigger, row.To, row.ToPlace, handoff, pr, reason, open)
	}
	// A parent whose last step is not the owner's acceptance moves into its retro by that step's
	// pass or by a move back out of merging, and while any of the three holds on its acceptance
	// holds it — an issue beneath it still in the workflow, a root that rests on its approval still
	// in the workflow, an open Dispatch child of it — Legion holds it before its retro instead
	// (retroHeld, holdBeforeRetro).
	held, err := e.retroHeld(ctx, tx, s, row.To)
	if err != nil {
		return err
	}
	if held.code != "" {
		return e.holdBeforeRetro(ctx, tx, s, trigger, row.To, row.ToPlace, handoff, pr, reason, held)
	}
	from, written, err := e.arrive(ctx, tx, &s, trigger, row.To, row.ToPlace, nil, handoff, pr, reason)
	if err != nil {
		return err
	}
	issue, w = s.issue, s.walk
	// A move back of a task with plan rows writes why the step it returns to starts on its task
	// (moveBackReason: the report of the step that sent the work back, then the move's reason), after
	// its move, which cleared the reason before it. The start of that step tells that one stored
	// reason in the reason's place, and no report of its own, whether it starts below or after a
	// wait for its agent (startAfterWait), so the two starts are told the same and each counts its
	// entry as raised by a move back.
	report, why := handoff, reason
	if movedBack && w.Stored {
		s.walk.StartReason = moveBackReason(issue, handoff, reason)
		w.StartReason = s.walk.StartReason
		if w.StartReason != "" {
			if err := e.store.SetStartReason(ctx, tx, issue.Key, issue.Generation, w.Task, w.StartReason); err != nil {
				return err
			}
		}
		report, why = record.PhaseRow{}, w.StartReason
	}
	// The role the row moves the task to: on a task with plan rows the role of the step at the
	// row's place (Walk.RoleAt), so the second of two steps that run as step starts its own role,
	// and on an issue with none the role of the phase. The merge's row keeps the task at its merge
	// while the issue goes to done, which no role works.
	starting := phase.RoleFor(row.To)
	if w.Stored && row.To != phase.Done {
		starting = w.RoleAt(row.ToPlace)
	}
	// An issue the daemon moves into retro or merging is accepted (record.Accepted), so it leaves
	// R1's first condition, and every step of another root that waits on it as an ancestor ends
	// its wait (dispatch://INFRA-3320). A step that stands open on it stays open (R3). It is read on
	// the issue as written (arrive), which stands where its tasks put it, so of a plan of several
	// tasks it is the move that leaves the issue itself in retro or merging.
	if record.Accepted(written) {
		if err := e.endPendingOn(ctx, tx, issue.Key, movedAccepted(issue.Key, written.Phase)); err != nil {
			return err
		}
	}
	if err := e.clearHandoff(ctx, tx, issue.Key, starting); err != nil {
		return err
	}
	// The move out of admitted is where the run's work starts, and so where the plan it carries
	// out is written (WritePlan): the run's plan node, the plan whose first step is the start below.
	// The node, the move's write and the start each read w, the walk this transition read once.
	if trigger == TriggerGateOpened {
		if err := e.writePlanOn(ctx, tx, issue, s.plan); err != nil {
			return err
		}
	}
	// No transition stops a worker: the claim whose phase this ends keeps its process, its role and
	// its session until its issue closes (leave), or until its agent's next step is of another
	// role, which the start of that step waits for its turn to end to give it (the outbox's
	// reroleForStep), and its phase entry ends here, not its process. The workspace passes to the
	// next role at the completion this move records, which the worker reports once its handoff is
	// committed and pushed, so the next role's start may run while that worker's turn is still
	// finishing (the issue's lifecycle lane runs its supervise rows one at a time, oldest due
	// first, and the start of a claim on the role it holds waits for no turn to end). From then on
	// the finished role answers questions read-only: a completion it reports is refused, since it no
	// longer works the issue's phase (handoff), and a task queued for a phase the issue has left is
	// dropped rather than sent (supervise.Deps's PhaseHolds). A move between two phases of one role -
	// retro back to implementing on the implementer's own backward move - hands the running worker
	// the new phase's task once its turn is over, as any start of a working claim does. A move no
	// completion records - the review App's decision on a round the reviewer had not completed -
	// starts the next role the same way: that role keeps its turn, its jobs, its process and its
	// session, and the start waits for none of them.
	// A pull request GitHub has already merged leaves the merger nothing to merge, so no merger is
	// started for it and the merge closes the issue the moment it reaches merging. The close is
	// closeMergedTelling's rather than a second transition's because a merged issue's close waits
	// for the issues beneath it still in the workflow (for a root, the rest of its tree), and an
	// issue that arrives there and waits must be told why: the notice its architect got at the
	// merge could not say it, since the issue was elsewhere then. The close of the pull requests
	// whose work that merge holds is queued here too (closeHeldPullRequests), since the merge landed
	// while the task stood short of its merge, where it queues none.
	mergedAlready := row.To == phase.Merging && pr != nil && pr.State == record.PullRequestMerged
	// A merge GitHub has already made is one of two moves that start a step no phase worker's
	// start begins: no merger runs it, so it starts here under the agent its step names
	// (startStandingStep), and its close ends it. Every other move starts what a move into its
	// phase starts (start): the phase's worker, and for accepting, whose role is the architect, a
	// child's failed architect started again and the accepting step.
	if mergedAlready {
		if err := e.startStandingStep(ctx, tx, s); err != nil {
			return err
		}
	} else if err := e.start(ctx, tx, s, starting, readPull(pr), func(at Standing) string {
		return task(issue, at, report, why)
	}, from, movedBack); err != nil {
		return err
	}
	if err := e.queueMovedNews(ctx, tx, issue); err != nil {
		return err
	}
	if err := e.notice(ctx, tx, issue.Key, noticeFor(trigger, from, row.To, handoff, reason, ended)); err != nil {
		return err
	}
	// The deployment's hooks that stand on a move back fire once its start has written the agent of
	// the round the move opened on its role's row, so each prompt names the claim of the agent that
	// works its role's step in that round (fireHook), and each prompt and the firing's message name
	// the task whose returns fired it (taskLabel, taskSentence).
	if backwardMoves != nil {
		if err := e.fireWorkBackHooks(ctx, tx, issue, w, from, row.To, backwardMoves, pr); err != nil {
			return err
		}
	}
	// The merge forward follows the move (mergeForwardOnEntry): a move into implementing drops
	// it, and a move at a head held in a confirmed conflict asks for it where the merge comes
	// next. It comes after the start, so the worker of the phase entered is started before the
	// hold that tells it to wait for the merge. It reads pr, the row the start named, as the
	// handler read it: the move reads its task's pull request once.
	if err := e.mergeForwardOnEntry(ctx, tx, s, pr); err != nil {
		return err
	}
	if mergedAlready {
		if err := e.closeHeldPullRequests(ctx, tx, s, *pr); err != nil {
			return err
		}
		return e.closeMergedTelling(ctx, tx, s, pr)
	}
	// The issue leaves the workflow where its tasks put it in done (putIssue), through the one place
	// of that leave (leaveByTasks): the move of the one task of an issue to done, and of the last
	// task of a plan of several, whose others are each done. A task that does not merge is done at
	// its last step while another task waits or runs, and the issue goes on: each task whose needs
	// that move made done starts (startReady).
	if written.Phase == phase.Done {
		return e.leaveByTasks(ctx, tx, written)
	}
	if w.State == record.TaskDone {
		_, err := e.startReady(ctx, tx, written, s.plan, trigger)
		return err
	}
	return nil
}

// arrive writes the move of issue into to on trigger, on w, the walk of its run, in the caller's
// transaction, and answers the phase the issue moved from and the issue as written (putIssue): the
// Dispatch status (Engine.status, "" for none, with reason, the move's own, where moveReason posts
// one), the end of the step the issue stood in (nameMoveEnd, on the issue as it stood: before the
// phase write, and before the start that follows ends that run and begins its own), the task's
// move with the issue's phase (moveTask), and the issue's write and its mark of movement. A hold,
// where one is given, overlays the phase the task's new place derives, which its From names, and
// holds the task too (record.Store.HoldTask), whose hold the issue's derivation reads, for every
// run with plan rows. s's copy is left standing where its task now stands, and the issue is written
// where its tasks put it (putScope). The status written is the one of the phase the issue arrives
// in (arrivedPhase), carrying the move's reason, in one row, whatever the plan's count of tasks.
func (e *Engine) arrive(ctx context.Context, tx pgx.Tx, s *TaskScope, trigger TriggerKind, to phase.Phase, place record.TaskPlace,
	hold *record.Hold, handoff record.PhaseRow, pr *record.PullRequest, reason string) (phase.Phase, record.Issue, error) {
	issue, w := &s.issue, &s.walk
	state := record.TaskRunning
	if to == phase.Done {
		state = record.TaskDone
	}
	status := PhaseStatus(arrivedPhase(*s, to, place, state, hold))
	if err := e.status(ctx, tx, *issue, status, moveReason(trigger, issue.Phase, to, w.endingRole(issue.Phase), reason)); err != nil {
		return "", record.Issue{}, err
	}
	if err := e.nameMoveEnd(ctx, *w, *issue, trigger, placeNamed(place, to), handoff); err != nil {
		return "", record.Issue{}, err
	}
	from := issue.Phase
	if err := e.moveTask(ctx, tx, s, to, place, state); err != nil {
		return "", record.Issue{}, err
	}
	issue.Hold = nil
	if hold != nil {
		overlay := *hold
		overlay.From = issue.Phase
		issue.Phase, issue.Hold = phase.Held, &overlay
		if w.Stored {
			if err := e.store.HoldTask(ctx, tx, issue.Key, issue.Generation, w.Task, overlay.Reason); err != nil {
				return "", record.Issue{}, err
			}
			w.State, w.HoldReason = record.TaskHeld, overlay.Reason
		}
	}
	// This is the one write that moves an issue into a phase of its own, so it is where the issue's
	// run earns record.Issue.TakenBack: it has stood in a phase. The mark is a fact about that run,
	// not about the issue's history. It lasts as long as the run's record does — through the phase's
	// end and its root's leave into a linger, and into the generation re-admission opens when Legion
	// takes that same tree back, which is the work the mark was earned by and so starts again whatever
	// its links say. Every other path that opens a run starts it clean: a child's re-entry into its
	// tree (enterChild) and a child handed over as a root of its own without its run
	// (admit.Admission.readmit). The invariant: no issue is exempt from the blocker check unless the
	// run it is exempt for is the run it arrived with.
	if issue.CarriesRun() {
		issue.TakenBack = true
	}
	if status != "" {
		issue.Status = status
	}
	written, err := e.putScope(ctx, tx, s)
	if err != nil {
		return "", record.Issue{}, err
	}
	// The issue has moved: this is the write that takes it into another phase, the plainest
	// movement there is (the rule, and what is left unmarked, in dispatchIssue).
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return "", record.Issue{}, err
	}
	// The journal names the task, and the phase the issue stands in beside the task's, only where
	// the plan holds a choice of tasks (textNamesTask): a plan of one task, and a run with no plan
	// rows, keep the line they always had.
	if !textNamesTask(*s) {
		e.logOnCommit(ctx, "workflow: phase changed", "issue", issue.Key, "tree", issue.Tree, "from", from, "to", to, "trigger", trigger, "status", written.Status)
	} else {
		e.logOnCommit(ctx, "workflow: phase changed", "issue", issue.Key, "tree", issue.Tree, "task", w.Task, "from", from, "to", to, "trigger", trigger,
			"status", written.Status, "issuePhase", written.Phase)
	}
	return from, written, nil
}

// arrivedPhase is the phase the issue of s stands in once s's task stands at place in state, held
// where hold is given: the phase its tasks put it in then (issuePhaseOfTasks, with s's task's walk
// moved there), and for a run with no plan rows to, or held where hold is given.
func arrivedPhase(s TaskScope, to phase.Phase, place record.TaskPlace, state record.TaskState, hold *record.Hold) phase.Phase {
	if s.plan == nil {
		if hold != nil {
			return phase.Held
		}
		return to
	}
	walks := walksOf(s.issue.Key, s.plan)
	for i := range walks {
		if walks[i].Task != s.walk.Task {
			continue
		}
		walks[i].Place, walks[i].State = place, state
		if hold != nil {
			walks[i].State, walks[i].HoldReason = record.TaskHeld, hold.Reason
		}
	}
	at, _ := issuePhaseOfTasks(walks)
	return at
}

// holdBeforeRetro is the move of a parent into to, its retro, by the pass of its task's last step
// or by a move back out of merging (TriggerBackward), on a task whose last step is not the owner's
// acceptance while held, the first of the three holds on its acceptance, holds it (retroHeld): the
// task moves to its retro's place and the issue stands in held for its children
// (record.HoldForChildren) instead. The hold writes no status, since held has none, starts no
// retro, which starts once none of the three holds it (StartHeldRetro), and keeps the last step's
// handoff for the retro's task, which no clear of a handoff takes. The architect is told the move
// and the hold, naming what holds the parent and the move that entered the hold (tellRetroHeld,
// trigger). The hold asks nothing of the merge forward: the retro is where the merge comes next
// (classify.MergeComesNext), where a move asks nothing (mergeForwardOnEntry). An ask made in
// implementing stays on the record, neither the hold nor its release (StartHeldRetro) asks it
// again, and the move into merging asks the merger.
func (e *Engine) holdBeforeRetro(ctx context.Context, tx pgx.Tx, s TaskScope, trigger TriggerKind, to phase.Phase, place record.TaskPlace,
	handoff record.PhaseRow, pr *record.PullRequest, reason string, held acceptanceHold) error {
	issue, w := s.issue, s.walk
	ended := w.endingRole(issue.Phase)
	from, _, err := e.arrive(ctx, tx, &s, trigger, to, place, &record.Hold{Reason: record.HoldForChildren}, handoff, pr, reason)
	if err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a parent is held before its retro", "issue", issue.Key, "tree", issue.Tree,
		"task", w.Task, "hold", held.code, "holding", held.names)
	if err := e.notice(ctx, tx, issue.Key, noticeFor(trigger, from, to, handoff, reason, ended)); err != nil {
		return err
	}
	return e.tellRetroHeld(ctx, tx, s.issue, s.walk, held, trigger)
}

// queueMovedNews queues the reconcile of issue's tree's news of its approved design
// (record.QueueDesignNews) when the move just written leaves the holder of issue's phase owed news
// (record.OwedNews over the record as the move leaves it). A phase worker's start carries what its
// holder is owed (the daemon's start executor), but no start carries news into accepting, since
// the engine starts no architect (start): the reconcile is what sends an owed member in accepting
// its news, as a task to the architect that owns it, beside the phase-finished notice. A move into
// any other phase queues it too, so news whose change set no start could carry yet is sent once
// the reconcile has recorded it.
//
// Each release out of held calls it after the start of the phase it releases into, as a move does
// (startAtFirstStep, retryPhase, StartHeldRetro): a hold owes the issue nothing (record.OwedNews
// names a held issue UntoldHeld), so a version approved while it was held can reach the holder of
// the phase entered with no change set recorded, which that start cannot carry and the reconcile
// records and sends.
func (e *Engine) queueMovedNews(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	facts, err := record.TreeNewsFacts(ctx, e.store, tx, e.cfg.Steps, issue.Tree)
	if err != nil {
		return err
	}
	if _, owes := record.OwedNews(facts).Member(issue.Key); owes {
		return record.QueueDesignNews(ctx, e.store, tx, issue.Tree, e.now())
	}
	return nil
}

// noticeFor is what a transition on trigger out of from into to tells the architect: that from
// finished, with its worker's handoff. Two moves tell it nothing (a notice of no kind, which notice
// drops): its own sign-off, whose only recipient would be the architect that just acted, and the
// merge that closes the issue, whose close tells the architect above a child child-closed itself
// (leave) and closes the child's own architect with its other claims, and suspends a root's
// architect with the rest of its tree. A third move names no role: the gate's opening, the one move
// out of admitted (Table), whose role is the architect. A phase-finished names the worker whose
// phase ended, and admitted has none: its role is an architect, the role that reads this notice.
//
// A worker's own backward move (TriggerBackward, request_backward_move) keeps the phase-finished
// kind the architect's prompt routes on, and says in its reason what happened and who asked: the
// phase it moved the issue to, the role that moved it and the words that role gave. The handoff
// row a move writes is empty, so without that reason the architect reads a completion with no
// summary and no verdict and cannot tell it from a worker that finished its phase.
//
// ended is the role whose step the move ends (Walk.endingRole), so a step of a role the deployment
// defines, which runs as step, is named by its own role.
func noticeFor(trigger TriggerKind, from, to phase.Phase, handoff record.PhaseRow, reason string, ended claim.Role) record.Notice {
	switch trigger {
	case TriggerGateOpened:
		return record.Notice{Kind: "phase-finished", Phase: from, Summary: handoff.Summary, Verdict: handoff.Verdict}
	case TriggerBackward:
		return record.Notice{Kind: "phase-finished", Role: ended, Phase: from,
			Reason: fmt.Sprintf("moved back to %s by the %s: %s", to, ended, reason)}
	case TriggerSignOff, TriggerPullRequestMerged:
		return record.Notice{}
	}
	return record.Notice{Kind: "phase-finished", Role: ended, Phase: from, Summary: handoff.Summary, Verdict: handoff.Verdict}
}

// StartFirstPhase starts the first phase of one issue the waiting line has just given a slot to.
// For a child it also starts the child's own architect, which owns the child while it holds its
// role, and tells the architect above the child that it started (ChildStartedNotice). It is the
// half of the old gate-opening behaviour that needs a slot, and it is deliberately the only half
// here.
//
// It is the one place a child's first phase starts the child's architect (a later phase start
// starts a failed one again, reviveArchitect), because every door into a child's run — an
// architect's release_children or rerun_child, a person's todo under a live tree, the close of the
// last issue a child waited on — reaches the child's first phase only here. A start in each door
// would start an architect for a child that then waits at admitted for a slot, and build a pod
// for it. The start is the plain one (gateOpened's), with no task: the architect's catch-up at
// its launch's ready tells it the child and every issue under it (claimReady). Starting a claim
// that already exists resumes its session, so a child that comes back — a rerun, a person's move
// back to todo — gets the architect that worked it before (in a new session once supervise's
// Machine.forgetSession has dropped the old one). A root's architect is admission's, started
// when its tree was admitted, so a root's first phase starts no architect.
//
// It replaces the two doors that used to start a first phase themselves, the gate's opening
// (advanceAdmittedTree) and a child's release (enterChild), which are gone. Neither could stay:
// an issue that started at its gate's opening would be in a worker phase with no slot, and would
// then join the line's first part by the moment its gate opened rather than by its tree root's
// Dispatch rank, so a tree that keeps filing children would take the slots a higher-ranked root
// is waiting for. An issue that has not started waits at admitted in the line's second part,
// which is ordered by that rank, and this is where it starts.
//
// Choosing the issues — a member of a live tree, standing where enterChild left it
// (phase.Admitted) with its tree's design gate open and no unfinished Dispatch issue of its own
// — happens outside the cap loop, once per admission pass, over the facts that pass already read
// (record.Waiting and the mirror rows admission loads beside it). That selection needs no slot
// and costs nothing, so a pass that stops at a held candidate still makes it, and the state's
// `blocked` still reports exactly what the deciders decided.
//
// The start itself, the child's architect's start, its log line and the notice all say that the
// issue starts its first step, so all of them wait for the slot and travel together: told for an
// issue that got no slot, the notice would tell the architect above the child that a child began
// work it has not begun, and the architect would run for a child that does none. A pass that stops
// at a held candidate therefore withholds the starts and the notice of them alike, which is the
// honest set. The notice names the phase the move reached (phaseNow), the first step of the child's
// plan, which is planning only on today's default steps.
//
// waited is every issue the mirror row still names, in Dispatch's own order, all of them done by
// now — or empty, when the issue named none or someone took its blocked-by links off while it
// waited, which the notice's reason says instead. A tree's own root gets no notice: the
// architect this would tell is the root's own, which the phase-finished of the move out of
// admitted already tells (noticeFor).
func (e *Engine) StartFirstPhase(ctx context.Context, tx pgx.Tx, issue record.Issue, waited []string) error {
	if err := e.gateOpened(ctx, tx, issue); err != nil {
		return err
	}
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		e.logOnCommit(ctx, "workflow: the root of this tree takes its slot and starts its own first phase", "issue", issue.Key)
		return nil
	}
	if len(waited) == 0 {
		e.logOnCommit(ctx, "workflow: this child names no issue it waits on any longer, so it starts",
			"issue", issue.Key, "tree", issue.Tree)
	} else {
		e.logOnCommit(ctx, "workflow: every issue this child waited on is done, so it starts",
			"issue", issue.Key, "tree", issue.Tree, "blockers", waited)
	}
	first, err := e.phaseNow(ctx, tx, issue)
	if err != nil {
		return err
	}
	return e.notice(ctx, tx, issue.Key, ChildStartedNotice(issue.Key, issue.Tree, first, waited))
}

// gateOpened moves an admitted member of a tree whose gate is open into the first step of its
// plan, behind a row creating its branch (record.IssueBranch): its first worker's start waits for
// that row, so no worker's push is the one that creates the branch. A member whose earlier row a
// linger dropped, or one admitted before the daemon queued such rows, gets its row here. The plan
// is settled first (planFirstStep). Its one caller, StartFirstPhase, runs for an issue the waiting
// line offers at admitted (record.LineFirstPhase), so the issue always stands there. Whatever the
// plan's count of tasks, each task whose needs are done starts at its first step (startReady),
// which writes the plan node after the first task's status, and the architect is told the gate's
// opening once (noticeFor). A tree that lingers starts nothing, as no move runs in it. A run
// with no plan rows, which the first step settles for no issue Legion records, moves out of
// admitted on today's line (transition on noPlanScope).
//
// A child's own architect starts between the branch row and the first worker's start
// (StartFirstPhase says why here): the plain start (childArchitectStart), with no phase, task,
// entry or slot mark, so it waits for the branch row as every start of the issue does and for no
// slot. It is queued before the first worker's start because the outbox runs an issue's starts one
// at a time, oldest first, so the architect's claim exists by that worker's launch. That is the
// order the outbox tries, not one it holds to: a start that fails and backs off holds back none of
// the rows behind it.
func (e *Engine) gateOpened(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	stored, err := e.planFirstStep(ctx, tx, issue)
	if err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.IssueBranch{Generation: issue.Generation}); err != nil {
		return err
	}
	if !claim.IsTreeRoot(issue.Key, issue.Tree) {
		if err := e.enqueue(ctx, tx, issue.Key, childArchitectStart(issue)); err != nil {
			return err
		}
	}
	if stored == nil {
		s := noPlanScope(issue)
		pr, err := e.taskPullRequest(ctx, tx, s)
		if err != nil {
			return err
		}
		return e.transition(ctx, tx, s, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{}, pr, "")
	}
	// The gate's opening is the move out of admitted (Table's one row of TriggerGateOpened): an
	// issue that has left admitted starts nothing, and tells nothing, at a second opening, and a
	// tree that lingers starts nothing, as no move runs in it.
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers || issue.Phase != phase.Admitted {
		return err
	}
	written, err := e.startReady(ctx, tx, issue, stored, TriggerGateOpened)
	if err != nil {
		return err
	}
	return e.notice(ctx, tx, issue.Key, noticeFor(TriggerGateOpened, issue.Phase, written.Phase, record.PhaseRow{}, "", ""))
}

// stopLostRole stops s's task, whose step at lost names a role the engine's deployment no longer
// defines (Walk.lostAt): the daemon restarted with settings that dropped the role while a task
// still had a step of it to run. The task stops where it stands as stop_task stops it (stopTask),
// its step ending as end says, with the reason that names the step and the role, and the task's
// owner is told once, in a task-stopped notice that names the lost role. The owner's next version
// of the plan, or a person who defines the role again and sets the root back to todo, starts the
// task again. It answers stopTask's refusal of a stop that cannot stand where the issue stands now
// (a root that waits on a person before its first step), and writes nothing then.
func (e *Engine) stopLostRole(ctx context.Context, tx pgx.Tx, s TaskScope, lost record.TaskPlace, end StepEnd) (*intake.Refusal, error) {
	role := s.walk.Steps[lost.Position-1].Role
	why := fmt.Sprintf("step %d names the role %s, which the deployment no longer defines", lost.Position, role)
	said := fmt.Sprintf("task %s of %s was stopped: %s", s.walk.Task, s.issue.Key, why)
	from := record.StandsAt(s.issue)
	if refusal, err := e.stopTask(ctx, tx, s, why, said, end); err != nil || refusal != nil {
		return refusal, err
	}
	return nil, e.notice(ctx, tx, s.issue.Key, record.Notice{Kind: "task-stopped", Role: claim.Role(role), Phase: from, Reason: said})
}

// stopLostRoles stops, at the daemon's boot, the task of each issue of the project in the workflow
// whose task still has a step to run of a role the engine's deployment no longer defines
// (Walk.lostStep), running or waiting: the settings are read at the start, so a restart is where a
// role leaves the deployment while the store still names it. A task its plan already holds
// (planHold) has nothing to stop. A task whose stop cannot stand where its issue stands now (a root
// that waits on a person before its first step) is passed over with one log line: the move into
// its first step stops it then (transition's lostAt).
func (e *Engine) stopLostRoles(ctx context.Context, tx pgx.Tx) error {
	issues, err := e.store.Issues(ctx, tx)
	if err != nil {
		return err
	}
	for _, issue := range issues {
		if issue.Project != e.cfg.Project || issue.Phase == phase.Done || record.OutOfWorkflow(issue.Status) {
			continue
		}
		// Each task of the run is read on its own scope (scopes), and each task's lost step is
		// stopped where that task stands. A stop writes the issue and its task, so each task after
		// the first stop is read again on the issue as that write left it (ScopeOf), and its write
		// derives the issue from every task as it stands.
		scopes, err := e.scopes(ctx, tx, issue)
		if err != nil {
			return err
		}
		stopped := false
		for _, s := range scopes {
			if stopped {
				current, err := e.store.Issue(ctx, tx, issue.Key)
				if err != nil {
					return err
				}
				if current == nil {
					return fmt.Errorf("stop the lost roles of %s: the issue is no longer recorded", issue.Key)
				}
				if s, err = e.ScopeOf(ctx, tx, *current, s.walk.Task); err != nil {
					return err
				}
			}
			if planHold(s.issue, s.walk.Stored) {
				continue
			}
			lost, ok := s.walk.lostStep(e.cfg.Deployment)
			if !ok {
				continue
			}
			refusal, err := e.stopLostRole(ctx, tx, s, lost, stepStopped)
			if err != nil {
				return err
			}
			if refusal != nil {
				e.logOnCommit(ctx, "workflow: a task whose step names a role the deployment no longer defines is not stopped where it stands",
					"issue", s.issue.Key, "task", s.walk.Task, "step", lost.Position, "reason", refusal.Message)
				continue
			}
			stopped = true
		}
	}
	return nil
}

// everyClaim enqueues op for every claim of issue but each claim of except (claimTargets), one row
// for each, on issue's tree and generation.
func (e *Engine) everyClaim(ctx context.Context, tx pgx.Tx, issue record.Issue, op record.SuperviseOp, reason string, except []claim.Token) error {
	targets, err := e.claimTargets(ctx, tx, issue, except)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: op, Tree: issue.Tree, Role: target.role, Claim: target.token,
			Generation: issue.Generation, Reason: reason}); err != nil {
			return err
		}
	}
	return nil
}

// claimTarget is one claim a request for every claim of an issue reaches: a stored claim by its
// token with its stored role, or a role alone (token ""), which the executor resolves to the claim
// the record gives that role's step when the row runs.
type claimTarget struct {
	role  claim.Role
	token claim.Token
}

// claimTargets is every claim of issue a request for all of them reaches (everyClaim, and the
// linger's tree close), but each claim of except: each stored claim of issue
// (record.Store.IssueClaims) by its token, whether or not its agent ever reported a handoff — its
// architect among them, which admission starts with a root's tree and a child's first phase start
// starts for a child (StartFirstPhase). Two claims can hold one role — a keyed agent on a step of
// another role, beside that role's own claim from an earlier version of the plan — so a row naming
// a role alone would reach the claim of the worker on that role's step twice and leave the other
// claim without its request. except is the claims left out, none for nil: a child's close leaves
// out the claim of each production check that stays open past it (leave), which that check's own
// close retires (closeCheck), naming the same claim (checkClaim).
//
// A claim can also come up after this read: a start already past its fences when the request's
// fact commits brings its claim up later, with no row of its own. So each role the issue can hold
// (record.IssueRoles: Legion's own and each role of the deployment that ran on it) whose worker
// claim (record.WorkerClaim) is none of the stored claims and none of except is a target by the
// role alone: the executor reaches the claim the record gives that role's step when the row runs,
// and treats a request for a claim that does not exist as done.
func (e *Engine) claimTargets(ctx context.Context, tx pgx.Tx, issue record.Issue, except []claim.Token) ([]claimTarget, error) {
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return nil, fmt.Errorf("the claims of %s: %w", issue.Key, err)
	}
	claims, err := e.store.IssueClaims(ctx, tx, project, issue.Key)
	if err != nil {
		return nil, err
	}
	rows, err := e.store.Phases(ctx, tx, issue.Key)
	if err != nil {
		return nil, err
	}
	// Every issue the record holds has a key of Dispatch's shape, so each of its roles names a
	// claim token (claim.NewToken): a failure here is a record the daemon did not write. Each claim
	// left out counts as reached, so no row by role reaches it either.
	named := make(map[claim.Token]bool, len(except))
	for _, token := range except {
		named[token] = true
	}
	var targets []claimTarget
	for _, c := range claims {
		if slices.Contains(except, c.Token) {
			continue
		}
		targets = append(targets, claimTarget{role: c.Role, token: c.Token})
		named[c.Token] = true
	}
	roles, err := record.IssueRoles(ctx, tx, e.store, issue.Key)
	if err != nil {
		return nil, err
	}
	for _, role := range roles {
		worker, err := record.WorkerClaim(project, issue.Key, role, rows)
		if err != nil {
			return nil, fmt.Errorf("the %s of %s: %w", role, issue.Key, err)
		}
		if !named[worker] {
			targets = append(targets, claimTarget{role: role})
		}
	}
	return targets, nil
}

// phaseRow is role's phases row of issue (record.PhaseRowOf), and the zero row stamped with the
// issue and the role where the record holds none: every caller that writes the row back writes
// the row of that role, and a caller that only reads it reads the zero value of each field.
func (e *Engine) phaseRow(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (record.PhaseRow, error) {
	rows, err := e.store.Phases(ctx, tx, issue)
	if err != nil {
		return record.PhaseRow{}, err
	}
	row := record.PhaseRowOf(rows, role)
	row.Issue, row.Role = issue, role
	return row, nil
}

// workerClaim is the claim of the agent that works role's step of issue now (record.WorkerClaim),
// read from the issue's phases rows in tx: what the engine tells a role's worker reaches that
// agent, a keyed agent on a step of another role included. Where the role's row runs no step of
// the plan, it is the role's own token.
func (e *Engine) workerClaim(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role) (claim.Token, error) {
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return "", err
	}
	rows, err := e.store.Phases(ctx, tx, issue.Key)
	if err != nil {
		return "", err
	}
	return record.WorkerClaim(project, issue.Key, role, rows)
}

// issueForBranch is the issue a pull request on branch is recorded for (pullRequestOpened, its one
// caller): the issue of branch's shape (claim.BranchIssue), when it is recorded on repo, as its
// record keeps GitHub's spelling (record.Issue.GitHubRepo), compared as sameRepository compares two
// names — without case, as GitHub compares names, and a configured rename's two names as one
// repository: a pull request on legion/<KEY> in another repository is never <KEY>'s. It is nil
// where a task of the issue's plan holds no such branch, or for an issue with no plan rows the
// branch is not its own (record.StoredPlan.HoldsBranch, the rule the merge's close reads too): a
// person's or a fork's pull request on a name of a task's shape that no task was given, and one
// whose row the record deleted when it let it go, delivered or reopened again, are no issue's. The
// one task of a plan of one that has not started holds the issue's own branch only where its
// generation gave that branch to no task (record.Store.BranchGiven), since its start then gives
// it a branch of its own. The task of a pull request on branch is read from the branch wherever it
// is needed (viewOfPullRequest, record.PullRequest.Task).
func (e *Engine) issueForBranch(ctx context.Context, tx pgx.Tx, repo, branch string) (*record.Issue, error) {
	key, ok := claim.BranchIssue(branch)
	if !ok {
		return nil, nil
	}
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !e.sameRepository(issue.GitHubRepo, repo) {
		return nil, err
	}
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return nil, err
	}
	holds := stored.HoldsBranch(*issue, issue.GitHubRepo, branch)
	if holds && stored.HoldsBranchBeforeItsStart(*issue, issue.GitHubRepo, branch) {
		given, err := e.store.BranchGiven(ctx, tx, issue.Key, issue.Generation, branch)
		if err != nil {
			return nil, err
		}
		holds = !given
	}
	if !holds {
		e.logOnCommit(ctx, "workflow: a pull request on a branch no task of its issue holds is not recorded",
			"issue", issue.Key, "generation", issue.Generation, "branch", branch)
		return nil, nil
	}
	return issue, nil
}

// pullRequest is the recorded pull request number of repo, when its issue is recorded on repo.
// The read is made under the name the fact carries and then, when it finds nothing, under the
// other name of the pair that names it (pullRequestUnder): a record keeps the repository it was
// written under until the daemon's one-time rename pass writes the pair's `to` over it, so a pull
// request already open when its repository was transferred is recorded under the name it moved
// from while every event after names the one it moved to. A deployment that configures no rename
// makes exactly the one query it always made; a configured one makes the second only when the
// first finds nothing, which is the common case — an event of a pull request this daemon does not
// record at all.
func (e *Engine) pullRequest(ctx context.Context, tx pgx.Tx, repo string, number int) (*record.PullRequest, error) {
	return e.pullRequestUnder(ctx, tx, repo, func(name string) (*record.PullRequest, error) {
		return e.store.PullRequestByNumber(ctx, tx, name, number)
	})
}

// pullRequestByBranch is a recorded pull request of repo on branch, when its issue is recorded
// on repo. It reads under the same names as pullRequest, in the same order and for the same
// reason.
//
// A branch can hold several open pull requests (dispatch://INFRA-5910 decision 20), and this takes
// one: the first, the lowest number, which is the task's own pull request wherever the branch has
// one open on the task's base alone. It reads no merged or closed row, as the read it replaces did:
// no fact it serves acts on one (pullRequestSynchronized and push return on a merged pull request).
// The facts that name a branch alone apply to each open pull request on it (decision 20, b2); until
// a later step does that, they apply to this one.
func (e *Engine) pullRequestByBranch(ctx context.Context, tx pgx.Tx, repo, branch string) (*record.PullRequest, error) {
	return e.pullRequestUnder(ctx, tx, repo, func(name string) (*record.PullRequest, error) {
		prs, err := e.store.OpenPullRequestsOnBranch(ctx, tx, name, branch)
		if err != nil || len(prs) == 0 {
			return nil, err
		}
		return &prs[0], nil
	})
}

// pullRequestRow and pullRequestRowByBranch are what pullRequest and pullRequestByBranch answer,
// read under the same names and owned by the same rule, as the pull request's row alone
// (record.Store.PullRequestRowByNumber, OpenPullRequestRowsOnBranch): with none of what it
// remembers of its settled heads, for a fact's handling that never writes the record back and reads
// neither its SettledHeads nor its head run's Settled.
func (e *Engine) pullRequestRow(ctx context.Context, tx pgx.Tx, repo string, number int) (*record.PullRequest, error) {
	return e.pullRequestUnder(ctx, tx, repo, func(name string) (*record.PullRequest, error) {
		return e.store.PullRequestRowByNumber(ctx, tx, name, number)
	})
}

func (e *Engine) pullRequestRowByBranch(ctx context.Context, tx pgx.Tx, repo, branch string) (*record.PullRequest, error) {
	return e.pullRequestUnder(ctx, tx, repo, func(name string) (*record.PullRequest, error) {
		prs, err := e.store.OpenPullRequestRowsOnBranch(ctx, tx, name, branch)
		if err != nil || len(prs) == 0 {
			return nil, err
		}
		return &prs[0], nil
	})
}

// pullRequestUnder is the pull request read finds under repo and then, when it finds nothing,
// under the other name of the pair that names repo (ghrepo.Under), when its issue is recorded on
// repo (ownPullRequest).
func (e *Engine) pullRequestUnder(ctx context.Context, tx pgx.Tx, repo string, read func(name string) (*record.PullRequest, error)) (*record.PullRequest, error) {
	pr, err := ghrepo.Under(e.cfg.RepositoryRenames, repo, read)
	if err != nil || pr == nil {
		return nil, err
	}
	return e.ownPullRequest(ctx, tx, pr, repo)
}

// ownPullRequest is pr when the issue it is recorded for is recorded on repo, as its record keeps
// GitHub's spelling and sameRepository compares it, and nil otherwise: a GitHub fact applies to an
// issue only when it names the issue's repository.
func (e *Engine) ownPullRequest(ctx context.Context, tx pgx.Tx, pr *record.PullRequest, repo string) (*record.PullRequest, error) {
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil || !e.sameRepository(issue.GitHubRepo, repo) {
		return nil, err
	}
	return pr, nil
}

// treeMembers is every issue of root's tree, the root included, read by tree (record.Store.TreeIssues),
// never from the project's whole record.
func (e *Engine) treeMembers(ctx context.Context, tx pgx.Tx, root record.Issue) ([]record.Issue, error) {
	return e.store.TreeIssues(ctx, tx, root.Tree)
}
