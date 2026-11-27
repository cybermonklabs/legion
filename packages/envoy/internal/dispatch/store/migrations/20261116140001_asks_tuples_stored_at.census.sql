-- asks gains one nullable timestamptz column with no default. That is a catalog change: no stored
-- row is read or rewritten, and every existing row reads null, which no constraint can refuse. The
-- number of existing rows this migration would refuse or rewrite is therefore 0, whatever asks
-- holds.
select 0;
