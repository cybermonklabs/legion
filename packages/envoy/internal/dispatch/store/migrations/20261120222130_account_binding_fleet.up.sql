-- 20261120222130_account_binding_fleet.up.sql
-- The account binding starts only once no build without it is in the fleet, which this migration
-- lets a build see by the fence 20261115200014_owner_lock_legacy_gate built for the owner lock.
--
-- The binding holds every credential of an address to the Google account Dispatch records for it
-- (people.sub and sub_recorded_at): a personal token's and a broker token's admission, a mint's and
-- a decision's re-read, the membership answer (api.tokenAccountReplaced, api.brokerAccountReplaced,
-- api.accountHolder), the account a mint records (agent_tokens.subject) and a decision names, and
-- the sign-in's revocations when it replaces the account or records the first one
-- (store.PgPeopleStore.SignInSubject). A build without it checks no account, records none and
-- revokes nothing when another account signs in, so beside it a replaced account's credentials act
-- on that build whatever this one does, and what this one wrote of the binding meanwhile (an owed
-- revocation naming an account that build then lets take the address back, a token recording an
-- account) rests on replacements only one build recorded. So the binding is one rule over the
-- fleet: while a build without it is in the fleet, a build with it checks, records and replaces as
-- that build does (store.AccountBindingHolds answers false), and it starts once none is.
--
-- A build without the binding is seen as 20261115200014 sees a build before the owner lock, and
-- for the same reason: its boot writes before it registers (its repairs and the background passes
-- that start before its registration), so server_processes alone misses it for that time, while
-- its dashboard publish comes before all of that, and every build publishes one (the image ships
-- its web app). That fence sees only owner lock protocol 0; a build at owner lock protocol 1 that
-- lacks the binding is as much a build without it. So, of the same shape:
--
--   - server_processes.account_binding and dashboard_builds.account_binding, each a constant
--     default of 0 that rewrites no row: a build with the binding registers and publishes 1
--     (store.AccountBindingProtocol; processes.Register, dashboard.Publish), and every build
--     before it, which names its columns in every insert it makes on both tables, writes nothing
--     there and reads 0.
--   - account_binding_legacy_mark, one row: when a build without the binding last arrived. The
--     migration stamps it at the newest publish already in dashboard_builds, when that is within
--     the hour (store.OwnerLockMarkWindow, spelled again), since every row there reads 0 here and
--     a publish that came before the trigger made no arrival. A database no build has published
--     to, as every test's is, starts with no mark.
--   - account_binding_legacy_arrival on dashboard_builds and account_binding_legacy_registration
--     on server_processes, BEFORE INSERT triggers for a row at 0, which fire for an
--     `insert ... on conflict ... do update` whether or not the row exists, and so for every
--     return of a lapsed registration too: each takes the binding's gate, a transaction advisory
--     lock (0x4F574E47 "OWNG" and 1: the owner lock gate's namespace, its second key,
--     store.OwnerLockGateNamespace and store.AccountBindingGateKey), exclusively, and stamps the
--     mark. The arrival waits for every transaction of a build with the binding that read the
--     fleet as having none without it and holds the gate shared, and its own transaction holds
--     the gate until it commits.
--   - account_binding_fleet_read, the fleet read: a build without the binding is in the fleet
--     while a row at 0 has heartbeated within one window (store.OwnerLockLegacyWindow,
--     processes.PruneAge: until it is deregistered or pruned), or while the mark is younger than
--     the other (store.OwnerLockMarkWindow, an hour, which covers a boot from its publish to its
--     registration); the caller passes both, as it passes the owner lock's.
--   - account_binding_fleet, which reads the fleet, and when it finds none tries the gate shared,
--     without waiting: a try that fails, while an arrival holds the gate or waits for it, answers
--     that such a build is in the fleet, and one that succeeds reads the fleet again under a
--     snapshot of its own, so a mark committed before the try is read. A transaction that found
--     none holds the gate to its end, so a build without the binding that arrives meanwhile
--     commits its arrival only after every such transaction's binding writes have committed.
--
-- The gate is the owner lock gate's namespace with a key of its own, so an arrival of a build at
-- owner lock protocol 1 waits for no owner lock transaction and makes no owner lock try fail: those
-- hold and try key 0 alone. An arrival at protocol 0 fires both triggers of its table, this
-- migration's first (Postgres fires a table's triggers in the order of their names), so it takes
-- key 1 and then key 0; no transaction waits for either, since a holder only ever tries them.
--
-- The registration's wait for the gate is bounded at 2 seconds (lock_timeout, restored as the
-- function returns), after which the insert fails with 55P03 lock_not_available, as the owner lock
-- gate's bound of 5 seconds does (20261115200014 says why a registration's wait must be short: it
-- holds the registry's lock, which every heartbeat takes). A registration of a build before the
-- owner lock fires both, so it waits at most 7 seconds. The binding's gate is held shared only by a
-- sign-in's, a mint's and a decision's re-read's transaction, each a few statements after the
-- address's lock, so the bound is met in practice by a wait of milliseconds.
--
-- Every name is written with its schema and every function pins search_path = pg_catalog, public,
-- pg_temp, as the binding's guards pin theirs (20261119065002): a session's temporary table or
-- operator must not answer for the fleet, and 20261115200014 already holds Dispatch to schema
-- public.
--
-- Rolled back to a release without the binding, the columns, the mark and the triggers stay: that
-- release's publishes and registrations take the gate and stamp the mark, which nothing of that
-- release reads, and no transaction of it holds the gate, so they wait only for this build's.
--
-- Locks: each ALTER takes its table ACCESS EXCLUSIVE for the catalog write, which the mark's insert
-- then reads dashboard_builds under, and each CREATE TRIGGER its table SHARE ROW EXCLUSIVE, until
-- the commit; both tables take a write only at a process's boot and on its heartbeat, so a write
-- waits for an instant and a read not at all.
alter table public.server_processes
  add column account_binding integer not null default 0;

