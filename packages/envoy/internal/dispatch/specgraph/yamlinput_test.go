package specgraph

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"gopkg.in/yaml.v3"
)

// The spec files are an input of the derivation too, and the rules that the JSON files of a
// scenario keep hold for them (members.go, sweep_test.go): a null, an empty value and a name of no
// letter and no digit say nothing, so the files refuse each at each place; each member of each node
// has a mark, nonzero or zero, that the schema holds; and no input that says nothing at one place of
// a spec file moves a state toward true.

// fullNodes is one node of each kind, with each member of its document set: a statement with a
// parent, a statement that it serves and a node that it is about; a program check with its test
// and a time limit; a component with a parent; an interface; and a term.
var fullNodes = []specNode{
	Statement{ID: "a-claim-survives-a-restart", Kind: KindBehavior, Parent: "legion-keeps-each-claim", Serves: []string{"legion-keeps-the-work"},
		About: []string{"legion"}, Text: "Legion keeps each claim after it starts again."},
	Check{ID: "a-restart-keeps-the-claim", Serves: "a-claim-survives-a-restart", Subject: SubjectCode, Procedure: ProcedureProgram,
		Test: &Test{Runner: "go", Selector: "example.com/acme/daemon TestARestartKeepsTheClaim", Job: "go-checks"}, TimeLimit: time.Hour,
		Claim: "The daemon reads the claim after it starts again.", ProcedureText: "The program claims one issue and starts the daemon again.",
		FailingExample: "A daemon with no store."},
	Component{ID: "legion", Name: "Legion", Parent: "acme", Purpose: "It runs the work."},
	Component{ID: "legion-to-dispatch", Name: "Legion to Dispatch", Interface: true, Joins: []string{"dispatch", "legion"}, Purpose: "Legion reads issues through it."},
	Term{ID: "work-unit", Name: "work unit", Definition: "A work unit is work."},
}

// nodeFileTypes is the type that the reader reads the data of a document into, by the name of its
// definition in the schema (schema/spec-node.schema.json).
var nodeFileTypes = map[string]reflect.Type{
	"requirement": reflect.TypeFor[requirementFile](),
	"check":       reflect.TypeFor[checkFile](),
	"test":        reflect.TypeFor[testFile](),
	"component":   reflect.TypeFor[componentFile](),
	"term":        reflect.TypeFor[termFile](),
}

// yamlPlace is one place of a YAML document: its JSON pointer, the path of indexes into Content
// from the document node to its value, and whether it is a member of a mapping, which can be left
// out.
type yamlPlace struct {
	pointer string
	path    []int
	member  bool
}

// specDocuments is each document of a spec file as yaml.v3 reads it.
func specDocuments(t *testing.T, data []byte) []*yaml.Node {
	t.Helper()
	var docs []*yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	for {
		var doc yaml.Node
		if err := decoder.Decode(&doc); err != nil {
			if err.Error() == "EOF" {
				return docs
			}
			t.Fatalf("the spec file does not read: %v", err)
		}
		docs = append(docs, &doc)
	}
}

// writeDocuments is docs as the printer writes its documents: "---", then each one written by a
// yaml.v3 Encoder with SetIndent(2) (encodeYAML).
func writeDocuments(t *testing.T, docs []*yaml.Node) []byte {
	t.Helper()
	var b strings.Builder
	for _, doc := range docs {
		text, err := encodeYAML(doc)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("---\n" + text)
	}
	return []byte(b.String())
}

