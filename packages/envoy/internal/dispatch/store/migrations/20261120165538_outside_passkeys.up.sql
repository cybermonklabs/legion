-- 20261120165538_outside_passkeys.up.sql — a restricted member signs in with a passkey at the
-- outside origin, enrolled through a single-use link the project's owner makes.
--
--   - outside_invitations holds each link an owner made: the SHA-256 of its 32 random bytes (the
--     link itself carries them in its URL fragment, and no row holds them), the email and project
--     it is for, who made it and when, when it lapses (24 hours after), and when it was used. A
--     link is used once: the enrollment that uses it sets used_at in its own transaction. A new
--     link for the same person and project and a removal from that project's list delete the
--     person's unused links of that project, and the owner's Reset every unused link of the person
--     (store.PgPasskeys).
--   - passkeys holds each WebAuthn credential a person enrolled with such a link: its id, the
--     person's email, the user handle the authenticator stores with it (the person's subject), the
--     COSE public key, the signature counter, the AAGUID, the transports the browser reported, the
--     credential's flags as go-webauthn packs them into one byte (webauthn.CredentialFlags:
--     backup eligible is checked at every sign-in), the project of the link that enrolled it and
--     who made that link, and when it was enrolled and last used. A passkey reads every project
--     that names its person, so one owner binds an outside person: a list of another owner refuses
--     an address whose passkey a link of someone else enrolled (api.putProjectAccessPerson). The
--     owner's Reset, a removal from the last list that names the person and the hourly check of a
--     person no list names delete them, in the transaction that ends the sign-in.
--   - people.sub_issuer is the issuer of the account people.sub records: null for an account of
--     the deployment's sign-in issuer (DISPATCH_SIGNIN_ISSUER, every row before this one), and the
--     outside origin for a passkey's subject (16 random bytes, base64url, made at the person's
--     first enrollment). The secrets broker names an approver by the pair, so Dispatch relays it.
--     It is written with sub, under people's binding guard: a write that leaves sub as it was
--     leaves sub_issuer, and a row with no sub holds none (guard_people_binding_columns below).
--   - people_check paired a refresh token with a confirmation: a sign-in stands while the row holds
--     the issuer's refresh token, and its hourly confirmation refreshes it. A passkey's sign-in has
--     no refresh token; it stands while confirmed_at holds a time, and its hourly confirmation reads
--     the access lists and the passkeys instead. So the pairing holds for a row of the sign-in
--     issuer's account, and a passkey's row holds no refresh token at all.
--   - passkey_ceremonies holds each ceremony a begin started and no finish has used yet: the
--     SHA-256 of its challenge and when it lapses (5 minutes after the begin). A finish deletes its
--     ceremony's row before it verifies anything, and a finish whose row is gone is refused, so a
--     ceremony's answer is taken once: a replayed answer and its ceremony cookie are refused even
--     for a passkey whose signature counter stays 0, which the counter check cannot catch. Each
--     begin deletes the rows that lapsed, so the table holds the last 5 minutes of begins.
--
-- New tables, a nullable column with no default and its check added NOT VALID (as 20261117064356
-- adds agent_tokens.subject's: an inline check on `add column` would read every row of people), a
-- check dropped and one added NOT VALID, a function replaced and its setting pinned: none reads or
-- rewrites a row of people. The column is null in every row, and the new people check admits
-- every row the old one did (a row with no sub_issuer is held to the old rule), so neither needs a
-- validating scan; both hold every write from here on.
create table outside_invitations (
  token_sha256 bytea primary key check (octet_length(token_sha256) = 32),
  email text not null check (email <> '' and email = lower(email)),
  project_key text not null references projects (key) on delete cascade,
  made_by text not null check (made_by <> ''),
  made_at timestamptz not null default now(),
  expires_at timestamptz not null check (expires_at > made_at),
  used_at timestamptz
);

create index outside_invitations_project_email on outside_invitations (project_key, email);

create table passkeys (
  credential_id bytea primary key check (octet_length(credential_id) between 1 and 1023),
  -- No foreign key to people: the enrollment writes the person's row, recording their account, in
  -- the transaction that stores the passkey, and a row that records an account is never removed
  -- (people_recorded_account_kept refuses its delete, people_not_truncated its truncate); a second
  -- table referring to people would refuse the truncate before that guard says why.
  email text not null check (email <> ''),
  user_handle bytea not null check (octet_length(user_handle) between 1 and 64),
  public_key bytea not null check (octet_length(public_key) > 0),
  sign_count bigint not null default 0 check (sign_count between 0 and 4294967295),
  aaguid bytea not null check (octet_length(aaguid) in (0, 16)),
  transports text[] not null default '{}',
  flags smallint not null check (flags between 0 and 255),
  -- The link's project and maker, which the owner's access answer shows. No foreign key to
  -- projects: a project deleted later leaves the record of whose link enrolled the passkey.
  project_key text not null check (project_key <> ''),
  made_by text not null check (made_by <> ''),
  created_at timestamptz not null default now(),
  last_used_at timestamptz
);

create index passkeys_email on passkeys (email);

create table passkey_ceremonies (
  challenge_sha256 bytea primary key check (octet_length(challenge_sha256) = 32),
  expires_at timestamptz not null
);

create index passkey_ceremonies_expires_at on passkey_ceremonies (expires_at);

alter table people add column sub_issuer text;

alter table people add constraint people_sub_issuer_check check (sub_issuer <> '') not valid;

alter table people drop constraint people_check;

alter table people add constraint people_sign_in_stands check (
  case when sub_issuer is null then (refresh_token is null) = (confirmed_at is null)
  else refresh_token is null end
) not valid;

-- 20261118200651's function whole, with sub_issuer's two rules added: it is set only with sub, and
-- a write that leaves sub as it was leaves it as it was.
create or replace function guard_people_binding_columns() returns trigger language plpgsql as $$
begin
  if tg_op = 'INSERT' then
    new.sub_recorded_at := case when new.sub is not null then clock_timestamp() end;
  else
    if new.email is distinct from old.email then
      raise exception 'people.email is the address every credential of the person names, and it never changes'
        using errcode = 'check_violation';
    end if;
    if old.sub is not null and new.sub is null then
      raise exception 'people.sub records the address''s Google account, which Dispatch holds its credentials to, and it is never taken off'
        using errcode = 'check_violation';
    elsif new.sub is distinct from old.sub then
      new.sub_recorded_at := clock_timestamp();
    elsif new.sub_recorded_at is distinct from old.sub_recorded_at then
      raise exception 'people.sub_recorded_at is when the database recorded the address''s Google account, and only a change of sub moves it'
        using errcode = 'check_violation';
    elsif new.sub_issuer is distinct from old.sub_issuer then
      raise exception 'people.sub_issuer is the issuer of the account people.sub records, and only a change of sub moves it'
        using errcode = 'check_violation';
    end if;
  end if;
  if new.sub is null and new.sub_issuer is not null then
    raise exception 'people.sub_issuer is the issuer of the account people.sub records, and a row that records no account has none'
      using errcode = 'check_violation';
  end if;
  if new.confirmed_at > greatest(now(), clock_timestamp()) and (tg_op = 'INSERT' or new.confirmed_at is distinct from old.confirmed_at) then
    raise exception 'people.confirmed_at is when the hourly confirmation last confirmed the person, and it is never later than now'
      using errcode = 'check_violation';
  end if;
  return new;
end
$$;

-- A create or replace drops the function's settings, so the pin of 20261119021826 runs again.
do $$
begin
  execute format('alter function %I.guard_people_binding_columns() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
end
$$;
