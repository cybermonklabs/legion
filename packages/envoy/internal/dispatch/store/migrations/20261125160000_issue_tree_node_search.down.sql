-- Run by hand: no runner applies a rollback script. Nothing references the table, and the release
-- before reads it nowhere and searches document_search, so it is dropped on its own with its three
-- indexes, node_search_vector, node_search_document_vector and node_search_ask_fences.
drop table if exists node_search;
