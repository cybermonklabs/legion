-- 20261125005352_pull_request_former_heads.up.sql
--
-- The former heads of each open pull request: each head the pull request had while it was open
-- and has since left, which the check states settler computes a state at as it does at an open
-- head, so a tester's result at the head it tested still decides after a push moves the pull
-- request on (api/check_states.go, formerHeadsOf).
--
-- No trigger here makes a writer wait for a lock. Each trigger this migration puts on a writer's
-- table (pull_request_heads, check_results, ci_check_results) only reads and appends one row to a
-- table that has no unique key and no foreign key, and marks an issue owed (check_states_owe, whose
-- mark has a key a sequence draws and whose notification queues at the commit, after every lock
-- the transaction takes); an insert into such a table waits for no other transaction, and a read
-- under READ COMMITTED waits for none either. Everything that reads and rewrites the record of
-- former heads runs in the check states settler: under the issue's lease
-- (pull_request_former_heads_drain), and under the pull request's lock
-- (pull_request_former_heads_lock), which no writer takes.
-- So these triggers add no lock to a writer's transaction, and none can close a cycle with the
-- locks the writers take themselves, each in its own order: a result's write takes the owner locks
-- and then inserts (api/results.go, recordResultUnderOwnerLocks), a head's write takes its row and
-- then the owner locks (delivery.UpsertPullRequestHead, appendAttributionChange), a change of an
-- issue's links takes the owner lock and then the rows (delivery.LinksChanged), and the re-key's
-- delete takes the old row alone. A trigger that took a lock one writer holds while another waits
-- for it in the other order would close such a cycle, which Postgres breaks by failing one writer.
--
-- One pull request has one identity, a number Dispatch draws for it (pull), under every name its
-- repository goes by: a name it is stored under, or a name its repository was renamed away from
-- (repo_projects.renamed_to, the rule of refs.CanonicalRepoSQL). Its history, its records, its
-- lifecycle, its lock and the claim of its read all go by that identity; a name chooses only the
-- GitHub route a read takes and the repository an event names. A writer appends its record under
-- the name it wrote, whatever Dispatch knows of renames then, and the settler finds the record's
-- identity when it applies it, so a record appended before Dispatch learned of a rename is of the
-- same pull request as every record after.
--
--   pull_request_former_heads_pulls
--                              one row per pull request: its identity, its number, ended, the
--                              place in pull_request_former_heads_order of the last close or merge
--                              of it under any of its names, null where none is known, and
--                              generation, its lifecycle's number. A rename and a move to another
--                              issue keep the lifecycle; each close or merge applied ends it, and
--                              the reopened pull request is of the next (generation + 1), so an
--                              answer read in one lifecycle can tell it is about an ended one.
--   pull_request_former_heads_aliases
--                              each name (lowercased) and number the settler has found a pull
--                              request under, and its identity. A name a rename maps to another
--                              name of a known pull request is that pull request: the settler adds
--                              it to the pull request's names the first time it meets it
--                              (pull_request_former_heads_resolve), and a name never leaves.
--   pull_request_former_heads  one row per pull request (its identity) and head it left while
--                              open, with when it last left that head and since, the place at
--                              which the pull request reached that head, or, on a candidate, the
--                              place of the result that made it one (the install's place for a
--                              result recorded before this release), null where no applied
--                              record shows the place. verified is false on a
--                              candidate, a commit of the results of the pull request's issue
--                              (below), which no read of the pull request's commits has confirmed
--                              yet: the settler computes no state there until one does.
--                              incarnation is drawn from pull_request_former_heads_order at the
--                              row's insert and again at each update of it (a trigger of its own,
--                              pull_request_former_heads_incarnation), so a row replaced or changed
--                              since a read of it is told apart from the row that read saw. No
--                              foreign key, and only the settler writes it.
--   pull_request_former_heads_moves
--                              the writers' record: one row for each write of pull_request_heads
--                              that can move a pull request's history, with its place in
--                              pull_request_former_heads_order, the name and number of the row
--                              written, and its head and state before and after, which
--                              pull_request_former_heads_keep() appends in the writer's own
--                              transaction, whichever build wrote the row. No key: an insert waits
--                              for nothing. The settler deletes each row it applied.
--   pull_request_former_heads_pending
--                              the commits and the rows owed to an issue's candidates: a result's
--                              commit, or a row that starts an open lifecycle at the issue, which
--                              the triggers below append, each with its place in
--                              pull_request_former_heads_order, and the issue's settle drains. No
--                              key.
--   pull_request_former_heads_results
--                              the settler's own record of when each result was recorded: for an
--                              issue and a commit of its population (below), the latest place of a
--                              result there, which the drain keeps from the pending rows, and the
--                              install from the results recorded before it, in the order of their
--                              times (below).
--   pull_request_former_heads_rows
--                              the settler's own state: for each open row of a pull request it
--                              applied a record of, by its name, the head it stands at and the
--                              place at which it reached that head.
--   pull_request_former_heads_reads
--                              one row per pull request whose candidates a settle is reading
--                              GitHub about now, by its identity, which is the same under every
--                              issue it moves to and every name it is stored under, with until
--                              when the claim holds and its token, drawn from
--                              pull_request_former_heads_order by the claim that took it. The
--                              settle that holds it makes the one read; a settle of any other
--                              issue leaves the candidates to it (api/check_states.go,
--                              verifyFormerHeads). The answer gives the row back by its token; one
--                              a stopped process left is taken over, with a new token, once its
--                              time has passed.
--
-- A settle applies GitHub's answer about a pull request's candidates through one operation,
-- pull_request_former_heads_answer, and through no other path. The claim
-- (pull_request_former_heads_claim) answers what the read is about: the claim's token, the pull
-- request's generation, a snapshot of its open rows (pull_request_former_heads_snapshot: each row's
-- name, number, head, base branch and the revision of the base branch the row was last read at),
-- the head and the base revision of the row the read goes by, read with that snapshot, and each
-- candidate with its incarnation. The read is GitHub's comparison of those two commits
-- (delivery.PullRequestMembership), so its answer is about them by construction, whatever GitHub's
-- pull request stands at while it is read; a base branch moves with no change of its name, so the
-- snapshot and the read name its revision, not the name alone. The answer brings the claim back
-- with each member and each commit shown outside, and, under the pull request's lock, applies only
-- where the claim with that token is still live, the generation and the snapshot are still the
-- pull request's, so the row still stores the two commits the read compared, and each candidate
-- still has the incarnation the read saw; a positive answer marks it verified, a negative one
-- deletes it. Any other answer, about a claim that ran out or was taken over, about a lifecycle
-- that ended, about a head or a base revision since stored anew, or about a candidate since
-- replaced, applies nothing, and its candidates are read again. A row that stores no base revision
-- is not read until a write stores one, and a read that finds GitHub's pull request at another
-- head or base revision than the row stores decides nothing: no write has stored where it went
-- yet, so both keep the obligation and read again after the failure backoff.
--
-- pull_request_former_heads_keep() records each insert of a row, open, closed or merged, each
-- update that moves a row's key, head or state while it is or becomes open, and each delete, by an
-- insert, an update and a delete trigger, each record under the name of the row it is of: an
-- update that keys the row anew is the delete of the row under its old name and the insert of the
-- row under its new one. The update trigger fires only where the key, head_sha or state moved,
-- since every writer of the row names each column in its `do update set` on each write, a
-- read-through included. So a head is recorded whether or not a settle ran while the pull request
-- was there, every writer is recorded, a write of the release before this one too, and a writer a
-- later change adds is recorded with no code of its own. The records of one row are in the order
-- of their places: a writer holds the row's lock from its write to its commit, so the next writer
-- of the row draws its place after that commit.
--
-- pull_request_former_heads_apply(pull request, issue) applies a pull request's records, the
-- records of every name it goes by, under its lock, in the order of their places, before any read
-- of its former heads: an open row that moves on from a head keeps that head, verified, with the
-- place at which it reached it; an open row that goes, as the re-key's delete of the row stored
-- under a name the repository was renamed away from, keeps the head it stood at the same way,
-- since the pull request it stood for is the one its other name stands for; a close or a merge
-- under any name ends the pull request's lifecycle, so it raises the pull request's ended
-- place, starts the next generation and drops every row of history reached before it,
-- candidates whose result was recorded before it included. A close is every way Dispatch can
-- first see one: an open row that closes or merges, and as much a row written closed or merged
-- by its insert or its re-key, such as the canonical-name writer's first write of a pull request
-- it finds closed, which then deletes the open row of its old name (whose head that delete keeps
-- only where it was reached after the close), and the install's record of where the last closed
-- period of each pull request before it began, whether the pull request is closed when the
-- install runs or was reopened since (below); a close no update of an open row shows would
-- otherwise leave the lifecycle open, and a reopen would bring back its heads and its results. And
-- a head reached before the last end is of a lifecycle that ended, so a move on from it keeps
-- nothing. A row stored under an old name can stay open, unread, while the row under the
-- canonical name closes and reopens, and the head it stands at is of the lifecycle the close
-- ended. A record of another name can commit after a record of a later place was applied;
-- whatever the order, a row of history stays only where its place is after the pull request's
-- last end, and each commit an end drops, or a move on keeps nothing of, is left again where a
-- result there was recorded after the end (pull_request_former_heads_leave_again: a row holds the
-- place of what put it there first, and a later result's pending row is taken once, so a late
-- end would otherwise drop what that result supplied), so the settler answers as it would have,
-- had every record committed in the order of its places. An application that changes the
-- history owes the issue that holds the pull request when it applies, in its own transaction,
-- not only the issue a writer saw when it recorded the change: a record's own trigger marks the
-- issue its writer saw, which may have given the pull request up, and settled, before the record
-- was applied. So apply, like the join and the answer (below), marks owed each issue an open row
-- of the pull request is at, but the issue whose settle applies it, which reads the history next
-- anyway.
--
-- pull_request_former_heads_resolve(name, number, issue) is the identity of a pull request a name
-- and a number stand for today: the identity of that name where the settler met it before, else the
-- identity of any name of the same number that the rename rule makes the same repository, else a
-- new one. Where two identities turn out to be one pull request, because each name's records were
-- applied before Dispatch learned the rename that joins them, it joins them into the older:
-- pull_request_former_heads_join moves the younger's names, history and rows to the older, keeps
-- the later of their ends, and drops whatever that end ended. A join is an application too: each
-- issue that holds an open row under either name now holds the other name's history and the
-- joined lifecycle, which it has not settled, and no record of either name may be left to owe it,
-- since both were applied before; so the join marks owed each such issue, in its own transaction,
-- but the issue whose drain joins them, which reads the history next anyway. Creating and joining
-- identities runs under a lock of its own (the pull request lock's namespace and key 0, which no
-- identity holds), taken before any pull request's lock, and a settle takes pull requests' locks
-- in the order of their identities, so no two settles wait for each other in a cycle.
--
-- A head an open pull request left before this release is in no record. Such a head needs a state
-- only where someone recorded a result at it that the settle of the pull request's issue reads: a
-- result recorded, on any issue, against a check of that issue's Verification document
-- (api/results_rows.go, loadCheckResults, which the settle's computation reads the results by), in
-- check_results or ci_check_results. So the install seeds, for each open pull request attributed
-- to an issue, each distinct commit of such a result other than the pull request's head, as a
-- candidate: verified false, since a result there does not show that the commit was ever this
-- pull request's. The Verification document is the issue's document of that role, by the
-- predicate the server reads it by (api/artifacts.go, documentRolePredicate), which
-- check_states_owe_check_block (20261125005338) states too. The settler asks GitHub once which of
-- each such pull request's candidates are its own commits and keeps those (api/check_states.go,
-- verifyFormerHeads), and each issue holding a candidate is marked owed here, so that read comes
-- at the settler's first pass. A head left before this release with no result at it is not known,
-- and needs no state.
--
-- The same holds for a result recorded after this release at such a head, and for a head an open
-- pull request left while Dispatch held no open row of it at the issue: no record shows the pull
-- request leaving it, and the pull request now names another head. Dispatch's intake of GitHub's
-- webhooks can be off, and then the read-through of a result's commit writes a pull request's row
-- at GitHub's head after the result's own transaction, so a pull request first written at its
-- issue after a result at a head it already left is the common case, not a rare one. So a commit
-- of the population above, the results of an issue's Verification document, is a candidate of
-- each open pull request of that issue at another commit whenever either side is written:
--   - pull_request_former_heads_of_result(), an insert trigger on each result table, appends the
--     result's commit as pending for the issue whose settle reads the result
--     (pull_request_former_heads_reader);
--   - pull_request_former_heads_begin(), an insert and an update trigger on pull_request_heads,
--     appends as pending a row that starts an open lifecycle at an issue: an open row inserted, a
--     row reopened, a row keyed anew, and an open row that moves to another issue; it owes the
--     issue a settle;
--   - the settle of the issue first drains what is pending (pull_request_former_heads_drain): for
--     each pull request an open row of the issue stands for it applies that pull request's
--     records, and then leaves each pending result's commit, and the issue's whole population
--     (pull_request_former_heads_population) for a row pending as started;
--   - the install leaves the population for each open row, by the same function the drain calls.
-- Each leaves its candidates by pull_request_former_heads_leave(), unverified, where the pull
-- request holds no row of the commit yet; the settler confirms them as it confirms the install's.
-- Ended provenance stays ended: leave takes a commit only where a result there was recorded after
-- the pull request's last end (pull_request_former_heads_results), or, where no result there has a
-- place yet, only where no end of the pull request is known. The install places each result
-- recorded before it, and the start of the last closed period of each pull request before it, the
-- pull request closed when the install runs or reopened since, in the order of their times
-- (below), which it reads from Dispatch's record of each write that opened or closed a row
-- (pull_request_heads_lifecycle); the migration after this one drops that record, which nothing
-- reads once this one has. So a reopen, whose start leaves the issue's whole population, never
-- brings back a result of the lifecycle its close ended, however healthy the read that would
-- confirm it, and neither does a reopen before the install, which finds the pull request open;
-- while a result recorded after the install, or before it since a close Dispatch recorded, while
-- the pull request was closed included, is a candidate of the new lifecycle; and a head the
-- reopened pull request truly visits is its former head by its own record.
-- A head an open pull request left since this release has its row once applied, verified, so a
-- result there leaves nothing to read. They draw from one population through those functions, so
-- they cannot drift apart. A result and a row's start written at once in two transactions are
-- never both missed: the settles of an issue run under its lease, one at a time, and each pending
-- row is drained by a settle that starts after its writer committed, which owes it; of the two
-- drains, the later starts after both writers committed and reads both.
--
-- One transaction: the tables, the functions, the triggers and the seed commit together, and the
-- install holds the three tables they read and watch from the lock below to its commit: a writer
-- of pull_request_heads, check_results or ci_check_results either committed before the seed's
-- statement, which reads what it wrote, or waits for the install's commit and then fires the
-- triggers. So no head move and no result lands between the seed's read and the triggers. The
-- lock is SHARE ROW EXCLUSIVE, the one the triggers' catalog writes take anyway, taken in one
-- statement in one order: a read of any of the three is not held, every write of them waits.
--
-- No writer of one of the three writes another of them in its transaction: a result's write
-- (api/results.go, recordResultUnderOwnerLocks, a role's or a CI job's) reads pull_request_heads
-- only, under ACCESS SHARE, which the lock leaves free, and a head's write
-- (delivery.UpsertPullRequestHead and the re-key's delete) writes no result. The one write that
-- holds two of them is the delete of an issue or a document, whose foreign keys cascade into both
-- result tables and set pull_request_heads.issue_key to null, in an order of their own; an install
-- that held one lock while it waited for the next could close a cycle with such a delete, and
-- Postgres's deadlock check, which runs deadlock_timeout (1 s by default) into a wait, would then
-- refuse one of them: the delete, answered 500, or the install, whose 40P01 the runner does not
-- try again, failing the boot. So the install asks for the three in one statement, in one order,
-- waiting at most half a second (lock_timeout, shorter than deadlock_timeout), inside a block of
-- its own: a try that runs out gives back what it took, so a delete that waits for it goes on, and
-- the install asks again at once. It asks for as long as the runner lets any migration wait for a
-- lock (pgmigrate.LockTimeout, 5 s), and past that fails with 55P03, having applied nothing, and
-- the runner applies it again after a pause, up to its attempts (store/store.go applyMigration,
-- pgmigrate.RetryLockTimeouts). So a writer that holds one of the three for a while is waited out
-- as any migration waits one out, and a cycle with a delete costs the install half a second and
-- fails neither side. The seed reads the open rows and the result tables, and writes only the new
-- tables and check_states_owed. The census answers 0: nothing existing is refused or rewritten.
set local lock_timeout = '500ms';

create sequence pull_request_former_heads_order;
create sequence pull_request_former_heads_pull_ids as integer;

create table pull_request_former_heads_pulls (
    pull integer primary key default nextval('pull_request_former_heads_pull_ids'),
    number integer not null,
    ended bigint,
    generation integer not null default 1
);

create table pull_request_former_heads_aliases (
    repo text not null,
    number integer not null,
    pull integer not null,
    primary key (repo, number)
);
create index pull_request_former_heads_aliases_pull on pull_request_former_heads_aliases (pull);

create table pull_request_former_heads (
    pull integer not null,
    head_sha text not null,
    left_at timestamptz not null default now(),
    verified boolean not null default true,
    since bigint default nextval('pull_request_former_heads_order'),
    incarnation bigint not null default nextval('pull_request_former_heads_order'),
    primary key (pull, head_sha)
);

-- A row of pull_request_former_heads changed in any way is a new incarnation of it (above), by
-- whichever statement of the settler changes it.
create function pull_request_former_heads_incarnation() returns trigger language plpgsql as $$
begin
  new.incarnation := nextval('pull_request_former_heads_order');
  return new;
end $$;
create trigger pull_request_former_heads_incarnation before update on pull_request_former_heads
  for each row execute function pull_request_former_heads_incarnation();

create table pull_request_former_heads_moves (
    seq bigint not null default nextval('pull_request_former_heads_order'),
    moved_at timestamptz not null default now(),
    repo text not null,
    number integer not null,
    op text not null,
    old_head text,
    old_open boolean,
    new_head text,
    new_open boolean
);
create index pull_request_former_heads_moves_name on pull_request_former_heads_moves (number, repo, seq);

create table pull_request_former_heads_pending (
    issue_key text not null,
    repo text,
    number integer,
    commit_sha text,
    seq bigint not null default nextval('pull_request_former_heads_order')
);
create index pull_request_former_heads_pending_issue on pull_request_former_heads_pending (issue_key);

create table pull_request_former_heads_results (
    issue_key text not null,
    commit_sha text not null,
    place bigint not null,
    primary key (issue_key, commit_sha)
);

create table pull_request_former_heads_rows (
    pull integer not null,
    repo text not null,
    head_sha text not null,
    since bigint not null,
    primary key (pull, repo)
);

create table pull_request_former_heads_reads (
    pull integer primary key,
    until timestamptz not null,
    token bigint not null
);

-- The lock of a pull request's identity, to the end of the transaction, under which the settler
-- reads and writes its history (above). Every holder first takes the registry's lock, key 0 of
-- the namespace, which no identity holds, shared: pull_request_former_heads_resolve takes it alone
-- to make or join identities, so no identity is made or joined while a transaction holds the lock
-- of one, and no transaction that holds the lock of one waits for the registry.
create function pull_request_former_heads_lock(integer) returns void language sql as $$
  select pg_advisory_xact_lock_shared(hashtext('pull-request-former-heads'), 0);
  select pg_advisory_xact_lock(hashtext('pull-request-former-heads'), $1)
$$;

-- The identities of the names the settler has met that stand for the pull request a name and a
-- number stand for today: that name's, and each of a name of the same number the rename rule
-- makes the same repository, least first. It reads only.
create function pull_request_former_heads_named(text, integer) returns integer[] language sql stable as $$
  select coalesce(array_agg(distinct alias.pull order by alias.pull), '{}') from pull_request_former_heads_aliases alias
  where alias.number = $2 and (alias.repo = lower($1)
    or lower(pull_request_canonical_repo(alias.repo)) = lower(pull_request_canonical_repo($1)))
$$;

-- The identity a name and a number stand for today, null where the settler has met none of its
-- names: the least of pull_request_former_heads_named. It reads only, so a read of former heads
-- calls it; the settler first finds each name it applies (pull_request_former_heads_resolve).
create function pull_request_former_heads_pull_of(text, integer) returns integer language sql stable as $$
  select (pull_request_former_heads_named($1, $2))[1]
$$;

-- Joins identity $2 into $1: $2's names, rows of history and rows of the settler's state become
-- $1's, the later of their ends stands, and every row of history reached before it goes, each
-- left again where a result there was recorded after that end (pull_request_former_heads_leave_again).
-- The joined pull request is of a generation after both, so no answer read about either before the
-- join applies to it. Each issue that holds an open row of it under any of its names now reads a
-- history and a lifecycle it has not settled, so the join marks each owed, but $3, the issue whose
-- drain joins them, null where a pass does (above). Called by pull_request_former_heads_resolve
-- alone, which holds the registry's lock alone, so no other transaction holds the lock of either
-- identity.
create function pull_request_former_heads_join(integer, integer, text) returns void language plpgsql as $$
declare
  last_end bigint;
  last_generation integer;
  gone text[];
begin
  update pull_request_former_heads_aliases set pull = $1 where pull = $2;
  insert into pull_request_former_heads (pull, head_sha, left_at, verified, since)
  select $1, former.head_sha, former.left_at, former.verified, former.since
  from pull_request_former_heads former where former.pull = $2
  on conflict (pull, head_sha) do update set
    left_at = greatest(pull_request_former_heads.left_at, excluded.left_at),
    verified = pull_request_former_heads.verified or excluded.verified,
    since = greatest(pull_request_former_heads.since, excluded.since);
  delete from pull_request_former_heads where pull = $2;
  update pull_request_former_heads_rows set pull = $1 where pull = $2;
  delete from pull_request_former_heads_reads where pull = $2;
  select greatest(kept.ended, joined.ended), greatest(kept.generation, joined.generation) + 1 into last_end, last_generation
  from pull_request_former_heads_pulls kept, pull_request_former_heads_pulls joined
  where kept.pull = $1 and joined.pull = $2;
  delete from pull_request_former_heads_pulls where pull = $2;
  update pull_request_former_heads_pulls set ended = last_end, generation = last_generation where pull = $1;
  if last_end is not null then
    with ended as (
      delete from pull_request_former_heads former where former.pull = $1 and (former.since is null or former.since < last_end)
      returning former.head_sha
    ) select coalesce(array_agg(ended.head_sha), '{}') into gone from ended;
    perform pull_request_former_heads_leave_again($1, gone);
  end if;
  perform check_states_owe(holder.issue_key) from (
    select distinct head.issue_key from pull_request_heads head
    join pull_request_former_heads_aliases alias on alias.repo = lower(head.repo) and alias.number = head.number
    where alias.pull = $1 and head.state = 'open' and head.issue_key is not null and head.issue_key is distinct from $3
  ) holder;
end $$;

-- The identity of the pull request a name and a number stand for today (above), which it makes
-- where the settler has met none of its names, joins where it has met two, and adds the name to.
-- Where it must write, it does so under the registry's lock alone, read again under it; its
-- caller holds no pull request's lock yet, nor the registry's shared. $3 is the issue whose drain
-- resolves it, which a join owes nothing (pull_request_former_heads_join), null for any other
-- caller.
create function pull_request_former_heads_resolve(text, integer, text default null) returns integer language plpgsql as $$
declare
  known integer[] := pull_request_former_heads_named($1, $2);
  kept integer;
  other integer;
begin
  if cardinality(known) = 1 and exists (
    select 1 from pull_request_former_heads_aliases alias where alias.repo = lower($1) and alias.number = $2
  ) then
    return known[1];
  end if;
  perform pg_advisory_xact_lock(hashtext('pull-request-former-heads'), 0);
  known := pull_request_former_heads_named($1, $2);
  if cardinality(known) = 0 then
    insert into pull_request_former_heads_pulls (number) values ($2) returning pull into kept;
  else
    kept := known[1];
    foreach other in array known[2:] loop
      perform pull_request_former_heads_join(kept, other, $3);
    end loop;
  end if;
  insert into pull_request_former_heads_aliases (repo, number, pull) values (lower($1), $2, kept)
  on conflict (repo, number) do nothing;
  return kept;
end $$;

-- A write of pull_request_heads that can move a pull request's history (its triggers' when says
-- which): one record of it under the name of the row written, with its head and state before and
-- after; an update that keys the row anew is two, the delete of the row under its old name and the
-- insert of it under its new one. The delete of an open row whose repository the rename rule
-- names otherwise, the re-key, marks owed the issue of the open row of the canonical name, which
-- may be written with the head, state and issue it already holds and so mark nothing, so the
-- settle that publishes the head the delete keeps comes soon. It reads and appends, and marks an
-- issue owed: it waits for no lock.
create function pull_request_former_heads_keep() returns trigger language plpgsql as $$
declare
  canonical text;
