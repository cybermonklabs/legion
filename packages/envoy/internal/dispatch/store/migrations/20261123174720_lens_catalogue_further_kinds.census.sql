-- This migration writes twelve lens kinds of the catalogue with their sources, each a lens_kind
-- node and a constraint node with the tuples they carry. It adds no constraint and rewrites no row
-- another writer wrote: a lens_kind node is written by nothing but this release's migrations and
-- its `envoy-dispatch lens-kind-add`, which ships beside them and so has run nowhere before they
-- apply, and each constraint node takes an id this file fixes. The tuples it appends are facts of
-- those nodes alone, and it reads no existing row of another table. The number of existing rows
-- this migration would refuse or rewrite is therefore 0.
select 0;
