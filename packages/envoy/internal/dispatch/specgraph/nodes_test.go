package specgraph

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// allKinds is the five nodes of testdata/nodes/all-kinds.yaml, in the order of the file, as Go
// values with no position: a statement, a check, a component, an interface and a term.
func allKinds() []Node {
	return []Node{
		Statement{
			ID:     "a-stored-message-survives-a-restart",
			Text:   "Envoy delivers each stored message to its session after the listener starts again.",
			Kind:   KindBehavior,
			Parent: "envoy-delivers-each-message",
			About:  []string{"envoy-delivery"},
		},
		Check{
			ID:             "a-restart-loses-no-message",
			Serves:         "a-stored-message-survives-a-restart",
			Claim:          "A message that the listener stores before a restart reaches its session after the restart.",
			ProcedureText:  "The program stores one message, starts the listener again and reads the messages of the session.\nIt must find the message one time.",
			FailingExample: "A listener that deletes its stored messages when it starts.",
			Subject:        SubjectCode,
			Procedure:      ProcedureProgram,
			Test:           &Test{Runner: "go", Selector: "github.com/sjawhar/envoy/cmd/listener TestARestartLosesNoMessage", Job: "go-checks"},
		},
		Component{
			ID:      "envoy-delivery",
			Name:    "Envoy delivery",
			Purpose: "It delivers each stored message to the session that it is for.",
			Parent:  "envoy",
		},
		Component{
			ID:        "legion-to-dispatch",
			Name:      "Legion to Dispatch",
			Purpose:   "Legion reads issues and writes results through it.",
			Interface: true,
			Joins:     []string{"legion", "dispatch"},
		},
		Term{ID: "work-unit", Name: "work unit", Definition: "A work unit is work that makes one or more statements true."},
	}
}

// quotedTerm is the term of testdata/nodes/quoted.yaml: a definition whose lines YAML must quote or
// escape, each in the style yaml.v3 picks for it.
var quotedTerm = Term{ID: "quoted", Name: "quoted term", Definition: strings.Join([]string{
	"a: b", "x #y", "no", "2026-11-16", "it's", `say "x"`, "a\tb", "café", "emoji \U0001F600", " lead",
}, "\n")}

// printNode writes each node as the document that a person wrote by hand in testdata/nodes: the
// keys in the order of the schema's table, each list sorted, each text a list of its lines, and a
// component's kind always written.
func TestPrintWritesTheFileForm(t *testing.T) {
	t.Parallel()
	var printed strings.Builder
	for _, node := range allKinds() {
		document, err := printNode(node)
		if err != nil {
			t.Fatalf("printNode(%+v): %v", node, err)
		}
		printed.WriteString(document)
	}
	sameText(t, "all-kinds.yaml", printed.String(), fileText(t, "testdata/nodes/all-kinds.yaml"))

	document, err := printNode(quotedTerm)
	if err != nil {
		t.Fatal(err)
	}
	sameText(t, "quoted.yaml", document, fileText(t, "testdata/nodes/quoted.yaml"))

	// The line ends of YAML 1.1 that YAML 1.2 reads as characters print as escapes in double
	// quotes, so the yaml library of TypeScript reads the same strings (spec-node-schema.test.ts).
	document, err = printNode(Term{ID: "separators", Name: "separators term", Definition: "a\u2028b\nc\u2029d\ne\u0085f"})
	if err != nil {
		t.Fatal(err)
	}
	sameText(t, "separators.yaml", document, fileText(t, "testdata/nodes/separators.yaml"))

	document, err = printNode(hardTextsTerm)
	if err != nil {
		t.Fatal(err)
	}
	sameText(t, "hard-texts.yaml", document, fileText(t, "testdata/nodes/hard-texts.yaml"))
}

// sameText fails the test at the first line where got and want differ.
func sameText(t *testing.T, name, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	gotLines, wantLines := strings.SplitAfter(got, "\n"), strings.SplitAfter(want, "\n")
	for i := range max(len(gotLines), len(wantLines)) {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			t.Fatalf("%s: line %d is %q, want %q", name, i+1, g, w)
		}
	}
}

