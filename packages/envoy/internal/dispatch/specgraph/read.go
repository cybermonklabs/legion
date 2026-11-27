package specgraph

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"
)

// schemaJSON is the schema of one node: the data of one YAML document of a spec file. It is the
// contract of the files: the reader checks each document against it, the TypeScript test of the
// schema reads it, and a person's editor can check a document against it.
// The TypeScript test of the schema, packages/dispatch/e2e/__tests__/spec-node-schema.test.ts,
// holds ajv and the TypeScript yaml library to the readings of the Go side over
// testdata/schema-cases.json (TestSchemaCases), testdata/nodes (TestNodesDecodeToTheirData) and
// each YAML fixture of testdata, whose data testdata/fixtures.decoded.json holds
// (TestEachYAMLFixtureDecodesAsItsData). The package embeds the file of the schema, so the reader
// opens no file to read it.
//
//go:embed schema/spec-node.schema.json
var schemaJSON []byte

// schemaID is the $id of the schema.
const schemaID = "urn:dispatch:spec-node"

// nodeSchema is the compiled schema. A schema that does not compile is a fault of the build: it
// panics.
var nodeSchema = sync.OnceValue(func() *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		panic(fmt.Sprintf("specgraph: the node schema is not JSON: %v", err))
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(schemaID, doc); err != nil {
		panic(fmt.Sprintf("specgraph: the node schema: %v", err))
	}
	schema, err := compiler.Compile(schemaID)
	if err != nil {
		panic(fmt.Sprintf("specgraph: the node schema does not compile: %v", err))
	}
	return schema
})

// english writes the messages of the schema validator.
var english = message.NewPrinter(language.English)

// yamlLine is the line that a message of yaml.v3 names at its start.
var yamlLine = regexp.MustCompile(`^line ([0-9]+):`)

// readerFaults are the messages of the reader of yaml.v3 (its readerc.go) for a byte that is not
// UTF-8 and for a character that YAML does not allow. The reader names no line, so such a fault is
// at the line of the first character that the reader refuses (refusedLine).
var readerFaults = map[string]bool{
	"control characters are not allowed": true,
	"incomplete UTF-8 octet sequence":    true,
	"invalid leading UTF-8 octet":        true,
	"invalid length of a UTF-8 sequence": true,
	"invalid trailing UTF-8 octet":       true,
	"invalid Unicode character":          true,
}

// refusedLine is the line of the first character of data that the reader of yaml.v3 refuses: a
// byte that is not part of a UTF-8 character, or a character outside the set that YAML allows. It
// is 0 when data has none.
func refusedLine(data []byte) int {
	for offset := 0; offset < len(data); {
		r, width := utf8.DecodeRune(data[offset:])
		if r == utf8.RuneError && width == 1 || !yamlCharacter(r) {
			return lineOf(lineStarts(data), offset)
		}
		offset += width
	}
	return 0
}

// lineStarts is the byte offset of the start of each line of data, from line 1. A line ends at
// each line break that yaml.v3 reads as one: CR LF, and each other CR, LF, U+0085, U+2028 and
// U+2029 (is_break and is_crlf of yaml.v3, in skip_line and read_line of its scanner, the only
// steps that add to its line). So for a file in UTF-8, the line that a message of yaml.v3 or a
// yaml.Node names starts at lineStarts(data)[line-1], and each reader of lines in this package
// counts them with it.
func lineStarts(data []byte) []int {
	starts := []int{0}
	for i := 0; i < len(data); {
		n := lineBreak(data[i:])
		if n == 0 {
			i++
			continue
		}
		i += n
		starts = append(starts, i)
	}
	return starts
}

// lineBreak is the length in bytes of the line break that data starts with, as lineStarts counts
// one, and 0 when data does not start with a line break.
func lineBreak(data []byte) int {
	switch {
	case bytes.HasPrefix(data, []byte("\r\n")):
		return 2
	case data[0] == '\r' || data[0] == '\n':
		return 1
	case bytes.HasPrefix(data, []byte("\u0085")):
		return 2
	case bytes.HasPrefix(data, []byte("\u2028")) || bytes.HasPrefix(data, []byte("\u2029")):
		return 3
	}
	return 0
}

// lineOf is the line, from 1, that holds the byte at offset, of a text whose lines start at
// starts.
func lineOf(starts []int, offset int) int {
	i, found := slices.BinarySearch(starts, offset)
	if found {
		return i + 1
	}
	return i
}

