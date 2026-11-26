-- 20261117053553_going_requests_asked_unseen.up.sql — whether a run's asker could not see the
-- project it asked of when it asked.
--
-- A private project's rule: a run is read by its author only while the author sees the project it was asked
-- of, because its frames hold that project's hits (api/going.go, readGoingRequest). A run asked of
-- a project the asker could not see holds none of that project's hits, since its search reads as
-- the asker; read under that rule all the same, it answered its author 404 where a run asked of a
-- key no project holds answers 200, which told the asker that a hidden key exists. The create now
-- records that the asker did not see the project, and a run so recorded is read by its author
-- whatever the author sees, as a run of a key no project holds is.
--
-- Adding a column with a constant default writes the catalog alone (Postgres 11 on), so it holds
-- the table's ACCESS EXCLUSIVE lock for milliseconds and rewrites no row. Every existing row, and
-- every row the release before this one writes while both serve, reads false, which keeps the
-- rule each was asked under: the release before this one neither reads nor writes the column.
--
-- Its identifier is the UTC moment it was written, as `migrationid` requires of every migration
-- past the numbered series, which closed at 0161.
alter table going_requests
  add column asked_unseen boolean not null default false;
