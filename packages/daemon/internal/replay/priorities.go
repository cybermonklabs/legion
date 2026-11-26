package replay

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// resolvedPriorities is the priority Dispatch resolves for each issue of a window's record at each
// moment (spec R10; Dispatch's J1), read once from the record's trace (resolvePriorities): an
// issue's own priority, else that of the nearest issue above it on its Dispatch parent chain that
// has one, in any project, up to ancestorLevels levels, else none. The measures read it under every
// rule (ranksOf, Timeline.readUrgency), so every rule is judged on one set of urgent issues and one
// banding of the line, and a rule with the new P0 start (record.SlotRule.StartsNow) reads it for
// its admissions, through the replay's copy of Dispatch (shadow.priorities) and its start state
// (Trace.shownPriorities). A recorded rule reads each issue's own, as its release did.
type resolvedPriorities struct {
	// courses is each issue's resolved priority from each moment on, in time order, from its first
	// issue event, a change only where the priority changes.
	courses map[string][]PriorityAt
	// lacking is each ancestor whose history the window lacks on which the priority of an issue of
	// the window's project rests at a moment from the window's start to its end, by its key, with
	// those issues: an input no comparison that reads the priority can do without (missingAncestors).
	lacking map[string][]string
	// inherited says an issue of the window's project takes, at such a moment, the priority of an
	// issue above it that the replay reconstructs, Dispatch having recorded none for it (assumption
	// 51, assumeResolvedFromChains).
	inherited bool
}

// priorityLink is an issue as the walk of a chain reads it at a moment: its own priority, its
// Dispatch parent ("" for none) and, from its first event that carries one, the priority Dispatch
// recorded it resolves to.
type priorityLink struct {
	own      record.Priority
	parent   string
	recorded bool
	resolved record.Priority
}

// priorityWalker is resolvePriorities' state as it reads a trace's issue events in time order: each
// issue's link as its events leave it, each issue's children by the parent its link names, and the
// priorities it writes.
type priorityWalker struct {
	t        Trace
	links    map[string]*priorityLink
	children map[string]map[string]struct{}
	started  map[string]bool
	p        resolvedPriorities
}

// resolvePriorities is the priority Dispatch resolves for each issue of t at each moment
// (resolvedPriorities). Where an issue event carries the value Dispatch resolved
// (DispatchIssueState.ShowsResolved), that value stands from the event on, a null one as none, and
// nothing of the issue is reconstructed after it, an ancestor's later change among it: Dispatch
// records that change on the issue itself. Before an issue's first such event, as in every event of
// a release before resolved_priority, its priority is reconstructed by J1's walk, which reads each
// issue above it at its own priority as its newest event before the moment shows it, and is read
// again at the window's start and at each change of a priority, a parent or a recorded value of the
// issue or of an issue above it, in any project: each issue up to ancestorLevels levels below an
// issue an event changes. An issue on a chain the window holds no issue event of is an ancestor
// whose history the window lacks: the walk that reaches it before a priority gives the issue none
// and names it (lacking) where the issue is of the window's project and the moment is in the window
// or its run-out. An issue reads as its first event shows it before that event, as its ranks do
// (ranksOf).
func resolvePriorities(t Trace) resolvedPriorities {
	type change struct {
		at    time.Time
		id    int64
		key   string
		state *DispatchIssueState
	}
	var changes []change
	w := priorityWalker{t: t, links: map[string]*priorityLink{}, children: map[string]map[string]struct{}{}, started: map[string]bool{},
		p: resolvedPriorities{courses: map[string][]PriorityAt{}, lacking: map[string][]string{}}}
	for key, iss := range t.Issues {
		for _, event := range iss.Dispatch {
			if event.Issue == nil {
				continue
			}
			changes = append(changes, change{at: event.At, id: event.ID, key: key, state: event.Issue})
			if _, linked := w.links[key]; !linked {
				w.links[key] = &priorityLink{own: event.Issue.Priority, parent: event.Issue.Parent}
				w.link(key, "", event.Issue.Parent)
			}
		}
	}
	slices.SortFunc(changes, func(a, b change) int { return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.id, b.id)) })
	atStart := false
	for i := 0; i < len(changes); {
		at := changes[i].at
		if !atStart && !at.Before(t.From) {
			w.settle(t.From, slices.Collect(maps.Keys(w.started)))
			atStart = true
		}
		var moved []string
		for ; i < len(changes) && changes[i].at.Equal(at); i++ {
			if c := changes[i]; w.apply(c.key, c.state) {
				moved = append(moved, c.key)
			}
		}
		w.settle(at, w.below(moved))
	}
	if !atStart {
		w.settle(t.From, slices.Collect(maps.Keys(w.started)))
	}
	for ancestor, issues := range w.p.lacking {
		slices.Sort(issues)
		w.p.lacking[ancestor] = slices.Compact(issues)
	}
	return w.p
}

