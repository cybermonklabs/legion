package replay

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// Admission by signals in a replay (spec R11). The snapshot of each pass (readSignals) reads R11's
// signals from the replay itself, never from the record. The idle share comes from the replay's own
// slot holders: the states its pass observer reads over the newest pass's view and the stub's
// claims. The approvals come from the replay's copy of Dispatch. The provider's headroom, the kills
// and the merge queue come from the load models, read at the replay's own work in flight, segment
// starts and queue entries as the run goes (ModelReader). After every pass, at
// each moment a modelled signal's input changes with no pass, and at each turn's start and end that
// moves the idle share, the run computes the project's one deadline: the earliest crossing of the
// signals with a limit, and under the back-off the allowance's next rise. It keeps one
// intake.SignalDeadline item at that moment (recomputeDeadline), and each fire runs a deadline's
// pass. The daemon's timer lives in its memory. So no deadline fires while the daemon is down, a
// boot drops the one that stands, and the boot's first pass computes it again.

// SlotPass is one slot a pass gave out or took back, as run.json's slotPasses gives it: the
// issue, the pass's moment, take or give, and the kind of the fact whose pass it is.
type SlotPass struct {
	Issue    string           `json:"issue"`
	At       time.Time        `json:"at"`
	Action   string           `json:"action"`
	PassKind signals.PassKind `json:"passKind"`
}

// The actions of a SlotPass.
const (
	slotTake = "take"
	slotGive = "give"
)

// DeadlineEntry is one change of a run's deadline, as run.json's deadlines gives it: the action
// (set, moved, dropped or fired), the moment it happened, the moment the deadline is due, the
// signal whose crossing it is (allowanceCrossing for the back-off's rise), and, for a deadline
// moved, the moment it was due before.
type DeadlineEntry struct {
	Action   string    `json:"action"`
	At       time.Time `json:"at"`
	Due      time.Time `json:"due"`
	Signal   string    `json:"signal"`
	Previous time.Time `json:"previous,omitzero"`
}

// The actions of a DeadlineEntry.
const (
	deadlineSet     = "set"
	deadlineMoved   = "moved"
	deadlineDropped = "dropped"
	deadlineFired   = "fired"
)

// allowanceCrossing names the back-off's next rise among a run's crossings (signals.NextRise).
const allowanceCrossing = "allowance"

// SignalSummary is how one limited signal read over a run's span [From, To), as run.json's
// signalSummaries gives it: its mean over the minutes the window records it in, each minute read at
// the minute's start, its 95th percentile by the nearest rank and its largest value there; how many
// minutes of the span the window records it in (Recorded) of the span's (Minutes); and the share of
// the span's minutes at which the signal held admission, at or above its limit (HeldShare), the
// minutes it bound a pass. A signal the window does not record in a minute binds nothing there.
type SignalSummary struct {
	Mean      float64 `json:"mean"`
	P95       float64 `json:"p95"`
	Max       float64 `json:"max"`
	Recorded  int     `json:"recorded"`
	Minutes   int     `json:"minutes"`
	HeldShare float64 `json:"heldShare"`
}

// SignalReading is a signal's limit and the value read at it in a run's run.json under the key
// signals, which every run of a signal rule that limits a signal writes: the value is the signal's
// mean over the minutes the window records it in, as its summary (SignalSummary) gives. The
// comparison's document gives each run's readings (Runs.Comparison).
type SignalReading struct {
	Limit float64 `json:"limit"`
	Value float64 `json:"value"`
}

// signalModels is the load model each modelled signal comes from (spec R11). A model that gives
// no limit for its signal leaves the signal with no limit in a run, but where givesSuppliedLimit
var signalModels = map[string]string{
	SignalQueue:    ModelQueue,
	SignalProvider: ModelTokens,
	SignalKills:    ModelMemory,
}

// givesSuppliedLimit says whether model limits its signal in a run given the project's token
// limit (RunConfig.TokenLimit) though it gives no limit itself: the tokens model where it gives no
// daily test only because the window records no limit of the project's own (assumption 48), the
// run then modelling the project's own refused minutes against the token limit (assumption 49). A
// model that fails gives none. Whether such a limit stands for a comparison is the limit test's to
// say, over two records.
func givesSuppliedLimit(model LoadModel) bool {
	return model.Name == ModelTokens && model.Verdict == VerdictNoTest && model.Reason == reasonNoLimitSpan
}

