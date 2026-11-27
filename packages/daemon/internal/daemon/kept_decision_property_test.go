package daemon

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The property test's bounds. A run is one root's life under one seed; a step is one operation the
// seed's generator chose. The defaults are 30 runs of 100 steps, 3000 steps in all, which the
// isolated database of each run (isolatedOutboxPool) dominates.
const (
	keptPropertyRuns  = 30
	keptPropertySteps = 100
	// keptPropertyTest is the one test whose seeds LEGION_KEPT_PROPERTY_SEED and
	// LEGION_KEPT_PROPERTY_RUNS set (keptPropertySeeds); every other driver hard-codes its own,
	// which is what a failure's replay command has to say (keptRun.replay).
	keptPropertyTest = "TestKeptDecisionsReachTheCurrentArchitectOnce"
	// keptQuiesceBound caps the quiescence loop: a root whose rows and deliveries do not drain
	// with every failure switched off is itself a failure of the invariant, reported with its
	// trace rather than left to the package deadline.
	keptQuiesceBound = 400
	// keptRoot is the root every run opens with.
	keptRoot = "LEGION-1"
	// keptQueued is a second root of the project, waiting in `todo` for an admission slot
	// (record.WaitsForSlot) behind an issue Dispatch has not finished, which is where a person's
	// decision on it is kept for the start its admission will queue.
	keptQueued = "LEGION-4"
	// keptBlocker is the Dispatch issue the mirror shows the queued root waiting on: it holds that
	// root out of the line, and the quiescence finishes it, as a person finishing that issue does
	// (blockers.Unfinished).
	keptBlocker = "LEGION-9"
	// keptTogether is the most rows the concurrent step runs at once, each on a lane of its own.
	keptTogether = 3
)

// keptTelling is one time an architect was told a kept decision: the generation the telling names
// (a catch-up's; a start task names none), whether the root had an architect to hear it when the
// architect received it, the generation that root stood at then, whose architect heard it, the
// tree the decision's own issue stood in then, and what carried it.
type keptTelling struct {
	ref string
	// prompt is whether a start task's prompt carried it. A start task names no generation
	// (workflow.ParkedDecisionTask writes none) and the claim it reaches is the root's one
	// architect session, which holds its task across the root's new runs, so what it is held
	// to is that the root has an architect to hear it (hears) rather than the generation named.
	prompt bool
	hears  bool
	// retold is whether this telling is one the machine's own take-back made a second time: a
	// prompt under an id minted when a task was taken back (keptRun.resent), which carries the task
	// of the row it was queued by again. It is the one extra telling to an architect the
	// invariant accepts (held).
	retold bool
	// afterFailedWrite is whether this telling repeats, to the same architect, a catch-up whose own
	// telling write failed (keptForgetStore.tellingFailed): the publish reached the architect and
	// the forget did not commit, so the decision stood kept and a later catch-up named it again.
	// The architect ruled that second telling acceptable
	// (https://github.com/sjawhar/legion/pull/2163#issuecomment-6542200410): a failed write may tell
	// a decision twice, never lose it. tell sets it, and held counts it as it counts a retold task.
	afterFailedWrite bool
	// named is the generation the telling itself names, and root the generation its root stood at
	// when the daemon decided to send it — read where the catch-up's publish began, not once it
	// returned (observe).
	named uint64
	root  uint64
	// row is the outbox row that carried the telling: the start row a prompt's delivery id names,
	// or the row a published notice's dedupe key names (its original, for a copy). It is what says
	// whether an earlier telling's own write failed (tell, afterFailedWrite).
	row int64
	// tree is the root whose architect heard it, and issueTree the tree the record names for the
	// decision's issue then, which a failure's report names beside it.
	tree      string
	issueTree string
	how       string
	step      int
}

// keptRef is one decision the run tracks: the Dispatch reference a person decided on, and the
// issue of that ask, which is the root itself or the queued root.
type keptRef struct {
	ref   string
	issue string
}

// keptPublishMode is what the listener does with a role publish this run.
type keptPublishMode int

const (
	// keptDelivers: every publish reaches its topic's holder.
	keptDelivers keptPublishMode = iota
	// keptNoHolder: no live session holds the role (notify.ErrNoHolder), so the row waits while
	// the architect's claim can hold its role again, and finishes undelivered once nobody will
	// hold it (architectEnded).
	keptNoHolder
	// keptRefuses: the holder does not advertise the delivery mode, so the row finishes
	// undelivered (notify.ErrRefusedByHolder) and releases what it claimed.
	keptRefuses
	// keptReceiptTimeout: the holder did not confirm in time, so the daemon re-holds the notice
	// (notify.ErrNotDelivered) and the copy takes the original's claims over (MoveKeptClaims).
	// Under the run's own mode only a notice nobody has re-held yet is answered this way, so the
	// re-hold budget (noticeReholdDelays) is never spent and the copy is never dropped for
	// exhausting it; the interleaving step answers the one publish it holds open whatever its
	// resends, which is how a catch-up the run superseded during its publish reaches the
	// re-hold's superseded branch (raceRow).
	keptReceiptTimeout
	// keptDenied: the listener's own NATS grant refuses the forward (notify.ErrPublishDenied),
	// which no retry can change until an operator changes the grant, so the row finishes
	// undelivered as a refused one does.
	keptDenied
	// keptDeliveryFailed: the listener could not forward the notice at all — the hub down, the
	// role or session unreadable (notify.ReasonDeliveryFailed) — which is none of the answers the
	// executor classifies, so the row fails without finishing: its lane retries it on its backoff,
	// and the run settles it the same way, due again at once (retryRow).
	keptDeliveryFailed
	// keptModes is how many modes the generator chooses among.
	keptModes
)

func (m keptPublishMode) String() string {
	switch m {
	case keptNoHolder:
		return "no-holder"
	case keptRefuses:
		return "refused"
	case keptReceiptTimeout:
		return "receipt-timeout"
	case keptDenied:
		return "denied"
	case keptDeliveryFailed:
		return "delivery-failed"
	default:
		return "delivers"
	}
}

// keptRace is one armed interleaving: the operation that runs, on a goroutine of its own and to
// completion, while one catch-up's publish is held open, and the answer that publish is then
// given. The publisher holds no lock of its own while it runs, so the operation meets the daemon
// exactly where a lane would: the publish is outside every transaction of the row's run.
type keptRace struct {
	what string
	mode keptPublishMode
	run  func() error
	// ran says the publish the race was armed for happened, err what the operation failed with,
	// and outcome what it made of itself, each written by the race's own goroutine before the
	// publish is answered and read by the step that armed it afterwards.
	ran     bool
	err     error
	outcome string
	// unrun undoes what preparing the operation took when the publish never came, so the race ran
	// nothing: the row op's lease, which no run then holds.
	unrun func() error
}

// keptPublisher is the listener as a role publish meets it, under one mode the run switches. It
// counts the answer each catch-up drew, so a run reports which modes its catch-ups actually met,
// holds one publish open for the interleaving a step armed (keptRace), and records, for each
// catch-up it takes, the generation that catch-up's own root stood at then (atPublish). A kept
// publish (PublishKept) is answered by the run's mode as any
// other is and never kept: the mode's no-holder is the 404 a listener before #4079 answers a
// claim whose holder is gone.
type keptPublisher struct {
	mu        sync.Mutex
	mode      keptPublishMode
	delivered []outboxPublish
	answered  [keptModes]int

	raceMu sync.Mutex
	race   *keptRace

	// atPublish is the generation of the root whose architect holds a topic, false for a topic no
	// driven architect holds. genMu guards what it answered:
	// genAt, by the notice's dedupe key, and genErr, the read that failed, which ends the run.
	atPublish func(topic string) (uint64, bool, error)
	genMu     sync.Mutex
	genAt     map[string]uint64
	genErr    error
}

func (p *keptPublisher) Publish(_ context.Context, topic, message string, payload any, key string) (string, error) {
	return "", p.publish(topic, message, payload, key, false)
}

func (p *keptPublisher) PublishKept(_ context.Context, topic, message string, payload any, key string) (bool, error) {
	return false, p.publish(topic, message, payload, key, true)
}

func (p *keptPublisher) publish(topic, message string, payload any, key string, keep bool) error {
	notice, isNotice := payload.(record.Notice)
	catchUp := isNotice && notice.CatchUp != nil
	var raced *keptRace
	if catchUp {
		// The generation is read before anything else of this publish: it is the one the daemon
		// decided to send this catch-up at, where its lease-time check read the catch-up's own
		// issue (catchUpSuperseded), and a new run landing inside the publish is followed by
		// its own catch-up rather than making this telling wrong.
		p.noteGeneration(topic, key)
		raced = p.disarm()
	}
	if raced != nil {
		// The listener has the notice and has answered nothing. One other operation runs to
		// completion on a goroutine of its own, and only then is the publish answered.
		done := make(chan struct{})
		go func() {
			defer close(done)
			raced.err = raced.run()
		}()
		<-done
		raced.ran = true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	mode, err := p.answer(topic, payload, key, raced)
	if catchUp {
		p.answered[mode]++
	}
	if err != nil {
		return err
	}
	p.delivered = append(p.delivered, outboxPublish{topic: topic, message: message, key: key, payload: payload, keep: keep})
	return nil
}

// noteGeneration records the generation of the issue whose architect holds the publish's topic, as
// it stands now, under the notice's dedupe key, keeping the first reading of a key: one key is one
// telling, and that telling is the first delivery under it.
func (p *keptPublisher) noteGeneration(topic, key string) {
	if p.atPublish == nil {
		return
	}
	generation, known, err := p.atPublish(topic)
	p.genMu.Lock()
	defer p.genMu.Unlock()
	if err != nil {
		p.genErr = errors.Join(p.genErr, err)
		return
	}
	if _, seen := p.genAt[key]; known && !seen {
		p.genAt[key] = generation
	}
}

// generationAtPublish is the generation the catch-up's own issue stood at when the listener took
// the catch-up published under key, and whatever read of one failed, which ends the run.
func (p *keptPublisher) generationAtPublish(key string) (uint64, bool, error) {
	p.genMu.Lock()
	defer p.genMu.Unlock()
	generation, known := p.genAt[key]
	return generation, known, p.genErr
}

// answer is what this run's mode makes of one publish, and the mode that answer is of: a copy the
// re-hold already queued is delivered rather than timed out again, so the re-hold budget is never
// spent. A publish a race held open is answered as the step that armed it chose, whatever the
// notice's resends: that answer is one the generator picked, not the run's standing mode.
func (p *keptPublisher) answer(topic string, payload any, key string, raced *keptRace) (keptPublishMode, error) {
	mode := p.mode
	if raced != nil {
		mode = raced.mode
	}
	switch mode {
	case keptNoHolder:
		return mode, fmt.Errorf("publish notice to %s: listener returned 404 Not Found: %w", topic, notify.ErrNoHolder)
	case keptRefuses:
		return mode, fmt.Errorf("publish notice to %s: listener returned 403 Forbidden: %w", topic,
			notify.ErrRefusedByHolder{Holder: "ses-other"})
	case keptDenied:
		return mode, fmt.Errorf("publish notice to %s: listener returned 403 Forbidden: %w", topic,
			notify.ErrPublishDenied{Message: fmt.Sprintf(
				"envoy: the message bus denied the publish under this listener's own permissions: bus: NATS denied the publish to %q", topic)})
	case keptDeliveryFailed:
		return mode, fmt.Errorf("publish notice to %s: listener returned 504 Gateway Timeout: %w", topic,
			notify.ErrNotDelivered{Reason: notify.ReasonDeliveryFailed, EventID: key})
	case keptReceiptTimeout:
		if notice, ok := payload.(record.Notice); ok && (raced != nil || notice.Resends == 0) {
			return mode, fmt.Errorf("publish notice to %s: listener returned 504 Gateway Timeout: %w", topic,
				notify.ErrNotDelivered{Reason: notify.ReasonReceiptTimeout, Holder: "ses-other"})
		}
	}
	return keptDelivers, nil
}

func (p *keptPublisher) setMode(mode keptPublishMode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = mode
}

// arm holds the next catch-up publish open for race, and disarm takes the armed race off, so one
// arming is one interleaving: a step whose publish never came takes its own race back unrun.
func (p *keptPublisher) arm(race *keptRace) {
	p.raceMu.Lock()
	defer p.raceMu.Unlock()
	p.race = race
}

func (p *keptPublisher) disarm() *keptRace {
	p.raceMu.Lock()
	defer p.raceMu.Unlock()
	race := p.race
	p.race = nil
	return race
}

// answers is how many catch-up publishes each mode has answered, which a step reads before and
// after a row's run to see what that run's publish met.
func (p *keptPublisher) answers() [keptModes]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.answered
}

func (p *keptPublisher) published() []outboxPublish {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]outboxPublish(nil), p.delivered...)
}

// keptForgetStore is the record store with ForgetKeptDecisionsOf failing while the run's toggle is
// on, as a database error — or the daemon dying — between a start's delivery, or a catch-up's
// publish, and the forget's commit leaves it. The toggle is read from the lanes the concurrent
// step runs, so it is held as one word rather than as a field.
type keptForgetStore struct {
	record.Store
	fails atomic.Bool
	// failedTellings is every catch-up telling, by the outbox row its publish went out under (the
	// row a re-held copy copies), whose forget failed: the telling's own write did not commit, so
	// the decision it told stands kept and a later catch-up may name it again (held, tell).
	failedTellings sync.Map
}

func (s *keptForgetStore) ForgetKeptDecisionsOf(ctx context.Context, tx pgx.Tx, root string, members []string, kept record.KeptDecisions, row int64) error {
	if s.fails.Load() {
		telling := row
		if err := tx.QueryRow(ctx, `select coalesce((payload->>'resend_of')::bigint, id) from outbox where id = $1`, row).Scan(&telling); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		s.failedTellings.Store(telling, true)
		return errors.New("tester: the forget's transaction did not commit")
	}
	return s.Store.ForgetKeptDecisionsOf(ctx, tx, root, members, kept, row)
}

// tellingFailed is whether the catch-up telling published under row had its forget fail.
func (s *keptForgetStore) tellingFailed(row int64) bool {
	_, failed := s.failedTellings.Load(row)
	return failed
}

// keptClaimStore is the claim store this run's supervisor writes through: the daemon's own
// (store.Store), over the run's database, so every claim and the task pending on it stand in
// `claims` and `pending_task_deliveries` as a live daemon leaves them. The held task's decisions
// are the reason it is the real one: what such a task names of what its root keeps is persisted
// with the delivery (pending_task_deliveries.kept_decisions), and the claim store holds and tells
// the rows it names with that same write (store's holdKeptDecisions, tellKeptDecisions), and a
// restart reads the claims back from there (keptRun.storedClaims).
//
// Two answers are not the daemon's: a claim is admitted by writing it, and every launch passes.
// Both are the tree lifecycle's, which this run opens for the root alone — the queued root's
// promotion takes an architect claim of its own, under a lifecycle no step here confirms.
type keptClaimStore struct{ *store.Store }

func (s keptClaimStore) AdmitClaim(ctx context.Context, c supervise.Claim) (supervise.Claim, error) {
	return c, s.PutClaim(ctx, c)
}

func (keptClaimStore) CheckLaunch(context.Context, supervise.Claim) error { return nil }

// keptRuntime is the fake runtime with every launch of the architect refused while the run's
// toggle is on — the pod the cluster would not open, the session it could not resume — so a start
// spends the claim's launch budget (supervise.Limits.LaunchFailures) and leaves the claim failed.
type keptRuntime struct {
	*fake.Runtime
	mu       sync.Mutex
	refuses  bool
	refusals int
}

var _ runtime.Runtime = (*keptRuntime)(nil)

func (r *keptRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if err := r.refusal(); err != nil {
		return runtime.Locator{}, err
	}
	return r.Runtime.Spawn(ctx, spec)
}

func (r *keptRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if err := r.refusal(); err != nil {
		return runtime.Locator{}, err
	}
	return r.Runtime.Resume(ctx, prev, spec)
}

// refusal is the answer a launch draws while the toggle is on, which supervise charges to the
// claim's launch budget.
func (r *keptRuntime) refusal() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.refuses {
		return nil
	}
	r.refusals++
	return errors.New("tester: no process could be opened for the claim")
}

