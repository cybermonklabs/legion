package specgraph

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The valid tree holds a top statement with two children, a leaf with three checks (a normal
// case, a boundary and a failure), a structure statement about an interface, a program check
// that names its runner, selector and job, a query check of the graph and an agent check with
// no test part, and a term. Validate finds no problem in it.
func TestValidTreesHaveNoProblem(t *testing.T) {
	t.Parallel()
	tree := parseDir(t, "testdata/trees/valid")
	if got := len(tree.Statements) + len(tree.Checks) + len(tree.Components) + len(tree.Terms); got != 11 {
		t.Fatalf("the valid tree holds %d nodes, want 11: %+v", got, tree)
	}
	if problems := Validate(tree, Options{}); len(problems) != 0 {
		t.Errorf("Validate(valid): %+v, want none", problems)
	}
}

// A program check whose test part names a runner, a selector and a job is valid (v-tree: a
// checker that refuses that tree fails).
func TestAProgramCheckWithItsTestPartIsValid(t *testing.T) {
	t.Parallel()
	tree := parseDir(t, "testdata/trees/program-check-test-part")
	if c, ok := tree.CheckByID("a-restart-keeps-the-claim"); !ok || c.Test == nil || c.Test.Job != "go-checks" {
		t.Fatalf("the tree holds no program check with its test part: %+v", tree)
	}
	if problems := Validate(tree, Options{}); len(problems) != 0 {
		t.Errorf("Validate: %+v, want none", problems)
	}
}

// plantedFaults is each fixture tree of the controls of v-tree, v-components and v-check-shape,
// and of R1, R3 and R6, with the one problem Validate must find in it. spec-check's built-binary
// test reads the same table.
var plantedFaults = []struct {
	dir     string
	retired []string
	want    Problem
}{
	{dir: "doubled-id", want: Problem{Pos: Position{"b.md", 1}, Node: "a-claim-survives-a-restart", Rule: "doubled-id", Detail: "doubled-id: also at a.md:1"}},
	{dir: "missing-parent", want: Problem{Pos: Position{"spec.md", 1}, Node: "a-claim-survives-a-restart", Rule: "no-parent", Detail: `no-parent: the files hold no statement "legion-keeps-each-claim"`}},
	{dir: "parent-loop", want: Problem{Pos: Position{"spec.md", 1}, Node: "a-claim-survives-a-restart", Rule: "loop", Detail: "loop: a-claim-survives-a-restart -[parent]-> legion-keeps-each-claim -[parent]-> a-claim-survives-a-restart"}},
	{dir: "parent-and-serves-loop", want: Problem{Pos: Position{"spec.md", 1}, Node: "a-claim-survives-a-restart", Rule: "loop", Detail: "loop: a-claim-survives-a-restart -[serves]-> legion-keeps-each-claim -[parent]-> a-claim-survives-a-restart"}},
	{dir: "retired-id", retired: []string{"gone-statement"}, want: Problem{Pos: Position{"spec.md", 1}, Node: "gone-statement", Rule: "retired-id", Detail: "retired-id: the approved spec removed this id"}},
	{dir: "leaf-without-check", want: Problem{Pos: Position{"spec.md", 1}, Node: "a-claim-survives-a-restart", Rule: "leaf-without-check", Detail: "leaf-without-check: no statement is under it and no check serves it"}},
	{dir: "claim-names-a-path", want: Problem{Pos: Position{"spec.md", 5}, Node: "a-restart-keeps-the-claim", Rule: "code-in-text", Detail: "code-in-text: claim names code: cmd/listener/main.go"}},
	{dir: "statement-names-a-package", want: Problem{Pos: Position{"spec.md", 1}, Node: "a-document-keeps-its-blocks", Rule: "code-in-text", Detail: "code-in-text: text names code: internal/dispatch/pmdoc"}},
	{dir: "component-no-purpose", want: Problem{Pos: Position{"spec.md", 1}, Node: "dispatch", Rule: "bad-component", Detail: "bad-component: a component has a name and one sentence of purpose"}},
	{dir: "interface-joins-one", want: Problem{Pos: Position{"spec.md", 9}, Node: "legion-to-dispatch", Rule: "bad-interface", Detail: "bad-interface: an interface joins two or more components"}},
	{dir: "interface-joins-a-missing-component", want: Problem{Pos: Position{"spec.md", 5}, Node: "legion-to-dispatch", Rule: "bad-interface", Detail: `bad-interface: joins "legion", which is no component`}},
	{dir: "component-names-a-package", want: Problem{Pos: Position{"spec.md", 1}, Node: "dispatch", Rule: "code-in-text", Detail: "code-in-text: purpose names code: internal/dispatch/pmdoc"}},
	{dir: "about-a-missing-component", want: Problem{Pos: Position{"spec.md", 1}, Node: "a-claim-survives-a-restart", Rule: "missing-target", Detail: `missing-target: about names "legion", which the files do not hold`}},
	{dir: "structure-statement-without-about", want: Problem{Pos: Position{"spec.md", 1}, Node: "legion-reads-issues-only-through-dispatch", Rule: "no-about", Detail: "no-about: a statement of kind structure names its components or interfaces"}},
	{dir: "check-without-failing-example", want: Problem{Pos: Position{"spec.md", 5}, Node: "a-restart-keeps-the-claim", Rule: "no-failing-example", Detail: "no-failing-example: the check has no failing example"}},
	{dir: "check-subject-not-allowed", want: Problem{Pos: Position{"spec.md", 5}, Node: "a-restart-keeps-the-claim", Rule: "bad-subject", Detail: `bad-subject: "the logs" is not code, production or graph`}},
	{dir: "program-procedure-names-a-test", want: Problem{Pos: Position{"spec.md", 5}, Node: "a-restart-keeps-the-claim", Rule: "code-in-text", Detail: "code-in-text: procedure names code: TestARestart_LosesNoClaim"}},
	{dir: "program-check-without-test-part", want: Problem{Pos: Position{"spec.md", 5}, Node: "a-restart-keeps-the-claim", Rule: "no-test", Detail: "no-test: a program check names its runner and its selector"}},
	{dir: "program-check-without-job", want: Problem{Pos: Position{"spec.md", 5}, Node: "a-restart-keeps-the-claim", Rule: "no-job", Detail: "no-job: a program check names the job that runs its test"}},
	{dir: "query-check-with-test-part", want: Problem{Pos: Position{"spec.md", 5}, Node: "each-claim-is-in-the-graph", Rule: "test-on-non-program", Detail: "test-on-non-program: a query check names no runner, selector or job"}},
	{dir: "go-selector-two-words-missing", want: Problem{Pos: Position{"spec.md", 5}, Node: "a-restart-keeps-the-claim", Rule: "bad-selector", Detail: `bad-selector: a go selector is "<import path> <TopLevelTestName>"`}},
	{dir: "check-serves-two-statements", want: Problem{Pos: Position{"spec.md", 21}, Node: "each-restart-keeps-the-claim", Rule: "check-serves", Detail: "check-serves: a check serves exactly one statement of the files"}},
	{dir: "statement-without-kind", want: Problem{Pos: Position{"spec.md", 1}, Node: "a-claim-survives-a-restart", Rule: "bad-kind", Detail: `bad-kind: "" is not behavior, structure or property`}},
	{dir: "bad-id", want: Problem{Pos: Position{"spec.md", 1}, Node: "step-3", Rule: "bad-id", Detail: `bad-id: "step-3" is not an id: words of lower-case ASCII letters and digits joined by single hyphens, with no word that states a position`}},
}

// Each planted fault fails the checker, which names its node, its rule and the line of its block,
// and finds no other problem in the tree.
func TestEachPlantedFaultIsNamed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir("testdata/trees")
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{"valid": true, "program-check-test-part": true}
	for _, row := range plantedFaults {
		covered[row.dir] = true
		t.Run(row.dir, func(t *testing.T) {
			t.Parallel()
			tree := parseDir(t, filepath.Join("testdata/trees", row.dir))
			got := Validate(tree, Options{Retired: row.retired})
			if !reflect.DeepEqual(got, []Problem{row.want}) {
				t.Errorf("Validate(%s):\n got %+v\nwant %+v", row.dir, got, []Problem{row.want})
			}
		})
	}
	for _, entry := range entries {
		if entry.IsDir() && !covered[entry.Name()] {
			t.Errorf("testdata/trees/%s is in no row", entry.Name())
		}
	}
}