// runSignals is what one run reads and records of R11's signals. It holds:
//   - the rule's mode, the run's limits as given (RunConfig.Limits), the limits in force, which
//     leave out each signal whose model gives no limit (unlimited, by signal, with the model), d
//     (idleAfter), and, by issue, the newest end of a turn the record shows before the window's
//     start (endedBefore), which is the replay's own course up to its start;
//   - the load models read as the run goes (models), nil where the run reads no modelled signal;
//   - the project's default assignee from each settings event that moves it, applied up to the
//     moment read (defaults, nextDefault), and the approvals counted when the replay's copy of
//     Dispatch last moved them;
//   - the moments the run writes each limited signal's value at (probes, probed so far, values);
//   - the deadline that stands and the item of the next moment a modelled signal's input changes;
//     armed says a pass of the daemon process now running has set the timer;
//   - each change of the deadline, and each slot each pass gave out or took back.
type runSignals struct {
	mode        signals.Mode
	limits      map[string]float64
	inForce     map[string]float64
	unlimited   map[string]string
	idleAfter   time.Duration
	endedBefore map[string]time.Time
	models      *ModelReader

	defaults       []personAt
	nextDefault    int
	approvals      int
	approvalsMoved int
	approvalsRead  bool

	probes []time.Time
	probed int
	// sampleAt is the next minute of the window's span the run reads each limited signal at, and
	// tallies what it has read of each (sampleSignals).
	sampleAt time.Time
	tallies  map[string]*signalTally
	values   map[string]map[string]*float64

	deadline  *standingDeadline
	input     *item
	armed     bool
	deadlines []DeadlineEntry
	passes    []SlotPass
}

// standingDeadline is the deadline that stands: its item, its moment and its signal.
type standingDeadline struct {
	it     *item
	due    time.Time
	signal string
}

// setupSignals reads what the run reads of R11's signals (runSignals): its limits, d, its probes,
// and, where it limits a modelled signal or backs off, the load models fitted on the window
// (windowInputs.models), read from the window's start. A model limits its signal where it gives a
// limit, and, under a token limit (RunConfig.TokenLimit), where givesSuppliedLimit says it does. The
// back-off folds the kills where the memory model limits its signal. The models read the token
// limit only where the run limits the provider's headroom, so a run that does not limit it rests on
// no minute modelled against it. Where the run limits the approvals, the replay's copy of Dispatch
// reads every ask, assignee and default assignee.
func (d *driver) setupSignals() error {
	cfg, w := d.cfg, d.window
	s := &runSignals{mode: d.rule.Signals, limits: maps.Clone(cfg.Limits), inForce: map[string]float64{}, unlimited: map[string]string{},
		idleAfter: cfg.IdleAfter, values: map[string]map[string]*float64{}, deadlines: []DeadlineEntry{}, passes: []SlotPass{}}
	if s.limits == nil {
		s.limits = map[string]float64{}
	}
	if s.idleAfter == 0 {
		s.idleAfter = signals.DefaultIdleAfter
	}
	for _, at := range cfg.Probes {
		s.probes = append(s.probes, at.UTC())
	}
	slices.SortFunc(s.probes, time.Time.Compare)
	s.probes = slices.CompactFunc(s.probes, time.Time.Equal)
	s.endedBefore = turnsEndedBefore(d.trace.Issues, w.From)
	d.sig = s
	modelled := s.mode == signals.Backoff
	for name := range s.limits {
		modelled = modelled || signalModels[name] != ""
	}
	if !modelled {
		maps.Copy(s.inForce, s.limits)
	} else {
		models, err := d.inputs.models()
		if err != nil {
			return fmt.Errorf("replay: fit the load models the run's signals come from: %w", err)
		}
		gives := map[string]bool{}
		for _, model := range models.Models {
			gives[model.Name] = model.GivesLimit || cfg.TokenLimit > 0 && givesSuppliedLimit(model)
		}
		readsTokens := false
		for name, limit := range s.limits {
			if model := signalModels[name]; model != "" && !gives[model] {
				s.unlimited[name] = model
				continue
			}
			s.inForce[name] = limit
			readsTokens = readsTokens || signalModels[name] == ModelTokens
		}
		var tokenLimit int64
		if readsTokens && gives[ModelTokens] {
			tokenLimit = cfg.TokenLimit
		}
		if s.models, err = models.Reader(w.From, gives[ModelMemory], tokenLimit); err != nil {
			return err
		}
	}
	s.sampleAt, s.tallies = w.From, map[string]*signalTally{}
	for name := range s.inForce {
		s.tallies[name] = &signalTally{}
	}
	if _, reads := s.limits[SignalApprovals]; reads {
		d.shadow.readsApprovals = true
		d.shadow.approvals.SetDefaultAssignee(w.DefaultAssignee)
		for i, raw := range w.Settings {
			event, err := ParseSettingsEvent(raw)
			if err != nil {
				return fmt.Errorf("replay: line %d of %s: %w", i+1, settingsFile, err)
			}
			if event.Type == defaultAssigneeEvent && event.Project == w.Project {
				s.defaults = append(s.defaults, personAt{at: event.At, person: event.DefaultAssignee})
			}
		}
		slices.SortStableFunc(s.defaults, func(a, b personAt) int { return a.at.Compare(b.at) })
	}
	return nil
}

