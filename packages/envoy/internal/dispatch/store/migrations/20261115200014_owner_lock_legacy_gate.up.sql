-- 20261115200014_owner_lock_legacy_gate.up.sql
-- INFRA-636: an earlier build's boot is seen before it writes anything an owner lock guards, and a
-- transaction of this build that read the fleet as having no earlier build in it has ended before
-- an earlier build is seen.
--
-- 20261115100924 registers each server process's owner lock protocol: a build at 1 serialises an
-- owner's writers on the owner's advisory lock and takes the owner's row only in its write phase
-- (store.LockOwner); a build before it, whose rows read the default 0, serialises them on the
-- owner's row alone, which it takes first. While the fleet holds a build at 0, a writer at 1 takes
-- the owner's row too, right after the advisory lock (the legacy mode). Two things left a window:
--
--   - every earlier build writes owners before it registers (its boot's repairs and seeds, the
--     background passes that start before its registration), so a fleet read of server_processes
--     alone missed it for that time;
--   - a transaction at 1 that read the fleet as having no earlier build in it holds its owner lock
--     and writes the owner's row only in its write phase, so an earlier build that arrives between
--     that read and that write writes the owner under it unseen.
--
-- Every earlier build publishes its dashboard (dashboard_builds, migration 0136) before its first
-- owner write and exits when the publish fails, and registers in server_processes before it
-- serves; both are an `insert ... on conflict ... do update`, which fires BEFORE INSERT whether or
-- not the row exists, and so is every return of a lapsed registration (its heartbeat's admit). So:
--
--   - dashboard_builds.owner_lock_protocol, a constant default of 0 that rewrites no row: this
--     build's publish names 1 (dashboard.Publish), an earlier build's names nothing and reads 0.
--     Nothing reads the column; it tells the trigger below which build wrote the row.
--   - owner_lock_legacy_mark, one row: when an earlier build was last seen arriving.
--   - owner_lock_legacy_arrival, a trigger on both tables for a row at protocol 0, BEFORE INSERT:
--     it takes the gate, a transaction advisory lock of its own (two keys, 0x4F574E47 "OWNG" and 0,
--     store.OwnerLockGateNamespace and store.OwnerLockGateKey), exclusively, and stamps the mark.
--     So the earlier build's publish or registration waits for every transaction of this build
--     that holds the gate, and its own transaction holds the gate until it commits.
--   - owner_lock_fleet, the fleet read: an earlier build is in the fleet while a row at protocol 0
--     has heartbeated within the window or the mark is younger than it, the window being
--     store.OwnerLockLegacyWindow (processes.PruneAge, 10 minutes), passed in by the caller. When
--     it finds none it takes the gate shared and reads again, under a snapshot of its own, so a
--     mark committed while it waited for the gate is read.
--   - owner_lock, which every read-phase owner lock statement of this build calls: the fleet read,
--     and then the owners' advisory locks, exclusive, shared or tried, so the gate is always taken
--     before any owner lock of the transaction. A transaction in the legacy mode takes no gate,
--     and an earlier build's arrival never waits for one.
--
-- Every name is written with its schema, and every function it calls with pg_catalog: these
-- functions run under the caller's search_path, and a login that can create objects in schema
-- public must not be able to put a name of its own ahead of these.
--
-- The release before this one names its columns in every insert and select it makes on both
-- tables, so the column is one it neither reads nor writes, and the triggers and the functions are
-- run only by its inserts and by this build (cmd/dispatch/README.md, "Database migrations", "Two
-- builds at once"). Rolled back to that release, the column, the mark and the triggers stay: its
-- publishes and registrations take the gate and stamp the mark, which nothing of that release
-- reads, and no transaction of that release holds the gate, so they never wait.
--
-- Locks: the ALTER takes dashboard_builds ACCESS EXCLUSIVE for the catalog write, and each
-- CREATE TRIGGER its table SHARE ROW EXCLUSIVE, until the commit; both tables take a write only at
-- a process's boot and on its heartbeat, so a write waits for an instant and a read not at all.
alter table public.dashboard_builds
  add column owner_lock_protocol integer not null default 0;

create table public.owner_lock_legacy_mark (
  singleton boolean primary key default true check (singleton),
  seen_at timestamptz not null
);

-- The mark only moves forward: two arrivals take the gate one after the other, and the second's
-- transaction may have started before the first's.
create function public.owner_lock_legacy_arrival() returns trigger language plpgsql as $$
begin
  perform pg_catalog.pg_advisory_xact_lock(1331121735, 0);
  insert into public.owner_lock_legacy_mark (singleton, seen_at) values (true, pg_catalog.now())
  on conflict (singleton) do update
    set seen_at = greatest(public.owner_lock_legacy_mark.seen_at, excluded.seen_at);
  return new;
end $$;

create trigger dashboard_builds_owner_lock_legacy_arrival before insert on public.dashboard_builds
  for each row when (new.owner_lock_protocol = 0) execute function public.owner_lock_legacy_arrival();

create trigger server_processes_owner_lock_legacy_arrival before insert on public.server_processes
  for each row when (new.owner_lock_protocol = 0) execute function public.owner_lock_legacy_arrival();

-- Volatile, as a plpgsql function is unless it says otherwise: under READ COMMITTED each of its
-- statements reads under a snapshot taken when that statement starts, so the second read sees a
-- mark the arrival that held the gate committed while the shared lock waited.
create function public.owner_lock_fleet(legacy_window_secs double precision) returns boolean
language plpgsql as $$
declare
  legacy boolean;
begin
  select exists (
      select from public.server_processes
      where owner_lock_protocol = 0
        and heartbeat_at > pg_catalog.now() - pg_catalog.make_interval(secs => legacy_window_secs))
    or exists (
      select from public.owner_lock_legacy_mark
      where seen_at > pg_catalog.now() - pg_catalog.make_interval(secs => legacy_window_secs))
  into legacy;
  if legacy then
    return true;
  end if;
  perform pg_catalog.pg_advisory_xact_lock_shared(1331121735, 0);
  select exists (
      select from public.server_processes
      where owner_lock_protocol = 0
        and heartbeat_at > pg_catalog.now() - pg_catalog.make_interval(secs => legacy_window_secs))
    or exists (
      select from public.owner_lock_legacy_mark
      where seen_at > pg_catalog.now() - pg_catalog.make_interval(secs => legacy_window_secs))
  into legacy;
  return legacy;
end $$;

-- lock_mode 'exclusive' and 'shared' take the lock of every key, deduplicated by its hashtext and
-- in that hash's order, so two transactions take any two keys in one order, and answer one row;
-- 'try' tries each key in the order given without waiting and answers a row for each it took.
-- Every row carries the fleet read.
create function public.owner_lock(lock_mode text, lock_namespace integer, lock_keys text[],
    legacy_window_secs double precision)
returns table (locked_key text, legacy_fleet boolean)
language plpgsql as $$
declare
  key_hash integer;
begin
  if lock_mode not in ('exclusive', 'shared', 'try') then
    raise exception 'owner_lock: no lock mode %', lock_mode;
  end if;
  legacy_fleet := public.owner_lock_fleet(legacy_window_secs);
  if lock_mode = 'try' then
    for locked_key in
      select listed.lock_key
      from pg_catalog.unnest(lock_keys) with ordinality as listed(lock_key, place)
      order by listed.place
    loop
      if pg_catalog.pg_try_advisory_xact_lock(lock_namespace, pg_catalog.hashtext(locked_key)) then
        return next;
      end if;
    end loop;
    return;
  end if;
  for key_hash in
    select distinct pg_catalog.hashtext(keyed.lock_key)
    from pg_catalog.unnest(lock_keys) as keyed(lock_key)
    order by 1
  loop
    if lock_mode = 'shared' then
      perform pg_catalog.pg_advisory_xact_lock_shared(lock_namespace, key_hash);
    else
      perform pg_catalog.pg_advisory_xact_lock(lock_namespace, key_hash);
    end if;
  end loop;
  return next;
end $$;
