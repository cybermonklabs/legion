-- Run by hand outside a transaction, before rolling back 20261109022248.
drop index concurrently if exists block_refs_node_address;
