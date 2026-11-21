package specgraph

import (
	"bytes"
	"encoding/json"
	"flag"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"gopkg.in/yaml.v3"
)

// Each document of the hand-written files of testdata/nodes reads, with yaml.v3 and as JSON, as
// the data that a person wrote for it in the .decoded.json file beside it, and the schema takes
// that data (v-second-library). The TypeScript test of the schema holds yaml's reading of the same
// files to the same data and the same schema.
func TestNodesDecodeToTheirData(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"all-kinds", "quoted", "separators"} {
		documents, problems, _ := readDocuments(name+".yaml", []byte(fileText(t, "testdata/nodes/"+name+".yaml")))
		if len(problems) != 0 {
			t.Fatalf("%s.yaml: problems %+v", name, problems)
		}
		var want []any
		if err := json.Unmarshal([]byte(fileText(t, "testdata/nodes/"+name+".decoded.json")), &want); err != nil {
			t.Fatal(err)
		}
		if len(documents) != len(want) {
			t.Fatalf("%s.yaml holds %d documents, and %s.decoded.json %d objects", name, len(documents), name, len(want))
		}
		for i, document := range documents {
			var got any
			if err := json.Unmarshal(document.data, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want[i]) {
				t.Errorf("%s.yaml, the document at line %d:\n got %s\nwant the object %d of %s.decoded.json, %v", name, document.line, document.data, i+1, name, want[i])
			}
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(document.data))
			if err != nil {
				t.Fatal(err)
			}
			if err := nodeSchema().Validate(inst); err != nil {
				t.Errorf("%s.yaml, the document at line %d: the schema refuses it: %v", name, document.line, err)
			}
		}
	}
}

// updateFixtures is this package's one -update flag: it rewrites decodedFixtures from the YAML
// fixtures, `go test ./internal/dispatch/specgraph -run TestEachYAMLFixtureDecodesAsItsData
// -update`, after which `bunx biome format --write` gives the file biome's form.
var updateFixtures = flag.Bool("update", false, "rewrite testdata/fixtures.decoded.json from the YAML fixtures of testdata")

// decodedFixtures is the data of each document of each YAML fixture of testdata, by the path of
// the fixture under testdata, as yaml.v3 reads it, and whether the schema takes it. The TypeScript
// test of the schema holds yaml's reading of each of those files to it (R5).
const decodedFixtures = "testdata/fixtures.decoded.json"

// decodedDocument is one document of decodedFixtures: its data, and whether the schema takes it.
type decodedDocument struct {
	Data   json.RawMessage `json:"data"`
	Schema bool            `json:"schema"`
}

