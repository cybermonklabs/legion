-- 20261125144002_issues_last_event_at_backfill.up.sql — the time and the seq of the newest event
-- of every issue that has one, on the rows written before 20261125143901.
--
-- Each row whose last_seq names an event and whose pair does not already name it takes that
-- event's created_at and seq: the event of (issue_key, last_seq), read for each row by its own
-- probe of events_issue_key_seq_key, which the correlated subquery makes. A row whose event the
-- probe does not find keeps both null, which the read takes for unknown and probes (api's
-- priorityReadQuery). It writes the pair alone, a pair no release before 20261125143901 reads, so
-- it changes no value any reader had.
--
-- It is one statement in a migration of its own rather than a pass in batches
-- (store.FillEmbeddingProjectKeys is that shape, for work of hours) or a boot pass: issues holds
-- one row per issue, 13,298 in a seed of production's counts, far below
-- pgmigrate.CensusTableLimit, so the census counts it, and the statement costs one index probe
-- of events and one new tuple for each row. It takes issues' ROW EXCLUSIVE lock, which holds off
-- no read and no write of another table, and a lock on each row it writes until it commits: an
-- append to an issue waits at most for this one statement. The ALTER that took the table's
-- ACCESS EXCLUSIVE lock committed in 20261125143901, so no reader of issues waits on it.
--
-- It waits on no row: `unfilled` selects the rows it writes `for no key update skip locked`, and
-- the update joins it, so a row another transaction holds is left out rather than waited for. A
-- request takes its issues' owner locks first (store.LockOwners) and their rows after, in the
-- order its statements reach them: a child's status write takes the child's row and then its
-- parent's, for child.status (api/issue_patch.go, events/broker.go's lockEventOwner). No order of
-- rows matches every such write, and the fill takes no owner lock, which would be one lock for
-- each of its rows, so a fill that waited for a row a write holds could hold the row that write
-- asks for next, and Postgres would abort one of the two, the migration or the request. A fill
-- that takes the rows in key order deadlocked so, as one that takes them as its scan meets them
-- did, beside a child's status write whose parent's key sorts first (dispatch://INFRA-5791,
-- 2026-11-25). A fill that waits on nothing takes no order, so `unfilled` has none.
--
-- A row left out keeps its pair as it was, null on a row written before 20261125143901, which the
-- read takes for unknown and probes; the row's next append through this release writes the pair.
-- A process of the release before this one that appends an event while this runs, or later in
-- the same roll, leaves its row's pair behind last_seq; the read probes that row, and the row's
-- next append through this release writes the pair again.
with unfilled as materialized (
  select key
  from issues
  where last_seq > 0 and last_event_at_seq is distinct from last_seq
  for no key update skip locked
)
update issues
set (last_event_at, last_event_at_seq) = (
  select e.created_at, e.seq
  from events e
  where e.issue_key = issues.key and e.seq = issues.last_seq
)
from unfilled
where issues.key = unfilled.key;
