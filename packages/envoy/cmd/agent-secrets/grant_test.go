// packages/envoy/cmd/agent-secrets/grant_test.go
// What one row of `agent-secrets grant list` says about who approved a grant and who decided it.
package main

import (
	"slices"
	"testing"
)

// cell is the value of the column named head in a row grantRow built. A name grantColumns does
// not hold is a test bug, not a missing cell.
func cell(t *testing.T, row []string, head string) string {
	t.Helper()
	i := slices.Index(grantColumns, head)
	if i < 0 {
		t.Fatalf("the table has no %s column; its columns are %v", head, grantColumns)
	}
	return row[i]
}

// TestGrantListNamesTheApproverAndTheDecider: the model judge approves a request in the secret's
// owner's stead, and the grant it mints records that owner as its approver — the grant is the one
// the owner would have made. A table that printed the approver alone therefore read as the
// owner's own approval of a request the owner never saw, which is what the broker's decided_by
// is for: the judge's "judge:<model id>", the deciding person's login for a person's approval,
// and nothing at all for a grant the policy gave without asking.
//
// The three rows are the three answers side by side: only the judged one has an approver and a
// decider that differ, and that difference is the reason the two columns are printed together.
func TestGrantListNamesTheApproverAndTheDecider(t *testing.T) {
	const (
		owner = "ada@example.com"
		judge = "judge:claude-opus-4-6"
	)
	owned := func(s string) *string { return &s }
	for _, tc := range []struct {
		name              string
		grant             OperatorGrant
		approver, decider string
	}{
		{
			name:     "the model judge approved it in the owner's stead",
			grant:    OperatorGrant{Granted: "approval", Approver: owned(owner), DecidedBy: owned(judge)},
			approver: owner, decider: judge,
		},
		{
			name:     "the owner approved it herself",
			grant:    OperatorGrant{Granted: "approval", Approver: owned(owner), DecidedBy: owned(owner)},
			approver: owner, decider: owner,
		},
		{
			name:     "the policy gave it without asking",
			grant:    OperatorGrant{Granted: "automatic"},
			approver: "-", decider: "-",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := grantRow(tc.grant)
			if len(row) != len(grantColumns) {
				t.Fatalf("the row has %d cells for %d columns: %v under %v", len(row), len(grantColumns), row, grantColumns)
			}
			if got := cell(t, row, "APPROVER"); got != tc.approver {
				t.Errorf("APPROVER = %q, want %q", got, tc.approver)
			}
			if got := cell(t, row, "DECIDED_BY"); got != tc.decider {
				t.Errorf("DECIDED_BY = %q, want %q", got, tc.decider)
			}
		})
	}
}
