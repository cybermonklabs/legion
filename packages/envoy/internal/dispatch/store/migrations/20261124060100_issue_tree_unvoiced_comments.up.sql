-- pgmigrate:non-transactional
-- 20261124060100_issue_tree_unvoiced_comments.up.sql - the comments of an issue no person wrote.
--
-- The read with no view of an issue's tree answers the newest page of its conversation (refs'
-- ReadCommentPage, R2): a comment a person wrote is of it as it stands, and only a comment no
-- person wrote climbs to its thread to ask whether a person wrote there. The page reads the first
-- newest first through comments_issue_created, as far as the page reaches; the second it read by
-- testing the author of every comment of the issue, 9,252 of them on an issue of the most
-- comments, all a person's. This index holds the comments no person wrote alone, keyed by issue
-- as the read narrows to it, so that read is a probe of it. Its predicate is the read's own test of
-- the author, written as the read writes it (refs' personAuthor, negated `is not true`), which is
-- what lets the planner prove the read keeps to it.
--
-- comments grows with every comment. A concurrent build does not hold off its writers while it
-- scans the table, so the migration runs outside a transaction. It is a performance index: no
-- constraint and no later migration depends on it. A failed concurrent build leaves an INVALID
-- index of this name on comments, which the runner drops with DROP INDEX CONCURRENTLY and builds
-- again on the next attempt.
create index concurrently if not exists comments_issue_unvoiced
    on comments (issue_key, created_at)
    where (author->>'kind' = 'user') is not true;