begin
  if tg_op = 'DELETE' or (tg_op = 'UPDATE' and (old.repo, old.number) is distinct from (new.repo, new.number)) then
    if old.state = 'open' and pull_request_canonical_repo(old.repo) <> old.repo then
      select head.issue_key into canonical from pull_request_heads head
      where head.repo = pull_request_canonical_repo(old.repo) and head.number = old.number and head.state = 'open';
      if canonical is not null then
        perform check_states_owe(canonical);
      end if;
    end if;
    insert into pull_request_former_heads_moves (repo, number, op, old_head, old_open)
    values (lower(old.repo), old.number, 'DELETE', old.head_sha, old.state = 'open');
    if tg_op = 'UPDATE' and new.state = 'open' then
      insert into pull_request_former_heads_moves (repo, number, op, new_head, new_open)
      values (lower(new.repo), new.number, 'INSERT', new.head_sha, true);
    elsif tg_op = 'UPDATE' and old.state = 'open' then
      -- An open row keyed anew closed or merged: the close, under the new name.
      insert into pull_request_former_heads_moves (repo, number, op, old_open, new_head, new_open)
      values (lower(new.repo), new.number, 'INSERT', true, new.head_sha, false);
    end if;
    return null;
  end if;
  if tg_op = 'INSERT' then
    insert into pull_request_former_heads_moves (repo, number, op, new_head, new_open)
    values (lower(new.repo), new.number, 'INSERT', new.head_sha, new.state = 'open');
    return null;
  end if;
  if old.state <> 'open' and new.state <> 'open' then
    return null;
  end if;
  insert into pull_request_former_heads_moves (repo, number, op, old_head, old_open, new_head, new_open)
  values (lower(old.repo), old.number, 'UPDATE', old.head_sha, old.state = 'open', new.head_sha, new.state = 'open');
  return null;
