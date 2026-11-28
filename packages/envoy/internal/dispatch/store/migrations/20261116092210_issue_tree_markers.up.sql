-- 20261116092210_issue_tree_markers.up.sql
--
-- Six more tables learn the address of the node they project, as document_blocks, block_refs
-- and comments did in 20261109022248: issues, issue_links, components, component_depends,
-- issue_component_members and artifacts. A node is a thing Dispatch holds and a tuple is one
-- fact stated about it (20261109013400_graph_store.up.sql); each of these tables stays where it
-- is and becomes a projection of those tuples, so each row has to say which node it is the
-- projection of.
--   node_kind      the `kind` of the row's node in graph_nodes.
--   node_address   its `id` there. Two columns rather than one text address, because that is how
--                  graph_nodes is keyed.
--
-- The pair is the marker a row is read by, as 20261109215947 reads the three tables before
-- these: a row that names its node is answered from the tuples, and the arm of `graph_edges`
-- over its table answers it only while it names none. Nothing fills either column in this
-- commit: the writers that set the pair, in the transaction that appends the row's tuples, and
-- the backfill that sets it on the rows written before them are this branch's own, and a row
-- that names no node is answered as it is today.
--
-- Both columns are nullable and have no default, so each ALTER is a catalog change: no stored
-- row is rewritten or read, and every row these tables hold reads null in both. No foreign key
-- is added beside them. issue_links takes this one ALTER and nothing else, and the other five
-- take the same pair the same way.
--
-- Each ALTER still holds ACCESS EXCLUSIVE on its table for the rest of this one transaction, so
-- the migration holds every table it has altered while it waits for the next one, and a live
-- writer that holds one of the later tables and then reaches for an earlier one closes a cycle
-- with it. Under the runner's five-second bound (pgmigrate.LockTimeout) either side can lose
-- that race as a 40P01 deadlock, which the runner does not retry (store/store.go
-- RetryLockTimeouts). Half a second, shorter than deadlock_timeout (1 s by default), makes this
-- migration give up first instead, with 55P03 and nothing applied, and the runner applies it
-- again, as 20261109022248 does for the same reason.
set local lock_timeout = '500ms';

alter table issues add column node_kind text, add column node_address text;

alter table issue_links add column node_kind text, add column node_address text;

alter table components add column node_kind text, add column node_address text;

alter table component_depends add column node_kind text, add column node_address text;

alter table issue_component_members add column node_kind text, add column node_address text;

alter table artifacts add column node_kind text, add column node_address text;
