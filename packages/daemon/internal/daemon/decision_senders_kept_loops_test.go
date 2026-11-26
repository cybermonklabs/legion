package daemon

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testtypes"
)

// This file holds the planted rows of the rule for a list of kept decisions, which
// TestDecisionSendersAskWhetherTheReviewIsStillThePersonsWord holds the three sender packages to:
// each row is a shape in which a loop has named, or could name, a kept decision without asking the
// predicate whether it is still the person's word, and the walk must read that loop as it is.
// The rows read the walk through readDecisionFunc, decisionFunc.keptLoops and decisionWalk.reaches
// alone, so this file runs unchanged against any version of the walk that has those three, an
// earlier one among them, on which each row of a shape that version misses fails by name. It
// stands while that test does; delete it with that test.

// plantedDecisionPackage is the import path the planted source is type-checked as, and
// plantedDecisionRender the function each row plants, as go/types names it.
const (
	plantedDecisionPackage = senderModule + "planted"
	plantedDecisionRender  = plantedDecisionPackage + ".render"
)

// plantedDecisionPreamble opens every row's source: the imports a row may use, and the helpers
// it may call, one that asks the predicate and two that ask nothing.
const plantedDecisionPreamble = `package planted

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// stands asks the predicate whether d is still the person's word.
func stands(gate *record.DesignGate, d record.DecisionMark) bool {
	return workflow.SupersedingApproval(gate, true, workflow.Decision{Kind: d.Decision, Artifact: d.Artifact, Version: d.Version}) == nil
}

// line is how a planted task names d, asking nothing.
func line(d record.DecisionMark) string { return fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor) }

// joined is a planted task's text of what it names.
func joined(named []string) string { return strings.Join(named, "; ") }
`

// plantedDecisionSource type-checks planted source against the compiler's export data of the
// daemon's dependencies (testtypes.CompiledDependencies), as readDecisionWalk checks the three
// packages. Its importer is not safe for concurrent use, so a test's rows take it in turn.
type plantedDecisionSource struct {
	fset    *token.FileSet
	imports types.Importer
}

func newPlantedDecisionSource(t *testing.T) plantedDecisionSource {
	t.Helper()
	compiled, err := testtypes.CompiledDependencies(senderModule + "daemon")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	return plantedDecisionSource{fset: fset, imports: importer.ForCompiler(fset, "gc", compiled)}
}

// walk is the walk of decls, planted after plantedDecisionPreamble, with each function read by
// readDecisionFunc as readDecisionWalk reads each function of the three packages.
func (p plantedDecisionSource) walk(t *testing.T, decls string) *decisionWalk {
	t.Helper()
	file, err := parser.ParseFile(p.fset, "planted.go", plantedDecisionPreamble+decls, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the planted source: %v", err)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{}}
	config := types.Config{Importer: p.imports}
	if _, err := config.Check(plantedDecisionPackage, p.fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("type-check the planted source: %v", err)
	}
	pkg := &checkedPackage{name: "planted", files: []*ast.File{file}, fset: p.fset, info: info}
	w := &decisionWalk{byName: map[string]*decisionFunc{}}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			f := readDecisionFunc(pkg, fn)
			w.funcs = append(w.funcs, f)
			w.byName[f.name] = f
		}
	}
	return w
}

