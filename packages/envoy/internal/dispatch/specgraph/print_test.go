package specgraph

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Each node prints as a block that Parse reads back as the same node, whatever its texts hold, and
// the printed block keeps the node's digest. An approved text that holds a quote, an ampersand or a
// character reference must be written into the files exactly.
func TestPrintParseRoundTripsEveryText(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		`a "quoted" word`, "a & b", "a &amp; b", "a &lt; b", "a } b", "a { b", "a # b", "a `code` b",
		"a\u202fb", "a \U0001F600 b", "#a}", "line one\nline two",
	} {
		oneLine := strings.ReplaceAll(text, "\n", " ")
		nodes := []interface {
			Print() (string, error)
			Digest() string
		}{
			Statement{ID: "a-statement", Text: text, Kind: KindStructure, Parent: "the-parent", Serves: []string{"one", "two"}, About: []string{"envoy"}},
			Check{
				ID: "a-check", Serves: "a-statement", Claim: oneLine, ProcedureText: text, FailingExample: text,
				Subject: SubjectCode, Procedure: ProcedureProgram, TimeLimit: 90 * time.Minute,
				Test: &Test{Runner: "go", Selector: "example.com/p TestA", Job: "go-checks"},
			},
			Check{ID: "a-query", Serves: "a-statement", Claim: oneLine, ProcedureText: text, Subject: SubjectGraph, Procedure: ProcedureQuery},
			Component{ID: "a-component", Name: oneLine, Purpose: text, Parent: "envoy"},
			Component{ID: "an-interface", Name: oneLine, Purpose: text, Interface: true, Joins: []string{"dispatch", "legion"}},
			Term{ID: "a-term", Name: oneLine, Definition: text},
		}
		for _, node := range nodes {
			printed, err := node.Print()
			if err != nil {
				t.Errorf("Print(%+v): %v", node, err)
				continue
			}
			tree, problems := Parse(map[string][]byte{"x.md": []byte(printed)})
			if len(problems) != 0 {
				t.Errorf("Parse(Print(%+v)) = Parse(%q): problems %+v", node, printed, problems)
				continue
			}
			got := onlyNode(t, tree)
			if !reflect.DeepEqual(got, node) {
				t.Errorf("Parse(%q):\n got %+v\nwant %+v", printed, got, node)
			}
			if got.(interface{ Digest() string }).Digest() != node.Digest() {
				t.Errorf("Parse(%q) has another digest than the node it printed", printed)
			}
		}
	}
}

// Print writes the attributes in the grammar's order, leaves out each empty one, sorts each list
// and writes & and " in a value as character references.
func TestPrintWritesTheCanonicalBlock(t *testing.T) {
	t.Parallel()
	printed, err := Check{
		ID: "a-check", Serves: "a-statement", Claim: "The page shows each statement.",
		ProcedureText: "An agent reads the page.\nIt lists each statement.", Subject: SubjectProduction, Procedure: ProcedureAgent,
	}.Print()
	if err != nil {
		t.Fatal(err)
	}
	want := ":::check{#a-check serves=\"a-statement\" subject=\"production\" procedure=\"agent\"}\n" +
		"Claim: The page shows each statement.\nProcedure: An agent reads the page.\nIt lists each statement.\n:::\n"
	if printed != want {
		t.Errorf("Print:\n got %q\nwant %q", printed, want)
	}
	printed, err = Component{ID: "an-interface", Name: `"A" & B`, Purpose: "It joins two parts.", Interface: true, Joins: []string{"legion", "dispatch"}}.Print()
	if err != nil {
		t.Fatal(err)
	}
	if want := ":::component{#an-interface name=\"&quot;A&quot; &amp; B\" kind=\"interface\" joins=\"dispatch legion\"}\nIt joins two parts.\n:::\n"; printed != want {
		t.Errorf("Print:\n got %q\nwant %q", printed, want)
	}
}

