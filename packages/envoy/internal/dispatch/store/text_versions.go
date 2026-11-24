package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// textVersionsTimeLayout is how a time is written into the relation's SQL and into
// TextVersionsNotReachedError: in UTC, to the microsecond Postgres stores a timestamptz at.
const textVersionsTimeLayout = "2006-01-02T15:04:05.000000Z"

// textVersionsTime writes t in textVersionsTimeLayout, cut (never rounded) to the microsecond.
func textVersionsTime(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format(textVersionsTimeLayout)
}

// TextVersions is the keyword table at one time: each item's version of its searchable text that
// held at Cut (migration 20261120021308, search_text_versions). TextVersionsAt hands it out only
// for a cut the keyword table's fill has reached, so every version that held then is a row.
type TextVersions struct {
	Cut time.Time
}

// SQL is the relation as one parenthesised select, for a statement to join as `from <SQL> v`
// and match, filter, rank, page and cut snippets in itself, as the present search's arms do over
// the items' own tables. Its columns, in this order, are a contract its readers build on
// (TestTheKeywordRelationAnswersTheContractColumns): kind text, id text, issue_key text,
// project_key text, event_id bigint, event_seq integer, started_at timestamptz, body text,
// spec_artifact_id uuid, spec_version integer, search tsvector and deleted_at timestamptz, which
// is null until the posts carry a deleted_at of their own.
//
// A version holds at the cut when it started at or before the cut and no later version of the
// same item, in its owner's seq order, started at or before the cut; so an item has at most one
// version at any time, and none before its first version started. The cut is written into the
// text as a literal, cut to the microsecond in UTC, from the time value alone and never from text
// a request carried. Its tables are named with their schema, so a common table expression of the
// statement that joins it cannot stand in for them.
func (v TextVersions) SQL() string {
	cut := "'" + textVersionsTime(v.Cut) + "'::timestamptz"
	return `(select v.kind, v.id, v.issue_key, v.project_key, v.event_id, v.event_seq, v.started_at, v.body,
        v.spec_artifact_id, v.spec_version, v.search, null::timestamptz as deleted_at
   from public.search_text_versions v
  where v.started_at <= ` + cut + `
    and not exists (select from public.search_text_versions l
                     where l.kind = v.kind and l.id = v.id and l.event_seq > v.event_seq
                       and l.started_at <= ` + cut + `))`
}

// TextVersionsNotReachedError is TextVersionsAt's refusal of a cut the keyword table's fill has
// not reached: some event written at or before Cut may have no row yet. Reach is the latest time
// the fill has reached, and Reached is false, with Reach zero, while the fill has reached no time
// at all.
type TextVersionsNotReachedError struct {
	Cut, Reach time.Time
	Reached    bool
}

func (e *TextVersionsNotReachedError) Error() string {
	if !e.Reached {
		return "the keyword table's fill has reached no time yet"
	}
	return "the keyword table's fill has reached " + textVersionsTime(e.Reach) + ", before the cut " + textVersionsTime(e.Cut)
}

// textVersionsCursorQuery reads the fill's one row: its cursor and the reach it stored.
const textVersionsCursorQuery = `select event_id, reach from public.search_text_versions_fill where id = 1`

// scanTextVersionsCursor scans textVersionsCursorQuery's row. found is false when the fill has
// written no row yet; reach is nil while it has stored none.
func scanTextVersionsCursor(row pgx.Row) (cursor int64, reach *time.Time, found bool, err error) {
	if err := row.Scan(&cursor, &reach); errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, false, nil
	} else if err != nil {
		return 0, nil, false, fmt.Errorf("read the keyword table's fill cursor: %w", err)
	}
	return cursor, reach, true, nil
}

// TextVersionsAt answers the keyword table at cut, or *TextVersionsNotReachedError when the fill
// has not reached cut, so a search at a past time never reads a table that lacks a version that
// held then. The cut is judged as SQL writes it, cut to the microsecond.
//
// With no cursor row the fill has reached no time, and the error says so (Reached false). A cut
// at or before the stored reach is answered from that row alone. A cut past it has the reach
// computed again (TextVersionsReach), so a database whose fill is caught up and quiet is not
// refused a cut it has reached merely because no batch has stored a later reach since; the cut
// is answered when it is at or before the greater of the two, and refused naming that reach
// otherwise.
//
// Its statements decide whether a cut can be answered, so they read the present tables and never
// the database as it stood at a past time: the fill's row, public.events, pg_stat_activity and
// pg_locks, each table of Dispatch's named with its schema.
func (s *Store) TextVersionsAt(ctx context.Context, cut time.Time) (TextVersions, error) {
	at := cut.Truncate(time.Microsecond)
	_, stored, found, err := scanTextVersionsCursor(s.Pool.QueryRow(ctx, textVersionsCursorQuery))
	if err != nil {
		return TextVersions{}, err
	}
	if !found {
		return TextVersions{}, &TextVersionsNotReachedError{Cut: cut}
	}
	if stored != nil && !at.After(*stored) {
		return TextVersions{Cut: cut}, nil
	}
	reach, reached, err := s.TextVersionsReach(ctx)
	if err != nil {
		return TextVersions{}, err
	}
	if reached && !at.After(reach) {
		return TextVersions{Cut: cut}, nil
	}
	return TextVersions{}, &TextVersionsNotReachedError{Cut: cut, Reach: reach, Reached: reached}
}