// textLines is each line of text as lineStarts counts them, with its line break; a text that ends
// in a line break has no empty line after it.
func textLines(text string) []string {
	starts := lineStarts([]byte(text))
	all := make([]string, 0, len(starts))
	for i, start := range starts {
		end := len(text)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		if start < end {
			all = append(all, text[start:end])
		}
	}
	return all
}

// yamlCharacter reports whether YAML allows r in a stream, as the reader of yaml.v3 checks it: a
// tab, a line feed, a carriage return, the printable ASCII characters, U+0085, and the rest of
// Unicode except the C1 controls, the surrogates, U+FFFE and U+FFFF.
func yamlCharacter(r rune) bool {
	return r == 0x09 || r == 0x0A || r == 0x0D || 0x20 <= r && r <= 0x7E || r == 0x85 ||
		0xA0 <= r && r <= 0xD7FF || 0xE000 <= r && r <= 0xFFFD || 0x10000 <= r && r <= 0x10FFFF
}

// problemKind is one kind of problem that the reader gives (Parse, ReadLayout), an index of
// problemKinds.
type problemKind int

// The kinds of problem that the reader gives. numProblemKinds counts them, so each new kind is a
// place of problemKinds, and TestEachProblemOfTheReaderSaysWhetherItLosesANode fails until it has
// a row.
const (
	yamlStops problemKind = iota
	yamlDocument
	noJSONForm
	yamlNull
	noNode
	schemaDropped
	schemaKept
	timeLimitDropped
	notPrinted
	numProblemKinds
)

// problemKinds is each kind of problem that the reader gives: its Effect on the tree, which
// readerProblem sets on each problem of the kind, and the format of its Detail, which begins with
// its rule. A kind loses a node (LosesNode) when the tree can miss a node of the file or hold one
// in another text than the file writes:
//   - yamlStops: yaml.v3 stops reading the file, so no document after the fault is in the tree;
//   - yamlDocument: yaml.v3 reads no data from one document, such as one with a key written two
//     times, so it gives no node;
//   - noJSONForm: the data of a document has no JSON form, such as a key that is not a string;
//   - yamlNull: the data of a document holds a null at a place, the document itself, a member or an
//     element of a list (null, ~, or no value), which says nothing and which yaml.v3 reads as the
//     zero value of the place, a real value, such as a statement with no parent. No member of a
//     node takes null, so the document gives no node;
//   - schemaDropped: the schema refuses a value that the node does not hold as the file writes it:
//     a key that the node takes no field for (additional properties), a value of another type, a
//     component kind that is not "component" or "interface", or any refusal of a document that
//     gives no node;
//   - timeLimitDropped: a time limit that is not a positive Go duration, which the check drops;
//   - notPrinted: the file is not as Print writes its nodes. The printed form is the one form that
//     each YAML library reads alike, because the printer writes it; another form can read as two
//     texts in two libraries, such as a plain date, which yaml.v3 reads as a time, or a raw U+2028,
//     which yaml.v3 (YAML 1.1) reads as a line break and a YAML 1.2 library as a character. So the
//     tree that yaml.v3 reads from it is no text that the file holds for each reader.
//     The reader holds no list of such forms: it compares the file with the printed form, so each
//     other form, such as a tag or an anchor, is not-printed too (TestReadRefuses).
//
// A kind keeps each node (KeepsEachNode) when the tree holds each node as the file writes it:
//   - noNode: a file with no document, which holds no node to lose;
//   - schemaKept: the schema refuses a value that the node keeps, such as a value outside a closed
//     set, a text that breaks a pattern or a missing key.
var problemKinds = [numProblemKinds]struct {
	effect Effect
	format string
}{
	yamlStops:        {LosesNode, "bad-yaml: %s"},
	yamlDocument:     {LosesNode, "bad-yaml: %s"},
	noJSONForm:       {LosesNode, "bad-yaml: the document holds a value that JSON cannot hold"},
	yamlNull:         {LosesNode, "bad-yaml: %s is null"},
	noNode:           {KeepsEachNode, "no-node: a spec file holds one or more nodes"},
	schemaDropped:    {LosesNode, "schema: %s"},
	schemaKept:       {KeepsEachNode, "schema: %s"},
	timeLimitDropped: {LosesNode, "bad-time-limit: %q is not a positive Go duration"},
	notPrinted:       {LosesNode, "not-printed: %s"},
}

// readerProblem is the problem of the kind kind at pos, of the node id ("" for none): its Detail
// is the kind's format filled with args, its rule the word before the Detail's first colon, and its
// Effect the kind's.
func readerProblem(pos Position, id string, kind problemKind, args ...any) Problem {
	k := problemKinds[kind]
	p := idProblem(pos, id, fmt.Sprintf(k.format, args...))
	p.Effect = k.effect
	return p
}

