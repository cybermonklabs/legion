package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// DecisionStep is the step of this release's boot and of a tick of its own that brings what the
// release before wrote onto the store. Its phases run in
// order:
//
//  1. The backfill (graphcensus.BackfillAsks): the object nodes of every row written before its
//     kind's writer wrote one, then every asks and ask_followers row that names no node, stated on
//     the ask's node and marked, in committed batches.
//  2. The catch-up of the events a process of another build appended on a row this build marked
//     (catchUp): its ask.answered, ask.resolved, ask.opened, ask.follower_added and
//     ask.follower_removed, each stated on the ask's node under its own event, from its payload,
//     and never from the row.
//  3. The restore and the conversion (RestoreAndConvert): the gated steps rewritten without
//     `decisions`, then, for each issue that still holds a decisions document or an answered ask
//     whose block left its spec, oldest first and in one transaction each, the restore of those
//     asks into the spec, the conversion of every section of the decisions document, and the
//     document's retirement once it holds none (restoreIssueDecisions). It does nothing while
//     processes.Live lists a process of a commit other than this one's (anotherBuildLive), since
//     any other build can write a row without its node, and a pass that finds one live in an
//     issue's transaction stops there, with that issue rolled back, and starts again once the
//     fleet is one build. It needs the document service (UseDocuments), which writes the
//     documents.
//
// It runs at boot, before the server serves, where only phase 1 runs and a failure stops the boot
// (Boot). Readers that check an ask's node - a reading's check (askAnswers), this step's own
// count, and approval carry (asksAgreeWithTheirNodes) - can only use the facts the store holds.
// Boot states the older rows before this process serves; readers such as onItsNode fall back to
// the row for an ask that still names no node. The boot reads only what phase 1 needs - whether
// a row stands unmarked (graphcensus.AsksOwed), and the rows the backfill states - and nothing
// else: a table no phase 1 statement reads, held by another transaction, must not keep the process
// from serving. It then
// runs on a tick of its own (Run) every BlockIndexReconcileInterval for as long as the process
// runs: a process of another build can go live after a tick found nothing to do - a rollback, or a
// process of the release before that starts late - so the tick never stops for want of work.
// Phase 1 has work while an unmarked row stands (graphcensus.AsksOwed). Phase 2 has work on each
// tick while processes.Live lists a process of another build, once more on the first tick that
// finds the fleet one build, which is also a process's first tick - the window it reads opens
// before this process started, so a restart of a process on a fleet that stood whole never misses
// a write - and again after any pass on which an ask failed. Each tick answers what the restore and
// the conversion owe (DecisionStepOwed), which /healthz answers (Owed). The boot counts none of it,
// since that count reads document_block_versions, which phase 1 never reads: the count stands
// absent until the process's first tick has run.
//
// A tick with no work reads no table whole (probe): it reads the live processes, the newest event
// and whether any document's block index is owed, each one probe of an index, and where events were
// appended since its last read, a mark of the event log and whether one of them is of a write that
// can move the count of what the restore and the conversion owe (recountOwed), which reads those
// events through their primary key, and the document each version among them names by its key.
// graphcensus.AsksOwed reads its kinds' tables to the end when nothing is owed, so it runs at
// boot, on a tick that owes a catch-up, and after a tick that saw another build live, until it
// finds nothing: this build's own create sites write their nodes and markers, so only another
// build writes a row it finds. The count reads whole tables too, so
// where it read zero it is read again only when something can have made it non-zero: another build
// live, a phase of the step run, a block index owed, or an event of a write that can
// (decisionStepOwedMoved). The events of every other write, which a fleet appends all the time, a
// version of any document but an issue's spec among them, leave the count standing.
//
// The runner takes a session advisory lock of its own (decisionStepLock) for phases 1 and 2, so
// two processes do not run them both at once. A booting process waits for it only while phase 1
// owes a row, so every process has phase 1 behind it before it serves. Where phase 1 owes
// nothing, a booting process takes no lock, so a tick that holds the lock for phase 2, or a
// process stalled while it holds it, keeps no boot waiting. A tick that finds the lock held skips,
// since the holder is doing the work. Phase 3 takes a second key of its own
// (decisionRestoreLock) once phases 1 and 2 are behind it and the first key is released, so a
// process that boots while a pass of phase 3 runs - every owed issue, one transaction each - or
// while someone runs it by hand waits for no part of that pass; a tick that finds the second key
// held skips phase 3. A tick runs no phase 3 after a catch-up that failed: the restore's finder
// reads an answer from its ask's node, and an answer the catch-up did not state there would read
// as none. A failure on one batch of phase 1 rolls back that batch, and one on one ask of phase 2
// that ask; either is logged with its reason and tried again at the next tick.
// kept for one release: decisions-step
type DecisionStep struct {
	database *store.Store
	commit   string
	logger   *slog.Logger
	// documents is the document service phase 3 writes the documents through (UseDocuments); nil
	// until a caller gives it one, and phase 3 does not run while it is.
	documents *Service
	// interval is the wait between two ticks of Run: BlockIndexReconcileInterval.
	interval time.Duration
	// catchUpOwed is whether phase 2 owes a pass on a tick that finds the fleet one build: true
	// until the first such tick runs it, and again after any tick that finds another build live or
	// on which an ask failed.
	// kept for one release: catch-up
	catchUpOwed bool
	// horizon marks the event log for the catch-up and the count (writerFloor), and marks are the
	// marks their passes took, newest first and at most decisionStepMarksKept of them.
	horizon *events.Horizon
	marks   []events.Mark
	// caughtUpThrough and caughtUpAbove are the catch-up's high-water mark: every event of the
	// window at or below caughtUpThrough, and each event above it whose id caughtUpAbove holds,
	// was read by a pass on which no ask failed, and so applied or found to need nothing.
	// kept for one release: catch-up
	caughtUpThrough int64
	// kept for one release: catch-up
	caughtUpAbove []int64
	// windowOpens is where the catch-up's window opens (catchUpWindowOpens), and windowRead
	// whether a pass has read it: the first pass reads it, and every pass after it reads from it.
	// kept for one release: catch-up
	windowOpens int64
	// kept for one release: catch-up
	windowRead bool
	// backfillUnknown is whether a tick reads graphcensus.AsksOwed even where it owes no
	// catch-up: true until a read of it finds nothing owed with no other build live, and again
	// once a tick sees another build live.
	backfillUnknown bool
	// recount is whether the next tick counts what the restore and the conversion owe whatever
	// the event log says (recountOwed): true until the first count, after a tick that saw another
	// build live, and after a tick that ran a phase, which states facts under events already
	// appended. countedThrough is the writer floor of the event log read before the last count, or
	// before a later read that found no event since able to move it (decisionStepOwedMoved), and
	// countedIndexOwed whether a document's block index was owed when the count ran.
	recount          bool
	countedThrough   int64
	countedIndexOwed bool
	// owed is what the last count found owed, nil until a pass has run.
	owed atomic.Pointer[DecisionStepOwed]
}

