// Package workflow applies the daemon-owned phase transition table inside intake's transaction.
package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
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
// a phase, which the daemon counts by the move's own `<from>-<to>` pair (phase.BackwardPairs).
// It is the word the configuration spells as well (config.HookEventWorkBack), written out again
// here because this package imports no configuration: a hook reaches the engine with its event
// as the operator wrote it, and recordPass fires the hooks that name this one.
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
	Clock               func() time.Time
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
	// ReadMergeState is asked, on the commit of a fact that owes a read of GitHub's merge state
	// (askMergeState, basePush, through askMergeStateFor), to read it for the pull request's head:
	// a fact that leaves a review round waiting on GitHub's answer, which is what moves an approved
	// issue to accepting; and, by the gate classify.MergeStateReadOwed, a head move of an issue in
	// implementing through merging and a base push of an issue in testing through merging, each of
	// which can turn the head conflicting with its base (dispatch://INFRA-4374 R0). The daemon
	// answers it with a ladder of reads (workflowRuntime.queueMergeState); nil asks for none, which
	// is the API-only daemon and a test that drives the reads itself.
	ReadMergeState func(issue string)
	// Reserve is the moment a wake the engine causes may run, given the caller's own now and
	// never a moment before it: the daemon's one wake limiter (internal/wakepace, openSupervision
	// in internal/daemon), which paces every wake the daemon causes together — the role processes
	// supervision launches (supervise.WakePace) and the catch-up the outbox writes in place of a
	// launch, which is the one wake this engine reserves (claimReady, for an intake.ClaimReady
	// the outbox's start row marked paced). nil is the pacing off, which is this package's
	// convention for a collaborator a deployment does not have, as NoteUnread is: the catch-up is
	// then due at once, exactly as every other catch-up is. The engine reads no configuration of
	// the pacing and knows no limiter; what the rate and the burst were is the outbox's to log,
	// where the wake runs (the daemon's wakeRan).
	Reserve func(now time.Time) time.Time
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
	// experiment.CryptoSource, the daemon's own, and a test hands in one of its own.
	Experiments      []config.Experiment
	ExperimentSource func(uint64) (uint64, error)
	// StepAgent is the daemon's read of the agent of a step at the moment the engine writes the
	// step's end (endStep): the session its claim records and the model that answered it last in
	// that session (supervise.Machine.LastAnswer), read from the claim's machine without waiting
	// on a decision in flight. tx is the fact's own transaction, which holds the fact lock: a read
	// the agent's record needs is made in it, never on another connection and never a write. Nil
	// reads nothing, so a record names no session and no model, which is the API-only daemon and
	// every test that reads no agent.
	StepAgent func(ctx context.Context, tx pgx.Tx, agent claim.Token) StepAgent
}

// Engine interprets Table and writes only records and outbox rows through the supplied Store.
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

// New constructs a workflow engine, taking the shipped review round cap, production check bound
// and clock for every one of them a caller omits.
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
	if log == nil {
		log = slog.Default()
	}
	return &Engine{store: store, cfg: cfg, log: log, droppedSettlements: intake.NewRecentKeys[intake.SettlementIdentity](intake.DroppedSettlementsKept)}
}

