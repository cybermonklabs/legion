-- 20261112023740 drops two constraints and refuses no row: a drop never fails on the data it
-- holds, and every row that satisfied the constraints before still exists after. It reads no row
-- and rewrites none, so there is nothing for a census to count. What the pre-deploy report should
-- be read for is the holders of the locks on issues and repo_projects: each drop takes ACCESS
-- EXCLUSIVE for the instant the transaction lasts.
select 0;
