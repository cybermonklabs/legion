package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// The definitions of R11's signals (spec R11) are package signals': each signal at a moment, lower
// being better for each, and, for each signal that can cross a limit with no event, the next moment
// after it at which it would. This file is the replay's adapter: it reads a collected window's own
// record (SignalRecord) into the plain values those definitions take — the holders and kills and
// waits of the trace and the launcher's and the queue's lines, the provider's minutes, each ask with
// the people it can wait on — and says where the window's record holds each signal and where it
// begins or stops. The coverage of the record, the minutes of the future the record already holds
// and the decoding of its files and events stay here; the daemon's own admission pass reads the
// same definitions from its events. The docs page gives each signal a section of its own
// (replay.md, "The signals").

// projectLimitScope is the scope the provider's record names the project's own limit by: its
// limit over all its models, as t25's provider-record script writes it (ProviderMinute.LimitScope).
// A limit of another scope is one model's own, which is no limit on the project's tokens.
const projectLimitScope = "Claude"

// SignalDefinition is one signal of R11 as the tool defines it: its name, a key of a window's
// signals map; At, its value at a moment; Next, the next moment after a moment at which it would
// cross a limit, in either direction, if no new event came, and whether one is due, nil for a
// signal that changes only at events (EventOnly); the window's inputs it reads; and the source a
// replay under another rule takes it from, the replay itself or t31's model fitted on the window.
// A signal binds where it is recorded and at or above its limit, since a rule admits while each
// signal is under its limit (spec R7, R11), and it crosses its limit where it starts or stops
// binding: at an edge of its record's span too, since an unrecorded signal binds nothing.
type SignalDefinition struct {
	Name      string
	At        func(r *SignalRecord, at time.Time) signals.Value
	Next      func(r *SignalRecord, at time.Time, limit float64) (time.Time, bool)
	EventOnly bool
	Inputs    []string
	Source    string
}

// signalDefinitions is every signal of R11, one definition each, in the order of R11's list.
var signalDefinitions = []SignalDefinition{
	{Name: SignalIdle, At: idleAt, Next: idleNext, Inputs: []string{logFile}, Source: "replay"},
	{Name: SignalQueue, At: queueAt, Next: queueNext, Inputs: []string{queueDir + "/<owner>__<repo>.jsonl"}, Source: "merge queue model"},
	{Name: SignalProvider, At: providerAt, Next: providerNext, Inputs: []string{providerFile}, Source: "tokens model"},
	{Name: SignalKills, At: killsAt, Next: killsNext, Inputs: []string{launcherFile}, Source: "memory model"},
	{Name: SignalApprovals, At: approvalsAt, EventOnly: true,
		Inputs: []string{eventsDir + "/<KEY>.jsonl", settingsFile, windowFile + "#defaultAssignee"}, Source: "replay"},
}

// SignalDefinitions is every signal of R11, one definition each, in the order of R11's list.
func SignalDefinitions() []SignalDefinition { return slices.Clone(signalDefinitions) }

// SignalRecord is a window's record read for R11's signals (ReadSignalRecord): the slot holders
// of its trace, the memory guard's kills, the merge queue's waits, the provider's minutes, and
// each ask with the people it can wait on, so that each definition reads its signal at any moment.
// tokenLimit is the project's token limit its caller gives, zero for none (assumption 49).
type SignalRecord struct {
	window     Window
	idleAfter  time.Duration
	tokenLimit int64

	// holders is the index of each issue of the window's project, which the idle signal reads
	// the slot holders and their states from.
	holders []*issueIndex
	// kills is the moment of each kill the launcher's lines record, in order.
	kills []time.Time
	// waits is each wait of a pull request in the merge queue, by the moment it began.
	waits []signals.Wait
	// minutes is each minute of the provider's record, by its Unix time, and lastMinute the
	// latest; covered is each moment the record begins or stops holding the minute before.
	minutes    map[int64]ProviderMinute
	lastMinute time.Time
	covered    []time.Time
	// asks is each ask of an issue of the project, assignees each such issue's assignee from each
	// issue event that names it, and defaults the project's default assignee from each settings
	// event that moves it.
	asks      []*askCourse
	assignees map[string][]personAt
	defaults  []personAt
}

