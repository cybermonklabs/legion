-- 20261113090412_events_session_titles.up.sql
--
-- Keeps 20261113082156's session_titles in step with events (dispatch://INFRA-2916, R13): every
-- event a session writes stamping a title records that title as the session's own, unless the
-- row already holds a title the session stamped later. delivery.ResolveSessionTitles reads that
-- row by primary key where it used to resolve the title out of events on every page read.
--
-- A trigger rather than a write in events.Broker.Append, which is the one server path that
-- inserts an event (api.TestOnlyTheBrokerInsertsEvents). Two other writers insert events and a
-- write in Append would reach neither: during the rolling deploy that ships this, pods of the
-- release before it append events through their own Append, which writes no title, for as long
-- as they serve; and the test-only helper packages and tests that seed events with a bare insert
-- (legion/legiontest among them) would each have to write the title row themselves. The trigger
-- runs in Postgres, so it covers every insert from the commit of this migration on, whichever
-- build sends it. The events stored before that are 20261113090413's fill.
--
-- The rule is the read it replaces, the newest title a session stamped: an event of a session
-- actor whose origin carries a non-empty session_title (an event that carries none, as a
-- delivery reply does, takes no title away), the newest created_at winning. `where
-- excluded.titled_at > session_titles.titled_at` is that order: an event whose transaction
-- opened before the stored one's and committed after it leaves the newer title in place. It is
-- `>` and not `>=` because of the tie, two titled events of one session at one created_at, which
-- one transaction's events all are (created_at defaults to now(), the transaction's start). The
-- read this replaces stated no order for it and took the first of them in
-- events_session_actor_created_at_idx, whose rows of equal key lie in heap order: the order they
-- were inserted in, on a heap they were appended to. The trigger fires in insert order, so `>`
-- keeps the first event of the instant, and the fill names the same tie outright, the lower event
-- id. An actor with no id is no session a reader can ask about, and the primary key would refuse
-- it, so the guard leaves it out.
--
-- It is a constraint trigger deferred to the commit, as 0120's is, for its row lock. The upsert
-- locks the session's row until the transaction ends. Taken at the insert, that lock would be
-- held through everything the transaction does after it, and a write that appends one event and
-- then locks another owner's row could deadlock with a second write of the same session that
-- holds that owner and waits for the title row: two parallel writes of one agent, which is
-- ordinary. At the commit the transaction has taken every lock it will take, so the title row
-- is the last lock it waits for and no cycle can run through it. The row is still written in the
-- event's own transaction: a reader on another connection sees the new title exactly when it
-- sees the event. A read later in the same transaction does not, and no caller of
-- ResolveSessionTitles reads a title after appending an event in its own transaction.
--
-- A function and a trigger alone, so the pre-deploy census exempts the migration from its
-- table-size limit (internal/pgmigrate/exempt.go): neither statement reads a row of events, the
-- largest table a deployment holds. Its own migration, so the transaction holds that table's lock
-- alone: create trigger takes SHARE ROW EXCLUSIVE on events, which waits for the transactions
-- that have inserted an event and holds back new inserts for as long as it waits, under the
-- runner's five-second bound.
create function events_session_title() returns trigger language plpgsql as $$
begin
  insert into session_titles (session_id, title, titled_at)
  values (new.actor->>'id', new.actor->'origin'->>'session_title', new.created_at)
  on conflict (session_id) do update
     set title = excluded.title, titled_at = excluded.titled_at
   where excluded.titled_at > session_titles.titled_at;
  return null;
end;
$$;
create constraint trigger events_session_title after insert on events
  deferrable initially deferred
  for each row when (new.actor->>'kind' = 'session' and new.actor->>'id' is not null
                     and nullif(new.actor->'origin'->>'session_title', '') is not null)
  execute function events_session_title();
