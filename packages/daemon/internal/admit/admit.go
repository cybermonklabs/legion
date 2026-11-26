// Package admit owns root admission slots and the Dispatch-rank waiting line.
package admit

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Admission assigns root issues and orphans to the configured number of architect slots.
// All work occurs in the transaction supplied by intake or daemon boot reconciliation.
type Admission struct {
	store   record.Store
	engine  *workflow.Engine
	cap     int
	project string
	log     *slog.Logger
	now     func() time.Time
	// experiments are the project's declared experiments (UseExperiments,
	// config.Project.ExperimentsDeclared), drawn for each unit this admission records: every
	// tree-unit experiment when it opens a tree for the first time, and every issue-unit
	// experiment when it first records an issue (drawExperiments), each drawn with the engine's
	// random source (workflow.Engine's ExperimentSource), the one source of the daemon's draws, and
	// the one a replay of a recorded window seeds from its run (workflow.Config.ExperimentSource).
	experiments []config.Experiment
	// stalledTreeNotice is `stalled_tree_notice_hours` (UseStalledTreeNotice), how long a tree
	// of this project may be quiet before the pass tells the project's controller once that it
	// has stopped (tellStalledTrees). Zero is no bound and tells nobody, which is the
	// admission a test that configures none builds: a daemon always sets a positive number,
	// since the loader refuses zero for a key in hours and gives six hours to a file that
	// names none (config.Config.StalledTreeNotice).
	stalledTreeNotice time.Duration

	// mu guards pending, stamps, target, catchingUp, retroHeldReads, retroHeldPass and the catch-up
	// releases, the only Admission state a caller outside its own transactions touches: the daemon's
	// boot-owned poll calls Held on every tick, outside any transaction and the database's own
	// advisory fact lock, concurrently with whatever release call currently holds both while it
	// clears them, and a commit hook that runs late clears or writes them on the commit watcher's
	// goroutine (clearReleased, rememberHeldRetro).
	mu sync.Mutex
	// pending holds, for every key Reconcile found the Dispatch consumer still needs to catch up
	// before an admission decision on it is safe, the boot listing's own summary of that key —
	// recorded or not, whatever its listed status: the stream may still redeliver a stale event
	// for it (a nak, a slow ack, the outbox's own publish backoff, or a key the listing itself
	// never carried at all because it never hit the record). It empties all at once, not key by
	// key: target is one position in one stream, not a per-issue property, so nothing
	// distinguishes one held key's own catching-up from another's. release applies every key's
	// own summary — exactly as Reconcile's own immediate path would — before it clears them, so a
	// key the stream never gets around to explaining still lands on what the listing already knew.
	pending map[string]heldSummary
	// stamps counts the writes into pending, so each held summary carries the write that put it
	// there (heldSummary.stamp), and a release clears only the holds it applied (clearReleased).
	stamps uint64
	// target is the notification stream's own last sequence Reconcile captured when it found the
	// first record behind: every key in pending releases once the Dispatch consumer's position
	// has Reached it. Zero means nothing is held.
	target int64
	// holds counts Reconcile's deferrals into pending (Holds), so a poller of the hold tells one
	// hold from the next, which can wait on the same target and be released by the same position.
	holds uint64
	// catchingUp names each root the daemon's boot catch-up has yet to finish
	// (HoldForCatchUp): its record may still miss what its Dispatch event log holds, so no pass
	// decides whether it waits on a person (workflow's ReconcileAwaiting), nor resumes it once
	// answered — it keeps the place its answer gives it in the waiting line, within its priority,
	// among the issues whose work came back, oldest first (record.Waiting) — until the catch-up
	// has applied its window and releases it (ReleaseCatchUp). Unlike pending, a release's own
	// pass does not lift it.
	catchingUp map[string]struct{}
	// catchUpReleases counts the catch-up holds released (ReleaseCatchUp), and catchUpPassed is
	// the count the last committed pass that ran saw (promoteHolds): the difference is the pass
	// the pass rule (Apply) owes the releases.
	catchUpReleases, catchUpPassed uint64

	// retroHeldReads is, for each parent held before its retro (record.HoldForChildren), what the
	// facts of the last pass that read its release, committed, and whose hook ran before a later
	// pass began showed of it (retroHeldRead, readsHeldRetro, rememberHeldRetro). It is this daemon
	// run's own memory and nothing durable: a restart reads each such parent once more, which tells
	// nothing twice, since the told-once marks are durable (workflow's tellRetroHeld). It holds an
	// entry only for a parent the last pass's facts held so: each pass first drops every other
	// (forgetRetroHeldReads), a parent whose retro started or that left the workflow among them.
	retroHeldReads map[string]retroHeldRead
	// retroHeldPass counts the passes that read retroHeldReads (forgetRetroHeldReads). It fences
	// the hooks that write it: a hook writes only while no pass has begun after its own
	// (rememberHeldRetro).
	retroHeldPass uint64

	// openCapabilities answers the deployment capabilities with no decision, by name
	// (capabilities.Deployment.Open), for the tick notice to carry; nil until the daemon registers
	// it (ReportCapabilities), when a tick names none.
	openCapabilities func() []string

	// unclassifiedLogged is the generation at which the tick last logged each issue whose held
	// row read unclassified (logUnclassified, C2 of dispatch://INFRA-3107). It is this daemon
	// run's own memory and nothing durable, written and read inside the fact lock alone, so it
	// takes none of the mutex above. A re-admission raises the generation and clears the phase,
	// which is a state of that issue the log has not reported, so it is logged again; the
	// generation is the value rather than half of the key because it never goes back for one
	// record, which bounds the map at one entry per issue for the life of the run.
	unclassifiedLogged map[string]uint64

	// workerSilence is how long a phase worker's last recorded step may be in the past before
	// the tick notice names its issue silent (config.Config.WorkerSilence,
	// `worker_silence_minutes`), the one bound the sweep, the silence handler, this list and
	// the stalled-tree measure's reading of a claim at work (workingTrees) are
	// all read against (workflow.SilentPhaseWorker). workerBackground is how long a worker's own
	// session's report of background work may stand before the tick names it all the same
	// (config.Config.WorkerBackground, `worker_background_hours`,
	// workflow.BackgroundStalledWorker). An admission nobody registered the bounds with holds
	// zero for both, which only a test builds, and both rules answer false for every run of it,
	// so such a tick names nothing silent and the stalled-tree measure reads every claim at work
	// as work.
	workerSilence    time.Duration
	workerBackground time.Duration

	// tables is where a pass under a rule the daemon does not run reads and keeps what that rule
	// records beyond the daemon's own tables (UseRuleTables, RuleTables): nil in the daemon.
	tables RuleTables

	// releasedWakes reads the recorded wakes of released pods that the stranded judgement reads a
	// released claim by (UseReleasedWakes, workflow.Stranded), for the tick's slots reading; nil
	// until the daemon registers it, when the tick judges no released claim stranded.
	releasedWakes func(ctx context.Context, tx pgx.Tx, claims []supervise.Claim) ([]workflow.ReleasedWake, error)

	// observer is handed the view of each pass once its fact commits (UsePassObserver,
	// observePass): a replay's measure of its own run. nil in the daemon, where a pass gathers no
	// view.
	observer func(PassView)

	// p0Bound is the P0 bound in force at the moment of a pass (UseP0Bound): how many issues that
	// start outside the cap (record.SlotRule.StartsOutsideCap) may hold a slot at once, nil for no
	// bound. nil, the daemon's own, reads no bound, as R10 has none unless one is configured; only a
	// replay registers one, from its window's P0 bounds.
	p0Bound func() *int

	// signalRead is the signals a pass admits below P0 by (UseSignals, gateFor), read through the
	// pass's own transaction: nil in the daemon until a person turns admission by signals on, when
	// every pass admits under the cap.
	signalRead func(ctx context.Context, tx pgx.Tx, at time.Time) (signals.Snapshot, bool, error)

	// passFacts names more facts the pass rule (Apply) runs the pass on whatever their transaction
	// wrote, beside passFact's (UsePassFacts): nil in the daemon, which names none; a replay names
	// its own facts.
	passFacts func(intake.Fact) bool
}

// ReportCapabilities registers open, whose answer every tick controller notice carries as its
// openCapabilities: the daemon's report of the deployment capabilities with no decision, so the
// controller's periodic turn names each gap in the day's report. No other wake carries it.
func (a *Admission) ReportCapabilities(open func() []string) {
	a.openCapabilities = open
}

// UseReleasedWakes registers read, the daemon's read of the wakes of its released pods that have
// not reached the woken role's first turn, through the transaction it is handed: what the
// stranded judgement reads a released claim by (workflow.Stranded), which the tick's slots
// reading counts a holder `stopped` by, as `legion state` does (internal/daemon's
// strandedReleaseWakes reads both). claims are the project's stopped claims, which say whether
// any is released at all. It is a setter rather than a parameter of New for the reason
// ReportCapabilities is, and an admission given none judges no released claim stranded.
func (a *Admission) UseReleasedWakes(read func(ctx context.Context, tx pgx.Tx, claims []supervise.Claim) ([]workflow.ReleasedWake, error)) {
	a.releasedWakes = read
}

// UseExperiments registers the project's declared experiments
// (config.Project.ExperimentsDeclared), each of which is drawn for the units this admission
// records (drawExperiments). Every project the loader reads declares the memory experiment, at
// mode off where the file writes no `memory` block, and a draw of an experiment at mode off
// records its unit at no variant, so such a project has one answer recorded for each tree it
// opens. The empty list, which draws nothing at all, is a caller that declared no experiment at
// all: a test that builds one by hand. It is a setter rather than a parameter of New because
// every test that admits anything builds an admission, and most of them declare no experiment.
func (a *Admission) UseExperiments(experiments []config.Experiment) {
	a.experiments = experiments
}

// UseStalledTreeNotice registers how long a tree of this project may be quiet before the
// admission pass tells the project's controller that it has stopped (`stalled_tree_notice_hours`,
// config.Config.StalledTreeNotice, tellStalledTrees). It is a setter rather than a parameter of
// New because every test that admits anything builds an admission, and the stall rule is no part
// of what most of them are about. A bound of zero or less is no bound and
// tells nobody, which is what such a test leaves.
func (a *Admission) UseStalledTreeNotice(bound time.Duration) {
	a.stalledTreeNotice = bound
}

// UseWorkerSilence registers the two bounds a phase worker's quiet is read against, which every
// tick controller notice derives its silent list from (silentWorkers): silence for a worker
// whose last step is old, background for a worker whose own report of background work is. It
// is a setter rather than a parameter of New for the reason UseExperiments and ReportCapabilities
// are: every test that admits anything builds an admission, and none of them configures a bound.
func (a *Admission) UseWorkerSilence(silence, background time.Duration) {
	a.workerSilence = silence
	a.workerBackground = background
}

// UseClock sets the clock this admission's passes read the time from: the slot a promotion
// grants (record.Slot.AdmittedAt), the mark of a release for a wait (releaseSlotsForWaits), the
// movement a promotion marks, the rows it queues and every judgement of the line made at a
// moment. New sets the wall clock, which is the daemon's, and a replay of a recorded window sets
// its simulated clock, the one its engine (workflow.Config.Clock) and its store
// (record.NewStoreWithClock) read, so the pass writes and compares the window's times. It is a
// setter rather than a parameter of New for the reason UseExperiments is: every test that admits
// anything builds an admission, and none of them but the package's own sets a clock.
func (a *Admission) UseClock(clock func() time.Time) {
	a.now = clock
}

// UseP0Bound registers bound, which answers the P0 bound in force at the moment it is asked (R10
// of the tool's spec): how many issues that start outside the cap may hold a slot at once, nil for
// no bound. Each pass asks it once, before it judges the line (promoteHolds), so a change of the
// bound is in force from the next pass on. An admission given none admits under no bound, which is
// the daemon's own until cybermonklabs/legion#5488 gives it a bound of its own, set in legion.yaml
// or while it runs and read from its store at each pass; that read replaces this one at the forward
// merge that brings it, and a replay then applies each change of its window's bound as the change's
// own fact. It is a setter rather than a parameter of New for the reason UseExperiments is.
func (a *Admission) UseP0Bound(bound func() *int) {
	a.p0Bound = bound
}

// UsePassFacts registers runs, which names more facts the pass rule (Apply) runs the pass on
// whatever their transaction wrote, beside passFact's. A replay names its own facts with it: each
// stands for a change its daemon made outside any fact (a change of the P0 bound, a claim the
// supervisor settled) or for a pass of its own, as passFact's facts do, and writes no record that
// would run the pass by itself. The daemon registers none. It is a setter rather than a parameter
// of New for the reason UseExperiments is.
func (a *Admission) UsePassFacts(runs func(intake.Fact) bool) {
	a.passFacts = runs
}

// p0BoundNow is the P0 bound this pass admits under (UseP0Bound), nil for none.
func (a *Admission) p0BoundNow() *int {
	if a.p0Bound == nil {
		return nil
	}
	return a.p0Bound()
}

// The lines of R10's starts, in the words cybermonklabs/legion#5488 writes them: each start
// outside the cap (P0StartLine), and each issue that would start outside the cap held at the P0
// bound (P0BoundRefusalLine), each with the P0 holders, the other holders, the cap and the bound
// (boundWords). A replay's log carries them, which a fixture window checks.
const (
	P0StartLine        = "admission starts a P0 issue outside the cap"
	P0BoundRefusalLine = "admission holds a P0 issue at the P0 bound"
)

// boundWords is a P0 bound as a log line writes it: the number, or "none".
func boundWords(bound *int) any {
	if bound == nil {
		return "none"
	}
	return *bound
}

var _ intake.Handler = (*Admission)(nil)

// New creates an admission handler for one Dispatch project. engine is the workflow's own, shared
// with intake's other handler: reenterStrandedChildren calls its exported ReenterChild rather than
// keeping a second copy of re-entry and gate-open behavior. The pass judges by the engine's slot
// rule (workflow.Engine.SlotRule), the one its transitions and the slot mark of each start read,
// so the two never judge one record by two rules: the facts every pass reads carry it
// (record.ReadLineFactsUnder, lineFacts), so the line, the waits, the held words, the release and
// the grant all answer under it.
func New(store record.Store, engine *workflow.Engine, cap int, project string, log *slog.Logger) *Admission {
	if log == nil {
		log = slog.Default()
	}
	return &Admission{store: store, engine: engine, cap: cap, project: project, log: log, now: time.Now,
		pending: make(map[string]heldSummary), unclassifiedLogged: make(map[string]uint64), retroHeldReads: make(map[string]retroHeldRead)}
}

// Apply records root and orphan todo observations of issues handed to Legion and every newer
// observation of a recorded issue, queues the todo of an unrecorded root in triage whose legion
// label a person set (queuePersonHandover) and wakes the controller for one whose label a session
// set (a controller notice, not a record), and promotes the waiting line while capacity remains,
// releasing the slot of every issue that no longer needs one on the way (promoteHolds) and waking
// the controller when a slot it released is still free after that (wakeForFreeSlot). The workflow
// handler runs first: it records every live-tree child, leaving admission to record only a
// still-unrecorded root or orphan. A release's adoption of children it cannot adopt
// (adoptChildren) is the one fact admission refuses. A Dispatch issue fact's Reread is written
// first (applyReread), before anything of the fact itself is recorded or promoted. The entry
// sweep (intake.EntrySweep) is the one fact that promotes nothing: it converges the phase
// entries and does no more (convergeEntries).
//
// The pass rule, which every other comment of this package refers to. A fact runs the pass where
// its transaction wrote a record admission reads (record.RecordsWritten), and also where a
// catch-up hold was released since the last pass that ran (ReleaseCatchUp): the next fact runs
// the pass that release owes, whatever it wrote, and a pass a pause skipped pays nothing of it.
// The Dispatch consumer's position runs it whatever it wrote, whether or not it releases a hold
// (release), and so do the controller tick and the facts passFact names. Any other fact that
// wrote nothing runs none (promoteWritten).
func (a *Admission) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	released, refusal, err := a.applyFact(ctx, tx, fact)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{Refusal: refusal}, a.wakeForFreeSlot(ctx, tx, released)
}

// OpenFact and SettleFact are the engine's (intake.Settler): admission writes issues through it —
// a re-admission's new generation, a promotion's starts — so the runs those writes end are settled
// whichever of the two a fact applies through, once. Every admission holds an engine (New).
func (a *Admission) OpenFact(ctx context.Context) context.Context {
	return a.engine.OpenFact(ctx)
}

func (a *Admission) SettleFact(ctx context.Context, tx pgx.Tx) error {
	return a.engine.SettleFact(ctx, tx)
}

// applyFact is Apply's work, and reports the issues whose slots it released and the refusal of a
// release's adoption.
func (a *Admission) applyFact(ctx context.Context, tx pgx.Tx, fact intake.Fact) ([]string, *intake.Refusal, error) {
	if adoption, ok := fact.(intake.ChildAdoption); ok {
		return a.adoptChildren(ctx, tx, adoption)
	}
	if position, ok := fact.(intake.DispatchConsumerPosition); ok {
		released, err := a.release(ctx, tx, position)
		return released, nil, err
	}
	if _, ok := fact.(intake.EntrySweep); ok {
		// The sweep converges the entries and does no more: it promotes nothing, since the pass
		// runs by the pass rule (Apply) without it, and it releases no slot, so it reports none.
		return nil, nil, a.convergeEntries(ctx, tx)
	}
	if _, ok := fact.(intake.ControllerTick); ok {
		_, returned, err := a.promoteHolds(ctx, tx, true, signals.PassTick)
		if err != nil {
			return nil, nil, err
		}
		// What the project waits on, what nothing starts, whose phase worker nothing will prompt
		// and what its slots in use are spent on are read here, after the promotion this tick
		// made, from one read of the project (tickRead): the first two and the slots reading are
		// assemblies of one judgement of the line, and they are built whether or not a controller
		// is registered to be told. The wake carries them as it carries the open capabilities,
		// and judges nothing a second time.
		read, err := a.tickRead(ctx, tx)
		if err != nil {
			return nil, nil, err
		}
		// The reading is the daemon's own record of each tick's slots as well, one line once the
		// fact commits whether or not a controller is told, so a window of the log read later
		// says what the slots were spent on at every tick.
		reading := read.slots
		store.OnCommit(ctx, func() { a.log.Info("admission: slots reading", "slots", reading) })
		// The tick wakes the controller whatever the slots: with every slot taken, it is the only
		// wake a tree waiting on a refused root claim gets, since only the controller's recheck of
		// that claim moves it, and it is the turn that posts the day's report.
		_, err = a.wakeController(ctx, tx, record.ControllerNotice{Kind: record.TickNotice, Held: read.held,
			Blocked: read.blocked, Silent: read.silent, Slots: &reading}, a.project, a.now())
		return returned, nil, err
	}
	if handover, ok := fact.(intake.Handover); ok {
		if err := a.consumeHandover(ctx, tx, handover); err != nil {
			return nil, nil, err
		}
		_, returned, err := a.promoteWritten(ctx, tx)
		return returned, nil, err
	}

	observation, ok := fact.(intake.DispatchIssue)
	if !ok {
		// A signal's deadline (intake.SignalDeadline) is the timer fact whose pass admits below P0
		// once a signal crosses back under its limit: it runs its pass whatever it wrote, as a
		// deadline's pass (signals.PassDeadline), which the pass rule's promote would run as an
		// event's.
		if _, deadline := fact.(intake.SignalDeadline); deadline {
			_, returned, err := a.promoteHolds(ctx, tx, true, signals.PassDeadline)
			return returned, nil, err
		}
		promote := a.promoteWritten
		if passFact(fact) || a.passFacts != nil && a.passFacts(fact) {
			promote = a.promote
		}
		_, returned, err := promote(ctx, tx)
		return returned, nil, err
	}
	if err := a.applyReread(ctx, tx, observation.Reread); err != nil {
		return nil, nil, err
	}

	stored, err := a.store.Issue(ctx, tx, observation.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("read admission issue %s: %w", observation.Key, err)
	}
	if observation, err = workflow.SettleRetainedRepository(observation, stored); err != nil {
		return nil, nil, err
	}
	if stored != nil {
		if err := a.applyObservation(ctx, tx, *stored, observation); err != nil {
			return nil, nil, err
		}
		_, released, err := a.promoteWritten(ctx, tx)
		return released, nil, err
	}
	handed := observation.HandedOver
	// A root in triage handed to Legion is queued by the daemon when a person set the label
	// (observation.LabelSetter, read from the issue's event log when the fact was prepared): a
	// person's label is the handover, so the daemon writes its todo through the outbox
	// (queuePersonHandover) and no agent's write admits it. A label a session set, or one whose
	// setter the log did not show (the zero LabelSetter: a fact nothing prepared, or a log with no
	// labelling event) is not a person's, so each observation of the root while it is unrecorded
	// wakes the controller as before: its creation with the label, or the edit that adds it, since
	// the dashboard creates an issue without labels. The controller triages from Dispatch and the
	// daemon's state, never from the wake. A root without the label is not Legion's and wakes
	// nobody. This runs before any hold check: a hold defers only whether an unrecorded todo is
	// admitted, never whether an unrecorded triage root is queued or wakes the controller. The
	// record stays empty, as for any root not yet todo; the daemon's own todo write records it
	// through the path below once Dispatch echoes it. Reconcile's boot listing reads every status
	// now, but a triage root is never a todo candidate it admits.
	if observation.LabelledTriageRoot() {
		if observation.LabelSetByPerson() {
			if err := a.queuePersonHandover(ctx, tx, observation.Key, "triage", "event"); err != nil {
				return nil, nil, err
			}
		} else if err := a.enqueue(ctx, tx, observation.Key, record.ControllerNotice{Kind: record.TriageNotice}, a.now()); err != nil {
			return nil, nil, err
		}
	}
	// An unrecorded todo issue without the label is someone else's work in a project Legion may
	// share with humans and other agents: Legion records nothing of it until it is handed over.
	// Neither exit records anything, but each still refreshes a held key's own pending summary:
	// an event moving the key out of todo, or taking its label off, is real information about
	// what Dispatch shows now, and leaving the held summary as Reconcile's stale boot listing
	// found it would have release admit on that stale information instead.
	//
	// Every exit below is still a look at the waiting line where the fact wrote a record, though it
	// records nothing of the issue: the blockers.Mirror handler, which runs first, has already
	// written this issue's row in this transaction wherever the event is newer than the row, and a
	// root or a child may be waiting on exactly this issue. Its close is an observation of an
	// unrecorded issue out of todo, so without this look a waiting root whose last blocker is an
	// issue Legion keeps no record of would wait for the controller tick to be admitted.
	switch {
	case observation.Status != "todo":
		a.refreshHeldSummary(observation)
		_, released, err := a.promoteWritten(ctx, tx)
		return released, nil, err
	case !handed:
		a.log.Debug("admission: not handed to Legion", "issue", observation.Key, "label", dispatch.LegionLabel)
		a.refreshHeldSummary(observation)
		// Promotion comes before the slot is judged: a root this look admits takes a slot that
		// was free before it, and the wake must name a slot still free once this fact commits,
		// never one the same transaction has just filled.
		_, released, err := a.promoteWritten(ctx, tx)
		if err != nil {
			return nil, nil, err
		}
		// An issue in todo that nobody handed to Legion wakes the controller while a slot stands
		// free; the controller takes nothing for it, since only a person hands Legion work
		// (skill://legion-controller, "Free slots"). The notice waits todoWakeDelay, so the
		// events of one edit burst find it pending and fold into it.
		free, err := a.slotFree(ctx, tx)
		if err != nil || !free {
			return released, nil, err
		}
		_, err = a.wakeController(ctx, tx, record.ControllerNotice{Kind: record.TodoNotice}, observation.Key, a.now().Add(todoWakeDelay))
		return released, nil, err
	}
	a.mu.Lock()
	hold, held := a.pending[observation.Key]
	a.mu.Unlock()
	if held && observation.Seq <= hold.summary.Seq {
		// Reconcile already holds this key on the listing's own summary because it found it
		// behind the stream: an unrecorded key has no record to fence a live event against, so
		// a replay at or behind that summary's own sequence — a nak's redelivery, the outbox's
		// own publish backoff — decides nothing about it. release applies that summary once the
		// consumer catches up. A newer event, past the sequence the summary was taken at, is
		// not a stale replay: it is recorded normally below, even while the key is still
		// technically held — a release that has already committed but not yet run its
		// commit hook (which clears pending) still shows this key as held, and dropping this
		// event here would lose it once that hook clears pending out from under it, the
		// event already acknowledged as processed. promote's own pending membership check —
		// not this one — is what still holds the freshly recorded candidate back from a slot
		// until pending is actually clear; a later pass once the hold clears promotes it. The
		// replay records nothing of itself, but where the mirror wrote its row it runs the pass,
		// as the pass rule (Apply) has it.
		_, released, err := a.promoteWritten(ctx, tx)
		return released, nil, err
	}
	if err := a.putNewRoot(ctx, tx, observation, true); err != nil {
		return nil, nil, err
	}
	_, released, err := a.promoteWritten(ctx, tx)
	return released, nil, err
}

// promoteWritten is the pass a fact runs under the pass rule (Apply): promote, where the rule
// gives one, and no pass where it does not. A fact that wrote nothing leaves every input of the
// pass as the last pass left it, so a pass would decide nothing new and would cost every read it
// makes under the lock every fact takes (intake.SerializeFacts): a push to a branch no issue
// records, or a check of a head no pull request records, are most of what GitHub sends. What
// changes without a fact — a claim's state, the clock, a catch-up hold's release — is the tick's,
// the hold release's and the owed pass's.
func (a *Admission) promoteWritten(ctx context.Context, tx pgx.Tx) (int, []string, error) {
	if !record.RecordsWritten(ctx) && !a.catchUpPassOwed() {
		return 0, nil, nil
	}
	return a.promote(ctx, tx)
}

// catchUpPassOwed says whether a catch-up hold was released since the last pass that ran past the
// pause committed.
func (a *Admission) catchUpPassOwed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.catchUpReleases > a.catchUpPassed
}

// passCatchUpReleases records, once tx commits, that a pass that ran past the pause saw every
// catch-up release made before it began, so those releases owe no further pass (catchUpPassOwed).
// A pass rolled back records nothing, and the release still owes one; a pass a pause stops never
// calls it (promoteHolds).
func (a *Admission) passCatchUpReleases(ctx context.Context) {
	a.mu.Lock()
	seen := a.catchUpReleases
	a.mu.Unlock()
	store.OnCommit(ctx, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.catchUpPassed = max(a.catchUpPassed, seen)
	})
}

// passFact says whether fact is one the pass rule (Apply) runs the pass on whatever its
// transaction wrote: each stands for a change made outside any fact, or is itself a pass. The
// resume of a pause (intake.ProjectResumed) is the pass the pause held back; the end of a root's
// boot catch-up (intake.CatchUpApplied) is the pass that decides it; the boot (intake.DaemonBooted)
// is a pass of the boot; a role's agent taking its task (intake.TaskConfirmed) stands for the
// confirmation the supervisor wrote outside the fact, which decides whether a root whose
// architect owed that task now waits on a person (workflow's ReconcileAwaiting).
func passFact(fact intake.Fact) bool {
	switch fact.(type) {
	case intake.ProjectResumed, intake.CatchUpApplied, intake.DaemonBooted, intake.TaskConfirmed:
		return true
	}
	return false
}

// applyReread writes each Dispatch chain the preparation of an unrecorded or re-parented issue's
// event read again (intake.DispatchIssue.Reread) to its root's record while that record is still
// a todo root, as the engine's Ancestry takes it. The read was taken outside the fact lock, so a
// root adopted or moved on meanwhile is left as it now stands.
func (a *Admission) applyReread(ctx context.Context, tx pgx.Tx, reread map[string][]string) error {
	for _, key := range slices.Sorted(maps.Keys(reread)) {
		stored, err := a.store.Issue(ctx, tx, key)
		if err != nil {
			return fmt.Errorf("read admission issue %s: %w", key, err)
		}
		if stored == nil || stored.Status != "todo" || !claim.IsTreeRoot(stored.Key, stored.Tree) {
			continue
		}
		ancestry, err := a.engine.Ancestry(ctx, tx, *stored, reread[key])
		if err != nil {
			return fmt.Errorf("read whether %s ran as a tree of its own: %w", key, err)
		}
		if slices.Equal(stored.Ancestry, ancestry) {
			continue
		}
		stored.Ancestry = ancestry
		if err := a.engine.WriteIssue(ctx, tx, *stored); err != nil {
			return fmt.Errorf("record the Dispatch chain of %s: %w", key, err)
		}
	}
	return nil
}

// wakeForFreeSlot wakes the controller when this transaction released a slot that promotion left
// free: an issue reached its done or left the workflow, or gave its slot back to wait on another
// issue (releaseSlotsForWaits), and nothing waiting in the line took its place. The controller
// names the free slot in its summary and takes no issue for it (skill://legion-controller,
// "Free slots"). It is one `slot-free` notice on the first issue whose slot was released, under
// the same rules as every slot wake: a slot free by slotFree, a registered controller, none already unsent (wakeController).
func (a *Admission) wakeForFreeSlot(ctx context.Context, tx pgx.Tx, released []string) error {
	if len(released) == 0 {
		return nil
	}
	free, err := a.slotFree(ctx, tx)
	if err != nil || !free {
		return err
	}
	_, err = a.wakeController(ctx, tx, record.ControllerNotice{Kind: record.SlotFreeNotice}, released[0], a.now())
	return err
}

// todoWakeDelay is how long a `todo` controller notice waits in the outbox before it is published.
// Every Dispatch event of an unlabelled todo issue within it finds the notice still pending, so a
// burst of edits is one wake rather than one a poll.
const todoWakeDelay = 30 * time.Second

