package replay

import (
	"cmp"
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// FactWay is how a replay comes by one kind of fact the daemon applies. A daemon applies facts
// its Dispatch consumer decodes and facts its own producers make — the API, the ticks and boots,
// the outbox's executors, supervision, GitHub — and a replay has none of those producers, so each
// kind of fact is either rebuilt from the window's record, made by the replay as the daemon's own
// producer makes it, or left out under an assumption the run names.
type FactWay string

const (
	// FactInjected is a fact the replay applies at its recorded time, or at its recorded offset
	// from the start it depends on, built from the window's record: a Dispatch event through
	// intake.DecodeDispatchEvent, or a recorded line of the act that applied it.
	FactInjected FactWay = "injected"
	// FactProduced is a fact the replay's own driver or worker stub applies as the daemon's own
	// producer applies it, from the replay's own state: its ticks, its boots and the outbox rows
	// it executes.
	FactProduced FactWay = "produced"
	// FactLeftOut is a fact a replay never applies, under the assumption FactSource.Assumption
	// names: one that changes nothing the replay decides or measures.
	FactLeftOut FactWay = "left_out"
	// FactUnrecorded is a fact no window this tool has recorded holds: its act arrived after
	// every recorded window, so no record carries a line of it and the replay injects none.
	// FactSource.From says which act makes it and when the act arrived. A window recorded after
	// that release does hold it, and the fact then takes an injected entry of its own.
	FactUnrecorded FactWay = "unrecorded"
)

// FactSource is how a replay comes by one kind of fact: its way, what it builds the fact from or
// the act of its own that applies it, and, for a fact it leaves out, the assumption it leaves it
// out under.
type FactSource struct {
	// Way is how the replay comes by the fact.
	Way FactWay
	// From is what the replay builds the fact from — the recorded Dispatch event or line it reads
	// — or the act of the replay's own that applies it; empty for a fact left out.
	From string
	// Assumption is the number of the assumption (Assumptions) the fact is left out under; zero
	// for a fact the replay applies.
	Assumption int
}

// The classes of the items of a replay's queue, which order the items of one instant: a boot
// first, then the turns that end, then the record's facts in the record's own order, then the
// ticks, the outbox rows woken at their next_at, then the turns that begin, and last the sizing of
// each piece of work opened in the instant (sizePiece), once every pass of the instant has run.
const (
	classBoot = iota
	classTurnEnd
	classAct
	classTick
	classWake
	classTurnStart
	classSize
)

// item is one thing a replay does at a moment of the simulated clock: apply a fact, end a
// segment, write a turn of a claim, boot, tick, wake an outbox row. rec and order are the record's
// moment and order of what the item comes from, which order two items of one instant and class.
type item struct {
	at    time.Time
	class int
	rec   time.Time
	order int64
	seq   int64
	// piece is the piece of work the item is an act of, nil for none: a paused piece holds its
	// items back and a cancelled one drops them. eventKey and eventKind are the issue and the type
	// of a recorded Dispatch event the item applies, "" for an item of another kind.
	piece               *piece
	eventKey, eventKind string
	done                bool
	// timer says the item is the daemon's own timer (a signal's deadline, or the next change of a
	// modelled signal's input), which the daemon keeps in memory: one that falls while the daemon
	// is down is dropped, never held for its next boot.
	timer bool
	// dispatch is the item's change to the replay's copy of Dispatch, which runs at its moment
	// whether or not the daemon is up (Dispatch is), and run its act on the daemon, which waits
	// for the daemon's next boot while it is down (assumption 24).
	dispatch func() error
	run      func() error
}

// itemQueue is the replay's queue of items, by moment, class, the record's moment and order, and
// the order they were queued in.
type itemQueue []*item

func (q itemQueue) Len() int { return len(q) }
func (q itemQueue) Less(i, j int) bool {
	x, y := q[i], q[j]
	return cmp.Or(x.at.Compare(y.at), cmp.Compare(x.class, y.class), x.rec.Compare(y.rec), cmp.Compare(x.order, y.order),
		cmp.Compare(x.seq, y.seq)) < 0
}
func (q itemQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *itemQueue) Push(x any)   { *q = append(*q, x.(*item)) }
func (q *itemQueue) Pop() any {
	old := *q
	last := old[len(old)-1]
	*q = old[:len(old)-1]
	return last
}

// lineOrder is the order of an item that comes from a line of the record, after every Dispatch
// event of the same instant: the fact a pass applied comes before what the pass wrote.
const lineOrder = int64(1) << 50

// tapped is the daemon's lines the driver reads as the replay writes them (lineTap).
var tapped = []string{
	phaseChangedLine,
	leftWorkflowLine,
	slotTakenLine,
	slotReleasedLine,
	waitsForChildrenLine,
	gateClosedLine,
	mergeForwardAskedLine,
	mergeForwardLandedLine,
	admit.CoverTakenLine,
	admit.P0StartLine,
}

// driver is one replay's run: the window and its reading, the replay's database, its simulated
// clock and log, the daemon's chain it drives, its copy of Dispatch, its queue, its worker stub's
// state and what it counts. inputs is what every run of the window reads alike (windowInputs),
// trace and lines among them, which no run writes into.
type driver struct {
	ctx    context.Context
	cfg    RunConfig
	window Window
	trace  Trace
	inputs *windowInputs
	script script
	lines  recordLines
	rule   record.SlotRule

	clock   *simClock
	st      *store.Store
	pool    *store.Pool
	records *record.Postgres
	log     *slog.Logger
	tap     *lineTap
	shadow  *shadow
	// firstStarts is the order of first starts the branch's FirstStarts, recorded, shows, whose
	// roots' ranks the replay moves as the issues of their trees take their first slots (rerank);
	// nil where the branch keeps Dispatch's ranks.
	firstStarts *firstStarts
	// token is the project's claim token, which claims and tree lifecycles are keyed by.
	token string

	engine    *workflow.Engine
	admission *admit.Admission
	handlers  []intake.Handler
	// cap is the admission cap the newest chain serves under (newChain), the ceiling of a rule that
	// admits by signals.
	cap int
	// boots is how many boots the replay has made, which names each boot's facts.
	boots int
	// sig is what the run reads and records of R11's signals (setupSignals).
	sig *runSignals

	queue itemQueue
	seq   int64
	// held is every act that fell while the daemon was down, in order, which the next boot
	// applies after it has booted (assumption 24).
	held []*item
	// wakeAt is the moment of the outbox wake queued, zero for none.
	wakeAt time.Time

	// claims is every claim the stub holds, as last written, with its pending delivery.
	claims map[claim.Token]supervise.Claim

	// inFlight is the issues in flight in the replay, started is when each issue of its current
	// entry first took a slot, childWait when each issue's children wait began, and placed when
	// each recorded event was placed, by its index in the script.
	inFlight  map[string]bool
	started   map[string]time.Time
	childWait map[string]time.Time
	placed    map[int]time.Time
	// anchored is the events whose place waits for the replay's start of the wait they ended,
	// starting, by issue, the events whose place waits for the replay's first start of that issue
	// (anchorStart, placeStarted), and reopens, by issue, each reopen of the daemon's own session
	// that waits for the replay's issue to leave the workflow (defersReopen).
	anchored []int
	starting map[string][]int
	reopens  map[string][]int
	// serving is when the daemon process that runs at the window's start begins to serve, its
	// recorded reconcile where that comes after the start (servesFrom), zero where it serves from
	// the start: every act before it waits for that boot, as an act that falls while the daemon is
	// down waits for the next.
	serving time.Time
	// signOffs is each issue whose sign-off the engine holds behind a member beneath, which the
	// replay applies again at each leave of a member of its tree.
	signOffs map[string]bool
	// drops is, for each issue whose recorded move back the replay is entering, the line of its
	// pull request leaving the merge queue the record shows before it (Move.Drop), which names the
	// move's cause in the snapshot timeline.
	drops map[string]string
	// ownFacts counts the replay's own facts, which names each (factID).
	ownFacts int
	// released is what the slots of the current fact did: a slot released before a slot taken in
	// one fact is a slot given out in the pass that freed it (assumption 11).
	released bool

	// snap is the replay's snapshot timeline (snapshots), which each admission pass's observer
	// and each settle feed, and dirty says an act ran since the last settle.
	snap  *snapshots
	dirty bool

	counters Divergence
	used     map[int]bool
	segments []Segment
	facts    int
	passTime time.Duration
	paused   bool
	pauseID  int64
	unplaced []Unplaced

	// conflicts is the fitted generator, merges the window's commits of each base branch it counts a
	// stretch's M from, loops the recorded conflict loops a drawn conflict's work is drawn from,
	// draws the run's source of every conflict draw, which reads drawSource, seeded afresh for each
	// draw by the run's seed and the draw's place (seedDraw), stretchDraws how many draws each
	// stretch has made, by its place with no count (stretchDraw), lastDraw the place of each
	// issue's latest stretch draw, at which the loop of a conflict it made is drawn (drawnLoop),
	// stretchFrom the replay's start of the stretch each issue stands in, stretches the live stretch
	// of each issue whose stretch the run has read, which holds its one draw (liveStretch),
	// liveStretches how many live stretches the run has numbered, and waiting each issue's conflict
	// past the review that waits for merging (waitingConflict). heads counts the heads the replay's
	// own pull request records moved to, which names each, and numbers is the number of each of
	// those records.
	conflicts     ConflictModel
	merges        baseMerges
	loops         []conflictLoop
	draws         *rand.Rand
	drawSource    *rand.PCG
	stretchDraws  map[stretchDraw]int
	lastDraw      map[string]stretchDraw
	stretchFrom   map[string]time.Time
	git           replayGit
	stretches     map[string]*liveStretch
	liveStretches int
	waiting       map[string]waitingConflict
	heads         int
	numbers       *pullRequestNumbers
	// noWork is each issue the replay admitted with no work, recorded or drawn (neverStartedWork),
	// whose slot-hours the run counts (Divergence.NoWorkSlotHours).
	noWork map[string]bool
	// drawn is each draw of a stretch's conflict the run made, in its order, with its key and its
	// value (recordDraw), which run.json records (runRecord.Draws).
	drawn []StretchDraw
	// unkeyed counts the draws the run made that it keys to no stretch (newChain), which run.json
	// records (runRecord.UnkeyedDraws).
	unkeyed UnkeyedDraws
}

// Divergence is what a replay counts of where it left the record's course, which a replay under
// the recorded rule leaves at zero but for the causes the record itself holds (design 10.9).
type Divergence struct {
	// Refused is each recorded move the replay entered and the daemon's code refused, with the
	// reason; the segment after it never opens.
	Refused []RefusedMove `json:"refused"`
	// KeptOpenLoops is how many stretches kept their recorded conflict, its loop running to the
	// window's end in the record, rather than draw (keepConflict, assumption 35).
	KeptOpenLoops int `json:"keptOpenLoops"`
	// SkippedLoops is how many recorded conflict loops the replay passed over, each stretch whose
	// recorded end is a conflict drawing none (enterRecorded, stretchEnds), and AddedLoops how many
	// conflicts the replay made at stretches the record ended otherwise (addsLoop): the conflicts
	// the replay's draws took away from the record and added to it (assumption 3).
	SkippedLoops int `json:"skippedLoops"`
	AddedLoops   int `json:"addedLoops"`
	// ConflictsToMerger is how many conflicts past the review, drawn or kept, the daemon the replay
	// runs left to the merger, where a daemon before cybermonklabs/legion#4979 moved the issue back
	// to implementing or asked the implementer (assumption 37): one in merging whose merger was asked
	// to merge the base forward, and one in accepting or retro, which waits for merging, where the
	// merger is asked (driver.conflictFor).
	ConflictsToMerger int `json:"conflictsToMerger"`
	// HeldSignOffs is how many sign-offs the engine held behind a member beneath, each applied
	// again at the member's leave.
	HeldSignOffs int `json:"heldSignOffs"`
	// HeldMerges is how many merges left their issue in merging, its close waiting for the issues
	// beneath it still in the workflow (record.OpenBeneath), which the engine makes at the last
	// one's leave as the daemon does.
	HeldMerges int `json:"heldMerges"`
	// Reassertions is how many starts re-sent a claim the entry it had already been given
	// (admission's repair of a role behind its entry), which opened no segment.
	Reassertions int `json:"reassertions"`
	// NoSegment is how many starts found no recorded segment of their issue left to open.
	NoSegment int `json:"noSegment"`
	// NoWorkAdmissions is how many issues the replay admitted that hold their slot with no work,
	// recorded or drawn (driver.neverStartedWork: an issue the record never started, under the
	// branch's NeverStarted none or where the window holds no issue to draw its work from), and
	// NoWorkSlotHours the slot-hours they hold in [From, To), read from the snapshot timeline.
	NoWorkAdmissions int     `json:"noWorkAdmissions"`
	NoWorkSlotHours  float64 `json:"noWorkSlotHours"`
	// DrawnWork is how many issues the record never started took the work of another issue under
	// the branch's NeverStarted drawn or drawn-done, by where the issue drawn from came from
	// (script.drawNeverStarted).
	DrawnWork DrawnWork `json:"drawnWork"`
	// SkippedSegments is how many recorded segments the replay passed over, a later one of its
	// issue opening first.
	SkippedSegments int `json:"skippedSegments"`
	// Fenced is how many Dispatch events a fence of the daemon's dropped, its issue's record
	// already past the event's sequence.
	Fenced int `json:"fenced"`
	// Unanchored is how many recorded events never took a place, their anchor never reached in
	// the replay before the window's end: the start of the wait they ended, the first start of the
	// issue a filing under it or its own architect's act on it waits for (anchorStart), or the
	// filing of their issue, which no event of the issue comes before (awaitsFiling).
	Unanchored int `json:"unanchored"`
	// Finished is how many outbox rows the stub finished without acting, by the assumption each
	// rests on.
	Finished map[int]int `json:"finished"`
	// Retried is how many times the stub gave an outbox row back to wait on its backoff, as a lane
	// gives back a row whose executor waits on something.
	Retried int `json:"retried"`
	// TurnsLeftOut is how many recorded turns the replay wrote no claim of, by the assumption each
	// rests on: a turn of an issue outside the workflow, a production check's (assumption 30),
	// standing at the window's start (SeedReport.LeftOut) or begun in a tree that runs no lifecycle
	// in the replay (ensureClaim).
	TurnsLeftOut map[int]int `json:"turnsLeftOut"`
	// NoLifecycleTurns is how many recorded turns of an issue in the workflow, or of one the replay
	// does not record, began in the replay while the issue's tree ran no lifecycle there, which the
	// replay wrote no claim of and went on (ensureClaim).
	NoLifecycleTurns int `json:"noLifecycleTurns"`
	// InferredSlots is how many of the start state's slots the trace inferred an issue held with no
	// slot line of it (assumption 34), and SlotCountDifference the slots the trace holds right after
	// the slot line at the window's start, the first at or after it, less the slots in use that line
	// says (SlotCount.Difference), for which the start state names no issue.
	InferredSlots       int `json:"inferredSlots"`
	SlotCountDifference int `json:"slotCountDifference"`
}

// RefusedMove is one recorded move the daemon's code refused in a replay.
type RefusedMove struct {
	Issue   string               `json:"issue"`
	At      time.Time            `json:"at"`
	Trigger workflow.TriggerKind `json:"trigger"`
	From    phase.Phase          `json:"from"`
	To      phase.Phase          `json:"to"`
	Reason  string               `json:"reason"`
}

// push queues it, numbered after every item queued before it.
func (d *driver) push(it *item) *item {
	d.seq++
	it.seq = d.seq
	if it.piece != nil {
		it.piece.pending = append(it.piece.pending, it)
	}
	heap.Push(&d.queue, it)
	return it
}

// now is the simulated clock's next moment.
func (d *driver) now() time.Time { return d.clock.Now() }

// newChain builds the daemon's handler chain afresh, as each of the daemon's boots does: a new
// engine and admission pass under the run's rule, on the simulated clock, with the window's
// settings and the cap the daemon process running from at served under (processCap), or the run's
// own cap where it names one (RunConfig.Cap), the replay's tables of what a rule the daemon does not
// run records (ruleTables), the replay's own handler before the chain, and its reading of the
// recorded first starts right before the admission pass (firstStartsHandler). The source of the
// experiments' draws starts afresh from the run's seed at each boot and is read in the order the
// engine and the pass record their trees and issues, keyed to no issue, so the run counts each
// draw of it (UnkeyedDraws.Experiments).
func (d *driver) newChain(at time.Time) {
	w := d.window
	source := rand.New(rand.NewPCG(uint64(d.cfg.Seed), experimentStream))
	draw := func(bound uint64) (uint64, error) {
		if bound == 0 {
			return 0, fmt.Errorf("draw a value below %d: there is no value below zero", bound)
		}
		d.unkeyed.Experiments++
		return source.Uint64N(bound), nil
	}
	d.engine = workflow.New(d.records, workflow.Config{Project: w.Project, Steps: w.GateSteps, ReviewRoundCap: w.ReviewRoundCap,
		Linger: time.Duration(w.Linger), Clock: d.clock.Now, Repositories: replayRepositories{branches: d.trace.Branches},
		Experiments: w.Experiments, ExperimentSource: draw, SlotRule: d.rule}, d.log)
	limit := d.processCap(at)
	if d.cfg.Cap > 0 {
		limit = d.cfg.Cap
	}
	d.admission, d.cap = admit.New(d.records, d.engine, limit, w.Project, d.log), limit
	d.admission.UseRuleTables(ruleTables{})
	d.admission.UseClock(d.clock.Now)
	d.admission.UseExperiments(w.Experiments)
	d.admission.UseStalledTreeNotice(time.Duration(w.StalledTreeNotice))
	d.admission.UseWorkerSilence(time.Duration(w.WorkerSilence), time.Duration(w.WorkerBackground))
	d.admission.UsePassObserver(d.observePass)
	// R10's P0 bound in force at the moment of each pass, from the window's record of its changes
	// (p0BoundAt): a rule that starts a P0 issue outside the cap admits under it, and every other
	// rule reads it not at all.
	d.admission.UseP0Bound(func() *int { return p0BoundAt(w, d.clock.Instant()) })
	// R11's signals as the replay reads them (readSignals), which a pass admits below P0 by under a
	// rule whose mode is not Off and reads not at all under any other.
	d.admission.UseSignals(d.readSignals)
	// Each of the replay's own facts runs the pass, whatever it wrote (ownFact).
	d.admission.UsePassFacts(ownFact)
	d.handlers = []intake.Handler{replayHandler{engine: d.engine, clock: d.clock.Now, rule: d.rule, records: d.records, project: w.Project}}
	for _, handler := range admit.Handlers(d.records, w.Project, d.engine, d.admission) {
		if _, pass := handler.(*admit.Admission); pass {
			d.handlers = append(d.handlers, firstStartsHandler{d: d})
		}
		d.handlers = append(d.handlers, handler)
	}
}

// firstStartsHandler is the replay's reading of its recorded first starts inside each fact, in
// the fact's own transaction, right before the admission pass (driver.reviewFirstStarts): the pass
// that gives out a slot then orders the line by the ranks the holds it reads leave, as the daemon's
// line is ordered by the ranks of the moment.
type firstStartsHandler struct {
	d *driver
}

// Apply reads the recorded first starts against the line as the admission pass of the fact is
// about to judge it.
func (h firstStartsHandler) Apply(ctx context.Context, tx pgx.Tx, _ intake.Fact) (intake.Result, error) {
	return intake.Result{}, h.d.reviewFirstStarts(ctx, tx)
}

// capAt is the admission cap w's daemon ran with at at: the last change at or before it, or the
// first where at is before every change. A change is dated by the first line naming it, which a
// process writes minutes into its boot, so the cap a process starts under is processCap's.
func capAt(w Window, at time.Time) int {
	if len(w.Caps) == 0 {
		return 0
	}
	limit := w.Caps[0].Cap
	for _, change := range w.Caps {
		if !change.At.After(at) {
			limit = change.Cap
		}
	}
	return limit
}

// p0BoundAt is R10's P0 bound w's daemon ran with at at: the last change at or before it, nil for
// no bound where at is before every change or the window records none (Window.P0Bounds). The
// daemon's bound has no value unless its configuration gives one, and the window records each
// change from the daemon's line of it, so before the first change no bound stands.
func p0BoundAt(w Window, at time.Time) *int {
	var bound *int
	for _, change := range w.P0Bounds {
		if !change.At.After(at) {
			bound = &change.Bound
		}
	}
	return bound
}

// processCap is the admission cap the daemon process running from at served under (capOfProcess),
// reading the record's boots: at a boot, that boot's own start and the line saying it finished
// booting; the next boot's start bounds the process.
func (d *driver) processCap(at time.Time) int {
	var started, until time.Time
	for _, boot := range d.trace.Boots {
		if boot.At.Equal(at) {
			started = boot.Started
		}
		if boot.At.After(at) && (until.IsZero() || boot.At.Before(until)) {
			until = boot.At
		}
	}
	return capOfProcess(d.window, at, started, until)
}

// capOfProcess is the admission cap of the daemon process that runs from from: the first cap change
// at or after from that the process's own lines name — no later than started, the line saying it
// finished its boot, where the record holds one, and before until, the next process's start — and
// the cap that stood at from where its lines name no change. The cap is the process's configuration,
// read before its first pass, so it holds from the process's first line, though the window dates the
// change by the first line naming it (collect's caps): a slot line of the boot's first pass, or its
// `legion daemon started` line, minutes after the process began. Every cap change the windows hold
// falls inside a restart so. Read at the process's start alone (capAt), a raise served the old cap
// until the next restart.
func capOfProcess(w Window, from, started, until time.Time) int {
	limit := capAt(w, from)
	for _, change := range w.Caps {
		if change.At.Before(from) {
			continue
		}
		if (!started.IsZero() && change.At.After(started)) || (!until.IsZero() && !change.At.Before(until)) {
			break
		}
		return change.Cap
	}
	return limit
}

// CapSpan is a stretch of a window over which its daemon served one admission cap, as a replay
// serves it (capSpans): from From to To, at Cap.
type CapSpan struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Cap  int       `json:"cap"`
}