// Without its retired list, the retired-id tree is valid: the rule reads only the ids the
// approved spec removed, and Options.Retired nil checks none.
func TestNoRetiredListChecksNoID(t *testing.T) {
	t.Parallel()
	if problems := Validate(parseDir(t, "testdata/trees/retired-id"), Options{}); len(problems) != 0 {
		t.Errorf("Validate: %+v, want none", problems)
	}
}

// The rules that no fixture tree of the approved checks plants, over trees built in memory: each
// row adds one fault to a valid leaf and its check, and wants exactly the one problem.
func TestTheOtherRules(t *testing.T) {
	t.Parallel()
	pos := func(line int) Position { return Position{File: "x.md", Line: line} }
	leaf := func(edit func(*Statement)) Statement {
		s := Statement{ID: "a-claim-survives-a-restart", Text: "Legion keeps each claim.", Kind: KindBehavior, Pos: pos(1)}
		if edit != nil {
			edit(&s)
		}
		return s
	}
	check := func(edit func(*Check)) Check {
		c := Check{
			ID: "a-restart-keeps-the-claim", Serves: "a-claim-survives-a-restart", Claim: "The daemon reads the claim.",
			ProcedureText: "An agent reads the claims.", FailingExample: "A daemon with no store.",
			Subject: SubjectCode, Procedure: ProcedureAgent, Pos: pos(5),
		}
		if edit != nil {
			edit(&c)
		}
		return c
	}
	// program makes the check a program check of a go test, with procedure as its procedure.
	program := func(procedure string) func(*Check) {
		return func(c *Check) {
			c.Procedure, c.ProcedureText = ProcedureProgram, procedure
			c.Test = &Test{Runner: "go", Selector: "example.com/acme/daemon TestARestartKeepsTheClaim", Job: "go-checks"}
		}
	}
	component := func(id string, edit func(*Component)) Component {
		c := Component{ID: id, Name: "Part " + id, Purpose: "It does one thing.", Pos: pos(20)}
		if edit != nil {
			edit(&c)
		}
		return c
	}
	// says makes the leaf's text text.
	says := func(text string) func(*Statement) { return func(s *Statement) { s.Text = text } }
	for _, row := range []struct {
		name string
		tree Tree
		want Problem
	}{
		{
			name: "a statement with no text",
			tree: Tree{Statements: []Statement{leaf(func(s *Statement) { s.Text = "" })}, Checks: []Check{check(nil)}},
			want: Problem{Pos: pos(1), Node: "a-claim-survives-a-restart", Rule: "no-text", Detail: "no-text: a statement has a text"},
		},
		{
			name: "an id with a letter outside ASCII",
			tree: Tree{Statements: []Statement{leaf(func(s *Statement) { s.ID = "boîte" })}, Checks: []Check{check(func(c *Check) { c.Serves = "boîte" })}},
			want: Problem{Pos: pos(1), Node: "boîte", Rule: "bad-id", Detail: `bad-id: "boîte" is not an id: words of lower-case ASCII letters and digits joined by single hyphens, with no word that states a position`},
		},
		{
			name: "a statement that names a symbol with an underscore",
			tree: Tree{Statements: []Statement{leaf(says("read_set gives each claim one owner."))}, Checks: []Check{check(nil)}},
			want: Problem{Pos: pos(1), Node: "a-claim-survives-a-restart", Rule: "code-in-text", Detail: "code-in-text: text names code: read_set"},
		},
		{
			name: "a statement that names a symbol in lower camel case",
			tree: Tree{Statements: []Statement{leaf(says("readSet gives each claim one owner."))}, Checks: []Check{check(nil)}},
			want: Problem{Pos: pos(1), Node: "a-claim-survives-a-restart", Rule: "code-in-text", Detail: "code-in-text: text names code: readSet"},
		},
		{
			name: "a statement that names a package and its symbol",
			tree: Tree{Statements: []Statement{leaf(says("pmdoc.ReadSet gives each claim one owner."))}, Checks: []Check{check(nil)}},
			want: Problem{Pos: pos(1), Node: "a-claim-survives-a-restart", Rule: "code-in-text", Detail: "code-in-text: text names code: pmdoc.ReadSet"},
		},
		{
			name: "a statement that names a call",
			tree: Tree{Statements: []Statement{leaf(says("ReadSet() gives each claim one owner."))}, Checks: []Check{check(nil)}},
			want: Problem{Pos: pos(1), Node: "a-claim-survives-a-restart", Rule: "code-in-text", Detail: "code-in-text: text names code: ReadSet()"},
		},
		{
			name: "a block with no id",
			tree: Tree{Statements: []Statement{leaf(nil)}, Checks: []Check{check(nil)}, Terms: []Term{{Name: "work unit", Definition: "A work unit is work.", Pos: pos(9)}}},
			want: Problem{Pos: pos(9), Node: "-", Rule: "bad-id", Detail: "bad-id: the block has no id"},
		},
		{
			name: "a statement that serves one that the files do not hold",
			tree: Tree{Statements: []Statement{leaf(func(s *Statement) { s.Serves = []string{"legion-keeps-each-claim"} })}, Checks: []Check{check(nil)}},
			want: Problem{Pos: pos(1), Node: "a-claim-survives-a-restart", Rule: "missing-target", Detail: `missing-target: serves names "legion-keeps-each-claim", which the files do not hold`},
		},
		{
			name: "a statement that another statement serves and that has no check of its own",
			tree: servedTree(false),
			want: Problem{Pos: Position{"x.md", 10}, Node: "legion-keeps-each-claim", Rule: "supported-without-check", Detail: "supported-without-check: other statements serve it and no check serves it"},
		},
		{
			name: "a leaf that another statement serves and that has no check, named once",
			tree: Tree{
				Statements: []Statement{leaf(nil), {
					ID: "a-lost-claim-is-reported", Text: "Legion reports each lost claim.", Kind: KindBehavior,
					Serves: []string{"a-claim-survives-a-restart"}, Pos: pos(20),
				}},
				Checks: []Check{check(func(c *Check) {
					c.ID, c.Serves, c.Pos = "a-lost-claim-is-in-the-log", "a-lost-claim-is-reported", pos(25)
				})},
			},
			want: Problem{Pos: pos(1), Node: "a-claim-survives-a-restart", Rule: "leaf-without-check", Detail: "leaf-without-check: no statement is under it and no check serves it"},
		},
		{
			name: "a statement about a node that is no component",
			tree: Tree{Statements: []Statement{leaf(func(s *Statement) { s.About = []string{"a-restart-keeps-the-claim"} })}, Checks: []Check{check(nil)}},
			want: Problem{Pos: pos(1), Node: "a-claim-survives-a-restart", Rule: "missing-target", Detail: `missing-target: about names "a-restart-keeps-the-claim", which is a check, not a component or an interface`},
		},
		{
			name: "a statement whose parent is a check",
			tree: Tree{Statements: []Statement{leaf(func(s *Statement) { s.Parent = "a-restart-keeps-the-claim" })}, Checks: []Check{check(nil)}},
			want: Problem{Pos: pos(1), Node: "a-claim-survives-a-restart", Rule: "no-parent", Detail: `no-parent: the files hold no statement "a-restart-keeps-the-claim"`},
		},
		{
			name: "a statement that is its own parent",
			tree: Tree{Statements: []Statement{leaf(func(s *Statement) { s.Parent = s.ID })}, Checks: []Check{check(nil)}},
			want: Problem{Pos: pos(1), Node: "a-claim-survives-a-restart", Rule: "loop", Detail: "loop: a-claim-survives-a-restart -[parent]-> a-claim-survives-a-restart"},
		},
		{
			name: "a check and a statement that share an id",
			tree: Tree{Statements: []Statement{leaf(nil)}, Checks: []Check{check(nil), check(func(c *Check) { c.ID = "a-claim-survives-a-restart"; c.Pos = pos(9) })}},
			want: Problem{Pos: pos(9), Node: "a-claim-survives-a-restart", Rule: "doubled-id", Detail: "doubled-id: also at x.md:1"},
		},
		{
			name: "a program procedure that names the go test of its own selector",
			tree: Tree{Statements: []Statement{leaf(nil)}, Checks: []Check{check(program("The program runs TestARestartKeepsTheClaim, which claims one issue and starts the daemon again."))}},
			want: Problem{Pos: pos(5), Node: "a-restart-keeps-the-claim", Rule: "code-in-text", Detail: "code-in-text: procedure names code: TestARestartKeepsTheClaim"},
		},
		{
			name: "a program procedure that names another go test",
			tree: Tree{Statements: []Statement{leaf(nil)}, Checks: []Check{check(program("The program TestAClaimAtTheTimeLimitStays claims one issue and starts the daemon again."))}},
			want: Problem{Pos: pos(5), Node: "a-restart-keeps-the-claim", Rule: "code-in-text", Detail: "code-in-text: procedure names code: TestAClaimAtTheTimeLimitStays"},
		},
		{
			name: "a check with no claim",
			tree: Tree{Statements: []Statement{leaf(nil)}, Checks: []Check{check(func(c *Check) { c.Claim = "" })}},
			want: Problem{Pos: pos(5), Node: "a-restart-keeps-the-claim", Rule: "no-claim", Detail: "no-claim: the check has no claim"},
		},
		{
			name: "a check with no procedure",
			tree: Tree{Statements: []Statement{leaf(nil)}, Checks: []Check{check(func(c *Check) { c.ProcedureText = "" })}},
			want: Problem{Pos: pos(5), Node: "a-restart-keeps-the-claim", Rule: "no-procedure", Detail: "no-procedure: the check has no procedure"},
		},
		{
			name: "a procedure that is not program, query or agent",
			tree: Tree{Statements: []Statement{leaf(nil)}, Checks: []Check{check(func(c *Check) { c.Procedure = "programm" })}},
			want: Problem{Pos: pos(5), Node: "a-restart-keeps-the-claim", Rule: "bad-procedure", Detail: `bad-procedure: "programm" is not program, query or agent`},
		},
		{
			name: "an agent check with a test part",
			tree: Tree{Statements: []Statement{leaf(nil)}, Checks: []Check{check(func(c *Check) { c.Test = &Test{Job: "go-checks"} })}},
			want: Problem{Pos: pos(5), Node: "a-restart-keeps-the-claim", Rule: "test-on-non-program", Detail: "test-on-non-program: an agent check names no runner, selector or job"},
		},
		{
			name: "a term with no definition",
			tree: Tree{Statements: []Statement{leaf(nil)}, Checks: []Check{check(nil)}, Terms: []Term{{ID: "work-unit", Name: "work unit", Pos: pos(9)}}},
			want: Problem{Pos: pos(9), Node: "work-unit", Rule: "bad-term", Detail: "bad-term: a term has a name and a definition"},
		},
		{
			name: "a term whose name names code",
			tree: Tree{Statements: []Statement{leaf(nil)}, Checks: []Check{check(nil)}, Terms: []Term{{ID: "work-unit", Name: "read_set", Definition: "A work unit is work.", Pos: pos(9)}}},
			want: Problem{Pos: pos(9), Node: "work-unit", Rule: "code-in-text", Detail: "code-in-text: name names code: read_set"},
		},
		{
			name: "a program procedure that names its go test in curly quotes",
			tree: Tree{Statements: []Statement{leaf(nil)}, Checks: []Check{check(program("The program runs \u201cTestARestartKeepsTheClaim\u201d. It must find the claim."))}},
			want: Problem{Pos: pos(5), Node: "a-restart-keeps-the-claim", Rule: "code-in-text", Detail: "code-in-text: procedure names code: TestARestartKeepsTheClaim"},
		},
		{
			name: "a component whose name is only spaces",
			tree: Tree{Components: []Component{component("dispatch", func(c *Component) { c.Name = "   " })}},
			want: Problem{Pos: pos(20), Node: "dispatch", Rule: "bad-component", Detail: "bad-component: a component has a name and one sentence of purpose"},
		},
		{
			name: "a component whose purpose is only a zero-width space",
			tree: Tree{Components: []Component{component("dispatch", func(c *Component) { c.Purpose = "\u200b" })}},
			want: Problem{Pos: pos(20), Node: "dispatch", Rule: "bad-component", Detail: "bad-component: a component has a name and one sentence of purpose"},
		},
		{
			name: "a component whose purpose is two sentences",
			tree: Tree{Components: []Component{component("dispatch", func(c *Component) {
				c.Purpose = "It keeps each message until its session reads it. It also deletes each message after a week."
			})}},
			want: Problem{Pos: pos(20), Node: "dispatch", Rule: "bad-component", Detail: "bad-component: a component has a name and one sentence of purpose"},
		},
		{
			name: "a component whose purpose is two sentences on two lines",
			tree: Tree{Components: []Component{component("dispatch", func(c *Component) { c.Purpose = "It keeps each issue.\nDoes it keep each result?" })}},
			want: Problem{Pos: pos(20), Node: "dispatch", Rule: "bad-component", Detail: "bad-component: a component has a name and one sentence of purpose"},
		},
		{
			name: "a component whose purpose is two sentences joined by a no-break space",
			tree: Tree{Components: []Component{component("dispatch", func(c *Component) { c.Purpose = "It keeps each issue.\u00a0It keeps each result." })}},
			want: Problem{Pos: pos(20), Node: "dispatch", Rule: "bad-component", Detail: "bad-component: a component has a name and one sentence of purpose"},
		},
		{
			name: "a component whose purpose is two sentences joined by a narrow no-break space",
			tree: Tree{Components: []Component{component("dispatch", func(c *Component) { c.Purpose = "It keeps each issue.\u202fIt keeps each result." })}},
			want: Problem{Pos: pos(20), Node: "dispatch", Rule: "bad-component", Detail: "bad-component: a component has a name and one sentence of purpose"},
		},
		{
			name: "a component whose parent is an interface",
			tree: Tree{Components: []Component{
				component("dispatch", nil), component("legion", nil),
				component("legion-to-dispatch", func(c *Component) { c.Interface, c.Joins, c.Pos = true, []string{"dispatch", "legion"}, pos(30) }),
				component("the-queue", func(c *Component) { c.Parent, c.Pos = "legion-to-dispatch", pos(40) }),
			}},
			want: Problem{Pos: pos(40), Node: "the-queue", Rule: "bad-component", Detail: `bad-component: parent "legion-to-dispatch" is no component`},
		},
		{
			name: "a component that joins components",
			tree: Tree{Components: []Component{component("dispatch", nil), component("legion", func(c *Component) { c.Joins = []string{"dispatch"} })}},
			want: Problem{Pos: pos(20), Node: "legion", Rule: "bad-component", Detail: `bad-component: only a component of kind="interface" joins components`},
		},
		{
			name: "two components that are each other's parent",
			tree: Tree{Components: []Component{
				component("dispatch", func(c *Component) { c.Parent = "legion" }),
				component("legion", func(c *Component) { c.Parent, c.Pos = "dispatch", pos(30) }),
			}},
			want: Problem{Pos: pos(20), Node: "dispatch", Rule: "loop", Detail: "loop: dispatch -[parent]-> legion -[parent]-> dispatch"},
		},
		{
			name: "an interface that joins an interface",
			tree: Tree{Components: []Component{
				component("dispatch", nil), component("legion", nil),
				component("legion-to-dispatch", func(c *Component) { c.Interface, c.Joins, c.Pos = true, []string{"dispatch", "legion"}, pos(30) }),
				component("one-more-interface", func(c *Component) {
					c.Interface, c.Joins, c.Pos = true, []string{"dispatch", "legion-to-dispatch"}, pos(40)
				}),
			}},
			want: Problem{Pos: pos(40), Node: "one-more-interface", Rule: "bad-interface", Detail: `bad-interface: joins "legion-to-dispatch", which is no component`},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got := Validate(row.tree, Options{})
			if !reflect.DeepEqual(got, []Problem{row.want}) {
				t.Errorf("Validate:\n got %+v\nwant %+v", got, []Problem{row.want})
			}
		})
	}
}