// slotFree says whether an admission slot stands free: fewer issues holding one than the cap,
// counting a slot a waiting issue will take as taken. The cap counts the issues the daemon has
// started, so the line it counts against is a line of issues, children included. A candidate
// promotion passes over because it waits on an unfinished Dispatch issue (blockers.LineWaitsOn)
// will take none, and neither will one that already holds a row or whose phase keeps none
// (slotFor), so none of them hides free capacity from the project: the controller hears of the
// slot and may hand Legion work that can actually run. A candidate already holding a row is no
// taker: the line carries holders (the kinds record.LineEntry.Holds names), and counting such a
// candidate would say the cap was full while rows stood free, so the wake would not fire. The
// controller is woken about it and takes nothing for it; only a person hands Legion work
// (skill://legion-controller, "Free slots").
//
// The line is the one the promotion itself would read (blockers.JudgeLine), held by the same
// blocker hold and the same children-first wait: a line built here from its own reads would
// answer differently from the pass that acts, and a slot this said was free would then go to
// nobody. It is judged under the pass's own rule (lineFacts) and counted as the promotion counts
// against the cap (capLets): a candidate a counted waiter offers its slot to under a cover
// (covers) starts on that slot, which the waiter gives up for it, and takes no free slot.
//
// The slot it speaks of is one of the cap's, so under a rule that starts a P0 issue outside the
// cap it counts the holders the cap counts (record.SlotRule.HoldersOf): a P0 holder takes none of
// the cap's slots, and a P0 candidate is no taker of one either, since it starts outside the cap
// (record.SlotRule.StartsOutsideCap) or, at the P0 bound, waits for a P0 holder to finish. The
// promotion, the stall measure's free-slot term (lineCanMove) and this read count the same holders.
func (a *Admission) slotFree(ctx context.Context, tx pgx.Tx) (bool, error) {
	if a.cap <= 0 {
		return false, nil
	}
	issues, err := a.store.LiveIssues(ctx, tx, a.project)
	if err != nil {
		return false, fmt.Errorf("list admission issues: %w", err)
	}
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return false, fmt.Errorf("list admission slots: %w", err)
	}
	facts, err := a.lineFacts(ctx, tx, issues)
	if err != nil {
		return false, err
	}
	own := ownSlots(issues, slots)
	rows, facts, line, _, err := blockers.JudgeLine(ctx, tx, a.store, a.project, issues, own, facts, nil, a.now())
	if err != nil {
		return false, err
	}
	lending := coversOf(issues, own, rows, facts)
	rule := facts.Rule.OrMain()
	held := rule.HoldersOf(issues, own)
	taking := 0
	for _, candidate := range line {
		if len(blockers.LineWaitsOn(rows, candidate)) > 0 || !slotFor(candidate, facts) || rule.StartsOutsideCap(candidate.Issue) {
			continue
		}
		if _, covering := lending.take(candidate.Issue.Key); !covering {
			taking++
		}
	}
	return held.Capped+taking < a.cap, nil
}

// wakeController queues notice on issue, due at dueAt, when a controller is registered and no
// notice of the same kind still waits in the outbox, so a burst of events or ticks queues one
// wake. The wake needs no record: the controller reads Dispatch and the daemon's state
// (skill://legion-controller). A tick alone carries the daemon's open capabilities
// (ReportCapabilities), which this adds, and the three lists the tick's own fact built from one
// read of the project and put on the notice before it reached here (applyFact, tickRead): every
// issue Legion waits on another issue for, one it has still to start or one it started that gave
// its slot back, every issue nothing starts at all, and every issue whose phase worker nothing
// will prompt (silentWorkers). So a tick that wakes nobody has judged them all the same, and
// nothing here judges anything a second time. Every other kind is queued as the caller built it,
// so a kind that says something of its own — the role and the reason of TellController's —
// carries it here.
//
// Which notices count as the same one is the kind's own rule (record.NoticePerClaim): a kind
// that speaks for the project is one wake per project, which is what the slot wakes and the tick
// want, and a kind that speaks for one claim is one wake per claim — its issue and its role. The
// second is the provider-errors notice: in an outage every claim's turns fail, each claim owes a
// notice of its own, and the machine tells at exactly the sixth turn in a row, so a notice
// dropped because another claim's still waited would never be sent. Two claims of one issue, an
// architect and its worker, are two such claims, which is why the role is part of the key. The
// tree-stalled notice is keyed the same way with no role at all, which keys it by its issue
// alone: a role that is empty is a term the pending read leaves out.
//
// It answers whether it queued a row. Two of its own rules queue nothing and are no error: no
// controller is registered, so nothing would read the wake, and an older notice of the same
// kind, issue and role still waits unsent. A caller that records something of its own about
// the wake must know which happened, and one does: the stalled-tree rule marks a root only for
// a notice that went into the outbox, in this same transaction, so a tree that stopped while
// no controller ran is told at a later pass instead of being marked told and lost
// (tellStalledTrees). Every other caller has nothing to record and drops the answer.
func (a *Admission) wakeController(ctx context.Context, tx pgx.Tx, notice record.ControllerNotice, issue string, dueAt time.Time) (bool, error) {
	registered, err := a.store.ControllerRegistered(ctx, tx, a.project)
	if err != nil || !registered {
		return false, err
	}
	of, from := "", claim.Role("")
	if record.NoticePerClaim(notice.Kind) {
		of, from = issue, notice.Role
	}
	pending, err := a.store.ControllerNoticePending(ctx, tx, a.project, notice.Kind, of, from)
	if err != nil || pending {
		return false, err
	}
	if notice.Kind == record.TickNotice && a.openCapabilities != nil {
		notice.OpenCapabilities = a.openCapabilities()
	}
	if err := a.enqueue(ctx, tx, issue, notice, dueAt); err != nil {
		return false, err
	}
	return true, nil
}

// TellController queues notice on issue in the caller's transaction, for a caller outside any
// fact: the supervisor, whose machine decides inside its own lock (supervise's
// Deps.ProviderErrorRunReached, wired in internal/daemon, which opens the transaction). It is
// wakeController and nothing else: a controller that is not registered is told nothing, and a
// notice the same wake already waits to send stops this one, so a run of the same cause on the
// same claim is one wake and another claim's run is a wake of its own (record.NoticePerClaim).
func (a *Admission) TellController(ctx context.Context, tx pgx.Tx, notice record.ControllerNotice, issue string) error {
	_, err := a.wakeController(ctx, tx, notice, issue, a.now())
	return err
}

// queuePersonHandover queues the todo write of a person's handover of key: a root a person handed
// to Legion, which the daemon admits without an agent's act. Today that is a person's legion label
// on an unrecorded root in triage (applyFact for the event, applySummary for the boot listing);
// the workflow queues the same write for a person's approval of a parked labelled root's spec. The
// controller used to write this todo at a triage wake, so a handover waited on an agent's memory;
// a person's decision is the handover, and the daemon writes it itself and wakes nobody. The row
// carries observedStatus, the status the person's act found the issue in, because its delivery
// (the daemon's outbox) re-reads the issue and writes only while Dispatch still shows that status:
// a person's later park, between the act and the delivery, is never undone. One row per issue at
// a time: while a marked row is pending (record.Store.PersonHandoverPending) a second act — the
// stream's redelivery, another edit of the same root, the boot listing of a root an event already
// queued — queues nothing, since the pending row carries the same write, and the triage wake that
// act would otherwise write is not written either; once the row is delivered the issue is todo, so
// a later look at the same label finds nothing to queue. The workflow's other status rows of the
// issue suppress nothing. The queue is logged once it commits, under one message for every source
// of a person's handover — `event` and `listing` here, `approval` from the workflow — so the boot
// proof reads which path queued a root from the line's source field.
func (a *Admission) queuePersonHandover(ctx context.Context, tx pgx.Tx, key, observedStatus, source string) error {
	pending, err := a.store.PersonHandoverPending(ctx, tx, key)
	if err != nil || pending {
		return err
	}
	if err := a.enqueue(ctx, tx, key, record.PersonHandoverStatusWrite("todo", observedStatus), a.now()); err != nil {
		return err
	}
	store.OnCommit(ctx, func() { a.log.Info("admission: queued a person's handover", "issue", key, "source", source) })
	return nil
}

// tickReading is what the tick controller notice carries beside the open capabilities: the three
// lists tickRead reads, and the project's slots reading (slotsReading).
type tickReading struct {
	held    []record.HeldIssue
	blocked []record.BlockedIssue
	silent  []record.SilentIssue
	slots   record.SlotsReading
}

// tickRead is what the tick controller notice carries beside the open capabilities: every issue
// Legion waits on another issue for, one it has still to start or one it started that gave its
// slot back (`blocked`, blockers.Blocked), every issue the daemon's own record holds in the
// workflow that stands in neither the active set nor the waiting line, with the one word for why
// (`held`, C3 of dispatch://INFRA-3107), and every issue whose phase worker nothing will prompt
// (`silent`, silentWorkers).
//
// The two are two assemblies of one judgement of the line, so they are built together here:
// `blocked` is blockers.Blocked over the mirror rows and the line, `held` is record.HeldList
// over the same line and the rows the pass left behind, and both read the judgement promotion
// itself reads (blockers.JudgeLine). Built apart they cost the tick two judgements of the whole
// project — the same four reads, and the mirror read twice in one transaction — and a reader of
// either list could not say the two were made over one record.
//
// Each is built by the function the state builds the same list with, over this transaction's
// own reads, so what the controller is told and what `legion state` shows for one issue agree:
// one word in both, and one set of blocked rows in both. The rows are built whether or not a
// controller is registered to be told, because the unclassified line is written from them
// (logUnclassified), and that line is the daemon's own record of what the words miss.
//
// It makes no read of the line of its own: the three reads are the pass's (reread), and the
// candidates are the live issues the daemon holds in the workflow, which record.HeldList takes
// out of them itself (record.InWorkflow). The silent list is read from those same issues and
// line facts, before the judgement adds to them, so its tree gates are the gates the blocked
// list is judged from and nothing is read twice in this one transaction.
//
// The slots reading beside the three lists is assembled from that same judgement of the line and
// those same two lists (slotsReading), so the reading the controller is told and the lists it is
// told beside it are one reading of one record.
func (a *Admission) tickRead(ctx context.Context, tx pgx.Tx) (tickReading, error) {
	issues, slots, facts, err := a.reread(ctx, tx)
	if err != nil {
		return tickReading{}, err
	}
	silent, err := a.silentWorkers(ctx, tx, issues, facts)
	if err != nil {
		return tickReading{}, err
	}
	own := ownSlots(issues, slots)
	rows, facts, line, dropped, err := blockers.JudgeLine(ctx, tx, a.store, a.project, issues, own, facts, nil, a.now())
	if err != nil {
		return tickReading{}, err
	}
	held := record.HeldList(issues, own, line, dropped, facts)
	a.logUnclassified(held, issues)
	blocked := blockers.Blocked(rows, issues, line, facts)
	reading, err := a.slotsReading(ctx, tx, blockers.SlotsInputs{Cap: a.cap, Issues: issues, Slots: own, Rows: rows, Facts: facts,
		Line: line, Blocked: blocked, Held: held})
	if err != nil {
		return tickReading{}, err
	}
	return tickReading{held: held, blocked: blocked, silent: silent, slots: reading}, nil
}

// slotsReading is the project's slots reading over in, the tick's judgement of the line and its
// two lists, with the claims of the project's slot holders and the claims among them nothing will
// wake filled in: one narrow read of the claims (record.Store.HolderClaims, the statement the
// state's source reads them through, over the slots this transaction holds, the ones reread read)
// and, where one of them stands stopped, the daemon's own judgement of which nothing will wake
// (workflow.Stranded), so a holder reads `stopped` for a stranded claim here as it does in
// `legion state`. The released wakes that judgement reads a released claim by are the daemon's
// (UseReleasedWakes); an admission given none judges no released claim stranded.
func (a *Admission) slotsReading(ctx context.Context, tx pgx.Tx, in blockers.SlotsInputs) (record.SlotsReading, error) {
	project, err := claim.ProjectToken(a.project)
	if err != nil {
		return record.SlotsReading{}, err
	}
	if in.Claims, err = a.store.HolderClaims(ctx, tx, project); err != nil {
		return record.SlotsReading{}, err
	}
	if in.Stranded, err = a.strandedClaims(ctx, tx, project, in.Claims); err != nil {
		return record.SlotsReading{}, err
	}
	reading, _ := blockers.SlotsReading(in)
	return reading, nil
}

// strandedClaims is, of claims, every one nothing will wake (workflow.Stranded), judged over the
// fields of a claim that judgement reads, which the narrow read holds. The claims are the slot
// holders' alone (record.Store.HolderClaims), the only ones the slots reading reads, and the
// judgement reads each claim on its own, so a project none of whose holders has a claim standing
// suspended, failed or retired has none, and costs no read.
func (a *Admission) strandedClaims(ctx context.Context, tx pgx.Tx, project string, claims []record.HolderClaim) ([]supervise.Claim, error) {
	stopped := make([]supervise.Claim, 0, len(claims))
	for _, held := range claims {
		if held.State == supervise.StateSuspended || held.State == supervise.StateFailed || held.State == supervise.StateRetired {
			stopped = append(stopped, supervise.Claim{Token: held.Token, Project: project, Tree: held.Tree, Issue: held.Issue,
				Role: held.Role, State: held.State, SuspendedBy: held.SuspendedBy})
		}
	}
	if len(stopped) == 0 {
		return nil, nil
	}
	var wakes []workflow.ReleasedWake
	if a.releasedWakes != nil {
		var err error
		if wakes, err = a.releasedWakes(ctx, tx, stopped); err != nil {
			return nil, err
		}
	}
	return workflow.Stranded(ctx, tx, a.store, a.project, a.engine.GatedSteps(), stopped, wakes, a.now())
}

// logUnclassified writes one line for each row the ladder could put no word to (C2), naming the
// issue, the phase and the status a reader needs to see what the words miss, and writes it once
// for that issue and this daemon run: a state nobody has closed would otherwise print a line at
// every tick, for as long as the record stands. The line is the daemon's own record of a state
// the state body already names in `admission.held`, so it earns no table and no migration —
// unlike the children-first notice, which is durable because it reaches a person and must not be
// told twice across restarts.
//
// The run is the daemon's own: the mark lives in memory and a restart logs each such issue once
// more, which is what a reader of a fresh run's log wants. It is keyed by the issue's own
// generation beside its key, so that a re-admission, which raises the generation and clears the
// phase, is a new state of that issue and earns a line of its own.
//
// Every write of unclassifiedLogged happens here, inside the one fact lock each intake fact
// holds (intake.ApplyFact), and nothing outside a fact reads it: it needs none of the mutex
// above, which guards pending because the daemon's own poller reads that one outside any
// transaction. The mark is written beside the line rather than after the commit because the line
// is already written: a fact rolled back has still logged what it logged, and a second line for
// the same issue would say nothing more.
func (a *Admission) logUnclassified(rows []record.HeldIssue, issues []record.Issue) {
	// The map is built on the first row that needs it: a project where every held issue reads a
	// word of its own — which is every healthy project — builds none at all, and the tick runs
	// this on each of its passes.
	var byKey map[string]record.Issue
	for _, row := range rows {
		if row.Reason != record.HeldUnclassified {
			continue
		}
		if byKey == nil {
			byKey = make(map[string]record.Issue, len(issues))
			for _, issue := range issues {
				byKey[issue.Key] = issue
			}
		}
		issue := byKey[row.Issue]
		if logged, seen := a.unclassifiedLogged[row.Issue]; seen && logged == issue.Generation {
			continue
		}
		a.unclassifiedLogged[row.Issue] = issue.Generation
		a.log.Info("admission: the record holds an issue no held word is true of", "issue", row.Issue,
			"phase", issue.Phase, "status", issue.Status, "reason", record.HeldUnclassified)
	}
}

// phaseWorker is one candidate of a repair over phase workers: the issue, the root of its tree
// as the live records hold it, the role that works the issue's phase and that role's claim
// token. The entry repair holds a root's own architect in the same shape (rootArchitectOf).
type phaseWorker struct {
	issue record.Issue
	root  record.Issue
	role  claim.Role
	token claim.Token
}

// phaseWorkerOf is issue as a candidate of a repair over phase workers, and whether it is one:
// an issue of this project whose phase has a worker role (workflow.OpenPhaseRole) and whose work
// the record shows under way in a tree that does not linger. project is the project's claim
// token (claim.ProjectToken), and byKey is the project's live records by key, which the tree
// roots are read from and which the slot repair walks its slots and its issues in accepting,
// retro or merging through.
//
// Its two callers are the repairs over phase workers: the tick's silent list (silentWorkers,
// through phaseWorkers) and the selection of the roles behind the entry their phases row names
// (owedEntryRoles, which resumeSlottedWorkers sends). They select one candidate the same way so
// that the two read one claim alike, and each judges what it selected in a loop of its own,
// which is where the two differ. A silent claim behind its entry is the one both can name:
// admission sends it that entry's work (workflow.SendOwedEntry) and so does the silence handler
// (workflow's repairSilence), and whichever writes first leaves a queued start carrying that
// entry, which the other's rule reads and passes over (workflow.OwedEntry, and the queued start
// unpromptedPhaseWorker refuses).
//
// A candidate with no work where it stands is passed over, because both repairs are about a
// worker that is owed a turn, and that candidate has none to start. The gate is unreachable on
// this release: the role skip above it passes over every phase phase.RoleFor names no worker
// role for — the architect's admitted and accepting, and the empty role of held,
// awaiting_person, awaiting_children and done — and merging, the one holder record.AtWork used
// to answer false for, is a worker's phase now, since its merger makes the merge itself. It is
// kept so that a phase added later with a worker role of its own, in a state record.AtWork
// answers false for, is sent nothing by either repair, and so record.AtWork stays the one
// statement of what work is rather than a second list of phases here.
//
// An issue whose key yields no claim token has no run to read, so it is passed over rather
// than failing the whole fact, as the wait reconcile passes over such a root (workflow's
// ReconcileAwaiting).
func (a *Admission) phaseWorkerOf(project string, issue record.Issue, byKey map[string]record.Issue,
	facts record.LineFacts,
) (phaseWorker, bool) {
	role := workflow.OpenPhaseRole(issue.Phase)
	if role == "" {
		return phaseWorker{}, false
	}
	if !record.AtWork(issue, facts.Gates[issue.Tree]) {
		return phaseWorker{}, false
	}
	root, rooted := byKey[issue.Tree]
	if !rooted || root.Lingers() {
		return phaseWorker{}, false
	}
	token, err := claim.NewToken(project, issue.Key, role)
	if err != nil {
		return phaseWorker{}, false
	}
	return phaseWorker{issue: issue, root: root, role: role, token: token}, true
}

// phaseWorkers is, of candidates, every one phaseWorkerOf selects, with that role's run: the
// candidates of the tick's silent list (silentWorkers). The runs are one batched
// record.RoleRuns call and never a call per candidate, because a tick runs over the whole
// project.
func (a *Admission) phaseWorkers(ctx context.Context, tx pgx.Tx, candidates []record.Issue,
	byKey map[string]record.Issue, facts record.LineFacts,
) ([]phaseWorker, map[claim.Token]record.RoleRun, error) {
	project, err := claim.ProjectToken(a.project)
	if err != nil {
		return nil, nil, err
	}
	var workers []phaseWorker
	var keys []record.RoleRunKey
	for _, issue := range candidates {
		worker, selected := a.phaseWorkerOf(project, issue, byKey, facts)
		if !selected {
			continue
		}
		workers = append(workers, worker)
		keys = append(keys, record.RoleRunKey{Token: worker.token, Issue: issue.Key, Role: worker.role, Generation: issue.Generation})
	}
	if len(keys) == 0 {
		return nil, nil, nil
	}
	runs, err := a.store.RoleRuns(ctx, tx, keys)
	if err != nil {
		return nil, nil, err
	}
	return workers, runs, nil
}

// rootArchitectOf is issue's own architect as a holder the entry repair weighs (owedEntryRoles),
// and whether it is one: issue is a tree root of this project whose tree does not linger, and its
// key yields a claim token. Which of these roots the rule reaches is read once the claim is
// behind (architectReached), so a root owed nothing costs no read of its own.
func (a *Admission) rootArchitectOf(project string, issue record.Issue) (phaseWorker, bool) {
	if !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.Lingers() {
		return phaseWorker{}, false
	}
	token, err := claim.NewToken(project, issue.Key, claim.RoleArchitect)
	if err != nil {
		return phaseWorker{}, false
	}
	return phaseWorker{issue: issue, root: issue, role: claim.RoleArchitect, token: token}, true
}

// silentWorkers is every issue of the project whose phase worker is one nothing will prompt, as
// the tick notice carries it: its process is up and between turns, it holds no task, and either
// its agent's last recorded step is older than the bound this deployment gives a phase worker
// (UseWorkerSilence, config.Config.WorkerSilence; workflow.SilentPhaseWorker) or its own
// session's report of background work has stood past the longer bound
// (config.Config.WorkerBackground; workflow.BackgroundStalledWorker). The daemon prompts the
// second kind never, and the list marks it with the moment of that report
// (record.SilentIssue.BackgroundSince). workflow.PhaseWorkerOwedTurn answers the half of both
// only the store can answer.
//
// The silence handlers read the same rules over the same record, so the list names the workers
// the `worker-silent` and `worker-background-stalled` notices to the architects name, with two
// differences. A handler also refuses a fact whose run is not the issue's current generation
// (workflow's phaseWorkerRecord), and the sweep gives each fact the claim's serving run
// (supervise.Claim.ServingRun). The tick weighs no such term: it reads each worker's claim by its
// token alone (record.Postgres.RoleRuns, whose key's generation picks only the queued starts read
// beside the claim), and the claim it reads carries no serving run (record.RoleClaim), so the list
// can name a worker whose claim still serves an earlier run of the issue, whose fact the handler
// refuses. And the sweep that applies those facts runs once a minute (internal/daemon's
// reconcileOrphans, at orphanSweepInterval), so a tick can name a worker that passed its bound
// since the last sweep up to a minute before the next one writes its architect's notice.
//
// Why the controller is told at all, when the architect that owns the issue already was or is
// within the minute: a child's architect that does not hold its role is passed over for the one
// above it, but the notice can end at a tree root's architect claim that has failed or is gone,
// where it reaches nobody. The controller is the role that holds `fresh_session` and reads the
// whole project (this root's architect ruled it on 2026-11-12, dispatch://INFRA-3163). The tick
// carries the list. The state route gains no field for it; the state's settings gain the two
// bounds, `worker_silence_minutes` and `worker_background_hours`, as two keys of a loose object,
// so no daemon API contract number is taken for either.
//
// What that costs, plainly: record.DecodeOutboxPayload refuses a field and a notice kind it
// does not know, so a daemon of an older release that met a queued row carrying this list, or a
// queued `worker-silent` or `worker-background-stalled` notice, would retry that row for ever.
// It is the cost every notice field and every notice kind of this record has, and the
// deployment's roll-forward is the answer to it.
//
// The candidates are the project's live issues, each selected as the repair of a slot selects
// its own (phaseWorkerOf) and read in one batch (phaseWorkers). The last step and the report
// come from the claim the record holds (record.RoleClaim), because admission holds no claim of
// the supervisor at all, where the sweep reads the live one.
//
// The list is in issue order so that two ticks over one record name the silent issues in one
// order, whatever order the live read answered in.
func (a *Admission) silentWorkers(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts record.LineFacts) ([]record.SilentIssue, error) {
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	workers, runs, err := a.phaseWorkers(ctx, tx, issues, byKey, facts)
	if err != nil {
		return nil, err
	}
	now := a.now()
	var silent []record.SilentIssue
	for _, held := range workers {
		run := runs[held.token]
		gate := facts.Gates[held.issue.Tree]
		// The two rules never both hold: SilentPhaseWorker refuses the claim whose report
		// names background work, and BackgroundStalledWorker is about that claim alone.
		stepped := workflow.SilentPhaseWorker(held.issue, held.root, gate, held.role, run, a.workerSilence, now)
		background := !stepped && workflow.BackgroundStalledWorker(held.issue, held.root, gate, held.role, run, a.workerBackground, now)
		if !stepped && !background {
			continue
		}
		// A worker owed no turn is left out, as the silence handlers leave it (workflow's
		// phaseWorkerRecord): a merged root's merger, or a role whose completion of this phase
		// entry was accepted. The rules read no store, so the pull request and the phase row
		// that say so are read here, and only for a worker a rule has already named, never for
		// every issue of the project.
		_, owed, err := workflow.PhaseWorkerOwedTurn(ctx, a.store, tx, held.issue, held.role)
		if err != nil {
			return nil, err
		}
		if !owed {
			continue
		}
		// A rule answered true, so this run has a claim, and its record holds the last step and,
		// for a background stall, the moment of the report the rule judged it by.
		lastStep := run.Claim.Step.LastStep()
		entry := record.SilentIssue{Issue: held.issue.Key, Role: held.role, LastStep: lastStep,
			Silent: now.Sub(lastStep).Round(time.Minute).String()}
		if background {
			entry.BackgroundSince = run.Claim.ActivityAt
		}
		silent = append(silent, entry)
	}
	slices.SortFunc(silent, func(x, y record.SilentIssue) int { return cmp.Compare(x.Issue, y.Issue) })
	return silent, nil
}

// Reconcile applies the bounded Dispatch boot read to existing records, then fills newly available
// capacity using the same promotion effects Apply emits. It performs no Dispatch I/O itself.
//
// The read is a snapshot with no actor on it: in it, a session's own status write during a restart
// looks exactly like a human's move. Dispatch says how far each issue's event log has run, so a
// key the listing shows behind that log — recorded or not, whatever its listed status — is held
// back rather than decided on now: the stream still holds those events (or the outbox that would
// still publish one), and a stale one can still redeliver after this read, unless the Dispatch
// consumer's position had already Reached target when the caller measured it. Then nothing more
// is coming for any of it, ever, so every summary applies now instead of holding a key nothing
// will later release. target and position are the caller's own single measurement of the stream
// and the Dispatch consumer, taken once for the whole call, not per issue.
//
// The listing reaches admission outside intake.ApplyFact, so each summary takes the store's own
// rule here as a fact takes it there (storetext.Strings), before anything compares it with a
// record: a title, a rank or a status the rule changes is stored as the rule makes it, and
// compared as listed it would be written again at every boot. That holds for a summary Reconcile
// defers too, which release applies later from memory.
//
// It answers the key of each summary it found older than an issue fact of its issue the daemon
// has already applied, so recorded nothing from (applySummary), in the order it met them. That
// fact decided the issue as it stood when it applied, and some of what it decided depends on what
// the daemon held then: a child's event applied while its parent had no record records nothing,
// and only a listing records such a child later. So a recovery keeps each such key in the scope of
// its next pass (internal/daemon's recoverRepositories), whose listing, read after that fact,
// decides it. Such a summary is found so before it could be held, so a summary Reconcile defers
// was no older than any fact applied when it was deferred, and release answers nothing: a fact
// applied after it was deferred, newer than it, that records nothing refreshes it
// (refreshHeldSummary) or leaves its own trace — a note, or its repository's recovery.
func (a *Admission) Reconcile(ctx context.Context, tx pgx.Tx, summaries []intake.DispatchIssue, target int64, position intake.DispatchConsumerPosition) ([]string, error) {
	summaries = storetext.Strings(summaries)
	issues, err := a.store.LiveIssues(ctx, tx, a.project)
	if err != nil {
		return nil, fmt.Errorf("list admission issues: %w", err)
	}
	facts, err := a.lineFacts(ctx, tx, issues)
	if err != nil {
		return nil, err
	}
	live := record.LiveRoots(issues, facts)

	caughtUp := position.Reached(target)
	deferred := make(map[string]intake.DispatchIssue)
	var refused []string
	for _, summary := range parentsFirst(summaries) {
		outcome, err := a.applySummary(ctx, tx, summary, live, !caughtUp)
		if err != nil {
			return nil, err
		}
		switch outcome {
		case summaryHeld:
			a.log.Debug("admission reconcile: the stream may still hold or redeliver an event for this issue; holding it for the consumer to catch up",
				"issue", summary.Key, "status", summary.Status, "dispatch", summary.Seq)
			deferred[summary.Key] = summary
		case summaryStale:
			refused = append(refused, summary.Key)
		}
	}

	if len(deferred) > 0 {
		a.holdDeferred(deferred, target)
		a.log.Info("admission reconcile: holding roots for the Dispatch consumer to reach a stream position",
			"count", len(deferred), "target", target, "ack_floor", position.AckFloorStream)
	}

	_, released, err := a.promote(ctx, tx)
	if err != nil {
		return nil, err
	}
	return refused, a.wakeForFreeSlot(ctx, tx, released)
}

// parentsFirst orders a Dispatch listing by how many of each issue's ancestors the listing itself
// holds, so every issue comes after its parent, and keeps the listing's own order among the issues
// of one such depth. The workflow records a child only under a recorded parent, so an unrecorded
// todo child under an unrecorded todo parent of a live tree is recorded in the same pass as that
// parent, at any depth. A recorded issue is ordered by the same rule, so a root's own listed move —
// a close, a re-admission — is applied before its children's, which is the order the tree's own
// events carry: a child's re-entry then reads the tree as that move left it.
func parentsFirst(summaries []intake.DispatchIssue) []intake.DispatchIssue {
	parents := make(map[string]string, len(summaries))
	for _, summary := range summaries {
		parents[summary.Key] = summary.Parent
	}
	depth := make(map[string]int, len(summaries))
	for _, summary := range summaries {
		// The bound stops a parent cycle Dispatch should never hold; such a cycle only orders late.
		for parent, steps := summary.Parent, 0; parent != "" && steps < len(summaries); steps++ {
			next, listed := parents[parent]
			if !listed {
				break
			}
			depth[summary.Key]++
			parent = next
		}
	}
	ordered := slices.Clone(summaries)
	slices.SortStableFunc(ordered, func(x, y intake.DispatchIssue) int { return cmp.Compare(depth[x.Key], depth[y.Key]) })
	return ordered
}