// yamlPlaces is each place of the document doc: the document's value itself, each member of each
// mapping and each element of each list, in the order of the document.
func yamlPlaces(doc *yaml.Node) []yamlPlace {
	places := []yamlPlace{{pointer: "", path: []int{0}}}
	var walk func(n *yaml.Node, at yamlPlace)
	walk = func(n *yaml.Node, at yamlPlace) {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				place := yamlPlace{pointer: at.pointer + "/" + pointerToken.Replace(n.Content[i].Value), path: append(slices.Clone(at.path), i+1), member: true}
				places = append(places, place)
				walk(n.Content[i+1], place)
			}
		case yaml.SequenceNode:
			for i, item := range n.Content {
				place := yamlPlace{pointer: at.pointer + "/" + strconv.Itoa(i), path: append(slices.Clone(at.path), i)}
				places = append(places, place)
				walk(item, place)
			}
		}
	}
	walk(doc.Content[0], places[0])
	return places
}

// copyNode is a deep copy of n.
func copyNode(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		c.Content[i] = copyNode(child)
	}
	return &c
}

// holderAt is the node of c, a copy of a document, that holds the value at place, and the index of
// that value in its Content.
func holderAt(c *yaml.Node, place yamlPlace) (*yaml.Node, int) {
	parent := c
	for _, i := range place.path[:len(place.path)-1] {
		parent = parent.Content[i]
	}
	return parent, place.path[len(place.path)-1]
}

// changedAt is a copy of doc with the value at place null, or, with leftOut, the member at place
// taken out of its mapping.
func changedAt(doc *yaml.Node, place yamlPlace, leftOut bool) *yaml.Node {
	c := copyNode(doc)
	parent, last := holderAt(c, place)
	if leftOut {
		parent.Content = slices.Delete(parent.Content, last-1, last+1)
	} else {
		parent.Content[last] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	}
	return c
}

// nullDetail is the reader's detail for a null at the place pointer of a document.
func nullDetail(pointer string) string {
	if pointer == "" {
		return "bad-yaml: the document is null"
	}
	return "bad-yaml: the value at " + quoted(pointer) + " is null"
}

// A null says nothing: yaml.v3 reads it, and the empty value and ~ that spell it, as the zero value
// of its place, a real value of the node's type, such as a statement with no parent. So the reader
// refuses a document that holds a null at any place, the document itself, a member of a mapping or
// an element of a list, with one problem that names the place, and gives no node of it. A text
// that only spells null in quotes, which the printer writes, is a text: the control reads it.
func TestTheReaderRefusesANullAtEachPlaceOfADocument(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, n := range append(slices.Clone(fullNodes), Term{ID: "null-term", Name: "null", Definition: "~"}) {
		printed, err := n.Print()
		if err != nil {
			t.Fatal(err)
		}
		if _, problems := Parse(map[string][]byte{"x.yaml": []byte(printed)}); len(problems) != 0 {
			t.Fatalf("the control %q: %+v", printed, problems)
		}
		docs := specDocuments(t, []byte(printed))
		if got := string(writeDocuments(t, docs)); got != printed {
			t.Fatalf("the document written again is %q, not %q", got, printed)
		}
		for _, place := range yamlPlaces(docs[0]) {
			text := writeDocuments(t, []*yaml.Node{changedAt(docs[0], place, false)})
			tree, problems := Parse(map[string][]byte{"x.yaml": text})
			want := []Problem{{Pos: Position{"x.yaml", 1}, Node: "-", Rule: "bad-yaml", Detail: nullDetail(place.pointer), Effect: LosesNode}}
			if !reflect.DeepEqual(problems, want) {
				t.Errorf("Parse(%q):\n got %+v\nwant %+v", text, problems, want)
			}
			if got := len(nodesOf(tree)); got != 0 {
				t.Errorf("Parse(%q) gives %d nodes, want none", text, got)
			}
			checked++
		}
	}
	for _, text := range []string{"---\n", "---\nnull\n", "---\n~\n", "---\ntype: term\nid: work-unit\nname:\ndefinition:\n  - A work unit is work.\n"} {
		_, problems := Parse(map[string][]byte{"x.yaml": []byte(text)})
		if len(problems) != 1 || !strings.HasSuffix(problems[0].Detail, " is null") || problems[0].Effect != LosesNode {
			t.Errorf("Parse(%q) = %+v, want one problem of a null that loses the node", text, problems)
		}
	}
	t.Logf("%d places", checked)
}

