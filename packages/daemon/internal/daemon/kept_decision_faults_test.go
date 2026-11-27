package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The three fault states that keptLongRegressionSeeds keeps seeds 41, 427 and 701 for, and that
// TestEachKeptFaultSeedReachesTheStateOfItsFault checks each seed reaches.
const (
	keptFaultToldAgainAfterAFailedWrite   = "a decision told again after a catch-up whose telling write failed"
	keptFaultDropBesideADeliveringTelling = "a drop of the root's catch-ups while a row of a kept telling is inside a publish that delivers and an unclaimed, untold copy of that telling stands"
	keptFaultSpentRehold                  = "a catch-up's re-holds spent while its architect holds its launch"
)

// keptFaultMarkers counts each time a run of the kept-decision model reaches one of the three
// fault states. Each counter is incremented at the moment the state occurs and logged once on the
// first firing, naming the seed and the step, so a control run with that fault's fix undone prints
// its marker line before the fault fails the test.
type keptFaultMarkers struct {
	toldAgainAfterAFailedWrite   atomic.Int64
	dropBesideADeliveringTelling atomic.Int64
	spentRehold                  atomic.Int64
}

// markToldAgainAfterAFailedWrite marks seed 41's state in keptRun.tell after earlier and excused
// are counted and before the switch decides, so it fires whether the oracle then accepts or fails.
func (r *keptRun) markToldAgainAfterAFailedWrite(ref, tree string) {
	if r.faults.toldAgainAfterAFailedWrite.Add(1) == 1 {
		r.t.Logf("seed %d: fault marker %q reached at step %d (ref=%s tree=%s)",
			r.seed, keptFaultToldAgainAfterAFailedWrite, r.step, ref, tree)
	}
}

// markDelivering wraps a held-open catch-up race whose answer is keptDelivers so r.delivering holds
// row for the time the race op runs. A receipt timeout tells nothing (keptPublisher.publish records
// no delivery for it, and workflow/parked_decision.go says the same of the daemon), so a publish
// answered with a receipt timeout leaves r.delivering at 0.
func (r *keptRun) markDelivering(race *keptRace, row int64) {
	if race.mode != keptDelivers {
		return
	}
	run := race.run
	race.run = func() error {
		r.delivering.Store(row)
		defer r.delivering.Store(0)
		return run()
	}
}

// markDropBesideADeliveringTelling marks seed 427's state when DropCatchUps runs while r.delivering
// is inside a delivering publish and an unclaimed, untold copy of that telling stands in the outbox.
func (r *keptRun) markDropBesideADeliveringTelling(issue string, delivering int64) {
	if r.faults.dropBesideADeliveringTelling.Add(1) == 1 {
		r.t.Logf("seed %d: fault marker %q reached at step %d (issue=%s delivering=%d)",
			r.seed, keptFaultDropBesideADeliveringTelling, r.step, issue, delivering)
	}
}

// markSpentRehold marks seed 701's state when a held-open catch-up publish in raceRow drew a
// receipt timeout on a copy whose re-holds are spent (Resends >= len(noticeReholdDelays)) and whose
// catch-up carries kept decisions, and reholdNotice took its spent branch rather than stopping with
// its finished tree or taking its superseded branch. In reholdNotice (notice_exceptions.go),
// stoppedWithTree runs before the Resends check, and the Resends check runs before
// catchUpSuperseded, so a non-stopped row at Resends >= len(noticeReholdDelays) takes the spent
// branch alone. In a run under the seek of that state (seekSpentRehold), the first firing names the
// tree whose architect the seek then holds at its launch (spentHold, holdTheSpentState).
func (r *keptRun) markSpentRehold(row record.OutboxRow, payload record.OutboxPayload) {
	notice, ok := payload.(record.Notice)
	if !ok || notice.CatchUp == nil || notice.CatchUp.Kept == nil || notice.Resends < len(noticeReholdDelays) {
		return
	}
	tree := r.treeRow(row.Issue)
	if tree == nil {
		return
	}
	held := r.runner.supervisedClaim(tree.token)
	if !holdsRole(held) {
		_, lingers, err := r.runner.issueTree(r.ctx, row.Issue)
		if err != nil {
			r.fatalf("read %s's tree for the spent-rehold marker: %v", row.Issue, err)
		}
		if stoppedWithTree(lingers, held) {
			return
		}
	}
	if r.faults.spentRehold.Add(1) == 1 {
		r.t.Logf("seed %d: fault marker %q reached at step %d (row=%d issue=%s resends=%d)",
			r.seed, keptFaultSpentRehold, r.step, row.ID, row.Issue, notice.Resends)
		if r.seekSpentRehold {
			r.spentHold = tree
		}
	}
}