// fileText is the content of path.
func fileText(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// The texts that YAML must quote or escape: a line that starts or holds one of its marks, a word
// that a YAML 1.1 reader reads as another type, a tab, an emoji and the white space and format
// characters outside ASCII. Each line is one line of a text, so a text of more lines is a list of
// more items.
var hardTexts = []string{
	`a "quoted" word`, "a & b", "a &amp; b", "a &lt; b", "a } b", "a { b", "a # b", "a `code` b",
	"a\u202fb", "a \U0001F600 b", "#a}", "line one\nline two",
	"a: b", "x #y", "- x", "? x", ": x", "|", ">", "'", `"`, "#", "@at", "`", " lead",
	"yes", "no", "Off", "0x1F", "1_000", ".inf", "2026-11-16", "a\tb", "a\u2028b", "a\u2029b", "a\u0085b",
	"=", "<<", "0x_", "1.2.3", "2026-11-16 10:00:00 +01:00",
	"a\ufeffb", "a\u00a0", "a\x01b", "emoji \U0001F600", "line one\nline two\nline three",
}

// timedTexts are texts that a YAML 1.1 reader (PyYAML) reads as another type and that hardTexts
// does not hold: a date with a time in each of its spellings.
var timedTexts = []string{
	"2026-11-16T10:00:00", "2026-11-16 10:00:00 Z", "2026-11-16T10:00:00 +01:00", "2026-1-5 10:00:00",
}

// hardTextsTerm is the term of testdata/nodes/hard-texts.yaml, whose definition holds each line of
// hardTexts and timedTexts: the printer writes it (TestPrintWritesTheFileForm), and a person wrote
// the data of hard-texts.decoded.json, which yaml.v3 (TestNodesDecodeToTheirData) and the TypeScript
// yaml library (spec-node-schema.test.ts) each read the file as, so both read the texts that the
// printer quotes as the same strings.
var hardTextsTerm = Term{ID: "hard-texts", Name: "hard texts", Definition: uniqueLines(slices.Concat(hardTexts, timedTexts))}

// uniqueLines is each line of texts once, in the order of its first place, one line of a text each:
// a list of a node holds no line two times.
func uniqueLines(texts []string) string {
	var lines []string
	for _, text := range texts {
		for line := range strings.SplitSeq(text, "\n") {
			if !slices.Contains(lines, line) {
				lines = append(lines, line)
			}
		}
	}
	return strings.Join(lines, "\n")
}

// Each node prints as a document that the reader reads back as the same node, with the same digest
// and no problem, whatever its texts hold; the lists come back sorted. Each scalar is one line: no
// document holds U+0085, U+2028 or U+2029 as it is, since YAML 1.1 (yaml.v3) reads each as a line
// end and YAML 1.2 (the TypeScript yaml library) does not.
func TestPrintThenReadKeepsEveryText(t *testing.T) {
	t.Parallel()
	for _, text := range hardTexts {
		oneLine := strings.ReplaceAll(text, "\n", " ")
		for _, node := range []Node{
			Statement{ID: "a-statement", Text: text, Kind: KindStructure, Parent: "the-parent", Serves: []string{"two", "one"}, About: []string{"envoy"}},
			Check{
				ID: "a-check", Serves: "a-statement", Claim: oneLine, ProcedureText: text, FailingExample: text,
				Subject: SubjectCode, Procedure: ProcedureProgram, TimeLimit: 90 * time.Minute,
				Test: &Test{Runner: "go", Selector: oneLine, Job: oneLine},
			},
			Check{ID: "a-query", Serves: "a-statement", Claim: oneLine, ProcedureText: text, FailingExample: text, Subject: SubjectGraph, Procedure: ProcedureQuery},
			Component{ID: "a-component", Name: oneLine, Purpose: text, Parent: "envoy"},
			Component{ID: "an-interface", Name: oneLine, Purpose: text, Interface: true, Joins: []string{"legion", "dispatch"}},
			Term{ID: "a-term", Name: oneLine, Definition: text},
		} {
			document, err := printNode(node)
			if err != nil {
				t.Errorf("printNode(%+v): %v", node, err)
				continue
			}
			if strings.ContainsAny(document, "\u0085\u2028\u2029") {
				t.Errorf("printNode(%+v) = %q: a scalar holds a line end of YAML 1.1 as it is", node, document)
			}
			tree, problems := Parse(map[string][]byte{"x.yaml": []byte(document)})
			if len(problems) != 0 {
				t.Errorf("Parse(printNode(%+v)) = Parse(%q): problems %+v", node, document, problems)
				continue
			}
			got, want := onlyTreeNode(t, tree), sortedLists(node)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Parse(%q):\n got %+v\nwant %+v", document, got, want)
			}
			if nodeDigest(got) != nodeDigest(node) {
				t.Errorf("Parse(%q) has another digest than the node it printed", document)
			}
		}
	}
}

