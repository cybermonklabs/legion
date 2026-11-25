package replay

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// heldRootReleaseReason is the reason of the release of a held root set to todo
// (workflow.ReleaseReason), by a person or by the controller through the daemon's API: the todo's
// own event releases it in a replay, a person's as any person's event is applied and the daemon's
// as keepsDaemonEvent applies it (releasesHold), so no retry is entered for it.
// operatorRetryReason is the reason of the operator's retry of a phase
// (workflow.OperatorRetryReason), which a replay enters as intake.RestartPhase, and by which the
// trace tells that retry of a worker whose issue stands in no hold from a release of a hold it
// missed. Each is the constant its writer writes, so a change of its words moves the replay's
// reader with it.
const (
	heldRootReleaseReason = workflow.ReleaseReason
	operatorRetryReason   = workflow.OperatorRetryReason
)

// openSeeded runs each piece under way at the window's start at its recorded times: the start
// state's claims stand in its worker, so only its acts from the start on are entered.
func (d *driver) openSeeded() {
	for _, key := range slices.Sorted(maps.Keys(d.script.pieces)) {
		for _, p := range d.script.pieces[key] {
			if p.opens == opensAtSeed && p.state == pieceWaiting {
				d.openPiece(p, p.start, true)
			}
		}
	}
}

// openOnPhase opens the next piece of key's work that the move into to opens rather than a start
// row (opensOnPhase): accepting's, whose role is the architect that owns the issue.
func (d *driver) openOnPhase(key string, to phase.Phase) {
	for _, p := range d.script.pieces[key] {
		if p.state != pieceWaiting {
			continue
		}
		if p.opens == opensOnPhase && p.phase == to {
			d.openPiece(p, d.clock.Instant(), false)
		}
		return
	}
}

// startPiece opens, for a start of role on key that the stub took, serving the phase served, the
// next piece of key's work that role's start opens: the first piece not yet run that is role's in
// served; a piece before it the replay passes over (passOver), counted. It answers the piece it
// opened, nil for none.
func (d *driver) startPiece(key string, role claim.Role, served phase.Phase, retry bool) *piece {
	for _, p := range d.script.pieces[key] {
		if p.state != pieceWaiting {
			continue
		}
		if p.role != role || p.phase != served || p.opens == opensOnPhase || p.opens == opensAtSeed || (p.opens == opensOnRetry) != retry {
			continue
		}
		for _, before := range d.script.pieces[key][:p.index] {
			if before.state == pieceWaiting {
				d.passOver(before)
				d.counters.SkippedSegments++
			}
		}
		d.openPiece(p, d.clock.Instant(), false)
		return p
	}
	return nil
}

// runningPiece is the piece of key's work role's claim stands in now, running or paused; nil for
// none.
func (d *driver) runningPiece(key string, role claim.Role) *piece {
	for _, p := range d.script.pieces[key] {
		if (p.state == pieceRunning || p.state == piecePaused) && p.role == role {
			return p
		}
	}
	return nil
}

// openPiece runs p from at: each of its acts is queued at its offset from at, and its end at at
// plus its length, its recorded length slowed by the issues in flight (slowed). A piece under way
// at the window's start (seeded) runs at its recorded times, its acts before the start already in
// the start state. Any other piece is sized at the end of the instant it opens in (sizePiece), so
// the issues in flight it is slowed by are the instant's, once every pass of the instant has run,
// as the record's count it is set against is (script.inFlightAt): the starts of one instant, each
// in a pass of its own, are each read at the instant's count, not at the count of the passes
// before it. Its acts at its very start are queued at once.
func (d *driver) openPiece(p *piece, at time.Time, seeded bool) {
	p.state, p.at, p.turns = pieceRunning, at, map[claim.Role]bool{}
	if seeded {
		d.sizePiece(p, at, p.m, true)
		return
	}
	for _, a := range p.acts {
		if a.offset <= 0 {
			d.queueAct(p, a, at)
		}
	}
	d.push(&item{at: at, class: classSize, rec: at, piece: p, run: func() error {
		d.sizePiece(p, d.clock.Instant(), len(d.inFlight), false)
		return nil
	}})
}

// openPieceFrom opens p as from, a moment at or before the clock's instant, had opened it: each of
// its acts is queued at its offset from from, those already past coming at once in their order,
// and its end at from plus its length, sized at the end of the instant as openPiece sizes a piece.
// The operator's retry with a fresh session opens a piece so (driver.freshRetry), from the replay's
// start of the visit it retries at the piece's recorded offset in that visit.
func (d *driver) openPieceFrom(p *piece, from time.Time) {
	p.state, p.at, p.turns = pieceRunning, from, map[claim.Role]bool{}
	for _, a := range p.acts {
		if a.offset <= 0 {
			d.queueAct(p, a, from)
		}
	}
	now := d.clock.Instant()
	d.push(&item{at: now, class: classSize, rec: now, piece: p, run: func() error {
		d.sizePiece(p, from, len(d.inFlight), false)
		return nil
	}})
}

// sizePiece sets p's length for n issues in flight at its start and queues its acts after its
// start, at their offsets from at, and its end at at plus its length. An act the record shows at
// the segment's end, the last turn's end among them, comes at the replay's end of it, and an act
// the record shows after the end of a piece the replay shortened is queued at its end, so a turn
// that ran to the end of its segment runs to the end of the replay's, however long the replay made
// it. A seeded piece's acts before the window's start are left out but for the turns under way at
// it.
func (d *driver) sizePiece(p *piece, at time.Time, n int, seeded bool) {
	p.n = n
	p.length = slowed(p.worked(), d.cfg.Slowdown, p.n, p.m)
	if d.cfg.TraceSegments {
		d.segments = append(d.segments, Segment{Issue: p.issue, Phase: p.phase, Start: p.at, N: p.n, M: p.m, Recorded: Duration(p.recorded()),
			Length: Duration(p.length)})
	}
	for _, a := range p.acts {
		if seeded && a.rec.Before(d.window.From) {
			if a.kind == actTurnStart {
				p.turns[a.role] = true
			}
			continue
		}
		if !seeded && a.offset <= 0 {
			continue
		}
		when := at.Add(a.offset)
		switch {
		case p.end.IsZero():
		case a.offset >= p.recorded():
			when = at.Add(p.length)
		case a.offset > p.length && a.kind != actTurnEnd:
			when = at.Add(p.length)
		}
		d.queueAct(p, a, when)
	}
	if !p.end.IsZero() {
		d.push(&item{at: at.Add(p.length), class: classAct, rec: p.end, order: lineOrder + 1, piece: p, run: func() error { return d.endPiece(p) }})
	}
}