// capSpans is each span of [w.From, w.To) over which w's daemon served one cap, in order, as a
// replay serves it (processCap): each process's cap from its boot to the next one's, the boots
// being the record's (Trace.Boots), the process running at w.From read from its own boot in the
// window's reach; a stretch before every boot stands at the cap the window shows at w.From
// (capAt). Adjacent processes of one cap make one span.
func capSpans(w Window, boots []Boot) []CapSpan {
	spans := []CapSpan{}
	add := func(from, to time.Time, limit int) {
		from, to = maxTime(from, w.From), minTime(to, w.To)
		if !to.After(from) {
			return
		}
		if n := len(spans); n > 0 && spans[n-1].Cap == limit && spans[n-1].To.Equal(from) {
			spans[n-1].To = to
			return
		}
		spans = append(spans, CapSpan{From: from, To: to, Cap: limit})
	}
	sorted := slices.SortedStableFunc(slices.Values(boots), func(a, b Boot) int { return a.At.Compare(b.At) })
	if len(sorted) == 0 || sorted[0].At.After(w.From) {
		end := w.To
		if len(sorted) > 0 {
			end = sorted[0].At
		}
		add(w.From, end, capAt(w, w.From))
	}
	for i, boot := range sorted {
		var until time.Time
		end := w.To
		if i+1 < len(sorted) {
			until, end = sorted[i+1].At, sorted[i+1].At
		}
		add(boot.At, end, capOfProcess(w, boot.At, boot.Started, until))
	}
	return spans
}