// nonStringPlain is the one rule of the printer for a text that another resolver types: it matches
// each implicit type of YAML 1.1 (yaml.org/type and PyYAML's resolver) and of the core schema of
// YAML 1.2, and no text that both read as a string. A text it matches prints double-quoted; a
// text it does not match prints as yaml.v3 picks, plain for each row below.
func TestNonStringPlainMatchesEachImplicitType(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		kind  string
		texts []string
		typed bool
	}{
		{"null", []string{"~", "null", "Null", "NULL"}, true},
		{"bool", []string{"y", "Y", "yes", "Yes", "YES", "n", "N", "no", "No", "NO", "true", "True", "TRUE", "false", "False", "FALSE", "on", "On", "ON", "off", "Off", "OFF"}, true},
		{"int in base 2", []string{"0b1010", "-0b1_0", "+0b0"}, true},
		{"int in base 8", []string{"017", "-0_17", "0o17", "0_"}, true},
		{"int in base 10", []string{"0", "12", "-12", "+1_000", "012345678", "09"}, true},
		{"int in base 16", []string{"0x1F", "-0xff_ff", "0x_"}, true},
		{"int in base 60", []string{"190:20:30", "-1:30"}, true},
		{"float", []string{"1.5", "-1.5", "1.", ".5", "+.5e3", "1e3", "1E-3", "1_000.5", "1.5_", "1.2.3", ".", "1.0e+3"}, true},
		{"float in base 60", []string{"190:20:30.15", "1:30.5", "0:7", "03:17", "-0:0"}, true},
		{"infinity and NaN", []string{".inf", "-.Inf", "+.INF", ".nan", ".NaN", ".NAN"}, true},
		{"timestamp", []string{"2026-11-16", "2026-11-16T10:00:00", "2026-11-16t10:00:00Z", "2026-1-5 10:00:00", "2026-11-16 10:00:00 +01:00",
			"2026-11-16 10:00:00 Z", "2026-11-16T10:00:00 +01:00", "2026-11-16T10:00:00.5-1", "2026-11-16\t10:00:00"}, true},
		{"the merge key", []string{"<<"}, true},
		{"the value key", []string{"="}, true},
		{"a string", []string{"Legion", "a = b", "<<<", "==", "1a", "0x", "0b", "0bx", "1:60", "yesterday", "nothing", "inf", "nan", ".info",
			"2026-11-16x", "2026-11-16 10:00", "v1.2.3", "1e", "e3", "1_000 words", "work unit"}, false},
	} {
		for _, text := range row.texts {
			if got := nonStringPlain.MatchString(text); got != row.typed {
				t.Errorf("nonStringPlain(%q), a %s: %t, want %t", text, row.kind, got, row.typed)
			}
			document, err := printNode(Term{ID: "typed", Name: "typed term", Definition: text})
			if err != nil {
				t.Fatalf("printNode(%q): %v", text, err)
			}
			plain, quoted := "  - "+text+"\n", "  - "+strconv.Quote(text)+"\n"
			if row.typed && !strings.Contains(document, quoted) || !row.typed && !strings.Contains(document, plain) {
				t.Errorf("%q, a %s, prints as %q", text, row.kind, document)
			}
		}
	}
}

// FuzzPrintThenRead holds the printer to yaml.v3's quoting: a term whose definition is one line of
// any text that a line may hold (UTF-8, not empty, no NUL, CR or LF) prints, and reads back as the
// same term with no problem.
func FuzzPrintThenRead(f *testing.F) {
	for _, text := range hardTexts {
		for line := range strings.SplitSeq(text, "\n") {
			f.Add(line)
		}
	}
	f.Fuzz(func(t *testing.T, line string) {
		if line == "" || !utf8.ValidString(line) || strings.ContainsAny(line, "\x00\r\n") {
			t.Skip("not a line")
		}
		term := Term{ID: "a-term", Name: "a term", Definition: line}
		document, err := printNode(term)
		if err != nil {
			t.Fatalf("printNode(%q): %v", line, err)
		}
		tree, problems := Parse(map[string][]byte{"x.yaml": []byte(document)})
		if len(problems) != 0 || len(tree.Terms) != 1 {
			t.Fatalf("Parse(%q): %+v, problems %+v", document, tree, problems)
		}
		if got := tree.Terms[0].Definition; got != line {
			t.Fatalf("Parse(%q): the definition reads back as %q, want %q", document, got, line)
		}
	})
}

