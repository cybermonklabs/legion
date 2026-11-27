-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and the keyword fill's first-body repair and the store census's count of the cut first
-- bodies then read the comment versions through the primary key again.
drop index concurrently if exists search_text_versions_cut_comments;