// emptyAt is a copy of doc with the value at place empty, of its own kind: "" for a text, [] for
// a list and {} for a mapping.
func emptyAt(doc *yaml.Node, place yamlPlace) *yaml.Node {
	c := copyNode(doc)
	parent, last := holderAt(c, place)
	parent.Content[last] = map[yaml.Kind]*yaml.Node{
		yaml.ScalarNode:   {Kind: yaml.ScalarNode, Tag: "!!str", Value: ""},
		yaml.SequenceNode: {Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle},
		yaml.MappingNode:  {Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle},
	}[parent.Content[last].Kind]
	return c
}

// An empty value names nothing, as an empty content label of the history and an empty rule of
// uncited.json name nothing, which the readers of those files refuse: a text "", a list [] or a
// mapping {} is the zero value of its place written out, and the printer writes no zero value,
// since an absent value is an absent key. So the reader refuses a document that holds an empty
// value at any place, the document itself, a member of a mapping or an element of a list, with a
// problem that names the place. And it reads no empty value as a value of another text: when no
// problem loses the node, the node that it gives prints as the file writes it, as a problem that
// keeps each node says (KeepsEachNode), so compare judges the text that the file holds; else a
// problem loses the node.
func TestTheReaderRefusesAnEmptyValueAtEachPlaceOfADocument(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, n := range fullNodes {
		printed, err := n.Print()
		if err != nil {
			t.Fatal(err)
		}
		docs := specDocuments(t, []byte(printed))
		for _, place := range yamlPlaces(docs[0]) {
			text := string(writeDocuments(t, []*yaml.Node{emptyAt(docs[0], place)}))
			tree, problems := Parse(map[string][]byte{"x.yaml": []byte(text)})
			at := place.pointer + ":"
			if place.pointer == "" {
				at = "node:"
			}
			if !slices.ContainsFunc(problems, func(p Problem) bool { return strings.Contains(p.Detail, at) }) {
				t.Errorf("Parse(%q) gives %+v, want a problem at %s", text, problems, at)
			}
			if !slices.ContainsFunc(problems, func(p Problem) bool { return p.Effect == LosesNode }) {
				nodes := nodesOf(tree)
				if len(nodes) != 1 {
					t.Errorf("Parse(%q) gives %d nodes and no problem that loses one", text, len(nodes))
				} else if again, err := nodes[0].node.Print(); err != nil || again != text {
					t.Errorf("Parse(%q) keeps each node, and the node prints %q (%v): the empty value at %s reads as another text", text, again, err, at)
				}
			}
			checked++
		}
	}
	t.Logf("%d places", checked)
}

// nameAt is a copy of doc with the value at place, a text, written as name.
func nameAt(doc *yaml.Node, place yamlPlace, name string) *yaml.Node {
	c := copyNode(doc)
	parent, last := holderAt(c, place)
	parent.Content[last] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}
	return c
}

