package specchange

import (
	"reflect"
	"testing"
)

// conclusionSpec is a version of an issue's spec that holds prose, a requirement and the
// contract's example conclusion: an approved statement, the kind of difference, its evidence, a
// prose paragraph, and two outcomes, one restored and one required by another statement.
const conclusionSpec = "# The spec\n\nProse before the conclusion.\n\n" +
	":::requirement{#a-bill-is-sent kind=\"behavior\"}\nA bill is sent.\n:::\n\n" +
	":::conclusion{#c-restore statement=\"an-order-is-paid-once\" difference=\"code_breaks\"}\n" +
	"Evidence: TestASecondPaymentIsRefused fails on the code before the fix and passes after it.\n\n" +
	"The fix puts back the check of the payment.\n\n" +
	"Outcome: (restored) A second payment of one order is refused again.\n\n" +
	"Outcome: (requires a-bill-names-its-order) The refusal names the order.\n:::\n"

// A conclusion is read from the version it is in: its block id, its statement and difference,
// each Evidence: paragraph with text, and each Outcome: paragraph in the line form; any other
// paragraph is prose and is read as nothing.
func TestConclusionOfReadsItsAttributesEvidenceAndOutcomes(t *testing.T) {
	t.Parallel()
	got, err := ConclusionOf(VersionText{Document: testIssue + "/spec", Version: 4, Markdown: conclusionSpec})
	if err != nil {
		t.Fatalf("read the conclusion: %v", err)
	}
	want := &Conclusion{
		Block: "c-restore", Statement: "an-order-is-paid-once", Difference: "code_breaks",
		Evidence: []string{"TestASecondPaymentIsRefused fails on the code before the fix and passes after it."},
		Outcomes: []Outcome{
			{Text: "A second payment of one order is refused again.", Restored: true},
			{Text: "The refusal names the order.", Statement: "a-bill-names-its-order"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the conclusion reads\n%+v\nwant\n%+v", got, want)
	}
}

// A version with no conclusion block holds none; a conclusion that sets no_statement says so; an
// Evidence: paragraph with nothing after its colon is no evidence; an Outcome: line whose marker
// names no statement id is kept as written; and a version with two conclusions answers the first
// with the ids of the others.
func TestConclusionOfReadsEachShapeOfAVersion(t *testing.T) {
	t.Parallel()
	read := func(t *testing.T, markdown string) *Conclusion {
		t.Helper()
		conclusion, err := ConclusionOf(VersionText{Document: testIssue + "/spec", Version: 1, Markdown: markdown})
		if err != nil {
			t.Fatalf("read the conclusion of %q: %v", markdown, err)
		}
		return conclusion
	}
	if got := read(t, "# The spec\n\nNo conclusion yet.\n"); got != nil {
		t.Errorf("a version with no conclusion reads %+v, want none", got)
	}
	operational := read(t, ":::conclusion{#c-deploy no_statement=\"true\"}\nEvidence:   \n\nEvidence: The deploy job named the wrong host.\n\n"+
		"Outcome: (requires Not An Id) The host is right.\n:::\n")
	want := &Conclusion{Block: "c-deploy", NoStatement: true, Evidence: []string{"The deploy job named the wrong host."},
		Outcomes: []Outcome{{Text: "(requires Not An Id) The host is right."}}}
	if !reflect.DeepEqual(operational, want) {
		t.Errorf("the operational conclusion reads\n%+v\nwant\n%+v", operational, want)
	}
	two := read(t, ":::conclusion{#c-first no_statement=\"true\"}\nEvidence: One.\n:::\n\n"+
		":::conclusion{#c-second no_statement=\"true\"}\nEvidence: Two.\n:::\n\n"+
		":::conclusion{#c-third no_statement=\"true\"}\nEvidence: Three.\n:::\n")
	if two == nil || two.Block != "c-first" || !reflect.DeepEqual(two.Others, []string{"c-second", "c-third"}) {
		t.Errorf("three conclusions read %+v, want c-first with the others c-second and c-third", two)
	}
}

// A version the document parser cannot read is an error, never a version with no conclusion.
func TestConclusionOfRefusesAVersionItCannotRead(t *testing.T) {
	t.Parallel()
	if got, err := ConclusionOf(VersionText{Document: testIssue + "/spec", Version: 2, Markdown: "<div>\nblock HTML\n</div>\n"}); err == nil {
		t.Errorf("an unreadable version reads %+v, want an error", got)
	}
}
