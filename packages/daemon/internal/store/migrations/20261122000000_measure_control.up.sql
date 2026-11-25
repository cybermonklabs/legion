-- The control of TestADatabaseMainMigratedTakesThisBranchsMigrationsLast: it sorts below main's
-- 20261123050533, which adds this column with if not exists. Never merge this file.
alter table issue_releases add column turn_start timestamptz;