// DropCatchUps checks, in the drop's transaction under record.LockCatchUps(issue), whether an
// unclaimed, untold copy of the telling of r.delivering stands before calling the embedded drop.
// An advisory transaction lock is reentrant in one session, so the embedded drop's own LockCatchUps
// call succeeds without waiting. Every row of one telling shares the row it copies, or its own id
// where it copies none, and the telling's told mark is keyed by that row (record.KeptToldSource
// under record.OutboxKey).
func (s *keptForgetStore) DropCatchUps(ctx context.Context, tx pgx.Tx, issue string) ([]int64, error) {
	if s.run != nil {
		if delivering := s.run.delivering.Load(); delivering != 0 {
			if err := record.LockCatchUps(ctx, tx, issue); err != nil {
				return nil, err
			}
			var telling int64
			err := tx.QueryRow(ctx, `select coalesce((payload->>'resend_of')::bigint, id) from outbox
				where id = $1 and lease_token <> ''`, delivering).Scan(&telling)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			var stood bool
			if err == nil {
				if err := tx.QueryRow(ctx, `select exists (select 1 from outbox copy
					where copy.kind = $1 and copy.issue = $2 and copy.payload->>'kind' = 'catch-up'
					and copy.payload->'catch_up'->'kept' is not null and copy.lease_token = '' and copy.id <> $3
					and coalesce((copy.payload->>'resend_of')::bigint, copy.id) = $4)
					and not exists (select 1 from processed_events where source = $5 and event_id = $6)`,
					string(record.OutboxKindNotice), issue, delivering, telling, record.KeptToldSource,
					record.OutboxKey(telling)).Scan(&stood); err != nil {
					return nil, err
				}
			}
			if stood {
				s.run.markDropBesideADeliveringTelling(issue, delivering)
			}
		}
	}
	return s.Store.DropCatchUps(ctx, tx, issue)
}

// MarkProcessed is the record store's own, except that the told mark of one catch-up telling
// fails while the seek of seed 41's state arms it (failsToldMark): the publish reached the
// architect and the mark that records the telling did not commit (toldKept), so the notice
// executor returns that error, the lane retries the row and its retry clears the row's claim. The
// row then stands untold and unclaimed, so a drop of its tree's catch-ups deletes it and the
// foreign key releases what it claimed (record.Store.DropCatchUps). The telling is recorded as a
// failed forget records it, by the row its publish went out under, which is the row the mark's key
// names (a re-held copy's being the row it copies), so tellingFailed reads it. One mark fails per
// arming, and with the seek off nothing is armed and every mark is the store's.
func (s *keptForgetStore) MarkProcessed(ctx context.Context, tx pgx.Tx, source, eventID string) (bool, error) {
	if source == record.KeptToldSource && s.failsToldMark.CompareAndSwap(true, false) {
		if telling, ok := record.ParseOutboxKey(eventID); ok {
			s.failedTellings.Store(telling, true)
		}
		return false, errors.New("tester: the told mark's transaction did not commit")
	}
	return s.Store.MarkProcessed(ctx, tx, source, eventID)
}

// keptSeekPlan is what the seek of a fault state takes now: the steps the drive may choose among
// (nil leaves the step to the generator's pick among every enabled step), and the row runRow or
// raceRow then takes.
type keptSeekPlan struct {
	steps []func()
	run   int64
	race  int64
}

// faultSeekPlan is the plan of the seek this run's seed is set to (keptFaultSeeds), while the
// marker of its state has not fired; once it has, the seek takes nothing and the run goes on as
// the generator picks, save that the seek of seed 701's state then holds that state open
// (holdTheSpentState).
func (r *keptRun) faultSeekPlan() keptSeekPlan {
	switch {
	case r.seekingToldAgain():
		return r.seekToldAgain()
	case r.seekingTheDrop():
		return r.seekTheDrop()
	case r.seekingSpentRehold():
		return r.seekTheSpentRehold()
	case r.spentHolding():
		return r.holdTheSpentState()
	}
	return keptSeekPlan{}
}

func (r *keptRun) seekingToldAgain() bool {
	return r.seekToldAgainAfterAFailedWrite && r.faults.toldAgainAfterAFailedWrite.Load() == 0
}

func (r *keptRun) seekingTheDrop() bool {
	return r.seekDropBesideADeliveringTelling && r.faults.dropBesideADeliveringTelling.Load() == 0
}

func (r *keptRun) seekingSpentRehold() bool {
	return r.seekSpentRehold && r.faults.spentRehold.Load() == 0
}

// spentHolding is whether the seek of seed 701's state holds that state open (spentHold), which
// it does only in a run whose seek is seekSpentRehold, once that state's marker has fired.
func (r *keptRun) spentHolding() bool {
	return r.spentHold != nil
}

// spentHoldOn is whether the seek of seed 701's state holds tree's architect at its launch, so the
// drive leaves out tree's steps that would end, relaunch or move that launch: the death of the
// process in a task's turn (deathInTheTasksTurn), which relaunches the architect; for the root,
// its park (park), whose re-admission starts it anew; and the adoption of the queued root into
// tree (adoptQueued), whose catch-up names everything tree keeps and nothing claims.
func (r *keptRun) spentHoldOn(tree *keptTree) bool {
	return tree != nil && r.spentHold == tree
}

// holdTheSpentState is what the seek of seed 701's state takes while it holds that state open:
// the listener switched to delivering where it stands at refusing the delivery or denying the
// forward, so no catch-up of the held tree is finished undelivered by a refusal (noteRefusal),
// which quiesce would answer with that architect's relaunch (relaunchAfterRefusal). The drive also
// leaves out the listener's switch (togglePublisher) for as long as the hold lasts.
func (r *keptRun) holdTheSpentState() keptSeekPlan {
	if mode := r.publisher.standingMode(); mode == keptRefuses || mode == keptDenied {
		return keptSeekPlan{steps: []func(){r.setModeForASeek(keptDelivers)}}
	}
	return keptSeekPlan{}
}