// TextVersionsReach answers how far the keyword table's fill has reached: the latest time at or
// before which every event, one a transaction still open will commit included, has its rows.
// reached is false while the fill has written no cursor row, and while it has stored no reach and
// none can be computed now.
//
// It reads the cursor row and computes the reach again (ComputeTextVersionsReach) in one
// read-only READ COMMITTED transaction, and answers the greater of the stored reach and the
// computed one, so the reach it answers is never earlier than the one the fill stored.
func (s *Store) TextVersionsReach(ctx context.Context) (reach time.Time, reached bool, err error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return time.Time{}, false, fmt.Errorf("begin the keyword table's reach read: %w", err)
	}
	defer tx.Rollback(ctx)
	cursor, stored, found, err := scanTextVersionsCursor(tx.QueryRow(ctx, textVersionsCursorQuery))
	if err != nil || !found {
		return time.Time{}, false, err
	}
	computed, ok, err := ComputeTextVersionsReach(ctx, tx, cursor)
	if err != nil {
		return time.Time{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, false, fmt.Errorf("end the keyword table's reach read: %w", err)
	}
	switch {
	case stored != nil && (!ok || !computed.After(*stored)):
		return *stored, true, nil
	case ok:
		return computed, true, nil
	default:
		return time.Time{}, false, nil
	}
}

// textVersionsOpenWritersQuery reads, over the other backends of this database that can write an
// event, the oldest start of a transaction whose start it can read, and whether one is in a
// transaction whose start it cannot read.
//
// A backend can write an event when its login is a superuser, or is a member of a role with
// INSERT on public.events (its login included: SET ROLE works for a member whatever INHERIT
// says, which has_table_privilege on the login alone misses for a NOINHERIT member), or when its
// usesysid names a role this query cannot resolve. Only client backends are read: those
// Postgres shows as such, and, to a role without pg_read_all_stats, another role's backend, which
// Postgres shows with backend_type, state and xact_start null and usesysid shown. A row with
// usesysid null too is no client (only client backends, WAL senders and background workers record
// a user, and a client of another role shows its role), so it counts in neither answer.
//
// The second answer is the census's test (pgmigrate.Census's long transactions): a backend in a
// transaction locks its virtual transaction id, so one that shows no xact_start yet holds that lock
// and is not idle is in a transaction whose start this role cannot read; an idle one that holds it
// began its transaction after pg_stat_activity was read, which is later than the T0 read before.
// state is compared with `is distinct from`, since a hidden row's state is null.
const textVersionsOpenWritersQuery = `
with inserters as materialized (
       select r.oid from pg_roles r where has_table_privilege(r.oid, 'public.events', 'INSERT')),
     in_transaction as materialized (
       select distinct l.pid from pg_locks l where l.locktype = 'virtualxid')
select min(a.xact_start) filter (where a.backend_type = 'client backend'),
       coalesce(bool_or(a.xact_start is null and a.state is distinct from 'idle' and x.pid is not null), false)
  from pg_stat_activity a
  left join pg_roles u on u.oid = a.usesysid
  left join in_transaction x on x.pid = a.pid
 where a.datname = current_database() and a.pid <> pg_backend_pid()
   and (a.backend_type = 'client backend' or a.backend_type is null and a.usesysid is not null)
   and case when u.oid is null then true
            when u.rolsuper then true
            else exists (select from inserters r where pg_has_role(a.usesysid, r.oid, 'MEMBER'))
       end`

// textVersionsFillWindow is how far behind the newest event the cursor may be for the reach to be
// computed at all: further behind, the fill is catching up and every cut is refused at the stored
// reach, and the oldest event above the cursor is not worth a read.
const textVersionsFillWindow = 5000

// textVersionsStartQuery is the reach's first statement: T0, the transaction's isolation, and
// whether the cursor ($1) is at most textVersionsFillWindow ($2) ids behind the newest event and
// at or below it, so a cursor outside the window reads no backend.
const textVersionsStartQuery = `select clock_timestamp(), current_setting('transaction_isolation'),
       coalesce((select max(id) from public.events), 0) between $1 and $1 + $2`

