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
		{Node: "ship-at-head", Kind: KindStatement, Body: "Legion lands a change at the head it read. It refuses another head.", Where: fixtureWhere},
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
	// No text of the fixture holds code font, so the page shows each one as its body. The run and
	// the negative_control of a check are attributes, which a text holds as they are written.
	for index, text := range want {
		if text.Kind != KindProcedure && text.Kind != KindFailingExample {
			want[index].Held.Shown = text.Body
		}
	}
	if !reflect.DeepEqual(got.Texts, want) {
		t.Errorf("Texts =\n%#v\nwant\n%#v", got.Texts, want)
	}
	terms := []Term{
		{Name: "session", Definition: "One conversation of an agent.", Where: fixtureWhere,
			NameHeld: Held{Shown: "session"}, DefinitionHeld: Held{Shown: "One conversation of an agent."}},
		{Name: "work unit", Definition: "One piece of work that one owner runs.", Where: fixtureWhere,
			NameHeld: Held{Shown: "work unit"}, DefinitionHeld: Held{Shown: "One piece of work that one owner runs."}},
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

// TestExtractNamesARepeatedID reads two typed blocks that name one id as every reader of the
// document reads them: the first keeps the id, and the second has the id that the parser gives
// it. A node and kind that two other forms give with another body, a component's purpose under
// its heading and in the table, is two texts, the second named "<node> (2)".
func TestExtractNamesARepeatedID(t *testing.T) {
	markdown := ":::requirement{#a}\nLegion lands a change.\n:::\n\n:::requirement{#a}\nOne. Two. Three.\n:::\n"
	got, err := ExtractDocument("spec.md", 1, markdown)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Texts) != 2 || got.Texts[0].Node != "a" || got.Texts[1].Node == "a" || got.Texts[1].Node == "" {
		t.Fatalf("Texts = %#v, want a, then the id the parser gives the second block", got.Texts)
	}
	if findings := Check(Context{}, Input{Texts: got.Texts}); len(findings) != 1 || findings[0].Node != got.Texts[1].Node {
		t.Fatalf("Check = %#v, want one finding of Node %s", findings, got.Texts[1].Node)
	}
	component := "## Records (component `r`)\n\nIt holds each record.\n\n" +
		"| Id | Kind | Name | Purpose |\n| --- | --- | --- | --- |\n| `r` | component | Records | It keeps each record. |\n"
	twice, err := ExtractDocument("spec.md", 1, component)
	if err != nil {
		t.Fatal(err)
	}
	var purposes []string
	for _, text := range twice.Texts {
		if text.Kind == KindPurpose {
			purposes = append(purposes, text.Node)
		}
	}
	if !reflect.DeepEqual(purposes, []string{"r", "r (2)"}) {
		t.Fatalf("purposes = %v, want [r r (2)]", purposes)
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

// Each paragraph or list item under a heading Terms that is no term line is a text of Kind line,
// named by its text in quotes, as a person reads it, and Check gives it one finding of rule
// term-line, whose detail shows the form of a term line. A heading below Terms is no such line, and
// Terms ends at the next heading of its level or above. The control: the term lines are terms, and
// give no finding.
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
	drift := Text{Node: `"drift: The slow change of the clock of one machine."`, Kind: line, Body: "drift: The slow change of the clock of one machine.", Where: where}
	if !reflect.DeepEqual(got.Texts, []Text{prose, drift}) {
		t.Errorf("Texts =\n%#v\nwant\n%#v", got.Texts, []Text{prose, drift})
	}
	terms := []Term{
		{Name: "lease", Definition: "The right of one session to one work unit for a time.", Where: where,
			NameHeld: Held{Shown: "lease"}, DefinitionHeld: Held{Shown: "The right of one session to one work unit for a time."}},
		{Name: "session", Definition: "One conversation of an agent.", Where: where,
			NameHeld: Held{Shown: "session"}, DefinitionHeld: Held{Shown: "One conversation of an agent."}},
	}
	if !reflect.DeepEqual(got.Terms, terms) {
		t.Errorf("Terms = %#v, want %#v", got.Terms, terms)
	}
	if got.Counts[line] != 2 || got.Counts[KindDefinition] != 2 {
		t.Errorf("Counts = %v, want 2 lines and 2 definitions", got.Counts)
	}
	detail := `it is no term line, and a term line has the form "- **<term>**: <definition>". ` +
		`Move a line that defines no term, such as an example or a note, out from under the heading Terms. ` +
		`Put a decision block above the heading Terms, and after the answer, write the chosen definition under the heading`
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

// A heading whose text, as a person reads it, is Terms in another case or with a last colon is a
// text of Kind heading, named by that text in quotes, and Check gives it one finding of rule
// terms-heading, whose detail shows the one form. One form is the decision, so the blocks under it
// are no Terms part: they are neither terms nor texts of Kind line, and a line in the form of a
// term line there is a text of Kind term_line, which adds no term. The controls: the one form at
// another level is a Terms heading, and another word, or Terms in a longer text, is no rule's.
func TestATermsHeadingInAnotherFormIsAFinding(t *testing.T) {
	detail := `it is no Terms heading, and a Terms heading has the form "## Terms" at any level`
	for _, c := range []struct{ heading, reads string }{
		{"## terms", "terms"}, {"## TERMS", "TERMS"}, {"## **TERMS**", "TERMS"}, {"## Terms:", "Terms:"},
		{"### __terms__ :", "terms :"}, {"# *Terms*:", "Terms:"},
	} {
		got, err := ExtractDocument("spec.md", 12, "# Spec\n\n"+c.heading+"\n\n- **heartbeat**: A pulse of a pump.\n\nEach term has one definition.\n")
		if err != nil {
			t.Fatal(err)
		}
		text := Text{Node: fmt.Sprintf("%q", c.reads), Kind: KindHeading, Body: c.reads, Where: where}
		line := Text{Node: "heartbeat", Kind: KindTermLine, Body: "A pulse of a pump.", Where: where, Held: Held{Shown: "A pulse of a pump."}}
		if !reflect.DeepEqual(got.Texts, []Text{text, line}) || len(got.Terms) != 0 || got.Counts[KindHeading] != 1 {
			t.Errorf("under %q: Texts = %#v, Terms = %#v, Counts = %v; want the heading text, the term_line text and no term", c.heading, got.Texts, got.Terms, got.Counts)
			continue
		}
		if findings := Check(planted(), Input{Texts: got.Texts, Proposed: got.Terms}); !sameFindings(findings, []Finding{finding(text, "terms-heading", detail)}) {
			t.Errorf("Check under %q = %#v, want one terms-heading finding", c.heading, findings)
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

// A heading whose text, as a person reads it, is Terms is the heading Terms, also when emphasis
// marks it or when it is written in another markdown form: the person reads Terms, and the plan
// reads the list of terms under "a heading whose text is Terms". So each opens the Terms part,
// whose term line is a term, and none is a finding. The control: "## **Terms:**" reads as Terms:,
// which is a terms-heading finding.
func TestAHeadingThatAPersonReadsAsTermsIsTheListOfTerms(t *testing.T) {
	for _, heading := range []string{"## **Terms**", "## *Terms*", "### __Terms__", "## Terms ##", "Terms\n-----"} {
		got, err := ExtractDocument("spec.md", 12, "# Spec\n\n"+heading+"\n\n- **heartbeat**: A pulse of a pump.\n")
		if err != nil {
			t.Fatal(err)
		}
		want := []Term{{Name: "heartbeat", Definition: "A pulse of a pump.", Where: where, NameHeld: Held{Shown: "heartbeat"}, DefinitionHeld: Held{Shown: "A pulse of a pump."}}}
		if !reflect.DeepEqual(got.Terms, want) || len(got.Texts) != 0 || got.Counts[KindHeading] != 0 {
			t.Errorf("under %q: Terms = %#v, Texts = %#v, Counts = %v; want the term heartbeat and no text", heading, got.Terms, got.Texts, got.Counts)
		}
		if findings := Check(planted(), Input{Texts: got.Texts, Proposed: got.Terms}); len(findings) != 0 {
			t.Errorf("Check under %q = %#v, want no finding", heading, findings)
		}
	}
	got, err := ExtractDocument("spec.md", 12, "# Spec\n\n## **Terms:**\n\n- **heartbeat**: A pulse of a pump.\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Terms) != 0 || got.Counts[KindHeading] != 1 {
		t.Errorf("control ## **Terms:**: Terms = %#v, Counts = %v, want no term and one heading text", got.Terms, got.Counts)
	}
}

// Under a heading Terms each line is a term line, so a decision block there breaks the form at its
// question line, and the finding tells the writer the way out: the block stands above the heading,
// and the chosen definition goes under it after the answer. The control: the same block above the
// heading gives no finding, and the term line under the heading is the one term.
func TestADecisionBlockUnderTermsIsAFindingThatNamesTheWayOut(t *testing.T) {
	block := ":::ask{#which urgency=\"med\" multiple=\"false\"}\nWhich definition does the spec keep?\n\n- Keep it: The spec keeps the definition.\n:::\n"
	term := "- **lease**: The right of one session to one work unit for a time.\n"
	for _, test := range []struct {
		name, markdown string
		want           []string
	}{
		{"under the heading", "# Spec\n\n## Terms\n\n" + block + "\n" + term, []string{
			`"Which definition does the spec keep?"`, `"Keep it: The spec keeps the definition."`,
		}},
		{"above the heading", "# Spec\n\n" + block + "\n## Terms\n\n" + term, nil},
	} {
		got, err := ExtractDocument("spec.md", 12, test.markdown)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Terms) != 1 || got.Terms[0].Name != "lease" {
			t.Errorf("%s: Terms = %#v, want the one term lease", test.name, got.Terms)
		}
		findings := Check(planted(), Input{Texts: got.Texts, Proposed: got.Terms})
		var nodes []string
		for _, finding := range findings {
			nodes = append(nodes, finding.Node)
			if finding.Rule != "term-line" || !strings.Contains(finding.Detail, "Put a decision block above the heading Terms") {
				t.Errorf("%s: finding %#v, want rule term-line with the way out for a decision block", test.name, finding)
			}
		}
		if !reflect.DeepEqual(nodes, test.want) {
			t.Errorf("%s: the findings name %q, want %q", test.name, nodes, test.want)
		}
	}
}
