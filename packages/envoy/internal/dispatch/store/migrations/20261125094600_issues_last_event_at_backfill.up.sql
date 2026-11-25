-- 20261125094600_issues_last_event_at_backfill.up.sql — the time and the seq of the newest event
-- of every issue that has one, on the rows written before 20261125094500.
--
-- Each row whose last_seq names an event and whose pair does not already name it takes that
-- event's created_at and seq: the event of (issue_key, last_seq), read for each row by its own
-- probe of events_issue_key_seq_key, which the correlated subquery makes. A row whose event the
-- probe does not find keeps both null, which the read takes for unknown and probes (api's
-- priorityReadQuery). It writes the pair alone, a pair no release before 20261125094500 reads, so
-- it changes no value any reader had.
--
-- It is one statement in a migration of its own rather than a pass in batches
-- (store.FillEmbeddingProjectKeys is that shape, for work of hours) or a boot pass: issues holds
-- one row per issue, 13,298 in a seed of production's counts, far below
-- pgmigrate.CensusTableLimit, so the census counts it, and the statement costs one index probe
-- of events and one new tuple for each row. It takes issues' ROW EXCLUSIVE lock, which holds off
-- no read and no write of another table, and a lock on each row it writes until it commits: an
-- append to an issue waits at most for this one statement. The ALTER that took the table's
-- ACCESS EXCLUSIVE lock committed in 20261125094500, so no reader of issues waits on it.
--
-- It takes the rows in key order (`key collate "C"`): `unfilled` selects the rows it writes in
-- that order `for no key update`, and the update joins it, so each row's lock is taken before the
-- update reaches any row. That is the order in which store.LockOwners takes the rows of several
-- issues (ownerRowsIssues in store/ownerlock.go), so the fill and such a write take any two issue
-- rows in one order. An update that locked each row when its scan reached it could hold one row
-- of such a write and wait for another the write holds, while the write waits for the first, and
-- Postgres would abort one of the two, the migration or the write.
--
-- A process of the release before this one that appends an event while this runs, or later in
-- the same roll, leaves its row's pair behind last_seq; the read probes that row, and the row's
-- next append through this release writes the pair again.
with unfilled as materialized (
  select key
  from issues
  where last_seq > 0 and last_event_at_seq is distinct from last_seq
  order by key collate "C"
  for no key update
)
update issues
set (last_event_at, last_event_at_seq) = (
  select e.created_at, e.seq
  from events e
  where e.issue_key = issues.key and e.seq = issues.last_seq
)
from unfilled
where issues.key = unfilled.key;
