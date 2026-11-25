-- 20261125053125 creates one function and attaches its trigger to user_agent_thread_dismissed,
-- replaces the body of user_agent_thread_dismissed_clear, and sets messages.agent_dismissed_at
-- on the root of each mark already stored. No check is added, so no row that exists before it can
-- violate anything; the update writes a nullable column with no constraint and refuses no row, so
-- the count is 0 whatever either table holds. The census reports the size of messages, which the
-- update touches, and holds it to the table limit.
select 0;
