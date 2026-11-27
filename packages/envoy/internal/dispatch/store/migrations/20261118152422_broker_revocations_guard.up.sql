-- 20261118152422_broker_revocations_guard.up.sql — broker_revocations moves only as its writers
-- move it, and records where each replacement happened.
--
-- broker_revocations (20261116232217) is what ends a replaced account's credentials at the
-- secrets broker, which serves secrets without asking Dispatch: the server's revocation loop
-- learns what it owes the broker from it alone, and the predicates read it to name a refusal. Its
-- writers are the recording (store.recordBrokerRevocationSQL, an insert or, for an account
-- replaced again, an upsert that adds one to generation, sets holder_subject, session_generation
-- and recorded_at and makes the row owed), a pass's claim (tried_at = now()), and the three
-- settlements (the broker's confirmation, the settlement of an account that takes the address
-- back, and clear-broker-revocation), each done_at = now() on an owed row. A statement that moved
-- a column another way, which no code of Dispatch sends, could remove, rewrite or hold back a
-- revocation the broker is owed, so the broker never ends the replaced account's credentials, or
-- ends another's. So the database holds each column to the transitions its writers make, whatever
-- statement is sent, and refuses any other with SQLSTATE 23514 (check_violation), as
-- 20261118041916 does for people, user_sessions and agent_tokens:
--
--   broker_revocations row  never deleted or truncated (a revocation removed is one the broker is
--                           never told).
--   broker_revocations.email, .issuer, .subject  written once, at the insert.
--   broker_revocations.generation  +1 only, when the same account is replaced again: the
--                           recording's upsert, which also sets holder_subject, session_generation
--                           and recorded_at and makes the row owed (done_at null).
--   broker_revocations.holder_subject  at an insert and at that upsert, the account people records
--                           for the address at that moment (the sign-in records the revocation
--                           after it writes the new account, under the people row's lock), and
--                           never otherwise: a revocation naming another holder would have the
--                           broker stop refusing that account.
--   broker_revocations.session_generation  at an insert and at that upsert, the address's
--                           user_sessions.generation at that moment, which the recording reads in
--                           its own statement (the sign-in records the revocation after its people
--                           write moved the generation), and never otherwise.
--   broker_revocations.recorded_at  at an insert and at that upsert, and never otherwise.
--   broker_revocations.done_at  null -> a time (a settlement); back to null only by that upsert.
--   broker_revocations.tried_at  never later than the database's clock (a later one would keep
--                           every pass from claiming the row).
--
-- session_generation is where the replacement happened, as the address's session generation counts
-- it. An approve of a machine login is checked at Dispatch (api.approverStanding, under the
-- address's advisory lock) and relayed to the secrets broker afterwards; a replacing sign-in can
-- commit between the two, and the server's loop can pay its revocation before the relayed approve
-- arrives, when an approve naming no account would mint a login recording none that the paid
-- revocation no longer ends. So both carry a point on one counter, user_sessions.generation, which
-- every recording of an account moves in its own transaction (people_account_ends_sessions,
-- people_first_account_ends_sessions) and which never moves back (user_sessions_binding_columns,
-- both 20261118041916's): the approve carries the generation its check read, the revocation the
-- generation its replacing sign-in left, which this column records and the server's loop sends
-- (session_generation on POST /v1/person-subjects/revoke), and the broker refuses an approve whose
-- generation is below a revocation's of its address.
--
-- A statement that makes one of the writers' own transitions without the rest of what that writer
-- does passes, since no trigger can tell it from the writer's. Two matter: an account written into
-- people.sub by itself skips the revocations the sign-in records, and a revocation settled by
-- itself is the operator's clear-broker-revocation, which tells the broker nothing. Both leave a
-- replaced account's credentials at the broker. After A held the address and B took it, a
-- statement writing A back records no revocation of B, so B's machine logins and sign-ins stay
-- live at the broker, which serves them secrets without asking Dispatch, and the broker goes on
-- refusing A; a settled revocation leaves its account's credentials there until it is recorded
-- again. The store README and the runbook give the recovery of each: recording the revocation the
-- statement skipped, or recording the settled one again.
--
-- Every statement is written into the catalogue alone: session_generation is a nullable column with
-- no default, and the function and its triggers run on later writes only, so the migration reads,
-- refuses and rewrites no row. A row recorded before it holds no session_generation, which the
-- guard keeps until the revocation is recorded again; the server's loop then sends none for it, and
-- the broker holds an approve against that replacement to a time window instead.
--
-- broker_revocations ships in the release that carries this migration, so the release before it
-- writes nothing this refuses. A server of an earlier head of cybermonklabs/legion#4199, which had
-- the table and no session_generation, records a revocation naming none, which this guard refuses;
-- a database such a head migrated takes this migration at its next boot. Another earlier head,
-- 7142c3108a, made this column, this function and these two triggers inside 20261118041916, so a
-- database it migrated records that version and holds all four already: the column is added only
-- where it is missing, the function is replaced, and each trigger is dropped where it exists before
-- it is made, so this applies over either shape and leaves the one below
-- (TestTheBrokerRevocationsGuardMigrationAppliesOverAnEarlierHeadsShape). The function's name is
-- Dispatch's own: the broker's migrations may share this schema
-- (TestTheBrokerAndDispatchMigrationsCreateNoNameInCommon).
alter table broker_revocations add column if not exists session_generation bigint;

create or replace function guard_broker_revocation_columns() returns trigger language plpgsql as $$
declare
  holder text;
  current_generation bigint;
begin
  if tg_op in ('DELETE', 'TRUNCATE') then
    raise exception 'broker_revocations holds what Dispatch owes the secrets broker, and its rows are never removed: a revocation removed is one the broker is never told'
      using errcode = 'check_violation';
  end if;
  if new.tried_at > clock_timestamp() then
    raise exception 'broker_revocations.tried_at is when a pass last claimed the revocation, and it is never later than the database''s clock: a later one would keep every pass from claiming it'
      using errcode = 'check_violation';
  end if;
  if tg_op = 'UPDATE' then
    if new.email is distinct from old.email or new.issuer is distinct from old.issuer or new.subject is distinct from old.subject then
      raise exception 'broker_revocations.email, issuer and subject name the address and the replaced account, and they are written once'
        using errcode = 'check_violation';
    end if;
    if new.generation is not distinct from old.generation then
      if new.holder_subject is distinct from old.holder_subject or new.recorded_at is distinct from old.recorded_at then
        raise exception 'broker_revocations.holder_subject and recorded_at change only when the revocation is recorded again, as its next generation'
          using errcode = 'check_violation';
      end if;
      if new.session_generation is distinct from old.session_generation then
        raise exception 'broker_revocations.session_generation is the address''s session generation when the revocation was recorded, and it changes only when the revocation is recorded again, as its next generation'
          using errcode = 'check_violation';
      end if;
      if old.done_at is not null and new.done_at is distinct from old.done_at then
        raise exception 'broker_revocations.done_at is when the revocation was settled, and a settlement is undone only by recording the revocation again'
          using errcode = 'check_violation';
      end if;
      return new;
    end if;
    if new.generation is distinct from old.generation + 1 or new.done_at is not null then
      raise exception 'broker_revocations.generation counts the revocations recorded for the account, and it moves by one, to a revocation owed'
        using errcode = 'check_violation';
    end if;
  end if;
  select sub into holder from people where email = new.email;
  if holder is distinct from new.holder_subject then
    raise exception 'broker_revocations.holder_subject is the account that took the address, which people records for it when the revocation is recorded'
      using errcode = 'check_violation';
  end if;
  select generation into current_generation from user_sessions where login = new.email;
  if current_generation is distinct from new.session_generation then
    raise exception 'broker_revocations.session_generation is the address''s session generation when the revocation is recorded, which user_sessions holds for it then'
      using errcode = 'check_violation';
  end if;
  return new;
end
$$;

drop trigger if exists broker_revocations_binding_columns on broker_revocations;
create trigger broker_revocations_binding_columns before insert or update or delete on broker_revocations
  for each row execute function guard_broker_revocation_columns();

drop trigger if exists broker_revocations_not_truncated on broker_revocations;
create trigger broker_revocations_not_truncated before truncate on broker_revocations
  for each statement execute function guard_broker_revocation_columns();
