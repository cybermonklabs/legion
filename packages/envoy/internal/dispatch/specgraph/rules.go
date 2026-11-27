package specgraph

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Options are the inputs of Validate beyond the files.
type Options struct {
	// Retired is each id the approved spec removed, which no node may hold again (R1). nil checks
	// no id: the files hold no history, so only the approved spec can say which ids were removed.
	Retired []string
}

// Validate finds each fault of the tree that a program can find beyond the schema, sorted by file,
// line, node and rule. The schema of the files (schema/spec-node.schema.json, which Parse checks)
// states presence, type and closed values; Validate states words and meaning, and passes over each
// value that the schema refuses (an empty value, or a reference that is not an id), so one fault
// gives one problem:
//
//   - an id: doubled-id (one id on two nodes of any kind), bad-id (an id with a word that states a
//     position), retired-id (an id the approved spec removed);
//   - a statement: no-text (a text with no letter or digit), no-parent, missing-target (a serves or
//     about relation to a node the files do not hold as a statement, or as a component or
//     interface), leaf-without-check, supported-without-check (a statement with statements under
//     it that other statements serve, and that no check serves: R5), and loop (a cycle through
//     parent and serves relations, or through the parents of components);
//   - a check: check-serves (a statement that the files do not hold), no-claim, no-procedure,
//     no-failing-example (each a text with no letter or digit), bad-claim (a claim of more than one
//     sentence), no-test and no-job (a runner, selector or job with no word), bad-selector (a go
//     selector that picks no one test);
//   - a component: bad-component (a name or a purpose with no letter or digit, a purpose that is
//     not one sentence, or a parent that is no component) and bad-interface (a joined node that is
//     no component);
//   - a term: bad-term (a name or a definition with no letter or digit);
//   - any text: code-in-text, a name of code in a statement's text, a check's claim, procedure or
//     failing example, a component's name or purpose, or a term's name or definition (R4 and R15).
//     A check's test part and job name code by design and are not read.
//
// The rule no-parent is a parent of a statement that the files do not hold as a statement, and
// leaf-without-check is a statement that is the parent of no statement and that no check serves.
func Validate(t Tree, o Options) []Problem {
	v := validator{tree: t, kinds: map[string]string{}, retired: map[string]bool{}, edges: map[string][]edge{},
		checked: map[string]bool{}, parents: map[string]bool{}, servedBy: map[string]bool{}}
	for _, id := range o.Retired {
		v.retired[id] = true
	}
	for _, c := range t.Checks {
		v.checked[c.Serves] = true
	}
	for _, s := range t.Statements {
		v.parents[s.Parent] = true
		for _, id := range s.Serves {
			if id != s.ID {
				v.servedBy[id] = true
			}
		}
	}
	v.ids()
	for _, s := range t.Statements {
		v.statement(s)
	}
	for _, c := range t.Checks {
		v.check(c)
	}
	for _, c := range t.Components {
		v.component(c)
	}
	for _, term := range t.Terms {
		if missing(term.Name) || missing(term.Definition) {
			v.problem(term.Pos, term.ID, ruleBadTerm)
		}
		v.code(term.Pos, term.ID, "name", term.Name)
		v.code(term.Pos, term.ID, "definition", term.Definition)
	}
	for _, cycle := range findLoops(v.edges) {
		v.problem(v.first[cycle[0]], cycle[0], ruleLoop, loopDetail(cycle))
	}
	SortProblems(v.problems)
	return v.problems
}

// validator is the state of one Validate: the kind and the first position of each id, the
// relations the loop finder walks, each id that a check serves, that a statement names as its
// parent and that another statement serves, read once so that a statement's rules read no list of
// the tree, and the problems found so far.
type validator struct {
	tree     Tree
	kinds    map[string]string // id -> kindOf its first node
	first    map[string]Position
	retired  map[string]bool
	edges    map[string][]edge
	checked  map[string]bool // each id that a check serves
	parents  map[string]bool // each id that a statement names as its parent
	servedBy map[string]bool // each id that a statement other than itself serves
	problems []Problem
}