// ReadSignalRecord reads w's record of the signals, idleAfter being d, how long a holder has been
// idle before the idle signal counts it, and tokenLimit the project's token limit in uncached input
// tokens a minute, which the provider's headroom reads in each minute whose record holds no limit
// of the project's own, zero for none (assumption 49). It refuses a d below zero, a token limit
// below zero, a window whose trace does not build (BuildTrace), and a line of a signal's input
// that its definition cannot read.
func ReadSignalRecord(w Window, idleAfter time.Duration, tokenLimit int64) (*SignalRecord, error) {
	if idleAfter < 0 {
		return nil, fmt.Errorf("replay: a holder idle for %s: d is below zero", idleAfter)
	}
	if tokenLimit < 0 {
		return nil, tokenLimitBelowZero(tokenLimit)
	}
	trace, err := BuildTrace(w)
	if err != nil {
		return nil, err
	}
	r := &SignalRecord{window: w, idleAfter: idleAfter, tokenLimit: tokenLimit, minutes: make(map[int64]ProviderMinute, len(w.Provider))}
	for _, key := range slices.Sorted(maps.Keys(trace.Issues)) {
		if iss := trace.Issues[key]; iss.Project == trace.Project {
			r.holders = append(r.holders, indexIssue(iss, trace.GateLines))
		}
	}
	kills, err := launcherKills(w.Launcher)
	if err != nil {
		return nil, err
	}
	for _, kill := range kills {
		r.kills = append(r.kills, kill.At)
	}
	if r.waits, err = queueWaits(w.Queue); err != nil {
		return nil, err
	}
	for _, minute := range w.Provider {
		r.minutes[minute.Minute.Unix()] = minute
		r.lastMinute = maxTime(r.lastMinute, minute.Minute)
	}
	for _, minute := range w.Provider {
		if _, held := r.minutes[minute.Minute.Add(-time.Minute).Unix()]; !held {
			r.covered = append(r.covered, minute.Minute.Add(time.Minute))
		}
		if _, held := r.minutes[minute.Minute.Add(time.Minute).Unix()]; !held {
			r.covered = append(r.covered, minute.Minute.Add(2*time.Minute))
		}
	}
	if err := r.readAsks(); err != nil {
		return nil, err
	}
	return r, nil
}

// IdleAfter is d, how long a holder has been idle before r's idle signal counts it.
func (r *SignalRecord) IdleAfter() time.Duration { return r.idleAfter }

// spanEdges is each moment after at at which the window's record of signal begins or ends.
func (r *SignalRecord) spanEdges(signal string, at time.Time) []time.Time {
	return r.window.signalEdges(signal, at)
}

// signalEdges is each moment after at at which w's record of signal begins or ends.
func (w Window) signalEdges(signal string, at time.Time) []time.Time {
	var edges []time.Time
	for _, span := range w.Signals[signal] {
		for _, edge := range []time.Time{span.From, span.To} {
			if edge.After(at) {
				edges = append(edges, edge)
			}
		}
	}
	return edges
}

// coverageOf is where w's record holds signal, as the definitions read it, with each edge of that
// record after at, for a definition of the next crossing.
func (w Window) coverageOf(signal string, at time.Time) signals.Coverage {
	return signals.Coverage{Held: func(t time.Time) bool { return w.Recorded(signal, t) }, Edges: w.signalEdges(signal, at)}
}

// held is where the window's record holds signal, as the definitions read it.
func (r *SignalRecord) held(signal string) signals.Coverage {
	return signals.Coverage{Held: func(at time.Time) bool { return r.window.Recorded(signal, at) }}
}

// coverage is held with each edge of the record of signal after at, for a definition of the next
// crossing.
func (r *SignalRecord) coverage(signal string, at time.Time) signals.Coverage {
	c := r.held(signal)
	c.Edges = r.spanEdges(signal, at)
	return c
}

// idleAt is the idle signal at at: the share of the slot holders whose state (R6's, as the slots
// reading names it: record.HolderStateOf) is idle with its time at least d before at, from the
// daemon's slot lines and the `supervise:` lines of each turn's start and end, as the window's
// trace reads them. A holder's idle time is the later of the newest end of a turn of its claims
// and the moment it took the slot it holds. A moment with no holder has a share of 0.
func idleAt(r *SignalRecord, at time.Time) signals.Value {
	holders, idle := r.idleHolders(at)
	return signals.IdleAt(r.held(SignalIdle), holders, idle, at, r.idleAfter)
}

