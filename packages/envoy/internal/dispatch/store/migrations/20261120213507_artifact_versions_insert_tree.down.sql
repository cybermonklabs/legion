-- 20261120213507_artifact_versions_insert_tree.down.sql
--
-- The rollback an operator runs by hand: the column goes, and with it the record of which words of
-- each version a pending insert suggestion put there. Every version then reads as stored, so a
-- version holding the words of an insert whose comment was deleted gives them again, which no
-- rollback can take back. Run it only against a build that reads none of it.
alter table artifact_versions drop column insert_tree;