// apply applies one issue event of key, which shows the issue as state, to its link, and says
// whether the event is its first or changes what the walk reads of it: its own priority, its parent
// or the value Dispatch recorded for it. A recorded value stays where a later event carries none.
func (w *priorityWalker) apply(key string, state *DispatchIssueState) bool {
	link := w.links[key]
	before := *link
	link.own = state.Priority
	if state.Parent != link.parent {
		w.link(key, link.parent, state.Parent)
		link.parent = state.Parent
	}
	if state.ShowsResolved {
		link.recorded, link.resolved = true, state.Resolved
	}
	first := !w.started[key]
	w.started[key] = true
	return first || *link != before
}

// link moves key from among the children of from to those of to ("" for none).
func (w *priorityWalker) link(key, from, to string) {
	delete(w.children[from], key)
	if to == "" {
		return
	}
	if w.children[to] == nil {
		w.children[to] = map[string]struct{}{}
	}
	w.children[to][key] = struct{}{}
}

// below is each of keys and each issue up to ancestorLevels levels below one of them, by the
// parents the links name: the issues whose walk reads one of keys.
func (w *priorityWalker) below(keys []string) []string {
	seen := map[string]bool{}
	level := keys
	for depth := 0; depth <= ancestorLevels && len(level) > 0; depth++ {
		var next []string
		for _, key := range level {
			if seen[key] {
				continue
			}
			seen[key] = true
			for child := range w.children[key] {
				next = append(next, child)
			}
		}
		level = next
	}
	return slices.Sorted(maps.Keys(seen))
}

// settle reads again, at at, the priority of each issue of keys whose first event the walker has
// applied (resolve), and writes it where it changes; an issue of the window's project whose walk
// reaches an ancestor the window lacks, or takes a reconstructed priority from an issue above it,
// is noted where at falls in the window or its run-out.
func (w *priorityWalker) settle(at time.Time, keys []string) {
	end := cmp.Or(w.t.End, w.t.To)
	counts := !at.Before(w.t.From) && at.Before(end)
	for _, key := range keys {
		if !w.started[key] {
			continue
		}
		priority, missing, inherited := w.resolve(key)
		w.p.note(key, at, priority)
		if iss := w.t.Issues[key]; !counts || iss == nil || iss.Project != w.t.Project {
			continue
		}
		if missing != "" {
			w.p.lacking[missing] = append(w.p.lacking[missing], key)
		}
		w.p.inherited = w.p.inherited || inherited
	}
}

// resolve is key's priority as its link stands (resolvePriorities): the value Dispatch recorded
// for it, where it recorded one; else its own; else the own priority of the nearest issue above it
// that has one, up to ancestorLevels levels, with inherited set; else none. missing is the ancestor
// the walk reached with no link, whose history the window lacks, which gives none.
func (w *priorityWalker) resolve(key string) (priority record.Priority, missing string, inherited bool) {
	link := w.links[key]
	if link.recorded {
		return link.resolved, "", false
	}
	for depth := 0; ; depth++ {
		if link.own != record.NoPriority {
			return link.own, "", depth > 0
		}
		if link.parent == "" || depth == ancestorLevels {
			return record.NoPriority, "", false
		}
		above, held := w.links[link.parent]
		if !held {
			return record.NoPriority, link.parent, false
		}
		link = above
	}
}

// note writes that key resolves to priority from at: a change of its course where it changes, in
// place of a change at the same moment.
func (p *resolvedPriorities) note(key string, at time.Time, priority record.Priority) {
	course := p.courses[key]
	n := len(course)
	switch {
	case n > 0 && course[n-1].Priority == priority:
	case n > 0 && course[n-1].At.Equal(at):
		course[n-1].Priority = priority
		if n > 1 && course[n-2].Priority == priority {
			course = course[:n-1]
		}
	default:
		course = append(course, PriorityAt{At: at, Priority: priority})
	}
	p.courses[key] = course
}

// at is key's resolved priority at at, once every event at at applied: that of the newest change
// of its course at or before at, or of its first for a moment before every one; none for an issue
// the window holds no issue event of.
func (p resolvedPriorities) at(key string, at time.Time) record.Priority {
	course := p.courses[key]
	if len(course) == 0 {
		return record.NoPriority
	}
	i := sort.Search(len(course), func(i int) bool { return course[i].At.After(at) })
	return course[max(i, 1)-1].Priority
}

// before is key's resolved priority just before at, before any event at at applied: the start
// state's (Trace.shownPriorities), and what the replay's copy of Dispatch shows at the window's
// start.
func (p resolvedPriorities) before(key string, at time.Time) record.Priority {
	course := p.courses[key]
	if len(course) == 0 {
		return record.NoPriority
	}
	i := sort.Search(len(course), func(i int) bool { return !course[i].At.Before(at) })
	return course[max(i, 1)-1].Priority
}

