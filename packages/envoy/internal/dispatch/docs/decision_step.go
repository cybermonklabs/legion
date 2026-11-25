package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
//     any other build can write a row without its node, and it needs the document service
//     (UseDocuments), which writes the documents.
//
// It runs at boot, before the server serves, where only phase 1 runs and a failure stops the boot
// (Boot), as FillDocumentSearch does: the readers of this release that take an ask's answer from
// its node - a reading's check of its ask (askAnswers) and this step's own count - read an ask
// phase 1 has not stated yet as open, and the release before keeps serving through a roll until
// this process serves. The boot reads only what phase 1 needs - whether a row stands unmarked
// (graphcensus.AsksOwed), and the rows the backfill states - and nothing else: a table no phase 1
// statement reads, held by another transaction, must not keep the process from serving. It then
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
// and whether any document's block index is owed, each one probe of an index. graphcensus.AsksOwed
// reads its kinds' tables to the end when nothing is owed, so it runs at boot, on a tick that owes
// a catch-up, and after a tick that saw another build live, until it finds nothing: this build's
// own create sites write their nodes and markers, so only another build writes a row it finds. The
// count of what the restore and the conversion owe reads whole tables too, so where it read zero it
// is read again only when something can have made it non-zero (recountOwed).
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
	// backfillUnknown is whether a tick reads graphcensus.AsksOwed even where it owes no
	// catch-up: true until a read of it finds nothing owed with no other build live, and again
	// once a tick sees another build live.
	backfillUnknown bool
	// recount is whether the next tick counts what the restore and the conversion owe whatever
	// the event log says (recountOwed): true until the first count, after a tick that saw another
	// build live, and after a tick that ran a phase, which states facts under events already
	// appended. countedThrough is the writer floor of the event log read before the last count,
	// and countedIndexOwed whether a document's block index was owed when it ran.
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
// these, which stops the boot. It reads nothing else - not whether another build is live, which
// phase 1 does not wait on, and not what the restore and the conversion owe, which the process's
// first tick counts (probe) - so the boot waits on no table phase 1 does not read. Its context is
// marked for the pool's nested-acquire guard (store.WithTransactionTracking), as every entry point
// of the pool's transactions marks its own.
// kept for one release: decisions-step
func (s *DecisionStep) Boot(ctx context.Context) error {
	ctx = store.WithTransactionTracking(ctx)
	owed, err := graphcensus.AsksOwed(ctx, s.database.Pool)
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
	if owed, err = graphcensus.AsksOwed(ctx, s.database.Pool); err != nil || !owed {
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
	if _, failures, err := s.RestoreAndConvert(ctx, "", true, false); err != nil {
		s.logger.Error("dispatch: decisions step restore and conversion; the next tick tries again", "error", err)
	} else {
		for _, failure := range failures {
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
	if work.other, err = s.anotherBuildLive(ctx); err != nil {
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
		if work.backfill, err = graphcensus.AsksOwed(ctx, s.database.Pool); err != nil {
			return decisionStepWork{}, err
		}
		s.backfillUnknown = work.backfill || work.other
	}
	return work, nil
}

// recountOwed counts what the restore and the conversion owe (countDecisionStepOwed), which reads
// whole tables, where the last count read something owed, and where it read nothing only when
// something can have made it non-zero since: another build live (recount), a phase of this step
// run (recount), or a write of this build or another, each of which appends an event - a document
// stored or renamed with the role or slug of a decisions document, a spec's version that drops an
// answered ask's block, a project's list of gated steps - so the event log holds an event above the
// writer floor read before the last count (countedThrough). A document's block index the count
// waits for (a spec whose index is owed is left out) moves with no event, so the count is read
// again while any is owed, and once more after. Otherwise it reads the newest event and whether a
// block index is owed, each one probe of an index, and leaves the last count standing. The floor is
// read before the count, so an event the count did not see is above it; an event log reset (newest
// below the floor) is a change too.
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
	if last := s.owed.Load(); last != nil && last.Total == 0 && !s.recount &&
		!indexOwed && !s.countedIndexOwed && newest == s.countedThrough {
		return nil
	}
	floor, _, err := s.writerFloor(ctx)
	if err != nil {
		return fmt.Errorf("read which events the decisions step's count reads final: %w", err)
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

// backfill runs phase 1 and logs what each of its passes stated.
// kept for one release: decisions-step
func (s *DecisionStep) backfill(ctx context.Context) error {
	done, err := graphcensus.BackfillAsks(ctx, s.database)
	for _, kind := range done {
		if kind.Rows > 0 {
			s.logger.Info("dispatch: decisions step stated rows written before this release on the store",
				"kind", kind.Kind, "rows", kind.Rows, "tuples", kind.Tuples, "remaining", kind.Remaining)
		}
	}
	if err != nil {
		return fmt.Errorf("decisions step, the backfill: %w", err)
	}
	return nil
}

// anotherBuildLive reports whether processes.Live lists a process serving a commit other than
// this process's own. An empty commit is one build with every other empty commit.
// kept for one release: decisions-step
func (s *DecisionStep) anotherBuildLive(ctx context.Context) (bool, error) {
	live, err := processes.Live(ctx, s.database.Pool)
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

// caughtUpAsk is the ask an event of the catch-up's read names, aliased e: a follower event names
// it as ask_id, every other as the ask's own id.
// kept for one release: catch-up
const caughtUpAsk = `case when e.type like 'ask.follower_%' then e.payload->>'ask_id' else e.payload->>'id' end`

// caughtUpTypes is the type list of every read of the catch-up, written out so the planner can
// match it against a partial index's own: an answer, a resolution (a decline is one), an opening
// (a reopen, or settlement's restoration), a follower added or removed.
// kept for one release: catch-up
const caughtUpTypes = `('ask.answered', 'ask.resolved', 'ask.opened', 'ask.follower_added', 'ask.follower_removed')`

// caughtUpColumns is the event's own columns each row of a catch-up read starts with, in the order
// scanCaughtUpEvent takes them; the ask it names and its owner follow.
// kept for one release: catch-up
const caughtUpColumns = `e.id, e.type, e.payload, e.actor, e.created_at`

// caughtUpRead is the catch-up's one read of a pass: every event of the window above $1 of
// caughtUpTypes that no tuple of its ask's node states, since none carries the event's id as its
// source_event, and whose id is none of $2, which an earlier pass read already. Each row is the
// event, the ask it names and the event's owner, with the owner's last_seq as the read's snapshot
// holds it: every event of that owner the read could see stands at or below it, which is where
// catchUpAsk reads on from once it holds the ask (caughtUpSince). The rows come in the order of
// the asks and, within an ask, of the events' ids. It reads events through their primary key from
// $1 on, so a pass reads what was appended since the passes before it and not the whole window
// again, each event's tuples through an index keyed on its ask's node, and each owner's row by its
// key (TestTheCatchUpReadsItsWindowThroughIndexes). The tuples are looked for by a scalar
// subquery, `(select … limit 1) is null`, rather than by NOT EXISTS, which Postgres plans as an
// anti-join over every tuple of every ask, read once and kept beside the events (copied_asks.go's
// owedCopiesQuery meets the same with a hashed subplan); the subquery runs once for each event,
// each run one probe of the index.
// kept for one release: catch-up
const caughtUpRead = `
	select ` + caughtUpColumns + `, coalesce(` + caughtUpAsk + `, '') as ask,
	       coalesce(e.issue_key, ''), coalesce(e.artifact_id::text, ''),
	       coalesce((select i.last_seq from issues i where i.key = e.issue_key),
	                (select d.last_seq from artifacts d where d.id = e.artifact_id), 0)
	from events e
	where e.id > $1
	  and e.type in ` + caughtUpTypes + `
	  and e.id <> all($2::bigint[])
	  and (select 1 from graph_tuples t
	       where t.from_kind = 'ask' and t.from_id = ` + caughtUpAsk + ` and t.source_event = e.id
	       limit 1) is null
	order by ask, e.id`

// caughtUpSince is the read catchUpAsk makes once it holds an ask's owner row and the ask's row:
// the events of caughtUpTypes of the ask $3 that its owner, keyed by owner ($1), appended after
// the sequence $2 the pass's read saw it at, which no tuple of the ask's node states, in the order
// of their ids. The owner's row is held, so they are every such event a write committed between
// the pass's read and the lock, and no later one can commit while the transaction stands. It reads
// the owner's events through the owner's unique (key, seq) index from $2 on, so it reads only
// what the owner appended in that gap, follower events among them, which name their ask as
// ask_id where events_ask_payload_id keys only on id.
// kept for one release: catch-up
func caughtUpSince(owner string) string {
	return `
	select ` + caughtUpColumns + `, $3::text, coalesce(e.issue_key, ''), coalesce(e.artifact_id::text, ''), e.seq
	from events e
	where ` + owner + ` and e.seq > $2
	  and e.type in ` + caughtUpTypes + `
	  and ` + caughtUpAsk + ` = $3
	  and (select 1 from graph_tuples t
	       where t.from_kind = 'ask' and t.from_id = $3 and t.source_event = e.id
	       limit 1) is null
	order by e.id`
}

// caughtUpSinceOnIssue and caughtUpSinceOnDocument are caughtUpSince of an ask an issue owns, read
// through events_issue_key_seq_key, and of one a document no issue owns, read through
// events_artifact_id_seq_key.
// kept for one release: catch-up
var (
	caughtUpSinceOnIssue    = caughtUpSince(`e.issue_key = $1`)
	caughtUpSinceOnDocument = caughtUpSince(`e.artifact_id = $1::uuid`)
)

// scanCaughtUpEvent reads one row of caughtUpRead or caughtUpSince.
// kept for one release: catch-up
func scanCaughtUpEvent(row pgx.CollectableRow) (caughtUpEvent, error) {
	var event caughtUpEvent
	var actor []byte
	if err := row.Scan(&event.id, &event.eventType, &event.payload, &actor, &event.at, &event.ask,
		&event.issue, &event.document, &event.ownerSeq); err != nil {
		return event, err
	}
	if err := json.Unmarshal(actor, &event.actor); err != nil {
		return event, fmt.Errorf("read the actor of event %d: %w", event.id, err)
	}
	return event, nil
}

// decisionStepMarksKept bounds the marks of the event log the catch-up and the count keep
// (writerFloor).
// kept for one release: decisions-step
const decisionStepMarksKept = 8

// catchUp is phase 2, and reports whether an ask failed. Its window opens at the newest
// server_build_announcements row naming a commit other than this process's own: the fleet last
// stood whole on another build then, and every process of this build started after it. Where none
// names another commit, as on a database this build created, it opens at the first event. A pass
// reads the window's events no tuple states once (caughtUpRead), from the step's high-water mark
// on, and catches each ask up in a transaction of its own (catchUpAsk); a failure on one is logged
// and leaves the others to go on.
//
// An event that needs nothing - one older than the node's newest tuple of its attribute, a follow
// the backfill already stated at the sentinel, an event of an ask the backfill has not marked yet -
// states nothing, so no tuple ever names it and every pass would read it again. The step keeps
// instead what its passes read. After a pass on which no ask failed, every event at or below the
// floor it read from (writerFloor) was final before it read, so it read each of them, and the
// high-water mark moves up to that floor; the ids it read above the floor are kept until the mark
// passes them. A pass on which an ask failed moves neither, and the next pass reads that ask's
// events again, with whatever else needed nothing. The mark is the process's own: a process that
// takes the step over reads its window again from where it opens, and an event already stated is
// left out of its read as it is of every read.
// kept for one release: catch-up
func (s *DecisionStep) catchUp(ctx context.Context) (bool, error) {
	var start int64
	if err := s.database.Pool.QueryRow(ctx, `
		select coalesce((
			select e.id from events e
			where e.created_at < (select a.announced_at from server_build_announcements a
			                      where a.commit <> $1 order by a.seq desc limit 1)
			order by e.id desc limit 1), 0)
	`, s.commit).Scan(&start); err != nil {
		return false, fmt.Errorf("read where the catch-up's window opens: %w", err)
	}
	floor, newest, err := s.writerFloor(ctx)
	if err != nil {
		return false, fmt.Errorf("read which events the catch-up reads final: %w", err)
	}
	if newest < s.caughtUpThrough {
		// The log holds no event as far as the mark (TRUNCATE ... RESTART IDENTITY): its ids are
		// a new log's, and nothing the mark says holds of them.
		s.caughtUpThrough, s.caughtUpAbove = 0, nil
	}
	read := s.caughtUpAbove
	if read == nil {
		// A nil list is bound as null, and no id is unequal to every member of null.
		read = []int64{}
	}
	rows, err := s.database.Pool.Query(ctx, caughtUpRead, max(start, s.caughtUpThrough), read)
	if err != nil {
		return false, fmt.Errorf("read the events no tuple states: %w", err)
	}
	unstated, err := pgx.CollectRows(rows, scanCaughtUpEvent)
	if err != nil {
		return false, fmt.Errorf("read the events no tuple states: %w", err)
	}
	failed := false
	// What the asks' own reads under their locks found beside what the pass read, which the
	// high-water mark takes in as read too.
	var readUnderLocks []caughtUpEvent
	for first := 0; first < len(unstated); {
		last := first + 1
		for last < len(unstated) && unstated[last].ask == unstated[first].ask {
			last++
		}
		askID := unstated[first].ask
		applied, later, err := s.catchUpAsk(ctx, askID, unstated[first:last])
		first = last
		if err != nil {
			failed = true
			s.logger.Error("dispatch: decisions step could not catch up an ask; the next tick tries again",
				"ask", askID, "error", err)
			continue
		}
		readUnderLocks = append(readUnderLocks, later...)
		if applied > 0 {
			s.logger.Info("dispatch: decisions step caught up the events another build wrote", "ask", askID, "events", applied)
		}
	}
	if !failed {
		s.keepCaughtUp(floor, append(unstated, readUnderLocks...))
	}
	return failed, nil
}

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

// keepCaughtUp moves the high-water mark over what a pass on which no ask failed read: every event
// at or below floor, and the ids of what it read above it, beside those an earlier pass read above
// the mark that it has not passed yet.
// kept for one release: catch-up
func (s *DecisionStep) keepCaughtUp(floor int64, read []caughtUpEvent) {
	through := max(s.caughtUpThrough, floor)
	above := s.caughtUpAbove[:0]
	for _, id := range s.caughtUpAbove {
		if id > through {
			above = append(above, id)
		}
	}
	for _, event := range read {
		if event.id > through {
			above = append(above, event.id)
		}
	}
	s.caughtUpThrough, s.caughtUpAbove = through, above
}

// caughtUpEvent is one event a catch-up read (caughtUpRead, caughtUpSince): the event, the ask it
// names, and its owner's key - its issue, or the document it belongs to - with ownerSeq, for a row
// of caughtUpRead the owner's last_seq as that read saw it.
// kept for one release: catch-up
type caughtUpEvent struct {
	id              int64
	eventType       string
	payload         []byte
	actor           model.Actor
	at              time.Time
	ask             string
	issue, document string
	ownerSeq        int64
}

// catchUpAsk applies, in one transaction, the events of askID a pass read (unstated), and every
// later one, in the order of their ids. It takes the store's lock order (api/inbox_read.go's
// markInboxReadThreads), as every writer of an ask's answer does: the ask's owner row first - its
// issue, or the document an ask on no issue belongs to, read off the row without a lock and taken
// through events.LockOwnerRows - then the ask's row, `for no key update`, and only then the nodes
// and the tuples asks.Settle and asks.CatchUpFollower write. An ask whose owner moved between the
// two is tried again at the next pass. An ask whose row is gone, or names no node, is the
// backfill's: the row is stated whole when it is marked.
//
// The pass read its events before it took any lock, so a write of another build can commit on the
// ask in between: the write holds the owner row, which the lock waits for, and its event is newer
// than any the pass read. So under its locks catchUpAsk reads the ask's events its owner appended
// since the pass's read (caughtUpSince) and applies them after the ones read. Without them the row
// Settle writes would be the one the older event records, written over the newer write, and every
// reader of the row would see the older state until the next pass, which would then skip the newer
// event as older than the node's answer_state. It answers the events it read under its locks too,
// which the pass's high-water mark takes in.
//
// An answer event older than the node's newest answer_state, and a follower event older than the
// node's newest followed_by to that session, is skipped, so an old event caught up after a newer
// write never puts back what that write replaced. Every other answer event is written as
// asks.Settle writes the event's own state, answer and resolution, under the event, by its actor at
// its time; a follower event is stated as the row stands (asks.CatchUpFollower). It answers how
// many events it applied.
// kept for one release: catch-up
func (s *DecisionStep) catchUpAsk(ctx context.Context, askID string, unstated []caughtUpEvent) (int, []caughtUpEvent, error) {
	if askID == "" {
		return 0, nil, nil
	}
	var issue, document string
	err := s.database.Pool.QueryRow(ctx, `
		select coalesce(issue_key, ''), coalesce(artifact_id::text, '') from asks where id = $1::uuid
	`, askID).Scan(&issue, &document)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("read the owner of ask %s: %w", askID, err)
	}
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)
	owners := events.OwnerRows{}
	owners.Add(issue, document)
	if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
		return 0, nil, err
	}
	var marked bool
	var heldIssue, heldDocument string
	err = tx.QueryRow(ctx, `
		select node_kind is not null, coalesce(issue_key, ''), coalesce(artifact_id::text, '')
		from asks where id = $1::uuid for no key update
	`, askID).Scan(&marked, &heldIssue, &heldDocument)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !marked) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("lock ask %s: %w", askID, err)
	}
	if heldIssue != issue || heldDocument != document {
		return 0, nil, fmt.Errorf("ask %s moved to another owner while its catch-up took the owner's row", askID)
	}
	var later []caughtUpEvent
	if read := unstated[0]; read.issue != "" || read.document != "" {
		since, owner := caughtUpSinceOnIssue, read.issue
		if read.issue == "" {
			since, owner = caughtUpSinceOnDocument, read.document
		}
		rows, err := tx.Query(ctx, since, owner, read.ownerSeq, askID)
		if err != nil {
			return 0, nil, fmt.Errorf("read the events of ask %s appended since the pass read it: %w", askID, err)
		}
		if later, err = pgx.CollectRows(rows, scanCaughtUpEvent); err != nil {
			return 0, nil, fmt.Errorf("read the events of ask %s appended since the pass read it: %w", askID, err)
		}
		// Each is newer than every event the pass read: the owner's events commit in the order of
		// their sequences, which is the order of their ids.
		unstated = append(slices.Clip(unstated), later...)
	}
	applied := 0
	for _, event := range unstated {
		done, err := applyCaughtUpEvent(ctx, tx, askID, event)
		if err != nil {
			return 0, nil, fmt.Errorf("apply event %d (%s): %w", event.id, event.eventType, err)
		}
		if done {
			applied++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, fmt.Errorf("commit the catch-up of ask %s: %w", askID, err)
	}
	return applied, later, nil
}

// applyCaughtUpEvent applies one event a pass read, in catchUpAsk's transaction, and reports
// whether it stated anything.
// kept for one release: catch-up
func applyCaughtUpEvent(ctx context.Context, tx pgx.Tx, askID string, event caughtUpEvent) (bool, error) {
	if event.eventType == "ask.follower_added" || event.eventType == "ask.follower_removed" {
		var payload model.AskFollowerEventPayload
		if err := json.Unmarshal(event.payload, &payload); err != nil {
			return false, err
		}
		newest, err := newestSource(ctx, tx, `t.kind = $2 and t.to_kind = $3 and t.to_id = $4`,
			askID, graphstore.TupleFollowedBy, graphstore.KindSession, payload.SessionID)
		if err != nil || event.id < newest {
			return false, err
		}
		return asks.CatchUpFollower(ctx, tx, askID, payload.SessionID, event.eventType == "ask.follower_added", event.actor, event.at, event.id)
	}
	var recorded model.Ask
	if err := json.Unmarshal(event.payload, &recorded); err != nil {
		return false, err
	}
	switch recorded.State {
	case asks.StateOpen, asks.StateAnswered, asks.StateResolved:
	default:
		return false, fmt.Errorf("the event records the answer state %q", recorded.State)
	}
	newest, err := newestSource(ctx, tx, `t.kind = $2 and t.to_kind = $3`, askID, graphstore.TupleAnswerState, graphstore.KindValue)
	if err != nil || event.id < newest {
		return false, err
	}
	change := asks.Recorded(recorded.State, recorded.Answer, recorded.Resolution)
	change.Who, change.At, change.Event = event.actor, event.at, event.id
	if _, err := asks.Settle(ctx, tx, askID, change); err != nil {
		return false, err
	}
	return true, nil
}

// newestSource is the source_event of the newest tuple out of askID's node that narrow keeps, 0
// where there is none.
// kept for one release: catch-up
func newestSource(ctx context.Context, tx pgx.Tx, narrow string, askID string, args ...any) (int64, error) {
	var newest int64
	if err := tx.QueryRow(ctx, `
		select coalesce((select t.source_event from graph_tuples t
		                 where t.from_kind = 'ask' and t.from_id = $1 and `+narrow+`
		                 order by t.seq desc limit 1), 0)
	`, append([]any{askID}, args...)...).Scan(&newest); err != nil {
		return 0, fmt.Errorf("read the newest tuple of ask %s: %w", askID, err)
	}
	return newest, nil
}
