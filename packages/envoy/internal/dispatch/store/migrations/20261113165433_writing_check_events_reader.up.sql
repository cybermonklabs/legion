-- 20261113165433_writing_check_events_reader.up.sql — each writing check decision names its reader.
--
-- INFRA-3231: the check reads a write only when a person reads it. The server decides that person
-- from the record before the check reads anything (api/writing_check_reader.go): the person an
-- ask waits on, the person whose text a reply answers, the issue's assignee or the project's
-- default assignee, or the person in a direct conversation. `reader` is that person's login, and
-- null where the write has no person as its reader: a send to an agent, a broadcast, a reply in a
-- direct conversation between two sessions, or a record that names nobody. The server records the
-- reader on every row, so a row the check exempted by its writer also says whom the text reached.
--
-- A write an agent makes with no person as its reader is not read, and it is recorded with the new
-- outcome `exempt_no_reader`. The outcome check is dropped and added again in one statement, so no
-- moment passes with the column unchecked, and the new check admits everything the old one did.
-- Widening it is safe beside the release before this one, which writes only outcomes the wider
-- check admits and reads the column without asking what values it may hold.
--
-- The column is a plain nullable add with no default, a catalog change that rewrites no row. The
-- check is added `not valid`, the rule for a constraint a migration adds to a table that may
-- already hold rows (packages/envoy/cmd/dispatch/README.md, "Database migrations"): the statement
-- takes ACCESS EXCLUSIVE on writing_check_events for the catalog write alone and scans no stored
-- row. Every stored row satisfies it, since the check it replaces is narrower, and every row
-- written after it is checked. No row is backfilled: a row written before this migration recorded
-- no reader, and its null says so. Its identifier is the UTC moment it was written, as
-- `migrationid` requires of every migration past the numbered series.
alter table writing_check_events
  add column reader text,
  drop constraint writing_check_events_outcome_check,
  add constraint writing_check_events_outcome_check check (outcome in (
    'refused', 'forced', 'exempt_person', 'exempt_daemon', 'exempt_system', 'exempt_unread_upload',
    'exempt_no_reader', 'clean', 'reader_timeout', 'reader_error', 'reader_malformed', 'reader_budget',
    'candidate_cap', 'model_mismatch', 'no_model'
  )) not valid;
