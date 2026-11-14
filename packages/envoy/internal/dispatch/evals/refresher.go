package evals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The day-row refresher (dispatch://INFRA-2916, R10, R12, R15 and R16) keeps every project's
// computed day rows true to their inputs: the delivery and CI rows (delivery_days.go), the Legion
// rows (legion_days.go) and the unowned-p0 row of today, and the explicit zero rows of every series
// from its first input day on, the logged evals' included. It is one woken loop under one lease
// (RefresherLease), with a cursor of its own in each of the two feeds it reads (eval_days_cursor,
// migration 20261113170753):
//
//   - delivery_changes, woken by delivery.ChangesChannel. Each change names the days it touches
//     (refresher_work.go): a pull request's merge day and the start day of the deploy run that
//     ships it now and of the one that shipped it before; a run's start day and the merge days of
//     the pull requests it ships now and shipped before; a stored Legion fact's close day; a
//     filing's day; and, through eval_day_members, the day each of those items was counted on
//     before, so an item whose day or project moved rewrites the day it left as well as the day
//     it joined. A settings change (the delivery settings, or a repository's
//     mapping in repo_projects, 20261113170752) begins the delivery walk again, and a mapping
//     begins every project's Legion walk again too.
//   - the event log up to its safe horizon, woken by the events channel: an issue event that can
//     move the count of open P0 issues with no owner rewrites that project's unowned-p0 row of
//     today, and one that moves an issue's repository or parent rewrites the days of the filings of
//     that issue and every issue below it, whose repository it moves.
//
// Beside the feeds, each pass makes the walks (eval_days_walks): one over every day of the delivery
// and CI rows, one per project over its Legion days, and one per project filling its logged evals'
// zero rows. A walk builds every day from the first input day of what it walks to today, a batch
// of days a pass (walkBatch), then the day of each new UTC day as it comes, which is what writes
// the zero rows of a day nothing happened on. A walk recorded by another build is begun again, so
// a release rebuilds every row of each family the build before it wrote, and while two builds run
// each rebuilds in turn, as issue_facts_computed makes the Legion facts consumer read each project
// whole again.
//
// A day written is every row of its project's family of that day, upserted where a value moved
// (upsertDays), the rows the day no longer builds written as zero rows (zeroStaleDays: each had
// input once, so a zero is its value), and a zero row of every series whose
// first input day is on or before it and that the day has no input of. A series' first input day
// is the first day the store holds a row of it: the builders write a row of a series only on a day
// with its input, and the refresher writes a zero row only on a day at or after that first day, so
// the first row is the first input day and a missing row is a day before it. A day that loses all
// its input therefore becomes zero rows rather than losing its rows, and the eval_days trigger,
// which logs nothing for a delete, logs the change and wakes the views.
//
// A pass that fails logs and leaves its cursors where they were, so the next pass reads the same
// changes again; every write is safe to make twice.

// RefresherLease names the lease every refresher pass takes (duty.Exclusive with no period, which
// is mutual exclusion and nothing else): two processes are woken by one commit, and two passes at
// once would build the same days twice and walk the same days twice.
const RefresherLease = "eval-days"

// RefresherGap is the loop's MinGap: the least time from one pass's start to the next whatever woke
// it, so a burst of changes is answered by one pass every ten seconds rather than one per change.
// A computed eval's view wakes once the refresher has rewritten its rows (R10), so this is how far
// behind its inputs a computed eval can stand while changes arrive, plus one pass.
const RefresherGap = 10 * time.Second

// refresherPage is the most rows one page of a pass reads of each feed. A pass whose page was full
// reads the next one at once.
const refresherPage = 1000

// walkBatch is how many days one pass builds of each walk. A walk of INFRA's history (a year of
// days, each one read of the day's delivery rows and one write per project) is many passes, each
// started at once after the one before it, so the changes that arrive meanwhile are answered
// between two batches rather than after the whole rebuild, and no pass holds the lease for the
// whole of it. Every day is a transaction of its own per project (WriteProjectDay).
const walkBatch = 28

// The cursors, one row of eval_days_cursor each, named for the feed they are read by.
const (
	changesCursor = "delivery_changes"
	eventsCursor  = "events"
)

// refresherCursors is the two cursors in one list, which is how a pass writes them.
var refresherCursors = []string{changesCursor, eventsCursor}

// The walks of eval_days_walks, by family: the delivery and CI rows of every project (one walk,
// project ”), the Legion rows of one project, and the zero rows of one project's logged evals.
const (
	walkDelivery = "delivery"
	walkLegion   = "legion"
	walkLogged   = "logged"
)

// RefresherDeps is what RunRefresher needs of the server.
type RefresherDeps struct {
	// Horizon is the process's one events.Horizon: the event log is paged by a global id, which
	// is not commit order, so a page stops at its safe horizon.
	Horizon *events.Horizon
	// Build names the build this process is, which every row the refresher writes records
	// (built_by) and every walk is recorded under: cmd/dispatch's processBuild, the commit the
	// image build stamped, as the Legion facts consumer reads it (legionfacts.Deps.Build). The
	// empty string is no build, and RunRefresher refuses it: a walk recorded under it would be
	// found its own by the next process that carried none.
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
// gap is RefresherGap, page refresherPage, batch walkBatch, and now answers the moment whose UTC
// day is today.
type refresherSchedule struct {
	gap   time.Duration
	page  int
	batch int
	now   func() time.Time
}

// serverRefresherSchedule is the pace the server runs the loop at.
var serverRefresherSchedule = refresherSchedule{
	gap:   RefresherGap,
	page:  refresherPage,
	batch: walkBatch,
	now:   func() time.Time { return time.Now().UTC() },
}

// RunRefresher keeps every project's day rows true to their inputs (the refresher's rules above)
// until ctx is done: one pass at start, a pass on each wake, a pass at once after one that left a
// walk unfinished, and a pass at the earliest of the next UTC midnight, which begins a day every
// walk builds, and the moment the oldest delivery change passes delivery.ChangeRetention, which
// the pass then prunes (delivery.PruneChanges), so the change log keeps its retention where no
// GitHub App runs the reconcile that prunes it too.
func (s *Store) RunRefresher(ctx context.Context, deps RefresherDeps) {
	s.runRefresher(ctx, deps, serverRefresherSchedule)
}

func (s *Store) runRefresher(ctx context.Context, deps RefresherDeps, sched refresherSchedule) {
	if deps.Build == "" {
		panic("evals: RefresherDeps.Build is empty, and the refresher needs the build this process is")
	}
	ctx = store.WithTransactionTracking(ctx)
	duty.RunWoken(ctx, duty.WokenLoop{
		Lease:  RefresherLease,
		Leases: deps.Leases,
		Wake:   duty.MergeWakes(ctx, deps.Events, deps.Deliveries),
		MinGap: sched.gap,
		Pass: func(ctx context.Context) time.Time {
			report, next, err := s.refresh(ctx, deps, sched)
			if err != nil && ctx.Err() == nil {
				slog.Warn("dispatch: the eval day-row refresher's pass failed", "error", err)
				next = time.Now().Add(sched.gap)
			}
			if ctx.Err() != nil {
				return time.Time{}
			}
			if len(report.days) > 0 {
				slog.Debug("dispatch: eval day rows refreshed", "days", len(report.days))
			}
			return next
		},
	})
}

// refreshed is what one pass wrote: each family's day of each project it built and wrote, which is
// what a check of which days a change touches reads. The unowned-p0 row of today is recorded under
// its slug, and a logged eval's zero fill under walkLogged and the day it filled through.
type refreshed struct {
	days map[builtDay]bool
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

// refresh is one pass: it seeds the projects no seed has reached, reads page after page of the two
// feeds until one comes back short and writes the days each names, makes a batch of each walk,
// prunes the change log, and answers what it wrote and when the loop should run again.
func (s *Store) refresh(ctx context.Context, deps RefresherDeps, sched refresherSchedule) (refreshed, time.Time, error) {
	now := sched.now().UTC()
	r := &refresh{
		store: s, build: deps.Build, now: now, today: utcMidnight(now), batch: sched.batch,
		report: refreshed{days: map[builtDay]bool{}},
		first:  map[walkKey]map[string]time.Time{}, since: map[string]LegionSince{},
		reads: map[time.Time]DeliveryDay{}, unowned: map[string]bool{},
	}
	if err := r.load(ctx); err != nil {
		return r.report, time.Time{}, err
	}
	safe, err := safeHorizon(ctx, deps.Horizon)
	if err != nil {
		return r.report, time.Time{}, err
	}
	for {
		more, err := r.page(ctx, safe, sched.page)
		if err != nil {
			return r.report, time.Time{}, err
		}
		if !more {
			break
		}
	}
	pending, err := r.walk(ctx)
	if err != nil {
		return r.report, time.Time{}, err
	}
	// The log keeps a day (delivery.ChangeRetention). A failed prune leaves the rows for the next
	// pass and fails nothing else, as the reconcile's does.
	if _, err := delivery.PruneChanges(ctx, s.database.Pool); err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: the eval day-row refresher could not prune the delivery changes", "error", err)
	}
	next, err := r.nextPass(ctx, pending)
	if err != nil {
		return r.report, time.Time{}, err
	}
	return r.report, next, nil
}

// safeHorizon is the highest event id at or below which every event is final (events.Horizon). A
// safe of 0 is a horizon that has proven nothing yet, which is every first pass of a process, since
// one snapshot never proves its own newest id: such a pass takes a horizon with Await instead of
// reading no event at all, as the Legion facts consumer's does.
func safeHorizon(ctx context.Context, horizon *events.Horizon) (int64, error) {
	safe, err := horizon.Safe(ctx)
	if err != nil {
		return 0, fmt.Errorf("read the event log's safe horizon: %w", err)
	}
	if safe > 0 {
		return safe, nil
	}
	if safe, err = horizon.Await(ctx); err != nil {
		return 0, fmt.Errorf("await the event log's safe horizon: %w", err)
	}
	return safe, nil
}

// nextPass is when the loop should run again with no wake: at once while a walk has days left, and
// otherwise at the earlier of the next UTC midnight, the first moment of a day every walk builds,
// and the moment the oldest change the log holds passes its retention, which the pass then prunes.
func (r *refresh) nextPass(ctx context.Context, pending bool) (time.Time, error) {
	if pending {
		return r.now, nil
	}
	next := r.today.AddDate(0, 0, 1)
	var oldest *time.Time
	if err := r.store.database.Pool.QueryRow(ctx, `select min(at) from delivery_changes`).Scan(&oldest); err != nil {
		return time.Time{}, fmt.Errorf("read the oldest delivery change: %w", err)
	}
	if oldest != nil {
		if expires := oldest.Add(delivery.ChangeRetention + time.Second); expires.Before(next) {
			next = expires
		}
	}
	return next, nil
}

// readRefresherCursors is where each feed stands, by cursor name; no row at all is a database no
// refresher pass has committed on.
func (s *Store) readRefresherCursors(ctx context.Context) (map[string]int64, error) {
	rows, err := s.database.Pool.Query(ctx, `select name, position from eval_days_cursor where name = any($1)`, refresherCursors)
	if err != nil {
		return nil, fmt.Errorf("read the eval day-row refresher's cursors: %w", err)
	}
	cursors := map[string]int64{}
	var name string
	var position int64
	if _, err := pgx.ForEachRow(rows, []any{&name, &position}, func() error {
		cursors[name] = position
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the eval day-row refresher's cursors: %w", err)
	}
	return cursors, nil
}

// commitPage moves the two cursors to the positions a page read, stamped with the moment the pass
// ran as of, and begins again each walk the page's changes call for, in one transaction: the days
// the page named are written before it, so a crash between the two reads the same changes again,
// and a walk the changes begin again is recorded with the cursor that passed them.
func (s *Store) commitPage(ctx context.Context, positions map[string]int64, restarts []walkKey, build string, now time.Time) error {
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the eval day-row refresher's cursors: %w", err)
	}
	defer tx.Rollback(ctx)
	at := make([]int64, len(refresherCursors))
	for i, name := range refresherCursors {
		at[i] = positions[name]
	}
	if _, err := tx.Exec(ctx, `
		insert into eval_days_cursor (name, position, updated_at)
		select name, position, $3 from unnest($1::text[], $2::bigint[]) as given(name, position)
		on conflict (name) do update set position = excluded.position, updated_at = excluded.updated_at
	`, refresherCursors, at, now); err != nil {
		return fmt.Errorf("move the eval day-row refresher's cursors: %w", err)
	}
	if len(restarts) > 0 {
		families, projects := make([]string, len(restarts)), make([]string, len(restarts))
		for i, key := range restarts {
			families[i], projects[i] = key.family, key.project
		}
		if _, err := tx.Exec(ctx, `
			insert into eval_days_walks (family, project_key, built_by, next_day, updated_at)
			select family, project, $3, null, $4 from unnest($1::text[], $2::text[]) as given(family, project)
			order by family, project
			on conflict (family, project_key) do update
			set built_by = excluded.built_by, next_day = null, updated_at = excluded.updated_at
		`, families, projects, build, now); err != nil {
			return fmt.Errorf("begin %d walks of the day rows again: %w", len(restarts), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the eval day-row refresher's cursors: %w", err)
	}
	return nil
}

// walkKey names one walk: its family and its project (” for the delivery walk).
type walkKey struct {
	family  string
	project string
}

// walkState is one walk's record: the build it was recorded under and the first day it has not
// built, nil while it is to begin at its first input day.
type walkState struct {
	build string
	next  *time.Time
}

// readWalks is every walk's record.
func (s *Store) readWalks(ctx context.Context) (map[walkKey]walkState, error) {
	rows, err := s.database.Pool.Query(ctx, `select family, project_key, built_by, next_day from eval_days_walks`)
	if err != nil {
		return nil, fmt.Errorf("read the walks of the day rows: %w", err)
	}
	walks := map[walkKey]walkState{}
	var key walkKey
	var state walkState
	if _, err := pgx.ForEachRow(rows, []any{&key.family, &key.project, &state.build, &state.next}, func() error {
		kept := state
		if state.next != nil {
			next := utcMidnight(*state.next)
			kept.next = &next
		}
		walks[key] = kept
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the walks of the day rows: %w", err)
	}
	return walks, nil
}

// recordWalk records that the walk key under build has built every day before next.
func (s *Store) recordWalk(ctx context.Context, key walkKey, build string, next, now time.Time) error {
	if _, err := s.database.Pool.Exec(ctx, `
		insert into eval_days_walks (family, project_key, built_by, next_day, updated_at)
		values ($1, $2, $3, $4, $5)
		on conflict (family, project_key) do update
		set built_by = excluded.built_by, next_day = excluded.next_day, updated_at = excluded.updated_at
	`, key.family, key.project, build, next, now); err != nil {
		return fmt.Errorf("record the %s walk of %q through %s: %w", key.family, key.project, next.Format(time.DateOnly), err)
	}
	return nil
}

// RefresherAge answers how long before now the refresher's last pass committed, measured from the
// moment that pass ran as of, and false when no pass has committed on this database. Every pass
// stamps both its cursors whether or not its feeds held anything new (commitPage), so the age is
// how long ago the last pass ran, not how long ago the last change arrived: the freshness the eval
// list states of the computed evals' day rows. A caller whose clock stands behind the writing
// process's reads a negative age, as it would of any moment another process stamped.
func (s *Store) RefresherAge(ctx context.Context, now time.Time) (time.Duration, bool, error) {
	var oldest *time.Time
	err := s.database.Pool.QueryRow(ctx, `select min(updated_at) from eval_days_cursor`).Scan(&oldest)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("read the age of the eval day-row refresher's cursors: %w", err)
	}
	if oldest == nil {
		return 0, false, nil
	}
	return now.Sub(*oldest), true, nil
}
