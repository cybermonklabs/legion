-- The drop deletes the cached answers of a table no running build reads: 20261107234701's
-- branch_commits holds that answer now. No stored value anything still reads is lost, and no
-- copy of a row here could be read back faithfully in any case - a row names no branch, and the
-- default branch it was asked about may since have been renamed, which is the argument
-- 20261107234701_branch_commits.up.sql:34-36 already makes for refusing a backfill. So there is
-- nothing here for a census to count.
select 0;