// textVersionsUnwrittenQuery answers whether the cursor is at most textVersionsFillWindow ids
// behind the newest event and at or below it, and then the oldest created_at of the events
// above it, which the fill has not written. Both are read in one statement and so one snapshot,
// where every id above the cursor is at most the newest one, so the bound on the ids, which keeps
// the read to the window's rows whatever the plan, leaves no event out.
const textVersionsUnwrittenQuery = `
select n.near,
       case when n.near then (select min(e.created_at) from public.events e where e.id > $1 and e.id <= $1 + $2) end
  from (select coalesce(max(id), 0) between $1 and $1 + $2 as near from public.events) n`

// ComputeTextVersionsReach computes the keyword table's reach for a fill whose cursor is cursor,
// on tx, which must be a READ COMMITTED transaction: the latest time T such that every event whose
// created_at is at or before T has its rows, an event a transaction still open will commit
// included. Its bool is false when no value can be computed now, and the stored reach stands:
// while a backend that can write an event is in a transaction whose start this role cannot read,
// while the cursor is more than 5,000 ids behind the newest event, and while the cursor is past
// the newest event, which is a log reset the fill has not met yet.
//
// One computation, each step its own statement, in this order:
//
//  1. T0, clock_timestamp(), with whether the cursor is in the window, at most 5,000 ids behind
//     the newest event and at or below it (textVersionsStartQuery). Outside it no value is
//     computed, and no later step runs.
//  2. pg_stat_clear_snapshot(), so the next read of pg_stat_activity is fresh rather than the
//     transaction's copy, then A, the oldest xact_start of another backend of this database that
//     can write an event, and whether one is in a transaction whose start cannot be read
//     (textVersionsOpenWritersQuery).
//  3. In a later statement, with a snapshot of its own: whether the cursor is still in the
//     window, and B, the oldest created_at of the committed events above the cursor
//     (textVersionsUnwrittenQuery).
//  4. The reach: least(A, B, T0 − 1 s) − 1 µs.
//
// The order is what makes it safe. An event's created_at is the start of the transaction that
// wrote it. A transaction open at step 2 is in A; one that commits after step 2 and before step
// 3's snapshot is in B; one that starts after step 2 started after T0. The one-second margin
// covers the moment between a transaction's start and its report of xact_start.
//
// The fill calls it inside each batch's transaction, and TextVersionsReach inside its own.
func ComputeTextVersionsReach(ctx context.Context, tx pgx.Tx, cursor int64) (time.Time, bool, error) {
	var t0 time.Time
	var isolation string
	var near bool
	if err := tx.QueryRow(ctx, textVersionsStartQuery, cursor, textVersionsFillWindow).Scan(&t0, &isolation, &near); err != nil {
		return time.Time{}, false, fmt.Errorf("compute the keyword table's reach: read the time and the newest event: %w", err)
	}
	// Under a snapshot taken once per transaction, step 3 would read the events as they stood
	// before step 2, and miss one committed between the two.
	if isolation != "read committed" {
		return time.Time{}, false, fmt.Errorf("compute the keyword table's reach: the transaction's isolation is %s, not read committed", isolation)
	}
	if !near {
		return time.Time{}, false, nil
	}
	if _, err := tx.Exec(ctx, `select pg_stat_clear_snapshot()`); err != nil {
		return time.Time{}, false, fmt.Errorf("compute the keyword table's reach: clear the activity snapshot: %w", err)
	}
	var oldestOpen *time.Time
	var unreadable bool
	if err := tx.QueryRow(ctx, textVersionsOpenWritersQuery).Scan(&oldestOpen, &unreadable); err != nil {
		return time.Time{}, false, fmt.Errorf("compute the keyword table's reach: read the open transactions: %w", err)
	}
	if unreadable {
		return time.Time{}, false, nil
	}
	var oldestUnwritten *time.Time
	if err := tx.QueryRow(ctx, textVersionsUnwrittenQuery, cursor, textVersionsFillWindow).Scan(&near, &oldestUnwritten); err != nil {
		return time.Time{}, false, fmt.Errorf("compute the keyword table's reach: read the events past the cursor: %w", err)
	}
	if !near {
		return time.Time{}, false, nil
	}
	reach := t0.Add(-time.Second)
	for _, bound := range []*time.Time{oldestOpen, oldestUnwritten} {
		if bound != nil && bound.Before(reach) {
			reach = *bound
		}
	}
	return reach.Add(-time.Microsecond), true, nil
}
