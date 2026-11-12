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
	"github.com/sjawhar/envoy/internal/dispatch/legion"
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
// of the facts' own rig, 210 of them with Legion phase work: one ComputeProject and one
// WriteProject of the whole project take 115 ms to 205 ms, and one sweep pass that finds nothing
// new 45 ms to 80 ms (TestTheSweepsPaceHoldsTheFreshnessRequirement prints both, over five
// runs). A pass may therefore grow to about 380 times its measured length before a stored row
// can stand a minute old.
//
// The sweep also reads the rows whose issue lost its Legion phase work (staleStoredRows), which
// it did not read before dispatch://INFRA-2915's fourth round. That read costs a sweep pass
// about 10 ms over those 252 issues and about 20 ms over the 1,008 of the same test's larger
// population, measured as the difference between three runs of the test with the read and three
// without it.
//
// Every pass that computes a project narrowly also reads the `tree` the daemon reported of each
// of its issues beside the parent links (treeLinks), which it did not read before this round.
// That read costs about 1 ms over the rig's 252 issues and 2,142 observations, measured against
// the parent links alone over five reads of one seeded history.
//
// A page that reads a `tree` observation now marks that issue moved, as it marks an issue whose
// parent link moved (touch), so it makes the two reads of leftTrees over that issue's project:
// one row of issue_facts for the moved issue's stored root, and the rows of that project held
// under that root. Over the rig's eight-tree history the second read answers five rows, which
// is a root and the four issues of its tree that carry Legion phase work, and the set the pass
// states grows from two keys to six: the issue, the root it joined, and those five. A `tree`
// observation is written only where the tree the daemon reports differs from the newest one
// recorded (legion.RecordObservations), so a daemon that goes on reporting one tree costs a
// pass nothing.
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
	// Deliveries is signalled when a delivery fact changed: the hub's wake on
	// delivery.ChangesChannel.
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
//     recomputes the stored rows of the project's open issues (openStoredRows), and the stored
//     rows whose issue carries no Legion phase work any more, which no feed names at all
//     (staleStoredRows).
//
// A pass that fails logs and leaves its cursors where they were, so the next pass reads the same
// rows again. Every pass is safe to run twice: a row whose values did not move is not rewritten
// (Upsert), and a cursor moves only in the transaction that wrote the rows it covers (passPage).
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
// computes the rows those pages and the sweep name, and writes those rows, the deletion of the
// stale rows inside the set it stated, and the three cursors in one transaction. It answers the
// keys whose stored rows it changed and whether a page was full, which means more rows are
// waiting.
//
// sweep says whether this page carries the work no feed announces: the recomputation of the open
// rows, the recomputation of the rows whose issue lost its Legion phase work, and the backfill
// of a project a feed cannot answer for. Only the first page of a pass carries it, because all
// three read the whole of what they cover.
//
// A page that acted on a lost change cursor leaves that cursor at the head of the change log
// rather than where it stood, and that feed is then not full however much of it the page read.
// The loss is answered by computing every project whole, which covers every change the log ever
// held, so the cursor has nothing left to read below the head; a cursor left where it stood
// would read lost again on the next pass and compute every project whole every SweepEvery for
// as long as no change commits.
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
	read, err := readPages(ctx, deps.Pool, cursors, safe, sched.page)
	if err != nil {
		return nil, false, err
	}
	// Only a sweep page acts on a loss: the backfill a loss calls for is set under the sweep
	// below, so a loss a page of any other kind meets is answered by the next pass's first page.
	lost := sweep && read.lost
	if lost {
		head, err := delivery.LatestChangeSeq(ctx, deps.Pool)
		if err != nil {
			return nil, false, fmt.Errorf("read the head of the delivery changes: %w", err)
		}
		read.changes.position, read.changes.full = head, false
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
	for _, page := range []feed{read.events, read.observations, read.changes} {
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
		stale, err := staleStoredRows(ctx, deps.Pool)
		if err != nil {
			return nil, false, err
		}
		for _, swept := range []map[string][]string{open, stale} {
			for project, keys := range swept {
				if work := needed(project); work != nil {
					for _, key := range keys {
						work.keys[key] = true
					}
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

	// Every project this page owes work for is kept, whatever its computation answered. A
	// project whose issues have all lost their Legion phase work answers no row at all and
	// still owes the deletion of the stale rows its stated set holds, which is the one pass
	// that takes those rows away.
	computed := []ProjectWrite{}
	for _, project := range slices.Sorted(maps.Keys(work)) {
		rows, err := computeProjectWork(ctx, deps.Pool, project, work[project], now)
		if err != nil {
			return nil, false, err
		}
		computed = append(computed, rows)
	}

	positions := map[string]int64{
		eventsCursor:       read.events.position,
		observationsCursor: read.observations.position,
		changesCursor:      read.changes.position,
	}
	changed, err := writePass(ctx, deps.Pool, computed, positions, now)
	if err != nil {
		return changed, false, err
	}
	return changed, read.events.full || read.observations.full || read.changes.full, nil
}

// pages is what one page of a pass read of the three feeds, and whether the change cursor had
// fallen below the oldest change the log still holds.
type pages struct {
	events       feed
	observations feed
	changes      feed
	lost         bool
}

// readPages is one page of each of the three feeds, read from where each cursor stands.
//
// A database holding no cursor row at all has committed no pass, because every pass writes the
// three together (advanceCursors), so every feed would be read from its start. It is read from
// its head instead (readFeedHeads): the first page of such a pass computes every project whole,
// because no project has a stored row, and a whole project is computed from the history itself
// rather than from the feeds, so the pages below that head hold nothing the backfill has not
// already answered. Reading them would recompute every project once per page - about 813 pages
// of a log the size of INFRA's - and no sweep runs until the pass ends, so the open rows would
// stand still while it replayed.
func readPages(ctx context.Context, q Querier, cursors map[string]int64, safe int64, limit int) (pages, error) {
	if len(cursors) == 0 {
		return readFeedHeads(ctx, q, safe)
	}
	read := pages{}
	var err error
	if read.events, err = readEventPage(ctx, q, cursors[eventsCursor], safe, limit); err != nil {
		return pages{}, err
	}
	if read.observations, err = readObservationPage(ctx, q, cursors[observationsCursor], limit); err != nil {
		return pages{}, err
	}
	if read.changes, read.lost, err = readChangePage(ctx, q, cursors[changesCursor], limit); err != nil {
		return pages{}, err
	}
	return read, nil
}

// readFeedHeads is the three feeds standing at their heads and naming no issue: where the
// cursors of a database no pass has committed on start. The events feed's head is the safe
// horizon rather than the newest id, because an event above the horizon may still be committing
// and a later pass reads it (readEventPage).
func readFeedHeads(ctx context.Context, q Querier, safe int64) (pages, error) {
	var observations int64
	if err := q.QueryRow(ctx, `select coalesce(max(id), 0) from legion_observations`).Scan(&observations); err != nil {
		return pages{}, fmt.Errorf("read the head of the Legion observations: %w", err)
	}
	changes, err := delivery.LatestChangeSeq(ctx, q)
	if err != nil {
		return pages{}, fmt.Errorf("read the head of the delivery changes: %w", err)
	}
	return pages{
		events:       feed{position: safe},
		observations: feed{position: observations},
		changes:      feed{position: changes},
	}, nil
}

// projectWork is what one page owes one project: the issues its feeds named, those of them a
// row moved between trees (touch), and whether the project is to be computed whole (a
// backfill).
type projectWork struct {
	keys     map[string]bool
	moved    map[string]bool
	backfill bool
}

// computeProjectWork computes the rows one project owes and answers them as the write of that
// project (ProjectWrite): the rows, the set the pass stated for them, and whether the pass read
// the project whole. A backfill reads the whole project (ComputeProject) and the keys its
// feeds named are left out of the call, because the whole project already holds them: its
// stated set is the project itself. Every other project is computed over the rows its changes
// touch (rowsToCompute), and no row outside that set is written or deleted.
func computeProjectWork(ctx context.Context, pool *store.Pool, project string, work *projectWork, now time.Time) (ProjectWrite, error) {
	if work.backfill {
		rows, err := ComputeProject(ctx, pool, project, now)
		if err != nil {
			return ProjectWrite{}, err
		}
		return ProjectWrite{Project: project, Whole: true, Rows: rows}, nil
	}
	above, err := treeLinks(ctx, pool, project)
	if err != nil {
		return ProjectWrite{}, err
	}
	keys, err := rowsToCompute(ctx, pool, project, slices.Sorted(maps.Keys(work.keys)), work.moved, above)
	if err != nil {
		return ProjectWrite{}, err
	}
	rows, err := Compute(ctx, pool, project, keys, now)
	if err != nil {
		return ProjectWrite{}, err
	}
	return ProjectWrite{Project: project, Stated: keys, Rows: rows}, nil
}

// writePass writes every project's rows, deletes the stale rows inside the set each project's
// pass stated, records the projects it computed whole and moves the three cursors, all in one
// transaction. It answers the keys whose stored rows changed, the deleted keys among them,
// because a row that went is a change of the stored facts as much as a row that moved. A
// failure leaves the cursors and the marks where they were, so the next pass reads the same
// rows again.
func writePass(ctx context.Context, pool *store.Pool, computed []ProjectWrite, positions map[string]int64, now time.Time) ([]string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("open the Legion facts' write: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	changed := []string{}
	for _, project := range computed {
		written, err := WriteProject(ctx, tx, project)
		if err != nil {
			return nil, err
		}
		changed = append(changed, written...)
	}
	if err := markComputedWhole(ctx, tx, computed, now); err != nil {
		return nil, err
	}
	if err := advanceCursors(ctx, tx, positions, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit the Legion facts: %w", err)
	}
	return changed, nil
}

// markComputedWhole records that each project this write read whole was computed whole as of now
// and how many rows that computation answered, which is what legionProjects reads a project's
// backfill from. It runs in the transaction that wrote those rows (writePass), so a pass that
// failed leaves no mark and the next pass reads the project whole again.
//
// It takes the writes themselves and reads Whole off each one, rather than the projects and the
// counts as two arrays its caller filled: the rows a project answered stand beside the project
// in one value, so nothing has to hold two lists in step.
//
// The mark is a record rather than the absence of a stored row, because a project whose issues
// carry no Legion phase work is computed whole and answers no row at all: read from an absence,
// such a project would be computed whole again every SweepEvery for ever. The count is what
// still answers for the rows of a project going missing under the consumer, which is the other
// thing an absence used to say (migration 20261111153000).
func markComputedWhole(ctx context.Context, tx pgx.Tx, computed []ProjectWrite, now time.Time) error {
	projects := []string{}
	answered := []int32{}
	for _, write := range computed {
		if !write.Whole {
			continue
		}
		projects = append(projects, write.Project)
		answered = append(answered, int32(len(write.Rows)))
	}
	if len(projects) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		insert into issue_facts_computed (project_key, computed_at, computed_rows)
		select project, $3, rows from unnest($1::text[], $2::integer[]) as given(project, rows)
		on conflict (project_key) do update
		set computed_at = excluded.computed_at, computed_rows = excluded.computed_rows
	`, projects, answered, now)
	if err != nil {
		return fmt.Errorf("record the %d projects computed whole: %w", len(projects), err)
	}
	return nil
}

// feed is what one page of one feed read: where that feed now stands, the issues it named and
// whether the page was full.
type feed struct {
	position int64
	touches  []touch
	full     bool
}

// touch is one issue a feed named: the project it belongs to, the issue itself, and whether the
// row that named it moves the issue between trees. Two rows of two feeds move an issue that
// way, and they are one kind of change: Dispatch's parent link, which an event's `changed`
// names (readEventPage), and the `tree` the daemon reports, which is the field of an
// observation (readObservationPage). The rule that places an issue in a tree reads the `tree`
// first and the parent link only where no daemon reported one (legion.TreeChain), so either
// row can take the issue out of one tree and put it in another.
//
// A move states two trees: the tree the issue joined, which the links now give (rowsTouchedBy),
// and the tree it left, which is read from the issue's stored root_key (leftTrees).
type touch struct {
	project string
	issue   string
	moved   bool
}

// readEventPage is the page of events after the cursor: up to limit events above after and at or
// below safe, with the issues those events name and the issues the sessions those events name
// have written on (sessionIssues).
//
// The page stops at safe because events.id is not commit order. An event draws its id when its
// write inserts the row, and two writes that wait on nothing commit out of id order, so a cursor
// moved by an uncapped `id > cursor` read could pass an event that was still committing and
// never read it (events, the "Safe horizon" section). An event above safe is read by a later
// pass: its own commit notifies the events channel, and a pass falls every SweepEvery anyway.
//
// `changed` holds the fields an issue write moved (api's issueChangedFields), so an event naming
// `parent` is the parent link moving. That is one of the two rows that move an issue between
// trees (touch), and a move widens the set a change touches from the issue and its ancestors to
// the issue's whole subtree (rowsTouchedBy) and to the tree the issue left (leftTrees).
//
// An event that is on no issue at all is kept where its actor is a session, because such an
// event carries that session's title as much as an event on an issue does, and a session's
// newest title decides which issues every event of that session works (legion.PhaseWork). A
// session that writes on a project or on an artifact therefore moves the facts of the issues it
// wrote on, and no issue of its own names it. The event's own project is not the work: the
// issues are read from the session (sessionIssues).
func readEventPage(ctx context.Context, q Querier, after, safe int64, limit int) (feed, error) {
	rows, err := q.Query(ctx, `
		select event.id, coalesce(issue.project_key, ''), coalesce(event.issue_key, ''),
		       coalesce(event.payload -> 'changed' @> '["parent"]', false),
		       case when event.actor ->> 'kind' = 'session' then coalesce(event.actor ->> 'id', '') else '' end
		from events event
		left join issues issue on issue.key = event.issue_key
		where event.id > $1 and event.id <= $2
		order by event.id
		limit $3
	`, after, safe, limit)
	if err != nil {
		return feed{}, fmt.Errorf("read the events after %d: %w", after, err)
	}
	defer rows.Close()
	page := feed{position: after}
	count := 0
	sessions := map[string]bool{}
	for rows.Next() {
		var touched touch
		var session string
		if err := rows.Scan(&page.position, &touched.project, &touched.issue, &touched.moved, &session); err != nil {
			return feed{}, fmt.Errorf("read the events after %d: %w", after, err)
		}
		count++
		if session != "" {
			sessions[session] = true
		}
		if touched.project != "" {
			page.touches = append(page.touches, touched)
		}
	}
	if err := rows.Err(); err != nil {
		return feed{}, fmt.Errorf("read the events after %d: %w", after, err)
	}
	page.full = count == limit
	worked, err := sessionIssues(ctx, q, slices.Sorted(maps.Keys(sessions)))
	if err != nil {
		return feed{}, err
	}
	page.touches = append(page.touches, worked...)
	return page, nil
}

// sessionIssues is every issue the named sessions have written an event on, with the project of
// each: the issues one page of events can have moved the facts of through the titles its
// sessions carry.
//
// A session's newest title decides which of the issues it wrote on carry its work
// (legion.PhaseWork), so one write anywhere by one session can take the work off one issue and
// give it to another. Both issues are in this answer, and neither is named by the event itself:
// the issue that lost the work has no event of its own, and the issue that gained it is named
// by a title rather than by a key. They join the keys the pass computes, and nothing downstream
// treats them differently - a row is written or deleted only because the computation of that
// issue said so.
//
// It reads events_session_actor_created_at_idx (migration 0026), the partial index on
// ((actor->>'id'), created_at desc) for a session actor: one scan of that index for each session
// the page named, over every event that session ever wrote, because the read has no order and no
// limit and keeps every issue it finds. The index bounds it to the sessions the page named and
// not to the events the page read, which is the trap delivery/agents.go documents of the same
// index. The set the pass then computes is therefore every issue those sessions ever wrote on,
// not the issues of this page: a session that moved its title through ten issues names all ten,
// and the computation of each decides whether its row is written, deleted or left alone.
// legion.PhaseWorkKeys would answer the same pairs and reads the session half of the whole log
// to do it, which is why a page does not call it.
func sessionIssues(ctx context.Context, q Querier, sessions []string) ([]touch, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select distinct issue.project_key, event.issue_key
		from events event
		join issues issue on issue.key = event.issue_key
		where event.actor ->> 'id' = any($1::text[])
		  and event.actor ->> 'kind' = 'session'
	`, sessions)
	if err != nil {
		return nil, fmt.Errorf("read the issues %d sessions wrote on: %w", len(sessions), err)
	}
	defer rows.Close()
	worked := []touch{}
	for rows.Next() {
		var touched touch
		if err := rows.Scan(&touched.project, &touched.issue); err != nil {
			return nil, fmt.Errorf("read the issues %d sessions wrote on: %w", len(sessions), err)
		}
		worked = append(worked, touched)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the issues %d sessions wrote on: %w", len(sessions), err)
	}
	return worked, nil
}

// readObservationPage is the page of the daemon's observations after the cursor: up to limit
// rows above after, each naming its project, its issue and whether it moves that issue between
// trees.
//
// An observation of legion.FieldTree is the daemon naming the issue's tree, and that is a move
// of the same kind as a parent link moving: it takes the issue out of the tree it stood in and
// puts it in the tree the field names. It is marked as one, so the pass states the tree the
// issue left beside the tree it joined (leftTrees), and no rule of its own is needed.
//
// An observation that repeats the tree the issue already had is marked a move as well, and the
// move then states the same tree twice: the tree the stored root_key records is the tree the
// links give. That costs the two reads of leftTrees over one tree's rows and writes no wrong
// row, because every row of the set is computed before it is written. A comparison that skipped
// those reads is not a comparison this pass can make: the stored root_key is the only record of
// where the issue was, and reading it is the work leftTrees does.
//
// It needs no horizon, and that is the one way this cursor differs from the events cursor. One
// leased sampler writes legion_observations (legion.RunSampler, one pass a minute, one process
// at a time), so no second writer can hold a lower id open while a higher one commits, and the
// ids a reader pages by are commit order.
func readObservationPage(ctx context.Context, q Querier, after int64, limit int) (feed, error) {
	rows, err := q.Query(ctx, `
		select id, project, issue_key, field = $2
		from legion_observations
		where id > $1
		order by id
		limit $3
	`, after, legion.FieldTree, limit)
	if err != nil {
		return feed{}, fmt.Errorf("read the Legion observations after %d: %w", after, err)
	}
	defer rows.Close()
	page := feed{position: after}
	for rows.Next() {
		var touched touch
		if err := rows.Scan(&page.position, &touched.project, &touched.issue, &touched.moved); err != nil {
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
// A project is a backfill - a pass of this same loop that runs ComputeProject over the project
// rather than Compute over a key set - where its mark says it was never computed whole, where
// the rows that mark counted have gone, or where lost says the change cursor fell below the
// log, which loses the changes of every project at once. The first run on an empty table is
// the first of those cases and needs no second code path.
//
// The mark is a row of issue_facts_computed, which the pass that read the project whole writes
// (markComputedWhole). "Never computed" is read from that record and not from "this project
// holds no stored row", because a project whose issues carry no Legion phase work is computed
// whole and answers no row: read from the absence of a row, such a project would be computed
// whole on every pass for as long as it held no worked issue, which is the whole-project read
// this loop exists to make rare.
//
// The count the mark carries is what still answers for a project whose stored rows went
// missing under the consumer, an emptied table among them: a project whose last whole read
// answered rows and which now holds none has lost them to something this loop did not do, and
// no narrow pass can name rows that are not there. It costs one whole read, because that read
// writes the count it answered: a project that truly holds no row any more is marked with none
// and is left alone from then on.
func legionProjects(ctx context.Context, q Querier, lost bool) (map[string]bool, error) {
	rows, err := q.Query(ctx, `
		select project.key,
		       mark.project_key is null
		       or (mark.computed_rows > 0
		           and not exists (select 1 from issue_facts where project_key = project.key))
		from projects project
		left join issue_facts_computed mark on mark.project_key = project.key
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
// stand still, so the sweep passes over it and WriteProject leaves its row alone.
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

// staleStoredRows is the stored row of every issue that a session wrote on and that carries no
// Legion phase work any more, by project: the second set the sweep adds to its work, beside the
// open rows.
//
// A stored row means Legion worked the issue, and an issue loses its phase work as well as
// gains it: a role session that writes next on another issue under that issue's title takes the
// work with it (legion.PhaseWork). Nothing else a pass reads names the issue it left. No event
// is written on that issue, the links do not reach it, and the sweep reads it among the open
// rows only while it is open, so the row of a closed issue would stand for ever. The key is
// therefore read from what the table already holds, as the tree a moved issue left is read
// from the stored root_key (leftTrees), and it joins the set the pass states. It is one
// more source of keys of the same kind: the computation decides whether the issue still carries
// work, and the deletion inside the stated set takes the row only where the computation
// answered it none.
//
// A row whose issue no session ever wrote on is left where it stands. Such a row is no row this
// loop can have written, because the computation answers a row for an issue a handoff names or
// a role session worked, and a handoff is never taken back. The whole-project pass is what
// answers for a row of that kind (deleteStaleProjectRows), as it does for a row whose key names
// no issue at all.
//
// Both reads are of the whole deployment and run once a sweep, which is what their cost is
// measured against. legion.PhaseWorkKeys is the one read of the phase-work rule, and the
// computation of every project of the pass makes it anyway (metrics.Population). `select
// project_key, issue_key from issue_facts` reads two narrow columns of the whole table, which
// Postgres answers with one sequential scan: 26 shared buffers and 0.3 ms over the 1,008 rows
// of the freshness test's larger population, and 519 buffers and 8 ms over 20,000 rows, under
// explain (analyze, buffers). Neither read grows with the issues the pass computes.
func staleStoredRows(ctx context.Context, pool *store.Pool) (map[string][]string, error) {
	work, err := legion.PhaseWorkKeys(ctx, pool)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `select project_key, issue_key from issue_facts`)
	if err != nil {
		return nil, fmt.Errorf("read the stored facts of the issues that lost their Legion phase work: %w", err)
	}
	defer rows.Close()
	stale := map[string][]string{}
	for rows.Next() {
		var project, issue string
		if err := rows.Scan(&project, &issue); err != nil {
			return nil, fmt.Errorf("read the stored facts of the issues that lost their Legion phase work: %w", err)
		}
		if _, works := work.Keys[issue]; works {
			continue
		}
		if _, wrote := work.SessionIssues[issue]; !wrote {
			continue
		}
		stale[project] = append(stale[project], issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the stored facts of the issues that lost their Legion phase work: %w", err)
	}
	return stale, nil
}

// treeLinks is every issue of the project by the issue above it, and "" for an issue nothing
// stands above: the links rowsTouchedBy climbs and descends.
//
// The issue above another is the `tree` the daemon reported for it, and the parent Dispatch
// records where the daemon reported none. That is the one rule that places an issue in a tree
// (legion.TreeChain), and the rule the computation this set is stated for reads as well
// (metrics.treeScopeKeys). A walk of the parent links alone is a second rule, and it answers
// another tree: it leaves out the root a child's `tree` observation names, and the hours a
// root waited while a child worked are taken out of the hours its row holds
// (legion/metrics/segments.go measures them, legion/metrics/breakdown.go takes them out). The
// root's stored row would then stand wrong until a whole-project pass computed it again.
//
// An issue the daemon reports as its own tree is a root, so nothing stands above it. A key the
// project does not hold has no entry, so a walk that reaches it stops.
//
// It is one read of two sources: the project's issues, and the newest `tree` the sampler
// recorded of each of them, which is the row the Legion view and the metrics read too
// (metrics.readReported). The rows under legion.AdmissionKey hold the daemon's settings and
// belong to no issue, and the join to the project's issues leaves them out.
//
// The subquery says `materialized` because the planner otherwise inlines it and runs it once
// per issue. Over the facts' own rig, 252 issues against 2,142 observations, that is 252 index
// scans of the project's observations and 48 ms of the pass, against the 1 ms the one scan
// here takes.
func treeLinks(ctx context.Context, q Querier, project string) (map[string]string, error) {
	rows, err := q.Query(ctx, `
		with reported as materialized (
			select issue_key, (array_agg(value::text order by observed_at desc, id desc))[1] as tree
			from legion_observations
			where project = $1 and field = $2
			group by issue_key
		)
		select issue.key, coalesce(issue.parent_key, ''), coalesce(reported.tree, '')
		from issues issue
		left join reported on reported.issue_key = issue.key
		where issue.project_key = $1
	`, project, legion.FieldTree)
	if err != nil {
		return nil, fmt.Errorf("read the tree links of %s: %w", project, err)
	}
	defer rows.Close()
	above := map[string]string{}
	for rows.Next() {
		var key, parent, reported string
		if err := rows.Scan(&key, &parent, &reported); err != nil {
			return nil, fmt.Errorf("read the tree links of %s: %w", project, err)
		}
		tree := legion.JSONString([]byte(reported))
		switch {
		case tree == key:
			above[key] = ""
		case tree != "":
			above[key] = tree
		default:
			above[key] = parent
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the tree links of %s: %w", project, err)
	}
	return above, nil
}

// rowsToCompute is the whole set one pass computes for a project, sorted: the rows the changes
// touch through the issues' links now (rowsTouchedBy) and the rows of the tree a moved issue
// left (leftTrees). It is the set the pass states before it writes, and it writes no row
// outside it.
func rowsToCompute(ctx context.Context, q Querier, project string, keys []string, moved map[string]bool, above map[string]string) ([]string, error) {
	touched := rowsTouchedBy(keys, moved, above)
	left, err := leftTrees(ctx, q, project, keys, moved)
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

// leftTrees is every stored row of the tree a moved issue left, sorted. An issue a move took
// out of one tree and into another takes its own hours with it, so the old tree's root goes on
// counting the time it waited while that issue worked until its row is computed again. A closed
// root is never recomputed by the sweep, which recomputes the open issues alone, so a pass that
// left the old tree out would leave that row wrong for good.
//
// Two rows move an issue that way and both are marked the same (touch): an event whose
// `changed` names `parent`, and an observation of legion.FieldTree. This reads the tree left
// for either of them, because the tree rule reads the daemon's `tree` before the parent link
// (legion.TreeChain) and an issue leaves a tree whichever of the two moved it.
//
// The old tree is the tree as the last computation recorded it: the moved issue's stored
// root_key, and every issue the table holds under that root. Nothing else can name it, because
// the links the pass reads are the links after the move. It is two queries, and only where an
// issue moved. A move is uncommon: a parent link is moved by hand, and the sampler writes a
// `tree` observation only where the tree it reports differs from the newest one it recorded
// (legion.RecordObservations), so a daemon that goes on reporting one tree writes one row.
//
// The stored root is taken whether or not the links now give another one. There is one root
// rule, legion.TreeChain, which is what wrote that root_key, and a walk of the raw parent links
// is a second rule that can answer differently: it reads neither the daemon's tree nor the stop
// at a parent with no Legion work. Comparing the two to skip a read would be a guess between
// them. Taking the stored root always states a superset of the rows a comparison would state,
// which the scope rule allows: the set is stated before anything is written, and a row inside
// it is written or deleted only because the computation of that issue said so.
func leftTrees(ctx context.Context, q Querier, project string, keys []string, moved map[string]bool) ([]string, error) {
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
		select root_key from issue_facts
		where project_key = $1 and issue_key = any($2::text[]) and root_key <> ''
	`, project, wasMoved)
	if err != nil {
		return nil, fmt.Errorf("read the stored roots of %d moved issues of %s: %w", len(wasMoved), project, err)
	}
	roots := []string{}
	for stored.Next() {
		var root string
		if err := stored.Scan(&root); err != nil {
			stored.Close()
			return nil, fmt.Errorf("read the stored roots of %d moved issues of %s: %w", len(wasMoved), project, err)
		}
		roots = append(roots, root)
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

// rowsTouchedBy is every issue whose stored row a change to one of keys can move, sorted: the
// issues themselves, their ancestors, and the whole subtree of an issue a move took into
// another tree.
//
// An ancestor's row moves because an issue's hours carry the time it waited while a child
// worked, so a child's own change moves every row above it. A subtree moves where the issue
// moved, because its root and its tree depth are read from the chain above it, which the move
// changed for every issue below as well. moved names the issues a row of a feed moved between
// trees - a parent link an event's `changed` names, or a `tree` an observation names (touch) -
// and above is the issue above each issue of the project (treeLinks).
//
// It climbs and descends the one rule that places an issue in a tree, which reads the daemon's
// `tree` before any parent link, because the computation it states a set for reads that rule
// too (metrics.treeScopeKeys). The issues below an issue are therefore the issues that rule
// puts below it: the one whose `tree` names it as well as the one whose parent link does.
//
// It reads the links as they stand now, so it cannot name the tree a moved issue left: that one
// is read from the stored rows beside it (leftTrees), and rowsToCompute is the two together.
//
// Each walk keeps the keys it has already visited, so a chain that loops stops rather than
// running for ever, as the one tree rule does (legion.TreeChain).
func rowsTouchedBy(keys []string, moved map[string]bool, above map[string]string) []string {
	touched := make(map[string]bool, len(keys))
	climbed := map[string]bool{}
	for _, key := range keys {
		touched[key] = true
		clear(climbed)
		climbed[key] = true
		for up := above[key]; up != "" && !climbed[up]; up = above[up] {
			climbed[up] = true
			touched[up] = true
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
		for key, up := range above {
			if up != "" {
				children[up] = append(children[up], key)
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

// readCursors is where each feed stands, by cursor name. A name with no row stands at 0, which is
// a feed this database has read nothing of, and no row at all is a database no pass has committed
// on, which is what readPages reads its feeds' heads for.
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