// Each YAML fixture of testdata, each file that print wrote for the tests and the hand-written
// nodes, reads with yaml.v3 as decodedFixtures holds it, document by document, and the schema
// gives the verdict that it holds. The file names each fixture, and no other.
func TestEachYAMLFixtureDecodesAsItsData(t *testing.T) {
	t.Parallel()
	got := map[string][]decodedDocument{}
	err := filepath.WalkDir("testdata", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, FileSuffix) {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		documents, problems, _ := readDocuments(path, content)
		if len(problems) != 0 {
			t.Errorf("%s: yaml.v3 does not read each document: %+v", path, problems)
		}
		name := filepath.ToSlash(strings.TrimPrefix(path, "testdata"+string(filepath.Separator)))
		got[name] = []decodedDocument{}
		for _, d := range documents {
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(d.data))
			if err != nil {
				return err
			}
			got[name] = append(got[name], decodedDocument{Data: d.data, Schema: nodeSchema().Validate(inst) == nil})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if *updateFixtures {
		js, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(decodedFixtures, append(js, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	var want map[string][]decodedDocument
	if err := json.Unmarshal([]byte(fileText(t, decodedFixtures)), &want); err != nil {
		t.Fatal(err)
	}
	if names, wantNames := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)); !slices.Equal(names, wantNames) {
		t.Fatalf("the YAML fixtures are %q, and %s names %q (run with -update when the change is deliberate)", names, decodedFixtures, wantNames)
	}
	for name, documents := range got {
		if len(documents) != len(want[name]) {
			t.Errorf("%s holds %d documents, and %s %d", name, len(documents), decodedFixtures, len(want[name]))
			continue
		}
		for i, d := range documents {
			var data, wantData any
			if err := json.Unmarshal(d.Data, &data); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(want[name][i].Data, &wantData); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(data, wantData) || d.Schema != want[name][i].Schema {
				t.Errorf("%s, document %d: yaml.v3 reads %s (schema %t), and %s holds %s (schema %t)", name, i+1, d.Data, d.Schema, decodedFixtures, want[name][i].Data, want[name][i].Schema)
			}
		}
	}
}

// The reader reads each kind of node from the hand-written file, with each node at the line of its
// document's "---", and finds no problem in it.
func TestReadReadsEachNodeKind(t *testing.T) {
	t.Parallel()
	all := fileText(t, "testdata/nodes/all-kinds.yaml")
	tree, problems := Parse(map[string][]byte{"all-kinds.yaml": []byte(all)})
	if len(problems) != 0 {
		t.Fatalf("Parse: problems %+v, want none", problems)
	}
	nodes := allKinds()
	at := func(line int) Position { return Position{File: "all-kinds.yaml", Line: line} }
	statement, check, component, iface, term := nodes[0].(Statement), nodes[1].(Check), nodes[2].(Component), nodes[3].(Component), nodes[4].(Term)
	statement.Pos, check.Pos, component.Pos, iface.Pos, term.Pos = at(1), at(10), at(26), at(34), at(44)
	iface.Joins = []string{"dispatch", "legion"}
	want := Tree{
		Statements: []Statement{statement},
		Checks:     []Check{check},
		Components: []Component{component, iface},
		Terms:      []Term{term},
	}
	if !reflect.DeepEqual(tree, want) {
		t.Errorf("Parse:\n got %+v\nwant %+v", tree, want)
	}

	// The control: the same documents in the reverse order read with no problem and keep each
	// digest.
	documents := splitDocuments(all)
	slices.Reverse(documents)
	other, problems := Parse(map[string][]byte{"reversed.yaml": []byte(strings.Join(documents, ""))})
	if len(problems) != 0 {
		t.Fatalf("Parse(reversed): problems %+v, want none", problems)
	}
	if got, want := digests(other), digests(tree); !reflect.DeepEqual(got, want) {
		t.Errorf("digests of the reversed file %v, want %v", got, want)
	}
	if other.Statements[0].Pos == tree.Statements[0].Pos {
		t.Fatalf("the statement is at %v in both trees; the control moves nothing", tree.Statements[0].Pos)
	}
}

// The reader refuses each fault of a file with exactly one problem: its line, its node and its
// rule. A fault of the YAML or of its JSON form gives no node of that document; a fault that only
// the schema finds gives the node when its keys have the schema's types; and each other spelling
// of a node is not-printed.
func TestReadRefuses(t *testing.T) {
	t.Parallel()
	const term = "---\ntype: term\nid: work-unit\nname: work unit\ndefinition:\n  - A work unit is work.\n"
	const program = "---\ntype: check\nid: a-restart-keeps-the-claim\nserves: a-claim-survives-a-restart\nsubject: code\nprocedure: program\n" +
		"test:\n  runner: go\n  selector: example.com/acme/daemon TestARestartKeepsTheClaim\n  job: go-checks\n" +
		"time_limit: %s\nclaim: The daemon reads the claim after it starts again.\n" +
		"procedure_text:\n  - The program claims one issue and starts the daemon again.\nfailing_example:\n  - A daemon with no store.\n"
	const query = "---\ntype: check\nid: each-claim-is-in-the-graph\nserves: a-claim-survives-a-restart\nsubject: graph\nprocedure: query\n" +
		"test:\n  runner: go\n  selector: example.com/acme/daemon TestARestartKeepsTheClaim\n  job: go-checks\n" +
		"claim: The graph holds each claim.\nprocedure_text:\n  - A query reads the claims of the graph.\nfailing_example:\n  - A graph with no claims.\n"
	const structure = "---\ntype: requirement\nid: legion-reads-issues-only-through-dispatch\nkind: structure\nabout:\n  - legion\n  - dispatch\n" +
		"text:\n  - Legion reads each issue through Dispatch.\n"
	notPrinted := func(line int, node, got, want, difference string) Problem {
		quoted := func(text, none string) string {
			if text == "" {
				return none
			}
			return strconv.Quote(text)
		}
		detail := "not-printed: line " + strconv.Itoa(line) + " is " + quoted(got, "past the end of the file") + ", and print writes " +
			quoted(want, "nothing") + " there (" + difference + "); spec-check print writes each node in that form"
		return Problem{Pos: Position{"x.yaml", line}, Node: node, Rule: "not-printed", Detail: detail, Effect: LosesNode}
	}
	for _, row := range []struct {
		name  string
		text  string
		want  Problem
		also  *Problem // a second problem, after want
		nodes int      // how many nodes the tree holds
	}{
		{name: "an open flow list", text: "---\ntype: [\n",
			want: Problem{Pos: Position{"x.yaml", 2}, Node: "-", Rule: "bad-yaml", Detail: "bad-yaml: line 2: did not find expected node content", Effect: LosesNode}},
		{name: "a byte that is not UTF-8", text: strings.Replace(term, "work unit\n", "caf\xe9 unit\n", 1),
			want: Problem{Pos: Position{"x.yaml", 4}, Node: "-", Rule: "bad-yaml", Detail: "bad-yaml: invalid trailing UTF-8 octet", Effect: LosesNode}},
		{name: "a NUL byte", text: strings.Replace(term, "work unit\n", "work\x00unit\n", 1),
			want: Problem{Pos: Position{"x.yaml", 4}, Node: "-", Rule: "bad-yaml", Detail: "bad-yaml: control characters are not allowed", Effect: LosesNode}},
		// The reader of yaml.v3 names no line for a byte it refuses; the problem is at the line of
		// that byte, also in a later document of a file larger than one read of the reader. The
		// reader reads ahead of the parser, so the tree keeps the 24 nodes the parser had finished.
		{name: "a byte that is not UTF-8 in the 30th document", text: strings.Repeat(term, 29) + strings.Replace(term, "work unit\n", "caf\xe9 unit\n", 1), nodes: 24,
			want: Problem{Pos: Position{"x.yaml", 178}, Node: "-", Rule: "bad-yaml", Detail: "bad-yaml: invalid trailing UTF-8 octet", Effect: LosesNode}},
		{name: "a control character in the 30th document", text: strings.Repeat(term, 29) + strings.Replace(term, "work unit\n", "work\x01unit\n", 1), nodes: 24,
			want: Problem{Pos: Position{"x.yaml", 178}, Node: "-", Rule: "bad-yaml", Detail: "bad-yaml: control characters are not allowed", Effect: LosesNode}},
		// A fault of the YAML on line 1, which yaml.v3 names with no line, keeps line 1 when a byte
		// far after it is not UTF-8: only a fault of the reader takes the line of that byte.
		{name: "a fault on line 1 before a byte that is not UTF-8", text: "@x\n" + strings.Repeat(term, 29) + strings.Replace(term, "work unit\n", "caf\xe9 unit\n", 1),
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "-", Rule: "bad-yaml", Detail: "bad-yaml: found character that cannot start any token", Effect: LosesNode}},
		{name: "a key written two times", text: strings.Replace(term, "type: term\n", "type: term\ntype: term\n", 1),
			want: Problem{Pos: Position{"x.yaml", 3}, Node: "-", Rule: "bad-yaml", Detail: `bad-yaml: line 3: mapping key "type" already defined at line 2`, Effect: LosesNode}},
		{name: "a key that is a number", text: "---\n1: x\n",
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "-", Rule: "bad-yaml", Detail: "bad-yaml: the document holds a value that JSON cannot hold", Effect: LosesNode}},
		{name: "an empty file", text: "",
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "-", Rule: "no-node", Detail: "no-node: a spec file holds one or more nodes", Effect: KeepsEachNode}},
		{name: "a key that the node does not take", text: term + "surprise: true\n", nodes: 1,
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "work-unit", Rule: "schema", Detail: "schema: node: additional properties 'surprise' not allowed", Effect: LosesNode}},
		{name: "a type that is no node kind", text: strings.Replace(term, "type: term", "type: plan_task", 1),
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "work-unit", Rule: "schema", Detail: "schema: /type: value must be one of 'requirement', 'check', 'component', 'term'", Effect: LosesNode}},
		{name: "a text that is a string", text: "---\ntype: requirement\nid: a-claim-survives-a-restart\nkind: behavior\ntext: Legion keeps each claim.\n",
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "a-claim-survives-a-restart", Rule: "schema", Detail: "schema: /text: got string, want array", Effect: LosesNode}},
		// A null says nothing, and yaml.v3 reads it as the zero value of its place: the reader refuses
		// it at each place, the document itself included, before the schema, and gives no node.
		{name: "a document that is only its --- line", text: "---\n",
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "-", Rule: "bad-yaml", Detail: "bad-yaml: the document is null", Effect: LosesNode}},
		{name: "a value that is null", text: strings.Replace(term, "name: work unit", "name: null", 1),
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "-", Rule: "bad-yaml", Detail: `bad-yaml: the value at "/name" is null`, Effect: LosesNode}},
		{name: "a value left empty", text: strings.Replace(term, "name: work unit", "name:", 1),
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "-", Rule: "bad-yaml", Detail: `bad-yaml: the value at "/name" is null`, Effect: LosesNode}},
		{name: "a line of a text that is ~", text: strings.Replace(term, "  - A work unit is work.", "  - ~", 1),
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "-", Rule: "bad-yaml", Detail: `bad-yaml: the value at "/definition/0" is null`, Effect: LosesNode}},
		{name: "a null of a kind, which the schema would keep", text: strings.Replace(structure, "kind: structure", "kind: null", 1),
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "-", Rule: "bad-yaml", Detail: `bad-yaml: the value at "/kind" is null`, Effect: LosesNode}},
		{name: "a null in the test part", text: strings.Replace(strings.Replace(program, "%s", "1h0m0s", 1), "  job: go-checks", "  job: null", 1),
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "-", Rule: "bad-yaml", Detail: `bad-yaml: the value at "/test/job" is null`, Effect: LosesNode}},
		{name: "a null of a key that the node does not take", text: term + "surprise: ~\n",
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "-", Rule: "bad-yaml", Detail: `bad-yaml: the value at "/surprise" is null`, Effect: LosesNode}},
		{name: "a null in the second document", text: term + strings.Replace(strings.Replace(term, "work-unit", "work-item", 1), "name: work unit", "name: ~", 1), nodes: 1,
			want: Problem{Pos: Position{"x.yaml", 7}, Node: "-", Rule: "bad-yaml", Detail: `bad-yaml: the value at "/name" is null`, Effect: LosesNode}},
		{name: "a name that is a number in the second document", text: term + strings.Replace(strings.Replace(term, "work-unit", "work-item", 1), "work unit", "12", 1), nodes: 1,
			want: Problem{Pos: Position{"x.yaml", 7}, Node: "work-item", Rule: "schema", Detail: "schema: /name: got number, want string", Effect: LosesNode}},
		{name: "a query check with a test part", text: query, nodes: 1,
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "each-claim-is-in-the-graph", Rule: "schema", Detail: "schema: /test: the node takes no such key", Effect: KeepsEachNode}},
		{name: "a key out of its order", text: strings.Replace(term, "id: work-unit\nname: work unit\n", "name: work unit\nid: work-unit\n", 1), nodes: 1,
			want: notPrinted(3, "work-unit", "name: work unit\n", "id: work-unit\n", "another order of keys")},
		{name: "a comment", text: strings.Replace(term, "---\n", "---\n# The term of work.\n", 1), nodes: 1,
			want: notPrinted(2, "work-unit", "# The term of work.\n", "type: term\n", "a comment")},
		{name: "a tag", text: strings.Replace(term, "name: work unit", "name: !!str work unit", 1), nodes: 1,
			want: notPrinted(4, "work-unit", "name: !!str work unit\n", "name: work unit\n", "another spelling")},
		{name: "a binary", text: strings.Replace(term, "name: work unit", "name: !!binary d29yayB1bml0", 1), nodes: 1,
			want: notPrinted(4, "work-unit", "name: !!binary d29yayB1bml0\n", "name: work unit\n", "another spelling")},
		{name: "a merge key", text: strings.Replace(term, "name: work unit", "<<: {name: work unit}", 1), nodes: 1,
			want: notPrinted(4, "work-unit", "<<: {name: work unit}\n", "name: work unit\n", "another order of keys")},
		{name: "a space at the end of a line", text: strings.Replace(term, "name: work unit", "name: work unit ", 1), nodes: 1,
			want: notPrinted(4, "work-unit", "name: work unit \n", "name: work unit\n", "a space at the end of the line")},
		{name: "quotes around a plain value", text: strings.Replace(term, "name: work unit", `name: "work unit"`, 1), nodes: 1,
			want: notPrinted(4, "work-unit", "name: \"work unit\"\n", "name: work unit\n", "other quotes")},
		{name: "a flow list", text: strings.Replace(term, "definition:\n  - A work unit is work.", "definition: [A work unit is work.]", 1), nodes: 1,
			want: notPrinted(5, "work-unit", "definition: [A work unit is work.]\n", "definition:\n", "another spelling")},
		{name: "a block scalar", text: strings.Replace(term, "  - A work unit is work.", "  - |-\n    A work unit is work.", 1), nodes: 1,
			want: notPrinted(6, "work-unit", "  - |-\n", "  - A work unit is work.\n", "another spelling")},
		{name: "a list out of its order", text: structure, nodes: 1,
			want: notPrinted(6, "legion-reads-issues-only-through-dispatch", "  - legion\n", "  - dispatch\n", "another spelling")},
		{name: "an indent of four spaces", text: strings.Replace(term, "  - A work", "    - A work", 1), nodes: 1,
			want: notPrinted(6, "work-unit", "    - A work unit is work.\n", "  - A work unit is work.\n", "another indent")},
		{name: "a date that is not quoted", text: strings.Replace(term, "name: work unit", "name: 2026-11-16", 1), nodes: 1,
			want: notPrinted(4, "work-unit", "name: 2026-11-16\n", "name: \"2026-11-16T00:00:00Z\"\n", "another spelling")},
		{name: "a date with one-digit parts, which yaml.v3 reads as a time", text: strings.Replace(term, "  - A work unit is work.", "  - 2026-1-2", 1), nodes: 1,
			want: notPrinted(6, "work-unit", "  - 2026-1-2\n", "  - \"2026-01-02T00:00:00Z\"\n", "another spelling")},
		{name: "a plain text that yaml.v3 reads as a text and YAML 1.1 as a bool", text: strings.Replace(term, "name: work unit", "name: yes", 1), nodes: 1,
			want: notPrinted(4, "work-unit", "name: yes\n", "name: \"yes\"\n", "other quotes")},
		{name: "an anchor and an alias", text: strings.Replace(term, "name: work unit\ndefinition:\n  - A work unit is work.", "name: &n work unit\ndefinition:\n  - *n", 1), nodes: 1,
			want: notPrinted(4, "work-unit", "name: &n work unit\n", "name: work unit\n", "another spelling")},
		{name: "an anchor with no alias", text: strings.Replace(term, "name: work unit", "name: &n work unit", 1), nodes: 1,
			want: notPrinted(4, "work-unit", "name: &n work unit\n", "name: work unit\n", "another spelling")},
		// A problem that keeps each node in one document does not hide a document of the file that is
		// not in its printed form: the file gets the problem that loses a node too.
		{name: "a plain date beside an id that breaks its pattern", text: strings.Replace(term, "id: work-unit", "id: Work-Unit", 1) +
			strings.Replace(strings.Replace(term, "id: work-unit", "id: work-item", 1), "name: work unit", "name: 2026-11-16", 1), nodes: 2,
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "Work-Unit", Rule: "schema", Detail: "schema: /id: 'Work-Unit' does not match pattern '^[a-z0-9]+(-[a-z0-9]+)*$'", Effect: KeepsEachNode},
			also: new(notPrinted(10, "work-item", "name: 2026-11-16\n", "name: \"2026-11-16T00:00:00Z\"\n", "another spelling"))},
		{name: "a first document with no --- line", text: strings.TrimPrefix(term, "---\n"), nodes: 1,
			want: notPrinted(1, "work-unit", "type: term\n", "---\n", "another spelling")},
		{name: "CRLF line ends", text: strings.ReplaceAll(term, "\n", "\r\n"), nodes: 1,
			want: notPrinted(1, "work-unit", "---\r\n", "---\n", "a carriage return: print ends each line with a line feed only")},
		{name: "a byte order mark", text: "\ufeff" + term, nodes: 1, want: notPrinted(1, "work-unit", "\ufeff---\n", "---\n", "a byte order mark")},
		{name: "a tab after a colon", text: strings.Replace(term, "name: work unit", "name:\twork unit", 1), nodes: 1,
			want: notPrinted(4, "work-unit", "name:\twork unit\n", "name: work unit\n", "a tab")},
		{name: "no line break at the end of the file", text: strings.TrimSuffix(term, "\n"), nodes: 1,
			want: notPrinted(6, "work-unit", "  - A work unit is work.", "  - A work unit is work.\n", "no line break at the end of the file")},
		{name: "a line after the last node", text: term + "\n", nodes: 1, want: notPrinted(7, "work-unit", "\n", "", "a line after the last node")},
		{name: "a carriage return as a line end", text: strings.ReplaceAll(term, "\n", "\r"), nodes: 1,
			want: notPrinted(1, "work-unit", "---\r", "---\n", "a carriage return: print ends each line with a line feed only")},
		{name: "a line separator in a quoted name", text: strings.Replace(term, "name: work unit", "name: 'work\u2028  unit'", 1), nodes: 1,
			want: notPrinted(4, "work-unit", "name: 'work\u2028", "name: \"work\\Lunit\"\n", "the character U+2028, which YAML reads as a line break")},
		// A problem that keeps each node is no reason to trust a file that is not in its printed form:
		// the file gets the not-printed problem too, in the document of that problem and in a file
		// of no node.
		{name: "a space at the end of a line beside an id that breaks its pattern", text: strings.Replace(strings.Replace(term, "id: work-unit", "id: Work-Unit", 1), "name: work unit", "name: work unit ", 1), nodes: 1,
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "Work-Unit", Rule: "schema", Detail: "schema: /id: 'Work-Unit' does not match pattern '^[a-z0-9]+(-[a-z0-9]+)*$'", Effect: KeepsEachNode},
			also: new(notPrinted(4, "Work-Unit", "name: work unit \n", "name: work unit\n", "a space at the end of the line"))},
		{name: "a file of a comment only", text: "# No node.\n",
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "-", Rule: "no-node", Detail: "no-node: a spec file holds one or more nodes", Effect: KeepsEachNode},
			also: new(notPrinted(1, "-", "# No node.\n", "", "a line after the last node"))},
		{name: "a time limit that is not a Go duration", text: strings.Replace(program, "%s", "a day", 1), nodes: 1,
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "a-restart-keeps-the-claim", Rule: "bad-time-limit", Detail: `bad-time-limit: "a day" is not a positive Go duration`, Effect: LosesNode}},
		{name: "a time limit of zero", text: strings.Replace(program, "%s", "0s", 1), nodes: 1,
			want: Problem{Pos: Position{"x.yaml", 1}, Node: "a-restart-keeps-the-claim", Rule: "bad-time-limit", Detail: `bad-time-limit: "0s" is not a positive Go duration`, Effect: LosesNode}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			tree, problems := Parse(map[string][]byte{"x.yaml": []byte(row.text)})
			want := []Problem{row.want}
			if row.also != nil {
				want = append(want, *row.also)
			}
			if !reflect.DeepEqual(problems, want) {
				t.Errorf("Parse(%q):\n got %+v\nwant %+v", row.text, problems, want)
			}
			if got := len(nodesOf(tree)); got != row.nodes {
				t.Errorf("Parse(%q): the tree holds %d nodes, want %d: %+v", row.text, got, row.nodes, tree)
			}
		})
	}
}