// relationRule is one relation that a node of one kind sets: its member, and the kinds of node that
// it may name. Each is a named value below, the one rule of its relation, which Validate reads in
// the files and the approved reader in the approved spec (relationFault); a rule is read by its
// name, so a misspelt one does not compile, where a misspelt key of a map would read no kind and
// refuse every such relation.
type relationRule struct {
	member string
	kinds  []string
}

// The relations of each kind of node, by the kind that sets them and the member.
var (
	statementParent = relationRule{member: "parent", kinds: []string{"statement"}}
	statementServes = relationRule{member: "serves", kinds: []string{"statement"}}
	statementAbout  = relationRule{member: "about", kinds: []string{"component", "interface"}}
	checkServes     = relationRule{member: "serves", kinds: []string{"statement"}}
	componentParent = relationRule{member: "parent", kinds: []string{"component"}}
	componentJoins  = relationRule{member: "joins", kinds: []string{"component"}}
)

// ruleKind is one kind of problem that Validate gives, an index of ruleKinds.
type ruleKind int

// The kinds of problem that Validate gives. numRuleKinds counts them, so each new kind is a place
// of ruleKinds, which ProblemKinds lists, and the table test of the lines that print a problem
// (cmd/spec-check) fails until it has a row.
const (
	ruleDoubledID ruleKind = iota
	ruleBadID
	ruleRetiredID
	ruleNoText
	ruleNoParent
	ruleMissingTarget
	ruleTargetOfAnotherKind
	ruleLeafWithoutCheck
	ruleSupportedWithoutCheck
	ruleLoop
	ruleCheckServes
	ruleNoClaim
	ruleNoProcedure
	ruleNoFailingExample
	ruleBadClaim
	ruleNoTest
	ruleBadSelector
	ruleNoJob
	ruleBadComponent
	ruleComponentParent
	ruleBadInterface
	ruleBadTerm
	ruleCodeInText
	numRuleKinds
)

// ruleKinds is the format of the Detail of each kind of problem that Validate gives, which begins
// with its rule; validator.problem fills it. Each value of the input in it is cut where the rule
// passes it (Cut), so a long id or a long name of code prints its start and its end.
var ruleKinds = [numRuleKinds]string{
	ruleDoubledID:             "doubled-id: also at %s:%d",
	ruleBadID:                 "bad-id: %q is not an id: words of lower-case ASCII letters and digits joined by single hyphens, with no word that states a position",
	ruleRetiredID:             "retired-id: the approved spec removed this id",
	ruleNoText:                "no-text: a statement has a text",
	ruleNoParent:              "no-parent: the files hold no statement %q",
	ruleMissingTarget:         "missing-target: %s names %q, which the files do not hold",
	ruleTargetOfAnotherKind:   "missing-target: %s names %q, which is a %s, not a %s",
	ruleLeafWithoutCheck:      "leaf-without-check: no statement is under it and no check serves it",
	ruleSupportedWithoutCheck: "supported-without-check: other statements serve it and no check serves it",
	ruleLoop:                  "loop: %s",
	ruleCheckServes:           "check-serves: a check serves exactly one statement of the files",
	ruleNoClaim:               "no-claim: the check has no claim",
	ruleNoProcedure:           "no-procedure: the check has no procedure",
	ruleNoFailingExample:      "no-failing-example: the check has no failing example",
	ruleBadClaim:              "bad-claim: a claim is one sentence",
	ruleNoTest:                "no-test: a program check names its runner and its selector",
	ruleBadSelector:           `bad-selector: a go selector is "<import path> <TopLevelTestName>"`,
	ruleNoJob:                 "no-job: a program check names the job that runs its test",
	ruleBadComponent:          "bad-component: a component has a name and one sentence of purpose",
	ruleComponentParent:       "bad-component: parent %q is no component",
	ruleBadInterface:          "bad-interface: joins %q, which is no component",
	ruleBadTerm:               "bad-term: a term has a name and a definition",
	ruleCodeInText:            "code-in-text: %s names code: %s",
}