// A purpose of one sentence is valid, also on two lines and with a dot inside a word: the controls
// of the two-sentence rows of TestTheOtherRules.
func TestAPurposeOfOneSentenceIsValid(t *testing.T) {
	t.Parallel()
	for _, purpose := range []string{
		"It keeps each message until its session reads it.",
		"It keeps each message\nuntil its session reads it.",
		"It keeps version 1.2 of each message for Node.js clients",
	} {
		tree := Tree{Components: []Component{{ID: "dispatch", Name: "Dispatch", Purpose: purpose, Pos: Position{File: "x.md", Line: 1}}}}
		if problems := Validate(tree, Options{}); len(problems) != 0 {
			t.Errorf("purpose %q: %+v, want none", purpose, problems)
		}
	}
}

// The white space after a stop is each of the 29 code points that Python's \s matches, where
// ste-lint splits a line into its sentences: each one ends the first of two sentences of a purpose.
// A format character that Python's \s does not match, such as U+200B, ends none.
func TestEachWhiteSpaceThatSteLintSplitsAtEndsASentence(t *testing.T) {
	t.Parallel()
	purpose := func(between rune) []Component {
		return []Component{{ID: "dispatch", Name: "Dispatch", Purpose: "It keeps each issue." + string(between) + "It keeps each result.", Pos: Position{File: "x.md", Line: 1}}}
	}
	for _, space := range []rune{
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x1c, 0x1d, 0x1e, 0x1f, 0x20, 0x85, 0xa0, 0x1680,
		0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a,
		0x2028, 0x2029, 0x202f, 0x205f, 0x3000,
	} {
		if got := Validate(Tree{Components: purpose(space)}, Options{}); len(got) != 1 || got[0].Rule != "bad-component" {
			t.Errorf("U+%04X between two sentences: %+v, want one bad-component", space, got)
		}
	}
	for _, other := range []rune{0x200b, 0x180e, 0xfeff, 0x2060} {
		if got := Validate(Tree{Components: purpose(other)}, Options{}); len(got) != 0 {
			t.Errorf("U+%04X after the stop: %+v, want none", other, got)
		}
	}
}