func (r *keptRuntime) setRefuses(refuses bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refuses = refuses
}

func (r *keptRuntime) refusing() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refuses
}

func (r *keptRuntime) refused() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refusals
}

// keptConn is an architect's connection with one prompt held open on demand: the send is in
// flight, the daemon has had no answer, and the step that armed the gate runs what it likes
// before that answer comes (keptGate). Every other prompt goes straight to the connection
// underneath, which records it as the run reads it (fake.Conn.Prompts).
//
// A prompt the agent refuses busy is recorded nowhere: Oh My Pi answers that way exactly when a
// turn of its own is already running, so the frame reached no agent and told it nothing
// (supervise's agentBusy, and its own "the agent never read this task"). The run reads tellings
// from the frames the connection kept, so a refused one must not stand among them.
type keptConn struct {
	*fake.Conn
	mu   sync.Mutex
	gate *keptGate
}

var _ runtime.Conn = (*keptConn)(nil)

// keptGate is one prompt held open: arrived is closed by the prompt that met the gate, release by
// the step that armed it, and refusal is the answer that prompt is then given — Oh My Pi's busy
// answer, or "" to let the frame through.
type keptGate struct {
	arrived chan struct{}
	release chan struct{}
	refusal string
}

// keptBusyRefusal is Oh My Pi's own answer to a prompt arriving while a turn runs, worded as it
// words it: the daemon reads it as the agent being busy rather than refusing the work, and charges
// it nothing (supervise's agentBusy).
const keptBusyRefusal = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."

// keptSendBound is how long a step that armed the gate waits for the prompt. A send starts inside
// the event that triggers it, on a goroutine of the machine's own, so the wait is a safety net
// rather than a step's pace: the step that arms the gate first reads whether the send has anything
// to carry (heldForItsTree), and takes its gate back when none came.
const keptSendBound = 5 * time.Second

func newKeptConn() *keptConn { return &keptConn{Conn: fake.NewConn()} }

// hold arms the gate for the next prompt, which is held open until the step releases it.
func (c *keptConn) hold(refusal string) *keptGate {
	gate := &keptGate{arrived: make(chan struct{}), release: make(chan struct{}), refusal: refusal}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gate = gate
	return gate
}

// standDown takes an armed gate back and says whether no prompt had met it: a send the daemon
// decided against leaves the gate armed, and nothing later must be caught by it.
func (c *keptConn) standDown(gate *keptGate) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gate != gate {
		return false
	}
	c.gate = nil
	return true
}

func (c *keptConn) Prompt(ctx context.Context, deliveryID, message string) error {
	c.mu.Lock()
	gate := c.gate
	c.gate = nil
	c.mu.Unlock()
	if gate == nil {
		return c.Conn.Prompt(ctx, deliveryID, message)
	}
	close(gate.arrived)
	select {
	case <-gate.release:
	case <-ctx.Done():
		// The send's own deadline (supervise.Timeouts.RPC) ran out while the gate held it, which
		// the daemon reads as the transport losing the frame.
		return ctx.Err()
	}
	if gate.refusal != "" {
		return keptRefusal{reason: gate.refusal}
	}
	return c.Conn.Prompt(ctx, deliveryID, message)
}

// keptRefusal is the agent answering a prompt `success: false`, as the runtime's own double writes
// one (fake.Conn.RefusePrompt): an error that is runtime.ErrPromptRefused and carries the reason
// the daemon reads to tell a busy agent from one that will not take the work.
type keptRefusal struct{ reason string }

func (r keptRefusal) Error() string         { return r.reason + ": " + runtime.ErrPromptRefused.Error() }
func (r keptRefusal) Is(target error) bool  { return target == runtime.ErrPromptRefused }
func (r keptRefusal) RefusalReason() string { return r.reason }

// keptCounts is what a run's injected failures drove, reported at the run's end so the operations
// the generator chose are shown to have fired rather than assumed.
type keptCounts struct {
	// refusedLaunches is every Spawn or Resume the runtime refused.
	refusedLaunches int
	// failedClaim is every time an architect's claim spent its launch budget and failed.
	failedClaim int
	// retriedFailed is every start row that found the claim failed and relaunched it (the
	// outbox's start, case StateFailed).
	retriedFailed int
	// endedCatchUps is every catch-up the executor finished undelivered because nobody will hold
	// the architect's role again (architectEnded), and endedByFailure those of them whose reason
	// was the claim itself — failed, retired or unsupervised.
	endedCatchUps  int
	endedByFailure int
	// raced is every catch-up publish held open for an interleaving, together every step that ran
	// several rows at once, streamed every restart whose agent then answered get_state in a turn,
	// and foreign every turn the agent started on its own on a claim holding a task nobody sent.
	raced    int
	together int
	streamed int
	foreign  int
	// supersededTimeouts is every catch-up whose receipt timeout drew no copy from the re-hold:
	// the operation that ran inside its publish superseded it, or the copy was the last the
	// re-hold queues. Either way what the row claims is handed to a fresh catch-up
	// (notice_exceptions.go's reholdNotice, supersededKept and spentKept).
	supersededTimeouts int
	// refusedConfirmations is every send a turn of the agent's own confirmed while it was in
	// flight and the agent then answered busy, so the machine took that confirmation back
	// (supervise's refused); lateRefusals every replayed refusal that took back a confirmation a
	// foreign turn had made (takesBackConfirmed); and interruptedTasks every task taken back
	// because the process running its turn died (interrupted). Each of the three is answered by
	// sending the task again.
	refusedConfirmations int
	lateRefusals         int
	interruptedTasks     int
	// publishes is how many catch-up publishes each listener mode answered.
	publishes [keptModes]int
}

func (c *keptCounts) add(other keptCounts) {
	c.refusedLaunches += other.refusedLaunches
	c.failedClaim += other.failedClaim
	c.retriedFailed += other.retriedFailed
	c.endedCatchUps += other.endedCatchUps
	c.endedByFailure += other.endedByFailure
	c.raced += other.raced
	c.together += other.together
	c.streamed += other.streamed
	c.foreign += other.foreign
	c.supersededTimeouts += other.supersededTimeouts
	c.refusedConfirmations += other.refusedConfirmations
	c.lateRefusals += other.lateRefusals
	c.interruptedTasks += other.interruptedTasks
	for mode := range c.publishes {
		c.publishes[mode] += other.publishes[mode]
	}
}

func (c keptCounts) String() string {
	return fmt.Sprintf("%d launches refused, %d claim failures, %d starts relaunched a failed claim, "+
		"%d catch-ups finished for an architect that will not hold its role again (%d of them a failed claim); "+
		"%d publishes held open for an interleaving, %d steps ran rows at once, "+
		"%d restarts met a streaming agent, %d foreign turns, %d receipt timeouts the re-hold queued no copy for; "+
		"%d sends refused busy after a turn of the agent's own confirmed them, %d replayed refusals took a confirmation back, "+
		"%d processes died in a task's turn; "+
		"catch-up publishes answered: %d delivered, %d no-holder, %d refused, %d receipt-timeout, %d denied, %d delivery-failed",
		c.refusedLaunches, c.failedClaim, c.retriedFailed, c.endedCatchUps, c.endedByFailure,
		c.raced, c.together, c.streamed, c.foreign, c.supersededTimeouts,
		c.refusedConfirmations, c.lateRefusals, c.interruptedTasks,
		c.publishes[keptDelivers], c.publishes[keptNoHolder], c.publishes[keptRefuses],
		c.publishes[keptReceiptTimeout], c.publishes[keptDenied], c.publishes[keptDeliveryFailed])
}

// keptTree is one root whose architect the run drives: the root every run opens with, and the
// second root that waits for an admission slot. Each has its claim, its agent's connection and its
// role topic, and a decision on a root is owed to that root's architect.
type keptTree struct {
	key   string
	issue record.Issue
	token claim.Token
	conn  *keptConn
	topic string
	// prompts is how many of the connection's prompts observe has read.
	prompts int
}

// keptRun is one seeded life of one root: the real record store, workflow engine, admission,
// outbox runner and claim supervisor over a database of its own, the model the generator drives
// them from, and what each architect was told.
type keptRun struct {
	t *testing.T
	// ctx is the run's own calls' context — its facts, reads, writes and row runs — with a hold
	// mark of its own (store.ForConcurrentWorker), so the pool refuses one of them a second
	// connection while another holds one (store.ErrNestedAcquire). It is the mark of one
	// sequential flow: a goroutine the run starts beside it takes worker() instead. On an unmarked
	// context the pool refuses nothing, and a run built on one passes whatever its calls do
	// (TestTheKeptRunsGuardRefusesASecondConnectionOfItsOwnContext).
	ctx   context.Context
	seed  int64
	rng   *rand.Rand
	steps int
	step  int
	trace []string

	pool      *store.Pool
	records   *keptForgetStore
	engine    *workflow.Engine
	admission *admit.Admission
	trees     *store.Store
	claims    keptClaimStore
	rt        *keptRuntime
	publisher *keptPublisher
	sup       *supervisor
	runner    *outbox
	stateDir  string

	// root is the root the run opens with, queued the second root waiting for its slot, and roots
	// both, every root whose architect the run drives.
	root   *keptTree
	queued *keptTree
	roots  []*keptTree

	decided  int
	at       time.Time
	tracked  []keptRef
	told     map[string][]keptTelling
	rowGen   map[int64]uint64
	refIssue map[string]string
	// sent is every prompt delivery id an architect has been sent, keys every dedupe key a notice
	// was delivered under — a repeat of either is the same telling, not a second one — and applied
	// every terminal fact key the run has applied, which is what says a launch's ready has been
	// told to the workflow already.
	sent    map[string]bool
	keys    map[string]bool
	applied map[string]bool
	// resent is the outbox row the task of each delivery the machine minted for itself came from,
	// by the id it minted: a take-back gives the task a new id the shim has no record of
	// (supervise's takeBackPending, interrupted), so a prompt under one of these is the task of a
	// row long finished, told again to the architect that was prompted with it before.
	resent map[string]int64
	// refused is every driven tree whose catch-up a holder's refusal or the listener's grant
	// finished undelivered while its root heard, which quiesce ends with that architect's relaunch
	// (relaunchAfterRefusal).
	refused map[string]bool
	// owedTree is the root each decision is owed to, by reference: the root of the issue a person
	// made it on. A change of parent in Dispatch moves no issue of this run, so only that root's
	// architect pays it (held).
	owedTree map[string]string
	publishd int
	counts   keptCounts
	// crashed is the lease each crash the run simulated left on a row, by row (noteCrash): the one
	// lease heldNoStaleLease lets stand at the end of a step while no run holds it.
	crashed map[int64]keptCrashLease
}

// A person's decision on a root nobody hears reaches that root's architect exactly once, whatever
// order the machinery runs in and whatever runs beside it. The test drives random interleavings of
// the real parts — record.RelayParkedDecision for the keeping, workflow.Engine.ParkedDecisionTask
// and EnqueueNaming for the naming, the outbox runner for the delivery and the forget, the
// supervisor's machines for the architects, admit.Admission for the queued root's promotion, and
// intake.ApplyFact for the ready — over a real PostgreSQL, and holds each run to the invariant
// after every step:
//
//   - no decision is told twice (a start task prompt or a published catch-up naming it again),
//     save for a task the machine itself took back and sent again under a new id, which its
//     architect may hear once more and no oftener (tell, held);
//   - every catch-up telling names the generation its root stood at when the daemon decided to
//     send it — read where its publish began, not once the publish returned, since the publish is
//     network I/O outside every transaction and a new run landing inside it is followed by that
//     generation's own catch-up (observe) — and every start-task telling reaches a root that has
//     an architect to hear it: a start task names no generation, and it is held on its claim,
//     unsent, while its root is out of Legion's work or waiting for its slot (supervise's
//     Deps.RootHears);
//   - once the run quiesces — every injected failure off, every row drained, what held the queued
//     root out of the line finished, each architect ready and its turns ended, and each architect
//     whose catch-up a refusal or a denial finished undelivered while its root heard relaunched
//     once, which is what ends such a refusal in life (relaunchAfterRefusal) — every decision kept
//     has been told once to the architect it was owed to, and nothing is left kept anywhere.
//
// Owed to whom follows the decision's issue: a decision is owed to the architect of the root it
// was made on. No issue of this run moves (R15 of dispatch://INFRA-3320: no tree is stored, and a
// change of parent moves no debt), so a telling to the other root's architect is a fault, reported
// with the debt it left unpaid.
//
// Each seed's operations are its own: the generator is the only source of choices, the machines'
// timers are armed and never fire (stillClock, whose Now is the wall clock), and every wait is
// for a goroutine the step started. A run whose steps all ran one at a time replays exactly from
// its seed; the two steps that run rows beside each other — raceRow, which holds one catch-up's
// publish open while one other operation runs, and runTogether, which runs several due rows at
// once on lanes of their own — are ordered by PostgreSQL and the Go scheduler as much as by the
// seed, so a failure either reaches is reported with its seed and its whole trace and may need
// more than one replay to come back.
// LEGION_KEPT_PROPERTY_SEED runs one seed, LEGION_KEPT_PROPERTY_RUNS how many seeds and
// LEGION_KEPT_PROPERTY_STEPS how many operations each drives.
//
// The operations a step chooses among are a person's decision on the root or on the queued root,
// the root's close or park out of Legion's work with the architect's suspend, its hand back to
// todo with no slot, its new run with the start admission writes, one due supervise or notice row
// of a driven root run in no particular order (finished, or its finish lost to a crash), several
// due rows run at once, one catch-up run with its publish held open for another operation, an
// architect's agent coming up on its launch, the ready fact the daemon's terminal hook then tells
// the workflow, its turn on the task it holds, a turn the agent started on its own while a task
// nobody sent waits on the claim, the send a turn of the agent's own confirmed and the agent then
// answered busy, which takes that confirmation back (busyRefusalAfterAConfirmation), the shim's
// replayed refusal that takes back a confirmation a foreign turn made
// (lateRefusalAfterAConfirmation), the death of the process running a task's turn, which takes the
// task back for the relaunch (deathInTheTasksTurn), the daemon's restart, the restart whose agent
// then answers get_state in a turn, the forget's failure switched on and off, the runtime's
// refusal of every launch switched on and off, and the listener switched between delivering,
// holding no session, refusing the delivery mode, not confirming in time (the re-hold), denying
// the forward under its own NATS grant and failing to forward at all.
//
// The refused launch is what drives a claim through its launch budget
// (supervise.Limits.LaunchFailures) into failed, which the run tells the workflow as the daemon's
// terminal hook does (claimFailed), and from there through the start row's relaunch of a failed
// claim and the catch-up the executor finishes because nobody will hold the architect's role again
// (architectEnded). The denied forward finishes a catch-up undelivered as a refusal does; the
// failed forward is none of the answers the executor classifies, so its row fails without
// finishing and is retried: on its backoff in a lane, at once here (retryRow). Each run reports
// how often each of these fired (keptCounts).
//
// Three the machinery has are left out. A newer decision on a reference already kept replaces its
// row (Store.AddParkedDecision), so that reference is told twice by design and "told once per
// reference" stops meaning anything; every decision here is made on a reference of its own. The
// outbox's own parkDecision, which keeps the decision a spec-decision notice could not deliver,
// reaches the same record.RelayParkedDecision this test calls directly, and adds only the notice
// routing its own tests cover. The notice the executor cannot route (errNoticeUnroutable) needs a
// record with no such issue, no tree or no recorded root, which the rest of the run reads as a
// broken record rather than as a state the root can be in.
func TestKeptDecisionsReachTheCurrentArchitectOnce(t *testing.T) {
	t.Parallel()
	steps := keptPropertyEnv(t, "LEGION_KEPT_PROPERTY_STEPS", keptPropertySteps)
	var mu sync.Mutex
	var all keptCounts
	var seeds, failedSeeds int
	var modeSeeds [keptModes]int
	// Each seed's run has a database, runtime and state directory of its own (newKeptRun), so the
	// seeds run in parallel, with each other and with the other kept-decision seed tests; the group
	// returns once every seed has, before the totals below.
	t.Run("seeds", func(t *testing.T) {
		for _, seed := range keptPropertySeeds(t) {
			t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
				t.Parallel()
				run := newKeptRun(t, seed, steps)
				run.drive()
				run.quiesce()
				run.held()
				run.report()
				mu.Lock()
				defer mu.Unlock()
				all.add(run.counts)
				seeds++
				if run.counts.failedClaim > 0 {
					failedSeeds++
				}
				for mode, count := range run.counts.publishes {
					if count > 0 {
						modeSeeds[mode]++
					}
				}
			})
		}
	})
	t.Logf("%d seeds of %d steps: %s", seeds, steps, all)
	t.Logf("%d of %d seeds reached a failed architect claim; seeds whose catch-ups met each listener mode: "+
		"%d delivered, %d no-holder, %d refused, %d receipt-timeout, %d denied, %d delivery-failed",
		failedSeeds, seeds, modeSeeds[keptDelivers], modeSeeds[keptNoHolder], modeSeeds[keptRefuses],
		modeSeeds[keptReceiptTimeout], modeSeeds[keptDenied], modeSeeds[keptDeliveryFailed])
}

