-- 20261117022936_legion_current.up.sql — the newest row of each field the Legion sampler records.
--
-- dispatch://INFRA-1077: three readers of what Dispatch holds of a Legion daemon's state take the
-- newest row of each issue and field of legion_observations (20261103170923): the sampler's write,
-- which compares each read against them (legion's recordObservations); the Legion board, whose
-- cards they are; and the issues the daemon reports, which the board and the metrics place an
-- issue in a tree by. The last two are one statement (legion.ReadCurrent). legion_observations
-- keeps every row it was ever given, so taking the newest of each field from it read the project's
-- whole history: on the test Postgres, at 3,000 issues of 6 fields each and nothing moving, a
-- pass's write took 201 ms over 180,000 rows and 835 ms over 720,000, and the board's read 203 ms
-- and 869 ms, every minute and every board read, more each month the table grows. Read from this
-- table, the same write takes 43 ms and 32 ms, and the board's read, which takes its header and
-- its cards in one snapshot, 39 ms and 44 ms (the median of 20 reads, measured 2026-11-17),
-- whatever the history holds; it grows only with the issues the daemon has ever reported, a
-- dropped issue's fields staying as `null` rows. A first read of such a daemon, which writes all
-- 18,000 rows, pays for it once: 664 ms where it took 365 ms.
--
-- legion_current is one row per (project, issue_key, field) legion_observations holds a row of:
-- that field's newest row, its value and when it was observed. A `null` row is kept like any other,
-- since a field the newest good read no longer carries is written `null` (recordObservations) and
-- is the newest row all the same: the readers leave such a row out where they want a value, and
-- legion.ReadCurrent says which issues the rows report.
--
-- - The primary key is the row's identity and the readers' one index: every read takes one
--   project's rows.
-- - No row is deleted but by the project's cascade: a row is an issue's last word, which
--   legion_observations keeps too.
--
-- legion_observations_current keeps it, as 20261101182454's trigger keeps document_search: one
-- statement-level trigger after every insert into legion_observations, whoever writes it, upserts
-- the newest row of each field the statement wrote, in the writer's transaction. A row written
-- older than the one this table holds leaves it, which is what "newest" means: the unique index
-- legion_observations_point gives one field one row per moment, so no two rows tie. Statement-level
-- because a read writes all of its rows in one statement: the first read of a daemon writes every
-- field of every issue, which is one upsert of the statement's rows rather than one per row, and a
-- read in which nothing moved runs it over no rows.
--
-- The trigger is created before the fill, and its SHARE ROW EXCLUSIVE on legion_observations holds
-- every insert until the commit, so no row lands between the two: a database that already holds
-- observations reads the same newest rows from this table as from the history.
--
-- A new table and a trigger on one 20261103170923 created in this same release, which no earlier
-- release names: nothing existing is refused or rewritten, and the census answers 0. The foreign
-- key holds projects in SHARE ROW EXCLUSIVE until the migration commits, the fill included. The
-- identifier is the UTC moment this was written, as `migrationid` requires of every migration past
-- the numbered series.
create table legion_current (
  project text not null references projects(key) on delete cascade,
  issue_key text not null,
  field text not null,
  value jsonb not null,
  observed_at timestamptz not null,
  primary key (project, issue_key, field)
);

create function legion_observations_current() returns trigger language plpgsql as $$
begin
  insert into legion_current (project, issue_key, field, value, observed_at)
  select distinct on (project, issue_key, field) project, issue_key, field, value, observed_at
  from written
  order by project, issue_key, field, observed_at desc
  on conflict (project, issue_key, field) do update
     set value = excluded.value, observed_at = excluded.observed_at
   where excluded.observed_at > legion_current.observed_at;
  return null;
end;
$$;

create trigger legion_observations_current after insert on legion_observations
  referencing new table as written
  for each statement execute function legion_observations_current();

insert into legion_current (project, issue_key, field, value, observed_at)
select distinct on (project, issue_key, field) project, issue_key, field, value, observed_at
from legion_observations
order by project, issue_key, field, observed_at desc;