// replayRepositories is the replay's stand-in for the daemon's view of its repositories
// (workflow.Repositories): every repository an issue resolves to is served, on the base branch the
// window's merges name for it, or main.
type replayRepositories struct {
	branches map[string]string
}

// ResolveRepository serves resolved.
func (r replayRepositories) ResolveRepository(resolved string) (workflow.Resolution, error) {
	return workflow.Resolution{Repo: resolved, GitHubRepo: resolved, DefaultBranch: cmp.Or(r.branches[resolved], defaultBranch)}, nil
}

// apply applies fact as source's eventID through the chain, on the simulated clock, reads what the
// daemon's lines say it did, and, the pass having run, computes the project's deadline again
// (recomputeDeadline), which the pass arms once the daemon process now running has served one.
func (d *driver) apply(source, eventID string, fact intake.Fact) (intake.Result, error) {
	began := time.Now()
	d.released = false
	result, err := intake.ApplyFactWith(d.ctx, d.pool, d.records, source, eventID, fact, d.handlers...)
	d.facts++
	d.passTime += time.Since(began)
	if err != nil {
		return result, fmt.Errorf("replay: apply %s %s at %s: %w", source, eventID, stamp(d.clock.Instant()), err)
	}
	if err := d.observe(); err != nil {
		return result, err
	}
	if d.cfg.afterFact != nil {
		if err := d.cfg.afterFact(d.ctx, d.pool, d.shadow); err != nil {
			return result, err
		}
	}
	d.sig.armed = true
	return result, d.recomputeDeadline()
}

// factID names one of the replay's own facts uniquely in its run.
func (d *driver) factID(kind string, parts ...any) string {
	d.ownFacts++
	id := fmt.Sprintf("replay-%s:%d", kind, d.ownFacts)
	for _, part := range parts {
		id += fmt.Sprintf(":%v", part)
	}
	return id
}

// observe reads the daemon's lines the last fact wrote: the moves that open a segment no start
// opens and the leaves that apply a held sign-off again, the slots that put an issue in flight,
// the children waits that place the events that ended them, the moves and the merger's asks a
// conflict that waits for merging follows (waitMoves, mergerMergesForward), the first slots that
// move a root's rank under the recorded order of first starts (take), and what the run's
// assumptions are named from.
func (d *driver) observe() error {
	// A slot taken after a slot released in one fact is a freed slot given out in the pass that freed
	// it (assumption 11), unless the issue starts outside the cap (admit.P0StartLine), which takes no
	// slot of the cap. The two lines of one issue may come in either order.
	var takenAfterRelease []string
	outsideCap := map[string]bool{}
	for _, line := range d.tap.take() {
		key := line.str("issue")
		switch line.msg {
		case phaseChangedLine:
			from, to := phase.Phase(line.str("from")), phase.Phase(line.str("to"))
			trigger, drop := workflow.TriggerKind(line.str("trigger")), ""
			if trigger == workflow.TriggerBackward {
				drop = d.drops[key]
				delete(d.drops, key)
			}
			d.snap.moved(key, d.clock.Instant(), from, to, trigger, drop)
			if pastReviewPhase(to) {
				d.used[2] = true
			}
			if phase.MovesBack(from, to) {
				d.used[3], d.used[13] = true, true
			}
			if startsStretch(from, to) {
				d.stretchFrom[key] = d.clock.Instant()
			}
			// A move that begins a stretch or breaks one ends the live stretch the issue stood in,
			// with its draw and any conflict that waited for its end.
			if startsStretch(from, to) || breaksStretch(to) {
				delete(d.stretches, key)
			}
			if to == phase.Done {
				if err := d.setFlight(key, false); err != nil {
					return err
				}
			}
			d.endedGitLoops(key, to)
			d.openOnPhase(key, to)
			d.queueFreshRetries(key, to)
			if err := d.waitMoves(key, to); err != nil {
				return err
			}
		case leftWorkflowLine:
			if err := d.setFlight(key, false); err != nil {
				return err
			}
			delete(d.started, key)
			delete(d.childWait, key)
			if err := d.cancelWork(key); err != nil {
				return err
			}
			d.placeUnstarted(key)
			d.placeReopens(key)
			delete(d.waiting, key)
			delete(d.stretches, key)
			delete(d.git.loopEnds, key)
			delete(d.git.firstImpl, key)
			delete(d.git.forwardAt, key)
			tree := line.str("tree")
			for _, waiting := range slices.Sorted(maps.Keys(d.signOffs)) {
				if waiting != key && d.treeOf(waiting) == tree {
					d.push(&item{at: d.clock.Instant(), class: classAct, rec: d.clock.Instant(), order: lineOrder,
						run: func() error { return d.signOff(waiting) }})
				}
			}
		case slotTakenLine:
			d.used[9] = true
			if err := d.setFlight(key, true); err != nil {
				return err
			}
			// Only the first slot of the run the issue stands in is a first start: a slot taken
			// again in a run, after a return or a wait's end, starts nothing (firstStartIn).
			_, again := d.started[key]
			if !again {
				d.started[key] = d.clock.Instant()
				for _, root := range d.firstStarts.take(key) {
					d.rerank(root)
				}
				d.placeStarted(key)
			}
			if d.released {
				takenAfterRelease = append(takenAfterRelease, key)
			}
		case admit.P0StartLine:
			outsideCap[key] = true
		case slotReleasedLine:
			d.released = true
		case waitsForChildrenLine:
			d.used[4] = true
			if _, ok := d.childWait[key]; !ok {
				d.childWait[key] = d.clock.Instant()
			}
		case gateClosedLine:
			d.used[4] = true
		case mergeForwardAskedLine:
			d.snap.roundBegins(key, d.clock.Instant())
			if phase.Phase(line.str("phase")) == phase.Merging {
				d.mergerMergesForward(key)
			}
		case mergeForwardLandedLine:
			d.snap.roundEnds(key, d.clock.Instant())
		case admit.CoverTakenLine:
			d.used[14] = true
		}
	}
	for _, key := range takenAfterRelease {
		if !outsideCap[key] {
			d.used[11] = true
		}
	}
	d.placeAnchored()
	return nil
}