// readSignals is the pass's snapshot of the signals (admit.Admission.UseSignals), on only under a
// rule whose mode is not Off: the reading of each signal whose limit is in force, at the clock's
// instant, and under the back-off the setbacks it folds, under the cap the daemon process serves
// under as the ceiling. A signal with no limit holds no start, so the pass reads no value of it. It
// reads the replay's own state, never tx.
func (d *driver) readSignals(_ context.Context, _ pgx.Tx, _ time.Time) (signals.Snapshot, bool, error) {
	s := d.sig
	if s.mode == signals.Off {
		return signals.Snapshot{}, false, nil
	}
	at := d.clock.Instant()
	snapshot := signals.Snapshot{Mode: s.mode, At: at, Readings: make(map[string]signals.Reading, len(s.inForce))}
	for _, name := range signals.Names() {
		if limit, set := s.inForce[name]; set {
			snapshot.Readings[name] = signals.Reading{Value: d.signalValue(name, at), Limit: &limit}
		}
	}
	if s.mode == signals.Backoff {
		snapshot.Setbacks = s.models.Setbacks(at, d.cap)
	}
	if d.cfg.onSnapshot != nil {
		d.cfg.onSnapshot(snapshot)
	}
	return snapshot, true, nil
}

// signalValue is the signal name at at as the replay reads it: the idle share of the replay's own
// holders (snapshots.idleHolders) and the approvals of its copy of Dispatch, each unrecorded where
// the window holds no record of it (assumption 47); every other signal from the load models.
func (d *driver) signalValue(name string, at time.Time) signals.Value {
	switch name {
	case SignalIdle:
		if !d.window.Recorded(SignalIdle, at) {
			return signals.Value{}
		}
		holders, idle := d.idleHolders()
		return signals.IdleAt(signals.Coverage{}, holders, idle, at, d.sig.idleAfter)
	case SignalApprovals:
		if !d.window.Recorded(SignalApprovals, at) {
			return signals.Value{}
		}
		return signals.Value{Recorded: true, Value: float64(d.waitingApprovals(at))}
	case SignalQueue:
		return d.sig.models.Queue(at)
	case SignalProvider:
		return d.sig.models.Headroom(at)
	case SignalKills:
		return d.sig.models.Kills(at)
	}
	return signals.Value{}
}

// signalNext is the next crossing after at of limit by the signal name as the replay reads it, if
// no new event came, and whether one is due. The approvals change only at events, so they have none.
func (d *driver) signalNext(name string, at time.Time, limit float64) (time.Time, bool) {
	switch name {
	case SignalIdle:
		holders, idle := d.idleHolders()
		return signals.IdleNext(d.window.coverageOf(SignalIdle, at), holders, idle, at, d.sig.idleAfter, limit)
	case SignalQueue:
		return d.sig.models.QueueNext(at, limit)
	case SignalProvider:
		return d.sig.models.HeadroomNext(at, limit)
	case SignalKills:
		return d.sig.models.KillsNext(at, limit)
	}
	return time.Time{}, false
}

// idleHolders is the replay's own slot holders as the newest pass left them, and the moment each
// of them that is idle has stood idle since (snapshots.idleHolders), each moment at the instant of
// the fact that wrote it (simClock.instantOf), as the record's lines date a turn's end.
func (d *driver) idleHolders() (int, []time.Time) {
	holders, idle := d.snap.idleHolders(d.claims, d.sig.endedBefore)
	for i, since := range idle {
		idle[i] = d.clock.instantOf(since)
	}
	return holders, idle
}

