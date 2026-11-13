package api

// going_landed.go is how a command-bar write or undo knows which of its requests landed. A
// request has landed when its handler's transaction committed the change it made, whatever status
// its answer then carried: a route may commit and then answer 5xx (createStoredMessage commits a
// message and then fails to read its deliveries), and an answer may be lost after its commit with
// the process that was to read it. So no answer is taken for a landing. The record is written by
// the request itself: each request a write or an undo sends in-process carries a goingLanding on
// its context, and goingInProcess.Do hands its route the commit step that writes it
// (goingLanding.step), inside the route's own transaction, so the change and the record of its
// landing commit together or not at all. The write or the undo then reads that record back
// (goingLandings.landed), and a later request for the same invocation resumes after every request
// the record names as landed, sending none of them again, whether the request that sent them
// answered, was refused or ended with its process.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingLandingKey carries a goingLanding on the context of the request it belongs to.
type goingLandingKey struct{}

// goingLandings is where the requests of one write, or of one write's undo, record that they
// landed: the author's run, the write's id, and for an undo the moment its claim was made
// (goingUndoClaimedAt), "" for the write itself. A nil *goingLandings records nothing, which is
// a GoingCaller sending to another server's table, where no record of this server's is reached.
type goingLandings struct {
	s     *server
	run   string
	login string
	write string
	undo  string
}

// goingLanding is one request's landing as goingLandings records it: its index among the
// requests the write or the undo sends, and its method and path.
type goingLanding struct {
	landings goingLandings
	index    int
	request  goingWriteRequest
}

// goingLandingUnread is a request whose landing could not be read back: whether it landed is
// unknown, so the write or the undo that sent it keeps its claim and is never made again.
type goingLandingUnread struct{ err error }

func (e goingLandingUnread) Error() string {
	return "read whether the request landed: " + e.err.Error()
}

func (e goingLandingUnread) Unwrap() error { return e.err }

// on is ctx carrying the landing of the request at index, method and path, for goingInProcess.Do
// to hand that request's route.
func (l *goingLandings) on(ctx context.Context, index int, method, path string) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, goingLandingKey{}, goingLanding{landings: *l, index: index,
		request: goingWriteRequest{Method: method, Path: path, Outcome: goingWriteOutcome{OK: true}}})
}

// element is the column of the run the landings' claim is an element of, and the condition, on
// the element e, that holds of that claim: the pending write of id $3, or the pending undo of
// write $3 claimed at $4, which is empty for a write.
func (l goingLandings) element() (column, condition string) {
	if l.undo == "" {
		return "writes", `(e->>'id' = $3 and coalesce((e->>'pending')::boolean, false) and $4::text = '')`
	}
	return "undos", `(e->>'write_id' = $3 and coalesce((e->>'pending')::boolean, false) and e->>'at' = $4::text)`
}

// goingLandedRead reads whether the pending claim of the run $1 of author $2 - the write of id $3,
// or where $4 is the moment an undo was claimed, that undo of write $3 - records its request $5
// landed.
const goingLandedRead = `
	select exists (
	  select 1 from going_runs r, jsonb_array_elements(case when $4::text = '' then r.writes else r.undos end) e
	  where r.id = $1::uuid and r.author_login = $2 and coalesce((e->>'pending')::boolean, false)
	    and case when $4::text = '' then e->>'id' = $3 else e->>'write_id' = $3 and e->>'at' = $4::text end
	    and coalesce((e->'requests'->$5::int->'outcome'->>'ok')::boolean, false))`

// landed reports whether the request at index landed: whether the claim's record names it landed,
// as its own transaction wrote it (goingLanding.step). It reads on its own bounded context, so a
// request whose time ran out as it landed is still read as landed.
func (l *goingLandings) landed(ctx context.Context, index int) (bool, error) {
	bookkeeping, cancel := goingBookkeeping(ctx)
	defer cancel()
	var landed bool
	if err := l.s.deps.Store.Pool.QueryRow(bookkeeping, goingLandedRead, l.run, l.login, l.write, l.undo, index).Scan(&landed); err != nil {
		return false, goingLandingUnread{err}
	}
	return landed, nil
}

