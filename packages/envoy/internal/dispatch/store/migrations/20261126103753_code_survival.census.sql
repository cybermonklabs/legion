-- 20261126103753 creates five tables, code_repos, code_changes, code_pairs, code_days and
-- code_projects, and reads, refuses and rewrites no stored row: each is new, so none holds a row
-- that a check of it could refuse (code_days' state checks and its blocked_files and
-- blocked_unlisted columns among them), and code_projects' foreign key to projects checks no row
-- of projects when the table is created.
select 0;
