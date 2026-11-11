package dispatchbody

import (
	"strings"
	"testing"
)

// theDecisionBlock is a decision block as a writer writes one into a body: a question and its
// options inside an `ask` directive, which the reader draws a decision card for and nobody can
// answer where it stands.
const theDecisionBlock = ":::ask{urgency=\"med\"}\n" +
	"Which transport should we expose?\n\n- REST: the platform's own\n- gRPC: streaming\n:::"

// A caller outside this module refuses its own caller differently for a decision block than for
// a bound — the text is not too long, and shortening it changes nothing — so it must read the
// two apart. IsDecisionBlock answers the decision block alone: not a block over the unit bound,
// not a body of more blocks than the count bound, not a body Dispatch takes, and not the nil
// Check answers that one with.
func TestIsDecisionBlockAnswersTheDecisionBlockRefusalAlone(t *testing.T) {
	for _, tc := range []struct {
		name              string
		body              string
		refused, decision bool
	}{
		{"a body holding a decision block", "Lead text.\n\n" + theDecisionBlock + "\n", true, true},
		{"a body whose one block passes the unit bound", strings.Repeat("x", BlockUnits+1), true, false},
		{"a body of one block more than the count bound", strings.TrimSuffix(strings.Repeat("y\n\n", ItemBlocks+1), "\n\n"), true, false},
		{"a body Dispatch takes", "Lead text.\n\n- REST: the platform's own\n- gRPC: streaming\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.body)
			if (err != nil) != tc.refused {
				t.Fatalf("Check = %v, want refused %t", err, tc.refused)
			}
			if got := IsDecisionBlock(err); got != tc.decision {
				t.Fatalf("IsDecisionBlock(%v) = %t, want %t", err, got, tc.decision)
			}
		})
	}
	if IsDecisionBlock(nil) {
		t.Fatal("IsDecisionBlock(nil) = true, want false: no error is no refusal")
	}
}

// The refusal the reader reads is the server's own, worded by itemblocks and nowhere else, over
// the field name every Dispatch body route passes. A caller quotes it whole, so it is pinned
// whole here: the block it names is the body's second, the one the decision block stands at.
func TestCheckAnswersTheServersOwnDecisionBlockRefusal(t *testing.T) {
	err := Check("Lead text.\n\n" + theDecisionBlock + "\n")
	if err == nil {
		t.Fatal("Check = nil, want the decision block refused")
	}
	want := "body block 2 (ask) holds a decision block, which nobody can answer where it stands: " +
		"ask the question in the issue's own document, as a decision block there, or open an ask with " +
		"`dispatch ask`; to show a decision block as an example, put it in a fenced code block"
	if err.Error() != want {
		t.Fatalf("Check = %q, want %q", err, want)
	}
}