end $$;

-- Applies the records of the pull request of an identity, the records of each of its names, in the
-- order of their places, and deletes them (above); each close or merge it applies starts the next
-- generation. Each commit an end drops from the history, and each head an open row leaves whose
-- visit, reached before the last end, keeps nothing, it leaves again where a result there was
-- recorded after that end (pull_request_former_heads_leave_again), once the end is stored. Where
-- that changes the pull request's history, it marks owed, in the same transaction, each issue that
-- holds an open row of the pull request now, but $2, the issue whose settle applies it and computes
-- its states next: the issue a writer saw when it recorded the change may hold the pull request no
-- more, and the issue that holds it now may have settled before this application. A move of the
-- row to another issue that commits after this read marks that issue owed itself
-- (pull_request_former_heads_begin, check_states_owe_head), and that issue's drain waits for this
-- transaction's lock of the pull request, so it reads this history. The caller holds the pull
-- request's lock. Answers how many records it applied.
create function pull_request_former_heads_apply(integer, text) returns integer language plpgsql as $$
declare
  taken pull_request_former_heads_moves[];
  seqs bigint[];
  fact pull_request_former_heads_moves;
  last_end bigint;
  last_generation integer;
  entered bigint;
  changed boolean := false;
  dropped text[] := '{}';
  gone text[];