// turnsEndedBefore is, by issue, the newest end of a turn of any of its roles that the trace's
// issues record before at: where a replay's holder stood idle since at its start, as the record ran
// up to it.
func turnsEndedBefore(issues map[string]*IssueTrace, at time.Time) map[string]time.Time {
	ended := map[string]time.Time{}
	for key, iss := range issues {
		for _, turn := range iss.Turns {
			if end := turn.End; !end.IsZero() && end.Before(at) && end.After(ended[key]) {
				ended[key] = end
			}
		}
	}
	return ended
}

// waitingApprovals is how many asks wait on the project's default assignee at at, in the replay's
// copy of Dispatch (shadow.approvals), each settings event that moves the default assignee applied
// at its recorded moment, counted again only where something moved since the last count.
func (d *driver) waitingApprovals(at time.Time) int {
	s := d.sig
	for s.nextDefault < len(s.defaults) && !s.defaults[s.nextDefault].at.After(at) {
		d.shadow.approvals.SetDefaultAssignee(s.defaults[s.nextDefault].person)
		s.nextDefault, s.approvalsRead = s.nextDefault+1, false
	}
	if !s.approvalsRead || s.approvalsMoved != d.shadow.approvalsMoved {
		s.approvals, s.approvalsMoved, s.approvalsRead = len(d.shadow.approvals.Waiting()), d.shadow.approvalsMoved, true
	}
	return s.approvals
}

// recomputeDeadline computes the project's deadline at the clock's instant, under a rule that
// admits by signals, once a pass of the daemon process now running has armed its timer. The
// deadline is the earliest crossing (signals.Earliest) of each signal with a limit in force, and
// under the back-off the allowance's next rise (signals.NextRise). A deadline at a new moment
// replaces the one that stands. One that is no longer due is dropped. The run records each change.
// It then keeps the item of the next moment a modelled signal's input changes with no pass
// (scheduleInput).
func (d *driver) recomputeDeadline() error {
	s := d.sig
	if s.mode == signals.Off || !s.armed {
		return nil
	}
	at := d.clock.Instant()
	var crossings []signals.Crossing
	for _, name := range signals.Names() {
		if limit, set := s.inForce[name]; set {
			if next, due := d.signalNext(name, at, limit); due {
				crossings = append(crossings, signals.Crossing{Signal: name, At: next})
			}
		}
	}
	if s.mode == signals.Backoff {
		if next, due := signals.NextRise(d.cap, s.models.Setbacks(at, d.cap), at); due {
			crossings = append(crossings, signals.Crossing{Signal: allowanceCrossing, At: next})
		}
	}
	crossing, due := signals.Earliest(crossings)
	switch standing := s.deadline; {
	case !due && standing != nil:
		standing.it.done, s.deadline = true, nil
		s.deadlines = append(s.deadlines, DeadlineEntry{Action: deadlineDropped, At: at, Due: standing.due, Signal: standing.signal})
	case due && standing == nil:
		s.deadline = d.pushDeadline(crossing)
		s.deadlines = append(s.deadlines, DeadlineEntry{Action: deadlineSet, At: at, Due: crossing.At, Signal: crossing.Signal})
	case due && !crossing.At.Equal(standing.due):
		standing.it.done = true
		s.deadline = d.pushDeadline(crossing)
		s.deadlines = append(s.deadlines, DeadlineEntry{Action: deadlineMoved, At: at, Due: crossing.At, Signal: crossing.Signal, Previous: standing.due})
	}
	d.scheduleInput(at)
	return nil
}

// pushDeadline queues the item of a deadline at crossing's moment: at it, the run records the fire
// and applies intake.SignalDeadline through the chain, whose pass is a deadline's. The item is the
// daemon's timer, which a down daemon drops (item.timer).
func (d *driver) pushDeadline(crossing signals.Crossing) *standingDeadline {
	standing := &standingDeadline{due: crossing.At, signal: crossing.Signal}
	standing.it = d.push(&item{at: crossing.At, class: classWake, rec: crossing.At, timer: true, run: func() error {
		d.sig.deadline = nil
		d.sig.deadlines = append(d.sig.deadlines, DeadlineEntry{Action: deadlineFired, At: d.clock.Instant(), Due: standing.due, Signal: standing.signal})
		_, err := d.apply("replay", d.factID("signal-deadline", standing.signal), intake.SignalDeadline{At: standing.due})
		return err
	}})
	return standing
}

