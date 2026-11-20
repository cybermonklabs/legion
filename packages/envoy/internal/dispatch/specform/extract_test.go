package specform

import (
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// everyForm holds each form of a text of The Spec that ExtractDocument reads, and a term-shaped
// line outside Terms.
const everyForm = "# Fixture\n" +
	"\n" +
	":::requirement{#ship-at-head}\n" +
	"Legion lands a change at the head it read.\n" +
	"It refuses another head.\n" +
	":::\n" +
	"\n" +
	"- **Dispatch keeps each answer.** `keeps-answers` · under: `dispatch` · check: `none`\n" +
	"\n" +
	":::check{#v-one serves=\"dispatch://ACME-1/spec#b-ship-at-head\" run=\"Run the merge test.\" negative_control=\"The merger lands any head.\"}\n" +
	"The merger lands only the head it read.\n" +
	":::\n" +
	"\n" +
	"### Records (component `dispatch-records`)\n" +
	"\n" +
	"It holds each record of Dispatch.\n" +
	"\n" +
	"Purpose of dispatch-search: It finds relevant records.\n" +
	"\n" +
	"| Id | Kind | Name | Purpose | Parent or joins |\n" +
	"| --- | --- | --- | --- | --- |\n" +
	"| `dispatch-records` | component | Records | It holds each record of Dispatch. | dispatch |\n" +
	"| dispatch-inbox | component | Inbox | It shows a person what waits on that person. | dispatch |\n" +
	"\n" +
	"## Terms\n" +
	"\n" +
	"- **session**: One conversation of an agent.\n" +
	"* **work unit**: One piece of work that one owner runs.\n" +
	"\n" +
	"## Notes\n" +
	"\n" +
	"- **stray**: This line is no term.\n"

const fixtureWhere = "spec.md version 4"

func TestExtractReadsEachFormOfText(t *testing.T) {
	got, err := ExtractDocument("spec.md", 4, everyForm)
	if err != nil {
		t.Fatal(err)
	}
	want := []Text{
		{Node: "ship-at-head", Kind: KindStatement, Body: "Legion lands a change at the head it read.\nIt refuses another head.", Where: fixtureWhere},
		{Node: "keeps-answers", Kind: KindStatement, Body: "Dispatch keeps each answer.", Where: fixtureWhere},
		{Node: "v-one", Kind: KindClaim, Body: "The merger lands only the head it read.", Where: fixtureWhere},
		{Node: "v-one", Kind: KindProcedure, Body: "Run the merge test.", Where: fixtureWhere},
		{Node: "v-one", Kind: KindFailingExample, Body: "The merger lands any head.", Where: fixtureWhere},
		{Node: "dispatch-records", Kind: KindName, Body: "Records", Where: fixtureWhere},
		{Node: "dispatch-records", Kind: KindPurpose, Body: "It holds each record of Dispatch.", Where: fixtureWhere},
		{Node: "dispatch-search", Kind: KindPurpose, Body: "It finds relevant records.", Where: fixtureWhere},
		{Node: "dispatch-inbox", Kind: KindName, Body: "Inbox", Where: fixtureWhere},
		{Node: "dispatch-inbox", Kind: KindPurpose, Body: "It shows a person what waits on that person.", Where: fixtureWhere},
		{Node: "stray", Kind: KindTermLine, Body: "This line is no term.", Where: fixtureWhere},
	}
	if !reflect.DeepEqual(got.Texts, want) {
		t.Errorf("Texts =\n%#v\nwant\n%#v", got.Texts, want)
	}
	terms := []Term{
		{Name: "session", Definition: "One conversation of an agent.", Where: fixtureWhere},
		{Name: "work unit", Definition: "One piece of work that one owner runs.", Where: fixtureWhere},
	}
	if !reflect.DeepEqual(got.Terms, terms) {
		t.Errorf("Terms = %#v, want %#v", got.Terms, terms)
	}
}

func TestExtractCountsWhatItRead(t *testing.T) {
	got, err := ExtractDocument("spec.md", 4, everyForm)
	if err != nil {
		t.Fatal(err)
	}
	want := map[Kind]int{KindStatement: 2, KindClaim: 1, KindProcedure: 1, KindFailingExample: 1, KindName: 2, KindPurpose: 3, KindDefinition: 2}
	if !reflect.DeepEqual(got.Counts, want) {
		t.Errorf("Counts = %v, want %v", got.Counts, want)
	}
	none, err := ExtractDocument("notes.md", 1, "# Notes\n\nProse with no text of The Spec.\n")
	if err != nil {
		t.Fatal(err)
	}
	zero := map[Kind]int{KindStatement: 0, KindClaim: 0, KindProcedure: 0, KindFailingExample: 0, KindName: 0, KindPurpose: 0, KindDefinition: 0}
	if !reflect.DeepEqual(none.Counts, zero) || len(none.Texts) != 0 || len(none.Terms) != 0 {
		t.Errorf("a document with no text: %#v, want every count 0 and nothing read", none)
	}
}

// TestExtractReadsLevelOneShape reads level one's 43 system, component and interface headings
// and its table of components: the 40 that stand under a heading of their own and in the table
// are one text each.
func TestExtractReadsLevelOneShape(t *testing.T) {
	markdown, err := os.ReadFile("testdata/level-one-components.md")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ExtractDocument("spec.md", 10, string(markdown))
	if err != nil {
		t.Fatal(err)
	}
	if got.Counts[KindName] != 43 || got.Counts[KindPurpose] != 43 {
		t.Errorf("level one gives %d names and %d purposes, want 43 and 43", got.Counts[KindName], got.Counts[KindPurpose])
	}
	var purposes []string
	for _, text := range got.Texts {
		if text.Kind == KindPurpose {
			purposes = append(purposes, text.Node)
		}
	}
	for _, node := range []string{"legion", "dispatch", "envoy"} {
		if !slices.Contains(purposes, node) {
			t.Errorf("no purpose of %s among %v", node, purposes)
		}
	}
}

func TestExtractNamesABlockWithNoID(t *testing.T) {
	got, err := ExtractDocument("spec.md", 1, ":::requirement{}\nLegion lands a change.\n:::\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Texts) != 1 || got.Texts[0].Node != "requirement 1" {
		t.Fatalf("Texts = %#v, want one text of Node requirement 1", got.Texts)
	}
}

func TestExtractNamesARepeatedID(t *testing.T) {
	got, err := ExtractDocument("spec.md", 1, ":::requirement{#a}\nLegion lands a change.\n:::\n\n:::requirement{#a}\nLegion lands another change.\n:::\n")
	if err != nil {
		t.Fatal(err)
	}
	var nodes []string
	for _, text := range got.Texts {
		nodes = append(nodes, text.Node)
	}
	if !reflect.DeepEqual(nodes, []string{"a", "a (2)"}) {
		t.Fatalf("Nodes = %v, want [a a (2)]", nodes)
	}
	if findings := Check(planted(), Input{Texts: got.Texts}); len(findings) != 0 {
		t.Fatalf("Check = %#v, want none", findings)
	}
	markdown := ":::requirement{#a}\nLegion lands a change.\n:::\n\n:::requirement{#a}\nOne. Two. Three.\n:::\n"
	broken, err := ExtractDocument("spec.md", 1, markdown)
	if err != nil {
		t.Fatal(err)
	}
	if findings := Check(Context{}, Input{Texts: broken.Texts}); len(findings) != 1 || findings[0].Node != "a (2)" {
		t.Fatalf("Check = %#v, want one finding of Node a (2)", findings)
	}
}

// TestExtractSkipsCodeFences holds the extractor to the document's own texts: an example in a
// code fence, as a how-to writes one, is no text of The Spec.
func TestExtractSkipsCodeFences(t *testing.T) {
	markdown := "```\n:::requirement{#example}\nOne. Two. Three.\n:::\n- **<statement>** `<id>` · under: `<parent id>` · check: `none`\n```\n\n~~~\nPurpose of x: One. Two.\n~~~\n"
	got, err := ExtractDocument("the-spec.md", 2, markdown)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Texts) != 0 || len(got.Terms) != 0 {
		t.Fatalf("ExtractDocument read %#v from code fences, want nothing", got)
	}
}

