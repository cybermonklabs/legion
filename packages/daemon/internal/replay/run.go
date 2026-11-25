// Package replay replays a recorded window of a Legion daemon's project (Window) through the
// daemon's own handler chain (admit.Handlers: the Dispatch mirror, the workflow engine and the
// admission pass) under a named slot rule (record.SlotRule), on a simulated clock and in a
// database of its own, and measures the record and each replay with one code (Scorecard). It is
// what `legion replay` runs: a rule is judged by the daemon's own decisions over a window the
// daemon lived through, not by a copy of its rules.
package replay

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// RunConfig is one replay of a window: the window, the rule and branch it runs under, and where
// it runs and writes.
type RunConfig struct {
	// Window is the recorded window (ReadWindow).
	Window Window
	// Rule is the slot rule the replay's admission pass and engine run (record.SlotRuleNamed).
	Rule record.SlotRule
	// Branch is the value of each knob the record does not give.
	Branch Branch
	// Seed seeds every draw the replay makes: the experiments' and the conflicts'.
	Seed int64
	// Slowdown is the factor f by which a work segment's recorded length is scaled for each 42
	// issues in flight above the record's; 1.0 keeps the recorded lengths.
	Slowdown float64
	// PGDSN is a Postgres server the replay may create a database on. The replay writes only the
	// database it makes there, never the one the DSN names.
	PGDSN string
	// Out is the directory the replay writes to: log.jsonl, the daemon's lines stamped with the
	// simulated time; scorecard.json; run.json; and segments.jsonl under TraceSegments.
	Out string
	// KeepDB keeps the replay's database after the run, where it is otherwise dropped.
	KeepDB bool
	// TraceSegments writes each work segment's n, m and length (Segment) to Out and to the result.
	TraceSegments bool
	// BuildCommit is the commit of the legion binary that runs the replay, which run.json names
	// (runRecord.BuildCommit); unknownBuild where it is empty.
	BuildCommit string

	// afterFact, where set, runs after each fact the replay applies, with the replay's pool and
	// its copy of Dispatch, between facts: a test's look at the replay's state as each pass leaves
	// it (TestTheShadowMatchesTheMirrorAfterEveryFact).
	afterFact func(ctx context.Context, pool *store.Pool, shadow *shadow) error
	// atStart, where set, runs once, before the replay's first fact, with its copy of Dispatch
	// brought to the record's state at the window's start: a test's look at the copy the window
	// starts from (TestAPlacedIssuesDispatchWritesApplyAfterAMidwayStart).
	atStart func(shadow *shadow)
	// atSettled, where set, runs at each instant the snapshot timeline takes a state at, once
	// every act of the instant has run, with the replay's store and the instant: a test's look at
	// the replay's database at the moments its timeline reads (TestReplayStatesAreTheProjectionsStates).
	atSettled func(ctx context.Context, st *store.Store, at time.Time) error
	// inputs, where set, answers what every run of the window reads of it alike (windowInputs),
	// read once for all of a window's runs (runWindow); Run reads its own where it is unset.
	inputs func() (*windowInputs, error)
	// onDraw, where set, is told of each draw of a stretch's conflict as the run makes it: a test's
	// look at the draws two runs at one seed share (TestTwoRulesAtOneSeedShareEachStretchsDraw).
	onDraw func(conflictDraw)
}

// windowInputs is what every run of one window reads of it alike, read once for all of them
// (runWindow): the window's trace (BuildTrace); the lines of the record a run's script reads
// beside it (recordLines) and the window's Dispatch events, both in the order the trace read them
// (readTrace); the commits of the base branches a stretch's M is counted from and the conflict
// model fitted on them (newBaseMerges, windowConflicts); the record's ranks (ranksOf); and the
// record's lines and events from before the window's start, which each run's line timeline reads
// (recordBefore), read the first time a run asks for them. No run writes into any of it.
type windowInputs struct {
	trace     Trace
	lines     recordLines
	events    []readEvent
	merges    baseMerges
	conflicts ConflictModel
	ranks     rankFunc
	before    func() (recordBefore, error)
}

// newWindowInputs reads w's inputs (windowInputs).
func newWindowInputs(w Window) (*windowInputs, error) {
	trace, lines, events, err := readTrace(w, nil)
	if err != nil {
		return nil, err
	}
	merges := newBaseMerges(w, trace)
	return &windowInputs{trace: trace, lines: recordLinesOf(lines), events: events, merges: merges,
		conflicts: windowConflicts(trace, merges), ranks: ranksOf(trace),
		before: sync.OnceValues(func() (recordBefore, error) { return readRecordBefore(w) })}, nil
}

// readInputs is the window's inputs cfg runs on: the ones its window's runs share where it names
// them, and otherwise its window's own, read now.
func (cfg RunConfig) readInputs() (*windowInputs, error) {
	if cfg.inputs != nil {
		return cfg.inputs()
	}
	return newWindowInputs(cfg.Window)
}

// recordBefore is the record's lines and events from before a window's start, which the start
// state was built from and which a run's line timeline holds beside the run's own (lineTimeline).
type recordBefore struct {
	log    []json.RawMessage
	events map[string][]json.RawMessage
}