begin
  select coalesce(array_agg(applied order by applied.seq), '{}'), coalesce(array_agg(applied.seq), '{}')
  into taken, seqs
  from pull_request_former_heads_moves applied
  join pull_request_former_heads_aliases alias on alias.repo = applied.repo and alias.number = applied.number
  where alias.pull = $1;
  if cardinality(taken) = 0 then
    return 0;
  end if;
  delete from pull_request_former_heads_moves where seq = any(seqs);
  select pulls.ended, pulls.generation into last_end, last_generation from pull_request_former_heads_pulls pulls where pulls.pull = $1;
  foreach fact in array taken loop
    -- The place at which the row reached the head it leaves, null where no applied record shows it.
    entered := null;
    select reached.since into entered from pull_request_former_heads_rows reached
    where reached.pull = $1 and reached.repo = fact.repo and reached.head_sha = fact.old_head;
    if fact.old_open and (fact.op = 'DELETE' or (fact.new_open and fact.old_head <> fact.new_head)) then
      -- An open row that moves on from a head, or goes, keeps the head where it was reached in the
      -- lifecycle that has not ended.
      if last_end is null or entered > last_end then
        insert into pull_request_former_heads (pull, head_sha, left_at, verified, since)
        values ($1, fact.old_head, fact.moved_at, true, entered)
        on conflict (pull, head_sha) do update set left_at = greatest(pull_request_former_heads.left_at, excluded.left_at),
          verified = true, since = greatest(pull_request_former_heads.since, excluded.since);
        changed := true;
      else
        -- Reached in a lifecycle that ended: the visit keeps nothing, a result after the end may.
        dropped := dropped || fact.old_head;
      end if;
    elsif fact.op <> 'DELETE' and not fact.new_open and (fact.old_open or (fact.old_open is null and (last_end is null
        or exists (select 1 from pull_request_former_heads_rows open_row where open_row.pull = $1)))) then
      -- A close or a merge: an open row that closes, or a row written closed or merged where the
      -- pull request is open under another name or no close of it is known, the install's record
      -- of a pull request closed when it ran among them. A row written closed while every row
      -- applied is closed and a close is known shows that close again, and ends nothing new, so
      -- a result recorded while the pull request was closed stays of the next lifecycle.
      last_end := greatest(last_end, fact.seq);
      last_generation := last_generation + 1;
      with ended as (
        delete from pull_request_former_heads former where former.pull = $1 and (former.since is null or former.since < last_end)
        returning former.head_sha
      ) select coalesce(array_agg(ended.head_sha), '{}') into gone from ended;
      dropped := dropped || gone;
      changed := changed or cardinality(gone) > 0;
    end if;
    if fact.op = 'DELETE' or not fact.new_open then
      delete from pull_request_former_heads_rows where pull = $1 and repo = fact.repo;
    elsif fact.op = 'INSERT' or not fact.old_open or fact.old_head <> fact.new_head then
      insert into pull_request_former_heads_rows (pull, repo, head_sha, since) values ($1, fact.repo, fact.new_head, fact.seq)
      on conflict (pull, repo) do update set head_sha = excluded.head_sha, since = excluded.since;
    end if;
  end loop;
  update pull_request_former_heads_pulls set ended = last_end, generation = last_generation where pull = $1;
  if pull_request_former_heads_leave_again($1, dropped) then
    changed := true;
  end if;
  if changed then
    perform check_states_owe(holder.issue_key) from (
      select distinct head.issue_key from pull_request_heads head
      join pull_request_former_heads_aliases alias on alias.repo = lower(head.repo) and alias.number = head.number
      where alias.pull = $1 and head.state = 'open' and head.issue_key is not null and head.issue_key is distinct from $2
    ) holder;
  end if;
  return cardinality(taken);