// putNewRoot records an unrecorded root or orphan handed to Legion in todo, on the repository its
// observation resolves to and that repository's default branch (workflow's
// ResolveIssueRepository, which takes the refusal the observation's own preparation read from
// GitHub when it carries one). One whose repository resolves to none, or one GitHub does not
// answer for as a repository a role App reaches, is not recorded: the daemon says why on the
// issue, once for that resolution, and the issue's next observation in todo is admission's next
// look at it. A root recorded on a repository nothing named before has that repository heard and
// cached from here (workflow.Engine.HearRepository).
//
// This is where a root is first recorded, so it is where the issue-unit experiments the project
// declares are drawn for it (drawExperiments): every issue the daemon records is drawn for at the
// moment it records it, and a root is an issue like any other. Its tree's own experiments are
// another draw, made when its tree is opened (promoteHolds).
func (a *Admission) putNewRoot(ctx context.Context, tx pgx.Tx, observation intake.DispatchIssue, logOrphan bool) error {
	resolution, err := a.engine.ResolveIssueRepository(observation)
	if err != nil {
		return fmt.Errorf("resolve the repository of %s: %w", observation.Key, err)
	}
	if resolution.Refusal != "" {
		noted, err := a.engine.RefuseRecording(ctx, tx, observation.Key, observation.ResolvedRepo, resolution)
		if err != nil {
			return fmt.Errorf("say why %s is not admitted: %w", observation.Key, err)
		}
		if noted {
			store.OnCommit(ctx, func() {
				a.log.Info("admission: not admitted; GitHub does not answer for its repository as one Legion's Apps reach", "issue", observation.Key,
					"resolved", observation.ResolvedRepo, "reason", resolution.Refusal)
			})
		}
		return nil
	}
	issue := record.Issue{
		Key:             observation.Key,
		Tree:            observation.Key,
		Project:         a.project,
		Title:           observation.Title,
		Parent:          record.ParentOf(observation.Parent),
		Phase:           phase.Admitted,
		Generation:      1,
		Status:          "todo",
		Rank:            observation.Rank,
		Priority:        observation.Priority,
		HandedOver:      observation.HandedOver,
		LastDispatchSeq: observation.Seq,
		DispatchStatus:  observation.Status,
		Repo:            resolution.Repo,
		DefaultBranch:   resolution.DefaultBranch,
		GitHubRepo:      resolution.GitHubRepo,
	}
	if issue.Ancestry, err = a.engine.Ancestry(ctx, tx, issue, observation.Ancestry); err != nil {
		return fmt.Errorf("read whether %s ran as a tree of its own: %w", observation.Key, err)
	}
	if err := a.engine.WriteIssue(ctx, tx, issue); err != nil {
		return fmt.Errorf("record admitted root %s: %w", observation.Key, err)
	}
	a.engine.HearRepository(ctx, issue.GitHubRepo)
	if err := a.drawExperiments(ctx, tx, issue.Key, config.ExperimentIssue); err != nil {
		return err
	}
	if logOrphan && observation.Parent != "" {
		store.OnCommit(ctx, func() { a.log.Info("admission orphan", "issue", observation.Key, "parent", observation.Parent) })
	}
	return nil
}

// drawExperiments draws every experiment the project declares over unit (UseExperiments) and
// records the answers against key, in the caller's transaction (experiment.DrawAndRecord), with
// the engine's random source: the tree-unit experiments of a tree against its root's key when the
// tree's lifecycle row is created (promoteHolds), and the issue-unit experiments of an issue
// against its own key when the daemon first records it (putNewRoot).
func (a *Admission) drawExperiments(ctx context.Context, tx pgx.Tx, key string, unit config.ExperimentUnit) error {
	return experiment.DrawAndRecord(ctx, tx, a.store, key, a.experiments, unit, a.engine.ExperimentSource(), a.now())
}

// declaredAssignments is one entry per experiment the project declares now, carrying its name and
// its unit: what a hand-over settles a child at for an experiment the tree it left holds no
// answer for (record.Store.CarryAssignments, readmit).
func (a *Admission) declaredAssignments() []record.DeclaredExperiment {
	settled := make([]record.DeclaredExperiment, 0, len(a.experiments))
	for _, declared := range a.experiments {
		settled = append(settled, record.DeclaredExperiment{Experiment: declared.Name, Unit: string(declared.Unit)})
	}
	return settled
}

// applySummary applies one Dispatch boot-listing summary to admission's own record of the issue
// it names, exactly as it would if nothing had ever deferred it: a still-unrecorded key is
// recorded only when the summary itself shows it todo — into its parent's live tree as the
// workflow records a child, else as a root when it is handed to Legion — or queued when it shows a
// root in triage whose legion label a person set, and a recorded key a live event has already
// carried strictly past the summary's own sequence is left alone — reapplying the summary would
// overwrite fresher information with stale. An unrecorded key is held to the issue facts the
// daemon applied: a summary older than an event of the issue, or a view intake let through, that
// the daemon has already applied records and queues nothing (unrecordedSummaryStale), and
// applySummary answers summaryStale for it. Both Reconcile, immediately when the consumer is
// already caught up, and release, once the consumer catches up to a summary Reconcile deferred,
// call this the same way: a key nothing else ever gets around to explaining still lands on what
// the listing already knew.
//
// With hold set — Reconcile's call while the consumer has not reached its target — a summary the
// listing shows behind Dispatch's event log (Reconcile says why) applies nothing either, and
// applySummary answers summaryHeld for it, for the caller to defer. A stale summary is answered
// so before it could be held, since no fact the stream still delivers makes it any less stale.
func (a *Admission) applySummary(ctx context.Context, tx pgx.Tx, summary intake.DispatchIssue, live map[string]struct{}, hold bool) (summaryOutcome, error) {
	stored, err := a.store.Issue(ctx, tx, summary.Key)
	if err != nil {
		return summaryApplied, fmt.Errorf("read reconciled issue %s: %w", summary.Key, err)
	}
	if stored == nil {
		stale, err := a.unrecordedSummaryStale(ctx, tx, summary)
		if err != nil {
			return summaryApplied, err
		}
		if stale {
			return summaryStale, nil
		}
	}
	if hold && (stored == nil && summary.Seq > 0 || stored != nil && summary.Seq > stored.LastDispatchSeq) {
		return summaryHeld, nil
	}
	return summaryApplied, a.applyListed(ctx, tx, summary, stored, live)
}

// summaryOutcome is what applySummary did with a summary.
type summaryOutcome uint8

const (
	// summaryApplied is a summary applied to admission's record (applyListed).
	summaryApplied summaryOutcome = iota
	// summaryStale is a summary of an unrecorded issue older than an issue fact of it the daemon
	// applied, which applied nothing (unrecordedSummaryStale).
	summaryStale
	// summaryHeld is a summary behind Dispatch's event log that applied nothing, for Reconcile to
	// defer.
	summaryHeld
)

// applyListed is applySummary's work once the summary is found no older than the issue facts the
// daemon applied and not held: stored is admission's record of the issue, nil for none.
func (a *Admission) applyListed(ctx context.Context, tx pgx.Tx, summary intake.DispatchIssue, stored *record.Issue, live map[string]struct{}) error {
	handed := summary.HandedOver
	if stored == nil {
		// An unrecorded root the listing shows in triage with the label is queued as its event
		// would be (applyFact) when a person set the label, which the listing's preparation read
		// from the issue's event log: the person's label fell while the daemon was down, or on a
		// stream position a fresh consumer never reads, and only the listing can see it. Any other
		// setter returns below as it did before, with no wake: the listing carries no actor, so
		// only a person's label read from the log queues, and a session's label at boot is the
		// controller's to find at its own start, as it always was.
		if summary.LabelledTriageRoot() && summary.LabelSetByPerson() {
			return a.queuePersonHandover(ctx, tx, summary.Key, "triage", "listing")
		}
		if summary.Status != "todo" {
			return nil
		}
		// An unrecorded todo child of a live tree is the workflow's to record, as its own todo event
		// would (the engine's recordChildUnderLiveTree, which runs before admission on every live
		// event), from the parent the listing shows now. Without this, a child no event of it ever
		// recorded stays unrecorded under its running tree, and no later event of it need come:
		// Dispatch already shows it todo, so nothing writes that status again. Two ways that
		// happens — an event the daemon never received (a durable consumer created fresh takes only
		// what is published after it, and the notification stream drops a message 72 hours old
		// whether or not anyone read it), and one it received while the child's parent had no
		// record or its tree was not live, where the workflow records nothing. It runs before the
		// root path, so a labelled child of a live tree is recorded as that tree's member, never as
		// a root of its own that the line then adopts (adoptQueuedRoots). A child the workflow does
		// not record here — its tree is not live, or its parent has no record — is left to the root
		// path below, as an orphan handed to Legion is: one whose Dispatch chain reaches a live root
		// waits in the line held under that tree's unreleased child (its Ancestry).
		if summary.Parent != "" {
			if _, err := a.engine.Apply(ctx, tx, summary); err != nil {
				return fmt.Errorf("record %s under its parent's tree: %w", summary.Key, err)
			}
			recorded, err := a.store.Issue(ctx, tx, summary.Key)
			if err != nil {
				return fmt.Errorf("re-read %s after recording it under its parent's tree: %w", summary.Key, err)
			}
			if recorded != nil {
				return nil
			}
		}
		if !handed {
			a.log.Debug("admission: not handed to Legion", "issue", summary.Key, "label", dispatch.LegionLabel)
			return nil
		}
		return a.putNewRoot(ctx, tx, summary, false)
	}
	if summary.Seq > 0 && summary.Seq < stored.LastDispatchSeq {
		return nil
	}
	// The record keeps the repository it was recorded with, as for a live event (the engine's
	// NoteRepositoryKept); the note is said once per resolution, so a listing no newer than the
	// record says nothing again.
	if err := a.engine.NoteRepositoryKept(ctx, tx, *stored, summary.ResolvedRepo); err != nil {
		return err
	}
	seq := stored.LastDispatchSeq
	if summary.Seq > seq {
		seq = summary.Seq
	}
	status := stored.Status
	if summary.Status != stored.Status && summary.Seq > stored.LastDispatchSeq {
		// A snapshot that changes a recorded issue's status, strictly newer than what is stored, is
		// the fact a live Dispatch event of the same change would carry, so it runs through the
		// engine's own transition table first — leave/beginLinger out of the workflow, or
		// ReenterChild for a live tree's child moved back to todo — exactly as that event would.
		// Engine.dispatchIssue already makes this same newer-or-not decision on its own sequence
		// fence (fact.Seq <= issue.LastDispatchSeq); pre-filtering by status here to
		// OutOfWorkflow alone left a parked child's own re-entry unreached, stranding it recorded
		// todo at phase done under a live tree. Left to recordObservation alone, the record would
		// move to the snapshot's status with no suspend and no linger, and the record's
		// LastDispatchSeq would reach the snapshot's own sequence, so the real event, once it
		// finally arrives, would be dropped by that same sequence fence — nothing would ever run
		// the transition this snapshot stands in for. A snapshot no newer than the record — a
		// session's own status write the daemon already recorded through sessionStatusWrite, echoed
		// back by a boot listing taken before the daemon's own reassert landed — is level, not a
		// change to apply: status stays what recordObservation below already knows, an admitted
		// root's or a live tree's own state, not the record it briefly showed on Dispatch. Re-read
		// afterward: the engine's own write (phase, hold, linger) must not be clobbered by
		// recordObservation writing back the stale copy this call fetched before the engine ran.
		if _, err := a.engine.Apply(ctx, tx, summary); err != nil {
			return fmt.Errorf("apply engine transition for %s: %w", summary.Key, err)
		}
		refreshed, err := a.store.Issue(ctx, tx, summary.Key)
		if err != nil {
			return fmt.Errorf("re-read %s after its engine transition: %w", summary.Key, err)
		}
		// The engine consumed the snapshot when it recorded the snapshot's sequence: it kept its own
		// status over a status Dispatch already showed (keepStatus), or re-entered a child, and wrote
		// the observation itself. Recording the snapshot's status now would put a set-back's backlog
		// on a running tree, whose slot the release would then free. Otherwise the snapshot's
		// move is recorded, as the live event's would be.
		if refreshed.LastDispatchSeq >= summary.Seq {
			return nil
		}
		stored, status = refreshed, summary.Status
	}
	o := observed{
		Title: summary.Title, Parent: summary.Parent, Rank: summary.Rank, Priority: summary.Priority,
		Status: status, Shown: summary.Status, Ancestry: summary.Ancestry, HandedOver: handed, Seq: seq,
	}
	if summary.Status == "todo" && handed && stored.Readmittable() {
		return a.readmit(ctx, tx, *stored, o, false)
	}
	// A todo echo of an issue the record shows running is the boot listing catching a status the
	// daemon itself wrote and Dispatch had not shown yet. It is taken only while the issue's tree
	// is not live: a running tree's member keeps what the record says, because the tree is the
	// thing that says the work is under way, and the row the member holds, or does not hold, says
	// nothing about it: it keeps that row through every phase that keeps one, worked or not
	// (record.KeepsSlot), holds none in accepting, retro or merging, and gives it back while it
	// waits on another issue (releaseSlotsForWaits).
	if summary.Status == "todo" && stored.Status == "in_progress" {
		if _, running := live[stored.Tree]; running {
			return nil
		}
	}
	return a.recordObservation(ctx, tx, *stored, o)
}

// unrecordedSummaryStale rejects a listing older than an issue fact already applied to an
// unrecorded issue, even when that fact created no record. The fact may have applied while the
// listing waited for GitHub or for the consumer, so admitting it could lose a repository edit.
//
// Compare the applied sequence, not dispatch_seq: blockers.Mirror raises it only for an issue fact
// (an event or a view intake accepted), in the fact's own transaction. Listings and tick reads do
// not move it. Both callers of applySummary hold the fact lock; intake.settleObserved uses the
// same sequence for views. A summary without a sequence cannot be ordered.
//
// Reconcile returns stale keys so recovery retains them for a later listing. A child's event can
// record nothing because its parent is not yet recorded, without leaving a recovery note.
func (a *Admission) unrecordedSummaryStale(ctx context.Context, tx pgx.Tx, summary intake.DispatchIssue) (bool, error) {
	if summary.Seq <= 0 {
		return false, nil
	}
	applied, err := a.store.DispatchIssueApplied(ctx, tx, summary.Key)
	if err != nil {
		return false, fmt.Errorf("read how far the daemon has applied the issue facts of %s: %w", summary.Key, err)
	}
	if applied <= summary.Seq {
		return false, nil
	}
	a.log.Debug("admission: the listing is older than an issue fact of the issue the daemon applied; it records nothing",
		"issue", summary.Key, "listed", summary.Seq, "applied", applied)
	return true, nil
}

// applyObservation records a newer Dispatch observation of a recorded issue: the one place a live
// event's title, rank, priority, parent, label, and status reach the record, so a re-rank, a
// priority change, a rename, or the label taken off or put back at the same status reaches the
// record at once. A todo on a lingering or closed root handed to Legion is a re-admission.
func (a *Admission) applyObservation(ctx context.Context, tx pgx.Tx, stored record.Issue, observation intake.DispatchIssue) error {
	if observation.Seq != 0 && observation.Seq <= stored.LastDispatchSeq {
		return nil
	}
	handed := observation.HandedOver
	o := observed{
		Title: observation.Title, Parent: observation.Parent, Rank: observation.Rank, Priority: observation.Priority,
		Status: observation.Status, Shown: observation.Status, Ancestry: observation.Ancestry, HandedOver: handed, Seq: observation.Seq,
	}
	if observation.DaemonActor && observation.HandedOver && observation.Status == "todo" {
		if handover, err := a.store.Handover(ctx, tx, stored.Key); err != nil || handover == nil {
			if err != nil {
				return err
			}
		} else if stored.Tree == handover.Tree {
			root, err := a.store.Issue(ctx, tx, handover.Tree)
			if err != nil {
				return err
			}
			// The carry's own test is the child's own run (record.Issue.CarriesRun), deliberately
			// and only that: what the child waits on is not read here. A hand-over moves work
			// already under way into a root of its own, and promotion admits such a candidate
			// whatever its links say (blockers.WaitsOn).
			if !handover.Cancelled && root != nil && root.LingersAt(handover.Generation) {
				store.OnCommit(ctx, func() {
					a.log.Info("admission hand-over", "issue", stored.Key, "tree", handover.Tree, "linger", handover.Generation)
				})
				return a.readmit(ctx, tx, stored, o, stored.CarriesRun())
			}
		}
	}
	if observation.Status == "todo" && handed && stored.Readmittable() {
		return a.readmit(ctx, tx, stored, o, false)
	}
	orphan, err := a.orphan(ctx, tx, stored, observation.Status, handed)
	if err != nil {
		return err
	}
	if orphan {
		// An orphan that started a phase is the same work as a hand-over, whoever handed it to
		// Legion: a child stranded by a root that closed before its hand-over existed carries its
		// phase, gate and pull request too (record.Issue.CarriesRun).
		store.OnCommit(ctx, func() { a.log.Info("admission orphan", "issue", stored.Key, "parent", observation.Parent) })
		return a.readmit(ctx, tx, stored, o, stored.CarriesRun())
	}
	return a.recordObservation(ctx, tx, stored, o)
}

// consumeHandover completes an outbox hand-over when Dispatch already showed its todo and label.
// It is the direct equivalent of the daemon-authored event path, but cannot be mistaken for an
// ordinary daemon todo echo because it carries the persisted fence identity. It logs the hand-over
// as the event path does, marked as the outbox's, once it commits: the live proof tells the two
// consumers of a person's labelled todo (this, or admission's orphan carry) apart by it. Its carry
// is the event path's: record.Issue.CarriesRun, and not what the child waits on.
func (a *Admission) consumeHandover(ctx context.Context, tx pgx.Tx, fact intake.Handover) error {
	stored, err := a.store.Issue(ctx, tx, fact.Issue)
	if err != nil || stored == nil {
		return err
	}
	handover, err := a.store.Handover(ctx, tx, fact.Issue)
	if err != nil || handover == nil || handover.Cancelled || handover.Tree != fact.Tree || handover.Generation != fact.Generation || stored.Tree != fact.Tree {
		return err
	}
	root, err := a.store.Issue(ctx, tx, fact.Tree)
	if err != nil || root == nil || !root.LingersAt(fact.Generation) {
		return err
	}
	store.OnCommit(ctx, func() {
		a.log.Info("admission hand-over", "issue", fact.Issue, "tree", fact.Tree, "linger", fact.Generation, "via", "outbox")
	})
	return a.readmit(ctx, tx, *stored, observed{
		Title: stored.Title, Parent: deref(stored.Parent), Rank: stored.Rank, Priority: stored.Priority, Ancestry: stored.Ancestry, Seq: stored.LastDispatchSeq,
	}, stored.CarriesRun())
}

// orphan says whether an observation makes a recorded child an orphan, admitted as a root of its
// own: the child is todo and handed to Legion, and its tree is not live. Only a change hands it
// over — its move into todo, or the label reaching it while it is todo — so a later edit of a child
// that was both already is none. The tree is read either way: the workflow handler, which runs
// first, re-enters a live tree's child moved into todo, but not one the label reaches while it
// waits in its live tree, where the label changes nothing.
func (a *Admission) orphan(ctx context.Context, tx pgx.Tx, stored record.Issue, status string, handed bool) (bool, error) {
	if status != "todo" || !handed || claim.IsTreeRoot(stored.Key, stored.Tree) || (stored.Status == "todo" && stored.HandedOver) {
		return false, nil
	}
	root, err := a.store.Issue(ctx, tx, stored.Tree)
	if err != nil {
		return false, fmt.Errorf("read the tree root of %s: %w", stored.Key, err)
	}
	if root == nil {
		return true, nil
	}
	live, err := record.TreeLive(ctx, a.store, tx, *root, a.engine.GatedSteps())
	if err != nil {
		return false, fmt.Errorf("read whether %s's tree is live: %w", stored.Key, err)
	}
	return !live, nil
}

// observed is what one Dispatch observation of an issue says about it, from a live event or from
// the boot read: Shown is the status Dispatch showed, and Status the one the record takes, which is
// Shown except from a boot listing no newer than the record, which keeps the record's own.
// Ancestry is the Dispatch chain its preparation read, which a record takes as the engine's
// Ancestry says, and Priority the issue's own Dispatch priority.
type observed struct {
	Title, Parent, Rank, Status, Shown string
	Priority                           record.Priority
	Ancestry                           []string
	HandedOver                         bool
	Seq                                int64
}

// recordObservation is where admission writes a Dispatch observation to an issue record: a live
// event's and the boot read's, so a rename, a re-rank, a priority change, a re-parent, a label or a
// status change is written the same way whichever brought it. Each caller owns its own guards — the
// stream's sequence fence, the boot read's — and this writes what they let through. The engine,
// which runs first, writes the observation itself when it keeps its own status over the event's
// (its keepStatus: a session's write it sets back, or an edit that changed no status), and a live
// event it wrote then stops at the sequence fence here. A re-parent never changes the record's tree
// here: the only path that does is adoption, which the same fact's promote runs next
// (adoptQueuedRoots) for a todo root that never ran and whose new parent is in a live tree.
func (a *Admission) recordObservation(ctx context.Context, tx pgx.Tx, stored record.Issue, o observed) error {
	ancestry, err := a.engine.Ancestry(ctx, tx, stored, o.Ancestry)
	if err != nil {
		return fmt.Errorf("read whether %s ran as a tree of its own: %w", stored.Key, err)
	}
	if stored.Title == o.Title && stored.Rank == o.Rank && stored.Priority == o.Priority && stored.Status == o.Status && stored.DispatchStatus == o.Shown &&
		stored.HandedOver == o.HandedOver && sameParent(stored.Parent, record.ParentOf(o.Parent)) && stored.LastDispatchSeq == o.Seq &&
		slices.Equal(stored.Ancestry, ancestry) {
		return nil
	}
	stored.Title = o.Title
	stored.Parent = record.ParentOf(o.Parent)
	stored.Rank = o.Rank
	stored.Priority = o.Priority
	stored.Status = o.Status
	stored.DispatchStatus = o.Shown
	stored.HandedOver = o.HandedOver
	stored.LastDispatchSeq = o.Seq
	stored.Ancestry = ancestry
	if err := a.engine.WriteIssue(ctx, tx, stored); err != nil {
		return fmt.Errorf("record observation of %s: %w", stored.Key, err)
	}
	return nil
}

// readmit records a lingering or closed root's todo, or an orphan's, as a new generation waiting
// for a slot, taking o's title, parent, rank, priority, Dispatch chain (as the engine's Ancestry
// takes it; a root that left another tree takes none) and sequence. A root starts a fresh
// generation: its facts clear across its whole tree, as do those of an orphan readmitted without
// carry. An orphan readmitted with carry (one that started a phase) is the same work moving from a
// finished root to a root of its own, so its phase, phase rows, gate and pull request (with its
// observed head) carry into the new generation, and its promotion starts both its architect and the
// role of that carried phase. A merged pull request is never observed as merged again, and a review
// or an acceptance would otherwise rerun work already decided or landed.
//
// An issue leaving another tree, a hand-over or an orphan, carried or not, records that tree
// (record.Issue.HandedOverFrom) and stays a tree of its own from then on: re-admitting or reopening
// the root it left never takes it back (workflow's RanAsRoot), nor does any other tree's release.
//
// The record's record.Issue.TakenBack is the workflow's mark that the issue has stood in a phase of
// its own (workflow.Engine.transition writes it), and promotion exempts a root carrying it as a tree
// Legion takes back (blockers.Exempt). Only the issue's own tree coming back is that: so a root
// whose tree ran keeps the mark here and starts again whatever its links say, while a child
// arriving from another tree's run without its run loses it — what it ran was that tree's work, and
// the root of its own it becomes has run nothing — and is checked like any other root, as is a
// waiting root that only left the line and returned, which never earned one. An orphan carrying its
// run keeps its mark with the run: that run is its new tree's from here on, and the tree it roots
// has run when it is taken back. Nothing here writes the mark: a run is earned by running, never by
// arriving.
func (a *Admission) readmit(ctx context.Context, tx pgx.Tx, stored record.Issue, o observed, carry bool) error {
	if stored.Generation == ^uint64(0) {
		return fmt.Errorf("re-admit %s: generation overflows", stored.Key)
	}
	orphan := carry && !claim.IsTreeRoot(stored.Key, stored.Tree)
	oldTree, oldGeneration := stored.Tree, stored.Generation
	// A member leaving another tree gets that tree's suspend for every role before the member's new
	// generation is written. The outbox decides whether each claim still stands in the old tree.
	if oldTree != stored.Key {
		if err := a.engine.StopLeftTree(ctx, tx, stored); err != nil {
			return err
		}
	}
	var gates map[record.DesignStep]record.DesignGate
	var pr *record.PullRequest
	var err error
	if orphan {
		gates, err = a.store.GatesOf(ctx, tx, oldTree)
		if err != nil {
			return fmt.Errorf("read gate of re-admitted orphan %s: %w", stored.Key, err)
		}
		pr, err = a.store.PullRequest(ctx, tx, stored.Key)
		if err != nil {
			return fmt.Errorf("read pull request of re-admitted orphan %s: %w", stored.Key, err)
		}
	}
	stored.Project = a.project
	stored.Title = o.Title
	stored.HandedOver = true
	stored.Parent = record.ParentOf(o.Parent)
	stored.Tree = stored.Key
	// A member leaving another tree, carried or not, is a root of its own from here on: the tree it
	// left is recorded so nothing adopts it back (workflow's RanAsRoot). A root re-admitting itself
	// keeps whatever its record says.
	if oldTree != stored.Key {
		stored.HandedOverFrom = oldTree
	}
	// A root that left another tree, a carried orphan with its run's mark (record.Issue.TakenBack)
	// among them, stays its own tree (workflow's RanAsRoot) and records no chain to be held under.
	stored.Ancestry = nil
	if !orphan {
		// A new generation starts at admitted with nothing held: the start a parent held in
		// awaiting_children goes with the generation that held it (record.Issue.ChildrenWait),
		// as the hold and the wait on a person go. Its first phase waits on its open children
		// under the children-first rule, as any re-admitted root's does.
		stored.Phase, stored.Hold, stored.Awaiting, stored.ChildrenWait = phase.Admitted, nil, nil, nil
		// Another tree's work arriving without its run: the run it had was that tree's.
		if !claim.IsTreeRoot(stored.Key, oldTree) {
			stored.TakenBack = false
		}
		// Read once the mark and the tree it left are settled: only a root re-admitting itself that
		// never ran and never left another tree keeps a chain.
		if stored.Ancestry, err = a.engine.Ancestry(ctx, tx, stored, o.Ancestry); err != nil {
			return fmt.Errorf("read whether re-admitted %s ran as a tree of its own: %w", stored.Key, err)
		}
	}
	stored.Generation++
	stored.Status, stored.DispatchStatus = "todo", "todo"
	stored.Rank = o.Rank
	stored.Priority = o.Priority
	stored.LingerUntil = nil
	stored.LastDispatchSeq = o.Seq
	if err := a.engine.WriteIssue(ctx, tx, stored); err != nil {
		return fmt.Errorf("record re-admission %s: %w", stored.Key, err)
	}
	// The re-admission is a new generation of the tree: this write changes the generation, the
	// phase, the status and, below, the facts of every issue under the root, which is news of
	// the issue and so a movement (record.Store.MarkIssueMoved, the rule in workflow's
	// dispatchIssue). A person's live todo is marked by the engine's own handler as well, and
	// the mark is written forwards alone, so the second write of the same fact changes nothing;
	// the daemon's own hand-over and the boot listing's re-admission reach no such mark, and
	// the generation this opens is news whichever of the three brought it. That is what parts
	// it from the listing's plain observation, which the rule leaves unmarked because it tells
	// the daemon what it already knows.
	if err := a.store.MarkIssueMoved(ctx, tx, stored.Key, a.now()); err != nil {
		return err
	}
	// The members this generation leaves mid-phase, and those reopened at done, come back the
	// first time its tree runs: at the promotion that opens it, or at the pass that finds it
	// running first, its design gate opened before the root's promotion from the waiting line
	// (returnMembersOfRunningTrees). The mark is what makes either of them bring the members back
	// once in the generation (record.Issue.MembersOwed, dispatch://INFRA-5471).
	if err := a.store.OweMembers(ctx, tx, stored.Key); err != nil {
		return fmt.Errorf("record re-admission %s: %w", stored.Key, err)
	}
	// A generation that ended takes what its architect handed back with it: the re-admission is a
	// handout, so an ask.opened or a hand-back of the old run, delivered late, at or below what the
	// record holds now, declares nothing in this one. Its catch-up cursor starts again too, so the
	// next boot reads from this handout rather than replaying the old generation's tail, whose
	// changes requests the new generation would tell again.
	if err := a.store.StartHandout(ctx, tx, stored.Key); err != nil {
		return fmt.Errorf("record re-admission %s: %w", stored.Key, err)
	}
	if err := a.store.RestartCaughtUp(ctx, tx, stored.Key); err != nil {
		return fmt.Errorf("record re-admission %s: %w", stored.Key, err)
	}
	// A child leaving its old tree, carried or fresh, takes its hand-over fence with it: left behind,
	// it would read a later daemon todo of this root (a controller's reopen) as the stale hand-over.
	// The hand-over settles its experiments too, here and not at its own first open: it takes the
	// answers of the tree it leaves, each verbatim with the time it was drawn, so the work goes on
	// running under the arms it has been running under. Every experiment declared at this moment
	// is passed as settled, so one the old tree holds no answer for is settled here at no variant
	// and is never drawn for later — a tree first opened before that experiment was declared,
	// which nothing draws for. A child out of a tree nothing ever opened has run under nothing, so
	// the carry writes it nothing and its own first promotion draws it, which the carry decides
	// from the old tree's lifecycle row. tree_lifecycles is keyed by the normalized project token,
	// so the carry is handed that token, as promoteHolds reads it.
	if oldTree != stored.Key {
		if err := a.store.DeleteHandover(ctx, tx, stored.Key); err != nil {
			return err
		}
		project, err := claim.ProjectToken(a.project)
		if err != nil {
			return fmt.Errorf("carry the experiments of re-admitted %s: %w", stored.Key, err)
		}
		if _, err := a.store.CarryAssignments(ctx, tx, project, stored.Key, oldTree, a.declaredAssignments()); err != nil {
			return err
		}
	}
	if orphan {
		if pr != nil {
			// The carried pull request keeps its CI settlement fence: its Generation is the
			// listener's generation of the settlement that stands, not the issue's, so a settlement
			// of its head from before the carry, delivered late or replayed, is as stale after it as
			// it was before. Its head run goes, as with a new generation (record.ClearGeneration),
			// since its replay would put the old fence back; the write deletes the run's own memory.
			pr.Run = nil
			if err := a.store.PutPullRequest(ctx, tx, *pr); err != nil {
				return fmt.Errorf("carry pull request to re-admitted orphan %s: %w", stored.Key, err)
			}
		}
		// The carried run goes on at the step it stood at, so its plan goes with it, each task
		// where it stood; an orphan that ran before plans has none, and the carry writes nothing.
		if err := a.store.CarryPlan(ctx, tx, stored.Key, oldGeneration, stored.Generation, true); err != nil {
			return fmt.Errorf("carry the plan to re-admitted orphan %s: %w", stored.Key, err)
		}
		// The carried run keeps the phase it ran in, past admitted (record.Issue.CarriesRun), so it
		// never leaves admitted, where a run's plan is written otherwise: its new generation's plan
		// node is written here (WritePlan), after the carry, so it names the plan the run carries.
		if err := a.engine.WritePlan(ctx, tx, stored); err != nil {
			return fmt.Errorf("write the plan of re-admitted orphan %s: %w", stored.Key, err)
		}
		// Every root a hand-over created is recorded as one, so its architect's catch-up says it was
		// handed over and decomposes nothing anew; it is marked as carrying a gate when its old root
		// had one, and its architect then registers, on this issue's own document, each step the
		// carry brought no row for. The carry takes every step's row, each keeping its own step, so a
		// tree gated on several steps arrives approved on each of them as the old root was; only a
		// step that rests on an ancestor's approval can arrive otherwise (carriedGate). A root
		// reopened in its linger has its gate cleared with the generation, so a second close hands
		// its children over with none: such a child keeps its phase and pull request, and its
		// architect registers its spec as a fresh root's does, the gate every member of its tree
		// starts behind.
		//
		// An old root that recorded no gate is the common case, and there the carry writes nothing:
		// the orphan's own pairs stand as the hand-over left them, so neither the read of them nor
		// the comparison against them is made. The carry writes no view fact either: the old root's
		// (record.Issue.ViewOf) is what its own spec registration read of its own documents, and
		// this issue's stands as it is until its own architect registers its spec.
		//
		// What the orphan's holders took of the approved design (record.DesignTold) stays with the
		// gate it was taken from, as the copy does: the carry clears no generation, so its holder
		// row stands as its last holder left it, at versions of the very documents the carried rows
		// name. The phase the promotion starts again inherits it rather than taking a new baseline
		// (workflow's Engine.enter), and is owed whatever the old root's gate approved since. Only
		// the generation clear of a tree the orphan still belongs to takes the row away, with that
		// tree's gate.
		//
		// The carried gate is the new tree's, and the new tree's architect starts from it: the carry
		// takes its architect's baseline at the versions the carried rows stand approved at
		// (record.Store.BaselineTold, which leaves a row already there as it stands), as a tree's
		// first opening takes its own (workflow's putGateTold). Its architect never saw the versions
		// the old root's gate approved before, so it is owed no summary of them, and is owed what a
		// later approval of the carried documents changes.
		if len(gates) > 0 {
			// The pairs the orphan itself holds before the carry: a member of a gated tree holds
			// the copy its hand-over carried to it, and the carry writes the old root's rows over
			// that.
			held, err := a.store.GatesOf(ctx, tx, stored.Key)
			if err != nil {
				return fmt.Errorf("read the gates of re-admitted orphan %s: %w", stored.Key, err)
			}
			for _, gate := range gates {
				carried, err := a.carriedGate(ctx, tx, stored.Key, gate)
				if err != nil {
					return fmt.Errorf("carry gate to re-admitted orphan %s: %w", stored.Key, err)
				}
				if carried == nil {
					continue
				}
				if err := a.store.PutGate(ctx, tx, *carried); err != nil {
					return fmt.Errorf("carry gate to re-admitted orphan %s: %w", stored.Key, err)
				}
			}
			if err := a.store.BaselineTold(ctx, tx, stored.Key, record.AudienceArchitect, stored.Key, a.now()); err != nil {
				return fmt.Errorf("take the baseline of re-admitted orphan %s's architect: %w", stored.Key, err)
			}
			// The carry binds this issue's steps to the old root's documents, so it moves the pairs
			// of an orphan that held none and leaves those of one re-admitted onto the same
			// documents (record.PublishChangedGates); a carry is the one path that gives an issue a
			// gate without its architect registering anything, so Dispatch hears of it here or
			// nowhere.
			if err := record.PublishChangedGates(ctx, a.store, tx, a.engine.GatedSteps(), stored.Key, held, a.now()); err != nil {
				return fmt.Errorf("publish the gates carried to re-admitted orphan %s: %w", stored.Key, err)
			}
		}
		return a.store.PutHandedOverRoot(ctx, tx, record.HandedOverRoot{Issue: stored.Key, Generation: stored.Generation, GateCarried: len(gates) > 0})
	}
	// Only the old tree's own root coming back cancels its pending hand-overs; an orphan of that
	// tree admitted by a person leaves its siblings' fences standing.
	if claim.IsTreeRoot(stored.Key, oldTree) {
		if err := a.store.CancelTreeHandovers(ctx, tx, oldTree, oldGeneration); err != nil {
			return err
		}
		if err := a.releaseTreeSlots(ctx, tx, stored.Key); err != nil {
			return fmt.Errorf("record re-admission %s: %w", stored.Key, err)
		}
	}
	if err := a.keepStaleApproval(ctx, tx, stored.Key); err != nil {
		return err
	}
	// A root set back to todo is a new generation of the whole tree, so the old generation's pull
	// request, gate and handoffs go for every issue of it, not only the root's.
	return a.store.ClearTreeGeneration(ctx, tx, stored.Tree, a.now())
}