// DecisionStepOwed is what the step's last pass found the restore and the conversion owe: the
// decisions documents still standing, the answered asks whose block left their spec, the stored
// lists of gated steps that hold `decisions`, and their sum. /healthz answers the sum alone, as
// `decisions_step_owed` (OwedTotal), which a deploy check reads at zero as the step having nothing
// left to do; each count logs the three parts beside it (recountOwed).
// kept for one release: decisions-step
type DecisionStepOwed struct {
	DecisionsDocuments int64
	AnswersLeftSpec    int64
	GatedStepsLists    int64
	Total              int64
}

// Owed is what the step's last pass found owed, part by part, nil before a tick has counted it -
// and so through the boot, whose pass counts nothing, for as long as
// DISPATCH_TEST_HOLD_DECISIONS_STEP holds the step, and on a nil step.
// kept for one release: decisions-step
func (s *DecisionStep) Owed() *DecisionStepOwed {
	if s == nil {
		return nil
	}
	return s.owed.Load()
}

// OwedTotal is the sum of what the step's last pass found owed (Owed), nil while Owed is.
// kept for one release: decisions-step
func (s *DecisionStep) OwedTotal() *int64 {
	owed := s.Owed()
	if owed == nil {
		return nil
	}
	total := owed.Total
	return &total
}