end $$;

-- Applies the records of the pull request a name and a number stand for, and finds first the
-- identity of every name a record of that number was written under, so a record written under a
-- name before Dispatch learned of a rename is applied with the rest (above). It reads the identity
-- again once it holds the registry's lock shared, when no identity is joined any more. Answers
-- how many it applied.
create function pull_request_former_heads_fold(text, integer) returns integer language plpgsql as $$
declare
  named text;
  pull integer;
begin
  for named in select distinct moves.repo from pull_request_former_heads_moves moves where moves.number = $2 loop
    perform pull_request_former_heads_resolve(named, $2);
  end loop;
  perform pull_request_former_heads_resolve($1, $2);
  perform pg_advisory_xact_lock_shared(hashtext('pull-request-former-heads'), 0);
  pull := pull_request_former_heads_pull_of($1, $2);
  perform pull_request_former_heads_lock(pull);
  return pull_request_former_heads_apply(pull, null);
end $$;

-- The issue whose settle reads the results recorded against a check of a document: the issue
-- the document is the Verification document of, by the role predicate the server reads it by
-- (above), and null for any other document. The one statement of that rule here: the population
-- below and the result's trigger both read it.
create function pull_request_former_heads_reader(uuid) returns text language sql stable as $$
  select document.issue_key from artifacts document
  where document.id = $1
    and (document.role = 'verification' or (document.role is null and document.issue_key is not null and document.slug = 'verification-md'))
$$;

-- The population of an issue's candidates: each distinct commit of a result, a role's or a CI
-- job's, whose document's results the settle of the issue reads (pull_request_former_heads_reader),
-- whichever issue recorded it.
create function pull_request_former_heads_population(text) returns setof text language sql stable as $$
  select result.commit_sha from artifacts document
  join check_results result on result.check_artifact_id = document.id
  where document.issue_key = $1 and pull_request_former_heads_reader(document.id) = $1
  union
  select result.commit_sha from artifacts document
  join ci_check_results result on result.check_artifact_id = document.id
  where document.issue_key = $1 and pull_request_former_heads_reader(document.id) = $1
$$;

-- Leaves each of the commits given as a candidate of the pull request of an identity, where a row
-- of it is open at the issue given and no open row of it stands at the commit, unverified, where it
-- holds no row of the commit yet, and only where the commit's result is of the lifecycle that has
-- not ended: its place (pull_request_former_heads_results, of the issue given) is after the pull
-- request's last end, or, where no result there has a place yet, no end is known (above); a result
-- recorded before this release has the place the install gave it. The candidate's since is that
-- place. A commit the history holds a row of keeps that row and its since, and the result's place
-- stays in pull_request_former_heads_results, from which an end that drops the row leaves the
-- commit again (pull_request_former_heads_leave_again).
-- Answers whether it left one. It takes no lock itself: the install holds the tables, and the drain
-- takes the pull request's lock first.
create function pull_request_former_heads_leave(integer, text, text[]) returns boolean language plpgsql as $$
begin
  insert into pull_request_former_heads (pull, head_sha, verified, since)
  select $1, candidate.commit_sha, false, placed.place
  from unnest($3) as candidate (commit_sha)
  join pull_request_former_heads_pulls pulls on pulls.pull = $1
  left join pull_request_former_heads_results placed on placed.issue_key = $2 and placed.commit_sha = candidate.commit_sha
  where (pulls.ended is null or placed.place > pulls.ended)
    and exists (
      select 1 from pull_request_heads head
      join pull_request_former_heads_aliases alias on alias.repo = lower(head.repo) and alias.number = head.number
      where alias.pull = $1 and head.issue_key = $2 and head.state = 'open')
    and not exists (
      select 1 from pull_request_heads head
      join pull_request_former_heads_aliases alias on alias.repo = lower(head.repo) and alias.number = head.number
      where alias.pull = $1 and head.state = 'open' and head.head_sha = candidate.commit_sha)
  on conflict (pull, head_sha) do nothing;
  return found;
end $$;

