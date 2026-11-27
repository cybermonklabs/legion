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

// work recomputes g's dirty views and the expired ones, and keeps the freshness verdict on its
// threshold, until g ends: each delivery view, Legion view and expiry itself, one at a time, and
// each evals view, which its expiry reads again too, on a goroutine of its own, up to
// evalsRecomputes at once (recomputeAside). One timer, re-armed after every pass, wakes it at the
// earliest moment a view is due or the reconciled sources cross their threshold; nothing else
// wakes it but a mark and the end of an evals view's recompute, after which a mark made during it
// falls due (dueAt).
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

// runDue recomputes, or narrows, the view that is due earliest, and is false when none is due or
// when g ended while an evals view waited for a slot. An evals view is read again whatever it is
// due for, its expiry among them, since its expiry is the moment the refresher's age it states
// moves a step (readEvalsView), which no narrowing reads; it is set computing before the lock is
// let go, so no later pass picks it while its recompute waits for a slot or runs. Every other
// view's expiry narrows what it read last (expire).
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
	aside := due != nil && due.spec.classes().reads == viewReadsEvals
	if aside {
		due.computing = true
	}
	h.mu.Unlock()
	switch {
	case due == nil:
		return false
	case aside:
		return h.recomputeAside(g, due, cause)
	case cause == deliveryCauseExpiry:
		h.expire(g, due)
	default:
		h.recompute(g, due, cause)
	}
	return true
}

// recomputeAside recomputes view, an evals view runDue set computing, on a goroutine of its own
// once one of g's evalsRecomputes slots is free, and is false where g ended first, leaving view
// computed by none. The goroutine frees its slot once recompute has ended and wakes the worker:
// a mark made during the recompute left view dirty, and computing kept it due nowhere until then
// (dueAt). Each recompute's read takes a pool mark of its own (readContext), so the reads that run
// at once refuse each other no connection. A view is computed by one recompute at a time, which
// sends its frames in the hold of the lock that ends it, so the deltas of one view go out in the
// order of its reads.
func (h *deliveryHub) recomputeAside(g *hubGeneration, view *deliveryView, cause string) bool {
	select {
	case g.evalsSlots <- struct{}{}:
	case <-g.ctx.Done():
		h.mu.Lock()
		view.computing = false
		h.mu.Unlock()
		return false
	}
	h.running.Add(1)
	go func() {
		defer h.running.Done()
		h.recompute(g, view, cause)
		<-g.evalsSlots
		signal(g.wakeWork)
	}()
	return true
}

// dueAt is when the view is next due, and why: a dirty view viewQuiet after its last mark or
// viewMaxWait after its first, whichever is sooner, or at once for a mark that waits for no quiet
// (immediate, markDirtyLocked), never before a failed read's retry; a clean one with an answer at
// its expiry. A view being computed is not due, so it is never recomputed twice at once: a mark
// made meanwhile falls due by these rules once that recompute has ended.
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

// deliveryViewRead is one recompute's read and compute.
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
	// ageStep is an evals read's coarse refresher age (refresherAgeStep), which its body's
	// freshness revision holds, and 0 where the body states no age.
	ageStep int
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
		sent = h.sendLocked(g, view)
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
	h.finishRecompute(spec, cause, started, read.prs, read.runs, subscribers, sent)
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
	head, err := delivery.ChangesHead(ctx, h.server.deps.Store.Pool)
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
	}
	h.mu.Unlock()
	now := h.clock.Now()
	body, err := spec.classes().read(h, ctx, spec, now, &read)
	if err != nil {
		return read, err
	}
	read.body, err = encodeDeliveryBody(spec.kind, body)
	if err == nil && read.ageStep > 0 {
		read.body.foldRefresherAgeStep(read.ageStep)
	}
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
	// Every issue the daemon reports, so an event about one marks the view (dirtyIssueLocked); a
	// stored facts row moves it through the issue_facts change kind (touchedBy) instead, and the
	// board shows no session, so it names none.
	read.issues = board.issueKeys()
	return legionBoardCompute(board, now), nil
}

// readLegionIssueView is a legion_issue view's read: GET /api/v1/issues/{key}/legion's body, through
// the route's own readLegionIssue, so the view and the route answer one body. A key Dispatch holds
// no issue of is that function's *apiError, NOT_FOUND in the route's words, which the hub refuses
// the view's subscribers with once (viewReads.refusal). A project that names no daemon is the body
// whose configured is false and never an error, so this view, unlike the board, is never refused
// LEGION_NOT_CONFIGURED. It names no issue in read.issues: no Dispatch event moves the body, which
// only its project's notifications and its issue's runs move (dirtyLegionProject, dirtyLegionIssue).
func (h *deliveryHub) readLegionIssueView(ctx context.Context, spec deliveryLiveSpec, now time.Time, read *deliveryViewRead) (any, error) {
	answer, err := h.server.readLegionIssue(ctx, spec.issue)
	if err != nil {
		return nil, err
	}
	return answer, nil
}

