package dispatchblocks

import (
	"reflect"
	"testing"
)

// aVersion is a document version as Dispatch stores one: requirement blocks, checks whose
// `serves` names blocks of an issue's primary document, of another of its documents and of a
// project's document, pinned and unpinned, a block id that holds a character a reference
// percent-encodes, a check repeating an earlier check's id, a check nested in a callout, and the
// paragraphs and headings around them, which are no typed blocks.
const aVersion = "# The design\n\n" +
	"A paragraph before any block.\n\n" +
	":::requirement{#r1-ancestor}\n**R1.** The first requirement.\n:::\n\n" +
	":::requirement{#r:2}\nThe second requirement, its id holding a colon.\n:::\n\n" +
	`:::check{#k1 serves="dispatch://LEGION-1/spec#b-r1-ancestor dispatch://LEGION-1/artifact/plan-md#b-t1@v3" surface="api"}` + "\n" +
	"The first check.\n:::\n\n" +
	`:::check{#k2 serves="dispatch://LEGION-1/spec#b-r%3A2@v7" surface="cli"}` + "\n" +
	"The second check, serving a block whose id is percent-encoded, at version 7.\n:::\n\n" +
	`:::check{#k1 serves="dispatch://LEGION-1/spec#b-r9"}` + "\n" +
	"A check repeating the first one's id.\n:::\n\n" +
	"::::callout{#c1}\n" +
	`:::check{#k3 serves="dispatch://ACME/artifact/roadmap#b-goal"}` + "\n" +
	"A check nested in a callout, serving a project's document.\n:::\n" +
	"::::\n\n" +
	":::check{#k4}\nA check that serves nothing.\n:::\n\n" +
	"A closing paragraph.\n"

// A version reads as its typed blocks in document order, each with its id, its type and the
// references its `block[]` attributes hold, each parsed into the issue or project, the document,
// the decoded block id and the pin it names. The paragraphs and the heading are no typed blocks,
// the check repeating k1's id is not listed (the block index takes the first holder of an id),
// and the callout holding a check lends it none of the check's references.
func TestReadAnswersTheTypedBlocksOfAVersionInDocumentOrder(t *testing.T) {
	want := []Block{
		{ID: "r1-ancestor", Type: "requirement"},
		{ID: "r:2", Type: "requirement"},
		{ID: "k1", Type: "check", Refs: []Ref{
			{Attribute: "serves", Text: "dispatch://LEGION-1/spec#b-r1-ancestor", Issue: "LEGION-1", Primary: true, Block: "r1-ancestor"},
			{Attribute: "serves", Text: "dispatch://LEGION-1/artifact/plan-md#b-t1@v3", Issue: "LEGION-1", Slug: "plan-md", Block: "t1", Pin: 3},
		}},
		{ID: "k2", Type: "check", Refs: []Ref{
			{Attribute: "serves", Text: "dispatch://LEGION-1/spec#b-r%3A2@v7", Issue: "LEGION-1", Primary: true, Block: "r:2", Pin: 7},
		}},
		{ID: "c1", Type: "callout"},
		{ID: "k3", Type: "check", Refs: []Ref{
			{Attribute: "serves", Text: "dispatch://ACME/artifact/roadmap#b-goal", Project: "ACME", Slug: "roadmap", Block: "goal"},
		}},
		{ID: "k4", Type: "check"},
	}
	if got := Read(aVersion); !reflect.DeepEqual(got, want) {
		t.Fatalf("Read =\n%#v\nwant\n%#v", got, want)
	}
}

// A plan task's two `block[]` attributes are read by name, `needs` before `serves`, as the block
// index reads them, each attribute's references in the order it holds them.
func TestReadTakesEveryBlockAttributeByName(t *testing.T) {
	const markdown = `:::plan_task{#t2 serves="dispatch://LEGION-1/spec#b-r1 dispatch://LEGION-1/spec#b-r2" needs="dispatch://LEGION-1/artifact/plan-md#b-t1"}` + "\n" +
		"Build the second part.\n:::\n"
	want := []Block{{ID: "t2", Type: "plan_task", Refs: []Ref{
		{Attribute: "needs", Text: "dispatch://LEGION-1/artifact/plan-md#b-t1", Issue: "LEGION-1", Slug: "plan-md", Block: "t1"},
		{Attribute: "serves", Text: "dispatch://LEGION-1/spec#b-r1", Issue: "LEGION-1", Primary: true, Block: "r1"},
		{Attribute: "serves", Text: "dispatch://LEGION-1/spec#b-r2", Issue: "LEGION-1", Primary: true, Block: "r2"},
	}}}
	if got := Read(markdown); !reflect.DeepEqual(got, want) {
		t.Fatalf("Read =\n%#v\nwant\n%#v", got, want)
	}
}

// A typed block the block schema refuses is the literal text Dispatch draws it as, and no typed
// block: here a check whose `serves` holds references not written as Dispatch writes one (an
// issue's primary document spelled `artifact/spec`, a lower-case percent-encoding), and a
// directive of a name the schema does not know. The blocks around them are read as they stand.
func TestReadListsNoBlockTheSchemaRefuses(t *testing.T) {
	const markdown = ":::requirement{#r1}\nThe requirement.\n:::\n\n" +
		`:::check{#k1 serves="dispatch://LEGION-1/artifact/spec#b-r1"}` + "\nNot canonical.\n:::\n\n" +
		`:::check{#k2 serves="dispatch://LEGION-1/spec#b-r%3a1"}` + "\nNot canonical either.\n:::\n\n" +
		":::unknown_block{#u1}\nNo such type.\n:::\n\n" +
		`:::check{#k3 serves="dispatch://LEGION-1/spec#b-r1"}` + "\nCanonical.\n:::\n"
	want := []Block{
		{ID: "r1", Type: "requirement"},
		{ID: "k3", Type: "check", Refs: []Ref{
			{Attribute: "serves", Text: "dispatch://LEGION-1/spec#b-r1", Issue: "LEGION-1", Primary: true, Block: "r1"},
		}},
	}
	if got := Read(markdown); !reflect.DeepEqual(got, want) {
		t.Fatalf("Read =\n%#v\nwant\n%#v", got, want)
	}
}

// A text holding no typed block, the empty text among them, answers none.
func TestReadAnswersNoBlockForATextWithoutTypedBlocks(t *testing.T) {
	for _, markdown := range []string{"", "Just a paragraph.\n", "# A heading\n\n- a list item\n"} {
		if got := Read(markdown); len(got) != 0 {
			t.Errorf("Read(%q) = %#v, want no block", markdown, got)
		}
	}
}