// keptPropertySeeds is the seeds this invocation runs: the one LEGION_KEPT_PROPERTY_SEED names, or
// 1..runs, which are the same seeds at every invocation so a failure found by the default run is
// replayed by naming its seed.
func keptPropertySeeds(t *testing.T) []int64 {
	t.Helper()
	if named := os.Getenv("LEGION_KEPT_PROPERTY_SEED"); named != "" {
		seed, err := strconv.ParseInt(named, 10, 64)
		if err != nil {
			t.Fatalf("LEGION_KEPT_PROPERTY_SEED=%q is no seed: %v", named, err)
		}
		return []int64{seed}
	}
	runs := keptPropertyEnv(t, "LEGION_KEPT_PROPERTY_RUNS", keptPropertyRuns)
	seeds := make([]int64, 0, runs)
	for seed := 1; seed <= runs; seed++ {
		seeds = append(seeds, int64(seed))
	}
	return seeds
}

// keptPropertyEnv is the positive count the environment variable name sets, or fallback where it
// is unset; a value that is no positive count fails the test. The news model test reads its
// counts through it too (TestTheNewsRecordHoldsToWhatTheHolderWasReallyTold).
func keptPropertyEnv(t *testing.T, name string, fallback int) int {
	t.Helper()
	set := os.Getenv(name)
	if set == "" {
		return fallback
	}
	value, err := strconv.Atoi(set)
	if err != nil || value <= 0 {
		t.Fatalf("%s=%q is no count: %v", name, set, err)
	}
	return value
}

func newKeptRun(t *testing.T, seed int64, steps int) *keptRun {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	base := record.NewStore()
	records := &keptForgetStore{Store: base}
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()},
		quietLogger())
	trees := outboxTreeStore(t, pool, keptRoot)
	run := &keptRun{
		t: t, ctx: store.ForConcurrentWorker(ctx), seed: seed, rng: rand.New(rand.NewPCG(uint64(seed), uint64(seed))), steps: steps,
		pool: pool, records: records,
		engine: engine,
		// Admission runs over the project key the records carry, which is what its own reads of the
		// project's live issues match (record.Store.LiveIssues); the claims it opens are keyed by
		// the normalized token of it (claim.ProjectToken), the one the supervisor uses. Its cap
		// holds both roots at once: the root with its own slot, and the second root, once nothing
		// holds it out of the line.
		admission: admit.New(records, engine, 5, "LEGION", quietLogger()),
		trees:     trees,
		claims:    keptClaimStore{Store: trees},
		rt:        &keptRuntime{Runtime: fake.NewRuntime()},
		publisher: &keptPublisher{genAt: map[string]uint64{}},
		stateDir:  t.TempDir(),
		at:        time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC),
		told:      map[string][]keptTelling{},
		owedTree:  map[string]string{},
		rowGen:    map[int64]uint64{},
		refIssue:  map[string]string{},
		sent:      map[string]bool{},
		keys:      map[string]bool{},
		applied:   map[string]bool{},
		resent:    map[string]int64{},
	}
	run.publisher.atPublish = run.rootGeneration
	run.root = &keptTree{key: keptRoot, token: mustClaimToken(t, keptRoot, claim.RoleArchitect), conn: newKeptConn(),
		topic: architectTopic(t, keptRoot),
		issue: record.Issue{Key: keptRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: keptRoot, Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress",
			Rank: "A", LastDispatchSeq: 1},
	}
	// The second root is handed to Legion and waits in `todo` behind an issue Dispatch has not
	// finished (keptBlocker), which is what keeps it in the line rather than promoted: a person's
	// decision on it is kept for the start its admission will queue (record.WaitsForSlot).
	run.queued = &keptTree{key: keptQueued, token: mustClaimToken(t, keptQueued, claim.RoleArchitect), conn: newKeptConn(),
		topic: architectTopic(t, keptQueued),
		issue: record.Issue{Key: keptQueued, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: keptQueued, Title: "Queued", Phase: phase.Admitted, Generation: 1,
			Status: "todo", DispatchStatus: "todo", Rank: "D", HandedOver: true, LastDispatchSeq: 1},
	}
	run.roots = []*keptTree{run.root, run.queued}
	run.putIssue(run.root.issue)
	run.putIssue(run.queued.issue)
	// The root runs, so it holds the slot its promotion gave it (takeSlot).
	if err := run.takeSlot(keptRoot); err != nil {
		run.fatalf("give %s the slot its run holds: %v", keptRoot, err)
	}
	run.mirror(keptRoot, "in_progress", 1)
	run.mirror(keptBlocker, "in_progress", 1)
	run.mirror(keptQueued, "todo", 1, keptBlocker)
	run.restart()
	// The run opens as a fresh admission does: the architect's start, naming what the root keeps,
	// which is nothing yet.
	run.enqueueStart()
	return run
}

// mirror writes what Dispatch says of key — its status, no parent, and the keys it waits on — as
// the blockers.Mirror handler writes one from an event (record.DispatchReading), under the project
// key admission judges the line by ("LEGION", the one the records carry): the copy is what each
// root's chain is read from (record.Chains), and its blockers are the only thing that holds a root
// out of the line here (blockers.WaitsOn).
func (r *keptRun) mirror(key, status string, seq int64, waits ...string) {
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.PutDispatchIssue(r.ctx, tx, record.DispatchReading{Key: key, Project: "LEGION", Seq: seq,
			Status: record.Carry(status), Parent: record.Carry(""), Blockers: record.Carry(waits)})
	}); err != nil {
		r.fatalf("mirror what Dispatch says of %s: %v", key, err)
	}
}

// restart builds the supervisor and the outbox runner again over the claim store and the fake
// runtime the run keeps: a daemon that died and booted, its memory gone and Postgres and the
// stored claims as they were. The first call is the run's first boot, which restores nothing.
func (r *keptRun) restart() {
	r.t.Helper()
	first := r.sup == nil
	if !first {
		r.sup.stop()
	}
	sup := newSupervisor(r.ctx, nil, "legion", r.stateDir, quietLogger())
	sup.deps = supervise.Deps{
		Runtime: r.rt, Conns: fake.NewConns(), Store: r.claims, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		// The daemon's own readings of whether a root architect's task may be sent and of what that
		// task says now (workflowRuntime.rootHears, rootTask), over this run's pool and store.
		RootHears:      (&workflowRuntime{pool: r.pool, records: r.records}).rootHears,
		RootTask:       (&workflowRuntime{pool: r.pool, records: r.records}).rootTask,
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: 24,
		RefreshGate:    supervise.NewRefreshGate(0),
		// The supervisor runs on the run's own mark (r.ctx), and each goroutine a machine starts —
		// a timer's firing, a prompt's send, whose outcome is a delivery write — takes a mark of its
		// own, as the daemon gives it (daemon.go's supervise.Deps). Without it every such goroutine
		// would share the run's mark (Machine.worker) and collide with the run's own reads.
		Worker: store.ForConcurrentWorker,
	}
	r.t.Cleanup(sup.stop)
	if !first {
		if _, err := sup.restore(r.ctx, r.storedClaims()); err != nil {
			r.fatalf("restore the stored claims: %v", err)
		}
	}
	r.sup = sup
	r.runner = &outbox{
		log: quietLogger(), pool: r.pool, dispatchProject: "LEGION", records: r.records, notices: r.publisher,
		supervisor: sup, trees: r.trees, tokens: outboxTokens{}, handlers: []intake.Handler{r.engine},
		project: "legion", stateDir: r.stateDir, now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: r.t.TempDir(), Bookmark: "legion/" + keptRoot}, nil
		},
	}
}

// storedClaims is every claim the store holds, each with the delivery the store holds for it, as
// the boot reads them (store.Store.Claims): a restart rebuilds the machines from these alone.
func (r *keptRun) storedClaims() []supervise.Claim {
	stored, err := r.claims.Claims(r.ctx)
	if err != nil {
		r.fatalf("read the stored claims: %v", err)
	}
	return stored
}

// drive runs the seed's operations, each chosen among those the model state enables.
func (r *keptRun) drive() {
	for r.step = 1; r.step <= r.steps; r.step++ {
		r.snapshotRows()
		enabled := r.enabled()
		enabled[r.rng.IntN(len(enabled))]()
		r.observe()
		r.deliverRowsTheRunDoesNotDrive()
		r.heldNoStaleLease()
	}
}

// enabled is every operation the model state allows now, in a fixed order so one seed always picks
// the same one.
func (r *keptRun) enabled() []func() {
	ops := []func(){r.toggleForget, r.togglePublisher, r.toggleLaunch, r.restartDaemon}
	if !r.architectHears(r.root) {
		ops = append(ops, r.decide)
	}
	if r.waitsForItsSlot(r.queued) {
		ops = append(ops, r.decideQueued)
	}
	if r.root.issue.Status == "in_progress" {
		ops = append(ops, r.park)
	}
	if record.OutOfWorkflow(r.root.issue.Status) {
		ops = append(ops, r.todoWait)
	}
	if r.parkedOrWaiting() {
		ops = append(ops, r.readmit)
	}
	due := r.rows(true)
	if len(due) > 0 {
		ops = append(ops, r.runRow)
	}
	if len(due) > 1 {
		ops = append(ops, r.runTogether)
	}
	if len(r.keptCatchUps(due)) > 0 && len(r.raceOps(0)) > 0 {
		ops = append(ops, r.raceRow)
	}
	for _, tree := range r.roots {
		if r.readyEnabled(tree) {
			ops = append(ops, func() { r.ready(tree) })
		}
		if _, _, told := r.readyFactEnabled(tree); told {
			ops = append(ops, func() { r.readyFact(tree) })
		}
		if r.turnEnabled(tree) {
			ops = append(ops, func() { r.turn(tree) })
		}
		if r.heldTask(tree) {
			ops = append(ops, func() { r.foreignTurn(tree) }, func() { r.restartStreaming(tree) })
		}
		if r.busyRefusalEnabled(tree) {
			ops = append(ops, func() { r.busyRefusalAfterAConfirmation(tree) })
		}
		if r.lateRefusalEnabled(tree) {
			ops = append(ops, func() { r.lateRefusalAfterAConfirmation(tree) })
		}
		if r.deathEnabled(tree) {
			ops = append(ops, func() { r.deathInTheTasksTurn(tree) })
		}
	}
	return ops
}

// architectHears is whether the tree's architect claim has an agent that said it can be prompted
// now, so a decision made on an issue of that tree reaches the tree itself and nothing is kept
// for it.
func (r *keptRun) architectHears(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	return found && supervise.SaidReady(machine.Claim().State)
}

// parkedOrWaiting is whether the root has no run of its own: out of Legion's work, or waiting in
// todo for an admission slot.
func (r *keptRun) parkedOrWaiting() bool {
	return r.root.issue.Status != "in_progress"
}

// rootKeys is every root the run drives decisions for, which is where kept decisions can stand.
func (r *keptRun) rootKeys() []string {
	keys := make([]string, 0, len(r.roots))
	for _, tree := range r.roots {
		keys = append(keys, tree.key)
	}
	return keys
}

// decide is a person deciding on an ask of the root while nobody hears it, kept by the one rule
// both paths that find such a decision follow (record.RelayParkedDecision).
func (r *keptRun) decide() { r.decideOn(keptRoot) }

// decideQueued is a person deciding on an ask of the second root while it waits for its slot: the
// rule keeps it for the start its admission will queue and tells the controller nothing
// (record.RelayKept).
func (r *keptRun) decideQueued() { r.decideOn(keptQueued) }

// decideOn keeps one person's decision made on an ask of issue, a root of this run, which is the
// record RelayParkedDecision judges (workflow's relayParkedDecision reads the same root). The debt
// is that root's architect's.
func (r *keptRun) decideOn(issue string) {
	r.decided++
	ref := fmt.Sprintf("dispatch://%s/ask/ask-%04d", issue, r.decided)
	actor := []string{"user:alice", "user:bob"}[r.rng.IntN(2)]
	kind := []record.DecisionKind{record.DecisionAskAnswered, record.DecisionAskResolved}[r.rng.IntN(2)]
	r.at = r.at.Add(time.Minute)
	outcome, tree := record.RelayNone, ""
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		stored, err := r.records.Issue(r.ctx, tx, issue)
		if err != nil || stored == nil {
			return err
		}
		root := stored
		if stored.Tree != issue {
			if root, err = r.records.Issue(r.ctx, tx, stored.Tree); err != nil || root == nil {
				return err
			}
		}
		tree = root.Key
		outcome, err = record.RelayParkedDecision(r.ctx, tx, r.records, issue, *root, true,
			record.ParkedDecision{Ref: ref, Decision: kind, Actor: actor, At: r.at}, 0, "", nil)
		return err
	}); err != nil {
		r.fatalf("keep the decision %s: %v", ref, err)
	}
	if outcome == record.RelayKept || outcome == record.RelaySent {
		r.tracked = append(r.tracked, keptRef{ref: ref, issue: issue})
		r.refIssue[ref] = issue
		r.owedTree[ref] = tree
	}
	r.tracef("decide ref=%s issue=%s tree=%s by=%s kind=%s outcome=%s kept=%t", ref, issue, tree, actor, kind,
		keptOutcome(outcome), outcome == record.RelayKept || outcome == record.RelaySent)
}

func keptOutcome(outcome record.RelayOutcome) string {
	switch outcome {
	case record.RelayKept:
		return "kept"
	case record.RelaySent:
		return "sent"
	case record.RelayNoPerson:
		return "no-person"
	case record.RelayNoPersonWaiting:
		return "no-person-waiting"
	case record.RelayNotParked:
		return "not-parked"
	default:
		return "none"
	}
}

// park takes the root out of Legion's work — closed as done, or a person's backlog over a root
// already closed — releases the slot its run held, as admission's own pass does for an issue that
// needs none, and writes the architect's suspend as the workflow writes one (workflow's leave).
// Its tree lifecycle is confirmed closed here, as its cleanup confirms it once every claim of it
// has retired (store.CleanupReservedIssue), since those rows are among the ones this run does not
// drive.
func (r *keptRun) park() {
	status := []string{"done", "backlog"}[r.rng.IntN(2)]
	r.root.issue.Phase, r.root.issue.Status = phase.Done, status
	r.putRoot()
	r.releaseSlot(keptRoot)
	r.closeTreeLifecycle(keptRoot)
	r.enqueue(keptRoot, record.SuperviseRequest{Op: "suspend", Tree: r.root.issue.Tree, Role: claim.RoleArchitect,
		Generation: r.root.issue.Generation, Reason: "the issue left Legion's work"})
	r.tracef("park status=%s generation=%d suspend-row=%d", r.root.issue.Status, r.root.issue.Generation, r.lastRow())
}

// todoWait hands the parked root back to Legion in todo with no slot, where record.WaitsForSlot
// finds it waiting: a person's decision on it is kept for the start its admission will queue, and
// the controller is told nothing.
func (r *keptRun) todoWait() {
	r.root.issue.Status, r.root.issue.HandedOver = "todo", true
	r.putRoot()
	r.tracef("todo-wait generation=%d waits=%t", r.root.issue.Generation, r.waitsForSlot())
}