// A name of no letter and no digit names nothing, and the readers of the JSON files of a scenario
// refuse one at each place, as a value or as the key of a map (nameFault, hasWord). Each text of a
// document is a name or a line: an id, a parent or an entry of serves, about or joins; a runner, a
// selector or a job of a test; a name of a component or of a term; a kind, a subject, a procedure
// or a time limit; a line of a text. So the files refuse a document of each kind with the text at
// any place written as a name of no letter and no digit, "" and " " as the sweeps write them and a
// format character (U+200B): the reader, by the schema or by its rule of the time limit, or the
// rules of the files that `spec-check tree` runs and the scenario reader asks of each spec file
// (Validate), by the one test of a missing text (hasWord). A document holds no key that its writer
// picks and no repository path, so the other forms of the sweep of the JSON files have no place in
// it.
func TestTheFilesRefuseANameOfNoLetterAndNoDigitAtEachPlaceOfADocument(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, n := range fullNodes {
		printed, err := n.Print()
		if err != nil {
			t.Fatal(err)
		}
		tree, problems := Parse(map[string][]byte{"x.yaml": []byte(printed)})
		if len(problems) != 0 {
			t.Fatalf("the control %q: %+v", printed, problems)
		}
		// A node alone names nodes that its file does not hold: those problems are the control's.
		control := Validate(tree, Options{})
		docs := specDocuments(t, []byte(printed))
		for _, place := range yamlPlaces(docs[0]) {
			if parent, last := holderAt(docs[0], place); parent.Content[last].Kind != yaml.ScalarNode {
				continue
			}
			for _, name := range append(slices.Clone(namesOfNothing()), "\u200b") {
				text := writeDocuments(t, []*yaml.Node{nameAt(docs[0], place, name)})
				tree, problems := Parse(map[string][]byte{"x.yaml": text})
				for _, p := range Validate(tree, Options{}) {
					if !slices.Contains(control, p) {
						problems = append(problems, p)
					}
				}
				if len(problems) == 0 {
					t.Errorf("%s of %s as %q: the files take %q", place.pointer, nodeID(n), name, text)
				}
				checked++
			}
		}
	}
	t.Logf("%d places and names", checked)
}

// An approved document names each node by an id: its own, and each node that it relates to, its
// parent and each node that it serves, is about or joins, the places that the schema gives the
// definition id (#/$defs/id, or an item of #/$defs/ids). The derivation reads a relation only by an
// id (isIDShape), so a value of another shape there, a name of no letter and no digit among them,
// would read as no relation: a statement at the top of the tree in place of under its parent, which
// can make that parent met. The problems of the schema in an approved document pass, as the rules
// of the files do, so versionText refuses such a value itself, at each id place of a node of each
// kind. At a place that names no node, such as a claim, the same values do not read as an id, and
// some read as the node (the control), as an approved text that breaks a rule of the files does.
func TestAnApprovedDocumentNamesEachNodeByAnID(t *testing.T) {
	t.Parallel()
	defs, _ := schemaDefinitions(t)
	at := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	refused, read := 0, 0
	for _, n := range fullNodes {
		printed, err := n.Print()
		if err != nil {
			t.Fatal(err)
		}
		if _, problems := versionText(nodeID(n), ApprovedVersion{Version: 1, At: at, Document: printed}); len(problems) != 0 {
			t.Fatalf("the control %q: %+v", printed, problems)
		}
		file, _ := fileOf(n)
		def := ""
		for name, typ := range nodeFileTypes {
			if typ == reflect.TypeOf(file) {
				def = name
			}
		}
		properties, _ := defs[def]["properties"].(map[string]any)
		docs := specDocuments(t, []byte(printed))
		for _, place := range yamlPlaces(docs[0]) {
			if holder, last := holderAt(docs[0], place); holder.Content[last].Kind != yaml.ScalarNode {
				continue
			}
			member, _, _ := strings.Cut(strings.TrimPrefix(place.pointer, "/"), "/")
			property, _ := properties[member].(map[string]any)
			names := member == "id" || property["$ref"] == "#/$defs/id" || property["$ref"] == "#/$defs/ids"
			for _, value := range []string{" ", "\u200b", "Not-An-Id"} {
				text := string(writeDocuments(t, []*yaml.Node{nameAt(docs[0], place, value)}))
				id := nodeID(n)
				if member == "id" {
					id = value
				}
				_, problems := versionText(id, ApprovedVersion{Version: 1, At: at, Document: text})
				want := "bad-approved-document: " + member + " " + quoted(value) + " is no id"
				// The printer sorts the ids of a list, so a value in a list can also break its order,
				// and the reader refuses that document as not printed first.
				notPrinted := len(problems) == 1 && problems[0].Rule == "not-printed"
				switch noID := slices.ContainsFunc(problems, func(p Problem) bool { return p.Detail == want }); {
				case names && !(noID && len(problems) == 1 || notPrinted):
					t.Errorf("%s of %s as %q: versionText gives %+v, want the one problem %q", place.pointer, nodeID(n), value, problems, want)
				case names:
					refused++
				case noID:
					t.Errorf("%s of %s names no node, and versionText refuses %q at it as no id", place.pointer, nodeID(n), value)
				case len(problems) == 0:
					read++
				}
			}
		}
	}
	if read == 0 {
		t.Error("versionText refuses each value at each place that names no node, so the test cannot tell an id place from another")
	}
	t.Logf("%d values at an id place refused; %d at another place read as the node", refused, read)
}

