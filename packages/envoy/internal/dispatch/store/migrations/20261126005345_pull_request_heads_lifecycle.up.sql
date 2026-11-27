-- 20261126005345_pull_request_heads_lifecycle.up.sql
--
-- Every write that opens or closes a row of pull_request_heads, on the database's own clock, kept
-- until the former heads install has read it (pull_request_heads_lifecycle): one row for each
-- insert of a row, open, closed or merged, and for each update that makes an open row closed or
-- merged, makes a closed or merged row open, or keys an open row anew, whichever writer and
-- whichever build wrote it, under the name of the row written (lowercased). Each row says whether
-- the write left the row open (open) and whether the row it wrote was open before it (from_open:
-- false for an insert), with the clock's time at that write (at, clock_timestamp()), which comes
-- after Dispatch read the state from GitHub. No write of pull_request_heads changes or deletes a
-- row of it: a reopen, a re-key and a delete of the pull request's row leave every write recorded
-- before them. A write that leaves a closed row closed or merged (an edit, a read that finds it
-- closed still, a rewrite as merged, a re-key of a closed row) opens and closes nothing, and is not
-- recorded.
-- pull_request_heads.event_at is not a time of this record: it is the pull request's updated_at
-- on GitHub's clock, which each later edit of the closed pull request moves, and a write that
-- names none keeps an earlier one.
--
-- The former heads install (migration 20261126005348) is the record's one reader, and the
-- migration after it (20261126005349) drops the table, its triggers and its function, so no write
-- appends to it once the install has read it. The install commits in a transaction of its own
-- after this one, and a writer of the release before it can close a pull request and reopen it
-- between the two, which leaves its row open. So the install reads this record, not the rows'
-- states alone, and finds in it where each pull request's last closed period began, by the writes
-- of every name of it (that migration says how): at a close recorded after the last write recorded
-- that opened a row of it, never at a write of the pull request closed already that closes no
-- open row, such as the canonical-name writer's first write of a pull request it reads closed,
-- which then deletes the row of its old name. So no later write of the closed pull request moves
-- that start past a result recorded in its closed period, and a close, a reopen and a second
-- close begin it at the second.
--
-- This migration also records each pull request closed or merged when it runs, with no time (at
-- null): nothing of Dispatch's says when such a pull request closed, only that it closed before
-- the record began, so the record dates it by no clock, and the install keeps out every result
-- recorded before it of a pull request whose last closed period began so (that migration says
-- why). A pull request is closed where the row of its canonical name (pull_request_canonical_repo,
-- below) is not open, or, with no row of that name, where no row of it is open: the canonical row
-- is the one Dispatch reads, and a row stored under an old name may be a stale copy, so an open
-- pull request whose old name's row reads closed is recorded as no close here, the rule the
-- install reads a pull request by. A write that closes such an old name's row later is recorded
-- as every close is, as the settler, once the install runs, ends the lifecycle at a row written
-- closed under any name of an open pull request. A pull request open when this migration runs is
-- recorded as nothing: the record shows only the writes that follow.
--
-- pull_request_heads_lifecycle holds no key and its index is not unique, and seq is drawn from
-- the identity's sequence, so the trigger's insert waits for no other transaction, and the
-- trigger reads nothing: it adds no lock to a writer's transaction. seq orders two writes at one
-- time. The first CREATE TRIGGER takes SHARE ROW EXCLUSIVE on pull_request_heads, after waiting
-- for each transaction that holds the table, a wait the runner bounds at pgmigrate.LockTimeout and
-- tries again, and holds it to the commit, so a write of the table either committed before the
-- record of the rows closed now reads it or waits for the commit and fires the triggers: no close
-- is in neither. The release before this one neither reads nor writes the new table, and its
-- writes fire the triggers too, so the two roll side by side. Nothing existing is refused or
-- rewritten: the census answers 0.
create table pull_request_heads_lifecycle (
    repo text not null,
    number integer not null,
    open boolean not null,
    from_open boolean not null,
    at timestamptz default clock_timestamp(),
    seq bigint generated always as identity
);
create index pull_request_heads_lifecycle_number on pull_request_heads_lifecycle (number, repo);

-- The canonical name of a repository's name, by the rename rule every Go reader holds
-- (store.ReadRepoRenames, refs.Renames.Canonical, refs.CanonicalRepoSQL): the pairs are the
-- repo_projects rows with a renamed_to, both names in lower case, and a row whose two names are
-- then equal, or one of them empty, renames nothing; of the pairs of one old name, the one with
-- the greatest new name in byte order stands; a name that is a pair's old name in any case is that
-- pair's new name, and any other name is itself as it is given. So the row a writer keys under the
-- canonical name (delivery's Attributor) is the row this name finds. A database without
-- repo_projects holds no pairs, as ReadRepoRenames reads none there, so the table is read only
-- once it is there. The install (20261126005348) and the settler read it too.
create function pull_request_canonical_repo(text) returns text language plpgsql stable as $$
declare
  renamed text;
begin
  if $1 is null or to_regclass('repo_projects') is null then
    return $1;
  end if;
  execute $read$
    select max(lower(pair.renamed_to) collate "C") from repo_projects pair
    where pair.renamed_to is not null and lower(pair.repo) = lower($1)
      and lower(pair.repo) <> '' and lower(pair.renamed_to) <> ''
      and lower(pair.repo) <> lower(pair.renamed_to)
  $read$ into renamed using $1;
  return coalesce(renamed, $1);
end
$$;

-- A write of pull_request_heads that opens or closes a row (its triggers' when says which): one
-- record of it, under the name of the row written, whether it left the row open and whether the
-- row was open before it. It appends: it waits for no lock.
create function pull_request_heads_lifecycle_keep() returns trigger language plpgsql as $$
begin
  if tg_op = 'INSERT' then
    insert into pull_request_heads_lifecycle (repo, number, open, from_open)
    values (lower(new.repo), new.number, new.state = 'open', false);
  else
    insert into pull_request_heads_lifecycle (repo, number, open, from_open)
    values (lower(new.repo), new.number, new.state = 'open', old.state = 'open');
  end if;
  return null;
end $$;

create trigger pull_request_heads_lifecycle_insert after insert on pull_request_heads
  for each row execute function pull_request_heads_lifecycle_keep();
create trigger pull_request_heads_lifecycle_update after update on pull_request_heads
  for each row when ((old.state = 'open') <> (new.state = 'open')
    or (new.state = 'open' and (old.repo, old.number) is distinct from (new.repo, new.number)))
  execute function pull_request_heads_lifecycle_keep();

-- Each pull request closed or merged now, by the rule above, undated: one record of each of its
-- closed rows that decide it. Each name's canonical name is read once.
with named as materialized (
  select lower(head.repo) as repo, head.number, head.state <> 'open' as closed,
    lower(pull_request_canonical_repo(head.repo)) as pull_repo
  from pull_request_heads head
)
insert into pull_request_heads_lifecycle (repo, number, open, from_open, at)
select head.repo, head.number, false, false, null from named head
where head.closed and not exists (
  select 1 from named other
  where other.number = head.number and other.pull_repo = head.pull_repo and not other.closed
    and (other.repo = other.pull_repo or head.repo <> head.pull_repo));