// readerRow is one input of the table of the reader's problems: the kind of problem that it gives,
// its Effect, the Detail that it gives, and the file.
type readerRow struct {
	kind         problemKind
	effect       Effect
	detail, file string
}

// Each kind of problem of the reader is in one of two columns: it can lose a node of the files or
// change one, so a comparison of the tree misses that text (LosesNode), or it leaves each node as the
// files write it (KeepsEachNode). The table has one or more rows for each kind that the reader
// declares (problemKinds), and Parse gives the row's problem for the row's file with the row's
// Effect, so a new kind fails the test until it has a row. Every problem that Parse gives for
// those files has one of the two Effects: a problem with no Effect fails, so no kind reads as one
// that keeps each node by default. The rows of the schema hold each case of what the node keeps: a
// value that the node holds, and a key, a type, a component kind or a whole node that it drops.
func TestEachProblemOfTheReaderSaysWhetherItLosesANode(t *testing.T) {
	t.Parallel()
	const term = "---\ntype: term\nid: work-unit\nname: work unit\ndefinition:\n  - A work unit is work.\n"
	const component = "---\ntype: component\nid: dispatch\nname: Dispatch\nkind: component\npurpose:\n  - It keeps each issue.\n"
	const check = "---\ntype: check\nid: a-restart-keeps-the-claim\nserves: a-claim-survives-a-restart\nsubject: code\nprocedure: agent\n" +
		"claim: The daemon reads the claim after it starts again.\nprocedure_text:\n  - An agent starts the daemon again.\nfailing_example:\n  - A daemon with no store.\n"
	rows := []readerRow{
		{yamlStops, LosesNode, "bad-yaml: line 8: did not find expected node content", term + "---\ntype: [\n"},
		{yamlDocument, LosesNode, `bad-yaml: line 3: mapping key "type" already defined at line 2`, strings.Replace(term, "type: term\n", "type: term\ntype: term\n", 1)},
		{noJSONForm, LosesNode, "bad-yaml: the document holds a value that JSON cannot hold", "---\n1: x\n"},
		{yamlNull, LosesNode, `bad-yaml: the value at "/name" is null`, strings.Replace(term, "name: work unit", "name: null", 1)},
		{yamlNull, LosesNode, "bad-yaml: the document is null", "---\n"},
		{noNode, KeepsEachNode, "no-node: a spec file holds one or more nodes", ""},
		{schemaDropped, LosesNode, "schema: node: additional properties 'surprise' not allowed", term + "surprise: true\n"},
		{schemaDropped, LosesNode, "schema: /name: got number, want string", strings.Replace(term, "name: work unit", "name: 12", 1)},
		{schemaDropped, LosesNode, "schema: /type: value must be one of 'requirement', 'check', 'component', 'term'", strings.Replace(term, "type: term", "type: note", 1)},
		{schemaDropped, LosesNode, "schema: /kind: value must be one of 'component', 'interface'", strings.Replace(component, "kind: component", "kind: service", 1)},
		{schemaKept, KeepsEachNode, "schema: /subject: value must be one of 'code', 'production', 'graph'", strings.Replace(check, "subject: code", "subject: the logs", 1)},
		{schemaKept, KeepsEachNode, "schema: node: missing property 'failing_example'", strings.Replace(check, "failing_example:\n  - A daemon with no store.\n", "", 1)},
		{schemaKept, KeepsEachNode, "schema: /id: 'Work-Unit' does not match pattern '^[a-z0-9]+(-[a-z0-9]+)*$'", strings.Replace(term, "id: work-unit", "id: Work-Unit", 1)},
		{timeLimitDropped, LosesNode, `bad-time-limit: "soon" is not a positive Go duration`, strings.Replace(check, "procedure: agent\n", "procedure: agent\ntime_limit: soon\n", 1)},
		// A file that is not in its printed form loses a node, whatever the reason: two YAML libraries
		// can read it as two texts, as for a raw U+2028, U+2029 or U+0085, which YAML 1.1 reads as a
		// line break and YAML 1.2 as a character, in any style of text.
		{notPrinted, LosesNode, `not-printed: line 4 is "name: 'work unit'\n", and print writes "name: work unit\n" there (other quotes); spec-check print writes each node in that form`,
			strings.Replace(term, "name: work unit", "name: 'work unit'", 1)},
		{notPrinted, LosesNode, `not-printed: line 4 is "name: 2026-11-16\n", and print writes "name: \"2026-11-16T00:00:00Z\"\n" there (another spelling); spec-check print writes each node in that form`,
			strings.Replace(term, "name: work unit", "name: 2026-11-16", 1)},
		{notPrinted, LosesNode, `not-printed: line 4 is "name: !!binary d29yayB1bml0\n", and print writes "name: work unit\n" there (another spelling); spec-check print writes each node in that form`,
			strings.Replace(term, "name: work unit", "name: !!binary d29yayB1bml0", 1)},
		{notPrinted, LosesNode, `not-printed: line 4 is "<<: {name: work unit}\n", and print writes "name: work unit\n" there (another order of keys); spec-check print writes each node in that form`,
			strings.Replace(term, "name: work unit", "<<: {name: work unit}", 1)},
		{notPrinted, LosesNode, `not-printed: line 4 is "name: &n work unit\n", and print writes "name: work unit\n" there (another spelling); spec-check print writes each node in that form`,
			strings.Replace(term, "name: work unit\ndefinition:\n  - A work unit is work.", "name: &n work unit\ndefinition:\n  - *n", 1)},
		{notPrinted, LosesNode, "not-printed: line 4 is " + strconv.Quote("name: \"work\u2028") + ", and print writes " + strconv.Quote("name: \"work\\Lunit\"\n") +
			" there (the character U+2028, which YAML reads as a line break); spec-check print writes each node in that form",
			strings.Replace(term, "name: work unit", "name: \"work\u2028  unit\"", 1)},
		{notPrinted, LosesNode, "not-printed: line 4 is " + strconv.Quote("name: 'work\u2029") + ", and print writes " + strconv.Quote("name: \"work\\Punit\"\n") +
			" there (the character U+2029, which YAML reads as a line break); spec-check print writes each node in that form",
			strings.Replace(term, "name: work unit", "name: 'work\u2029  unit'", 1)},
		{notPrinted, LosesNode, `not-printed: line 6 is "  - |-\n", and print writes "  - A work\n" there (another spelling); spec-check print writes each node in that form`,
			strings.Replace(term, "  - A work unit is work.", "  - |-\n    A work\u0085    unit is work.", 1)},
	}
	name := map[Effect]string{LosesNode: "LosesNode", KeepsEachNode: "KeepsEachNode"}
	covered := map[problemKind]bool{}
	for _, row := range rows {
		if start, _, _ := strings.Cut(problemKinds[row.kind].format, "%"); !strings.HasPrefix(row.detail, start) || row.effect != problemKinds[row.kind].effect {
			t.Errorf("the row %q is not of its kind %d, %q with the Effect %s", row.detail, row.kind, problemKinds[row.kind].format, name[problemKinds[row.kind].effect])
		}
		covered[row.kind] = true
		_, problems := Parse(map[string][]byte{"spec.yaml": []byte(row.file)})
		given := 0
		for _, p := range problems {
			if _, ok := name[p.Effect]; !ok {
				t.Errorf("Parse(%q) gives %+v, which has no Effect", row.file, p)
			}
			if p.Detail == row.detail {
				given++
				if p.Effect != row.effect {
					t.Errorf("Parse(%q) gives %q the Effect %s, want %s", row.file, p.Detail, name[p.Effect], name[row.effect])
				}
			}
		}
		if given == 0 {
			t.Errorf("Parse(%q) gives no problem %q, only %+v", row.file, row.detail, problems)
		}
	}
	for kind := range numProblemKinds {
		if !covered[kind] {
			t.Errorf("kind %d of problem of the reader, %q, has no row in the table: give it the Effect of its column and a file that gives it", kind, problemKinds[kind].format)
		}
	}
}

