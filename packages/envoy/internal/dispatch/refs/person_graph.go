package refs

// AllEdgesSQL is the complete derived graph relation for new named reads. graph_edges retains
// the relations old builds read; sibling views add identity, recorded delivery and issue
// relations. Keeping the union here leaves old readers' results and query plans unchanged.
const AllEdgesSQL = `(
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_edges
	union all
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_person_edges
	union all
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_issue_edges
)`