func (e *Engine) now() time.Time { return e.cfg.Clock().UTC() }

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
	switch fact := fact.(type) {
	case intake.DispatchIssue:
		return e.dispatchIssue(ctx, tx, fact)
	case intake.GateRegistered:
		return e.gateRegistered(ctx, tx, fact)
	case intake.DispatchArtifact:
		return e.dispatchArtifact(ctx, tx, fact)
	case intake.GateAboveDispatch:
		return e.gateAboveDispatch(ctx, tx, fact)
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
		return e.push(ctx, tx, fact)
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
	case intake.MergeQueueReadDone:
		return e.mergeQueueReadDone(ctx, tx, fact)
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
	if !fact.DaemonActor {
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
func (e *Engine) releaseHeldRoot(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) error {
	released, err := e.observedIssue(ctx, tx, issue, fact)
	if err != nil {
		return err
	}
	// The release names the status write that made it (RetriedByTodo), since no caller of the
	// status route names a reason of its own (R7).
	ended, err := e.retryPhase(ctx, tx, released, issue.Hold.From, PhaseMove{Retried: RetriedByTodo}, false)
	if err != nil {
		return err
	}
	return e.enqueue(ctx, tx, issue.Key, record.OrdinaryStatusWrite(PhaseStatus(ended), fact.Status, ""))
}

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
func (e *Engine) sessionStatusWrite(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) (bool, error) {
	if fact.ActorSession == "" {
		return false, nil
	}
	agent, err := e.store.SessionClaimsTree(ctx, tx, issue.Tree, fact.ActorSession)
	if err != nil {
		return false, err
	}
	if !agent {
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
	if err := e.enqueue(ctx, tx, issue.Key, record.ReassertStatusWrite(issue.Status, fact.Status)); err != nil {
		return false, err
	}
	if agent {
		return true, nil
	}
	return true, e.notice(ctx, tx, issue.Key, StatusReassertedNotice(issue.Key, fact.ActorSession, fact.Status, issue.Status))
}

// StatusReassertedNotice is the status-reasserted notice of issue: the session that wrote a
// lifecycle status, the status it wrote, and the status the daemon set back.
func StatusReassertedNotice(issue, session, wrote, status string) texts.StatusReassertedRecord {
	return texts.StatusReassertedRecord{NoticeHead: texts.NoticeHead{Kind: texts.StatusReasserted, Issue: issue}, Session: session,
		Wrote: wrote, Status: status}
}

// observedIssue is issue with everything an observation of fact records on it: its title, rank,
// parent, label, sequence, the status Dispatch showed and the Dispatch chain its preparation
// read (Ancestry), with the recorded status kept. It writes nothing, so a caller that gives the
// issue a phase of its own puts the two in one row (releaseHeldRoot).
func (e *Engine) observedIssue(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) (record.Issue, error) {
	ancestry, err := e.Ancestry(ctx, tx, issue, fact.Ancestry)
	if err != nil {
		return issue, err
	}
	issue.Title, issue.Rank, issue.Parent, issue.LastDispatchSeq = fact.Title, fact.Rank, record.ParentOf(fact.Parent), fact.Seq
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
	return e.store.PutIssue(ctx, tx, observed)
}

// rejectStaleHandover consumes a daemon-authored todo that was written for a closed tree but
// arrived after its root came back. The durable hand-over fence is checked in intake's serialized
// transaction, not before the outbox PATCH: otherwise the reopen and this event could both act on
// the child. Keep the child in its live tree, reassert its workflow status, and fence admission
// from seeing this todo by recording the event sequence.
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
	if err := e.keepStatus(ctx, tx, issue, fact); err != nil {
		return false, err
	}
	if issue.Status != fact.Status {
		if err := e.enqueue(ctx, tx, issue.Key, record.ReassertStatusWrite(issue.Status, fact.Status)); err != nil {
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
// root's todo members.
func RecordedFact(issue record.Issue) intake.DispatchIssue {
	fact := intake.DispatchIssue{
		Key: issue.Key, Seq: issue.LastDispatchSeq, Status: issue.Status, Title: issue.Title,
		Rank: issue.Rank, HandedOver: issue.HandedOver, ResolvedRepo: issue.Repo,
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
	// it. These starts ask for no new session themselves.
	check, err := e.store.ProductionCheck(ctx, tx, child.Key)
	if err != nil {
		return err
	}
	if check != nil && check.Open() {
		if err := e.markCheckClosed(ctx, tx, check, record.CheckNotRun); err != nil {
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
// A child entering beneath an ancestor the daemon has already accepted waits for that ancestor
// to close before anything starts it (record.AcceptedAncestor, which the waiting line reads),
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
		Generation: generation, Status: fact.Status, Rank: fact.Rank, HandedOver: fact.HandedOver, LastDispatchSeq: fact.Seq, DispatchStatus: fact.Status,
		Repo: at.Repo, DefaultBranch: at.DefaultBranch, GitHubRepo: at.GitHubRepo}
	if err := e.store.PutIssue(ctx, tx, child); err != nil {
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
// rather than refused: the daemon started role on this issue, which is what gives it the claim the
// completion names (record.Store.RoleRun), and role reported no completion of the phase it was in
// (record.PhaseRow.Completed). That is this completion arriving after the daemon moved the issue
// on without it - the review event taking reviewing on - and its handoff is the findings the next
// phase reads, which a refusal would lose. Four completions are not: one from a role whose handoff
// already stands, which is reporting a phase it finished a second time; one from a role the daemon
// never started on this issue, which has held no phase of it to report; any while the issue stands
// where no role works - parked, closed or held - which is past recording work at all; and any
// while it stands at admitted. Admitted is the architect's own phase (phase.RoleFor), which
// completes nothing (handoff), and it comes before every phase a worker works, so no worker's phase
// there is one the issue moved on from: the guard keeps the refusal a completion at admitted had
// while no role was named for that phase.
func (e *Engine) leftItsPhase(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, token claim.Token) (bool, error) {
	if token == "" || issue.Phase == phase.Admitted || RoleFor(issue.Phase) == "" || phase.LeftBy(role, issue.Phase) == "" {
		return false, nil
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, role)
	if err != nil || row.Completed() {
		return false, err
	}
	run, err := e.store.RoleRun(ctx, tx, token, issue.Key, role, issue.Generation)
	if err != nil {
		return false, err
	}
	return run.Claim != nil, nil
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
	// A role can still be mid-turn in a phase the issue has left: the review event moves reviewing
	// on, so a reviewer's own request for changes starts the implementer before the reviewer's
	// handoff lands. Its completion is recorded on its own row and moves nothing (leftItsPhase),
	// and the handoff it names is the report of the run of its step the move ended
	// (reportEndedStep).
	//
	// reported is the phase this completion is of (phase.Of): the issue's own, or, for a role the
	// issue has moved on without, the phase that role has left.
	reported := phase.Of(fact.Role, issue.Phase)
	current := reported == issue.Phase
	if !current {
		left, err := e.leftItsPhase(ctx, tx, *issue, fact.Role, fact.Claim)
		if err != nil {
			return intake.Result{}, err
		}
		if !left {
			return intake.Result{Refusal: intake.NotCurrentPhase(fact.Role, issue.Phase, issue.Key)}, nil
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
	row, err := e.phaseRow(ctx, tx, fact.Issue, fact.Role)
	if err != nil {
		return intake.Result{}, err
	}
	pr, err := e.store.PullRequest(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
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
		return intake.Result{}, e.reportEndedStep(ctx, tx, *issue, reported, fact.HandoffRecord)
	}
	switch issue.Phase {
	case phase.Planning:
		return intake.Result{}, e.transition(ctx, tx, *issue, TriggerPlannerCompleted, "", row, pr, "")
	case phase.Implementing:
		if pr == nil {
			return intake.Result{}, nil
		}
		return intake.Result{}, e.implementationReady(ctx, tx, *issue, row, pr)
	case phase.Testing:
		if fact.Verdict == "fail" {
			return intake.Result{}, e.transition(ctx, tx, *issue, TriggerTesterFailed, "", row, pr, "")
		}
		if fact.Verdict == "pass" {
			return intake.Result{}, e.transition(ctx, tx, *issue, TriggerTesterPassed, "", row, pr, "")
		}
	case phase.Reviewing:
		_, err := e.settleRound(ctx, tx, *issue, row, pr, found, texts.StuckByCompletion)
		return intake.Result{}, err
	case phase.Retro:
		// The completion that ends the retro carries the rating its agent gave the plan, which
		// planRatingRefusal above took on no other completion, and a plugin before the rating
		// sends none, so its retro writes no rating.
		if err := e.transition(ctx, tx, *issue, TriggerRetroCompleted, "", row, pr, ""); err != nil || fact.PlanRating == nil {
			return intake.Result{}, err
		}
		return intake.Result{}, e.rateRetro(ctx, tx, *issue, *fact.PlanRating)
	}
	return intake.Result{}, nil
}

// claimFailed holds the issue whose phase worker's claim failed — a budget ran out — and tells the
// architect and the controller of the hold; the worker-died that comes with it is the architect's
// alone. The tree's architect failing holds nothing, since a phase is its worker's; it is told as a
// worker-died of the architect, lingering or not, and the controller hears it from a row of its
// own, because every notice of the tree that no child's own architect takes reaches that
// architect and nobody inside the tree is left to act on its own. That is why the architect's
// branch comes before the lingering tree's guard. The row sent to the architect's own role
// finishes undelivered once its claim has failed (the daemon's notice executor).
//
// The hold, the copy of it the controller reads and the worker-died beside them name the failure
// the fact carries: the one word for what failed the claim, the sentence written beside it and
// the four counts the failure left, each against the limit it ran out at (noticeFailure). The
// worker-died of a role that does not work the issue's phase names it too, since that role's
// architect decides whether to start it again on the same evidence. The tree architect's own
// worker-died carries no failure record, so it names the sentence as its detail instead, with the
// pod end when the claim holds one, or else, for a claim that failed because its process died,
// the first line of the runtime's account of that death (diedOf): it is the one notice of that
// failure the architect and the controller are sent.
//
// A child's own architect works no phase but its child's acceptance (accepting), where no worker
// runs: the daemon starts it with the child's first phase (StartFirstPhase). Its claim failing
// holds nothing, even at accepting, since its branch below comes before any read of the phase, and
// is told as a worker-died of the architect on the child, which reaches the architect above it,
// since the owner walk starts at the child's parent for an architect's own worker-died
// (record.OwningArchitect). That architect takes the child's notices from then on, because the
// failed claim no longer holds its role, until the child's architect runs again: the daemon starts
// it again at the child's next phase start (start), and so do a re-admission of the tree while the
// child stands in a phase (admit's startMidPhaseMembers) and an operator's retry. The worker-died
// names the failure, as a finished role's does, so the architect above it reads why the child's
// architect stopped; no controller copy goes with it, since an architect inside the tree is left
// to act on it. A child that left the workflow or a member of a lingering tree tells nobody,
// as below.
//
// A phase role that finished its phase stays live until its issue closes, so its claim can fail
// while another role works the issue's phase, or while the issue waits on a person: the architect
// is told in a worker-died of that role, which names the phase the issue works (Working),
// and nothing is held, since the phase is not that role's. A later start of the role relaunches its
// session (the outbox executor's retry of a failed claim). An issue that left the workflow
// suspended its claims (a parked child) or closed them (a child closed as done), and a lingering
// tree holds its members where they stood, so neither tells anyone.
//
// The checking role of a closed issue failing inside a look of its production check ends that look
// as a report of pending would (endLook): a done issue has no phase to hold, and the look's next
// wake starts the claim again.
func (e *Engine) claimFailed(ctx context.Context, tx pgx.Tx, fact intake.ClaimFailed) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if claim.IsTreeArchitect(fact.Role, issue.Key, issue.Tree) {
		died := WorkerDied(issue.Key, fact.Role, issue.Phase, nil)
		diedOf(&died, fact, false)
		return intake.Result{}, e.noticeWithController(ctx, tx, issue.Key, died)
	}
	if fact.Role == claim.RoleArchitect {
		if !record.InWorkflow(*issue) {
			return intake.Result{}, nil
		}
		if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
			return intake.Result{}, err
		}
		return intake.Result{}, e.notice(ctx, tx, issue.Key, WorkerDied(issue.Key, fact.Role, issue.Phase, noticeFailure(fact)))
	}
	// The failure of the role an open check records ends the look of its closed issue's production
	// check, as its bound would. Which role that is the check's own row says (record.ProductionCheck.Role,
	// the role it opened under), never the setting as it stands now, so a check opened before the
	// project changed its `production_check` still ends its look on its own role's failure. Unlike
	// an ordinary pending report, nobody remains to report the look or wait for
	// the next wake, so the architect is told now with the persisted launch failure. An issue in the
	// workflow (one reopened inside its linger with a look still open at the reopen) takes the
	// live-phase path below instead, where the failure holds the phase and is told; its stale look
	// ends at the look's own bound (check_look), which endLookPending keeps for the run.
	if !record.InWorkflow(*issue) {
		check, err := e.store.ProductionCheck(ctx, tx, issue.Key)
		if err != nil {
			return intake.Result{}, err
		}
		if check != nil && check.Open() && check.LookOpen && fact.Role == check.Role {
			if err := e.endLook(ctx, tx, check, "claim failed"); err != nil {
				return intake.Result{}, err
			}
			died := WorkerDied(issue.Key, fact.Role, "", noticeFailure(fact))
			died.Look = check.Looks
			if died.Failure == nil {
				// A failure that recorded no cause (a fact an older daemon recorded) still names
				// why the claim failed, in the sentence its site wrote. That sentence is text the
				// daemon did not write, so it is carried on one line and bounded, as every
				// worker-died's detail is (oneBoundedLine); a failure record keeps it whole.
				died.Detail, died.DetailCut = oneBoundedLine(fact.Reason)
			}
			return intake.Result{}, e.notice(ctx, tx, issue.Key, died)
		}
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers || issue.Phase == phase.Done {
		return intake.Result{}, err
	}
	working := issue.WorkingPhase()
	failure := noticeFailure(fact)
	if phase.RoleFor(working) != fact.Role {
		died := WorkerDied(issue.Key, fact.Role, issue.Phase, failure)
		died.Working = working
		return intake.Result{}, e.notice(ctx, tx, issue.Key, died)
	}
	if issue.Phase == phase.Held {
		return intake.Result{}, nil
	}
	from := issue.Phase
	issue.Phase, issue.Hold = phase.Held, &record.Hold{From: from}
	if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
		return intake.Result{}, err
	}
	// A claim that failed takes its issue into held, which is a phase change and so a movement
	// (the rule in dispatchIssue). The early return above is what keeps it honest: an issue
	// already held writes nothing here, so a second report of the same dead claim moves the
	// tree nowhere. It matters for the measure rather than for the record: a tree stops at the
	// moment its worker dies, and marking the movement here is what makes the quiet time run
	// from that moment instead of from whatever last touched the issue before it.
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return intake.Result{}, err
	}
	if err := e.noticeWithController(ctx, tx, issue.Key, HeldFailureNotice(issue.Key, fact.Role, from, failure)); err != nil {
		return intake.Result{}, err
	}
	died := WorkerDied(issue.Key, fact.Role, from, failure)
	diedOf(&died, fact, failure != nil)
	return intake.Result{}, e.notice(ctx, tx, issue.Key, died)
}

// WorkerDied is the worker-died notice of issue that role's claim failed, at the issue's phase
// (empty where the notice names none, as a production check's look does), with the failure where
// the daemon recorded its cause, its texts on one line (failureOnOneLine).
func WorkerDied(issue string, role claim.Role, at phase.Phase, failure *record.NoticeFailure) texts.WorkerDiedRecord {
	return texts.WorkerDiedRecord{NoticeHead: texts.NoticeHead{Kind: texts.WorkerDied, Issue: issue}, Role: role, Phase: at,
		Failure: failureOnOneLine(failure)}
}

// HeldFailureNotice is the held notice of issue that role's claim failed in the phase from, which
// it now holds, with the failure where the daemon recorded its cause (texts.HoldFailure), its
// texts on one line (failureOnOneLine).
func HeldFailureNotice(issue string, role claim.Role, from phase.Phase, failure *record.NoticeFailure) texts.HeldRecord {
	return texts.HeldRecord{NoticeHead: texts.NoticeHead{Kind: texts.Held, Issue: issue}, Role: role, Phase: from, Hold: texts.HoldFailure,
		Failure: failureOnOneLine(failure)}
}

// failureOnOneLine is failure as a notice carries it: its sentence, its account of the death and
// its account of the start each whole and on one line (lineJoined), by the rule the worker-died's
// own detail and death follow. The account of a death ends in a tail of the role's log over many
// lines, a sentence of refused turns holds the provider's message, and a start's account can run
// over lines too, so as they came each would be a value over several lines, which the card that
// shows the notice can write only with an escaped new line. It is a copy where any of the three
// holds a line break, and failure itself, nil included, where none does: the failure the caller
// holds is never written, so the held notice and the worker-died built from one value name the
// same failure, and the claim keeps each text as it came (supervise.Claim.Failure,
// LastDeathCause and LastStartFailure), where the claim's failure record that read_record serves
// reads it (api's FailureViewOf).
func failureOnOneLine(failure *record.NoticeFailure) *record.NoticeFailure {
	if failure == nil || !hasLineBreak(failure.Detail) && !hasLineBreak(failure.Death) && !hasLineBreak(failure.StartFailure) {
		return failure
	}
	written := *failure
	written.Detail, written.Death, written.StartFailure = lineJoined(failure.Detail), lineJoined(failure.Death), lineJoined(failure.StartFailure)
	return &written
}

// diedOf puts on died what ended the claim's process, on the two paths that name nothing else of
// it — the tree architect's and the held phase's. It names the supervisor's own sentence
// (supervise.Claim.Failure, intake.ClaimFailed.Reason) as the notice's detail, then what ended the
// process: the pod end it was last observed to have died of (supervise.Claim.PodEnd), which is the
// cause no role chose — an eviction for ephemeral storage above all — or, where the claim holds no
// pod end and failed because its process died, the first line of the runtime's account of that
// death (supervise.Claim.LastDeathCause, intake.ClaimFailed.Death) as its death. Any of them the
// fact does not hold is left out. The sentence and the death are each one line and cut to a bound
// (oneBoundedLine), and the notice says where it cut one (DetailCut, DeathCut), so neither a
// provider's message nor a role's log writes a line of its own into the notice, and the notice's
// size is one this package chose; the pod end is the kubelet's reason and message, bounded where it
// is built (runtime.PodEnd). Each is a field of its own, so nothing in the sentence reads as the
// pod end or the death: a provider's message that ends in `the pod ended: Evicted: …` stays inside
// the detail. All three are persisted on the claim, so the boot's replay of a failure no daemon
// applied says the same thing (the daemon's replayTerminal).
//
// The death is named only where there is no pod end, because on a pod the cluster ended it is
// the runtime's whole detail (sandbox's Runtime.ended): the pod's reason and the kubelet's
// message, which are the pod end, and after them a tail of the role's log and the container's.
// Its first line holds that reason and message too, so beside the pod end it would name the
// kubelet's message twice.
//
// It is named only where the fact's cause is supervise.CauseDied, too: the death is written at
// every death of the claim and cleared by nothing (supervise's died), so a claim that died, was
// launched again and then failed for something else — its provider's refusals, its prompt
// retirements, an agent that never registered — still holds the account of the earlier death,
// and beside that failure the notice would name an end that did not cause it: an old eviction
// beside a provider error, which reads as the cluster's fault. A fact whose cause is "" or
// `unrecorded` does not say the process's death failed it either, so it names no death. The pod
// end needs no such test: each launch clears it (supervise's launchNow), so a fact that carries
// one failed with no launch since the death that wrote it.
//
// failureCarried says the notice carries a failure record, as the held phase's does whenever the
// fact names a cause. That record's detail already carries the sentence and its death the
// runtime's account (noticeFailure), so such a notice names the pod end alone, since repeating
// either would say one thing twice. The tree architect's notice carries no failure record, and
// neither does a held phase's whose fact names no cause, so they name the sentence and the pod
// end, and the tree architect's, where there is no pod end and its claim failed because it died,
// the death: without them the notice would say nothing of why the claim failed. A held phase's
// fact that names no cause does not name died either, so it names no death.
//
// The two other paths name what they name: a role that does not work the issue's phase names the
// phase the issue works (Working) beside its failure record, and a checking role of a closed
// issue names its look (Look) beside its failure record, or the sentence as its detail where the
// fact names no cause (claimFailed).
func diedOf(died *texts.WorkerDiedRecord, fact intake.ClaimFailed, failureCarried bool) {
	if !failureCarried && fact.Reason != "" {
		died.Detail, died.DetailCut = oneBoundedLine(fact.Reason)
	}
	switch {
	case fact.PodEnd != "":
		died.PodEnd = lineJoined(fact.PodEnd)
	case !failureCarried && fact.Cause == supervise.CauseDied && fact.Death != "":
		died.Death, died.DeathCut = oneBoundedLine(deathFirstLine(fact.Death))
	}
}

// oneBoundedLine is a text the daemon did not write as a worker-died carries it in a field of
// its own — the supervisor's sentence, the first line of the runtime's account of a death — and
// whether the bound cut it: the text on one line (lineJoined) and cut to runtime.PodEndRunes
// runes, the bound the pod end beside it is cut to. For a run of refused turns the sentence holds
// the provider's message as the role's own plugin posted it (supervise's refusalCause, api's
// turn-outcome route), which can run to a mebibyte over many lines; the claim and a failure
// record's `detail` keep it whole (noticeFailure). The text is cut at the bound before its line
// breaks are written, so no more of a mebibyte is read than the bound keeps, and again after,
// since " / " is longer than the break it replaces. The cut falls between two characters, never
// inside one.
func oneBoundedLine(text string) (string, bool) {
	cut := false
	if at := runeOffset(text, runtime.PodEndRunes); at < len(text) {
		text, cut = text[:at], true
	}
	text = lineJoined(text)
	if at := runeOffset(text, runtime.PodEndRunes); at < len(text) {
		text, cut = text[:at], true
	}
	return text, cut
}

// lineJoined is text as a worker-died writes a text of the runtime's or the supervisor's on one
// line: each run of line breaks between two pieces of the text written as " / " and any at either
// end dropped. The pod end goes through it too, since a kubelet's message is the cluster's text and
// nothing holds it to one line. Text with no line break is returned as it is.
func lineJoined(text string) string {
	if !hasLineBreak(text) {
		return text
	}
	return strings.Join(strings.FieldsFunc(text, isLineBreak), " / ")
}

// hasLineBreak is whether text holds a character that breaks a line (isLineBreak).
func hasLineBreak(text string) bool {
	return strings.IndexFunc(text, isLineBreak) >= 0
}

// lineEscaped is text on one line with every character of it kept: each character that breaks a
// line (isLineBreak) written as Go writes it in a string, `\n` for a line feed and `\u2028` for
// the line separator, and every other byte as it was. Text with no line break is returned as it
// is.
func lineEscaped(text string) string {
	at := strings.IndexFunc(text, isLineBreak)
	if at < 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text) + 8)
	for at >= 0 {
		r, size := utf8.DecodeRuneInString(text[at:])
		quoted := strconv.QuoteRune(r)
		b.WriteString(text[:at])
		b.WriteString(quoted[1 : len(quoted)-1])
		text = text[at+size:]
		at = strings.IndexFunc(text, isLineBreak)
	}
	b.WriteString(text)
	return b.String()
}

// linesEscaped is list with each item lineEscaped, and list itself where no item holds a line
// break, so the list the caller holds is never written to.
func linesEscaped(list []string) []string {
	if !slices.ContainsFunc(list, hasLineBreak) {
		return list
	}
	escaped := make([]string, len(list))
	for i, item := range list {
		escaped[i] = lineEscaped(item)
	}
	return escaped
}

// isLineBreak is whether r ends a line where the notice is read: a line feed, a carriage return,
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

// deathFirstLine is the part of a death the worker-died carries where it carries no failure
// record: its first line — the verdict and the start of the runtime's account — without the
// header of a log tail that ends it. The sandbox runtime ends that line with "; last lines of
// <role>:" and writes the tail of the role's log and of the container's on the lines after it,
// among them lines a role wrote: carried whole, each of those became a line of the architect's
// notice. The whole account stays on the claim (supervise.Claim.LastDeathCause), where an
// operator reads it, and in the `death` of the failure record a notice carries when the fact
// names a cause (noticeFailure). The line itself can run long — for a pod left unscheduled past
// the boot timeout it holds the scheduler's message and the pod's events joined (sandbox's
// Runtime.verdict) — so it is cut where it is written (diedOf, oneBoundedLine).
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
	if err := e.notice(ctx, tx, issue.Key, VolumeNearFullNotice(issue.Key, report)); err != nil {
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

// VolumeNearFullNotice is the volume-near-full notice of issue: one sweep of its pod's volume as
// the pod's launcher reported it, with the pod the session named (texts.VolumeSweep). The notice
// states the sweep's facts and writes no sentence of them. Every string of it comes from the pod
// — a path two levels under /legion, which a role of the pod chose (/legion/scratch is writable
// by the role uid), the reason the kernel gave for a refused read, the trimmer's stop reason and
// its faults, each a recovered panic with its stack, the trimmer's identifier and what raised its
// episode — and each is written on one line with every character kept (lineEscaped): a value of
// a daemon text is one line of the card that shows it, a name a role chose is told as it is on
// the disk, and a fault keeps its stack. The lists the caller's report holds are copied before
// any of them is written, never written in place. How the architect reads the fields — a sweep
// told in the past, since the frame carries no time for it and the launcher holds a report
// through an outage; a negative freedBytes as the volume growing while the sweep ran; a size as
// the apparent size of the files at or under its path, with a file linked under several names
// counted under each; a size with a refused read as a lower bound; the bound that left entries
// out of a list; a line break written as Go writes it; and the report of a switched-off trimmer,
// which no sweep made — is the architect's prompt's (prompts/go/architect-common.md), as every
// reading guide of a daemon text is.
func VolumeNearFullNotice(issue string, sweep record.VolumeNearFull) texts.VolumeNearFullRecord {
	sweep.PodUID, sweep.Trimmer = lineEscaped(sweep.PodUID), lineEscaped(sweep.Trimmer)
	sweep.RaisedBy, sweep.StopReason = lineEscaped(sweep.RaisedBy), lineEscaped(sweep.StopReason)
	sweep.RefusedReads, sweep.Faults = linesEscaped(sweep.RefusedReads), linesEscaped(sweep.Faults)
	if slices.ContainsFunc(sweep.Sizes, func(size record.VolumeSize) bool {
		return hasLineBreak(size.Path) || hasLineBreak(size.Refused)
	}) {
		sizes := make([]record.VolumeSize, len(sweep.Sizes))
		for i, size := range sweep.Sizes {
			size.Path, size.Refused = lineEscaped(size.Path), lineEscaped(size.Refused)
			sizes[i] = size
		}
		sweep.Sizes = sizes
	}
	if slices.ContainsFunc(sweep.SizesMissing, func(missing record.VolumeMissing) bool {
		return hasLineBreak(missing.Path) || hasLineBreak(missing.Reason)
	}) {
		missing := make([]record.VolumeMissing, len(sweep.SizesMissing))
		for i, one := range sweep.SizesMissing {
			one.Path, one.Reason = lineEscaped(one.Path), lineEscaped(one.Reason)
			missing[i] = one
		}
		sweep.SizesMissing = missing
	}
	return texts.VolumeNearFullRecord{NoticeHead: texts.NoticeHead{Kind: texts.VolumeNearFull, Issue: issue}, Volume: &sweep}
}

// sessionLost tells the issue, its architect and the controller that a worker forgot its recorded
// session, so the agent working the issue now has read none of its own earlier work: it runs from
// a fresh session, and only what it pushed and committed survives. Nothing of the issue moves — the
// worker is running — and nothing is held: the report is the whole point, since a worker that lost
// its memory is otherwise indistinguishable from one that is merely slow. An issue the record does
// not hold tells nobody.
//
// The fact's cause chooses the words of the message posted on the issue, the notice's kind and
// the warning line. A session no store holds (gone, and a fact with no cause) and a Claude Code
// claim's session with no transcript file are told as lost, in a session-lost notice
// (SessionLostNotice). A session that the pod's launcher, older than this release, could not look
// up by its id, and that Oh My Pi did not resume by that id, is told as forgotten, in a
// session-forgotten notice (SessionForgottenNotice), and nothing says it was lost: nothing of it
// was deleted, and a store may hold it still (dispatch://INFRA-4108 R3). The kind is part of what
// the architect and the controller hear, since the outbox publishes each notice as `<kind> on
// <issue>` (the daemon's noticeSummary), so that forget has a kind of its own.
//
// The detail of a lost session can be the runtime's own account of the failed resume, which
// carries a session file name and a transport error from outside the daemon. The message posted
// on the issue is read by a person and quotes it as a Go string (strconv.Quote), so a quote or a
// newline inside it is escaped rather than closing the one around it; the notice carries it as a
// field of its own. sessionStoreFault does the same with its own detail. The message of a
// forgotten session carries no detail: it names the session the claim forgot
// (intake.SessionLost.Session), which the agent's registration chose, quoted the same way, and
// its notice carries that session as a field of its own.
func (e *Engine) sessionLost(ctx context.Context, tx pgx.Tx, fact intake.SessionLost) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	var notice any = SessionLostNotice(issue.Key, fact.Role, issue.Phase, fact.Detail)
	told := fmt.Sprintf("The %s of %s lost the session it recorded: %s. It is running a fresh agent, which has read none of its own earlier turns; only what it pushed and committed survives.",
		fact.Role, issue.Key, strconv.Quote(fact.Detail))
	line := "workflow: a worker's recorded session was in no session store"
	fields := []any{"issue", issue.Key, "role", fact.Role, "detail", fact.Detail}
	if fact.Cause == supervise.SessionLossNotResumedByID {
		notice = SessionForgottenNotice(issue.Key, fact.Role, issue.Phase, fact.Session)
		told = fmt.Sprintf("The %s of %s forgot the session it recorded, %s: its pod's launcher is older than this release and could not look the session up by its id, and Oh My Pi did not resume it by that id. "+
			"The daemon deleted nothing of that session. The %s is running a fresh agent, which has read none of its own earlier turns; only what it pushed and committed survives.",
			fact.Role, issue.Key, strconv.Quote(fact.Session), fact.Role)
		line = "workflow: a worker forgot its recorded session, which its pod's launcher could not look up and Oh My Pi did not resume by its id"
		fields = []any{"issue", issue.Key, "role", fact.Role, "session", fact.Session, "cause", fact.Cause, "detail", fact.Detail}
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.MessagePost{Body: told}); err != nil {
		return intake.Result{}, err
	}
	e.warnOnCommit(ctx, line, fields...)
	return intake.Result{}, e.noticeWithController(ctx, tx, issue.Key, notice)
}

// SessionLostNotice is the session-lost notice of issue: the role whose recorded session is in no
// session store, the issue's phase and the runtime's own account of the failed resume.
func SessionLostNotice(issue string, role claim.Role, at phase.Phase, detail string) texts.SessionLostRecord {
	return texts.SessionLostRecord{NoticeHead: texts.NoticeHead{Kind: texts.SessionLost, Issue: issue}, Role: role, Phase: at, Detail: detail}
}

// SessionForgottenNotice is the session-forgotten notice of issue: the role whose claim forgot the
// session it recorded, the issue's phase, the session forgotten, and why it was forgotten, as
// fields (dispatch://INFRA-4108 R3): the pod's launcher could not look the session up by its id
// (launcher_lookup: failed), and Oh My Pi did not resume it by that id (resumed_by_id: false).
func SessionForgottenNotice(issue string, role claim.Role, at phase.Phase, session string) texts.SessionForgottenRecord {
	return texts.SessionForgottenRecord{NoticeHead: texts.NoticeHead{Kind: texts.SessionForgotten, Issue: issue}, Role: role, Phase: at,
		Session: session, LauncherLookup: texts.LookupFailed, ResumedByID: false}
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
	notice, err := record.ControllerNoticeOf(SessionStoreFaultNotice(issue.Key, fact.Role, issue.Phase, fact.Detail))
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.enqueue(ctx, tx, issue.Key, notice)
}

// SessionStoreFaultNotice is the session-store-fault notice of issue: the role whose start the
// session store could not answer, the issue's phase, the runtime's own account, and how often the
// daemon tries again (supervise.SessionStoreRetry).
func SessionStoreFaultNotice(issue string, role claim.Role, at phase.Phase, detail string) texts.SessionStoreFaultRecord {
	return texts.SessionStoreFaultRecord{NoticeHead: texts.NoticeHead{Kind: texts.SessionStoreFault, Issue: issue}, Role: role, Phase: at,
		Detail: detail, RetryEvery: supervise.SessionStoreRetry.String()}
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
// The report's cause chooses the record's cause and the pod's reading it names: the kernel's
// (texts.KillKernel) for shimwire.MemoryKillCauseKernel, a command of the role's; the kernel's for
// one of the processes that run the role's session (texts.KillKernelProtected) for
// shimwire.MemoryKillCauseKernelProtected, each beside the pod's memory counter; and the guard's
// (texts.KillGuard) for shimwire.MemoryKillCauseGuard, the only other cause the launcher session
// lets through (shimwire.LauncherMemoryKill.Validate), beside the pod's anonymous memory reading.
// The line logged names the cause.
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
	rec := MemoryKillMessage(issue.Key, report)
	told := "workflow: the role whose process the memory guard ended is told why"
	switch rec.Cause {
	case texts.KillKernel:
		told = "workflow: the role whose process the pod's kernel stopped is told why"
	case texts.KillKernelProtected:
		told = "workflow: the role whose session's own process the pod's kernel stopped is told why"
	}
	reader, _ := fact.Token.Role()
	if err := e.roleMessage(ctx, tx, issue.Key, fact.Token, rec, texts.UrgencyOf(texts.MemoryKill, reader, texts.Variant{})); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, told,
		"issue", issue.Key, "role", fact.Token, "pod", fact.PodUID, "guard", report.Guard, "sequence", report.Sequence, "pid", report.PID,
		"cause", report.Cause, "sizeBytes", report.SizeBytes, "readingBytes", report.ReadingBytes, "counterBytes", report.CounterBytes,
		"limitBytes", report.LimitBytes)
	return intake.Result{}, nil
}

// MemoryKillMessage is the memory-kill message of issue for report: what stopped the process (a
// texts.Kill word), the process by its executable as the launcher sent it (the base name escaped
// and cut to shimwire.MemoryKillExecutableBytes there, with none of its arguments, which the role
// that started it already has), its pid, how much it held, and the pod's memory against its
// limit: the anonymous memory reading for the guard, the memory counter for the kernel. The sizes
// are binary units a reader can compare at a glance (binarySize).
func MemoryKillMessage(issue string, report shimwire.LauncherMemoryKill) texts.MemoryKillRecord {
	cause, pod := texts.KillGuard, report.ReadingBytes
	switch report.Cause {
	case shimwire.MemoryKillCauseKernel:
		cause, pod = texts.KillKernel, report.CounterBytes
	case shimwire.MemoryKillCauseKernelProtected:
		cause, pod = texts.KillKernelProtected, report.CounterBytes
	}
	return texts.MemoryKillRecord{RoleHead: texts.RoleHead{Kind: texts.MemoryKill, Issue: issue}, Cause: cause, Process: report.Executable,
		PID: report.PID, Size: binarySize(report.SizeBytes), PodMemory: binarySize(pod), Limit: binarySize(report.LimitBytes)}
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
// The hold, the copy of it the controller reads and the worker-died are built from this one
// value, so the three cannot name the same failure differently. It is nil for a fact carrying no
// cause — a failure recorded before the daemon kept one, replayed at a boot — and such a notice
// carries no failure record at all rather than a record of zeroes, which would read as a claim
// that failed with every budget untouched.
//
// The notice builders write its texts on one line (WorkerDied, HeldFailureNotice,
// failureOnOneLine); this value keeps them as the fact gave them.
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

func (e *Engine) retryOrEscalate(ctx context.Context, tx pgx.Tx, fact intake.RetryOrEscalate) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || issue.Phase != phase.Held || issue.Hold == nil {
		return intake.Result{}, err
	}
	if fact.Decision == intake.EscalateDecision {
		// Recorded on the issue as well as sent: the controller reads holds from the record at every
		// start, so an escalation made while none ran still reaches it.
		issue.Hold.Reason = record.HoldEscalated
		if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
			return intake.Result{}, err
		}
		// The architect's own words where it named any, beside the cause the hold already names:
		// the controller must act on this hold, and why the architect escalated it is what it
		// acts on (HeldEscalationNotice).
		return intake.Result{}, e.noticeWithController(ctx, tx, issue.Key, HeldEscalationNotice(issue.Key, issue.Hold.From, fact.Reason, e.now()))
	}
	if fact.Decision != intake.RetryDecision {
		return intake.Result{}, nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return intake.Result{}, err
	}
	// The architect's own words where it named any, which the restarted role's task and the log
	// line for the release both carry.
	_, err = e.retryPhase(ctx, tx, *issue, issue.Hold.From, PhaseMove{Retried: RetriedByArchitect, Reason: fact.Reason}, false)
	return intake.Result{}, err
}

// HeldEscalationNotice is the held notice of issue whose architect escalated its hold from the
// phase from: the reason word escalated, which both agents' texts branch on, and the architect's
// own words, in a field of their own (record.Notice.Escalation), named in the words object as the
// architect's at at, where it gave any.
func HeldEscalationNotice(issue string, from phase.Phase, words string, at time.Time) texts.HeldRecord {
	held := texts.HeldRecord{NoticeHead: texts.NoticeHead{Kind: texts.Held, Issue: issue}, Phase: from, Hold: texts.HoldEscalation,
		Reason: string(record.HoldEscalated), Escalation: words}
	if words != "" {
		held.Words = map[string]texts.WordsMeta{"escalation": {By: string(claim.RoleArchitect), At: at}}
	}
	return held
}

// restartPhase is the operator's retry of a phase worker through `legion claims retry` (api's
// retryClaim): the worker failed, or an operator stopped it, and a stop is no death, so no hold
// may be waiting for its architect to decide. The phase the worker works is started again, which
// is what gives it the task of that phase; an issue held for this role is restored from its hold
// first, as the architect's retry does. One whose phase another role works, or that has left the
// workflow, is refused, since starting that phase under this claim's name would run the wrong
// role. A restart that asks for a new session (`--fresh-session`) says so on the start it
// queues, so the claim forgets its recorded session in the retry that start makes and no refusal
// above can leave a session discarded for a restart that never happened.
func (e *Engine) restartPhase(ctx context.Context, tx pgx.Tx, fact intake.RestartPhase) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	working := issue.WorkingPhase()
	if !InWorkflow(*issue) || phase.RoleFor(working) != fact.Role {
		return refused("PHASE_NOT_RESTARTED", fmt.Sprintf("the %s does not work %s's phase %s, so nothing restarted it",
			fact.Role, issue.Key, working)), nil
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return intake.Result{}, err
	}
	if lingers {
		return refusedLingering(*issue, fmt.Sprintf("the operator's restart of the %s", fact.Role)), nil
	}
	_, err = e.retryPhase(ctx, tx, *issue, working, PhaseMove{Retried: RetriedByOperator}, fact.FreshSession)
	return intake.Result{}, err
}

// retryPhase gives issue the phase from back — a held issue leaves its hold — and starts that
// phase's worker, told who retried it (retry.Retried) and the architect's own words where it gave
// any (retry.Reason). It re-decides no CI
// result: a settlement that reached the issue while it was held moved nothing, as it would
// have moved nothing had it arrived with the issue in its phase (dispatch://INFRA-714/spec
// R1), so there is no standing red left for the retry to act on, and the restored worker reads
// the head's checks itself. From
// reviewing a review round held open may already have both of its halves - the reviewer
// completed before the hold, and its review was recorded while held - and ends here
// (reviewRound); the reviewer is started only if the round is still open, since a second
// completion of the same commit would be refused as not new. A round its reviewer completed that
// nothing would end is stuck: the reviewer's task names it (StuckRound), so the restarted reviewer
// decides it. The architect, who ordered the retry, is told nothing: a notice now would read as the
// reviewer's answer to its request.
//
// freshSession is the operator's `--fresh-session`: the start it queues retries the claim in a
// new session rather than the one it recorded. An architect's retry of a held phase asks for
// none — a claim the provider refused out of its budget takes a new session all the same, which
// the claim's own retry decides (supervise's freshSession).
//
// The release is logged as it commits: the issue, the phase it comes back to, who retried it and
// the architect's own words where its retry named any.
//
// Its callers have established that the tree does not linger, which is what holds every member
// where it stood.
//
// It answers the phase the issue ends in, which the caller that must write a status of its own
// reads (releaseHeldRoot): from, where nothing moved, and otherwise the phase the move took the
// issue to, read back from the record because the move's own row chose that target
// (transition).
func (e *Engine) retryPhase(ctx context.Context, tx pgx.Tx, issue record.Issue, from phase.Phase, retry PhaseMove, freshSession bool) (phase.Phase, error) {
	issue.Phase, issue.Hold = from, nil
	if err := e.store.PutIssue(ctx, tx, issue); err != nil {
		return "", err
	}
	// The retry puts the issue back in the phase its worker failed in, which is a phase write
	// like any other, so it is a movement (the rule in dispatchIssue).
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return "", err
	}
	// The daemon's own line for the release (R11), written whether or not a worker is started
	// below: the hold is gone either way, and who retried it is what the restarted role's task
	// names.
	e.logOnCommit(ctx, "workflow: a held phase is released", "issue", issue.Key, "phase", from, "retried", retry.Retried, "reason", retry.Reason)
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return "", err
	}
	if from == phase.Reviewing {
		reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
		if err != nil {
			return "", err
		}
		// The retry tells nothing of a stuck round: passed as its own before, it is stuck the same way.
		r := e.reviewRound(issue, reviewer, pr)
		settled, err := e.settleRound(ctx, tx, issue, reviewer, pr, r, "")
		if err != nil {
			return "", err
		}
		if settled {
			return e.phaseNow(ctx, tx, issue)
		}
		if r.outcome == roundStuck {
			retry.Stuck = r.stuck()
		}
	}
	retry.At = e.now()
	return from, e.restart(ctx, tx, issue, phase.RoleFor(from), task(issue, record.PhaseRow{}, pr, retry), freshSession)
}

// backwardRefusal is why the guard refuses fact, and "" where it does not: the one sentence that
// names which of the guard's three readings failed. served is whether the table holds a backward
// row of the issue's phase for the move's target.
func backwardRefusal(issue record.Issue, fact intake.BackwardMove, served bool) string {
	switch {
	case fact.To == "":
		return fmt.Sprintf("a backward move names the phase it moves to; the %s's move of %s named none, so it changed nothing",
			fact.Requester, issue.Key)
	case !served:
		return fmt.Sprintf("a backward move reaches an earlier phase, and no backward row of %s serves %s; the %s's move of %s changed nothing",
			issue.Phase, fact.To, fact.Requester, issue.Key)
	case phase.RoleFor(issue.Phase) != fact.Requester:
		return fmt.Sprintf("a backward move is the role running the issue's phase: %s of %s is the %s's, so the %s's move to %s changed nothing",
			issue.Phase, issue.Key, phase.RoleFor(issue.Phase), fact.Requester, fact.To)
	}
	return ""
}

// phaseNow is the phase issue stands in now, read back after a move inside the retry wrote it
// (retryPhase): the move's own row decides which phase it reaches, so the record is asked rather
// than the target named here a second time. An issue the move took out of the record stands
// where the caller last wrote it.
func (e *Engine) phaseNow(ctx context.Context, tx pgx.Tx, issue record.Issue) (phase.Phase, error) {
	moved, err := e.store.Issue(ctx, tx, issue.Key)
	if err != nil || moved == nil {
		return issue.Phase, err
	}
	return moved.Phase, nil
}

func (e *Engine) backward(ctx context.Context, tx pgx.Tx, fact intake.BackwardMove) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	// The table is the guard, and it reads three things: the move names a target, a backward row
	// of the issue's phase serves that target - anything but an earlier phase has none - and the
	// caller is the role running that phase. An empty target is read first, since the row lookup
	// takes it to match any row.
	//
	// The refusal says which of the three failed, because the three are different mistakes and a
	// sentence naming only the last had two readers of this file guess wrong on one day: a target
	// the table does not serve is the caller's own phase misread, while a requester mismatch is a
	// role acting on a phase that is not its own.
	_, served := e.row(issue.Phase, TriggerBackward, fact.To, Snapshot{Phase: issue.Phase})
	if why := backwardRefusal(*issue, fact, served); why != "" {
		return refused("BACKWARD_REFUSED", why), nil
	}
	// The move posts its reason on the issue, so a reason Dispatch would refuse there is refused
	// here, before anything moves (backwardReasonRefusal).
	if code, why := backwardReasonRefusal(issue.Phase, fact.To, fact.Reason); code != "" {
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
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.transition(ctx, tx, *issue, TriggerBackward, fact.To, row, pr, fact.Reason)
}

// parkRule is the condition dispatch://INFRA-2971 sets on each move of an issue to backlog,
// icebox or triage. Each refusal of this package that names such a move (park_child) carries it
// word for word, as the skills and the prompts do, so that an architect reads one rule wherever it
// reads of a park.
const parkRule = "only when a person asked for that move, or when the work is done, folded into another issue or no longer needed"

// signOff is the architect's acceptance of a reviewed head: it moves the issue from accepting to
// retro and starts the implementer there, told the review's handoff and the pull request as the
// approval told it before acceptance existed. Outside accepting it changes nothing and says so.
//
// It is refused while any issue beneath the one being accepted is still in the workflow
// (OpenBeneath), with SIGNOFF_CHILD_IN_WORKFLOW naming them. A parent's work is proved by a head
// that carries its children's merges: accepted first, it would reach retro and the merge
// with their work not in it, which is what dispatch://INFRA-980 forbids. The acceptance
// was held this way once before (dispatch://INFRA-229) and the hold was taken off because a
// parent's approved pull request then waited outside the merge queue and went stale against its
// base; under children-first a parent starts its own phases only after its children have left,
// so by the time it is accepted there is nothing left to wait for, and the refusal is the
// backstop for a child filed after the parent started.
func (e *Engine) signOff(ctx context.Context, tx pgx.Tx, fact intake.SignOff) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if issue.Phase != phase.Accepting {
		return refused("SIGNOFF_OUTSIDE_ACCEPTING", fmt.Sprintf("%s is in phase %s, not accepting; this sign-off changed nothing", issue.Key, issue.Phase)), nil
	}
	open, err := e.openBeneath(ctx, tx, *issue)
	if err != nil {
		return intake.Result{}, err
	}
	if len(open) > 0 {
		return refused("SIGNOFF_CHILD_IN_WORKFLOW", fmt.Sprintf("%s is not accepted while these issues beneath it are still in the workflow: %s. A parent's head proves its children's merged work, so it is accepted after them: run each one to done, and sign off again. Park one with park_child "+parkRule+", and say in a comment which of those it is. Ask the person about a child that can reach neither done nor a park under that rule, and hold the acceptance until they answer",
			issue.Key, strings.Join(open, ", "))), nil
	}
	review, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil {
		return intake.Result{}, err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.transition(ctx, tx, *issue, TriggerSignOff, "", review, pr, "")
}

func (e *Engine) transition(ctx context.Context, tx pgx.Tx, issue record.Issue, trigger TriggerKind, target phase.Phase, handoff record.PhaseRow, pr *record.PullRequest, reason string) error {
	row, ok := e.row(issue.Phase, trigger, target, Snapshot{Phase: issue.Phase, HasOpenOrMergedPR: record.LetsImplementationMove(pr)})
	if !ok {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	// Two counters move here, and they count different things. Every move back to an earlier phase
	// counts the implementer a pass: the tester's fail, a review's request for changes and a
	// worker's backward move. The pass tells each pass through a phase from the last
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
	// counts either (recordRound, recordPass).
	//
	// The same reading goes onto the entry the start below raises, in this transaction, because
	// it is the one thing the entry fence cannot read from anywhere else: a phase re-entered by a
	// move back asks for its work to be done again, and the completion of the entry before it
	// does not answer for that however its commit reads (record.PhaseRow.EntryMovedBack,
	// api/handoff.go). It is read here and not below because issue.Phase is moved onto row.To a
	// few lines down, which would leave the start comparing a phase with itself.
	movedBack := phase.MovesBack(issue.Phase, row.To)
	if movedBack {
		if err := e.recordPass(ctx, tx, issue.Key, issue.Phase, row.To, pr); err != nil {
			return err
		}
	}
	if trigger == TriggerReviewRejected {
		if err := e.recordRound(ctx, tx, issue.Key, pr); err != nil {
			return err
		}
	}
	status := PhaseStatus(row.To)
	if err := e.status(ctx, tx, issue, status, moveReason(trigger, issue.Phase, row.To, reason)); err != nil {
		return err
	}
	// The move names the end of the step the issue stood in, on the issue as it stood, before the
	// phase write: the result and the return the record does not hold (nameMoveEnd). The start
	// below ends that run before it begins its own (startStep), and a move that starts nothing
	// leaves it to the fact's settle (SettleFact).
	if err := e.nameMoveEnd(ctx, issue, trigger, row.To, handoff); err != nil {
		return err
	}
	from := issue.Phase
	issue.Phase, issue.Hold = row.To, nil
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
	if err := e.store.PutIssue(ctx, tx, issue); err != nil {
		return err
	}
	// The issue has moved: this is the write that takes it into another phase, the plainest
	// movement there is (the rule, and what is left unmarked, in dispatchIssue).
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: phase changed", "issue", issue.Key, "tree", issue.Tree, "from", from, "to", row.To, "trigger", trigger, "status", issue.Status)
	if err := e.clearHandoff(ctx, tx, issue.Key, phase.RoleFor(row.To)); err != nil {
		return err
	}
	// The move out of admitted is where the run's work starts, and so where the plan it carries
	// out is written (WritePlan): the default plan, the steps whose first is the start below.
	if trigger == TriggerGateOpened {
		if err := e.WritePlan(ctx, tx, issue); err != nil {
			return err
		}
	}
	// No transition stops a worker: the role whose phase this ends keeps its process, claim and
	// session until its issue closes (leave), and its phase entry ends here, not its process. The
	// workspace passes to the next role at the completion this move records, which the worker reports
	// once its handoff is committed and pushed, so the next role's start may run while that worker's
	// turn is still finishing (the issue's lifecycle lane runs its supervise rows one at a time,
	// oldest due first, and a start waits for no turn to end). From then on
	// the finished role answers questions read-only: a completion it reports is refused, since it no
	// longer works the issue's phase (handoff), and a task queued for a phase the issue has left is
	// dropped rather than sent (supervise.Deps's PhaseHolds). A move between two phases of one role -
	// retro back to implementing on the implementer's own backward move - hands the running worker
	// the new phase's task once its turn is over, as any start of a working claim does. A move no
	// completion records - the review App's decision on a round the reviewer had not completed -
	// starts the next role the same way: that role keeps its turn, its jobs, its process and its
	// session, and the start waits for none of them.
	starting := phase.RoleFor(row.To)
	// A pull request GitHub has already merged leaves the merger nothing to merge, so no merger is
	// started for it and the merge closes the issue the moment it reaches merging. The close is
	// closeMergedTelling's rather than a second transition's because a merged issue's close waits
	// for the issues beneath it still in the workflow (for a root, the rest of its tree), and an
	// issue that arrives there and waits must be told why: the notice its architect got at the
	// merge could not say it, since the issue was elsewhere then.
	mergedAlready := row.To == phase.Merging && pr != nil && pr.State == record.PullRequestMerged
	if !mergedAlready {
		if err := e.start(ctx, tx, issue, starting, task(issue, handoff, pr, phaseMove(trigger, from, handoff, reason, e.now())), movedBack); err != nil {
			return err
		}
	}
	// Two moves start a step no phase worker's start begins, so it starts here, under the agent it
	// names (startNamedStep): a merge GitHub has already made, which no merger runs and whose close
	// ends it, and accepting, whose role is the architect, for which start above starts no worker
	// and only starts a child's failed architect again (reviveArchitect), as every phase start of a
	// child does.
	if mergedAlready || row.To == phase.Accepting {
		if step, ok := standingStep(issue); ok {
			if err := e.startNamedStep(ctx, tx, issue, step); err != nil {
				return err
			}
		}
	}
	if err := e.notice(ctx, tx, issue.Key, NoticeFor(issue.Key, trigger, from, row.To, handoff, reason, e.now())); err != nil {
		return err
	}
	// The merge forward follows the move (mergeForwardOnEntry): a move into implementing drops
	// it, and a move at a head held in a confirmed conflict asks for it where the phase entered
	// needs a head that merges. It comes after the start, so the worker of the phase entered is
	// started before the hold that tells it to wait for the merge.
	if err := e.mergeForwardOnEntry(ctx, tx, issue, from); err != nil {
		return err
	}
	if mergedAlready {
		return e.closeMergedTelling(ctx, tx, issue, pr)
	}
	if row.To == phase.Done {
		return e.leave(ctx, tx, issue, "done")
	}
	return nil
}

// phaseMove is how a transition on trigger out of from reached the phase whose task it starts
// (PhaseMove): a worker's backward move, with the words of the role that asked (reason) at now, or
// a review's request for changes, whose decision is on the reviewer's row (handoff). Every other
// move says nothing more.
func phaseMove(trigger TriggerKind, from phase.Phase, handoff record.PhaseRow, reason string, now time.Time) PhaseMove {
	switch trigger {
	case TriggerBackward:
		return PhaseMove{BackFrom: from, Reason: reason, At: now}
	case TriggerReviewRejected:
		return PhaseMove{Review: handoff.Decision}
	}
	return PhaseMove{}
}

// NoticeFor is what a transition of issue on trigger out of from into to tells the architect: that
// from finished, with its worker's handoff. Two moves tell it nothing (a nil record, which notice
// drops): its own sign-off, whose only recipient would be the architect that just acted, and the
// merge that closes the issue, whose close tells the architect above a child child-closed itself
// (leave) and closes the child's own architect with its other claims, and suspends a root's
// architect with the rest of its tree. A third move names no role: the gate's opening, the one move
// out of admitted (Table), whose role is the architect. A phase-finished names the worker whose
// phase ended, and admitted has none: its role is an architect, the role that reads this notice.
//
// A phase-finished names the phase the issue moved to (to), which the urgency rule reads: a
// reviewer's request for changes moves it to implementing, an approval to accepting. It carries
// the worker's summary cut to texts.Summary, the rest counted in the words object, with the
// handoff's reference beside it: its phase word and version (decision 9 of the plan), and the
// handoff commit.
//
// A worker's own backward move (TriggerBackward, request_backward_move) keeps the phase-finished
// kind the architect's prompt routes on, and carries in reason the words the role that moved it
// gave, whole, by that role at now: Dispatch's own bound already holds them, since the move posts
// them on the issue (backwardReasonRefusal). The handoff row a move writes is empty, so without
// those words the architect reads a completion with no summary and no verdict and cannot tell it
// from a worker that finished its phase.
func NoticeFor(issue string, trigger TriggerKind, from, to phase.Phase, handoff record.PhaseRow, reason string, now time.Time) any {
	head := texts.NoticeHead{Kind: texts.PhaseFinished, Issue: issue}
	switch trigger {
	case TriggerGateOpened:
		return texts.PhaseFinishedRecord{NoticeHead: head, Phase: from, To: to}
	case TriggerBackward:
		by := RoleFor(from)
		finished := texts.PhaseFinishedRecord{NoticeHead: head, Role: by, Phase: from, To: to, Reason: reason}
		if reason != "" {
			finished.Words = map[string]texts.WordsMeta{"reason": {By: string(by), At: now}}
		}
		return finished
	case TriggerSignOff, TriggerPullRequestMerged:
		return nil
	}
	by := phase.RoleFor(from)
	finished := texts.PhaseFinishedRecord{NoticeHead: head, Role: by, Phase: from, To: to, Verdict: handoff.Verdict, HandoffCommit: handoff.HandoffCommit}
	if handoff.HandoffRecord != "" {
		finished.HandoffPhase, finished.HandoffVersion = handoffWord(handoff.Role), handoff.HandoffVersion
	}
	if handoff.Summary != "" {
		summary, rest := texts.Bound(handoff.Summary, texts.Summary)
		finished.Summary = summary
		finished.Words = map[string]texts.WordsMeta{"summary": {By: string(by), At: handoff.CompletedAt, Rest: rest}}
	}
	return finished
}

// pullRequestRef is the reference of pr, owner/name#n (texts.PullRequestRef).
func pullRequestRef(pr record.PullRequest) string {
	return texts.PullRequestRef(pr.Repo, pr.Number)
}

func (e *Engine) row(from phase.Phase, trigger TriggerKind, target phase.Phase, snapshot Snapshot) (Row, bool) {
	for _, row := range Table {
		if row.From == from && row.Trigger == trigger && (target == "" || row.To == target) && (row.Guard == nil || row.Guard(snapshot)) {
			return row, true
		}
	}
	return Row{}, false
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
// issue starts planning, so all of them wait for the slot and travel together: told for an issue
// that got no slot, the notice would tell the architect above the child that a child began work
// it has not begun, and the architect would run for a child that does none. A pass that stops at
// a held candidate therefore withholds the starts and the notice of them alike, which is the
// honest set.
//
// waited is every issue the mirror row still names, in Dispatch's own order, all of them done by
// now — or empty, when the issue named none or someone took its blocked-by links off while it
// waited, and the notice then names no issue it waited on. A tree's own root gets no notice: the
// architect this would tell is the root's own, which the phase-finished of the move out of
// admitted already tells (NoticeFor).
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
	return e.notice(ctx, tx, issue.Key, ChildStartedNotice(issue.Key, issue.Tree, waited))
}

// gateOpened moves an admitted member of a tree whose gate is open into planning, behind a row
// creating its branch (record.IssueBranch): its planner's start, the first worker start of every
// member, waits for that row, so no planner's push is the one that creates the branch. A member
// whose earlier row a linger dropped, or one admitted before the daemon queued such rows, gets its
// row here.
//
// A child's own architect starts between the two (StartFirstPhase says why here): the plain start
// (childArchitectStart), with no phase, task, entry or slot mark, so it waits for the branch row
// as every start of the issue does and for no slot. It is queued before the planner's start
// because the outbox runs an issue's starts one at a time, oldest first, so the architect's claim
// exists by the planner's launch. That is the order the outbox tries, not one it holds to: a start
// that fails and backs off holds back none of the rows behind it.
func (e *Engine) gateOpened(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	if err := e.enqueue(ctx, tx, issue.Key, record.IssueBranch{Generation: issue.Generation}); err != nil {
		return err
	}
	if !claim.IsTreeRoot(issue.Key, issue.Tree) {
		if err := e.enqueue(ctx, tx, issue.Key, childArchitectStart(issue)); err != nil {
			return err
		}
	}
	return e.transition(ctx, tx, issue, TriggerGateOpened, "", record.PhaseRow{}, nil, "")
}

// everyClaim enqueues op for every claim an issue can hold but except's: its architect, which
// admission starts with a root's tree and a child's first phase start starts for a child
// (StartFirstPhase), and each phase worker, whether or not it ever reported a handoff. The phase
// rows record only handoffs, so they cannot list the claims; the executor treats a request for a
// claim that does not exist as done. except is the one role left out, "" for none: a merged
// child's close leaves out the role that works its production check (Engine.checkRole), whose
// claim the check's own close retires (closeCheck).
func (e *Engine) everyClaim(ctx context.Context, tx pgx.Tx, issue record.Issue, op record.SuperviseOp, reason string, except claim.Role) error {
	for _, role := range claim.Roles {
		if role == except {
			continue
		}
		if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: op, Tree: issue.Tree, Role: role, Generation: issue.Generation, Reason: reason}); err != nil {
			return err
		}
	}
	return nil
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

// issueForBranch is the issue whose branch (claim.IssueBranch) branch is, when it is recorded on
// repo, as its record keeps GitHub's spelling (record.Issue.GitHubRepo), compared as
// sameRepository compares two names — without case, as GitHub compares names, and a configured
// rename's two names as one repository: a pull request on legion/<KEY> in another repository is
// never <KEY>'s.
func (e *Engine) issueForBranch(ctx context.Context, tx pgx.Tx, repo, branch string) (*record.Issue, error) {
	key, ok := claim.BranchIssue(branch)
	if !ok {
		return nil, nil
	}
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !e.sameRepository(issue.GitHubRepo, repo) {
		return nil, err
	}
	return issue, nil
}

// pullRequest is the recorded pull request number of repo, when its issue is recorded on repo.
// The read is made under the name the fact carries and then, when it finds nothing, under the
// other name of the pair that names it (ghrepo.Under): a record keeps the repository it was
// written under until the daemon's one-time rename pass writes the pair's `to` over it, so a pull
// request already open when its repository was transferred is recorded under the name it moved
// from while every event after names the one it moved to. A deployment that configures no rename
// makes exactly the one query it always made; a configured one makes the second only when the
// first finds nothing, which is the common case — an event of a pull request this daemon does not
// record at all.
func (e *Engine) pullRequest(ctx context.Context, tx pgx.Tx, repo string, number int) (*record.PullRequest, error) {
	pr, err := ghrepo.Under(e.cfg.RepositoryRenames, repo, func(name string) (*record.PullRequest, error) {
		return e.store.PullRequestByNumber(ctx, tx, name, number)
	})
	if err != nil || pr == nil {
		return nil, err
	}
	return e.ownPullRequest(ctx, tx, pr, repo)
}

// pullRequestByBranch is the recorded pull request of repo on branch, when its issue is recorded
// on repo. It reads under the same names as pullRequest, in the same order and for the same
// reason.
func (e *Engine) pullRequestByBranch(ctx context.Context, tx pgx.Tx, repo, branch string) (*record.PullRequest, error) {
	pr, err := ghrepo.Under(e.cfg.RepositoryRenames, repo, func(name string) (*record.PullRequest, error) {
		return e.store.PullRequestByBranch(ctx, tx, name, branch)
	})
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