// Print refuses a node that no document can hold as the same node, naming the node and the field,
// rather than write a document that Parse reads as another node or as no node of the files' form.
func TestPrintRefusesUnprintable(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		node  interface{ Print() (string, error) }
		field string
	}{
		{name: "a node with no id", node: Term{Name: "term", Definition: "A term."}, field: "id"},
		{name: "a claim of two lines", node: Check{ID: "a-check", Serves: "s", Claim: "One.\nTwo.", Subject: SubjectCode, Procedure: ProcedureAgent}, field: "claim"},
		{name: "a text with an empty line", node: Component{ID: "a-component", Name: "C", Purpose: "It runs.\n\nIt stops."}, field: "purpose"},
		{name: "a name that holds a line break", node: Component{ID: "a-component", Name: "C\nD", Purpose: "It runs."}, field: "name"},
		{name: "a carriage return", node: Statement{ID: "a-statement", Kind: KindBehavior, Text: "One.\rTwo."}, field: "text"},
		{name: "a NUL byte", node: Term{ID: "a-term", Name: "term", Definition: "A te\x00rm."}, field: "definition"},
		{name: "a byte that is not UTF-8", node: Term{ID: "a-term", Name: "caf\xe9", Definition: "A term."}, field: "name"},
		{name: "a test part that names nothing", node: Check{ID: "a-check", Serves: "s", Claim: "One.", Subject: SubjectCode, Procedure: ProcedureProgram, Test: &Test{}}, field: "test"},
		{name: "a negative time limit", node: Check{ID: "a-check", Serves: "s", Claim: "One.", Subject: SubjectCode, Procedure: ProcedureAgent, TimeLimit: -time.Hour}, field: "time_limit"},
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

// A digest holds no file and no position: the same documents, each in a file of its own and in
// the reverse order, keep every digest, and one more word in one statement changes that
// statement's digest alone.
func TestDigestIgnoresTheFileAndTheOrder(t *testing.T) {
	t.Parallel()
	all := fileText(t, "testdata/nodes/all-kinds.yaml")
	documents := splitDocuments(all)
	reversed := map[string][]byte{}
	for i, document := range documents {
		reversed[fmt.Sprintf("z%d.yaml", len(documents)-i)] = []byte(document)
	}
	one, problems := Parse(map[string][]byte{"all-kinds.yaml": []byte(all)})
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

	changed, problems := Parse(map[string][]byte{"all-kinds.yaml": []byte(strings.Replace(all, "after the listener starts again.", "after the listener starts again soon.", 1))})
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	before, after := digests(one), digests(changed)
	for key := range before {
		if moved := before[key] != after[key]; moved != (key == "statement a-stored-message-survives-a-restart") {
			t.Errorf("%s: digest changed %t", key, moved)
		}
	}
}

// onlyTreeNode is the one node tree holds, with no position.
func onlyTreeNode(t *testing.T, tree Tree) Node {
	t.Helper()
	nodes := Nodes(tree)
	if len(nodes) != 1 {
		t.Fatalf("tree %+v holds %d nodes, want one", tree, len(nodes))
	}
	return nodes[0].WithPos(Position{})
}

// sortedLists is node with each of its lists sorted, as the reader gives a printed node back.
func sortedLists(node Node) Node {
	sorted := func(list []string) []string { return slices.Sorted(slices.Values(list)) }
	switch n := node.(type) {
	case Statement:
		n.Serves, n.About = sorted(n.Serves), sorted(n.About)
		return n
	case Component:
		n.Joins = sorted(n.Joins)
		return n
	}
	return node
}

// nodeDigest is the digest of node.
func nodeDigest(node Node) string {
	return node.Digest()
}

// typedPlain is each pattern by which a YAML reader reads a plain scalar as another type than a
// string: PyYAML 6.0.1's implicit resolvers for YAML 1.1 (yaml/resolver.py, written without the
// verbose mode), and the YAML 1.2 core schema's, which the TypeScript yaml library reads.
var typedPlain = []struct {
	reader, tag string
	re          *regexp.Regexp
}{
	{"YAML 1.1", "null", regexp.MustCompile(`^(?:~|null|Null|NULL|)$`)},
	{"YAML 1.1", "bool", regexp.MustCompile(`^(?:yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)$`)},
	{"YAML 1.1", "int", regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`)},
	{"YAML 1.1", "float", regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)},
	{"YAML 1.1", "timestamp", regexp.MustCompile(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`)},
	{"YAML 1.1", "merge", regexp.MustCompile(`^<<$`)},
	{"YAML 1.1", "value", regexp.MustCompile(`^=$`)},
	{"YAML 1.1", "yaml", regexp.MustCompile(`^(?:!|&|\*)$`)},
	{"YAML 1.2 core", "null", regexp.MustCompile(`^(?:~|null|Null|NULL|)$`)},
	{"YAML 1.2 core", "bool", regexp.MustCompile(`^(?:true|True|TRUE|false|False|FALSE)$`)},
	{"YAML 1.2 core", "int", regexp.MustCompile(`^(?:[-+]?[0-9]+|0o[0-7]+|0x[0-9a-fA-F]+)$`)},
	{"YAML 1.2 core", "float", regexp.MustCompile(`^(?:[-+]?(?:\.[0-9]+|[0-9]+(?:\.[0-9]*)?)(?:[eE][-+]?[0-9]+)?|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)},
}

// The printer writes a scalar plain only when yaml.v3 reads the plain form back as the same string,
// and a YAML 1.1 reader resolves more plain scalars to other types: "=" (value) and "<<" (merge)
// make PyYAML refuse the whole file, and "0x_" or "2026-11-16T10:00:00" read as an error or a
// datetime. So no text of hardTexts or timedTexts prints as a plain scalar that a pattern of
// typedPlain matches.
func TestPrintWritesNoPlainScalarThatAYAML11ReaderTypes(t *testing.T) {
	t.Parallel()
	for _, text := range slices.Concat(hardTexts, timedTexts) {
		document, err := printNode(Term{ID: "typed", Name: "typed term", Definition: text})
		if err != nil {
			t.Fatalf("printNode(%q): %v", text, err)
		}
		var root yaml.Node
		if err := yaml.Unmarshal([]byte(document), &root); err != nil {
			t.Fatalf("the document of %q does not read: %v", text, err)
		}
		var walk func(n *yaml.Node)
		walk = func(n *yaml.Node) {
			if n.Kind == yaml.ScalarNode && n.Style == 0 {
				for _, p := range typedPlain {
					if p.re.MatchString(n.Value) {
						t.Errorf("%q prints as the plain scalar %q, which a %s reader reads as a %s, not a string", text, n.Value, p.reader, p.tag)
					}
				}
			}
			for _, child := range n.Content {
				walk(child)
			}
		}
		walk(&root)
	}
}

// Nodes is each node of a tree, its statements, checks, components and terms, each list in its
// order, as a Node: its id, its position, its kind as a person names it, the same node at another
// position, and its document, which Parse reads back as the node. So a caller of any package reads
// a node of any kind with no switch on its type.
func TestNodesGivesEachNodeOfTheTreeAsANode(t *testing.T) {
	t.Parallel()
	at := func(line int) Position { return Position{File: "spec.yaml", Line: line} }
	tree := Tree{
		Statements: []Statement{{ID: "a-statement", Text: "Envoy keeps it.", Kind: KindBehavior, Pos: at(1)}},
		Checks: []Check{{ID: "a-check", Serves: "a-statement", Subject: SubjectCode, Procedure: ProcedureAgent, Claim: "It keeps it.",
			ProcedureText: "An agent reads it.", FailingExample: "A listener that loses it.", Pos: at(7)}},
		Components: []Component{{ID: "a-part", Name: "a part", Purpose: "It holds a part.", Pos: at(20)},
			{ID: "a-joint", Name: "a joint", Purpose: "It joins parts.", Interface: true, Joins: []string{"a-part", "b-part"}, Pos: at(26)}},
		Terms: []Term{{ID: "a-term", Name: "a term", Definition: "A term is a word.", Pos: at(34)}},
	}
	var got []string
	for _, n := range Nodes(tree) {
		got = append(got, fmt.Sprintf("%s %s %d", n.KindName(), n.NodeID(), n.NodePos().Line))
		if bare := n.WithPos(Position{}); bare.NodePos() != (Position{}) || bare.NodeID() != n.NodeID() {
			t.Errorf("%s at no position is %+v", n.NodeID(), bare)
		}
		document, err := n.Print()
		if err != nil {
			t.Fatal(err)
		}
		read, problems := Parse(map[string][]byte{"spec.yaml": []byte(document)})
		if len(problems) != 0 || len(Nodes(read)) != 1 || !reflect.DeepEqual(Nodes(read)[0].WithPos(n.NodePos()), n) {
			t.Errorf("%s prints %q, which Parse reads as %+v with %+v", n.NodeID(), document, Nodes(read), problems)
		}
	}
	want := []string{"statement a-statement 1", "check a-check 7", "component a-part 20", "interface a-joint 26", "term a-term 34"}
	if !slices.Equal(got, want) {
		t.Errorf("Nodes gives %q, want %q", got, want)
	}
}