// A line under a heading Terms that is no term line is never dropped in silence: the extractor
// reads it as the term, or Check finds it. Each line here is in another form than the term line
// "- **<term>**: <definition>": the colon in the bold, a space before the colon, no space after
// it, two spaces after the marker, and a numbered item. The control: the term line is read as one
// term.
func TestALineUnderTermsThatIsNoTermLineIsNotDroppedInSilence(t *testing.T) {
	for _, line := range []string{
		"- **drift:** The slow change of the clock of one machine.",
		"- **drift** : The slow change of the clock of one machine.",
		"- **drift**:The slow change of the clock of one machine.",
		"-  **drift**: The slow change of the clock of one machine.",
		"1. **drift**: The slow change of the clock of one machine.",
	} {
		markdown := "# Terms\n\n" + line + "\n"
		extracted, err := ExtractDocument("terms.md", 4, markdown)
		if err != nil {
			t.Fatalf("ExtractDocument(%q): %v", line, err)
		}
		read := false
		for _, term := range extracted.Terms {
			if strings.EqualFold(strings.TrimSpace(term.Name), "drift") {
				read = true
			}
		}
		findings := Check(planted(), Input{Texts: extracted.Texts, Proposed: extracted.Terms})
		if !read && len(findings) == 0 {
			t.Errorf("%q under # Terms: the extractor read terms %#v and Check found nothing, so the definition is dropped in silence", line, extracted.Terms)
		}
	}
	extracted, err := ExtractDocument("terms.md", 4, "# Terms\n\n- **drift**: The slow change of the clock of one machine.\n")
	if err != nil || len(extracted.Terms) != 1 || extracted.Terms[0].Name != "drift" {
		t.Fatalf("control, the plan's form: ExtractDocument = %#v, %v, want the one term drift", extracted.Terms, err)
	}
}