// document is one YAML document of a spec file: the line of its "---" (or of its first line, for a
// document that has none) and its data written as JSON, nil when the reader refused the data.
type document struct {
	line int
	data []byte
}

// yamlStop is the fault of the YAML that stopped the reading of a file: its line, and the words of
// yaml.v3 for it, cut, as the Detail of its bad-yaml problem holds them after "bad-yaml: ".
type yamlStop struct {
	line  int
	fault string
}

// readDocuments reads the documents of one spec file, the steps of readFile before the schema: it
// decodes each document with yaml.v3 into a yaml.Node, then into a value, and writes the value as
// JSON. A fault of the YAML is bad-yaml: one that ends the stream stops the reading, keeps the
// documents before it and answers its stop (nil for a file read whole); one that only its document
// holds, such as a key written two times, a key that is not a string or a null at a place, keeps
// the document with no data. A null is named by its place (nullPlace, in the order of the JSON
// form), as decodeJSON names a null of a JSON file. A file with no document is no-node.
// The detail of a null is bad-yaml: the value at "<JSON pointer>" is null, or bad-yaml: the
// document is null for a document that is null.
func readDocuments(name string, data []byte) (documents []document, problems []Problem, stop *yamlStop) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc yaml.Node
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			line := 1
			if readerFaults[strings.TrimPrefix(err.Error(), "yaml: ")] {
				line = max(refusedLine(data), 1)
			}
			stop = &yamlStop{}
			stop.line, stop.fault = yamlFault(err.Error(), line)
			problems = append(problems, readerProblem(Position{File: name, Line: stop.line}, "", yamlStops, stop.fault))
			break
		}
		d := document{line: doc.Line}
		var value any
		var typeErr *yaml.TypeError
		switch err := doc.Decode(&value); {
		case errors.As(err, &typeErr):
			for _, entry := range typeErr.Errors {
				problems = append(problems, yamlProblem(name, yamlDocument, entry, d.line))
			}
		case err != nil:
			problems = append(problems, yamlProblem(name, yamlDocument, err.Error(), d.line))
		default:
			js, err := json.Marshal(value)
			if err != nil || !stringKeys(value) {
				problems = append(problems, readerProblem(Position{File: name, Line: d.line}, "", noJSONForm))
				break
			}
			place, null, err := nullPlace(js)
			if err != nil {
				problems = append(problems, readerProblem(Position{File: name, Line: d.line}, "", noJSONForm))
				break
			}
			if null {
				where := "the document"
				if place != "" {
					where = "the value at " + quoted(place)
				}
				problems = append(problems, readerProblem(Position{File: name, Line: d.line}, "", yamlNull, where))
				break
			}
			d.data = js
		}
		documents = append(documents, d)
	}
	if len(documents) == 0 && len(problems) == 0 {
		problems = append(problems, readerProblem(Position{File: name, Line: 1}, "", noNode))
	}
	return documents, problems, stop
}

// yamlProblem is the bad-yaml problem of the kind kind (yamlStops or yamlDocument) for one message
// of yaml.v3, at its line and in its words (yamlFault).
func yamlProblem(name string, kind problemKind, msg string, line int) Problem {
	line, msg = yamlFault(msg, line)
	return readerProblem(Position{File: name, Line: line}, "", kind, msg)
}

// yamlFault is the line and the words of one message of yaml.v3: the message without its "yaml: "
// prefix, each value of the input that it holds cut (cutYAMLValues), at the line that it names,
// else at line.
func yamlFault(msg string, line int) (int, string) {
	msg = strings.TrimPrefix(msg, "yaml: ")
	if m := yamlLine.FindStringSubmatch(msg); m != nil {
		line, _ = strconv.Atoi(m[1])
	}
	return line, cutYAMLValues(msg)
}

// yamlValues are the messages of yaml.v3 (v3.0.1: decode.go and resolve.go) that hold a value of
// the input, each with one group for that value: a key written two times, which the message
// quotes as Go quotes a string (goQuoted); an alias of no anchor and an anchor whose value holds
// its alias, by its name; a key that is a list or a map, as Go writes it; and a value that its tag
// does not take. Each other message of yaml.v3 holds no value of the input, and its type errors
// cut a value to 7 bytes themselves.
var yamlValues = []struct {
	message  *regexp.Regexp
	goQuoted bool
}{
	{regexp.MustCompile(`^line [0-9]+: mapping key ("(?:[^"\\]|\\.)*") already defined at line [0-9]+$`), true},
	{regexp.MustCompile(`^unknown anchor '(.*)' referenced$`), false},
	{regexp.MustCompile(`^anchor '(.*)' value contains itself$`), false},
	{regexp.MustCompile(`^invalid map key: (.*)$`), false},
	{regexp.MustCompile("^cannot decode !!\\w+ `(.*)` as a !!\\w+$"), false},
}

