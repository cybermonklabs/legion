-- 20261125053125_user_agent_thread_dismissed_root.up.sql
--
-- INFRA-1152: the one writer of messages.agent_dismissed_at (20261125052902), the mark of an Agent
-- dismissal on the root it dismisses. A trigger after each insert, update and delete of a
-- user_agent_thread_dismissed row mirrors the row onto its root, so every write of a mark moves the
-- column in the statement that writes the mark, whichever build wrote it and through whichever
-- path: the author's dismissal (api/inbox_dismiss.go's inboxDismissMarkAgentThreads, an upsert that
-- inserts the mark or moves it later), the restore (inboxRestoreAgentThreadRows, a delete), and a
-- session's reply that brings the thread back (inboxwrite's queueAgentDismissalsClear in this
-- release, the trigger user_agent_thread_dismissed_clear for a reply of the release before, each a
-- delete of every mark of the root), each of them as the release before writes it too while it
-- serves beside this one. The one exception is a delete that finds the root held by another
-- transaction, which leaves the column standing and owes it instead (below).
--
-- The column is the author's mark alone: a row moves its root's column only where the root is a
-- user's message whose login, lowered as the Inbox reads it (myAgentRootFilter), is the row's, so
-- a mark naming a message of somebody else's, which no route writes, moves nothing. An insert or
-- an update sets the column to the row's dismissed_at; a delete, or an update that moves the row
-- off its root or its login, clears that root's column. A write that leaves the column as it
-- stands writes no row of messages.
--
-- Every writer of a root's column in this release takes the root's row first, `for no key update`
-- or stronger, and the mark after it (the lock order written on api/inbox_read.go's
-- markInboxReadThreads): the dismissal takes the roots `for update` (inboxDismissLockAgentRoots),
-- the restore `for no key update` (inboxRestoreLockAgentRoots), and a session's reply takes its
-- root `for no key update` before its insert (inboxwrite's Recording.QueueRootLock). A reply of the
-- release before reaches the marks through user_agent_thread_dismissed_clear, whose body this
-- migration replaces with the one 20261116160001 wrote plus two steps: where a mark of the root or
-- an owed row of it stands, it takes the root `for no key update` before its delete, beside the
-- `for key share` it always takes, so a reply into a thread nobody dismissed takes the root as it
-- did and two sessions replying there still do not queue behind each other; and after the delete
-- it settles the root (below). The read of the marks and the owed rows in that step is a
-- statement of its own, after the root is held `for key share`, so it sees every mark committed
-- while the reply waited there, and none can be written behind it: a dismissal writes a mark only
-- under the root's `for update`, which the reply's `for key share` holds off.
--
-- The restore of the release before takes no root (its api/inbox_dismiss.go: "A restore takes no
-- root"): it locks the caller's marks and deletes them, so the trigger's delete reaches the root
-- while the restore holds the mark, the reverse of every writer above. A trigger that waited there
-- for a reply holding the root, while the reply waited for that mark, would close a cycle Postgres
-- breaks by failing one of the two (40P01): a restore that answers 500, or a session's reply lost.
-- So a delete of a mark never waits at its root. Where the row is the root's author's mark and the
-- column is set, both read with no lock, the trigger takes the root `for no key update skip
-- locked`, with no NOWAIT and no exception block, which would make a subtransaction of every
-- thread a restore of many deletes. Where it gets the root, which is every delete a writer of this
-- release makes, since each holds the root already, it clears the column as above and deletes the
-- root's owed rows. Where another transaction holds the root, it appends one row of
--
--   user_agent_thread_dismissed_owed   a root whose column may stand set with no mark of its author
--                                      behind it: root_id, the root, and owed_at, the moment the
--                                      delete found the root held. The Inbox reads such a root as
--                                      live (api/replies_agent.go, myAgentOwedRoot).
--
-- and returns, leaving the column for the writer that holds the root. A write of a mark waits at
-- nothing either: every writer of a mark holds its root first, so the update the trigger makes
-- there is of a row its own transaction holds, and once that update has set the column it deletes
-- the root's owed rows.
--
-- The table has no unique key on root_id, so an insert never waits on a concurrent insert or
-- delete of a row of the same root: the delete that appends one holds a mark, and a wait there is
-- that cycle again. Two deletes that both found the root held append a row each. It has no foreign
-- key to messages either: the key share such a key takes on the root would conflict with a
-- dismissal's `for update` of that root while the inserter holds a mark, the same cycle again. A
-- root that no longer exists leaves its rows to the sweep (below).
--
-- The settle, user_agent_thread_dismissed_settle, is one root's catch-up: under the root's lock,
-- which its caller holds `for no key update` or stronger, it sets the root's column from its
-- author's mark as it now stands, null where none, and then deletes the root's owed rows. The owed
-- rows of a root that exists are deleted only after their deleter has touched the marks, never
-- before: every writer that holds a root runs the settle after its own delete or write of the
-- marks, as the trigger deletes them after the delete or write it runs on. That delete or write is
-- where the writer waits for a restore of the release before holding a mark of the root, which has
-- committed its owed row by the time the wait ends, and each statement of the settle reads with a
-- snapshot of its own, so it reads that row and the mark as they now stand. It runs in the clear
-- trigger above, for a reply of the release before; in inboxwrite's queueAgentDismissalsClear, for
-- a reply of this release; in the restore route after its delete of the caller's marks, for the
-- roots it locked (api/inbox_dismiss.go, inboxRestoreSettleAgentRoots); and in the maintenance
-- sweep (api/maintenance.go, sweepAgentDismissalsOwed), which takes the owed roots `for no key
-- update skip locked` in ascending id, settles each one it got, and deletes the owed rows of roots
-- that no longer exist. A dismissal of this release settles nothing of its own: the trigger's write
-- of its mark, where it moves the column, deletes the owed rows, and one that leaves the column as
-- the mark already holds leaves them to the sweep, a root its author's mark keeps dismissed either
-- way. So when a restore of the release before and a session's reply meet, the reply waits at the
-- mark for the restore, which appends the owed row and commits, and the reply's settle then clears
-- the column.
--
-- Then the catch-up: every mark stored before this migration, which the release before wrote with
-- no trigger to mirror it, sets its root's column here, after the trigger is attached. `create
-- trigger` holds SHARE ROW EXCLUSIVE on user_agent_thread_dismissed to the end of the migration, so
-- no mark is written or deleted between the catch-up's read and its commit, and every mark written
-- after the commit is the trigger's. The update rewrites one messages row for each stored mark of
-- an author, read by the marks' own primary key into messages by its own, and no other row. The
-- census holds messages to its size limit (pgmigrate.CensusTableLimit), since an update driven by
-- the marks is no bounded write: messages held 5,810 rows and 20 MiB at the census of 2026-10-30,
-- far under it.
--
-- The locks: `create table` and `create index` lock only the table this migration creates, which
-- holds no row; `create function` and `create or replace function` take no table lock; `create
-- trigger` takes SHARE ROW EXCLUSIVE on user_agent_thread_dismissed, which blocks the writers of
-- marks and no reader; the update takes ROW EXCLUSIVE on messages and the rows it rewrites. A
-- dismissal of the release before can hold a root `for update` and wait to write its mark behind
-- the first lock while the catch-up waits at that root, so the migration bounds its lock waits at
-- half a second, shorter than deadlock_timeout (1 s by default), as 20261116114515 does for the
-- same kind of cycle: it fails with 55P03 having applied nothing, the dismissal finishes, and the
-- runner applies it again (pgmigrate.LockTimeoutAttempts).
set local lock_timeout = '500ms';

-- kept for one release: agent-dismissed-owed
create table user_agent_thread_dismissed_owed (
  seq     bigserial primary key,
  root_id uuid not null,
  owed_at timestamptz not null default now()
);

-- kept for one release: agent-dismissed-owed
create index user_agent_thread_dismissed_owed_root_id on user_agent_thread_dismissed_owed (root_id);

create function user_agent_thread_dismissed_settle(p_root uuid) returns void language plpgsql as $$
declare
  v_mark timestamptz;
begin
  select d.dismissed_at into v_mark
  from messages m
  join user_agent_thread_dismissed d on d.message_id = m.id and d.login = lower(m.author->>'id')
  where m.id = p_root and m.author->>'kind' = 'user';
  update messages set agent_dismissed_at = v_mark
  where id = p_root and agent_dismissed_at is distinct from v_mark;
  -- kept for one release: agent-dismissed-owed
  delete from user_agent_thread_dismissed_owed where root_id = p_root;
end;
$$;

create function user_agent_thread_dismissed_root() returns trigger language plpgsql as $$
begin
  -- The root the row named before, where the row no longer names it with the same login and was
  -- its author's mark, whose column is set: taken with no wait, or owed where another transaction
  -- holds it.
  if tg_op = 'DELETE' or (tg_op = 'UPDATE' and (new.message_id, new.login) is distinct from (old.message_id, old.login)) then
    if exists (
      select 1 from messages
      where id = old.message_id and agent_dismissed_at is not null
        and author->>'kind' = 'user' and lower(author->>'id') = old.login
    ) then
      perform 1 from messages where id = old.message_id for no key update skip locked;
      if found then
        update messages set agent_dismissed_at = null
        where id = old.message_id and agent_dismissed_at is not null
          and author->>'kind' = 'user' and lower(author->>'id') = old.login;
        -- kept for one release: agent-dismissed-owed
        delete from user_agent_thread_dismissed_owed where root_id = old.message_id;
      else
        -- kept for one release: agent-dismissed-owed
        insert into user_agent_thread_dismissed_owed (root_id) values (old.message_id);
      end if;
    end if;
  end if;
  -- The root the row names now, at the row's moment, which the row's writer holds.
  if tg_op <> 'DELETE' then
    update messages set agent_dismissed_at = new.dismissed_at
    where id = new.message_id and agent_dismissed_at is distinct from new.dismissed_at
      and author->>'kind' = 'user' and lower(author->>'id') = new.login;
    if found then
      -- kept for one release: agent-dismissed-owed
      delete from user_agent_thread_dismissed_owed where root_id = new.message_id;
    end if;
  end if;
  return null;
end;
$$;

create trigger user_agent_thread_dismissed_root after insert or update or delete on user_agent_thread_dismissed
  for each row execute function user_agent_thread_dismissed_root();

-- kept for one release: user-threads-triggers
create or replace function user_agent_thread_dismissed_clear() returns trigger language plpgsql as $$
declare
  v_root uuid := new.id;
  v_parent uuid := new.in_reply_to;
  v_depth integer := 0;
  v_held boolean;
begin
  -- A message that names its node, or which this release recorded in its thread, is a write whose
  -- root inboxwrite cleared in Go, with the retraction of each dismissal it cleared.
  if (new.node_kind is not null and new.node_address is not null) or new.threads_recorded then
    return null;
  end if;
  -- A root is dismissed by the person who sent it; a root's own insert can clear nothing, and a
  -- write by anyone but the session leaves the mark where it is.
  if new.in_reply_to is null or new.author->>'kind' <> 'session' then
    return null;
  end if;
  while v_parent is not null and v_depth < 64 loop
    v_root := v_parent;
    select in_reply_to into v_parent from messages where id = v_root;
    v_depth := v_depth + 1;
  end loop;
  perform 1 from messages where id = v_root for key share;
  -- The delete below moves the root's agent_dismissed_at (user_agent_thread_dismissed_root), and
  -- the settle after it writes the column a restore of the release before left owed, so where a
  -- mark or an owed row of the root stands the root is taken `for no key update` before the
  -- delete, the order every writer of that column takes, and settled once the delete has run.
  v_held := exists (select 1 from user_agent_thread_dismissed where message_id = v_root)
    or exists (select 1 from user_agent_thread_dismissed_owed where root_id = v_root);
  if v_held then
    perform 1 from messages where id = v_root for no key update;
  end if;
  delete from user_agent_thread_dismissed where message_id = v_root;
  if v_held then
    perform user_agent_thread_dismissed_settle(v_root);
  end if;
  return null;
end;
$$;

update messages m set agent_dismissed_at = d.dismissed_at
from user_agent_thread_dismissed d
where m.id = d.message_id
  and m.author->>'kind' = 'user' and lower(m.author->>'id') = d.login
  and m.agent_dismissed_at is distinct from d.dismissed_at;