// faultSeekSteps is the steps the drive chooses among while a seek has one to take (enabled).
func (r *keptRun) faultSeekSteps() []func() {
	return r.faultSeekPlan().steps
}

// soughtRow is the index in due of the row a seek has runRow take, and -1 for none.
func (r *keptRun) soughtRow(due []record.OutboxRow) int {
	return keptRowIndex(due, r.faultSeekPlan().run)
}

// soughtRace is the index in catchUps of the catch-up a seek has raceRow race, and -1 for none.
func (r *keptRun) soughtRace(catchUps []record.OutboxRow) int {
	return keptRowIndex(catchUps, r.faultSeekPlan().race)
}

func keptRowIndex(rows []record.OutboxRow, id int64) int {
	if id == 0 {
		return -1
	}
	return slices.IndexFunc(rows, func(row record.OutboxRow) bool { return row.ID == id })
}

// soughtRaceOp is the index in ops of the operation raceRow runs inside the publish of row while
// a seek races it, and -1 to leave the pick to the generator: for seed 427's state, an operation
// that drops the catch-ups of the row's tree (dropOpRank); for seed 701's, one that supersedes
// nothing the re-hold reads, one of another tree before one of the row's own (spentReholdOpRank).
// Ops are ranked, and the first of the best rank is taken, so one seed always takes the same one.
func (r *keptRun) soughtRaceOp(row record.OutboxRow, ops []keptRaceOp) int {
	var rank func(keptRaceOp) int
	switch {
	case r.seekingTheDrop():
		armed, ok := r.armedDrop()
		if !ok || armed.row.ID != row.ID {
			return -1
		}
		rank = func(op keptRaceOp) int { return r.dropOpRank(armed.tree, op) }
	case r.seekingSpentRehold():
		raced, ok := r.soughtCatchUp(row)
		if !ok {
			return -1
		}
		rank = func(op keptRaceOp) int { return r.spentReholdOpRank(raced.tree, op) }
	default:
		return -1
	}
	best, pick := -1, -1
	for i, op := range ops {
		if k := rank(op); k >= 0 && (best < 0 || k < best) {
			best, pick = k, i
		}
	}
	return pick
}

// soughtRaceMode is the answer a seek gives the publish of row it races, and whether it gives one:
// for seed 427's state a delivery, so the row is inside a publish that delivers while the race
// drops its tree's catch-ups; for seed 701's a receipt timeout, so the re-hold queues the next
// copy until the last one's re-holds are spent.
func (r *keptRun) soughtRaceMode(row record.OutboxRow) (keptPublishMode, bool) {
	switch {
	case r.seekingTheDrop() && r.isArmedDrop(row):
		return keptDelivers, true
	case r.seekingSpentRehold() && r.isSoughtCatchUp(row):
		return keptReceiptTimeout, true
	}
	return keptDelivers, false
}

// crashesForTheDrop is whether runRow loses the finish of row to a crash because the seek of seed
// 427's state needs it: row is a catch-up of a driven tree naming kept decisions whose run drew a
// receipt timeout, and the re-hold queued its copy and moved row's claims onto it, which stands
// unclaimed and untold. The crash leaves row its claim's token, which keeps that copy through a
// drop of its tree's catch-ups (record.Store.DropCatchUps) until row runs again.
func (r *keptRun) crashesForTheDrop(row record.OutboxRow) bool {
	if !r.seekingTheDrop() {
		return false
	}
	catchUp, ok := r.soughtCatchUp(row)
	return ok && catchUp.notice.Resends == 0 && r.untoldCopy(row.ID)
}

// keptSoughtCatchUp is one catch-up of a driven tree that names kept decisions, read with its
// lease and its due moment, as a seek weighs it, and the tree whose architect it tells.
type keptSoughtCatchUp struct {
	row    record.OutboxRow
	notice record.Notice
	tree   *keptTree
}

// leased is whether a live lease holds the row, so no lane claims it now.
func (c keptSoughtCatchUp) leased(now time.Time) bool {
	return c.row.LeaseUntil != nil && c.row.LeaseUntil.After(now)
}

// telling is the id the telling c publishes is known by: the row it copies
// (record.Notice.ResendOf), or its own for a row that copies none.
func (c keptSoughtCatchUp) telling() int64 {
	return cmp.Or(c.notice.ResendOf, c.row.ID)
}

