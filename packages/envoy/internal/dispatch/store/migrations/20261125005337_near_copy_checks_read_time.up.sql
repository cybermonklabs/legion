-- 20261125005337_near_copy_checks_read_time.up.sql — how long each near-copy gate read took, and
-- the step it was in when it ended.
--
-- A near_copy_checks row (20261115182525) records that the gate's read ran out of its bound
-- (reason bound) or failed, but not where the read waited nor how long an answered read took, so
-- the share of creates that go in unchecked cannot be placed (a pool connection, the term pick, the
-- key lexemes or the scoring statement) and the bound (api's duplicateGateTimeout) cannot be set
-- from a measurement. read_ms is the read's wall time in milliseconds, from the moment the gate
-- started it to the moment it answered or ended. read_stage is the step it was in at that moment:
-- connect (taking a pool connection and opening the read transaction with its statement_timeout),
-- terms (picking the new text's rarest words), keys (reading the lexemes the project's issue keys
-- give) or score (scoring the project's open issues, which every answered read ends in). Both are
-- null on every row written before this migration, and only together: a row that holds one holds
-- the other.
--
-- The release before this one names its own columns in every insert and select of
-- near_copy_checks (nearCopyCheckInsert, nearCopyCheckColumns), so it serves on this schema, and
-- a row it writes holds both columns null, which the checks admit.
--
-- The ALTER takes near_copy_checks' ACCESS EXCLUSIVE lock and validates the checks over the
-- table's rows, every one null in both columns; the table holds one row per issue create, so the
-- scan is short, and the runner's lock_timeout (pgmigrate.LockTimeout, 5 s) bounds the wait for
-- the lock. The identifier is the UTC moment this was written, as `migrationid` requires of every
-- migration past the numbered series.
alter table near_copy_checks
  add column read_ms integer check (read_ms >= 0),
  add column read_stage text check (read_stage in ('connect', 'terms', 'keys', 'score')),
  add check ((read_ms is null) = (read_stage is null));
