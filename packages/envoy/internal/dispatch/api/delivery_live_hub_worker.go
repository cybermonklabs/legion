package api

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// work recomputes g's dirty views and the expired ones, one at a time, and keeps the freshness
// verdict on its threshold, until g ends. One timer, re-armed after every pass, wakes it at the
// earliest moment a view is due or the reconciled sources cross their threshold; nothing else
// wakes it but a mark, and the socket's heartbeat (accessRecheckEvery), on which it resolves every
// sub's reader again: an access change that appends no event and announces nothing on
// store.AccessChangedChannel, one made by SQL alone, moves a reader all the same, and the re-check is
// what moves its sub to the view of the reader it is now (recheckSubs), as an announced change does.
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
	recheck := time.NewTicker(h.server.accessRecheckEvery())
	defer recheck.Stop()
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
		case <-recheck.C:
			g.readersStale.Store(true)
		}
	}
}

// runDue recomputes, or narrows, the view that is due earliest, and is false when none is due. An
// access change announced since the last pass is read first (recheckStaleReaders), so the view is
// recomputed for the readers it left.
func (h *deliveryHub) runDue(g *hubGeneration) bool {
	h.recheckStaleReaders(g)
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

// deliveryViewRead is one recompute's read and compute: seq the change position it read through,
// and shownSeq and shownAt the position it names to its reader (deliveryShownChange); changedAt,
// when seq was recorded, is read for a reader that sees every object alone, which is named seq.
type deliveryViewRead struct {
	seq               int64
	changedAt         *time.Time
	shownSeq          int64
	shownAt           *time.Time
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
			// The change log was reset while it read, so its position names nothing in the new one.
			read.seq, read.shownSeq = 0, 0
		}
		view.settings, view.pop, view.pipe, view.readFrom = read.settings, read.pop, read.pipe, read.from
		view.body, view.seq, view.shownSeq, view.shownAt = &read.body, read.seq, read.shownSeq, read.shownAt
		view.from, view.at, view.expiry = read.from, read.at, read.expiry
		view.issues, view.ancestors, view.sessions = read.issues, read.ancestors, read.sessions
		sent = h.sendLocked(g, view)
		if classes.reads != viewReadsDelivery {
			// A view that reads no delivery row holds no delivery settings, so it judges no
			// freshness: the verdict stays the one the delivery views' own reads set.
			break
		}
		if !g.known {
			// The row's freshness is set silently, from the first settings read.
			g.fresh = deliveryFreshnessHeard{
				lastEventAt: read.settings.LastEventAt, lastReconcileAt: read.settings.LastReconcileAt,
				lastError: read.settings.LastError,
			}
			g.known = true
		}
		g.unfetchable[spec.viewer] = read.unfetchable
		if _, told := g.sent[spec.viewer]; !told {
			// A reader's first verdict is set silently too, from its first read.
			g.sent[spec.viewer] = g.verdictLocked(now, spec.viewer)
		}
		h.judgeLocked(g, now)
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
	ctx, pool := g.readContext(), h.server.deps.Store.Pool
	var read deliveryViewRead
	var err error
	if read.seq, err = delivery.LatestChangeSeq(ctx, pool); err != nil {
		return read, err
	}
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
	if read.seq > 0 && seesEveryObject(spec.reader) {
		if read.changedAt, err = delivery.ChangeAt(ctx, pool, read.seq); err != nil {
			return read, err
		}
	}
	now := h.clock.Now()
	body, err := spec.classes().read(h, ctx, spec, now, &read)
	if err != nil {
		return read, err
	}
	if read.shownSeq, read.shownAt, err = deliveryShownChange(ctx, pool, spec, &read); err != nil {
		return read, err
	}
	read.body, err = encodeDeliveryBody(spec.kind, body)
	return read, err
}

// deliveryShownChange is the change position a view names to its reader, a snapshot's and a
// delta's seq and changed_at: the newest change at or below the position its read took (read.seq)
// of a row that read shows the reader, a pull request or a run of its population or, for a view
// that reads delivery rows, the delivery settings, which every reader of one reads; with when it
// was recorded, and 0 with no time when there is none. So a change of a row the reader is not shown
// moves neither, nor the time the page shows beside its last change, and neither does a change no
// row names, a session group's. A page gives the position back only to ask whether the log still
// keeps it (Subscribe), which one below the newest answers as truly. A reader that sees every
// object (seesEveryObject) is shown every row, and is named read.seq and when it was recorded, as
// every reader was before a project could be private (R9).
func deliveryShownChange(ctx context.Context, q queryer, spec deliveryLiveSpec, read *deliveryViewRead) (int64, *time.Time, error) {
	if seesEveryObject(spec.reader) {
		return read.seq, read.changedAt, nil
	}
	settings := spec.classes().reads == viewReadsDelivery
	prs, runs := read.shownRows()
	if read.seq == 0 || (!settings && len(prs) == 0 && len(runs) == 0) {
		return 0, nil, nil
	}
	var seq int64
	var at time.Time
	err := q.QueryRow(ctx, `
		select seq, at from delivery_changes
		where seq <= $1 and (($2 and kind = 'settings')
			or (kind = 'pull_request' and repo || '#' || ref = any($3::text[]))
			or (kind in ('run', 'run_job') and repo || '#' || ref = any($4::text[])))
		order by seq desc limit 1
	`, read.seq, settings, prs, runs).Scan(&seq, &at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, nil, nil
	case err != nil:
		return 0, nil, err
	}
	return seq, &at, nil
}