// soughtCatchUps is every catch-up of a driven tree that names kept decisions, in id order: the
// root's, and those of every tree a member left the root's for, whose architects are told what
// is kept for them as the root's is.
func (r *keptRun) soughtCatchUps() []keptSoughtCatchUp {
	keys := r.rootKeys()
	rows, err := r.pool.Query(r.ctx, `select id, issue, payload, lease_token, lease_until, next_at from outbox
		where issue = any($1) and kind = $2 and payload->>'kind' = 'catch-up' order by id`, keys, string(record.OutboxKindNotice))
	if err != nil {
		r.fatalf("read the catch-ups of %v for a seek: %v", keys, err)
	}
	var found []keptSoughtCatchUp
	for rows.Next() {
		row := record.OutboxRow{Kind: record.OutboxKindNotice}
		if err := rows.Scan(&row.ID, &row.Issue, &row.Payload, &row.LeaseToken, &row.LeaseUntil, &row.NextAt); err != nil {
			rows.Close()
			r.fatalf("scan a catch-up of %v for a seek: %v", keys, err)
		}
		if catchUp, ok := r.soughtCatchUp(row); ok {
			found = append(found, catchUp)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		r.fatalf("read the catch-ups of %v for a seek: %v", keys, err)
	}
	return found
}

// soughtCatchUp is row read as a catch-up of a driven tree that names kept decisions, false for
// any other row.
func (r *keptRun) soughtCatchUp(row record.OutboxRow) (keptSoughtCatchUp, bool) {
	tree := r.treeRow(row.Issue)
	if tree == nil || row.Kind != record.OutboxKindNotice {
		return keptSoughtCatchUp{}, false
	}
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		return keptSoughtCatchUp{}, false
	}
	notice, ok := payload.(record.Notice)
	if !ok || notice.CatchUp == nil || keptCount(notice.CatchUp.Kept) == 0 {
		return keptSoughtCatchUp{}, false
	}
	return keptSoughtCatchUp{row: row, notice: notice, tree: tree}, true
}

func (r *keptRun) isSoughtCatchUp(row record.OutboxRow) bool {
	_, ok := r.soughtCatchUp(row)
	return ok
}

// reachesItsPublish is whether a run of the catch-up now would publish it, read as the notice
// executor reads it (deliverNotice): the row still stands under the lease it holds, a newer
// catch-up does not supersede it, its architect did not stop with its finished tree, no wake of
// that architect is to be made or awaited, and no earlier notice of that architect fences it. A
// fenced row answers the fencing row, which a seek runs first (clearFence).
func (r *keptRun) reachesItsPublish(c keptSoughtCatchUp) (bool, int64) {
	issue, tree, _, err := r.runner.readNoticeTree(r.ctx, c.row, c.notice)
	if errors.Is(err, errNoticeRowGone) || errors.Is(err, errNoticeUnroutable) {
		return false, 0
	}
	if err != nil {
		r.fatalf("read the tree of catch-up row %d for a seek: %v", c.row.ID, err)
	}
	architect, err := noticeArchitect(tree.project, tree.issues, issue, c.notice, r.runner.holdsItsRole)
	if err != nil {
		return false, 0
	}
	held := r.runner.supervisedClaim(architect)
	if catchUpSuperseded(*c.notice.CatchUp, held.Claim, &issue) || stoppedWithTree(tree.root.Lingers(), held) {
		return false, 0
	}
	wake, err := r.runner.claimWakeOf(r.ctx, architect)
	if err != nil {
		r.fatalf("read the wake of %s for a seek: %v", architect, err)
	}
	if wake.resume || wake.parked {
		return false, 0
	}
	if earlier, _, _ := earlierNoticeFor(tree, architect, r.runner.holdsItsRole); earlier != 0 {
		return false, earlier
	}
	return true, 0
}

// clearFence is the step that takes the fencing notice id out of a sought catch-up's way: its run,
// once it is due (bringDue), once the forget's failure is off, so a catch-up that tells is not
// retried for a failed settle, and once the listener's standing mode lets it finish rather than
// wait for a holder or fail its forward; a step switches each first. A row no lane could claim
// even when due is left to the generator.
func (r *keptRun) clearFence(id int64) keptSeekPlan {
	if r.records.fails.Load() {
		return keptSeekPlan{steps: []func(){r.toggleForget}}
	}
	if mode := r.publisher.standingMode(); mode == keptNoHolder || mode == keptDeliveryFailed {
		return keptSeekPlan{steps: []func(){r.setModeForASeek(keptDelivers)}}
	}
	if keptRowIndex(r.claimable(), id) >= 0 {
		return keptSeekPlan{steps: []func(){r.runRow}, run: id}
	}
	var leased bool
	if err := r.pool.QueryRow(r.ctx, "select lease_until is not null and lease_until > now() from outbox where id = $1", id).Scan(&leased); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return keptSeekPlan{}
		}
		r.fatalf("read the lease of outbox row %d for a seek: %v", id, err)
	}
	if leased {
		return keptSeekPlan{}
	}
	return keptSeekPlan{steps: []func(){func() {
		r.bringDue(id)
		r.runRow()
	}}, run: id}
}

// bringDue makes row id due now, as its re-hold delay or its backoff passing would
// (record.Store.ExpediteOutbox), so a seek can run it in this step.
func (r *keptRun) bringDue(id int64) {
	r.expedite(id)
	r.tracef("bring-due id=%d: its delay has passed", id)
}

// setModeForASeek is the listener switched to mode, a step togglePublisher takes at random, taken
// by a seek that needs that answer.
func (r *keptRun) setModeForASeek(mode keptPublishMode) func() {
	return func() {
		r.publisher.setMode(mode)
		r.tracef("publisher=%s (a seek's step)", mode)
	}
}

func (p *keptPublisher) standingMode() keptPublishMode {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode
}