// The printer writes a text double-quoted where quotedScalar says that another YAML reader can read
// it plain as another value than the text, or yaml.v3 says it of its own reading, and the reader
// gives a problem that loses a node for each file that is not in that printed form, so for the
// plain spelling of each such text. Each text of a fixed list
// and 20,000 random texts made of the characters of YAML's implicit types is the name of a term,
// written plain, in a file that yaml.v3 reads with that text as a plain scalar: the reader gives a
// problem that loses a node exactly when the printer writes the text double-quoted, and a text that
// the printer writes plain makes the printed document, which the reader takes with no problem. A
// text that the printer writes in single quotes, for a reason of form such as a leading "---", is
// no case of the rule, and neither is a text that yaml.v3 reads as more than one plain scalar. The
// test requires 1,000 cases on each side, so it cannot pass on no case.
func TestTheReaderLosesANodeAtEachPlainTextThatThePrinterQuotes(t *testing.T) {
	t.Parallel()
	texts := []string{
		"~", "null", "y", "Y", "yes", "No", "on", "OFF", "true", "False",
		"0b1010", "017", "0o17", "12", "+1_000", "0x1F", "0x_", "190:20:30", "1:30", "-1:30",
		"1.5", ".5", "1e3", "1E-3", "1.2.3", ".", "...", "1.5_", "190:20:30.15", ".inf", "-.Inf", ".nan",
		"2026-11-16", "2026-1-2", "2026-11-16T10:00:00Z", "2026-11-16t10:00:00", "2026-1-2 1:2:3", "2026-11-16 10:00:00 +01:00",
		"<<", "=", "0X1F", "0B101", "0O17", "-0b1", "1e_5", "1_e5", "1__0", "2053-9-40",
		"work unit", "Legion", "a = b", "<<<", "==", "1a", "0x", "e3", "v1.2.3", "2026-11-16x", "---", "x:y", ":x",
	}
	const alphabet = "0123456789+-._:~<= eExXoObBtTfFyYnNlLuUaAsSiIzZ"
	rng := rand.New(rand.NewPCG(4083, 3))
	edit := func(text string) string {
		r := []byte(text)
		at := rng.IntN(len(r) + 1)
		c := alphabet[rng.IntN(len(alphabet))]
		switch rng.IntN(3) {
		case 0:
			return string(r[:at]) + string(c) + string(r[at:])
		case 1:
			if at < len(r) {
				return string(r[:at]) + string(r[at+1:])
			}
		}
		if at < len(r) {
			r[at] = c
		}
		return string(r)
	}
	fixed := len(texts)
	for i := range 20000 {
		if i%2 == 0 {
			text := texts[rng.IntN(fixed)]
			for range 1 + rng.IntN(2) {
				text = edit(text)
			}
			texts = append(texts, text)
			continue
		}
		b := make([]byte, 1+rng.IntN(10))
		for k := range b {
			b[k] = alphabet[rng.IntN(len(alphabet))]
		}
		texts = append(texts, string(b))
	}
	const definition = "A work unit is work."
	quotedCases, plainCases := 0, 0
	for _, text := range texts {
		file := "---\ntype: term\nid: work-unit\nname: " + text + "\ndefinition:\n  - " + definition + "\n"
		if !readsAsPlainName(file, text) {
			continue
		}
		printed, err := printNode(Term{ID: "work-unit", Name: text, Definition: definition})
		if err != nil {
			t.Fatalf("printNode of the name %q: %v", text, err)
		}
		var quoted bool
		switch line := strings.Split(printed, "\n")[3]; {
		case line == "name: "+text:
		case strings.HasPrefix(line, `name: "`):
			quoted = true
		default:
			continue
		}
		_, problems := Parse(map[string][]byte{"x.yaml": []byte(file)})
		loses := slices.ContainsFunc(problems, func(p Problem) bool { return p.Effect == LosesNode })
		if loses != quoted {
			t.Errorf("the name %q: the printer writes it double-quoted: %t, and the reader gives a problem that loses a node for it plain: %t (%+v)", text, quoted, loses, problems)
		}
		if quoted {
			quotedCases++
			continue
		}
		plainCases++
		if file != printed || len(problems) != 0 {
			t.Errorf("the name %q, which the printer writes plain: the printed document %q, the file %q, problems %+v", text, printed, file, problems)
		}
	}
	t.Logf("%d texts: %d that the printer double-quotes, %d that it writes plain", len(texts), quotedCases, plainCases)
	if quotedCases < 1000 || plainCases < 1000 {
		t.Errorf("%d texts that the printer double-quotes and %d that it writes plain, want 1,000 of each", quotedCases, plainCases)
	}
}

