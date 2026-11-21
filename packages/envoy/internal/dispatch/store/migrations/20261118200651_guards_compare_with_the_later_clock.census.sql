-- 20261118200651 replaces two plpgsql functions whole (create or replace), written into the
-- catalogue alone; the triggers that call them run on later writes only, so the migration reads,
-- refuses and rewrites no stored row, and counts 0.
select 0;