// carriedGate is gate, a step of the old root's gate, as the carry writes it onto key, the orphan
// that becomes a root of its own, or nil where the carry brings no row for that step
// (dispatch://INFRA-3320, R2 and R8):
//
//   - A step an ancestor's approval opened (record.DesignGate.OnBasis) arrives with its basis, the
//     person's approval it keeps apart, and the ancestor it rests on, so key reads the step as open
//     on that basis, and the ancestor's sign-off waits for key as it waited for the old root. A
//     basis is only on an ancestor that is not accepted (R1), so where the ancestor is done or
//     accepted (record.BasisAcceptance), no row comes: key's catch-up reports the step
//     unregistered, and its architect registers key's own document for it.
//   - A step that waits on an ancestor's open approval request (PendingOnBasis) arrives closed and
//     registered on the old root's document with no basis (record.EndPendingBasis): the wait was
//     the old root's registration on one request, and key's architect registers again.
//   - Every other step arrives as the old root held it, open on a person's approval or closed.
//
// key rests on each ancestor the old root rested on from then on (record.DesignGate.RestsOn): the
// old root rests on every ancestor a basis opened the step on until its generation ends, open or
// closed later, and so does the step it carries, but only on each that is neither done nor
// accepted. A rest on an ancestor that is either does not come with the step, which still comes
// as above.
func (a *Admission) carriedGate(ctx context.Context, tx pgx.Tx, key string, gate record.DesignGate) (*record.DesignGate, error) {
	gate.Issue = key
	standing := ""
	if gate.OnBasis() {
		standing = gate.Basis.Issue
		accepted, _, err := record.BasisAcceptance(ctx, a.store, tx, standing)
		if err != nil {
			return nil, err
		}
		if accepted != "" {
			a.log.Info("admission carry: no row for a step whose basis is on an accepted ancestor", "issue", key,
				"step", gate.Step.OrSpec(), "ancestor", standing, "accepted", accepted)
			return nil, nil
		}
	}
	gate = record.EndPendingBasis(gate)
	var rests []string
	for _, ancestor := range gate.RestsOn {
		if ancestor != standing {
			accepted, _, err := record.BasisAcceptance(ctx, a.store, tx, ancestor)
			if err != nil {
				return nil, err
			}
			if accepted != "" {
				a.log.Info("admission carry: no rest on an accepted ancestor", "issue", key, "step", gate.Step.OrSpec(),
					"ancestor", ancestor, "accepted", accepted)
				continue
			}
		}
		rests = append(rests, ancestor)
	}
	gate.RestsOn = rests
	return &gate, nil
}

// releaseTreeSlots frees every slot an issue of tree holds and clears the mark of a release for a
// wait from every issue of it, at the re-admission of the tree's own root (readmit): no slot and
// no wait mark of the generation that ended outlives it. They are the rows releaseSlots frees and
// the marks it clears for a tree that lingers (record.NeedsSlot), and on a project that is not
// paused the pass of the backlog's own fact has freed them already, so this finds nothing. A
// paused project's pass releases nothing, though, and the re-admission ends the linger that
// release reads, so a tree moved out and back during a pause would otherwise come back holding
// its old slots: the line would read its root's as a promotion already made, the pass that clears
// the mark an earlier release's promotion left would take it for one and strand the members
// (promotedByEarlierRelease), and its members' would fill the cap against a tree that has not
// opened. So a slot on a marked root that re-admitted itself was granted after its re-admission.
// The root's own wait mark is gone already, since its leave cleared it (workflow's leave); its
// members' are not.
//
// Rows are freed here only under a pause, and they reach neither the pass's released list nor the
// controller's slot-free wake (wakeForFreeSlot): the project is still paused, so a wake would call
// the controller to fill capacity it cannot use, and the resume's notice tells it of the capacity
// instead. The resume's pass promotes the root into one of them at most, and into none while its
// tree's cleanup is reserved or a Dispatch blocker holds it, and its own release finds nothing, so
// no slot-free wake follows for these rows. A member arriving from another tree keeps the row it
// held there (Waiting's queued-root loop), so only the tree's own root coming back reaches this.
// Each release is logged with the slots the project holds after it, lowered in slot order, as
// releaseSlots logs its own; the count is read here, where every caller of logSlot counts, and
// only when something was released.
func (a *Admission) releaseTreeSlots(ctx context.Context, tx pgx.Tx, tree string) error {
	released, err := a.store.ReleaseTreeSlots(ctx, tx, tree)
	if err != nil || len(released) == 0 {
		return err
	}
	own, err := a.store.SlottedIssues(ctx, tx, a.project)
	if err != nil {
		return err
	}
	for i, key := range released {
		a.logSlot(ctx, "released", key, len(own)+len(released)-1-i)
	}
	return nil
}

// keepStaleApproval keeps, for the architect start of the root key re-admission is clearing, the
// person's approval a gate of its holds of a version its document has since moved past: the new
// generation's architect registers each step again on a cleared gate, and its start task
// (workflow.Engine.ParkedDecisionTask) tells it to request approval of the latest version. One
// approval is kept, read from the first required step whose approved version sits below its
// latest, because readmitted_approvals keeps one row for each root; it carries that step, so the
// task names the document the person reviewed rather than the spec of every deployment. The
// required steps are the ones the root's tree is gated on (record.TreeSteps), so a Verification
// document its architect registered as criteria beside a deployment that gates spec and not
// criteria is read after the spec, as every other rule of the gate reads it.
//
// The approval read is the person's own (record.DesignGate.ApprovedByPerson), never the version
// an ancestor's approval opened the step at (dispatch://INFRA-3320, R2 and R3): a step that stands
// open on a basis at its latest version keeps the person's approval of an earlier version apart,
// and that is the approval kept, so the re-admission neither loses it nor names the basis as a
// person's approval.
//
// Where the deployment gates no step, which asks no person, nothing is kept and any pair an
// earlier re-admission kept is dropped: no start names one there, so no delivery would forget
// it. Otherwise a root whose required steps hold no stale approval drops an earlier pair, which
// its gates have since outgrown; a root with a required step it holds no gate for (an earlier
// re-admission cleared it and nothing has registered since) keeps the earlier pair, which no
// start has delivered yet.
func (a *Admission) keepStaleApproval(ctx context.Context, tx pgx.Tx, key string) error {
	steps := a.engine.GatedSteps()
	if len(steps) == 0 {
		return a.store.DropReadmittedApproval(ctx, tx, key)
	}
	gates, err := a.store.GatesOf(ctx, tx, key)
	if err != nil {
		return fmt.Errorf("read the gate of re-admitted %s: %w", key, err)
	}
	unregistered := false
	for _, step := range record.TreeSteps(steps, gates) {
		gate, ok := gates[step]
		if !ok {
			unregistered = true
			continue
		}
		if approved := gate.ApprovedByPerson(); approved != nil && approved.Version < gate.LatestVersion {
			return a.store.KeepReadmittedApproval(ctx, tx, key, record.KeptApproval{
				Step: step, Approved: approved.Version, Latest: gate.LatestVersion})
		}
	}
	if unregistered {
		return nil
	}
	return a.store.DropReadmittedApproval(ctx, tx, key)
}

// releaseSlots releases the slot of every issue of this project that no longer needs one
// (record.NeedsSlot, the one statement of the release of an issue that needs no slot at all,
// whose phase half the grant reads too — record.KeepsSlot — asked of the pass's own rule through
// its facts: record.LineFacts.NeedsSlot) and reports those issues in slot order. Each release is
// logged once the fact commits. A tree leaving the workflow, a root finishing, an issue moving
// into accepting, retro or merging, an issue reaching its done, and a linger all read the same
// predicate, so one rule says when an issue stops needing a slot where it stands; under a rule
// that keeps a slot in merging and gives it back at the merger's ready mark
// (record.SlotRule.ReleaseAtReady), that mark is one more such moment (record.LineFacts.Ready).
// The daemon makes two other releases. releaseSlotsForWaits, judged by blockers.waitsOf rather
// than by record.NeedsSlot, frees the row of an issue that goes on working and waits on
// another issue; the two are apart because they free rows for opposite reasons — this one
// because the issue's phase holds no row, that one because the work is held — and only that one
// leaves a mark, since only its issue stands in the line for the row again. An issue in
// accepting, retro or merging is never one it frees (blockers.waitsOf says why). The
// re-admission of a tree's own root frees every row and mark of its tree (releaseTreeSlots): the
// ones this would have freed while the tree lingered, had a pause not held the project's passes.
//
// It reads the slotted issues in one query (record.Store.SlottedIssues), never a record per slot
// and never the whole issues table: every pass makes this read under the one fact lock
// (intake.ApplyFact), so a read per slot cost every pass of every consumer, the API and the
// boot's replay a round trip to the database for each slot of the shared table, and a read of
// every record would grow with every issue the project has ever recorded. The read leaves out
// another project's slot, whose own daemon releases it. The further facts the predicate needs of
// each slotted issue — its tree root, and whether that root's tree is live — come from issues
// and facts, which the pass read once for all of them, so this still makes exactly one query.
//
// It also clears the mark of a release for a wait from every issue that needs no slot at all
// (record.Issue.ReleasedForWaitAt, over issues rather than over the slotted rows): an issue
// released for a wait holds no row, so the loop below never sees it, and it goes on working, so
// it can reach its done, or be parked, with nothing else to clear the mark. Left standing, the
// mark would say the issue stands in the line waiting for its slot
// back when nothing of it is waiting, and its work coming back later would take its place in
// the line from the moment before the park (record.Waiting).
//
// Each release is recorded on moves with the term of the predicate that failed (releaseReason),
// for the pass's record line.
func (a *Admission) releaseSlots(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts record.LineFacts, moves *passMoves) ([]string, error) {
	own, err := a.store.SlottedIssues(ctx, tx, a.project)
	if err != nil {
		return nil, fmt.Errorf("list this project's slotted issues: %w", err)
	}
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	live := record.LiveRoots(issues, facts)
	inUse := len(own)
	var released []string
	for _, issue := range own {
		// A slotted issue whose root the live records do not hold belongs to a tree that has
		// finished: LiveRoots does not name it either, so the predicate releases it.
		_, running := live[issue.Tree]
		root := byKey[issue.Tree]
		if facts.NeedsSlot(issue, root, running) {
			continue
		}
		if err := a.store.ReleaseSlot(ctx, tx, issue.Key); err != nil {
			return nil, fmt.Errorf("release the slot of %s: %w", issue.Key, err)
		}
		inUse--
		released = append(released, issue.Key)
		a.logSlot(ctx, "released", issue.Key, inUse)
		moves.take(issue.Key, releaseReason(issue, root, running), inUse)
	}
	for _, issue := range issues {
		if issue.ReleasedForWaitAt == nil {
			continue
		}
		_, running := live[issue.Tree]
		if facts.NeedsSlot(issue, byKey[issue.Tree], running) {
			continue
		}
		if err := a.store.ClearReleasedForWait(ctx, tx, issue.Key); err != nil {
			return nil, fmt.Errorf("clear the slot release of %s for what it waits on: %w", issue.Key, err)
		}
	}
	return released, nil
}

// releaseSlotsForWaits takes back the row of every issue of this project that holds one and
// waits on another issue (record.LineFacts.Waits, the pass's own judgement by blockers.waitsOf),
// and reports the issues it took one from. It is the release for a wait, beside releaseSlots,
// which frees the row of an issue that needs none at all (record.NeedsSlot): an issue this one
// frees goes on working and keeps its place in the line, so it takes a row again by itself in
// that place once the promotion sees its wait end and a row is free (promoteHolds), which is why
// the two are written apart and why this one marks the issue.
// The cap counts the issues Legion is working on, and an issue waiting for another issue to
// finish is not one of them, so its row goes to the line and it stands there for a row of its
// own again. Under the daemon's own rule nothing else of it moves —
// no claim is suspended, no phase changes, no Dispatch status is written.
//
// Under a rule whose wait gives the slot back only after a grace (record.SlotRule.WaitGrace) the
// judgement names no holder whose wait is younger than the grace (record.LineFacts.InWaitGrace),
// so this frees none of them: such a holder keeps its row through the wait, the pass marks the
// wait's start beside it (markSlotWaits), and the first pass at or after that start plus the
// grace finds it waiting and frees its row here. Nothing asks for a pass at that moment: the
// next fact of the project or the next controller tick makes it, so the delay is the rule's own.
//
// A wait that stops the phase under way (record.SlotRule.WaitStopsPhase, the daemon's own since
// cybermonklabs/legion#4592) is no release here: a started parent an open Dispatch child holds
// moves into awaiting_children before the releases (childrenWaits), where its phase keeps no
// slot, so the ordinary release frees its row (releaseSlots) and it stands in no line, and a
// blocked_by link stops nothing under any rule the repository keeps.
//
// The release is one statement an issue (record.Store.ReleaseSlotForWait): the delete of the row
// and the mark that says why are written together, so no pass can read one without the other.
// The rows it frees are reported to the caller with the rows the ordinary release frees, because
// the controller's free-slot wake is about the capacity of the project and not about the reason
// a row came free. Each release is recorded on moves with what the issue waits on (waitReason),
// for the pass's record line.
func (a *Admission) releaseSlotsForWaits(ctx context.Context, tx pgx.Tx, issues []record.Issue,
	slots []record.Slot, facts record.LineFacts, moves *passMoves,
) ([]string, error) {
	own := ownSlots(issues, slots)
	inUse := len(own)
	var released []string
	for _, slot := range own {
		waits, waiting := facts.Waits[slot.Issue]
		if !waiting {
			continue
		}
		if err := a.store.ReleaseSlotForWait(ctx, tx, slot.Issue, a.now()); err != nil {
			return released, fmt.Errorf("release the slot of %s for what it waits on: %w", slot.Issue, err)
		}
		inUse--
		released = append(released, slot.Issue)
		a.logSlot(ctx, "released", slot.Issue, inUse)
		moves.take(slot.Issue, waitReason(slot.Issue, waits, facts), inUse)
	}
	return released, nil
}

// releaseIdleSlots takes back, under a rule with an idle return (record.SlotRule.IdleReturn), the
// row of every issue of this project that holds one, waits on its children or on a person, and
// none of whose roles has had a turn for that long (record.LineFacts.IdleReturns), and reports
// the issues it took one from. It reads the claims of the project's slot holders once
// (record.Store.HolderClaims, the narrow read the slots reading takes their turns from), and
// under every other rule, the daemon's own among them, it reads nothing and frees nothing.
//
// It is a release for a wait, written as releaseSlotsForWaits writes one
// (record.Store.ReleaseSlotForWait): the row goes and the mark that says why is written with it,
// so the issue keeps its place in the line by the moment of the release, ahead of every slot given
// back after it, and stands there passed over while it waits on its children, or out of the line
// until a person answers it (record.Waiting). An issue that pass already freed for a wait on
// another issue (record.LineFacts.Waits) is not judged again, and nothing else of the issue
// moves: no claim is stopped, no phase changes and no Dispatch status is written. Each release is
// recorded on moves as an idle return (record.TakenIdleReturn), for the pass's record line.
func (a *Admission) releaseIdleSlots(ctx context.Context, tx pgx.Tx, issues []record.Issue,
	slots []record.Slot, facts record.LineFacts, moves *passMoves,
) ([]string, error) {
	if facts.Rule.OrMain().IdleReturn <= 0 {
		return nil, nil
	}
	project, err := claim.ProjectToken(a.project)
	if err != nil {
		return nil, err
	}
	claims, err := a.store.HolderClaims(ctx, tx, project)
	if err != nil {
		return nil, fmt.Errorf("read the claims of %s's slot holders: %w", a.project, err)
	}
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	own := ownSlots(issues, slots)
	inUse := len(own)
	var released []string
	for _, slot := range own {
		if _, waits := facts.Waits[slot.Issue]; waits {
			inUse--
			continue
		}
		if !facts.IdleReturns(byKey[slot.Issue], slot, issues, claims) {
			continue
		}
		if err := a.store.ReleaseSlotForWait(ctx, tx, slot.Issue, a.now()); err != nil {
			return released, fmt.Errorf("release the idle slot of %s: %w", slot.Issue, err)
		}
		inUse--
		released = append(released, slot.Issue)
		a.logSlot(ctx, "released", slot.Issue, inUse)
		moves.take(slot.Issue, record.TakenIdleReturn, inUse)
	}
	return released, nil
}

// childrenWaits is the admission pass's reading of the started parents' wait on their open
// Dispatch children (record.LineFacts.StartedParentWaits, dispatch://INFRA-3320 R9 and R12), and
// of each parent held before its retro (record.HoldForChildren, R8), and answers whether it wrote
// anything, which tells the pass to take its shared reads again. It makes three moves and one
// write, each through the engine, and no other:
//
//   - a started parent an open child now holds moves into awaiting_children
//     (workflow.Engine.StopForChildren): a parent in accepting, one whose start is written and
//     not delivered, and one whose worker is in the middle of its phase, the three a transition
//     never meets, since no transition starts them;
//   - a parent in awaiting_children that an open child still holds keeps each child it finds
//     open under it on the wait's children (workflow.Engine.KeepWaitChildren), so a child filed
//     or released during the wait is one the return names if it merged;
//   - a parent in awaiting_children that no open child holds any longer goes back
//     (workflow.Engine.ReturnFromChildren), on this pass and with nobody acting;
//   - a parent held before its retro that nothing holds any longer — no issue beneath it in the
//     workflow, no root that rests on its approval in the workflow, no open Dispatch child — has
//     its retro started (workflow.Engine.StartHeldRetro), on this pass and with nobody acting.
//     The close of an open Dispatch child, or its move to icebox or out from under the parent,
//     changes the daemon's mirror of Dispatch alone, a child never released among them, so no
//     settle of the engine reads it and this pass is what releases the parent. A Dispatch child
//     filed under it, or a root registered on its approval, is a hold no settle reads either, and
//     this pass tells it; a parent whose facts show what the last pass that read it found is not
//     read again (readsHeldRetro).
//
// Each reads an issue of this project in the workflow whose tree runs (record.RunningRoots: its
// lifecycle open under the workflow, its root neither lingering nor waiting in the line as a
// queued root), so a lingering or unopened tree, or one whose root is on the backlog, keeps every
// member where it stands, a queued root is left alone (record.QueuedRoot), and a root handed over
// in awaiting_children comes back once its promotion has opened its tree. A key a hold still names
// (held, the pass's own predicate) is read on the first pass after the hold ends, as the promotion
// reads it. The engine leaves alone a parent held for any other reason, in retro or in merging,
// whose pull request has merged, or whose phase's role stands on an entry a role's move back raised
// out of retro or merging (record.PhaseRow.EntryFrom), and a second pass finds a moved parent in
// awaiting_children already, so it sends nothing again. The stop leaves a member alone when its
// tree's return starts the tree's members later in this pass (startsMembersThisPass): that return
// decides whether to hold a new start or stop a worker already started (startMidPhaseMembers). A
// tree whose root an earlier release's promotion left is not one, since its return clears the mark
// and starts nothing, so its members stop here as any started parent's does.
//
// It runs before the releases (promoteHolds), so the slot a stopped parent held is freed on this
// very pass by the one rule for a phase that keeps none (releaseSlots, record.NeedsSlot), and the
// start a returned parent owes stands in this pass's own line.
func (a *Admission) childrenWaits(ctx context.Context, tx pgx.Tx, issues []record.Issue, slots []record.Slot, facts record.LineFacts,
	held func(string) bool,
) (bool, error) {
	// The reads of the parents no longer held before their retro are dropped first, on every pass,
	// whatever it reads after.
	pass := a.forgetRetroHeldReads(facts.RetroHolds)
	// Each move reaches a parent in awaiting_children, one an open child holds, or one held before
	// its retro, and nothing else, and on most passes there is none: the rest is read for those
	// alone, in byte order of their keys (SQL's `order by key` may collate otherwise).
	var parents []record.Issue
	for _, issue := range issues {
		if _, open := facts.StartedParentWaits[issue.Key]; open || issue.Phase == phase.AwaitingChildren || record.HeldForChildren(issue) {
			parents = append(parents, issue)
		}
	}
	if len(parents) == 0 {
		return false, nil
	}
	slices.SortFunc(parents, func(left, right record.Issue) int { return cmp.Compare(left.Key, right.Key) })
	running := record.RunningRoots(issues, facts)
	owed := owedStartsByIssue(facts.OwedRows)
	// Let the return decide how to hold these members, but only if the pass will reach it and the
	// return will start them. A root's catch-up hold does not keep a member's own queued start from
	// being delivered. These slots are read before the pass releases any, and the return reads them
	// after releaseSlots and releaseSlotsForWaits. For every root whose return runs
	// (returnsMembersThisPass) both reads still give one answer of promotedByEarlierRelease, the one
	// term of startsMembersThisPass that reads the slots: nothing between them grants a slot;
	// releaseSlots takes a root's slot, or clears its record.Issue.ReleasedForWaitAt, only where the
	// root needs none (record.NeedsSlot), which for a root whose tree runs is a phase past admitted,
	// where the term is true either way; and releaseSlotsForWaits writes that mark in the statement
	// that takes the slot, and the return reads the mark again.
	slotted := slottedIssues(slots)
	returning := make(map[string]bool)
	for _, root := range issues {
		if startsMembersThisPass(root, facts, held, slotted) {
			returning[root.Key] = true
		}
	}
	wrote := false
	for _, parent := range parents {
		if parent.Project != a.project || !record.InWorkflow(parent) || held(parent.Key) {
			continue
		}
		if _, runs := running[parent.Tree]; !runs {
			continue
		}
		wait, open := facts.StartedParentWaits[parent.Key]
		switch {
		case record.HeldForChildren(parent):
			read := retroHeldRead{moved: parent.MovedAt, holds: facts.RetroHolds[parent.Key]}
			if !a.readsHeldRetro(parent.Key, read) {
				continue
			}
			released, err := a.engine.StartHeldRetro(ctx, tx, parent)
			if err != nil {
				return wrote, fmt.Errorf("start the held retro of %s: %w", parent.Key, err)
			}
			if !released {
				a.rememberHeldRetro(ctx, pass, parent.Key, read)
			}
			wrote = wrote || released
		case parent.Phase == phase.AwaitingChildren && !open:
			if err := a.engine.ReturnFromChildren(ctx, tx, parent); err != nil {
				return wrote, fmt.Errorf("take %s back from awaiting_children to the step its return takes: %w", parent.Key, err)
			}
			wrote = true
		case open && parent.Phase == phase.AwaitingChildren:
			kept, err := a.engine.KeepWaitChildren(ctx, tx, parent, wait.Open)
			if err != nil {
				return wrote, fmt.Errorf("keep the open children of %s's wait: %w", parent.Key, err)
			}
			wrote = wrote || kept
		case open && !returning[parent.Tree]:
			moved, err := a.engine.StopForChildren(ctx, tx, parent, wait.Open, owed[parent.Key])
			if err != nil {
				return wrote, fmt.Errorf("move %s to awaiting_children: %w", parent.Key, err)
			}
			wrote = wrote || moved
		}
	}
	return wrote, nil
}

func owedStartsByIssue(rows []record.OwedStart) map[string][]record.OwedStart {
	owed := make(map[string][]record.OwedStart)
	for _, start := range rows {
		owed[start.Issue] = append(owed[start.Issue], start)
	}
	return owed
}

// retroHeldRead is what one pass's facts show of a parent held before its retro: the movement of
// its record (record.Issue.MovedAt), which the move that enters its hold and the release that ends
// it each write, and the three holds on its acceptance (record.RetroHolds).
type retroHeldRead struct {
	moved time.Time
	holds record.RetroHolds
}

// same says two reads show one movement of the parent's record and the same issues in each hold.
func (r retroHeldRead) same(other retroHeldRead) bool {
	return r.moved.Equal(other.moved) && slices.Equal(r.holds.Beneath, other.holds.Beneath) &&
		slices.Equal(r.holds.Resting, other.holds.Resting) && slices.Equal(r.holds.Open, other.holds.Open)
}