// readsAsPlainName reports whether yaml.v3 reads file as one document of the four keys of a term,
// whose name is the plain scalar text.
func readsAsPlainName(file, text string) bool {
	var doc yaml.Node
	if yaml.Unmarshal([]byte(file), &doc) != nil || len(doc.Content) != 1 {
		return false
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode || len(m.Content) != 8 {
		return false
	}
	for i, key := range []string{"type", "id", "name", "definition"} {
		if m.Content[2*i].Value != key {
			return false
		}
	}
	name := m.Content[5]
	return name.Kind == yaml.ScalarNode && name.Style == 0 && name.Value == text
}

// firstUnprinted names a line and a node for each file it is given, and never panics: a file
// whose documents give no node, which no document that the schema takes reaches today, is refused
// at its first line with no node, and a node that printNode refuses is at the line of its own
// document, not the file's first.
func TestFirstUnprintedNamesTheLineAndTheNode(t *testing.T) {
	t.Parallel()
	term := Term{ID: "work-unit", Name: "work unit", Definition: "A work unit is work."}
	unprintable := Term{Name: "no id", Definition: "A term with no id prints as no document."}
	for _, row := range []struct {
		name  string
		data  string
		nodes []any
		lines []int
		line  int
		node  any
	}{
		{"documents that give no node", "---\ntype: term\n", nil, nil, 1, nil},
		{"a node that printNode refuses, in the second document", "", []any{term, unprintable}, []int{1, 7}, 7, unprintable},
	} {
		u := firstUnprinted([]byte(row.data), row.nodes, row.lines)
		if u.line != row.line || !reflect.DeepEqual(u.node, row.node) {
			t.Errorf("%s: firstUnprinted = %d, %+v; want %d, %+v", row.name, u.line, u.node, row.line, row.node)
		}
	}
}

// splitDocuments is each document of text, a file that starts with its first "---" line: the text
// from each line that is exactly "---" to the next.
func splitDocuments(text string) []string {
	var documents []string
	for line := range strings.Lines(text) {
		if line == "---\n" || len(documents) == 0 {
			documents = append(documents, "")
		}
		documents[len(documents)-1] += line
	}
	return documents
}

// An entry of the spec directory is a spec file when it is a regular file directly in the
// directory whose name ends in .yaml; every other entry is passed over.
func TestIsFile(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		kind notes.Kind
		want bool
	}{
		{"a.yaml", notes.KindFile, true},
		{"a.yml", notes.KindFile, false},
		{"a.md", notes.KindFile, false},
		{"a.json", notes.KindFile, false},
		{"a.yaml", notes.KindSymlink, false},
		{"sub/a.yaml", notes.KindFile, false},
		{"a.yaml", notes.KindDirectory, false},
	} {
		if got := IsFile(row.name, row.kind); got != row.want {
			t.Errorf("IsFile(%q, %v) = %t, want %t", row.name, row.kind, got, row.want)
		}
	}
}