// queueAct queues one act of p at when.
func (d *driver) queueAct(p *piece, a act, when time.Time) {
	switch a.kind {
	case actTurnStart:
		d.push(&item{at: when, class: classTurnStart, rec: a.rec, piece: p, run: func() error { return d.turnStarts(p, a.role) }})
	case actTurnEnd:
		d.push(&item{at: when, class: classTurnEnd, rec: a.rec, piece: p, run: func() error { return d.turnEnds(p, a.role) }})
	case actEvent:
		d.queueEvent(a.event, when, a.rec, p)
	case actRegistration:
		d.push(&item{at: when, class: classAct, rec: a.rec, order: lineOrder + int64(a.order), piece: p,
			run: func() error { return d.register(p.issue, d.gateFor(p, a.gate)) }})
	case actClaimFailed:
		d.push(&item{at: when, class: classAct, rec: a.rec, order: lineOrder, piece: p, run: func() error { return d.claimFailed(a.failed) }})
	case actAdoption:
		d.push(&item{at: when, class: classAct, rec: a.rec, order: lineOrder, piece: p, run: func() error { return d.adopt(a.joined) }})
	case actCloseRoot:
		d.push(&item{at: when, class: classAct, rec: a.rec, order: lineOrder, piece: p, run: func() error { return d.closeRoot(p.issue) }})
	case actReady:
		d.push(&item{at: when, class: classAct, rec: a.rec, order: lineOrder, piece: p, run: func() error { return d.readyMark(p.issue) }})
	}
}

// passOver passes over p, a piece of work the replay has not opened and will not run: its
// recorded events are placed by their anchors (placeUnrun).
func (d *driver) passOver(p *piece) {
	p.state, p.pending = pieceSkipped, nil
	d.placeUnrun(p)
}

// cancelPiece cancels p, running or paused, so nothing more of its work goes on: its recorded
// events still to come in it are placed by their anchors (placeUnrun), and the items it held are
// let go, since only a paused piece's resume reads them.
func (d *driver) cancelPiece(p *piece) {
	p.state, p.pending = pieceCancelled, nil
	d.placeUnrun(p)
}

// placeUnrun places each recorded event of p's work that p no longer runs, p passed over or
// cancelled, as an event no piece places (placeByAnchor): each event p never queued, and each it
// queued that it has not applied, whose item the queue drops with p's. So each worker's event is
// applied once: where the replay runs its piece, by that piece; otherwise as any other event.
func (d *driver) placeUnrun(p *piece) {
	for _, a := range p.acts {
		if a.kind != actEvent {
			continue
		}
		event := &d.script.events[a.event]
		if event.piece != p || event.dispatched {
			continue
		}
		event.piece = nil
		delete(d.placed, a.event)
		d.placeByAnchor(a.event)
	}
}

// pausePiece stops p where its worker's claim was suspended for a wait the rule stops the phase
// for (record.SlotRule.WaitStopsPhase): its items wait, keeping the rest of its length, and the
// turns under way end.
func (d *driver) pausePiece(p *piece) error {
	if p.state != pieceRunning {
		return nil
	}
	p.state, p.pausedAt = piecePaused, d.clock.Instant()
	for _, role := range slices.Sorted(maps.Keys(p.turns)) {
		if p.turns[role] {
			if err := d.writeTurnEnd(p.issue, role); err != nil {
				return err
			}
		}
	}
	return nil
}

// resumePiece runs p again from the clock's instant, with the rest of its length: every item it
// held is queued again later by the time it stood paused, and the turns the pause ended begin
// again.
func (d *driver) resumePiece(p *piece) error {
	if p.state != piecePaused {
		return nil
	}
	now := d.clock.Instant()
	shift := now.Sub(p.pausedAt)
	p.state = pieceRunning
	held := p.pending
	p.pending = nil
	for _, it := range held {
		if it.done {
			continue
		}
		it.done = true
		again := *it
		again.done, again.at = false, maxTime(it.at.Add(shift), now)
		d.push(&again)
	}
	for _, role := range slices.Sorted(maps.Keys(p.turns)) {
		if p.turns[role] {
			if err := d.writeTurnStart(p.issue, role); err != nil {
				return err
			}
		}
	}
	return nil
}

// cancelWork cancels every piece of key's work under way (cancelPiece): its issue left the
// workflow, so nothing of that work goes on. The recorded events of the pieces of the generation it
// stands in that the replay has not opened enter then too, by their anchors (placeUnrun), as the
// events of work the replay passed over or cancelled enter: an issue the replay ran ahead of the
// record, or behind it, can leave before the piece that files a child, writes a document or sets a
// link opens, and that event must still enter (assumption 17). Such a piece is left to run where the
// replay's issue enters the workflow again, at the generation it then stands at (piece.leftUnrun),
// the generation the record ran it in being one the replay left without running it. A piece of a
// later generation, work the record ran after the issue entered the workflow again, is left to run
// where the replay enters it again (assumption 16); a piece left unopened stays so, its events
// entered once.
func (d *driver) cancelWork(key string) error {
	boundary, err := d.nextGenerationAt(key)
	if err != nil {
		return err
	}
	for _, p := range d.script.pieces[key] {
		switch p.state {
		case pieceRunning, piecePaused:
			d.cancelPiece(p)
		case pieceWaiting:
			if boundary.IsZero() || p.start.Before(boundary) {
				d.placeUnrun(p)
				p.leftUnrun = true
			}
		}
	}
	return nil
}

