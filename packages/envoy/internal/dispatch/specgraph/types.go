package specgraph

import (
	"slices"
	"time"
)

// Position is where a node or a problem is: File, the name of the file inside the spec directory,
// and Line, the 1-based line of the node's document's "---" line, or of the line a problem names.
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

// Statement is a requirement node: one or two sentences that must be true of the software.
type Statement struct {
	ID string
	// Text is its text, the lines of its document's text joined with "\n".
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

// Check is a check node: a claim that the system decides by itself, under one statement.
type Check struct {
	ID string
	// Serves holds the id of the one statement the check sits under; the schema refuses a value
	// that is not one id, such as two ids joined by a space.
	Serves                               string
	Claim, ProcedureText, FailingExample string
	Subject                              Subject
	Procedure                            Procedure
	// Test is non-nil exactly when the document holds a test part.
	Test *Test
	// TimeLimit is how long a result of the check counts (R9); 0 is no time limit.
	TimeLimit time.Duration
	Pos       Position
}

// Component is a component node (R15). An interface is a component with Interface set, which
// joins two or more components; the document of each holds its kind, component or interface.
type Component struct {
	ID, Name, Purpose, Parent string
	Interface                 bool
	// Joins is the components an interface joins, sorted and unique.
	Joins []string
	Pos   Position
}

// Term is a term node: a noun of the system and its definition of one sentence (R50).
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

// Problem is one fault of the files: where it is, the id of its node ("-" for a document with no
// id or a fault of no document), its rule, a Detail that begins with the rule, and, for a problem
// of Parse, its Effect on the tree that Parse gives.
type Problem struct {
	Pos                Position
	Node, Rule, Detail string
	Effect             Effect
}

// Text is the problem as a line prints it after its place, "<node>: <detail>": its node cut as Cut
// cuts a value of the input, since an id has no bound of length and Node stays whole as a key, and
// its Detail, whose values were cut where it was made. Each line that names a problem prints this.
func (p Problem) Text() string {
	return Cut(p.Node) + ": " + p.Detail
}

// Effect is what a problem of Parse does to the tree that Parse gives with it: LosesNode or
// KeepsEachNode. The reader sets on each of its problems the Effect of its kind (problemKinds,
// read.go). The zero value is no Effect, as on a problem of Validate or of Compare, and a caller
// that compares the tree reads it as LosesNode, so no problem reads as one that keeps each node by
// default.
type Effect uint8

const (
	// LosesNode is the Effect of a problem after which the tree can miss a node that the files hold,
	// or hold a node in another text than the files write: a document that the YAML library does not
	// read, or whose data has no JSON form or gives no node, the documents after a fault that stops
	// the reading, a key that the node takes no field for, a value of another type, a component kind
	// or a time limit that the node drops, or a file that is not as Print writes its nodes, which two
	// YAML libraries can read as two texts. A comparison of that tree with an approved spec would
	// not see that text, so a caller that compares refuses the files.
	LosesNode Effect = iota + 1
	// KeepsEachNode is the Effect of a problem after which the tree holds each node in the text that
	// the files write, each file in the form that Print writes: a file with no document, or a value
	// that the schema refuses and the node keeps, such as a value outside a closed set or a missing
	// key. The comparison of the files' own text decides.
	KeepsEachNode
)

// ProblemKind is one kind of problem that the package gives: the format of its Detail, which
// begins with its rule, and its Effect, which only a kind of the reader has.
type ProblemKind struct {
	Format string
	Effect Effect
}

// ProblemKinds is each kind of problem that the package gives: the reader's (problemKinds),
// Validate's (ruleKinds), and those of a comparison with the approved spec, Compare's and
// bad-approved-document, which ReadApproved names (approvalKinds). Each list is the table that the
// code makes its problems from, so no kind can be made that it does not give. Two kinds can have
// one format, such as the two of bad-yaml. A caller that must meet each kind, such as the table
// test of the lines that print a problem (cmd/spec-check), checks itself against it, so a new kind
// fails that test until it has a row.
func ProblemKinds() []ProblemKind {
	kinds := make([]ProblemKind, 0, len(problemKinds)+len(ruleKinds)+len(approvalKinds))
	for _, k := range problemKinds {
		kinds = append(kinds, ProblemKind{Format: k.format, Effect: k.effect})
	}
	for _, format := range ruleKinds {
		kinds = append(kinds, ProblemKind{Format: format})
	}
	for _, format := range approvalKinds {
		kinds = append(kinds, ProblemKind{Format: format})
	}
	return kinds
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