// readsHeldRetro says whether the pass reads the release of key, a parent held before its retro
// whose facts are read (workflow.Engine.StartHeldRetro). It reads one whose facts show nothing
// holding it, since its retro may start. It reads one a hold holds where its facts differ from
// those of the last pass that read it, committed, and whose hook ran before a later pass began
// (retroHeldReads, rememberHeldRetro), or where no pass of this run has: a hold no settle of its
// tree reads, a Dispatch child filed or reopened under it or a root registered on its approval,
// shows as such a change, and its notice may not have been told yet. It does not read one whose
// facts are the ones that read found: that read told the first hold they show where no notice of
// the run had named it (workflow's tellRetroHeld), so a read now has nothing new to tell, and a
// hold still holds it, so no release can start its retro. The movement keeps the run apart: the
// move that enters a hold and the release that ends it each write it, so a parent held again is
// read again. A change beneath it is a write of its tree, whose settle reads it already
// (workflow's releaseHeldParents).
func (a *Admission) readsHeldRetro(key string, read retroHeldRead) bool {
	if read.holds.None() {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	last, seen := a.retroHeldReads[key]
	return !seen || !last.same(read)
}

// forgetRetroHeldReads begins a pass's reading of the parents held before their retro, held
// (record.LineFacts.RetroHolds): it drops the read of every parent not among them — one a settle
// released into its retro, or one that left the workflow — and answers the number of this pass
// (retroHeldPass), which fences the hooks of every pass before it (rememberHeldRetro). A drop that
// a rolled-back pass made costs that parent one more read, which tells nothing twice.
func (a *Admission) forgetRetroHeldReads(held map[string]record.RetroHolds) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	maps.DeleteFunc(a.retroHeldReads, func(key string, _ retroHeldRead) bool {
		_, still := held[key]
		return !still
	})
	a.retroHeldPass++
	return a.retroHeldPass
}

// rememberHeldRetro writes read, a read of key's release that left key held, as what the last read
// of it found, once the pass commits: a pass rolled back told nothing, so its facts stay unread.
// The write stands behind a fence a late run cannot cross (store.OnCommit): pass is the number of
// the pass that read key (forgetRetroHeldReads), and the hook writes only while no later pass has
// begun, so a hook the commit watcher runs after a later pass writes nothing over what that pass
// read. What it leaves unwritten costs key one more read, which tells nothing twice.
func (a *Admission) rememberHeldRetro(ctx context.Context, pass uint64, key string, read retroHeldRead) {
	store.OnCommit(ctx, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.retroHeldPass == pass {
			a.retroHeldReads[key] = read
		}
	})
}

// logSlot logs a slot released or taken once the fact commits: the issue, the slots the project
// holds after the change, and the cap.
func (a *Admission) logSlot(ctx context.Context, change, issue string, inUse int) {
	store.OnCommit(ctx, func() { a.log.Info("admission: slot "+change, "issue", issue, "slots", inUse, "cap", a.cap) })
}

// logOnCommit writes one Info line once the transaction commits (store.OnCommit), for a line that
// says what the transaction wrote: a pass rolled back writes none, and one retried after a failed
// commit writes it once.
func (a *Admission) logOnCommit(ctx context.Context, msg string, args ...any) {
	store.OnCommit(ctx, func() { a.log.Info(msg, args...) })
}

// promote gives each issue of the waiting line what its promotion needs — a slot, a tree opened,
// a first phase started, an architect resumed — in the line's own order, until the cap is full
// or the line empties, and reports how many candidates it promoted and the slots it released.
// Before it promotes any, every admitted root that now waits only on a person enters that wait
// (workflow's ReconcileAwaiting), every issue that no longer needs its slot gives it back
// (releaseSlots), and every root left in the workflow that nothing will ever start leaves it
// (releaseUnslotted), so each pass of admission — each the pass rule (Apply) runs, and the boot
// read's — repairs that state rather than keeping it. The pass stops at the first candidate
// either hold holds: one a.pending still names, since the Dispatch consumer has not yet reached
// the stream position Reconcile captured when it deferred that key, or one the boot's catch-up
// still holds (catchingUpFor). It stops rather than passing over it because the cap counts
// issues, so every candidate behind the held one competes for the same slots, and a slot is
// taken back only from an issue that waits on another issue (releaseSlotsForWaits), never to
// restore rank order: a skip would invert Dispatch's rank order for good. A pass after the hold's
// release fills the free slots in line order. release, not promote, is what clears pending — it
// empties the whole set at once, so this need only check membership. An admitted root's
// architect start is queued behind a row creating its issue branch (record.IssueBranch), which
// the start waits for.
func (a *Admission) promote(ctx context.Context, tx pgx.Tx) (int, []string, error) {
	// The pass of an event: every fact but the controller's tick and a signal's deadline, which
	// run their own (applyFact).
	return a.promoteHolds(ctx, tx, true, signals.PassEvent)
}

// promoteHolds is promote; respectHolds false is release's own call, once every held key's own
// summary has already been applied: release always ignores every hold, never a subset, since it
// only ever runs once the consumer has caught up to every key Reconcile deferred. The in-memory
// pending set itself only clears after that whole call's transaction commits (see release), so
// without this, the pass's own membership check would hold every one of them back in the very
// call that released them. A queued root a live tree took in (adoptQueuedRoots) leaves the line
// before anything else reads it.
//
// It makes one read of each kind for the whole pass and shares it: one read of the project's
// pause, which also gives the floor the pass's stall readings count from (readStallFloor), one
// record.Store.LiveIssues, one record.Store.Slots, one bulk tree-lifecycle read, one design-gate
// read, one read of the starts the project owes its workers (the three of record.ReadLineFacts)
// and one read of the Dispatch mirror rows of everything it judges (blockers.Judged,
// blockers.Load). The release, the waiting line, the INFRA-129 safety net, the promotion and the
// two repair passes all judge from those, so no path reads per slot, per tree or per issue under
// the one fact lock every fact takes (intake.ApplyFact). A read is made again only where this
// pass has itself written something that would make it stale (reread).
//
// Every path that promotes comes through here — each pass the pass rule (Apply) runs, and the
// boot's Reconcile — so each of them is also a look at every issue a blocker holds, at every slot
// whose worker has lost its task, and at every tree a step that left gates.design released
// (record.Waiting's LineFirstPhase).
//
// It is also every path that moves one of the cap's slots, so it writes the record of the pass
// (passMoves): each slot the releases take back and the promotion gives out, and each candidate
// the promotion passes over, in one line once the fact commits where any slot moved
// (logPassMoves), whichever return ends the pass. A pass that ends with no error hands what it
// read, judged and moved to the admission's observer once its fact commits (observePass), which
// only a replay registers.
func (a *Admission) promoteHolds(ctx context.Context, tx pgx.Tx, respectHolds bool, kind signals.PassKind) (int, []string, error) {
	var moves passMoves
	defer a.logPassMoves(ctx, &moves)
	// A paused project promotes nothing (decision 11 of dispatch://INFRA-3759): no slot, no owed
	// entry, no stalled-tree notice and no children-first notice. The tick's wake of the
	// controller is the caller's and still runs, and the resume applies one pass of its own. The
	// read that answers whether the project is paused also gives the floor of the pass's stall
	// readings, which is handed to the stalled-tree notice and to the architect-stalled arms
	// (tellStalledTrees, resumeSlottedWorkers), so the pass reads the pause once.
	floor, paused, err := a.readStallFloor(ctx, tx)
	if err != nil || paused {
		return 0, nil, err
	}
	// The pass is taken past the pause, so a pause's skipped pass leaves a catch-up release owed.
	a.passCatchUpReleases(ctx)
	// The P0 bound and the signals are read once for the pass, beside the pause (gateFor), and the
	// stall measure and the promotion below judge under them (capHold).
	gate, err := a.gateFor(ctx, tx, kind)
	if err != nil {
		return 0, nil, err
	}
	p0Bound := gate.p0Bound
	issues, slots, facts, err := a.adoptQueuedRoots(ctx, tx, respectHolds)
	if err != nil {
		return 0, nil, err
	}
	heldKeys := a.catchingUpFor
	if respectHolds {
		heldKeys = func(key string) bool { return a.held(key) || a.catchingUpFor(key) }
	}
	_, changed, err := a.engine.ReconcileAwaiting(ctx, tx, a.project, issues, heldKeys)
	if err != nil {
		return 0, nil, fmt.Errorf("reconcile the roots waiting on a person: %w", err)
	}
	// Reconcile wrote roots of the snapshot (a wait entered or answered, a resume told in place),
	// so what follows reads them as they now stand: an answer it recorded puts its root in this
	// pass's waiting line, within its priority, among the issues whose work came back, oldest
	// first (record.Waiting), at the moment its own wait was answered.
	if changed {
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return 0, nil, err
		}
	}
	// A started parent an open Dispatch child holds stops, one whose last open child has gone comes
	// back as the step its return takes (record.ReturnStep: the start it held, or for a held
	// testing, reviewing or accepting the first step of its walk after the implementer's that stands
	// at or before that start), and one held before its retro that nothing holds any longer starts
	// its retro (childrenWaits). Each is written before the releases and read again where any was,
	// so a stopped parent's slot is freed on this pass by the release below (awaiting_children
	// keeps none), and a returned parent's start stands in this pass's line among the issues whose
	// work came back, at the moment its wait ended.
	waited, err := a.childrenWaits(ctx, tx, issues, slots, facts, heldKeys)
	if err != nil {
		return 0, nil, err
	}
	if waited {
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return 0, nil, err
		}
	}
	released, err := a.releaseSlots(ctx, tx, issues, facts, &moves)
	if err != nil {
		return 0, nil, err
	}
	if len(released) > 0 {
		if slots, err = a.store.Slots(ctx, tx); err != nil {
			return 0, released, fmt.Errorf("list admission slots: %w", err)
		}
	}
	// The rows the fence repair ends are decided before the line is judged, so an issue whose
	// owed start it ends is offered no slot on this pass and the slot goes to the issue behind
	// it (finishFencedStarts, record.Waiting). The records are read again where it ended one,
	// as they are after releaseUnslotted below, since the line is built from the starts the
	// project owes and this pass has just taken some of them away.
	fenced, err := a.finishFencedStarts(ctx, tx, issues, facts)
	if err != nil {
		return 0, released, err
	}
	if fenced {
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return 0, released, err
		}
	}
	// The rows the line left behind come back beside it (record.Waiting's second answer, through
	// blockers.JudgeLine): each with the one word for why (record.HeldReason), written by the walk
	// that built the line, so the stalled-tree measure below reads those words rather than
	// walking the project's issues again.
	rows, facts, line, dropped, err := blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, nil, a.now())
	if err != nil {
		return 0, released, err
	}
	// The issues this judgement finds waiting give their rows back before the line is promoted,
	// so this pass's own line and its ownSlots read the state after the release: an issue that
	// waits holds none of the cap's slots, and the slot it gives back is offered to the line on
	// this very pass rather than the next one.
	waiting, err := a.releaseSlotsForWaits(ctx, tx, issues, slots, facts, &moves)
	if err != nil {
		return 0, append(released, waiting...), err
	}
	released = append(released, waiting...)
	// Under a rule with an idle return, each holder that waits on its children or on a person and
	// none of whose roles had a turn for that long gives its row back here too, beside the release
	// for a wait and on the same judgement (releaseIdleSlots); under every other rule nothing is
	// read for it.
	idle, err := a.releaseIdleSlots(ctx, tx, issues, slots, facts, &moves)
	if err != nil {
		return 0, append(released, idle...), err
	}
	released, waiting = append(released, idle...), append(waiting, idle...)
	unslotted, err := a.releaseUnslotted(ctx, tx, issues, record.LiveRoots(issues, facts), facts.ChildWaits, respectHolds)
	if err != nil {
		return 0, released, err
	}
	// One re-read and one second judgement answer for both releases: each took rows away, and
	// what the promotion loop and ownSlots must read is the state after the last of them.
	if len(waiting) > 0 || unslotted {
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return 0, released, err
		}
		if rows, facts, line, dropped, err = blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, rows, a.now()); err != nil {
			return 0, released, err
		}
	}
	// A re-admitted tree that runs before its root's promotion from the waiting line — its architect
	// registered a design a person had already approved, so the gate opened and the root stopped
	// being a queued root — will see no promotion, and so no return of its members from one. They
	// come back here instead, after the releases, so that a root the releases took out of the
	// workflow is not read as running, and before the line is promoted, so that a member's owed
	// start stands in this pass's own line (returnMembersOfRunningTrees, dispatch://INFRA-5471).
	wrote, err := a.returnMembersOfRunningTrees(ctx, tx, issues, slots, facts, heldKeys)
	if err != nil {
		return 0, released, err
	}
	if wrote {
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return 0, released, err
		}
		if rows, facts, line, dropped, err = blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, rows, a.now()); err != nil {
			return 0, released, err
		}
	}
	// Under a rule whose start may take a cover, each cover that ended gives its waiter's slot
	// back and each waiter a started issue can hold the slot of gives it to that issue, after the
	// releases and the members' return and before the line is promoted, and the records are read
	// again where a slot or a cover moved (settleCovers). A waiter it covered freed a slot the cap
	// no longer counts.
	covered, freed, err := a.settleCovers(ctx, tx, issues, slots, rows, facts, &moves)
	if err != nil {
		return 0, released, err
	}
	released = append(released, freed...)
	if covered {
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return 0, released, err
		}
		if rows, facts, line, dropped, err = blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, rows, a.now()); err != nil {
			return 0, released, err
		}
	}
	// Each holder that waits on another issue while it keeps its slot has the moment its wait
	// began written beside its slot, which the slots reading dates the holder state `issue` by,
	// and the mark goes once its waits end (markSlotWaits). It is judged here, after the releases
	// and the members' return, from the rows and facts of the judgement the line was built from,
	// so a holder the release just freed is marked nothing.
	if err := a.markSlotWaits(ctx, tx, issues, slots, rows, facts); err != nil {
		return 0, released, err
	}
	if err := a.tellChildrenFirst(ctx, tx, issues, facts); err != nil {
		return 0, released, err
	}
	// Every tree of this project that has stopped is told to the controller here, after the
	// releases and the line's judgement: the rule reads the facts this pass already holds —
	// what a blocker holds, what waits for its children, and the slot rows the line was judged
	// from, which name every holder of the cap and not only the holders standing in the line —
	// and no pass may read them twice. Why the daemon starts no issue of the project now is
	// handed over with them, from the walk that built the line this pass judged above: the line
	// itself and the rows it left behind with their words (record.Waiting through
	// blockers.JudgeLine), so the measure and the line are one reading of one project, they
	// cannot disagree about what holds an issue, and no pass walks the project's issues a second
	// time to say it. The slots are what the rule's liveness clause is read from: a project is
	// stopped for good when no slot holder of it can finish, no agent of it is at work and no
	// free slot is left for an entry of the line that the promotion below would start, and a
	// holder in a phase of its own stands in no line (lineCanMove, which reads the mirror rows
	// for that last term as the promotion reads them). They are this pass's own
	// rows, read again after every release it made, so the rule judges the slots the project
	// holds now. It runs before the cap check below, because a deployment whose cap admits
	// nothing still runs the trees it opened before and still owes its controller the news that
	// one has stopped.
	if err := a.tellStalledTrees(ctx, tx, issues, facts, slots, line, dropped, rows, floor, gate); err != nil {
		return 0, released, err
	}
	if a.cap <= 0 {
		a.observePass(ctx, issues, slots, facts, rows, line, dropped, &moves, nil, kind)
		return 0, released, nil
	}
	admitted := 0
	// promoted is each candidate the current round of the loop promoted, which the line the
	// observer is handed leaves out (observePass).
	var promoted []string
	for {
		own := ownSlots(issues, slots)
		// The holders as the cap and the P0 bound count them under the pass's rule
		// (record.SlotRule.HoldersOf), which grow with each grant below: the holders below P0,
		// which the cap counts, and under a rule that starts a P0 outside the cap the P0 holders,
		// which the bound counts and the cap does not.
		rule := facts.Rule.OrMain()
		held := rule.HoldersOf(issues, own)
		// The covers of this round under a rule whose start may take a cover: the slots its
		// counted waiters offer to what they wait on (covers), nothing under every other rule,
		// the daemon's own among them.
		lending := coversOf(issues, own, rows, facts)
		stopped, opened, resumed := false, false, false
		moves.round()
		promoted = promoted[:0]
		for at, candidate := range line {
			issue := candidate.Issue
			// The candidate's place in this round's line, counted from 1, which the pass's record
			// names for each candidate it passes over.
			place := at + 1
			// A hold stops the whole pass, the candidates behind this one included. Every
			// candidate competes for the same slots now that the cap counts the project's
			// issues rather than its trees, so passing one over would invert Dispatch's rank
			// order for all of them and not only inside the held candidate's own tree — and a
			// slot is taken back only from an issue that waits on another issue
			// (releaseSlotsForWaits), never to restore rank order, so that inversion would
			// be permanent. The design is
			// that the daemon gives the first issue of the line its slot; skipping the first
			// issue is not that. The cost is bounded: a hold ends when the Dispatch consumer
			// reaches the position Reconcile captured for it, and the next pass (the pass rule,
			// Apply) promotes again, so a hold delays a pass rather than holding a state.
			// heldKeys is the one predicate, the same closure ReconcileAwaiting was given above:
			// with respectHolds false — release's own call — it is the boot catch-up's hold
			// alone, so a release bypasses the Dispatch consumer's hold and never the catch-up's.
			if heldKeys(issue.Key) {
				moves.pass(candidate, place, record.PassedHeldKey)
				stopped = true
				break
			}
			// A candidate that waits on an unfinished Dispatch issue is passed over and keeps
			// its place: the next pass promotes it once the last issue it waits on is done.
			// That is a decided skip rather than a hold — the pass knows enough to decide it —
			// so the line moves on. The rule is read per kind, as the state's own `blocked`
			// reads it (blockers.LineWaitsOn), so no issue is reported waiting on a link this
			// loop would start anyway.
			if waits := blockers.LineWaitsOn(rows, candidate); len(waits) > 0 {
				a.log.Info("admission waits for what this issue waits on", "issue", issue.Key, "blockers", waits)
				moves.pass(candidate, place, record.PassedWaits)
				continue
			}
			// A candidate that already holds a row, or that stands in a phase keeping none,
			// takes no slot and so is never held by the cap: a root another tree handed to
			// Legion carrying the phase it ran there opens its tree at once on the row it
			// brought, and an answered root resumes its architect on the row it kept through
			// the wait. The phases are the pass's rule's (slotFor, over its facts), and so is
			// what the cap lets through (capLets, with the slots its holders lend).
			takes := slotFor(candidate, facts)
			if reason := a.capHold(candidate, held, facts, lending, gate); reason != "" {
				// A full cap gives no further slot, and the scan goes on. This is not the hold
				// above and must not be read as one: a hold is an answer for every candidate
				// behind it, because it says the pass does not yet know what this candidate is;
				// the cap is an answer for this candidate alone, because it says only that there
				// is no slot for it. The count only grows inside this loop, so no candidate
				// behind this one that the cap counts takes a slot either and the rank order of
				// the issues that need one is kept whole, while the candidates the cap cannot
				// bind — the ones slotFor is false for, and under a rule that starts a P0 outside
				// the cap an issue whose own priority is P0 — are promoted as they are reached,
				// which is what makes a hand-over and an answered root resume under a cap that is
				// permanently full.
				//
				// An answered root this branch passes over is told nothing. It reads where it
				// stands from its own record — `waitingLine`, which is the live place
				// (api.WaitingLineView) — and the daemon resumes it by itself on the pass that
				// finds a slot free, so a message would carry a snapshot of a place that moves
				// with every pass and nothing besides.
				//
				// An issue that starts outside the cap is held here by the P0 bound alone, never
				// by the cap: the bound is set and as many such issues as it allows hold a slot.
				// It waits in the line where its priority places it, and the pass says why, in
				// the words and keys of cybermonklabs/legion#5488's line, so the swap of the seam
				// at its merge leaves this line as it stands.
				if takes && rule.StartsOutsideCap(issue) {
					a.logOnCommit(ctx, P0BoundRefusalLine, "issue", issue.Key, "p0_holders", held.P0, "p0_bound", boundWords(p0Bound),
						"other_holders", held.Capped, "cap", a.cap)
				}
				moves.pass(candidate, place, reason)
				continue
			}
			if candidate.Kind == record.LineAnswered {
				// The root's tree already runs: only a reason it may not resume in its own
				// session holds it here (its label gone, a foreign Dispatch claim), which
				// releaseUnslotted takes out of the workflow on this same pass. Its links hold
				// nothing here: a root waiting on a person keeps its row through a wait
				// (blockers.waitsOf).
				reason, err := workflow.Unresumable(ctx, a.store, tx, a.project, issue)
				if err != nil {
					return admitted, released, fmt.Errorf("check whether answered root %s can resume: %w", issue.Key, err)
				}
				if reason != "" {
					a.log.Warn("admission skips an answered root it may not resume", "issue", issue.Key, "reason", reason)
					moves.pass(candidate, place, record.PassedNotResumable)
					continue
				}
			}
			if candidate.Kind == record.LineQueuedRoot {
				// Claims, tree lifecycles and the runtime key a tree by the normalized project
				// token, not the Dispatch project key the issue record carries.
				project, err := claim.ProjectToken(a.project)
				if err != nil {
					return admitted, released, fmt.Errorf("open admission lifecycle of %s: %w", issue.Key, err)
				}
				lifecycle, err := a.store.OpenTreeLifecycle(ctx, tx, project, issue.Tree, treelifecycle.AuthorityWorkflow)
				if err != nil {
					if errors.Is(err, treelifecycle.ErrCleanupReserved) {
						a.log.Info("admission waits for tree cleanup", "issue", issue.Key, "tree", issue.Tree)
						moves.pass(candidate, place, record.PassedTreeCleanup)
						continue
					}
					// The operator opened a tree under this root's key: this root waits until
					// that tree's cleanup confirms, and the candidates behind it go on.
					if errors.Is(err, treelifecycle.ErrAuthorityHeld) {
						a.log.Warn("admission waits for its tree's other authority", "issue", issue.Key, "tree", issue.Tree, "error", err)
						moves.pass(candidate, place, record.PassedTreeCleanup)
						continue
					}
					return admitted, released, fmt.Errorf("open admission lifecycle of %s: %w", issue.Key, err)
				}
				// The tree's row was created by this call, so this is the one moment the tree-unit
				// experiments the project declares are drawn for it, in the fact's own
				// transaction: a crash before the commit leaves neither the lifecycle nor the
				// draws. Every later open of the tree — its next epoch after a cleanup, and every
				// re-admission — creates nothing and draws nothing, and each answer is itself
				// written once (record.Store.PutAssignments). The answers are recorded against
				// the tree's root issue key, which is what a tree's unit is read by.
				if lifecycle.Created {
					if err := a.drawExperiments(ctx, tx, issue.Tree, config.ExperimentTree); err != nil {
						return admitted, released, err
					}
				}
			}
			now := a.now()
			if takes {
				// A candidate a counted waiter waits on starts on that waiter's slot where one
				// is offered, whichever half of the cap let it through (covers.take): the waiter
				// gives its slot up first, so the candidate takes none of the cap's own, and the
				// covers that stand are kept with the new one. Under every other rule nothing is
				// offered.
				if waiter, covering := lending.take(issue.Key); covering {
					var err error
					if slots, _, err = a.cover(ctx, tx, waiter, issue.Key, slots, len(own), facts, &moves); err != nil {
						return admitted, released, err
					}
					own = slices.DeleteFunc(own, func(slot record.Slot) bool { return slot.Issue == waiter })
					// The waiter holds no slot now, so the holders are counted again without it
					// (record.SlotRule.HoldersOf), which reads no record under a rule that starts
					// no P0 outside the cap.
					held = rule.HoldersOf(issues, own)
					if err := a.keepCovers(ctx, tx, lending.held); err != nil {
						return admitted, released, err
					}
				}
				// An entry carries whether its issue held a row when the line was built
				// (record.LineEntry.Holds), and the line this loop reads holds each issue once,
				// so no grant here changes the answer slotFor gives a candidate behind it; what
				// the grant does change, the count against the cap and the P0 bound, is own and
				// held, which grow with it. finishFencedStarts keeps the line to one entry an
				// issue, not record.Waiting: Waiting would list an issue at admitted that holds no
				// row and still owes a start both as a return and as a first phase, but every owed
				// start names a worker phase, and finishFencedStarts ends each one whose phase is
				// not the issue's own before the line is built, so none is left owed at admitted.
				slot := record.Slot{Issue: issue.Key, Index: nextSlotIndex(slots), AdmittedAt: now}
				if err := a.store.PutSlot(ctx, tx, slot); err != nil {
					return admitted, released, fmt.Errorf("put admission slot for %s: %w", issue.Key, err)
				}
				// The mark of a release for a wait goes with the row's return: an issue holding
				// a row has its place against the cap back, and a mark left standing would say
				// it gave that place up (record.Issue.ReleasedForWaitAt). Only an issue that
				// carries the mark is written, so an ordinary grant costs no statement.
				if issue.ReleasedForWaitAt != nil {
					if err := a.store.ClearReleasedForWait(ctx, tx, issue.Key); err != nil {
						return admitted, released, fmt.Errorf("clear the slot release of %s for what it waits on: %w", issue.Key, err)
					}
				}
				slots, own = append(slots, slot), append(own, slot)
				rule.AddHolder(&held, issue)
				a.logSlot(ctx, "taken", issue.Key, len(own))
				moves.give(issue.Key, candidate.Kind, len(own))
				// A start outside the cap is the one start the cap does not count, so it says so
				// with what the cap and the bound count, in the words and keys of
				// cybermonklabs/legion#5488's line: the P0 holders, this one among them, the other
				// holders, the cap and the bound.
				if rule.StartsOutsideCap(issue) {
					a.logOnCommit(ctx, P0StartLine, "issue", issue.Key, "p0_holders", held.P0, "other_holders", held.Capped,
						"cap", a.cap, "p0_bound", boundWords(p0Bound))
				}
			}
			switch candidate.Kind {
			case record.LineReturning:
				// The transition that sent this issue's work back wrote its worker's task in its
				// own transaction and left it unclaimable until a slot row of this issue existed
				// (record.SuperviseRequest.NeedsSlot). The slot above is the whole promotion:
				// the row is claimable the moment it commits, and the grant wakes the issue's
				// lifecycle lane (the daemon's projectRecords.PutSlot), so the task is delivered
				// in the same second rather than at the next sweep.
				a.logOnCommit(ctx, "admission gives a slot back to an issue whose work came back", "issue", issue.Key, "phase", issue.Phase)
			case record.LineWaitingOnIssue:
				// The wait is over — a candidate still waiting never reaches here, the skip
				// above passes over it — and the row granted above is the whole of this
				// promotion: nothing of the issue's work ever stopped, so there is nothing to
				// start again. What its phase needs next, a first phase at admitted among it,
				// the line offers from the record this grant leaves, the mark cleared, which is
				// why this round is read again below (record.Waiting).
				a.logOnCommit(ctx, "admission gives a slot back to an issue whose wait ended", "issue", issue.Key, "phase", issue.Phase)
				resumed = true
			case record.LineAnswered:
				awaiting := issue.Awaiting
				// An answered root keeps the status it ran with, so its resume writes none,
				// unless a person set it back to todo while it waited: then, like a fresh root,
				// it is written in_progress.
				writeInProgress := issue.Status == "todo"
				issue.Phase, issue.Awaiting = phase.Admitted, nil
				if writeInProgress {
					issue.Status = "in_progress"
				}
				issue.Ancestry = nil
				if err := a.engine.WriteIssue(ctx, tx, issue); err != nil {
					return admitted, released, fmt.Errorf("record admitted issue %s: %w", issue.Key, err)
				}
				if writeInProgress {
					if err := a.enqueue(ctx, tx, issue.Key, record.OrdinaryStatusWrite("in_progress", "todo", ""), now); err != nil {
						return admitted, released, err
					}
				}
				// ResumeAwaitedRoot is the resume's handout (record.Store.StartHandout), and it
				// is where this promotion's movement is marked: the resume starts the root's
				// architect, and the mark sits at that write so that the other resume carries
				// it too — the root whose person had already acted, which workflow tells where
				// it stands (its tellInPlace). The reasoning is in ResumeAwaitedRoot.
				if err := a.engine.ResumeAwaitedRoot(ctx, tx, issue, *awaiting); err != nil {
					return admitted, released, fmt.Errorf("resume %s after a person's answer: %w", issue.Key, err)
				}
				opened = opened || facts.Gates[issue.Tree].Open()
			case record.LineQueuedRoot:
				// LineQueuedRoot is the QueuedRoot judgement. k12's exemption is read here whole,
				// before the write below clears the hand-over mark it reads: a root handed over
				// from another tree carrying its slot in implementing starts its carried phase
				// whatever open children hold it (startMidPhaseMembers, workflow's
				// StartPromotedWorker).
				carriedSlot := candidate.Holds && issue.HandedOverFrom != "" && issue.Phase == phase.Implementing
				issue.Status = "in_progress"
				// A root whose tree is opened runs as a tree of its own, which no ancestry hold
				// holds (workflow's Ancestry), so the chain it queued under goes now: a root
				// coming back to the line later is never held by a chain left from before it ran.
				issue.Ancestry = nil
				// The hand-over mark goes with it, for the same reason and with the same reach:
				// it says a root arrived from another tree and has still to open a tree of its
				// own, and this is the write that opens it. Left standing it would make the
				// record say for ever that this generation never ran (record.QueuedRoot, ran),
				// so a person's todo on the running root would queue it again and promote it a
				// second time, starting its merger over a merge that has already been made. The
				// row the root holds would not stop that: the line passes over a queued root
				// holding one only where the mark is gone (record.Waiting). Nothing else reads
				// the mark after the tree is open: a root with an opened lifecycle is no tree's
				// to adopt whatever the mark says (workflow's RanAsRoot), and its own record
				// says it is a root (record.ChildOpen). The architect's first task below does not
				// read the mark either: whether a hand-over carried the root in at its phase is the
				// hand-over row of its generation, which the re-admission that carried it wrote
				// (workflow.ArchitectStartOf says why the two agree here).
				issue.HandedOverFrom = ""
				if err := a.engine.WriteIssue(ctx, tx, issue); err != nil {
					return admitted, released, fmt.Errorf("record admitted issue %s: %w", issue.Key, err)
				}
				// The promotion opens the tree's lifecycle, writes the root in_progress and
				// queues its architect's start below: a phase of the tree's life the record did
				// not carry before, and a start, which the rule counts as movements
				// (record.Store.MarkIssueMoved, the rule in workflow's dispatchIssue). The
				// architect start writes none of its own — it is queued directly
				// (workflow.Engine.StartArchitect), which waits for no slot and so never reaches
				// workflow.StartOrOwe, where a phase worker's start marks one — so without this
				// the root would keep the moved_at of the moment a person labelled it, however
				// many hours it then waited in the line. The next pass reads that stamp:
				// record.RunningRoots takes the root in from here on, no claim of it is at work
				// yet because the outbox has still to build its pod, and a root at admitted
				// with no open child under it is no wait the daemon makes by design. So that
				// next pass would tell the controller that a tree the daemon started a moment
				// ago has stopped.
				if err := a.store.MarkIssueMoved(ctx, tx, issue.Key, now); err != nil {
					return admitted, released, err
				}
				if err := a.enqueue(ctx, tx, issue.Key, record.OrdinaryStatusWrite("in_progress", "todo", ""), now); err != nil {
					return admitted, released, err
				}
				// A fresh admission hands the architect its tree: what it hands back to a person
				// afterwards is what it may wait on (workflow's ReconcileAwaiting).
				if err := a.store.StartHandout(ctx, tx, issue.Key); err != nil {
					return admitted, released, fmt.Errorf("record admitted issue %s: %w", issue.Key, err)
				}
				if err := a.enqueue(ctx, tx, issue.Key, record.IssueBranch{Generation: issue.Generation}, now); err != nil {
					return admitted, released, err
				}
				// The architect's start hands it work it was not on: the derivation's first task
				// over the record as it stands, if it serves this root one, raised as the
				// architect's phase entry and carried on the start with that entry, in this
				// transaction (workflow.Engine.StartArchitect, which the sweep's raise makes too).
				// A root's architect holds that row exactly as every phase worker does: the work
				// is a fact of the record rather than of the one outbox row that carried it, so a
				// start that reaches no claim loses none of it, and the outbox reads the entry off
				// this row as it does for every other role. workflow.ArchitectStartTask says
				// which of its shapes a root gets, and why a root it serves no task is started
				// with what its tree is owed and the roots that rest on it alone, or with nothing.
				// Every shape names each root that rests on this root's approval and is still in
				// the workflow, apart from what its tree is owed (dispatch://INFRA-3320, R8),
				// which is how an ancestor the daemon recorded in no live tree when such a root
				// registered learns of that root.
				//
				// The architect start is not one of the starts that wait for a slot: this very
				// transaction grants the tree whatever slot it needs, and an architect starts no
				// phase worker. It is marked decided (workflow.Engine.StartPromotedArchitect): a root
				// stands in the line only by a person's or the controller's todo, and the restart of
				// a tree, a backlog and then a todo on its root, brings back an architect held for
				// OpenAI's cyber-policy refusal this way, in a new session.
				if err := a.engine.StartPromotedArchitect(ctx, tx, issue); err != nil {
					return admitted, released, fmt.Errorf("start the architect of %s: %w", issue.Key, err)
				}
				if err := a.returnOwedMembers(ctx, tx, issue, issues, facts.OwedRows, carriedSlot, now); err != nil {
					return admitted, released, err
				}
				opened = opened || facts.Gates[issue.Tree].Open()
			case record.LineFirstPhase:
				if err := a.engine.StartFirstPhase(ctx, tx, issue, rows[issue.Key].Blockers); err != nil {
					return admitted, released, err
				}
			}
			admitted++
			promoted = append(promoted, issue.Key)
		}
		// A resume, or a tree opened above whose design gate already stands open, releases
		// members of its own tree that stood at admitted on the snapshot this round read and
		// would otherwise wait a whole fact for their first phase. An issue whose wait ended
		// and took its row back is read again for the same reason and with the same reach: the
		// row is all its own promotion gave it, and what its record needs next — the first
		// phase of an issue standing at admitted — is the line's to offer, from the record the
		// grant leaves. The records are read again and the line rebuilt so they are promoted in
		// this pass; a pass a hold stopped goes no further, since the hold is the answer for
		// everything behind it too.
		if stopped || (!opened && !resumed) {
			break
		}
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return admitted, released, err
		}
		if rows, facts, line, dropped, err = blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, rows, a.now()); err != nil {
			return admitted, released, err
		}
	}
	if err := a.resumeSlottedWorkers(ctx, tx, issues, slots, facts, floor); err != nil {
		return admitted, released, err
	}
	// A step leaving gates.design needs nothing here. A root left at admitted by a step that has
	// gone is an issue whose first phase the waiting line offers a slot to on this very pass
	// (record.Waiting, LineFirstPhase), because the line reads the gate's whole step list on
	// every pass rather than waiting for a fact about it. The other half that once needed a pass
	// of its own — a merger's READY the dropped step refused — went with READY: the merger merges
	// the head it read and no gate refuses a completion (dispatch://INFRA-714 R2).
	a.observePass(ctx, issues, slots, facts, rows, line, dropped, &moves, promoted, kind)
	return admitted, released, nil
}