-- Leaves again, by pull_request_former_heads_leave, for each issue an open row of the pull request
-- of an identity is at, each commit given: a row of history an end of its lifecycle just dropped,
-- or a head an open row of it left whose visit, reached before the end, keeps nothing. A row of
-- history holds one since, the place of what put it there first, and a result recorded at its
-- commit later leaves no row of its own, its pending row taken; so where the end that drops the
-- row is applied after that result, as when the close commits late, the result's place, kept in
-- pull_request_former_heads_results, must leave the commit again, as it would have, had the end
-- been applied first; and a result recorded while the commit was an open head, which left nothing
-- then, leaves it once the head is left. Each issue's own results decide, after the end stored in
-- pull_request_former_heads_pulls, so the history is the same whatever order the end, the result
-- and the move are applied in. Its caller holds the pull request's lock, or the registry's alone.
-- Answers whether it left one.
create function pull_request_former_heads_leave_again(integer, text[]) returns boolean language plpgsql as $$
declare
  holder text;
  commits text[] := array(select distinct dropped.commit_sha from unnest($2) as dropped (commit_sha));
  left_one boolean := false;
begin
  if cardinality(commits) = 0 then
    return false;
  end if;
  for holder in
    select distinct head.issue_key from pull_request_heads head
    join pull_request_former_heads_aliases alias on alias.repo = lower(head.repo) and alias.number = head.number
    where alias.pull = $1 and head.state = 'open' and head.issue_key is not null
  loop
    if pull_request_former_heads_leave($1, holder, commits) then
      left_one := true;
    end if;
  end loop;
  return left_one;
end $$;

-- The snapshot of the pull request of an identity that GitHub's answer about its commits depends
-- on: each open row of it, by name and number, with its head, its base branch and the commit the
-- base branch was at when the row was last read (pull_request_heads.base_sha, migration
-- 20261125005350), in one order. The base branch can move with no change of its name, and the
-- answer is about one base revision, so the snapshot names the revision, and "unknown" where the
-- row holds none, a row a writer of the release before this one wrote, which is not read until a
-- write stores one: the reconcile's read of the pull request, which of n rows due in its first
-- selection, the open rows due and the merged rows no read has reached (the merged rows of an
-- older attribution rule take only the room those leave, at most 50 a pass), tries each within
-- ceil(n / delivery.PullRequestHeadBatch) passes that try their whole batch, each stamp
-- acknowledged, whatever their reads answer, a row never tried first; a pass a rate limit stops
-- leaves the rows it did not try in their places, so the next pass takes them first
-- (delivery.ListStalePullRequestHeads); a try whose stamp's call times out or loses its connection
-- may still commit the stamp with no read, at a moment the reconcile never learns, so the bound
-- counts no pass with such a stamp and holds again only once the stamp has settled, with no bound
-- while such stamps recur, and the stamp stores no revision. It reads only.
create function pull_request_former_heads_snapshot(integer) returns text language sql stable as $$
  select coalesce(string_agg(lower(head.repo) || '#' || head.number || '@' || head.head_sha || ' onto ' || head.base_ref
    || ' at ' || coalesce(head.base_sha, 'unknown'), ', ' order by lower(head.repo), head.number), '')
  from pull_request_heads head
  join pull_request_former_heads_aliases alias on alias.repo = lower(head.repo) and alias.number = head.number
  where alias.pull = $1 and head.state = 'open'
$$;

-- Claims the read of the candidates of the pull request of an identity for $2 seconds, where no
-- live claim holds it, and answers what that read is about (above): the claim's token, the pull
-- request's generation and snapshot, the head and the base revision ("" where none is stored) of
-- its open row named $3 and numbered $4, the row the read goes by, read in the statement that
-- reads the snapshot, and each candidate with its incarnation, in commit order. It answers no row
-- where another claim holds the read. The claim commits with the caller's statement, so a settle
-- of another issue sees it at once.
create function pull_request_former_heads_claim(integer, double precision, text, integer)
returns table (token bigint, generation integer, snapshot text, head text, base text, candidates text[], incarnations bigint[])
language plpgsql as $$
#variable_conflict use_column
declare
  taken bigint;
begin
  insert into pull_request_former_heads_reads as claim (pull, until, token)
  values ($1, now() + make_interval(secs => $2), nextval('pull_request_former_heads_order'))
  on conflict (pull) do update set until = excluded.until, token = excluded.token
  where claim.until <= now()
  returning claim.token into taken;
  if taken is null then
    return;
  end if;
  return query
  select taken, pulls.generation, pull_request_former_heads_snapshot($1),
    coalesce((select named.head_sha from pull_request_heads named where named.repo = $3 and named.number = $4 and named.state = 'open'), ''),
    coalesce((select named.base_sha from pull_request_heads named where named.repo = $3 and named.number = $4 and named.state = 'open'), ''),
    coalesce(array_agg(former.head_sha order by former.head_sha) filter (where former.head_sha is not null), '{}'),
    coalesce(array_agg(former.incarnation order by former.head_sha) filter (where former.head_sha is not null), '{}')
  from pull_request_former_heads_pulls pulls
  left join pull_request_former_heads former on former.pull = pulls.pull and not former.verified
  where pulls.pull = $1
  group by pulls.generation;
end $$;

-- The one application of GitHub's answer about the candidates of the pull request of an identity
-- (above): $2 the claim's token, $3 the generation and $4 the snapshot the claim read, $5 and $6
-- each member and the incarnation the claim read it at, $7 and $8 each commit GitHub showed is not
-- one and its incarnation, and $9 the issue settling. The answer is GitHub's comparison of the head
-- and the base revision the claim read with the snapshot (api/check_states.go, verifyFormerHeads),
-- so the snapshot unchanged shows that the row the read went by still stores the two commits the
-- answer is about. Under the pull request's lock it gives the claim back by its token, and applies
-- only where that claim was still live and the generation and the snapshot are still the pull
-- request's. It marks each member verified and deletes each commit shown outside, each only where
-- it is still a candidate of the incarnation read, and marks owed each other issue an open row of
-- the pull request is at where a candidate became a former head. Answers whether it applied, and
-- how many candidates it kept, dropped and issues it marked owed.
create function pull_request_former_heads_answer(integer, bigint, integer, text, text[], bigint[], text[], bigint[], text)
returns table (applied boolean, kept integer, dropped integer, owed integer)
language plpgsql as $$
#variable_conflict use_column
declare
  live boolean;
  standing integer;
  kept_count integer := 0;
  dropped_count integer := 0;
  owed_count integer := 0;