// cutYAMLValues is msg, a message of yaml.v3, with the value of the input that one of yamlValues
// finds in it cut (Cut): a value that the message quotes as Go does is unquoted, cut and quoted
// again. A short value, and a message of no value, stay as they are.
func cutYAMLValues(msg string) string {
	for _, v := range yamlValues {
		m := v.message.FindStringSubmatchIndex(msg)
		if m == nil {
			continue
		}
		value := msg[m[2]:m[3]]
		cut := Cut(value)
		if v.goQuoted {
			if unquoted, err := strconv.Unquote(value); err == nil {
				cut = quoted(unquoted)
			}
		}
		return msg[:m[2]] + cut + msg[m[3]:]
	}
	return msg
}

// stringKeys reports whether each mapping of value has strings for keys, as JSON's objects do:
// yaml.v3 gives a mapping with another key as a map[any]any, which encoding/json would write with
// the key as a string.
func stringKeys(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for _, item := range v {
			if !stringKeys(item) {
				return false
			}
		}
	case []any:
		for _, item := range v {
			if !stringKeys(item) {
				return false
			}
		}
	case map[any]any:
		return false
	}
	return true
}

// readFile reads one spec file: each of its documents, in order, with its node (Pos{name, the
// line of its document's "---"}, nil for a document that gives none) and its text, and each
// problem. A document whose data the schema refuses still gives its node when its keys have the
// schema's types, so one fault gives one problem. When the file has no problem that loses a node,
// it must be exactly the documents that printNode writes for its nodes, in its order: one node has
// one spelling, and it is the one that each YAML library reads alike. So a file with a problem that
// keeps each node and that is not in its printed form gets the not-printed problem too, and no
// comparison of the tree trusts a reading of a file that is not printed. The texts of the
// documents, joined, are data: the text of a document runs from the line of its "---" to the next
// document's (the first from the start of the file), each line as yaml.v3 counts it (lineStarts),
// and the part of a file after a fault that stopped the reading is a document of its own with no
// node, from the first line after the last document read that starts the next one ("---" or
// "..."). uncounted is why the reader cannot say for certain which documents the file holds, ""
// when it can (splitFile).
func readFile(name string, data []byte) (documents []Document, problems []Problem, uncounted string) {
	read, problems, stopped := readDocuments(name, data)
	stop, fault := 0, ""
	if stopped != nil {
		stop, fault = stopped.line, stopped.fault
	}
	var nodes []Node
	var lines []int
	for _, d := range read {
		if d.data == nil {
			continue
		}
		node, found := readNode(name, d)
		problems = append(problems, found...)
		if node != nil {
			nodes = append(nodes, node)
			lines = append(lines, d.line)
		}
	}
	if !slices.ContainsFunc(problems, func(p Problem) bool { return p.Effect == LosesNode }) {
		if u := firstUnprinted(data, nodes, lines); u.line > 0 {
			problems = append(problems, readerProblem(Position{File: name, Line: u.line}, u.id(), notPrinted, u.detail()))
		}
	}
	documents, uncounted = splitFile(data, read, nodes, lines, stop, fault)
	return documents, problems, uncounted
}

