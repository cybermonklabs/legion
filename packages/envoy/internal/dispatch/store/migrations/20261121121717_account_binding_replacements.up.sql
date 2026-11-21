-- 20261121121717_account_binding_replacements.up.sql
-- The changes of an address's Google account made while the account binding was off, kept until
-- the binding holds and catches them up.
--
-- While a build without the account binding is in the fleet (20261120222130_account_binding_fleet),
-- a sign-in that records an address's account revokes and owes nothing, whichever build serves it.
-- Once the binding holds, a credential of a replaced account that records it is refused, and so is
-- one that records no account and was made before the recording (people.sub_recorded_at); but one
-- that records none and was made after it would act as the holder's, though a credential from
-- before the recording could have minted it while the binding was off (through either build: an
-- earlier build never records the account a mint is bound to), and a replaced account's broker
-- credentials would never be revoked at the broker. With the binding on, the sign-in would have
-- revoked every such credential and owed the broker. So the database keeps each change made while
-- the binding is off, as it keeps sub_recorded_at, whichever build changes people.sub:
--
--   - account_binding_replacements, one row per change: the address (people.email), the account
--     it replaced (null for the address's first recording, which replaced none), and when (the
--     change's people.sub_recorded_at, which the before trigger people_sub_recorded_at stamps from
--     the database's clock, the time a token's created_at is compared with).
--   - people_account_binding_replacement, an AFTER INSERT OR UPDATE OF sub trigger on people, for
--     a row that records an account it did not record before (sub is never taken off:
--     people_sub_recorded_at refuses that). It keeps the row only while a build without the
--     binding is in the fleet (account_binding_fleet_read, over the windows store.AccountBindingHolds
--     passes: store.OwnerLockLegacyWindow, 600 seconds, and store.OwnerLockMarkWindow, 3600,
--     spelled here as TestTheReplacementTriggerReadsTheFleetOverTheBindingsWindows holds them):
--     with the binding on, the sign-in revokes and owes itself. It notifies
--     dispatch_broker_revocations, so the revocation loop (api.RunBrokerRevocations) wakes and
--     watches for the binding to start.
--
-- The binding reads the rows in two ways, both only while it holds (store.AccountBindingHolds):
--
--   - At once, in every check of a personal token (api.tokenAccountReplaced): a token recording no
--     account made at or after a kept change of its address is refused, as one made before the
--     address's recording is.
--   - In a catch-up under the address's lock (store.CatchUpAccountBinding), which does what the
--     sign-in would have done with the binding on: every personal token of the address that records
--     no account and was made at or after its earliest kept change is revoked, the holder's own
--     included (they mint again); for a kept first recording, every token recording another
--     account or none is revoked too, as a first recording revokes them; a broker revocation is
--     recorded for each account replaced, or recorded by a revoked token, that does not hold the
--     address now; and the address's rows are deleted, so a second catch-up finds nothing. A
--     sign-in of a build with the binding runs it before its own writes, and the revocation loop
--     runs it for every address with a row.
--
-- A sign-in that finds the binding holding revokes and owes its own change in its own transaction,
-- and deletes any row kept for it there (store.PgPeopleStore.SignInSubject).
--
-- The function pins search_path = pg_catalog, public, pg_temp and names every table and function
-- with its schema, as the binding's guards do (20261119065002).
--
-- Rolled back to a release without the binding, the table and the trigger stay: that release's
-- changes are kept here, and nothing of it reads them; a later release with the binding catches
-- them up when its binding holds.
--
-- Locks: CREATE TRIGGER takes people SHARE ROW EXCLUSIVE until the commit, so a sign-in waits for
-- an instant; the table is new.
create table public.account_binding_replacements (
  email text not null,
  replaced_subject text,
  replaced_at timestamptz not null,
  primary key (email, replaced_at)
);

create function public.keep_account_binding_replacement() returns trigger language plpgsql
set search_path = pg_catalog, public, pg_temp as $$
declare
  replaced text;
begin
  if tg_op = 'INSERT' then
    if new.sub is null then
      return null;
    end if;
  elsif new.sub is not distinct from old.sub then
    return null;
  else
    replaced := old.sub;
  end if;
  if not public.account_binding_fleet_read(600, 3600) then
    return null;
  end if;
  insert into public.account_binding_replacements (email, replaced_subject, replaced_at)
  values (new.email, replaced, new.sub_recorded_at)
  on conflict do nothing;
  perform pg_catalog.pg_notify('dispatch_broker_revocations', '');
  return null;
end $$;

create trigger people_account_binding_replacement after insert or update of sub on public.people
  for each row execute function public.keep_account_binding_replacement();