// toldMarked is whether the telling published under row has its told mark
// (record.KeptToldSource under record.OutboxKey).
func (r *keptRun) toldMarked(row int64) bool {
	var told bool
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		told, err = r.records.Processed(ctx, tx, record.KeptToldSource, record.OutboxKey(row))
		return err
	}); err != nil {
		r.fatalf("read the told mark of outbox row %d: %v", row, err)
	}
	return told
}

// untoldCopy is whether a copy the re-hold queued of row stands unclaimed while the telling of row
// has no told mark.
func (r *keptRun) untoldCopy(row int64) bool {
	var stands bool
	if err := r.pool.QueryRow(r.ctx, `select exists (select 1 from outbox where (payload->>'resend_of')::bigint = $1 and lease_token = '')`,
		row).Scan(&stands); err != nil {
		r.fatalf("read the copies of outbox row %d: %v", row, err)
	}
	return stands && !r.toldMarked(row)
}

// claimsKept is whether outbox row still claims kept decisions, which its settlement hands over
// (record.Store.ReleaseKeptClaims).
func (r *keptRun) claimsKept(row int64) bool {
	var claims bool
	if err := r.pool.QueryRow(r.ctx, "select exists (select 1 from parked_decisions where claimed_by = $1)", row).Scan(&claims); err != nil {
		r.fatalf("read what outbox row %d claims: %v", row, err)
	}
	return claims
}

// startRunningApplied is whether the start row of tree has applied the ready a start applies
// when it finds the architect running (the outbox's start, "start-running"), which a later run of
// that row applies no second time (intake.ApplyFact marks it processed).
func (r *keptRun) startRunningApplied(tree *keptTree, row int64) bool {
	var applied bool
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		applied, err = r.records.Processed(ctx, tx, "outbox", fmt.Sprintf("start-running:%s:%d", tree.token, row))
		return err
	}); err != nil {
		r.fatalf("read whether start row %d applied its ready: %v", row, err)
	}
	return applied
}

// launchComing is whether the tree's architect claim has a launch coming up that has not said it
// is ready, so the ready step (ready) makes it ready and its ready fact (readyFact) is the next
// one the workflow is told.
func (r *keptRun) launchComing(tree *keptTree) bool {
	return r.readyEnabled(tree) && !supervise.SaidReady(r.claimState(tree))
}

// seekToldAgain steers seed 41's run to its state: a decision told again after a catch-up whose
// telling write failed. The telling write is the told mark, written after the publish and before
// the forget (toldKept). A catch-up R of a driven tree that names kept decisions and would publish
// is run (runRow) with the listener delivering and its told mark failing (failsToldMark), while its
// tree has an architect to hear it. R is retried untold and unclaimed, and the seek holds it back
// (holdBack) until a drop of its tree's catch-ups comes, which the seek takes as soon as one is
// enabled (dropStep): the drop deletes R, so the foreign key releases the decisions it claimed and
// the fresh catch-up names them (workflow.QueueCatchUp). That fresh catch-up is then run, with the
// forget's failure off and the listener delivering, which tells the architect a decision a second
// time. A tree with a failed telling waiting for its drop has no other catch-up armed.
func (r *keptRun) seekToldAgain() keptSeekPlan {
	catchUps := r.soughtCatchUps()
	now := time.Now()
	waiting := map[string]bool{}
	for _, c := range catchUps {
		telling := c.telling()
		if !r.records.tellingFailed(telling) || r.toldMarked(telling) {
			continue
		}
		if drop, coming := r.dropStep(c.tree); coming {
			return drop
		}
		waiting[c.tree.key] = true
	}
	for _, c := range catchUps {
		if !r.namesWhatAFailedTellingTold(c) || c.leased(now) {
			continue
		}
		reaches, fence := r.reachesItsPublish(c)
		if fence != 0 {
			return r.clearFence(fence)
		}
		if !reaches || keptRowIndex(r.claimable(), c.row.ID) < 0 {
			continue
		}
		if r.records.fails.Load() {
			return keptSeekPlan{steps: []func(){r.toggleForget}}
		}
		if r.publisher.standingMode() != keptDelivers {
			return keptSeekPlan{steps: []func(){r.setModeForASeek(keptDelivers)}}
		}
		return keptSeekPlan{steps: []func(){r.runRow}, run: c.row.ID}
	}
	for _, c := range catchUps {
		telling := c.telling()
		if waiting[c.tree.key] || r.records.tellingFailed(telling) || r.toldMarked(telling) || c.leased(now) || !r.architectToHear(c.tree.key) {
			continue
		}
		reaches, fence := r.reachesItsPublish(c)
		if fence != 0 {
			return r.clearFence(fence)
		}
		if !reaches || keptRowIndex(r.claimable(), c.row.ID) < 0 {
			continue
		}
		if r.publisher.standingMode() != keptDelivers {
			return keptSeekPlan{steps: []func(){r.setModeForASeek(keptDelivers)}}
		}
		id := c.row.ID
		return keptSeekPlan{steps: []func(){func() {
			r.records.failsToldMark.Store(true)
			defer r.records.failsToldMark.Store(false)
			r.tracef("the told mark of the next catch-up telling fails (the seek of %q)", keptFaultToldAgainAfterAFailedWrite)
			r.runRow()
			if r.records.tellingFailed(telling) && !r.toldMarked(telling) {
				r.holdBack(id)
			}
		}}, run: id}
	}
	return keptSeekPlan{}
}

