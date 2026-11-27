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
	// random source (workflow.Engine's ExperimentSource), the one source of the daemon's draws.
	experiments []config.Experiment
	// stalledTreeNotice is `stalled_tree_notice_hours` (UseStalledTreeNotice), how long a tree
	// of this project may be quiet before the pass tells the project's controller once that it
	// has stopped (tellStalledTrees). Zero is no bound and tells nobody, which is the
	// admission a test that configures none builds: a daemon always sets a positive number,
	// since the loader refuses zero for a key in hours and gives six hours to a file that
	// names none (config.Config.StalledTreeNotice).
	stalledTreeNotice time.Duration

	// mu guards pending, stamps, target, catchingUp, the catch-up releases, unknownChains,
	// retroHeldReads and retroHeldPass, the only Admission state a caller outside its own
	// transactions touches: the daemon's boot-owned poll calls Held on every tick, outside any
	// transaction and the database's own advisory fact lock, concurrently with whatever release
	// call currently holds both while it clears them, and a commit hook that runs late clears or
	// writes them on the commit watcher's goroutine (clearReleased, rememberHeldRetro).
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
	// unknownChains is every key a pass found on a Dispatch chain the copy cannot read to its top
	// since the daemon last took them (noteUnknownChain, UnknownChains), guarded by mu, and
	// unknownWake what the daemon wakes its re-read with when a key is noted (OnUnknownChain), nil
	// for none.
	unknownChains map[string]struct{}
	unknownWake   func()
	// keeper keeps the tree lifecycles the stored trees name open for the release before, once a
	// pass (record.TreeKeeper, promoteHolds).
	keeper *record.TreeKeeper

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
	// catchUpReleases counts the catch-up holds released (ReleaseCatchUp), and catchUpPassed is
	// the count the last committed pass that ran saw (promoteHolds): the difference is the pass
	// the pass rule (Apply) owes the releases.
	catchUpReleases, catchUpPassed uint64

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

	// p0Configured is legion.yaml's `admission_p0_bound` (UseP0Bound,
	// config.Config.AdmissionP0Bound): how many P0 issues may hold a slot at once, outside the cap,
	// while no value set while the daemon runs stands, and nil for no bound, which is what an
	// admission nobody registered a bound with holds. Every pass reads the bound in force again
	// (p0BoundAt), so a value set while the daemon runs is in force from its own fact's pass on.
	p0Configured *int
}

// ReportCapabilities registers open, whose answer every tick controller notice carries as its
// openCapabilities: the daemon's report of the deployment capabilities with no decision, so the
// controller's periodic turn names each gap in the day's report. No other wake carries it.
func (a *Admission) ReportCapabilities(open func() []string) {
	a.openCapabilities = open
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

// UseClock registers the daemon's one clock, which every moment this admission stamps and every
// bound it reads is measured on, as the workflow engine's own (workflow.Config.Clock) and the
// state's read (internal/daemon's source.State) are: the architect-stalled arms the state serves
// are then judged at the instant the rest of the state is read at. Without it the admission reads
// the wall clock, as every test that builds one by hand leaves it.
func (a *Admission) UseClock(now func() time.Time) {
	a.now = now
}

// UseP0Bound registers legion.yaml's P0 bound (`admission_p0_bound`, config.Config.AdmissionP0Bound),
// nil where the file gives none: the bound every pass admits under while no value set while the
// daemon runs stands (p0BoundAt). It is a setter rather than a parameter of New for the reason
// UseWorkerSilence is: every test that admits anything builds an admission, and most of them
// configure no bound.
func (a *Admission) UseP0Bound(configured *int) {
	a.p0Configured = configured
}

// p0BoundAt is the P0 bound this pass admits under, read through tx: the value the controller or
// the operator set while the daemon runs where one stands, a number or none, else legion.yaml's
// (store.EffectiveP0Bound). nil is no bound. It is read on every pass rather than held, so a change
// is in force from the pass of its own fact on (recordP0BoundChange), with no restart, and one made
// before a restart is in force from the boot's first pass.
func (a *Admission) p0BoundAt(ctx context.Context, tx pgx.Tx) (*int, error) {
	standing, found, err := store.P0BoundTx(ctx, tx, a.project)
	if err != nil {
		return nil, err
	}
	return store.EffectiveP0Bound(a.p0Configured, standing, found), nil
}

// recordP0BoundChange appends the change a P0BoundChanged fact carries, for this admission's
// project (store.RecordP0BoundChangeTx), and logs it once it commits: who made it, why, what it
// did, and the bound in force before and after it. It refuses nothing the route did not refuse
// first: the routes refused a bound below 1 or above 2147483647 and a change with no reason
// before the fact.
func (a *Admission) recordP0BoundChange(ctx context.Context, tx pgx.Tx, change store.P0BoundChange) error {
	before, err := a.p0BoundAt(ctx, tx)
	if err != nil {
		return err
	}
	change.Project = a.project
	recorded, err := store.RecordP0BoundChangeTx(ctx, tx, change)
	if err != nil {
		return err
	}
	after := store.EffectiveP0Bound(a.p0Configured, recorded, true)
	a.logOnCommit(ctx, "admission: the P0 bound changed", "change", recorded.ID, "mode", string(recorded.Mode),
		"by", string(change.By.Kind), "by_session", change.By.Session, "reason", change.Reason,
		"p0_bound_before", boundWords(before), "p0_bound", boundWords(after), "p0_bound_configured", boundWords(a.p0Configured))
	return nil
}

// boundWords is a P0 bound as a log line writes it: the number, or "none".
func boundWords(bound *int) any {
	if bound == nil {
		return "none"
	}
	return *bound
}

var _ intake.Handler = (*Admission)(nil)

// New creates an admission handler for one Dispatch project. engine is the workflow's own, shared
// with intake's other handler, so a promotion's starts and a listing's records go through the one
// copy of the workflow's rules.
func New(store record.Store, engine *workflow.Engine, cap int, project string, log *slog.Logger) *Admission {
	if log == nil {
		log = slog.Default()
	}
	return &Admission{store: store, engine: engine, cap: cap, project: project, log: log, now: time.Now,
		pending: make(map[string]heldSummary), unclassifiedLogged: make(map[string]uint64),
		unknownChains: make(map[string]struct{}), keeper: record.NewTreeKeeper(log), retroHeldReads: make(map[string]retroHeldRead)}
}

// takeTreeDraw settles the tree-unit experiments of key at its first start (R17 of
// dispatch://INFRA-3320): each experiment the nearest issue above it in Legion's work holds a row
// for is copied from that issue (record.Store.CarryAssignments), and key draws its own for each
// experiment it holds no row of after that, and only those, with the engine's random source, so
// the source is read for no answer that would not be written. Both writes are write-once, so a
// draw stays as it was made, and a later change of parent changes no answer.
func (a *Admission) takeTreeDraw(ctx context.Context, tx pgx.Tx, key string, chains *record.Chains) error {
	if _, err := a.store.CarryAssignments(ctx, tx, key, chains.Above(key)); err != nil {
		return err
	}
	held, err := a.store.AssignmentsFor(ctx, tx, []string{key})
	if err != nil {
		return err
	}
	var rest []config.Experiment
	for _, declared := range a.experiments {
		if declared.Unit != config.ExperimentTree || slices.ContainsFunc(held[key], func(row record.Assignment) bool { return row.Experiment == declared.Name }) {
			continue
		}
		rest = append(rest, declared)
	}
	return experiment.DrawAndRecord(ctx, tx, a.store, key, rest, config.ExperimentTree, a.engine.ExperimentSource(), a.now())
}

// Apply records a todo observation of an unrecorded root handed to Legion and every newer
// observation of a recorded issue, queues the todo of an unrecorded root in triage whose legion
// label a person set (queuePersonHandover) and wakes the controller for one whose label a session
// set (a controller notice, not a record), and promotes the waiting line while capacity remains,
// releasing the slot of every issue that no longer needs one on the way (promoteHolds) and waking
// the controller when a slot it released is still free after that (wakeForFreeSlot). The workflow
// handler runs first: it records an issue under a parent in Legion's work and moves a recorded
// issue on its status, leaving admission to record only a still-unrecorded root. Admission refuses
// no fact. The entry sweep (intake.EntrySweep) is the one fact that promotes nothing: it converges
// the phase entries and does no more (convergeEntries).
//
// The pass rule, which every other comment of this package refers to. A fact runs the pass where
// its transaction wrote a record admission reads (record.RecordsWritten), and also where a
// catch-up hold was released since the last pass that ran (ReleaseCatchUp): the next fact runs
// the pass that release owes, whatever it wrote, and a pass a pause skipped pays nothing of it.
// The Dispatch consumer's position runs it whatever it wrote, whether or not it releases a hold
// (release), and so do the controller tick and the facts passFact names. Any other fact that
// wrote nothing runs none (promoteWritten).
func (a *Admission) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	released, err := a.applyFact(ctx, tx, fact)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, a.wakeForFreeSlot(ctx, tx, released)
}

