package api

import (
	"context"
	"log"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// work recomputes g's dirty views and the expired ones, one at a time, and keeps the freshness
// verdict on its threshold, until g ends. One timer, re-armed after every pass, wakes it at the
// earliest moment a view is due or the reconciled sources cross their threshold; nothing else
// wakes it but a mark.
func (h *deliveryHub) work(g *hubGeneration) {
	select {
	case <-g.ctx.Done():
		return
	case <-g.ready:
	}
	var timer hubTimer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		for h.runDue(g) {
		}
		now := h.clock.Now()
		h.mu.Lock()
		h.judgeLocked(g, now)
		next, ok := h.nextWakeLocked(g, now)
		h.mu.Unlock()
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		if ok {
			timer = h.clock.At(next, func() { signal(g.wakeWork) })
		}
		select {
		case <-g.ctx.Done():
			return
		case <-g.wakeWork:
		}
	}
}

// runDue recomputes, or narrows, the view that is due earliest, and is false when none is due.
func (h *deliveryHub) runDue(g *hubGeneration) bool {
	now := h.clock.Now()
	h.mu.Lock()
	var due *deliveryView
	var dueAt time.Time
	var cause string
	for _, view := range g.views {
		at, why, ok := view.dueAt()
		if ok && !at.After(now) && (due == nil || at.Before(dueAt)) {
			due, dueAt, cause = view, at, why
		}
	}
	h.mu.Unlock()
	switch {
	case due == nil:
		return false
	case cause == deliveryCauseExpiry:
		h.expire(g, due)
	default:
		h.recompute(g, due, cause)
	}
	return true
}

// dueAt is when the view is next due, and why: a dirty view viewQuiet after its last mark or
// viewMaxWait after its first, whichever is sooner (at once for a subscribe's), not before a
// failed read's retry; a clean one with an answer at its expiry.
func (view *deliveryView) dueAt() (time.Time, string, bool) {
	if view.computing {
		return time.Time{}, "", false
	}
	if view.dirty {
		at := view.lastDirty.Add(viewQuiet)
		if latest := view.firstDirty.Add(viewMaxWait); latest.Before(at) {
			at = latest
		}
		if view.immediate {
			at = view.firstDirty
		}
		if at.Before(view.notBefore) {
			at = view.notBefore
		}
		return at, view.cause, true
	}
	if view.body != nil && !view.expiry.IsZero() {
		return view.expiry, deliveryCauseExpiry, true
	}
	return time.Time{}, "", false
}

