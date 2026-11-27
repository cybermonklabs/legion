-- 20261121180429_account_binding_unmarked_changes.up.sql
-- The trigger people_account_binding_replacement keeps every change of an address's Google account
-- that a writer without the account binding makes, whatever the fleet reads.
--
-- 20261121121717 kept such a change only while it read a build without the binding in the fleet
-- (account_binding_fleet_read). A process of such a build that the fleet does not see, one silent
-- past the prune age whose last arrival is more than an hour old (the owner lock's model), still
-- serves, and a change its sign-in made then was revoked by nobody and kept for nobody. So the
-- trigger now tells the writers apart by a mark, not by the fleet:
--
--   - This build's sign-in (store.PgPeopleStore.SignInSubject), the one writer of people.sub that
--     knows the binding, sets dispatch.account_binding_writer to 'on' for its own transaction
--     (store.markBindingWriterSQL, set_config's is_local) before it writes sub. It keeps or revokes
--     its own change as before, and the trigger keeps nothing for it.
--   - Any other write that records an account, an insert with sub or an update that changes it (a
--     build without the binding's sign-in, or any other statement), is kept, a first recording
--     included, so the revocation loop catches it up as soon as the binding holds: at once when it
--     holds already.
--   - Every change that records an account, kept or not, is announced on
--     dispatch_broker_revocations, so the revocation loop wakes at its commit: a change no row keeps
--     can still leave a token the binding refuses, and what that token minted, for the loop's
--     catch-up (store.CatchUpAccountBinding), which no other wake might bring for a long time.
--   - A write that takes the account off records none (people_binding_columns refuses it outside a
--     test), and is neither kept nor announced.
--
-- Rolled back to a release without the binding, the function stays: that release's sign-ins carry
-- no mark, so each change they make is kept, which nothing of that release reads; a later release
-- with the binding catches them up once its binding holds.
--
-- The function pins search_path = pg_catalog, public, pg_temp and names every table and function
-- with its schema, as the binding's guards do (20261119065002).
--
-- Locks: CREATE OR REPLACE FUNCTION writes the catalog alone and takes no lock on people, so a
-- sign-in in flight finishes under the function it started with.
create or replace function public.keep_account_binding_replacement() returns trigger language plpgsql
set search_path = pg_catalog, public, pg_temp as $$
declare
  replaced text;
begin
  if tg_op = 'INSERT' then
    if new.sub is null then
      return null;
    end if;
  elsif new.sub is null or new.sub is not distinct from old.sub then
    return null;
  else
    replaced := old.sub;
  end if;
  perform pg_catalog.pg_notify('dispatch_broker_revocations', '');
  if pg_catalog.current_setting('dispatch.account_binding_writer', true) = 'on' then
    return null;
  end if;
  insert into public.account_binding_replacements (email, replaced_subject, replaced_at)
  values (new.email, replaced, new.sub_recorded_at)
  on conflict do nothing;
  return null;
end $$;
