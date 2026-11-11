package legionfacts

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Lease names the lease every pass takes (duty.Exclusive with no period, which is mutual
// exclusion and nothing else). Two processes are woken by one commit, and a pass reads its
// cursors, computes the rows and then writes both in one transaction, so two passes at once
// would compute the same projects twice and the second would write what the first already
// wrote. The lease is what keeps the read and the write one step.
const Lease = "legion-facts"

// Freshness is how far behind the clock a stored row may stand: the requirement of
// dispatch://INFRA-2915. An open issue's hours grow with the clock while no input moves, so the
// loop recomputes the open rows on a pace of its own (SweepEvery) rather than wait for a change
// that never comes.
const Freshness = time.Minute

// SweepEvery is that pace: the loop's MinGap, the least time from one pass's start to the next
// whatever woke it, and the due time each pass asks for, so a pass runs this often while nothing
// changes.
//
// A row is computed as of the start of the pass that writes it, so a row's age when the next
// pass rewrites it is SweepEvery plus that pass's own length. Thirty seconds holds Freshness
// while a pass runs under thirty seconds. Measured against a real Postgres over the 252 issues
// of the facts' own rig, 210 of them with Legion phase work: one ComputeProject and Write of
// the whole project takes 115 ms to 205 ms, and one sweep pass that finds nothing new 45 ms to
// 80 ms (TestTheSweepsPaceHoldsTheFreshnessRequirement prints both, over five runs). A pass
// may therefore grow to about 380 times its measured length before a stored row can stand a
// minute old.
const SweepEvery = 30 * time.Second

// feedPage is the most rows one transaction reads of each feed. A pass whose page was full reads
// the next one at once, in a transaction of its own, so a backlog is worked through in bounded
// steps that each commit their progress.
const feedPage = 1000

// The cursors, one row of issue_facts_cursor each, named for the feed it is read by. A cursor
// holds the position of the last row of its feed a committed pass read.
const (
	eventsCursor       = "events"
	observationsCursor = "legion_observations"
	changesCursor      = "delivery_changes"
)

// cursorNames is the three in one list, which is how a pass reads and writes them.
var cursorNames = []string{eventsCursor, observationsCursor, changesCursor}

// ChangesChannel is the Postgres notification channel a delivery fact change announces itself on:
// the trigger that writes each delivery_changes row sends it at commit (migration 0100,
// delivery_change_log). The Delivery page's live hub listens on the same name.
const ChangesChannel = "delivery_changes"

// pullRequestChange is the delivery_changes kind this loop keeps: a pull request's row, whose
// merge time is the one delivery fact an issue's facts read.
const pullRequestChange = "pull_request"

// Deps is what Run needs of the server.
type Deps struct {
	// Pool is the database the facts are computed from and written to.
	Pool *store.Pool
	// Horizon is the process's one events.Horizon, the Horizon the live event feed reads
	// through: the events feed is paged by a global id, so it must stop at a safe horizon.
	Horizon *events.Horizon
	// Leases is the pool a pass takes its lease connection from (duty.Exclusive).
	Leases duty.Leases
	// Events is signalled when an event commits, whichever process wrote it: in the server the
	// pglisten.Hub wake on api.EventsChannel.
	Events <-chan struct{}
	// Observations is signalled when the sampler has read a daemon: the hub's wake on
	// legion.StateChannel.
	Observations <-chan struct{}
	// Deliveries is signalled when a delivery fact changed: the hub's wake on ChangesChannel.
	Deliveries <-chan struct{}
}

// schedule is the loop's pace and the clock a pass computes as of, which a test replaces: every
// is SweepEvery, page is feedPage, and now answers the moment a pass's rows are the facts as of.
type schedule struct {
	every time.Duration
	page  int
	now   func() time.Time
}

// serverSchedule is the pace the server runs the loop at.
var serverSchedule = schedule{
	every: SweepEvery,
	page:  feedPage,
	now:   func() time.Time { return time.Now().UTC() },
}

// Run keeps every project's stored Legion facts within a minute of its inputs and of the clock.
// It stops when ctx is done.
//
// It is a woken loop (duty.RunWoken) under one lease (Lease): one pass at start, a pass on each
// of the three wakes, and a pass every SweepEvery whether or not anything moved, because an open
// issue's hours grow with the clock and no wake announces that.
//
// Four inputs move a stored row, and a pass reads the first three from a cursor of its own:
//  1. The issue's events, woken by Deps.Events. Every event of an issue can move that issue's
//     facts - a status mark, a role event, an ask turn, a handoff.recorded, and the session
//     title an event's actor carries, which decides whether the issue carries Legion phase work
//     at all - so the pass takes the page's issues and recomputes them (readEventPage).
//  2. The daemon's observations, woken by Deps.Observations: the phase, the slot and the wait
//     the sampler recorded (readObservationPage).
//  3. A pull request's merge time, woken by Deps.Deliveries (readChangePage).
//  4. The clock, for an open issue, which no event moves. That is the sweep: each pass
//     recomputes the stored rows of the project's open issues (openStoredRows).
//
// A pass that fails logs and leaves its cursors where they were, so the next pass reads the same
// rows again. Every pass is safe to run twice: a row whose values did not move is not rewritten
// (Write), and a cursor moves only in the transaction that wrote the rows it covers (passPage).
func Run(ctx context.Context, deps Deps) {
	run(ctx, deps, serverSchedule)
}

func run(ctx context.Context, deps Deps, sched schedule) {
	ctx = store.WithTransactionTracking(ctx)
	duty.RunWoken(ctx, duty.WokenLoop{
		Lease:  Lease,
		Leases: deps.Leases,
		Wake:   mergeWakes(ctx, deps.Events, deps.Observations, deps.Deliveries),
		MinGap: sched.every,
		Pass: func(ctx context.Context) time.Time {
			changed, next, err := pass(ctx, deps, sched)
			if err != nil && ctx.Err() == nil {
				slog.Warn("dispatch: the Legion facts pass failed", "error", err)
				next = time.Now().Add(sched.every)
			}
			if ctx.Err() != nil {
				return time.Time{}
			}
			if len(changed) > 0 {
				slog.Debug("dispatch: Legion facts written", "rows", len(changed))
			}
			return next
		},
	})
}

// mergeWakes is the one wake a WokenLoop reads, signalled by any of the loop's three. It keeps
// the shape a pglisten.Hub wake has: a buffer of one and a send that never blocks, so any number
// of changes collapse into one signal and a change that arrives while a pass runs earns a pass
// after it. A nil wake is left out, and three nil wakes answer nil, which is a loop that runs at
// start and at each due time alone.
func mergeWakes(ctx context.Context, wakes ...<-chan struct{}) <-chan struct{} {
	live := make([]<-chan struct{}, 0, len(wakes))
	for _, wake := range wakes {
		if wake != nil {
			live = append(live, wake)
		}
	}
	switch len(live) {
	case 0:
		return nil
	case 1:
		return live[0]
	}
	merged := make(chan struct{}, 1)
	for _, wake := range live {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-wake:
					select {
					case merged <- struct{}{}:
					default:
					}
				}
			}
		}()
	}
	return merged
}