// treeOf is the tree key's record stands in, "" for an issue the replay does not record.
func (d *driver) treeOf(key string) string {
	if issue, _ := d.recorded(key); issue != nil {
		return issue.Tree
	}
	return ""
}

// recorded is key's record in the replay's database, nil for an issue it does not record.
func (d *driver) recorded(key string) (*record.Issue, error) {
	var issue *record.Issue
	err := store.TransactWith(d.ctx, d.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, err = d.records.Issue(ctx, tx, key)
		return err
	})
	return issue, err
}

// rerank queues the move of root's rank to the one its tree's next recorded first start gives it
// (firstStarts.rootRank), at the instant of the first slot or the pass that moved it, as a person's
// change of a rank on the board reaches the daemon: the move in the replay's copy of Dispatch and
// the issue.updated of the rank it makes there (shadow.rankWrite), then that event applied through
// the decoder, the preparation's stand-in and the chain (applyEcho), so the daemon's mirror and its
// next pass read the new rank. Where the rank does not move, as at a later issue's first slot ahead
// of an earlier one's, nothing is written.
func (d *driver) rerank(root string) {
	at := d.clock.Instant()
	var written []byte
	d.push(&item{at: at, class: classAct, rec: at, order: lineOrder,
		dispatch: func() error {
			rank, shown := d.firstStarts.rootRank(root)
			raw, wrote, err := d.shadow.rankWrite(root, rank, shown, d.clock.Instant())
			if wrote {
				written = raw
			}
			return err
		},
		run: func() error {
			if written == nil {
				return nil
			}
			return d.applyEcho(root, written)
		}})
}

// reviewFirstStarts reads again, in tx, the transaction of the fact being applied, right before
// its admission pass (firstStartsHandler), whether each issue whose recorded first start the replay
// has still to make (firstStarts.pending) can make it now: by the daemon's own test of the line's
// starting part (record.FirstPhaseStartsNow) over the line as that pass is about to judge it
// (admit.Admission.View), read as the line reads it (record.HeldFactsOf). An issue the replay has
// not filed yet, or has finished, moved past its first phase or taken out of the workflow cannot,
// nor can one a hold of the starting part keeps back: its tree not running or not yet opened by
// its root's promotion, its root waiting on a person, its tree's design gate closed, a blocker,
// its open children or an accepted ancestor. A root waiting for its tree to be opened cannot
// either, since its promotion starts it, which ranks it at its own recorded start
// (firstStarts.rootRank). Such an issue pins no tree's rank to its own, and one that can again
// pins it again. Where an answer changes, the rank of each root it moves is written into the
// root's record in tx, so the pass orders its line by it, as it orders the daemon's line by the
// ranks of the moment whatever the fact changed of the holds; once the fact commits, the new
// answers stand and each such root is reranked (rerank), which shows the rank in the replay's copy
// of Dispatch and reaches the daemon's mirror as a change of it.
func (d *driver) reviewFirstStarts(ctx context.Context, tx pgx.Tx) error {
	f := d.firstStarts
	if !f.left() {
		return nil
	}
	view, err := d.admission.View(ctx, tx)
	if err != nil {
		return fmt.Errorf("replay: read the line at %s for the recorded first starts: %w", stamp(d.clock.Instant()), err)
	}
	line := record.HeldFactsOf(view.Issues, view.Slots, view.Facts)
	changed := map[string]bool{}
	for _, key := range f.keys() {
		if !f.pending(key) {
			continue
		}
		issue, recorded := line.ByKey[key]
		queued := record.QueuedRoot(issue, view.Facts.Lifecycles[issue.Key], view.Facts.Gates[issue.Key])
		barred := !recorded || !record.FirstPhaseStartsNow(issue, queued, line)
		if barred != f.barred[key] {
			changed[key] = barred
		}
	}
	if len(changed) == 0 {
		return nil
	}
	next := f.with(changed)
	moved := map[string]bool{}
	for key := range changed {
		for _, root := range f.rootsOf(key) {
			moved[root] = true
		}
	}
	roots := slices.Sorted(maps.Keys(moved))
	for _, root := range roots {
		rank, shown := next.rootRank(root)
		if !shown {
			iss, held := d.shadow.issues[root]
			if !held || iss.payload == nil {
				continue
			}
			rank = iss.str("rank")
		}
		if _, err := tx.Exec(ctx, `update issues set rank = $2 where key = $1`, root, rank); err != nil {
			return fmt.Errorf("replay: write the rank of %s at %s: %w", root, stamp(d.clock.Instant()), err)
		}
	}
	store.OnCommit(ctx, func() {
		for _, key := range slices.Sorted(maps.Keys(changed)) {
			f.bar(key, changed[key])
		}
		for _, root := range roots {
			d.rerank(root)
		}
	})
	return nil
}

// run replays the window: the chain that runs at its start, the start state at its start, the
// replay's copy of Dispatch brought to the record's state there, then every item of the queue in
// order to the end of the window's inputs, its run-out's end where it holds one (Window.End), the
// worker stub after each. The measures read [From, To) alone.
func (d *driver) run() error {
	w, t := d.window, d.trace
	end := w.End()
	d.clock.advance(w.From)
	d.stretches = map[string]*liveStretch{}
	// The process that runs at the window's start is the newest that started before it; a boot
	// at the start itself is applied as a boot. It is built before the start state, which asks its
	// engine for the resume task of each owed start it seeds (seed, SeedAndReport).
	d.newChain(w.From)
	if err := d.seed(); err != nil {
		return err
	}
	// The load models read the replay's own issues in flight from its start on.
	if m := d.sig.models; m != nil {
		if err := m.Flight(w.From, len(d.inFlight)); err != nil {
			return err
		}
	}
	// The copy of Dispatch starts where the record stands at the window's start: every recorded
	// event before it, the daemon's own writes among them, at its recorded time and sequence, in
	// the script's order (shadow.seedEvent).
	for i := range d.script.events {
		event := &d.script.events[i]
		if !event.at.Before(w.From) {
			continue
		}
		if err := d.shadow.seedEvent(event.env, d.recordedBefore(event), i, event.parsed.Seq, event.at); err != nil {
			return fmt.Errorf("replay: bring the copy of Dispatch to the window's start with the %s event of %s at %s: %w", event.env.kind,
				event.env.key, stamp(event.at), err)
		}
	}
	if d.cfg.atStart != nil {
		d.cfg.atStart(d.shadow)
	}
	// An issue that stands in a stretch at the window's start began it where the record shows.
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		if start, standing := stretchStartAt(t.Issues[key], w.From); standing {
			d.stretchFrom[key] = start
		}
	}
	// Under a rule whose start may take a cover the start state the record's rule left is first
	// booted onto the rule, as the release that runs it boots (FirstBoot, spec v4 R9): every started
	// issue holding no slot takes one, and the pass of the same fact works out the covers.
	if d.rule.Take == record.TakeCover {
		if _, err := d.apply("replay", d.factID("first-boot"), FirstBoot{}); err != nil {
			return err
		}
	}
	for i := range t.Boots {
		boot := t.Boots[i]
		if boot.At.Before(w.From) || !boot.At.Before(end) {
			continue
		}
		d.push(&item{at: boot.At, class: classBoot, rec: boot.At, run: func() error { return d.boot(boot) }})
	}
	// The process that runs at the window's start serves from its recorded reconcile where that
	// comes after the start: no pass runs before it, and every act before it waits for it, as an
	// act that falls while the daemon is down waits for the next boot (servesFrom).
	if boot, serves, waits := d.servesFrom(); waits {
		d.serving = serves
		d.push(&item{at: serves, class: classBoot, rec: serves, run: func() error {
			d.serving = time.Time{}
			return d.boot(boot)
		}})
	}
	for _, at := range t.Ticks {
		if at.Before(w.From) || !at.Before(end) {
			continue
		}
		d.push(&item{at: at, class: classTick, rec: at, run: func() error { return d.tick(at) }})
	}
	for _, pause := range t.Pauses {
		if !pause.From.Before(w.From) && pause.From.Before(end) {
			d.push(&item{at: pause.From, class: classAct, rec: pause.From, order: lineOrder, run: func() error { return d.pause(pause) }})
		}
		if !pause.To.IsZero() && !pause.To.Before(w.From) && pause.To.Before(end) {
			d.push(&item{at: pause.To, class: classAct, rec: pause.To, order: lineOrder, run: func() error { return d.resume(pause) }})
		}
	}
	// Each change of R10's P0 bound the window records runs a pass at its moment, as the daemon's
	// change of the bound does (P0BoundChanged).
	for _, change := range w.P0Bounds {
		if change.At.Before(w.From) || !change.At.Before(end) {
			continue
		}
		d.push(&item{at: change.At, class: classAct, rec: change.At, order: lineOrder, run: func() error {
			_, err := d.apply("replay", d.factID("p0-bound", change.Bound), P0BoundChanged{Bound: change.Bound})
			return err
		}})
	}
	for i := range d.script.events {
		if event := &d.script.events[i]; !event.at.Before(w.From) && event.piece == nil {
			d.placeByAnchor(i)
		}
	}
	d.openSeeded()
	d.releaseSeededHolds()
	if err := d.drain(); err != nil {
		return err
	}
	if err := d.startView(); err != nil {
		return err
	}
	d.dirty = true
	for d.queue.Len() > 0 {
		it := heap.Pop(&d.queue).(*item)
		// Every act at or before each probe before this item has run: the probe reads the replay
		// as they left it.
		d.readProbes(it.at, false)
		if !it.at.Before(end) {
			break
		}
		if it.done {
			continue
		}
		if it.piece != nil && it.piece.state != pieceRunning {
			// A paused piece keeps the item for its resume; an ended, cancelled or passed-over one
			// drops it, its recorded events placed by their anchors as it was cancelled or passed
			// over (placeUnrun).
			if it.piece.state != piecePaused {
				it.done = true
			}
			continue
		}
		it.done = true
		// Every act of the instant before this one has run: the snapshot timeline takes the
		// state they left before the clock moves on.
		if d.dirty && it.at.After(d.clock.Instant()) {
			if err := d.settle(); err != nil {
				return err
			}
		}
		d.clock.advance(it.at)
		if it.dispatch != nil {
			if err := it.dispatch(); err != nil {
				return err
			}
		}
		if it.class != classBoot && (d.down(it.at) || it.at.Before(d.serving)) {
			// The daemon's timer is in its memory, so one that falls while the daemon is down never
			// fires: the boot's first pass computes the deadline again.
			if !it.timer {
				d.held = append(d.held, it)
			}
			continue
		}
		if err := it.run(); err != nil {
			return err
		}
		if err := d.drain(); err != nil {
			return err
		}
		d.dirty = true
	}
	unplaced := func(events []int) {
		for _, i := range events {
			if _, placed := d.placed[i]; !placed {
				d.counters.Unanchored++
			}
		}
	}
	unplaced(d.anchored)
	for _, waiting := range d.starting {
		unplaced(waiting)
	}
	for i := range d.script.events {
		unplaced(d.script.events[i].awaiting)
	}
	d.readProbes(time.Time{}, true)
	return d.settle()
}