// schemaDefinitions is each definition of the schema, by its name, and the members that the root
// of the schema requires of each node.
func schemaDefinitions(t *testing.T) (map[string]map[string]any, []any) {
	t.Helper()
	var schema struct {
		Required []any                     `json:"required"`
		Defs     map[string]map[string]any `json:"$defs"`
	}
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		t.Fatal(err)
	}
	return schema.Defs, schema.Required
}

// names is each string of list, a JSON list, as a set.
func names(list any) map[string]bool {
	set := map[string]bool{}
	items, _ := list.([]any)
	for _, item := range items {
		if s, ok := item.(string); ok {
			set[s] = true
		}
	}
	return set
}

// Each member of the data of a node, of each type that the reader reads a document into, has a
// mark (memberMark), and the schema holds it: a member marked nonzero is one that the schema
// requires of each node of the type, and a member marked zero is one that it does not require of
// each, whose zero value, the key left out, is the value that its reason states. No member is
// marked present or one of: the printer writes no zero value, since an absent value is an absent
// key, so a member left out is its zero value or a refusal. Each member is a key of its definition
// in the schema, and each key of the definition is a member.
func TestEachMemberOfANodeHasAMarkThatTheSchemaHolds(t *testing.T) {
	t.Parallel()
	defs, rootRequired := schemaDefinitions(t)
	marked := 0
	for _, name := range slices.Sorted(maps.Keys(nodeFileTypes)) {
		typ, def := nodeFileTypes[name], defs[name]
		if def == nil {
			t.Errorf("the schema has no definition %q for %s", name, typ)
			continue
		}
		required := names(def["required"])
		if name != "test" {
			maps.Copy(required, names(rootRequired))
		}
		properties, _ := def["properties"].(map[string]any)
		members := map[string]bool{}
		for _, m := range inputMembers(typ) {
			members[m.name] = true
			mark, err := markOf(m.field)
			if err != nil {
				t.Errorf("%s: %v", typ, err)
				continue
			}
			marked++
			if _, ok := properties[m.name]; !ok {
				t.Errorf("%s: member %s is no key of the definition %q of the schema", typ, m.name, name)
			}
			switch {
			case mark.kind == "nonzero" && !required[m.name]:
				t.Errorf("%s: member %s is marked nonzero, and the schema does not require it of each node", typ, m.name)
			case mark.kind == "zero" && required[m.name]:
				t.Errorf("%s: member %s is marked zero, and the schema requires it of each node", typ, m.name)
			case mark.kind != "nonzero" && mark.kind != "zero":
				t.Errorf("%s: member %s is marked %s; a member of a node is nonzero or zero", typ, m.name, mark.kind)
			}
		}
		for key := range properties {
			if !members[key] {
				t.Errorf("the definition %q of the schema has the key %s, which %s does not hold", name, key, typ)
			}
		}
	}
	if marked < 30 {
		t.Errorf("%d members are marked, fewer than the node types hold", marked)
	}
}

