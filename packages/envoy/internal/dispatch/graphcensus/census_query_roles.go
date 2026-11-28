package graphcensus

import "github.com/sjawhar/envoy/internal/dispatch/graphstore"

// queryRoleProjections is R16's role census of converted issues (projections): the artifacts rows
// of converted issues - those holding an issue_trees row or a document_rooms row - that still carry
// a role. A converted issue's documents are query nodes and hold no role on their artifacts rows,
// so a clean store reads zero rows and zero differences; any row carrying a role on a converted
// issue stands as a row-side difference against an empty node side and refuses the census.
var queryRoleProjections = []projection{{
	name:     "converted_artifact_roles",
	nodeKind: graphstore.KindArtifact,
	table:    "artifacts",
	note: "the artifacts rows of converted issues (holding an issue_trees or document_rooms row) that carry a role;" +
		" every such row is a difference because a converted issue holds no role on its artifacts rows (R16)",
	rows: `
		select 'artifact ' || a.ref_key, '` + graphstore.KindArtifact + `', a.ref_key, 'role:' || a.role
		from artifacts a
		where a.role is not null
		  and (exists (select 1 from issue_trees t where t.issue_key = a.issue_key)
		       or exists (select 1 from document_rooms r join artifacts d on d.id = r.artifact_id where d.issue_key = a.issue_key))`,
	nodes: `
		select ''::text, ''::text where false`,
	emptyAgrees: true,
}}

// queryRoleBackfills are the backfill's passes over the roles an issue's documents hold and the
// queries that take their place (backfillPasses). The conversion clears a converted issue's
// document roles inside its transaction, so no backfill pass runs here.
var queryRoleBackfills []backfillPass