// from is key's resolved priority from since on: the priority at since, then each change after it,
// in time order (Course.Priorities).
func (p resolvedPriorities) from(key string, since time.Time) []PriorityAt {
	course := p.courses[key]
	i := sort.Search(len(course), func(i int) bool { return course[i].At.After(since) })
	return append([]PriorityAt{{At: since, Priority: p.at(key, since)}}, course[i:]...)
}

// missingAncestors is each ancestor whose history the window lacks on which the priority of an
// issue of the window's project rests (lacking), sorted, and empty, never nil, for none: the
// missing input a run names (RunResult.MissingAncestors) and a window's record names
// (windowRecord.MissingAncestors).
func (p resolvedPriorities) missingAncestors() []string {
	missing := slices.AppendSeq(make([]string, 0, len(p.lacking)), maps.Keys(p.lacking))
	slices.Sort(missing)
	return missing
}

// missingText is each missing ancestor with the issues whose priority rests on it, as a refusal or
// a report names the missing input: "ACMEAUX-1, above ACME-2 and ACME-3; ACMEAUX-4, above ACME-5".
func (p resolvedPriorities) missingText() string {
	named := make([]string, 0, len(p.lacking))
	for _, ancestor := range p.missingAncestors() {
		named = append(named, fmt.Sprintf("%s, above %s", ancestor, keyAnd(p.lacking[ancestor])))
	}
	return strings.Join(named, "; ")
}

// keyAnd is keys as a sentence lists them: "A", "A and B", "A, B and C".
func keyAnd(keys []string) string {
	if len(keys) < 2 {
		return strings.Join(keys, "")
	}
	return strings.Join(keys[:len(keys)-1], ", ") + " and " + keys[len(keys)-1]
}

// refuseMissing is the refusal of a replay under rule of a window whose priorities are p, where the
// rule starts a P0 issue outside the cap (record.SlotRule.StartsNow), which orders its line and
// counts its holders by the priority Dispatch resolves too, and the window lacks the history of an
// ancestor on which that priority rests (spec R10: "If the record lacks a required ancestor's
// history, report the missing input and make no comparison that depends on it"): the refusal names
// each such ancestor with the issues resting on it, and says to collect the window again with
// their history. Nil where the rule reads no resolved priority or the window lacks no ancestor: a
// recorded rule still runs on such a window.
func (p resolvedPriorities) refuseMissing(rule record.SlotRule) error {
	if rule = rule.OrMain(); !rule.StartsNow || len(p.lacking) == 0 {
		return nil
	}
	return fmt.Errorf("replay: the rule %s starts an issue outside the cap, orders its line and counts its holders by the priority Dispatch resolves for each issue, "+
		"and the window lacks the history of the Dispatch ancestors that priority rests on: %s; no replay of the window under %s is made; "+
		"collect the window again with the history of those ancestors (spec R10)",
		rule.Name, p.missingText(), rule.Name)
}

// urgentMeasure is the name of the wait of urgent work among the measures (Scorecard.UrgentWaits).
const urgentMeasure = "urgentWaits"

// restsOnResolved says whether the measure name, as MeasuresOf and costMeasures name a measure,
// rests on the priority Dispatch resolves for each issue: priority taken in today's line's order,
// of the window or of its first day, and the wait of urgent work.
func restsOnResolved(name string) bool {
	name = strings.TrimPrefix(name, firstDayPrefix)
	return name == "priorityTakenBanded" || name == urgentMeasure || strings.HasPrefix(name, urgentMeasure+"[")
}

// gives says whether the window r gives the measure name: every measure but one that rests on the
// priority Dispatch resolves (restsOnResolved) where the window lacks the history of an ancestor
// such a priority rests on (MissingAncestors), which a validation of the window under a recorded
// rule leaves out (Runs.Validation, spec R10).
func (r windowRecord) gives(name string) bool {
	return len(r.MissingAncestors) == 0 || !restsOnResolved(name)
}

// MissingAncestorsLine is the sentence a report gives in place of the measures of window that rest
// on the priority Dispatch resolves, which the window cannot give since it lacks the history of the
// ancestors missing (RunResult.MissingAncestors, ScoreRecord).
func MissingAncestorsLine(window string, missing []string) string {
	return fmt.Sprintf("%s lacks the history of the Dispatch ancestors %s, on which the priority Dispatch resolves for an issue of it rests, "+
		"so it gives no priority taken in today's line's order (`priorityTakenBanded`) and no wait of urgent work (`urgentWaits`), "+
		"and no rule is judged on either there (spec R10).", window, strings.Join(missing, ", "))
}

// MeasuresRead is each measure of card (MeasuresOf) a reader prints of a window that lacks the
// history of the ancestors missing: every one, but, where missing names any, none that rests on
// the priority Dispatch resolves (restsOnResolved), which the window cannot give.
func MeasuresRead(card Scorecard, missing []string) []Measured {
	measured := MeasuresOf(card)
	if len(missing) == 0 {
		return measured
	}
	return slices.DeleteFunc(measured, func(m Measured) bool { return restsOnResolved(m.Name) })
}
