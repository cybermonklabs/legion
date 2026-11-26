// Package refstest holds what the tests of other packages read of refs and no code of refs reads:
// the node kinds refs.BacklinkCounts answers for (BacklinkKinds). It is an ordinary package rather
// than a _test.go file because Go cannot share test-only code across packages, and it imports
// nothing of refs, so refs's own tests can import it: refs's
// TestRefstestBacklinkKindsAreTheArmTablesKinds holds the copy kept here to refs's own table.
package refstest

// BacklinkKinds is every node kind refs.BacklinkCounts answers for, sorted, a fresh slice for each
// caller: a copy of the keys of refs's arm table, which is what the count refuses a kind for not
// holding, held equal to them by refs's TestRefstestBacklinkKindsAreTheArmTablesKinds. A test
// outside refs counts the same kinds the count itself declares rather than keeping a list of its
// own (docs.TestGraphEdgesLosesEveryArmOverAMovedKind).
func BacklinkKinds() []string {
	return []string{"artifact", "ask", "ask_block", "block", "comment", "comment_block", "component",
		"constraint", "interaction", "issue", "lens", "lens_block", "lens_kind", "message", "message_block",
		"page", "person", "risk", "role", "session"}
}
