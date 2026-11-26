package specgraph

import "testing"

// A name of an author that could be the name of the same agent counts as that agent (R9: "An agent
// that did not write the test writes the fault"): byAnotherAgent compares two names with the white
// space at their ends and inside them taken as one space and with no regard to case, so "alice",
// "alice ", " Alice" and "ALICE" name one agent, and a failing example whose fault one of them wrote
// and whose test another wrote shows no other agent. The controls: two other names show one, and a
// name of no letter and no digit names nobody.
func TestTheNamesOfOneAgentAreOneAuthor(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		fault   string
		tests   []string
		another bool
	}{
		{"alice", []string{"alice "}, false},
		{" alice", []string{"alice"}, false},
		{"alice", []string{"bob", "\talice\n"}, false},
		{"Alice", []string{"alice"}, false},
		{"ALICE", []string{"bob", "alice"}, false},
		{"alice  smith", []string{"alice smith"}, false},
		{"alice", []string{"bob"}, true},           // control: two agents
		{"alice", []string{"alicia", "bob"}, true}, // control: a name that only starts like another
		{" ", []string{"bob"}, false},              // control: a fault of nobody's
		{"alice", []string{" "}, false},            // control: a test of nobody's
	} {
		if got := byAnotherAgent(Result{FaultAuthor: row.fault, TestAuthors: row.tests}); got != row.another {
			t.Errorf("the fault by %q and the test by %q: byAnotherAgent gives %t, want %t", row.fault, row.tests, got, row.another)
		}
	}
}