// Each line under a heading Terms that is not blank and is no term line is a text of Kind line,
// named by the line in quotes, and Check gives it one finding of rule term-line, whose detail shows
// the form of a term line. A heading below Terms is no such line, and Terms ends at the next heading
// of its level or above. The control: the term lines are terms, and give no finding.
func TestALineUnderTermsThatIsNoTermLineIsAFinding(t *testing.T) {
	markdown := "# Spec\n\n## Terms\n\nEach term has one definition.\n\n" +
		"- **drift:** The slow change of the clock of one machine.\n" +
		"- **lease**: The right of one session to one work unit for a time.\n\n" +
		"### Dispatch\n\n" +
		"- **session**: One conversation of an agent.\n\n" +
		"## Notes\n\nThis paragraph is under Notes.\n"
	got, err := ExtractDocument("spec.md", 12, markdown)
	if err != nil {
		t.Fatal(err)
	}
	line := Kind("line")
	prose := Text{Node: `"Each term has one definition."`, Kind: line, Body: "Each term has one definition.", Where: where}
	drift := Text{Node: `"- **drift:** The slow change of the clock of one machine."`, Kind: line, Body: "- **drift:** The slow change of the clock of one machine.", Where: where}
	if !reflect.DeepEqual(got.Texts, []Text{prose, drift}) {
		t.Errorf("Texts =\n%#v\nwant\n%#v", got.Texts, []Text{prose, drift})
	}
	terms := []Term{
		{Name: "lease", Definition: "The right of one session to one work unit for a time.", Where: where},
		{Name: "session", Definition: "One conversation of an agent.", Where: where},
	}
	if !reflect.DeepEqual(got.Terms, terms) {
		t.Errorf("Terms = %#v, want %#v", got.Terms, terms)
	}
	if got.Counts[line] != 2 || got.Counts[KindDefinition] != 2 {
		t.Errorf("Counts = %v, want 2 lines and 2 definitions", got.Counts)
	}
	detail := `it is no term line, and a term line has the form "- **<term>**: <definition>"`
	want := []Finding{finding(prose, "term-line", detail), finding(drift, "term-line", detail)}
	if findings := Check(planted(), Input{Texts: got.Texts, Proposed: got.Terms}); !sameFindings(findings, want) {
		t.Fatalf("Check =\n%#v\nwant\n%#v", findings, want)
	}
}