alter table public.dashboard_builds
  add column account_binding integer not null default 0;

create table public.account_binding_legacy_mark (
  singleton boolean primary key default true check (singleton),
  seen_at timestamptz not null
);

-- Every row of dashboard_builds reads 0 here, the column having just been added, so the newest
-- last_started_at among them is when a build without the binding last booted.
insert into public.account_binding_legacy_mark (singleton, seen_at)
select true, pg_catalog.max(last_started_at) from public.dashboard_builds
where account_binding = 0
having pg_catalog.max(last_started_at) > pg_catalog.now() - interval '1 hour';

-- The mark only moves forward: two arrivals take the gate one after the other, and the second's
-- transaction may have started before the first's.
create function public.account_binding_legacy_arrival() returns trigger language plpgsql
set search_path = pg_catalog, public, pg_temp as $$
begin
  perform pg_catalog.pg_advisory_xact_lock(1331121735, 1);
  insert into public.account_binding_legacy_mark (singleton, seen_at) values (true, pg_catalog.now())
  on conflict (singleton) do update
    set seen_at = greatest(public.account_binding_legacy_mark.seen_at, excluded.seen_at);
  return new;
end $$;

create function public.account_binding_legacy_registration() returns trigger language plpgsql
set search_path = pg_catalog, public, pg_temp
set lock_timeout = '2s' as $$
begin
  perform pg_catalog.pg_advisory_xact_lock(1331121735, 1);
  insert into public.account_binding_legacy_mark (singleton, seen_at) values (true, pg_catalog.now())
  on conflict (singleton) do update
    set seen_at = greatest(public.account_binding_legacy_mark.seen_at, excluded.seen_at);
  return new;
end $$;

create trigger dashboard_builds_account_binding_legacy_arrival before insert on public.dashboard_builds
  for each row when (new.account_binding = 0) execute function public.account_binding_legacy_arrival();

create trigger server_processes_account_binding_legacy_arrival before insert on public.server_processes
  for each row when (new.account_binding = 0) execute function public.account_binding_legacy_registration();

-- Volatile, the default, written out because the second read rests on it: under READ COMMITTED a
-- volatile function takes a snapshot of its own for each statement it runs rather than reading
-- under its caller's, so the read account_binding_fleet makes after its try sees a mark an arrival
-- committed between its first read and the try.
create function public.account_binding_fleet_read(legacy_window_secs double precision, mark_window_secs double precision)
returns boolean
language sql volatile
set search_path = pg_catalog, public, pg_temp as $$
  select exists (
      select from public.server_processes
      where account_binding = 0
        and heartbeat_at > pg_catalog.now() - pg_catalog.make_interval(secs => legacy_window_secs))
    or exists (
      select from public.account_binding_legacy_mark
      where seen_at > pg_catalog.now() - pg_catalog.make_interval(secs => mark_window_secs))
$$;

-- True while a build without the binding is in the fleet. The gate is only tried, never waited
-- for: Postgres grants no conditional request that conflicts with a request already queued
-- (lock.c, LockAcquireExtended), so a try fails while an arrival holds the gate or waits for it,
-- and that answers true. No caller ever queues behind an arrival.
create function public.account_binding_fleet(legacy_window_secs double precision, mark_window_secs double precision)
returns boolean
language plpgsql
set search_path = pg_catalog, public, pg_temp as $$
begin
  if public.account_binding_fleet_read(legacy_window_secs, mark_window_secs) then
    return true;
  end if;
  if not pg_catalog.pg_try_advisory_xact_lock_shared(1331121735, 1) then
    return true;
  end if;
  return public.account_binding_fleet_read(legacy_window_secs, mark_window_secs);
end $$;