// reread takes this pass's shared reads again, after the pass has itself written something that
// makes them stale: the project's live records, the slots, and the facts the line, the release
// and the repair passes are all judged from.
func (a *Admission) reread(ctx context.Context, tx pgx.Tx) ([]record.Issue, []record.Slot, record.LineFacts, error) {
	issues, err := a.store.LiveIssues(ctx, tx, a.project)
	if err != nil {
		return nil, nil, record.LineFacts{}, fmt.Errorf("list admission issues: %w", err)
	}
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return nil, nil, record.LineFacts{}, fmt.Errorf("list admission slots: %w", err)
	}
	facts, err := a.lineFacts(ctx, tx, issues)
	return issues, slots, facts, err
}

// lineFacts is the facts of this admission's pass for issues (record.ReadLineFacts), read under
// the pass's own slot rule, its engine's (record.ReadLineFactsUnder, workflow.Engine.SlotRule):
// every read of the facts the pass judges comes through here, so the release, the line, the
// waits, the held words, the grant and the repair all answer under the one rule the engine runs,
// and none of them reads the daemon's own in its place. Where the admission was given tables of a
// rule the daemon does not run (UseRuleTables), what that rule records there is read in beside
// them: the merger's ready marks, the covers that stand, the first slots and the arrivals
// (RuleTables.ReadRuleFacts).
func (a *Admission) lineFacts(ctx context.Context, tx pgx.Tx, issues []record.Issue) (record.LineFacts, error) {
	rule := a.engine.SlotRule()
	facts, err := record.ReadLineFactsUnder(ctx, tx, a.store, a.project, issues, a.engine.GatedSteps(), rule)
	if err != nil || a.tables == nil {
		return facts, err
	}
	if err := a.tables.ReadRuleFacts(ctx, tx, issues, &facts); err != nil {
		return record.LineFacts{}, fmt.Errorf("read what the slot rule %s keeps for %s: %w", rule.Name, a.project, err)
	}
	return facts, nil
}

// slotFor says whether promoting this candidate must take one of the cap's slots for it: it
// holds none already (record.LineEntry.Holds, which the line sets from the slots it was built
// from) and its phase keeps one (record.KeepsSlot, asked of the pass's own rule through its
// facts: record.LineFacts.KeepsSlot). The grant and the release read that one predicate, so no
// pass hands out a row the next pass would take back.
//
// It reads the phase the candidate's record carries rather than the phase its promotion leaves
// it in, and the two agree wherever this is asked: KeepsSlot refuses accepting, retro and
// merging (in the default steps, the phases after the review), a phase of done and a Dispatch
// status out of the workflow, and no promotion moves an issue into or out of any of them. A first
// phase starts at the phase of the task's first step from a record standing at admitted —
// planning or implementing, since every tester, reviewer and acceptance step stands after the
// implementer — an answered root goes from awaiting_person back to admitted, a returning issue
// keeps the worker phase it stands in, and a root whose tree is opened keeps the phase it ran in
// the tree it left.
//
// So a root another tree handed over carrying accepting, retro or merging is a candidate this
// refuses, and its promotion opens its tree while it holds no row. That follows from the rule
// and is intended: the work of such a root in those phases is its sign-off, its retro and its
// merge, which write no more of the change, and the cap counts the issues whose change is still
// to write. The tree opens, the phase's own worker starts on a row the outbox holds for no slot
// (workflow.StartOrOwe), and the root takes a row again only where its work moves to a phase
// that keeps one: its architect sends the work back, or a step after its acceptance whose phase
// keeps a slot starts, and it waits for that row in the line. Under a rule that keeps a slot in
// one of those phases, such a root takes one at its promotion as any candidate in a phase that
// keeps one does.
//
// A candidate that holds a row takes no second one: the line carries holders (the kinds
// record.LineEntry.Holds names). A candidate that takes no slot is promoted whatever the cap:
// holding it back would bound nothing and would leave its tree closed for no reason.
func slotFor(entry record.LineEntry, facts record.LineFacts) bool {
	return !entry.Holds && facts.KeepsSlot(entry.Issue)
}

// capLets says whether the cap and the P0 bound let the promotion start entry, given held, the
// project's holders as the two count them under the pass's rule (record.SlotRule.HoldersOf),
// facts, the pass's own, lending, the covers of the pass (covers), and gate, what the pass read
// once (passGate): the P0 bound it admits under (nil for none) and the signals it admits below P0
// by, if any (capHold). An entry that takes no slot (slotFor) starts whatever the cap
// says, and so does one that a counted waiter that waits on it offers its own slot to, under a rule
// whose start may take a cover (record.TakeCover), which the waiter gives up for it (covers.take),
// so the start leaves the count where it stood; a covered waiter holds no row, so held is the count
// the cap bounds. Under a rule that starts a P0 issue outside the cap
// (record.SlotRule.StartsOutsideCap: its own priority is P0, never its tree root's), such an entry
// that takes a slot starts while fewer such issues than the bound hold a slot, or always where no
// bound is in force, whatever the number of rows in use, so the slots in use may stand above the
// cap after it. Any other entry that takes a slot starts only while the holders the cap counts
// (held.Capped, every holder under a rule that starts none outside the cap) leave a row free. A
// cap that admits nothing lets nothing start, a P0 among it, since the promotion does not run under
// it. It answers the cap's half alone: what an entry waits on is the other half
// (blockers.LineWaitsOn), which each reader asks beside this, so every hold on a start but the cap
// holds a P0 as it holds any other entry.
//
// It is the one statement of the cap's half, and two readers ask it: the promotion itself
// (promoteHolds) and the stall measure's free-slot term (lineCanMove). Read apart, the measure
// said the cap held a queued root in accepting, retro or merging that the promotion of the same pass started,
// and told the controller of a stall that was not one.
func (a *Admission) capLets(entry record.LineEntry, held record.SlotHolders, facts record.LineFacts, lending covers, gate passGate) bool {
	return a.capHold(entry, held, facts, lending, gate) == ""
}

// capHold is why the cap's half holds entry, the word the pass's record gives for it, and "" where
// the cap lets it through (capLets). A cap that admits nothing holds every entry. An entry that
// takes no slot, or starts on a slot a waiter offers, is never held. An entry that starts outside
// the cap is held by the P0 bound alone, at every kind of pass. Any other entry that takes a slot
// is decided by the rule's own half of admission by signals (record.SlotRule.SignalsLet) where the
// pass was given a snapshot (UseSignals), which holds it at a tick's pass (record.PassedTick), by a
// signal over its limit (record.PassedSignal) or by the holders below P0 at the ceiling or the
// allowance (record.PassedCap); without one it starts while the holders the cap counts leave a
// row free, as before. It gives no slot back: a holder is never judged here.
func (a *Admission) capHold(entry record.LineEntry, held record.SlotHolders, facts record.LineFacts, lending covers, gate passGate) record.SlotPassedReason {
	if a.cap <= 0 {
		return record.PassedCap
	}
	if !slotFor(entry, facts) || lending.offers(entry.Issue.Key) {
		return ""
	}
	rule := facts.Rule.OrMain()
	if rule.StartsOutsideCap(entry.Issue) {
		if gate.p0Bound == nil || held.P0 < *gate.p0Bound {
			return ""
		}
		return record.PassedCap
	}
	if gate.bySignals {
		switch rule.SignalsLet(gate.kind, gate.snapshot, held.Capped, a.cap) {
		case signals.Starts:
			return ""
		case signals.HeldByTick:
			return record.PassedTick
		case signals.HeldBySignal:
			return record.PassedSignal
		default:
			return record.PassedCap
		}
	}
	if held.Capped < a.cap {
		return ""
	}
	return record.PassedCap
}

// held says whether Reconcile still holds key for the Dispatch consumer to reach the stream
// position it captured for it (a.pending).
func (a *Admission) held(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, held := a.pending[key]
	return held
}

// HoldForCatchUp holds each of keys out of the decision whether it waits on a person until
// ReleaseCatchUp releases it: the daemon's boot holds every root its catch-up will read before the
// boot's own admission pass, so no root is decided on a record that has not yet applied what
// Dispatch recorded while the daemon was down.
func (a *Admission) HoldForCatchUp(keys ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.catchingUp == nil {
		a.catchingUp = make(map[string]struct{}, len(keys))
	}
	for _, key := range keys {
		a.catchingUp[key] = struct{}{}
	}
}

// ReleaseCatchUp ends key's catch-up hold; the next pass decides it, which the pass rule (Apply)
// owes the release (catchUpPassOwed).
func (a *Admission) ReleaseCatchUp(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.catchingUp, key)
	a.catchUpReleases++
}

// CatchUpHeld is every key the boot's catch-up still holds, in no order.
func (a *Admission) CatchUpHeld() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Collect(maps.Keys(a.catchingUp))
}

// catchingUpFor says whether the boot's catch-up still holds key (HoldForCatchUp).
func (a *Admission) catchingUpFor(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, holding := a.catchingUp[key]
	return holding
}

// tellChildrenFirst tells the architect that owns each parent whose own first phase now waits for
// its Dispatch children, once per issue and per run (record.Store.TellChildrenFirst): a root's own
// architect, and for a child the architect above it, since the child's own starts only with the
// first phase this wait holds. A child a person set back to todo while it ran or was held is the
// exception: that re-entry starts and stops nothing (workflow's ReenterChild), so a child's own
// architect that held its role still holds it and is told. The notice names the parent, each
// open child with the Dispatch status that holds it, and each cycle among them
// (workflow.ChildrenFirstNotice).
//
// The wait is judged again every pass and the telling is written once, which is the whole of
// why the two are recorded differently: every pass the pass rule (Apply) runs, and the boot's,
// judges it, so a notice decided by the wait alone would reach the architect over and over for
// one wait, while a wait decided by a stored flag would outlive the child that caused it. A child
// filed later during one wait therefore tells nobody again — the architect already knows the
// issue waits, and the state names what for — and a re-admission, which opens a new generation,
// tells it anew, because that run's architect has not heard it.
//
// The run is the tree root's generation, not the waiting issue's own: re-admission raises the
// root's and leaves every member's where it stands (workflow's linger), so a sub-parent that
// waited for its unreleased children before the park would be marked told for ever and the new
// run's architect would never hear of that wait. The mark is keyed on the tree as well, because
// a generation names a run only within one tree: a sub-parent handed over as a root of its own
// numbers its run from its own record (readmit), which can be the number the tree it left was
// at. The mark's own table says so (20261104004500_children_first_notices).
//
// The parents are told in key order so that one pass's rows read the same whatever order the
// judgement's map iterated in.
func (a *Admission) tellChildrenFirst(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts record.LineFacts) error {
	if len(facts.ChildWaits) == 0 {
		return nil
	}
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	now := a.now()
	for _, key := range slices.Sorted(maps.Keys(facts.ChildWaits)) {
		parent, recorded := byKey[key]
		if !recorded {
			continue
		}
		told, err := a.store.TellChildrenFirst(ctx, tx, key, parent.Tree, byKey[parent.Tree].Generation, now)
		if err != nil {
			return err
		}
		if !told {
			continue
		}
		if err := a.enqueue(ctx, tx, key, workflow.ChildrenFirstNotice(facts.ChildWaits[key]), now); err != nil {
			return err
		}
		children := len(facts.ChildWaits[key].Open)
		a.logOnCommit(ctx, "admission: an issue waits for the Dispatch children under it", "issue", key,
			"tree", parent.Tree, "generation", parent.Generation, "children", children)
	}
	return nil
}

// waitsForChildren says whether an issue's own first phase is held because a Dispatch child of
// its own is still open — the children-first rule of dispatch://INFRA-980. It is the named seam
// the safety net below asks, so that the rule arrives in one place rather than as a condition
// spread through this file, and waits is the pass's own judgement of it
// (record.LineFacts.ChildWaits, filled by blockers.JudgeLine from record.ChildWaitsOf): never a
// stored flag, so a parent whose last open child has gone waits no longer than the pass that
// reads it.
func (a *Admission) waitsForChildren(waits map[string]record.ChildWait, issue record.Issue) bool {
	_, waiting := waits[issue.Key]
	return waiting
}

// unslotted says whether issue is a root of this project left in the workflow (workflow's
// InWorkflow) with no way to run, the state dispatch://INFRA-129 reported: not lingering, at a
// status only a running tree has (in_progress, testing, needs_review, retro) rather than todo,
// where a root waits in the line (record.Waiting), and in none of the states that explain a root
// holding no slot.
//
// Holding no slot is the normal state of a root waiting to start: the cap counts the issues the
// daemon has started whose change is still to write, so a root waiting in the line, one whose
// tree has left the workflow and one standing in accepting, retro or
// merging hold none, and a started root in a phase that keeps one that waits on another issue — a
// blocker of it that is not done, or an open child its own first phase waits for — gives its row
// back for that wait as well. Three started roots keep the row they were started with through
// such a wait (blockers.waitsOf): one waiting on a person (awaiting_person, answered or not), one
// at admitted whose design gate has never opened (record.AtWork), and a queued root that arrived
// from another tree carrying its row (record.QueuedRoot). Every one of those is a tree whose
// lifecycle stands open under the workflow, or whose root waits in the line or on a person,
// which is exactly record.LiveRoots: so liveness is the term that replaces the old slot test,
// and it covers the slots, the line and the awaiting_person wait together. The children-first
// wait is asked of the issue as well, so that a later rule which holds an issue somewhere
// liveness does not reach cannot silently have its work taken out of the workflow.
//
// What a root's own Dispatch links say is not asked, though it reads as a wait of the same
// kind. A root the line holds behind an unfinished link is in todo, which this rule has already
// passed over, and a root at a running status whose links hold it is a started root the pass
// has released for that wait (record.LineFacts.Waits): its tree stands open, so liveness has
// already spared it above and the links add nothing here. Asking blockers.WaitsOn here answered
// from the mirror rows this pass happened to read for other issues, so whether the safety net
// spared a root turned on whether something else had named it.
//
// What is left is a root whose tree nothing will ever open again, recorded at a running status.
// The workflow takes a root out on a status out of the workflow (its leave) but does nothing on a
// running tree's status where no tree runs, so only such a move made while the root waited leaves
// one here: a person's, or an outside session's, which is a person's where no tree runs
// (workflow's sessionStatusWrite). A root in todo without the label is not one: taking the label
// off is a pause the label ends.
func (a *Admission) unslotted(issue record.Issue, live map[string]struct{}, waits map[string]record.ChildWait) bool {
	if !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.Lingers() || !record.InWorkflow(issue) || issue.AwaitingPerson() {
		return false
	}
	switch issue.Status {
	case "in_progress", "testing", "needs_review", "retro":
	default:
		return false
	}
	if _, running := live[issue.Key]; running {
		return false
	}
	if a.waitsForChildren(waits, issue) {
		return false
	}
	return true
}

// releaseUnslotted takes every root nothing will ever start out of the workflow, as a status
// leaving todo takes a waiting root out (workflow's LeaveUnslotted): no tree of it is open and
// none will be opened again, so nothing would ever start it while its record went on saying its
// tree runs. A conventional unslotted root belongs to no live tree (unslotted); an
// awaiting-person root is live by design, and leaves only when its hand-over label or its current
// Dispatch claim makes a resume unsafe (workflow's Unresumable). Legion starts nothing for
// either — it is someone else's work now, or a person's move to make — and the controller is told
// on the controller topic alone (record.UnslottedNotice, one per root, its reason naming why). A
// key Reconcile holds is left for release to apply first, when respectHolds is set. It reports
// whether it released any.
func (a *Admission) releaseUnslotted(ctx context.Context, tx pgx.Tx, issues []record.Issue, live map[string]struct{},
	waits map[string]record.ChildWait, respectHolds bool,
) (bool, error) {
	released := false
	for _, issue := range issues {
		if respectHolds && a.held(issue.Key) {
			continue
		}
		why, awaiting, err := a.awaitingUnresumable(ctx, tx, issue)
		if err != nil {
			return released, fmt.Errorf("check whether awaiting root %s can resume: %w", issue.Key, err)
		}
		if !awaiting && !a.unslotted(issue, live, waits) {
			continue
		}
		reason := fmt.Sprintf("%s was %s, a status only a running tree has, while no tree of it runs: its lifecycle is not open, it waits in no line and on no person, so nothing would ever start it", issue.Key, issue.Status)
		if why != "" {
			reason = fmt.Sprintf("%s awaits a person, but %s", issue.Key, why)
		}
		if err := a.engine.LeaveUnslotted(ctx, tx, issue); err != nil {
			return released, fmt.Errorf("release unslotted root %s: %w", issue.Key, err)
		}
		reason += "; the daemon set its phase to done and starts nothing for it; todo with the legion label queues it again"
		if err := a.enqueue(ctx, tx, issue.Key, record.ControllerNotice{Kind: record.UnslottedNotice, Reason: reason}, a.now()); err != nil {
			return released, err
		}
		store.OnCommit(ctx, func() {
			a.log.Warn("admission: released a root no tree of which runs and nothing would ever start", "issue", issue.Key, "status", issue.Status, "phase", issue.Phase, "generation", issue.Generation)
		})
		released = true
	}
	return released, nil
}

// awaitingUnresumable returns why an awaiting root of this project may not resume (workflow's
// Unresumable, the one rule ReconcileAwaiting also asks) and whether this pass must remove it. It
// asks nothing of the slots because a row answers none of what it asks: the question is whether
// this root may come back at all, which its label and its Dispatch claim decide, and the root
// keeps its row through the whole wait either way (record.KeepsSlot).
func (a *Admission) awaitingUnresumable(ctx context.Context, tx pgx.Tx, issue record.Issue) (string, bool, error) {
	if !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.Lingers() || !record.InWorkflow(issue) || !issue.AwaitingPerson() {
		return "", false, nil
	}
	reason, err := workflow.Unresumable(ctx, a.store, tx, a.project, issue)
	return reason, reason != "", err
}

// Held reports whether any key currently waits on the Dispatch consumer reaching the stream
// position Reconcile captured for it. The intake consume loop calls it once per Dispatch message,
// outside any transaction, to decide whether a JetStream Info call and a synthetic position fact
// are worth their cost at all: once nothing is held, every later delivery is a lock check and
// nothing else.
func (a *Admission) Held() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending) > 0
}

// Target is the notification stream position Held's hold is waiting for: the highest target any
// currently deferred key was captured against. Zero while nothing is held. A boot-owned watchdog
// reads it to name what a long-lived hold is still waiting to reach.
func (a *Admission) Target() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.target
}

// Holds counts the times Reconcile has deferred keys into the hold since the daemon started: the
// boot's reconcile, and each recovery of a repository GitHub answered for again. A poller of the
// hold (the daemon's pollHoldRelease) folds it into what it applies, so a later hold, released by
// a position equal to an earlier hold's, is applied rather than taken for that earlier release.
func (a *Admission) Holds() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.holds
}

// heldSummary is the summary pending holds for a key, with the write into pending that put it there
// (Admission.stamps), so a release clears the key only while pending still holds the write the
// release applied (clearReleased).
type heldSummary struct {
	summary intake.DispatchIssue
	stamp   uint64
}

// holdDeferred holds each key of deferred on its summary until the Dispatch consumer reaches
// target, and counts the deferral (Holds).
func (a *Admission) holdDeferred(deferred map[string]intake.DispatchIssue, target int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, summary := range deferred {
		a.stamps++
		a.pending[key] = heldSummary{summary: summary, stamp: a.stamps}
	}
	a.holds++
	if target > a.target {
		a.target = target
	}
}

// refreshHeldSummary keeps a held, unrecorded key's own pending summary — the one release
// eventually applies — current with a live event that is newer than it but creates no record: an
// event moving the key out of todo, or taking its label off, teaches nothing to the record (there
// is none yet), but it is real information about what Dispatch shows now. Left alone, release
// would find the summary Reconcile originally deferred older than this event and apply nothing of
// it (unrecordedSummaryStale); refreshed, release applies what this event showed. A key that is
// not held, or an event at or behind the held summary's own sequence, is left untouched.
func (a *Admission) refreshHeldSummary(observation intake.DispatchIssue) {
	a.mu.Lock()
	defer a.mu.Unlock()
	hold, held := a.pending[observation.Key]
	if !held || observation.Seq <= hold.summary.Seq {
		return
	}
	a.stamps++
	a.pending[observation.Key] = heldSummary{summary: intake.DispatchIssue{
		Key: observation.Key, Seq: observation.Seq, Status: observation.Status, Title: observation.Title,
		Parent: observation.Parent, Rank: observation.Rank, Priority: observation.Priority, HandedOver: observation.HandedOver,
		ResolvedRepo: observation.ResolvedRepo, Ancestry: observation.Ancestry,
	}, stamp: a.stamps}
}

// release applies the Dispatch consumer's current position: once it has Reached target, every key
// Reconcile deferred is re-applied through applySummary — the listing's own snapshot for it, in
// case the stream never delivers the event that would otherwise have caught it up — before
// promote decides anything, so a held key is never admitted, or left admitted, on stale
// information. The in-memory pending set only clears once this call's own transaction has
// actually committed (store.OnCommit): clearing it any earlier would say a hold is
// resolved while a failed commit leaves nothing of this release durable, so the next
// promote-triggering call would trust an unreleased hold's authority.
func (a *Admission) release(ctx context.Context, tx pgx.Tx, position intake.DispatchConsumerPosition) ([]string, error) {
	releasing := a.releasable(position)
	caughtUp := releasing != nil
	if caughtUp {
		issues, err := a.store.LiveIssues(ctx, tx, a.project)
		if err != nil {
			return nil, fmt.Errorf("list admission issues: %w", err)
		}
		facts, err := a.lineFacts(ctx, tx, issues)
		if err != nil {
			return nil, err
		}
		live := record.LiveRoots(issues, facts)
		held := make([]intake.DispatchIssue, 0, len(releasing))
		for _, hold := range releasing {
			held = append(held, hold.summary)
		}
		slices.SortFunc(held, func(x, y intake.DispatchIssue) int { return cmp.Compare(x.Key, y.Key) })
		for _, summary := range parentsFirst(held) {
			// A held summary older than a fact the daemon applied records nothing, and that fact,
			// applied after Reconcile deferred the summary, refreshed it or left a trace of its own
			// (Reconcile says which), so nothing more is owed for it here. It is said at Info all the
			// same, so a change that ever leaves such an issue with no trace shows in the log.
			outcome, err := a.applySummary(ctx, tx, summary, live, false)
			if err != nil {
				return nil, err
			}
			if outcome == summaryStale {
				key, listed := summary.Key, summary.Seq
				store.OnCommit(ctx, func() {
					a.log.Info("admission: a held summary is older than an issue fact of the issue the daemon applied after it was held, so its release records nothing; that fact decided the issue",
						"issue", key, "listed", listed)
				})
			}
		}
	}
	// The release's line says what this transaction does with the held roots, so it is a hook of
	// its commit, registered before the pass's own so it still comes before the slots the pass
	// takes, and it names the count the pass admitted, which the hook reads once the pass is done.
	var admitted int
	if caughtUp {
		store.OnCommit(ctx, func() {
			a.log.Info("admission: the Dispatch consumer has caught up; releasing held roots",
				"count", len(releasing), "ack_floor", position.AckFloorStream, "idle", position.Idle, "admitted", admitted)
		})
	}
	admitted, returned, err := a.promoteHolds(ctx, tx, !caughtUp, signals.PassEvent)
	if err != nil {
		return nil, err
	}
	if !caughtUp {
		return returned, nil
	}
	store.OnCommit(ctx, func() { a.clearReleased(releasing) })
	return returned, nil
}

// releasable is every key pending holds, with the write that holds it, once position has reached
// the target Reconcile captured; nil while nothing is held or the consumer is behind it.
func (a *Admission) releasable(position intake.DispatchConsumerPosition) map[string]heldSummary {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pending) == 0 || !position.Reached(a.target) {
		return nil
	}
	return maps.Clone(a.pending)
}

// clearReleased clears each key of releasing that pending still holds on the write the release
// applied. A release's hook can run after a later refresh or a later hold of the same key, on the
// commit watcher's goroutine when the store could say only later that the release committed
// (store.OnCommit), and it must not clear what it did not apply: such a key stays held for a
// release of its own.
func (a *Admission) clearReleased(releasing map[string]heldSummary) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, applied := range releasing {
		if a.pending[key].stamp == applied.stamp {
			delete(a.pending, key)
		}
	}
}