// R5: "A statement that other statements support still has checks of its own." With a check of
// its own, the served statement of servedTree is valid: the control of the supported-without-check
// row of TestTheOtherRules.
func TestAServedStatementWithACheckOfItsOwnIsValid(t *testing.T) {
	t.Parallel()
	if problems := Validate(servedTree(true), Options{}); len(problems) != 0 {
		t.Errorf("Validate: %+v, want none", problems)
	}
}

// servedTree is a statement with one child and a statement that serves it, each leaf with a check,
// and a check of the served statement's own when checked is true.
func servedTree(checked bool) Tree {
	pos := func(line int) Position { return Position{File: "x.md", Line: line} }
	check := func(id, serves string, line int) Check {
		return Check{
			ID: id, Serves: serves, Claim: "The daemon reads the claim.", ProcedureText: "An agent reads the claims.",
			FailingExample: "A daemon with no store.", Subject: SubjectCode, Procedure: ProcedureAgent, Pos: pos(line),
		}
	}
	tree := Tree{
		Statements: []Statement{
			{ID: "a-claim-survives-a-restart", Text: "Legion keeps each claim after a restart.", Kind: KindBehavior, Parent: "legion-keeps-each-claim", Pos: pos(1)},
			{ID: "a-lost-claim-is-reported", Text: "Legion reports each lost claim.", Kind: KindBehavior, Serves: []string{"legion-keeps-each-claim"}, Pos: pos(20)},
			{ID: "legion-keeps-each-claim", Text: "Legion keeps each claim in Dispatch.", Kind: KindBehavior, Pos: pos(10)},
		},
		Checks: []Check{check("a-lost-claim-is-in-the-log", "a-lost-claim-is-reported", 25), check("a-restart-keeps-the-claim", "a-claim-survives-a-restart", 5)},
	}
	if checked {
		tree.Checks = append(tree.Checks, check("dispatch-holds-each-claim", "legion-keeps-each-claim", 15))
	}
	return tree
}