// idleNext is the idle signal's next crossing of limit after at: the holders standing as they do
// at at, the moment a holder idle since s reaches s + d, or an edge of the record's span. A holder
// leaves idle only at an event, so the signal falls at none, and a crossing downward is due only
// where the record ends.
func idleNext(r *SignalRecord, at time.Time, limit float64) (time.Time, bool) {
	holders, idle := r.idleHolders(at)
	return signals.IdleNext(r.coverage(SignalIdle, at), holders, idle, at, r.idleAfter, limit)
}

// idleHolders is how many of the project's issues hold a slot at at, and the idle time of each of
// them whose state is idle (record.HolderStateOf). Its holder facts are the line timeline's
// (holderFactsAt), with the moment the holder took its slot and the newest end of a turn of it,
// which time the idle state.
func (r *SignalRecord) idleHolders(at time.Time) (int, []time.Time) {
	holders := 0
	var idle []time.Time
	for _, x := range r.holders {
		if x.slots.at(at) == 0 {
			continue
		}
		holders++
		iss := x.iss
		tree := iss.Key
		if entry := entryOf(iss, at); entry != nil && entry.Tree != "" {
			tree = entry.Tree
		}
		facts := holderFactsAt(x, at, tree, visitOf(iss, at), x.waits.at(at) > 0, x.held.at(at) > 0)
		if i := sort.Search(len(iss.Slots), func(i int) bool { return iss.Slots[i].Start.After(at) }); i > 0 {
			facts.AdmittedAt = iss.Slots[i-1].Start
		}
		for _, turn := range iss.Turns {
			if end := turn.End; !end.IsZero() && !end.After(at) && (facts.LastTurnEnd == nil || end.After(*facts.LastTurnEnd)) {
				facts.LastTurnEnd = &end
			}
		}
		if state, since := record.HolderStateOf(facts); state == record.HolderIdle && since != nil {
			idle = append(idle, *since)
		}
	}
	return holders, idle
}