// A layout keeps each file byte for byte, also a file that the reader refuses: a file with a key
// that the schema refuses, a file with a fault of the YAML after a node, and a file of no YAML.
// Uncounted names the file whose reading stops, and only that file.
func TestReadLayoutKeepsEachFile(t *testing.T) {
	t.Parallel()
	all := fileText(t, "testdata/nodes/all-kinds.yaml")
	files := map[string][]byte{
		"all-kinds.yaml": []byte(all),
		"surprise.yaml":  []byte(strings.Replace(all, "name: work unit\n", "name: work unit\nsurprise: true\n", 1)),
		"broken.yaml":    []byte(all + "---\ntype: [\n"),
		"prose.yaml":     []byte("A line that a person wrote.\n"),
	}
	l, problems := ReadLayout(files)
	if len(problems) == 0 {
		t.Fatal("ReadLayout found no problem in the three refused files")
	}
	if got := l.Bytes(); !reflect.DeepEqual(got, files) {
		t.Errorf("Bytes:\n got %q\nwant %q", got, files)
	}
	if n := len(l.Files["broken.yaml"]); n != 6 || l.Files["broken.yaml"][5].Node != nil || l.Files["broken.yaml"][5].Text != "---\ntype: [\n" {
		t.Errorf("broken.yaml: %d documents %+v, want the five nodes and the text after them as a document of no node", n, l.Files["broken.yaml"])
	}
	want := map[string]string{"broken.yaml": "the YAML library stops at line 51 (line 51: did not find expected node content), so the reader does not know the documents after that line"}
	if !reflect.DeepEqual(l.Uncounted, want) {
		t.Errorf("Uncounted = %q, want %q", l.Uncounted, want)
	}
}

