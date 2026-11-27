-- 20261127022136_check_fail_bindings.up.sql
--
-- A failed after-merge check result stays bound to its first matching failure row.
-- The first match finds the deploy that first shipped the commit and writes or finds the row
-- keyed by check:<document>/<block>@<deploy>. It binds the result to that row in the same
-- transaction that writes it (signals.RecordCheckFailure). A bound result stays on its row: a
-- repeat writes nothing, a deploy row found later that shipped the commit earlier makes it no
-- second row, and its retraction rejects the row it is bound to unless another result bound to
-- that row still stands (signals.RejectRetractedCheck).
--
-- The columns:
--   result_id   the check_results row; a result is bound once.
--   failure_id  the production_events check_fail row it made or stands behind. Several results of
--               one check whose commits one deploy first shipped stand behind one row.
--
-- check_fail_bindings_failure indexes the foreign key when a failure row is deleted.
--
-- The release before this one has no production event record, so there is nothing to backfill.
-- The foreign keys take SHARE ROW EXCLUSIVE on their referenced tables. No existing row is
-- rewritten or refused, and the release before this one neither reads nor writes the new table.
-- The census answers 0.
create table check_fail_bindings (
  result_id  uuid primary key references check_results (id) on delete cascade,
  failure_id uuid not null references production_events (id) on delete cascade
);
create index check_fail_bindings_failure on check_fail_bindings (failure_id);