// A program check of another runner than go keeps its selector as its runner writes it: the go
// selector rule judges a go check alone.
func TestAnotherRunnersSelectorIsItsOwn(t *testing.T) {
	t.Parallel()
	tree := Tree{
		Statements: []Statement{{ID: "a-claim-survives-a-restart", Text: "Legion keeps each claim.", Kind: KindBehavior}},
		Checks: []Check{{
			ID: "a-restart-keeps-the-claim", Serves: "a-claim-survives-a-restart", Claim: "The daemon reads the claim.",
			ProcedureText: "The program reads the claims.", FailingExample: "A daemon with no store.", Subject: SubjectCode,
			Procedure: ProcedureProgram, Test: &Test{Runner: "bun", Selector: "src/claims.test.ts", Job: "bun-checks"},
		}},
	}
	if problems := Validate(tree, Options{}); len(problems) != 0 {
		t.Errorf("Validate: %+v, want none", problems)
	}
}

// Validate sorts its problems by file, line, node and rule, whatever order the rules find them in.
func TestValidateSortsItsProblems(t *testing.T) {
	t.Parallel()
	tree, parseProblems := Parse(map[string][]byte{
		"b.md": []byte(":::requirement{#leaf-two}\nA leaf.\n:::\n"),
		"a.md": []byte(":::requirement{#leaf-one kind=\"behavior\"}\nA leaf in cmd/listener.\n:::\n"),
	})
	if len(parseProblems) != 0 {
		t.Fatal(parseProblems)
	}
	got := Validate(tree, Options{})
	var keys []string
	for _, p := range got {
		keys = append(keys, p.Pos.File+" "+p.Node+" "+p.Rule)
	}
	want := []string{"a.md leaf-one code-in-text", "a.md leaf-one leaf-without-check", "b.md leaf-two bad-kind", "b.md leaf-two leaf-without-check"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("Validate order %q, want %q", keys, want)
	}
}

