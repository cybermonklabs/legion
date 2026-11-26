-- This migration writes one attribute node of the interaction type and the tuples it carries. It
-- adds no constraint and rewrites no row another writer wrote: a type node is written by
-- migrations alone, and the node's id is one only this file writes. The tuples it appends are facts
-- of that node alone, and it reads no existing row of another table. The number of existing rows
-- this migration would refuse or rewrite is therefore 0.
select 0;
