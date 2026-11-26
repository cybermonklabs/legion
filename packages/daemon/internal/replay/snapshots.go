package replay

import (
	"maps"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// snapshots builds a replay's snapshot timeline (design 10.1 (a)): at each instant the replay
// settles, each issue's state as the newest admission pass left it (admit.PassView, handed over
// by the pass's observer), the claims the worker stub holds and the issues the replay holds in
// flight; and each issue's slots as the passes gave them, its moves as the engine wrote them and
// its waits on other issues as the passes judged them. It reads nothing of its own: every part is
// what the daemon's code decided from, or the stub's own record of the claims it wrote.
type snapshots struct {
	timeline Timeline
	ranks    rankFunc
	// view is the newest pass's view, nil before the first; fresh says it came after the last
	// settle. byKey, slotOf, line, heldBack, blocked and held are what a settle reads of it, read
	// once for each view (read).
	view     *admit.PassView
	fresh    bool
	byKey    map[string]record.Issue
	slotOf   map[string]record.Slot
	line     map[string]record.LineEntry
	heldBack map[string]bool
	blocked  map[string]bool
	held     map[string]bool
	// given is each slot the passes gave out since the last settle, waits each issue's wait on
	// other issues that stands, by key, and live each issue whose last state is not the zero
	// state.
	given []record.SlotGiven
	waits map[string]*standingWait
	live  map[string]bool
	// records is the daemon's newest record of every issue it has recorded, by key: each the start
	// state wrote (seeded), and each a view has held since (read).
	records map[string]record.Issue
	// firstStartHeld says a pass found an issue not yet started held by a hold other than the cap
	// (assumption 7): a link, a design gate, an ancestor, a tree.
	firstStartHeld bool
	// keys, working and byIssue are what a settle gathers of the instant, kept from one settle
	// to the next so a settle makes none of them anew: byIssue keeps a holder's slice of claims,
	// emptied, for as long as it holds its slot.
	keys    map[string]struct{}
	working map[string]bool
	byIssue map[string][]supervise.Claim
	// holders is each slot of the newest view held by an issue of the project, read once for each
	// view (idleHolders); nil until a reading asks for it.
	holders []heldSlot
}

// standingWait is a wait of an issue on other issues that stands: when it began, and the blockers
// and open children it waited on at the last settle.
type standingWait struct {
	start           time.Time
	links, children []string
}

// firstStartHolds is the held words (record.HeldReason) of a hold other than the cap that holds
// the first start of an issue not yet started (assumption 7).
var firstStartHolds = []record.HeldReason{record.HeldTreeClosed, record.HeldTreeParked, record.HeldAncestry,
	record.HeldRootAwaitingPerson, record.HeldDesignGate, record.HeldDispatchBlocker, record.HeldChildrenFirst,
	record.HeldAcceptedAncestor}

// newSnapshots is the snapshot timeline of a replay of [from, to) that runs to end, its ranks and
// priorities read by ranks.
func newSnapshots(from, to, end time.Time, ranks rankFunc) *snapshots {
	return &snapshots{timeline: Timeline{From: from, To: to, End: end, Issues: map[string]*Course{}}, ranks: ranks,
		waits: map[string]*standingWait{}, live: map[string]bool{}, records: map[string]record.Issue{}, keys: map[string]struct{}{},
		working: map[string]bool{}, byIssue: map[string][]supervise.Claim{}}
}

// seeded takes the records the start state wrote, the issues that left the workflow before the
// window's start among them, which no pass's view holds.
func (s *snapshots) seeded(issues []record.Issue) {
	for _, issue := range issues {
		s.records[issue.Key] = issue
	}
}

// pass is the admission pass's observer (admit.Admission.UsePassObserver): the view of a pass
// whose fact committed.
func (s *snapshots) pass(view admit.PassView) {
	s.view, s.fresh, s.holders = &view, true, nil
	s.given = append(s.given, view.Given...)
}

// moved records a move of key's phase the engine wrote at at, drop being the line of its pull
// request leaving the merge queue the record shows before a move out of merging (SendBack.Drop).
func (s *snapshots) moved(key string, at time.Time, from, to phase.Phase, trigger workflow.TriggerKind, drop string) {
	c := s.timeline.course(key)
	c.Moves = append(c.Moves, moveOf(at, from, to, trigger, drop))
}

// roundBegins records that key's conflicting head was asked to merge its base forward at at, the
// issue keeping its phase: a round of conflict begins, unless one stands already (the engine asks
// again on a later move at a head it asked of).
func (s *snapshots) roundBegins(key string, at time.Time) {
	c := s.timeline.course(key)
	if n := len(c.Rounds); n > 0 && c.Rounds[n-1].End.IsZero() {
		return
	}
	c.Rounds = append(c.Rounds, CourseRound{Start: at, Cause: CauseConflict})
}

// roundEnds records that key's merge forward landed at at, which ends the round that stands.
func (s *snapshots) roundEnds(key string, at time.Time) {
	c := s.timeline.course(key)
	if n := len(c.Rounds); n > 0 && c.Rounds[n-1].End.IsZero() {
		c.Rounds[n-1].End = at
	}
}

// read reads the newest view once: its records and its own slots by key, its line and what holds
// each member back, admission.blocked and admission.held over its judgement (blockers.Blocked,
// record.HeldList), as the state's projection reads them; and whether an issue not yet started,
// neither in flight nor holding a slot, is held by a hold other than the cap. Its records join the
// daemon's newest records (records).
func (s *snapshots) read(inFlight map[string]bool) {
	v := s.view
	s.byKey = make(map[string]record.Issue, len(v.Issues))
	for _, issue := range v.Issues {
		s.byKey[issue.Key] = issue
		s.records[issue.Key] = issue
	}
	s.slotOf = make(map[string]record.Slot, len(v.Slots))
	own := make([]record.Slot, 0, len(v.Slots))
	for _, slot := range v.Slots {
		if _, ours := s.byKey[slot.Issue]; ours {
			s.slotOf[slot.Issue] = slot
			own = append(own, slot)
		}
	}
	s.line = make(map[string]record.LineEntry, len(v.Line))
	s.heldBack = map[string]bool{}
	for _, entry := range v.Line {
		s.line[entry.Issue.Key] = entry
		if len(blockers.LineWaitsOn(v.Rows, entry)) > 0 {
			s.heldBack[entry.Issue.Key] = true
		}
	}
	s.blocked = map[string]bool{}
	for _, row := range blockers.Blocked(v.Rows, v.Issues, v.Line, v.Facts) {
		s.blocked[row.Issue] = true
	}
	s.held = map[string]bool{}
	for _, row := range record.HeldList(v.Issues, own, v.Line, v.Dropped, v.Facts) {
		s.held[row.Issue] = true
		if _, holds := s.slotOf[row.Issue]; !holds && !inFlight[row.Issue] && slices.Contains(firstStartHolds, row.Reason) {
			s.firstStartHeld = true
		}
	}
	for key := range s.heldBack {
		if _, holds := s.slotOf[key]; !holds && !inFlight[key] {
			s.firstStartHeld = true
		}
	}
}

// settle records the state the replay stands in at at, once every act of the instant is done: the
// slots the passes gave since the last settle, each issue's state, and each wait on other issues
// that began or ended. claims are the stub's claims, inFlight the issues in flight, and dispatch
// the replay's copy of Dispatch, which says what ended a wait. It reads each issue that is in
// flight, holds a slot, stands in the line, waits on another issue or stood in a counted state at
// the last settle, once.
func (s *snapshots) settle(at time.Time, claims map[claim.Token]supervise.Claim, inFlight map[string]bool, dispatch *shadow) {
	for _, given := range s.given {
		c := s.timeline.course(given.Issue)
		c.Given = append(c.Given, at)
	}
	s.given = s.given[:0]
	if s.view == nil {
		return
	}
	if s.fresh {
		s.read(inFlight)
		s.fresh = false
	}
	clear(s.keys)
	clear(s.working)
	for key, held := range s.byIssue {
		if _, holds := s.slotOf[key]; holds {
			s.byIssue[key] = held[:0]
		} else {
			delete(s.byIssue, key)
		}
	}
	keys, working, byIssue := s.keys, s.working, s.byIssue
	for _, c := range claims {
		if c.State == supervise.StateWorking {
			working[c.Issue] = true
		}
		if _, holds := s.slotOf[c.Issue]; holds {
			byIssue[c.Issue] = append(byIssue[c.Issue], c)
		}
	}
	for _, set := range []map[string]bool{inFlight, s.live} {
		for key := range set {
			keys[key] = struct{}{}
		}
	}
	for key := range s.slotOf {
		keys[key] = struct{}{}
	}
	for key := range s.line {
		keys[key] = struct{}{}
	}
	for key := range s.waits {
		keys[key] = struct{}{}
	}
	for key := range keys {
		issue, recorded := s.byKey[key]
		tree := key
		if recorded && issue.Tree != "" {
			tree = issue.Tree
		}
		slot, holds := s.slotOf[key]
		state := IssueState{InFlight: inFlight[key], Holds: holds, InTurn: working[key]}
		if holds {
			state.Holder, _ = s.holderState(issue, slot, byIssue[key])
		}
		if recorded && (state.InFlight || holds) {
			state.PastReview = record.InSlotlessPhase(issue)
			state.AwaitingChildren = state.InFlight && issue.Phase == phase.AwaitingChildren
		}
		if entry, member := s.line[key]; member {
			state.Part, state.Blocked = entry.Part, s.blocked[key]
			state.Waiting = !holds && !s.heldBack[key] && !state.Blocked && !s.held[key]
		}
		if !state.InFlight && !holds && state.Part == 0 {
			state = IssueState{}
		}
		if holds || state.Waiting {
			s.ranks.place(&state, key, tree, at)
		}
		if state != (IssueState{}) || s.timeline.Issues[key] != nil {
			s.timeline.course(key).set(at, state)
		}
		if state == (IssueState{}) {
			delete(s.live, key)
		} else {
			s.live[key] = true
		}
		s.waitAt(at, key, issue, recorded && record.InWorkflow(issue), inFlight[key], dispatch)
	}
}

// holderState is the state of the slots reading issue, holding slot, stands in, read by the slots
// reading's own reading of a holder (blockers.SlotHolderState) over the view and claims, the stub's
// claims of its issue, and the moment it has stood in that state since, nil where the reading gives
// none. The stub's claims are none that nothing will wake, since a replay's worker is never
// stranded.
func (s *snapshots) holderState(issue record.Issue, slot record.Slot, claims []supervise.Claim) (record.HolderState, *time.Time) {
	return blockers.SlotHolderState(issue, slot, record.HolderClaimsOf(claims), nil, s.view.Rows, s.view.Facts)
}

// idleHolders is how many of the project's issues hold a slot as the newest pass left them, and the
// moment each of them whose state is idle has stood idle since (holderState), the claims being the
// stub's as they stand: the replay's own holders, which the idle signal reads. Before the window's
// start the replay's course is the record's, so a holder's idle time begins no earlier than the
// newest end of a turn of its issue that the record shows before the start (endedBefore), as the
// record's own reading of the signal dates it (SignalRecord.idleHolders); the stub's claims hold
// such an end only for a turn under way at the start. None before the first pass's view.
func (s *snapshots) idleHolders(claims map[claim.Token]supervise.Claim, endedBefore map[string]time.Time) (int, []time.Time) {
	if s.view == nil {
		return 0, nil
	}
	if s.holders == nil {
		issues := make(map[string]record.Issue, len(s.view.Issues))
		for _, issue := range s.view.Issues {
			issues[issue.Key] = issue
		}
		s.holders = []heldSlot{}
		for _, slot := range s.view.Slots {
			if issue, ours := issues[slot.Issue]; ours {
				s.holders = append(s.holders, heldSlot{issue: issue, slot: slot})
			}
		}
	}
	byIssue := make(map[string][]supervise.Claim, len(s.holders))
	for _, held := range s.holders {
		byIssue[held.issue.Key] = nil
	}
	for _, c := range claims {
		if held, holds := byIssue[c.Issue]; holds {
			byIssue[c.Issue] = append(held, c)
		}
	}
	var idle []time.Time
	for _, held := range s.holders {
		state, since := s.holderState(held.issue, held.slot, byIssue[held.issue.Key])
		if state != record.HolderIdle || since == nil {
			continue
		}
		if ended, ok := endedBefore[held.issue.Key]; ok && ended.After(*since) {
			since = &ended
		}
		idle = append(idle, *since)
	}
	return len(s.holders), idle
}

// heldSlot is a slot of the newest pass's view and the record of the issue of the project that
// holds it.
type heldSlot struct {
	issue record.Issue
	slot  record.Slot
}

// waitAt begins, carries or ends key's wait on other issues at at: an issue in flight that stands
// before its review, or waits in awaiting_children, waits while a blocker its links name is not
// done (unfinishedLinks) or a Dispatch child it waits for is open (openChildren), and a started
// parent whose wait on its open children stands in its phase (waitsInItsPhase) waits while such a
// child is open; the wait ends at the first settle where nothing is left, read against the newest
// view.
func (s *snapshots) waitAt(at time.Time, key string, issue record.Issue, inWorkflow, inFlight bool, dispatch *shadow) {
	var links, children []string
	if inWorkflow && inFlight && waitsBeforeReview(issue) {
		links, _ = unfinishedLinks(key, dispatch)
	}
	if inWorkflow && inFlight && (waitsBeforeReview(issue) || waitsInItsPhase(issue, s.view.Facts.Rule)) {
		children = s.openChildren(key, issue, dispatch)
	}
	wait := s.waits[key]
	switch {
	case len(links)+len(children) > 0 && wait == nil:
		s.waits[key] = &standingWait{start: at, links: links, children: children}
	case len(links)+len(children) > 0:
		wait.links, wait.children = links, children
	case wait != nil:
		end := WaitLeft
		if inWorkflow && inFlight {
			end = waitEnd(key, issue, wait, s.view, dispatch)
		}
		c := s.timeline.course(key)
		c.Waits = append(c.Waits, CourseWait{Start: wait.start, End: at, EndedBy: end})
		delete(s.waits, key)
	}
}

// openChildren is the open Dispatch children key waits for: the newest pass's children-first
// judgement of its first phase (record.LineFacts.ChildWaits) or, for a parent whose phases have
// started, the pass's judgement of the children that hold its next start
// (record.LineFacts.StartedParentWaits), read where the parent waits on them: in
// awaiting_children, which a rule whose wait stops the phase moves it into, or in the phase it
// stands in under a rule whose cover takes that wait (waitsInItsPhase); and, where the first
// judgement leaves out an issue whose wait on its children stood at the last settle because the
// root of its tree lingers or the pass's records hold no root of it
// (record.SlotRule.ChildrenFirstCandidates), the children the replay's copy of Dispatch shows under
// it, each open by the daemon's own reading (record.ChildOpen) with the daemon's record of it
// (recordOf). That is the trace's reading of the wait (traceBuilder.childrenAt), which no linger of
// a root ends: the wait goes on until its children close or move out, as the line timeline reads it
// from the record.
func (s *snapshots) openChildren(key string, issue record.Issue, dispatch *shadow) []string {
	var open []string
	judged, ok := s.view.Facts.ChildWaits[key]
	if !ok && (issue.Phase == phase.AwaitingChildren || waitsInItsPhase(issue, s.view.Facts.Rule)) {
		judged, ok = s.view.Facts.StartedParentWaits[key]
	}
	if ok {
		for _, child := range judged.Open {
			open = append(open, child.Key)
		}
		return open
	}
	root, recorded := s.byKey[issue.Tree]
	if standing := s.waits[key]; standing == nil || len(standing.children) == 0 || recorded && !root.Lingers() {
		return nil
	}
	for child, shown := range dispatch.issues {
		if shown.payload == nil || shown.parent() != key {
			continue
		}
		if record.ChildOpen(record.DispatchIssueRow{Key: child, Status: shown.status(), Parent: key}, s.recordOf(child)) {
			open = append(open, child)
		}
	}
	slices.Sort(open)
	return open
}

// recordOf is the daemon's record of key now, nil for an issue it has never recorded: the newest
// view's where that view holds it, and otherwise the newest it has held, standing at done. A view
// holds every record but a done one that is not set back to todo and holds no slot
// (record.Store.LiveIssues), so a record the view leaves out has finished or left the workflow,
// as the trace reads a child it holds in no visit (traceBuilder.childrenAt).
func (s *snapshots) recordOf(key string) *record.Issue {
	if issue, live := s.byKey[key]; live {
		return &issue
	}
	issue, recorded := s.records[key]
	if !recorded {
		return nil
	}
	issue.Phase = phase.Done
	return &issue
}

// unfinishedLinks is the blockers key's Dispatch links name that are not done, and every blocker
// they name, as the replay's copy of Dispatch shows them: a blocker it holds no payload of is not
// done. It is the trace's reading of a link wait (traceBuilder.linkWaits), over the copy the
// daemon's mirror is written from (TestTheShadowMatchesTheMirrorAfterEveryFact), and never the
// rows a pass read. A pass reads the mirror rows of the issues it judges, each with the rows of
// the blockers it names (blockers.JudgeLine, record.Store.DispatchIssuesOf): one a blocker can
// hold where it stands and one it has started (blockers' startedIssues: a slot, an owed start or
// the mark of a release for a wait). An issue it judges no wait of — one in flight that holds no
// slot, owes no start and carries no mark — has no row there, or a row read only as another
// issue's blocker, with no row of the blockers it names itself, which blockers.Unfinished counts
// unfinished whether or not they are done.
func unfinishedLinks(key string, dispatch *shadow) (unfinished, linked []string) {
	iss, shown := dispatch.issues[key]
	if !shown {
		return nil, nil
	}
	linked = iss.blockers()
	for _, blocker := range linked {
		if other, ok := dispatch.issues[blocker]; !ok || other.status() != record.DoneStatus {
			unfinished = append(unfinished, blocker)
		}
	}
	return unfinished, linked
}

// waitsBeforeReview says whether issue stands where a wait on other issues is measured
// (measuredPhase, read where it stands): before its review, or in awaiting_children, where a
// started parent waits on its open children under a rule whose wait stops the phase
// (cybermonklabs/legion#4592).
func waitsBeforeReview(issue record.Issue) bool {
	return measuredPhase(record.StandsAt(issue))
}

// waitsInItsPhase says whether issue, a started parent, waits on its open children in the phase it
// stands in under rule: a rule whose cover takes that wait where the parent stands
// (coverTakesStartedParents), and a phase whose start an open child holds (record.OpenChildHolds,
// read where it stands), accepting among them, where the measures count no other wait of it.
func waitsInItsPhase(issue record.Issue, rule record.SlotRule) bool {
	return coverTakesStartedParents(rule) && record.OpenChildHolds(record.StandsAt(issue))
}

// waitEnd is what ended key's wait, which stood on wait's blockers and children at the settle
// before: an issue that moved past its review moved on, unless it waits on its children in its
// phase (waitsInItsPhase) and the wait stood on children; a wait on blockers ended, before the
// review, with a link taken off where one of them is no longer linked (unfinishedLinks), and with a
// blocker done where each is linked still; a wait on children ended by the first of them, as the
// replay's copy of Dispatch shows it now: a child closed done or icebox is done, one under another
// parent moved out, and a child still open under the issue holds it no longer because its tree's
// gate closed, which holds no started parent that waits in its phase.
func waitEnd(key string, issue record.Issue, wait *standingWait, v *admit.PassView, dispatch *shadow) WaitEnd {
	before, inPhase := waitsBeforeReview(issue), waitsInItsPhase(issue, v.Facts.Rule)
	if !before && (!inPhase || len(wait.children) == 0) {
		return WaitMovedOn
	}
	if before && len(wait.links) > 0 {
		_, linked := unfinishedLinks(key, dispatch)
		for _, blocker := range wait.links {
			if !slices.Contains(linked, blocker) {
				return WaitLinkRemoved
			}
		}
		return WaitBlockerDone
	}
	for _, child := range wait.children {
		shown, ok := dispatch.issues[child]
		switch {
		case !ok:
			return WaitChildDone
		case shown.status() == record.DoneStatus || shown.status() == "icebox":
			return WaitChildDone
		case shown.parent() != key:
			return WaitChildMovedOut
		case !inPhase && !v.Facts.Gates[issue.Tree].Open():
			return WaitGateClosed
		}
	}
	return WaitChildDone
}

// timelineAt is the snapshot timeline with every wait that stands at its end left open.
func (s *snapshots) timelineAt() Timeline {
	for _, key := range slices.Sorted(maps.Keys(s.waits)) {
		c := s.timeline.course(key)
		c.Waits = append(c.Waits, CourseWait{Start: s.waits[key].start, EndedBy: WaitOpen})
	}
	s.waits = map[string]*standingWait{}
	return s.timeline
}

// beginWaitsAt begins each issue of tl that waits for a slot in its first state, taken at tl's
// start, at the moment starts gives it, where that is earlier: the moment the record shows it
// began that wait (recordedWaitStarts). The start state is the record's (assumption 6), so when
// its waits began is part of it, and the measures that run back to a wait's beginning
// (Scorecard.MedianSlotWaitMinutes) read it as the record's own scorecard and the line timeline
// do. The state before tl's start is the first state carried back; no measure counts time before
// the start. The waits on other issues that stand at the start need nothing: the measures count
// their hours from the window's start alone (measureSpan).
func (tl Timeline) beginWaitsAt(starts map[string]time.Time) {
	for key, since := range starts {
		c := tl.Issues[key]
		if c == nil || len(c.States) == 0 || !c.States[0].At.Equal(tl.From) || !waitsForSlot(c.States[0].IssueState) ||
			!since.Before(tl.From) {
			continue
		}
		c.States = slices.Insert(c.States, 0, Stint{At: since, IssueState: c.States[0].IssueState})
	}
}

// startWaiters is the issues of tl that wait for a slot in their first state, taken at tl's start.
func (tl Timeline) startWaiters() []string {
	var keys []string
	for key, c := range tl.Issues {
		if len(c.States) > 0 && c.States[0].At.Equal(tl.From) && waitsForSlot(c.States[0].IssueState) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// waitsOnIssues says whether the timeline holds a wait of an issue on another issue
// (assumption 4).
func (s *snapshots) waitsOnIssues() bool {
	if len(s.waits) > 0 {
		return true
	}
	for _, c := range s.timeline.Issues {
		if len(c.Waits) > 0 {
			return true
		}
	}
	return false
}
