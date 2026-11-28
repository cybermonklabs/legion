-- packages/envoy/internal/cipher/store/migrations/0053_dispatch_signin_scopes.up.sql
-- A Dispatch sign-in: a device login its person approves for Dispatch alone, with the Dispatch
-- scopes they tick, which enrolls no session and reads no secret (oauthwire.ScopeDispatch).
--
-- credential_requests.dispatch_scopes is what a machine-login record is, written with the record
-- when its device login opens (machine.Service.LoginDevice): null for a machine login, and for a
-- Dispatch sign-in the scopes it asks for, {} for one that asks none. Its lookup, the pending list
-- and its decision read this column alone, so a sign-in reads as one in every state, also once its
-- device code is swept. credential_requests_no_update (0005) already refuses every update of a
-- record, so the column is never changed. A record of a broker from before it reads null, a
-- machine login, which is what every such record is.
--
-- launcher_credentials.dispatch_scopes are the scopes a Dispatch sign-in's person approved
-- (machine.Service.ApplyDecision), written once when its credential is minted: null keeps a
-- machine login apart from a sign-in approved with no scope, and
-- enroll.Service.AuthenticateLauncher refuses every credential whose value is not null. The
-- trigger below holds it to that write, whatever code, language or migration sends an update, as
-- 0048 holds a credential's account: an update that would change it, null to a list or back
-- included, is refused with SQLSTATE 23514 (check_violation), and the statement and its
-- transaction roll back. It fires only on an update whose SET list names the column (`before
-- update of`), so a refresh's new expiry or a revoke costs nothing, and one that names it and
-- leaves it as it was passes, since its condition compares the old row with the new.
--
-- Both columns are additive and nullable with no default, so this rewrites no row and is safe on
-- a live table. refuse_dispatch_scopes_change runs under a search_path of its own, as 0048's
-- function does and for the reason 0047 gives.
alter table credential_requests add column dispatch_scopes text[];

alter table launcher_credentials add column dispatch_scopes text[];

create function refuse_dispatch_scopes_change() returns trigger language plpgsql as $$
begin
  raise exception 'the Dispatch scopes % records are written once, when it is made, and are never changed', tg_table_name
    using errcode = 'check_violation';
end
$$;

create trigger launcher_credentials_dispatch_scopes_unchanged before update of dispatch_scopes on launcher_credentials for each row
  when (old.dispatch_scopes is distinct from new.dispatch_scopes)
  execute function refuse_dispatch_scopes_change();

do $$
begin
  execute format('alter function %I.refuse_dispatch_scopes_change() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
end
$$;
