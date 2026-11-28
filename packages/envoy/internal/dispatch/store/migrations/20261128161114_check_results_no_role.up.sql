-- 20261128161114_check_results_no_role.up.sql
-- INFRA-3994: a result's stored role is the role its session holds on the recording issue, as
-- the Legion daemon published it (issue_role_holders), never the role the request names. A
-- session or a person that holds no role of the issue records its result with no role, stored as
-- the empty string (api/results.go, resultRoleOf). The role check of 20261102122200_check_results
-- refuses '', so it is widened to take it beside every role it took.
--
-- The statement widens what the schema allows and narrows nothing, which a rolling update requires
-- (packages/envoy/cmd/dispatch/README.md, "Database migrations"): the release before this one
-- writes only roles of a-z and -, which this check still takes, so the two builds run against it
-- at once. The check is dropped without `cascade` and added back `not valid`, which records it for
-- every row written from now on without reading a stored one: no stored row can fail a check wider
-- than the one it was written under, so the read a validation makes would find nothing.
--
-- It is one of the census's catalog-only forms (pgmigrate's catalogOnlyAlterTable: drop constraint
-- without cascade, add check not valid), so it is exempt from the table size limit at any size, and
-- it takes ACCESS EXCLUSIVE on check_results for its catalogue write alone, under the runner's
-- lock_timeout. No row is refused or rewritten, so the census answers 0.
--
-- Rolling back past this migration is an image rollback alone while no row carries the role '';
-- once one does, an earlier build reads such a row and prints its role as empty, and the check has
-- to stay as this migration leaves it: putting the narrower one back refuses those rows.
alter table check_results
  drop constraint check_results_role_check,
  add constraint check_results_role_check
    check (role = '' or role ~ '^[a-z-]{1,32}$') not valid;