// splitFile is the documents of data, each with its node: documents is each document read, at
// its line, nodes the node of each document at lines that gave one, and stop the line of the
// fault that stopped the reading, 0 when the reader read the whole file, and fault the words of
// yaml.v3 for it. uncounted is why the texts may not be the documents that yaml.v3 read, "" when
// they are: a file that yaml.v3 reads as UTF-16, whose lines lineStarts does not count; a file
// whose reading stopped, since the reader counts no document after the fault; and, as a check of
// lineStarts, a document at a line that the file does not hold or at the line of the document
// before it.
func splitFile(data []byte, documents []document, nodes []Node, lines []int, stop int, fault string) ([]Document, string) {
	text := string(data)
	starts := lineStarts(data)
	offset := func(line int) int { return starts[min(max(line, 1), len(starts))-1] }
	uncounted := ""
	switch {
	case bytes.HasPrefix(data, []byte("\xff\xfe")) || bytes.HasPrefix(data, []byte("\xfe\xff")):
		uncounted = "the YAML library reads it as UTF-16, and the reader counts the lines of UTF-8 only"
	case stop > 0:
		uncounted = fmt.Sprintf("the YAML library stops at line %d (%s), so the reader does not know the documents after that line", stop, fault)
	}
	cuts := make([]int, 0, len(documents)+1)
	for i, d := range documents {
		at := offset(d.line)
		switch {
		case i == 0:
			at = 0
		case uncounted == "" && (d.line > len(starts) || d.line <= documents[i-1].line):
			uncounted = fmt.Sprintf("the YAML library reads a document at line %d, which the reader does not count after the document at line %d", d.line, documents[i-1].line)
		}
		cuts = append(cuts, at)
	}
	if stop > 0 {
		rest := len(text)
		if len(documents) == 0 {
			rest = 0
		} else {
			for line := documents[len(documents)-1].line + 1; line <= len(starts); line++ {
				if l := text[offset(line):]; strings.HasPrefix(l, "---") || strings.HasPrefix(l, "...") {
					rest = offset(line)
					break
				}
			}
		}
		cuts = append(cuts, rest)
	}
	var result []Document
	k := 0 // nodes and lines are in the order of documents, and each document has its own line
	for i, at := range cuts {
		end := len(text)
		if i+1 < len(cuts) {
			end = cuts[i+1]
		}
		if at == end && i == len(documents) {
			continue // a stop at the end of the file leaves no text after the documents
		}
		d := Document{Text: text[at:end]}
		if i < len(documents) && k < len(lines) && lines[k] == documents[i].line {
			d.Node = nodes[k]
			k++
		}
		result = append(result, d)
	}
	if len(result) == 0 && len(text) > 0 {
		result = []Document{{Text: text}}
	}
	return result, uncounted
}

// schemaRefusal is one place where the data of a node breaks the schema: its detail, "<where>:
// <message>"; whether the value at that place is one that no node holds as the data writes it, a
// key that the node takes no field for (additional properties) or a value of another type; and
// whether the place is the kind of the node.
type schemaRefusal struct {
	detail    string
	dropped   bool
	kindPlace bool
}

// problemKind is the kind of the problem of r for a node that its document gives (node) and holds
// in its kind (keptKind; false for a component of a kind that is not "component" or "interface"):
// schemaDropped when the document gives no node, when r refuses a key or a type, and when r is at
// the kind that the node does not hold; else schemaKept.
func (r schemaRefusal) problemKind(node, keptKind bool) problemKind {
	if !node || r.dropped || r.kindPlace && !keptKind {
		return schemaDropped
	}
	return schemaKept
}

// schemaRefusals is each place where the data inst breaks the schema, each detail once, in the
// order of the details.
func schemaRefusals(inst any) []schemaRefusal {
	var refused *jsonschema.ValidationError
	if !errors.As(nodeSchema().Validate(inst), &refused) {
		return nil
	}
	var all []schemaRefusal
	for _, leaf := range schemaLeaves(refused) {
		_, extra := leaf.ErrorKind.(*kind.AdditionalProperties)
		_, otherType := leaf.ErrorKind.(*kind.Type)
		all = append(all, schemaRefusal{detail: schemaDetail(leaf), dropped: extra || otherType,
			kindPlace: slices.Equal(leaf.InstanceLocation, []string{"kind"})})
	}
	slices.SortFunc(all, func(a, b schemaRefusal) int { return strings.Compare(a.detail, b.detail) })
	return slices.CompactFunc(all, func(a, b schemaRefusal) bool { return a.detail == b.detail })
}

// schemaProblems is each problem that the schema finds in the data of a node of tree, at the
// node's position, as Parse finds it in the node's document. A tree that a caller builds with no
// reader, as the pilot does, meets the same rules of shape as the files: CheckStates judges the
// tree by schemaProblems and Validate.
func schemaProblems(tree Tree) []Problem {
	var problems []Problem
	for _, n := range Nodes(tree) {
		file, _ := n.file()
		js, err := json.Marshal(file)
		if err != nil {
			continue
		}
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
		if err != nil {
			continue
		}
		for _, r := range schemaRefusals(inst) {
			problems = append(problems, readerProblem(n.NodePos(), n.NodeID(), r.problemKind(true, true), r.detail))
		}
	}
	return problems
}