// readmit is the root's new run (workflow.Engine.NewRun: a todo on a record at phase done): the
// next generation's record, the slot its promotion gives it and the architect start it enqueues,
// naming what the root keeps and claiming those rows for that start in one transaction, exactly as
// admit.Admission writes them.
func (r *keptRun) readmit() {
	if err := r.readmitRoot(); err != nil {
		r.fatalf("re-admit the root: %v", err)
	}
}

// readmitRoot is readmit without the test's own end: the interleaving step runs it on a goroutine
// of its own, where a failure is the step's to report.
func (r *keptRun) readmitRoot() error {
	r.root.issue.Generation++
	r.root.issue.Status, r.root.issue.Phase = "in_progress", phase.Admitted
	if err := r.putIssueErr(r.root.issue); err != nil {
		return err
	}
	if err := r.takeSlot(keptRoot); err != nil {
		return err
	}
	return r.enqueueStartRow()
}

// takeSlot gives key the admission slot and the open tree lifecycle its promotion gives a root it
// admits (admit's promoteHolds writes both in one transaction): the lifecycle is the shadow the
// release before reads (R22 of dispatch://INFRA-3320), and the slot is what keeps
// admission's own pass from taking the root out of the workflow as one no slot can reach
// (releaseUnslotted). The index is the lowest one no slot holds, which is admission's own rule
// (its nextSlotIndex): the slots a run holds are not a run of indexes from zero once a root in
// the middle of them has given its own back, and the column is unique.
func (r *keptRun) takeSlot(key string) error {
	return store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		slots, err := r.records.Slots(r.ctx, tx)
		if err != nil {
			return err
		}
		held := make(map[int]bool, len(slots))
		for _, slot := range slots {
			held[slot.Index] = true
		}
		index := 0
		for held[index] {
			index++
		}
		if err := r.records.PutSlot(r.ctx, tx, record.Slot{Issue: key, Index: index, AdmittedAt: time.Now().UTC()}); err != nil {
			return err
		}
		return openTreeTx(r.ctx, tx, "LEGION", key)
	})
}

// closeTreeLifecycle confirms the cleanup of key's tree, as the tree_close rows of a tree that has
// ended confirm it once every claim of the tree has retired (store.CleanupReservedIssue). Those
// rows are among the ones this run does not drive, so their one lasting mark is written here.
func (r *keptRun) closeTreeLifecycle(key string) {
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		lifecycle, err := treelifecycle.Trees.ReserveCleanup(r.ctx, tx, "legion", key, treelifecycle.AuthorityWorkflow)
		if err != nil {
			return err
		}
		return treelifecycle.Trees.ConfirmCleanup(r.ctx, tx, "legion", key, lifecycle.Epoch)
	}); err != nil {
		r.fatalf("close the tree lifecycle of %s: %v", key, err)
	}
}

// releaseSlot gives back the slot of a root that left the workflow, as admission's own pass does
// for every issue that no longer needs one (releaseSlots).
func (r *keptRun) releaseSlot(key string) {
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.ReleaseSlot(r.ctx, tx, key)
	}); err != nil {
		r.fatalf("release the slot of %s: %v", key, err)
	}
}

func (r *keptRun) enqueueStart() {
	r.t.Helper()
	if err := r.enqueueStartRow(); err != nil {
		r.fatalf("enqueue the architect start of generation %d: %v", r.root.issue.Generation, err)
	}
}

func (r *keptRun) enqueueStartRow() error {
	var kept *record.KeptDecisions
	now := time.Now()
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		task, named, err := r.engine.ParkedDecisionTask(r.ctx, tx, r.root.issue)
		if err != nil {
			return err
		}
		kept = named
		return r.engine.EnqueueNaming(r.ctx, tx, r.root.issue.Key, record.SuperviseRequest{Op: "start",
			Tree: r.root.issue.Tree, Role: claim.RoleArchitect, Generation: r.root.issue.Generation, Task: task, Kept: named}, named, now)
	}); err != nil {
		return err
	}
	r.tracef("re-admit generation=%d start-row=%d names=%d", r.root.issue.Generation, r.lastRow(), keptCount(kept))
	return nil
}

func keptCount(kept *record.KeptDecisions) int {
	if kept == nil {
		return 0
	}
	return len(kept.Decisions)
}

// runRow runs one due supervise or notice row of a driven tree, chosen at random rather than in
// row order: the supervise rows of one issue do not run in row order (record's outboxInOrder). A
// row whose run returns nothing is finished as its lane finishes it, unless the generator chose
// the crash: the finish never commits and the row keeps its lease, as a crashed daemon's row keeps
// it until it lapses (outboxLease), so the row runs again at a later step under a fresh lease
// (noteCrash records the lease it left). A row whose run failed is settled as its lane settles it
// (runOne).
func (r *keptRun) runRow() {
	due := r.rows(true)
	row := due[r.rng.IntN(len(due))]
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		r.fatalf("decode outbox row %d: %v", row.ID, err)
	}
	runErr := r.runOne(&row, payload)
	crashed := runErr == nil && r.rng.IntN(6) == 0
	if runErr == nil && !crashed {
		if err := r.finishRow(row); err != nil {
			r.fatalf("finish outbox row %d: %v", row.ID, err)
		}
	}
	r.tracef("run-row id=%d issue=%s kind=%s what=%s outcome=%s", row.ID, row.Issue, row.Kind, keptRowWhat(payload),
		keptRunOutcome(runErr, crashed))
	if crashed {
		r.noteCrash(row)
	}
}

// runTogether is the daemon's lanes running beside each other: several due rows of the driven
// trees, each under a lease token of its own, run on a goroutine each and all waited for. One row
// at a time is the shape every other step has, and it is not the shape the daemon has: two lanes
// publish, settle and drop catch-ups beside each other, and only the database serializes them.
// Every row of the step holds its lease (leaseOutboxRow) before any row of the step starts to run,
// as a lane holds its claim (record.Store.ClaimRow) before it starts its row, so a drop of the
// root's catch-ups (record.Store.DropCatchUps) that one row runs sees the lease of each other row
// as it sees a lane's claim. A lease written after another row of the step has started can commit
// inside that row's drop, which then deletes a row it read unleased, a schedule no lane makes while
// its lease is live (dispatch://INFRA-2836). The model picks its rows by next_at alone, without the
// lanes' claim rule (record's outboxClaimable), so it still differs from a lane in three cases: a
// step can run two rows of one issue's lifecycle lane at once, which outboxLaneFree forbids the
// lanes; a row whose simulated crash left its lease can run again at a later step while that lease
// is live, where a lane waits for the lease to lapse; and quiesce, which runs any row left, can run
// a start that outboxInOrder holds behind an older issue_branch row of its issue. A row whose run
// returned nothing is finished and one whose run failed is settled as its lane settles it
// (retryRow). What a step in this mode did is read once every goroutine is done, so a failure it
// finds is reported with its seed and its trace, which replay may not reproduce exactly.
func (r *keptRun) runTogether() {
	due := r.rows(true)
	r.rng.Shuffle(len(due), func(i, j int) { due[i], due[j] = due[j], due[i] })
	count := 2
	if most := min(len(due), keptTogether); most > 2 {
		count = 2 + r.rng.IntN(most-1)
	}
	rows := due[:count]
	before := r.claimStates()
	answered := r.publisher.answers()
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range rows {
		rows[i].LeaseToken = leaseOutboxRow(r.t, r.pool, rows[i].ID)
	}
	for i := range rows {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = r.runner.execute(r.worker(), rows[i])
		}(i)
	}
	wg.Wait()
	r.waitMachines()
	what := make([]string, 0, count)
	for i, row := range rows {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			r.fatalf("decode outbox row %d: %v", row.ID, err)
		}
		if errs[i] == nil {
			if err := r.finishRow(row); err != nil {
				r.fatalf("finish outbox row %d: %v", row.ID, err)
			}
		} else if err := r.retryRow(row, errs[i]); err != nil {
			r.fatalf("retry outbox row %d: %v", row.ID, err)
		}
		what = append(what, fmt.Sprintf("id=%d issue=%s what=%s outcome=%s", row.ID, row.Issue, keptRowWhat(payload),
			keptRunOutcome(errs[i], false)))
	}
	r.noteTogether(before, answered)
	r.counts.together++
	r.tracef("run-together rows=%d [%s]", count, strings.Join(what, " | "))
}

// raceRow runs one due catch-up with the listener's answer held open: while the listener has the
// notice and has answered nothing, one other operation runs to completion on a goroutine of its
// own, and only then does the publish answer. It is the one interleaving the daemon holds no lock
// across — the publish is outside every transaction of the row's run — and it is where a ready, a
// start's settlement and a new run meet a catch-up that is already on its way: each of them
// drops the root's queued catch-ups (Store.DropCatchUps) or moves the root past the generation the
// catch-up states, while the row that is publishing still claims the decisions it names.
func (r *keptRun) raceRow() {
	catchUps := r.keptCatchUps(r.rows(true))
	row := catchUps[r.rng.IntN(len(catchUps))]
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		r.fatalf("decode outbox row %d: %v", row.ID, err)
	}
	ops := r.raceOps(row.ID)
	if len(ops) == 0 {
		r.fatalf("no operation to interleave with the publish of outbox row %d", row.ID)
	}
	race := r.prepareRace(ops[r.rng.IntN(len(ops))])
	race.mode = []keptPublishMode{keptDelivers, keptReceiptTimeout}[r.rng.IntN(2)]
	answered := r.publisher.answers()
	r.publisher.arm(race)
	runErr := r.runOne(&row, payload)
	r.publisher.disarm()
	if runErr == nil {
		if err := r.finishRow(row); err != nil {
			r.fatalf("finish outbox row %d: %v", row.ID, err)
		}
	}
	if race.ran {
		r.counts.raced++
	} else if race.unrun != nil {
		if err := race.unrun(); err != nil {
			r.fatalf("release what the race on the publish of outbox row %d took and never ran: %v", row.ID, err)
		}
	}
	if race.err != nil {
		r.fatalf("the operation interleaved with the publish of outbox row %d (%s) failed: %v", row.ID, race.what, race.err)
	}
	r.tracef("race-row id=%d what=%s answer=%s during=%s ran=%t [%s] outcome=%s", row.ID, keptRowWhat(payload),
		race.mode, race.what, race.ran, race.outcome, keptRunOutcome(runErr, false))
	// A receipt timeout the re-hold answered with no copy of its own is the superseded branch or
	// the spent one (notice_exceptions.go's reholdNotice): the operation that ran inside the
	// publish moved the root or its architect past the catch-up, or the row was the last copy the
	// re-hold queues. Nothing takes over the claims this row holds, so they are handed to a fresh
	// catch-up.
	if r.publisher.answers()[keptReceiptTimeout] > answered[keptReceiptTimeout] && r.copiesOf(row.ID) == 0 {
		r.counts.supersededTimeouts++
		r.tracef("the re-hold of row %d queued no copy: the publish was superseded while it ran or its re-holds are spent, so what it claimed is handed over", row.ID)
	}
}

// copiesOf is how many copies of an outbox row the re-hold has queued (record.Notice.ResendOf).
func (r *keptRun) copiesOf(row int64) int {
	var copies int
	if err := r.pool.QueryRow(r.ctx, "select count(*) from outbox where (payload->>'resend_of')::bigint = $1", row).Scan(&copies); err != nil {
		r.fatalf("count the copies of outbox row %d: %v", row, err)
	}
	return copies
}

// keptRaceOp is one operation the generator can run inside a catch-up's publish, named before the
// step picks among them so that one seed always picks the same one.
type keptRaceOp struct {
	what   string
	kind   string
	tree   *keptTree
	row    record.OutboxRow
	key    string
	launch uint64
}

// raceOps is every operation that can run inside a catch-up's publish now: the ready fact of a
// launch whose agent is up and whose ready the daemon has not told the workflow yet, which writes
// that launch its catch-up; one other due start row of a driven root — a stale start, or one that
// finds the architect running and applies the ready fact itself; and the root's new run, which is
// the other thing that supersedes a catch-up (catchUpSuperseded reads the root's generation
// against the catch-up's). The row the step is publishing is left out, and so is every row that
// touches no kept decision.
func (r *keptRun) raceOps(publishing int64) []keptRaceOp {
	var ops []keptRaceOp
	for _, tree := range r.roots {
		key, launch, told := r.readyFactEnabled(tree)
		if !told {
			continue
		}
		ops = append(ops, keptRaceOp{kind: "ready", tree: tree, key: key, launch: launch,
			what: fmt.Sprintf("the ready fact of %s's architect at launch %d", tree.key, launch)})
	}
	for _, row := range r.rows(true) {
		if row.ID == publishing || row.Kind != record.OutboxKindSupervise {
			continue
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			continue
		}
		if request, ok := payload.(record.SuperviseRequest); !ok || request.Op != "start" {
			continue
		}
		ops = append(ops, keptRaceOp{kind: "row", row: row,
			what: fmt.Sprintf("the run of row %d of %s (%s)", row.ID, row.Issue, keptRowWhat(payload))})
	}
	if r.parkedOrWaiting() {
		ops = append(ops, keptRaceOp{kind: "readmit", what: fmt.Sprintf("the new run of %s", keptRoot)})
	}
	return ops
}

// prepareRace is the armed interleaving for one operation: whatever a lease or a claim token the
// operation needs is taken here, on the test's own goroutine, so the goroutine the publish starts
// does nothing but the daemon's work and reports what it made of it.
func (r *keptRun) prepareRace(op keptRaceOp) *keptRace {
	race := &keptRace{what: op.what}
	switch op.kind {
	case "ready":
		race.run = func() error {
			if err := r.applyReadyFact(op.tree, op.key, op.launch); err != nil {
				return err
			}
			race.outcome = "the ready fact was applied"
			return nil
		}
	case "row":
		row := op.row
		row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
		race.run = func() error {
			runErr := r.runner.execute(r.ctx, row)
			if runErr == nil {
				if err := r.finishRow(row); err != nil {
					return err
				}
			} else if err := r.retryRow(row, runErr); err != nil {
				return err
			}
			race.outcome = keptRunOutcome(runErr, false)
			return nil
		}
		race.unrun = func() error {
			_, err := r.pool.Exec(r.ctx, "update outbox set lease_token = '', lease_until = null where id = $1 and lease_token = $2",
				row.ID, row.LeaseToken)
			return err
		}
	default:
		race.run = func() error {
			if err := r.readmitRoot(); err != nil {
				return err
			}
			race.outcome = fmt.Sprintf("the root stands at generation %d", r.root.issue.Generation)
			return nil
		}
	}
	return race
}

// runOne leases the row and runs it as its lane would, settles a run that returned an error as the
// lane settles it (retryRow), and records what the run did to the architect's claim and to what
// the row named (noteRun). A row that ran without error is the caller's to finish.
func (r *keptRun) runOne(row *record.OutboxRow, payload record.OutboxPayload) error {
	tree := r.treeRow(row.Issue)
	state, answered := r.claimState(tree), r.publisher.answers()
	row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
	runErr := r.runner.execute(r.ctx, *row)
	r.waitMachines()
	if runErr != nil {
		if err := r.retryRow(*row, runErr); err != nil {
			r.fatalf("retry outbox row %d: %v", row.ID, err)
		}
	}
	r.noteRun(tree, state, answered, payload, runErr)
	return runErr
}

// finishRow finishes a row as its lane finishes it, under the lease the run took.
func (r *keptRun) finishRow(row record.OutboxRow) error {
	return store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.FinishOutbox(r.ctx, tx, row.ID, row.LeaseToken)
	})
}

// retryRow settles a row whose run returned runErr as its lane settles a failure (outbox.go's
// settle and retry): the store's RetryOutbox under the run's lease, which clears the lease, due
// again now rather than after the lane's backoff, since the run's time is compressed.
func (r *keptRun) retryRow(row record.OutboxRow, runErr error) error {
	return store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.RetryOutbox(r.ctx, tx, row.ID, row.LeaseToken, time.Now().UTC(), runErr.Error())
	})
}

// keptCrashLease is the lease a crash the run simulated left on a row (runRow): its token and the
// moment it lapses. A new lease of the row writes a new moment, so it no longer matches.
type keptCrashLease struct {
	token string
	until time.Time
}

