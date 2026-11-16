-- 20261111212235 replaces write_suggestions_role_check with one that admits a third role,
-- `other_project`, and writes no row. The count is the stored rows the new check would refuse. It
-- reads write_suggestions.role, which 0070_write_suggestions creates, so the column is there when
-- a deployment takes this census, before any migration of this release applies. The check being
-- replaced admits two of the three roles the new one admits, so no stored row is refused, and a
-- non-zero answer would mean a role reached the column that no release writes.
select count(*)
from write_suggestions
where role not in ('related', 'decision', 'other_project');