begin
  perform pull_request_former_heads_lock($1);
  select exists (select 1 from pull_request_former_heads_reads claim where claim.pull = $1 and claim.token = $2 and claim.until > now())
  into live;
  delete from pull_request_former_heads_reads claim where claim.pull = $1 and claim.token = $2;
  select pulls.generation into standing from pull_request_former_heads_pulls pulls where pulls.pull = $1;
  if not live or standing is distinct from $3 or pull_request_former_heads_snapshot($1) <> $4 then
    return query select false, 0, 0, 0;
    return;
  end if;
  with confirmed as (
    update pull_request_former_heads former set verified = true
    from unnest($5, $6) as member (head_sha, incarnation)
    where former.pull = $1 and not former.verified and former.head_sha = member.head_sha and former.incarnation = member.incarnation
    returning 1
  ) select count(*) into kept_count from confirmed;
  with refuted as (
    delete from pull_request_former_heads former
    using unnest($7, $8) as outsider (head_sha, incarnation)
    where former.pull = $1 and not former.verified and former.head_sha = outsider.head_sha and former.incarnation = outsider.incarnation
    returning 1
  ) select count(*) into dropped_count from refuted;
  if kept_count > 0 then
    with owing as (
      select check_states_owe(holder.issue_key) from (
        select distinct head.issue_key from pull_request_heads head
        join pull_request_former_heads_aliases alias on alias.repo = lower(head.repo) and alias.number = head.number
        where alias.pull = $1 and head.state = 'open' and head.issue_key <> $9
      ) holder
    ) select count(*) into owed_count from owing;
  end if;
  return query select true, kept_count, dropped_count, owed_count;
end $$;

-- A result recorded, a role's or a CI job's, whose settle an issue reads: its commit is pending
-- for that issue, which the settle drains (pull_request_former_heads_drain). It reads and appends:
-- it waits for no lock.
create function pull_request_former_heads_of_result() returns trigger language plpgsql as $$
declare
  owner text := pull_request_former_heads_reader(new.check_artifact_id);
begin
  if owner is not null then
    insert into pull_request_former_heads_pending (issue_key, commit_sha) values (owner, new.commit_sha);
    if owner is distinct from new.check_issue_key then
      perform check_states_owe(owner);
    end if;
  end if;
  return null;
end $$;