// scheduleInput keeps the item of the first moment after at at which a modelled signal the run
// reads changes with no pass, as the models stand: a kill, where the run limits the kills or backs
// off on them; and a pull request entering or leaving the merge queue, where it limits the queue.
// At that moment the run computes its deadline again and runs no pass: the replay leaves each
// memory kill's fact out (assumption 31), as it leaves every other modelled event.
func (d *driver) scheduleInput(at time.Time) {
	s := d.sig
	var next time.Time
	consider := func(moment time.Time, due bool) {
		if due && moment.After(at) && (next.IsZero() || moment.Before(next)) {
			next = moment
		}
	}
	if m := s.models; m != nil {
		_, kills := s.inForce[SignalKills]
		_, queue := s.inForce[SignalQueue]
		if kills || s.mode == signals.Backoff && m.foldKills {
			consider(m.NextKill(at))
		}
		if queue {
			consider(m.NextQueueChange(at))
		}
	}
	if s.input != nil && !s.input.done && s.input.at.Equal(next) {
		return
	}
	if s.input != nil {
		s.input.done, s.input = true, nil
	}
	if next.IsZero() {
		return
	}
	s.input = d.push(&item{at: next, class: classWake, rec: next, order: 1, timer: true, run: func() error {
		d.sig.input = nil
		return d.recomputeDeadline()
	}})
}

// idleMoved computes the deadline again where a turn of the replay's stub starts or ends with no
// pass, which moves the idle share where the run limits it.
func (d *driver) idleMoved() error {
	if _, idle := d.sig.inForce[SignalIdle]; !idle {
		return nil
	}
	return d.recomputeDeadline()
}

// bootDropsTimer is the boot of a new daemon process: the timer the old process kept in memory is
// gone with it, so the deadline that stands is dropped at the boot, fired or not, and the item of
// the next change of a modelled input with it. The boot's first pass arms the timer again.
func (d *driver) bootDropsTimer() {
	s := d.sig
	s.armed = false
	if standing := s.deadline; standing != nil {
		standing.it.done, s.deadline = true, nil
		s.deadlines = append(s.deadlines, DeadlineEntry{Action: deadlineDropped, At: d.clock.Instant(), Due: standing.due, Signal: standing.signal})
	}
	if s.input != nil {
		s.input.done, s.input = true, nil
	}
}

// observePass is the pass observer of the run (admit.Admission.UsePassObserver): the snapshot
// timeline's view, and each slot the pass took back and then each it gave out, at the clock's
// instant, with the pass's kind (run.json's slotPasses).
func (d *driver) observePass(view admit.PassView) {
	d.snap.pass(view)
	at := d.clock.Instant()
	for _, taken := range view.Taken {
		d.sig.passes = append(d.sig.passes, SlotPass{Issue: taken.Issue, At: at, Action: slotTake, PassKind: view.Kind})
	}
	for _, given := range view.Given {
		d.sig.passes = append(d.sig.passes, SlotPass{Issue: given.Issue, At: at, Action: slotGive, PassKind: view.Kind})
	}
}

// readProbes writes the value of each signal the run's limits name at each probe before until, or
// at each probe left where all is set: once every act at or before the probe has run, as the
// replay stands then. An unrecorded value is null.
func (d *driver) readProbes(until time.Time, all bool) {
	s := d.sig
	for ; s.probed < len(s.probes) && (all || s.probes[s.probed].Before(until)); s.probed++ {
		at := s.probes[s.probed]
		values := make(map[string]*float64, len(s.limits))
		for _, name := range slices.Sorted(maps.Keys(s.limits)) {
			values[name] = nil
			if value := d.signalValue(name, at); value.Recorded {
				values[name] = &value.Value
			}
		}
		s.values[at.Format(time.RFC3339Nano)] = values
	}
}