// ownSlots is the slots of issues, this project's. The slots table is shared by every project's
// daemon, and a slot's index is unique across it (0004_record), so the next index is chosen over
// every slot while the cap counts this project's alone.
func ownSlots(issues []record.Issue, slots []record.Slot) []record.Slot {
	known := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		known[issue.Key] = struct{}{}
	}
	own := make([]record.Slot, 0, len(slots))
	for _, slot := range slots {
		if _, ok := known[slot.Issue]; ok {
			own = append(own, slot)
		}
	}
	return own
}

// markSlotWaits writes, for each holder of this project's slots, whether it waits on another
// issue while it keeps its slot (blockers.KeptWaits over the pass's own rows and facts): a holder
// whose waits went from none to some is marked at the pass's clock, and one whose waits ended is
// cleared (record.Store.MarkSlotWait). A holder whose mark already says what it waits on is
// written nothing, so a wait keeps the moment it began across every pass and a pass that changes
// nothing writes nothing. It reads the mark off the slots the pass holds (record.Slot.WaitsSince),
// so it makes no read of its own.
func (a *Admission) markSlotWaits(ctx context.Context, tx pgx.Tx, issues []record.Issue, slots []record.Slot,
	rows map[string]blockers.Row, facts record.LineFacts,
) error {
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	for _, slot := range ownSlots(issues, slots) {
		waits := len(blockers.KeptWaits(rows, byKey[slot.Issue], facts)) > 0
		if waits == (slot.WaitsSince != nil) {
			continue
		}
		if err := a.store.MarkSlotWait(ctx, tx, slot.Issue, waits, a.now()); err != nil {
			return err
		}
	}
	return nil
}

// returnOwedMembers is the first run of a re-admitted tree: it brings back the members the
// re-admission left mid-phase (startMidPhaseMembers) and those reopened at done
// (reenterStrandedChildren), and clears root's mark (record.Issue.MembersOwed) in the same
// transaction. Bringing the members back and clearing the mark together is what makes the return
// once in the generation (dispatch://INFRA-5471), so both callers come through here: the promotion
// of a queued root, which brings back the members of every tree it opens whatever its mark, and
// the pass that finds a re-admitted tree running before any promotion
// (returnMembersOfRunningTrees). Only a root that carries the mark is written, so a fresh root's
// promotion costs no statement.
func (a *Admission) returnOwedMembers(ctx context.Context, tx pgx.Tx, root record.Issue, issues []record.Issue,
	owedRows []record.OwedStart, carriedSlot bool, now time.Time,
) error {
	if err := a.startMidPhaseMembers(ctx, tx, root, issues, owedRows, carriedSlot, now); err != nil {
		return err
	}
	if err := a.reenterStrandedChildren(ctx, tx, root, issues); err != nil {
		return err
	}
	if !root.MembersOwed {
		return nil
	}
	return a.store.ClearMembersOwed(ctx, tx, root.Key)
}

// returnsMembersThisPass says whether the pass's member return reads root's mark as a return it
// owes and may run: the mark is on a root whose tree runs, its design gate has opened and nothing
// holds it (heldKeys). Whether the return starts the members is startsMembersThisPass's.
func returnsMembersThisPass(root record.Issue, facts record.LineFacts, held func(string) bool) bool {
	return record.MembersReturnOwed(root, facts.Lifecycles[root.Key], facts.Gates[root.Key]) && !held(root.Key)
}

// startsMembersThisPass is the one reading of whether this pass's member return starts root's
// members, shared by the return (returnMembersOfRunningTrees) and the stop that leaves it to
// decide whether to hold those members (childrenWaits): the return runs for root
// (returnsMembersThisPass) and root's promotion is not one an earlier release already made
// (promotedByEarlierRelease), whose pass clears the mark and starts nothing. slotted is
// slottedIssues of the caller's slots: the stop reads them before the pass's releases and the
// return after them, and childrenWaits says why both give one answer.
func startsMembersThisPass(root record.Issue, facts record.LineFacts, held func(string) bool, slotted map[string]struct{}) bool {
	_, holds := slotted[root.Key]
	return returnsMembersThisPass(root, facts, held) && !promotedByEarlierRelease(root, holds)
}

// slottedIssues is the keys of the issues that hold one of slots, the read of the slots that
// promotedByEarlierRelease takes its answer from.
func slottedIssues(slots []record.Slot) map[string]struct{} {
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	return slotted
}

// returnMembersOfRunningTrees brings back the members of each re-admitted tree that runs with no
// promotion to come (record.MembersReturnOwed): its root's design gate opened while the root still
// waited in the line, so the root stopped being a queued root and its promotion, which brings a
// re-admitted tree's members back, will not run. The members come back here as that promotion
// brings them (returnOwedMembers), in the same pass as the gate's opening and whatever path opened
// it: register_gate, a person's approval or a carried copy all reach this pass, since each writes
// the gate's row (record.RecordsWritten). It answers whether it wrote anything, so the caller
// reads the records again.
//
// The mark is what says a member waits for its tree's first run: a suspended member is no sign of
// it, since an idle release, a tester between production-check looks and an operator's suspension
// leave the same state, and only the re-admission knows which trees it left mid-phase.
//
// heldKeys is the promotion's own hold (promoteHolds): a root the Dispatch consumer's pending
// Reconcile summary or the boot's catch-up still holds is passed over, as the promotion stops at
// it, because its record may still miss what Dispatch holds. The pass after the hold's release
// brings its members back.
//
// A marked root an earlier release's promotion left (promotedByEarlierRelease) has its mark
// cleared and nothing started: that generation's members came back already.
func (a *Admission) returnMembersOfRunningTrees(ctx context.Context, tx pgx.Tx, issues []record.Issue, slots []record.Slot,
	facts record.LineFacts, heldKeys func(string) bool,
) (bool, error) {
	slotted := slottedIssues(slots)
	wrote := false
	for _, root := range issues {
		if !returnsMembersThisPass(root, facts, heldKeys) {
			continue
		}
		wrote = true
		if !startsMembersThisPass(root, facts, heldKeys, slotted) {
			if err := a.store.ClearMembersOwed(ctx, tx, root.Key); err != nil {
				return wrote, err
			}
			a.logOnCommit(ctx, "admission clears the members' return an earlier release's promotion already made",
				"issue", root.Key, "generation", root.Generation)
			continue
		}
		if err := a.returnOwedMembers(ctx, tx, root, issues, facts.OwedRows, false, a.now()); err != nil {
			return wrote, err
		}
		a.logOnCommit(ctx, "admission brings back the members of a re-admitted tree that runs before its promotion",
			"issue", root.Key, "generation", root.Generation)
	}
	return wrote, nil
}

// promotedByEarlierRelease says whether root, which carries the mark of a return still owed
// (record.Issue.MembersOwed), was promoted by a daemon of an earlier release that shares this
// database: holds is whether it holds an admission slot. That release's promotion brought the
// members back and never names the mark, so the mark outlived it. Such a root holds the slot the
// promotion granted, or carries the mark of the slot it gave back for a wait
// (record.Issue.ReleasedForWaitAt), or stands in a phase of its own tree past admitted, as it does
// once its tree has moved on to accepting, retro or merging, which keep no slot and clear the wait
// mark under the rule every earlier release ran (record.MainRule's PhaseKeepsSlot, releaseSlots).
//
// A marked root of this release is none of these. Its promotion clears the mark in the
// transaction that grants the slot; its re-admission leaves its tree no slot of the generation
// before (releaseTreeSlots says why); and it takes no first phase before
// returnMembersOfRunningTrees has brought its members back, since that pass runs ahead of the
// promotion loop, a root it passes over for the promotion's hold is one that loop stops at through
// the same hold, and a root whose label a person took off is held in the line (record.Waiting).
// The phase is read where the root stands (record.StandsAt), not as Issue.CarriesRun reads it: a
// marked root whose retried architect failed is held while its members are still owed, and it
// stands at admitted. A root that arrived from another tree carrying its phase there
// (Issue.HandedOverFrom) stays a queued root until its promotion, which clears that mark, so it is
// never one either. Once no daemon of an earlier release shares a database, this goes with its
// term in startsMembersThisPass, the slotted map that function reads (slottedIssues) and the
// slots parameters of childrenWaits and returnMembersOfRunningTrees.
func promotedByEarlierRelease(root record.Issue, holds bool) bool {
	return holds || root.ReleasedForWaitAt != nil || (root.HandedOverFrom == "" && record.StandsAt(root) != phase.Admitted)
}

// startMidPhaseMembers brings back the roles that have run of every member of a re-admitted tree at
// its first run (returnOwedMembers, its one caller), and gives the one role whose phase the member
// stands in the task
// that carries that phase on. A hand-over turns a child into that root while preserving its
// phase, so its phase role needs the same resume start ordinary mid-phase children receive
// (workflow's Engine.ResumeTask, which says to carry the phase's work on and names no head, since a
// merger reads the head it merges on GitHub). A root promoted held, which only a hand-over leaves
// (every other re-admission starts its root at admitted), starts no worker. The hold's first
// notice was routed in the tree the root was handed over from, where it reached the root's own
// architect only if that claim held its role then, and else an architect above it, so its
// architect is told of the hold again as a root's (workflow.HeldNotice), whether or not it carried
// a gate. Promotion already queued the root's branch; each other member queues its own before
// starting.
// Unless open children hold the start, each member stays in the phase it carried. A child of a
// re-admitted tree and a root a hand-over created come back in merging where they stood in it.
// Their new tree's merger ends the phase: the old tree's linger stopped the merge its own merger
// had not made (beginLinger).
//
// It brings back a member's other roles beside the phase's own, because the admission cap counts
// the issues the daemon has started now: the one start that waits for a slot is the phase
// worker's, which carries that phase's task (workflow.StartOrOwe), and the live sessions of a
// member's other roles are exactly what the design keeps so that a person can still talk to any of
// them. Those rows carry no task and wait for no slot, so each is claimable at once and a role
// with nothing to do comes back and is told nothing. The role of a held member's stopped task's
// step is queued with them, and its start finishes at the outbox, where every start of a role is
// run (workflow.StoppedStepRole): that role starts again only when its task does.
//
// Such a role comes back only where it holds a claim row, which is the durable record that the
// role has run: a re-admission clears the generation's facts — clearGeneration touches
// pull_requests, design_gates, phases and issues, never claims — and a claim's token carries no
// generation (claim.NewToken), so the row outlives the generation that wrote it. A role that has
// never run holds none and is started by nothing here: its session would come up with no
// transcript and nothing asked of it, and only a message could ever wake it. The rows of every
// role of every member are read in one batched record.RoleRuns call, the read resumeSlottedWorkers
// makes of its own slots, rather than one read per role.
//
// The phase's own role is the exception, gated by workflow.StartFor alone: a member whose phase
// worker never started before the tree left the workflow holds no claim row for it, which is the
// ordinary case of a phase still to be worked, so gating that start on a claim row would mean the
// phase never runs again.
//
// The roles are enumerated as every other sweep of them does, over claim.Roles (the engine's
// everyClaim).
//
// A member at admitted is passed over: its first phase is the waiting line's to start. R9
// (dispatch://INFRA-3320) holds a mid-phase member's start at the root's promotion or at the pass
// that finds the tree running first, and judges each member before any role of it comes back. The
// return holds the phase worker's start where it makes that start (workflow's
// StartPromotedWorker), and judges every other member through workflow's StopPromotedWorker, which
// moves only a member that stands in a phase R9 holds (record.OpenChildHolds). A member R9 holds
// moves to awaiting_children, brings no other role back and starts no step. The promotion alone
// exempts k12's "a root handed over from another tree carrying its slot in `implementing`
// (`record.QueuedRoot`)": carriedSlot is that exemption whole, as the promotion read it of root,
// and false on every other return. The next pass stops that root as any started parent with an
// open child (childrenWaits).
//
// root is the tree's root as returnOwedMembers has it, and issues the pass's own records, which
// carry that root as the pass read it. The caller passes both and appends nothing: the root is
// read from the parameter and its copy among the records is passed over.
func (a *Admission) startMidPhaseMembers(ctx context.Context, tx pgx.Tx, root record.Issue, issues []record.Issue,
	owedRows []record.OwedStart, carriedSlot bool, now time.Time,
) error {
	project, err := claim.ProjectToken(a.project)
	if err != nil {
		return err
	}
	if root.Phase == phase.Held && root.Hold != nil {
		if err := a.enqueue(ctx, tx, root.Key, workflow.HeldNotice(root), now); err != nil {
			return err
		}
	}
	type roleToken struct {
		role  claim.Role
		token claim.Token
	}
	type midPhaseMember struct {
		issue record.Issue
		// owner is the role whose phase worker the return starts for the member, empty wherever
		// it starts none: a phase an architect works (accepting), a phase no role works, a member
		// in awaiting_children, and a merging member whose pull request has merged.
		owner claim.Role
		roles []roleToken
		// carriedSlot is k12's exemption of the member's carried start, which the root's walk
		// alone carries.
		carriedSlot bool
	}
	// The first pass reads each member where its record leaves it and names every role run the
	// second pass judges; the one read of them stands between the two, so no row is written on a
	// reading of the claims half of it made.
	var members []midPhaseMember
	var keys []record.RoleRunKey
	// The root's own record is taken from the parameter, which is never the staler of the two
	// (the promotion hands over the root it has just written), and its copy among the records is
	// passed over. The members are walked in the order of the records and the root after them, so
	// the root's rows are the last this writes. Reading both copies would start one member twice:
	// a phase worker handed the same task on two rows, which the start of accepting, retro or
	// merging no longer coalesces into one (workflow.StartOrOwe). carried is the exemption the
	// walked member carries: carriedSlot for the root, false for every other member.
	walk := func(member record.Issue, carried bool) error {
		if member.Tree != root.Tree || record.OutOfWorkflow(member.Status) {
			return nil
		}
		// A member standing at admitted has run nothing in this generation, and nothing of it is
		// brought back here: its own first phase starts when the waiting line gives it a slot
		// (record.LineFirstPhase), and a claim it kept from an earlier run comes back on its kept
		// session when a start of its role reaches it. A root whose tree is opened for the first
		// time is exactly that, so admitting a fresh root relaunches nobody and creates no claim
		// for a worker that has never worked.
		if member.Phase == phase.Admitted {
			return nil
		}
		// A member waiting in awaiting_children has no role at work and none is woken for it
		// (dispatch://INFRA-3320 R11): its next start is the one it holds, which the admission
		// pass gives it back once no open child is left (childrenWaits, workflow's
		// ReturnFromChildren), and every role of it waits for that start. So a promotion brings
		// none of its roles back, its phase worker or any other, and queues its branch alone, as
		// it queues the branch of every member past admitted.
		if member.Phase == phase.AwaitingChildren {
			members = append(members, midPhaseMember{issue: member})
			return nil
		}
		// Accepting's role is an architect, which no phase start brings back here: the root's is
		// promotion's own start, and a child's own comes back below with the member's other roles
		// that have run.
		owner := phase.RoleFor(member.Phase)
		if owner == claim.RoleArchitect {
			owner = ""
		}
		// Nor is a phase its own terminating fact has already met (workflow.PhaseFinished): a
		// merged root stands in merging for as long as the rest of its tree runs, and giving its
		// merger that phase's task again at every promotion would hand it work the merge has
		// already done. It is read here, where the owner is decided, rather than beside the start
		// below, so the member's other roles come back as they do for any phase with no owner.
		if owner != "" {
			finished, err := workflow.PhaseFinished(ctx, a.store, tx, member)
			if err != nil {
				return err
			}
			if finished {
				owner = ""
			}
		}
		standing := midPhaseMember{issue: member, owner: owner, carriedSlot: carried}
		for _, role := range claim.Roles {
			// The tree's architect is promotion's own start, with the task that hands it its
			// tree; a member's own architect, which the daemon started with that member's first
			// phase, is no phase worker and comes back with the rest.
			if claim.IsTreeArchitect(role, member.Key, member.Tree) {
				continue
			}
			token, err := claim.NewToken(project, member.Key, role)
			if err != nil {
				// An issue whose key yields no claim token holds no claim row at all, so no role
				// of it has ever run and none of the roles this brings back is started, as the
				// slot repair passes over such an issue rather than failing the whole fact
				// (resumeSlottedWorkers). The phase's own role is the member's work rather than
				// a session brought back, and its token's refusal stays the fact's, as it was.
				if role != owner {
					continue
				}
				return err
			}
			standing.roles = append(standing.roles, roleToken{role: role, token: token})
			keys = append(keys, record.RoleRunKey{Token: token, Issue: member.Key, Role: role, Generation: member.Generation})
		}
		members = append(members, standing)
		return nil
	}
	for _, member := range issues {
		if member.Key == root.Key {
			continue
		}
		if err := walk(member, false); err != nil {
			return err
		}
	}
	if err := walk(root, carriedSlot); err != nil {
		return err
	}
	// A member whose key yields no claim token names no role run, and its branch row is still
	// the promotion's to queue, so the read is skipped rather than the pass.
	var runs map[claim.Token]record.RoleRun
	if len(keys) > 0 {
		read, err := a.store.RoleRuns(ctx, tx, keys)
		if err != nil {
			return err
		}
		runs = read
	}
	owed := owedStartsByIssue(owedRows)
	for _, standing := range members {
		member := standing.issue
		if member.Key != root.Key {
			if err := a.enqueue(ctx, tx, member.Key, record.IssueBranch{Generation: member.Generation}, now); err != nil {
				return err
			}
		}
		// R9 judges the member before any role of it comes back, as startMidPhaseMembers' doc says.
		var held bool
		var err error
		owned := slices.IndexFunc(standing.roles, func(r roleToken) bool { return r.role == standing.owner })
		switch {
		case owned >= 0 && workflow.StartFor(runs[standing.roles[owned].token], root, member.Generation, member.Phase):
			held, err = a.engine.StartPromotedWorker(ctx, tx, member, standing.owner, standing.carriedSlot)
		default:
			held, err = a.engine.StopPromotedWorker(ctx, tx, member, owed[member.Key])
		}
		if err != nil {
			return err
		}
		if held {
			continue
		}
		for _, other := range standing.roles {
			// No claim row, so the role has never run: there is no session to bring back and no
			// task this start could carry, and the one it would make could only ever be woken by
			// a message.
			if other.role == standing.owner || runs[other.token].Claim == nil {
				continue
			}
			if err := a.enqueue(ctx, tx, member.Key, record.SuperviseRequest{Op: "start", Tree: member.Tree, Role: other.role,
				Generation: member.Generation, Phase: member.Phase}, now); err != nil {
				return err
			}
		}
		// The member's step starts again with the tree, whatever the promotion starts of it: a
		// phase worker's start above began its run already, and accepting's (whose role is the
		// architect that owns the member, workflow's acceptingAgent: its own where a start of it
		// queued above will run), held's (whose hold starts none) and a role workflow.StartFor
		// finds already started begin it here. The store's run mark decides (workflow's
		// StartStepRecord): a run still open goes on and gets no second start — the one a start
		// above began, and the merge step of a member held from merging, which the linger left
		// open and the hold's retry goes on with. No step starts for a member R9 moved to
		// awaiting_children above, which never reaches here, nor there for a member its plan
		// holds, a stopped task or a parent held for its children.
		if err := a.engine.StartStepRecord(ctx, tx, member); err != nil {
			return err
		}
	}
	return nil
}

// owedEntryRole is one role the entry work is owed to: the issue whose current phase it is the
// role of, or the root whose own architect it is, the phases row the work and the entry are read
// from, how the pass sends that work (Send), and the entry that role's claim was last given, which
// only the log line of the send reads. It stays unexported: the shape of this selection is this
// package's, and the one question another package asks of it is OwesEntryWork's yes or no.
type owedEntryRole struct {
	Issue record.Issue
	Role  claim.Role
	Row   record.PhaseRow
	Send  entrySend
	Given int64
}

// entrySend is how the pass sends one owed role its work. The selection decides it once, from the
// role and the entry its batched read of the phases rows gave (owedOf), and the send reads it
// once (resumeSlottedWorkers), so the two never weigh the same role apart.
type entrySend int

const (
	// sendPhaseEntry sends a phase worker the work of the entry its phases row names
	// (workflow.Engine.SendOwedEntry).
	sendPhaseEntry entrySend = iota
	// resendArchitectStart sends a root's architect the start of the entry its phases row already
	// names (workflow.Engine.SendArchitectStart).
	resendArchitectStart
	// raiseArchitectEntry hands a root's architect whose phases row names no entry its work: the
	// entry is raised and its start written (workflow.Engine.StartArchitect).
	raiseArchitectEntry
)

// OwesEntryWork is whether the entry sweep's own pass would do anything for the project, read as
// every admission pass reads the project (reread) and answered by the two selections that pass
// itself acts on: a role owed the work of the entry its phases row names (owedEntryRoles), and an
// arm of the stall reading that holds for a root architect the selection holds (readSlotHolders
// names the three kinds of root it holds), that no mark has told the controller yet, while a
// controller is registered to be told (architectStalls, record.Store.ArchitectStallsUntold,
// wakeStalledArchitects). It writes nothing, so
// the entry sweep's gate (internal/daemon's applyEntrySweep) asks it in a read-only transaction
// of its own, before any fact. The answer is a yes or a no rather than the selection, so no
// caller outside this package depends on the shape of a row the pass reads. A root architect the
// selection names is owed exactly as a phase worker is, so the gate opens for it as it opens for
// any other role.
//
// The arms are read only where nothing is owed, and then they are exactly the arms the pass
// reads: that pass reads them after its sends, and a pass with nothing owed sends nothing. A
// stalled architect is owed nothing the pass sends, so a gate that read no arm would answer no
// for every stall, and a quiet project's controller would hear of the stall only at the next fact
// of the project, which can be its own hourly tick. The gate reads the project's pause once,
// before anything else, and that read gives the arms their floor (readStallFloor). Past it the
// arms add, after a pause, one read of the launches the pause held for architects
// (readPauseLetOuts), and two statements, the read of the controller's registration and the read
// of their marks, only where an arm holds: the one arm that reads the record for a root itself
// (architectTookNothing) reads it only for a root its own terms already hold. While no
// controller is registered the pass tells nobody of a stall (wakeStalledArchitects), so the gate
// answers no for a stall alone then: a gate that answered yes would apply a fact a minute for as
// long as no controller runs, each one a processed event that nothing prunes.
//
// A paused project owes nothing here (decision 11 of dispatch://INFRA-3759): the pass promotes
// nothing while the pause stands (promoteHolds), so a fact the gate let through would do nothing,
// one a minute for as long as the pause lasts. The resume applies a pass of its own, and the sweep
// after it asks again.
func (a *Admission) OwesEntryWork(ctx context.Context, tx pgx.Tx) (bool, error) {
	floor, paused, err := a.readStallFloor(ctx, tx)
	if err != nil || paused {
		return false, err
	}
	issues, slots, facts, err := a.reread(ctx, tx)
	if err != nil {
		return false, err
	}
	owed, read, err := a.owedEntryRoles(ctx, tx, issues, slots, facts)
	if err != nil {
		return false, err
	}
	if len(owed) > 0 {
		return true, nil
	}
	stalls, err := a.architectStalls(ctx, tx, read, facts, floor, nil)
	if err != nil || len(stalls) == 0 {
		return false, err
	}
	registered, err := a.store.ControllerRegistered(ctx, tx, a.project)
	if err != nil || !registered {
		return false, err
	}
	return a.store.ArchitectStallsUntold(ctx, tx, stallMarks(stalls))
}

// owedEntryRoles is every role of an issue at work that is behind the entry its phases row names
// (workflow.OwedEntry), in the order its candidates are read. It is the selection half of the
// repair resumeSlottedWorkers sends, and the only statement of which roles are owed that work: a
// reader asking whether anything is owed asks this rather than restating it, so no second
// statement can disagree with the pass. Every role of the daemon is inside it, the root architect
// among them.
//
// Its domain is the issues holding one of the cap's slots and this project's issues that stand
// in a phase that keeps no row (record.LineFacts.InSlotlessPhase, the complement of the working
// phases the pass's own rule keeps a row in, which under the daemon's own rule are accepting,
// retro and merging), by key
// and once each, and beside them the architect of each root released for a wait
// (readSlotHolders says why that root's architect alone). The second half is there because such
// an issue holds no row at all now (record.KeepsSlot) and its worker would otherwise be repaired
// by nothing. The phases are read from the rule the pass was given rather than listed here, so a
// phase that moves into or out of a rule's KeepsSlotIn moves this repair with it. Every half is
// read from the records the pass already holds, so the widening costs no further read.
//
// The second half asks of the record the three terms the first half gets from the row itself,
// and it must: a slot row exists only while record.NeedsSlot holds, which is the issue in the
// workflow, its tree running and its root not lingering, while an issue in accepting, retro or merging is in
// the pass's records whatever Dispatch now says of it, since record.Store.LiveIssues answers
// every record that is not done, a parked one included. So this half asks each of the three —
// record.InWorkflow, whose own statement is record.OutOfWorkflow over the Dispatch status, and
// record.TreeRuns over the pass's lifecycles and gates, which carries the lingering root with
// it. Without them the repair's bound would be the retired-claim skip further down rather than
// the rule: a parked issue in its retro, or one whose tree left the workflow, would have its
// worker started again. The three sibling passes carry the same guard
// (startMidPhaseMembers, finishFencedStarts, workflow's booted).
//
// record.TreeRuns is asked of each such issue's root rather than record.RunningRoots being
// built once for the call: the set costs a walk of every record of the project and a map the
// size of it on every pass, while the rule asked here costs two map lookups for each issue
// in accepting, retro or merging, of which a healthy project has a handful.
//
// Only the role of each candidate's current phase is read (workflow.OpenPhaseRole, in the
// selection both repairs over phase workers make, phaseWorkerOf), and a root's own architect
// (rootArchitectOf), so a role the issue's phase has left is no part of the answer: its phases
// row keeps the entry it was last handed work on, and nothing here would ever send that work
// again. The phase's own worker is held where a worker of that issue has work where it stands
// (readSlotHolders, record.AtWork).
//
// The per-issue tests of the shared selection (phaseWorkerOf) bound the phase workers it names,
// and they are what keeps it from starting a worker the cap has not admitted:
// workflow.OpenPhaseRole answers no worker role for accepting, which is the own phase of the
// architect that owns the issue, so the selection passes over it, and record.AtWork is false for
// accepting. So the widening reaches the worker of a retro and of a merging, where the merger's
// own work ends the phase, and no other phase worker. An issue at work that holds no row because
// it waits in the line is not reached, which is the cap bypass this repair must never be.
//
// The root's own architect is the second role, and record.AtWork is not its reading: that rule
// answers false for a root at admitted once its design gate has opened, where the architect is
// still the only role at work on the root itself, and a relaunched architect of exactly such a
// root is the measured defect (dispatch://INFRA-2614). The architect of every root of the domain
// is weighed here, slotted, in accepting, retro or merging, or released for a wait, and the reach that decides
// which of them is owed anything is read once its claim is behind (architectReached,
// workflow.ArchitectAtWork): a root at admitted whatever its gate, a root a hand-over carried in
// while it stands in the run that hand-over carried it into, and a root past admitted under its
// own power, waiting on no person, while its architect's row names an entry a resume raised that
// its claim was not given. The second half of the domain is what reaches a root a hand-over
// carried in at accepting, retro or merging, which holds no row, and the third is what reaches a
// root releaseSlotsForWaits gave its row back for, such as a root at admitted whose design gate
// has opened while it waits on an open child, which holds none either. A root that reached
// accepting under its own power is weighed and passed over by that reach, since the notice that
// moved it there told its architect, unless the resume's entry is still owed to it. So the
// architects this names are exactly those the pass sends a start to, and the sweep's gate opens
// for no root the pass would send nothing.
//
// The slots are walked here, in the order the store answered, then the issues in accepting, retro or merging
// in the order the pass holds them, and the candidates those two halves make are what the
// selection is given: it is the tick's silent list that walks the project's live issues
// (silentWorkers). The roots released for a wait come last and give that selection nothing,
// since their architect alone is held. What the loop below asks beyond that selection is this
// repair's own judgement, and the skips are in the order a reader of the two repairs must see
// them.
//
// The whole selection asks the same statements whatever number of slots the project holds,
// which is what every pass of the project pays (the statements a fact's pass asks): the two
// batched reads of readSlotHolders, and, for the rare holder that is owed something, that
// holder's own reads — the architect's reach, and the phases row whose work is sent. It answers
// the reading beside the selection, since the stall reading the pass makes after its sends reads
// the same holders (architectStalls).
func (a *Admission) owedEntryRoles(ctx context.Context, tx pgx.Tx, issues []record.Issue, slots []record.Slot,
	facts record.LineFacts,
) ([]owedEntryRole, slotReading, error) {
	read, err := a.readSlotHolders(ctx, tx, issues, slots, facts)
	if err != nil {
		return nil, slotReading{}, err
	}
	owed, err := a.owedOf(ctx, tx, read)
	return owed, read, err
}

// slotHolder is one role of a candidate the rule weighs, an issue holding a slot, one past its
// review or a root released for a wait: the issue, its tree's root, the role and its claim token,
// in the shape both selections answer (phaseWorker: phaseWorkerOf and rootArchitectOf), and when
// the issue was admitted (record.Slot.AdmittedAt), which the gate arm of the stall reading weighs
// its bound from (architectStalls). An issue in accepting, retro or merging and a root released for a wait hold
// no slot and carry the zero time, and that arm reads a root at admitted with no step of its
// design gate registered alone, which no issue in accepting, retro or merging is, and no released root either:
// a root at admitted is released only once its design gate has opened (blockers.waitsOf).
type slotHolder struct {
	phaseWorker
	admittedAt time.Time
}

// slotReading is what one pass reads of the holders its selection names, once: the holders, the
// run of each holder's role (record.RoleRun) and the phase entry each holder's row names with the
// moment it was raised (record.Store.PhaseEntries). The selection of the owed roles (owedOf) and
// the stall reading of the roots' architects (architectStalls) both read it, so the pass that
// sends and then wakes asks the two batched statements once.
type slotReading struct {
	holders []slotHolder
	runs    map[claim.Token]record.RoleRun
	entries map[record.PhaseRole]record.PhaseEntry
}

