-- packages/envoy/internal/cipher/store/migrations/0046_recorded_accounts_unchanged.up.sql
-- A credential's account is written once, when the credential is made, and never changed: the
-- account a person's sign-in on a machine records (person_signins' issuer and subject), the
-- account that approved a machine login (launcher_credentials' approver_issuer and
-- approver_subject), the account an approval-page session signed in as (browser_sessions' issuer
-- and subject), and the account a token's trade recorded on the token itself (the account_issuer
-- and account_subject of the session an access or refresh token stores in its request). Every
-- reader of a credential's account reads it from that credential's own row, so a credential acts
-- as the account it was made by and no other. A sign-in that records no account is never given
-- one: where the broker asks Dispatch its next trade ends it and writes a row of its own, and
-- elsewhere it goes on recording none (store.RecordPersonSignIn).
--
-- These triggers make that hold for every write, whatever code, language or migration sends it:
-- an update that would change a recorded account, null to an account included, is refused with
-- SQLSTATE 23514 (check_violation), and the statement and its transaction roll back. So is one
-- that would change when the credential was made (each row's created_at, and the
-- credential_created_at a token's trade recorded in its stored request), which a resource server
-- holds a token recording no account to. Each fires only on an update that names one of its
-- columns in its SET list (`before update of`), so a write that names none (a sign-in's last use
-- or revoke, a machine login's new key or expiry, a token's revoke or grace) costs nothing; one
-- that names a column and leaves it as it was passes, since the trigger's condition compares the
-- old row with the new.
--
-- refuse_recorded_account_change runs under a search_path of its own, pg_catalog, the schema the
-- broker's tables are in, pg_temp (the do block at the end), as 0045's guards do and for the
-- reason 0045 gives: it raises alone, and still resolves its own call under the caller's path
-- without one.
create function refuse_recorded_account_change() returns trigger language plpgsql as $$
begin
  raise exception 'the Google account % records is written once, when it is made, and is never changed', tg_table_name
    using errcode = 'check_violation';
end
$$;

create trigger person_signins_account_unchanged before update of issuer, subject, created_at on person_signins for each row
  when (old.issuer is distinct from new.issuer or old.subject is distinct from new.subject
    or old.created_at is distinct from new.created_at)
  execute function refuse_recorded_account_change();

create trigger launcher_credentials_approver_unchanged before update of approver_issuer, approver_subject, created_at on launcher_credentials for each row
  when (old.approver_issuer is distinct from new.approver_issuer or old.approver_subject is distinct from new.approver_subject
    or old.created_at is distinct from new.created_at)
  execute function refuse_recorded_account_change();

create trigger browser_sessions_account_unchanged before update of issuer, subject, created_at on browser_sessions for each row
  when (old.issuer is distinct from new.issuer or old.subject is distinct from new.subject
    or old.created_at is distinct from new.created_at)
  execute function refuse_recorded_account_change();

create trigger oauth_access_tokens_account_unchanged before update of request on oauth_access_tokens for each row
  when ((old.request #>> '{session,extra,account_issuer}') is distinct from (new.request #>> '{session,extra,account_issuer}')
    or (old.request #>> '{session,extra,account_subject}') is distinct from (new.request #>> '{session,extra,account_subject}')
    or (old.request #>> '{session,extra,credential_created_at}') is distinct from (new.request #>> '{session,extra,credential_created_at}'))
  execute function refuse_recorded_account_change();

create trigger oauth_refresh_tokens_account_unchanged before update of request on oauth_refresh_tokens for each row
  when ((old.request #>> '{session,extra,account_issuer}') is distinct from (new.request #>> '{session,extra,account_issuer}')
    or (old.request #>> '{session,extra,account_subject}') is distinct from (new.request #>> '{session,extra,account_subject}')
    or (old.request #>> '{session,extra,credential_created_at}') is distinct from (new.request #>> '{session,extra,credential_created_at}'))
  execute function refuse_recorded_account_change();

do $$
begin
  execute format('alter function %I.refuse_recorded_account_change() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
end
$$;