// goingPaddedRequests is the requests of the claim's element e padded with nulls up to index $7,
// so a landing at $7 is set at that index whatever the element held: jsonb_set past the end of an
// array appends rather than leaving a gap, which would put a landing at the wrong index.
const goingPaddedRequests = `(coalesce(e->'requests', '[]'::jsonb) || coalesce((
	select jsonb_agg('null'::jsonb) from generate_series(jsonb_array_length(coalesce(e->'requests', '[]'::jsonb)), $7::int)), '[]'::jsonb))`

// step is the commit step a route's transaction takes while the request runs (running): a
// transaction that wrote - it holds the ROW EXCLUSIVE lock an INSERT, UPDATE, DELETE or MERGE
// takes, as markRequestKeyCommitted reads it, on any table but request_keys and going_runs, which
// this statement's own update takes - records the request landed on the claim's element of the
// run, at the request's own index (goingPaddedRequests), in that same transaction. A transaction
// that only read records nothing. One whose record finds no such claim - the run is gone, or the
// claim was taken off - is refused and rolls back, so no change commits without the record of its
// landing. A route that commits several transactions records the same landing with each. The
// status its answer will carry and the item it makes are not known before the commit; the caller
// adds them once it reads the answer (goingLandings.answered).
func (landing goingLanding) step(running *atomic.Bool) store.BeforeCommit {
	return func(ctx context.Context, tx pgx.Tx) error {
		if !running.Load() {
			return nil
		}
		encoded, err := json.Marshal(landing.request)
		if err != nil {
			return err
		}
		column, condition := landing.landings.element()
		var wrote, recorded bool
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
			with wrote as (
				select txid_current_if_assigned() is not null and exists (
					select 1 from pg_locks l join pg_class c on c.oid = l.relation
					where l.pid = pg_backend_pid() and l.locktype = 'relation' and l.mode = 'RowExclusiveLock'
					  and c.relkind in ('r', 'p') and c.oid not in ('request_keys'::regclass, 'going_runs'::regclass)
				) as yes
			), recorded as (
				update going_runs r set %[1]s = (
				  select jsonb_agg(case when %[2]s then jsonb_set(e, '{requests}',
				    jsonb_set(%[3]s, array[$5::text], $6::jsonb, true)) else e end order by n)
				  from jsonb_array_elements(r.%[1]s) with ordinality x(e, n))
				where r.id = $1::uuid and r.author_login = $2 and (select yes from wrote)
				  and exists (select 1 from jsonb_array_elements(r.%[1]s) e where %[2]s)
				returning 1
			)
			select (select yes from wrote), exists (select 1 from recorded)`, column, condition, goingPaddedRequests),
			landing.landings.run, landing.landings.login, landing.landings.write, landing.landings.undo,
			strconv.Itoa(landing.index), string(encoded), landing.index).Scan(&wrote, &recorded); err != nil {
			return fmt.Errorf("record a going request landed: %w", err)
		}
		if wrote && !recorded {
			return fmt.Errorf("record a going request landed: the run holds no claim of write %s", landing.landings.write)
		}
		return nil
	}
}

// answered adds to the landing of the request at index, where the claim's record holds one, the
// status its answer carried and the item it made (record), so a write taken over after its
// process ended (adoptGoingRunWrite) names them as the request's own answer did. It is the
// writes route's bookkeeping, outside every commit step (store.WithoutBeforeCommit), and best
// effort: a landing it cannot amend still stands, without them.
func (l *goingLandings) answered(ctx context.Context, index int, record goingWriteRequest) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	column, condition := l.element()
	landed := `coalesce((e->'requests'->$7::int->'outcome'->>'ok')::boolean, false)`
	bookkeeping, cancel := goingBookkeeping(ctx)
	defer cancel()
	if err := goingReleased(l.s.appendToGoingRun(store.WithoutBeforeCommit(bookkeeping), fmt.Sprintf(`
		update going_runs r set %[1]s = (
		  select jsonb_agg(case when %[2]s and %[3]s then jsonb_set(e, array['requests', $5::text], $6::jsonb) else e end order by n)
		  from jsonb_array_elements(r.%[1]s) with ordinality x(e, n))
		where r.id = $1::uuid and r.author_login = $2
		  and exists (select 1 from jsonb_array_elements(r.%[1]s) e where %[2]s and %[3]s)`, column, condition, landed),
		l.run, l.login, l.write, l.undo, strconv.Itoa(index), string(encoded), index)); err != nil {
		slog.Warn("dispatch: add a going request's answer to its landing", "error", err)
	}
}