// readSlotHolders is the reading owedEntryRoles weighs, over its domain: every slot of the
// project, then each issue of the project in accepting, retro or merging in a tree that runs, then the architect
// of each root of the project released for a wait in a tree that runs. Only the role of each
// candidate's current phase is held, and a root's own architect.
//
// Which role of a candidate the rule reaches is two questions, because two roles can stand on
// one issue. The phase's own worker is held where a worker of that issue has work where it
// stands: a slot no longer says one has — an issue keeps its row through phases no worker of it
// works (record.KeepsSlot) — so a candidate record.AtWork answers false for is passed over, at
// the skip the shared selection states (phaseWorkerOf). The caller hands that selection the facts
// of its own pass, so the gate AtWork is asked with is the issue's real one and the call stays
// honest against its contract, at one map lookup a candidate.
//
// A root released for a wait (record.Issue.ReleasedForWaitAt, releaseSlotsForWaits) holds no row
// and goes on working, and its architect alone is held: a root at admitted whose design gate has
// opened gives its row back while an open child goes first, and its architect is still the role
// at work there (dispatch://INFRA-2614 R5), so the repair and the stall reading weigh it as they
// did while it held the row. Its phase worker is not held: the repair's reach over phase workers
// is the issues holding a row, so a waiting issue's lost worker starts again with the row's return
// and never before it (TestReturningASlotStopsNothing). The mark stands only while the root needs
// a row (releaseSlots clears it), and the loop asks the same two terms the past-review half asks,
// so a parked root, or one whose tree left the workflow, is weighed by nothing here.
//
// It reads over at most the cap's worth of slotted issues, the handful in accepting, retro or merging and the
// roots released for a wait, which the cap does not bound, through one batched record.RoleRuns
// read rather than a read per candidate, and one batched read
// of the phase entry each holder's row names with the moment it was raised
// (record.Store.PhaseEntries), the zero entry where the record holds no row of that role. Both
// are one statement for the whole pass: every root's architect of the domain is weighed beside
// its phase's worker on every pass of the project, and a statement a candidate is hundreds of
// round trips a pass for a project running hundreds of trees
// (TestAFactAsksAsManyStatementsWhateverNumberOfSlotsIsHeld).
func (a *Admission) readSlotHolders(ctx context.Context, tx pgx.Tx, issues []record.Issue, slots []record.Slot,
	facts record.LineFacts,
) (slotReading, error) {
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	project, err := claim.ProjectToken(a.project)
	if err != nil {
		return slotReading{}, err
	}
	var read slotReading
	var keys []record.RoleRunKey
	hold := func(held phaseWorker, admittedAt time.Time) {
		read.holders = append(read.holders, slotHolder{phaseWorker: held, admittedAt: admittedAt})
		keys = append(keys, record.RoleRunKey{Token: held.token, Issue: held.issue.Key, Role: held.role, Generation: held.issue.Generation})
	}
	seen := make(map[string]struct{}, len(slots))
	take := func(issue record.Issue, admittedAt time.Time) {
		if _, already := seen[issue.Key]; already {
			return
		}
		seen[issue.Key] = struct{}{}
		if worker, selected := a.phaseWorkerOf(project, issue, byKey, facts); selected {
			hold(worker, admittedAt)
		}
		// The architect of a root is weighed beside the phase's worker, because two roles can
		// stand on one issue: a root at admitted holds no phase worker at all, and a root a
		// hand-over carried in has both its phase's worker and an architect that has still to
		// be told the tree is its own.
		if architect, selected := a.rootArchitectOf(project, issue); selected {
			hold(architect, admittedAt)
		}
	}
	for _, slot := range slots {
		if issue, recorded := byKey[slot.Issue]; recorded {
			take(issue, slot.AdmittedAt)
		}
	}
	for _, issue := range issues {
		if !facts.InSlotlessPhase(issue) || !record.InWorkflow(issue) {
			continue
		}
		root, rooted := byKey[issue.Tree]
		if !rooted || !record.TreeRuns(root, facts.Lifecycles[root.Key], facts.Gates[root.Key]) {
			continue
		}
		take(issue, time.Time{})
	}
	for _, issue := range issues {
		if issue.ReleasedForWaitAt == nil || !record.InWorkflow(issue) {
			continue
		}
		if _, already := seen[issue.Key]; already {
			continue
		}
		architect, selected := a.rootArchitectOf(project, issue)
		if !selected || !record.TreeRuns(issue, facts.Lifecycles[issue.Key], facts.Gates[issue.Key]) {
			continue
		}
		seen[issue.Key] = struct{}{}
		hold(architect, time.Time{})
	}
	if len(keys) == 0 {
		return read, nil
	}
	if read.runs, err = a.store.RoleRuns(ctx, tx, keys); err != nil {
		return slotReading{}, err
	}
	weighed := make([]string, 0, len(read.holders))
	for _, holder := range read.holders {
		weighed = append(weighed, holder.issue.Key)
	}
	if read.entries, err = a.store.PhaseEntries(ctx, tx, weighed); err != nil {
		return slotReading{}, fmt.Errorf("read the phase entries of %d slot holders: %w", len(weighed), err)
	}
	return read, nil
}

// owedOf is the roles of read that are owed their entry's work: owedEntryRoles' rule, applied to
// the reading the pass has already made. It decides how each is sent (entrySend) once, here.
func (a *Admission) owedOf(ctx context.Context, tx pgx.Tx, read slotReading) ([]owedEntryRole, error) {
	var owed []owedEntryRole
	for _, held := range read.holders {
		run := read.runs[held.token]
		entry := read.entries[record.PhaseRole{Issue: held.issue.Key, Role: held.role}]
		send := sendPhaseEntry
		if held.role == claim.RoleArchitect {
			send = resendArchitectStart
			// A root architect whose phases row names no entry, beside a claim the daemon holds,
			// is weighed here though OwedEntry reads nothing as owed of it: that rule compares two
			// numbers, and this role is the one the sweep hands an entry rather than resumes. The
			// claim is part of it because a claim is what says this architect was started: a root
			// with neither claim nor entry was started by nothing, and the promotion that starts
			// it is what hands it its first work. Every other role of a row with no entry is owed
			// none, and is passed over by that rule.
			if entry.Entry == 0 && run.Claim != nil {
				send = raiseArchitectEntry
			}
		}
		if send != raiseArchitectEntry && !workflow.OwedEntry(run, held.root, entry.Entry) {
			continue
		}
		if c := run.Claim; c != nil && c.Pending != nil && c.Pending.StillWorked(held.issue.Generation, held.issue.Phase, c.State) {
			continue
		}
		// A claim whose pod the idle rule released has no skip of its own, and the entry rule
		// above is why: while the entry it was given is the entry its row names, the rule
		// answers false and the release stands — its turn ended and its process was stopped,
		// which is the release working rather than a worker that lost its task, and the wake
		// brings the pod back with the one task that says so (workflow's wakeRow). One below
		// that entry is behind like any other claim: no wake is armed for work it was never
		// given, and sending that entry's task is what brings its pod back. The one other start
		// of a released claim is the boot's one-time repair of a merger the `awaiting_merge`
		// cutover left stopped (workflow's cutoverMergerRepair), which no wake would start.

		// A claim an operator suspended is passed over whatever entry it stands on: `legion
		// claims suspend` suspended that worker, and an operator's suspension holds against every
		// rule of the daemon's (supervise's RuleHolds, which no start the daemon queues gets
		// past). A claim the daemon itself suspended is repaired, which is what the idle rule's
		// release and the daemon's own suspend both leave behind.
		if c := run.Claim; c != nil && c.State == supervise.StateSuspended && c.SuspendedBy == supervise.SuspendedByOperator {
			continue
		}
		// A retired claim was stopped, and two stops reach this repair: an operator's `legion
		// claims stop`, and an agent's own report of its exit, which retires a worker's claim in
		// the same way (api's exit, supervise's table) though no shipped agent sends one. The
		// workflow's own retires never reach it: it retires a claim at its tree's close and at
		// its issue's close, and each of those takes the issue out of the live records or
		// lingers its root, which the loop above passes over already. Starting the phase again
		// here would hand the operator back, a second later, the worker they stopped. The ways
		// back are the workflow's own starts — the held phase's retry, the tree's re-admission —
		// and this skip touches none of them; no operator command starts a retired phase worker
		// at this head, and INFRA-956 is where one is added. What it costs: the issue keeps its
		// admission slot with no worker in it until one of those starts or a person moves the
		// issue, and a claim reading retired from an earlier run is passed over for as long as
		// it reads so.
		if c := run.Claim; c != nil && c.State == supervise.StateRetired {
			continue
		}
		// A claim failed on OpenAI's cyber-policy refusal is passed over too (a retired one is
		// above): no start the daemon makes by itself launches its agent again
		// (supervise.ErrHeldForCyberPolicy), so a start written here would be refused at the outbox
		// on every pass. The way back is the held phase's retry, which a person or the issue's
		// architect decides and which starts the agent in a new session (workflow's restart,
		// record.SuperviseRequest.Decided). The outbox's refusal of a start of the issue's phase is
		// what holds the issue for that architect (the daemon outbox's heldStartRefused), so
		// passing over the claim here leaves nobody untold.
		if c := run.Claim; c != nil && c.HeldForCyberPolicy() {
			continue
		}
		if held.role == claim.RoleArchitect {
			reached, err := a.architectReached(ctx, tx, held.issue, entry, run)
			if err != nil {
				return nil, err
			}
			if !reached {
				continue
			}
		}
		// The role's phases row is read here, once the role is owed its entry's work, so a
		// holder owed nothing costs no query of its own. The whole row is read and carried out
		// of here, so the row whose work is sent is read once per owed role, in the transaction
		// the entry above was read in.
		rows, err := a.store.Phases(ctx, tx, held.issue.Key)
		if err != nil {
			return nil, fmt.Errorf("read the phase rows of %s: %w", held.issue.Key, err)
		}
		row := record.PhaseRowOf(rows, held.role)
		owed = append(owed, owedEntryRole{Issue: held.issue, Role: held.role, Row: row, Send: send, Given: workflow.GivenEntry(run)})
	}
	return owed, nil
}

// architectReached says whether a root architect behind the entry its phases row names (entry) is
// owed a start, which is the architect's half of owedEntryRoles' rule. Three things stop it. A
// start already on its way is not owed twice (workflow.ArchitectStartPending): a row still queued,
// a claim still starting, or a task the claim has not confirmed that the row's entry has not
// superseded, one of the root's run at that entry or above it, or of no run or no entry, an
// operator's own and a wake's among them. A task of an earlier run, or of an entry below the
// row's, that carries an entry is passed over: the start this writes takes its place in the
// claim's hands. While such a row waits, every later pass reads it and names nothing, and that
// reading is pure, so it is asked first. A root outside the reach is owed nothing, and neither is
// an architect that cannot hear what a start would send (architectServed).
//
// The same three hold for an architect whose phases row names no entry at all, which the pass
// hands an entry rather than resumes (raiseArchitectEntry, workflow.Engine.StartArchitect).
func (a *Admission) architectReached(ctx context.Context, tx pgx.Tx, root record.Issue, entry record.PhaseEntry,
	run record.RoleRun,
) (bool, error) {
	if _, pending := workflow.ArchitectStartPending(root, entry.Entry, run); pending {
		return false, nil
	}
	return a.architectServed(ctx, tx, root, entry, run)
}

// architectServed is whether the own architect of a root the selection holds is one the sweep has
// work for and can tell it: the two readings the sweep's send (architectReached) and the first
// stall arm (architectTookNothing) both stand on. Its architect must be able to hear
// (workflow.ArchitectHearsSlotted, the slotted form of the daemon's one reading of that
// question, workflow.ArchitectHears: a root that lingers, one Dispatch has taken out of the
// workflow, and an issue that is not its tree's root, which is what leaves a child's own
// sub-architect claim outside the rule, since its start, which the child's first phase start
// queues (workflow's gateOpened), carries no task and raises no entry). The slotted form
// reads nothing, because root is one readSlotHolders holds: it holds one of the slots this pass
// read, or stands in accepting, retro or merging in a tree that runs, or was released for a wait in a tree that
// runs (record.TreeRuns, which no queued root does), and so waits for none. And the root must be
// inside the reach (workflow.ArchitectAtWork): a root standing at admitted, whatever its design
// gate, a root a hand-over carried in while it stands in the run that hand-over carried it into,
// and a root past admitted under its own power, waiting on no person, only while the entry its
// architect's row names (entry) is one the resume of a person's answer raised and its claim (run)
// has not been given, since its phase role owns the rest of its work. Both callers hand over the
// entry and the run the pass read for the whole selection (slotReading), so the first arm reaches
// the same roots the send does, which is R7's "the root is one the rule of R5 reaches"
// (dispatch://INFRA-2614). The pure reading is asked first, so the reach's read of the hand-over
// row is made only for a root that hears.
func (a *Admission) architectServed(ctx context.Context, tx pgx.Tx, root record.Issue, entry record.PhaseEntry,
	run record.RoleRun,
) (bool, error) {
	if !workflow.ArchitectHearsSlotted(root) {
		return false, nil
	}
	reached, err := workflow.ArchitectAtWork(ctx, tx, a.store, root, entry, run)
	if err != nil {
		return false, fmt.Errorf("read whether the architect of %s is the role at work there: %w", root.Key, err)
	}
	return reached, nil
}

// resumeSlottedWorkers sends every role owedEntryRoles names the work of the entry its phases
// row names, in the way the selection decided for it (entrySend). A phase worker is sent the task
// that entry was raised with, at that entry, so no completion in flight is refused for an entry
// the worker was never given, and the phase's resume task where the record holds no task of that
// entry (workflow's SendOwedEntry). A root's own architect is sent its start instead, which
// differs from a phase worker's in the two ways a root architect's start differs: the row waits
// for no admission slot, because a root architect's start takes no slot, whether the root holds
// its row, holds none in accepting, retro or merging (record.KeepsSlot) or gave it back for a wait
// (releaseSlotsForWaits), and a task the derivation wrote is derived again from the
// record rather than taken off the row, while a task the resume of a person's answer wrote is
// sent as written (workflow's SendArchitectStart, over record.PhaseRow.EntryVerbatim). Where the
// root's architect claim does not exist, that start is what creates it, exactly as admission's
// queued-root branch does.
//
// An architect whose phases row names no entry at all is handed one here, rather than resumed:
// the entry is raised with the task the derivation gives over the record as it stands, and the
// start carries that entry (workflow.Engine.StartArchitect, the call admission's own promotion
// makes). Two roots stand in that shape and no others. One is a root the daemon of the release
// before this one admitted after this release's migration had run and before this daemon served:
// that promotion wrote no entry, so its architect holds a claim with no row and the fill of the
// migration never saw it. The other is a root whose architect claim exists while no transition of
// this generation ever raised an entry for it. Raising here is safe because admission raises the
// entry for every root it starts: an entry of 0 beside a live architect claim is a root no
// transition of this release ever handed work to, so there is no work of an earlier entry that
// this could write over. Both shapes hold an architect claim, and the selection weighs no root
// without one (owedOf): a root with neither claim nor entry was started by nothing, and its
// promotion is what hands its architect the tree. A rollback that takes the entry columns away
// re-arms the migration's fill as well as the phase entry's own
// (20261113182000_architect_entry_upgrade.up.sql), so the roots standing at a roll-forward are
// filled again rather than raised here all at once. Every root of that selection is inside the
// sweep's reach (workflow.ArchitectAtWork) with a row that names no entry, so no resume's entry
// keeps it there, and such roots are the two the derivation writes a first task for, so every such
// raise raises an entry and sends its work.
//
// Every other role is left as it was: a phase worker whose row names no entry is owed none
// (workflow.OwedEntry), because no transition of its issue has ever handed that role work.
//
// The difference it closes is the one the record carries on its own: the transition raises the
// entry and writes its work in its own transaction, the outbox records the entry as given when
// it queues the task, and between the two nothing else reads the two numbers. So this repairs
// the slot whose worker has no task it was written for, and the worker of an issue past its
// review that holds no slot at all — a start row finished without acting while the issue stayed
// where it was, one the outbox fenced out, one finishFencedStarts ended earlier in this pass —
// and, by the same rule and without asking why, every other role whose claim never got the work
// its entry names, whatever state that claim is in. None of those sends marks its issue moved,
// a root architect's start and the raise of its missing entry among them: each is the daemon
// sending work the record already owes, a re-assertion (workflow's sendOwedEntry says why), so a
// stopped tree's quiet time is not reset by this repair or by the entry sweep that runs it each
// minute.
//
// It runs after the cap loop on every pass admission makes, and on the entry sweep's own fact
// (convergeEntries). Running after the fence repair rather than before it is what repairs a slot
// whose start row the fences rejected on the pass that rejected it, instead of a pass later.
//
// After the sends it reads the three stall arms over the architect of every root the selection
// holds (readSlotHolders), and wakes the controller for each arm no mark has told yet,
// while a controller is registered (architectStalls, wakeStalledArchitects), so a root the pass
// could still do something for is wholly served before the controller is woken about it. The
// arms read the holders as the pass read them before its sends, and the roots whose architect
// start the pass wrote are handed to them, so the second arm reads that start and its reason
// says what the pass did. floor is the floor the arms count from, which the caller read with the
// project's pause (readStallFloor) and found the project not paused, so this reads no pause.
func (a *Admission) resumeSlottedWorkers(ctx context.Context, tx pgx.Tx, issues []record.Issue, slots []record.Slot,
	facts record.LineFacts, floor stallFloor,
) error {
	owed, read, err := a.owedEntryRoles(ctx, tx, issues, slots, facts)
	if err != nil {
		return err
	}
	var sent map[string]bool
	startedArchitect := func(root string) {
		if sent == nil {
			sent = map[string]bool{}
		}
		sent[root] = true
	}
	for _, role := range owed {
		behind := func() {
			a.log.Warn("admission: a role was behind the entry its phases row names, so that entry's work is sent again",
				"issue", role.Issue.Key, "role", role.Role, "phase", role.Issue.Phase, "entry", role.Row.Entry, "given", role.Given)
		}
		switch role.Send {
		case raiseArchitectEntry:
			if err := a.engine.StartArchitect(ctx, tx, role.Issue); err != nil {
				return fmt.Errorf("raise the phase entry of the architect of slotted root %s: %w", role.Issue.Key, err)
			}
			startedArchitect(role.Issue.Key)
			store.OnCommit(ctx, func() {
				a.log.Warn("admission: a root architect's phases row named no entry, so the pass raised one and sent its work",
					"issue", role.Issue.Key, "role", role.Role, "phase", role.Issue.Phase, "given", role.Given)
			})
		case resendArchitectStart:
			if err := a.engine.SendArchitectStart(ctx, tx, role.Issue, role.Row); err != nil {
				return fmt.Errorf("start the architect of slotted root %s again: %w", role.Issue.Key, err)
			}
			startedArchitect(role.Issue.Key)
			store.OnCommit(ctx, behind)
		case sendPhaseEntry:
			if err := a.engine.SendOwedEntry(ctx, tx, role.Issue, role.Role, role.Row); err != nil {
				return fmt.Errorf("start the worker of slotted issue %s again: %w", role.Issue.Key, err)
			}
			store.OnCommit(ctx, behind)
		}
	}
	stalls, err := a.architectStalls(ctx, tx, read, facts, floor, sent)
	if err != nil {
		return err
	}
	return a.wakeStalledArchitects(ctx, tx, stalls)
}

// convergeEntries is the entry sweep's whole work (intake.EntrySweep, the fact the daemon's own
// ticker applies): it reads the project's issues, its slots and its line facts as every pass
// reads them (reread), sends each role behind the entry its phases row names that entry's work,
// and wakes the controller for each stalled architect no wake has told yet
// (resumeSlottedWorkers). It promotes nothing — a tick already does that, on a period of its own
// — so what the sweep adds is that convergence and those wakes, run when no fact of the project
// arrives to run them.
func (a *Admission) convergeEntries(ctx context.Context, tx pgx.Tx) error {
	// A paused project sends no role its entry's work (decision 11 of dispatch://INFRA-3759), as
	// its promotion sends none (promoteHolds). The read that says so gives the stall arms their
	// floor, so the sweep reads the pause once.
	floor, paused, err := a.readStallFloor(ctx, tx)
	if err != nil || paused {
		return err
	}
	issues, slots, facts, err := a.reread(ctx, tx)
	if err != nil {
		return err
	}
	return a.resumeSlottedWorkers(ctx, tx, issues, slots, facts, floor)
}

// finishFencedStarts is the repair of a held start row that has outlived the fences the outbox
// would have run it under: its issue has left the workflow, its tree lingers or has left, or the
// issue has moved past the phase or the generation the row names. The outbox applies those
// fences when it runs a row, but a row waiting for a slot may never be run at all, and one that
// could never act would keep its issue in the waiting line for ever and take a slot from work
// that could use it. Finishing the row deletes it, exactly as the outbox's own fence would. The
// daemon's store reads the same phase and generation fence where a waiting start silences a
// backstop (store.PodRecords.WaitsForItsSlot), since a row this repair will finish brings no
// worker back: a change to this fence changes that one too.
//
// A tree that lingers is read by the absence of its root rather than by its root's deadline: a
// lingering root is in phase done at a status that is not todo, which is exactly what the
// project's live read leaves out (record.Store.LiveIssues), so its record is not here to carry
// a linger at all. Reading the zero record's deadline answered false for every parked tree,
// which is the one case this repair exists for: the members of a parked tree keep their phase
// and their generation, so the row passed every other fence, and at the tree's re-admission its
// issue stood in the line's first part with the old row's place and took a slot ahead of its
// own root.
//
// A row the fences keep is weighed once more, against the claim it would start: a stop that
// outlived the row's own start ends the row too, since the row's retry would otherwise start
// the stopped worker again the moment a slot came (stopOutlivedStart). That second read is one
// batched RoleClaims call over the rows the fences kept, and no read at all when they kept none:
// the rule reads nothing of the queued rows beside those claims.
//
// It runs before the waiting line is judged, and so before the cap loop hands out any slot
// (promoteHolds). That is what keeps the line and this repair from disagreeing: the line offers
// a slot to any unslotted issue that owes a start and reads no claim at all (record.Waiting), so
// a row ended after the line was built would leave its own issue holding a slot with no worker
// in it while the issue behind it in the line waited for one. Deciding the rows first leaves
// that issue out of the line instead, and the slot goes to the next issue in it. It answers
// whether it ended any row, which is what tells the pass to take its shared reads again.
func (a *Admission) finishFencedStarts(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts record.LineFacts) (bool, error) {
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	project, err := claim.ProjectToken(a.project)
	if err != nil {
		return false, err
	}
	type owedRow struct {
		owed  record.OwedStart
		token claim.Token
	}
	finished := false
	kept := make([]owedRow, 0, len(facts.OwedRows))
	tokens := make([]claim.Token, 0, len(facts.OwedRows))
	for _, owed := range facts.OwedRows {
		issue, recorded := byKey[owed.Issue]
		root, rooted := byKey[issue.Tree]
		if recorded && rooted && !record.OutOfWorkflow(issue.Status) && !root.Lingers() &&
			owed.Generation == issue.Generation && owed.Phase == issue.Phase {
			// The fences keep this row, so it stands for the stop rule below. A row whose issue
			// and role yield no claim token has no run to read, as the slot repair's holders have
			// none (resumeSlottedWorkers): it stays owed and the rule passes over it rather than
			// failing the whole fact.
			token, err := claim.NewToken(project, owed.Issue, owed.Role)
			if err != nil {
				continue
			}
			kept = append(kept, owedRow{owed: owed, token: token})
			tokens = append(tokens, token)
			continue
		}
		// No live lease holds the row (record.Store.OwedStarts), so the token it carries is
		// either none at all or the one a dead lane left on it, and the delete names that token:
		// a lane that took the row between this pass's read and this write keeps it, and the
		// next pass repairs what is left.
		if err := a.store.FinishOutbox(ctx, tx, owed.ID, owed.LeaseToken); err != nil {
			return finished, fmt.Errorf("finish the start %s owed a worker it can no longer reach: %w", owed.Issue, err)
		}
		finished = true
		store.OnCommit(ctx, func() {
			a.log.Info("admission: finished a held start whose issue has moved past it", "issue", owed.Issue, "row", owed.ID,
				"role", owed.Role, "phase", owed.Phase, "generation", owed.Generation)
		})
	}
	if len(tokens) == 0 {
		return finished, nil
	}
	held, err := a.store.RoleClaims(ctx, tx, tokens)
	if err != nil {
		return finished, err
	}
	for _, keptRow := range kept {
		owed := keptRow.owed
		if !stopOutlivedStart(held[keptRow.token], owed) {
			continue
		}
		if err := a.store.FinishOutbox(ctx, tx, owed.ID, owed.LeaseToken); err != nil {
			return finished, fmt.Errorf("finish the start %s owed a stopped worker: %w", owed.Issue, err)
		}
		finished = true
		store.OnCommit(ctx, func() {
			a.log.Info("admission: finished a held start owed a stopped worker", "issue", owed.Issue,
				"row", owed.ID, "role", owed.Role, "phase", owed.Phase, "generation", owed.Generation)
		})
	}
	return finished, nil
}

// stopOutlivedStart says whether owed is a start the stop of its worker outlived, and so a row
// that would only ever start a stopped worker. Its three terms together say the stop is the whole
// of this row's story, and held, the claim as record.Store.RoleClaims reads it (nil for a role
// the store holds no claim of), carries all three: its state, the write mark its retirement took,
// and the task it holds. That read runs after the owed rows the fences kept, so a stop made
// between the two reads is in the claim read here.
//
// That second term compares write marks and not row ids, and both sides take their numbers from
// one sequence (store's putClaim, whose doc holds what that sequence orders and what it does
// not): a row's write mark is the write that last set its payload, and the claim's is the write
// that recorded its retirement. A creation time cannot stand in for it, because
// record.Store.EnqueueStart writes the start an issue already owes as a row of its own carrying
// the earlier row's creation time — so a start written after a stop can carry a creation time
// from before it, which is exactly what `legion claims stop` and then `legion claims retry`,
// with no admission pass between them, writes.
func stopOutlivedStart(held *record.RoleClaim, owed record.OwedStart) bool {
	// The retired state says the worker was ended.
	if held == nil || held.State != supervise.StateRetired {
		return false
	}
	// A row at or below the mark took its number before the retirement took its own (store's
	// putClaim says what that order does and does not show of two writes in flight together),
	// so the stop is the whole of its story: whatever it was written for, the worker it would
	// reach is gone, and its only effect when a slot came would be to start a stopped worker.
	//
	// A row above the mark took its number after it, so it is the workflow's own next start of
	// this worker — a re-admitted tree's promotion, the held phase's retry — and the row is the
	// whole of that start. Ending it would end the way back: the issue would leave the waiting
	// line, since the line's first part is the issues that owe a start (record.Waiting), and
	// nothing would start that phase again, the slot repair being no backstop, as it skips a
	// retired claim on purpose (resumeSlottedWorkers).
	//
	// A claim an earlier release retired carries 0, below every write mark, so none of its owed
	// rows is ended and its stop reads as that release read it. What the rule still does not
	// reach is an owed row a lane has already leased, which is not in this read at all
	// (record.Store.OwedStarts).
	if owed.WriteMark > held.RetiredAtWriteMark {
		return false
	}
	// A task the claim still works would say the claim kept the work of this very row — the row
	// gave it that task on the run above and the stop came before the claim was done with it — so
	// the row is not simply a start the stop outlived.
	if p := held.Pending; p != nil && p.StillWorked(owed.Generation, owed.Phase, held.State) {
		return false
	}
	return true
}

// reenterStrandedChildren re-enters, at the first run of candidate's tree, every tree member
// admission left recorded todo with its previous run's phase at done: a reopen admission declined
// to re-enter for want of the label while the tree was not live, which recordObservation otherwise
// records with the phase untouched. That first run (returnOwedMembers, its one caller) is what
// makes the tree live for these, since nothing else reaches them afterward: Engine.ReenterChild only
// re-enters a live tree's reopened child on its own live event, and a later todo observation of
// one already todo changes nothing. The synthetic fact carries the child's own already-recorded
// fields: no live event reopened it, so there is nothing newer to apply, only its own next
// generation to start.
//
// What a child waits on is deliberately not read here either: this re-enters a child whose
// previous run finished (phase.Done), and Engine.ReenterChild's own entry withholds the start of
// one that waits on unfinished work.
func (a *Admission) reenterStrandedChildren(ctx context.Context, tx pgx.Tx, candidate record.Issue, issues []record.Issue) error {
	for _, child := range issues {
		if child.Key == candidate.Key || child.Tree != candidate.Tree {
			continue
		}
		if child.Status != "todo" || child.Phase != phase.Done {
			continue
		}
		if err := a.engine.ReenterChild(ctx, tx, child, workflow.RecordedFact(child)); err != nil {
			return fmt.Errorf("re-enter stranded child %s: %w", child.Key, err)
		}
	}
	return nil
}

func (a *Admission) enqueue(ctx context.Context, tx pgx.Tx, issue string, payload record.OutboxPayload, now time.Time) error {
	row, err := record.NewOutboxRow(issue, payload, now)
	if err != nil {
		return fmt.Errorf("build %s outbox row for %s: %w", payload.OutboxKind(), issue, err)
	}
	if err := a.store.Enqueue(ctx, tx, row); err != nil {
		return fmt.Errorf("enqueue %s outbox row for %s: %w", payload.OutboxKind(), issue, err)
	}
	return nil
}

func nextSlotIndex(slots []record.Slot) int {
	used := make(map[int]struct{}, len(slots))
	for _, slot := range slots {
		used[slot.Index] = struct{}{}
	}
	for index := 0; ; index++ {
		if _, present := used[index]; !present {
			return index
		}
	}
}

func sameParent(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