// parseDir parses every spec file directly in dir and fails the test on a parse problem.
func parseDir(t *testing.T, dir string) Tree {
	t.Helper()
	tree, problems := Parse(readFiles(t, dir))
	if len(problems) != 0 {
		t.Fatalf("Parse(%s): %+v", dir, problems)
	}
	return tree
}

// A required text with no letter and no digit is a missing one, in each place where a rule of
// Validate asks for a text: a statement's text, a check's claim, procedure and failing example, a
// component's name and purpose, and a term's name and definition. Each row gives the one problem
// that an empty text gives, for each of three empty shapes.
func TestATextWithNoLetterOrDigitIsMissing(t *testing.T) {
	t.Parallel()
	pos := Position{File: "x.md", Line: 1}
	tree := func() Tree {
		return Tree{
			Statements: []Statement{{ID: "a-claim-survives-a-restart", Text: "Legion keeps each claim.", Kind: KindBehavior, Pos: pos}},
			Checks: []Check{{
				ID: "a-restart-keeps-the-claim", Serves: "a-claim-survives-a-restart", Claim: "The daemon reads the claim.",
				ProcedureText: "An agent reads the claims.", FailingExample: "A daemon with no store.",
				Subject: SubjectCode, Procedure: ProcedureAgent, Pos: pos,
			}},
			Components: []Component{{ID: "dispatch", Name: "Dispatch", Purpose: "It keeps each issue.", Pos: pos}},
			Terms:      []Term{{ID: "work-unit", Name: "work unit", Definition: "A work unit is work.", Pos: pos}},
		}
	}
	for _, place := range []struct {
		name, node, detail string
		set                func(*Tree, string)
	}{
		{"a statement's text", "a-claim-survives-a-restart", "no-text: a statement has a text", func(t *Tree, s string) { t.Statements[0].Text = s }},
		{"a check's claim", "a-restart-keeps-the-claim", "no-claim: the check has no claim", func(t *Tree, s string) { t.Checks[0].Claim = s }},
		{"a check's procedure", "a-restart-keeps-the-claim", "no-procedure: the check has no procedure", func(t *Tree, s string) { t.Checks[0].ProcedureText = s }},
		{"a check's failing example", "a-restart-keeps-the-claim", "no-failing-example: the check has no failing example", func(t *Tree, s string) { t.Checks[0].FailingExample = s }},
		{"a component's name", "dispatch", "bad-component: a component has a name and one sentence of purpose", func(t *Tree, s string) { t.Components[0].Name = s }},
		{"a component's purpose", "dispatch", "bad-component: a component has a name and one sentence of purpose", func(t *Tree, s string) { t.Components[0].Purpose = s }},
		{"a term's name", "work-unit", "bad-term: a term has a name and a definition", func(t *Tree, s string) { t.Terms[0].Name = s }},
		{"a term's definition", "work-unit", "bad-term: a term has a name and a definition", func(t *Tree, s string) { t.Terms[0].Definition = s }},
	} {
		for shape, empty := range map[string]string{"only spaces": "   ", "only U+200B": "\u200b", "only punctuation": "...!?"} {
			tr := tree()
			place.set(&tr, empty)
			got := Validate(tr, Options{})
			rule, _, _ := strings.Cut(place.detail, ":")
			if want := []Problem{{Pos: pos, Node: place.node, Rule: rule, Detail: place.detail}}; !reflect.DeepEqual(got, want) {
				t.Errorf("%s of %s:\n got %+v\nwant %+v", place.name, shape, got, want)
			}
		}
	}
	if got := Validate(tree(), Options{}); len(got) != 0 {
		t.Errorf("control, every text present: %+v, want none", got)
	}
}