// Print refuses a node that no block can hold, naming the node and the field, rather than write a
// block that Parse reads as another node.
func TestPrintRefusesUnprintable(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		node  interface{ Print() (string, error) }
		field string
	}{
		{name: "a text line that starts with :::", node: Statement{ID: "a-statement", Kind: KindBehavior, Text: "The page shows it.\n::: no"}, field: "text"},
		{name: "a claim of two lines", node: Check{ID: "a-check", Serves: "s", Claim: "One.\nTwo.", Subject: SubjectCode, Procedure: ProcedureAgent}, field: "claim"},
		{name: "a procedure line that starts a field", node: Check{ID: "a-check", Serves: "s", Claim: "One.", ProcedureText: "It reads.\nFailing example: none", Subject: SubjectCode, Procedure: ProcedureAgent}, field: "procedure"},
		{name: "a text line that ends in white space", node: Term{ID: "a-term", Name: "term", Definition: "A term. "}, field: "definition"},
		{name: "a text that starts with a blank line", node: Component{ID: "a-component", Name: "C", Purpose: "\nIt runs."}, field: "purpose"},
		{name: "a name that holds a line break", node: Component{ID: "a-component", Name: "C\nD", Purpose: "It runs."}, field: "name"},
		{name: "a carriage return", node: Statement{ID: "a-statement", Kind: KindBehavior, Text: "One.\rTwo."}, field: "text"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			printed, err := row.node.Print()
			if !errors.Is(err, ErrUnprintable) {
				t.Fatalf("Print = %q, %v; want an error wrapping ErrUnprintable", printed, err)
			}
			id := reflect.ValueOf(row.node).FieldByName("ID").String()
			if !strings.Contains(err.Error(), `"`+id+`"`) || !strings.Contains(err.Error(), row.field) {
				t.Errorf("Print: %q, want an error naming %q and the field %s", err, id, row.field)
			}
		})
	}
}

// A digest holds no file and no position: the same blocks in other files and in another order
// keep every digest, and one more word in one statement changes that statement's digest alone.
func TestDigestIgnoresTheFileAndTheOrder(t *testing.T) {
	t.Parallel()
	all := string(readFiles(t, "testdata/blocks")["all-kinds.md"])
	blocks := strings.SplitAfter(all, ":::\n")
	blocks = blocks[:len(blocks)-1]
	reversed := map[string][]byte{}
	for i, block := range blocks {
		reversed[string(rune('z'-i))+".md"] = []byte(strings.TrimPrefix(block, "\n"))
	}
	one, problems := Parse(map[string][]byte{"all-kinds.md": []byte(all)})
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	other, problems := Parse(reversed)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	if got, want := digests(other), digests(one); !reflect.DeepEqual(got, want) {
		t.Errorf("digests in other files %v, want %v", got, want)
	}
	if other.Statements[0].Pos == one.Statements[0].Pos {
		t.Fatalf("the statement is at %v in both trees; the test moves nothing", one.Statements[0].Pos)
	}

	changed, problems := Parse(map[string][]byte{"all-kinds.md": []byte(strings.Replace(all, "after the listener starts again.", "after the listener starts again soon.", 1))})
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	before, after := digests(one), digests(changed)
	for key := range before {
		if moved := before[key] != after[key]; moved != (key == "requirement a-stored-message-survives-a-restart") {
			t.Errorf("%s: digest changed %t", key, moved)
		}
	}
}

// onlyNode is the one node tree holds.
func onlyNode(t *testing.T, tree Tree) interface {
	Print() (string, error)
	Digest() string
} {
	t.Helper()
	var nodes []interface {
		Print() (string, error)
		Digest() string
	}
	for _, s := range tree.Statements {
		s.Pos = Position{}
		nodes = append(nodes, s)
	}
	for _, c := range tree.Checks {
		c.Pos = Position{}
		nodes = append(nodes, c)
	}
	for _, c := range tree.Components {
		c.Pos = Position{}
		nodes = append(nodes, c)
	}
	for _, term := range tree.Terms {
		term.Pos = Position{}
		nodes = append(nodes, term)
	}
	if len(nodes) != 1 {
		t.Fatalf("tree %+v holds %d nodes, want one", tree, len(nodes))
	}
	return nodes[0]
}
