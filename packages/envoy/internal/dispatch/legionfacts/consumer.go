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
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Lease names the lease every pass takes (duty.Exclusive with no period, which is mutual
// exclusion and nothing else). Two processes are woken by one commit, and a pass reads its
// cursors, computes the rows and then writes both in one transaction, so two passes at once
// would compute the same projects twice and the second would write what the first already
// wrote. The lease is what keeps the read and the write one step.
const Lease = "legion-facts"

// Freshness is how far behind the event that changed it a stored row may stand: the requirement
// of dispatch://INFRA-2916 (R21). An open issue's hours grow with the clock while no input moves,
// and no pass recomputes them for that: the row holds its facts closed at the newest input they
// read and how fast each grows after it, and a reader closes each at its own clock (Row.At). A
// row is therefore never behind the clock, and only an input can leave it behind.
const Freshness = time.Minute

// PassRetry is how long a process that found the lease held waits, plus up to a tenth again,
// before it tries once more (duty.WokenLoop.Retry), since the other process's pass may have read
// before the change that woke this one, and how far on a pass that failed, whose cursors stand
// where they were, asks for the next (runPass). It is also the longest a pass whose safe horizon
// held an event back (events.Horizon.HeldBack) waits for the next, since the end of the write that
// holds the event wakes nothing: such a pass asks for the next horizonRetry on, doubling with each
// such pass in a row up to PassRetry (duty.Backoff). None of these is a timer that rewrites a row:
// the pass it asks for reads what the last one could not, and one that finds nothing new computes
// and writes nothing. Every other pass answers the zero time, and the loop waits for a wake.
//
// Nothing holds one pass back from the next (the loop's MinGap is zero): at most one pass runs
// and at most one is owed, so a change that commits while a pass runs earns the one pass after it,
// which starts as soon as the running one ends, and any number of changes during a pass earn that
// one pass. A row therefore stands behind the event that moved it by the pass running when the
// event commits, one lease retry and its own pass at most, from the moment the safe horizon can
// pass that event: the running pass, 11 s and the pass, against the minute Freshness allows. A
// held-back event waits past the end of the write that held it for the retry the loop then
// stands at, horizonRetry after a brief hold and PassRetry at most. A transaction left open on the
// Postgres instance holds the horizon, and so the event, back for as long as it stands, as it
// holds every reader of the horizon (events.Horizon), and the pass that meets it waits for
// nothing. Measured against the pod's test Postgres by TestAPassHoldsTheFreshnessRequirement,
// over two runs: one pass that reads one event on one issue takes 64 ms to 66 ms over the 252
// issues of the facts' own rig, 210 of them with Legion phase work, and 121 ms to 125 ms over
// four times that population; one whole read of the project (ComputeProject and WriteProject),
// the pass a new build makes once, takes 243 ms to 303 ms and 625 ms to 744 ms. The read of the
// rows another build wrote, which every first page of the recorded build makes (otherBuildsRows),
// takes 0.7 ms to 0.8 ms finding none over the rig and 1.3 ms to 1.6 ms over four times it, and
// 1.3 ms to 1.5 ms and 4.3 ms to 4.9 ms finding every row of the project written by another
// build.
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
// and 2 before the pass descended. The descent costs no read a pass did not make before it.
// metrics.ReadIssues reads the whole tree of every key it is given (treeScopeKeys), so an issue
// below a key is read whether or not the set names it. upsert writes a row only where a value of
// that row moved. A row the descent added and the computation left where it stood is therefore
// written by nothing.
const PassRetry = 10 * time.Second

// horizonRetry is how far on a pass whose safe horizon held an event back asks for the next, the
// first time in a row: each such pass after it doubles the wait, up to PassRetry.
const horizonRetry = 250 * time.Millisecond

// feedPage is the most rows one transaction reads of each feed. A pass whose page was full reads
// the next one at once, in a transaction of its own, so a backlog is worked through in bounded
// steps that each commit their progress.
const feedPage = 1000

// The cursors, one row of issue_facts_cursor each, named for the feed it is read by (feedCursors).
// The events and the observations cursors hold the id of the last row of their feed a committed
// page read, stamped with the moment its pass ran; the change cursor is a delivery.ChangeCursor,
// stamped with when a page last read the change log to its end.
const (
	eventsCursor       = "events"
	observationsCursor = "legion_observations"
	changesCursor      = "delivery_changes"
)

// cursorNames is the three in one list, which is how a pass reads them.
var cursorNames = []string{eventsCursor, observationsCursor, changesCursor}

// Deps is what Run needs of the server.
type Deps struct {
	// Pool is the database the facts are computed from and written to.
	Pool *store.Pool
	// Horizon is the process's one events.Horizon, the Horizon the live event feed reads
	// through: the events feed is paged by a global id, so it must stop at a safe horizon.
	Horizon *events.Horizon
	// Build names the build this process is, which the loop records against each project it
	// reads whole, compares on every pass after that (legionProjects), and stamps on every row
	// it writes (ProjectWrite.Build). The server gives it the commit the image build stamped
	// into the binary, and a build no release stamped gives it a string of that one process
	// (cmd/dispatch's processBuild). The empty string is no build: the loop refuses it where it
	// starts (run), because a pass that carried it would record "no build" against every
	// project it read whole, and the next process to carry it - another binary, holding another
	// phase-work rule - would compare equal to that record and read none of the projects R27
	// says to read whole.
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
	// delivery.ChangesChannel. A facts write of either process notifies it once its changes
	// are drained, which is how the build a project's record names hears of a row the other
	// build of a roll wrote (otherBuildsRows).
	Deliveries <-chan struct{}
}

// schedule is what a test replaces of the loop: page is the most rows a page reads of each feed
// (feedPage); now answers the moment a pass runs at, which a row's updated_at, the events and the
// observations cursors' stamp and the record of a whole read carry, and which each page reads
// again for its change cursor (passPage); and started is when this process started, which a
// record another build wrote is judged against (legionProjects). run sets started from now where
// the schedule leaves it zero.
type schedule struct {
	page    int
	now     func() time.Time
	started time.Time
}

// serverSchedule is the schedule the server runs the loop under.
var serverSchedule = schedule{
	page: feedPage,
	now:  func() time.Time { return time.Now().UTC() },
}

// process is the process a pass runs in, as a project's record of its whole read is judged
// against it (legionProjects) and written by it (markComputedWhole): the build it runs under
// (Deps.Build) and when it started (schedule.started).
type process struct {
	build   string
	started time.Time
}

// Run keeps every project's stored Legion facts within a minute of their inputs. It stops when
// ctx is done.
//
// It is a woken loop (duty.RunWoken) under one lease (Lease): one pass at start, a pass on each
// of the three wakes, which starts as soon as the pass running when it came ends (PassRetry), a
// pass PassRetry after one that failed, a pass soon after one whose safe horizon held an event
// back, and no timer that rewrites a row. A pass recomputes an issue and upserts its row when
// that issue's inputs move, and at no other time. An open issue's hours grow with the clock while
// no input moves, and that is no input: the row holds the issue's facts closed at the newest
// input they read and how fast each grows after it (metrics.ProjectFactsClosed), so a
// recomputation whose inputs did not move answers the row the table holds and writes nothing,
// and a reader gives each fact at its own clock (Row.At).
//
// Three inputs move a stored row, and a pass reads each from a cursor of its own:
//  1. The issue's events, woken by Deps.Events. Every event of an issue can move that issue's
//     facts - a status mark, a role event, an ask turn, a handoff.recorded, and the session
//     title an event's actor carries, which decides whether the issue carries Legion phase work
//     at all - so the pass takes the page's issues and recomputes them (readEventPage).
//  2. The daemon's observations, woken by Deps.Observations: the phase, the slot and the wait
//     the sampler recorded of an issue, and the project's admission block, which every open
//     row's waiting reason reads, so a move of the block recomputes those rows
//     (readObservationPage).
//  3. A pull request's merge time, woken by Deps.Deliveries: of the issue that holds the pull
//     request now, and of the issue a move of it left with a merge no pull request backs
//     (readChangePage).
//
// Two reads answer what no feed carries, both on the first page of a pass (unfed, passPage).
// A project the running build has not read whole is read whole (legionProjects, R27): the rule
// that decides which issues carry Legion phase work lives in the build, so a release can give an
// issue work or take it away with no event, and the whole read is what writes the one row and
// deletes the other (deleteStaleProjectRows). And the build a project's record names reads the
// rows and the filings another build of a roll wrote and computes them again (otherBuildsRows,
// otherBuildsFilings), which is how the running build learns the roll has ended with no timer:
// once the older process has stopped no row or filing is left that it wrote.
//
// The same passes keep each project's filings (issue_filings): an `issue.created` event a Legion
// session wrote is a new filing of that session's project, a session that writes under a title
// naming another issue moves every filing it made to the project and the filer that title names
// (readEventPage, sessionFilings), and a stored filing is computed again whenever the pass
// computes its filed issue or its filer's issue (filingsToCompute), so its class, its filer's
// phase and its root move with the inputs the rows move with.
//
// A pass that fails logs and leaves its cursors where they were, and asks for a pass PassRetry
// later, which reads the same rows again unless a wake's pass has read them first (runPass).
// Every pass is safe to run twice: a row whose values did not move is not rewritten (upsert), and
// a cursor moves only in the transaction that wrote the rows it covers (passPage).
func Run(ctx context.Context, deps Deps) {
	run(ctx, deps, serverSchedule)
}

func run(ctx context.Context, deps Deps, sched schedule) {
	// The build is what every pass compares and records. A loop with no build records "no
	// build" against each project it reads whole, and the next process that also carries none
	// finds its own record and reads none of those projects whole, although another binary holds
	// another phase-work rule. It is a caller's mistake and never a deployment's:
	// cmd/dispatch's processBuild answers a commit or a string of its own process, and neither is
	// empty. Refusing it here keeps a rig from passing it as well.
	if deps.Build == "" {
		panic("legionfacts: Deps.Build is empty, and the loop needs the build this process is")
	}
	if sched.started.IsZero() {
		sched.started = sched.now()
	}
	ctx = store.WithTransactionTracking(ctx)
	held := &duty.Backoff{First: horizonRetry, Max: PassRetry}
	duty.RunWoken(ctx, duty.WokenLoop{
		Lease:  Lease,
		Leases: deps.Leases,
		Wake:   duty.MergeWakes(ctx, deps.Events, deps.Observations, deps.Deliveries),
		Retry:  PassRetry,
		Pass:   func(ctx context.Context) time.Time { return runPass(ctx, deps, sched, held) },
	})
}

// runPass is one pass as the loop runs it (pass), and the due time it answers the loop, on the
// wall clock the loop's timer runs on rather than sched's, which a test stands still: PassRetry
// on for a pass that failed, as the suggestion outcomes and the eval refresher retry theirs,
// held's next wait on for a pass whose safe horizon held an event back (PassRetry), and the zero
// time for every other pass, which begins held's waits again. A failed pass left its cursors
// where they were, so the event whose wake started it is still unread, and with no retry it
// would wait for an unrelated wake. A pass whose context ended answers the zero time: the loop is
// stopping.
func runPass(ctx context.Context, deps Deps, sched schedule, held *duty.Backoff) time.Time {
	changed, heldBack, err := pass(ctx, deps, sched)
	if ctx.Err() != nil {
		return time.Time{}
	}
	if len(changed) > 0 {
		slog.Debug("dispatch: Legion facts written", "rows", len(changed))
	}
	if err != nil {
		slog.Warn("dispatch: the Legion facts pass failed", "error", err)
		return time.Now().Add(PassRetry)
	}
	if heldBack {
		return time.Now().Add(held.Next())
	}
	held.Reset()
	return time.Time{}
}

// pass is one pass of the loop: it reads page after page of the three feeds until one comes back
// short, each page computed and written in a transaction of its own, and answers the keys whose
// stored rows it changed and whether its safe horizon held an event back. Every page runs at the
// moment the pass read its horizon, and judges and stamps its change cursor by a reading of the
// clock of its own (passPage). The first page carries the work no feed announces (passPage), and
// so does a later page where the page before it met a lost change cursor it held (readPages), so
// the pass answers the loss itself rather than leave it to a pass no wake may start.
//
// The events feed is read up to the safe horizon the pass reads once (events.Horizon.SafeOrAwait,
// the reading the suggestion outcomes and the eval refresher take): one observation, which waits
// on no open transaction, and an Await only while the horizon has proven nothing yet, a process's
// first pass. The lease is held for the pass, so a pass that waited out every transaction open on
// the instance would hold every facts update, and the other process's, behind the longest of
// them. A pass that leaves an event above what it read (events.Horizon.HeldBack), the event whose
// commit woke it among them, says so, and the loop asks for another soon, which reads it once the
// horizon has passed it (runPass). The moment the pass computes at is read after the horizon, so
// no event the pass reads is newer than the moment its rows are of.
func pass(ctx context.Context, deps Deps, sched schedule) ([]string, bool, error) {
	safe, err := deps.Horizon.SafeOrAwait(ctx)
	if err != nil {
		return nil, false, err
	}
	now := sched.now()
	changed := []string{}
	for page := (nextPage{more: true, unfed: true}); page.more; {
		var keys []string
		keys, page, err = passPage(ctx, deps, sched, safe, now, page.unfed)
		changed = append(changed, keys...)
		if err != nil {
			return changed, false, err
		}
	}
	return changed, deps.Horizon.HeldBack(safe), nil
}

// nextPage is what one page tells its pass of the page after it (passPage): whether to read one
// at all, because a feed holds more rows, and whether it must carry the work no feed names
// (unfed).
type nextPage struct {
	more, unfed bool
}

// passPage is one page of one pass: it reads where each feed stands, reads a page of each,
// computes the rows those pages name, and writes those rows, the deletion of the stale rows
// inside the set it stated, and the three cursors in one transaction. It answers the keys whose
// stored rows it changed and what the page after it owes (nextPage).
//
// unfed says the page carries the work no feed names: the whole read of a project the running
// build has not read whole, the whole read a lost change cursor calls for, and the rows another
// build wrote (otherBuildsRows). The first page of a pass carries it, and so does the page after
// one that met a lost change cursor it held (readPages); no other page does, because each part of
// that work reads the whole of what it covers. The page reads its feeds under the same flag,
// because only a page that carries the whole read may move a lost change cursor.
//
// The rows are computed before the transaction opens and written inside it, so no connection is
// held open while a whole project is read. The cursor moves in the transaction that wrote the
// rows it covers, so a crash between the two cannot lose a change: the next pass reads the same
// rows again and writes the same values.
//
// The rows are computed as of now, the pass's moment, and the change cursor is judged lost or not,
// and a read of the change log to its end stamped, by checked, a reading of the clock this page
// takes before it reads (readPages, delivery.ChangeCursor): a pass that catches up a backlog for
// long enough crosses the change log's retention between two of its pages, and a page judged at
// the pass's start would read on past what a prune took meanwhile.
func passPage(ctx context.Context, deps Deps, sched schedule, safe int64, now time.Time, unfed bool) ([]string, nextPage, error) {
	self := process{build: deps.Build, started: sched.started}
	cursors, committed, err := readCursors(ctx, deps.Pool)
	if err != nil {
		return nil, nextPage{}, err
	}
	checked := sched.now()
	read, err := readPages(ctx, deps.Pool, cursors, committed, safe, checked, sched.page, unfed)
	if err != nil {
		return nil, nextPage{}, err
	}
	projects, err := legionProjects(ctx, deps.Pool, self, read.lost)
	if err != nil {
		return nil, nextPage{}, err
	}

	work := map[string]*projectWork{}
	needed := func(project string) *projectWork {
		if _, known := projects[project]; !known {
			return nil
		}
		if work[project] == nil {
			work[project] = &projectWork{
				keys: map[string]bool{}, moved: map[string]bool{}, filed: map[string]bool{},
				yielded: projects[project].yielded,
			}
		}
		return work[project]
	}
	for _, page := range []feed{read.events, read.observations, read.changes} {
		for _, touched := range page.touches {
			if project := needed(touched.project); project != nil {
				if touched.admission {
					project.admission = true
				}
				// The project's own observations, its admission block and its settings, name
				// no issue and so no row: a move of the block owes the open rows (admission).
				if touched.issue == legion.AdmissionKey {
					continue
				}
				project.keys[touched.issue] = true
				if touched.moved {
					project.moved[touched.issue] = true
				}
			}
		}
	}
	// A filing is the filer's project's, wherever the filed issue lives (legion.SessionTitle's
	// OfProject, the rule metrics reads a filing by), so it is owed to the project its filer's
	// newest title names, which computes and writes it. One project at most answers that: a
	// project's key is upper case, and OfProject compares it ignoring case. A filing stored under
	// another project is owed to that project as well, whose computation answers no filing for it
	// and so deletes it: the one pass takes the filing out of the project it left and writes it
	// into the project it joined, and the upsert of the one moves the stored row to it whichever
	// of the two is written first (upsertFilings).
	for _, filed := range read.events.filings {
		for project := range projects {
			if !(filed.legion && filed.filer.OfProject(project)) && project != filed.stored {
				continue
			}
			if work := needed(project); work != nil {
				work.filed[filed.issue] = true
			}
		}
	}
	if unfed {
		for project, standing := range projects {
			if !standing.whole {
				continue
			}
			if work := needed(project); work != nil {
				work.backfill = true
			}
		}
		// The rows another build wrote join the feeds' keys, and the filings another build wrote
		// the filings the events moved: the build that wrote them may hold another phase-work
		// rule, which can move a filing as well as a row. The reads answer the projects whose
		// record names this build alone, and a project read whole already reads them.
		rows, err := otherBuildsRows(ctx, deps.Pool, self.build)
		if err != nil {
			return nil, nextPage{}, err
		}
		filings, err := otherBuildsFilings(ctx, deps.Pool, self.build)
		if err != nil {
			return nil, nextPage{}, err
		}
		narrow := func(project string) *projectWork {
			if projects[project].whole {
				return nil
			}
			return needed(project)
		}
		for project, keys := range rows {
			if work := narrow(project); work != nil {
				for _, key := range keys {
					work.keys[key] = true
				}
			}
		}
		for project, issues := range filings {
			if work := narrow(project); work != nil {
				for _, issue := range issues {
					work.filed[issue] = true
				}
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
			return nil, nextPage{}, err
		}
		computed = append(computed, one)
	}

	moved := feedCursors{
		events:       read.events.position,
		observations: read.observations.position,
		changes:      delivery.ChangeCursor{Seq: read.changes.position, At: read.changes.at, ReadAt: read.changes.readAt},
	}
	changed, err := writePass(ctx, deps.Pool, computed, moved, now, self)
	if err != nil {
		return changed, nextPage{}, err
	}
	return changed, nextPage{more: read.more, unfed: read.unfed}, nil
}

// projectWork is what one page owes one project: the issues its feeds named and the rows another
// build wrote (keys), those of the feeds' issues a row moved between trees (touch), whether the
// page read a move of the project's admission block, which owes every open row of the project
// (openRows), the filings its events moved that the project owes, the ones it joined and the ones
// it left (feed.filings), whether the project is to be computed whole (a backfill), and whether
// its record names a newer build this process yields to (projectStanding.yielded), which the write
// of its rows carries (ProjectWrite.Yielded).
type projectWork struct {
	keys      map[string]bool
	moved     map[string]bool
	admission bool
	filed     map[string]bool
	backfill  bool
	yielded   bool
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
// project (ComputeProject) and the keys its feeds named are left out of the call, because the
// whole project already holds them: its stated set is the project itself, and its filings are
// every filing a Legion session of the project made. Every other project is computed over the
// rows its keys touch (rowsToCompute) and the filings named by its moved filings and by those
// rows (filingsToCompute), and no row or filing outside those sets is written or deleted.
func computeProjectWork(ctx context.Context, pool *store.Pool, project string, work *projectWork, now time.Time) (projectPass, error) {
	if work.backfill {
		rows, filings, err := ComputeProject(ctx, pool, project, now)
		if err != nil {
			return projectPass{}, err
		}
		return projectPass{
			facts:   ProjectWrite{Project: project, Whole: true, Rows: rows, Yielded: work.yielded},
			filings: FilingsWrite{Project: project, Whole: true, Filings: filings, Yielded: work.yielded},
		}, nil
	}
	above, err := metrics.TreeLinks(ctx, pool, project)
	if err != nil {
		return projectPass{}, err
	}
	keys, err := rowsToCompute(ctx, pool, project, slices.Sorted(maps.Keys(work.keys)), work.moved, work.admission, above)
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
		facts:   ProjectWrite{Project: project, Stated: keys, Rows: rows, Yielded: work.yielded},
		filings: FilingsWrite{Project: project, Stated: filed, Filings: filings, Yielded: work.yielded},
	}, nil
}

// writePass writes every project's rows and filings under the build self runs in, deletes the
// stale rows and filings inside the sets each project's pass stated (a project whose record names
// a newer build has its stale rows and filings stamped instead, WriteProject and WriteFilings),
// records the projects it computed whole and moves the three cursors, all in one transaction. It
// answers the keys whose stored rows changed, the
// deleted and stamped keys among them, because a row that went is a change of the stored facts as
// much as a row that moved. A failure leaves the cursors and the marks where they were, so the
// next pass reads the same rows again.
//
// The rows' and the filings' triggers record their changes in the change log's outbox (migrations
// 20261117100002 and 20261117100019) and take no lock, so the transaction never takes the change
// log's lock (6900, delivery's lockChangesStatement); once it has committed, the pass moves them
// into the log (delivery.DrainPending). A drain that fails is logged and fails nothing: the rows committed, and
// the next drain of any process moves their changes (delivery.DrainPending states the bound). The
// drain's notification is also the wake of the other process of a roll, whose build the project's
// record may name (Deps.Deliveries).
func writePass(ctx context.Context, pool *store.Pool, computed []projectPass, cursors feedCursors, now time.Time, self process) ([]string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("open the Legion facts' write: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	changed := []string{}
	for _, project := range computed {
		facts, filings := project.facts, project.filings
		facts.Build, filings.Build = self.build, self.build
		written, err := WriteProject(ctx, tx, facts)
		if err != nil {
			return nil, err
		}
		changed = append(changed, written...)
		if err := WriteFilings(ctx, tx, filings); err != nil {
			return nil, err
		}
	}
	if err := markComputedWhole(ctx, tx, computed, now, self); err != nil {
		return nil, err
	}
	if err := advanceCursors(ctx, tx, cursors, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit the Legion facts: %w", err)
	}
	if _, err := delivery.DrainPending(ctx, pool); err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: the Legion facts consumer could not move its changes into the delivery change log; the next drain moves them", "error", err)
	}
	return changed, nil
}

// markComputedWhole records that each project this write read whole was computed whole as of now,
// how many rows that computation answered and which build made it, which is what legionProjects
// reads a project's whole read from. It runs in the transaction that wrote those rows (writePass),
// so a pass that failed leaves no mark and the next pass reads the project whole again.
//
// The mark is a record rather than the absence of a stored row, because a project whose issues
// carry no Legion phase work is computed whole and answers no row at all: read from an absence,
// such a project would be computed whole again on every pass for ever. The count is what still
// answers for the rows of a project going missing under the consumer, which is the other thing
// an absence used to say (migration 20261117100003).
//
// The build is the one the pass ran under (Deps.Build), the same string the read that sent
// this project to a whole read compared against (legionProjects). The pass carries that one
// value through both steps rather than reading it a second time (process), so a project read
// whole is marked as read whole by the build that read it and the next pass of that build reads
// it whole no more.
//
// A record another build wrote after this process started is left where it stands: that build
// is newer, rolling in while this one still serves, and the record is what makes it the build
// that corrects the rows of the other (otherBuildsRows). This process reads such a project whole
// only for a lost change cursor (legionProjects), and a mark of that read taken over the newer
// build's would hand the project back to the older build for good. The process's start is the
// moment the record's own time is judged against.
func markComputedWhole(ctx context.Context, tx pgx.Tx, computed []projectPass, now time.Time, self process) error {
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
		where issue_facts_computed.computed_build is not distinct from excluded.computed_build
		   or issue_facts_computed.computed_at < $5
	`, projects, answered, now, self.build, self.started)
	if err != nil {
		return fmt.Errorf("record the %d projects computed whole: %w", len(projects), err)
	}
	return nil
}
