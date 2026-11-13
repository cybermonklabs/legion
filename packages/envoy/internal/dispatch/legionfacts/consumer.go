package legionfacts

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
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
// The sweep also reads the rows whose issue lost its Legion phase work (staleStoredRows). That
// read costs a sweep pass about 10 ms over those 252 issues and about 20 ms over the 1,008 of
// the same test's larger population, measured as the difference between three runs of the test
// with the read and three without it.
//
// Every pass that computes a project narrowly also reads the project's tree links
// (metrics.TreeLinks). That read takes about 4.5 ms over the rig's 252 issues and 2,142
// observations, best of five against the pod's test Postgres.
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
//
// Every pass also states the issues below each key it computes, because an input that names one
// issue can move where every issue under that issue stands (rowsTouchedBy). Over the rig's 252
// issues one event of a worker session states 2.8 keys on average and 6 at most, against 1.8
// and 2 before the pass descended. A sweep pass that finds nothing new states the same keys as
// before the descent: 84 over those 252 issues and 336 over the 1,008 of the larger population,
// because an open issue of the rig holds no issue below it. The descent costs no read a pass did
// not make before it. metrics.ReadIssues reads the whole tree of every key it is given
// (treeScopeKeys), so an issue below a key is read whether or not the set names it. upsert writes
// a row only where a value of that row moved. A row the descent added and the computation left
// where it stood is therefore written by nothing.
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

// settingsChange is the delivery_changes kind a repository's mapping logs, naming the repository
// (migration 20261113170752), which moves the repository a stored row resolves to (readChangePage).
const settingsChange = "settings"

