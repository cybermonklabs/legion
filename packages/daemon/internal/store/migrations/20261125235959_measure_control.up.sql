-- 20261125235959_measure_control.up.sql — a planted control of
-- TestADatabaseMainMigratedTakesThisBranchsMigrationsLast. Never merge.
alter table issue_plans add column if not exists earlier_givers text;
