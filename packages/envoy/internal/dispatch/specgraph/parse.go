package specgraph

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// blockAttributes is each block the spec files hold, with every attribute it takes after its id,
// in the order Print writes them and Digest records them. Parse refuses every other attribute, so
// no setting it does not list can change what a block means: none makes a failing example
// optional (R9).
var blockAttributes = map[string][]string{
	"requirement": {"kind", "parent", "serves", "about"},
	"check":       {"serves", "subject", "procedure", "runner", "selector", "job", "time_limit"},
	"component":   {"name", "kind", "parent", "joins"},
	"term":        {"name"},
}

// checkFields is the three fields of a check's body, in the order they must come, each starting a
// line as its marker followed by a space.
var checkFields = [...]string{"Claim:", "Procedure:", "Failing example:"}

// heading is a markdown heading line, which a spec file may hold between blocks.
var heading = regexp.MustCompile(`^#{1,6}(\s|$)`)

// problemKind is one kind of problem that Parse gives, an index of problemKinds.
type problemKind int

// The kinds of problem that Parse gives. numProblemKinds counts them, so each new kind is a place
// of problemKinds, and TestEachProblemOfParseSaysWhetherItLosesANode fails until it has a row.
const (
	textOutsideBlock problemKind = iota
	unclosedBlock
	nestedBlock
	badBlockLine
	unknownBlock
	unknownAttribute
	badComponentKind
	badTimeLimit
	badFieldOrder
	claimOfLines
	claimOfSentences
	badEncoding
	headingNamesCode
	numProblemKinds
)

// problemKinds is each kind of problem that Parse gives: its Effect on the tree, which the reader's
// problem method sets on each problem of the kind, and the format of its Detail, which begins with
// its rule. A kind with no entry has no Effect, and a caller that compares reads that as LosesNode.
var problemKinds = [numProblemKinds]struct {
	effect Effect
	format string
}{
	textOutsideBlock: {LosesNode, "text-outside-block: a spec file holds only blocks, headings and blank lines"},
	unclosedBlock:    {LosesNode, "unclosed-block: the block has no closing ::: line"},
	nestedBlock:      {LosesNode, "nested-block: a block cannot hold a block"},
	badBlockLine:     {LosesNode, "bad-block-line: the line opens no block"},
	unknownBlock:     {LosesNode, "unknown-block: %q is not requirement, check, component or term"},
	unknownAttribute: {LosesNode, "unknown-attribute: %s takes no attribute %q"},
	badComponentKind: {LosesNode, `bad-component: kind is "interface" or absent, not %q`},
	badTimeLimit:     {LosesNode, "bad-time-limit: %q is not a positive Go duration"},
	badFieldOrder:    {LosesNode, "bad-field: a check holds one Claim, one Procedure and one Failing example line, in that order"},
	claimOfLines:     {KeepsEachNode, "bad-field: a claim is one line"},
	claimOfSentences: {KeepsEachNode, "bad-field: a claim is one sentence"},
	badEncoding:      {KeepsEachNode, "bad-encoding: a spec file is UTF-8 text with no NUL byte"},
	headingNamesCode: {KeepsEachNode, "code-in-text: heading names code: %s"},
}

// Parse reads the spec files, keyed by their names directly in the spec directory, and gives the
// tree they hold and each line of them that the grammar does not hold.
//
// A block opens on a line that starts with exactly three colons and that
// pmdoc.ParseTypedDirectiveOpen reads, and closes on the next line that is exactly ":::". Outside a
// block a file holds only headings and blank lines, and a heading names no code (code-in-text, as
// Validate reads each text). A check's claim is one line and one sentence, split as a component's
// purpose is (sentenceEnd). A file's leading byte order mark is dropped, a CRLF or a lone CR ends a
// line as an LF does, each line loses its trailing white space, and a body loses its leading and
// trailing blank lines. A file that is not UTF-8, or that holds a NUL byte, is refused at its first
// such line, as the importer of .dispatch/architecture refuses it (both by text.Storable's rule),
// and is read all the same. A block whose opening line the grammar refuses, or whose name is not
// one of the four, gives no node, and its lines up to its closing line are its own; every other
// block gives its node even when Parse finds a problem in it. A block with no closing line runs on
// to the next line that is exactly ":::", so the block after it gives no node either
// (nested-block). So one fault can give more than one problem: a node that names a lost block, or
// that a lost block named, can then break a rule of its own, such as leaf-without-check or
// check-serves.
func Parse(files map[string][]byte) (Tree, []Problem) {
	var r reader
	for _, name := range slices.Sorted(maps.Keys(files)) {
		r.file(name, files[name])
	}
	byIDThenPos := func(aID, bID string, a, b Position) int {
		return cmp.Or(cmp.Compare(aID, bID), cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line))
	}
	slices.SortStableFunc(r.tree.Statements, func(a, b Statement) int { return byIDThenPos(a.ID, b.ID, a.Pos, b.Pos) })
	slices.SortStableFunc(r.tree.Checks, func(a, b Check) int { return byIDThenPos(a.ID, b.ID, a.Pos, b.Pos) })
	slices.SortStableFunc(r.tree.Components, func(a, b Component) int { return byIDThenPos(a.ID, b.ID, a.Pos, b.Pos) })
	slices.SortStableFunc(r.tree.Terms, func(a, b Term) int { return byIDThenPos(a.ID, b.ID, a.Pos, b.Pos) })
	return r.tree, r.problems
}