// noteCrash records the lease a simulated crash left on row, which heldNoStaleLease lets stand
// while it does: a crashed daemon's lease stays until it lapses (outboxLease), and the run
// leases the row afresh at the later step that runs it again.
func (r *keptRun) noteCrash(row record.OutboxRow) {
	var until time.Time
	if err := r.pool.QueryRow(r.ctx, "select lease_until from outbox where id = $1 and lease_token = $2",
		row.ID, row.LeaseToken).Scan(&until); err != nil {
		r.fatalf("read the lease the crash left on outbox row %d: %v", row.ID, err)
	}
	if r.crashed == nil {
		r.crashed = map[int64]keptCrashLease{}
	}
	r.crashed[row.ID] = keptCrashLease{token: row.LeaseToken, until: until}
}

// heldNoStaleLease is a check of the model itself, run at the end of every step of drive, when no
// goroutine of the model runs a row: no row of a driven tree holds a live lease (a lease token and
// a lease_until after now) that no run holds. A lane's settle clears or ends the lease of every row
// it ran — finished, retried — so a lease the run leaves standing is a lane that does not exist,
// and a drop of the root's catch-ups (record.Store.DropCatchUps) keeps a catch-up for it that a
// daemon would drop. The one lease let stand is the one a crash the run simulated left (noteCrash),
// and only while it stands: the row's next lease, its finish or its retry ends it.
func (r *keptRun) heldNoStaleLease() {
	type leased struct {
		id          int64
		what, token string
		until       time.Time
	}
	rows, err := r.pool.Query(r.ctx, `select id, kind || ' ' || coalesce(payload->>'op', payload->>'kind', ''), lease_token, lease_until
		from outbox where issue = any($1) and lease_token <> '' and lease_until > now() order by id`, r.rootKeys())
	if err != nil {
		r.fatalf("read the leased rows of %s: %v", strings.Join(r.rootKeys(), ", "), err)
	}
	var found []leased
	for rows.Next() {
		var row leased
		if err := rows.Scan(&row.id, &row.what, &row.token, &row.until); err != nil {
			rows.Close()
			r.fatalf("scan a leased row: %v", err)
		}
		found = append(found, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		r.fatalf("read the leased rows of %s: %v", strings.Join(r.rootKeys(), ", "), err)
	}
	for _, row := range found {
		if crash, ok := r.crashed[row.id]; ok && crash.token == row.token && crash.until.Equal(row.until) {
			continue
		}
		r.fatalf("outbox row %d (%s) still holds lease %s when step %d has ended and no run holds it: a lane's "+
			"settle clears the lease of every row it ran, and only a crash leaves one standing; %s",
			row.id, row.what, row.token, r.step, r.stepNames(row.id))
	}
}

// stepNames is what the trace of the step just run says of outbox row id: every entry of the step
// that names it, or that none does.
func (r *keptRun) stepNames(id int64) string {
	prefix := fmt.Sprintf("%3d ", r.step)
	var named []string
	for i := len(r.trace) - 1; i >= 0 && strings.HasPrefix(r.trace[i], prefix); i-- {
		if line := r.trace[i]; strings.Contains(line, fmt.Sprintf("id=%d ", id)) || strings.Contains(line, fmt.Sprintf("row %d ", id)) {
			named = append([]string{strings.TrimPrefix(line, prefix)}, named...)
		}
	}
	if len(named) == 0 {
		return "the step's trace names it nowhere"
	}
	return "the step's trace says: " + strings.Join(named, " / ")
}

// noteRun records what one row's run did, read against the claim and the listener as they stood
// before it: a start that relaunched a failed claim (the outbox's start, case StateFailed), a
// catch-up the executor finished because nobody will hold the architect's role again — the only
// ending a no-holder answer has besides the wait — a catch-up a refusal or a denial finished
// undelivered while its root heard (noteRefusal), and the claim's own fall to failed, which the
// daemon tells the workflow (claimFailed).
func (r *keptRun) noteRun(tree *keptTree, before supervise.ClaimState, answered [keptModes]int, payload record.OutboxPayload, runErr error) {
	switch value := payload.(type) {
	case record.SuperviseRequest:
		if value.Op == "start" && before == supervise.StateFailed {
			r.counts.retriedFailed++
		}
	case record.Notice:
		if value.CatchUp == nil {
			break
		}
		if runErr == nil && r.publisher.answers()[keptNoHolder] > answered[keptNoHolder] {
			r.counts.endedCatchUps++
			if claimEnded(before) {
				r.counts.endedByFailure++
			}
			r.tracef("catch-up finished undelivered: nobody will hold the architect's role again (claim=%s)", keptClaimState(before))
		}
		if tree != nil && refusedOrDenied(r.publisher.answers(), answered) {
			r.noteRefusal(tree)
		}
	}
	if tree == nil {
		return
	}
	if after := r.claimState(tree); before != supervise.StateFailed && after == supervise.StateFailed {
		r.counts.failedClaim++
		r.claimFailed(tree)
	}
}

// noteTogether reads what a step that ran several rows at once did: which architect claims fell to
// failed while they ran, which the daemon's terminal hook tells the workflow, how many catch-ups
// ended for an architect that will not hold its role again, and whether a refusal or a denial
// finished one undelivered. Which row did which is not read: the rows ran beside each other, and
// only what they left behind is a fact of the step, so a refusal is noted for every driven tree
// whose root heard then.
func (r *keptRun) noteTogether(before map[string]supervise.ClaimState, answered [keptModes]int) {
	if ended := r.publisher.answers()[keptNoHolder] - answered[keptNoHolder]; ended > 0 {
		r.counts.endedCatchUps += ended
	}
	refused := refusedOrDenied(r.publisher.answers(), answered)
	for _, tree := range r.roots {
		if refused {
			r.noteRefusal(tree)
		}
		if after := r.claimState(tree); before[tree.key] != supervise.StateFailed && after == supervise.StateFailed {
			r.counts.failedClaim++
			r.claimFailed(tree)
		}
	}
}

// refusedOrDenied says whether a catch-up publish was refused by its architect's holder or denied
// by the listener's own NATS grant between the two readings of the publisher's answers: the two
// endings that finish a catch-up undelivered with its decisions left to the row's deletion, which
// no retry of that row and no fresh catch-up can change while the refusal stands.
func refusedOrDenied(after, before [keptModes]int) bool {
	return after[keptRefuses] > before[keptRefuses] || after[keptDenied] > before[keptDenied]
}

// noteRefusal records that a catch-up of tree was refused or denied while tree's root had an
// architect to hear it, so what that catch-up named was released by the row's deletion with
// nothing to name it again. What ends a refusal in real life is the architect's next launch, whose
// ready names every unclaimed decision, and quiesce drives one (relaunchAfterRefusal).
func (r *keptRun) noteRefusal(tree *keptTree) {
	if !r.architectToHear(tree.key) {
		return
	}
	if r.refused == nil {
		r.refused = map[string]bool{}
	}
	r.refused[tree.key] = true
}

// claimStates is the state of every driven architect claim now, by the key of its root.
func (r *keptRun) claimStates() map[string]supervise.ClaimState {
	states := make(map[string]supervise.ClaimState, len(r.roots))
	for _, tree := range r.roots {
		states[tree.key] = r.claimState(tree)
	}
	return states
}

// claimState is the architect's claim as the supervisor holds it now, and "" when this daemon
// supervises none, which is what the notice executor reads for a claim it does not supervise
// (claimEnded).
func (r *keptRun) claimState(tree *keptTree) supervise.ClaimState {
	if tree == nil {
		return ""
	}
	machine, found := r.sup.Machine(tree.token)
	if !found {
		return ""
	}
	return machine.Claim().State
}

func keptClaimState(state supervise.ClaimState) supervise.ClaimState {
	if state == "" {
		return "none"
	}
	return state
}

// claimFailed tells the workflow that an architect's claim spent its launch budget, as the
// daemon's terminal hook does (workflowRuntime.applyTerminal), carrying the same cause, counters
// and limits that hook carries: the engine writes a worker-died of that architect, which goes to
// that same architect, with a copy to the controller, and waits ahead of its catch-ups.
func (r *keptRun) claimFailed(tree *keptTree) {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		r.fatalf("the architect claim of %s is gone", tree.key)
	}
	held := machine.Claim()
	if _, err := intake.ApplyFact(r.ctx, r.pool, terminalSource, terminalEventID(held, supervise.StateFailed),
		intake.ClaimFailed{Issue: tree.key, Role: claim.RoleArchitect, Reason: held.Failure,
			Cause: held.FailureCause, Budgets: held.Budgets, Limits: r.sup.deps.Limits}, r.engine); err != nil {
		r.fatalf("apply the failure of %s's architect at launch %d: %v", tree.key, held.Generation, err)
	}
	r.tracef("claim-failed tree=%s launch=%d refusals=%d notice-row=%d", tree.key, held.Generation, r.rt.refused(), r.lastRow())
}

func keptRunOutcome(err error, crashed bool) string {
	switch {
	case err != nil:
		return "error: " + err.Error()
	case crashed:
		return "ran, finish lost to a crash"
	default:
		return "ran and finished"
	}
}

// keptRowWhat names an outbox row's payload for the trace.
func keptRowWhat(payload record.OutboxPayload) string {
	switch value := payload.(type) {
	case record.SuperviseRequest:
		return fmt.Sprintf("%s %s generation=%d names=%d", value.Op, value.Role, value.Generation, keptCount(value.Kept))
	case record.Notice:
		if value.CatchUp != nil {
			return fmt.Sprintf("catch-up generation=%d launch=%d names=%d resends=%d",
				value.CatchUp.Generation, value.CatchUp.Launch, keptCount(value.CatchUp.Kept), value.Resends)
		}
		return string(value.Kind)
	default:
		return fmt.Sprintf("%T", payload)
	}
}

// readyEnabled is whether a tree's architect claim has a launch to make ready: one coming up, or
// one live whose connection a restart took with it.
func (r *keptRun) readyEnabled(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		return false
	}
	switch state := machine.Claim().State; state {
	case supervise.StateLaunching, supervise.StateShimConnected, supervise.StateRegistered:
		return true
	case supervise.StateReady, supervise.StateWorking, supervise.StateIdle:
		_, connected := r.sup.deps.Conns.Conn(tree.token)
		return !connected
	default:
		return false
	}
}

// ready is an architect's agent coming up on its launch: its connection, the shim's hello, the
// registration and the ready. What the workflow makes of that ready is a step of its own
// (readyFact): the daemon's terminal hook runs outside the machine's own transition, so a claim
// stands ready for as long as that hook takes to tell the workflow, and a restart that forgot it
// has only the claim's stored state to tell it from.
func (r *keptRun) ready(tree *keptTree) {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		r.fatalf("the architect claim of %s is gone", tree.key)
	}
	r.sup.deps.Conns.(*fake.Conns).Register(tree.token, tree.conn)
	launch := machine.Claim().Generation
	r.handle(machine, supervise.StreamHello{Claim: tree.token, Generation: launch})
	// Each architect is a session of its own, with a file of its own: the daemon holds a session's
	// id and its file's path and name to the first claim of the project that recorded them
	// (supervise's holdRecorded), so two trees' architects registering one would have the second
	// refused.
	session := "ses-architect-" + tree.key
	if state := machine.Claim().State; state == supervise.StateShimConnected || state == supervise.StateRegistered || supervise.SaidReady(state) {
		r.handle(machine, supervise.RequestRegister{Claim: tree.token, Generation: launch, Session: session, SessionFile: "/tmp/" + session + ".jsonl"})
	}
	if state := machine.Claim().State; state == supervise.StateRegistered || supervise.SaidReady(state) {
		r.handle(machine, supervise.RequestReady{Claim: tree.token, Generation: launch, Session: session})
	}
	r.waitMachines()
	r.tracef("ready tree=%s launch=%d state=%s", tree.key, launch, machine.Claim().State)
}

// readyFactEnabled is whether a launch an architect's agent came up on is one the workflow has
// not been told about yet: the claim has said it is ready, and the fact that launch's ready
// writes (terminalEventID, one per launch) has not been applied.
func (r *keptRun) readyFactEnabled(tree *keptTree) (string, uint64, bool) {
	machine, found := r.sup.Machine(tree.token)
	if !found || !supervise.SaidReady(machine.Claim().State) {
		return "", 0, false
	}
	launch := machine.Claim().Generation
	key := terminalEventID(supervise.Claim{Token: tree.token, Generation: launch}, supervise.StateReady)
	return key, launch, !r.applied[key]
}

// readyFact is the ready fact the daemon applies for a launch its agent came up on
// (workflowRuntime.applyTerminal), which is what writes that launch its catch-up (workflow's
// claimReady).
func (r *keptRun) readyFact(tree *keptTree) {
	key, launch, enabled := r.readyFactEnabled(tree)
	if !enabled {
		r.fatalf("the architect of %s has no ready to tell the workflow", tree.key)
	}
	if err := r.applyReadyFact(tree, key, launch); err != nil {
		r.fatalf("apply the ready of %s's architect for launch %d: %v", tree.key, launch, err)
	}
	r.waitMachines()
	r.tracef("ready-fact tree=%s launch=%d catch-up-row=%d", tree.key, launch, r.lastRow())
}

// applyReadyFact is readyFact's write alone, which the interleaving step runs on a goroutine of
// its own, where a failure is the step's to report.
func (r *keptRun) applyReadyFact(tree *keptTree, key string, launch uint64) error {
	if _, err := intake.ApplyFact(r.ctx, r.pool, terminalSource, key,
		intake.ClaimReady{Issue: tree.key, Role: claim.RoleArchitect, Launch: launch}, r.engine); err != nil {
		return err
	}
	r.applied[key] = true
	return nil
}

// turnEnabled is whether an architect holds a task it acknowledged and has not started a turn on,
// or is working a turn that confirmed its task — the turn a restart's get_state found, or one the
// agent started itself — which only its end retires.
func (r *keptRun) turnEnabled(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		return false
	}
	held := machine.Claim()
	pending := held.Pending
	if pending == nil {
		return false
	}
	if !pending.DeliveredAt.IsZero() && pending.ConfirmedAt.IsZero() {
		return true
	}
	return held.State == supervise.StateWorking && !pending.ConfirmedAt.IsZero()
}

// turn is the architect's turn on the task it holds, from the stream's agent_start to its
// agent_end, which retires the delivery. A claim already working is in the turn: only its end is
// left, and the agent's own state says so no longer (SetStreaming).
func (r *keptRun) turn(tree *keptTree) {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		r.fatalf("the architect claim of %s is gone", tree.key)
	}
	if machine.Claim().State != supervise.StateWorking {
		r.handle(machine, supervise.StreamTurnStart{Claim: tree.token})
		r.waitMachines()
	}
	tree.conn.SetStreaming(false)
	r.endTurn(tree, machine)
	r.waitMachines()
	r.tracef("turn tree=%s state=%s pending=%t", tree.key, machine.Claim().State, machine.Claim().Pending != nil)
}

// endTurn is a turn's end as Oh My Pi reports it: the agent_end, and, for the turn a confirmed
// task's prompt ran, that prompt's result, which is what retires the delivery now that a role's
// own background work can start turns the daemon never prompted (supervise's promptResulted). A
// turn no confirmed task was running in ends with no result, as on the wire; a result for a
// delivery the claim no longer holds is fenced out by the machine.
func (r *keptRun) endTurn(tree *keptTree, machine *supervise.Machine) {
	pending := machine.Claim().Pending
	r.handle(machine, supervise.StreamTurnEnd{Claim: tree.token})
	if pending != nil && !pending.ConfirmedAt.IsZero() {
		r.handle(machine, supervise.StreamPromptResult{Claim: tree.token, DeliveryID: pending.ID, AgentInvoked: true})
	}
}

// heldTask is whether the architect's claim holds a task of a run that nobody has sent and nobody
// has acknowledged: a root architect's start task held back because its root has no architect to
// hear it (supervise's sendPending, Deps.RootHears). The rows it names are held by its delivery,
// and its own confirmation is what tells them.
func (r *keptRun) heldTask(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		return false
	}
	held := machine.Claim()
	pending := held.Pending
	return pending != nil && pending.ConfirmedAt.IsZero() && pending.DeliveredAt.IsZero() && pending.Generation != 0 &&
		(held.State == supervise.StateReady || held.State == supervise.StateIdle)
}

