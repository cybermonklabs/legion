package refs

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// The reasoning nodes of the backlink fixture (writeBacklinkReasoning): a pre-mortem lens on the
// issue, its finding and its risk, a lens on the spec it triggered, a lens on that finding, a lens
// on a comment's block, a constraint under the issue, and a lens on the issue the store holds gone.
const (
	backlinkLens         = "11111111-1111-4111-8111-111111111111"
	backlinkDocumentLens = "11111111-1111-4111-8111-111111111112"
	backlinkFindingLens  = "11111111-1111-4111-8111-111111111113"
	backlinkBlockLens    = "11111111-1111-4111-8111-111111111114"
	backlinkGoneLens     = "11111111-1111-4111-8111-111111111115"
	backlinkFinding      = "lens/" + backlinkLens + "#f1"
	backlinkRisk         = "33333333-3333-4333-8333-333333333331"
	backlinkConstraint   = "44444444-4444-4444-8444-444444444441"
)

// writeBacklinkReasoning writes what stands on the fixture's nodes and under its lens, as rows of
// the store, which the view's tuple arms answer from the tuples alone: the lens backlinkLens
// (written already, with its authored_by and its has_kind) on CORE-1; its finding, with its own
// authored_by, and its risk, which a check of the spec - its first block, here - prevents; a lens
// on the spec that the first lens triggered; a lens on the finding; a lens on a current block of a
// comment, which counts on the block and on the comment that holds it; and a constraint under
// CORE-1. Beside them are two rows no reading may count: a lens on CORE-1 the store holds gone, and
// a part_of of the constraint to CORE-2 that a later tuple retracts.
func writeBacklinkReasoning(t *testing.T, tx pgx.Tx, spec, specBlock, commentBlock string) {
	t.Helper()
	for _, node := range []struct{ kind, id, supertag, state string }{
		{"lens", backlinkDocumentLens, "lens", "live"},
		{"lens", backlinkFindingLens, "lens", "live"},
		{"lens", backlinkBlockLens, "lens", "live"},
		{"lens", backlinkGoneLens, "lens", "gone"},
		{"block", backlinkFinding, "block", "live"},
		{"risk", backlinkRisk, "risk", "live"},
		{"constraint", backlinkConstraint, "constraint", "live"},
	} {
		execFixture(t, tx, "write the "+node.kind+" "+node.id, `
			insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
			values ($1, $2, $3, 'project', $4, now(), '{"kind":"user","id":"alice"}', 0)
		`, node.kind, node.id, node.supertag, node.state)
	}
	for _, tuple := range []struct {
		fromKind, fromID, kind, toKind, toID string
		asserted                             bool
	}{
		{"lens", backlinkLens, "part_of", "issue", "CORE-1", true},
		{"block", backlinkFinding, "part_of", "lens", backlinkLens, true},
		{"block", backlinkFinding, "authored_by", "person", "person-backlink@example.com", true},
		{"risk", backlinkRisk, "part_of", "lens", backlinkLens, true},
		{"risk", backlinkRisk, "prevented_by", "block", specBlock, true},
		{"lens", backlinkDocumentLens, "part_of", "artifact", spec, true},
		{"lens", backlinkDocumentLens, "triggered_by", "lens", backlinkLens, true},
		{"lens", backlinkFindingLens, "part_of", "lens_block", backlinkFinding, true},
		{"lens", backlinkBlockLens, "part_of", "comment_block", commentBlock, true},
		{"constraint", backlinkConstraint, "part_of", "issue", "CORE-1", true},
		{"lens", backlinkGoneLens, "part_of", "issue", "CORE-1", true},
		{"constraint", backlinkConstraint, "part_of", "issue", "CORE-2", true},
		{"constraint", backlinkConstraint, "part_of", "issue", "CORE-2", false},
	} {
		execFixture(t, tx, "write "+tuple.fromKind+" "+tuple.fromID+" -"+tuple.kind+"-> "+tuple.toKind+" "+tuple.toID, `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
			values ($1, $2, $3, $4, $5, now(), '{"kind":"user","id":"alice"}', 0, $6)
		`, tuple.fromKind, tuple.fromID, tuple.kind, tuple.toKind, tuple.toID, tuple.asserted)
	}
}
