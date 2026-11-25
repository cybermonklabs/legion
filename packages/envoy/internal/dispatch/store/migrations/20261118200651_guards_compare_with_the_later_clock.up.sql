-- 20261118200651_guards_compare_with_the_later_clock.up.sql — the two guards that hold a stamp to
-- the database's clock compare it with the later of the transaction's start and the clock.
--
-- people.confirmed_at (guard_people_binding_columns, 20261118041916) and broker_revocations.tried_at
-- (guard_broker_revocation_columns, 20261118152422) are each refused when later than
-- clock_timestamp(). Their writers stamp them with now(), the transaction's start: the hourly
-- confirmation and a sign-in write confirmed_at = now(), and a pass's claim writes tried_at = now().
-- now() is never later than clock_timestamp() while the clock moves forward, but after the
-- database's clock steps backwards, a transaction that began before the step holds a now() later
-- than clock_timestamp() for as long as the step, and its own stamp was refused with SQLSTATE 23514:
-- a sign-in, a confirmation or a claim spanning the step failed. So each now compares with
-- greatest(now(), clock_timestamp()): a writer's own now() always passes, and a stamp later than
-- both, which no writer makes, is refused as before.
--
-- 20261118041916 and 20261118152422 are not edited, since shared and lane databases ran them, and
-- the migrator records a version with no checksum. This replaces each function whole
-- (create or replace, so the triggers that call them stand), with that one comparison changed and
-- every other rule as it was. It is written into the catalogue alone and reads, refuses and
-- rewrites no row (TestCensusExemptsTheLaterClockMigrationAtAnyTableSize); the guards run on later
-- writes only (TestTheGuardsAcceptAWritersOwnStampAfterTheClockStepsBack).
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
    end if;
  end if;
  if new.confirmed_at > greatest(now(), clock_timestamp()) and (tg_op = 'INSERT' or new.confirmed_at is distinct from old.confirmed_at) then
    raise exception 'people.confirmed_at is when the hourly confirmation last confirmed the person, and it is never later than now'
      using errcode = 'check_violation';
  end if;
  return new;
end
$$;

create or replace function guard_broker_revocation_columns() returns trigger language plpgsql as $$
declare
  holder text;
  current_generation bigint;
begin
  if tg_op in ('DELETE', 'TRUNCATE') then
    raise exception 'broker_revocations holds what Dispatch owes the secrets broker, and its rows are never removed: a revocation removed is one the broker is never told'
      using errcode = 'check_violation';
  end if;
  if new.tried_at > greatest(now(), clock_timestamp()) then
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
