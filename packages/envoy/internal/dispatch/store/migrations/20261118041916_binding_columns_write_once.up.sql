-- 20261118041916_binding_columns_write_once.up.sql — every column the account binding reads moves
-- only as its writers move it.
--
-- Which credential acts for a person, and which account a mint or an approve names, is decided by
-- predicates that read three tables: people (the address, its recorded Google account and when the
-- database recorded it, the hourly confirmation), user_sessions (the generation a session cookie
-- must carry) and agent_tokens (a personal token's address, its recorded account, its creation time
-- and its revocation). Each column has a short list of writers (store.PgPeopleStore,
-- store.PgSessionStore, api.createAgentToken and the token revokes), and each writer moves it one
-- way. A statement that moved one another way, which no code of Dispatch sends, could make a
-- credential act for an address that is no longer its account's: a session's generation set back
-- or its row deleted, so an old cookie matches again; a token's revocation cleared, or its owner
-- changed; the address renamed, or its account set to the empty string, which one reader took for
-- no account and another for an account. So the database holds each column to the transitions its
-- writers make, whatever statement is sent, and refuses any other with SQLSTATE 23514
-- (check_violation), or 23503 (foreign_key_violation) for a people row a session still names.
--
--   people.email            written once, at the insert; never changed.
--   people.sub              null -> an account (the first recording), an account -> another
--                           account (a sign-in under another account replaces it); never back to
--                           null (20261117175133's rule, kept here), never the empty string
--                           (people_sub_not_empty).
--   people.sub_recorded_at  the database's own clock, stamped whenever sub changes
--                           (20261117175133's rule, kept here); no statement moves it.
--   people.confirmed_at     null, or a time no later than the database's clock when it is written
--                           (a writer's now() or clock_timestamp()): never later, which would make
--                           the hourly confirmation (identity.Membership.Confirm) skip a person.
--   people row              kept while it records an account (20261117175133) or while a session
--                           names it (user_sessions_login_fkey).
--   user_sessions.login     written once, at the insert, for an address people holds; never
--                           changed, and the row is never deleted.
--   user_sessions.generation  never lower than it was.
--   agent_tokens.id, .token_hash, .owner, .subject, .created_at  written once, at the mint.
--   agent_tokens.revoked_at null -> the revoke's time, once; never cleared or moved.
--
-- And the one change of the account the rule allows, an account recorded where none was or another
-- account replacing it, ends every session of the address in the same statement
-- (people_account_ends_sessions, people_first_account_ends_sessions): a cookie carries an address
-- and a generation and no account, so the account's change has to move the generation itself.
-- SignInSubject used to do that in its own statements, only for a row that existed before it; the
-- database now does it for every statement that records an account, a fresh row included.
--
-- The columns the predicates read and this leaves as they are, each with its writers: people's
-- member_kind (full <-> restricted, by three writers; its check holds the two values), refresh_token
-- (a sealed token or null, paired with confirmed_at by 0068's check) and signed_in_at (read by no
-- predicate); agent_tokens' scopes and projects (the token's authority as its owner, changed by its
-- owner through PATCH) and last_used_at (read by none); and agent_tokens rows, which nothing deletes
-- and whose loss refuses the token. The guard is against a mistaken statement sent with the serving
-- role's normal grants. A role that disables a trigger, drops a constraint or replaces a function
-- (canonical_login among them) changes the schema itself, which this cannot hold.
--
-- Every statement is written into the catalogue alone: the check and the
-- foreign key are added `not valid` (pgmigrate's catalog-only forms), so neither reads a stored row,
-- and both hold every row written from now on. The census refuses a deploy while any people row
-- holds an empty sub, the one stored value the check would then refuse at its next write; no writer
-- of Dispatch has ever stored one. A user_sessions row from before people existed may name an
-- address no people row holds: the key is not validated, so such a row stays, and what the key
-- protects is the other side, a people row a session names, which no stored row escapes.
--
-- The release before this one writes nothing these refuse (its sign-ins record a people row before
-- they make a session, write sub only from Google's own subject, and revoke a token by stamping
-- revoked_at once), so it runs against this schema unchanged; its SignInSubject still ends the
-- sessions itself, which moves a replaced address's generation by two rather than one.
alter table people add constraint people_sub_not_empty check (sub <> '') not valid;

alter table user_sessions
  add constraint user_sessions_login_fkey foreign key (login) references people (email) not valid;

create function guard_people_binding_columns() returns trigger language plpgsql as $$
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
    end if;
  end if;
  if new.confirmed_at > clock_timestamp() and (tg_op = 'INSERT' or new.confirmed_at is distinct from old.confirmed_at) then
    raise exception 'people.confirmed_at is when the hourly confirmation last confirmed the person, and it is never later than now'
      using errcode = 'check_violation';
  end if;
  return new;
end
$$;

drop trigger people_sub_recorded_at on people;
drop function stamp_people_sub_recorded_at();

create trigger people_binding_columns before insert or update on people
  for each row execute function guard_people_binding_columns();

create function end_people_sessions_on_new_account() returns trigger language plpgsql as $$
begin
  insert into user_sessions (login, generation) values (new.email, 1)
    on conflict (login) do update set generation = user_sessions.generation + 1;
  return null;
end
$$;

create trigger people_account_ends_sessions after update on people
  for each row when (new.sub is not null and new.sub is distinct from old.sub)
  execute function end_people_sessions_on_new_account();

create trigger people_first_account_ends_sessions after insert on people
  for each row when (new.sub is not null)
  execute function end_people_sessions_on_new_account();

create function guard_user_sessions_binding_columns() returns trigger language plpgsql as $$
begin
  if tg_op in ('DELETE', 'TRUNCATE') then
    raise exception 'user_sessions holds the generation every session cookie of an address must carry, and its rows are never removed: a row made again would start at 0, where old cookies match'
      using errcode = 'check_violation';
  end if;
  if new.login is distinct from old.login then
    raise exception 'user_sessions.login names the address a generation belongs to, and it never changes'
      using errcode = 'check_violation';
  end if;
  if new.generation < old.generation then
    raise exception 'user_sessions.generation ends every older session cookie of its address when it moves, so it never moves back'
      using errcode = 'check_violation';
  end if;
  return new;
end
$$;

create trigger user_sessions_binding_columns before update or delete on user_sessions
  for each row execute function guard_user_sessions_binding_columns();

create trigger user_sessions_not_truncated before truncate on user_sessions
  for each statement execute function guard_user_sessions_binding_columns();

-- One trigger holds every agent_tokens column the binding reads, in place of 20261117160116's,
-- which held subject and created_at alone. It fires only when one of them changes (its `when`),
-- so a write that changes none (a last use, a change of scopes, a token's first revoke) costs
-- nothing, and its function refuses every row it is handed: the `when` is what admits the rest.
-- The function's name is Dispatch's own: the broker's migrations may share this schema
-- (TestTheBrokerAndDispatchMigrationsCreateNoNameInCommon).
create function refuse_agent_token_binding_column_change() returns trigger language plpgsql as $$
begin
  if old.revoked_at is not null and new.revoked_at is distinct from old.revoked_at then
    raise exception 'agent_tokens.revoked_at is when the token was revoked, and a revoke is never undone or moved'
      using errcode = 'check_violation';
  end if;
  raise exception 'agent_tokens.% is written once, when the token is minted, and is never changed',
    case
      when new.id is distinct from old.id then 'id'
      when new.token_hash is distinct from old.token_hash then 'token_hash'
      when new.owner is distinct from old.owner then 'owner'
      when new.subject is distinct from old.subject then 'subject'
      else 'created_at'
    end
    using errcode = 'check_violation';
end
$$;

drop trigger agent_tokens_subject_unchanged on agent_tokens;
drop function refuse_agent_token_account_change();

create trigger agent_tokens_binding_columns before update on agent_tokens
  for each row when (
    old.id is distinct from new.id
    or old.token_hash is distinct from new.token_hash
    or old.owner is distinct from new.owner
    or old.subject is distinct from new.subject
    or old.created_at is distinct from new.created_at
    or (old.revoked_at is not null and old.revoked_at is distinct from new.revoked_at)
  )
  execute function refuse_agent_token_binding_column_change();
