-- packages/envoy/internal/broker/store/migrations/0040_enrollment_projects.up.sql
-- The Dispatch projects a session's enrollment names: the projects whose private secrets the
-- broker may release to it (dispatch://INFRA-3862, R11). A session names them when it enrols,
-- from AGENT_SECRETS_PROJECTS, and the broker takes the list only when Dispatch says the session's
-- person may see every project on it (enroll.Service.Create, projectaccess.ListAllowed). The list
-- is fixed for the enrollment's life: nothing updates the column, and an adoption takes a
-- predecessor's grants and requests but never its list, so the successor's own list decides.
--
-- Every row this migration finds names no project, which is what each enrolled before this
-- release did, so the column's default is the empty list and the release reads every existing
-- enrollment exactly as before: a secret of no private project goes to it as it always went.
--
-- The two checks are the broker's own rule for a credential's list (oauthwire.ValidProjects): at
-- most 16 keys, each a Dispatch project key (contracts.ProjectKeyPattern, written here once more
-- because a check constraint cannot call Go), and no null element, which array_to_string would
-- otherwise skip. A row the Go side would refuse cannot be written by a later change that forgets
-- to call it.
--
-- A constant default makes the add a change of the catalogue alone, rewriting no row; the two
-- checks read every row once, which on an enrollments table that is never pruned is a scan under
-- the lock the runner's one transaction already holds, as 0012's kind check was.
alter table enrollments add column if not exists projects text[] not null default '{}';
alter table enrollments add constraint enrollments_projects_bounded
  check (cardinality(projects) <= 16 and array_position(projects, null) is null);
alter table enrollments add constraint enrollments_projects_keys
  check (array_to_string(projects, ' ') ~ '^([A-Z][A-Z0-9]{1,9}( [A-Z][A-Z0-9]{1,9})*)?$');
