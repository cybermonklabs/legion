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

// Validate finds each fault of the tree that a program can find, sorted by file, line, node and
// rule:
//
//   - an id: doubled-id (one id on two nodes of any kind), bad-id (not words of lower-case ASCII
//     letters and digits joined by single hyphens, or a word that states a position), retired-id
//     (an id the approved spec removed);
//   - a statement: no-text, bad-kind, no-parent, missing-target (a serves or about relation to a
//     node the files do not hold as a statement, or as a component or interface), no-about (a
//     structure statement about no component), leaf-without-check, supported-without-check (a
//     statement with statements under it that other statements serve, and that no check serves:
//     R5), and loop (a cycle through parent and serves relations, or through the parents of
//     components);
//   - a check: check-serves (not exactly one statement), no-claim, no-procedure,
//     no-failing-example, bad-subject, bad-procedure, no-test and no-job (a program check without
//     its test part), test-on-non-program, bad-selector (a go selector that picks no one test);
//   - a component: bad-component (no name, a purpose that is not one sentence, a parent that is no
//     component, or joins on a component that is no interface) and bad-interface (fewer than two
//     components joined, or a joined node that is no component);
//   - a term: bad-term (no name or definition);
//   - any text: code-in-text, a name of code in a statement's text, a check's claim, procedure or
//     failing example, a component's name or purpose, or a term's name or definition (R4 and R15).
//     A check's test part and job name code by design and are not read. Parse reads each heading
//     for code too, and refuses a claim of more than one sentence (bad-field).
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
		if !hasWord(term.Name) || !hasWord(term.Definition) {
			v.problem(term.Pos, term.ID, "bad-term: a term has a name and a definition")
		}
		v.code(term.Pos, term.ID, "name", term.Name)
		v.code(term.Pos, term.ID, "definition", term.Definition)
	}
	for _, cycle := range findLoops(v.edges) {
		v.problem(v.first[cycle[0]], cycle[0], "loop: "+loopDetail(cycle))
	}
	SortProblems(v.problems)
	return v.problems
}