// markAt is the mark of the member at the place pointer of a document of the node type typ, a key
// of the document or of its test part.
func markAt(t *testing.T, typ reflect.Type, pointer string) memberMark {
	t.Helper()
	steps := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i, step := range steps {
		j := slices.IndexFunc(inputMembers(typ), func(m inputMember) bool { return m.name == step })
		if j < 0 {
			t.Fatalf("%s holds no member %s", typ, step)
		}
		m := inputMembers(typ)[j]
		if i == len(steps)-1 {
			mark, err := markOf(m.field)
			if err != nil {
				t.Fatal(err)
			}
			return mark
		}
		typ = m.field.Type
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
	}
	t.Fatalf("no member at %s", pointer)
	return memberMark{}
}

// Each member of each node of fullNodes, left out of its document, reads by its mark: one marked
// nonzero is refused; one marked zero leaves the document that the printer writes for the node
// with the member at its zero value, which the reader reads with no problem, unless the schema
// requires the member of that node by a condition of its definition (its then), such as the test
// part of a program check, and then the reader refuses it as missing.
func TestAMemberLeftOutOfADocumentIsReadByItsMark(t *testing.T) {
	t.Parallel()
	defs, _ := schemaDefinitions(t)
	readAsZero := 0
	for _, n := range fullNodes {
		printed, err := n.Print()
		if err != nil {
			t.Fatal(err)
		}
		file, _ := fileOf(n)
		typ := reflect.TypeOf(file)
		def := ""
		for name, ft := range nodeFileTypes {
			if ft == typ {
				def = name
			}
		}
		then, _ := defs[def]["then"].(map[string]any)
		conditional := names(then["required"])
		docs := specDocuments(t, []byte(printed))
		for _, place := range yamlPlaces(docs[0]) {
			if !place.member {
				continue
			}
			text := string(writeDocuments(t, []*yaml.Node{changedAt(docs[0], place, true)}))
			tree, problems := Parse(map[string][]byte{"x.yaml": []byte(text)})
			key := place.pointer[strings.LastIndexByte(place.pointer, '/')+1:]
			switch mark := markAt(t, typ, place.pointer); {
			case mark.kind == "nonzero":
				if len(problems) == 0 {
					t.Errorf("%s of %s, marked nonzero, left out reads with no problem: %q", place.pointer, nodeID(n), text)
				}
			case len(problems) != 0:
				if !conditional[key] || len(problems) != 1 || problems[0].Detail != "schema: node: missing property '"+key+"'" {
					t.Errorf("%s of %s, marked zero, left out gives %+v, want no problem, or the schema's missing property of a condition", place.pointer, nodeID(n), problems)
				}
			default:
				nodes := nodesOf(tree)
				if len(nodes) != 1 {
					t.Fatalf("%q gives %d nodes", text, len(nodes))
				}
				again, err := nodes[0].node.Print()
				if err != nil || again != text {
					t.Errorf("%s of %s, marked zero, left out reads as a node that prints %q (%v), not %q", place.pointer, nodeID(n), again, err, text)
				}
				readAsZero++
			}
		}
	}
	if readAsZero < 5 {
		t.Errorf("%d members left out read as their zero value, fewer than the nodes hold", readAsZero)
	}
}

// documentInput is one input of the sweep at one place of a list of YAML documents: what it
// changed, and the documents with it as the printer writes them.
type documentInput struct {
	what string
	text []byte
}

