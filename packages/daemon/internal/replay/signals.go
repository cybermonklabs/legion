package replay

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The definitions of R11's signals (spec R11): each signal of a collected window at a moment t,
// read from the window's own record (SignalRecord), lower being better for each, and, for each
// signal that can cross a limit with no event, the next moment after t at which it would
// (SignalDefinition.Next). The docs page gives each a section of its own (replay.md, "The
// signals").

// DefaultIdleAfter is d, how long a slot holder has been idle before the idle signal counts it:
// an hour, as R11 gives it. The comparison also reads d at 10 minutes and at 4 hours.
const DefaultIdleAfter = time.Hour

// The spans a signal counts its events over: the merge queue's exits over the 6 hours before a
// moment, and the memory guard's kills and the provider's refusals over the hour before it.
const (
	queueExitSpan = 6 * time.Hour
	killSpan      = time.Hour
	refusalSpan   = time.Hour
)

// projectLimitScope is the scope the provider's record names the project's own limit by: its
// limit over all its models, as t25's provider-record script writes it (ProviderMinute.LimitScope).
// A limit of another scope is one model's own, which is no limit on the project's tokens.
const projectLimitScope = "Claude"

// SignalValue is one signal at a moment: its value, lower being better for every signal, and
// whether the window holds a record of it there. A signal with no record is unrecorded, never
// zero, and binds no rule (assumption 47).
type SignalValue struct {
	Recorded bool
	Value    float64
}

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
	At        func(r *SignalRecord, at time.Time) SignalValue
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
	{Name: SignalKills, At: killsAt, Next: killsNext, Inputs: []string{launcherFile, memoryFile}, Source: "memory model"},
	{Name: SignalApprovals, At: approvalsAt, EventOnly: true,
		Inputs: []string{eventsDir + "/<KEY>.jsonl", settingsFile, windowFile + "#defaultAssignee"}, Source: "replay"},
}

// SignalDefinitions is every signal of R11, one definition each, in the order of R11's list.
func SignalDefinitions() []SignalDefinition { return slices.Clone(signalDefinitions) }

