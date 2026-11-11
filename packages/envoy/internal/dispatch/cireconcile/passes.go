package cireconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The record every pass leaves in ci_reconcile_passes (migration
// 20261110030133_ci_reconcile_passes), beside the one INFO line it logs.
//
// The line is all a pass says in the log, and a container log rotates: the production
// pod's reached back 45 minutes, nine passes, which is not enough to answer why a head read six
// hours ago still has no check. So each pass writes a row when it starts and fills the rest in
// when it ends, and GET /api/v1/ci-reconcile/passes reads the last day of them back out
// (RecentPasses). A row costs one insert and one update per five minutes and is kept for
// PassRetention, which is twice the window the route serves, so a pass that is still being read
// about is never pruned out from under the reader.
const (
	// PassWindow is how far back the route reads: the passes of the last day.
	PassWindow = 24 * time.Hour
	// PassRetention is how long a row is kept. Twice the window, so the oldest pass the route
	// answers has a day of life left in it, and a deployment whose passes all ran can see the
	// one before the oldest it reports.
	PassRetention = 48 * time.Hour
)

// PassState is what a pass's row reads as: how the pass ended, for a row with an end time, and
// how a row without one reads (RecentPasses). The first four are the whole set the table's own
// check holds the outcome column to; the two below them are read off the row rather than
// written to it, since a pass that never recorded its end wrote no outcome at all.
type PassState string

const (
	// outcomeComplete is a pass that read everything it set out to read in this interval and
	// ended of its own accord. It does not mean the pass closed a cycle: a pass that ends at the
	// end of the order closes one (counts.cycle) and a pass that was never cut short still ran
	// to the end of the order it was given.
	outcomeComplete PassState = "complete"
	// outcomeDeadline is a pass its own five-minute bound cut short, which leaves the cursor at
	// the head the next pass takes up.
	outcomeDeadline PassState = "deadline"
	// outcomeRateLimit is a pass a shared GitHub rate limit ended where it stood.
	outcomeRateLimit PassState = "rate_limit"
	// outcomeCancelled is a pass the caller whose loop runs it cancelled, which is the process
	// shutting down. It is recorded apart from the three above because the pass did not finish
	// what it set out to read and nothing in its counts says so: the heads it never reached
	// were not read, and a reader of a row that said `complete` would take the gap they leave
	// for a pass that found nothing there.
	outcomeCancelled PassState = "cancelled"

	// PassStateRunning is a row with no end time whose pass is still running: its process is
	// live, the lease is held and it is the newest row (RecentPasses says why all three).
	PassStateRunning PassState = "running"
	// PassStateAbandoned is a row with no end time whose pass is not running: the process was
	// killed between the pass's start and its end, so nothing ever wrote the end. Nothing
	// repairs such a row -- the prune deletes it with the rest after PassRetention -- because
	// no later pass knows what this one had done when it died.
	PassStateAbandoned PassState = "ended_without_completing"
)

// insertPass opens a pass's row. The counts and the outcome are left null: a pass that has not
// ended has counted nothing, and a zero would read as a pass that found nothing to do.
const insertPass = `
	insert into ci_reconcile_passes (instance, started_at) values ($1, now())
	returning id`

// updatePass closes it, with everything the pass's one log line carries and the outcome, which
// the line does not: a cancelled pass logs the same line as a complete one.
const updatePass = `
	update ci_reconcile_passes set
		ended_at = now(),
		outcome = $2,
		graphql_queries = $3,
		graphql_points = $4,
		rest_requests = $5,
		rest_not_modified = $6,
		heads_over_rest = $7,
		records = $8,
		heads = $9,
		heads_known = $10,
		cursor_repository = $11,
		cursor_from = $12,
		cursor_number = $13,
		cursor_rest = $14,
		cursor_counted = $15
	where id = $1`

// prunePasses deletes the rows past PassRetention. It runs with the pass that ends rather than on
// a loop of its own: the passes are the only writer of this table, so there is nothing to prune
// between two of them, and a deployment whose pass has stopped running has a dead loop to look at
// rather than a table to tidy.
const prunePasses = `delete from ci_reconcile_passes where started_at < now() - make_interval(secs => $1)`

// beginPass writes the row this pass fills in when it ends, and answers its id; zero is no row,
// which every later step of the pass reads as nothing to record.
//
// A pass writes no row before its process holds one in the registry of live server processes.
// The registry row is what says whether a pass with no end time is still running (RecentPasses),
// and cmd/dispatch registers the process after it has started its loops -- registration waits for
// a live peer whose signing keys do not match to lapse, and the loops must not wait on that --
// so the first pass or two of a process can run before its instance id exists. Such a pass logs
// its line and records nothing, which is honest: a row naming no process could never be read as
// running or as ended without completing.
//
// It writes on the pass's own context, which the end write may not use: this runs at the start of
// the pass, where the whole interval is still ahead of it.
func (r *Reconciler) beginPass(ctx context.Context) int64 {
	if r.Instance == nil {
		return 0
	}
	instance := r.Instance()
	if instance == "" {
		return 0
	}
	var id int64
	if err := r.Pool.QueryRow(ctx, insertPass, instance).Scan(&id); err != nil {
		r.Logger.Warn("ci reconcile pass recorded no start", "instance", instance, "error", err)
		return 0
	}
	return id
}

// endPass closes the row beginPass opened and prunes the rows past PassRetention. id of zero is a
// pass that opened none, which records nothing and prunes nothing: the prune belongs to a pass
// that wrote, so a process with no registry row never deletes another process's record.
//
// at is the rate limit that ended the pass, nil for a pass no limit stopped, and cancelled says
// the caller whose loop runs the pass cancelled it, which is the process going down.
func (r *Reconciler) endPass(id int64, c *counts, at *stop, cancelled bool) {
	if id == 0 {
		return
	}
	// The end is recorded on a context detached from the pass's own and bounded at five
	// seconds, which is duty.LeaseCall, the context a lease's own statements run on, taken here
	// for the same two reasons.
	//
	// Detached, because the pass's own context is the one thing that is reliably dead at this
	// moment. A pass is bound at its loop's interval, and a pass that ran out of that bound is
	// precisely the pass whose row must say so: recording its end on a context whose deadline
	// has passed would write nothing at all and leave a row that reads as a pass that vanished,
	// which is the one state this record exists to tell apart from a deadline. The same holds
	// for a shutdown that cancelled the pass.
	//
	// Bounded, because a statement with no deadline at all wedges the loop's goroutine on a
	// half-open connection, where a send draws neither an answer nor an error, while an update
	// of one row by its primary key answers at once on a connection that works.
	ctx, done := duty.LeaseCall()
	defer done()
	cursor := r.resume
	if _, err := r.Pool.Exec(ctx, updatePass, id, string(passOutcome(c, at, cancelled)),
		c.queries, c.points, c.restRequests, c.restNotModified, c.restHeads, c.records, c.heads, r.known(),
		cursor.repository, cursor.from, cursor.number, cursor.rest, cursor.counted); err != nil {
		r.Logger.Warn("ci reconcile pass recorded no end", "pass", id, "error", err)
	}
	if _, err := r.Pool.Exec(ctx, prunePasses, PassRetention.Seconds()); err != nil {
		r.Logger.Warn("ci reconcile passes older than the retention were not pruned", "error", err)
	}
}

// passOutcome is how the pass ended, as the row records it. A rate limit outranks the bound: a
// pass both stopped reading at a limit and ran out of its interval is a pass the limit ended,
// since what it would have done with the time left is nothing. A cancellation is read after
// both, so it displaces only outcomeComplete: a pass that had already stopped reading at a
// limit or at its own bound was over before its caller's shutdown reached it, and the limit or
// the bound is what ended it. What a cancellation must never be read as is a pass that
// finished.
func passOutcome(c *counts, at *stop, cancelled bool) PassState {
	switch {
	case at != nil:
		return outcomeRateLimit
	case c.deadline:
		return outcomeDeadline
	case cancelled:
		return outcomeCancelled
	default:
		return outcomeComplete
	}
}

// PassRecord is one row of ci_reconcile_passes as GET /api/v1/ci-reconcile/passes answers it.
type PassRecord struct {
	// ID is the row's own identity, which orders two passes that started in the same instant.
	ID int64 `json:"id"`
	// Instance is the server_processes instance id of the process that ran the pass. The
	// process may be long gone: the row outlives it by up to PassRetention.
	Instance string `json:"instance"`
	// StartedAt is when the pass started and EndedAt when it ended, null for a pass that is
	// running and for one that died before it could record its end. State tells those apart.
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	// State is the pass's own outcome once it has ended, and PassStateRunning or
	// PassStateAbandoned for a row with no end time.
	State PassState `json:"state"`
	// Counts is what the pass did, null until it ended.
	Counts *PassCounts `json:"counts"`
	// Cursor is where the pass left the order for the pass after it, null until it ended. The
	// zero cursor -- no repository, no number, no pull request -- is the start of the order,
	// which is what a pass that reached the end of the order leaves behind.
	Cursor *PassCursor `json:"cursor"`
}

// PassCounts is what one pass did, the same eight numbers its one log line carries.
type PassCounts struct {
	// GraphQLQueries is the batched queries the pass asked and GraphQLPoints what GitHub
	// charged them against the App's GraphQL budget.
	GraphQLQueries int `json:"graphql_queries"`
	GraphQLPoints  int `json:"graphql_points"`
	// RESTRequests is the REST reads beside them and RESTNotModified how many of those GitHub
	// answered 304, which cost the budget nothing.
	RESTRequests    int `json:"rest_requests"`
	RESTNotModified int `json:"rest_not_modified"`
	// HeadsOverREST is the heads a query could not answer whole, which the pass read over REST.
	HeadsOverREST int `json:"heads_over_rest"`
	// Records is the CI records the pass changed, one per head however many objects it wrote.
	Records int `json:"records"`
	// Heads is the heads this pass read whole and HeadsKnown the open heads the cycle's queries
	// listed, so the two together say how far round the order the pass got.
	Heads      int `json:"heads"`
	HeadsKnown int `json:"heads_known"`
}

// PassCursor is where a pass left the order, which is the address the pass after it takes the
// order up at: the mapped repository, the number that names the page of its open pull requests
// -- one more than the last head of the page before it, and zero for a repository's first page
// -- and the first pull request of that page the pass did not finish, zero for the whole page.
// Rest says the page is read from the repository's REST listing rather than from a query, and
// Counted that the heads of that page are already among the heads the cycle knows the
// repository has, so the pass taking the cursor up neither queries for a page it holds no
// cursor for nor counts those heads a second time.
//
// The page's position in the repository is recorded nowhere, because it is not what the next
// pass reads by: a pull request merged below the page moves the position and moves no number,
// so the number names the same page in any later listing and the position names a page the
// cursor never stood at (cursor, walk.window). GitHub's own opaque page token is recorded
// nowhere either -- it is valid only to the process that was handed it, and it says nothing to
// a person reading the row.
type PassCursor struct {
	Repository string `json:"repository"`
	From       int64  `json:"from"`
	Number     int64  `json:"number"`
	Rest       bool   `json:"rest"`
	Counted    bool   `json:"counted"`
}

// recentPasses reads the rows of the last PassWindow, newest first, each with whether the process
// that wrote it is still live by the registry's own clock.
const recentPasses = `
	select p.id, p.instance::text, p.started_at, p.ended_at, p.outcome,
	       coalesce(p.graphql_queries, 0), coalesce(p.graphql_points, 0), coalesce(p.rest_requests, 0),
	       coalesce(p.rest_not_modified, 0), coalesce(p.heads_over_rest, 0), coalesce(p.records, 0),
	       coalesce(p.heads, 0), coalesce(p.heads_known, 0),
	       coalesce(p.cursor_repository, ''), coalesce(p.cursor_from, 0), coalesce(p.cursor_number, 0),
	       coalesce(p.cursor_rest, false), coalesce(p.cursor_counted, false),
	       coalesce(s.heartbeat_at > now() - make_interval(secs => $1), false)
	from ci_reconcile_passes p
	left join server_processes s on s.instance = p.instance
	where p.started_at > now() - make_interval(secs => $2)
	order by p.started_at desc, p.id desc`

// RecentPasses is the CI reconcile passes of the last PassWindow, newest first.
//
// A row with no end time is read as still running only while all three of these hold, and as
// PassStateAbandoned otherwise:
//
//   - the process that wrote it is live, which is its server_processes heartbeat within
//     processes.LiveWindow of the database's now(). A process killed mid-pass leaves both its
//     registry row and this one behind, and only the heartbeat says which of the two kinds of
//     silence this is;
//   - the `ci-reconcile` lease is held by some session. A pass runs under that lease for the
//     whole of its life (duty.Exclusive), so a lease nobody holds is a pass nobody is running;
//   - it is the newest row. One process runs one pass at a time, and the lease keeps the
//     processes to one pass between them, so an older row with no end time is a pass that was
//     already over when the row above it began, whatever the lease and the heartbeat say.
//
// No one of the three is enough on its own, which is why all three are read. A killed process's
// registry row stays live for up to processes.LiveWindow after its last heartbeat. A lease is a
// session-level advisory lock, and a session the database has not yet reaped still holds it, so
// a held lease is not proof that a pass is running either. And neither would notice a row the
// pass after it has already overtaken. The newest row of a live process under a held lease is the
// one row all three agree about.
//
// The lease is read once for the whole answer rather than per row: it is one fact about the
// database at the moment of the read, and the rows are read in one statement beside it.
func RecentPasses(ctx context.Context, pool *store.Pool) ([]PassRecord, error) {
	held, err := duty.LeaseHeld(ctx, pool, Lease)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, recentPasses, processes.LiveWindow.Seconds(), PassWindow.Seconds())
	if err != nil {
		return nil, fmt.Errorf("read the CI reconcile passes: %w", err)
	}
	defer rows.Close()
	passes := []PassRecord{}
	for rows.Next() {
		pass, live, err := scanPass(rows)
		if err != nil {
			return nil, err
		}
		// The rows come newest first, so the first of them is the newest row there is: a row
		// outside this window cannot be newer than one inside it.
		newest := len(passes) == 0
		pass.State = passState(pass, live && held && newest)
		passes = append(passes, pass)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the CI reconcile passes: %w", err)
	}
	return passes, nil
}

