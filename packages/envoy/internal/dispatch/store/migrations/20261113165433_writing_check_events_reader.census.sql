-- 20261113165433 adds writing_check_events.reader and widens the table's outcome check, and
-- writes no row. It cannot name the table: 20261113085841, which creates it, ships in the same
-- release, so the table does not exist when a deployment takes this census. The check it replaces
-- admits every outcome but one of the new check's, so no stored row is refused.
select 0;