// startView hands the snapshot timeline the view of the start state, read as a pass reads it
// (admit.Admission.View), so the states from the window's start to its first pass are the start
// state's.
func (d *driver) startView() error {
	return store.TransactWith(d.ctx, d.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		view, err := d.admission.View(ctx, tx)
		if err != nil {
			return fmt.Errorf("replay: read the start state at %s as a pass reads it: %w", stamp(d.window.From), err)
		}
		d.snap.pass(view)
		return nil
	})
}

// settle takes the state every act of the clock's instant left into the snapshot timeline, where
// an act ran since the last settle, and hands the instant to the test's look at it
// (RunConfig.atSettled).
func (d *driver) settle() error {
	if !d.dirty {
		return nil
	}
	d.dirty = false
	at := d.clock.Instant()
	d.snap.settle(at, d.claims, d.inFlight, d.shadow)
	if d.cfg.atSettled != nil {
		return d.cfg.atSettled(d.ctx, d.st, at)
	}
	return nil
}

// down says whether no daemon ran at at: inside a downtime of the record, after its last line and
// before the next process started, or after the last process began to stop where no process
// started after it (assumption 24).
func (d *driver) down(at time.Time) bool {
	for _, downtime := range d.trace.Downtimes {
		if downtime.From.Before(at) && at.Before(downtime.To) {
			d.used[24] = true
			return true
		}
	}
	if n := len(d.trace.Boots); n > 0 {
		if stopped := d.trace.Boots[n-1].Stopped; !stopped.IsZero() && stopped.Before(at) {
			d.used[24] = true
			return true
		}
	}
	return false
}

// seed writes the start state at the window's start (SeedAndReport), on the trace whose ranks the
// branch shows, reads its report into the run — the issues it named, and into the divergence the
// turns it wrote no claim of under each assumption, the slots the trace inferred and the difference
// of the trace's slots from the record's count, the assumptions of the turns and of the slots being
// named among those the run rests on where it counted any — and the moment each children wait the
// start state marked told began (childWait), as the line of its telling would have set it; hands the
// records it wrote to the snapshot timeline, and reads back the claims it wrote for the stub. Under
// a rule that places the line by first slots it keeps the first slot of each issue in the run it
// stands in (firstSlotBefore, keepFirstSlot), the start state holding no slot's moment once the
// slot went back; and under a rule that places it by arrival it keeps each issue's arrival, the
// record's entry of the run it stands in (arrivalBefore, keepArrival), which no record of the start
// state carries.
func (d *driver) seed() error {
	return store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		report, err := SeedAndReport(ctx, tx, d.records, d.st, d.engine, withRanks(d.trace, d.shadow.ranks), d.window.From)
		if err != nil {
			return err
		}
		d.unplaced = report.Unplaced
		for number, turns := range report.LeftOut {
			d.counters.TurnsLeftOut[number] += turns
			d.used[number] = d.used[number] || turns > 0
		}
		d.counters.InferredSlots, d.counters.SlotCountDifference = report.InferredSlots, report.SlotCount.Difference()
		if report.InferredSlots > 0 {
			d.used[assumeSlotFromBeforeTheReach] = true
		}
		maps.Copy(d.childWait, report.ChildrenTold)
		issues, err := d.records.Issues(ctx, tx)
		if err != nil {
			return err
		}
		if len(issues) > 0 {
			d.used[6] = true
		}
		d.snap.seeded(issues)
		for _, issue := range issues {
			if !record.InWorkflow(issue) {
				continue
			}
			if withinStretches(d.script.flight[issue.Key], d.window.From) {
				if err := d.setFlight(issue.Key, true); err != nil {
					return err
				}
			}
			if first := d.firstSlotBefore(issue.Key); !first.IsZero() {
				d.started[issue.Key] = first
				if readsFirstSlots(d.rule) {
					if err := keepFirstSlot(ctx, tx, issue, first); err != nil {
						return err
					}
				}
			}
			if readsArrivals(d.rule) {
				if arrived := d.arrivalBefore(issue.Key); !arrived.IsZero() {
					if err := keepArrival(ctx, tx, issue, arrived); err != nil {
						return err
					}
				}
			}
		}
		claims, err := d.st.ClaimsTx(ctx, tx)
		if err != nil {
			return err
		}
		for _, c := range claims {
			d.claims[c.Token] = c
		}
		return nil
	})
}

// firstSlotBefore is when key first took a slot in the run it stands in at the window's start,
// zero where it took none before then.
func (d *driver) firstSlotBefore(key string) time.Time {
	iss, ok := d.trace.Issues[key]
	if !ok {
		return time.Time{}
	}
	entry := iss.entryAt(d.window.From.Add(-time.Nanosecond))
	for _, span := range iss.Slots {
		if span.Start.Before(d.window.From) && (entry == nil || !span.Start.Before(entry.At) || span.Before) {
			return span.Start
		}
	}
	return time.Time{}
}

// arrivalBefore is when key entered the workflow in the run it stands in at the window's start,
// the record's entry of that run, which the rules that place by arrival read as its arrival in the
// line; zero where the record shows no entry before then.
func (d *driver) arrivalBefore(key string) time.Time {
	iss, ok := d.trace.Issues[key]
	if !ok {
		return time.Time{}
	}
	if entry := iss.entryAt(d.window.From.Add(-time.Nanosecond)); entry != nil {
		return entry.At
	}
	return time.Time{}
}

// placeByAnchor places the recorded event events[i] by its anchor, as no piece of work places it:
// at its recorded time, from its question (Branch.Answer), from the replay's start of the wait it
// ended (Branch.Timing), as the branch asks, or from the replay's first start of the issue it
// depends on (anchorStart): the issue it files a child under, or the issue its own architect acts
// on; and never before the clock's instant, which an event whose piece the replay does not run can
// stand past by then (placeUnrun). A person's answer under answer offset waits for its question to
// place it, an event that follows another, a child's filing among them, waits for it (anchorFollow,
// placeFollowers), and an event whose issue the replay has not started in its run waits for that
// start (placeStarted), or for the issue's leave where it leaves unstarted (placeUnstarted).
func (d *driver) placeByAnchor(i int) {
	event := &d.script.events[i]
	at := maxTime(d.clock.Instant(), event.at)
	switch {
	case event.anchor == anchorTime, event.anchor == anchorWait && d.cfg.Branch.timingWall(), event.anchor == anchorEvent && d.cfg.Branch.answerWall():
		d.placeEvent(i, at)
	case event.anchor == anchorWait:
		d.anchored = append(d.anchored, i)
		d.push(&item{at: at, class: classAct, rec: event.at, order: int64(event.order), run: func() error {
			d.placeUnbegun(i)
			return nil
		}})
	case event.anchor == anchorStart:
		if started, ok := d.started[event.startOf]; ok {
			d.placeEvent(i, maxTime(at, started.Add(event.offset)))
			return
		}
		d.starting[event.startOf] = append(d.starting[event.startOf], i)
	}
}

// placeStarted places each event that waited for key's first start in the replay (anchorStart),
// key having just taken its first slot of its run: each at its recorded offset from the record's
// first start of the issue, from this moment, or at its recorded time where that is later.
func (d *driver) placeStarted(key string) {
	waiting := d.starting[key]
	delete(d.starting, key)
	now := d.clock.Instant()
	for _, i := range waiting {
		event := &d.script.events[i]
		d.placeEvent(i, maxTime(event.at, now.Add(event.offset)))
	}
}

