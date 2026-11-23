// The reads of this database's event writers (the package documentation's "Safe horizon"): the
// common table expressions that list them, and the three statements built on those, the
// observation, the stall diagnosis and WriterFloor's read.

package events

// eventWritersCTE is the common table expressions every read of the event writers (the package
// documentation's "Safe horizon") starts with, over pg_locks and the reader's snapshot's xmax.
var eventWritersCTE = eventWritersFrom("pg_locks", "pg_snapshot_xmax(pg_current_snapshot())")

// eventWritersFrom is eventWritersCTE over the lock rows of locks, a from-item with pg_locks's
// columns, and the xmax xmax, an expression of type xid8; a test passes rows and an xmax of its
// own, which no server it can run against has. locks is one read of the lock rows, kept to the
// rows the read needs (every transactionid lock, and the relation locks on this database's events),
// each transactionid widened from the 32 bits the lock table gives to xid, the 64-bit id
// pg_current_xact_id answers, against xmax, from which no running id is more than 2^31. writers
// has a row for each RowExclusiveLock on events in this database that a transaction
// other than the reader's holds: its pid, the backend's or null for a prepared transaction, and
// its xid, the least of the widened ids it holds an ExclusiveLock on under its virtualtransaction,
// or null when the lock table lists none. Each id is widened before the least is taken: a
// subtransaction's id past a multiple of 2^32 has low bits below its top id's. next is
// materialized so that the snapshot is read once, not once for each lock row (pg_current_snapshot
// copies every running id: 19 ms a read beside 300 busy clients measured, against 3 ms
// materialized), and writers so that its subquery runs once for each lock.
func eventWritersFrom(locks, xmax string) string {
	return `
	next as materialized (
		select ` + xmax + `::text::numeric as nx
	), locks as materialized (
		select l.locktype, l.database, l.relation, l.virtualtransaction, l.pid, l.mode, l.granted,
			l.x32 + 4294967296 * (floor(next.nx / 4294967296)
				+ case
					when l.x32 - mod(next.nx, 4294967296) > 2147483648 then -1
					when mod(next.nx, 4294967296) - l.x32 > 2147483648 then 1
					else 0
				end) as xid
		from (
			select *, transactionid::text::numeric as x32 from ` + locks + `
			where locktype = 'transactionid' or (locktype = 'relation'
				and database = (select oid from pg_database where datname = current_database())
				and relation = 'events'::regclass)
		) l, next
	), writers as materialized (
		select t.pid, (
			select min(x.xid) from locks x
			where x.virtualtransaction = t.virtualtransaction and x.locktype = 'transactionid'
				and x.mode = 'ExclusiveLock' and x.granted
		) as xid
		from locks t
		where t.locktype = 'relation' and t.granted and t.pid is distinct from pg_backend_pid()
			and t.relation = 'events'::regclass and t.mode = 'RowExclusiveLock'
	)`
}

// oldestWriter is the oldest transaction id among eventWritersFrom's writers, an aggregate over
// them: 0 when one of them has no id the read can date, which proves nothing, and null when there
// are none. The observation and oldestEventWriterSelect both read the writers through it.
const oldestWriter = `case when bool_or(xid is null) then 0 else min(xid) end`

// horizonObserveSQL is an observation (the package documentation's "Safe horizon"): A and the
// created_at of its event, as the statement's snapshot S sees them; F; the xmax of S and whether
// S lists no transaction in progress, from which take reads whether S proved A itself; and X,
// which pg_current_xact_id assigns while the statement runs, after S was taken. Every
// pg_current_snapshot in it is S: a statement reads one snapshot throughout. $1 is the X of the
// newest observation still pending, 0 when none is, in decimal text: compared as numeric, it is
// exact for every 64-bit id, where a bigint holds none past 2^63 - 1. F is the xmin of S when
// that already passes $1, and the lock table goes unread; otherwise it is the greater of that xmin
// and the oldest transaction id of an event writer, which is 0 when the read lists a writer it
// cannot date and X when it lists none.
var horizonObserveSQL = `
	with` + eventWritersCTE + `
	select m.id, e.created_at,
		(select case when s.xmin > $1::text::numeric then s.xmin else greatest(s.xmin, (
			select least(` + oldestWriter + `, pg_current_xact_id()::text::numeric) from writers
		)) end from (select pg_snapshot_xmin(pg_current_snapshot())::text::numeric as xmin) s)::text,
		pg_snapshot_xmax(pg_current_snapshot())::text,
		not exists (select 1 from pg_snapshot_xip(pg_current_snapshot())),
		pg_current_xact_id()::text
	from (select coalesce(max(id), 0) as id from events) m left join events e on e.id = m.id
`

// StallHolderSQL names the oldest event writer of this database, a backend's transaction or a
// prepared transaction (eventWritersCTE): the one that keeps a pending observation from being
// proven safe. A writer the read cannot date comes first, since it holds every observation back.
// warnStall reads it, and a test's wait on the horizon reads it too (internal/dispatch/horizontest),
// when the warning is late.
var StallHolderSQL = `
	with` + eventWritersCTE + `
	select kind, pid, started, state, application_name, database, query from (
		select 'backend' as kind, a.pid, a.xact_start as started, coalesce(a.state, '') as state,
			coalesce(a.application_name, '') as application_name, coalesce(a.datname::text, '') as database,
			left(coalesce(a.query, ''), 90) as query, w.xid
		from writers w join pg_stat_activity a on a.pid = w.pid
		union all
		select 'prepared', 0, p.prepared, 'prepared', '', p.database::text,
			left('prepared transaction ' || p.gid || ' owned by ' || p.owner::text, 90), w.xid
		from writers w join pg_prepared_xacts p on p.transaction::text::numeric = mod(w.xid, 4294967296)
		where w.pid is null
	) holder
	order by xid nulls first
	limit 1
`

// oldestEventWriterSQL answers the oldest transaction id an event writer of this database holds
// (eventWritersCTE), widened to the 64-bit id marks carry: 0 when the read lists a writer it cannot
// date, which no mark is older than, and null when there is none. A transaction that holds a
// transaction id alone (a read in a transaction, a write to another table, an operator's open
// session) holds no event id: any it draws later is drawn after this read's marks. WriterFloor
// answers from it at once, from marks taken before the call; the horizon waits for an observation
// taken after one it has made.
var oldestEventWriterSQL = "with" + eventWritersCTE + oldestEventWriterSelect

// oldestEventWriterSelect is oldestEventWriterSQL's answer over the writers eventWritersFrom reads.
const oldestEventWriterSelect = `
	select (` + oldestWriter + `)::text from writers
`
