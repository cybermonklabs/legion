-- This migration re-states a view. A view holds no row, and re-stating one reads and writes no
-- row of any table: it replaces the view's definition in the catalogue. The number of existing
-- rows this migration would refuse or rewrite is therefore 0.
select 0;
