-- 20261125143901 adds two nullable columns to issues, last_event_at and last_event_at_seq, with no
-- default and no check, and fills neither: every existing row holds both null, so it refuses no
-- row and rewrites none.
select 0;