// Judge is the one reading of a set of spec files that each of their readers shares: the tree
// Parse reads from files, and every problem of Parse and of Validate with o, in one list in
// Validate's order. The tree is refused when the list holds any problem, and a refusal names the
// first. spec-check and Dispatch's results route read the files through it, and a CI job that
// reports results must too, so one tree has one answer.
func Judge(files map[string][]byte, o Options) (Tree, []Problem) {
	tree, problems := Parse(files)
	problems = append(problems, Validate(tree, o)...)
	SortProblems(problems)
	return tree, problems
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

// problem records a problem of node at pos; detail begins with the rule.
func (v *validator) problem(pos Position, node, detail string) {
	if node == "" {
		node = "-"
	}
	rule, _, _ := strings.Cut(detail, ":")
	v.problems = append(v.problems, Problem{Pos: pos, Node: node, Rule: rule, Detail: detail})
}

// ids judges the id of every node, and records the kind and the first position of each, in file
// and line order, so a doubled id names the first place that holds it.
func (v *validator) ids() {
	type held struct {
		id, kind string
		pos      Position
	}
	var all []held
	for _, s := range v.tree.Statements {
		all = append(all, held{s.ID, "statement", s.Pos})
	}
	for _, c := range v.tree.Checks {
		all = append(all, held{c.ID, "check", c.Pos})
	}
	for _, c := range v.tree.Components {
		all = append(all, held{c.ID, kindOf(c), c.Pos})
	}
	for _, term := range v.tree.Terms {
		all = append(all, held{term.ID, "term", term.Pos})
	}
	slices.SortStableFunc(all, func(a, b held) int {
		return cmp.Or(cmp.Compare(a.pos.File, b.pos.File), cmp.Compare(a.pos.Line, b.pos.Line))
	})
	v.first = map[string]Position{}
	for _, h := range all {
		switch {
		case h.id == "":
			v.problem(h.pos, "-", "bad-id: the block has no id")
			continue
		case !IsID(h.id):
			v.problem(h.pos, h.id, fmt.Sprintf("bad-id: %q is not an id: words of lower-case ASCII letters and digits joined by single hyphens, with no word that states a position", h.id))
		}
		if v.retired[h.id] {
			v.problem(h.pos, h.id, "retired-id: the approved spec removed this id")
		}
		if at, doubled := v.first[h.id]; doubled {
			v.problem(h.pos, h.id, fmt.Sprintf("doubled-id: also at %s:%d", at.File, at.Line))
			continue
		}
		v.first[h.id], v.kinds[h.id] = h.pos, h.kind
	}
}

// target judges one relation of node to the node id names, which must be of one of the kinds
// wanted: missing-target when the files hold no such node.
func (v *validator) target(pos Position, node, attribute, id string, wanted ...string) bool {
	kind, held := v.kinds[id]
	switch {
	case !held:
		v.problem(pos, node, fmt.Sprintf("missing-target: %s names %q, which the files do not hold", attribute, id))
	case !slices.Contains(wanted, kind):
		v.problem(pos, node, fmt.Sprintf("missing-target: %s names %q, which is a %s, not a %s", attribute, id, kind, strings.Join(wanted, " or an ")))
	default:
		return true
	}
	return false
}

func (v *validator) statement(s Statement) {
	if !hasWord(s.Text) {
		v.problem(s.Pos, s.ID, "no-text: a statement has a text")
	}
	switch s.Kind {
	case KindBehavior, KindStructure, KindProperty:
	default:
		v.problem(s.Pos, s.ID, fmt.Sprintf("bad-kind: %q is not behavior, structure or property", s.Kind))
	}
	if s.Parent != "" {
		if slices.Contains(statementParent.kinds, v.kinds[s.Parent]) {
			v.edges[s.ID] = append(v.edges[s.ID], edge{to: s.Parent, relation: statementParent.member})
		} else {
			v.problem(s.Pos, s.ID, fmt.Sprintf("no-parent: the files hold no statement %q", s.Parent))
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
	if s.Kind == KindStructure && len(s.About) == 0 {
		v.problem(s.Pos, s.ID, "no-about: a statement of kind structure names its components or interfaces")
	}
	if s.ID != "" && !v.checked[s.ID] {
		switch {
		case !v.parents[s.ID]:
			v.problem(s.Pos, s.ID, "leaf-without-check: no statement is under it and no check serves it")
		case v.servedBy[s.ID]:
			v.problem(s.Pos, s.ID, "supported-without-check: other statements serve it and no check serves it")
		}
	}
	v.code(s.Pos, s.ID, "text", s.Text)
}

func (v *validator) check(c Check) {
	if strings.Contains(c.Serves, " ") || !slices.Contains(checkServes.kinds, v.kinds[c.Serves]) {
		v.problem(c.Pos, c.ID, "check-serves: a check serves exactly one statement of the files")
	}
	for _, part := range []struct{ value, detail string }{
		{c.Claim, "no-claim: the check has no claim"},
		{c.ProcedureText, "no-procedure: the check has no procedure"},
		{c.FailingExample, "no-failing-example: the check has no failing example"},
	} {
		if !hasWord(part.value) {
			v.problem(c.Pos, c.ID, part.detail)
		}
	}
	switch c.Subject {
	case SubjectCode, SubjectProduction, SubjectGraph:
	default:
		v.problem(c.Pos, c.ID, fmt.Sprintf("bad-subject: %q is not code, production or graph", c.Subject))
	}
	switch c.Procedure {
	case ProcedureProgram:
		switch {
		case c.Test == nil || c.Test.Runner == "" || c.Test.Selector == "":
			v.problem(c.Pos, c.ID, "no-test: a program check names its runner and its selector")
		case c.Test.Runner == "go":
			if _, _, err := ParseSelector(c.Test.Selector); err != nil {
				v.problem(c.Pos, c.ID, `bad-selector: a go selector is "<import path> <TopLevelTestName>"`)
			}
		}
		if c.Test != nil && c.Test.Job == "" {
			v.problem(c.Pos, c.ID, "no-job: a program check names the job that runs its test")
		}
	case ProcedureQuery, ProcedureAgent:
		if c.Test != nil {
			article := "a"
			if c.Procedure == ProcedureAgent {
				article = "an"
			}
			v.problem(c.Pos, c.ID, fmt.Sprintf("test-on-non-program: %s %s check names no runner, selector or job", article, c.Procedure))
		}
	default:
		v.problem(c.Pos, c.ID, fmt.Sprintf("bad-procedure: %q is not program, query or agent", c.Procedure))
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

func (v *validator) component(c Component) {
	if !hasWord(c.Name) || !hasWord(c.Purpose) || sentenceEnd.MatchString(c.Purpose) {
		v.problem(c.Pos, c.ID, "bad-component: a component has a name and one sentence of purpose")
	}
	if c.Parent != "" {
		if slices.Contains(componentParent.kinds, v.kinds[c.Parent]) {
			v.edges[c.ID] = append(v.edges[c.ID], edge{to: c.Parent, relation: componentParent.member})
		} else {
			v.problem(c.Pos, c.ID, fmt.Sprintf("bad-component: parent %q is no component", c.Parent))
		}
	}
	switch {
	case c.Interface && len(c.Joins) < 2:
		v.problem(c.Pos, c.ID, "bad-interface: an interface joins two or more components")
	case !c.Interface && len(c.Joins) > 0:
		v.problem(c.Pos, c.ID, `bad-component: only a component of kind="interface" joins components`)
	}
	if c.Interface {
		for _, id := range c.Joins {
			if !slices.Contains(componentJoins.kinds, v.kinds[id]) {
				v.problem(c.Pos, c.ID, fmt.Sprintf("bad-interface: joins %q, which is no component", id))
			}
		}
	}
	v.code(c.Pos, c.ID, "name", c.Name)
	v.code(c.Pos, c.ID, "purpose", c.Purpose)
}

// code records code-in-text when text, the field of node, names code.
func (v *validator) code(pos Position, node, field, text string) {
	if names := CodeNames(text); len(names) > 0 {
		v.problem(pos, node, fmt.Sprintf("code-in-text: %s names code: %s", field, strings.Join(names, ", ")))
	}
}