// reader is the state of one Parse: the tree so far and the problems found so far.
type reader struct {
	tree     Tree
	problems []Problem
}

// block is one block being read: its name, attributes and id, where it opened, its body lines,
// and whether it gives a node.
type block struct {
	name     string
	attrs    pmdoc.Attrs
	id       string
	pos      Position
	body     []line
	readable bool
}

// line is one line of a body and its 1-based number in its file.
type line struct {
	number int
	text   string
}

// node is the id a problem in the block names, "-" when it has none.
func (b *block) node() string {
	if b.id == "" {
		return "-"
	}
	return b.id
}

// problem records a problem of Parse of the kind kind at pos, of the node with the id node: its
// Detail is the kind's format filled with args, its rule the word before the Detail's first colon,
// and its Effect the kind's.
func (r *reader) problem(pos Position, node string, kind problemKind, args ...any) {
	k := problemKinds[kind]
	detail := fmt.Sprintf(k.format, args...)
	rule, _, _ := strings.Cut(detail, ":")
	r.problems = append(r.problems, Problem{Pos: pos, Node: node, Rule: rule, Detail: detail, Effect: k.effect})
}

// file reads one spec file into the tree.
func (r *reader) file(name string, content []byte) {
	text := strings.TrimPrefix(string(content), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if number := unstorableLine(lines); number > 0 {
		r.problem(Position{File: name, Line: number}, "-", badEncoding)
	}
	var open *block
	inParagraph := false
	for i, raw := range lines {
		text := strings.TrimRightFunc(raw, unicode.IsSpace)
		pos := Position{File: name, Line: i + 1}
		if open != nil {
			switch {
			case text == ":::":
				r.finish(open)
				open = nil
			case strings.HasPrefix(text, ":::"):
				if _, _, ok := openingLine(text); ok {
					r.problem(pos, open.node(), nestedBlock)
				} else {
					r.problem(pos, open.node(), badBlockLine)
				}
			default:
				open.body = append(open.body, line{number: pos.Line, text: text})
			}
			continue
		}
		switch {
		case text == "":
			inParagraph = false
		case text == ":::":
			inParagraph = false
			r.problem(pos, "-", badBlockLine)
		case strings.HasPrefix(text, ":::"):
			inParagraph = false
			open = r.open(pos, text)
		case heading.MatchString(text):
			inParagraph = false
			if names := CodeNames(strings.TrimLeft(text, "#")); len(names) > 0 {
				r.problem(pos, "-", headingNamesCode, strings.Join(names, ", "))
			}
		default:
			if !inParagraph {
				r.problem(pos, "-", textOutsideBlock)
			}
			inParagraph = true
		}
	}
	if open != nil {
		r.problem(open.pos, open.node(), unclosedBlock)
		r.finish(open)
	}
}

// unstorableLine is the 1-based number of the first of lines that is not text Dispatch can store,
// UTF-8 with no NUL byte (text.Storable), and 0 when each line is.
func unstorableLine(lines []string) int {
	for i, l := range lines {
		if !text.Storable(l) {
			return i + 1
		}
	}
	return 0
}

// openingLine reads text as the opening line of a block: exactly three colons, then what
// pmdoc.ParseTypedDirectiveOpen reads.
func openingLine(text string) (string, pmdoc.Attrs, bool) {
	if strings.HasPrefix(text, "::::") {
		return "", nil, false
	}
	return pmdoc.ParseTypedDirectiveOpen(text)
}

// open starts the block whose opening line text is at pos, refusing a line that opens no block, a
// block that is not a spec block and an attribute the block does not take.
func (r *reader) open(pos Position, text string) *block {
	name, attrs, ok := openingLine(text)
	b := &block{name: name, attrs: attrs, pos: pos}
	if !ok {
		r.problem(pos, "-", badBlockLine)
		return b
	}
	b.id, _ = attrs[pmdoc.BlockIDAttr].(string)
	allowed, known := blockAttributes[name]
	if !known {
		r.problem(pos, b.node(), unknownBlock, name)
		return b
	}
	for _, attr := range slices.Sorted(maps.Keys(attrs)) {
		if attr != pmdoc.BlockIDAttr && !slices.Contains(allowed, attr) {
			r.problem(pos, b.node(), unknownAttribute, name, attr)
		}
	}
	b.readable = true
	return b
}

// attr is the value of one attribute of b, "" when b does not write it.
func (b *block) attr(name string) string {
	value, _ := b.attrs[name].(string)
	return value
}

// list is the ids one attribute of b names, separated by white space, sorted and unique; nil when
// it names none.
func (b *block) list(name string) []string {
	ids := strings.Fields(b.attr(name))
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// finish adds the node a readable block gives to the tree.
func (r *reader) finish(b *block) {
	if !b.readable {
		return
	}
	switch b.name {
	case "requirement":
		r.tree.Statements = append(r.tree.Statements, Statement{
			ID: b.id, Text: bodyText(b.body), Kind: Kind(b.attr("kind")), Parent: b.attr("parent"),
			Serves: b.list("serves"), About: b.list("about"), Pos: b.pos,
		})
	case "check":
		r.tree.Checks = append(r.tree.Checks, r.check(b))
	case "component":
		c := Component{ID: b.id, Name: b.attr("name"), Purpose: bodyText(b.body), Parent: b.attr("parent"), Joins: b.list("joins"), Pos: b.pos}
		switch kind := b.attr("kind"); kind {
		case "":
		case "interface":
			c.Interface = true
		default:
			r.problem(b.pos, b.node(), badComponentKind, kind)
		}
		r.tree.Components = append(r.tree.Components, c)
	case "term":
		r.tree.Terms = append(r.tree.Terms, Term{ID: b.id, Name: b.attr("name"), Definition: bodyText(b.body), Pos: b.pos})
	}
}

// check reads a check block: its attributes, its test part, its time limit and the three fields of
// its body.
func (r *reader) check(b *block) Check {
	c := Check{
		ID: b.id, Serves: strings.Join(b.list("serves"), " "), Subject: Subject(b.attr("subject")),
		Procedure: Procedure(b.attr("procedure")), Pos: b.pos,
	}
	if runner, selector, job := b.attr("runner"), b.attr("selector"), b.attr("job"); runner != "" || selector != "" || job != "" {
		c.Test = &Test{Runner: runner, Selector: selector, Job: job}
	}
	if limit := b.attr("time_limit"); limit != "" {
		d, err := time.ParseDuration(limit)
		if err != nil || d <= 0 {
			r.problem(b.pos, b.node(), badTimeLimit, limit)
		} else {
			c.TimeLimit = d
		}
	}

	// current is the field the next line continues: -1 before the first field, -2 after a line
	// the grammar refused, until the next field starts.
	var fields [len(checkFields)][]line
	current, next := -1, 0
	for _, l := range b.body {
		if k, rest, ok := fieldStart(l.text); ok {
			if k < next {
				r.problem(Position{File: b.pos.File, Line: l.number}, b.node(), badFieldOrder)
				current = -2
				continue
			}
			current, next = k, k+1
			fields[k] = []line{{number: l.number, text: rest}}
			continue
		}
		switch current {
		case -1:
			if l.text != "" {
				r.problem(Position{File: b.pos.File, Line: l.number}, b.node(), badFieldOrder)
				current = -2
			}
		case -2:
		default:
			fields[current] = append(fields[current], l)
		}
	}
	switch claim := trimBlank(fields[0]); {
	case len(claim) > 1:
		r.problem(Position{File: b.pos.File, Line: claim[1].number}, b.node(), claimOfLines)
	case len(claim) == 1 && sentenceEnd.MatchString(claim[0].text):
		r.problem(Position{File: b.pos.File, Line: claim[0].number}, b.node(), claimOfSentences)
	}
	c.Claim, c.ProcedureText, c.FailingExample = bodyText(fields[0]), bodyText(fields[1]), bodyText(fields[2])
	return c
}

// fieldStart reads text as the first line of a check's field: the field's index and the rest of
// the line after its marker and one space.
func fieldStart(text string) (int, string, bool) {
	for k, marker := range checkFields {
		if text == marker {
			return k, "", true
		}
		if rest, ok := strings.CutPrefix(text, marker+" "); ok {
			return k, rest, true
		}
	}
	return 0, "", false
}

// trimBlank is lines without their leading and trailing blank lines.
func trimBlank(lines []line) []line {
	for len(lines) > 0 && lines[0].text == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1].text == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// bodyText is the text of lines: without leading and trailing blank lines, joined with "\n".
func bodyText(lines []line) string {
	var b strings.Builder
	for i, l := range trimBlank(lines) {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l.text)
	}
	return b.String()
}