// holdBack keeps outbox row id out of every step of the drive: its next attempt moves an hour
// out, which no step reaches, as a lane's backoff keeps a failed row waiting (record.RetryBackoff)
// where the run's compressed retry leaves it due at once (retryRow). A drop of its issue's
// catch-ups deletes it, and quiesce brings it due if one never came (runLeftoverRow).
func (r *keptRun) holdBack(id int64) {
	if _, err := r.pool.Exec(r.ctx, "update outbox set next_at = $2 where id = $1 and lease_token = ''", id, time.Now().UTC().Add(time.Hour)); err != nil {
		r.fatalf("hold outbox row %d back: %v", id, err)
	}
	r.tracef("hold-back id=%d: it waits for the drop of its tree's catch-ups", id)
}

// dropStep is the step that drops tree's queued catch-ups now, or brings that drop one step
// nearer, and whether there is one: the ready fact of tree's architect, when its launch has said it
// is ready and the workflow has not been told (readyFact); that launch's ready, when it has not
// said it yet (ready); or the run of an architect start of tree that drops them (droppingStart).
func (r *keptRun) dropStep(tree *keptTree) (keptSeekPlan, bool) {
	if _, _, enabled := r.readyFactEnabled(tree); enabled {
		return keptSeekPlan{steps: []func(){func() { r.readyFact(tree) }}}, true
	}
	if r.launchComing(tree) {
		return keptSeekPlan{steps: []func(){func() { r.ready(tree) }}}, true
	}
	if start := r.droppingStart(tree); start != 0 {
		return keptSeekPlan{steps: []func(){r.runRow}, run: start}, true
	}
	return keptSeekPlan{}, false
}

// droppingStart is the claimable architect start of tree whose run drops tree's queued catch-ups
// (startDropRank), the first of the best rank, and 0 for none.
func (r *keptRun) droppingStart(tree *keptTree) int64 {
	best, pick := -1, int64(0)
	for _, row := range r.claimable() {
		if k := r.startDropRank(tree, row); k >= 0 && (best < 0 || k < best) {
			best, pick = k, row.ID
		}
	}
	return pick
}

// namesWhatAFailedTellingTold is whether the catch-up names a decision the architect of its tree
// was told by another catch-up whose telling write failed. Its own telling, which a run of the row
// or of a copy of it publishes again under the same key, is not another.
func (r *keptRun) namesWhatAFailedTellingTold(c keptSoughtCatchUp) bool {
	own := c.telling()
	for _, decision := range c.notice.CatchUp.Kept.Decisions {
		for _, telling := range r.told[decision.Ref] {
			if telling.tree == c.tree.key && !telling.prompt && telling.row != own && r.records.tellingFailed(telling.row) {
				return true
			}
		}
	}
	return false
}

// seekTheDrop steers seed 427's run to its state: a drop of a tree's catch-ups while a row of a
// kept telling is inside a publish that delivers and an unclaimed, untold copy of that telling
// stands. A catch-up R of a driven tree that names kept decisions, has no copy and would publish
// is run (runRow) with the listener answering a receipt timeout, at a moment its tree has an
// architect to hear it and an operation that drops that tree's catch-ups can run inside a publish
// (dropRaceable), so the re-hold queues its copy C and moves R's claims onto it; R's run then
// loses its finish to a crash (crashesForTheDrop), so R keeps its claim's token. Where no such
// operation can run yet but the launch R was written for is coming up (launchComing), that
// launch's ready is taken first: its ready fact is then one, and a ready of R's own launch
// supersedes nothing R states (catchUpSuperseded). Until the run lapses the crash's lease
// (keptCrashLapseSteps), the drive takes only steps that touch no row — the forget's, the
// listener's and the launches' switches — so nothing runs, drops or supersedes R or C meanwhile.
// Once it has lapsed, the forget's failure is switched off where it is on, so R's telling is not
// one whose write failed, and raceRow takes R again with a delivering answer and the dropping
// operation, the first of dropOpRank's order that is enabled. That drop is the state.
func (r *keptRun) seekTheDrop() keptSeekPlan {
	now := time.Now()
	if armed, ok := r.armedDrop(); ok {
		if r.crashed[armed.row.ID].until.After(now) {
			return keptSeekPlan{steps: []func(){r.toggleForget, r.togglePublisher, r.toggleLaunch}}
		}
		if r.records.fails.Load() {
			return keptSeekPlan{steps: []func(){r.toggleForget}}
		}
		reaches, _ := r.reachesItsPublish(armed)
		if !reaches || keptRowIndex(r.claimable(), armed.row.ID) < 0 || !r.dropRaceable(armed) {
			return keptSeekPlan{}
		}
		return keptSeekPlan{steps: []func(){r.raceRow}, race: armed.row.ID}
	}
	for _, c := range r.soughtCatchUps() {
		if c.notice.Resends != 0 || c.leased(now) || r.copiesOf(c.row.ID) != 0 || !r.architectToHear(c.tree.key) {
			continue
		}
		reaches, fence := r.reachesItsPublish(c)
		if fence != 0 {
			return r.clearFence(fence)
		}
		if !reaches || keptRowIndex(r.claimable(), c.row.ID) < 0 {
			continue
		}
		if !r.dropRaceable(c) {
			if tree := c.tree; r.launchComing(tree) && r.runner.supervisedClaim(tree.token).Claim.Generation == c.notice.CatchUp.Launch {
				return keptSeekPlan{steps: []func(){func() { r.ready(tree) }}}
			}
			continue
		}
		if r.publisher.standingMode() != keptReceiptTimeout {
			return keptSeekPlan{steps: []func(){r.setModeForASeek(keptReceiptTimeout)}}
		}
		return keptSeekPlan{steps: []func(){r.runRow}, run: c.row.ID}
	}
	return keptSeekPlan{}
}

