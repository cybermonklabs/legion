package specgraph

import (
	"slices"
	"time"
)

// Position is where a block or a problem is: File, the name of the file inside the spec directory,
// and Line, the 1-based line of the block's opening ::: line, or of the line a problem names.
type Position struct {
	File string
	Line int
}

// Kind is what a statement says of the software (R3): what it does, how it is built, or a property
// that it has.
type Kind string

// The three kinds of statement.
const (
	KindBehavior  Kind = "behavior"
	KindStructure Kind = "structure"
	KindProperty  Kind = "property"
)

// Statement is a requirement block: one or two sentences that must be true of the software.
type Statement struct {
	ID string
	// Text is the body, its lines joined with "\n", each line without its trailing white space.
	Text string
	Kind Kind
	// Parent is the statement it sits under, "" at the top level.
	Parent string
	// Serves is the statements it supports (R5), sorted and unique.
	Serves []string
	// About is the components and interfaces it is about (R16), sorted and unique.
	About []string
	Pos   Position
}

// Subject is what a check reads (R6): the code at one commit, production, or the graph.
type Subject string

// The three subjects of a check.
const (
	SubjectCode       Subject = "code"
	SubjectProduction Subject = "production"
	SubjectGraph      Subject = "graph"
)

// Procedure is what decides a check (R6): a program, a query, or an agent with fixed instructions.
type Procedure string

// The three procedures of a check.
const (
	ProcedureProgram Procedure = "program"
	ProcedureQuery   Procedure = "query"
	ProcedureAgent   Procedure = "agent"
)

// Test is the test part of a check that a program runs (R6): the runner and the selector that pick
// one test in the code, and the job whose reports alone count for the check.
type Test struct {
	Runner, Selector, Job string
}

// Check is a check block: a claim that the system decides by itself, under one statement.
type Check struct {
	ID string
	// Serves holds the id of the one statement the check sits under. Parse writes the ids the
	// attribute names joined by one space, so Validate can refuse a check that names two.
	Serves                               string
	Claim, ProcedureText, FailingExample string
	Subject                              Subject
	Procedure                            Procedure
	// Test is non-nil exactly when the block names a runner, a selector or a job.
	Test *Test
	// TimeLimit is how long a result of the check counts (R9); 0 is no time limit.
	TimeLimit time.Duration
	Pos       Position
}

// Component is a component block (R15). An interface is a component with Interface set, which
// joins two or more components.
type Component struct {
	ID, Name, Purpose, Parent string
	Interface                 bool
	// Joins is the components an interface joins, sorted and unique.
	Joins []string
	Pos   Position
}

// Term is a term block: a noun of the system and its definition of one sentence (R50).
type Term struct {
	ID, Name, Definition string
	Pos                  Position
}

// Tree is every node the spec files hold, each list sorted by ID, then by Pos.
type Tree struct {
	Statements []Statement
	Checks     []Check
	Components []Component
	Terms      []Term
}

// Problem is one fault of the files: where it is, the id of its node ("-" for a block with no id
// or a line in no block), its rule, and a Detail that begins with the rule.
type Problem struct {
	Pos                Position
	Node, Rule, Detail string
}

// StatementByID is the first statement the tree holds with id, and whether it holds one.
func (t Tree) StatementByID(id string) (Statement, bool) {
	i := slices.IndexFunc(t.Statements, func(s Statement) bool { return s.ID == id })
	if i < 0 {
		return Statement{}, false
	}
	return t.Statements[i], true
}

// CheckByID is the first check the tree holds with id, and whether it holds one.
func (t Tree) CheckByID(id string) (Check, bool) {
	i := slices.IndexFunc(t.Checks, func(c Check) bool { return c.ID == id })
	if i < 0 {
		return Check{}, false
	}
	return t.Checks[i], true
}

// ComponentByID is the first component or interface the tree holds with id, and whether it holds
// one.
func (t Tree) ComponentByID(id string) (Component, bool) {
	i := slices.IndexFunc(t.Components, func(c Component) bool { return c.ID == id })
	if i < 0 {
		return Component{}, false
	}
	return t.Components[i], true
}

// ChecksOf is the checks that serve statement, in the tree's order.
func (t Tree) ChecksOf(statement string) []Check {
	var checks []Check
	for _, c := range t.Checks {
		if c.Serves == statement {
			checks = append(checks, c)
		}
	}
	return checks
}

// Children is the statements whose parent is statement, in the tree's order.
func (t Tree) Children(statement string) []Statement {
	var children []Statement
	for _, s := range t.Statements {
		if s.Parent == statement {
			children = append(children, s)
		}
	}
	return children
}