// OpenFact and SettleFact are the engine's (intake.Settler): admission writes issues through it —
// a promotion's starts — so the runs those writes end are settled whichever of the two a fact
// applies through, once. Every admission holds an engine (New).
func (a *Admission) OpenFact(ctx context.Context) context.Context {
	return a.engine.OpenFact(ctx)
}

func (a *Admission) SettleFact(ctx context.Context, tx pgx.Tx) error {
	return a.engine.SettleFact(ctx, tx)
}

// applyFact is Apply's work, and reports the issues whose slots it released.
func (a *Admission) applyFact(ctx context.Context, tx pgx.Tx, fact intake.Fact) ([]string, error) {
	if position, ok := fact.(intake.DispatchConsumerPosition); ok {
		return a.release(ctx, tx, position)
	}
	if _, ok := fact.(intake.EntrySweep); ok {
		// The sweep converges the entries and does no more: it promotes nothing, since the pass
		// runs by the pass rule (Apply) without it, and it releases no slot, so it reports none.
		return nil, a.convergeEntries(ctx, tx)
	}
	if _, ok := fact.(intake.ControllerTick); ok {
		_, returned, err := a.promote(ctx, tx)
		if err != nil {
			return nil, err
		}
		// What the project waits on, what nothing starts and whose phase worker nothing will
		// prompt are read here, after the promotion this tick made, from one read of the
		// project (tickRows): the first two are two assemblies of one judgement of the line, and they are built whether or not a controller is
		// registered to be told. The wake carries them as it carries the open capabilities, and
		// judges nothing a second time.
		held, blocked, silent, err := a.tickRows(ctx, tx)
		if err != nil {
			return nil, err
		}
		// The tick wakes the controller whatever the slots: with every slot taken, it is the only
		// wake a tree waiting on a refused root claim gets, since only the controller's recheck of
		// that claim moves it, and it is the turn that posts the day's report.
		_, err = a.wakeController(ctx, tx,
			record.ControllerNotice{Kind: record.TickNotice, Held: held, Blocked: blocked, Silent: silent}, a.project, a.now())
		return returned, err
	}
	if changed, ok := fact.(intake.P0BoundChanged); ok {
		// The bound is written before the pass, in the fact's own transaction, so this very pass
		// admits under it: a bound raised or set to none starts the P0 issues it lets in now, and a
		// lower one starts no P0 issue while it is at the bound and stops nothing that runs.
		if err := a.recordP0BoundChange(ctx, tx, changed.Change); err != nil {
			return nil, err
		}
		_, returned, err := a.promote(ctx, tx)
		return returned, err
	}

	observation, ok := fact.(intake.DispatchIssue)
	// A view of an issue the daemon records, prepared in place of no event (intake's
	// settleObserved), is a reading for the copy alone: nothing here decides on it. It is still a
	// look at what Legion could now start where the copy it wrote changed a row, since that can
	// end a wait, as the pass rule (Apply) has it.
	if !ok || observation.Observed {
		promote := a.promoteWritten
		if passFact(fact) {
			promote = a.promote
		}
		_, returned, err := promote(ctx, tx)
		return returned, err
	}
	stored, err := a.store.Issue(ctx, tx, observation.Key)
	if err != nil {
		return nil, fmt.Errorf("read admission issue %s: %w", observation.Key, err)
	}
	if observation, err = workflow.SettleRetainedRepository(observation, stored); err != nil {
		return nil, err
	}
	if stored != nil {
		if err := a.applyObservation(ctx, tx, *stored, observation); err != nil {
			return nil, err
		}
		_, released, err := a.promoteWritten(ctx, tx)
		return released, err
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
				return nil, err
			}
		} else if err := a.enqueue(ctx, tx, observation.Key, record.ControllerNotice{Kind: record.TriageNotice}, a.now()); err != nil {
			return nil, err
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
	if observation.Status != "todo" {
		a.refreshHeldSummary(observation)
		_, released, err := a.promoteWritten(ctx, tx)
		return released, err
	}
	// An unrecorded todo issue on a chain the copy cannot read to its top starts nothing and is
	// recorded by nothing (R16 of dispatch://INFRA-3320): its key is noted for the daemon to read
	// Dispatch again (UnknownChains), and a later reading that makes the chain known decides it.
	chains, err := record.ReadChainsOf(ctx, tx, a.store, a.project, []string{observation.Key})
	if err != nil {
		return nil, err
	}
	if !chains.Chain(observation.Key).Known() {
		a.noteUnknownChain(ctx, observation.Key)
		a.refreshHeldSummary(observation)
		_, released, err := a.promoteWritten(ctx, tx)
		return released, err
	}
	if !handed {
		a.log.Debug("admission: not handed to Legion", "issue", observation.Key, "label", dispatch.LegionLabel)
		a.refreshHeldSummary(observation)
		// Promotion comes before the slot is judged: a root this look admits takes a slot that
		// was free before it, and the wake must name a slot still free once this fact commits,
		// never one the same transaction has just filled.
		_, released, err := a.promoteWritten(ctx, tx)
		if err != nil {
			return nil, err
		}
		// An issue in todo that nobody handed to Legion wakes the controller while a slot stands
		// free; the controller takes nothing for it, since only a person hands Legion work
		// (skill://legion-controller, "Free slots"). The notice waits todoWakeDelay, so the
		// events of one edit burst find it pending and fold into it.
		free, err := a.slotFree(ctx, tx)
		if err != nil || !free {
			return released, err
		}
		_, err = a.wakeController(ctx, tx, record.ControllerNotice{Kind: record.TodoNotice}, observation.Key, a.now().Add(todoWakeDelay))
		return released, err
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
		return released, err
	}
	if err := a.putNewRoot(ctx, tx, observation, chains); err != nil {
		return nil, err
	}
	_, released, err := a.promoteWritten(ctx, tx)
	return released, err
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
// architect owed that task now waits on a person (workflow's ReconcileAwaiting); and the
// confirmation of an issue's own cleanup (intake.IssueCleanedUp) stands for the lifecycle row the
// confirmation wrote outside the fact, which a pass that leaves an issue whose cleanup is reserved
// in the line waits for (promote), so the issue's next start takes a free slot at once.
func passFact(fact intake.Fact) bool {
	switch fact.(type) {
	case intake.ProjectResumed, intake.CatchUpApplied, intake.DaemonBooted, intake.TaskConfirmed, intake.IssueCleanedUp:
		return true
	}
	return false
}

// noteUnknownChain adds key to the issues whose Dispatch chain the copy cannot read to its top
// (UnknownChains), once the fact that found it commits: a fact rolled back found nothing. The
// daemon's re-read is woken then (OnUnknownChain).
func (a *Admission) noteUnknownChain(ctx context.Context, key string) {
	store.OnCommit(ctx, func() {
		a.mu.Lock()
		a.unknownChains[key] = struct{}{}
		wake := a.unknownWake
		a.mu.Unlock()
		if wake != nil {
			wake()
		}
	})
}

// noteUnknownChains notes for the daemon's re-read (noteUnknownChain) each of issues, the pass's
// records, whose Dispatch chain chains, the pass's own reading (record.ReadLineFacts, which walks
// from every one of them), holds unknown, in Legion's work or out of it: R16 of
// dispatch://INFRA-3320 reads Dispatch again for an issue on an unknown chain until the chain is
// known, whatever the issue's phase, and what waits on that chain waits for that read, the telling
// a re-parent seed owes among them (workflow's ReparentChainPending). It sends no statement.
func (a *Admission) noteUnknownChains(ctx context.Context, issues []record.Issue, chains *record.Chains) {
	for _, issue := range issues {
		if !chains.Chain(issue.Key).Known() {
			a.noteUnknownChain(ctx, issue.Key)
		}
	}
}

// OnUnknownChain sets what a noted key wakes (noteUnknownChain): the daemon's re-read of the
// chains the copy cannot read to their top, which takes them (UnknownChains). wake must not block.
func (a *Admission) OnUnknownChain(wake func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unknownWake = wake
}

// UnknownChains takes every key an admission pass found on a Dispatch chain the copy cannot read to
// its top (R16 of dispatch://INFRA-3320: a row missing, a key repeated, or more than 32 links) since
// the last take, in key order, and empties the set: the daemon reads Dispatch again for each, the
// issue and each link of its chain, outside any decision, until the chain is known. A key found
// again after a take is noted again.
func (a *Admission) UnknownChains() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	keys := slices.Sorted(maps.Keys(a.unknownChains))
	clear(a.unknownChains)
	return keys
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
// nobody.
//
// The slot it speaks of is one of the cap's, so it counts the holders the cap counts: a P0 holder
// takes none of the cap's slots (record.HoldersOf), and a P0 candidate the line would start is no
// taker of one either, since it starts outside the cap (capLets). The promotion, the stall
// measure's free-slot term (lineCanMove) and this read count the same holders of one record.
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
	facts, err := record.ReadLineFacts(ctx, tx, a.store, a.project, issues, a.engine.GatedSteps())
	if err != nil {
		return false, err
	}
	own := ownSlots(issues, slots)
	rows, facts, line, _, err := blockers.JudgeLine(ctx, tx, a.store, a.project, issues, own, facts, nil)
	if err != nil {
		return false, err
	}
	held := record.HoldersOf(issues, own)
	taking := 0
	for _, candidate := range line {
		if len(blockers.LineWaitsOn(rows, facts.Chains, candidate)) == 0 && slotFor(candidate) && !candidate.Issue.StartsNow() {
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
// read of the project and put on the notice before it reached here (applyFact, tickRows): every
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

// tickRows is what the tick controller notice carries beside the open capabilities: every issue
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
func (a *Admission) tickRows(ctx context.Context, tx pgx.Tx) ([]record.HeldIssue, []record.BlockedIssue, []record.SilentIssue, error) {
	issues, slots, facts, err := a.reread(ctx, tx)
	if err != nil {
		return nil, nil, nil, err
	}
	silent, err := a.silentWorkers(ctx, tx, issues, facts)
	if err != nil {
		return nil, nil, nil, err
	}
	own := ownSlots(issues, slots)
	rows, facts, line, dropped, err := blockers.JudgeLine(ctx, tx, a.store, a.project, issues, own, facts, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	held := record.HeldList(issues, own, line, dropped, facts)
	a.logUnclassified(held, issues)
	return held, blockers.Blocked(rows, issues, line, facts), silent, nil
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
// the record shows under way under a gate root in Legion's work (record.Chains.GateRoot). project
// is the project's claim token (claim.ProjectToken), and byKey is the project's live records by
// key, which the gate roots are read from and which the slot repair walks its slots and its
// issues in accepting, retro or merging through.
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
	if !facts.AtWork(issue) {
		return phaseWorker{}, false
	}
	root, rooted := byKey[facts.Chains.GateRoot(issue.Key)]
	if !rooted {
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
// and whether it is one: issue is its own gate root on the pass's chains (record.Chains.GateRoot:
// in Legion's work and holding its own gate), and its key yields a claim token. Which of these
// roots the rule reaches is read once the claim is behind (architectReached), so a root owed
// nothing costs no read of its own.
func (a *Admission) rootArchitectOf(project string, issue record.Issue, chains *record.Chains) (phaseWorker, bool) {
	if chains.GateRoot(issue.Key) != issue.Key {
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
		gate := facts.Gates[held.root.Key]
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
// Each summary reads the Dispatch chains as the records stand when it applies (applySummary,
// putNewRoot), so a summary that records an issue is on the chains a later summary reads: the
// listing's parents come first (parentsFirst), and a chain built once for the whole listing would
// miss every record the listing itself writes.
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
	caughtUp := position.Reached(target)
	deferred := make(map[string]intake.DispatchIssue)
	var refused []string
	for _, summary := range parentsFirst(summaries) {
		outcome, err := a.applySummary(ctx, tx, summary, !caughtUp)
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

// putNewRoot records an unrecorded issue handed to Legion in todo as a root, on the repository its
// observation resolves to and that repository's default branch (workflow's
// ResolveIssueRepository, which takes the refusal the observation's own preparation read from
// GitHub when it carries one). It records one only where chains, the pass's reading of the
// Dispatch chains with the issue named, shows the chain known and no issue above it in Legion's
// work (record.Chains.IsRoot, R15 and R19 of dispatch://INFRA-3320): an issue under a parent in
// Legion's work is the workflow's to record (workflow's recordChildUnderParentAtWork), one held
// under a parent out of work with an issue above it in work is held `ancestry` and recorded by
// nothing, and one on an unknown chain is recorded by nothing (R16). The root is recorded as its
// own tree, written once (R22: the column stays written for the release before), and as holding its
// own gate (record.Issue.HoldsGate), which it keeps once it gets a parent in Legion's work.
//
// One whose repository resolves to none, or one GitHub does not answer for as a repository a role
// App reaches, is not recorded: the daemon says why on the issue, once for that resolution, and the
// issue's next observation in todo is admission's next look at it. A root recorded on a repository
// nothing named before has that repository heard and cached from here
// (workflow.Engine.HearRepository).
//
// This is where a root is first recorded, so it is where the issue-unit experiments the project
// declares are drawn for it (drawExperiments): every issue the daemon records is drawn for at the
// moment it records it, and a root is an issue like any other. Its tree-unit experiments are taken
// at its first start (promoteHolds).
func (a *Admission) putNewRoot(ctx context.Context, tx pgx.Tx, observation intake.DispatchIssue, chains *record.Chains) error {
	if !chains.IsRoot(observation.Key) {
		return nil
	}
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
		Resolved:        observation.Resolved,
		HandedOver:      observation.HandedOver,
		LastDispatchSeq: observation.Seq,
		DispatchStatus:  observation.Status,
		Repo:            resolution.Repo,
		DefaultBranch:   resolution.DefaultBranch,
		GitHubRepo:      resolution.GitHubRepo,
		HoldsGate:       true,
	}
	if err := a.engine.WriteIssue(ctx, tx, issue); err != nil {
		return fmt.Errorf("record admitted root %s: %w", observation.Key, err)
	}
	a.engine.HearRepository(ctx, issue.GitHubRepo)
	return a.drawExperiments(ctx, tx, issue.Key, config.ExperimentIssue)
}

// drawExperiments draws every experiment the project declares over unit (UseExperiments) and
// records the answers against key, in the caller's transaction (experiment.DrawAndRecord), with
// the engine's random source: the issue-unit experiments of an issue against its own key when the
// daemon first records it (putNewRoot). The tree-unit ones are settled at an issue's first start
// (takeTreeDraw).
func (a *Admission) drawExperiments(ctx context.Context, tx pgx.Tx, key string, unit config.ExperimentUnit) error {
	return experiment.DrawAndRecord(ctx, tx, a.store, key, a.experiments, unit, a.engine.ExperimentSource(), a.now())
}

// applySummary applies one Dispatch boot-listing summary to admission's own record of the issue
// it names, exactly as it would if nothing had ever deferred it: a still-unrecorded key is
// recorded only when the summary itself shows it todo — under its parent in Legion's work as the
// workflow records a child, else as a root when it is handed to Legion and nothing above it is in
// Legion's work — or queued when it shows a root in triage whose legion label a person set, and a
// recorded key a live event has already carried strictly past the summary's own sequence is left
// alone — reapplying the summary would overwrite fresher information with stale. An unrecorded
// key is held to the issue facts the daemon applied: a summary older than an event of the issue,
// or a view intake let through, that the daemon has already applied records and queues nothing
// (unrecordedSummaryStale), and applySummary answers summaryStale for it. Both Reconcile,
// immediately when the consumer is already caught up, and release, once the consumer catches up to
// a summary Reconcile deferred, call this the same way: a key nothing else ever gets around to
// explaining still lands on what the listing already knew.
//
// With hold set — Reconcile's call while the consumer has not reached its target — a summary the
// listing shows behind Dispatch's event log (Reconcile says why) applies nothing either, and
// applySummary answers summaryHeld for it, for the caller to defer. A stale summary is answered
// so before it could be held, since no fact the stream still delivers makes it any less stale.
func (a *Admission) applySummary(ctx context.Context, tx pgx.Tx, summary intake.DispatchIssue, hold bool) (summaryOutcome, error) {
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
	return summaryApplied, a.applyListed(ctx, tx, summary, stored)
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
func (a *Admission) applyListed(ctx context.Context, tx pgx.Tx, summary intake.DispatchIssue, stored *record.Issue) error {
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
		// An unrecorded todo child under a parent in Legion's work is the workflow's to record, as
		// its own todo event would (the engine's recordChildUnderParentAtWork, which runs before
		// admission on every live event), from the parent the listing shows now. Without this, a
		// child no event of it ever recorded stays unrecorded, and no later event of it need come:
		// Dispatch already shows it todo, so nothing writes that status again. Two ways that
		// happens — an event the daemon never received (a durable consumer created fresh takes only
		// what is published after it, and the notification stream drops a message 72 hours old
		// whether or not anyone read it), and one it received while the child's parent was not in
		// Legion's work, where the workflow records nothing.
		if summary.Parent != "" {
			if _, err := a.engine.Apply(ctx, tx, summary); err != nil {
				return fmt.Errorf("record %s under its parent: %w", summary.Key, err)
			}
			recorded, err := a.store.Issue(ctx, tx, summary.Key)
			if err != nil {
				return fmt.Errorf("re-read %s after recording it under its parent: %w", summary.Key, err)
			}
			if recorded != nil {
				return nil
			}
		}
		chains, err := record.ReadChainsOf(ctx, tx, a.store, a.project, []string{summary.Key})
		if err != nil {
			return err
		}
		if !chains.Chain(summary.Key).Known() {
			a.noteUnknownChain(ctx, summary.Key)
			return nil
		}
		if !handed {
			a.log.Debug("admission: not handed to Legion", "issue", summary.Key, "label", dispatch.LegionLabel)
			return nil
		}
		return a.putNewRoot(ctx, tx, summary, chains)
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
		// engine first — a stop, a return to work or a new run — exactly as that event would.
		// Engine.dispatchIssue already makes this same newer-or-not decision on its own sequence
		// fence (fact.Seq <= issue.LastDispatchSeq). Left to recordObservation alone, the record
		// would move to the snapshot's status with no stop and no return, and the record's
		// LastDispatchSeq would reach the snapshot's own sequence, so the real event, once it
		// finally arrives, would be dropped by that same sequence fence — nothing would ever run
		// what this snapshot stands in for. A snapshot no newer than the record — a session's own
		// status write the daemon already recorded through sessionStatusWrite, echoed back by a
		// boot listing taken before the daemon's own reassert landed — is level, not a change to
		// apply: status stays what recordObservation below already knows. Re-read afterward: the
		// engine's own write must not be clobbered by recordObservation writing back the stale
		// copy this call fetched before the engine ran.
		if _, err := a.engine.Apply(ctx, tx, summary); err != nil {
			return fmt.Errorf("apply engine transition for %s: %w", summary.Key, err)
		}
		refreshed, err := a.store.Issue(ctx, tx, summary.Key)
		if err != nil {
			return fmt.Errorf("re-read %s after its engine transition: %w", summary.Key, err)
		}
		// The engine consumed the snapshot when it recorded the snapshot's sequence: it kept its own
		// status over a status Dispatch already showed (keepStatus), or returned the issue to work
		// or opened its new run, and wrote the observation itself. Recording the snapshot's status
		// now would put a set-back's backlog on a running issue, whose slot the release would then
		// free. Otherwise the snapshot's move is recorded, as the live event's would be.
		if refreshed.LastDispatchSeq >= summary.Seq {
			return nil
		}
		stored, status = refreshed, summary.Status
	}
	// A summary with no sequence is no newer than the record, so it keeps the priorities the
	// record holds, the own and the resolved alike.
	priority, resolved := summary.Priority, summary.Resolved
	if summary.Seq == 0 && stored.LastDispatchSeq > 0 {
		priority, resolved = stored.Priority, stored.Resolved
	}
	o := observed{
		Title: summary.Title, Parent: summary.Parent, Rank: summary.Rank, Priority: priority, Resolved: resolved,
		Status: status, Shown: summary.Status, HandedOver: handed, Seq: seq,
	}
	// A todo echo of an issue the record shows running is the boot listing catching a status the
	// daemon itself wrote and Dispatch had not shown yet. Its status is taken only while the issue
	// is out of Legion's work: an issue in it keeps the status the record says, because its own
	// record is the thing that says the work is under way, and the row it holds, or does not hold,
	// says nothing about it: it keeps that row through every phase that keeps one, worked or not
	// (record.KeepsSlot), holds none in accepting, retro or merging, and gives it back while it
	// waits on another issue (releaseSlotsForWaits). Its priorities are Dispatch's alone, though,
	// and the listing can be the one read that carries them: the first boot after Dispatch resolves
	// priorities lists an issue's inherited P0 at the issue's own sequence, with no event of the
	// issue's (C1 of dispatch://INFRA-5636). So the echo writes the own and the resolved priority it
	// shows, and nothing else of the record.
	if summary.Status == "todo" && stored.Status == "in_progress" && record.InLegionsWork(*stored) {
		if stored.Priority == o.Priority && stored.Resolved == o.Resolved {
			return nil
		}
		stored.Priority, stored.Resolved = o.Priority, o.Resolved
		if err := a.engine.WriteIssue(ctx, tx, *stored); err != nil {
			return fmt.Errorf("record the priorities the listing shows of %s: %w", stored.Key, err)
		}
		return nil
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
// event's title, rank, priority, parent, label, and status reach the record if the engine has
// not already taken them in a stop, return, or new run. The sequence fence keeps that write.
func (a *Admission) applyObservation(ctx context.Context, tx pgx.Tx, stored record.Issue, observation intake.DispatchIssue) error {
	if observation.Seq != 0 && observation.Seq <= stored.LastDispatchSeq {
		return nil
	}
	priority, resolved := observation.Priority, observation.Resolved
	if observation.Seq == 0 && stored.LastDispatchSeq > 0 {
		priority, resolved = stored.Priority, stored.Resolved
	}
	return a.recordObservation(ctx, tx, stored, observed{
		Title: observation.Title, Parent: observation.Parent, Rank: observation.Rank, Priority: priority, Resolved: resolved,
		Status: observation.Status, Shown: observation.Status, HandedOver: observation.HandedOver, Seq: max(observation.Seq, stored.LastDispatchSeq),
	})
}

// observed is what one Dispatch observation of an issue says about it, from a live event or from
// the boot read: Shown is the status Dispatch showed, and Status the one the record takes, which is
// Shown except from a boot listing no newer than the record, which keeps the record's own.
// Priority is the issue's own Dispatch priority and Resolved the one Dispatch resolves it to
// (record.Resolved), which the record takes with it.
type observed struct {
	Title, Parent, Rank, Status, Shown string
	Priority                           record.Priority
	Resolved                           record.Resolved
	HandedOver                         bool
	Seq                                int64
}

// recordObservation is where admission writes a Dispatch observation to an issue record: a live
// event's and the boot read's. It takes a rename, re-rank, priority change, re-parent, label or
// status change the same way. Each caller owns its sequence fence. The engine, which runs
// first, can also write the observation while keeping its own status (keepStatus).
// A re-parent writes only the issue's own parent. Decisions read its place from record.Chains.
func (a *Admission) recordObservation(ctx context.Context, tx pgx.Tx, stored record.Issue, o observed) error {
	if stored.Title == o.Title && stored.Rank == o.Rank && stored.Priority == o.Priority && stored.Resolved == o.Resolved && stored.Status == o.Status && stored.DispatchStatus == o.Shown &&
		stored.HandedOver == o.HandedOver && sameParent(stored.Parent, record.ParentOf(o.Parent)) && stored.LastDispatchSeq == o.Seq {
		return nil
	}
	stored.Title = o.Title
	stored.Parent = record.ParentOf(o.Parent)
	stored.Rank = o.Rank
	stored.Priority = o.Priority
	stored.Resolved = o.Resolved
	stored.Status = o.Status
	stored.DispatchStatus = o.Shown
	stored.HandedOver = o.HandedOver
	stored.LastDispatchSeq = o.Seq
	if err := a.engine.WriteIssue(ctx, tx, stored); err != nil {
		return fmt.Errorf("record observation of %s: %w", stored.Key, err)
	}
	return nil
}

// releaseSlots releases the slot of every issue of this project that no longer needs one
// (record.NeedsSlot, the one statement of the release of an issue that needs no slot at all,
// whose phase half the grant reads too — record.KeepsSlot) and reports those issues in slot
// order. Each release is logged once the fact commits. An issue Dispatch shows out of Legion's
// work, an issue moving into accepting, retro or merging and an issue reaching its done all read
// the same predicate, so one rule says when an issue stops needing a slot where it stands, and it
// reads the issue's own status alone (R20 of dispatch://INFRA-3320): an issue above it leaving
// work frees nothing of it. The daemon makes one other release, releaseSlotsForWaits, judged by
// blockers.waitsOf rather than by record.NeedsSlot, which frees the row of an issue that goes on
// working and waits on another issue; the two are apart because they free rows for opposite
// reasons — this one because the issue's phase holds no row, that one because the work is held —
// and only that one leaves a mark, since only its issue stands in the line for the row again. An
// issue in accepting, retro or merging is never one it frees (blockers.waitsOf says why).
//
// It reads the slotted issues in one query (record.Store.SlottedIssues), never a record per slot
// and never the whole issues table: every pass makes this read under the one fact lock
// (intake.ApplyFact), so a read per slot cost every pass of every consumer, the API and the
// boot's replay a round trip to the database for each slot of the shared table, and a read of
// every record would grow with every issue the project has ever recorded. The read leaves out
// another project's slot, whose own daemon releases it.
//
// It also clears the mark of a release for a wait from every issue that needs no slot at all
// (record.Issue.ReleasedForWaitAt, over issues rather than over the slotted rows): an issue
// released for a wait holds no row, so the loop below never sees it, and it goes on working, so
// it can reach its done, or be parked, with nothing else to clear the mark. Left standing, the
// mark would say the issue stands in the line waiting for its slot
// back when nothing of it is waiting, and its work coming back later would take its place in
// the line from the moment before the park (record.Waiting).
func (a *Admission) releaseSlots(ctx context.Context, tx pgx.Tx, issues []record.Issue) ([]string, error) {
	own, err := a.store.SlottedIssues(ctx, tx, a.project)
	if err != nil {
		return nil, fmt.Errorf("list this project's slotted issues: %w", err)
	}
	inUse := len(own)
	var released []string
	for _, issue := range own {
		if record.NeedsSlot(issue) {
			continue
		}
		if err := a.store.ReleaseSlot(ctx, tx, issue.Key); err != nil {
			return nil, fmt.Errorf("release the slot of %s: %w", issue.Key, err)
		}
		inUse--
		released = append(released, issue.Key)
		a.logSlot(ctx, "released", issue.Key, inUse)
	}
	for _, issue := range issues {
		if issue.ReleasedForWaitAt == nil || record.NeedsSlot(issue) {
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
// own again. Nothing else of it moves —
// no claim is suspended, no phase changes, no Dispatch status is written.
//
// The release is one statement an issue (record.Store.ReleaseSlotForWait): the delete of the row
// and the mark that says why are written together, so no pass can read one without the other.
// The rows it frees are reported to the caller with the rows the ordinary release frees, because
// the controller's free-slot wake is about the capacity of the project and not about the reason
// a row came free.
func (a *Admission) releaseSlotsForWaits(ctx context.Context, tx pgx.Tx, issues []record.Issue,
	slots []record.Slot, facts record.LineFacts,
) ([]string, error) {
	own := ownSlots(issues, slots)
	inUse := len(own)
	var released []string
	for _, slot := range own {
		if _, waits := facts.Waits[slot.Issue]; !waits {
			continue
		}
		if err := a.store.ReleaseSlotForWait(ctx, tx, slot.Issue, a.now()); err != nil {
			return released, fmt.Errorf("release the slot of %s for what it waits on: %w", slot.Issue, err)
		}
		inUse--
		released = append(released, slot.Issue)
		a.logSlot(ctx, "released", slot.Issue, inUse)
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
// Each reads an issue of this project in the workflow whose gate root has started
// (record.StartedRoots, record.TreeStarted: the gate root in Legion's work on the pass's chains
// and not waiting in the line for its first start), so an issue under a root still queued stays
// where it stands, a queued root is left alone (record.LineFacts.Queued), and a root handed over in
// awaiting_children comes back once its promotion has started it. It does not ask whether the tree
// runs (record.TreeRuns, R4 of dispatch://INFRA-3320), which a root waiting in the line for the slot
// of its returned work answers false: such a root is a parent whose start is written and not
// delivered, the case the first move is for (R9), and the issues under it work. An issue whose own
// chain the copy cannot show to its top is decided nothing either (R16 of dispatch://INFRA-3320):
// at work, it goes on in the list it stands in, neither moved into awaiting_children nor taken
// back from it, until the daemon has read Dispatch
// again. A key a hold still names (held, the pass's own predicate) is read on the first pass after
// the hold ends, as the promotion reads it. The engine leaves alone a parent held for any other
// reason, in retro or in merging, whose pull request has merged, or whose phase's role stands on an
// entry a role's move back raised out of retro or merging (record.PhaseRow.EntryFrom), and a second
// pass finds a moved parent in awaiting_children already, so it sends nothing again.
//
// It runs before the releases (promoteHolds), so the slot a stopped parent held is freed on this
// very pass by the one rule for a phase that keeps none (releaseSlots, record.NeedsSlot), and the
// start a returned parent owes stands in this pass's own line.
func (a *Admission) childrenWaits(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts record.LineFacts,
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
	started := record.StartedRoots(issues, facts)
	owed := make(map[string][]record.OwedStart, len(facts.OwedRows))
	for _, start := range facts.OwedRows {
		owed[start.Issue] = append(owed[start.Issue], start)
	}
	wrote := false
	for _, parent := range parents {
		if parent.Project != a.project || !record.InWorkflow(parent) || held(parent.Key) || !facts.Chains.Chain(parent.Key).Known() {
			continue
		}
		if _, ok := started[facts.Chains.GateRoot(parent.Key)]; !ok {
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
		case open:
			moved, err := a.engine.StopForChildren(ctx, tx, parent, wait.Open, owed[parent.Key])
			if err != nil {
				return wrote, fmt.Errorf("move %s to awaiting_children: %w", parent.Key, err)
			}
			wrote = wrote || moved
		}
	}
	return wrote, nil
}

// dueCloses asks the engine, of every issue of the pass that carries a close the daemon queued or
// awaits its merge (workflow.ClosePassCandidate), whether its close is due on this pass's copy
// (workflow.Engine.WriteDueCloses, R24 of dispatch://INFRA-3320), and answers whether it queued
// one. issues are this project's own (reread's LiveIssues). A close it queues is a status write
// and moves no issue into or out of any slot or line, but it marks the issue's record
// (record.Issue.PendingClose), so the pass reads the records again before any later step writes
// one.
func (a *Admission) dueCloses(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts record.LineFacts) (bool, error) {
	return a.engine.WriteDueCloses(ctx, tx, issues, facts.Chains)
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
	return a.promoteHolds(ctx, tx, true)
}

// promoteHolds is promote; respectHolds false is release's own call, once every held key's own
// summary has already been applied: release always ignores every hold, never a subset, since it
// only ever runs once the consumer has caught up to every key Reconcile deferred. The in-memory
// pending set itself only clears after that whole call's transaction commits (see release), so
// without this, the pass's own membership check would hold every one of them back in the very
// call that released them. No issue is adopted into a tree here (R19 of dispatch://INFRA-3320):
// an issue under a parent at work is read under it on Dispatch's chain as it stands.
//
// It makes one read of each kind for the whole pass and shares it: one read of the project's
// pause, which also gives the floor the pass's stall readings count from (readStallFloor), one
// record.Store.LiveIssues, one record.Store.Slots, the pass's one reading of the Dispatch chains
// with the gates on them and one read of the starts the project owes its workers
// (record.ReadLineFacts), and one read of the Dispatch mirror rows of everything it judges
// (blockers.Judged, blockers.Load). The release, the waiting line, the INFRA-129 safety net, the
// promotion and the two repair passes all judge from those, so no path reads per slot, per chain
// or per issue under the one fact lock every fact takes (intake.ApplyFact). A read is made again
// only where this pass has itself written something that would make it stale (reread).
//
// Every path that promotes comes through here — each pass the pass rule (Apply) runs, and the
// boot's Reconcile — so each of them is also a look at every issue a blocker holds, at every slot
// whose worker has lost its task, at every tree a step that left gates.design released
// (record.Waiting's LineFirstPhase), and at every recorded issue on a Dispatch chain the copy
// cannot read to its top, which it notes for the daemon's re-read (noteUnknownChains).
func (a *Admission) promoteHolds(ctx context.Context, tx pgx.Tx, respectHolds bool) (int, []string, error) {
	// The session writes the repair holds on an unknown chain are judged at the first pass that
	// knows the chain, a paused project's included, since a status write is no start.
	if err := a.judgeHeldStatusWrites(ctx, tx); err != nil {
		return 0, nil, err
	}
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
	// The P0 bound is read once for the pass, beside the pause, and the stall measure and the
	// promotion below judge under it (capLets).
	bound, err := a.p0BoundAt(ctx, tx)
	if err != nil {
		return 0, nil, err
	}
	issues, slots, facts, err := a.reread(ctx, tx)
	if err != nil {
		return 0, nil, err
	}
	// Every recorded issue whose chain this read holds unknown is read again from Dispatch by the
	// daemon (R16 of dispatch://INFRA-3320), out of Legion's work as in it, from the chains already
	// read: the pass's statements stay as many.
	a.noteUnknownChains(ctx, issues, facts.Chains)
	// The tree lifecycles the stored trees name stay open for the release before (R22 of
	// dispatch://INFRA-3320): one statement for the project, read by no decision of this pass.
	if err := a.keeper.Keep(ctx, tx, a.store, a.project); err != nil {
		return 0, nil, err
	}
	heldKeys := a.catchingUpFor
	if respectHolds {
		heldKeys = func(key string) bool { return a.held(key) || a.catchingUpFor(key) }
	}
	_, changed, err := a.engine.ReconcileAwaiting(ctx, tx, a.project, facts.Chains, issues, heldKeys)
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
	waited, err := a.childrenWaits(ctx, tx, issues, facts, heldKeys)
	if err != nil {
		return 0, nil, err
	}
	if waited {
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return 0, nil, err
		}
	}
	// A close the daemon owes once each Dispatch child of its issue is done is queued on the first
	// pass whose copy shows them so (dueCloses): a close Dispatch refused, and a merged issue that
	// waits in merging for a child the daemon records nothing of, whose done no departure reads.
	closed, err := a.dueCloses(ctx, tx, issues, facts)
	if err != nil {
		return 0, nil, err
	}
	if closed {
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return 0, nil, err
		}
	}
	released, err := a.releaseSlots(ctx, tx, issues)
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
	rows, facts, line, dropped, err := blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, nil)
	if err != nil {
		return 0, released, err
	}
	// The issues this judgement finds waiting give their rows back before the line is promoted,
	// so this pass's own line and its ownSlots read the state after the release: an issue that
	// waits holds none of the cap's slots, and the slot it gives back is offered to the line on
	// this very pass rather than the next one.
	waiting, err := a.releaseSlotsForWaits(ctx, tx, issues, slots, facts)
	if err != nil {
		return 0, append(released, waiting...), err
	}
	released = append(released, waiting...)
	unslotted, err := a.releaseUnslotted(ctx, tx, issues, facts, respectHolds)
	if err != nil {
		return 0, released, err
	}
	// One re-read and one second judgement answer for both releases: each took rows away, and
	// what the promotion loop and ownSlots must read is the state after the last of them.
	if len(waiting) > 0 || unslotted {
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return 0, released, err
		}
		if rows, facts, line, dropped, err = blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, rows); err != nil {
			return 0, released, err
		}
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
	if err := a.tellStalledTrees(ctx, tx, issues, facts, slots, line, dropped, rows, floor, bound); err != nil {
		return 0, released, err
	}
	if a.cap <= 0 {
		return 0, released, nil
	}
	admitted := 0
	for {
		// The holders as the cap and the P0 bound count them, which grow with each grant below.
		held := record.HoldersOf(issues, slots)
		stopped, opened, resumed := false, false, false
		for _, candidate := range line {
			issue := candidate.Issue
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
				stopped = true
				break
			}
			// A candidate that waits on an unfinished Dispatch issue is passed over and keeps
			// its place: the next pass promotes it once the last issue it waits on is done.
			// That is a decided skip rather than a hold — the pass knows enough to decide it —
			// so the line moves on. The rule is read per kind, as the state's own `blocked`
			// reads it (blockers.LineWaitsOn), so no issue is reported waiting on a link this
			// loop would start anyway.
			if waits := blockers.LineWaitsOn(rows, facts.Chains, candidate); len(waits) > 0 {
				a.log.Info("admission waits for what this issue waits on", "issue", issue.Key, "blockers", waits)
				continue
			}
			// A candidate that already holds a row, or that stands in a phase keeping none,
			// takes no slot and so is never held by the cap: a root another tree handed to
			// Legion carrying the phase it ran there opens its tree at once on the row it
			// brought, and an answered root resumes its architect on the row it kept through
			// the wait.
			takes := slotFor(candidate)
			if !capLets(candidate, held, a.cap, bound) {
				// A full cap gives no further slot below P0, and the scan goes on. This is not the
				// hold above and must not be read as one: a hold is an answer for every candidate
				// behind it, because it says the pass does not yet know what this candidate is;
				// the cap is an answer for this candidate alone, because it says only that there
				// is no slot for it. The count only grows inside this loop, so no candidate below
				// P0 behind this one takes a slot either and the rank order of the issues that
				// need one is kept whole, while the candidates the cap cannot bind — the ones
				// slotFor is false for, and a P0 issue, which starts outside the cap (capLets) —
				// are promoted as they are reached, which is what makes a hand-over, an answered
				// root and a P0 issue start under a cap that is permanently full.
				//
				// An answered root this branch passes over is told nothing. It reads where it
				// stands from its own record — `waitingLine`, which is the live place
				// (api.WaitingLineView) — and the daemon resumes it by itself on the pass that
				// finds a slot free, so a message would carry a snapshot of a place that moves
				// with every pass and nothing besides.
				//
				// A P0 issue is held here by the P0 bound alone, never by the cap: the bound is
				// set and as many P0 issues as it allows hold a slot. It waits first in the line,
				// where its priority places it (record.Waiting), and the pass says why.
				if takes && issue.StartsNow() {
					a.logOnCommit(ctx, "admission holds a P0 issue at the P0 bound", "issue", issue.Key,
						"p0_holders", held.P0, "p0_bound", boundWords(bound), "other_holders", held.Capped, "cap", a.cap)
				}
				continue
			}
			if candidate.Kind == record.LineAnswered {
				// The root's tree already runs: only a reason it may not resume in its own
				// session holds it here (its label gone, a foreign Dispatch claim), which
				// releaseUnslotted takes out of the workflow on this same pass. Its links hold
				// nothing here: a root waiting on a person keeps its row through a wait
				// (blockers.waitsOf).
				reason, err := workflow.Unresumable(ctx, a.store, tx, a.project, facts.Chains, issue)
				if err != nil {
					return admitted, released, fmt.Errorf("check whether answered root %s can resume: %w", issue.Key, err)
				}
				if reason != "" {
					a.log.Warn("admission skips an answered root it may not resume", "issue", issue.Key, "reason", reason)
					continue
				}
			}
			// Claims, issue lifecycles and the runtime key an issue by the normalized project
			// token, not the Dispatch project key the issue record carries.
			project, err := claim.ProjectToken(a.project)
			if err != nil {
				return admitted, released, fmt.Errorf("open admission lifecycle of %s: %w", issue.Key, err)
			}
			// Every issue the pass starts opens its own lifecycle (R17 of dispatch://INFRA-3320):
			// each claim of it binds that lifecycle's epoch (store.Store.AdmitClaim), and a cleanup
			// of the issue reserves it. An issue whose cleanup is still reserved, or whose
			// lifecycle the operator holds, waits, and the candidates behind it go on.
			lifecycle, err := a.store.OpenIssueLifecycle(ctx, tx, project, issue.Key, treelifecycle.AuthorityWorkflow)
			if err != nil {
				if errors.Is(err, treelifecycle.ErrCleanupReserved) {
					a.log.Info("admission waits for the issue's cleanup", "issue", issue.Key)
					continue
				}
				if errors.Is(err, treelifecycle.ErrAuthorityHeld) {
					a.log.Warn("admission waits for the issue's other authority", "issue", issue.Key, "error", err)
					continue
				}
				return admitted, released, fmt.Errorf("open admission lifecycle of %s: %w", issue.Key, err)
			}
			// The issue's row was created by this call, so this is the issue's first start and the
			// one moment its tree-unit experiments are settled, in the fact's own transaction
			// (takeTreeDraw): a crash before the commit leaves neither the lifecycle nor the
			// answers, and every later start creates nothing and settles nothing.
			if lifecycle.Created {
				if err := a.takeTreeDraw(ctx, tx, issue.Key, facts.Chains); err != nil {
					return admitted, released, err
				}
			}
			now := a.now()
			if takes {
				// An entry carries whether its issue held a row when the line was built
				// (record.LineEntry.Holds), and the line this loop reads holds each issue once,
				// so no grant here changes the answer slotFor gives a candidate behind it; what
				// the grant does change is held, the count of holders, which grows with it: the
				// cap reads held.Capped and the P0 bound held.P0 (capLets). finishFencedStarts
				// keeps the line to one entry an issue, not record.Waiting: Waiting would list an
				// issue at admitted that holds no row and still owes a start both as a return and
				// as a first phase, but every owed start names a worker phase, and
				// finishFencedStarts ends each one whose phase is not the issue's own before the
				// line is built, so none is left owed at admitted.
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
				slots = append(slots, slot)
				held.Add(issue)
				a.logSlot(ctx, "taken", issue.Key, held.Total())
				// A P0 start is the one start the cap does not count, so it says so with what the
				// cap and the bound count (C4 of dispatch://INFRA-5636): the P0 holders, this one
				// among them, the other holders and the cap.
				if issue.StartsNow() {
					a.logOnCommit(ctx, "admission starts a P0 issue outside the cap", "issue", issue.Key,
						"p0_holders", held.P0, "other_holders", held.Capped, "cap", a.cap, "p0_bound", boundWords(bound))
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
				// Leaving awaiting_person ends the root_awaiting_person hold on every issue under
				// this top issue — returning work in a working phase as well as first phases when
				// its design gate stands open — so rebuild the line in this pass so they can take
				// slots without waiting for another fact.
				resumed = true
			case record.LineQueuedRoot:
				issue.Status = "in_progress"
				// The hand-over mark goes at a root's first start: it says a root arrived from
				// another tree under the release before and has still to start, and this is the
				// write that starts it (decision 15 of dispatch://INFRA-4730). No decision of this
				// release reads it.
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
				// stands in the line only by a person's todo on it or its architect's rerun_child,
				// and an architect held for OpenAI's cyber-policy refusal comes back this way, in a
				// new session, under the gate root the outbox's start reads on the Dispatch chain.
				if err := a.engine.StartPromotedArchitect(ctx, tx, issue); err != nil {
					return admitted, released, fmt.Errorf("start the architect of %s: %w", issue.Key, err)
				}
				// A root a hand-over of the release before carried in brings the run it carries:
				// its phase resumes at a new entry and the claims it ran with come back
				// (workflow.Engine.StartCarriedRun). Any other queued root stands at admitted, where
				// its architect is the whole of its start.
				if record.CarriedIn(issue) && issue.CarriesRun() {
					if err := a.engine.StartCarriedRun(ctx, tx, issue); err != nil {
						return admitted, released, fmt.Errorf("start the run carried into %s: %w", issue.Key, err)
					}
				}
				opened = opened || facts.GatesOpen(issue.Key)
			case record.LineFirstPhase:
				if err := a.engine.StartFirstPhase(ctx, tx, issue, facts.Chains, rows[issue.Key].Blockers); err != nil {
					return admitted, released, err
				}
			}
			admitted++
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
		if rows, facts, line, _, err = blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, rows); err != nil {
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
	return admitted, released, nil
}

// judgeHeldStatusWrites judges the session writes each issue of the project holds
// (record.Issue.HeldSessionWrites) whose Dispatch chain the copy now knows, in Dispatch's order,
// and clears them in this pass's transaction (workflow's Engine.JudgeHeldStatusWrites), so a later
// pass of the same transaction finds none. An issue whose chain is still unknown keeps them, and its
// key joins the issues the daemon reads Dispatch again for (noteUnknownChain; decision 24 of
// dispatch://INFRA-4730).
func (a *Admission) judgeHeldStatusWrites(ctx context.Context, tx pgx.Tx) error {
	held, err := a.store.HeldSessionWriteIssues(ctx, tx, a.project)
	if err != nil || len(held) == 0 {
		return err
	}
	// One read of the held issues' own chains says which the copy knows (record.ReadChainsOf):
	// judging one issue's writes changes no copy row, so it changes no other issue's answer.
	keys := make([]string, len(held))
	for i, issue := range held {
		keys[i] = issue.Key
	}
	chains, err := record.ReadChainsOf(ctx, tx, a.store, a.project, keys)
	if err != nil {
		return err
	}
	for _, issue := range held {
		known, err := a.engine.JudgeHeldStatusWrites(ctx, tx, chains, issue)
		if err != nil {
			return fmt.Errorf("judge the session writes held on %s: %w", issue.Key, err)
		}
		if !known {
			a.noteUnknownChain(ctx, issue.Key)
		}
	}
	return nil
}

// reread takes this pass's shared reads again, after the pass has itself written something that
// makes them stale (readPass), and marks every root the read finds as holding its own gate: a
// decision pass writes, so it keeps that mark for a root that later gets a parent in Legion's work.
func (a *Admission) reread(ctx context.Context, tx pgx.Tx) ([]record.Issue, []record.Slot, record.LineFacts, error) {
	issues, slots, facts, err := a.readPass(ctx, tx)
	if err != nil {
		return nil, nil, record.LineFacts{}, err
	}
	// Every recorded issue the pass finds a root holds its own gate from now on, and keeps it once
	// it gets a parent in Legion's work (R15 of dispatch://INFRA-3320, record.Chains.HoldsOwnGate):
	// one statement marks every root the read finds unmarked, and none is sent when there is none.
	var roots []string
	for _, issue := range issues {
		if !issue.HoldsGate && facts.Chains.IsRoot(issue.Key) {
			roots = append(roots, issue.Key)
		}
	}
	if len(roots) > 0 {
		if err := a.store.MarkHoldsGate(ctx, tx, roots); err != nil {
			return nil, nil, record.LineFacts{}, fmt.Errorf("mark the roots that hold their own gate: %w", err)
		}
	}
	return issues, slots, facts, nil
}

// readPass is the pass's shared reads alone: the project's live records, the slots, and the facts
// the line, the release and the repair passes are all judged from. A reader in a read-only
// transaction (OwesEntryWork, ArchitectStallArms) takes these and writes nothing; an
// unmarked root still holds its own gate while it stands a root (record.Chains.HoldsOwnGate), so
// its answers are the ones the next decision pass, which marks it, reads.
func (a *Admission) readPass(ctx context.Context, tx pgx.Tx) ([]record.Issue, []record.Slot, record.LineFacts, error) {
	issues, err := a.store.LiveIssues(ctx, tx, a.project)
	if err != nil {
		return nil, nil, record.LineFacts{}, fmt.Errorf("list admission issues: %w", err)
	}
	slots, err := a.store.Slots(ctx, tx)
	if err != nil {
		return nil, nil, record.LineFacts{}, fmt.Errorf("list admission slots: %w", err)
	}
	facts, err := record.ReadLineFacts(ctx, tx, a.store, a.project, issues, a.engine.GatedSteps())
	if err != nil {
		return nil, nil, record.LineFacts{}, err
	}
	return issues, slots, facts, nil
}

// slotFor says whether promoting this candidate must take one of the cap's slots for it: it
// holds none already (record.LineEntry.Holds, which the line sets from the slots it was built
// from) and its phase keeps one (record.KeepsSlot). The grant and the release read that one
// predicate, so no pass hands out a row the next pass would take back.
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
// keeps a slot starts, and it waits for that row in the line.
//
// A candidate that holds a row takes no second one: the line carries holders (the kinds
// record.LineEntry.Holds names). A candidate that takes no slot is promoted whatever the cap:
// holding it back would bound nothing and would leave its tree closed for no reason.
func slotFor(entry record.LineEntry) bool {
	return !entry.Holds && record.KeepsSlot(entry.Issue)
}

// capLets says whether the cap and the P0 bound let the promotion start entry, given held, the
// project's holders of a slot as the two count them (record.HoldersOf), cap and bound, the P0 bound
// in force (p0BoundAt, nil for none): an entry that takes no slot (slotFor) starts whatever the cap
// says; a P0 entry that takes one (record.Issue.StartsNow: Dispatch's resolved P0, or own P0 with
// an older Dispatch release) starts outside the cap, while fewer P0 issues than the bound hold a
// slot, or always where no bound is set (R10 of dispatch://INFRA-3199); and any other entry that
// takes one starts only while the holders below P0 leave a row of the cap free. A cap that admits
// nothing lets nothing start, since the promotion does not run under it. It answers the cap's half
// alone: what an entry waits on is the other half (blockers.LineWaitsOn), which each reader asks
// beside this.
//
// It is the one statement of the cap's half, and two readers ask it: the promotion itself
// (promoteHolds) and the stall measure's free-slot term (lineCanMove). Read apart, the measure
// said the cap held a queued root in accepting, retro or merging that the promotion of the same pass started,
// and told the controller of a stall that was not one.
func capLets(entry record.LineEntry, held record.SlotHolders, cap int, bound *int) bool {
	if cap <= 0 {
		return false
	}
	switch {
	case !slotFor(entry):
		return true
	case entry.Issue.StartsNow():
		return bound == nil || held.P0 < *bound
	default:
		return held.Capped < cap
	}
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
// its Dispatch children, once per issue and per run (record.Store.TellChildrenFirst): the notice
// goes as every notice about an issue goes, to the architect the Dispatch chain names (R18 of
// dispatch://INFRA-3320). The notice names the parent, each open child with the Dispatch status
// that holds it, and each cycle among them (workflow.ChildrenFirstNotice).
//
// The wait is judged again every pass and the telling is written once, which is the whole of
// why the two are recorded differently: every pass the pass rule (Apply) runs, and the boot's,
// judges it, so a notice decided by the wait alone would reach the architect over and over for
// one wait, while a wait decided by a stored flag would outlive the child that caused it. A child
// filed later during one wait therefore tells nobody again — the architect already knows the
// issue waits, and the state names what for — and a new run of the parent, which opens a new
// generation, tells it anew, because that run's architect has not heard it.
//
// The run is the parent's own generation (R17: a run is a fact of the issue), and the mark is
// keyed on the parent as its own tree, so a change of parent neither tells it again nor keeps it
// from being told. The mark's own table says so (20261104004500_children_first_notices).
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
		told, err := a.store.TellChildrenFirst(ctx, tx, key, key, parent.Generation, now)
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
			"generation", parent.Generation, "children", children)
	}
	return nil
}

// releaseUnslotted takes every root waiting on a person that may not resume out of Legion's work:
// its hand-over label or its current Dispatch claim makes a resume unsafe (workflow's
// Unresumable), so nothing would ever start it again while its record went on saying it waits.
// Legion starts nothing for it — it is someone else's work now, or a person's move to make — and
// tells nobody: the controller re-admits nothing for it, and reads it from the state, a root at
// `phase: done` whose status is one only a running tree has (skill://legion-controller, "What
// happened before you started"). The log line names why it left. A key Reconcile holds is left for
// release to apply first, when respectHolds is set. It reports whether it released any.
//
// A root here is the gate root of itself on the pass's chains (record.Chains.GateRoot), which is
// in Legion's work, waiting on a person. It asks nothing of the slots because a row answers none of
// what it asks: the question is whether the root may come back at all, which its label and its
// Dispatch claim decide, and the root keeps its row through the whole wait either way
// (record.KeepsSlot). Unresumable reads who stands in the root's tree on trees, the pass's chains
// until a leave here takes a root out of Legion's work, which changes the trees, and the chains
// read again after each such leave.
//
// No other issue in Legion's work is taken out here (R21 of dispatch://INFRA-3320): every issue in
// it has one place, and a recorded issue at phase admitted whose status is in work and that holds
// no slot waits in the line among the first starts (record.Waiting) rather than leaving it.
func (a *Admission) releaseUnslotted(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts record.LineFacts, respectHolds bool) (bool, error) {
	released := false
	trees := facts.Chains
	for _, issue := range issues {
		if respectHolds && a.held(issue.Key) {
			continue
		}
		if facts.Chains.GateRoot(issue.Key) != issue.Key || !issue.AwaitingPerson() {
			continue
		}
		why, err := workflow.Unresumable(ctx, a.store, tx, a.project, trees, issue)
		if err != nil {
			return released, fmt.Errorf("check whether awaiting root %s can resume: %w", issue.Key, err)
		}
		if why == "" {
			continue
		}
		if err := a.engine.LeaveUnslotted(ctx, tx, issue); err != nil {
			return released, fmt.Errorf("release unslotted root %s: %w", issue.Key, err)
		}
		if trees, err = record.ReadChains(ctx, tx, a.store, a.project, nil); err != nil {
			return released, err
		}
		reason := fmt.Sprintf("%s awaits a person, but %s", issue.Key, why)
		store.OnCommit(ctx, func() {
			a.log.Warn("admission: released a root waiting on a person that may not resume", "issue", issue.Key, "status", issue.Status, "phase", issue.Phase, "generation", issue.Generation,
				"reason", reason)
		})
		released = true
	}
	return released, nil
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
		Parent: observation.Parent, Rank: observation.Rank, Priority: observation.Priority, Resolved: observation.Resolved, HandedOver: observation.HandedOver,
		ResolvedRepo: observation.ResolvedRepo,
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
			outcome, err := a.applySummary(ctx, tx, summary, false)
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
	admitted, returned, err := a.promoteHolds(ctx, tx, !caughtUp)
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
// every admission pass's shared reads read it (readPass) and answered by the two selections that pass
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
	issues, slots, facts, err := a.readPass(ctx, tx)
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
// in accepting, retro or merging (record.InSlotlessPhase, the complement of the phases that keep a row), by key
// and once each, and beside them the architect of each root released for a wait
// (readSlotHolders says why that root's architect alone). The second half is there because such
// an issue holds no row at all now (record.KeepsSlot) and its worker would otherwise be repaired
// by nothing. The three phases are read from the record package rather than listed here, so a
// phase that moves into or out of record.PhaseKeepsSlot moves this repair with it. Every half is
// read from the records the pass already holds, so the widening costs no further read.
//
// The second half asks of the record the three terms the first half gets from the row itself,
// and it must: a slot row exists only while record.NeedsSlot holds, which is the issue in Legion's
// work and a phase of it that keeps a row (record.KeepsSlot), while an issue in accepting, retro or
// merging is in
// the pass's records whatever Dispatch now says of it, since record.Store.LiveIssues answers
// every record that is not done, a parked one included. So this half asks each of the three —
// record.InWorkflow, whose own statement is record.OutOfWorkflow over the Dispatch status, and
// record.TreeRuns over the pass's chains and gates, which carries the root out of Legion's work with
// it. Without them the repair's bound would be the retired-claim skip further down rather than
// the rule: a parked issue in its retro, or one whose tree left the workflow, would have its
// worker started again. The three sibling passes carry the same guard
// (the promotion's own starts, finishFencedStarts, workflow's booted).
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
	// chains is the pass's own reading of the Dispatch chains (record.LineFacts.Chains), which
	// says whether a root's issue holds its own design gate (architectServed).
	chains *record.Chains
}

// readSlotHolders is the reading owedEntryRoles weighs, over its domain: every slot of the
// project, then each issue of the project in accepting, retro or merging under a gate root that
// has started (record.TreeStarted), then the architect of each root of the project released for a
// wait in a tree that runs (record.TreeRuns). Only the role of each
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
		// stand on one issue: a root at admitted holds no phase worker at all, and a root past
		// admitted has both its phase's worker and its own architect.
		if architect, selected := a.rootArchitectOf(project, issue, facts.Chains); selected {
			hold(architect, admittedAt)
		}
	}
	for _, slot := range slots {
		if issue, recorded := byKey[slot.Issue]; recorded {
			take(issue, slot.AdmittedAt)
		}
	}
	for _, issue := range issues {
		if !record.InSlotlessPhase(issue) || !record.InWorkflow(issue) {
			continue
		}
		root, rooted := byKey[facts.Chains.GateRoot(issue.Key)]
		if !rooted || !record.TreeStarted(root, facts.Gates[root.Key]) {
			continue
		}
		take(issue, time.Time{})
	}
	for _, issue := range issues {
		if issue.ReleasedForWaitAt == nil || !record.InLegionsWork(issue) {
			continue
		}
		if _, already := seen[issue.Key]; already {
			continue
		}
		// The root holds no row while it waits on another issue, and that wait is no wait for a
		// slot (record.TreeRuns reads it as none), so this asks R4's rule as it stands.
		architect, selected := a.rootArchitectOf(project, issue, facts.Chains)
		if !selected || !record.TreeRuns(issue, facts.Gates[issue.Key], false) {
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
	read.chains = facts.Chains
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
		if send != raiseArchitectEntry && !workflow.OwedEntry(run, entry.Entry) {
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

		// An agent the operator or the project's controller suspended is passed over whatever
		// entry its claim stands on: `legion claims suspend`, or the controller's own suspend,
		// suspended that worker, and each such suspension holds against
		// every rule of the daemon's until a resume lifts it (supervise's RuleHolds, which no
		// start the daemon queues gets past). A claim the daemon itself suspended is repaired,
		// which is what the idle rule's release and the daemon's own suspend both leave behind.
		if c := run.Claim; c != nil && c.State == supervise.StateSuspended && c.SuspendedBy.Holds() {
			continue
		}
		// A retired claim was stopped, and two stops reach this repair: an operator's `legion
		// claims stop`, and an agent's own report of its exit, which retires a worker's claim in
		// the same way (api's exit, supervise's table) though no shipped agent sends one. The
		// workflow's own retires never reach it: it retires a claim at its issue's close, which
		// takes the issue out of the live records, which the loop above passes over already.
		// Starting the phase again
		// here would hand the operator back, a second later, the worker they stopped. The ways
		// back are the workflow's own starts — the held phase's retry, the issue's return to work —
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
		if held.role == claim.RoleArchitect && !architectReached(held.issue, entry, run, read.chains.HoldsOwnGate(held.issue.Key)) {
			continue
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
func architectReached(root record.Issue, entry record.PhaseEntry, run record.RoleRun, holdsGate bool) bool {
	if _, pending := workflow.ArchitectStartPending(root, entry.Entry, run); pending {
		return false
	}
	return architectServed(root, entry, run, holdsGate)
}

// architectServed is whether the own architect of a root the selection holds is one the sweep has
// work for and can tell it: the two readings the sweep's send (architectReached) and the first
// stall arm (architectTookNothing) both stand on. Its architect must be able to hear
// (workflow.ArchitectHearsSlotted, the slotted form of the daemon's one reading of that
// question, workflow.ArchitectHears: a root Dispatch has taken out of Legion's work, and an
// issue that holds no gate of its own, which is what leaves a child's own
// sub-architect claim outside the rule, since its start, which the child's first phase start
// queues (workflow's gateOpened), carries no task and raises no entry). The slotted form
// reads nothing, because root is one readSlotHolders holds: it holds one of the slots this pass
// read, or stands in accepting, retro or merging under a gate root that has started
// (record.TreeStarted), or was released for a wait in a tree that runs (record.TreeRuns), neither
// of which a queued root is, and so waits for none. And the root must be
// inside the reach (workflow.ArchitectAtWork): a root standing at admitted, whatever its design
// gate, a root a hand-over carried in while it stands in the run that hand-over carried it into,
// and a root past admitted under its own power, waiting on no person, only while the entry its
// architect's row names (entry) is one the resume of a person's answer raised and its claim (run)
// has not been given, since its phase role owns the rest of its work. Both callers hand over the
// entry and the run the pass read for the whole selection (slotReading), so the first arm reaches
// the same roots the send does, which is R7's "the root is one the rule of R5 reaches"
// (dispatch://INFRA-2614). Neither reading reads anything.
func architectServed(root record.Issue, entry record.PhaseEntry, run record.RoleRun, holdsGate bool) bool {
	return workflow.ArchitectHearsSlotted(root, holdsGate) && workflow.ArchitectAtWork(root, holdsGate, entry, run)
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
// would have run it under: its issue is out of Legion's work (record.InLegionsWork), or has moved
// past the phase or the generation the row names. The outbox applies those fences when it runs a
// row, but a row waiting for a slot may never be run at all, and one that could never act would
// keep its issue in the waiting line for ever and take a slot from work that could use it.
// Finishing the row deletes it, exactly as the outbox's own fence would. An issue that returns to
// work is owed its start again by the return itself (workflow.Engine.ReturnToWork), at a new
// entry, so nothing of the stopped run's row is needed then. The daemon's store reads the same
// phase and generation fence where a waiting start silences a backstop
// (store.PodRecords.WaitsForItsSlot), since a row this repair will finish brings no worker back: a
// change to this fence changes that one too.
//
// An issue at phase done is read by its absence: the project's live read leaves it out
// (record.Store.LiveIssues), so its record is not here, and a row of it is finished too.
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
		if recorded && record.InLegionsWork(issue) && owed.Generation == issue.Generation && owed.Phase == issue.Phase {
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

// enqueue queues payload for issue due at now, fenced by the run and phase entry issue stands at
// (record.NewFencedRow), which the fact's settle sets again to where the fact leaves the issue.
func (a *Admission) enqueue(ctx context.Context, tx pgx.Tx, issue string, payload record.OutboxPayload, now time.Time) error {
	row, err := record.NewFencedRow(ctx, tx, a.store, issue, payload, now)
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