// armedDrop is the catch-up R the seek of seed 427's state has armed: a catch-up of a driven tree
// that names kept decisions, holds the token a crash left it (noteCrash) and has an unclaimed,
// untold copy.
func (r *keptRun) armedDrop() (keptSoughtCatchUp, bool) {
	for _, c := range r.soughtCatchUps() {
		if crash, crashed := r.crashed[c.row.ID]; crashed && crash.token == c.row.LeaseToken && c.notice.Resends == 0 && r.untoldCopy(c.row.ID) {
			return c, true
		}
	}
	return keptSoughtCatchUp{}, false
}

func (r *keptRun) isArmedDrop(row record.OutboxRow) bool {
	armed, ok := r.armedDrop()
	return ok && armed.row.ID == row.ID
}

// dropRaceable is whether an operation that drops the queued catch-ups of c's tree can run inside
// c's publish now (dropOpRank).
func (r *keptRun) dropRaceable(c keptSoughtCatchUp) bool {
	return slices.ContainsFunc(r.raceOps(c.row.ID), func(op keptRaceOp) bool { return r.dropOpRank(c.tree, op) >= 0 })
}

// dropOpRank ranks an operation for the race of seed 427's state by whether, run inside a
// publish, it drops tree's queued catch-ups (record.Store.DropCatchUps, through
// workflow.QueueCatchUp): the ready fact of tree's architect first (workflow's claimReady), then
// the run of a start of tree that drops them in startDropRank's order, and -1 for one that drops
// nothing.
func (r *keptRun) dropOpRank(tree *keptTree, op keptRaceOp) int {
	switch op.kind {
	case "ready":
		if op.tree == tree {
			return 0
		}
	case "row":
		if k := r.startDropRank(tree, op.row); k >= 0 {
			return 1 + k
		}
	}
	return -1
}

// startDropRank ranks an architect start of tree by whether its run drops tree's queued
// catch-ups: first one of tree's generation whose run finds the architect said it is ready and
// applies that ready itself (the outbox's start, "start-running"), which it reaches only when it
// has no task to give or the claim holds none already (a second is refused), and only once
// (startRunningApplied); then one of an earlier generation than tree's, which the outbox finishes
// without acting, whose settlement hands what it still claims (claimsKept) to a fresh catch-up
// unless the claim took its task (the outbox's settleKept, workflow.HandOverKept); and -1 for any
// other row.
func (r *keptRun) startDropRank(tree *keptTree, row record.OutboxRow) int {
	request, ok := keptArchitectStart(tree, row)
	if !ok {
		return -1
	}
	held := r.runner.supervisedClaim(tree.token).Claim
	generation := r.storedGeneration(tree.key)
	if request.Generation == generation && supervise.SaidReady(held.State) && (request.Task == "" || held.Pending == nil) &&
		!r.startRunningApplied(tree, row.ID) {
		return 0
	}
	if request.Generation < generation && r.claimsKept(row.ID) && !slices.Contains(held.DeliveredRows, row.ID) {
		return 1
	}
	return -1
}

// keptArchitectStart is the request of row when row is a start of tree's architect that is not for
// news alone (record.SuperviseRequest.News), and false for any other row.
func keptArchitectStart(tree *keptTree, row record.OutboxRow) (record.SuperviseRequest, bool) {
	if row.Issue != tree.key || row.Kind != record.OutboxKindSupervise {
		return record.SuperviseRequest{}, false
	}
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		return record.SuperviseRequest{}, false
	}
	request, ok := payload.(record.SuperviseRequest)
	if !ok || request.Op != "start" || request.News || request.Role != claim.RoleArchitect {
		return record.SuperviseRequest{}, false
	}
	return request, true
}

