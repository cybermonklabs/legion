-- 20261117175133_people_sub_recorded_at.up.sql — when Dispatch last recorded the address's Google account.
--
-- A credential that records no Google account (a personal token minted by one, a broker token
-- whose sign-in or machine login was made before the broker recorded accounts) might be any
-- account's. Dispatch holds it to the moment the address's account was recorded: one made before
-- that moment is refused as the person (api.tokenAccountReplaced, api.brokerAccountReplaced, the
-- membership answer's holder), since an account that held the address before may have made it.
--
-- The column is null for every row before it, and null means there is no recording to compare
-- with: no credential is refused at the deploy, and nothing is reset. A nullable column with no
-- default is a catalog-only alter table, so it reads no row of people.
alter table people add column sub_recorded_at timestamptz;

-- The comparison is only as sound as this column, so the database writes it, not the code: an
-- insert that records an account, and an update that changes sub to another account
-- (store.PgPeopleStore.SignInSubject is its one writer), stamp it from clock_timestamp(), whatever
-- the statement said; an update that would change it otherwise, moving it back so an old
-- credential counts as newer, or to null so the comparison refuses nothing, is refused with
-- SQLSTATE 23514 (check_violation), and the statement and its transaction roll back. A write that
-- names neither column costs nothing.
--
-- Every predicate of the rule also needs people.sub itself: a row recording no account binds no
-- credential. So a recorded account never disappears: an update setting sub to null, a delete of
-- a row recording an account, and any truncate of people are refused with 23514 too. No writer of
-- Dispatch does any of them (store.PgPeopleStore writes sub only to an account, and nothing
-- deletes from people); the browser harness's reset deletes people rows and turns the delete
-- trigger off in its own transaction (packages/dispatch/e2e/seed.ts), and a test that stands for a
-- row recording no account uses storetest.TestOnlyClearAccount, which does the same.
create function stamp_people_sub_recorded_at() returns trigger language plpgsql as $$
begin
  if tg_op = 'INSERT' then
    new.sub_recorded_at := case when new.sub is not null then clock_timestamp() end;
  elsif old.sub is not null and new.sub is null then
    raise exception 'people.sub records the address''s Google account, which Dispatch holds its credentials to, and it is never taken off'
      using errcode = 'check_violation';
  elsif new.sub is distinct from old.sub then
    new.sub_recorded_at := clock_timestamp();
  elsif new.sub_recorded_at is distinct from old.sub_recorded_at then
    raise exception 'people.sub_recorded_at is when the database recorded the address''s Google account, and only a change of sub moves it'
      using errcode = 'check_violation';
  end if;
  return new;
end
$$;

create trigger people_sub_recorded_at before insert or update of sub, sub_recorded_at on people
  for each row execute function stamp_people_sub_recorded_at();

create function keep_people_recorded_account() returns trigger language plpgsql as $$
begin
  if tg_op = 'TRUNCATE' then
    raise exception 'people holds the Google account each address records, which Dispatch holds its credentials to, and it is never truncated'
      using errcode = 'check_violation';
  end if;
  if old.sub is not null then
    raise exception 'people.sub records the address''s Google account, which Dispatch holds its credentials to, and its row is never deleted'
      using errcode = 'check_violation';
  end if;
  return old;
end
$$;

create trigger people_recorded_account_kept before delete on people
  for each row execute function keep_people_recorded_account();

create trigger people_not_truncated before truncate on people
  for each statement execute function keep_people_recorded_account();