// Deps is what Run needs of the server.
type Deps struct {
	// Pool is the database the facts are computed from and written to.
	Pool *store.Pool
	// Horizon is the process's one events.Horizon, the Horizon the live event feed reads
	// through: the events feed is paged by a global id, so it must stop at a safe horizon.
	Horizon *events.Horizon
	// Build names the build this process is, which the loop records against each project it
	// reads whole and compares on every pass after that (legionProjects). The server gives it
	// the commit the image build stamped into the binary, and a build no release stamped gives
	// it a string of that one process (cmd/dispatch's processBuild). The empty string is no
	// build: the loop refuses it where it starts (run), because a pass that carried it would
	// record "no build" against every project it read whole, and the next process to carry it
	// - another binary, holding another phase-work rule - would compare equal to that record
	// and sweep the projects R9 says to read whole.
	Build string
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
// The same passes keep each project's filings (issue_filings): an `issue.created` event a Legion
// session wrote is a new filing of that session's project (readEventPage), and a stored filing
// is computed again whenever the pass computes its filed issue or its filer's issue
// (filingsToCompute), so its class, its filer's phase and its root move with the inputs the
// rows move with.
//
// A pass that fails logs and leaves its cursors where they were, so the next pass reads the same
// rows again. Every pass is safe to run twice: a row whose values did not move is not rewritten
// (Upsert), and a cursor moves only in the transaction that wrote the rows it covers (passPage).
func Run(ctx context.Context, deps Deps) {
	run(ctx, deps, serverSchedule)
}

func run(ctx context.Context, deps Deps, sched schedule) {
	// The build is what every pass compares and records. A loop with no build records "no
	// build" against each project it reads whole, and the next process that also carries none
	// finds its own record and sweeps those projects, although another binary holds another
	// phase-work rule. It is a caller's mistake and never a deployment's: cmd/dispatch's
	// processBuild answers a commit or a string of its own process, and neither is empty.
	// Refusing it here keeps a rig from passing it as well.
	if deps.Build == "" {
		panic("legionfacts: Deps.Build is empty, and the loop needs the build this process is")
	}
	ctx = store.WithTransactionTracking(ctx)
	duty.RunWoken(ctx, duty.WokenLoop{
		Lease:  Lease,
		Leases: deps.Leases,
		Wake:   duty.MergeWakes(ctx, deps.Events, deps.Observations, deps.Deliveries),
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
// keys whose stored rows it changed and whether a feed holds more rows, which makes the pass
// read another page at once.
//
// sweep says whether this page carries the work no feed announces: the recomputation of the open
// rows, the recomputation of the rows whose issue lost its Legion phase work, the backfill of a
// project a feed cannot answer for, and the backfill a lost change cursor calls for. Only the
// first page of a pass carries it, because all four read the whole of what they cover. The page
// reads its feeds under the same flag, because only a sweep page may move a lost change cursor
// (readPages).
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
	read, err := readPages(ctx, deps.Pool, cursors, safe, sched.page, sweep)
	if err != nil {
		return nil, false, err
	}
	projects, err := legionProjects(ctx, deps.Pool, deps.Build, read.lost)
	if err != nil {
		return nil, false, err
	}

	work := map[string]*projectWork{}
	needed := func(project string) *projectWork {
		if _, known := projects[project]; !known {
			return nil
		}
		if work[project] == nil {
			work[project] = &projectWork{keys: map[string]bool{}, moved: map[string]bool{}, filed: map[string]bool{}}
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
	// A filing is the filer's project's, wherever the filed issue lives (legion.SessionTitle's
	// OfProject, the rule metrics reads a filing by), so it is owed to that project and to no
	// other. One project at most answers it: a project's key is upper case, and OfProject
	// compares it ignoring case.
	for _, filed := range read.events.filings {
		for project := range projects {
			if !filed.filer.OfProject(project) {
				continue
			}
			if work := needed(project); work != nil {
				work.filed[filed.issue] = true
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
	computed := []projectPass{}
	for _, project := range slices.Sorted(maps.Keys(work)) {
		one, err := computeProjectWork(ctx, deps.Pool, project, work[project], now)
		if err != nil {
			return nil, false, err
		}
		computed = append(computed, one)
	}

	positions := map[string]int64{
		eventsCursor:       read.events.position,
		observationsCursor: read.observations.position,
		changesCursor:      read.changes.position,
	}
	changed, err := writePass(ctx, deps.Pool, computed, positions, now, deps.Build, read.changes.remapped)
	if err != nil {
		return changed, false, err
	}
	return changed, read.more, nil
}

// projectWork is what one page owes one project: the issues its feeds named, those of them a
// row moved between trees (touch), the issues its events say a Legion session of the project
// filed (feed.filings), and whether the project is to be computed whole (a backfill).
type projectWork struct {
	keys     map[string]bool
	moved    map[string]bool
	filed    map[string]bool
	backfill bool
}

// projectPass is what one page computed for one project: the write of its rows and the write of
// its filings, which writePass makes in one transaction.
type projectPass struct {
	facts   ProjectWrite
	filings FilingsWrite
}

// computeProjectWork computes the rows and the filings one project owes and answers them as the
// writes of that project (ProjectWrite, FilingsWrite): the rows and the filings, the sets the
// pass stated for them, and whether the pass read the project whole. A backfill reads the whole
// project (computeWhole) and the keys its feeds named are left out of the call, because the
// whole project already holds them: its stated set is the project itself, and its filings are
// every filing a Legion session of the project made. Every other project is computed over the
// rows its changes touch (rowsToCompute) and the filings those rows and its new filings name
// (filingsToCompute), and no row or filing outside those sets is written or deleted.
func computeProjectWork(ctx context.Context, pool *store.Pool, project string, work *projectWork, now time.Time) (projectPass, error) {
	if work.backfill {
		rows, filings, err := computeWhole(ctx, pool, project, now)
		if err != nil {
			return projectPass{}, err
		}
		return projectPass{
			facts:   ProjectWrite{Project: project, Whole: true, Rows: rows},
			filings: FilingsWrite{Project: project, Whole: true, Filings: filings},
		}, nil
	}
	above, err := metrics.TreeLinks(ctx, pool, project)
	if err != nil {
		return projectPass{}, err
	}
	keys, err := rowsToCompute(ctx, pool, project, slices.Sorted(maps.Keys(work.keys)), work.moved, above)
	if err != nil {
		return projectPass{}, err
	}
	filed, err := filingsToCompute(ctx, pool, project, keys, work.filed)
	if err != nil {
		return projectPass{}, err
	}
	rows, filings, err := computeKeys(ctx, pool, project, keys, filed, now)
	if err != nil {
		return projectPass{}, err
	}
	return projectPass{
		facts:   ProjectWrite{Project: project, Stated: keys, Rows: rows},
		filings: FilingsWrite{Project: project, Stated: filed, Filings: filings},
	}, nil
}

// writePass writes every project's rows and filings, deletes the stale rows and filings inside
// the sets each project's pass stated, resolves every stored row's repository again where remapped
// says the page read a change of a repository's mapping (reresolveRepos), records the projects it
// computed whole and moves the three cursors, all in one transaction. It answers the keys whose
// stored rows changed, the deleted keys among them, because a row that went is a change of the
// stored facts as much as a row that moved. A failure leaves the cursors and the marks where they
// were, so the next pass reads the same rows again.
func writePass(ctx context.Context, pool *store.Pool, computed []projectPass, positions map[string]int64, now time.Time, build string, remapped bool) ([]string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("open the Legion facts' write: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	changed := []string{}
	for _, project := range computed {
		written, err := WriteProject(ctx, tx, project.facts)
		if err != nil {
			return nil, err
		}
		changed = append(changed, written...)
		if err := WriteFilings(ctx, tx, project.filings); err != nil {
			return nil, err
		}
	}
	if remapped {
		moved, err := reresolveRepos(ctx, tx)
		if err != nil {
			return nil, err
		}
		changed = append(changed, moved...)
	}
	if err := markComputedWhole(ctx, tx, computed, now, build); err != nil {
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

// markComputedWhole records that each project this write read whole was computed whole as of now,
// how many rows that computation answered and which build made it, which is what legionProjects
// reads a project's backfill from. It runs in the transaction that wrote those rows (writePass),
// so a pass that failed leaves no mark and the next pass reads the project whole again.
//
// The mark is a record rather than the absence of a stored row, because a project whose issues
// carry no Legion phase work is computed whole and answers no row at all: read from an absence,
// such a project would be computed whole again every SweepEvery for ever. The count is what
// still answers for the rows of a project going missing under the consumer, which is the other
// thing an absence used to say (migration 20261111153000).
//
// The build is the one the pass ran under (Deps.Build), the same string the read that sent
// this project to a whole read compared against (legionProjects). The pass carries that one
// value through both steps rather than reading it a second time, so a project read whole is
// marked as read whole by the build that read it and the next pass of that build sweeps it.
func markComputedWhole(ctx context.Context, tx pgx.Tx, computed []projectPass, now time.Time, build string) error {
	projects := []string{}
	answered := []int32{}
	for _, one := range computed {
		if !one.facts.Whole {
			continue
		}
		projects = append(projects, one.facts.Project)
		answered = append(answered, int32(len(one.facts.Rows)))
	}
	if len(projects) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		insert into issue_facts_computed (project_key, computed_at, computed_rows, computed_build)
		select project, $3, rows, $4 from unnest($1::text[], $2::integer[]) as given(project, rows)
		on conflict (project_key) do update
		set computed_at = excluded.computed_at,
		    computed_rows = excluded.computed_rows,
		    computed_build = excluded.computed_build
	`, projects, answered, now, build)
	if err != nil {
		return fmt.Errorf("record the %d projects computed whole: %w", len(projects), err)
	}
	return nil
}