// seekTheSpentRehold steers seed 701's run to its state: a catch-up's re-holds spent while its
// architect holds its launch. A catch-up of a driven tree whose architect holds the launch the
// catch-up was written for (holdsItsLaunch), and whose tree nothing else stands to tell again
// (nothingRetells), naming kept decisions, is raced (raceRow) with a receipt timeout, the one
// re-held the most times first, after a copy's re-hold delay is brought to its end when it is not
// due yet (bringDue), so the re-hold queues the next copy, until a copy whose re-holds are spent
// (Resends == len(noticeReholdDelays)) draws the receipt timeout and the re-hold takes its spent
// branch. A catch-up an earlier notice of its architect fences waits while that notice runs
// (clearFence), and one a newer catch-up supersedes is passed over. The operation inside each
// publish is one that supersedes nothing the re-hold reads (spentReholdOpRank).
//
// Once the state's marker has fired, the seek holds the state open for the rest of the run, as
// holdsTheWindowOpen holds the hand-over's window (spentHold): the drive leaves out the listener's
// switch (togglePublisher), and switches a listener standing at refusing or denying to delivering
// (holdTheSpentState), so no refusal of the held tree's catch-up has quiesce relaunch its
// architect (relaunchAfterRefusal); and for the held tree it leaves out the death of the process in
// a task's turn (deathInTheTasksTurn), the root's park (park) where the held tree is the root, and
// the adoption of the queued root into it (adoptQueued) (spentHoldOn). Every other step stays. So
// nothing but the spent branch's own hand-over (spentKept) names what the spent copy claimed: with
// that hand-over its fresh catch-up tells it, and without it the decision is left with nothing to
// tell it, which held reports.
func (r *keptRun) seekTheSpentRehold() keptSeekPlan {
	now := time.Now()
	catchUps := r.soughtCatchUps()
	slices.SortStableFunc(catchUps, func(a, b keptSoughtCatchUp) int { return cmp.Compare(b.notice.Resends, a.notice.Resends) })
	for _, c := range catchUps {
		if !r.holdsItsLaunch(c) || !r.nothingRetells(c.tree) {
			continue
		}
		reaches, fence := r.reachesItsPublish(c)
		if fence != 0 {
			return r.clearFence(fence)
		}
		if !reaches || c.leased(now) || !slices.ContainsFunc(r.raceOps(c.row.ID), func(op keptRaceOp) bool { return r.spentReholdOpRank(c.tree, op) >= 0 }) {
			continue
		}
		id := c.row.ID
		if !c.row.NextAt.After(now) {
			if keptRowIndex(r.claimable(), id) < 0 {
				continue
			}
			return keptSeekPlan{steps: []func(){r.raceRow}, race: id}
		}
		if c.notice.Resends == 0 {
			continue
		}
		return keptSeekPlan{steps: []func(){func() {
			r.bringDue(id)
			if keptRowIndex(r.keptCatchUps(r.claimable()), id) >= 0 {
				r.raceRow()
			}
		}}}
	}
	return keptSeekPlan{}
}

// holdsItsLaunch is whether the architect of c's tree holds the launch c was written for: its
// claim holds its role, has said it is ready on that launch, and the workflow has been told that
// ready (readyFactEnabled), so no ready of that architect is coming that would name what c claims,
// and the re-hold does not find it stopped with its tree (reholdNotice's stoppedWithTree). Without
// the spent branch's hand-over (spentKept), what c claims would then wait for that architect's next
// launch, which may never come.
func (r *keptRun) holdsItsLaunch(c keptSoughtCatchUp) bool {
	held := r.runner.supervisedClaim(c.tree.token)
	_, _, untold := r.readyFactEnabled(c.tree)
	return holdsRole(held) && supervise.SaidReady(held.State) && held.Generation == c.notice.CatchUp.Launch && !untold
}

// nothingRetells is whether nothing the run already holds would name tree's kept decisions again
// once the hold begins (holdTheSpentState): the tree has an architect to hear it, so quiesce
// re-admits nothing of it; none of its catch-ups was finished undelivered by a refusal, which
// quiesce answers with its architect's relaunch (relaunchAfterRefusal); and no start of its
// architect stands that would: one of tree's generation, whose run applies a ready (the outbox's
// start, "start-running") or relaunches the architect, or one of an earlier generation that still
// claims kept decisions its claim never took, whose settlement hands them to a fresh catch-up
// (settleKept). A start for news alone does neither (record.SuperviseRequest.News).
func (r *keptRun) nothingRetells(tree *keptTree) bool {
	if !r.architectToHear(tree.key) || r.refused[tree.key] {
		return false
	}
	generation := r.storedGeneration(tree.key)
	delivered := r.runner.supervisedClaim(tree.token).Claim.DeliveredRows
	for _, row := range r.rows(false) {
		request, ok := keptArchitectStart(tree, row)
		if !ok {
			continue
		}
		if request.Generation == generation || r.claimsKept(row.ID) && !slices.Contains(delivered, row.ID) {
			return false
		}
	}
	return true
}

// spentReholdOpRank ranks an operation for the races of seed 701's state on a catch-up of tree:
// the run of a start of another tree first, which a refused launch leaves to run again; then any
// other operation of another tree — its ready, a hand-over, the re-admission of the root when tree
// is not the root's; then one of tree itself, whose ready or start may queue a catch-up that
// fences the next copy until it runs; and -1 for the root's re-admission on a catch-up of the
// root, which supersedes it (catchUpSuperseded).
func (r *keptRun) spentReholdOpRank(tree *keptTree, op keptRaceOp) int {
	switch {
	case op.kind == "readmit" && tree == r.root:
		return -1
	case op.kind == "ready" && op.tree == tree, op.kind == "row" && op.row.Issue == tree.key:
		return 2
	case op.kind == "row":
		return 0
	}
	return 1
}