// readEvalsView is an evals view's read: the list read of GET /api/v1/projects/{key}/evals, through
// the function the route answers with (readProjectEvalList), from the query the route would be
// sent (evalsQuery). So the body is the route's for the same query, byte for byte. A project no
// row holds is loadProject's errProjectNotFound, which the view refuses as the route answers it
// (viewReads.refusal, live_frames.go): its subscribers are told so once, with that refusal's code
// and message, and stay open, since the project's creation seeds its evals and the change rows of
// those versions mark the view, whose next read answers it.
//
// It names no issue and no session, and reads no delivery settings: what moves its rows is a write
// of its project's readings, versions or day rows, each of which the change log carries
// (touchedBy). A window is whole UTC days, which move at midnight, and the refresher's pass at
// midnight writes every project's rows of the new day, its unowned-p0 row at least, whatever its
// inputs (evals' walkDaily), whose change marks the view. The refresher's age the body states
// moves with the clock, which no change carries: the read keeps its step (refresherAgeStep) in
// the freshness revision, and the view expires when the age reaches the next step, when runDue
// reads it again, so its pages hold an age at most one step behind while no write moves it, a
// refresher that stopped among them. A body that states no age, where no pass has run, sets no
// expiry.
func (h *deliveryHub) readEvalsView(ctx context.Context, spec deliveryLiveSpec, now time.Time, read *deliveryViewRead) (any, error) {
	answer, err := h.server.readProjectEvalList(ctx, spec.evalsQuery(now), spec.project, now)
	if err != nil {
		return nil, err
	}
	if seconds := answer.Freshness.RefresherAgeSeconds; seconds != nil {
		age := time.Duration(*seconds) * time.Second
		step, next := refresherAgeStep(age)
		read.ageStep = step
		if next > 0 {
			read.expiry = now.Add(next - age)
		}
	}
	return answer, nil
}

// readPipelineView is a pipeline view's read: the delivery settings the stored day rows stand
// built under (recordedDeliverySettings) and the runs, jobs and merges of the seven days ending on
// its at, or on the present for a view that names none, with the min_n its slowest jobs are ranked
// by, through the GET route's own read and compute (readPipelinePopulation,
// deliveryPipelineCompute), so the view ranks them as the route does.
func (h *deliveryHub) readPipelineView(ctx context.Context, spec deliveryLiveSpec, now time.Time, read *deliveryViewRead) (any, error) {
	pool := h.server.deps.Store.Pool
	var err error
	if read.settings, err = recordedDeliverySettings(ctx, pool); err != nil {
		return nil, err
	}
	read.at = now
	if spec.at != nil {
		read.at = *spec.at
	}
	if read.pipe, err = h.server.readPipelinePopulation(ctx, read.settings, read.at); err != nil {
		return nil, err
	}
	body := deliveryPipelineCompute(read.settings, read.pipe, read.at, now)
	if spec.at == nil {
		read.expiry = pipelineNextExpiry(read.pipe, now)
	}
	read.from = measures.WindowStart(read.at)
	read.prs, read.runs = len(read.pipe.leadTimes.prs), len(read.pipe.checks.Runs)+len(read.pipe.deploys.Runs)
	read.unfetchable = read.pipe.leadTimes.unfetchable
	return body, nil
}

// readDeliveryView is a timeline or measures view's read: the delivery settings the stored day
// rows stand built under (recordedDeliverySettings), the population of its window and facets, and,
// where that population is labeled, the ancestors of every issue it names.
func (h *deliveryHub) readDeliveryView(ctx context.Context, spec deliveryLiveSpec, now time.Time, read *deliveryViewRead) (any, error) {
	pool := h.server.deps.Store.Pool
	var err error
	if read.settings, err = recordedDeliverySettings(ctx, pool); err != nil {
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
	body := deliveryLiveCompute(spec.kind, read.settings, read.pop, from, to, now)
	if spec.preset > 0 {
		read.expiry = nextExpiry(read.pop, spec.days(), now)
	}
	read.from = from
	read.prs, read.runs = len(read.pop.prs), len(read.pop.applyRuns)+len(read.pop.windowRuns)
	read.unfetchable = read.pop.unfetchable
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
	encoded, err := encodeDeliveryBody(spec.kind, body)

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
		sent = h.sendLocked(g, view)
	}
	subscribers := len(view.subs)
	h.mu.Unlock()
	h.finishRecompute(spec, deliveryCauseExpiry, started, prs, runs, subscribers, sent)
}

// finishRecompute records one recompute's shared log line and test observation.
func (h *deliveryHub) finishRecompute(spec deliveryLiveSpec, cause string, started time.Time, prs, runs, subscribers, sent int) {
	log.Printf("dispatch: delivery live recompute view=%s cause=%s elapsed_ms=%d prs=%d runs=%d subscribers=%d sent=%d",
		spec.kind, cause, time.Since(started).Milliseconds(), prs, runs, subscribers, sent)
	h.note(deliveryHubNote{what: "recompute end", view: spec.key, cause: cause, sent: sent})
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
