-- packages/envoy/internal/broker/store/migrations/0033_credential_columns_write_once.up.sql
-- Every column that decides whether a broker credential is live, for which person and machine it
-- acts, and which key may present it moves only as the broker's own writers move it.
--
-- Dispatch admits a broker token from the broker's introspection answer, which the broker reads
-- from oauth_access_tokens and oauth_refresh_tokens, and a trade or a machine login copies
-- person_signins, launcher_credentials and oauth_device_codes into the token it mints. Each column
-- below has a short list of writers, and each writer moves it one way. A statement that moved one
-- another way, which no code of the broker sends, could make a revoked or spent token act again,
-- move a credential to another person, machine or key, or clear a revoke. So the database holds
-- each column to the transitions its writers make, whatever statement is sent, and refuses any
-- other with SQLSTATE 23514 (check_violation):
--
--   oauth_access_tokens, oauth_refresh_tokens
--     signature, request, request_id, credential_id  written once, at the insert
--       (oauthstore.Store.CreateAccessTokenSession and CreateRefreshTokenSession).
--     active  true -> false (RevokeAccessToken, RevokeRefreshToken, a rotation's retry inside its
--       grace, EndCredential, store.RevokePersonSignInTx); never back to true.
--     grace_until  null -> a time, once (RotateRefreshToken's coalesce); never cleared or moved.
--     A row may be deleted (the sweep, a retire's DeleteCredentialTokens, the library's reuse
--     path): a deleted token is refused.
--   person_signins
--     person, thumbprint, machine_account, created_at  written once, at the insert
--       (store.RecordPersonSignIn, whose conflict update moves last_used_at alone).
--     revoked_at  null -> the revoke's time, once (store.RevokePersonSignInTx); never cleared or
--       moved.
--     A revoked row is never deleted, and the table is never truncated: the revoked row is what
--     refuses its box key for good (0025). A live row may be deleted, which ends the sign-in (no
--     writer here deletes one; cybermonklabs/legion#4199's RecordPersonSignIn replaces a live row
--     that records no account).
--   launcher_credentials
--     subject  written once, at the insert; the token-login upsert never writes it.
--     revoked_at  null -> the revoke's time, once (enroll.endCredential, whose every caller holds
--       the row and has found it live); never cleared or moved.
--     expires_at  moved only while the login is live (machine.Service.Refreshed, the retire, the
--       token-login upsert); fixed once it is revoked.
--     operator, service, host, key_thumbprint, public_jwk, pod_uid  fixed on a login that names
--       no subject (a device approval's, enroll.mintLauncherCredential) and on any login once it
--       is revoked. A live login that names a subject may move them, as the token-login upsert
--       and the pod claim do (enroll.Service.IssueTokenLoginTx): a service account's or a pod's
--       login moves to the login's new key and its pod is written once it claims the row; a
--       machine's own login and a person's sign-in credential name the key in their subject
--       (exchange.MachineSubject, exchange.PersonSubject), so their key never moves, but the
--       upsert still moves their operator, host and expiry.
--   oauth_device_codes
--     request  replaced while the code is new (status 0) by its decision
--       (oauthstore.Store.UpdateDeviceCodeSession). Once it is decided, it changes in nothing
--       but its top-level last_checked (a poll of the decided code).
--     status  new (0) -> a decision, approved (1) or denied (2), by that same writer; any
--       status -> invalidated (-1), by InvalidateDeviceCodeSession when the code is exchanged.
--       Never back to new, never from one decision to the other, and never out of invalidated:
--       oauthstore.Store.GetDeviceCodeSession answers "not found" only for an invalidated code,
--       so a code moved out of it would be exchanged again.
--
-- Each before-update trigger fires only when one of its columns moves as no writer moves it (its
-- `when`), so a writer's statement costs nothing, and its function refuses every row it is
-- handed, naming the column: the `when` is what admits the rest. The functions' names are the
-- broker's own, since Dispatch's migrations may share this schema.
--
-- The guard is against a mistaken statement sent with the serving role's normal grants. A role
-- that disables a trigger, drops it or replaces a function changes the schema itself, which this
-- cannot hold. Every statement here is written into the catalogue alone (create function in
-- plpgsql, create trigger) and reads no stored row; the triggers hold every row written from now
-- on. A test that builds a state no writer makes turns the guard off for its own transaction
-- (storetest.Unguarded).
create function broker_refuse_access_token_change() returns trigger language plpgsql as $$
begin
  if not old.active and new.active then
    raise exception 'oauth_access_tokens.active goes false when the token is revoked or spent, and never back to true'
      using errcode = 'check_violation';
  end if;
  raise exception 'oauth_access_tokens.% is written once, when the token is stored, and is never changed',
    case
      when new.signature is distinct from old.signature then 'signature'
      when new.request_id is distinct from old.request_id then 'request_id'
      when new.credential_id is distinct from old.credential_id then 'credential_id'
      else 'request'
    end
    using errcode = 'check_violation';
end
$$;

create trigger oauth_access_tokens_credential_columns before update on oauth_access_tokens
  for each row when (
    old.signature is distinct from new.signature
    or old.request is distinct from new.request
    or old.request_id is distinct from new.request_id
    or old.credential_id is distinct from new.credential_id
    or (not old.active and new.active)
  )
  execute function broker_refuse_access_token_change();

create function broker_refuse_refresh_token_change() returns trigger language plpgsql as $$
begin
  if not old.active and new.active then
    raise exception 'oauth_refresh_tokens.active goes false when the token is revoked or spent, and never back to true'
      using errcode = 'check_violation';
  end if;
  if old.grace_until is not null and new.grace_until is distinct from old.grace_until then
    raise exception 'oauth_refresh_tokens.grace_until is when a rotated token stops being redeemable, set at its first rotation, and is never cleared or moved'
      using errcode = 'check_violation';
  end if;
  raise exception 'oauth_refresh_tokens.% is written once, when the token is stored, and is never changed',
    case
      when new.signature is distinct from old.signature then 'signature'
      when new.request_id is distinct from old.request_id then 'request_id'
      when new.credential_id is distinct from old.credential_id then 'credential_id'
      else 'request'
    end
    using errcode = 'check_violation';
end
$$;

create trigger oauth_refresh_tokens_credential_columns before update on oauth_refresh_tokens
  for each row when (
    old.signature is distinct from new.signature
    or old.request is distinct from new.request
    or old.request_id is distinct from new.request_id
    or old.credential_id is distinct from new.credential_id
    or (not old.active and new.active)
    or (old.grace_until is not null and old.grace_until is distinct from new.grace_until)
  )
  execute function broker_refuse_refresh_token_change();

create function broker_refuse_person_signin_change() returns trigger language plpgsql as $$
begin
  if tg_op = 'TRUNCATE' then
    raise exception 'person_signins holds every revoked sign-in, which refuses its box key for good, and is never truncated'
      using errcode = 'check_violation';
  end if;
  if tg_op = 'DELETE' then
    raise exception 'person_signins row % is a revoked sign-in, which refuses its box key for good, and is never deleted', old.id
      using errcode = 'check_violation';
  end if;
  if old.revoked_at is not null and new.revoked_at is distinct from old.revoked_at then
    raise exception 'person_signins.revoked_at is when the sign-in was revoked, and a revoke is never undone or moved'
      using errcode = 'check_violation';
  end if;
  raise exception 'person_signins.% is written once, when the sign-in is recorded, and is never changed',
    case
      when new.person is distinct from old.person then 'person'
      when new.thumbprint is distinct from old.thumbprint then 'thumbprint'
      when new.machine_account is distinct from old.machine_account then 'machine_account'
      else 'created_at'
    end
    using errcode = 'check_violation';
end
$$;

create trigger person_signins_credential_columns before update on person_signins
  for each row when (
    old.person is distinct from new.person
    or old.thumbprint is distinct from new.thumbprint
    or old.machine_account is distinct from new.machine_account
    or old.created_at is distinct from new.created_at
    or (old.revoked_at is not null and old.revoked_at is distinct from new.revoked_at)
  )
  execute function broker_refuse_person_signin_change();

create trigger person_signins_revoked_kept before delete on person_signins
  for each row when (old.revoked_at is not null)
  execute function broker_refuse_person_signin_change();

create trigger person_signins_not_truncated before truncate on person_signins
  for each statement execute function broker_refuse_person_signin_change();

create function broker_refuse_launcher_credential_change() returns trigger language plpgsql as $$
declare
  moved text := case
    when new.operator is distinct from old.operator then 'operator'
    when new.service is distinct from old.service then 'service'
    when new.host is distinct from old.host then 'host'
    when new.key_thumbprint is distinct from old.key_thumbprint then 'key_thumbprint'
    when new.public_jwk is distinct from old.public_jwk then 'public_jwk'
    else 'pod_uid'
  end;
begin
  if new.subject is distinct from old.subject then
    raise exception 'launcher_credentials.subject names the account, pod, machine or sign-in a login acts for, is written once, when the login is minted, and is never changed'
      using errcode = 'check_violation';
  end if;
  if old.revoked_at is not null and new.revoked_at is distinct from old.revoked_at then
    raise exception 'launcher_credentials.revoked_at is when the login was revoked, and a revoke is never undone or moved'
      using errcode = 'check_violation';
  end if;
  if old.revoked_at is not null and new.expires_at is distinct from old.expires_at then
    raise exception 'launcher_credentials.expires_at of a revoked login is fixed: a revoked login is never made to last longer or shorter'
      using errcode = 'check_violation';
  end if;
  if old.revoked_at is not null then
    raise exception 'launcher_credentials.% of a revoked login is fixed: the login a revoke ended never moves to another key, operator, service, host or pod', moved
      using errcode = 'check_violation';
  end if;
  raise exception 'launcher_credentials.% of a login that names no subject is written once, when its approval mints it, and is never changed', moved
    using errcode = 'check_violation';
end
$$;

create trigger launcher_credentials_credential_columns before update on launcher_credentials
  for each row when (
    old.subject is distinct from new.subject
    or (old.revoked_at is not null
      and (old.revoked_at is distinct from new.revoked_at or old.expires_at is distinct from new.expires_at))
    or ((old.subject is null or old.revoked_at is not null)
      and (old.operator is distinct from new.operator
        or old.service is distinct from new.service
        or old.host is distinct from new.host
        or old.key_thumbprint is distinct from new.key_thumbprint
        or old.public_jwk is distinct from new.public_jwk
        or old.pod_uid is distinct from new.pod_uid))
  )
  execute function broker_refuse_launcher_credential_change();

-- A decided code's request is compared without its top-level last_checked, the one key a poll of
-- it writes. A request that is not a JSON object, which no writer stores, is compared whole, so
-- the comparison itself never raises an error of its own in place of the refusal. The statuses
-- are the library's (authelia.com/provider/oauth2: new 0, approved 1, denied 2) and the store's
-- own invalidated (-1, oauthstore's deviceInvalidated).
create function broker_refuse_device_code_change() returns trigger language plpgsql as $$
begin
  if new.status <> old.status and new.status <> -1 and not (old.status = 0 and new.status in (1, 2)) then
    raise exception 'oauth_device_codes.status moves only from new (0) to a decision (1 or 2) and from any status to invalidated (-1): a code is decided once, and an invalidated code is never exchanged again'
      using errcode = 'check_violation';
  end if;
  raise exception 'oauth_device_codes.request of a decided code holds its decision, and changes in nothing but its last_checked (a poll)'
    using errcode = 'check_violation';
end
$$;

create trigger oauth_device_codes_decision_kept before update on oauth_device_codes
  for each row when (
    (new.status <> old.status and new.status <> -1 and not (old.status = 0 and new.status in (1, 2)))
    or (old.status <> 0 and case
      when jsonb_typeof(old.request) = 'object' and jsonb_typeof(new.request) = 'object'
        then (old.request - 'last_checked') is distinct from (new.request - 'last_checked')
      else old.request is distinct from new.request
    end)
  )
  execute function broker_refuse_device_code_change();
