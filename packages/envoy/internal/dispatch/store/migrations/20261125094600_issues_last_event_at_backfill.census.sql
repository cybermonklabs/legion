-- 20261125094600 writes the pair of newest-event columns of every issue whose last_seq names an
-- event. The count is those rows, the rows it writes: 20261125094500 adds the pair in the same
-- deploy, so every such row still holds it null when the census runs. It reads last_seq alone,
-- which ships in an earlier release, so the column is there when a deployment takes this census.
select count(*)
from issues
where last_seq > 0;