// nextGenerationAt is the record's moment key entered the workflow at a generation after the one
// its replay record stands at, the first piece of whose work is no work of the generation it
// stands in; zero where the record shows no later entry.
func (d *driver) nextGenerationAt(key string) (time.Time, error) {
	iss, ok := d.trace.Issues[key]
	if !ok {
		return time.Time{}, nil
	}
	var generation uint64
	if err := store.TransactWith(d.ctx, d.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := d.records.Issue(ctx, tx, key)
		if issue != nil {
			generation = issue.Generation
		}
		return err
	}); err != nil {
		return time.Time{}, err
	}
	for _, entry := range iss.Entries {
		if entry.Generation > generation {
			return entry.At, nil
		}
	}
	return time.Time{}, nil
}

// endPiece ends p at the replay's end of its segment: the turns under way in it end, the task its
// role's claim held for it is retired as its worker's last turn retires it (retireTask), and its end
// is entered by how it ends — its recorded move, a sign-off the engine may hold, the failure of its
// role's claim, which keeps its task as a failed claim does, the landing of a merge forward — or by
// nothing, where the pass made its move or nothing ended it. A merge forward retires no task: the
// merger's merge forward of a conflict that waited for merging runs in the task the merger's start
// gave it, whose work in merging comes after it (mergerMergesForward), and a merge forward asked at
// once runs after the piece of its phase retired the task. A recorded move to done waits for each
// child the record brought in under the issue before that move and the replay has yet to bring in
// (childrenToEnter): the daemon holds a merged issue's close while an issue beneath it is in the
// workflow, and the record filed the child before the issue's done, so the replay keeps that
// order rather than close the issue's tree under a child filed at its recorded time (assumption 3:
// a passed-over loop's acts on other issues keep their recorded times).
func (d *driver) endPiece(p *piece) error {
	for _, role := range slices.Sorted(maps.Keys(p.turns)) {
		if p.turns[role] {
			if err := d.turnEnds(p, role); err != nil {
				return err
			}
		}
	}
	if move := p.move(); p.ends == endMove && move != nil && cmp.Or(move.Source.To, move.To) == phase.Done {
		if at := d.childrenToEnter(p.issue, move.At); !at.IsZero() {
			d.push(&item{at: at, class: classAct, rec: p.end, order: lineOrder + 1, piece: p, run: func() error { return d.endPiece(p) }})
			return nil
		}
	}
	p.state, p.pending = pieceEnded, nil
	if p.ends == endFailure {
		return d.failPiece(p)
	}
	if p.ends == endMergeForward {
		return d.mergedForward(p)
	}
	if err := d.retireTask(p.issue, p.role); err != nil {
		return err
	}
	switch p.ends {
	case endMove:
		return d.enterMove(p)
	case endHeldSignOff:
		return d.signOff(p.issue)
	}
	return nil
}

// childrenToEnter is the latest moment the replay placed, and has not yet applied, a recorded event
// that brings a child under key into the workflow, made before by, key's recorded move to done:
// the child's filing under key (filedUnder), or its own move to todo from a status out of the
// workflow, as the architect's release writes it; zero for none. Such an event placed for a later
// moment holds key's own move to done to it (endPiece), and an event the replay never placed holds
// nothing.
func (d *driver) childrenToEnter(key string, by time.Time) time.Time {
	now := d.clock.Instant()
	var latest time.Time
	for i, at := range d.placed {
		event := &d.script.events[i]
		if event.dispatched || !at.After(now) || !event.at.Before(by) || !d.entersChildUnder(event, key) {
			continue
		}
		latest = maxTime(latest, at)
	}
	return latest
}

// entersChildUnder says whether event brings a child under key into the workflow: its filing under
// key, or a move of an issue whose parent is key to todo from a status out of the workflow.
func (d *driver) entersChildUnder(event *scriptEvent, key string) bool {
	if filedUnder(event) == key {
		return true
	}
	shown := event.parsed.Issue
	if shown == nil || shown.Parent != key || shown.Status != "todo" || event.prev < 0 {
		return false
	}
	before := d.script.events[event.prev].parsed.Issue
	return before != nil && record.OutOfWorkflow(before.Status)
}

// generationAt is the generation key's record stood at in the record at at.
func (d *driver) generationAt(key string, at time.Time) uint64 {
	if iss, ok := d.trace.Issues[key]; ok {
		if entry := iss.entryAt(at); entry != nil {
			return entry.Generation
		}
	}
	return 1
}

// enterMove enters the recorded move that ends p's visit, or the merge at which its segment ended
// (piece.move). Where the move is a recorded conflict whose loop runs to the window's end, the
// conflict is kept and not drawn (keepConflict, assumption 35). Under the branch's Conflicts
// recorded no stretch draws, and each recorded conflict is made as recorded (recordedConflict).
// Otherwise, where the move ends a stretch in which a head can conflict (spec R2, assumption 3),
// the stretch's end comes first (drawConflict): a stretch that has not drawn draws whether its head
// conflicts, a conflict a boot's read drew for it in this phase enters, and a conflict the daemon
// acts on takes the move's place; otherwise the move enters as recorded (enterRecorded).
func (d *driver) enterMove(p *piece) error {
	if p.move() == nil {
		return nil
	}
	if d.keepsOpenLoop(p) {
		return d.keepConflict(p)
	}
	if d.cfg.Branch.recordedConflicts() {
		return d.recordedConflict(p)
	}
	if stretchPhase(p.phase) && endsStretch(p.move()) {
		conflicted, err := d.drawConflict(p)
		if err != nil || conflicted {
			return err
		}
	}
	return d.enterRecorded(p)
}