// passState is the state the route answers: the outcome the pass recorded, or, for a row with no
// end time, whether the three liveness facts hold (RecentPasses).
func passState(pass PassRecord, running bool) PassState {
	switch {
	case pass.EndedAt != nil:
		return pass.State
	case running:
		return PassStateRunning
	default:
		return PassStateAbandoned
	}
}

// scanPass reads one row, with whether its process is live. The counts and the cursor are null
// exactly while the pass has not ended, which the table's own check holds to the end time, so
// the statement reads each of them through a coalesce to its own zero and the outcome, the one
// column that says which kind of row this is, is the one nullable scan: a row with no outcome
// is a pass that has not ended, whose counts and cursor the answer leaves null together.
func scanPass(row pgx.Row) (pass PassRecord, live bool, err error) {
	var outcome *string
	var counts PassCounts
	var cursor PassCursor
	if err := row.Scan(&pass.ID, &pass.Instance, &pass.StartedAt, &pass.EndedAt, &outcome,
		&counts.GraphQLQueries, &counts.GraphQLPoints, &counts.RESTRequests, &counts.RESTNotModified,
		&counts.HeadsOverREST, &counts.Records, &counts.Heads, &counts.HeadsKnown,
		&cursor.Repository, &cursor.From, &cursor.Number, &cursor.Rest, &cursor.Counted, &live); err != nil {
		return PassRecord{}, false, fmt.Errorf("read the CI reconcile passes: %w", err)
	}
	if outcome != nil {
		pass.State = PassState(*outcome)
		pass.Counts = &counts
		pass.Cursor = &cursor
	}
	return pass, live, nil
}