// foreignTurn is a turn the agent started on its own — an Envoy notice, a person's message in its
// session — while a task nobody sent waits on its claim: agent_start naming no delivery, and then
// its end. A restored claim the machine still has to ask about (askFirst) counts that turn as the
// held task's (table.go's turnStarted), so the task is confirmed without ever being sent.
func (r *keptRun) foreignTurn(tree *keptTree) {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		r.fatalf("the architect claim of %s is gone", tree.key)
	}
	before := machine.Claim().Pending.ID
	r.handle(machine, supervise.StreamTurnStart{Claim: tree.token})
	r.waitMachines()
	r.handle(machine, supervise.StreamTurnEnd{Claim: tree.token})
	r.waitMachines()
	r.counts.foreign++
	r.tracef("foreign-turn tree=%s delivery=%s state=%s pending=%t", tree.key, before, machine.Claim().State,
		machine.Claim().Pending != nil)
}

// restartStreaming is the daemon dying and booting while an architect holds a task nobody sent,
// and its agent then answering get_state that it is in a turn. The restored claim is one the
// machine has to ask about before it acts (askFirst), and the answer it gets is what
// sendPending's askFirst block reads before anything asks whether the root hears.
func (r *keptRun) restartStreaming(tree *keptTree) {
	r.restart()
	tree.conn.SetStreaming(true)
	r.counts.streamed++
	r.tracef("restart-streaming tree=%s claims=%d architect=%s: its agent answers get_state in a turn",
		tree.key, r.sup.count(), keptClaimState(r.claimState(tree)))
}

// busyRefusalEnabled is whether this tree's architect can be given the send a turn of the agent's
// own overtakes: it holds a task of a run nobody has sent, a connection is registered to send it
// through, and its root hears — or is the root this run re-admits, which is what makes it hear.
// The task must still carry a decision of this tree (heldForItsTree): the send narrows it to what
// the record still shows standing in the claim's tree (supervise's Deps.RootTask, the daemon's
// rootTask), and a task narrowed to nothing is retired rather than sent, so no prompt would come.
func (r *keptRun) busyRefusalEnabled(tree *keptTree) bool {
	if !r.heldTask(tree) || !r.connected(tree) {
		return false
	}
	if !r.architectToHear(tree.key) && !(tree.key == keptRoot && r.parkedOrWaiting()) {
		return false
	}
	return r.heldForItsTree(tree) > 0
}

// busyRefusalAfterAConfirmation is the send a turn of the agent's own overtakes. The task held on
// the claim is sent, and its prompt waits at the gate — in flight, unanswered, exactly where the
// daemon holds no lock. While it is out the agent starts a turn of its own (an Envoy notice's, a
// person's message in its pane), which the machine takes for this task's and confirms, telling
// every decision the task names (the claim store's tellKeptDecisions). Oh My Pi then answers that
// very prompt busy, since the turn it is in is not this task's: the machine takes the
// confirmation back, charges nothing (supervise's refused), and the write that records it puts
// back every mark the confirmation told (the claim store's putBackARetakenConfirmation). The
// turn ends, and the task is sent again under the id it kept, which the shim and this run read
// as the same telling.
func (r *keptRun) busyRefusalAfterAConfirmation(tree *keptTree) {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		r.fatalf("the architect claim of %s is gone", tree.key)
	}
	if !r.architectToHear(tree.key) {
		r.readmit()
	}
	gate := tree.conn.hold(keptBusyRefusal)
	// A turn the agent started itself, whose end is what sends the task the claim holds
	// (turnEnded's sendPending).
	r.handle(machine, supervise.StreamTurnStart{Claim: tree.token})
	r.handle(machine, supervise.StreamTurnEnd{Claim: tree.token})
	if !keptArrived(gate) {
		if tree.conn.standDown(gate) {
			r.waitMachines()
			r.tracef("busy-refusal tree=%s: nothing was sent, so no prompt met the gate; pending=%s state=%s",
				tree.key, keptPendingID(machine.Claim()), machine.Claim().State)
			return
		}
		<-gate.arrived
	}
	sent := keptPendingID(machine.Claim())
	// The turn of the agent's own, which the machine takes for the task's while its send is in
	// flight (table.go's turnStarted) and confirms.
	r.handle(machine, supervise.StreamTurnStart{Claim: tree.token})
	confirmed := machine.Claim().Pending != nil && !machine.Claim().Pending.ConfirmedAt.IsZero()
	// The agent's busy answer to the prompt held open, which takes that confirmation back.
	close(gate.release)
	r.waitMachines()
	r.noteTakeBack(tree, sent)
	// The turn ends, and its end sends the task again.
	r.handle(machine, supervise.StreamTurnEnd{Claim: tree.token})
	r.waitMachines()
	if confirmed {
		r.counts.refusedConfirmations++
	}
	r.tracef("busy-refusal tree=%s delivery=%s confirmed-in-flight=%t pending=%s state=%s", tree.key, sent,
		confirmed, keptPendingID(machine.Claim()), machine.Claim().State)
}

// awaitingItsTurn is whether this tree's architect holds a task its agent acknowledged and no turn
// has started on: the prompt was answered, the machine waits for the turn it opens (TimerTurn),
// and the next agent_start is taken for this task's and confirms it (table.go's turnStarted). It
// is the window both take-backs of a confirmation are reached through, and the claim is the same
// one the turn step would drive.
func (r *keptRun) awaitingItsTurn(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		return false
	}
	held := machine.Claim()
	if held.State != supervise.StateReady && held.State != supervise.StateIdle {
		return false
	}
	pending := held.Pending
	return pending != nil && !pending.DeliveredAt.IsZero() && pending.ConfirmedAt.IsZero()
}

// takenBackOnce is whether the task this tree's architect holds is one the machine has already
// taken back and sent again, so it waits under an id of the machine's own (keptRun.resent). The
// invariant accepts one telling more to one architect and no more, and one take-back is what
// earns it, so the model drives one per task: a second would be another accepted fault of the
// same kind, and says nothing of the machinery the first does not.
func (r *keptRun) takenBackOnce(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		return false
	}
	pending := machine.Claim().Pending
	if pending == nil {
		return false
	}
	_, minted := r.resent[pending.ID]
	return minted
}

// lateRefusalEnabled is whether a replayed refusal can take this tree's architect's confirmation
// back (supervise's takesBackConfirmed): the architect holds an acknowledged task a turn is about
// to confirm, nothing has taken that task back before, and a connection is registered for the
// shim to redial on. No send is in flight — every step waits for the sends it started — so the
// refusal reaches that branch rather than the read mark's.
func (r *keptRun) lateRefusalEnabled(tree *keptTree) bool {
	return r.awaitingItsTurn(tree) && r.connected(tree) && !r.takenBackOnce(tree)
}

// lateRefusalAfterAConfirmation is the shim redialing and replaying its backlog. A turn of the
// agent's own starts while the machine waits for this task's, so the machine takes it for the
// task's and confirms, telling every decision the task names (tellKeptDecisions). The agent's own
// answer to the prompt that task was sent under then arrives from the backlog of the connection
// that replaced the one it was sent through, and says the agent was busy: that turn was not this
// task's and the task was never read, so the machine takes the confirmation back under a new id
// the shim has no record of (supervise's takesBackConfirmed, takeBackPending), and the write that
// records it puts back every mark the confirmation told. The turn ends and the task goes again:
// the same decisions reach the same architect a second time, the accepted lesser fault — the
// daemon cannot know whether the agent read the first (held).
func (r *keptRun) lateRefusalAfterAConfirmation(tree *keptTree) {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		r.fatalf("the architect claim of %s is gone", tree.key)
	}
	sent := keptPendingID(machine.Claim())
	r.handle(machine, supervise.StreamTurnStart{Claim: tree.token})
	r.waitMachines()
	if !r.confirmedPending(tree) {
		r.endTurnUnconfirmed(tree, machine, "late-refusal", sent)
		return
	}
	// The shim redialed: its connection is registered again, newer than the one the task was sent
	// through, which is what says the turn that confirmed the task was no prompt of this
	// connection's (supervise's sentThrough).
	r.sup.deps.Conns.(*fake.Conns).Register(tree.token, tree.conn)
	r.handle(machine, supervise.StreamLateRefusal{Claim: tree.token, DeliveryID: sent, Error: keptBusyRefusal,
		Replayed: true, ConnSequence: tree.conn.Sequence()})
	r.waitMachines()
	r.noteTakeBack(tree, sent)
	// The turn the agent was in ends, and its end sends the task again.
	r.handle(machine, supervise.StreamTurnEnd{Claim: tree.token})
	r.waitMachines()
	r.counts.lateRefusals++
	r.tracef("late-refusal tree=%s delivery=%s pending=%s state=%s", tree.key, sent,
		keptPendingID(machine.Claim()), machine.Claim().State)
}

// deathEnabled is whether this tree's architect can be given the death of the process running its
// task's turn: it holds an acknowledged task a turn is about to confirm, nothing has taken that
// task back before (takenBackOnce), and it records the process that would run it. A death that
// would spend the claim's last one (supervise's chargeDeath) and one while every launch is
// refused are left out: both end in a failed claim, which is the launch budget's own path and is
// driven by toggleLaunch.
func (r *keptRun) deathEnabled(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	if !found || r.rt.refusing() || !r.awaitingItsTurn(tree) || r.takenBackOnce(tree) {
		return false
	}
	held := machine.Claim()
	return held.Locator != nil && held.Budgets.Deaths+1 < r.sup.deps.Limits.LaunchFailures
}

// deathInTheTasksTurn is the architect's process dying in the turn on its task, reported by the
// runtime as the refusal's relaunch is (relaunchAfterRefusal). The turn starts, which confirms the
// task and tells what it names, and the process is then found gone. Oh My Pi does not go on with
// an interrupted turn, so the machine takes the task back under a new id the relaunched process's
// shim has no record of (supervise's interrupted), and the write that records it puts back every
// mark that confirmation told (putBackARetakenConfirmation). The relaunch's agent comes up and
// its ready sends the task again, which tells the same decisions to the same architect a second
// time: the accepted lesser fault, since the turn died and nobody can say what of it the agent
// read.
func (r *keptRun) deathInTheTasksTurn(tree *keptTree) {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		r.fatalf("the architect claim of %s is gone", tree.key)
	}
	sent := keptPendingID(machine.Claim())
	r.handle(machine, supervise.StreamTurnStart{Claim: tree.token})
	r.waitMachines()
	if !r.confirmedPending(tree) {
		r.endTurnUnconfirmed(tree, machine, "death", sent)
		return
	}
	held := machine.Claim()
	if held.Locator == nil {
		r.fatalf("the architect of %s records no process to die in the turn on its task", tree.key)
	}
	if err := machine.Handle(r.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *held.Locator, Kind: runtime.Gone, At: time.Now()}}); err != nil {
		r.fatalf("end the process of %s's architect in the turn on its task: %v", tree.key, err)
	}
	r.waitMachines()
	r.noteTakeBack(tree, sent)
	r.counts.interruptedTasks++
	r.tracef("death tree=%s delivery=%s pending=%s architect=%s: the process died in the task's turn", tree.key,
		sent, keptPendingID(machine.Claim()), keptClaimState(r.claimState(tree)))
	// The relaunch's agent comes up, and its ready is what sends the task again.
	if r.readyEnabled(tree) {
		r.ready(tree)
	}
}

// confirmedPending is whether the task this tree's architect holds stands confirmed now.
func (r *keptRun) confirmedPending(tree *keptTree) bool {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		return false
	}
	pending := machine.Claim().Pending
	return pending != nil && !pending.ConfirmedAt.IsZero()
}

// endTurnUnconfirmed ends a turn that confirmed no task, which is a turn this machine takes for a
// foreign one: a restart loses the wait for the task's turn (TimerTurn) while the store keeps the
// task acknowledged, and a turn of the agent's own is then nobody's to confirm. There is no
// confirmation to take back, so the step counts nothing and leaves the claim as the turn step
// would.
func (r *keptRun) endTurnUnconfirmed(tree *keptTree, machine *supervise.Machine, what, sent string) {
	r.handle(machine, supervise.StreamTurnEnd{Claim: tree.token})
	r.waitMachines()
	r.tracef("%s tree=%s delivery=%s: the turn confirmed no task, so nothing was taken back; pending=%s state=%s",
		what, tree.key, sent, keptPendingID(machine.Claim()), machine.Claim().State)
}

// noteTakeBack records a task the machine took back under an id of its own: the outbox row that
// task was queued by, which the new id no longer names, so a prompt under it is read as that
// row's telling again and as the one extra telling its architect may hear (keptRun.resent,
// observe, held). A take-back that kept the delivery's id (supervise's refused) records nothing:
// the send that follows is the same prompt, which the shim answers without a second turn.
func (r *keptRun) noteTakeBack(tree *keptTree, before string) {
	machine, found := r.sup.Machine(tree.token)
	if !found {
		return
	}
	pending := machine.Claim().Pending
	if pending == nil || pending.ID == before {
		return
	}
	row := keptRowOfDelivery(before)
	if origin, minted := r.resent[before]; minted {
		row = origin
	}
	r.resent[pending.ID] = row
	r.tracef("take-back tree=%s delivery=%s waits as %s: the task of row %d, to be sent again", tree.key, before,
		pending.ID, row)
}

// keptArrived waits for the prompt a gate holds open, false for a send that never came.
func keptArrived(gate *keptGate) bool {
	select {
	case <-gate.arrived:
		return true
	case <-time.After(keptSendBound):
		return false
	}
}

// connected is whether a connection is registered for the tree's architect, without which no
// delivery is sent at all (supervise's sendPending).
func (r *keptRun) connected(tree *keptTree) bool {
	_, registered := r.sup.deps.Conns.Conn(tree.token)
	return registered
}