// countDecisionStepOwed reads what the restore and the conversion owe, through the finders the
// third phase itself reads (DecisionStep.RestoreAndConvert), so the count and the step read one
// finder each:
//
//   - a decisions document is one decisions_documents() answers: an issue's unretired document
//     whose role is decisions, or, stored before the role existed, whose slug is that document's;
//   - an answer whose block left its spec is the main shape of the restore's work
//     (graphcensus.ReadRestoreWork, graphcensus.RestoreReturns): an answered `question` ask, as
//     its node holds it, whose row names its issue's spec and whose block id no `document_blocks`
//     row of type `ask` holds in that spec and that no person removed from it (removed_by),
//     leaving out a spec whose block index is owed, whose blocks are not known yet - the second
//     shape's asks stand in a decisions document, which the first part counts;
//   - a list of gated steps that holds `decisions` is a projects row whose gated_steps does.
//
// kept for one release: decisions-step
func countDecisionStepOwed(ctx context.Context, q graphstore.Queryer) (DecisionStepOwed, error) {
	var owed DecisionStepOwed
	if err := q.QueryRow(ctx, `
		select (select count(*) from decisions_documents()), -- kept for one release: decisions-documents-fn
		       (select count(*) from projects p where 'decisions' = any(p.gated_steps))
	`).Scan(&owed.DecisionsDocuments, &owed.GatedStepsLists); err != nil {
		return DecisionStepOwed{}, fmt.Errorf("count what the decisions step owes: %w", err)
	}
	work, err := graphcensus.ReadRestoreWork(ctx, q)
	if err != nil {
		return DecisionStepOwed{}, fmt.Errorf("count what the decisions step owes: %w", err)
	}
	for _, ask := range work {
		if ask.Shape == graphcensus.RestoreReturns {
			owed.AnswersLeftSpec++
		}
	}
	owed.Total = owed.DecisionsDocuments + owed.AnswersLeftSpec + owed.GatedStepsLists
	return owed, nil
}

// NewDecisionStep is the step of a process serving commit, the commit it registers
// (processes.Instance.Commit), over database, logging through logger (slog.Default when nil).
// kept for one release: decisions-step
func NewDecisionStep(database *store.Store, commit string, logger *slog.Logger) *DecisionStep {
	if logger == nil {
		logger = slog.Default()
	}
	return &DecisionStep{
		database: database, commit: commit, logger: logger, interval: BlockIndexReconcileInterval,
		catchUpOwed: true, backfillUnknown: true, recount: true, horizon: events.NewHorizon(database.Pool),
	}
}

// decisionStepLock is the session advisory lock the step's first two phases take ("DDECISNS"), a
// single 64-bit key apart from every other single-key advisory lock of the server.
// kept for one release: decisions-step
const decisionStepLock int64 = 0x4444454349534E53

// decisionRestoreLock is the session advisory lock the step's third phase takes ("DRESTORE"), a
// single 64-bit key apart from decisionStepLock and every other single-key advisory lock of the
// server: a pass of the third phase holds it, and never the step's own lock, which a booting
// process waits for while phase 1 owes a row (Boot).
// kept for one release: decisions-step
const decisionRestoreLock int64 = 0x44524553544F5245