// The reader cuts a file into the documents that yaml.v3 reads, with each line break that yaml.v3
// counts: CR LF, CR, U+0085, U+2028 and U+2029 in a quoted key, after a key, in a quoted text, in
// a block text and between two documents, a CR at the end of the file, and the two files of the
// red team of print (CR line ends, and U+2028 in a quoted name). Each document's text, read
// alone, is the document that the whole file gives, so Place, which replaces one text, leaves the
// bytes of each other document as they were. A file whose reading stops, and a file that yaml.v3
// reads as UTF-16, are the files that Uncounted names. LF is the control: the same file with LF
// alone is three printed documents.
func TestReadLayoutCutsEachDocumentWhereYAMLReadsIt(t *testing.T) {
	t.Parallel()
	term := func(id string) string {
		return "---\ntype: term\nid: " + id + "\nname: " + id + "\ndefinition:\n  - A term is a unit of meaning.\n"
	}
	first, rest := term("first-term"), term("second-term")+term("third-term")
	rows := map[string]string{
		"LF alone":                      first + rest,
		"carriage returns as line ends": strings.ReplaceAll(first+rest, "\n", "\r"),
		"U+2028 in a quoted name":       strings.Replace(first, "name: first-term", "name: 'A"+strings.Repeat("\u2028  B", 30)+"'", 1) + rest,
		"a CR at the end of the file":   first + rest + "\r",
	}
	for name, lineBreak := range map[string]string{"U+0085": "\u0085", "U+2028": "\u2028", "U+2029": "\u2029", "CR LF": "\r\n", "CR": "\r"} {
		rows[name+" in a quoted key"] = strings.Replace(first, "definition:\n", "'a"+lineBreak+"  key': x\ndefinition:\n", 1) + rest
		rows[name+" after a key"] = strings.Replace(first, "name: first-term", "name:"+lineBreak+"  first-term", 1) + rest
		rows[name+" in a quoted text"] = strings.Replace(first, "name: first-term", "name: 'first"+lineBreak+"  term'", 1) + rest
		rows[name+" in a block text"] = strings.Replace(first, "  - A term is a unit of meaning.\n", "  - |-\n    A term"+lineBreak+"    is a unit of meaning.\n", 1) + rest
		rows[name+" between documents"] = first + lineBreak + rest
	}
	for name, text := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data := []byte(text)
			whole, _, stopped := readDocuments("terms.yaml", data)
			l, problems := ReadLayout(map[string][]byte{"terms.yaml": data})
			if got := l.Bytes()["terms.yaml"]; !bytes.Equal(got, data) {
				t.Fatalf("Bytes gives %q, want the file %q", got, data)
			}
			if (name == "LF alone") != (len(problems) == 0) {
				t.Fatalf("problems %+v: want none for LF alone and one or more for each other file", problems)
			}
			if stopped {
				if l.Uncounted["terms.yaml"] == "" {
					t.Fatal("the reading of the file stops, and Uncounted does not name it")
				}
				t.Logf("the reading stops; Uncounted: %s", l.Uncounted["terms.yaml"])
				return
			}
			if reason := l.Uncounted["terms.yaml"]; reason != "" {
				t.Fatalf("Uncounted names a file that yaml.v3 reads to its end: %s", reason)
			}
			documents := l.Files["terms.yaml"]
			if len(documents) != len(whole) || len(whole) != 3 {
				t.Fatalf("the layout holds %d documents and yaml.v3 reads %d, want 3 and 3", len(documents), len(whole))
			}
			for i, d := range documents {
				alone, problems, stopped := readDocuments("terms.yaml", []byte(d.Text))
				if stopped || len(alone) != 1 || !bytes.Equal(alone[0].data, whole[i].data) {
					t.Errorf("the text %q of document %d, read alone, gives %d documents (problems %+v), want the document of line %d, %s",
						d.Text, i+1, len(alone), problems, whole[i].line, whole[i].data)
				}
			}
			if _, err := l.Place(Term{ID: "third-term", Name: "third term", Definition: "A third term is a word."}, notes.MaxFileSize); err != nil {
				t.Fatal(err)
			}
			for i, d := range l.Files["terms.yaml"][:2] {
				if d.Text != documents[i].Text {
					t.Errorf("Place of the third term changed document %d from %q to %q", i+1, documents[i].Text, d.Text)
				}
			}
		})
	}
	t.Run("UTF-16", func(t *testing.T) {
		t.Parallel()
		units := utf16.Encode([]rune("\ufeff" + first + rest))
		data := make([]byte, 0, 2*len(units))
		for _, u := range units {
			data = append(data, byte(u), byte(u>>8))
		}
		whole, _, _ := readDocuments("terms.yaml", data)
		l, _ := ReadLayout(map[string][]byte{"terms.yaml": data})
		if len(whole) != 3 || !strings.Contains(l.Uncounted["terms.yaml"], "UTF-16") {
			t.Errorf("yaml.v3 reads %d documents, and Uncounted is %q; want 3, and the reason that names UTF-16", len(whole), l.Uncounted)
		}
	})
}

// readFiles reads every file directly in dir, by name, as Parse takes them.
func readFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = content
	}
	return files
}

// digests is the digest of every node of tree, by its kind and id.
func digests(tree Tree) map[string]string {
	all := map[string]string{}
	for _, n := range nodesOf(tree) {
		all[kindName(n.node)+" "+n.id] = n.node.Digest()
	}
	return all
}

// specFile is the spec file of nodes: the document that Print writes for each, in order.
func specFile(t *testing.T, nodes ...any) []byte {
	t.Helper()
	var b strings.Builder
	for _, node := range nodes {
		document, err := printNode(node)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(document)
	}
	return []byte(b.String())
}