// shownRows is every pull request and every run the read shows its reader, each as delivery_changes
// names it, its repository and its number or run id ("owner/name#n"): the population's, a timeline's
// or a measures view's, and a pipeline view's lead times and runs.
func (read *deliveryViewRead) shownRows() (prs, runs []string) {
	prs, runs = []string{}, []string{}
	for _, pop := range []deliveryPopulation{read.pop, read.pipe.leadTimes} {
		for _, pr := range pop.prs {
			prs = append(prs, pr.Repo+"#"+strconv.Itoa(pr.Number))
		}
		for _, pr := range pop.waiting {
			prs = append(prs, pr.Repo+"#"+strconv.Itoa(pr.Number))
		}
		for _, list := range [][]delivery.DeliveryRun{pop.applyRuns, pop.windowRuns} {
			for _, run := range list {
				runs = append(runs, run.Repo+"#"+strconv.FormatInt(run.RunID, 10))
			}
		}
	}
	for _, list := range [][]delivery.DeliveryRun{read.pipe.checks.Runs, read.pipe.deploys.Runs} {
		for _, run := range list {
			runs = append(runs, run.Repo+"#"+strconv.FormatInt(run.RunID, 10))
		}
	}
	return prs, runs
}

// readLegionBoardView is a Legion board's read: the project's own daemon state as the sampler
// recorded it. It reads no delivery settings — its own are the project's Legion setting, and a
// deployment that configured one and not the other still shows the board. A project that does
// not exist, or that the view's reader does not see (access.ProjectClause), is loadProject's
// refusal, as GET /api/v1/projects/{key}/legion/board answers it.
func (h *deliveryHub) readLegionBoardView(ctx context.Context, spec deliveryLiveSpec, now time.Time, read *deliveryViewRead) (any, error) {
	if _, err := h.server.loadProject(ctx, h.server.deps.Store.Pool, spec.reader, spec.project); err != nil {
		return nil, err
	}
	board, err := h.server.readLegionBoard(ctx, spec.reader, spec.project)
	if err != nil {
		return nil, err
	}
	// Every issue the board names, so an event about one marks the view (dirtyIssueLocked); the
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
// An issue of a project the view's reader does not see (projectSeenBy, on the key's project, which
// is the issue's: parseLegionIssueLiveView) is refused as a key Dispatch holds no issue of, before
// anything of it is read, as the board's read refuses a project its reader does not see.
func (h *deliveryHub) readLegionIssueView(ctx context.Context, spec deliveryLiveSpec, now time.Time, read *deliveryViewRead) (any, error) {
	seen, err := projectSeenBy(ctx, h.server.deps.Store.Pool, spec.reader, spec.project)
	if err != nil {
		return nil, err
	}
	if !seen {
		return nil, errorf(http.StatusNotFound, "NOT_FOUND", "not found")
	}
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
	if read.pipe, err = h.server.readPipelinePopulation(ctx, spec.reader, read.settings, read.at); err != nil {
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

// readDeliveryView is a timeline or measures view's read: the delivery settings, the population
// of its window and facets, and the ancestors of every issue that population names.
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
	if read.pop, err = h.server.readDeliveryPopulation(ctx, spec.reader, read.settings, from, to, spec.query, spec.kind == deliveryLiveTimeline); err != nil {
		return nil, err
	}
	read.issues = map[string]struct{}{}
	read.sessions = map[string]string{}
	name := func(issue *string, sessions []string) {
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
		if verdict, told := g.sent[sub.view.spec.viewer]; g.known && told {
			sub.conn.Send(g.freshnessFrameLocked(verdict))
		}
	}
	return sent
}

// answerBodyLocked sends sub the snapshot, when it holds none, or the delta against its
// revisions, when anything differs, each naming the change position its view names to its reader
// (deliveryShownChange).
func (h *deliveryHub) answerBodyLocked(sub *deliverySub) bool {
	view := sub.view
	sub.told = false
	if sub.needsSnapshot {
		sub.conn.Send(view.body.snapshot(sub.id, view.shownSeq, view.shownAt))
		sub.revs, sub.needsSnapshot = view.body.revs, false
		return true
	}
	delta, changed := diffDeliveryBody(*view.body, sub.revs)
	if !changed {
		return false
	}
	delta.Sub, delta.Seq, delta.ChangedAt = sub.id, strconv.FormatInt(view.shownSeq, 10), view.shownAt
	sub.conn.Send(delta)
	sub.revs = view.body.revs
	return true
}