// setFlight puts key in flight in the replay, or takes it out, and tells the load models of the
// change: the issues in flight from the clock's instant on, and the stretch in flight of an issue
// whose work the record never ran (script.drawnWork).
func (d *driver) setFlight(key string, in bool) error {
	if d.inFlight[key] == in {
		return nil
	}
	if in {
		d.inFlight[key] = true
	} else {
		delete(d.inFlight, key)
	}
	m := d.sig.models
	if m == nil {
		return nil
	}
	at := d.clock.Instant()
	if d.script.drawnWork[key] {
		if in {
			m.DrawnEnters(key, at)
		} else {
			m.DrawnLeaves(key, at)
		}
	}
	return m.Flight(at, len(d.inFlight))
}

// noteSegment tells the load models of the replay's start of p: a recorded work segment's start,
// by the moment the record started it; or drawn work's start, a copy of its source's segment, whose
// pull requests enter the merge queue at their offsets in it.
func (d *driver) noteSegment(p *piece) {
	m := d.sig.models
	switch {
	case m == nil:
	case p.drawn && p.source != "":
		m.DrawnSegment(p.source, p.start, p.at)
	case p.visit != nil && p.visit.Segment != nil && p.start.Equal(p.visit.Segment.Start):
		m.Segment(p.issue, p.start, p.at)
	}
}

// ceiling is the cap the run serves under as a ceiling, as run.json gives it: the run's own fixed
// cap where it sets one, else the one cap the window's record serves over its span (capSpans), and
// nil where the record's cap changes in the span, which the window's caps then give.
func (d *driver) ceiling() *int {
	if d.cfg.Cap > 0 {
		return &d.cfg.Cap
	}
	spans := capSpans(d.window, d.trace.Boots)
	if len(spans) == 0 {
		return nil
	}
	for _, span := range spans[1:] {
		if span.Cap != spans[0].Cap {
			return nil
		}
	}
	return &spans[0].Cap
}

// signalTally is what a run has read of one limited signal at the start of each minute of its span
// so far: each value it read where the window records it, how many minutes it read, and in how many
// the signal held admission, at or above its limit.
type signalTally struct {
	values  []float64
	minutes int
	held    int
}

// sampleSignals reads each signal the run limits at the start of each minute of the window's span
// [From, To) that comes before until, or every minute left where all is set, once every act at or
// before the minute has run, as the replay stands then, as readProbes reads a probe. A probe before
// the minute is read first, so that nothing is read out of the order of the clock. The tallies it
// keeps make run.json's signalSummaries and signals (runSignals.summaries, runSignals.readings).
func (d *driver) sampleSignals(until time.Time, all bool) {
	s := d.sig
	if len(s.tallies) == 0 {
		return
	}
	for ; s.sampleAt.Before(d.window.To) && (all || s.sampleAt.Before(until)); s.sampleAt = s.sampleAt.Add(time.Minute) {
		d.readProbes(s.sampleAt, false)
		for _, name := range signals.Names() {
			tally, limited := s.tallies[name]
			if !limited {
				continue
			}
			value := d.signalValue(name, s.sampleAt)
			tally.minutes++
			if value.Recorded {
				tally.values = append(tally.values, value.Value)
			}
			if value.Binds(s.inForce[name]) {
				tally.held++
			}
		}
	}
}

// summaries is how each signal the run limits read over the window's span (SignalSummary), from the
// tallies sampleSignals kept: {} for a run that limits none.
func (s *runSignals) summaries() map[string]SignalSummary {
	out := map[string]SignalSummary{}
	for name, tally := range s.tallies {
		sum := SignalSummary{Recorded: len(tally.values), Minutes: tally.minutes}
		if len(tally.values) > 0 {
			total := 0.0
			for _, v := range tally.values {
				total += v
			}
			sum.Mean, sum.P95, sum.Max = total/float64(len(tally.values)), percentile(tally.values, 0.95), slices.Max(tally.values)
		}
		if tally.minutes > 0 {
			sum.HeldShare = float64(tally.held) / float64(tally.minutes)
		}
		out[name] = sum
	}
	return out
}

// readings is each signal the run limits with its limit and the value it read at it: its mean over
// the minutes the window records it in (SignalReading), which `compare` gives beside the limit. It
// is {} for a run that limits no signal.
func (s *runSignals) readings(summaries map[string]SignalSummary) map[string]SignalReading {
	out := map[string]SignalReading{}
	for name, sum := range summaries {
		out[name] = SignalReading{Limit: s.inForce[name], Value: sum.Mean}
	}
	return out
}