// problem records the problem of the kind kind of node at pos: its Detail is the kind's format
// filled with args, and its rule the word before the Detail's first colon.
func (v *validator) problem(pos Position, node string, kind ruleKind, args ...any) {
	v.problems = append(v.problems, idProblem(pos, node, fmt.Sprintf(ruleKinds[kind], args...)))
}

// ids judges the id of every node, and records the kind and the first position of each, in file
// and line order, so a doubled id names the first place that holds it. An id that is not of the
// schema's shape (an empty one among them) is the schema's problem, and passes here.
func (v *validator) ids() {
	type held struct {
		id, kind string
		pos      Position
	}
	nodes := Nodes(v.tree)
	all := make([]held, 0, len(nodes))
	for _, n := range nodes {
		all = append(all, held{n.NodeID(), n.KindName(), n.NodePos()})
	}
	slices.SortStableFunc(all, func(a, b held) int {
		return cmp.Or(cmp.Compare(a.pos.File, b.pos.File), cmp.Compare(a.pos.Line, b.pos.Line))
	})
	v.first = map[string]Position{}
	for _, h := range all {
		switch {
		case !isIDShape(h.id):
			continue
		case !IsID(h.id):
			v.problem(h.pos, h.id, ruleBadID, Cut(h.id))
		}
		if v.retired[h.id] {
			v.problem(h.pos, h.id, ruleRetiredID)
		}
		if at, doubled := v.first[h.id]; doubled {
			v.problem(h.pos, h.id, ruleDoubledID, at.File, at.Line)
			continue
		}
		v.first[h.id], v.kinds[h.id] = h.pos, h.kind
	}
}

// target judges one relation of node to the node id names, which must be of one of the kinds
// wanted: missing-target when the files hold no such node. A reference that is not an id is the
// schema's problem, and passes here.
func (v *validator) target(pos Position, node, attribute, id string, wanted ...string) bool {
	if !isIDShape(id) {
		return false
	}
	kind, held := v.kinds[id]
	switch {
	case !held:
		v.problem(pos, node, ruleMissingTarget, attribute, Cut(id))
	case !slices.Contains(wanted, kind):
		v.problem(pos, node, ruleTargetOfAnotherKind, attribute, Cut(id), kind, strings.Join(wanted, " or an "))
	default:
		return true
	}
	return false
}

func (v *validator) statement(s Statement) {
	if missing(s.Text) {
		v.problem(s.Pos, s.ID, ruleNoText)
	}
	if isIDShape(s.Parent) {
		if slices.Contains(statementParent.kinds, v.kinds[s.Parent]) {
			v.edges[s.ID] = append(v.edges[s.ID], edge{to: s.Parent, relation: statementParent.member})
		} else {
			v.problem(s.Pos, s.ID, ruleNoParent, Cut(s.Parent))
		}
	}
	for _, id := range s.Serves {
		if v.target(s.Pos, s.ID, statementServes.member, id, statementServes.kinds...) {
			v.edges[s.ID] = append(v.edges[s.ID], edge{to: id, relation: statementServes.member})
		}
	}
	for _, id := range s.About {
		v.target(s.Pos, s.ID, statementAbout.member, id, statementAbout.kinds...)
	}
	if isIDShape(s.ID) && !v.checked[s.ID] {
		switch {
		case !v.parents[s.ID]:
			v.problem(s.Pos, s.ID, ruleLeafWithoutCheck)
		case v.servedBy[s.ID]:
			v.problem(s.Pos, s.ID, ruleSupportedWithoutCheck)
		}
	}
	v.code(s.Pos, s.ID, "text", s.Text)
}