// stepConn is the connection the step holds its session locks on, one of the shared pool's, so it
// is one store.ConnectionsPerProcess already counts, and held names the lock it may hold there
// now: decisionStepLock, decisionRestoreLock, or 0 for none.
// kept for one release: decisions-step
type stepConn struct {
	conn *pgxpool.Conn
	held int64
}

// acquireStepConn takes the shared pool's connection the step holds its locks on, for one pass,
// which gives it back (release). The acquire marks nothing on ctx (store.Pool.Acquire), so the
// pass's own transactions take connections of the same pool beside it, as the migration runner's
// transactions do beside its lock's: no caller in the process waits for a lock of the step's -
// a tick tries the lock, and a booting process of another server waits for it on a pool of its
// own - so the two connections cannot close the cycle the nested-acquire guard exists for.
// kept for one release: decisions-step
func (s *DecisionStep) acquireStepConn(ctx context.Context) (*stepConn, error) {
	conn, err := s.database.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("take the decision step's connection: %w", err)
	}
	return &stepConn{conn: conn}, nil
}

// lock takes key, waiting for it where wait and otherwise trying it once, and reports whether the
// connection holds it. key counts as held from before the statement, so a statement that fails
// leaves release to unlock it, or to close the connection where Postgres does not answer that the
// unlock freed it.
// kept for one release: decisions-step
func (c *stepConn) lock(ctx context.Context, key int64, wait bool) (bool, error) {
	c.held = key
	if wait {
		if _, err := c.conn.Exec(ctx, `select pg_advisory_lock($1)`, key); err != nil {
			return false, err
		}
		return true, nil
	}
	var locked bool
	if err := c.conn.QueryRow(ctx, `select pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		return false, err
	}
	if !locked {
		c.held = 0
	}
	return locked, nil
}

// release gives the connection back to the pool: at once where it holds no lock, and otherwise
// through duty.FreeSessionLock, which returns it only once Postgres answers that the unlock freed
// the lock and closes it otherwise, so a pooled connection that may still hold a lock of the step's
// serves no later caller and keeps no other process's boot waiting.
// kept for one release: decisions-step
func (c *stepConn) release(logger *slog.Logger) {
	if c.held == 0 {
		c.conn.Release()
		return
	}
	err := duty.FreeSessionLock(c.conn, `select pg_advisory_unlock($1)`, c.held)
	switch {
	case err == nil:
	case errors.Is(err, duty.ErrSessionClosed):
		logger.Info("dispatch: the decisions step's connection was closed before its unlock; its session ended the lock",
			"key", c.held, "error", err)
	default:
		logger.Error("dispatch: the decisions step's lock was not released; its connection leaves the pool",
			"key", c.held, "error", err)
	}
}

// Boot is the step's pass at boot: it runs phase 1 where a row stands unmarked, and waits for the
// step's lock only then. It first reads whether phase 1 owes a row (graphcensus.AsksOwed) on the
// shared pool, holding no lock. Where nothing is owed it answers at once, having held no
// connection of the step's (acquireStepConn) and taken no lock, so a tick that holds the lock for
// phase 2, or a process stalled while it holds it, keeps no boot waiting. A row the release before writes after that
// read is a tick's work, as a row written after a read under the lock would be. Where a row is
// owed, it waits for the step's lock and reads AsksOwed again, since the holder may have stated
// the row meanwhile, and runs phase 1 where one still stands. It answers a failure of any of
// these, which stops the boot; a row phase 1 leaves because the store holds its node in another
// project is no failure (backfill). It reads nothing else - not whether another build is live, which
// phase 1 does not wait on, and not what the restore and the conversion owe, which the process's
// first tick counts (probe) - so the boot waits on no table phase 1 does not read. Its context is
// marked for the pool's nested-acquire guard (store.WithTransactionTracking), as every entry point
// of the pool's transactions marks its own.
// kept for one release: decisions-step
func (s *DecisionStep) Boot(ctx context.Context) error {
	ctx = store.WithTransactionTracking(ctx)
	owed, err := graphcensus.AsksOwed(ctx, s.database)
	if err != nil || !owed {
		return err
	}
	conn, err := s.acquireStepConn(ctx)
	if err != nil {
		return err
	}
	defer conn.release(s.logger)
	if _, err := conn.lock(ctx, decisionStepLock, true); err != nil {
		return fmt.Errorf("take the decision step's lock: %w", err)
	}
	if owed, err = graphcensus.AsksOwed(ctx, s.database); err != nil || !owed {
		return err
	}
	return s.backfill(ctx)
}

// Tick is one pass of the step on its tick. It probes what each phase owes first (probe), on the
// shared pool, so a tick with no work - every tick once the fleet is one build and every row is
// stated - costs those probes and holds no connection past them. Otherwise it takes the step's
// lock on the shared pool's connection it holds for the pass (acquireStepConn), skips where
// another process holds it, since the holder is doing the work, and runs each of phases 1 and 2
// with work; then, where phase 3 has work and no catch-up
// of this tick failed, it releases the step's lock and takes phase 3's (takeRestoreLock), and
// runs phase 3, or skips it where another process holds that lock. A failure of a phase is logged
// and tried again at the next tick; it answers a failure of the probes or of a lock. Its context is
// marked for the pool's nested-acquire guard, as Boot's is.
// kept for one release: decisions-step
func (s *DecisionStep) Tick(ctx context.Context) error {
	ctx = store.WithTransactionTracking(ctx)
	work, err := s.probe(ctx)
	if err != nil {
		return err
	}
	// kept for one release: catch-up
	catchUp := work.other || s.catchUpOwed
	// Phase 3 has work while the last count found something owed, and runs only on a fleet of
	// this one build.
	owed := s.Owed()
	restore := s.documents != nil && !work.other && owed != nil && owed.Total > 0
	if !work.backfill && !catchUp && !restore {
		return nil
	}
	conn, err := s.acquireStepConn(ctx)
	if err != nil {
		return err
	}
	defer conn.release(s.logger)
	locked, err := conn.lock(ctx, decisionStepLock, false)
	if err != nil {
		return fmt.Errorf("take the decision step's lock: %w", err)
	}
	if !locked {
		return nil
	}
	// A phase states facts under events already appended, an answer state among them, which the
	// count reads: the next tick counts again.
	s.recount = true
	if work.backfill {
		if err := s.backfill(ctx); err != nil {
			s.logger.Error("dispatch: decisions step backfill; the next tick tries again", "error", err)
		}
	}
	// kept for one release: catch-up
	if catchUp {
		failed, err := s.catchUp(ctx)
		if err != nil {
			s.logger.Error("dispatch: decisions step catch-up; the next tick tries again", "error", err)
		}
		s.catchUpOwed = work.other || failed || err != nil
		// The restore's finder reads an ask's answer from its node, which an answer the catch-up
		// did not state leaves unanswered, so phase 3 waits for a tick whose catch-up went through,
		// as RunByHand refuses it.
		restore = restore && !failed && err == nil
	}
	if !restore {
		return nil
	}
	if restoring, err := conn.takeRestoreLock(ctx, false); err != nil || !restoring {
		return err
	}
	if pass, err := s.RestoreAndConvert(ctx, "", true, false); err != nil {
		s.logger.Error("dispatch: decisions step restore and conversion; the next tick tries again", "error", err)
	} else {
		for _, failure := range pass.Failed {
			s.logger.Error("dispatch: decisions step restore and conversion of an issue; the next tick tries again",
				"issue", failure.Issue, "error", failure.Err)
		}
	}
	return nil
}

// takeRestoreLock releases the step's lock (decisionStepLock), which c holds, and takes phase 3's
// (decisionRestoreLock) on c, so the pass of phase 3 it is taken for holds no lock a booting
// process waits for: it waits for the lock where wait, as a command run by hand does, and
// otherwise tries once, reporting whether it took it, as a tick does.
// kept for one release: decisions-step
func (c *stepConn) takeRestoreLock(ctx context.Context, wait bool) (bool, error) {
	if _, err := c.conn.Exec(ctx, `select pg_advisory_unlock($1)`, decisionStepLock); err != nil {
		return false, fmt.Errorf("release the decision step's lock: %w", err)
	}
	c.held = 0
	locked, err := c.lock(ctx, decisionRestoreLock, wait)
	if err != nil {
		return false, fmt.Errorf("take the lock of the decision step's third phase: %w", err)
	}
	return locked, nil
}

// UseDocuments gives the step the document service its third phase writes the documents through.
// A process calls it before the step's first tick; until it does, phase 3 does not run.
// kept for one release: decisions-step
func (s *DecisionStep) UseDocuments(documents *Service) {
	s.documents = documents
}

// Run ticks at once and then every interval (BlockIndexReconcileInterval) until ctx ends. It never
// stops for want of work: a process of another build can go live after a tick found none, and the
// next tick catches up what it writes. A tick that fails is logged and tried again at the next one.
// kept for one release: decisions-step
func (s *DecisionStep) Run(ctx context.Context) {
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("dispatch: decisions step tick; the next tick tries again", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.interval):
		}
	}
}

// decisionStepWork is what a pass's probes found the step's phases owe.
// kept for one release: decisions-step
type decisionStepWork struct {
	// backfill is whether phase 1 has a row to state (graphcensus.AsksOwed).
	backfill bool
	// other is whether processes.Live lists a process of another build.
	other bool
}

// probe reads what a tick owes: whether a process of another build is live, what the restore and
// the conversion owe (recountOwed), which Owed answers from then on, and whether phase 1 has a row
// to state. graphcensus.AsksOwed reads the tables of its kinds to the end when nothing is owed - an
// anti-join of issues, artifacts, asks and messages to graph_nodes, then asks and ask_followers,
// which no index keys on a null marker - so it is read only at boot (Boot), on a tick that owes a
// catch-up, and after a tick that saw another build live, until a read of it finds nothing owed
// with no other build live: this build's create sites write each node and marker in the write that
// stores the row, so a row it finds is one another build wrote. Every other tick takes phase 1 to
// owe nothing.
// kept for one release: decisions-step
func (s *DecisionStep) probe(ctx context.Context) (decisionStepWork, error) {
	var work decisionStepWork
	var err error
	if work.other, err = s.anotherBuildLive(ctx, s.database.Pool); err != nil {
		return decisionStepWork{}, err
	}
	if work.other {
		s.backfillUnknown, s.recount = true, true
	}
	if err := s.recountOwed(ctx, work.other); err != nil {
		return decisionStepWork{}, err
	}
	// kept for one release: catch-up
	if s.backfillUnknown || s.catchUpOwed {
		if work.backfill, err = graphcensus.AsksOwed(ctx, s.database); err != nil {
			return decisionStepWork{}, err
		}
		s.backfillUnknown = work.backfill || work.other
	}
	return work, nil
}

// decisionStepOwedMovers is the type list of the events whose writes can make a count of nothing
// owed (countDecisionStepOwed) read something owed, written out as caughtUpTypes is: a document
// stored (artifact.created), which the release before stores under a decisions document's slug; a
// version of an issue's spec (artifact.version, which decisionStepOwedMoved reads the document
// of), which can leave an answered ask's block out of it; an answer (ask.answered), which can
// answer an ask whose block its spec no longer holds; and a project stored or changed
// (project.created, project.updated), whose list of gated steps can hold `decisions`. No other
// write moves the count up from zero: a rename keeps the document's slug, a resolution, a
// reopening, a removal and a retirement only take from it, and the step's own phases count again
// (recount).
// kept for one release: decisions-step
const decisionStepOwedMovers = `('artifact.created', 'artifact.version', 'ask.answered', 'project.created', 'project.updated')`

// decisionStepOwedMoved is whether the event log holds an event above $1 that can move the count:
// one of decisionStepOwedMovers, and of those an artifact.version only where the document it names
// (its payload's artifact_id, ArtifactVersionEventPayload) is an issue's spec. The read runs only
// where the last count stood at zero (recountOwed), so decisions_documents() listed no document
// then, and no version lists one: a document enters that list only as its row is stored, under
// its artifact.created, a mover of its own. No write changes a stored document's slug, issue or
// kind or undoes its retirement, and the repair that gives a stored document the role it lacks
// gives the one its slug names (api's documentRole), which leaves a decisions document listed
// as it was, by its slug. So the version of a decisions document a count of zero can meet follows
// that document's artifact.created, which moves the count already, and the one document whose
// version can move a zero count is an issue's spec, which can leave an answered ask's block out
// of it. Every settled edit, upload and doc-edit of any document appends an artifact.version, so a
// version of any other document leaves the count standing. It reads the events through their
// primary key from $1 on, so it reads the events appended since and stops at the first that can
// move the count, and the document of each version among them by its key. The document is looked
// for by a scalar subquery, `(select … limit 1) is not null`, rather than by EXISTS, which
// Postgres can plan as a hashed subplan over every document, read once and kept beside the events
// (caughtUpRead's reason).
// kept for one release: decisions-step
var decisionStepOwedMoved = `
	select (select e.id from events e
	        where e.id > $1 and e.type in ` + decisionStepOwedMovers + `
	          and (e.type <> 'artifact.version'
	               or (select 1 from artifacts a
	                   where a.id = ` + store.CastableUUID(`e.payload->>'artifact_id'`) + `
	                     and a.issue_key is not null and a.is_primary
	                   limit 1) is not null)
	        order by e.id limit 1) is not null`

// recountOwed counts what the restore and the conversion owe (countDecisionStepOwed), which reads
// whole tables, where the last count read something owed, and where it read nothing only when
// something can have made it non-zero since: another build live (recount), a phase of this step
// run (recount), or a write of this build or another that appends an event above countedThrough
// that can move the count (decisionStepOwedMoved), the writer floor read before the last count or
// the last read of the events. A document's block index the count waits for (a spec whose index is
// owed is left out) moves with no event, so the count is read again while any is owed, and once
// more after. Otherwise it reads the newest event and whether a block index is owed, each one probe
// of an index, and where no event was appended since countedThrough, it leaves the last count
// standing. Where one was, it marks the event log (writerFloor) and reads whether an event above
// countedThrough can move the count (decisionStepOwedMoved), which reads only the events
// appended since; where none is, every event at or below the floor was final before that read and
// none of them can move the count, so countedThrough moves up to the floor and the last count
// stands. The floor is read before the count and before that read, so an event either did not see
// is above it; an event log reset (newest below countedThrough) is a change too.
// kept for one release: decisions-step
func (s *DecisionStep) recountOwed(ctx context.Context, other bool) error {
	var newest int64
	var indexOwed bool
	if err := s.database.Pool.QueryRow(ctx, `
		select coalesce((select id from events order by id desc limit 1), 0),
		       exists (select 1 from document_block_versions where version is null)
	`).Scan(&newest, &indexOwed); err != nil {
		return fmt.Errorf("read whether the decisions step's count can have moved: %w", err)
	}
	last := s.owed.Load()
	standing := last != nil && last.Total == 0 && !s.recount && !indexOwed && !s.countedIndexOwed
	if standing && newest == s.countedThrough {
		return nil
	}
	floor, _, err := s.writerFloor(ctx)
	if err != nil {
		return fmt.Errorf("read which events the decisions step's count reads final: %w", err)
	}
	if standing && newest > s.countedThrough {
		var moved bool
		if err := s.database.Pool.QueryRow(ctx, decisionStepOwedMoved, s.countedThrough).Scan(&moved); err != nil {
			return fmt.Errorf("read whether an event since the decisions step's count can have moved it: %w", err)
		}
		if !moved {
			s.countedThrough = max(s.countedThrough, floor)
			return nil
		}
	}
	owed, err := countDecisionStepOwed(ctx, s.database.Pool)
	if err != nil {
		return err
	}
	s.owed.Store(&owed)
	s.logger.Info("dispatch: decisions step counted what it owes",
		"owed", owed.Total, "decisions_documents", owed.DecisionsDocuments,
		"answers_left_spec", owed.AnswersLeftSpec, "gated_steps_lists", owed.GatedStepsLists,
		"commit", s.commit)
	s.recount, s.countedThrough, s.countedIndexOwed = other, floor, indexOwed
	return nil
}

// backfill runs phase 1 and logs what each of its passes stated. A row its object pass leaves
// because the store holds the row's node in another project (graphstore.ErrProjectMoved) is one
// no run cures, so it fails neither phase 1 nor the boot, and phase 1 owes nothing for it
// (graphcensus.AsksOwed): it is logged for a person, and the project census names the row.
// kept for one release: decisions-step
func (s *DecisionStep) backfill(ctx context.Context) error {
	done, err := graphcensus.BackfillAsks(ctx, s.database)
	for _, kind := range done.Kinds {
		if kind.Rows > 0 {
			s.logger.Info("dispatch: decisions step stated rows written before this release on the store",
				"kind", kind.Kind, "rows", kind.Rows, "tuples", kind.Tuples, "remaining", kind.Remaining)
		}
	}
	if done.Moved != nil {
		s.logger.Error("dispatch: decisions step left rows whose node the store holds in another project", "error", done.Moved)
	}
	if err != nil {
		return fmt.Errorf("decisions step, the backfill: %w", err)
	}
	return nil
}

// anotherBuildLive reports whether processes.Live, read through q, lists a process serving a
// commit other than this process's own. An empty commit is one build with every other empty
// commit.
// kept for one release: decisions-step
func (s *DecisionStep) anotherBuildLive(ctx context.Context, q processes.Querier) (bool, error) {
	live, err := processes.Live(ctx, q)
	if err != nil {
		return false, err
	}
	for _, process := range live {
		if process.Commit != s.commit {
			return true, nil
		}
	}
	return false, nil
}

// decisionStepMarksKept bounds the marks of the event log the catch-up and the count keep
// (writerFloor).
// kept for one release: decisions-step
const decisionStepMarksKept = 8

// writerFloor marks the event log now and answers the highest id at or below which no event can
// still be open (events.Horizon.WriterFloor), read from the marks the catch-up's passes took,
// newest first, or, where none of them counts, from the horizon some process persisted, and the
// newest id the new mark saw. A mark counts once every transaction that holds a write lock on
// events took its id after it: this pass's own does on a log no write holds open, and under writes
// that overlap without a break an earlier pass's does, as soon as the writes open when it was
// taken have ended. It waits on no transaction.
// kept for one release: decisions-step
func (s *DecisionStep) writerFloor(ctx context.Context) (int64, int64, error) {
	mark, err := s.horizon.Mark(ctx)
	if err != nil {
		return 0, 0, err
	}
	s.marks = slices.Insert(s.marks, 0, mark)
	if len(s.marks) > decisionStepMarksKept {
		s.marks = s.marks[:decisionStepMarksKept]
	}
	floor, ok, err := s.horizon.WriterFloor(ctx, s.marks...)
	if err != nil || ok {
		return floor, mark.Newest, err
	}
	persisted, err := s.horizon.Persisted(ctx)
	return max(floor, persisted), mark.Newest, err
}