// SignalRecord is a window's record read for R11's signals (ReadSignalRecord): the slot holders
// of its trace, the memory guard's kills, the merge queue's waits, the provider's minutes, and
// each ask with the people it can wait on, so that each definition reads its signal at any moment.
type SignalRecord struct {
	window    Window
	idleAfter time.Duration

	// holders is the index of each issue of the window's project, which the idle signal reads
	// the slot holders and their states from.
	holders []*issueIndex
	// kills is the moment of each kill the launcher's lines record, in order.
	kills []time.Time
	// waits is each wait of a pull request in the merge queue, by the moment it began.
	waits []queueWait
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
// idle before the idle signal counts it. It refuses a d below zero, a window whose trace does not
// build (BuildTrace), and a line of a signal's input that its definition cannot read.
func ReadSignalRecord(w Window, idleAfter time.Duration) (*SignalRecord, error) {
	if idleAfter < 0 {
		return nil, fmt.Errorf("replay: a holder idle for %s: d is below zero", idleAfter)
	}
	trace, err := BuildTrace(w)
	if err != nil {
		return nil, err
	}
	r := &SignalRecord{window: w, idleAfter: idleAfter, minutes: make(map[int64]ProviderMinute, len(w.Provider))}
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
	var edges []time.Time
	for _, span := range r.window.Signals[signal] {
		for _, edge := range []time.Time{span.From, span.To} {
			if edge.After(at) {
				edges = append(edges, edge)
			}
		}
	}
	return edges
}

// firstChange is the first of moments after at at which binds answers otherwise than at at, and
// whether one does.
func firstChange(at time.Time, moments []time.Time, binds func(time.Time) bool) (time.Time, bool) {
	slices.SortFunc(moments, time.Time.Compare)
	now := binds(at)
	for _, moment := range slices.CompactFunc(moments, time.Time.Equal) {
		if moment.After(at) && binds(moment) != now {
			return moment, true
		}
	}
	return time.Time{}, false
}

// idleAt is the idle signal at at: the share of the slot holders whose state (R6's, as the slots
// reading names it: record.HolderStateOf) is idle with its time at least d before at, from the
// daemon's slot lines and the `supervise:` lines of each turn's start and end, as the window's
// trace reads them. A holder's idle time is the later of the newest end of a turn of its claims
// and the moment it took the slot it holds. A moment with no holder has a share of 0.
func idleAt(r *SignalRecord, at time.Time) SignalValue {
	if !r.window.Recorded(SignalIdle, at) {
		return SignalValue{}
	}
	holders, idle := r.idleHolders(at)
	return SignalValue{Recorded: true, Value: idleShare(holders, idle, at, r.idleAfter)}
}

// idleNext is the idle signal's next crossing of limit after at: the holders standing as they do
// at at, the moment a holder idle since s reaches s + d, or an edge of the record's span. A holder
// leaves idle only at an event, so the signal falls at none, and a crossing downward is due only
// where the record ends.
func idleNext(r *SignalRecord, at time.Time, limit float64) (time.Time, bool) {
	holders, idle := r.idleHolders(at)
	moments := r.spanEdges(SignalIdle, at)
	for _, since := range idle {
		moments = append(moments, since.Add(r.idleAfter))
	}
	return firstChange(at, moments, func(t time.Time) bool {
		return r.window.Recorded(SignalIdle, t) && idleShare(holders, idle, t, r.idleAfter) >= limit
	})
}

// idleShare is the share of holders holders of which idle are idle since a moment at least after
// before at, 0 where there is no holder.
func idleShare(holders int, idle []time.Time, at time.Time, after time.Duration) float64 {
	if holders == 0 {
		return 0
	}
	long := 0
	for _, since := range idle {
		if !since.Add(after).After(at) {
			long++
		}
	}
	return float64(long) / float64(holders)
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
	return idleShare(holders, idle, at, after), nil
}

// queueWait is one wait of a pull request in the merge queue: from its added_to_merge_queue to the
// merged or removed_from_merge_queue that ends it, its left zero while it stands at the record's
// end.
type queueWait struct {
	added, left time.Time
}

// queueWaits is each wait the queue files' lines record, by the moment it began. It reads the
// events of every file together, each by the repository its url names and its pull request, and
// each event id once, at the first line that carries it, the files in the order of their names:
// GitHub answers a renamed repository's former name with the repository's own events, so a window
// collected under both names holds some events in two files, and a pull request's events can be
// split between them. Of each pull request's events in time order, the record's own order among
// those of one moment, it makes a wait from each added_to_merge_queue of a pull request standing
// in no wait to its next merged or removed_from_merge_queue, so a drop and a re-add are two waits,
// and the removal GitHub records beside a merge ends nothing. It refuses a line ParseQueueLine
// refuses.
func queueWaits(queue map[string][]json.RawMessage) ([]queueWait, error) {
	type pullRequest struct {
		repo   string
		number int
	}
	byPull := make(map[pullRequest][]QueueEvent)
	seen := make(map[int64]bool)
	for _, file := range slices.Sorted(maps.Keys(queue)) {
		for i, line := range queue[file] {
			event, err := ParseQueueLine(line)
			if err != nil {
				return nil, fmt.Errorf("replay: line %d of the merge queue of %s: %w", i+1, file, err)
			}
			if seen[event.ID] {
				continue
			}
			seen[event.ID] = true
			pull := pullRequest{repo: event.Repo, number: event.PullRequest}
			byPull[pull] = append(byPull[pull], event)
		}
	}
	var waits []queueWait
	for _, pull := range slices.SortedFunc(maps.Keys(byPull), func(a, b pullRequest) int {
		return cmp.Or(strings.Compare(a.repo, b.repo), cmp.Compare(a.number, b.number))
	}) {
		events := byPull[pull]
		slices.SortStableFunc(events, func(a, b QueueEvent) int { return a.At.Compare(b.At) })
		open := -1
		for _, event := range events {
			switch {
			case event.Event == queueAdded && open < 0:
				waits = append(waits, queueWait{added: event.At})
				open = len(waits) - 1
			case event.Event != queueAdded && open >= 0:
				waits[open].left = event.At
				open = -1
			}
		}
	}
	slices.SortStableFunc(waits, func(a, b queueWait) int { return a.added.Compare(b.added) })
	return waits, nil
}

// queueAt is the queue signal at at, in hours: the larger of the median wait of the project's pull
// requests that left the merge queue in the 6 hours before at, (at − 6h, at], and the wait so far
// of the one longest in the queue at at, 0 where none left and none waits.
func queueAt(r *SignalRecord, at time.Time) SignalValue {
	if !r.window.Recorded(SignalQueue, at) {
		return SignalValue{}
	}
	return SignalValue{Recorded: true, Value: r.queueWait(at, at).Hours()}
}

// queueNext is the queue signal's next crossing of limit, in hours, after at: the waits standing as
// they do at at, the moment the wait of the pull request longest in the queue reaches the limit,
// the moment an exit leaves the 6 hours, or an edge of the record's span.
func queueNext(r *SignalRecord, at time.Time, limit float64) (time.Time, bool) {
	reach := time.Duration(math.Ceil(limit * float64(time.Hour)))
	moments := r.spanEdges(SignalQueue, at)
	for _, wait := range r.waits {
		switch {
		case wait.added.After(at):
		case wait.left.IsZero() || wait.left.After(at):
			moments = append(moments, wait.added.Add(reach))
		case wait.left.After(at.Add(-queueExitSpan)):
			moments = append(moments, wait.left.Add(queueExitSpan))
		}
	}
	return firstChange(at, moments, func(t time.Time) bool {
		return r.window.Recorded(SignalQueue, t) && r.queueWait(t, at) >= reach
	})
}

// queueWait is the queue signal at at as the record stands at known, at or before at: the waits
// begun and the exits made by known, read at at.
func (r *SignalRecord) queueWait(at, known time.Time) time.Duration {
	var exits []time.Duration
	var longest time.Duration
	for _, wait := range r.waits {
		switch {
		case wait.added.After(known):
		case wait.left.IsZero() || wait.left.After(known):
			longest = max(longest, at.Sub(wait.added))
		case wait.left.After(at.Add(-queueExitSpan)):
			exits = append(exits, wait.left.Sub(wait.added))
		}
	}
	return max(medianDuration(exits), longest)
}

// medianDuration is the median of waits, the mean of the middle two of an even number, 0 for none.
func medianDuration(waits []time.Duration) time.Duration {
	if len(waits) == 0 {
		return 0
	}
	slices.Sort(waits)
	middle := len(waits) / 2
	if len(waits)%2 == 1 {
		return waits[middle]
	}
	return (waits[middle-1] + waits[middle]) / 2
}

// minuteBefore is the line of the provider's record of the minute before at: the last whole
// minute that ends at or before at, and whether the record holds it.
func (r *SignalRecord) minuteBefore(at time.Time) (ProviderMinute, bool) {
	minute, held := r.minutes[at.Truncate(time.Minute).Add(-time.Minute).Unix()]
	return minute, held
}

// providerAt is the provider's headroom signal at at: the project's uncached input tokens in the
// minute before at over the limit in force in that minute, as the record writes the tokens, below
// zero too. The limit holds from one observation to the next (assumption 48, the architect's
// D13), as the record writes it, so a minute before the first observation, a minute of no limit
// (none, or 0, the limit taken away) and a minute of a limit of one model's own (any scope but
// the project's, projectLimitScope) hold no record of the headroom, as does a minute outside the
// record's span: there the headroom binds nothing (assumption 47).
func providerAt(r *SignalRecord, at time.Time) SignalValue {
	minute, held := r.minuteBefore(at)
	limit, limited := minute.projectLimit()
	if !held || !r.window.Recorded(SignalProvider, minute.Minute) || !limited {
		return SignalValue{}
	}
	return SignalValue{Recorded: true, Value: float64(minute.Uncached) / float64(limit)}
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

// providerNext is the headroom's next crossing of limit after at: the first minute boundary at
// which the record's series, read as providerAt reads it, starts or stops binding, up to the
// boundary after the record's last minute.
func providerNext(r *SignalRecord, at time.Time, limit float64) (time.Time, bool) {
	binds := func(t time.Time) bool {
		value := providerAt(r, t)
		return value.Recorded && value.Value >= limit
	}
	now := binds(at)
	for boundary := at.Truncate(time.Minute).Add(time.Minute); !boundary.After(r.lastMinute.Add(2 * time.Minute)); boundary = boundary.Add(time.Minute) {
		if binds(boundary) != now {
			return boundary, true
		}
	}
	return time.Time{}, false
}

// Refusals is the refusals part of the provider's signal at a moment (SignalRecord.Refusals): the
// refusals of the hour before it, each by the limit that gave it, the project's own (Own) or the
// provider's (Provider), as the record splits them (assumption 46), and whether the record holds
// the minute before it.
type Refusals struct {
	Recorded      bool
	Own, Provider int64
}

// Count is every refusal r counts, of either limit: the value a limit of the refusals reads.
func (r Refusals) Count() int64 { return r.Own + r.Provider }

// Refusals is the provider's refusals at at: those of each minute of the record that ended in the
// hour before at, (at − 1h, at], since a minute's refusals are known at its end. The record holds
// them where it holds the minute before at, whatever the limit.
func (r *SignalRecord) Refusals(at time.Time) Refusals {
	return r.refusals(at, at)
}

// NextRefusals is the next crossing after at of limit by the count of refusals, the refusals
// standing as they do at at: the moment an old refusal leaves the hour, or the record begins or
// stops holding the minute before. Refusals come only at events, so the count rises at none.
func (r *SignalRecord) NextRefusals(at time.Time, limit float64) (time.Time, bool) {
	moments := slices.Clone(r.covered)
	for minute := at.Truncate(time.Minute).Add(-refusalSpan); minute.Before(at.Truncate(time.Minute)); minute = minute.Add(time.Minute) {
		if line, held := r.minutes[minute.Unix()]; held && line.Own+line.Provider > 0 {
			moments = append(moments, minute.Add(time.Minute+refusalSpan))
		}
	}
	return firstChange(at, moments, func(t time.Time) bool {
		counted := r.refusals(t, at)
		return counted.Recorded && float64(counted.Count()) >= limit
	})
}

// refusals is the refusals at at as the record stands at known, at or before at: those of the
// minutes that ended by known.
func (r *SignalRecord) refusals(at, known time.Time) Refusals {
	if _, held := r.minuteBefore(at); !held {
		return Refusals{}
	}
	counted := Refusals{Recorded: true}
	end := minTime(at, known).Truncate(time.Minute)
	for minute := at.Truncate(time.Minute).Add(-refusalSpan); minute.Before(end); minute = minute.Add(time.Minute) {
		if line, held := r.minutes[minute.Unix()]; held {
			counted.Own += line.Own
			counted.Provider += line.Provider
		}
	}
	return counted
}

// killsAt is the kills signal at at: the count of the memory guard's kills of the project's issue
// pods in the hour before at, (at − 1h, at], one for each launcher line `launcher: memory guard:
// ended`, so a kill counts from its own moment to an hour later.
func killsAt(r *SignalRecord, at time.Time) SignalValue {
	if !r.window.Recorded(SignalKills, at) {
		return SignalValue{}
	}
	return SignalValue{Recorded: true, Value: float64(r.killCount(at, at))}
}

// killsNext is the kills signal's next crossing of limit after at: the kills standing as they do
// at at, the moment an old kill leaves the hour, or an edge of the record's span. Kills come only
// at events, so the count rises at none.
func killsNext(r *SignalRecord, at time.Time, limit float64) (time.Time, bool) {
	moments := r.spanEdges(SignalKills, at)
	for _, kill := range r.kills[upTo(r.kills, at.Add(-killSpan)):upTo(r.kills, at)] {
		moments = append(moments, kill.Add(killSpan))
	}
	return firstChange(at, moments, func(t time.Time) bool {
		return r.window.Recorded(SignalKills, t) && float64(r.killCount(t, at)) >= limit
	})
}

// killCount is the kills of the hour before at that the record holds by known, at or before at.
func (r *SignalRecord) killCount(at, known time.Time) int {
	return max(upTo(r.kills, minTime(at, known))-upTo(r.kills, at.Add(-killSpan)), 0)
}

// approvalsAt is the approvals signal at at: the count of the asks open at at, approval requests
// among them, that wait on the project's default assignee then (waitingAsks). It changes only at
// events, so it has no next crossing.
func approvalsAt(r *SignalRecord, at time.Time) SignalValue {
	if !r.window.Recorded(SignalApprovals, at) {
		return SignalValue{}
	}
	return SignalValue{Recorded: true, Value: float64(len(r.waitingAsks(at)))}
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
// project's default assignee then: an ask waits on its asked_of person, else its issue's assignee,
// else the project's default assignee at at, from the newest settings event before at that moves
// it, else window.json's defaultAssignee.
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
		person := ask.changes[i-1].askedOf
		if person == "" {
			person = personOn(r.assignees[ask.issue], at)
		}
		if person == "" {
			person = owner
		}
		if owner != "" && person == owner {
			waiting = append(waiting, ask.id)
		}
	}
	return waiting
}

// readAsks reads every ask of the project's issues from their Dispatch events, the assignee of
// each such issue, and each move of the project's default assignee. An ask's turn follows the
// daemon's own reading of Dispatch's events (intake's decodeDispatchEvent) and Dispatch's
// contract (packages/contracts/src/dispatch-api-events.ts and dispatch-api-asks.ts):
//
//   - ask.opened opens the ask waiting on a person, and any ask event whose payload's state is
//     open reopens a closed one so (a re-ask, or a restored one); ask.answered and ask.resolved
//     close it, a decline among them (resolution.kind declined);
//   - an ask.edited that moves an approval request, its requested_version below its version,
//     leaves it waiting on its agent, and any other edit leaves its turn as it was; its
//     ask.handed_back, the agent handing it back to the person, leaves it waiting on the person;
//   - a comment on an open ask (comment.created, comment.answered) leaves it waiting on whom its
//     ask_waiting_on names: agent after a person's reply, human after an agent's;
//   - asked_of, which every ask event's payload carries as the ask's row holds it (ask.opened,
//     ask.asked_of, ask.edited among them), names the person it is asked of, null for none.
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
// assignee, as readAsks says.
func (r *SignalRecord) readAskEvent(key string, raw json.RawMessage, byID map[string]*askCourse) error {
	var event struct {
		Type      string          `json:"type"`
		CreatedAt time.Time       `json:"created_at"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	switch event.Type {
	case "issue.created", "issue.updated", "issue.closed":
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		if member, named := payload["assignee"]; named {
			var assignee *string
			if err := json.Unmarshal(member, &assignee); err != nil {
				return err
			}
			r.assignees[key] = append(r.assignees[key], personAt{at: event.CreatedAt, person: personOf(assignee)})
		}
	case "comment.created", "comment.answered":
		var payload struct {
			Ask       string `json:"ask_id"`
			WaitingOn string `json:"ask_waiting_on"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		ask, known := byID[payload.Ask]
		if !known || payload.WaitingOn != "human" && payload.WaitingOn != "agent" {
			return nil
		}
		last := ask.changes[len(ask.changes)-1]
		ask.changes = append(ask.changes, askChange{at: event.CreatedAt, open: last.open, human: payload.WaitingOn == "human", askedOf: last.askedOf})
	case "ask.opened", "ask.asked_of", "ask.edited", "ask.handed_back", "ask.answered", "ask.resolved":
		var payload struct {
			ID       string          `json:"id"`
			Kind     string          `json:"kind"`
			State    *string         `json:"state"`
			AskedOf  json.RawMessage `json:"asked_of"`
			Approval *struct {
				Version          *int `json:"version"`
				RequestedVersion *int `json:"requested_version"`
			} `json:"approval"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		if payload.ID == "" {
			return fmt.Errorf("the %s event names no ask", event.Type)
		}
		ask, known := byID[payload.ID]
		if !known {
			ask = &askCourse{id: payload.ID, issue: key}
			byID[payload.ID] = ask
		}
		var last askChange
		if n := len(ask.changes); n > 0 {
			last = ask.changes[n-1]
		}
		open, human := last.open, last.human
		switch event.Type {
		case "ask.opened":
			open, human = true, true
		case "ask.handed_back":
			human = true
		case "ask.edited":
			if moved := payload.Approval; payload.Kind == record.AskApproval && moved != nil && moved.Version != nil &&
				moved.RequestedVersion != nil && *moved.RequestedVersion < *moved.Version {
				human = false
			}
		case "ask.answered", "ask.resolved":
			open = false
		}
		if payload.State != nil {
			open = *payload.State == "open"
		}
		if open && !last.open {
			human = true
		}
		askedOf := last.askedOf
		if payload.AskedOf != nil {
			var person *string
			if err := json.Unmarshal(payload.AskedOf, &person); err != nil {
				return err
			}
			askedOf = personOf(person)
		}
		ask.changes = append(ask.changes, askChange{at: event.CreatedAt, open: open, human: human, askedOf: askedOf})
	}
	return nil
}

// personOf is the login person names, "" for null.
func personOf(person *string) string {
	if person == nil {
		return ""
	}
	return *person
}