// A heading that a reader reads as Terms is not dropped in silence. Under "## terms", "## TERMS",
// "## **Terms**" and "## Terms:" the definitions would never join the list. Either fix passes this
// test: read the heading as Terms, or make the heading a finding. The control: "## Terms" reads the
// term.
func TestATermsHeadingInAnotherFormIsNotDroppedInSilence(t *testing.T) {
	body := "\n\n- **heartbeat**: A pulse of a pump.\n"
	extracted, err := ExtractDocument("spec.md", 1, "# Spec\n\n## Terms"+body)
	if err != nil || len(extracted.Terms) != 1 {
		t.Fatalf("control, ## Terms: ExtractDocument = %#v, %v, want the one term heartbeat", extracted.Terms, err)
	}
	for _, heading := range []string{"## terms", "## TERMS", "## **Terms**", "## Terms:"} {
		extracted, err := ExtractDocument("spec.md", 1, "# Spec\n\n"+heading+body)
		if err != nil {
			t.Fatalf("ExtractDocument under %q: %v", heading, err)
		}
		if len(extracted.Terms) == 0 && len(Check(planted(), Input{Texts: extracted.Texts, Proposed: extracted.Terms})) == 0 {
			t.Errorf("under %q the extractor read no term and Check found nothing, so the definition is dropped in silence", heading)
		}
	}
}

// A heading whose text is Terms in another case, with emphasis marks or with a last colon is a
// text of Kind heading, named by the line in quotes, and Check gives it one finding of rule
// terms-heading, whose detail shows the one form. One form is the decision, so the lines under it
// are no Terms part: they are neither terms nor texts of Kind line, and a line in the form of a
// term line there is a text of Kind term_line, which adds no term. The controls: the one form at
// another level is a Terms heading, and another word, or Terms in a longer text, is no rule's.
func TestATermsHeadingInAnotherFormIsAFinding(t *testing.T) {
	detail := `it is no Terms heading, and a Terms heading has the form "## Terms" at any level`
	for _, heading := range []string{"## terms", "## TERMS", "## **Terms**", "## Terms:", "### __terms__ :", "# *Terms*:"} {
		got, err := ExtractDocument("spec.md", 12, "# Spec\n\n"+heading+"\n\n- **heartbeat**: A pulse of a pump.\nEach term has one definition.\n")
		if err != nil {
			t.Fatal(err)
		}
		text := Text{Node: fmt.Sprintf("%q", heading), Kind: KindHeading, Body: heading, Where: where}
		line := Text{Node: "heartbeat", Kind: KindTermLine, Body: "A pulse of a pump.", Where: where}
		if !reflect.DeepEqual(got.Texts, []Text{text, line}) || len(got.Terms) != 0 || got.Counts[KindHeading] != 1 {
			t.Errorf("under %q: Texts = %#v, Terms = %#v, Counts = %v; want the heading text, the term_line text and no term", heading, got.Texts, got.Terms, got.Counts)
			continue
		}
		if findings := Check(planted(), Input{Texts: got.Texts, Proposed: got.Terms}); !sameFindings(findings, []Finding{finding(text, "terms-heading", detail)}) {
			t.Errorf("Check under %q = %#v, want one terms-heading finding", heading, findings)
		}
	}
	for _, heading := range []string{"# Terms", "#### Terms", "## Glossary", "## Definitions", "## Terms of use", "## The terms"} {
		got, err := ExtractDocument("spec.md", 12, "# Spec\n\n"+heading+"\n\n- **heartbeat**: A pulse of a pump.\n")
		if err != nil {
			t.Fatal(err)
		}
		if findings := Check(planted(), Input{Texts: got.Texts, Proposed: got.Terms}); len(findings) != 0 || got.Counts[KindHeading] != 0 {
			t.Errorf("control %q: Check = %#v, Counts = %v, want no finding and no heading text", heading, findings, got.Counts)
		}
	}
}
