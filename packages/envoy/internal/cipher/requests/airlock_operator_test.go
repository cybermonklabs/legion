package requests

import "testing"

// TestAirlockConnectionAnswersTheEnrollmentsOperator: the airlock proxy asks the hold about the
// person a grant's session acts for (proxy's holdRefusal), and this read is where that
// person comes from: the enrollment's own operator column, read in the join the connection read
// already makes. An enrollment that names no operator - a pod's own, a machine's own session -
// answers the empty string rather than refusing the connection, so the proxy asks nobody about it.
func TestAirlockConnectionAnswersTheEnrollmentsOperator(t *testing.T) {
	t.Parallel()
	m, enr, _, release := twoNameGrant(t, "reach the service")
	placeholder := release.Airlock.Names["LOGIN_KEY"].Slots[SlotValue]
	var operator string
	if err := m.Store.Pool.QueryRow(t.Context(),
		`select operator from enrollments where id=$1`, enr).Scan(&operator); err != nil {
		t.Fatalf("read the enrollment's operator: %v", err)
	}
	if operator == "" {
		t.Fatal("the fixture's enrollment names no operator, so this row could not tell a read of that column from an empty one")
	}
	conn, err := m.AirlockConnection(t.Context(), placeholder)
	if err != nil {
		t.Fatalf("AirlockConnection: %v", err)
	}
	if conn.Operator != operator {
		t.Errorf("connection operator = %q, want the enrollment's %q", conn.Operator, operator)
	}

	// An enrollment with no operator at all: the column is null, which the read answers as the
	// empty string rather than failing the scan.
	if _, err := m.Store.Pool.Exec(t.Context(), `update enrollments set operator=null where id=$1`, enr); err != nil {
		t.Fatalf("clear the enrollment's operator: %v", err)
	}
	conn, err = m.AirlockConnection(t.Context(), placeholder)
	if err != nil {
		t.Fatalf("AirlockConnection with no operator on the enrollment: %v", err)
	}
	if conn.Operator != "" {
		t.Fatalf("connection operator = %q; want none for an enrollment that names no operator", conn.Operator)
	}
}