// documentInputs is each input at one place of docs: each document as null and null at each place
// of each document, an empty value at each place (emptyAt), each text written as each name of no
// letter and no digit that the sweep of the JSON files writes (namesOfNothing; its "" is the empty
// value), and with leftOut each member of each mapping left out.
func documentInputs(t *testing.T, docs []*yaml.Node, leftOut bool) []documentInput {
	t.Helper()
	var inputs []documentInput
	for i, doc := range docs {
		for _, place := range yamlPlaces(doc) {
			with := func(changed *yaml.Node) []byte {
				all := slices.Clone(docs)
				all[i] = changed
				return writeDocuments(t, all)
			}
			where := fmt.Sprintf("document %d %s", i+1, place.pointer)
			inputs = append(inputs, documentInput{where + " as null", with(changedAt(doc, place, false))}, documentInput{where + " empty", with(emptyAt(doc, place))})
			if leftOut && place.member {
				inputs = append(inputs, documentInput{where + " left out", with(changedAt(doc, place, true))})
			}
			if holder, last := holderAt(doc, place); holder.Content[last].Kind == yaml.ScalarNode {
				for _, name := range namesOfNothing() {
					if name != "" {
						inputs = append(inputs, documentInput{fmt.Sprintf("%s as %q", where, name), with(nameAt(doc, place, name))})
					}
				}
			}
		}
	}
	return inputs
}