// recordedConflict enters the recorded move that ends p's visit under the branch's Conflicts
// recorded (assumption 44): a recorded conflict that ends a stretch (MoveDraw) is made through the
// daemon's own path for a conflicting head as a kept one is (conflictFor), and its issue runs its
// own loop as recorded; one the daemon does not act on ends the stretch as recorded. Every other
// move enters as recorded, and no stretch draws.
func (d *driver) recordedConflict(p *piece) error {
	if p.move().Source.Way != MoveDraw || !stretchPhase(p.phase) {
		return d.enterRecorded(p)
	}
	conflicted, err := d.conflictFor(p, true, false)
	if err != nil || conflicted {
		return err
	}
	return d.enterRecorded(p)
}

// enterRecorded enters the recorded move that ends p's visit (piece.move), by its source
// (MoveSources): through the engine's ReplayTransition, or through the daemon's own handler of the
// fact the API or GitHub applies. A move whose source is the conflict draw is no move this replay
// enters: the stretch it ended drew no conflict, and goes on past the loop the record ran for it
// (stretchEnds), which the run counts as a loop passed over (Divergence.SkippedLoops). Every move
// the daemon's code refuses is counted with its reason, and a merge whose close waits for the
// issues beneath it is counted as held (merged). The run names the assumptions a move of a recorded
// release rests on (MoveSource.Assumptions: 21 and 22 for that release's checks_red), and, for a
// worker's completion entered through ReplayTransition, the one every such move rests on
// (assumeCompletionPassed), since the completion itself is a fact the replay leaves out
// (FactSources' HandoffComplete).
func (d *driver) enterRecorded(p *piece) error {
	move := p.move()
	if move.Source.Way == MoveDraw {
		d.counters.SkippedLoops++
		return d.stretchEnds(p)
	}
	for _, number := range move.Source.Assumptions {
		d.used[number] = true
	}
	var pr *record.PullRequest
	if move.Trigger == workflow.TriggerImplementationReady || move.Trigger == workflow.TriggerPullRequestMerged || pastImplementing(move.From) {
		var err error
		if pr, err = d.ensurePullRequest(p.issue); err != nil {
			return err
		}
	}
	to := cmp.Or(move.Source.To, move.To)
	if phase.MovesBack(move.From, to) {
		d.used[3], d.used[13] = true, true
	}
	refuse := func(reason string) {
		d.counters.Refused = append(d.counters.Refused, RefusedMove{Issue: p.issue, At: d.clock.Instant(), Trigger: move.Trigger,
			From: move.From, To: to, Reason: reason})
	}
	switch move.Source.Way {
	case MoveTransition:
		// checks_red is GitHub's move, not a worker's completion.
		if move.Trigger != workflow.TriggerChecksRed {
			d.used[assumeCompletionPassed] = true
		}
		generation, err := d.moveGeneration(p, move)
		if err != nil {
			return err
		}
		result, err := d.apply("replay", d.factID("move", p.issue, move.Trigger), RecordedMove{Issue: p.issue,
			Generation: generation, From: move.From, To: to, Trigger: move.Trigger})
		if err != nil {
			return err
		}
		if result.Refusal != nil {
			refuse(result.Refusal.Message)
		}
		return nil
	case MoveFact:
		switch move.Trigger {
		case workflow.TriggerSignOff:
			return d.signOff(p.issue)
		case workflow.TriggerPullRequestMerged:
			if pr == nil {
				refuse(fmt.Sprintf("%s holds no pull request record to merge", p.issue))
				return nil
			}
			if _, err := d.apply("github", d.factID("merge", p.issue, pr.Number), intake.PullRequestMerged{Repo: pr.Repo, Number: pr.Number,
				MergeSHA: fmt.Sprintf("replay-merge-%s-%d", p.issue, pr.Number), HeadSHA: pr.HeadSHA}); err != nil {
				return err
			}
			return d.merged(p.issue, move, to, refuse)
		case workflow.TriggerBackward:
			if move.Drop != "" {
				d.drops[p.issue] = move.Drop
				d.queueLeft(p.issue, move, pr)
			}
			// A move back the architect asks for is an architect's request, judged again under
			// the fact lock (intake.ArchitectRequest), so it carries the claim, the generation and
			// the run the record stands at (architectRequest); a worker's own move back carries
			// none of them, as the daemon's own route leaves them unset for it.
			back := intake.BackwardMove{Issue: p.issue, Requester: phaseRole(move.From), To: to,
				Reason: fmt.Sprintf("the record's move back from %s to %s", move.From, to)}
			if back.Requester == claim.RoleArchitect {
				by, generation, entry, err := d.architectRequest(p.issue)
				if err != nil {
					return err
				}
				back.By, back.Generation, back.Entry = by, generation, entry
			}
			result, err := d.apply("api", d.factID("backward", p.issue, to), back)
			if err != nil {
				return err
			}
			if result.Refusal != nil {
				refuse(result.Refusal.Message)
			}
			return nil
		}
	}
	return nil
}

// queueLeft writes, just before the replay enters key's recorded move back out of merging, the line
// of key's pull request leaving the merge queue that the record shows before that move (Move.Drop):
// the msg and attributes of the daemon's own line (workflow's dequeued), on pr, the replay's own pull
// request record of key (nil for none, which writes nothing), which stands in for the issue's
// (assumption 29), with the recorded line's reason where it gives one. The merge queue is GitHub's,
// so no code the replay runs writes the line, and without it the line timeline, whose trace reads
// the replay's own log as a record's is read (CauseOf), would name the loop send_back where the
// snapshot timeline names it queue_drop.
func (d *driver) queueLeft(key string, move *Move, pr *record.PullRequest) {
	if pr == nil {
		return
	}
	args := []any{"issue", key, "pullRequest", pr.Number, "head", pr.HeadSHA}
	if iss, ok := d.trace.Issues[key]; ok {
		for _, drop := range slices.Backward(iss.QueueDrops) {
			if drop.At.After(move.At) {
				continue
			}
			if drop.Why != "" {
				args = append(args, "why", drop.Why)
			}
			break
		}
	}
	d.log.Info(move.Drop, args...)
}