// placeUnstarted places each event that waited for key's first start in the replay (anchorStart),
// key having just left the workflow before it took that start: each at its recorded time, or at
// once where that has passed, as an event of work the leave leaves unrun enters (cancelWork). So a
// child the record filed under an issue that the replay ends before it starts it is still filed,
// once.
func (d *driver) placeUnstarted(key string) {
	waiting := d.starting[key]
	delete(d.starting, key)
	now := d.clock.Instant()
	for _, i := range waiting {
		d.placeEvent(i, maxTime(now, d.script.events[i].at))
	}
}

// placeEvent queues the recorded event events[i] at at, by its anchor (placeByAnchor), ordered
// among the items of its instant by its own moment in the record (queueEvent).
func (d *driver) placeEvent(i int, at time.Time) {
	d.queueEvent(i, at, d.script.events[i].at, nil)
}

// queueEvent queues the recorded event events[i] at at: its change to the shadow, then its fact,
// ordered among the items of its instant by rec, its moment in the record, and the act of p's work
// it is, nil for an event no piece places. A piece's event is ordered by the act's moment in the
// record of p's work: the event's own, or, for an event another piece's work took over
// (giveEvents), its moment in that work. A person's answer to it, each event that follows it
// (anchorFollow) and, for an issue's filing, each event of the issue held back for it are placed
// once it is applied (placeAnswers, placeFollowers, placeFiled). An event the replay has placed
// already is not queued again, but for a reopen of the daemon's own session that waits for the
// replay's issue to leave the workflow (defersReopen), which is queued again at that leave
// (placeReopens), and an event held back for its issue's filing (awaitsFiling), which is queued
// again once that filing is applied.
func (d *driver) queueEvent(i int, at, rec time.Time, p *piece) {
	if _, placed := d.placed[i]; placed {
		return
	}
	d.placed[i] = at
	event := &d.script.events[i]
	var applied []byte
	d.push(&item{at: at, class: classAct, rec: rec, order: int64(event.order), piece: p, eventKey: event.env.key, eventKind: event.env.kind,
		dispatch: func() error {
			var deferred bool
			var err error
			applied, deferred, err = d.dispatchEvent(i)
			if err != nil || deferred {
				return err
			}
			event.dispatched = true
			d.placeAnswers(event)
			d.placeFollowers(event)
			d.placeFiled(event)
			return nil
		},
		run: func() error { return d.applyEvent(i, applied) }})
}

// placeAnswers places each person's answer to event, a question the replay has just applied to its
// copy of Dispatch, at its recorded offset from this moment (Branch.Answer offset): an answer comes
// from the replay's moment of its question, wherever the replay placed that question — at its
// offset in its piece of work, at its moment after a pause, or by its anchor where the replay did
// not run its piece (placeUnrun) — so no answer comes before its question. Under answer wall each
// answer was placed at its recorded time at the start.
func (d *driver) placeAnswers(event *scriptEvent) {
	if d.cfg.Branch.answerWall() {
		return
	}
	now := d.clock.Instant()
	for _, answer := range event.dependents {
		d.placeEvent(answer, maxTime(now, now.Add(d.script.events[answer].offset)))
	}
}

// placeFollowers places each event that follows event in the record (anchorFollow), event just
// applied to the replay's copy of Dispatch: at its recorded offset from this moment, or at its
// recorded time where that is later. So an architect's later act on a child it filed, and its
// release or park of the child, which Dispatch records under the daemon's session, come after the
// architect's filing of the child, wherever the replay placed the filing, as in the record
// (keepsDaemonEvent judges the daemon's write against that filing), and never before their recorded
// times, which a filing the replay placed early leaves them at.
func (d *driver) placeFollowers(event *scriptEvent) {
	now := d.clock.Instant()
	for _, i := range event.followers {
		follower := &d.script.events[i]
		d.placeEvent(i, maxTime(follower.at, now.Add(follower.offset)))
	}
}

// placeFiled places each event of event's issue the replay held back for event, the issue's filing,
// just applied to its copy of Dispatch (awaitsFiling): at its recorded offset from the filing, from
// this moment, or at its recorded time where that is later.
func (d *driver) placeFiled(event *scriptEvent) {
	now := d.clock.Instant()
	held := event.awaiting
	event.awaiting = nil
	for _, i := range held {
		waited := &d.script.events[i]
		d.placeEvent(i, maxTime(waited.at, now.Add(waited.at.Sub(event.at))))
	}
}

// placeReopens queues again each reopen of the daemon's own session that waited for key to leave
// the workflow in the replay (defersReopen), key having just left: each at its recorded offset from
// the record's last leave of key before it, from this moment, so the issue's next run follows the
// replay's end of the run before it as the record's followed its own.
func (d *driver) placeReopens(key string) {
	waiting := d.reopens[key]
	delete(d.reopens, key)
	now := d.clock.Instant()
	for _, i := range waiting {
		event := &d.script.events[i]
		offset := time.Duration(0)
		if iss, ok := d.trace.Issues[key]; ok {
			for _, leave := range iss.Leaves {
				if !leave.At.After(event.at) {
					offset = event.at.Sub(leave.At)
				}
			}
		}
		d.placeEvent(i, now.Add(offset))
	}
}

// placeAnchored places each event whose anchor, the replay's start of the wait it ended in the
// record, the replay has now reached (Branch.Timing).
func (d *driver) placeAnchored() {
	kept := d.anchored[:0]
	for _, i := range d.anchored {
		if _, placed := d.placed[i]; placed {
			continue
		}
		start, reached := d.waitStart(d.script.events[i].wait)
		if !reached {
			kept = append(kept, i)
			continue
		}
		d.placeEvent(i, maxTime(start.Add(d.script.events[i].offset), d.clock.Instant()))
	}
	d.anchored = kept
}

// placeUnbegun places the recorded event events[i], whose place waits for the replay's start of
// the wait it ended (Branch.Timing offset), at its recorded time, the clock's instant, where that
// wait cannot begin (the windows' README, "The knobs": an event whose wait never begins in the
// replay happens at its recorded time): a link wait of an issue the replay has not started, its
// link standing, since an issue whose blocker is unfinished does not start, and so never waits on
// it as a started issue does.
func (d *driver) placeUnbegun(i int) {
	event := &d.script.events[i]
	if _, placed := d.placed[i]; placed || event.wait.kind != WaitLink {
		return
	}
	if _, started := d.started[event.wait.issue]; started {
		return
	}
	if event.wait.added >= 0 {
		if added, placed := d.placed[event.wait.added]; !placed || added.After(d.clock.Instant()) {
			return
		}
	}
	d.placeEvent(i, d.clock.Instant())
}

// waitStart is the replay's start of the wait an anchor names, and whether the replay has reached
// it: for a link wait, the later of the replay's moment of the event that added the link and the
// waiting issue's start; for a children wait, the moment the replay's pass began it.
func (d *driver) waitStart(wait waitAnchor) (time.Time, bool) {
	switch wait.kind {
	case WaitLink:
		started, ok := d.started[wait.issue]
		if !ok {
			return time.Time{}, false
		}
		if wait.added < 0 {
			return started, true
		}
		added, placed := d.placed[wait.added]
		if !placed || added.After(d.clock.Instant()) {
			return time.Time{}, false
		}
		return maxTime(started, added), true
	case WaitChildren:
		began, ok := d.childWait[wait.issue]
		return began, ok
	}
	return time.Time{}, false
}

// dispatchEvent applies the recorded event events[i] to the shadow at the clock's instant, read
// against its issue's event before it in the record's order (recordedBefore), and answers the event
// as the replay applies it; nil for an event of the daemon's own, the session of the window's
// project's daemon on an issue of that project, which the replay makes again from its own outbox,
// and which the shadow does not apply. Another project's daemon is another daemon the replay does
// not run: its writes on its own issues, an outside blocker's close among them, are applied as
// recorded, as a person's are. An event of an issue whose filing the replay has not applied yet
// (awaitsFiling), and a reopen of the daemon's own session that waits for the replay's issue to
// leave the workflow (defersReopen), are not applied: each answers deferred, and waits to be queued
// again.
func (d *driver) dispatchEvent(i int) (applied []byte, deferred bool, err error) {
	event := &d.script.events[i]
	if d.awaitsFiling(i) {
		return nil, true, nil
	}
	if event.env.project == d.window.Project && daemonWrote(event.env.actor, event.env.key) {
		if !d.keepsDaemonEvent(event) {
			return nil, false, nil
		}
		if deferred, err := d.defersReopen(i); err != nil || deferred {
			return nil, deferred, err
		}
	}
	if d.namesLinkOrChild(event) {
		d.used[10] = true
	}
	applied, err = d.shadow.apply(event.env, d.recordedBefore(event), i, d.clock.Instant())
	return applied, false, err
}

// awaitsFiling says whether events[i] waits for its issue's filing (scriptEvent.filing), which the
// replay has not applied yet, and holds it back where it does: no recorded event of an issue applies
// before the issue's filing in the replay, whoever made it and however the replay placed it (spec
// R2: a child's filing enters at its recorded offset from the start of the issue it is filed under,
// and the child's own course follows its filing). The event is taken out of its place and kept with
// the filing, which places it once applied, at its recorded offset from the filing and never before
// its recorded time (placeFiled). Applied first, an issue event would give the copy of Dispatch the
// issue's whole payload as that event shows it, marking every field as set by it, and the filing
// applied after it would set none of them (shadow.take): the child would stand at that event's
// status, a status of its later work, and never at the todo the architect's release writes, which
// the daemon takes into its workflow.
func (d *driver) awaitsFiling(i int) bool {
	event := &d.script.events[i]
	if event.filing < 0 {
		return false
	}
	filing := &d.script.events[event.filing]
	if filing.dispatched {
		return false
	}
	delete(d.placed, i)
	if !slices.Contains(filing.awaiting, i) {
		filing.awaiting = append(filing.awaiting, i)
	}
	return true
}