// approvedDocuments is the document of each version of each node of the approved spec of a
// scenario, data, by the place of the node and of the version in the file.
func approvedDocuments(t *testing.T, data []byte) map[[2]int]string {
	t.Helper()
	var file struct {
		Nodes []struct {
			Versions []struct {
				Document string `json:"document"`
			} `json:"versions"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	documents := map[[2]int]string{}
	for i, n := range file.Nodes {
		for j, v := range n.Versions {
			documents[[2]int{i, j}] = v.Document
		}
	}
	return documents
}

// yamlScenarioInput is one changed YAML input for the same scenario.
type yamlScenarioInput struct {
	what string
	fsys fstest.MapFS
}

// yamlInputStates derives every check, statement and own-statement state of each subject of fsys.
// A changed input may be refused instead; then no state is returned.
func yamlInputStates(fsys fstest.MapFS) (map[string]string, error) {
	scenario, err := ReadScenario(fsys)
	if err != nil {
		return nil, err
	}
	states := map[string]string{}
	for i := range scenario.Subjects {
		state, err := Derive(scenario.Input(i))
		if err != nil {
			return nil, err
		}
		prefix := fmt.Sprintf("%d/", i)
		for _, check := range state.Checks {
			states[prefix+"check/"+check.ID] = check.State
		}
		for _, statement := range state.Statements {
			states[prefix+"statement/"+statement.ID] = statement.State
			states[prefix+"own/"+statement.ID] = statement.Own
		}
	}
	return states, nil
}

// yamlStatesTowardTrue names each check, statement or own statement that became true or met.
func yamlStatesTowardTrue(before, after map[string]string) []string {
	var moved []string
	for key, state := range after {
		if state == "true" && before[key] != "true" || state == "met" && before[key] != "met" {
			moved = append(moved, key+" became "+state)
		}
	}
	slices.Sort(moved)
	return moved
}

// The sweep of TestNoInputThatSaysNothingMovesAStateTowardTrue reads each JSON file of a scenario
// as a tree; the spec files at each commit and the documents of the approved spec are YAML
// documents in the printer's form, inputs too. A null says nothing, and so do an empty value and a
// name of no letter and no digit, since the printer writes no null and no empty value and a text
// of no letter and no digit names nothing, so no null, no empty value and no such name at one place
// of such a document may move a check toward true, a statement or its own state toward met, or a
// wait toward ends. A member left out is another case: the printer writes a zero value as no key,
// so a document with a member left out is the printed text of another node. In the spec files that
// is a text that the approved spec never held, which may move no state toward true either. In the
// approved spec it is another approved text, the person's to approve, such as a statement that is
// at the top of the tree in place of under its parent, which can make that parent met; so the
// sweep leaves no member out of an approved document. Each *.yaml file of each scenario and each
// document of its approved.json is read as YAML, and the sweep derives the states of each of those
// inputs: a file as null, each document as null, null, an empty value and " " at each place (each
// text for " "), and in the spec files each member of each mapping left out. It reads each base
// that the sweep of the JSON files reads (sweepBases), silent-leg with its matrix dropped at m2
// among them. The reader may refuse the input. The documents written again unchanged must give
// each text byte for byte, so the sweep writes each input in the printer's form, and an input that
// leaves out a member that the node does not need reads as a node, not as a text that is not
// printed: some inputs must read, or the sweep could not see a state move.
func TestNoInputThatSaysNothingInASpecFileMovesAStateTowardTrue(t *testing.T) {
	t.Parallel()
	// read counts the inputs that the reader read, of each scenario: in a scenario whose tree each
	// left-out member breaks, such as a statement whose parent then has no check, the reader refuses
	// each input, and only the scenarios together can show that the sweep sees a state.
	var read atomic.Int64
	t.Cleanup(func() {
		if read.Load() == 0 {
			t.Error("each input of the YAML documents of each scenario is refused, so the sweep could not see a state move")
		}
	})
	bases := sweepBases(t)
	for _, scenario := range slices.Sorted(maps.Keys(bases)) {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			base := bases[scenario]
			whole, err := yamlInputStates(base)
			if err != nil {
				t.Fatalf("the scenario: %v", err)
			}
			var inputs []yamlScenarioInput
			with := func(what string, change func(fsys fstest.MapFS)) {
				fsys := maps.Clone(base)
				change(fsys)
				inputs = append(inputs, yamlScenarioInput{what, fsys})
			}
			readBack := func(what string, text []byte) []*yaml.Node {
				docs := specDocuments(t, text)
				if got := writeDocuments(t, docs); string(got) != string(text) {
					t.Fatalf("%s written again by the sweep is %q, not %q", what, got, text)
				}
				return docs
			}
			files := 0
			for _, name := range slices.Sorted(maps.Keys(base)) {
				if path.Ext(name) != FileSuffix {
					continue
				}
				files++
				put := func(data []byte) func(fstest.MapFS) {
					return func(fsys fstest.MapFS) { fsys[name] = &fstest.MapFile{Data: data} }
				}
				with(name+" as null", put([]byte("null\n")))
				for _, in := range documentInputs(t, readBack(name, base[name].Data), true) {
					with(name+" "+in.what, put(in.text))
				}
			}
			if files == 0 {
				t.Fatal("the scenario holds no spec file, so the sweep read none")
			}
			documents := approvedDocuments(t, base["approved.json"].Data)
			for _, at := range slices.SortedFunc(maps.Keys(documents), func(a, b [2]int) int { return cmp.Or(a[0]-b[0], a[1]-b[1]) }) {
				where := fmt.Sprintf("approved.json /nodes/%d/versions/%d/document", at[0], at[1])
				for _, in := range documentInputs(t, readBack(where, []byte(documents[at])), false) {
					with(where+" "+in.what, func(fsys fstest.MapFS) {
						editTree(t, fsys, "approved.json", func(tree any) any {
							versions := tree.(map[string]any)["nodes"].([]any)[at[0]].(map[string]any)["versions"].([]any)
							versions[at[1]].(map[string]any)["document"] = string(in.text)
							return tree
						})
					})
				}
			}
			refused, same, moved, names := 0, 0, 0, 0
			for _, in := range inputs {
				if strings.HasSuffix(in.what, ` as " "`) {
					names++
				}
				got, err := yamlInputStates(in.fsys)
				switch {
				case err != nil:
					refused++
				case maps.Equal(got, whole):
					same++
				default:
					moved++
					if toward := yamlStatesTowardTrue(whole, got); len(toward) > 0 {
						t.Errorf("%s: %s", in.what, strings.Join(toward, "; "))
					}
				}
			}
			read.Add(int64(same + moved))
			t.Logf(`%d inputs of %d spec files and %d approved documents: %d refused, %d read as the whole scenario, %d read another state; %d of them a text written as " "`,
				len(inputs), files, len(documents), refused, same, moved, names)
		})
	}
}
