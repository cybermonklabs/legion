-- 20261115200014_owner_lock_legacy_gate.up.sql
-- INFRA-636: an earlier build's boot is seen at its first step that can be seen, its dashboard
-- publish or else its registration, and a transaction of this build that read the fleet as having no
-- earlier build in it has ended before an earlier build is seen.
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
-- An earlier build that serves a dashboard, as the image always does, publishes it (dashboard_builds,
-- migration 0136) before its first owner write and exits when the publish fails. One that serves
-- none (its web dist holds no index.html, dashboard.ErrNoBuild) publishes nothing, and its boot's
-- repairs and background passes write before its registration, the first step of it that can be
-- seen. Every earlier build registers in server_processes before it serves. Both are an
-- `insert ... on conflict ... do update`, which fires BEFORE INSERT whether or not the row exists,
-- and so is every return of a lapsed registration (its heartbeat's admit). So:
--
--   - dashboard_builds.owner_lock_protocol, a constant default of 0 that rewrites no row: this
--     build's publish names 1 (dashboard.Publish), an earlier build's names nothing and reads 0.
--     Only the mark's insert below and the trigger read the column: it tells them which build
--     wrote the row.
--   - owner_lock_legacy_mark, one row: when an earlier build was last seen arriving. The migration
--     stamps it at the newest earlier publish already in dashboard_builds, when that is within the
--     hour, since a publish that came before the trigger made no arrival.
--   - owner_lock_legacy_arrival on dashboard_builds and owner_lock_legacy_registration on
--     server_processes, triggers for a row at protocol 0, BEFORE INSERT: each takes the gate, a
--     transaction advisory lock of its own (two keys, 0x4F574E47 "OWNG" and 0,
--     store.OwnerLockGateNamespace and store.OwnerLockGateKey), exclusively, and stamps the mark.
--     So the earlier build's publish or registration waits for every transaction of this build
--     that holds the gate, and its own transaction holds the gate until it commits. A publish waits
--     as long as the earlier build's publish deadline lets it; a registration, which holds the
--     registry's lock that every heartbeat takes, waits at most 5 seconds and is then refused.
--   - owner_lock_fleet_read, the fleet read: an earlier build is in the fleet while a row at
--     protocol 0 has heartbeated within one window, store.OwnerLockLegacyWindow
--     (processes.PruneAge, 10 minutes), so until it is deregistered or pruned, or while the mark
--     is younger than another, store.OwnerLockMarkWindow (an hour), which covers an earlier
--     build's boot from its publish to its registration; the caller passes both.
--   - owner_lock_fleet, which reads the fleet, and when it finds none tries the gate shared,
--     without waiting: a try that fails, while an arrival holds the gate or waits for it, answers
--     as an earlier build in the fleet, and one that succeeds reads the fleet again, under a
--     snapshot of its own, so a mark committed before the try is read.
--   - owner_lock, which every read-phase owner lock statement of this build calls: the fleet read,
--     and then the owners' advisory locks, exclusive, shared or tried, so the gate is tried before
--     any owner lock of the transaction. A transaction in the legacy mode holds no gate, and no
--     owner lock ever waits for the gate: an arrival waits only for the transactions that held it
--     when it asked.
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
-- Locks: the ALTER takes dashboard_builds ACCESS EXCLUSIVE for the catalog write, which the mark's
-- insert then reads under, and each CREATE TRIGGER its table SHARE ROW EXCLUSIVE, until the
-- commit; both tables take a write only at a process's boot and on its heartbeat, so a write waits
-- for an instant and a read not at all.
alter table public.dashboard_builds
  add column owner_lock_protocol integer not null default 0;

create table public.owner_lock_legacy_mark (
  singleton boolean primary key default true check (singleton),
  seen_at timestamptz not null
);

-- An earlier build whose dashboard publish committed before this migration ran made no arrival:
-- its insert came before the trigger below. Every row of dashboard_builds reads 0 here, the column
-- having just been added, so the newest last_started_at among them is when an earlier build last
-- booted, and the mark starts there when that is within the mark's window
-- (store.OwnerLockMarkWindow, an hour, spelled again here). A database no build has published to,
-- as every test's is, starts with no mark.
insert into public.owner_lock_legacy_mark (singleton, seen_at)
select true, pg_catalog.max(last_started_at) from public.dashboard_builds
where owner_lock_protocol = 0
having pg_catalog.max(last_started_at) > pg_catalog.now() - interval '1 hour';

-- The mark only moves forward: two arrivals take the gate one after the other, and the second's
-- transaction may have started before the first's.
--
-- owner_lock_legacy_arrival, a publish's, waits for the gate as long as the publish lets it: every
-- earlier build bounds its publish at 60 seconds and ends its boot when it fails.
-- owner_lock_legacy_registration, a registration's, is the same with its wait for the gate bounded
-- at 5 seconds (lock_timeout, restored as the function returns), after which the insert fails with
-- 55P03 lock_not_available. A registration holds the registry's lock (processes.registryLock)
-- while it waits, and every process's heartbeat takes that lock within a 2 second bound of its
-- own, so a longer wait would stop every heartbeat of the fleet toward the 15 second live window.
-- An earlier build's fresh registration that fails ends its process (its main exits 1), and the
-- return of its lapsed row, bounded at 2 seconds by that heartbeat's own bound, is logged and
-- tried again at its next heartbeat.
create function public.owner_lock_legacy_arrival() returns trigger language plpgsql as $$
begin
  perform pg_catalog.pg_advisory_xact_lock(1331121735, 0);
  insert into public.owner_lock_legacy_mark (singleton, seen_at) values (true, pg_catalog.now())
  on conflict (singleton) do update
    set seen_at = greatest(public.owner_lock_legacy_mark.seen_at, excluded.seen_at);
  return new;
end $$;

create function public.owner_lock_legacy_registration() returns trigger language plpgsql
set lock_timeout = '5s' as $$
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
  for each row when (new.owner_lock_protocol = 0) execute function public.owner_lock_legacy_registration();

-- The fleet read, which owner_lock_fleet makes twice. Volatile, the default, written out because
-- the second read rests on it: under READ COMMITTED a volatile function takes a snapshot of its
-- own for its statement rather than reading under its caller's, so the read owner_lock_fleet
-- makes after its try sees a mark an arrival committed between its first read and the try.
create function public.owner_lock_fleet_read(legacy_window_secs double precision, mark_window_secs double precision)
returns boolean
language sql volatile as $$
  select exists (
      select from public.server_processes
      where owner_lock_protocol = 0
        and heartbeat_at > pg_catalog.now() - pg_catalog.make_interval(secs => legacy_window_secs))
    or exists (
      select from public.owner_lock_legacy_mark
      where seen_at > pg_catalog.now() - pg_catalog.make_interval(secs => mark_window_secs))
$$;

-- Volatile, as a plpgsql function is unless it says otherwise: under READ COMMITTED each of its
-- statements reads under a snapshot taken when that statement starts, so its second fleet read, a
-- statement of its own after the try, sees a mark an arrival committed between the first read and
-- the try.
--
-- The gate is only tried, never waited for. A try fails while an arrival holds the gate or waits
-- for it: Postgres grants no conditional request that conflicts with a request already queued
-- (lock.c, LockAcquireExtended, which checks the queue's wait mask before the granted locks). A
-- failed try answers that an earlier build is in the fleet, so the transaction takes the owners'
-- rows as it does beside one, and no writer queues behind an arrival: an arrival waits only for
-- the transactions that held the gate when it asked for it.
create function public.owner_lock_fleet(legacy_window_secs double precision, mark_window_secs double precision)
returns boolean
language plpgsql as $$
begin
  if public.owner_lock_fleet_read(legacy_window_secs, mark_window_secs) then
    return true;
  end if;
  if not pg_catalog.pg_try_advisory_xact_lock_shared(1331121735, 0) then
    return true;
  end if;
  return public.owner_lock_fleet_read(legacy_window_secs, mark_window_secs);
end $$;

-- lock_mode 'exclusive' and 'shared' take the lock of every key, deduplicated by its hashtext and
-- in that hash's order, so two transactions take any two keys in one order, and answer one row;
-- 'try' tries each key in the order given without waiting and answers a row for each it took.
-- Every row carries the fleet read.
create function public.owner_lock(lock_mode text, lock_namespace integer, lock_keys text[],
    legacy_window_secs double precision, mark_window_secs double precision)
returns table (locked_key text, legacy_fleet boolean)
language plpgsql as $$
declare
  key_hash integer;
begin
  if lock_mode not in ('exclusive', 'shared', 'try') then
    raise exception 'owner_lock: no lock mode %', lock_mode;
  end if;
  legacy_fleet := public.owner_lock_fleet(legacy_window_secs, mark_window_secs);
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
