package api

import (
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// intake moves g's change-log cursor and applies the relay's events, until g ends.
//
// ready closes once the change log's cursor is read. The relay subscription the events arrive on
// waits for a safe horizon (firstRelayCursor), which an event write of Dispatch's own database
// holds back for as long as it stays open, and neither the worker's answers nor the change log
// depend on it, so it is taken on a goroutine of its own (seedEvents) and handed to
// this one (applyFeed). Until it is, the hub acts on no event; the subscription's head, read
// before the open P0 issues it arrives with, is what leaves no gap: every event at or below the
// head was committed and is final before the reads that follow it (those P0 issues, each view's
// recompute) run, and every event above it arrives on the feed. The listener's connection
// (onConnected) catches the change log up after its LISTEN for the same reason, and marks every
// Legion view, which a notification lost before that LISTEN would otherwise leave stale until
// the next one came, with no bound (dirtyLegionViews).
func (h *deliveryHub) intake(g *hubGeneration) {
	var feed relaySubscription
	var held, open bool
	defer func() {
		if held {
			feed.cancel()
		}
	}()
	for attempt := 1; !h.readChangeCursor(g); attempt++ {
		if !waitFor(g.ctx, pglisten.RedialBackoff(attempt)) {
			return
		}
	}
	close(g.ready)
	for {
		var events <-chan relayedEvent
		if open {
			events = feed.events
		}
		select {
		case <-g.ctx.Done():
			return
		case taken := <-g.feeds:
			// The overflowed or ended subscription is cancelled only once the new one stands, so
			// the relay's generation runs on between the two.
			if held {
				feed.cancel()
			}
			feed, held, open = taken.feed, true, true
			h.applyFeed(g, taken)
		case <-g.wakeChanges:
			h.catchUp(g)
		case <-g.connected:
			h.catchUp(g)
			h.dirtyLegionViews(g)
			h.note(deliveryHubNote{what: "connected"})
		case done := <-g.barrier:
			h.catchUp(g)
			close(done)
		case relayed, more := <-events:
			if !more {
				// The feed fell its buffer behind, or the relay's generation ended (a reset of the
				// event log): events may have gone by unapplied. Subscribe again, then read the
				// open P0 issues afresh and recompute every view, which reads what the missed
				// events changed (applyFeed).
				open = false
				h.note(deliveryHubNote{what: "feed closed"})
				h.seedEvents(g)
				continue
			}
			h.receive(g, relayed.Event)
		}
	}
}

// hubEventFeed is a relay subscription seedEvents took for the intake, with the open P0 issues as
// they stood once it had.
type hubEventFeed struct {
	feed relaySubscription
	p0   map[string]struct{}
}

// seedEvents subscribes g to the relay's feed on a goroutine of its own, run on g's context, and
// hands it to the intake on g.feeds; until the intake takes it the hub acts on no event. It is
// the generation's first subscription and the one after a feed closed.
//
// The subscribe waits for the relay's first cursor when it starts a generation, which an open
// event write of this database can hold back, which is why it does not run on the intake's
// goroutine: the hub's ready, its delivery changes and its views' answers do not wait for the
// event horizon. The open P0 issues are read after the subscription stands, so they hold every
// event at or below its head, none of which arrives on the feed.
//
// A subscribe or a read that fails is tried again after the listener's redial backoff. The hand
// over waits for the intake to take the feed (g.feeds has no slot) and gives up when g ends,
// cancelling the feed, so a feed the intake will never take leaves no goroutine behind for running
// to wait on, and no subscription the relay would keep its generation running for. The "feed
// ready" note is made as the hand-over begins.
func (h *deliveryHub) seedEvents(g *hubGeneration) {
	h.running.Add(1)
	go func() {
		defer h.running.Done()
		for attempt := 1; ; attempt++ {
			feed, err := h.server.eventRelay.subscribe(g.readContext(), deliveryHubFeedBuffer)
			var p0 map[string]struct{}
			if err == nil {
				if p0, err = delivery.OpenP0Keys(g.readContext(), h.server.deps.Store.Pool); err != nil {
					feed.cancel()
				}
			}
			if err == nil {
				h.note(deliveryHubNote{what: "feed ready", eventID: feed.head})
				select {
				case g.feeds <- hubEventFeed{feed: feed, p0: p0}:
				case <-g.ctx.Done():
					feed.cancel()
				}
				return
			}
			if g.ctx.Err() != nil {
				return
			}
			slog.Warn("dispatch: delivery live hub could not take the live event stream's feed; retrying", "error", err)
			if !waitFor(g.ctx, pglisten.RedialBackoff(attempt)) {
				return
			}
		}
	}()
}

// applyFeed takes the feed seedEvents stood up, with the open P0 issues read with it, and marks
// every view dirty: a view computed before it read the database at a moment the hub cannot place
// against the feed's head, so it may have missed an event at or below it, which no feed will carry.
func (h *deliveryHub) applyFeed(g *hubGeneration, taken hubEventFeed) {
	now := h.clock.Now()
	h.mu.Lock()
	g.p0 = taken.p0
	h.dirtyAllLocked(g, now)
	h.mu.Unlock()
	h.note(deliveryHubNote{what: "events seeded", eventID: taken.feed.head})
}

// readChangeCursor moves the cursor to the head of the delivery change log, a read of the log to
// its end, whose ReadAt is the hub's clock as it stood before the read.
func (h *deliveryHub) readChangeCursor(g *hubGeneration) bool {
	checked := h.clock.Now()
	head, err := delivery.ChangesHead(g.readContext(), h.server.deps.Store.Pool)
	if err != nil {
		if g.ctx.Err() == nil {
			slog.Warn("dispatch: delivery live hub could not read where the change log stands; retrying", "error", err)
		}
		return false
	}
	head.ReadAt = checked
	h.mu.Lock()
	defer h.mu.Unlock()
	g.change = head
	return true
}

// catchUp catches the change log up; a read that fails is tried again a second later.
func (h *deliveryHub) catchUp(g *hubGeneration) {
	if err := h.catchUpChanges(g); err != nil && g.ctx.Err() == nil {
		slog.Warn("dispatch: delivery live hub could not read the delivery changes; retrying", "error", err)
		time.AfterFunc(time.Second, func() { signal(g.wakeChanges) })
	}
}

// catchUpChanges reads every delivery change after the cursor, marking the views each can touch.
// It first moves the change log's pending rows into the log (delivery.DrainPending): an eval or
// Legion write's change rows wait there until a drain moves them, their notification wakes this
// hub at the write's commit, and the write's own drain may not have run, its process killed
// between the two. Before each read it checks for a lost cursor by the log's one rule
// (delivery.CursorLost), on the hub's own clock; a lost cursor moves to the newest row and marks
// every view dirty.
//
// The clock is read once before each page, for the check and for the cursor's ReadAt: a page
// shorter than its limit and a move to the newest row read the log to its end and take that
// reading, and a full page moves the cursor and leaves its ReadAt where it stood
// (delivery.ChangeCursor and its Through).
func (h *deliveryHub) catchUpChanges(g *hubGeneration) error {
	ctx, pool := g.readContext(), h.server.deps.Store.Pool
	if _, err := delivery.DrainPending(ctx, pool); err != nil {
		return err
	}
	for {
		h.mu.Lock()
		cursor := g.change
		h.mu.Unlock()
		checked := h.clock.Now()
		lost, err := delivery.CursorLost(ctx, pool, cursor, checked)
		if err != nil {
			return err
		}
		if lost {
			head, err := delivery.ChangesHead(ctx, pool)
			if err != nil {
				return err
			}
			head.ReadAt = checked
			now := h.clock.Now()
			h.mu.Lock()
			defer h.mu.Unlock()
			g.change = head
			g.epoch++
			h.dirtyAllLocked(g, now)
			return nil
		}
		changes, err := delivery.ChangesSince(ctx, pool, cursor.Seq, deliveryLivePage)
		if err != nil {
			return err
		}
		now := h.clock.Now()
		h.mu.Lock()
		h.markChangesLocked(g, changes, now)
		g.change = g.change.Through(changes, deliveryLivePage, checked)
		h.mu.Unlock()
		if len(changes) < deliveryLivePage {
			return nil
		}
	}
}

// receive applies one event of the relay's feed. The relay publishes every committed event once,
// in id order, so the hub keeps no cursor of the event log: a gap in what it receives is a feed
// that closed, which intake answers.
func (h *deliveryHub) receive(g *hubGeneration, event model.Event) {
	now := h.clock.Now()
	h.mu.Lock()
	h.applyEventLocked(g, event, now)
	h.mu.Unlock()
	h.note(deliveryHubNote{what: "event", eventID: event.ID})
}

// applyEventLocked marks the views one event can touch: an issue write or claim the views whose
// issues or their ancestors hold the issue, and every timeline and measures view when the issue
// is a P0 before or after it (the unowned P0 count each shows); an architecture import every
// view; and a session's titled write every view that shows the session under another title than
// the one it wrote under, while a write carrying no title takes no name away and marks nothing.
// A project write (project.updated, which a write of its default repository appends) marks no
// view: every view reads the published default repositories, which the refresher's commit moves
// and logs (delivery.EvalDaysWrittenByDefaults, touchedBy).
// Those are every writer of a field the Delivery page shows (Task 0 Step 18: route has one writer,
// PATCH /api/v1/issues/{key}, whose event is issue.updated or issue.closed). The event is the
// relay's, shared with every other subscriber, and is only read.
//
// A mark judges a view by the issues, ancestors and sessions its last answer names (none before
// its first), while a view whose read is under way (reading, set as that read took its change
// position) names its own only once the read lands: an event about an issue or a session only
// that read names marks nothing here. So an issue or session event also records on such a view
// what it touched, for recompute to check against the answer its read gives (recheckHeardLocked).
func (h *deliveryHub) applyEventLocked(g *hubGeneration, event model.Event, now time.Time) {
	var issueKey string
	if event.IssueKey != nil {
		issueKey = *event.IssueKey
	}
	switch event.Type {
	case "issue.created", "issue.updated", "issue.closed":
		var issue struct {
			Key      string `json:"key"`
			Status   string `json:"status"`
			Priority *int   `json:"priority"`
		}
		// An issue event's payload is the JSON the log holds (readEventRows).
		payload, _ := event.Payload.(json.RawMessage)
		key := issueKey
		if err := json.Unmarshal(payload, &issue); err == nil && issue.Key != "" {
			key = issue.Key
		}
		_, before := g.p0[key]
		after := issue.Priority != nil && *issue.Priority == 0 && issue.Status != "done"
		if after {
			g.p0[key] = struct{}{}
		} else {
			delete(g.p0, key)
		}
		h.dirtyIssueLocked(g, key, before || after, now)
	case "issue.claimed", "issue.released":
		// A claim carries no priority (IssueClaimEventPayload), so p0 answers for it.
		_, p0 := g.p0[issueKey]
		h.dirtyIssueLocked(g, issueKey, p0, now)
	case "architecture.synced":
		h.dirtyAllLocked(g, now)
	}
	if event.Actor.Kind == "session" && event.Actor.ID != "" {
		origin := event.Actor.Origin
		if origin == nil || origin.SessionTitle == "" {
			// The name delivery.ResolveSessionTitles reads is the newest title the session
			// stamped, which a write carrying none does not take away. So such a write is no
			// retitle: it marks no view, and nothing is recorded for the session, which
			// recheckHeardLocked would replay as one.
			return
		}
		title := origin.SessionTitle
		for _, view := range g.views {
			if view.showsSessionRetitled(event.Actor.ID, title) {
				h.markDirtyLocked(g, view, now, deliveryCauseChange)
			}
			if view.reading {
				if view.heardSessions == nil {
					view.heardSessions = map[string]string{}
				}
				view.heardSessions[event.Actor.ID] = title
			}
		}
	}
}

func (h *deliveryHub) dirtyIssueLocked(g *hubGeneration, key string, p0 bool, now time.Time) {
	for _, view := range g.views {
		if view.showsIssue(key, p0) {
			h.markDirtyLocked(g, view, now, deliveryCauseChange)
		}
		if view.reading {
			if view.heardIssues == nil {
				view.heardIssues = map[string]bool{}
			}
			view.heardIssues[key] = view.heardIssues[key] || p0
		}
	}
}

// recheckHeardLocked marks view dirty when an event heard since its read took its change position
// touches the answer recompute has just installed, by the rules applyEventLocked marks with, and
// clears the record.
func (h *deliveryHub) recheckHeardLocked(g *hubGeneration, view *deliveryView, now time.Time) {
	for key, p0 := range view.heardIssues {
		if view.showsIssue(key, p0) {
			h.markDirtyLocked(g, view, now, deliveryCauseChange)
			break
		}
	}
	for session, title := range view.heardSessions {
		if view.showsSessionRetitled(session, title) {
			h.markDirtyLocked(g, view, now, deliveryCauseChange)
			break
		}
	}
	view.reading, view.heardIssues, view.heardSessions = false, nil, nil
}

// showsIssue is whether a write to the issue key, a P0 before or after it when p0, can move the
// view's answer: the view's population, or its ancestors, names the issue, or the view shows the
// unowned P0 list and the issue is a P0. A Legion board names its own issues and shows no P0
// list, so only the first rule reaches it; a legion_issue view names no issue, and an evals view
// neither names one nor shows the list, so no issue write reaches either (an evals view's
// unowned-p0 count is a stored row, whose rewrite the change log carries).
func (view *deliveryView) showsIssue(key string, p0 bool) bool {
	_, named := view.issues[key]
	_, ancestor := view.ancestors[key]
	return named || ancestor || (p0 && view.spec.classes().unownedP0)
}

// showsSessionRetitled is whether the view shows session under another title than title.
func (view *deliveryView) showsSessionRetitled(session, title string) bool {
	shown, ok := view.sessions[session]
	return ok && shown != title
}

func (h *deliveryHub) dirtyAllLocked(g *hubGeneration, now time.Time) {
	for _, view := range g.views {
		h.markDirtyLocked(g, view, now, deliveryCauseChange)
	}
}

// markChangesLocked marks, for each change, the views it can touch (touchedBy) whose answer does
// not already hold it: a change at or before a view's last answer, or the position of the read
// under way, marks nothing.
//
// An eval_days change that the refresher's rewrite of a delivery settings change wrote carries the
// rewrite's mark (delivery.EvalDaysWrittenBySettingsRewrite in its RunKind, which the rewrite's
// transaction sets: evals.MarkSettingsRewrite), and marks with cause settings-rewrite; one its
// rebuild of a span or a rebuild's flip wrote carries the rebuild's
// (delivery.EvalDaysWrittenByRebuild, evals.MarkRebuild), and marks with cause rebuild; and one its
// page logs in the commit that publishes the projects' default repositories carries the
// publishing's (delivery.EvalDaysWrittenByDefaults), and marks with cause defaults. None of them
// waits for a quiet (delivery.EvalDaysImmediate, markDirtyLocked). Every other change waits the
// quiet. The mark is the change's own. The rewrite, the rebuild and the publishing each write what
// they move in one transaction, so their changes reach the log together, and the one recompute
// they make due answers them all.
func (h *deliveryHub) markChangesLocked(g *hubGeneration, changes []delivery.Change, now time.Time) {
	for _, change := range changes {
		cause := deliveryCauseChange
		if delivery.EvalDaysImmediate(change) {
			switch change.RunKind {
			case delivery.EvalDaysWrittenByRebuild:
				cause = deliveryCauseRebuild
			case delivery.EvalDaysWrittenByDefaults:
				cause = deliveryCauseDefaults
			default:
				cause = deliveryCauseSettingsRewrite
			}
		}
		for _, view := range g.views {
			if change.Seq > max(view.seq, view.readingSeq) && view.touchedBy(change, now) {
				h.markDirtyLocked(g, view, now, cause)
			}
		}
	}
}

// markDirtyLocked marks view for a recompute and wakes g's worker. Marks coalesce on the view
// whatever their cause, so one recompute answers every mark made before its read takes them over
// (deliveryHub.read). Four marks wait for no quiet (immediate): a subscribe's, whose sub waits
// for its first answer, a settings rewrite's, a rebuild's and a publishing of the defaults'
// (markChangesLocked). The quiet gathers a burst of changes into one recompute, and a rewrite, a
// rebuild or a publishing has no burst left to gather, since the refresher writes what it moves in
// one transaction, whose changes arrive in one drain. Its page has meanwhile waited for the
// refresher's pass, which shares with the recompute the 2 s R14 gives the change.
func (h *deliveryHub) markDirtyLocked(g *hubGeneration, view *deliveryView, now time.Time, cause string) {
	if !view.dirty {
		view.dirty, view.firstDirty, view.cause = true, now, cause
	}
	view.lastDirty = now
	if cause == deliveryCauseSubscribe || cause == deliveryCauseSettingsRewrite || cause == deliveryCauseRebuild || cause == deliveryCauseDefaults {
		view.immediate, view.cause = true, cause
	}
	signal(g.wakeWork)
}

// touchedBy is whether a delivery change can move the view's answer (plan v4 Task 7 Step 7's
// table), by the change's kind: a write of the stored facts, of an eval's rows or of a filing is
// answered by its own arm, and every other kind is a delivery row, which moves only a view that
// reads the delivery rows (touchedByDeliveryRow, behind that test) and pr-ci's items views
// (touchedByItemsRun).
//
// The rows the named kinds write are rows no delivery view reads, but for three: the timeline, the
// measures and the Pipeline read the current versions of the evals that judge or rank their
// figures (deliveryLiveClasses.evalVersions), and the timeline and the measures fold the failure
// measures' day rows (deliveryLiveClasses.evalDays), so a version of one of those evals, or a
// write of those rows, marks them here; and they answer under the rules recorded with the day
// rows, which the refresher's commit that brings the rows to the settings moves.
//
// A change of the delivery settings marks no evals or items view. Such a view reads the settings
// and the day rows together: the source repositories recorded with the rows choose the rows of the
// evals that read them (evals.DeployReposIn, evals.DeployRepoOf), the one setting the list read
// takes, and the judged names are what the refresher built each repository's rows under. A
// recompute at the settings change would answer the new settings over rows the refresher has not
// rewritten yet, a state the inputs never had where one save moves a source's repository and a
// judged name together. The refresher's commit that brings the rows to the settings marks what it
// moved: its rewrite's eval_days changes, one of each eval whose rows moved under that eval's
// project, mark the evals views of the projects whose rows moved and the items views of those
// evals, and, where the recorded source repositories moved (one gained or lost), the commit that
// records them logs one eval_days change of no eval (delivery.SettingsApplied), which marks every
// evals and items view. Each marks at once (markChangesLocked). A change that moves no row and no
// source repository moves nothing an evals or items view reads, and marks none.
//
// A delivery view answers under the same recorded rules (readDeliveryRules, evals.BuiltSettingsIn),
// so a page shows the state the Evals tab shows: the change of a project's settings marks every
// one (touchedByDeliveryRow), since the settings hold the freshness it states and every setting the
// record does not keep, and its recompute answers the old rule still; the commit that records the
// new rules marks every one again, at once, through each change it logs, all of which carry the
// rewrite's mark (delivery.EvalDaysWrittenBySettingsRewrite): the rules recorded
// (delivery.SettingsRecorded), the settings applied, and each eval_days change of its rewrite.
func (view *deliveryView) touchedBy(change delivery.Change, now time.Time) bool {
	switch change.Kind {
	case delivery.ChangeKindIssueFacts:
		// One issue's stored Legion facts, which the Legion board is the one reader of
		// (legionfacts.Read). The board's read takes the rows of every open issue and of the
		// issues it shows, every issue of each tree a card stands in among them, so a row of an
		// issue the board names nowhere yet - a new member of a shown tree, the first row of an
		// issue, a row a build's whole read writes - moves its answer as a row of an issue it does
		// name does: the change marks the board of its project, which the trigger carries in the
		// change's repo (migration 20261117100002), and a closed row of a tree the board does not
		// show is recomputed into the same body and sends nothing. No other kind of view reads a
		// facts row, so every delivery view is left: a build's whole read rewrites every row of a
		// project whose values or writer moved, and a timeline, measures or pipeline answer cannot
		// move when one does.
		return view.spec.kind == legionBoardView && view.spec.project == change.Repo
	case delivery.ChangeKindEvalReadings, delivery.ChangeKindEvalVersions, delivery.ChangeKindEvalDays:
		// One eval's readings, a new version of its definition, or its day rows: what the evals
		// view of the eval's project reads (readEvalList), and the items view of that eval
		// (readEvalItems), whose rows its readings and day rows are and whose definition names its
		// series and its min_n. The change's repo holds that project and its ref the eval
		// (migrations 20261117100012 to 20261117100014).
		//
		// No other kind marks an evals view. A computed eval's value moves when the refresher
		// rewrites its day rows, after its input's own change (a pull request, a run, a stored
		// fact, a filing, the settings row) was logged: a view woken by the input would read the
		// rows before the rewrite, and the eval_days change of the rewrite is what wakes it. The
		// eval_days change of no eval is the refresher's word that the rows stand at the delivery
		// settings under source repositories that moved (delivery.SettingsApplied), which every
		// evals and items view reads, whatever its project.
		//
		// Every eval_days change that carries the settings rewrite's mark, that word, the word that
		// the rules are recorded (delivery.SettingsRecorded) and each change of the rewrite's rows,
		// is logged by a commit that records the rules the rows stand built under, which every
		// delivery view answers under: it marks every view that reads the delivery rows.
		//
		// A new version of an eval a delivery view reads (its definition or one of its series)
		// marks every view of that kind too, and readings mark none: the timeline's and the
		// measures' KPIs and the Pipeline's lead-time cards are judged by the targets of the
		// delivery evals, and the Pipeline ranks its slowest jobs by the min_n of pr-ci's job
		// series, each of the project whose deploy source reads the deploy repository, a source
		// that names no repository reading its project's published default
		// (evals.Store.DeployRepoEvals), which a register write sets and a project's seed first
		// writes. Which project that is, no view holds, so the version in any project marks it,
		// and a recompute that reads the same evals sends nothing. The refresher's commit that
		// moves that project (delivery.JudgingMoved) marks every such view too; a person's write
		// of a default repository marks none until it does. Other day rows mark a delivery view
		// only where its answer folds them: the timeline's and the measures' failure measures are
		// the three failure evals' day rows of the delivery-on project
		// (evals.Store.DeploymentFailures), which no view names either, so a write of them in any
		// project marks those views.
		//
		// A project timeline counts a row no issue attributes for the project whose published
		// default repository holds it, and a deploy that ships nothing for the project whose deploy
		// source reads its repository under the published defaults (evals.Attribution). The
		// refresher's commit that publishes the defaults logs each project whose default it moved,
		// which marks that project's timeline views, and each deploy repository whose judging
		// project (the one whose source reads it) it moved, which marks every timeline view, since
		// the change names the repository and not the projects it moved between
		// (delivery.EvalDaysWrittenByDefaults). It answers under the recorded rules as every
		// delivery view does, so the settings rewrite's changes mark it too.
		switch view.spec.classes().reads {
		case viewReadsEvals:
			return view.spec.project == change.Repo || delivery.SettingsApplied(change)
		case viewReadsEvalItems:
			return change.Ref == evals.EvalID(view.spec.project, view.spec.slug) || delivery.SettingsApplied(change)
		}
		if change.Kind == delivery.ChangeKindEvalDays && change.RunKind == delivery.EvalDaysWrittenBySettingsRewrite {
			return view.spec.classes().reads == viewReadsDelivery
		}
		if view.spec.kind == deliveryLiveProjectTimeline {
			return change.RunKind == delivery.EvalDaysWrittenByDefaults && (change.Repo == view.spec.project || delivery.JudgingMoved(change))
		}
		if delivery.JudgingMoved(change) {
			return len(view.spec.classes().evalVersions) > 0
		}
		named := func(slug string) bool { return change.Ref == evals.EvalID(change.Repo, slug) }
		switch change.Kind {
		case delivery.ChangeKindEvalVersions:
			return slices.ContainsFunc(view.spec.classes().evalVersions, named)
		case delivery.ChangeKindEvalDays:
			return slices.ContainsFunc(view.spec.classes().evalDays, named)
		}
		return false
	case delivery.ChangeKindIssueFilings:
		// One filing of a Legion session. No view reads a filing: the refresher does, and
		// rewrites the day's Legion rows, whose eval_days change marks the evals view; the
		// Legion board reads the stored facts (legionfacts.Read) and the daemon's state; and
		// the delivery views read the delivery rows alone.
		return false
	case delivery.ChangeKindProductionEvents:
		// One write of the record of deploys and failed changes (dispatch://INFRA-978). No view
		// reads the record: the refresher does, and rewrites the three failure evals' day rows of
		// each project the row counts for, whose eval_days change marks that project's evals view
		// and every view that folds those rows: the timeline and the measures (above).
		return false
	default:
		// A delivery row. A view that reads none moves by other things: a Legion board by a read
		// of its daemon (legion.StateChannel) or a write of one of its issues (dirtyIssueLocked),
		// a legion_issue view by a read of its project's daemon or a write of one of its runs
		// (workflowstore.RunsChannel), and an evals view by the eval kinds above; an items view
		// reads pr-ci's runs and their jobs themselves besides (touchedByItemsRun), and a narrowed
		// evals view the pull requests and deploy runs its evals that count pull requests are built
		// from when it is read (deliveryLiveSpec.narrowed), which a delivery row's change marks as
		// it marks a measures view (touchedByDeliveryRow), so it is answered within R14's 2 s.
		switch view.spec.classes().reads {
		case viewReadsDelivery:
			return view.touchedByDeliveryRow(change, now)
		case viewReadsEvalItems:
			return view.touchedByItemsRun(change, now)
		case viewReadsEvals:
			return view.spec.narrowed && view.touchedByDeliveryRow(change, now)
		}
		return false
	}
}

// touchedByItemsRun is whether a change of a delivery row can move the answer of an items view:
// pr-ci's run and job rows are read from the pull-request checks runs and their jobs themselves
// (runItemRows, jobItemRows, failingJobRows), not only from the day rows the refresher rewrites
// after them, so a change of such a run or of one of its jobs marks a view of pr-ci that keeps
// either kind of row where the change's time, the run's start or its head commit's time where that
// is later (migration 0100), falls on a day of one of the view's ranges as it reads them at now,
// and where the change carries no time. The other evals' delivery rows (a delivery eval's pull
// requests and deploys, a deploy card's runs) mark nothing here: what a change of one moves in
// them moves the day rows the refresher rewrites after it, whose eval_days change marks the view.
func (view *deliveryView) touchedByItemsRun(change delivery.Change, now time.Time) bool {
	if change.Kind != delivery.ChangeKindRun && change.Kind != delivery.ChangeKindRunJob ||
		change.RunKind != string(delivery.DeliveryRunKindPRChecks) || view.spec.slug != evals.SlugPRCI {
		return false
	}
	request, err := parseEvalItemsQuery(view.spec.evalsQuery(now))
	if err != nil {
		// The view's query was read as the route reads it when it was subscribed
		// (parseEvalItemsLiveView), and only a preset's window moves with now, so this is no
		// view the hub holds; its read would refuse it all the same.
		return true
	}
	if !request.keeps(evalRowRun) && !request.keeps(evalRowJob) {
		return false
	}
	ranges := []evals.Range{request.ranges.window}
	if request.ranges.compare != nil {
		ranges = append(ranges, *request.ranges.compare)
	}
	var times []time.Time
	for _, t := range []*time.Time{change.TOld, change.TNew} {
		if t != nil {
			times = append(times, *t)
		}
	}
	return len(times) == 0 || slices.ContainsFunc(times, func(t time.Time) bool {
		return slices.ContainsFunc(ranges, func(r evals.Range) bool { return !t.Before(r.From) && t.Before(r.To) })
	})
}

// touchedByDeliveryRow is whether a change of a delivery row can move the answer of a view that
// reads the delivery rows. The view's window is the one its last answer resolved, a preset's end
// the present; a change whose rows carry no time at all (a pull request never merged, a job of a
// run not stored) is taken as touching timeline and measures views. Pipeline lead times read only
// merged pull requests (ListPullRequestsInWindow), so an unmerged pull request cannot move a
// Pipeline view. A narrowed evals view (deliveryLiveSpec.narrowed) builds its evals that count
// pull requests from the pull requests merged on the days of its window and compare range and
// the deploy runs that ship them, and is marked as a measures view is, up to the end of the later
// of its two ranges' days (evalsEnd). A kind no arm names is taken as touching every such view.
func (view *deliveryView) touchedByDeliveryRow(change delivery.Change, now time.Time) bool {
	var times []time.Time
	for _, t := range []*time.Time{change.TOld, change.TNew} {
		if t != nil {
			times = append(times, *t)
		}
	}
	from, to := view.window(now)
	if view.spec.kind == evalsView {
		to = view.spec.evalsEnd(now, to)
	}
	some := func(in func(time.Time) bool) bool { return slices.ContainsFunc(times, in) }
	atOrAfterFrom := func(t time.Time) bool { return !t.Before(from) }
	switch change.Kind {
	case delivery.ChangeKindSessionGroup:
		return true
	case delivery.ChangeKindSettings:
		// A narrowed evals view builds its rows under the settings the stored rows stand built
		// under, as its stored evals read them (evals.BuiltSettingsIn), so the refresher's word
		// that the rows stand at the settings marks it (touchedBy), and the save does not.
		return view.spec.kind != evalsView
	case delivery.ChangeKindPullRequest:
		switch view.spec.kind {
		case deliveryLiveTimeline, deliveryLiveMeasures, deliveryLiveProjectTimeline, evalsView:
			// The timeline's waiting line reads merges before the window, and the measures'
			// deploys_with_prs counts a deploy of the window that ships a pull request merged before
			// it (delivery.Deploys.ShippedBy), which also decides the projects a project timeline's
			// deploy counts for, so every merge before the window's end can move any of them.
			return len(times) == 0 || some(func(t time.Time) bool { return t.Before(to) })
		default:
			return some(atOrAfterFrom)
		}
	case delivery.ChangeKindRun, delivery.ChangeKindRunJob:
		switch change.RunKind {
		case string(delivery.DeliveryRunKindPRChecks):
			return view.spec.kind == deliveryLivePipeline && (len(times) == 0 || some(atOrAfterFrom))
		case string(delivery.DeliveryRunKindDeploy):
			switch view.spec.kind {
			case deliveryLiveTimeline, deliveryLiveMeasures, deliveryLiveProjectTimeline, evalsView:
				// The timeline's waiting line at the window's start reads deploy runs of any age,
				// and the measures' deploys_with_prs reads the runs created before the window too:
				// a run of any age whose production job lands late can ship a pull request a deploy
				// of the window shipped first until then (pop.windowShipped), and so move the
				// project a project timeline's deploy counts for, or the deploy a narrowed evals
				// view's lead times to production time a pull request to.
				return true
			default:
				return len(times) == 0 || some(atOrAfterFrom)
			}
		}
	}
	return true
}

// evalsEnd is the end of the days an evals view reads at now, the later of its window's and its
// compare range's (parseEvalRanges, the midnight after each range's last day), or to where its
// query names no compare range ending later; its query was read as the route reads it when it was
// subscribed (parseEvalsLiveView), so it parses.
func (spec deliveryLiveSpec) evalsEnd(now, to time.Time) time.Time {
	ranges, err := parseEvalRanges(spec.evalsQuery(now))
	if err != nil {
		return to
	}
	for _, days := range []*evals.Range{&ranges.window, ranges.compare} {
		if days != nil && days.To.After(to) {
			to = days.To
		}
	}
	return to
}

// window is the view's window as its last answer resolved it: a custom range as named, a preset
// from its last start (or its length before now, before its first answer) to the present, and a
// pipeline view from the first shown day of its pinned at or of its last answer's.
func (view *deliveryView) window(now time.Time) (from, to time.Time) {
	switch {
	case view.spec.kind == deliveryLivePipeline:
		at := now
		if view.spec.at != nil {
			at = *view.spec.at
		} else if !view.at.IsZero() {
			at = view.at
		}
		return measures.WindowStart(at), time.Time{}
	case view.spec.preset > 0:
		if view.from.IsZero() {
			return now.Add(-view.spec.days()), now
		}
		return view.from, now
	default:
		return view.spec.from, view.spec.to
	}
}

// days is a preset's length.
func (spec deliveryLiveSpec) days() time.Duration {
	return time.Duration(spec.preset) * 24 * time.Hour
}
