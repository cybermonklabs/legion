-- The green control of TestADatabaseMainMigratedTakesThisBranchsMigrationsLast: a branch migration
-- that sorts below main's newest and conflicts with nothing. Never merge this file.
alter table issue_releases add column measure_green text;