// heldForItsTree is how many decisions the task this tree's architect holds still carries of its
// own tree: rows its pending delivery holds whose issue the record shows standing in the claim's
// tree. It is what the send's own narrowing keeps (the daemon's rootTask); a task with none left
// is retired rather than sent.
func (r *keptRun) heldForItsTree(tree *keptTree) int {
	machine, found := r.sup.Machine(tree.token)
	if !found || machine.Claim().Pending == nil {
		return 0
	}
	var held int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from parked_decisions kept
		join issues owner on owner.key = kept.issue
		join claims holder on holder.token = kept.held_by_claim and holder.tree = owner.tree
		where kept.held_by_claim = $1 and kept.held_by_delivery = $2`,
		string(tree.token), machine.Claim().Pending.ID).Scan(&held); err != nil {
		r.fatalf("count what the task held on %s still names of its own tree: %v", tree.key, err)
	}
	return held
}

// keptPendingID names the delivery a claim holds now, "none" for a claim holding none.
func keptPendingID(held supervise.Claim) string {
	if held.Pending == nil {
		return "none"
	}
	return held.Pending.ID
}

func (r *keptRun) toggleForget() {
	r.records.fails.Store(!r.records.fails.Load())
	r.tracef("forget-failure=%t", r.records.fails.Load())
}

func (r *keptRun) togglePublisher() {
	mode := keptPublishMode(r.rng.IntN(int(keptModes)))
	r.publisher.setMode(mode)
	r.tracef("publisher=%s", mode)
}

// toggleLaunch switches the runtime's refusal of every launch on and off: while it is on, every
// Spawn and Resume is refused, so the next start of a claim spends its launch budget
// (supervise.Limits.LaunchFailures) and leaves it failed, where its start rows relaunch it and its
// catch-ups find nobody to hold its role.
func (r *keptRun) toggleLaunch() {
	r.rt.setRefuses(!r.rt.refusing())
	r.tracef("launch-refusal=%t architect=%s", r.rt.refusing(), keptClaimState(r.claimState(r.root)))
}

func (r *keptRun) restartDaemon() {
	r.restart()
	r.tracef("restart claims=%d architect=%s", r.sup.count(), keptClaimState(r.claimState(r.root)))
}

// quiesce ends the run: every injected failure off, the root re-admitted when it is parked or
// waiting — never when it runs, which would hand a lost decision a fresh namer and hide it — and
// then the rows of every driven tree, its architects' launches and their turns driven, in random
// order, until no row is left and no delivery is pending.
// Two things more are driven. What holds a root out of the admission line is finished, as the
// person who closes that Dispatch issue finishes it, and a controller tick is applied so that
// admission promotes every root left waiting (unblockAndPromote): a root nobody promotes has no
// architect, and a decision owed to one is owed to an architect that never runs. And, only where
// the run met it, a catch-up a holder's refusal or the listener's own grant finished undelivered
// while its root heard (noteRefusal) leaves what it named to the row's deletion, and nothing
// names it again while that refusal stands — a fresh catch-up would be refused too. What ends a
// refusal in real life is the architect's next launch, so once nothing else is left to drive,
// each such architect is relaunched once (relaunchAfterRefusal) and that launch's ready names
// every unclaimed decision. Nothing else is relaxed: each is an operation of the machinery,
// driven as every other step's is.
func (r *keptRun) quiesce() {
	r.records.fails.Store(false)
	r.rt.setRefuses(false)
	r.publisher.setMode(keptDelivers)
	r.tracef("quiesce: every failure off, launches open, publisher delivers")
	if r.parkedOrWaiting() {
		r.readmit()
	}
	r.unblockAndPromote()
	for range keptQuiesceBound {
		r.step++
		r.snapshotRows()
		actions := []func(){}
		if len(r.rows(false)) > 0 {
			actions = append(actions, r.runLeftoverRow)
		}
		for _, tree := range r.roots {
			if r.readyEnabled(tree) {
				actions = append(actions, func() { r.ready(tree) })
			}
			if _, _, told := r.readyFactEnabled(tree); told {
				actions = append(actions, func() { r.readyFact(tree) })
			}
			if r.turnEnabled(tree) {
				actions = append(actions, func() { r.turn(tree) })
			}
		}
		if len(actions) == 0 {
			if r.relaunchAfterRefusal() {
				continue
			}
			r.tracef("quiesced")
			return
		}
		actions[r.rng.IntN(len(actions))]()
		r.observe()
		r.deliverRowsTheRunDoesNotDrive()
	}
	r.fatalf("the rows and deliveries of %s did not drain in %d quiescence steps", strings.Join(r.rootKeys(), ", "), keptQuiesceBound)
}

// unblockAndPromote finishes the Dispatch issue the queued root waits on and applies a controller
// tick, which is the pass admission makes of its own accord: the root the line held out takes a
// slot and is started, so each architect a decision is owed to is one that runs. A tick is one
// fact of the machinery, applied as any other.
func (r *keptRun) unblockAndPromote() {
	r.mirror(keptBlocker, "done", 2)
	if _, err := intake.ApplyFact(r.ctx, r.pool, "dispatch", fmt.Sprintf("quiesce-tick:%s", keptRoot),
		intake.ControllerTick{}, r.engine, r.admission); err != nil {
		r.fatalf("promote the roots the line held out: %v", err)
	}
	for _, tree := range r.roots {
		if stored := r.issueRecord(tree.key); stored != nil {
			tree.issue = *stored
		}
	}
	r.tracef("quiesce: %s is finished and the line promoted; last-row=%d", keptBlocker, r.lastRow())
}

// relaunchAfterRefusal ends one refusal the run met: the architect whose catch-up a holder's
// refusal or the listener's grant finished undelivered while its root heard is relaunched, which
// is what ends such a refusal in life — the session that would not take the delivery is gone, and
// the launch that replaces it is told everything unclaimed in its ready's catch-up. Its process is
// ended through the fake runtime as a death, which the supervisor answers with the relaunch; the
// agent is then brought up (ready) and the workflow told that ready (readyFact), as every launch
// of the run is. False is no refusal left to end.
func (r *keptRun) relaunchAfterRefusal() bool {
	for _, tree := range r.roots {
		if !r.refused[tree.key] {
			continue
		}
		delete(r.refused, tree.key)
		machine, found := r.sup.Machine(tree.token)
		if !found || machine.Claim().Locator == nil {
			r.tracef("the refusal of %s's catch-up ends with no relaunch: its architect has no process", tree.key)
			continue
		}
		r.step++
		if err := machine.Handle(r.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
			Locator: *machine.Claim().Locator, Kind: runtime.Gone, At: time.Now()}}); err != nil {
			r.fatalf("end the process of %s's architect after its catch-up was refused: %v", tree.key, err)
		}
		r.waitMachines()
		r.tracef("relaunch tree=%s architect=%s: the session that refused its catch-up is gone", tree.key,
			keptClaimState(r.claimState(tree)))
		if r.readyEnabled(tree) {
			r.ready(tree)
		}
		if _, _, told := r.readyFactEnabled(tree); told {
			r.readyFact(tree)
		}
		r.observe()
		r.deliverRowsTheRunDoesNotDrive()
		return true
	}
	return false
}

// runLeftoverRow runs one of the remaining rows whatever its backoff says: a quiesced daemon has
// run every row it holds, including a re-held copy waiting out its delay.
func (r *keptRun) runLeftoverRow() {
	left := r.rows(false)
	row := left[r.rng.IntN(len(left))]
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		r.fatalf("decode outbox row %d: %v", row.ID, err)
	}
	runErr := r.runOne(&row, payload)
	if runErr == nil {
		if err := r.finishRow(row); err != nil {
			r.fatalf("finish outbox row %d: %v", row.ID, err)
		}
	}
	r.tracef("run-row id=%d issue=%s kind=%s what=%s outcome=%s", row.ID, row.Issue, row.Kind, keptRowWhat(payload),
		keptRunOutcome(runErr, false))
}

// report says what the run's injected failures drove, so a reader of the test's output sees the
// operations the generator chose actually fire.
func (r *keptRun) report() {
	r.counts.refusedLaunches = r.rt.refused()
	r.counts.publishes = r.publisher.answers()
	r.t.Logf("seed %d: %s", r.seed, r.counts)
}

// held is the run's verdict once it has quiesced: every decision kept was told exactly once to
// the architect it was owed to, and nothing is left kept anywhere.
//
// Owed to which architect follows the decision's issue: a person deciding on a root is owed an
// answer by that root's architect, and no issue of this run moves, so any telling to the other
// root's architect, and any second telling to one architect, is a fault.
//
// A task the machine itself took back is the one second telling to one architect that is accepted
// (retold), and at most one: the task goes again under a new id, naming the same decisions, to
// the architect that was already prompted with it. The daemon takes a task back where it cannot
// know whether the agent read it — a confirmation a refusal took back (the agent answered the
// prompt busy from a turn of its own, or its shim replayed that answer after such a turn had
// confirmed the task), and a turn the death of its process ended — and sending it again is the
// lesser fault there too. Zero tellings is never one of them: the resend is excused only beside a
// telling that stands, so a decision the machine lost between the two is a fault all the same.
//
// A catch-up after one whose own telling write failed is counted the same way (afterFailedWrite):
// the first publish reached the architect and its forget did not commit, so the decision stood
// kept and a later catch-up named it again, which the architect ruled acceptable
// (https://github.com/sjawhar/legion/pull/2163#issuecomment-6542200410).
func (r *keptRun) held() {
	for _, tracked := range r.tracked {
		owedTo := r.owedTree[tracked.ref]
		var owed, retold int
		var elsewhere []string
		told := map[string]bool{}
		for _, telling := range r.told[tracked.ref] {
			if (telling.retold || telling.afterFailedWrite) && told[telling.tree] {
				// The task this architect was already told was taken back and sent again, or the
				// catch-up that told it failed its own telling write (afterFailedWrite, ruled at
				// https://github.com/sjawhar/legion/pull/2163#issuecomment-6542200410). The telling
				// it repeats is the one counted; this one is the accepted second.
				retold++
				continue
			}
			told[telling.tree] = true
			if telling.tree == owedTo {
				owed++
				continue
			}
			elsewhere = append(elsewhere, fmt.Sprintf("at step %d by %s, the architect of %s", telling.step, telling.how, telling.tree))
		}
		if owed == 1 && len(elsewhere) == 0 {
			continue
		}
		r.fatalf("%s, an ask of %s, which owes its answer to the architect of %s, was told %d times to that architect and %d times "+
			"to another root's architect (%s), want once and never (%d further tellings were tasks the machine took back and sent "+
			"again, or catch-ups after one whose telling write failed): a person's decision kept while nobody could hear it must "+
			"reach the architect of the root it was made on",
			tracked.ref, tracked.issue, owedTo, owed, len(elsewhere), keptElsewhere(elsewhere), retold)
	}
	if left := r.keptAnywhere(); len(left) != 0 {
		r.fatalf("%d decisions are still kept after the run quiesced (%s), want none: each was named by a start or catch-up that never "+
			"reached an architect, or stands under a root nothing starts again, and nothing names them again", len(left), strings.Join(left, ", "))
	}
}

// keptAnywhere is every decision still kept, with the issue it was decided on, the tree that issue
// stands in — the tree that owes it, which the row is read through now that a kept decision is
// keyed by its issue — and the row that claims it: once the run has quiesced there is no architect
// left to tell, so a row standing here is a decision nobody will be told.
func (r *keptRun) keptAnywhere() []string {
	rows, err := r.pool.Query(r.ctx, `select kept.ref, coalesce(owner.tree, ''), kept.issue, coalesce(kept.claimed_by, 0)
		from parked_decisions kept left join issues owner on owner.key = kept.issue order by kept.ref`)
	if err != nil {
		r.fatalf("read the decisions still kept: %v", err)
	}
	defer rows.Close()
	var left []string
	for rows.Next() {
		var ref, tree, issue string
		var claimed int64
		if err := rows.Scan(&ref, &tree, &issue, &claimed); err != nil {
			r.fatalf("scan a decision still kept: %v", err)
		}
		left = append(left, fmt.Sprintf("%s owed-to=%s issue=%s claimed-by=%d", ref, tree, issue, claimed))
	}
	if err := rows.Err(); err != nil {
		r.fatalf("read the decisions still kept: %v", err)
	}
	return left
}

func keptElsewhere(elsewhere []string) string {
	if len(elsewhere) == 0 {
		return "none"
	}
	return strings.Join(elsewhere, "; ")
}

// keptRefs names each kept decision and the row that claims it, which the tests of one publish
// report what a root still keeps with.
func keptRefs(decisions []record.ParkedDecision) string {
	refs := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		refs = append(refs, fmt.Sprintf("%s claimed-by=%d", decision.Ref, decision.ClaimedBy))
	}
	return strings.Join(refs, ", ")
}

// observe reads what each architect was told since the last step — the prompts its connection
// received and the catch-ups the publisher delivered to its role topic — and holds each telling to
// the invariant: no reference twice, a catch-up naming the generation its root stood at when the
// daemon decided to send it, and a start task reaching a root that has an architect to hear it.
//
// The generation a catch-up is held to is the one the publisher read as the listener took it
// (generationAtPublish), not the one the root stands at once the publish has returned: the daemon
// decides to send a catch-up at its lease-time check, before the publish, and the publish is
// network I/O outside every transaction. A new run that lands inside it leaves this telling
// the generation it was decided at, and is followed by that generation's own catch-up, queued
// behind this one.
//
// One telling is one delivery, not one frame: a prompt frame carries the daemon's delivery id, and
// a shim that sees the same id twice answers it without a second turn (supervise.Delivery), so the
// re-send a reconnect or a retried row makes is the same telling; a published notice carries the
// outbox key of the row it is a copy of (record.Notice.Published), which the listener's role lane
// and the hosts dedupe on, so a copy published under a key already delivered is too. A prompt
// under an id the machine minted when it took a task back is a telling of its own, and the task
// of the row that queued it (keptRun.resent): the shim has no record of that id, so the agent is
// prompted again, which is the one extra telling the invariant accepts (held).
//
// One prompt comes from no row at all: the daemon's own delivery of what a relaunch ended, which a
// claim holding no task is given at its ready (supervise's queueRelaunch, dispatch://INFRA-3276
// R3). Its text is the relaunch's sentence alone, so it tells no decision, and the invariant holds
// it to that: a reference in it is a telling no row accounts for.
func (r *keptRun) observe() {
	for _, tree := range r.roots {
		generation := r.storedGeneration(tree.key)
		prompts := tree.conn.Prompts()
		for ; tree.prompts < len(prompts); tree.prompts++ {
			prompt := prompts[tree.prompts]
			if r.sent[prompt.DeliveryID] {
				continue
			}
			r.sent[prompt.DeliveryID] = true
			if strings.HasPrefix(prompt.DeliveryID, relaunchDeliveryPrefix) {
				if refs := r.refsIn(prompt.Message); len(refs) != 0 {
					r.fatalf("the architect of %s was told %v by the daemon's own delivery %q of a relaunch's sentence, which names no decision",
						tree.key, refs, prompt.DeliveryID)
				}
				continue
			}
			row, retold := keptRowOfDelivery(prompt.DeliveryID), false
			if origin, minted := r.resent[prompt.DeliveryID]; minted {
				row, retold = origin, true
			}
			named, known := r.rowGen[row]
			if !known {
				r.fatalf("the architect of %s was prompted by delivery %q, whose outbox row %d the run never saw", tree.key, prompt.DeliveryID, row)
			}
			how := fmt.Sprintf("the start task of row %d, prompt %s", row, prompt.DeliveryID)
			if retold {
				how = fmt.Sprintf("the start task of row %d sent again after the machine took it back, prompt %s", row, prompt.DeliveryID)
			}
			for _, ref := range r.refsIn(prompt.Message) {
				r.tell(keptTelling{ref: ref, prompt: true, retold: retold, hears: r.architectToHear(tree.key), named: named,
					root: generation, row: row, tree: tree.key, issueTree: r.treeOf(r.refIssue[ref]), step: r.step, how: how})
			}
		}
	}
	published := r.publisher.published()
	for ; r.publishd < len(published); r.publishd++ {
		delivered := published[r.publishd]
		tree := r.treeTopic(delivered.topic)
		if tree == nil || r.keys[delivered.key] {
			continue
		}
		notice, ok := delivered.payload.(record.Notice)
		if !ok || notice.CatchUp == nil {
			continue
		}
		r.keys[delivered.key] = true
		generation, known, err := r.publisher.generationAtPublish(delivered.key)
		if err != nil {
			r.fatalf("the generation a catch-up published at could not be read: %v", err)
		}
		if !known {
			r.fatalf("the catch-up published under %s to the architect of %s was taken by the listener without its root's generation read",
				delivered.key, tree.key)
		}
		for _, ref := range r.refsIn(notice.CatchUp.Decided) {
			r.tell(keptTelling{ref: ref, named: notice.CatchUp.Generation, root: generation, row: keptRowOfKey(delivered.key), tree: tree.key,
				issueTree: r.treeOf(r.refIssue[ref]), step: r.step,
				how: fmt.Sprintf("the catch-up published under %s", delivered.key)})
		}
	}
}

// architectToHear is whether the root key, as the record holds it now, has an architect to hear a
// decision: in Legion's work and not waiting in `todo` for its slot (workflow.ArchitectHears, the
// reading the daemon holds a start task to). A start task's
// prompt is held to this rather than to the generation it names, since it names none and reaches
// the root's one architect session whenever that session is next ready.
func (r *keptRun) architectToHear(key string) bool {
	var hears bool
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		root, err := r.records.Issue(r.ctx, tx, key)
		if err != nil || root == nil {
			return err
		}
		hears, err = workflow.ArchitectHears(r.ctx, tx, r.records, *root, nil)
		return err
	}); err != nil {
		r.fatalf("read whether %s has an architect to hear a decision: %v", key, err)
	}
	return hears
}

// tell records one telling and holds it to the invariants a telling can break on its own: no
// architect is told one decision twice, a start task reaches a root that has an architect to hear
// it, and a catch-up names the generation its root was at when the daemon decided to send it.
// Which architect heard it is not one of them: where the debt stands is read once the run has
// quiesced (held).
//
// One second telling to one architect is accepted per cause, and only these: the task it was
// already told is one the machine took back — a confirmation a refusal took back, or a turn its
// process died in — and sent again under a new id (retold, keptRun.resent); or the catch-up that
// told it failed its own telling write (afterFailedWrite: its forget did not commit, so the
// decision stood kept and a later catch-up named it again), which the architect ruled acceptable
// (https://github.com/sjawhar/legion/pull/2163#issuecomment-6542200410). The daemon cannot know
// whether the agent read the first, or the write that would have recorded the telling did not
// commit, so telling it again is the lesser fault. Every other repeat is a fault: a telling
// repeats an earlier one only once, and only where that earlier telling's own write failed or its
// task was taken back.
func (r *keptRun) tell(telling keptTelling) {
	var earlier, excused int
	var first keptTelling
	for _, before := range r.told[telling.ref] {
		if before.tree != telling.tree {
			continue
		}
		if earlier == 0 {
			first = before
		}
		earlier++
		if !before.prompt && r.records.tellingFailed(before.row) {
			excused++
		}
	}
	switch {
	case earlier == 0:
	case earlier-excused == 0 && excused <= 1 && !telling.retold:
		telling.afterFailedWrite = true
	case earlier-excused == 1 && excused == 0 && telling.retold:
	default:
		r.fatalf("%s was told %d times to the architect of %s: first at step %d by %s, now by %s; a person's decision reaches its "+
			"architect once, and a second time only as a task the machine took back and sent again, or after a catch-up whose own "+
			"telling write failed",
			telling.ref, earlier+1, telling.tree, first.step, first.how, telling.how)
	}
	switch {
	case telling.prompt && !telling.hears:
		r.fatalf("%s was told by %s while %s has no architect to hear it (out of Legion's work, or waiting for its slot): a start task reaches an architect of the root's current run",
			telling.ref, telling.how, telling.tree)
	case !telling.prompt && telling.named != telling.root:
		r.fatalf("%s was told by %s, which names generation %d, while %s stood at generation %d when the daemon decided to send it: "+
			"every catch-up names the generation of its own issue when it is sent",
			telling.ref, telling.how, telling.named, telling.tree, telling.root)
	}
	r.told[telling.ref] = append(r.told[telling.ref], telling)
	r.tracef("told ref=%s generation=%d tree=%s issue-tree=%s by=%s", telling.ref, telling.named, telling.tree,
		telling.issueTree, telling.how)
}

// refsIn is every tracked reference the text names. The references are fixed width, so no one is a
// substring of another.
func (r *keptRun) refsIn(text string) []string {
	var named []string
	for _, tracked := range r.tracked {
		if strings.Contains(text, tracked.ref) {
			named = append(named, tracked.ref)
		}
	}
	return named
}

// keptRowOfDelivery is the outbox row a delivery id was minted from (outboxDeliveryPrefix), and 0
// for an id no row minted.
func keptRowOfDelivery(id string) int64 {
	rest, found := strings.CutPrefix(id, outboxDeliveryPrefix)
	if !found {
		return 0
	}
	row, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0
	}
	return row
}

// keptRowOfKey is the outbox row a published notice's dedupe key names (record.OutboxKey), and 0
// for a key no row minted. A re-held copy goes out under the key of the row it copies, which is the
// row whose text it carries.
func keptRowOfKey(key string) int64 {
	_, rest, found := strings.Cut(key, ":")
	if !found {
		return 0
	}
	row, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0
	}
	return row
}

// rows is the supervise and notice rows of every driven tree, those due now or every one left, in
// id order; the caller picks among them at random.
func (r *keptRun) rows(dueOnly bool) []record.OutboxRow {
	r.t.Helper()
	query := `select id, kind, issue, payload from outbox where issue = any($1) and kind = any($2)`
	args := []any{r.rootKeys(), []string{string(record.OutboxKindSupervise), string(record.OutboxKindNotice)}}
	if dueOnly {
		query += " and next_at <= $3"
		args = append(args, time.Now())
	}
	rows, err := r.pool.Query(r.ctx, query+" order by id", args...)
	if err != nil {
		r.fatalf("list the outbox rows: %v", err)
	}
	defer rows.Close()
	var listed []record.OutboxRow
	for rows.Next() {
		var row record.OutboxRow
		var kind string
		if err := rows.Scan(&row.ID, &kind, &row.Issue, &row.Payload); err != nil {
			r.fatalf("scan an outbox row: %v", err)
		}
		row.Kind = record.OutboxKind(kind)
		listed = append(listed, row)
	}
	if err := rows.Err(); err != nil {
		r.fatalf("list the outbox rows: %v", err)
	}
	return listed
}

// keptCatchUps is the catch-ups among rows that name decisions their root keeps. They are the
// rows a publish is held open for: a publish that carries nothing is one no interleaving can lose
// or duplicate, and the point of the interleaving is what the drop and the new run do to what the
// publishing row claims.
func (r *keptRun) keptCatchUps(rows []record.OutboxRow) []record.OutboxRow {
	var found []record.OutboxRow
	for _, row := range rows {
		if row.Kind != record.OutboxKindNotice {
			continue
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			continue
		}
		if notice, ok := payload.(record.Notice); ok && notice.CatchUp != nil && keptCount(notice.CatchUp.Kept) > 0 {
			found = append(found, row)
		}
	}
	return found
}

// snapshotRows records the generation every outbox row names, so a prompt an architect receives
// later can be attributed to the start that queued it even once that row is gone. A row's
// generation never changes, and a row is never seen under two of them.
func (r *keptRun) snapshotRows() {
	for _, row := range r.rows(false) {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			continue
		}
		switch value := payload.(type) {
		case record.SuperviseRequest:
			r.rowGen[row.ID] = value.Generation
		case record.Notice:
			if value.CatchUp != nil {
				r.rowGen[row.ID] = value.CatchUp.Generation
			}
		}
	}
}

// deliverRowsTheRunDoesNotDrive takes off the outbox every row that is neither a supervise request
// nor a notice: the controller's notices, which go to the project's controller rather than to an
// architect, and the Dispatch and GitHub writes admission queues for a promoted root — its status
// write and its issue branch — which go where this run drives nobody. Each has tests of its own.
func (r *keptRun) deliverRowsTheRunDoesNotDrive() {
	if _, err := r.pool.Exec(r.ctx, `delete from outbox where (kind <> $1 and kind <> $2)
		or (kind = $1 and payload->>'op' = 'tree_close')`,
		string(record.OutboxKindSupervise), string(record.OutboxKindNotice)); err != nil {
		r.fatalf("deliver the rows this run does not drive: %v", err)
	}
}

func (r *keptRun) putRoot() {
	r.t.Helper()
	r.putIssue(r.root.issue)
}

func (r *keptRun) putIssue(issue record.Issue) {
	r.t.Helper()
	if err := r.putIssueErr(issue); err != nil {
		r.fatalf("record %s: %v", issue.Key, err)
	}
}

func (r *keptRun) putIssueErr(issue record.Issue) error {
	return store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.PutIssue(r.ctx, tx, issue)
	})
}

// issueRecord is the record of key, nil for an issue the daemon does not record.
func (r *keptRun) issueRecord(key string) *record.Issue {
	var stored *record.Issue
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stored, err = r.records.Issue(r.ctx, tx, key)
		return err
	}); err != nil {
		r.fatalf("read the record of %s: %v", key, err)
	}
	return stored
}

// treeOf is the tree key stands in now, which is the root whose architect is owed the decisions a
// person made on its asks.
func (r *keptRun) treeOf(key string) string {
	stored := r.issueRecord(key)
	if stored == nil {
		r.fatalf("%s has no record", key)
	}
	return stored.Tree
}

// treeRow is the driven tree an outbox row belongs to, nil for a row of none.
func (r *keptRun) treeRow(issue string) *keptTree {
	for _, tree := range r.roots {
		if tree.key == issue {
			return tree
		}
	}
	return nil
}

// treeTopic is the driven tree whose architect holds topic, nil for a topic of none.
func (r *keptRun) treeTopic(topic string) *keptTree {
	for _, tree := range r.roots {
		if tree.topic == topic {
			return tree
		}
	}
	return nil
}

func (r *keptRun) enqueue(issue string, payload record.OutboxPayload) {
	r.t.Helper()
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		row, err := record.NewFencedRow(r.ctx, tx, r.records, issue, payload, time.Now())
		if err != nil {
			return err
		}
		return r.records.Enqueue(r.ctx, tx, row)
	}); err != nil {
		r.fatalf("enqueue an outbox row: %v", err)
	}
}

// lastRow is the newest outbox row's id, which names in the trace the row an operation wrote.
func (r *keptRun) lastRow() int64 {
	var id int64
	if err := r.pool.QueryRow(r.ctx, "select coalesce(max(id), 0) from outbox").Scan(&id); err != nil {
		r.fatalf("read the newest outbox row: %v", err)
	}
	return id
}

func (r *keptRun) storedGeneration(key string) uint64 {
	var generation uint64
	if err := r.pool.QueryRow(r.ctx, "select generation from issues where key = $1", key).Scan(&generation); err != nil {
		r.fatalf("read the generation of %s: %v", key, err)
	}
	return generation
}

// rootGeneration is the generation of the root whose architect holds topic, false for a topic no
// driven architect holds. The publisher reads it as it takes
// a catch-up, on whichever lane is publishing, so it answers the error rather than ending the run
// from a goroutine of its own.
func (r *keptRun) rootGeneration(topic string) (uint64, bool, error) {
	tree := r.treeTopic(topic)
	if tree == nil {
		return 0, false, nil
	}
	var generation uint64
	if err := r.pool.QueryRow(r.ctx, "select generation from issues where key = $1", tree.key).Scan(&generation); err != nil {
		return 0, false, fmt.Errorf("read the generation of %s while its catch-up published: %w", tree.key, err)
	}
	return generation, true, nil
}

// waitsForSlot is whether the root waits in `todo` for an admission slot (record.WaitsForSlot),
// which is where a person's decision on it is kept for the start its admission will queue.
func (r *keptRun) waitsForSlot() bool { return r.waitsForItsSlot(r.root) }

// waitsForItsSlot is the same reading for any driven tree, over the record as it stands now.
func (r *keptRun) waitsForItsSlot(tree *keptTree) bool {
	stored := r.issueRecord(tree.key)
	if stored == nil {
		return false
	}
	var waits bool
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		waits, err = record.WaitsForSlot(r.ctx, tx, r.records, *stored, nil)
		return err
	}); err != nil {
		r.fatalf("read whether %s waits for its slot: %v", tree.key, err)
	}
	return waits
}

// waitMachines waits for every send each architect's machine started to have had its outcome
// handled, which is what makes a prompt and its acknowledgement part of the step that caused them.
func (r *keptRun) waitMachines() {
	for _, tree := range r.roots {
		if machine, found := r.sup.Machine(tree.token); found {
			machine.Wait()
		}
	}
}

func (r *keptRun) handle(machine *supervise.Machine, ev supervise.Event) {
	r.t.Helper()
	if err := machine.Handle(r.ctx, ev); err != nil {
		r.fatalf("handle %T: %v", ev, err)
	}
}

func (r *keptRun) tracef(format string, args ...any) {
	r.trace = append(r.trace, fmt.Sprintf("%3d %s", r.step, fmt.Sprintf(format, args...)))
}

// worker is the context of a goroutine the run starts beside its own calls — a row run of the
// step that runs rows beside each other (runTogether) — with a hold mark of its own
// (store.ForConcurrentWorker), as each lane of the outbox takes in production. On r.ctx those
// goroutines would share one mark: one's transaction would set it, and the pool would refuse
// another a connection it does not hold (store.ErrNestedAcquire). The operation a publish races
// against (keptPublisher.publish) runs on r.ctx: the publishing flow waits for it outside every
// transaction, so nothing else on the run's mark runs beside it.
func (r *keptRun) worker() context.Context {
	return store.ForConcurrentWorker(r.ctx)
}

// The kept-decision run's own context carries a hold mark, so the pool refuses a second
// connection to a caller that already holds one: a goroutine started on r.ctx rather than on
// worker() collides with the run's own reads, as #5396's pod-update tests did. On the unmarked
// context this run used to take, the pool refused nothing and no run of this property could
// catch that. Here a planted holder takes a connection on the run's context, and the run's own
// read must be refused; so must a read on the supervisor's context, which every machine's
// goroutine would run on with no Deps.Worker (Machine.worker). The same read on worker(), and on
// the context Deps.Worker gives a machine's goroutine, is allowed.
func TestTheKeptRunsGuardRefusesASecondConnectionOfItsOwnContext(t *testing.T) {
	t.Parallel()
	r := newKeptRun(t, 1, 1)
	held, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	go func() {
		_ = store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
			close(held)
			<-release
			return nil
		})
	}()
	// The mark is set when the transaction takes its connection, before the body this waits for.
	<-held

	for _, shared := range []struct {
		what string
		ctx  context.Context
	}{{"the run's own read", r.ctx}, {"a read on the supervisor's context, a machine goroutine's with no Deps.Worker", r.sup.ctx}} {
		if _, err := r.pool.Exec(shared.ctx, "select 1"); !errors.Is(err, store.ErrNestedAcquire) {
			t.Fatalf("%s while the planted holder holds a connection = %v, want %v: that context carries no hold mark, so nothing of this property can catch a goroutine that shares it",
				shared.what, err, store.ErrNestedAcquire)
		}
	}
	if r.sup.deps.Worker == nil {
		t.Fatal("the run's supervisor sets no Deps.Worker, so each goroutine a machine starts shares the run's mark")
	}
	for _, own := range []struct {
		what string
		ctx  context.Context
	}{{"a read on a goroutine's own mark (worker)", r.worker()}, {"a read on a machine goroutine's mark (Deps.Worker)", r.sup.deps.Worker(r.sup.ctx)}} {
		if _, err := r.pool.Exec(own.ctx, "select 1"); err != nil {
			t.Fatalf("%s while the holder holds one = %v, want it allowed: each goroutine's mark is its own", own.what, err)
		}
	}
}

// fatalf ends the run with what broke, the command that replays it, the step it broke at and
// every operation that led there.
func (r *keptRun) fatalf(format string, args ...any) {
	r.t.Helper()
	r.t.Fatalf("%s\n\nseed %d, step %d (replay with %s; a replay can take more than one run, since the two steps that "+
		"run rows beside each other are ordered by PostgreSQL and the Go scheduler as well as by the seed)\ntrace:\n%s",
		fmt.Sprintf(format, args...), r.seed, r.step, r.replay(), strings.Join(r.trace, "\n"))
}

// replay is the command that runs this one failing subtest again, as CI ran it and with nothing
// served from the test cache: CGO_ENABLED=1, which -race needs and a Legion pod leaves at 0
// (packages/daemon/docker/worker.Dockerfile), where these failures are replayed; -count=1, since
// -run is a cacheable flag and the cache keys on the files and the environment rather than on
// what Postgres holds, so a second identical run would print `ok … (cached)` and run nothing;
// and -race, which is how CI runs this package (.github/workflows/ci.yaml) and where the
// schedules that fail come from. A seed driver hard-codes its seeds
// (kept_decision_handover_seeds_test.go, kept_decision_regression_seeds_test.go) and reads
// LEGION_KEPT_PROPERTY_SEED not at all, so -run alone names its subtest; under
// TestKeptDecisionsReachTheCurrentArchitectOnce the subtests are the seeds that variable or
// LEGION_KEPT_PROPERTY_RUNS makes (keptPropertySeeds), so the seed is named with it or -run
// matches nothing and the command passes having run no test at all, and the step count goes with
// it whenever this run took one the default does not give.
func (r *keptRun) replay() string {
	command := fmt.Sprintf("CGO_ENABLED=1 go test -count=1 -race -run '%s' ./internal/daemon/", keptRunPattern(r.t.Name()))
	if !strings.HasPrefix(r.t.Name(), keptPropertyTest+"/") {
		return command
	}
	command = fmt.Sprintf("LEGION_KEPT_PROPERTY_SEED=%d %s", r.seed, command)
	if r.steps != keptPropertySteps {
		command = fmt.Sprintf("LEGION_KEPT_PROPERTY_STEPS=%d %s", r.steps, command)
	}
	return command
}

// keptRunPattern is go test's -run pattern for one test by its full name, each element of the
// path anchored and quoted so that no sibling subtest runs beside it.
func keptRunPattern(name string) string {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		parts[i] = "^" + regexp.QuoteMeta(part) + "$"
	}
	return strings.Join(parts, "/")
}