// merged counts the merge of key's pull request the replay applied by move: one that left key
// standing in merging while issues beneath it are in the workflow (record.OpenBeneath) is a held
// merge (Divergence.HeldMerges), whose close the engine makes at the last one's leave, as the
// daemon's does, and for which the replay enters no move of its own; one that left key anywhere
// else but to is refused.
func (d *driver) merged(key string, move *Move, to phase.Phase, refuse func(string)) error {
	var issue *record.Issue
	var open []string
	if err := store.TransactWith(d.ctx, d.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if issue, err = d.records.Issue(ctx, tx, key); err != nil || issue == nil {
			return err
		}
		members, err := d.records.TreeIssues(ctx, tx, issue.Tree)
		open = record.OpenBeneath(*issue, members)
		return err
	}); err != nil {
		return err
	}
	switch {
	case issue == nil:
		refuse(fmt.Sprintf("%s is not recorded", key))
	case issue.Phase == to:
	case record.StandsAt(*issue) == phase.Merging && len(open) > 0:
		d.counters.HeldMerges++
	default:
		refuse(fmt.Sprintf("the %s of %s left it in %s, not %s", move.Trigger, key, issue.Phase, to))
	}
	return nil
}

// stretchEnds is the end of a stretch in which a head can conflict whose recorded end is a
// conflict (MoveDraw, assumption 3), where the replay's stretch did not conflict, or merged its
// base forward and kept its phase (mergedForward): the stretch goes on past the loop the record
// ran for its conflict, whose work the replay passes over (passOver), with the work the record
// shows after the loop returned to the phase it left, which opens at once in the same live stretch
// (liveStretch), so neither it nor any later end of that stretch draws a second time. A version or
// a registration of a root's design in that work keeps its recorded time (keepDesignTimes).
func (d *driver) stretchEnds(p *piece) error {
	pieces := d.script.pieces[p.issue]
	for _, next := range pieces[p.index+1:] {
		if next.state != pieceWaiting {
			continue
		}
		if next.returnsTo(p.phase) {
			d.keepDesignTimes(next)
			d.openPiece(next, d.clock.Instant(), false)
			return nil
		}
		d.passOver(next)
	}
	return nil
}

// keepDesignTimes takes out of p, a piece of a root's architect that stretchEnds opens before its
// recorded start, each act on the root's design documents, and places it as no piece places it
// (assumption 37): each artifact version its session writes, by the event's anchor (placeByAnchor),
// its recorded time but where it ended a wait, and each registration of a document at its recorded
// time. Such an act opens or closes the tree's design gate, which holds the first start of every
// member of the tree and the start of its merger, and the passed-over loop it followed in the
// record is no work of the root's alone: moved with the work the replay opens early, it would close
// or open the gate for the whole tree as early. A piece opened at or after its recorded start, and
// a piece of a child or of another role, keeps its acts.
func (d *driver) keepDesignTimes(p *piece) {
	if p.role != claim.RoleArchitect || !d.clock.Instant().Before(p.start) {
		return
	}
	if entry := d.trace.Issues[p.issue].entryAt(p.start); entry == nil || entry.Tree != p.issue {
		return
	}
	p.acts = slices.DeleteFunc(p.acts, func(a act) bool {
		switch a.kind {
		case actEvent:
			event := &d.script.events[a.event]
			if event.piece != p || event.dispatched || event.env.kind != "artifact.version" {
				return false
			}
			event.piece = nil
			d.placeByAnchor(a.event)
			return true
		case actRegistration:
			d.push(&item{at: a.rec, class: classAct, rec: a.rec, order: lineOrder + int64(a.order),
				run: func() error { return d.register(p.issue, d.gateFor(p, a.gate)) }})
			return true
		}
		return false
	})
}

// architectRequest is what an architect's request of key carries beside its own words
// (intake.ArchitectRequest): the claim of the architect that owns the issue, the issue's
// generation and the run of the step it stands in. The workflow judges all three again under the
// fact lock (workflow's requestRefusal), as it judges a live request, and a request that names
// none of them is refused as one of an architect outside the tree.
//
// The architect is the issue's tree root's (record.ArchitectAbove, which takes the root's
// architect for every issue of the tree): the recorded windows this tool replays ran before a
// child held an architect of its own, and the record's own authority for each act it replays was
// judged live when the act happened, so the replay makes each as the architect the record shows
// owning the tree. The generation and the run are the record's at the moment of the act, so a
// request of a run the replay has already left is refused here exactly as the daemon refuses it.
func (d *driver) architectRequest(key string) (claim.Token, uint64, record.StepEntry, error) {
	issue, err := d.recorded(key)
	if err != nil || issue == nil {
		return "", 0, record.StepEntry{}, err
	}
	project, err := claim.ProjectToken(d.window.Project)
	if err != nil {
		return "", 0, record.StepEntry{}, fmt.Errorf("replay: the architect of %s: %w", key, err)
	}
	token, err := claim.NewToken(project, issue.Tree, claim.RoleArchitect)
	if err != nil {
		return "", 0, record.StepEntry{}, fmt.Errorf("replay: the architect of %s: %w", key, err)
	}
	var entry record.StepEntry
	err = store.TransactWith(d.ctx, d.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		entry, err = d.records.OpenStep(ctx, tx, key)
		return err
	})
	return token, issue.Generation, entry, err
}

// signOff applies key's architect's sign-off (intake.SignOff) through the engine's own handler.
// A sign-off the engine holds while a member beneath is in the workflow is applied again at each
// leave of a member of its tree (observe), and is no refusal: the record shows the architect
// signing off again at once.
func (d *driver) signOff(key string) error {
	by, generation, entry, err := d.architectRequest(key)
	if err != nil {
		return err
	}
	result, err := d.apply("api", d.factID("sign-off", key), intake.SignOff{Issue: key, By: by, Generation: generation, Entry: entry})
	if err != nil {
		return err
	}
	switch {
	case result.Refusal == nil:
		delete(d.signOffs, key)
	case result.Refusal.Code == workflow.SignOffChildInWorkflow:
		if !d.signOffs[key] {
			d.counters.HeldSignOffs++
		}
		d.signOffs[key] = true
		if d.rule.Take == record.TakeCover {
			return d.heldSignOff(key)
		}
	default:
		delete(d.signOffs, key)
		d.counters.Refused = append(d.counters.Refused, RefusedMove{Issue: key, At: d.clock.Instant(), Trigger: workflow.TriggerSignOff,
			From: phase.Accepting, To: phase.Retro, Reason: result.Refusal.Message})
	}
	return nil
}