// recordedBefore is the payload of event's issue's recorded issue event before it in the record's
// order (scriptEvent.prev), which the event's change is read against; nil for none.
func (d *driver) recordedBefore(event *scriptEvent) map[string]json.RawMessage {
	if event.prev < 0 {
		return nil
	}
	return d.script.events[event.prev].env.payload
}

// keepsDaemonEvent says whether event, one the daemon's own session wrote, is a write the replay's
// outbox does not make again and so is applied as recorded. The outbox writes the workflow's own
// statuses and the done of its own close, never backlog, icebox or triage (shelved), and todo only
// where it hands an issue to Legion or sets its own todo back over a session's status. A hand-over
// adds the legion label with it; a person's hand-over and a set-back write todo at the fact that
// queued them, which the replay applies before their recorded echo, so that echo applied as well
// changes no status. The daemon's API writes those statuses under the same session for the agent
// whose call it serves: the controller's set_status (todo, backlog or icebox, and a root's restart,
// backlog then todo), an architect's park of a child (park_child, backlog, in the releases that
// have it), its release of a child into its tree (release_children, todo) and its run again of a
// child out of the workflow (rerun_child, todo). So a write that moves an issue, root or child
// alike, to backlog, icebox or triage, or from any status out of the workflow, done among them, to
// todo without adding the legion label, is applied as recorded. So is the controller's todo on a
// held root, which moves it from the status of its held phase (releasesHold): the record's release
// of that hold names a todo as its cause, for which the replay enters no retry of its own
// (queueHoldEnd), so the hold ends only where the todo's own event is applied, as a person's todo
// on a held root ends it. Every other write of the daemon's own is its outbox's status write,
// which the replay's own rows make. What a write moves the issue from is the issue's event before
// it in the record (scriptEvent.prev), whatever the replay's copy applied before it: the write's
// meaning is the record's, and an event the replay applies early or late, an act of a piece of work
// it moves, would otherwise stand in for the status the write was made over.
func (d *driver) keepsDaemonEvent(event *scriptEvent) bool {
	after := event.parsed.Issue
	if after == nil || event.prev < 0 {
		return false
	}
	before := d.script.events[event.prev].parsed.Issue
	if before == nil || before.Status == after.Status {
		return false
	}
	if shelved(after.Status) {
		return true
	}
	labelled := dispatch.CarriesLegionLabel(after.Labels) && !dispatch.CarriesLegionLabel(before.Labels)
	return after.Status == "todo" && !labelled && (record.OutOfWorkflow(before.Status) || event.releasesHold)
}

// defersReopen says whether events[i], a write of the daemon's own session the replay keeps
// (keepsDaemonEvent) that sets its issue todo from a status out of the workflow, waits for the
// replay's issue to leave the workflow: the replay's own record of the issue stands in the
// workflow, its run, which the record shows ended before the write, still under way in the replay.
// Such a write deferred is queued again at the replay's leave (placeReopens). A todo on an issue the
// replay does not record, or records out of the workflow, applies now, and so does one that ends a
// hold (releasesHold), whose issue stands held in the workflow by design.
func (d *driver) defersReopen(i int) (bool, error) {
	event := &d.script.events[i]
	if event.parsed.Issue.Status != "todo" || event.releasesHold || !record.OutOfWorkflow(d.script.events[event.prev].parsed.Issue.Status) {
		return false, nil
	}
	issue, err := d.recorded(event.env.key)
	if err != nil || issue == nil || !record.InWorkflow(*issue) {
		return false, err
	}
	delete(d.placed, i)
	d.reopens[event.env.key] = append(d.reopens[event.env.key], i)
	return true, nil
}

// shelved says whether status is out of the workflow, as done is (record.OutOfWorkflow), but not
// done: backlog, icebox or triage, which the daemon's outbox never writes (keepsDaemonEvent).
func shelved(status string) bool {
	return status != record.DoneStatus && record.OutOfWorkflow(status)
}

// namesLinkOrChild says whether event shows an issue with a link or a parent (assumption 10).
func (d *driver) namesLinkOrChild(event *scriptEvent) bool {
	shown := event.parsed.Issue
	if shown == nil {
		return false
	}
	for _, blocker := range shown.BlockedBy {
		if project, _, _ := strings.Cut(blocker, "-"); project != d.window.Project {
			d.used[25] = true
		}
	}
	return len(shown.BlockedBy) > 0 || shown.Parent != ""
}

// applyEvent applies the event the shadow answered for events[i] (applied) through the daemon's
// decoder, its preparation's stand-in and the chain.
func (d *driver) applyEvent(i int, applied []byte) error {
	if applied == nil {
		return nil
	}
	event := &d.script.events[i]
	fact, _, err := intake.DecodeDispatchEvent(d.window.Project, event.env.key, applied)
	if err != nil {
		return fmt.Errorf("replay: decode the %s event of %s at %s: %w", event.env.kind, event.env.key, stamp(event.at), err)
	}
	if fact == nil {
		return nil
	}
	if event.env.actor.Person() && answers(fact) {
		d.used[5] = true
	}
	prepared, recorded, err := d.prepare(fact)
	if err != nil {
		return err
	}
	d.countFence(prepared, recorded)
	_, err = d.apply("dispatch", "dispatch-"+strconv.Itoa(event.order), prepared)
	return err
}

// answers says whether fact is a person's answer or review, which moves a root's wait on a person
// (assumption 5).
func answers(fact intake.Fact) bool {
	switch fact.(type) {
	case intake.DispatchArtifact, intake.DispatchAskClosed, intake.DispatchAskReply:
		return true
	}
	return false
}

// boot is a daemon process starting at the record's moment of it: a fresh engine and admission
// pass, the reconcile of the shadow's listing with the holds the record's lines show at that boot
// (the listing's summaries held for the Dispatch consumer, and the roots the boot's catch-up
// holds), the release of each root's catch-up where the record shows it applied, DaemonBooted,
// and then every act that fell while the daemon was down, in order (assumption 24). Once it
// serves, its read of the pull requests (bootRead) is where a conflict a stretch under way draws
// enters (bootConflicts, assumption 3).
func (d *driver) boot(boot Boot) error {
	d.boots++
	d.used[assumeBootRepairsNothing] = true
	d.bootDropsTimer()
	d.newChain(boot.At)
	if err := d.serialized(func(ctx context.Context, tx pgx.Tx) error {
		_, err := d.engine.OpenUncountedRuns(ctx, tx)
		return err
	}); err != nil {
		return fmt.Errorf("replay: open the uncounted runs of the boot at %s: %w", stamp(boot.At), err)
	}
	listing := d.shadow.listing()
	recorded := map[string]*record.Issue{}
	if err := store.TransactWith(d.ctx, d.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		issues, err := d.records.Issues(ctx, tx)
		for i := range issues {
			recorded[issues[i].Key] = &issues[i]
		}
		return err
	}); err != nil {
		return err
	}
	listed := make([]intake.DispatchIssue, 0, len(listing))
	for _, summary := range listing {
		fact := intake.ListedIssue(summary)
		listed = append(listed, d.prepareListed(fact, recorded[fact.Key]))
	}
	// A boot whose listing the record shows held for the Dispatch consumer reads the consumer
	// short of the stream's position, so the summaries ahead of the record wait for the release
	// the record shows; every other boot finds the consumer caught up.
	position, target := intake.DispatchConsumerPosition{Idle: true}, int64(1)
	if !boot.Holding.IsZero() {
		position = intake.DispatchConsumerPosition{}
	}
	var catchingUp []record.Issue
	if err := d.serialized(func(ctx context.Context, tx pgx.Tx) error {
		if _, _, err := d.records.TakeDispatchRefill(ctx, tx, d.window.Project); err != nil {
			return err
		}
		if err := blockers.Reconcile(ctx, tx, d.records, d.window.Project, listing); err != nil {
			return err
		}
		var err error
		if catchingUp, err = d.catchesUp(ctx, tx); err != nil {
			return err
		}
		keys := make([]string, len(catchingUp))
		for i, root := range catchingUp {
			keys[i] = root.Key
		}
		d.admission.HoldForCatchUp(keys...)
		// A summary the listing gives older than a fact already applied to its issue is refused and
		// named for the daemon's next recovery pass (admit's Reconcile); the replay's listing is its
		// copy of Dispatch at this instant, which every applied fact has reached, so it names none.
		_, err = d.admission.Reconcile(ctx, tx, listed, target, position)
		return err
	}); err != nil {
		return fmt.Errorf("replay: reconcile the boot at %s: %w", stamp(boot.At), err)
	}
	if err := d.observe(); err != nil {
		return err
	}
	bootNumber := d.boots
	if !boot.Holding.IsZero() && !boot.CaughtUp.IsZero() && boot.CaughtUp.Before(d.window.End()) {
		d.push(&item{at: boot.CaughtUp, class: classAct, rec: boot.CaughtUp, order: lineOrder, run: func() error {
			_, err := d.apply("dispatch", d.factID("dispatch-position", d.window.Project, bootNumber),
				intake.DispatchConsumerPosition{AckFloorStream: target, Idle: true})
			return err
		}})
	}
	// Each root the catch-up holds is released where the record shows its catch-up applied, and
	// the boot's DaemonBooted comes after the last of them, as the daemon serves only once its
	// catch-up is done.
	last := boot.At
	for _, root := range catchingUp {
		at := d.caughtUpAt(boot, root.Key)
		last = maxTime(at, last)
		d.push(&item{at: at, class: classAct, rec: at, order: lineOrder - 1, run: func() error { return d.releaseCatchUp(root) }})
	}
	d.push(&item{at: last, class: classAct, rec: last, order: lineOrder, run: func() error {
		if _, err := d.apply("daemon", fmt.Sprintf("boot:%s:%d", d.window.Project, bootNumber), intake.DaemonBooted{Boot: int64(bootNumber)}); err != nil {
			return err
		}
		held := d.held
		d.held = nil
		for _, it := range held {
			if err := it.run(); err != nil {
				return err
			}
			if err := d.drain(); err != nil {
				return err
			}
		}
		return nil
	}})
	if read := maxTime(maxTime(bootRead(boot), last), d.clock.Instant()); read.Before(d.window.End()) &&
		(d.cfg.Branch.asksGit() || d.cfg.Branch.Conflicts == conflictsDrawn && d.conflicts.canDrawConflict()) {
		d.push(&item{at: read, class: classAct, rec: read, order: lineOrder + 1, run: d.bootConflicts})
	}
	return nil
}

