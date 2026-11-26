package evals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The day-row refresher (dispatch://INFRA-2916, R10, R12, R15 and R16) keeps every project's
// computed day rows true to their inputs: the delivery and CI rows (delivery_days.go), the Legion
// rows (legion_days.go) and the unowned-p0 row of today, and the explicit zero rows of every series
// from its first input day on, the logged evals' included. It is one woken loop under one lease
// (RefresherLease), with a cursor of its own in each of the two feeds it reads (eval_days_cursor,
// migration 20261117100021):
//
//   - delivery_changes, woken by delivery.ChangesChannel. Each change names the days it touches
//     (refresher_work.go): a pull request's merge day, the start day of the deploy run that ships
//     it now and of the one that shipped it before, and the start day of each of its PR-checks
//     runs, which count for its project; a run's start day and the merge days of the pull requests
//     it ships now and shipped before; and, through eval_day_members, the day
//     each of those items was counted on before, so an item whose day or project moved rewrites
//     the day it left as well as the day it joined. A stored Legion fact's change and a filing's
//     name their days themselves: an issue's close day before and after the change, a filing's
//     day before and after. A change of the delivery settings rewrites, in one transaction, every
//     day of the delivery and CI evals it moves (settings_rewrite.go), and begins no walk; a move
//     of the deploy repository moves none, since the evals that read it are kept for every
//     repository that holds a delivery run and a read chooses among them (deploy_repos.go). A page
//     whose changes move a series' first input day, or the repositories that hold a delivery run,
//     writes the days it names and every other day those moves reach in its one commit, and
//     moves no walk (span.go).
//   - the event log up to its safe horizon, woken by the events channel: an issue event that can
//     move the count of open P0 issues with no owner rewrites that project's unowned-p0 row of
//     today, and one that moves an issue's repository or parent rewrites the days of the filings of
//     that issue and every issue below it, whose repository it moves.
//
// Beside the feeds, the passes make the walks (eval_days_walks): one over every day of the
// delivery and CI rows, one per project over its Legion days, one per project filling its logged
// evals' zero rows, and one per project writing its unowned-p0 row of today, which reads issues
// alone and so is written whether or not delivery settings exist. A walk builds every day from the
// first input day of what it walks to today, a batch of days each RefresherGap (walkBatch), then
// the day of each new UTC day as it comes, which is what writes the zero rows of a day nothing
// happened on. There is one walk of each family and project, whichever build's process moves it.
// A family's first walk, over a database or a project no rows are served of, builds its days in
// the open, a day a commit, and its serving record (eval_days_serving) follows it. Where rows are
// served, nothing that changed no input begins a walk again: a release (a process whose build is
// not the one that serves the rows), a lost change cursor and delivery settings the rows stand
// built under that are not recorded each rebuild the served families aside, in short transactions,
// and make what moved visible in one short commit, so a rebuild whose rules move nothing writes
// nothing (rebuild.go). While two builds roll, the release's rebuild flips once.
//
// The stored rows are a function of the inputs and the settings alone (dispatch://INFRA-2916, item
// 8): two databases loaded with the same inputs and settings hold the same rows, whichever order
// the inputs arrived in. A day written holds exactly the rows its project's family builds of it,
// upserted where a value moved (upsertDays), the rows the day no longer builds deleted
// (deleteStaleDays), which the eval_days trigger logs, so a view of them wakes; and a zero row of
// every series whose first input day is on or before it and that the day has no input of, written
// on a day the stored rows are whole through (eval_days_serving, servingNext), whatever walk or
// rebuild runs. A series' first input day is the first day the walk built holding input of it: the
// builders write a row of a series only on a day with its input, the walk writes a zero row only
// on a day at or after that first day, and it takes each series' first day from the days it built
// (refresh.firstOf), so a missing row is a day before it, or a day no walk has computed yet, which
// reads "no data" until a walk computes it (item 5). A day a change names is written from its own
// input; every day after a first input day it moves earlier, or from one that lost its input, and
// every day of a repository whose first runs arrived, is written with it, in the page's one
// commit, as a whole walk builds it (span.go).
//
// A pass that fails logs and leaves its cursors where they were, so the next pass reads the same
// changes again; every write is safe to make twice.

// RefresherLease names the lease every refresher pass takes (duty.Exclusive with no period, which
// is mutual exclusion and nothing else): two processes are woken by one commit, and two passes at
// once would build the same days twice and walk the same days twice.
const RefresherLease = "eval-days"

// RefresherGap is the least time between the starts of two passes that begin a batch of the walks
// (walkBatch, gapBatch), whose part of a pass is the background work no change asks for: the
// walks' days, the daily tick, the fold of the walked days' figures, the figures walk and the
// prune of the change log. It is also how long a process that found the lease held waits, plus up
// to a tenth again, before it tries once more (duty.WokenLoop.Retry), how far on a pass that
// failed asks for the next, and the longest a pass whose safe horizon held an event back waits for
// the next (horizonRetry).
//
// No gap holds a pass a change woke: the loop's MinGap is zero, so at most one pass runs and at
// most one is owed, a change that commits while a pass runs earns the one pass after it, which
// starts as soon as the running one ends, and any number of changes during a pass earn that one
// pass. A pass that begins no batch and continues none does the changes' work alone - its pages,
// the days its writes left owed, the figures of the change log and the refold of the cuts a move
// of a default repository marked stale - and asks for a pass at the gap's end, which begins the
// next batch where the walks have days left or a page began one again. A computed eval's view
// wakes once the refresher has rewritten its rows (R10), so a change reaches the view one running
// pass and its own pass after its commit, the 2 s R14 allows.
//
// No gap paces the refold of a stale cut either: every pass, with a batch of the walks or none,
// folds each stale cut again batch after batch until its mark is cleared (refoldStaleCuts), and its
// batches are none of the gap's. The refold runs after the pass's pages, and, in a pass that builds
// the gap's batch of the walks, after that batch's walks and the figures walk's batch too
// (refresh): the pass that publishes a move begins the refold of the cuts it marks once that work
// is done.
//
// A batch's walks stop between two days when a wake is owed (duty.WakeOwed), once the pass has
// built a day, so a change waits behind one day's build rather than a whole batch. The owed pass,
// which starts at once, does its changes' work first and then builds the days the batch has left
// (gapBatch), so the walks still build up to a whole batch per gap, whether the wake was a
// change's or the echo of the refresher's own writes, which notify the change log's channel as
// every eval write does. The figures walk, whose batch of each project is one transaction, stops
// between two projects, and the refold of the stale cuts between two of its batches, each one
// transaction, where the owed pass goes on with it.
const RefresherGap = 10 * time.Second

// horizonRetry is how far on a pass whose safe horizon held an event back asks for the next, the
// first time in a row: each such pass after it doubles the wait, up to RefresherGap (refresh).
const horizonRetry = 250 * time.Millisecond

// refresherPage is the most rows one page of a pass reads of each feed. A pass whose page was full
// reads the next one at once.
const refresherPage = 1000

// walkBatch is how many days of each walk the passes build each RefresherGap. A walk of INFRA's
// history (a year of days, each one read of the day's delivery rows and one write per project) is
// many batches, one begun each RefresherGap, so no pass holds the lease for the whole of it and the
// rebuild's load on the database is spread over it, and the changes that arrive meanwhile are
// answered between two of its days (RefresherGap). Every day is a transaction of its own per
// project (walkWrite, writeProjectDay), or, in a rebuild, a batch is (stageWalk).
const walkBatch = 28

// The cursors, one row of eval_days_cursor each, named for the feed they are read by (feedCursors).
const (
	changesCursor = "delivery_changes"
	eventsCursor  = "events"
)

// refresherCursors is the two cursors in one list, which is how a pass reads them.
var refresherCursors = []string{changesCursor, eventsCursor}

// The walks of eval_days_walks, by family: the delivery and CI rows of every project (one walk,
// whose project is the empty string), the Legion rows of one project, the zero rows of one
// project's logged evals, one project's unowned-p0 row of today, and one project's stored figures,
// a walk of their ranges' end days (walkFigures).
const (
	walkDelivery = "delivery"
	walkLegion   = "legion"
	walkLogged   = "logged"
	walkUnowned  = "unowned"
	walkFigures  = "figures"
)

// RefresherDeps is what RunRefresher needs of the server.
type RefresherDeps struct {
	// Horizon is the process's one events.Horizon: the event log is paged by a global id, which
	// is not commit order, so a page stops at its safe horizon.
	Horizon *events.Horizon
	// Build names the build this process is, which every row the refresher writes records
	// (built_by), and which a release's rebuild compares with the build that serves the rows
	// (eval_days_serving): cmd/dispatch's processBuild, the commit the image build stamped, as the
	// Legion facts consumer reads it (legionfacts.Deps.Build). The empty string is no build, and
	// RunRefresher refuses it: rows served under it would be found its own by the next process
	// that carried none.
	Build string
	// Leases is the pool a pass takes its lease connection from (duty.Exclusive).
	Leases duty.Leases
	// Events is signalled when an event commits: in the server the pglisten.Hub wake on
	// api.EventsChannel.
	Events <-chan struct{}
	// Deliveries is signalled when a delivery change commits: the hub's wake on
	// delivery.ChangesChannel.
	Deliveries <-chan struct{}
}

// refresherSchedule is the loop's pace and the clock a pass builds as of, which a test replaces:
// gap is RefresherGap, page refresherPage, batch walkBatch, refoldBudget refoldBatchBudget, and now
// answers the moment whose UTC day is today. refoldBudget is about how long each batch of a stale
// cut's refold after the pass's first takes (refoldBatchDays); none, which a test's schedule
// leaves, bounds no batch by time, and each such batch folds batch end days.
type refresherSchedule struct {
	gap          time.Duration
	page         int
	batch        int
	refoldBudget time.Duration
	now          func() time.Time
}

// serverRefresherSchedule is the pace the server runs the loop at, which holds each batch of a
// stale cut's refold to about 250 ms (refoldBatchBudget).
var serverRefresherSchedule = refresherSchedule{
	gap:          RefresherGap,
	page:         refresherPage,
	batch:        walkBatch,
	refoldBudget: refoldBatchBudget,
	now:          func() time.Time { return time.Now().UTC() },
}

// RunRefresher keeps every project's day rows true to their inputs (the refresher's rules above)
// until ctx is done: one pass at start; a pass on each wake, which starts as soon as the pass
// running when it came ends; a pass RefresherGap after the last pass that began a batch of the
// walks, where the walks had work left after it or its refold of the cuts a default move marked
// stale stopped for an owed wake (refreshed.walksLeft), or a pass since then began none
// (RefresherGap); a pass horizonRetry after one that left an event above the safe horizon it
// read, the wait doubling with each such pass in a row up to RefresherGap; a pass RefresherGap
// after one that failed; and a pass at the earliest of the next UTC midnight, which begins a day
// every walk builds, and pruneSlack after the oldest delivery change passes
// delivery.ChangeRetention, which the pass then prunes (delivery.PruneChanges), so the change log
// keeps its retention where no GitHub App runs the reconcile that prunes it too. Every pass folds
// again each stale cut to its end (refoldStaleCuts), so the pass a move's wake runs begins the
// refold of the cuts the move marks, and one that stops for an owed wake is followed at once by
// the owed pass, which goes on with it.
func (s *Store) RunRefresher(ctx context.Context, deps RefresherDeps) {
	s.runRefresher(ctx, deps, serverRefresherSchedule)
}

func (s *Store) runRefresher(ctx context.Context, deps RefresherDeps, sched refresherSchedule) {
	if deps.Build == "" {
		panic("evals: RefresherDeps.Build is empty, and the refresher needs the build this process is")
	}
	ctx = store.WithTransactionTracking(ctx)
	seeds := &seeding{}
	// The loop's goroutine alone reads and writes these. began is when the last pass that began a
	// batch of the walks started; batch is that batch while it has days a pass left for the next
	// (gapBatch), nil once a pass built them; held is the retry of a pass whose safe horizon held
	// an event back. Their times are the wall clock's, which the loop's timer runs on, not
	// sched.now's, which a test stands still.
	var (
		began time.Time
		batch *gapBatch
		held  = duty.Backoff{First: horizonRetry, Max: sched.gap}
	)
	duty.RunWoken(ctx, duty.WokenLoop{
		Lease:  RefresherLease,
		Leases: deps.Leases,
		Wake:   duty.MergeWakes(ctx, deps.Events, deps.Deliveries),
		Retry:  sched.gap,
		Pass: func(ctx context.Context) time.Time {
			started, asOf := time.Now(), sched.now()
			begun := !started.Before(began.Add(sched.gap))
			if begun {
				began, batch = started, &gapBatch{}
			}
			walking := batch != nil
			report, next, err := s.refresh(ctx, deps, sched, seeds, batch)
			if ctx.Err() != nil {
				return time.Time{}
			}
			if len(report.days) > 0 || len(report.seeded) > 0 {
				slog.Debug("dispatch: eval day rows refreshed", "days", len(report.days), "projects_seeded", len(report.seeded))
			}
			if err != nil {
				slog.Warn("dispatch: the eval day-row refresher's pass failed", "error", err)
				return time.Now().Add(sched.gap)
			}
			report.logSlow(begun, walking)
			// A due time the pass read off sched.now stands as far ahead on the wall clock.
			if !next.IsZero() {
				next = started.Add(next.Sub(asOf))
			}
			if walking && !report.yielded {
				batch = nil
			}
			if !walking || report.walksLeft {
				next = earlier(next, began.Add(sched.gap))
			}
			if report.heldBack {
				next = earlier(next, time.Now().Add(held.Next()))
			} else {
				held.Reset()
			}
			return next
		},
	})
}

// earlier is the earlier of a and b, where the zero time is no time at all.
func earlier(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

// gapBatch is the walks' batch of one RefresherGap: how many of the batch days the gap gives each
// walk (refresherSchedule.batch) it has built, by walk, and of each project's figures walk how many
// end days. A pass whose walks stopped for an owed wake (refresh.yields) leaves the rest to the
// next pass, which the loop starts at once and which builds them after its changes' work, so each
// walk builds up to a whole batch per gap however many passes the wakes cut it into. The loop keeps
// one across its passes and begins a new one each RefresherGap; RefreshThrough and a test's pass
// give each pass a new one, which builds a whole batch. The refold of a stale cut takes none of it
// (refoldStaleCuts).
type gapBatch struct {
	built map[walkKey]int
}

// through is the last day of the days the batch has left key's walk of size days to build from
// next, at most last, and false where it has none left.
func (b *gapBatch) through(key walkKey, size int, next, last time.Time) (time.Time, bool) {
	left := max(size, 1) - b.built[key]
	if left <= 0 {
		return time.Time{}, false
	}
	end := next.AddDate(0, 0, left-1)
	if end.After(last) {
		end = last
	}
	return end, true
}

// spend records that key's walk built n more days of the batch.
func (b *gapBatch) spend(key walkKey, n int) {
	if b.built == nil {
		b.built = map[walkKey]int{}
	}
	b.built[key] += n
}

// refreshThroughPasses bounds RefreshThrough: a pass builds walkBatch days of each walk, so this
// many cover a history far longer than any a caller loads.
const refreshThroughPasses = 100

// RefreshThrough runs the refresher's pass, the one RunRefresher's loop runs, as of now and under
// deps.Build (deps.Leases and the wakes unused), pass after pass, each with a batch of its own,
// until one leaves no walk with days left: every project's day rows then stand from each family's
// first input day through now's UTC day as the server's refresher leaves them, every project
// seeded. It takes no lease, so it is for a database no refresher of a running server writes: a
// caller that loads one and then reads it, such as the tests of the eval routes, which so read the
// rows production writes. The event log is read through everything committed before the call
// (events.Horizon.Await). Its schedule is the server's with no refold budget, so each batch of a
// stale cut's refold after a pass's first folds walkBatch end days however long it takes: the
// budget spares a readings write a long wait on a batch's locks (refoldBatchBudget), and no
// production code calls RefreshThrough, whose caller writes nothing while it runs.
func (s *Store) RefreshThrough(ctx context.Context, deps RefresherDeps, now time.Time) error {
	if deps.Build == "" {
		return errors.New("evals: RefresherDeps.Build is empty, and the refresher needs the build it writes as")
	}
	if _, err := deps.Horizon.Await(ctx); err != nil {
		return fmt.Errorf("await the event log's safe horizon: %w", err)
	}
	now = now.UTC()
	sched := serverRefresherSchedule
	sched.refoldBudget = 0
	sched.now = func() time.Time { return now }
	seeds := &seeding{}
	for range refreshThroughPasses {
		report, _, err := s.refresh(ctx, deps, sched, seeds, &gapBatch{})
		if err != nil {
			return err
		}
		if !report.walksLeft {
			return nil
		}
	}
	return fmt.Errorf("%d refresher passes as of %s left a walk with days still to build", refreshThroughPasses, now.Format(time.RFC3339))
}

// refreshed is what one pass wrote: each family's day of each project it built and wrote, which is
// what a check of which days a change touches reads, and each project whose seed it ran, in the
// order it ran them. The unowned-p0 row of today is recorded under its slug, and a logged eval's
// zero fill under walkLogged and the day it filled through. walksLeft is whether a walk of the day
// rows or a figures walk has days left after the pass's batch, the refold of a cut a default move
// marked stale stopped for an owed wake with a mark left (refoldStaleCuts), or the rebuild or its
// cleanup has work left (finishRebuild), yielded whether its walks or its refold stopped for an
// owed wake with work left (refresh.yields), heldBack whether its safe horizon held an event back
// (refresh), and parts how long each part of it took (passParts).
type refreshed struct {
	days      map[builtDay]bool
	seeded    []string
	walksLeft bool
	yielded   bool
	heldBack  bool
	parts     passParts
}

// slowPass is the shortest pass runRefresher logs the parts of (refreshed.logSlow): the pass a
// change waits for when it commits during one is part of the 2 s R14 gives the change
// (RefresherGap), and one of a tenth of a second or more is a twentieth of it.
const slowPass = 100 * time.Millisecond

// stopwatch splits a span of wall time into parts: each lap answers the time since the lap before
// it, or since start, so every moment from the start to the last lap falls in exactly one part.
type stopwatch struct {
	began, last time.Time
}

func (s *stopwatch) start() {
	s.began = time.Now()
	s.last = s.began
}

// lap answers the time since the last lap, or since start, and begins the next part there.
func (s *stopwatch) lap() time.Duration {
	now := time.Now()
	took := now.Sub(s.last)
	s.last = now
	return took
}

// total is the time from the start to the last lap.
func (s *stopwatch) total() time.Duration {
	return s.last.Sub(s.began)
}

// passPart is one part of a pass (refresh), in the order a pass runs them: the change log's
// outbox moved into the log (drain, delivery.DrainPending); the projects, the owner keys and the
// settings read, and the seeds owed (seed, load); the event log's safe horizon read (horizon);
// then, of each page, its cursors and delivery changes read (changes), its events read (events),
// its rows brought to the delivery settings (settings, syncSettings, which runs a settings rewrite
// where one is owed), its repositories that hold a delivery run compared with the record (kept,
// syncKept), its default repositories compared with those published (defaults, syncDefaults), its
// days written (work, apply), its commit (commit, commitPage) and the days its writes left owed
// (owed, writeOwed), each page's added to the page's before; then the walks' and the serving
// records read (walk_reads), the delivery walk's batch (delivery_walk), each project's Legion
// walk's (legion_walks), the rebuild's walks (rebuild_walks, rebuildDays), each project's logged
// zero fill and unowned-p0 row of the day (daily, walkDaily), and the stored figures of the days
// the walks wrote folded (fold_walked, foldWalked); the stored figures' read of the change log and
// its folds (figure_changes, figureChanges), each project's figures walk's batch (figures_walk,
// walkFigures), the stale cuts folded again (refold, refoldStaleCuts), the rebuild's rest
// (finish_rebuild, finishRebuild), the change log pruned (prune), and the next due pass read
// (next, nextPass) with the safe horizon's held-back check. A pass with no batch of the walks runs
// none of the walks' parts, the figures walk, the rebuild's rest or the prune.
type passPart int

const (
	partDrain passPart = iota
	partSeed
	partHorizon
	partChanges
	partEvents
	partSettings
	partKept
	partDefaults
	partWork
	partCommit
	partOwed
	partWalkReads
	partDeliveryWalk
	partLegionWalks
	partRebuildWalks
	partDaily
	partFoldWalked
	partFigureChanges
	partFiguresWalk
	partRefold
	partFinishRebuild
	partPrune
	partNext
	passPartCount
)

// passPartNames is each part's field in the line a slow pass logs, in the order of passPart.
var passPartNames = [passPartCount]string{
	"drain_ms", "seed_ms", "horizon_ms", "changes_ms", "events_ms", "settings_ms", "kept_ms", "defaults_ms", "work_ms",
	"commit_ms", "owed_ms", "walk_reads_ms", "delivery_walk_ms", "legion_walks_ms", "rebuild_walks_ms", "daily_ms",
	"fold_walked_ms", "figure_changes_ms", "figures_walk_ms", "refold_ms", "finish_rebuild_ms", "prune_ms", "next_ms",
}

// passParts is how long each part of one pass took, read off a stopwatch the pass starts as it
// begins, so every moment of the pass falls in exactly one part (mark), and how many pages it read
// and how many delivery changes they held.
type passParts struct {
	stopwatch
	took           [passPartCount]time.Duration
	pages, changes int
}

// mark gives part the time since the part before it ended.
func (p *passParts) mark(part passPart) {
	p.took[part] += p.lap()
}

// logSlow logs the parts of a pass that took slowPass or more (passParts), and nothing for a faster
// one: whether it began the gap's batch of the walks, went on with the batch a pass before it left
// (walking), or built none, whether its walks or its refold stopped for an owed wake, whether work
// is left, and how many pages and delivery changes it read.
func (r refreshed) logSlow(begun, walking bool) {
	total := r.parts.total()
	if total < slowPass {
		return
	}
	batch := "none"
	switch {
	case begun:
		batch = "began"
	case walking:
		batch = "continued"
	}
	fields := make([]any, 0, 12+2*int(passPartCount))
	fields = append(fields, "total_ms", total.Milliseconds(), "batch", batch, "yielded", r.yielded, "walks_left", r.walksLeft,
		"pages", r.parts.pages, "changes", r.parts.changes)
	for part, took := range r.parts.took {
		fields = append(fields, passPartNames[part], took.Milliseconds())
	}
	slog.Info("dispatch: the eval day-row refresher's pass took 100 ms or more", fields...)
}

// builtDay is one project's day of one family a pass wrote.
type builtDay struct {
	family  string
	project string
	day     time.Time
}

// sorted is the days written as `<family> <project> <day>`, in that order.
func (r refreshed) sorted() []string {
	lines := make([]string, 0, len(r.days))
	for day := range r.days {
		lines = append(lines, day.family+" "+day.project+" "+day.day.Format(time.DateOnly))
	}
	slices.Sort(lines)
	return lines
}

// refresh is one pass: it moves the change log's pending rows into the log (delivery.DrainPending),
// so the page it reads holds every change committed before it began, seeds the projects it owes a
// seed (refresh.load, with seeds, the loop's own), and reads page after page of the two feeds until
// one comes back short and writes the days each names: the changes' work, which every pass does.
// Given batch, the walks' batch of the gap (gapBatch), it then does the background work: it builds
// what the batch has left of each walk (walk), makes the stored figures' part (figuresPass), folds
// again each cut a default move marked stale (refoldStaleCuts), does the rebuild's rest
// (finishRebuild), prunes the change log, and answers when the loop should run again with no wake
// (nextPass). Given none, it reads the stored figures' part of the change log (figureChanges),
// folds again each stale cut and answers no due time: its loop asks for the pass that begins the
// next batch (runRefresher). Either way the refold runs to its end or to an owed wake, whose pass
// the loop starts at once and which goes on with it. It answers what it wrote, given batch whether
// a walk, the refold, the rebuild or its cleanup has work left, whether its walks or its refold
// stopped for an owed wake, and how long each of its parts took (passParts).
//
// A pass that leaves an event id standing above the safe horizon it read has read less than the
// log holds (events.Horizon.HeldBack): an event that committed while a lower id was still open,
// which its pages stopped below. It says so (heldBack), and its loop asks for a pass horizonRetry
// away, doubling with each such pass in a row, not for nextPass's day's end, because nothing
// announces the moment the horizon moves past that event, and the event, a P0 issue's opening say,
// would otherwise wait for an unrelated wake or the next UTC midnight.
func (s *Store) refresh(ctx context.Context, deps RefresherDeps, sched refresherSchedule, seeds *seeding, batch *gapBatch) (refreshed, time.Time, error) {
	r := s.newRefresh(deps.Build, sched.now().UTC(), sched.batch)
	r.clock, r.refoldBudget = sched.now, sched.refoldBudget
	parts := &r.report.parts
	parts.start()
	if _, err := delivery.DrainPending(ctx, s.database.Pool); err != nil {
		return r.report, time.Time{}, err
	}
	parts.mark(partDrain)
	if err := r.load(ctx, seeds); err != nil {
		return r.report, time.Time{}, err
	}
	parts.mark(partSeed)
	safe, err := deps.Horizon.SafeOrAwait(ctx)
	if err != nil {
		return r.report, time.Time{}, err
	}
	parts.mark(partHorizon)
	for {
		more, err := r.page(ctx, safe, sched.page)
		if err != nil {
			return r.report, time.Time{}, err
		}
		if !more {
			break
		}
	}
	if batch == nil {
		if err := r.figureChanges(ctx, sched.page); err != nil {
			return r.report, time.Time{}, err
		}
		parts.mark(partFigureChanges)
		if _, err := r.refoldStaleCuts(ctx); err != nil {
			return r.report, time.Time{}, err
		}
		parts.mark(partRefold)
		r.report.heldBack = deps.Horizon.HeldBack(safe)
		parts.mark(partNext)
		return r.report, time.Time{}, nil
	}
	r.batched = batch
	live, rebuilding, err := r.walk(ctx)
	if err != nil {
		return r.report, time.Time{}, err
	}
	building, err := r.figuresPass(ctx, live, sched.page)
	if err != nil {
		return r.report, time.Time{}, err
	}
	parts.mark(partFiguresWalk)
	refolding, err := r.refoldStaleCuts(ctx)
	if err != nil {
		return r.report, time.Time{}, err
	}
	parts.mark(partRefold)
	finishing, err := r.finishRebuild(ctx, rebuilding)
	if err != nil {
		return r.report, time.Time{}, err
	}
	parts.mark(partFinishRebuild)
	r.report.walksLeft = live || rebuilding || building || refolding || finishing
	// The log keeps a day (delivery.ChangeRetention). A failed prune leaves the rows for the next
	// pass and fails nothing else, as the reconcile's does.
	if _, err := delivery.PruneChanges(ctx, s.database.Pool); err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: the eval day-row refresher could not prune the delivery changes", "error", err)
	}
	parts.mark(partPrune)
	next, err := r.nextPass(ctx)
	if err != nil {
		return r.report, time.Time{}, err
	}
	r.report.heldBack = deps.Horizon.HeldBack(safe)
	parts.mark(partNext)
	return r.report, next, nil
}

// newRefresh is the state of one pass that builds as build, as of now, batch days of each walk,
// with a batch of its own (gapBatch), which refresh replaces with the loop's. Its clock stands at
// now, which a caller that runs a page on its own keeps or replaces; refresh gives it the loop's
// (refresherSchedule.now), so each page reads the time it runs at. It has no refold budget, which
// refresh gives it from the schedule (refresherSchedule.refoldBudget).
func (s *Store) newRefresh(build string, now time.Time, batch int) *refresh {
	return &refresh{
		store: s, build: build, now: now, today: measures.Midnight(now), batch: batch,
		clock:  func() time.Time { return now },
		report: refreshed{days: map[builtDay]bool{}},
		first:  map[walkKey]map[evalKey]time.Time{}, since: map[string]LegionSince{},
		batched: &gapBatch{},
	}
}

// pruneSlack is how long after the oldest change passes its retention the loop runs the pass that
// prunes it, with no wake: the log keeps its 24 hours within these five minutes, the reconcile's
// prune period. Only a pass that builds the walks' batch prunes (refresh), not one that does its
// changes' work alone, so a log changes reach every few seconds is pruned about once each
// RefresherGap, each prune taking what expired since the one before it.
const pruneSlack = 5 * time.Minute

// nextPass is when the loop should run again with no wake, as far as the stored rows ask: at the
// earlier of the next UTC midnight, the first moment of a day every walk builds, and pruneSlack
// after the oldest change the log holds passes its retention, which the pass then prunes. A walk
// with days left asks for the pass that begins its next batch itself (runRefresher).
func (r *refresh) nextPass(ctx context.Context) (time.Time, error) {
	next := r.today.AddDate(0, 0, 1)
	var oldest *time.Time
	if err := r.store.database.Pool.QueryRow(ctx, `select min(at) from delivery_changes`).Scan(&oldest); err != nil {
		return time.Time{}, fmt.Errorf("read the oldest delivery change: %w", err)
	}
	if oldest != nil {
		if expires := oldest.Add(delivery.ChangeRetention + pruneSlack); expires.Before(next) {
			next = expires
		}
	}
	return next, nil
}

// feedCursors is where the refresher's two feeds stand, the rows of eval_days_cursor: the change
// cursor (delivery.ChangeCursor), which the lost-cursor rule judges, and the id of the last event
// a committed page read.
type feedCursors struct {
	changes delivery.ChangeCursor
	events  int64
}

// readRefresherCursors is where each feed stands, and false where no row exists at all, a database
// no refresher pass has committed on. The event cursor is the id the last committed page read
// through. The change cursor is the seq the last committed page read through (Seq); when the
// change at that seq was recorded (delivery_changes.at), which tells a change pruned in its time
// from one lost (delivery.CursorLost), nil for a seq no change holds; and when a page last read
// the change log to its end (ReadAt, the row's updated_at), which tells a log pruned past changes
// no page read since.
func (s *Store) readRefresherCursors(ctx context.Context) (feedCursors, bool, error) {
	rows, err := s.database.Pool.Query(ctx, `select name, position, at, updated_at from eval_days_cursor where name = any($1)`, refresherCursors)
	if err != nil {
		return feedCursors{}, false, fmt.Errorf("read the eval day-row refresher's cursors: %w", err)
	}
	var cursors feedCursors
	var name string
	var position int64
	var at *time.Time
	var stamped time.Time
	read, err := pgx.ForEachRow(rows, []any{&name, &position, &at, &stamped}, func() error {
		switch name {
		case changesCursor:
			cursors.changes = delivery.ChangeCursor{Seq: position, At: at, ReadAt: stamped}
		case eventsCursor:
			cursors.events = position
		}
		return nil
	})
	if err != nil {
		return feedCursors{}, false, fmt.Errorf("read the eval day-row refresher's cursors: %w", err)
	}
	return cursors, read.RowsAffected() > 0, nil
}

// commitPage moves the two cursors to the positions a page read, begins again each walk the page's
// changes call for, begins the rebuild they call for (work.rebuild: of every served family,
// replaceRebuild), records work.built, where it is set, as the delivery settings the delivery and
// CI rows stand built under (eval_days_settings), and work.kept, where it is set, as the
// repositories they stand built for (syncKept), in one transaction, which first writes spans:
// the days of each family whose days the page writes in its commit, with every other day they
// move (writeSpans: the rows, then the members, then the stored figures, ahead of the cursors,
// the walks and the settings). The days the page named that move no other day are written before
// it, so a crash between the two reads the same changes again, and a walk or a rebuild the changes
// begin is recorded with the cursor that passed them, and with the settings it builds under where
// none were recorded (syncSettings). A walk a lost cursor begins again is one of a family no rows
// are served of, which a walk builds in the open; a served family's is the rebuild's. Where the
// settings it records move the deploy repository a read of the evals takes, or none were recorded
// and the page read a change of the settings row (work.settingsApplied, syncSettings), the commit
// says that the day rows stand at the settings
// (markSettingsApplied); a rewrite of the rows says so in its own transaction where it moved the
// deploy repository (rewriteSettings). Where a project's
// default repository moved (work.defaults, syncDefaults), the commit publishes it, with the marks
// of the stored cuts it moves and the first paints of those whose evals are all computed
// (publishDefaults). It takes no eval lock, whose place in the eval tables' lock order is before
// every row lock (defaults.go): the day rows and stored figures it writes are of computed evals,
// whose rows are the refresher's alone. The event cursor
// is stamped with the moment the pass ran as of, and the change cursor with its ReadAt, when a
// page last read the change log to its end (page), which the lost-cursor rule reads and which a
// full page leaves where it stood. Once the transaction commits, the pass records what the spans
// wrote (committed), and moves a batch of the change log's outbox into the log where the commit
// logged a change.
func (r *refresh) commitPage(ctx context.Context, cursors feedCursors, restarts []walkKey, work *pageWork, spans []*spanWrite) error {
	var rebuildWalks []walkKey
	if work.rebuild != "" {
		var err error
		if rebuildWalks, err = r.servedWalks(ctx); err != nil {
			return err
		}
		served := map[walkKey]bool{}
		for _, key := range rebuildWalks {
			served[key] = true
		}
		restarts = slices.DeleteFunc(slices.Clone(restarts), func(key walkKey) bool { return served[key] })
	}
	began := time.Now()
	tx, err := r.store.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the eval day-row refresher's cursors: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := writeSpans(ctx, tx, spans); err != nil {
		return err
	}
	if work.settingsApplied {
		if err := markSettingsApplied(ctx, tx); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		insert into eval_days_cursor (name, position, at, updated_at)
		select name, position, at, read_at
		from unnest($1::text[], $2::bigint[], $3::timestamptz[], $4::timestamptz[]) as given(name, position, at, read_at)
		on conflict (name) do update set position = excluded.position, at = excluded.at, updated_at = excluded.updated_at
	`, []string{changesCursor, eventsCursor},
		[]int64{cursors.changes.Seq, cursors.events},
		[]*time.Time{cursors.changes.At, nil},
		[]time.Time{cursors.changes.ReadAt, r.now}); err != nil {
		return fmt.Errorf("move the eval day-row refresher's cursors: %w", err)
	}
	if len(restarts) > 0 {
		families, projects := walkColumns(restarts)
		// The walk begins again whichever build's process runs it next: a change this page passed
		// is read by no other pass.
		if _, err := tx.Exec(ctx, `
			update eval_days_walks walk set next_day = null, built_by = $3, updated_at = $4
			from unnest($1::text[], $2::text[]) as given(family, project)
			where walk.family = given.family and walk.project_key = given.project
		`, families, projects, r.build, r.now); err != nil {
			return fmt.Errorf("begin %d walks of the day rows again: %w", len(restarts), err)
		}
	}
	if work.rebuild != "" {
		if err := replaceRebuild(ctx, tx, work.rebuild, r.build, rebuildWalks); err != nil {
			return err
		}
	}
	if work.built != nil {
		if err := recordBuiltUnder(ctx, tx, *work.built); err != nil {
			return err
		}
	}
	if work.kept != nil {
		if err := recordKept(ctx, tx, work.kept); err != nil {
			return err
		}
	}
	var published *movePublished
	if work.defaults != nil {
		move, err := publishDefaults(ctx, tx, r.build, work.defaults)
		if err != nil {
			return err
		}
		published = &move
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the eval day-row refresher's cursors: %w", err)
	}
	committed := time.Now()
	r.committed(spans)
	logSpans(spans, committed.Sub(began))
	if published != nil {
		published.log(began, committed)
	}
	if work.settingsApplied || len(spans) > 0 || work.defaults != nil {
		r.store.drainChanges(ctx)
	}
	return nil
}

// walkKey names one walk: its family and its project (the empty string for the delivery walk).
type walkKey struct {
	family  string
	project string
}

// walkColumns is keys as the two arrays a statement takes them in: their families and their
// projects, pairwise.
func walkColumns(keys []walkKey) (families, projects []string) {
	families, projects = make([]string, len(keys)), make([]string, len(keys))
	for i, key := range keys {
		families[i], projects[i] = key.family, key.project
	}
	return families, projects
}

// walkState is one walk's record: the first day it has not built, nil while it is to begin at its
// first input day.
type walkState struct {
	next *time.Time
}

// staleRebuild is how long a rebuild stands unmoved before another build's refresher abandons it
// (rebuildDays): a rebuild its build's process runs moves with each batch it stages, so one two
// days still is one no running process holds.
const staleRebuild = 48 * time.Hour

// readWalks is every record of a walk, whichever build's process last moved it.
func (s *Store) readWalks(ctx context.Context) (map[walkKey]walkState, error) {
	rows, err := s.database.Pool.Query(ctx, `select family, project_key, next_day from eval_days_walks`)
	if err != nil {
		return nil, fmt.Errorf("read the walks of the day rows: %w", err)
	}
	walks := map[walkKey]walkState{}
	var key walkKey
	var state walkState
	if _, err := pgx.ForEachRow(rows, []any{&key.family, &key.project, &state.next}, func() error {
		kept := state
		if state.next != nil {
			next := measures.Midnight(*state.next)
			kept.next = &next
		}
		walks[key] = kept
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the walks of the day rows: %w", err)
	}
	return walks, nil
}

// recordWalk records that the walk key has built every day before next, under the pass's build.
func (r *refresh) recordWalk(ctx context.Context, key walkKey, next time.Time) error {
	return r.recordWalkIn(ctx, r.store.database.Pool, key, next)
}

// executor is what recordWalkIn writes through: the pool, or a transaction whose other writes the
// record commits with (the figures walk's, buildFigures).
type executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// recordWalkIn is recordWalk's write in q. A walk of the delivery or the Legion rows that builds
// a day the stored rows were not whole through moves their serving record on with it
// (eval_days_serving), and makes it, under the walk's build, where none is: a family's first walk,
// whose days are served from the first it writes. It never moves the record back: a walk begun
// again stands behind it.
func (r *refresh) recordWalkIn(ctx context.Context, q executor, key walkKey, next time.Time) error {
	if _, err := q.Exec(ctx, `
		insert into eval_days_walks (family, project_key, built_by, next_day, updated_at)
		values ($1, $2, $3, $4, $5)
		on conflict (family, project_key) do update
		set next_day = excluded.next_day, built_by = excluded.built_by, updated_at = excluded.updated_at
	`, key.family, key.project, r.build, next, r.now); err != nil {
		return fmt.Errorf("record the %s walk of %q through %s: %w", key.family, key.project, next.Format(time.DateOnly), err)
	}
	if key.family != walkDelivery && key.family != walkLegion {
		return nil
	}
	if _, err := q.Exec(ctx, `
		insert into eval_days_serving (family, project_key, built_by, next_day, updated_at) values ($1, $2, $3, $4, $5)
		on conflict (family, project_key) do update set next_day = excluded.next_day, updated_at = excluded.updated_at
		where eval_days_serving.next_day < excluded.next_day
	`, key.family, key.project, r.build, next, r.now); err != nil {
		return fmt.Errorf("move the serving record of the %s rows of %q to %s: %w", key.family, key.project, next.Format(time.DateOnly), err)
	}
	return nil
}

// RefresherAgeIn answers, read in q, how long before now the refresher last read its feeds, as the
// oldest of its two cursors' updated_at (the stored figures' own cursor, figuresCursor, is not one
// of them), and false when no pass has committed on this database. Every pass stamps the event
// cursor with the moment it ran as of, whether or not its feed held anything new, and the change
// cursor with the moment one of its pages last read the change log to its end (commitPage). So
// the age is the time since the last pass ran, or since a pass last read the change log to its
// end, whichever is older: a pass catching up a backlog of changes reads as old as its last read
// of the whole log. It is not how long ago the last change arrived: it is the freshness the eval
// list states of the computed evals' day rows, read in the snapshot of the rows it states it of
// (BeginSnapshot). A caller whose clock stands behind the writing process's reads a negative age,
// as it would of any moment another process stamped.
func RefresherAgeIn(ctx context.Context, q delivery.Querier, now time.Time) (time.Duration, bool, error) {
	var oldest *time.Time
	if err := q.QueryRow(ctx, `select min(updated_at) from eval_days_cursor where name = any($1)`, refresherCursors).Scan(&oldest); err != nil {
		return 0, false, fmt.Errorf("read the age of the eval day-row refresher's cursors: %w", err)
	}
	if oldest == nil {
		return 0, false, nil
	}
	return now.Sub(*oldest), true, nil
}