// readNode checks the data of one document against the schema, and gives its node (nodeOf) and
// each problem. It sets the Effect of each problem of the schema by what the node keeps: a
// refusal of a value that the node holds as the data writes it keeps each node (schemaKept); a
// refusal of a document that gives no node, of a key that the node takes no field for, of a value
// of another type, or of a component kind that the node drops, loses or changes the node
// (schemaDropped). A time limit that the check drops is timeLimitDropped.
func readNode(name string, d document) (Node, []Problem) {
	pos := Position{File: name, Line: d.line}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(d.data))
	if err != nil {
		return nil, []Problem{readerProblem(pos, "", noJSONForm)}
	}
	object, _ := inst.(map[string]any)
	id, _ := object["id"].(string)
	typ, _ := object["type"].(string)
	node, keptKind, droppedLimit := nodeOf(d.data, typ, pos)
	var problems []Problem
	for _, r := range schemaRefusals(inst) {
		problems = append(problems, readerProblem(pos, id, r.problemKind(node != nil, keptKind), r.detail))
	}
	if droppedLimit != "" {
		problems = append(problems, readerProblem(pos, id, timeLimitDropped, Cut(droppedLimit)))
	}
	return node, problems
}

// nodeOf is the node of data, the data of a document of the type typ at pos: nil for a type that
// is not one of the four, or for a key whose value is of another type than its field's. keptKind is
// false for a component whose kind the node does not hold, one that is not "component" or
// "interface", and droppedLimit is a time limit that the check drops, one that is not a positive Go
// duration ("" for none).
func nodeOf(data []byte, typ string, pos Position) (node Node, keptKind bool, droppedLimit string) {
	switch typ {
	case "requirement":
		var f requirementFile
		if json.Unmarshal(data, &f) != nil {
			return nil, true, ""
		}
		return Statement{ID: f.ID, Text: joinLines(f.Text), Kind: Kind(f.Kind), Parent: f.Parent, Serves: f.Serves, About: f.About, Pos: pos}, true, ""
	case "check":
		var f checkFile
		if json.Unmarshal(data, &f) != nil {
			return nil, true, ""
		}
		c := Check{
			ID: f.ID, Serves: f.Serves, Claim: f.Claim, ProcedureText: joinLines(f.ProcedureText), FailingExample: joinLines(f.FailingExample),
			Subject: Subject(f.Subject), Procedure: Procedure(f.Procedure), Pos: pos,
		}
		if f.Test != nil {
			c.Test = &Test{Runner: f.Test.Runner, Selector: f.Test.Selector, Job: f.Test.Job}
		}
		if f.TimeLimit != "" {
			limit, err := time.ParseDuration(f.TimeLimit)
			if err != nil || limit <= 0 {
				return c, true, f.TimeLimit
			}
			c.TimeLimit = limit
		}
		return c, true, ""
	case "component":
		var f componentFile
		if json.Unmarshal(data, &f) != nil {
			return nil, true, ""
		}
		holds := f.Kind == "" || f.Kind == "component" || f.Kind == "interface"
		return Component{ID: f.ID, Name: f.Name, Purpose: joinLines(f.Purpose), Parent: f.Parent, Interface: f.Kind == "interface", Joins: f.Joins, Pos: pos}, holds, ""
	case "term":
		var f termFile
		if json.Unmarshal(data, &f) != nil {
			return nil, true, ""
		}
		return Term{ID: f.ID, Name: f.Name, Definition: joinLines(f.Definition), Pos: pos}, true, ""
	}
	return nil, true, ""
}

// schemaLeaves is each error of the tree under e that has no cause of its own: the places where
// the data breaks the schema.
func schemaLeaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(e.Causes) == 0 {
		return []*jsonschema.ValidationError{e}
	}
	var leaves []*jsonschema.ValidationError
	for _, cause := range e.Causes {
		leaves = append(leaves, schemaLeaves(cause)...)
	}
	return leaves
}

// schemaDetail is "<where>: <message>" for one leaf: where is the JSON pointer of the value it
// refuses, "node" for the node itself, and the message is the validator's, except for a key that
// the schema forbids, which the validator names only "false schema". The pointer and each value of
// the input in the message are cut (Cut): the text that a pattern refuses, and the keys that the
// node does not take, as one list. Each other message of the schema's kinds names values of the
// schema, or a type or a count.
func schemaDetail(leaf *jsonschema.ValidationError) string {
	where := Cut(jsonPointer(leaf.InstanceLocation))
	if where == "" {
		where = "node"
	}
	switch k := leaf.ErrorKind.(type) {
	case *kind.FalseSchema:
		return where + ": the node takes no such key"
	case *kind.Pattern:
		return where + ": " + (&kind.Pattern{Got: Cut(k.Got), Want: k.Want}).LocalizedString(english)
	case *kind.AdditionalProperties:
		msg := k.LocalizedString(english)
		const before, after = "additional properties ", " not allowed"
		if list, found := strings.CutPrefix(msg, before); found {
			if list, found = strings.CutSuffix(list, after); found {
				msg = before + Cut(list) + after
			}
		}
		return where + ": " + msg
	}
	return where + ": " + leaf.ErrorKind.LocalizedString(english)
}

