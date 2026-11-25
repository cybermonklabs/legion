-- pgmigrate:non-transactional
-- The Inbox's two states that answer every Agent row, state=inbox and state=unread
-- (api/replies_agent.go's myAgentInboxRoots and myAgentUnreadRoots), read the person's one-off
-- roots they have not dismissed. messages_direct_roots (0051) holds every one-off root of theirs,
-- dismissed or not, so a read through it reads the whole pile and throws the dismissed roots away.
-- This index holds the same roots in the same order, less every root its author's dismissal marks
-- (messages.agent_dismissed_at, 20261125052902, which 20261125053125's trigger keeps), so both
-- reads read the roots they answer and no dismissed one, however many the person dismissed.
--
-- messages_direct_roots stays: the release before reads its roots through it while it serves
-- beside this one.
--
-- It is a performance index: no constraint and no later migration depends on it. A failed
-- concurrent build leaves an INVALID index of this name on messages, which the runner drops with
-- DROP INDEX CONCURRENTLY and builds again on the next attempt.
create index concurrently if not exists messages_direct_roots_live
    on messages (lower(author->>'id'), target)
    where issue_key is null and in_reply_to is null and target like 'session:%'
      and author->>'kind' = 'user' and agent_dismissed_at is null;
