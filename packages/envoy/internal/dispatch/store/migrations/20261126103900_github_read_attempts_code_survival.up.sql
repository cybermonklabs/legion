-- 20261126103900_github_read_attempts_code_survival.up.sql — the code survival run joins the reads.
--
-- dispatch://INFRA-3206: the daily run that reads each repository's patches from GitHub
-- (internal/dispatch/codesurvival) records its attempts as the fifth read of the record
-- github_read_attempts (20261118020802), `code_survival` (githubreads.CodeSurvival), which GET
-- /healthz names beside the four before it.
--
-- The check on `read` is dropped and added again in one statement, so no moment passes with the
-- column unchecked, and the new one admits everything the old one did: the four names
-- 20261118020802 holds it to, the only migration that wrote this check, and `code_survival`.
-- Widening it is safe beside the release before this migration. That release records the four
-- names alone, each of which the wider check admits, and it reads the table without asking what
-- values `read` may hold. It is the next release that writes `code_survival`, and by then every
-- server runs with this check (internal/dispatch/store/README.md).
--
-- No row is backfilled and no row is rewritten. The add records the check in the catalog and scans
-- no stored row (`not valid`), which is the rule for a constraint a migration adds to a table that
-- already has rows (packages/envoy/cmd/dispatch/README.md): the statement takes ACCESS EXCLUSIVE on
-- github_read_attempts for the catalog write alone, which holds at most one row for each read, and
-- the runner's lock_timeout (pgmigrate.LockTimeout, 5 s) bounds its wait. The check still refuses
-- every row written or updated after it, which is what the new read needs. No stored row would
-- violate it, and no release validates it: the check it replaces is narrower, so every row the
-- table holds already satisfies the new one. The constraint keeps its name, so the refusal of a
-- read that is none of the five names the same constraint as before. The identifier is the UTC
-- moment this file was created, as `migrationid` requires of every migration past the numbered
-- series, which closed at 0161 (cmd/dispatch/README.md, "Database migrations").
alter table github_read_attempts
    drop constraint github_read_attempts_read_check,
    add constraint github_read_attempts_read_check check (read in ('webhook_redelivery', 'architecture_import', 'delivery_reconcile', 'ci_reconcile', 'code_survival')) not valid;
