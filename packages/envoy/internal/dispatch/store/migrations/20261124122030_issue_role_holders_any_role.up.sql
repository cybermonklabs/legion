-- 20261124122030_issue_role_holders_any_role.up.sql — a role holder of a role the deployment defines.
--
-- A Legion deployment defines roles of its own in its settings (`roles`), and a step of an issue's
-- plan names one as it names Legion's six. The daemon publishes the session of each live claim of
-- an issue, whatever its role (PUT /api/v1/issues/{key}/role-holders, api/issue_role_holders.go),
-- and the check 20261116163813 put on issue_role_holders.role names the six alone, so the route
-- refused the whole set of an issue with a live claim of such a role, and the daemon dropped it.
-- The route now takes a role of the role grammar but the controller's (legionrole.IsRole), and
-- this check takes the same names: `^[a-z][a-z0-9-]{0,31}$`, other than `controller`, which is the
-- project's role and no issue's.
--
-- The check widens what the table takes and narrows nothing, so it rolls
-- (packages/envoy/cmd/dispatch/README.md, "Database migrations"). It follows every legion-envoy
-- release up to the one that carries it: v34.11.0 to v34.11.8, whose newest migration is
-- 20261121060636, and v34.12.0 to v34.14.0 on 2026-11-23, whose newest is 20261123080000. Its
-- version stands below that one, which is no refusal: the runner applies every version
-- schema_migrations does not record, so a store any of them migrated applies this file at the next
-- boot. Each of those releases writes only the six, each a name of the grammar, and reads a stored
-- role as text, ordering a role it does not list last. It is dropped without `cascade` and added
-- back `not valid`, which records the check for every row written from now on and reads no stored
-- row: every stored row was written
-- under the narrower check, which no name of it can fail. Each statement is one of the census's
-- catalog-only forms (pgmigrate's catalogOnlyAlterTable), so it is exempt from the table size limit
-- at any size, and holds ACCESS EXCLUSIVE on issue_role_holders for its catalogue write alone, under
-- the runner's lock_timeout. No row is refused or rewritten, so the census answers 0. Rolling back
-- past this migration is an image rollback alone while no row names a role outside the six; once
-- one does, that row is deleted, or the check widened again by hand, before an earlier set is
-- restored.
alter table issue_role_holders
  drop constraint issue_role_holders_role_check,
  add constraint issue_role_holders_role_check
    check (role ~ '^[a-z][a-z0-9-]{0,31}$' and role <> 'controller') not valid;