// heldSignOff enters the sign-off of key the workflow held behind an issue beneath it (HeldSignOff),
// which a rule whose start may take a cover reads as the parent's wait on the issues beneath it.
func (d *driver) heldSignOff(key string) error {
	issue, err := d.recorded(key)
	if err != nil || issue == nil {
		return err
	}
	_, err = d.apply("replay", d.factID("held-sign-off", key), HeldSignOff{Issue: key, Generation: issue.Generation})
	return err
}

// failPiece enters the failure of the claim of p's role that ends p (intake.ClaimFailed), which
// holds the issue, and queues the hold's recorded end and escalations at their recorded offsets
// from the failure (queueHoldEnd).
func (d *driver) failPiece(p *piece) error {
	failed := ClaimFailed{LineHead: LineHead{Time: p.end}, Claim: ClaimRef{Issue: p.issue, Role: p.role}}
	for _, line := range d.lines.failed {
		if line.Claim.Issue == p.issue && line.Claim.Role == p.role && line.Time.Equal(p.end) {
			failed = line
		}
	}
	if err := d.claimFailed(failed); err != nil {
		return err
	}
	if p.hold != nil {
		d.queueHoldEnd(p.issue, p.role, p.hold, d.clock.Instant())
	}
	return nil
}

// releaseSeededHolds queues the end and the escalations of each hold that stands at the window's
// start, which the start state places (Seed) and no failure of the replay's begins: each at its
// recorded time, the hold having begun at its recorded start (queueHoldEnd).
func (d *driver) releaseSeededHolds() {
	for _, key := range slices.Sorted(maps.Keys(d.script.pieces)) {
		iss := d.trace.Issues[key]
		for i := range iss.Waits {
			if hold := &iss.Waits[i]; hold.Kind == WaitHeld && standsAt(hold.Start, hold.End, d.window.From) {
				d.queueHoldEnd(key, hold.Role, hold, hold.Start)
			}
		}
	}
}

// queueHoldEnd queues what ends hold, a hold of key's role's claim that began at began in the
// replay, each at its recorded offset from the hold's start: each escalation of the hold from the
// window's start on, and the hold's end where the record shows the issue working the held phase
// again (Wait.released) — the architect's retry (intake.RetryOrEscalate), the operator's
// (intake.RestartPhase), or nothing for a todo on the held root, a person's or the controller's,
// whose own event releases it. A hold the record shows going on with no release line (WaitWentOn)
// is released as the architect's retry releases one, under the daemon's own reason.
func (d *driver) queueHoldEnd(key string, role claim.Role, hold *Wait, began time.Time) {
	for _, escalation := range d.lines.escalated {
		if escalation.Issue == key && escalation.Time.After(hold.Start) && !escalation.Time.Before(d.window.From) &&
			(hold.End.IsZero() || escalation.Time.Before(hold.End)) {
			d.push(&item{at: began.Add(escalation.Time.Sub(hold.Start)), class: classAct, rec: escalation.Time, order: lineOrder,
				run: func() error {
					_, err := d.apply("api", d.factID("escalate", key), intake.RetryOrEscalate{Issue: key, Decision: intake.EscalateDecision,
						Reason: escalation.Reason})
					return err
				}})
		}
	}
	if !hold.released() {
		return
	}
	reason := ""
	for _, line := range d.lines.released {
		if line.Issue == key && line.Time.Equal(hold.End) {
			reason = line.Reason
		}
	}
	if reason == heldRootReleaseReason {
		return
	}
	d.push(&item{at: began.Add(hold.End.Sub(hold.Start)), class: classAct, rec: hold.End, order: lineOrder, run: func() error {
		if reason == operatorRetryReason {
			_, err := d.apply("api", d.factID("restart", key), intake.RestartPhase{Issue: key, Role: role})
			return err
		}
		_, err := d.apply("api", d.factID("retry", key), intake.RetryOrEscalate{Issue: key, Decision: intake.RetryDecision, Reason: reason})
		return err
	}})
}

// queueFreshRetries queues each of the operator's retries with a fresh session the record shows in
// the visit of key's next piece of work in to (IssueTrace.FreshRetries), key having just moved into
// to: each at its recorded offset from the start of that visit, from this moment, the replay's
// start of it (freshRetry). A piece of drawn work stands in another issue's visit, whose retries
// are that issue's.
func (d *driver) queueFreshRetries(key string, to phase.Phase) {
	iss, ok := d.trace.Issues[key]
	if !ok || len(iss.FreshRetries) == 0 {
		return
	}
	i := slices.IndexFunc(d.script.pieces[key], func(q *piece) bool { return q.state == pieceWaiting && q.phase == to })
	if i < 0 {
		return
	}
	q, from := d.script.pieces[key][i], d.clock.Instant()
	if q.drawn || q.visit == nil {
		return
	}
	for _, retry := range iss.FreshRetries {
		if retry.Role != q.role || retry.Phase != q.phase || retry.At.Before(q.visit.Enter) ||
			!q.visit.Exit.IsZero() && !retry.At.Before(q.visit.Exit) {
			continue
		}
		d.push(&item{at: from.Add(retry.At.Sub(q.visit.Enter)), class: classAct, rec: retry.At, order: lineOrder,
			run: func() error { return d.freshRetry(key, q, from) }})
	}
}