// readRecordBefore reads w's lines and events from before its start (recordBefore).
func readRecordBefore(w Window) (recordBefore, error) {
	before := recordBefore{events: make(map[string][]json.RawMessage, len(w.Events))}
	for i, raw := range w.Log {
		var head struct {
			Time time.Time `json:"time"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return recordBefore{}, fmt.Errorf("replay: line %d of the window's log has no time: %w", i+1, err)
		}
		if head.Time.Before(w.From) {
			before.log = append(before.log, raw)
		}
	}
	for key, events := range w.Events {
		for i, raw := range events {
			event, err := ParseDispatchEvent(raw)
			if err != nil {
				return recordBefore{}, fmt.Errorf("replay: event %d of %s: %w", i+1, key, err)
			}
			if event.At.Before(w.From) {
				before.events[key] = append(before.events[key], raw)
			}
		}
	}
	return before, nil
}

// RunResult is what one replay made and measured.
type RunResult struct {
	// Database is the name of the database the replay made on RunConfig.PGDSN's server
	// (legion_replay_<run id>), which is dropped at the end of the run unless RunConfig.KeepDB
	// kept it.
	Database string
	// Scorecard is the replay's measures, read from its snapshot timeline (design 10.1 (a)): the
	// states the daemon's own admission passes decided from, as each pass's observer handed them
	// over, and the claims the worker stub holds. The run fills what it names of itself (its
	// source, rule, branch, seed, slowdown, span, unplaced count, the assumptions it used and its
	// scenario); the measures are Measure's.
	Scorecard Scorecard
	// LineScorecard is the same measures read from the replay's line timeline (design 10.1 (b)):
	// BuildTrace over the replay's own log and its own copy of Dispatch's events, read through the
	// window's line kinds as a record's scorecard is (Score), with the same run's own names.
	LineScorecard Scorecard
	// Gaps is each measure of the two scorecards side by side (Gaps): the error of reading a
	// course from its lines, which a record's scorecard rests on.
	Gaps []MeasureGap
	// Segments is each work segment the replay ran, under RunConfig.TraceSegments.
	Segments []Segment
	// Unplaced is each issue the start state could not place at the window's start.
	Unplaced []Unplaced
	// Divergence is what the replay counted of where it left the record's course: refused moves
	// with their reasons, the recorded conflicts it kept for a loop open at the window's end,
	// reassertion starts, events a sequence fence dropped, the outbox rows finished under each
	// assumption, the recorded turns it wrote no claim of, and what its start state counted of the
	// slots the trace inferred and of the record's own count (design 10.9).
	Divergence Divergence
	// Assumptions is every assumption the run rests on, by number: the spec's and the plan's,
	// which the scorecard names too, and the tool's own (26 on), under which it left a fact or a
	// turn out, finished an outbox row without acting, seeded a slot the trace inferred or kept a
	// recorded conflict, which run.json names beside them.
	Assumptions []int
	// Conflicts is the conflict model the run drew each stretch's conflict by (spec R2): fitted on
	// the recorded stretches inside the window (windowConflicts) with each coefficient's interval,
	// its merges term set to zero where the branch's Merges is zero, and how many stretches, of the
	// window's and of the replay's, counted a merge in M that may be their own.
	Conflicts ConflictModel

	// snapshot is the snapshot timeline Scorecard is read from, which a test reads states from.
	snapshot Timeline
}

// Segment is one work segment of an issue in a replay: the phase it worked, when it started, the
// issues in flight at its start in the replay (N) and in the record (M), its recorded length and
// the length the replay gave it, Recorded × f^((N − M)/42).
type Segment struct {
	Issue    string      `json:"issue"`
	Phase    phase.Phase `json:"phase"`
	Start    time.Time   `json:"start"`
	N        int         `json:"n"`
	M        int         `json:"m"`
	Recorded Duration    `json:"recorded"`
	Length   Duration    `json:"length"`
}

// Unplaced is an issue the replay's start state could not place at the window's start, and why.
type Unplaced struct {
	Issue  string `json:"issue"`
	Reason string `json:"reason"`
}

// The files a run writes into RunConfig.Out beside its log (logFile): its scorecard, read from its
// snapshot timeline, the same read from its line timeline, what it ran and counted, and its
// segments under TraceSegments.
const (
	scorecardFile     = "scorecard.json"
	lineScorecardFile = "scorecard-line.json"
	runFile           = "run.json"
	segmentsFile      = "segments.jsonl"
)

// specAssumptionsTop is the highest number of an assumption of the spec and the plan (1 to 25),
// the ones a scorecard names; the tool's own after them (26 on) are named in run.json alone.
const specAssumptionsTop = 25

// unknownBuild is the build commit a run names where nothing names the commit of the legion binary
// that ran it: a build that linked no commit in and recorded none from a checkout, or a run.json
// written before runs named one.
const unknownBuild = "unknown"

// Run replays cfg.Window under cfg.Rule and cfg.Branch. It makes a database of its own on
// cfg.PGDSN's server, migrated with the daemon's migrations; applies every fact of the window —
// the project's Dispatch events, the recorded phase moves, the ticks and the boots — each in one
// pass of the daemon's handler chain (intake.ApplyFact over admit.Handlers), with the rule set on
// the engine and the admission pass and one simulated clock set on the engine, the admission
// pass and the store; runs a worker stub over the outbox rows the daemon's lanes would take after
// each fact; and measures the replay twice (design 10.1): from the states its admission passes
// left and its stub's claims (the snapshot timeline, RunResult.Scorecard), and from the log and
// the Dispatch events it wrote, read back as a record is read (the line timeline,
// RunResult.LineScorecard), with the gap between the two per measure. It makes no request
// outside the database: no Dispatch, no GitHub and no NATS.
//
// The branch's knobs (design section 7) shape the record's work before the replay runs it: the
// anchors of the events that end a wait and of a person's answers (Timing, Answer), the length of
// the work after a send-back (AfterSendBack), the ranks the replay's copy of Dispatch shows
// (FirstStarts, design 10.7), the work of an issue the record never started (NeverStarted), and
// the conflict model each stretch draws by (Merges), fitted on the recorded stretches inside the
// window.
func Run(ctx context.Context, cfg RunConfig) (RunResult, error) {
	began := time.Now()
	w := cfg.Window
	if cfg.Branch == (Branch{}) {
		cfg.Branch = MainBranch
	}
	if err := cfg.Branch.check(); err != nil {
		return RunResult{}, err
	}
	cfg.Branch = cfg.Branch.orMain()
	if cfg.Slowdown <= 0 {
		return RunResult{}, fmt.Errorf("replay: the slowdown factor is %v; it must be above zero (1 keeps the recorded lengths)", cfg.Slowdown)
	}
	if cfg.Out == "" {
		return RunResult{}, errors.New("replay: no directory to write the run's output to")
	}
	in, err := cfg.readInputs()
	if err != nil {
		return RunResult{}, err
	}
	trace := in.trace
	script, err := buildScript(trace, in.events, in.lines)
	if err != nil {
		return RunResult{}, err
	}
	if cfg.Branch.AfterSendBack == lessSlotWait {
		script.cutSlotWaits()
	}
	var drawn DrawnWork
	if cfg.Branch.NeverStarted != MainBranch.NeverStarted {
		drawn = script.drawNeverStarted(trace, cfg.Branch.NeverStarted, rand.New(rand.NewPCG(uint64(cfg.Seed), neverStartedStream)))
	}
	var starts *firstStarts
	if cfg.Branch.FirstStarts != firstStartsRank {
		starts = firstStartRanks(trace)
	}
	conflicts := in.conflicts
	if cfg.Branch.Merges == mergesZero {
		conflicts = conflicts.WithoutTerm()
	}
	token, err := claim.ProjectToken(w.Project)
	if err != nil {
		return RunResult{}, fmt.Errorf("replay: the claim token of %s: %w", w.Project, err)
	}
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return RunResult{}, fmt.Errorf("replay: make the run's output directory: %w", err)
	}
	logged, err := os.Create(filepath.Join(cfg.Out, logFile))
	if err != nil {
		return RunResult{}, fmt.Errorf("replay: open the run's log: %w", err)
	}
	defer func() { _ = logged.Close() }()
	written := bufio.NewWriter(logged)

	db, st, err := openReplayDatabase(ctx, cfg.PGDSN)
	if err != nil {
		return RunResult{}, err
	}
	result := RunResult{Database: db.name, Conflicts: conflicts}
	closeDB := func() error {
		st.Close()
		if cfg.KeepDB {
			return nil
		}
		return db.drop(ctx)
	}

	clock := newSimClock(w.From)
	st.UseClock(clock.Now)
	tap := newLineTap(tapped...)
	d := &driver{
		ctx: ctx, cfg: cfg, window: w, trace: trace, inputs: in, script: script, lines: in.lines, rule: cfg.Rule.OrMain(),
		clock: clock, st: st, pool: st.Pool(), records: record.NewStoreWithClock(clock.Now), tap: tap,
		log:    slog.New(&clockHandler{inner: slog.NewJSONHandler(written, &slog.HandlerOptions{Level: slog.LevelInfo}), clock: clock, tap: tap}),
		shadow: newShadow(w.Project, starts.ranks()), firstStarts: starts, token: token,
		claims:   map[claim.Token]supervise.Claim{},
		inFlight: map[string]bool{}, started: map[string]time.Time{}, childWait: map[string]time.Time{}, placed: map[int]time.Time{},
		signOffs: map[string]bool{}, drops: map[string]string{}, used: map[int]bool{1: true, assumeRootRanksByNextStart: starts != nil},
		snap:     newSnapshots(w.From, w.To, w.End(), in.ranks),
		counters: Divergence{Refused: []RefusedMove{}, Finished: map[int]int{}, TurnsLeftOut: map[int]int{}, DrawnWork: drawn},
	}
	d.conflicts, d.merges, d.loops, d.stretchFrom = conflicts, in.merges, recordedLoops(script), map[string]time.Time{}
	d.waiting, d.reopens, d.starting, d.stretchDraws, d.lastDraw = map[string]waitingConflict{}, map[string][]int{}, map[string][]int{},
		map[stretchDraw]int{}, map[string]stretchDraw{}
	d.noWork, d.drawSource = map[string]bool{}, rand.NewPCG(0, 0)
	d.draws, d.numbers = rand.New(d.drawSource), newPullRequestNumbers(trace)
	runErr := d.run()
	flushErr := written.Flush()
	if err := errors.Join(runErr, flushErr); err != nil {
		return result, errors.Join(err, closeDB())
	}
	if err := closeDB(); err != nil {
		return result, err
	}
	d.traceAssumptions()
	// What the replay's own passes judged names two more: 4 where an issue waited on another issue
	// (a gate's closing names it as the replay writes it, observe), and 7 where a hold other than
	// the cap held the first start of an issue not yet started.
	if d.snap.waitsOnIssues() {
		d.used[4] = true
	}
	if d.snap.firstStartHeld {
		d.used[7] = true
	}
	result.Unplaced, result.Assumptions = d.unplaced, d.assumptionsUsed(len(Assumptions))
	result.Conflicts = d.conflicts
	if cfg.TraceSegments {
		result.Segments = d.segments
	}
	result.snapshot = d.snap.timelineAt()
	// An issue that waits for a slot at the start began that wait where the record shows, its start
	// state being the record's.
	result.snapshot.beginWaitsAt(recordedWaitStarts(trace, in.ranks, w.From, result.snapshot.startWaiters()))
	// Each issue's wait as urgent work begins where the record's issue events show, as its ranks
	// and priorities are the record's: a person's todo and a person's priority are kept as recorded.
	result.snapshot.readUrgency(trace)
	d.counters.NoWorkAdmissions, d.counters.NoWorkSlotHours = len(d.noWork), result.snapshot.heldHours(d.noWork)
	result.Divergence = d.counters
	result.Scorecard = d.scorecard(Measure(result.snapshot))
	lined, linedTrace, err := d.lineTimeline()
	if err != nil {
		return result, err
	}
	result.LineScorecard = d.scorecard(Measure(lined))
	traceMeasures(&result.LineScorecard, linedTrace)
	result.Gaps = Gaps(result.Scorecard, result.LineScorecard)
	if err := d.writeOutputs(result, began); err != nil {
		return result, err
	}
	return result, nil
}

// Counts is what the run counted of where it left the record's course, as `legion replay run`
// prints it on one line beside the run's name: the moves refused, the conflicts kept, the recorded
// conflict loops passed over and the conflicts drawn where the record shows none, the conflicts
// left to the merger, the starts that opened no recorded work, the events fenced, the issues the
// start state could not place, the issues admitted with no work and the slot-hours they hold, and,
// under a drawn scenario, how many issues drew their work from each pool (DrawnWork).
func (r RunResult) Counts() string {
	c := r.Divergence
	counts := fmt.Sprintf("%d moves refused, %d recorded conflicts kept for a loop open at the window's end, "+
		"%d recorded conflict loops passed over, %d conflicts drawn where the record shows none, %d conflicts past the review left to the merger, "+
		"%d reassertion starts, %d events fenced, %d starts with no segment left, %d segments passed over, %d unplaced issues, "+
		"%d issues admitted with no work holding %s slot-hours",
		len(c.Refused), c.KeptOpenLoops, c.SkippedLoops, c.AddedLoops, c.ConflictsToMerger, c.Reassertions, c.Fenced, c.NoSegment,
		c.SkippedSegments, len(r.Unplaced), c.NoWorkAdmissions, formatNumber(c.NoWorkSlotHours))
	if drawn := c.DrawnWork; drawn != (DrawnWork{}) {
		counts += fmt.Sprintf(", the drawn work of %d issues: %d from its own tree, %d of its priority and repository, %d from the window",
			drawn.Tree+drawn.PriorityRepository+drawn.Window, drawn.Tree, drawn.PriorityRepository, drawn.Window)
	}
	return counts
}

// traceAssumptions names the assumptions the run rests on that the window's own record shows
// rather than a fact the replay applied: 8 where it holds an issue handed to Legion that the
// record never started, 15 where work is left in flight at its end, 16 where an issue reopens, 17
// where a child is filed after its parent started, and 18 where it records a conflict.
func (d *driver) traceAssumptions() {
	w := d.window
	within := func(at time.Time) bool { return !at.Before(w.From) && at.Before(w.To) }
	if len(d.inFlight) > 0 {
		d.used[15] = true
	}
	for _, key := range slices.Sorted(maps.Keys(d.trace.Issues)) {
		iss := d.trace.Issues[key]
		if iss.Project != d.trace.Project {
			continue
		}
		for _, entry := range iss.Entries {
			if entry.At.Before(w.To) && d.neverStarted(key) {
				d.used[8] = true
			}
			if entry.Reopen && within(entry.At) {
				d.used[16] = true
			}
		}
		for _, filing := range iss.Children {
			if !filing.Moved && within(filing.At) && startedBy(iss, filing.At) {
				d.used[17] = true
			}
		}
		for _, visit := range iss.Visits {
			if visit.Out != nil && visit.Out.conflict() && within(visit.Out.At) {
				d.used[18] = true
			}
		}
	}
}

// startedBy says whether iss had started by at: it held a slot or a worker of it had started.
func startedBy(iss *IssueTrace, at time.Time) bool {
	return slices.ContainsFunc(iss.Slots, func(span SlotSpan) bool { return span.Start.Before(at) }) ||
		slices.ContainsFunc(iss.Visits, func(visit Visit) bool { return !visit.WorkerStart.IsZero() && visit.WorkerStart.Before(at) })
}

// assumptionsUsed is the numbers of the assumptions the run used, in order, up to top.
func (d *driver) assumptionsUsed(top int) []int {
	used := []int{}
	for _, number := range slices.Sorted(maps.Keys(d.used)) {
		if d.used[number] && number <= top {
			used = append(used, number)
		}
	}
	return used
}

// scorecard is card, measured from one of the replay's timelines, with what the run itself names:
// its source, rule, branch, seed, slowdown and span, the issues its start state could not place,
// the assumptions of the spec and the plan its measures rest on, and its never-started scenario.
func (d *driver) scorecard(card Scorecard) Scorecard {
	card.Source, card.Rule, card.Branch, card.Seed, card.Slowdown = "replay", d.rule.Name, d.cfg.Branch.Name, d.cfg.Seed, d.cfg.Slowdown
	card.From, card.To, card.Unplaced = d.window.From, d.window.To, len(d.unplaced)
	card.Assumptions = d.assumptionsUsed(specAssumptionsTop)
	card.Scenario = d.cfg.Branch.NeverStarted
	return card
}

// laterLines is the line kinds a later release writes where an older one wrote other lines that
// BuildTrace reads the same facts from, the trace reading each where the window holds its kind and
// the older lines where it does not: the record line of a pass that moves slots and the hold's
// beginning (passMovedSlotsLine, phaseHeldLine).
var laterLines = []string{passMovedSlotsLine, phaseHeldLine}

// lineTimeline is the replay's line timeline (design 10.1 (b)): BuildTrace over a window that holds
// the record's lines and events from before the window's start, which the replay's start state
// was built from, and from the start on the replay's own log and the events its copy of Dispatch
// applied, recorded and its own alike: each issue's log past the recorded events that brought the
// copy to the window's start (shadowIssue.seeded), which the record's own events before the start
// stand for here. Its log from the start on is read as written under the rule the replay ran
// (buildTrace), and it is measured by the record's true ranks (design 10.7) under that rule.
//
// The replay's log is read through the window's line kinds as the record is, so a later kind the
// replay's daemon writes in place of the older lines a window holds (laterLines) is left out, and
// the record and the replay are read by one path. Every other kind the trace reads is read where the
// replay wrote it: a window lacks such a kind only because nothing it holds wrote one (a window in
// which no slot went back holds no `admission: slot released`), and the replay's line is of what the
// replay did. It answers the trace beside the timeline, which the measures read from a trace alone
// are read from (traceMeasures).
func (d *driver) lineTimeline() (Timeline, Trace, error) {
	w := d.window
	before, err := d.inputs.before()
	if err != nil {
		return Timeline{}, Trace{}, err
	}
	lined := w
	// Clipped, a run's own lines and events are appended to copies, never to the shared record's.
	lined.Log, lined.Events = slices.Clip(before.log), make(map[string][]json.RawMessage, len(w.Events)+len(d.shadow.issues))
	for key, events := range before.events {
		lined.Events[key] = slices.Clip(events)
	}
	lined.Lines = slices.Clone(w.Lines)
	listed := make(map[string]bool, len(w.Lines)+len(laterLines))
	for _, msg := range slices.Concat(w.Lines, laterLines) {
		listed[msg] = true
	}
	replayed, err := os.ReadFile(filepath.Join(d.cfg.Out, logFile))
	if err != nil {
		return Timeline{}, Trace{}, fmt.Errorf("replay: read the run's own log back: %w", err)
	}
	for i, line := range bytes.Split(replayed, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var head struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return Timeline{}, Trace{}, fmt.Errorf("replay: line %d of the run's own log is no JSON object: %w", i+1, err)
		}
		if TraceReads(head.Msg) && !listed[head.Msg] {
			lined.Lines = append(lined.Lines, head.Msg)
			listed[head.Msg] = true
		}
		lined.Log = append(lined.Log, json.RawMessage(line))
	}
	for key, iss := range d.shadow.issues {
		lined.Events[key] = append(lined.Events[key], iss.log[iss.seeded:]...)
	}
	rule := d.rule
	t, err := buildTrace(lined, &rule)
	if err != nil {
		return Timeline{}, Trace{}, fmt.Errorf("replay: read the run's own log and events back as a record: %w", err)
	}
	return lineTimeline(t, d.inputs.ranks, func(time.Time) record.SlotRule { return d.rule }), t, nil
}

// runRecord is a run's run.json: what it ran — the rule, the branch, the seed, the slowdown, the
// scenario, every assumption it used by number, and the window's run-out it ran past the window's
// end (After, none for a window with no run-out), its measures reading the window alone — what ran
// it — the commit of the legion binary (BuildCommit, where RuleCommit is the commit the rule was
// first written as a value at) and the moment on the wall clock the run started — and what it
// counted: the facts it applied and the passes they ran, the mean time of a pass, the run's wall
// time and peak memory, the issues the start state could not place, the segments it ran, its
// divergence counters, the gap of each measure between its snapshot and line timelines, and the
// conflict model it drew by. It holds numbers and names alone, never an assumption's text.
type runRecord struct {
	Rule            string        `json:"rule"`
	RuleCommit      string        `json:"ruleCommit"`
	BuildCommit     string        `json:"buildCommit"`
	Branch          Branch        `json:"branch"`
	Seed            int64         `json:"seed"`
	Slowdown        float64       `json:"slowdown"`
	Scenario        string        `json:"scenario"`
	Assumptions     []int         `json:"assumptions"`
	After           Duration      `json:"after"`
	Database        string        `json:"database"`
	KeptDatabase    bool          `json:"keptDatabase"`
	Facts           int           `json:"facts"`
	Passes          int           `json:"passes"`
	MeanPassMillis  float64       `json:"meanPassMillis"`
	StartedAt       time.Time     `json:"startedAt"`
	WallSeconds     float64       `json:"wallSeconds"`
	PeakMemoryBytes uint64        `json:"peakMemoryBytes"`
	Unplaced        int           `json:"unplaced"`
	Segments        int           `json:"segments"`
	Divergence      Divergence    `json:"divergence"`
	Gaps            []MeasureGap  `json:"gaps"`
	Conflicts       ConflictModel `json:"conflicts"`
}

// writeOutputs writes the run's scorecard.json (its snapshot timeline's), scorecard-line.json (its
// line timeline's) and run.json into its output directory, and its segments.jsonl under
// TraceSegments. began is the moment on the wall clock the run started, which run.json names in
// UTC beside the wall time since.
func (d *driver) writeOutputs(result RunResult, began time.Time) error {
	mean := 0.0
	if d.facts > 0 {
		mean = float64(d.passTime.Microseconds()) / 1000 / float64(d.facts)
	}
	ran := runRecord{Rule: d.rule.Name, RuleCommit: d.rule.Commit, BuildCommit: cmp.Or(d.cfg.BuildCommit, unknownBuild), Branch: d.cfg.Branch,
		Seed: d.cfg.Seed, Slowdown: d.cfg.Slowdown, Scenario: result.Scorecard.Scenario, Assumptions: result.Assumptions, After: d.window.After,
		Database: result.Database, KeptDatabase: d.cfg.KeepDB, Facts: d.facts, Passes: d.facts + d.boots, MeanPassMillis: mean, StartedAt: began.UTC(),
		WallSeconds: time.Since(began).Seconds(), PeakMemoryBytes: peakMemory(), Unplaced: len(result.Unplaced), Segments: len(d.segments),
		Divergence: result.Divergence, Gaps: result.Gaps, Conflicts: result.Conflicts}
	for name, value := range map[string]any{scorecardFile: result.Scorecard, lineScorecardFile: result.LineScorecard, runFile: ran} {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return fmt.Errorf("replay: write the run's %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(d.cfg.Out, name), append(data, '\n'), 0o644); err != nil {
			return fmt.Errorf("replay: write the run's %s: %w", name, err)
		}
	}
	if !d.cfg.TraceSegments {
		return nil
	}
	var segments bytes.Buffer
	encoder := json.NewEncoder(&segments)
	for _, segment := range d.segments {
		if err := encoder.Encode(segment); err != nil {
			return fmt.Errorf("replay: write the run's %s: %w", segmentsFile, err)
		}
	}
	if err := os.WriteFile(filepath.Join(d.cfg.Out, segmentsFile), segments.Bytes(), 0o644); err != nil {
		return fmt.Errorf("replay: write the run's %s: %w", segmentsFile, err)
	}
	return nil
}

// peakMemory is the most memory the process has held, in bytes: the kernel's high-water mark of
// its resident set where /proc shows it, and otherwise what the Go runtime has taken from the
// system.
func peakMemory() uint64 {
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for line := range strings.SplitSeq(string(status), "\n") {
			if rest, ok := strings.CutPrefix(line, "VmHWM:"); ok {
				if kib, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64); err == nil {
					return kib * 1024
				}
			}
		}
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.Sys
}

// withRanks is t with each issue of ranks shown at its rank there in every Dispatch event of it,
// the ranks the replay's copy of Dispatch shows at the window's start (firstStarts.ranks), so the
// start state records the ranks the replay's events carry; t itself where ranks is empty.
func withRanks(t Trace, ranks map[string]string) Trace {
	if len(ranks) == 0 {
		return t
	}
	shown := t
	shown.Issues = make(map[string]*IssueTrace, len(t.Issues))
	for key, iss := range t.Issues {
		rank, ranked := ranks[key]
		if !ranked {
			shown.Issues[key] = iss
			continue
		}
		copied := *iss
		copied.Dispatch = slices.Clone(iss.Dispatch)
		for i := range copied.Dispatch {
			if state := copied.Dispatch[i].Issue; state != nil {
				reranked := *state
				reranked.Rank = rank
				copied.Dispatch[i].Issue = &reranked
			}
		}
		shown.Issues[key] = &copied
	}
	return shown
}

// firstStartRanks is the input transform of the main branch's FirstStarts, recorded (design 10.7):
// each issue of the project that the record started in the window, by its first start there
// (firstStartIn), is shown at a rank that orders it by that first start, below every rank Dispatch
// gave any issue of the window. A first start is the first slot of a run that had taken none: a
// slot the issue held at the start from before it is none, nor is a later slot of a run that held
// one at the start or took one before it, a slot of an earlier run, a slot the record gave to work
// that came back (a return after a send-back or a conflict, a wait's end, a held phase's restart)
// or a slot the trace infers. The line orders the issues whose first phase can start by their tree
// root's rank and then their own (record.Waiting), so each tree's root is shown at the earliest
// recorded first start among the issues of its tree that can still take theirs in the replay
// (rootRank): the root itself, and each issue whose entry stood in the root's tree at its first
// start. The replay moves the root's rank each time one of them takes its first slot
// (firstStarts.take, driver.rerank), and each time the daemon's own test of the line's starting
// part answers otherwise of one of them than it did (firstStarts.bar, driver.reviewFirstStarts,
// record.FirstPhaseStartsNow), so a root in flight at the start ranks by its first child's start,
// then by its next child's, and a tree's next child takes the place its own recorded start gives it
// among every tree's work: the replay starts the issues in the order the record started them,
// across trees, where the rule lets it (assumption 36). An issue whose first phase the replay's
// daemon would not start now pins no tree to its rank: one the replay has not filed yet, or has
// finished, moved past its first phase or taken out of the workflow, from the window's start on
// (barAtStart), and one a hold of the starting part keeps back — its tree not running, its root
// waiting on a person, its tree's design gate closed, a blocker, its open children or an accepted
// ancestor — from the pass of the replay's daemon in which the hold begins, which orders its line
// by the ranks the holds it reads leave. So a tree whose next recorded start a hold keeps back
// ranks by its next start that can be made, and another tree's later recorded start that can be
// made goes ahead of it. A tree with no recorded start left that can be made shows its root at
// Dispatch's rank, behind every recorded start that can be made, as every issue the record did not
// start in the window is shown: a root that left the workflow before the window and was set back
// to todo waits at its own rank behind the issues the record started, as it did in the record,
// rather than at the head of the line with no recorded work left to open; and an issue the record
// never started waits behind its tree's recorded starts that can be made, and among other trees at
// its root's rank of the moment. A recorded change of a rank the replay shows changes nothing it
// shows (shadow.render), and the measures read the true ranks from the trace. Nil where no issue
// started in the window or no rank sorts below the window's lowest.
func firstStartRanks(t Trace) *firstStarts {
	type first struct {
		key, tree string
		at        time.Time
		rank      string
	}
	var started []first
	lowest, ranked := "", false
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		iss := t.Issues[key]
		rank := ""
		for _, event := range iss.Dispatch {
			if event.Issue != nil {
				rank = event.Issue.Rank
				if !ranked || rank < lowest {
					lowest, ranked = rank, true
				}
			}
		}
		if iss.Project != t.Project {
			continue
		}
		start, began := firstStartIn(iss, t.From)
		if !began {
			continue
		}
		tree := key
		if entry := iss.entryAt(start); entry != nil && entry.Tree != "" {
			tree = entry.Tree
		}
		started = append(started, first{key: key, tree: tree, at: start, rank: rank})
	}
	if len(started) == 0 || lowest == "" || lowest[0] <= ' '+1 {
		return nil
	}
	prefix := "0"
	if lowest[0] <= '0' {
		prefix = string(rune(lowest[0] - 1))
	}
	slices.SortStableFunc(started, func(x, y first) int {
		return cmp.Or(x.at.Compare(y.at), cmp.Compare(x.rank, y.rank), cmp.Compare(x.key, y.key))
	})
	// A tree's root ranks where it is an issue of the project; each issue counts in the tree it
	// started in, and a root the record started in another tree counts in its own tree as well.
	roots := map[string]bool{}
	for _, issue := range started {
		if root, known := t.Issues[issue.tree]; known && root.Project == t.Project {
			roots[issue.tree] = true
		}
	}
	width := len(strconv.Itoa(len(started)))
	f := &firstStarts{own: make(map[string]string, len(started)), trees: make(map[string][]string, len(roots)),
		tree: make(map[string]string, len(started)), taken: map[string]bool{}, barred: map[string]bool{}}
	for i, issue := range started {
		f.own[issue.key] = fmt.Sprintf("%s%0*d", prefix, width, i+1)
		if roots[issue.tree] {
			f.tree[issue.key] = issue.tree
			f.trees[issue.tree] = append(f.trees[issue.tree], issue.key)
		}
		if roots[issue.key] && issue.tree != issue.key {
			f.trees[issue.key] = append(f.trees[issue.key], issue.key)
		}
	}
	f.order = slices.Sorted(maps.Keys(f.own))
	f.barAtStart(t, t.From)
	return f
}

// firstStartIn is when iss took its first start in the window from from on: its first slot that
// begins there and is the first slot of its run, false where it took none. The run of a slot is the
// issue's entry at the slot's start, and a slot that had not ended by that entry is of the same
// run: one held at the window's start, one taken before it in that run, or one held where the log
// begins. A slot the trace infers (SlotSpan.Inferred), or held where the log begins
// (SlotSpan.Before), is no start, nor is a slot the record gave to work that came back to an issue
// the daemon had started (cameBack), whose earlier slot the inputs may not reach.
func firstStartIn(iss *IssueTrace, from time.Time) (time.Time, bool) {
	for i, span := range iss.Slots {
		if span.Before || span.Inferred || span.Start.Before(from) || cameBack(span.Kind) {
			continue
		}
		entry := iss.entryAt(span.Start)
		if !slices.ContainsFunc(iss.Slots[:i], func(earlier SlotSpan) bool {
			return entry == nil || earlier.End.IsZero() || earlier.End.After(entry.At)
		}) {
			return span.Start, true
		}
	}
	return time.Time{}, false
}

// cameBack says whether kind, the kind of entry of the waiting line a slot was given to
// (SlotSpan.Kind), is work that came back to an issue the daemon had started: a return of its work
// (record.LineReturning), the end of its wait on another issue (record.LineWaitingOnIssue), or a
// person's answer to a root (record.LineAnswered).
func cameBack(kind string) bool {
	switch kind {
	case record.LineReturning.String(), record.LineWaitingOnIssue.String(), record.LineAnswered.String():
		return true
	}
	return false
}

// firstStarts is the main branch's FirstStarts, recorded, over one window (firstStartRanks): the
// rank of each issue the record started in the window by its first start, the issues of each tree
// whose earliest rank still to come its root is shown at, which of them have taken their first slot
// in the replay, and which of the others cannot take it there now.
type firstStarts struct {
	// own is the rank of each issue the record started in the window, by its first start, and
	// order its issues in key order, the order the replay reads them in.
	own   map[string]string
	order []string
	// trees is each tree root's issues of own, in the order of their first starts: those that
	// stood in its tree at their first start, and the root itself where it started in another.
	trees map[string][]string
	// tree is the root of the tree each issue of own stood in at its first start, where that root
	// is one of trees.
	tree map[string]string
	// taken is each issue of own that has taken its first slot in the replay.
	taken map[string]bool
	// barred is each issue of own that has not taken its first slot in the replay and whose first
	// phase the replay's daemon would not start now (barAtStart, driver.reviewFirstStarts).
	barred map[string]bool
}

// ranks is the rank each issue is shown at now in place of Dispatch's: an issue of own at its own
// rank, and a root at the rank of its tree's next first start (rootRank), or at Dispatch's where
// none is left. Nil for no first starts.
func (f *firstStarts) ranks() map[string]string {
	if f == nil {
		return nil
	}
	ranks := maps.Clone(f.own)
	for root := range f.trees {
		if rank, shown := f.rootRank(root); shown {
			ranks[root] = rank
		} else {
			delete(ranks, root)
		}
	}
	return ranks
}

// rootRank is the rank root is shown at now: the own rank of the first issue of its tree that has
// not taken its first slot in the replay and can still take it there; where none can, its own rank
// where it has not taken its own first slot, since a root that cannot start yet has no tree open
// for a member to start in and its rank pins none; and false where neither is left, the root then
// being shown at Dispatch's rank.
func (f *firstStarts) rootRank(root string) (string, bool) {
	for _, key := range f.trees[root] {
		if !f.taken[key] && !f.barred[key] {
			return f.own[key], true
		}
	}
	if f.pending(root) {
		return f.own[root], true
	}
	return "", false
}

// with is a copy of f whose barred holds changed in place of f's: each key barred where its value
// is true and not where it is false. It is how a reading inside a fact's transaction ranks the roots
// before the fact commits (driver.reviewFirstStarts), f itself taking the change only once it does.
func (f *firstStarts) with(changed map[string]bool) *firstStarts {
	next := *f
	next.barred = maps.Clone(f.barred)
	for key, barred := range changed {
		if barred {
			next.barred[key] = true
		} else {
			delete(next.barred, key)
		}
	}
	return &next
}

// keys is the issues of own in key order, none for no first starts.
func (f *firstStarts) keys() []string {
	if f == nil {
		return nil
	}
	return f.order
}

// left says whether an issue of own has still to take its first slot in the replay.
func (f *firstStarts) left() bool {
	return f != nil && len(f.taken) < len(f.own)
}

// barAtStart bars each issue of own whose first phase cannot start where t's start state at at
// records it (startRecords, record.FirstPhaseMayStart): one the start state does not record yet,
// or records finished, past its first phase or out of the workflow. It is the reading the start
// state is written with, before the replay's daemon holds a record to judge the line on; the
// replay reads each again by the line's whole test before each admission pass of its daemon, from
// the first fact on (driver.reviewFirstStarts).
func (f *firstStarts) barAtStart(t Trace, at time.Time) {
	keys := f.keys()
	placed := startRecords(t, at, keys)
	for _, key := range keys {
		issue, recorded := placed[key]
		f.bar(key, !recorded || !record.FirstPhaseMayStart(issue))
	}
}

// pending says whether key is an issue of own that has not taken its first slot in the replay.
func (f *firstStarts) pending(key string) bool {
	if f == nil {
		return false
	}
	_, started := f.own[key]
	return started && !f.taken[key]
}

// take records key's first slot in the replay and answers the roots whose rank it may move
// (rootsOf). It answers none for an issue the record did not start in the window, and for one that
// has taken its first slot already.
func (f *firstStarts) take(key string) []string {
	if !f.pending(key) {
		return nil
	}
	f.taken[key] = true
	return f.rootsOf(key)
}

// bar records whether key, an issue of own that has not taken its first slot in the replay, cannot
// take it there now, and answers the roots whose rank the change may move (rootsOf). It answers none
// where nothing changed, and for an issue that is not pending.
func (f *firstStarts) bar(key string, barred bool) []string {
	if !f.pending(key) || f.barred[key] == barred {
		return nil
	}
	if barred {
		f.barred[key] = true
	} else {
		delete(f.barred, key)
	}
	return f.rootsOf(key)
}

// rootsOf is the roots whose rank key's first start counts in: the root of the tree key started in,
// and key itself where it roots another.
func (f *firstStarts) rootsOf(key string) []string {
	var roots []string
	if root, ranked := f.tree[key]; ranked {
		roots = append(roots, root)
	}
	if _, rooted := f.trees[key]; rooted && f.tree[key] != key {
		roots = append(roots, key)
	}
	return roots
}
