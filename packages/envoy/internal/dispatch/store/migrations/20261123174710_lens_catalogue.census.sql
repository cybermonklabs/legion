-- This migration writes one supertag node, the attribute nodes of the store's types that declare
-- no block type, and the ten lens kinds with their sources, each with the tuples it carries. It
-- adds no constraint and rewrites no row another writer wrote: a type node is written by
-- migrations alone; a lens_kind node by nothing but this release's migrations and its
-- `envoy-dispatch lens-kind-add`, which ships beside them and so has run nowhere before they
-- apply; and each constraint node takes an id this file fixes. The tuples it appends are facts of
-- those nodes alone, and it reads no existing row of another table. The number of existing rows
-- this migration would refuse or rewrite is therefore 0.
select 0;