// pass is one pass of the loop: it reads page after page of the three feeds until one comes back
// short, each page computed and written in a transaction of its own, and answers the keys whose
// stored rows it changed and when the loop should run again.
//
// Every page of one pass computes as of one moment, the moment the pass started, so a row
// written by the pass holds the facts as of then. The next pass is asked for one SweepEvery from
// that moment, which is what holds a stored row within Freshness of the clock.
func pass(ctx context.Context, deps Deps, sched schedule) ([]string, time.Time, error) {
	now := sched.now()
	safe, err := safeHorizon(ctx, deps.Horizon)
	if err != nil {
		return nil, time.Time{}, err
	}
	changed := []string{}
	for sweep := true; ; sweep = false {
		keys, full, err := passPage(ctx, deps, sched, safe, now, sweep)
		changed = append(changed, keys...)
		if err != nil {
			return changed, time.Time{}, err
		}
		if !full {
			return changed, now.Add(sched.every), nil
		}
	}
}

// safeHorizon is the highest event id at or below which every event is final (events.Horizon).
// A safe of 0 is a horizon that has proven nothing yet, which is every first pass of a process,
// since one snapshot never proves its own newest id: such a pass takes a horizon with Await
// instead of reading no event at all.
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

// passPage is one page of one pass: it reads where each feed stands, reads a page of each,
// computes the rows those pages and the sweep name, and writes the rows and the three cursors in
// one transaction. It answers the keys whose stored rows it changed and whether a page was full,
// which means more rows are waiting.
//
// sweep says whether this page carries the work no feed announces: the recomputation of the open
// rows, and the backfill of a project a feed cannot answer for. Only the first page of a pass
// carries it, because both read the whole of what they cover.
//
// The rows are computed before the transaction opens and written inside it, so no connection is
// held open while a whole project is read. The cursor moves in the transaction that wrote the
// rows it covers, so a crash between the two cannot lose a change: the next pass reads the same
// rows again and writes the same values.
func passPage(ctx context.Context, deps Deps, sched schedule, safe int64, now time.Time, sweep bool) ([]string, bool, error) {
	cursors, err := readCursors(ctx, deps.Pool)
	if err != nil {
		return nil, false, err
	}
	eventPage, err := readEventPage(ctx, deps.Pool, cursors[eventsCursor], safe, sched.page)
	if err != nil {
		return nil, false, err
	}
	observationPage, err := readObservationPage(ctx, deps.Pool, cursors[observationsCursor], sched.page)
	if err != nil {
		return nil, false, err
	}
	changePage, lost, err := readChangePage(ctx, deps.Pool, cursors[changesCursor], sched.page)
	if err != nil {
		return nil, false, err
	}
	projects, err := legionProjects(ctx, deps.Pool, lost)
	if err != nil {
		return nil, false, err
	}

	work := map[string]*projectWork{}
	needed := func(project string) *projectWork {
		if _, known := projects[project]; !known {
			return nil
		}
		if work[project] == nil {
			work[project] = &projectWork{keys: map[string]bool{}, moved: map[string]bool{}}
		}
		return work[project]
	}
	for _, page := range []feed{eventPage, observationPage, changePage} {
		for _, touched := range page.touches {
			if project := needed(touched.project); project != nil {
				project.keys[touched.issue] = true
				if touched.moved {
					project.moved[touched.issue] = true
				}
			}
		}
	}
	if sweep {
		open, err := openStoredRows(ctx, deps.Pool)
		if err != nil {
			return nil, false, err
		}
		for project, keys := range open {
			if work := needed(project); work != nil {
				for _, key := range keys {
					work.keys[key] = true
				}
			}
		}
		for project, backfill := range projects {
			if !backfill {
				continue
			}
			if work := needed(project); work != nil {
				work.backfill = true
			}
		}
	}

	computed := []projectRows{}
	for _, project := range slices.Sorted(maps.Keys(work)) {
		rows, err := computeProjectWork(ctx, deps.Pool, project, work[project], now)
		if err != nil {
			return nil, false, err
		}
		if len(rows) > 0 {
			computed = append(computed, projectRows{project: project, rows: rows})
		}
	}

	positions := map[string]int64{
		eventsCursor:       eventPage.position,
		observationsCursor: observationPage.position,
		changesCursor:      changePage.position,
	}
	changed, err := writePass(ctx, deps.Pool, computed, positions, now)
	if err != nil {
		return changed, false, err
	}
	return changed, eventPage.full || observationPage.full || changePage.full, nil
}

