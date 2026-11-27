-- 20261119065002_binding_guards_search_path.up.sql — every trigger function of the account
-- binding resolves its names under a search_path of its own, and the claim guard lets a stamp its
-- write does not move through.
--
-- 20261119021826 pinned the two clock guards. The binding's other trigger functions were left
-- under the caller's path, and each resolves something at run time:
--
--   end_people_sessions_on_new_account (20261118041916) names user_sessions. A session holding a
--     temporary table of that name takes the generation's move, so a write that records another
--     account leaves the real row where it was and every cookie of the replaced account goes on
--     matching — the one write the binding's safety rests on (store.PgPeopleStore.SignInSubject).
--   guard_user_sessions_binding_columns (20261118041916) compares with `<`. An operator of that
--     name in a schema the session lists before pg_catalog answers what it likes, and a generation
--     moves back, where an ended cookie matches again.
--   refuse_agent_token_binding_column_change (20261118041916) and keep_people_recorded_account
--     (20261117175133) raise alone, and broker_revocations_notify (20261116232217) calls
--     pg_notify; each still resolves a function name under the caller's path.
--
-- A temporary table needs only the TEMP right, which PUBLIC holds, and a schema with an operator
-- of one's own needs only CREATE on the database, so neither is the privilege of a writer. Every
-- one of them is pinned here to pg_catalog, the schema the tables are in, then pg_temp, built from
-- current_schema() in a DO block for the reason 20261119021826 gives (a fixed public breaks a
-- deployment whose tables are elsewhere), and TestEveryBindingTriggerRunsUnderItsOwnSearchPath
-- reads every trigger of the binding's tables, so a guard added later fails that test until it is
-- pinned too.
--
-- guard_broker_revocation_columns is replaced first, whole, as 20261118200651 wrote it with one
-- comparison changed: its tried_at arm refused any row whose tried_at is later than the database's
-- clock, a row the write does not touch included. After the clock steps back, a claim's stamp is
-- later than the clock for the length of the step, and every later write of that row — a
-- settlement, the operator's clear, the settle a sign-in that takes an address back makes, and the
-- recording the same sign-in's last statement makes — was refused with SQLSTATE 23514, which rolls
-- the sign-in back: the account Google gives the address could not sign in for the length of the
-- step, and the account it replaces kept its sessions and tokens for that time. So the arm now
-- refuses only a stamp the write itself sets or moves, as the confirmed_at arm already did
-- (20261118200651's own `tg_op = 'INSERT' or ... is distinct from`), and a stamp later than both
-- clocks, which no writer makes, is still refused. A create or replace drops a function's
-- settings, so the pin below runs after it.
--
-- It replaces one function and alters six functions' settings, writes into the catalogue alone and
-- reads, refuses and rewrites no row; the census exempts no DO block, since it cannot read what
-- one runs, but this one names no table, so nothing is measured or refused.
create or replace function guard_broker_revocation_columns() returns trigger language plpgsql as $$
declare
  holder text;
  current_generation bigint;
begin
  if tg_op in ('DELETE', 'TRUNCATE') then
    raise exception 'broker_revocations holds what Dispatch owes the secrets broker, and its rows are never removed: a revocation removed is one the broker is never told'
      using errcode = 'check_violation';
  end if;
  if new.tried_at > greatest(now(), clock_timestamp()) and (tg_op = 'INSERT' or new.tried_at is distinct from old.tried_at) then
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

do $$
begin
  execute format('alter function %I.guard_broker_revocation_columns() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
  execute format('alter function %I.end_people_sessions_on_new_account() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
  execute format('alter function %I.guard_user_sessions_binding_columns() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
  execute format('alter function %I.refuse_agent_token_binding_column_change() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
  execute format('alter function %I.keep_people_recorded_account() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
  execute format('alter function %I.broker_revocations_notify() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
end
$$;