// pointerEscape writes a token of a JSON pointer as RFC 6901 writes it.
var pointerEscape = strings.NewReplacer("~", "~0", "/", "~1")

// jsonPointer is the JSON pointer of a location within a document: "" for the document itself.
func jsonPointer(tokens []string) string {
	var b strings.Builder
	for _, token := range tokens {
		b.WriteString("/" + pointerEscape.Replace(token))
	}
	return b.String()
}

// unprinted is the first line of a file that is not as printNode writes the nodes of its
// documents: the line, the node whose document holds it (nil for none), and the text of that line
// in the file (got) and in the documents that printNode writes (want), each with its line break,
// "" past the end of its text. For a node that printNode refuses, line is the line of its
// document, and refused is the refusal.
type unprinted struct {
	line      int
	node      Node
	got, want string
	refused   error
}

// detail is the detail of the not-printed problem after its rule: the two texts of the line,
// quoted as Go quotes a string so that a space at the end, a carriage return or a byte order mark
// shows, each cut (quoted), what differs in kind, and the command that writes each node in its
// printed form.
func (u unprinted) detail() string {
	if u.refused != nil {
		return fmt.Sprintf("print writes no document for the node at line %d: %v", u.line, u.refused)
	}
	got, want := "past the end of the file", "nothing"
	if u.got != "" {
		got = quoted(u.got)
	}
	if u.want != "" {
		want = quoted(u.want)
	}
	return fmt.Sprintf("line %d is %s, and print writes %s there (%s); spec-check print writes each node in that form", u.line, got, want, lineDifference(u.got, u.want))
}

// lineDifference names in kind how the line got of a file differs from the line want that print
// writes: a byte order mark, a line break that is not LF or no line break, a space or a tab, a
// comment, quotes, an indent, other spaces, an order of keys, a line that only one of them holds,
// or another spelling.
func lineDifference(got, want string) string {
	if got == "" {
		return "the file ends before this line"
	}
	if want == "" {
		return "a line after the last node"
	}
	if strings.Contains(got, "\ufeff") {
		return "a byte order mark"
	}
	body := strings.TrimRight(got, "\r\n\u0085\u2028\u2029")
	wantBody := strings.TrimSuffix(want, "\n")
	switch end := got[len(body):]; {
	case end == "\n":
	case strings.HasPrefix(end, "\r"):
		return "a carriage return: print ends each line with a line feed only"
	case end != "":
		r, _ := utf8.DecodeRuneInString(end)
		return fmt.Sprintf("the character %U, which YAML reads as a line break", r)
	case body == wantBody:
		return "no line break at the end of the file"
	}
	key := func(line string) string {
		if k, _, found := strings.Cut(line, ":"); found && !strings.HasPrefix(line, " ") {
			return k
		}
		return ""
	}
	switch {
	case strings.TrimRight(body, " \t") == wantBody:
		return "a space at the end of the line"
	case strings.Contains(body, "\t"):
		return "a tab"
	case strings.HasPrefix(strings.TrimLeft(body, " "), "#") || strings.Contains(body, " #") && !strings.Contains(wantBody, " #"):
		return "a comment"
	case quoteMarks.Replace(body) == quoteMarks.Replace(wantBody):
		return "other quotes"
	case strings.TrimLeft(body, " ") == strings.TrimLeft(wantBody, " "):
		return "another indent"
	case strings.Join(strings.Fields(body), " ") == strings.Join(strings.Fields(wantBody), " "):
		return "other spaces"
	case key(body) != "" && key(wantBody) != "" && key(body) != key(wantBody):
		return "another order of keys"
	}
	return "another spelling"
}

// quoteMarks removes the quote marks of a line.
var quoteMarks = strings.NewReplacer(`'`, "", `"`, "")

// firstUnprinted is the first line of data that is not as printNode writes nodes, which the
// documents at lines hold, with the node whose document holds that line; its line is 0 when data
// is exactly their documents. Lines are counted as yaml.v3 counts them (textLines). Past the end
// of the shorter text, the line is the one after it. A node that printNode refuses is at the line
// of its document. With no node, the node is nil: a file whose documents give no node is not as
// Print writes any node.
func firstUnprinted(data []byte, nodes []Node, lines []int) unprinted {
	var printed strings.Builder
	for i, node := range nodes {
		document, err := printNode(node)
		if err != nil {
			return unprinted{line: lines[i], node: node, refused: err}
		}
		printed.WriteString(document)
	}
	if printed.String() == string(data) {
		return unprinted{}
	}
	got, want := textLines(string(data)), textLines(printed.String())
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	u := unprinted{line: i + 1}
	if i < len(got) {
		u.got = got[i]
	}
	if i < len(want) {
		u.want = want[i]
	}
	for k, start := range lines {
		if k == 0 || start <= u.line {
			u.node = nodes[k]
		}
	}
	return u
}