// projectWork is what one page owes one project: the issues its feeds named, those of them whose
// parent link moved, and whether the project is to be computed whole (a backfill).
type projectWork struct {
	keys     map[string]bool
	moved    map[string]bool
	backfill bool
}

// projectRows is one project's computed rows, kept with its project because Write writes the
// rows of one project in one statement.
type projectRows struct {
	project string
	rows    []Row
}

// computeProjectWork computes the rows one project owes. A backfill reads the whole project
// (ComputeProject) and the keys its feeds named are left out of the call, because the whole
// project already holds them. Every other project is computed over the rows its changes touch
// (rowsToCompute), and no row outside that set is written.
func computeProjectWork(ctx context.Context, pool *store.Pool, project string, work *projectWork, now time.Time) ([]Row, error) {
	if work.backfill {
		return ComputeProject(ctx, pool, project, now)
	}
	parents, err := parentLinks(ctx, pool, project)
	if err != nil {
		return nil, err
	}
	keys, err := rowsToCompute(ctx, pool, project, slices.Sorted(maps.Keys(work.keys)), work.moved, parents)
	if err != nil {
		return nil, err
	}
	return Compute(ctx, pool, project, keys, now)
}

// writePass writes every project's rows and the three cursors in one transaction, and answers
// the keys whose stored rows changed. A failure leaves the cursors where they were, so the next
// pass reads the same rows again.
func writePass(ctx context.Context, pool *store.Pool, computed []projectRows, positions map[string]int64, now time.Time) ([]string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("open the Legion facts' write: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	changed := []string{}
	for _, project := range computed {
		keys, err := Write(ctx, tx, project.rows)
		if err != nil {
			return nil, err
		}
		changed = append(changed, keys...)
	}
	if err := advanceCursors(ctx, tx, positions, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit the Legion facts: %w", err)
	}
	return changed, nil
}

// feed is what one page of one feed read: where that feed now stands, the issues it named and
// whether the page was full.
type feed struct {
	position int64
	touches  []touch
	full     bool
}

// touch is one issue a feed named: the project it belongs to, the issue itself, and whether the
// row that named it says the issue's parent link moved.
type touch struct {
	project string
	issue   string
	moved   bool
}

// readEventPage is the page of events after the cursor: up to limit events above after and at or
// below safe, with each event's project and whether it says the issue's parent moved.
//
// The page stops at safe because events.id is not commit order. An event draws its id when its
// write inserts the row, and two writes that wait on nothing commit out of id order, so a cursor
// moved by an uncapped `id > cursor` read could pass an event that was still committing and
// never read it (events, the "Safe horizon" section). An event above safe is read by a later
// pass: its own commit notifies the events channel, and a pass falls every SweepEvery anyway.
//
// `changed` holds the fields an issue write moved (api's issueChangedFields), so an event naming
// `parent` is the parent link moving. That is what widens the set a change touches from the
// issue and its ancestors to the issue's whole subtree (rowsTouchedBy).
func readEventPage(ctx context.Context, q Querier, after, safe int64, limit int) (feed, error) {
	rows, err := q.Query(ctx, `
		select event.id, issue.project_key, event.issue_key,
		       coalesce(event.payload -> 'changed' @> '["parent"]', false)
		from events event
		join issues issue on issue.key = event.issue_key
		where event.id > $1 and event.id <= $2
		order by event.id
		limit $3
	`, after, safe, limit)
	if err != nil {
		return feed{}, fmt.Errorf("read the events after %d: %w", after, err)
	}
	defer rows.Close()
	page := feed{position: after}
	for rows.Next() {
		var touched touch
		if err := rows.Scan(&page.position, &touched.project, &touched.issue, &touched.moved); err != nil {
			return feed{}, fmt.Errorf("read the events after %d: %w", after, err)
		}
		page.touches = append(page.touches, touched)
	}
	if err := rows.Err(); err != nil {
		return feed{}, fmt.Errorf("read the events after %d: %w", after, err)
	}
	page.full = len(page.touches) == limit
	return page, nil
}

// readObservationPage is the page of the daemon's observations after the cursor: up to limit
// rows above after, each naming its project and its issue.
//
// It needs no horizon, and that is the one way this cursor differs from the events cursor. One
// leased sampler writes legion_observations (legion.RunSampler, one pass a minute, one process
// at a time), so no second writer can hold a lower id open while a higher one commits, and the
// ids a reader pages by are commit order.
func readObservationPage(ctx context.Context, q Querier, after int64, limit int) (feed, error) {
	rows, err := q.Query(ctx, `
		select id, project, issue_key
		from legion_observations
		where id > $1
		order by id
		limit $2
	`, after, limit)
	if err != nil {
		return feed{}, fmt.Errorf("read the Legion observations after %d: %w", after, err)
	}
	defer rows.Close()
	page := feed{position: after}
	for rows.Next() {
		var touched touch
		if err := rows.Scan(&page.position, &touched.project, &touched.issue); err != nil {
			return feed{}, fmt.Errorf("read the Legion observations after %d: %w", after, err)
		}
		page.touches = append(page.touches, touched)
	}
	if err := rows.Err(); err != nil {
		return feed{}, fmt.Errorf("read the Legion observations after %d: %w", after, err)
	}
	page.full = len(page.touches) == limit
	return page, nil
}

// readChangePage is the page of delivery changes after the cursor (delivery.ChangesSince, whose
// seq is allocated in commit order): up to limit changes above after, of which it keeps the pull
// requests. A change's repo and ref name a row of delivery_pull_requests, whose issue_key is the
// issue whose facts moved, and a pull request no issue carries names none.
//
// It also answers whether the cursor had fallen below the oldest change the log still holds.
// delivery_changes is pruned at delivery.ChangeRetention and oldest first, so a cursor whose own
// row is gone may have passed over rows it never read. That is a backfill (legionProjects): the
// rows those changes would have named cannot be known, so every project is computed whole.
func readChangePage(ctx context.Context, q Querier, after int64, limit int) (feed, bool, error) {
	changes, err := delivery.ChangesSince(ctx, q, after, limit)
	if err != nil {
		return feed{}, false, fmt.Errorf("read the delivery changes after %d: %w", after, err)
	}
	page := feed{position: after, full: len(changes) == limit}
	repos, refs := make([]string, 0, len(changes)), make([]string, 0, len(changes))
	for _, change := range changes {
		page.position = change.Seq
		if change.Kind != pullRequestChange {
			continue
		}
		repos = append(repos, change.Repo)
		refs = append(refs, change.Ref)
	}
	lost, err := changesLost(ctx, q, after)
	if err != nil {
		return feed{}, false, err
	}
	if len(repos) == 0 {
		return page, lost, nil
	}
	rows, err := q.Query(ctx, `
		select distinct issue.project_key, request.issue_key
		from unnest($1::text[], $2::text[]) as given(repo, ref)
		join delivery_pull_requests request on request.repo = given.repo and request.number::text = given.ref
		join issues issue on issue.key = request.issue_key
	`, repos, refs)
	if err != nil {
		return feed{}, false, fmt.Errorf("read the issues of %d changed pull requests: %w", len(repos), err)
	}
	defer rows.Close()
	for rows.Next() {
		var touched touch
		if err := rows.Scan(&touched.project, &touched.issue); err != nil {
			return feed{}, false, fmt.Errorf("read the issues of %d changed pull requests: %w", len(repos), err)
		}
		page.touches = append(page.touches, touched)
	}
	if err := rows.Err(); err != nil {
		return feed{}, false, fmt.Errorf("read the issues of %d changed pull requests: %w", len(repos), err)
	}
	return page, lost, nil
}

// changesLost reports whether the change at the cursor is gone from the log, which is a cursor
// that may have lost rows it never read. A cursor of 0 has read nothing and has lost nothing.
func changesLost(ctx context.Context, q Querier, cursor int64) (bool, error) {
	if cursor == 0 {
		return false, nil
	}
	at, err := delivery.ChangeAt(ctx, q, cursor)
	if err != nil {
		return false, fmt.Errorf("read the delivery change at %d: %w", cursor, err)
	}
	return at == nil, nil
}

// legionProjects is every project this loop computes, by key, each with whether its facts are to
// be computed whole on this pass. A project is one of them once a Legion daemon has been read of
// it (its sampler's observations) or once it holds a stored row. An event of a project neither
// answers for is no work of this loop: its first observation brings the project in, and the
// backfill then computes it.
//
// A project with no stored row has never been computed, and every project at all has lost its
// delivery changes when lost says the change cursor fell below the log. Either is a backfill: a
// pass of this same loop that runs ComputeProject over the project rather than Compute over a
// key set. The first run on an empty table is that case and needs no second code path.
func legionProjects(ctx context.Context, q Querier, lost bool) (map[string]bool, error) {
	rows, err := q.Query(ctx, `
		select project.key, not exists (select 1 from issue_facts where project_key = project.key)
		from projects project
		where exists (select 1 from legion_observations where project = project.key)
		   or exists (select 1 from issue_facts where project_key = project.key)
	`)
	if err != nil {
		return nil, fmt.Errorf("read the projects Legion works: %w", err)
	}
	defer rows.Close()
	projects := map[string]bool{}
	for rows.Next() {
		var key string
		var backfill bool
		if err := rows.Scan(&key, &backfill); err != nil {
			return nil, fmt.Errorf("read the projects Legion works: %w", err)
		}
		projects[key] = backfill || lost
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the projects Legion works: %w", err)
	}
	return projects, nil
}

// openStoredRows is the stored row of every open issue, by project: what the sweep recomputes,
// because an open issue's hours grow with the clock while no input moves. A closed issue's facts
// stand still, so the sweep passes over it and Write leaves its row alone.
//
// It reads the rows rather than the issues: an open issue with no stored row carries no Legion
// phase work yet, or has never been computed, and its own event or the backfill answers for it.
func openStoredRows(ctx context.Context, q Querier) (map[string][]string, error) {
	rows, err := q.Query(ctx, `select project_key, issue_key from issue_facts where closed_at is null`)
	if err != nil {
		return nil, fmt.Errorf("read the open issues' stored facts: %w", err)
	}
	defer rows.Close()
	open := map[string][]string{}
	for rows.Next() {
		var project, issue string
		if err := rows.Scan(&project, &issue); err != nil {
			return nil, fmt.Errorf("read the open issues' stored facts: %w", err)
		}
		open[project] = append(open[project], issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the open issues' stored facts: %w", err)
	}
	return open, nil
}

// parentLinks is every issue of the project by its parent's key, and "" for an issue with no
// parent: the links rowsTouchedBy walks.
func parentLinks(ctx context.Context, q Querier, project string) (map[string]string, error) {
	rows, err := q.Query(ctx, `select key, coalesce(parent_key, '') from issues where project_key = $1`, project)
	if err != nil {
		return nil, fmt.Errorf("read the parent links of %s: %w", project, err)
	}
	defer rows.Close()
	parents := map[string]string{}
	for rows.Next() {
		var key, parent string
		if err := rows.Scan(&key, &parent); err != nil {
			return nil, fmt.Errorf("read the parent links of %s: %w", project, err)
		}
		parents[key] = parent
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the parent links of %s: %w", project, err)
	}
	return parents, nil
}

// rowsToCompute is the whole set one pass computes for a project, sorted: the rows the changes
// touch through the issues' links now (rowsTouchedBy) and the rows of the tree a moved issue
// left (leftTrees). It is the set the pass states before it writes, and it writes no row
// outside it.
func rowsToCompute(ctx context.Context, q Querier, project string, keys []string, moved map[string]bool, parent map[string]string) ([]string, error) {
	touched := rowsTouchedBy(keys, moved, parent)
	left, err := leftTrees(ctx, q, project, keys, moved, parent)
	if err != nil {
		return nil, err
	}
	if len(left) == 0 {
		return touched, nil
	}
	whole := make(map[string]bool, len(touched)+len(left))
	for _, key := range touched {
		whole[key] = true
	}
	for _, key := range left {
		whole[key] = true
	}
	return slices.Sorted(maps.Keys(whole)), nil
}

// leftTrees is every stored row of the tree a moved issue left, sorted. An issue whose parent
// link moved out of one tree and into another takes its own hours with it, so the old tree's
// root goes on counting the time it waited while that issue worked until its row is computed
// again. A closed root is never recomputed by the sweep, which recomputes the open issues
// alone, so a pass that left the old tree out would leave that row wrong for good.
//
// The old tree is the tree as the last computation recorded it: the moved issue's stored
// root_key, where it differs from the root its links give now (rootOf), and every issue the
// table holds under that root. Nothing else can name it, because the links the pass reads are
// the links after the move. It is one query, and only where an issue moved, which is rare.
func leftTrees(ctx context.Context, q Querier, project string, keys []string, moved map[string]bool, parent map[string]string) ([]string, error) {
	wasMoved := make([]string, 0, len(moved))
	for _, key := range keys {
		if moved[key] {
			wasMoved = append(wasMoved, key)
		}
	}
	if len(wasMoved) == 0 {
		return nil, nil
	}
	stored, err := q.Query(ctx, `
		select issue_key, root_key from issue_facts where project_key = $1 and issue_key = any($2::text[])
	`, project, wasMoved)
	if err != nil {
		return nil, fmt.Errorf("read the stored roots of %d moved issues of %s: %w", len(wasMoved), project, err)
	}
	roots := []string{}
	for stored.Next() {
		var key, root string
		if err := stored.Scan(&key, &root); err != nil {
			stored.Close()
			return nil, fmt.Errorf("read the stored roots of %d moved issues of %s: %w", len(wasMoved), project, err)
		}
		if root != "" && root != rootOf(key, parent) {
			roots = append(roots, root)
		}
	}
	stored.Close()
	if err := stored.Err(); err != nil {
		return nil, fmt.Errorf("read the stored roots of %d moved issues of %s: %w", len(wasMoved), project, err)
	}
	if len(roots) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select issue_key from issue_facts
		where project_key = $1 and (root_key = any($2::text[]) or issue_key = any($2::text[]))
	`, project, roots)
	if err != nil {
		return nil, fmt.Errorf("read the trees %v of %s: %w", roots, project, err)
	}
	defer rows.Close()
	left := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("read the trees %v of %s: %w", roots, project, err)
		}
		left = append(left, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the trees %v of %s: %w", roots, project, err)
	}
	slices.Sort(left)
	return left, nil
}

// rootOf is the top of the issue's parent chain as the links stand now, and the issue itself
// where it has no parent. The walk keeps the keys it has visited, so a chain that loops stops.
func rootOf(key string, parent map[string]string) string {
	climbed := map[string]bool{key: true}
	for above := parent[key]; above != "" && !climbed[above]; above = parent[above] {
		climbed[above] = true
		key = above
	}
	return key
}

// rowsTouchedBy is every issue whose stored row a change to one of keys can move, sorted: the
// issues themselves, their ancestors, and the whole subtree of an issue whose parent link moved.
//
// An ancestor's row moves because an issue's hours carry the time it waited while a child
// worked, so a child's own change moves every row above it. A subtree moves where the issue's
// parent link moved, because its root and its tree depth are read from the chain above it, which
// that one link changed for every issue below as well. moved names the issues an event said
// moved their parent (readEventPage), and parent is the project's links (parentLinks).
//
// It reads the links as they stand now, so it cannot name the tree a moved issue left: that one
// is read from the stored rows beside it (leftTrees), and rowsToCompute is the two together.
//
// Each walk keeps the keys it has already visited, so a parent chain that loops stops rather
// than running for ever, as the one tree rule does (legion.TreeChain).
func rowsTouchedBy(keys []string, moved map[string]bool, parent map[string]string) []string {
	touched := make(map[string]bool, len(keys))
	climbed := map[string]bool{}
	for _, key := range keys {
		touched[key] = true
		clear(climbed)
		climbed[key] = true
		for above := parent[key]; above != "" && !climbed[above]; above = parent[above] {
			climbed[above] = true
			touched[above] = true
		}
	}
	descending := make([]string, 0, len(moved))
	for _, key := range keys {
		if moved[key] {
			descending = append(descending, key)
		}
	}
	if len(descending) > 0 {
		children := map[string][]string{}
		for key, above := range parent {
			if above != "" {
				children[above] = append(children[above], key)
			}
		}
		walked := make(map[string]bool, len(descending))
		for len(descending) > 0 {
			key := descending[len(descending)-1]
			descending = descending[:len(descending)-1]
			if walked[key] {
				continue
			}
			walked[key] = true
			for _, below := range children[key] {
				touched[below] = true
				descending = append(descending, below)
			}
		}
	}
	return slices.Sorted(maps.Keys(touched))
}

// readCursors is where each feed stands, by cursor name. A name with no row stands at 0, which
// is a feed this database has read nothing of.
func readCursors(ctx context.Context, q Querier) (map[string]int64, error) {
	rows, err := q.Query(ctx, `select name, position from issue_facts_cursor where name = any($1::text[])`, cursorNames)
	if err != nil {
		return nil, fmt.Errorf("read the Legion facts' cursors: %w", err)
	}
	defer rows.Close()
	cursors := make(map[string]int64, len(cursorNames))
	for rows.Next() {
		var name string
		var position int64
		if err := rows.Scan(&name, &position); err != nil {
			return nil, fmt.Errorf("read the Legion facts' cursors: %w", err)
		}
		cursors[name] = position
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the Legion facts' cursors: %w", err)
	}
	return cursors, nil
}

// advanceCursors moves the three cursors to the positions this page read, stamped with the
// moment its rows were computed as of, which is the moment CursorAge measures the stored facts
// from. A cursor whose feed held nothing new keeps its position and takes the new stamp: the
// pass covered the feed up to there, and the stamp is what says how fresh the facts are.
//
// It runs in the transaction that wrote the rows the positions cover (writePass).
func advanceCursors(ctx context.Context, tx pgx.Tx, positions map[string]int64, now time.Time) error {
	at := make([]int64, len(cursorNames))
	for index, name := range cursorNames {
		at[index] = positions[name]
	}
	_, err := tx.Exec(ctx, `
		insert into issue_facts_cursor (name, position, updated_at)
		select name, position, $3 from unnest($1::text[], $2::bigint[]) as given(name, position)
		on conflict (name) do update set position = excluded.position, updated_at = excluded.updated_at
	`, cursorNames, at, now)
	if err != nil {
		return fmt.Errorf("move the Legion facts' cursors: %w", err)
	}
	return nil
}

// CursorAge answers how long ago each cursor last moved, as the oldest of the three cursors'
// updated_at measured against now, and false when no cursor row exists at all, which is a
// database no pass has committed on. It is what the board's header reads to say how fresh the
// served facts are.
//
// Every pass stamps all three cursors with the moment it computed as of, whether or not that
// feed held anything new, so the age is how long ago the last pass ran and not how long ago the
// last change arrived. A caller whose clock stands behind the writing process's reads a negative
// age, as it would of any moment another process stamped.
func CursorAge(ctx context.Context, q Querier, now time.Time) (time.Duration, bool, error) {
	var oldest *time.Time
	if err := q.QueryRow(ctx, `select min(updated_at) from issue_facts_cursor`).Scan(&oldest); err != nil {
		return 0, false, fmt.Errorf("read the age of the Legion facts' cursors: %w", err)
	}
	if oldest == nil {
		return 0, false, nil
	}
	return now.Sub(*oldest), true, nil
}
