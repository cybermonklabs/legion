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
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/memorygroup"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
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
	// memory is the project's `memory` block (UseMemory), whose mode each tree this admission
	// opens is drawn under; the zero block is a project that names none, which draws every tree
	// `none`. coin is the fair coin the `random` mode is drawn with, memorygroup.CryptoCoin in
	// production and a pinned one in a test, injected as now is.
	memory config.Memory
	coin   func() (bool, error)

	// mu guards pending, target and catchingUp, the only Admission state a caller outside its own
	// transactions touches: the daemon's boot-owned poll calls Held on every tick, outside any
	// transaction and the database's own advisory fact lock, concurrently with whatever release
	// call currently holds both while it clears them.
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
	pending map[string]intake.DispatchIssue
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
	// answered — it keeps the place its answer gives it in the waiting line's first part,
	// among the issues whose work came back, oldest first (record.Waiting) — until the catch-up
	// has applied its window and releases it (ReleaseCatchUp). Unlike pending, a release's own
	// pass does not lift it.
	catchingUp map[string]struct{}

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
	// `worker_silence_minutes`), the one bound the sweep, the silence handler and this list are
	// all read against (workflow.SilentPhaseWorker). workerBackground is how long a worker's own
	// session's report of background work may stand before the tick names it all the same
	// (config.Config.WorkerBackground, `worker_background_hours`,
	// workflow.BackgroundStalledWorker). An admission nobody registered the bounds with holds
	// zero for both, which only a test builds, and both rules answer false for every run of it,
	// so such a tick names nothing silent.
	workerSilence    time.Duration
	workerBackground time.Duration
}

// ReportCapabilities registers open, whose answer every tick controller notice carries as its
// openCapabilities: the daemon's report of the deployment capabilities with no decision, so the
// controller's periodic turn names each gap in the day's report. No other wake carries it.
func (a *Admission) ReportCapabilities(open func() []string) {
	a.openCapabilities = open
}

// UseMemory registers the project's `memory` block, under whose mode every tree this admission
// opens for the first time is drawn (memorygroup.DrawAndRecord, promoteHolds). A project whose
// file names no block leaves the zero value, which draws every tree `none`. It is a setter rather
// than a parameter of New for the reason ReportCapabilities is: every test that admits anything
// builds an admission, and none of them configures memory.
func (a *Admission) UseMemory(memory config.Memory) {
	a.memory = memory
}

// UseWorkerSilence registers the two bounds a phase worker's quiet is read against, which every
// tick controller notice derives its silent list from (silentWorkers): silence for a worker
// whose last step is old, background for a worker whose own report of background work is. It
// is a setter rather than a parameter of New for the reason UseMemory and ReportCapabilities
// are: every test that admits anything builds an admission, and none of them configures a bound.
func (a *Admission) UseWorkerSilence(silence, background time.Duration) {
	a.workerSilence = silence
	a.workerBackground = background
}

var _ intake.Handler = (*Admission)(nil)

// New creates an admission handler for one Dispatch project. engine is the workflow's own, shared
// with intake's other handler: reenterStrandedChildren calls its exported ReenterChild rather than
// keeping a second copy of re-entry and gate-open behavior.
func New(store record.Store, engine *workflow.Engine, cap int, project string, log *slog.Logger) *Admission {
	if log == nil {
		log = slog.Default()
	}
	return &Admission{store: store, engine: engine, cap: cap, project: project, log: log, now: time.Now,
		coin: memorygroup.CryptoCoin, pending: make(map[string]intake.DispatchIssue),
		unclassifiedLogged: make(map[string]uint64)}
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
func (a *Admission) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	released, refusal, err := a.applyFact(ctx, tx, fact)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{Refusal: refusal}, a.wakeForFreeSlot(ctx, tx, released)
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
		// The sweep converges the entries and does no more: it promotes nothing, since a pass
		// that promotes runs on every fact and on the tick, and it releases no slot, so it
		// reports none.
		return nil, nil, a.convergeEntries(ctx, tx)
	}
	if _, ok := fact.(intake.ControllerTick); ok {
		_, returned, err := a.promote(ctx, tx)
		if err != nil {
			return nil, nil, err
		}
		// What the project waits on, what nothing starts and whose phase worker nothing will
		// prompt are read here, after the promotion this tick made, from one read of the
		// project (tickRows): the first two are two assemblies of one judgement of the line, and they are built whether or not a controller is
		// registered to be told. The wake carries them as it carries the open capabilities, and
		// judges nothing a second time.
		held, blocked, silent, err := a.tickRows(ctx, tx)
		if err != nil {
			return nil, nil, err
		}
		// The tick wakes the controller whatever the slots: with every slot taken, it is the only
		// wake a tree waiting on a refused root claim gets, since only the controller's recheck of
		// that claim moves it, and it is the turn that posts the day's report.
		return returned, nil, a.wakeController(ctx, tx,
			record.ControllerNotice{Kind: record.TickNotice, Held: held, Blocked: blocked, Silent: silent}, a.project, a.now())
	}
	if handover, ok := fact.(intake.Handover); ok {
		if err := a.consumeHandover(ctx, tx, handover); err != nil {
			return nil, nil, err
		}
		_, returned, err := a.promote(ctx, tx)
		return returned, nil, err
	}

	observation, ok := fact.(intake.DispatchIssue)
	if !ok {
		_, returned, err := a.promote(ctx, tx)
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
		_, released, err := a.promote(ctx, tx)
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
	// Every exit below still promotes, though it records nothing: the
	// blockers.Mirror handler, which runs first, has already written this issue's row in this
	// transaction, and a root or a child may be waiting on exactly this issue. Its close is an
	// observation of an unrecorded issue out of todo, so without this look a waiting root whose
	// last blocker is an issue Legion keeps no record of would wait for the controller tick to be
	// admitted.
	switch {
	case observation.Status != "todo":
		a.refreshHeldSummary(observation)
		_, released, err := a.promote(ctx, tx)
		return released, nil, err
	case !handed:
		a.log.Debug("admission: not handed to Legion", "issue", observation.Key, "label", dispatch.LegionLabel)
		a.refreshHeldSummary(observation)
		// Promotion comes before the slot is judged: a root this look admits takes a slot that
		// was free before it, and the wake must name a slot still free once this fact commits,
		// never one the same transaction has just filled.
		_, released, err := a.promote(ctx, tx)
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
		return released, nil, a.wakeController(ctx, tx, record.ControllerNotice{Kind: record.TodoNotice}, observation.Key, a.now().Add(todoWakeDelay))
	}
	a.mu.Lock()
	summary, held := a.pending[observation.Key]
	a.mu.Unlock()
	if held && observation.Seq <= summary.Seq {
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
		// replay records nothing of itself, but it is still a look at what Legion could now
		// start, as every fact is.
		_, released, err := a.promote(ctx, tx)
		return released, nil, err
	}
	if err := a.putNewRoot(ctx, tx, observation, true); err != nil {
		return nil, nil, err
	}
	_, released, err := a.promote(ctx, tx)
	return released, nil, err
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
		if err := a.store.PutIssue(ctx, tx, *stored); err != nil {
			return fmt.Errorf("record the Dispatch chain of %s: %w", key, err)
		}
	}
	return nil
}

// wakeForFreeSlot wakes the controller when this transaction released a slot that promotion left
// free: an issue reached its done or its wait for a merge, or left the workflow, and nothing
// waiting in the line took its place. The controller names the free slot in its summary and takes
// no issue for it (skill://legion-controller, "Free slots"). It is one `slot-free` notice on the
// first issue whose slot was released, under the same rules as every slot wake: a slot free by
// slotFree, a registered controller, none already unsent (wakeController).
func (a *Admission) wakeForFreeSlot(ctx context.Context, tx pgx.Tx, released []string) error {
	if len(released) == 0 {
		return nil
	}
	free, err := a.slotFree(ctx, tx)
	if err != nil || !free {
		return err
	}
	return a.wakeController(ctx, tx, record.ControllerNotice{Kind: record.SlotFreeNotice}, released[0], a.now())
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
	rows, facts, line, _, err := blockers.JudgeLine(ctx, tx, a.store, a.project, issues, own, facts, nil, a.now())
	if err != nil {
		return false, err
	}
	taking := 0
	for _, candidate := range line {
		if len(blockers.LineWaitsOn(rows, candidate)) == 0 && slotFor(candidate) {
			taking++
		}
	}
	return len(own)+taking < a.cap, nil
}

// wakeController queues notice on issue, due at dueAt, when a controller is registered and no
// notice of the same kind still waits in the outbox, so a burst of events or ticks queues one
// wake. The wake needs no record: the controller reads Dispatch and the daemon's state
// (skill://legion-controller). A tick alone carries the daemon's open capabilities
// (ReportCapabilities), which this adds, and the three lists the tick's own fact built from one
// read of the project and put on the notice before it reached here (applyFact, tickRows): every
// issue Legion is waiting to start behind an unfinished Dispatch issue, every issue nothing
// starts at all, and every issue whose phase worker nothing will prompt (silentWorkers). So a
// tick that wakes nobody has judged them all the same, and nothing here judges anything a second
// time. Every other kind is queued as the caller built it, so a kind that says something of its
// own — the role and the reason of TellController's — carries it here.
//
// Which notices count as the same one is the kind's own rule (record.NoticePerClaim): a kind
// that speaks for the project is one wake per project, which is what the slot wakes and the tick
// want, and a kind that speaks for one claim is one wake per claim — its issue and its role. The
// second is the provider-errors notice: in an outage every claim's turns fail, each claim owes a
// notice of its own, and the machine tells at exactly the sixth turn in a row, so a notice
// dropped because another claim's still waited would never be sent. Two claims of one issue, an
// architect and its worker, are two such claims, which is why the role is part of the key.
func (a *Admission) wakeController(ctx context.Context, tx pgx.Tx, notice record.ControllerNotice, issue string, dueAt time.Time) error {
	registered, err := a.store.ControllerRegistered(ctx, tx, a.project)
	if err != nil || !registered {
		return err
	}
	of, from := "", claim.Role("")
	if record.NoticePerClaim(notice.Kind) {
		of, from = issue, notice.Role
	}
	pending, err := a.store.ControllerNoticePending(ctx, tx, a.project, notice.Kind, of, from)
	if err != nil || pending {
		return err
	}
	if notice.Kind == record.TickNotice && a.openCapabilities != nil {
		notice.OpenCapabilities = a.openCapabilities()
	}
	return a.enqueue(ctx, tx, issue, notice, dueAt)
}