// Each row plants a function render and names, in source order, each loop of it the walk must
// read as reading a kept decision, by whether that loop reaches the predicate in its own pass:
// false for a loop that names a decision unasked, true for one that asks. A row that names no
// loop plants one that reads no kept decision, which the walk must not take for one.
func TestDecisionSendersSeeEveryLoopThatReadsAKeptDecision(t *testing.T) {
	t.Parallel()
	planted := newPlantedDecisionSource(t)
	for _, row := range []struct {
		name   string
		asks   []bool
		source string
	}{
		{name: "fmt.Sprintf of Ref and Actor alone", asks: []bool{false}, source: `
func render(marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		named = append(named, fmt.Sprintf("%s by %s", d.Ref, d.Actor))
	}
	return joined(named)
}
`},
		{name: "the whole decision handed to a formatting helper", asks: []bool{false}, source: `
func render(marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		named = append(named, line(d))
	}
	return joined(named)
}
`},
		{name: "the whole decision handed to a method", asks: []bool{false}, source: `
type task struct{ text strings.Builder }

func (t *task) add(d record.DecisionMark) {
	fmt.Fprintf(&t.text, "%s (%s by %s); ", d.Ref, d.Decision, d.Actor)
}

func render(marks []record.DecisionMark) string {
	var t task
	for _, d := range marks {
		t.add(d)
	}
	return t.text.String()
}
`},
		{name: "a switch on Decision that writes constants", asks: []bool{false}, source: `
func render(marks []record.DecisionMark) string {
	var text strings.Builder
	for _, d := range marks {
		switch d.Decision {
		case record.DecisionDesignApproved:
			text.WriteString("A person approved the spec. ")
		case record.DecisionDesignChangesRequested:
			text.WriteString("A person requested changes to the spec. ")
		}
	}
	return text.String()
}
`},
		{name: "fmt.Sprint of the whole decision", asks: []bool{false}, source: `
func render(marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		named = append(named, fmt.Sprint(d))
	}
	return joined(named)
}
`},
		{name: "a C-style for that indexes the list", asks: []bool{false}, source: `
func render(marks []record.DecisionMark) string {
	var named []string
	for i := 0; i < len(marks); i++ {
		named = append(named, fmt.Sprintf("%s (%s by %s)", marks[i].Ref, marks[i].Decision, marks[i].Actor))
	}
	return joined(named)
}
`},
		{name: "a range over an iterator function", asks: []bool{false}, source: `
func render(seq func(yield func(record.DecisionMark) bool)) string {
	var named []string
	for d := range seq {
		named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
	}
	return joined(named)
}
`},
		{name: "a range over a map", asks: []bool{false}, source: `
func render(byRef map[string]record.DecisionMark) string {
	var named []string
	for _, d := range byRef {
		named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
	}
	return joined(named)
}
`},
		{name: "a decision read only in the clause of a loop nested in it", asks: []bool{false}, source: `
func render(marks []record.DecisionMark) string {
	var text strings.Builder
	for _, d := range marks {
		for _, part := range []string{d.Ref, d.Actor} {
			text.WriteString(part + "; ")
		}
	}
	return text.String()
}
`},
		{name: "a loop that asks the predicate of each decision it names", asks: []bool{true}, source: `
func render(gate *record.DesignGate, marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		if workflow.SupersedingApproval(gate, true, workflow.Decision{Kind: d.Decision, Artifact: d.Artifact, Version: d.Version}) != nil {
			continue
		}
		named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
	}
	return joined(named)
}
`},
		{name: "a loop that asks through a helper that asks", asks: []bool{true}, source: `
func render(gate *record.DesignGate, marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		if stands(gate, d) {
			named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
		}
	}
	return joined(named)
}
`},
		{name: "a predicate call before the loop does not ask for it", asks: []bool{false}, source: `
func render(gate *record.DesignGate, first record.DecisionMark, marks []record.DecisionMark) string {
	if workflow.SupersedingApproval(gate, true, workflow.Decision{Kind: first.Decision, Artifact: first.Artifact, Version: first.Version}) != nil {
		return ""
	}
	var named []string
	for _, d := range marks {
		named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
	}
	return joined(named)
}
`},
		{name: "a predicate call in a nested loop asks for that loop alone", asks: []bool{false, true}, source: `
func render(gates map[record.DesignStep]record.DesignGate, marks []record.DecisionMark) (string, int) {
	var named []string
	spent := 0
	for _, d := range marks {
		named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
		for step, gate := range gates {
			if workflow.SupersedingApproval(&gate, true, workflow.Decision{Kind: d.Decision, Step: step, Artifact: d.Artifact, Version: d.Version}) != nil {
				spent++
			}
		}
	}
	return joined(named), spent
}
`},
		{name: "a loop over owed starts named kept reads no kept decision", source: `
type owedRow struct{ issue, token string }

func render(kept []owedRow) []string {
	var keys []string
	for _, keptRow := range kept {
		keys = append(keys, keptRow.issue+"/"+keptRow.token)
	}
	return keys
}
`},
	} {
		t.Run(row.name, func(t *testing.T) {
			walk := planted.walk(t, row.source)
			render := walk.byName[plantedDecisionRender]
			if render == nil {
				t.Fatalf("the walk read no %s", short(plantedDecisionRender))
			}
			var asks []bool
			var read []string
			for _, loop := range render.keptLoops {
				reached := walk.reaches(loop.calls)
				asks = append(asks, reached)
				read = append(read, fmt.Sprintf("%s reaches %s: %t", loop.at, short(decisionPredicate), reached))
			}
			if !slices.Equal(asks, row.asks) {
				t.Errorf("the walk read render's loops that read a kept decision as %v, whether each reaches %s in its own pass, want %v: %q",
					asks, short(decisionPredicate), row.asks, read)
			}
		})
	}
}