// freshRetry enters the operator's retry of the worker of q, key's next piece of work in the
// visit the retry falls in, with a fresh session, the replay having begun that visit at from, as
// 958173da ran it (api's freshSession): the worker's claim is launched afresh outside the outbox's
// start fence (freshLaunch, the resume of the suspended claim), the piece opens as from places it
// at its recorded offset in the visit (openPieceFrom), and then the phase is restarted
// (intake.RestartPhase), whose own start the fence holds as the record shows it held. A retry that
// finds the replay's issue standing in another phase is refused and counted; one that finds the
// piece opened already, its start having passed the fence in the replay, adds nothing.
func (d *driver) freshRetry(key string, q *piece, from time.Time) error {
	if q.state != pieceWaiting {
		return nil
	}
	issue, err := d.recorded(key)
	if err != nil {
		return err
	}
	refuse := func(standing phase.Phase, reason string) {
		d.counters.Refused = append(d.counters.Refused, RefusedMove{Issue: key, At: d.clock.Instant(), From: standing, To: q.phase, Reason: reason})
	}
	if issue == nil || !record.InWorkflow(*issue) || record.StandsAt(*issue) != q.phase {
		standing := phase.Phase("")
		if issue != nil {
			standing = record.StandsAt(*issue)
		}
		refuse(standing, fmt.Sprintf("the operator's retry of the %s of %s with a fresh session finds it in %s, not %s", q.role, key, standing, q.phase))
		return nil
	}
	if err := d.freshLaunch(key, q.role); err != nil {
		return err
	}
	d.openPieceFrom(q, from.Add(q.start.Sub(q.visit.Enter)))
	result, err := d.apply("api", d.factID("fresh-session", key), intake.RestartPhase{Issue: key, Role: q.role})
	if err != nil {
		return err
	}
	if result.Refusal != nil {
		refuse(q.phase, result.Refusal.Message)
	}
	return nil
}

// freshLaunch launches key's role's claim afresh, as supervise's resume of a suspended claim does
// for the operator's fresh session: admitted and launched where the stub holds none (ensureClaim),
// and otherwise launched again, its turn under way ended first. The claim takes the entry its
// role's phases row names (record.PhaseRow.Entry), so a start of that entry the fence held, which
// passes it later, opens no more work (startWork), the work having opened here.
func (d *driver) freshLaunch(key string, role claim.Role) error {
	token, err := claim.NewToken(d.token, key, role)
	if err != nil {
		return err
	}
	c, had := d.claims[token]
	if had {
		if d.turnOpen(c) {
			if err := d.writeTurnEnd(key, role); err != nil {
				return err
			}
			c = d.claims[token]
		}
		d.launch(&c, false)
	} else {
		var held bool
		if c, held, err = d.ensureClaim(key, role); err != nil || !held {
			return err
		}
	}
	entry, err := d.phaseEntry(key, role)
	if err != nil {
		return err
	}
	c.GivenEntry = max(c.GivenEntry, entry)
	return d.putClaim(c)
}

// claimFailed enters a recorded claim failure: the claim the stub holds is failed, its turn
// ended, the line written, and intake.ClaimFailed applied, which holds the issue where the
// claim's role works the phase it stands in, and otherwise only tells its architect.
func (d *driver) claimFailed(failed ClaimFailed) error {
	key, role := failed.Claim.Issue, failed.Claim.Role
	token, err := claim.NewToken(d.token, key, role)
	if err != nil {
		return err
	}
	if c, ok := d.claims[token]; ok {
		if d.turnOpen(c) {
			if err := d.writeTurnEnd(key, role); err != nil {
				return err
			}
		}
		c = d.claims[token]
		c.State, c.FailedAt, c.Failure, c.FailureCause = supervise.StateFailed, d.now(), failed.Why, supervise.FailureCause(failed.Cause)
		if err := d.putClaim(c); err != nil {
			return err
		}
	}
	d.log.Error(claimFailedLine, "claim", string(token), "cause", failed.Cause, "why", failed.Why)
	_, err = d.apply("supervise", d.factID("claim-failed", key, role), intake.ClaimFailed{Issue: key, Role: role, Reason: failed.Why,
		Cause: supervise.FailureCause(failed.Cause)})
	return err
}

// register enters the architect's registration of a design document (intake.GateRegistered),
// rebuilt from its recorded line and the shadow's reading of the document, as register_gate reads
// Dispatch's approval of it: its latest version, the approval or changes request on that version,
// the approval request open on it, and the issue's sequence before the read (Since).
func (d *driver) register(key string, gate GateFact) error {
	read := d.shadow.approvalOf(key, gate.Artifact)
	fact := intake.GateRegistered{Issue: key, Step: gate.Step, ArtifactID: gate.Artifact, Version: gate.Version, DispatchVersion: read.latest,
		RequestAsk: read.ask, RequestWaitsOnAgent: read.ask != "" && read.waitingOn == "agent", ReviewedBy: read.latestReviewer}
	if iss, ok := d.shadow.issues[key]; ok {
		fact.Since = record.EventMark{Seq: iss.seq}
	}
	if read.approved != nil && read.approved.version >= max(gate.Version, read.latest) {
		fact.ApprovedVersion, fact.ApprovedMark = read.approved.version, read.approved.mark
	}
	if read.changes != nil && read.changes.version >= max(gate.Version, read.latest) {
		fact.ChangesRequestedVersion, fact.ChangesReason, fact.ChangesRequestedMark = read.changes.version, read.changes.reason, read.changes.mark
	}
	_, err := d.apply("api", d.factID("gate", key, gate.Artifact, gate.Version), fact)
	return err
}

// adopt enters an architect's release of the queued roots joined into its tree
// (intake.ChildAdoption), each as the shadow shows it.
func (d *driver) adopt(joined []RootJoined) error {
	if len(joined) == 0 {
		return nil
	}
	fact := intake.ChildAdoption{Tree: joined[0].Tree}
	for _, line := range joined {
		child := intake.AdoptedChild{Issue: line.Issue}
		if iss, ok := d.shadow.issues[line.Issue]; ok {
			child.Parent, child.Status, child.BlockedBy, child.Seq = iss.parent(), iss.status(), iss.blockers(), iss.seq
		}
		fact.Children = append(fact.Children, child)
	}
	_, err := d.apply("api", d.factID("adopt", fact.Tree), fact)
	return err
}

