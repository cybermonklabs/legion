-- 20261117025905_refs_forms_seed.up.sql
-- Owes an index with the forms 20261117025900 adds to every text already stored: each comment,
-- each ask, each issue message and each document with a markdown version - the sources
-- rebuild-refs reads (refs.RebuildAll). No build that stored them read those forms, so none of
-- their edges was written.
-- It enqueues every row rather than the ones whose text looks like a tab address or a document's
-- dashboard URL: no SQL predicate finds them, which is why the four triggers before it enqueue
-- unconditionally too. The drain reads each text once and writes only what moved.
-- Those triggers are live by now, so a write that lands while this runs is owed by its trigger,
-- and a row a trigger already wrote keeps its enqueued_at.
-- Reads comments, asks, messages, artifacts and artifact_versions (ACCESS SHARE) and writes only
-- refs_forms_owed (0126's and 20261103205042's precedent). A write of a text already stored still
-- waits until the seed commits: its trigger's insert meets the (kind, id) key this statement has
-- inserted and not yet committed. A writer that touches two stored sources in the reverse of this
-- statement's order (comments, asks, messages, documents) can deadlock with it (40P01), which the
-- migration runner does not retry.
insert into refs_forms_owed (kind, id)
select 'comment', id from comments
union all
select 'ask', id from asks
union all
select 'message', id from messages where issue_key is not null
union all
select 'artifact', a.id from artifacts a
where exists (select 1 from artifact_versions v where v.artifact_id = a.id and v.markdown is not null)
on conflict (kind, id) do nothing;
