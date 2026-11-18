-- Run by hand, outside a transaction: DROP INDEX CONCURRENTLY refuses to run inside one, and no
-- runner applies a rollback script. Nothing depends on this index, so it can be dropped on its
-- own, and the sweep of overdue runs then scans going_requests again.
drop index concurrently if exists going_requests_ends_at;