// IdleShareOfState is the idle signal at at, d being after, read live from R6's states and times
// in a body of `legion state --json`: the share of the slots in use (admission.slots.inUse) whose
// holder's slot (issues.<KEY>.slot) stands idle since a moment at least after before at. It reads
// one rule with idleAt: a slot's state and time are the slots reading's (record.HolderStateOf). It
// refuses a body with no slots reading, one whose holders' slots are not as many as the slots in
// use, and a slot with no state, as a body narrowed for an older caller carries.
func IdleShareOfState(body []byte, at time.Time, after time.Duration) (float64, error) {
	var state struct {
		Admission struct {
			Slots *struct {
				InUse int `json:"inUse"`
			} `json:"slots"`
		} `json:"admission"`
		Issues map[string]struct {
			Slot *struct {
				State string     `json:"state"`
				Since *time.Time `json:"since"`
			} `json:"slot"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return 0, fmt.Errorf("replay: the state is no body of `legion state --json`: %w", err)
	}
	if state.Admission.Slots == nil {
		return 0, errors.New("replay: the state carries no slots reading (admission.slots)")
	}
	holders := 0
	var idle []time.Time
	for _, key := range slices.Sorted(maps.Keys(state.Issues)) {
		slot := state.Issues[key].Slot
		if slot == nil {
			continue
		}
		holders++
		switch {
		case slot.State == "":
			return 0, fmt.Errorf("replay: the slot of %s carries no state", key)
		case slot.State == string(record.HolderIdle) && slot.Since != nil:
			idle = append(idle, *slot.Since)
		}
	}
	if holders != state.Admission.Slots.InUse {
		return 0, fmt.Errorf("replay: the state gives %d holders a slot, and its slots reading %d slots in use", holders, state.Admission.Slots.InUse)
	}
	return signals.IdleShare(holders, idle, at, after), nil
}

// queueWaits is each wait the queue files' lines record, by the moment it began. It reads the
// events of every file together, the files in the order of their names, each by the repository its
// url names and its pull request: GitHub answers a renamed repository's former name with the
// repository's own events, so a window collected under both names holds some events in two files,
// and a pull request's events can be split between them. Package signals pairs them
// (signals.WaitsOf): each event id once, at the first line that carries it, and of each pull
// request's events in time order, the record's own order among those of one moment, a wait from
// each added_to_merge_queue of a pull request standing in no wait to its next merged or
// removed_from_merge_queue, so a drop and a re-add are two waits, and the removal GitHub records
// beside a merge ends nothing. It refuses a line ParseQueueLine refuses.
func queueWaits(queue map[string][]json.RawMessage) ([]signals.Wait, error) {
	events, err := queueEvents(queue)
	if err != nil {
		return nil, err
	}
	return signals.WaitsOf(events), nil
}

// queueEvents is each event the queue files' lines record, as package signals reads it: the files
// in the order of their names, each file's lines in its order, each event by the repository its url
// names and its pull request. queueWaits pairs them for the signal, and the merge queue's model
// pairs each pull request's own (queuePulls). It refuses a line ParseQueueLine refuses.
func queueEvents(queue map[string][]json.RawMessage) ([]signals.QueueEvent, error) {
	var events []signals.QueueEvent
	for _, file := range slices.Sorted(maps.Keys(queue)) {
		for i, line := range queue[file] {
			event, err := ParseQueueLine(line)
			if err != nil {
				return nil, fmt.Errorf("replay: line %d of the merge queue of %s: %w", i+1, file, err)
			}
			events = append(events, signals.QueueEvent{ID: event.ID, Repo: event.Repo, PullRequest: event.PullRequest, At: event.At,
				Enters: event.Event == queueAdded})
		}
	}
	return events, nil
}

// queueAt is the queue signal at at, in hours: the larger of the median wait of the project's pull
// requests that left the merge queue in the 6 hours before at, (at − 6h, at], and the wait so far
// of the one longest in the queue at at, 0 where none left and none waits.
func queueAt(r *SignalRecord, at time.Time) signals.Value {
	return signals.QueueAt(r.held(SignalQueue), r.waits, at)
}

// queueNext is the queue signal's next crossing of limit, in hours, after at: the waits standing as
// they do at at, the moment the wait of the pull request longest in the queue reaches the limit,
// the moment an exit leaves the 6 hours, or an edge of the record's span.
func queueNext(r *SignalRecord, at time.Time, limit float64) (time.Time, bool) {
	return signals.QueueNext(r.coverage(SignalQueue, at), r.waits, at, limit)
}

// series is the provider's minutes as the definitions read them: the minute that starts at start,
// whether the record holds it, and the limit of the project's own in force in it where the headroom
// is recorded there and one binds (Window.limitIn). The limit holds from one observation to the
// next (assumption 48, the architect's D13), as the record writes it, so a minute before the first
// observation, a minute of no limit (none, or 0, the limit taken away) and a minute of a limit of
// one model's own (any scope but the project's, projectLimitScope) hold no limit of the project's
// own: there the headroom reads the token limit the caller gives (assumption 49), and with none it
// holds no record and binds nothing. A minute outside the record's span holds no record of the
// headroom whatever the caller gives (assumption 47). The refusals of a minute are held wherever
// the record holds the minute, whatever the limit.
func (r *SignalRecord) series(start time.Time) (signals.Minute, bool) {
	line, held := r.minutes[start.Unix()]
	if !held {
		return signals.Minute{}, false
	}
	minute := signals.Minute{Uncached: line.Uncached, Own: line.Own, Provider: line.Provider}
	if limit, limited, _ := r.window.limitIn(line, r.tokenLimit); limited {
		minute.Limit = limit
	}
	return minute, true
}

// providerAt is the provider's headroom signal at at: the project's uncached input tokens in the
// minute before at over the limit in force in that minute, as the record writes the tokens, below
// zero too (series, signals.ProviderAt).
func providerAt(r *SignalRecord, at time.Time) signals.Value {
	return signals.ProviderAt(r.series, at)
}

// projectLimit is the limit of the project's own in force in m, and whether one is: a limit above
// 0 whose scope is the project's (projectLimitScope) or names none. None, a limit of 0 (the limit
// taken away) and a limit of one model's own are no limit on the project's tokens.
func (m ProviderMinute) projectLimit() (int64, bool) {
	if m.Limit == nil || *m.Limit <= 0 || m.LimitScope != nil && *m.LimitScope != projectLimitScope {
		return 0, false
	}
	return *m.Limit, true
}

// limitIn is the limit of the project's own in force in line's minute, where w holds a record of
// the provider's headroom there, and whether one is: the limit the record holds (projectLimit),
// else supplied, the project's token limit the caller gives (RunConfig.TokenLimit, assumption 49),
// where it is above zero; assumed says the limit is supplied. A minute outside the record's span
// holds none, whatever the caller gives (assumption 47).
func (w Window) limitIn(line ProviderMinute, supplied int64) (limit int64, limited, assumed bool) {
	if !w.Recorded(SignalProvider, line.Minute) {
		return 0, false, false
	}
	if limit, ok := line.projectLimit(); ok {
		return limit, true, false
	}
	return supplied, supplied > 0, supplied > 0
}

// tokenLimitBelowZero is the refusal of a token limit below zero (RunConfig.TokenLimit).
func tokenLimitBelowZero(limit int64) error {
	return fmt.Errorf("replay: the project's token limit is %d tokens a minute; give one above zero, or zero for none", limit)
}

// providerNext is the headroom's next crossing of limit after at: the first minute boundary at
// which the record's series, read as providerAt reads it, starts or stops binding, up to the
// boundary after the record's last minute.
func providerNext(r *SignalRecord, at time.Time, limit float64) (time.Time, bool) {
	return signals.ProviderNext(r.series, at, r.lastMinute.Add(2*time.Minute), limit)
}

// Refusals is the provider's refusals at at: those of each minute of the record that ended in the
// hour before at, (at − 1h, at], since a minute's refusals are known at its end. The record holds
// them where it holds the minute before at, whatever the limit.
func (r *SignalRecord) Refusals(at time.Time) signals.RefusalCount {
	return signals.RefusalsAt(r.series, at)
}

// NextRefusals is the next crossing after at of limit by the count of refusals, the refusals
// standing as they do at at: the moment an old refusal leaves the hour, or the record begins or
// stops holding the minute before. Refusals come only at events, so the count rises at none.
func (r *SignalRecord) NextRefusals(at time.Time, limit float64) (time.Time, bool) {
	return signals.RefusalsNext(r.series, at, limit, r.covered)
}

// killsAt is the kills signal at at: the count of the memory guard's kills of the project's issue
// pods in the hour before at, (at − 1h, at], one for each launcher line `launcher: memory guard:
// ended`, so a kill counts from its own moment to an hour later.
func killsAt(r *SignalRecord, at time.Time) signals.Value {
	return signals.KillsAt(r.held(SignalKills), r.kills, at)
}

// killsNext is the kills signal's next crossing of limit after at: the kills standing as they do
// at at, the moment an old kill leaves the hour, or an edge of the record's span. Kills come only
// at events, so the count rises at none.
func killsNext(r *SignalRecord, at time.Time, limit float64) (time.Time, bool) {
	return signals.KillsNext(r.coverage(SignalKills, at), r.kills, at, limit)
}

// approvalsAt is the approvals signal at at: the count of the asks open at at, approval requests
// among them, that wait on the project's default assignee then (waitingAsks). It changes only at
// events, so it has no next crossing.
func approvalsAt(r *SignalRecord, at time.Time) signals.Value {
	if !r.window.Recorded(SignalApprovals, at) {
		return signals.Value{}
	}
	return signals.Value{Recorded: true, Value: float64(len(r.waitingAsks(at)))}
}

// personAt is a person (a login, "" for nobody) from a moment on.
type personAt struct {
	at     time.Time
	person string
}

// personOn is the person of the latest of people at or before at, in time order, "" before the
// first.
func personOn(people []personAt, at time.Time) string {
	i := sort.Search(len(people), func(i int) bool { return people[i].at.After(at) })
	if i == 0 {
		return ""
	}
	return people[i-1].person
}

// askCourse is one ask of an issue of the project: its id, its issue, and each change of whether
// it waits on a person and whom it is asked of, in time order.
type askCourse struct {
	id, issue string
	changes   []askChange
}

// askChange is an ask from a moment on: whether it is open and waits on a person, and the person
// its asked_of names, "" for none.
type askChange struct {
	at          time.Time
	open, human bool
	askedOf     string
}

// waits says whether the ask is open and waits on a person.
func (c askChange) waits() bool { return c.open && c.human }

// waitingAsks is the id of each ask at at that is open and waits on a person, the person being the
// project's default assignee then (signals.WaitsOnDefault): an ask waits on its asked_of person,
// else its issue's assignee, else the project's default assignee at at, from the newest settings
// event before at that moves it, else window.json's defaultAssignee.
func (r *SignalRecord) waitingAsks(at time.Time) []string {
	owner := personOn(r.defaults, at)
	if owner == "" {
		owner = r.window.DefaultAssignee
	}
	var waiting []string
	for _, ask := range r.asks {
		i := sort.Search(len(ask.changes), func(i int) bool { return ask.changes[i].at.After(at) })
		if i == 0 || !ask.changes[i-1].waits() {
			continue
		}
		if signals.WaitsOnDefault(ask.changes[i-1].askedOf, personOn(r.assignees[ask.issue], at), owner) {
			waiting = append(waiting, ask.id)
		}
	}
	return waiting
}

// readAsks reads every ask of the project's issues from their Dispatch events, the assignee of
// each such issue, and each move of the project's default assignee. An ask's turn is package
// signals' (signals.NextAsk), which follows the daemon's own reading of Dispatch's events
// (intake's decodeDispatchEvent) and Dispatch's contract
// (packages/contracts/src/dispatch-api-events.ts and dispatch-api-asks.ts); this reads each event
// of an ask (ask.opened, ask.asked_of, ask.edited, ask.handed_back, ask.answered, ask.resolved,
// and a comment on an ask, comment.created and comment.answered, with its ask_waiting_on) into the
// normalized event it turns the ask by. asked_of, which every ask event's payload carries as the
// ask's row holds it, is named by the event or absent from it, and null names nobody.
//
// An issue's assignee is its issue events' assignee (issue.created, issue.updated, issue.closed).
// It refuses an event that is not one JSON object of an event, and a settings event
// ParseSettingsEvent refuses.
func (r *SignalRecord) readAsks() error {
	r.assignees = make(map[string][]personAt)
	byID := make(map[string]*askCourse)
	for _, key := range slices.Sorted(maps.Keys(r.window.Events)) {
		if !claim.KeyOfProject(r.window.Project, key) {
			continue
		}
		for i, raw := range r.window.Events[key] {
			if err := r.readAskEvent(key, raw, byID); err != nil {
				return fmt.Errorf("replay: event %d of %s: %w", i+1, key, err)
			}
		}
	}
	r.asks = slices.SortedFunc(maps.Values(byID), func(a, b *askCourse) int { return strings.Compare(a.id, b.id) })
	for i, raw := range r.window.Settings {
		event, err := ParseSettingsEvent(raw)
		if err != nil {
			return fmt.Errorf("replay: line %d of %s: %w", i+1, settingsFile, err)
		}
		if event.Type == defaultAssigneeEvent && event.Project == r.window.Project {
			r.defaults = append(r.defaults, personAt{at: event.At, person: event.DefaultAssignee})
		}
	}
	slices.SortStableFunc(r.defaults, func(a, b personAt) int { return a.at.Compare(b.at) })
	return nil
}

// readAskEvent reads one Dispatch event of key's issue into its ask's course or the issue's
// assignee, as readAsks says: the event's payload decoded into the normalized event package signals
// turns an ask by (approvalEventOf, signals.NextAsk), whose result is the ask's next change.
func (r *SignalRecord) readAskEvent(key string, raw json.RawMessage, byID map[string]*askCourse) error {
	var event struct {
		Type      string          `json:"type"`
		CreatedAt time.Time       `json:"created_at"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	read, err := approvalEventOf(event.Type, key, event.Payload)
	switch {
	case err != nil:
		return err
	case read.assigns:
		r.assignees[key] = append(r.assignees[key], personAt{at: event.CreatedAt, person: read.assignee})
	case read.asks && read.ask.Kind == signals.AskCommented:
		course, known := byID[read.ask.Ask]
		if !known {
			return nil
		}
		if next, changed := signals.NextAsk(course.last(), true, read.ask); changed {
			course.changes = append(course.changes, askChangeAt(event.CreatedAt, next))
		}
	case read.asks:
		course, known := byID[read.ask.Ask]
		if !known {
			course = &askCourse{id: read.ask.Ask, issue: key}
			byID[read.ask.Ask] = course
		}
		next, _ := signals.NextAsk(course.last(), known, read.ask)
		course.changes = append(course.changes, askChangeAt(event.CreatedAt, next))
	}
	return nil
}

// approvalEvent is what one Dispatch event of an issue says to the approvals signal
// (approvalEventOf): the event of an ask it makes, where asks says it makes one, and the issue's
// assignee it names, "" for null, where assigns says it names one.
type approvalEvent struct {
	ask      signals.AskEvent
	asks     bool
	assignee string
	assigns  bool
}

// approvalEventOf reads the Dispatch event of kind on key's issue, with its payload, as readAsks
// says: an issue event's assignee where its payload names one; a comment on an ask, with its
// ask_waiting_on; and an ask's event, with what its payload says of the ask's state, of an
// approval request's versions and of asked_of, null naming nobody. It refuses a payload that does
// not read, and an ask's event that names no ask.
func approvalEventOf(kind, key string, payload json.RawMessage) (approvalEvent, error) {
	switch kind {
	case "issue.created", "issue.updated", "issue.closed":
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil {
			return approvalEvent{}, err
		}
		member, named := fields["assignee"]
		if !named {
			return approvalEvent{}, nil
		}
		var assignee *string
		if err := json.Unmarshal(member, &assignee); err != nil {
			return approvalEvent{}, err
		}
		return approvalEvent{assignee: personOf(assignee), assigns: true}, nil
	case "comment.created", "comment.answered":
		var comment struct {
			Ask       string `json:"ask_id"`
			WaitingOn string `json:"ask_waiting_on"`
		}
		if err := json.Unmarshal(payload, &comment); err != nil {
			return approvalEvent{}, err
		}
		return approvalEvent{ask: signals.AskEvent{Kind: signals.AskCommented, Ask: comment.Ask, WaitingOn: signals.AskWaiting(comment.WaitingOn)},
			asks: comment.Ask != ""}, nil
	case "ask.opened", "ask.asked_of", "ask.edited", "ask.handed_back", "ask.answered", "ask.resolved":
		var ask struct {
			ID       string          `json:"id"`
			Kind     string          `json:"kind"`
			State    *string         `json:"state"`
			AskedOf  json.RawMessage `json:"asked_of"`
			Approval *struct {
				Version          *int `json:"version"`
				RequestedVersion *int `json:"requested_version"`
			} `json:"approval"`
		}
		if err := json.Unmarshal(payload, &ask); err != nil {
			return approvalEvent{}, err
		}
		if ask.ID == "" {
			return approvalEvent{}, fmt.Errorf("the %s event names no ask", kind)
		}
		turn := signals.AskEvent{Kind: signals.AskEventKind(strings.TrimPrefix(kind, "ask.")), Ask: ask.ID, Issue: key}
		if ask.State != nil {
			turn.State = signals.ReportsClosed
			if *ask.State == "open" {
				turn.State = signals.ReportsOpen
			}
		}
		if moved := ask.Approval; ask.Kind == record.AskApproval && moved != nil {
			turn.Approval = &signals.AskApproval{Version: moved.Version, RequestedVersion: moved.RequestedVersion}
		}
		if ask.AskedOf != nil {
			var person *string
			if err := json.Unmarshal(ask.AskedOf, &person); err != nil {
				return approvalEvent{}, err
			}
			turn.AskedOfSet, turn.AskedOf = true, personOf(person)
		}
		return approvalEvent{ask: turn, asks: true}, nil
	}
	return approvalEvent{}, nil
}

// last is the ask as its newest change leaves it, the zero ask where it has none.
func (c *askCourse) last() signals.Ask {
	if len(c.changes) == 0 {
		return signals.Ask{}
	}
	change := c.changes[len(c.changes)-1]
	return signals.Ask{ID: c.id, Issue: c.issue, Open: change.open, Human: change.human, AskedOf: change.askedOf}
}

// askChangeAt is ask, as an event at at leaves it, a change of its course.
func askChangeAt(at time.Time, ask signals.Ask) askChange {
	return askChange{at: at, open: ask.Open, human: ask.Human, askedOf: ask.AskedOf}
}

// personOf is the login person names, "" for null.
func personOf(person *string) string {
	if person == nil {
		return ""
	}
	return *person
}