// TellController queues notice on issue in the caller's transaction, for a caller outside any
// fact: the supervisor, whose machine decides inside its own lock (supervise's
// Deps.ProviderErrorRunReached, wired in internal/daemon, which opens the transaction). It is
// wakeController and nothing else: a controller that is not registered is told nothing, and a
// notice the same wake already waits to send stops this one, so a run of the same cause on the
// same claim is one wake and another claim's run is a wake of its own (record.NoticePerClaim).
func (a *Admission) TellController(ctx context.Context, tx pgx.Tx, notice record.ControllerNotice, issue string) error {
	return a.wakeController(ctx, tx, notice, issue, a.now())
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
	intake.OnCommit(ctx, func() { a.log.Info("admission: queued a person's handover", "issue", key, "source", source) })
	return nil
}

// tickRows is what the tick controller notice carries beside the open capabilities: every issue
// Legion is waiting to start that waits on something else (`blocked`), every issue the daemon's
// own record holds in the workflow that stands in neither the active set nor the waiting line,
// with the one word for why (`held`, C3 of dispatch://INFRA-3107), and every issue whose phase
// worker nothing will prompt (`silent`, silentWorkers).
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
	rows, facts, line, dropped, err := blockers.JudgeLine(ctx, tx, a.store, a.project, issues, own, facts, nil, a.now())
	if err != nil {
		return nil, nil, nil, err
	}
	held := record.HeldList(issues, own, line, dropped, facts)
	a.logUnclassified(held, issues)
	return held, blockers.Blocked(rows, issues, line, record.ChildWaitRows(facts.ChildWaits)), silent, nil
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
// roots are read from and which the slot repair walks its slots through.
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
// worker that is owed a turn, and that holder has none to start. The gate is unreachable on
// this release: the role skip above it passes over every phase phase.RoleFor names no worker
// role for — the architect's admitted and accepting, and the empty role of held,
// awaiting_person and done — and merging, the one holder record.AtWork used to answer false
// for, is a worker's phase now, since its merger makes the merge itself. It is kept so that a
// phase added later with a worker role of its own, in a state record.AtWork answers false for,
// is sent nothing by either repair, and so record.AtWork stays the one statement of what work
// is rather than a second list of phases here.
//
// An issue whose key yields no claim token has no run to read, so it is passed over rather
// than failing the whole fact, as the wait reconcile passes over such a root (workflow's
// ReconcileAwaiting).
func (a *Admission) phaseWorkerOf(project string, issue record.Issue, byKey map[string]record.Issue,
	facts record.LineFacts,
) (phaseWorker, bool) {
	if issue.Project != a.project {
		return phaseWorker{}, false
	}
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
	if issue.Project != a.project || !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.Lingers() {
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
// The silence handlers read the same rules over the same record, so the list and the
// `worker-silent` and `worker-background-stalled` notices the architects were sent name the
// same workers, with one difference: a handler also refuses a fact whose generation is not the
// issue's current one (workflow's phaseWorkerRecord), a term the tick never meets, because it
// reads each issue at its current generation.
//
// Why the controller is told at all, when the issue's own architect already was: that architect
// may be failed or gone, so its notice reaches nobody, and the controller is the role that holds
// `fresh_session` and reads the whole project (this root's architect ruled it on 2026-11-12,
// dispatch://INFRA-3163). The tick carries the list. The state route gains no field for it; the
// state's settings gain the two bounds, `worker_silence_minutes` and `worker_background_hours`,
// as two keys of a loose object, so no daemon API contract number is taken for either.
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
func (a *Admission) Reconcile(ctx context.Context, tx pgx.Tx, summaries []intake.DispatchIssue, target int64, position intake.DispatchConsumerPosition) error {
	issues, err := a.store.LiveIssues(ctx, tx, a.project)
	if err != nil {
		return fmt.Errorf("list admission issues: %w", err)
	}
	facts, err := record.ReadLineFacts(ctx, tx, a.store, a.project, issues, a.engine.GatedSteps())
	if err != nil {
		return err
	}
	live := record.LiveRoots(issues, facts)

	caughtUp := position.Reached(target)
	deferred := make(map[string]intake.DispatchIssue)
	for _, summary := range parentsFirst(summaries) {
		stored, err := a.store.Issue(ctx, tx, summary.Key)
		if err != nil {
			return fmt.Errorf("read reconciled issue %s: %w", summary.Key, err)
		}
		behind := stored == nil && summary.Seq > 0 || stored != nil && summary.Seq > stored.LastDispatchSeq
		if behind && !caughtUp {
			a.log.Debug("admission reconcile: the stream may still hold or redeliver an event for this issue; holding it for the consumer to catch up",
				"issue", summary.Key, "status", summary.Status, "dispatch", summary.Seq)
			deferred[summary.Key] = summary
			continue
		}
		if err := a.applySummary(ctx, tx, summary, live); err != nil {
			return err
		}
	}

	if len(deferred) > 0 {
		a.mu.Lock()
		for key, summary := range deferred {
			a.pending[key] = summary
		}
		a.holds++
		if target > a.target {
			a.target = target
		}
		a.mu.Unlock()
		a.log.Info("admission reconcile: holding roots for the Dispatch consumer to reach a stream position",
			"count", len(deferred), "target", target, "ack_floor", position.AckFloorStream)
	}

	_, released, err := a.promote(ctx, tx)
	if err != nil {
		return err
	}
	return a.wakeForFreeSlot(ctx, tx, released)
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
// observation resolves to and that repository's default branch (workflow's ResolveRepository).
// One whose repository resolves to none, or to one Legion does not serve, is not recorded: the
// daemon says why on the issue, once for that resolution, and the issue's next observation in
// todo is admission's next look at it.
func (a *Admission) putNewRoot(ctx context.Context, tx pgx.Tx, observation intake.DispatchIssue, logOrphan bool) error {
	resolution, err := a.engine.ResolveRepository(observation.ResolvedRepo)
	if err != nil {
		return fmt.Errorf("resolve the repository of %s: %w", observation.Key, err)
	}
	if resolution.Refusal != "" {
		noted, err := a.engine.RefuseRecording(ctx, tx, observation.Key, observation.ResolvedRepo, resolution)
		if err != nil {
			return fmt.Errorf("say why %s is not admitted: %w", observation.Key, err)
		}
		if noted {
			intake.OnCommit(ctx, func() {
				a.log.Info("admission: not admitted; its repository is not one Legion serves", "issue", observation.Key,
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
	if err := a.store.PutIssue(ctx, tx, issue); err != nil {
		return fmt.Errorf("record admitted root %s: %w", observation.Key, err)
	}
	if logOrphan && observation.Parent != "" {
		a.log.Info("admission orphan", "issue", observation.Key, "parent", observation.Parent)
	}
	return nil
}

// applySummary applies one Dispatch boot-listing summary to admission's own record of the issue
// it names, exactly as it would if nothing had ever deferred it: a still-unrecorded key is
// recorded only when the summary itself shows it todo — into its parent's live tree as the
// workflow records a child, else as a root when it is handed to Legion — or queued when it shows a
// root in triage whose legion label a person set, and a recorded key a live event has already
// carried strictly past the summary's own sequence is left alone — reapplying the summary would
// overwrite fresher information with stale. Both Reconcile, immediately when the consumer is
// already caught up, and release, once the consumer catches up to a summary Reconcile deferred,
// call this the same way: a key nothing else ever gets around to explaining still lands on what
// the listing already knew.
func (a *Admission) applySummary(ctx context.Context, tx pgx.Tx, summary intake.DispatchIssue, live map[string]struct{}) error {
	stored, err := a.store.Issue(ctx, tx, summary.Key)
	if err != nil {
		return fmt.Errorf("read reconciled issue %s: %w", summary.Key, err)
	}
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
		Title: summary.Title, Parent: summary.Parent, Rank: summary.Rank,
		Status: status, Shown: summary.Status, Ancestry: summary.Ancestry, HandedOver: handed, Seq: seq,
	}
	if summary.Status == "todo" && handed && stored.Readmittable() {
		return a.readmit(ctx, tx, *stored, o, false)
	}
	// A todo echo of an issue the record shows running is the boot listing catching a status the
	// daemon itself wrote and Dispatch had not shown yet. It is taken only while the issue's tree
	// is not live: a running tree's member keeps what the record says, because the tree is the
	// thing that says the work is under way, and the row the member holds says nothing about it —
	// it keeps that row through every phase short of done, worked or not (record.KeepsSlot).
	if summary.Status == "todo" && stored.Status == "in_progress" {
		if _, running := live[stored.Tree]; running {
			return nil
		}
	}
	return a.recordObservation(ctx, tx, *stored, o)
}

// applyObservation records a newer Dispatch observation of a recorded issue: the one place a live
// event's title, rank, parent, label, and status reach the record, so a re-rank, a rename, or the
// label taken off or put back at the same status moves the waiting line at once. A todo on a
// lingering or closed root handed to Legion is a re-admission.
func (a *Admission) applyObservation(ctx context.Context, tx pgx.Tx, stored record.Issue, observation intake.DispatchIssue) error {
	if observation.Seq != 0 && observation.Seq <= stored.LastDispatchSeq {
		return nil
	}
	handed := observation.HandedOver
	o := observed{
		Title: observation.Title, Parent: observation.Parent, Rank: observation.Rank,
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
				a.log.Info("admission hand-over", "issue", stored.Key, "tree", handover.Tree, "linger", handover.Generation)
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
		a.log.Info("admission orphan", "issue", stored.Key, "parent", observation.Parent)
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
	intake.OnCommit(ctx, func() {
		a.log.Info("admission hand-over", "issue", fact.Issue, "tree", fact.Tree, "linger", fact.Generation, "via", "outbox")
	})
	return a.readmit(ctx, tx, *stored, observed{
		Title: stored.Title, Parent: deref(stored.Parent), Rank: stored.Rank, Ancestry: stored.Ancestry, Seq: stored.LastDispatchSeq,
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
// Ancestry says.
type observed struct {
	Title, Parent, Rank, Status, Shown string
	Ancestry                           []string
	HandedOver                         bool
	Seq                                int64
}

// recordObservation is where admission writes a Dispatch observation to an issue record: a live
// event's and the boot read's, so a rename, a re-rank, a re-parent, a label or a status change is
// written the same way whichever brought it. Each caller owns its own guards — the stream's
// sequence fence, the boot read's — and this writes what they let through. The engine, which runs
// first, writes the observation itself when it keeps its own status over the event's (its
// keepStatus: a session's write it sets back, or an edit that changed no status), and a live event
// it wrote then stops at the sequence fence here. A re-parent never changes the record's tree
// here: the only path that does is adoption, which the same fact's promote runs next
// (adoptQueuedRoots) for a todo root that never ran and whose new parent is in a live tree.
func (a *Admission) recordObservation(ctx context.Context, tx pgx.Tx, stored record.Issue, o observed) error {
	ancestry, err := a.engine.Ancestry(ctx, tx, stored, o.Ancestry)
	if err != nil {
		return fmt.Errorf("read whether %s ran as a tree of its own: %w", stored.Key, err)
	}
	if stored.Title == o.Title && stored.Rank == o.Rank && stored.Status == o.Status && stored.DispatchStatus == o.Shown &&
		stored.HandedOver == o.HandedOver && sameParent(stored.Parent, record.ParentOf(o.Parent)) && stored.LastDispatchSeq == o.Seq &&
		slices.Equal(stored.Ancestry, ancestry) {
		return nil
	}
	stored.Title = o.Title
	stored.Parent = record.ParentOf(o.Parent)
	stored.Rank = o.Rank
	stored.Status = o.Status
	stored.DispatchStatus = o.Shown
	stored.HandedOver = o.HandedOver
	stored.LastDispatchSeq = o.Seq
	stored.Ancestry = ancestry
	if err := a.store.PutIssue(ctx, tx, stored); err != nil {
		return fmt.Errorf("record observation of %s: %w", stored.Key, err)
	}
	return nil
}

// readmit records a lingering or closed root's todo, or an orphan's, as a new generation waiting
// for a slot, taking o's title, parent, rank, Dispatch chain (as the engine's Ancestry takes it;
// a root that left another tree takes none) and sequence. A root starts a fresh generation: its
// facts clear across its whole tree, as do those of an orphan readmitted without carry. An orphan
// readmitted with carry (one that started a phase) is the same work moving from a finished root to
// a root of its own, so its phase, phase rows, gate and pull request (with its observed head) carry
// into the new generation, and its promotion starts both its architect and the role of that
// carried phase. A merged pull request is never observed as merged again, and a review or an
// acceptance would otherwise rerun work already decided or landed.
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
		stored.Phase, stored.Hold, stored.Awaiting = phase.Admitted, nil, nil
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
	stored.LingerUntil = nil
	stored.LastDispatchSeq = o.Seq
	if err := a.store.PutIssue(ctx, tx, stored); err != nil {
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
	// The hand-over settles its memory too, here and not at its own first open: it takes the answer
	// of the tree it leaves, that tree's draw verbatim with the time it was drawn where it had one,
	// so the work goes on running under the group it has been running under, and no draw at all
	// where that tree had been opened with none — a tree first opened before this release, which
	// nothing draws for. A child out of a tree nothing ever opened has run under nothing, so the
	// carry writes it nothing and its own first promotion draws it. tree_lifecycles is keyed by
	// the normalized project token, so the carry is handed that token, as promoteHolds reads it.
	if oldTree != stored.Key {
		if err := a.store.DeleteHandover(ctx, tx, stored.Key); err != nil {
			return err
		}
		project, err := claim.ProjectToken(a.project)
		if err != nil {
			return fmt.Errorf("carry the memory of re-admitted %s: %w", stored.Key, err)
		}
		if _, err := a.store.CarryMemory(ctx, tx, project, stored.Key, oldTree); err != nil {
			return err
		}
	}
	if orphan {
		if pr != nil {
			// The carried pull request starts the generation's fence; its head run goes, as with a
			// new generation (record.ClearGeneration), since its replay would put the old fence back.
			pr.Generation, pr.Run = int64(stored.Generation), nil
			if err := a.store.PutPullRequest(ctx, tx, *pr); err != nil {
				return fmt.Errorf("carry pull request to re-admitted orphan %s: %w", stored.Key, err)
			}
		}
		// Every root a hand-over created is recorded as one, so its architect's catch-up says it was
		// handed over and decomposes nothing anew; it is marked as carrying a gate, which tells the
		// architect never to register its own spec, only when its old root had one. The carry takes
		// every step's row, each keeping its own step, so a tree gated on several steps arrives
		// approved on each of them as the old root was. A root reopened in its linger has its gate
		// cleared with the generation, so a second close hands its children over with none: such a
		// child keeps its phase and pull request, and its architect registers its spec as a fresh
		// root's does, the gate every member of its tree starts behind.
		//
		// An old root that recorded no gate is the common case, and there the carry writes nothing:
		// the orphan's own pairs stand as the hand-over left them, so neither the read of them nor
		// the comparison against them is made.
		if len(gates) > 0 {
			// The pairs the orphan itself holds before the carry: a member of a gated tree holds
			// the copy its hand-over carried to it, and the carry writes the old root's rows over
			// that.
			held, err := a.store.GatesOf(ctx, tx, stored.Key)
			if err != nil {
				return fmt.Errorf("read the gates of re-admitted orphan %s: %w", stored.Key, err)
			}
			for _, gate := range gates {
				gate.Issue = stored.Key
				if err := a.store.PutGate(ctx, tx, gate); err != nil {
					return fmt.Errorf("carry gate to re-admitted orphan %s: %w", stored.Key, err)
				}
			}
			// The carry binds this issue's steps to the old root's documents, so it moves the pairs
			// of an orphan that held none and leaves those of one re-admitted onto the same
			// documents (record.PublishChangedGates); a carry is the one path that gives an issue a
			// gate without its architect registering anything, so Dispatch hears of it here or
			// nowhere.
			if err := record.PublishChangedGates(ctx, a.store, tx, stored.Key, held, a.now()); err != nil {
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
	}
	if err := a.keepStaleApproval(ctx, tx, stored.Key); err != nil {
		return err
	}
	// A root set back to todo is a new generation of the whole tree, so the old generation's pull
	// request, gate and handoffs go for every issue of it, not only the root's.
	return a.store.ClearTreeGeneration(ctx, tx, stored.Tree, a.now())
}

// keepStaleApproval keeps, for the architect start of the root key re-admission is clearing, the
// person's approval a gate of its holds of a version its document has since moved past: the new
// generation's architect registers each step again on a cleared gate, and its start task
// (workflow.Engine.ParkedDecisionTask) tells it to request approval of the latest version. One
// approval is kept, read from the first required step whose approved version sits below its
// latest, because readmitted_approvals keeps one row for each root; it carries that step, so the
// task names the document the person reviewed rather than the spec of every deployment.
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
	for _, step := range steps {
		gate, ok := gates[step]
		switch {
		case !ok:
			unregistered = true
		case gate.ApprovedVersion != nil && *gate.ApprovedVersion < gate.LatestVersion:
			return a.store.KeepReadmittedApproval(ctx, tx, key, record.KeptApproval{
				Step: step, Approved: *gate.ApprovedVersion, Latest: gate.LatestVersion})
		}
	}
	if unregistered {
		return nil
	}
	return a.store.DropReadmittedApproval(ctx, tx, key)
}

// releaseSlots releases the slot of every issue of this project that no longer needs one
// (record.NeedsSlot, the one statement of the release, whose phase half the grant reads too —
// record.KeepsSlot) and reports those issues in slot order. Each release is logged once the
// fact commits. It is the one release the daemon makes: a tree leaving the workflow, a root
// finishing, an issue reaching its done or its wait for a merge, and a linger all read the same
// predicate, so there is one rule and one place a slot can go.
//
// It reads the slotted issues in one query (record.Store.SlottedIssues), never a record per slot
// and never the whole issues table: every fact applies this under the one fact lock
// (intake.ApplyFact), so a read per slot cost every fact of every consumer, the API and the
// boot's replay a round trip to the database for each slot of the shared table, and a read of
// every record would grow with every issue the project has ever recorded. The read leaves out
// another project's slot, whose own daemon releases it. The further facts the predicate needs of
// each slotted issue — its tree root, and whether that root's tree is live — come from issues
// and facts, which the pass read once for all of them, so this still makes exactly one query.
func (a *Admission) releaseSlots(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts record.LineFacts) ([]string, error) {
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
		if record.NeedsSlot(issue, byKey[issue.Tree], running) {
			continue
		}
		if err := a.store.ReleaseSlot(ctx, tx, issue.Key); err != nil {
			return nil, fmt.Errorf("release the slot of %s: %w", issue.Key, err)
		}
		inUse--
		released = append(released, issue.Key)
		a.logSlot(ctx, "released", issue.Key, inUse)
	}
	return released, nil
}

// logSlot logs a slot released or taken once the fact commits: the issue, the slots the project
// holds after the change, and the cap.
func (a *Admission) logSlot(ctx context.Context, change, issue string, inUse int) {
	intake.OnCommit(ctx, func() { a.log.Info("admission: slot "+change, "issue", issue, "slots", inUse, "cap", a.cap) })
}

// promote gives each issue of the waiting line what its promotion needs — a slot, a tree opened,
// a first phase started, an architect resumed — in the line's own order, until the cap is full
// or the line empties, and reports how many candidates it promoted and the slots it released.
// Before it promotes any, every admitted root that now waits only on a person enters that wait
// (workflow's ReconcileAwaiting), every issue that no longer needs its slot gives it back
// (releaseSlots), and every root left in the workflow that nothing will ever start leaves it
// (releaseUnslotted), so each pass of admission — a live fact, the boot read, a release, a tick
// — repairs that state rather than keeping it. The pass stops at the first candidate either hold
// holds: one a.pending still names, since the Dispatch consumer has not yet reached the stream
// position Reconcile captured when it deferred that key, or one the boot's catch-up still holds
// (catchingUpFor). It stops rather than passing over it because the cap counts issues, so every
// candidate behind the held one competes for the same slots, and a slot is never taken back: a
// skip would invert Dispatch's rank order for good. A pass after the hold's release fills the
// free slots in line order. release, not promote, is what clears pending — it empties the whole
// set at once, so this need only check membership. An admitted root's architect start is queued
// behind a row creating its issue branch (record.IssueBranch), which the start waits for.
func (a *Admission) promote(ctx context.Context, tx pgx.Tx) (int, []string, error) {
	return a.promoteHolds(ctx, tx, true)
}

// promoteHolds is promote; respectHolds false is release's own call, once every held key's own
// summary has already been applied: release always ignores every hold, never a subset, since it
// only ever runs once the consumer has caught up to every key Reconcile deferred. The in-memory
// pending set itself only clears after that whole call's transaction commits (see release), so
// without this, the pass's own membership check would hold every one of them back in the very
// call that released them. A queued root a live tree took in (adoptQueuedRoots) leaves the line
// before anything else reads it.
//
// It makes one read of each kind for the whole pass and shares it: one record.Store.LiveIssues,
// one record.Store.Slots, one bulk tree-lifecycle read, one design-gate read, one read of the
// starts the project owes its workers (the three of record.ReadLineFacts) and one read of the
// Dispatch mirror rows of everything it judges (blockers.Judged, blockers.Load). The release,
// the waiting line, the INFRA-129 safety net, the promotion and the two repair passes all judge
// from those, so no path reads per slot, per tree or per issue under the one fact lock every
// fact takes (intake.ApplyFact). A read is made again only where this pass has itself written
// something that would make it stale (reread).
//
// Every path that promotes comes through here — a live fact, the controller tick, a hold's
// release, the boot's Reconcile — so each of them is also a look at every issue a blocker holds,
// at every slot whose worker has lost its task, and at every tree a step that left gates.design
// released (record.Waiting's LineFirstPhase).
func (a *Admission) promoteHolds(ctx context.Context, tx pgx.Tx, respectHolds bool) (int, []string, error) {
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
	// so what follows reads them as they now stand: an answer it recorded puts its root in the
	// first part of this pass's waiting line, among the issues whose work came back, oldest first
	// (record.Waiting), at the moment its own wait was answered.
	if changed {
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return 0, nil, err
		}
	}
	released, err := a.releaseSlots(ctx, tx, issues, facts)
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
	rows, facts, line, _, err := blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, nil, a.now())
	if err != nil {
		return 0, released, err
	}
	unslotted, err := a.releaseUnslotted(ctx, tx, issues, record.LiveRoots(issues, facts), facts.ChildWaits, respectHolds)
	if err != nil {
		return 0, released, err
	}
	if unslotted {
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return 0, released, err
		}
		if rows, facts, line, _, err = blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, rows, a.now()); err != nil {
			return 0, released, err
		}
	}
	if err := a.tellChildrenFirst(ctx, tx, issues, facts); err != nil {
		return 0, released, err
	}
	if a.cap <= 0 {
		return 0, released, nil
	}
	admitted := 0
	for {
		own := ownSlots(issues, slots)
		stopped, opened := false, false
		for _, candidate := range line {
			issue := candidate.Issue
			// A hold stops the whole pass, the candidates behind this one included. Every
			// candidate competes for the same slots now that the cap counts the project's
			// issues rather than its trees, so passing one over would invert Dispatch's rank
			// order for all of them and not only inside the held candidate's own tree — and a
			// slot is never taken back, so that inversion would be permanent. The design is
			// that the daemon gives the first issue of the line its slot; skipping the first
			// issue is not that. The cost is bounded: a hold ends when the Dispatch consumer
			// reaches the position Reconcile captured for it, and every fact, tick, hold
			// release and boot runs this pass again, so a hold delays a pass rather than
			// holding a state. heldKeys is the one predicate, the same closure
			// ReconcileAwaiting was given above: with respectHolds false — release's own call —
			// it is the boot catch-up's hold alone, so a release bypasses the Dispatch
			// consumer's hold and never the catch-up's.
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
			if waits := blockers.LineWaitsOn(rows, candidate); len(waits) > 0 {
				a.log.Info("admission waits for what this issue waits on", "issue", issue.Key, "blockers", waits)
				continue
			}
			// A candidate that already holds a row, or that stands in a phase keeping none,
			// takes no slot and so is never held by the cap: a root another tree handed to
			// Legion carrying the phase it ran there opens its tree at once on the row it
			// brought, and an answered root resumes its architect on the row it kept through
			// the wait.
			takes := slotFor(candidate)
			if takes && len(own) >= a.cap {
				// A full cap gives no further slot, and the scan goes on. This is not the hold
				// above and must not be read as one: a hold is an answer for every candidate
				// behind it, because it says the pass does not yet know what this candidate is;
				// the cap is an answer for this candidate alone, because it says only that there
				// is no slot for it. own only grows inside this loop, so no candidate behind
				// this one takes a slot either and the rank order of the issues that need one is
				// kept whole, while the candidates the cap cannot bind — the ones slotFor is
				// false for — are promoted as they are reached, which is what makes a hand-over
				// and an answered root resume under a cap that is permanently full.
				//
				// An answered root this branch passes over is told nothing. It reads where it
				// stands from its own record — `waitingLine`, which is the live place
				// (api.WaitingLineView) — and the daemon resumes it by itself on the pass that
				// finds a slot free, so a message would carry a snapshot of a place that moves
				// with every pass and nothing besides.
				continue
			}
			if candidate.Kind == record.LineAnswered {
				// The root's tree already runs: only a reason it may not resume in its own
				// session holds it here (its label gone, a foreign Dispatch claim), which
				// releaseUnslotted takes out of the workflow on this same pass. Its links are
				// not re-read, as no running tree's are.
				reason, err := workflow.Unresumable(ctx, a.store, tx, issue)
				if err != nil {
					return admitted, released, fmt.Errorf("check whether answered root %s can resume: %w", issue.Key, err)
				}
				if reason != "" {
					a.log.Warn("admission skips an answered root it may not resume", "issue", issue.Key, "reason", reason)
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
						continue
					}
					// The operator opened a tree under this root's key: this root waits until
					// that tree's cleanup confirms, and the candidates behind it go on.
					if errors.Is(err, treelifecycle.ErrAuthorityHeld) {
						a.log.Warn("admission waits for its tree's other authority", "issue", issue.Key, "tree", issue.Tree, "error", err)
						continue
					}
					return admitted, released, fmt.Errorf("open admission lifecycle of %s: %w", issue.Key, err)
				}
				// The tree's row was created by this call, so this is the one moment its memory
				// group is drawn, in the fact's own transaction: a crash before the commit leaves
				// neither the lifecycle nor the draw. Every later open of the tree — its next
				// epoch after a cleanup, and every re-admission — creates nothing and draws
				// nothing, and the draw itself is written once (record.Store.DrawMemory).
				if lifecycle.Created {
					if err := memorygroup.DrawAndRecord(ctx, tx, a.store, issue.Tree, a.memory, a.coin, a.now()); err != nil {
						return admitted, released, err
					}
				}
			}
			now := a.now()
			if takes {
				// An entry carries whether its issue held a row when the line was built
				// (record.LineEntry.Holds), and the line this loop reads holds each issue once,
				// so no grant here changes the answer slotFor gives a candidate behind it; what
				// the grant does change, the count against the cap, is own, which grows with
				// it. finishFencedStarts keeps the line to one entry an issue, not record.Waiting:
				// Waiting would list an issue at admitted that holds no row and still owes a start
				// both as a return and as a first phase, but every owed start names a worker
				// phase, and finishFencedStarts ends each one whose phase is not the issue's own
				// before the line is built, so none is left owed at admitted.
				slot := record.Slot{Issue: issue.Key, Index: nextSlotIndex(slots), AdmittedAt: now}
				if err := a.store.PutSlot(ctx, tx, slot); err != nil {
					return admitted, released, fmt.Errorf("put admission slot for %s: %w", issue.Key, err)
				}
				slots, own = append(slots, slot), append(own, slot)
				a.logSlot(ctx, "taken", issue.Key, len(own))
			}
			switch candidate.Kind {
			case record.LineReturning:
				// The transition that sent this issue's work back wrote its worker's task in its
				// own transaction and left it unclaimable until a slot row of this issue existed
				// (record.SuperviseRequest.NeedsSlot). The slot above is the whole promotion:
				// the row is claimable the moment it commits, and the grant wakes the issue's
				// lifecycle lane (the daemon's projectRecords.PutSlot), so the task is delivered
				// in the same second rather than at the next sweep.
				a.log.Info("admission gives a slot back to an issue whose work came back", "issue", issue.Key, "phase", issue.Phase)
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
				if err := a.store.PutIssue(ctx, tx, issue); err != nil {
					return admitted, released, fmt.Errorf("record admitted issue %s: %w", issue.Key, err)
				}
				if writeInProgress {
					if err := a.enqueue(ctx, tx, issue.Key, record.OrdinaryStatusWrite("in_progress", "todo", ""), now); err != nil {
						return admitted, released, err
					}
				}
				// ResumeAwaitedRoot is the resume's handout (record.Store.StartHandout).
				if err := a.engine.ResumeAwaitedRoot(ctx, tx, issue, *awaiting); err != nil {
					return admitted, released, fmt.Errorf("resume %s after a person's answer: %w", issue.Key, err)
				}
				opened = opened || facts.Gates[issue.Tree].Open()
			case record.LineQueuedRoot:
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
				// says it is a root (record.ChildOpen).
				// Read before it goes: the first task below reads the mark for the one shape it
				// decides, a root a hand-over carried in at the phase that came with it
				// (workflow.ArchitectStart.Carried).
				carried := issue.HandedOverFrom != ""
				issue.HandedOverFrom = ""
				if err := a.store.PutIssue(ctx, tx, issue); err != nil {
					return admitted, released, fmt.Errorf("record admitted issue %s: %w", issue.Key, err)
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
				decided, kept, err := a.engine.ParkedDecisionTask(ctx, tx, issue)
				if err != nil {
					return admitted, released, fmt.Errorf("name the decisions on %s while nobody in its tree could hear them: %w", issue.Key, err)
				}
				// The first task, if the derivation serves this root one: workflow.ArchitectStartTask
				// says which of its shapes a root gets, and why a root it serves no task is started
				// with what its tree is owed alone.
				launches, err := a.store.ArchitectLaunches(ctx, tx, []string{issue.Tree})
				if err != nil {
					return admitted, released, fmt.Errorf("read what generation %s's architect claim stands at: %w", issue.Key, err)
				}
				task := workflow.ArchitectStartTask(workflow.ArchitectStart{Root: issue, Carried: carried,
					Gate: facts.Gates[issue.Tree], Launch: launches[issue.Tree], Decided: decided})
				// The start hands the architect work it was not on, so it raises the architect's
				// phase entry and writes that task as the work of it on the architect's phases
				// row, in this transaction (workflow.Engine.EnterArchitect), and carries the
				// number itself (record.SuperviseRequest.Entry). A root's architect holds that
				// row exactly as every phase worker does: the work is a fact of the record rather
				// than of the one outbox row that carried it, so a start that reaches no claim
				// loses none of it, and the outbox reads the entry off this row as it does for
				// every other role.
				//
				// A root the derivation serves no task — one past admitted under its own power,
				// with nothing decided while its tree could not hear it — is handed nothing, so
				// it raises no entry: the start carries no task, nothing is delivered for it, and
				// an entry raised for it would leave the architect's row above what its claim was
				// ever given with no work to send it.
				var entry int64
				if task != "" {
					if entry, err = a.engine.EnterArchitect(ctx, tx, issue, task); err != nil {
						return admitted, released, fmt.Errorf("raise the phase entry of %s's architect: %w", issue.Key, err)
					}
				}
				// The architect start is not one of the starts that wait for a slot: this very
				// transaction grants the tree whatever slot it needs, and an architect starts no
				// phase worker.
				if err := a.engine.EnqueueNaming(ctx, tx, issue.Key, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree,
					Role: claim.RoleArchitect, Generation: issue.Generation, Task: task, Entry: entry, Kept: kept}, kept, now); err != nil {
					return admitted, released, fmt.Errorf("enqueue the architect start of %s: %w", issue.Key, err)
				}
				if err := a.startMidPhaseMembers(ctx, tx, issue, append(issues, issue), now); err != nil {
					return admitted, released, err
				}
				if err := a.reenterStrandedChildren(ctx, tx, issue, issues); err != nil {
					return admitted, released, err
				}
				opened = opened || facts.Gates[issue.Tree].Open()
			case record.LineFirstPhase:
				if err := a.engine.StartFirstPhase(ctx, tx, issue, rows[issue.Key].Blockers); err != nil {
					return admitted, released, err
				}
			}
			admitted++
		}
		// A resume, or a tree opened above whose design gate already stands open, releases
		// members of its own tree that stood at admitted on the snapshot this round read and
		// would otherwise wait a whole fact for their first phase. The records are read again
		// and the line rebuilt so they are promoted in this pass; a pass a hold stopped goes no
		// further, since the hold is the answer for everything behind it too.
		if stopped || !opened {
			break
		}
		if issues, slots, facts, err = a.reread(ctx, tx); err != nil {
			return admitted, released, err
		}
		if rows, facts, line, _, err = blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, rows, a.now()); err != nil {
			return admitted, released, err
		}
	}
	if err := a.resumeSlottedWorkers(ctx, tx, issues, slots, facts); err != nil {
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
	facts, err := record.ReadLineFacts(ctx, tx, a.store, a.project, issues, a.engine.GatedSteps())
	return issues, slots, facts, err
}

// slotFor says whether promoting this candidate must take one of the cap's slots for it: it
// holds none already (record.LineEntry.Holds, which the line sets from the slots it was built
// from) and its phase keeps one (record.KeepsSlot). The grant and the release read that one
// predicate, so no pass hands out a row the next pass would take back.
//
// It reads the phase the candidate's record carries rather than the phase its promotion leaves
// it in, and the two agree wherever this is asked: KeepsSlot refuses a phase of done and a
// Dispatch status out of the workflow alone, and no promotion moves an issue into or out of
// either. A first phase starts at phase.Planning from a record standing at admitted, an
// answered root goes from awaiting_person back to admitted, a returning issue keeps the worker
// phase it stands in, and a root whose tree is opened keeps the phase it ran in the tree it
// left, which keeps a row like any other phase short of done.
//
// A candidate that holds a row takes no second one: the line carries holders (the kinds
// record.LineEntry.Holds names). A candidate that takes no slot is promoted whatever the cap:
// holding it back would bound nothing and would leave its tree closed for no reason.
func slotFor(entry record.LineEntry) bool {
	return !entry.Holds && record.KeepsSlot(entry.Issue)
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

// ReleaseCatchUp ends key's catch-up hold; the next pass decides it.
func (a *Admission) ReleaseCatchUp(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.catchingUp, key)
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

// tellChildrenFirst tells the architect of each parent whose own first phase now waits for its
// Dispatch children, once per issue and per run (record.Store.TellChildrenFirst). The notice
// names the parent, each open child with the Dispatch status that holds it, and each cycle
// among them (workflow.ChildrenFirstNotice).
//
// The wait is judged again every pass and the telling is written once, which is the whole of
// why the two are recorded differently: every fact, tick, hold release and boot runs this pass,
// so a notice decided by the wait alone would reach the architect over and over for one wait,
// while a wait decided by a stored flag would outlive the child that caused it. A child filed
// later during one wait therefore tells nobody again — the architect already knows the issue
// waits, and the state names what for — and a re-admission, which opens a new generation, tells
// it anew, because that run's architect has not heard it.
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
		a.log.Info("admission: an issue waits for the Dispatch children under it", "issue", key,
			"tree", parent.Tree, "generation", parent.Generation, "children", len(facts.ChildWaits[key].Open))
	}
	return nil
}

// waitsForChildren says whether an issue's own first phase is held because a Dispatch child of
// its own is still open — the children-first rule of dispatch://INFRA-980. It is the named seam
// the safety net below asks, so that the rule arrives in one place rather than as a condition
// spread through this file, and waits is the pass's own judgement of it
// (record.LineFacts.ChildWaits, filled by blockers.JudgeLine from record.ChildWaits): never a stored
// flag, so a parent whose last open child has gone waits no longer than the pass that reads it.
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
// daemon has started, so a root waiting in the line and one whose tree has left the workflow
// hold none, while a root in accepting, in merging, waiting on a person, on a blocker or for
// its children keeps the row it was started with. Every one of those is a tree whose lifecycle
// stands open under the workflow, or whose root waits in the line or on a person, which is
// exactly record.LiveRoots: so liveness is the term that replaces the old slot test, and it
// covers the slots, the line and the awaiting_person wait together. The children-first wait is
// asked of the issue as well, so that a later rule which holds an issue somewhere liveness does
// not reach cannot silently have its work taken out of the workflow.
//
// What a root's own Dispatch links say is not asked, though it reads as a wait of the same
// kind. A root the line holds behind an unfinished link is in todo, which this rule has already
// passed over, and the links of a root at a running status are read by nothing: its tree was
// opened under whatever they said then, and a link added since re-checks nothing (the line's
// own blockers.LineWaitsOn). Asking blockers.WaitsOn here answered from the mirror rows this pass
// happened to read for other issues, so whether the safety net spared a root turned on whether
// something else had named it.
//
// What is left is a root whose tree nothing will ever open again, recorded at a running status.
// The workflow takes a root out on a status out of the workflow (its leave) but does nothing on a
// running tree's status where no tree runs, so only such a move made while the root waited leaves
// one here: a person's, or an outside session's, which is a person's where no tree runs
// (workflow's sessionStatusWrite). A root in todo without the label is not one: taking the label
// off is a pause the label ends.
func (a *Admission) unslotted(issue record.Issue, live map[string]struct{}, waits map[string]record.ChildWait) bool {
	if issue.Project != a.project || !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.Lingers() || !record.InWorkflow(issue) || issue.AwaitingPerson() {
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
		reason += "; the daemon took it out of the workflow and starts nothing for it; todo with the legion label queues it again"
		if err := a.enqueue(ctx, tx, issue.Key, record.ControllerNotice{Kind: record.UnslottedNotice, Reason: reason}, a.now()); err != nil {
			return released, err
		}
		intake.OnCommit(ctx, func() {
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
	if issue.Project != a.project || !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.Lingers() || !record.InWorkflow(issue) || !issue.AwaitingPerson() {
		return "", false, nil
	}
	reason, err := workflow.Unresumable(ctx, a.store, tx, issue)
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

// refreshHeldSummary keeps a held, unrecorded key's own pending summary — the one release
// eventually applies — current with a live event that is newer than it but creates no record: an
// event moving the key out of todo, or taking its label off, teaches nothing to the record (there
// is none yet), but it is real information about what Dispatch shows now. Left alone, release
// would apply the stale boot summary Reconcile originally deferred, as if this event had never
// arrived. A key that is not held, or an event at or behind the held summary's own sequence, is
// left untouched.
func (a *Admission) refreshHeldSummary(observation intake.DispatchIssue) {
	a.mu.Lock()
	defer a.mu.Unlock()
	summary, held := a.pending[observation.Key]
	if !held || observation.Seq <= summary.Seq {
		return
	}
	a.pending[observation.Key] = intake.DispatchIssue{
		Key: observation.Key, Seq: observation.Seq, Status: observation.Status, Title: observation.Title,
		Parent: observation.Parent, Rank: observation.Rank, HandedOver: observation.HandedOver,
		ResolvedRepo: observation.ResolvedRepo, Ancestry: observation.Ancestry,
	}
}

// release applies the Dispatch consumer's current position: once it has Reached target, every key
// Reconcile deferred is re-applied through applySummary — the listing's own snapshot for it, in
// case the stream never delivers the event that would otherwise have caught it up — before
// promote decides anything, so a held key is never admitted, or left admitted, on stale
// information. The in-memory pending set only clears once this call's own transaction has
// actually committed (intake.OnCommit): clearing it any earlier would say a hold is
// resolved while a failed commit leaves nothing of this release durable, so the next
// promote-triggering call would trust an unreleased hold's authority.
func (a *Admission) release(ctx context.Context, tx pgx.Tx, position intake.DispatchConsumerPosition) ([]string, error) {
	a.mu.Lock()
	caughtUp := len(a.pending) > 0 && position.Reached(a.target)
	var releasing map[string]intake.DispatchIssue
	if caughtUp {
		releasing = maps.Clone(a.pending)
	}
	a.mu.Unlock()

	var released []string
	if caughtUp {
		issues, err := a.store.LiveIssues(ctx, tx, a.project)
		if err != nil {
			return nil, fmt.Errorf("list admission issues: %w", err)
		}
		facts, err := record.ReadLineFacts(ctx, tx, a.store, a.project, issues, a.engine.GatedSteps())
		if err != nil {
			return nil, err
		}
		live := record.LiveRoots(issues, facts)
		held := slices.SortedFunc(maps.Values(releasing), func(x, y intake.DispatchIssue) int { return cmp.Compare(x.Key, y.Key) })
		for _, summary := range parentsFirst(held) {
			if err := a.applySummary(ctx, tx, summary, live); err != nil {
				return nil, err
			}
		}
	}
	admitted, returned, err := a.promoteHolds(ctx, tx, !caughtUp)
	if err != nil {
		return nil, err
	}
	if !caughtUp {
		return returned, nil
	}
	a.log.Info("admission: the Dispatch consumer has caught up; releasing held roots",
		"count", len(releasing), "ack_floor", position.AckFloorStream, "idle", position.Idle, "admitted", admitted)
	intake.OnCommit(ctx, func() {
		a.mu.Lock()
		for key := range releasing {
			delete(a.pending, key)
		}
		a.mu.Unlock()
	})
	return append(released, returned...), nil
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

// startMidPhaseMembers brings back the roles that have run of every member of the tree promotion
// has just opened, and gives the one role whose phase the member stands in the task that carries
// that phase on. A hand-over turns a child into that root while preserving its phase, so its phase
// role needs the same resume start ordinary mid-phase children receive
// (workflow.ResumePhaseTask, which says to carry the phase's work on and names no head, since a
// merger reads the head it merges on GitHub). A root promoted held, which only a
// hand-over leaves (every other re-admission starts its root at admitted), starts no worker, and
// the hold's notice went to the architect of the tree it was handed over from, so its own
// architect is told of the hold (workflow.HeldNotice), whether or not it carried a gate.
// Promotion already queued the root's branch; each other member queues its own before starting.
// No member's phase is rewritten on the way back: the phase its record carries is the phase it
// stands in, since the merge is what ends merging and no phase of this workflow is a wait for
// one. A child of a re-admitted tree, and a root a hand-over created, come back in merging where
// they stood in it, and the merge their new tree's merger makes is what ends that phase: the old
// tree's linger stopped the merge its own merger had not made (beginLinger).
//
// It brings back a member's other roles beside the phase's own, because the admission cap counts
// the issues the daemon has started now: the one start that waits for a slot is the phase
// worker's, which carries that phase's task (workflow.StartOrOwe), and the live sessions of a
// member's other roles are exactly what the design keeps so that a person can still talk to any of
// them. Those rows carry no task and wait for no slot, so each is claimable at once and a role
// with nothing to do comes back and is told nothing.
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
// What an issue waits on is deliberately not read here: every member this starts is mid-phase,
// work already under way, and a member standing at admitted is passed over, since nothing of it
// has run and its first phase is the waiting line's to start.
func (a *Admission) startMidPhaseMembers(ctx context.Context, tx pgx.Tx, root record.Issue, issues []record.Issue, now time.Time) error {
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
		// owner is the role whose phase the member stands in, empty where that phase is the
		// tree architect's.
		owner claim.Role
		roles []roleToken
	}
	// The first pass reads each member where its record leaves it and names every role run the
	// second pass judges; the one read of them stands between the two, so no row is written on a
	// reading of the claims half of it made.
	var members []midPhaseMember
	var keys []record.RoleRunKey
	for _, member := range issues {
		if member.Tree != root.Tree || record.OutOfWorkflow(member.Status) {
			continue
		}
		// A member standing at admitted has run nothing in this generation: it holds no session
		// to bring back, and its own first phase starts when the waiting line gives it a slot
		// (record.LineFirstPhase). A root whose tree is opened for the first time is exactly
		// that, so admitting a fresh root relaunches nobody and creates no claim for a worker
		// that has never worked.
		if member.Phase == phase.Admitted {
			continue
		}
		// Accepting's role is the tree's architect, which promotion already started: nothing of
		// that phase is resumed here.
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
		standing := midPhaseMember{issue: member, owner: owner}
		for _, role := range claim.Roles {
			// The tree's architect is promotion's own start, with the task that hands it its
			// tree; a member's own sub-architect is no phase worker and comes back with the rest.
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
	for _, standing := range members {
		member := standing.issue
		if member.Key != root.Key {
			if err := a.enqueue(ctx, tx, member.Key, record.IssueBranch{Generation: member.Generation}, now); err != nil {
				return err
			}
		}
		for _, held := range standing.roles {
			run := runs[held.token]
			if held.role != standing.owner {
				// No claim row, so the role has never run: there is no session to bring back and
				// no task this start could carry, and the one it would make could only ever be
				// woken by a message.
				if run.Claim == nil {
					continue
				}
				if err := a.enqueue(ctx, tx, member.Key, record.SuperviseRequest{Op: "start", Tree: member.Tree, Role: held.role,
					Generation: member.Generation, Phase: member.Phase}, now); err != nil {
					return err
				}
				continue
			}
			if !workflow.StartFor(run, root, member.Generation, member.Phase) {
				continue
			}
			if err := a.engine.StartPromotedWorker(ctx, tx, member, held.role); err != nil {
				return err
			}
		}
	}
	return nil
}

// owedEntryRole is one role the entry work is owed to: the issue whose current phase it is the
// role of, or the root whose own architect it is, the phases row the work and the entry are read
// from, and the entry that role's claim was last given, which only the log line of the send
// reads. It stays unexported: the shape of this selection is this package's, and the one
// question another package asks of it is OwesEntryWork's yes or no.
type owedEntryRole struct {
	Issue record.Issue
	Role  claim.Role
	Row   record.PhaseRow
	Given int64
}

// OwesEntryWork is whether the entry sweep's own pass would do anything for the project, read as
// every admission pass reads the project (reread) and answered by the two selections that pass
// itself acts on: a role owed the work of the entry its phases row names (owedEntryRoles), and an
// arm of the stall reading that holds for a slotted root's architect and that no mark has told
// the controller yet (architectStalls, record.Store.ArchitectStallsUntold). It writes nothing, so
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
// of the project, which can be its own hourly tick. The arms add one statement, the read of their
// marks, and only where an arm holds: the one arm that reads the record itself
// (architectTookNothing) reads it only for a root its own terms already hold.
func (a *Admission) OwesEntryWork(ctx context.Context, tx pgx.Tx) (bool, error) {
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
	stalls, err := a.architectStalls(ctx, tx, read, facts, nil)
	if err != nil {
		return false, err
	}
	return a.store.ArchitectStallsUntold(ctx, tx, stallMarks(stalls))
}

// owedEntryRoles is every role of a slotted issue that is behind the entry its phases row names
// (workflow.OwedEntry), in the order the slots are read. It is the selection half of the repair
// resumeSlottedWorkers sends, and the only statement of which roles are owed that work: a reader
// asking whether anything is owed asks this rather than restating it, so no second statement can
// disagree with the pass. Every role of the daemon is inside it, the root architect among them.
//
// Only the role of each slotted issue's current phase is read (workflow.OpenPhaseRole, in the
// selection both repairs over phase workers make, phaseWorkerOf), and a root's own architect
// (rootArchitectOf), so a role the issue's phase has left is no part of the answer: its phases
// row keeps the entry it was last handed work on, and nothing here would ever send that work
// again. The phase's own worker is held where a worker of that issue has work where it stands
// (readSlotHolders, record.AtWork).
//
// The root's own architect is the second role, and record.AtWork is not its reading: that rule
// answers false for a root at admitted once its design gate has opened, where the architect is
// still the only role of the tree at work, and a relaunched architect of exactly such a root is
// the measured defect (dispatch://INFRA-2614). Every slotted root's architect is weighed here,
// and the reach that decides which of them is owed anything is read once its claim is behind
// (architectReached, workflow.ArchitectAtWork): a root at admitted whatever its gate, and a root
// a hand-over carried in while it stands in the phase that came with it. So the architects this
// names are exactly those the pass sends a start to, and the sweep's gate opens for no root the
// pass would send nothing.
//
// The slots are walked here, in the order the store answered, and the issues they hold are what
// the selection is given: it is the tick's silent list that walks the project's live issues
// (silentWorkers). What the loop below asks beyond that selection is this repair's own
// judgement, and the skips are in the order a reader of the two repairs must see them.
//
// The whole selection asks the same statements whatever number of slots the project holds,
// which is what every fact of the project pays (the statements this fact asks): the two batched
// reads of readSlotHolders, and, for the rare holder that is owed something, that holder's own
// reads — the architect's reach, and the phases row whose work is sent. It answers the reading
// beside the selection, since the stall reading the pass makes after its sends reads the same
// holders (architectStalls).
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

// slotHolder is one role of a slotted issue the rule weighs: the issue, its tree's root, the
// role and its claim token, in the shape both selections answer (phaseWorker: phaseWorkerOf and
// rootArchitectOf), and when the issue was admitted (record.Slot.AdmittedAt), which the gate arm
// of the stall reading weighs its bound from (architectStalls).
type slotHolder struct {
	phaseWorker
	admittedAt time.Time
}

// slotReading is what one pass reads of its slot holders, once: the holders, the run of each
// holder's role (record.RoleRun) and the phase entry each holder's row names with the moment it
// was raised (record.Store.PhaseEntries). The selection of the owed roles (owedOf) and the stall
// reading of the roots' architects (architectStalls) both read it, so the pass that sends and
// then wakes asks the two batched statements once.
type slotReading struct {
	holders []slotHolder
	runs    map[claim.Token]record.RoleRun
	entries map[record.PhaseRole]record.PhaseEntry
}

// readSlotHolders is the reading owedEntryRoles weighs, over every slot of the project. Only the
// role of each slotted issue's current phase is held, and a root's own architect.
//
// Which role of a slotted issue the rule reaches is two questions, because two roles can stand
// on one issue. The phase's own worker is held where a worker of that issue has work where it
// stands: a slot no longer says one has — an issue keeps its row through phases no worker of it
// works (record.KeepsSlot) — so a holder record.AtWork answers false for is passed over, at the
// skip the shared selection states (phaseWorkerOf). The caller hands that selection the facts of
// its own pass, so the gate AtWork is asked with is the issue's real one and the call stays
// honest against its contract, at one map lookup a holder.
//
// It reads over at most the cap's worth of slotted issues, through one batched record.RoleRuns
// read rather than a read per slot, and one batched read of the phase entry each holder's row
// names with the moment it was raised (record.Store.PhaseEntries), the zero entry where the
// record holds no row of that role. Both are one statement for the whole pass: every slotted
// root's architect is weighed beside its phase's worker on every fact of the project, and a
// statement a slot is hundreds of round trips a fact for a project running hundreds of trees
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
	for _, slot := range slots {
		issue, recorded := byKey[slot.Issue]
		if !recorded {
			continue
		}
		if worker, selected := a.phaseWorkerOf(project, issue, byKey, facts); selected {
			hold(worker, slot.AdmittedAt)
		}
		// The architect of a root is weighed beside the phase's worker, because two roles can
		// stand on one issue: a root at admitted holds no phase worker at all, and a root a
		// hand-over carried in has both its phase's worker and an architect that has still to
		// be told the tree is its own.
		if architect, selected := a.rootArchitectOf(project, issue); selected {
			hold(architect, slot.AdmittedAt)
		}
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
// the reading the pass has already made.
func (a *Admission) owedOf(ctx context.Context, tx pgx.Tx, read slotReading) ([]owedEntryRole, error) {
	var owed []owedEntryRole
	for _, held := range read.holders {
		run := read.runs[held.token]
		entry := read.entries[record.PhaseRole{Issue: held.issue.Key, Role: held.role}]
		// A root architect whose phases row names no entry, beside a claim the daemon holds, is
		// weighed here though OwedEntry reads nothing as owed of it: that rule compares two
		// numbers, and this role is the one the sweep hands an entry rather than resumes
		// (resumeSlottedWorkers' raise). The claim is part of it because a claim is what says
		// this architect was started: a root with neither claim nor entry was started by
		// nothing, and the promotion that starts it is what hands it its first work. Every
		// other role of a row with no entry is owed none, and is passed over by that rule.
		raises := held.role == claim.RoleArchitect && entry.Entry == 0 && run.Claim != nil
		if !raises && !workflow.OwedEntry(run, held.root, entry.Entry) {
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
		// given, and sending that entry's task is what brings its pod back.

		// A claim an operator suspended is passed over whatever entry it stands on: `legion
		// claims stop` stopped that worker and only the operator's own resume starts it again
		// (supervise's RuleHolds, which no start the daemon queues gets past). A claim the
		// daemon itself suspended is repaired, which is what the idle rule's release and the
		// daemon's own suspend both leave behind.
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
		if held.role == claim.RoleArchitect {
			reached, err := a.architectReached(ctx, tx, held.issue, run)
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
		var given int64
		if c := run.Claim; c != nil {
			given = c.GivenEntry
		}
		owed = append(owed, owedEntryRole{Issue: held.issue, Role: held.role, Row: row, Given: given})
	}
	return owed, nil
}

// architectReached says whether a root architect behind the entry its phases row names is owed a
// start, which is the architect's half of owedEntryRoles' rule. Three things stop it. A start
// already on its way is not owed twice (workflow.ArchitectStartPending): a row still queued, a
// claim still starting, or a task the claim has not confirmed. While such a row waits, every
// later pass reads it and names nothing, and that reading is pure, so it is asked first. A root
// outside the reach is owed nothing, and neither is an architect that cannot hear what a start
// would send (architectServed).
//
// The same three hold for an architect whose phases row names no entry at all, which the pass
// hands an entry rather than resumes (resumeSlottedWorkers, workflow.RaiseArchitectEntryAndStart).
func (a *Admission) architectReached(ctx context.Context, tx pgx.Tx, root record.Issue, run record.RoleRun) (bool, error) {
	if _, pending := workflow.ArchitectStartPending(run); pending {
		return false, nil
	}
	return a.architectServed(ctx, tx, root)
}

// architectServed is whether a slotted root's own architect is one the sweep has work for and
// can tell it: the two readings the sweep's send (architectReached) and the first stall arm
// (architectTookNothing) both stand on. Its architect must be able to hear
// (workflow.ArchitectHearsSlotted, the slotted form of the daemon's one reading of that
// question, workflow.ArchitectHears: a root that lingers, one Dispatch has taken out of the
// workflow, and an issue that is not its tree's root, which is what leaves a child's own
// sub-architect claim outside the rule, since no daemon start creates one). The slotted form
// reads nothing, because root holds one of the slots this pass read and so waits for none. And
// the root must be inside the reach (workflow.ArchitectAtWork): a root standing at admitted,
// whatever its design gate, and a root a hand-over carried in while it stands in the phase that
// came with it, and no root past admitted under its own power, whose phase role owns its work.
// The pure reading is asked first, so the reach's read of the hand-over row is made only for a
// root that hears.
func (a *Admission) architectServed(ctx context.Context, tx pgx.Tx, root record.Issue) (bool, error) {
	if !workflow.ArchitectHearsSlotted(root) {
		return false, nil
	}
	reached, err := workflow.ArchitectAtWork(ctx, tx, a.store, root)
	if err != nil {
		return false, fmt.Errorf("read whether the architect of %s is the role at work there: %w", root.Key, err)
	}
	return reached, nil
}

// architectStall is one arm the stall reading raises over one slotted root on a pass: the root,
// the arm, what the record says, which the wake's reason carries after the arm's own words, and
// the moment the pass read it.
type architectStall struct {
	root record.Issue
	arm  record.ArchitectStallArm
	why  string
	at   time.Time
}

// mark is the stall as the mark that keeps its arm to one wake keys it (record.ArchitectStall).
func (s architectStall) mark() record.ArchitectStall {
	return record.ArchitectStall{Issue: s.root.Key, Tree: s.root.Tree, Generation: s.root.Generation, Arm: s.arm, At: s.at}
}

// stallMarks is the mark of each of stalls, in their order.
func stallMarks(stalls []architectStall) []record.ArchitectStall {
	marks := make([]record.ArchitectStall, len(stalls))
	for i, stall := range stalls {
		marks[i] = stall.mark()
	}
	return marks
}

// architectStalls reads the three arms over the architect of every slotted root of read, in the
// order the holders are read and, for each root, in the order a controller is told them: the
// architect took nothing, its claim cannot be reached, the root is slow before its gate. sent
// names the roots whose architect start this pass wrote (resumeSlottedWorkers), which the second
// arm's reason says; the sweep's gate passes none, since it asks only where the pass would send
// nothing (OwesEntryWork). Each root's arms are read at that root's own reading of the clock.
func (a *Admission) architectStalls(ctx context.Context, tx pgx.Tx, read slotReading, facts record.LineFacts,
	sent map[string]bool,
) ([]architectStall, error) {
	var stalls []architectStall
	for _, held := range read.holders {
		if held.role != claim.RoleArchitect {
			continue
		}
		root, run := held.issue, read.runs[held.token]
		row := read.entries[record.PhaseRole{Issue: root.Key, Role: claim.RoleArchitect}]
		now := a.now()
		took, err := a.architectTookNothing(ctx, tx, root, run, now)
		if err != nil {
			return nil, err
		}
		for _, arm := range []struct {
			arm record.ArchitectStallArm
			why string
		}{
			{record.ArchitectTookNothing, took},
			{record.ArchitectClaimUnreachable, architectClaimUnreachable(root, row, run, sent[root.Key], now)},
			{record.ArchitectSlowBeforeGate, architectSlowBeforeGate(root, facts.Gates[root.Tree], held.admittedAt, row, now)},
		} {
			if arm.why != "" {
				stalls = append(stalls, architectStall{root: root, arm: arm.arm, why: arm.why, at: now})
			}
		}
	}
	return stalls, nil
}

// wakeStalledArchitects wakes the project's controller once for each of stalls no mark has told
// yet, once per run of its root's tree (record.Store.TellArchitectsStalled). The pass calls it
// after it has sent every role the work its phase entry names, so each arm reads a root the
// daemon has already done what it can for.
//
// Every stall of the pass is marked in one statement, and a stall marked already answers false,
// so a root that stays stalled costs no statement of its own on the facts after its telling. The
// wakes are queued for the stalls that statement told, in the order the pass read them, and per
// issue rather than through wakeController, which keeps one unpublished notice per project and
// kind: every root of a project whose architects are stuck owes a wake of its own, and the
// project-wide rule would let the first root's wake hide every other root's. What keeps one
// root's arm to one wake is the mark instead, which is keyed by the arm as well, so a root two
// arms read wakes the controller twice and neither case hides the other.
//
// The reason each wake carries begins with the arm's own words (record.ArchitectStallArm.Words)
// and then says what the record says, so a controller reads which of the three raised the wake
// it holds without opening anything.
func (a *Admission) wakeStalledArchitects(ctx context.Context, tx pgx.Tx, stalls []architectStall) error {
	if len(stalls) == 0 {
		return nil
	}
	told, err := a.store.TellArchitectsStalled(ctx, tx, stallMarks(stalls))
	if err != nil {
		return err
	}
	for i, stall := range stalls {
		if !told[i] {
			continue
		}
		reason := stall.arm.Words() + ": " + stall.why
		if err := a.enqueue(ctx, tx, stall.root.Key, record.ControllerNotice{Kind: record.ArchitectStalledNotice,
			Role: claim.RoleArchitect, Reason: reason}, stall.at); err != nil {
			return err
		}
		intake.OnCommit(ctx, func() {
			a.log.Warn("admission: the controller was told a root's architect has stalled",
				"issue", stall.root.Key, "arm", string(stall.arm), "phase", stall.root.Phase,
				"generation", stall.root.Generation, "reason", reason)
		})
	}
	return nil
}

// architectTookNothing is the first arm: the architect's claim holds the task of the entry it
// was given, that task was queued more than workflow.StrandedReleaseWake ago, and no turn of the
// claim has confirmed it. It answers what the record says, and "" for a root it does not read.
//
// The task is read as the claim's own handshake — the delivery carrying the entry the claim was
// given, which is the pair the state shows beside the two numbers (api.Issue.EntryQueuedAt) —
// rather than as the entry the phases row names, because a claim still holding an older entry's
// task unconfirmed took nothing either, and the sweep writes no newer start for it while that
// task stands (workflow.ArchitectStartPending).
//
// The root must also be one the sweep serves (architectServed): no root outside the reach is
// sent an architect's task at all, so none can have failed to take one, and a root that cannot
// hear is one nothing a controller does reaches, so it raises nothing, while one that could not
// hear earlier and can now raises the arm, because the task it was owed never arrived. Those
// readings are asked last, so a root whose claim holds no such task costs no read.
func (a *Admission) architectTookNothing(ctx context.Context, tx pgx.Tx, root record.Issue, run record.RoleRun,
	now time.Time,
) (string, error) {
	held := run.Claim
	if held == nil || held.Pending == nil {
		return "", nil
	}
	task := held.Pending
	if task.Entry == 0 || task.Entry != held.GivenEntry || !task.ConfirmedAt.IsZero() {
		return "", nil
	}
	if task.QueuedAt.IsZero() || now.Sub(task.QueuedAt) <= workflow.StrandedReleaseWake {
		return "", nil
	}
	served, err := a.architectServed(ctx, tx, root)
	if err != nil || !served {
		return "", err
	}
	return fmt.Sprintf("the task of phase entry %d was queued to the architect's claim of %s at %s. No turn of that claim has confirmed it. The root stands at %s. The daemon writes no second start while that task stands.",
		task.Entry, root.Key, task.QueuedAt.UTC().Format(time.RFC3339), root.Phase), nil
}

// architectClaimUnreachable is the second arm: the root is in the workflow and its architect's
// claim is failed or retired, or the root holds no architect claim at all while the work of its
// entry has stood more than workflow.StrandedReleaseWake. It answers what the record says, and
// "" for a root it does not read.
//
// A failed or a retired claim is read whatever else is queued for it, which is what makes this
// the wake for a claim workflow.Stranded names. A root with no claim is read unless a start of
// one already waited in the outbox when the pass began (workflow.ArchitectStartPending, the
// sweep's own reading), since a queued start is the daemon's own next move and the claim is
// created by running it. run is the reading the pass made before its sends, so where the pass
// itself wrote the start again (sent) the wake still stands — a root with no claim that long is
// stalled whatever this pass writes — and its reason says the start was written, rather than that
// none waits.
func architectClaimUnreachable(root record.Issue, row record.PhaseEntry, run record.RoleRun, sent bool, now time.Time) string {
	if !record.InWorkflow(root) {
		return ""
	}
	if held := run.Claim; held != nil {
		if held.State != supervise.StateFailed && held.State != supervise.StateRetired {
			return ""
		}
		return fmt.Sprintf("the architect claim of %s is %s, and the root stands at %s in the workflow.",
			root.Key, held.State, root.Phase)
	}
	if _, pending := workflow.ArchitectStartPending(run); pending {
		return ""
	}
	if row.Entry == 0 || row.RaisedAt.IsZero() || now.Sub(row.RaisedAt) <= workflow.StrandedReleaseWake {
		return ""
	}
	queue := "no start of one waits in the queue"
	if sent {
		queue = "the daemon wrote the start of its architect again in the same transaction as this wake"
	}
	return fmt.Sprintf("%s holds no architect claim at all, and %s. The work of phase entry %d has stood since %s.",
		root.Key, queue, row.Entry, row.RaisedAt.UTC().Format(time.RFC3339))
}

// architectSlowBeforeGate is the third arm: the root stands at phase.Admitted with no step of
// its design gate registered, more than workflow.ArchitectGateWake after the later of its
// admission and the moment its architect's entry was last raised. It answers what the record
// says, and "" for a root it does not read.
//
// The later of the two moments is the issue's own decision. A root a person answered after a six
// hour wait comes back to admitted holding the admission row it has held all along, so its
// admitted moment is six hours old and the bound would be past the moment it resumed. The entry
// its resume raised is what says when the work came back to it. For a first admission and for a
// re-admission the two are one moment, since the promotion that takes the slot raises the entry
// in the same transaction (promoteHolds).
//
// It reads a root standing at admitted and no other. A root a hand-over carried in stands in the
// phase that came with it, where its tree's work is already running under a phase worker, and a
// gate it has not registered holds nothing up yet.
func architectSlowBeforeGate(root record.Issue, gate record.TreeGate, admittedAt time.Time, row record.PhaseEntry, now time.Time) string {
	if root.Phase != phase.Admitted || len(gate.Rows) > 0 {
		return ""
	}
	since := admittedAt
	if row.RaisedAt.After(since) {
		since = row.RaisedAt
	}
	if since.IsZero() || now.Sub(since) <= workflow.ArchitectGateWake {
		return ""
	}
	return fmt.Sprintf("%s has stood at admitted since %s with no step of its design gate registered, which is more than %s.",
		root.Key, since.UTC().Format(time.RFC3339), workflow.ArchitectGateWake)
}

// resumeSlottedWorkers sends every role owedEntryRoles names the work of the entry its phases
// row names: the task that entry was raised with, at that entry, so no completion in flight is
// refused for an entry the worker was never given, and the phase's resume task where the record
// holds no task of that entry (workflow's SendOwedEntry). A root's own architect is sent its
// start instead, which differs from a phase worker's in the two ways a root architect's start
// differs: the row waits for no admission slot, since the root holds the one row the cap counts
// from its promotion until its work ends (record.KeepsSlot), and the task is derived again from
// the record rather than taken off the row (workflow's SendArchitectStart). Where the root's
// architect claim does not exist, that start is what creates it, exactly as admission's
// queued-root branch does.
//
// An architect whose phases row names no entry at all is handed one here, rather than resumed:
// the entry is raised with the task the derivation gives over the record as it stands, and the
// start carries that entry (workflow.RaiseArchitectEntryAndStart, over the EnterArchitect call
// admission's own promotion makes). Two roots stand in that shape and no others. One is a root
// the daemon of the release before this one admitted after this release's migration had run and
// before this daemon served: that promotion wrote no entry, so its architect holds a claim with
// no row and the fill of the migration never saw it. The other is a root whose architect claim
// exists while no transition of this generation ever raised an entry for it. Raising here is
// safe because admission raises the entry for every root it starts: an entry of 0 beside a live
// architect claim is a root no transition of this release ever handed work to, so there is no
// work of an earlier entry that this could write over. Both shapes hold an architect claim, and
// the selection weighs no root without one (owedOf): a root with neither claim nor entry was
// started by nothing, and its promotion is what hands its architect the tree. A rollback that
// takes the entry columns away re-arms the migration's fill as well as the phase entry's own
// (20261113182000_architect_entry_upgrade.up.sql), so the roots standing at a roll-forward are
// filled again rather than raised here all at once.
//
// Every other role is left as it was: a phase worker whose row names no entry is owed none
// (workflow.OwedEntry), because no transition of its issue has ever handed that role work.
//
// The difference it closes is the one the record carries on its own: the transition raises the
// entry and writes its work in its own transaction, the outbox records the entry as given when
// it queues the task, and between the two nothing else reads the two numbers. So this repairs
// the slot whose worker has no task it was written for — a start row finished without acting
// while the slot stayed, one the outbox fenced out, one finishFencedStarts ended earlier in this
// pass — and, by the same rule and without asking why, every other role whose claim never got
// the work its entry names, whatever state that claim is in.
//
// It runs after the cap loop on every pass admission makes, and on the entry sweep's own fact
// (convergeEntries). Running after the fence repair rather than before it is what repairs a slot
// whose start row the fences rejected on the pass that rejected it, instead of a pass later.
//
// After the sends it reads the three stall arms over every slotted root's architect and wakes the
// controller for each arm no mark has told yet (architectStalls, wakeStalledArchitects), so a
// root the pass could still do something for is wholly served before the controller is woken
// about it. The arms read the holders as the pass read them before its sends, and the roots
// whose architect start the pass wrote are handed to them, so the second arm's reason says what
// the pass did.
func (a *Admission) resumeSlottedWorkers(ctx context.Context, tx pgx.Tx, issues []record.Issue, slots []record.Slot,
	facts record.LineFacts,
) error {
	owed, read, err := a.owedEntryRoles(ctx, tx, issues, slots, facts)
	if err != nil {
		return err
	}
	var sent map[string]bool
	for _, role := range owed {
		raise := role.Role == claim.RoleArchitect && role.Row.Entry == 0
		switch {
		case raise:
			raised, err := a.engine.RaiseArchitectEntryAndStart(ctx, tx, role.Issue, facts.Gates[role.Issue.Tree])
			if err != nil {
				return fmt.Errorf("raise the phase entry of the architect of slotted root %s: %w", role.Issue.Key, err)
			}
			if raised == 0 {
				continue
			}
		case role.Role == claim.RoleArchitect:
			if err := a.engine.SendArchitectStart(ctx, tx, role.Issue, facts.Gates[role.Issue.Tree], role.Row.Entry); err != nil {
				return fmt.Errorf("start the architect of slotted root %s again: %w", role.Issue.Key, err)
			}
		default:
			if err := a.engine.SendOwedEntry(ctx, tx, role.Issue, role.Role, role.Row); err != nil {
				return fmt.Errorf("start the worker of slotted issue %s again: %w", role.Issue.Key, err)
			}
		}
		if role.Role == claim.RoleArchitect {
			if sent == nil {
				sent = map[string]bool{}
			}
			sent[role.Issue.Key] = true
		}
		intake.OnCommit(ctx, func() {
			if raise {
				a.log.Warn("admission: a root architect's phases row named no entry, so the pass raised one and sent its work",
					"issue", role.Issue.Key, "role", role.Role, "phase", role.Issue.Phase, "given", role.Given)
				return
			}
			a.log.Warn("admission: a role was behind the entry its phases row names, so that entry's work is sent again",
				"issue", role.Issue.Key, "role", role.Role, "phase", role.Issue.Phase, "entry", role.Row.Entry, "given", role.Given)
		})
	}
	stalls, err := a.architectStalls(ctx, tx, read, facts, sent)
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
	issues, slots, facts, err := a.reread(ctx, tx)
	if err != nil {
		return err
	}
	return a.resumeSlottedWorkers(ctx, tx, issues, slots, facts)
}

// finishFencedStarts is the repair of a held start row that has outlived the fences the outbox
// would have run it under: its issue has left the workflow, its tree lingers or has left, or the
// issue has moved past the phase or the generation the row names. The outbox applies those
// fences when it runs a row, but a row waiting for a slot may never be run at all, and one that
// could never act would keep its issue in the waiting line for ever and take a slot from work
// that could use it. Finishing the row deletes it, exactly as the outbox's own fence would.
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
		intake.OnCommit(ctx, func() {
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
		intake.OnCommit(ctx, func() {
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

// reenterStrandedChildren re-enters, at candidate's promotion, every tree member admission left
// recorded todo with its previous run's phase at done: a reopen admission declined to re-enter for
// want of the label while the tree was not live, which recordObservation otherwise records with the
// phase untouched. Candidate's own promotion is what makes the tree live for these, since nothing
// else reaches them afterward — Engine.ReenterChild only re-enters a live tree's reopened child on
// its own live event, and a later todo observation of one already todo changes nothing. The
// synthetic fact carries the child's own already-recorded fields: no live event reopened it, so
// there is nothing newer to apply, only its own next generation to start.
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
