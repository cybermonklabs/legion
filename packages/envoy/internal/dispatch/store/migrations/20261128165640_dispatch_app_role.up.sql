-- 20261128165640_dispatch_app_role.up.sql — the request role, dispatch_app, and its grants (plan D7, INFRA-6168).
--
-- The request pool runs every statement as dispatch_app (set role), which meets the policies the
-- row-security migrations before this one made. The role is NOLOGIN (only the migrating login
-- takes it), NOBYPASSRLS and no superuser (so row security holds it), NOINHERIT, and owns nothing
-- (a table's owner bypasses row security). A role spans the cluster, so the DO block makes it only
-- where no database of the cluster has yet: it ignores duplicate_object, and unique_violation,
-- which two databases migrating at once on one cluster meet when both make it in the same moment.
-- No down migration and no test teardown drops it.
--
-- The migrating login is granted the role with SET, so it may set role dispatch_app, and without
-- INHERIT, so it never holds the role's privileges as its own; on PostgreSQL 16 and later a login
-- with CREATEROLE holds ADMIN on a role it makes and may grant it to itself. dispatch_app reads and
-- writes every table and uses every sequence of schema public, and the default privileges grant
-- the same on every table and sequence the migrating login makes later, so a later migration's
-- table is the request role's to read and write as it is made; its row security is that
-- migration's to state.
--
-- It is the last migration of this release because a DO block, a GRANT and ALTER DEFAULT
-- PRIVILEGES are no statements the census reads, so they take the census's exemptions from every
-- pending migration after them; every row-security migration of this release comes before it. It
-- touches no table's rows, and the census answers 0. The release before this one never takes the
-- role, so rollback is an image rollback, which leaves the role and its grants in place.

do $$
begin
  create role dispatch_app nologin nobypassrls noinherit;
exception
  when duplicate_object or unique_violation then
    null;
end
$$;

grant dispatch_app to current_user with set true, inherit false;

grant usage on schema public to dispatch_app;
grant select, insert, update, delete on all tables in schema public to dispatch_app;
grant usage, select, update on all sequences in schema public to dispatch_app;
alter default privileges in schema public grant select, insert, update, delete on tables to dispatch_app;
alter default privileges in schema public grant usage, select, update on sequences to dispatch_app;