// closeRoot enters the architect's close of its root before the first phase (intake.CloseRoot).
func (d *driver) closeRoot(key string) error {
	_, err := d.apply("api", d.factID("close-root", key), intake.CloseRoot{Issue: key, Reason: "the record's close of the root by its architect"})
	return err
}

// readyMark enters the merger's ready mark of key inside merging (ReadyMark, assumption 20).
func (d *driver) readyMark(key string) error {
	d.used[19], d.used[20] = true, true
	issue, err := d.recorded(key)
	if err != nil || issue == nil {
		return err
	}
	_, err = d.apply("replay", d.factID("ready", key), ReadyMark{Issue: key, Generation: issue.Generation})
	return err
}

// ensurePullRequest is the replay's own pull request record of key, written where it holds none,
// on the issue's repository and branch against its default branch, at the number the run gives
// key's record in that repository (pullRequestNumbers), its head arriving now as a live webhook's
// would: the record the daemon's guards read. It answers the record as the database holds it once
// written, read back in the same transaction; nil for an issue the replay does not record.
func (d *driver) ensurePullRequest(key string) (*record.PullRequest, error) {
	pr, err := d.pullRequestOf(key)
	if err != nil || pr != nil {
		return pr, err
	}
	issue, err := d.recorded(key)
	if err != nil || issue == nil {
		return nil, err
	}
	d.used[assumeNoGitHub] = true
	arrived := d.now()
	err = store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := d.records.PutPullRequest(ctx, tx, record.PullRequest{Issue: key, Repo: issue.Repo, Number: d.numbers.of(key, issue.Repo),
			Branch: claim.IssueBranch(key), Base: issue.DefaultBranch, HeadUpdatedAt: arrived, State: record.PullRequestOpen}); err != nil {
			return err
		}
		var err error
		pr, err = d.records.PullRequest(ctx, tx, key)
		return err
	})
	return pr, err
}

// pullRequestNumbers is the number of the replay's own pull request record of each issue
// (assumption 29), no two issues' alike in one repository. GitHub numbers a repository's pull
// requests, and the daemon finds one by its repository and number, the repository compared
// without case (record.Postgres.PullRequestByNumber's lower(repo)), so two records of one number
// in one repository would leave which issue a merge, a merge-state read or a push of that number
// reaches to the order the database answers the two rows in, which no statement fixes and which
// differs from run to run; two in different repositories are each GitHub's own and meet nothing.
type pullRequestNumbers struct {
	numbered map[string]repositoryNumber
	// top is the highest number given in each repository, by its name in lower case, above which
	// a number is made there.
	top map[string]int
}

// repositoryNumber is a pull request's number and its repository's name in lower case.
type repositoryNumber struct {
	repo   string
	number int
}

// newPullRequestNumbers numbers the pull request record of each issue of t in the repository the
// daemon records it on (IssueTrace.Repo, which the start state writes as the issue's Repo and the
// engine records at its entry), in key order, so a run numbers them the same at every run of the
// window: first the number a line of the record names for the issue's pull request
// (IssueTrace.PullRequestNumber), where no issue before it in that order takes the same in that
// repository; then, for each issue still unnumbered, the number of its own key, where no issue
// takes it in that repository already (a key's number can be the recorded number of another
// issue's pull request); and last, for each issue left, the next number above every number given
// in its repository.
func newPullRequestNumbers(t Trace) *pullRequestNumbers {
	keys := slices.Sorted(maps.Keys(t.Issues))
	n := &pullRequestNumbers{numbered: make(map[string]repositoryNumber, len(keys)), top: map[string]int{}}
	taken := make(map[repositoryNumber]bool, len(keys))
	give := func(key string, at repositoryNumber) {
		n.numbered[key], taken[at], n.top[at.repo] = at, true, max(n.top[at.repo], at.number)
	}
	for _, key := range keys {
		at := repositoryNumber{repo: strings.ToLower(t.Issues[key].Repo), number: t.Issues[key].PullRequestNumber}
		if at.number > 0 && !taken[at] {
			give(key, at)
		}
	}
	var left []string
	for _, key := range keys {
		if _, numbered := n.numbered[key]; numbered {
			continue
		}
		_, suffix, _ := strings.Cut(key, "-")
		number, err := strconv.Atoi(suffix)
		if at := (repositoryNumber{repo: strings.ToLower(t.Issues[key].Repo), number: number}); err == nil && number > 0 && !taken[at] {
			give(key, at)
			continue
		}
		left = append(left, key)
	}
	for _, key := range left {
		n.of(key, t.Issues[key].Repo)
	}
	return n
}

// of is the number of key's pull request record in repo: the number the run gave it there, or,
// for an issue the window's trace does not hold or records on another repository, the next number
// above every number given in repo, which it keeps.
func (n *pullRequestNumbers) of(key, repo string) int {
	repo = strings.ToLower(repo)
	if given, numbered := n.numbered[key]; numbered && given.repo == repo {
		return given.number
	}
	n.top[repo]++
	n.numbered[key] = repositoryNumber{repo: repo, number: n.top[repo]}
	return n.top[repo]
}

// pullRequestOf is the pull request record key holds in the replay's database, nil for none.
func (d *driver) pullRequestOf(key string) (*record.PullRequest, error) {
	var pr *record.PullRequest
	err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pr, err = d.records.PullRequest(ctx, tx, key)
		return err
	})
	return pr, err
}

// turnStarts is a recorded turn of role's worker beginning in p: its claim is in a turn.
func (d *driver) turnStarts(p *piece, role claim.Role) error {
	p.turns[role] = true
	return d.writeTurnStart(p.issue, role)
}

// turnEnds is a turn of role's worker in p ending.
func (d *driver) turnEnds(p *piece, role claim.Role) error {
	if !p.turns[role] {
		return nil
	}
	p.turns[role] = false
	return d.writeTurnEnd(p.issue, role)
}