func (v *validator) check(c Check) {
	if isIDShape(c.Serves) && !slices.Contains(checkServes.kinds, v.kinds[c.Serves]) {
		v.problem(c.Pos, c.ID, ruleCheckServes)
	}
	for _, part := range []struct {
		value string
		kind  ruleKind
	}{
		{c.Claim, ruleNoClaim},
		{c.ProcedureText, ruleNoProcedure},
		{c.FailingExample, ruleNoFailingExample},
	} {
		if missing(part.value) {
			v.problem(c.Pos, c.ID, part.kind)
		}
	}
	if sentenceEnd.MatchString(c.Claim) {
		v.problem(c.Pos, c.ID, ruleBadClaim)
	}
	if c.Procedure == ProcedureProgram && c.Test != nil {
		switch {
		case missing(c.Test.Runner) || missing(c.Test.Selector):
			v.problem(c.Pos, c.ID, ruleNoTest)
		case c.Test.Runner == "go" && c.Test.Selector != "":
			if _, _, err := ParseSelector(c.Test.Selector); err != nil {
				v.problem(c.Pos, c.ID, ruleBadSelector)
			}
		}
		if missing(c.Test.Job) {
			v.problem(c.Pos, c.ID, ruleNoJob)
		}
	}
	v.code(c.Pos, c.ID, "claim", c.Claim)
	v.code(c.Pos, c.ID, "procedure", c.ProcedureText)
	v.code(c.Pos, c.ID, "failing example", c.FailingExample)
}

// sentenceSpace is each character that Python's \s matches, the white space at which ste-lint, the
// ASD-STE100 program of R49, splits a line into its sentences: the ASCII white space, the
// separators of Unicode (U+00A0 and U+202F among them), U+001C to U+001F and U+0085. Go's \s is
// ASCII alone and leaves out \v.
const sentenceSpace = `\s\v\x{1c}-\x{1f}\x{85}\p{Z}`

// sentenceEnd is the end of a sentence that more text follows: ".", "!" or "?", then white space,
// where ste-lint splits a line into its sentences. A dot inside a word, such as the one of 1.2 or
// Node.js, ends no sentence.
var sentenceEnd = regexp.MustCompile(`[.!?][` + sentenceSpace + `]+[^` + sentenceSpace + `]`)

// hasWord reports whether s holds a letter or a digit: a name or a purpose of only spaces, or of
// only a format character such as U+200B, is a missing one (R15).
func hasWord(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

// missing reports whether s is a text that is present and holds no letter or digit. An absent text
// ("") is the schema's problem.
func missing(s string) bool {
	return s != "" && !hasWord(s)
}

// isIDShape reports whether id has the schema's shape of an id (slug, the pattern of the schema's
// id). Validate reads a relation, and judges an id, only when it has; the schema refuses each
// other one.
func isIDShape(id string) bool {
	return slug.MatchString(id)
}

func (v *validator) component(c Component) {
	if missing(c.Name) || missing(c.Purpose) || sentenceEnd.MatchString(c.Purpose) {
		v.problem(c.Pos, c.ID, ruleBadComponent)
	}
	if isIDShape(c.Parent) {
		if slices.Contains(componentParent.kinds, v.kinds[c.Parent]) {
			v.edges[c.ID] = append(v.edges[c.ID], edge{to: c.Parent, relation: componentParent.member})
		} else {
			v.problem(c.Pos, c.ID, ruleComponentParent, Cut(c.Parent))
		}
	}
	if c.Interface {
		for _, id := range c.Joins {
			if isIDShape(id) && !slices.Contains(componentJoins.kinds, v.kinds[id]) {
				v.problem(c.Pos, c.ID, ruleBadInterface, Cut(id))
			}
		}
	}
	v.code(c.Pos, c.ID, "name", c.Name)
	v.code(c.Pos, c.ID, "purpose", c.Purpose)
}

// code records code-in-text when text, the field of node, names code, each name cut (Cut).
func (v *validator) code(pos Position, node, field, text string) {
	if names := CodeNames(text); len(names) > 0 {
		for i, name := range names {
			names[i] = Cut(name)
		}
		v.problem(pos, node, ruleCodeInText, field, strings.Join(names, ", "))
	}
}