// nextWakeLocked is the earliest moment after now a view is due or g's verdict's reconciled
// sources cross their threshold.
func (h *deliveryHub) nextWakeLocked(g *hubGeneration, now time.Time) (time.Time, bool) {
	var next time.Time
	consider := func(at time.Time) {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	for _, view := range g.views {
		if at, _, ok := view.dueAt(); ok {
			consider(at)
		}
	}
	if g.known && g.fresh.lastReconcileAt != nil {
		if crossing := g.fresh.lastReconcileAt.Add(deliveryStaleAfter).Add(time.Nanosecond); crossing.After(now) {
			consider(crossing)
		}
	}
	return next, !next.IsZero()
}

// deliveryViewRead is one recompute's read and compute, how long each of its stages took, and
// the marks it took over from the view (deliveryView.marks), by what made them.
type deliveryViewRead struct {
	seq               int64
	changedAt         *time.Time
	settings          delivery.DeliverySettings
	pop               deliveryPopulation
	pipe              pipelinePopulation
	from, at          time.Time
	body              deliveryLiveEncoded
	expiry            time.Time
	issues, ancestors map[string]struct{}
	sessions          map[string]string
	prs, runs         int
	unfetchable       int
	times             deliveryRecomputeTimes
	marks             map[string]int
}

// recompute answers one dirty view from a fresh read: the newest change position first, which
// takes over every mark made until then (read), so a change committed during the read lands past
// it and marks the view again; then the GET route's population read, the ancestors of the issues
// it names, and the pure compute. It sends each sub the delta against what it holds (a snapshot to
// a sub with none), or the refusal its read answered (viewReads.refusal) to a sub not yet told.
func (h *deliveryHub) recompute(g *hubGeneration, view *deliveryView, cause string) {
	started := time.Now()
	h.mu.Lock()
	view.computing = true
	spec, epoch := view.spec, g.epoch
	h.mu.Unlock()
	h.note(deliveryHubNote{what: "recompute begin", view: spec.key, cause: cause})

	read, err := h.read(g, view, spec, epoch)
	h.note(deliveryHubNote{what: "recompute read", view: spec.key, cause: cause, through: read.seq})

	now := h.clock.Now()
	classes := spec.classes()
	code, message, refused := classes.reads.refusal(err)
	h.mu.Lock()
	view.computing, view.readingSeq = false, 0
	sent := 0
	switch {
	case g.views[spec.key] != view:
		// Every sub left while it read, and the generation stopped if they were the last.
	case refused:
		view.body, view.expiry, view.seq = nil, time.Time{}, read.seq
		for sub := range view.subs {
			if !sub.told {
				sub.conn.Send(liveRefusal(sub.id, code, message))
				sub.told = true
				sent++
			}
		}
	case err != nil:
		if g.ctx.Err() == nil {
			slog.Warn("dispatch: delivery live recompute failed; retrying", "view", spec.kind, "error", err)
		}
		// A read that failed past its position took the marks over and is marked again here; one
		// that failed before it took over nothing, and the view is still dirty from them.
		if !view.dirty {
			view.dirty, view.firstDirty, view.cause = true, now, cause
		}
		view.lastDirty, view.notBefore = now, now.Add(deliveryLiveRetry)
		view.countMark(deliveryMarkRetry)
	default:
		if g.epoch != epoch {
			// The change log was reset while it read, so its position names nothing in the new
			// one, and no change of the new log was recorded at it.
			read.seq, read.changedAt = 0, nil
		}
		view.settings, view.pop, view.pipe, view.readFrom = read.settings, read.pop, read.pipe, read.from
		view.body, view.seq, view.changedAt = &read.body, read.seq, read.changedAt
		view.from, view.at, view.expiry = read.from, read.at, read.expiry
		view.issues, view.ancestors, view.sessions = read.issues, read.ancestors, read.sessions
		sending := time.Now()
		sent = h.sendLocked(g, view)
		read.times.diff = time.Since(sending)
		if classes.reads != viewReadsDelivery {
			// A view that reads no delivery row holds no delivery settings, so it judges no
			// freshness: the verdict stays the one the delivery views' own reads set.
			break
		}
		if !g.known {
			// The first verdict is set silently, from the first settings read.
			g.fresh = deliveryFreshnessHeard{
				lastEventAt: read.settings.LastEventAt, lastReconcileAt: read.settings.LastReconcileAt,
				lastError: read.settings.LastError, unfetchable: read.unfetchable,
			}
			g.sent = g.verdictLocked(now)
			g.known = true
		} else {
			g.fresh.unfetchable = read.unfetchable
			h.judgeLocked(g, now)
		}
	}
	// What the events heard since the read took its position touched is checked against the
	// answer it installed, or the last one when it installed none (recheckHeardLocked), and the
	// record is cleared.
	h.recheckHeardLocked(g, view, now)
	subscribers := len(view.subs)
	h.mu.Unlock()
	h.finishRecompute(spec, cause, started, read.prs, read.runs, subscribers, sent, read.times, read.marks)
}

// read is recompute's read and compute, outside the hub's lock, on g's context. Once it holds the
// newest change position it takes over the view's marks, under the lock: every change, event and
// subscribe that marked the view until then was committed before the reads that follow, which
// answer it, so it clears dirty, and from there a change at or before the position marks nothing
// (readingSeq) while what the intake hears about the view is kept for recompute to judge the
// read's answer by (reading, recheckHeardLocked). A reset of the change log since the recompute
// began leaves dirty set: the position may name the old log, so the marks are taken over only
// while the generation stands at the epoch the recompute read.
func (h *deliveryHub) read(g *hubGeneration, view *deliveryView, spec deliveryLiveSpec, epoch int64) (deliveryViewRead, error) {
	ctx := g.readContext()
	var read deliveryViewRead
	started := time.Now()
	head, err := delivery.ChangesHead(ctx, h.server.deps.Store.Pool)
	read.times.read = time.Since(started)
	if err != nil {
		return read, err
	}
	read.seq, read.changedAt = head.Seq, head.At
	h.note(deliveryHubNote{what: "recompute position", view: spec.key, through: read.seq})
	h.mu.Lock()
	view.readingSeq, view.reading = read.seq, true
	if g.epoch == epoch {
		// The log has not been reset since the recompute began, so the position names the log the
		// marks were made against and the reads that follow answer them. A reset leaves them:
		// the position may name the old log, and the reset's own mark recomputes the view again.
		view.dirty, view.immediate = false, false
		read.marks, view.marks = view.marks, nil
	}
	h.mu.Unlock()
	now := h.clock.Now()
	// The kind's read times its own compute (deliveryLiveClasses.read), and the rest of its call
	// is its reads; the hub's lock and note between the position and the call time as neither.
	started = time.Now()
	body, err := spec.classes().read(h, ctx, spec, now, &read)
	read.times.read += time.Since(started) - read.times.compute
	if err != nil {
		return read, err
	}
	started = time.Now()
	read.body, read.times.revision, err = encodeDeliveryBody(spec.kind, body)
	read.times.encode = time.Since(started) - read.times.revision
	return read, err
}

// readLegionBoardView is a Legion board's read: the project's own daemon state as the Legion
// intake recorded it, and its stored facts closed at now, the moment the body is answered as of.
// It reads no delivery settings — its own are the project's Legion setting, and a deployment that
// configured one and not the other still shows the board. A project that does not exist is
// loadProject's refusal, as GET /api/v1/projects/{key}/legion/board answers it.
func (h *deliveryHub) readLegionBoardView(ctx context.Context, spec deliveryLiveSpec, now time.Time, read *deliveryViewRead) (any, error) {
	if _, err := h.server.loadProject(ctx, h.server.deps.Store.Pool, spec.project); err != nil {
		return nil, err
	}
	board, err := h.server.readLegionBoard(ctx, spec.project, now)
	if err != nil {
		return nil, err
	}
	computing := time.Now()
	// Every issue the daemon reports, so an event about one marks the view (dirtyIssueLocked); a
	// stored facts row moves it through the issue_facts change kind (touchedBy) instead, and the
	// board shows no session, so it names none.
	read.issues = board.issueKeys()
	body := legionBoardCompute(board, now)
	read.times.compute = time.Since(computing)
	read.times.board = board.times
	return body, nil
}

// readLegionIssueView is a legion_issue view's read: GET /api/v1/issues/{key}/legion's body, through
// the route's own readLegionIssue, so the view and the route answer one body. A key Dispatch holds
// no issue of is that function's *apiError, NOT_FOUND in the route's words, which the hub refuses
// the view's subscribers with once (viewReads.refusal). A project that names no daemon is the body
// whose configured is false and never an error, so this view, unlike the board, is never refused
// LEGION_NOT_CONFIGURED. It names no issue in read.issues: no Dispatch event moves the body, which
// only its project's notifications and its issue's runs move (dirtyLegionProject, dirtyLegionIssue).
// It times no compute: the route's read builds the body inside its one transaction, so the whole
// of it is the recompute's read stage.
func (h *deliveryHub) readLegionIssueView(ctx context.Context, spec deliveryLiveSpec, now time.Time, read *deliveryViewRead) (any, error) {
	answer, err := h.server.readLegionIssue(ctx, spec.issue)
	if err != nil {
		return nil, err
	}
	return answer, nil
}

// readPipelineView is a pipeline view's read: the delivery settings and the runs, jobs and merges
// of the seven days ending on its at, or on the present for a view that names none.
func (h *deliveryHub) readPipelineView(ctx context.Context, spec deliveryLiveSpec, now time.Time, read *deliveryViewRead) (any, error) {
	pool := h.server.deps.Store.Pool
	var err error
	if read.settings, err = delivery.GetSettings(ctx, pool); err != nil {
		return nil, err
	}
	read.at = now
	if spec.at != nil {
		read.at = *spec.at
	}
	if read.pipe, err = h.server.readPipelinePopulation(ctx, read.settings, read.at); err != nil {
		return nil, err
	}
	computing := time.Now()
	body := deliveryPipelineCompute(read.settings, read.pipe, read.at, now)
	if spec.at == nil {
		read.expiry = pipelineNextExpiry(read.pipe, now)
	}
	read.from = measures.WindowStart(read.at)
	read.prs, read.runs = len(read.pipe.leadTimes.prs), len(read.pipe.checks.Runs)+len(read.pipe.deploys.Runs)
	read.unfetchable = read.pipe.leadTimes.unfetchable
	read.times.compute = time.Since(computing)
	return body, nil
}

// readDeliveryView is a timeline or measures view's read: the delivery settings, the population
// of its window and facets, and, where that population is labeled, the ancestors of every issue
// it names.
func (h *deliveryHub) readDeliveryView(ctx context.Context, spec deliveryLiveSpec, now time.Time, read *deliveryViewRead) (any, error) {
	pool := h.server.deps.Store.Pool
	var err error
	if read.settings, err = delivery.GetSettings(ctx, pool); err != nil {
		return nil, err
	}
	from, to := spec.from, spec.to
	if spec.preset > 0 {
		from, to = now.Add(-spec.days()), now
	}
	if read.pop, err = h.server.readDeliveryPopulation(ctx, read.settings, from, to, spec.query, spec.kind == deliveryLiveTimeline); err != nil {
		return nil, err
	}
	read.issues = map[string]struct{}{}
	read.sessions = map[string]string{}
	labeled := read.pop.labeled()
	name := func(issue *string, sessions []string) {
		// A population read without its labels (a measures view selecting nothing) shows no fact
		// of the issues its pull requests name and no session by its title, so no write of those
		// issues, of their ancestors or of a session's title moves its answer: it records none of
		// them, and so reads no ancestors. The one issue fact it shows, the unowned P0 list, moves
		// with a P0's write, which showsIssue's p0 rule marks every view showing that list for.
		if !labeled {
			return
		}
		if issue != nil {
			read.issues[*issue] = struct{}{}
		}
		for _, session := range sessions {
			read.sessions[session] = delivery.SessionTitle(read.pop.titles, session)
		}
	}
	for _, pr := range read.pop.prs {
		name(pr.IssueKey, pr.Sessions)
	}
	for _, pr := range read.pop.waiting {
		name(pr.IssueKey, pr.Sessions)
	}
	if read.ancestors, err = delivery.IssueAncestors(ctx, pool, slices.Sorted(maps.Keys(read.issues)), parentDepthCap); err != nil {
		return nil, err
	}
	computing := time.Now()
	body := deliveryLiveCompute(spec.kind, read.settings, read.pop, from, to, now)
	if spec.preset > 0 {
		read.expiry = nextExpiry(read.pop, spec.days(), now)
	}
	read.from = from
	read.prs, read.runs = len(read.pop.prs), len(read.pop.applyRuns)+len(read.pop.windowRuns)
	read.unfetchable = read.pop.unfetchable
	read.times.compute = time.Since(computing)
	return body, nil
}

// expire answers a view whose preset window, or whose pipeline day, moved past something it holds,
// reading nothing: the compute over the population as last read, narrowed to the window now, with
// the settings that read held. The population and its read's start stay as read, and the next
// expiry comes from the narrowed population.
func (h *deliveryHub) expire(g *hubGeneration, view *deliveryView) {
	started := time.Now()
	h.mu.Lock()
	view.computing = true
	spec, settings, pop, pipe := view.spec, view.settings, view.pop, view.pipe
	h.mu.Unlock()
	h.note(deliveryHubNote{what: "recompute begin", view: spec.key, cause: deliveryCauseExpiry})

	now := h.clock.Now()
	var times deliveryRecomputeTimes
	computing := time.Now()
	var body any
	var from, at, expiry time.Time
	var prs, runs int
	if spec.kind == deliveryLivePipeline {
		at = now
		narrowed := pipe.narrowedTo(settings, at)
		body = deliveryPipelineCompute(settings, narrowed, at, now)
		expiry = pipelineNextExpiry(narrowed, now)
		from = measures.WindowStart(at)
		prs, runs = len(narrowed.leadTimes.prs), len(narrowed.checks.Runs)+len(narrowed.deploys.Runs)
	} else {
		from = now.Add(-spec.days())
		narrowed := pop.narrowedTo(settings, from)
		body = deliveryLiveCompute(spec.kind, settings, narrowed, from, now, now)
		expiry = nextExpiry(narrowed, spec.days(), now)
		prs, runs = len(narrowed.prs), len(narrowed.applyRuns)+len(narrowed.windowRuns)
	}
	times.compute = time.Since(computing)
	encoding := time.Now()
	encoded, revision, err := encodeDeliveryBody(spec.kind, body)
	times.encode, times.revision = time.Since(encoding)-revision, revision

	h.mu.Lock()
	view.computing = false
	sent := 0
	switch {
	case g.views[spec.key] != view:
	case err != nil:
		slog.Error("dispatch: delivery live expiry could not encode its body", "view", spec.kind, "error", err)
		view.expiry = time.Time{}
	default:
		view.body, view.from, view.at, view.expiry = &encoded, from, at, expiry
		sending := time.Now()
		sent = h.sendLocked(g, view)
		times.diff = time.Since(sending)
	}
	subscribers := len(view.subs)
	h.mu.Unlock()
	h.finishRecompute(spec, deliveryCauseExpiry, started, prs, runs, subscribers, sent, times, map[string]int{deliveryCauseExpiry: 1})
}

// deliveryRecomputeTimes is how long each stage of one recompute took, which its log line
// carries (finishRecompute), so a run whose recomputes slowed says which stage the time went to:
//
//   - read, the database reads: the change position and the kind's own reads, with what the kind
//     does between them (the board's placing of its cards, the closing of its facts at now);
//   - compute, the body built from what was read, and what the hub keeps of it beside the body
//     (the issues an event marks the view by, the next expiry);
//   - encode, the body's JSON encoding and its reading back into the list items, map entries,
//     counts and fields a delta sends (encodeDeliveryBody), but for their revisions; for a Legion
//     board, its issues entries' own encodings and the body written from them and the rest
//     (legionBoardResponse's encodedMap, composeDeliveryBody), of which only the rest is read back;
//   - revision, the revisions of those (deliveryLiveRevision, deliveryLiveRevisionWithout, and a
//     Legion board's own of its issues map, legionBoardResponse's mapRevisions), the timeline's
//     maps and lists among them;
//   - diff, the send to every subscriber of its snapshot or of its delta against the revisions
//     it holds (sendLocked), each built and queued on its connection.
//
// board is the parts of a Legion board's read, inside the read stage (legionBoardReadTimes),
// and 0 for every other kind.
//
// A stage the recompute did not run is 0: an expiry reads nothing, a failed read encodes nothing,
// and a refused or failed read sends no subscriber a body. The hub's lock and its test notes are
// in no stage, so the stages sum to a little less than the recompute's elapsed time.
type deliveryRecomputeTimes struct {
	read, compute, encode, revision, diff time.Duration
	board                                 legionBoardReadTimes
}

// finishRecompute records one recompute's shared log line and test observation. The line carries
// the recompute's elapsed time, its counts and then the time of each of its stages
// (deliveryRecomputeTimes) as read_ms, compute_ms, encode_ms, revision_ms and diff_ms. The stages
// come after every other field, since legionbench's drive reads the line only up to elapsed_ms
// (recomputeLine), and they keep a tenth of a millisecond where elapsed_ms is whole: a stage that
// ran, a delta for one subscriber or the whole compute of a small view, is routinely under a
// millisecond, and a whole number would log it 0, as a stage that did not run is.
//
// After the stages come the parts of the read stage, to a tenth of a millisecond each, as
// read_cards_ms, read_facts_ms, read_grow_ms, read_open_ms, read_open_grow_ms, read_cursor_ms and
// read_wait_ms (a Legion board's, legionBoardReadTimes, 0.0 for every other kind and for a board
// read that did not reach them), and read_other_ms, the read stage less a board's cards, facts and
// grow, which run one after the other: the change position, the project's own read (loadProject),
// the read of the open rows or of the cursor where either outlasts those three beside it, and the
// whole of any other kind's reads. read_open_ms, read_open_grow_ms, read_cursor_ms and
// read_wait_ms lie inside the other parts rather than beside them. Last comes marks, every mark
// of the view the recompute took over (recomputeMarks), and expiry:1 for an expiry's, which
// answers no mark.
func (h *deliveryHub) finishRecompute(spec deliveryLiveSpec, cause string, started time.Time, prs, runs, subscribers, sent int, times deliveryRecomputeTimes, marks map[string]int) {
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	board := times.board
	other := times.read - board.cards - board.facts - board.grow
	log.Printf("dispatch: delivery live recompute view=%s cause=%s elapsed_ms=%d prs=%d runs=%d subscribers=%d sent=%d read_ms=%.1f compute_ms=%.1f encode_ms=%.1f revision_ms=%.1f diff_ms=%.1f read_cards_ms=%.1f read_facts_ms=%.1f read_grow_ms=%.1f read_open_ms=%.1f read_open_grow_ms=%.1f read_cursor_ms=%.1f read_wait_ms=%.1f read_other_ms=%.1f marks=%s",
		spec.kind, cause, time.Since(started).Milliseconds(), prs, runs, subscribers, sent,
		ms(times.read), ms(times.compute), ms(times.encode), ms(times.revision), ms(times.diff),
		ms(board.cards), ms(board.facts), ms(board.grow), ms(board.open), ms(board.openGrow), ms(board.cursor), ms(board.wait), ms(other),
		recomputeMarks(marks))
	h.note(deliveryHubNote{what: "recompute end", view: spec.key, cause: cause, sent: sent})
}

// recomputeMarks is the marks field of a recompute's line: each kind of mark it took over, by
// kind in order, with how many of it, as kind:n joined by commas, and none where it took over
// none (a read that failed before its change position, or ran across a reset of the change log,
// leaves the view's marks to the recompute after it).
func recomputeMarks(marks map[string]int) string {
	if len(marks) == 0 {
		return "none"
	}
	var field []byte
	for _, kind := range slices.Sorted(maps.Keys(marks)) {
		if len(field) > 0 {
			field = append(field, ',')
		}
		field = append(append(field, kind...), ':')
		field = strconv.AppendInt(field, int64(marks[kind]), 10)
	}
	return string(field)
}

// deliveryLiveCompute is the timeline's or the measures' body for [from, to).
func deliveryLiveCompute(kind string, settings delivery.DeliverySettings, pop deliveryPopulation, from, to, now time.Time) any {
	if kind == deliveryLiveTimeline {
		return deliveryTimelineCompute(settings, pop, from, to, now)
	}
	return deliveryMeasuresCompute(settings, pop, from, to, now)
}

// sendLocked answers every sub of view from its body, and counts the frames it sent.
func (h *deliveryHub) sendLocked(g *hubGeneration, view *deliveryView) int {
	sent := 0
	for sub := range view.subs {
		if h.answerLocked(g, sub) {
			sent++
		}
	}
	return sent
}

// answerLocked sends sub what it needs to hold its view's body (answerBodyLocked) and, after its
// first answer, the verdict the generation knows, when it knows one (Subscribe). It reports
// whether it sent sub a snapshot or a delta.
func (h *deliveryHub) answerLocked(g *hubGeneration, sub *deliverySub) bool {
	sent := h.answerBodyLocked(sub)
	if sub.needsVerdict {
		sub.needsVerdict = false
		if g.known {
			sub.conn.Send(g.freshnessFrameLocked(g.sent))
		}
	}
	return sent
}

// answerBodyLocked sends sub the snapshot, when it holds none, or the delta against its
// revisions, when anything differs.
func (h *deliveryHub) answerBodyLocked(sub *deliverySub) bool {
	view := sub.view
	sub.told = false
	if sub.needsSnapshot {
		sub.conn.Send(view.body.snapshot(sub.id, view.seq, view.changedAt))
		sub.revs, sub.needsSnapshot = view.body.revs, false
		return true
	}
	delta, changed := diffDeliveryBody(*view.body, sub.revs)
	if !changed {
		return false
	}
	delta.Sub, delta.Seq, delta.ChangedAt = sub.id, strconv.FormatInt(view.seq, 10), view.changedAt
	sub.conn.Send(delta)
	sub.revs = view.body.revs
	return true
}