-- A row that starts an open lifecycle at an issue (its triggers' when says which): the row is
-- pending for that issue, which the settle drains, and the issue is owed that settle. It appends:
-- it waits for no lock.
create function pull_request_former_heads_begin() returns trigger language plpgsql as $$
begin
  insert into pull_request_former_heads_pending (issue_key, repo, number) values (new.issue_key, new.repo, new.number);
  perform check_states_owe(new.issue_key);
  return null;
end $$;

-- What a settle of an issue runs before it reads former heads (api/check_states.go, settleRound):
-- it takes the issue's pending rows and keeps the latest place of each pending result's commit
-- (pull_request_former_heads_results), whether or not a pull request of the issue is open now, so
-- a result recorded while its pull request was closed counts once the pull request reopens; finds
-- the identity of each pull request an open row of the issue stands for, and of every name a
-- record of their numbers was written under, before it takes any lock of a pull request; then,
-- holding the registry's lock shared, so no identity is joined any more, reads those identities
-- again and, for each in the order of the identities, under its lock, applies its records and
-- leaves the commits of the pending results, and the issue's whole population where a row of it
-- is pending as started. Answers how many pending rows it took.
create function pull_request_former_heads_drain(text) returns integer language plpgsql as $$
declare
  taken integer;
  results text[];
  starts text[];
  head record;
  named text;
  held integer;
begin
  with pending as (
    delete from pull_request_former_heads_pending where issue_key = $1 returning repo, number, commit_sha, seq
  ), placed as (
    insert into pull_request_former_heads_results (issue_key, commit_sha, place)
    select $1, pending.commit_sha, max(pending.seq) from pending where pending.commit_sha is not null group by pending.commit_sha
    on conflict (issue_key, commit_sha) do update set place = greatest(pull_request_former_heads_results.place, excluded.place)
  )
  select count(*), coalesce(array_agg(distinct commit_sha) filter (where commit_sha is not null), '{}'),
    coalesce(array_agg(distinct lower(repo) || '#' || number) filter (where commit_sha is null), '{}')
  into taken, results, starts from pending;
  for head in
    select distinct stored.repo, stored.number from pull_request_heads stored where stored.issue_key = $1 and stored.state = 'open'
  loop
    for named in select distinct moves.repo from pull_request_former_heads_moves moves where moves.number = head.number loop
      perform pull_request_former_heads_resolve(named, head.number, $1);
    end loop;
    perform pull_request_former_heads_resolve(head.repo, head.number, $1);
  end loop;
  perform pg_advisory_xact_lock_shared(hashtext('pull-request-former-heads'), 0);
  for held in
    select distinct pull_request_former_heads_pull_of(stored.repo, stored.number) from pull_request_heads stored
    where stored.issue_key = $1 and stored.state = 'open' order by 1
  loop
    perform pull_request_former_heads_lock(held);
    perform pull_request_former_heads_apply(held, $1);
    if taken > 0 then
      perform pull_request_former_heads_leave(held, $1, results ||
        case when exists (
          select 1 from pull_request_former_heads_aliases alias where alias.pull = held and alias.repo || '#' || alias.number = any(starts)
        ) then array(select pull_request_former_heads_population($1)) else '{}'::text[] end);
    end if;
  end loop;
  return taken;
end $$;

-- The three tables' locks, by tries of half a second each for at most five seconds (above): the
-- block's exception handler rolls a try that ran out back, which gives back the locks it took.
do $$
declare
  started timestamptz := clock_timestamp();
begin
  loop
    begin
      lock table pull_request_heads, check_results, ci_check_results in share row exclusive mode;
      exit;
    exception when lock_not_available then
      if clock_timestamp() - started >= interval '5 seconds' then
        raise;
      end if;
    end;
  end loop;
end $$;

create trigger pull_request_former_heads_begin_insert after insert on pull_request_heads
  for each row when (new.state = 'open' and new.issue_key is not null)
  execute function pull_request_former_heads_begin();
create trigger pull_request_former_heads_begin_update after update on pull_request_heads
  for each row when (new.state = 'open' and new.issue_key is not null and (old.state <> 'open'
    or (old.repo, old.number) is distinct from (new.repo, new.number) or old.issue_key is distinct from new.issue_key))
  execute function pull_request_former_heads_begin();
create trigger pull_request_former_heads_keep_insert after insert on pull_request_heads
  for each row execute function pull_request_former_heads_keep();
create trigger pull_request_former_heads_keep_update after update on pull_request_heads
  for each row when ((old.repo, old.number, old.head_sha, old.state) is distinct from (new.repo, new.number, new.head_sha, new.state))
  execute function pull_request_former_heads_keep();
create trigger pull_request_former_heads_keep_delete after delete on pull_request_heads
  for each row execute function pull_request_former_heads_keep();
create trigger pull_request_former_heads_of_check_results after insert on check_results
  for each row execute function pull_request_former_heads_of_result();
create trigger pull_request_former_heads_of_ci_check_results after insert on ci_check_results
  for each row execute function pull_request_former_heads_of_result();

-- The install's places: each result recorded before it, for each issue and commit of the issue's
-- population (pull_request_former_heads_results, the latest result there), and the end of each
-- pull request's last lifecycle before it, in the order of Dispatch's own times, since no record
-- of this migration's tables orders them. Without an end before the install, a pull request that
-- reopens after it, or that reopened before it, would take the results recorded before that end
-- as its own, a pass from before the close among them, which the close forbids.
--
-- The end is where the pull request's last closed period began, as Dispatch's record of each
-- write that opened or closed a row of it shows (pull_request_heads_lifecycle, migration
-- 20261125005349), which this install is the one reader of: the migration after it,
-- 20261125005353, drops the record, its triggers and its function. The writes are those of every
-- name the rename rule maps to the pull request at its canonical name, in the order of their
-- times. Each write that opened a row starts a new period; the last closed period is the closes
-- after the last such write before the last close. It begins at its first close, or, where a later
-- close in it closed a row that was open, as a pull request open under another name can, at the
-- latest of those, since a row of the pull request was open until then.
-- Any other write in that period, a row of the pull request written closed while it was closed
-- already, such as the canonical-name writer's first write of a pull request it reads closed,
-- which then deletes the row of its old name, is the closed pull request written again, and moves
-- the end nowhere; and a write that leaves a closed row closed or merged (an edit, a read that
-- finds it closed still, a rewrite as merged, a re-key of a closed row) is in no record. So a
-- close, a reopen and a second close end the lifecycle at the second close, and no write of the
-- closed pull request moves the end past a result recorded in its closed period. That record
-- keeps every write whatever writes follow it, a reopen, a re-key or a delete of the row, so a
-- pull request a writer of the release before this one closed and reopened between that migration
-- and this one, which this one finds open, ends its lifecycle here as one closed now does.
--
-- A closed period whose first close is undated, the record's of a pull request closed when
-- migration 20261125005349 ran, began before that migration by a span no record of Dispatch's
-- shows. Of such a pull request every result recorded before the install stays out after a
-- reopen, however long after that migration it was recorded, so that end goes after every result
-- the install finds; so does the end of a pull request closed now whose last recorded write opened
-- a row, whose close is in no record, and of one closed now the record holds nothing of, which only
-- a rename learned since that migration makes. Taken the other way, a result from before the close
-- could carry its pass across the reopen, which is the defect the close exists to prevent. The
-- cost is liveness alone: the head such a result was recorded at gets no state, and Legion waits
-- there until a tester records again at a head it tests. A result recorded after the install is
-- placed after the end all the same.
--
-- The times are of one clock, the database's: each result's at the time Dispatch recorded it
-- (created_at), each write's at the time the record took it. A result goes first where the two
-- times are equal, so a result counts after a reopen only where it was recorded after the end. No
-- time GitHub gave (pull_request_heads.event_at) orders anything here: it is of another clock, a
-- later edit of the closed pull request moves it, and a write that names none keeps an earlier
-- one, so it can place a result from before the close after it. The other costs: a result recorded
-- between GitHub's close and Dispatch's write of it stays out, and the head a pull request reopened
-- before the install stands at, once it moves on, keeps no visit of its own, since no record shows
-- when it was reached. A close and a reopen both before migration 20261125005349 are in no record
-- of Dispatch's, as every pull request's history before this release is not, and the seed below
-- takes its results as it finds them.
--
-- Each place is drawn from pull_request_former_heads_order in that order, before any place a
-- record after the install draws. The end is one record of a row written closed for each pull
-- request, under its canonical name, which the settler applies as any other
-- (pull_request_former_heads_apply): it ends the lifecycle at its place, so a result placed after
-- it is of the next lifecycle and one placed before it is not (pull_request_former_heads_leave),
-- whichever side of the install it was recorded on. A pull request is closed now by the rule
-- migration 20261125005349 records the closed ones by: where the row of its canonical name is not
-- open, or, with no row of that name, where no row of it is open, so an open pull request whose
-- old name's row reads closed from before that migration keeps the results it holds. The places
-- come before the seed, so a candidate the seed leaves holds its result's place as its since, and
-- the settler's first application of the pull request's records, before any read of its former
-- heads, drops each candidate the end's place comes after.
do $$
declare
  dated record;
begin
  for dated in
    with named as materialized (
      select lower(head.repo) as repo, head.number, head.state <> 'open' as closed,
        lower(pull_request_canonical_repo(head.repo)) as pull_repo
      from pull_request_heads head
    ), writes as materialized (
      select lower(pull_request_canonical_repo(noted.repo)) as pull_repo, noted.number, noted.open, noted.from_open,
        noted.at, noted.seq
      from pull_request_heads_lifecycle noted
    ), periods as (
      -- Each write's period: how many writes that opened a row of its pull request came before it
      -- or are it; and how many there are in all.
      select writes.*,
        count(*) filter (where writes.open) over (partition by writes.pull_repo, writes.number
          order by writes.at nulls first, writes.seq) as period,
        count(*) filter (where writes.open) over (partition by writes.pull_repo, writes.number) as opens
      from writes
    ), closes as (
      -- Each close, its order in its period, and the last period that holds a close.
      select periods.*,
        row_number() over (partition by periods.pull_repo, periods.number, periods.period
          order by periods.at nulls first, periods.seq) as nth,
        max(periods.period) over (partition by periods.pull_repo, periods.number) as last_closed
      from periods where not periods.open
    ), recorded as (
      select closes.pull_repo, closes.number,
        max(coalesce(closes.at, 'infinity')) filter (where closes.period = closes.last_closed and (closes.nth = 1 or closes.from_open)) as at,
        bool_or(closes.opens > closes.last_closed) as reopened
      from closes
      group by closes.pull_repo, closes.number
    )
    select history.issue_key, history.commit_sha, history.repo, history.number
    from (
      select document.issue_key, result.commit_sha, null::text as repo, null::integer as number,
        max(result.created_at) as at, 0 as closes
      from artifacts document
      join (
        select recorded.check_artifact_id, recorded.commit_sha, recorded.created_at from check_results recorded
        union all
        select recorded.check_artifact_id, recorded.commit_sha, recorded.created_at from ci_check_results recorded
      ) result on result.check_artifact_id = document.id
      where pull_request_former_heads_reader(document.id) = document.issue_key
      group by document.issue_key, result.commit_sha
      union all
      select null, null, coalesce(closed.pull_repo, recorded.pull_repo), coalesce(closed.number, recorded.number),
        case when closed.pull_repo is not null and (recorded.at is null or recorded.reopened) then 'infinity' else recorded.at end, 1
      from (
        select distinct head.pull_repo, head.number from named head
        where head.closed and not exists (
          select 1 from named other
          where other.number = head.number and other.pull_repo = head.pull_repo and not other.closed
            and (other.repo = other.pull_repo or head.repo <> head.pull_repo))
      ) closed
      full join recorded on recorded.pull_repo = closed.pull_repo and recorded.number = closed.number
    ) history
    order by history.at, history.closes
  loop
    if dated.commit_sha is not null then
      insert into pull_request_former_heads_results (issue_key, commit_sha, place)
      values (dated.issue_key, dated.commit_sha, nextval('pull_request_former_heads_order'));
    else
      insert into pull_request_former_heads_moves (repo, number, op, new_open)
      values (dated.repo, dated.number, 'INSERT', false);
    end if;
  end loop;
end $$;

-- The seed: each open row attributed to an issue starts with the issue's population, by the
-- function the drain of a row's start calls, and each issue holding a candidate is owed.
select pull_request_former_heads_leave(pull_request_former_heads_resolve(head.repo, head.number), head.issue_key,
  array(select pull_request_former_heads_population(head.issue_key)))
from pull_request_heads head
where head.state = 'open' and head.issue_key is not null;

select check_states_owe(owed.issue_key)
from (
  select distinct head.issue_key from pull_request_heads head
  join pull_request_former_heads_aliases alias on alias.repo = lower(head.repo) and alias.number = head.number
  join pull_request_former_heads former on former.pull = alias.pull
  where head.state = 'open' and not former.verified
) owed;