// id is the id of the node of u, "" for none.
func (u unprinted) id() string {
	if u.node == nil {
		return ""
	}
	return u.node.NodeID()
}

// FileSuffix is the ending of the name of a spec file.
const FileSuffix = ".yaml"

// IsFile reports whether one entry of the spec directory is a spec file: a regular file (kind
// notes.KindFile) directly in the directory, whose name ends in FileSuffix. Each reader of the
// spec files takes its entries by this rule: spec-check, through notes.ReadSet, ReadScenario for
// the files of each commit, and Dispatch's results route, through the GitHub client's listing of
// a commit's spec directory (githubapp.Client.Dir).
func IsFile(name string, entryKind notes.Kind) bool {
	return entryKind == notes.KindFile && !strings.Contains(name, "/") && strings.HasSuffix(name, FileSuffix)
}

// ReadLayout reads the spec files, keyed by their names directly in the spec directory, as Parse
// does, and keeps each document of each file with its node and its text, so Layout.Bytes gives
// each file back byte for byte. Parse is ReadLayout and the tree of its documents, so one reader
// serves both. Uncounted names each file whose documents the reader cannot say for certain. The
// problems are sorted by SortProblems.
func ReadLayout(files map[string][]byte) (Layout, []Problem) {
	l := Layout{Files: make(map[string][]Document, len(files))}
	var problems []Problem
	for _, name := range slices.Sorted(maps.Keys(files)) {
		documents, found, uncounted := readFile(name, files[name])
		l.Files[name] = documents
		problems = append(problems, found...)
		if uncounted != "" {
			if l.Uncounted == nil {
				l.Uncounted = map[string]string{}
			}
			l.Uncounted[name] = uncounted
		}
	}
	SortProblems(problems)
	return l, problems
}

// Parse reads the spec files, keyed by their names directly in the spec directory, and gives the
// tree they hold, each list sorted by ID then Pos, and every problem, sorted by SortProblems.
//
// A spec file holds one or more nodes, one YAML document for each. The reader decodes each
// document with yaml.v3 (bad-yaml for YAML it does not read, a key written two times, a value
// that JSON cannot hold, or a null at any place, which gives no node), checks its data against the
// schema of schema/spec-node.schema.json (schema, with the JSON pointer of the value it refuses),
// and reads a positive Go duration from a time limit (bad-time-limit). A document that the schema
// refuses still gives its node when its keys have the schema's types, so one fault gives one
// problem. A file with no document is no-node, and a file with no other problem must be exactly
// the documents that Print writes for its nodes, in its order (not-printed): the reader normalizes
// nothing, so one node has one spelling in git. A node is at the line of its document's "---".
// Print writes no comment, anchor, tag, flow list, block scalar, other order of keys, other
// quotes, carriage return or byte order mark, so the reader refuses each of them as not-printed
// (TestReadRefuses).
func Parse(files map[string][]byte) (Tree, []Problem) {
	l, problems := ReadLayout(files)
	return l.tree(), problems
}

// tree is the node of each document of l, each list sorted by ID, then by Pos.
func (l Layout) tree() Tree {
	var tree Tree
	for _, name := range slices.Sorted(maps.Keys(l.Files)) {
		for _, d := range l.Files[name] {
			if d.Node != nil {
				d.Node.addTo(&tree)
			}
		}
	}
	byIDThenPos := func(aID, bID string, a, b Position) int {
		return cmp.Or(cmp.Compare(aID, bID), cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line))
	}
	slices.SortStableFunc(tree.Statements, func(a, b Statement) int { return byIDThenPos(a.ID, b.ID, a.Pos, b.Pos) })
	slices.SortStableFunc(tree.Checks, func(a, b Check) int { return byIDThenPos(a.ID, b.ID, a.Pos, b.Pos) })
	slices.SortStableFunc(tree.Components, func(a, b Component) int { return byIDThenPos(a.ID, b.ID, a.Pos, b.Pos) })
	slices.SortStableFunc(tree.Terms, func(a, b Term) int { return byIDThenPos(a.ID, b.ID, a.Pos, b.Pos) })
	return tree
}