// servesFrom is the daemon process that runs at the window's start, the newest begun before it,
// and the moment it begins to serve, where that comes inside the window: its reconcile, the line of
// which the log holds where the reconcile held summaries for the Dispatch consumer (Boot.Holding),
// and otherwise the first slot line of the process before its `legion daemon started` line, the
// pass of its reconcile. False where no process began before the start, or the log shows its
// reconcile before the start or not at all. The daemon applies no fact before its reconcile, so a
// replay's first pass waits for it, and no act before it runs (run), as none runs while no daemon
// does.
func (d *driver) servesFrom() (Boot, time.Time, bool) {
	t, w := d.trace, d.window
	var boot *Boot
	for i := range t.Boots {
		if t.Boots[i].At.Before(w.From) {
			boot = &t.Boots[i]
		}
	}
	if boot == nil {
		return Boot{}, time.Time{}, false
	}
	serves := boot.Holding
	if serves.IsZero() && !boot.Started.IsZero() {
		for _, move := range t.Slots {
			if !move.At.Before(boot.At) && move.At.Before(boot.Started) {
				serves = move.At
				break
			}
		}
	}
	return *boot, serves, serves.After(w.From) && serves.Before(w.End())
}

// serialized runs write in one transaction that takes the lock every fact applies under, with
// the run settling a fact has (intake.OpenSettlers), as the daemon's boot runs its reconcile and
// its uncounted runs outside ApplyFact; the hooks write registers run once it commits
// (store.Transact, store.OnCommit).
func (d *driver) serialized(write func(ctx context.Context, tx pgx.Tx) error) error {
	return store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		ctx, settle := intake.OpenSettlers(ctx, d.handlers...)
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return err
		}
		if err := write(ctx, tx); err != nil {
			return err
		}
		return settle(ctx, tx)
	})
}

// catchesUp is every root a boot's catch-up reads, and so holds out of the pass's decision whether
// it waits on a person until its catch-up has applied what Dispatch recorded while the daemon was
// down, by the daemon's own reading of each record (record.CatchesUp), over its design gate under
// the window's steps and whether it holds a slot.
func (d *driver) catchesUp(ctx context.Context, tx pgx.Tx) ([]record.Issue, error) {
	issues, err := d.records.Issues(ctx, tx)
	if err != nil {
		return nil, err
	}
	gates, err := record.ReadTreeGates(ctx, tx, d.records, issues, d.window.GateSteps)
	if err != nil {
		return nil, err
	}
	slots, err := d.records.Slots(ctx, tx)
	if err != nil {
		return nil, err
	}
	slotted := make(map[string]bool, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = true
	}
	var roots []record.Issue
	for _, issue := range issues {
		if record.CatchesUp(issue, gates[issue.Key], slotted[issue.Key]) {
			roots = append(roots, issue)
		}
	}
	return roots, nil
}

// caughtUpAt is when the record shows the catch-up of root at boot applied: the first gate line of
// the root from the boot on, which is the catch-up's own act on what the downtime left it, or the
// daemon's start of service, whichever is first; the boot itself where the record shows neither.
func (d *driver) caughtUpAt(boot Boot, root string) time.Time {
	next := d.window.End()
	for _, other := range d.trace.Boots {
		if other.At.After(boot.At) && other.At.Before(next) {
			next = other.At
		}
	}
	at := time.Time{}
	if iss, ok := d.trace.Issues[root]; ok {
		for _, gate := range iss.Gates {
			if (gate.Kind == GateOpenedFact || gate.Kind == GateClosedFact) && !gate.At.Before(boot.At) && gate.At.Before(next) {
				at = gate.At
				break
			}
		}
	}
	if !boot.Started.IsZero() && (at.IsZero() || boot.Started.Before(at)) {
		at = boot.Started
	}
	if at.IsZero() {
		at = boot.At
	}
	return at
}

// releaseCatchUp ends the catch-up hold of root: the ask, comment and artifact events of it the
// downtime left, which the daemon's catch-up reads (its catchUpWindow), applied first, then the
// release and CatchUpApplied.
func (d *driver) releaseCatchUp(root record.Issue) error {
	var rest []*item
	for _, it := range d.held {
		if it.eventKey == root.Key && caughtUpKind(it.eventKind) {
			if err := it.run(); err != nil {
				return err
			}
			continue
		}
		rest = append(rest, it)
	}
	d.held = rest
	d.admission.ReleaseCatchUp(root.Key)
	through := int64(0)
	if iss, ok := d.shadow.issues[root.Key]; ok {
		through = iss.seq
	}
	_, err := d.apply("daemon", fmt.Sprintf("awaiting-catch-up:caught-up:%s:%d:%d:%d", root.Key, root.Generation, through, d.boots),
		intake.CatchUpApplied{Issue: root.Key, Through: through})
	return err
}

// caughtUpKind says whether a Dispatch event of kind is one a boot's catch-up of a root reads:
// an ask's, a comment's or a document's, the events the boot's listing carries nothing of.
func caughtUpKind(kind string) bool {
	return strings.HasPrefix(kind, "ask.") || strings.HasPrefix(kind, "comment.") || strings.HasPrefix(kind, "artifact.")
}

// tick is the controller's tick at at: Observed read from the shadow by the keys the daemon's own
// selection reads (blockers.WaitedOn, blockers.ReadPlan), as the daemon's tick reads Dispatch.
func (d *driver) tick(at time.Time) error {
	var keys []string
	if err := store.TransactWith(d.ctx, d.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		keys, err = blockers.WaitedOn(ctx, tx, d.records, d.window.Project, d.window.GateSteps, d.rule, d.now())
		return err
	}); err != nil {
		return fmt.Errorf("replay: read what the tick at %s re-reads: %w", stamp(at), err)
	}
	var observed []intake.IssueStatus
	if len(keys) > 0 {
		listProject, single := blockers.ReadPlan(d.window.Project, keys)
		if listProject {
			for _, summary := range d.shadow.listing() {
				observed = append(observed, blockers.Observed(d.window.Project, summary))
			}
		}
		for _, key := range single {
			if iss, ok := d.shadow.issues[key]; ok && iss.payload != nil && iss.project != "" {
				observed = append(observed, blockers.Observed(iss.project, d.shadow.summary(iss)))
			}
		}
	}
	_, err := d.apply("controller", fmt.Sprintf("controller-tick:%s:%d:%d", d.window.Project, d.boots, at.UnixNano()),
		intake.ControllerTick{Observed: observed})
	return err
}

// pause is the project's pause the record shows beginning: the replay's own, through the store's
// pause, which holds every promotion and the outbox rows a pause holds, and stops every live claim
// with a pause_stop row (internal/pause's StopRow), which the stub finishes (assumption 28).
func (d *driver) pause(span PauseSpan) error {
	reason := cmp.Or(span.Reason, "the record's pause")
	paused, _, err := d.st.Pause(d.ctx, store.PauseRequest{Project: d.window.Project, By: store.PauseActor{Kind: store.PauseByOperator},
		Reason: reason, At: d.now(), StopRow: pause.StopRow})
	if err != nil {
		return fmt.Errorf("replay: pause the project at %s: %w", stamp(span.From), err)
	}
	d.paused, d.pauseID = true, paused.ID
	d.log.Info(pauseBeganLine, "pause", paused.ID, "by", string(store.PauseByOperator), "reason", reason)
	return nil
}

// resume ends the replay's pause where the record shows the pause ended, and applies the resume's
// own pass (intake.ProjectResumed).
func (d *driver) resume(span PauseSpan) error {
	if !d.paused {
		return nil
	}
	at := d.now()
	if _, err := d.st.Resume(d.ctx, store.ResumeRequest{Project: d.window.Project, By: store.PauseActor{Kind: store.PauseByOperator},
		At: at, MarkHeld: func(ctx context.Context, tx pgx.Tx, held store.Pause) error {
			_, err := d.records.MarkPauseHeldRows(ctx, tx, d.window.Project, held.ID, d.now())
			return err
		}}); err != nil {
		return fmt.Errorf("replay: resume the project at %s: %w", stamp(span.To), err)
	}
	d.paused = false
	d.log.Info(pauseEndedLine, "pause", d.pauseID, "by", string(store.PauseByOperator))
	_, err := d.apply("pause", fmt.Sprintf("project-resumed:%s:%d", d.window.Project, d.pauseID), intake.ProjectResumed{})
	return err
}
